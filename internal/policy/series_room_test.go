package policy

import "testing"

func TestCanChangeBooking(t *testing.T) {
	leader, admin := &Person{ID: 1}, &Person{ID: 2, Admin: true}
	for _, tc := range []struct {
		name                       string
		p                          *Person
		restricted, overlap, wants bool
	}{
		{"leader, an ordinary booking", leader, false, false, true},
		{"leader, a restricted room", leader, true, false, false},
		{"leader, a double-booking", leader, false, true, false},
		{"admin, a restricted double-booking", admin, true, true, true},
	} {
		if got := CanChangeBooking(tc.p, tc.restricted, tc.overlap); got != tc.wants {
			t.Errorf("%s: got %v", tc.name, got)
		}
	}
}

func TestSeriesRoomChange(t *testing.T) {
	leader, admin := &Person{ID: 1}, &Person{ID: 2, Admin: true}
	hall, chapel, annex := uint(1), uint(2), uint(3)
	at := func(s RoomBookingState, room uint) SeriesDateBooking {
		return SeriesDateBooking{State: s, RoomID: room}
	}
	nursery := SeriesDateBooking{State: RoomBookingConfirmed, RoomID: annex, Restricted: true}
	inUse := func(b SeriesDateBooking) SeriesDateBooking {
		b.PickInUse = true
		return b
	}
	for _, tc := range []struct {
		name         string
		p            *Person
		b            SeriesDateBooking
		series, pick *uint
		named        bool
		want         SeriesRoomOutcome
	}{
		{"a confirmed own room is kept", leader, at(RoomBookingConfirmed, chapel), &hall, &annex, false, SeriesKept},
		{"a waiting own room is kept", leader, at(RoomBookingWaiting, chapel), &hall, &annex, false, SeriesKept},
		{"a named own room is included", leader, at(RoomBookingWaiting, chapel), &hall, &annex, true, SeriesIncluded},
		{"with no series room, every booked date has its own", leader, at(RoomBookingConfirmed, chapel), nil, &hall, false, SeriesKept},
		{"a booking in the series room follows it", leader, at(RoomBookingConfirmed, hall), &hall, &chapel, false, SeriesFollows},
		{"a waiting booking in the series room follows it", leader, at(RoomBookingWaiting, hall), &hall, &chapel, false, SeriesFollows},
		{"a lost booking in another room follows a new pick", leader, at(RoomBookingLost, chapel), &hall, &annex, false, SeriesFollows},
		{"a rejected one too", leader, at(RoomBookingRejected, chapel), &hall, &annex, false, SeriesFollows},
		{"a date that holds the room picked is already there", leader, at(RoomBookingConfirmed, chapel), &hall, &chapel, false, SeriesAlreadyThere},
		{"named, it is still already there", leader, at(RoomBookingConfirmed, chapel), &hall, &chapel, true, SeriesAlreadyThere},
		{"a date waiting for the room picked keeps its own request", leader, at(RoomBookingWaiting, chapel), &hall, &chapel, false, SeriesKept},
		{"an admin re-picking the series room replaces the series' waiting dates", admin, at(RoomBookingWaiting, hall), &hall, &hall, false, SeriesFollows},
		{"a leader cannot include an admin's restricted room", leader, nursery, &hall, &chapel, true, SeriesKept},
		{"an admin can", admin, nursery, &hall, &chapel, true, SeriesIncluded},
		{"a leader keeps an admin's double-booking in the series room", leader, SeriesDateBooking{State: RoomBookingDoubleBooked, RoomID: hall, AllowOverlap: true}, &hall, &chapel, true, SeriesKept},
		{"a lost restricted booking is left to an admin", leader, SeriesDateBooking{State: RoomBookingLost, RoomID: annex, Restricted: true}, &hall, &chapel, true, SeriesLeftAlone},
		{"stopping releases the series room's booking", leader, at(RoomBookingConfirmed, hall), &hall, nil, false, SeriesFollows},
		{"stopping keeps an own room", leader, at(RoomBookingConfirmed, chapel), &hall, nil, false, SeriesKept},
		{"stopping with it named releases it", leader, at(RoomBookingWaiting, chapel), &hall, nil, true, SeriesIncluded},
		{"stopping leaves a lost booking in another room", leader, at(RoomBookingLost, chapel), &hall, nil, false, SeriesLeftAlone},
		{"a named own room keeps it when the room picked is in use then", leader, inUse(at(RoomBookingConfirmed, chapel)), &hall, &annex, true, SeriesKeptInUse},
		{"a named waiting own room keeps its request too", leader, inUse(at(RoomBookingWaiting, chapel)), &hall, &annex, true, SeriesKeptInUse},
		{"not named, it is kept as any own room is", leader, inUse(at(RoomBookingConfirmed, chapel)), &hall, &annex, false, SeriesKept},
		{"stopping with it named releases it whatever else is booked", leader, inUse(at(RoomBookingConfirmed, chapel)), &hall, nil, true, SeriesIncluded},
		{"a date that follows the series room is not kept by it", leader, inUse(at(RoomBookingConfirmed, hall)), &hall, &annex, true, SeriesFollows},
		{"an admin's named restricted room keeps it too", admin, inUse(nursery), &hall, &chapel, true, SeriesKeptInUse},
		{"stopping releases a lost booking in the series room", leader, at(RoomBookingLost, hall), &hall, nil, false, SeriesFollows},
	} {
		if got := SeriesRoomChange(tc.p, tc.b, tc.series, tc.pick, tc.named); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The control's list, less the dates already in the room picked, is exactly
// what a change can keep or include: a date it keeps was shown, and a date it
// was not shown is never named into a change.
func TestSeriesRoomListIsWhatAChangeKeeps(t *testing.T) {
	rooms := []uint{1, 2}
	pointers := []*uint{nil, &rooms[0], &rooms[1]}
	for _, p := range []*Person{{ID: 1}, {ID: 2, Admin: true}} {
		for _, s := range RoomBookingStates() {
			for _, room := range rooms {
				for _, restricted := range []bool{false, true} {
					for _, overlap := range []bool{false, true} {
						for _, inUse := range []bool{false, true} {
							b := SeriesDateBooking{State: s, RoomID: room, Restricted: restricted, AllowOverlap: overlap, PickInUse: inUse}
							for _, series := range pointers {
								for _, pick := range pointers {
									shown := b.Listed(p, series) && !(pick != nil && room == *pick && s.HoldsRoom())
									for _, named := range []bool{false, true} {
										got := SeriesRoomChange(p, b, series, pick, named)
										keeps := got == SeriesKept || got == SeriesIncluded || got == SeriesKeptInUse
										if keeps != shown {
											t.Errorf("%+v %+v series %v pick %v named %v: %q, shown %v", p, b, series, pick, named, got, shown)
										}
										if (got == SeriesIncluded || got == SeriesKeptInUse) && !named {
											t.Errorf("%+v: included without being named", b)
										}
										if got == SeriesIncluded && inUse && pick != nil {
											t.Errorf("%+v pick %v: included into a room in use then", b, pick)
										}
									}
								}
							}
						}
					}
				}
			}
		}
	}
}
