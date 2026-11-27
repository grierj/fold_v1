package app

import (
	"context"
	"time"

	"fold/internal/audit"
	"fold/models"
)

const expiredLinkRetention = 30 * 24 * time.Hour

func (a *App) pruneExpiredLinks(ctx context.Context, now time.Time) error {
	cutoff := now.Add(-expiredLinkRetention)
	db := a.DB.WithContext(ctx)
	if err := db.Where("expires_at <= ?", cutoff).Delete(&models.GuestSignup{}).Error; err != nil {
		return err
	}
	if err := db.Where("COALESCE(used_at, expires_at) <= ?", cutoff).Delete(&models.AccountToken{}).Error; err != nil {
		return err
	}
	// Household conversion can resend an open invitation after its link expires.
	// Account requests need their invitation through approval, consent and erasure.
	// A worker can outlive a database restore. Release the audit-email hold
	// only when this DELETE sees the level-4 boot fence on this database.
	// Without an acceptance audit, a legacy row is the only proof of acceptance.
	return db.Exec(`DELETE FROM invitations i
		WHERE i.expires_at <= ?
		  AND (i.household_person_id IS NULL OR NOT (`+sqlInvitationOpen+`))
		  AND NOT EXISTS (SELECT 1 FROM account_requests r WHERE r.invitation_id = i.id)
		  AND ((i.accepted_at IS NULL AND EXISTS (
			SELECT 1 FROM pg_trigger WHERE tgrelid = 'settings'::regclass
			  AND tgname = 'settings_recovery_floor_invitations' AND tgenabled IN ('O', 'A')
		  )) OR NOT EXISTS (
			SELECT 1 FROM audit_logs a WHERE a.target_type = 'invitation' AND a.target_id = i.id
			  AND a.action = ? AND a.meta_json->>'email' IS NOT NULL
		  ) OR (i.accepted_at IS NOT NULL AND EXISTS (
			SELECT 1 FROM audit_logs a WHERE a.target_type = 'invitation' AND a.target_id = i.id AND a.action = ?
		  )))`, cutoff, audit.InvitationCreated, audit.InvitationAccepted).Error
}
