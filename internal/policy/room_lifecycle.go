package policy

import "slices"

// Room booking statuses (9.2): the values of room_bookings_status_check.
const (
	BookingConfirmed = "confirmed"
	BookingRequested = "requested"
	BookingRejected  = "rejected"
)

// Room request statuses (M2.1d): the values of room_requests_status_check.
const (
	RoomRequestPending   = "pending"
	RoomRequestConfirmed = "confirmed"
	RoomRequestRejected  = "rejected"
	RoomRequestWithdrawn = "withdrawn"
)

// RoomBookingState is what a booking does, which its status alone does not
// say: a requested booking waits while its request is pending and is lost
// otherwise, and a confirmed one may be an admin's double-booking. The
// table of moves below is the machine (docs/agents/lifecycles.md).
type RoomBookingState string

const (
	// RoomBookingConfirmed holds the room, under room_bookings_no_overlap.
	RoomBookingConfirmed RoomBookingState = "confirmed"
	// RoomBookingDoubleBooked holds the room beside another booking, with an
	// admin's reason (allow_overlap), outside the exclusion constraint.
	RoomBookingDoubleBooked RoomBookingState = "double-booked"
	// RoomBookingWaiting holds nothing until an admin decides its request
	// (DEC-102). Only admins see it on the room calendar (DEC-109).
	RoomBookingWaiting RoomBookingState = "waiting"
	// RoomBookingLost holds nothing and is the leader's to re-pick: its date
	// moved into a conflict, the room was busy, or its request closed.
	RoomBookingLost RoomBookingState = "lost"
	// RoomBookingRejected holds nothing: an admin didn't approve it.
	RoomBookingRejected RoomBookingState = "rejected"
)

func RoomBookingStates() []RoomBookingState {
	return []RoomBookingState{RoomBookingConfirmed, RoomBookingDoubleBooked, RoomBookingWaiting, RoomBookingLost, RoomBookingRejected}
}

// RoomBookingStateOf names a booking's state from what is stored: its
// status, whether request_id names a room request that is still pending,
// and allow_overlap. The request matters only to a requested booking and
// allow_overlap only to a confirmed one, as in every reader: the exclusion
// constraint and heldByOverlap read confirmed rows alone. So the combinations
// no writer leaves behind map as follows: confirmed with a pending request (a booking between its
// insert and its move to waiting, or confirmed before its request's own row
// moves) is confirmed; requested with allow_overlap is waiting or lost;
// rejected with a pending request or allow_overlap is rejected. An unknown
// status is "".
func RoomBookingStateOf(status string, requestPending, allowOverlap bool) RoomBookingState {
	switch status {
	case BookingConfirmed:
		if allowOverlap {
			return RoomBookingDoubleBooked
		}
		return RoomBookingConfirmed
	case BookingRequested:
		if requestPending {
			return RoomBookingWaiting
		}
		return RoomBookingLost
	case BookingRejected:
		return RoomBookingRejected
	}
	return ""
}

// Status is the room_bookings.status a booking in s stores.
func (s RoomBookingState) Status() string {
	switch s {
	case RoomBookingConfirmed, RoomBookingDoubleBooked:
		return BookingConfirmed
	case RoomBookingWaiting, RoomBookingLost:
		return BookingRequested
	case RoomBookingRejected:
		return BookingRejected
	}
	return ""
}

// HoldsRoom: the booking keeps the room from everyone else. Its SQL form is
// the status test in rooms.HeldCondition.
func (s RoomBookingState) HoldsRoom() bool {
	return s == RoomBookingConfirmed || s == RoomBookingDoubleBooked
}

// RoomBookingMove is one way a booking's state changes, named by what causes
// it. The root package's changeBookings writes every move of an existing
// row; a move that makes the row is the status its insert writes; a release
// is a delete; and a move the request makes changes no booking row.
type RoomBookingMove string

const (
	BookingPlace          RoomBookingMove = "place a booking"
	BookingPlaceOverlap   RoomBookingMove = "double-book with a reason"
	BookingPlaceBusy      RoomBookingMove = "keep a busy date"
	BookingAwaitApproval  RoomBookingMove = "wait for an admin's approval"
	BookingDateMoved      RoomBookingMove = "its date moved"
	BookingDateMovedBusy  RoomBookingMove = "its date moved into a conflict"
	BookingConfirm        RoomBookingMove = "an admin confirms"
	BookingConfirmOverlap RoomBookingMove = "an admin confirms over a conflict"
	BookingCannotConfirm  RoomBookingMove = "an admin confirms a busy date"
	BookingReject         RoomBookingMove = "an admin rejects"
	BookingRequestClosed  RoomBookingMove = "its request is withdrawn or deleted"
	BookingRelease        RoomBookingMove = "release"
)

type bookingWrite int

const (
	insertsBooking bookingWrite = iota
	updatesBooking
	deletesBooking
	writesTheRequest
)

type bookingRule struct {
	from []RoomBookingState
	// to is "" for a move that keeps the state, and for a release.
	to    RoomBookingState
	write bookingWrite
}

// Recorded as built, odd moves included. Waiting for approval starts from
// confirmed because the insert is what proves the room free (placeBooking).
// A date that moves keeps whatever state it had, a waiting or rejected one
// included; only a confirmed one can lose its room, since a double-booking
// moves as the admin chose. An admin who confirms a date that became busy,
// with no reason, leaves it lost (request_id cleared). A waiting booking
// also turns lost with no write to it: a new series pick withdraws its
// request (withdrawSeriesRequest), or the request row is deleted and the
// foreign key clears request_id.
var bookingRules = map[RoomBookingMove]bookingRule{
	BookingPlace:          {to: RoomBookingConfirmed, write: insertsBooking},
	BookingPlaceOverlap:   {to: RoomBookingDoubleBooked, write: insertsBooking},
	BookingPlaceBusy:      {to: RoomBookingLost, write: insertsBooking},
	BookingAwaitApproval:  {from: []RoomBookingState{RoomBookingConfirmed}, to: RoomBookingWaiting, write: updatesBooking},
	BookingDateMoved:      {from: RoomBookingStates(), write: updatesBooking},
	BookingDateMovedBusy:  {from: []RoomBookingState{RoomBookingConfirmed}, to: RoomBookingLost, write: updatesBooking},
	BookingConfirm:        {from: []RoomBookingState{RoomBookingWaiting}, to: RoomBookingConfirmed, write: updatesBooking},
	BookingConfirmOverlap: {from: []RoomBookingState{RoomBookingWaiting}, to: RoomBookingDoubleBooked, write: updatesBooking},
	BookingCannotConfirm:  {from: []RoomBookingState{RoomBookingWaiting}, to: RoomBookingLost, write: updatesBooking},
	BookingReject:         {from: []RoomBookingState{RoomBookingWaiting}, to: RoomBookingRejected, write: updatesBooking},
	BookingRequestClosed:  {from: []RoomBookingState{RoomBookingWaiting}, to: RoomBookingLost, write: writesTheRequest},
	BookingRelease:        {from: RoomBookingStates(), write: deletesBooking},
}

func RoomBookingMoves() []RoomBookingMove {
	out := make([]RoomBookingMove, 0, len(bookingRules))
	for m := range bookingRules {
		out = append(out, m)
	}
	slices.Sort(out)
	return out
}

// From is the states the move starts from; none for a move that makes the
// row, or for a name the table does not hold.
func (m RoomBookingMove) From() []RoomBookingState { return slices.Clone(bookingRules[m].from) }

// To is the state the move ends in; "" when it keeps the state, or removes
// the row.
func (m RoomBookingMove) To() RoomBookingState { return bookingRules[m].to }

func (m RoomBookingMove) Allows(s RoomBookingState) bool {
	return slices.Contains(bookingRules[m].from, s)
}

func (m RoomBookingMove) Creates() bool {
	r, ok := bookingRules[m]
	return ok && r.write == insertsBooking
}

// Updates: changeBookings writes the move.
func (m RoomBookingMove) Updates() bool {
	r, ok := bookingRules[m]
	return ok && r.write == updatesBooking
}

func (m RoomBookingMove) Removes() bool {
	r, ok := bookingRules[m]
	return ok && r.write == deletesBooking
}

// FromStatuses is the stored statuses of the move's from-states, sorted:
// what an UPDATE's WHERE can test on the row itself. Whether a requested
// booking is waiting or lost is its request's status, and whether a
// confirmed one is double-booked is allow_overlap; a caller that moves only
// some states of a status picks its rows by id or request and holds the
// locks that keep them in those states.
func (m RoomBookingMove) FromStatuses() []string {
	var out []string
	for _, s := range bookingRules[m].from {
		if st := s.Status(); !slices.Contains(out, st) {
			out = append(out, st)
		}
	}
	slices.Sort(out)
	return out
}

// RoomRequestMove is one way a room request's status changes, named by what
// causes it. The root package's moveRoomRequest and moveRoomRequestsWhere
// write every move of an existing row; opening one inserts it pending.
type RoomRequestMove string

const (
	RoomRequestOpen           RoomRequestMove = "open a request"
	RoomRequestConfirm        RoomRequestMove = "an admin confirms"
	RoomRequestReject         RoomRequestMove = "an admin rejects"
	RoomRequestWithdrawEmpty  RoomRequestMove = "no date waits on it any more"
	RoomRequestWithdrawSeries RoomRequestMove = "a new series pick replaces it"
	RoomRequestDropUnused     RoomRequestMove = "nothing ever waited on it"
)

type requestRule struct {
	from []string
	// to is "" for a delete.
	to string
}

// A request is decided or withdrawn once: every move that changes a row
// starts from pending. A request whose booking action left no date waiting
// is deleted in the transaction that opened it (bookWholeSeries,
// bookSeriesRoom).
var requestRules = map[RoomRequestMove]requestRule{
	RoomRequestOpen:           {to: RoomRequestPending},
	RoomRequestConfirm:        {from: []string{RoomRequestPending}, to: RoomRequestConfirmed},
	RoomRequestReject:         {from: []string{RoomRequestPending}, to: RoomRequestRejected},
	RoomRequestWithdrawEmpty:  {from: []string{RoomRequestPending}, to: RoomRequestWithdrawn},
	RoomRequestWithdrawSeries: {from: []string{RoomRequestPending}, to: RoomRequestWithdrawn},
	RoomRequestDropUnused:     {from: []string{RoomRequestPending}},
}

func RoomRequestMoves() []RoomRequestMove {
	out := make([]RoomRequestMove, 0, len(requestRules))
	for m := range requestRules {
		out = append(out, m)
	}
	slices.Sort(out)
	return out
}

// From is the statuses the move starts from; none for opening a request, or
// for a name the table does not hold.
func (m RoomRequestMove) From() []string { return slices.Clone(requestRules[m].from) }

// To is the status the move ends in; "" for a delete.
func (m RoomRequestMove) To() string { return requestRules[m].to }

func (m RoomRequestMove) Allows(status string) bool {
	return slices.Contains(requestRules[m].from, status)
}

// RoomRequestUndecided: an admin may still confirm or reject the request,
// and its requested bookings wait on it.
func RoomRequestUndecided(status string) bool { return RoomRequestConfirm.Allows(status) }
