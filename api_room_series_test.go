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
	"fold/internal/eventdates"
	"fold/internal/notify"
	"fold/models"
)

func (f bookingFixture) seriesRoom(t *testing.T, c *apiClient, ev eventJSON, body map[string]any) (int, seriesBookingJSON) {
	t.Helper()
	w := roomPick(t, c, "/api/v1/events/"+itoa(ev.ID)+"/room", body)
	var out seriesBookingJSON
	if w.Code == http.StatusOK {
		decodeJSON(t, w, &out)
	}
	return w.Code, out
}

// seriesBookings counts an event's bookings by status.
func seriesBookings(t *testing.T, app *App, eventID uint, status string) int64 {
	t.Helper()
	return contactCount(t, app, &models.RoomBooking{},
		"status = ? AND occurrence_id IN (SELECT id FROM event_occurrences WHERE event_id = ?)", status, eventID)
}

func TestRoomSeriesBooksFreeDatesAndFlagsBusyOnes(t *testing.T) {
	f := newBookingFixture(t)
	// A holds the hall on the first date, padded [18:30, 20:00).
	if code, body := f.book(t, f.leader, f.eventA, map[string]any{"roomId": f.hall.ID}); code != http.StatusOK {
		t.Fatalf("book A: %d %s", code, body)
	}
	// A weekly series of 10 at 19:30: the hall is busy on its first date only.
	series := createFeedEvent(t, f.leader, f.group, "Weekly", f.startOn.Add(30*time.Minute), "FREQ=WEEKLY;COUNT=10")
	chapel := func() roomJSON {
		var out struct {
			Room roomJSON `json:"room"`
		}
		decodeJSON(t, f.admin.do("POST", "/api/v1/admin/rooms", map[string]any{"name": "Chapel"}, true), &out)
		return out.Room
	}()
	// The third date gets its own room first; the series room leaves it.
	third := feedDate(f.startOn.AddDate(0, 0, 14))
	if w := roomPick(t, f.leader, "/api/v1/events/"+itoa(series.ID)+"/dates/"+third+"/room", map[string]any{"roomId": chapel.ID}); w.Code != http.StatusOK {
		t.Fatalf("book the chapel on one date: %d %s", w.Code, w.Body.String())
	}

	var picker struct {
		Rooms       []seriesRoomJSON    `json:"rooms"`
		Dates       int                 `json:"dates"`
		DefaultRoom *occurrenceRoomJSON `json:"defaultRoom"`
		NeedsRoom   []needsRoomJSON     `json:"needsRoom"`
	}
	decodeJSON(t, f.leader.do("GET", "/api/v1/events/"+itoa(series.ID)+"/rooms", nil, false), &picker)
	busy := map[uint]int{}
	for _, r := range picker.Rooms {
		busy[r.ID] = r.BusyDates
		if r.ID == f.nursery.ID {
			t.Fatal("a leader's series picker lists the restricted nursery")
		}
	}
	if picker.Dates != 10 || busy[f.hall.ID] != 1 || busy[chapel.ID] != 0 || picker.DefaultRoom != nil {
		t.Fatalf("series picker: %+v", picker)
	}

	code, out := f.seriesRoom(t, f.leader, series, map[string]any{"roomId": f.hall.ID})
	if code != http.StatusOK || out.Booked != 8 || out.Kept != 1 || len(out.Lost) != 1 || out.Lost[0] != feedDate(f.startOn) ||
		out.DefaultRoom == nil || out.DefaultRoom.ID != f.hall.ID {
		t.Fatalf("book the series: %d %+v", code, out)
	}
	if n := seriesBookings(t, f.app, series.ID, models.BookingConfirmed); n != 9 { // 8 in the hall, 1 in the chapel
		t.Fatalf("%d confirmed bookings", n)
	}
	if contactCount(t, f.app, &models.AuditLog{}, "action = ? AND target_id = ?", audit.RoomBookingSeries, series.ID) != 1 {
		t.Fatal("booking a series room is audited")
	}
	decodeJSON(t, f.leader.do("GET", "/api/v1/events/"+itoa(series.ID)+"/rooms", nil, false), &picker)
	if len(picker.NeedsRoom) != 1 || picker.NeedsRoom[0].Date != feedDate(f.startOn) || picker.NeedsRoom[0].RoomName != "Fellowship Hall" ||
		picker.DefaultRoom == nil || picker.DefaultRoom.ID != f.hall.ID {
		t.Fatalf("needs a room: %+v", picker)
	}

	// Booking it again changes nothing.
	if code, again := f.seriesRoom(t, f.leader, series, map[string]any{"roomId": f.hall.ID}); code != http.StatusOK || again.Booked != 0 || len(again.Lost) != 1 {
		t.Fatalf("book the series again: %d %+v", code, again)
	}

	// Editing the series time moves every booking.
	later := f.startOn.Add(90 * time.Minute)
	if w := f.leader.do("PUT", "/api/v1/events/"+itoa(series.ID), seriesBody(t, f.leader, "/api/v1/events/"+itoa(series.ID), map[string]any{"startsLocal": later.Format("2006-01-02T15:04")}), true); w.Code != http.StatusOK {
		t.Fatalf("series edit: %d %s", w.Code, w.Body.String())
	}
	if n := contactCount(t, f.app, &models.RoomBooking{},
		"status = ? AND occurrence_id IN (SELECT id FROM event_occurrences WHERE event_id = ? AND starts_at = room_bookings.starts_at) AND extract(hour FROM starts_at AT TIME ZONE 'UTC') = 20",
		models.BookingConfirmed, series.ID); n != 9 {
		t.Fatalf("%d bookings moved to 20:30", n)
	}

	// Stopping the series room frees its bookings and keeps the chapel.
	if code, stop := f.seriesRoom(t, f.leader, series, map[string]any{"roomId": nil}); code != http.StatusOK || stop.DefaultRoom != nil {
		t.Fatalf("stop the series room: %d %+v", code, stop)
	}
	if n := contactCount(t, f.app, &models.RoomBooking{}, "occurrence_id IN (SELECT id FROM event_occurrences WHERE event_id = ?)", series.ID); n != 1 {
		t.Fatalf("%d bookings left; only the chapel should stay", n)
	}
}

func TestRoomSeriesGuards(t *testing.T) {
	f := newBookingFixture(t)
	series := createFeedEvent(t, f.leader, f.group, "Weekly", f.startOn, "FREQ=WEEKLY;COUNT=3")
	if code, _ := f.seriesRoom(t, f.leader, series, map[string]any{"roomId": f.nursery.ID}); code != http.StatusForbidden {
		t.Fatalf("leader booking the nursery for a series: %d", code)
	}
	newTestUser(t, f.app, "outsider", "password1234", models.RoleMember)
	outsider := loginAPI(t, f.app, "outsider", "password1234")
	if w := outsider.do("GET", "/api/v1/events/"+itoa(series.ID)+"/rooms", nil, false); w.Code != http.StatusForbidden && w.Code != http.StatusNotFound {
		t.Fatalf("outsider listing series rooms: %d", w.Code)
	}
	if code, _ := f.seriesRoom(t, outsider, series, map[string]any{"roomId": f.hall.ID}); code != http.StatusForbidden && code != http.StatusNotFound {
		t.Fatalf("outsider booking a series room: %d", code)
	}
	if w := outsider.do("POST", "/api/v1/rooms/check", map[string]any{"groupId": f.group, "startsLocal": f.startOn.Format("2006-01-02T15:04"), "durationMinutes": 60}, true); w.Code != http.StatusForbidden && w.Code != http.StatusNotFound {
		t.Fatalf("outsider checking rooms for a group: %d", w.Code)
	}
	if w := outsider.do("POST", "/api/v1/rooms/check", map[string]any{"startsLocal": f.startOn.Format("2006-01-02T15:04"), "durationMinutes": 60}, true); w.Code != http.StatusForbidden {
		t.Fatalf("non-admin checking rooms for a church-wide event: %d", w.Code)
	}

	// An admin's reason double-books the busy dates instead.
	if code, body := f.book(t, f.leader, f.eventA, map[string]any{"roomId": f.hall.ID}); code != http.StatusOK {
		t.Fatalf("book A: %d %s", code, body)
	}
	code, out := f.seriesRoom(t, f.admin, series, map[string]any{"roomId": f.hall.ID, "overlapReason": "Shared with A", "reviewedBusyDates": []string{feedDate(f.startOn)}})
	if code != http.StatusOK || out.Booked != 3 || len(out.Lost) != 0 {
		t.Fatalf("admin double-booking the series: %d %+v", code, out)
	}
	// A leader can't stop a series room holding an admin's double-booking
	// on that date; the rest are released.
	if code, _ := f.seriesRoom(t, f.leader, series, map[string]any{"roomId": nil}); code != http.StatusOK {
		t.Fatalf("leader stopping the series room: %d", code)
	}
	if n := contactCount(t, f.app, &models.RoomBooking{}, "allow_overlap AND occurrence_id IN (SELECT id FROM event_occurrences WHERE event_id = ?)", series.ID); n != 1 {
		t.Fatalf("the admin's double-booking should stay: %d", n)
	}
}

func TestRoomSeriesOnCreate(t *testing.T) {
	f := newBookingFixture(t)
	start := f.startOn.AddDate(0, 0, 1)
	var check struct {
		Rooms []seriesRoomJSON `json:"rooms"`
		Dates int              `json:"dates"`
	}
	decodeJSON(t, f.leader.do("POST", "/api/v1/rooms/check", map[string]any{"groupId": f.group,
		"startsLocal": start.Format("2006-01-02T15:04"), "durationMinutes": 60, "tzid": "UTC", "recurrenceRule": "FREQ=WEEKLY;COUNT=3", "capacity": 30}, true), &check)
	if check.Dates != 3 || len(check.Rooms) != 1 || check.Rooms[0].BusyDates != 0 || !check.Rooms[0].TooSmall {
		t.Fatalf("rooms check: %+v", check)
	}

	create := func(title string, roomID uint) int {
		return f.leader.do("POST", "/api/v1/groups/"+itoa(f.group)+"/events", map[string]any{"title": title,
			"startsLocal": start.Format("2006-01-02T15:04"), "durationMinutes": 60, "tzid": "UTC",
			"recurrenceRule": "FREQ=WEEKLY;COUNT=3", "roomId": roomID}, true).Code
	}
	if code := create("Booked", f.hall.ID); code != http.StatusCreated {
		t.Fatalf("create with a room: %d", code)
	}
	var ev models.Event
	f.app.DB.Where("title = ?", "Booked").First(&ev)
	if ev.DefaultRoomID == nil || *ev.DefaultRoomID != f.hall.ID || seriesBookings(t, f.app, ev.ID, models.BookingConfirmed) != 3 {
		t.Fatalf("created series should book its 3 dates: %+v", ev)
	}
	if code := create("Nursery night", f.nursery.ID); code != http.StatusForbidden {
		t.Fatalf("leader creating with the nursery: %d", code)
	}
	if contactCount(t, f.app, &models.Event{}, "title = ?", "Nursery night") != 0 {
		t.Fatal("a refused room must not leave the event behind")
	}

	// The room is the place in calendars: "Fellowship Hall" leads LOCATION.
	w := f.leader.do("GET", "/api/v1/events/"+itoa(ev.ID)+"/dates/"+feedDate(start)+"/calendar.ics", nil, false)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "LOCATION:Fellowship Hall") {
		t.Fatalf("calendar LOCATION: %d %s", w.Code, w.Body.String())
	}
}

func TestRoomSeriesExtenderBooksNewDates(t *testing.T) {
	f := newBookingFixture(t)
	series := createFeedEvent(t, f.leader, f.group, "Weekly", f.startOn.AddDate(0, 0, 1), "FREQ=WEEKLY;COUNT=4")
	if code, out := f.seriesRoom(t, f.leader, series, map[string]any{"roomId": f.hall.ID}); code != http.StatusOK || out.Booked != 4 {
		t.Fatalf("book the series: %d %+v", code, out)
	}
	// The last two dates go missing (as if not yet generated), and another
	// event takes the hall on the last one.
	third, fourth := feedDate(f.startOn.AddDate(0, 0, 15)), feedDate(f.startOn.AddDate(0, 0, 22))
	if err := f.app.DB.Where("event_id = ? AND original_date IN ?", series.ID, []string{third, fourth}).Delete(&models.EventOccurrence{}).Error; err != nil {
		t.Fatal(err)
	}
	other := createFeedEvent(t, f.leader, f.group, "Other", f.startOn.AddDate(0, 0, 22), "")
	if w := roomPick(t, f.leader, "/api/v1/events/"+itoa(other.ID)+"/dates/"+fourth+"/room", map[string]any{"roomId": f.hall.ID}); w.Code != http.StatusOK {
		t.Fatalf("book the other event: %d %s", w.Code, w.Body.String())
	}

	if err := eventdates.Extend(f.app.DB, time.Now().UTC(), f.app.afterExtend()); err != nil {
		t.Fatal(err)
	}
	if n := seriesBookings(t, f.app, series.ID, models.BookingConfirmed); n != 3 {
		t.Fatalf("%d confirmed after the extender; want 3", n)
	}
	if n := seriesBookings(t, f.app, series.ID, models.BookingRequested); n != 1 {
		t.Fatalf("%d requested after the extender; want 1", n)
	}
	var n models.Notification
	if err := f.app.DB.Where("user_id = ? AND kind = ?", f.leaderUser.ID, notify.RoomLost).First(&n).Error; err != nil {
		t.Fatalf("the leader should hear the new date lost its room: %v", err)
	}
	// Running it again books nothing twice.
	if err := eventdates.Extend(f.app.DB, time.Now().UTC(), f.app.afterExtend()); err != nil {
		t.Fatal(err)
	}
	if n := contactCount(t, f.app, &models.RoomBooking{}, "occurrence_id IN (SELECT id FROM event_occurrences WHERE event_id = ?)", series.ID); n != 4 {
		t.Fatalf("%d bookings after a second run", n)
	}
}

func TestRoomSeriesEditsBookAddedAndRestoredDates(t *testing.T) {
	f := newBookingFixture(t)
	start := f.startOn.AddDate(0, 0, 1)
	series := createFeedEvent(t, f.leader, f.group, "Weekly", start, "FREQ=WEEKLY;COUNT=3")
	if code, out := f.seriesRoom(t, f.leader, series, map[string]any{"roomId": f.hall.ID}); code != http.StatusOK || out.Booked != 3 {
		t.Fatalf("book the series: %d %+v", code, out)
	}
	edit := func(rule string) {
		t.Helper()
		if w := f.leader.do("PUT", "/api/v1/events/"+itoa(series.ID), seriesBody(t, f.leader, "/api/v1/events/"+itoa(series.ID), map[string]any{"recurrenceRule": rule}), true); w.Code != http.StatusOK {
			t.Fatalf("series edit %s: %d %s", rule, w.Code, w.Body.String())
		}
	}
	// Shortening cancels and frees the third date; another event takes the
	// hall then.
	edit("FREQ=WEEKLY;COUNT=2")
	third := start.AddDate(0, 0, 14)
	other := createFeedEvent(t, f.leader, f.group, "Other", third, "")
	if w := roomPick(t, f.leader, "/api/v1/events/"+itoa(other.ID)+"/dates/"+feedDate(third)+"/room", map[string]any{"roomId": f.hall.ID}); w.Code != http.StatusOK {
		t.Fatalf("book the other event: %d %s", w.Code, w.Body.String())
	}
	// Lengthening restores the third date (busy: requested, and the leader
	// is told) and adds a fourth (booked).
	edit("FREQ=WEEKLY;COUNT=4")
	if n := seriesBookings(t, f.app, series.ID, models.BookingConfirmed); n != 3 {
		t.Fatalf("%d confirmed; want the first, second and new fourth date", n)
	}
	if n := seriesBookings(t, f.app, series.ID, models.BookingRequested); n != 1 {
		t.Fatalf("%d requested; want the restored, busy third date", n)
	}
	if contactCount(t, f.app, &models.Notification{}, "user_id = ? AND kind = ?", f.leaderUser.ID, notify.RoomLost) != 1 {
		t.Fatal("the restored date losing its room should be announced once")
	}
}

func TestRoomSeriesSwitchRooms(t *testing.T) {
	f := newBookingFixture(t)
	var chapel struct {
		Room roomJSON `json:"room"`
	}
	decodeJSON(t, f.admin.do("POST", "/api/v1/admin/rooms", map[string]any{"name": "Chapel"}, true), &chapel)
	var annex struct {
		Room roomJSON `json:"room"`
	}
	decodeJSON(t, f.admin.do("POST", "/api/v1/admin/rooms", map[string]any{"name": "Annex"}, true), &annex)
	start := f.startOn.AddDate(0, 0, 1)
	series := createFeedEvent(t, f.leader, f.group, "Weekly", start, "FREQ=WEEKLY;COUNT=3")
	if code, _ := f.seriesRoom(t, f.leader, series, map[string]any{"roomId": f.hall.ID}); code != http.StatusOK {
		t.Fatalf("book the hall: %d", code)
	}
	// The second date gets the annex on its own; the chapel is busy on the third.
	second, third := feedDate(start.AddDate(0, 0, 7)), start.AddDate(0, 0, 14)
	if w := roomPick(t, f.leader, "/api/v1/events/"+itoa(series.ID)+"/dates/"+second+"/room", map[string]any{"roomId": annex.Room.ID}); w.Code != http.StatusOK {
		t.Fatalf("annex on one date: %d", w.Code)
	}
	other := createFeedEvent(t, f.leader, f.group, "Other", third, "")
	if w := roomPick(t, f.leader, "/api/v1/events/"+itoa(other.ID)+"/dates/"+feedDate(third)+"/room", map[string]any{"roomId": chapel.Room.ID}); w.Code != http.StatusOK {
		t.Fatalf("chapel for the other event: %d", w.Code)
	}

	code, out := f.seriesRoom(t, f.leader, series, map[string]any{"roomId": chapel.Room.ID})
	if code != http.StatusOK || out.Booked != 1 || out.Kept != 1 || len(out.Lost) != 1 || out.Lost[0] != feedDate(third) {
		t.Fatalf("switch to the chapel: %d %+v", code, out)
	}
	rooms := map[string]uint{}
	var rows []struct {
		OriginalDate string
		RoomID       uint
		Status       string
	}
	f.app.DB.Raw(`SELECT to_char(o.original_date, 'YYYY-MM-DD') AS original_date, b.room_id, b.status FROM room_bookings b JOIN event_occurrences o ON o.id = b.occurrence_id
		WHERE o.event_id = ?`, series.ID).Scan(&rows)
	for _, r := range rows {
		rooms[r.OriginalDate+" "+r.Status] = r.RoomID
	}
	if rooms[feedDate(start)+" confirmed"] != chapel.Room.ID || rooms[second+" confirmed"] != annex.Room.ID ||
		rooms[feedDate(third)+" requested"] != chapel.Room.ID || len(rows) != 3 {
		t.Fatalf("after the switch: %+v", rows)
	}
	if contactCount(t, f.app, &models.RoomBooking{}, "room_id = ? AND occurrence_id IN (SELECT id FROM event_occurrences WHERE event_id = ?)", f.hall.ID, series.ID) != 0 {
		t.Fatal("switching rooms must free the old series room on every date")
	}
}

func TestRoomSeriesChurchWideAndRoomsOff(t *testing.T) {
	f := newBookingFixture(t)
	start := f.startOn.AddDate(0, 0, 2)
	body := map[string]any{"title": "Church night", "startsLocal": start.Format("2006-01-02T15:04"),
		"durationMinutes": 60, "tzid": "UTC", "recurrenceRule": "FREQ=WEEKLY;COUNT=2", "roomId": f.nursery.ID}
	if w := f.admin.do("POST", "/api/v1/events", body, true); w.Code != http.StatusCreated {
		t.Fatalf("admin creating a church-wide event in the nursery: %d %s", w.Code, w.Body.String())
	}
	var ev models.Event
	f.app.DB.Where("title = ?", "Church night").First(&ev)
	if seriesBookings(t, f.app, ev.ID, models.BookingConfirmed) != 2 {
		t.Fatal("the church-wide series should book both dates")
	}
	// The public feed names the room (DEC-104: a restricted room shows as the place).
	anon := newAPIClient(t, f.app)
	if w := fetchFeed(t, anon, "/cal/public.ics"); !strings.Contains(w.Body.String(), "LOCATION:Nursery") {
		t.Fatalf("public feed LOCATION: %s", w.Body.String())
	}

	// Rooms off: create with a room is refused, and the extender books nothing.
	if w := f.admin.do("PUT", "/api/v1/admin/settings/rooms", map[string]any{"enabled": false}, true); w.Code != http.StatusOK {
		t.Fatalf("rooms off: %d", w.Code)
	}
	body["title"] = "Off night"
	if w := f.admin.do("POST", "/api/v1/events", body, true); w.Code != http.StatusNotFound {
		t.Fatalf("create with a room while rooms are off: %d", w.Code)
	}
	if err := f.app.DB.Where("event_id = ?", ev.ID).Delete(&models.EventOccurrence{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := eventdates.Extend(f.app.DB, time.Now().UTC(), f.app.afterExtend()); err != nil {
		t.Fatal(err)
	}
	if n := contactCount(t, f.app, &models.EventOccurrence{}, "event_id = ?", ev.ID); n != 2 {
		t.Fatalf("%d dates regenerated", n)
	}
	if n := contactCount(t, f.app, &models.RoomBooking{}, "occurrence_id IN (SELECT id FROM event_occurrences WHERE event_id = ?)", ev.ID); n != 0 {
		t.Fatalf("%d bookings made while rooms are off", n)
	}
}

// N-21 (inventory 9.2.2, a plain entry reached only by the daily extender): a
// date the extender adds loses its room and the event's leaders hear of it,
// inside a savepoint, holding only the extender's advisory lock. A second
// leader erased meanwhile is left out in either order, and the first leader is
// still told.
func TestRoomLostNoticeFromExtenderSerializesWithLeaderErasure(t *testing.T) {
	runErasureBarriers(t, 7402008, func(t *testing.T) erasureScene {
		f := newBookingFixture(t)
		victim := newTestUser(t, f.app, "leader2", "password1234", models.RoleMember)
		addMember(t, f.app, victim, &models.Group{ID: f.group}, models.MembershipLeader)
		series := createFeedEvent(t, f.leader, f.group, "Weekly", f.startOn.AddDate(0, 0, 1), "FREQ=WEEKLY;COUNT=4")
		if code, out := f.seriesRoom(t, f.leader, series, map[string]any{"roomId": f.hall.ID}); code != http.StatusOK || out.Booked != 4 {
			t.Fatalf("book the series: %d %+v", code, out)
		}
		// As in TestRoomSeriesExtenderBooksNewDates: the last two dates are
		// missing, and another event holds the hall on the last one, so the
		// extender books one date and reports one lost.
		third, fourth := feedDate(f.startOn.AddDate(0, 0, 15)), feedDate(f.startOn.AddDate(0, 0, 22))
		if err := f.app.DB.Where("event_id = ? AND original_date IN ?", series.ID, []string{third, fourth}).Delete(&models.EventOccurrence{}).Error; err != nil {
			t.Fatal(err)
		}
		other := createFeedEvent(t, f.leader, f.group, "Other", f.startOn.AddDate(0, 0, 22), "")
		if w := roomPick(t, f.leader, "/api/v1/events/"+itoa(other.ID)+"/dates/"+fourth+"/room", map[string]any{"roomId": f.hall.ID}); w.Code != http.StatusOK {
			t.Fatalf("book the other event: %d %s", w.Code, w.Body.String())
		}
		// The setup's picks told the second leader (room.changed).
		return erasureScene{app: f.app, victim: victim, mailBase: mailCount(t, f.app, victim.Email),
			run: func() error { return eventdates.Extend(f.app.DB, time.Now().UTC(), f.app.afterExtend()) },
			told: func(t *testing.T) {
				if n := contactCount(t, f.app, &models.Notification{}, "user_id = ? AND kind = ?", f.leaderUser.ID, notify.RoomLost); n != 1 {
					t.Fatalf("the first leader has %d lost-room notices, want 1", n)
				}
			},
		}
	})
}

// DEC-147: an archived group's series room can be stopped but not set.
func TestSeriesRoomNeedsActiveGroup(t *testing.T) {
	f := newBookingFixture(t)
	series := createFeedEvent(t, f.leader, f.group, "Weekly", f.startOn.AddDate(0, 0, 1), "FREQ=WEEKLY;COUNT=3")
	if code, out := f.seriesRoom(t, f.leader, series, map[string]any{"roomId": f.hall.ID}); code != http.StatusOK || out.Booked != 3 {
		t.Fatalf("book the series while active: %d %+v", code, out)
	}
	f.archiveGroup(t)
	var chapel struct {
		Room roomJSON `json:"room"`
	}
	w := f.admin.do("POST", "/api/v1/admin/rooms", map[string]any{"name": "Chapel"}, true)
	decodeJSON(t, w, &chapel)

	for name, c := range map[string]*apiClient{"leader": f.leader, "admin": f.admin} {
		w := roomPick(t, c, "/api/v1/events/"+itoa(series.ID)+"/room", map[string]any{"roomId": chapel.Room.ID})
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"code":"group_rooms_not_approved"`) {
			t.Fatalf("%s setting a series room for an archived group: %d %s", name, w.Code, w.Body.String())
		}
	}
	var ev models.Event
	f.app.DB.First(&ev, series.ID)
	if ev.DefaultRoomID == nil || *ev.DefaultRoomID != f.hall.ID {
		t.Fatalf("the series room should be unchanged: %+v", ev.DefaultRoomID)
	}
	if n := seriesBookings(t, f.app, series.ID, models.BookingConfirmed); n != 3 {
		t.Fatalf("%d confirmed bookings; the existing ones stay", n)
	}
	if code, _ := f.seriesRoom(t, f.leader, series, map[string]any{"roomId": nil}); code != http.StatusOK {
		t.Fatalf("stopping the series room: %d", code)
	}
	f.app.DB.First(&ev, series.ID)
	if ev.DefaultRoomID != nil {
		t.Fatal("stopping should clear the series room")
	}
}

// DEC-147: the extender, series edits and restores book nothing new for a
// group that isn't active, and tell no one a date lost its room.
func TestRoomSeriesExtenderSkipsInactiveGroup(t *testing.T) {
	f := newBookingFixture(t)
	series := createFeedEvent(t, f.leader, f.group, "Weekly", f.startOn.AddDate(0, 0, 1), "FREQ=WEEKLY;COUNT=4")
	if code, out := f.seriesRoom(t, f.leader, series, map[string]any{"roomId": f.hall.ID}); code != http.StatusOK || out.Booked != 4 {
		t.Fatalf("book the series: %d %+v", code, out)
	}
	f.archiveGroup(t)
	third, fourth := feedDate(f.startOn.AddDate(0, 0, 15)), feedDate(f.startOn.AddDate(0, 0, 22))
	if err := f.app.DB.Where("event_id = ? AND original_date IN ?", series.ID, []string{third, fourth}).Delete(&models.EventOccurrence{}).Error; err != nil {
		t.Fatal(err)
	}

	if err := eventdates.Extend(f.app.DB, time.Now().UTC(), f.app.afterExtend()); err != nil {
		t.Fatal(err)
	}
	if n := contactCount(t, f.app, &models.EventOccurrence{}, "event_id = ?", series.ID); n != 4 {
		t.Fatalf("the extender should still add the dates back; %d dates", n)
	}
	if n := seriesBookings(t, f.app, series.ID, models.BookingConfirmed); n != 2 {
		t.Fatalf("%d confirmed bookings; want only the two made while active", n)
	}
	if n := contactCount(t, f.app, &models.RoomBooking{}, "occurrence_id IN (SELECT id FROM event_occurrences WHERE event_id = ?)", series.ID); n != 2 {
		t.Fatalf("%d bookings of any status; the new dates should have none", n)
	}
	if n := contactCount(t, f.app, &models.RoomBooking{}, "1 = 1"); n != 2 {
		t.Fatalf("%d total bookings after extension; want the two kept bookings", n)
	}
	if contactCount(t, f.app, &models.RoomRequest{}, "event_id = ?", series.ID) != 0 {
		t.Fatal("the extender should open no room request")
	}
	if contactCount(t, f.app, &models.Notification{}, "kind = ?", notify.RoomLost) != 0 {
		t.Fatal("no room.lost notice for a group that isn't active")
	}
}

func TestRoomSeriesEditsSkipInactiveGroup(t *testing.T) {
	f := newBookingFixture(t)
	start := f.startOn.AddDate(0, 0, 1)
	series := createFeedEvent(t, f.leader, f.group, "Weekly", start, "FREQ=WEEKLY;COUNT=3")
	if code, out := f.seriesRoom(t, f.leader, series, map[string]any{"roomId": f.hall.ID}); code != http.StatusOK || out.Booked != 3 {
		t.Fatalf("book the series: %d %+v", code, out)
	}
	f.archiveGroup(t)
	edit := func(rule string) {
		t.Helper()
		if w := f.leader.do("PUT", "/api/v1/events/"+itoa(series.ID), seriesBody(t, f.leader, "/api/v1/events/"+itoa(series.ID), map[string]any{"recurrenceRule": rule}), true); w.Code != http.StatusOK {
			t.Fatalf("series edit %s: %d %s", rule, w.Code, w.Body.String())
		}
	}
	// Shorten then lengthen: the third date is cancelled (its room freed),
	// then restored along with a new fourth; neither is booked.
	edit("FREQ=WEEKLY;COUNT=2")
	edit("FREQ=WEEKLY;COUNT=4")
	if n := contactCount(t, f.app, &models.EventOccurrence{}, "event_id = ? AND status = ?", series.ID, models.OccurrenceScheduled); n != 4 {
		t.Fatalf("%d scheduled dates after the edits; want 4", n)
	}
	if n := contactCount(t, f.app, &models.RoomBooking{}, "occurrence_id IN (SELECT id FROM event_occurrences WHERE event_id = ?)", series.ID); n != 2 {
		t.Fatalf("%d bookings; only the first two dates keep theirs", n)
	}
	if contactCount(t, f.app, &models.Notification{}, "kind = ?", notify.RoomLost) != 0 {
		t.Fatal("no room.lost notice for a group that isn't active")
	}
}

// DEC-167: a series pick names the event's read. The series room moves the
// event's revision (default_room_id), so a second manager's pick from an
// older read is refused with nothing written, and the series editor's own
// save from before the pick is refused as event_changed with every field it
// shows as it was (the editor then sends it again by itself, DEC-162).
func TestSeriesPickFromAnOlderReadIsRefused(t *testing.T) {
	f := newBookingFixture(t)
	co, _ := f.coLeader(t)
	series := createFeedEvent(t, f.leader, f.group, "Weekly", f.startOn.AddDate(0, 0, 1), "FREQ=WEEKLY;COUNT=4")
	path := "/api/v1/events/" + itoa(series.ID) + "/room"
	var chapel struct {
		Room roomJSON `json:"room"`
	}
	decodeJSON(t, f.admin.do("POST", "/api/v1/admin/rooms", map[string]any{"name": "Chapel"}, true), &chapel)
	editorOpened := seriesRevisionOf(t, f.leader, series.ID)
	first, second := roomRevision(t, f.leader, path), roomRevision(t, co, path)
	if first == "" || first != second || first != editorOpened {
		t.Fatalf("the series reads: %q, %q; the editor's %q", first, second, editorOpened)
	}
	w := f.leader.do("PUT", path, withRevision(map[string]any{"roomId": f.hall.ID}, first), true)
	if w.Code != http.StatusOK {
		t.Fatalf("the first pick: %d %s", w.Code, w.Body.String())
	}
	var landed seriesBookingJSON
	decodeJSON(t, w, &landed)
	if landed.Event.ID != series.ID || landed.Event.Revision == first || landed.Event.Revision != roomRevision(t, f.leader, path) || landed.Booked != 4 {
		t.Fatalf("the first pick's answer %+v", landed)
	}
	before := roomState(t, f.app)
	// Naming dates to change too writes nothing either.
	stale := map[string]any{"roomId": chapel.Room.ID, "includeDates": []string{feedDate(f.startOn.AddDate(0, 0, 1))}}
	wantCode(t, "the second pick from the older read", co.do("PUT", path, withRevision(stale, second), true), http.StatusConflict, "event_changed")
	wantCode(t, "a pick with no revision", co.do("PUT", path, map[string]any{"roomId": chapel.Room.ID}, true), http.StatusConflict, "event_changed")
	if after := roomState(t, f.app); after != before {
		t.Fatalf("a refused series pick wrote:\n%s\nwant\n%s", after, before)
	}
	// The editor opened before the pick: its save is refused, and the read it
	// then makes shows every field it compares as it opened.
	events := "/api/v1/events/" + itoa(series.ID)
	wantCode(t, "the editor's save from before the pick", f.leader.do("PUT", events, map[string]any{"title": "Weekly supper", "revision": editorOpened}, true), http.StatusConflict, "event_changed")
	var page struct {
		Event eventJSON `json:"event"`
	}
	decodeJSON(t, f.leader.do("GET", events, nil, false), &page)
	if page.Event.Title != series.Title || page.Event.RecurrenceRule != series.RecurrenceRule || page.Event.StartsLocal != series.StartsLocal {
		t.Fatalf("the series pick changed what the editor shows: %+v", page.Event)
	}
	apiDo(t, f.leader, "PUT", events, map[string]any{"title": "Weekly supper", "revision": page.Event.Revision}, http.StatusOK)
	// And the series pick from the answer's revision is stale now too; a fresh read goes through.
	wantCode(t, "a pick from before the editor's save", f.leader.do("PUT", path, withRevision(map[string]any{"roomId": chapel.Room.ID}, landed.Event.Revision), true), http.StatusConflict, "event_changed")
	if code, out := f.seriesRoom(t, co, series, map[string]any{"roomId": chapel.Room.ID}); code != http.StatusOK || out.DefaultRoom == nil || out.DefaultRoom.ID != chapel.Room.ID {
		t.Fatalf("the second leader's pick from a fresh read: %d %+v", code, out)
	}
}

// A leader demoted while their series pick waits for the event's lock gets
// the refusal of someone who may not manage the event, and nothing is written.
func TestSeriesPickByALeaderDemotedWhileItWaitsWritesNothing(t *testing.T) {
	f := newBookingFixture(t)
	co, coUser := f.coLeader(t)
	series := createFeedEvent(t, f.leader, f.group, "Weekly", f.startOn.AddDate(0, 0, 1), "FREQ=WEEKLY;COUNT=4")
	path := "/api/v1/events/" + itoa(series.ID) + "/room"
	pick := withRevision(map[string]any{"roomId": f.hall.ID}, roomRevision(t, co, path))
	before := roomState(t, f.app)
	holder := f.app.DB.Begin()
	defer holder.Rollback()
	gormMust(t, holder.Exec("SELECT 1 FROM events WHERE id = ? FOR UPDATE", series.ID))
	answer := make(chan *httptest.ResponseRecorder, 1)
	go func() { answer <- co.do("PUT", path, pick, true) }()
	waitForLockedUsersQuery(t, f.app, "%events%FOR UPDATE%")
	gormMust(t, holder.Exec("UPDATE memberships SET role = ? WHERE user_id = ? AND group_id = ?", models.MembershipMember, coUser.ID, f.group))
	gormMust(t, holder.Commit())
	wantCode(t, "the demoted leader's series pick", <-answer, http.StatusForbidden, "")
	if after := roomState(t, f.app); after != before {
		t.Fatalf("the demoted leader's series pick wrote:\n%s\nwant\n%s", after, before)
	}
}

// seriesRoomsRead is c's read of the series room control.
func seriesRoomsRead(t *testing.T, c *apiClient, ev eventJSON) seriesRoomsJSON {
	t.Helper()
	var out seriesRoomsJSON
	if err := json.Unmarshal([]byte(apiDo(t, c, "GET", "/api/v1/events/"+itoa(ev.ID)+"/rooms", nil, http.StatusOK)), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// seriesPickFrom sends c's series pick made from read.
func seriesPickFrom(t *testing.T, c *apiClient, ev eventJSON, read seriesRoomsJSON, body map[string]any) seriesBookingJSON {
	t.Helper()
	var out seriesBookingJSON
	if err := json.Unmarshal([]byte(apiDo(t, c, "PUT", "/api/v1/events/"+itoa(ev.ID)+"/room", withRevision(body, read.Revision), http.StatusOK)), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// dateBookingOf is the booking of ev's date day ("" status when it has none),
// and whether its request is still undecided.
func dateBookingOf(t *testing.T, app *App, ev eventJSON, day string) (roomID uint, status string, waiting bool, requestID *uint) {
	t.Helper()
	var rows []struct {
		RoomID    uint
		Status    string
		Waiting   bool
		RequestID *uint
	}
	gormMust(t, app.DB.Raw(`SELECT b.room_id, b.status, b.request_id, EXISTS (SELECT 1 FROM room_requests q WHERE q.id = b.request_id AND q.status = ?) AS waiting
		FROM room_bookings b JOIN event_occurrences o ON o.id = b.occurrence_id WHERE o.event_id = ? AND o.original_date = ?`,
		models.RoomRequestPending, ev.ID, day).Scan(&rows))
	if len(rows) == 0 {
		return 0, "", false, nil
	}
	return rows[0].RoomID, rows[0].Status, rows[0].Waiting, rows[0].RequestID
}

func ownDates(list []ownRoomJSON) []string {
	out := []string{}
	for _, o := range list {
		out = append(out, o.Date)
	}
	return out
}

// noOrphanRequests: no pending request is left with no date waiting on it,
// and no admin task stays open for a request that is no longer pending.
func noOrphanRequests(t *testing.T, app *App) {
	t.Helper()
	if n := contactCount(t, app, &models.RoomRequest{}, "status = ? AND NOT EXISTS (SELECT 1 FROM room_bookings b WHERE b.request_id = room_requests.id AND b.status = ?)",
		models.RoomRequestPending, models.BookingRequested); n != 0 {
		t.Fatalf("%d pending requests no date waits on", n)
	}
	if n := contactCount(t, app, &models.Notification{}, "kind = ? AND waiting AND resolved_at IS NULL AND subject_id IN (SELECT id FROM room_requests WHERE status <> ?)",
		notify.RoomRequested, models.RoomRequestPending); n != 0 {
		t.Fatalf("%d admin tasks open for requests that are closed", n)
	}
}

// DEC-168: a series room change leaves alone every date with a room of its
// own, confirmed or waiting for an admin, and the read lists them first. Named,
// exactly those dates change, and a waiting one's request is withdrawn.
func TestSeriesPickLeavesDatesWithTheirOwnRoom(t *testing.T) {
	f := newBookingFixture(t)
	series := createFeedEvent(t, f.leader, f.group, "Weekly", f.startOn.AddDate(0, 0, 1), "FREQ=WEEKLY;COUNT=4")
	chapel := newRoom(t, f, "Chapel")
	day := func(n int) string { return feedDate(f.startOn.AddDate(0, 0, 1+7*n)) }
	datePath := func(n int) string { return "/api/v1/events/" + itoa(series.ID) + "/dates/" + day(n) + "/room" }
	roomPickOK(t, f.leader, datePath(0), map[string]any{"roomId": chapel.ID})
	approvalOn(t, f)
	roomPickOK(t, f.leader, datePath(1), map[string]any{"roomId": chapel.ID})
	_, _, _, ownRequest := dateBookingOf(t, f.app, series, day(1))
	if ownRequest == nil {
		t.Fatal("the second date's pick should wait on a request")
	}

	read := seriesRoomsRead(t, f.leader, series)
	want := []ownRoomJSON{
		{Date: day(0), Room: occurrenceRoomJSON{ID: chapel.ID, Name: "Chapel"}, MayChange: true},
		{Date: day(1), Room: occurrenceRoomJSON{ID: chapel.ID, Name: "Chapel"}, Waiting: true, MayChange: true},
	}
	if !reflect.DeepEqual(read.OwnRooms, want) || read.Pending != 0 || len(read.NeedsRoom) != 0 {
		t.Fatalf("the read: own rooms %+v, pending %d, needs %v; want %+v and nothing else", read.OwnRooms, read.Pending, read.NeedsRoom, want)
	}

	out := seriesPickFrom(t, f.leader, series, read, map[string]any{"roomId": f.hall.ID})
	if out.Kept != 2 || out.Changed != 0 || out.Pending != 2 || out.Booked != 0 || out.KeptWaiting != 1 || !reflect.DeepEqual(out.After.OwnRooms, want) {
		t.Fatalf("the pick: %+v, want 2 kept, 2 waiting and the same list", out)
	}
	if room, status, _, _ := dateBookingOf(t, f.app, series, day(0)); room != chapel.ID || status != models.BookingConfirmed {
		t.Fatalf("the confirmed own room is now %d %s", room, status)
	}
	if room, _, waiting, req := dateBookingOf(t, f.app, series, day(1)); room != chapel.ID || !waiting || req == nil || *req != *ownRequest {
		t.Fatalf("the waiting own room is now in %d, waiting %v on %v", room, waiting, req)
	}
	noOrphanRequests(t, f.app)

	read = seriesRoomsRead(t, f.leader, series)
	if !reflect.DeepEqual(read.OwnRooms, want) || read.Pending != 2 {
		t.Fatalf("after the pick, the read: %+v pending %d", read.OwnRooms, read.Pending)
	}
	out = seriesPickFrom(t, f.leader, series, read, map[string]any{"roomId": f.hall.ID, "includeDates": ownDates(read.OwnRooms)})
	if out.Kept != 0 || out.Changed != 2 || out.Pending != 4 || len(out.After.OwnRooms) != 0 {
		t.Fatalf("the pick with them named: %+v, want 2 changed and 4 waiting", out)
	}
	for n := range 4 {
		if room, _, waiting, req := dateBookingOf(t, f.app, series, day(n)); room != f.hall.ID || !waiting || req == nil || *req == *ownRequest {
			t.Fatalf("date %d: in %d, waiting %v on %v; want the hall, on the series request", n, room, waiting, req)
		}
	}
	var mine models.RoomRequest
	gormMust(t, f.app.DB.First(&mine, *ownRequest))
	if mine.Status != models.RoomRequestWithdrawn {
		t.Fatalf("the included date's own request is %s, want withdrawn", mine.Status)
	}
	if n := contactCount(t, f.app, &models.RoomRequest{}, "event_id = ? AND status = ?", series.ID, models.RoomRequestPending); n != 1 {
		t.Fatalf("%d pending requests, want the series' one", n)
	}
	noOrphanRequests(t, f.app)
	var row models.AuditLog
	gormMust(t, f.app.DB.Where("action = ? AND target_id = ?", audit.RoomBookingSeries, series.ID).Order("id DESC").First(&row))
	var meta struct {
		KeptDates    []string `json:"keptDates"`
		ChangedDates []string `json:"changedDates"`
	}
	if err := json.Unmarshal([]byte(row.MetaJSON), &meta); err != nil || meta.KeptDates == nil || len(meta.KeptDates) != 0 ||
		!reflect.DeepEqual(meta.ChangedDates, []string{day(0), day(1)}) {
		t.Fatalf("the audit row's dates: %v %s", err, row.MetaJSON)
	}
}

// The named dates are the only ones a pick takes a room from: a date that got
// a room of its own after the read is kept, and so is a shown one left out.
// A date booked on its own does not move the event's revision, so the pick
// from the older read is not refused.
func TestSeriesPickChangesOnlyTheNamedDates(t *testing.T) {
	f := newBookingFixture(t)
	co, _ := f.coLeader(t)
	series := createFeedEvent(t, f.leader, f.group, "Weekly", f.startOn.AddDate(0, 0, 1), "FREQ=WEEKLY;COUNT=4")
	chapel, annex := newRoom(t, f, "Chapel"), newRoom(t, f, "Annex")
	day := func(n int) string { return feedDate(f.startOn.AddDate(0, 0, 1+7*n)) }
	datePath := func(n int) string { return "/api/v1/events/" + itoa(series.ID) + "/dates/" + day(n) + "/room" }
	roomPickOK(t, f.leader, datePath(0), map[string]any{"roomId": chapel.ID})
	roomPickOK(t, f.leader, datePath(1), map[string]any{"roomId": annex.ID})
	read := seriesRoomsRead(t, f.leader, series)
	if got := ownDates(read.OwnRooms); !reflect.DeepEqual(got, []string{day(0), day(1)}) {
		t.Fatalf("listed %v", got)
	}
	roomPickOK(t, co, datePath(2), map[string]any{"roomId": chapel.ID})

	// The first date is named, the second shown and left out; the third was
	// never shown, and the request also names a date with no room of its own.
	out := seriesPickFrom(t, f.leader, series, read, map[string]any{"roomId": f.hall.ID, "includeDates": []string{day(0), day(3)}})
	if out.Changed != 1 || out.Kept != 2 || out.Booked != 2 || !reflect.DeepEqual(ownDates(out.After.OwnRooms), []string{day(1), day(2)}) {
		t.Fatalf("the pick: %+v, want 1 changed, 2 kept and 2 booked", out)
	}
	for n, room := range []uint{f.hall.ID, annex.ID, chapel.ID, f.hall.ID} {
		if got, status, _, _ := dateBookingOf(t, f.app, series, day(n)); got != room || status != models.BookingConfirmed {
			t.Fatalf("date %d: %d %s, want room %d", n, got, status, room)
		}
	}
	wantCode(t, "a named date that is no date", f.leader.do("PUT", "/api/v1/events/"+itoa(series.ID)+"/room",
		withRevision(map[string]any{"roomId": nil, "includeDates": []string{"next week"}}, out.Event.Revision), true), http.StatusBadRequest, "series_date_invalid")
}

// A leader cannot include a booking only an admin could make: it is listed,
// may not be changed, and stays. An admin's pick naming it changes it.
func TestSeriesPickIncludesAnAdminOnlyBookingOnlyForAnAdmin(t *testing.T) {
	f := newBookingFixture(t)
	series := createFeedEvent(t, f.leader, f.group, "Weekly", f.startOn.AddDate(0, 0, 1), "FREQ=WEEKLY;COUNT=3")
	chapel := newRoom(t, f, "Chapel")
	first := feedDate(f.startOn.AddDate(0, 0, 1))
	roomPickOK(t, f.admin, "/api/v1/events/"+itoa(series.ID)+"/dates/"+first+"/room", map[string]any{"roomId": f.nursery.ID})

	read := seriesRoomsRead(t, f.leader, series)
	if len(read.OwnRooms) != 1 || read.OwnRooms[0].MayChange || read.OwnRooms[0].Room.Name != "Nursery" {
		t.Fatalf("the leader's list: %+v", read.OwnRooms)
	}
	out := seriesPickFrom(t, f.leader, series, read, map[string]any{"roomId": f.hall.ID, "includeDates": []string{first}})
	if out.Changed != 0 || out.Kept != 1 || out.Booked != 2 {
		t.Fatalf("the leader's pick: %+v", out)
	}
	if room, _, _, _ := dateBookingOf(t, f.app, series, first); room != f.nursery.ID {
		t.Fatalf("the nursery date is now in room %d", room)
	}

	read = seriesRoomsRead(t, f.admin, series)
	if len(read.OwnRooms) != 1 || !read.OwnRooms[0].MayChange {
		t.Fatalf("the admin's list: %+v", read.OwnRooms)
	}
	out = seriesPickFrom(t, f.admin, series, read, map[string]any{"roomId": chapel.ID, "includeDates": []string{first}})
	if out.Changed != 1 || out.Kept != 0 || out.Booked != 3 {
		t.Fatalf("the admin's pick: %+v", out)
	}
	if room, _, _, _ := dateBookingOf(t, f.app, series, first); room != chapel.ID {
		t.Fatalf("the nursery date is now in room %d, want the chapel", room)
	}
}

// Stopping the series room releases its bookings and keeps every date's own
// room; with those dates named, it releases theirs too.
func TestStoppingTheSeriesRoomKeepsOwnRoomsUnlessNamed(t *testing.T) {
	f := newBookingFixture(t)
	series := createFeedEvent(t, f.leader, f.group, "Weekly", f.startOn.AddDate(0, 0, 1), "FREQ=WEEKLY;COUNT=3")
	chapel := newRoom(t, f, "Chapel")
	second := feedDate(f.startOn.AddDate(0, 0, 8))
	roomPickOK(t, f.leader, "/api/v1/events/"+itoa(series.ID)+"/room", map[string]any{"roomId": f.hall.ID})
	roomPickOK(t, f.leader, "/api/v1/events/"+itoa(series.ID)+"/dates/"+second+"/room", map[string]any{"roomId": chapel.ID})
	bookings := func() int64 {
		return contactCount(t, f.app, &models.RoomBooking{}, "occurrence_id IN (SELECT id FROM event_occurrences WHERE event_id = ?)", series.ID)
	}

	out := seriesPickFrom(t, f.leader, series, seriesRoomsRead(t, f.leader, series), map[string]any{"roomId": nil})
	if out.DefaultRoom != nil || out.Kept != 1 || out.Changed != 0 || bookings() != 1 || !reflect.DeepEqual(ownDates(out.After.OwnRooms), []string{second}) {
		t.Fatalf("stop: %+v, %d bookings left", out, bookings())
	}
	if room, _, _, _ := dateBookingOf(t, f.app, series, second); room != chapel.ID {
		t.Fatalf("the own room is now %d", room)
	}

	roomPickOK(t, f.leader, "/api/v1/events/"+itoa(series.ID)+"/room", map[string]any{"roomId": f.hall.ID})
	read := seriesRoomsRead(t, f.leader, series)
	out = seriesPickFrom(t, f.leader, series, read, map[string]any{"roomId": nil, "includeDates": ownDates(read.OwnRooms)})
	if out.Kept != 0 || out.Changed != 1 || bookings() != 0 || len(out.After.OwnRooms) != 0 {
		t.Fatalf("stop with the own room named: %+v, %d bookings left", out, bookings())
	}
}

// Picking the series room it already has books every date that has no booking
// (a date released on its own since), without asking for approval again, and
// the answer counts only what this pick did: the dates it booked, and of them
// the ones an admin's reason booked over another booking.
func TestSeriesPickOfItsOwnRoomBooksBareDatesAndCountsOverlaps(t *testing.T) {
	f := newBookingFixture(t)
	// A holds the hall on the series' first date.
	roomPickOK(t, f.leader, f.path(f.eventA, "/room"), map[string]any{"roomId": f.hall.ID})
	series := createFeedEvent(t, f.leader, f.group, "Weekly", f.startOn.Add(30*time.Minute), "FREQ=WEEKLY;COUNT=4")
	if code, out := f.seriesRoom(t, f.admin, series, map[string]any{"roomId": f.hall.ID}); code != http.StatusOK || out.Booked != 3 || len(out.Lost) != 1 || out.DoubleBooked != 0 {
		t.Fatalf("book the series: %d %+v", code, out)
	}
	if code, out := f.seriesRoom(t, f.admin, series, map[string]any{"roomId": f.hall.ID, "overlapReason": "Choir moved", "reviewedBusyDates": []string{feedDate(f.startOn)}}); code != http.StatusOK ||
		out.Booked != 1 || out.DoubleBooked != 1 || len(out.Lost) != 0 {
		t.Fatalf("book the busy date anyway: %d %+v", code, out)
	}
	if w := f.admin.do("PUT", "/api/v1/admin/settings/room-approval", map[string]any{"enabled": true}, true); w.Code != http.StatusOK {
		t.Fatalf("approval on: %d", w.Code)
	}
	second := feedDate(f.startOn.AddDate(0, 0, 7))
	roomPickOK(t, f.leader, "/api/v1/events/"+itoa(series.ID)+"/dates/"+second+"/room", map[string]any{"roomId": nil})
	requests := contactCount(t, f.app, &models.RoomRequest{}, "event_id = ?", series.ID)
	if code, out := f.seriesRoom(t, f.leader, series, map[string]any{"roomId": f.hall.ID}); code != http.StatusOK ||
		out.Booked != 1 || out.Pending != 0 || out.DoubleBooked != 0 || len(out.Lost) != 0 {
		t.Fatalf("pick the series room again: %d %+v", code, out)
	}
	if n := seriesBookings(t, f.app, series.ID, models.BookingConfirmed); n != 4 {
		t.Fatalf("%d confirmed bookings, want every date", n)
	}
	if n := contactCount(t, f.app, &models.RoomRequest{}, "event_id = ?", series.ID); n != requests {
		t.Fatalf("picking the approved series room again opened a request: %d, was %d", n, requests)
	}
}

// afterIsTheRead: the answer's dates are what c's read shows right after it.
func afterIsTheRead(t *testing.T, c *apiClient, ev eventJSON, out seriesBookingJSON) {
	t.Helper()
	read := seriesRoomsRead(t, c, ev)
	if now := (seriesDatesJSON{OwnRooms: read.OwnRooms, NeedsRoom: read.NeedsRoom, Pending: read.Pending}); !reflect.DeepEqual(out.After, now) || out.Event.Revision != read.Revision {
		t.Fatalf("the answer's dates %+v (%s), the read %+v (%s)", out.After, out.Event.Revision, now, read.Revision)
	}
}

// The next pick is chosen from the dates as they are after the last one: once
// the series room is the hall, a date waiting for the hall is the series' own,
// so the answer no longer lists it as keeping its room, and stopping the series
// room releases it as the answer's list said it would.
func TestSeriesPickAnswersWithTheDatesAsTheyAreNow(t *testing.T) {
	f := newBookingFixture(t)
	series := createFeedEvent(t, f.leader, f.group, "Weekly", f.startOn.AddDate(0, 0, 1), "FREQ=WEEKLY;COUNT=3")
	first := feedDate(f.startOn.AddDate(0, 0, 1))
	approvalOn(t, f)
	roomPickOK(t, f.leader, "/api/v1/events/"+itoa(series.ID)+"/dates/"+first+"/room", map[string]any{"roomId": f.hall.ID})

	read := seriesRoomsRead(t, f.admin, series)
	if got := ownDates(read.OwnRooms); !reflect.DeepEqual(got, []string{first}) {
		t.Fatalf("listed %v", got)
	}
	out := seriesPickFrom(t, f.admin, series, read, map[string]any{"roomId": f.hall.ID})
	if out.Kept != 1 || out.KeptWaiting != 1 || out.Booked != 2 {
		t.Fatalf("the hall for the series: %+v", out)
	}
	afterIsTheRead(t, f.admin, series, out)
	if len(out.After.OwnRooms) != 0 || out.After.Pending != 1 {
		t.Fatalf("after the hall, the answer lists %+v with %d waiting; want none listed and the hall's waiting date counted", out.After.OwnRooms, out.After.Pending)
	}

	read = seriesRoomsRead(t, f.admin, series)
	stop := seriesPickFrom(t, f.admin, series, read, map[string]any{"roomId": nil})
	if stop.Kept != len(out.After.OwnRooms) || stop.KeptWaiting != 0 {
		t.Fatalf("stop kept %d, the list showed %d", stop.Kept, len(out.After.OwnRooms))
	}
	if _, status, _, _ := dateBookingOf(t, f.app, series, first); status != "" {
		t.Fatalf("the waiting date is %q, want released as the list said", status)
	}
	afterIsTheRead(t, f.admin, series, stop)
	noOrphanRequests(t, f.app)
}

// A leader's pick never withdraws a request the list says only an admin can
// change: with dates kept on the series request, the pick cannot open its
// own, so it is refused and writes nothing. An admin's pick, which opens no
// request, changes them.
func TestSeriesPickKeepsARequestOnlyAnAdminCanChange(t *testing.T) {
	f := newBookingFixture(t)
	series := createFeedEvent(t, f.leader, f.group, "Weekly", f.startOn.AddDate(0, 0, 1), "FREQ=WEEKLY;COUNT=2")
	chapel := newRoom(t, f, "Chapel")
	approvalOn(t, f)
	if code, out := f.seriesRoom(t, f.leader, series, map[string]any{"roomId": f.hall.ID}); code != http.StatusOK || out.Pending != 2 {
		t.Fatalf("request the hall: %d %+v", code, out)
	}
	apiDo(t, f.admin, "PUT", "/api/v1/admin/rooms/"+itoa(f.hall.ID), withRevision(map[string]any{"restricted": true}, adminRoomRevision(t, f.admin, f.hall.ID)), http.StatusOK)

	read := seriesRoomsRead(t, f.leader, series)
	if len(read.OwnRooms) != 2 || read.OwnRooms[0].MayChange || !read.OwnRooms[0].Waiting {
		t.Fatalf("the leader's list: %+v", read.OwnRooms)
	}
	before := roomState(t, f.app)
	wantCode(t, "the leader's pick", f.leader.do("PUT", "/api/v1/events/"+itoa(series.ID)+"/room",
		withRevision(map[string]any{"roomId": chapel.ID}, read.Revision), true), http.StatusConflict, "series_request_kept")
	if after := roomState(t, f.app); after != before {
		t.Fatalf("the refused pick wrote:\n%s\nwant\n%s", after, before)
	}
	if n := contactCount(t, f.app, &models.Notification{}, "kind = ? AND waiting AND resolved_at IS NULL", notify.RoomRequested); n != 1 {
		t.Fatalf("%d open admin tasks, want the hall's", n)
	}

	out := seriesPickFrom(t, f.admin, series, seriesRoomsRead(t, f.admin, series), map[string]any{"roomId": chapel.ID})
	if out.Booked != 2 || out.Kept != 0 || out.Pending != 0 {
		t.Fatalf("the admin's pick: %+v", out)
	}
	for _, day := range []string{feedDate(f.startOn.AddDate(0, 0, 1)), feedDate(f.startOn.AddDate(0, 0, 8))} {
		if room, status, _, _ := dateBookingOf(t, f.app, series, day); room != chapel.ID || status != models.BookingConfirmed {
			t.Fatalf("%s: %d %s, want the chapel", day, room, status)
		}
	}
	noOrphanRequests(t, f.app)
}

func TestSeriesBookAnywayRequiresTheSameBusyDates(t *testing.T) {
	f := newBookingFixture(t)
	first, second := feedDate(f.startOn), feedDate(f.startOn.AddDate(0, 0, 7))
	other := createFeedEvent(t, f.leader, f.group, "Second holder", f.startOn.AddDate(0, 0, 7), "")
	series := createFeedEvent(t, f.leader, f.group, "Weekly", f.startOn, "FREQ=WEEKLY;COUNT=3")
	roomPickOK(t, f.leader, f.path(f.eventA, "/room"), map[string]any{"roomId": f.hall.ID})
	pick := seriesPickFrom(t, f.admin, series, seriesRoomsRead(t, f.admin, series), map[string]any{"roomId": f.hall.ID})
	if !reflect.DeepEqual(pick.Lost, []string{first}) {
		t.Fatalf("first pick reported busy dates %v, want %s", pick.Lost, first)
	}

	roomPath := "/api/v1/events/" + itoa(series.ID) + "/room"
	confirm := func(reviewed []string) map[string]any {
		return withRevision(map[string]any{"roomId": f.hall.ID, "overlapReason": "Shared night", "reviewedBusyDates": reviewed}, pick.Event.Revision)
	}
	refused := func(reviewed, want []string) {
		t.Helper()
		w := f.admin.do("PUT", roomPath, confirm(reviewed), true)
		wantCode(t, "confirm series busy dates", w, http.StatusConflict, "series_busy_dates_changed")
		var res struct {
			BusyDates []string `json:"busyDates"`
		}
		decodeJSON(t, w, &res)
		if !reflect.DeepEqual(res.BusyDates, want) {
			t.Fatalf("busy dates %v, want %v", res.BusyDates, want)
		}
	}
	audits := func() int64 {
		return contactCount(t, f.app, &models.AuditLog{}, "action = ? AND target_id = ?", audit.RoomBookingSeries, series.ID)
	}
	refused(nil, []string{first})
	roomPickOK(t, f.leader, "/api/v1/events/"+itoa(series.ID)+"/dates/"+second+"/room", map[string]any{"roomId": nil})
	roomPickOK(t, f.leader, "/api/v1/events/"+itoa(other.ID)+"/dates/"+second+"/room", map[string]any{"roomId": f.hall.ID})
	refused(pick.Lost, []string{first, second})
	if count := seriesBookings(t, f.app, series.ID, models.BookingConfirmed); count != 1 || audits() != 1 {
		t.Fatalf("refused confirmation left %d confirmed bookings and %d series audits, want 1 of each", count, audits())
	}

	chapel := newRoom(t, f, "Chapel")
	roomPickOK(t, f.admin, "/api/v1/events/"+itoa(series.ID)+"/dates/"+first+"/room", map[string]any{"roomId": chapel.ID})
	refused([]string{first, second}, []string{second})
	refused(pick.Lost, []string{second})
	if room, _, _, _ := dateBookingOf(t, f.app, series, first); room != chapel.ID || audits() != 1 {
		t.Fatalf("the refused confirmation changed the date's own room (%d) or the series audit count (%d)", room, audits())
	}

	w := f.admin.do("PUT", roomPath, confirm([]string{second}), true)
	if w.Code != http.StatusOK {
		t.Fatalf("confirming the fresh list: %d %s", w.Code, w.Body.String())
	}
	var booked seriesBookingJSON
	decodeJSON(t, w, &booked)
	if booked.DoubleBooked != 1 || booked.Booked != 1 || audits() != 2 {
		t.Fatalf("fresh confirmation: %+v, %d audits", booked, audits())
	}
	refused([]string{second}, []string{})
	if audits() != 2 {
		t.Fatalf("the replay added a series audit: %d", audits())
	}
}

// "Book anyway" books over others only where the pick left a date with no
// room, which is what its prompt counted: a date that waits for the room keeps
// its request for an admin to decide.
func TestSeriesBookAnywayLeavesAWaitingDateToItsRequest(t *testing.T) {
	f := newBookingFixture(t)
	start := f.startOn.AddDate(0, 0, 1)
	series := createFeedEvent(t, f.leader, f.group, "Choir", start, "FREQ=WEEKLY;COUNT=2")
	other := createFeedEvent(t, f.leader, f.group, "Band", start, "FREQ=WEEKLY;COUNT=2")
	first, second := feedDate(start), feedDate(start.AddDate(0, 0, 7))
	approvalOn(t, f)
	roomPickOK(t, f.leader, "/api/v1/events/"+itoa(series.ID)+"/dates/"+first+"/room", map[string]any{"roomId": f.hall.ID})
	if code, out := f.seriesRoom(t, f.admin, other, map[string]any{"roomId": f.hall.ID}); code != http.StatusOK || out.Booked != 2 {
		t.Fatalf("the band's hall: %d %+v", code, out)
	}

	out := seriesPickFrom(t, f.admin, series, seriesRoomsRead(t, f.admin, series), map[string]any{"roomId": f.hall.ID})
	if out.Kept != 1 || out.KeptWaiting != 1 || len(out.Lost) != 1 || out.Booked != 0 {
		t.Fatalf("the pick: %+v, want the waiting date kept and one date with no room", out)
	}
	out = seriesPickFrom(t, f.admin, series, seriesRoomsRead(t, f.admin, series), map[string]any{"roomId": f.hall.ID, "overlapReason": "Choir moved", "reviewedBusyDates": out.Lost})
	if out.Booked != 1 || out.DoubleBooked != 1 {
		t.Fatalf("book anyway: %+v, want the one date its prompt counted", out)
	}
	if room, status, waiting, _ := dateBookingOf(t, f.app, series, first); room != f.hall.ID || status != models.BookingRequested || !waiting {
		t.Fatalf("the waiting date: %d %s waiting %v, want it still waiting for the hall", room, status, waiting)
	}
	if _, status, _, _ := dateBookingOf(t, f.app, series, second); status != models.BookingConfirmed {
		t.Fatalf("the busy date: %s, want it booked", status)
	}
	noOrphanRequests(t, f.app)
}

// Including a date never leaves it with no room: a named date whose new room is
// in use then keeps the room it has, untouched, and is not counted as changed.
func TestSeriesPickKeepsANamedDateWhoseNewRoomIsInUse(t *testing.T) {
	f := newBookingFixture(t)
	music, parlor := newRoom(t, f, "Music"), newRoom(t, f, "Parlor")
	roomPickOK(t, f.leader, f.path(f.eventA, "/room"), map[string]any{"roomId": music.ID})
	series := createFeedEvent(t, f.leader, f.group, "Weekly", f.startOn.Add(30*time.Minute), "FREQ=WEEKLY;COUNT=3")
	day := func(n int) string { return feedDate(f.startOn.AddDate(0, 0, 7*n)) }
	for n := range 2 {
		roomPickOK(t, f.leader, "/api/v1/events/"+itoa(series.ID)+"/dates/"+day(n)+"/room", map[string]any{"roomId": parlor.ID})
	}
	var was models.RoomBooking
	gormMust(t, f.app.DB.Where("occurrence_id IN (SELECT id FROM event_occurrences WHERE event_id = ? AND original_date = ?)", series.ID, day(0)).First(&was))

	read := seriesRoomsRead(t, f.leader, series)
	out := seriesPickFrom(t, f.leader, series, read, map[string]any{"roomId": music.ID, "includeDates": ownDates(read.OwnRooms)})
	if out.Changed != 1 || out.KeptInUse != 1 || out.Kept != 0 || out.Booked != 2 || len(out.Lost) != 0 {
		t.Fatalf("the pick: %+v, want 1 changed, 1 kept as the music room is in use, 2 booked", out)
	}
	var now models.RoomBooking
	gormMust(t, f.app.DB.First(&now, was.ID))
	if now.RoomID != parlor.ID || now.Status != models.BookingConfirmed || !now.UpdatedAt.Equal(was.UpdatedAt) {
		t.Fatalf("the busy date's booking is now %+v, was %+v", now, was)
	}
	if room, _, _, _ := dateBookingOf(t, f.app, series, day(1)); room != music.ID {
		t.Fatalf("the free date is in %d, want the music room", room)
	}
	if got := ownDates(out.After.OwnRooms); !reflect.DeepEqual(got, []string{day(0)}) {
		t.Fatalf("the answer lists %v, want the busy date", got)
	}
	afterIsTheRead(t, f.leader, series, out)
}

// The dates a pick names are its own event's: another event's booking on a
// named date stays as it is.
func TestSeriesPickNamingAnotherEventsDateLeavesItAlone(t *testing.T) {
	f := newBookingFixture(t)
	chapel := newRoom(t, f, "Chapel")
	series := createFeedEvent(t, f.leader, f.group, "Weekly", f.startOn.AddDate(0, 0, 1), "FREQ=WEEKLY;COUNT=2")
	roomPickOK(t, f.leader, "/api/v1/events/"+itoa(series.ID)+"/room", map[string]any{"roomId": f.hall.ID})
	roomPickOK(t, f.leader, f.path(f.eventA, "/room"), map[string]any{"roomId": chapel.ID})
	other := func() string {
		var b []models.RoomBooking
		gormMust(t, f.app.DB.Where("occurrence_id IN (SELECT id FROM event_occurrences WHERE event_id <> ?)", series.ID).Order("id").Find(&b))
		out, _ := json.Marshal(b)
		return string(out)
	}
	before := other()
	seriesPickFrom(t, f.leader, series, seriesRoomsRead(t, f.leader, series), map[string]any{"roomId": nil, "includeDates": []string{feedDate(f.startOn)}})
	if after := other(); after != before {
		t.Fatalf("another event's bookings changed:\n%s\nwas\n%s", after, before)
	}
}
