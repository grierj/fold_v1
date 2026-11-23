package main

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"fold/models"

	"gorm.io/gorm"
)

// householdReadFixture is a household with a guardian who reads it, a second
// adult account, a teen seat and a child with care notes whose confirmed
// guardian is the reader.
type householdReadFixture struct {
	app                  *App
	adminUser, guardian  *models.User
	adult, teen          *models.User
	admin, reader        *apiClient
	household            models.Household
	child                models.HouseholdPerson
	link                 models.GuardianLink
	careNote, adultName  string
	householdPath        string
	householdChildFamily string
}

func newHouseholdReadFixture(t *testing.T) householdReadFixture {
	t.Helper()
	app := testApp(t)
	f := householdReadFixture{app: app, careNote: "Household snapshot care note", householdPath: "/api/v1/me/household", householdChildFamily: "Snapshotfamily"}
	f.adminUser = newTestUser(t, app, "household admin", "password1234", models.RoleAdmin)
	f.admin = loginAPI(t, app, f.adminUser.Name, "password1234")
	f.guardian = newTestUser(t, app, "household guardian", "password1234", models.RoleMember)
	f.adult = newTestUser(t, app, "household second adult", "password1234", models.RoleMember)
	f.adultName = f.adult.PublicName()
	f.teen = activeTeen(t, app, "household teen", f.adminUser.ID)
	f.household = models.Household{Name: "Snapshot household", CreatedBy: f.guardian.ID}
	gormMust(t, app.DB.Create(&f.household))
	for _, seat := range []models.HouseholdAccount{
		{HouseholdID: f.household.ID, UserID: f.guardian.ID, Relation: models.HouseholdRelationGuardian},
		{HouseholdID: f.household.ID, UserID: f.adult.ID, Relation: models.HouseholdRelationAdult},
		{HouseholdID: f.household.ID, UserID: f.teen.ID, Relation: models.HouseholdRelationTeen},
	} {
		gormMust(t, app.DB.Create(&seat))
	}
	birth := time.Now().UTC().AddDate(-6, 0, 0)
	f.child = models.HouseholdPerson{HouseholdID: f.household.ID, FirstName: "Hana", LastName: f.householdChildFamily, Kind: models.HouseholdChild, BirthDate: &birth, CareNotes: f.careNote, CreatedBy: f.guardian.ID}
	gormMust(t, app.DB.Create(&f.child))
	f.link = models.GuardianLink{GuardianUserID: f.guardian.ID, PersonID: &f.child.ID, Status: models.GuardianConfirmed, ConfirmedBy: &f.adminUser.ID}
	gormMust(t, app.DB.Create(&f.link))
	f.reader = loginAPI(t, app, f.guardian.Name, "password1234")
	return f
}

// TestMyHouseholdReadsOneSnapshotAndAcknowledgesIt: GET /me/household reads
// the viewer, their seat, the policy check, the household and everyone in it
// in one read-only snapshot on one connection, and a participating read
// (M8.1) is acknowledged for the viewer judged there, with the body a plain
// read gets: on the household, on "no household" and on a teen's 403. A seat
// that cannot be read is the generic 500, never "no household". An erasure of
// another adult through the real pipeline and a revoked guardian link,
// committed after the judgment, leave the answer the snapshot's. A session
// ended before the snapshot answers 401, and a failed COMMIT the generic 500,
// neither acknowledged; a plain read carries no acknowledgment.
func TestMyHouseholdReadsOneSnapshotAndAcknowledgesIt(t *testing.T) {
	f := newHouseholdReadFixture(t)
	reader := pinnedAs(t, f.reader)

	plain := f.reader.do("GET", f.householdPath, nil, false)
	wantNoAck(t, "a plain household read", plain)
	w := reader.do("GET", f.householdPath, nil, false)
	if w.Code != http.StatusOK || w.Body.String() != plain.Body.String() {
		t.Fatalf("the household: %d, the same body as a plain read %t: %s", w.Code, w.Body.String() == plain.Body.String(), w.Body.String())
	}
	wantIn(t, "the guardian's household", w.Body.String(), f.careNote, f.adultName, f.householdChildFamily, f.household.Name)
	wantAck(t, "the household", w, reader, f.guardian)

	loner := newTestUser(t, f.app, "household loner", "password1234", models.RoleMember)
	lonerC := pinnedAs(t, loginAPI(t, f.app, loner.Name, "password1234"))
	w = lonerC.do("GET", f.householdPath, nil, false)
	if w.Code != http.StatusOK || w.Body.String() != `{"household":null}` {
		t.Fatalf("no household: %d %s", w.Code, w.Body.String())
	}
	wantAck(t, "no household", w, lonerC, loner)

	teenC := pinnedAs(t, loginAPI(t, f.app, f.teen.Name, "password1234"))
	w = teenC.do("GET", f.householdPath, nil, false)
	if w.Code != http.StatusForbidden || w.Body.String() != `{"code":"not_allowed","error":"forbidden"}` {
		t.Fatalf("a teen's household: %d %s", w.Code, w.Body.String())
	}
	wantAck(t, "a teen's household", w, teenC, f.teen)

	t.Run("a failed seat read", func(t *testing.T) {
		n := failStatement(t, f.app, func(tx *gorm.DB) bool { return tx.Statement.Table == "household_accounts" })
		for _, c := range []*apiClient{reader, lonerC} {
			w := c.do("GET", f.householdPath, nil, false)
			if n.Load() == 0 || w.Code != http.StatusInternalServerError || w.Body.String() != serverErrorBody {
				t.Fatalf("a failed seat read (failed %d): %d %s", n.Load(), w.Code, w.Body.String())
			}
			wantNoAck(t, "a failed seat read", w)
		}
	})

	t.Run("a session ended before the snapshot", func(t *testing.T) {
		ended := newTestUser(t, f.app, "household ended", "password1234", models.RoleMember)
		gormMust(t, f.app.DB.Create(&models.HouseholdAccount{HouseholdID: f.household.ID, UserID: ended.ID, Relation: models.HouseholdRelationAdult}))
		c := pinnedAs(t, loginAPI(t, f.app, ended.Name, "password1234"))
		var fired atomic.Bool
		f.app.hooks.readerBegin = func(surface string) {
			if surface == "my household" && fired.CompareAndSwap(false, true) {
				gormMust(t, f.app.DB.Model(&models.User{}).Where("id = ?", ended.ID).Update("session_version", gorm.Expr("session_version + 1")))
			}
		}
		w := c.do("GET", f.householdPath, nil, false)
		f.app.hooks.readerBegin = nil
		if !fired.Load() || w.Code != http.StatusUnauthorized {
			t.Fatalf("an ended session (fired %t): %d %s", fired.Load(), w.Code, w.Body.String())
		}
		wantOut(t, "an ended session", w.Body.String(), f.careNote, f.adultName)
		wantNoAck(t, "an ended session", w)
		gormMust(t, f.app.DB.Where("user_id = ?", ended.ID).Delete(&models.HouseholdAccount{}))
	})

	t.Run("one connection", func(t *testing.T) {
		orig := f.app.DB
		one := oneConnectionPool(t, f.app)
		defer func() { f.app.DB = orig }()
		for _, c := range []*apiClient{reader, lonerC, teenC} {
			oneConnection(t, f.app, one)
			w := c.do("GET", f.householdPath, nil, false)
			if w.Code != http.StatusOK && w.Code != http.StatusForbidden {
				t.Fatalf("on one connection: %d %s (a read outside the snapshot waits for the one connection)", w.Code, w.Body.String())
			}
			if len(w.Header().Values(sessionCheckedHeader)) != 1 {
				t.Fatalf("on one connection: no acknowledgment (%d %s)", w.Code, w.Body.String())
			}
		}
		oneConnection(t, f.app, one)
		wantIn(t, "the household on one connection", apiDo(t, reader, "GET", f.householdPath, nil, http.StatusOK), f.careNote, f.adultName, f.householdChildFamily)
	})

	t.Run("a failed commit", func(t *testing.T) {
		var fired atomic.Bool
		f.app.hooks.readerEnd = func(surface string, tx *gorm.DB) {
			if surface == "my household" && fired.CompareAndSwap(false, true) {
				endBackend(t, f.app, tx)
			}
		}
		w := reader.do("GET", f.householdPath, nil, false)
		f.app.hooks.readerEnd = nil
		if !fired.Load() || w.Code != http.StatusInternalServerError || w.Body.String() != serverErrorBody {
			t.Fatalf("a failed commit (fired %t): %d %s", fired.Load(), w.Code, w.Body.String())
		}
		wantNoAck(t, "a failed commit", w)
	})

	t.Run("an erasure and a revoked link after the judgment", func(t *testing.T) {
		before := apiDo(t, reader, "GET", f.householdPath, nil, http.StatusOK)
		fired := atReaderSeam(f.app, "authorized", "my household", func() {
			apiDo(t, f.admin, "DELETE", "/api/v1/admin/users/"+itoa(f.adult.ID), nil, http.StatusOK)
			gormMust(t, f.app.DB.Model(&models.GuardianLink{}).Where("id = ?", f.link.ID).Update("status", models.GuardianRevoked))
		})
		w := reader.do("GET", f.householdPath, nil, false)
		f.app.hooks.readerAuthorized = nil
		if !fired.Load() || w.Code != http.StatusOK || w.Body.String() != before {
			t.Fatalf("the read straddled the erasure (fired %t): %d\nbefore %s\ngot    %s", fired.Load(), w.Code, before, w.Body.String())
		}
		wantAck(t, "the snapshot's household", w, reader, f.guardian)
		after := apiDo(t, reader, "GET", f.householdPath, nil, http.StatusOK)
		wantOut(t, "the next household", after, f.adultName, f.careNote)
		wantIn(t, "the next household", after, f.householdChildFamily)
	})

	t.Run("the reader's own seat removed after the judgment", func(t *testing.T) {
		before := apiDo(t, reader, "GET", f.householdPath, nil, http.StatusOK)
		wantIn(t, "the household", before, f.householdChildFamily)
		fired := atReaderSeam(f.app, "authorized", "my household", func() {
			gormMust(t, f.app.DB.Where("user_id = ?", f.guardian.ID).Delete(&models.HouseholdAccount{}))
		})
		w := reader.do("GET", f.householdPath, nil, false)
		f.app.hooks.readerAuthorized = nil
		if !fired.Load() || w.Code != http.StatusOK || w.Body.String() != before {
			t.Fatalf("the read straddled the seat's removal (fired %t): %d\nbefore %s\ngot    %s", fired.Load(), w.Code, before, w.Body.String())
		}
		wantAck(t, "the snapshot's household", w, reader, f.guardian)
		w = reader.do("GET", f.householdPath, nil, false)
		if w.Code != http.StatusOK || w.Body.String() != `{"household":null}` {
			t.Fatalf("the next read: %d %s", w.Code, w.Body.String())
		}
		wantAck(t, "no household any more", w, reader, f.guardian)
	})
}
