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

// revisionRows is a table whose rows a form names by a digest of the row
// (rowRevisionIn), with no counter column: adding a column to a table fails
// the statements an older process still serving had prepared (0A000). Every
// field is a constant of this file, never input.
type revisionRows struct {
	table string
	// kind tags the table's tokens (1.<generation>.<kind><key>.<digest>): a
	// letter no other token starts its third part with, so one table's token is
	// never another's, an event's or a date's.
	kind string
	// key is the column the token is bound to, at most one row per value.
	key string
	// instants are the row's timestamptz columns, which go in as their epochs:
	// a timestamp's text follows the session's time zone. updated_at, stamped on
	// each write, is one, so a write that puts the row back as it was still
	// moves the token (DEC-164).
	instants []string
	// omit are columns another column decides, left out for the same reason.
	omit []string
	// also is an expression over the row (alias r) the token covers besides it.
	also string
	// absent is a query for the digest when no row has the key; empty makes a
	// missing row an error.
	absent string
}

// groupRevisionRows: created_at never changes.
var groupRevisionRows = revisionRows{table: "groups", kind: "g", key: "id", instants: []string{"updated_at"}, omit: []string{"created_at"}}

// digestSQL is the md5 of the row r: every column but the instants and the
// omitted ones as JSON, then each instant's epoch, then also.
func (t revisionRows) digestSQL() string {
	drop := make([]string, 0, len(t.instants)+len(t.omit))
	text := make([]string, 0, len(t.instants)+1)
	for _, c := range t.instants {
		drop = append(drop, "'"+c+"'")
		text = append(text, "coalesce(extract(epoch FROM r."+c+")::text, '')")
	}
	for _, c := range t.omit {
		drop = append(drop, "'"+c+"'")
	}
	if t.also != "" {
		text = append(text, t.also)
	}
	return "md5((to_jsonb(r) - ARRAY[" + strings.Join(drop, ",") + "])::text || " + strings.Join(text, " || '.' || ") + ")"
}

// rowRevisionIn is the token of t's row whose key is key, as tx sees it:
// 1.<generation>.<kind><key>.<digest>. The digest covers every column, so
// every writer of the row moves it, a previous binary included, and there is
// no list of fields to keep. A reader calls it in the snapshot that read the
// fields it shows, and a writer under the lock every writer of the row holds:
// a token newer than the fields beside it would let a stale form pass.
func (a *App) rowRevisionIn(tx *gorm.DB, t revisionRows, key uint) (string, error) {
	var digests []string
	if err := tx.Raw("SELECT "+t.digestSQL()+" FROM "+t.table+" r WHERE r."+t.key+" = ?", key).Scan(&digests).Error; err != nil {
		return "", err
	}
	switch {
	case len(digests) == 1:
		return a.editRevision(fmt.Sprintf("%s%d", t.kind, key), digests[0]), nil
	case len(digests) == 0 && t.absent != "":
		var absent string
		if err := tx.Raw(t.absent).Row().Scan(&absent); err != nil {
			return "", err
		}
		return a.editRevision(fmt.Sprintf("%s%d", t.kind, key), absent), nil
	case len(digests) == 0:
		return "", gorm.ErrRecordNotFound
	}
	return "", fmt.Errorf("%s: %d rows with %s %d", t.table, len(digests), t.key, key)
}

// groupRevisionIn is the token the group form holds for the group row as tx
// sees it (rowRevisionIn): a reader calls it in the snapshot that read the
// fields it shows, a writer under the group's row lock.
func (a *App) groupRevisionIn(tx *gorm.DB, id uint) (string, error) {
	return a.rowRevisionIn(tx, groupRevisionRows, id)
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

// errDateRoomChanged refuses a pick of a date's room made from an older read
// of the date's booking than the one it would replace. Nothing is written.
var errDateRoomChanged = apiErr(http.StatusConflict, "date_room_changed", "the date's room changed since this pick was based on it")

// bookingRevisionRows is a date's booking, bound to its date (one booking a date).
// A pick deletes the booking and inserts a new one, whose new id moves the
// token. Beside the row, the token covers the status of the request it waits
// on, which decides whether the control says it is waiting for an admin.
// padded_range is the CHECK's formula over the other columns; created_at
// never changes. With no booking the digest is "none" and the position of the
// bookings' id sequence: a booking made and released since the read took an
// id, even rolled back, so a pick whose answer was lost is not applied again
// over the release that undid it (DEC-164). A booking anywhere moves it, which
// costs a silent resend at most.
var bookingRevisionRows = revisionRows{table: "room_bookings", kind: "b", key: "occurrence_id",
	instants: []string{"starts_at", "ends_at", "updated_at"}, omit: []string{"created_at", "padded_range"},
	also: "coalesce((SELECT q.status FROM room_requests q WHERE q.id = r.request_id), '')", absent: "SELECT 'none-' || last_value || '-' || is_called FROM room_bookings_id_seq"}

// dateRoomRevisionIn is the token a date's room control holds for the date's
// booking. Every writer of a date's booking holds its event's row lock, which
// the pick takes before it compares.
func (a *App) dateRoomRevisionIn(tx *gorm.DB, occurrenceID uint) (string, error) {
	return a.rowRevisionIn(tx, bookingRevisionRows, occurrenceID)
}

// checkDateRoomRevision refuses given unless it is the token of the date's
// booking as tx reads it under the event's lock.
func (a *App) checkDateRoomRevision(tx *gorm.DB, occurrenceID uint, given string) error {
	current, err := a.dateRoomRevisionIn(tx, occurrenceID)
	if err != nil {
		return err
	}
	return checkEditRevision(given, current, errDateRoomChanged)
}
