package app

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
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

func TestMyHouseholdWriteRepliesJudgeAuthorityInTheirSnapshot(t *testing.T) {
	const surface = "household write reply"
	for _, route := range []struct {
		name, method string
		status       int
	}{
		{"save household", "PUT", http.StatusOK},
		{"edit person", "PUT", http.StatusOK},
		{"add person", "POST", http.StatusCreated},
	} {
		t.Run(route.name, func(t *testing.T) {
			f := newHouseholdReadFixture(t)
			reader := pinnedAs(t, f.reader)
			write := func() *httptest.ResponseRecorder {
				path := f.householdPath
				var body map[string]any
				switch route.name {
				case "save household":
					body = withRevision(map[string]any{"name": "Saved household"}, getMyHousehold(t, reader).Revision)
				case "edit person":
					path += "/people/" + itoa(f.child.ID)
					body = withRevision(map[string]any{"firstName": "Saved child"}, myPersonRevision(t, reader, f.child.ID))
				case "add person":
					path += "/people"
					body = map[string]any{"firstName": "New child", "kind": "child", "careNotes": f.careNote}
				}
				return reader.do(route.method, path, body, true)
			}
			setRole := func(role string) {
				gormMust(t, f.app.DB.Model(f.guardian).Update("role", role))
			}
			setRole(models.RoleAdmin)
			const laterNote = "Later snapshot care note"
			fired := atReaderSeam(f.app, "authorized", surface, func() {
				gormMust(t, &gorm.DB{Error: f.app.DB.Transaction(func(tx *gorm.DB) error {
					if err := tx.Model(f.guardian).Update("role", models.RoleMember).Error; err != nil {
						return err
					}
					if err := tx.Model(&f.link).Update("status", models.GuardianRevoked).Error; err != nil {
						return err
					}
					return tx.Model(&models.HouseholdPerson{}).Where("household_id = ?", f.household.ID).Update("care_notes", laterNote).Error
				})})
			})
			w := write()
			f.app.hooks.readerAuthorized = nil
			if !fired.Load() || w.Code != route.status {
				t.Fatalf("a reply judged before the change (fired %t): %d %s", fired.Load(), w.Code, w.Body.String())
			}
			wantIn(t, "the reply before the change", w.Body.String(), f.careNote)
			wantOut(t, "the reply before the change", w.Body.String(), laterNote)
			wantAck(t, "the reply before the change", w, reader, f.guardian)

			setRole(models.RoleAdmin)
			var began atomic.Bool
			f.app.hooks.readerBegin = func(s string) {
				if s == surface && began.CompareAndSwap(false, true) {
					setRole(models.RoleMember)
				}
			}
			w = write()
			f.app.hooks.readerBegin = nil
			if !began.Load() || w.Code != route.status {
				t.Fatalf("a reply judged after the change (fired %t): %d %s", began.Load(), w.Code, w.Body.String())
			}
			wantOut(t, "the reply after the change", w.Body.String(), `"careNotes"`, f.careNote, laterNote)
			wantAck(t, "the reply after the change", w, reader, f.guardian)

			oneConnection(t, f.app, oneConnectionPool(t, f.app))
			w = write()
			if w.Code != route.status {
				t.Fatalf("a reply on one connection: %d %s", w.Code, w.Body.String())
			}
			wantOut(t, "the reply on one connection", w.Body.String(), `"careNotes"`)
		})
	}
}

func TestMyHouseholdWriteReplyRequiresTheWritersCurrentSeatAndSession(t *testing.T) {
	for _, erased := range []bool{false, true} {
		name, status := "removed seat", http.StatusConflict
		if erased {
			name, status = "erased account", http.StatusUnauthorized
		}
		t.Run(name, func(t *testing.T) {
			f := newHouseholdReadFixture(t)
			reader := pinnedAs(t, f.reader)
			var fired atomic.Bool
			f.app.hooks.readerBegin = func(surface string) {
				if surface != "household write reply" || !fired.CompareAndSwap(false, true) {
					return
				}
				if erased {
					apiDo(t, f.admin, "DELETE", "/api/v1/admin/users/"+itoa(f.guardian.ID), nil, http.StatusOK)
				} else {
					gormMust(t, f.app.DB.Where("user_id = ?", f.guardian.ID).Delete(&models.HouseholdAccount{}))
				}
			}
			const saved = "A committed household name"
			w := reader.do("PUT", f.householdPath, withRevision(map[string]any{"name": saved}, getMyHousehold(t, reader).Revision), true)
			if !fired.Load() || w.Code != status {
				t.Fatalf("a reply with a %s (fired %t): %d %s", name, fired.Load(), w.Code, w.Body.String())
			}
			wantOut(t, "the refused reply", w.Body.String(), saved, f.careNote, f.householdChildFamily)
			if erased {
				wantNoAck(t, "the erased account's reply", w)
			} else {
				wantIn(t, "the unseated adult's reply", w.Body.String(), "household_write_unconfirmed")
				wantAck(t, "the unseated adult's reply", w, reader, f.guardian)
			}
			gormMust(t, f.app.DB.First(&f.household, f.household.ID))
			if f.household.Name != saved {
				t.Fatalf("the write did not commit before the refusal: %q", f.household.Name)
			}
		})
	}
}

func TestHouseholdPersonWriteReplyOmitsConvertedPerson(t *testing.T) {
	f := newHouseholdEditFixture(t)
	const email = "mary-own@example.com"
	acceptConversion(t, f.app, f.invite, http.StatusOK)
	var fired atomic.Bool
	f.app.hooks.readerBegin = func(surface string) {
		if surface == "household write reply" && fired.CompareAndSwap(false, true) {
			approveConversionForEmail(t, f.app, f.admin, email)
		}
	}
	path := "/api/v1/me/household/people/" + itoa(f.mary.ID)
	w := f.c.do("PUT", path, withRevision(map[string]any{"photoConsent": true}, myPersonRevision(t, f.c, f.mary.ID)), true)
	if !fired.Load() || w.Code != http.StatusConflict {
		t.Fatalf("person write reply after conversion (fired %t): %d %s", fired.Load(), w.Code, w.Body.String())
	}
	wantIn(t, "the converted person's reply", w.Body.String(), "household_write_unconfirmed")
	wantOut(t, "the converted person's reply", w.Body.String(), f.mary.FirstName, "birthDate", "careNotes", "accountUserId")
	var person models.HouseholdPerson
	gormMust(t, f.app.DB.First(&person, f.mary.ID))
	if person.ConvertedUserID == nil || !person.PhotoConsent {
		t.Fatal("the person did not convert after the edit committed")
	}
}

// householdEditFixture is a household whose holder and a second adult both
// manage it: Mary, an adult with an open account invitation and an upcoming
// sign-up, and Kit, a child with care notes and a confirmed guardian link.
type householdEditFixture struct {
	app          *App
	admin        *apiClient
	holder       *models.User
	c, other     *apiClient
	otherUser    *models.User
	mary, kit    householdPersonJSON
	householdID  uint
	eventID      uint
	gid          string
	date, invite string
}

func newHouseholdEditFixture(t *testing.T) householdEditFixture {
	t.Helper()
	app := testApp(t)
	admin, gid := openGroup(t, app, "HouseholdEdits")
	f := householdEditFixture{app: app, admin: admin, gid: gid}
	f.holder, f.c, f.mary, f.kit = householdFamily(t, app, admin, "editholder")
	f.householdID = f.mary.HouseholdID
	f.otherUser = newTestUser(t, app, "editsecond", "password1234", models.RoleMember)
	gormMust(t, app.DB.Create(&models.HouseholdAccount{HouseholdID: f.householdID, UserID: f.otherUser.ID, Relation: models.HouseholdRelationAdult}))
	f.other = loginAPI(t, app, "editsecond", "password1234")
	f.eventID, f.date = seedCappedEvent(t, app, admin, gid, intPtr(10), true)
	batchAnswer(t, f.c, f.eventID, f.date, map[string]any{"people": []map[string]any{{"personId": f.mary.ID, "status": "going"}, {"personId": f.kit.ID, "status": "going"}}}, http.StatusOK)
	f.invite = inviteHouseholdPerson(t, app, f.c, f.mary.ID, "mary-own@example.com")
	return f
}

// householdEditState is every row a household save, or a refusal of one,
// could add or change, as one string.
func householdEditState(t *testing.T, db *gorm.DB, skip ...string) string {
	t.Helper()
	var parts []string
	for _, table := range []string{"households", "household_accounts", "household_people", "guardian_links", "invitations", "account_requests", "registrations", "notifications", "audit_logs", "jobs"} {
		if !slices.Contains(skip, table) {
			parts = append(parts, table+": "+dumpRows(t, db, table, "id", "true"))
		}
	}
	return strings.Join(parts, "\n")
}

func householdPersonAnswer(t *testing.T, w *httptest.ResponseRecorder) householdPersonJSON {
	t.Helper()
	var out householdPersonResponseJSON
	decodeJSON(t, w, &out)
	return out.Person
}

// A person's edit names the read it was made from (DEC-174). Another adult's
// edit moves Mary's revision; a save from the older read, even one that would
// make her a child (revoking her invitation and cancelling her sign-up), is
// 409 with nothing written: no field, invitation, registration, link, notice,
// job or audit row. So is a save naming no revision, or another row's. The
// same change from the current read goes ahead and does all of that, and
// answers the revision the next read gives; a second save naming that answer
// saves too.
func TestHouseholdPersonEditNamesItsRead(t *testing.T) {
	f := newHouseholdEditFixture(t)
	path := "/api/v1/me/household/people/" + itoa(f.mary.ID)
	read := myPersonRevision(t, f.c, f.mary.ID)
	if read != f.mary.Revision || myPersonRevision(t, f.other, f.mary.ID) != read {
		t.Fatalf("the add answered %q, the reads give %q and %q", f.mary.Revision, read, myPersonRevision(t, f.other, f.mary.ID))
	}
	editMyPerson(t, f.other, f.mary.ID, map[string]any{"careNotes": "Needs the ramp"}, http.StatusOK)
	if myPersonRevision(t, f.c, f.mary.ID) == read {
		t.Fatal("another adult's edit left Mary's revision as it was")
	}
	child := map[string]any{"firstName": "Mary", "kind": "child", "birthDate": "2018-01-01"}
	before := householdEditState(t, f.app.DB)
	for name, body := range map[string]map[string]any{
		"a stale kind change":       withRevision(child, read),
		"a stale birth date change": withRevision(map[string]any{"birthDate": "1985-02-02"}, read),
		"stale care notes":          withRevision(map[string]any{"careNotes": "Older notes"}, read),
		"no revision":               child,
		"an empty revision":         withRevision(child, ""),
		"Kit's revision":            withRevision(child, myPersonRevision(t, f.c, f.kit.ID)),
		"the household's revision":  withRevision(child, getMyHousehold(t, f.c).Revision),
	} {
		wantCode(t, name, f.c.do("PUT", path, body, true), http.StatusConflict, "household_person_changed")
		if after := householdEditState(t, f.app.DB); after != before {
			t.Fatalf("%s wrote:\n%s\nwant\n%s", name, after, before)
		}
	}

	w := f.c.do("PUT", path, withRevision(child, myPersonRevision(t, f.c, f.mary.ID)), true)
	if w.Code != http.StatusOK {
		t.Fatalf("the current read's save: %d %s", w.Code, w.Body.String())
	}
	saved := householdPersonAnswer(t, w)
	if saved.Kind != models.HouseholdChild || saved.Revision == "" || saved.Revision != myPersonRevision(t, f.c, f.mary.ID) {
		t.Fatalf("the answer %+v, the next read's revision %q", saved, myPersonRevision(t, f.c, f.mary.ID))
	}
	if n := countRows(t, f.app.DB, "invitations", "household_person_id = ? AND revoked_at IS NOT NULL", f.mary.ID); n != 1 {
		t.Fatalf("%d revoked invitations, want the save to revoke Mary's", n)
	}
	if n := countRows(t, f.app.DB, "registrations", "household_person_id = ? AND status = ?", f.mary.ID, models.RegCancelled); n != 1 {
		t.Fatalf("%d cancelled sign-ups, want the save to cancel Mary's", n)
	}
	w = f.c.do("PUT", path, withRevision(map[string]any{"careNotes": "Ramp and a quiet room"}, saved.Revision), true)
	if w.Code != http.StatusOK {
		t.Fatalf("a save naming the last answer: %d %s", w.Code, w.Body.String())
	}
	if again := householdPersonAnswer(t, w); again.Revision == saved.Revision || again.Revision != myPersonRevision(t, f.c, f.mary.ID) {
		t.Fatalf("the second answer's revision %q, the first's %q, the next read's %q", again.Revision, saved.Revision, myPersonRevision(t, f.c, f.mary.ID))
	}
}

// The household's name and address save names its read too (DEC-174). An
// empty body names no field and needs none; the request that makes the
// household on first use had no read to name, so its fields save without one.
func TestHouseholdDetailsSaveNamesItsRead(t *testing.T) {
	f := newHouseholdEditFixture(t)
	read := getMyHousehold(t, f.c).Revision
	if read == "" || getMyHousehold(t, f.other).Revision != read {
		t.Fatalf("the reads give %q and %q", read, getMyHousehold(t, f.other).Revision)
	}
	editMyHousehold(t, f.other, map[string]any{"name": "Editholder family", "address": "2 Elm St"}, http.StatusOK)
	if getMyHousehold(t, f.c).Revision == read {
		t.Fatal("another adult's save left the household's revision as it was")
	}
	before := householdEditState(t, f.app.DB)
	for name, body := range map[string]map[string]any{
		"a stale rename":           withRevision(map[string]any{"name": "Older name"}, read),
		"a stale address":          withRevision(map[string]any{"address": "1 Oak St"}, read),
		"no revision":              {"name": "Older name"},
		"an empty revision":        withRevision(map[string]any{"name": "Older name"}, ""),
		"Mary's revision":          withRevision(map[string]any{"name": "Older name"}, myPersonRevision(t, f.c, f.mary.ID)),
		"another household's":      withRevision(map[string]any{"name": "Older name"}, f.app.editRevision(fmt.Sprintf("h%d", f.householdID+1), "0")),
		"an empty body is no save": {},
	} {
		w := f.c.do("PUT", "/api/v1/me/household", body, true)
		if len(body) == 0 {
			wantCode(t, name, w, http.StatusOK, "")
		} else {
			wantCode(t, name, w, http.StatusConflict, "household_changed")
		}
		if after := householdEditState(t, f.app.DB); after != before {
			t.Fatalf("%s wrote:\n%s\nwant\n%s", name, after, before)
		}
	}
	w := f.c.do("PUT", "/api/v1/me/household", withRevision(map[string]any{"name": "The Editholders"}, getMyHousehold(t, f.c).Revision), true)
	if w.Code != http.StatusOK {
		t.Fatalf("the current read's save: %d %s", w.Code, w.Body.String())
	}
	var saved householdResponseJSON
	decodeJSON(t, w, &saved)
	if saved.Household.Name != "The Editholders" || saved.Household.Address != "2 Elm St" || saved.Household.Revision != getMyHousehold(t, f.c).Revision {
		t.Fatalf("the answer %+v, the next read's revision %q", saved.Household, getMyHousehold(t, f.c).Revision)
	}
	apiDo(t, f.c, "PUT", "/api/v1/me/household", withRevision(map[string]any{"address": ""}, saved.Household.Revision), http.StatusOK)

	newTestUser(t, f.app, "editfirstuse", "password1234", models.RoleMember)
	first := loginAPI(t, f.app, "editfirstuse", "password1234")
	apiDo(t, first, "PUT", "/api/v1/me/household", map[string]any{"name": "Fresh start"}, http.StatusOK)
	if h := getMyHousehold(t, first); h.Name != "Fresh start" {
		t.Fatalf("first use saved %+v", h)
	}
	wantCode(t, "a second save naming no read", first.do("PUT", "/api/v1/me/household", map[string]any{"name": "Again"}, true), http.StatusConflict, "household_changed")
}

// An adult whose session ends, or who leaves the household, while their save
// waits on their own user row (the erasure's lock) is refused when the lock is
// let go, and nothing is written. Each save names the current read.
func TestHouseholdSavesByAnAdultRevokedWhileTheyWaitWriteNothing(t *testing.T) {
	for _, save := range []struct {
		name, gone string
		path       func(f householdEditFixture) string
		body       func(t *testing.T, f householdEditFixture) map[string]any
		removed    int
	}{
		{name: "household", path: func(householdEditFixture) string { return "/api/v1/me/household" }, removed: http.StatusForbidden,
			body: func(t *testing.T, f householdEditFixture) map[string]any {
				return withRevision(map[string]any{"name": "Late"}, getMyHousehold(t, f.other).Revision)
			}},
		{name: "person", path: func(f householdEditFixture) string { return "/api/v1/me/household/people/" + itoa(f.mary.ID) }, removed: http.StatusNotFound,
			body: func(t *testing.T, f householdEditFixture) map[string]any {
				return withRevision(map[string]any{"kind": "child", "birthDate": "2018-01-01"}, myPersonRevision(t, f.other, f.mary.ID))
			}},
	} {
		for change, want := range map[string]int{"session ended": http.StatusUnauthorized, "left the household": save.removed} {
			t.Run(save.name+", "+change, func(t *testing.T) {
				f := newHouseholdEditFixture(t)
				body := save.body(t, f)
				before := householdEditState(t, f.app.DB, "household_accounts")
				holder := f.app.DB.Begin()
				t.Cleanup(func() { _ = holder.Rollback() })
				gormMust(t, holder.Exec("SELECT 1 FROM users WHERE id = ? FOR UPDATE", f.otherUser.ID))
				if change == "session ended" {
					gormMust(t, holder.Exec("UPDATE users SET session_version = session_version + 1 WHERE id = ?", f.otherUser.ID))
				} else {
					gormMust(t, holder.Exec("DELETE FROM household_accounts WHERE user_id = ?", f.otherUser.ID))
				}
				res := bgDo(f.other, "PUT", save.path(f), body)
				hwWaiters(t, f.app, 1)
				gormMust(t, holder.Commit())
				if got := awaitResult(t, res); got.code != want {
					t.Fatalf("the waiting save: %d %s, want %d", got.code, got.body, want)
				}
				if after := householdEditState(t, f.app.DB, "household_accounts"); after != before {
					t.Fatalf("the waiting save wrote:\n%s\nwant\n%s", after, before)
				}
			})
		}
	}
}

// A save that waits on the household's lock compares after the wait: the
// holder of the lock changes the row and commits, and the waiting save, named
// from the read before, is refused with nothing more written.
func TestHouseholdSavesCompareAfterTheLockWait(t *testing.T) {
	for _, save := range []struct {
		name, code, table, change, kept string
		path                            func(f householdEditFixture) string
		body                            func(t *testing.T, f householdEditFixture) map[string]any
	}{
		{name: "household", code: "household_changed", table: "households", change: "UPDATE households SET address = 'Changed meanwhile', updated_at = now() WHERE id = ?", kept: "address = 'Changed meanwhile'",
			path: func(householdEditFixture) string { return "/api/v1/me/household" },
			body: func(t *testing.T, f householdEditFixture) map[string]any {
				return withRevision(map[string]any{"name": "Late"}, getMyHousehold(t, f.c).Revision)
			}},
		{name: "person", code: "household_person_changed", table: "household_people", change: "UPDATE household_people SET care_notes = 'Changed meanwhile', updated_at = now() WHERE household_id = ?", kept: "care_notes = 'Changed meanwhile' AND kind = 'adult'",
			path: func(f householdEditFixture) string { return "/api/v1/me/household/people/" + itoa(f.mary.ID) },
			body: func(t *testing.T, f householdEditFixture) map[string]any {
				return withRevision(map[string]any{"kind": "child", "birthDate": "2018-01-01"}, myPersonRevision(t, f.c, f.mary.ID))
			}},
	} {
		t.Run(save.name, func(t *testing.T) {
			f := newHouseholdEditFixture(t)
			body := save.body(t, f)
			before := householdEditState(t, f.app.DB, save.table)
			holder := f.app.DB.Begin()
			t.Cleanup(func() { _ = holder.Rollback() })
			gormMust(t, holder.Exec("SELECT 1 FROM households WHERE id = ? FOR UPDATE", f.householdID))
			res := bgDo(f.c, "PUT", save.path(f), body)
			waitForLockedUsersQuery(t, f.app, `%"households"%FOR UPDATE%`)
			gormMust(t, holder.Exec(save.change, f.householdID))
			gormMust(t, holder.Commit())
			got := awaitResult(t, res)
			if got.code != http.StatusConflict || !strings.Contains(got.body, save.code) {
				t.Fatalf("the save that waited: %d %s, want 409 %s", got.code, got.body, save.code)
			}
			if after := householdEditState(t, f.app.DB, save.table); after != before {
				t.Fatalf("the save that waited wrote:\n%s\nwant\n%s", after, before)
			}
			if n := countRows(t, f.app.DB, save.table, save.kept); n == 0 {
				t.Fatalf("no %s row is as the lock's holder left it (%s)", save.table, save.kept)
			}
		})
	}
}

// The household read gives each revision in the snapshot that read the fields
// beside it: an edit committed right after the people were read leaves the
// answer with Mary's older care notes and her older revision, so a save made
// from it is refused instead of overwriting the edit.
func TestMyHouseholdReadsFieldsAndRevisionsInOneSnapshot(t *testing.T) {
	f := newHouseholdEditFixture(t)
	var fired atomic.Bool
	name := "test:household_people_seam"
	gormMust(t, &gorm.DB{Error: f.app.DB.Callback().Query().After("gorm:query").Register(name, func(tx *gorm.DB) {
		if tx.Statement.Table == "household_people" && strings.Contains(tx.Statement.SQL.String(), "converted_user_id IS NULL") && fired.CompareAndSwap(false, true) {
			if err := f.app.DB.Exec("UPDATE household_people SET care_notes = 'Committed meanwhile', updated_at = now() WHERE id = ?", f.kit.ID).Error; err != nil {
				t.Error(err)
			}
		}
	})})
	t.Cleanup(func() { _ = f.app.DB.Callback().Query().Remove(name) })
	var shown householdPersonJSON
	for _, p := range getMyHousehold(t, f.c).People {
		if p.ID == f.kit.ID {
			shown = p
		}
	}
	if !fired.Load() {
		t.Fatal("the read never reached its seam")
	}
	if shown.CareNotes == nil || *shown.CareNotes != "Private allergy note" {
		t.Fatalf("the read showed %+v, want the care notes its snapshot read", shown)
	}
	before := householdEditState(t, f.app.DB)
	wantCode(t, "a save made from the read", f.c.do("PUT", "/api/v1/me/household/people/"+itoa(f.kit.ID),
		withRevision(map[string]any{"firstName": "Kitty", "careNotes": "Private allergy note"}, shown.Revision), true), http.StatusConflict, "household_person_changed")
	if after := householdEditState(t, f.app.DB); after != before {
		t.Fatalf("the refused save wrote:\n%s\nwant\n%s", after, before)
	}
}

// What moves a person's revision: every column of the row (another adult's
// edit, the conversion of the person to their own account, the age rollover,
// a removal or an erasure each write one), and a write that only stamps
// updated_at. A sign-up for the person, a guardian link's decision and an
// admin adding someone else leave it, and the household's, alone; the token is
// the row's, whoever reads it.
func TestHouseholdRevisionsFollowTheirRows(t *testing.T) {
	f := newHouseholdEditFixture(t)
	token := func(rows revisionRows, id uint) string {
		t.Helper()
		got, err := f.app.rowRevisionIn(f.app.DB, rows, id)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	kit := token(householdPersonRevisionRows, f.kit.ID)
	if !regexp.MustCompile(`^1\.[0-9a-f]{16}\.p`+itoa(f.kit.ID)+`\.[0-9a-f]{32}$`).MatchString(kit) || kit != myPersonRevision(t, f.c, f.kit.ID) {
		t.Fatalf("Kit's token %q, the read's %q", kit, myPersonRevision(t, f.c, f.kit.ID))
	}
	household := token(householdRevisionRows, f.householdID)
	if !regexp.MustCompile(`^1\.[0-9a-f]{16}\.h` + itoa(f.householdID) + `\.[0-9a-f]{32}$`).MatchString(household) {
		t.Fatalf("the household's token %q", household)
	}

	eventID, date := seedCappedEvent(t, f.app, f.admin, f.gid, intPtr(5), true)
	batchAnswer(t, f.c, eventID, date, map[string]any{"people": []map[string]any{{"personId": f.kit.ID, "status": "going"}}}, http.StatusOK)
	var link models.GuardianLink
	gormMust(t, f.app.DB.Where("person_id = ?", f.kit.ID).First(&link))
	gormMust(t, f.app.DB.Model(&link).Update("check_note", "Checked again"))
	var adminRow householdPersonResponseJSON
	decodeJSON(t, f.admin.do("POST", "/api/v1/admin/users/"+itoa(f.holder.ID)+"/household/people",
		map[string]any{"firstName": "Lou", "kind": "adult", "birthDate": "1980-03-03"}, true), &adminRow)
	if adminRow.Person.Revision == "" || adminRow.Person.Revision != myPersonRevision(t, f.c, adminRow.Person.ID) {
		t.Fatalf("the admin's answer gave Lou %q, the household's read %q", adminRow.Person.Revision, myPersonRevision(t, f.c, adminRow.Person.ID))
	}
	if now := token(householdPersonRevisionRows, f.kit.ID); now != kit {
		t.Fatalf("a sign-up, a link's note or another person's add moved Kit's token to %q", now)
	}
	if now := token(householdRevisionRows, f.householdID); now != household {
		t.Fatalf("a person's add or a sign-up moved the household's token to %q", now)
	}

	for _, change := range []string{
		"updated_at = updated_at + interval '1 second'", "first_name = 'Kitt'", "last_name = 'Lee'", "kind = 'teen'",
		"birth_date = birth_date - 1", "photo_consent = NOT photo_consent",
		"created_by = " + itoa(f.otherUser.ID), "converted_user_id = " + itoa(f.otherUser.ID), "converted_user_id = NULL",
		"deleted_at = now()", "deleted_at = NULL",
	} {
		before := token(householdPersonRevisionRows, f.kit.ID)
		gormMust(t, f.app.DB.Exec("UPDATE household_people SET "+change+" WHERE id = ?", f.kit.ID))
		if now := token(householdPersonRevisionRows, f.kit.ID); now == before {
			t.Errorf("%s left Kit's token at %q", change, now)
		}
	}
	// The care notes are no part of the digest, so an adult who can't read them
	// can't test a guess at them; every write of them moves the token anyway,
	// by updated_at (a care-notes-only edit, TestHouseholdPersonEditNamesItsRead)
	// or deleted_at (an erasure).
	before := token(householdPersonRevisionRows, f.kit.ID)
	gormMust(t, f.app.DB.Exec("UPDATE household_people SET care_notes = 'Unseen guess' WHERE id = ?", f.kit.ID))
	if now := token(householdPersonRevisionRows, f.kit.ID); now != before {
		t.Fatalf("the care notes alone moved Kit's token to %q: they are in the digest", now)
	}
	editMyPerson(t, f.c, f.kit.ID, map[string]any{"careNotes": "Edited notes"}, http.StatusOK)
	if now := token(householdPersonRevisionRows, f.kit.ID); now == before {
		t.Fatal("a care-notes-only edit left Kit's token as it was")
	}
	for _, change := range []string{"updated_at = updated_at + interval '1 second'", "name = 'Renamed'", "address = 'Somewhere'", "created_by = " + itoa(f.otherUser.ID)} {
		before := token(householdRevisionRows, f.householdID)
		gormMust(t, f.app.DB.Exec("UPDATE households SET "+change+" WHERE id = ?", f.householdID))
		if now := token(householdRevisionRows, f.householdID); now == before {
			t.Errorf("%s left the household's token at %q", change, now)
		}
	}

	mary := token(householdPersonRevisionRows, f.mary.ID)
	acceptConversion(t, f.app, f.invite, http.StatusOK)
	if now := token(householdPersonRevisionRows, f.mary.ID); now != mary {
		t.Fatalf("accepting the invitation, which writes no person row, moved Mary's token to %q", now)
	}
	approveConversionForEmail(t, f.app, f.admin, "mary-own@example.com")
	if now := token(householdPersonRevisionRows, f.mary.ID); now == mary {
		t.Fatal("the conversion left Mary's token as it was")
	}
}

// The answers of the household writes read each revision in the snapshot that
// read the fields beside it (toHouseholdJSON, householdPersonRow): an edit
// committed right after the row was read leaves the answer with the older
// fields and the older revision, never the older fields with the newer one.
func TestHouseholdAnswersReadFieldsAndRevisionsInOneSnapshot(t *testing.T) {
	f := newHouseholdEditFixture(t)
	seam := func(t *testing.T, table, change string, id uint) *atomic.Bool {
		var fired atomic.Bool
		name := "test:household_answer_seam_" + table
		gormMust(t, &gorm.DB{Error: f.app.DB.Callback().Query().After("gorm:query").Register(name, func(tx *gorm.DB) {
			if tx.Statement.Table == table && fired.CompareAndSwap(false, true) {
				if err := f.app.DB.Exec(change, id).Error; err != nil {
					t.Error(err)
				}
			}
		})})
		t.Cleanup(func() { _ = f.app.DB.Callback().Query().Remove(name) })
		return &fired
	}
	token := func(rows revisionRows, id uint) string {
		got, err := f.app.rowRevisionIn(f.app.DB, rows, id)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	t.Run("the household", func(t *testing.T) {
		before := token(householdRevisionRows, f.householdID)
		fired := seam(t, "households", "UPDATE households SET name = 'Committed meanwhile', updated_at = now() WHERE id = ?", f.householdID)
		out, err := f.app.toHouseholdJSON(f.householdID, f.holder)
		if err != nil {
			t.Fatal(err)
		}
		if !fired.Load() || out.Name == "Committed meanwhile" || out.Revision != before || token(householdRevisionRows, f.householdID) == before {
			t.Fatalf("fired %t: the answer %q with %q, the token before %q", fired.Load(), out.Name, out.Revision, before)
		}
	})
	t.Run("a person", func(t *testing.T) {
		before := token(householdPersonRevisionRows, f.kit.ID)
		fired := seam(t, "household_people", "UPDATE household_people SET care_notes = 'Committed meanwhile', updated_at = now() WHERE id = ?", f.kit.ID)
		row, err := f.app.householdPersonRow(f.kit.ID, f.holder)
		if err != nil {
			t.Fatal(err)
		}
		if !fired.Load() || row.CareNotes == nil || *row.CareNotes != "Private allergy note" || row.Revision != before || token(householdPersonRevisionRows, f.kit.ID) == before {
			t.Fatalf("fired %t: the answer %+v, the token before %q", fired.Load(), row, before)
		}
	})
}

// A first use's save is compared with the household as its own creation left
// it (DEC-174): the household is committed before the body is read, so a second
// device can read it and save first. That save stands, and the first use's
// fields are refused with nothing written, not stored over it.
func TestHouseholdFirstUseComparesWithItsCreation(t *testing.T) {
	app := testApp(t)
	newTestUser(t, app, "firstuse", "password1234", models.RoleMember)
	first := loginAPI(t, app, "firstuse", "password1234")
	second := loginAPI(t, app, "firstuse", "password1234")
	held := first.doHeld("PUT", "/api/v1/me/household", map[string]any{"name": "First draft"})
	editMyHousehold(t, second, map[string]any{"name": "Second device"}, http.StatusOK)
	before := householdEditState(t, app.DB)
	held.release()
	wantCode(t, "the first use's save", held.wait(t), http.StatusConflict, "household_changed")
	if after := householdEditState(t, app.DB); after != before {
		t.Fatalf("the refused first use wrote:\n%s\nwant\n%s", after, before)
	}
	if h := getMyHousehold(t, second); h.Name != "Second device" {
		t.Fatalf("the household is named %q, want the second device's save", h.Name)
	}

	// With nothing in between, the first use's fields save.
	newTestUser(t, app, "firstuse2", "password1234", models.RoleMember)
	alone := loginAPI(t, app, "firstuse2", "password1234")
	apiDo(t, alone, "PUT", "/api/v1/me/household", map[string]any{"name": "Fresh start"}, http.StatusOK)
	if h := getMyHousehold(t, alone); h.Name != "Fresh start" {
		t.Fatalf("first use saved %+v", h)
	}
}

// A person who became their own account can no longer be edited here, and the
// answer says so the same way whether the conversion came before the edit or
// while it waited for the household's lock: 409 household_person_converted,
// which the open form takes as the person gone from the card.
func TestHouseholdPersonEditOfAConvertedPersonSaysSo(t *testing.T) {
	f := newHouseholdEditFixture(t)
	path := "/api/v1/me/household/people/" + itoa(f.mary.ID)
	body := withRevision(map[string]any{"firstName": "Maria"}, myPersonRevision(t, f.c, f.mary.ID))
	holder := f.app.DB.Begin()
	t.Cleanup(func() { _ = holder.Rollback() })
	gormMust(t, holder.Exec("SELECT 1 FROM households WHERE id = ? FOR UPDATE", f.householdID))
	res := bgDo(f.c, "PUT", path, body)
	waitForLockedUsersQuery(t, f.app, `%"households"%FOR UPDATE%`)
	gormMust(t, holder.Exec("UPDATE household_people SET converted_user_id = ?, updated_at = now() WHERE id = ?", f.otherUser.ID, f.mary.ID))
	gormMust(t, holder.Commit())
	before := householdEditState(t, f.app.DB)
	if got := awaitResult(t, res); got.code != http.StatusConflict || !strings.Contains(got.body, "household_person_converted") {
		t.Fatalf("an edit that waited on the conversion: %d %s, want 409 household_person_converted", got.code, got.body)
	}
	wantCode(t, "an edit after the conversion", f.c.do("PUT", path, body, true), http.StatusConflict, "household_person_converted")
	if after := householdEditState(t, f.app.DB); after != before {
		t.Fatalf("the refused edits wrote:\n%s\nwant\n%s", after, before)
	}
}

func TestHouseholdPersonEditRefusesRemovalAfterPreflight(t *testing.T) {
	f := newHouseholdEditFixture(t)
	path := "/api/v1/me/household/people/" + itoa(f.mary.ID)
	body := withRevision(map[string]any{"firstName": "Maria"}, myPersonRevision(t, f.c, f.mary.ID))
	held := f.c.doHeld("PUT", path, body)
	apiDo(t, f.other, "DELETE", path, nil, http.StatusOK)
	before := householdEditState(t, f.app.DB)
	want := apiDo(t, f.other, "PUT", path, body, http.StatusNotFound)
	held.release()
	if w := held.wait(t); w.Code != http.StatusNotFound || w.Body.String() != want {
		t.Fatalf("the edit after removal committed: %d %s, want 404 %s", w.Code, w.Body.String(), want)
	}
	if after := householdEditState(t, f.app.DB); after != before {
		t.Fatalf("the refused edit wrote:\n%s\nwant\n%s", after, before)
	}
}

func TestHouseholdPersonEditLockedLookupFailureAnswers500(t *testing.T) {
	f := newHouseholdEditFixture(t)
	path := "/api/v1/me/household/people/" + itoa(f.mary.ID)
	body := withRevision(map[string]any{"firstName": "Maria"}, myPersonRevision(t, f.c, f.mary.ID))
	before := householdEditState(t, f.app.DB)
	held := f.c.doHeld("PUT", path, body)
	failures := failStatement(t, f.app, func(tx *gorm.DB) bool {
		return tx.Statement.Table == "household_people"
	})
	held.release()
	w := held.wait(t)
	if n := failures.Load(); n != 1 || w.Code != http.StatusInternalServerError || w.Body.String() != serverErrorBody {
		t.Fatalf("the locked lookup with %d SQL failures: %d %s, want one failure and 500 %s", n, w.Code, w.Body.String(), serverErrorBody)
	}
	if after := householdEditState(t, f.app.DB); after != before {
		t.Fatalf("the failed edit wrote:\n%s\nwant\n%s", after, before)
	}
}
