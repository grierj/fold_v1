package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"fold/internal/audit"
	"fold/internal/consent"
	"fold/internal/events"
	"fold/internal/guardkit"
	"fold/internal/notify"
	"fold/internal/policy"
	"fold/models"

	"gorm.io/gorm"
)

func dateEditPath(eventID uint, date string) string {
	return fmt.Sprintf("/api/v1/events/%d/occurrences/%s/edits", eventID, date)
}

func managedDatePath(eventID uint, date string) string {
	return fmt.Sprintf("/api/v1/events/%d/occurrences/%s", eventID, date)
}

func newDateAttempt(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// editBaseOf reads one date's edit base through the manager's HTTP read.
func editBaseOf(t *testing.T, c *apiClient, eventID uint, date string) occurrenceJSON {
	t.Helper()
	w := c.do("GET", managedDatePath(eventID, date), nil, false)
	if w.Code != http.StatusOK {
		t.Fatalf("edit base %s: %d %s", date, w.Code, w.Body.String())
	}
	var body occurrenceResponseJSON
	decodeJSON(t, w, &body)
	if body.Occurrence.Revision == "" {
		t.Fatalf("edit base %s has no revision: %s", date, w.Body.String())
	}
	return body.Occurrence
}

func dateEditBody(attempt, revision string, changes map[string]any, extra ...any) map[string]any {
	body := map[string]any{"attempt": attempt, "revision": revision, "changes": changes}
	for i := 0; i+1 < len(extra); i += 2 {
		body[extra[i].(string)] = extra[i+1]
	}
	return body
}

func postDateEdit(c *apiClient, eventID uint, date string, body map[string]any) *httptest.ResponseRecorder {
	return c.do("POST", dateEditPath(eventID, date), body, true)
}

// wantDateEdit posts body and checks the status, and the code for an error.
func wantDateEdit(t *testing.T, c *apiClient, eventID uint, date string, body map[string]any, status int, code string) *httptest.ResponseRecorder {
	t.Helper()
	w := postDateEdit(c, eventID, date, body)
	if w.Code != status {
		t.Fatalf("date edit %v: %d %s, want %d", body["changes"], w.Code, w.Body.String(), status)
	}
	if got := answerCode(t, w); code != "" && got != code {
		t.Fatalf("date edit %v: code %s, want %s (%s)", body["changes"], got, code, w.Body.String())
	}
	return w
}

// wantEditedDate checks that a 200 from the guarded edit acknowledges base's
// date: the same occurrence and a non-empty revision.
func wantEditedDate(t *testing.T, w *httptest.ResponseRecorder, base occurrenceJSON) occurrenceJSON {
	t.Helper()
	var body occurrenceResponseJSON
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("answer %q: %v", w.Body.String(), err)
	}
	if body.Occurrence.ID != base.ID || body.Occurrence.Revision == "" {
		t.Fatalf("the save of date %d (%s) answered %s", base.ID, base.Date, w.Body.String())
	}
	return body.Occurrence
}

// saveDate is an ordinary save of one date through the guarded edit, as the
// date editor sends it: the base from c's own HTTP edit-base read, a fresh
// attempt and only the changed fields (a nil value clears one), with extra as
// the body's own fields (a visibility change's reason). It wants status and,
// for a refusal, code; a 200 must acknowledge the date. It never answers a
// consent question: a save meant to cancel registrations is
// saveDateConsenting.
func saveDate(t *testing.T, c *apiClient, eventID uint, date string, changes map[string]any, status int, code string, extra ...any) *httptest.ResponseRecorder {
	t.Helper()
	base := editBaseOf(t, c, eventID, date)
	w := wantDateEdit(t, c, eventID, date, dateEditBody(newDateAttempt(t), base.Revision, changes, extra...), status, code)
	if status == http.StatusOK {
		wantEditedDate(t, w, base)
	}
	return w
}

// saveDateConsenting is a save meant to cancel registrations, as the editor
// asks it: the first answer must be 409 date_needs_consent naming someone,
// and the same attempt and revision sent again with that question's consent
// must save. It returns the question's impact for the caller's own checks.
func saveDateConsenting(t *testing.T, c *apiClient, eventID uint, date string, changes map[string]any, extra ...any) visibilityImpactJSON {
	t.Helper()
	base := editBaseOf(t, c, eventID, date)
	attempt := newDateAttempt(t)
	w := wantDateEdit(t, c, eventID, date, dateEditBody(attempt, base.Revision, changes, extra...), http.StatusConflict, "date_needs_consent")
	var asked dateNeedsConsentJSON
	if err := json.Unmarshal(w.Body.Bytes(), &asked); err != nil {
		t.Fatalf("answer %q: %v", w.Body.String(), err)
	}
	if len(asked.Impact.Tightened) == 0 || asked.Impact.Tightened[0].Registrants == 0 || !consent.WellFormed(asked.Impact.Consent) {
		t.Fatalf("the consent question names nobody: %s", w.Body.String())
	}
	w = wantDateEdit(t, c, eventID, date, dateEditBody(attempt, base.Revision, changes, append(slices.Clone(extra), "consent", asked.Impact.Consent)...), http.StatusOK, "")
	wantEditedDate(t, w, base)
	return asked.Impact
}

// answerCode is the answer's code, read without consuming the body.
func answerCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var out struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("answer %q: %v", w.Body.String(), err)
	}
	return out.Code
}

func answeredRevision(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body occurrenceResponseJSON
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("answer %q: %v", w.Body.String(), err)
	}
	if body.Occurrence.Revision == "" {
		t.Fatalf("no revision in %s", w.Body.String())
	}
	return body.Occurrence.Revision
}

// mutableClock is an App clock a test moves while a request is in flight.
type mutableClock struct{ at atomic.Int64 }

func newMutableClock(app *App, at time.Time) *mutableClock {
	c := &mutableClock{}
	c.set(at)
	app.clock = func() time.Time { return time.Unix(0, c.at.Load()).UTC() }
	return c
}

func (c *mutableClock) set(at time.Time) { c.at.Store(at.UnixNano()) }

// holdEventRow keeps the event row locked FOR UPDATE in another session until
// the returned release runs.
func holdEventRow(t *testing.T, app *App, eventID uint) func() {
	t.Helper()
	tx := app.DB.Begin()
	gormMust(t, tx)
	gormMust(t, tx.Exec("SELECT id FROM events WHERE id = ? FOR UPDATE", eventID))
	done := false
	release := func() {
		if !done {
			done = true
			tx.Rollback()
		}
	}
	t.Cleanup(release)
	return release
}

func goPostDateEdit(c *apiClient, eventID uint, date string, body map[string]any) <-chan *httptest.ResponseRecorder {
	out := make(chan *httptest.ResponseRecorder, 1)
	go func() { out <- postDateEdit(c, eventID, date, body) }()
	return out
}

func awaitResponse(t *testing.T, ch <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case w := <-ch:
		return w
	case <-time.After(30 * time.Second):
		t.Fatal("date edit did not answer")
		return nil
	}
}

// dateEditFixture is an invite-only group with a place, a weekly series with
// no place of its own and four dates, its admin, a leader, a member and two
// outsiders (signed-in people who are not members).
type dateEditFixture struct {
	app                   *App
	adminUser, leaderUser *models.User
	admin, leader, member *apiClient
	outsiders             []*models.User
	memberUser            *models.User
	group                 uint
	event                 eventJSON
	dates                 []models.EventOccurrence
}

func newDateEditFixture(t *testing.T) dateEditFixture {
	t.Helper()
	app := testApp(t)
	f := dateEditFixture{app: app}
	f.adminUser = newTestUser(t, app, "edit admin", "password1234", models.RoleAdmin)
	f.admin = loginAPI(t, app, f.adminUser.Name, "password1234")
	f.group = createFeedGroup(t, f.admin, "Supper club", models.VisibilityInviteOnly, "Group hall")
	start := feedStart(7)
	f.event = createFeedEvent(t, f.admin, f.group, "Supper", start, "FREQ=WEEKLY;COUNT=4")
	gormMust(t, app.DB.Where("event_id = ?", f.event.ID).Order("starts_at").Find(&f.dates))
	if len(f.dates) != 4 {
		t.Fatalf("fixture: %d dates", len(f.dates))
	}
	f.leaderUser = newTestUser(t, app, "edit leader", "password1234", models.RoleMember)
	gormMust(t, app.DB.Create(&models.Membership{UserID: f.leaderUser.ID, GroupID: f.group, Role: models.MembershipLeader, Status: models.MembershipActive}))
	f.leader = loginAPI(t, app, f.leaderUser.Name, "password1234")
	f.memberUser = newTestUser(t, app, "edit member", "password1234", models.RoleMember)
	gormMust(t, app.DB.Create(&models.Membership{UserID: f.memberUser.ID, GroupID: f.group, Role: models.MembershipMember, Status: models.MembershipActive}))
	f.member = loginAPI(t, app, f.memberUser.Name, "password1234")
	for _, name := range []string{"outsider one", "outsider two"} {
		f.outsiders = append(f.outsiders, newTestUser(t, app, name, "password1234", models.RoleMember))
	}
	return f
}

func (f dateEditFixture) date(i int) string {
	return models.FormatCalendarDate(f.dates[i].OriginalDate)
}

// openWithOutsiders opens date i to the public and signs both outsiders up
// for it, so tightening it back to the series cancels exactly them.
func (f dateEditFixture) openWithOutsiders(t *testing.T, i int) []models.Registration {
	t.Helper()
	gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", f.dates[i].ID).Update("visibility_override", models.VisibilityPublic))
	var regs []models.Registration
	for _, u := range f.outsiders {
		r := models.Registration{UserID: &u.ID, OccurrenceID: f.dates[i].ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}
		gormMust(t, f.app.DB.Create(&r))
		regs = append(regs, r)
	}
	return regs
}

func (f dateEditFixture) state(t *testing.T) string { return eventWriterState(t, f.app, f.event.ID) }

func occurrenceRow(t *testing.T, app *App, id uint) models.EventOccurrence {
	t.Helper()
	var o models.EventOccurrence
	gormMust(t, app.DB.First(&o, id))
	return o
}

// TestDateEditAppliesAndAnswersRevision: a manager's token from the event page
// applies, and the answer's token is the one every manager read shows next.
func TestDateEditAppliesAndAnswersRevision(t *testing.T) {
	f := newDateEditFixture(t)
	page, _ := getEvent(t, f.admin, f.event.ID, http.StatusOK)
	base := occurrenceOn(t, page.Occurrences, f.date(0))
	if base.Revision == "" {
		t.Fatal("the manager's event page has no revision")
	}
	w := wantDateEdit(t, f.admin, f.event.ID, f.date(0), dateEditBody(newDateAttempt(t), base.Revision, map[string]any{"title": "  Harvest supper "}), http.StatusOK, "")
	got := answeredRevision(t, w)
	if got == base.Revision {
		t.Fatal("an applied edit answered its base revision")
	}
	var body occurrenceResponseJSON
	gormMust(t, &gorm.DB{Error: json.Unmarshal(w.Body.Bytes(), &body)})
	if body.Occurrence.Title != "Harvest supper" || body.Occurrence.Location != "Group hall" {
		t.Fatalf("answer = %+v", body.Occurrence)
	}
	page, _ = getEvent(t, f.admin, f.event.ID, http.StatusOK)
	if next := occurrenceOn(t, page.Occurrences, f.date(0)).Revision; next != got {
		t.Fatalf("next GET revision %q, the answer said %q", next, got)
	}
	if b := editBaseOf(t, f.admin, f.event.ID, f.date(0)); b.Revision != got || b.Title != "Harvest supper" || b.Cancelled {
		t.Fatalf("edit base = %+v", b)
	}
	if o := occurrenceRow(t, f.app, f.dates[0].ID); o.TitleOverride == nil || *o.TitleOverride != "Harvest supper" || len(o.EditReceipt) != 32 {
		t.Fatalf("stored = %+v", o)
	}
	// Nobody else sees a token: not a member, not a signed-out visitor.
	_, raw := getEvent(t, f.member, f.event.ID, http.StatusOK)
	if strings.Contains(raw, `"revision"`) {
		t.Fatalf("a member's event page carries a revision: %s", raw)
	}
	apiDo(t, f.member, "GET", managedDatePath(f.event.ID, f.date(0)), nil, http.StatusForbidden)
}

// TestManagedDateAnswersAMovedDateByItsOriginalDay: the SPA's date editors open from a listed date's event,
// original day and occurrence id (web/src/lib/editBase.ts editBaseOf), and on the group page also its group. A
// date moved to another day is still read by its original day, as the same row, in its group, with a token.
func TestManagedDateAnswersAMovedDateByItsOriginalDay(t *testing.T) {
	f := newDateEditFixture(t)
	base := editBaseOf(t, f.leader, f.event.ID, f.date(0))
	moved := f.dates[0].StartsAt.AddDate(0, 0, 2).Format("2006-01-02T15:04")
	w := wantDateEdit(t, f.leader, f.event.ID, f.date(0), dateEditBody(newDateAttempt(t), base.Revision, map[string]any{"startsLocal": moved}), http.StatusOK, "")
	after := editBaseOf(t, f.leader, f.event.ID, f.date(0))
	if after.ID != f.dates[0].ID || after.EventID != f.event.ID || after.Date != f.date(0) || after.Revision != answeredRevision(t, w) {
		t.Fatalf("moved date read by its original day = %+v", after)
	}
	if after.StartsLocal != moved || after.Cancelled {
		t.Fatalf("moved date's own clock = %q cancelled=%v, want %q", after.StartsLocal, after.Cancelled, moved)
	}
	if after.GroupID == nil || *after.GroupID != f.group {
		t.Fatalf("moved date's group = %v, want %d", after.GroupID, f.group)
	}
	// The day it moved to names no date of the event.
	apiDo(t, f.leader, "GET", managedDatePath(f.event.ID, f.dates[0].StartsAt.AddDate(0, 0, 2).Format("2006-01-02")), nil, http.StatusNotFound)
	// Every manager the group page opens an editor for gets the group: an admin who is not a member, and a
	// pending group's leader.
	gormMust(t, f.app.DB.Where("group_id = ? AND user_id = ?", f.group, f.adminUser.ID).Delete(&models.Membership{}))
	gormMust(t, f.app.DB.Model(&models.Group{}).Where("id = ?", f.group).Update("status", models.GroupPending))
	for name, c := range map[string]*apiClient{"admin": f.admin, "pending group's leader": f.leader} {
		b := editBaseOf(t, c, f.event.ID, f.date(1))
		if b.ID != f.dates[1].ID || b.GroupID == nil || *b.GroupID != f.group {
			t.Fatalf("%s's edit base = id %d group %v, want id %d group %d", name, b.ID, b.GroupID, f.dates[1].ID, f.group)
		}
	}
}

// TestDateEditRefusesStaleRevision: any write after the token was read, by
// any writer (another manager's date edit, a raw UPDATE as a previous binary
// would make, a series edit), refuses the edit with date_changed and writes
// nothing.
func TestDateEditRefusesStaleRevision(t *testing.T) {
	f := newDateEditFixture(t)
	for name, write := range map[string]func(){
		"another date edit": func() {
			saveDate(t, f.leader, f.event.ID, f.date(0), map[string]any{"title": "Another title"}, http.StatusOK, "")
		},
		"previous binary": func() {
			gormMust(t, f.app.DB.Exec("UPDATE event_occurrences SET duration_override = 75 WHERE id = ?", f.dates[0].ID))
		},
		"series edit": func() {
			apiDo(t, f.admin, "PUT", fmt.Sprintf("/api/v1/events/%d", f.event.ID), seriesBody(t, f.admin, fmt.Sprintf("/api/v1/events/%d", f.event.ID), map[string]any{"title": "Supper renamed"}), http.StatusOK)
		},
	} {
		t.Run(name, func(t *testing.T) {
			base := editBaseOf(t, f.admin, f.event.ID, f.date(0))
			write()
			before := f.state(t)
			wantDateEdit(t, f.admin, f.event.ID, f.date(0), dateEditBody(newDateAttempt(t), base.Revision, map[string]any{"location": "Kitchen"}), http.StatusConflict, "date_changed")
			if after := f.state(t); after != before {
				t.Fatalf("a stale edit wrote:\n%s\n%s", before, after)
			}
		})
	}
}

// TestDateEditDelayedDeliveryABA: a request held in delivery while another
// manager moves the date and moves it back is refused, so its registrants
// hear about the two real changes only.
func TestDateEditDelayedDeliveryABA(t *testing.T) {
	f := newDateEditFixture(t)
	gormMust(t, f.app.DB.Create(&models.Registration{UserID: &f.memberUser.ID, OccurrenceID: f.dates[0].ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
	base := editBaseOf(t, f.admin, f.event.ID, f.date(0))
	held := f.leader.doHeld("POST", dateEditPath(f.event.ID, f.date(0)), dateEditBody(newDateAttempt(t), base.Revision, map[string]any{"startsLocal": f.dates[0].StartsAt.Add(time.Hour).Format("2006-01-02T15:04")}))
	for _, start := range []time.Time{f.dates[0].StartsAt.Add(2 * time.Hour), f.dates[0].StartsAt} {
		b := editBaseOf(t, f.admin, f.event.ID, f.date(0))
		wantDateEdit(t, f.admin, f.event.ID, f.date(0), dateEditBody(newDateAttempt(t), b.Revision, map[string]any{"startsLocal": start.Format("2006-01-02T15:04")}), http.StatusOK, "")
	}
	before := f.state(t)
	held.release()
	if w := held.wait(t); w.Code != http.StatusConflict || answerCode(t, w) != "date_changed" {
		t.Fatalf("the held edit: %d %s", w.Code, w.Body.String())
	}
	if after := f.state(t); after != before {
		t.Fatal("the held edit wrote")
	}
	if n := countOf(t, f.app, f.memberUser.ID, notify.EventChanged); n != 2 {
		t.Fatalf("the registrant got %d change notices, want 2", n)
	}
}

// TestDateEditReplayIsReadOnly: the same attempt again answers 200 and
// changes nothing at all, including the reconcile work the wrapper would
// otherwise commit: the edited date carries an exact-date grant, and the
// clock is past that date's stored end when the replay runs.
func TestDateEditReplayIsReadOnly(t *testing.T) {
	f := newDepartureReadFixture(t, models.VisibilityInviteOnly, models.RegGoing, policy.OccurrenceOriginDepartureSelf)
	edited := f.dates[1] // the retained grant's own date
	target := models.FormatCalendarDate(edited.OriginalDate)
	clock := newMutableClock(f.app, time.Now().UTC())
	cases := map[string]func(t *testing.T) (map[string]any, []any){
		"time and title": func(t *testing.T) (map[string]any, []any) {
			// A kept date can move earlier while its grant is live (DEC-156).
			return map[string]any{"title": "Moved supper", "startsLocal": wallAt(edited.StartsAt.Add(-90 * time.Minute))}, nil
		},
		"tightening with consent": func(t *testing.T) (map[string]any, []any) {
			gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", edited.ID).Update("visibility_override", models.VisibilityPublic))
			gormMust(t, f.app.DB.Create(&models.Registration{UserID: &f.other.ID, OccurrenceID: edited.ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
			return map[string]any{"visibility": "series"}, []any{"consent", dateEditConsent(t, f.admin, f.event.ID, target, map[string]any{"visibility": "series"}), "reason", "Back to members"}
		},
		"capacity raise with a waitlist": func(t *testing.T) (map[string]any, []any) {
			gormMust(t, f.app.DB.Model(&models.Event{}).Where("id = ?", f.event.ID).Update("waitlist_enabled", true))
			gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", edited.ID).Updates(map[string]any{"capacity_override": 1, "visibility_override": nil}))
			at := time.Now().UTC().Add(-time.Minute)
			for i, u := range []*models.User{f.issuer, f.other} {
				r := models.Registration{UserID: &u.ID, OccurrenceID: edited.ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}
				if i == 1 {
					r.Status, r.WaitlistedAt = models.RegWaitlisted, &at
				}
				gormMust(t, f.app.DB.Where("user_id = ? AND occurrence_id = ?", u.ID, edited.ID).Delete(&models.Registration{}))
				gormMust(t, f.app.DB.Create(&r))
			}
			return map[string]any{"capacityOverride": 3}, nil
		},
	}
	for _, name := range []string{"time and title", "tightening with consent", "capacity raise with a waitlist"} {
		t.Run(name, func(t *testing.T) {
			clock.set(time.Now().UTC())
			changes, extra := cases[name](t)
			base := editBaseOf(t, f.admin, f.event.ID, target)
			body := dateEditBody(newDateAttempt(t), base.Revision, changes, extra...)
			first := answeredRevision(t, wantDateEdit(t, f.admin, f.event.ID, target, body, http.StatusOK, ""))
			// The edited date, as the edit stored it, ends; a commit of the
			// replay's own reconcile would now end the grant.
			clock.set(occurrenceRow(t, f.app, edited.ID).EndsAt.Add(time.Minute))
			if savedOccurrenceAccess(t, f).Status != policy.OccurrenceAccessActive {
				t.Fatal("fixture: the grant ended early")
			}
			state := eventWriterState(t, f.app, f.event.ID)
			if second := answeredRevision(t, wantDateEdit(t, f.admin, f.event.ID, target, body, http.StatusOK, "")); second != first {
				t.Fatalf("the replay answered %q, the edit %q", second, first)
			}
			if after := eventWriterState(t, f.app, f.event.ID); after != state {
				t.Fatalf("the replay wrote:\n%s\n%s", state, after)
			}
			if savedOccurrenceAccess(t, f).Status != policy.OccurrenceAccessActive {
				t.Fatal("the replay committed the wrapper's reconcile")
			}
		})
	}
}

// TestDateEditConcurrentIdenticalPosts: two identical requests, the second
// waiting on the contact gate behind the first, apply once.
func TestDateEditConcurrentIdenticalPosts(t *testing.T) {
	f := newDateEditFixture(t)
	gormMust(t, f.app.DB.Create(&models.Registration{UserID: &f.memberUser.ID, OccurrenceID: f.dates[0].ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
	base := editBaseOf(t, f.admin, f.event.ID, f.date(0))
	body := dateEditBody(newDateAttempt(t), base.Revision, map[string]any{"location": "Kitchen"})
	// A second client of the same person: the receipt binds the person, and
	// the two requests share no cookie jar.
	second := loginAPI(t, f.app, f.adminUser.Name, "password1234")
	release := holdEventRow(t, f.app, f.event.ID)
	one := goPostDateEdit(f.admin, f.event.ID, f.date(0), body)
	waitForLockedUsersQuery(t, f.app, `%FROM "events"%FOR UPDATE%`)
	two := goPostDateEdit(second, f.event.ID, f.date(0), body)
	waitForLockedUsersQuery(t, f.app, "%pg_advisory_xact_lock%")
	release()
	w1, w2 := awaitResponse(t, one), awaitResponse(t, two)
	if w1.Code != http.StatusOK || w2.Code != http.StatusOK || answeredRevision(t, w1) != answeredRevision(t, w2) {
		t.Fatalf("identical edits: %d %s / %d %s", w1.Code, w1.Body.String(), w2.Code, w2.Body.String())
	}
	if n := countOf(t, f.app, f.memberUser.ID, notify.EventChanged); n != 1 {
		t.Fatalf("%d change notices, want 1", n)
	}
}

// TestDateEditReceiptBindsIntent: once an attempt applied, its id with any
// other intent, or the same body from another manager, is a stale edit, and
// the stored receipt stays the one the attempt wrote.
func TestDateEditReceiptBindsIntent(t *testing.T) {
	f := newDateEditFixture(t)
	gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", f.dates[1].ID).Update("visibility_override", models.VisibilityPublic))
	base := editBaseOf(t, f.admin, f.event.ID, f.date(1))
	attempt := newDateAttempt(t)
	// Nobody is cancelled, so the consent is not checked; the receipt still
	// binds it.
	given, other := strings.Repeat("a", 32), strings.Repeat("b", 32)
	body := dateEditBody(attempt, base.Revision, map[string]any{"visibility": "private", "title": "Open supper"}, "reason", "Members and friends", "consent", given)
	wantDateEdit(t, f.admin, f.event.ID, f.date(1), body, http.StatusOK, "")
	receipt := occurrenceRow(t, f.app, f.dates[1].ID).EditReceipt
	state := f.state(t)
	for name, other := range map[string]map[string]any{
		"other changes": dateEditBody(attempt, base.Revision, map[string]any{"visibility": "private", "title": "Closed supper"}, "reason", "Members and friends", "consent", given),
		"other reason":  dateEditBody(attempt, base.Revision, map[string]any{"visibility": "private", "title": "Open supper"}, "reason", "Someone else's words", "consent", given),
		"other consent": dateEditBody(attempt, base.Revision, map[string]any{"visibility": "private", "title": "Open supper"}, "reason", "Members and friends", "consent", other),
		"no consent":    dateEditBody(attempt, base.Revision, map[string]any{"visibility": "private", "title": "Open supper"}, "reason", "Members and friends"),
	} {
		t.Run(name, func(t *testing.T) {
			wantDateEdit(t, f.admin, f.event.ID, f.date(1), other, http.StatusConflict, "date_changed")
		})
	}
	t.Run("another manager", func(t *testing.T) {
		wantDateEdit(t, f.leader, f.event.ID, f.date(1), body, http.StatusConflict, "date_changed")
	})
	if after := f.state(t); after != state {
		t.Fatal("a refused replay wrote")
	}
	wantDateEdit(t, f.admin, f.event.ID, f.date(1), body, http.StatusOK, "")
	if got := occurrenceRow(t, f.app, f.dates[1].ID).EditReceipt; !bytes.Equal(got, receipt) {
		t.Fatal("a replay changed the receipt")
	}
}

// TestDateEditReplayAfterSeriesEditConflicts: the receipt binds the token the
// edit produced, so a later series-only change ends the replay.
func TestDateEditReplayAfterSeriesEditConflicts(t *testing.T) {
	f := newDateEditFixture(t)
	base := editBaseOf(t, f.admin, f.event.ID, f.date(0))
	body := dateEditBody(newDateAttempt(t), base.Revision, map[string]any{"title": "Moved"})
	wantDateEdit(t, f.admin, f.event.ID, f.date(0), body, http.StatusOK, "")
	apiDo(t, f.admin, "PUT", fmt.Sprintf("/api/v1/events/%d", f.event.ID), seriesBody(t, f.admin, fmt.Sprintf("/api/v1/events/%d", f.event.ID), map[string]any{"description": "Bring a dish"}), http.StatusOK)
	wantDateEdit(t, f.admin, f.event.ID, f.date(0), body, http.StatusConflict, "date_changed")
}

// TestDateEditNoOpWritesNothing: changes equal to the stored values answer 200
// with no write and leave the base valid. An omitted visibility never clears
// an explicit override, and an equal one is not a change.
func TestDateEditNoOpWritesNothing(t *testing.T) {
	f := newDateEditFixture(t)
	gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", f.dates[1].ID).Updates(map[string]any{"visibility_override": models.VisibilityPrivate, "capacity_override": 8}))
	base := editBaseOf(t, f.admin, f.event.ID, f.date(1))
	state := f.state(t)
	for name, changes := range map[string]map[string]any{
		"equal capacity":   {"capacityOverride": 8},
		"equal visibility": {"visibility": "private"},
		"equal both":       {"capacityOverride": 8, "visibility": "private"},
	} {
		t.Run(name, func(t *testing.T) {
			w := wantDateEdit(t, f.admin, f.event.ID, f.date(1), dateEditBody(newDateAttempt(t), base.Revision, changes), http.StatusOK, "")
			if got := answeredRevision(t, w); got != base.Revision {
				t.Fatalf("a no-op answered %q, base %q", got, base.Revision)
			}
			if after := f.state(t); after != state {
				t.Fatalf("a no-op wrote:\n%s\n%s", state, after)
			}
		})
	}
	// A capacity-only change keeps the explicit private override, and its
	// replay is read-only too.
	body := dateEditBody(newDateAttempt(t), base.Revision, map[string]any{"capacityOverride": 5})
	wantDateEdit(t, f.admin, f.event.ID, f.date(1), body, http.StatusOK, "")
	o := occurrenceRow(t, f.app, f.dates[1].ID)
	if o.VisibilityOverride == nil || *o.VisibilityOverride != models.VisibilityPrivate || o.CapacityOverride == nil || *o.CapacityOverride != 5 {
		t.Fatalf("after a capacity-only edit: %+v", o)
	}
	state = f.state(t)
	wantDateEdit(t, f.admin, f.event.ID, f.date(1), body, http.StatusOK, "")
	if after := f.state(t); after != state {
		t.Fatal("the capacity replay wrote")
	}
	if n := auditCount(t, f.app, "event.visibility_changed") + auditCount(t, f.app, "event.visibility_override"); n != 0 {
		t.Fatalf("%d visibility audit rows from edits that did not change it", n)
	}
}

// TestDateEditAuthorization: the same 404 and 403 as the date edit always had,
// checked before any receipt, and the locked recheck answers 404 when access
// is lost or the event is deleted while the body arrives. None of them write.
func TestDateEditAuthorization(t *testing.T) {
	f := newDateEditFixture(t)
	stranger := loginAPI(t, f.app, f.outsiders[0].Name, "password1234")
	base := editBaseOf(t, f.admin, f.event.ID, f.date(0))
	state := f.state(t)
	wantDateEdit(t, stranger, f.event.ID, f.date(0), dateEditBody(newDateAttempt(t), base.Revision, map[string]any{"title": "x"}), http.StatusNotFound, "")
	wantDateEdit(t, f.admin, 999999, f.date(0), dateEditBody(newDateAttempt(t), base.Revision, map[string]any{"title": "x"}), http.StatusNotFound, "")
	wantDateEdit(t, f.member, f.event.ID, f.date(0), dateEditBody(newDateAttempt(t), base.Revision, map[string]any{"title": "x"}), http.StatusForbidden, "")
	wantDateEdit(t, f.admin, f.event.ID, "2001-01-01", dateEditBody(newDateAttempt(t), base.Revision, map[string]any{"title": "x"}), http.StatusNotFound, "")
	apiDo(t, stranger, "GET", managedDatePath(f.event.ID, f.date(0)), nil, http.StatusNotFound)
	apiDo(t, f.admin, "GET", managedDatePath(f.event.ID, "2001-01-01"), nil, http.StatusNotFound)
	if after := f.state(t); after != state {
		t.Fatal("a refused edit wrote")
	}

	// A demoted leader who is still a member gets 403 before any receipt,
	// even for an attempt they applied.
	leaderBase := editBaseOf(t, f.leader, f.event.ID, f.date(0))
	applied := dateEditBody(newDateAttempt(t), leaderBase.Revision, map[string]any{"title": "Leader's title"})
	wantDateEdit(t, f.leader, f.event.ID, f.date(0), applied, http.StatusOK, "")
	roles := "/api/v1/groups/" + itoa(f.group) + "/members/" + itoa(f.leaderUser.ID) + "/role"
	apiDo(t, f.admin, "PUT", roles, map[string]any{"role": models.MembershipMember}, http.StatusOK)
	state = f.state(t)
	wantDateEdit(t, f.leader, f.event.ID, f.date(0), applied, http.StatusForbidden, "")
	if after := f.state(t); after != state {
		t.Fatal("a demoted leader's replay wrote")
	}

	// Demoted after the preflight, while the body arrives: 404 at the lock.
	apiDo(t, f.admin, "PUT", roles, map[string]any{"role": models.MembershipLeader}, http.StatusOK)
	fresh := editBaseOf(t, f.leader, f.event.ID, f.date(0))
	held := f.leader.doHeld("POST", dateEditPath(f.event.ID, f.date(0)), dateEditBody(newDateAttempt(t), fresh.Revision, map[string]any{"title": "Too late"}))
	apiDo(t, f.admin, "PUT", roles, map[string]any{"role": models.MembershipMember}, http.StatusOK)
	state = f.state(t)
	held.release()
	if w := held.wait(t); w.Code != http.StatusNotFound {
		t.Fatalf("demoted while the body arrived: %d %s", w.Code, w.Body.String())
	}
	if after := f.state(t); after != state {
		t.Fatal("the held edit wrote after the demotion")
	}

	// Deleted after the preflight: the existing 404.
	fresh = editBaseOf(t, f.admin, f.event.ID, f.date(0))
	held = f.admin.doHeld("POST", dateEditPath(f.event.ID, f.date(0)), dateEditBody(newDateAttempt(t), fresh.Revision, map[string]any{"title": "Gone"}))
	apiDo(t, f.admin, "DELETE", fmt.Sprintf("/api/v1/events/%d", f.event.ID), nil, http.StatusOK)
	held.release()
	if w := held.wait(t); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "event not found") {
		t.Fatalf("deleted while the body arrived: %d %s", w.Code, w.Body.String())
	}
}

// TestDateEditConsentMatchesLockedSelection: the consent must be the token of
// the locked selection's impact, and exactly those rows go.
func TestDateEditConsentMatchesLockedSelection(t *testing.T) {
	f := newDateEditFixture(t)
	regs := f.openWithOutsiders(t, 1)
	base := editBaseOf(t, f.admin, f.event.ID, f.date(1))
	state := f.state(t)
	// Absent, a well-formed token of nothing, and the token of another change.
	stale := dateEditConsent(t, f.admin, f.event.ID, f.date(1), map[string]any{"visibility": "private"})
	for _, extra := range [][]any{nil, {"consent", strings.Repeat("0", 32)}, {"consent", stale}} {
		w := wantDateEdit(t, f.admin, f.event.ID, f.date(1), dateEditBody(newDateAttempt(t), base.Revision, map[string]any{"visibility": "series"}, extra...), http.StatusConflict, "date_needs_consent")
		var body struct {
			Impact visibilityImpactJSON `json:"impact"`
		}
		gormMust(t, &gorm.DB{Error: json.Unmarshal(w.Body.Bytes(), &body)})
		if len(body.Impact.Tightened) != 1 || body.Impact.Tightened[0].Registrants != 2 || body.Impact.Visibility != models.VisibilityInviteOnly || !consent.WellFormed(body.Impact.Consent) {
			t.Fatalf("impact with %v = %+v", extra, body.Impact)
		}
	}
	if after := f.state(t); after != state {
		t.Fatal("a consent refusal wrote")
	}
	wantDateEdit(t, f.admin, f.event.ID, f.date(1), dateEditBody(newDateAttempt(t), base.Revision, map[string]any{"visibility": "series"}, "consent", dateEditConsent(t, f.admin, f.event.ID, f.date(1), map[string]any{"visibility": "series"})), http.StatusOK, "")
	for _, r := range regs {
		if s := regStatus(t, f.app, *r.UserID, r.OccurrenceID); s != models.RegCancelled {
			t.Fatalf("an agreed loser is %s", s)
		}
	}
	if n := auditCount(t, f.app, "registration.cancelled"); n != 2 {
		t.Fatalf("%d cancellation audit rows, want 2", n)
	}
	// Nobody to cancel: no consent needed.
	gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", f.dates[2].ID).Update("visibility_override", models.VisibilityPublic))
	b2 := editBaseOf(t, f.admin, f.event.ID, f.date(2))
	wantDateEdit(t, f.admin, f.event.ID, f.date(2), dateEditBody(newDateAttempt(t), b2.Revision, map[string]any{"visibility": nil}), http.StatusOK, "")
	// A consent given for people who are no longer there cancels nobody, so
	// nothing needs asking again.
	gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", f.dates[3].ID).Update("visibility_override", models.VisibilityPublic))
	b3 := editBaseOf(t, f.admin, f.event.ID, f.date(3))
	wantDateEdit(t, f.admin, f.event.ID, f.date(3), dateEditBody(newDateAttempt(t), b3.Revision, map[string]any{"visibility": "series"}, "consent", dateEditConsent(t, f.admin, f.event.ID, f.date(3), map[string]any{"visibility": "series"})), http.StatusOK, "")
}

// TestDateEditConsentUsesTheMovedStart: consent counts the date as it will be,
// so moving a started date into the future and tightening it asks first.
func TestDateEditConsentUsesTheMovedStart(t *testing.T) {
	f := newDateEditFixture(t)
	f.openWithOutsiders(t, 1)
	past := time.Now().UTC().Add(-2 * time.Hour)
	gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", f.dates[1].ID).Updates(map[string]any{"starts_at": past, "ends_at": past.Add(time.Hour)}))
	base := editBaseOf(t, f.admin, f.event.ID, f.date(1))
	future := time.Now().UTC().AddDate(0, 0, 3).Format("2006-01-02T15:04")
	w := wantDateEdit(t, f.admin, f.event.ID, f.date(1), dateEditBody(newDateAttempt(t), base.Revision, map[string]any{"startsLocal": future, "visibility": "series"}), http.StatusConflict, "date_needs_consent")
	if !strings.Contains(w.Body.String(), `"registrants":2`) {
		t.Fatalf("consent for a moved date: %s", w.Body.String())
	}
}

// TestDateEditOneClockAtTheBoundary: the clock moving past the date's start
// between the count and the cancellation changes nothing: the agreed rows go
// and no others.
func TestDateEditOneClockAtTheBoundary(t *testing.T) {
	f := newDateEditFixture(t)
	regs := f.openWithOutsiders(t, 1)
	clock := newMutableClock(f.app, time.Now().UTC())
	f.app.hooks.dateEditStep = func(tx *gorm.DB, step string) error {
		if step == "selected" {
			clock.set(f.dates[1].StartsAt.Add(time.Minute))
		}
		return nil
	}
	base := editBaseOf(t, f.admin, f.event.ID, f.date(1))
	wantDateEdit(t, f.admin, f.event.ID, f.date(1), dateEditBody(newDateAttempt(t), base.Revision, map[string]any{"visibility": "series"}, "consent", dateEditConsent(t, f.admin, f.event.ID, f.date(1), map[string]any{"visibility": "series"})), http.StatusOK, "")
	for _, r := range regs {
		if s := regStatus(t, f.app, *r.UserID, r.OccurrenceID); s != models.RegCancelled {
			t.Fatalf("an agreed loser is %s", s)
		}
	}
	if n := auditCount(t, f.app, "registration.cancelled"); n != 2 {
		t.Fatalf("%d cancellations, want 2", n)
	}
}

// TestDateEditRenameAndTightenNamesTheKnownTitle: renaming a date and taking
// it back to its invite-only series in one save cancels the outsider under the
// title they knew; neither their notice nor their guardians' copies (DEC-149)
// carry the new title, which only members may now see.
func TestDateEditRenameAndTightenNamesTheKnownTitle(t *testing.T) {
	renameAndTightenNamesTheKnownTitle(t, func(f dateEditFixture) {
		base := editBaseOf(t, f.admin, f.event.ID, f.date(1))
		wantDateEdit(t, f.admin, f.event.ID, f.date(1), dateEditBody(newDateAttempt(t), base.Revision,
			map[string]any{"title": "NEWPRIVATETITLE", "visibility": "series"}, "consent", dateEditConsent(t, f.admin, f.event.ID, f.date(1), map[string]any{"title": "NEWPRIVATETITLE", "visibility": "series"})), http.StatusOK, "")
	})
}

// renameAndTightenNamesTheKnownTitle runs save, which renames date 1 to
// NEWPRIVATETITLE and takes it back to its invite-only series in one save
// that agrees to cancel one person, and checks the outsider was told the
// title they knew.
func renameAndTightenNamesTheKnownTitle(t *testing.T, save func(f dateEditFixture)) {
	t.Helper()
	f := newDateEditFixture(t)
	gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", f.dates[1].ID).Updates(map[string]any{"visibility_override": models.VisibilityPublic, "title_override": "Open supper"}))
	teen := f.outsiders[0]
	gormMust(t, f.app.DB.Model(teen).Update("account_type", models.AccountTeen))
	var guardians []*models.User
	for _, name := range []string{"teen mom", "teen dad"} {
		g := newTestUser(t, f.app, name, "password1234", models.RoleMember)
		gormMust(t, f.app.DB.Model(g).Update("account_type", models.AccountAdult))
		gormMust(t, f.app.DB.Create(&models.GuardianLink{GuardianUserID: g.ID, TeenUserID: &teen.ID, Status: models.GuardianConfirmed}))
		guardians = append(guardians, g)
	}
	gormMust(t, f.app.DB.Create(&models.Setting{Key: settingGuardianCopies, Value: "true"}))
	gormMust(t, f.app.DB.Create(&models.Registration{UserID: &teen.ID, OccurrenceID: f.dates[1].ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
	gormMust(t, f.app.DB.Create(&models.Registration{UserID: &f.memberUser.ID, OccurrenceID: f.dates[1].ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
	save(f)
	if s := regStatus(t, f.app, teen.ID, f.dates[1].ID); s != models.RegCancelled {
		t.Fatalf("the outsider is %s", s)
	}
	if s := regStatus(t, f.app, f.memberUser.ID, f.dates[1].ID); s != models.RegGoing {
		t.Fatalf("the member is %s", s)
	}
	if o := occurrenceRow(t, f.app, f.dates[1].ID); o.TitleOverride == nil || *o.TitleOverride != "NEWPRIVATETITLE" {
		t.Fatalf("the rename did not save: %+v", o)
	}
	drainNotices(t, f.app)
	var stored []models.Notification
	gormMust(t, f.app.DB.Where("user_id = ?", teen.ID).Find(&stored))
	if len(stored) != 1 || stored[0].Kind != notify.RegistrationCancelled {
		t.Fatalf("the outsider's notices: %+v", stored)
	}
	want := "Registration cancelled: Open supper · " + f.date(1)
	if stored[0].Title != want {
		t.Fatalf("the outsider's notice is %q, want %q", stored[0].Title, want)
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(stored[0].Data), &data); err != nil || data[notify.DataEventTitle] != "Open supper" {
		t.Fatalf("stored data %q (%v)", stored[0].Data, err)
	}
	raw, err := json.Marshal(stored[0])
	gormMust(t, &gorm.DB{Error: err})
	if strings.Contains(string(raw), "NEWPRIVATETITLE") {
		t.Fatalf("the outsider's notice carries the new title: %s", raw)
	}
	if m := latestEmail(t, f.app, teen.Email); strings.Contains(m.Subject+m.Body, "NEWPRIVATETITLE") {
		t.Fatalf("the outsider's email carries the new title: %q / %q", m.Subject, m.Body)
	}
	for _, g := range guardians {
		if got := copiesTo(t, f.app, g.Email); len(got) != 1 || got[0] != "Copy: Registration cancelled: Open supper" {
			t.Fatalf("%s copies %v", g.Email, got)
		}
		if m := latestEmail(t, f.app, g.Email); strings.Contains(m.Subject+m.Body, "NEWPRIVATETITLE") {
			t.Fatalf("%s's copy carries the new title: %q / %q", g.Email, m.Subject, m.Body)
		}
	}
}

// TestDateEditTightenWithholdsATitleSetWhileHidden: a date of an archived
// group is hidden from everyone but its leaders and admins, though its
// registrations stay. A title an admin sets then was never the retained
// outsider's to see, so when a later guarded save tightens the date, the
// outsider is cancelled under no title at all and their guardians get no copy
// (a notice with no plain title is never copied, DEC-149).
func TestDateEditTightenWithholdsATitleSetWhileHidden(t *testing.T) {
	f := newDateEditFixture(t)
	gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", f.dates[1].ID).Updates(map[string]any{"visibility_override": models.VisibilityPublic, "title_override": "Open supper"}))
	teen := f.outsiders[0]
	gormMust(t, f.app.DB.Model(teen).Update("account_type", models.AccountTeen))
	var guardians []*models.User
	for _, name := range []string{"archived teen mom", "archived teen dad"} {
		g := newTestUser(t, f.app, name, "password1234", models.RoleMember)
		gormMust(t, f.app.DB.Model(g).Update("account_type", models.AccountAdult))
		gormMust(t, f.app.DB.Create(&models.GuardianLink{GuardianUserID: g.ID, TeenUserID: &teen.ID, Status: models.GuardianConfirmed}))
		guardians = append(guardians, g)
	}
	gormMust(t, f.app.DB.Create(&models.Setting{Key: settingGuardianCopies, Value: "true"}))
	gormMust(t, f.app.DB.Create(&models.Registration{UserID: &teen.ID, OccurrenceID: f.dates[1].ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}))

	apiDo(t, f.admin, "PUT", "/api/v1/groups/"+itoa(f.group), map[string]any{"status": models.GroupArchived}, http.StatusOK)
	if s := regStatus(t, f.app, teen.ID, f.dates[1].ID); s != models.RegGoing {
		t.Fatalf("fixture: archiving left the teen %s", s)
	}
	base := editBaseOf(t, f.admin, f.event.ID, f.date(1))
	w := wantDateEdit(t, f.admin, f.event.ID, f.date(1), dateEditBody(newDateAttempt(t), base.Revision, map[string]any{"title": "HIDDENTITLE"}), http.StatusOK, "")
	wantDateEdit(t, f.admin, f.event.ID, f.date(1), dateEditBody(newDateAttempt(t), answeredRevision(t, w),
		map[string]any{"visibility": "series"}, "consent", dateEditConsent(t, f.admin, f.event.ID, f.date(1), map[string]any{"visibility": "series"})), http.StatusOK, "")
	if s := regStatus(t, f.app, teen.ID, f.dates[1].ID); s != models.RegCancelled {
		t.Fatalf("the retained outsider is %s", s)
	}
	drainNotices(t, f.app)
	var stored []models.Notification
	gormMust(t, f.app.DB.Where("user_id = ?", teen.ID).Find(&stored))
	if len(stored) != 1 || stored[0].Kind != notify.RegistrationCancelled {
		t.Fatalf("the retained outsider's notices: %+v", stored)
	}
	if want := "Registration cancelled: An event you registered for"; stored[0].Title != want {
		t.Fatalf("the retained outsider's notice is %q, want %q", stored[0].Title, want)
	}
	raw, err := json.Marshal(stored[0])
	gormMust(t, &gorm.DB{Error: err})
	if strings.Contains(string(raw), "HIDDENTITLE") {
		t.Fatalf("the retained outsider's notice carries the hidden title: %s", raw)
	}
	if m := latestEmail(t, f.app, teen.Email); strings.Contains(m.Subject+m.Body, "HIDDENTITLE") {
		t.Fatalf("the retained outsider's email carries the hidden title: %q / %q", m.Subject, m.Body)
	}
	msgs, err := devOutbox(t, f.app).List(200)
	gormMust(t, &gorm.DB{Error: err})
	for _, g := range guardians {
		if got := copiesTo(t, f.app, g.Email); len(got) != 0 {
			t.Fatalf("%s got copies %v of a notice with no title", g.Email, got)
		}
		for _, m := range msgs {
			if m.To == g.Email && strings.Contains(m.Subject+m.Body, "HIDDENTITLE") {
				t.Fatalf("%s was mailed the hidden title: %q / %q", g.Email, m.Subject, m.Body)
			}
		}
	}
}

// TestDateEditLateFailureRollsBackEverything: a failure after the receipt is
// written rolls back the date, its cancellations, promotion, notices and the
// reconcile pass that ended an expired grant on the edited date.
func TestDateEditLateFailureRollsBackEverything(t *testing.T) {
	f := newDepartureReadFixture(t, models.VisibilityInviteOnly, models.RegGoing, policy.OccurrenceOriginDepartureSelf)
	target := f.dates[1] // the retained grant's own date
	date := models.FormatCalendarDate(target.OriginalDate)
	gormMust(t, f.app.DB.Model(&models.Event{}).Where("id = ?", f.event.ID).Update("waitlist_enabled", true))
	gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", target.ID).Updates(map[string]any{"visibility_override": models.VisibilityPublic, "capacity_override": 1}))
	member := newTestUser(t, f.app, "waiting member", "password1234", models.RoleMember)
	gormMust(t, f.app.DB.Create(&models.Membership{UserID: member.ID, GroupID: f.group, Role: models.MembershipMember, Status: models.MembershipActive}))
	gormMust(t, f.app.DB.Create(&models.Registration{UserID: &f.other.ID, OccurrenceID: target.ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
	at := time.Now().UTC().Add(-time.Minute)
	gormMust(t, f.app.DB.Create(&models.Registration{UserID: &member.ID, OccurrenceID: target.ID, Status: models.RegWaitlisted, WaitlistedAt: &at, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
	// The clock is past the edited date's stored end, so its before pass
	// ends the retained grant; the edit moves the date a month on, and the
	// departed holder, no longer a member, is cancelled with the outsider.
	newMutableClock(f.app, target.EndsAt.Add(time.Minute))
	base := editBaseOf(t, f.admin, f.event.ID, date)
	var reached []string
	f.app.hooks.dateEditStep = func(tx *gorm.DB, step string) error {
		reached = append(reached, step)
		if step != "receipt" {
			return nil
		}
		var receipt []byte
		if err := tx.Raw("SELECT edit_receipt FROM event_occurrences WHERE id = ?", target.ID).Row().Scan(&receipt); err != nil || len(receipt) != 32 {
			t.Errorf("the receipt is not written before the hook: %x %v", receipt, err)
		}
		var ended int64
		tx.Model(&models.Registration{}).Where("id = ? AND access_ended_at IS NOT NULL", f.access.RegistrationID).Count(&ended)
		var promoted int64
		tx.Model(&models.Registration{}).Where("user_id = ? AND occurrence_id = ? AND status = ?", member.ID, target.ID, models.RegGoing).Count(&promoted)
		if ended != 1 || promoted != 1 {
			t.Errorf("before the failure: grant ended %d, promoted %d", ended, promoted)
		}
		return errors.New("late failure")
	}
	state := eventWriterState(t, f.app, f.event.ID)
	w := postDateEdit(f.admin, f.event.ID, date, dateEditBody(newDateAttempt(t), base.Revision,
		map[string]any{"visibility": "series", "startsLocal": target.StartsAt.AddDate(0, 0, 30).Format("2006-01-02T15:04")}, "consent", dateEditConsent(t, f.admin, f.event.ID, date, map[string]any{"visibility": "series", "startsLocal": target.StartsAt.AddDate(0, 0, 30).Format("2006-01-02T15:04")})))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("late failure: %d %s", w.Code, w.Body.String())
	}
	if strings.Join(reached, ",") != "selected,effects,receipt" {
		t.Fatalf("steps reached: %v", reached)
	}
	if after := eventWriterState(t, f.app, f.event.ID); after != state {
		t.Fatalf("a failed edit left writes:\n%s\n%s", state, after)
	}
	if savedOccurrenceAccess(t, f).Status != policy.OccurrenceAccessActive {
		t.Fatal("the failed edit committed the grant's ending")
	}
}

// bookedClash books event A's date in the hall and another event of the group
// in the hall four hours later, and answers that later start: a move of A onto
// it loses the room.
func bookedClash(t *testing.T, f bookingFixture) string {
	t.Helper()
	if code, body := f.book(t, f.leader, f.eventA, map[string]any{"roomId": f.hall.ID}); code != http.StatusOK {
		t.Fatalf("book A: %d %s", code, body)
	}
	other := createFeedEvent(t, f.leader, f.group, "Other", f.startOn.Add(4*time.Hour), "")
	if code, body := f.book(t, f.leader, other, map[string]any{"roomId": f.hall.ID}); code != http.StatusOK {
		t.Fatalf("book Other: %d %s", code, body)
	}
	return f.startOn.Add(4 * time.Hour).Format("2006-01-02T15:04")
}

func bookingOf(t *testing.T, app *App, eventID uint) models.RoomBooking {
	t.Helper()
	var b models.RoomBooking
	gormMust(t, app.DB.Joins("JOIN event_occurrences o ON o.id = room_bookings.occurrence_id").Where("o.event_id = ?", eventID).First(&b))
	return b
}

func answeredDate(t *testing.T, w *httptest.ResponseRecorder) occurrenceJSON {
	t.Helper()
	var body occurrenceResponseJSON
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("answer %q: %v", w.Body.String(), err)
	}
	return body.Occurrence
}

// TestDateEditSettlesTheRoom: a guarded move keeps a free room and moves its
// booking; a move into a conflict loses the room, tells the leader once, and
// the answer shows the date as a fresh read does, with no room.
func TestDateEditSettlesTheRoom(t *testing.T) {
	f := newBookingFixture(t)
	clash := bookedClash(t, f)
	day := feedDate(f.startOn)
	base := editBaseOf(t, f.leader, f.eventA.ID, day)
	if base.Room == nil || base.Room.ID != f.hall.ID {
		t.Fatalf("fixture: the edit base shows room %+v", base.Room)
	}
	free := f.startOn.Add(time.Hour)
	w := wantDateEdit(t, f.leader, f.eventA.ID, day, dateEditBody(newDateAttempt(t), base.Revision, map[string]any{"startsLocal": free.Format("2006-01-02T15:04")}), http.StatusOK, "")
	if got := answeredDate(t, w); got.Room == nil || got.Room.ID != f.hall.ID {
		t.Fatalf("a free move answered room %+v", got.Room)
	}
	if b := bookingOf(t, f.app, f.eventA.ID); b.Status != models.BookingConfirmed || !b.StartsAt.Equal(free) {
		t.Fatalf("the booking after a free move: %+v", b)
	}

	w = wantDateEdit(t, f.admin, f.eventA.ID, day, dateEditBody(newDateAttempt(t), answeredRevision(t, w), map[string]any{"startsLocal": clash}), http.StatusOK, "")
	got := answeredDate(t, w)
	if got.Room != nil {
		t.Fatalf("a move into a conflict answered room %+v", got.Room)
	}
	if fresh := editBaseOf(t, f.admin, f.eventA.ID, day); fresh.Room != nil || fresh.Revision != got.Revision || !fresh.StartsAt.Equal(got.StartsAt) {
		t.Fatalf("the answer %+v disagrees with a fresh read %+v", got, fresh)
	}
	if b := bookingOf(t, f.app, f.eventA.ID); b.Status != models.BookingRequested || !b.StartsAt.Equal(f.startOn.Add(4*time.Hour)) {
		t.Fatalf("the booking after a conflicting move: %+v", b)
	}
	if n := contactCount(t, f.app, &models.Notification{}, "user_id = ? AND kind = ?", f.leaderUser.ID, notify.RoomLost); n != 1 {
		t.Fatalf("%d room.lost notices to the leader, want 1", n)
	}
	if n := contactCount(t, f.app, &models.Notification{}, "kind = ?", notify.RoomLost); n != 1 {
		t.Fatalf("%d room.lost notices in all, want 1", n)
	}
}

// TestDateEditRoomLossRollsBack: a late failure after the date lost its room
// undoes the lost booking and its notice with everything else.
func TestDateEditRoomLossRollsBack(t *testing.T) {
	f := newBookingFixture(t)
	clash := bookedClash(t, f)
	day := feedDate(f.startOn)
	before := bookingOf(t, f.app, f.eventA.ID)
	base := editBaseOf(t, f.leader, f.eventA.ID, day)
	f.app.hooks.dateEditStep = func(tx *gorm.DB, step string) error {
		if step != "receipt" {
			return nil
		}
		var b models.RoomBooking
		if err := tx.First(&b, before.ID).Error; err != nil || b.Status != models.BookingRequested {
			t.Errorf("before the failure the booking is %+v (%v)", b, err)
		}
		var lost int64
		tx.Model(&models.Notification{}).Where("kind = ?", notify.RoomLost).Count(&lost)
		if lost != 1 {
			t.Errorf("before the failure: %d room.lost notices", lost)
		}
		return errors.New("late failure")
	}
	state := eventWriterState(t, f.app, f.eventA.ID)
	w := postDateEdit(f.admin, f.eventA.ID, day, dateEditBody(newDateAttempt(t), base.Revision, map[string]any{"startsLocal": clash}))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("late failure: %d %s", w.Code, w.Body.String())
	}
	if after := eventWriterState(t, f.app, f.eventA.ID); after != state {
		t.Fatalf("a failed edit left writes:\n%s\n%s", state, after)
	}
	if b := bookingOf(t, f.app, f.eventA.ID); b.Status != models.BookingConfirmed || !b.StartsAt.Equal(before.StartsAt) || !b.EndsAt.Equal(before.EndsAt) {
		t.Fatalf("the booking after the rollback: %+v", b)
	}
	if n := contactCount(t, f.app, &models.Notification{}, "kind = ?", notify.RoomLost); n != 0 {
		t.Fatalf("%d room.lost notices survived the rollback", n)
	}
}

// TestDateEditClearsCapacityWithNull: null clears a date's cap to the event's,
// promoting into the seats that opened; an omitted field changes nothing.
func TestDateEditClearsCapacityWithNull(t *testing.T) {
	f := newDateEditFixture(t)
	gormMust(t, f.app.DB.Model(&models.Event{}).Where("id = ?", f.event.ID).Update("waitlist_enabled", true))
	gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", f.dates[0].ID).Update("capacity_override", 1))
	at := time.Now().UTC().Add(-time.Minute)
	gormMust(t, f.app.DB.Create(&models.Registration{UserID: &f.leaderUser.ID, OccurrenceID: f.dates[0].ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
	gormMust(t, f.app.DB.Create(&models.Registration{UserID: &f.memberUser.ID, OccurrenceID: f.dates[0].ID, Status: models.RegWaitlisted, WaitlistedAt: &at, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
	base := editBaseOf(t, f.admin, f.event.ID, f.date(0))
	w := wantDateEdit(t, f.admin, f.event.ID, f.date(0), dateEditBody(newDateAttempt(t), base.Revision, map[string]any{"title": "Seats for all"}), http.StatusOK, "")
	if o := occurrenceRow(t, f.app, f.dates[0].ID); o.CapacityOverride == nil || *o.CapacityOverride != 1 {
		t.Fatal("an omitted capacity changed the cap")
	}
	wantDateEdit(t, f.admin, f.event.ID, f.date(0), dateEditBody(newDateAttempt(t), answeredRevision(t, w), map[string]any{"capacityOverride": nil}), http.StatusOK, "")
	if o := occurrenceRow(t, f.app, f.dates[0].ID); o.CapacityOverride != nil {
		t.Fatalf("null left the cap at %d", *o.CapacityOverride)
	}
	if s := regStatus(t, f.app, f.memberUser.ID, f.dates[0].ID); s != models.RegGoing {
		t.Fatalf("the waitlisted member is %s after the cap was cleared", s)
	}
}

// TestDateEditTokenFromEveryManagerSurface: a token from each manager read
// edits with no false conflict, for a leader, an admin and a church-wide host.
func TestDateEditTokenFromEveryManagerSurface(t *testing.T) {
	f := newDateEditFixture(t)
	from, to := f.date(0), f.date(3)
	gid := itoa(f.group)
	surfaces := f.tokenSurfaces(t)
	n := 0
	for _, who := range []struct {
		name   string
		client *apiClient
	}{{"admin", f.admin}, {"leader", f.leader}} {
		for _, name := range tokenSurfaceNames {
			n++
			date := f.date(n % 4)
			token := surfaces[name](who.client, date)
			w := wantDateEdit(t, who.client, f.event.ID, date, dateEditBody(newDateAttempt(t), token, map[string]any{"title": fmt.Sprintf("Edit %d", n)}), http.StatusOK, "")
			// The answer's own token edits again.
			wantDateEdit(t, who.client, f.event.ID, date, dateEditBody(newDateAttempt(t), answeredRevision(t, w), map[string]any{"durationMinutes": 30 + n}), http.StatusOK, "")
		}
	}
	for _, c := range []*apiClient{f.member, anonClient(t, f.app, "10.9.9.9")} {
		for _, path := range []string{fmt.Sprintf("/api/v1/events/%d", f.event.ID), groupDetailPath(f.group), "/api/v1/groups/" + gid + "/events?from=" + from + "&to=" + to} {
			if raw := c.do("GET", path, nil, false).Body.String(); strings.Contains(raw, `"revision"`) {
				t.Fatalf("a non-manager read of %s carries a revision: %s", path, raw)
			}
		}
	}

	// A church-wide event's host manages it with no group place.
	host := newTestUser(t, f.app, "edit host", "password1234", models.RoleMember)
	w := f.admin.do("POST", "/api/v1/events", map[string]any{"title": "Church picnic", "startsLocal": feedStart(5).Format("2006-01-02T15:04"), "durationMinutes": 60, "tzid": "UTC", "location": "Park"}, true)
	cw := requireCreatedEvent(t, f.app, w)
	apiDo(t, f.admin, "POST", fmt.Sprintf("/api/v1/events/%d/hosts", cw.ID), map[string]any{"email": host.Email}, http.StatusOK)
	hc := loginAPI(t, f.app, host.Name, "password1234")
	page, _ := getEvent(t, hc, cw.ID, http.StatusOK)
	if len(page.Occurrences) != 1 || !strings.HasSuffix(page.Occurrences[0].Revision, ".-") {
		t.Fatalf("the host's page: %+v", page.Occurrences)
	}
	wantDateEdit(t, hc, cw.ID, page.Occurrences[0].Date, dateEditBody(newDateAttempt(t), page.Occurrences[0].Revision, map[string]any{"location": "Lake"}), http.StatusOK, "")
}

// TestManagedDateReadCarriesTheDatesOwnValues: the manager's read of a date,
// and the edit's answer, carry what the date holds of its own (DEC-164),
// exactly as the edit writes it: each field absent while the date falls back,
// present once set (also to the event's own value, and an empty place), and
// absent again once cleared. A clock that differs from the series start with
// no start of the date's own carries none. No other read, a manager's list
// included, carries it.
func TestManagedDateReadCarriesTheDatesOwnValues(t *testing.T) {
	f := newDateEditFixture(t)
	gormMust(t, f.app.DB.Model(&models.Event{}).Where("id = ?", f.event.ID).Update("remote_available", true))
	date := f.date(0)
	none := dateOwnJSON{}
	ownOf := func(o occurrenceJSON) dateOwnJSON {
		t.Helper()
		if o.Own == nil {
			t.Fatalf("the manager's read of %s has no own values", o.Date)
		}
		return *o.Own
	}
	if got := ownOf(editBaseOf(t, f.admin, f.event.ID, date)); got != none {
		t.Fatalf("an unedited date's own values = %+v", got)
	}
	w := f.admin.do("GET", managedDatePath(f.event.ID, date), nil, false)
	if !strings.Contains(w.Body.String(), `"own":{}`) {
		t.Fatalf("an unedited date's read: %s", w.Body.String())
	}

	str := func(s string) *string { return &s }
	num := func(n int) *int { return &n }
	moved := f.dates[0].StartsAt.Add(90 * time.Minute).Format("2006-01-02T15:04")
	vis := models.VisibilityPrivate
	for _, step := range []struct {
		name    string
		changes map[string]any
		want    dateOwnJSON
	}{
		{"start", map[string]any{"startsLocal": moved}, dateOwnJSON{StartsLocal: &moved}},
		{"duration", map[string]any{"durationMinutes": 75}, dateOwnJSON{StartsLocal: &moved, DurationMinutes: num(75)}},
		{"title equal to the event's", map[string]any{"title": f.event.Title}, dateOwnJSON{StartsLocal: &moved, DurationMinutes: num(75), Title: str(f.event.Title)}},
		{"empty place", map[string]any{"location": ""}, dateOwnJSON{StartsLocal: &moved, DurationMinutes: num(75), Title: str(f.event.Title), Location: str("")}},
		{"seat limits and visibility", map[string]any{"capacityOverride": 12, "remoteCapacityOverride": 0, "visibility": vis},
			dateOwnJSON{StartsLocal: &moved, DurationMinutes: num(75), Title: str(f.event.Title), Location: str(""), CapacityOverride: num(12), RemoteCapacityOverride: num(0), Visibility: &vis}},
		{"cleared", map[string]any{"capacityOverride": nil, "remoteCapacityOverride": nil, "visibility": "series"},
			dateOwnJSON{StartsLocal: &moved, DurationMinutes: num(75), Title: str(f.event.Title), Location: str("")}},
	} {
		answer := wantEditedDate(t, saveDate(t, f.admin, f.event.ID, date, step.changes, http.StatusOK, ""), editBaseOf(t, f.admin, f.event.ID, date))
		for name, got := range map[string]dateOwnJSON{"answer": ownOf(answer), "read": ownOf(editBaseOf(t, f.admin, f.event.ID, date))} {
			if !reflect.DeepEqual(got, step.want) {
				t.Fatalf("%s: the %s's own values = %s, want %s", step.name, name, ownString(got), ownString(step.want))
			}
		}
	}

	// A clock of the date's that is not its own (as a rule's own hour gives it) is not an own start.
	gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", f.dates[1].ID).
		Updates(map[string]any{"starts_at": f.dates[1].StartsAt.Add(time.Hour), "ends_at": f.dates[1].EndsAt.Add(time.Hour)}))
	other := editBaseOf(t, f.admin, f.event.ID, f.date(1))
	if other.StartsLocal[11:] == other.SeriesStartsLocal[11:] {
		t.Fatalf("the shifted date starts at the series time: %s", other.StartsLocal)
	}
	if got := ownOf(other); got != none {
		t.Fatalf("a date whose clock differs from the series start with no own start has own values %s", ownString(got))
	}

	from, to := f.date(0), f.date(3)
	gid := itoa(f.group)
	reads := []string{
		fmt.Sprintf("/api/v1/events/%d", f.event.ID), groupDetailPath(f.group), "/api/v1/groups/" + gid + "/events?from=" + from + "&to=" + to,
		fmt.Sprintf("/api/v1/events/%d/dates/%s", f.event.ID, date), "/api/v1/calendar?from=" + from + "&to=" + to,
	}
	for _, c := range []*apiClient{f.admin, f.leader, f.member, anonClient(t, f.app, "10.9.9.8")} {
		for _, path := range reads {
			if raw := c.do("GET", path, nil, false).Body.String(); strings.Contains(raw, `"own":`) {
				t.Fatalf("%s carries a date's own values: %s", path, raw)
			}
		}
	}
	if raw := f.member.do("GET", managedDatePath(f.event.ID, date), nil, false); raw.Code != http.StatusForbidden || strings.Contains(raw.Body.String(), `"own":`) {
		t.Fatalf("a member's manager read: %d %s", raw.Code, raw.Body.String())
	}
}

func ownString(o dateOwnJSON) string {
	b, _ := json.Marshal(o)
	return string(b)
}

// TestDateEditPendingGroupLeader: the leader of a group still waiting for
// approval manages its dates, so every manager surface gives them a token
// that edits, with the group's place tagged.
func TestDateEditPendingGroupLeader(t *testing.T) {
	f := newDateEditFixture(t)
	gormMust(t, f.app.DB.Model(&models.Group{}).Where("id = ?", f.group).Update("status", models.GroupPending))
	surfaces := f.tokenSurfaces(t)
	for i, name := range tokenSurfaceNames {
		date := f.date(i)
		token := surfaces[name](f.leader, date)
		if strings.HasSuffix(token, ".-") {
			t.Fatalf("%s: the group's place is not tagged: %s", name, token)
		}
		w := wantDateEdit(t, f.leader, f.event.ID, date, dateEditBody(newDateAttempt(t), token, map[string]any{"title": "Pending " + name}), http.StatusOK, "")
		wantDateEdit(t, f.leader, f.event.ID, date, dateEditBody(newDateAttempt(t), answeredRevision(t, w), map[string]any{"durationMinutes": 45}), http.StatusOK, "")
	}
	if raw := f.member.do("GET", fmt.Sprintf("/api/v1/events/%d", f.event.ID), nil, false).Body.String(); strings.Contains(raw, `"revision"`) {
		t.Fatalf("a member's read of a pending group's event carries a revision: %s", raw)
	}
}

var tokenSurfaceNames = []string{"event page", "group page", "group events", "edit base"}

// tokenSurfaces reads one date's revision from each manager surface.
func (f dateEditFixture) tokenSurfaces(t *testing.T) map[string]func(c *apiClient, date string) string {
	t.Helper()
	from, to := f.date(0), f.date(3)
	gid := itoa(f.group)
	return map[string]func(c *apiClient, date string) string{
		"event page": func(c *apiClient, date string) string {
			page, _ := getEvent(t, c, f.event.ID, http.StatusOK)
			return occurrenceOn(t, page.Occurrences, date).Revision
		},
		"group page": func(c *apiClient, date string) string {
			var detail groupDetailJSON
			decodeJSON(t, c.do("GET", groupDetailPath(f.group), nil, false), &detail)
			return occurrenceOn(t, detail.UpcomingEvents, date).Revision
		},
		"group events": func(c *apiClient, date string) string {
			return occurrenceOn(t, listOccurrences(t, c, gid, from, to), date).Revision
		},
		"edit base": func(c *apiClient, date string) string { return editBaseOf(t, c, f.event.ID, date).Revision },
	}
}

// TestDateEditInheritedPlace: when the date shows the group's place, a change
// to that place refuses an earlier token, even one to the value the person
// typed; with a place of the event's own, a group-place change does not.
func TestDateEditInheritedPlace(t *testing.T) {
	f := newDateEditFixture(t)
	gid := itoa(f.group)
	base := editBaseOf(t, f.admin, f.event.ID, f.date(0))
	if base.Location != "Group hall" || strings.HasSuffix(base.Revision, ".-") {
		t.Fatalf("an inherited place: %+v", base)
	}
	apiDo(t, f.admin, "PUT", "/api/v1/groups/"+gid, map[string]any{"location": "Chapel"}, http.StatusOK)
	wantDateEdit(t, f.admin, f.event.ID, f.date(0), dateEditBody(newDateAttempt(t), base.Revision, map[string]any{"title": "Still here"}), http.StatusConflict, "date_changed")
	base = editBaseOf(t, f.admin, f.event.ID, f.date(0))
	apiDo(t, f.admin, "PUT", "/api/v1/groups/"+gid, map[string]any{"location": "Garden"}, http.StatusOK)
	wantDateEdit(t, f.admin, f.event.ID, f.date(0), dateEditBody(newDateAttempt(t), base.Revision, map[string]any{"location": "Garden"}), http.StatusConflict, "date_changed")
	if o := occurrenceRow(t, f.app, f.dates[0].ID); o.LocationOverride != nil {
		t.Fatal("a refused edit pinned the place")
	}
	// Back to the place the token was read with: the tag matches again.
	apiDo(t, f.admin, "PUT", "/api/v1/groups/"+gid, map[string]any{"location": "Chapel"}, http.StatusOK)
	wantDateEdit(t, f.admin, f.event.ID, f.date(0), dateEditBody(newDateAttempt(t), base.Revision, map[string]any{"title": "Chapel supper"}), http.StatusOK, "")

	apiDo(t, f.admin, "PUT", fmt.Sprintf("/api/v1/events/%d", f.event.ID), seriesBody(t, f.admin, fmt.Sprintf("/api/v1/events/%d", f.event.ID), map[string]any{"location": "Own room"}), http.StatusOK)
	own := editBaseOf(t, f.admin, f.event.ID, f.date(1))
	if !strings.HasSuffix(own.Revision, ".-") {
		t.Fatalf("an event place still tags the group: %s", own.Revision)
	}
	apiDo(t, f.admin, "PUT", "/api/v1/groups/"+gid, map[string]any{"location": "Somewhere else"}, http.StatusOK)
	wantDateEdit(t, f.admin, f.event.ID, f.date(1), dateEditBody(newDateAttempt(t), own.Revision, map[string]any{"title": "Own supper"}), http.StatusOK, "")
}

// TestDateEditClearingTheInheritedPlaceTellsAttendees: a date that shows its
// group's place loses it when the date's place is set to empty, so a member
// going is told of the place change.
func TestDateEditClearingTheInheritedPlaceTellsAttendees(t *testing.T) {
	f := newDateEditFixture(t)
	gormMust(t, f.app.DB.Create(&models.Registration{UserID: &f.memberUser.ID, OccurrenceID: f.dates[0].ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
	base := editBaseOf(t, f.admin, f.event.ID, f.date(0))
	if base.Location != "Group hall" {
		t.Fatalf("fixture: the date shows %q", base.Location)
	}
	w := wantDateEdit(t, f.admin, f.event.ID, f.date(0), dateEditBody(newDateAttempt(t), base.Revision, map[string]any{"location": ""}), http.StatusOK, "")
	if got := answeredDate(t, w); got.Location != "" {
		t.Fatalf("the answer still shows %q", got.Location)
	}
	var told []models.Notification
	gormMust(t, f.app.DB.Where("user_id = ? AND kind = ?", f.memberUser.ID, notify.EventChanged).Find(&told))
	if len(told) != 1 {
		t.Fatalf("the member going got %d change notices: %+v", len(told), told)
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(told[0].Data), &data); err != nil || data["change"] != "place" {
		t.Fatalf("the change notice data %q (%v)", told[0].Data, err)
	}
}

// placeNotices is every event.changed row about occID for uid.
func placeNotices(t *testing.T, app *App, uid, occID uint) []models.Notification {
	t.Helper()
	var rows []models.Notification
	gormMust(t, app.DB.Where("user_id = ? AND kind = ? AND subject_id = ?", uid, notify.EventChanged, occID).Order("id").Find(&rows))
	return rows
}

// TestDateEditPlaceChangeIsJudgedPerRecipient: whether a date's place changed
// depends on who reads it, since only someone who may see the group sees its
// place. Pinning the group's place on a public date of an invite-only group
// shows it to an outsider going, who is told; the member going sees the same
// place as before and is not. Clearing an inherited place is the reverse: the
// member is told, the outsider, who never saw it, is not.
func TestDateEditPlaceChangeIsJudgedPerRecipient(t *testing.T) {
	f := newDateEditFixture(t)
	pinned, cleared := f.dates[1], f.dates[2]
	for i, d := range []models.EventOccurrence{pinned, cleared} {
		gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", d.ID).Update("visibility_override", models.VisibilityPublic))
		for _, u := range []*models.User{f.outsiders[i], f.memberUser} {
			gormMust(t, f.app.DB.Create(&models.Registration{UserID: &u.ID, OccurrenceID: d.ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
		}
	}
	base := editBaseOf(t, f.admin, f.event.ID, f.date(1))
	wantDateEdit(t, f.admin, f.event.ID, f.date(1), dateEditBody(newDateAttempt(t), base.Revision, map[string]any{"location": "Group hall"}), http.StatusOK, "")
	if got := placeNotices(t, f.app, f.outsiders[0].ID, pinned.ID); len(got) != 1 || !strings.Contains(got[0].Body, "The place changed.") || !strings.Contains(got[0].Body, "Group hall") {
		t.Fatalf("the outsider going, newly shown the place: %+v", got)
	}
	if got := placeNotices(t, f.app, f.memberUser.ID, pinned.ID); len(got) != 0 {
		t.Fatalf("the member going, whose place did not change: %+v", got)
	}

	base = editBaseOf(t, f.admin, f.event.ID, f.date(2))
	wantDateEdit(t, f.admin, f.event.ID, f.date(2), dateEditBody(newDateAttempt(t), base.Revision, map[string]any{"location": ""}), http.StatusOK, "")
	if got := placeNotices(t, f.app, f.memberUser.ID, cleared.ID); len(got) != 1 || !strings.Contains(got[0].Body, "The place changed.") {
		t.Fatalf("the member going, who lost the place: %+v", got)
	}
	if got := placeNotices(t, f.app, f.outsiders[1].ID, cleared.ID); len(got) != 0 {
		t.Fatalf("the outsider going, whose place was already blank: %+v", got)
	}
}

// invitationNotices is every occurrence.changed uid got about their exact-date
// grant regID: the invitation's own notice, which names nothing of the date.
func invitationNotices(t *testing.T, app *App, uid, regID uint) []models.Notification {
	t.Helper()
	var rows []models.Notification
	gormMust(t, app.DB.Where("user_id = ? AND kind = ? AND subject_type = ? AND subject_id = ?", uid, notify.OccurrenceChanged, accessTargetType, regID).Order("id").Find(&rows))
	return rows
}

// TestDateEditPlaceChangeFollowsEachReadersOwnView: a guest sees a public
// group's place as a signed-out visitor does, and someone who reaches a date
// only through an exact-date invitation sees it without its group, even when
// they may see the group itself. So pinning the group's place tells the
// invitee, in the invitation's own words, and not the guest; clearing an
// inherited place tells the guest and not the invitee.
func TestDateEditPlaceChangeFollowsEachReadersOwnView(t *testing.T) {
	f := newDateEditFixture(t)
	gormMust(t, f.app.DB.Model(&models.Group{}).Where("id = ?", f.group).Update("visibility", models.VisibilityPublic))
	var grp models.Group
	gormMust(t, f.app.DB.First(&grp, f.group))
	var ev models.Event
	gormMust(t, f.app.DB.First(&ev, f.event.ID))
	guestPin, invitePin, guestClear, inviteClear := 0, 1, 2, 3
	guests := map[int]*models.User{}
	guestRegs := map[int]models.Registration{}
	for _, i := range []int{guestPin, guestClear} {
		gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", f.dates[i].ID).Update("visibility_override", models.VisibilityPublic))
		g := &models.User{Name: "Place guest " + itoa(uint(i)), Email: "place-guest-" + itoa(uint(i)) + "@example.com", Role: models.RoleMember, Status: models.UserGuest}
		gormMust(t, f.app.DB.Create(g))
		r := models.Registration{UserID: &g.ID, OccurrenceID: f.dates[i].ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}
		gormMust(t, f.app.DB.Create(&r))
		guests[i], guestRegs[i] = g, r
	}
	invitees := map[int]*models.User{invitePin: f.outsiders[0], inviteClear: f.outsiders[1]}
	grants := map[int]uint{}
	for i, u := range invitees {
		grants[i] = seedReadInvitation(t, f.app, f.adminUser, u, f.event, f.dates[i]).ID
	}
	guestSees := func(i int) string {
		o := occurrenceRow(t, f.app, f.dates[i].ID)
		r := guestRegs[i]
		return f.app.guestDisplayWith(func() string { return "" }, &r, &o, &ev, &grp).Location
	}
	inviteeSees := func(i int) string {
		var page struct {
			Occurrence occurrenceJSON `json:"occurrence"`
		}
		client := loginAPI(t, f.app, invitees[i].Name, "password1234")
		gormMust(t, &gorm.DB{Error: json.Unmarshal([]byte(apiDo(t, client, "GET", dateReadPath(f.event.ID, f.date(i)), nil, http.StatusOK)), &page)})
		return page.Occurrence.Location
	}
	if guestSees(guestPin) != "Group hall" || guestSees(guestClear) != "Group hall" || inviteeSees(invitePin) != "" || inviteeSees(inviteClear) != "" {
		t.Fatalf("fixture: guests see %q and %q, invitees %q and %q", guestSees(guestPin), guestSees(guestClear), inviteeSees(invitePin), inviteeSees(inviteClear))
	}

	for _, i := range []int{guestPin, invitePin} {
		base := editBaseOf(t, f.admin, f.event.ID, f.date(i))
		wantDateEdit(t, f.admin, f.event.ID, f.date(i), dateEditBody(newDateAttempt(t), base.Revision, map[string]any{"location": "Group hall"}), http.StatusOK, "")
	}
	for _, i := range []int{guestClear, inviteClear} {
		base := editBaseOf(t, f.admin, f.event.ID, f.date(i))
		wantDateEdit(t, f.admin, f.event.ID, f.date(i), dateEditBody(newDateAttempt(t), base.Revision, map[string]any{"location": ""}), http.StatusOK, "")
	}
	if guestSees(guestPin) != "Group hall" || guestSees(guestClear) != "" || inviteeSees(invitePin) != "Group hall" || inviteeSees(inviteClear) != "" {
		t.Fatalf("fixture: after the edits guests see %q and %q, invitees %q and %q", guestSees(guestPin), guestSees(guestClear), inviteeSees(invitePin), inviteeSees(inviteClear))
	}

	if got := placeNotices(t, f.app, guests[guestPin].ID, f.dates[guestPin].ID); len(got) != 0 {
		t.Errorf("the guest, whose place did not change: %+v", got)
	}
	if got := placeNotices(t, f.app, guests[guestClear].ID, f.dates[guestClear].ID); len(got) != 1 || !strings.Contains(got[0].Body, "The place changed.") {
		t.Errorf("the guest, who lost the place: %+v", got)
	}
	got := invitationNotices(t, f.app, invitees[invitePin].ID, grants[invitePin])
	if len(got) != 1 || strings.Contains(got[0].Title+got[0].Body, "Group hall") || strings.Contains(got[0].Title+got[0].Body, "Supper") {
		t.Errorf("the invitee newly shown the place, told in the invitation's words: %+v", got)
	}
	if got := invitationNotices(t, f.app, invitees[inviteClear].ID, grants[inviteClear]); len(got) != 0 {
		t.Errorf("the invitee, whose place was already blank: %+v", got)
	}
	for _, i := range []int{invitePin, inviteClear} {
		if got := placeNotices(t, f.app, invitees[i].ID, f.dates[i].ID); len(got) != 0 {
			t.Errorf("an invitee got the ordinary change notice: %+v", got)
		}
	}
}

// TestDateEditTighteningACancelledDateNamesItOnlyToItsReaders: a cancelled
// date showed its title only to someone entitled to its tombstone. A holder
// whose only row on it is a household person's was not, so when the date,
// renamed after it was cancelled, is tightened, they are told their sign-up
// was cancelled without the title, and the row follows nothing about the
// date. An outsider with their own row, who could read the tombstone, is told
// its title.
func TestDateEditTighteningACancelledDateNamesItOnlyToItsReaders(t *testing.T) {
	f := newDateEditFixture(t)
	target := f.dates[1]
	gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", target.ID).Update("visibility_override", models.VisibilityPublic))
	holder, holderClient, spouse, _ := householdFamily(t, f.app, f.admin, "cancelled date holder")
	reader := f.outsiders[0]
	gormMust(t, f.app.DB.Create(&models.Registration{HouseholdPersonID: &spouse.ID, RegisteredBy: &holder.ID, OccurrenceID: target.ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
	gormMust(t, f.app.DB.Create(&models.Registration{UserID: &reader.ID, OccurrenceID: target.ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
	apiDo(t, f.admin, "POST", "/api/v1/events/"+itoa(f.event.ID)+"/cancel", map[string]any{"date": f.date(1)}, http.StatusOK)
	base := editBaseOf(t, f.admin, f.event.ID, f.date(1))
	w := wantDateEdit(t, f.admin, f.event.ID, f.date(1), dateEditBody(newDateAttempt(t), base.Revision, map[string]any{"title": "HIDDENTITLE"}), http.StatusOK, "")
	if o := occurrenceRow(t, f.app, target.ID); o.Status != models.OccurrenceCancelled {
		t.Fatalf("fixture: the date is %s", o.Status)
	}
	readerClient := loginAPI(t, f.app, reader.Name, "password1234")
	if raw := apiDo(t, readerClient, "GET", dateReadPath(f.event.ID, f.date(1)), nil, http.StatusOK); !strings.Contains(raw, "HIDDENTITLE") {
		t.Fatalf("fixture: the reader can't read the tombstone: %s", raw)
	}
	apiDo(t, holderClient, "GET", dateReadPath(f.event.ID, f.date(1)), nil, http.StatusNotFound)
	mark := markNotices(t, f.app)

	wantDateEdit(t, f.admin, f.event.ID, f.date(1), dateEditBody(newDateAttempt(t), answeredRevision(t, w),
		map[string]any{"visibility": "series"}, "consent", dateEditConsent(t, f.admin, f.event.ID, f.date(1), map[string]any{"visibility": "series"})), http.StatusOK, "")
	got := noticesSince(t, f.app, holder.ID, notify.RegistrationCancelled, mark.notice)
	if len(got) != 1 || got[0].Title != "Registration cancelled: An event you registered for" {
		t.Fatalf("the household holder's cancellation notices: %+v", got)
	}
	wantWithheld(t, f.app, "the household holder", got[0], holderClient, target.ID)
	noSecrets(t, f.app, "the household holder", holder.ID, holder.Email, mark, []string{"HIDDENTITLE", "Supper", "Group hall"})
	told := noticesSince(t, f.app, reader.ID, notify.RegistrationCancelled, mark.notice)
	if len(told) != 1 || told[0].Title != "Registration cancelled: HIDDENTITLE · "+f.date(1) {
		t.Fatalf("the tombstone's reader: %+v", told)
	}
}

// freshProcess is the same database served by a new process: a new App with
// serve's start-up steps.
func freshProcess(t *testing.T, app *App) *App {
	t.Helper()
	fresh := &App{DB: app.DB, Store: app.Store, Config: app.Config, Uploads: app.Uploads, Mailer: app.Mailer}
	bootFence(t, fresh)
	return fresh
}

// TestDateEditRestartRefusesOldTokens: a token or receipt from an earlier
// process never matches, so a retry after a restart is a safe conflict and
// never a second application.
func TestDateEditRestartRefusesOldTokens(t *testing.T) {
	f := newDateEditFixture(t)
	base := editBaseOf(t, f.admin, f.event.ID, f.date(0))
	applied := dateEditBody(newDateAttempt(t), base.Revision, map[string]any{"title": "Before the restart"})
	wantDateEdit(t, f.admin, f.event.ID, f.date(0), applied, http.StatusOK, "")
	unapplied := editBaseOf(t, f.admin, f.event.ID, f.date(1))

	app2 := freshProcess(t, f.app)
	c2 := loginAPI(t, app2, f.adminUser.Name, "password1234")
	state := f.state(t)
	wantDateEdit(t, c2, f.event.ID, f.date(0), applied, http.StatusConflict, "date_changed")
	wantDateEdit(t, c2, f.event.ID, f.date(1), dateEditBody(newDateAttempt(t), unapplied.Revision, map[string]any{"title": "x"}), http.StatusConflict, "date_changed")
	if after := f.state(t); after != state {
		t.Fatal("an old token wrote in a new process")
	}
	fresh := editBaseOf(t, c2, f.event.ID, f.date(1))
	if fresh.Revision[:18] == unapplied.Revision[:18] {
		t.Fatal("a new process minted the same generation")
	}
	wantDateEdit(t, c2, f.event.ID, f.date(1), dateEditBody(newDateAttempt(t), fresh.Revision, map[string]any{"title": "After the restart"}), http.StatusOK, "")
}

// TestDateEditRestoredDumpRefusesExactOldToken: restore.sh restores with the
// app stopped. The exact token from before the dump, sent to the process that
// serves the restored data, is a conflict, not a replay or a second write.
func TestDateEditRestoredDumpRefusesExactOldToken(t *testing.T) {
	fenceTools(t)
	f := newDateEditFixture(t)
	base := editBaseOf(t, f.admin, f.event.ID, f.date(0))
	body := dateEditBody(newDateAttempt(t), base.Revision, map[string]any{"title": "After the dump"})
	dumped := f.state(t)
	dump := fenceDump(t, f.app.DB)
	wantDateEdit(t, f.admin, f.event.ID, f.date(0), body, http.StatusOK, "")
	fenceRestore(t, f.app.DB, dump, false)
	if restored := f.state(t); restored != dumped {
		t.Fatal("fixture: the restore did not bring back the dump")
	}
	app2 := freshProcess(t, f.app)
	c2 := loginAPI(t, app2, f.adminUser.Name, "password1234")
	state := f.state(t)
	wantDateEdit(t, c2, f.event.ID, f.date(0), body, http.StatusConflict, "date_changed")
	if after := f.state(t); after != state {
		t.Fatal("the old token wrote over the restored data")
	}
	if o := occurrenceRow(t, f.app, f.dates[0].ID); o.TitleOverride != nil {
		t.Fatalf("the restored date took the old edit: %+v", o)
	}
}

// TestDateEditRestoreUnderLiveProcessIsTheStatedLimit pins the limit the
// restore barrier shares: restored under the same process, the old token
// matches the restored rows again, and the attempt applies a second time with
// its effects (never a 200 that applied nothing).
func TestDateEditRestoreUnderLiveProcessIsTheStatedLimit(t *testing.T) {
	fenceTools(t)
	f := newDateEditFixture(t)
	base := editBaseOf(t, f.admin, f.event.ID, f.date(0))
	body := dateEditBody(newDateAttempt(t), base.Revision, map[string]any{"title": "Applied twice"})
	dump := fenceDump(t, f.app.DB)
	wantDateEdit(t, f.admin, f.event.ID, f.date(0), body, http.StatusOK, "")
	fenceRestore(t, f.app.DB, dump, false)
	wantDateEdit(t, f.admin, f.event.ID, f.date(0), body, http.StatusOK, "")
	if o := occurrenceRow(t, f.app, f.dates[0].ID); o.TitleOverride == nil || *o.TitleOverride != "Applied twice" || len(o.EditReceipt) != 32 {
		t.Fatalf("after the second application: %+v", o)
	}
}

// TestDateEditExpiryDuringLockWait: a retained departure grant whose date ends
// while the edit waits for the event lock is ended against the stored end,
// under the edit's own clock, before the edit moves the date into the future.
// The departed person, who can no longer see the date, hears only that their
// invitation ended, never its new time, title or place; a member still going
// gets the change.
func TestDateEditExpiryDuringLockWait(t *testing.T) {
	f := newDepartureReadFixture(t, models.VisibilityInviteOnly, models.RegGoing, policy.OccurrenceOriginDepartureSelf)
	target := f.dates[1]
	date := models.FormatCalendarDate(target.OriginalDate)
	member := newTestUser(t, f.app, "still a member", "password1234", models.RoleMember)
	gormMust(t, f.app.DB.Create(&models.Membership{UserID: member.ID, GroupID: f.group, Role: models.MembershipMember, Status: models.MembershipActive}))
	gormMust(t, f.app.DB.Create(&models.Registration{UserID: &member.ID, OccurrenceID: target.ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
	var mark uint
	gormMust(t, f.app.DB.Model(&models.Notification{}).Select("COALESCE(MAX(id), 0)").Scan(&mark))
	clock := newMutableClock(f.app, target.EndsAt.Add(-time.Minute))
	base := editBaseOf(t, f.admin, f.event.ID, date)
	release := holdEventRow(t, f.app, f.event.ID)
	moved := target.StartsAt.AddDate(0, 0, 30).Format("2006-01-02T15:04")
	answer := goPostDateEdit(f.admin, f.event.ID, date, dateEditBody(newDateAttempt(t), base.Revision,
		map[string]any{"startsLocal": moved, "title": "NEWSECRETTITLE", "location": "NEWSECRETPLACE"}))
	waitForLockedUsersQuery(t, f.app, `%FROM "events"%FOR UPDATE%`)
	if savedOccurrenceAccess(t, f).Status != policy.OccurrenceAccessActive {
		t.Fatal("fixture: the grant ended before the wait")
	}
	after := target.EndsAt.Add(time.Minute)
	clock.set(after)
	release()
	if w := awaitResponse(t, answer); w.Code != http.StatusOK {
		t.Fatalf("the edit: %d %s", w.Code, w.Body.String())
	}
	if o := occurrenceRow(t, f.app, target.ID); !o.EndsAt.After(after.AddDate(0, 0, 29)) {
		t.Fatalf("fixture: the date did not move: %+v", o)
	}
	got := savedOccurrenceAccess(t, f)
	if got.Status != policy.OccurrenceAccessEnded || got.EndReason != policy.OccurrenceEnded || got.EndedAt == nil || !got.EndedAt.Equal(after) {
		t.Fatalf("the move revived the expired grant: %+v", got)
	}
	notices := func(uid uint) []models.Notification {
		var rows []models.Notification
		gormMust(t, f.app.DB.Where("user_id = ? AND id > ?", uid, mark).Order("id").Find(&rows))
		return rows
	}
	departed := notices(f.recipient.ID)
	if len(departed) != 1 || departed[0].Kind != notify.OccurrenceUpdated {
		t.Fatalf("the departed person's notices: %+v", departed)
	}
	for _, n := range departed {
		raw, err := json.Marshal(n)
		gormMust(t, &gorm.DB{Error: err})
		for _, secret := range []string{"NEWSECRETTITLE", "NEWSECRETPLACE"} {
			if strings.Contains(string(raw), secret) {
				t.Fatalf("the departed person was told %s: %s", secret, raw)
			}
		}
	}
	var told bool
	for _, n := range notices(member.ID) {
		if n.Kind == notify.EventChanged && strings.Contains(n.Title+n.Body, "NEWSECRETPLACE") {
			told = true
		}
	}
	if !told {
		t.Fatalf("the member still going was not told the change: %+v", notices(member.ID))
	}
}

// TestDateEditHeldZoneChange: a series zone change while the body arrives
// makes the edit stale; a fresh token saves the wall clock in the new zone.
func TestDateEditHeldZoneChange(t *testing.T) {
	f := newDateEditFixture(t)
	base := editBaseOf(t, f.admin, f.event.ID, f.date(0))
	wall := f.dates[0].StartsAt.Add(time.Hour).Format("2006-01-02T15:04")
	held := f.admin.doHeld("POST", dateEditPath(f.event.ID, f.date(0)), dateEditBody(newDateAttempt(t), base.Revision, map[string]any{"startsLocal": wall}))
	apiDo(t, f.admin, "PUT", fmt.Sprintf("/api/v1/events/%d", f.event.ID), seriesBody(t, f.admin, fmt.Sprintf("/api/v1/events/%d", f.event.ID), map[string]any{"tzid": "America/Chicago"}), http.StatusOK)
	held.release()
	if w := held.wait(t); w.Code != http.StatusConflict || answerCode(t, w) != "date_changed" {
		t.Fatalf("the held edit after a zone change: %d %s", w.Code, w.Body.String())
	}
	fresh := editBaseOf(t, f.admin, f.event.ID, f.date(0))
	wantDateEdit(t, f.admin, f.event.ID, f.date(0), dateEditBody(newDateAttempt(t), fresh.Revision, map[string]any{"startsLocal": wall}), http.StatusOK, "")
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	want, err := time.ParseInLocation("2006-01-02T15:04", wall, chicago)
	if err != nil {
		t.Fatal(err)
	}
	if o := occurrenceRow(t, f.app, f.dates[0].ID); !o.StartsAt.Equal(want) {
		t.Fatalf("saved %s, want %s in the new zone", o.StartsAt, want.UTC())
	}
}

// TestDateEditRejectsMalformed: a request that cannot be read writes nothing.
func TestDateEditRejectsMalformed(t *testing.T) {
	f := newDateEditFixture(t)
	base := editBaseOf(t, f.admin, f.event.ID, f.date(0))
	good := newDateAttempt(t)
	state := f.state(t)
	for name, body := range map[string]map[string]any{
		"no attempt":           {"revision": base.Revision, "changes": map[string]any{"title": "x"}},
		"short attempt":        dateEditBody("abcd", base.Revision, map[string]any{"title": "x"}),
		"upper-case attempt":   dateEditBody(strings.ToUpper(good), base.Revision, map[string]any{"title": "x"}),
		"bad revision":         dateEditBody(good, "2.abc", map[string]any{"title": "x"}),
		"empty changes":        dateEditBody(good, base.Revision, map[string]any{}),
		"unknown change":       dateEditBody(good, base.Revision, map[string]any{"description": "x"}),
		"unknown key":          dateEditBody(good, base.Revision, map[string]any{"title": "x"}, "confirm", true),
		"reason alone":         dateEditBody(good, base.Revision, map[string]any{"title": "x"}, "reason", "why"),
		"the old count key":    dateEditBody(good, base.Revision, map[string]any{"title": "x"}, "confirmCount", 1),
		"consent not a token":  dateEditBody(good, base.Revision, map[string]any{"title": "x"}, "consent", "abc"),
		"upper-case consent":   dateEditBody(good, base.Revision, map[string]any{"title": "x"}, "consent", strings.Repeat("A", 32)),
		"consent not a string": dateEditBody(good, base.Revision, map[string]any{"title": "x"}, "consent", 1),
		"null title":           dateEditBody(good, base.Revision, map[string]any{"title": nil}),
		"numeric start":        dateEditBody(good, base.Revision, map[string]any{"startsLocal": 5}),
		"null duration":        dateEditBody(good, base.Revision, map[string]any{"durationMinutes": nil}),
		"changes not a object": {"attempt": good, "revision": base.Revision, "changes": "title"},
	} {
		t.Run(name, func(t *testing.T) {
			wantDateEdit(t, f.admin, f.event.ID, f.date(0), body, http.StatusBadRequest, "date_edit_invalid")
		})
	}
	wantDateEdit(t, f.admin, f.event.ID, f.date(0), dateEditBody(good, base.Revision, map[string]any{"capacityOverride": 0}), http.StatusBadRequest, "invalid_capacity")
	wantDateEdit(t, f.admin, f.event.ID, f.date(0), dateEditBody(good, base.Revision, map[string]any{"visibility": "secret"}), http.StatusBadRequest, "visibility_not_allowed")
	wantDateEdit(t, f.admin, f.event.ID, f.date(0), dateEditBody(good, base.Revision, map[string]any{"visibility": "private"}, "reason", strings.Repeat("x", 501)), http.StatusBadRequest, "")
	if after := f.state(t); after != state {
		t.Fatal("a malformed edit wrote")
	}
	raw := f.admin.do("POST", dateEditPath(f.event.ID, f.date(0)), rawJSON(`{"attempt":"`+good+`","revision":"`+base.Revision+`","changes":{"title":"x"}} trailing`), true)
	if raw.Code != http.StatusBadRequest {
		t.Fatalf("trailing data: %d %s", raw.Code, raw.Body.String())
	}
	// A consent where nothing needs one is not checked.
	wantDateEdit(t, f.admin, f.event.ID, f.date(0), dateEditBody(good, base.Revision, map[string]any{"title": "x"}, "consent", strings.Repeat("f", 32)), http.StatusOK, "")
}

// TestDateEditTighteningPromotesOnTheSavedDate: one save renames a public date,
// gives it a new place and tightens it to the series, which cancels the guest
// going and frees the only seat. The waiter, an invited member who is not yet
// active, is promoted into the date as saved: eligibility judges them signed
// in, the notice judges them signed out, so neither the stored row nor the
// feed once they are active names the new title, the place or the links.
func TestDateEditTighteningPromotesOnTheSavedDate(t *testing.T) {
	f := newDateEditFixture(t)
	target := f.dates[0]
	const video = "https://video.example/VIDEOSECRET"
	gormMust(t, f.app.DB.Model(&models.Event{}).Where("id = ?", f.event.ID).Updates(map[string]any{
		"video_provider": models.VideoProviderLink, "video_url": video, "video_reveal": models.VideoRevealRegisteredOnly}))
	gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", target.ID).Updates(map[string]any{
		"visibility_override": models.VisibilityPublic, "capacity_override": 1}))
	guest := &models.User{Name: "Seat guest", Email: "seat-guest@example.com", Role: models.RoleMember, Status: models.UserGuest}
	gormMust(t, f.app.DB.Create(guest))
	gormMust(t, f.app.DB.Create(&models.Registration{UserID: &guest.ID, OccurrenceID: target.ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
	waiter := newTestUser(t, f.app, "invited waiter", "password1234", models.RoleMember)
	gormMust(t, f.app.DB.Model(waiter).Update("status", models.UserInvited))
	gormMust(t, f.app.DB.Create(&models.Membership{UserID: waiter.ID, GroupID: f.group, Role: models.MembershipMember, Status: models.MembershipActive}))
	since := time.Now().UTC().Add(-time.Hour)
	gormMust(t, f.app.DB.Create(&models.Registration{UserID: &waiter.ID, OccurrenceID: target.ID, Status: models.RegWaitlisted, WaitlistedAt: &since, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
	mark := markNotices(t, f.app)

	base := editBaseOf(t, f.admin, f.event.ID, f.date(0))
	wantDateEdit(t, f.admin, f.event.ID, f.date(0), dateEditBody(newDateAttempt(t), base.Revision,
		map[string]any{"title": "NEWDATESECRET", "location": "NEWPLACESECRET", "visibility": "series"}, "consent", dateEditConsent(t, f.admin, f.event.ID, f.date(0), map[string]any{"title": "NEWDATESECRET", "location": "NEWPLACESECRET", "visibility": "series"})), http.StatusOK, "")
	if s := regStatus(t, f.app, guest.ID, target.ID); s != models.RegCancelled {
		t.Fatalf("fixture: the guest is %s", s)
	}
	if s := regStatus(t, f.app, waiter.ID, target.ID); s != models.RegGoing {
		t.Fatalf("the waiter was not promoted: %s", s)
	}
	drainNotices(t, f.app)
	got := noticesSince(t, f.app, waiter.ID, notify.RegistrationPromoted, mark.notice)
	if len(got) != 1 || got[0].Title != "You're in: an event" || got[0].Body != "" {
		t.Fatalf("the waiter's promotion notices: %+v", got)
	}
	secrets := []string{"NEWDATESECRET", "NEWPLACESECRET", "Supper", "Group hall", "/events/", "/groups/", "/guest/manage/", "VIDEOSECRET"}
	noSecrets(t, f.app, "the invited waiter", waiter.ID, waiter.Email, mark, secrets)
	gormMust(t, f.app.DB.Model(&models.User{}).Where("id = ?", waiter.ID).Update("status", models.UserActive))
	wantWithheld(t, f.app, "the waiter once active", got[0], loginAPI(t, f.app, waiter.Name, "password1234"), target.ID)
	noSecrets(t, f.app, "the waiter once active", waiter.ID, waiter.Email, mark, secrets)
}

// TestDateEditOnePromotionPassInsideTheCutoff: inside the 2-hour cutoff, one
// save that tightens a date (cancelling the guest going) and raises its cap
// tells each runner (but the acting admin) once that a seat opened, and
// promotes nobody.
func TestDateEditOnePromotionPassInsideTheCutoff(t *testing.T) {
	f := newDateEditFixture(t)
	target := f.dates[0]
	gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", target.ID).Updates(map[string]any{
		"visibility_override": models.VisibilityPublic, "capacity_override": 1}))
	guest := &models.User{Name: "Cutoff guest", Email: "cutoff-guest@example.com", Role: models.RoleMember, Status: models.UserGuest}
	gormMust(t, f.app.DB.Create(guest))
	gormMust(t, f.app.DB.Create(&models.Registration{UserID: &guest.ID, OccurrenceID: target.ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
	since := time.Now().UTC().Add(-time.Hour)
	gormMust(t, f.app.DB.Create(&models.Registration{UserID: &f.memberUser.ID, OccurrenceID: target.ID, Status: models.RegWaitlisted, WaitlistedAt: &since, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
	var ev models.Event
	gormMust(t, f.app.DB.First(&ev, f.event.ID))
	runners, err := eventRunnerIDs(f.app.DB, &ev)
	gormMust(t, &gorm.DB{Error: err})
	if len(runners) == 0 {
		t.Fatal("fixture: the event has no runners")
	}
	base := editBaseOf(t, f.admin, f.event.ID, f.date(0))
	newMutableClock(f.app, target.StartsAt.Add(-time.Hour))
	mark := markNotices(t, f.app)

	wantDateEdit(t, f.admin, f.event.ID, f.date(0), dateEditBody(newDateAttempt(t), base.Revision,
		map[string]any{"visibility": "series", "capacityOverride": 2}, "consent", dateEditConsent(t, f.admin, f.event.ID, f.date(0), map[string]any{"visibility": "series", "capacityOverride": 2})), http.StatusOK, "")
	if s := regStatus(t, f.app, guest.ID, target.ID); s != models.RegCancelled {
		t.Fatalf("fixture: the guest is %s", s)
	}
	if s := regStatus(t, f.app, f.memberUser.ID, target.ID); s != models.RegWaitlisted {
		t.Fatalf("the waiter left the line inside the cutoff: %s", s)
	}
	told := 0
	for _, uid := range runners {
		want := 1
		if uid == f.adminUser.ID {
			want = 0 // the actor is not told of their own change
		}
		got := noticesSince(t, f.app, uid, notify.WaitlistCutoff, mark.notice)
		if len(got) != want {
			t.Errorf("runner %d's cutoff notices: %+v", uid, got)
		}
		told += len(got)
	}
	if told == 0 {
		t.Fatal("fixture: no runner but the actor")
	}
}

// TestDateEditPlaceJudgedAcrossAVisibilityChange: someone going on a public
// date of a public group under an exact-date invitation sees the group's
// place. When one save tightens the date to the series, after which they
// reach it only through the invitation (so without its group), and clears
// the inherited place, the place they see changed and they are told. Pinning
// the group's place in such a save leaves what they see unchanged, and they
// are told nothing.
func TestDateEditPlaceJudgedAcrossAVisibilityChange(t *testing.T) {
	f := newDateEditFixture(t)
	gormMust(t, f.app.DB.Model(&models.Group{}).Where("id = ?", f.group).Update("visibility", models.VisibilityPublic))
	cleared, pinned := 1, 2
	grants := map[int]uint{}
	for _, i := range []int{cleared, pinned} {
		gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", f.dates[i].ID).Update("visibility_override", models.VisibilityPublic))
		grants[i] = seedReadInvitation(t, f.app, f.adminUser, f.outsiders[i-1], f.event, f.dates[i]).ID
	}
	before := map[int]int{}
	for _, i := range []int{cleared, pinned} {
		before[i] = len(invitationNotices(t, f.app, f.outsiders[i-1].ID, grants[i]))
	}
	for i, place := range map[int]string{cleared: "", pinned: "Group hall"} {
		base := editBaseOf(t, f.admin, f.event.ID, f.date(i))
		wantDateEdit(t, f.admin, f.event.ID, f.date(i), dateEditBody(newDateAttempt(t), base.Revision,
			map[string]any{"location": place, "visibility": "series"}), http.StatusOK, "")
	}
	if s := regStatus(t, f.app, f.outsiders[0].ID, f.dates[cleared].ID); s != models.RegGoing {
		t.Fatalf("fixture: the invitation was not kept: %s", s)
	}
	got := invitationNotices(t, f.app, f.outsiders[0].ID, grants[cleared])
	if len(got)-before[cleared] != 1 {
		t.Errorf("the invitee who lost the group's place: %+v", got)
	}
	for _, n := range got {
		if strings.Contains(n.Title+n.Body, "Group hall") || strings.Contains(n.Title+n.Body, "Supper") {
			t.Errorf("the invitation's notice names the date: %+v", n)
		}
	}
	if got := invitationNotices(t, f.app, f.outsiders[1].ID, grants[pinned]); len(got) != before[pinned] {
		t.Errorf("the invitee who sees the same place: %+v", got)
	}
	for i, u := range []*models.User{f.outsiders[0], f.outsiders[1]} {
		if got := placeNotices(t, f.app, u.ID, f.dates[i+1].ID); len(got) != 0 {
			t.Errorf("an invitee got the ordinary change notice: %+v", got)
		}
	}
}

// TestDateEditRSVPPromotingAGrantDoesNotWaitOnTheEdit: the guarded edit takes
// its date's live grant rows only after the date. While the edit waits for
// the event row, an ordinary attendee's RSVP holds the date and promotes a
// different holder's waitlisted departure grant: the grant row is free, the
// RSVP answers and promotes before the edit moves on, and then the edit
// applies, with the grant kept.
func TestDateEditRSVPPromotingAGrantDoesNotWaitOnTheEdit(t *testing.T) {
	f := newDepartureReadFixture(t, models.VisibilityPublic, models.RegWaitlisted, policy.OccurrenceOriginDepartureSelf)
	target := f.dates[1]
	date := models.FormatCalendarDate(target.OriginalDate)
	gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", target.ID).Update("capacity_override", 1))
	gormMust(t, f.app.DB.Create(&models.Registration{UserID: &f.other.ID, OccurrenceID: target.ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
	waiting := regOf(t, f.app, f.recipient.ID, target.ID)
	if waiting.Status != models.RegWaitlisted || savedOccurrenceAccess(t, f).Status != policy.OccurrenceAccessActive {
		t.Fatalf("fixture: the retained grant is %s, %+v", waiting.Status, savedOccurrenceAccess(t, f))
	}
	base := editBaseOf(t, f.admin, f.event.ID, date)
	release := holdEventRow(t, f.app, f.event.ID)
	answer := goPostDateEdit(f.admin, f.event.ID, date, dateEditBody(newDateAttempt(t), base.Revision, map[string]any{"title": "Edited title"}))
	waitForLockedUsersQuery(t, f.app, `%FROM "events"%FOR UPDATE%`)

	var free []uint
	gormMust(t, &gorm.DB{Error: f.app.DB.Transaction(func(tx *gorm.DB) error {
		return tx.Raw("SELECT id FROM registrations WHERE id = ? FOR UPDATE SKIP LOCKED", waiting.ID).Scan(&free).Error
	})})
	if len(free) != 1 {
		release()
		awaitResponse(t, answer)
		t.Fatal("the edit held the grant row while it waited for the event")
	}
	rsvpClient := cloneContactClient(f.stranger)
	rsvp := queueContact(func() *httptest.ResponseRecorder {
		return rsvpClient.do("PUT", "/api/v1/events/"+itoa(f.event.ID)+"/registrations", map[string]any{"date": date, "status": models.RegDeclined}, true)
	})
	var w *httptest.ResponseRecorder
	select {
	case w = <-rsvp:
	case <-time.After(8 * time.Second):
		release()
		t.Fatalf("the RSVP waited on the edit; then the RSVP %d, the edit %d", awaitContactResponse(t, rsvp).Code, awaitResponse(t, answer).Code)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("the RSVP: %d %s", w.Code, w.Body.String())
	}
	if s := regStatus(t, f.app, f.recipient.ID, target.ID); s != models.RegGoing {
		t.Fatalf("the RSVP did not promote the grant holder: %s", s)
	}
	release()
	if w := awaitResponse(t, answer); w.Code != http.StatusOK {
		t.Fatalf("the edit: %d %s", w.Code, w.Body.String())
	}
	if o := occurrenceRow(t, f.app, target.ID); o.TitleOverride == nil || *o.TitleOverride != "Edited title" {
		t.Fatalf("the edit did not apply: %+v", o)
	}
	if got := savedOccurrenceAccess(t, f); got.Status != policy.OccurrenceAccessActive {
		t.Fatalf("the grant did not survive: %+v", got)
	}
}

// holdRows keeps the rows query selects locked in another session until the
// returned release runs. The query names its own lock (FOR UPDATE).
func holdRows(t *testing.T, app *App, query string, args ...any) func() {
	t.Helper()
	tx := app.DB.Begin()
	gormMust(t, tx)
	var ids []uint
	gormMust(t, tx.Raw(query, args...).Scan(&ids))
	if len(ids) == 0 {
		tx.Rollback()
		t.Fatalf("holdRows held nothing: %s %v", query, args)
	}
	done := false
	release := func() {
		if !done {
			done = true
			tx.Rollback()
		}
	}
	t.Cleanup(release)
	return release
}

// countDateAttempts counts the attempts of every single-date edit from now on.
func countDateAttempts(app *App) *atomic.Int64 {
	n := &atomic.Int64{}
	app.hooks.singleDateAttempts = n
	return n
}

// dateEditRoutes are the single-date editors, each sending changes for date
// in the background: the guarded POST on a fresh token. The retired PUT
// writes nothing (TestRetiredDatePutRefusesAfterItsChecks).
var dateEditRoutes = []struct {
	name string
	run  func(t *testing.T, f occurrenceReadFixture, date string, changes map[string]any) <-chan *httptest.ResponseRecorder
}{
	{"guarded", func(t *testing.T, f occurrenceReadFixture, date string, changes map[string]any) <-chan *httptest.ResponseRecorder {
		base := editBaseOf(t, f.admin, f.event.ID, date)
		// A consent in changes is the body's own field on this route.
		var extra []any
		if c, ok := changes["consent"]; ok {
			changes = maps.Clone(changes)
			delete(changes, "consent")
			extra = append(extra, "consent", c)
		}
		return goPostDateEdit(f.admin, f.event.ID, date, dateEditBody(newDateAttempt(t), base.Revision, changes, extra...))
	}},
}

const holdRegistrationRow = "SELECT id FROM registrations WHERE id = ? FOR UPDATE"

// noticesAfter is every notice uid holds after mark, oldest first.
func noticesAfter(t *testing.T, app *App, uid, mark uint) []models.Notification {
	t.Helper()
	var rows []models.Notification
	gormMust(t, app.DB.Where("user_id = ? AND id > ?", uid, mark).Order("id").Find(&rows))
	return rows
}

// wantOnlyGrantEnded fails unless the departed person of f was told only that
// their invitation ended, with none of secrets, and cannot read the date.
func wantOnlyGrantEnded(t *testing.T, f occurrenceReadFixture, mark uint, date string, secrets ...string) {
	t.Helper()
	got := noticesAfter(t, f.app, f.recipient.ID, mark)
	if len(got) != 1 || got[0].Kind != notify.OccurrenceUpdated {
		t.Fatalf("the departed person's notices: %+v", got)
	}
	raw, err := json.Marshal(got)
	gormMust(t, &gorm.DB{Error: err})
	for _, secret := range secrets {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("the departed person was told %s: %s", secret, raw)
		}
	}
	if w := f.client.do("GET", dateReadPath(f.event.ID, date), nil, false); w.Code != http.StatusNotFound {
		t.Fatalf("the departed person's read of the date: %d %s", w.Code, w.Body.String())
	}
}

func wantGrantEndedAt(t *testing.T, f occurrenceReadFixture, at time.Time) {
	t.Helper()
	got := savedOccurrenceAccess(t, f)
	if got.Status != policy.OccurrenceAccessEnded || got.EndReason != policy.OccurrenceEnded || got.EndedAt == nil || !got.EndedAt.Equal(at) {
		t.Fatalf("the grant was not ended against the date's end at %v: %+v", at, got)
	}
}

// goingMember adds a group member going on occ.
func goingMember(t *testing.T, f occurrenceReadFixture, occ uint) *models.User {
	t.Helper()
	member := newTestUser(t, f.app, "still a member", "password1234", models.RoleMember)
	gormMust(t, f.app.DB.Create(&models.Membership{UserID: member.ID, GroupID: f.group, Role: models.MembershipMember, Status: models.MembershipActive}))
	gormMust(t, f.app.DB.Create(&models.Registration{UserID: &member.ID, OccurrenceID: occ, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
	return member
}

// TestDateEditDoesNotWaitOnASiblingGrant: a single-date edit locks only its
// own date's grant rows, in both passes. Another session holds a sibling
// date's live grant row, as an RSVP or a household change promoting that
// holder would; the edit of another date answers while the row is held and
// leaves the sibling grant alone.
func TestDateEditDoesNotWaitOnASiblingGrant(t *testing.T) {
	for _, route := range dateEditRoutes {
		t.Run(route.name, func(t *testing.T) {
			f := newDepartureReadFixture(t, models.VisibilityInviteOnly, models.RegGoing, policy.OccurrenceOriginDepartureSelf)
			target := f.dates[2]
			release := holdRows(t, f.app, holdRegistrationRow, f.access.RegistrationID)
			answer := route.run(t, f, models.FormatCalendarDate(target.OriginalDate), map[string]any{"title": "Edited beside a held grant"})
			select {
			case w := <-answer:
				if w.Code != http.StatusOK {
					t.Fatalf("the edit: %d %s", w.Code, w.Body.String())
				}
			case <-time.After(8 * time.Second):
				release()
				t.Fatalf("the edit waited on another date's grant row; after release it answered %d", awaitResponse(t, answer).Code)
			}
			release()
			if o := occurrenceRow(t, f.app, target.ID); o.TitleOverride == nil || *o.TitleOverride != "Edited beside a held grant" {
				t.Fatalf("the edit did not apply: %+v", o)
			}
			if got := savedOccurrenceAccess(t, f); got.Status != policy.OccurrenceAccessActive {
				t.Fatalf("the sibling grant changed: %+v", got)
			}
		})
	}
}

// TestDateEditExpiryWhileAwaitingADateGrantRow: the edit reads its clock only
// once the edited date's grant rows are locked. Another session holds the
// date's retained departure grant; the edit parks on it while holding the
// date, the date ends, and the released edit ends the grant against the old
// end in its first attempt (no rerun hides an early clock) before moving the
// date a month on. The departed person hears only that their invitation
// ended; a member still going is told the change.
func TestDateEditExpiryWhileAwaitingADateGrantRow(t *testing.T) {
	for _, route := range dateEditRoutes {
		t.Run(route.name, func(t *testing.T) {
			f := newDepartureReadFixture(t, models.VisibilityInviteOnly, models.RegGoing, policy.OccurrenceOriginDepartureSelf)
			target := f.dates[1]
			date := models.FormatCalendarDate(target.OriginalDate)
			member := goingMember(t, f, target.ID)
			mark := markNotices(t, f.app).notice
			clock := newMutableClock(f.app, target.EndsAt.Add(-time.Minute))
			attempts := countDateAttempts(f.app)
			release := holdRows(t, f.app, holdRegistrationRow, f.access.RegistrationID)
			moved := target.StartsAt.AddDate(0, 0, 30).Format("2006-01-02T15:04")
			answer := route.run(t, f, date, map[string]any{"startsLocal": moved, "title": "NEWSECRETTITLE", "location": "NEWSECRETPLACE"})
			waitForLockedUsersQuery(t, f.app, `%FROM "registrations"%FOR UPDATE%`)
			after := target.EndsAt.Add(time.Minute)
			clock.set(after)
			release()
			if w := awaitResponse(t, answer); w.Code != http.StatusOK {
				t.Fatalf("the edit: %d %s", w.Code, w.Body.String())
			}
			if n := attempts.Load(); n != 1 {
				t.Fatalf("the edit ran %d times; its first attempt judged the grant on a clock read before the row lock", n)
			}
			if o := occurrenceRow(t, f.app, target.ID); !o.EndsAt.After(after.AddDate(0, 0, 29)) {
				t.Fatalf("fixture: the date did not move: %+v", o)
			}
			wantGrantEndedAt(t, f, after)
			wantOnlyGrantEnded(t, f, mark, date, "NEWSECRETTITLE", "NEWSECRETPLACE")
			if got := noticesSince(t, f.app, member.ID, notify.EventChanged, mark); len(got) != 1 {
				t.Fatalf("the member still going was told %d changes: %+v", len(got), got)
			}
		})
	}
}

// TestDateEditMoveLaterOfAKeptDateIsRefusedBeforeTheEnd: a move later of a date
// someone kept after leaving, a minute before its old end, is refused before
// anything is written (DEC-156), so the old end passing later in the save can
// no longer revive the grant: one attempt, the "effects" step never reached,
// and the date, the grant and the notices as they were.
func TestDateEditMoveLaterOfAKeptDateIsRefusedBeforeTheEnd(t *testing.T) {
	f := newDepartureReadFixture(t, models.VisibilityInviteOnly, models.RegGoing, policy.OccurrenceOriginDepartureSelf)
	target := f.dates[1]
	date := models.FormatCalendarDate(target.OriginalDate)
	goingMember(t, f, target.ID)
	mark := markNotices(t, f.app).notice
	clock := newMutableClock(f.app, target.EndsAt.Add(-time.Minute))
	attempts := countDateAttempts(f.app)
	var effects atomic.Int64
	f.app.hooks.dateEditStep = func(_ *gorm.DB, step string) error {
		if step == "effects" && effects.Add(1) == 1 {
			clock.set(target.EndsAt.Add(time.Minute))
		}
		return nil
	}
	base := editBaseOf(t, f.admin, f.event.ID, date)
	state := eventWriterState(t, f.app, f.event.ID)
	moved := target.StartsAt.AddDate(0, 0, 30).Format("2006-01-02T15:04")
	w := wantDateEdit(t, f.admin, f.event.ID, date, dateEditBody(newDateAttempt(t), base.Revision,
		map[string]any{"startsLocal": moved, "title": "NEWSECRETTITLE", "location": "NEWSECRETPLACE"}), http.StatusConflict, "date_kept_after_leaving")
	wantKeptDates(t, w, keptDateCountJSON{Date: date, KeptCount: 1})
	if attempts.Load() != 1 || effects.Load() != 0 {
		t.Fatalf("attempts %d, effects reached %d; want one refused attempt", attempts.Load(), effects.Load())
	}
	if after := eventWriterState(t, f.app, f.event.ID); after != state {
		t.Fatalf("the refused edit wrote:\n%s\n%s", state, after)
	}
	if got := savedOccurrenceAccess(t, f); got.Status != policy.OccurrenceAccessActive {
		t.Fatalf("the refused edit ended the grant: %+v", got)
	}
	if got := noticesAfter(t, f.app, f.recipient.ID, mark); len(got) != 0 {
		t.Fatalf("the refused edit told the departed person: %+v", got)
	}
}

// TestDateEditEndPassingInTheWrapperRerunsTheEdit: the edit's last waits come
// after its callback, in the wrapper's request reconciliation. An expired add
// request on the event, between two people with nothing on the edited date,
// is finished there, and its notice waits on a recipient whose row a session
// with no contact gate holds (a push device save takes users FOR UPDATE). The
// date's end passes during that wait. The fence after it rolls the first
// attempt back, and the rerun ends the grant before the new title and place
// are saved, so the departed person is never told them.
func TestDateEditEndPassingInTheWrapperRerunsTheEdit(t *testing.T) {
	f := newDepartureReadFixture(t, models.VisibilityInviteOnly, models.RegGoing, policy.OccurrenceOriginDepartureSelf)
	target := f.dates[1]
	date := models.FormatCalendarDate(target.OriginalDate)
	start := target.EndsAt.Add(-time.Minute)
	clock := newMutableClock(f.app, start)
	requester := newTestUser(t, f.app, "asking leader", "password1234", models.RoleMember)
	subject := newTestUser(t, f.app, "asked person", "password1234", models.RoleMember)
	gormMust(t, f.app.DB.Create(&models.ContactRequest{Purpose: policy.RequestOccurrenceAdd, RequesterID: requester.ID, SubjectUserID: subject.ID,
		ContextType: policy.ContactEvent, ContextID: f.event.ID, OccurrenceID: &f.dates[0].ID, AddAttendanceMode: models.AttendanceInPerson,
		SubjectDecision: policy.ContactPending, AdminDecision: policy.ContactNotNeeded, Status: policy.ContactPending,
		CreatedAt: start.Add(-2 * time.Hour), ExpiresAt: start.Add(-time.Hour)}))
	mark := markNotices(t, f.app).notice
	attempts := countDateAttempts(f.app)
	var receipt atomic.Bool
	f.app.hooks.dateEditStep = func(_ *gorm.DB, step string) error {
		if step == "receipt" {
			receipt.Store(true)
		}
		return nil
	}
	release := holdRows(t, f.app, "SELECT id FROM users WHERE id = ? FOR UPDATE", requester.ID)
	base := editBaseOf(t, f.admin, f.event.ID, date)
	// The edit keeps the date's end: a later end on this kept date is refused
	// outright (DEC-156), so the rerun guards a change of title and place.
	answer := goPostDateEdit(f.admin, f.event.ID, date, dateEditBody(newDateAttempt(t), base.Revision,
		map[string]any{"title": "NEWSECRETTITLE", "location": "NEWSECRETPLACE"}))
	waitForLockedUsersQuery(t, f.app, `%FROM users%FOR KEY SHARE%`)
	if !receipt.Load() {
		release()
		t.Fatal("the edit waited on the recipient before its callback finished")
	}
	after := target.EndsAt.Add(time.Minute)
	clock.set(after)
	release()
	if w := awaitResponse(t, answer); w.Code != http.StatusOK {
		t.Fatalf("the edit: %d %s", w.Code, w.Body.String())
	}
	if n := attempts.Load(); n != 2 {
		t.Fatalf("the edit ran %d times, want a rerun after the wrapper's wait", n)
	}
	if o := occurrenceRow(t, f.app, target.ID); o.TitleOverride == nil || *o.TitleOverride != "NEWSECRETTITLE" || !o.EndsAt.Equal(target.EndsAt) {
		t.Fatalf("fixture: the edit did not apply: %+v", o)
	}
	wantGrantEndedAt(t, f, after)
	wantOnlyGrantEnded(t, f, mark, date, "NEWSECRETTITLE", "NEWSECRETPLACE")
	var finished models.ContactRequest
	gormMust(t, f.app.DB.Where("requester_id = ?", requester.ID).First(&finished))
	if finished.Status != policy.ContactExpired {
		t.Fatalf("the rerun did not finish the expired request: %s", finished.Status)
	}
}

// TestDateEditSecondEndCrossingAnswersDateChanged: the old end passes during
// the first attempt and the new end, two minutes later, during the rerun. The
// edit answers date_changed and writes nothing: the date, the event's other
// grant, the notices and the receipt are as they were.
func TestDateEditSecondEndCrossingAnswersDateChanged(t *testing.T) {
	f := newDepartureReadFixture(t, models.VisibilityInviteOnly, models.RegGoing, policy.OccurrenceOriginDepartureSelf)
	// A later end needs a date nobody kept after leaving (DEC-156): the
	// fixture's grant is on the next date, live and untouched throughout.
	target := f.dates[0]
	date := models.FormatCalendarDate(target.OriginalDate)
	mark := markNotices(t, f.app).notice
	clock := newMutableClock(f.app, target.EndsAt.Add(-time.Minute))
	attempts := countDateAttempts(f.app)
	var effects atomic.Int64
	f.app.hooks.dateEditStep = func(_ *gorm.DB, step string) error {
		if step != "effects" {
			return nil
		}
		switch effects.Add(1) {
		case 1:
			clock.set(target.EndsAt.Add(30 * time.Second))
		case 2:
			clock.set(target.EndsAt.Add(3 * time.Minute))
		}
		return nil
	}
	base := editBaseOf(t, f.admin, f.event.ID, date)
	state := eventWriterState(t, f.app, f.event.ID)
	minutes := int(target.EndsAt.Sub(target.StartsAt)/time.Minute) + 2
	w := wantDateEdit(t, f.admin, f.event.ID, date, dateEditBody(newDateAttempt(t), base.Revision, map[string]any{"durationMinutes": minutes}), http.StatusConflict, "date_changed")
	// The base was the date's own: an end crossing proves nothing about it.
	wantSuperseded(t, "a second end crossing", w, false)
	if attempts.Load() != 2 {
		t.Fatalf("attempts %d, want 2", attempts.Load())
	}
	if after := eventWriterState(t, f.app, f.event.ID); after != state {
		t.Fatalf("the refused edit wrote:\n%s\n%s", state, after)
	}
	if o := occurrenceRow(t, f.app, target.ID); !o.EndsAt.Equal(target.EndsAt) || o.EditReceipt != nil {
		t.Fatalf("the refused edit changed the date: %+v", o)
	}
	if got := savedOccurrenceAccess(t, f); got.Status != policy.OccurrenceAccessActive {
		t.Fatalf("the refused edit ended the grant: %+v", got)
	}
	if got := noticesAfter(t, f.app, f.recipient.ID, mark); len(got) != 0 {
		t.Fatalf("the refused edit told the departed person: %+v", got)
	}
}

// TestDateEditSavedEndIsJudgedBeforeTheChangeNotices: a shortening whose new
// end has passed while the old end has not ends the grant against the date as
// saved before anyone is told of the change. The departed person gets one end
// notice and no invitation change notice, and cannot read the date: when the
// new end has passed from the start, and when it passes during the first
// attempt and the rerun keeps the grant
// against the old end in its before pass.
func TestDateEditSavedEndIsJudgedBeforeTheChangeNotices(t *testing.T) {
	cases := []struct {
		name, route string
		rerun       bool
	}{{"guarded, already past", "guarded", false}, {"guarded, passing in the first attempt", "guarded", true}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDepartureReadFixture(t, models.VisibilityInviteOnly, models.RegGoing, policy.OccurrenceOriginDepartureSelf)
			target := f.dates[1]
			date := models.FormatCalendarDate(target.OriginalDate)
			goingMember(t, f, target.ID)
			mark := markNotices(t, f.app).notice
			newEnd := target.EndsAt.Add(-20 * time.Minute)
			minutes := int(newEnd.Sub(target.StartsAt) / time.Minute)
			judged := newEnd.Add(time.Minute)
			if tc.rerun {
				judged = newEnd.Add(-time.Minute)
			}
			clock := newMutableClock(f.app, judged)
			attempts := countDateAttempts(f.app)
			var effects atomic.Int64
			f.app.hooks.dateEditStep = func(_ *gorm.DB, step string) error {
				if tc.rerun && step == "effects" && effects.Add(1) == 1 {
					clock.set(newEnd.Add(time.Minute))
				}
				return nil
			}
			for _, route := range dateEditRoutes {
				if route.name != tc.route {
					continue
				}
				if w := awaitResponse(t, route.run(t, f, date, map[string]any{"durationMinutes": minutes})); w.Code != http.StatusOK {
					t.Fatalf("the edit: %d %s", w.Code, w.Body.String())
				}
			}
			want := int64(1)
			if tc.rerun {
				want = 2
			}
			if attempts.Load() != want {
				t.Fatalf("attempts %d, want %d", attempts.Load(), want)
			}
			if o := occurrenceRow(t, f.app, target.ID); !o.EndsAt.Equal(newEnd) {
				t.Fatalf("fixture: the date was not shortened: %+v", o)
			}
			if got := savedOccurrenceAccess(t, f); got.Status != policy.OccurrenceAccessEnded || got.EndReason != policy.OccurrenceEnded {
				t.Fatalf("the grant outlived the saved end: %+v", got)
			}
			if got := noticesSince(t, f.app, f.recipient.ID, notify.OccurrenceChanged, mark); len(got) != 0 {
				t.Fatalf("the change was sent through an ended grant: %+v", got)
			}
			wantOnlyGrantEnded(t, f, mark, date)
		})
	}
}

// singleDateScopeProblems: a contactChange's SingleDate is set only inside
// singleDateMutation (api_date_edits.go), exactly once. The wrapper skips its
// event-wide access passes for a SingleDate change, and the M8.1 brief's
// cancellation proof relies on every canceller keeping them, so no other
// writer may opt out.
func singleDateScopeProblems(sources map[string]string) []string {
	parsed := guardkit.Sources(sources).Parse(0)
	bad := append([]string(nil), parsed.Problems...)
	home := 0
	for _, pf := range parsed.Files {
		guardkit.InspectStack(pf.AST, func(n ast.Node, stack []ast.Node) bool {
			inHome := false
			for _, s := range stack {
				if fd, ok := s.(*ast.FuncDecl); ok && pf.Path == "api_date_edits.go" && fd.Recv != nil && fd.Name.Name == "singleDateMutation" {
					inHome = true
				}
			}
			pos := parsed.Fset.Position(n.Pos())
			switch x := n.(type) {
			case *ast.KeyValueExpr:
				if k, ok := x.Key.(*ast.Ident); ok && k.Name == "SingleDate" && fenceInContactChange(stack) {
					if inHome {
						home++
					} else {
						bad = append(bad, fmt.Sprintf("%s:%d: a contactChange literal sets SingleDate outside singleDateMutation", pf.Path, pos.Line))
					}
				}
			case *ast.SelectorExpr:
				if x.Sel.Name == "SingleDate" && fenceAssigned(stack) && !inHome {
					bad = append(bad, fmt.Sprintf("%s:%d: SingleDate is assigned outside singleDateMutation", pf.Path, pos.Line))
				}
			}
			return true
		})
	}
	if home != 1 {
		bad = append(bad, fmt.Sprintf("singleDateMutation sets SingleDate %d times, want once", home))
	}
	return bad
}

func TestSingleDateScopeIsSetOnlyBySingleDateMutation(t *testing.T) {
	for _, p := range singleDateScopeProblems(guardkit.Production(t)) {
		t.Error(p)
	}
	home := "package main\nfunc (a *App) singleDateMutation() { _ = contactChange{EventID: 1, SingleDate: nil} }\n"
	for name, extra := range map[string]string{
		"a literal elsewhere":     "package main\nfunc (a *App) cancelDate() { _ = contactChange{EventID: 1, SingleDate: &singleDate{}} }\n",
		"an assignment elsewhere": "package main\nfunc (a *App) cancelDate() { c := contactChange{EventID: 1}; c.SingleDate = &singleDate{} }\n",
		"a literal in a slice":    "package main\nvar changes = []contactChange{{SingleDate: &singleDate{}}}\n",
		"a pointer literal":       "package main\nfunc f() { _ = &contactChange{SingleDate: &singleDate{}} }\n",
	} {
		if len(singleDateScopeProblems(map[string]string{"api_date_edits.go": home, "other.go": extra})) == 0 {
			t.Errorf("the guard missed %s", name)
		}
	}
	if bad := singleDateScopeProblems(map[string]string{"api_date_edits.go": home}); len(bad) != 0 {
		t.Errorf("the guard refused its own home: %v", bad)
	}
}

// TestDateEditPromotionJudgesTheCutoffAfterTheWaits: the guarded date edit
// runs one promotion pass on the saved date, on a clock read when it runs.
// Date 0 is open to the public with two seats, taken by a member and an
// outsider, and a member waits in line. The save changes the place, so its
// change notice waits on the going member's users row, which a session holds
// as a push device save does. The clock moves while it waits. A seat freed by
// tightening (the outsider cancelled), by a raised cap, or both, is offered
// only before starts_at-2h: landing exactly on the cutoff keeps the waiter in
// line and tells each runner but the actor once; one microsecond before
// promotes them once.
func TestDateEditPromotionJudgesTheCutoffAfterTheWaits(t *testing.T) {
	kinds := []struct {
		name    string
		changes map[string]any
		losers  int
	}{
		{"visibility", map[string]any{"location": "A bigger hall", "visibility": "series"}, 1},
		{"capacity", map[string]any{"location": "A bigger hall", "capacityOverride": 3}, 0},
		{"both", map[string]any{"location": "A bigger hall", "visibility": "series", "capacityOverride": 3}, 1},
	}
	routes := []struct {
		name string
		run  func(t *testing.T, f dateEditFixture, changes map[string]any, losers int) <-chan *httptest.ResponseRecorder
	}{
		{"guarded", func(t *testing.T, f dateEditFixture, changes map[string]any, losers int) <-chan *httptest.ResponseRecorder {
			base := editBaseOf(t, f.admin, f.event.ID, f.date(0))
			var extra []any
			if losers > 0 {
				extra = []any{"consent", dateEditConsent(t, f.admin, f.event.ID, f.date(0), changes)}
			}
			return goPostDateEdit(f.admin, f.event.ID, f.date(0), dateEditBody(newDateAttempt(t), base.Revision, changes, extra...))
		}},
	}
	for _, route := range routes {
		for _, kind := range kinds {
			for _, inside := range []bool{true, false} {
				name := route.name + "/" + kind.name + "/at the cutoff"
				if !inside {
					name = route.name + "/" + kind.name + "/just before it"
				}
				t.Run(name, func(t *testing.T) {
					f := newDateEditFixture(t)
					target := f.dates[0]
					gormMust(t, f.app.DB.Model(&models.Event{}).Where("id = ?", f.event.ID).Update("waitlist_enabled", true))
					gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", target.ID).Updates(map[string]any{
						"visibility_override": models.VisibilityPublic, "capacity_override": 2}))
					for _, uid := range []uint{f.memberUser.ID, f.outsiders[0].ID} {
						gormMust(t, f.app.DB.Create(&models.Registration{UserID: &uid, OccurrenceID: target.ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
					}
					waiter := newTestUser(t, f.app, "waiting member", "password1234", models.RoleMember)
					gormMust(t, f.app.DB.Create(&models.Membership{UserID: waiter.ID, GroupID: f.group, Role: models.MembershipMember, Status: models.MembershipActive}))
					since := time.Now().UTC().Add(-time.Hour)
					gormMust(t, f.app.DB.Create(&models.Registration{UserID: &waiter.ID, OccurrenceID: target.ID, Status: models.RegWaitlisted, WaitlistedAt: &since, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
					var ev models.Event
					gormMust(t, f.app.DB.First(&ev, f.event.ID))
					runners, err := eventRunnerIDs(f.app.DB, &ev)
					gormMust(t, &gorm.DB{Error: err})
					mark := markNotices(t, f.app).notice
					cutoff := target.StartsAt.Add(-events.CutoffWindow)
					clock := newMutableClock(f.app, cutoff.Add(-time.Minute))
					attempts := countDateAttempts(f.app)
					release := holdRows(t, f.app, "SELECT id FROM users WHERE id = ? FOR UPDATE", f.memberUser.ID)
					answer := route.run(t, f, kind.changes, kind.losers)
					waitForLockedUsersQuery(t, f.app, `%FROM users%FOR KEY SHARE%`)
					if inside {
						clock.set(cutoff)
					} else {
						clock.set(cutoff.Add(-time.Microsecond))
					}
					release()
					if w := awaitResponse(t, answer); w.Code != http.StatusOK {
						t.Fatalf("the edit: %d %s", w.Code, w.Body.String())
					}
					if n := attempts.Load(); n != 1 {
						t.Fatalf("%d attempts, want 1", n)
					}
					wantOutsider := models.RegGoing
					if kind.losers > 0 {
						wantOutsider = models.RegCancelled
					}
					if s := regStatus(t, f.app, f.outsiders[0].ID, target.ID); s != wantOutsider {
						t.Fatalf("fixture: the outsider is %s", s)
					}
					told := 0
					for _, uid := range runners {
						got := noticesSince(t, f.app, uid, notify.WaitlistCutoff, mark)
						want := 0
						if inside && uid != f.adminUser.ID {
							want = 1
						}
						if len(got) != want {
							t.Errorf("runner %d's cutoff notices: %+v", uid, got)
						}
						told += len(got)
					}
					promoted := noticesSince(t, f.app, waiter.ID, notify.RegistrationPromoted, mark)
					if inside {
						if s := regStatus(t, f.app, waiter.ID, target.ID); s != models.RegWaitlisted {
							t.Fatalf("the waiter was promoted inside the cutoff: %s", s)
						}
						if told == 0 || len(promoted) != 0 {
							t.Fatalf("runners told %d times; the waiter's promotion notices: %+v", told, promoted)
						}
						return
					}
					if s := regStatus(t, f.app, waiter.ID, target.ID); s != models.RegGoing {
						t.Fatalf("the waiter was not promoted before the cutoff: %s", s)
					}
					if len(promoted) != 1 {
						t.Fatalf("the waiter's promotion notices: %+v", promoted)
					}
				})
			}
		}
	}
}

// TestDateEditRetryHonorsRestoreDetectedUnderTheGate: the guarded edit's
// first attempt rolls back because the date's end passed during it. Before
// the rerun takes the contact gate, a fenced writer holding the gate detects
// a restore and marks this process, then releases the gate. The rerun, once
// it holds the gate, answers 503 restore_detected and writes nothing: the
// date, its receipt, the departed person's grant, notices and audit stay as
// they were.
func TestDateEditRetryHonorsRestoreDetectedUnderTheGate(t *testing.T) {
	f := newDepartureReadFixture(t, models.VisibilityInviteOnly, models.RegGoing, policy.OccurrenceOriginDepartureSelf)
	target := f.dates[1]
	date := models.FormatCalendarDate(target.OriginalDate)
	goingMember(t, f, target.ID)
	base := editBaseOf(t, f.admin, f.event.ID, date)
	markNotices(t, f.app)
	state := eventWriterState(t, f.app, f.event.ID)
	grant := savedOccurrenceAccess(t, f)
	clock := newMutableClock(f.app, target.EndsAt.Add(-time.Minute))
	attempts := countDateAttempts(f.app)
	var effects atomic.Int64
	f.app.hooks.dateEditStep = func(_ *gorm.DB, step string) error {
		if step == "effects" && effects.Add(1) == 1 {
			clock.set(target.EndsAt.Add(time.Minute))
		}
		return nil
	}
	detector := make(chan error, 1)
	f.app.hooks.singleDateBeforeRetry = func() {
		tx := f.app.DB.Begin(contactGateTx)
		if err := lockContacts(tx); err != nil {
			tx.Rollback()
			detector <- err
			return
		}
		go func() {
			defer tx.Rollback()
			deadline := time.Now().Add(10 * time.Second)
			for {
				var n int64
				if err := f.app.DB.Raw("SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted AND " + advisoryInThisDatabase).Scan(&n).Error; err != nil {
					detector <- err
					return
				}
				if n > 0 {
					break
				}
				if time.Now().After(deadline) {
					detector <- errors.New("the rerun never waited on the contact gate")
					return
				}
				time.Sleep(5 * time.Millisecond)
			}
			f.app.fence.detected.Store(true)
			detector <- nil
		}()
	}
	// A title change keeps the date's end, so the kept grant does not refuse
	// it (DEC-156) and the old end passing mid-edit still forces the rerun.
	wantDateEdit(t, f.admin, f.event.ID, date, dateEditBody(newDateAttempt(t), base.Revision,
		map[string]any{"title": "NEWSECRETTITLE"}), http.StatusServiceUnavailable, "restore_detected")
	select {
	case err := <-detector:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the detector never ran")
	}
	if n := attempts.Load(); n != 2 || effects.Load() != 1 {
		t.Fatalf("attempts %d, effects reached %d; want a refused rerun", n, effects.Load())
	}
	if after := eventWriterState(t, f.app, f.event.ID); after != state {
		t.Fatal("the refused rerun wrote")
	}
	if got := savedOccurrenceAccess(t, f); got.Status != grant.Status || got.EndedAt != nil {
		t.Fatalf("the grant changed: %+v, was %+v", got, grant)
	}
	if o := occurrenceRow(t, f.app, target.ID); o.EditReceipt != nil || !o.EndsAt.Equal(target.EndsAt) {
		t.Fatalf("the date changed: %+v", o)
	}
}

// TestDateConsentCountsTheProspectiveDate: the date preview and the guarded
// POST count a tightening on the date as the same save would
// store it. (i) An ended public date moved three days ahead loses both
// outsiders; (ii) a future date moved into the past loses nobody; (iii) a date
// tightened in place loses both outsiders and keeps a live exact-date
// invitation. Without consent nothing is written; with it, exactly the
// counted rows are cancelled.
func TestDateConsentCountsTheProspectiveDate(t *testing.T) {
	wall := func(at time.Time) string { return at.UTC().Format("2006-01-02T15:04") }
	cases := []struct {
		name  string
		setup func(t *testing.T, f dateEditFixture) map[string]any
		want  int
	}{
		{"an ended date moved ahead", func(t *testing.T, f dateEditFixture) map[string]any {
			past := time.Now().UTC().Add(-3 * time.Hour)
			gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", f.dates[1].ID).Updates(map[string]any{"starts_at": past, "ends_at": past.Add(time.Hour)}))
			return map[string]any{"startsLocal": wall(time.Now().AddDate(0, 0, 3))}
		}, 2},
		{"a future date moved into the past", func(t *testing.T, f dateEditFixture) map[string]any {
			return map[string]any{"startsLocal": wall(time.Now().Add(-2 * time.Hour))}
		}, 0},
		{"a date tightened in place", func(t *testing.T, f dateEditFixture) map[string]any {
			invited := newTestUser(t, f.app, "invited outsider", "password1234", models.RoleMember)
			seedReadInvitation(t, f.app, f.adminUser, invited, f.event, f.dates[1])
			return map[string]any{}
		}, 2},
	}
	// count sums the registrants of an answer's impact: the preview's body,
	// or a writer question's nested impact.
	count := func(t *testing.T, w *httptest.ResponseRecorder, nested bool) int {
		t.Helper()
		var answer struct {
			visibilityImpactJSON
			Impact visibilityImpactJSON `json:"impact"`
		}
		gormMust(t, &gorm.DB{Error: json.Unmarshal(w.Body.Bytes(), &answer)})
		impact := answer.visibilityImpactJSON
		if nested {
			impact = answer.Impact
		}
		n := 0
		for _, row := range impact.Tightened {
			n += int(row.Registrants)
		}
		return n
	}
	for _, tc := range cases {
		for _, writer := range []string{"preview", "guarded POST"} {
			t.Run(tc.name+"/"+writer, func(t *testing.T) {
				f := newDateEditFixture(t)
				regs := f.openWithOutsiders(t, 1)
				changes := tc.setup(t, f)
				date := f.date(1)
				cancelled := func() int {
					n := 0
					for _, r := range regs {
						if regStatus(t, f.app, *r.UserID, r.OccurrenceID) == models.RegCancelled {
							n++
						}
					}
					return n
				}
				wantUntouched := func(before string) {
					t.Helper()
					if after := f.state(t); after != before {
						t.Fatalf("asking for consent wrote:\nbefore %s\nafter  %s", before, after)
					}
				}
				switch writer {
				case "preview":
					q := "?visibility=series"
					if s, ok := changes["startsLocal"].(string); ok {
						q += "&startsLocal=" + s
					}
					before := f.state(t)
					w := f.admin.do("GET", managedDatePath(f.event.ID, date)+"/visibility-impact"+q, nil, false)
					if w.Code != http.StatusOK {
						t.Fatalf("preview: %d %s", w.Code, w.Body.String())
					}
					if got := count(t, w, false); got != tc.want {
						t.Fatalf("the preview counts %d, want %d: %s", got, tc.want, w.Body.String())
					}
					wantUntouched(before)
				case "guarded POST":
					extra := []any{}
					if tc.want > 0 {
						before := f.state(t)
						base := editBaseOf(t, f.admin, f.event.ID, date)
						w := wantDateEdit(t, f.admin, f.event.ID, date, dateEditBody(newDateAttempt(t), base.Revision, withVisibility(changes)), http.StatusConflict, "date_needs_consent")
						if got := count(t, w, true); got != tc.want {
							t.Fatalf("the POST counts %d, want %d: %s", got, tc.want, w.Body.String())
						}
						wantUntouched(before)
						extra = append(extra, "consent", consentOf(t, w))
					}
					base := editBaseOf(t, f.admin, f.event.ID, date)
					wantDateEdit(t, f.admin, f.event.ID, date, dateEditBody(newDateAttempt(t), base.Revision, withVisibility(changes), extra...), http.StatusOK, "")
				}
				wantCancelled := tc.want
				if writer == "preview" {
					wantCancelled = 0
				}
				if got := cancelled(); got != wantCancelled {
					t.Fatalf("%d outsiders cancelled, want %d", got, wantCancelled)
				}
				if got := auditCount(t, f.app, "registration.cancelled"); got != int64(wantCancelled) {
					t.Fatalf("%d cancellation audit rows, want %d", got, wantCancelled)
				}
			})
		}
	}
}

func withVisibility(changes map[string]any) map[string]any {
	out := map[string]any{"visibility": "series"}
	for k, v := range changes {
		out[k] = v
	}
	return out
}

func TestDateVisibilityPreviewReadsAuthorityAndDateInOneSnapshot(t *testing.T) {
	f := newDateEditFixture(t)
	f.openWithOutsiders(t, 1)
	manager := newTestUser(t, f.app, "preview leader", "password1234", models.RoleMember)
	gormMust(t, f.app.DB.Create(&models.Membership{UserID: manager.ID, GroupID: f.group, Role: models.MembershipLeader, Status: models.MembershipActive}))
	client := loginAPI(t, f.app, manager.Name, "password1234")
	fired := atReaderSeam(f.app, "authorized", "date visibility preview", func() {
		if err := f.app.DB.Transaction(func(tx *gorm.DB) error {
			if err := tx.Model(&models.Membership{}).Where("user_id = ? AND group_id = ?", manager.ID, f.group).Update("status", models.MembershipRemoved).Error; err != nil {
				return err
			}
			past := time.Now().UTC().Add(-3 * time.Hour)
			return tx.Model(&models.EventOccurrence{}).Where("id = ?", f.dates[1].ID).Updates(map[string]any{"starts_at": past, "ends_at": past.Add(time.Hour)}).Error
		}); err != nil {
			t.Error(err)
		}
	})
	path := managedDatePath(f.event.ID, f.date(1)) + "/visibility-impact?visibility=series"
	w := client.do("GET", path, nil, false)
	if !fired.Load() || w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"registrants":2`) {
		t.Fatalf("preview mixed authority and date states: fired=%v, %d %s", fired.Load(), w.Code, w.Body.String())
	}
	if w = client.do("GET", path, nil, false); w.Code == http.StatusOK {
		t.Fatalf("removed leader retained preview access: %d %s", w.Code, w.Body.String())
	}
}

func TestDateVisibilityPreviewUsesInjectedClock(t *testing.T) {
	f := newDateEditFixture(t)
	f.openWithOutsiders(t, 1)
	f.app.clock = func() time.Time { return time.Now().UTC().AddDate(0, 0, 100) }
	w := f.admin.do("GET", managedDatePath(f.event.ID, f.date(1))+"/visibility-impact?visibility=series", nil, false)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"registrants":0`) {
		t.Fatalf("preview did not use its injected clock: %d %s", w.Code, w.Body.String())
	}
}

func TestDateVisibilityPreviewCountsLosersOnOneConnection(t *testing.T) {
	t.Run("losers", func(t *testing.T) {
		f := newDateEditFixture(t)
		f.openWithOutsiders(t, 1)
		invited := newTestUser(t, f.app, "preview invited", "password1234", models.RoleMember)
		seedReadInvitation(t, f.app, f.adminUser, invited, f.event, f.dates[1])
		one := oneConnectionPool(t, f.app)
		f.app.DB = one
		oneConnection(t, f.app, one)
		w := f.admin.do("GET", managedDatePath(f.event.ID, f.date(1))+"/visibility-impact?visibility=series", nil, false)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"registrants":2`) {
			t.Fatalf("preview did not count losers on its snapshot connection: %d %s", w.Code, w.Body.String())
		}
	})
	// A later end on a kept date counts the kept grants on the same connection.
	t.Run("kept later end", func(t *testing.T) {
		f, path := keptPreviewFixture(t)
		one := oneConnectionPool(t, f.app)
		f.app.DB = one
		oneConnection(t, f.app, one)
		date := models.FormatCalendarDate(f.dates[1].OriginalDate)
		wantKeptDates(t, f.admin.do("GET", path+"&startsLocal="+wallAt(f.dates[1].StartsAt.Add(time.Hour)), nil, false), keptDateCountJSON{Date: date, KeptCount: 1})
	})
}

// TestDatePreviewValidatesItsTimesAsTheEditDoes: the date preview refuses the
// start and duration the guarded date edit refuses, with the same answers.
func TestDatePreviewValidatesItsTimesAsTheEditDoes(t *testing.T) {
	f := newDateEditFixture(t)
	path := managedDatePath(f.event.ID, f.date(1))
	for _, tc := range []struct {
		query string
		body  map[string]any
		code  string
	}{
		{"&durationMinutes=0", map[string]any{"durationMinutes": 0}, "invalid_duration"},
		{"&durationMinutes=x", nil, "invalid_duration"},
		{"&startsLocal=2026-02-30T19:00", map[string]any{"startsLocal": "2026-02-30T19:00"}, "invalid_start"},
	} {
		w := f.admin.do("GET", path+"/visibility-impact?visibility=series"+tc.query, nil, false)
		if w.Code != http.StatusBadRequest || answerCode(t, w) != tc.code {
			t.Fatalf("preview %s: %d %s, want 400 %s", tc.query, w.Code, w.Body.String(), tc.code)
		}
		if tc.body == nil {
			continue
		}
		tc.body["visibility"] = "series"
		saveDate(t, f.admin, f.event.ID, f.date(1), tc.body, http.StatusBadRequest, tc.code)
	}
}

// wallAt is at as a UTC wall clock, the form the date editors take (the
// fixtures' events are in UTC).
func wallAt(at time.Time) string { return at.UTC().Format("2006-01-02T15:04") }

// wantKeptDates fails unless w is 409 date_kept_after_leaving with exactly the
// keys error, code and keptDates, each refused date carrying exactly date and
// keptCount, equal to want in order.
func wantKeptDates(t *testing.T, w *httptest.ResponseRecorder, want ...keptDateCountJSON) {
	t.Helper()
	if w.Code != http.StatusConflict {
		t.Fatalf("want 409 date_kept_after_leaving, got %d %s", w.Code, w.Body.String())
	}
	var top map[string]json.RawMessage
	gormMust(t, &gorm.DB{Error: json.Unmarshal(w.Body.Bytes(), &top)})
	keys := func(m map[string]json.RawMessage) string {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		slices.Sort(out)
		return strings.Join(out, " ")
	}
	if got := keys(top); got != "code error keptDates" || string(top["code"]) != `"date_kept_after_leaving"` {
		t.Fatalf("the refusal's keys %q: %s", got, w.Body.String())
	}
	var rows []map[string]json.RawMessage
	gormMust(t, &gorm.DB{Error: json.Unmarshal(top["keptDates"], &rows)})
	for _, row := range rows {
		if got := keys(row); got != "date keptCount" {
			t.Fatalf("a refused date's keys %q: %s", got, w.Body.String())
		}
	}
	var got []keptDateCountJSON
	gormMust(t, &gorm.DB{Error: json.Unmarshal(top["keptDates"], &got)})
	if !slices.Equal(got, want) {
		t.Fatalf("refused dates %+v, want %+v", got, want)
	}
}

// noticeTotal is how many notifications there are after mark.
func noticeTotal(t *testing.T, app *App, mark uint) int64 {
	t.Helper()
	var n int64
	gormMust(t, app.DB.Model(&models.Notification{}).Where("id > ?", mark).Count(&n))
	return n
}

// removeKept is the manager's roster Remove of the fixture's kept row.
func removeKept(f occurrenceReadFixture, date string) *httptest.ResponseRecorder {
	return f.admin.do("POST", "/api/v1/events/"+itoa(f.event.ID)+"/registrations/remove",
		map[string]any{"date": date, "registrationId": f.access.RegistrationID}, true)
}

// TestLaterEndRefusedWhileADateIsKept (DEC-156): on both single-date editors,
// any change that makes a date someone kept after leaving end later is
// refused on its first attempt with the refused date and its count, before
// any write or question: the date, its receipt, the grant, the notices and
// the audit are as they were, and the departed person still reads the date.
// A change that keeps or brings the end earlier applies, and a leader's add
// never refuses anything.
func TestLaterEndRefusedWhileADateIsKept(t *testing.T) {
	later := []struct {
		name    string
		changes func(o models.EventOccurrence) map[string]any
		tighten bool
		// token sends the date's own tightening token (DEC-158): the kept-date
		// refusal still comes first (C11).
		token bool
	}{
		{"a month later", func(o models.EventOccurrence) map[string]any {
			return map[string]any{"startsLocal": wallAt(o.StartsAt.AddDate(0, 0, 30))}
		}, false, false},
		{"longer", func(o models.EventOccurrence) map[string]any {
			return map[string]any{"durationMinutes": int(o.EndsAt.Sub(o.StartsAt)/time.Minute) + 30}
		}, false, false},
		{"later start, shorter, later end", func(o models.EventOccurrence) map[string]any {
			return map[string]any{"startsLocal": wallAt(o.StartsAt.Add(30 * time.Minute)), "durationMinutes": int(o.EndsAt.Sub(o.StartsAt)/time.Minute) - 15}
		}, false, false},
		{"tightened and later", func(o models.EventOccurrence) map[string]any {
			return map[string]any{"startsLocal": wallAt(o.StartsAt.AddDate(0, 0, 30)), "visibility": "series"}
		}, true, false},
		{"tightened and later, with a valid tightening token", func(o models.EventOccurrence) map[string]any {
			return map[string]any{"startsLocal": wallAt(o.StartsAt.AddDate(0, 0, 30)), "visibility": "series"}
		}, true, true},
	}
	for _, route := range dateEditRoutes {
		for _, tc := range later {
			t.Run(route.name+"/"+tc.name, func(t *testing.T) {
				f := newDepartureReadFixture(t, models.VisibilityInviteOnly, models.RegGoing, policy.OccurrenceOriginDepartureSelf)
				target := f.dates[1]
				date := models.FormatCalendarDate(target.OriginalDate)
				var outsider *models.User
				if tc.tighten {
					// A public date with an ordinary sign-up the tightening would cancel.
					outsider = f.other
					gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", target.ID).Update("visibility_override", models.VisibilityPublic))
					gormMust(t, f.app.DB.Create(&models.Registration{UserID: &outsider.ID, OccurrenceID: target.ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
				}
				changes := tc.changes(target)
				if tc.token {
					changes["consent"] = dateEditConsent(t, f.admin, f.event.ID, date, map[string]any{"visibility": "series"})
				}
				newMutableClock(f.app, target.EndsAt.Add(-time.Hour))
				mark := markNotices(t, f.app).notice
				audits := auditTotal(t, f.app)
				state := eventWriterState(t, f.app, f.event.ID)
				attempts := countDateAttempts(f.app)
				w := awaitResponse(t, route.run(t, f, date, changes))
				wantKeptDates(t, w, keptDateCountJSON{Date: date, KeptCount: 1})
				if n := attempts.Load(); n != 1 {
					t.Fatalf("the refusal took %d attempts", n)
				}
				if after := eventWriterState(t, f.app, f.event.ID); after != state {
					t.Fatalf("the refused edit wrote:\n%s\n%s", state, after)
				}
				if o := occurrenceRow(t, f.app, target.ID); o.EditReceipt != nil {
					t.Fatalf("the refusal left a receipt: %+v", o)
				}
				if got := savedOccurrenceAccess(t, f); got.Status != policy.OccurrenceAccessActive {
					t.Fatalf("the refusal ended the grant: %+v", got)
				}
				if n := noticeTotal(t, f.app, mark); n != 0 {
					t.Fatalf("the refusal sent %d notices", n)
				}
				if n := auditTotal(t, f.app); n != audits {
					t.Fatalf("the refusal wrote %d audit rows", n-audits)
				}
				if tc.tighten {
					if s := regStatus(t, f.app, outsider.ID, target.ID); s != models.RegGoing {
						t.Fatalf("the refused tightening cancelled the outsider: %s", s)
					}
				}
				apiDo(t, f.client, "GET", dateReadPath(f.event.ID, date), nil, http.StatusOK)
				// A6: the managers' exact-date reads count what the refusal counted.
				_, leader := groupLeader(t, f.app, f.group, "refusing leader")
				wantManagedKept(t, f.admin, f.event.ID, date, 1)
				wantManagedKept(t, leader, f.event.ID, date, 1)
			})
		}
		controls := []struct {
			name    string
			changes func(o models.EventOccurrence) map[string]any
		}{
			{"earlier", func(o models.EventOccurrence) map[string]any {
				return map[string]any{"startsLocal": wallAt(o.StartsAt.Add(-30 * time.Minute))}
			}},
			{"shorter", func(o models.EventOccurrence) map[string]any {
				return map[string]any{"durationMinutes": int(o.EndsAt.Sub(o.StartsAt)/time.Minute) - 15}
			}},
			{"title only", func(models.EventOccurrence) map[string]any { return map[string]any{"title": "A new title"} }},
			{"place only", func(models.EventOccurrence) map[string]any { return map[string]any{"location": "A new place"} }},
		}
		for _, tc := range controls {
			t.Run(route.name+"/control "+tc.name, func(t *testing.T) {
				f := newDepartureReadFixture(t, models.VisibilityInviteOnly, models.RegGoing, policy.OccurrenceOriginDepartureSelf)
				target := f.dates[1]
				newMutableClock(f.app, target.EndsAt.Add(-time.Hour))
				if w := awaitResponse(t, route.run(t, f, models.FormatCalendarDate(target.OriginalDate), tc.changes(target))); w.Code != http.StatusOK {
					t.Fatalf("the edit: %d %s", w.Code, w.Body.String())
				}
				if got := savedOccurrenceAccess(t, f); got.Status != policy.OccurrenceAccessActive {
					t.Fatalf("the edit ended the grant: %+v", got)
				}
			})
		}
		t.Run(route.name+"/control a leader's add", func(t *testing.T) {
			f := newOccurrenceReadFixture(t)
			target := f.dates[1]
			newMutableClock(f.app, target.EndsAt.Add(-time.Hour))
			changes := map[string]any{"startsLocal": wallAt(target.StartsAt.AddDate(0, 0, 30))}
			if w := awaitResponse(t, route.run(t, f, models.FormatCalendarDate(target.OriginalDate), changes)); w.Code != http.StatusOK {
				t.Fatalf("the edit: %d %s", w.Code, w.Body.String())
			}
			if got := savedOccurrenceAccess(t, f); got.Status != policy.OccurrenceAccessActive || got.Origin != policy.OccurrenceOriginManagerAdd {
				t.Fatalf("the leader's add changed: %+v", got)
			}
		})
		// A1b: a kept answer of Maybe, or a place in line, is refused too.
		for _, status := range []string{models.RegMaybe, models.RegWaitlisted} {
			t.Run(route.name+"/kept "+status, func(t *testing.T) {
				f := newDepartureReadFixture(t, models.VisibilityInviteOnly, status, policy.OccurrenceOriginDepartureSelf)
				target := f.dates[1]
				date := models.FormatCalendarDate(target.OriginalDate)
				newMutableClock(f.app, target.EndsAt.Add(-time.Hour))
				changes := map[string]any{"durationMinutes": int(target.EndsAt.Sub(target.StartsAt)/time.Minute) + 30}
				wantKeptDates(t, awaitResponse(t, route.run(t, f, date, changes)), keptDateCountJSON{Date: date, KeptCount: 1})
				_, leader := groupLeader(t, f.app, f.group, "refusing leader")
				wantManagedKept(t, f.admin, f.event.ID, date, 1)
				wantManagedKept(t, leader, f.event.ID, date, 1)
			})
		}
	}
}

// TestKeptDateRefusalAtTheOldEndBoundary: a microsecond before its old end
// the kept date is refused a later end; at the old end the edit's before pass
// ends the grant (occurrence_ended) and the move applies, so the departed
// person reads exactly 404 and is told only that the invitation ended.
func TestKeptDateRefusalAtTheOldEndBoundary(t *testing.T) {
	for _, route := range dateEditRoutes {
		t.Run(route.name, func(t *testing.T) {
			f := newDepartureReadFixture(t, models.VisibilityInviteOnly, models.RegGoing, policy.OccurrenceOriginDepartureSelf)
			target := f.dates[1]
			date := models.FormatCalendarDate(target.OriginalDate)
			goingMember(t, f, target.ID)
			mark := markNotices(t, f.app).notice
			clock := newMutableClock(f.app, target.EndsAt.Add(-time.Microsecond))
			changes := map[string]any{"startsLocal": wallAt(target.StartsAt.AddDate(0, 0, 30)), "title": "NEWSECRETTITLE"}
			wantKeptDates(t, awaitResponse(t, route.run(t, f, date, changes)), keptDateCountJSON{Date: date, KeptCount: 1})
			wantManagedKept(t, f.admin, f.event.ID, date, 1)
			clock.set(target.EndsAt)
			if w := awaitResponse(t, route.run(t, f, date, changes)); w.Code != http.StatusOK {
				t.Fatalf("the edit at the old end: %d %s", w.Code, w.Body.String())
			}
			if o := occurrenceRow(t, f.app, target.ID); !o.StartsAt.Equal(target.StartsAt.AddDate(0, 0, 30)) {
				t.Fatalf("the date did not move: %+v", o)
			}
			wantGrantEndedAt(t, f, target.EndsAt)
			wantOnlyGrantEnded(t, f, mark, date, "NEWSECRETTITLE")
			if got := noticesSince(t, f.app, f.recipient.ID, notify.EventChanged, mark); len(got) != 0 {
				t.Fatalf("the departed person was told the change: %+v", got)
			}
		})
	}
}

// TestKeptDateRefusalReaderInTheEdit: the departed person's read on another
// connection, while the guarded edit applies at the old end, answers 404, as
// it does after the commit; around a refused edit before the old end it
// answers 200 and the date is unchanged.
func TestKeptDateRefusalReaderInTheEdit(t *testing.T) {
	t.Run("applying", func(t *testing.T) {
		f := newDepartureReadFixture(t, models.VisibilityInviteOnly, models.RegGoing, policy.OccurrenceOriginDepartureSelf)
		target := f.dates[1]
		date := models.FormatCalendarDate(target.OriginalDate)
		newMutableClock(f.app, target.EndsAt)
		reader := cloneContactClient(f.client)
		var inside atomic.Int64
		f.app.hooks.dateEditStep = func(_ *gorm.DB, step string) error {
			if step == "effects" {
				inside.Store(int64(reader.do("GET", dateReadPath(f.event.ID, date), nil, false).Code))
			}
			return nil
		}
		base := editBaseOf(t, f.admin, f.event.ID, date)
		wantDateEdit(t, f.admin, f.event.ID, date, dateEditBody(newDateAttempt(t), base.Revision,
			map[string]any{"startsLocal": wallAt(target.StartsAt.AddDate(0, 0, 30))}), http.StatusOK, "")
		if inside.Load() != http.StatusNotFound {
			t.Fatalf("the read inside the edit: %d", inside.Load())
		}
		apiDo(t, f.client, "GET", dateReadPath(f.event.ID, date), nil, http.StatusNotFound)
	})
	t.Run("refused", func(t *testing.T) {
		f := newDepartureReadFixture(t, models.VisibilityInviteOnly, models.RegGoing, policy.OccurrenceOriginDepartureSelf)
		target := f.dates[1]
		date := models.FormatCalendarDate(target.OriginalDate)
		newMutableClock(f.app, target.EndsAt.Add(-time.Hour))
		apiDo(t, f.client, "GET", dateReadPath(f.event.ID, date), nil, http.StatusOK)
		base := editBaseOf(t, f.admin, f.event.ID, date)
		wantDateEdit(t, f.admin, f.event.ID, date, dateEditBody(newDateAttempt(t), base.Revision,
			map[string]any{"startsLocal": wallAt(target.StartsAt.AddDate(0, 0, 30))}), http.StatusConflict, "date_kept_after_leaving")
		apiDo(t, f.client, "GET", dateReadPath(f.event.ID, date), nil, http.StatusOK)
		if o := occurrenceRow(t, f.app, target.ID); !o.EndsAt.Equal(target.EndsAt) {
			t.Fatalf("the refused edit moved the date: %+v", o)
		}
	})
}

// keptWriters are the writers that can make a date end later, each moving the
// fixture's kept date later: the guarded POST a month on, and a series edit an
// hour on. at is the clock each runs on. The single-date writers come first.
var keptWriters = []struct {
	name string
	at   func(o models.EventOccurrence) time.Time
	run  func(t *testing.T, f occurrenceReadFixture, o models.EventOccurrence) *httptest.ResponseRecorder
}{
	{"guarded", func(o models.EventOccurrence) time.Time { return o.EndsAt.Add(-time.Hour) },
		func(t *testing.T, f occurrenceReadFixture, o models.EventOccurrence) *httptest.ResponseRecorder {
			return awaitResponse(t, dateEditRoutes[0].run(t, f, models.FormatCalendarDate(o.OriginalDate), map[string]any{"startsLocal": wallAt(o.StartsAt.AddDate(0, 0, 30))}))
		}},
	// The series moves only dates that have not started.
	{"series", func(o models.EventOccurrence) time.Time { return o.StartsAt.Add(-time.Hour) },
		func(t *testing.T, f occurrenceReadFixture, o models.EventOccurrence) *httptest.ResponseRecorder {
			return f.admin.do("PUT", "/api/v1/events/"+itoa(f.event.ID), seriesBody(t, f.admin, "/api/v1/events/"+itoa(f.event.ID), map[string]any{"startsLocal": wallAt(f.dates[0].StartsAt.Add(time.Hour))}), true)
		}},
}

// keptSeriesFixture is newDepartureReadFixture with the kept date's own title
// and place cleared, so a series edit moves it like any other date.
func keptSeriesFixture(t *testing.T, visibility string) occurrenceReadFixture {
	t.Helper()
	return keptSeriesFixtureAs(t, visibility, models.RegGoing)
}

// keptSeriesFixtureAs is keptSeriesFixture with the kept row in status.
func keptSeriesFixtureAs(t *testing.T, visibility, status string) occurrenceReadFixture {
	t.Helper()
	f := newDepartureReadFixture(t, visibility, status, policy.OccurrenceOriginDepartureSelf)
	gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", f.dates[1].ID).
		Updates(map[string]any{"title_override": nil, "location_override": nil}))
	f.dates[1] = occurrenceRow(t, f.app, f.dates[1].ID)
	return f
}

// TestKeptDateRefusedThenRemovedThenSaved: the way through a refusal is the
// roster Remove. On every writer the later end answers 409; Remove ends the
// grant as removed by the manager and tells only the person, once; the same
// save then applies. Afterwards the invite-only date is closed to the person
// (date and .ics), the public one shows them as removed, and only the
// ordinary member is told of the change.
func TestKeptDateRefusedThenRemovedThenSaved(t *testing.T) {
	for _, writer := range keptWriters {
		for _, visibility := range []string{models.VisibilityInviteOnly, models.VisibilityPublic} {
			t.Run(writer.name+"/"+visibility, func(t *testing.T) {
				f := keptSeriesFixture(t, visibility)
				target := f.dates[1]
				date := models.FormatCalendarDate(target.OriginalDate)
				member := goingMember(t, f, target.ID)
				mark := markNotices(t, f.app).notice
				newMutableClock(f.app, writer.at(target))
				wantKeptDates(t, writer.run(t, f, target), keptDateCountJSON{Date: date, KeptCount: 1})
				_, leader := groupLeader(t, f.app, f.group, "refusing leader")
				wantManagedKept(t, f.admin, f.event.ID, date, 1)
				wantManagedKept(t, leader, f.event.ID, date, 1)
				if w := removeKept(f, date); w.Code != http.StatusOK {
					t.Fatalf("Remove: %d %s", w.Code, w.Body.String())
				}
				wantManagedKept(t, leader, f.event.ID, date, 0)
				got := savedOccurrenceAccess(t, f)
				if got.Status != policy.OccurrenceAccessEnded || got.EndReason != policy.OccurrenceRemoved || ptrUint(got.EndedBy) != f.issuer.ID {
					t.Fatalf("Remove did not end the grant as the manager's: %+v", got)
				}
				if told := noticesAfter(t, f.app, f.recipient.ID, mark); len(told) != 1 || told[0].Kind != notify.OccurrenceUpdated {
					t.Fatalf("the person was told %+v", told)
				}
				if w := writer.run(t, f, target); w.Code != http.StatusOK {
					t.Fatalf("the save after Remove: %d %s", w.Code, w.Body.String())
				}
				if o := occurrenceRow(t, f.app, target.ID); !o.EndsAt.After(target.EndsAt) {
					t.Fatalf("the save did not move the date: %+v", o)
				}
				if visibility == models.VisibilityInviteOnly {
					apiDo(t, f.client, "GET", dateReadPath(f.event.ID, date), nil, http.StatusNotFound)
					apiDo(t, f.client, "GET", dateReadPath(f.event.ID, date)+"/calendar.ics", nil, http.StatusNotFound)
				} else {
					var read eventDateResponseJSON
					gormMust(t, &gorm.DB{Error: json.Unmarshal([]byte(apiDo(t, f.client, "GET", dateReadPath(f.event.ID, date), nil, http.StatusOK)), &read)})
					if read.Occurrence.MyRegistration == nil || *read.Occurrence.MyRegistration != models.RegRemoved {
						t.Fatalf("the public date's read: %+v", read.Occurrence.MyRegistration)
					}
				}
				if told := noticesSince(t, f.app, f.recipient.ID, notify.EventChanged, mark); len(told) != 0 {
					t.Fatalf("the removed person was told the change: %+v", told)
				}
				if told := noticesSince(t, f.app, member.ID, notify.EventChanged, mark); len(told) != 1 {
					t.Fatalf("the member was told %d changes: %+v", len(told), told)
				}
			})
		}
	}
}

// TestKeptDateRemovedAfterTheOldEnd (A3-late): the refusal is answered, then
// the old end passes before the manager removes the person. Remove's own
// before pass ends the grant (occurrence_ended, by nobody) and tells them once
// that the invitation ended; Remove then removes a row with no grant. The
// same save then applies.
func TestKeptDateRemovedAfterTheOldEnd(t *testing.T) {
	for _, writer := range keptWriters[:1] {
		t.Run(writer.name, func(t *testing.T) {
			f := newDepartureReadFixture(t, models.VisibilityInviteOnly, models.RegGoing, policy.OccurrenceOriginDepartureSelf)
			target := f.dates[1]
			date := models.FormatCalendarDate(target.OriginalDate)
			mark := markNotices(t, f.app).notice
			clock := newMutableClock(f.app, writer.at(target))
			wantKeptDates(t, writer.run(t, f, target), keptDateCountJSON{Date: date, KeptCount: 1})
			clock.set(target.EndsAt)
			if w := removeKept(f, date); w.Code != http.StatusOK {
				t.Fatalf("Remove: %d %s", w.Code, w.Body.String())
			}
			got := savedOccurrenceAccess(t, f)
			if got.Status != policy.OccurrenceAccessEnded || got.EndReason != policy.OccurrenceEnded || got.EndedBy != nil {
				t.Fatalf("the grant did not end with its date: %+v", got)
			}
			told := noticesAfter(t, f.app, f.recipient.ID, mark)
			kinds := []string{}
			for _, n := range told {
				kinds = append(kinds, n.Kind)
			}
			if want := []string{notify.OccurrenceUpdated, notify.RegistrationRemoved}; !slices.Equal(kinds, want) {
				t.Fatalf("the person was told %v, want the invitation's end and then the removal", kinds)
			}
			raw, err := json.Marshal(told)
			gormMust(t, &gorm.DB{Error: err})
			for _, hidden := range []string{"Retained date title", "Retained date place", "Retained hidden"} {
				if strings.Contains(string(raw), hidden) {
					t.Fatalf("the person, who can no longer see the date, was told %q: %s", hidden, raw)
				}
			}
			if s := regStatus(t, f.app, f.recipient.ID, target.ID); s != models.RegRemoved {
				t.Fatalf("Remove left %s", s)
			}
			if w := writer.run(t, f, target); w.Code != http.StatusOK {
				t.Fatalf("the save after Remove: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

// lengthen is the guarded POST making occ end 30 minutes later, as c.
func lengthen(t *testing.T, app *App, c *apiClient, eventID uint, occ models.EventOccurrence) *httptest.ResponseRecorder {
	t.Helper()
	o := occurrenceRow(t, app, occ.ID)
	date := models.FormatCalendarDate(o.OriginalDate)
	base := editBaseOf(t, c, eventID, date)
	return postDateEdit(c, eventID, date, dateEditBody(newDateAttempt(t), base.Revision, map[string]any{"durationMinutes": int(o.EndsAt.Sub(o.StartsAt)/time.Minute) + 30}))
}

// TestKeptDateRegrantPaths: what refuses a later end is the live grant's
// origin, wherever it came from. (a) After Remove and a rejoin, a direct add
// needs an ask; the approved ask's leader's add allows a later end, and
// leaving again, keeping the date, supersedes it with a departure grant that
// refuses one. (b) A grant ended by a series cancellation and replaced on the
// same row when the person leaves again after the restore refuses too. (c) A
// leader's add alone never refuses.
func TestKeptDateRegrantPaths(t *testing.T) {
	t.Run("removed, asked back, left again", func(t *testing.T) {
		f := newDepartureTestFixture(t)
		start := time.Now().UTC().Truncate(time.Minute).AddDate(0, 0, 2)
		occ, reg := addDepartureRegistration(t, f, f.subject, "Regrant date", models.VisibilityPublic, nil, start, start.Add(time.Hour), models.OccurrenceScheduled, models.RegGoing)
		date := models.FormatCalendarDate(occ.OriginalDate)
		base := "/api/v1/groups/" + itoa(f.group.ID)
		leave := func() {
			t.Helper()
			preview := getDeparturePreview(t, f.subjectC, base+"/departure-preview", http.StatusOK)
			if w := confirmDeparturePreview(t, f.subjectC, "POST", base+"/leave", preview, []uint{occ.ID}, ""); w.Code != http.StatusOK {
				t.Fatalf("leave: %d %s", w.Code, w.Body.String())
			}
		}
		leave()
		wantKeptDates(t, lengthen(t, f.app, f.leaderC, occ.EventID, occ), keptDateCountJSON{Date: date, KeptCount: 1})
		wantManagedKept(t, f.leaderC, occ.EventID, date, 1)
		apiDo(t, f.leaderC, "POST", "/api/v1/events/"+itoa(occ.EventID)+"/registrations/remove", map[string]any{"date": date, "registrationId": reg.ID}, http.StatusOK)
		apiDo(t, f.subjectC, "POST", base+"/join", nil, http.StatusOK)
		w := f.leaderC.do("POST", "/api/v1/events/"+itoa(occ.EventID)+"/registrations/add", map[string]any{"userId": f.subject.ID, "date": date}, true)
		if w.Code != http.StatusConflict || answerCode(t, w) != "date_add_approval_required" {
			t.Fatalf("the direct add after Remove: %d %s", w.Code, w.Body.String())
		}
		ask := createDateAsk(t, f.app, f.leaderC, f.leader, f.subject, occ.EventID, occ.ID)
		if w := addDecision(f.subjectC, ask.ID, "subject", "approved"); w.Code != http.StatusOK {
			t.Fatalf("approval: %d %s", w.Code, w.Body.String())
		}
		if row := loadAccessRow(t, f.app.DB, reg.ID); row.Status != policy.OccurrenceAccessActive || row.Origin != policy.OccurrenceOriginManagerAdd {
			t.Fatalf("the approved ask's grant: %+v", row)
		}
		if w := lengthen(t, f.app, f.leaderC, occ.EventID, occ); w.Code != http.StatusOK {
			t.Fatalf("a later end with a leader's add: %d %s", w.Code, w.Body.String())
		}
		leave()
		history := accessSnapshotsFor(t, f.app.DB, reg.ID)
		n := len(history)
		if n < 2 || history[n-2].Action != audit.OccurrenceAccessEnded || history[n-2].Meta["endReason"] != policy.OccurrenceDepartureSuperseded ||
			history[n-2].Meta["origin"] != policy.OccurrenceOriginManagerAdd || history[n-1].Action != audit.OccurrenceAccessAdded ||
			history[n-1].Meta["origin"] != policy.OccurrenceOriginDepartureSelf || history[n-1].Meta["grantedAtMicro"] != history[n-2].Meta["endedAtMicro"] {
			t.Fatalf("leaving again did not supersede the leader's add: %+v", history)
		}
		wantKeptDates(t, lengthen(t, f.app, f.leaderC, occ.EventID, occ), keptDateCountJSON{Date: date, KeptCount: 1})
		wantManagedKept(t, f.leaderC, occ.EventID, date, 1)
	})
	t.Run("same row after a series cancel and restore", func(t *testing.T) {
		f := newDepartureTestFixture(t)
		ev := createFeedEvent(t, f.leaderC, f.group.ID, "Regrant series", feedStart(2), "FREQ=WEEKLY;COUNT=6")
		var dates []models.EventOccurrence
		gormMust(t, f.app.DB.Where("event_id = ?", ev.ID).Order("starts_at").Find(&dates))
		occ := dates[4]
		date := models.FormatCalendarDate(occ.OriginalDate)
		keepOnLeaving(t, f.subjectC, f.group.ID, ev.ID, occ)
		var reg models.Registration
		gormMust(t, f.app.DB.Where("occurrence_id = ? AND user_id = ?", occ.ID, f.subject.ID).First(&reg))
		apiDo(t, f.leaderC, "PUT", "/api/v1/events/"+itoa(ev.ID), seriesBody(t, f.leaderC, "/api/v1/events/"+itoa(ev.ID), map[string]any{"recurrenceRule": "FREQ=WEEKLY;COUNT=4"}), http.StatusOK)
		if row := loadAccessRow(t, f.app.DB, reg.ID); row.Status != policy.OccurrenceAccessEnded || row.EndReason != policy.OccurrenceCancelled {
			t.Fatalf("the series cancel did not end the grant: %+v", row)
		}
		apiDo(t, f.leaderC, "PUT", "/api/v1/events/"+itoa(ev.ID), seriesBody(t, f.leaderC, "/api/v1/events/"+itoa(ev.ID), map[string]any{"recurrenceRule": "FREQ=WEEKLY;COUNT=6"}), http.StatusOK)
		if o := occurrenceRow(t, f.app, occ.ID); o.Status != models.OccurrenceScheduled {
			t.Fatalf("the series edit did not restore the date: %+v", o)
		}
		apiDo(t, f.subjectC, "POST", "/api/v1/groups/"+itoa(f.group.ID)+"/join", nil, http.StatusOK)
		preview := getDeparturePreview(t, f.subjectC, "/api/v1/groups/"+itoa(f.group.ID)+"/departure-preview", http.StatusOK)
		if w := confirmDeparturePreview(t, f.subjectC, "POST", "/api/v1/groups/"+itoa(f.group.ID)+"/leave", preview, []uint{occ.ID}, ""); w.Code != http.StatusOK {
			t.Fatalf("leaving again: %d %s", w.Code, w.Body.String())
		}
		row := loadAccessRow(t, f.app.DB, reg.ID)
		added := 0
		for _, s := range accessSnapshotsFor(t, f.app.DB, reg.ID) {
			if s.Action == audit.OccurrenceAccessAdded {
				added++
			}
		}
		if row.Status != policy.OccurrenceAccessActive || row.Origin != policy.OccurrenceOriginDepartureSelf || added != 2 {
			t.Fatalf("the same row's new grant: %+v, %d added snapshots", row, added)
		}
		wantKeptDates(t, lengthen(t, f.app, f.leaderC, ev.ID, occ), keptDateCountJSON{Date: date, KeptCount: 1})
		wantManagedKept(t, f.leaderC, ev.ID, date, 1)
	})
	t.Run("a leader's add alone", func(t *testing.T) {
		f := newOccurrenceReadFixture(t)
		wantManagedKept(t, f.admin, f.event.ID, models.FormatCalendarDate(f.dates[1].OriginalDate), 0)
		if w := lengthen(t, f.app, f.admin, f.event.ID, f.dates[1]); w.Code != http.StatusOK {
			t.Fatalf("a later end with only a leader's add: %d %s", w.Code, w.Body.String())
		}
	})
}

// keptPreviewFixture is A1's tightening case for the date preview: a public
// date someone kept after leaving, with an ordinary sign-up that tightening
// it back to the series would cancel.
func keptPreviewFixture(t *testing.T) (occurrenceReadFixture, string) {
	t.Helper()
	f := newDepartureReadFixture(t, models.VisibilityInviteOnly, models.RegGoing, policy.OccurrenceOriginDepartureSelf)
	target := f.dates[1]
	gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", target.ID).Update("visibility_override", models.VisibilityPublic))
	gormMust(t, f.app.DB.Create(&models.Registration{UserID: &f.other.ID, OccurrenceID: target.ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
	return f, managedDatePath(f.event.ID, models.FormatCalendarDate(target.OriginalDate)) + "/visibility-impact?visibility=series"
}

// TestDatePreviewRefusesALaterEndOnAKeptDate: the date preview answers the
// refusal the save would give when its times make a kept date end later,
// counting only grants current on its own clock; otherwise it answers the
// impact as before.
func TestDatePreviewRefusesALaterEndOnAKeptDate(t *testing.T) {
	f, path := keptPreviewFixture(t)
	target := f.dates[1]
	date := models.FormatCalendarDate(target.OriginalDate)
	// Two hours on, so the moved date is still upcoming at the old end and the
	// impact has someone to count.
	laterQ := "&startsLocal=" + wallAt(target.StartsAt.Add(2*time.Hour))
	clock := newMutableClock(f.app, target.StartsAt.Add(-2*time.Hour))
	wantImpact := func(label string, w *httptest.ResponseRecorder, registrants int) {
		t.Helper()
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), fmt.Sprintf(`"registrants":%d`, registrants)) {
			t.Fatalf("%s: %d %s, want the impact for %d", label, w.Code, w.Body.String(), registrants)
		}
	}
	wantKeptDates(t, f.admin.do("GET", path+laterQ, nil, false), keptDateCountJSON{Date: date, KeptCount: 1})
	wantImpact("no times", f.admin.do("GET", path, nil, false), 1)
	wantImpact("an earlier end", f.admin.do("GET", path+"&startsLocal="+wallAt(target.StartsAt.Add(-time.Hour)), nil, false), 1)
	// The fixture date ends in the wall clock's future: at its old end on the
	// injected clock the grant has lapsed, unstamped, and is not counted as
	// kept. The departed person's row now counts among those the tightening
	// would cancel, as the save's before pass would leave it.
	clock.set(target.EndsAt)
	wantImpact("a lapsed grant", f.admin.do("GET", path+laterQ, nil, false), 2)
	clock.set(target.EndsAt.Add(-time.Hour))
	wantKeptDates(t, f.admin.do("GET", path+laterQ, nil, false), keptDateCountJSON{Date: date, KeptCount: 1})
	wantManagedKept(t, f.admin, f.event.ID, date, 1)
	if got := savedOccurrenceAccess(t, f); got.Status != policy.OccurrenceAccessActive {
		t.Fatalf("the preview ended the grant: %+v", got)
	}
}

// TestDatePreviewKeptCountReadsItsSnapshot: the preview counts the kept grants
// in the snapshot its authority was judged in. The real Remove commits right
// after that judgment: the preview in flight still refuses with the one kept
// grant, and the next one answers the impact.
func TestDatePreviewKeptCountReadsItsSnapshot(t *testing.T) {
	f, path := keptPreviewFixture(t)
	target := f.dates[1]
	date := models.FormatCalendarDate(target.OriginalDate)
	path += "&startsLocal=" + wallAt(target.StartsAt.Add(time.Hour))
	remover := cloneContactClient(f.admin)
	fired := atReaderSeam(f.app, "authorized", "date visibility preview", func() {
		if w := removeKept(occurrenceReadFixture{admin: remover, event: f.event, access: f.access}, date); w.Code != http.StatusOK {
			t.Errorf("Remove at the seam: %d %s", w.Code, w.Body.String())
		}
	})
	w := f.admin.do("GET", path, nil, false)
	if !fired.Load() {
		t.Fatal("the preview never reached its seam")
	}
	wantKeptDates(t, w, keptDateCountJSON{Date: date, KeptCount: 1})
	if got := savedOccurrenceAccess(t, f); got.EndReason != policy.OccurrenceRemoved {
		t.Fatalf("Remove did not end the grant: %+v", got)
	}
	if w := f.admin.do("GET", path, nil, false); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"tightened"`) {
		t.Fatalf("the next preview: %d %s", w.Code, w.Body.String())
	}
}

// TestKeptDateNonTimeSaveKeepsItsStoredEnd (DEC-156): a date with its own
// title and place is left alone when the series is lengthened
// (events.ApplySeries), so it now lasts less than the event's duration. On
// both single-date editors a save that names no start or duration (title,
// place, capacity, visibility) keeps its stored start and end, applies while
// the date is kept and tells no one of a time change; the preview with no
// times answers the ordinary impact. A start or duration still counts from the
// duration the date is shown with (the event's): a later start, a start one
// minute earlier that ends later on that duration, and a duration past the
// stored span are refused, on the save and on the preview, and a duration
// that lands exactly on the stored end applies.
func TestKeptDateNonTimeSaveKeepsItsStoredEnd(t *testing.T) {
	saves := []map[string]any{
		{"title": "Only a new title"},
		{"location": "Only a new place"},
		{"capacityOverride": 5},
		{"visibility": models.VisibilityPublic},
	}
	for _, route := range dateEditRoutes {
		t.Run(route.name, func(t *testing.T) {
			f := newDepartureReadFixture(t, models.VisibilityInviteOnly, models.RegGoing, policy.OccurrenceOriginDepartureSelf)
			target := f.dates[1]
			date := models.FormatCalendarDate(target.OriginalDate)
			member := goingMember(t, f, target.ID)
			span := target.EndsAt.Sub(target.StartsAt)
			apiDo(t, f.admin, "PUT", "/api/v1/events/"+itoa(f.event.ID), seriesBody(t, f.admin, "/api/v1/events/"+itoa(f.event.ID), map[string]any{"durationMinutes": int(span/time.Minute) + 30}), http.StatusOK)
			if o := occurrenceRow(t, f.app, target.ID); !o.StartsAt.Equal(target.StartsAt) || !o.EndsAt.Equal(target.EndsAt) {
				t.Fatalf("fixture: the series edit moved the customized date: %+v", o)
			}
			mark := markNotices(t, f.app).notice
			preview := managedDatePath(f.event.ID, date) + "/visibility-impact?visibility=series"
			if w := f.admin.do("GET", preview, nil, false); w.Code != http.StatusOK || strings.Contains(w.Body.String(), "keptDates") || !strings.Contains(w.Body.String(), `"tightened"`) {
				t.Fatalf("the preview with no times: %d %s", w.Code, w.Body.String())
			}
			for _, save := range saves {
				if w := awaitResponse(t, route.run(t, f, date, save)); w.Code != http.StatusOK {
					t.Fatalf("%v: %d %s", save, w.Code, w.Body.String())
				}
				o := occurrenceRow(t, f.app, target.ID)
				if !o.StartsAt.Equal(target.StartsAt) || !o.EndsAt.Equal(target.EndsAt) || o.StartsAtOverride != nil || o.DurationOverride != nil {
					t.Fatalf("%v moved the date's clock: %+v", save, o)
				}
			}
			if got := savedOccurrenceAccess(t, f); got.Status != policy.OccurrenceAccessActive {
				t.Fatalf("a save with no times ended the kept grant: %+v", got)
			}
			// The place save is told as a place change, never a time change.
			for _, uid := range []uint{member.ID, f.recipient.ID} {
				for _, n := range noticesSince(t, f.app, uid, notify.EventChanged, mark) {
					if !strings.Contains(n.Data, `"change": "place"`) {
						t.Fatalf("a save with no times told %d of a time change: %+v", uid, n)
					}
				}
			}
			kept := keptDateCountJSON{Date: date, KeptCount: 1}
			later := map[string]any{"durationMinutes": int(span/time.Minute) + 1}
			wantKeptDates(t, awaitResponse(t, route.run(t, f, date, later)), kept)
			wantKeptDates(t, f.admin.do("GET", preview+"&durationMinutes="+itoa(uint(span/time.Minute)+1), nil, false), kept)
			for _, start := range []time.Time{target.StartsAt.Add(time.Minute), target.StartsAt.Add(-time.Minute)} {
				wantKeptDates(t, awaitResponse(t, route.run(t, f, date, map[string]any{"startsLocal": wallAt(start)})), kept)
				wantKeptDates(t, f.admin.do("GET", preview+"&startsLocal="+url.QueryEscape(wallAt(start)), nil, false), kept)
			}
			if w := awaitResponse(t, route.run(t, f, date, map[string]any{"durationMinutes": int(span / time.Minute)})); w.Code != http.StatusOK {
				t.Fatalf("a duration ending on the stored end: %d %s", w.Code, w.Body.String())
			}
			if o := occurrenceRow(t, f.app, target.ID); !o.EndsAt.Equal(target.EndsAt) {
				t.Fatalf("a duration ending on the stored end moved it: %+v", o)
			}
		})
	}
}

// TestKeptGoingRemovalPromotesTheWaiterOnce (DEC-156): the way through a
// refusal promotes the waitlist when the kept row was Going. The kept row holds
// the date's one seat and a member waits for it. The refusal changes nothing;
// Remove ends the grant as removed and promotes exactly that waiter, with one
// audit row and one notice; the retried save applies and promotes no one
// again. With the kept row Maybe (no seat held, so the seat is already free)
// Remove promotes no one: a kept grant alone is no reason to promote.
func TestKeptGoingRemovalPromotesTheWaiterOnce(t *testing.T) {
	for _, writer := range keptWriters {
		for _, status := range []string{models.RegGoing, models.RegMaybe} {
			t.Run(writer.name+"/"+status, func(t *testing.T) {
				f := keptSeriesFixtureAs(t, models.VisibilityInviteOnly, status)
				target := f.dates[1]
				date := models.FormatCalendarDate(target.OriginalDate)
				gormMust(t, f.app.DB.Model(&models.Event{}).Where("id = ?", f.event.ID).Update("capacity", 1))
				waiter := newTestUser(t, f.app, "waiting member", "password1234", models.RoleMember)
				gormMust(t, f.app.DB.Create(&models.Membership{UserID: waiter.ID, GroupID: f.group, Role: models.MembershipMember, Status: models.MembershipActive}))
				queued := time.Now().UTC().Add(-time.Minute)
				waiting := models.Registration{UserID: &waiter.ID, OccurrenceID: target.ID, Status: models.RegWaitlisted, AttendanceMode: models.AttendanceInPerson, Source: "web", WaitlistedAt: &queued}
				gormMust(t, f.app.DB.Create(&waiting))
				mark := markNotices(t, f.app).notice
				var auditMark uint
				gormMust(t, f.app.DB.Model(&models.AuditLog{}).Select("COALESCE(MAX(id), 0)").Scan(&auditMark))
				promoted := func() (audits int64, notices int) {
					t.Helper()
					gormMust(t, f.app.DB.Model(&models.AuditLog{}).Where("id > ? AND action = ? AND meta_json->>'registrationId' = ? AND meta_json->>'newStatus' = ?",
						auditMark, audit.RegistrationChanged, itoa(waiting.ID), models.RegGoing).Count(&audits))
					return audits, len(noticesSince(t, f.app, waiter.ID, notify.RegistrationPromoted, mark))
				}
				// Before the waitlist cutoff, so a freed seat is offered.
				newMutableClock(f.app, target.StartsAt.Add(-3*time.Hour))
				wantKeptDates(t, writer.run(t, f, target), keptDateCountJSON{Date: date, KeptCount: 1})
				if o := occurrenceRow(t, f.app, target.ID); !o.StartsAt.Equal(target.StartsAt) || !o.EndsAt.Equal(target.EndsAt) {
					t.Fatalf("the refusal moved the date: %+v", o)
				}
				if got := regStatus(t, f.app, f.recipient.ID, target.ID); got != status {
					t.Fatalf("the refusal changed the kept row: %s", got)
				}
				if got := regStatus(t, f.app, waiter.ID, target.ID); got != models.RegWaitlisted {
					t.Fatalf("the refusal changed the waiter: %s", got)
				}
				if a, n := promoted(); a != 0 || n != 0 || noticeTotal(t, f.app, mark) != 0 {
					t.Fatalf("the refusal promoted or told someone: %d audits, %d promotion notices, %d notices", a, n, noticeTotal(t, f.app, mark))
				}
				if w := removeKept(f, date); w.Code != http.StatusOK {
					t.Fatalf("Remove: %d %s", w.Code, w.Body.String())
				}
				if got := savedOccurrenceAccess(t, f); got.Status != policy.OccurrenceAccessEnded || got.EndReason != policy.OccurrenceRemoved {
					t.Fatalf("Remove did not end the grant as removed: %+v", got)
				}
				if told := noticesAfter(t, f.app, f.recipient.ID, mark); len(told) != 1 || told[0].Kind != notify.OccurrenceUpdated {
					t.Fatalf("the removed person was told %+v", told)
				}
				wantStatus, wantAudits, wantNotices := models.RegWaitlisted, int64(0), 0
				if status == models.RegGoing {
					wantStatus, wantAudits, wantNotices = models.RegGoing, 1, 1
				}
				check := func(when string) {
					t.Helper()
					if got := regStatus(t, f.app, waiter.ID, target.ID); got != wantStatus {
						t.Fatalf("%s: the waiter is %s, want %s", when, got, wantStatus)
					}
					if a, n := promoted(); a != wantAudits || n != wantNotices {
						t.Fatalf("%s: %d promotion audits and %d promotion notices, want %d and %d", when, a, n, wantAudits, wantNotices)
					}
				}
				check("after Remove")
				if w := writer.run(t, f, target); w.Code != http.StatusOK {
					t.Fatalf("the save after Remove: %d %s", w.Code, w.Body.String())
				}
				if o := occurrenceRow(t, f.app, target.ID); !o.EndsAt.After(target.EndsAt) {
					t.Fatalf("the save did not move the date: %+v", o)
				}
				check("after the save")
			})
		}
	}
}

// managedRead is the manager's exact-date read of date as c: the date and the
// raw body.
func managedRead(t *testing.T, c *apiClient, eventID uint, date string) (occurrenceJSON, string) {
	t.Helper()
	w := c.do("GET", managedDatePath(eventID, date), nil, false)
	if w.Code != http.StatusOK {
		t.Fatalf("managed read %s: %d %s", date, w.Code, w.Body.String())
	}
	var body occurrenceResponseJSON
	decodeJSON(t, w, &body)
	return body.Occurrence, w.Body.String()
}

// managedKeys is the managed read's top-level keys, sorted.
func managedKeys(t *testing.T, raw string) []string {
	t.Helper()
	var body struct {
		Occurrence map[string]json.RawMessage `json:"occurrence"`
	}
	gormMust(t, &gorm.DB{Error: json.Unmarshal([]byte(raw), &body)})
	keys := make([]string, 0, len(body.Occurrence))
	for k := range body.Occurrence {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// wantManagedKept fails unless c's managed read of date carries its roster
// with keptCount want and exactly want listed rows flagged keptAfterLeaving,
// every listed row carrying the manager's flag. It is the 409's count (DEC-156)
// when every counted grant is on a row c's roster lists.
func wantManagedKept(t *testing.T, c *apiClient, eventID uint, date string, want int) occurrenceJSON {
	t.Helper()
	got, raw := managedRead(t, c, eventID, date)
	if !got.AttendeesVisible || got.KeptCount == nil || *got.KeptCount != want {
		t.Fatalf("managed read of %s: keptCount %v, want %d with its roster: %s", date, got.KeptCount, want, raw)
	}
	flagged := 0
	for _, row := range slices.Concat(got.Attendees, got.RemovedRegistrations) {
		if row.KeptAfterLeaving == nil {
			t.Fatalf("a manager's roster row has no keptAfterLeaving: %s", raw)
		}
		if *row.KeptAfterLeaving {
			flagged++
		}
	}
	if flagged != want {
		t.Fatalf("managed read of %s flags %d rows kept, want %d: %s", date, flagged, want, raw)
	}
	return got
}

// groupLeader adds a signed-in leader of group who is not an admin.
func groupLeader(t *testing.T, app *App, group uint, name string) (*models.User, *apiClient) {
	t.Helper()
	u := newTestUser(t, app, name, "password1234", models.RoleMember)
	gormMust(t, app.DB.Create(&models.Membership{UserID: u.ID, GroupID: group, Role: models.MembershipLeader, Status: models.MembershipActive}))
	return u, loginAPI(t, app, name, "password1234")
}

// keepDateFor gives u a live departure grant of origin on occurrence occ, the
// way departure leaves one: a Going row while u is a member, then the
// membership removed. actor is who made the departure.
func keepDateFor(app *App, group, occ uint, u *models.User, origin string, actor uint) (uint, error) {
	membership := models.Membership{UserID: u.ID, GroupID: group, Role: models.MembershipMember, Status: models.MembershipActive}
	var regID uint
	if err := app.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&membership).Error; err != nil {
			return err
		}
		reg := models.Registration{UserID: &u.ID, OccurrenceID: occ, Status: models.RegGoing, Source: models.RegSourceAdmin, AttendanceMode: models.AttendanceInPerson}
		if err := tx.Create(&reg).Error; err != nil {
			return err
		}
		regID = reg.ID
		_, err := seedAccessGrantIn(tx, reg.ID, accessGrant{Origin: origin, DepartureMembershipID: &membership.ID, DepartureActorID: &actor}, &actor, time.Now().UTC())
		return err
	}); err != nil {
		return 0, err
	}
	return regID, app.contactMutation(contactChange{UserID: u.ID}, func(tx *gorm.DB) error {
		return tx.Model(&models.Membership{}).Where("id = ?", membership.ID).Update("status", "removed").Error
	})
}

// activeTeen is a signed-in teen with account consent, as consent leaves one.
func activeTeen(t *testing.T, app *App, name string, grantedBy uint) *models.User {
	t.Helper()
	u := newTestUser(t, app, name, "password1234", models.RoleMember)
	gormMust(t, app.DB.Model(u).Updates(map[string]any{"account_type": models.AccountTeen, "birth_date": time.Now().UTC().AddDate(-15, 0, 0)}))
	gormMust(t, app.DB.Create(&models.TeenConsent{TeenUserID: u.ID, Scope: models.TeenConsentAccount, Status: models.ConsentGranted, GrantedBy: grantedBy, GranterRole: "admin", EvidenceText: "Paper form"}))
	gormMust(t, app.DB.First(u, u.ID))
	return u
}

// rosterPeople is what seedRosterPeople puts on one date: a teen Going whose
// confirmed guardian is guardian, a household child Going with care notes, and
// an adult of the same household whose row was not restored (a recoverable
// row). Every helper the roster reads runs for them.
type rosterPeople struct {
	teen                  *models.User
	holder                *models.User
	child, recoverable    models.HouseholdPerson
	careNote              string
	childName, adultName  string
	teenEmail, holderName string
}

func seedRosterPeople(t *testing.T, app *App, group uint, ev uint, occ uint, guardian, admin *models.User) rosterPeople {
	t.Helper()
	p := rosterPeople{careNote: "Roster care note: peanut allergy", childName: "Robin Rosterchild", adultName: "Ada Recoverable"}
	p.teen = activeTeen(t, app, "roster teen", admin.ID)
	p.teenEmail = p.teen.Email
	gormMust(t, app.DB.Create(&models.Membership{UserID: p.teen.ID, GroupID: group, Role: models.MembershipMember, Status: models.MembershipActive}))
	gormMust(t, app.DB.Create(&models.Registration{UserID: &p.teen.ID, OccurrenceID: occ, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
	gormMust(t, app.DB.Create(&models.GuardianLink{GuardianUserID: guardian.ID, TeenUserID: &p.teen.ID, Status: models.GuardianConfirmed, ConfirmedBy: &admin.ID}))

	p.holder = newTestUser(t, app, "roster holder", "password1234", models.RoleMember)
	p.holderName = p.holder.PublicName()
	gormMust(t, app.DB.Create(&models.Membership{UserID: p.holder.ID, GroupID: group, Role: models.MembershipMember, Status: models.MembershipActive}))
	h := models.Household{Name: "Roster household", CreatedBy: p.holder.ID}
	gormMust(t, app.DB.Create(&h))
	gormMust(t, app.DB.Create(&models.HouseholdAccount{HouseholdID: h.ID, UserID: p.holder.ID, Relation: models.HouseholdRelationGuardian}))
	childBirth := time.Now().UTC().AddDate(-8, 0, 0)
	p.child = models.HouseholdPerson{HouseholdID: h.ID, FirstName: "Robin", LastName: "Rosterchild", Kind: models.HouseholdChild, BirthDate: &childBirth, CareNotes: p.careNote, CreatedBy: p.holder.ID}
	gormMust(t, app.DB.Create(&p.child))
	gormMust(t, app.DB.Create(&models.GuardianLink{GuardianUserID: p.holder.ID, PersonID: &p.child.ID, Status: models.GuardianConfirmed, ConfirmedBy: &admin.ID}))
	gormMust(t, app.DB.Create(&models.Registration{HouseholdPersonID: &p.child.ID, RegisteredBy: &p.holder.ID, OccurrenceID: occ, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}))

	adultBirth := time.Now().UTC().AddDate(-30, 0, 0)
	p.recoverable = models.HouseholdPerson{HouseholdID: h.ID, FirstName: "Ada", LastName: "Recoverable", Kind: models.HouseholdAdult, BirthDate: &adultBirth, CreatedBy: p.holder.ID}
	gormMust(t, app.DB.Create(&p.recoverable))
	meta, err := json.Marshal(droppedRegistrationMeta{RegistrationID: 0, PersonID: &p.recoverable.ID, RegisteredBy: &p.holder.ID, OccurrenceID: occ, AttendanceMode: models.AttendanceInPerson})
	gormMust(t, &gorm.DB{Error: err})
	gormMust(t, app.DB.Create(&models.AuditLog{Action: audit.RegistrationNotRestored, TargetType: "event", TargetID: ev, MetaJSON: string(meta)}))
	return p
}

// wantRosterPeople fails unless the managed read as a leader who is the teen's
// guardian shows every seeded person: the teen with their email (the guardian
// rule), the child with care notes (the care window on the read's clock), and
// the recoverable adult.
func wantRosterPeople(t *testing.T, got occurrenceJSON, raw string, p rosterPeople) {
	t.Helper()
	var teen, child *attendeeJSON
	for i := range got.Attendees {
		row := &got.Attendees[i]
		if row.UserID != nil && *row.UserID == p.teen.ID {
			teen = row
		}
		if row.HouseholdPerson != nil && row.HouseholdPerson.PersonID == p.child.ID {
			child = row
		}
	}
	if teen == nil || teen.Email == nil || *teen.Email != p.teenEmail {
		t.Fatalf("the teen's guardian lost their contact on the roster: %s", raw)
	}
	if child == nil || child.HouseholdPerson.CareNotes == nil || *child.HouseholdPerson.CareNotes != p.careNote {
		t.Fatalf("the child's care notes are missing in their window: %s", raw)
	}
	recovered := false
	for _, row := range got.RemovedRegistrations {
		if row.HouseholdPerson != nil && row.HouseholdPerson.PersonID == p.recoverable.ID && row.Status == "not_restored" {
			recovered = true
		}
	}
	if !recovered {
		t.Fatalf("the recoverable row is missing: %s", raw)
	}
}

// TestManagedDateCarriesItsRoster (DEC-156): the manager's exact-date read
// carries the roster the event list gives a manager for a scheduled date that
// has not ended on its own clock, whether it is underway or beyond the event
// page's 90 days. A cancelled date answers the plain edit base, key for key
// (no label, no roster), and an ended one adds only the group label: neither shows a name.
// For a listed date the read and the event list agree on every field the
// editor and the roster read.
func TestManagedDateCarriesItsRoster(t *testing.T) {
	f := newDateEditFixture(t)
	leader := f.leaderUser
	now := time.Now().UTC()
	setClock := func(t *testing.T, o *models.EventOccurrence, start, end time.Time) {
		gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", o.ID).Updates(map[string]any{"starts_at": start, "ends_at": end}))
		*o = occurrenceRow(t, f.app, o.ID)
	}
	going := func(t *testing.T, u *models.User, occ uint, status string) {
		gormMust(t, f.app.DB.Create(&models.Registration{UserID: &u.ID, OccurrenceID: occ, Status: status, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
	}
	underway, ended, cancelled, listed := f.dates[0], f.dates[1], f.dates[2], f.dates[3]
	setClock(t, &underway, now.Add(-time.Hour), now.Add(time.Hour))
	setClock(t, &ended, now.Add(-3*time.Hour), now.Add(-2*time.Hour))
	for _, o := range []models.EventOccurrence{underway, ended, cancelled, listed} {
		going(t, f.memberUser, o.ID, models.RegGoing)
	}
	// The cancelled date holds a Going row, a removed row and a teen with a
	// confirmed guardian, all stored before it was cancelled.
	going(t, f.outsiders[0], cancelled.ID, models.RegRemoved)
	teen := activeTeen(t, f.app, "cancelled teen", f.adminUser.ID)
	going(t, teen, cancelled.ID, models.RegGoing)
	gormMust(t, f.app.DB.Create(&models.GuardianLink{GuardianUserID: leader.ID, TeenUserID: &teen.ID, Status: models.GuardianConfirmed, ConfirmedBy: &f.adminUser.ID}))
	gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", cancelled.ID).Updates(map[string]any{"status": models.OccurrenceCancelled, "cancel_source": "manual"}))
	far := createFeedEvent(t, f.admin, f.group, "Far supper", feedStart(121), "")
	var farDate models.EventOccurrence
	gormMust(t, f.app.DB.Where("event_id = ?", far.ID).First(&farDate))
	going(t, f.memberUser, farDate.ID, models.RegGoing)
	member := f.memberUser.PublicName()
	names := []string{member, f.outsiders[0].PublicName(), teen.PublicName(), teen.Email, f.memberUser.Email}

	for _, c := range []struct {
		name string
		c    *apiClient
	}{{"admin", f.admin}, {"leader", f.leader}} {
		t.Run(c.name+"/with a roster", func(t *testing.T) {
			for _, d := range []struct {
				label string
				ev    uint
				date  models.EventOccurrence
			}{{"underway", f.event.ID, underway}, {"121 days ahead", far.ID, farDate}} {
				got, raw := managedRead(t, c.c, d.ev, models.FormatCalendarDate(d.date.OriginalDate))
				if !got.AttendeesVisible || got.Revision == "" || got.KeptCount == nil || *got.KeptCount != 0 || got.GroupName != "Supper club" || got.GroupID == nil {
					t.Fatalf("%s: %s", d.label, raw)
				}
				if len(got.Attendees) != 1 || got.Attendees[0].Name != member || got.Attendees[0].AddedByLeader == nil || got.Attendees[0].KeptAfterLeaving == nil || *got.Attendees[0].KeptAfterLeaving {
					t.Fatalf("%s roster: %s", d.label, raw)
				}
				if got.Registrations[models.RegGoing] != 1 {
					t.Fatalf("%s counts: %s", d.label, raw)
				}
				if _, ok := got.Registrations[models.RegDeclined]; !ok {
					t.Fatalf("%s: a manager's counts carry \"can't go\": %s", d.label, raw)
				}
			}
		})
		t.Run(c.name+"/cancelled", func(t *testing.T) {
			got, raw := managedRead(t, c.c, f.event.ID, models.FormatCalendarDate(cancelled.OriginalDate))
			want := strings.Fields("attendeesVisible cancelled capacity childcareAges childcareAvailable childcareCapacity childcareFull childcareNotes churchWide date description durationMinutes endsAt endsLocal eventId guestsAllowed id leaderOpened location myHouseholdRegistrations myRegistration overridden recurrenceRule registrations reminderEmailMinutes reminderPush remoteAvailable remoteCapacity remoteSeatsUsed remoteSpotsLeft revision seatsUsed seriesDurationMinutes seriesLocation seriesStartsLocal seriesTitle seriesVisibility spotsLeft startsAt startsLocal title tzid videoProvider videoReveal visibility waitlistEnabled")
			if keys := managedKeys(t, raw); !slices.Equal(keys, want) {
				t.Fatalf("a cancelled date's edit base keys:\n%v\nwant\n%v", keys, want)
			}
			if got.AttendeesVisible || !got.Cancelled || !maps.Equal(got.Registrations, registrationCounts(nil, false)) {
				t.Fatalf("a cancelled date's edit base: %s", raw)
			}
			for _, s := range names {
				if strings.Contains(raw, s) {
					t.Fatalf("a cancelled date's edit base shows %q: %s", s, raw)
				}
			}
		})
		t.Run(c.name+"/ended", func(t *testing.T) {
			got, raw := managedRead(t, c.c, f.event.ID, models.FormatCalendarDate(ended.OriginalDate))
			if got.AttendeesVisible || got.GroupName != "Supper club" || got.GroupID == nil || got.GroupVisibility != models.VisibilityInviteOnly {
				t.Fatalf("an ended date's edit base: %s", raw)
			}
			for _, key := range []string{"attendees", "removedRegistrations", "keptCount"} {
				if slices.Contains(managedKeys(t, raw), key) {
					t.Fatalf("an ended date carries %s: %s", key, raw)
				}
			}
			for _, s := range names {
				if strings.Contains(raw, s) {
					t.Fatalf("an ended date's edit base shows %q: %s", s, raw)
				}
			}
		})
	}

	t.Run("the end on the read's clock", func(t *testing.T) {
		// One date ends two hours after the wall clock, one ended two hours
		// before it: each is judged on the injected clock alone.
		later, earlier := listed, ended
		setClock(t, &later, now.Add(time.Hour), now.Add(2*time.Hour))
		defer setClock(t, &later, listed.StartsAt, listed.EndsAt)
		clock := newMutableClock(f.app, later.EndsAt)
		defer func() { f.app.clock = nil }()
		roster := func(o models.EventOccurrence) bool {
			got, _ := managedRead(t, f.leader, f.event.ID, models.FormatCalendarDate(o.OriginalDate))
			return got.AttendeesVisible
		}
		if roster(later) {
			t.Fatal("a date the read's clock says has ended carries its roster")
		}
		clock.set(later.EndsAt.Add(-time.Microsecond))
		if !roster(later) {
			t.Fatal("a date a microsecond before its end lost its roster")
		}
		clock.set(now.Add(-3 * time.Hour))
		if !roster(earlier) {
			t.Fatal("a date that has not ended on the read's clock lost its roster")
		}
	})

	t.Run("care notes on the read's clock", func(t *testing.T) {
		// Three days ahead, outside the care window on the wall clock and
		// inside it on the read's.
		soon := createFeedEvent(t, f.admin, f.group, "Soon supper", feedStart(3), "")
		var o models.EventOccurrence
		gormMust(t, f.app.DB.Where("event_id = ?", soon.ID).First(&o))
		p := seedRosterPeople(t, f.app, f.group, soon.ID, o.ID, leader, f.adminUser)
		newMutableClock(f.app, o.StartsAt.Add(-time.Hour))
		defer func() { f.app.clock = nil }()
		got, raw := managedRead(t, f.leader, soon.ID, models.FormatCalendarDate(o.OriginalDate))
		wantRosterPeople(t, got, raw, p)
		f.app.clock = nil
		if _, raw := managedRead(t, f.leader, soon.ID, models.FormatCalendarDate(o.OriginalDate)); strings.Contains(raw, p.careNote) {
			t.Fatalf("care notes outside the window: %s", raw)
		}
	})

	t.Run("matches the event list", func(t *testing.T) {
		church := churchEventAt(t, f.admin, "Church supper", feedStart(10))
		var churchDate models.EventOccurrence
		gormMust(t, f.app.DB.Where("event_id = ?", church.ID).First(&churchDate))
		going(t, f.memberUser, churchDate.ID, models.RegGoing)
		gormMust(t, f.app.DB.Create(&models.Registration{UserID: &f.outsiders[1].ID, OccurrenceID: listed.ID, Status: models.RegDeclined, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
		for _, d := range []struct {
			name string
			c    *apiClient
			ev   uint
			date models.EventOccurrence
		}{{"group event, admin", f.admin, f.event.ID, listed}, {"group event, leader", f.leader, f.event.ID, listed}, {"church-wide", f.admin, church.ID, churchDate}} {
			date := models.FormatCalendarDate(d.date.OriginalDate)
			page, _ := getEvent(t, d.c, d.ev, http.StatusOK)
			fromList := occurrenceOn(t, page.Occurrences, date)
			got, raw := managedRead(t, d.c, d.ev, date)
			pick := func(o occurrenceJSON) []any {
				return []any{o.StartsLocal, o.DurationMinutes, o.Title, o.Location, o.Capacity, o.RemoteCapacity, o.RemoteAvailable,
					o.Visibility, o.VisibilityOverride, o.SeriesVisibility, o.ChurchWide, o.GroupID, o.GroupName, o.GroupVisibility,
					o.HostedBy, o.TZID, o.Overridden, o.Attendees, o.RemovedRegistrations, o.Registrations}
			}
			a, _ := json.Marshal(pick(fromList))
			b, _ := json.Marshal(pick(got))
			if !bytes.Equal(a, b) {
				t.Fatalf("%s: the managed read and the event list disagree:\nlist    %s\nmanaged %s", d.name, a, b)
			}
			if d.ev == church.ID && (got.GroupID != nil || got.HostedBy == "" || !got.ChurchWide) {
				t.Fatalf("%s label: %s", d.name, raw)
			}
			if d.ev != church.ID && got.GroupName == "" {
				t.Fatalf("%s label: %s", d.name, raw)
			}
		}
	})

	t.Run("refusals", func(t *testing.T) {
		apiDo(t, f.member, "GET", managedDatePath(f.event.ID, f.date(3)), nil, http.StatusForbidden)
		w := f.admin.do("GET", managedDatePath(f.event.ID, "2026-02-30"), nil, false)
		if w.Code != http.StatusBadRequest || answerCode(t, w) != "series_date_invalid" {
			t.Fatalf("a malformed date: %d %s", w.Code, w.Body.String())
		}
	})
}

// TestRosterMarksKeptDatesForManagersOnly (DEC-156): a row whose live grant
// ends with its date says keptAfterLeaving to the event's managers, an admin
// and a leader who is not one, on the event list, the group list and the
// exact-date read, a teen's kept row included; every other row says false. No
// one else ever gets the key: not a member who sees the names, not the person,
// not a feed. Once Remove ends the grant the row says false. The managed read's
// keptCount is the save's 409 count, and the flagged rows are as many.
func TestRosterMarksKeptDatesForManagersOnly(t *testing.T) {
	f := newDepartureReadFixture(t, models.VisibilityInviteOnly, models.RegGoing, policy.OccurrenceOriginDepartureSelf)
	target := f.dates[1]
	date := models.FormatCalendarDate(target.OriginalDate)
	teen := activeTeen(t, f.app, "kept teen", f.issuer.ID)
	teenReg, err := keepDateFor(f.app, f.group, target.ID, teen, policy.OccurrenceOriginDepartureRemoval, f.issuer.ID)
	gormMust(t, &gorm.DB{Error: err})
	if r := loadAccessRow(t, f.app.DB, teenReg); r.Status != policy.OccurrenceAccessActive {
		t.Fatalf("fixture: the teen's grant is not live: %+v", r)
	}
	member := goingMember(t, f, target.ID)
	memberC := loginAPI(t, f.app, member.Name, "password1234")
	seedReadInvitation(t, f.app, f.issuer, f.other, f.event, target)
	_, leader := groupLeader(t, f.app, f.group, "kept roster leader")
	newMutableClock(f.app, target.EndsAt.Add(-time.Hour))

	flags := func(raw string) (kept, other int) {
		return strings.Count(raw, `"keptAfterLeaving":true`), strings.Count(raw, `"keptAfterLeaving":false`)
	}
	lists := []string{"/api/v1/events/" + itoa(f.event.ID), "/api/v1/groups/" + itoa(f.group) + "/events"}
	for _, c := range []struct {
		name string
		c    *apiClient
	}{{"admin", f.admin}, {"leader", leader}} {
		for _, path := range lists {
			if kept, other := flags(apiDo(t, c.c, "GET", path, nil, http.StatusOK)); kept != 2 || other != 2 {
				t.Fatalf("%s's %s flags %d kept and %d other rows, want 2 and 2", c.name, path, kept, other)
			}
		}
		got := wantManagedKept(t, c.c, f.event.ID, date, 2)
		for _, row := range got.Attendees {
			isKept := row.UserID != nil && (*row.UserID == f.recipient.ID || *row.UserID == teen.ID)
			if *row.KeptAfterLeaving != isKept {
				t.Fatalf("%s: row of %s says keptAfterLeaving %t", c.name, row.Name, *row.KeptAfterLeaving)
			}
		}
		// The save's refusal counts the same two grants.
		wantKeptDates(t, awaitResponse(t, dateEditRoutes[0].run(t, occurrenceReadFixture{app: f.app, admin: c.c, event: f.event}, date,
			map[string]any{"durationMinutes": int(target.EndsAt.Sub(target.StartsAt)/time.Minute) + 30})), keptDateCountJSON{Date: date, KeptCount: 2})
	}

	// Everyone else: a member who sees the names, the person themselves, the
	// person invited by a leader, and the person's feed.
	memberList := apiDo(t, memberC, "GET", lists[0], nil, http.StatusOK)
	if !strings.Contains(memberList, f.recipient.PublicName()) {
		t.Fatalf("fixture: the member does not see the names: %s", memberList)
	}
	for _, read := range []struct {
		name, raw string
	}{
		{"member's event list", memberList},
		{"member's group list", apiDo(t, memberC, "GET", lists[1], nil, http.StatusOK)},
		{"person's date page", apiDo(t, f.client, "GET", dateReadPath(f.event.ID, date), nil, http.StatusOK)},
		{"invited person's date page", apiDo(t, f.stranger, "GET", dateReadPath(f.event.ID, date), nil, http.StatusOK)},
		{"person's feed", apiDo(t, f.client, "GET", personalFeedPath(t, f.client, models.CalendarFeedMine), nil, http.StatusOK)},
		{"member's feed", apiDo(t, memberC, "GET", personalFeedPath(t, memberC, models.CalendarFeedAll), nil, http.StatusOK)},
	} {
		if strings.Contains(read.raw, "keptAfterLeaving") || strings.Contains(read.raw, "keptCount") {
			t.Fatalf("the %s carries the managers' kept facts: %s", read.name, read.raw)
		}
	}

	// After Remove the person's row is removed and says false; the teen's
	// still counts, until Remove ends it too.
	if w := removeKept(f, date); w.Code != http.StatusOK {
		t.Fatalf("Remove: %d %s", w.Code, w.Body.String())
	}
	got := wantManagedKept(t, leader, f.event.ID, date, 1)
	removed := false
	for _, row := range got.RemovedRegistrations {
		if row.UserID != nil && *row.UserID == f.recipient.ID {
			removed = !*row.KeptAfterLeaving
		}
	}
	if !removed {
		t.Fatalf("the removed row should say keptAfterLeaving false: %+v", got.RemovedRegistrations)
	}
	apiDo(t, leader, "POST", "/api/v1/events/"+itoa(f.event.ID)+"/registrations/remove", map[string]any{"date": date, "registrationId": teenReg}, http.StatusOK)
	wantManagedKept(t, f.admin, f.event.ID, date, 0)
}

// TestManagedDateRosterReadsAuthorityInItsSnapshot: the manager's exact-date
// read judges its authority and reads its roster in one snapshot. Right after
// the judgment another connection demotes the leader, signs a new person up
// and leaves a new kept grant, and commits: the answer is the state before
// that commit, and the next read is refused.
func TestManagedDateRosterReadsAuthorityInItsSnapshot(t *testing.T) {
	f := newDepartureReadFixture(t, models.VisibilityInviteOnly, models.RegGoing, policy.OccurrenceOriginDepartureSelf)
	target := f.dates[1]
	date := models.FormatCalendarDate(target.OriginalDate)
	leaderUser, leader := groupLeader(t, f.app, f.group, "snapshot leader")
	joiner := newTestUser(t, f.app, "seam joiner", "password1234", models.RoleMember)
	leaver := newTestUser(t, f.app, "seam leaver", "password1234", models.RoleMember)
	var fired atomic.Bool
	f.app.hooks.managedDateStep = func(step string) {
		if step != "authorized" || fired.Swap(true) {
			return
		}
		if err := f.app.DB.Model(&models.Membership{}).Where("user_id = ? AND group_id = ?", leaderUser.ID, f.group).Update("role", models.MembershipMember).Error; err != nil {
			t.Error(err)
		}
		if err := f.app.DB.Create(&models.Registration{UserID: &joiner.ID, OccurrenceID: target.ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}).Error; err != nil {
			t.Error(err)
		}
		if _, err := keepDateFor(f.app, f.group, target.ID, leaver, policy.OccurrenceOriginDepartureSelf, leaver.ID); err != nil {
			t.Error(err)
		}
	}
	got, raw := managedRead(t, leader, f.event.ID, date)
	if !fired.Load() {
		t.Fatal("the read never reached its seam")
	}
	if !got.AttendeesVisible || got.KeptCount == nil || *got.KeptCount != 1 || len(got.Attendees) != 1 || !*got.Attendees[0].KeptAfterLeaving {
		t.Fatalf("the read straddled the commit: %s", raw)
	}
	for _, s := range []string{joiner.PublicName(), leaver.PublicName()} {
		if strings.Contains(raw, s) {
			t.Fatalf("the read shows %q, committed after its judgment: %s", s, raw)
		}
	}
	if n, err := keptDatesIn(f.app.DB, []uint{target.ID}); err != nil || n[target.ID] != 2 {
		t.Fatalf("fixture: the seam's grant did not commit: %v %v", n, err)
	}
	apiDo(t, leader, "GET", managedDatePath(f.event.ID, date), nil, http.StatusForbidden)
}

// TestManagedDateHouseholdAgeFollowsTheSnapshotClock: the roster decides who
// sees a household person, and the age it shows, on the church's date at the
// read's one clock, with the zone read in its snapshot. An adult-kind person
// turns 18 on church date B: the day before, a leader does not see them and
// the admin sees 17; on B, the leader sees them and the admin sees 18. A
// zone change committed after the read's judgment changes nothing.
func TestManagedDateHouseholdAgeFollowsTheSnapshotClock(t *testing.T) {
	app := testApp(t)
	la, err := time.LoadLocation("America/Los_Angeles")
	gormMust(t, &gorm.DB{Error: err})
	gormMust(t, &gorm.DB{Error: setSettingIn(app.DB, settingTimezone, "America/Los_Angeles")})
	adminUser := newTestUser(t, app, "birthday admin", "password1234", models.RoleAdmin)
	admin := loginAPI(t, app, adminUser.Name, "password1234")
	group := createFeedGroup(t, admin, "Birthday group", models.VisibilityInviteOnly, "Hall")
	_, leader := groupLeader(t, app, group, "birthday leader")
	ev := createFeedEvent(t, admin, group, "Birthday supper", feedStart(5), "")
	var occ models.EventOccurrence
	gormMust(t, app.DB.Where("event_id = ?", ev.ID).First(&occ))
	controlUser := newTestUser(t, app, "birthday control", "password1234", models.RoleMember)
	gormMust(t, app.DB.Create(&models.Registration{UserID: &controlUser.ID, OccurrenceID: occ.ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
	holder := newTestUser(t, app, "birthday holder", "password1234", models.RoleMember)
	h := models.Household{Name: "Birthday household", CreatedBy: holder.ID}
	gormMust(t, app.DB.Create(&h))
	gormMust(t, app.DB.Create(&models.HouseholdAccount{HouseholdID: h.ID, UserID: holder.ID, Relation: models.HouseholdRelationAdult}))
	birth := time.Now().UTC().AddDate(-30, 0, 0)
	person := models.HouseholdPerson{HouseholdID: h.ID, FirstName: "Bea", LastName: "Birthdayperson", Kind: models.HouseholdAdult, BirthDate: &birth, CreatedBy: holder.ID}
	gormMust(t, app.DB.Create(&person))
	gormMust(t, app.DB.Create(&models.Registration{HouseholdPersonID: &person.ID, RegisteredBy: &holder.ID, OccurrenceID: occ.ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}))

	wall := time.Now().In(la)
	day := func(offset int) time.Time {
		return time.Date(wall.Year(), wall.Month(), wall.Day()+offset, 0, 0, 0, 0, la)
	}
	// set pins the read's clock at, turns the person 18 on b, and puts the
	// date an hour after the clock, for two hours.
	set := func(t *testing.T, at time.Time, b time.Time) {
		t.Helper()
		if b.Month() == time.February && b.Day() == 29 {
			t.Skip("an 18th birthday on February 29 has no 18-years-earlier date")
		}
		born := time.Date(b.Year()-18, b.Month(), b.Day(), 0, 0, 0, 0, time.UTC)
		gormMust(t, app.DB.Model(&models.HouseholdPerson{}).Where("id = ?", person.ID).Update("birth_date", born))
		gormMust(t, app.DB.Model(&models.EventOccurrence{}).Where("id = ?", occ.ID).Updates(map[string]any{"starts_at": at.Add(time.Hour).UTC(), "ends_at": at.Add(3 * time.Hour).UTC()}))
		newMutableClock(app, at)
	}
	date := models.FormatCalendarDate(occ.OriginalDate)
	want := func(t *testing.T, label string, leaderSees bool, age int) {
		t.Helper()
		lead, raw := managedRead(t, leader, ev.ID, date)
		if !lead.AttendeesVisible || !strings.Contains(raw, controlUser.PublicName()) {
			t.Fatalf("%s: the leader's read has no roster: %s", label, raw)
		}
		if strings.Contains(raw, person.FirstName) != leaderSees {
			t.Fatalf("%s: the leader sees the person %t, want %t: %s", label, !leaderSees, leaderSees, raw)
		}
		adm, raw := managedRead(t, admin, ev.ID, date)
		var seen *householdAttendeeJSON
		for _, row := range adm.Attendees {
			if row.HouseholdPerson != nil && row.HouseholdPerson.PersonID == person.ID {
				seen = row.HouseholdPerson
			}
		}
		if !adm.AttendeesVisible || seen == nil || seen.Age == nil || *seen.Age != age {
			t.Fatalf("%s: the admin's read, want age %d: %s", label, age, raw)
		}
	}

	t.Run("the day before, at church midnight", func(t *testing.T) {
		// 23:30 in Los Angeles on W-2 is already W-1 (the birthday) in UTC.
		set(t, day(-2).Add(23*time.Hour+30*time.Minute), day(-1))
		want(t, "before", false, 17)
	})
	t.Run("on the birthday", func(t *testing.T) {
		set(t, day(2).Add(30*time.Minute), day(2))
		want(t, "birthday", true, 18)
	})
	t.Run("the zone is read in the snapshot", func(t *testing.T) {
		set(t, day(-2).Add(23*time.Hour+30*time.Minute), day(-1))
		defer func() {
			app.hooks.managedDateStep = nil
			gormMust(t, &gorm.DB{Error: setSettingIn(app.DB, settingTimezone, "America/Los_Angeles")})
		}()
		var reads atomic.Int64
		app.hooks.managedDateStep = func(string) {
			reads.Add(1)
			// Auckland's date at this instant is already the birthday.
			if err := setSettingIn(app.DB, settingTimezone, "Pacific/Auckland"); err != nil {
				t.Error(err)
			}
		}
		lead, raw := managedRead(t, leader, ev.ID, date)
		if !lead.AttendeesVisible || strings.Contains(raw, person.FirstName) {
			t.Fatalf("the leader's read took the zone committed after its judgment: %s", raw)
		}
		gormMust(t, &gorm.DB{Error: setSettingIn(app.DB, settingTimezone, "America/Los_Angeles")})
		adm, raw := managedRead(t, admin, ev.ID, date)
		for _, row := range adm.Attendees {
			if row.HouseholdPerson != nil && (row.HouseholdPerson.Age == nil || *row.HouseholdPerson.Age != 17) {
				t.Fatalf("the admin's read took the zone committed after its judgment: %s", raw)
			}
		}
		if reads.Load() != 2 {
			t.Fatalf("the seam ran %d times", reads.Load())
		}
	})
}

// TestManagedDateRefusesABadChurchZone: a stored church zone that is no zone
// fails the roster read (fail's 500, nothing but the server error) rather than deciding a
// household person's visibility on UTC. A cancelled date, which builds no
// roster, answers exactly as it does with a good zone.
func TestManagedDateRefusesABadChurchZone(t *testing.T) {
	f := newDateEditFixture(t)
	p := seedRosterPeople(t, f.app, f.group, f.event.ID, f.dates[1].ID, f.leaderUser, f.adminUser)
	cancelled := f.dates[2]
	gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", cancelled.ID).Updates(map[string]any{"status": models.OccurrenceCancelled, "cancel_source": "manual"}))
	good := apiDo(t, f.leader, "GET", managedDatePath(f.event.ID, f.date(2)), nil, http.StatusOK)
	if got, raw := managedRead(t, f.leader, f.event.ID, f.date(1)); !got.AttendeesVisible || !strings.Contains(raw, p.childName) {
		t.Fatalf("control: the roster with a good zone: %s", raw)
	}
	gormMust(t, &gorm.DB{Error: setSettingIn(f.app.DB, settingTimezone, "Not/AZone")})
	w := f.leader.do("GET", managedDatePath(f.event.ID, f.date(1)), nil, false)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("the roster read with a bad zone: %d %s", w.Code, w.Body.String())
	}
	var body map[string]json.RawMessage
	gormMust(t, &gorm.DB{Error: json.Unmarshal(w.Body.Bytes(), &body)})
	if _, ok := body["error"]; !ok || len(body) != 2 || string(body["code"]) != `"server_error"` {
		t.Fatalf("the failure says more than fail's server error: %s", w.Body.String())
	}
	for _, s := range []string{p.childName, p.teen.PublicName(), p.holderName, f.memberUser.PublicName()} {
		if strings.Contains(w.Body.String(), s) {
			t.Fatalf("the failure shows %q: %s", s, w.Body.String())
		}
	}
	if bad := apiDo(t, f.leader, "GET", managedDatePath(f.event.ID, f.date(2)), nil, http.StatusOK); bad != good {
		t.Fatalf("the cancelled date's read changed with the zone:\n%s\n%s", good, bad)
	}
}

// dateEditConsent is the consent a page sends after reviewing changes to one
// date: the date preview's token for them, read as c (DEC-158).
func dateEditConsent(t *testing.T, c *apiClient, eventID uint, date string, changes map[string]any) string {
	t.Helper()
	path := "/api/v1/events/" + itoa(eventID) + "/occurrences/" + date
	return confirmVisibility(t, c, path, changes)["consent"].(string)
}

// consentOf is the token a 409 decision (needsDecision or date_needs_consent)
// carries in its impact.
func consentOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Impact visibilityImpactJSON `json:"impact"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || !consent.WellFormed(body.Impact.Consent) {
		t.Fatalf("a decision without a consent token: %s (%v)", w.Body.String(), err)
	}
	return body.Impact.Consent
}

// TestManagedDateReadAcknowledgesItsSnapshotViewer: the manager's exact-date
// read decides every refusal in its snapshot, in the edit's order (hidden
// 404, 403, malformed date, missing date), so a participating read (M8.1) is
// acknowledged for the viewer judged there on its answer and on each refusal,
// a missing event exactly like a hidden one. A plain read carries none; a
// stale session's 401 and a failed read none either. A demotion committed
// after the judgment leaves the answer and its acknowledgment the snapshot's.
func TestManagedDateReadAcknowledgesItsSnapshotViewer(t *testing.T) {
	f := newDateEditFixture(t)
	admin, leader, member := pinnedAs(t, f.admin), pinnedAs(t, f.leader), pinnedAs(t, f.member)
	stranger := pinnedAs(t, loginAPI(t, f.app, f.outsiders[0].Name, "password1234"))
	path := managedDatePath(f.event.ID, f.date(0))

	plain := f.admin.do("GET", path, nil, false)
	wantNoAck(t, "a plain managed read", plain)
	w := admin.do("GET", path, nil, false)
	if w.Code != http.StatusOK || w.Body.String() != plain.Body.String() {
		t.Fatalf("the managed read: %d, the same body as a plain read %t", w.Code, w.Body.String() == plain.Body.String())
	}
	wantAck(t, "the managed read", w, admin, f.adminUser)

	hidden := stranger.do("GET", path, nil, false)
	missing := stranger.do("GET", managedDatePath(999999, f.date(0)), nil, false)
	if hidden.Code != http.StatusNotFound || missing.Code != hidden.Code || missing.Body.String() != hidden.Body.String() {
		t.Fatalf("hidden %d %s, missing %d %s", hidden.Code, hidden.Body.String(), missing.Code, missing.Body.String())
	}
	wantAck(t, "a hidden event", hidden, stranger, f.outsiders[0])
	wantAck(t, "a missing event", missing, stranger, f.outsiders[0])
	for _, c := range []struct {
		name   string
		client *apiClient
		viewer *models.User
		date   string
		status int
	}{
		{"a member", member, f.memberUser, f.date(0), http.StatusForbidden},
		{"a member's malformed date", member, f.memberUser, "not-a-date", http.StatusForbidden},
		{"a stranger's malformed date", stranger, f.outsiders[0], "not-a-date", http.StatusNotFound},
		{"a malformed date", admin, f.adminUser, "not-a-date", errSeriesDateInvalid.Status},
		{"a missing date", admin, f.adminUser, "2001-01-01", http.StatusNotFound},
	} {
		w := c.client.do("GET", managedDatePath(f.event.ID, c.date), nil, false)
		if w.Code != c.status {
			t.Fatalf("%s: %d %s, want %d", c.name, w.Code, w.Body.String(), c.status)
		}
		wantAck(t, c.name, w, c.client, c.viewer)
	}

	before := apiDo(t, leader, "GET", path, nil, http.StatusOK)
	f.app.hooks.managedDateStep = func(step string) {
		f.app.hooks.managedDateStep = nil
		gormMust(t, f.app.DB.Model(&models.Membership{}).Where("user_id = ? AND group_id = ?", f.leaderUser.ID, f.group).Update("role", models.MembershipMember))
	}
	w = leader.do("GET", path, nil, false)
	if w.Code != http.StatusOK || w.Body.String() != before {
		t.Fatalf("the managed read straddled a demotion: %d\nbefore %s\ngot    %s", w.Code, before, w.Body.String())
	}
	wantAck(t, "the snapshot's managed read", w, leader, f.leaderUser)
	w = leader.do("GET", path, nil, false)
	if w.Code != http.StatusForbidden {
		t.Fatalf("the demoted leader's next read: %d %s", w.Code, w.Body.String())
	}
	wantAck(t, "the demoted leader's refusal", w, leader, f.leaderUser)

	n := failStatement(t, f.app, func(tx *gorm.DB) bool { return selects(tx, "count(*) as n") })
	w = admin.do("GET", path, nil, false)
	if n.Load() == 0 || w.Code != http.StatusInternalServerError || w.Body.String() != serverErrorBody {
		t.Fatalf("a failed count (failed %d): %d %s", n.Load(), w.Code, w.Body.String())
	}
	wantNoAck(t, "a failed managed read", w)

	gormMust(t, f.app.DB.Model(&models.User{}).Where("id = ?", f.adminUser.ID).Update("session_version", gorm.Expr("session_version + 1")))
	w = admin.do("GET", path, nil, false)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("a stale session: %d %s", w.Code, w.Body.String())
	}
	wantNoAck(t, "a stale session", w)
}

// TestDateEditAcknowledgesTheWritersOutcome: a participating guarded edit
// (M8.1) is acknowledged for the actor its writer locked, only once the
// outcome is known: an applied edit, a settled replay, and the writer's
// refusals whose rollback was confirmed (a stale base, a start the writer
// cannot parse). A failed edit, an unconfirmed rollback and a failed COMMIT
// carry no acknowledgment, nor do the refusals made before the writer (the
// preflight's 403, a malformed body), which no locked actor decided.
func TestDateEditAcknowledgesTheWritersOutcome(t *testing.T) {
	f := newDateEditFixture(t)
	admin, member := pinnedAs(t, f.admin), pinnedAs(t, f.member)
	date := f.date(0)

	base := editBaseOf(t, admin, f.event.ID, date)
	applied := dateEditBody(newDateAttempt(t), base.Revision, map[string]any{"title": "Acknowledged title"})
	w := wantDateEdit(t, admin, f.event.ID, date, applied, http.StatusOK, "")
	wantAck(t, "an applied edit", w, admin, f.adminUser)
	w = wantDateEdit(t, admin, f.event.ID, date, applied, http.StatusOK, "")
	wantAck(t, "a settled replay", w, admin, f.adminUser)
	w = wantDateEdit(t, admin, f.event.ID, date, dateEditBody(newDateAttempt(t), base.Revision, map[string]any{"title": "Stale"}), http.StatusConflict, "date_changed")
	wantAck(t, "a stale base", w, admin, f.adminUser)
	base = editBaseOf(t, admin, f.event.ID, date)
	w = postDateEdit(admin, f.event.ID, date, dateEditBody(newDateAttempt(t), base.Revision, map[string]any{"startsLocal": "not a time"}))
	if w.Code < 400 || w.Code >= 500 {
		t.Fatalf("an unparsable start: %d %s", w.Code, w.Body.String())
	}
	wantAck(t, "the writer's field refusal", w, admin, f.adminUser)

	w = wantDateEdit(t, member, f.event.ID, date, dateEditBody(newDateAttempt(t), base.Revision, map[string]any{"title": "x"}), http.StatusForbidden, "")
	wantNoAck(t, "the preflight's refusal", w)
	w = postDateEdit(admin, f.event.ID, date, map[string]any{"attempt": "nope"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("a malformed body: %d %s", w.Code, w.Body.String())
	}
	wantNoAck(t, "a malformed body", w)
	plain := wantDateEdit(t, f.admin, f.event.ID, date, dateEditBody(newDateAttempt(t), base.Revision, map[string]any{"title": "Plain title"}), http.StatusOK, "")
	wantNoAck(t, "a plain edit", plain)

	for _, c := range []struct {
		name string
		step func(tx *gorm.DB) error
	}{
		{"a failed edit", func(*gorm.DB) error { return errors.New("late failure") }},
		{"an unconfirmed rollback", func(tx *gorm.DB) error { endBackend(t, f.app, tx); return errDateChanged }},
		{"a failed commit", func(tx *gorm.DB) error { endBackend(t, f.app, tx); return nil }},
	} {
		t.Run(c.name, func(t *testing.T) {
			base := editBaseOf(t, admin, f.event.ID, date)
			state := f.state(t)
			var fired atomic.Bool
			f.app.hooks.dateEditStep = func(tx *gorm.DB, step string) error {
				if step != "receipt" || !fired.CompareAndSwap(false, true) {
					return nil
				}
				return c.step(tx)
			}
			w := postDateEdit(admin, f.event.ID, date, dateEditBody(newDateAttempt(t), base.Revision, map[string]any{"title": "Never acknowledged"}))
			f.app.hooks.dateEditStep = nil
			if !fired.Load() || w.Code == http.StatusOK {
				t.Fatalf("%s (fired %t): %d %s", c.name, fired.Load(), w.Code, w.Body.String())
			}
			t.Logf("%s answered %d %s", c.name, w.Code, w.Body.String())
			wantNoAck(t, c.name, w)
			if after := f.state(t); after != state {
				t.Fatalf("%s wrote", c.name)
			}
		})
	}
}

// supersededKey reads a 409 date_changed answer's baseSuperseded key: absent
// (false, false), or present with its value.
func supersededKey(t *testing.T, w *httptest.ResponseRecorder) (value, present bool) {
	t.Helper()
	var body map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("answer %q: %v", w.Body.String(), err)
	}
	raw, ok := body["baseSuperseded"]
	if !ok {
		return false, false
	}
	if string(raw) != "true" {
		t.Fatalf("baseSuperseded is %s, never anything but true: %s", raw, w.Body.String())
	}
	var shape dateChangedJSON
	decodeJSON(t, w, &shape)
	return shape.BaseSuperseded, true
}

func wantSuperseded(t *testing.T, what string, w *httptest.ResponseRecorder, want bool) {
	t.Helper()
	if w.Code != http.StatusConflict || answerCode(t, w) != "date_changed" {
		t.Fatalf("%s: %d %s, want 409 date_changed", what, w.Code, w.Body.String())
	}
	if got, present := supersededKey(t, w); got != want || present != want {
		t.Fatalf("%s: baseSuperseded %t (present %t), want %t: %s", what, got, present, want, w.Body.String())
	}
}

// withCounters is token with its occurrence id and counters replaced.
func withCounters(t *testing.T, token string, occ uint, eventRev, dateRev int64) string {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 6 {
		t.Fatalf("token %q", token)
	}
	parts[2], parts[3], parts[4] = fmt.Sprint(occ), fmt.Sprint(eventRev), fmt.Sprint(dateRev)
	return strings.Join(parts, ".")
}

// TestBaseSupersededIsProvedOnlyByOlderCounters: the flag needs the same
// occurrence, no counter ahead and one behind; a place tag never decides it,
// and nothing that does not parse does.
func TestBaseSupersededIsProvedOnlyByOlderCounters(t *testing.T) {
	const gen = "0123456789abcdef"
	tok := func(occ, ev, date string, place string) string {
		return "1." + gen + "." + occ + "." + ev + "." + date + "." + place
	}
	for _, c := range []struct {
		name string
		base string
		want bool
	}{
		{"an older date counter", tok("7", "3", "4", "-"), true},
		{"an older event counter", tok("7", "2", "5", "-"), true},
		{"both older", tok("7", "1", "1", "aaaaaaaaaaaaaaaa"), true},
		{"the same counters, another place", tok("7", "3", "5", "aaaaaaaaaaaaaaaa"), false},
		{"the same counters", tok("7", "3", "5", "-"), false},
		{"a date counter ahead", tok("7", "2", "6", "-"), false},
		{"an event counter ahead", tok("7", "4", "4", "-"), false},
		{"another occurrence", tok("8", "1", "1", "-"), false},
		{"a counter past uint64", tok("7", "18446744073709551616", "1", "-"), false},
		{"a short token", "1." + gen + ".7.3", false},
		{"not numbers", tok("7", "x", "1", "-"), false},
	} {
		if got := baseSuperseded(c.base, 7, 3, 5); got != c.want {
			t.Errorf("%s (%s): %t, want %t", c.name, c.base, got, c.want)
		}
	}
	if baseSuperseded(tok("7", "1", "1", "-"), 7, -1, 5) {
		t.Error("a negative stored counter proved a supersession")
	}
}

// TestDateEditSignalsASupersededBase: date_changed carries baseSuperseded
// only when the locked check proves the base older than the date, and the
// answer is still the writer's acknowledged decision. Every other
// date_changed, and every refusal before the writer, leaves the key out.
func TestDateEditSignalsASupersededBase(t *testing.T) {
	f := newDateEditFixture(t)
	admin := pinnedAs(t, f.admin)
	date := f.date(0)
	edit := func(base string, changes map[string]any) *httptest.ResponseRecorder {
		return postDateEdit(admin, f.event.ID, date, dateEditBody(newDateAttempt(t), base, changes))
	}

	t.Run("a later date edit", func(t *testing.T) {
		base := editBaseOf(t, admin, f.event.ID, date)
		wantDateEdit(t, admin, f.event.ID, date, dateEditBody(newDateAttempt(t), base.Revision, map[string]any{"title": "Moved on"}), http.StatusOK, "")
		state := f.state(t)
		w := edit(base.Revision, map[string]any{"title": "Stale"})
		wantSuperseded(t, "a stale date base", w, true)
		wantAck(t, "a superseded base", w, admin, f.adminUser)
		if f.state(t) != state {
			t.Fatal("a superseded base wrote")
		}
	})
	t.Run("a series edit", func(t *testing.T) {
		base := editBaseOf(t, admin, f.event.ID, date)
		apiDo(t, f.admin, "PUT", fmt.Sprintf("/api/v1/events/%d", f.event.ID), seriesBody(t, f.admin, fmt.Sprintf("/api/v1/events/%d", f.event.ID), map[string]any{"title": "Supper again"}), http.StatusOK)
		wantSuperseded(t, "a stale series base", edit(base.Revision, map[string]any{"title": "Stale"}), true)
	})
	t.Run("a replay after a later edit", func(t *testing.T) {
		base := editBaseOf(t, admin, f.event.ID, date)
		applied := dateEditBody(newDateAttempt(t), base.Revision, map[string]any{"title": "Applied once"})
		wantDateEdit(t, admin, f.event.ID, date, applied, http.StatusOK, "")
		next := editBaseOf(t, admin, f.event.ID, date)
		wantDateEdit(t, admin, f.event.ID, date, dateEditBody(newDateAttempt(t), next.Revision, map[string]any{"title": "Later"}), http.StatusOK, "")
		wantSuperseded(t, "the replay", postDateEdit(admin, f.event.ID, date, applied), true)
	})
	t.Run("the inherited place alone", func(t *testing.T) {
		base := editBaseOf(t, admin, f.event.ID, date)
		apiDo(t, f.admin, "PUT", "/api/v1/groups/"+itoa(f.group), map[string]any{"location": "Chapel " + newDateAttempt(t)[:6]}, http.StatusOK)
		wantSuperseded(t, "a place-only change", edit(base.Revision, map[string]any{"title": "Stale"}), false)
	})
	t.Run("counters ahead or another occurrence", func(t *testing.T) {
		base := editBaseOf(t, admin, f.event.ID, date)
		o := occurrenceRow(t, f.app, f.dates[0].ID)
		var evRev int64
		gormMust(t, f.app.DB.Raw("SELECT edit_revision FROM events WHERE id = ?", f.event.ID).Scan(&evRev))
		for name, tok := range map[string]string{
			"a date counter ahead":   withCounters(t, base.Revision, o.ID, evRev, o.EditRevision+3),
			"an event counter ahead": withCounters(t, base.Revision, o.ID, evRev+1, 1),
			"another occurrence":     withCounters(t, base.Revision, f.dates[1].ID, 1, 1),
		} {
			wantSuperseded(t, name, edit(tok, map[string]any{"title": "Forged"}), false)
		}
	})
	t.Run("refusals before the writer", func(t *testing.T) {
		base := editBaseOf(t, admin, f.event.ID, date)
		w := postDateEdit(admin, f.event.ID, date, map[string]any{"attempt": newDateAttempt(t), "revision": base.Revision})
		if w.Code != http.StatusBadRequest {
			t.Fatalf("a body with no changes: %d %s", w.Code, w.Body.String())
		}
		if _, present := supersededKey(t, w); present {
			t.Fatalf("a malformed body carried the flag: %s", w.Body.String())
		}
		f.app.fence.detected.Store(true)
		defer f.app.fence.detected.Store(false)
		w = edit(withCounters(t, base.Revision, f.dates[0].ID, 1, 1), map[string]any{"title": "x"})
		if answerCode(t, w) != "restore_detected" {
			t.Fatalf("a detected restore: %d %s", w.Code, w.Body.String())
		}
		if _, present := supersededKey(t, w); present {
			t.Fatalf("a detected restore carried the flag: %s", w.Body.String())
		}
	})
}

// TestDateEditSupersessionAcrossProcesses: a base another process issued is
// never proved superseded, whatever its counters; the process that issued it
// proves it once the date moves on, even when the change came from the other
// process.
func TestDateEditSupersessionAcrossProcesses(t *testing.T) {
	f := newDateEditFixture(t)
	date := f.date(0)
	base := editBaseOf(t, f.admin, f.event.ID, date)
	stale := dateEditBody(newDateAttempt(t), base.Revision, map[string]any{"title": "From the first process"})
	app2 := freshProcess(t, f.app)
	c2 := loginAPI(t, app2, f.adminUser.Name, "password1234")
	wantSuperseded(t, "the first process's base on the second", postDateEdit(c2, f.event.ID, date, stale), false)
	b2 := editBaseOf(t, c2, f.event.ID, date)
	wantDateEdit(t, c2, f.event.ID, date, dateEditBody(newDateAttempt(t), b2.Revision, map[string]any{"title": "From the second process"}), http.StatusOK, "")
	wantSuperseded(t, "the second process's write proved on the first", postDateEdit(f.admin, f.event.ID, date, stale), true)
	wantSuperseded(t, "the second process's older base on the first", postDateEdit(f.admin, f.event.ID, date, dateEditBody(newDateAttempt(t), b2.Revision, map[string]any{"title": "x"})), false)
}

// TestDateEditHeldOriginalAcrossAnInheritedPlaceABA: the case the flag must
// never claim. A send held in delivery is based on the group's place A; the
// place changes to B, so the same bytes sent again answer date_changed with
// no flag, and a fresh read shows a different base. The place goes back to A
// and the held original applies after all. Had the browser taken the
// conflict and the different read as proof, it would have started a second
// edit while the first was still on its way. The replay after it is a settled
// 200 with one set of effects.
func TestDateEditHeldOriginalAcrossAnInheritedPlaceABA(t *testing.T) {
	f := newDateEditFixture(t)
	gid := itoa(f.group)
	date := f.date(0)
	gormMust(t, f.app.DB.Create(&models.Registration{UserID: &f.memberUser.ID, OccurrenceID: f.dates[0].ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
	base := editBaseOf(t, f.admin, f.event.ID, date)
	if base.Location != "Group hall" || strings.HasSuffix(base.Revision, ".-") {
		t.Fatalf("fixture: the date does not show the group's place: %+v", base)
	}
	body := dateEditBody(newDateAttempt(t), base.Revision, map[string]any{"startsLocal": f.dates[0].StartsAt.Add(time.Hour).Format("2006-01-02T15:04")})
	held := f.admin.doHeld("POST", dateEditPath(f.event.ID, date), body)

	apiDo(t, f.admin, "PUT", "/api/v1/groups/"+gid, map[string]any{"location": "Chapel"}, http.StatusOK)
	wantSuperseded(t, "the replay under place B", postDateEdit(f.admin, f.event.ID, date, body), false)
	if b := editBaseOf(t, f.admin, f.event.ID, date); b.Revision == base.Revision {
		t.Fatal("fixture: the read under place B has the original base")
	}
	apiDo(t, f.admin, "PUT", "/api/v1/groups/"+gid, map[string]any{"location": "Group hall"}, http.StatusOK)
	held.release()
	if w := held.wait(t); w.Code != http.StatusOK {
		t.Fatalf("the held original under place A again: %d %s", w.Code, w.Body.String())
	}
	if o := occurrenceRow(t, f.app, f.dates[0].ID); o.StartsAtOverride == nil || o.StartsAt.Equal(f.dates[0].StartsAt) || len(o.EditReceipt) != 32 {
		t.Fatalf("the held original did not apply: %+v", o)
	}
	state := f.state(t)
	notices := countOf(t, f.app, f.memberUser.ID, notify.EventChanged)
	if notices != 1 {
		t.Fatalf("the registrant got %d change notices, want 1", notices)
	}
	wantDateEdit(t, f.admin, f.event.ID, date, body, http.StatusOK, "")
	if f.state(t) != state || countOf(t, f.app, f.memberUser.ID, notify.EventChanged) != notices {
		t.Fatal("the replay after the original applied it a second time")
	}
}

// TestDateEditTakesTheBrowsersFrozenBytes keeps a tab still running the
// earlier release's client working: that client froze its body
// (DateEdits.freeze, since removed from web/src/api/events.ts), sent those
// bytes, appended the challenge's token to them, and replayed them byte for
// byte from "Try again". The handler reads them, they apply, and a settlement
// and its replay name the edited occurrence with a revision, which is what
// that client checked before it counted a send as stored.
func TestDateEditTakesTheBrowsersFrozenBytes(t *testing.T) {
	f := newDateEditFixture(t)
	admin := pinnedAs(t, f.admin)
	f.openWithOutsiders(t, 0)
	date := f.date(0)
	base := editBaseOf(t, admin, f.event.ID, date)
	attempt := newDateAttempt(t)
	frozen := `{"attempt":"` + attempt + `","revision":"` + base.Revision + `","changes":{"title":"Supper","capacityOverride":null,"visibility":"series"},"reason":"Too many"}`
	post := func(body string) *httptest.ResponseRecorder {
		return admin.do("POST", dateEditPath(f.event.ID, date), rawJSON(body), true)
	}
	w := post(frozen)
	if w.Code != http.StatusConflict || answerCode(t, w) != "date_needs_consent" {
		t.Fatalf("the frozen body: %d %s", w.Code, w.Body.String())
	}
	token := consentOf(t, w)
	consented := frozen[:len(frozen)-1] + `,"consent":"` + token + `"}`
	for _, what := range []string{"the consented body", "its replay"} {
		w = post(consented)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", what, w.Code, w.Body.String())
		}
		wantAck(t, what, w, admin, f.adminUser)
		got := answeredDate(t, w)
		if got.ID != f.dates[0].ID || got.Revision == "" || got.Title != "Supper" {
			t.Fatalf("%s answered %+v", what, got)
		}
	}
}

// wantDatePageOutdated checks w is the retired date PUT's refusal: 409
// date_page_outdated with only its error and code, so nothing of the date,
// its roster, an impact or a revision reaches an old page.
func wantDatePageOutdated(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	var body map[string]any
	if w.Code != http.StatusConflict || json.Unmarshal(w.Body.Bytes(), &body) != nil || len(body) != 2 ||
		body["code"] != errDatePageOutdated.Code || body["error"] != errDatePageOutdated.Message {
		t.Fatalf("want 409 date_page_outdated with only error and code: %d %s", w.Code, w.Body.String())
	}
}

// TestRetiredDatePutRefusesAfterItsChecks: the retired PUT
// /events/:id/occurrences/:date keeps CSRF and the session, then decides who
// reaches the date as the manager's exact-date read does, in one snapshot,
// before saying anything of the date: 403 without CSRF, 401 signed out, the
// same 404 for a hidden and a missing event, 403 for a viewer who can't
// manage it, then a malformed date (400) and a missing one (404). A manager
// reaching a date gets 409 date_page_outdated whatever the body, a body that
// would have tightened and renamed the date or one that can't be read, and
// nothing is written: the date and its revision and receipt, registrations,
// audit, notices and jobs. Refusals the snapshot decided are acknowledged for
// the viewer it judged, and a demotion or a move of the date committed after
// the judgment leaves the answer the snapshot's.
func TestRetiredDatePutRefusesAfterItsChecks(t *testing.T) {
	f := newDateEditFixture(t)
	f.openWithOutsiders(t, 1)
	admin, leader, member := pinnedAs(t, f.admin), pinnedAs(t, f.leader), pinnedAs(t, f.member)
	stranger := pinnedAs(t, loginAPI(t, f.app, f.outsiders[0].Name, "password1234"))
	put := func(c *apiClient, eventID uint, date string, body any, csrf bool) *httptest.ResponseRecorder {
		return c.do("PUT", managedDatePath(eventID, date), body, csrf)
	}
	tighten := map[string]any{"title": "RETIREDSECRETTITLE", "visibility": "series", "reason": "Back to the group",
		"consent": dateEditConsent(t, f.admin, f.event.ID, f.date(1), map[string]any{"title": "RETIREDSECRETTITLE", "visibility": "series"})}
	before := f.state(t)
	base := editBaseOf(t, f.admin, f.event.ID, f.date(1))

	if w := put(f.admin, f.event.ID, f.date(1), tighten, false); w.Code != http.StatusForbidden {
		t.Fatalf("without CSRF: %d %s", w.Code, w.Body.String())
	}
	if w := put(anonClient(t, f.app, "10.21.0.4"), f.event.ID, f.date(1), tighten, true); w.Code != http.StatusUnauthorized {
		t.Fatalf("signed out with a valid CSRF token: %d %s", w.Code, w.Body.String())
	}
	hidden := put(stranger, f.event.ID, f.date(1), tighten, true)
	missing := put(stranger, 999999, f.date(1), tighten, true)
	if hidden.Code != http.StatusNotFound || missing.Code != hidden.Code || missing.Body.String() != hidden.Body.String() {
		t.Fatalf("hidden %d %s, missing %d %s", hidden.Code, hidden.Body.String(), missing.Code, missing.Body.String())
	}
	wantAck(t, "a hidden event", hidden, stranger, f.outsiders[0])
	wantAck(t, "a missing event", missing, stranger, f.outsiders[0])
	for _, c := range []struct {
		name   string
		client *apiClient
		viewer *models.User
		date   string
		status int
	}{
		{"a member", member, f.memberUser, f.date(1), http.StatusForbidden},
		{"a member's malformed date", member, f.memberUser, "not-a-date", http.StatusForbidden},
		{"a member's missing date", member, f.memberUser, "2001-01-01", http.StatusForbidden},
		{"a stranger's malformed date", stranger, f.outsiders[0], "not-a-date", http.StatusNotFound},
		{"a stranger's missing date", stranger, f.outsiders[0], "2001-01-01", http.StatusNotFound},
		{"a malformed date", admin, f.adminUser, "not-a-date", http.StatusBadRequest},
		{"a missing date", admin, f.adminUser, "2001-01-01", http.StatusNotFound},
	} {
		w := put(c.client, f.event.ID, c.date, tighten, true)
		if w.Code != c.status {
			t.Fatalf("%s: %d %s, want %d", c.name, w.Code, w.Body.String(), c.status)
		}
		if c.status == http.StatusBadRequest && answerCode(t, w) != errSeriesDateInvalid.Code {
			t.Fatalf("%s: %s, want series_date_invalid", c.name, w.Body.String())
		}
		wantAck(t, c.name, w, c.client, c.viewer)
	}
	for name, body := range map[string]any{"a tightening and rename": tighten, "an unreadable body": rawJSON(`{"visibility":`)} {
		for _, c := range []struct {
			client *apiClient
			viewer *models.User
		}{{admin, f.adminUser}, {leader, f.leaderUser}} {
			w := put(c.client, f.event.ID, f.date(1), body, true)
			wantDatePageOutdated(t, w)
			wantAck(t, name, w, c.client, c.viewer)
			for _, secret := range []string{f.date(1), base.Revision, "RETIREDSECRETTITLE", "outsider", "impact", "revision"} {
				if strings.Contains(w.Body.String(), secret) {
					t.Fatalf("%s: the refusal names %q: %s", name, secret, w.Body.String())
				}
			}
		}
	}
	if after := f.state(t); after != before {
		t.Fatalf("the retired PUT wrote:\n%s\n%s", before, after)
	}
	if got := editBaseOf(t, f.admin, f.event.ID, f.date(1)); got.Revision != base.Revision {
		t.Fatalf("the retired PUT moved the revision: %s -> %s", base.Revision, got.Revision)
	}

	// A demotion committed after the snapshot judged the leader leaves the
	// answer and its acknowledgment the snapshot's; the next request is 403.
	f.app.hooks.managedDateStep = func(string) {
		f.app.hooks.managedDateStep = nil
		gormMust(t, f.app.DB.Model(&models.Membership{}).Where("user_id = ? AND group_id = ?", f.leaderUser.ID, f.group).Update("role", models.MembershipMember))
	}
	w := put(leader, f.event.ID, f.date(1), tighten, true)
	wantDatePageOutdated(t, w)
	wantAck(t, "the snapshot's refusal", w, leader, f.leaderUser)
	if w := put(leader, f.event.ID, f.date(1), tighten, true); w.Code != http.StatusForbidden {
		t.Fatalf("the demoted leader's next PUT: %d %s", w.Code, w.Body.String())
	}
	// A date moved off its day after the judgment is still found in the
	// snapshot; the next request no longer finds it.
	f.app.hooks.managedDateStep = func(string) {
		f.app.hooks.managedDateStep = nil
		gormMust(t, f.app.DB.Exec("UPDATE event_occurrences SET original_date = '2001-01-02' WHERE id = ?", f.dates[1].ID))
	}
	wantDatePageOutdated(t, put(admin, f.event.ID, f.date(1), tighten, true))
	if w := put(admin, f.event.ID, f.date(1), tighten, true); w.Code != http.StatusNotFound {
		t.Fatalf("the moved date's next PUT: %d %s", w.Code, w.Body.String())
	}
	if after := f.state(t); after == before {
		t.Fatal("fixture: the demotion and the move did not commit")
	}
	if n := auditCount(t, f.app, "registration.cancelled"); n != 0 {
		t.Fatalf("%d cancellations from the retired PUT", n)
	}
}
