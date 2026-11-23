package main

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"testing"
	"time"

	"fold/internal/policy"
	"fold/internal/rooms"
	"fold/models"

	"gorm.io/gorm"
)

func TestRoomStatusesMatchTheSchema(t *testing.T) {
	checks := enumeratedChecks(t)
	var bookings []string
	for _, s := range policy.RoomBookingStates() {
		if !slices.Contains(bookings, s.Status()) {
			bookings = append(bookings, s.Status())
		}
	}
	slices.Sort(bookings)
	consts := []string{models.BookingConfirmed, models.BookingRequested, models.BookingRejected}
	slices.Sort(consts)
	if len(checks["room_bookings.status"]) == 0 || !reflect.DeepEqual(bookings, checks["room_bookings.status"]) || !reflect.DeepEqual(consts, checks["room_bookings.status"]) {
		t.Errorf("room_bookings_status_check allows %v; the states store %v and models names %v", checks["room_bookings.status"], bookings, consts)
	}
	named := map[string]bool{}
	for _, m := range policy.RoomRequestMoves() {
		for _, s := range append(m.From(), m.To()) {
			if s != "" {
				named[s] = true
			}
		}
	}
	var requests []string
	for s := range named {
		requests = append(requests, s)
	}
	slices.Sort(requests)
	rconsts := []string{models.RoomRequestPending, models.RoomRequestConfirmed, models.RoomRequestRejected, models.RoomRequestWithdrawn}
	slices.Sort(rconsts)
	if len(checks["room_requests.status"]) == 0 || !reflect.DeepEqual(requests, checks["room_requests.status"]) || !reflect.DeepEqual(rconsts, checks["room_requests.status"]) {
		t.Errorf("room_requests_status_check allows %v; the moves name %v and models names %v", checks["room_requests.status"], requests, rconsts)
	}
}

// roomCombo is one booking with its own room and its own event's one date,
// so no two of them share a room and the exclusion constraint never meets.
type roomCombo struct {
	status, request string // request "" is no request_id
	overlap         bool
	room            models.Room
	ev              eventJSON
	occ             models.EventOccurrence
	bookingID       uint
	requestID       *uint
}

func (c roomCombo) String() string {
	return fmt.Sprintf("%s booking, request %q, overlap %v", c.status, c.request, c.overlap)
}

func (c roomCombo) state() policy.RoomBookingState {
	return policy.RoomBookingStateOf(c.status, c.request == models.RoomRequestPending, c.overlap)
}

// roomCombos writes a booking in every combination the schema allows of
// status, the request it names (none, or one in each request status) and
// allow_overlap, all at f.startOn. Rows go in by SQL because most of these
// combinations have no writer.
func roomCombos(t *testing.T, f bookingFixture) []roomCombo {
	t.Helper()
	var out []roomCombo
	for _, status := range []string{models.BookingConfirmed, models.BookingRequested, models.BookingRejected} {
		for _, request := range []string{"", models.RoomRequestPending, models.RoomRequestConfirmed, models.RoomRequestRejected, models.RoomRequestWithdrawn} {
			for _, overlap := range []bool{false, true} {
				c := roomCombo{status: status, request: request, overlap: overlap}
				n := len(out)
				c.room = models.Room{Name: fmt.Sprintf("Combo room %d", n), Active: true}
				if err := f.app.DB.Create(&c.room).Error; err != nil {
					t.Fatal(err)
				}
				c.ev = createFeedEvent(t, f.leader, f.group, fmt.Sprintf("Combo %d", n), f.startOn, "")
				if err := f.app.DB.Where("event_id = ?", c.ev.ID).First(&c.occ).Error; err != nil {
					t.Fatal(err)
				}
				if request != "" {
					req := models.RoomRequest{EventID: c.ev.ID, RoomID: c.room.ID, Status: request}
					if err := f.app.DB.Create(&req).Error; err != nil {
						t.Fatal(err)
					}
					c.requestID = &req.ID
				}
				why := ""
				if overlap {
					why = "Shared night"
				}
				if err := f.app.DB.Raw(`INSERT INTO room_bookings (occurrence_id, room_id, starts_at, ends_at, padded_range, status, allow_overlap, overlap_reason, request_id)
					VALUES (?, ?, ?, ?, `+paddedRangeSQL+`, ?, ?, ?, ?) RETURNING id`,
					c.occ.ID, c.room.ID, c.occ.StartsAt, c.occ.EndsAt, c.occ.StartsAt, 0, c.occ.EndsAt, 0,
					status, overlap, why, c.requestID).Scan(&c.bookingID).Error; err != nil {
					t.Fatal(err)
				}
				out = append(out, c)
			}
		}
	}
	return out
}

var errRoomTestRollback = errors.New("roll back")

// inRollback runs fn in a transaction that is then rolled back.
func inRollback(t *testing.T, db *gorm.DB, fn func(tx *gorm.DB)) {
	t.Helper()
	if err := db.Transaction(func(tx *gorm.DB) error { fn(tx); return errRoomTestRollback }); !errors.Is(err, errRoomTestRollback) {
		t.Fatal(err)
	}
}

func bookingRow(t *testing.T, tx *gorm.DB, id uint) models.RoomBooking {
	t.Helper()
	var b models.RoomBooking
	if err := tx.First(&b, id).Error; err != nil {
		t.Fatal(err)
	}
	return b
}

// changeBookings' SQL moves a row exactly when its status is one of the
// move's from-statuses, to the status of the move's end state (a kept state
// keeps it), writing the caller's columns with it; a false extra condition
// moves nothing.
func TestChangeBookingsSQLMatchesTheRule(t *testing.T) {
	f := newBookingFixture(t)
	combos := roomCombos(t, f)
	by := f.leaderUser.ID
	for _, move := range policy.RoomBookingMoves() {
		if !move.Updates() {
			continue
		}
		for _, c := range combos {
			inRollback(t, f.app.DB, func(tx *gorm.DB) {
				if n, err := changeBookings(tx, move, "created_by = ?", []any{by}, "id = ? AND false", c.bookingID); err != nil || n != 0 {
					t.Fatalf("%q on %v with a false condition: moved %d, %v", move, c, n, err)
				}
				n, err := changeBookings(tx, move, "created_by = ?", []any{by}, "id = ?", c.bookingID)
				if err != nil {
					t.Fatalf("%q on %v: %v", move, c, err)
				}
				allowed := slices.Contains(move.FromStatuses(), c.status)
				want := c.status
				if allowed && move.To() != "" {
					want = move.To().Status()
				}
				b := bookingRow(t, tx, c.bookingID)
				if (n == 1) != allowed || b.Status != want || (b.CreatedBy != nil) != allowed {
					t.Errorf("%q on %v: moved %d to %s (created_by %v), want moved %v to %s", move, c, n, b.Status, b.CreatedBy, allowed, want)
				}
			})
		}
	}
}

// The set form counts exactly the matching rows in the from-statuses, as
// the rejection's audit row reports them.
func TestChangeBookingsCountsTheRowsItMoves(t *testing.T) {
	f := newBookingFixture(t)
	combos := roomCombos(t, f)
	inRollback(t, f.app.DB, func(tx *gorm.DB) {
		n, err := changeBookings(tx, policy.BookingReject, "", nil, "request_id IS NOT NULL")
		if err != nil {
			t.Fatal(err)
		}
		want := int64(0)
		for _, c := range combos {
			if c.requestID != nil && c.status == models.BookingRequested {
				want++
			}
		}
		if n != want {
			t.Fatalf("rejected %d rows, want %d", n, want)
		}
	})
}

func TestRoomStatusWritersRefuseWhatTheRuleDoesNotHold(t *testing.T) {
	f := newBookingFixture(t)
	if code, body := f.book(t, f.leader, f.eventA, map[string]any{"roomId": f.hall.ID}); code != http.StatusOK {
		t.Fatalf("book A: %d %s", code, body)
	}
	var c struct {
		bookingID uint
		status    string
	}
	if err := f.app.DB.Raw("SELECT id AS booking_id, status FROM room_bookings").Row().Scan(&c.bookingID, &c.status); err != nil {
		t.Fatal(err)
	}
	req := models.RoomRequest{EventID: f.eventA.ID, RoomID: f.hall.ID, Status: models.RoomRequestPending}
	if err := f.app.DB.Create(&req).Error; err != nil {
		t.Fatal(err)
	}
	for name, try := range map[string]func(tx *gorm.DB) error{
		"a booking insert": func(tx *gorm.DB) error {
			_, err := changeBookings(tx, policy.BookingPlace, "", nil, "id = ?", c.bookingID)
			return err
		},
		"a release": func(tx *gorm.DB) error {
			_, err := changeBookings(tx, policy.BookingRelease, "", nil, "id = ?", c.bookingID)
			return err
		},
		"a move of the request alone": func(tx *gorm.DB) error {
			_, err := changeBookings(tx, policy.BookingRequestClosed, "", nil, "id = ?", c.bookingID)
			return err
		},
		"an unknown booking move": func(tx *gorm.DB) error {
			_, err := changeBookings(tx, policy.RoomBookingMove("hold"), "", nil, "id = ?", c.bookingID)
			return err
		},
		"a status in the caller's columns": func(tx *gorm.DB) error {
			_, err := changeBookings(tx, policy.BookingDateMoved, "overlap_reason = '', status = 'rejected'", nil, "id = ?", c.bookingID)
			return err
		},
		"opening a request": func(tx *gorm.DB) error {
			_, err := moveRoomRequest(tx, &req, policy.RoomRequestOpen, nil)
			return err
		},
		"a request delete": func(tx *gorm.DB) error {
			_, err := moveRoomRequest(tx, &req, policy.RoomRequestDropUnused, nil)
			return err
		},
		"an unknown request move": func(tx *gorm.DB) error {
			_, err := moveRoomRequest(tx, &req, policy.RoomRequestMove("approve"), nil)
			return err
		},
		"a status in the request's fields": func(tx *gorm.DB) error {
			_, err := moveRoomRequest(tx, &req, policy.RoomRequestConfirm, map[string]any{"status": models.RoomRequestRejected})
			return err
		},
		"opening requests as a set": func(tx *gorm.DB) error {
			_, err := moveRoomRequestsWhere(tx, policy.RoomRequestOpen, "id = ?", req.ID)
			return err
		},
	} {
		inRollback(t, f.app.DB, func(tx *gorm.DB) {
			if try(tx) == nil {
				t.Errorf("%s was written", name)
			}
		})
	}
	if b := bookingRow(t, f.app.DB, c.bookingID); b.Status != c.status {
		t.Fatalf("the booking is now %s", b.Status)
	}
}

// requestsInEveryStatus writes one request in each status for eventID.
func requestsInEveryStatus(t *testing.T, db *gorm.DB, eventID, roomID uint) []models.RoomRequest {
	t.Helper()
	var out []models.RoomRequest
	for _, s := range []string{models.RoomRequestPending, models.RoomRequestConfirmed, models.RoomRequestRejected, models.RoomRequestWithdrawn} {
		r := models.RoomRequest{EventID: eventID, RoomID: roomID, Status: s}
		if err := db.Create(&r).Error; err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func requestStatusIn(t *testing.T, tx *gorm.DB, id uint) string {
	t.Helper()
	var r models.RoomRequest
	if err := tx.First(&r, id).Error; err != nil {
		t.Fatal(err)
	}
	return r.Status
}

// moveRoomRequest moves a request exactly when the move allows its status,
// writing the move's status and the caller's fields together; the set form
// moves exactly the matching requests it allows and returns their ids.
func TestRoomRequestWritersMatchTheRule(t *testing.T) {
	f := newBookingFixture(t)
	for _, move := range policy.RoomRequestMoves() {
		if len(move.From()) == 0 || move.To() == "" {
			continue
		}
		rows := requestsInEveryStatus(t, f.app.DB, f.eventA.ID, f.hall.ID)
		bystanders := requestsInEveryStatus(t, f.app.DB, f.eventB.ID, f.hall.ID)
		inRollback(t, f.app.DB, func(tx *gorm.DB) {
			for _, r := range rows {
				from := r.Status
				moved, err := moveRoomRequest(tx, &r, move, map[string]any{"reject_reason": "moved"})
				if err != nil {
					t.Fatal(err)
				}
				want := from
				if move.Allows(from) {
					want = move.To()
				}
				if got := requestStatusIn(t, tx, r.ID); moved != move.Allows(from) || got != want {
					t.Errorf("%q from %s: moved %v to %s, want moved %v to %s", move, from, moved, got, move.Allows(from), want)
				}
			}
		})
		inRollback(t, f.app.DB, func(tx *gorm.DB) {
			ids, err := moveRoomRequestsWhere(tx, move, "event_id = ?", f.eventA.ID)
			if err != nil {
				t.Fatal(err)
			}
			var want []uint
			for _, r := range rows {
				if move.Allows(r.Status) {
					want = append(want, r.ID)
				}
			}
			slices.Sort(ids)
			if !reflect.DeepEqual(ids, want) {
				t.Errorf("%q moved %v, want %v", move, ids, want)
			}
			for _, r := range append(append([]models.RoomRequest{}, rows...), bystanders...) {
				want := r.Status
				if r.EventID == f.eventA.ID && move.Allows(r.Status) {
					want = move.To()
				}
				if got := requestStatusIn(t, tx, r.ID); got != want {
					t.Errorf("%q: event %d's %s request is now %s, want %s", move, r.EventID, r.Status, got, want)
				}
			}
		})
		if err := f.app.DB.Where("event_id IN ?", []uint{f.eventA.ID, f.eventB.ID}).Delete(&models.RoomRequest{}).Error; err != nil {
			t.Fatal(err)
		}
	}
}

// TestRoomBookingStateSQLMatchesGo runs every reader that picks bookings or
// requests by status in SQL over a booking in every combination, and checks
// it picks exactly what policy.RoomBookingStateOf says.
func TestRoomBookingStateSQLMatchesGo(t *testing.T) {
	f := newBookingFixture(t)
	if err := f.app.DB.Where("key = ?", settingTimezone).Assign(models.Setting{Key: settingTimezone, Value: "UTC"}).
		FirstOrCreate(&models.Setting{}).Error; err != nil {
		t.Fatal(err)
	}
	combos := roomCombos(t, f)
	byRoom := map[uint]roomCombo{}
	var roomIDs, occIDs []uint
	for _, c := range combos {
		byRoom[c.room.ID] = c
		roomIDs = append(roomIDs, c.room.ID)
		occIDs = append(occIDs, c.occ.ID)
	}
	start, end := combos[0].occ.StartsAt, combos[0].occ.EndsAt

	t.Run("a date's booking", func(t *testing.T) {
		for _, c := range combos {
			b, err := f.app.dateBooking(f.app.DB, c.occ.ID, 0)
			if err != nil {
				t.Fatal(err)
			}
			if b.Pending != (c.state() == policy.RoomBookingWaiting) || b.Status != c.status || b.DoubleBooked != c.overlap {
				t.Errorf("%v (%s): %+v", c, c.state(), b)
			}
		}
	})

	t.Run("the room calendar", func(t *testing.T) {
		for _, viewer := range []struct {
			name  string
			c     *apiClient
			shown func(policy.RoomBookingState) bool
		}{
			{"admin", f.admin, func(s policy.RoomBookingState) bool { return s.HoldsRoom() || s == policy.RoomBookingWaiting }},
			{"leader", f.leader, policy.RoomBookingState.HoldsRoom},
		} {
			w := viewer.c.do("GET", "/api/v1/calendar/rooms?view=day&date="+feedDate(f.startOn), nil, false)
			var cal roomCalendarJSON
			decodeJSON(t, w, &cal)
			seen := map[uint]bool{}
			for _, r := range cal.Rooms {
				c, ok := byRoom[r.ID]
				if !ok {
					continue
				}
				seen[r.ID] = true
				if shown := len(r.Bookings) == 1; shown != viewer.shown(c.state()) || len(r.Bookings) > 1 {
					t.Errorf("%s sees %v (%s): %d slots", viewer.name, c, c.state(), len(r.Bookings))
					continue
				}
				if len(r.Bookings) == 1 && r.Bookings[0].Waiting != (c.state() == policy.RoomBookingWaiting) {
					t.Errorf("%s sees %v (%s) with waiting %v", viewer.name, c, c.state(), r.Bookings[0].Waiting)
				}
			}
			if len(seen) != len(combos) {
				t.Errorf("%s's calendar listed %d of the %d rooms", viewer.name, len(seen), len(combos))
			}
		}
	})

	t.Run("a date's place", func(t *testing.T) {
		shown, err := roomsFor(f.app.DB, occIDs)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range combos {
			if _, ok := shown[c.occ.ID]; ok != c.state().HoldsRoom() {
				t.Errorf("%v (%s) is the date's place: %v", c, c.state(), ok)
			}
		}
	})

	t.Run("the room is busy", func(t *testing.T) {
		busy, err := rooms.BusyCounts(f.app.DB, roomIDs, []rooms.Window{{Start: start, End: end}}, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range combos {
			if (busy[c.room.ID] > 0) != c.state().HoldsRoom() {
				t.Errorf("%v (%s) makes the room busy: %d", c, c.state(), busy[c.room.ID])
			}
			held, err := heldByOverlap(f.app.DB, c.room.ID, 0, start, end, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			if held != (c.state() == policy.RoomBookingDoubleBooked) {
				t.Errorf("%v (%s) is a double-booking holding the room: %v", c, c.state(), held)
			}
		}
	})

	t.Run("the room feed", func(t *testing.T) {
		for _, c := range combos {
			feed, err := f.app.buildRoomFeedIn(f.app.DB, &c.room, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if (len(feed.Singles) == 1) != c.state().HoldsRoom() || len(feed.Singles) > 1 {
				t.Errorf("%v (%s) is in the room's feed %d times", c, c.state(), len(feed.Singles))
			}
		}
	})

	t.Run("the admins' request list", func(t *testing.T) {
		listed := map[uint]int{}
		for _, r := range listRequests(t, f.admin) {
			listed[r.ID] = r.Dates
		}
		for _, c := range combos {
			if c.requestID == nil {
				continue
			}
			if want := map[bool]int{true: 1}[c.state() == policy.RoomBookingWaiting]; listed[*c.requestID] != want {
				t.Errorf("%v (%s): its request lists %d dates, want %d", c, c.state(), listed[*c.requestID], want)
			}
		}
	})

	t.Run("a request's dates", func(t *testing.T) {
		for _, c := range combos {
			if c.request != models.RoomRequestPending {
				continue // lockRequest decides only pending requests
			}
			inRollback(t, f.app.DB, func(tx *gorm.DB) {
				var req models.RoomRequest
				if err := tx.First(&req, *c.requestID).Error; err != nil {
					t.Fatal(err)
				}
				bookings, _, err := requestBookings(tx, &req)
				if err != nil {
					t.Fatal(err)
				}
				if (len(bookings) == 1) != (c.state() == policy.RoomBookingWaiting) {
					t.Errorf("%v (%s): the request has %d dates", c, c.state(), len(bookings))
				}
			})
		}
	})

	t.Run("an emptied request is withdrawn", func(t *testing.T) {
		inRollback(t, f.app.DB, func(tx *gorm.DB) {
			if err := closeEmptyRoomRequests(tx); err != nil {
				t.Fatal(err)
			}
			for _, c := range combos {
				if c.requestID == nil {
					continue
				}
				want := c.request
				if policy.RoomRequestUndecided(c.request) && c.state() != policy.RoomBookingWaiting {
					want = models.RoomRequestWithdrawn
				}
				if got := requestStatusIn(t, tx, *c.requestID); got != want {
					t.Errorf("%v (%s): its request is now %s, want %s", c, c.state(), got, want)
				}
			}
		})
	})

	// The series picker counts a date as waiting by its request alone, so a
	// rejected booking whose request is still pending would count as waiting
	// where every other reader calls it rejected. No writer leaves that
	// combination (a rejection moves the request in the same transaction);
	// this pins how the picker reads it.
	t.Run("the series picker", func(t *testing.T) {
		for _, c := range combos {
			var out seriesRoomsJSON
			decodeJSON(t, f.leader.do("GET", "/api/v1/events/"+itoa(c.ev.ID)+"/rooms", nil, false), &out)
			wantPending, wantNeeds := 0, 0
			switch s := c.state(); {
			case s == policy.RoomBookingWaiting, s == policy.RoomBookingRejected && c.request == models.RoomRequestPending:
				wantPending = 1
			case s == policy.RoomBookingLost, s == policy.RoomBookingRejected:
				wantNeeds = 1
			}
			if out.Pending != wantPending || len(out.NeedsRoom) != wantNeeds {
				t.Errorf("%v (%s): pending %d, needs a room %v; want %d and %d", c, c.state(), out.Pending, out.NeedsRoom, wantPending, wantNeeds)
			}
			if wantNeeds == 1 && len(out.NeedsRoom) == 1 && out.NeedsRoom[0].Status != c.status {
				t.Errorf("%v: needs a room as %q", c, out.NeedsRoom[0].Status)
			}
		}
	})
}
