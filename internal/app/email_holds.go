package app

import (
	"fold/internal/mail"
	"fold/models"

	"gorm.io/gorm"
)

// lockInvitationEmail serializes everything that can claim one address:
// ordinary and household invitations reserve the same email namespace.
func lockInvitationEmail(tx *gorm.DB, email string) error {
	return tx.Exec("SELECT pg_advisory_xact_lock(hashtextextended(?, ?))", mail.NormalizeAddress(email), invitationEmailLockSeed).Error
}

// What can already hold an address (the audited reason an invitation was refused).
const (
	holdAccount           = "existing_account"
	holdPendingConversion = "pending_conversion"
	holdPendingInvitation = "pending_invitation"
)

// emailHeldBy says what already claims email, or "" when it is free.
// Writers hold lockInvitationEmail when reserving an address. Mail preparation
// reads the same facts in one statement without taking that lock.
// A soft-deleted account holds its address. A guest row holds it too unless
// guestUpgradable: an ordinary invitation upgrades a guest on the same row
// (C5), but a household conversion creates a new row and would collide with
// the guest. exceptInvitation and exceptRequest name the invitation and
// conversion request being worked on, which do not hold the address against
// themselves.
func emailHeldBy(tx *gorm.DB, email string, exceptInvitation, exceptRequest uint, guestUpgradable bool) (string, error) {
	var hold string
	err := tx.Raw(`SELECT CASE
		WHEN EXISTS (SELECT 1 FROM users WHERE email = ? AND (NOT ? OR status <> ? OR deleted_at IS NOT NULL)) THEN ?
		WHEN EXISTS (SELECT 1 FROM account_requests WHERE email = ? AND source = ? AND status = ? AND id <> ?) THEN ?
		WHEN EXISTS (SELECT 1 FROM invitations WHERE email = ? AND id <> ? AND `+sqlInvitationOpen+` AND `+sqlStillLive+`) THEN ?
		ELSE '' END`,
		email, guestUpgradable, models.UserGuest, holdAccount,
		email, models.RequestHouseholdConversion, models.RequestPending, exceptRequest, holdPendingConversion,
		email, exceptInvitation, holdPendingInvitation).Scan(&hold).Error
	return hold, err
}
