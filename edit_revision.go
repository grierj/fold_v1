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

// editGeneration is this process's generation, 16 hex characters. A token
// from before a restart, and so before any restore, never matches. The
// purpose string is the one the date edit has always used, so tokens keep
// their bytes across this file's move.
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
// control based on ev holds: 1.<generation>.<event id>.<event revision>. The
// trigger of migration 00010 raises the revision on every change to a tracked
// column of the event row.
func (a *App) eventRevision(ev *models.Event) string {
	return a.editRevision(ev.ID, ev.EditRevision)
}

// checkEventRevision refuses given unless it is the token of locked, the event
// as read under its row lock.
func (a *App) checkEventRevision(locked *models.Event, given string) error {
	return checkEditRevision(given, a.eventRevision(locked), errEventChanged)
}

// readEventRevision reads ev's revision back into it after the transaction's
// last write: edit_revision is the trigger's, never written by GORM.
func readEventRevision(tx *gorm.DB, ev *models.Event) error {
	return tx.Raw("SELECT edit_revision FROM events WHERE id = ?", ev.ID).Row().Scan(&ev.EditRevision)
}
