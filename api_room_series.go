package main

import (
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"fold/internal/audit"
	"fold/internal/eventdates"
	"fold/internal/policy"
	"fold/internal/rooms"
	"fold/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Series rooms (M2.1c, 9.2). "Book for the whole series" stores the room on
// the event (default_room_id) and books every scheduled future date. A date
// the room is busy on keeps a requested booking, holding nothing, and shows
// on the event page as needing a room (DEC-103). Dates the daily extender or
// a series edit adds later are booked in the series room the same way.

var errRoomGone = apiErr(http.StatusNotFound, "", "room not found")

// maxSeriesIncludeDates is more dates than a series holds in its 18 months.
const maxSeriesIncludeDates = 1000

// errPickedRoomGone: the room a pick named was turned off since the picker
// listed it. Its own code, so the picker tells it from the event's or the
// date's 404, which say the person may no longer manage them.
var errPickedRoomGone = apiErr(http.StatusNotFound, "room_gone", "that room is no longer available")

type seriesRoomJSON struct {
	ID         uint   `json:"id"`
	Name       string `json:"name"`
	Building   string `json:"building"`
	Capacity   *int   `json:"capacity"`
	Features   string `json:"features"`
	Restricted bool   `json:"restricted"`
	// BusyDates: how many of the dates another confirmed booking holds the
	// room on. Who holds it is never said.
	BusyDates int `json:"busyDates"`
	// TooSmall: the event's seat cap is above the room's capacity. A
	// warning, never a block (9.2).
	TooSmall bool `json:"tooSmall"`
}

// needsRoomJSON is a date whose booking holds nothing and is the leader's
// to re-pick: it lost its room (status requested) when it moved or the room
// was busy, or an admin didn't approve it (status rejected, M2.1d). Dates
// waiting on an admin are not listed (DEC-105).
type needsRoomJSON struct {
	Date     string `json:"date"`
	RoomName string `json:"roomName"`
	Status   string `json:"status"`
}

// ownRoomJSON is a future date a series room change leaves as it is unless
// the person names it (DEC-168, policy.SeriesDateBooking.Listed): a room of
// its own, or a booking that holds or waits and only an admin may change
// (MayChange false). Its room is named as the date's own place names it.
type ownRoomJSON struct {
	Date      string             `json:"date"`
	Room      occurrenceRoomJSON `json:"room"`
	Waiting   bool               `json:"waiting"`
	MayChange bool               `json:"mayChange"`
}

type seriesBookingJSON struct {
	// Event: the event the pick stored, with the token the next pick sends.
	Event       revisionRefJSON     `json:"event"`
	DefaultRoom *occurrenceRoomJSON `json:"defaultRoom"`
	Booked      int                 `json:"booked"`
	// Lost: dates the room was busy on, kept as requested.
	Lost []string `json:"lost"`
	// Kept: listed dates this pick left with their room (policy.SeriesKept),
	// each in KeptRooms.
	Kept      int           `json:"kept"`
	KeptRooms []ownRoomJSON `json:"keptRooms"`
	// Changed: dates the request named whose own room this pick released
	// (policy.SeriesIncluded); where they ended is in the counts beside it.
	Changed int `json:"changed"`
	// Pending: dates waiting on an admin's approval (M2.1d).
	Pending int `json:"pending"`
	// DoubleBooked: of the dates booked, those an admin's reason booked over
	// another booking.
	DoubleBooked int `json:"doubleBooked"`
}

// roomChoices lists the active rooms p may book for the given dates, with
// how many of them each room is busy on. Bookings of eventID's own dates
// don't count: re-picking a series' room never finds it busy with itself.
func roomChoices(db *gorm.DB, p *policy.Person, windows []rooms.Window, eventID uint, capacity *int) ([]seriesRoomJSON, error) {
	list, busy, err := activeRoomsBusy(db, windows, eventID)
	if err != nil {
		return nil, err
	}
	want := 0
	if capacity != nil {
		want = *capacity
	}
	out := []seriesRoomJSON{}
	for _, r := range list {
		if !policy.CanBookRoom(p, true, r.Restricted) {
			continue
		}
		out = append(out, seriesRoomJSON{ID: r.ID, Name: r.Name, Building: r.Building, Capacity: r.Capacity,
			Features: r.Features, Restricted: r.Restricted, BusyDates: busy[r.ID], TooSmall: tooSmall(r.Capacity, want)})
	}
	return out, nil
}

// apiSeriesRooms is the series' room control: the rooms its manager may book
// over the upcoming dates, the series room, the dates that keep a room of
// their own through a change of it (DEC-168), the dates that need a room, the
// event's token (eventRevision) the series pick sends and the suggestions.
// One read-only snapshot decides everything (docs/agents/snapshots.md): the
// rooms switch, the viewer and their authority over the event
// (managedEventTargetIn), and every row the answer shows, on one clock.
func (a *App) apiSeriesRooms(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	var out seriesRoomsJSON
	var judged sessionJudgment
	err := decidingSnapshot(a.DB.WithContext(c.Request.Context()), func(tx *gorm.DB) error {
		if err := roomsOnIn(tx); err != nil {
			return err
		}
		t, err := a.managedEventTargetIn(tx, apiUser(c), id, &judged)
		if err != nil {
			return err
		}
		if a.hooks.readerAuthorized != nil {
			a.hooks.readerAuthorized("series rooms")
		}
		now := a.now().UTC()
		me, ev := asPerson(t.me), &t.ev
		out.Revision = a.eventRevision(ev, t.f.group)
		var occs []models.EventOccurrence
		if err := tx.Where("event_id = ? AND status = ? AND starts_at > ?", ev.ID, models.OccurrenceScheduled, now).
			Order("starts_at").Find(&occs).Error; err != nil {
			return err
		}
		windows := make([]rooms.Window, len(occs))
		for i, o := range occs {
			windows[i] = rooms.Window{Start: o.StartsAt, End: o.EndsAt}
		}
		if out.Rooms, err = roomChoices(tx, me, windows, ev.ID, ev.Capacity); err != nil {
			return err
		}
		held, err := futureDateBookingsIn(tx, ev.ID, now)
		if err != nil {
			return err
		}
		// A listed date is neither the series room's waiting date nor one that
		// needs a room.
		out.OwnRooms, out.NeedsRoom = ownRooms(me, ev.DefaultRoomID, held), []needsRoomJSON{}
		for _, h := range held {
			b := h.judged()
			switch {
			case b.Listed(me, ev.DefaultRoomID):
			case b.State == policy.RoomBookingWaiting:
				out.Pending++
			case !b.State.HoldsRoom():
				out.NeedsRoom = append(out.NeedsRoom, needsRoomJSON{Date: h.Date, RoomName: h.RoomName, Status: h.Status})
			}
		}
		out.Dates = len(occs)
		if out.DefaultRoom, err = seriesRoomIn(tx, ev); err != nil {
			return err
		}
		out.Suggestions = roomSuggestions(tx, now, suggestFor{person: me, eventID: ev.ID, groupID: ev.GroupID,
			visibility: ev.Visibility, capacity: ev.Capacity, windows: windowsOf(occs)}, offeredRooms(out.Rooms))
		return nil
	})
	if ackErr := a.acknowledgeDecision(c, judged, err == nil || decidedRefusal(err)); ackErr != nil {
		err = ackErr
	}
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// dateBookingRow is one date's booking with what a series room change judges
// it by (policy.SeriesDateBooking).
type dateBookingRow struct {
	ID, OccurrenceID   uint
	Date               string
	RoomID             uint
	RoomName, Building string
	Restricted         bool
	Status             string
	AllowOverlap       bool
	// Waiting: the request it names is undecided.
	Waiting bool
}

func (r dateBookingRow) judged() policy.SeriesDateBooking {
	return policy.SeriesDateBooking{State: policy.RoomBookingStateOf(r.Status, r.Waiting, r.AllowOverlap),
		RoomID: r.RoomID, Restricted: r.Restricted, AllowOverlap: r.AllowOverlap}
}

// dateBookingsIn reads the bookings where picks, in date order (o is the
// date, b the booking).
func dateBookingsIn(tx *gorm.DB, where string, args ...any) ([]dateBookingRow, error) {
	var rows []dateBookingRow
	err := tx.Raw(`SELECT b.id, b.occurrence_id, to_char(o.original_date, 'YYYY-MM-DD') AS date, b.room_id, r.name AS room_name,
			r.building, r.restricted, b.status, b.allow_overlap,
			EXISTS (SELECT 1 FROM room_requests q WHERE q.id = b.request_id AND q.status IN ?) AS waiting
		FROM room_bookings b JOIN event_occurrences o ON o.id = b.occurrence_id JOIN rooms r ON r.id = b.room_id
		WHERE `+where+` ORDER BY o.starts_at, o.id`, append([]any{policy.RoomRequestConfirm.From()}, args...)...).Scan(&rows).Error
	return rows, err
}

func futureDateBookingsIn(tx *gorm.DB, eventID uint, now time.Time) ([]dateBookingRow, error) {
	return dateBookingsIn(tx, "o.event_id = ? AND o.status = ? AND o.starts_at > ?", eventID, models.OccurrenceScheduled, now)
}

// ownRooms is the list the series room control shows me (DEC-168) of rows,
// under the series room seriesRoom.
func ownRooms(me *policy.Person, seriesRoom *uint, rows []dateBookingRow) []ownRoomJSON {
	out := []ownRoomJSON{}
	for _, r := range rows {
		if r.judged().Listed(me, seriesRoom) {
			out = append(out, r.ownRoom(me))
		}
	}
	return out
}

func (r dateBookingRow) ownRoom(me *policy.Person) ownRoomJSON {
	return ownRoomJSON{Date: r.Date, Room: occurrenceRoomJSON{ID: r.RoomID, Name: r.RoomName, Building: r.Building},
		Waiting: r.judged().State == policy.RoomBookingWaiting, MayChange: policy.CanChangeBooking(me, r.Restricted, r.AllowOverlap)}
}

// seriesRoomIn is ev's series room as tx reads it; nil when it has none.
func seriesRoomIn(tx *gorm.DB, ev *models.Event) (*occurrenceRoomJSON, error) {
	if ev.DefaultRoomID == nil {
		return nil, nil
	}
	var r models.Room
	if err := tx.First(&r, *ev.DefaultRoomID).Error; err != nil {
		return nil, err
	}
	return &occurrenceRoomJSON{ID: r.ID, Name: r.Name, Building: r.Building}, nil
}

// apiSetSeriesRoom books a room for the whole series, or (roomId null)
// stops booking one. An admin's reason double-books busy dates instead of
// leaving them needing a room. includeDates names the dates with a room of
// their own that the control showed and the person chose to change too
// (DEC-168); without it every such date keeps its room. The pick names the
// event's read it was made from (revision, eventRevision): the trigger moves
// it with the series room (default_room_id, default_room_pending), and an
// older one is 409 event_changed, writing nothing (DEC-167).
func (a *App) apiSetSeriesRoom(c *gin.Context) {
	ev, ok := a.loadManagedEvent(c)
	if !ok {
		return
	}
	var body struct {
		RoomID        *uint    `json:"roomId"`
		OverlapReason string   `json:"overlapReason"`
		Revision      string   `json:"revision"`
		IncludeDates  []string `json:"includeDates"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || len(body.IncludeDates) > maxSeriesIncludeDates {
		jsonError(c, http.StatusBadRequest, "invalid request")
		return
	}
	for _, d := range body.IncludeDates {
		if !isCalendarDate(d) {
			fail(c, errSeriesDateInvalid)
			return
		}
	}
	reason := strings.TrimSpace(body.OverlapReason)
	if utf8.RuneCountInString(reason) > maxOverlapReason {
		fail(c, errOverlapReasonTooLong)
		return
	}
	var out seriesBookingJSON
	err := a.DB.Transaction(func(tx *gorm.DB) error {
		user, locked, f, err := lockManagedEventIn(tx, apiUser(c), ev.ID)
		if err != nil {
			return err
		}
		if err := a.checkEventRevision(locked, f.group, body.Revision); err != nil {
			return err
		}
		pick := seriesPick{roomID: body.RoomID, include: body.IncludeDates, reason: reason}
		if out, err = bookWholeSeries(tx, locked, pick, user, c.ClientIP()); err != nil {
			return err
		}
		if err := readEventRevision(tx, locked); err != nil {
			return err
		}
		out.Event = revisionRefJSON{ID: locked.ID, Revision: a.eventRevision(locked, f.group)}
		return nil
	}, readCommitted)
	if err != nil {
		answerEventWrite(c, err, "could not save the room")
		return
	}
	c.JSON(http.StatusOK, out)
}

// seriesPick is one change of a series room: the room (nil stops it), the
// dates with a room of their own the person named to change too (DEC-168),
// and an admin's reason to double-book busy dates.
type seriesPick struct {
	roomID  *uint
	include []string
	reason  string
}

// bookWholeSeries sets ev's series room (nil stops it) and books it on every
// scheduled future date, under ev's lock. What it does to a date that has a
// booking is policy.SeriesRoomChange (DEC-168): a date with a room of its own
// keeps it unless the pick names it, and a booking only an admin could make
// stays unless an admin is booking. A busy date keeps a requested booking
// (DEC-103) unless an admin's reason double-books it.
//
// With approval on (DEC-102), a leader's or host's free dates wait on one
// request for the whole series (DEC-106); busy ones are lost as above.
//
// A group that isn't active books no room (DEC-147); stopping the series room
// (roomID nil) is always allowed.
func bookWholeSeries(tx *gorm.DB, ev *models.Event, pick seriesPick, user *models.User, ip string) (seriesBookingJSON, error) {
	roomID, reason := pick.roomID, pick.reason
	out := seriesBookingJSON{Lost: []string{}, KeptRooms: []ownRoomJSON{}}
	if roomID != nil {
		if ok, err := groupSchedulesIn(tx, ev.GroupID); err != nil {
			return out, err
		} else if !ok {
			return out, errGroupRoomsNotApproved
		}
	}
	me := asPerson(user)
	if !policy.CanDoubleBookRoom(me) {
		reason = ""
	}
	var occs []models.EventOccurrence
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("event_id = ? AND status = ? AND starts_at > ?", ev.ID, models.OccurrenceScheduled, time.Now()).
		Order("id").Find(&occs).Error; err != nil {
		return out, err
	}
	// Dates first, then the room: the same lock order as one date's booking.
	var room *models.Room
	if roomID != nil {
		r, err := lockRoom(tx, *roomID)
		if errors.Is(err, gorm.ErrRecordNotFound) || (err == nil && !r.Active) {
			return out, errPickedRoomGone
		}
		if err != nil {
			return out, err
		}
		if !policy.CanBookRoom(me, true, r.Restricted) {
			return out, errRoomRestricted
		}
		room = r
		out.DefaultRoom = &occurrenceRoomJSON{ID: r.ID, Name: r.Name, Building: r.Building}
	}
	var req *models.RoomRequest
	var requestID *uint
	// Re-booking the series room an admin already approved (to retry lost
	// dates) needs no new approval.
	approved := ev.DefaultRoomID != nil && room != nil && *ev.DefaultRoomID == room.ID && !ev.DefaultRoomPending
	needsApproval := room != nil && !approved && policy.BookingNeedsApproval(me, roomApprovalOn(tx))
	if needsApproval {
		if err := withdrawSeriesRequest(tx, ev.ID); err != nil {
			return out, err
		}
		var err error
		if req, err = openRoomRequest(tx, ev.ID, room.ID, &user.ID, true); err != nil {
			return out, err
		}
		requestID = &req.ID
	}
	ids := make([]uint, len(occs))
	for i, o := range occs {
		ids[i] = o.ID
	}
	// Read after the withdrawal above: the series' own dates it left waiting
	// on nothing are lost now, and replaced.
	var current []dateBookingRow
	if len(ids) > 0 {
		var err error
		if current, err = dateBookingsIn(tx, "b.occurrence_id IN ?", ids); err != nil {
			return out, err
		}
	}
	byDate := make(map[uint]int, len(current))
	for i, b := range current {
		byDate[b.OccurrenceID] = i
	}
	named := make(map[string]bool, len(pick.include))
	for _, d := range pick.include {
		named[d] = true
	}
	oldRoom := ev.DefaultRoomID
	kept, changed := []string{}, []string{}
	released := 0
	for i := range occs {
		occ := &occs[i]
		if j, has := byDate[occ.ID]; has {
			b := current[j]
			switch policy.SeriesRoomChange(me, b.judged(), oldRoom, roomID, named[b.Date]) {
			case policy.SeriesAlreadyThere, policy.SeriesLeftAlone:
				continue
			case policy.SeriesKept:
				kept = append(kept, b.Date)
				out.KeptRooms = append(out.KeptRooms, b.ownRoom(me))
				continue
			case policy.SeriesIncluded:
				changed = append(changed, b.Date)
			}
			if err := tx.Delete(&models.RoomBooking{}, b.ID).Error; err != nil {
				return out, err
			}
			released++
		}
		if room == nil {
			continue
		}
		result, err := placeBooking(tx, occ, room, room.DefaultSetupMinutes, room.DefaultTeardownMinutes,
			placement{by: &user.ID, reason: reason, keepRequested: true, requestID: requestID})
		if err != nil {
			return out, err
		}
		switch result {
		case bookedRequested:
			out.Lost = append(out.Lost, models.FormatCalendarDate(occ.OriginalDate))
		case bookedPending:
			out.Pending++
		case bookedOverlap:
			out.Booked++
			out.DoubleBooked++
		default:
			out.Booked++
		}
	}
	out.Kept, out.Changed = len(kept), len(changed)
	if req != nil && out.Pending == 0 {
		if err := tx.Delete(req).Error; err != nil {
			return out, err
		}
		req = nil
	}
	if req != nil {
		var err error
		if err = notifyRoomRequested(tx, req, ev, room, out.Pending, &user.ID); err != nil {
			return out, err
		}
	}
	// Replaced bookings may have been other requests' last dates: an
	// included date's own request is withdrawn here and its task closed.
	if err := closeEmptyRoomRequests(tx); err != nil {
		return out, err
	}
	// Until an admin approves it, the series room's new dates keep going to
	// an admin, even if this request is withdrawn or had nothing to wait on.
	if err := tx.Model(&models.Event{}).Where("id = ?", ev.ID).
		Updates(map[string]any{"default_room_id": roomID, "default_room_pending": needsApproval}).Error; err != nil {
		return out, err
	}
	ev.DefaultRoomID, ev.DefaultRoomPending = roomID, needsApproval
	meta := map[string]any{"eventId": ev.ID, "roomId": roomID, "previousRoomId": oldRoom,
		"booked": out.Booked, "lost": out.Lost, "kept": out.Kept, "keptDates": kept, "changedDates": changed,
		"released": released, "pending": out.Pending}
	if reason != "" {
		meta["reason"] = reason
	}
	if req != nil {
		meta["requestId"] = req.ID
	}
	return out, audit.Write(tx, audit.Entry{Actor: &user.ID, Action: audit.RoomBookingSeries, TargetType: "event", TargetID: ev.ID,
		Meta: meta, IP: ip})
}

// bookSeriesRoom books the series room for dates that were just added (by
// the daily extender or a series edit) or brought back (DEC-91) and have no
// booking. A busy date keeps a requested booking; its date is returned.
// A series room that was since deactivated books nothing, and neither does a
// group that isn't active (DEC-147): no booking, request or room.lost notice.
//
// A series room not yet approved (default_room_pending) books new dates as
// a request while approval is on: into the waiting series request, or a
// new one if that was withdrawn.
func bookSeriesRoom(tx *gorm.DB, ev *models.Event, occurrenceIDs []uint, by *uint) ([]string, error) {
	if ev.DefaultRoomID == nil || len(occurrenceIDs) == 0 {
		return nil, nil
	}
	if ok, err := groupSchedulesIn(tx, ev.GroupID); err != nil || !ok {
		return nil, err
	}
	var occs []models.EventOccurrence
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id IN ? AND status = ? AND starts_at > now() AND NOT EXISTS (SELECT 1 FROM room_bookings b WHERE b.occurrence_id = event_occurrences.id)",
			occurrenceIDs, models.OccurrenceScheduled).Order("id").Find(&occs).Error; err != nil {
		return nil, err
	}
	if len(occs) == 0 {
		return nil, nil
	}
	room, err := lockRoom(tx, *ev.DefaultRoomID)
	if err != nil || !room.Active {
		return nil, err
	}
	var req *models.RoomRequest
	opened := false
	if ev.DefaultRoomPending && roomApprovalOn(tx) {
		var open []models.RoomRequest
		if err := tx.Where("event_id = ? AND room_id = ? AND series AND status = ?", ev.ID, room.ID, models.RoomRequestPending).
			Limit(1).Find(&open).Error; err != nil {
			return nil, err
		}
		if len(open) == 1 {
			req = &open[0]
		} else {
			// Asked for by whoever asked for the series room last.
			var last []models.RoomRequest
			if err := tx.Where("event_id = ? AND room_id = ? AND series", ev.ID, room.ID).Order("id DESC").Limit(1).Find(&last).Error; err != nil {
				return nil, err
			}
			var asker *uint
			if len(last) == 1 {
				asker = last[0].RequestedBy
			}
			if req, err = openRoomRequest(tx, ev.ID, room.ID, asker, true); err != nil {
				return nil, err
			}
			opened = true
		}
	}
	var requestID *uint
	if req != nil {
		requestID = &req.ID
	}
	var lost []string
	waiting := 0
	for i := range occs {
		result, err := placeBooking(tx, &occs[i], room, room.DefaultSetupMinutes, room.DefaultTeardownMinutes,
			placement{by: by, keepRequested: true, requestID: requestID})
		if err != nil {
			return nil, err
		}
		switch result {
		case bookedRequested:
			lost = append(lost, models.FormatCalendarDate(occs[i].OriginalDate))
		case bookedPending:
			waiting++
		}
	}
	if !opened {
		return lost, nil
	}
	if waiting == 0 {
		return lost, tx.Delete(req).Error
	}
	return lost, notifyRoomRequested(tx, req, ev, room, waiting, req.RequestedBy)
}

// settleDateRooms brings an event's bookings in line after its dates
// changed: moved dates take their bookings along, added or restored dates
// get the series room, and the leaders or hosts hear once about every date
// that ended up without its room.
func settleDateRooms(tx *gorm.DB, ev *models.Event, moved, added []uint, by *uint) error {
	lost, err := moveRoomBookings(tx, moved)
	if err != nil {
		return err
	}
	more, err := bookSeriesRoom(tx, ev, added, by)
	if err != nil {
		return err
	}
	lost = append(lost, more...)
	if len(lost) == 0 {
		return nil
	}
	return notifyRoomLost(tx, ev, lost)
}

// afterExtend books the series room for dates the daily extender adds;
// its notices queue their own delivery in the same transaction. While rooms
// are off it books nothing and tells no one. A failure here is logged and
// undone on its own, so it never stops the event's new dates being added.
func (a *App) afterExtend() eventdates.AfterEnsure {
	return func(tx *gorm.DB, ev *models.Event, added []string) error {
		if ev.DefaultRoomID == nil || !a.roomsEnabled() {
			return nil
		}
		if err := tx.SavePoint("rooms").Error; err != nil {
			return err
		}
		err := func() error {
			var ids []uint
			if err := tx.Model(&models.EventOccurrence{}).Where("event_id = ? AND original_date IN ?", ev.ID, added).
				Pluck("id", &ids).Error; err != nil {
				return err
			}
			return settleDateRooms(tx, ev, nil, ids, nil)
		}()
		if err != nil {
			log.Printf("occurrences: event %d: series room: %v", ev.ID, err)
			return tx.RollbackTo("rooms").Error
		}
		return nil
	}
}

// apiCheckRooms lists the rooms for an event being created, with busy
// counts over the dates it would have (the create form's Room field).
// groupId names the group creating it; without one, the caller creates a
// church-wide event, which only admins do.
func (a *App) apiCheckRooms(c *gin.Context) {
	var body struct {
		eventBody
		GroupID *uint `json:"groupId"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		fail(c, errStartRequired)
		return
	}
	me := apiUser(c)
	var group *models.Group
	if body.GroupID != nil {
		g, _, ok := a.loadManagedGroupID(c, *body.GroupID)
		if !ok {
			return
		}
		group = g
	} else if !policy.IsAdmin(asPerson(me)) {
		jsonError(c, http.StatusForbidden, "admin required")
		return
	}
	_, x, err := a.specFrom(body.eventBody, nil, true)
	if err != nil {
		fail(c, err)
		return
	}
	now := time.Now()
	upcoming := []rooms.Window{}
	for _, w := range x.Whens {
		if w.StartsAt.After(now) {
			upcoming = append(upcoming, rooms.Window{Start: w.StartsAt, End: w.EndsAt})
		}
	}
	ed, err := parseCapacityEdits(body.eventBody)
	if err != nil {
		fail(c, err)
		return
	}
	list, err := roomChoices(a.DB, asPerson(me), upcoming, 0, ed.cap)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "could not list rooms")
		return
	}
	// A new event has no dates of its own yet; Rule 1 uses the visibility
	// picked on the form (DEC-110).
	suggestions := roomSuggestions(a.DB, time.Now(), suggestFor{person: asPerson(me), groupID: body.GroupID,
		visibility: createVisibility(body.Visibility, group), capacity: ed.cap, windows: upcoming, tags: groupTags(group)}, offeredRooms(list))
	c.JSON(http.StatusOK, roomsCheckJSON{Rooms: list, Dates: len(upcoming), Suggestions: suggestions})
}

// bookCreatedSeries books the room picked on a create form for every date
// of the new event, in the creating transaction.
func (a *App) bookCreatedSeries(tx *gorm.DB, ev *models.Event, roomID *uint, user *models.User, ip string) error {
	if roomID == nil {
		return nil
	}
	if !a.roomsEnabled() {
		return errRoomGone
	}
	_, err := bookWholeSeries(tx, ev, seriesPick{roomID: roomID}, user, ip)
	return err
}
