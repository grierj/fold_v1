package policy

// DateReader is how one viewer reaches one scheduled date, from facts the date
// projector already decided with the rules above: Ordinary is CanSeeEvent at the
// date's effective visibility (false for a reader who holds only an exact-date
// invitation), SeriesVisible is CanSeeEvent at the series' own visibility,
// AttendeeNames is CanSeeAttendeeNames, Manager is CanManageEvent and
// Childcare is CanSeeChildcare. Recurring is whether the event repeats: the
// series writer answers only for a repeating event.
type DateReader struct {
	Ordinary      bool
	SeriesVisible bool
	AttendeeNames bool
	Manager       bool
	Childcare     bool
	Recurring     bool
	SignedIn      bool
}

// DatePageFacts names the groups of facts a date page may carry beyond the
// date itself (M8.1 brief sections 4 and 6).
type DatePageFacts struct {
	// Header is the shown group's cover and leader; the group itself is still
	// decided by CanSeeGroup.
	Header bool
	// Attendance is the row counts by answer; Names the attendee names and
	// Declined the "can't go" count (DEC-64).
	Attendance bool
	Names      bool
	Declined   bool
	// Capacity is the seat pools, the remote RSVP state, the viewer's waitlist
	// position and childcare availability; ChildcareTotals the exact childcare
	// usage and places left.
	Capacity        bool
	ChildcareTotals bool
	// Series is the recurrence, the series' usual values and how this date
	// differs from them; Coverage the viewer's own coverage of the series'
	// upcoming dates and whether they may answer for the series, only for a
	// repeating event (a one-time event has no series answer to count).
	Series   bool
	Coverage bool
}

// DatePage decides what one reader's date page carries. An invitation-only
// reader gets none of it: their invitation is to this date's own answer, never
// to its organizer, attendance, capacity or series (CanSeeOccurrenceAttendees).
// A date opened on its own (a hidden series, DEC-46) carries nothing of the
// series, and childcare totals are a series detail as well as CanSeeChildcare's.
func DatePage(r DateReader) DatePageFacts {
	if !r.Ordinary {
		return DatePageFacts{}
	}
	return DatePageFacts{
		Header:          true,
		Attendance:      true,
		Names:           CanSeeOccurrenceAttendees(r.Ordinary, r.AttendeeNames),
		Declined:        r.Manager,
		Capacity:        true,
		ChildcareTotals: r.Childcare && r.SeriesVisible,
		Series:          r.SeriesVisible,
		Coverage:        r.SeriesVisible && r.SignedIn && r.Recurring,
	}
}
