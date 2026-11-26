package main

import (
	"fmt"
	"slices"
	"time"

	"fold/internal/policy"
	"fold/models"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Registration rows are written here. saveRegistration is the one writer of an
// answer (insert or update); promoteRegistration is the one transition that
// keeps a waiter's history. Each caller names the policy.RegistrationMove it
// makes and the status it judged the row on; a move the rule does not hold is
// a programming error. Both leave the exact-date access columns alone: those
// are written only by registration_access.go. Callers set the fields that
// carry the decision (status, mode, guests, childcare, removal note) and the
// writer clears what that status cannot hold.

// saveRegistration writes r in full as move's last write, from the status the
// caller judged it on (policy.RegNone for a row it makes), and returns it as
// stored, so a caller that reads r afterwards sees the cleared fields. The
// check is of the caller's decision, not of the stored row: it reads nothing.
//   - waitlisted keeps its place in line and nothing else changes;
//   - going has no wait: waitlisted_at and promoted_at are cleared;
//   - cancelled clears them too and keeps an account row's manage link live,
//     because a second cancel from the same link is a no-op and reopening a
//     date can restore access; a household row's link dies with the cancel,
//     and so does any other status's;
//   - removal fields exist only on a removed row.
func saveRegistration(tx *gorm.DB, move policy.RegistrationMove, from string, r *models.Registration) error {
	return writeRegistration(tx, move, from, r, move.Allows(from, r.Status))
}

// saveRegistrationStep is a write of move before its last one in the same
// transaction (applyRegistrationBatch frees a seat first), checked from the
// status the move started from.
func saveRegistrationStep(tx *gorm.DB, move policy.RegistrationMove, from string, r *models.Registration) error {
	return writeRegistration(tx, move, from, r, move.AllowsStep(from, r.Status))
}

func writeRegistration(tx *gorm.DB, move policy.RegistrationMove, from string, r *models.Registration, allowed bool) error {
	if move == policy.RegPromote || !allowed || (from == policy.RegNone) != (r.ID == 0) {
		return fmt.Errorf("registration %d: %q does not take %q to %q", r.ID, move, from, r.Status)
	}
	switch r.Status {
	case models.RegWaitlisted:
	case models.RegGoing:
		r.WaitlistedAt, r.PromotedAt = nil, nil
	case models.RegCancelled:
		r.WaitlistedAt, r.PromotedAt = nil, nil
		if r.HouseholdPersonID != nil {
			r.ManageTokenHash = ""
		}
	default:
		r.WaitlistedAt, r.PromotedAt, r.ManageTokenHash = nil, nil, ""
	}
	if r.Status != models.RegRemoved {
		r.RemovedBy, r.RemovalNote = nil, ""
	}
	return tx.Omit(registrationSaveOmits()...).Save(r).Error
}

// registrationStartsFrom is move.StartsFrom as SQL, a condition on
// registrations.status (TestRegistrationStartsFromSQLMatchesTheRule).
func registrationStartsFrom(move policy.RegistrationMove) clause.Expr {
	from := slices.DeleteFunc(move.From(), func(s string) bool { return s == policy.RegNone })
	return clause.Expr{SQL: "registrations.status IN ?", Vars: []any{from}}
}

// promoteRegistration moves a waiter to going: waitlisted_at stays as history,
// promoted_at is stamped and the rotated manage link is stored. A row the
// promotion no longer starts from (another seat-changing write got there
// first) is left alone and reported false.
func promoteRegistration(tx *gorm.DB, r models.Registration, manageHash string, now time.Time) (bool, error) {
	res := tx.Model(&models.Registration{}).
		Where("id = ?", r.ID).Where(registrationStartsFrom(policy.RegPromote)).
		Updates(map[string]any{"status": models.RegGoing, "promoted_at": now, "manage_token_hash": manageHash})
	return res.RowsAffected > 0, res.Error
}
