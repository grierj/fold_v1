package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"fold/internal/notify"
	"fold/internal/policy"
	"fold/models"

	"gorm.io/gorm"
)

func TestRegistrationSubjectIdentity(t *testing.T) {
	user, person, holder := uint(7), uint(7), uint(9)
	for _, tt := range []struct {
		name   string
		r      models.Registration
		holder uint
		valid  bool
	}{
		{"user", models.Registration{UserID: &user}, user, true},
		{"household shares numeric id", models.Registration{HouseholdPersonID: &person, RegisteredBy: &holder}, holder, true},
		{"neither", models.Registration{}, 0, false},
		{"both", models.Registration{UserID: &user, HouseholdPersonID: &person, RegisteredBy: &holder}, 0, false},
		{"missing holder", models.Registration{HouseholdPersonID: &person}, 0, false},
		{"user has household holder", models.Registration{UserID: &user, RegisteredBy: &holder}, 0, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, err := subjectOf(tt.r)
			if (err == nil) != tt.valid {
				t.Fatalf("subject: %+v %v", s, err)
			}
			if tt.valid && s.holderID != tt.holder {
				t.Fatalf("holder %d", s.holderID)
			}
		})
	}
}

func TestListedRegistrationSubjects(t *testing.T) {
	app := testApp(t)
	holder := newTestUser(t, app, "holder", "password1234", models.RoleMember)
	other := newTestUser(t, app, "other", "password1234", models.RoleMember)
	h := models.Household{Name: "Household", CreatedBy: holder.ID}
	if err := app.DB.Create(&h).Error; err != nil {
		t.Fatal(err)
	}
	if err := app.DB.Create(&models.HouseholdAccount{HouseholdID: h.ID, UserID: holder.ID, Relation: models.HouseholdRelationAdult}).Error; err != nil {
		t.Fatal(err)
	}
	birth := time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC)
	person := models.HouseholdPerson{HouseholdID: h.ID, FirstName: "Mary", Kind: models.HouseholdAdult, BirthDate: &birth, CreatedBy: holder.ID}
	if err := app.DB.Create(&person).Error; err != nil {
		t.Fatal(err)
	}
	subject, err := householdSubject(app.DB, person.ID, holder.ID)
	if err != nil {
		t.Fatal(err)
	}
	if subject.holderID != holder.ID {
		t.Fatal("wrong holder")
	}
	if _, err := householdSubject(app.DB, person.ID, other.ID); err == nil {
		t.Fatal("unrelated holder admitted")
	}
	// A real event supplies foreign keys without going through a fake user.
	admin, gid := openGroup(t, app, "Subjects")
	eventID, date := seedCappedEvent(t, app, admin, gid, nil, true)
	var occ models.EventOccurrence
	if err := app.DB.Where("event_id = ? AND original_date = ?", eventID, date).First(&occ).Error; err != nil {
		t.Fatal(err)
	}
	r := subject.row(occ.ID)
	r.Status = models.RegGoing
	r.AttendanceMode = models.AttendanceInPerson
	if err := app.DB.Create(&r).Error; err != nil {
		t.Fatal(err)
	}
	assertSeats := func(want int) {
		t.Helper()
		got, err := seatsUsedForMode(app.DB, occ.ID, models.AttendanceInPerson, 0)
		if err != nil || got != want {
			t.Fatalf("seats %d want %d: %v", got, want, err)
		}
	}
	assertSeats(1)
	if err := app.DB.Model(holder).Update("status", models.UserDisabled).Error; err != nil {
		t.Fatal(err)
	}
	assertSeats(0)
	app.DB.Model(holder).Update("status", models.UserActive)
	assertSeats(1)
	app.DB.Model(&person).Update("converted_user_id", other.ID)
	assertSeats(0)
	app.DB.Model(&person).Update("converted_user_id", nil)
	assertSeats(1)
	app.DB.Delete(&person)
	assertSeats(0)
}

func TestHouseholdAttendeeCareProjection(t *testing.T) {
	holderID, personID := uint(1), uint(2)
	start := time.Date(2026, 11, 1, 18, 0, 0, 0, time.UTC)
	birth := time.Date(2020, 11, 2, 0, 0, 0, 0, time.UTC)
	person := models.HouseholdPerson{ID: personID, HouseholdID: 3, FirstName: "Kit", Kind: models.HouseholdChild, BirthDate: &birth, CareNotes: "Private care\n" + strings.Repeat("long", 100)}
	holder := models.User{ID: holderID, Name: "John Smith", Status: models.UserActive}
	manager := models.User{ID: 4, Name: "Host", Status: models.UserActive}
	f := subjectFacts{people: map[uint]*models.HouseholdPerson{personID: &person}, links: map[uint][]models.GuardianLink{personID: {{PersonID: &personID, GuardianUserID: holderID, Status: models.GuardianConfirmed}}}, households: map[uint]models.Household{3: {ID: 3, Name: "Leader household", Address: "Never expose address"}}, users: map[uint]*models.User{holderID: &holder}, accounts: map[uint]models.HouseholdAccount{holderID: {HouseholdID: 3, UserID: holderID, Relation: models.HouseholdRelationGuardian}}}
	r := models.Registration{ID: 10, HouseholdPersonID: &personID, RegisteredBy: &holderID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, ChildcareRequested: true}
	o := models.EventOccurrence{ID: 5, EventID: 6, Event: models.Event{TZID: "UTC"}, StartsAt: start, EndsAt: start.Add(time.Hour), Status: models.OccurrenceScheduled}
	for _, tt := range []struct {
		name          string
		now           time.Time
		status        string
		manager       bool
		viewer        *models.User
		visible, care bool
	}{
		{"manager exact opening", start.Add(-24 * time.Hour), models.RegGoing, true, &manager, true, true},
		{"manager exact closing", start.Add(25 * time.Hour), models.RegGoing, true, &manager, true, true},
		{"manager too early", start.Add(-24*time.Hour - time.Nanosecond), models.RegGoing, true, &manager, true, false},
		{"manager too late", start.Add(25*time.Hour + time.Nanosecond), models.RegGoing, true, &manager, true, false},
		{"wrong event manager", start, models.RegGoing, false, &manager, false, false},
		{"waitlisted", start, models.RegWaitlisted, true, &manager, true, false},
		{"maybe", start, models.RegMaybe, true, &manager, true, false},
		{"removed", start, models.RegRemoved, true, &manager, true, false},
		{"guardian outside window", start.Add(-40 * time.Hour), models.RegGoing, false, &holder, true, true},
		{"anonymous", start, models.RegGoing, false, nil, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r.Status = tt.status
			f.today = start // the age clock is fixed; only the care window moves
			dto := f.attendee(r, o, tt.viewer, tt.manager, true, tt.now)
			if (dto != nil) != tt.visible {
				t.Fatalf("visibility: %+v", dto)
			}
			if dto == nil {
				return
			}
			hp := dto.HouseholdPerson
			if (hp.CareNotes != nil) != tt.care {
				t.Fatalf("care: %+v", hp)
			}
			if hp.Age == nil || *hp.Age != 5 || dto.UserID != nil || hp.RegisteredBy.Name != "John Smith" {
				t.Fatalf("identity or age: %+v", dto)
			}
			raw, _ := json.Marshal(dto)
			for _, secret := range []string{"2020-11-02", "Never expose address"} {
				if strings.Contains(string(raw), secret) {
					t.Fatalf("private data %s", raw)
				}
			}
		})
	}
}

func TestHouseholdClockInUsesTransactionTimezone(t *testing.T) {
	app := testApp(t)
	if err := app.DB.Create(&models.Setting{Key: settingTimezone, Value: "UTC"}).Error; err != nil {
		t.Fatal(err)
	}
	tx := app.DB.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	if err := tx.Model(&models.Setting{}).Where("key = ?", settingTimezone).Update("value", "America/Boise").Error; err != nil {
		t.Fatal(err)
	}

	// The church date changes at local midnight, including across DST.
	for _, tt := range []struct {
		instant, want string
	}{
		{"2026-10-22T00:30:00Z", "2026-10-21T18:30:00-06:00"},
		{"2026-11-01T07:30:00Z", "2026-11-01T01:30:00-06:00"},
		{"2026-11-01T08:30:00Z", "2026-11-01T01:30:00-07:00"},
	} {
		now, err := time.Parse(time.RFC3339, tt.instant)
		if err != nil {
			t.Fatal(err)
		}
		got, err := churchClockIn(tx, now)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Equal(now) || got.Format(time.RFC3339) != tt.want || got.Location().String() != "America/Boise" {
			t.Fatalf("transaction clock for %s = %s in %s; want %s in America/Boise", tt.instant, got.Format(time.RFC3339), got.Location(), tt.want)
		}
		if outside, err := churchClockIn(app.DB, now); err != nil || outside.Location() != time.UTC || !outside.Equal(now) {
			t.Fatalf("request clock read uncommitted timezone: %v", outside)
		}
	}
}

func TestSubjectFactsChurchLocalBirthday(t *testing.T) {
	app := testApp(t)
	holder := newTestUser(t, app, "clockholder", "password1234", models.RoleMember)
	household := models.Household{Name: "Clock family", CreatedBy: holder.ID}
	if err := app.DB.Create(&household).Error; err != nil {
		t.Fatal(err)
	}
	if err := app.DB.Create(&models.HouseholdAccount{HouseholdID: household.ID, UserID: holder.ID, Relation: models.HouseholdRelationAdult}).Error; err != nil {
		t.Fatal(err)
	}
	birth := time.Date(2008, 10, 22, 0, 0, 0, 0, time.UTC)
	person := models.HouseholdPerson{HouseholdID: household.ID, FirstName: "Birthday", Kind: models.HouseholdAdult, BirthDate: &birth, CreatedBy: holder.ID}
	if err := app.DB.Create(&person).Error; err != nil {
		t.Fatal(err)
	}
	row := models.Registration{ID: 1, HouseholdPersonID: &person.ID, RegisteredBy: &holder.ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson}
	for _, tt := range []struct {
		zone, instant string
		age           int
		eligible      bool
	}{
		{"America/Boise", "2026-10-22T00:30:00Z", 17, false},
		{"Pacific/Kiritimati", "2026-10-21T23:30:00Z", 18, true},
		{"America/Boise", "2026-10-22T06:00:00Z", 18, true},
	} {
		t.Run(tt.zone+tt.instant, func(t *testing.T) {
			app.DB.Where(models.Setting{Key: settingTimezone}).Assign(models.Setting{Value: tt.zone}).FirstOrCreate(&models.Setting{})
			instant, err := time.Parse(time.RFC3339, tt.instant)
			if err != nil {
				t.Fatal(err)
			}
			f := app.requestFacts()
			if err := f.load(app.DB, []models.Registration{row}, holder); err != nil {
				t.Fatal(err)
			}
			if !f.today.IsZero() || f.clock == nil {
				t.Fatal("facts eagerly chose UTC instead of a lazy church clock")
			}
			f.clock = func() time.Time { return instant.In(churchZoneOrUTC(app.DB)) }
			facts := householdFacts(f.people[person.ID], nil, f.day())
			if facts.AgeYears == nil || *facts.AgeYears != tt.age {
				t.Fatalf("age: %+v", facts)
			}
			if got := policy.HouseholdRegistrationProblem(facts) == ""; got != tt.eligible {
				t.Fatalf("birthday eligibility: %t", got)
			}
			attendee := f.attendee(row, models.EventOccurrence{Status: models.OccurrenceScheduled}, holder, false, false, instant)
			if attendee == nil || attendee.HouseholdPerson == nil || attendee.HouseholdPerson.Age == nil || *attendee.HouseholdPerson.Age != tt.age {
				t.Fatalf("display disagrees: %+v", attendee)
			}
		})
	}
	// Account-only facts never read the church time zone, including eligibility.
	f := app.requestFacts()
	if err := f.load(app.DB, []models.Registration{{UserID: &holder.ID}}, holder); err != nil {
		t.Fatal(err)
	}
	called := false
	f.clock = func() time.Time { called = true; return time.Now() }
	_ = f.attendee(models.Registration{UserID: &holder.ID}, models.EventOccurrence{}, holder, false, false, time.Now())
	if called {
		t.Fatal("account-only rendering read the church clock")
	}
}

func TestPendingTeenRegistrationNamesStayPrivate(t *testing.T) {
	teen := models.User{ID: 3, Name: "Taylor", Status: models.UserPendingConsent, AccountType: models.AccountTeen}
	member := models.User{ID: 4, Status: models.UserActive, AccountType: models.AccountAdult}
	f := newSubjectFacts(time.Now())
	f.users[teen.ID] = &teen
	f.accounts[teen.ID] = models.HouseholdAccount{HouseholdID: 9, UserID: teen.ID, Relation: models.HouseholdRelationTeen}
	row := models.Registration{ID: 5, UserID: &teen.ID, Status: models.RegGoing}
	if dto := f.attendee(row, models.EventOccurrence{}, &member, false, false, time.Now()); dto != nil {
		t.Fatal("unrelated member saw pending teen")
	}
	if dto := f.attendee(row, models.EventOccurrence{}, &member, true, false, time.Now()); dto == nil || dto.AccountType != models.AccountTeen {
		t.Fatal("manager lost pending teen visibility")
	}
	f.accounts[member.ID] = models.HouseholdAccount{HouseholdID: 9, UserID: member.ID, Relation: models.HouseholdRelationAdult}
	if dto := f.attendee(row, models.EventOccurrence{}, &member, false, false, time.Now()); dto == nil {
		t.Fatal("household lost visibility")
	}
}

// DEC-82: the teen badge is for event leaders and admins only.
func TestTeenAccountTypeOnlyForLeadersAndAdmins(t *testing.T) {
	teen := models.User{ID: 3, Name: "Taylor", Status: models.UserActive, AccountType: models.AccountTeen}
	member := models.User{ID: 4, Status: models.UserActive, Role: models.RoleMember, AccountType: models.AccountAdult}
	admin := models.User{ID: 5, Status: models.UserActive, Role: models.RoleAdmin, AccountType: models.AccountAdult}
	f := newSubjectFacts(time.Now())
	f.users[teen.ID] = &teen
	row := models.Registration{ID: 5, UserID: &teen.ID, Status: models.RegGoing}
	if dto := f.attendee(row, models.EventOccurrence{}, &member, false, false, time.Now()); dto != nil {
		t.Fatalf("ordinary attendee got the minor identity: %+v", dto)
	}
	if dto := f.attendee(row, models.EventOccurrence{}, &member, true, false, time.Now()); dto == nil || dto.AccountType != models.AccountTeen {
		t.Fatal("leader lost the badge")
	}
	if dto := f.attendee(row, models.EventOccurrence{}, &admin, false, false, time.Now()); dto == nil || dto.AccountType != models.AccountTeen {
		t.Fatal("admin lost the badge")
	}
}

// TestHouseholdReconcileSkipsADateCancelledWhileItWaited: removing a household
// person selects their scheduled dates, then locks each. A date cancelled and
// renamed while the removal, by another adult of the household, waited for
// its lock is left as a date cancelled before the selection would be: the row
// stays, and the adult who signed the person up, who reached the date only
// through that row, is told nothing, least of all the new title.
func TestHouseholdReconcileSkipsADateCancelledWhileItWaited(t *testing.T) {
	f := newDateEditFixture(t)
	target := f.dates[1]
	gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", target.ID).Update("visibility_override", models.VisibilityPublic))
	holder, _, spouse, _ := householdFamily(t, f.app, f.admin, "waiting household holder")
	partner := newTestUser(t, f.app, "household partner", "password1234", models.RoleMember)
	gormMust(t, f.app.DB.Create(&models.HouseholdAccount{HouseholdID: spouse.HouseholdID, UserID: partner.ID, Relation: models.HouseholdRelationAdult}))
	row := models.Registration{HouseholdPersonID: &spouse.ID, RegisteredBy: &holder.ID, OccurrenceID: target.ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}
	gormMust(t, f.app.DB.Create(&row))
	mark := markNotices(t, f.app)

	blocker := f.app.DB.Begin()
	gormMust(t, blocker)
	t.Cleanup(func() { blocker.Rollback() })
	if _, err := lockOccurrence(blocker, target.ID); err != nil {
		t.Fatal(err)
	}
	deleter := loginAPI(t, f.app, partner.Name, "password1234")
	removal := queueContact(func() *httptest.ResponseRecorder {
		return deleter.do("DELETE", "/api/v1/me/household/people/"+itoa(spouse.ID), nil, true)
	})
	waitForLockedUsersQuery(t, f.app, `%FROM "event_occurrences"%FOR UPDATE%`)
	gormMust(t, blocker.Model(&models.EventOccurrence{}).Where("id = ?", target.ID).Updates(map[string]any{
		"status": models.OccurrenceCancelled, "cancel_source": models.CancelSourceManual, "title_override": "RENAMEDSECRET"}))
	gormMust(t, blocker.Commit())
	if w := awaitContactResponse(t, removal); w.Code != http.StatusOK {
		t.Fatalf("the removal: %d %s", w.Code, w.Body.String())
	}

	var after models.Registration
	gormMust(t, f.app.DB.First(&after, row.ID))
	if after.Status != models.RegGoing {
		t.Fatalf("the row on the cancelled date was reconciled: %s", after.Status)
	}
	drainNotices(t, f.app)
	if got := noticesSince(t, f.app, holder.ID, notify.RegistrationRemoved, mark.notice); len(got) != 0 {
		t.Fatalf("the holder was told about a cancelled date: %+v", got)
	}
	noSecrets(t, f.app, "the household holder", holder.ID, holder.Email, mark, []string{"RENAMEDSECRET"})
}

// householdAcrossTwoDates is a household person going on dates A and B of a
// series (A first by id), with A full, and a departed member, w, holding live
// departure grants on both: going on B, and waitlisted on A behind the
// household person. w's row on B has the lower id, so an id-ordered lock of
// the event's grant rows takes it before w's row on A.
type householdAcrossTwoDates struct {
	f          dateEditFixture
	a, b       models.EventOccurrence
	spouse     householdPersonJSON
	deleter    *apiClient
	w          *models.User
	wOnA, wOnB models.Registration
}

func newHouseholdAcrossTwoDates(t *testing.T) householdAcrossTwoDates {
	t.Helper()
	f := newDateEditFixture(t)
	h := householdAcrossTwoDates{f: f, a: f.dates[0], b: f.dates[1]}
	if h.a.ID >= h.b.ID {
		t.Fatalf("fixture: date A %d is not before B %d by id", h.a.ID, h.b.ID)
	}
	gormMust(t, f.app.DB.Model(&models.Event{}).Where("id = ?", f.event.ID).Update("waitlist_enabled", true))
	gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", h.a.ID).Update("capacity_override", 1))
	holder, _, spouse, _ := householdFamily(t, f.app, f.admin, "two date holder")
	h.spouse = spouse
	partner := newTestUser(t, f.app, "two date partner", "password1234", models.RoleMember)
	gormMust(t, f.app.DB.Create(&models.HouseholdAccount{HouseholdID: spouse.HouseholdID, UserID: partner.ID, Relation: models.HouseholdRelationAdult}))
	h.deleter = loginAPI(t, f.app, partner.Name, "password1234")
	for _, occ := range []uint{h.a.ID, h.b.ID} {
		gormMust(t, f.app.DB.Create(&models.Registration{HouseholdPersonID: &spouse.ID, RegisteredBy: &holder.ID, OccurrenceID: occ, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
	}
	h.w = newTestUser(t, f.app, "departed waiter", "password1234", models.RoleMember)
	membership := models.Membership{UserID: h.w.ID, GroupID: f.group, Role: models.MembershipMember, Status: models.MembershipActive}
	gormMust(t, &gorm.DB{Error: f.app.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&membership).Error; err != nil {
			return err
		}
		at := time.Now().UTC().Add(-time.Minute)
		h.wOnB = models.Registration{UserID: &h.w.ID, OccurrenceID: h.b.ID, Status: models.RegGoing, Source: models.RegSourceAdmin, AttendanceMode: models.AttendanceInPerson}
		h.wOnA = models.Registration{UserID: &h.w.ID, OccurrenceID: h.a.ID, Status: models.RegWaitlisted, WaitlistedAt: &at, Source: models.RegSourceAdmin, AttendanceMode: models.AttendanceInPerson}
		for _, r := range []*models.Registration{&h.wOnB, &h.wOnA} {
			if err := tx.Create(r).Error; err != nil {
				return err
			}
			if _, err := seedAccessGrantIn(tx, r.ID, accessGrant{Origin: policy.OccurrenceOriginDepartureSelf, DepartureMembershipID: &membership.ID, DepartureActorID: &h.w.ID}, &h.w.ID, time.Now().UTC()); err != nil {
				return err
			}
		}
		return nil
	})})
	gormMust(t, &gorm.DB{Error: f.app.contactMutation(contactChange{UserID: h.w.ID}, func(tx *gorm.DB) error {
		return tx.Model(&models.Membership{}).Where("id = ?", membership.ID).Update("status", "removed").Error
	})})
	if h.wOnB.ID >= h.wOnA.ID {
		t.Fatalf("fixture: w's row on B (%d) is not before its row on A (%d)", h.wOnB.ID, h.wOnA.ID)
	}
	return h
}

func (h householdAcrossTwoDates) remove() <-chan *httptest.ResponseRecorder {
	return queueContact(func() *httptest.ResponseRecorder {
		return h.deleter.do("DELETE", "/api/v1/me/household/people/"+itoa(h.spouse.ID), nil, true)
	})
}

func (h householdAcrossTwoDates) editB(t *testing.T) <-chan *httptest.ResponseRecorder {
	t.Helper()
	date := models.FormatCalendarDate(h.b.OriginalDate)
	base := editBaseOf(t, h.f.admin, h.f.event.ID, date)
	return goPostDateEdit(h.f.admin, h.f.event.ID, date, dateEditBody(base.Revision, map[string]any{"title": "Edited B"}))
}

// both waits for the edit and the removal and checks each applied: w was
// promoted on A with its grant, and B carries the edit.
func (h householdAcrossTwoDates) both(t *testing.T, edit, removal <-chan *httptest.ResponseRecorder) {
	t.Helper()
	w1, w2 := awaitResponse(t, edit), awaitContactResponse(t, removal)
	if w1.Code != http.StatusOK || w2.Code != http.StatusOK {
		t.Fatalf("the edit %d %s; the removal %d %s", w1.Code, w1.Body.String(), w2.Code, w2.Body.String())
	}
	var onA models.Registration
	gormMust(t, h.f.app.DB.First(&onA, h.wOnA.ID))
	if onA.Status != models.RegGoing || onA.AccessEndedAt != nil {
		t.Fatalf("w on A: %s, grant ended %v", onA.Status, onA.AccessEndedAt)
	}
	if o := occurrenceRow(t, h.f.app, h.b.ID); o.TitleOverride == nil || *o.TitleOverride != "Edited B" {
		t.Fatalf("the edit of B did not apply: %+v", o)
	}
}

// TestHouseholdRemovalAndDateEditBeforePassDoNotDeadlock: a session holds w's
// live grant row on B, so the edit of B parks in its before pass holding B.
// The household removal locks A, cancels the person there, promotes w and so
// holds w's row on A, then waits for B behind the edit. Released, the edit
// asks only for B's grant rows and finishes; the removal then takes B. With an
// event-wide before pass the edit would ask for w's row on A next and the
// database would abort one side with 40P01.
func TestHouseholdRemovalAndDateEditBeforePassDoNotDeadlock(t *testing.T) {
	h := newHouseholdAcrossTwoDates(t)
	release := holdRows(t, h.f.app, holdRegistrationRow, h.wOnB.ID)
	edit := h.editB(t)
	editor := waitForLockedUsersQuery(t, h.f.app, `%FROM "registrations"%FOR UPDATE%`)
	removal := h.remove()
	waiterBlockedBy(t, h.f.app.DB, editor, "event_occurrences")
	release()
	h.both(t, edit, removal)
}

// TestHouseholdRemovalAndDateEditAfterPassDoNotDeadlock: the edit of B is
// parked at its "effects" step, past its before pass and holding B. The
// household removal promotes w on A, holding w's row there, and waits on B's
// occurrence lock behind the edit. Released, the edit's after pass locks only
// B's grant rows and both finish. This guards a partial fix that scopes the
// before pass but leaves the wrapper's after pass event-wide: that after pass
// would ask for w's row on A and close the cycle. (Against fix7's event-wide
// before pass this schedule cannot be set up: the edit already holds w's row
// on A, so the removal blocks on that row and never reaches B.)
func TestHouseholdRemovalAndDateEditAfterPassDoNotDeadlock(t *testing.T) {
	h := newHouseholdAcrossTwoDates(t)
	parked, resume := make(chan int, 1), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-resume:
		default:
			close(resume)
		}
	})
	h.f.app.hooks.dateEditStep = func(tx *gorm.DB, step string) error {
		if step != "effects" {
			return nil
		}
		var pid int
		if err := tx.Raw("SELECT pg_backend_pid()").Scan(&pid).Error; err != nil {
			return err
		}
		parked <- pid
		<-resume
		return nil
	}
	edit := h.editB(t)
	var editor int
	select {
	case editor = <-parked:
	case <-time.After(10 * time.Second):
		t.Fatal("the edit never reached its effects step")
	}
	removal := h.remove()
	waiterBlockedBy(t, h.f.app.DB, editor, "event_occurrences")
	close(resume)
	h.both(t, edit, removal)
}
