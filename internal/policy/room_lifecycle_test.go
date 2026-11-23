package policy

import (
	"reflect"
	"slices"
	"testing"
)

var (
	bookingStatuses = []string{BookingConfirmed, BookingRequested, BookingRejected}
	requestStatuses = []string{RoomRequestPending, RoomRequestConfirmed, RoomRequestRejected, RoomRequestWithdrawn}
)

// TestRoomBookingStateOf pins every combination of the stored facts,
// including the ones no writer leaves behind (marked unwritten).
func TestRoomBookingStateOf(t *testing.T) {
	for _, tc := range []struct {
		status           string
		pending, overlap bool
		want             RoomBookingState
		unwritten        bool
	}{
		{BookingConfirmed, false, false, RoomBookingConfirmed, false},
		{BookingConfirmed, false, true, RoomBookingDoubleBooked, false},
		{BookingConfirmed, true, false, RoomBookingConfirmed, true},
		{BookingConfirmed, true, true, RoomBookingDoubleBooked, true},
		{BookingRequested, true, false, RoomBookingWaiting, false},
		{BookingRequested, false, false, RoomBookingLost, false},
		{BookingRequested, true, true, RoomBookingWaiting, true},
		{BookingRequested, false, true, RoomBookingLost, true},
		{BookingRejected, false, false, RoomBookingRejected, false},
		{BookingRejected, true, false, RoomBookingRejected, true},
		{BookingRejected, false, true, RoomBookingRejected, true},
		{BookingRejected, true, true, RoomBookingRejected, true},
		{"held", false, false, "", true},
	} {
		if got := RoomBookingStateOf(tc.status, tc.pending, tc.overlap); got != tc.want {
			t.Errorf("RoomBookingStateOf(%s, pending %v, overlap %v) = %q, want %q", tc.status, tc.pending, tc.overlap, got, tc.want)
		}
		if tc.want != "" && tc.want.Status() != tc.status {
			t.Errorf("%q stores %q, but came from %q", tc.want, tc.want.Status(), tc.status)
		}
	}
}

func TestRoomBookingStatesAreTheStatuses(t *testing.T) {
	stored := map[string]bool{}
	for _, s := range RoomBookingStates() {
		if !slices.Contains(bookingStatuses, s.Status()) {
			t.Errorf("%q stores %q, which is not a booking status", s, s.Status())
		}
		stored[s.Status()] = true
	}
	for _, st := range bookingStatuses {
		if !stored[st] {
			t.Errorf("no state stores %q", st)
		}
	}
	if RoomBookingState("held").Status() != "" {
		t.Error("an unknown state stores a status")
	}
}

// HoldsRoom is the status test of rooms.HeldCondition: a confirmed
// booking holds the room, whatever its request and allow_overlap.
func TestRoomBookingHoldsRoom(t *testing.T) {
	for _, st := range bookingStatuses {
		for _, pending := range []bool{false, true} {
			for _, overlap := range []bool{false, true} {
				if got, want := RoomBookingStateOf(st, pending, overlap).HoldsRoom(), st == BookingConfirmed; got != want {
					t.Errorf("%s (pending %v, overlap %v) holds the room: %v, want %v", st, pending, overlap, got, want)
				}
			}
		}
	}
}

// TestRoomBookingMoves pins every (move, from) pair: the state each move
// ends in from each state, "" where it may not start, and "kept" or "gone".
func TestRoomBookingMoves(t *testing.T) {
	const kept, gone = "kept", "gone"
	all := func(to RoomBookingState) map[RoomBookingState]RoomBookingState {
		out := map[RoomBookingState]RoomBookingState{}
		for _, s := range RoomBookingStates() {
			out[s] = to
		}
		return out
	}
	want := map[RoomBookingMove]map[RoomBookingState]RoomBookingState{
		BookingPlace:          {},
		BookingPlaceOverlap:   {},
		BookingPlaceBusy:      {},
		BookingAwaitApproval:  {RoomBookingConfirmed: RoomBookingWaiting},
		BookingDateMoved:      all(kept),
		BookingDateMovedBusy:  {RoomBookingConfirmed: RoomBookingLost},
		BookingConfirm:        {RoomBookingWaiting: RoomBookingConfirmed},
		BookingConfirmOverlap: {RoomBookingWaiting: RoomBookingDoubleBooked},
		BookingCannotConfirm:  {RoomBookingWaiting: RoomBookingLost},
		BookingReject:         {RoomBookingWaiting: RoomBookingRejected},
		BookingRequestClosed:  {RoomBookingWaiting: RoomBookingLost},
		BookingRelease:        all(gone),
	}
	moves := RoomBookingMoves()
	if len(moves) != len(want) {
		t.Fatalf("the table has %d moves, the test %d: %v", len(moves), len(want), moves)
	}
	for _, m := range moves {
		cases, ok := want[m]
		if !ok {
			t.Errorf("%q has no row in this test", m)
			continue
		}
		for _, from := range RoomBookingStates() {
			var got RoomBookingState
			if m.Allows(from) {
				switch {
				case m.Removes():
					got = gone
				case m.To() == "":
					got = kept
				default:
					got = m.To()
				}
			}
			if got != cases[from] {
				t.Errorf("%q from %s: got %q, want %q", m, from, got, cases[from])
			}
		}
	}
}

// Each move is exactly one kind of write: an insert, an update of the
// booking (changeBookings), a delete, or a move of its request alone.
func TestRoomBookingMoveWrites(t *testing.T) {
	creates := map[RoomBookingMove]RoomBookingState{
		BookingPlace: RoomBookingConfirmed, BookingPlaceOverlap: RoomBookingDoubleBooked, BookingPlaceBusy: RoomBookingLost,
	}
	updates := []RoomBookingMove{BookingAwaitApproval, BookingDateMoved, BookingDateMovedBusy, BookingConfirm,
		BookingConfirmOverlap, BookingCannotConfirm, BookingReject}
	for _, m := range RoomBookingMoves() {
		to, creating := creates[m]
		if m.Creates() != creating || (creating && (len(m.From()) != 0 || m.To() != to)) {
			t.Errorf("%q: creates %v from %v to %q", m, m.Creates(), m.From(), m.To())
		}
		if got, want := m.Updates(), slices.Contains(updates, m); got != want {
			t.Errorf("%q updates a booking: %v, want %v", m, got, want)
		}
		if got, want := m.Removes(), m == BookingRelease; got != want {
			t.Errorf("%q removes a booking: %v, want %v", m, got, want)
		}
		if !m.Creates() && len(m.From()) == 0 {
			t.Errorf("%q starts from nothing but makes no row", m)
		}
	}
}

func TestRoomBookingFromStatuses(t *testing.T) {
	for m, want := range map[RoomBookingMove][]string{
		BookingPlace:          nil,
		BookingAwaitApproval:  {BookingConfirmed},
		BookingDateMoved:      {BookingConfirmed, BookingRejected, BookingRequested},
		BookingDateMovedBusy:  {BookingConfirmed},
		BookingConfirm:        {BookingRequested},
		BookingConfirmOverlap: {BookingRequested},
		BookingCannotConfirm:  {BookingRequested},
		BookingReject:         {BookingRequested},
	} {
		if got := m.FromStatuses(); !reflect.DeepEqual(got, want) {
			t.Errorf("%q from statuses %v, want %v", m, got, want)
		}
	}
}

func TestUnknownRoomMovesAllowNothing(t *testing.T) {
	b := RoomBookingMove("hold")
	for _, s := range RoomBookingStates() {
		if b.Allows(s) {
			t.Errorf("an unknown booking move starts from %s", s)
		}
	}
	if b.To() != "" || len(b.From()) != 0 || b.Creates() || b.Updates() || b.Removes() || len(b.FromStatuses()) != 0 {
		t.Error("an unknown booking move does something")
	}
	r := RoomRequestMove("approve")
	for _, s := range requestStatuses {
		if r.Allows(s) {
			t.Errorf("an unknown request move starts from %s", s)
		}
	}
	if r.To() != "" || len(r.From()) != 0 {
		t.Error("an unknown request move does something")
	}
}

func TestRoomMoveFromIsACopy(t *testing.T) {
	from := BookingConfirm.From()
	from[0] = RoomBookingLost
	if !reflect.DeepEqual(BookingConfirm.From(), []RoomBookingState{RoomBookingWaiting}) {
		t.Fatal("changing From's result changed the booking table")
	}
	rf := RoomRequestConfirm.From()
	rf[0] = RoomRequestWithdrawn
	if !reflect.DeepEqual(RoomRequestConfirm.From(), []string{RoomRequestPending}) {
		t.Fatal("changing From's result changed the request table")
	}
}

// TestRoomRequestMoves pins every (move, from) pair; "gone" is a delete.
func TestRoomRequestMoves(t *testing.T) {
	const gone = "gone"
	want := map[RoomRequestMove]map[string]string{
		RoomRequestOpen:           {},
		RoomRequestConfirm:        {RoomRequestPending: RoomRequestConfirmed},
		RoomRequestReject:         {RoomRequestPending: RoomRequestRejected},
		RoomRequestWithdrawEmpty:  {RoomRequestPending: RoomRequestWithdrawn},
		RoomRequestWithdrawSeries: {RoomRequestPending: RoomRequestWithdrawn},
		RoomRequestDropUnused:     {RoomRequestPending: gone},
	}
	moves := RoomRequestMoves()
	if len(moves) != len(want) {
		t.Fatalf("the table has %d moves, the test %d: %v", len(moves), len(want), moves)
	}
	for _, m := range moves {
		cases, ok := want[m]
		if !ok {
			t.Errorf("%q has no row in this test", m)
			continue
		}
		for _, from := range requestStatuses {
			got := ""
			if m.Allows(from) {
				if got = m.To(); got == "" {
					got = gone
				}
			}
			if got != cases[from] {
				t.Errorf("%q from %s: got %q, want %q", m, from, got, cases[from])
			}
		}
	}
	if len(RoomRequestOpen.From()) != 0 || RoomRequestOpen.To() != RoomRequestPending {
		t.Errorf("opening a request: from %v to %q", RoomRequestOpen.From(), RoomRequestOpen.To())
	}
}

// RoomRequestUndecided is the status test lockRequest and dateBooking made
// by hand (status = pending), and every move of an existing request starts
// from exactly those statuses.
func TestRoomRequestUndecided(t *testing.T) {
	for _, s := range requestStatuses {
		if got, want := RoomRequestUndecided(s), s == RoomRequestPending; got != want {
			t.Errorf("RoomRequestUndecided(%s) = %v, want %v", s, got, want)
		}
		for _, m := range RoomRequestMoves() {
			if len(m.From()) > 0 && m.Allows(s) != RoomRequestUndecided(s) {
				t.Errorf("%q from %s: %v, but undecided is %v", m, s, m.Allows(s), RoomRequestUndecided(s))
			}
		}
	}
}

// Every status the request table names is a request status, and every one
// is named; root's TestRoomStatusesMatchTheSchema ties the lists to the
// CHECKs.
func TestRoomRequestMovesNameEveryStatus(t *testing.T) {
	named := map[string]bool{}
	for _, m := range RoomRequestMoves() {
		for _, s := range append(m.From(), m.To()) {
			if s == "" {
				continue
			}
			if !slices.Contains(requestStatuses, s) {
				t.Errorf("%q names %q, which is not a request status", m, s)
			}
			named[s] = true
		}
	}
	for _, s := range requestStatuses {
		if !named[s] {
			t.Errorf("no move names %q", s)
		}
	}
}
