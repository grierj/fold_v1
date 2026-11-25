package main

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"fold/internal/notify"
	"fold/models"
)

// roomChangedOf is uid's room.changed notices, oldest first.
func roomChangedOf(t *testing.T, app *App, uid uint) []models.Notification {
	t.Helper()
	var out []models.Notification
	gormMust(t, app.DB.Where("user_id = ? AND kind = ?", uid, notify.RoomChanged).Order("id").Find(&out))
	return out
}

// lastRoomChanged is uid's newest room.changed notice, failing unless uid
// has exactly want of them.
func lastRoomChanged(t *testing.T, app *App, uid uint, want int) models.Notification {
	t.Helper()
	got := roomChangedOf(t, app, uid)
	if len(got) != want {
		t.Fatalf("user %d has %d room.changed notices, want %d: %+v", uid, len(got), want, got)
	}
	if want == 0 {
		return models.Notification{}
	}
	return got[want-1]
}

// noRoomNames checks every room.changed row says nothing a room is called and
// carries no contact detail (DEC-124).
func noRoomNames(t *testing.T, app *App, names ...string) {
	t.Helper()
	var all []models.Notification
	gormMust(t, app.DB.Where("kind = ?", notify.RoomChanged).Find(&all))
	if len(all) == 0 {
		t.Fatal("no room.changed notices to check")
	}
	for _, n := range all {
		text := strings.Join([]string{n.Title, n.Body, n.EmailTitle, n.EmailBody, n.PushTitle, n.PushBody, n.Data}, "\n")
		for _, name := range names {
			if strings.Contains(text, name) {
				t.Errorf("notice %d names the room %q: %s", n.ID, name, text)
			}
		}
		if strings.Contains(text, "@") {
			t.Errorf("notice %d has an email address: %s", n.ID, text)
		}
	}
}

func noticeDates(t *testing.T, n models.Notification) []string {
	t.Helper()
	var data struct {
		Dates []string `json:"dates"`
	}
	if err := json.Unmarshal([]byte(n.Data), &data); err != nil {
		t.Fatal(err)
	}
	return data.Dates
}

// DEC-170: a co-leader's pick, change and clearing of one date's room each
// tell the other leader once, never the person who did it; a pick that leaves
// the date as it was tells no one, and neither does a refused pick.
func TestRoomChangedOneDate(t *testing.T) {
	f := newBookingFixture(t)
	co, coUser := f.coLeader(t)
	chapel := newRoom(t, f, "Chapel")
	pick := func(roomID any) {
		t.Helper()
		if code, body := f.book(t, co, f.eventA, map[string]any{"roomId": roomID}); code != http.StatusOK {
			t.Fatalf("pick %v: %d %s", roomID, code, body)
		}
	}
	day := f.startOn.Format("Jan 2")

	pick(f.hall.ID)
	n := lastRoomChanged(t, f.app, f.leaderUser.ID, 1)
	if n.Title != "Room booked: Event A" || !strings.HasPrefix(n.Body, "coleader booked a room for ") || !strings.Contains(n.Body, day) ||
		n.SubjectType != "occurrence" || n.ActorID == nil || *n.ActorID != coUser.ID || n.PushTitle != "" || n.Category != notify.CatEvents {
		t.Fatalf("the pick's notice: %+v", n)
	}
	pick(chapel.ID)
	if n := lastRoomChanged(t, f.app, f.leaderUser.ID, 2); n.Title != "Room changed: Event A" || !strings.HasPrefix(n.Body, "coleader changed the room for ") || !strings.HasSuffix(n.Body, ".") {
		t.Fatalf("the change's notice: %+v", n)
	}
	pick(chapel.ID) // the same room again: the date holds what it held
	lastRoomChanged(t, f.app, f.leaderUser.ID, 2)
	pick(nil)
	if n := lastRoomChanged(t, f.app, f.leaderUser.ID, 3); n.Title != "Room removed: Event A" ||
		!strings.HasPrefix(n.Body, "coleader took the room off ") || !strings.HasSuffix(n.Body, "That date has no room booked now.") || n.PushTitle != "" {
		t.Fatalf("the clearing's notice: %+v", n)
	}
	pick(nil) // nothing to clear
	lastRoomChanged(t, f.app, f.leaderUser.ID, 3)
	lastRoomChanged(t, f.app, coUser.ID, 0)

	// The leader's own pick tells the co-leader.
	if code, body := f.book(t, f.leader, f.eventB, map[string]any{"roomId": chapel.ID}); code != http.StatusOK {
		t.Fatalf("leader's pick: %d %s", code, body)
	}
	if n := lastRoomChanged(t, f.app, coUser.ID, 1); n.Title != "Room booked: Event B" || !strings.HasPrefix(n.Body, "leader booked") {
		t.Fatalf("the co-leader's notice: %+v", n)
	}

	// Refused picks write nothing: one from an older read, and one by a
	// leader who no longer leads.
	path := f.path(f.eventA, "/room")
	stale := roomRevision(t, co, path)
	pick(f.hall.ID)
	before := roomState(t, f.app)
	wantCode(t, "a pick from an older read", co.do("PUT", path, withRevision(map[string]any{"roomId": chapel.ID}, stale), true), http.StatusConflict, "date_room_changed")
	fresh := roomRevision(t, co, path)
	gormMust(t, f.app.DB.Exec("UPDATE memberships SET role = ? WHERE user_id = ? AND group_id = ?", models.MembershipMember, coUser.ID, f.group))
	if w := co.do("PUT", path, withRevision(map[string]any{"roomId": chapel.ID}, fresh), true); w.Code != http.StatusForbidden && w.Code != http.StatusNotFound {
		t.Fatalf("a pick by a former leader: %d %s", w.Code, w.Body.String())
	}
	if after := roomState(t, f.app); after != before {
		t.Fatalf("a refused pick wrote:\n%s\nwant\n%s", after, before)
	}
	noRoomNames(t, f.app, f.hall.Name, chapel.Name, "Hall")
}

// A sole leader's change tells no one: they are the only one who runs it.
func TestRoomChangedSoleLeaderTellsNoOne(t *testing.T) {
	f := newBookingFixture(t)
	if code, body := f.book(t, f.leader, f.eventA, map[string]any{"roomId": f.hall.ID}); code != http.StatusOK {
		t.Fatalf("pick: %d %s", code, body)
	}
	if code, body := f.book(t, f.leader, f.eventA, map[string]any{"roomId": nil}); code != http.StatusOK {
		t.Fatalf("clear: %d %s", code, body)
	}
	if n := contactCount(t, f.app, &models.Notification{}, "kind = ?", notify.RoomChanged); n != 0 {
		t.Fatalf("%d room.changed notices for a sole leader's changes", n)
	}
}

// While bookings need approval, a co-leader's pick says it waits, and so does
// a change of a waiting date; an admin who does not lead the group tells every
// leader, and a disabled leader is not told.
func TestRoomChangedWaitingAndByAnAdmin(t *testing.T) {
	f := newBookingFixture(t)
	co, coUser := f.coLeader(t)
	chapel := newRoom(t, f, "Chapel")
	approvalOn(t, f)
	off := newTestUser(t, f.app, "offleader", "password1234", models.RoleMember)
	addMember(t, f.app, off, &models.Group{ID: f.group}, models.MembershipLeader)
	gormMust(t, f.app.DB.Model(&models.User{}).Where("id = ?", off.ID).Update("status", models.UserDisabled))

	if code, body := f.book(t, co, f.eventA, map[string]any{"roomId": f.hall.ID}); code != http.StatusOK {
		t.Fatalf("pick: %d %s", code, body)
	}
	if n := lastRoomChanged(t, f.app, f.leaderUser.ID, 1); n.Title != "Room requested: Event A" ||
		!strings.HasPrefix(n.Body, "coleader asked for a room for ") || !strings.HasSuffix(n.Body, " It's waiting for an admin's approval.") {
		t.Fatalf("the request's notice: %+v", n)
	}
	if code, body := f.book(t, co, f.eventA, map[string]any{"roomId": chapel.ID}); code != http.StatusOK {
		t.Fatalf("change: %d %s", code, body)
	}
	if n := lastRoomChanged(t, f.app, f.leaderUser.ID, 2); n.Title != "Room changed: Event A" || !strings.HasSuffix(n.Body, " It's waiting for an admin's approval.") {
		t.Fatalf("the waiting change's notice: %+v", n)
	}
	noOrphanRequests(t, f.app)

	// The admin runs nothing here: both leaders are told, the disabled one not.
	if code, body := f.book(t, f.admin, f.eventA, map[string]any{"roomId": f.hall.ID}); code != http.StatusOK {
		t.Fatalf("admin's pick: %d %s", code, body)
	}
	for _, uid := range []uint{f.leaderUser.ID, coUser.ID} {
		all := roomChangedOf(t, f.app, uid)
		if n := all[len(all)-1]; n.Title != "Room changed: Event A" || !strings.HasPrefix(n.Body, "admin changed the room for ") || strings.Contains(n.Body, "waiting") {
			t.Fatalf("user %d: the admin's notice: %+v", uid, n)
		}
	}
	lastRoomChanged(t, f.app, off.ID, 0)
	noRoomNames(t, f.app, f.hall.Name, chapel.Name)
}

// A church-wide event's hosts are told when someone else changes its room.
func TestRoomChangedTellsChurchHosts(t *testing.T) {
	f := newBookingFixture(t)
	host := newTestUser(t, f.app, "host", "password1234", models.RoleMember)
	ev := churchEventAt(t, f.admin, "Church supper", f.startOn, host.Email)
	var e models.Event
	gormMust(t, f.app.DB.First(&e, ev.ID))
	runners, err := eventRunnerIDs(f.app.DB, &e)
	if err != nil || !slices.Contains(runners, host.ID) {
		t.Fatalf("runners %v %v", runners, err)
	}
	newTestUser(t, f.app, "admin2", "password1234", models.RoleAdmin)
	other := loginAPI(t, f.app, "admin2", "password1234")
	if w := roomPick(t, other, "/api/v1/events/"+itoa(ev.ID)+"/dates/"+feedDate(f.startOn)+"/room", map[string]any{"roomId": f.hall.ID}); w.Code != http.StatusOK {
		t.Fatalf("pick: %d %s", w.Code, w.Body.String())
	}
	for _, uid := range runners {
		if n := lastRoomChanged(t, f.app, uid, 1); n.Title != "Room booked: Church supper" || !strings.HasPrefix(n.Body, "admin2 booked") {
			t.Fatalf("host %d: %+v", uid, n)
		}
	}
}

// DEC-170: a co-leader's series pick tells the other leader once, with the
// pick's own counts, and names the dates left needing a room (which allows the
// push). Picking the same room again with nothing changed tells no one; a
// change, the dates named to change too, and a stop each tell once.
func TestRoomChangedSeries(t *testing.T) {
	f := newBookingFixture(t)
	co, coUser := f.coLeader(t)
	chapel, annex := newRoom(t, f, "Chapel"), newRoom(t, f, "Annex")
	// A holds the hall on the series' first date.
	if code, body := f.book(t, f.leader, f.eventA, map[string]any{"roomId": f.hall.ID}); code != http.StatusOK {
		t.Fatalf("book A: %d %s", code, body)
	}
	series := createFeedEvent(t, f.leader, f.group, "Weekly", f.startOn.Add(30*time.Minute), "FREQ=WEEKLY;COUNT=10")
	base := len(roomChangedOf(t, f.app, f.leaderUser.ID))
	seen := base
	next := func(t *testing.T) models.Notification {
		t.Helper()
		seen++
		return lastRoomChanged(t, f.app, f.leaderUser.ID, seen)
	}
	same := func(t *testing.T) {
		t.Helper()
		lastRoomChanged(t, f.app, f.leaderUser.ID, seen)
	}

	if code, out := f.seriesRoom(t, co, series, map[string]any{"roomId": f.hall.ID}); code != http.StatusOK || out.Booked != 9 || len(out.Lost) != 1 {
		t.Fatalf("the series pick: %d %+v", code, out)
	}
	n := next(t)
	if n.Title != "Series room picked: Weekly" || n.SubjectType != "event" ||
		n.Body != "coleader picked a room for the series. Booked 9 dates. 1 date still needs a room because the room is in use then. The event page lists it." ||
		n.PushTitle != "Room needed: Weekly" || n.PushBody != "1 date needs a room. See it in Fold." {
		t.Fatalf("the series pick's notice: %+v", n)
	}
	if d := noticeDates(t, n); len(d) != 1 || d[0] != feedDate(f.startOn) {
		t.Fatalf("the dates named: %v", d)
	}
	// The same room again: the busy date is lost again, nothing else moved.
	if code, again := f.seriesRoom(t, co, series, map[string]any{"roomId": f.hall.ID}); code != http.StatusOK || again.Booked != 0 || len(again.Lost) != 1 {
		t.Fatalf("the same pick again: %d %+v", code, again)
	}
	same(t)

	if code, _ := f.seriesRoom(t, co, series, map[string]any{"roomId": chapel.ID}); code != http.StatusOK {
		t.Fatalf("change: %d", code)
	}
	if n := next(t); n.Title != "Series room changed: Weekly" || n.Body != "coleader changed the series room. Booked 10 dates." || n.PushTitle != "" || len(noticeDates(t, n)) != 0 {
		t.Fatalf("the change's notice: %+v", n)
	}

	// The third date gets a room of its own (one date's notice); the series
	// room's change leaves it, and then includes it when named.
	third := feedDate(f.startOn.AddDate(0, 0, 14))
	if w := roomPick(t, co, "/api/v1/events/"+itoa(series.ID)+"/dates/"+third+"/room", map[string]any{"roomId": f.hall.ID}); w.Code != http.StatusOK {
		t.Fatalf("the third date's own room: %d %s", w.Code, w.Body.String())
	}
	if n := next(t); n.Title != "Room changed: Weekly" {
		t.Fatalf("the date's notice: %+v", n)
	}
	if code, _ := f.seriesRoom(t, co, series, map[string]any{"roomId": annex.ID}); code != http.StatusOK {
		t.Fatalf("change to the annex: %d", code)
	}
	if n := next(t); n.Body != "coleader changed the series room. Booked 9 dates. 1 date was left as it is." {
		t.Fatalf("the change that keeps a date: %+v", n)
	}
	if code, out := f.seriesRoom(t, co, series, map[string]any{"roomId": annex.ID, "includeDates": []string{third}}); code != http.StatusOK || out.Changed != 1 {
		t.Fatalf("include the third date: %d %+v", code, out)
	}
	if n := next(t); n.Title != "Series room picked: Weekly" || n.Body != "coleader picked a room for the series. Booked 1 date." {
		t.Fatalf("the included date's notice: %+v", n)
	}

	// A refused pick tells no one.
	before := roomState(t, f.app)
	wantCode(t, "a pick with no revision", co.do("PUT", "/api/v1/events/"+itoa(series.ID)+"/room", map[string]any{"roomId": chapel.ID}, true), http.StatusConflict, "event_changed")
	if after := roomState(t, f.app); after != before {
		t.Fatalf("a refused series pick wrote:\n%s\nwant\n%s", after, before)
	}

	if code, _ := f.seriesRoom(t, co, series, map[string]any{"roomId": nil}); code != http.StatusOK {
		t.Fatalf("stop: %d", code)
	}
	if n := next(t); n.Title != "Series room stopped: Weekly" || n.Body != "coleader stopped the series room. 10 dates have no room now." {
		t.Fatalf("the stop's notice: %+v", n)
	}
	if code, _ := f.seriesRoom(t, co, series, map[string]any{"roomId": nil}); code != http.StatusOK {
		t.Fatalf("stop again: %d", code)
	}
	same(t)
	lastRoomChanged(t, f.app, coUser.ID, 1) // only the leader's own booking of A
	noRoomNames(t, f.app, f.hall.Name, chapel.Name, annex.Name)
}

// An admin's Book anyway tells the leaders the dates are booked and never
// says the reason; with approval on, a co-leader's series pick says how many
// dates wait; and a new event's room tells no one.
func TestRoomChangedSeriesBookAnywayApprovalAndCreate(t *testing.T) {
	f := newBookingFixture(t)
	co, coUser := f.coLeader(t)
	if code, body := f.book(t, f.leader, f.eventA, map[string]any{"roomId": f.hall.ID}); code != http.StatusOK {
		t.Fatalf("book A: %d %s", code, body)
	}
	series := createFeedEvent(t, f.leader, f.group, "Weekly", f.startOn.Add(30*time.Minute), "FREQ=WEEKLY;COUNT=3")
	if code, out := f.seriesRoom(t, f.admin, series, map[string]any{"roomId": f.hall.ID, "overlapReason": "Shared with the youth", "reviewedBusyDates": []string{feedDate(f.startOn)}}); code != http.StatusOK || out.Booked != 3 || out.DoubleBooked != 1 {
		t.Fatalf("book anyway: %d %+v", code, out)
	}
	for _, uid := range []uint{f.leaderUser.ID, coUser.ID} {
		all := roomChangedOf(t, f.app, uid)
		if n := all[len(all)-1]; n.Body != "admin picked a room for the series. Booked 3 dates." || strings.Contains(n.Body+n.EmailBody, "youth") {
			t.Fatalf("user %d: the admin's notice: %+v", uid, n)
		}
	}

	approvalOn(t, f)
	chapel := newRoom(t, f, "Chapel")
	busy := createFeedEvent(t, f.leader, f.group, "Busy", f.startOn.AddDate(0, 0, 7).Add(30*time.Minute), "")
	if w := roomPick(t, f.admin, "/api/v1/events/"+itoa(busy.ID)+"/dates/"+feedDate(f.startOn.AddDate(0, 0, 7))+"/room", map[string]any{"roomId": chapel.ID}); w.Code != http.StatusOK {
		t.Fatalf("hold the chapel: %d %s", w.Code, w.Body.String())
	}
	// The first date's double-booking is the admin's to change: it stays.
	if code, out := f.seriesRoom(t, co, series, map[string]any{"roomId": chapel.ID}); code != http.StatusOK || out.Pending != 1 || len(out.Lost) != 1 || out.Kept != 1 {
		t.Fatalf("a waiting series pick: %d %+v", code, out)
	}
	all := roomChangedOf(t, f.app, f.leaderUser.ID)
	if n := all[len(all)-1]; n.Body != "coleader changed the series room. 1 date is waiting for an admin's approval. 1 date was left as it is. 1 date still needs a room because the room is in use then. The event page lists it." ||
		n.PushTitle != "Room needed: Weekly" {
		t.Fatalf("the waiting pick's notice: %+v", n)
	}
	noOrphanRequests(t, f.app)

	// Creating an event with a room tells no one, the co-leader included.
	coBefore := len(roomChangedOf(t, f.app, coUser.ID))
	w := f.leader.do("POST", "/api/v1/groups/"+itoa(f.group)+"/events", map[string]any{"title": "New with a room",
		"startsLocal": f.startOn.AddDate(0, 0, 2).Format("2006-01-02T15:04"), "durationMinutes": 60, "tzid": "UTC",
		"recurrenceRule": "FREQ=WEEKLY;COUNT=2", "roomId": f.hall.ID}, true)
	if w.Code != http.StatusCreated {
		t.Fatalf("create with a room: %d %s", w.Code, w.Body.String())
	}
	if got := len(roomChangedOf(t, f.app, coUser.ID)); got != coBefore {
		t.Fatalf("creating an event with a room told the co-leader: %d -> %d", coBefore, got)
	}
	noRoomNames(t, f.app, f.hall.Name, chapel.Name)
}

// DEC-170 (inventory N-34, N-35): a room pick tells the event's other leaders while
// it holds the event row. A second leader erased meanwhile is left out in
// either order, and the first leader is still told.
func TestRoomChangedNoticeSerializesWithLeaderErasure(t *testing.T) {
	t.Run("one date", func(t *testing.T) {
		runErasureBarriers(t, 7402051, func(t *testing.T) erasureScene {
			f := newBookingFixture(t)
			_, victim := f.coLeader(t)
			second := newTestUser(t, f.app, "leader3", "password1234", models.RoleMember)
			addMember(t, f.app, second, &models.Group{ID: f.group}, models.MembershipLeader)
			pick := withRevision(map[string]any{"roomId": f.hall.ID}, roomRevision(t, f.leader, f.path(f.eventA, "/room")))
			f.leader.holdNotices = true
			return erasureScene{app: f.app, victim: victim,
				run: func() error { return doExpect(f.leader, "PUT", f.path(f.eventA, "/room"), pick, http.StatusOK) },
				told: func(t *testing.T) {
					lastRoomChanged(t, f.app, second.ID, 1)
				},
			}
		})
	})
	t.Run("series", func(t *testing.T) {
		runErasureBarriers(t, 7402052, func(t *testing.T) erasureScene {
			f := newBookingFixture(t)
			_, victim := f.coLeader(t)
			second := newTestUser(t, f.app, "leader3", "password1234", models.RoleMember)
			addMember(t, f.app, second, &models.Group{ID: f.group}, models.MembershipLeader)
			series := createFeedEvent(t, f.leader, f.group, "Weekly", f.startOn.AddDate(0, 0, 1), "FREQ=WEEKLY;COUNT=3")
			path := "/api/v1/events/" + itoa(series.ID) + "/room"
			pick := withRevision(map[string]any{"roomId": f.hall.ID}, roomRevision(t, f.leader, path))
			f.leader.holdNotices = true
			return erasureScene{app: f.app, victim: victim,
				run: func() error { return doExpect(f.leader, "PUT", path, pick, http.StatusOK) },
				told: func(t *testing.T) {
					lastRoomChanged(t, f.app, second.ID, 1)
				},
			}
		})
	})
}

// DEC-170 (inventory N-34, N-35), the case the test above leaves out: the erased
// person is an admin, who holds an open "room requested" task, and a leader of
// the event's group. A leader's new pick withdraws the old request and closes
// that task, and tells the admins and the other leaders. Closing the task
// first would hold the task row while waiting for the person's user row, and
// the erasure (user row FOR UPDATE, then the notices, task included) the other
// way round; so the notices lock their recipients first (settleDatePick,
// settleRoomDecision), and both orders finish.
func TestRoomChangedNoticeSerializesWithAdminLeaderErasure(t *testing.T) {
	type scene struct {
		f      bookingFixture
		victim *models.User
		pick   func() error
		erase  func() error
	}
	date := func(f bookingFixture) string { return f.path(f.eventA, "/room") }
	series := func(f bookingFixture) string { return "/api/v1/events/" + itoa(f.eventB.ID) + "/room" }
	// approvalOff: approval is turned off before the new pick, so it opens no
	// request and tells no admin; only the room.changed notice locks the
	// victim before the old request's task closes.
	cases := []struct {
		name        string
		path        func(f bookingFixture) string
		approvalOff bool
		keyA, keyB  int64
	}{
		{name: "one date", path: date, approvalOff: true, keyA: 7402061, keyB: 7402062},
		{name: "one date, a new request", path: date, keyA: 7402063, keyB: 7402064},
		{name: "series", path: series, approvalOff: true, keyA: 7402065, keyB: 7402066},
		{name: "series, a new request", path: series, keyA: 7402067, keyB: 7402068},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setup := func(t *testing.T) scene {
				f, victim := roomRequestsFixture(t) // the victim is an admin
				addMember(t, f.app, victim, &models.Group{ID: f.group}, models.MembershipLeader)
				chapel := newRoom(t, f, "Chapel")
				deleter := loginAPI(t, f.app, "admin", "password1234")
				deleter.holdNotices = true
				path := tc.path(f)
				roomPickOK(t, f.leader, path, map[string]any{"roomId": f.hall.ID})
				if n := contactCount(t, f.app, &models.Notification{}, "user_id = ? AND kind = ? AND waiting AND resolved_at IS NULL", victim.ID, notify.RoomRequested); n != 1 {
					t.Fatalf("the victim has %d open room tasks, want 1", n)
				}
				if tc.approvalOff {
					apiDo(t, f.admin, "PUT", "/api/v1/admin/settings/room-approval", map[string]any{"enabled": false}, http.StatusOK)
				}
				pick := withRevision(map[string]any{"roomId": chapel.ID}, roomRevision(t, f.leader, path))
				return scene{f: f, victim: victim,
					pick: func() error { return doExpect(f.leader, "PUT", path, pick, http.StatusOK) },
					erase: func() error {
						return doExpect(deleter, "DELETE", "/api/v1/admin/users/"+itoa(victim.ID), nil, http.StatusOK)
					},
				}
			}
			settle := func(t *testing.T, s scene) {
				settleErasure(t, erasureScene{app: s.f.app, victim: s.victim, told: func(t *testing.T) {
					noOrphanRequests(t, s.f.app)
					want := int64(1)
					if tc.approvalOff {
						want = 0
					}
					if n := contactCount(t, s.f.app, &models.RoomRequest{}, "status = ?", models.RoomRequestPending); n != want {
						t.Fatalf("%d pending requests, want %d", n, want)
					}
				}})
			}
			t.Run("sender first", func(t *testing.T) {
				s := setup(t)
				db := s.f.app.DB
				pauseWhenTaskResolvedFor(t, db, s.victim.ID, tc.keyA)
				release, hPID := holdAdvisory(t, db, tc.keyA)
				done := startOperation(s.pick)
				opPID := waiterOn(t, db, hPID) // it told everyone and closed the victim's task; its transaction is open
				erased := startOperation(s.erase)
				delPID := waiterOn(t, db, opPID)
				if q := statementOf(t, db, delPID); !strings.Contains(q, "users") || strings.Contains(q, "notifications") {
					t.Fatalf("the delete waits in %q, want the victim's user row, not their notices", q)
				}
				release()
				if err := awaitDBResult(t, done); err != nil {
					t.Fatalf("the pick: %v", err)
				}
				if err := awaitDBResult(t, erased); err != nil {
					t.Fatalf("the delete: %v", err)
				}
				settle(t, s)
			})
			t.Run("deletion first", func(t *testing.T) {
				s := setup(t)
				db := s.f.app.DB
				pauseOnStatement(t, db, "BEFORE DELETE", "notifications", tc.keyB)
				release, hPID := holdAdvisory(t, db, tc.keyB)
				erased := startOperation(s.erase)
				deleter := waiterOn(t, db, hPID) // it holds the gate and the victim's user row
				done := startOperation(s.pick)
				opPID := waiterOn(t, db, deleter)
				q := statementOf(t, db, opPID)
				for _, part := range recipientLockText {
					if !strings.Contains(q, part) {
						t.Fatalf("the pick waits in %q, want the recipient lock (%q)", q, part)
					}
				}
				release()
				if err := awaitDBResult(t, erased); err != nil {
					t.Fatalf("the delete: %v", err)
				}
				if err := awaitDBResult(t, done); err != nil {
					t.Fatalf("a pick that lost the race must finish: %v", err)
				}
				settle(t, s)
			})
		})
	}
}

// The day is written in each recipient's own zone (DEC-58).
func TestRoomChangedDateIsInTheRecipientsZone(t *testing.T) {
	f := newBookingFixture(t)
	co, _ := f.coLeader(t)
	far, err := time.LoadLocation("Pacific/Kiritimati") // UTC+14: the next calendar day
	if err != nil {
		t.Fatal(err)
	}
	gormMust(t, f.app.DB.Model(&models.User{}).Where("id = ?", f.leaderUser.ID).Update("timezone", far.String()))
	if code, body := f.book(t, co, f.eventA, map[string]any{"roomId": f.hall.ID}); code != http.StatusOK {
		t.Fatalf("pick: %d %s", code, body)
	}
	want := "coleader booked a room for " + formatWhen(f.startOn, far) + "."
	if n := lastRoomChanged(t, f.app, f.leaderUser.ID, 1); n.Body != want || strings.Contains(n.Body, f.startOn.Format("Mon, Jan 2")) {
		t.Fatalf("body %q, want %q", n.Body, want)
	}
}

// Delivery: every room.changed row is emailed; only a row with dates left
// needing a room is pushed, and the hourly reconcile leaves the others be.
func TestRoomChangedPushesOnlyForDatesNeedingARoom(t *testing.T) {
	f := newBookingFixture(t)
	co, _ := f.coLeader(t)
	useFakePush(t, f.app)
	pushConsentRow(t, f.app, f.leaderUser, "all", models.ConsentOptedIn)
	newPushDevice(t, f.app, f.leaderUser, "phone")
	if code, body := f.book(t, f.leader, f.eventA, map[string]any{"roomId": f.hall.ID}); code != http.StatusOK {
		t.Fatalf("book A: %d %s", code, body)
	}
	co.holdNotices = true
	series := createFeedEvent(t, f.leader, f.group, "Weekly", f.startOn.Add(30*time.Minute), "FREQ=WEEKLY;COUNT=3")
	if code, body := f.book(t, co, f.eventB, map[string]any{"roomId": newRoom(t, f, "Chapel").ID}); code != http.StatusOK {
		t.Fatalf("one date: %d %s", code, body)
	}
	if code, out := f.seriesRoom(t, co, series, map[string]any{"roomId": f.hall.ID}); code != http.StatusOK || len(out.Lost) != 1 {
		t.Fatalf("series: %d %+v", code, out)
	}
	rows := roomChangedOf(t, f.app, f.leaderUser.ID)
	if len(rows) != 2 || rows[0].PushTitle != "" || rows[1].PushTitle == "" {
		t.Fatalf("rows %+v", rows)
	}
	drainNotices(t, f.app)
	for i, n := range rows {
		if d := emailDeliveryOf(t, f.app, n.ID); d.Status != models.DeliverySent {
			t.Fatalf("row %d: email %q", i, d.Status)
		}
		if got, want := len(pushJobsFor(t, f.app, n.ID)), i; got != want {
			t.Fatalf("row %d: %d push jobs, want %d", i, got, want)
		}
	}
	if err := f.app.reconcileNotifications(context.Background(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := len(pushJobsFor(t, f.app, rows[0].ID)); got != 0 {
		t.Fatalf("the reconcile queued %d pushes for a row that does not push", got)
	}
	if status, _ := pushDelivery(t, f.app, rows[0].ID); status != "" {
		t.Fatalf("a row that does not push has a push delivery %q", status)
	}
}

// Erasing the person who changed the room leaves the other leader's notice
// as it leaves an admin's room.requested task by the same person: the name in
// the text may stay (O-16), the data carries the event and its dates only.
func TestRoomChangedAfterTheActorIsErased(t *testing.T) {
	f := newBookingFixture(t)
	co, coUser := f.coLeader(t)
	approvalOn(t, f)
	if code, body := f.book(t, co, f.eventA, map[string]any{"roomId": f.hall.ID}); code != http.StatusOK {
		t.Fatalf("pick: %d %s", code, body)
	}
	apiDo(t, f.admin, "DELETE", "/api/v1/admin/users/"+itoa(coUser.ID), nil, http.StatusOK)
	changed := lastRoomChanged(t, f.app, f.leaderUser.ID, 1)
	var task models.Notification
	gormMust(t, f.app.DB.Where("kind = ? AND user_id IN (SELECT id FROM users WHERE email = ?)", notify.RoomRequested, testEmail("admin")).First(&task))
	if (changed.ActorID == nil) != (task.ActorID == nil) || (changed.ActorID != nil && *changed.ActorID != *task.ActorID) {
		t.Fatalf("actor ids differ: room.changed %v, room.requested %v", changed.ActorID, task.ActorID)
	}
	if !strings.HasPrefix(changed.Body, "coleader ") || !strings.HasPrefix(task.Title, "coleader ") {
		t.Fatalf("the names: %q / %q", changed.Body, task.Title)
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(changed.Data), &data); err != nil {
		t.Fatal(err)
	}
	if len(data) != 2 || data["eventId"] == nil || data["date"] != feedDate(f.startOn) {
		t.Fatalf("data %v", data)
	}
	all := strings.Join([]string{changed.Title, changed.Body, changed.EmailTitle, changed.EmailBody, changed.PushTitle, changed.PushBody, changed.Data}, "\n")
	if strings.Contains(all, "@") || strings.Contains(all, coUser.Email) {
		t.Fatalf("contact detail in %q", all)
	}
}
