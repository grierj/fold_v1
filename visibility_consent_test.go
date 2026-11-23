package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/token"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fold/internal/audit"
	"fold/internal/consent"
	"fold/internal/eventdates"
	"fold/internal/guardkit"
	"fold/internal/notify"
	"fold/internal/policy"
	"fold/internal/testdb"
	"fold/models"

	"gorm.io/gorm"
)

// The exact visibility consent rule (DEC-158), end to end through the routes.

type vcFixture struct {
	app   *App
	admin *apiClient
	gid   uint
	ev    eventJSON
	dates []models.EventOccurrence
	// declared collects every consent declaration checkConsentIn made.
	mu       sync.Mutex
	declared []string
}

func newVCFixture(t *testing.T, vis string) *vcFixture {
	t.Helper()
	app, admin, _, ev, dates := dateVisibilityFixture(t, vis)
	var g models.Group
	if err := app.DB.Where("id = (SELECT group_id FROM events WHERE id = ?)", ev.ID).First(&g).Error; err != nil {
		t.Fatal(err)
	}
	f := &vcFixture{app: app, admin: admin, gid: g.ID, ev: ev, dates: dates}
	app.hooks.consentDeclared = func(v string) {
		f.mu.Lock()
		f.declared = append(f.declared, v)
		f.mu.Unlock()
	}
	return f
}

func (f *vcFixture) takeDeclared() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.declared
	f.declared = nil
	return out
}

func (f *vcFixture) eventPath() string { return fmt.Sprintf("/api/v1/events/%d/visibility", f.ev.ID) }
func (f *vcFixture) groupPath() string { return fmt.Sprintf("/api/v1/groups/%d", f.gid) }
func (f *vcFixture) datePath(i int) string {
	return fmt.Sprintf("/api/v1/events/%d/occurrences/%s", f.ev.ID, f.day(i))
}

func (f *vcFixture) day(i int) string { return models.FormatCalendarDate(f.dates[i].OriginalDate) }

// outsider signs up a non-member for date i.
func (f *vcFixture) outsider(t *testing.T, name string, i int) *models.User {
	t.Helper()
	u := newTestUser(t, f.app, name, "password1234", models.RoleMember)
	if err := f.app.DB.Create(&models.Registration{UserID: &u.ID, OccurrenceID: f.dates[i].ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}).Error; err != nil {
		t.Fatal(err)
	}
	return u
}

func (f *vcFixture) setOverride(t *testing.T, i int, vis *string) {
	t.Helper()
	gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", f.dates[i].ID).Update("visibility_override", vis))
}

func (f *vcFixture) setSeries(t *testing.T, vis string) {
	t.Helper()
	gormMust(t, f.app.DB.Model(&models.Event{}).Where("id = ?", f.ev.ID).Update("visibility", vis))
}

func (f *vcFixture) override(t *testing.T, i int) string {
	t.Helper()
	var o models.EventOccurrence
	gormMust(t, f.app.DB.First(&o, f.dates[i].ID))
	if o.VisibilityOverride == nil {
		return "series"
	}
	return *o.VisibilityOverride
}

func (f *vcFixture) preview(t *testing.T, path string, body map[string]any) visibilityImpactJSON {
	t.Helper()
	var impact visibilityImpactJSON
	if err := json.Unmarshal([]byte(apiDo(t, f.admin, "GET", visibilityPreviewPath(t, path, body), nil, http.StatusOK)), &impact); err != nil {
		t.Fatal(err)
	}
	return impact
}

func (f *vcFixture) state(t *testing.T) string {
	t.Helper()
	return eventWriterState(t, f.app, f.ev.ID)
}

// with is a copy of body with k set to v.
func with(body map[string]any, k string, v any) map[string]any {
	out := map[string]any{}
	for key, val := range body {
		out[key] = val
	}
	out[k] = v
	return out
}

// closeOnce closes c unless it is closed already, so a failed test never
// leaves a parked request holding its locks.
func closeOnce(c chan struct{}) {
	select {
	case <-c:
	default:
		close(c)
	}
}

func strp(s string) *string { return &s }

type decisionAnswer struct {
	Code          string               `json:"code"`
	NeedsDecision bool                 `json:"needsDecision"`
	Impact        visibilityImpactJSON `json:"impact"`
}

func wantDecision(t *testing.T, w *httptest.ResponseRecorder) visibilityImpactJSON {
	t.Helper()
	var a decisionAnswer
	if w.Code != http.StatusConflict || json.Unmarshal(w.Body.Bytes(), &a) != nil || (!a.NeedsDecision && a.Code != "date_needs_consent") || !consent.WellFormed(a.Impact.Consent) {
		t.Fatalf("want a 409 decision with a fresh token: %d %s", w.Code, w.Body.String())
	}
	return a.Impact
}

func wantOutdated(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"code":"visibility_review_outdated"`) || strings.Contains(w.Body.String(), `"impact"`) {
		t.Fatalf("want visibility_review_outdated with no impact: %d %s", w.Code, w.Body.String())
	}
}

func TestDateLosersCloneIsDeep(t *testing.T) {
	at := time.Date(2026, 12, 1, 18, 0, 0, 0, time.UTC)
	u1, u2, hp := uint(4), uint(5), uint(9)
	vis := "private"
	occ := func(id uint) *models.EventOccurrence {
		return &models.EventOccurrence{ID: id, EventID: 3, OriginalDate: at, StartsAt: at, EndsAt: at.Add(time.Hour), VisibilityOverride: &vis, TitleOverride: strp("Supper")}
	}
	src := dateLosers{
		now:            at,
		occIDs:         []uint{11, 12},
		dates:          map[uint]*models.EventOccurrence{11: occ(11), 12: occ(12)},
		rows:           []models.Registration{{ID: 1, OccurrenceID: 11, UserID: &u1, RegisteredBy: &u1, WaitlistedAt: &at}, {ID: 2, OccurrenceID: 12, RegisteredBy: &u2, HouseholdPersonID: &hp}},
		oldVis:         "public",
		prior:          map[uint]*models.EventOccurrence{11: occ(11)},
		callerPromotes: true,
	}
	filled := map[string]bool{"now": true, "occIDs": true, "dates": true, "rows": true, "oldVis": true, "prior": true, "callerPromotes": true}
	rt := reflect.TypeOf(src)
	for i := 0; i < rt.NumField(); i++ {
		if !filled[rt.Field(i).Name] {
			t.Errorf("dateLosers.%s is not filled here: add it to this test and to Clone", rt.Field(i).Name)
		}
		if reflect.ValueOf(src).Field(i).IsZero() {
			t.Errorf("dateLosers.%s is zero in the fixture", rt.Field(i).Name)
		}
	}
	clone := src.Clone()
	if !reflect.DeepEqual(src, clone) {
		t.Fatalf("the clone differs:\n%+v\n%+v", src, clone)
	}
	if shared := sharedReferences(reflect.ValueOf(src), reflect.ValueOf(clone), "dateLosers"); len(shared) > 0 {
		t.Fatalf("the clone shares memory with the original: %v", shared)
	}
	*clone.rows[0].UserID = 99
	*clone.dates[11].TitleOverride = "x"
	if *src.rows[0].UserID != 4 || *src.dates[11].TitleOverride != "Supper" {
		t.Fatal("an edit of the clone reached the original")
	}
}

// sharedReferences lists every pointer, map and slice backing array a and b
// share (time.Time's location excepted: it is immutable).
func sharedReferences(a, b reflect.Value, path string) []string {
	var out []string
	switch a.Kind() {
	case reflect.Pointer:
		if a.IsNil() || b.IsNil() {
			return nil
		}
		if a.Pointer() == b.Pointer() {
			out = append(out, path)
		}
		out = append(out, sharedReferences(a.Elem(), b.Elem(), path+".*")...)
	case reflect.Map:
		if a.IsNil() || b.IsNil() {
			return nil
		}
		if a.Pointer() == b.Pointer() {
			out = append(out, path)
		}
		for _, k := range a.MapKeys() {
			out = append(out, sharedReferences(a.MapIndex(k), b.MapIndex(k), fmt.Sprintf("%s[%v]", path, k))...)
		}
	case reflect.Slice:
		if a.Len() > 0 && b.Len() > 0 && a.Pointer() == b.Pointer() {
			out = append(out, path)
		}
		for i := 0; i < a.Len() && i < b.Len(); i++ {
			out = append(out, sharedReferences(a.Index(i), b.Index(i), fmt.Sprintf("%s[%d]", path, i))...)
		}
	case reflect.Struct:
		if a.Type() == timeType {
			return nil
		}
		for i := 0; i < a.NumField(); i++ {
			out = append(out, sharedReferences(a.Field(i), b.Field(i), path+"."+a.Type().Field(i).Name)...)
		}
	}
	return out
}

// TestConsentKeyIsStableAcrossAppValues: two App values with one secret derive
// one key, with no boot; another secret derives another. Key stability only:
// a real restart also rotates the epoch (TestVisibilityConsentAcrossServeRestart).
func TestConsentKeyIsStableAcrossAppValues(t *testing.T) {
	a := &App{Config: Config{SessionSecret: []byte("secret-one-secret-one-secret-one")}}
	b := &App{Config: Config{SessionSecret: []byte("secret-one-secret-one-secret-one")}}
	c := &App{Config: Config{SessionSecret: []byte("secret-two-secret-two-secret-two")}}
	ka, _ := a.consentKey()
	kb, _ := b.consentKey()
	kc, _ := c.consentKey()
	if !bytes.Equal(ka, kb) || bytes.Equal(ka, kc) {
		t.Fatal("the consent key is not the secret's alone")
	}
	if _, err := (&App{}).consentKey(); err == nil {
		t.Fatal("an empty secret derived a key")
	}
}

// TestVisibilityConsentAcrossServeRestart: another process's boot rotates the
// epoch once, so a token minted before it is asked once more, and the fresh
// token is valid on either process.
func TestVisibilityConsentAcrossServeRestart(t *testing.T) {
	f := newVCFixture(t, models.VisibilityPublic)
	out := f.outsider(t, "outsider", 0)
	body := map[string]any{"visibility": models.VisibilityInviteOnly}
	old := f.preview(t, f.eventPath(), body).Consent
	e1, _ := consentEpochIn(f.app.DB)
	if err := (&App{DB: f.app.DB}).bootRestoreFence(); err != nil {
		t.Fatal(err)
	}
	e2, _ := consentEpochIn(f.app.DB)
	if e1 == e2 || e2 == "" {
		t.Fatalf("the boot did not rotate the epoch: %q -> %q", e1, e2)
	}
	before := f.state(t)
	fresh := wantDecision(t, f.admin.do("PUT", f.eventPath(), with(body, "consent", old), true))
	if f.state(t) != before {
		t.Fatal("a token from before the restart wrote")
	}
	apiDo(t, f.admin, "PUT", f.eventPath(), with(body, "consent", fresh.Consent), http.StatusOK)
	if got := regStatus(t, f.app, out.ID, f.dates[0].ID); got != models.RegCancelled {
		t.Fatalf("the reviewed registrant is %s", got)
	}
	if e3, _ := consentEpochIn(f.app.DB); e3 != e2 {
		t.Fatal("a confirmation moved the epoch")
	}
}

// TestVisibilityConsentAcrossSecrets: processes with different secrets refuse
// each other's tokens with a fresh 409 and write nothing, however often a
// manager alternates; once one secret is used again, the token holds.
func TestVisibilityConsentAcrossSecrets(t *testing.T) {
	f := newVCFixture(t, models.VisibilityPublic)
	out := f.outsider(t, "outsider", 0)
	body := map[string]any{"visibility": models.VisibilityInviteOnly}
	s1 := f.app.Config.SessionSecret
	s2 := []byte("another-secret-another-secret-12")
	before := f.state(t)
	for i := 0; i < 5; i++ {
		f.app.Config.SessionSecret = s2
		other := f.preview(t, f.eventPath(), body).Consent
		f.app.Config.SessionSecret = s1
		wantDecision(t, f.admin.do("PUT", f.eventPath(), with(body, "consent", other), true))
	}
	if f.state(t) != before {
		t.Fatal("a token under another secret wrote")
	}
	apiDo(t, f.admin, "PUT", f.eventPath(), confirmVisibility(t, f.admin, f.eventPath(), body), http.StatusOK)
	if got := regStatus(t, f.app, out.ID, f.dates[0].ID); got != models.RegCancelled {
		t.Fatalf("the reviewed registrant is %s", got)
	}
}

// TestNoConsentWhereNothingIsReviewRequired (C6): a change that cancels nobody
// and is not a reset saves whatever its consent says, and declares
// 'v1:no-review'; every reset is review-required, even with nobody registered
// and no override left.
func TestNoConsentWhereNothingIsReviewRequired(t *testing.T) {
	for _, given := range []any{nil, "", "garbage", strings.Repeat("a", 32)} {
		t.Run(fmt.Sprintf("consent %v", given), func(t *testing.T) {
			f := newVCFixture(t, models.VisibilityPublic)
			body := func(b map[string]any) map[string]any {
				if given != nil {
					return with(b, "consent", given)
				}
				return b
			}
			// The guarded date edit reads a consent only as a token: a
			// malformed one is an unreadable body, refused before any check.
			want := []string{consentNoReview, consentNoReview, consentNoReview}
			changes := map[string]any{"visibility": models.VisibilityInviteOnly}
			switch s, _ := given.(string); {
			case given == nil:
				saveDate(t, f.admin, f.ev.ID, f.day(0), changes, http.StatusOK, "")
			case consent.WellFormed(s):
				saveDate(t, f.admin, f.ev.ID, f.day(0), changes, http.StatusOK, "", "consent", s)
			default:
				saveDate(t, f.admin, f.ev.ID, f.day(0), changes, http.StatusBadRequest, "date_edit_invalid", "consent", s)
				want = want[1:]
			}
			apiDo(t, f.admin, "PUT", f.eventPath(), body(map[string]any{"visibility": models.VisibilityPrivate}), http.StatusOK)
			apiDo(t, f.admin, "PUT", f.groupPath(), body(map[string]any{"visibility": models.VisibilityInviteOnly, "openedEvents": map[string]string{"date:" + itoa(f.dates[0].ID): "keep"}}), http.StatusOK)
			if got := f.takeDeclared(); !slices.Equal(got, want) {
				t.Fatalf("declarations %v, want %v", got, want)
			}
		})
	}
	t.Run("a reset with nobody registered", func(t *testing.T) {
		f := newVCFixture(t, models.VisibilityPublic)
		reset := map[string]any{"visibility": models.VisibilityPublic, "resetOverrides": true}
		for _, overrides := range []int{0, 1} {
			if overrides == 1 {
				f.setOverride(t, 1, strp(models.VisibilityInviteOnly))
			}
			before := f.state(t)
			wantOutdated(t, f.admin.do("PUT", f.eventPath(), reset, true))
			impact := wantDecision(t, f.admin.do("PUT", f.eventPath(), with(reset, "consent", ""), true))
			if impact.CurrentVisibility != models.VisibilityPublic || len(impact.ResetDates) != overrides {
				t.Fatalf("the reset's decision: %+v", impact)
			}
			if f.state(t) != before {
				t.Fatal("an unreviewed reset wrote")
			}
			f.takeDeclared()
			apiDo(t, f.admin, "PUT", f.eventPath(), confirmVisibility(t, f.admin, f.eventPath(), reset), http.StatusOK)
			if got := f.takeDeclared(); !slices.Equal(got, []string{consentChecked}) {
				t.Fatalf("a checked reset declared %v, want v1", got)
			}
		}
		if f.override(t, 1) != "series" {
			t.Fatal("the reviewed reset left the override")
		}
	})
}

// TestStaleTabBodiesAreRefused (C8): a page from before this rule (confirm:true,
// or a group body with no consent) is refused over a registrant with nothing
// written, saves over nobody, and an event reset is refused either way. A
// date page from before the guarded edit gets date_page_outdated whatever its
// body, over a registrant or not (apiRetiredDatePut).
func TestStaleTabBodiesAreRefused(t *testing.T) {
	f := newVCFixture(t, models.VisibilityPublic)
	f.outsider(t, "outsider", 0)
	before := f.state(t)
	wantDatePageOutdated(t, f.admin.do("PUT", f.datePath(0), map[string]any{"visibility": models.VisibilityInviteOnly, "confirm": true}, true))
	wantOutdated(t, f.admin.do("PUT", f.eventPath(), map[string]any{"visibility": models.VisibilityInviteOnly, "confirm": true}, true))
	wantOutdated(t, f.admin.do("PUT", f.groupPath(), map[string]any{"visibility": models.VisibilityInviteOnly}, true))
	wantDatePageOutdated(t, f.admin.do("PUT", f.datePath(1), map[string]any{"visibility": models.VisibilityInviteOnly, "confirm": true}, true))
	if f.state(t) != before {
		t.Fatal("a stale tab's body wrote")
	}
	f.setOverride(t, 1, strp(models.VisibilityInviteOnly))
	wantOutdated(t, f.admin.do("PUT", f.eventPath(), map[string]any{"visibility": models.VisibilityPublic, "confirm": true, "resetOverrides": true}, true))
	f.setOverride(t, 1, nil)
	wantOutdated(t, f.admin.do("PUT", f.eventPath(), map[string]any{"visibility": models.VisibilityPublic, "confirm": true, "resetOverrides": true}, true))
}

// TestVisibilityCancellationsCancelExactlyTheCheckedSet (C9): a sign-up sent
// from another connection while the write holds its checked selection waits
// on the date's lock, lands after the commit, and is not cancelled; the
// reviewed registrant is.
func TestVisibilityCancellationsCancelExactlyTheCheckedSet(t *testing.T) {
	for _, route := range []string{"event", "group", "date"} {
		t.Run(route, func(t *testing.T) {
			f := newVCFixture(t, models.VisibilityPublic)
			reviewed := f.outsider(t, "reviewed", 0)
			late := newTestUser(t, f.app, "late", "password1234", models.RoleMember)
			path, body := f.eventPath(), map[string]any{"visibility": models.VisibilityInviteOnly}
			if route == "group" {
				path = f.groupPath()
			}
			send := func() *httptest.ResponseRecorder { return f.admin.do("PUT", path, body, true) }
			if route == "date" {
				// The editor's question first, then the same attempt with its
				// consent, the confirmation the hook below runs under.
				base := editBaseOf(t, f.admin, f.ev.ID, f.day(0))
				attempt := newDateAttempt(t)
				asked := wantDateEdit(t, f.admin, f.ev.ID, f.day(0), dateEditBody(attempt, base.Revision, body), http.StatusConflict, "date_needs_consent")
				confirmed := dateEditBody(attempt, base.Revision, body, "consent", consentOf(t, asked))
				send = func() *httptest.ResponseRecorder { return postDateEdit(f.admin, f.ev.ID, f.day(0), confirmed) }
			} else {
				body = confirmVisibility(t, f.admin, path, body)
			}
			inserted := make(chan error, 1)
			var once sync.Once
			f.app.hooks.consentEpochLocked = func(*gorm.DB, string) {
				once.Do(func() {
					go func() {
						inserted <- f.app.DB.Exec(`INSERT INTO registrations (user_id, occurrence_id, status, attendance_mode, source, created_at, updated_at) VALUES (?, ?, 'going', 'in_person', 'web', now(), now())`, late.ID, f.dates[0].ID).Error
					}()
					time.Sleep(200 * time.Millisecond)
				})
			}
			if w := send(); w.Code != http.StatusOK {
				t.Fatalf("the confirmation: %d %s", w.Code, w.Body.String())
			}
			if err := <-inserted; err != nil {
				t.Fatal(err)
			}
			if got := regStatus(t, f.app, reviewed.ID, f.dates[0].ID); got != models.RegCancelled {
				t.Fatalf("the reviewed registrant is %s", got)
			}
			if got := regStatus(t, f.app, late.ID, f.dates[0].ID); got != models.RegGoing {
				t.Fatalf("a sign-up sent after the check is %s", got)
			}
		})
	}
}

// TestCancellationRefusesRowsOutsideItsPlan (C13): the canceller verifies the
// copy it was handed against the checked identities, on every field, before
// any write; an edit of the route's own selection after the check is ignored
// by every checked writer; and a group route that leaves a checked cancel
// entry untaken rolls back whole.
func TestCancellationRefusesRowsOutsideItsPlan(t *testing.T) {
	t.Run("the handed-out copy edited before Verify", func(t *testing.T) {
		edits := map[string]func(sel *dateLosers){
			"registration id": func(sel *dateLosers) { sel.rows[0].ID++ },
			"occurrence id":   func(sel *dateLosers) { sel.rows[0].OccurrenceID++ },
			"user id":         func(sel *dateLosers) { v := *sel.rows[0].UserID + 1; sel.rows[0].UserID = &v },
			"user to null":    func(sel *dateLosers) { sel.rows[0].UserID = nil },
			"registered_by":   func(sel *dateLosers) { v := uint(77); sel.rows[0].RegisteredBy = &v },
			"household":       func(sel *dateLosers) { v := uint(78); sel.rows[0].HouseholdPersonID = &v },
			"an added row":    func(sel *dateLosers) { sel.rows = append(sel.rows, sel.rows[0]) },
			"a removed row":   func(sel *dateLosers) { sel.rows = sel.rows[:0] },
		}
		for name, edit := range edits {
			t.Run(name, func(t *testing.T) {
				f := newVCFixture(t, models.VisibilityPublic)
				f.outsider(t, "outsider", 0)
				body := confirmVisibility(t, f.admin, f.eventPath(), map[string]any{"visibility": models.VisibilityInviteOnly})
				f.app.hooks.consentSelection = edit
				before := f.state(t)
				apiDo(t, f.admin, "PUT", f.eventPath(), body, http.StatusInternalServerError)
				if f.state(t) != before {
					t.Fatal("a selection outside the plan wrote")
				}
				if n := auditCount(t, f.app, "registration.cancelled"); n != 0 {
					t.Fatalf("%d cancellation rows", n)
				}
			})
		}
	})
	t.Run("the caller's copy edited after the check", testCallerEditAfterCheck)
	t.Run("a cancel entry never consumed", testUntakenCancelEntryRollsBack)
}

// checkedWriter is one route that runs checkConsentIn, confirming a
// tightening of date 0 (or the series, or the group) to invite-only with a
// fresh token.
type checkedWriter struct {
	name    string
	confirm func(t *testing.T, f *vcFixture) *httptest.ResponseRecorder
	// series: the change is the series', so its notices name no day.
	series bool
}

var checkedWriters = func() []checkedWriter {
	io := models.VisibilityInviteOnly
	put := func(path func(*vcFixture) string) func(t *testing.T, f *vcFixture) *httptest.ResponseRecorder {
		return func(t *testing.T, f *vcFixture) *httptest.ResponseRecorder {
			p := path(f)
			return f.admin.do("PUT", p, confirmVisibility(t, f.admin, p, map[string]any{"visibility": io}), true)
		}
	}
	return []checkedWriter{
		{"guarded date POST", func(t *testing.T, f *vcFixture) *httptest.ResponseRecorder {
			date := models.FormatCalendarDate(f.dates[0].OriginalDate)
			changes := map[string]any{"visibility": io}
			token := dateEditConsent(t, f.admin, f.ev.ID, date, changes)
			body := dateEditBody(newDateAttempt(t), editBaseOf(t, f.admin, f.ev.ID, date).Revision, changes, "consent", token)
			return postDateEdit(f.admin, f.ev.ID, date, body)
		}, false},
		{"event", put((*vcFixture).eventPath), true},
		{"group", put((*vcFixture).groupPath), true},
	}
}()

const editedBehindTheCheck = "Edited behind the check"

// testCallerEditAfterCheck (C13, "the caller's copy edited after the
// check"): at the consentPassed seam, right after the token matched, the
// route's own selections are edited (each identity field of one row with its
// registration id kept, a party's status and size, an appended non-loser row,
// and the dates, the dates as they were before the change and the old
// visibility the notices read). The route still cancels exactly the checked
// rows, saves each as it was checked, and audits and notifies the checked
// holders with the checked title and day.
func testCallerEditAfterCheck(t *testing.T) {
	type rows struct{ o1, kit *models.Registration }
	find := func(t *testing.T, sel *dateLosers, p *lifecyclePeople) rows {
		var out rows
		for i := range sel.rows {
			r := &sel.rows[i]
			if r.UserID != nil && *r.UserID == p.o1.ID {
				out.o1 = r
			}
			if r.HouseholdPersonID != nil && *r.HouseholdPersonID == p.kit {
				out.kit = r
			}
		}
		if out.o1 == nil || out.kit == nil {
			t.Fatalf("fixture: the selection lacks o1's or Kit's row: %+v", sel.rows)
		}
		return out
	}
	edits := map[string]func(t *testing.T, f *vcFixture, p *lifecyclePeople, sel *dateLosers){
		"occurrence id": func(t *testing.T, f *vcFixture, p *lifecyclePeople, sel *dateLosers) {
			find(t, sel, p).o1.OccurrenceID = f.dates[3].ID
		},
		"user id": func(t *testing.T, f *vcFixture, p *lifecyclePeople, sel *dateLosers) {
			find(t, sel, p).o1.UserID = &p.m.ID
		},
		"user to null": func(t *testing.T, f *vcFixture, p *lifecyclePeople, sel *dateLosers) { find(t, sel, p).o1.UserID = nil },
		"registered_by": func(t *testing.T, f *vcFixture, p *lifecyclePeople, sel *dateLosers) {
			find(t, sel, p).kit.RegisteredBy = &p.m.ID
		},
		"registered_by to null": func(t *testing.T, f *vcFixture, p *lifecyclePeople, sel *dateLosers) {
			find(t, sel, p).kit.RegisteredBy = nil
		},
		"household person from null": func(t *testing.T, f *vcFixture, p *lifecyclePeople, sel *dateLosers) {
			find(t, sel, p).o1.HouseholdPersonID = &p.kit
		},
		"household person to null": func(t *testing.T, f *vcFixture, p *lifecyclePeople, sel *dateLosers) {
			find(t, sel, p).kit.HouseholdPersonID = nil
		},
		"status and party size": func(t *testing.T, f *vcFixture, p *lifecyclePeople, sel *dateLosers) {
			r := find(t, sel, p).o1
			r.Status, r.GuestCount = models.RegWaitlisted, 3
		},
		"an appended row": func(t *testing.T, f *vcFixture, p *lifecyclePeople, sel *dateLosers) {
			var mine models.Registration
			gormMust(t, f.app.DB.Where("user_id = ? AND occurrence_id = ?", p.m.ID, f.dates[0].ID).First(&mine))
			sel.rows = append(sel.rows, mine)
		},
		"the dates, the dates before and the old visibility": func(t *testing.T, f *vcFixture, p *lifecyclePeople, sel *dateLosers) {
			moved := func(o *models.EventOccurrence) *models.EventOccurrence {
				c := *o
				c.TitleOverride = strp(editedBehindTheCheck)
				c.OriginalDate = c.OriginalDate.AddDate(0, 0, 1)
				c.StartsAt = c.StartsAt.AddDate(0, 0, 1)
				return &c
			}
			// Each entry is replaced, so the route's own date values stay
			// as they were and only what the selections point at moves.
			for id, o := range sel.dates {
				sel.dates[id] = moved(o)
			}
			for id, o := range sel.prior {
				sel.prior[id] = moved(o)
			}
			sel.oldVis = models.VisibilityInviteOnly
		},
	}
	for _, w := range checkedWriters {
		for name, edit := range edits {
			t.Run(w.name+"/"+name, func(t *testing.T) {
				f, p := newLifecycleFixture(t)
				var checked []models.Registration
				edited := 0
				f.app.hooks.consentPassed = func(entries []consent.Entry[dateLosers]) {
					for i := range entries {
						if !entries[i].Cancels || len(entries[i].Sel.rows) == 0 {
							continue
						}
						checked = append(checked, deepCopy(entries[i].Sel.rows)...)
						edit(t, f, p, &entries[i].Sel)
						edited++
					}
				}
				if got := w.confirm(t, f); got.Code != http.StatusOK {
					t.Fatalf("the confirmation after the edit: %d %s", got.Code, got.Body.String())
				}
				if edited == 0 {
					t.Fatal("fixture: the seam edited no selection")
				}
				wantCheckedRowsCancelled(t, f, p, checked, w.series)
			})
		}
	}
}

// wantCheckedRowsCancelled: exactly checked was cancelled, each row saved as
// it was checked, one cancellation audit row per checked row with its
// checked identity, and one notice per checked holder with the checked title
// and (for one date) the checked day.
func wantCheckedRowsCancelled(t *testing.T, f *vcFixture, p *lifecyclePeople, checked []models.Registration, series bool) {
	t.Helper()
	ids := map[uint]models.Registration{}
	holders := map[uint]bool{}
	for _, r := range checked {
		ids[r.ID] = r
		holders[registrationHolder(r)] = true
		var got models.Registration
		gormMust(t, f.app.DB.First(&got, r.ID))
		want := r
		want.Status, want.ManageTokenHash, want.WaitlistedAt, want.PromotedAt = models.RegCancelled, got.ManageTokenHash, nil, nil
		want.UpdatedAt = got.UpdatedAt
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("registration %d was saved as\n%+v\nwant the checked row cancelled\n%+v", r.ID, got, want)
		}
	}
	if n := cancelledCount(t, f.app); n != int64(len(checked)) {
		t.Fatalf("%d registrations cancelled, %d were checked", n, len(checked))
	}
	if got := regStatus(t, f.app, p.m.ID, f.dates[0].ID); got != models.RegGoing {
		t.Fatalf("the member's row outside the plan is %s", got)
	}
	var logs []models.AuditLog
	gormMust(t, f.app.DB.Where("action = ?", audit.RegistrationCancelled).Order("id").Find(&logs))
	if len(logs) != len(checked) {
		t.Fatalf("%d cancellation audit rows, %d were checked", len(logs), len(checked))
	}
	for _, l := range logs {
		var meta map[string]any
		if err := json.Unmarshal([]byte(l.MetaJSON), &meta); err != nil {
			t.Fatal(err)
		}
		r, ok := ids[uint(meta["registrationId"].(float64))]
		if !ok {
			t.Fatalf("an audit row for a registration outside the plan: %s", l.MetaJSON)
		}
		want := registrationMeta(r)
		for _, k := range []string{"userId", "personId", "registeredBy", "occurrenceId", "guestCount", "status"} {
			if fmt.Sprint(jsonValue(t, want[k])) != fmt.Sprint(meta[k]) {
				t.Fatalf("the audit row of registration %d has %s %v, checked %v", r.ID, k, meta[k], jsonValue(t, want[k]))
			}
		}
	}
	var notices []models.Notification
	gormMust(t, f.app.DB.Where("kind = ?", notify.RegistrationCancelled).Order("id").Find(&notices))
	told := map[uint]bool{}
	day := models.FormatCalendarDate(f.dates[0].OriginalDate)
	for _, n := range notices {
		if !holders[n.UserID] || told[n.UserID] {
			t.Fatalf("a notice to %d, checked holders %v (told %v)", n.UserID, holders, told)
		}
		told[n.UserID] = true
		text := n.Title + " " + n.Body
		if !strings.Contains(text, f.ev.Title) || strings.Contains(text, editedBehindTheCheck) {
			t.Fatalf("the notice to %d does not carry the checked title: %q", n.UserID, text)
		}
		if !series && !strings.Contains(text, day) {
			t.Fatalf("the notice to %d does not carry the checked day %s: %q", n.UserID, day, text)
		}
		if next := models.FormatCalendarDate(f.dates[0].OriginalDate.AddDate(0, 0, 1)); strings.Contains(text, next) {
			t.Fatalf("the notice to %d carries the edited day: %q", n.UserID, text)
		}
	}
	if len(told) != len(holders) {
		t.Fatalf("notices to %v, checked holders %v", told, holders)
	}
}

// jsonValue is v as encoding/json would decode it into an any.
func jsonValue(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// testUntakenCancelEntryRollsBack (C13, "a cancel entry never consumed"): the
// group tightens two events with a registrant each, the consentSkipTake seam
// drops the second event's Take (and its writes) with Done left in place, and
// the confirmation answers 500 with nothing written: the first event's
// cancellation, its visibility, the group, and every audit, notice and job
// row roll back with it.
func testUntakenCancelEntryRollsBack(t *testing.T) {
	f := newVCFixture(t, models.VisibilityPublic)
	f.outsider(t, "first", 0)
	second := requireCreatedEvent(t, f.app, f.admin.do("POST", "/api/v1/groups/"+itoa(f.gid)+"/events", createEventBodyForTest(), true))
	var occ models.EventOccurrence
	gormMust(t, f.app.DB.Where("event_id = ?", second.ID).Order("starts_at").First(&occ))
	other := newTestUser(t, f.app, "second", "password1234", models.RoleMember)
	gormMust(t, f.app.DB.Create(&models.Registration{UserID: &other.ID, OccurrenceID: occ.ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
	body := confirmVisibility(t, f.admin, f.groupPath(), map[string]any{"visibility": models.VisibilityInviteOnly})
	var seen []consent.Key
	f.app.hooks.consentSkipTake = func(k consent.Key) bool {
		seen = append(seen, k)
		return k.Event == uint64(second.ID)
	}
	var taken atomic.Int64
	f.app.hooks.consentSelection = func(*dateLosers) { taken.Add(1) }
	before := wholeState(t, f.app)
	w := f.admin.do("PUT", f.groupPath(), body, true)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("a group write that left a checked entry untaken answered %d %s", w.Code, w.Body.String())
	}
	if len(seen) != 2 || taken.Load() != 1 {
		t.Fatalf("fixture: the route reached %v and cancelled for %d events, want both reached and the first cancelled before Done", seen, taken.Load())
	}
	if after := wholeState(t, f.app); after != before {
		t.Fatalf("the refused write left changes:\n%s\nwant\n%s", after, before)
	}
}

// wholeState is every row a group tightening can touch: the groups, events,
// dates and registrations, and the audit, notice, delivery and job rows.
func wholeState(t *testing.T, app *App) string {
	t.Helper()
	var state struct {
		Groups        []models.Group
		Events        []models.Event
		Dates         []models.EventOccurrence
		Registrations []models.Registration
		Audit         []models.AuditLog
		Notices       []models.Notification
		Jobs          []models.Job
	}
	for _, q := range []any{&state.Groups, &state.Events, &state.Dates, &state.Registrations, &state.Audit, &state.Notices, &state.Jobs} {
		gormMust(t, app.DB.Unscoped().Order("id").Find(q))
	}
	b, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestEventResetIsReviewedWithNoLosers (C19): every reset is checked against
// the reset dates it showed, with nobody registered anywhere.
func TestEventResetIsReviewedWithNoLosers(t *testing.T) {
	reset := map[string]any{"visibility": models.VisibilityPublic, "resetOverrides": true}
	io := models.VisibilityInviteOnly
	setup := func(t *testing.T) *vcFixture {
		f := newVCFixture(t, models.VisibilityPublic)
		f.setOverride(t, 0, &io)
		f.setOverride(t, 1, &io)
		return f
	}
	ids := func(impact visibilityImpactJSON) []uint {
		var out []uint
		for _, d := range impact.ResetDates {
			out = append(out, d.OccurrenceID)
		}
		return out
	}
	t.Run("an override added", func(t *testing.T) {
		f := setup(t)
		token := f.preview(t, f.eventPath(), reset).Consent
		f.setOverride(t, 2, &io)
		before := f.state(t)
		fresh := wantDecision(t, f.admin.do("PUT", f.eventPath(), with(reset, "consent", token), true))
		if len(fresh.ResetDates) != 3 || f.state(t) != before {
			t.Fatalf("the 409 lists %v, or something was written", ids(fresh))
		}
		apiDo(t, f.admin, "PUT", f.eventPath(), with(reset, "consent", fresh.Consent), http.StatusOK)
		for i := 0; i < 3; i++ {
			if f.override(t, i) != "series" {
				t.Fatalf("date %d was not reset", i)
			}
		}
	})
	t.Run("an override removed", func(t *testing.T) {
		f := setup(t)
		token := f.preview(t, f.eventPath(), reset).Consent
		f.setOverride(t, 1, nil)
		fresh := wantDecision(t, f.admin.do("PUT", f.eventPath(), with(reset, "consent", token), true))
		if !slices.Equal(ids(fresh), []uint{f.dates[0].ID}) {
			t.Fatalf("the 409 lists %v", ids(fresh))
		}
	})
	t.Run("a same-count swap", func(t *testing.T) {
		f := setup(t)
		token := f.preview(t, f.eventPath(), reset).Consent
		f.setOverride(t, 1, nil)
		f.setOverride(t, 2, &io)
		before := f.state(t)
		fresh := wantDecision(t, f.admin.do("PUT", f.eventPath(), with(reset, "consent", token), true))
		if !slices.Equal(ids(fresh), []uint{f.dates[0].ID, f.dates[2].ID}) || f.state(t) != before {
			t.Fatalf("the swapped 409 lists %v, or the old token wrote", ids(fresh))
		}
	})
	t.Run("an override value changed", func(t *testing.T) {
		f := setup(t)
		token := f.preview(t, f.eventPath(), reset).Consent
		f.setOverride(t, 1, strp(models.VisibilityPrivate))
		wantDecision(t, f.admin.do("PUT", f.eventPath(), with(reset, "consent", token), true))
	})
	t.Run("private to public: the second date stays until reviewed", func(t *testing.T) {
		f := newVCFixture(t, models.VisibilityPublic)
		f.setOverride(t, 0, &io)
		token := f.preview(t, f.eventPath(), reset).Consent
		f.setOverride(t, 1, &io)
		fresh := wantDecision(t, f.admin.do("PUT", f.eventPath(), with(reset, "consent", token), true))
		if f.override(t, 1) != io || len(fresh.ResetDates) != 2 {
			t.Fatalf("the second date: %s, the 409 lists %v", f.override(t, 1), ids(fresh))
		}
		apiDo(t, f.admin, "PUT", f.eventPath(), with(reset, "consent", fresh.Consent), http.StatusOK)
		if f.override(t, 0) != "series" || f.override(t, 1) != "series" {
			t.Fatal("the reviewed dates were not reset")
		}
	})
	t.Run("absent and empty consent", func(t *testing.T) {
		f := newVCFixture(t, models.VisibilityPublic)
		f.setOverride(t, 0, &io)
		before := f.everything(t)
		wantOutdated(t, f.admin.do("PUT", f.eventPath(), reset, true))
		wantDecision(t, f.admin.do("PUT", f.eventPath(), with(reset, "consent", ""), true))
		if f.everything(t) != before || f.override(t, 0) != io {
			t.Fatal("an unreviewed reset wrote")
		}
	})
	t.Run("nothing to reset", func(t *testing.T) {
		f := newVCFixture(t, models.VisibilityPublic)
		before := f.everything(t)
		fresh := wantDecision(t, f.admin.do("PUT", f.eventPath(), with(reset, "consent", ""), true))
		if len(fresh.ResetDates) != 0 || f.everything(t) != before {
			t.Fatalf("the empty reset's 409: %+v, or it wrote", fresh)
		}
		apiDo(t, f.admin, "PUT", f.eventPath(), with(reset, "consent", f.preview(t, f.eventPath(), reset).Consent), http.StatusOK)
	})
	t.Run("a checked reset that cancels nobody declares v1 under the live fence", func(t *testing.T) {
		f := setup(t)
		audits := auditCount(t, f.app, "event.visibility_changed")
		f.takeDeclared()
		apiDo(t, f.admin, "PUT", f.eventPath(), with(reset, "consent", f.preview(t, f.eventPath(), reset).Consent), http.StatusOK)
		if got := f.takeDeclared(); !slices.Equal(got, []string{consentChecked}) {
			t.Fatalf("the reset declared %v, want v1", got)
		}
		var fence int64
		gormMust(t, f.app.DB.Raw("SELECT count(*) FROM pg_trigger WHERE tgname = 'audit_logs_visibility_consent' AND tgenabled <> 'D'").Scan(&fence))
		// Two reset rows and the series row, inserted under the live trigger.
		if n := auditCount(t, f.app, "event.visibility_changed") - audits; fence != 1 || n != 3 {
			t.Fatalf("the fence is enabled %d times; %d visibility rows", fence, n)
		}
	})
	t.Run("an override only on a date that has started stays", func(t *testing.T) {
		f := newVCFixture(t, models.VisibilityPublic)
		f.setOverride(t, 0, &io)
		underway := f.dates[0].StartsAt.Add(time.Minute)
		f.app.clock = func() time.Time { return underway }
		impact := f.preview(t, f.eventPath(), reset)
		if len(impact.ResetDates) != 0 || impact.FutureOverrides != 0 {
			t.Fatalf("a started date is in the reset list: %+v", impact)
		}
		apiDo(t, f.admin, "PUT", f.eventPath(), with(reset, "consent", impact.Consent), http.StatusOK)
		if f.override(t, 0) != io {
			t.Fatal("the started date lost its own visibility")
		}
		var n int64
		gormMust(t, f.app.DB.Model(&models.AuditLog{}).Where("meta_json->>'reason' = 'series_reset'").Count(&n))
		if n != 0 {
			t.Fatalf("%d reset rows for a date that started", n)
		}
	})
}

// TestEventResetBindsTheCurrentSeries (C21): another manager's series change
// under a reset review re-asks, with the new current value.
func TestEventResetBindsTheCurrentSeries(t *testing.T) {
	io := models.VisibilityInviteOnly
	reset := map[string]any{"visibility": models.VisibilityPublic, "resetOverrides": true}
	t.Run("the reviewer's example", func(t *testing.T) {
		f := newVCFixture(t, models.VisibilityPublic)
		f.setOverride(t, 0, &io)
		token := f.preview(t, f.eventPath(), reset).Consent
		f.setSeries(t, io)
		before := f.state(t)
		fresh := wantDecision(t, f.admin.do("PUT", f.eventPath(), with(reset, "consent", token), true))
		if fresh.CurrentVisibility != io || len(fresh.ResetDates) != 1 || f.state(t) != before {
			t.Fatalf("the 409: %+v", fresh)
		}
		apiDo(t, f.admin, "PUT", f.eventPath(), with(reset, "consent", fresh.Consent), http.StatusOK)
		if ev := eventRow(t, f.app, f.ev.ID); ev.Visibility != models.VisibilityPublic || f.override(t, 0) != "series" {
			t.Fatalf("after the reviewed reset: %s, %s", ev.Visibility, f.override(t, 0))
		}
	})
	for _, move := range [][2]string{{models.VisibilityPublic, models.VisibilityPrivate}, {models.VisibilityPublic, io}, {models.VisibilityPrivate, io}} {
		t.Run("another manager moves the series from "+move[0]+" to "+move[1], func(t *testing.T) {
			f := newVCFixture(t, models.VisibilityPublic)
			f.setSeries(t, move[0])
			f.setOverride(t, 0, strp(models.VisibilityPublic))
			body := map[string]any{"visibility": move[0], "resetOverrides": true}
			token := f.preview(t, f.eventPath(), body).Consent
			apiDo(t, f.admin, "PUT", f.eventPath(), map[string]any{"visibility": move[1], "consent": ""}, http.StatusOK)
			before := f.everything(t)
			fresh := wantDecision(t, f.admin.do("PUT", f.eventPath(), with(body, "consent", token), true))
			if fresh.CurrentVisibility != move[1] || len(fresh.ResetDates) != 1 || f.everything(t) != before {
				t.Fatalf("the 409: %+v, or the old token wrote", fresh)
			}
			apiDo(t, f.admin, "PUT", f.eventPath(), with(body, "consent", fresh.Consent), http.StatusOK)
			if ev := eventRow(t, f.app, f.ev.ID); ev.Visibility != move[0] || f.override(t, 0) != "series" {
				t.Fatalf("after the reviewed reset: %s, %s", ev.Visibility, f.override(t, 0))
			}
		})
	}
	t.Run("a date's current value", func(t *testing.T) {
		f := newVCFixture(t, models.VisibilityPublic)
		f.outsider(t, "outsider", 0)
		body := map[string]any{"visibility": io}
		token := f.preview(t, f.datePath(0), body).Consent
		apiDo(t, f.admin, "PUT", f.eventPath(), confirmVisibility(t, f.admin, f.eventPath(), map[string]any{"visibility": models.VisibilityPrivate}), http.StatusOK)
		before := f.everything(t)
		base := editBaseOf(t, f.admin, f.ev.ID, f.day(0))
		fresh := wantDecision(t, postDateEdit(f.admin, f.ev.ID, f.day(0), dateEditBody(newDateAttempt(t), base.Revision, body, "consent", token)))
		if fresh.CurrentVisibility != models.VisibilityPrivate || f.everything(t) != before {
			t.Fatalf("the date's 409 names %q, or the old token wrote", fresh.CurrentVisibility)
		}
	})
	t.Run("no override left", func(t *testing.T) {
		f := newVCFixture(t, models.VisibilityPublic)
		f.setOverride(t, 0, &io)
		token := f.preview(t, f.eventPath(), reset).Consent
		f.setSeries(t, io)
		f.setOverride(t, 0, nil)
		fresh := wantDecision(t, f.admin.do("PUT", f.eventPath(), with(reset, "consent", token), true))
		if len(fresh.ResetDates) != 0 || fresh.CurrentVisibility != io {
			t.Fatalf("the 409: %+v", fresh)
		}
		if ev := eventRow(t, f.app, f.ev.ID); ev.Visibility != io {
			t.Fatal("the stale empty reset opened the series")
		}
	})
	t.Run("the boundary: a non-reset review turned opening saves its target", func(t *testing.T) {
		f := newVCFixture(t, models.VisibilityPublic)
		out := f.outsider(t, "outsider", 0)
		body := map[string]any{"visibility": models.VisibilityPrivate}
		token := f.preview(t, f.eventPath(), body).Consent
		f.setSeries(t, io)
		apiDo(t, f.admin, "PUT", f.eventPath(), with(body, "consent", token), http.StatusOK)
		if ev := eventRow(t, f.app, f.ev.ID); ev.Visibility != models.VisibilityPrivate {
			t.Fatalf("the series is %s", ev.Visibility)
		}
		if got := regStatus(t, f.app, out.ID, f.dates[0].ID); got != models.RegGoing {
			t.Fatalf("an opening cancelled someone: %s", got)
		}
	})
	t.Run("a group's current value", func(t *testing.T) {
		f := newVCFixture(t, models.VisibilityPublic)
		f.outsider(t, "outsider", 0)
		body := map[string]any{"visibility": io}
		token := f.preview(t, f.groupPath(), body).Consent
		gormMust(t, f.app.DB.Model(&models.Group{}).Where("id = ?", f.gid).Update("visibility", models.VisibilityPrivate))
		fresh := wantDecision(t, f.admin.do("PUT", f.groupPath(), with(body, "consent", token), true))
		if fresh.CurrentVisibility != models.VisibilityPrivate {
			t.Fatalf("the group's 409 names %q", fresh.CurrentVisibility)
		}
	})
}

// TestDateEditReasonNamedLikeAResetSaves (C20): a person's own reason typed as
// series_reset never trips the audit fence, on either date route.
func TestDateEditReasonNamedLikeAResetSaves(t *testing.T) {
	f := newVCFixture(t, models.VisibilityPublic)
	io := models.VisibilityInviteOnly
	f.setOverride(t, 0, &io)
	saveDate(t, f.admin, f.ev.ID, f.day(0), map[string]any{"visibility": nil}, http.StatusOK, "", "reason", "series_reset")
	saveDate(t, f.admin, f.ev.ID, f.day(1), map[string]any{"visibility": models.VisibilityPrivate}, http.StatusOK, "", "reason", "series_reset")
	if got := f.takeDeclared(); !slices.Equal(got, []string{consentNoReview, consentNoReview}) {
		t.Fatalf("declarations %v", got)
	}
	f.outsider(t, "outsider", 2)
	saveDateConsenting(t, f.admin, f.ev.ID, f.day(2), map[string]any{"visibility": io}, "reason", "series_reset")
	if got := f.takeDeclared(); !slices.Equal(got, []string{consentChecked}) {
		t.Fatalf("a checked tightening declared %v", got)
	}
	date := models.FormatCalendarDate(f.dates[3].OriginalDate)
	base := editBaseOf(t, f.admin, f.ev.ID, date)
	wantDateEdit(t, f.admin, f.ev.ID, date, dateEditBody(newDateAttempt(t), base.Revision, map[string]any{"visibility": "private"}, "reason", " series_reset "), http.StatusOK, "")
	if got := f.takeDeclared(); !slices.Equal(got, []string{consentNoReview}) {
		t.Fatalf("the guarded edit declared %v", got)
	}
}

// TestEveryEventVisibilityWriterDeclaresConsent (C22): every writer of an
// event visibility audit row declares once, under the live audit fence.
func TestEveryEventVisibilityWriterDeclaresConsent(t *testing.T) {
	f := newVCFixture(t, models.VisibilityPrivate)
	create := func(vis string) {
		t.Helper()
		w := f.admin.do("POST", fmt.Sprintf("/api/v1/groups/%d/events", f.gid), map[string]any{"title": "W1 " + vis, "startsLocal": "2026-12-20T18:00", "durationMinutes": 60, "tzid": "UTC", "visibility": vis}, true)
		if w.Code != http.StatusCreated {
			t.Fatalf("create at %s: %d %s", vis, w.Code, w.Body.String())
		}
	}
	for _, vis := range []string{models.VisibilityPublic, models.VisibilityInviteOnly, models.VisibilityPrivate} {
		create(vis)
		if got := f.takeDeclared(); !slices.Equal(got, []string{consentNoReview}) {
			t.Fatalf("W1 at %s declared %v", vis, got)
		}
	}
	steps := []struct {
		name string
		run  func()
		want string
	}{
		{"W2 open", func() {
			apiDo(t, f.admin, "PUT", f.eventPath(), map[string]any{"visibility": models.VisibilityPublic, "consent": ""}, http.StatusOK)
		}, consentNoReview},
		{"W2 tighten n=0", func() {
			apiDo(t, f.admin, "PUT", f.eventPath(), map[string]any{"visibility": models.VisibilityPrivate, "consent": ""}, http.StatusOK)
		}, consentNoReview},
		{"W3 set more open", func() {
			saveDate(t, f.admin, f.ev.ID, f.day(0), map[string]any{"visibility": models.VisibilityPublic}, http.StatusOK, "")
		}, consentNoReview},
		{"W3 clear", func() {
			saveDate(t, f.admin, f.ev.ID, f.day(0), map[string]any{"visibility": nil}, http.StatusOK, "")
		}, consentNoReview},
		{"W3 tighten n=0", func() {
			saveDate(t, f.admin, f.ev.ID, f.day(1), map[string]any{"visibility": models.VisibilityInviteOnly}, http.StatusOK, "")
		}, consentNoReview},
		{"W2 reset", func() {
			apiDo(t, f.admin, "PUT", f.eventPath(), confirmVisibility(t, f.admin, f.eventPath(), map[string]any{"visibility": models.VisibilityPrivate, "resetOverrides": true}), http.StatusOK)
		}, consentChecked},
		{"W4 n=0 with a leader-opened event kept (W5)", func() {
			var opened models.Event
			gormMust(t, f.app.DB.Where("group_id = ? AND visibility = ?", f.gid, models.VisibilityPublic).First(&opened))
			apiDo(t, f.admin, "PUT", f.groupPath(), map[string]any{"visibility": models.VisibilityInviteOnly, "consent": "", "openedEvents": map[string]string{itoa(opened.ID): "keep"}}, http.StatusOK)
		}, consentNoReview},
	}
	for _, s := range steps {
		s.run()
		if got := f.takeDeclared(); !slices.Equal(got, []string{s.want}) {
			t.Fatalf("%s declared %v, want %s", s.name, got, s.want)
		}
	}
	// A tightening that cancels someone is checked.
	g := newVCFixture(t, models.VisibilityPublic)
	g.outsider(t, "outsider", 0)
	apiDo(t, g.admin, "PUT", g.groupPath(), confirmVisibility(t, g.admin, g.groupPath(), map[string]any{"visibility": models.VisibilityInviteOnly}), http.StatusOK)
	if got := g.takeDeclared(); !slices.Equal(got, []string{consentChecked}) {
		t.Fatalf("W4 n>0 declared %v", got)
	}
	// A church-wide event's creation writes no visibility row and runs no rule.
	w := g.admin.do("POST", "/api/v1/events", createEventBodyForTest(), true)
	if w.Code != http.StatusCreated {
		t.Fatalf("church-wide create: %d %s", w.Code, w.Body.String())
	}
	if got := g.takeDeclared(); len(got) != 0 {
		t.Fatalf("a church-wide creation declared %v", got)
	}
	writerRowsUnderTheGate(t)
}

// gated counts the checked confirmations of f's App, asserting inside each
// epoch lock the premises of design 2.5.2: this backend holds the exclusive
// contact gate, at Read Committed.
func gated(t *testing.T, f *vcFixture) *atomic.Int64 {
	var n atomic.Int64
	f.app.hooks.consentEpochLocked = func(tx *gorm.DB, _ string) {
		var held int64
		var level string
		if err := tx.Raw(`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND classid::bigint = ? AND objid::bigint = ?
			AND mode = 'ExclusiveLock' AND granted AND pid = pg_backend_pid()`, contactLockID>>32, contactLockID&0xffffffff).Scan(&held).Error; err != nil {
			t.Error(err)
		}
		if err := tx.Raw("SELECT current_setting('transaction_isolation')").Scan(&level).Error; err != nil {
			t.Error(err)
		}
		if held != 1 || level != "read committed" {
			t.Errorf("a checked confirmation holds the gate %d times at %s", held, level)
		}
		n.Add(1)
	}
	return &n
}

// writerRowsUnderTheGate is C22's table rows the sequential walk above does
// not reach: each through its real handler, with the declaration it makes
// and, on a checked row, exactly one epoch lock under the gate.
func writerRowsUnderTheGate(t *testing.T) {
	io := models.VisibilityInviteOnly
	type row struct {
		name string
		run  func(t *testing.T, f *vcFixture)
		want []string
	}
	guarded := func(t *testing.T, f *vcFixture, i int, changes map[string]any, consent string, extra ...any) {
		date := models.FormatCalendarDate(f.dates[i].OriginalDate)
		args := extra
		if consent != "" {
			args = append([]any{"consent", consent}, extra...)
		}
		wantDateEdit(t, f.admin, f.ev.ID, date, dateEditBody(newDateAttempt(t), editBaseOf(t, f.admin, f.ev.ID, date).Revision, changes, args...), http.StatusOK, "")
	}
	rows := []row{
		{"4 a church-wide event created public and private", func(t *testing.T, f *vcFixture) {
			for _, vis := range []string{models.VisibilityPublic, models.VisibilityPrivate} {
				apiDo(t, f.admin, "POST", "/api/v1/events", with(createEventBodyForTest(), "visibility", vis), http.StatusCreated)
			}
		}, nil},
		{"7 W2 an event tightened over a registrant, with the token", func(t *testing.T, f *vcFixture) {
			f.outsider(t, "o", 0)
			apiDo(t, f.admin, "PUT", f.eventPath(), confirmVisibility(t, f.admin, f.eventPath(), map[string]any{"visibility": io}), http.StatusOK)
		}, []string{consentChecked}},
		{"8 W2 a church-wide event, public to private and back", func(t *testing.T, f *vcFixture) {
			ev := requireCreatedEvent(t, f.app, f.admin.do("POST", "/api/v1/events", createEventBodyForTest(), true))
			path := fmt.Sprintf("/api/v1/events/%d/visibility", ev.ID)
			apiDo(t, f.admin, "PUT", path, map[string]any{"visibility": models.VisibilityPrivate, "consent": ""}, http.StatusOK)
			apiDo(t, f.admin, "PUT", path, map[string]any{"visibility": models.VisibilityPublic, "consent": ""}, http.StatusOK)
		}, []string{consentNoReview, consentNoReview}},
		{"9 W2+W3 a reset over one override and over none", func(t *testing.T, f *vcFixture) {
			reset := map[string]any{"visibility": models.VisibilityPublic, "resetOverrides": true}
			f.setOverride(t, 0, &io)
			apiDo(t, f.admin, "PUT", f.eventPath(), confirmVisibility(t, f.admin, f.eventPath(), reset), http.StatusOK)
			apiDo(t, f.admin, "PUT", f.eventPath(), confirmVisibility(t, f.admin, f.eventPath(), reset), http.StatusOK)
		}, []string{consentChecked, consentChecked}},
		{"10 W3 the guarded POST: more open, more private with nobody cancelled, changed, cleared, the typed reason", func(t *testing.T, f *vcFixture) {
			f.setSeries(t, models.VisibilityPrivate)
			guarded(t, f, 0, map[string]any{"visibility": models.VisibilityPublic}, "")
			guarded(t, f, 1, map[string]any{"visibility": io}, "")
			guarded(t, f, 0, map[string]any{"visibility": io}, "")
			guarded(t, f, 0, map[string]any{"visibility": "series"}, "")
			guarded(t, f, 2, map[string]any{"visibility": models.VisibilityPublic}, "", "reason", "series_reset")
		}, []string{consentNoReview, consentNoReview, consentNoReview, consentNoReview, consentNoReview}},
		{"11 W3 the guarded POST tightening over a registrant with the reviewed token", func(t *testing.T, f *vcFixture) {
			f.outsider(t, "o", 0)
			changes := map[string]any{"visibility": io}
			guarded(t, f, 0, changes, dateEditConsent(t, f.admin, f.ev.ID, f.day(0), changes))
		}, []string{consentChecked}},
		{"13 W4 a group tightened over a registrant, with the token", func(t *testing.T, f *vcFixture) {
			f.outsider(t, "o", 0)
			apiDo(t, f.admin, "PUT", f.groupPath(), confirmVisibility(t, f.admin, f.groupPath(), map[string]any{"visibility": io}), http.StatusOK)
		}, []string{consentChecked}},
	}
	opened := func(t *testing.T, f *vcFixture, decision string) map[string]any {
		// A private group with a leader-opened event and an opened date, an
		// outsider signed up on each.
		ev := requireCreatedEvent(t, f.app, f.admin.do("POST", "/api/v1/groups/"+itoa(f.gid)+"/events", with(createEventBodyForTest(), "visibility", models.VisibilityPublic), true))
		var occ models.EventOccurrence
		gormMust(t, f.app.DB.Where("event_id = ?", ev.ID).First(&occ))
		u := newTestUser(t, f.app, "opened", "password1234", models.RoleMember)
		gormMust(t, f.app.DB.Create(&models.Registration{UserID: &u.ID, OccurrenceID: occ.ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
		f.setOverride(t, 1, strp(models.VisibilityPublic))
		f.outsider(t, "date", 1)
		f.takeDeclared()
		return map[string]any{"visibility": io, "openedEvents": map[string]string{itoa(ev.ID): decision, dateDecisionKey(f.dates[1].ID): decision}}
	}
	rows = append(rows,
		row{"14 W5+W3 a leader-opened event and an opened date kept, nobody cancelled", func(t *testing.T, f *vcFixture) {
			apiDo(t, f.admin, "PUT", f.groupPath(), with(opened(t, f, openedKeep), "consent", ""), http.StatusOK)
		}, []string{consentNoReview}},
		row{"15 W4+W3 the same tightened over a registrant, with the token", func(t *testing.T, f *vcFixture) {
			body := opened(t, f, openedTighten)
			apiDo(t, f.admin, "PUT", f.groupPath(), confirmVisibility(t, f.admin, f.groupPath(), body), http.StatusOK)
		}, []string{consentChecked}},
	)
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			vis := models.VisibilityPublic
			if strings.HasPrefix(r.name, "14") || strings.HasPrefix(r.name, "15") {
				vis = models.VisibilityPrivate
			}
			f := newVCFixture(t, vis)
			locked := gated(t, f)
			f.takeDeclared()
			r.run(t, f)
			got := f.takeDeclared()
			if !slices.Equal(got, r.want) {
				t.Fatalf("declared %v, want %v", got, r.want)
			}
			var checked int64
			for _, d := range r.want {
				if d == consentChecked {
					checked++
				}
			}
			if n := locked.Load(); n != checked {
				t.Fatalf("%d epoch locks, want one per checked confirmation (%d)", n, checked)
			}
		})
	}
	t.Run("a group event with its own visibility whose room is already booked", func(t *testing.T) {
		f := newVCFixture(t, models.VisibilityPublic)
		if err := setSettingIn(f.app.DB, settingRoomsEnabled, "true"); err != nil {
			t.Fatal(err)
		}
		room := models.Room{Name: "Fellowship Hall", Building: "North"}
		gormMust(t, f.app.DB.Create(&room))
		body := with(createEventBodyForTest(), "roomId", room.ID)
		apiDo(t, f.admin, "POST", "/api/v1/groups/"+itoa(f.gid)+"/events", body, http.StatusCreated)
		f.takeDeclared()
		audits := auditCount(t, f.app, "event.visibility_changed") + auditCount(t, f.app, "event.visibility_override")
		apiDo(t, f.admin, "POST", "/api/v1/groups/"+itoa(f.gid)+"/events", with(with(body, "title", "Overlapping"), "visibility", models.VisibilityPrivate), http.StatusCreated)
		if got := f.takeDeclared(); !slices.Equal(got, []string{consentNoReview}) {
			t.Fatalf("the creation over a booked room declared %v", got)
		}
		if n := auditCount(t, f.app, "event.visibility_changed") + auditCount(t, f.app, "event.visibility_override") - audits; n != 1 {
			t.Fatalf("the creation over a booked room wrote %d visibility rows, want its one W1 row", n)
		}
	})
}

// routeBody adds the group's decision for the leader-opened date the epoch
// tests give it (date 1, made public).
func (f *vcFixture) routeBody(path string, body map[string]any) map[string]any {
	if path != f.groupPath() {
		return body
	}
	return with(body, "openedEvents", map[string]string{dateDecisionKey(f.dates[1].ID): openedTighten})
}

// secondPool opens another pool on the test's database, for a second process.
func secondPool(t *testing.T, app *App) *gorm.DB {
	t.Helper()
	var name string
	gormMust(t, app.DB.Raw("SELECT current_database()").Scan(&name))
	db, err := testdb.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			sqlDB.Close()
		}
	})
	return db
}

// waitingOn finds the backend whose current statement matches like and that
// waits on a lock held by blocker.
func waitingOn(t *testing.T, db *gorm.DB, like string, blocker int) int {
	t.Helper()
	var pid int
	testdb.WaitUntil(t, "a statement "+like+" to wait on its blocker", 10*time.Second, func() bool {
		gormMust(t, db.Raw(`SELECT coalesce(max(pid), 0) FROM pg_stat_activity WHERE query LIKE ? AND wait_event_type = 'Lock' AND ? = ANY(pg_blocking_pids(pid))`, like, blocker).Scan(&pid))
		return pid != 0
	})
	return pid
}

// TestConsentEpochCheckIsOrderedWithTheBoot (C24): a checked confirmation
// holds the epoch FOR SHARE, so another process's boot rotation (FOR NO KEY
// UPDATE) waits for it to end, and a confirmation behind an uncommitted
// rotation waits and reads the new epoch. No transaction that validated the old
// epoch is still in progress when the rotation returns.
func TestConsentEpochCheckIsOrderedWithTheBoot(t *testing.T) {
	// prepare previews the route's change and returns the preview's token
	// and the confirmation that sends a token.
	type send func(consent string) *httptest.ResponseRecorder
	type route struct {
		name    string
		prepare func(t *testing.T, f *vcFixture) (string, send)
	}
	put := func(name string, path func(f *vcFixture) string, body map[string]any) route {
		return route{name, func(t *testing.T, f *vcFixture) (string, send) {
			p := path(f)
			b := f.routeBody(p, body)
			return f.preview(t, p, b).Consent, func(c string) *httptest.ResponseRecorder { return f.admin.do("PUT", p, with(b, "consent", c), true) }
		}}
	}
	routes := []route{
		put("event tightening", (*vcFixture).eventPath, map[string]any{"visibility": models.VisibilityInviteOnly}),
		put("event reset", (*vcFixture).eventPath, map[string]any{"visibility": models.VisibilityInviteOnly, "resetOverrides": true}),
		put("group PUT", (*vcFixture).groupPath, map[string]any{"visibility": models.VisibilityInviteOnly}),
		{"guarded date POST", func(t *testing.T, f *vcFixture) (string, send) {
			date := models.FormatCalendarDate(f.dates[0].OriginalDate)
			changes := map[string]any{"visibility": models.VisibilityInviteOnly}
			token := dateEditConsent(t, f.admin, f.ev.ID, date, changes)
			revision := editBaseOf(t, f.admin, f.ev.ID, date).Revision
			return token, func(c string) *httptest.ResponseRecorder {
				return postDateEdit(f.admin, f.ev.ID, date, dateEditBody(newDateAttempt(t), revision, changes, "consent", c))
			}
		}},
	}
	for _, r := range routes {
		t.Run(r.name+"/the confirmation first", func(t *testing.T) {
			f := newVCFixture(t, models.VisibilityPublic)
			out := f.outsider(t, "outsider", 0)
			f.setOverride(t, 1, strp(models.VisibilityPublic))
			token, confirm := r.prepare(t, f)
			e1, _ := consentEpochIn(f.app.DB)
			type parkedTx struct{ pid, xid int64 }
			parked := make(chan parkedTx, 1)
			release := make(chan struct{})
			t.Cleanup(func() { closeOnce(release) })
			f.app.hooks.consentEpochLocked = func(tx *gorm.DB, epoch string) {
				var p parkedTx
				if err := tx.Raw("SELECT pg_backend_pid(), txid_current()").Row().Scan(&p.pid, &p.xid); err != nil {
					t.Error(err)
				}
				if epoch != e1 {
					t.Errorf("the confirmation read %q, want %q", epoch, e1)
				}
				parked <- p
				<-release
			}
			answer := make(chan *httptest.ResponseRecorder, 1)
			go func() { answer <- confirm(token) }()
			var p parkedTx
			select {
			case p = <-parked:
			case w := <-answer:
				t.Fatalf("the confirmation answered before it checked: %d %s", w.Code, w.Body.String())
			}
			b := &App{DB: secondPool(t, f.app)}
			statusAtRotation := ""
			b.hooks.bootRotated = func(tx *gorm.DB) error {
				return tx.Raw(fmt.Sprintf("SELECT pg_xact_status('%d'::xid8)", p.xid)).Scan(&statusAtRotation).Error
			}
			booted := make(chan error, 1)
			go func() { booted <- b.bootRestoreFence() }()
			waitingOn(t, f.app.DB, "%ON CONFLICT (key) DO UPDATE%", int(p.pid))
			closeOnce(release)
			if w := <-answer; w.Code != http.StatusOK {
				t.Fatalf("the parked confirmation: %d %s", w.Code, w.Body.String())
			}
			select {
			case err := <-booted:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(20 * time.Second):
				t.Fatal("the boot never finished")
			}
			if statusAtRotation != "committed" {
				t.Fatalf("the old-epoch confirmation was %q when the rotation returned", statusAtRotation)
			}
			if got := regStatus(t, f.app, out.ID, f.dates[0].ID); got != models.RegCancelled {
				t.Fatalf("the reviewed registrant is %s", got)
			}
			if e2, _ := consentEpochIn(f.app.DB); e2 == e1 {
				t.Fatal("the boot did not rotate the epoch")
			}
		})
		t.Run(r.name+"/the boot first", func(t *testing.T) {
			f := newVCFixture(t, models.VisibilityPublic)
			f.outsider(t, "outsider", 0)
			f.setOverride(t, 1, strp(models.VisibilityPublic))
			token, confirm := r.prepare(t, f)
			b := &App{DB: secondPool(t, f.app)}
			bootPID := make(chan int, 1)
			releaseBoot := make(chan struct{})
			t.Cleanup(func() { closeOnce(releaseBoot) })
			b.hooks.bootRotated = func(tx *gorm.DB) error {
				var pid int
				gormMust(t, tx.Raw("SELECT pg_backend_pid()").Scan(&pid))
				bootPID <- pid
				<-releaseBoot
				return nil
			}
			booted := make(chan error, 1)
			go func() { booted <- b.bootRestoreFence() }()
			pid := <-bootPID
			locked := make(chan string, 1)
			f.app.hooks.consentEpochLocked = func(_ *gorm.DB, epoch string) { locked <- epoch }
			before := f.state(t)
			answer := make(chan *httptest.ResponseRecorder, 1)
			go func() { answer <- confirm(token) }()
			waitingOn(t, f.app.DB, "%FOR SHARE%", pid)
			select {
			case <-locked:
				t.Fatal("the confirmation read the epoch behind an uncommitted rotation")
			default:
			}
			closeOnce(releaseBoot)
			if err := <-booted; err != nil {
				t.Fatal(err)
			}
			e2, _ := consentEpochIn(f.app.DB)
			if got := <-locked; got != e2 {
				t.Fatalf("the confirmation read %q after the boot, want the new %q", got, e2)
			}
			fresh := wantDecision(t, <-answer)
			if f.state(t) != before {
				t.Fatal("a token from before the boot wrote")
			}
			f.app.hooks.consentEpochLocked = nil
			if w := confirm(fresh.Consent); w.Code != http.StatusOK {
				t.Fatalf("the 409's token: %d %s", w.Code, w.Body.String())
			}
		})
	}
	t.Run("under load", func(t *testing.T) {
		// Twelve checked confirmations (event resets, always review-required)
		// over three events on two instances, and four boots on a third, all
		// at once. Inside every boot no confirmation that read a replaced
		// epoch is still in progress.
		f := newVCFixture(t, models.VisibilityPublic)
		paths := []string{f.eventPath()}
		for i := 0; i < 2; i++ {
			ev := requireCreatedEvent(t, f.app, f.admin.do("POST", "/api/v1/groups/"+itoa(f.gid)+"/events", createEventBodyForTest(), true))
			paths = append(paths, fmt.Sprintf("/api/v1/events/%d/visibility", ev.ID))
		}
		second := appOn(t, secondPool(t, f.app), f.app.Config.SessionSecret)
		clients := []*apiClient{f.admin, loginAPI(t, second, testEmail("admin"), "password1234")}
		type validated struct {
			xid   int64
			epoch string
		}
		var mu sync.Mutex
		var seen []validated
		record := func(tx *gorm.DB, epoch string) {
			var xid int64
			if err := tx.Raw("SELECT txid_current()").Scan(&xid).Error; err != nil {
				t.Error(err)
			}
			mu.Lock()
			seen = append(seen, validated{xid, epoch})
			mu.Unlock()
		}
		f.app.hooks.consentEpochLocked, second.hooks.consentEpochLocked = record, record
		direct := secondPool(t, f.app)
		b := &App{DB: secondPool(t, f.app)}
		var violations []string
		b.hooks.bootRotated = func(tx *gorm.DB) error {
			var now string
			if err := tx.Raw("SELECT value FROM settings WHERE key = ?", consentEpochKey).Scan(&now).Error; err != nil {
				return err
			}
			mu.Lock()
			old := slices.Clone(seen)
			mu.Unlock()
			for _, v := range old {
				if v.epoch == now {
					continue
				}
				var st string
				if err := direct.Raw(fmt.Sprintf("SELECT pg_xact_status('%d'::xid8)", v.xid)).Scan(&st).Error; err != nil {
					return err
				}
				if st == "in progress" {
					mu.Lock()
					violations = append(violations, fmt.Sprintf("xid %d read %s and is in progress", v.xid, v.epoch))
					mu.Unlock()
				}
			}
			return nil
		}
		reset := map[string]any{"visibility": models.VisibilityPublic, "resetOverrides": true}
		type result struct {
			path  string
			code  int
			fresh string
			err   string
		}
		results := make(chan result, 12)
		boots := make(chan error, 4)
		var wg sync.WaitGroup
		for i := 0; i < 12; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				path := paths[i%3]
				c := clients[i%2]
				time.Sleep(time.Duration(i*7%40) * time.Millisecond)
				pw := c.do("GET", visibilityPreviewPath(t, path, reset), nil, false)
				var impact visibilityImpactJSON
				if pw.Code != http.StatusOK || json.Unmarshal(pw.Body.Bytes(), &impact) != nil {
					results <- result{path: path, err: fmt.Sprintf("preview %d %s", pw.Code, pw.Body.String())}
					return
				}
				time.Sleep(time.Duration(i*13%50) * time.Millisecond)
				w := clients[(i+1)%2].do("PUT", path, with(reset, "consent", impact.Consent), true)
				r := result{path: path, code: w.Code}
				var a decisionAnswer
				if w.Code == http.StatusConflict && json.Unmarshal(w.Body.Bytes(), &a) == nil && a.NeedsDecision {
					r.fresh = a.Impact.Consent
				} else if w.Code != http.StatusOK {
					r.err = fmt.Sprintf("%d %s", w.Code, w.Body.String())
				}
				results <- r
			}(i)
		}
		for i := 0; i < 4; i++ {
			go func(i int) {
				time.Sleep(time.Duration(i*25) * time.Millisecond)
				boots <- b.bootRestoreFence()
			}(i)
		}
		for i := 0; i < 4; i++ {
			select {
			case err := <-boots:
				if err != nil {
					t.Fatalf("a boot under load: %v (SQLSTATE %s)", err, pgCode(err))
				}
			case <-time.After(20 * time.Second):
				t.Fatal("a boot under load never finished")
			}
		}
		wg.Wait()
		close(results)
		f.app.hooks.consentEpochLocked, second.hooks.consentEpochLocked = nil, nil
		for r := range results {
			if r.err != "" {
				t.Fatalf("a confirmation under load: %s", r.err)
			}
			if r.code == http.StatusConflict {
				// A 409 minted before a later boot is stale in its turn and
				// asks once more; the token it then gives saves.
				w := f.admin.do("PUT", r.path, with(reset, "consent", r.fresh), true)
				if w.Code == http.StatusConflict {
					w = f.admin.do("PUT", r.path, with(reset, "consent", wantDecision(t, w).Consent), true)
				}
				if w.Code != http.StatusOK {
					t.Fatalf("a 409's token after the load: %d %s", w.Code, w.Body.String())
				}
			}
		}
		if len(violations) > 0 {
			t.Fatalf("the ordering invariant failed inside a boot: %v", violations)
		}
	})
	t.Run("a boot that rolls back rotates nothing", func(t *testing.T) {
		f := newVCFixture(t, models.VisibilityPublic)
		f.outsider(t, "outsider", 0)
		body := confirmVisibility(t, f.admin, f.eventPath(), map[string]any{"visibility": models.VisibilityInviteOnly})
		e1, _ := consentEpochIn(f.app.DB)
		b := &App{DB: secondPool(t, f.app)}
		b.hooks.bootRotated = func(*gorm.DB) error { return fmt.Errorf("the boot stopped") }
		if err := b.bootRestoreFence(); err == nil {
			t.Fatal("a failed boot returned nil")
		}
		if e, _ := consentEpochIn(f.app.DB); e != e1 {
			t.Fatal("a rolled-back boot rotated the epoch")
		}
		apiDo(t, f.admin, "PUT", f.eventPath(), body, http.StatusOK)
	})
}

// --- Source guards (DEC-158) ---

// enclosingFunc is the FuncDecl in file that contains pos.
func enclosingFunc(file *ast.File, pos token.Pos) *ast.FuncDecl {
	for _, d := range file.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Pos() <= pos && pos < fd.End() {
			return fd
		}
	}
	return nil
}

// callsIn lists the calls named name in root, with the node stack of each.
func callsIn(root ast.Node, name string) [][]ast.Node {
	var out [][]ast.Node
	guardkit.InspectStack(root, func(n ast.Node, stack []ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && guardkit.Callee(call) == name {
			out = append(out, slices.Clone(stack))
		}
		return true
	})
	return out
}

// atTopLevel is whether a call (its stack) is in no loop and in no function
// literal passed to a Transaction call.
func atTopLevel(stack []ast.Node) bool {
	for i, n := range stack {
		switch n.(type) {
		case *ast.ForStmt, *ast.RangeStmt:
			return false
		case *ast.FuncLit:
			if i > 0 {
				if call, ok := stack[i-1].(*ast.CallExpr); ok && (guardkit.Callee(call) == "Transaction" || guardkit.Callee(call) == "fencedContactMutation") {
					return false
				}
			}
		}
	}
	return true
}

var visibilityActionWriters = map[string]string{
	"apiCreateEvent":               "api_events.go",
	"apiSetEventVisibility":        "api_event_access.go",
	"applyGroupTightening":         "api_group_visibility.go",
	"changeDateVisibilitySelected": "api_occurrence_visibility.go",
}

// auditWriterProblems: every reference to an event visibility audit action in
// production Go lies in a listed writer, and each writer but
// changeDateVisibilitySelected (whose callers are checked by
// consentPlanProblems) runs checkConsentIn, at its top level, before its first
// reference. other holds non-Go files, where no INSERT of either action may
// appear outside 00015.
func auditWriterProblems(sources map[string]string, other map[string]string) []string {
	parsed := guardkit.Sources(sources).Parse(0)
	bad := append([]string(nil), parsed.Problems...)
	seen := map[string]bool{}
	for _, f := range parsed.Files {
		if strings.HasSuffix(f.Path, "_test.go") || f.Path == "internal/audit/audit.go" || strings.HasPrefix(f.Path, "internal/notify/") || f.Path == "account_erasure_audit.go" {
			continue
		}
		auditNames := guardkit.ImportNames(f.AST, "fold/internal/audit")
		first := map[*ast.FuncDecl]token.Pos{}
		ast.Inspect(f.AST, func(n ast.Node) bool {
			ref := false
			switch x := n.(type) {
			case *ast.SelectorExpr:
				if id, ok := x.X.(*ast.Ident); ok && auditNames[id.Name] && (x.Sel.Name == "VisibilityChanged" || x.Sel.Name == "VisibilityOverride") {
					ref = true
				}
			case *ast.BasicLit:
				ref = x.Kind == token.STRING && (x.Value == `"event.visibility_changed"` || x.Value == `"event.visibility_override"`)
			}
			if !ref {
				return true
			}
			fd := enclosingFunc(f.AST, n.Pos())
			if fd == nil || visibilityActionWriters[fd.Name.Name] != f.Path {
				bad = append(bad, fmt.Sprintf("%s:%d: an event visibility audit action outside the listed writers", f.Path, parsed.Fset.Position(n.Pos()).Line))
				return true
			}
			seen[fd.Name.Name] = true
			if p, ok := first[fd]; !ok || n.Pos() < p {
				first[fd] = n.Pos()
			}
			return true
		})
		for fd, ref := range first {
			if fd.Name.Name == "changeDateVisibilitySelected" {
				continue
			}
			ok := false
			for _, stack := range callsIn(fd, "checkConsentIn") {
				if stack[len(stack)-1].Pos() < ref && atTopLevel(stack) {
					ok = true
				}
			}
			if !ok {
				bad = append(bad, fmt.Sprintf("%s: %s references an event visibility action before it runs checkConsentIn at its top level", f.Path, fd.Name.Name))
			}
		}
	}
	for name := range visibilityActionWriters {
		if !seen[name] {
			bad = append(bad, name+": a listed writer no longer references an action; update visibilityActionWriters")
		}
	}
	insertRe := regexp.MustCompile(`(?is)INSERT.*event\.visibility_(changed|override)`)
	for p, text := range other {
		if p != "migrations/00015_visibility_consent.sql" && insertRe.MatchString(text) {
			bad = append(bad, p+": inserts an event visibility audit row outside the app")
		}
	}
	return bad
}

// scriptTree is every .sql, shell and JavaScript file outside the frontend's
// dependencies: migrations, deploy/, scripts/, .cursor/ and web/e2e.
func scriptTree(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, root := range []string{"migrations", "deploy", "scripts", ".cursor", ".github", "web/e2e"} {
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				if d != nil && d.Name() == "node_modules" {
					return fs.SkipDir
				}
				return nil
			}
			ext := filepath.Ext(p)
			if ext == ".sql" || ext == ".sh" || ext == ".js" || ext == ".ts" || ext == ".mjs" || ext == "" {
				b, err := os.ReadFile(p)
				if err == nil {
					out[filepath.ToSlash(p)] = string(b)
				}
			}
			return nil
		})
	}
	return out
}

func TestEventVisibilityAuditWritersDeclareConsent(t *testing.T) {
	sources := guardkit.Production(t)
	scripts := scriptTree(t)
	for _, v := range auditWriterProblems(sources, scripts) {
		t.Error(v)
	}
	plant := func(path, src string) map[string]string {
		out := maps.Clone(sources)
		out[path] = src
		return out
	}
	const head = "package main\nimport \"fold/internal/audit\"\n"
	for name, mutated := range map[string]map[string]string{
		"a writer in an unlisted function": plant("x.go", head+"func f() { _ = audit.Visibility"+"Changed }\n"),
		"a listed writer with no call":     plant("api_events.go", strings.Replace(sources["api_events.go"], "plan, _, err := a.checkConsentIn(", "plan, _, err := a.checkConsentInX(", 1)),
		"a call after the reference":       plant("y.go", head+"func (a *App) apiCreateEvent() { _ = audit.Visibility"+"Override; a.checkConsentIn() }\n"),
		"a call inside a Transaction":      plant("y.go", head+"func (a *App) apiCreateEvent() { a.DB.Transaction(func(tx *gorm.DB) error { a.checkConsentIn(); return nil }); _ = audit.Visibility"+"Override }\n"),
	} {
		if name == "a call after the reference" || name == "a call inside a Transaction" {
			delete(mutated, "api_events.go")
		}
		if len(auditWriterProblems(mutated, scripts)) == 0 {
			t.Errorf("%s: the guard missed it", name)
		}
	}
	withScript := maps.Clone(scripts)
	withScript["deploy/x.sql"] = "INSERT INTO audit_logs (action) VALUES ('event.visibility" + "_changed');"
	if len(auditWriterProblems(sources, withScript)) == 0 {
		t.Error("an INSERT in deploy/: the guard missed it")
	}
}

// consentPlanProblems: the consent marker is set only in checkConsentIn;
// every checkConsentIn call is at its function's top level and before any
// SavePoint there; every caller of changeDateVisibilitySelected calls
// checkConsentIn once and earlier; every caller of checkConsentIn calls Done;
// cancelSelectedLosers takes one copy and verifies it first; the event route
// resets only plan.ResetDates().
func consentPlanProblems(sources map[string]string) []string {
	parsed := guardkit.Sources(sources).Parse(0)
	bad := append([]string(nil), parsed.Problems...)
	markers := 0
	for _, f := range parsed.Files {
		ast.Inspect(f.AST, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING && strings.Contains(lit.Value, "set_config('fold.visibility_consent'") {
				markers++
				if fd := enclosingFunc(f.AST, lit.Pos()); fd == nil || fd.Name.Name != "checkConsentIn" || f.Path != "visibility_consent.go" {
					bad = append(bad, f.Path+": sets the visibility consent marker outside checkConsentIn")
				}
			}
			return true
		})
		for _, d := range f.AST.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			checks := callsIn(fd, "checkConsentIn")
			if fd.Name.Name == "checkConsentIn" {
				continue
			}
			for _, stack := range checks {
				call := stack[len(stack)-1]
				if !atTopLevel(stack) {
					bad = append(bad, fmt.Sprintf("%s: %s runs checkConsentIn in a loop or a nested transaction", f.Path, fd.Name.Name))
				}
				for _, sp := range callsIn(fd, "SavePoint") {
					if sp[len(sp)-1].Pos() < call.Pos() {
						bad = append(bad, fmt.Sprintf("%s: %s runs checkConsentIn after a SavePoint", f.Path, fd.Name.Name))
					}
				}
			}
			if len(checks) > 0 && len(callsIn(fd, "Done")) == 0 {
				bad = append(bad, fmt.Sprintf("%s: %s runs checkConsentIn and never returns plan.Done()", f.Path, fd.Name.Name))
			}
			if fd.Name.Name == "changeDateVisibilitySelected" {
				continue
			}
			for _, stack := range callsIn(fd, "changeDateVisibilitySelected") {
				if len(checks) != 1 || checks[0][len(checks[0])-1].Pos() > stack[len(stack)-1].Pos() {
					bad = append(bad, fmt.Sprintf("%s: %s calls changeDateVisibilitySelected without one earlier checkConsentIn", f.Path, fd.Name.Name))
				}
				if fd.Name.Name == "apiSetEventVisibility" {
					inReset := false
					for _, n := range stack {
						if r, ok := n.(*ast.RangeStmt); ok {
							if call, ok := r.X.(*ast.CallExpr); ok && guardkit.Callee(call) == "ResetDates" {
								inReset = true
							}
						}
					}
					if !inReset {
						bad = append(bad, "api_event_access.go: the event route resets a date that is not from plan.ResetDates()")
					}
				}
			}
		}
	}
	if markers != 1 {
		bad = append(bad, fmt.Sprintf("the visibility consent marker is set %d times, want once", markers))
	}
	if f := parsed.File("api_group_visibility.go"); f != nil {
		fd := guardkit.FindFunc(f.AST, "cancelSelectedLosers")
		if fd == nil || len(fd.Body.List) < 3 {
			bad = append(bad, "api_group_visibility.go: cancelSelectedLosers is missing")
		} else {
			src := func(n ast.Node) string {
				return guardkit.Squash(f.Src[parsed.Fset.Position(n.Pos()).Offset:parsed.Fset.Position(n.End()).Offset])
			}
			if src(fd.Body.List[0]) != "sel := c.Selection()" {
				bad = append(bad, "cancelSelectedLosers: the first statement is not sel := c.Selection()")
			}
			if src(fd.Body.List[1]) != "if h := a.hooks.consentSelection; h != nil { h(&sel) }" {
				bad = append(bad, "cancelSelectedLosers: only the nil-checked consentSelection seam may sit before Verify")
			}
			if !strings.HasPrefix(src(fd.Body.List[2]), "if err := c.Verify(sel.ConsentIdentities()); err != nil {") {
				bad = append(bad, "cancelSelectedLosers: the copy is not verified before anything else")
			}
			if n := len(callsIn(fd, "Selection")); n != 1 {
				bad = append(bad, fmt.Sprintf("cancelSelectedLosers: Selection() called %d times, want once", n))
			}
		}
	}
	return bad
}

func TestVisibilityCancellationsConsumeACheckedPlan(t *testing.T) {
	sources := guardkit.Production(t)
	for _, v := range consentPlanProblems(sources) {
		t.Error(v)
	}
	mutate := func(path, old, new string) map[string]string {
		out := maps.Clone(sources)
		if !strings.Contains(out[path], old) {
			t.Fatalf("fixture: %s has no %q", path, old)
		}
		out[path] = strings.Replace(out[path], old, new, 1)
		return out
	}
	for name, mutated := range map[string]map[string]string{
		"a second marker":                    mutate("api_events.go", "func (a *App) apiCreateEvent(", "func marker(tx *gorm.DB) { tx.Exec(\"SELECT set_config('fold.visibility_"+"consent', 'v1', true)\") }\nfunc (a *App) apiCreateEvent("),
		"a date writer with no check":        mutate("api_date_edits.go", "plan, token, err := a.checkConsentIn(tx, &edit.intent.consent, h, b.entries)", "plan, token, err := a.checkConsentInX(tx, &edit.intent.consent, h, b.entries)"),
		"a check in a loop":                  mutate("api_group_visibility.go", "plan, token, err := a.checkConsentIn(tx, given, h, b.entries)", "var plan *consent.Plan[dateLosers]; var token string; for range 1 { plan, token, err = a.checkConsentIn(tx, given, h, b.entries) }"),
		"no Done":                            mutate("api_event_access.go", "return plan.Done()", "return nil"),
		"a read of the plan's copy again":    mutate("api_group_visibility.go", "dates, rows, now := sel.dates, sel.rows, sel.now", "dates, rows, now := c.Selection().dates, sel.rows, sel.now"),
		"a statement before Verify":          mutate("api_group_visibility.go", "if h := a.hooks.consentSelection; h != nil {\n\t\th(&sel)\n\t}", "sel.rows = nil"),
		"a reset of a date outside the plan": mutate("api_event_access.go", "for _, id := range plan.ResetDates() {", "for _, id := range []uint64{} {"),
	} {
		if len(consentPlanProblems(mutated)) == 0 {
			t.Errorf("%s: the guard missed it", name)
		}
	}
}

// consentEpochProblems: the checked confirmation reads the epoch FOR SHARE,
// once, through lockConsentEpochIn in checkConsentIn only; the plain read is
// consentTokenIn's alone; nothing fenced checks a token; and the rotation is
// the boot's last statement but its test seam.
func consentEpochProblems(sources map[string]string) []string {
	parsed := guardkit.Sources(sources).Parse(0)
	bad := append([]string(nil), parsed.Problems...)
	f := parsed.File("visibility_consent.go")
	if f == nil {
		return append(bad, "visibility_consent.go is missing")
	}
	foundConst := false
	ast.Inspect(f.AST, func(n ast.Node) bool {
		if vs, ok := n.(*ast.ValueSpec); ok && len(vs.Names) == 1 && vs.Names[0].Name == "consentEpochLockSQL" && len(vs.Values) == 1 {
			foundConst = true
			if lit, ok := vs.Values[0].(*ast.BasicLit); !ok || lit.Value != `"SELECT value FROM settings WHERE key = ? FOR SHARE"` {
				bad = append(bad, "consentEpochLockSQL is not exactly SELECT value FROM settings WHERE key = ? FOR SHARE")
			}
		}
		return true
	})
	if !foundConst {
		bad = append(bad, "consentEpochLockSQL is missing")
	}
	locks, plains := 0, 0
	for _, pf := range parsed.Files {
		for _, d := range pf.AST.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			for range callsIn(fd, "lockConsentEpochIn") {
				locks++
				if fd.Name.Name != "checkConsentIn" {
					bad = append(bad, pf.Path+": "+fd.Name.Name+" locks the consent epoch; only checkConsentIn may")
				}
			}
			for range callsIn(fd, "consentEpochIn") {
				plains++
				if fd.Name.Name != "consentTokenIn" {
					bad = append(bad, pf.Path+": "+fd.Name.Name+" reads the consent epoch plainly; only consentTokenIn may")
				}
			}
			for _, stack := range callsIn(fd, "checkConsentIn") {
				if !atTopLevel(stack) {
					bad = append(bad, pf.Path+": "+fd.Name.Name+" checks consent inside a fenced or nested transaction")
				}
			}
			if fd.Name.Name == "checkConsentIn" && len(callsIn(fd, "consentTokenIn")) > 0 {
				bad = append(bad, "checkConsentIn mints a token with the plain epoch read")
			}
		}
	}
	if locks != 1 {
		bad = append(bad, fmt.Sprintf("lockConsentEpochIn is called %d times, want once", locks))
	}
	if plains != 1 {
		bad = append(bad, fmt.Sprintf("consentEpochIn is called %d times, want once", plains))
	}
	if lf := guardkit.FindFunc(f.AST, "lockConsentEpochIn"); lf != nil {
		src := f.Src[parsed.Fset.Position(lf.Pos()).Offset:parsed.Fset.Position(lf.End()).Offset]
		if strings.Count(src, "consentEpochLockSQL") != 1 || strings.Contains(src, "NOWAIT") || strings.Contains(src, "SKIP LOCKED") {
			bad = append(bad, "lockConsentEpochIn does not read consentEpochLockSQL exactly once")
		}
	}
	rf := parsed.File("restore_fence.go")
	if rf == nil {
		return append(bad, "restore_fence.go is missing")
	}
	boot := guardkit.FindFunc(rf.AST, "bootRestoreFence")
	if boot == nil {
		return append(bad, "bootRestoreFence is missing")
	}
	var lit *ast.FuncLit
	ast.Inspect(boot, func(n ast.Node) bool {
		if l, ok := n.(*ast.FuncLit); ok && lit == nil {
			lit = l
		}
		return true
	})
	if lit == nil || len(lit.Body.List) < 3 {
		return append(bad, "bootRestoreFence has no transaction body")
	}
	list := lit.Body.List
	text := func(n ast.Node) string {
		return guardkit.Squash(rf.Src[parsed.Fset.Position(n.Pos()).Offset:parsed.Fset.Position(n.End()).Offset])
	}
	rot := -1
	for i, st := range list {
		if strings.Contains(text(st), "consentEpochRotateSQL") {
			rot = i
		}
	}
	if rot < 0 || rot != len(list)-3 || !strings.HasPrefix(text(list[rot+1]), "if h := a.hooks.bootRotated; h != nil {") || text(list[rot+2]) != "return nil" {
		bad = append(bad, "bootRestoreFence: the epoch rotation is not its last statement before the test seam")
	}
	return bad
}

func TestConsentEpochIsLockedOnlyWhereATokenIsChecked(t *testing.T) {
	sources := guardkit.Production(t)
	for _, v := range consentEpochProblems(sources) {
		t.Error(v)
	}
	mutate := func(path, old, new string) map[string]string {
		out := maps.Clone(sources)
		if !strings.Contains(out[path], old) {
			t.Fatalf("fixture: %s has no %q", path, old)
		}
		out[path] = strings.Replace(out[path], old, new, 1)
		return out
	}
	const lockSQL = `"SELECT value FROM settings WHERE key = ? FOR SHARE"`
	for name, mutated := range map[string]map[string]string{
		"the plain read":                 mutate("visibility_consent.go", lockSQL, `"SELECT value FROM settings WHERE key = ?"`),
		"FOR KEY SHARE":                  mutate("visibility_consent.go", lockSQL, `"SELECT value FROM settings WHERE key = ? FOR KEY SHARE"`),
		"NOWAIT":                         mutate("visibility_consent.go", lockSQL, `"SELECT value FROM settings WHERE key = ? FOR SHARE NOWAIT"`),
		"SKIP LOCKED":                    mutate("visibility_consent.go", lockSQL, `"SELECT value FROM settings WHERE key = ? FOR SHARE SKIP LOCKED"`),
		"a second call":                  mutate("visibility_consent.go", "func (a *App) consentTokenIn(", "func second(a *App, tx *gorm.DB) { a.lockConsentEpochIn(tx) }\nfunc (a *App) consentTokenIn("),
		"a plain read in the check":      mutate("visibility_consent.go", "if h.Epoch, err = a.lockConsentEpochIn(tx); err != nil {", "if h.Epoch, err = consentEpochIn(tx); err != nil {"),
		"a fenced caller":                mutate("visibility_consent.go", "func (a *App) consentTokenIn(", "func fenced(a *App) { a.fencedContactMutation(nil, contactChange{}, func(tx *gorm.DB) (*contactSeal, error) { a.checkConsentIn(tx, nil, consent.Header{}, nil); return nil, nil }) }\nfunc (a *App) consentTokenIn("),
		"a statement after the rotation": mutate("restore_fence.go", "if h := a.hooks.bootRotated; h != nil {", "tx.Exec(\"SELECT 1\")\n\t\tif h := a.hooks.bootRotated; h != nil {"),
	} {
		if len(consentEpochProblems(mutated)) == 0 {
			t.Errorf("%s: the guard missed it", name)
		}
	}
}

// TestConsentEpochHasOneWriter: the epoch row's key is named only where it is
// declared, refused, rotated and read, and no SQL file but 00015 names it.
func TestConsentEpochHasOneWriter(t *testing.T) {
	allowed := map[string]bool{"settings.go:": true, "restore_fence.go:refuseRestoreFenceKey": true, "restore_fence.go:bootRestoreFence": true,
		"visibility_consent.go:consentEpochIn": true, "visibility_consent.go:lockConsentEpochIn": true}
	parsed := guardkit.Production(t).Parse(0)
	for _, f := range parsed.Files {
		ast.Inspect(f.AST, func(n ast.Node) bool {
			named := false
			switch x := n.(type) {
			case *ast.Ident:
				named = x.Name == "consentEpochKey"
			case *ast.BasicLit:
				named = x.Kind == token.STRING && strings.Contains(x.Value, "visibility_consent_epoch")
			}
			if !named {
				return true
			}
			where := f.Path + ":"
			if fd := enclosingFunc(f.AST, n.Pos()); fd != nil {
				where += fd.Name.Name
			}
			if !allowed[where] {
				t.Errorf("%s names the consent epoch row", where)
			}
			return true
		})
	}
	for p, text := range scriptTree(t) {
		if strings.Contains(text, "visibility_consent_epoch") && p != "migrations/00015_visibility_consent.sql" {
			t.Errorf("%s names the consent epoch row", p)
		}
	}
}

// TestVisibilityConsentReadsTheInjectedClock: the previews, the writes and
// their selections read a.now, never the wall clock, so a preview and its
// write judge a grant's end on one clock.
func TestVisibilityConsentReadsTheInjectedClock(t *testing.T) {
	listed := map[string]bool{"apiDateVisibilityImpact": true, "apiEventVisibilityImpact": true, "apiGroupVisibilityImpact": true,
		"editDateIn": true, "apiSetEventVisibility": true, "applyGroupTightening": true,
		"selectDateLosers": true, "lockGroupDates": true, "openedDatesAt": true, "changeDateVisibilitySelected": true, "cancelSelectedLosers": true}
	sources := guardkit.Production(t)
	parsed := sources.Parse(0)
	for _, f := range parsed.Files {
		whole := f.Path == "visibility_consent.go" || strings.HasPrefix(f.Path, "internal/consent/")
		for _, d := range f.AST.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || (!whole && !listed[fd.Name.Name]) {
				continue
			}
			ast.Inspect(fd, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "Now" {
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == "time" {
						t.Errorf("%s: %s reads time.Now", f.Path, fd.Name.Name)
					}
				}
				return true
			})
		}
	}
}

// TestDatabaseRecoveryFloorIsNotAboveTheBinary: the floor the migrations
// install is at most this binary's level, in the files and in the database.
func TestDatabaseRecoveryFloorIsNotAboveTheBinary(t *testing.T) {
	floorRe := regexp.MustCompile(`THEN declared::int ELSE 0 END\) < (\d+) THEN`)
	floor := 0
	entries, err := os.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		b, err := os.ReadFile(filepath.Join("migrations", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range floorRe.FindAllStringSubmatch(string(b), -1) {
			n, _ := strconv.Atoi(m[1])
			floor = max(floor, n)
		}
	}
	if floor == 0 || floor > recoveryPolicyLevel {
		t.Fatalf("the migrations' recovery floor is %d, the binary's level %d", floor, recoveryPolicyLevel)
	}
	app := testApp(t)
	var def string
	gormMust(t, app.DB.Raw("SELECT pg_get_functiondef('settings_recovery_floor'::regproc)").Scan(&def))
	m := floorRe.FindStringSubmatch(def)
	if m == nil {
		t.Fatalf("the installed floor function has no floor: %s", def)
	}
	if n, _ := strconv.Atoi(m[1]); n > recoveryPolicyLevel {
		t.Fatalf("the database's floor %d is above the binary's level %d", n, recoveryPolicyLevel)
	}
}

// --- The lifecycle matrix (design 8.3) ---

// lifecycleRoute is one confirmation a lifecycle row can move.
type lifecycleRoute struct {
	name string
	path func(f *vcFixture) string
	body map[string]any
}

var lifecycleRoutes = []lifecycleRoute{
	{"date", func(f *vcFixture) string { return f.datePath(0) }, map[string]any{"visibility": models.VisibilityInviteOnly}},
	{"event", (*vcFixture).eventPath, map[string]any{"visibility": models.VisibilityInviteOnly}},
	{"reset", (*vcFixture).eventPath, map[string]any{"visibility": models.VisibilityInviteOnly, "resetOverrides": true}},
	{"group", (*vcFixture).groupPath, map[string]any{"visibility": models.VisibilityInviteOnly}},
}

// lifecyclePeople: o1 and o2 (outsiders) on date 0, o4 (an outsider) on
// date 1, o1's household person on date 0, and m, a member, on date 0, who
// never loses the group's dates.
type lifecyclePeople struct {
	o1, o2, o4, m *models.User
	// kit is o1's household person, signed up for date 0 by o1.
	kit uint
}

func newLifecycleFixture(t *testing.T) (*vcFixture, *lifecyclePeople) {
	t.Helper()
	f := newVCFixture(t, models.VisibilityPublic)
	p := &lifecyclePeople{o1: f.outsider(t, "o1", 0), o2: f.outsider(t, "o2", 0), o4: f.outsider(t, "o4", 1)}
	p.m = newTestUser(t, f.app, "m", "password1234", models.RoleMember)
	var g models.Group
	gormMust(t, f.app.DB.First(&g, f.gid))
	addMember(t, f.app, p.m, &g, models.MembershipMember)
	gormMust(t, f.app.DB.Create(&models.Registration{UserID: &p.m.ID, OccurrenceID: f.dates[0].ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
	holder := f.client(t, p.o1)
	p.kit = addMyPerson(t, holder, map[string]any{"firstName": "Kit", "kind": "adult", "birthDate": "1990-05-01"}).ID
	apiDo(t, holder, "PUT", fmt.Sprintf("/api/v1/events/%d/registrations", f.ev.ID), map[string]any{"date": models.FormatCalendarDate(f.dates[0].OriginalDate),
		"people": []map[string]any{{"personId": p.kit, "status": models.RegGoing}}}, http.StatusOK)
	return f, p
}

// decided is body with a keep decision for every leader-opened row the group
// would show now, as the review asks the leader for them.
func (f *vcFixture) decided(t *testing.T, path string, body map[string]any) map[string]any {
	t.Helper()
	if path != f.groupPath() {
		return body
	}
	decisions := map[string]string{}
	for _, r := range f.preview(t, path, body).LeaderOpened {
		key := r.DecisionKey
		if key == "" {
			key = itoa(r.ID)
		}
		decisions[key] = "keep"
	}
	if len(decisions) == 0 {
		return body
	}
	return with(body, "openedEvents", decisions)
}

// everything is the state a refused confirmation must leave as it was: the
// event's rows, the group, and every audit, notice and job row.
func (f *vcFixture) everything(t *testing.T) string {
	t.Helper()
	var g models.Group
	gormMust(t, f.app.DB.First(&g, f.gid))
	var events []models.Event
	gormMust(t, f.app.DB.Where("group_id = ?", f.gid).Order("id").Find(&events))
	b, _ := json.Marshal(struct {
		Group  string
		Events []models.Event
	}{g.Visibility, events})
	return f.state(t) + string(b)
}

func cancelledCount(t *testing.T, app *App) int64 {
	t.Helper()
	var n int64
	gormMust(t, app.DB.Model(&models.Registration{}).Where("status = ?", models.RegCancelled).Count(&n))
	return n
}

// reviewedRegistrants is how many registrations the review says the change
// cancels (each person and household person here holds one registration in
// the series).
func reviewedRegistrants(impact visibilityImpactJSON) int64 {
	var n int64
	for _, r := range impact.Tightened {
		n += r.Registrants
	}
	return n
}

func (f *vcFixture) client(t *testing.T, u *models.User) *apiClient {
	t.Helper()
	return loginAPI(t, f.app, u.Email, "password1234")
}

func (f *vcFixture) register(t *testing.T, u *models.User, i int, status string) {
	t.Helper()
	apiDo(t, f.client(t, u), "PUT", fmt.Sprintf("/api/v1/events/%d/registrations", f.ev.ID),
		map[string]any{"status": status, "date": models.FormatCalendarDate(f.dates[i].OriginalDate)}, http.StatusOK)
}

// TestVisibilityConsentLifecycleMatrix (C5, design 8.3): each committed
// transition that moves a bound fact makes the old token's confirmation a 409
// decision with a fresh impact and another token, writing nothing; the fresh
// token then cancels exactly the fresh set. A transition that leaves every
// bound fact equal leaves the old token valid. Each transition runs through
// its real handler. Rows 20 and 21 are TestVisibilityConsentAcrossServeRestart
// and TestVisibilityConsentAcrossSecrets, 22 is TestEventResetIsReviewedWithNoLosers,
// 23 is TestEventResetBindsTheCurrentSeries' boundary case, and 24 and 25 are
// TestConsentEpochCheckIsOrderedWithTheBoot.
func TestVisibilityConsentLifecycleMatrix(t *testing.T) {
	all := map[string]bool{"date": true, "event": true, "reset": true, "group": true}
	none := map[string]bool{"date": false, "event": false, "reset": false, "group": false}
	only := func(moves ...string) map[string]bool {
		out := maps.Clone(none)
		for _, m := range moves {
			out[m] = true
		}
		return out
	}
	without := func(m map[string]bool, route string) map[string]bool {
		out := maps.Clone(m)
		delete(out, route)
		return out
	}
	rows := []struct {
		name  string
		moves map[string]bool
		apply func(t *testing.T, f *vcFixture, p *lifecyclePeople)
	}{
		{"1 a new going RSVP", all, func(t *testing.T, f *vcFixture, p *lifecyclePeople) {
			f.register(t, newTestUser(t, f.app, "o3", "password1234", models.RoleMember), 0, models.RegGoing)
		}},
		{"2 a same-count swap", all, func(t *testing.T, f *vcFixture, p *lifecyclePeople) {
			f.register(t, p.o1, 0, models.RegDeclined)
			f.register(t, newTestUser(t, f.app, "o3", "password1234", models.RoleMember), 0, models.RegGoing)
		}},
		{"3 a manager removes a registrant", all, func(t *testing.T, f *vcFixture, p *lifecyclePeople) {
			var reg models.Registration
			gormMust(t, f.app.DB.Where("user_id = ? AND occurrence_id = ?", p.o1.ID, f.dates[0].ID).First(&reg))
			apiDo(t, f.admin, "POST", fmt.Sprintf("/api/v1/events/%d/registrations/remove", f.ev.ID),
				map[string]any{"registrationId": reg.ID, "date": models.FormatCalendarDate(f.dates[0].OriginalDate)}, http.StatusOK)
		}},
		{"5 a.now passes the first date", without(only("event", "reset", "group"), "date"), func(t *testing.T, f *vcFixture, p *lifecyclePeople) {
			later := f.dates[0].EndsAt.Add(time.Minute)
			f.app.clock = func() time.Time { return later }
		}},
		{"6 an outsider is added to the group", all, func(t *testing.T, f *vcFixture, p *lifecyclePeople) {
			apiDo(t, f.admin, "POST", "/api/v1/groups/"+itoa(f.gid)+"/members", map[string]any{"email": p.o1.Email}, http.StatusOK)
		}},
		{"7 an outsider becomes an admin", all, func(t *testing.T, f *vcFixture, p *lifecyclePeople) {
			apiDo(t, f.admin, "PUT", "/api/v1/admin/users/"+itoa(p.o1.ID), map[string]any{"role": models.RoleAdmin}, http.StatusOK)
		}},
		{"9 a guest's sign-up is redeemed", all, func(t *testing.T, f *vcFixture, p *lifecyclePeople) {
			body := guestBody("Guest Visitor", "guest-visitor@example.com", true)
			body["date"] = models.FormatCalendarDate(f.dates[0].OriginalDate)
			guestSubmit(t, anonClient(t, f.app, "10.13.0.9"), f.ev.ID, body, http.StatusOK)
			token := tokenInEmail(t, latestEmail(t, f.app, "guest-visitor@example.com"))
			apiDo(t, anonClient(t, f.app, "10.13.0.9"), "POST", "/api/v1/guest-registrations/confirm", map[string]string{"token": token}, http.StatusOK)
		}},
		{"11 a household person is deleted", all, func(t *testing.T, f *vcFixture, p *lifecyclePeople) {
			apiDo(t, f.client(t, p.o1), "DELETE", "/api/v1/me/household/people/"+itoa(p.kit), nil, http.StatusOK)
		}},
		{"12 a household person's kind changes", all, func(t *testing.T, f *vcFixture, p *lifecyclePeople) {
			apiDo(t, f.client(t, p.o1), "PUT", "/api/v1/me/household/people/"+itoa(p.kit), map[string]any{"firstName": "Kit", "kind": "child", "birthDate": "2018-05-01"}, http.StatusOK)
		}},
		{"16 another manager retitles a date with its own visibility", only("reset"), func(t *testing.T, f *vcFixture, p *lifecyclePeople) {
			saveDate(t, f.admin, f.ev.ID, f.day(2), map[string]any{"title": "Harvest evening"}, http.StatusOK, "")
		}},
		{"15 the event is renamed", all, func(t *testing.T, f *vcFixture, p *lifecyclePeople) {
			apiDo(t, f.admin, "PUT", fmt.Sprintf("/api/v1/events/%d", f.ev.ID), map[string]any{"title": "Renamed evening",
				"startsLocal": models.FormatCalendarDate(f.dates[0].OriginalDate) + "T18:00", "durationMinutes": 60, "tzid": "UTC", "recurrenceRule": "FREQ=WEEKLY;COUNT=4"}, http.StatusOK)
		}},
		{"17 another date gets its own visibility", only("event", "reset", "group"), func(t *testing.T, f *vcFixture, p *lifecyclePeople) {
			saveDate(t, f.admin, f.ev.ID, f.day(2), map[string]any{"visibility": models.VisibilityPrivate}, http.StatusOK, "")
		}},
		{"18 another manager changes the series", all, func(t *testing.T, f *vcFixture, p *lifecyclePeople) {
			body := map[string]any{"visibility": models.VisibilityPrivate}
			apiDo(t, f.admin, "PUT", f.eventPath(), confirmVisibility(t, f.admin, f.eventPath(), body), http.StatusOK)
		}},
		{"19 a new event in the group", only("group"), func(t *testing.T, f *vcFixture, p *lifecyclePeople) {
			apiDo(t, f.admin, "POST", "/api/v1/groups/"+itoa(f.gid)+"/events", createEventBodyForTest(), http.StatusCreated)
		}},
		{"stays: a member's RSVP", none, func(t *testing.T, f *vcFixture, p *lifecyclePeople) {
			m2 := newTestUser(t, f.app, "m2", "password1234", models.RoleMember)
			apiDo(t, f.admin, "POST", "/api/v1/groups/"+itoa(f.gid)+"/members", map[string]any{"email": m2.Email}, http.StatusOK)
			f.register(t, m2, 0, models.RegGoing)
		}},
		{"stays: a member who is not a loser is disabled", none, func(t *testing.T, f *vcFixture, p *lifecyclePeople) {
			apiDo(t, f.admin, "PUT", "/api/v1/admin/users/"+itoa(p.m.ID), map[string]any{"status": models.UserDisabled}, http.StatusOK)
		}},
	}
	matrix := identityRows()
	for _, row := range rows {
		apply := row.apply
		matrix = append(matrix, lifecycleRow{name: row.name, moves: row.moves,
			apply: func(t *testing.T, f *vcFixture, p *lifecyclePeople, _ *lifecycleExtra) { apply(t, f, p) }})
	}
	for _, row := range matrix {
		for _, r := range lifecycleRoutes {
			moves, run := row.moves[r.name]
			if !run {
				continue
			}
			t.Run(row.name+"/"+r.name, func(t *testing.T) {
				f, p := newLifecycleFixture(t)
				// Row 16's date has its own visibility before the review, so
				// the reset lists it with its title.
				if strings.HasPrefix(row.name, "16 ") {
					saveDate(t, f.admin, f.ev.ID, f.day(2), map[string]any{"visibility": models.VisibilityPrivate}, http.StatusOK, "")
				}
				x := &lifecycleExtra{}
				if row.setup != nil {
					row.setup(t, f, p, x)
				}
				path := r.path(f)
				old := f.preview(t, path, r.body)
				row.apply(t, f, p, x)
				send := func(body map[string]any, token string) *httptest.ResponseRecorder {
					return f.admin.do("PUT", path, with(body, "consent", token), true)
				}
				if r.name == "date" {
					// The guarded edit on a base read after the transition, so
					// only the consent can be stale; the answer to its
					// question resends the same attempt.
					base := editBaseOf(t, f.admin, f.ev.ID, f.day(0))
					attempt := newDateAttempt(t)
					send = func(body map[string]any, token string) *httptest.ResponseRecorder {
						return postDateEdit(f.admin, f.ev.ID, f.day(0), dateEditBody(attempt, base.Revision, body, "consent", token))
					}
				}
				body := f.decided(t, path, r.body)
				before, cancelled := f.everything(t), cancelledCount(t, f.app)
				w := send(body, old.Consent)
				reviewed := old
				if moves {
					reviewed = wantDecision(t, w)
					if reviewed.Consent == old.Consent {
						t.Fatal("the 409 carried the old token")
					}
					if after := f.everything(t); after != before {
						t.Fatalf("the old token's confirmation wrote:\n%s\nwant\n%s", after, before)
					}
					if w := send(f.decided(t, path, r.body), reviewed.Consent); w.Code != http.StatusOK {
						t.Fatalf("the fresh token's confirmation: %d %s", w.Code, w.Body.String())
					}
				} else if w.Code != http.StatusOK {
					t.Fatalf("a transition that moves no bound fact refused the old token: %d %s", w.Code, w.Body.String())
				}
				if got, want := cancelledCount(t, f.app)-cancelled, reviewedRegistrants(reviewed); got != want {
					t.Fatalf("the confirmation cancelled %d registrations, the review said %d", got, want)
				}
				if got := regStatus(t, f.app, p.m.ID, f.dates[0].ID); got == models.RegCancelled {
					t.Fatal("the member's registration was cancelled")
				}
				if row.check != nil {
					row.check(t, f, p, x)
				}
			})
		}
	}
}

// lifecycleRow is one transition of the matrix. setup runs before the
// review, apply between the review and the old token's confirmation, and
// check after the confirmation that cancelled the reviewed set; the three
// share x.
type lifecycleRow struct {
	name  string
	moves map[string]bool
	setup func(t *testing.T, f *vcFixture, p *lifecyclePeople, x *lifecycleExtra)
	apply func(t *testing.T, f *vcFixture, p *lifecyclePeople, x *lifecycleExtra)
	check func(t *testing.T, f *vcFixture, p *lifecyclePeople, x *lifecycleExtra)
}

// lifecycleExtra is the person a row adds in its setup, signed up for date 0.
type lifecycleExtra struct {
	u    *models.User
	reg  uint
	teen models.User
	link uint
}

func (f *vcFixture) adminUser(t *testing.T) *models.User {
	t.Helper()
	var u models.User
	gormMust(t, f.app.DB.Where("email = ?", testEmail("admin")).First(&u))
	return &u
}

func (f *vcFixture) userPath(u *models.User) string { return "/api/v1/admin/users/" + itoa(u.ID) }

// signedUp registers u for date 0 through the self-service RSVP and records
// the row in x.
func (f *vcFixture) signedUp(t *testing.T, u *models.User, x *lifecycleExtra) {
	t.Helper()
	f.register(t, u, 0, models.RegGoing)
	var r models.Registration
	gormMust(t, f.app.DB.Where("user_id = ? AND occurrence_id = ?", u.ID, f.dates[0].ID).First(&r))
	x.u, x.reg = u, r.ID
}

func (x *lifecycleExtra) status(t *testing.T, f *vcFixture) string {
	t.Helper()
	var r models.Registration
	gormMust(t, f.app.DB.First(&r, x.reg))
	return r.Status
}

// wantStatus requires x's row to have status after the confirmation.
func wantStatus(status string) func(t *testing.T, f *vcFixture, p *lifecyclePeople, x *lifecycleExtra) {
	return func(t *testing.T, f *vcFixture, p *lifecyclePeople, x *lifecycleExtra) {
		t.Helper()
		if got := x.status(t, f); got != status {
			t.Fatalf("the row the transition moved is %s after the confirmation, want %s", got, status)
		}
	}
}

// identityRows are design 8.3's rows 4, 8, 10, 13 and 14 through their real
// entry points, and the outsider forms of 8 and 10 that move nothing.
func identityRows() []lifecycleRow {
	all := map[string]bool{"date": true, "event": true, "reset": true, "group": true}
	none := map[string]bool{"date": false, "event": false, "reset": false, "group": false}
	invitee := func(t *testing.T, f *vcFixture, p *lifecyclePeople, x *lifecycleExtra) {
		x.u = newTestUser(t, f.app, "invitee", "password1234", models.RoleMember)
		x.reg = seedReadInvitation(t, f.app, f.adminUser(t), x.u, f.ev, f.dates[0]).RegistrationID
	}
	activeAdmin := func(t *testing.T, f *vcFixture, p *lifecyclePeople, x *lifecycleExtra) {
		f.signedUp(t, newTestUser(t, f.app, "a2", "password1234", models.RoleAdmin), x)
	}
	remove := func(t *testing.T, f *vcFixture, p *lifecyclePeople, x *lifecycleExtra) {
		apiDo(t, f.admin, "POST", fmt.Sprintf("/api/v1/events/%d/registrations/remove", f.ev.ID),
			map[string]any{"registrationId": x.reg, "date": models.FormatCalendarDate(f.dates[0].OriginalDate)}, http.StatusOK)
	}
	setStatus := func(status string) func(t *testing.T, f *vcFixture, p *lifecyclePeople, x *lifecycleExtra) {
		return func(t *testing.T, f *vcFixture, p *lifecyclePeople, x *lifecycleExtra) {
			apiDo(t, f.admin, "PUT", f.userPath(x.u), map[string]any{"status": status}, http.StatusOK)
		}
	}
	return []lifecycleRow{
		// Row 4. Ending a grant through a manager's Remove or a kept date's
		// Remove also ends the answer (status removed), so the row goes from
		// kept by its grant to not current: no selection moves, and the
		// old token holds. A grant that ends while the answer stays current
		// (the invitee's account disabled, which ends the grant, then
		// enabled again, which restores the answer) adds the row to every
		// selection.
		{name: "4 a manager removes an exact-date invitee", moves: none, setup: invitee, apply: remove,
			check: wantStatus(models.RegRemoved)},
		{name: "4 a kept date is removed (DEC-74)", moves: none,
			setup: func(t *testing.T, f *vcFixture, p *lifecyclePeople, x *lifecycleExtra) {
				leaver := newTestUser(t, f.app, "leaver", "password1234", models.RoleMember)
				var g models.Group
				gormMust(t, f.app.DB.First(&g, f.gid))
				addMember(t, f.app, leaver, &g, models.MembershipMember)
				f.signedUp(t, leaver, x)
				c := f.client(t, leaver)
				groupPath := f.groupPath()
				preview := getDeparturePreview(t, c, groupPath+"/departure-preview", http.StatusOK)
				if w := confirmDeparturePreview(t, c, "POST", groupPath+"/leave", preview, []uint{f.dates[0].ID}, ""); w.Code != http.StatusOK {
					t.Fatalf("leave keeping date 0: %d %s", w.Code, w.Body.String())
				}
				if access := loadAccessRow(t, f.app.DB, x.reg); access.Origin != policy.OccurrenceOriginDepartureSelf || access.Status != policy.OccurrenceAccessActive {
					t.Fatalf("fixture: the departure kept no grant: %+v", access)
				}
			}, apply: remove, check: wantStatus(models.RegRemoved)},
		{name: "4 an invitee's grant ends with the answer kept", moves: all, setup: invitee,
			apply: func(t *testing.T, f *vcFixture, p *lifecyclePeople, x *lifecycleExtra) {
				setStatus(models.UserDisabled)(t, f, p, x)
				if access := loadAccessRow(t, f.app.DB, x.reg); access.Status == policy.OccurrenceAccessActive {
					t.Fatalf("fixture: disabling the invitee left the grant live: %+v", access)
				}
				setStatus(models.UserActive)(t, f, p, x)
				if got := x.status(t, f); got != models.RegGoing {
					t.Fatalf("fixture: the re-enabled invitee's answer is %s", got)
				}
			}, check: wantStatus(models.RegCancelled)},
		// Row 8 and row 10 with an active admin, whom invite-only never shuts
		// out: disabling or erasing them adds their row to the selection,
		// and enabling them again takes it out.
		{name: "8 an active admin is disabled", moves: all, setup: activeAdmin, apply: setStatus(models.UserDisabled),
			check: wantStatus(models.RegCancelled)},
		{name: "8 a disabled admin is enabled again", moves: all,
			setup: func(t *testing.T, f *vcFixture, p *lifecyclePeople, x *lifecycleExtra) {
				activeAdmin(t, f, p, x)
				setStatus(models.UserDisabled)(t, f, p, x)
			}, apply: setStatus(models.UserActive), check: wantStatus(models.RegGoing)},
		{name: "10 an active admin is erased", moves: all, setup: activeAdmin,
			apply: func(t *testing.T, f *vcFixture, p *lifecyclePeople, x *lifecycleExtra) {
				apiDo(t, f.admin, "DELETE", f.userPath(x.u), nil, http.StatusOK)
			}, check: wantStatus(models.RegCancelled)},
		{name: "10 a legacy active admin tombstone is swept", moves: all,
			setup: func(t *testing.T, f *vcFixture, p *lifecyclePeople, x *lifecycleExtra) {
				activeAdmin(t, f, p, x)
				tombstoneLegacy(t, f.app.DB, x.u.ID)
			},
			apply: func(t *testing.T, f *vcFixture, p *lifecyclePeople, x *lifecycleExtra) {
				tombstoneRun(t, f.app, x.u.ID, tombstoneSweepVersion)
			}, check: wantStatus(models.RegCancelled)},
		// The outsider forms move nothing: an outsider's row is a loser
		// whatever the account's status, and disabling or erasing it
		// changes no identity column.
		{name: "stays: an outsider's account is disabled", moves: none,
			apply: func(t *testing.T, f *vcFixture, p *lifecyclePeople, x *lifecycleExtra) {
				apiDo(t, f.admin, "PUT", f.userPath(p.o1), map[string]any{"status": models.UserDisabled}, http.StatusOK)
			}},
		{name: "stays: an outsider's account is erased", moves: none,
			apply: func(t *testing.T, f *vcFixture, p *lifecyclePeople, x *lifecycleExtra) {
				apiDo(t, f.admin, "DELETE", f.userPath(p.o1), nil, http.StatusOK)
			}},
		// Row 13: a teen household person becomes an account. The last
		// prerequisite (the guardian link of the new account, after its
		// consent) moves the row from (holder, person) to the teen's user id.
		{name: "13 a teen household person becomes an account", moves: all,
			setup: func(t *testing.T, f *vcFixture, p *lifecyclePeople, x *lifecycleExtra) {
				holder := f.client(t, p.o1)
				person := addMyPerson(t, holder, map[string]any{"firstName": "Teen", "kind": "teen", "birthDate": time.Now().UTC().AddDate(-15, 0, 0).Format("2006-01-02")})
				apiDo(t, f.admin, "POST", "/api/v1/admin/guardian-links/"+itoa(*person.GuardianLinkID)+"/confirm", map[string]any{"checkNote": "Known household"}, http.StatusOK)
				apiDo(t, holder, "PUT", fmt.Sprintf("/api/v1/events/%d/registrations", f.ev.ID), map[string]any{"date": models.FormatCalendarDate(f.dates[0].OriginalDate),
					"people": []map[string]any{{"personId": person.ID, "status": models.RegGoing}}}, http.StatusOK)
				var r models.Registration
				gormMust(t, f.app.DB.Where("household_person_id = ? AND occurrence_id = ?", person.ID, f.dates[0].ID).First(&r))
				x.reg = r.ID
				acceptConversion(t, f.app, inviteHouseholdPerson(t, f.app, holder, person.ID, "teen-own@example.com"), http.StatusOK)
				req := pendingRequest(t, f.app, "teen-own@example.com")
				apiDo(t, f.admin, "POST", "/api/v1/admin/account-requests/"+itoa(req.ID)+"/approve", map[string]any{}, http.StatusOK)
				gormMust(t, f.app.DB.Where("email = ?", "teen-own@example.com").First(&x.teen))
				var link models.GuardianLink
				gormMust(t, f.app.DB.Where("teen_user_id = ? AND guardian_user_id = ?", x.teen.ID, p.o1.ID).First(&link))
				x.link = link.ID
				apiDo(t, f.admin, "POST", consentPath(&x.teen), map[string]string{"scope": "account", "status": "granted", "evidenceText": "Signed paper consent"}, http.StatusOK)
				if got := x.status(t, f); got != models.RegGoing {
					t.Fatalf("fixture: the teen's row is %s before the conversion", got)
				}
			},
			apply: func(t *testing.T, f *vcFixture, p *lifecyclePeople, x *lifecycleExtra) {
				apiDo(t, f.admin, "POST", "/api/v1/admin/guardian-links/"+itoa(x.link)+"/confirm", map[string]string{"checkNote": "Checked teen account relationship"}, http.StatusOK)
				var r models.Registration
				gormMust(t, f.app.DB.First(&r, x.reg))
				if r.UserID == nil || *r.UserID != x.teen.ID || r.HouseholdPersonID != nil || r.RegisteredBy != nil {
					t.Fatalf("fixture: the conversion did not move the row: %+v", r)
				}
			}, check: wantStatus(models.RegCancelled)},
		// Row 14: the one writer of registered_by on an existing row is the
		// conversion transfer (an existing household answer keeps its
		// holder, and erasure never writes user_id or registered_by). An
		// adult household person's conversion, at its approval, clears the
		// holder and sets the user.
		{name: "14 a household adult's conversion clears the holder", moves: all,
			setup: func(t *testing.T, f *vcFixture, p *lifecyclePeople, x *lifecycleExtra) {
				var r models.Registration
				gormMust(t, f.app.DB.Where("household_person_id = ? AND occurrence_id = ?", p.kit, f.dates[0].ID).First(&r))
				x.reg = r.ID
				acceptConversion(t, f.app, inviteHouseholdPerson(t, f.app, f.client(t, p.o1), p.kit, "kit-own@example.com"), http.StatusOK)
			},
			apply: func(t *testing.T, f *vcFixture, p *lifecyclePeople, x *lifecycleExtra) {
				req := pendingRequest(t, f.app, "kit-own@example.com")
				apiDo(t, f.admin, "POST", "/api/v1/admin/account-requests/"+itoa(req.ID)+"/approve", map[string]any{}, http.StatusOK)
				var r models.Registration
				gormMust(t, f.app.DB.First(&r, x.reg))
				if r.UserID == nil || r.HouseholdPersonID != nil || r.RegisteredBy != nil {
					t.Fatalf("fixture: the conversion did not move the holder: %+v", r)
				}
			}, check: wantStatus(models.RegCancelled)},
	}
}

// TestVisibilityConsentSerializesWithLifecycleWriters (C16, design 8.3): a
// lifecycle writer that starts while a checked tightening is parked after its
// check (holding its locks, before it cancels) waits for it, on the lock its
// class names, with no 40P01, and then runs on the tightened state. The
// tightening cancels exactly what it reviewed. The one documented cycle (an
// event-scoped gate holder against an RSVP's promotion) ends in one 40P01
// that fails one side whole.
func TestVisibilityConsentSerializesWithLifecycleWriters(t *testing.T) {
	type writer struct {
		name     string
		advisory bool // waits on the contact gate, not on a row
		start    func(t *testing.T, f *vcFixture, p *lifecyclePeople) func() error
		after    func(t *testing.T, f *vcFixture, p *lifecyclePeople)
	}
	writers := []writer{
		{"a gate taker: an outsider becomes an admin", true,
			func(t *testing.T, f *vcFixture, p *lifecyclePeople) func() error {
				return func() error {
					return doExpect(f.admin, "PUT", "/api/v1/admin/users/"+itoa(p.o1.ID), map[string]any{"role": models.RoleAdmin}, http.StatusOK)
				}
			},
			func(t *testing.T, f *vcFixture, p *lifecyclePeople) {
				var u models.User
				gormMust(t, f.app.DB.First(&u, p.o1.ID))
				if u.Role != models.RoleAdmin || regStatus(t, f.app, p.o1.ID, f.dates[0].ID) != models.RegCancelled {
					t.Fatalf("after the tightening and then the promotion: role %s, registration %s", u.Role, regStatus(t, f.app, p.o1.ID, f.dates[0].ID))
				}
			}},
		{"a registration locker without the gate: a new RSVP", false,
			func(t *testing.T, f *vcFixture, p *lifecyclePeople) func() error {
				late := f.client(t, newTestUser(t, f.app, "late", "password1234", models.RoleMember))
				date := models.FormatCalendarDate(f.dates[0].OriginalDate)
				return func() error {
					// On the tightened date the outsider cannot see it any more.
					w := late.do("PUT", fmt.Sprintf("/api/v1/events/%d/registrations", f.ev.ID), map[string]any{"status": models.RegGoing, "date": date}, true)
					if w.Code == http.StatusOK || w.Code == http.StatusInternalServerError {
						return fmt.Errorf("an RSVP to a date the person can no longer see answered %d: %s", w.Code, w.Body.String())
					}
					return nil
				}
			},
			func(t *testing.T, f *vcFixture, p *lifecyclePeople) {
				var n int64
				gormMust(t, f.app.DB.Model(&models.Registration{}).Where("user_id = (SELECT id FROM users WHERE email = ?)", testEmail("late")).Count(&n))
				if n != 0 {
					t.Fatalf("the refused RSVP left %d registrations", n)
				}
			}},
		{"an event-first writer: eventdates.Extend", false,
			func(t *testing.T, f *vcFixture, p *lifecyclePeople) func() error {
				db := secondPool(t, f.app)
				return func() error { return eventdates.Extend(db, time.Now().UTC(), nil) }
			},
			func(t *testing.T, f *vcFixture, p *lifecyclePeople) {}},
	}
	for _, r := range []lifecycleRoute{lifecycleRoutes[1], lifecycleRoutes[3]} {
		for _, wr := range writers {
			t.Run(r.name+"/"+wr.name, func(t *testing.T) {
				f, p := newLifecycleFixture(t)
				path := r.path(f)
				reviewed := f.preview(t, path, r.body)
				body := with(r.body, "consent", reviewed.Consent)
				parked := make(chan int, 1)
				release := make(chan struct{})
				t.Cleanup(func() { closeOnce(release) })
				var once sync.Once
				f.app.hooks.consentEpochLocked = func(tx *gorm.DB, _ string) {
					once.Do(func() {
						var pid int
						if err := tx.Raw("SELECT pg_backend_pid()").Scan(&pid).Error; err != nil {
							t.Error(err)
						}
						parked <- pid
						<-release
					})
				}
				cancelled := cancelledCount(t, f.app)
				answer := make(chan *httptest.ResponseRecorder, 1)
				go func() { answer <- f.admin.do("PUT", path, body, true) }()
				holder := <-parked
				run := wr.start(t, f, p)
				done := make(chan error, 1)
				go func() { done <- run() }()
				waiter := waitingOn(t, f.app.DB, "%", holder)
				var locktype string
				gormMust(t, f.app.DB.Raw("SELECT coalesce(min(locktype), '') FROM pg_locks WHERE pid = ? AND NOT granted", waiter).Scan(&locktype))
				if (locktype == "advisory") != wr.advisory {
					t.Fatalf("the writer waits on a %q lock, want the contact gate %v", locktype, wr.advisory)
				}
				select {
				case err := <-done:
					t.Fatalf("the writer finished while the tightening held its locks: %v", err)
				case <-time.After(200 * time.Millisecond):
				}
				closeOnce(release)
				if w := <-answer; w.Code != http.StatusOK {
					t.Fatalf("the parked tightening: %d %s", w.Code, w.Body.String())
				}
				select {
				case err := <-done:
					if err != nil {
						if pgCode(err) == "40P01" {
							t.Fatalf("deadlock: %v", err)
						}
						t.Fatal(err)
					}
				case <-time.After(20 * time.Second):
					t.Fatal("the writer never finished")
				}
				if got, want := cancelledCount(t, f.app)-cancelled, reviewedRegistrants(reviewed); got != want {
					t.Fatalf("the tightening cancelled %d registrations, the review said %d", got, want)
				}
				wr.after(t, f, p)
			})
		}
	}
	t.Run("the documented cycle: an event-scoped gate holder against an RSVP's promotion", func(t *testing.T) {
		// The waiter whose deadlock check first finds the cycle fails. The
		// tightening waits first, so it is the one when the cycle closes
		// inside its deadlock_timeout, and the RSVP is when it closes after.
		t.Run("the tightening fails", func(t *testing.T) { testTheDocumentedCycle(t, false) })
		t.Run("the RSVP fails", func(t *testing.T) { testTheDocumentedCycle(t, true) })
	})
}

// testTheDocumentedCycle (design 8.3, registration_locks.go): the event
// route is a contactMutation with an event scope, so its before pass locks
// the event's live grant rows ahead of its date locks, while a self-service
// RSVP holds a date and then promotes a waitlisted grant holder on it. The
// order is forced: a raw connection holds the member's own row so the RSVP
// waits there holding date 0, the tightening then locks the grant and waits
// on date 0, and releasing the raw row closes the cycle. The database ends
// it with a 40P01: exactly one request fails whole and writes nothing, and
// the other's result is what it would have done alone. late closes the
// cycle only after the tightening's deadlock check has run, so the RSVP's
// check is the one that finds it.
func testTheDocumentedCycle(t *testing.T, late bool) {
	f := newVCFixture(t, models.VisibilityPublic)
	o := f.outsider(t, "o", 1)
	gormMust(t, f.app.DB.Model(&models.Event{}).Where("id = ?", f.ev.ID).Update("capacity", 1))
	waiter := newTestUser(t, f.app, "waiter", "password1234", models.RoleMember)
	grant := seedReadInvitation(t, f.app, f.adminUser(t), waiter, f.ev, f.dates[0]).RegistrationID
	queued := time.Now().UTC().Add(-time.Hour)
	gormMust(t, f.app.DB.Model(&models.Registration{}).Where("id = ?", grant).Updates(map[string]any{"status": models.RegWaitlisted, "waitlisted_at": queued}))
	m := newTestUser(t, f.app, "m", "password1234", models.RoleMember)
	var g models.Group
	gormMust(t, f.app.DB.First(&g, f.gid))
	addMember(t, f.app, m, &g, models.MembershipMember)
	mine := models.Registration{UserID: &m.ID, OccurrenceID: f.dates[0].ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}
	gormMust(t, f.app.DB.Create(&mine))
	if access := loadAccessRow(t, f.app.DB, grant); access.Status != policy.OccurrenceAccessActive {
		t.Fatalf("fixture: the waiter's grant is not live: %+v", access)
	}
	body := confirmVisibility(t, f.admin, f.eventPath(), map[string]any{"visibility": models.VisibilityInviteOnly})
	member := loginHeld(t, f.app, m.Email, "password1234")
	admin := loginHeld(t, f.app, testEmail("admin"), "password1234")
	deadlocks := func() int64 {
		var n int64
		gormMust(t, f.app.DB.Raw("SELECT deadlocks FROM pg_stat_database WHERE datname = current_database()").Scan(&n))
		return n
	}
	before := deadlocks()

	raw := secondPool(t, f.app)
	held := raw.Begin()
	t.Cleanup(func() { held.Rollback() })
	var rawPID int
	gormMust(t, held.Raw("SELECT pg_backend_pid()").Scan(&rawPID))
	var locked []uint
	gormMust(t, held.Raw("SELECT id FROM registrations WHERE id = ? FOR UPDATE", mine.ID).Scan(&locked))

	rsvp := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rsvp <- member.do("PUT", fmt.Sprintf("/api/v1/events/%d/registrations", f.ev.ID),
			map[string]any{"status": models.RegDeclined, "date": models.FormatCalendarDate(f.dates[0].OriginalDate)}, true)
	}()
	rsvpPID := waitingOn(t, f.app.DB, "%", rawPID)
	tightening := make(chan *httptest.ResponseRecorder, 1)
	go func() { tightening <- admin.do("PUT", f.eventPath(), body, true) }()
	waitingOn(t, f.app.DB, "%", rsvpPID)
	if late {
		var timeout string
		gormMust(t, f.app.DB.Raw("SHOW deadlock_timeout").Scan(&timeout))
		d, err := time.ParseDuration(strings.TrimSuffix(timeout, "s") + "s")
		if err != nil {
			t.Fatalf("deadlock_timeout %q: %v", timeout, err)
		}
		time.Sleep(d + 500*time.Millisecond)
	}
	gormMust(t, held.Commit())

	answers := map[string]*httptest.ResponseRecorder{}
	for len(answers) < 2 {
		select {
		case w := <-rsvp:
			answers["rsvp"] = w
		case w := <-tightening:
			answers["tightening"] = w
		case <-time.After(30 * time.Second):
			t.Fatalf("the cycle never resolved: %v", answers)
		}
	}
	testdb.WaitUntil(t, "the database to count the deadlock", 10*time.Second, func() bool { return deadlocks() > before })
	rsvpOK, tightOK := answers["rsvp"].Code == http.StatusOK, answers["tightening"].Code == http.StatusOK
	if rsvpOK == tightOK || tightOK != late {
		t.Fatalf("want exactly one side to fail (the RSVP: %v): RSVP %d %s, tightening %d %s", late, answers["rsvp"].Code, answers["rsvp"].Body.String(), answers["tightening"].Code, answers["tightening"].Body.String())
	}
	var ev models.Event
	gormMust(t, f.app.DB.First(&ev, f.ev.ID))
	cancellations := auditCount(t, f.app, audit.RegistrationCancelled)
	statuses := func() (string, string, string) {
		return regStatus(t, f.app, m.ID, f.dates[0].ID), regStatus(t, f.app, waiter.ID, f.dates[0].ID), regStatus(t, f.app, o.ID, f.dates[1].ID)
	}
	mine2, waiter2, o2 := statuses()
	if tightOK {
		// The RSVP rolled back whole; the tightening alone cancels exactly
		// the outsider, and the waiter keeps the place their grant holds.
		if answers["rsvp"].Code != http.StatusInternalServerError || ev.Visibility != models.VisibilityInviteOnly || cancellations != 1 ||
			mine2 != models.RegGoing || waiter2 != models.RegWaitlisted || o2 != models.RegCancelled {
			t.Fatalf("after the RSVP's 40P01: RSVP %d, visibility %s, %d cancellations, member %s, waiter %s, outsider %s",
				answers["rsvp"].Code, ev.Visibility, cancellations, mine2, waiter2, o2)
		}
		return
	}
	// The tightening rolled back whole; the RSVP alone frees the seat and
	// promotes the waiter.
	if answers["tightening"].Code != http.StatusInternalServerError || ev.Visibility != models.VisibilityPublic || cancellations != 0 ||
		auditCount(t, f.app, audit.VisibilityChanged) != 0 || mine2 != models.RegDeclined || waiter2 != models.RegGoing || o2 != models.RegGoing {
		t.Fatalf("after the tightening's 40P01: tightening %d, visibility %s, %d cancellations, member %s, waiter %s, outsider %s",
			answers["tightening"].Code, ev.Visibility, cancellations, mine2, waiter2, o2)
	}
}

// TestConsentEpochLockAddsNoDeadlockWithTheGates (C25, design 2.5.2): a
// checked confirmation (A), another instance's boot (B) and a fenced admin
// account deletion on a third instance (P), in both arrival orders, end with
// no 40P01 and no false restore_detected: every holder of the epoch's share
// lock holds the exclusive gate, so no fenced ticket holder coexists with it.
func TestConsentEpochLockAddsNoDeadlockWithTheGates(t *testing.T) {
	setup := func(t *testing.T) (f *vcFixture, b, p *App, padmin *apiClient, victim *models.User, path string, body map[string]any) {
		f, _ = newLifecycleFixture(t)
		victim = newTestUser(t, f.app, "victim", "password1234", models.RoleMember)
		// P boots before anything parks (design 8.1: a boot behind a parked
		// confirmation would wait for it).
		p = appOn(t, secondPool(t, f.app), f.app.Config.SessionSecret)
		padmin = loginAPI(t, p, testEmail("admin"), "password1234")
		b = &App{DB: secondPool(t, f.app)}
		path = f.eventPath()
		body = confirmVisibility(t, f.admin, path, map[string]any{"visibility": models.VisibilityInviteOnly})
		return
	}
	boot := func(b *App) chan error {
		booted := make(chan error, 1)
		go func() { booted <- b.bootRestoreFence() }()
		return booted
	}
	within := func(t *testing.T, what string, c chan error) {
		t.Helper()
		select {
		case err := <-c:
			if err != nil {
				if pgCode(err) == "40P01" {
					t.Fatalf("%s deadlocked: %v", what, err)
				}
				t.Fatalf("%s: %v", what, err)
			}
		case <-time.After(20 * time.Second):
			t.Fatalf("%s never finished", what)
		}
	}
	noRestore := func(t *testing.T, apps ...*App) {
		t.Helper()
		for i, a := range apps {
			if a.restoreDetected() {
				t.Fatalf("instance %d noted a restore", i)
			}
		}
	}
	t.Run("the confirmation, then the boot, then the deletion", func(t *testing.T) {
		f, b, p, padmin, victim, path, body := setup(t)
		parked := make(chan int, 1)
		release := make(chan struct{})
		t.Cleanup(func() { closeOnce(release) })
		var once sync.Once
		f.app.hooks.consentEpochLocked = func(tx *gorm.DB, _ string) {
			once.Do(func() {
				var pid int
				gormMust(t, tx.Raw("SELECT pg_backend_pid()").Scan(&pid))
				parked <- pid
				<-release
			})
		}
		answer := make(chan *httptest.ResponseRecorder, 1)
		go func() { answer <- f.admin.do("PUT", path, body, true) }()
		holder := <-parked
		booted := boot(b)
		waitingOn(t, f.app.DB, "%ON CONFLICT (key) DO UPDATE%", holder)
		deleted := make(chan error, 1)
		go func() { deleted <- eraseVia(padmin, victim)() }()
		// The deletion waits on the gate, not on the restore_fence row the
		// boot now holds.
		waitingOn(t, f.app.DB, "SELECT pg_advisory_xact_lock(%", holder)
		closeOnce(release)
		if w := <-answer; w.Code != http.StatusOK {
			t.Fatalf("the parked confirmation: %d %s", w.Code, w.Body.String())
		}
		within(t, "the boot", booted)
		within(t, "the deletion", deleted)
		noRestore(t, f.app, b, p)
	})
	t.Run("the deletion, then the boot, then the confirmation", func(t *testing.T) {
		f, b, p, padmin, victim, path, body := setup(t)
		parked := make(chan int, 1)
		release := make(chan struct{})
		t.Cleanup(func() { closeOnce(release) })
		var once sync.Once
		p.hooks.deleteBeforeSeal = func(tx *gorm.DB) {
			once.Do(func() {
				var pid int
				gormMust(t, tx.Raw("SELECT pg_backend_pid()").Scan(&pid))
				parked <- pid
				<-release
			})
		}
		deleted := make(chan error, 1)
		go func() { deleted <- eraseVia(padmin, victim)() }()
		holder := <-parked
		booted := boot(b)
		waitingOn(t, f.app.DB, "%", holder)
		answer := make(chan *httptest.ResponseRecorder, 1)
		go func() { answer <- f.admin.do("PUT", path, body, true) }()
		waitingOn(t, f.app.DB, "SELECT pg_advisory_xact_lock(%", holder)
		closeOnce(release)
		within(t, "the deletion", deleted)
		within(t, "the boot", booted)
		w := <-answer
		switch w.Code {
		case http.StatusOK:
		case http.StatusConflict:
			// It reached the epoch after the boot's rotation: one more question.
			apiDo(t, f.admin, "PUT", path, with(body, "consent", wantDecision(t, w).Consent), http.StatusOK)
		default:
			t.Fatalf("the confirmation behind the deletion and the boot: %d %s", w.Code, w.Body.String())
		}
		noRestore(t, f.app, b, p)
	})
}

// twin previews body for path, sends it with the preview's token through
// send, and requires 200 and exactly the reviewed registrations cancelled:
// the tightened rows, and the leader-opened rows the decisions tighten.
func (f *vcFixture) twin(t *testing.T, path string, body map[string]any, decisions map[string]string, send func(consent string) *httptest.ResponseRecorder) {
	t.Helper()
	impact := f.preview(t, path, body)
	want := reviewedRegistrants(impact)
	for _, r := range impact.LeaderOpened {
		key := r.DecisionKey
		if key == "" {
			key = itoa(r.ID)
		}
		if decisions[key] == openedTighten {
			want += r.Registrants
		}
	}
	before := cancelledCount(t, f.app)
	if w := send(impact.Consent); w.Code != http.StatusOK {
		t.Fatalf("the preview's own token was refused: %d %s", w.Code, w.Body.String())
	}
	if got := cancelledCount(t, f.app) - before; got != want {
		t.Fatalf("the write cancelled %d registrations, its preview showed %d", got, want)
	}
}

// TestVisibilityConsentPreviewMatchesWrite (C2): on unchanged data the
// preview's token is the write's, on every route and change shape, so the
// write saves at once and cancels exactly what the preview showed.
func TestVisibilityConsentPreviewMatchesWrite(t *testing.T) {
	io := models.VisibilityInviteOnly
	put := func(f *vcFixture, path string, body map[string]any) func(string) *httptest.ResponseRecorder {
		return func(c string) *httptest.ResponseRecorder {
			return f.admin.do("PUT", path, with(body, "consent", c), true)
		}
	}
	day := func(f *vcFixture, i int) string { return models.FormatCalendarDate(f.dates[i].OriginalDate) }
	dateShapes := map[string]map[string]any{
		"a plain date":                   {"visibility": io},
		"moved later":                    {"visibility": io, "startsLocal": "", "durationMinutes": 90},
		"moved earlier":                  {"visibility": io, "startsLocal": ""},
		"retitled in the same save":      {"visibility": io, "title": "A new name for one evening"},
		"a time and visibility together": {"visibility": io, "startsLocal": "", "durationMinutes": 30},
	}
	for name, shape := range dateShapes {
		t.Run("guarded POST/"+name, func(t *testing.T) {
			f := newVCFixture(t, models.VisibilityPublic)
			f.outsider(t, "o1", 1)
			f.outsider(t, "o2", 1)
			changes := maps.Clone(shape)
			if _, ok := changes["startsLocal"]; ok {
				hour := "T21:00"
				if name == "moved earlier" {
					hour = "T08:00"
				}
				changes["startsLocal"] = day(f, 1) + hour
			}
			revision := editBaseOf(t, f.admin, f.ev.ID, day(f, 1)).Revision
			f.twin(t, f.datePath(1), changes, nil, func(c string) *httptest.ResponseRecorder {
				return postDateEdit(f.admin, f.ev.ID, day(f, 1), dateEditBody(newDateAttempt(t), revision, changes, "consent", c))
			})
		})
	}
	for _, reset := range []bool{false, true} {
		t.Run(fmt.Sprintf("an event with override dates, reset %v", reset), func(t *testing.T) {
			f := newVCFixture(t, models.VisibilityPublic)
			f.outsider(t, "o1", 0)
			f.outsider(t, "o2", 2)
			f.setOverride(t, 2, strp(models.VisibilityPublic))
			f.setOverride(t, 3, strp(models.VisibilityPrivate))
			body := map[string]any{"visibility": io, "resetOverrides": reset}
			f.twin(t, f.eventPath(), body, nil, put(f, f.eventPath(), body))
			if reset && (f.override(t, 2) != "series" || f.override(t, 3) != "series") {
				t.Fatal("the reviewed reset left an override")
			}
		})
	}
	for _, mix := range [][2]string{{openedKeep, openedKeep}, {openedKeep, openedTighten}, {openedTighten, openedKeep}, {openedTighten, openedTighten}} {
		t.Run("a group: event "+mix[0]+", date "+mix[1], func(t *testing.T) {
			// A private group with an auto event, a leader-opened event and a
			// leader-opened date, each with an outsider signed up.
			f := newVCFixture(t, models.VisibilityPrivate)
			f.outsider(t, "auto", 0)
			opened := requireCreatedEvent(t, f.app, f.admin.do("POST", "/api/v1/groups/"+itoa(f.gid)+"/events", with(createEventBodyForTest(), "visibility", models.VisibilityPublic), true))
			var occ models.EventOccurrence
			gormMust(t, f.app.DB.Where("event_id = ?", opened.ID).First(&occ))
			u := newTestUser(t, f.app, "opened", "password1234", models.RoleMember)
			gormMust(t, f.app.DB.Create(&models.Registration{UserID: &u.ID, OccurrenceID: occ.ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
			f.setOverride(t, 1, strp(models.VisibilityPublic))
			f.outsider(t, "date", 1)
			decisions := map[string]string{itoa(opened.ID): mix[0], dateDecisionKey(f.dates[1].ID): mix[1]}
			body := map[string]any{"visibility": io, "openedEvents": decisions}
			f.twin(t, f.groupPath(), body, decisions, put(f, f.groupPath(), body))
		})
	}
	// (e) a boundary moved with a.now between the preview and the write: the
	// first confirmation is asked again, and that 409's token saves at once.
	for _, r := range []lifecycleRoute{lifecycleRoutes[1], lifecycleRoutes[2], lifecycleRoutes[3]} {
		t.Run("a.now passes a date's end/"+r.name, func(t *testing.T) {
			f, _ := newLifecycleFixture(t)
			path := r.path(f)
			old := f.preview(t, path, r.body)
			later := f.dates[0].EndsAt.Add(time.Minute)
			f.app.clock = func() time.Time { return later }
			fresh := wantDecision(t, f.admin.do("PUT", path, with(r.body, "consent", old.Consent), true))
			before := cancelledCount(t, f.app)
			apiDo(t, f.admin, "PUT", path, with(r.body, "consent", fresh.Consent), http.StatusOK)
			if got, want := cancelledCount(t, f.app)-before, reviewedRegistrants(fresh); got != want {
				t.Fatalf("cancelled %d, the 409 showed %d", got, want)
			}
		})
	}
}

// TestVisibilityConsentAcrossARestore (C4): a restore's recovery start rotates
// the epoch, so a token from before it is asked again; an epoch put back with
// an identical impact validates, the stated limit of design 2.5, pinned so it
// cannot widen.
func TestVisibilityConsentAcrossARestore(t *testing.T) {
	f := newVCFixture(t, models.VisibilityPublic)
	out := f.outsider(t, "outsider", 0)
	body := map[string]any{"visibility": models.VisibilityInviteOnly}
	token := f.preview(t, f.eventPath(), body).Consent
	saved, _ := consentEpochIn(f.app.DB)
	if err := (&App{DB: secondPool(t, f.app)}).bootRestoreFence(); err != nil {
		t.Fatal(err)
	}
	before := f.everything(t)
	wantDecision(t, f.admin.do("PUT", f.eventPath(), with(body, "consent", token), true))
	if f.everything(t) != before {
		t.Fatal("a token from before the restore wrote")
	}
	// The restore stands in as SQL: the saved epoch row comes back with an
	// identical loser set and impact.
	gormMust(t, f.app.DB.Exec("UPDATE settings SET value = ? WHERE key = ?", saved, consentEpochKey))
	apiDo(t, f.admin, "PUT", f.eventPath(), with(body, "consent", token), http.StatusOK)
	if regStatus(t, f.app, out.ID, f.dates[0].ID) != models.RegCancelled {
		t.Fatal("the reviewed registrant was not cancelled")
	}
}

// TestVisibilityConsentAuthority (C10): authority is judged where the rows
// are: a demoted leader's preview shows no impact, a valid token does not
// outlive its manager's authority, and one manager's token is not another's.
func TestVisibilityConsentAuthority(t *testing.T) {
	setup := func(t *testing.T) (*vcFixture, *models.User, *apiClient) {
		f := newVCFixture(t, models.VisibilityPublic)
		f.outsider(t, "outsider", 0)
		leader := newTestUser(t, f.app, "leader", "password1234", models.RoleMember)
		var g models.Group
		gormMust(t, f.app.DB.First(&g, f.gid))
		addMember(t, f.app, leader, &g, models.MembershipLeader)
		return f, leader, f.client(t, leader)
	}
	demote := func(t *testing.T, f *vcFixture, u *models.User) {
		apiDo(t, f.admin, "PUT", fmt.Sprintf("/api/v1/groups/%d/members/%d/role", f.gid, u.ID), map[string]any{"role": models.MembershipMember}, http.StatusOK)
	}
	refused := func(t *testing.T, w *httptest.ResponseRecorder) {
		t.Helper()
		if (w.Code != http.StatusForbidden && w.Code != http.StatusNotFound) || strings.Contains(w.Body.String(), `"impact"`) {
			t.Fatalf("want 403 or 404 with no impact: %d %s", w.Code, w.Body.String())
		}
	}
	body := map[string]any{"visibility": models.VisibilityInviteOnly}
	for _, route := range []string{"event", "group"} {
		path := func(f *vcFixture) string {
			if route == "group" {
				return f.groupPath()
			}
			return f.eventPath()
		}
		t.Run(route+"/a demoted leader's preview", func(t *testing.T) {
			f, leader, lc := setup(t)
			apiDo(t, lc, "GET", visibilityPreviewPath(t, path(f), body), nil, http.StatusOK)
			demote(t, f, leader)
			refused(t, lc.do("GET", visibilityPreviewPath(t, path(f), body), nil, false))
		})
		t.Run(route+"/a token outliving its manager's authority", func(t *testing.T) {
			f, leader, lc := setup(t)
			var impact visibilityImpactJSON
			if err := json.Unmarshal([]byte(apiDo(t, lc, "GET", visibilityPreviewPath(t, path(f), body), nil, http.StatusOK)), &impact); err != nil {
				t.Fatal(err)
			}
			demote(t, f, leader)
			before := f.everything(t)
			refused(t, lc.do("PUT", path(f), with(body, "consent", impact.Consent), true))
			if f.everything(t) != before {
				t.Fatal("a demoted manager's confirmation wrote")
			}
		})
		t.Run(route+"/another manager's token", func(t *testing.T) {
			f, _, lc := setup(t)
			theirs := f.preview(t, path(f), body).Consent
			mine := wantDecision(t, lc.do("PUT", path(f), with(body, "consent", theirs), true))
			if mine.Consent == theirs {
				t.Fatal("two managers share a token")
			}
			apiDo(t, lc, "PUT", path(f), with(body, "consent", mine.Consent), http.StatusOK)
		})
	}
}

// TestDateEditConsentRefusalsRunOnce (C12): the guarded date edit reruns
// only a date that ended during its first attempt, after a confirmed
// rollback (singleDateMutation); a consent mismatch, and the same attempt
// asked again, are answered from their first attempt with nothing written. An
// outdated page's body on the retired PUT never reaches the writer.
func TestDateEditConsentRefusalsRunOnce(t *testing.T) {
	f := newVCFixture(t, models.VisibilityPublic)
	f.outsider(t, "outsider", 0)
	changes := map[string]any{"visibility": models.VisibilityInviteOnly}
	base := editBaseOf(t, f.admin, f.ev.ID, f.day(0))
	attempt := newDateAttempt(t)
	for _, tc := range []struct {
		name     string
		send     func() *httptest.ResponseRecorder
		want     func(t *testing.T, w *httptest.ResponseRecorder)
		attempts int64
	}{
		{"a mismatch", func() *httptest.ResponseRecorder {
			return postDateEdit(f.admin, f.ev.ID, f.day(0), dateEditBody(attempt, base.Revision, changes, "consent", strings.Repeat("0", 32)))
		}, func(t *testing.T, w *httptest.ResponseRecorder) { wantDecision(t, w) }, 1},
		{"the same attempt with no consent", func() *httptest.ResponseRecorder {
			return postDateEdit(f.admin, f.ev.ID, f.day(0), dateEditBody(attempt, base.Revision, changes))
		}, func(t *testing.T, w *httptest.ResponseRecorder) { wantDecision(t, w) }, 1},
		{"an outdated body on the retired PUT", func() *httptest.ResponseRecorder {
			return f.admin.do("PUT", f.datePath(0), with(changes, "confirm", true), true)
		}, wantDatePageOutdated, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var attempts atomic.Int64
			f.app.hooks.singleDateAttempts = &attempts
			t.Cleanup(func() { f.app.hooks.singleDateAttempts = nil })
			before := f.everything(t)
			tc.want(t, tc.send())
			if n := attempts.Load(); n != tc.attempts {
				t.Fatalf("the refusal ran %d attempts, want %d", n, tc.attempts)
			}
			if f.everything(t) != before {
				t.Fatal("a refused date edit wrote")
			}
		})
	}
}

// TestConsentTokenDefinition (C1): the token an App mints moves with every
// field the review binds (the scope, the actor, the epoch, the target and the
// current value, the reset and its dates, every row field, every identity
// field, row order) and with the key, and is stable, 32 lowercase hex, for
// equal input. internal/consent's TestMessageIsInjective holds the encoding's
// collision pairs.
func TestConsentTokenDefinition(t *testing.T) {
	app := &App{Config: Config{SessionSecret: []byte("token-definition-secret-0123456789")}}
	key, err := app.consentKey()
	if err != nil {
		t.Fatal(err)
	}
	uid := func(v uint) *uint { return &v }
	type change struct {
		h       consent.Header
		entries []consent.Entry[dateLosers]
	}
	base := func() change {
		sel := func(id uint) dateLosers {
			return dateLosers{rows: []models.Registration{{ID: id, OccurrenceID: 40, UserID: uid(7), RegisteredBy: uid(8), HouseholdPersonID: uid(9)}}}
		}
		return change{
			h: consent.Header{Kind: consent.KindEvent, Actor: 1, Event: 2, Occurrence: 3, Group: 4, Target: models.VisibilityInviteOnly, Current: models.VisibilityPublic,
				Reset: true, FutureOverrides: 1, ResetDates: []consent.Reset{{Occurrence: 40, Date: "2026-12-01", Title: "Supper", Visibility: models.VisibilityPrivate}}, Epoch: "e1"},
			entries: []consent.Entry[dateLosers]{
				{Key: consent.Key{List: consent.ListTightened, Shape: consent.ShapeEvent, Event: 2}, Row: consent.RowDisplay{Title: "Supper", Visibility: models.VisibilityPublic, Registrants: 1}, Sel: sel(50), Cancels: true},
				{Key: consent.Key{List: consent.ListTightened, Shape: consent.ShapeDate, Event: 2, Occurrence: 41}, Row: consent.RowDisplay{Date: "2026-12-08", DecisionKey: "date:41", Title: "Supper · 2026-12-08", Visibility: models.VisibilityPublic, Registrants: 1}, Sel: sel(51), Cancels: true},
			},
		}
	}
	token := func(k []byte, c change) string {
		s, err := consent.TokenFor(k, c.h, c.entries)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	want := token(key, base())
	if !consent.WellFormed(want) || token(key, base()) != want {
		t.Fatalf("the token %q is not stable 32 lowercase hex", want)
	}
	reg := func(c *change) *models.Registration { return &c.entries[0].Sel.rows[0] }
	mutations := map[string]func(c *change){
		"kind":             func(c *change) { c.h.Kind = consent.KindGroup },
		"actor":            func(c *change) { c.h.Actor++ },
		"event":            func(c *change) { c.h.Event++ },
		"occurrence":       func(c *change) { c.h.Occurrence++ },
		"group":            func(c *change) { c.h.Group++ },
		"target":           func(c *change) { c.h.Target = models.VisibilityPrivate },
		"current":          func(c *change) { c.h.Current = models.VisibilityPrivate },
		"reset flag":       func(c *change) { c.h.Reset = false },
		"futureOverrides":  func(c *change) { c.h.FutureOverrides++ },
		"epoch":            func(c *change) { c.h.Epoch = "e2" },
		"reset occurrence": func(c *change) { c.h.ResetDates[0].Occurrence++ },
		"reset date":       func(c *change) { c.h.ResetDates[0].Date = "2026-12-02" },
		"reset title":      func(c *change) { c.h.ResetDates[0].Title = "Dinner" },
		"reset visibility": func(c *change) { c.h.ResetDates[0].Visibility = models.VisibilityInviteOnly },
		"reset count":      func(c *change) { c.h.ResetDates = nil },
		"row list":         func(c *change) { c.entries[0].Key.List = consent.ListLeaderOpened },
		"row shape":        func(c *change) { c.entries[0].Key.Shape = consent.ShapeDate },
		"row event":        func(c *change) { c.entries[0].Key.Event++ },
		"row occurrence":   func(c *change) { c.entries[1].Key.Occurrence++ },
		"row date":         func(c *change) { c.entries[1].Row.Date = "2026-12-09" },
		"row decision key": func(c *change) { c.entries[1].Row.DecisionKey = "date:42" },
		"row title":        func(c *change) { c.entries[0].Row.Title = "Dinner" },
		"row visibility":   func(c *change) { c.entries[0].Row.Visibility = models.VisibilityPrivate },
		"row registrants":  func(c *change) { c.entries[0].Row.Registrants++ },
		"row order":        func(c *change) { c.entries[0], c.entries[1] = c.entries[1], c.entries[0] },
		"a row more": func(c *change) {
			extra := c.entries[1]
			extra.Key.Occurrence = 43
			c.entries = append(c.entries, extra)
		},
		"registration id":     func(c *change) { reg(c).ID++ },
		"identity occurrence": func(c *change) { reg(c).OccurrenceID++ },
		"user":                func(c *change) { reg(c).UserID = uid(70) },
		"user to null":        func(c *change) { reg(c).UserID = nil },
		"registered_by":       func(c *change) { reg(c).RegisteredBy = uid(80) },
		"registered_by null":  func(c *change) { reg(c).RegisteredBy = nil },
		"household person":    func(c *change) { reg(c).HouseholdPersonID = uid(90) },
		"household null":      func(c *change) { reg(c).HouseholdPersonID = nil },
		"an identity more": func(c *change) {
			c.entries[0].Sel.rows = append(c.entries[0].Sel.rows, models.Registration{ID: 52, OccurrenceID: 40})
		},
		"no identity in a row": func(c *change) { c.entries[0].Sel.rows = nil },
	}
	for name, m := range mutations {
		c := base()
		m(&c)
		if token(key, c) == want {
			t.Errorf("the token does not move with the %s", name)
		}
	}
	other, err := (&App{Config: Config{SessionSecret: []byte("another-definition-secret-0123456789")}}).consentKey()
	if err != nil {
		t.Fatal(err)
	}
	if token(other, base()) == want {
		t.Error("the token does not move with the key")
	}
	for _, s := range []string{"", want[:31], want + "0", strings.ToUpper(want), "z" + want[1:]} {
		if consent.Matches(s, want) {
			t.Errorf("Matches accepted %q", s)
		}
	}
}
