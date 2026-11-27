package app

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"fold/internal/audit"
	"fold/internal/testdb"
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
		if err := audit.Write(db, audit.Entry{Action: audit.InvitationCreated, TargetType: "invitation",
			TargetID: inv.ID, Meta: map[string]any{"email": inv.Email}}); err != nil {
			t.Fatal(err)
		}
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
		{"unaccepted at held address", "CURRENT@example.com", "", false, false, true},
		{"unaccepted at former address", former, "", false, false, false},
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

func TestInvitationCleanupRequiresLevelFour(t *testing.T) {
	current, previous := buildFold(t, ""), buildFold(t, "v0.1.1")
	name := testdb.Fresh(t, false)
	db, err := testdb.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	migrate := func(bin string) {
		t.Helper()
		if out, err := runFold(t, bin, name, "migrate", "up"); err != nil {
			t.Fatalf("migrate up: %v, %s", err, out)
		}
	}
	// Run the installed restore check against the same PostgreSQL schema.
	// In production pg() connects to the staging database through Compose.
	findPGClients(t, "psql")
	archiveCheck := func(level string, wantError string) {
		t.Helper()
		const check = `
			source deploy/digitalocean/checks.sh
			level="$1"; shift
			client=("$@")
			pg() { shift; "${client[@]}" -X -d "$FOLD_TEST_STAGE_URL" -v ON_ERROR_STOP=1 "$@"; }
			check_archive_recovery_level staging "$level"`
		cmd := exec.Command("bash", append([]string{"-c", check, "restore-check", level}, pgClients["psql"]...)...)
		cmd.Env = append(os.Environ(), "FOLD_TEST_STAGE_URL="+fenceDatabaseURL(t, db))
		out, err := cmd.CombinedOutput()
		if (err != nil) != (wantError != "") || !strings.Contains(string(out), wantError) {
			t.Fatalf("restore check for level %s: %v, %s", level, err, out)
		}
		if countRows(t, db, "settings", "key = ?", restoreFenceKey) != 0 {
			t.Fatal("the staged archive check created the boot counter")
		}
	}
	migrate(previous)
	archiveCheck("3", "")
	inviter := newTestUser(t, &App{DB: db}, "inviter", erasurePW, models.RoleAdmin)
	const email = "expired@example.com"
	inv := models.Invitation{InvitedBy: inviter.ID, Email: email, TokenHash: "expired", ExpiresAt: time.Now().Add(-31 * 24 * time.Hour)}
	erasureMustCreate(t, db, &inv)
	if err := audit.Write(db, audit.Entry{Action: audit.InvitationCreated, TargetType: "invitation", TargetID: inv.ID, Meta: map[string]any{"email": email}}); err != nil {
		t.Fatal(err)
	}

	// A worker that survives a restore can see this older schema even though
	// its own startup migrated the database it originally connected to.
	if err := (&App{DB: db}).pruneExpiredLinks(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if countRows(t, db, "invitations", "id = ?", inv.ID) != 1 {
		t.Fatal("a current worker pruned an audited invitation on the level-3 database")
	}
	migrate(current)
	const refusal = "this database needs Fold at recovery level 4 or later"
	archiveCheck("3", refusal)
	archiveCheck("0", startRefusal)
	archiveCheck("4", "")
	old := startFold(t, previous, name)
	if done, err := old.exitedWith(); !done || err == nil || !strings.Contains(old.out.String(), refusal) {
		t.Fatalf("v0.1.1 start: exited %v (%v), log: %s", done, err, old.out.String())
	}
	if countRows(t, db, "invitations", "id = ?", inv.ID) != 1 {
		t.Fatal("the invitation was already gone before the first level-4 worker")
	}
	t.Setenv("FOLD_WORKERS", "1")
	t.Setenv("FOLD_JOB_POLL", "100ms")
	app := startFold(t, current, name)
	if !app.healthy() {
		t.Fatal(app.out.String())
	}
	deadline := time.Now().Add(15 * time.Second)
	for countRows(t, db, "invitations", "id = ?", inv.ID) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the worker left the expired invitation: %s", app.out.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}
