package app

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"fold/internal/audit"
	"fold/internal/policy"
	"fold/internal/rooms"
	"fold/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Room bookings (M2.1b, 9.2). A booking holds one room for one date, padded
// by the room's setup and teardown minutes (DEC-101). Confirmed bookings of
// one room never overlap: the database refuses it (room_bookings_no_overlap),
// so two leaders booking at once can't both win. An admin may double-book
// with a reason, which is audited. Moving a date moves its booking; a move
// into a conflict keeps the booking as requested (holding nothing) and tells
// the people running the event.

const maxOverlapReason = 500

// paddedRangeSQL is rooms.PaddedRange with four placeholders: start, setup,
// end, teardown. The table's CHECK compares padded_range against the same
// formula.
var paddedRangeSQL = rooms.PaddedRange("?", "?", "?", "?")

// heldByOverlap reports an admin's double-booking holding a room for the
// given padded time. Double-bookings sit outside the exclusion constraint,
// so booking and moving check for them here: they count as busy for
// everyone else (9.2).
func heldByOverlap(tx *gorm.DB, roomID, occurrenceID uint, start, end time.Time, setup, teardown int) (bool, error) {
	var held bool
	// The same held test as every busy check, narrowed to double-bookings so
	// the planner can use idx_room_bookings_overlap.
	err := tx.Raw(`SELECT EXISTS (SELECT 1 FROM room_bookings b WHERE `+rooms.HeldCondition("b", "?", "?", paddedRangeSQL)+` AND b.allow_overlap)`,
		roomID, occurrenceID, start, setup, end, teardown).Scan(&held).Error
	return held, err
}

// lockRoom serializes booking writes per room, so the double-booking check
// above can't race another booking of the same room. Callers lock the
// occurrence first and the room second, so the order never inverts.
func lockRoom(tx *gorm.DB, roomID uint) (*models.Room, error) {
	var r models.Room
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&r, roomID).Error; err != nil {
		return nil, err
	}
	return &r, nil
}

type bookResult int

const (
	bookedFree      bookResult = iota // confirmed: the room was free
	bookedOverlap                     // confirmed over a conflict, with an admin's reason
	bookedRequested                   // the room was busy: requested, holding nothing
	bookedPending                     // the room was free: requested, waiting on an admin's approval
	notBooked                         // the room was busy and nothing was written
)

type placement struct {
	by *uint
	// reason double-books a busy room; the caller checks it is an admin's.
	reason string
	// keepRequested keeps a busy date as requested (holding nothing)
	// instead of writing nothing.
	keepRequested bool
	// requestID, when set, makes a free date wait on that approval request
	// (M2.1d) instead of confirming.
	requestID *uint
}

// placeBooking books room for a date that has no booking. The caller holds
// the date's lock, then the room's.
func placeBooking(tx *gorm.DB, occ *models.EventOccurrence, room *models.Room, setup, teardown int, p placement) (bookResult, error) {
	by, reason, keepRequested := p.by, p.reason, p.keepRequested
	insert := func(move policy.RoomBookingMove, why string) error {
		return tx.Exec(`INSERT INTO room_bookings (occurrence_id, room_id, starts_at, ends_at, setup_minutes, teardown_minutes,
			padded_range, status, allow_overlap, overlap_reason, created_by) VALUES (?, ?, ?, ?, ?, ?, `+paddedRangeSQL+`, ?, ?, ?, ?)`,
			occ.ID, room.ID, occ.StartsAt, occ.EndsAt, setup, teardown,
			occ.StartsAt, setup, occ.EndsAt, teardown,
			move.To().Status(), move.To() == policy.RoomBookingDoubleBooked, why, by).Error
	}
	held, err := heldByOverlap(tx, room.ID, occ.ID, occ.StartsAt, occ.EndsAt, setup, teardown)
	if err != nil {
		return 0, err
	}
	if !held {
		free, err := trySavepoint(tx, fmt.Sprintf("book%d", occ.ID), func() error {
			return insert(policy.BookingPlace, "")
		})
		if err != nil {
			return 0, err
		}
		if free && p.requestID != nil {
			// The insert proved the room free; the booking then waits for an
			// admin, holding nothing until it is confirmed. It can't be
			// inserted as requested to begin with: room_bookings_no_overlap
			// covers confirmed rows only, so a requested insert never meets
			// the conflict, and the other way to ask (a second held-by test)
			// would be one more busy definition (rooms.HeldCondition).
			_, err := changeBookings(tx, policy.BookingAwaitApproval, "request_id = ?", []any{*p.requestID}, "occurrence_id = ?", occ.ID)
			return bookedPending, err
		}
		if free {
			return bookedFree, nil
		}
	}
	switch {
	case reason != "":
		return bookedOverlap, insert(policy.BookingPlaceOverlap, reason)
	case keepRequested:
		return bookedRequested, insert(policy.BookingPlaceBusy, "")
	}
	return notBooked, nil
}

// trySavepoint runs write under a savepoint. ok is false when the exclusion
// constraint refused it; the transaction is then as it was before. The
// savepoint is released either way: a whole series books hundreds of dates
// in one transaction, and open subtransactions slow every session.
func trySavepoint(tx *gorm.DB, name string, write func() error) (ok bool, err error) {
	if err := tx.SavePoint(name).Error; err != nil {
		return false, err
	}
	werr := write()
	if werr != nil && !isOverlap(werr) {
		return false, werr
	}
	if werr != nil {
		if err := tx.RollbackTo(name).Error; err != nil {
			return false, err
		}
	}
	return werr == nil, tx.Exec("RELEASE SAVEPOINT " + name).Error
}

func isOverlap(err error) bool {
	return pgConstraint(err, pgExclusionViolation, "room_bookings_no_overlap")
}

type roomPickerJSON struct {
	ID         uint   `json:"id"`
	Name       string `json:"name"`
	Building   string `json:"building"`
	Capacity   *int   `json:"capacity"`
	Features   string `json:"features"`
	Restricted bool   `json:"restricted"`
	// Busy: another confirmed booking holds the room for this date's padded
	// time. Who holds it is never said.
	Busy bool `json:"busy"`
	// TooSmall: the date's seat cap or going count is above the room's
	// capacity. A warning, never a block (9.2).
	TooSmall bool `json:"tooSmall"`
}

type roomBookingJSON struct {
	RoomID          uint   `json:"roomId"`
	RoomName        string `json:"roomName"`
	Building        string `json:"building"`
	Status          string `json:"status"`
	SetupMinutes    int    `json:"setupMinutes"`
	TeardownMinutes int    `json:"teardownMinutes"`
	DoubleBooked    bool   `json:"doubleBooked"`
	TooSmall        bool   `json:"tooSmall"`
	// Pending: requested, waiting on an admin's approval (M2.1d). A
	// requested booking that isn't pending lost its room.
	Pending bool `json:"pending"`
}

type occurrenceRoomJSON struct {
	ID       uint   `json:"id"`
	Name     string `json:"name"`
	Building string `json:"building"`
}

func (a *App) managedOccurrence(c *gin.Context) (*models.Event, *models.EventOccurrence, bool) {
	ev, ok := a.loadManagedEvent(c)
	if !ok {
		return nil, nil, false
	}
	date := c.Param("date")
	if !isCalendarDate(date) {
		fail(c, errSeriesDateInvalid)
		return nil, nil, false
	}
	var occ models.EventOccurrence
	err := a.DB.Where("event_id = ? AND original_date = ? AND status = ?", ev.ID, date, models.OccurrenceScheduled).First(&occ).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		err = errOccurrenceNotFound
	}
	if err != nil {
		fail(c, err)
		return nil, nil, false
	}
	return ev, &occ, true
}

// seatsWanted is what a room must hold for a date: the larger of its seat
// cap and the seats already going. The count is the one capacity uses: listed
// accounts only, remote attendees aside.
func seatsWanted(db *gorm.DB, ev *models.Event, occ *models.EventOccurrence) (int, error) {
	want := 0
	if cap := capacityForMode(ev, occ, models.AttendanceInPerson); cap != nil {
		want = *cap
	}
	going, err := seatsUsedForMode(db, occ.ID, models.AttendanceInPerson, 0)
	if err != nil {
		return 0, err
	}
	return max(want, going), nil
}

func tooSmall(capacity *int, want int) bool { return capacity != nil && want > *capacity }

func activeRoomsBusy(db *gorm.DB, windows []rooms.Window, exceptEvent uint) ([]models.Room, map[uint]int, error) {
	var list []models.Room
	if err := db.Where("active").Order("lower(building), lower(name)").Find(&list).Error; err != nil {
		return nil, nil, err
	}
	ids := make([]uint, len(list))
	for i, r := range list {
		ids[i] = r.ID
	}
	busy, err := rooms.BusyCounts(db, ids, windows, exceptEvent)
	return list, busy, err
}

// apiDateRooms is a date's room control: the rooms its manager may book, the
// date's booking with its token (dateRoomRevisionIn) and the suggestions. One
// read-only snapshot decides everything (docs/agents/snapshots.md): the rooms
// switch, the viewer and their authority over the event (managedDateTargetIn),
// the scheduled date, and every row the answer shows, on one clock.
func (a *App) apiDateRooms(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	var out dateRoomsJSON
	var judged sessionJudgment
	err := decidingSnapshot(a.DB.WithContext(c.Request.Context()), func(tx *gorm.DB) error {
		if err := roomsOnIn(tx, errRoomsOff); err != nil {
			return err
		}
		t, err := a.managedDateTargetIn(tx, apiUser(c), id, c.Param("date"), &judged)
		if err != nil {
			return err
		}
		if t.o.Status != models.OccurrenceScheduled {
			return errOccurrenceNotFound
		}
		if a.hooks.readerAuthorized != nil {
			a.hooks.readerAuthorized("date rooms")
		}
		now := a.now().UTC()
		me, ev, occ := asPerson(t.me), &t.ev, &t.o
		list, busy, err := activeRoomsBusy(tx, []rooms.Window{{Start: occ.StartsAt, End: occ.EndsAt, OccurrenceID: occ.ID}}, 0)
		if err != nil {
			return err
		}
		want, err := seatsWanted(tx, ev, occ)
		if err != nil {
			return err
		}
		out.Rooms = []roomPickerJSON{}
		offered := map[uint]bool{}
		for _, r := range list {
			if !policy.CanBookRoom(me, true, r.Restricted) {
				continue
			}
			offered[r.ID] = true
			out.Rooms = append(out.Rooms, roomPickerJSON{ID: r.ID, Name: r.Name, Building: r.Building, Capacity: r.Capacity,
				Features: r.Features, Restricted: r.Restricted, Busy: busy[r.ID] > 0, TooSmall: tooSmall(r.Capacity, want)})
		}
		if out.Booking, err = a.dateBooking(tx, occ.ID, want); err != nil {
			return err
		}
		out.DateRoom.ID = occ.ID
		if out.DateRoom.Revision, err = a.dateRoomRevisionIn(tx, occ.ID); err != nil {
			return err
		}
		// Rule 4's capacity tiebreak uses the same seats as the too-small note.
		var seats *int
		if want > 0 {
			seats = &want
		}
		out.Suggestions = roomSuggestions(tx, now, suggestFor{person: me, eventID: ev.ID, groupID: ev.GroupID,
			visibility: policy.EffectiveVisibility(ev.Visibility, occ.VisibilityOverride), capacity: seats,
			windows: windowsOf([]models.EventOccurrence{*occ})}, offered)
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

func (a *App) dateBooking(db *gorm.DB, occurrenceID uint, want int) (*roomBookingJSON, error) {
	var b models.RoomBooking
	err := db.Where("occurrence_id = ?", occurrenceID).First(&b).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r models.Room
	if err := db.First(&r, b.RoomID).Error; err != nil {
		return nil, err
	}
	requestPending := false
	if b.RequestID != nil && b.Status == policy.RoomBookingWaiting.Status() {
		if err := db.Raw("SELECT EXISTS (SELECT 1 FROM room_requests WHERE id = ? AND status IN ?)", *b.RequestID, policy.RoomRequestConfirm.From()).Scan(&requestPending).Error; err != nil {
			return nil, err
		}
	}
	state := policy.RoomBookingStateOf(b.Status, requestPending, b.AllowOverlap)
	return &roomBookingJSON{RoomID: r.ID, RoomName: r.Name, Building: r.Building, Status: b.Status,
		SetupMinutes: b.SetupMinutes, TeardownMinutes: b.TeardownMinutes, DoubleBooked: b.AllowOverlap,
		TooSmall: tooSmall(r.Capacity, want), Pending: state == policy.RoomBookingWaiting}, nil
}

// apiSetDateRoom books, changes or releases a date's room. roomId null
// releases it. Padding comes from the room; an admin may override it for
// this booking (DEC-101). A conflict is 409 room_busy unless an admin gives
// an overlap reason, which double-books (audited). The pick names the read it
// was made from (revision, dateRoomRevisionIn): an older one is 409
// date_room_changed, writing nothing (DEC-167).
func (a *App) apiSetDateRoom(c *gin.Context) {
	ev, occ, ok := a.managedOccurrence(c)
	if !ok {
		return
	}
	var body struct {
		RoomID          *uint  `json:"roomId"`
		OverlapReason   string `json:"overlapReason"`
		SetupMinutes    *int   `json:"setupMinutes"`
		TeardownMinutes *int   `json:"teardownMinutes"`
		Revision        string `json:"revision"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		jsonError(c, http.StatusBadRequest, "invalid request")
		return
	}
	reason := strings.TrimSpace(body.OverlapReason)
	if utf8.RuneCountInString(reason) > maxOverlapReason {
		fail(c, errOverlapReasonTooLong)
		return
	}
	var out roomBookingResponseJSON
	err := a.DB.Transaction(func(tx *gorm.DB) error {
		// The event first: every writer of a date's booking holds it, so the
		// booking compared below stays as read until this commits.
		user, lockedEv, _, err := lockManagedEventIn(tx, apiUser(c), ev.ID)
		if err != nil {
			return err
		}
		if (body.SetupMinutes != nil || body.TeardownMinutes != nil) && !policy.CanAdjustBookingPadding(asPerson(user)) {
			return errBookingSetupAdminOnly
		}
		for _, m := range []*int{body.SetupMinutes, body.TeardownMinutes} {
			if m != nil && (*m < 0 || *m > rooms.MaxPaddingMinutes) {
				return errInvalidSetupMinutes
			}
		}
		ev = lockedEv
		// Book against the date as it is under the lock: a reschedule or a
		// cancel that committed since it was read must win.
		locked, err := lockOccurrence(tx, occ.ID)
		if err != nil {
			return err
		}
		if locked.Status != models.OccurrenceScheduled {
			return errOccurrenceNotFound
		}
		occ = locked
		if err := a.checkDateRoomRevision(tx, occ.ID, body.Revision); err != nil {
			return err
		}
		pick := datePick{ev: ev, occ: occ, user: user, roomID: body.RoomID, reason: reason,
			setup: body.SetupMinutes, teardown: body.TeardownMinutes, ip: c.ClientIP()}
		if err := pickDateRoom(tx, pick); err != nil {
			return err
		}
		want, err := seatsWanted(tx, ev, occ)
		if err != nil {
			return err
		}
		if out.Booking, err = a.dateBooking(tx, occ.ID, want); err != nil {
			return err
		}
		out.DateRoom.ID = occ.ID
		out.DateRoom.Revision, err = a.dateRoomRevisionIn(tx, occ.ID)
		return err
	}, readCommitted)
	switch {
	case isAPIError(err):
		fail(c, err)
	case errors.Is(err, gorm.ErrRecordNotFound):
		fail(c, errEventNotFound)
	case err != nil:
		jsonError(c, http.StatusInternalServerError, "could not save the room")
	default:
		c.JSON(http.StatusOK, out)
	}
}

// datePick is one pick of a date's room by a manager the caller locked.
type datePick struct {
	ev              *models.Event
	occ             *models.EventOccurrence
	user            *models.User
	roomID          *uint
	reason          string
	setup, teardown *int
	ip              string
}

// pickDateRoom replaces the date's booking with the pick, under the event's
// and the date's locks.
func pickDateRoom(tx *gorm.DB, p datePick) error {
	ev, occ, user := p.ev, p.occ, p.user
	me := asPerson(user)
	// DEC-147: a group that isn't active books no new room; releasing
	// (roomId null) is always allowed. Checked before anything is deleted.
	if p.roomID != nil {
		if ok, err := groupSchedulesIn(tx, ev.GroupID); err != nil {
			return err
		} else if !ok {
			return errGroupRoomsNotApproved
		}
	}
	var old models.RoomBooking
	hadOld := tx.Where("occurrence_id = ?", occ.ID).First(&old).Error == nil
	var before roomHeld
	if hadOld {
		rows, err := dateBookingsIn(tx, "b.id = ?", old.ID)
		if err != nil {
			return err
		}
		if len(rows) != 1 {
			return fmt.Errorf("date %d: booking %d not read back", occ.ID, old.ID)
		}
		if !policy.CanChangeBooking(me, rows[0].Restricted, old.AllowOverlap) {
			return errBookingAdminOnly
		}
		before = roomHeldOf(old.RoomID, rows[0].judged().State)
		if err := tx.Delete(&old).Error; err != nil {
			return err
		}
	}
	if p.roomID == nil {
		if !hadOld {
			return nil
		}
		if err := settleDatePick(tx, ev, occ, user.ID, before, roomHeld{}, hadOld); err != nil {
			return err
		}
		return audit.Write(tx, audit.Entry{Actor: &user.ID, Action: audit.RoomBookingReleased, TargetType: "occurrence", TargetID: occ.ID,
			Meta: map[string]any{"roomId": old.RoomID, "eventId": ev.ID, "date": models.FormatCalendarDate(occ.OriginalDate)}, IP: p.ip})
	}
	room, err := lockRoom(tx, *p.roomID)
	if errors.Is(err, gorm.ErrRecordNotFound) || (err == nil && !room.Active) {
		return errPickedRoomGone
	}
	if err != nil {
		return err
	}
	if !policy.CanBookRoom(me, true, room.Restricted) {
		return errRoomRestricted
	}
	// The room's usual padding, unless this is the same room again: then
	// an admin's override on the booking is kept (DEC-101).
	setup, teardown := room.DefaultSetupMinutes, room.DefaultTeardownMinutes
	if hadOld && old.RoomID == room.ID {
		setup, teardown = old.SetupMinutes, old.TeardownMinutes
	}
	if p.setup != nil {
		setup = *p.setup
	}
	if p.teardown != nil {
		teardown = *p.teardown
	}
	reason := p.reason
	if !policy.CanDoubleBookRoom(me) {
		reason = ""
	}
	// With approval on, a leader's or host's booking is a request of one
	// date for an admin (DEC-102, DEC-106).
	var req *models.RoomRequest
	var requestID *uint
	if policy.BookingNeedsApproval(me, roomApprovalOn(tx)) {
		if req, err = openRoomRequest(tx, ev.ID, room.ID, &user.ID, false); err != nil {
			return err
		}
		requestID = &req.ID
	}
	result, err := placeBooking(tx, occ, room, setup, teardown, placement{by: &user.ID, reason: reason, requestID: requestID})
	if err != nil {
		return err
	}
	if result == notBooked {
		return errRoomBusy
	}
	if result == bookedPending {
		if err := notifyRoomRequested(tx, req, ev, room, 1, &user.ID); err != nil {
			return err
		}
	}
	if err := settleDatePick(tx, ev, occ, user.ID, before, roomHeldAfter(room.ID, result), hadOld); err != nil {
		return err
	}
	action := audit.RoomBookingBooked
	meta := map[string]any{"roomId": room.ID, "eventId": ev.ID, "date": models.FormatCalendarDate(occ.OriginalDate), "setupMinutes": setup, "teardownMinutes": teardown}
	if req != nil {
		meta["requestId"] = req.ID
	}
	if result == bookedOverlap {
		action = audit.RoomBookingOverlap
		meta["reason"] = reason
	}
	if hadOld {
		meta["previousRoomId"] = old.RoomID
	}
	return audit.Write(tx, audit.Entry{Actor: &user.ID, Action: action, TargetType: "occurrence", TargetID: occ.ID, Meta: meta, IP: p.ip})
}

// settleDatePick tells the event's other runners what the pick did, then,
// when it released an old booking, closes the requests that booking may have
// been the last date of. The notices lock their recipients before any admin's
// task is closed: an erasure takes the person's user row and then their
// notices, a closed task included, so the other way round the two could wait
// on each other (settleRoomDecision).
func settleDatePick(tx *gorm.DB, ev *models.Event, occ *models.EventOccurrence, actor uint, before, after roomHeld, released bool) error {
	if err := notifyDateRoomChanged(tx, ev, occ, actor, before, after); err != nil {
		return err
	}
	if !released {
		return nil
	}
	return closeEmptyRoomRequests(tx)
}

// releaseRoomBookings frees the rooms of dates that were cancelled, in the
// cancelling transaction (9.2).
func releaseRoomBookings(tx *gorm.DB, occurrenceIDs []uint) error {
	if len(occurrenceIDs) == 0 {
		return nil
	}
	if err := tx.Where("occurrence_id IN ?", occurrenceIDs).Delete(&models.RoomBooking{}).Error; err != nil {
		return err
	}
	return closeEmptyRoomRequests(tx)
}

// moveRoomBookings moves the bookings of dates whose times changed, in the
// same transaction (5.3, 9.2). A confirmed booking that now conflicts stays
// as requested, holding nothing; the dates that lost their room are
// returned for settleDateRooms to announce.
func moveRoomBookings(tx *gorm.DB, occurrenceIDs []uint) ([]string, error) {
	if len(occurrenceIDs) == 0 {
		return nil, nil
	}
	var bookings []models.RoomBooking
	// Room order, so two edits moving dates between the same rooms take the
	// room locks in the same order and can't deadlock.
	if err := tx.Where("occurrence_id IN ?", occurrenceIDs).Order("room_id, id").Find(&bookings).Error; err != nil {
		return nil, err
	}
	var lost []string
	for _, b := range bookings {
		var occ models.EventOccurrence
		if err := tx.First(&occ, b.OccurrenceID).Error; err != nil {
			return nil, err
		}
		if occ.StartsAt.Equal(b.StartsAt) && occ.EndsAt.Equal(b.EndsAt) {
			continue
		}
		move := func(m policy.RoomBookingMove) error {
			_, err := changeBookings(tx, m, `starts_at = ?, ends_at = ?, padded_range = `+paddedRangeSQL,
				[]any{occ.StartsAt, occ.EndsAt, occ.StartsAt, b.SetupMinutes, occ.EndsAt, b.TeardownMinutes}, "id = ?", b.ID)
			return err
		}
		if _, err := lockRoom(tx, b.RoomID); err != nil {
			return nil, err
		}
		// A confirmed booking keeps its room only if neither a confirmed
		// booking (the constraint) nor an admin's double-booking holds it
		// at the new time. An admin's own double-booking moves as it is:
		// the admin chose to share that room.
		held := false
		if b.Status == models.BookingConfirmed && !b.AllowOverlap {
			var err error
			if held, err = heldByOverlap(tx, b.RoomID, occ.ID, occ.StartsAt, occ.EndsAt, b.SetupMinutes, b.TeardownMinutes); err != nil {
				return nil, err
			}
		}
		kept := false
		if !held {
			var err error
			if kept, err = trySavepoint(tx, fmt.Sprintf("move%d", b.ID), func() error { return move(policy.BookingDateMoved) }); err != nil {
				return nil, err
			}
		}
		if !kept {
			if err := move(policy.BookingDateMovedBusy); err != nil {
				return nil, err
			}
			lost = append(lost, models.FormatCalendarDate(occ.OriginalDate))
		}
	}
	return lost, nil
}

// placeLine is a date's place (PLAN 14.5 "Where rooms show"), as
// web/src/lib/place.ts writes it: the room and its building lead, and a
// typed location follows as a detail. Both run the cases in
// web/src/lib/place-cases.json (TestPlaceLineSharesCasesWithTheWeb).
func placeLine(room *occurrenceRoomJSON, location string) string {
	if room == nil {
		return location
	}
	line := room.Name
	if room.Building != "" {
		line += " · " + room.Building
	}
	// A typed place that only repeats the room (often one the location
	// matcher mapped, M2.1e) isn't said twice.
	if typed := strings.TrimSpace(location); typed != "" && !strings.EqualFold(typed, room.Name) && !strings.EqualFold(typed, line) {
		line += " · " + typed
	}
	return line
}

// roomsFor maps dates to their confirmed booking's room, for showing the room
// as the place (9.2), and reports a failed read. Empty while rooms are off:
// the rooms switch is checked inside the same query. A feed's dates come in
// chunks of lookupChunk ids, so a list of dates under that pays one read.
func roomsFor(db *gorm.DB, occurrenceIDs []uint) (map[uint]*occurrenceRoomJSON, error) {
	out := map[uint]*occurrenceRoomJSON{}
	err := eachChunk(occurrenceIDs, lookupChunk, func(chunk []uint) error {
		return lookupChunkInto(db, chunk, out)
	})
	return out, err
}

// usualRoomIn is the series room (DefaultRoomID) the way roomsFor shows a
// date's room, under the series writer's own approval rule (bookSeriesRoom):
// while room booking is on, and, for a room still awaiting approval
// (DefaultRoomPending), only while approval is off, when new dates confirm in
// it directly; while approval is on they are only requested. A failed read
// is an error.
func usualRoomIn(db *gorm.DB, ev *models.Event) (*occurrenceRoomJSON, error) {
	if ev.DefaultRoomID == nil {
		return nil, nil
	}
	q := `SELECT r.id, r.name, r.building FROM rooms r WHERE r.id = ? AND ` + settingOnSQL
	args := []any{*ev.DefaultRoomID, settingRoomsEnabled}
	if ev.DefaultRoomPending {
		q += ` AND NOT ` + settingOnSQL
		args = append(args, settingRoomApproval)
	}
	var rows []occurrenceRoomJSON
	if err := db.Raw(q, args...).Scan(&rows).Error; err != nil || len(rows) == 0 {
		return nil, err
	}
	return &rows[0], nil
}

func lookupChunkInto(db *gorm.DB, occurrenceIDs []uint, out map[uint]*occurrenceRoomJSON) error {
	var rows []struct {
		OccurrenceID uint
		ID           uint
		Name         string
		Building     string
	}
	err := db.Raw(`SELECT b.occurrence_id, r.id, r.name, r.building FROM room_bookings b JOIN rooms r ON r.id = b.room_id
		WHERE b.occurrence_id IN ? AND b.status = ?
		AND `+settingOnSQL, occurrenceIDs, models.BookingConfirmed, settingRoomsEnabled).Scan(&rows).Error
	for _, r := range rows {
		out[r.OccurrenceID] = &occurrenceRoomJSON{ID: r.ID, Name: r.Name, Building: r.Building}
	}
	return err
}
