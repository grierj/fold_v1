package main

import (
	"fmt"
	"strings"

	"fold/internal/notify"
	"fold/internal/policy"
	"fold/models"

	"gorm.io/gorm"
)

// Room notices (9.2): a date lost its room (room.lost), a booking waits for an
// admin (room.requested), an admin decided it (room.request_decided), and
// someone changed an event's room plans (room.changed, DEC-170).

// notifyRoomLost tells the event's leaders (or a church-wide event's hosts)
// which dates lost their room, in one notice (9.2). The person who moved
// the date is told too (no Actor): the move saved, and a lost room is easy
// to miss on a page that no longer shows it.
func notifyRoomLost(tx *gorm.DB, ev *models.Event, dates []string) error {
	to, err := eventRunnerIDs(tx, ev)
	if err != nil {
		return err
	}
	_, err = notify.Send(tx, notify.Msg{
		Kind: notify.RoomLost, To: to,
		SubjectType: "event", SubjectID: ev.ID, Link: fmt.Sprintf("/events/%d", ev.ID),
		Vars: notify.Vars{Event: ev.Title, Change: dateCount(len(dates))}, // group-ok: the event's own leaders and hosts
		Data: map[string]any{"eventId": ev.ID, "dates": dates},
	})
	return err
}

func notifyRoomRequested(tx *gorm.DB, req *models.RoomRequest, ev *models.Event, room *models.Room, dates int, actor *uint) error {
	admins, err := activeAdminIDs(tx)
	if err != nil {
		return err
	}
	asker, err := askerName(tx, actor)
	if err != nil {
		return err
	}
	_, err = notify.Send(tx, notify.Msg{
		Kind: notify.RoomRequested, To: admins, Actor: actor,
		SubjectType: "room_request", SubjectID: req.ID, Link: roomRequestsLink,
		Vars:      notify.Vars{Actor: asker, Event: ev.Title, Change: room.Name + " for " + dateCount(dates)}, // group-ok: admin tasks go to admins
		Data:      map[string]any{"requestId": req.ID, "eventId": ev.ID},
		DedupeKey: fmt.Sprintf("room.requested:%d", req.ID),
	})
	return err
}

func askerName(tx *gorm.DB, id *uint) (string, error) {
	if id == nil {
		return "", nil
	}
	return nameOf(tx, *id)
}

// notifyRoomDecided tells the event's leaders (or hosts) what an admin
// decided; the admin is the actor and isn't told.
func notifyRoomDecided(tx *gorm.DB, ev *models.Event, actor uint, change, note string) error {
	to, err := eventRunnerIDs(tx, ev)
	if err != nil {
		return err
	}
	_, err = notify.Send(tx, notify.Msg{
		Kind: notify.RoomRequestDecided, To: to, Actor: &actor,
		SubjectType: "event", SubjectID: ev.ID, Link: fmt.Sprintf("/events/%d", ev.ID),
		Vars: notify.Vars{Event: ev.Title, Change: change, Note: note}, // group-ok: the event's own leaders and hosts
		Data: map[string]any{"eventId": ev.ID},
	})
	return err
}

// roomHeld is what a date's booking gives the people who run the event: the
// room it holds, or waits for an admin to approve, or nothing (room 0) when
// the date has no booking or one that holds nothing (lost or rejected).
type roomHeld struct {
	room    uint
	waiting bool
}

func roomHeldOf(roomID uint, s policy.RoomBookingState) roomHeld {
	switch {
	case s.HoldsRoom():
		return roomHeld{room: roomID}
	case s == policy.RoomBookingWaiting:
		return roomHeld{room: roomID, waiting: true}
	}
	return roomHeld{}
}

// roomHeldAfter is what placeBooking left a date with.
func roomHeldAfter(roomID uint, r bookResult) roomHeld {
	switch r {
	case bookedFree, bookedOverlap:
		return roomHeld{room: roomID}
	case bookedPending:
		return roomHeld{room: roomID, waiting: true}
	}
	return roomHeld{}
}

// roomChangedTo is eventRunnerIDs without the person who made the change.
func roomChangedTo(tx *gorm.DB, ev *models.Event, actor uint) ([]uint, error) {
	runners, err := eventRunnerIDs(tx, ev)
	if err != nil {
		return nil, err
	}
	to := make([]uint, 0, len(runners))
	for _, id := range runners {
		if id != actor {
			to = append(to, id)
		}
	}
	return to, nil
}

// notifyDateRoomChanged tells the event's other runners that actor changed
// one date's room (DEC-170), when what the date holds changed. The day is in
// each recipient's zone (DEC-58), so it is one Send per recipient under one
// recipient lock.
func notifyDateRoomChanged(tx *gorm.DB, ev *models.Event, occ *models.EventOccurrence, actor uint, before, after roomHeld) error {
	var change string
	switch {
	case before == after:
		return nil
	case after.room == 0:
		change = "cleared"
	case before.room != 0 && before.room != after.room:
		change = "changed"
	case after.waiting:
		change = "asked"
	default:
		change = "booked"
	}
	to, err := roomChangedTo(tx, ev, actor)
	if err != nil || len(to) == 0 {
		return err
	}
	name, err := nameOf(tx, actor)
	if err != nil {
		return err
	}
	facts, err := loadNoticeFacts(tx, eventGroupIDs(ev), to)
	if err != nil {
		return err
	}
	note := ""
	if after.waiting {
		note = "It's waiting for an admin's approval."
	}
	end := noticeEnd(occ)
	return notify.WithRecipients(tx, to, func(scope *notify.Scope) error {
		for _, uid := range to {
			_, err := notify.Send(tx, notify.Msg{
				Kind: notify.RoomChanged, To: []uint{uid}, Actor: &actor,
				SubjectType: "occurrence", SubjectID: occ.ID, Link: fmt.Sprintf("/events/%d", ev.ID),
				Vars: notify.Vars{Actor: name, Event: effectiveOccurrenceTitle(ev, occ), // group-ok: the event's own leaders and hosts
					When: formatWhen(occ.StartsAt, facts.zone(uid)), Change: change, Note: note},
				Data:      map[string]any{"eventId": ev.ID, "date": models.FormatCalendarDate(occ.OriginalDate)},
				ExpiresAt: &end, Scope: scope,
			})
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// notifySeriesRoomChanged tells the event's other runners, in one notice,
// that actor set, changed or stopped the series room and how its dates came
// out (DEC-170): the counts are the pick's own, and the dates left needing a
// room are named in Data "dates". freed is how many dates a stop left with no
// room.
func notifySeriesRoomChanged(tx *gorm.DB, ev *models.Event, actor uint, oldRoom, newRoom *uint, out seriesBookingJSON, freed int) error {
	change := "series_picked"
	switch {
	case newRoom == nil:
		change = "series_stopped"
	case oldRoom != nil && *oldRoom != *newRoom:
		change = "series_changed"
	}
	to, err := roomChangedTo(tx, ev, actor)
	if err != nil || len(to) == 0 {
		return err
	}
	name, err := nameOf(tx, actor)
	if err != nil {
		return err
	}
	var note []string
	if out.Booked > 0 {
		note = append(note, isAre(out.Booked)+" booked.")
	}
	if out.Pending > 0 {
		note = append(note, isAre(out.Pending)+" waiting for an admin's approval.")
	}
	if kept := out.Kept + out.KeptInUse; kept > 0 {
		note = append(note, keepsKeep(kept))
	}
	if freed > 0 {
		note = append(note, hasHave(freed, "no room now."))
	}
	if n := len(out.Lost); n > 0 {
		note = append(note, needsNeed(n)+" a room because the room is in use then. Pick another room for "+themIt(n)+" on the event page.")
	}
	_, err = notify.Send(tx, notify.Msg{
		Kind: notify.RoomChanged, To: to, Actor: &actor,
		SubjectType: "event", SubjectID: ev.ID, Link: fmt.Sprintf("/events/%d", ev.ID),
		Vars: notify.Vars{Actor: name, Event: ev.Title, Change: change, // group-ok: the event's own leaders and hosts
			Note: strings.Join(note, " "), NeedRoom: len(out.Lost)},
		Data: map[string]any{"eventId": ev.ID, "dates": out.Lost},
	})
	return err
}

func dateCount(n int) string {
	if n == 1 {
		return "1 date"
	}
	return fmt.Sprintf("%d dates", n)
}

func isAre(n int) string {
	if n == 1 {
		return "1 date is"
	}
	return fmt.Sprintf("%d dates are", n)
}

func hasHave(n int, rest string) string {
	if n == 1 {
		return "1 date has " + rest
	}
	return fmt.Sprintf("%d dates have %s", n, rest)
}

func keepsKeep(n int) string {
	if n == 1 {
		return "1 date keeps its room."
	}
	return fmt.Sprintf("%d dates keep their room.", n)
}

func needsNeed(n int) string {
	if n == 1 {
		return "1 date still needs"
	}
	return fmt.Sprintf("%d dates still need", n)
}

func themIt(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}
