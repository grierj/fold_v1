package main

import (
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"

	"fold/models"

	"gorm.io/gorm"
)

// A manager's save names the read it was made from, and the server refuses an
// older one under the row lock, writing nothing (DEC-162, DEC-163). This file
// is the one place a revision token is built and compared; each transaction
// keeps its own locking, authority and choice of fields.

// errEventChanged refuses a save of the event made from an older read of it
// than the one it would change. Nothing is written.
var errEventChanged = apiErr(http.StatusConflict, "event_changed", "the event changed since this edit was based on it")

// editGeneration is this process's generation, 16 hex characters, which the
// date edit reads at base[2:18]. A token from before a restart, and so before
// any restore, never matches.
func (a *App) editGeneration() string {
	return hex.EncodeToString(a.processKey("date-edit-generation:v1")[:8])
}

// editRevision is 1.<generation>.<parts joined by dots>.
func (a *App) editRevision(parts ...any) string {
	s := make([]string, 0, len(parts)+2)
	s = append(s, "1", a.editGeneration())
	for _, p := range parts {
		s = append(s, fmt.Sprint(p))
	}
	return strings.Join(s, ".")
}

// checkEditRevision answers stale unless given is current. There are no
// attempt receipts behind it, so equality is the whole rule: a missing,
// malformed, older, other row's or other generation's token is refused alike.
func checkEditRevision(given, current string, stale error) error {
	if given != current {
		return stale
	}
	return nil
}

// eventRevision is the token a series editor or the event's visibility
// control based on ev holds:
// 1.<generation>.<event id>.<event revision>.<audience>. The trigger of
// migration 00010 raises the revision on every change to a tracked column of
// the event row. audience tags the group's visibility on a group's event
// (group as the read or the locked write loaded it), since it decides whether
// a wider value is leader-opened (leaderOpens): a group change conflicts
// though the event row is untouched. It is "c" on a church-wide event. The tag
// is the value, not a counter: a group that changes and changes back matches
// again. Every update of an existing group's visibility holds the exclusive
// contact gate, which both event saves hold, so the group is not locked here.
func (a *App) eventRevision(ev *models.Event, group *models.Group) string {
	return a.editRevision(ev.ID, ev.EditRevision, revisionAudience(ev, group))
}

var revisionAudienceTags = map[string]string{
	models.VisibilityPublic:     "pub",
	models.VisibilityPrivate:    "prv",
	models.VisibilityInviteOnly: "inv",
}

func revisionAudience(ev *models.Event, group *models.Group) string {
	if ev.GroupID == nil {
		return "c"
	}
	if group == nil {
		return "-"
	}
	if tag, ok := revisionAudienceTags[group.Visibility]; ok {
		return tag
	}
	return group.Visibility
}

// checkEventRevision refuses given unless it is the token of locked, the event
// as read under its row lock, with group as that transaction read it.
func (a *App) checkEventRevision(locked *models.Event, group *models.Group, given string) error {
	return checkEditRevision(given, a.eventRevision(locked, group), errEventChanged)
}

// readEventRevision reads ev's revision back into it after the transaction's
// last write: edit_revision is the trigger's, never written by GORM.
func readEventRevision(tx *gorm.DB, ev *models.Event) error {
	return tx.Raw("SELECT edit_revision FROM events WHERE id = ?", ev.ID).Row().Scan(&ev.EditRevision)
}

// errGroupChanged refuses a group form save made from an older read of the
// group than the one it would change. Nothing is written.
var errGroupChanged = apiErr(http.StatusConflict, "group_changed", "the group changed since this edit was based on it")

// groupRevisionIn is the token the group form holds for the group row as tx
// sees it: 1.<generation>.g<group id>.<digest>. The "g" and the part count
// keep it apart from an event's or a date's token. The digest covers every
// column but updated_at, which GORM stamps on each write, and created_at,
// which never changes: every writer of the row changes it, the cover
// endpoints and a previous binary included, and there is no list of fields to
// keep. Leaving both timestamps out also keeps a live row's digest the same
// in every session (a timestamp's text follows the session's time zone, and
// deleted_at is null). It is the row's value, not a counter: a
// group changed and changed back matches again, as revisionAudience does. The
// group has no counter column because adding one fails the statements an
// older process still serving had prepared (0A000), which the restore drill's
// running-prior case does not retry on its last read.
//
// A reader must call it in the snapshot that read the fields it shows, and a
// writer under the group's row lock: a token newer than the fields beside it
// would let a stale form pass.
func (a *App) groupRevisionIn(tx *gorm.DB, id uint) (string, error) {
	var digest string
	if err := tx.Raw("SELECT md5((to_jsonb(g) - ARRAY['updated_at','created_at'])::text) FROM groups g WHERE g.id = ?", id).Row().Scan(&digest); err != nil {
		return "", err
	}
	return a.editRevision(fmt.Sprintf("g%d", id), digest), nil
}

// checkGroupRevision refuses given unless it is the token of the group as tx
// reads it under its row lock.
func (a *App) checkGroupRevision(tx *gorm.DB, id uint, given string) error {
	current, err := a.groupRevisionIn(tx, id)
	if err != nil {
		return err
	}
	return checkEditRevision(given, current, errGroupChanged)
}
