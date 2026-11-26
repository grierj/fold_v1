package main

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"fold/internal/audit"
	"fold/models"
)

func TestExpiredLinkRetention(t *testing.T) {
	s := newCoreErasureScene(t)
	_, occ, _ := erasureEventFixture(t, s)
	db := s.app.DB
	now := time.Now().UTC().Truncate(time.Microsecond)
	cutoff := now.Add(-30 * 24 * time.Hour)
	before, after := cutoff.Add(-time.Microsecond), cutoff.Add(time.Microsecond)
	type row struct {
		table string
		id    uint
		keep  bool
	}
	var rows []row
	for _, tc := range []struct {
		expires time.Time
		keep    bool
	}{{before, false}, {cutoff, false}, {after, true}, {now.Add(time.Hour), true}} {
		signup := models.GuestSignup{OccurrenceID: occ.ID, Name: "Guest", Email: "guest@example.com",
			TokenHash: "signup-" + tc.expires.String(), ExpiresAt: tc.expires}
		erasureMustCreate(t, db, &signup)
		rows = append(rows, row{"guest_signups", signup.ID, tc.keep})
		inv := models.Invitation{InvitedBy: s.admin.ID, Email: "invite@example.com",
			TokenHash: "invite-" + tc.expires.String(), ExpiresAt: tc.expires}
		erasureMustCreate(t, db, &inv)
		rows = append(rows, row{"invitations", inv.ID, tc.keep})
	}
	for i, tc := range []struct {
		expires time.Time
		used    *time.Time
		keep    bool
	}{
		{cutoff, nil, false},
		{after, nil, true},
		{now.Add(time.Hour), nil, true},
		{cutoff.Add(time.Hour), &cutoff, false},
		{cutoff.Add(time.Hour), &after, true},
	} {
		tok := models.AccountToken{UserID: s.admin.ID, Kind: models.TokenPasswordReset,
			TokenHash: fmt.Sprintf("account-%d", i), ExpiresAt: tc.expires, UsedAt: tc.used}
		erasureMustCreate(t, db, &tok)
		rows = append(rows, row{"account_tokens", tok.ID, tc.keep})
	}
	confirmed := models.GuestSignup{OccurrenceID: occ.ID, Name: "Confirmed", Email: "confirmed@example.com",
		TokenHash: "signup-confirmed", ExpiresAt: before, ConfirmedAt: &before}
	erasureMustCreate(t, db, &confirmed)
	rows = append(rows, row{"guest_signups", confirmed.ID, false})

	for range 2 {
		if err := s.app.pruneExpiredLinks(t.Context(), now); err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if kept := countRows(t, db, r.table, "id = ?", r.id) == 1; kept != r.keep {
				t.Errorf("%s id %d kept = %v, want %v", r.table, r.id, kept, r.keep)
			}
		}
	}
}

func TestExpiredLinkCleanupKeepsHouseholdConversionWorking(t *testing.T) {
	s := newCoreErasureScene(t)
	_, holder, person, _ := householdFamily(t, s.app, s.ac, "retention-holder")
	const email = "retention-convert@example.com"
	inviteHouseholdPerson(t, s.app, holder, person.ID, email)
	db := s.app.DB
	var inv models.Invitation
	if err := db.Where("household_person_id = ?", person.ID).First(&inv).Error; err != nil {
		t.Fatal(err)
	}
	// Cleanup uses the actual clock for a resend, then a later clock for the
	// pending and approved request's already-expired invitation.
	now := time.Now()
	if err := db.Model(&inv).Update("expires_at", now.Add(-31*24*time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	old := models.Invitation{HouseholdPersonID: &person.ID, InvitedBy: s.admin.ID, Email: "replaced@example.com",
		TokenHash: "replaced-conversion", ExpiresAt: now.Add(-32 * 24 * time.Hour), RevokedAt: &now}
	erasureMustCreate(t, db, &old)
	if err := s.app.pruneExpiredLinks(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, db, "invitations", "id = ?", old.ID); n != 0 {
		t.Fatal("kept the replaced, expired conversion invitation")
	}
	apiDo(t, holder, "POST", conversionPath(person.ID)+"/resend", nil, http.StatusOK)
	acceptConversion(t, s.app, tokenInEmail(t, latestEmail(t, s.app, email)), http.StatusOK)
	var req models.AccountRequest
	if err := db.Where("invitation_id = ?", inv.ID).First(&req).Error; err != nil {
		t.Fatal(err)
	}
	later := now.Add(32 * 24 * time.Hour)
	if err := s.app.pruneExpiredLinks(t.Context(), later); err != nil {
		t.Fatal(err)
	}
	apiDo(t, s.ac, "POST", "/api/v1/admin/account-requests/"+itoa(req.ID)+"/approve", map[string]any{}, http.StatusOK)
	if err := s.app.pruneExpiredLinks(t.Context(), later); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, db, "invitations", "id = ?", inv.ID); n != 1 {
		t.Fatal("lost the approved request's invitation")
	}
}

func TestErasureFindsInvitationAuditAfterCleanup(t *testing.T) {
	s := newCoreErasureScene(t)
	db := s.app.DB
	const current, former = "current@example.com", "former@example.com"
	u := newTestUser(t, s.app, current, erasurePW, models.RoleMember)
	other := newTestUser(t, s.app, "prior-holder", erasurePW, models.RoleMember)
	cases := []struct {
		name, address, acceptance string
		accepted, keepRow, scrub  bool
	}{
		{"unaccepted at held address", "CURRENT@example.com", "", false, true, true},
		{"unaccepted at former address", former, "", false, true, false},
		{"accepted before address change", former, fmt.Sprintf(`{"userId":%d}`, u.ID), true, false, true},
		{"accepted by prior holder", current, fmt.Sprintf(`{"userId":%d}`, other.ID), true, false, false},
		{"legacy acceptance without user id", current, `{}`, true, false, false},
		{"legacy acceptance without audit", current, "", true, true, false},
	}
	ids := make([]uint, len(cases))
	for i, tc := range cases {
		id := erasureInvitation(t, db, s.admin.ID, tc.address, tc.accepted, nil)
		ids[i] = id
		if tc.acceptance != "" {
			erasureAcceptedAudit(t, db, id, tc.acceptance)
		}
		if err := audit.Write(db, audit.Entry{Actor: &s.admin.ID, Action: audit.InvitationCreated,
			TargetType: "invitation", TargetID: id, Meta: map[string]any{"email": tc.address, "overLimit": false}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.app.pruneExpiredLinks(t.Context(), time.Now().Add(32*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	check := func(t *testing.T, id uint, want string) {
		t.Helper()
		var got models.AuditLog
		if err := db.Where("target_type = 'invitation' AND target_id = ? AND action = ?", id, audit.InvitationCreated).Take(&got).Error; err != nil {
			t.Fatal(err)
		}
		if got.MetaJSON != want {
			t.Errorf("invitation audit metadata = %s, want %s", got.MetaJSON, want)
		}
	}
	for i, tc := range cases {
		if kept := countRows(t, db, "invitations", "id = ?", ids[i]) == 1; kept != tc.keepRow {
			t.Errorf("%s: invitation kept = %v, want %v", tc.name, kept, tc.keepRow)
		}
		check(t, ids[i], fmt.Sprintf(`{"email": %q, "overLimit": false}`, tc.address))
	}
	// Exercise erasure against the data shape after the later cleanup stage.
	for i, tc := range cases {
		if !tc.accepted {
			if err := db.Delete(&models.Invitation{}, ids[i]).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	s.erase(t, u.ID)
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := fmt.Sprintf(`{"email": %q, "overLimit": false}`, tc.address)
			if tc.scrub {
				want = `{"overLimit": false}`
			}
			check(t, ids[i], want)
		})
	}
}
