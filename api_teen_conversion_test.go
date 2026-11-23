package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"fold/internal/audit"
	"fold/internal/testdb"
	"fold/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type teenConversionFixture struct {
	holder  *models.User
	parent  *apiClient
	person  householdPersonJSON
	request models.AccountRequest
	user    models.User
	link    models.GuardianLink
	rows    []models.Registration
}

func pendingTeenConversion(t *testing.T, app *App, admin *apiClient, gid, name string, approve bool) teenConversionFixture {
	t.Helper()
	holder, parent, _, _ := householdFamily(t, app, admin, name)
	person := addMyPerson(t, parent, map[string]any{"firstName": "Teen " + name, "kind": "teen", "birthDate": time.Now().UTC().AddDate(-15, 0, 0).Format("2006-01-02")})
	apiDo(t, admin, "POST", "/api/v1/admin/guardian-links/"+itoa(*person.GuardianLinkID)+"/confirm", map[string]any{"checkNote": "Known household"}, 200)
	eventID, _ := seedCappedEvent(t, app, admin, gid, intPtr(1), true)
	var event models.Event
	app.DB.First(&event, eventID)
	now := time.Now().UTC().Truncate(time.Second)
	for i, status := range []string{models.RegGoing, models.RegMaybe, models.RegWaitlisted, models.RegCancelled, models.RegDeclined, models.RegRemoved} {
		at := now.AddDate(0, 0, 10+i)
		occurrence := models.EventOccurrence{EventID: event.ID, OriginalDate: calDate(at.Format("2006-01-02")), StartsAt: at, EndsAt: at.Add(time.Hour), Status: models.OccurrenceScheduled}
		if err := app.DB.Create(&occurrence).Error; err != nil {
			t.Fatal(err)
		}
		row := models.Registration{OccurrenceID: occurrence.ID, HouseholdPersonID: &person.ID, RegisteredBy: &holder.ID, Status: status, AttendanceMode: models.AttendanceInPerson, Source: "admin", WaitlistedAt: &now, CreatedAt: now, UpdatedAt: now}
		if err := app.DB.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	var rows []models.Registration
	app.DB.Where("household_person_id = ?", person.ID).Order("id").Find(&rows)
	token := inviteHouseholdPerson(t, app, parent, person.ID, name+"-own@example.com")
	acceptConversion(t, app, token, 200)
	req := pendingRequest(t, app, name+"-own@example.com")
	if req.AccountType != models.AccountTeen || req.PasswordHash == nil {
		t.Fatalf("request %+v", req)
	}
	f := teenConversionFixture{holder: holder, parent: parent, person: person, request: req, rows: rows}
	if approve {
		apiDo(t, admin, "POST", "/api/v1/admin/account-requests/"+itoa(req.ID)+"/approve", map[string]any{}, 200)
		app.DB.First(&f.request, req.ID)
		if f.request.ApprovedUserID == nil {
			t.Fatal("approval did not record account identity")
		}
		app.DB.First(&f.user, *f.request.ApprovedUserID)
		app.DB.Where("teen_user_id = ? AND guardian_user_id = ?", f.user.ID, holder.ID).First(&f.link)
		if f.user.Status != models.UserPendingConsent || f.link.Status != models.GuardianPending || f.link.ConfirmedBy != nil {
			t.Fatalf("approval bypassed consent/link: %+v %+v", f.user, f.link)
		}
	}
	return f
}

func (f teenConversionFixture) unchanged(t *testing.T, app *App) {
	t.Helper()
	var person models.HouseholdPerson
	app.DB.First(&person, f.person.ID)
	if person.ConvertedUserID != nil {
		t.Fatal("household person moved early")
	}
	var rows []models.Registration
	app.DB.Where("household_person_id = ?", f.person.ID).Order("id").Find(&rows)
	if !reflect.DeepEqual(rows, f.rows) {
		t.Fatalf("household sign-ups changed early: %+v", rows)
	}
}

func TestTeenConversionApprovalConsentAndFreshLink(t *testing.T) {
	for _, first := range []string{"consent", "link"} {
		t.Run(first, func(t *testing.T) {
			app := testApp(t)
			admin, gid := openGroup(t, app, "Teen conversion")
			f := pendingTeenConversion(t, app, admin, gid, "stage-"+first, true)
			f.unchanged(t, app)
			var occurrence models.EventOccurrence
			app.DB.First(&occurrence, f.rows[0].OccurrenceID)
			if err := app.DB.Model(&models.Event{}).Where("id = ?", occurrence.EventID).Update("visibility", models.VisibilityInviteOnly).Error; err != nil {
				t.Fatal(err)
			}
			// The stored association survives an account email change.
			apiDo(t, admin, "PUT", "/api/v1/admin/users/"+itoa(f.user.ID), map[string]string{"email": first + "-changed@example.com"}, 200)
			grant := func() {
				apiDo(t, admin, "POST", consentPath(&f.user), map[string]string{"scope": "account", "status": "granted", "evidenceText": "Signed paper consent"}, 200)
			}
			confirm := func() {
				apiDo(t, admin, "POST", "/api/v1/admin/guardian-links/"+itoa(f.link.ID)+"/confirm", map[string]string{"checkNote": "Checked teen account relationship"}, 200)
			}
			if first == "consent" {
				grant()
			} else {
				confirm()
			}
			f.unchanged(t, app)
			var waiting models.User
			app.DB.First(&waiting, f.user.ID)
			if waiting.Status != models.UserPendingConsent {
				t.Fatalf("one prerequisite activated teen: %s", waiting.Status)
			}
			apiDo(t, f.parent, "POST", conversionPath(f.person.ID), map[string]string{"email": "another@example.com"}, 409)
			apiDo(t, f.parent, "POST", conversionPath(f.person.ID)+"/resend", nil, 409)
			if first == "consent" {
				confirm()
			} else {
				grant()
			}
			var person models.HouseholdPerson
			app.DB.First(&person, f.person.ID)
			if person.ConvertedUserID == nil || *person.ConvertedUserID != f.user.ID {
				t.Fatalf("person not completed: %+v", person)
			}
			var user models.User
			app.DB.First(&user, f.user.ID)
			if user.Status != models.UserActive || user.Email != first+"-changed@example.com" {
				t.Fatalf("completed account: %+v", user)
			}
			client := loginAPI(t, app, user.Email, "password1234")
			apiDo(t, client, "GET", "/api/v1/events/"+itoa(occurrence.EventID), nil, http.StatusNotFound)
			for _, before := range f.rows {
				var after models.Registration
				app.DB.First(&after, before.ID)
				expected := before
				expected.UserID = &user.ID
				expected.HouseholdPersonID = nil
				expected.RegisteredBy = nil
				expected.ChildcareRequested = false
				if !reflect.DeepEqual(expected, after) {
					t.Fatalf("registration %d changed beyond transfer:\nwant %+v\ngot %+v", before.ID, expected, after)
				}
			}
			var householdAccount models.HouseholdAccount
			app.DB.Where("user_id = ?", user.ID).First(&householdAccount)
			if householdAccount.HouseholdID != person.HouseholdID || householdAccount.Relation != models.HouseholdRelationTeen {
				t.Fatalf("household association %+v", householdAccount)
			}
			grant()
			if auditCount(t, app, audit.HouseholdPersonConverted) != 1 {
				t.Fatal("retry transferred more than once")
			}
		})
	}
}

// A conversion's own status moves end every session and are audited, like any
// other account status change (#428). Consent first: granting it activates the
// account and the waiting conversion sends it back to pending_consent in the
// same request; the link confirmation then completes it. The rows name no actor
// and no IP: who ran a completion step is not stored (DEC-159, inventory P5).
func TestTeenConversionStatusMovesEndSessionsAndAreAudited(t *testing.T) {
	app := testApp(t)
	admin, gid := openGroup(t, app, "Conversion sessions")
	f := pendingTeenConversion(t, app, admin, gid, "sessions", true)
	load := func() models.User {
		var u models.User
		if err := app.DB.First(&u, f.user.ID).Error; err != nil {
			t.Fatal(err)
		}
		return u
	}
	statusRows := func() []map[string]any {
		var rows []models.AuditLog
		app.DB.Where("action = ? AND target_type = 'user' AND target_id = ?", audit.UserStatusChanged, f.user.ID).Order("id").Find(&rows)
		var metas []map[string]any
		for _, r := range rows {
			if r.ActorID != nil || r.IP != "" {
				t.Fatalf("status row %d has actor %v and IP %q, want neither", r.ID, r.ActorID, r.IP)
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(r.MetaJSON), &m); err != nil {
				t.Fatal(err)
			}
			metas = append(metas, m)
		}
		return metas
	}
	start := load()

	apiDo(t, admin, "POST", consentPath(&f.user), map[string]string{"scope": "account", "status": "granted", "evidenceText": "Signed paper consent"}, 200)
	waiting := load()
	// One bump for consent's activation, one for the conversion's wait.
	if waiting.Status != models.UserPendingConsent || waiting.SessionVersion != start.SessionVersion+2 {
		t.Fatalf("after consent: status %s, session version %d, want pending_consent and %d", waiting.Status, waiting.SessionVersion, start.SessionVersion+2)
	}
	want := []map[string]any{{"previousStatus": models.UserActive, "status": models.UserPendingConsent, "reason": "household_conversion"}}
	if got := statusRows(); !reflect.DeepEqual(got, want) {
		t.Fatalf("status rows after consent %v, want %v", got, want)
	}

	apiDo(t, admin, "POST", "/api/v1/admin/guardian-links/"+itoa(f.link.ID)+"/confirm", map[string]string{"checkNote": "Checked teen account relationship"}, 200)
	done := load()
	if done.Status != models.UserActive || done.SessionVersion != waiting.SessionVersion+1 {
		t.Fatalf("after completion: status %s, session version %d, want active and %d", done.Status, done.SessionVersion, waiting.SessionVersion+1)
	}
	if sessionCurrent(&done, &waiting) {
		t.Fatal("a session from before the completion is still current")
	}
	want = append(want, map[string]any{"previousStatus": models.UserPendingConsent, "status": models.UserActive, "reason": "household_conversion"})
	if got := statusRows(); !reflect.DeepEqual(got, want) {
		t.Fatalf("status rows after completion %v, want %v", got, want)
	}
}

// A consent that enables a staged teen again restores their seats and decides
// the conversion at one instant (AGENTS.md: take now once per transaction). The
// clock steps a day after its first read: the restore reads it on the eve of the
// teen's 18th birthday, when the conversion still completes; a second read
// would find an adult, and a teen conversion at 18 waits.
func TestTeenConsentRestoreAndConversionShareOneInstant(t *testing.T) {
	app := testApp(t)
	admin, gid := openGroup(t, app, "One instant")
	f := pendingTeenConversion(t, app, admin, gid, "instant", true)
	apiDo(t, admin, "POST", "/api/v1/admin/guardian-links/"+itoa(f.link.ID)+"/confirm", map[string]string{"checkNote": "Checked teen account relationship"}, 200)
	apiDo(t, admin, "POST", consentPath(&f.user), map[string]string{"scope": "account", "status": "revoked"}, 200)
	birthday := time.Now().UTC().Truncate(time.Second)
	today, err := churchClockIn(app.DB, birthday)
	if err != nil {
		t.Fatal(err)
	}
	born := time.Date(today.Year()-18, today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
	for _, row := range []struct {
		model any
		id    uint
	}{{&models.User{}, f.user.ID}, {&models.AccountRequest{}, f.request.ID}, {&models.HouseholdPerson{}, f.person.ID}} {
		if err := app.DB.Model(row.model).Where("id = ?", row.id).Update("birth_date", born).Error; err != nil {
			t.Fatal(err)
		}
	}
	var adminUser models.User
	app.DB.Where("email = ?", testEmail("admin")).First(&adminUser)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/", nil)
	reads := 0
	err = app.contactMutation(contactChange{UserID: f.user.ID}, func(tx *gorm.DB) error {
		actor, teen, err := lockConsentUsers(tx, adminUser.ID, f.user.ID)
		if err != nil {
			return err
		}
		// The gate has read the clock already; count from here.
		app.clock = func() time.Time {
			reads++
			if reads == 1 {
				return birthday.Add(-24 * time.Hour)
			}
			return birthday
		}
		return app.changeTeenConsent(tx, c, teen, actor, teenConsentChange{Scope: models.TeenConsentAccount, Status: models.ConsentGranted, EvidenceText: "Signed paper consent"})
	})
	app.clock = nil
	if err != nil {
		t.Fatal(err)
	}
	if reads == 0 {
		t.Fatal("the consent never read the app clock")
	}
	var user models.User
	app.DB.First(&user, f.user.ID)
	var person models.HouseholdPerson
	app.DB.First(&person, f.person.ID)
	if user.Status != models.UserActive || person.ConvertedUserID == nil || *person.ConvertedUserID != user.ID {
		t.Fatalf("status %s, person converted to %v; want active and converted to %d: the restore and the conversion judged different instants", user.Status, person.ConvertedUserID, user.ID)
	}
}

func TestTeenConversionOnlyOriginalHouseholdGuardianCompletes(t *testing.T) {
	app := testApp(t)
	admin, gid := openGroup(t, app, "Guardian boundary")
	f := pendingTeenConversion(t, app, admin, gid, "original", true)
	other := newTestUser(t, app, "unrelated", "password1234", models.RoleMember)
	unrelated := models.GuardianLink{GuardianUserID: other.ID, TeenUserID: &f.user.ID, Status: models.GuardianConfirmed}
	if err := app.DB.Create(&unrelated).Error; err != nil {
		t.Fatal(err)
	}
	apiDo(t, admin, "POST", consentPath(&f.user), map[string]string{"scope": "account", "status": "granted", "evidenceText": "Paper"}, 200)
	f.unchanged(t, app)
	var user models.User
	app.DB.First(&user, f.user.ID)
	if user.Status != models.UserPendingConsent {
		t.Fatal("unrelated guardian completed household relationship")
	}
}

func TestTeenConversionStagingPreservesHouseholdManagement(t *testing.T) {
	app := testApp(t)
	admin, gid := openGroup(t, app, "Stage management")
	f := pendingTeenConversion(t, app, admin, gid, "manage", true)
	path := "/api/v1/me/household/people/" + itoa(f.person.ID)
	apiDo(t, f.parent, "PUT", path, map[string]any{"firstName": "Changed", "careNotes": "must roll back"}, 409)
	apiDo(t, f.parent, "DELETE", path, nil, 409)
	apiDo(t, f.parent, "PUT", path, map[string]any{"careNotes": "Care remains manageable", "photoConsent": true}, 200)
	var person models.HouseholdPerson
	app.DB.First(&person, f.person.ID)
	if person.FullName() != f.person.Name || person.CareNotes != "Care remains manageable" || !person.PhotoConsent {
		t.Fatalf("stage mutation %+v", person)
	}
	body := apiDo(t, f.parent, "GET", "/api/v1/me/household", nil, 200)
	if !strings.Contains(body, "awaiting_consent") {
		t.Fatal(body)
	}
	apiDo(t, admin, "PUT", "/api/v1/admin/users/"+itoa(f.user.ID), map[string]string{"status": "disabled"}, 200)
	apiDo(t, admin, "POST", consentPath(&f.user), map[string]string{"scope": "account", "status": "granted", "evidenceText": "Paper"}, 200)
	apiDo(t, admin, "POST", "/api/v1/admin/guardian-links/"+itoa(f.link.ID)+"/confirm", map[string]string{"checkNote": "Checked"}, 200)
	f.unchanged(t, app)
	apiDo(t, admin, "PUT", "/api/v1/admin/users/"+itoa(f.user.ID), map[string]string{"status": "active"}, 200)
	var final models.HouseholdPerson
	app.DB.First(&final, f.person.ID)
	if final.ConvertedUserID == nil {
		t.Fatal("reenable did not complete ready conversion")
	}
}

func TestTeenConversionRejectedOrExpiredDoesNotMove(t *testing.T) {
	for _, outcome := range []string{"reject", "expire"} {
		t.Run(outcome, func(t *testing.T) {
			app := testApp(t)
			admin, gid := openGroup(t, app, "Close teen request")
			f := pendingTeenConversion(t, app, admin, gid, outcome, false)
			if outcome == "reject" {
				apiDo(t, admin, "POST", "/api/v1/admin/account-requests/"+itoa(f.request.ID)+"/reject", map[string]string{}, 200)
			} else {
				app.DB.Model(&f.request).Update("expires_at", time.Now().Add(-time.Hour))
				if _, err := app.expirePendingRequests(); err != nil {
					t.Fatal(err)
				}
			}
			f.unchanged(t, app)
			var count int64
			app.DB.Model(&models.User{}).Where("email = ?", f.request.Email).Count(&count)
			if count != 0 {
				t.Fatal("closed request created account")
			}
			inviteHouseholdPerson(t, app, f.parent, f.person.ID, outcome+"-retry@example.com")
		})
	}
}

func TestTeenConversionLocksHolderBeforeTeenAndMovesLatestAnswer(t *testing.T) {
	app := testApp(t)
	admin, gid := openGroup(t, app, "Conversion lock order")
	f := pendingTeenConversion(t, app, admin, gid, "race", true)
	apiDo(t, admin, "POST", "/api/v1/admin/guardian-links/"+itoa(f.link.ID)+"/confirm", map[string]string{"checkNote": "Checked"}, 200)
	hold := app.DB.Begin()
	if hold.Error != nil {
		t.Fatal(hold.Error)
	}
	defer hold.Rollback()
	var pid int
	hold.Raw("SELECT pg_backend_pid()").Scan(&pid)
	if err := hold.Exec("SELECT 1 FROM users WHERE id = ? FOR NO KEY UPDATE", f.holder.ID).Error; err != nil {
		t.Fatal(err)
	}
	response := make(chan int, 1)
	go func() {
		response <- admin.do("POST", consentPath(&f.user), map[string]string{"scope": "account", "status": "granted", "evidenceText": "Paper"}, true).Code
	}()
	testdb.WaitUntil(t, "consent to wait for the household holder", 5*time.Second, func() bool {
		var blocked bool
		if err := app.DB.Raw("SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname = current_database() AND ? = ANY(pg_blocking_pids(pid)))", pid).Scan(&blocked).Error; err != nil {
			t.Fatal(err)
		}
		return blocked
	})
	if err := app.DB.Transaction(func(tx *gorm.DB) error {
		return tx.Exec("SELECT 1 FROM users WHERE id = ? FOR NO KEY UPDATE NOWAIT", f.user.ID).Error
	}); err != nil {
		t.Fatalf("consent locked teen before holder: %v", err)
	}
	if err := hold.Model(&models.Registration{}).Where("id = ?", f.rows[0].ID).UpdateColumn("status", models.RegMaybe).Error; err != nil {
		t.Fatal(err)
	}
	if err := hold.Commit().Error; err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-response:
		if code != http.StatusOK {
			t.Fatalf("grant %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("conversion deadlocked")
	}
	var row models.Registration
	app.DB.First(&row, f.rows[0].ID)
	if row.UserID == nil || *row.UserID != f.user.ID || row.Status != models.RegMaybe {
		t.Fatalf("latest answer lost: %+v", row)
	}
}

func TestTeenConversionApprovalRechecksAdminAfterLockWait(t *testing.T) {
	for _, change := range []string{"disabled", "demoted"} {
		t.Run(change, func(t *testing.T) {
			app := testApp(t)
			admin, gid := openGroup(t, app, "Approval authority")
			f := pendingTeenConversion(t, app, admin, gid, "authority-"+change, false)
			var actor models.User
			app.DB.Where("email = ?", "admin@example.com").First(&actor)
			held := app.DB.Begin()
			defer held.Rollback()
			var pid int
			held.Raw("SELECT pg_backend_pid()").Scan(&pid)
			if err := held.Exec("SELECT 1 FROM users WHERE id = ? FOR NO KEY UPDATE", actor.ID).Error; err != nil {
				t.Fatal(err)
			}
			result := make(chan int, 1)
			go func() {
				result <- admin.do("POST", "/api/v1/admin/account-requests/"+itoa(f.request.ID)+"/approve", map[string]string{}, true).Code
			}()
			testdb.WaitUntil(t, "approval to reach the participant lock", 5*time.Second, func() bool {
				var blocked bool
				if err := app.DB.Raw("SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname = current_database() AND ? = ANY(pg_blocking_pids(pid)))", pid).Scan(&blocked).Error; err != nil {
					t.Fatal(err)
				}
				return blocked
			})
			updates := map[string]any{"session_version": gorm.Expr("session_version + 1")}
			if change == "disabled" {
				updates["status"] = models.UserDisabled
			} else {
				updates["role"] = models.RoleMember
			}
			if err := held.Model(&actor).Updates(updates).Error; err != nil {
				t.Fatal(err)
			}
			if err := held.Commit().Error; err != nil {
				t.Fatal(err)
			}
			select {
			case code := <-result:
				if code != 403 {
					t.Fatalf("stale actor approved: %d", code)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("approval deadlocked")
			}
			var req models.AccountRequest
			app.DB.First(&req, f.request.ID)
			if req.Status != models.RequestPending || req.ApprovedUserID != nil {
				t.Fatalf("stale actor decided request: %+v", req)
			}
			f.unchanged(t, app)
		})
	}
}
