package policy

// CanChangeBooking: a booking only an admin could make (in a restricted
// room, or a double-booking) is only an admin's to change or release.
func CanChangeBooking(p *Person, restricted, allowOverlap bool) bool {
	return !(restricted || allowOverlap) || CanDoubleBookRoom(p)
}

// SeriesDateBooking is one future date's booking as a series room change
// judges it.
type SeriesDateBooking struct {
	State        RoomBookingState
	RoomID       uint
	Restricted   bool
	AllowOverlap bool
}

// holdsOrWaits: the booking holds its room, or waits for an admin's
// approval of it. A lost or rejected one holds nothing.
func (b SeriesDateBooking) holdsOrWaits() bool {
	return b.State.HoldsRoom() || b.State == RoomBookingWaiting
}

// OwnRoom is DEC-168's rule: the date has a room of its own when its
// booking holds a room or waits for one, in a room other than the series
// room (nil for none) it would otherwise follow.
func (b SeriesDateBooking) OwnRoom(seriesRoom *uint) bool {
	return b.holdsOrWaits() && (seriesRoom == nil || b.RoomID != *seriesRoom)
}

// Listed: the series room control shows p the date before any change, as
// keeping its room: a room of its own, or a booking that holds or waits and
// only an admin may change. The control leaves out a listed date that
// already holds the room picked (SeriesAlreadyThere), as SeriesRoomChange
// does.
func (b SeriesDateBooking) Listed(p *Person, seriesRoom *uint) bool {
	return b.OwnRoom(seriesRoom) || (b.holdsOrWaits() && !CanChangeBooking(p, b.Restricted, b.AllowOverlap))
}

// SeriesRoomOutcome is what a series room change does to one date's booking.
type SeriesRoomOutcome string

const (
	// SeriesFollows: released, and booked in the new series room if there is one.
	SeriesFollows SeriesRoomOutcome = "follows the series room"
	// SeriesIncluded: a room of its own the person was shown and named, which
	// then goes as SeriesFollows does.
	SeriesIncluded SeriesRoomOutcome = "a room of its own, included"
	// SeriesAlreadyThere: it already holds the room picked, with any padding
	// an admin set.
	SeriesAlreadyThere SeriesRoomOutcome = "already holds the room picked"
	// SeriesKept: a listed date that keeps its room: not named, or only an
	// admin may change it.
	SeriesKept SeriesRoomOutcome = "keeps its room"
	// SeriesLeftAlone: it holds nothing and is not the change's to replace:
	// a lost or rejected booking only an admin may change, or, when the
	// series room stops, one in another room than the series room.
	SeriesLeftAlone SeriesRoomOutcome = "left as it is"
)

// SeriesRoomChange decides what p's change of the series room from
// seriesRoom to pick (nil stops it) does to b (DEC-168). named: the request
// named b's date, which the person was shown in the control's list.
func SeriesRoomChange(p *Person, b SeriesDateBooking, seriesRoom, pick *uint, named bool) SeriesRoomOutcome {
	switch {
	case pick != nil && b.RoomID == *pick && b.State.HoldsRoom():
		return SeriesAlreadyThere
	case !CanChangeBooking(p, b.Restricted, b.AllowOverlap):
		if b.holdsOrWaits() {
			return SeriesKept
		}
		return SeriesLeftAlone
	case b.OwnRoom(seriesRoom):
		if named {
			return SeriesIncluded
		}
		return SeriesKept
	case pick == nil && (seriesRoom == nil || b.RoomID != *seriesRoom):
		return SeriesLeftAlone
	}
	return SeriesFollows
}
