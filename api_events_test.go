package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"fold/internal/audit"
	"fold/internal/eventdates"
	"fold/internal/events"
	"fold/internal/notify"
	"fold/internal/policy"
	"fold/internal/testdb"
	"fold/models"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func TestGroupListReadBudgetsAndSummaryValues(t *testing.T) {
	for _, path := range []string{"/api/v1/groups", "/api/v1/me/groups", "/api/v1/admin/groups", "/api/v1/admin/pending", "/api/v1/admin/deletions"} {
		t.Run(path, func(t *testing.T) {
			app := testApp(t)
			leader := newTestUser(t, app, "leader", "pw", models.RoleAdmin)
			member := newTestUser(t, app, "member", "pw", models.RoleMember)
			disabled := newTestUser(t, app, "disabled", "pw", models.RoleMember)
			if err := app.DB.Model(disabled).Update("status", models.UserDisabled).Error; err != nil {
				t.Fatal(err)
			}
			client := loginAPI(t, app, "leader", "pw")
			start := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Second)
			var baseline int64
			for i := 1; i <= 30; i++ {
				status := models.GroupActive
				if path == "/api/v1/admin/pending" {
					status = models.GroupPending
				}
				g := newTestGroup(t, app, leader, fmt.Sprintf("Group %02d", i), status)
				if err := app.DB.Model(g).Updates(map[string]any{"visibility": models.VisibilityPublic, "deletion_requested": true}).Error; err != nil {
					t.Fatal(err)
				}
				addMember(t, app, member, g, models.MembershipMember)
				addMember(t, app, disabled, g, models.MembershipMember)
				ev := models.Event{GroupID: &g.ID, Title: "Next", Visibility: models.VisibilityPublic, StartsLocal: start.Format("2006-01-02T15:04"), TZID: "UTC", DurationMinutes: 60, CreatedBy: leader.ID}
				if err := app.DB.Create(&ev).Error; err != nil {
					t.Fatal(err)
				}
				o := models.EventOccurrence{EventID: ev.ID, OriginalDate: calDate(start.Format("2006-01-02")), StartsAt: start, EndsAt: start.Add(time.Hour), Status: models.OccurrenceScheduled}
				if err := app.DB.Create(&o).Error; err != nil {
					t.Fatal(err)
				}
				if i != 3 && i != 30 {
					continue
				}
				counter := countDBReads(t, app.DB)
				w := client.do("GET", path, nil, false)
				reads := counter.Reads()
				counter.Stop()
				if w.Code != http.StatusOK {
					t.Fatalf("list: %d %s", w.Code, w.Body.String())
				}
				var body struct {
					Groups []groupSummaryJSON `json:"groups"`
				}
				decodeJSON(t, w, &body)
				if len(body.Groups) != i {
					t.Fatalf("got %d groups, want %d", len(body.Groups), i)
				}
				for _, summary := range body.Groups {
					if summary.MemberCount != 2 || summary.LeaderName != leader.PublicName() || summary.NextEventAt == nil || !summary.NextEventAt.Equal(start) {
						t.Fatalf("incorrect summary: %+v", summary)
					}
				}
				if i == 3 {
					baseline = reads
				}
				if reads != baseline || reads > 12 {
					t.Fatalf("%d groups: %d reads, baseline=%d, cap=12", i, reads, baseline)
				}
			}
		})
	}
}

func TestMeetingInstancesAreChronological(t *testing.T) {
	app := testApp(t)
	newTestUser(t, app, "admin", "password1234", models.RoleAdmin)
	admin := loginAPI(t, app, "admin", "password1234")

	w := admin.do("POST", "/api/v1/groups", map[string]any{"name": "Choir"}, true)
	var created struct {
		Group groupSummaryJSON `json:"group"`
	}
	decodeJSON(t, w, &created)
	gid := itoa(created.Group.ID)
	admin.do("POST", "/api/v1/admin/groups/"+gid+"/approve", map[string]any{}, true)

	now := time.Now().UTC().Truncate(time.Minute)
	later := now.Add(30 * 24 * time.Hour)
	sooner := now.Add(48 * time.Hour)
	for _, start := range []time.Time{later, sooner} {
		w = admin.do("POST", "/api/v1/groups/"+gid+"/events", map[string]any{
			"startsLocal": start.Format("2006-01-02T15:04"), "durationMinutes": 60, "tzid": "UTC",
		}, true)
		if w.Code != http.StatusCreated {
			t.Fatalf("create event: %d %s", w.Code, w.Body.String())
		}
	}

	from := now.Add(-time.Hour).Format("2006-01-02")
	to := now.Add(60 * 24 * time.Hour).Format("2006-01-02")
	w = admin.do("GET", "/api/v1/groups/"+gid+"/events?from="+from+"&to="+to, nil, false)
	var list struct {
		Events []occurrenceJSON `json:"events"`
	}
	decodeJSON(t, w, &list)
	if len(list.Events) != 2 {
		t.Fatalf("occurrences: got %d", len(list.Events))
	}
	if !list.Events[0].StartsAt.Before(list.Events[1].StartsAt) {
		t.Fatalf("not chronological: %s then %s", list.Events[0].StartsAt, list.Events[1].StartsAt)
	}

	w = admin.do("GET", groupDetailPath(created.Group.ID), nil, false)
	var detail struct {
		Group groupSummaryJSON `json:"group"`
	}
	decodeJSON(t, w, &detail)
	if detail.Group.NextEventAt == nil || !detail.Group.NextEventAt.UTC().Equal(sooner) {
		t.Fatalf("nextEventAt: got %v want %s", detail.Group.NextEventAt, sooner)
	}
}

func TestMeetingValidation(t *testing.T) {
	app := testApp(t)
	newTestUser(t, app, "admin", "password1234", models.RoleAdmin)
	admin := loginAPI(t, app, "admin", "password1234")
	w := admin.do("POST", "/api/v1/groups", map[string]any{"name": "Study"}, true)
	var created struct {
		Group groupSummaryJSON `json:"group"`
	}
	decodeJSON(t, w, &created)
	gid := itoa(created.Group.ID)
	admin.do("POST", "/api/v1/admin/groups/"+gid+"/approve", map[string]any{}, true)

	w = admin.do("POST", "/api/v1/groups/"+gid+"/events", map[string]any{
		"startsLocal": "2026-10-20T18:00", "durationMinutes": 60, "tzid": "UTC",
		"recurrenceRule": "NOT_A_RULE",
	}, true)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad rule: got %d %s", w.Code, w.Body.String())
	}

	w = admin.do("GET", "/api/v1/groups/"+gid+"/events?from=2000-01-01&to=2026-01-01", nil, false)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("wide range: got %d %s", w.Code, w.Body.String())
	}
}

func TestRemoteSeatLimitValidation(t *testing.T) {
	app := testApp(t)
	admin, gid := openGroup(t, app, "Hybrid study")
	const zeroMessage = "Uncheck remote attendance instead of a 0 remote seat limit"
	for _, path := range []string{"/api/v1/groups/" + gid + "/events", "/api/v1/events"} {
		w := admin.do("POST", path, map[string]any{
			"title": "Hybrid", "startsLocal": "2026-12-01T19:00", "durationMinutes": 60,
			"tzid": "UTC", "remoteAvailable": true, "remoteCapacity": 0,
		}, true)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("create with zero at %s: %d %s", path, w.Code, w.Body.String())
		}
		var body struct{ Error, Code string }
		decodeJSON(t, w, &body)
		if body.Error != zeroMessage || body.Code != "remote_capacity_zero" {
			t.Fatalf("zero limit error: %+v", body)
		}
	}
	eventID, date := seedRemoteEvent(t, app, admin, gid, nil, intPtr(5), false)
	path := fmt.Sprintf("/api/v1/events/%d", eventID)
	for _, bad := range []any{0, -2, 1.5, "1e", 100001} {
		w := admin.do("PUT", path, seriesBody(t, admin, path, map[string]any{"remoteCapacity": bad}), true)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("update remote limit %v: %d %s", bad, w.Code, w.Body.String())
		}
		if bad == 0 && !strings.Contains(w.Body.String(), zeroMessage) {
			t.Fatalf("zero limit message: %s", w.Body.String())
		}
	}
	var ev models.Event
	app.DB.First(&ev, eventID)
	if ev.RemoteCapacity == nil || *ev.RemoteCapacity != 5 {
		t.Fatalf("invalid update changed remote seats: %+v", ev.RemoteCapacity)
	}
	for _, bad := range []any{-1, 1.5, "1e", 100001} {
		saveDate(t, admin, eventID, date, map[string]any{"remoteCapacityOverride": bad}, http.StatusBadRequest, "")
	}
	saveDate(t, admin, eventID, date, map[string]any{"remoteCapacityOverride": 0}, http.StatusOK, "")
	var occ models.EventOccurrence
	app.DB.Where("event_id = ? AND original_date = ?", eventID, date).First(&occ)
	if occ.RemoteCapacityOverride == nil || *occ.RemoteCapacityOverride != 0 {
		t.Fatalf("zero per-date override was not stored: %+v", occ.RemoteCapacityOverride)
	}
	saveDate(t, admin, eventID, date, map[string]any{"title": "Same remote limit"}, http.StatusOK, "")
	occ = models.EventOccurrence{}
	app.DB.Where("event_id = ? AND original_date = ?", eventID, date).First(&occ)
	if occ.RemoteCapacityOverride == nil || *occ.RemoteCapacityOverride != 0 {
		t.Fatal("omitted remoteCapacityOverride changed the stored limit")
	}
	saveDate(t, admin, eventID, date, map[string]any{"remoteCapacityOverride": nil}, http.StatusOK, "")
	occ = models.EventOccurrence{}
	app.DB.Where("event_id = ? AND original_date = ?", eventID, date).First(&occ)
	if occ.RemoteCapacityOverride != nil {
		t.Fatal("explicit null did not clear remoteCapacityOverride")
	}
	apiDo(t, admin, "PUT", path, seriesBody(t, admin, path, map[string]any{"remoteCapacity": 1}), http.StatusOK)
	apiDo(t, admin, "PUT", path, seriesBody(t, admin, path, map[string]any{"title": "Same series remote limit"}), http.StatusOK)
	ev = models.Event{}
	app.DB.First(&ev, eventID)
	if ev.RemoteCapacity == nil || *ev.RemoteCapacity != 1 {
		t.Fatal("omitted remoteCapacity changed the stored limit")
	}
	apiDo(t, admin, "PUT", path, seriesBody(t, admin, path, map[string]any{"remoteCapacity": nil}), http.StatusOK)
	ev = models.Event{}
	app.DB.First(&ev, eventID)
	if ev.RemoteCapacity != nil {
		t.Fatal("explicit null did not clear remoteCapacity")
	}
	if raw := unfoldFeed(feedBody(t, newAPIClient(t, app), "/cal/public.ics")); !strings.Contains(raw, "Remote attendance available") {
		t.Fatalf("cleared remote capacity did not advertise unlimited remote attendance: %s", raw)
	}
	apiDo(t, admin, "PUT", path, seriesBody(t, admin, path, map[string]any{"remoteAvailable": false}), http.StatusOK)
}

// A weekly 7:00 pm America/Boise event stays at 7:00 pm local on both sides
// of the 2026-11-01 and 2027-03-14 changes, and the API returns both clocks.
func TestBoiseWeeklyKeepsLocalTime(t *testing.T) {
	app := testApp(t)
	admin, gid := openGroup(t, app, "Boise")

	w := admin.do("PUT", "/api/v1/admin/timezone", map[string]any{"timezone": "Not/AZone"}, true)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad zone: %d %s", w.Code, w.Body.String())
	}
	w = admin.do("PUT", "/api/v1/admin/timezone", adminSettingBody(admin, "/api/v1/admin/timezone", "timezone", "America/Boise"), true)
	if w.Code != http.StatusOK {
		t.Fatalf("set zone: %d %s", w.Code, w.Body.String())
	}
	anon := newAPIClient(t, app)
	w = anon.do("GET", "/api/v1/site", nil, false)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"timezone":"America/Boise"`) {
		t.Fatalf("site: %d %s", w.Code, w.Body.String())
	}

	w = admin.do("POST", "/api/v1/groups/"+gid+"/events", map[string]any{
		"title": "Evening", "startsLocal": "2026-10-28T19:00", "durationMinutes": 60,
		"tzid": "America/Boise", "recurrenceRule": "FREQ=WEEKLY",
	}, true)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		Event eventJSON `json:"event"`
	}
	decodeJSON(t, w, &created)
	if created.Event.StartsLocal != "2026-10-28T19:00" || created.Event.StartsAt.UTC().Hour() != 1 {
		t.Fatalf("create clocks: local %s utc %s", created.Event.StartsLocal, created.Event.StartsAt.UTC())
	}

	// Fall back is Nov 1 (MDT → MST). Spring forward is Mar 14 (MST → MDT).
	// 19:00 MDT is 01:00 UTC the next day; 19:00 MST is 02:00 UTC.
	checks := []struct {
		from, to string
		hours    map[string]int
	}{
		{"2026-10-28", "2026-11-04", map[string]int{"2026-10-28": 1, "2026-11-04": 2}},
		{"2027-03-10", "2027-03-17", map[string]int{"2027-03-10": 2, "2027-03-17": 1}},
	}
	for _, c := range checks {
		list := listOccurrences(t, admin, gid, c.from, c.to)
		for date, hour := range c.hours {
			o := occurrenceOn(t, list, date)
			if o.StartsLocal != date+"T19:00" || o.EndsLocal != date+"T20:00" {
				t.Fatalf("%s local: %s–%s", date, o.StartsLocal, o.EndsLocal)
			}
			if o.StartsAt.UTC().Hour() != hour || o.TZID != "America/Boise" {
				t.Fatalf("%s utc hour %d tz %s, want %d", date, o.StartsAt.UTC().Hour(), o.TZID, hour)
			}
		}
	}
}

// A series edit moves future dates and keeps their registrations, cancels
// dates the new rule drops (and tells going/waitlisted, not maybe), and
// leaves per-date overrides alone. A date already underway stays put.
func TestSeriesEditKeepsRegistrationsAndOverrides(t *testing.T) {
	app := testApp(t)
	admin, gid := openGroup(t, app, "Prayer")
	samUser := newTestUser(t, app, "sam", "password1234", models.RoleMember)
	patUser := newTestUser(t, app, "pat", "password1234", models.RoleMember)
	sam := loginAPI(t, app, "sam", "password1234")
	pat := loginAPI(t, app, "pat", "password1234")
	for _, c := range []*apiClient{sam, pat} {
		if w := c.do("POST", "/api/v1/groups/"+gid+"/join", map[string]any{}, true); w.Code != http.StatusOK {
			t.Fatalf("join: %d %s", w.Code, w.Body.String())
		}
	}

	start := atUTC(time.Now().AddDate(0, 0, 14), 19, 0)
	d0 := start.Format("2006-01-02")
	d1 := start.AddDate(0, 0, 7).Format("2006-01-02")
	d2 := start.AddDate(0, 0, 14).Format("2006-01-02")
	d3 := start.AddDate(0, 0, 21).Format("2006-01-02")
	d4 := start.AddDate(0, 0, 28).Format("2006-01-02")
	w := admin.do("POST", "/api/v1/groups/"+gid+"/events", map[string]any{
		"title": "Prayer", "startsLocal": start.Format("2006-01-02T15:04"),
		"durationMinutes": 60, "tzid": "UTC", "recurrenceRule": weeklyRule(start),
	}, true)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		Event eventJSON `json:"event"`
	}
	decodeJSON(t, w, &created)
	eid := itoa(created.Event.ID)
	windowEnd := start.AddDate(0, 0, 35).Format("2006-01-02")

	register := func(c *apiClient, date, status string) {
		t.Helper()
		w := c.do("PUT", "/api/v1/events/"+eid+"/registrations", map[string]any{"date": date, "status": status}, true)
		if w.Code != http.StatusOK {
			t.Fatalf("register %s: %d %s", date, w.Code, w.Body.String())
		}
	}
	register(sam, d1, "going")
	register(pat, d3, "maybe")
	register(sam, d3, "going")

	// One date moves to 9:00 pm. Another only changes its room.
	saveDate(t, admin, created.Event.ID, d2, map[string]any{"startsLocal": d2 + "T21:00"}, http.StatusOK, "")
	saveDate(t, admin, created.Event.ID, d4, map[string]any{"location": "Chapel"}, http.StatusOK, "")
	moved := occurrenceOn(t, listOccurrences(t, admin, gid, d0, windowEnd), d1)
	var reg models.Registration
	if err := app.DB.Where("user_id = ? AND occurrence_id = ?", samUser.ID, moved.ID).First(&reg).Error; err != nil {
		t.Fatal(err)
	}

	// The series moves to 8:00 pm. Future plain dates follow; overrides do not.
	w = admin.do("PUT", "/api/v1/events/"+eid, seriesBody(t, admin, "/api/v1/events/"+eid, map[string]any{
		"startsLocal": start.Format("2006-01-02") + "T20:00",
	}), true)
	if w.Code != http.StatusOK {
		t.Fatalf("series edit: %d %s", w.Code, w.Body.String())
	}
	after := listOccurrences(t, admin, gid, d0, windowEnd)
	if got := occurrenceOn(t, after, d1); got.ID != moved.ID || got.StartsLocal != d1+"T20:00" {
		t.Fatalf("moved date: %+v", got)
	}
	if got := occurrenceOn(t, after, d2); got.StartsLocal != d2+"T21:00" {
		t.Fatalf("time override moved: %s", got.StartsLocal)
	}
	if got := occurrenceOn(t, after, d4); got.StartsLocal != d4+"T19:00" || got.Location != "Chapel" {
		t.Fatalf("location override: %s %q", got.StartsLocal, got.Location)
	}
	var still models.Registration
	if err := app.DB.First(&still, reg.ID).Error; err != nil {
		t.Fatal(err)
	}
	if still.OccurrenceID != moved.ID || still.Status != models.RegGoing {
		t.Fatalf("registration changed: %+v", still)
	}

	// Ending the series on d1 drops d3. d2 and d4 are overrides, so they stay.
	w = admin.do("PUT", "/api/v1/events/"+eid, seriesBody(t, admin, "/api/v1/events/"+eid, map[string]any{"rruleUntil": d1}), true)
	if w.Code != http.StatusOK {
		t.Fatalf("until: %d %s", w.Code, w.Body.String())
	}
	left := listOccurrences(t, admin, gid, d0, windowEnd)
	for _, date := range []string{d0, d1, d2, d4} {
		if occurrenceOn(t, left, date).Date != date {
			t.Fatalf("lost %s", date)
		}
	}
	for _, o := range left {
		if o.Date == d3 {
			t.Fatalf("dropped date still listed: %+v", o)
		}
	}
	var cancelled models.EventOccurrence
	if err := app.DB.Where("event_id = ? AND original_date = ?", created.Event.ID, d3).First(&cancelled).Error; err != nil {
		t.Fatal(err)
	}
	if cancelled.Status != models.OccurrenceCancelled {
		t.Fatalf("status %s", cancelled.Status)
	}
	var kept int64
	app.DB.Model(&models.Registration{}).Where("occurrence_id = ? AND user_id = ?", cancelled.ID, patUser.ID).Count(&kept)
	if kept != 1 {
		t.Fatalf("cancelled registration rows: %d", kept)
	}

	assertNotice := func(c *apiClient, want bool) {
		t.Helper()
		ns := feed(t, c, "")
		found := false
		for _, n := range ns {
			if n.Kind == "event.cancelled" && n.Title == "Cancelled: Prayer" {
				found = true
			}
		}
		if found != want {
			t.Fatalf("notice found=%v want=%v in %+v", found, want, ns)
		}
	}
	// M1.5: cancellation tells going and waitlisted, not maybe. Pat was
	// maybe on the dropped date; Sam was going on it and on a kept date.
	assertNotice(pat, false)
	assertNotice(sam, true)
}

func TestPastOccurrenceStaysOnSeriesEdit(t *testing.T) {
	app := testApp(t)
	admin, gid := openGroup(t, app, "History")
	start := atUTC(time.Now().AddDate(0, 0, -35), 19, 0)
	w := admin.do("POST", "/api/v1/groups/"+gid+"/events", map[string]any{
		"startsLocal": start.Format("2006-01-02T15:04"), "durationMinutes": 60,
		"tzid": "UTC", "recurrenceRule": weeklyRule(start),
	}, true)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		Event eventJSON `json:"event"`
	}
	decodeJSON(t, w, &created)
	w = admin.do("PUT", "/api/v1/events/"+itoa(created.Event.ID), seriesBody(t, admin, "/api/v1/events/"+itoa(created.Event.ID), map[string]any{
		"startsLocal": start.Format("2006-01-02") + "T20:00",
	}), true)
	if w.Code != http.StatusOK {
		t.Fatalf("edit: %d %s", w.Code, w.Body.String())
	}
	list := listOccurrences(t, admin, gid, start.Format("2006-01-02"), time.Now().AddDate(0, 0, 21).Format("2006-01-02"))
	var sawPast, sawFuture bool
	now := time.Now()
	for _, o := range list {
		want := "T19:00"
		if o.StartsAt.After(now) {
			want = "T20:00"
			sawFuture = true
		} else {
			sawPast = true
		}
		if !strings.HasSuffix(o.StartsLocal, want) {
			t.Fatalf("%s starts %s (at %s), want %s", o.Date, o.StartsLocal, o.StartsAt, want)
		}
	}
	if !sawPast || !sawFuture {
		t.Fatalf("past %v future %v (%d rows)", sawPast, sawFuture, len(list))
	}
}

// The daily extender only inserts dates that are missing. A second run, and
// a run that cannot take the advisory lock, leaves cancelled rows cancelled.
func TestExtendInsertsOnlyMissingDates(t *testing.T) {
	app := testApp(t)
	admin, gid := openGroup(t, app, "Horizon")
	start := atUTC(time.Now().AddDate(0, 0, 7), 19, 0)
	w := admin.do("POST", "/api/v1/groups/"+gid+"/events", map[string]any{
		"startsLocal": start.Format("2006-01-02T15:04"), "durationMinutes": 60,
		"tzid": "UTC", "recurrenceRule": weeklyRule(start),
	}, true)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		Event eventJSON `json:"event"`
	}
	decodeJSON(t, w, &created)

	var n int64
	app.DB.Model(&models.EventOccurrence{}).Where("event_id = ?", created.Event.ID).Count(&n)
	if n < 70 || n > 90 {
		t.Fatalf("18-month weekly count %d", n)
	}
	if err := eventdates.Extend(app.DB, time.Now().UTC(), nil); err != nil {
		t.Fatal(err)
	}
	var again int64
	app.DB.Model(&models.EventOccurrence{}).Where("event_id = ?", created.Event.ID).Count(&again)
	if again != n {
		t.Fatalf("extend rewrote the series: %d then %d", n, again)
	}

	var cancel models.EventOccurrence
	if err := app.DB.Where("event_id = ? AND original_date = ?", created.Event.ID, start.Format("2006-01-02")).First(&cancel).Error; err != nil {
		t.Fatal(err)
	}
	app.DB.Model(&cancel).Update("status", models.OccurrenceCancelled)
	if err := eventdates.Extend(app.DB, time.Now().UTC(), nil); err != nil {
		t.Fatal(err)
	}
	var kept models.EventOccurrence
	if err := app.DB.First(&kept, cancel.ID).Error; err != nil {
		t.Fatal(err)
	}
	if kept.Status != models.OccurrenceCancelled {
		t.Fatalf("extend un-cancelled: %s", kept.Status)
	}

	var drop models.EventOccurrence
	if err := app.DB.Where("event_id = ? AND original_date <> ?", created.Event.ID, models.FormatCalendarDate(cancel.OriginalDate)).First(&drop).Error; err != nil {
		t.Fatal(err)
	}
	if err := app.DB.Delete(&drop).Error; err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	sqlDB, err := app.DB.DB()
	if err != nil {
		t.Fatal(err)
	}
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var got bool
	if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", eventdates.LockID).Scan(&got); err != nil || !got {
		t.Fatalf("lock: got %v err %v", got, err)
	}
	if err := eventdates.ExtendLocked(ctx, app.DB, time.Now().UTC(), nil); err != nil {
		t.Fatal(err)
	}
	var missing int64
	app.DB.Model(&models.EventOccurrence{}).Where("event_id = ? AND original_date = ?", created.Event.ID, models.FormatCalendarDate(drop.OriginalDate)).Count(&missing)
	if missing != 0 {
		t.Fatal("extend ran while the advisory lock was held")
	}
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", eventdates.LockID); err != nil {
		t.Fatal(err)
	}
	if err := eventdates.ExtendLocked(ctx, app.DB, time.Now().UTC(), nil); err != nil {
		t.Fatal(err)
	}
	app.DB.Model(&models.EventOccurrence{}).Where("event_id = ? AND original_date = ?", created.Event.ID, models.FormatCalendarDate(drop.OriginalDate)).Count(&missing)
	if missing != 1 {
		t.Fatalf("missing date not restored: %d", missing)
	}
}

// DEC-48: an event copies the group's visibility at creation and keeps its
// own value. Opening the group never changes events; making it more
// private tightens inherited events.
func TestEventKeepsItsOwnVisibility(t *testing.T) {
	app := testApp(t)
	newTestUser(t, app, "admin", "password1234", models.RoleAdmin)
	admin := loginAPI(t, app, "admin", "password1234")
	gid := createGroupAs(t, admin, "Choir", models.VisibilityPrivate)
	rehearsal := scheduleGroupEvent(t, admin, gid, "Rehearsal")
	if rehearsal.Visibility != models.VisibilityPrivate || rehearsal.LeaderOpened {
		t.Fatalf("copied at creation: %+v", rehearsal)
	}
	// Setting the group's own value is not an override.
	setVisibility(t, admin, rehearsal.ID, models.VisibilityPrivate, "", http.StatusOK)
	var overrides int64
	app.DB.Model(&models.AuditLog{}).Where("action = ? AND target_id = ?", audit.VisibilityOverride, rehearsal.ID).Count(&overrides)
	if overrides != 0 {
		t.Fatal("matching the group is not an override")
	}

	// More open group: events don't move.
	apiDo(t, admin, "PUT", "/api/v1/groups/"+itoa(gid), groupBody(t, admin, "/api/v1/groups/"+itoa(gid), map[string]any{"visibility": models.VisibilityPublic}), http.StatusOK)
	page, _ := getEvent(t, admin, rehearsal.ID, http.StatusOK)
	if page.Event.Visibility != models.VisibilityPrivate {
		t.Fatalf("opening the group changed the event: %+v", page.Event)
	}
	guest := newAPIClient(t, app)
	getEvent(t, guest, rehearsal.ID, http.StatusNotFound)

	// A stricter value is audited without a notice.
	concert := scheduleGroupEvent(t, admin, gid, "Concert")
	setVisibility(t, admin, concert.ID, models.VisibilityPrivate, "", http.StatusOK)
	var changed int64
	app.DB.Model(&models.AuditLog{}).Where("action = ? AND target_id = ?", audit.VisibilityChanged, concert.ID).Count(&changed)
	if changed != 1 {
		t.Fatal("stricter change was not audited")
	}
	if hasVisibilityNotice(noticeIDs(t, admin), "Concert", "private") {
		t.Fatal("stricter change notified")
	}

	// More private group: inherited events tighten, no decision needed.
	apiDo(t, admin, "PUT", "/api/v1/groups/"+itoa(gid), groupBody(t, admin, "/api/v1/groups/"+itoa(gid), map[string]any{"visibility": models.VisibilityInviteOnly}), http.StatusOK)
	for _, id := range []uint{rehearsal.ID, concert.ID} {
		page, _ = getEvent(t, admin, id, http.StatusOK)
		if page.Event.Visibility != models.VisibilityInviteOnly || page.Event.LeaderOpened {
			t.Fatalf("not tightened: %+v", page.Event)
		}
	}
}

func TestChurchWideHosts(t *testing.T) {
	app := testApp(t)
	adminUser := newTestUser(t, app, "admin", "password1234", models.RoleAdmin)
	admin := loginAPI(t, app, "admin", "password1234")
	hostUser := newTestUser(t, app, "host", "password1234", models.RoleMember)
	host := loginAPI(t, app, "host", "password1234")
	guest := newAPIClient(t, app)

	apiDo(t, host, "POST", "/api/v1/events", map[string]any{
		"title": "Nope", "startsLocal": "2026-12-01T19:00", "durationMinutes": 60,
	}, http.StatusForbidden)
	apiDo(t, admin, "POST", "/api/v1/events", map[string]any{
		"title": "Nope", "startsLocal": "2026-12-01T19:00", "durationMinutes": 60,
		"visibility": "invite_only",
	}, http.StatusBadRequest)

	w := admin.do("POST", "/api/v1/events", map[string]any{
		"title": "Potluck", "startsLocal": "2026-12-01T19:00", "durationMinutes": 90,
		"tzid": "UTC", "hosts": []string{hostUser.Email},
	}, true)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		Event eventJSON `json:"event"`
	}
	decodeJSON(t, w, &created)
	ev := created.Event
	if !ev.ChurchWide || ev.GroupID != nil || ev.HostedBy != "Fold" || ev.Visibility != models.VisibilityPublic {
		t.Fatalf("church event: %+v", ev)
	}
	if len(ev.Hosts) != 2 {
		t.Fatalf("hosts: %+v", ev.Hosts)
	}
	got := map[uint]bool{}
	for _, h := range ev.Hosts {
		got[h.UserID] = true
		if strings.Contains(h.Email, " ") {
			t.Fatalf("host email: %+v", h)
		}
	}
	if !got[adminUser.ID] || !got[hostUser.ID] {
		t.Fatalf("creator was not kept as a host: %+v", ev.Hosts)
	}

	// The host list is an admin view. A guest sees the event and no names or emails.
	guestPage, guestRaw := getEvent(t, guest, ev.ID, http.StatusOK)
	if guestPage.Event.HostedBy != "Fold" || len(guestPage.Event.Hosts) != 0 || strings.Contains(guestRaw, "admin@example.com") {
		t.Fatalf("guest church event: %s", guestRaw)
	}
	if guestPage.CanManage || guestPage.CanSetVisibility {
		t.Fatalf("guest permissions: %+v", guestPage)
	}

	apiDo(t, admin, "DELETE", fmt.Sprintf("/api/v1/events/%d/hosts/%d", ev.ID, adminUser.ID), nil, http.StatusOK)
	apiDo(t, admin, "DELETE", fmt.Sprintf("/api/v1/events/%d/hosts/%d", ev.ID, hostUser.ID), nil, http.StatusBadRequest)

	page, _ := getEvent(t, host, ev.ID, http.StatusOK)
	if !page.CanManage || !page.CanSetVisibility {
		t.Fatalf("host permissions: %+v", page)
	}
	apiDo(t, host, "PUT", fmt.Sprintf("/api/v1/events/%d", ev.ID), seriesBody(t, host, fmt.Sprintf("/api/v1/events/%d", ev.ID), map[string]any{"title": "Christmas Potluck"}), http.StatusOK)
	apiDo(t, host, "PUT", fmt.Sprintf("/api/v1/events/%d/visibility", ev.ID), visibilityBody(t, host, fmt.Sprintf("/api/v1/events/%d/visibility", ev.ID), map[string]any{
		"visibility": "private", "source": "custom",
	}), http.StatusOK)
	getEvent(t, guest, ev.ID, http.StatusNotFound)
	apiDo(t, host, "PUT", fmt.Sprintf("/api/v1/events/%d/visibility", ev.ID), visibilityBody(t, host, fmt.Sprintf("/api/v1/events/%d/visibility", ev.ID), map[string]any{
		"visibility": "invite_only", "source": "custom",
	}), http.StatusBadRequest)
	apiDo(t, host, "POST", fmt.Sprintf("/api/v1/events/%d/hosts", ev.ID), map[string]any{"email": adminUser.Email}, http.StatusForbidden)

	// A host of this event cannot manage another event or a group they can't see.
	pub := createGroupAs(t, admin, "Public Choir", models.VisibilityPublic)
	other := scheduleGroupEvent(t, admin, pub, "Rehearsal")
	apiDo(t, host, "PUT", fmt.Sprintf("/api/v1/events/%d", other.ID), seriesBody(t, host, fmt.Sprintf("/api/v1/events/%d", other.ID), map[string]any{"title": "Taken"}), http.StatusForbidden)
	hidden := createGroupAs(t, admin, "Hidden Care", models.VisibilityInviteOnly)
	secret := scheduleGroupEvent(t, admin, hidden, "Secret")
	apiDo(t, host, "GET", fmt.Sprintf("/api/v1/events/%d", secret.ID), nil, http.StatusNotFound)
	apiDo(t, host, "PUT", fmt.Sprintf("/api/v1/events/%d", secret.ID), seriesBody(t, host, fmt.Sprintf("/api/v1/events/%d", secret.ID), map[string]any{"title": "Taken"}), http.StatusNotFound)

	// Switch back so a signed-in non-host can see registrants' names.
	apiDo(t, host, "PUT", fmt.Sprintf("/api/v1/events/%d/visibility", ev.ID), visibilityBody(t, host, fmt.Sprintf("/api/v1/events/%d/visibility", ev.ID), map[string]any{
		"visibility": "public", "source": "custom",
	}), http.StatusOK)
	apiDo(t, host, "PUT", fmt.Sprintf("/api/v1/events/%d/registrations", ev.ID), map[string]any{
		"status": "going", "date": "2026-12-01",
	}, http.StatusOK)
	reader := newTestUser(t, app, "reader", "password1234", models.RoleMember)
	_ = reader
	readerC := loginAPI(t, app, "reader", "password1234")
	seen, raw := getEvent(t, readerC, ev.ID, http.StatusOK)
	if !seen.Occurrences[0].AttendeesVisible || !strings.Contains(raw, `"name":"host"`) {
		t.Fatalf("signed-in names: %s", raw)
	}
	if seen.CanManage {
		t.Fatal("a non-host can manage the event")
	}
}

func TestEventDescription(t *testing.T) {
	app := testApp(t)
	newTestUser(t, app, "admin", "password1234", models.RoleAdmin)
	admin := loginAPI(t, app, "admin", "password1234")

	apiDo(t, admin, "POST", "/api/v1/events", map[string]any{
		"title": "Too long", "startsLocal": "2026-12-02T19:00", "durationMinutes": 60,
		"description": strings.Repeat("a", descriptionField.max+1),
	}, http.StatusBadRequest)

	w := admin.do("POST", "/api/v1/events", map[string]any{
		"title": "Potluck", "startsLocal": "2026-12-02T19:00", "durationMinutes": 90,
		"description": "  Bring a salad.  ",
	}, true)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		Event eventJSON `json:"event"`
	}
	decodeJSON(t, w, &created)
	if created.Event.Description != "Bring a salad." {
		t.Fatalf("description: %q", created.Event.Description)
	}
	page, _ := getEvent(t, admin, created.Event.ID, http.StatusOK)
	if page.Event.Description != "Bring a salad." || page.Occurrences[0].Description != "Bring a salad." {
		t.Fatalf("page: %+v occ %+v", page.Event.Description, page.Occurrences[0].Description)
	}

	apiDo(t, admin, "PUT", fmt.Sprintf("/api/v1/events/%d", created.Event.ID), seriesBody(t, admin, fmt.Sprintf("/api/v1/events/%d", created.Event.ID), map[string]any{
		"description": "Dessert too.",
	}), http.StatusOK)
	page, _ = getEvent(t, admin, created.Event.ID, http.StatusOK)
	if page.Event.Description != "Dessert too." || page.Event.Title != "Potluck" {
		t.Fatalf("update: %+v", page.Event)
	}

	gid := createGroupAs(t, admin, "Choir", "public")
	raw := apiDo(t, admin, "POST", fmt.Sprintf("/api/v1/groups/%d/events", gid), map[string]any{
		"title": "Rehearsal", "startsLocal": "2026-12-03T19:00", "durationMinutes": 60,
		"description": "Music stands are in the closet.",
	}, http.StatusCreated)
	if !strings.Contains(raw, "Music stands are in the closet.") {
		t.Fatalf("group event: %s", raw)
	}
}

// Regression (Grier, Oct 14): a second series in a group must copy only the
// group's visibility, never a sibling series' override.
func TestNewSeriesCopiesOnlyGroupVisibility(t *testing.T) {
	app := testApp(t)
	newTestUser(t, app, "admin", "password1234", models.RoleAdmin)
	admin := loginAPI(t, app, "admin", "password1234")
	w := admin.do("POST", "/api/v1/groups", map[string]any{"name": "Choir", "visibility": "private"}, true)
	var created struct {
		Group groupSummaryJSON `json:"group"`
	}
	decodeJSON(t, w, &created)
	gid := itoa(created.Group.ID)
	admin.do("POST", "/api/v1/admin/groups/"+gid+"/approve", map[string]any{}, true)

	start := time.Now().AddDate(0, 0, 3).Format("2006-01-02") + "T19:00"
	mk := func(title string) uint {
		w := admin.do("POST", "/api/v1/groups/"+gid+"/events", map[string]any{
			"title": title, "startsLocal": start, "durationMinutes": 60, "tzid": "America/Boise",
			"recurrenceRule": "FREQ=WEEKLY;BYDAY=MO",
		}, true)
		if w.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", title, w.Code, w.Body.String())
		}
		var r struct {
			Event struct {
				ID uint `json:"id"`
			} `json:"event"`
		}
		decodeJSON(t, w, &r)
		return r.Event.ID
	}
	first := mk("First")
	if w := admin.do("PUT", "/api/v1/events/"+itoa(first)+"/visibility", visibilityBody(t, admin, "/api/v1/events/"+itoa(first)+"/visibility", map[string]any{"visibility": "public", "reason": "open house"}), true); w.Code != http.StatusOK {
		t.Fatalf("open first: %d %s", w.Code, w.Body.String())
	}
	second := mk("Second")

	var ev models.Event
	app.DB.First(&ev, second)
	if ev.Visibility != "private" || ev.LeaderOpened {
		t.Fatalf("second series: visibility=%s leaderOpened=%v", ev.Visibility, ev.LeaderOpened)
	}
	w = admin.do("GET", groupDetailPath(created.Group.ID), nil, false)
	var detail struct {
		UpcomingEvents []occurrenceJSON `json:"upcomingEvents"`
	}
	decodeJSON(t, w, &detail)
	seen := false
	for _, o := range detail.UpcomingEvents {
		if o.EventID == second {
			seen = true
			if o.Visibility != "private" || o.LeaderOpened {
				t.Fatalf("second series date %s: visibility=%s leaderOpened=%v", o.Date, o.Visibility, o.LeaderOpened)
			}
		}
	}
	if !seen {
		t.Fatal("second series not listed")
	}
}

// DEC-51: visibility is chosen at creation. Wider than the group goes
// through the override path (audit row, notice, leader_opened).
func TestCreateEventWithVisibility(t *testing.T) {
	app := testApp(t)
	newTestUser(t, app, "admin", "password1234", models.RoleAdmin)
	newTestUser(t, app, "admin2", "password1234", models.RoleAdmin)
	admin := loginAPI(t, app, "admin", "password1234")
	w := admin.do("POST", "/api/v1/groups", map[string]any{"name": "Choir", "visibility": "private"}, true)
	var created struct {
		Group groupSummaryJSON `json:"group"`
	}
	decodeJSON(t, w, &created)
	gid := itoa(created.Group.ID)
	start := time.Now().AddDate(0, 0, 3).Format("2006-01-02") + "T19:00"
	mk := func(body map[string]any) models.Event {
		body["startsLocal"], body["durationMinutes"], body["tzid"] = start, 60, "America/Boise"
		w := admin.do("POST", "/api/v1/groups/"+gid+"/events", body, true)
		if w.Code != http.StatusCreated {
			t.Fatalf("create: %d %s", w.Code, w.Body.String())
		}
		var r struct {
			Event struct {
				ID uint `json:"id"`
			} `json:"event"`
		}
		decodeJSON(t, w, &r)
		var ev models.Event
		app.DB.First(&ev, r.Event.ID)
		return ev
	}
	plain := mk(map[string]any{"title": "Plain"})
	if plain.Visibility != "private" || plain.LeaderOpened {
		t.Fatalf("default: %s %v", plain.Visibility, plain.LeaderOpened)
	}
	opened := mk(map[string]any{"title": "Open house", "visibility": "public", "visibilityReason": "guests welcome"})
	if opened.Visibility != "public" || !opened.LeaderOpened {
		t.Fatalf("opened: %s %v", opened.Visibility, opened.LeaderOpened)
	}
	var n int64
	app.DB.Model(&models.AuditLog{}).Where("action = ? AND target_id = ?", "event.visibility_override", opened.ID).Count(&n)
	if n != 1 {
		t.Fatalf("override audit rows: %d", n)
	}
	app.DB.Model(&models.Notification{}).Where("kind = ?", "event.visibility_override").Count(&n)
	if n == 0 {
		t.Fatal("no override notice")
	}
	tight := mk(map[string]any{"title": "Members", "visibility": "invite_only"})
	if tight.Visibility != "invite_only" || tight.LeaderOpened {
		t.Fatalf("tightened: %s %v", tight.Visibility, tight.LeaderOpened)
	}
	if w := admin.do("POST", "/api/v1/groups/"+gid+"/events", map[string]any{"startsLocal": start, "durationMinutes": 60, "visibility": "secret"}, true); w.Code != http.StatusBadRequest {
		t.Fatalf("bad visibility: %d", w.Code)
	}
}

// DEC-64: who said "can't go" (count and names) is for the group's leaders
// and admins only. A member and a signed-out guest get no declined data:
// no "declined" count key and no declined attendee.
func TestDeclinedOnlyForLeadersAndAdmins(t *testing.T) {
	app := testApp(t)
	newTestUser(t, app, "admin", "password1234", models.RoleAdmin)
	admin := loginAPI(t, app, "admin", "password1234")
	gid := createGroupAs(t, admin, "Open Choir", models.VisibilityPublic)
	var g models.Group
	if err := app.DB.First(&g, gid).Error; err != nil {
		t.Fatal(err)
	}
	ev := scheduleGroupEvent(t, admin, gid, "Rehearsal")
	_, leader := mkUser(t, app, &g, "lead")
	leadUser := models.User{}
	app.DB.Where("name = ?", "lead").First(&leadUser)
	app.DB.Model(&models.Membership{}).Where("user_id = ? AND group_id = ?", leadUser.ID, gid).Update("role", models.MembershipLeader)
	_, member := mkUser(t, app, &g, "mem")
	_, decliner := mkUser(t, app, &g, "nope")
	_, goer := mkUser(t, app, &g, "yes")
	guest := newAPIClient(t, app)
	path := fmt.Sprintf("/api/v1/events/%d/registrations", ev.ID)
	apiDo(t, decliner, "PUT", path, map[string]any{"status": "declined", "date": "2026-12-01"}, http.StatusOK)
	apiDo(t, goer, "PUT", path, map[string]any{"status": "going", "date": "2026-12-01"}, http.StatusOK)

	for _, tc := range []struct {
		who  string
		c    *apiClient
		sees bool
	}{{"leader", leader, true}, {"admin", admin, true}, {"member", member, false}, {"guest", guest, false}} {
		page, raw := getEvent(t, tc.c, ev.ID, http.StatusOK)
		o := page.Occurrences[0]
		n, hasKey := o.Registrations["declined"]
		named := strings.Contains(raw, `"name":"nope"`)
		if tc.sees && (!hasKey || n != 1 || !named) {
			t.Fatalf("%s should see the declined count and name: %s", tc.who, raw)
		}
		if !tc.sees && (hasKey || named || strings.Contains(raw, `"declined"`)) {
			t.Fatalf("%s got declined data: %s", tc.who, raw)
		}
		if o.Registrations["going"] != 1 {
			t.Fatalf("%s going count: %+v", tc.who, o.Registrations)
		}
	}
}

func TestChildcareEventValidationAndNullCapacity(t *testing.T) {
	app := testApp(t)
	admin, gid := openGroup(t, app, "ChildcareFields")
	id, _ := seedChildcareEvent(t, app, admin, gid, nil, intPtr(5), false)
	path := "/api/v1/events/" + itoa(id)
	for _, fields := range []map[string]any{
		{"childcareCapacity": 0}, {"childcareCapacity": -1}, {"childcareCapacity": 1.5}, {"childcareCapacity": "3"}, {"childcareCapacity": 100001},
		{"childcareNotes": strings.Repeat("x", 501)}, {"childcareAges": strings.Repeat("x", 101)},
	} {
		apiDo(t, admin, "PUT", path, seriesBody(t, admin, path, fields), http.StatusBadRequest)
		fields["startsLocal"], fields["durationMinutes"], fields["tzid"] = videoEventStart().Format("2006-01-02T15:04"), 60, "UTC"
		apiDo(t, admin, "POST", "/api/v1/groups/"+gid+"/events", fields, http.StatusBadRequest)
		fields["title"] = "Church childcare"
		apiDo(t, admin, "POST", "/api/v1/events", fields, http.StatusBadRequest)
	}
	ev := eventRow(t, app, id)
	if ev.ChildcareCapacity == nil || *ev.ChildcareCapacity != 5 {
		t.Fatal("bad update changed capacity")
	}
	// An unrelated edit preserves the limit. Explicit null clears it.
	apiDo(t, admin, "PUT", path, seriesBody(t, admin, path, map[string]any{"title": "With childcare"}), http.StatusOK)
	if ev = eventRow(t, app, id); ev.ChildcareCapacity == nil || *ev.ChildcareCapacity != 5 {
		t.Fatal("omitted limit cleared it")
	}
	apiDo(t, admin, "PUT", path, seriesBody(t, admin, path, map[string]any{"childcareCapacity": nil}), http.StatusOK)
	if ev = eventRow(t, app, id); ev.ChildcareCapacity != nil {
		t.Fatal("null did not clear limit")
	}
	w := admin.do("POST", "/api/v1/events", map[string]any{"title": "Childcare church event", "startsLocal": videoEventStart().Format("2006-01-02T15:04"), "durationMinutes": 60, "tzid": "UTC", "childcareAvailable": true, "childcareCapacity": 3, "childcareAges": "2–5", "childcareNotes": "Nursery"}, true)
	if w.Code != http.StatusCreated {
		t.Fatalf("church create: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		Event eventJSON `json:"event"`
	}
	decodeJSON(t, w, &created)
	if !created.Event.ChildcareAvailable || created.Event.ChildcareCapacity == nil || *created.Event.ChildcareCapacity != 3 || created.Event.ChildcareNotes != "Nursery" {
		t.Fatalf("childcare fields: %+v", created.Event)
	}
}

func TestChildcareVisibilityAndOwnRequest(t *testing.T) {
	app := testApp(t)
	admin, gid := openGroup(t, app, "ChildcarePrivacy")
	id, date := seedChildcareEvent(t, app, admin, gid, nil, intPtr(5), false)
	var group models.Group
	if err := app.DB.First(&group, "id = ?", gid).Error; err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"parent", "member", "leader", "outsider"} {
		u := newTestUser(t, app, name, "password1234", models.RoleMember)
		if name != "outsider" {
			role := models.MembershipMember
			if name == "leader" {
				role = models.MembershipLeader
			}
			addMember(t, app, u, &group, role)
		}
	}
	parent := loginAPI(t, app, "parent", "password1234")
	childcareAs(t, parent, id, date, "going", 2, http.StatusOK)
	for _, name := range []string{"parent", "member", "leader", "outsider", "admin", "signed-out"} {
		who := admin
		if name == "signed-out" {
			who = newAPIClient(t, app)
		} else if name != "admin" {
			who = loginAPI(t, app, name, "password1234")
		}
		page, raw := getEvent(t, who, id, http.StatusOK)
		if len(page.Occurrences) == 0 {
			t.Fatal("no event occurrences")
		}
		inst := page.Occurrences[0]
		manager := name == "leader" || name == "admin"
		if !manager {
			for _, key := range []string{`"childcareUsed"`, `"childcareSpotsLeft"`, `"childcareCount"`, `"childcareAgesNote"`} {
				if strings.Contains(raw, key) {
					t.Fatalf("%s got private field %s: %s", name, key, raw)
				}
			}
		}
		if manager && len(inst.Attendees) == 0 {
			t.Fatal("manager got no attendees")
		}
		if !inst.ChildcareAvailable || inst.ChildcareAges == "" || inst.ChildcareNotes == "" || inst.ChildcareCapacity == nil {
			t.Fatalf("%s missing public childcare: %+v", name, inst)
		}
		if inst.ChildcareFull || !strings.Contains(raw, `"childcareFull":false`) {
			t.Fatalf("%s missing public not-full flag: %s", name, raw)
		}
		if (inst.ChildcareUsed != nil) != manager || (inst.ChildcareSpotsLeft != nil) != manager {
			t.Fatalf("%s totals visibility: %+v", name, inst)
		}
		for _, a := range inst.Attendees {
			if (a.ChildcareCount != nil) != manager || (a.ChildcareAgesNote != nil) != manager {
				t.Fatalf("%s attendee request visibility: %+v", name, a)
			}
		}
		if name == "parent" {
			if inst.MyChildcareCount == nil || *inst.MyChildcareCount != 2 || inst.MyChildcareAgesNote == nil || *inst.MyChildcareAgesNote != "3 and 5" {
				t.Fatalf("own request missing: %+v", inst)
			}
		} else if inst.MyChildcareCount != nil || inst.MyChildcareAgesNote != nil {
			t.Fatalf("%s got another person's own request", name)
		}
	}
	// A church-event host has the same childcare access as group leaders.
	w := admin.do("POST", "/api/v1/events", map[string]any{"title": "Church night", "startsLocal": videoEventStart().Format("2006-01-02T15:04"), "durationMinutes": 60, "tzid": "UTC", "hosts": []string{testEmail("outsider")}, "childcareAvailable": true}, true)
	if w.Code != http.StatusCreated {
		t.Fatalf("church event: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		Event eventJSON `json:"event"`
	}
	decodeJSON(t, w, &created)
	host := eventOccurrence(t, loginAPI(t, app, "outsider", "password1234"), created.Event.ID)
	if host.ChildcareUsed == nil || *host.ChildcareUsed != 0 {
		t.Fatalf("host childcare access: %+v", host)
	}
}

func TestChildcareFullForEveryViewer(t *testing.T) {
	app := testApp(t)
	admin, gid := openGroup(t, app, "ChildcareFull")
	id, date := seedChildcareEvent(t, app, admin, gid, nil, intPtr(2), false)
	parent := newTestUser(t, app, "parent", "password1234", models.RoleMember)
	childcareAs(t, loginAPI(t, app, "parent", "password1234"), id, date, "going", 2, http.StatusOK)
	var group models.Group
	if err := app.DB.First(&group, "id = ?", gid).Error; err != nil {
		t.Fatal(err)
	}
	viewers := map[string]*apiClient{"signed-out": newAPIClient(t, app), "admin": admin}
	for _, role := range []string{models.MembershipMember, models.MembershipLeader} {
		u := newTestUser(t, app, role, "password1234", models.RoleMember)
		addMember(t, app, u, &group, role)
		viewers[role] = loginAPI(t, app, role, "password1234")
	}
	for _, tt := range []struct {
		name       string
		available  bool
		capacity   *int
		status     string
		userStatus string
		full       bool
	}{
		{"at capacity", true, intPtr(2), models.RegGoing, models.UserActive, true},
		{"over capacity", true, intPtr(1), models.RegGoing, models.UserActive, true},
		{"space left", true, intPtr(3), models.RegGoing, models.UserActive, false},
		{"unlimited", true, nil, models.RegGoing, models.UserActive, false},
		{"not offered", false, intPtr(2), models.RegGoing, models.UserActive, false},
		{"maybe does not count", true, intPtr(2), models.RegMaybe, models.UserActive, false},
		{"waitlisted does not count", true, intPtr(2), models.RegWaitlisted, models.UserActive, false},
		{"disabled does not count", true, intPtr(2), models.RegGoing, models.UserDisabled, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := app.DB.Model(&models.Event{}).Where("id = ?", id).Updates(map[string]any{
				"childcare_available": tt.available, "childcare_capacity": tt.capacity,
			}).Error; err != nil {
				t.Fatal(err)
			}
			if err := app.DB.Model(&models.Registration{}).Where("user_id = ?", parent.ID).Update("status", tt.status).Error; err != nil {
				t.Fatal(err)
			}
			if err := app.DB.Model(&models.User{}).Where("id = ?", parent.ID).Update("status", tt.userStatus).Error; err != nil {
				t.Fatal(err)
			}
			for name, viewer := range viewers {
				page, raw := getEvent(t, viewer, id, http.StatusOK)
				if len(page.Occurrences) != 1 || page.Occurrences[0].ChildcareFull != tt.full ||
					!strings.Contains(raw, fmt.Sprintf(`"childcareFull":%t`, tt.full)) {
					t.Fatalf("%s full=%t: %s", name, tt.full, raw)
				}
				if name == "signed-out" || name == models.MembershipMember {
					for _, key := range []string{`"childcareUsed"`, `"childcareSpotsLeft"`, `"childcareCount"`, `"childcareAgesNote"`} {
						if strings.Contains(raw, key) {
							t.Fatalf("%s got private field %s: %s", name, key, raw)
						}
					}
				}
			}
		})
	}
}

func TestChildcareTotalsQueriedOnlyWhenNeeded(t *testing.T) {
	app := testApp(t)
	admin, gid := openGroup(t, app, "ChildcareQueries")
	id, _ := seedChildcareEvent(t, app, admin, gid, nil, nil, false)
	for _, tt := range []struct {
		name      string
		available bool
		capacity  *int
		manager   bool
		queries   int
	}{
		{"off for public", false, intPtr(2), false, 0},
		{"off for manager", false, intPtr(2), true, 0},
		{"unlimited for public", true, nil, false, 0},
		{"unlimited for manager", true, nil, true, 1},
		{"limited for public", true, intPtr(2), false, 1},
		{"limited for manager", true, intPtr(2), true, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := app.DB.Model(&models.Event{}).Where("id = ?", id).Updates(map[string]any{
				"childcare_available": tt.available, "childcare_capacity": tt.capacity,
			}).Error; err != nil {
				t.Fatal(err)
			}
			queries := 0
			const name = "test:count_childcare_totals"
			if err := app.DB.Callback().Row().Before("gorm:row").Register(name, func(tx *gorm.DB) {
				if strings.Contains(strings.Join(tx.Statement.Selects, " "), "SUM(registrations.childcare_count") {
					queries++
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = app.DB.Callback().Row().Remove(name) })
			viewer := newAPIClient(t, app)
			if tt.manager {
				viewer = admin
			}
			getEvent(t, viewer, id, http.StatusOK)
			if queries != tt.queries {
				t.Fatalf("childcare total queries=%d, want %d", queries, tt.queries)
			}
		})
	}
}

func TestOccurrenceCountErrorsKeepDates(t *testing.T) {
	app := testApp(t)
	admin, gid := openGroup(t, app, "CountFallbacks")
	id, date := seedChildcareEvent(t, app, admin, gid, nil, intPtr(2), false)
	apiDo(t, admin, "PUT", "/api/v1/events/"+itoa(id), seriesBody(t, admin, "/api/v1/events/"+itoa(id), map[string]any{"remoteAvailable": true, "remoteCapacity": 3}), http.StatusOK)
	apiDo(t, admin, "PUT", "/api/v1/events/"+itoa(id)+"/registrations", map[string]any{
		"date": date, "status": "going", "attendanceMode": "remote", "childcareCount": 2,
	}, http.StatusOK)
	for _, pool := range []string{"childcare", "seats"} {
		t.Run(pool, func(t *testing.T) {
			failed := 0
			const name = "test:fail_occurrence_totals"
			if err := app.DB.Callback().Row().Before("gorm:row").Register(name, func(tx *gorm.DB) {
				selection := strings.Join(tx.Statement.Selects, " ")
				if (pool == "childcare" && strings.Contains(selection, "SUM(registrations.childcare_count")) ||
					(pool == "seats" && strings.Contains(selection, "SUM(1 + registrations.guest_count)")) {
					failed++
					_ = tx.AddError(errors.New("count unavailable"))
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = app.DB.Callback().Row().Remove(name) })
			for _, viewer := range []*apiClient{admin, newAPIClient(t, app)} {
				page, raw := getEvent(t, viewer, id, http.StatusOK)
				if len(page.Occurrences) != 1 {
					t.Fatalf("count error lost the date: %s", raw)
				}
				inst := page.Occurrences[0]
				if inst.Date != date || inst.MyRegistration != nil && *inst.MyRegistration != models.RegGoing {
					t.Fatalf("count error lost registration details: %+v", inst)
				}
				if pool == "childcare" && (inst.ChildcareFull || inst.ChildcareUsed != nil && *inst.ChildcareUsed != 0) {
					t.Fatalf("childcare did not fall back to zero: %+v", inst)
				}
				if pool == "seats" && (inst.SeatsUsed != 0 || inst.RemoteSeatsUsed != 0 || !inst.ChildcareFull) {
					t.Fatalf("seat counts did not fall back independently: %+v", inst)
				}
			}
			if failed == 0 {
				t.Fatal("count failure was not exercised")
			}
		})
	}
}

func TestHouseholdAttendeesArePrivateToHouseholdAndManagers(t *testing.T) {
	app := testApp(t)
	admin, gid := openGroup(t, app, "HouseholdPrivacy")
	eventID, date := seedChildcareEvent(t, app, admin, gid, nil, intPtr(1), true)
	_, c, spouse, child := householdFamily(t, app, admin, "privacyholder")
	editMyHousehold(t, c, map[string]any{"address": "Private household address"}, http.StatusOK)
	batchAnswer(t, c, eventID, date, map[string]any{"people": []map[string]any{{"personId": child.ID, "status": "going", "childcareRequested": true}, {"personId": spouse.ID, "status": "going"}}}, http.StatusOK)
	newTestUser(t, app, "unrelated", "password1234", models.RoleMember)
	ordinary := loginAPI(t, app, "unrelated", "password1234")
	for _, viewer := range []*apiClient{ordinary, newAPIClient(t, app)} {
		page, raw := getEvent(t, viewer, eventID, http.StatusOK)
		if page.Occurrences[0].Registrations[models.RegGoing] != 2 {
			t.Fatalf("counts %s", raw)
		}
		for _, secret := range []string{`"householdPerson"`, "Kit", "Mary", "Private allergy note", "Private household address", "2020-05-01"} {
			if strings.Contains(raw, secret) {
				t.Fatalf("private data %q: %s", secret, raw)
			}
		}
	}
	// Admin gets names and integer ages, with no date/address; a manager's
	// care notes are separately bounded to this going occurrence.
	page, raw := getEvent(t, admin, eventID, http.StatusOK)
	if len(page.Occurrences[0].Attendees) != 2 {
		t.Fatalf("mixed subjects %s", raw)
	}
	for _, secret := range []string{"Private household address", "2020-05-01", `"userId":0`} {
		if strings.Contains(raw, secret) {
			t.Fatalf("bad projection %s", raw)
		}
	}
	var group models.Group
	app.DB.Where("id = ?", gid).First(&group)
	leader := newTestUser(t, app, "eventleader", "password1234", models.RoleMember)
	addMember(t, app, leader, &group, models.MembershipLeader)
	lc := loginAPI(t, app, "eventleader", "password1234")
	_, raw = getEvent(t, lc, eventID, http.StatusOK)
	if strings.Contains(raw, "Private allergy note") {
		t.Fatal("care before window")
	}
	now := time.Now().UTC()
	app.DB.Model(&models.EventOccurrence{}).Where("event_id = ?", eventID).Updates(map[string]any{"starts_at": now.Add(time.Hour), "ends_at": now.Add(2 * time.Hour)})
	_, raw = getEvent(t, lc, eventID, http.StatusOK)
	if !strings.Contains(raw, "Private allergy note") || !strings.Contains(raw, "careNotesVisibleUntil") {
		t.Fatalf("missing windowed care %s", raw)
	}
}

func TestOpenEventsSiteNameReadBudget(t *testing.T) {
	app := testApp(t)
	creator := newTestUser(t, app, "creator", "pw", models.RoleAdmin)
	if err := app.DB.Create(&models.Setting{Key: "site_name", Value: "Our Church"}).Error; err != nil {
		t.Fatal(err)
	}
	client := newAPIClient(t, app)
	client.do("GET", "/api/v1/events", nil, false) // Warm the setup-complete cache.
	settings := countSiteNameReads(t, app.DB)
	start := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Minute)
	var baseline int64
	for i := 1; i <= 30; i++ {
		ev := models.Event{Title: "Church-wide", Visibility: models.VisibilityPublic, StartsLocal: start.Format("2006-01-02T15:04"), TZID: "UTC", DurationMinutes: 60, CreatedBy: creator.ID}
		if err := app.DB.Create(&ev).Error; err != nil {
			t.Fatal(err)
		}
		o := models.EventOccurrence{EventID: ev.ID, OriginalDate: calDate(start.Format("2006-01-02")), StartsAt: start, EndsAt: start.Add(time.Hour), Status: models.OccurrenceScheduled}
		if err := app.DB.Create(&o).Error; err != nil {
			t.Fatal(err)
		}
		if i != 3 && i != 30 {
			continue
		}
		*settings = 0
		counter := countDBReads(t, app.DB)
		w := client.do("GET", "/api/v1/events?from="+start.Format("2006-01-02")+"&to="+start.Format("2006-01-02"), nil, false)
		reads := counter.Reads()
		counter.Stop()
		if w.Code != http.StatusOK {
			t.Fatalf("events: %d %s", w.Code, w.Body.String())
		}
		var body struct {
			Events []occurrenceJSON `json:"events"`
		}
		decodeJSON(t, w, &body)
		if len(body.Events) != i {
			t.Fatalf("got %d events, want %d", len(body.Events), i)
		}
		for _, row := range body.Events {
			if row.HostedBy != "Our Church" || row.GroupID != nil || row.GroupName != "" {
				t.Fatalf("church-wide host: %+v", row)
			}
		}
		if i == 3 {
			baseline = reads
		}
		if reads != baseline || *settings != 1 {
			t.Fatalf("%d dates: %d reads (baseline=%d), site_name=%d", i, reads, baseline, *settings)
		}
		t.Logf("%d dates: %d reads, site_name=%d", i, reads, *settings)
	}
}

func TestGroupEventsContinuationBeyond100Dates(t *testing.T) {
	app := testApp(t)
	leader := newTestUser(t, app, "leader", "pw", models.RoleMember)
	g := newTestGroup(t, app, leader, "Daily group", models.GroupActive)
	client := newAPIClient(t, app)
	start := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Minute)
	ev, first := seedEvent(t, app, g, leader.ID, start)
	visible := []uint{}
	for i := 0; i < 160; i++ {
		o := first
		if i > 0 {
			date := start.AddDate(0, 0, i)
			o = models.EventOccurrence{EventID: ev.ID, OriginalDate: calDate(date.Format("2006-01-02")), StartsAt: date, EndsAt: date.Add(time.Hour), Status: models.OccurrenceScheduled}
			if err := app.DB.Create(&o).Error; err != nil {
				t.Fatal(err)
			}
		}
		if i%7 == 0 || i == 159 {
			if err := app.DB.Model(&o).Update("visibility_override", models.VisibilityInviteOnly).Error; err != nil {
				t.Fatal(err)
			}
		} else {
			visible = append(visible, o.ID)
		}
	}
	path := "/api/v1/groups/" + itoa(g.ID) + "/events"
	window := "?from=" + start.Format("2006-01-02") + "&to=" + start.AddDate(0, 0, 159).Format("2006-01-02")
	read := func(query string) groupEventsJSON {
		t.Helper()
		w := client.do("GET", path+window+query, nil, false)
		if w.Code != http.StatusOK {
			t.Fatalf("events: %d %s", w.Code, w.Body.String())
		}
		var page groupEventsJSON
		decodeJSON(t, w, &page)
		return page
	}
	firstPage := read("")
	if len(firstPage.Events) != 25 || !firstPage.HasMore || !firstPage.Truncated || firstPage.NextCursor == "" {
		t.Fatalf("first page: %d dates, hasMore=%v truncated=%v cursor=%q", len(firstPage.Events), firstPage.HasMore, firstPage.Truncated, firstPage.NextCursor)
	}
	second := read("&limit=100&cursor=" + firstPage.NextCursor)
	if len(second.Events) != 100 || !second.HasMore || !second.Truncated || second.NextCursor == "" {
		t.Fatalf("second page: %d dates, hasMore=%v truncated=%v cursor=%q", len(second.Events), second.HasMore, second.Truncated, second.NextCursor)
	}
	last := read("&cursor=" + second.NextCursor)
	if len(last.Events) != len(visible)-125 || last.HasMore || last.Truncated || last.NextCursor != "" {
		t.Fatalf("last page: %d dates, hasMore=%v truncated=%v cursor=%q", len(last.Events), last.HasMore, last.Truncated, last.NextCursor)
	}
	all := append(append(firstPage.Events, second.Events...), last.Events...)
	for i, row := range all {
		if row.ID != visible[i] {
			t.Fatalf("date %d: got %d want %d", i, row.ID, visible[i])
		}
	}
	// The non-paged shared listing must not silently drop the same series tail.
	full := app.occurrencesForEvents([]uint{ev.ID}, start.Format("2006-01-02"), start.AddDate(0, 0, 159).Format("2006-01-02"), nil, nil)
	if len(full) != len(visible) {
		t.Fatalf("shared list: got %d dates, want %d", len(full), len(visible))
	}
	// Current visibility, not the old cursor, controls the remaining dates.
	if err := app.DB.Model(&models.EventOccurrence{}).Where("id IN ?", visible[125:]).Update("visibility_override", models.VisibilityInviteOnly).Error; err != nil {
		t.Fatal(err)
	}
	hidden := read("&cursor=" + second.NextCursor)
	if len(hidden.Events) != 0 || hidden.HasMore || hidden.Truncated || hidden.NextCursor != "" {
		t.Fatal("hidden tail leaked into continuation metadata")
	}
	one := read("&limit=1")
	if len(one.Events) != 1 || one.Events[0].ID != visible[0] || !one.HasMore {
		t.Fatal("limit=1 did not apply after visibility")
	}
	other := newTestGroup(t, app, leader, "Other group", models.GroupActive)
	w := client.do("GET", "/api/v1/groups/"+itoa(other.ID)+"/events"+window+"&cursor="+firstPage.NextCursor, nil, false)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("cross-group cursor: %d %s", w.Code, w.Body.String())
	}
	for _, query := range []string{"&limit=0", "&limit=-1", "&limit=101", "&limit=abc", "&cursor=invalid", "&cursor=e30"} {
		w := client.do("GET", path+window+query, nil, false)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"code":"group_upcoming_page"`) {
			t.Fatalf("%s: %d %s", query, w.Code, w.Body.String())
		}
	}
}

// A repeat rule is refused when it repeats more than once a day, or makes
// more dates than the cap, and the answer carries its own code.
func TestRepeatRuleLimitsAreRefused(t *testing.T) {
	app := testApp(t)
	admin, gid := openGroup(t, app, "Rules")
	start := atUTC(time.Now().AddDate(0, 0, 7), 19, 0)
	create := func(rule string) *httptest.ResponseRecorder {
		return admin.do("POST", "/api/v1/groups/"+gid+"/events", map[string]any{
			"title": "Repeats", "startsLocal": start.Format("2006-01-02T15:04"), "durationMinutes": 60,
			"tzid": "UTC", "recurrenceRule": rule,
		}, true)
	}
	for _, rule := range []string{"FREQ=SECONDLY", "FREQ=MINUTELY", "FREQ=HOURLY;INTERVAL=2"} {
		w := create(rule)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"code":"recurrence_too_frequent"`) {
			t.Fatalf("%s: %d %s", rule, w.Code, w.Body.String())
		}
	}
	// Daily with every hour of the day is still only "daily", so the cap
	// catches it.
	hours := make([]string, 24)
	for i := range hours {
		hours[i] = fmt.Sprint(i)
	}
	w := create("FREQ=DAILY;BYHOUR=" + strings.Join(hours, ","))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"code":"recurrence_too_many"`) {
		t.Fatalf("byhour: %d %s", w.Code, w.Body.String())
	}
	var n int64
	app.DB.Model(&models.Event{}).Count(&n)
	if n != 0 {
		t.Fatalf("a refused rule left %d events", n)
	}
	for _, rule := range []string{weeklyRule(start), "FREQ=DAILY", "FREQ=MONTHLY;BYMONTHDAY=1"} {
		if w := create(rule); w.Code != http.StatusCreated {
			t.Fatalf("%s: %d %s", rule, w.Code, w.Body.String())
		}
	}
	var created struct {
		Event eventJSON `json:"event"`
	}
	decodeJSON(t, create(weeklyRule(start)), &created)
	apiDo(t, admin, "PUT", fmt.Sprintf("/api/v1/events/%d", created.Event.ID), seriesBody(t, admin, fmt.Sprintf("/api/v1/events/%d", created.Event.ID), map[string]any{"recurrenceRule": "FREQ=SECONDLY"}), http.StatusBadRequest)
}

// Creating, deleting and changing the hosts of an event leave audit rows.
func TestEventLifecycleIsAudited(t *testing.T) {
	app := testApp(t)
	admin, gid := openGroup(t, app, "Audited")
	newTestUser(t, app, "host2", "password1234", models.RoleMember)
	start := atUTC(time.Now().AddDate(0, 0, 7), 19, 0)
	count := func(action string, id uint) int64 {
		var n int64
		app.DB.Model(&models.AuditLog{}).Where("action = ? AND target_id = ?", action, id).Count(&n)
		return n
	}
	var grp struct {
		Event eventJSON `json:"event"`
	}
	decodeJSON(t, admin.do("POST", "/api/v1/groups/"+gid+"/events", map[string]any{
		"title": "Group night", "startsLocal": start.Format("2006-01-02T15:04"), "durationMinutes": 60, "tzid": "UTC",
	}, true), &grp)
	if count(audit.EventCreated, grp.Event.ID) != 1 {
		t.Fatal("group event creation not audited")
	}
	apiDo(t, admin, "DELETE", fmt.Sprintf("/api/v1/events/%d", grp.Event.ID), nil, http.StatusOK)
	if count(audit.EventDeleted, grp.Event.ID) != 1 {
		t.Fatal("event deletion not audited")
	}

	var church struct {
		Event eventJSON `json:"event"`
	}
	decodeJSON(t, admin.do("POST", "/api/v1/events", map[string]any{
		"title": "Church picnic", "startsLocal": start.Format("2006-01-02T15:04"), "durationMinutes": 60, "tzid": "UTC",
	}, true), &church)
	if church.Event.ID == 0 || count(audit.EventCreated, church.Event.ID) != 1 {
		t.Fatalf("church event creation not audited: %+v", church.Event)
	}
	hosts := fmt.Sprintf("/api/v1/events/%d/hosts", church.Event.ID)
	apiDo(t, admin, "POST", hosts, map[string]any{"email": "host2@example.com"}, http.StatusOK)
	apiDo(t, admin, "POST", hosts, map[string]any{"email": "host2@example.com"}, http.StatusOK)
	if got := count(audit.EventHostAdded, church.Event.ID); got != 1 {
		t.Fatalf("host added rows %d, want 1 (a repeat adds nothing)", got)
	}
	var host models.User
	app.DB.Where("email = ?", "host2@example.com").First(&host)
	apiDo(t, admin, "DELETE", fmt.Sprintf("%s/%d", hosts, host.ID), nil, http.StatusOK)
	if count(audit.EventHostRemoved, church.Event.ID) != 1 {
		t.Fatal("host removal not audited")
	}
}

// A bad event must not stop the extender from reaching later events.
func TestExtendContinuesPastABadEvent(t *testing.T) {
	app := testApp(t)
	admin, gid := openGroup(t, app, "Extender")
	start := atUTC(time.Now().AddDate(0, 0, 7), 19, 0)
	var first models.User
	app.DB.Order("id").First(&first)
	// Created first, so it sorts before the good event.
	bad := models.Event{Title: "Bad", StartsLocal: start.Format("2006-01-02T15:04"), DurationMinutes: 60,
		TZID: "UTC", RRule: "FREQ=SECONDLY", Visibility: models.VisibilityPublic, CreatedBy: first.ID}
	if err := app.DB.Create(&bad).Error; err != nil {
		t.Fatal(err)
	}
	var good struct {
		Event eventJSON `json:"event"`
	}
	decodeJSON(t, admin.do("POST", "/api/v1/groups/"+gid+"/events", map[string]any{
		"title": "Good", "startsLocal": start.Format("2006-01-02T15:04"), "durationMinutes": 60,
		"tzid": "UTC", "recurrenceRule": weeklyRule(start),
	}, true), &good)
	app.DB.Where("event_id = ?", good.Event.ID).Delete(&models.EventOccurrence{})
	err := eventdates.Extend(app.DB, time.Now().UTC(), nil)
	if err == nil || !errors.Is(err, events.ErrTooFrequent) {
		t.Fatalf("want the bad event's error, got %v", err)
	}
	var n int64
	app.DB.Model(&models.EventOccurrence{}).Where("event_id = ?", good.Event.ID).Count(&n)
	if n < 70 {
		t.Fatalf("later event was not extended: %d dates", n)
	}
}

// A date opened from a hidden series lists like the exact-date page: no
// description or childcare details from the series, and a registered-only
// video link stays hidden from someone who is not going.
func TestHiddenSeriesListHidesDescriptionChildcareAndVideo(t *testing.T) {
	app, admin, _, ev, dates := dateVisibilityFixture(t, models.VisibilityInviteOnly)
	apiDo(t, admin, "PUT", fmt.Sprintf("/api/v1/events/%d", ev.ID), seriesBody(t, admin, fmt.Sprintf("/api/v1/events/%d", ev.ID), map[string]any{
		"description":        "SECRET-DESCRIPTION",
		"childcareAvailable": true, "childcareNotes": "SECRET-CHILDCARE-NOTE", "childcareAges": "SECRET-AGES",
		"childcareCapacity": 7,
		"videoProvider":     "link", "videoUrl": "https://example.com/SECRET-VIDEO", "videoReveal": "registered_only",
	}), http.StatusOK)
	saveDate(t, admin, ev.ID, models.FormatCalendarDate(dates[0].OriginalDate), map[string]any{"visibility": "public"}, http.StatusOK, "", "reason", "Open")

	listed, err := app.occurrencesForEventsErr([]uint{ev.ID}, models.FormatCalendarDate(dates[0].OriginalDate), models.FormatCalendarDate(dates[3].OriginalDate), nil, nil)
	if err != nil || len(listed) != 1 {
		t.Fatalf("list: %v %+v", err, listed)
	}
	raw, err := json.Marshal(listed)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"SECRET-DESCRIPTION", "SECRET-CHILDCARE-NOTE", "SECRET-AGES", "SECRET-VIDEO"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("hidden series leaked %s: %s", secret, raw)
		}
	}
	if listed[0].ChildcareCapacity != nil {
		t.Fatalf("childcare capacity leaked: %v", *listed[0].ChildcareCapacity)
	}
}

// The exact-date page shows no cancelled registration as the viewer's answer.
// The date page tells three cases apart (M8.1 brief 2.1, 2.2): a scheduled
// date with the viewer's own cancelled answer, an unanswered scheduled date,
// and a cancelled date's tombstone. The cancelled answer is history: its
// status alone, with the capabilities of an unanswered date, and its .ics
// carries no STATUS, as an unanswered one doesn't.
func TestDatePageTellsACancelledAnswerFromNoAnswerAndACancelledDate(t *testing.T) {
	app, admin, _, ev, dates := dateVisibilityFixture(t, models.VisibilityPublic)
	gormMust(t, app.DB.Model(&models.Event{}).Where("id = ?", ev.ID).Updates(map[string]any{"guests_allowed": 3, "remote_available": true}))
	cancelled := newTestUser(t, app, "cancelled-member", "password1234", models.RoleMember)
	newTestUser(t, app, "unanswered-member", "password1234", models.RoleMember)
	gormMust(t, app.DB.Create(&models.Registration{
		UserID: &cancelled.ID, OccurrenceID: dates[0].ID, Status: models.RegCancelled, AttendanceMode: models.AttendanceRemote, GuestCount: 2,
	}))
	gormMust(t, app.DB.Create(&models.Registration{
		UserID: &cancelled.ID, OccurrenceID: dates[1].ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson,
	}))
	withAnswer := loginAPI(t, app, "cancelled-member", "password1234")
	without := loginAPI(t, app, "unanswered-member", "password1234")
	path := func(occ models.EventOccurrence) string {
		return fmt.Sprintf("/api/v1/events/%d/dates/%s", ev.ID, models.FormatCalendarDate(occ.OriginalDate))
	}
	read := func(c *apiClient, occ models.EventOccurrence) map[string]any {
		t.Helper()
		var body struct {
			Occurrence map[string]any `json:"occurrence"`
		}
		if err := json.Unmarshal([]byte(apiDo(t, c, "GET", path(occ), nil, http.StatusOK)), &body); err != nil {
			t.Fatal(err)
		}
		return body.Occurrence
	}
	history, none := read(withAnswer, dates[0]), read(without, dates[0])
	if history["myRegistration"] != models.RegCancelled || none["myRegistration"] != nil {
		t.Fatalf("answers: %v and %v", history["myRegistration"], none["myRegistration"])
	}
	for _, key := range []string{"myAttendanceMode", "myGuestCount"} {
		if _, ok := history[key]; ok {
			t.Fatalf("a cancelled answer carries %s: %v", key, history)
		}
	}
	delete(history, "myRegistration")
	delete(none, "myRegistration")
	// Series coverage counts each reader's own answers on the other dates
	// (the cancelled member is Going on the second); everything about this
	// date, the series action included, must match.
	for _, answer := range []map[string]any{history, none} {
		series, ok := answer["series"].(map[string]any)
		if !ok {
			t.Fatalf("fixture: no series facts: %v", answer)
		}
		delete(series, "coveredDates")
	}
	if !reflect.DeepEqual(history, none) {
		t.Fatalf("a cancelled answer changed the date's capabilities:\n%v\n%v", history, none)
	}
	if none["canRegister"] != true {
		t.Fatalf("fixture: %v", none)
	}
	if ics := apiDo(t, withAnswer, "GET", path(dates[0])+"/calendar.ics", nil, http.StatusOK); strings.Contains(ics, "STATUS:") {
		t.Fatalf("a cancelled answer's download has a STATUS: %s", ics)
	}

	apiDo(t, admin, "POST", fmt.Sprintf("/api/v1/events/%d/cancel", ev.ID), map[string]any{"date": models.FormatCalendarDate(dates[1].OriginalDate)}, http.StatusOK)
	if got := read(withAnswer, dates[1]); !reflect.DeepEqual(got, map[string]any{"title": "Open evening", "date": models.FormatCalendarDate(dates[1].OriginalDate), "cancelled": true}) {
		t.Fatalf("tombstone: %v", got)
	}
	apiDo(t, without, "GET", path(dates[1]), nil, http.StatusNotFound)
	apiDo(t, withAnswer, "GET", path(dates[1])+"/calendar.ics", nil, http.StatusNotFound)
}

func TestCancelOccurrenceChecksEndAndAuditsTheDecision(t *testing.T) {
	f := newDateEditFixture(t)
	path := "/api/v1/events/" + itoa(f.event.ID) + "/cancel"
	cancel := func(t *testing.T, i, status int) {
		t.Helper()
		apiDo(t, f.admin, "POST", path, map[string]any{"date": f.date(i)}, status)
	}

	t.Run("a date at its end stays scheduled", func(t *testing.T) {
		f.app.clock = func() time.Time { return f.dates[0].EndsAt }
		cancel(t, 0, http.StatusConflict)
		if got := occurrenceRow(t, f.app, f.dates[0].ID); got.Status != models.OccurrenceScheduled {
			t.Fatalf("the ended date was cancelled: %+v", got)
		}
		if rows, _ := auditRows(t, f.app, audit.EventDatesCancelled); len(rows) != 0 {
			t.Fatalf("the refusal wrote a cancellation audit: %+v", rows)
		}
	})

	t.Run("an underway date can be cancelled once", func(t *testing.T) {
		f.app.clock = func() time.Time { return f.dates[1].StartsAt.Add(time.Minute) }
		cancel(t, 1, http.StatusOK)
		cancel(t, 1, http.StatusOK)
		if got := occurrenceRow(t, f.app, f.dates[1].ID); got.Status != models.OccurrenceCancelled || got.CancelSource != models.CancelSourceManual {
			t.Fatalf("the date stayed on: %+v", got)
		}
		if rows, _ := auditRows(t, f.app, audit.EventDatesCancelled); len(rows) != 1 || rows[0].TargetID != f.event.ID || rows[0].ActorID == nil || *rows[0].ActorID != f.adminUser.ID {
			t.Fatalf("cancellation audit: %+v", rows)
		}
	})

	t.Run("a manual cancellation can take precedence over the series", func(t *testing.T) {
		eventPath := "/api/v1/events/" + itoa(f.event.ID)
		apiDo(t, f.admin, "PUT", eventPath, seriesBody(t, f.admin, eventPath, map[string]any{"recurrenceRule": "FREQ=WEEKLY;COUNT=2"}), http.StatusOK)
		if got := occurrenceRow(t, f.app, f.dates[2].ID); got.CancelSource != models.CancelSourceSeries {
			t.Fatalf("the series did not cancel the date: %+v", got)
		}
		cancel(t, 2, http.StatusOK)
		if got := occurrenceRow(t, f.app, f.dates[2].ID); got.CancelSource != models.CancelSourceManual {
			t.Fatalf("the date was not cancelled manually: %+v", got)
		}
		rows, _ := auditRows(t, f.app, audit.EventDatesCancelled)
		if len(rows) != 3 {
			t.Fatalf("expected separate audit rows for each decision: %+v", rows)
		}
		for i, wantSource := range []string{models.CancelSourceManual, models.CancelSourceSeries, models.CancelSourceManual} {
			var meta struct {
				IDs    []uint `json:"occurrenceIds"`
				Source string `json:"source"`
			}
			if err := json.Unmarshal([]byte(rows[i].MetaJSON), &meta); err != nil || meta.Source != wantSource {
				t.Fatalf("cancellation audit %d: %+v (%v)", i, meta, err)
			}
			if i > 0 && !slices.Contains(meta.IDs, f.dates[2].ID) {
				t.Fatalf("cancellation audit %d omitted the date: %+v", i, meta)
			}
		}
	})
}

// occurrencesForEvents is occurrencesForEventsErr for tests that only look
// at the rows.
func (a *App) occurrencesForEvents(eventIDs []uint, from, to string, startAfter *time.Time, me *models.User) []occurrenceJSON {
	out, _ := a.occurrencesForEventsErr(eventIDs, from, to, startAfter, me)
	return out
}

// A series edit that drops dates cancels them and says why. A later edit
// whose rule includes them again brings them back on and tells the people who
// were told they were cancelled; their registrations were never removed, so
// none is recreated. A date a person cancelled stays cancelled.
func TestSeriesEditRestoresSeriesCancelledDates(t *testing.T) {
	app := testApp(t)
	admin, gid := openGroup(t, app, "Restore")
	newTestUser(t, app, "sam", "password1234", models.RoleMember)
	newTestUser(t, app, "pat", "password1234", models.RoleMember)
	sam := loginAPI(t, app, "sam", "password1234")
	pat := loginAPI(t, app, "pat", "password1234")
	for _, c := range []*apiClient{sam, pat} {
		if w := c.do("POST", "/api/v1/groups/"+gid+"/join", map[string]any{}, true); w.Code != http.StatusOK {
			t.Fatalf("join: %d %s", w.Code, w.Body.String())
		}
	}
	start := atUTC(time.Now().AddDate(0, 0, 14), 19, 0)
	d1 := start.AddDate(0, 0, 7).Format("2006-01-02")
	d2 := start.AddDate(0, 0, 14).Format("2006-01-02")
	d3 := start.AddDate(0, 0, 21).Format("2006-01-02")
	d4 := start.AddDate(0, 0, 28).Format("2006-01-02")
	w := admin.do("POST", "/api/v1/groups/"+gid+"/events", map[string]any{
		"title": "Prayer", "startsLocal": start.Format("2006-01-02T15:04"),
		"durationMinutes": 60, "tzid": "UTC", "recurrenceRule": weeklyRule(start),
	}, true)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		Event eventJSON `json:"event"`
	}
	decodeJSON(t, w, &created)
	eid := itoa(created.Event.ID)
	reg := func(c *apiClient, date, status string) {
		t.Helper()
		if w := c.do("PUT", "/api/v1/events/"+eid+"/registrations", map[string]any{"date": date, "status": status}, true); w.Code != http.StatusOK {
			t.Fatalf("register %s: %d %s", date, w.Code, w.Body.String())
		}
	}
	reg(sam, d2, "going")
	reg(sam, d3, "going")
	reg(sam, d4, "going")
	reg(pat, d3, "maybe")

	occ := func(date string) models.EventOccurrence {
		t.Helper()
		var o models.EventOccurrence
		if err := app.DB.Where("event_id = ? AND original_date = ?", created.Event.ID, date).First(&o).Error; err != nil {
			t.Fatal(err)
		}
		return o
	}
	count := func(c *apiClient, title string) int {
		n := 0
		for _, item := range feed(t, c, "") {
			if item.Title == title {
				n++
			}
		}
		return n
	}
	registrations := func() int64 {
		var n int64
		app.DB.Model(&models.Registration{}).Joins("JOIN event_occurrences ON event_occurrences.id = registrations.occurrence_id").
			Where("event_occurrences.event_id = ? AND registrations.status <> ?", created.Event.ID, models.RegCancelled).Count(&n)
		return n
	}
	before := registrations()
	// The dates in notice data and audit rows are the dynamic sinks the
	// compiler cannot check: they must read exactly YYYY-MM-DD.
	noticeDates := func(kind string) []string {
		t.Helper()
		var rows []models.Notification
		if err := app.DB.Where("kind = ?", kind).Find(&rows).Error; err != nil {
			t.Fatal(err)
		}
		out := []string{}
		for _, n := range rows {
			var data struct {
				Date string `json:"date"`
			}
			if err := json.Unmarshal([]byte(n.Data), &data); err != nil {
				t.Fatal(err)
			}
			out = append(out, data.Date)
		}
		sort.Strings(out)
		return out
	}
	storedUntil := func() string {
		t.Helper()
		var ev models.Event
		if err := app.DB.First(&ev, created.Event.ID).Error; err != nil {
			t.Fatal(err)
		}
		if ev.RRuleUntil == nil {
			return ""
		}
		return models.FormatCalendarDate(*ev.RRuleUntil)
	}
	if storedUntil() != "" {
		t.Fatalf("a series with no end has until %q", storedUntil())
	}

	// A person cancels d4 by hand. Sam is told once.
	if w := admin.do("POST", "/api/v1/events/"+eid+"/cancel", map[string]any{"date": d4}, true); w.Code != http.StatusOK {
		t.Fatalf("cancel d4: %d %s", w.Code, w.Body.String())
	}
	if got := occ(d4); got.Status != models.OccurrenceCancelled || got.CancelSource != models.CancelSourceManual {
		t.Fatalf("d4 after manual cancel: %+v", got)
	}

	// Ending the series on d1 drops d2 and d3; d4 stays a manual cancellation.
	if w := admin.do("PUT", "/api/v1/events/"+eid, seriesBody(t, admin, "/api/v1/events/"+eid, map[string]any{"rruleUntil": d1}), true); w.Code != http.StatusOK {
		t.Fatalf("until: %d %s", w.Code, w.Body.String())
	}
	for _, d := range []string{d2, d3} {
		if got := occ(d); got.Status != models.OccurrenceCancelled || got.CancelSource != models.CancelSourceSeries {
			t.Fatalf("%s after until: %+v", d, got)
		}
	}
	if got := occ(d4); got.CancelSource != models.CancelSourceManual {
		t.Fatalf("d4 source changed: %q", got.CancelSource)
	}
	if got := count(sam, "Cancelled: Prayer"); got != 3 {
		t.Fatalf("sam cancelled notices = %d, want 3 (d2, d3, d4)", got)
	}
	if got := storedUntil(); got != d1 {
		t.Fatalf("stored until = %q, want %q", got, d1)
	}
	if got, want := strings.Join(noticeDates(notify.OccurrenceCancelled), ","), strings.Join([]string{d2, d3, d4}, ","); got != want {
		t.Fatalf("cancelled notice dates = %q, want %q", got, want)
	}
	if got := count(pat, "Cancelled: Prayer"); got != 0 {
		t.Fatalf("pat (maybe) cancelled notices = %d", got)
	}
	if registrations() != before {
		t.Fatal("cancelling changed registrations")
	}

	// Clearing the end date brings d2 and d3 back, not d4.
	if w := admin.do("PUT", "/api/v1/events/"+eid, seriesBody(t, admin, "/api/v1/events/"+eid, map[string]any{"rruleUntil": ""}), true); w.Code != http.StatusOK {
		t.Fatalf("clear until: %d %s", w.Code, w.Body.String())
	}
	for _, d := range []string{d2, d3} {
		if got := occ(d); got.Status != models.OccurrenceScheduled || got.CancelSource != "" || got.StartsAt.UTC().Hour() != 19 {
			t.Fatalf("%s after clearing until: %+v", d, got)
		}
	}
	if got := occ(d4); got.Status != models.OccurrenceCancelled || got.CancelSource != models.CancelSourceManual {
		t.Fatalf("manually cancelled d4 came back: %+v", got)
	}
	if got := count(sam, "Back on: Prayer"); got != 2 {
		t.Fatalf("sam back-on notices = %d, want 2 (d2, d3)", got)
	}
	if got := count(pat, "Back on: Prayer"); got != 0 {
		t.Fatalf("pat (maybe) back-on notices = %d", got)
	}
	if registrations() != before {
		t.Fatalf("restoring changed registrations: %d then %d", before, registrations())
	}
	if storedUntil() != "" {
		t.Fatalf("clearing the end date left until %q", storedUntil())
	}
	if got, want := strings.Join(noticeDates(notify.OccurrenceRestored), ","), strings.Join([]string{d2, d3}, ","); got != want {
		t.Fatalf("restored notice dates = %q, want %q", got, want)
	}
	var audits int64
	app.DB.Model(&models.AuditLog{}).Where("action = ? AND target_type = 'event' AND target_id = ?", audit.EventDatesRestored, created.Event.ID).Count(&audits)
	if audits != 1 {
		t.Fatalf("restore audit rows = %d", audits)
	}
	var restoredAudit models.AuditLog
	if err := app.DB.Where("action = ? AND target_type = 'event' AND target_id = ?", audit.EventDatesRestored, created.Event.ID).First(&restoredAudit).Error; err != nil {
		t.Fatal(err)
	}
	var restoredMeta struct {
		Dates []string `json:"dates"`
	}
	if err := json.Unmarshal([]byte(restoredAudit.MetaJSON), &restoredMeta); err != nil {
		t.Fatal(err)
	}
	// restoreSeriesDates records every occurrence the edit brought back, not
	// only the ones someone registered for. The series has no end date again,
	// so that is each generated date after d1 except the manually cancelled d4,
	// in date order. The people told (noticeDates above) are only d2 and d3.
	var generated []models.EventOccurrence
	if err := app.DB.Where("event_id = ?", created.Event.ID).Order("original_date").Find(&generated).Error; err != nil {
		t.Fatal(err)
	}
	wantRestored := []string{}
	for _, o := range generated {
		if day := models.FormatCalendarDate(o.OriginalDate); day > d1 && o.Status == models.OccurrenceScheduled && o.CancelSource == "" {
			wantRestored = append(wantRestored, day)
		}
	}
	if len(wantRestored) <= 2 || wantRestored[0] != d2 || wantRestored[1] != d3 {
		t.Fatalf("restored series dates %v, want d2 %s and d3 %s first, then the rest of the series", wantRestored, d2, d3)
	}
	for _, day := range wantRestored {
		if day == d4 {
			t.Fatalf("manually cancelled %s counted as restored in %v", d4, wantRestored)
		}
	}
	for _, day := range restoredMeta.Dates {
		if parsed, err := time.Parse("2006-01-02", day); err != nil || parsed.Format("2006-01-02") != day {
			t.Fatalf("restore audit date %q is not YYYY-MM-DD", day)
		}
	}
	if strings.Join(restoredMeta.Dates, ",") != strings.Join(wantRestored, ",") {
		t.Fatalf("restore audit dates = %v, want %v", restoredMeta.Dates, wantRestored)
	}

	// Cancelling again after a restore tells people again.
	if w := admin.do("PUT", "/api/v1/events/"+eid, seriesBody(t, admin, "/api/v1/events/"+eid, map[string]any{"rruleUntil": d1}), true); w.Code != http.StatusOK {
		t.Fatalf("until again: %d %s", w.Code, w.Body.String())
	}
	if got := count(sam, "Cancelled: Prayer"); got != 5 {
		t.Fatalf("sam cancelled notices after second cut = %d, want 5", got)
	}
}

// events.original_date and events.rrule_until are date columns, so a date
// that cannot exist would fail inside Postgres. Every place that takes one
// answers as it did when the columns were text: a missing date, or a 400 where
// the field is checked first.
func TestImpossibleDatesKeepTheirAnswers(t *testing.T) {
	app := testApp(t)
	admin, gid := openGroup(t, app, "Impossible dates")
	eventID, _ := seedCappedEvent(t, app, admin, gid, intPtr(5), false)
	snapshot := func() string {
		t.Helper()
		var state struct {
			Events []models.Event
			Dates  []models.EventOccurrence
		}
		if err := app.DB.Order("id").Find(&state.Events).Error; err != nil {
			t.Fatal(err)
		}
		if err := app.DB.Order("id").Find(&state.Dates).Error; err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(state)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	before := snapshot()
	apiDo(t, admin, "POST", "/api/v1/groups/"+gid+"/events", map[string]any{
		"title": "Never", "startsLocal": "0000-01-01T09:00", "durationMinutes": 60, "tzid": "UTC",
	}, http.StatusBadRequest)
	if snapshot() != before {
		t.Fatal("rejected year-zero creation changed events or occurrences")
	}
	apiDo(t, admin, "PUT", fmt.Sprintf("/api/v1/events/%d", eventID), seriesBody(t, admin, fmt.Sprintf("/api/v1/events/%d", eventID), map[string]any{
		"title": "Never updated", "startsLocal": "0000-01-01T09:00", "recurrenceRule": "", "rruleUntil": "",
	}), http.StatusBadRequest)
	if snapshot() != before {
		t.Fatal("rejected year-zero update changed events or occurrences")
	}
	absent, _ := registerAs(admin, eventID, "2099-12-31", "going", nil)
	for _, bad := range []string{"2026-02-30", "2027-02-29", "2026-13-01", "2026-04-31", "0000-01-01"} {
		if code, body := registerAs(admin, eventID, bad, "going", nil); code != absent || code >= http.StatusInternalServerError {
			t.Fatalf("register on %s: %d %s, want %d like any other date the event lacks", bad, code, body, absent)
		}
		apiDo(t, admin, "GET", fmt.Sprintf("/api/v1/events/%d/dates/%s", eventID, bad), nil, http.StatusNotFound)
		apiDo(t, admin, "GET", fmt.Sprintf("/api/v1/events/%d/dates/%s/calendar.ics", eventID, bad), nil, http.StatusNotFound)
		apiDo(t, admin, "POST", fmt.Sprintf("/api/v1/events/%d/cancel", eventID), map[string]any{"date": bad}, http.StatusBadRequest)
		apiDo(t, admin, "POST", dateEditPath(eventID, bad), dateEditBody("1.0000000000000000.0.0.0.-", map[string]any{"location": "Hall"}), http.StatusBadRequest)
		// The retired writer still refuses a malformed date before date_page_outdated.
		apiDo(t, admin, "PUT", fmt.Sprintf("/api/v1/events/%d/occurrences/%s", eventID, bad), map[string]any{"location": "Hall"}, http.StatusBadRequest)
		// parseDateWindow checks from and to on the group list and the calendar.
		// The church-wide /api/v1/events list takes no window (it uses
		// upcomingWindow), so it ignores the parameters and answers 200 as it did
		// before the columns became dates.
		apiDo(t, admin, "GET", "/api/v1/groups/"+gid+"/events?from="+bad+"&to=2030-01-01", nil, http.StatusBadRequest)
		apiDo(t, admin, "GET", "/api/v1/groups/"+gid+"/events?from=2030-01-01&to="+bad, nil, http.StatusBadRequest)
		apiDo(t, admin, "GET", "/api/v1/calendar?from="+bad+"&to=2030-01-01", nil, http.StatusBadRequest)
		apiDo(t, admin, "GET", "/api/v1/events?from="+bad+"&to=2030-01-01", nil, http.StatusOK)
		apiDo(t, admin, "POST", "/api/v1/groups/"+gid+"/events", map[string]any{
			"title": "Never", "startsLocal": "2030-01-07T19:00", "durationMinutes": 60, "tzid": "UTC",
			"recurrenceRule": "FREQ=WEEKLY", "rruleUntil": bad,
		}, http.StatusBadRequest)
	}
}

// A leap day is a real end date, kept as written; a one-digit hour that
// time.Parse accepts is stored in the full YYYY-MM-DDTHH:MM the column's
// check wants.
func TestEventStoresLeapUntilAndPaddedHour(t *testing.T) {
	app := testApp(t)
	admin, gid := openGroup(t, app, "Leap")
	start := atUTC(time.Now().AddDate(0, 0, 14), 9, 0)
	w := admin.do("POST", "/api/v1/groups/"+gid+"/events", map[string]any{
		"title": "Leap day", "startsLocal": start.Format("2006-01-02") + "T9:00",
		"durationMinutes": 60, "tzid": "UTC", "recurrenceRule": weeklyRule(start), "rruleUntil": "2032-02-29",
	}, true)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		Event eventJSON `json:"event"`
	}
	decodeJSON(t, w, &created)
	if created.Event.RRuleUntil != "2032-02-29" || created.Event.StartsLocal != start.Format("2006-01-02")+"T09:00" {
		t.Fatalf("created event: until %q, startsLocal %q", created.Event.RRuleUntil, created.Event.StartsLocal)
	}
	var stored models.Event
	if err := app.DB.First(&stored, created.Event.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.RRuleUntil == nil || models.FormatCalendarDate(*stored.RRuleUntil) != "2032-02-29" || stored.StartsLocal != start.Format("2006-01-02")+"T09:00" {
		t.Fatalf("stored event: %+v", stored)
	}
	// Clearing the end date stores NULL, not an empty or zero date.
	apiDo(t, admin, "PUT", fmt.Sprintf("/api/v1/events/%d", created.Event.ID), seriesBody(t, admin, fmt.Sprintf("/api/v1/events/%d", created.Event.ID), map[string]any{"rruleUntil": ""}), http.StatusOK)
	var cleared models.Event
	if err := app.DB.First(&cleared, created.Event.ID).Error; err != nil {
		t.Fatal(err)
	}
	if cleared.RRuleUntil != nil {
		t.Fatalf("a cleared end date is stored as %v, want NULL", cleared.RRuleUntil)
	}
}

// N-18 (inventory 9.2.2, a plain entry): a leader creating an event wider than
// its group tells the other leaders and the admins, from a transaction that
// takes no lock before createEvent. An admin erased meanwhile is left out in
// either order, and the other admin is still told.
func TestVisibilityOverrideAtCreationSerializesWithAdminErasure(t *testing.T) {
	runErasureBarriers(t, 7402007, func(t *testing.T) erasureScene {
		app := testApp(t)
		kept := newTestUser(t, app, "admin", "password1234", models.RoleAdmin)
		victim := newTestUser(t, app, "admin2", "password1234", models.RoleAdmin)
		lead := newTestUser(t, app, "lead", "password1234", models.RoleMember)
		g := newTestGroup(t, app, lead, "Youth", models.GroupActive)
		if err := app.DB.Model(g).Update("visibility", models.VisibilityPrivate).Error; err != nil {
			t.Fatal(err)
		}
		leader := loginAPI(t, app, "lead", "password1234")
		leader.holdNotices = true
		start := feedStart(5)
		return erasureScene{app: app, victim: victim,
			run: func() error {
				return doExpect(leader, "POST", "/api/v1/groups/"+itoa(g.ID)+"/events", map[string]any{
					"title": "Lock-in", "startsLocal": start.Format("2006-01-02T15:04"), "durationMinutes": 60, "tzid": "UTC",
					"visibility": models.VisibilityPublic, "visibilityReason": "invite the neighbors",
				}, http.StatusCreated)
			},
			told: func(t *testing.T) {
				if n := contactCount(t, app, &models.Notification{}, "user_id = ? AND kind = ?", kept.ID, notify.VisibilityOverride); n != 1 {
					t.Fatalf("the other admin has %d visibility notices, want 1", n)
				}
			},
		}
	})
}

// registrantRace is the fixture for the gated event routes: a public group with
// a single-date event thirty days out and three people signed up through the
// real registration route: a victim (the person the erasure removes), a kept
// registrant who is not a group member, and a group member. Two admin clients
// (the one who edits and the one who deletes) leave notice jobs queued so they
// may run on an operation's goroutine.
type registrantRace struct {
	app                  *App
	admin, eraser        *apiClient
	eventID              uint
	date                 string
	victim, kept, member *models.User
}

func newRegistrantRace(t *testing.T) registrantRace {
	t.Helper()
	app := testApp(t)
	admin, gid := openGroup(t, app, "Registrant race")
	newTestUser(t, app, "eraser", "password1234", models.RoleAdmin)
	victim := newTestUser(t, app, "victim", "password1234", models.RoleMember)
	kept := newTestUser(t, app, "kept", "password1234", models.RoleMember)
	member := newTestUser(t, app, "member", "password1234", models.RoleMember)
	var g models.Group
	if err := app.DB.Where("id = ?", gid).First(&g).Error; err != nil {
		t.Fatal(err)
	}
	addMember(t, app, member, &g, models.MembershipMember)
	eventID, date := seedCappedEvent(t, app, admin, gid, nil, false)
	for _, u := range []*models.User{victim, kept, member} {
		if code, body := registerAs(loginAPI(t, app, u.Name, "password1234"), eventID, date, "going", nil); code != http.StatusOK {
			t.Fatalf("%s going: %d %s", u.Name, code, body)
		}
	}
	admin.holdNotices = true
	return registrantRace{app: app, admin: admin, eraser: loginHeld(t, app, "eraser", "password1234"),
		eventID: eventID, date: date, victim: victim, kept: kept, member: member}
}

// N-28 (inventory 9.2.2, a gated entry): a change to where or when a date is
// held tells everyone going or waitlisted on it. The real operation is the
// guarded edit POST /events/:id/occurrences/:date/edits {changes: {location}},
// on a base read through the manager's HTTP read before either order starts
// -> apiEditDate -> singleDateMutation's contactMutation (gate first, with the
// event id, so occurrence access is reconciled first) -> the actor, the event
// row and the occurrence row FOR UPDATE -> the date is saved ->
// notifyEventChanged: the registrants of that date are read (listed
// users only), then notify.WithRecipients locks them all once and each Send
// reuses the scope. The edit answers 200 in both orders.
//
// Sender first: the edit stops after it wrote the victim's notice; the real
// delete waits on the gate and removes it. Deletion first: the edit waits on the
// gate, then reads its registrants after the erasure, so the victim is not in
// the list. The kept registrant and the member are told in both. Not covered:
// the series edit (apiUpdateEvent, which reaches notifyEventChanged for the
// time and place variants at api_events.go:1063 and :1095), the "time" and
// "time and place" variants, and invited holders (sendOccurrenceNotice, which
// has its own plain-entry test under N-32).
func TestEventChangedNoticeSerializesWithRealAdminDelete(t *testing.T) {
	runGatedErasureBarriers(t, 7402121, 7402122, func(t *testing.T) gatedErasureScene {
		f := newRegistrantRace(t)
		base := editBaseOf(t, f.admin, f.eventID, f.date)
		return gatedErasureScene{app: f.app, victim: f.victim, sends: []string{notify.EventChanged},
			run: func() error {
				return doExpect(f.admin, "POST", dateEditPath(f.eventID, f.date), dateEditBody(base.Revision, map[string]any{"location": "The new hall"}), http.StatusOK)
			},
			erase: eraseVia(f.eraser, f.victim),
			told: func(t *testing.T) {
				var o models.EventOccurrence
				if err := f.app.DB.Where("event_id = ?", f.eventID).First(&o).Error; err != nil || o.LocationOverride == nil || *o.LocationOverride != "The new hall" {
					t.Fatalf("the date was not moved to the new place: %+v %v", o, err)
				}
				wantNotices(t, f.app, notify.EventChanged, map[*models.User]int64{f.kept: 1, f.member: 1})
			},
		}
	})
}

// N-16 (inventory 9.2.2, a gated entry): cancelling a date tells everyone going
// or waitlisted on it. The real operation is POST /events/:id/cancel {date} ->
// apiCancelOccurrence -> contactMutation (gate first, with the event id) -> one
// UPDATE marks the date cancelled (its row lock; no earlier row lock) -> notifyOccurrences: the
// registrants are read (listed users only), one WithRecipients lock covers all
// of them, and each Send reuses the scope; the room is freed after. 200 in
// both orders.
//
// Sender first: the cancellation stops after it wrote the victim's notice; the
// delete waits on the gate. Deletion first: the cancellation waits, then does
// not see the victim among the registrants. The kept registrant and the member
// are told in both. Not covered: the series-edit cancel in apiUpdateEvent
// (api_events.go:1016), the restore variant (notifyOccurrences with OccurrenceRestored, same
// fan-out with another kind), and a date with invited holders.
func TestOccurrenceCancelledNoticeSerializesWithRealAdminDelete(t *testing.T) {
	runGatedErasureBarriers(t, 7402123, 7402124, func(t *testing.T) gatedErasureScene {
		f := newRegistrantRace(t)
		return gatedErasureScene{app: f.app, victim: f.victim, sends: []string{notify.OccurrenceCancelled},
			run: func() error {
				return doExpect(f.admin, "POST", "/api/v1/events/"+itoa(f.eventID)+"/cancel", map[string]any{"date": f.date}, http.StatusOK)
			},
			erase: eraseVia(f.eraser, f.victim),
			told: func(t *testing.T) {
				var o models.EventOccurrence
				if err := f.app.DB.Where("event_id = ?", f.eventID).First(&o).Error; err != nil || o.Status != models.OccurrenceCancelled {
					t.Fatalf("the date was not cancelled: %+v %v", o, err)
				}
				wantNotices(t, f.app, notify.OccurrenceCancelled, map[*models.User]int64{f.kept: 1, f.member: 1})
			},
		}
	})
}

// TestSeriesRestoreTellsAnEndedGrantOnlyThatItIsBack: a series edit that
// brings back a date it had dropped tells everyone still signed up that it is
// back on (DEC-91). The departed person, whose grant the drop ended and who
// can't see the date, hears only that something they signed up for is back
// on: not the title or place the same edit set, nor the date's subject or
// clock. The grant stays ended. An active member still going is told
// everything.
func TestSeriesRestoreTellsAnEndedGrantOnlyThatItIsBack(t *testing.T) {
	f := newDepartureReadFixture(t, models.VisibilityInviteOnly, models.RegGoing, policy.OccurrenceOriginDepartureSelf)
	target := f.dates[1]
	// A customized date is kept by series edits; this one must follow the rule.
	gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", target.ID).Updates(map[string]any{"title_override": nil, "location_override": nil}))
	member := newTestUser(t, f.app, "restored member", "password1234", models.RoleMember)
	gormMust(t, f.app.DB.Create(&models.Membership{UserID: member.ID, GroupID: f.group, Role: models.MembershipMember, Status: models.MembershipActive}))
	gormMust(t, f.app.DB.Create(&models.Registration{UserID: &member.ID, OccurrenceID: target.ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
	eid := itoa(f.event.ID)

	apiDo(t, f.admin, "PUT", "/api/v1/events/"+eid, seriesBody(t, f.admin, "/api/v1/events/"+eid, map[string]any{"recurrenceRule": "FREQ=WEEKLY;COUNT=1"}), http.StatusOK)
	if o := occurrenceRow(t, f.app, target.ID); o.Status != models.OccurrenceCancelled || o.CancelSource != models.CancelSourceSeries {
		t.Fatalf("fixture: the date was not dropped: %+v", o)
	}
	if got := savedOccurrenceAccess(t, f); got.Status != policy.OccurrenceAccessEnded {
		t.Fatalf("fixture: dropping the date left the grant %+v", got)
	}
	if s := regStatus(t, f.app, f.recipient.ID, target.ID); s != models.RegGoing {
		t.Fatalf("fixture: the departed row is %s", s)
	}
	mark := markNotices(t, f.app)

	apiDo(t, f.admin, "PUT", "/api/v1/events/"+eid, seriesBody(t, f.admin, "/api/v1/events/"+eid, map[string]any{"recurrenceRule": "FREQ=WEEKLY;COUNT=3",
		"title": "NEWSECRETTITLE", "location": "NEWSECRETPLACE"}), http.StatusOK)
	if o := occurrenceRow(t, f.app, target.ID); o.Status != models.OccurrenceScheduled {
		t.Fatalf("the date did not come back: %+v", o)
	}
	if got := savedOccurrenceAccess(t, f); got.Status != policy.OccurrenceAccessEnded {
		t.Fatalf("the restore revived the grant: %+v", got)
	}
	back := noticesSince(t, f.app, f.recipient.ID, notify.OccurrenceRestored, mark.notice)
	if len(back) != 1 || back[0].Title != "Back on: Church event" || back[0].Body != "Your sign-up is still in place." {
		t.Fatalf("the departed person's restore notices: %+v", back)
	}
	wantWithheld(t, f.app, "the departed person", back[0], f.client, target.ID)
	drainNotices(t, f.app)
	secrets := []string{"NEWSECRETTITLE", "NEWSECRETPLACE", "Retained hidden group", "Retained group place", "/events/" + eid}
	var rows []models.Notification
	gormMust(t, f.app.DB.Where("user_id = ? AND id > ?", f.recipient.ID, mark.notice).Find(&rows))
	for _, n := range rows {
		raw, err := json.Marshal(n)
		gormMust(t, &gorm.DB{Error: err})
		for _, secret := range secrets {
			if strings.Contains(string(raw), secret) {
				t.Fatalf("the departed person was told %s: %s", secret, raw)
			}
		}
	}
	mailed := false
	for _, m := range mailSince(t, f.app, f.recipient.Email, mark) {
		mailed = mailed || strings.Contains(m.Subject, "Back on: Church event")
		for _, secret := range secrets {
			if strings.Contains(m.Subject+m.Body, secret) {
				t.Fatalf("the departed person was mailed %s: %q / %q", secret, m.Subject, m.Body)
			}
		}
	}
	if !mailed {
		t.Fatal("the departed person was not mailed that the date is back on")
	}
	told := noticesSince(t, f.app, member.ID, notify.OccurrenceRestored, mark.notice)
	if len(told) != 1 || !strings.Contains(told[0].Title+told[0].Body, "NEWSECRETTITLE") || told[0].SubjectID == nil || *told[0].SubjectID != target.ID {
		t.Fatalf("the member still going: %+v", told)
	}
}

// A viewer who can see only one date of a hidden series must get none of the
// series: the "omit" keys are absent from the JSON, the blanked fields are
// empty. The tag names are pinned here so a typo in a `series:` tag cannot
// silently stop hiding a field.
func TestHiddenSeriesDateHidesEverySeriesField(t *testing.T) {
	omit := []string{"recurrenceRule", "rruleUntil", "seriesStartsLocal", "seriesVisibility"}
	blank := []string{"description", "childcareNotes", "childcareCapacity", "childcareAges"}

	// The tags name exactly these fields.
	gotOmit, gotBlank := []string{}, []string{}
	typ := reflect.TypeOf(occurrenceJSON{})
	for i := 0; i < typ.NumField(); i++ {
		name := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		switch typ.Field(i).Tag.Get("series") {
		case "omit":
			gotOmit = append(gotOmit, name)
		case "blank":
			gotBlank = append(gotBlank, name)
		case "":
		default:
			t.Fatalf("field %s has an unknown series tag %q", typ.Field(i).Name, typ.Field(i).Tag.Get("series"))
		}
	}
	sort.Strings(omit)
	sort.Strings(gotOmit)
	sort.Strings(blank)
	sort.Strings(gotBlank)
	if !reflect.DeepEqual(omit, gotOmit) || !reflect.DeepEqual(blank, gotBlank) {
		t.Fatalf("series tags: omit %v blank %v, want omit %v blank %v", gotOmit, gotBlank, omit, blank)
	}

	cap := 3
	inst := occurrenceJSON{
		ID: 1, EventID: 2, Date: "2030-01-01", Title: "The date", Location: "Hall",
		Description: "series description", RecurrenceRule: "FREQ=WEEKLY", RRuleUntil: "2031-01-01",
		SeriesStartsLocal: "2030-01-01T19:00", SeriesVisibility: "invite_only",
		ChildcareNotes: "notes", ChildcareCapacity: &cap, ChildcareAges: "2-5",
	}
	full, err := json.Marshal(inst)
	if err != nil {
		t.Fatal(err)
	}
	inst.hideSeriesDetails("/events/2/dates/2030-01-01")
	body, err := json.Marshal(inst)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range omit {
		if _, present := fields[key]; present {
			t.Errorf("hidden-series date still carries %q: %s", key, body)
		}
	}
	for _, key := range blank {
		switch v := fields[key].(type) {
		case nil:
		case string:
			if v != "" {
				t.Errorf("hidden-series date carries %q = %q", key, v)
			}
		default:
			t.Errorf("hidden-series date carries %q = %v", key, v)
		}
	}
	if fields["dateUrl"] != "/events/2/dates/2030-01-01" || fields["title"] != "The date" {
		t.Errorf("the date's own fields must stay: %s", body)
	}
	// A viewer who can see the series still gets every key.
	for _, key := range omit {
		if !strings.Contains(string(full), `"`+key+`"`) {
			t.Errorf("authorized JSON lost %q: %s", key, full)
		}
	}
}

// DEC-147: leaders and admins alike create no event for a group that isn't
// active, and a refusal writes nothing. Church-wide events have no group.
func TestCreateEventNeedsActiveGroup(t *testing.T) {
	app := testApp(t)
	newTestUser(t, app, "admin", "password1234", models.RoleAdmin)
	leader := newTestUser(t, app, "leader", "password1234", models.RoleMember)
	admin := loginAPI(t, app, "admin", "password1234")
	lc := loginAPI(t, app, "leader", "password1234")
	start := atUTC(time.Now().AddDate(0, 0, 7), 19, 0)
	body := map[string]any{"title": "Night", "startsLocal": start.Format("2006-01-02T15:04"), "durationMinutes": 60, "tzid": "UTC"}

	for _, status := range []string{models.GroupPending, models.GroupArchived} {
		g := newTestGroup(t, app, leader, "Not active "+status, status)
		path := "/api/v1/groups/" + itoa(g.ID) + "/events"
		auditBefore := contactCount(t, app, &models.AuditLog{}, "1 = 1")
		datesBefore := contactCount(t, app, &models.EventOccurrence{}, "1 = 1")
		for name, c := range map[string]*apiClient{"leader": lc, "admin": admin} {
			w := c.do("POST", path, body, true)
			if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"code":"group_events_not_approved"`) {
				t.Fatalf("%s creating in a %s group: %d %s", name, status, w.Code, w.Body.String())
			}
		}
		withRoom := map[string]any{"title": "Room night", "startsLocal": start.Format("2006-01-02T15:04"), "durationMinutes": 60, "tzid": "UTC", "roomId": 999999}
		beforeBookings := contactCount(t, app, &models.RoomBooking{}, "1 = 1")
		if w := admin.do("POST", path, withRoom, true); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"code":"group_events_not_approved"`) {
			t.Fatalf("creating with a room in a %s group: %d %s", status, w.Code, w.Body.String())
		}
		if n := contactCount(t, app, &models.RoomBooking{}, "1 = 1"); n != beforeBookings {
			t.Fatalf("refused event added room bookings: %d -> %d", beforeBookings, n)
		}
		if n := contactCount(t, app, &models.Event{}, "group_id = ?", g.ID); n != 0 {
			t.Fatalf("%d events written for a %s group", n, status)
		}
		if n := contactCount(t, app, &models.AuditLog{}, "action = ? AND meta_json->>'groupId' = ?", audit.EventCreated, itoa(g.ID)); n != 0 {
			t.Fatalf("%d event.created audit rows for a %s group", n, status)
		}
		// Every refusal above together: no audit row of any kind (the
		// visibility override's included) and no date.
		if n := contactCount(t, app, &models.AuditLog{}, "1 = 1"); n != auditBefore {
			t.Fatalf("refused creates wrote audit rows for a %s group: %d -> %d", status, auditBefore, n)
		}
		if n := contactCount(t, app, &models.EventOccurrence{}, "1 = 1"); n != datesBefore {
			t.Fatalf("refused creates wrote dates for a %s group: %d -> %d", status, datesBefore, n)
		}
		if status == models.GroupPending {
			if w := admin.do("POST", "/api/v1/admin/groups/"+itoa(g.ID)+"/approve", map[string]any{}, true); w.Code != http.StatusOK {
				t.Fatalf("approve group: %d %s", w.Code, w.Body.String())
			}
			if w := lc.do("POST", path, body, true); w.Code != http.StatusCreated {
				t.Fatalf("creating once the group is approved: %d %s", w.Code, w.Body.String())
			}
		}
	}

	w := admin.do("POST", "/api/v1/events", body, true)
	if w.Code != http.StatusCreated {
		t.Fatalf("church-wide event: %d %s", w.Code, w.Body.String())
	}
}

// The group card and the group page decide a date by the same rule: for every
// kind of viewer and every mix of group visibility, event visibility and date
// override, nextEventAt is the first date of the same viewer's upcoming list.
func TestGroupNextEventAtMatchesUpcomingEvents(t *testing.T) {
	app := testApp(t)
	leader := newTestUser(t, app, "leader", "password1234", models.RoleMember)
	member := newTestUser(t, app, "member", "password1234", models.RoleMember)
	outsider := newTestUser(t, app, "outsider", "password1234", models.RoleMember)
	admin := newTestUser(t, app, "admin", "password1234", models.RoleAdmin)
	guest := newTestUser(t, app, "guest", "password1234", models.RoleMember)
	if err := app.DB.Model(guest).Update("status", models.UserGuest).Error; err != nil {
		t.Fatal(err)
	}
	guest.Status = models.UserGuest

	vis := []string{models.VisibilityPublic, models.VisibilityPrivate, models.VisibilityInviteOnly}
	overrides := []*string{nil}
	for i := range vis {
		overrides = append(overrides, &vis[i])
	}
	first := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Minute)
	for _, status := range []string{models.GroupActive, models.GroupPending, models.GroupArchived} {
		for _, gv := range vis {
			for _, ev := range vis {
				for _, ov := range overrides {
					g := newTestGroup(t, app, leader, fmt.Sprintf("G %s %s %s", status, gv, ev), status)
					if err := app.DB.Model(g).Update("visibility", gv).Error; err != nil {
						t.Fatal(err)
					}
					addMember(t, app, member, g, models.MembershipMember)
					e, _ := nextEventSeed(t, app, g, leader.ID, first, ev, ov)
					// A plain second date, so a hidden first date falls back to it.
					second := first.Add(48 * time.Hour)
					if err := app.DB.Create(&models.EventOccurrence{EventID: e.ID, OriginalDate: calDate(second.Format("2006-01-02")),
						StartsAt: second, EndsAt: second.Add(time.Hour), Status: models.OccurrenceScheduled}).Error; err != nil {
						t.Fatal(err)
					}
				}
			}
		}
	}
	var groups []models.Group
	if err := app.DB.Find(&groups).Error; err != nil {
		t.Fatal(err)
	}
	if len(groups) != 3*len(vis)*len(vis)*len(overrides) {
		t.Fatalf("seeded %d groups", len(groups))
	}

	viewers := []struct {
		kind string
		u    *models.User
	}{{"anonymous", nil}, {"guest", guest}, {"signed-in", outsider}, {"member", member}, {"leader", leader}, {"admin", admin}}
	now := time.Now().UTC()
	from, to := upcomingWindow(now)
	sawNext, sawNone := 0, 0
	for _, v := range viewers {
		got, err := app.groupSummaries(v.u, groups)
		if err != nil {
			t.Fatal(err)
		}
		for i, g := range groups {
			var eventIDs []uint
			if err := app.DB.Model(&models.Event{}).Where("group_id = ?", g.ID).Pluck("id", &eventIDs).Error; err != nil {
				t.Fatal(err)
			}
			upcoming, _, err := app.occurrencesForEventsAfterErr(eventIDs, from, to, &now, v.u, maxGroupUpcomingOccurrences, nil)
			if err != nil {
				t.Fatal(err)
			}
			var want *time.Time
			if len(upcoming) > 0 {
				want = &upcoming[0].StartsAt
				sawNext++
			} else {
				sawNone++
			}
			requireNextAt(t, fmt.Sprintf("%s, %s", v.kind, g.Name), got[i].NextEventAt, want)
		}
	}
	if sawNext == 0 || sawNone == 0 {
		t.Fatalf("the property check needs both visible and hidden cases: visible=%d hidden=%d", sawNext, sawNone)
	}
}

// Seeing a hidden date costs no query per group: the statements groupSummaries
// runs are the same for one group and for many, each with several events and
// per-date overrides.
func TestGroupSummariesDateReadsDoNotGrowWithGroups(t *testing.T) {
	app := testApp(t)
	leader := newTestUser(t, app, "leader", "password1234", models.RoleMember)
	viewer := newTestUser(t, app, "viewer", "password1234", models.RoleMember)
	start := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Minute)
	seedGroup := func(name string) {
		t.Helper()
		g := newTestGroup(t, app, leader, name, models.GroupActive)
		for i, vis := range []string{models.VisibilityPublic, models.VisibilityPrivate, models.VisibilityInviteOnly} {
			override := models.VisibilityPrivate
			nextEventSeed(t, app, g, leader.ID, start.Add(time.Duration(i)*time.Hour), vis, nil)
			nextEventSeed(t, app, g, leader.ID, start.Add(time.Duration(i+3)*time.Hour), vis, &override)
		}
	}
	n := collectionReadCounter(t, app.DB)
	reads := func() int {
		t.Helper()
		var groups []models.Group
		if err := app.DB.Order("id").Find(&groups).Error; err != nil {
			t.Fatal(err)
		}
		*n = 0
		if _, err := app.groupSummaries(viewer, groups); err != nil {
			t.Fatal(err)
		}
		return *n
	}
	seedGroup("Group 1")
	one := reads()
	for i := 2; i <= 6; i++ {
		seedGroup(fmt.Sprintf("Group %d", i))
	}
	if six := reads(); six != one {
		t.Fatalf("groupSummaries ran %d statements for 6 groups and %d for 1", six, one)
	}
}

// A writer commits after the snapshot's first read or its last policy-fact
// read. The result must belong to the state before that commit, never combine
// stale permission with a start time that only exists after permission is lost.
func TestNextVisibleDatesUsesOneSnapshot(t *testing.T) {
	for _, boundary := range []string{"events", "memberships"} {
		for _, change := range []string{"event visibility", "group status", "membership", "deleted event", "deleted group"} {
			t.Run(boundary+"/"+change, func(t *testing.T) {
				app := testApp(t)
				leader := newTestUser(t, app, "leader", "password1234", models.RoleMember)
				viewer := newTestUser(t, app, "viewer", "password1234", models.RoleMember)
				g := newTestGroup(t, app, leader, "Choir", models.GroupActive)
				visibility := models.VisibilityPublic
				if change == "membership" {
					addMember(t, app, viewer, g, models.MembershipMember)
					visibility = models.VisibilityInviteOnly
				}
				before := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Minute)
				after := before.Add(time.Hour)
				ev, occ := nextEventSeed(t, app, g, leader.ID, before, visibility, nil)
				fired := false
				callback := "test:next_dates_snapshot"
				if err := app.DB.Callback().Query().After("gorm:query").Register(callback, func(read *gorm.DB) {
					if fired || read.Error != nil || read.Statement.Table != boundary {
						return
					}
					fired = true
					var mode struct {
						Isolation string
						ReadOnly  string
					}
					if err := read.Session(&gorm.Session{NewDB: true}).Raw(`SELECT
						current_setting('transaction_isolation') AS isolation,
						current_setting('transaction_read_only') AS read_only`).Scan(&mode).Error; err != nil {
						read.AddError(err)
						return
					}
					if mode.Isolation != "repeatable read" || mode.ReadOnly != "on" {
						read.AddError(fmt.Errorf("snapshot mode = %+v", mode))
						return
					}
					read.AddError(app.DB.Transaction(func(write *gorm.DB) error {
						switch change {
						case "event visibility":
							if err := write.Model(&ev).Update("visibility", models.VisibilityInviteOnly).Error; err != nil {
								return err
							}
						case "group status":
							if err := write.Model(g).Update("status", models.GroupArchived).Error; err != nil {
								return err
							}
						case "membership":
							if err := write.Model(&models.Membership{}).Where("group_id = ? AND user_id = ?", g.ID, viewer.ID).
								Update("status", models.MembershipRemoved).Error; err != nil {
								return err
							}
						case "deleted event":
							if err := write.Delete(&ev).Error; err != nil {
								return err
							}
						case "deleted group":
							if err := write.Delete(g).Error; err != nil {
								return err
							}
						}
						return write.Model(&occ).Updates(map[string]any{"starts_at": after, "ends_at": after.Add(time.Hour)}).Error
					}))
				}); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = app.DB.Callback().Query().Remove(callback) })

				got, err := nextVisibleDates(app.DB, viewer, []uint{g.ID})
				if err != nil {
					t.Fatal(err)
				}
				if !fired {
					t.Fatal("writer did not commit between snapshot reads")
				}
				requireNextAt(t, "in-flight snapshot", got[g.ID], &before)
				fresh, err := nextVisibleDates(app.DB, viewer, []uint{g.ID})
				if err != nil {
					t.Fatal(err)
				}
				requireNextAt(t, "fresh snapshot after tightening", fresh[g.ID], nil)
			})
		}
	}
}

func createEventBodyForTest() map[string]any {
	return map[string]any{"title": "Held event", "startsLocal": time.Now().UTC().AddDate(0, 0, 7).Format("2006-01-02") + "T19:00", "durationMinutes": 60, "tzid": "UTC"}
}

func createEventWriteCounts(t *testing.T, app *App) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for name, model := range map[string]any{
		"events": &models.Event{}, "dates": &models.EventOccurrence{}, "audit": &models.AuditLog{},
		"notices": &models.Notification{}, "bookings": &models.RoomBooking{}, "jobs": &models.Job{},
	} {
		out[name] = contactCount(t, app, model, "1 = 1")
	}
	return out
}

func TestCreateEventRechecksAfterHeldBody(t *testing.T) {
	for _, change := range []string{"removed leader", "demoted admin", "disabled account", "archived group", "deleted group", "hidden group"} {
		t.Run(change, func(t *testing.T) {
			app := testApp(t)
			leader := newTestUser(t, app, "leader", "password1234", models.RoleMember)
			other := newTestUser(t, app, "other", "password1234", models.RoleAdmin)
			g := newTestGroup(t, app, leader, "Choir", models.GroupActive)
			addMember(t, app, other, g, models.MembershipLeader)
			actor := leader
			if change == "demoted admin" {
				actor = newTestUser(t, app, "non-leading admin", "password1234", models.RoleAdmin)
			}
			client := loginHeld(t, app, actor.Email, "password1234")
			admin := loginHeld(t, app, other.Email, "password1234")
			body := createEventBodyForTest()
			// A revoked writer must fail before any attempt to book this room.
			body["roomId"] = 999999
			held := client.doHeld("POST", "/api/v1/groups/"+itoa(g.ID)+"/events", body)
			want, code := http.StatusForbidden, ""
			switch change {
			case "removed leader", "hidden group":
				if err := app.DB.Model(&models.Membership{}).Where("user_id = ? AND group_id = ?", actor.ID, g.ID).
					Update("status", models.MembershipRemoved).Error; err != nil {
					t.Fatal(err)
				}
				if change == "hidden group" {
					apiDo(t, admin, "PUT", "/api/v1/groups/"+itoa(g.ID), confirmVisibility(t, admin, "/api/v1/groups/"+itoa(g.ID), map[string]any{"visibility": models.VisibilityInviteOnly}), http.StatusOK)
					want = http.StatusNotFound
				}
			case "demoted admin":
				apiDo(t, admin, "PUT", "/api/v1/admin/users/"+itoa(actor.ID), adminEditBody(admin, actor.ID, map[string]any{"role": models.RoleMember}), http.StatusOK)
				want = http.StatusUnauthorized
			case "disabled account":
				apiDo(t, admin, "PUT", "/api/v1/admin/users/"+itoa(actor.ID), map[string]any{"status": models.UserDisabled}, http.StatusOK)
				want = http.StatusUnauthorized
			case "archived group":
				apiDo(t, admin, "PUT", "/api/v1/groups/"+itoa(g.ID), groupBody(t, admin, "/api/v1/groups/"+itoa(g.ID), map[string]any{"status": models.GroupArchived}), http.StatusOK)
				want, code = http.StatusConflict, "group_events_not_approved"
			case "deleted group":
				if err := app.DB.Delete(g).Error; err != nil {
					t.Fatal(err)
				}
				want = http.StatusNotFound
			}
			before := createEventWriteCounts(t, app)
			held.release()
			w := held.wait(t)
			if w.Code != want || (code != "" && !strings.Contains(w.Body.String(), `"code":"`+code+`"`)) {
				t.Fatalf("create after %s: %d %s, want %d %s", change, w.Code, w.Body.String(), want, code)
			}
			if want == http.StatusForbidden && w.Body.String() != `{"code":"not_allowed","error":"forbidden"}` {
				t.Fatalf("changed preflight refusal body: %s", w.Body.String())
			}
			if want == http.StatusNotFound && w.Body.String() != `{"code":"not_found","error":"group not found"}` {
				t.Fatalf("changed preflight hidden body: %s", w.Body.String())
			}
			if after := createEventWriteCounts(t, app); !reflect.DeepEqual(before, after) {
				t.Fatalf("refused create wrote rows: before=%v after=%v", before, after)
			}
		})
	}
}

func requireCreatedEvent(t *testing.T, app *App, w *httptest.ResponseRecorder) models.Event {
	t.Helper()
	if w.Code != http.StatusCreated {
		t.Fatalf("create event: %d %s", w.Code, w.Body.String())
	}
	var body eventSeriesResponseJSON
	decodeJSON(t, w, &body)
	var ev models.Event
	if err := app.DB.First(&ev, body.Event.ID).Error; err != nil {
		t.Fatal(err)
	}
	if n := contactCount(t, app, &models.AuditLog{}, "target_type = 'event' AND target_id = ? AND action = ?", ev.ID, audit.EventCreated); n != 1 {
		t.Fatalf("event.created audits = %d, want exactly one", n)
	}
	if n := contactCount(t, app, &models.EventOccurrence{}, "event_id = ?", ev.ID); n != 1 {
		t.Fatalf("created dates = %d, want one", n)
	}
	var created models.AuditLog
	if err := app.DB.Where("target_type = 'event' AND target_id = ? AND action = ?", ev.ID, audit.EventCreated).First(&created).Error; err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(created.MetaJSON), &meta); err != nil {
		t.Fatal(err)
	}
	if meta["visibility"] != ev.Visibility {
		t.Fatalf("created-event audit visibility = %v, saved %s", meta["visibility"], ev.Visibility)
	}
	return ev
}

func requireCreateVisibilityAudit(t *testing.T, app *App, ev models.Event, groupVisibility, action string) {
	t.Helper()
	var rows []models.AuditLog
	if err := app.DB.Where("target_type = 'event' AND target_id = ? AND action IN ?", ev.ID,
		[]string{audit.VisibilityChanged, audit.VisibilityOverride}).Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if action == "" {
		if len(rows) != 0 {
			t.Fatalf("inherited/equal visibility wrote audit: %+v", rows)
		}
		return
	}
	if len(rows) != 1 || rows[0].Action != action {
		t.Fatalf("visibility audits = %+v, want one %s", rows, action)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(rows[0].MetaJSON), &meta); err != nil {
		t.Fatal(err)
	}
	if meta["oldVisibility"] != groupVisibility || meta["groupVisibility"] != groupVisibility || meta["newVisibility"] != ev.Visibility ||
		meta["atCreation"] != true || meta["oldLeaderOpened"] != false || meta["leaderOpened"] != ev.LeaderOpened {
		t.Fatalf("creation visibility audit used stale facts: %v", meta)
	}
}

func TestCreateEventUsesGroupVisibilityAfterHeldBody(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicit=%v", explicit), func(t *testing.T) {
			app := testApp(t)
			leader := newTestUser(t, app, "leader", "password1234", models.RoleMember)
			other := newTestUser(t, app, "other leader", "password1234", models.RoleMember)
			admin := newTestUser(t, app, "admin", "password1234", models.RoleAdmin)
			g := newTestGroup(t, app, leader, "Choir", models.GroupActive)
			addMember(t, app, other, g, models.MembershipLeader)
			client := loginHeld(t, app, leader.Email, "password1234")
			coLeader := loginHeld(t, app, other.Email, "password1234")
			body := createEventBodyForTest()
			if explicit {
				body["visibility"], body["visibilityReason"] = models.VisibilityPublic, "Guests welcome"
			}
			held := client.doHeld("POST", "/api/v1/groups/"+itoa(g.ID)+"/events", body)
			apiDo(t, coLeader, "PUT", "/api/v1/groups/"+itoa(g.ID), confirmVisibility(t, coLeader, "/api/v1/groups/"+itoa(g.ID), map[string]any{"visibility": models.VisibilityPrivate}), http.StatusOK)
			held.release()
			ev := requireCreatedEvent(t, app, held.wait(t))
			want, action := models.VisibilityPrivate, ""
			if explicit {
				want, action = models.VisibilityPublic, audit.VisibilityOverride
			}
			if ev.Visibility != want || ev.LeaderOpened != explicit {
				t.Fatalf("saved visibility=%s leaderOpened=%v, want %s/%v", ev.Visibility, ev.LeaderOpened, want, explicit)
			}
			requireCreateVisibilityAudit(t, app, ev, models.VisibilityPrivate, action)
			for _, u := range []*models.User{leader, other, admin} {
				wantNotices := int64(0)
				if explicit && u.ID != leader.ID {
					wantNotices = 1
				}
				if n := contactCount(t, app, &models.Notification{}, "user_id = ? AND kind = ? AND subject_id = ?", u.ID, notify.VisibilityOverride, ev.ID); n != wantNotices {
					t.Fatalf("override notices for %s = %d, want %d", u.Name, n, wantNotices)
				}
			}
			if !explicit {
				anon := newAPIClient(t, app)
				var occ models.EventOccurrence
				if err := app.DB.Where("event_id = ?", ev.ID).First(&occ).Error; err != nil {
					t.Fatal(err)
				}
				for _, surface := range []struct {
					path   string
					status int
				}{
					{"/api/v1/groups/" + itoa(g.ID), http.StatusNotFound},
					{"/api/v1/groups", http.StatusOK},
					{"/cal/public.ics", http.StatusOK},
				} {
					w := anon.do("GET", surface.path, nil, false)
					if w.Code != surface.status || strings.Contains(w.Body.String(), ev.Title) || strings.Contains(w.Body.String(), occ.StartsAt.UTC().Format(time.RFC3339)) {
						t.Fatalf("anonymous surface %s disclosed private create: %d %s", surface.path, w.Code, w.Body.String())
					}
					if surface.status == http.StatusNotFound && w.Body.String() != `{"code":"not_found","error":"group not found"}` {
						t.Fatalf("private group detail changed non-disclosure body: %s", w.Body.String())
					}
					if surface.path == "/api/v1/groups" {
						var listed groupListResponseJSON
						decodeJSON(t, w, &listed)
						if len(listed.Groups) != 0 {
							t.Fatalf("private group or its nextEventAt appeared in anonymous directory: %s", w.Body.String())
						}
					}
				}
			}
		})
	}
}

func TestCreateEventVisibilityDefaultsAndChoices(t *testing.T) {
	for _, groupVis := range []string{models.VisibilityPublic, models.VisibilityPrivate, models.VisibilityInviteOnly} {
		for _, choice := range []string{"", models.VisibilityPublic, models.VisibilityPrivate, models.VisibilityInviteOnly} {
			t.Run(groupVis+"/"+choice, func(t *testing.T) {
				app := testApp(t)
				leader := newTestUser(t, app, "leader", "password1234", models.RoleMember)
				admin := newTestUser(t, app, "admin", "password1234", models.RoleAdmin)
				g := activeGroup(t, app, leader, "Choir", groupVis, "")
				client := loginHeld(t, app, leader.Email, "password1234")
				body := createEventBodyForTest()
				want := groupVis
				if choice != "" {
					body["visibility"] = choice
					want = choice
				}
				ev := requireCreatedEvent(t, app, client.do("POST", "/api/v1/groups/"+itoa(g.ID)+"/events", body, true))
				opened := policy.MoreOpen(want, groupVis)
				if ev.Visibility != want || ev.LeaderOpened != opened {
					t.Fatalf("visibility=%s leaderOpened=%v, want %s/%v", ev.Visibility, ev.LeaderOpened, want, opened)
				}
				action := ""
				if want != groupVis {
					action = audit.VisibilityChanged
					if opened {
						action = audit.VisibilityOverride
					}
				}
				requireCreateVisibilityAudit(t, app, ev, groupVis, action)
				wantNotices := int64(0)
				if opened {
					wantNotices = 1
				}
				if n := contactCount(t, app, &models.Notification{}, "kind = ? AND subject_id = ?", notify.VisibilityOverride, ev.ID); n != wantNotices {
					t.Fatalf("visibility notices = %d, want %d for admin %d", n, wantNotices, admin.ID)
				}
			})
		}
	}
}

func TestCreateEventWaitsForLockedGroup(t *testing.T) {
	app := testApp(t)
	leader := newTestUser(t, app, "leader", "password1234", models.RoleMember)
	g := newTestGroup(t, app, leader, "Choir", models.GroupActive)
	client := loginHeld(t, app, leader.Email, "password1234")
	tx := app.DB.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	var blocker int
	if err := tx.Raw("SELECT pg_backend_pid()").Scan(&blocker).Error; err != nil {
		t.Fatal(err)
	}
	var locked models.Group
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&locked, g.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := tx.Model(&locked).Update("visibility", models.VisibilityPrivate).Error; err != nil {
		t.Fatal(err)
	}
	body := createEventBodyForTest()
	done := queueContact(func() *httptest.ResponseRecorder {
		return client.do("POST", "/api/v1/groups/"+itoa(g.ID)+"/events", body, true)
	})
	// This holder deliberately has no contact gate. A SHARE waiter proves the
	// writing transaction, rather than the preflight, reads the new group row.
	testdb.WaitUntil(t, "create waiting on the group SHARE lock", 5*time.Second, func() bool {
		var waiting bool
		err := app.DB.Raw(`SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'
			AND query LIKE '%FROM "groups"%' AND query LIKE '%FOR SHARE%'
			AND ? = ANY(pg_blocking_pids(pid)))`, blocker).Scan(&waiting).Error
		if err != nil {
			t.Fatal(err)
		}
		return waiting
	})
	if err := tx.Commit().Error; err != nil {
		t.Fatal(err)
	}
	ev := requireCreatedEvent(t, app, awaitContactResponse(t, done))
	if ev.Visibility != models.VisibilityPrivate || ev.LeaderOpened {
		t.Fatalf("create used pre-lock group visibility: %+v", ev)
	}
	requireCreateVisibilityAudit(t, app, ev, models.VisibilityPrivate, "")
}

// Invitation acceptance and departure lock the group before the creating
// leader's user row. Both orders at the gate must serialize without a cycle.
func TestCreateEventSerializesWithGroupFirstWriters(t *testing.T) {
	for _, writer := range []string{"invitation", "departure"} {
		for iteration := 0; iteration < 4; iteration++ {
			t.Run(fmt.Sprintf("%s/%d", writer, iteration), func(t *testing.T) {
				app := testApp(t)
				logs := captureServerLog(t)
				states := erasureTestSQLStates(t, app)
				leader := newTestUser(t, app, "leader", "password1234", models.RoleMember)
				g := newTestGroup(t, app, leader, "Choir", models.GroupActive)
				creator := loginHeld(t, app, leader.Email, "password1234")
				otherClient := cloneContactClient(creator)
				method, path, otherBody := "POST", "", map[string]any{}
				if writer == "invitation" {
					const email = "new-invitee@example.com"
					apiDo(t, creator, "POST", "/api/v1/invitations", map[string]any{"email": email, "groupId": g.ID}, http.StatusOK)
					token := tokenInEmail(t, latestEmail(t, app, email))
					otherClient = newAPIClient(t, app)
					otherClient.fetchCSRF()
					otherClient.holdNotices = true
					path = "/api/v1/invitations/accept"
					otherBody = map[string]any{"token": token, "name": "Invitee", "password": "password1234", "ageBand": "adult"}
				} else {
					subject := newTestUser(t, app, "departing member", "password1234", models.RoleMember)
					addMember(t, app, subject, g, models.MembershipMember)
					path = "/api/v1/groups/" + itoa(g.ID) + "/members/" + itoa(subject.ID) + "/departure"
					preview := getDeparturePreview(t, otherClient, path+"-preview", http.StatusOK)
					otherBody = map[string]any{"snapshot": preview.Snapshot, "keepOccurrenceIds": []uint{}}
				}
				body := createEventBodyForTest()
				create := func() *httptest.ResponseRecorder {
					return creator.do("POST", "/api/v1/groups/"+itoa(g.ID)+"/events", body, true)
				}
				write := func() *httptest.ResponseRecorder { return otherClient.do(method, path, otherBody, true) }
				gate := holdContactGate(t, app)
				first, second := create, write
				if iteration%2 != 0 {
					first, second = write, create
				}
				one := queueContact(first)
				waitContactWaiters(t, app, 1)
				two := queueContact(second)
				waitContactWaiters(t, app, 2)
				if err := gate.Commit().Error; err != nil {
					t.Fatal(err)
				}
				firstResponse, secondResponse := awaitContactResponse(t, one), awaitContactResponse(t, two)
				created, changed := firstResponse, secondResponse
				if iteration%2 != 0 {
					created, changed = secondResponse, firstResponse
				}
				requireCreatedEvent(t, app, created)
				if changed.Code != http.StatusOK {
					t.Fatalf("%s interleaving: %d %s", writer, changed.Code, changed.Body.String())
				}
				if states.Count("40P01") != 0 || strings.Contains(logs.String(), "40P01") || strings.Contains(logs.String(), "deadlock detected") {
					t.Fatalf("%s/create deadlocked: %s", writer, logs.String())
				}
			})
		}
	}
}

func TestCreateEventRefusesMembersAndTeens(t *testing.T) {
	app := testApp(t)
	leader := newTestUser(t, app, "leader", "password1234", models.RoleMember)
	g := newTestGroup(t, app, leader, "Choir", models.GroupActive)
	for _, accountType := range []string{models.AccountAdult, models.AccountTeen} {
		u := newTestUser(t, app, accountType, "password1234", models.RoleMember)
		if err := app.DB.Model(u).Update("account_type", accountType).Error; err != nil {
			t.Fatal(err)
		}
		addMember(t, app, u, g, models.MembershipMember)
		client := loginHeld(t, app, u.Email, "password1234")
		before := createEventWriteCounts(t, app)
		apiDo(t, client, "POST", "/api/v1/groups/"+itoa(g.ID)+"/events", createEventBodyForTest(), http.StatusForbidden)
		if after := createEventWriteCounts(t, app); !reflect.DeepEqual(before, after) {
			t.Fatalf("%s member create wrote rows: %v -> %v", accountType, before, after)
		}
	}
}

// TestDateEditUsesZoneAfterHeldBody: a guarded date edit whose body is held
// while the series changes zone is refused with date_changed and writes
// nothing; the same change saved from a fresh base is read on the new zone,
// across a day boundary each way.
func TestDateEditUsesZoneAfterHeldBody(t *testing.T) {
	for _, tc := range []struct {
		zone      string
		clock     string
		utcOffset time.Duration
	}{
		{"Asia/Tokyo", "00:30", -8*time.Hour - 30*time.Minute},
		{"Pacific/Honolulu", "23:30", 33*time.Hour + 30*time.Minute},
	} {
		t.Run(tc.zone, func(t *testing.T) {
			f := newEventWriterRace(t, false)
			eventPath := "/api/v1/events/" + itoa(f.event.ID)
			wall := f.date + "T" + tc.clock
			changes := map[string]any{"startsLocal": " " + wall + " ", "durationMinutes": 90}
			base := editBaseOf(t, f.actor, f.event.ID, f.date)
			held := f.actor.doHeld("POST", dateEditPath(f.event.ID, f.date), dateEditBody(base.Revision, changes))
			apiDo(t, f.admin, "PUT", eventPath, seriesBody(t, f.admin, eventPath, map[string]any{"tzid": tc.zone}), http.StatusOK)
			var before models.EventOccurrence
			if err := f.app.DB.Where("event_id = ? AND original_date = ?", f.event.ID, f.date).First(&before).Error; err != nil {
				t.Fatal(err)
			}
			state := eventWriterState(t, f.app, f.event.ID)
			held.release()
			if w := held.wait(t); w.Code != http.StatusConflict || answerCode(t, w) != "date_changed" {
				t.Fatalf("the held edit after a zone change: %d %s", w.Code, w.Body.String())
			}
			if after := eventWriterState(t, f.app, f.event.ID); after != state {
				t.Fatalf("the refused held edit wrote:\n%s\n%s", state, after)
			}
			w := saveDate(t, f.actor, f.event.ID, f.date, changes, http.StatusOK, "")
			day, err := models.ParseCalendarDate(f.date)
			if err != nil {
				t.Fatal(err)
			}
			wantStart := day.Add(tc.utcOffset)
			wantEnd := wantStart.Add(90 * time.Minute)
			var response occurrenceResponseJSON
			decodeJSON(t, w, &response)
			got := response.Occurrence
			if got.ID != before.ID || got.Date != f.date || got.TZID != tc.zone || got.StartsLocal != wall ||
				!got.StartsAt.Equal(wantStart) || !got.EndsAt.Equal(wantEnd) || got.DurationMinutes != 90 {
				t.Fatalf("date response: %+v, want %s in %s, UTC %s to %s on original date %s", got, wall, tc.zone, wantStart, wantEnd, f.date)
			}
			var saved models.EventOccurrence
			if err := f.app.DB.First(&saved, before.ID).Error; err != nil {
				t.Fatal(err)
			}
			if !saved.StartsAt.Equal(wantStart) || !saved.EndsAt.Equal(wantEnd) ||
				saved.StartsAtOverride == nil || !saved.StartsAtOverride.Equal(wantStart) ||
				saved.DurationOverride == nil || *saved.DurationOverride != 90 ||
				models.FormatCalendarDate(saved.OriginalDate) != f.date || saved.Status != before.Status || saved.CancelSource != before.CancelSource {
				t.Fatalf("saved date: %+v, want UTC %s to %s with date and status preserved", saved, wantStart, wantEnd)
			}
		})
	}
}

// TestDateEditInvalidStartAfterHeldBodyWritesNothing: an invalid start held
// across a zone change is refused (date_changed, its base is stale), and from
// a fresh base it is invalid_start on the new zone; neither writes.
func TestDateEditInvalidStartAfterHeldBodyWritesNothing(t *testing.T) {
	f := newEventWriterRace(t, false)
	eventPath := "/api/v1/events/" + itoa(f.event.ID)
	changes := map[string]any{
		"startsLocal": "not a wall time", "durationMinutes": 90, "title": "Changed date",
		"location": "New hall", "visibility": models.VisibilityPrivate,
	}
	base := editBaseOf(t, f.actor, f.event.ID, f.date)
	held := f.actor.doHeld("POST", dateEditPath(f.event.ID, f.date), dateEditBody(base.Revision, changes))
	apiDo(t, f.admin, "PUT", eventPath, seriesBody(t, f.admin, eventPath, map[string]any{"tzid": "Asia/Tokyo"}), http.StatusOK)
	before := eventWriterState(t, f.app, f.event.ID)
	held.release()
	if w := held.wait(t); w.Code != http.StatusConflict || answerCode(t, w) != "date_changed" {
		t.Fatalf("the held edit after a zone change: %d %s", w.Code, w.Body.String())
	}
	w := saveDate(t, f.actor, f.event.ID, f.date, changes, http.StatusBadRequest, errInvalidStart.Code)
	var answer map[string]any
	decodeJSON(t, w, &answer)
	if len(answer) != 2 || answer["code"] != errInvalidStart.Code || answer["error"] != errInvalidStart.Message {
		t.Fatalf("invalid date start answer: %v", answer)
	}
	if after := eventWriterState(t, f.app, f.event.ID); before != after {
		t.Fatalf("invalid date start changed rows:\nbefore %s\nafter %s", before, after)
	}
}

// TestDateEditEventDeletedAfterHeldBody: a guarded date edit whose event is
// deleted while its body is held answers the event's 404 and writes nothing.
func TestDateEditEventDeletedAfterHeldBody(t *testing.T) {
	f := newEventWriterRace(t, false)
	eventPath := "/api/v1/events/" + itoa(f.event.ID)
	base := editBaseOf(t, f.actor, f.event.ID, f.date)
	held := f.actor.doHeld("POST", dateEditPath(f.event.ID, f.date), dateEditBody(base.Revision, map[string]any{
		"startsLocal": f.date + "T21:00", "location": "New hall", "visibility": models.VisibilityPrivate,
	}))
	apiDo(t, f.admin, "DELETE", eventPath, nil, http.StatusOK)
	before := eventWriterState(t, f.app, f.event.ID)
	held.release()
	w := held.wait(t)
	if w.Code != http.StatusNotFound {
		t.Fatalf("date edit after event deletion: %d %s", w.Code, w.Body.String())
	}
	var answer map[string]any
	decodeJSON(t, w, &answer)
	if len(answer) != 2 || answer["code"] != "not_found" || answer["error"] != "event not found" {
		t.Fatalf("deleted event answer: %v", answer)
	}
	if after := eventWriterState(t, f.app, f.event.ID); before != after {
		t.Fatalf("date edit after event deletion changed rows:\nbefore %s\nafter %s", before, after)
	}
}

func TestEventSiblingWritersRecheckAfterHeldBody(t *testing.T) {
	for _, writer := range []string{"series", "cancel"} {
		for _, change := range []string{"demoted admin", "disabled account", "removed leader"} {
			t.Run(writer+"/"+change, func(t *testing.T) {
				f := newEventWriterRace(t, change == "demoted admin")
				method, path := "PUT", "/api/v1/events/"+itoa(f.event.ID)
				body := map[string]any{"title": "Changed series", "location": "New hall"}
				if writer == "cancel" {
					method, path, body = "POST", path+"/cancel", map[string]any{"date": f.date}
				}
				held := f.actor.doHeld(method, path, body)
				f.revoke(t, change)
				before := eventWriterState(t, f.app, f.event.ID)
				held.release()
				w := held.wait(t)
				want := http.StatusUnauthorized
				if change == "removed leader" {
					want = http.StatusForbidden
				}
				if w.Code != want {
					t.Fatalf("%s after %s: %d %s, want %d", writer, change, w.Code, w.Body.String(), want)
				}
				if after := eventWriterState(t, f.app, f.event.ID); before != after {
					t.Fatalf("refused %s changed rows:\nbefore %s\nafter %s", writer, before, after)
				}
			})
		}
	}
}

// DELETE has no body. Queue a real demotion first at the contact gate, then
// the authenticated delete, so its transaction must judge the revoked actor.
func TestDeleteEventRechecksAfterContactGate(t *testing.T) {
	f := newEventWriterRace(t, true)
	before := eventRow(t, f.app, f.event.ID)
	gate := holdContactGate(t, f.app)
	demotion := queueContact(func() *httptest.ResponseRecorder {
		return f.admin.do("PUT", "/api/v1/admin/users/"+itoa(f.user.ID), adminEditBody(f.admin, f.user.ID, map[string]any{"role": models.RoleMember}), true)
	})
	waitContactWaiters(t, f.app, 1)
	deletion := queueContact(func() *httptest.ResponseRecorder {
		return f.actor.do("DELETE", "/api/v1/events/"+itoa(f.event.ID), nil, true)
	})
	waitContactWaiters(t, f.app, 2)
	if err := gate.Commit().Error; err != nil {
		t.Fatal(err)
	}
	if w := awaitContactResponse(t, demotion); w.Code != http.StatusOK {
		t.Fatalf("demotion: %d %s", w.Code, w.Body.String())
	}
	if w := awaitContactResponse(t, deletion); w.Code != http.StatusUnauthorized {
		t.Fatalf("delete by demoted admin: %d %s", w.Code, w.Body.String())
	}
	if after := eventRow(t, f.app, f.event.ID); !reflect.DeepEqual(before, after) {
		t.Fatalf("refused delete changed event: before=%+v after=%+v", before, after)
	}
	if n := contactCount(t, f.app, &models.EventOccurrence{}, "event_id = ?", f.event.ID); n != 1 {
		t.Fatalf("refused delete left %d dates, want 1", n)
	}
	if n := contactCount(t, f.app, &models.AuditLog{}, "target_type = 'event' AND target_id = ? AND action = ?", f.event.ID, audit.EventDeleted); n != 0 {
		t.Fatalf("refused delete wrote %d deletion audits", n)
	}
}

// A user row held by an ungated writer must not stop these gated writers
// before their event lock. Adding an actor row lock here would create a
// user/event cycle with registration paths that already hold the user.
func TestEventWritersKeepEventFirstLockOrder(t *testing.T) {
	for _, writer := range []string{"visibility", "series", "delete", "cancel", "date"} {
		t.Run(writer, func(t *testing.T) {
			f := newEventWriterRace(t, false)
			tx := f.app.DB.Begin()
			if tx.Error != nil {
				t.Fatal(tx.Error)
			}
			defer tx.Rollback()
			var blocker int
			if err := tx.Raw("SELECT pg_backend_pid()").Scan(&blocker).Error; err != nil {
				t.Fatal(err)
			}
			var actor models.User
			if err := tx.Clauses(clause.Locking{Strength: "NO KEY UPDATE"}).First(&actor, f.user.ID).Error; err != nil {
				t.Fatal(err)
			}
			var ev models.Event
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&ev, f.event.ID).Error; err != nil {
				t.Fatal(err)
			}
			method, path, body := "PUT", "/api/v1/events/"+itoa(f.event.ID), map[string]any{"title": "Changed title"}
			switch writer {
			case "series":
				body = seriesBody(t, f.actor, path, body)
			case "visibility":
				path = path + "/visibility"
				body = visibilityBody(t, f.actor, path, map[string]any{"visibility": models.VisibilityPrivate})
			case "delete":
				method, body = "DELETE", nil
			case "cancel":
				method, path, body = "POST", path+"/cancel", map[string]any{"date": f.date}
			case "date":
				// The guarded edit, on a base read without a lock.
				base := editBaseOf(t, f.actor, f.event.ID, f.date)
				method, path, body = "POST", dateEditPath(f.event.ID, f.date), dateEditBody(base.Revision, map[string]any{"visibility": models.VisibilityPrivate})
			}
			done := queueContact(func() *httptest.ResponseRecorder { return f.actor.do(method, path, body, true) })
			testdb.WaitUntil(t, "event writer waiting on its first row lock", 5*time.Second, func() bool {
				var waiting bool
				err := f.app.DB.Raw(`SELECT EXISTS (SELECT 1 FROM pg_stat_activity
					WHERE datname = current_database() AND wait_event_type = 'Lock'
					AND query LIKE '%FROM "events"%' AND query LIKE '%FOR UPDATE%'
					AND ? = ANY(pg_blocking_pids(pid)))`, blocker).Scan(&waiting).Error
				if err != nil {
					t.Fatal(err)
				}
				return waiting
			})
			if err := tx.Commit().Error; err != nil {
				t.Fatal(err)
			}
			if w := awaitContactResponse(t, done); w.Code != http.StatusOK {
				t.Fatalf("%s after lock release: %d %s", writer, w.Code, w.Body.String())
			}
		})
	}
}

// A series edit writes clock evidence (DEC-155) for every date that was
// upcoming before it, whatever it did to the date: kept by an override,
// frozen, cancelled by a person, cancelled by the series, restored, moved
// into the past; a date already past keeps what it had, known or not. The
// repair after it changes no clock, notice, audit row, change time or
// calendar output.
func TestSeriesEditRecordsEvidenceForEveryUpcomingDate(t *testing.T) {
	app := testApp(t)
	leader := newTestUser(t, app, "Lou Leader", "password1234", models.RoleMember)
	client := loginAPI(t, app, leader.Name, "password1234")
	group := activeGroup(t, app, leader, "Evening Circle", models.VisibilityPublic, "")
	start := feedStart(3)
	ev := createFeedEvent(t, client, group.ID, "Evening Prayer", start, "FREQ=WEEKLY;COUNT=7")
	eid := itoa(ev.ID)
	dates := seriesClockTestDates(t, app.DB, ev.ID)
	day := func(i int) string { return tombstoneDate(dates[i]) }
	// d1 kept by a title override, d2 frozen at 18:00, d3 cancelled by a
	// person; d0 and d6 are plain.
	saveDate(t, client, ev.ID, day(1), map[string]any{"title": "Special"}, http.StatusOK, "")
	moveDateTo(t, client, ev.ID, day(2), day(2)+"T18:00")
	gormMust(t, app.DB.Model(&dates[3]).Updates(map[string]any{"status": models.OccurrenceCancelled, "cancel_source": models.CancelSourceManual}))

	edit := func(body map[string]any) {
		t.Helper()
		apiDo(t, client, "PUT", "/api/v1/events/"+eid, seriesBody(t, client, "/api/v1/events/"+eid, body), http.StatusOK)
	}
	wantEvidence := func(i int, hhmm, written string) {
		t.Helper()
		rows := clockRowsOf(t, app.DB, dates[i].ID)
		w := ""
		if len(rows) == 1 && rows[0].WrittenLocal != nil {
			w = *rows[0].WrittenLocal
		}
		if len(rows) != 1 || !rows[0].IsFirst || rows[0].StartsAt.UTC().Format("15:04") != hhmm || w != written {
			t.Fatalf("date %d (%s) evidence %+v, want one first row at %s written %q", i, day(i), rows, hhmm, written)
		}
	}
	// The series ends after d3: d4 to d6 are cancelled by the series, and
	// their evidence is the series' written clock on a day the rule no
	// longer makes.
	edit(map[string]any{"rruleUntil": day(3)})
	for i := 4; i <= 6; i++ {
		wantEvidence(i, "19:00", day(i)+"T19:00")
	}
	wantEvidence(1, "19:00", "")
	wantEvidence(2, "19:00", "")
	wantEvidence(3, "19:00", "")
	// The end is lifted: d4 to d6 are restored with the rule's own times.
	edit(map[string]any{"rruleUntil": ""})
	for i := 4; i <= 6; i++ {
		var o models.EventOccurrence
		gormMust(t, app.DB.First(&o, dates[i].ID))
		if o.Status != models.OccurrenceScheduled {
			t.Fatalf("fixture: date %d is %s", i, o.Status)
		}
		wantEvidence(i, "19:00", "")
	}
	wantUsual(t, readSeriesDate(t, client, ev.ID, day(2)), "19:00", changeTime)

	// The clock moves to just before d0's start, and the series moves to
	// 18:00: d0 goes from upcoming into the past in this edit and still gets
	// its evidence.
	app.clock = func() time.Time { return dates[0].StartsAt.Add(-30 * time.Minute) }
	edit(map[string]any{"startsLocal": start.Add(-time.Hour).Format(events.WallLayout)})
	wantEvidence(0, "18:00", "")
	wantEvidence(6, "18:00", "")
	// Later, d0 is past: a known past date keeps its evidence through
	// another edit, and a past date with none stays unknown.
	app.clock = func() time.Time { return dates[1].StartsAt.Add(-30 * time.Minute) }
	gormMust(t, app.DB.Where("occurrence_id = ?", dates[0].ID).Delete(&models.OccurrenceSeriesStart{}))
	app.clock = func() time.Time { return dates[2].StartsAt.Add(-30 * time.Minute) }
	before1 := clockRowsOf(t, app.DB, dates[1].ID)
	edit(map[string]any{"startsLocal": start.Add(-2 * time.Hour).Format(events.WallLayout)})
	if rows := clockRowsOf(t, app.DB, dates[0].ID); len(rows) != 0 {
		t.Fatalf("a past date with no evidence was given some: %+v", rows)
	}
	if rows := clockRowsOf(t, app.DB, dates[1].ID); !reflect.DeepEqual(rows, before1) {
		t.Fatalf("a past date's evidence changed: %+v -> %+v", before1, rows)
	}
	wantEvidence(5, "17:00", "")
	app.clock = nil

	// The repair after the edits: give an upcoming date back its legacy
	// state, then run it twice. Only that date's evidence comes back; no
	// notice, audit row, change time or calendar output moves.
	gormMust(t, app.DB.Where("occurrence_id = ?", dates[6].ID).Delete(&models.OccurrenceSeriesStart{}))
	type sideEffects struct {
		changed, restored int64
		eventUpdated      time.Time
		dateUpdated       []time.Time
		ics, calendar     string
	}
	read := func() sideEffects {
		t.Helper()
		var s sideEffects
		gormMust(t, app.DB.Model(&models.Notification{}).Where("kind = ?", notify.EventChanged).Count(&s.changed))
		gormMust(t, app.DB.Model(&models.AuditLog{}).Where("action = ?", audit.EventDatesRestored).Count(&s.restored))
		gormMust(t, app.DB.Raw(`SELECT updated_at FROM events WHERE id = ?`, ev.ID).Scan(&s.eventUpdated))
		gormMust(t, app.DB.Model(&models.EventOccurrence{}).Where("event_id = ?", ev.ID).Order("id").Pluck("updated_at", &s.dateUpdated))
		s.ics = apiDo(t, client, "GET", dateReadPath(ev.ID, day(5))+"/calendar.ics", nil, http.StatusOK)
		from, to := feedDate(start), feedDate(start.AddDate(0, 0, 60))
		s.calendar = apiDo(t, client, "GET", "/api/v1/calendar?from="+from+"&to="+to, nil, http.StatusOK)
		return s
	}
	before := read()
	runExtend(t, app)
	repaired := clockRowsOf(t, app.DB, dates[6].ID)
	if len(repaired) != 1 || repaired[0].StartsAt.UTC().Format("15:04") != "17:00" {
		t.Fatalf("the repair gave the upcoming date %+v", repaired)
	}
	all := clockCount(t, app.DB)
	runExtend(t, app)
	if after := read(); !reflect.DeepEqual(after, before) {
		t.Fatalf("the repair changed what else the app says:\n%+v\n%+v", before, after)
	}
	if clockCount(t, app.DB) != all || !reflect.DeepEqual(clockRowsOf(t, app.DB, dates[6].ID), repaired) {
		t.Fatal("a second repair rewrote evidence")
	}
}

// A past date keeps the evidence it was written with through a change of the
// series zone, and is compared in that zone, so the zone change alone is not
// a move; upcoming dates take the new zone's evidence.
func TestSeriesZoneChangeKeepsAPastDatesComparison(t *testing.T) {
	app := testApp(t)
	leader := newTestUser(t, app, "Lou Leader", "password1234", models.RoleMember)
	client := loginAPI(t, app, leader.Name, "password1234")
	group := activeGroup(t, app, leader, "Evening Circle", models.VisibilityPublic, "")
	boise := eventZone("America/Boise")
	now := time.Now().In(boise)
	first := time.Date(now.Year(), now.Month(), now.Day(), 19, 0, 0, 0, boise).AddDate(0, 0, -14)
	w := client.do("POST", "/api/v1/groups/"+itoa(group.ID)+"/events", map[string]any{
		"title": "Evening Prayer", "startsLocal": first.Format(events.WallLayout), "durationMinutes": 60,
		"tzid": "America/Boise", "recurrenceRule": "FREQ=WEEKLY;COUNT=5",
	}, true)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		Event eventJSON `json:"event"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	ev := created.Event
	dates := seriesClockTestDates(t, app.DB, ev.ID)
	past, upcoming := dates[0], dates[4]
	apiDo(t, client, "PUT", "/api/v1/events/"+itoa(ev.ID), seriesBody(t, client, "/api/v1/events/"+itoa(ev.ID), map[string]any{"tzid": "UTC"}), http.StatusOK)
	r := readSeriesDate(t, client, ev.ID, tombstoneDate(past))
	if r.StartsLocal[:10] == tombstoneDate(past) {
		t.Fatalf("fixture: the past date still shows on its own day in UTC: %s", r.StartsLocal)
	}
	wantUsual(t, r, "19:00")
	if rows := clockRowsOf(t, app.DB, past.ID); len(rows) != 1 || rows[0].TZID != "America/Boise" {
		t.Fatalf("the past date's evidence: %+v", rows)
	}
	if rows := clockRowsOf(t, app.DB, upcoming.ID); len(rows) != 1 || rows[0].TZID != "UTC" {
		t.Fatalf("an upcoming date's evidence: %+v", rows)
	}
	wantUsual(t, readSeriesDate(t, client, ev.ID, tombstoneDate(upcoming)), "19:00")
}

// TestDateEditEndPassingDuringARoomWaitReruns: the guarded date edit moves
// its date's room after saving the date, and the room may be held by an
// operation of another event. The date's end passes while the edit, which
// shortens it and renames it, waits for the room. The fence at the end of the
// edit rolls the first attempt back, and the rerun ends the retained grant
// before the change: the departed person is told only that the invitation
// ended and cannot read the changed date.
func TestDateEditEndPassingDuringARoomWaitReruns(t *testing.T) {
	f := newDepartureReadFixture(t, models.VisibilityInviteOnly, models.RegGoing, policy.OccurrenceOriginDepartureSelf)
	target := f.dates[1]
	date := models.FormatCalendarDate(target.OriginalDate)
	if w := f.admin.do("PUT", "/api/v1/admin/settings/rooms", map[string]any{"enabled": true}, true); w.Code != http.StatusOK {
		t.Fatalf("rooms on: %d %s", w.Code, w.Body.String())
	}
	var room struct {
		Room roomJSON `json:"room"`
	}
	gormMust(t, &gorm.DB{Error: json.Unmarshal([]byte(apiDo(t, f.admin, "POST", "/api/v1/admin/rooms", map[string]any{"name": "Held hall", "capacity": 40}, http.StatusCreated)), &room)})
	roomPickOK(t, f.admin, "/api/v1/events/"+itoa(f.event.ID)+"/dates/"+date+"/room", map[string]any{"roomId": room.Room.ID})
	member := goingMember(t, f, target.ID)
	mark := markNotices(t, f.app).notice
	clock := newMutableClock(f.app, target.EndsAt.Add(-2*time.Minute))
	attempts := countDateAttempts(f.app)
	base := editBaseOf(t, f.admin, f.event.ID, date)
	release := holdRows(t, f.app, "SELECT id FROM rooms WHERE id = ? FOR UPDATE", room.Room.ID)
	// A minute shorter: a later end on this kept date is refused outright
	// (DEC-156), and the new end is still ahead of the clock the first
	// attempt judges on.
	minutes := int(target.EndsAt.Sub(target.StartsAt)/time.Minute) - 1
	answer := goPostDateEdit(f.admin, f.event.ID, date, dateEditBody(base.Revision, map[string]any{"durationMinutes": minutes, "title": "NEWSECRETTITLE"}))
	waitForLockedUsersQuery(t, f.app, `%FROM "rooms"%FOR UPDATE%`)
	after := target.EndsAt.Add(time.Minute)
	clock.set(after)
	release()
	if w := awaitResponse(t, answer); w.Code != http.StatusOK {
		t.Fatalf("the edit: %d %s", w.Code, w.Body.String())
	}
	if n := attempts.Load(); n != 2 {
		t.Fatalf("the edit ran %d times, want a rerun after the room wait", n)
	}
	if o := occurrenceRow(t, f.app, target.ID); !o.EndsAt.Equal(target.EndsAt.Add(-time.Minute)) {
		t.Fatalf("fixture: the date was not shortened: %+v", o)
	}
	wantGrantEndedAt(t, f, after)
	wantOnlyGrantEnded(t, f, mark, date, "NEWSECRETTITLE")
	if got := noticesSince(t, f.app, member.ID, notify.EventChanged, mark); len(got) != 1 {
		t.Fatalf("the member was told %d changes: %+v", len(got), got)
	}
}

// TestDateEditPromotionReadsItsOwnClock: the guarded date edit decides
// promotion on a clock read when it promotes. A raised cap with a place
// change waits on a going member's row (a recipient lock) while the date
// enters the two-hour cutoff: nobody is promoted, and the leader is told
// someone in line could take a seat.
func TestDateEditPromotionReadsItsOwnClock(t *testing.T) {
	f := newDepartureReadFixture(t, models.VisibilityInviteOnly, models.RegGoing, policy.OccurrenceOriginDepartureSelf)
	target := f.dates[1]
	date := models.FormatCalendarDate(target.OriginalDate)
	gormMust(t, f.app.DB.Model(&models.Event{}).Where("id = ?", f.event.ID).Update("waitlist_enabled", true))
	gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", target.ID).Update("capacity_override", 1))
	going := goingMember(t, f, target.ID)
	waiter := newTestUser(t, f.app, "waiting member", "password1234", models.RoleMember)
	gormMust(t, f.app.DB.Create(&models.Membership{UserID: waiter.ID, GroupID: f.group, Role: models.MembershipMember, Status: models.MembershipActive}))
	at := time.Now().UTC().Add(-time.Minute)
	gormMust(t, f.app.DB.Create(&models.Registration{UserID: &waiter.ID, OccurrenceID: target.ID, Status: models.RegWaitlisted, WaitlistedAt: &at, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
	leader := newTestUser(t, f.app, "series leader", "password1234", models.RoleMember)
	gormMust(t, f.app.DB.Create(&models.Membership{UserID: leader.ID, GroupID: f.group, Role: models.MembershipLeader, Status: models.MembershipActive}))
	mark := markNotices(t, f.app).notice
	cutoff := target.StartsAt.Add(-events.CutoffWindow)
	clock := newMutableClock(f.app, cutoff.Add(-time.Minute))
	base := editBaseOf(t, f.admin, f.event.ID, date)
	release := holdRows(t, f.app, "SELECT id FROM users WHERE id = ? FOR UPDATE", going.ID)
	answer := goPostDateEdit(f.admin, f.event.ID, date, dateEditBody(base.Revision, map[string]any{"location": "A bigger hall", "capacityOverride": 3}))
	waitForLockedUsersQuery(t, f.app, `%FROM users%FOR KEY SHARE%`)
	clock.set(cutoff.Add(time.Minute))
	release()
	if w := awaitResponse(t, answer); w.Code != http.StatusOK {
		t.Fatalf("the edit: %d %s", w.Code, w.Body.String())
	}
	if s := regStatus(t, f.app, waiter.ID, target.ID); s != models.RegWaitlisted {
		t.Fatalf("the waiter was promoted inside the cutoff: %s", s)
	}
	if got := noticesSince(t, f.app, leader.ID, notify.WaitlistCutoff, mark); len(got) != 1 {
		t.Fatalf("the leader got %d cutoff notices: %+v", len(got), got)
	}
}

// TestDateEditRenameAndTightenAfterTheQuestionNamesTheKnownTitle: the same
// rename and tightening agreed the way the date editor asks, through the
// server's 409 date_needs_consent and the same attempt sent again with its
// consent, tells the cancelled outsider, and both guardians' copies, the title
// they knew and never the new private one.
func TestDateEditRenameAndTightenAfterTheQuestionNamesTheKnownTitle(t *testing.T) {
	renameAndTightenNamesTheKnownTitle(t, func(f dateEditFixture) {
		impact := saveDateConsenting(t, f.admin, f.event.ID, f.date(1), map[string]any{"title": "NEWPRIVATETITLE", "visibility": "series"})
		if impact.Tightened[0].Registrants != 1 {
			t.Fatalf("the question counts %d registrants, want the one outsider", impact.Tightened[0].Registrants)
		}
	})
}

// keepOnLeaving signs subject up Going on occ and has them leave group through
// the real departure route, keeping that date (DEC-74).
func keepOnLeaving(t *testing.T, subject *apiClient, groupID uint, eventID uint, occ models.EventOccurrence) {
	t.Helper()
	if code, body := registerAs(subject, eventID, models.FormatCalendarDate(occ.OriginalDate), models.RegGoing, nil); code != http.StatusOK {
		t.Fatalf("going: %d %s", code, body)
	}
	base := "/api/v1/groups/" + itoa(groupID)
	preview := getDeparturePreview(t, subject, base+"/departure-preview", http.StatusOK)
	if w := confirmDeparturePreview(t, subject, "POST", base+"/leave", preview, []uint{occ.ID}, ""); w.Code != http.StatusOK {
		t.Fatalf("leave keeping %s: %d %s", models.FormatCalendarDate(occ.OriginalDate), w.Code, w.Body.String())
	}
}

// seriesClockState is the event's stored clock evidence, for comparing
// before and after an edit.
func seriesClockState(t *testing.T, app *App, eventID uint) string {
	t.Helper()
	var rows []map[string]any
	gormMust(t, app.DB.Raw("SELECT s.* FROM occurrence_series_starts s JOIN event_occurrences o ON o.id = s.occurrence_id WHERE o.event_id = ? ORDER BY s.occurrence_id", eventID).Scan(&rows))
	return fmt.Sprint(rows)
}

// TestSeriesEditRefusesMovingAKeptDateLater (DEC-156): a series edit that
// would move a date someone kept after leaving later, here a public date 121
// days ahead, beyond the event page's list, is refused as a whole, listing
// that date: no date moves, and the event row and its clock evidence are as
// they were. Moving the series earlier applies and keeps the grant; after the
// manager's Remove the same later edit applies.
func TestSeriesEditRefusesMovingAKeptDateLater(t *testing.T) {
	app := testApp(t)
	adminUser := newTestUser(t, app, "series admin", "password1234", models.RoleAdmin)
	admin := loginAPI(t, app, adminUser.Name, "password1234")
	subject := newTestUser(t, app, "series leaver", "password1234", models.RoleMember)
	groupID := createFeedGroup(t, admin, "Open series group", models.VisibilityPublic, "Open hall")
	gormMust(t, app.DB.Create(&models.Membership{UserID: subject.ID, GroupID: groupID, Role: models.MembershipMember, Status: models.MembershipActive}))
	ev := createFeedEvent(t, admin, groupID, "Weekly open", feedStart(2), "FREQ=WEEKLY;COUNT=20")
	var dates []models.EventOccurrence
	gormMust(t, app.DB.Where("event_id = ?", ev.ID).Order("starts_at").Find(&dates))
	kept := dates[17]
	if days := int(kept.StartsAt.Sub(feedStart(0)).Hours() / 24); days != 121 {
		t.Fatalf("fixture: the kept date is %d days ahead", days)
	}
	date := models.FormatCalendarDate(kept.OriginalDate)
	subjectC := loginAPI(t, app, subject.Name, "password1234")
	keepOnLeaving(t, subjectC, groupID, ev.ID, kept)
	var reg models.Registration
	gormMust(t, app.DB.Where("occurrence_id = ? AND user_id = ?", kept.ID, subject.ID).First(&reg))
	if reg.AccessOrigin != policy.OccurrenceOriginDepartureSelf || !reg.AccessLive() {
		t.Fatalf("fixture: the date was not kept: %+v", reg)
	}
	path := "/api/v1/events/" + itoa(ev.ID)
	later := map[string]any{"startsLocal": wallAt(dates[0].StartsAt.Add(time.Hour))}
	state, clocks := eventWriterState(t, app, ev.ID), seriesClockState(t, app, ev.ID)
	wantKeptDates(t, admin.do("PUT", path, seriesBody(t, admin, path, later), true), keptDateCountJSON{Date: date, KeptCount: 1})
	if after := eventWriterState(t, app, ev.ID); after != state {
		t.Fatalf("the refused series edit wrote:\n%s\n%s", state, after)
	}
	// A6: the managers' read of a date beyond the event page's 90 days counts it too.
	_, leader := groupLeader(t, app, groupID, "series leader")
	wantManagedKept(t, admin, ev.ID, date, 1)
	wantManagedKept(t, leader, ev.ID, date, 1)
	if after := seriesClockState(t, app, ev.ID); after != clocks {
		t.Fatalf("the refused series edit changed the clock evidence:\n%s\n%s", clocks, after)
	}

	apiDo(t, admin, "PUT", path, seriesBody(t, admin, path, map[string]any{"startsLocal": wallAt(dates[0].StartsAt.Add(-time.Hour))}), http.StatusOK)
	if o := occurrenceRow(t, app, kept.ID); !o.StartsAt.Equal(kept.StartsAt.Add(-time.Hour)) {
		t.Fatalf("the earlier series edit did not move the kept date: %+v", o)
	}
	if row := loadAccessRow(t, app.DB, reg.ID); row.Status != policy.OccurrenceAccessActive {
		t.Fatalf("the earlier series edit ended the grant: %+v", row)
	}

	apiDo(t, admin, "POST", path+"/registrations/remove", map[string]any{"date": date, "registrationId": reg.ID}, http.StatusOK)
	wantManagedKept(t, leader, ev.ID, date, 0)
	apiDo(t, admin, "PUT", path, seriesBody(t, admin, path, later), http.StatusOK)
	if o := occurrenceRow(t, app, kept.ID); !o.StartsAt.Equal(kept.StartsAt.Add(time.Hour)) {
		t.Fatalf("the series edit after Remove did not move the date: %+v", o)
	}
}

// seriesRevisionOf is the revision c's event page read carries, "" when it
// carries none.
func seriesRevisionOf(t *testing.T, c *apiClient, eventID uint) string {
	t.Helper()
	w := c.do("GET", "/api/v1/events/"+itoa(eventID), nil, false)
	if w.Code != http.StatusOK {
		t.Fatalf("event page: %d %s", w.Code, w.Body.String())
	}
	var page struct {
		Event map[string]any `json:"event"`
	}
	decodeJSON(t, w, &page)
	rev, _ := page.Event["revision"].(string)
	return rev
}

// DEC-162: a series save names the read it was made from, and one made from
// any other read is refused with nothing written.
func TestSeriesSaveNamesTheReadItWasMadeFrom(t *testing.T) {
	f := newBookingFixture(t)
	series := createFeedEvent(t, f.leader, f.group, "Weekly supper", feedStart(3), "FREQ=WEEKLY;COUNT=4")
	path := "/api/v1/events/" + itoa(series.ID)
	member := newTestUser(t, f.app, "series member", "password1234", models.RoleMember)
	gormMust(t, f.app.DB.Create(&models.Membership{UserID: member.ID, GroupID: f.group, Role: models.MembershipMember, Status: models.MembershipActive}))
	memberC := loginAPI(t, f.app, member.Name, "password1234")

	if rev := seriesRevisionOf(t, memberC, series.ID); rev != "" {
		t.Fatalf("a member's read carries a revision: %q", rev)
	}
	opened := seriesRevisionOf(t, f.leader, series.ID)
	if opened == "" || opened != seriesRevisionOf(t, f.admin, series.ID) {
		t.Fatalf("managers' revisions: %q and %q", opened, seriesRevisionOf(t, f.admin, series.ID))
	}

	// A save from the current read saves, and its answer carries the new
	// revision, the one the next read carries.
	var saved eventSeriesResponseJSON
	w := f.leader.do("PUT", path, map[string]any{"title": "Thursday supper", "revision": opened}, true)
	if w.Code != http.StatusOK {
		t.Fatalf("current save: %d %s", w.Code, w.Body.String())
	}
	decodeJSON(t, w, &saved)
	current := seriesRevisionOf(t, f.leader, series.ID)
	if saved.Event.Revision == opened || saved.Event.Revision != current {
		t.Fatalf("revisions: opened %q, answer %q, next read %q", opened, saved.Event.Revision, current)
	}

	other := seriesRevisionOf(t, f.leader, f.eventA.ID)
	gen := strings.Split(current, ".")
	gen[1] = strings.Repeat("0", 16)
	for name, body := range map[string]map[string]any{
		"older read":       {"title": "Stale supper", "revision": opened},
		"missing":          {"title": "Stale supper"},
		"empty":            {"title": "Stale supper", "revision": ""},
		"malformed":        {"title": "Stale supper", "revision": "not a token"},
		"another event":    {"title": "Stale supper", "revision": other},
		"other generation": {"title": "Stale supper", "revision": strings.Join(gen, ".")},
		"a number":         {"title": "Stale supper", "revision": 7},
	} {
		t.Run(name, func(t *testing.T) {
			before := eventWriterState(t, f.app, series.ID)
			w := f.leader.do("PUT", path, body, true)
			if name == "a number" {
				if w.Code != http.StatusBadRequest {
					t.Fatalf("a revision that is not a string: %d %s", w.Code, w.Body.String())
				}
			} else {
				wantCode(t, "refused save", w, http.StatusConflict, "event_changed")
			}
			if after := eventWriterState(t, f.app, series.ID); after != before {
				t.Fatalf("the refused save wrote:\n%s\n%s", before, after)
			}
		})
	}

	// A save that changes nothing leaves the revision (the trigger counts
	// only a changed tracked column), so the same read can still save.
	apiDo(t, f.leader, "PUT", path, map[string]any{"title": "Thursday supper", "revision": current}, http.StatusOK)
	if got := seriesRevisionOf(t, f.leader, series.ID); got != current {
		t.Fatalf("a save that changed nothing moved the revision: %q to %q", current, got)
	}
	apiDo(t, f.leader, "PUT", path, map[string]any{"title": "Friday supper", "revision": current}, http.StatusOK)

	// A non-manager is refused as before, whatever revision it sends.
	apiDo(t, memberC, "PUT", path, map[string]any{"title": "Taken", "revision": seriesRevisionOf(t, f.leader, series.ID)}, http.StatusForbidden)
}

// The event's other writers raise the revision too, so a series editor open
// across a visibility save or a series room change must read the event again;
// the daily extender and clearing the clock flag do not.
func TestSeriesRevisionFollowsTheEventsOtherWriters(t *testing.T) {
	f := newBookingFixture(t)
	series := createFeedEvent(t, f.leader, f.group, "Weekly choir", feedStart(3), "FREQ=WEEKLY;COUNT=4")
	path := "/api/v1/events/" + itoa(series.ID)
	stale := func(t *testing.T, opened string) {
		t.Helper()
		if now := seriesRevisionOf(t, f.leader, series.ID); now == opened {
			t.Fatalf("the revision did not move: %q", now)
		}
		wantCode(t, "save from before the change", f.leader.do("PUT", path, map[string]any{"title": "Late", "revision": opened}, true), http.StatusConflict, "event_changed")
	}

	t.Run("visibility", func(t *testing.T) {
		opened := seriesRevisionOf(t, f.leader, series.ID)
		apiDo(t, f.leader, "PUT", path+"/visibility", visibilityBody(t, f.leader, path+"/visibility", map[string]any{"visibility": models.VisibilityInviteOnly}), http.StatusOK)
		stale(t, opened)
	})
	t.Run("series room", func(t *testing.T) {
		opened := seriesRevisionOf(t, f.leader, series.ID)
		if code, _ := f.seriesRoom(t, f.leader, series, map[string]any{"roomId": f.hall.ID}); code != http.StatusOK {
			t.Fatalf("series room: %d", code)
		}
		stale(t, opened)
	})
	// Neither the extender nor the clock repair's flag clear moves the revision.
	t.Run("extender and the clock flag", func(t *testing.T) {
		opened := seriesRevisionOf(t, f.leader, series.ID)
		var last models.EventOccurrence
		gormMust(t, f.app.DB.Where("event_id = ?", series.ID).Order("original_date DESC").First(&last))
		gormMust(t, f.app.DB.Delete(&last))
		gormMust(t, f.app.DB.Model(&models.Event{}).Where("id = ?", series.ID).UpdateColumn("series_clock_dirty", true))
		if err := eventdates.Extend(f.app.DB, time.Now().UTC(), f.app.afterExtend()); err != nil {
			t.Fatal(err)
		}
		if err := eventdates.ClearDirty(f.app.DB, series.ID); err != nil {
			t.Fatal(err)
		}
		if n := contactCount(t, f.app, &models.EventOccurrence{}, "event_id = ? AND original_date = ?", series.ID, last.OriginalDate); n != 1 {
			t.Fatalf("fixture: the extender did not add the date back (%d)", n)
		}
		if now := seriesRevisionOf(t, f.leader, series.ID); now != opened {
			t.Fatalf("background work moved the revision: %q to %q", opened, now)
		}
		apiDo(t, f.leader, "PUT", path, map[string]any{"title": "Weekly choir practice", "revision": opened}, http.StatusOK)
	})
}

// Two saves from the same read: exactly one is stored, the other is refused.
func TestConcurrentSeriesSavesFromOneReadExactlyOneWins(t *testing.T) {
	f := newBookingFixture(t)
	series := createFeedEvent(t, f.leader, f.group, "Weekly study", feedStart(3), "FREQ=WEEKLY;COUNT=4")
	path := "/api/v1/events/" + itoa(series.ID)
	opened := seriesRevisionOf(t, f.leader, series.ID)
	type answer struct {
		code  int
		body  string
		title string
	}
	answers := make(chan answer, 2)
	start := make(chan struct{})
	for _, save := range []struct {
		c     *apiClient
		title string
	}{{f.leader, "Leader's title"}, {f.admin, "Admin's title"}} {
		go func() {
			<-start
			w := save.c.do("PUT", path, map[string]any{"title": save.title, "revision": opened}, true)
			answers <- answer{w.Code, w.Body.String(), save.title}
		}()
	}
	close(start)
	var won []string
	for range 2 {
		a := <-answers
		switch {
		case a.code == http.StatusOK:
			won = append(won, a.title)
		case a.code != http.StatusConflict || !strings.Contains(a.body, `"event_changed"`):
			t.Fatalf("a save answered %d %s", a.code, a.body)
		}
	}
	var ev models.Event
	gormMust(t, f.app.DB.First(&ev, series.ID))
	if len(won) != 1 || ev.Title != won[0] {
		t.Fatalf("stored %q; saves that won: %v", ev.Title, won)
	}
}

// #445: capacity null clears the series seat limit, and the waitlist moves up
// as when the limit is raised; guestsAllowed null clears to 0.
func TestSeriesNullCapacityClearsTheLimit(t *testing.T) {
	app := testApp(t)
	admin, gid := openGroup(t, app, "Unlimited")
	eventID, date := seedCappedEvent(t, app, admin, gid, intPtr(1), true)
	path := fmt.Sprintf("/api/v1/events/%d", eventID)
	var g models.Group
	gormMust(t, app.DB.First(&g, "id = ?", gid))
	var waiter *models.User
	for _, name := range []string{"first", "second"} {
		u := newTestUser(t, app, name, "password1234", models.RoleMember)
		addMember(t, app, u, &g, models.MembershipMember)
		if code, body := registerAs(loginAPI(t, app, name, "password1234"), eventID, date, "going", nil); code != http.StatusOK {
			t.Fatalf("register: %d %s", code, body)
		}
		waiter = u
	}
	var occ models.EventOccurrence
	gormMust(t, app.DB.Where("event_id = ?", eventID).First(&occ))
	if got := regStatus(t, app, waiter.ID, occ.ID); got != models.RegWaitlisted {
		t.Fatalf("fixture: %q, want waitlisted", got)
	}

	var saved eventSeriesResponseJSON
	decodeJSON(t, admin.do("PUT", path, seriesBody(t, admin, path, map[string]any{"capacity": nil, "guestsAllowed": nil}), true), &saved)
	var ev models.Event
	gormMust(t, app.DB.First(&ev, eventID))
	if ev.Capacity != nil || saved.Event.Capacity != nil || ev.GuestsAllowed != 0 || saved.Event.GuestsAllowed != 0 {
		t.Fatalf("stored capacity %v guests %d; answered capacity %v guests %d", ev.Capacity, ev.GuestsAllowed, saved.Event.Capacity, saved.Event.GuestsAllowed)
	}
	if got := regStatus(t, app, waiter.ID, occ.ID); got != models.RegGoing {
		t.Fatalf("after clearing the limit: %q, want going", got)
	}

	// Absent leaves the stored values alone.
	apiDo(t, admin, "PUT", path, seriesBody(t, admin, path, map[string]any{"capacity": 4, "guestsAllowed": 2}), http.StatusOK)
	apiDo(t, admin, "PUT", path, seriesBody(t, admin, path, map[string]any{"title": "Still capped"}), http.StatusOK)
	gormMust(t, app.DB.First(&ev, eventID))
	if ev.Capacity == nil || *ev.Capacity != 4 || ev.GuestsAllowed != 2 {
		t.Fatalf("an edit without capacity changed it: %v, guests %d", ev.Capacity, ev.GuestsAllowed)
	}

	// On create, null and absent both mean no limit.
	var created struct {
		Event eventJSON `json:"event"`
	}
	start := time.Now().UTC().AddDate(0, 0, 40).Format("2006-01-02") + "T18:00"
	w := admin.do("POST", "/api/v1/groups/"+gid+"/events", map[string]any{
		"title": "Open night", "startsLocal": start, "durationMinutes": 60, "tzid": "UTC", "capacity": nil, "guestsAllowed": nil,
	}, true)
	if w.Code != http.StatusCreated {
		t.Fatalf("create with null: %d %s", w.Code, w.Body.String())
	}
	decodeJSON(t, w, &created)
	if created.Event.ID == 0 || created.Event.Capacity != nil || created.Event.GuestsAllowed != 0 {
		t.Fatalf("created with null: capacity %v guests %d", created.Event.Capacity, created.Event.GuestsAllowed)
	}
}
