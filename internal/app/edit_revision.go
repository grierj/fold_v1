package app

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"fold/internal/audit"
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
// a token newer than the fields beside it would let a stale form pass. No row
// with the key is gorm.ErrRecordNotFound.
func (a *App) rowRevisionIn(tx *gorm.DB, t revisionRows, key uint) (string, error) {
	var digests []string
	if err := tx.Raw("SELECT "+t.digestSQL()+" FROM "+t.table+" r WHERE r."+t.key+" = ?", key).Scan(&digests).Error; err != nil {
		return "", err
	}
	switch len(digests) {
	case 1:
		return a.editRevision(fmt.Sprintf("%s%d", t.kind, key), digests[0]), nil
	case 0:
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
// never changes.
var bookingRevisionRows = revisionRows{table: "room_bookings", kind: "b", key: "occurrence_id",
	instants: []string{"starts_at", "ends_at", "updated_at"}, omit: []string{"created_at", "padded_range"},
	also: "coalesce((SELECT q.status FROM room_requests q WHERE q.id = r.request_id), '')"}

// dateRoomRevisionIn is the token a date's room control holds for the date's
// booking. Every writer of a date's booking holds its event's row lock, which
// the pick takes before it compares.
//
// With no booking the token is bound to the date's last room-booking audit
// row instead (0 before the first): every date pick that stores a booking or
// releases one writes it, so a pick whose answer was lost is never applied
// again over a release that undid it (DEC-164), while a booking of another
// date leaves the token alone. The id goes in as a keyed digest under this
// process's key: it counts audit rows across the church, which a manager must
// not read.
func (a *App) dateRoomRevisionIn(tx *gorm.DB, occurrenceID uint) (string, error) {
	token, err := a.rowRevisionIn(tx, bookingRevisionRows, occurrenceID)
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return token, err
	}
	var last uint
	if err := tx.Raw(`SELECT coalesce(max(id), 0) FROM audit_logs WHERE target_type = 'occurrence' AND target_id = ? AND action IN ?`,
		occurrenceID, []string{audit.RoomBookingBooked, audit.RoomBookingOverlap, audit.RoomBookingReleased}).Row().Scan(&last); err != nil {
		return "", err
	}
	m := hmac.New(sha256.New, a.processKey("date-room-absent:v1"))
	fmt.Fprintf(m, "%d.%d", occurrenceID, last)
	return a.editRevision(fmt.Sprintf("%s%d", bookingRevisionRows.kind, occurrenceID), "none-"+hex.EncodeToString(m.Sum(nil)[:16])), nil
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

// messageRevisionRows is a message: the composer's edit and send name it.
// Besides its fields, the token moves with a schedule, a claim, a settle and
// a cancel, since each stamps updated_at. created_at never changes.
var messageRevisionRows = revisionRows{table: "messages", kind: "m", key: "id",
	instants: []string{"scheduled_at", "sent_at", "updated_at"}, omit: []string{"created_at"}}

// messageRevisionIn is the token the composer holds for the message row as tx
// sees it (rowRevisionIn): a reader calls it in the snapshot that read the
// fields it shows, a writer under the message's row lock.
func (a *App) messageRevisionIn(tx *gorm.DB, id uint) (string, error) {
	return a.rowRevisionIn(tx, messageRevisionRows, id)
}

// checkMessageRevision refuses given unless it is the token of the message as
// tx reads it under its row lock.
func (a *App) checkMessageRevision(tx *gorm.DB, id uint, given string) error {
	current, err := a.messageRevisionIn(tx, id)
	if err != nil {
		return err
	}
	return checkEditRevision(given, current, errMessageChanged)
}

// errRoomChanged refuses an admin's save of a room made from an older read of
// the room than the one it would change. Nothing is written.
var errRoomChanged = apiErr(http.StatusConflict, "room_changed", "the room changed since this edit was based on it")

// roomRevisionRows is a room: the admin room editor and its Deactivate and
// Reactivate name it (DEC-171). created_at never changes.
var roomRevisionRows = revisionRows{table: "rooms", kind: "r", key: "id", instants: []string{"updated_at"}, omit: []string{"created_at"}}

// roomRevisionIn is the token the room editor holds for the room row as tx
// sees it (rowRevisionIn): a writer calls it under the room's row lock.
func (a *App) roomRevisionIn(tx *gorm.DB, id uint) (string, error) {
	return a.rowRevisionIn(tx, roomRevisionRows, id)
}

// roomRevisionsIn is roomRevisionIn for each of ids, keyed by id: the room
// list calls it in the snapshot that read the rooms it shows.
func (a *App) roomRevisionsIn(tx *gorm.DB, ids []uint) (map[uint]string, error) {
	return a.rowRevisionsIn(tx, roomRevisionRows, ids)
}

// rowRevisionsIn is rowRevisionIn for each of keys, keyed by key, in one
// statement a chunk. A key with no row is missing from the map.
func (a *App) rowRevisionsIn(tx *gorm.DB, t revisionRows, keys []uint) (map[uint]string, error) {
	out := make(map[uint]string, len(keys))
	err := eachIDChunk(keys, func(chunk []uint) error {
		var rows []struct {
			Key    uint
			Digest string
		}
		if err := tx.Raw("SELECT r."+t.key+" AS key, "+t.digestSQL()+" AS digest FROM "+t.table+" r WHERE r."+t.key+" IN ?", chunk).Scan(&rows).Error; err != nil {
			return err
		}
		for _, r := range rows {
			out[r.Key] = a.editRevision(fmt.Sprintf("%s%d", t.kind, r.Key), r.Digest)
		}
		return nil
	})
	return out, err
}

// checkRoomRevision refuses given unless it is the token of the room as tx
// reads it under its row lock.
func (a *App) checkRoomRevision(tx *gorm.DB, id uint, given string) error {
	current, err := a.roomRevisionIn(tx, id)
	if err != nil {
		return err
	}
	return checkEditRevision(given, current, errRoomChanged)
}

var (
	errSettingChanged   = apiErr(http.StatusConflict, "setting_changed", "the setting changed since this edit was based on it")
	errAdminUserChanged = apiErr(http.StatusConflict, "admin_user_changed", "the account changed since this edit was based on it")
)

// The audit id distinguishes a save from one that changed the value and then
// put it back. Only the setting's own audit action advances it. The anchor row
// also gives an unset setting a token that no other setting can use.
func (a *App) settingRevisionIn(tx *gorm.DB, setting revisedSetting) (value, revision string, err error) {
	var digest string
	var last int64
	err = tx.Raw(`
		SELECT coalesce(s.value, ''), md5(coalesce(to_jsonb(s)::text, 'null')),
		       coalesce((SELECT max(id) FROM audit_logs WHERE action = ?), 0)
		FROM (VALUES (?::text)) AS k(key)
		LEFT JOIN settings s ON s.key = k.key
	`, setting.action, setting.key).Row().Scan(&value, &digest, &last)
	if err != nil {
		return "", "", err
	}
	return value, a.editRevision("s"+setting.key, digest, last), nil
}

func (a *App) checkSettingRevision(tx *gorm.DB, setting revisedSetting, given string) error {
	_, current, err := a.settingRevisionIn(tx, setting)
	if err != nil {
		return err
	}
	return checkEditRevision(given, current, errSettingChanged)
}

// Credentials change the session version. The password hash itself has no
// place in an edit token.
var adminUserRevisionRows = revisionRows{table: "users", kind: "u", key: "id",
	instants: []string{"updated_at", "deleted_at"}, omit: []string{"created_at", "password_hash"}}

func (a *App) checkAdminUserRevision(tx *gorm.DB, id uint, given string) error {
	current, err := a.rowRevisionIn(tx, adminUserRevisionRows, id)
	if err != nil {
		return err
	}
	return checkEditRevision(given, current, errAdminUserChanged)
}

var (
	errHouseholdChanged       = apiErr(http.StatusConflict, "household_changed", "the household changed since this edit was based on it")
	errHouseholdPersonChanged = apiErr(http.StatusConflict, "household_person_changed", "this household member changed since this edit was based on them")
)

// The household's name and address form and a household person's edit form
// name these rows. Every writer of an existing row holds the household's row
// lock (lockHousehold), which both saves take before they compare. created_at
// never changes. A person's removal is a soft delete, so deleted_at moves the
// token too. An adult of the household who is not the person's confirmed
// guardian holds the person's token but may not read their care notes, so the
// notes stay out of the digest, as a password hash stays out of an account's:
// every write of them stamps updated_at (the edit) or deleted_at (the
// erasure's scrub).
var (
	householdRevisionRows       = revisionRows{table: "households", kind: "h", key: "id", instants: []string{"updated_at"}, omit: []string{"created_at"}}
	householdPersonRevisionRows = revisionRows{table: "household_people", kind: "p", key: "id", instants: []string{"updated_at", "deleted_at"}, omit: []string{"created_at", "care_notes"}}
)

func (a *App) checkHouseholdRevision(tx *gorm.DB, id uint, given string) error {
	current, err := a.rowRevisionIn(tx, householdRevisionRows, id)
	if err != nil {
		return err
	}
	return checkEditRevision(given, current, errHouseholdChanged)
}

func (a *App) checkHouseholdPersonRevision(tx *gorm.DB, id uint, given string) error {
	current, err := a.rowRevisionIn(tx, householdPersonRevisionRows, id)
	if err != nil {
		return err
	}
	return checkEditRevision(given, current, errHouseholdPersonChanged)
}

// errConsentChanged refuses a grant of teen consent made from an older read of
// the teen's consent log than the one it would add to (DEC-176). Nothing is
// written.
var errConsentChanged = apiErr(http.StatusConflict, "consent_changed", "the teen's consent changed since this change was based on it")

// consentRevision is the token an admin's consent panel or a guardian's card
// holds for a teen whose consent log has changes rows (0 before the first).
// teen_consents is append only (trigger teen_consents_append_only) and its rows
// are never deleted, erasure included, so the count moves with every change of
// every scope and never comes back; a restore restarts the process, whose new
// generation ends every earlier token. Every append holds the teen's user row
// lock, which a grant takes before it compares.
func (a *App) consentRevision(teenID uint, changes int) string {
	return a.editRevision(fmt.Sprintf("t%d", teenID), changes)
}

// consentRevisionIn is consentRevision as tx counts the teen's log: a writer
// calls it under the teen's user row lock.
func (a *App) consentRevisionIn(tx *gorm.DB, teenID uint) (string, error) {
	var changes int
	if err := tx.Raw(`SELECT count(*) FROM teen_consents WHERE teen_user_id = ?`, teenID).Row().Scan(&changes); err != nil {
		return "", err
	}
	return a.consentRevision(teenID, changes), nil
}

// checkConsentRevision refuses given unless it is the token of the teen's
// consent log as tx reads it under the teen's user row lock.
func (a *App) checkConsentRevision(tx *gorm.DB, teenID uint, given string) error {
	current, err := a.consentRevisionIn(tx, teenID)
	if err != nil {
		return err
	}
	return checkEditRevision(given, current, errConsentChanged)
}
