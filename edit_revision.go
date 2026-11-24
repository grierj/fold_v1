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

// groupRevision is the token the group form based on g holds:
// 1.<generation>.g<group id>.<group revision>. The "g" and the part count keep
// it apart from an event's or a date's token. The trigger of migration 00017
// raises the revision on every change to a tracked column of the group row,
// the cover endpoints' image_url included.
func (a *App) groupRevision(g *models.Group) string {
	return a.editRevision(fmt.Sprintf("g%d", g.ID), g.EditRevision)
}

// checkGroupRevision refuses given unless it is the token of locked, the group
// as read under its row lock.
func (a *App) checkGroupRevision(locked *models.Group, given string) error {
	return checkEditRevision(given, a.groupRevision(locked), errGroupChanged)
}

// readGroupRevision reads g's revision back into it after the transaction's
// last write: edit_revision is the trigger's, never written by GORM.
func readGroupRevision(tx *gorm.DB, g *models.Group) error {
	return tx.Raw("SELECT edit_revision FROM groups WHERE id = ?", g.ID).Row().Scan(&g.EditRevision)
}
