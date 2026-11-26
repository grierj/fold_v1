package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"fold/internal/policy"
	"fold/models"

	"gorm.io/gorm"
)

type calendarPage struct {
	Occurrences []calendarOccurrenceJSON `json:"occurrences"`
	Truncated   bool                     `json:"truncated"`
}

func useCalendarTestClock(app *App, now time.Time) {
	app.clock = func() time.Time { return now.UTC() }
}

func getCalendar(t *testing.T, c *apiClient, query string, want int) (calendarPage, string) {
	t.Helper()
	w := c.do("GET", "/api/v1/calendar?"+query, nil, false)
	if w.Code != want {
		t.Fatalf("get calendar %s: %d %s, want %d", query, w.Code, w.Body.String(), want)
	}
	var page calendarPage
	if want == http.StatusOK {
		decodeJSON(t, w, &page)
	}
	return page, w.Body.String()
}

func calendarTitles(page calendarPage) []string {
	out := []string{}
	for _, o := range page.Occurrences {
		out = append(out, o.Title+"@"+o.Date)
	}
	return out
}

func hasTitle(page calendarPage, title string) bool {
	for _, o := range page.Occurrences {
		if o.Title == title {
			return true
		}
	}
	return false
}

// M1.6 acceptance: a member never sees invite-only events of groups they
// are not in, signed-out visitors see public events only, and the slim
// calendar rows carry no attendee or declined data for anyone (DEC-64).
func TestCalendarVisibilityAndSlimRows(t *testing.T) {
	app := testApp(t)
	useCalendarTestClock(app, time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC))
	newTestUser(t, app, "admin", "password1234", models.RoleAdmin)
	admin := loginAPI(t, app, "admin", "password1234")

	open := createGroupAs(t, admin, "Open Choir", models.VisibilityPublic)
	openEv := scheduleGroupEvent(t, admin, open, "Rehearsal")
	secret := createGroupAs(t, admin, "Secret Circle", models.VisibilityInviteOnly)
	scheduleGroupEvent(t, admin, secret, "Secret Night")
	membersOnly := createGroupAs(t, admin, "Members Circle", models.VisibilityPrivate)
	scheduleGroupEvent(t, admin, membersOnly, "Members Night")

	var openGroup models.Group
	if err := app.DB.First(&openGroup, open).Error; err != nil {
		t.Fatal(err)
	}
	_, member := mkUser(t, app, &openGroup, "mem")
	_, decliner := mkUser(t, app, &openGroup, "nope")
	_, goer := mkUser(t, app, &openGroup, "yes")
	regPath := fmt.Sprintf("/api/v1/events/%d/registrations", openEv.ID)
	apiDo(t, decliner, "PUT", regPath, map[string]any{"status": "declined", "date": "2026-12-01"}, http.StatusOK)
	apiDo(t, goer, "PUT", regPath, map[string]any{"status": "going", "date": "2026-12-01"}, http.StatusOK)

	const window = "from=2026-11-30&to=2026-12-02&tz=UTC"
	guest := newAPIClient(t, app)

	page, raw := getCalendar(t, member, window, http.StatusOK)
	if !hasTitle(page, "Rehearsal") {
		t.Fatalf("member should see the public event: %v", calendarTitles(page))
	}
	if hasTitle(page, "Secret Night") || strings.Contains(raw, "Secret Circle") {
		t.Fatalf("member saw an invite-only group's event: %s", raw)
	}

	page, raw = getCalendar(t, guest, window, http.StatusOK)
	if !hasTitle(page, "Rehearsal") || hasTitle(page, "Secret Night") || hasTitle(page, "Members Night") {
		t.Fatalf("signed-out calendar should hold public events only: %v", calendarTitles(page))
	}
	if strings.Contains(raw, "Secret Circle") || strings.Contains(raw, "Members Circle") {
		t.Fatalf("signed-out calendar named a hidden group: %s", raw)
	}

	// The slim rows never carry attendance, whoever asks (DEC-64): no
	// attendee names, no counts, and no declined key or value, even for
	// the admin who runs the event.
	for _, who := range []*apiClient{member, admin, guest} {
		_, raw := getCalendar(t, who, window, http.StatusOK)
		for _, banned := range []string{`"attendees"`, `"registrations"`, "declined", `"seatsUsed"`, `"capacity"`, `"name":"nope"`, `"name":"yes"`} {
			if strings.Contains(raw, banned) {
				t.Fatalf("calendar response contains %s: %s", banned, raw)
			}
		}
	}

	// Each viewer gets only their own answer. Can't go remains visible in
	// Everything so they can find and change it.
	page, _ = getCalendar(t, goer, window, http.StatusOK)
	for _, o := range page.Occurrences {
		if o.Title == "Rehearsal" && (o.MyStatus == nil || *o.MyStatus != "going") {
			t.Fatalf("goer's row should say going: %+v", o)
		}
	}
	page, _ = getCalendar(t, decliner, window, http.StatusOK)
	for _, o := range page.Occurrences {
		if o.Title == "Rehearsal" && (o.MyStatus == nil || *o.MyStatus != models.RegDeclined || !o.OwnScope) {
			t.Fatalf("decliner's Everything row should say Can't go via their own status: %+v", o)
		}
	}
	page, _ = getCalendar(t, decliner, window+"&mine=1", http.StatusOK)
	if hasTitle(page, "Rehearsal") {
		t.Fatalf("Can't go should disappear from Just mine: %+v", page.Occurrences)
	}
}

// "Just mine" is per occurrence: my groups' events, events I host, and
// the specific dates I answered going or maybe — not the whole series.
func TestCalendarMineNarrowsPerOccurrence(t *testing.T) {
	app := testApp(t)
	useCalendarTestClock(app, time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC))
	newTestUser(t, app, "admin", "password1234", models.RoleAdmin)
	admin := loginAPI(t, app, "admin", "password1234")

	mine := createGroupAs(t, admin, "My Choir", models.VisibilityPublic)
	scheduleGroupEvent(t, admin, mine, "My Rehearsal")
	other := createGroupAs(t, admin, "Other Choir", models.VisibilityPublic)
	w := admin.do("POST", "/api/v1/groups/"+itoa(other)+"/events", map[string]any{
		"title": "Other Series", "startsLocal": "2026-12-01T19:00", "durationMinutes": 60,
		"tzid": "UTC", "recurrenceRule": "FREQ=WEEKLY;BYDAY=TU;COUNT=3",
	}, true)
	if w.Code != http.StatusCreated {
		t.Fatalf("create series: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		Event eventJSON `json:"event"`
	}
	decodeJSON(t, w, &created)
	series := created.Event

	var myGroup models.Group
	if err := app.DB.First(&myGroup, mine).Error; err != nil {
		t.Fatal(err)
	}
	_, member := mkUser(t, app, &myGroup, "mem")

	// A church-wide event the member hosts.
	w = admin.do("POST", "/api/v1/events", map[string]any{
		"title": "Church Picnic", "startsLocal": "2026-12-05T17:00", "durationMinutes": 60,
		"tzid": "UTC", "hosts": []string{"mem@example.com"},
	}, true)
	if w.Code != http.StatusCreated {
		t.Fatalf("create church event: %d %s", w.Code, w.Body.String())
	}
	regPath := fmt.Sprintf("/api/v1/events/%d/registrations", series.ID)
	apiDo(t, member, "PUT", regPath, map[string]any{"status": "going", "date": "2026-12-08"}, http.StatusOK)
	apiDo(t, member, "PUT", regPath, map[string]any{"status": "maybe", "date": "2026-12-15"}, http.StatusOK)

	const window = "from=2026-12-01&to=2026-12-31&tz=UTC"
	all, _ := getCalendar(t, member, window, http.StatusOK)
	if got := calendarTitles(all); len(got) != 4 {
		t.Fatalf("everything view: the out-of-window unanswered public date should be omitted, got %v", got)
	}

	only, _ := getCalendar(t, member, window+"&mine=1", http.StatusOK)
	want := []string{"My Rehearsal@2026-12-01", "Church Picnic@2026-12-05", "Other Series@2026-12-08", "Other Series@2026-12-15"}
	if got := calendarTitles(only); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("mine view: got %v, want %v", got, want)
	}
	for _, o := range only.Occurrences {
		if o.Title == "Church Picnic" && !o.ChurchWide {
			t.Fatalf("picnic should be church-wide: %+v", o)
		}
		if o.Title == "Other Series" && o.Date == "2026-12-08" && (o.MyStatus == nil || *o.MyStatus != "going") {
			t.Fatalf("Dec 8 should say going: %+v", o)
		}
		if o.Title == "Other Series" && o.Date == "2026-12-15" && (o.MyStatus == nil || *o.MyStatus != "maybe") {
			t.Fatalf("Dec 15 should say maybe: %+v", o)
		}
	}

	// mine is ignored when signed out: Everything still applies its window.
	guest := newAPIClient(t, app)
	guestAll, _ := getCalendar(t, guest, window, http.StatusOK)
	guestMine, _ := getCalendar(t, guest, window+"&mine=1", http.StatusOK)
	if got, want := strings.Join(calendarTitles(guestMine), ","), strings.Join(calendarTitles(guestAll), ","); got != want || len(guestMine.Occurrences) != 0 {
		t.Fatalf("signed-out mine=1 changed windowed Everything: all=%v mine=%v", calendarTitles(guestAll), calendarTitles(guestMine))
	}
}

// Nothing is a signed-out visitor's own, so a public church-wide date shows
// without a "Not answered" state (DEC-99); signed in, it is in their scope.
func TestSignedOutCalendarHasNoOwnScope(t *testing.T) {
	app := testApp(t)
	admin := newTestUser(t, app, "calendar scope admin", "password1234", models.RoleAdmin)
	newTestUser(t, app, "calendar scope member", "password1234", models.RoleMember)
	ac := loginAPI(t, app, admin.Name, "password1234")
	start := feedStart(3)
	if w := ac.do("POST", "/api/v1/events", map[string]any{
		"title": "Open picnic", "startsLocal": start.Format("2006-01-02T15:04"), "durationMinutes": 60, "tzid": "UTC", "visibility": models.VisibilityPublic,
	}, true); w.Code != http.StatusCreated {
		t.Fatalf("create church event: %d %s", w.Code, w.Body.String())
	}
	query := "from=" + start.AddDate(0, 0, -1).Format("2006-01-02") + "&to=" + start.AddDate(0, 0, 2).Format("2006-01-02") + "&tz=UTC"
	ownScope := func(client *apiClient) (bool, bool) {
		t.Helper()
		page, _ := getCalendar(t, client, query, http.StatusOK)
		for _, o := range page.Occurrences {
			if o.Title == "Open picnic" {
				return true, o.OwnScope
			}
		}
		return false, false
	}
	if listed, own := ownScope(newAPIClient(t, app)); !listed || own {
		t.Fatalf("signed out: listed=%v ownScope=%v, want listed and not own", listed, own)
	}
	if listed, own := ownScope(loginAPI(t, app, "calendar scope member", "password1234")); !listed || !own {
		t.Fatalf("signed in: listed=%v ownScope=%v, want listed and own", listed, own)
	}
}

func TestAnonymousEverythingUsesThirtyDayWindow(t *testing.T) {
	app := testApp(t)
	now := time.Date(2030, 1, 15, 12, 0, 0, 0, time.UTC)
	useCalendarTestClock(app, now)
	leader := newTestUser(t, app, "calendar boundary leader", "password1234", models.RoleMember)
	viewer := newTestUser(t, app, "calendar boundary viewer", "password1234", models.RoleMember)
	client := loginAPI(t, app, viewer.Name, "password1234")
	discovery := activeGroup(t, app, leader, "Calendar discovery boundaries", models.VisibilityPublic, "")
	own := activeGroup(t, app, leader, "Calendar own boundary", models.VisibilityPublic, "")
	addMember(t, app, viewer, &own, models.MembershipMember)

	seedNamed := func(group *models.Group, title string, startsAt time.Time) (models.Event, models.EventOccurrence) {
		t.Helper()
		ev, occ := seedEvent(t, app, group, leader.ID, startsAt)
		if err := app.DB.Model(&ev).Update("title", title).Error; err != nil {
			t.Fatal(err)
		}
		ev.Title = title
		return ev, occ
	}
	seedChurch := func(title string, startsAt time.Time) {
		t.Helper()
		ev := models.Event{Title: title, Visibility: models.VisibilityPublic, StartsLocal: startsAt.UTC().Format("2006-01-02T15:04"), DurationMinutes: 60, TZID: "UTC", CreatedBy: leader.ID}
		if err := app.DB.Create(&ev).Error; err != nil {
			t.Fatal(err)
		}
		occ := models.EventOccurrence{EventID: ev.ID, OriginalDate: calDate(startsAt.UTC().Format("2006-01-02")), StartsAt: startsAt.UTC(), EndsAt: startsAt.UTC().Add(time.Hour), Status: models.OccurrenceScheduled}
		if err := app.DB.Create(&occ).Error; err != nil {
			t.Fatal(err)
		}
	}
	seedNamed(&discovery, "Past public", now.Add(-time.Microsecond))
	seedNamed(&discovery, "At public boundary", now.AddDate(0, 0, 30))
	seedNamed(&discovery, "Beyond public boundary", now.AddDate(0, 0, 30).Add(time.Microsecond))
	seedChurch("Past church-wide", now.Add(-time.Microsecond))
	seedChurch("At church-wide boundary", now.AddDate(0, 0, 30))
	seedChurch("Beyond church-wide boundary", now.AddDate(0, 0, 30).Add(time.Microsecond))
	seedNamed(&discovery, "Far public", now.AddDate(0, 0, 60))
	answered, answeredOcc := seedNamed(&discovery, "Far answered", now.AddDate(0, 0, 60).Add(time.Hour))
	seedNamed(&own, "Far own group", now.AddDate(0, 0, 60).Add(2*time.Hour))
	apiDo(t, client, "PUT", fmt.Sprintf("/api/v1/events/%d/registrations", answered.ID), map[string]any{"status": models.RegMaybe, "date": models.FormatCalendarDate(answeredOcc.OriginalDate)}, http.StatusOK)

	query := "from=2030-01-14&to=2030-03-17&tz=UTC"
	anon := newAPIClient(t, app)
	for _, suffix := range []string{"", "&mine=1"} {
		page, raw := getCalendar(t, anon, query+suffix, http.StatusOK)
		if got := calendarTitles(page); len(got) != 2 || !hasTitle(page, "At public boundary") || !hasTitle(page, "At church-wide boundary") {
			t.Fatalf("anonymous Everything%s bypassed the fixed window: got=%v raw=%s", suffix, got, raw)
		}
		for _, title := range []string{"Past public", "Beyond public boundary", "Past church-wide", "Beyond church-wide boundary", "Far public", "Far answered", "Far own group"} {
			if hasTitle(page, title) {
				t.Fatalf("anonymous Everything%s kept %q: %s", suffix, title, raw)
			}
		}
	}

	all, raw := getCalendar(t, client, query, http.StatusOK)
	for _, title := range []string{"At public boundary", "At church-wide boundary", "Far answered", "Far own group"} {
		if !hasTitle(all, title) {
			t.Fatalf("signed-in Everything lost %q exception: %s", title, raw)
		}
	}
	for _, title := range []string{"Past public", "Beyond public boundary", "Past church-wide", "Beyond church-wide boundary", "Far public"} {
		if hasTitle(all, title) {
			t.Fatalf("signed-in Everything kept %q: %s", title, raw)
		}
	}
	mine, raw := getCalendar(t, client, query+"&mine=1", http.StatusOK)
	for _, title := range []string{"At church-wide boundary", "Far answered", "Far own group"} {
		if !hasTitle(mine, title) {
			t.Fatalf("signed-in Just mine lost %q exception: %s", title, raw)
		}
	}
	if hasTitle(mine, "At public boundary") {
		t.Fatalf("unanswered outsider public date entered Just mine: %s", raw)
	}
}

func TestCalendarWindowGroupFilterAndZones(t *testing.T) {
	app := testApp(t)
	useCalendarTestClock(app, time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC))
	newTestUser(t, app, "admin", "password1234", models.RoleAdmin)
	admin := loginAPI(t, app, "admin", "password1234")
	open := createGroupAs(t, admin, "Open Choir", models.VisibilityPublic)
	scheduleGroupEvent(t, admin, open, "Rehearsal") // 2026-12-01T19:00 UTC
	secret := createGroupAs(t, admin, "Secret Circle", models.VisibilityInviteOnly)

	guest := newAPIClient(t, app)

	// A window over 366 days is a 400, a bad zone is a 400, and a group
	// the viewer can't see is a 404.
	getCalendar(t, guest, "from=2026-01-01&to=2027-01-03&tz=UTC", http.StatusBadRequest)
	getCalendar(t, guest, "from=2026-12-01&to=2026-12-02&tz=Mars/Olympus", http.StatusBadRequest)
	getCalendar(t, guest, fmt.Sprintf("from=2026-12-01&to=2026-12-02&tz=UTC&group=%d", secret), http.StatusNotFound)

	// The group filter narrows to that group's events.
	page, _ := getCalendar(t, guest, fmt.Sprintf("from=2026-12-01&to=2026-12-02&tz=UTC&group=%d", open), http.StatusOK)
	if len(page.Occurrences) != 1 || page.Occurrences[0].GroupName != "Open Choir" {
		t.Fatalf("group filter: %+v", page.Occurrences)
	}

	// The window follows the viewer's zone: 19:00 UTC on Dec 1 is Dec 2
	// in Kiritimati (UTC+14), so it lands in a Dec 2 window there and
	// not in a Dec 2 UTC window.
	page, _ = getCalendar(t, guest, "from=2026-12-02&to=2026-12-02&tz=Pacific/Kiritimati", http.StatusOK)
	if !hasTitle(page, "Rehearsal") {
		t.Fatalf("Kiritimati Dec 2 should include the event: %v", calendarTitles(page))
	}
	page, _ = getCalendar(t, guest, "from=2026-12-02&to=2026-12-02&tz=UTC", http.StatusOK)
	if hasTitle(page, "Rehearsal") {
		t.Fatalf("UTC Dec 2 should not include the event: %v", calendarTitles(page))
	}
}

func TestCalendarTruncatesAt500(t *testing.T) {
	app := testApp(t)
	useCalendarTestClock(app, time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC))
	leader := newTestUser(t, app, "lead", "password1234", models.RoleMember)
	grp := activeGroup(t, app, leader, "Busy Choir", models.VisibilityPublic, "")
	start := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < maxCalendarOccurrences+1; i++ {
		seedEvent(t, app, &grp, leader.ID, start.Add(time.Duration(i)*time.Minute))
	}
	guest := newAPIClient(t, app)
	page, _ := getCalendar(t, guest, "from=2026-12-01&to=2026-12-01&tz=UTC", http.StatusOK)
	if len(page.Occurrences) != maxCalendarOccurrences || !page.Truncated {
		t.Fatalf("want %d rows and truncated, got %d rows truncated=%v",
			maxCalendarOccurrences, len(page.Occurrences), page.Truncated)
	}
}

func TestCalendarChurchWideFilterKeepsVisibility(t *testing.T) {
	app := testApp(t)
	useCalendarTestClock(app, time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC))
	adminUser := newTestUser(t, app, "admin", "password1234", models.RoleAdmin)
	admin := loginAPI(t, app, "admin", "password1234")
	group := activeGroup(t, app, adminUser, "Busy group", models.VisibilityPublic, "")
	start := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i <= maxCalendarOccurrences; i++ {
		seedEvent(t, app, &group, adminUser.ID, start.Add(time.Duration(i)*time.Minute))
	}
	for _, visibility := range []string{"public", "private"} {
		w := admin.do("POST", "/api/v1/events", map[string]any{"title": visibility + " church event", "startsLocal": "2026-12-01T19:00", "durationMinutes": 60, "tzid": "UTC", "visibility": visibility}, true)
		if w.Code != http.StatusCreated {
			t.Fatalf("church event: %d %s", w.Code, w.Body.String())
		}
	}
	query := "from=2026-12-01&to=2026-12-01&tz=UTC&church=1"
	visitor, _ := getCalendar(t, newAPIClient(t, app), query, http.StatusOK)
	if len(visitor.Occurrences) != 1 || visitor.Occurrences[0].Title != "public church event" || visitor.Truncated {
		t.Fatalf("visitor church filter: %+v", visitor)
	}
	staff, _ := getCalendar(t, admin, query, http.StatusOK)
	if len(staff.Occurrences) != 2 || staff.Truncated {
		t.Fatalf("staff church filter: %+v", staff)
	}
}

// DEC-68: a deleted group's events are hidden from everyone by default;
// an admin's "show deleted events" switch (deleted=1) lists them, marked
// groupDeleted, and the switch does nothing for anyone else.
func TestCalendarShowDeletedIsAdminOnly(t *testing.T) {
	app := testApp(t)
	newTestUser(t, app, "admin", "password1234", models.RoleAdmin)
	admin := loginAPI(t, app, "admin", "password1234")
	newTestUser(t, app, "mem", "password1234", models.RoleMember)
	mem := loginAPI(t, app, "mem", "password1234")
	gid := createGroupAs(t, admin, "Old Choir", models.VisibilityPublic)
	scheduleGroupEvent(t, admin, gid, "Last Rehearsal") // 2026-12-01T19:00 UTC
	if err := app.DB.Delete(&models.Group{}, gid).Error; err != nil {
		t.Fatal(err)
	}
	q := "from=2026-12-01&to=2026-12-02&tz=UTC"
	if page, _ := getCalendar(t, admin, q, http.StatusOK); hasTitle(page, "Last Rehearsal") {
		t.Fatalf("admin sees a deleted group's event by default: %v", calendarTitles(page))
	}
	page, _ := getCalendar(t, admin, q+"&deleted=1", http.StatusOK)
	if len(page.Occurrences) != 1 || !page.Occurrences[0].GroupDeleted {
		t.Fatalf("admin with deleted=1: %+v", page.Occurrences)
	}
	for _, c := range []*apiClient{mem, newAPIClient(t, app)} {
		if page, body := getCalendar(t, c, q+"&deleted=1", http.StatusOK); hasTitle(page, "Last Rehearsal") || strings.Contains(body, "groupDeleted") {
			t.Fatalf("non-admin got a deleted group's event: %s", body)
		}
	}
}

// remote=1 keeps only events that offer remote attendance (M1.8
// slice 2), for signed-in and signed-out viewers alike, and never
// widens visibility: a hidden group's remote event stays hidden.
func TestCalendarRemoteFilter(t *testing.T) {
	app := testApp(t)
	useCalendarTestClock(app, time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC))
	newTestUser(t, app, "admin", "password1234", models.RoleAdmin)
	admin := loginAPI(t, app, "admin", "password1234")

	open := createGroupAs(t, admin, "Open Choir", models.VisibilityPublic)
	scheduleGroupEvent(t, admin, open, "In Person Only")
	w := admin.do("POST", "/api/v1/groups/"+itoa(open)+"/events", map[string]any{
		"title": "Hybrid Night", "startsLocal": "2026-12-01T19:00", "durationMinutes": 60,
		"tzid": "UTC", "remoteAvailable": true,
	}, true)
	if w.Code != http.StatusCreated {
		t.Fatalf("create remote event: %d %s", w.Code, w.Body.String())
	}
	var hybrid struct {
		Event eventJSON `json:"event"`
	}
	decodeJSON(t, w, &hybrid)
	closed := scheduleGroupEvent(t, admin, open, "Remote Closed This Date")
	apiDo(t, admin, "PUT", fmt.Sprintf("/api/v1/events/%d", closed.ID), seriesBody(t, admin, fmt.Sprintf("/api/v1/events/%d", closed.ID), map[string]any{"remoteAvailable": true}), http.StatusOK)
	saveDate(t, admin, closed.ID, closed.StartsLocal[:10], map[string]any{"remoteCapacityOverride": 0}, http.StatusOK, "")
	secret := createGroupAs(t, admin, "Secret Remote", models.VisibilityInviteOnly)
	w = admin.do("POST", "/api/v1/groups/"+itoa(secret)+"/events", map[string]any{
		"title": "Secret Hybrid", "startsLocal": "2026-12-01T19:00", "durationMinutes": 60,
		"tzid": "UTC", "remoteAvailable": true,
	}, true)
	if w.Code != http.StatusCreated {
		t.Fatalf("create secret remote event: %d %s", w.Code, w.Body.String())
	}

	const window = "from=2026-11-30&to=2026-12-02&tz=UTC"
	for _, who := range []*apiClient{admin, newAPIClient(t, app)} {
		page, raw := getCalendar(t, who, window+"&remote=1", http.StatusOK)
		if hasTitle(page, "In Person Only") {
			t.Fatalf("remote=1 kept an in-person-only event: %v", calendarTitles(page))
		}
		if !hasTitle(page, "Hybrid Night") {
			t.Fatalf("remote=1 dropped the remote event: %v", calendarTitles(page))
		}
		if hasTitle(page, "Remote Closed This Date") {
			t.Fatalf("remote filter kept a date with zero remote seats: %s", raw)
		}
		for _, row := range page.Occurrences {
			if !row.RemoteAvailable {
				t.Fatalf("remote filter row missing availability: %+v", row)
			}
		}
		if who != admin && (hasTitle(page, "Secret Hybrid") || strings.Contains(raw, "Secret Remote")) {
			t.Fatalf("signed-out remote=1 saw an invite-only event: %s", raw)
		}
	}
	// Without the filter both public events are there.
	page, _ := getCalendar(t, newAPIClient(t, app), window, http.StatusOK)
	if !hasTitle(page, "In Person Only") || !hasTitle(page, "Hybrid Night") {
		t.Fatalf("unfiltered calendar: %v", calendarTitles(page))
	}
	for _, row := range page.Occurrences {
		if row.RemoteAvailable != (row.EventID == hybrid.Event.ID) {
			t.Fatalf("unfiltered calendar remote availability: %+v", row)
		}
	}
}

func TestCalendarChildcareFilter(t *testing.T) {
	app := testApp(t)
	useCalendarTestClock(app, time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC))
	newTestUser(t, app, "admin", "password1234", models.RoleAdmin)
	admin := loginAPI(t, app, "admin", "password1234")
	open := createGroupAs(t, admin, "Open Nursery", models.VisibilityPublic)
	plain := scheduleGroupEvent(t, admin, open, "No Childcare")
	offered := scheduleGroupEvent(t, admin, open, "Family Night")
	apiDo(t, admin, "PUT", "/api/v1/events/"+itoa(offered.ID), seriesBody(t, admin, "/api/v1/events/"+itoa(offered.ID), map[string]any{"childcareAvailable": true}), http.StatusOK)
	secret := createGroupAs(t, admin, "Secret Nursery", models.VisibilityInviteOnly)
	hidden := scheduleGroupEvent(t, admin, secret, "Hidden Family Night")
	apiDo(t, admin, "PUT", "/api/v1/events/"+itoa(hidden.ID), seriesBody(t, admin, "/api/v1/events/"+itoa(hidden.ID), map[string]any{"childcareAvailable": true}), http.StatusOK)
	const window = "from=2026-11-30&to=2026-12-02&tz=UTC"
	for _, who := range []*apiClient{admin, newAPIClient(t, app)} {
		page, raw := getCalendar(t, who, window+"&childcare=1", http.StatusOK)
		if hasTitle(page, plain.Title) || !hasTitle(page, offered.Title) {
			t.Fatalf("childcare filter: %s", raw)
		}
		for _, row := range page.Occurrences {
			if !row.ChildcareAvailable {
				t.Fatalf("missing childcare flag: %+v", row)
			}
		}
		if who != admin && (hasTitle(page, hidden.Title) || strings.Contains(raw, "Secret Nursery")) {
			t.Fatalf("filter widened visibility: %s", raw)
		}
	}
	page, _ := getCalendar(t, newAPIClient(t, app), window, http.StatusOK)
	if !hasTitle(page, plain.Title) || !hasTitle(page, offered.Title) {
		t.Fatalf("unfiltered: %v", calendarTitles(page))
	}
	for _, row := range page.Occurrences {
		if row.ChildcareAvailable != (row.EventID == offered.ID) {
			t.Fatalf("wrong childcare flag: %+v", row)
		}
	}
}

func TestHouseholdOnlyCalendarDoesNotOverwriteOwnStatus(t *testing.T) {
	app := testApp(t)
	admin, gid := openGroup(t, app, "HouseholdCalendar")
	eventID, date := seedCappedEvent(t, app, admin, gid, nil, true)
	_, c, spouse, _ := householdFamily(t, app, admin, "calendarholder")
	batchAnswer(t, c, eventID, date, map[string]any{"people": []map[string]any{{"personId": spouse.ID, "status": "going"}}}, http.StatusOK)
	page, raw := getCalendar(t, c, "from="+date+"&to="+date+"&tz=UTC&mine=1", http.StatusOK)
	if len(page.Occurrences) != 1 || page.Occurrences[0].EventID != eventID || page.Occurrences[0].MyStatus != nil || !page.Occurrences[0].OwnScope {
		t.Fatalf("household-only calendar %s", raw)
	}
	for _, secret := range []string{"Mary", "householdPerson", "careNotes", "birthDate"} {
		if strings.Contains(raw, secret) {
			t.Fatalf("calendar leak %s", raw)
		}
	}
	if code, body := registerAs(c, eventID, date, models.RegDeclined, nil); code != http.StatusOK {
		t.Fatalf("own Can't go: %d %s", code, body)
	}
	window := "from=" + date + "&to=" + date + "&tz=UTC"
	page, raw = getCalendar(t, c, window+"&mine=1", http.StatusOK)
	if len(page.Occurrences) != 0 {
		t.Fatalf("household Going overrode own Can't go in Just mine: %s", raw)
	}
	page, raw = getCalendar(t, c, window, http.StatusOK)
	if len(page.Occurrences) != 1 || page.Occurrences[0].MyStatus == nil || *page.Occurrences[0].MyStatus != models.RegDeclined || !page.Occurrences[0].OwnScope {
		t.Fatalf("Everything lost own Can't go: %s", raw)
	}
	for _, secret := range []string{"Mary", "householdPerson", "careNotes", "birthDate"} {
		if strings.Contains(raw, secret) {
			t.Fatalf("declined calendar leaked household data: %s", raw)
		}
	}
	var going int64
	if err := app.DB.Model(&models.Registration{}).Where("household_person_id = ? AND status = ?", spouse.ID, models.RegGoing).Count(&going).Error; err != nil || going != 1 {
		t.Fatalf("own Can't go changed household attendance: %d Going, err=%v", going, err)
	}
}

func TestCalendarScopedInvitationIsOneSlimGoingDate(t *testing.T) {
	f := newOccurrenceReadFixture(t)
	seedReadInvitation(t, f.app, f.issuer, f.other, f.event, f.dates[2])
	window := "from=" + models.FormatCalendarDate(f.dates[0].OriginalDate) + "&to=" + models.FormatCalendarDate(f.dates[2].OriginalDate) + "&tz=UTC"
	for _, mine := range []string{"", "&mine=1"} {
		page, raw := getCalendar(t, f.client, window+mine, 200)
		if len(page.Occurrences) != 1 {
			t.Fatalf("calendar broadened: %s", raw)
		}
		row := page.Occurrences[0]
		if row.Date != models.FormatCalendarDate(f.dates[1].OriginalDate) || row.DateURL != occurrenceDatePath(f.event.ID, row.Date) || row.MyStatus == nil || *row.MyStatus != "going" || row.Title != "Invited date title" || row.Location != "Invited date place" {
			t.Fatalf("wrong invited row: %+v", row)
		}
		for _, forbidden := range []string{"Hidden group", "Hidden series", `"groupId"`, `"groupName"`, `"hostedBy"`, `"attendees"`, `"registrations"`, `"capacity"`, `"seatsUsed"`, `"recurrenceRule"`, f.issuer.Email} {
			if strings.Contains(raw, forbidden) {
				t.Fatalf("calendar leaked %q: %s", forbidden, raw)
			}
		}
	}
	other, _ := getCalendar(t, f.stranger, window+"&mine=1", 200)
	if len(other.Occurrences) != 1 || other.Occurrences[0].Date != models.FormatCalendarDate(f.dates[2].OriginalDate) {
		t.Fatalf("distinct recipient scope: %+v", other)
	}
	getCalendar(t, f.client, window+"&group="+itoa(f.group), 404)
	getCalendar(t, f.client, window+"&remote=1", 200)
	// A visible group filter cannot be widened by an unrelated invitation.
	public := createFeedGroup(t, f.admin, "Other visible group", models.VisibilityPublic, "")
	page, _ := getCalendar(t, f.client, window+"&group="+itoa(public), 200)
	if len(page.Occurrences) != 0 {
		t.Fatal("invitation escaped group filter", page)
	}
	// There is no implicit hidden-group location fallback.
	if err := f.app.DB.Model(&f.dates[1]).Update("location_override", nil).Error; err != nil {
		t.Fatal(err)
	}
	page, _ = getCalendar(t, f.client, window, 200)
	if len(page.Occurrences) != 1 || page.Occurrences[0].Location != "" {
		t.Fatal("hidden fallback", page)
	}
}

func TestCalendarOwnScopeStatusAndThirtyDayWindow(t *testing.T) {
	app := testApp(t)
	newTestUser(t, app, "admin", "password1234", models.RoleAdmin)
	admin := loginAPI(t, app, "admin", "password1234")
	now := time.Now().UTC()
	near := time.Date(now.Year(), now.Month(), now.Day(), 19, 0, 0, 0, time.UTC).AddDate(0, 0, 7)
	far := near.AddDate(0, 0, 24) // 31 days from today.

	mineID := createFeedGroup(t, admin, "Scope members", models.VisibilityPublic, "")
	var mineGroup models.Group
	if err := app.DB.First(&mineGroup, mineID).Error; err != nil {
		t.Fatal(err)
	}
	_, member := mkUser(t, app, &mineGroup, "scopeviewer")
	createFeedEvent(t, admin, mineID, "Own far date", far, "")

	otherID := createFeedGroup(t, admin, "Scope discovery", models.VisibilityPublic, "")
	createFeedEvent(t, admin, otherID, "Near public unanswered", near, "")
	createFeedEvent(t, admin, otherID, "Far public unanswered", far, "")
	maybe := createFeedEvent(t, admin, otherID, "Far public maybe", far.Add(time.Hour), "")
	declined := createFeedEvent(t, admin, otherID, "Near public declined", near.Add(time.Hour), "")
	apiDo(t, member, "PUT", fmt.Sprintf("/api/v1/events/%d/registrations", maybe.ID), map[string]any{"status": "maybe", "date": feedDate(far)}, http.StatusOK)
	apiDo(t, member, "PUT", fmt.Sprintf("/api/v1/events/%d/registrations", declined.ID), map[string]any{"status": "declined", "date": feedDate(near)}, http.StatusOK)

	church := func(title string, start time.Time) {
		w := admin.do("POST", "/api/v1/events", map[string]any{"title": title, "startsLocal": start.Format("2006-01-02T15:04"), "durationMinutes": 60, "tzid": "UTC"}, true)
		if w.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", title, w.Code, w.Body.String())
		}
	}
	church("Near church-wide", near.Add(2*time.Hour))
	church("Far church-wide", far.Add(2*time.Hour))

	query := "from=" + now.Format("2006-01-02") + "&to=" + far.AddDate(0, 0, 1).Format("2006-01-02") + "&tz=UTC"
	all, raw := getCalendar(t, member, query, http.StatusOK)
	for _, title := range []string{"Own far date", "Near public unanswered", "Far public maybe", "Near public declined", "Near church-wide"} {
		if !hasTitle(all, title) {
			t.Fatalf("Everything missing %q: %s", title, raw)
		}
	}
	for _, title := range []string{"Far public unanswered", "Far church-wide"} {
		if hasTitle(all, title) {
			t.Fatalf("Everything kept out-of-window %q: %s", title, raw)
		}
	}
	mine, raw := getCalendar(t, member, query+"&mine=1", http.StatusOK)
	for _, title := range []string{"Own far date", "Far public maybe", "Near church-wide"} {
		if !hasTitle(mine, title) {
			t.Fatalf("Just mine missing %q: %s", title, raw)
		}
	}
	for _, title := range []string{"Near public unanswered", "Far public unanswered", "Near public declined", "Far church-wide"} {
		if hasTitle(mine, title) {
			t.Fatalf("Just mine kept %q: %s", title, raw)
		}
	}
	for _, row := range all.Occurrences {
		if row.Title == "Near public unanswered" && row.OwnScope {
			t.Fatalf("outsider discovery row marked own: %+v", row)
		}
		if row.Title == "Own far date" && (!row.OwnScope || row.MyStatus != nil) {
			t.Fatalf("own unanswered row: %+v", row)
		}
		if row.Title == "Far public maybe" && (!row.OwnScope || row.MyStatus == nil || *row.MyStatus != models.RegMaybe) {
			t.Fatalf("maybe row: %+v", row)
		}
	}
}

func TestCalendarScopedInvitationLoadingIsBatched(t *testing.T) {
	f := newOccurrenceReadFixture(t)
	window := "from=" + models.FormatCalendarDate(f.dates[0].OriginalDate) + "&to=" + models.FormatCalendarDate(f.dates[2].OriginalDate) + "&tz=UTC"
	counter := countDBReads(t, f.app.DB)
	getCalendar(t, f.client, window, 200)
	counter.Stop()
	one := counter.Reads()
	seedReadInvitation(t, f.app, f.issuer, f.recipient, f.event, f.dates[0])
	seedReadInvitation(t, f.app, f.issuer, f.recipient, f.event, f.dates[2])
	counter = countDBReads(t, f.app.DB)
	page, _ := getCalendar(t, f.client, window, 200)
	counter.Stop()
	three := counter.Reads()
	if len(page.Occurrences) != 3 || three != one {
		t.Fatalf("invitation reads grew per date: one=%d three=%d dates=%d", one, three, len(page.Occurrences))
	}
	t.Logf("calendar reads with 1 or 3 scoped dates: %d", one)
}

func TestCalendarHiddenGroupSiteNameReadBudget(t *testing.T) {
	app := testApp(t)
	creator := newTestUser(t, app, "creator", "pw", models.RoleAdmin)
	g := newTestGroup(t, app, creator, "Hidden", models.GroupActive)
	if err := app.DB.Model(g).Update("visibility", models.VisibilityInviteOnly).Error; err != nil {
		t.Fatal(err)
	}
	if err := app.DB.Create(&models.Setting{Key: "site_name", Value: "Our Church"}).Error; err != nil {
		t.Fatal(err)
	}
	client := newAPIClient(t, app)
	client.do("GET", "/api/v1/calendar", nil, false) // Warm the setup-complete cache.
	settings := countSiteNameReads(t, app.DB)
	start := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Minute)
	var baseline int64
	for i := 1; i <= 30; i++ {
		ev, _ := seedEvent(t, app, g, creator.ID, start)
		if err := app.DB.Model(&ev).Update("visibility", models.VisibilityPublic).Error; err != nil {
			t.Fatal(err)
		}
		if i != 3 && i != 30 {
			continue
		}
		*settings = 0
		counter := countDBReads(t, app.DB)
		w := client.do("GET", "/api/v1/calendar?tz=UTC&from="+start.Format("2006-01-02")+"&to="+start.Format("2006-01-02"), nil, false)
		reads := counter.Reads()
		counter.Stop()
		if w.Code != http.StatusOK {
			t.Fatalf("calendar: %d %s", w.Code, w.Body.String())
		}
		var body struct {
			Occurrences []calendarOccurrenceJSON `json:"occurrences"`
		}
		decodeJSON(t, w, &body)
		if len(body.Occurrences) != i {
			t.Fatalf("got %d dates, want %d", len(body.Occurrences), i)
		}
		for _, row := range body.Occurrences {
			if row.HostedBy != "Our Church" || row.GroupID != nil || row.GroupName != "" {
				t.Fatalf("hidden group host: %+v", row)
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

// TestCalendarGroupFilterIsJudgedInTheListsSnapshot: the group filter says
// which events are a group's, so it is judged in the snapshot that lists them.
// The group is made invite-only and one of its dates retitled in one commit,
// right after the filter is judged and again at the "grants" seam: the list
// must come from the state the filter was judged in, never pair the old
// authorization with the new title.
func TestCalendarGroupFilterIsJudgedInTheListsSnapshot(t *testing.T) {
	for _, seam := range readerSeams {
		t.Run(seam, func(t *testing.T) {
			f := newDepartureReadFixture(t, models.VisibilityPublic, models.RegGoing, policy.OccurrenceOriginDepartureSelf)
			const renamed = "Renamed under a hidden group"
			fired := atReaderSeam(f.app, seam, "calendar group", func() {
				if err := f.app.DB.Transaction(func(tx *gorm.DB) error {
					if err := tx.Model(&models.Group{}).Where("id = ?", f.group).Update("visibility", models.VisibilityInviteOnly).Error; err != nil {
						return err
					}
					return tx.Model(&models.EventOccurrence{}).Where("id = ?", f.dates[0].ID).Update("title_override", renamed).Error
				}); err != nil {
					t.Error(err)
				}
			})
			from, to := feedDate(f.dates[0].StartsAt.AddDate(0, 0, -1)), feedDate(f.dates[2].StartsAt.AddDate(0, 0, 1))
			w := f.client.do("GET", "/api/v1/calendar?tz=UTC&group="+itoa(f.group)+"&from="+from+"&to="+to, nil, false)
			if !fired.Load() {
				t.Fatalf("the calendar never reached the %s seam", seam)
			}
			if w.Code != http.StatusOK || strings.Contains(w.Body.String(), renamed) {
				t.Fatalf("the group filter paired its old authorization with a newer date: %d %s", w.Code, w.Body.String())
			}
			if w := f.client.do("GET", "/api/v1/calendar?tz=UTC&group="+itoa(f.group)+"&from="+from+"&to="+to, nil, false); w.Code != http.StatusNotFound {
				t.Fatalf("after the commit the hidden group's filter: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

// keptCalendar is a public group with a leader who is no admin, an admin who is
// neither a member nor a host, a member who leaves keeping dates, an ordinary
// member who stays, and a second member who can leave later.
type keptCalendar struct {
	app                                   *App
	group                                 uint
	issuer, leader, admin, member, other  *apiClient
	second                                *apiClient
	adminU, memberU, secondU, issuerU, lU *models.User
}

func newKeptCalendar(t *testing.T) keptCalendar {
	t.Helper()
	app := testApp(t)
	f := keptCalendar{app: app}
	f.issuerU = newTestUser(t, app, "kept issuer", "password1234", models.RoleAdmin)
	f.issuer = loginAPI(t, app, "kept issuer", "password1234")
	f.group = createFeedGroup(t, f.issuer, "Kept calendar group", models.VisibilityPublic, "Kept hall")
	var g models.Group
	gormMust(t, app.DB.First(&g, f.group))
	f.lU = newTestUser(t, app, "kept leader", "password1234", models.RoleMember)
	addMember(t, app, f.lU, &g, models.MembershipLeader)
	f.leader = loginAPI(t, app, "kept leader", "password1234")
	f.adminU = newTestUser(t, app, "kept admin", "password1234", models.RoleAdmin)
	f.admin = loginAPI(t, app, "kept admin", "password1234")
	f.memberU = newTestUser(t, app, "kept member", "password1234", models.RoleMember)
	addMember(t, app, f.memberU, &g, models.MembershipMember)
	f.member = loginAPI(t, app, "kept member", "password1234")
	otherU := newTestUser(t, app, "kept other", "password1234", models.RoleMember)
	addMember(t, app, otherU, &g, models.MembershipMember)
	f.other = loginAPI(t, app, "kept other", "password1234")
	f.secondU = newTestUser(t, app, "kept second", "password1234", models.RoleMember)
	addMember(t, app, f.secondU, &g, models.MembershipMember)
	f.second = loginAPI(t, app, "kept second", "password1234")
	return f
}

// event creates a weekly public event of n dates far from every fixture date,
// signs who up Going on all of them, and returns its dates in order. A test
// then places each date with place and has who leave keeping some.
func (f keptCalendar) event(t *testing.T, title string, n int, who ...*apiClient) (eventJSON, []models.EventOccurrence) {
	t.Helper()
	ev := createFeedEvent(t, f.issuer, f.group, title, feedStart(300), fmt.Sprintf("FREQ=WEEKLY;COUNT=%d", n))
	var dates []models.EventOccurrence
	gormMust(t, f.app.DB.Where("event_id = ?", ev.ID).Order("starts_at").Find(&dates))
	if len(dates) != n {
		t.Fatalf("%s has %d dates, want %d", title, len(dates), n)
	}
	for _, c := range who {
		for _, d := range dates {
			if code, body := registerAs(c, ev.ID, models.FormatCalendarDate(d.OriginalDate), models.RegGoing, nil); code != http.StatusOK {
				t.Fatalf("going on %s: %d %s", models.FormatCalendarDate(d.OriginalDate), code, body)
			}
		}
	}
	return ev, dates
}

// place moves a date (no grant is live yet, so 00014 allows it): its original
// day, its start, its length and a title of its own.
func (f keptCalendar) place(t *testing.T, d *models.EventOccurrence, title, day string, start time.Time, minutes int) {
	t.Helper()
	gormMust(t, f.app.DB.Exec("UPDATE event_occurrences SET original_date = ?, starts_at = ?, ends_at = ?, duration_override = ?, title_override = ? WHERE id = ?",
		day, start.UTC(), start.Add(time.Duration(minutes)*time.Minute).UTC(), minutes, title, d.ID))
	gormMust(t, f.app.DB.First(d, d.ID))
}

// leave has c leave the group through the real departure, keeping exactly keep.
func (f keptCalendar) leave(t *testing.T, c *apiClient, keep ...models.EventOccurrence) {
	t.Helper()
	base := "/api/v1/groups/" + itoa(f.group)
	preview := getDeparturePreview(t, c, base+"/departure-preview", http.StatusOK)
	ids := []uint{}
	for _, d := range keep {
		ids = append(ids, d.ID)
	}
	if w := confirmDeparturePreview(t, c, "POST", base+"/leave", preview, ids, ""); w.Code != http.StatusOK {
		t.Fatalf("leave keeping %v: %d %s", ids, w.Code, w.Body.String())
	}
	for _, d := range keep {
		if n, err := keptDatesIn(f.app.DB, []uint{d.ID}); err != nil || n[d.ID] == 0 {
			t.Fatalf("date %d is not kept after leaving: %v %v", d.ID, n, err)
		}
	}
}

// remove is a manager's roster Remove of u's row on d.
func (f keptCalendar) remove(t *testing.T, eventID uint, d models.EventOccurrence, u *models.User) {
	t.Helper()
	var reg models.Registration
	gormMust(t, f.app.DB.Where("occurrence_id = ? AND user_id = ?", d.ID, u.ID).First(&reg))
	if w := f.issuer.do("POST", "/api/v1/events/"+itoa(eventID)+"/registrations/remove",
		map[string]any{"date": models.FormatCalendarDate(d.OriginalDate), "registrationId": reg.ID}, true); w.Code != http.StatusOK {
		t.Fatalf("remove: %d %s", w.Code, w.Body.String())
	}
}

func calendarRowFor(page calendarPage, title string) (calendarOccurrenceJSON, bool) {
	for _, o := range page.Occurrences {
		if o.Title == title {
			return o, true
		}
	}
	return calendarOccurrenceJSON{}, false
}

func calendarQuery(from, to time.Time, extra string) string {
	return "tz=UTC&from=" + feedDate(from) + "&to=" + feedDate(to) + extra
}

// TestManagerCalendarLinkMatchesTheEventPageList (DEC-157): for the group's
// leader and for an admin who is neither member nor host, a calendar row links
// to the date page exactly when the event page does not list that date, at
// every boundary of the event page's filter; eventPageLists agrees with that
// filter. The admin keeps every kept date (ManagerKept) and no unkept public
// date outside DEC-77's window. A member's and a visitor's rows carry no link.
func TestManagerCalendarLinkMatchesTheEventPageList(t *testing.T) {
	f := newKeptCalendar(t)
	now := time.Now().UTC().Truncate(time.Minute)
	ev, d := f.event(t, "Boundary series", 7, f.member)
	day := func(t time.Time) string { return t.Format("2006-01-02") }
	noon := func(days int) time.Time {
		return time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC).AddDate(0, 0, days)
	}
	started, soon, last, past, custom, late, cancelled := &d[0], &d[1], &d[2], &d[3], &d[4], &d[5], &d[6]
	f.place(t, started, "Started ten minutes ago", day(now), now.Add(-10*time.Minute), 60)
	// One date per original day: this one's clock was moved to an hour from now.
	f.place(t, soon, "Starts in an hour", day(now.AddDate(0, 0, 1)), now.Add(time.Hour), 60)
	f.place(t, last, "Last listed day", day(noon(90)), noon(90), 60)
	f.place(t, past, "First unlisted day", day(noon(91)), noon(91), 60)
	f.place(t, custom, "Customized far date", day(noon(121)), noon(121), 60)
	f.place(t, late, "Yesterday's date later today", day(now.Add(-24*time.Hour)), now.Add(2*time.Hour), 60)
	f.place(t, cancelled, "Cancelled boundary date", day(noon(30)), noon(30), 60)
	f.leave(t, f.member, *started, *past, *custom)
	gormMust(t, f.app.DB.Exec("UPDATE event_occurrences SET status = ? WHERE id = ?", models.OccurrenceCancelled, cancelled.ID))
	if n, _ := keptDatesIn(f.app.DB, []uint{cancelled.ID}); n[cancelled.ID] != 0 {
		t.Fatal("the cancelled date holds a grant")
	}

	all := []*models.EventOccurrence{started, soon, last, past, custom, late, cancelled}
	q := calendarQuery(now.Add(-24*time.Hour), noon(125), "")
	from, to := upcomingWindow(now)
	useCalendarTestClock(f.app, now)
	for _, who := range []struct {
		name string
		c    *apiClient
		u    *models.User
	}{{"leader", f.leader, f.lU}, {"admin", f.admin, f.adminU}} {
		t.Run(who.name, func(t *testing.T) {
			listed, err := f.app.occurrencesForEventsErr([]uint{ev.ID}, from, to, &now, who.u)
			if err != nil {
				t.Fatal(err)
			}
			onList := map[string]bool{}
			for _, o := range listed {
				onList[o.Date] = true
			}
			for _, o := range all {
				gormMust(t, f.app.DB.First(o, o.ID))
				if got, want := eventPageLists(o, now), onList[models.FormatCalendarDate(o.OriginalDate)]; got != want {
					t.Errorf("eventPageLists(%s) = %v, the event page lists it: %v", derefTitle(o), got, want)
				}
			}
			page, _ := getCalendar(t, who.c, q, http.StatusOK)
			for _, row := range page.Occurrences {
				wantURL := ""
				if !onList[row.Date] {
					wantURL = occurrenceDatePath(ev.ID, row.Date)
				}
				if row.DateURL != wantURL {
					t.Errorf("%s: dateUrl %q, want %q", row.Title, row.DateURL, wantURL)
				}
			}
			for _, title := range []string{"Started ten minutes ago", "First unlisted day", "Customized far date"} {
				if row, ok := calendarRowFor(page, title); !ok || row.DateURL == "" {
					t.Errorf("%s: no linked row for a kept unlisted date: %+v", title, page.Occurrences)
				}
			}
			if hasTitle(page, "Cancelled boundary date") {
				t.Error("the cancelled date is on the calendar")
			}
			if who.name == "admin" {
				// DEC-77 still bounds the admin's unkept public dates.
				if hasTitle(page, "Last listed day") {
					t.Error("the admin sees an unkept public date 90 days ahead")
				}
				for _, title := range []string{"Starts in an hour", "Yesterday's date later today"} {
					if !hasTitle(page, title) {
						t.Errorf("the admin lost %s inside the window", title)
					}
				}
			} else if !hasTitle(page, "Last listed day") {
				t.Error("the leader lost a standing-scope date")
			}
		})
	}
	t.Run("member and visitor rows carry no link", func(t *testing.T) {
		for name, c := range map[string]*apiClient{"member": f.other, "visitor": newAPIClient(t, f.app)} {
			page, raw := getCalendar(t, c, q, http.StatusOK)
			if len(page.Occurrences) == 0 || strings.Contains(raw, `"dateUrl"`) {
				t.Errorf("%s rows: %s", name, raw)
			}
		}
	})

	// The HTTP control on the real clock, with margins no request can cross:
	// the calendar against GET /events/:id.
	f.app.clock = nil
	wall := time.Now().UTC().Truncate(time.Minute)
	ev2, d2 := f.event(t, "Wide margin series", 5, f.second)
	wideNoon := func(days int) time.Time {
		return time.Date(wall.Year(), wall.Month(), wall.Day(), 12, 0, 0, 0, time.UTC).AddDate(0, 0, days)
	}
	f.place(t, &d2[0], "Wide started", day(wall.Add(-time.Hour)), wall.Add(-time.Hour), 180)
	f.place(t, &d2[1], "Wide soon", day(wall.AddDate(0, 0, 1)), wall.Add(2*time.Hour), 60)
	f.place(t, &d2[2], "Wide 80", day(wideNoon(80)), wideNoon(80), 60)
	f.place(t, &d2[3], "Wide 100", day(wideNoon(100)), wideNoon(100), 60)
	f.place(t, &d2[4], "Wide 121", day(wideNoon(121)), wideNoon(121), 60)
	f.leave(t, f.second, d2[0], d2[3], d2[4])
	for _, c := range []*apiClient{f.leader, f.admin} {
		w := c.do("GET", "/api/v1/events/"+itoa(ev2.ID), nil, false)
		if w.Code != http.StatusOK {
			t.Fatal(w.Code, w.Body.String())
		}
		var body struct {
			Occurrences []occurrenceJSON `json:"occurrences"`
		}
		decodeJSON(t, w, &body)
		onList := map[string]bool{}
		for _, o := range body.Occurrences {
			onList[o.Date] = true
		}
		page, _ := getCalendar(t, c, calendarQuery(wall.Add(-24*time.Hour), wideNoon(125), ""), http.StatusOK)
		seen := 0
		for _, row := range page.Occurrences {
			if row.EventID != ev2.ID {
				continue
			}
			seen++
			if (row.DateURL != "") == onList[row.Date] {
				t.Errorf("%s: dateUrl %q, the event page lists it: %v", row.Title, row.DateURL, onList[row.Date])
			}
		}
		if seen < 4 {
			t.Errorf("wide-margin rows: %d, %+v", seen, page.Occurrences)
		}
	}
}

func derefTitle(o *models.EventOccurrence) string {
	if o.TitleOverride == nil {
		return models.FormatCalendarDate(o.OriginalDate)
	}
	return *o.TitleOverride
}

// TestManagersReachKeptDatesOnTheirCalendar (DEC-157) through the real
// calendar handler: in Everything, a date someone kept after leaving stays on
// the calendar of everyone who manages its event, outside DEC-77's window and
// while underway, carried in when it began before the range; never in Just
// mine, Home's request or the feeds, never for a member, and only while the
// grant is live and the date has not ended on the calendar's clock.
func TestManagersReachKeptDatesOnTheirCalendar(t *testing.T) {
	day := func(t time.Time) string { return t.Format("2006-01-02") }
	midnight := func(t time.Time) time.Time { return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC) }

	t.Run("everything, just mine, home and feeds", func(t *testing.T) {
		f := newKeptCalendar(t)
		now := time.Now().UTC().Truncate(time.Minute)
		today := midnight(now)
		ev, d := f.event(t, "Kept reach series", 4, f.member)
		f.place(t, &d[0], "Underway kept", day(now), now.Add(-30*time.Minute), 120)
		f.place(t, &d[1], "Far kept", day(today.AddDate(0, 0, 121)), today.AddDate(0, 0, 121).Add(12*time.Hour), 60)
		f.place(t, &d[2], "Far sibling", day(today.AddDate(0, 0, 114)), today.AddDate(0, 0, 114).Add(12*time.Hour), 60)
		yesterdayNoon := today.AddDate(0, 0, -1).Add(12 * time.Hour)
		f.place(t, &d[3], "Began yesterday kept", day(yesterdayNoon), yesterdayNoon, 2*24*60)
		_, d2 := f.event(t, "Unkept twin series", 1, f.member)
		f.place(t, &d2[0], "Began yesterday unkept", day(yesterdayNoon), yesterdayNoon, 2*24*60)
		f.leave(t, f.member, d[0], d[1], d[3])
		// Pinned after the departure, so the grants it made are live on this clock.
		useCalendarTestClock(f.app, time.Now())

		everything := calendarQuery(today, today.AddDate(0, 0, 29), "")
		farRange := calendarQuery(today.AddDate(0, 0, 110), today.AddDate(0, 0, 125), "")
		home := calendarQuery(today, today.AddDate(0, 0, 60), "&mine=1")
		farMine := calendarQuery(today.AddDate(0, 0, 110), today.AddDate(0, 0, 125), "&mine=1")
		wantLinked := func(t *testing.T, page calendarPage, title string, eventID uint) {
			t.Helper()
			row, ok := calendarRowFor(page, title)
			if !ok || row.DateURL != occurrenceDatePath(eventID, row.Date) {
				t.Fatalf("%s: no row linking its date page: %+v", title, page.Occurrences)
			}
		}
		absent := func(t *testing.T, page calendarPage, titles ...string) {
			t.Helper()
			for _, title := range titles {
				if hasTitle(page, title) {
					t.Fatalf("%s is on the calendar: %v", title, calendarTitles(page))
				}
			}
		}

		// (a) An underway public kept date, for an admin who is no member.
		page, _ := getCalendar(t, f.admin, everything, http.StatusOK)
		wantLinked(t, page, "Underway kept", ev.ID)
		// (b) 121 days ahead; DEC-77 still drops its unkept sibling.
		page, _ = getCalendar(t, f.admin, farRange, http.StatusOK)
		wantLinked(t, page, "Far kept", ev.ID)
		absent(t, page, "Far sibling")
		// (e) Began yesterday and still going: carried into the leader's today.
		page, _ = getCalendar(t, f.leader, everything, http.StatusOK)
		wantLinked(t, page, "Began yesterday kept", ev.ID)
		if row, _ := calendarRowFor(page, "Began yesterday kept"); row.Date != day(yesterdayNoon) {
			t.Fatalf("the carried row is not under its own start day: %+v", row)
		}
		// (f) The same shape without a kept grant is not carried.
		absent(t, page, "Began yesterday unkept")
		// (g) A member who is no leader gets no carried row.
		page, _ = getCalendar(t, f.other, everything, http.StatusOK)
		absent(t, page, "Began yesterday kept", "Began yesterday unkept")

		// (d) and (h): Just mine and Home's request shape have none of them.
		mineBefore := map[string]string{}
		for name, r := range map[string]struct {
			c *apiClient
			q string
		}{"admin home": {f.admin, home}, "admin far mine": {f.admin, farMine}, "leader home": {f.leader, home}} {
			page, raw := getCalendar(t, r.c, r.q, http.StatusOK)
			if r.c == f.leader {
				// The leader's own dates stay; only the carry-in is never mine.
				absent(t, page, "Began yesterday kept")
			} else {
				absent(t, page, "Underway kept", "Far kept", "Began yesterday kept")
			}
			mineBefore[name] = raw
		}
		// (k) The personal feeds, both scopes.
		anon := newAPIClient(t, f.app)
		feedClients := map[string]*apiClient{"admin": f.admin, "leader": f.leader}
		paths := map[string]string{}
		// Both scopes of one link: a person has one feed link at a time.
		feedsOf := func(t *testing.T) map[string]string {
			t.Helper()
			out := map[string]string{}
			for who, c := range feedClients {
				for _, scope := range []string{models.CalendarFeedMine, models.CalendarFeedAll} {
					if paths[who] == "" {
						paths[who] = personalFeedPath(t, c, scope)
					} else if w := c.do("PUT", "/api/v1/me/calendar-feed", map[string]any{"scope": scope}, true); w.Code != http.StatusOK {
						t.Fatalf("feed scope %s: %d %s", scope, w.Code, w.Body.String())
					}
					out[who+" "+scope] = feedBody(t, anon, paths[who])
				}
			}
			return out
		}
		feeds := feedsOf(t)
		for name, body := range feeds {
			if strings.HasPrefix(name, "admin") && (strings.Contains(body, "Far kept") || strings.Contains(body, "Underway kept")) {
				t.Fatalf("the %s feed keeps a kept date outside DEC-77's window:\n%s", name, body)
			}
		}

		// (i) After Remove the grant has ended: back to DEC-77 and no carry.
		f.remove(t, ev.ID, d[0], f.memberU)
		f.remove(t, ev.ID, d[1], f.memberU)
		f.remove(t, ev.ID, d[3], f.memberU)
		page, _ = getCalendar(t, f.admin, everything, http.StatusOK)
		absent(t, page, "Underway kept", "Began yesterday kept")
		page, _ = getCalendar(t, f.admin, farRange, http.StatusOK)
		absent(t, page, "Far kept", "Far sibling")
		page, _ = getCalendar(t, f.leader, everything, http.StatusOK)
		absent(t, page, "Began yesterday kept")
		for name, r := range map[string]struct {
			c *apiClient
			q string
		}{"admin home": {f.admin, home}, "admin far mine": {f.admin, farMine}, "leader home": {f.leader, home}} {
			if _, raw := getCalendar(t, r.c, r.q, http.StatusOK); raw != mineBefore[name] {
				t.Fatalf("%s changed with the grants:\nbefore %s\nafter  %s", name, mineBefore[name], raw)
			}
		}
		for name, got := range feedsOf(t) {
			if got != feeds[name] {
				t.Fatalf("the %s feed changed with the grants:\nbefore %s\nafter  %s", name, feeds[name], got)
			}
		}
	})

	t.Run("an ended date leaves on the calendar's clock", func(t *testing.T) {
		f := newKeptCalendar(t)
		wall := time.Now().UTC().Truncate(time.Minute)
		ev, d := f.event(t, "Ending series", 1, f.member)
		f.place(t, &d[0], "Ends in two hours", day(wall), wall.Add(time.Hour), 60)
		f.leave(t, f.member, d[0])
		q := calendarQuery(midnight(wall).AddDate(0, 0, -1), midnight(wall).AddDate(0, 0, 2), "")
		clock := newMutableClock(f.app, d[0].EndsAt.Add(time.Minute))
		page, _ := getCalendar(t, f.admin, q, http.StatusOK)
		if hasTitle(page, "Ends in two hours") {
			t.Fatalf("an ended kept date stayed on the calendar: %v", calendarTitles(page))
		}
		clock.set(d[0].EndsAt.Add(-time.Minute))
		page, _ = getCalendar(t, f.admin, q, http.StatusOK)
		if row, ok := calendarRowFor(page, "Ends in two hours"); !ok || row.DateURL != occurrenceDatePath(ev.ID, row.Date) {
			t.Fatalf("an underway kept date is missing before its end: %v", calendarTitles(page))
		}
	})

	t.Run("isolation", func(t *testing.T) {
		f := newKeptCalendar(t)
		now := time.Now().UTC().Truncate(time.Minute)
		today := midnight(now)
		_, d := f.event(t, "Isolated series", 2, f.member, f.second)
		f.place(t, &d[0], "Isolated kept", day(today.AddDate(0, 0, 60)), today.AddDate(0, 0, 60).Add(12*time.Hour), 60)
		f.place(t, &d[1], "Isolated newly kept", day(today.AddDate(0, 0, 67)), today.AddDate(0, 0, 67).Add(12*time.Hour), 60)
		f.leave(t, f.member, d[0])
		q := calendarQuery(today.AddDate(0, 0, 55), today.AddDate(0, 0, 70), "")
		fired := atReaderSeam(f.app, "authorized", "calendar kept dates", func() {
			gormMust(t, f.app.DB.Model(&models.User{}).Where("id = ?", f.adminU.ID).Update("role", models.RoleMember))
			f.leave(t, f.second, d[1])
		})
		page, raw := getCalendar(t, f.admin, q, http.StatusOK)
		if !fired.Load() {
			t.Fatal("the calendar never reached its kept-dates seam")
		}
		if !hasTitle(page, "Isolated kept") || hasTitle(page, "Isolated newly kept") {
			t.Fatalf("the in-flight answer is not the state its authority was judged in: %s", raw)
		}
		page, _ = getCalendar(t, f.admin, q, http.StatusOK)
		if hasTitle(page, "Isolated kept") || hasTitle(page, "Isolated newly kept") {
			t.Fatalf("after the demotion: %v", calendarTitles(page))
		}
	})

	// carried runs N3m's and N3n's clock cases on one kept date that starts two
	// hours before windowStart and lasts six, the range's only possible row.
	carried := func(t *testing.T, windowStart time.Time, to time.Time) {
		f := newKeptCalendar(t)
		ev, d := f.event(t, "Carried series", 1, f.member)
		start := windowStart.Add(-2 * time.Hour)
		if !start.After(time.Now().Add(24 * time.Hour)) {
			t.Fatalf("the fixture date starts too soon: %s", start)
		}
		f.place(t, &d[0], "Carried across the edge", day(start), start, 360)
		f.leave(t, f.member, d[0])
		q := calendarQuery(windowStart, to, "")
		clock := newMutableClock(f.app, time.Now())
		for _, at := range []time.Time{time.Now(), start.Add(-time.Minute)} {
			clock.set(at)
			if page, raw := getCalendar(t, f.leader, q, http.StatusOK); len(page.Occurrences) != 0 {
				t.Fatalf("at %s, before the date starts, it was carried: %s", at, raw)
			}
		}
		clock.set(start.Add(time.Minute))
		page, raw := getCalendar(t, f.leader, q, http.StatusOK)
		if len(page.Occurrences) != 1 || page.Occurrences[0].Date != day(start) || page.Occurrences[0].DateURL != occurrenceDatePath(ev.ID, day(start)) {
			t.Fatalf("an underway kept date was not carried under its start day: %s", raw)
		}
		f.remove(t, ev.ID, d[0], f.memberU)
		if page, raw := getCalendar(t, f.leader, q, http.StatusOK); len(page.Occurrences) != 0 {
			t.Fatalf("carried after Remove: %s", raw)
		}
	}
	t.Run("future week", func(t *testing.T) {
		ws := midnight(time.Now().UTC()).AddDate(0, 0, 3)
		carried(t, ws, ws.AddDate(0, 0, 6))
	})
	t.Run("future month", func(t *testing.T) {
		// G: the first grid day (the Sunday on or before the 1st) of the first
		// month whose grid starts at least two days after the wall clock.
		wall := time.Now().UTC()
		first := time.Date(wall.Year(), wall.Month(), 1, 0, 0, 0, 0, time.UTC)
		var g time.Time
		for {
			g = first.AddDate(0, 0, -int(first.Weekday()))
			if !g.Before(wall.Add(48 * time.Hour)) {
				break
			}
			first = first.AddDate(0, 1, 0)
		}
		carried(t, g, g.AddDate(0, 0, 41))
	})
}
