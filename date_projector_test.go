package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"fold/internal/eventdates"
	"fold/internal/events"
	"fold/internal/policy"
	"fold/models"

	"gorm.io/gorm"
)

// The date projector goldens (E1 item 4): every surface that turns a date into
// output (calendar rows, event pages, group lists, the open-events list, the
// date page, its one-date .ics and the three calendar feeds) is captured for
// every kind of viewer over one fixture, and must stay byte for byte the same
// when the code that builds them changes. Run with UPDATE_GOLDEN=1 to rewrite
// testdata/date_projector/*.golden after an intended change.

type projectorFixture struct {
	app                                         *App
	admin, leader, member, invited, stranger    *models.User
	host, teen                                  *models.User
	open, hidden                                models.Group
	study, opened, privateEv, potluck, churchPr models.Event
	byName                                      map[string]*apiClient
	base                                        time.Time
}

func projectorEvent(t *testing.T, f *projectorFixture, g *models.Group, title, vis string, day int, dates int, mod func(*models.Event)) (models.Event, []models.EventOccurrence) {
	t.Helper()
	start := f.base.AddDate(0, 0, day).Add(19 * time.Hour)
	ev := models.Event{Title: title, Description: title + " description", Visibility: vis, StartsLocal: start.Format("2006-01-02T15:04"),
		DurationMinutes: 60, TZID: "UTC", CreatedBy: f.admin.ID, ReminderEmailMinutes: 1440, VideoProvider: models.VideoProviderNone, VideoReveal: "registered_only"}
	if g != nil {
		ev.GroupID = &g.ID
	}
	if mod != nil {
		mod(&ev)
	}
	if err := f.app.DB.Create(&ev).Error; err != nil {
		t.Fatal(err)
	}
	var occs []models.EventOccurrence
	for i := 0; i < dates; i++ {
		s := start.AddDate(0, 0, 7*i)
		occ := models.EventOccurrence{EventID: ev.ID, OriginalDate: calDate(s.Format("2006-01-02")), StartsAt: s, EndsAt: s.Add(time.Hour), Status: models.OccurrenceScheduled}
		if err := f.app.DB.Create(&occ).Error; err != nil {
			t.Fatal(err)
		}
		occs = append(occs, occ)
	}
	return ev, occs
}

// recordClocks writes, through the series writer's own path, the clock
// evidence ev's walk gives each of occs.
func recordClocks(t *testing.T, db *gorm.DB, ev models.Event, occs ...models.EventOccurrence) {
	t.Helper()
	spec := events.Spec{StartsLocal: ev.StartsLocal, DurationMinutes: ev.DurationMinutes, TZID: ev.TZID, RRule: ev.RRule}
	if ev.RRuleUntil != nil {
		spec.Until = models.FormatCalendarDate(*ev.RRuleUntil)
	}
	start, _, err := events.ParseWall(ev.StartsLocal, ev.TZID)
	if err != nil {
		t.Fatal(err)
	}
	x, err := events.Expand(spec, events.WindowEnd(start, time.Now().UTC()))
	if err != nil {
		t.Fatal(err)
	}
	dates := make(map[uint]string, len(occs))
	for _, o := range occs {
		dates[o.ID] = models.FormatCalendarDate(o.OriginalDate)
	}
	if err := eventdates.ReplaceClocks(db, x, dates, false); err != nil {
		t.Fatal(err)
	}
}

func newProjectorFixture(t *testing.T) *projectorFixture {
	t.Helper()
	app := testApp(t)
	now := time.Now().UTC()
	f := &projectorFixture{app: app, base: time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC), byName: map[string]*apiClient{}}
	f.admin = newTestUser(t, app, "Ada Admin", "password1234", models.RoleAdmin)
	f.leader = newTestUser(t, app, "Lee Leader", "password1234", models.RoleMember)
	f.member = newTestUser(t, app, "Mia Member", "password1234", models.RoleMember)
	f.invited = newTestUser(t, app, "Ivy Invited", "password1234", models.RoleMember)
	f.stranger = newTestUser(t, app, "Sam Stranger", "password1234", models.RoleMember)
	f.host = newTestUser(t, app, "Hal Host", "password1234", models.RoleMember)
	f.teen = newTestUser(t, app, "Tess Teen", "password1234", models.RoleMember)
	if err := app.DB.Model(f.teen).Update("account_type", models.AccountTeen).Error; err != nil {
		t.Fatal(err)
	}
	f.open = activeGroup(t, app, f.leader, "Open Group", models.VisibilityPublic, "")
	f.hidden = activeGroup(t, app, f.leader, "Hidden Group", models.VisibilityInviteOnly, "")
	if err := app.DB.Model(&f.open).Update("location", "Open Hall").Error; err != nil {
		t.Fatal(err)
	}
	if err := app.DB.Model(&f.hidden).Update("location", "Secret Room").Error; err != nil {
		t.Fatal(err)
	}
	f.open.Location, f.hidden.Location = "Open Hall", "Secret Room"
	addMember(t, app, f.member, &f.open, models.MembershipMember)
	addMember(t, app, f.member, &f.hidden, models.MembershipMember)
	addMember(t, app, f.teen, &f.open, models.MembershipMember)

	capacity := 2
	var occs, opened, priv, potluck, churchPriv []models.EventOccurrence
	f.study, occs = projectorEvent(t, f, &f.open, "Open Study", models.VisibilityPublic, 7, 4, func(e *models.Event) {
		e.Capacity, e.WaitlistEnabled, e.GuestsAllowed = &capacity, true, 2
		e.ChildcareAvailable, e.ChildcareAges = true, "0-5"
		e.VideoProvider, e.VideoURL = models.VideoProviderLink, "https://example.com/study-call"
		e.RRule = "FREQ=WEEKLY;COUNT=4"
	})
	// The series writer's clock evidence for d1 and d4; d3 has none, as a
	// date written before the store, so its usual clock reads unknown.
	recordClocks(t, app.DB, f.study, occs[0], occs[3])
	// d2 cancelled by a person; d3 invite-only override; d4 renamed and moved.
	gormMust(t, app.DB.Model(&occs[1]).Updates(map[string]any{"status": models.OccurrenceCancelled, "cancel_source": models.CancelSourceManual}))
	gormMust(t, app.DB.Model(&occs[2]).Update("visibility_override", models.VisibilityInviteOnly))
	gormMust(t, app.DB.Model(&occs[3]).Updates(map[string]any{"title_override": "Renamed Study", "location_override": "Back Room", "duration_override": 90}))
	f.opened, opened = projectorEvent(t, f, &f.hidden, "Opened Event", models.VisibilityPublic, 8, 2, func(e *models.Event) {
		e.LeaderOpened = true
		e.VideoProvider, e.VideoURL, e.VideoReveal = models.VideoProviderLink, "https://example.com/opened-call", "always"
	})
	f.privateEv, priv = projectorEvent(t, f, &f.open, "Private Event", models.VisibilityPrivate, 9, 2, func(e *models.Event) {
		e.Location = "Private Porch"
		e.VideoProvider, e.VideoURL = models.VideoProviderLink, "https://example.com/private-call"
		e.RemoteAvailable = true
	})
	// The second date of the private event is public: a series-hidden date.
	gormMust(t, app.DB.Model(&priv[1]).Update("visibility_override", models.VisibilityPublic))
	f.potluck, potluck = projectorEvent(t, f, nil, "Church Potluck", models.VisibilityPublic, 10, 1, func(e *models.Event) {
		e.Location = "Fellowship Hall"
		e.RemoteAvailable = true
		e.VideoProvider, e.VideoURL = models.VideoProviderLink, "https://example.com/potluck-call"
	})
	f.churchPr, churchPriv = projectorEvent(t, f, nil, "Staff Retreat", models.VisibilityPrivate, 11, 1, nil)
	for _, ev := range []models.Event{f.potluck, f.churchPr} {
		gormMust(t, app.DB.Create(&models.EventHost{EventID: ev.ID, UserID: f.host.ID, AddedBy: f.admin.ID}))
	}
	_ = opened
	_ = churchPriv

	reg := func(u *models.User, occ models.EventOccurrence, status string, guests int) {
		r := models.Registration{UserID: &u.ID, OccurrenceID: occ.ID, Status: status, GuestCount: guests, Source: models.RegSourceWeb, AttendanceMode: models.AttendanceInPerson}
		gormMust(t, app.DB.Create(&r))
	}
	reg(f.member, occs[0], models.RegGoing, 1)
	reg(f.invited, occs[0], models.RegMaybe, 0)
	reg(f.teen, occs[0], models.RegWaitlisted, 0)
	reg(f.stranger, occs[0], models.RegDeclined, 0)
	reg(f.stranger, priv[1], models.RegGoing, 0)
	reg(f.host, potluck[0], models.RegGoing, 0)
	reg(f.member, potluck[0], models.RegMaybe, 0)
	seedReadInvitation(t, app, f.admin, f.invited, eventJSON{ID: f.study.ID}, occs[2])
	// The date page's other two answers (M8.1 brief 2.1, 2.2): the member's
	// standing row on the cancelled d2 is their tombstone, and a cancelled
	// answer with guests on d4 shows only its status. Neither is a seat, a
	// calendar entry or an attendee, so no other surface changes.
	reg(f.member, occs[1], models.RegGoing, 0)
	reg(f.stranger, occs[3], models.RegCancelled, 1)

	// Freeze the change times (they feed DTSTAMP): the event a day before today at
	// its id in hours, each date at 90 minutes times its id, so either may be later.
	gormMust(t, app.DB.Exec("UPDATE events SET updated_at = ?::timestamptz + id * interval '1 hour'", f.base.Add(-24*time.Hour)))
	gormMust(t, app.DB.Exec("UPDATE event_occurrences SET updated_at = ?::timestamptz + id * interval '90 minutes'", f.base.Add(-24*time.Hour)))

	for _, u := range []*models.User{f.admin, f.leader, f.member, f.invited, f.stranger, f.host, f.teen} {
		f.byName[u.Name] = loginAPI(t, app, u.Name, "password1234")
	}
	f.byName["signed out"] = anonClient(t, app, "")
	return f
}

func gormMust(t *testing.T, db *gorm.DB) {
	t.Helper()
	if db.Error != nil {
		t.Fatal(db.Error)
	}
}

var projectorViewers = []string{"signed out", "Ada Admin", "Lee Leader", "Mia Member", "Ivy Invited", "Sam Stranger", "Hal Host", "Tess Teen"}

var (
	projectorDateRe  = regexp.MustCompile(`\d{4}-\d{2}-\d{2}|\d{8}`)
	projectorStampRe = regexp.MustCompile(`(?m)^(LAST-MODIFIED|CREATED)[^\r\n]*\r?\n`)
	// A manager's edit token carries the per-process generation; it is
	// replaced before the date pattern could rewrite digit runs inside it.
	projectorRevisionRe = regexp.MustCompile(`"revision": ?"[^"]*"`)
)

// normalize replaces the fixture's moving dates with offsets from today, so
// the goldens do not rot.
func (f *projectorFixture) normalize(s string) string {
	s = projectorStampRe.ReplaceAllString(s, "")
	s = projectorRevisionRe.ReplaceAllString(s, `"revision": "R"`)
	return projectorDateRe.ReplaceAllStringFunc(s, func(m string) string {
		layout := "2006-01-02"
		if len(m) == 8 {
			layout = "20060102"
		}
		d, err := time.Parse(layout, m)
		if err != nil {
			return m
		}
		days := int(d.Sub(f.base).Hours() / 24)
		// A date far from today is a constant of the output (the VTIMEZONE
		// onset 19700101), not a moving fixture date; rewriting it would
		// change the golden every midnight.
		if days < -3650 || days > 3650 {
			return m
		}
		return fmt.Sprintf("D%+d", days)
	})
}

func (f *projectorFixture) record(sb *strings.Builder, viewer, path string) {
	c := f.byName[viewer]
	w := c.do("GET", path, nil, false)
	sb.WriteString(fmt.Sprintf("### %s GET %s -> %d\n", viewer, f.normalize(path), w.Code))
	sb.WriteString(f.normalize(prettyBody(w)))
	sb.WriteString("\n")
}

func prettyBody(w *httptest.ResponseRecorder) string {
	body := w.Body.Bytes()
	if strings.Contains(w.Header().Get("Content-Type"), "json") {
		var out bytes.Buffer
		if err := json.Indent(&out, body, "", "  "); err == nil {
			return out.String()
		}
	}
	return strings.ReplaceAll(string(body), "\r\n", "\n")
}

func (f *projectorFixture) dateStrings(t *testing.T, ev models.Event) []string {
	var occs []models.EventOccurrence
	gormMust(t, f.app.DB.Where("event_id = ?", ev.ID).Order("starts_at").Find(&occs))
	var out []string
	for _, o := range occs {
		out = append(out, models.FormatCalendarDate(o.OriginalDate))
	}
	return out
}

func checkProjectorGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "date_projector", name+".golden")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (UPDATE_GOLDEN=1 writes it)", err)
	}
	if string(want) != got {
		_ = os.WriteFile(path+".got", []byte(got), 0o644)
		t.Fatalf("%s differs from its golden; see %s.got", name, path)
	}
}

func TestDateProjectorGoldens(t *testing.T) {
	f := newProjectorFixture(t)
	from, to := f.base.Format("2006-01-02"), f.base.AddDate(0, 0, 60).Format("2006-01-02")
	events := []models.Event{f.study, f.opened, f.privateEv, f.potluck, f.churchPr}

	t.Run("calendar", func(t *testing.T) {
		var sb strings.Builder
		for _, v := range projectorViewers {
			base := "/api/v1/calendar?from=" + from + "&to=" + to
			f.record(&sb, v, base)
			f.record(&sb, v, base+"&remote=1")
			f.record(&sb, v, base+"&church=1")
			if v != "signed out" {
				f.record(&sb, v, base+"&mine=1")
			}
		}
		checkProjectorGolden(t, "calendar", sb.String())
	})
	t.Run("event_pages", func(t *testing.T) {
		var sb strings.Builder
		for _, v := range projectorViewers {
			for _, ev := range events {
				f.record(&sb, v, fmt.Sprintf("/api/v1/events/%d", ev.ID))
			}
			f.record(&sb, v, "/api/v1/events")
			for _, g := range []models.Group{f.open, f.hidden} {
				f.record(&sb, v, fmt.Sprintf("/api/v1/groups/%d/events?from=%s&to=%s", g.ID, from, to))
			}
		}
		checkProjectorGolden(t, "event_pages", sb.String())
	})
	t.Run("date_pages", func(t *testing.T) {
		var sb strings.Builder
		for _, v := range projectorViewers {
			for _, ev := range events {
				for _, d := range f.dateStrings(t, ev) {
					f.record(&sb, v, fmt.Sprintf("/api/v1/events/%d/dates/%s", ev.ID, d))
				}
			}
		}
		checkProjectorGolden(t, "date_pages", sb.String())
	})
	t.Run("date_ics", func(t *testing.T) {
		var sb strings.Builder
		for _, v := range projectorViewers {
			for _, ev := range events {
				for _, d := range f.dateStrings(t, ev) {
					f.record(&sb, v, fmt.Sprintf("/api/v1/events/%d/dates/%s/calendar.ics", ev.ID, d))
				}
			}
		}
		checkProjectorGolden(t, "date_ics", sb.String())
	})
	t.Run("feeds", func(t *testing.T) {
		var sb strings.Builder
		anon := f.byName["signed out"]
		for _, p := range []string{"/cal/public.ics", fmt.Sprintf("/cal/groups/%d.ics", f.open.ID), fmt.Sprintf("/cal/groups/%d.ics", f.hidden.ID)} {
			f.record(&sb, "signed out", p)
		}
		for _, v := range projectorViewers[1:] {
			var u models.User
			gormMust(t, f.app.DB.Where("name = ?", v).First(&u))
			for _, scope := range []string{models.CalendarFeedMine, models.CalendarFeedAll} {
				raw := "projector-" + scope + "-" + fmt.Sprint(u.ID)
				tok := models.CalendarToken{UserID: u.ID, TokenHash: hashToken(raw), Scope: scope}
				if err := f.app.DB.Where("user_id = ?", u.ID).Delete(&models.CalendarToken{}).Error; err != nil {
					t.Fatal(err)
				}
				if err := f.app.DB.Create(&tok).Error; err != nil {
					t.Fatal(err)
				}
				w := anon.do("GET", "/cal/u/"+raw+".ics", nil, false)
				sb.WriteString(fmt.Sprintf("### %s (%s) GET /cal/u/<token>.ics -> %d\n%s\n", v, scope, w.Code, f.normalize(prettyBody(w))))
			}
		}
		checkProjectorGolden(t, "feeds", sb.String())
	})
	_ = http.StatusOK
}

// writerClock is the clock the date page reads for occ: the evidence the
// series writer records for its original day (events.Expand through
// horizon, as Ensure, Extend and a series edit write it), picked as
// usualClockIn picks (the time occ starts at, else the earliest in its UTC
// minute, else the first). nil when the walk proves nothing for the day.
func writerClock(t *testing.T, ev models.Event, occ models.EventOccurrence, horizon time.Time) *seriesClock {
	t.Helper()
	spec := events.Spec{StartsLocal: ev.StartsLocal, DurationMinutes: ev.DurationMinutes, TZID: ev.TZID, RRule: ev.RRule}
	if ev.RRuleUntil != nil {
		spec.Until = models.FormatCalendarDate(*ev.RRuleUntil)
	}
	x, err := events.Expand(spec, horizon)
	if err != nil {
		t.Fatalf("fixture: %s does not expand: %v", ev.RRule, err)
	}
	day := models.FormatCalendarDate(occ.OriginalDate)
	return pickClock(x.Clocks([]string{day})[day], x.TZID(), occ.StartsAt)
}

// pickClock is usualClockIn's choice over one day's evidence, in Go.
func pickClock(cs []events.Clock, tzid string, at time.Time) *seriesClock {
	of := func(c events.Clock) *seriesClock {
		out := &seriesClock{StartsAt: c.StartsAt, TZID: tzid}
		if c.WrittenLocal != "" {
			w := c.WrittenLocal
			out.WrittenLocal = &w
		}
		return out
	}
	for _, c := range cs {
		if c.StartsAt.Equal(at) {
			return of(c)
		}
	}
	minute := at.Truncate(time.Minute)
	var best *events.Clock
	for i := range cs {
		c := &cs[i]
		if !c.StartsAt.Before(minute) && c.StartsAt.Before(minute.Add(time.Minute)) && (best == nil || c.StartsAt.Before(best.StartsAt)) {
			best = c
		}
	}
	if best != nil {
		return of(*best)
	}
	for _, c := range cs {
		if c.First {
			return of(c)
		}
	}
	return nil
}

var projectorHorizon = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)

// How a date differs from its series (M8.1 brief 4.4) is decided from values,
// over one weekly event in America/Boise, whose clocks fall back on Nov 1, 2026
// and spring forward on Mar 14, 2027. Each date starts where the recurrence
// engine put it (events.Expand), so a rule's own hour and the repeated
// fall-back hour are the series' real clock.
func TestDateChangesComparesValues(t *testing.T) {
	three, five := 3, 5
	str := func(s string) *string { return &s }
	num := func(n int) *int { return &n }
	hall, annex := &occurrenceRoomJSON{ID: 1, Name: "Hall"}, &occurrenceRoomJSON{ID: 2, Name: "Annex"}
	base := models.Event{ID: 1, Title: "Weekly", StartsLocal: "2026-10-21T19:00", DurationMinutes: 60, TZID: "America/Boise",
		RRule: "FREQ=WEEKLY", Location: "Main Hall", Capacity: &three, RemoteAvailable: true, Visibility: models.VisibilityPublic}
	loc := eventZone(base.TZID)
	at := func(day, clock string) time.Time {
		t.Helper()
		start, err := time.ParseInLocation(events.WallLayout, day+"T"+clock, loc)
		if err != nil {
			t.Fatal(err)
		}
		return start
	}
	generated := func(t *testing.T, ev models.Event, day string) events.When {
		t.Helper()
		x, err := events.Expand(events.Spec{StartsLocal: ev.StartsLocal, DurationMinutes: ev.DurationMinutes, TZID: ev.TZID, RRule: ev.RRule},
			time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC))
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range x.Whens {
			if w.OriginalDate == day {
				return w
			}
		}
		t.Fatalf("fixture: %s does not generate %s", ev.RRule, day)
		return events.When{}
	}
	fallBack := func(e *models.Event) { e.StartsLocal = "2026-10-25T01:30" }
	// otherPass is the other instant with the same wall clock as the series'
	// date in the repeated hour of Nov 1, 2026.
	var otherPass time.Time
	{
		ev := base
		fallBack(&ev)
		w := generated(t, ev, "2026-11-01")
		for _, other := range []time.Time{w.StartsAt.Add(time.Hour), w.StartsAt.Add(-time.Hour)} {
			if events.FormatWall(other, loc) == w.StartsLocal {
				otherPass = other
			}
		}
		if otherPass.IsZero() {
			t.Fatalf("fixture: %s is not in a repeated hour", w.StartsLocal)
		}
	}
	for _, tt := range []struct {
		name            string
		day             string
		event           func(*models.Event)
		date            func(*models.EventOccurrence)
		group           *models.Group
		usualRoom, room *occurrenceRoomJSON
		// kept is a date the rule no longer generates, kept at 19:00 by a
		// per-date override through a series edit.
		kept bool
		want []string
	}{
		{name: "the first date", day: "2026-10-21", want: []string{}},
		{name: "a kept date the rule dropped, at the series start's clock", day: "2026-11-04", kept: true,
			event: func(e *models.Event) { e.RRule = "FREQ=WEEKLY;BYDAY=TH" }, want: []string{}},
		{name: "a kept date the rule dropped, at another time", day: "2026-11-04", kept: true,
			event: func(e *models.Event) { e.RRule = "FREQ=WEEKLY;BYDAY=TH" }, date: func(o *models.EventOccurrence) {
				s := at("2026-11-04", "20:00")
				o.StartsAtOverride, o.StartsAt = &s, s
			}, want: []string{changeTime}},
		{name: "a rule whose BYHOUR is not the series start's", day: "2026-11-04", event: func(e *models.Event) { e.RRule = "FREQ=WEEKLY;BYHOUR=9" }, want: []string{}},
		{name: "the series' own pass through the repeated hour", day: "2026-11-01", event: fallBack, want: []string{}},
		{name: "a frozen date at the other pass through the repeated hour", day: "2026-11-01", event: fallBack, date: func(o *models.EventOccurrence) {
			s := otherPass
			o.StartsAtOverride, o.StartsAt = &s, s
		}, want: []string{changeTime}},
		{name: "a rule with two times a day, at the first (Ensure)", day: "2026-11-04", event: func(e *models.Event) { e.RRule = "FREQ=WEEKLY;BYHOUR=9,10" },
			want: []string{}},
		{name: "a rule with two times a day, at the last (a series edit)", day: "2026-11-04", event: func(e *models.Event) { e.RRule = "FREQ=WEEKLY;BYHOUR=9,10" },
			date: func(o *models.EventOccurrence) { o.StartsAt = at("2026-11-04", "10:00") }, want: []string{}},
		{name: "a rule with two times a day, moved to neither", day: "2026-11-04", event: func(e *models.Event) { e.RRule = "FREQ=WEEKLY;BYHOUR=9,10" },
			date: func(o *models.EventOccurrence) {
				s := at("2026-11-04", "11:00")
				o.StartsAtOverride, o.StartsAt = &s, s
			}, want: []string{changeTime}},
		{name: "a later date after the fall-back", day: "2026-11-04", want: []string{}},
		{name: "a later date after the spring-forward", day: "2027-03-17", want: []string{}},
		{name: "a frozen clock equal to the series clock", day: "2026-11-04", date: func(o *models.EventOccurrence) {
			s := at("2026-11-04", "19:00")
			o.StartsAtOverride, o.StartsAt = &s, s
		}, want: []string{}},
		{name: "moved to another day at the same time", day: "2026-11-04", date: func(o *models.EventOccurrence) {
			s := at("2026-11-05", "19:00")
			o.StartsAtOverride, o.StartsAt = &s, s
		}, want: []string{changeDay}},
		{name: "moved later the same day", day: "2026-11-04", date: func(o *models.EventOccurrence) {
			s := at("2026-11-04", "20:30")
			o.StartsAtOverride, o.StartsAt = &s, s
		}, want: []string{changeTime}},
		{name: "duration only", day: "2026-11-04", date: func(o *models.EventOccurrence) { o.DurationOverride = num(90) }, want: []string{changeDuration}},
		{name: "an equal duration override", day: "2026-11-04", date: func(o *models.EventOccurrence) { o.DurationOverride = num(60) }, want: []string{}},
		{name: "another place", day: "2026-11-04", date: func(o *models.EventOccurrence) { o.LocationOverride = str("Back Lawn") }, want: []string{changePlace}},
		{name: "an equal place override", day: "2026-11-04", date: func(o *models.EventOccurrence) { o.LocationOverride = str("Main Hall") }, want: []string{}},
		{name: "falls back to the shown group's place", day: "2026-11-04", event: func(e *models.Event) { e.Location = "" },
			group: &models.Group{ID: 7, Location: "Circle Hall"}, want: []string{}},
		{name: "an override equal to the group's place it falls back to", day: "2026-11-04", event: func(e *models.Event) { e.Location = "" },
			date: func(o *models.EventOccurrence) { o.LocationOverride = str("Circle Hall") }, group: &models.Group{ID: 7, Location: "Circle Hall"}, want: []string{}},
		{name: "the series room replaced", day: "2026-11-04", usualRoom: hall, room: annex, want: []string{changeRoom}},
		{name: "the series room kept", day: "2026-11-04", usualRoom: hall, room: hall, want: []string{}},
		{name: "no room on this date", day: "2026-11-04", usualRoom: hall, want: []string{changeRoom}},
		{name: "in-person cap alone", day: "2026-11-04", date: func(o *models.EventOccurrence) { o.CapacityOverride = &five }, want: []string{changeCapacity}},
		{name: "an equal in-person cap", day: "2026-11-04", date: func(o *models.EventOccurrence) { o.CapacityOverride = num(3) }, want: []string{}},
		{name: "a cap on an unlimited series", day: "2026-11-04", event: func(e *models.Event) { e.Capacity = nil },
			date: func(o *models.EventOccurrence) { o.CapacityOverride = &three }, want: []string{changeCapacity}},
		{name: "remote cap alone", day: "2026-11-04", date: func(o *models.EventOccurrence) { o.RemoteCapacityOverride = num(0) }, want: []string{changeRemoteCapacity}},
		{name: "a remote cap while remote is off", day: "2026-11-04", event: func(e *models.Event) { e.RemoteAvailable = false },
			date: func(o *models.EventOccurrence) { o.RemoteCapacityOverride = num(0) }, want: []string{}},
		{name: "more private", day: "2026-11-04", date: func(o *models.EventOccurrence) { o.VisibilityOverride = str(models.VisibilityPrivate) }, want: []string{changeVisibility}},
		{name: "an equal visibility override", day: "2026-11-04", date: func(o *models.EventOccurrence) { o.VisibilityOverride = str(models.VisibilityPublic) }, want: []string{}},
		{name: "everything, in order", day: "2026-11-04", usualRoom: hall, room: annex, date: func(o *models.EventOccurrence) {
			s := at("2026-11-06", "08:00")
			o.StartsAtOverride, o.StartsAt = &s, s
			o.DurationOverride, o.LocationOverride, o.CapacityOverride, o.RemoteCapacityOverride = num(30), str("Park"), &five, num(9)
			o.VisibilityOverride = str(models.VisibilityInviteOnly)
		}, want: []string{changeDay, changeTime, changeDuration, changePlace, changeRoom, changeCapacity, changeRemoteCapacity, changeVisibility}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ev := base
			if tt.event != nil {
				tt.event(&ev)
			}
			var start time.Time
			if tt.kept {
				start = at(tt.day, "19:00").UTC()
			} else {
				start = generated(t, ev, tt.day).StartsAt
			}
			occ := models.EventOccurrence{ID: 9, EventID: ev.ID, OriginalDate: calDate(tt.day), StartsAt: start, Status: models.OccurrenceScheduled}
			if tt.date != nil {
				tt.date(&occ)
			}
			p := &dateProjection{Event: &ev, Occ: &occ, Ordinary: true, SeriesVisible: true, Group: tt.group}
			clock := writerClock(t, ev, occ, time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC))
			if clock == nil {
				t.Fatal("fixture: the writer records no clock for the day")
			}
			s := p.dateSeries(policy.DatePageFacts{Series: true}, datePageLoad{UsualRoom: tt.usualRoom, UsualClock: clock}, tt.room)
			if s.Usual == nil || !reflect.DeepEqual(s.Changes, tt.want) {
				t.Fatalf("changes %v (usual %+v, date %+v), want %v", s.Changes, s.Usual, p.Eff(), tt.want)
			}
		})
	}
}

// A date's clock is compared in the zone it was written in: a past date
// written at 19:00 in America/Boise, whose series has since moved to UTC
// (where that instant is 02:00 the next day), has not moved.
func TestDateChangesComparesInTheWrittenZone(t *testing.T) {
	boise := eventZone("America/Boise")
	at := time.Date(2026, 11, 4, 19, 0, 0, 0, boise).UTC()
	ev := models.Event{ID: 1, Title: "Weekly", StartsLocal: "2026-12-02T19:00", DurationMinutes: 60, TZID: "UTC",
		RRule: "FREQ=WEEKLY", Visibility: models.VisibilityPublic}
	occ := models.EventOccurrence{ID: 9, EventID: 1, OriginalDate: calDate("2026-11-04"), StartsAt: at, EndsAt: at.Add(time.Hour),
		Status: models.OccurrenceScheduled}
	p := &dateProjection{Event: &ev, Occ: &occ, Ordinary: true, SeriesVisible: true}
	if e := p.Eff(); e.StartsLocal != "2026-11-05T02:00" {
		t.Fatalf("fixture: the date shows at %s in the series' new zone", e.StartsLocal)
	}
	s := p.dateSeries(policy.DatePageFacts{Series: true}, datePageLoad{UsualClock: &seriesClock{StartsAt: at, TZID: "America/Boise"}}, nil)
	if s.Usual.StartsLocal != "2026-11-04T19:00" || s.Changes == nil || len(s.Changes) != 0 {
		t.Fatalf("usual %+v, changes %v", s.Usual, s.Changes)
	}
	// Without a clock the shown day still differs from the original day.
	if s := p.dateSeries(policy.DatePageFacts{Series: true}, datePageLoad{}, nil); !reflect.DeepEqual(s.Changes, []string{changeDay}) || s.Usual.StartsLocal != "" {
		t.Fatalf("no clock: usual %+v, changes %v", s.Usual, s.Changes)
	}
}

// midnightGapDay finds a past day whose 00:30 does not exist in its zone (the
// clocks jump from 00:00 to 01:00) and that time.Date resolves onto that same
// day (sameDay) or onto the evening before, so the fixture never depends on
// which way a zone resolves the gap or on future zone rules.
func midnightGapDay(t *testing.T, sameDay bool) (zone, day string) {
	t.Helper()
	for _, c := range []struct {
		zone  string
		year  int
		month time.Month
	}{
		{"America/Havana", 2022, time.March}, {"America/Santiago", 2022, time.September}, {"America/Asuncion", 2022, time.October},
		{"Asia/Beirut", 2022, time.March}, {"Asia/Amman", 2022, time.February}, {"Africa/Cairo", 2023, time.April},
	} {
		loc, err := events.LoadZone(c.zone)
		if err != nil {
			continue
		}
		for d := 1; time.Date(c.year, c.month, d, 12, 0, 0, 0, time.UTC).Month() == c.month; d++ {
			day := fmt.Sprintf("%04d-%02d-%02d", c.year, int(c.month), d)
			got := time.Date(c.year, c.month, d, 0, 30, 0, 0, loc).Format(events.WallLayout)
			if got != day+"T00:30" && (got[:10] == day) == sameDay {
				return c.zone, day
			}
		}
	}
	t.Fatalf("fixture: no midnight daylight-saving gap found (same day %v)", sameDay)
	return "", ""
}

// keptGapDate is the series facts of a date on day that the rule no longer
// generates, kept through a series edit by its start override at start, with
// the clock the writer records for that day.
func keptGapDate(t *testing.T, ev models.Event, day string, start time.Time) *dateSeriesJSON {
	t.Helper()
	at := start.UTC()
	occ := models.EventOccurrence{ID: 9, EventID: ev.ID, OriginalDate: calDate(day), StartsAt: at, StartsAtOverride: &at,
		Status: models.OccurrenceScheduled}
	p := &dateProjection{Event: &ev, Occ: &occ, Ordinary: true, SeriesVisible: true}
	return p.dateSeries(policy.DatePageFacts{Series: true}, datePageLoad{UsualClock: writerClock(t, ev, occ, projectorHorizon)}, nil)
}

// Day compares local dates apart from the instants and time compares only
// when the instants differ, so a start that daylight saving moves across
// midnight is a day change and never a time change.
func TestDateChangesComparesDayApartFromTheInstant(t *testing.T) {
	same := time.Date(2027, 9, 5, 3, 30, 0, 0, time.UTC)
	later := same.Add(18 * time.Hour)
	for _, c := range []struct {
		usual, date string
		dateAt      time.Time
		want        []string
	}{
		{"2027-09-05T00:30", "2027-09-04T23:30", same, []string{changeDay}},
		{"2027-09-05T00:30", "2027-09-05T01:30", same, []string{}},
		{"2027-09-05T00:30", "2027-09-05T19:00", later, []string{changeTime}},
	} {
		got := dateChanges(dateUsualJSON{StartsLocal: c.usual}, dateUsualJSON{StartsLocal: c.date}, same, c.dateAt, false)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("usual %s, date %s: changes %v, want %v", c.usual, c.date, got, c.want)
		}
	}
}

// A kept date on a day whose series clock (00:30) falls in a midnight
// daylight-saving gap keeps its own day as the usual day. Where the gap
// resolves onto that day, the resolved instant is no change and another time
// that day is a time change alone. Where it resolves onto the evening before,
// a date moved to that evening is a day change, though it starts at the same
// instant; one moved to the written 00:30 on another, ordinary day is a day
// change alone (a different instant at the same clock), and one moved to
// 19:00 there is a day change and then a time change.
func TestDateChangesKeptDateInAMidnightGap(t *testing.T) {
	for _, sameDay := range []bool{true, false} {
		zone, day := midnightGapDay(t, sameDay)
		loc := eventZone(zone)
		ev := models.Event{ID: 1, Title: "Late", StartsLocal: day[:4] + "-01-05T00:30", DurationMinutes: 60, TZID: zone,
			RRule: "FREQ=WEEKLY;COUNT=1", Visibility: models.VisibilityPublic}
		wall := func(value string) time.Time {
			t.Helper()
			at, err := time.ParseInLocation(events.WallLayout, value, loc)
			if err != nil || at.Format(events.WallLayout) != value {
				t.Fatalf("fixture: %s does not exist in %s", value, zone)
			}
			return at
		}
		d, _ := time.Parse("2006-01-02", day)
		resolved := time.Date(d.Year(), d.Month(), d.Day(), 0, 30, 0, 0, loc)
		type gapCase struct {
			start time.Time
			want  []string
		}
		cases := []gapCase{{wall(day + "T19:00"), []string{changeTime}}}
		if sameDay {
			cases = append(cases, gapCase{resolved, []string{}})
		} else {
			evening := wall(d.AddDate(0, 0, -1).Format("2006-01-02") + "T23:30")
			if !evening.Equal(resolved) {
				t.Fatalf("fixture: %s %s resolves 00:30 to %s, not the evening before", zone, day, events.FormatWall(resolved, loc))
			}
			other := d.AddDate(0, 0, 2).Format("2006-01-02")
			otherAt := wall(other + "T00:30")
			if otherAt.Equal(resolved) {
				t.Fatalf("fixture: %s 00:30 is the gap day's resolved instant", other)
			}
			cases = append(cases, gapCase{evening, []string{changeDay}},
				gapCase{otherAt, []string{changeDay}},
				gapCase{wall(other + "T19:00"), []string{changeDay, changeTime}})
		}
		for _, c := range cases {
			s := keptGapDate(t, ev, day, c.start)
			if s.Usual == nil || s.Usual.StartsLocal != day+"T00:30" || !reflect.DeepEqual(s.Changes, c.want) {
				t.Fatalf("%s %s, date at %s: usual %+v, changes %v, want %v", zone, day, events.FormatWall(c.start, loc), s.Usual, s.Changes, c.want)
			}
		}
	}
}

// A series whose own start is in a midnight gap keeps its written clock as the
// usual clock of a kept date on an ordinary day, so a date there at that clock
// is no change.
func TestDateChangesSeriesStartInAMidnightGap(t *testing.T) {
	zone, gap := midnightGapDay(t, false)
	loc := eventZone(zone)
	d, _ := time.Parse("2006-01-02", gap)
	kept := d.AddDate(0, 0, 7).Format("2006-01-02")
	start, err := time.ParseInLocation(events.WallLayout, kept+"T00:30", loc)
	if err != nil || start.Format(events.WallLayout) != kept+"T00:30" {
		t.Fatalf("fixture: %s T00:30 does not exist in %s", kept, zone)
	}
	ev := models.Event{ID: 1, Title: "Late", StartsLocal: gap + "T00:30", DurationMinutes: 60, TZID: zone,
		RRule: "FREQ=WEEKLY;COUNT=1", Visibility: models.VisibilityPublic}
	s := keptGapDate(t, ev, kept, start)
	if s.Usual == nil || s.Usual.StartsLocal != kept+"T00:30" || len(s.Changes) != 0 || s.Changes == nil {
		t.Fatalf("%s %s: usual %+v, changes %v", zone, kept, s.Usual, s.Changes)
	}
}

// A dense rule the writer accepted because its dates are long past keeps its
// usual clock on a stored date, so a time moved there still shows.
func TestDateChangesAcceptedDenseHistoricalRule(t *testing.T) {
	ev := models.Event{ID: 1, Title: "Dense", StartsLocal: "2022-01-01T09:00", DurationMinutes: 60, TZID: "UTC",
		RRule: "FREQ=DAILY;BYHOUR=9,10,11;COUNT=2100", Visibility: models.VisibilityPublic}
	moved := time.Date(2023, 11, 30, 15, 0, 0, 0, time.UTC)
	occ := models.EventOccurrence{ID: 9, EventID: ev.ID, OriginalDate: calDate("2023-11-30"), StartsAt: moved, StartsAtOverride: &moved,
		Status: models.OccurrenceScheduled}
	p := &dateProjection{Event: &ev, Occ: &occ, Ordinary: true, SeriesVisible: true}
	s := p.dateSeries(policy.DatePageFacts{Series: true}, datePageLoad{UsualClock: writerClock(t, ev, occ, projectorHorizon)}, nil)
	if s.Usual == nil || s.Usual.StartsLocal != "2023-11-30T09:00" || !reflect.DeepEqual(s.Changes, []string{changeTime}) {
		t.Fatalf("usual %+v, changes %v", s.Usual, s.Changes)
	}
}

// The review's rule: a series begun in Asia/Kathmandu in 1986, the year the
// zone began at 00:15, with twelve times on its day. Moving the 2027 date to
// 21:00 is a time change from the usual 09:00, even as the only change.
func TestDateChangesAcrossAYearStartTransition(t *testing.T) {
	kathmandu, err := time.LoadLocation("Asia/Kathmandu")
	if err != nil {
		t.Fatal(err)
	}
	ev := models.Event{ID: 1, Title: "Dense", StartsLocal: "1986-02-01T09:00", DurationMinutes: 60, TZID: "Asia/Kathmandu",
		RRule: "FREQ=YEARLY;BYHOUR=9,10,11,12,13,14,15,16,17,18,19,20", Visibility: models.VisibilityPublic}
	moved := time.Date(2027, 2, 1, 21, 0, 0, 0, kathmandu)
	occ := models.EventOccurrence{ID: 9, EventID: ev.ID, OriginalDate: calDate("2027-02-01"), StartsAt: moved, StartsAtOverride: &moved,
		Status: models.OccurrenceScheduled}
	p := &dateProjection{Event: &ev, Occ: &occ, Ordinary: true, SeriesVisible: true}
	s := p.dateSeries(policy.DatePageFacts{Series: true}, datePageLoad{UsualClock: writerClock(t, ev, occ, projectorHorizon)}, nil)
	if s.Usual == nil || s.Usual.StartsLocal != "2027-02-01T09:00" || s.Usual.EndsLocal != "2027-02-01T10:00" ||
		!reflect.DeepEqual(s.Changes, []string{changeTime}) {
		t.Fatalf("usual %+v, changes %v", s.Usual, s.Changes)
	}
}

// A series repeating from Go's zero instant: the writer records the clock of
// the walk that wrote each date, so an untouched date has no time change and
// a date moved to another clock has one; a date written before the store has
// no evidence and claims neither.
func TestDateChangesZeroStartSeries(t *testing.T) {
	ev := models.Event{ID: 1, Title: "Zero", StartsLocal: "0001-01-01T00:00", DurationMinutes: 60, TZID: "UTC",
		RRule: "FREQ=YEARLY;BYMONTH=1;BYMONTHDAY=1;BYHOUR=9;BYSETPOS=1", Visibility: models.VisibilityPublic}
	spec := events.Spec{StartsLocal: ev.StartsLocal, DurationMinutes: ev.DurationMinutes, TZID: ev.TZID, RRule: ev.RRule}
	x, err := events.Expand(spec, events.WindowEnd(time.Time{}, time.Now().UTC()))
	if err != nil || len(x.Whens) == 0 || x.Replayable() {
		t.Fatalf("fixture: the writer stores %+v %v", x.Whens, err)
	}
	stored := x.Whens[0]
	evidence := x.Clocks([]string{stored.OriginalDate})[stored.OriginalDate]
	project := func(at time.Time, clock *seriesClock) *dateSeriesJSON {
		occ := models.EventOccurrence{ID: 9, EventID: ev.ID, OriginalDate: calDate(stored.OriginalDate), StartsAt: at,
			EndsAt: at.Add(time.Hour), Status: models.OccurrenceScheduled}
		p := &dateProjection{Event: &ev, Occ: &occ, Ordinary: true, SeriesVisible: true}
		return p.dateSeries(policy.DatePageFacts{Series: true}, datePageLoad{UsualClock: clock}, nil)
	}
	if s := project(stored.StartsAt, pickClock(evidence, x.TZID(), stored.StartsAt)); s.Usual.StartsLocal != stored.StartsLocal || len(s.Changes) != 0 || s.Changes == nil {
		t.Fatalf("untouched: usual %+v, changes %v", s.Usual, s.Changes)
	}
	moved := stored.StartsAt.Add(3 * time.Hour)
	if s := project(moved, pickClock(evidence, x.TZID(), moved)); !reflect.DeepEqual(s.Changes, []string{changeTime}) {
		t.Fatalf("moved: usual %+v, changes %v", s.Usual, s.Changes)
	}
	if s := project(stored.StartsAt, nil); s.Usual.StartsLocal != "" || s.Changes == nil || len(s.Changes) != 0 {
		t.Fatalf("no evidence: usual %+v, changes %v", s.Usual, s.Changes)
	}
}

// A stored date that the next day's time resolves back to is its own usual
// date: in America/St_Johns the rule's 00:30 on 2011-03-13 is 23:30 on the
// 12th, which the writer stores under the 12th with no UNTIL or one on the
// 13th, so that date unchanged reports no change.
func TestDateChangesStoredDateFromTheNextDay(t *testing.T) {
	ev := models.Event{ID: 1, Title: "Late", StartsLocal: "2011-03-06T00:00", DurationMinutes: 60, TZID: "America/St_Johns",
		RRule: "FREQ=WEEKLY;BYMINUTE=0,30;COUNT=4", Visibility: models.VisibilityPublic}
	thirteenth := calDate("2011-03-13")
	for _, until := range []*time.Time{nil, &thirteenth} {
		ev.RRuleUntil = until
		spec := events.Spec{StartsLocal: ev.StartsLocal, DurationMinutes: ev.DurationMinutes, TZID: ev.TZID, RRule: ev.RRule}
		if until != nil {
			spec.Until = models.FormatCalendarDate(*until)
		}
		x, err := events.Expand(spec, time.Date(2012, 1, 1, 0, 0, 0, 0, time.UTC))
		whens := x.Whens
		var stored *events.When
		for i := range whens {
			if whens[i].OriginalDate == "2011-03-12" {
				stored = &whens[i]
			}
		}
		if err != nil || stored == nil || stored.StartsLocal != "2011-03-12T23:30" {
			t.Fatalf("fixture (until %q): the writer stores no 2011-03-12T23:30: %+v %v", spec.Until, whens, err)
		}
		occ := models.EventOccurrence{ID: 9, EventID: ev.ID, OriginalDate: calDate("2011-03-12"), StartsAt: stored.StartsAt,
			EndsAt: stored.EndsAt, Status: models.OccurrenceScheduled}
		p := &dateProjection{Event: &ev, Occ: &occ, Ordinary: true, SeriesVisible: true}
		s := p.dateSeries(policy.DatePageFacts{Series: true}, datePageLoad{UsualClock: writerClock(t, ev, occ, time.Date(2012, 1, 1, 0, 0, 0, 0, time.UTC))}, nil)
		if s.Usual == nil || s.Usual.StartsLocal != "2011-03-12T23:30" || len(s.Changes) != 0 || s.Changes == nil {
			t.Fatalf("until %q: usual %+v, changes %v", spec.Until, s.Usual, s.Changes)
		}
	}
}

// A one-time event has no series to differ from or answer for, and a reader
// who may not see the series gets no series facts at all.
func TestDateSeriesOneTimeAndHidden(t *testing.T) {
	ev := models.Event{ID: 1, Title: "Once", StartsLocal: "2026-10-21T19:00", DurationMinutes: 60, TZID: "UTC", Visibility: models.VisibilityPublic}
	occ := models.EventOccurrence{ID: 9, EventID: 1, OriginalDate: calDate("2026-10-21"), StartsAt: time.Date(2026, 10, 22, 1, 0, 0, 0, time.UTC), Status: models.OccurrenceScheduled}
	p := &dateProjection{Event: &ev, Occ: &occ, Ordinary: true, SeriesVisible: true}
	show := p.pageFacts(&models.User{ID: 1})
	if !show.Series || show.Coverage {
		t.Fatalf("a one-time event's series facts without coverage: %+v", show)
	}
	s := p.dateSeries(show, datePageLoad{Upcoming: 1, Covered: 1, SeriesAuthority: true}, nil)
	if s.Usual != nil || len(s.Changes) != 0 || s.Changes == nil || s.UpcomingDates != nil || s.CoveredDates != nil || s.CanAnswerSeries {
		t.Fatalf("a one-time event has no coverage, series answer or usual values: %+v", s)
	}
	ev.RRule = "FREQ=WEEKLY"
	if s := p.dateSeries(policy.DatePageFacts{Series: true, Coverage: true}, datePageLoad{Upcoming: 2, SeriesAuthority: true}, nil); !s.CanAnswerSeries {
		t.Fatalf("a repeating series with upcoming dates: %+v", s)
	}
	if s := p.dateSeries(policy.DatePageFacts{Series: true, Coverage: true}, datePageLoad{SeriesAuthority: true}, nil); s.CanAnswerSeries {
		t.Fatal("a series answer with no upcoming date")
	}
	if s := p.dateSeries(policy.DatePageFacts{Series: true, Coverage: true}, datePageLoad{Upcoming: 2}, nil); s.CanAnswerSeries {
		t.Fatal("a series answer the series writer would refuse")
	}
	ev.RRule = ""
	var out dateOccurrenceJSON
	hidden := &dateProjection{Event: &ev, Occ: &occ, Ordinary: true}
	hidden.datePage(&out, hidden.pageFacts(nil), datePageLoad{Upcoming: 3}, models.Registration{})
	if out.Series != nil || out.Attendance == nil || out.Seats == nil {
		t.Fatalf("an opened date: %+v", out)
	}
	var invited dateOccurrenceJSON
	inv := &dateProjection{Event: &ev, Occ: &occ, SeriesVisible: true, Manager: true, Attendees: true}
	inv.datePage(&invited, inv.pageFacts(&models.User{ID: 1}), datePageLoad{Upcoming: 3, Seats: 2}, models.Registration{Status: models.RegWaitlisted})
	if !reflect.DeepEqual(invited, dateOccurrenceJSON{}) {
		t.Fatalf("invitation-only: %+v", invited)
	}
}

func TestRemoteRSVPState(t *testing.T) {
	zero, two := 0, 2
	for _, tt := range []struct {
		enabled bool
		cap     *int
		want    string
	}{
		{false, nil, remoteRSVPNone}, {false, &two, remoteRSVPNone}, {true, nil, remoteRSVPOffered},
		{true, &two, remoteRSVPOffered}, {true, &zero, remoteRSVPClosed},
	} {
		if got := remoteRSVPState(tt.enabled, tt.cap); got != tt.want {
			t.Errorf("remoteRSVPState(%v, %v) = %s, want %s", tt.enabled, tt.cap, got, tt.want)
		}
	}
}
