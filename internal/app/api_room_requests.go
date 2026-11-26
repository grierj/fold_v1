package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"fold/internal/audit"
	"fold/internal/notify"
	"fold/internal/policy"
	"fold/internal/rooms"
	"fold/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Room booking approval (M2.1d, DEC-102, DEC-105, DEC-106). With the
// room_bookings_need_approval setting on, a leader's or host's booking is a
// request: its free dates are requested bookings (holding nothing) that
// point at one room_requests row, and every admin gets one task. An admin
// confirms it (a reason double-books dates that became busy) or rejects it.
// Dates that lost their room are not requests: they stay the leader's.

const roomRequestsLink = "/admin?tab=rooms"

// roomApprovalOn reads the setting in the caller's transaction.
func roomApprovalOn(db *gorm.DB) bool {
	return boolSettingIn(db, settingRoomApproval, false)
}

// apiSetRoomApproval turns "Room bookings need an admin's approval" on or
// off (DEC-102). Turning it off leaves open requests for admins to decide.
func (a *App) apiSetRoomApproval(c *gin.Context) {
	if enabled, ok := a.setRoomSwitch(c, settingRoomApproval, audit.RoomApprovalChanged); ok {
		c.JSON(http.StatusOK, roomApprovalResponseJSON{NeedApproval: enabled})
	}
}

func openRoomRequest(tx *gorm.DB, eventID, roomID uint, by *uint, series bool) (*models.RoomRequest, error) {
	req := models.RoomRequest{EventID: eventID, RoomID: roomID, Series: series, RequestedBy: by, Status: policy.RoomRequestOpen.To()}
	return &req, tx.Create(&req).Error
}

// closeEmptyRoomRequests withdraws pending requests none of whose dates
// still wait (re-picked, released or cancelled), and closes their tasks.
func closeEmptyRoomRequests(tx *gorm.DB) error {
	// SKIP LOCKED: a request an admin is deciding right now is locked by
	// that decision, which settles it anyway; waiting on it here could
	// deadlock against the decision waiting on this caller's dates.
	ids, err := moveRoomRequestsWhere(tx, policy.RoomRequestWithdrawEmpty, `id IN (
			SELECT q.id FROM room_requests q WHERE q.status IN ?
			AND NOT EXISTS (SELECT 1 FROM room_bookings b WHERE b.request_id = q.id AND b.status = ?)
			FOR UPDATE SKIP LOCKED)`,
		policy.RoomRequestWithdrawEmpty.From(), policy.RoomBookingWaiting.Status())
	if err != nil {
		return err
	}
	return resolveWithdrawn(tx, ids)
}

// withdrawSeriesRequest withdraws eventID's pending series request, so a new
// pick can open the one the event may have (idx_room_requests_one_series),
// and returns its id for the caller to close its task (resolveWithdrawn)
// once its notices are written. The caller holds the event's lock, which
// every decision takes before the request's (lockRoomDecisionIn), so this needs no
// SKIP LOCKED.
func withdrawSeriesRequest(tx *gorm.DB, eventID uint) ([]uint, error) {
	return moveRoomRequestsWhere(tx, policy.RoomRequestWithdrawSeries, "event_id = ? AND series", eventID)
}

func resolveWithdrawn(tx *gorm.DB, ids []uint) error {
	for _, id := range ids {
		if _, err := notify.Resolve(tx, notify.RoomRequested, id, nil, models.RoomRequestWithdrawn); err != nil {
			return err
		}
	}
	return nil
}

type roomRequestJSON struct {
	ID          uint   `json:"id"`
	EventID     uint   `json:"eventId"`
	EventTitle  string `json:"eventTitle"`
	GroupName   string `json:"groupName"`
	RoomID      uint   `json:"roomId"`
	RoomName    string `json:"roomName"`
	Building    string `json:"building"`
	Series      bool   `json:"series"`
	RequestedBy string `json:"requestedBy"`
	// Dates waiting, and how many of them another booking now holds the
	// room on (confirming those needs a double-book reason).
	Dates         int       `json:"dates"`
	BusyDates     int       `json:"busyDates"`
	FirstDate     string    `json:"firstDate"`
	FirstStartsAt time.Time `json:"firstStartsAt"`
	CreatedAt     time.Time `json:"createdAt"`
}

func (a *App) apiAdminRoomRequests(c *gin.Context) {
	var rows []struct {
		ID, EventID, RoomID                       uint
		EventTitle, GroupName, RoomName, Building string
		Series                                    bool
		RequestedByID                             *uint
		Dates, BusyDates                          int
		FirstDate                                 string
		FirstStartsAt                             time.Time
		CreatedAt                                 time.Time
	}
	if err := a.DB.Raw(`SELECT q.id, q.event_id, e.title AS event_title, coalesce(g.name, '') AS group_name,
			q.room_id, r.name AS room_name, r.building, q.series, q.requested_by AS requested_by_id, q.created_at,
			count(*) AS dates, to_char(min(o.original_date), 'YYYY-MM-DD') AS first_date, min(o.starts_at) AS first_starts_at,
			count(*) FILTER (WHERE EXISTS (SELECT 1 FROM room_bookings c WHERE `+rooms.HeldCondition("c", "b.room_id", "b.occurrence_id", "b.padded_range")+`)) AS busy_dates
		FROM room_requests q
		JOIN events e ON e.id = q.event_id
		LEFT JOIN groups g ON g.id = e.group_id
		JOIN rooms r ON r.id = q.room_id
		JOIN room_bookings b ON b.request_id = q.id AND b.status = ?
		JOIN event_occurrences o ON o.id = b.occurrence_id
		WHERE q.status = ?
		GROUP BY q.id, e.title, g.name, r.name, r.building
		ORDER BY q.created_at, q.id`, models.BookingRequested, models.RoomRequestPending).Scan(&rows).Error; err != nil {
		jsonError(c, http.StatusInternalServerError, "could not list room requests")
		return
	}
	var askers []uint
	for _, r := range rows {
		if r.RequestedByID != nil {
			askers = append(askers, *r.RequestedByID)
		}
	}
	names := publicNamesIn(a.DB, askers)
	out := make([]roomRequestJSON, 0, len(rows))
	for _, r := range rows {
		out = append(out, roomRequestJSON{ID: r.ID, EventID: r.EventID, EventTitle: r.EventTitle,
			GroupName: r.GroupName, // group-ok: admins see every group
			RoomID:    r.RoomID, RoomName: r.RoomName, Building: r.Building, Series: r.Series,
			RequestedBy: names.actorName(r.RequestedByID), Dates: r.Dates, BusyDates: r.BusyDates,
			FirstDate: r.FirstDate, FirstStartsAt: r.FirstStartsAt, CreatedAt: r.CreatedAt})
	}
	c.JSON(http.StatusOK, roomRequestsResponseJSON{Requests: out})
}

var errRequestDecided = errors.New("request decided")

// lockRoomDecisionIn locks the event first, like every other booking write.
// A plain actor read avoids reversing the user and event lock order.
func lockRoomDecisionIn(tx *gorm.DB, session *models.User, id uint) (*models.User, *models.RoomRequest, *models.Event, error) {
	var peek models.RoomRequest
	if err := tx.First(&peek, id).Error; err != nil {
		return nil, nil, nil, err
	}
	var ev models.Event
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&ev, peek.EventID).Error; err != nil {
		return nil, nil, nil, err
	}
	var req models.RoomRequest
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&req, id).Error; err != nil {
		return nil, nil, nil, err
	}
	actor, err := lockActorIn(tx, session, actorRead)
	if err != nil {
		return nil, nil, nil, err
	}
	if !policy.CanDecideRoomRequest(asPerson(actor)) {
		return nil, nil, nil, errSecuritySession
	}
	if !policy.RoomRequestUndecided(req.Status) {
		return nil, nil, nil, errRequestDecided
	}
	return actor, &req, &ev, nil
}

// requestBookings locks a request's waiting dates (dates, then room) and
// returns their bookings in date-id order.
func requestBookings(tx *gorm.DB, req *models.RoomRequest) ([]models.RoomBooking, map[uint]models.EventOccurrence, error) {
	var occs []models.EventOccurrence
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id IN (SELECT occurrence_id FROM room_bookings WHERE request_id = ? AND status = ?)", req.ID, models.BookingRequested).
		Order("id").Find(&occs).Error; err != nil {
		return nil, nil, err
	}
	byID := make(map[uint]models.EventOccurrence, len(occs))
	for _, o := range occs {
		byID[o.ID] = o
	}
	if _, err := lockRoom(tx, req.RoomID); err != nil {
		return nil, nil, err
	}
	var bookings []models.RoomBooking
	err := tx.Where("request_id = ? AND status = ?", req.ID, models.BookingRequested).Order("occurrence_id").Find(&bookings).Error
	return bookings, byID, err
}

func requestID(c *gin.Context) (uint, bool) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 63)
	if err != nil || id == 0 {
		jsonError(c, http.StatusNotFound, "room request not found")
		return 0, false
	}
	return uint(id), true
}

// apiConfirmRoomRequest confirms a request's dates. A date another booking
// now holds the room on is double-booked with the admin's reason, or, with
// no reason, goes back to its leaders as a date that lost its room.
func (a *App) apiConfirmRoomRequest(c *gin.Context) {
	id, ok := requestID(c)
	if !ok {
		return
	}
	var body struct {
		OverlapReason string `json:"overlapReason"`
	}
	if err := c.ShouldBindJSON(&body); err != nil && !errors.Is(err, io.EOF) {
		jsonError(c, http.StatusBadRequest, "invalid request")
		return
	}
	reason := strings.TrimSpace(body.OverlapReason)
	if utf8.RuneCountInString(reason) > maxOverlapReason {
		fail(c, errOverlapReasonTooLong)
		return
	}
	var confirmed, overlapped int
	lost := []string{}
	err := a.DB.Transaction(func(tx *gorm.DB) error {
		me, req, ev, err := lockRoomDecisionIn(tx, apiUser(c), id)
		if err != nil {
			return err
		}
		// DEC-147: a group that is no longer active has its waiting request
		// left for an admin to reject; confirming would be a new booking.
		if ok, err := groupSchedulesIn(tx, ev.GroupID); err != nil {
			return err
		} else if !ok {
			return errGroupRoomsNotApproved
		}
		bookings, occs, err := requestBookings(tx, req)
		if err != nil {
			return err
		}
		for _, b := range bookings {
			occ := occs[b.OccurrenceID]
			held, err := heldByOverlap(tx, b.RoomID, b.OccurrenceID, occ.StartsAt, occ.EndsAt, b.SetupMinutes, b.TeardownMinutes)
			if err != nil {
				return err
			}
			free := false
			if !held {
				if free, err = trySavepoint(tx, fmt.Sprintf("confirm%d", b.ID), func() error {
					_, err := changeBookings(tx, policy.BookingConfirm, "", nil, "id = ?", b.ID)
					return err
				}); err != nil {
					return err
				}
			}
			switch {
			case free:
				confirmed++
			case reason != "":
				if _, err := changeBookings(tx, policy.BookingConfirmOverlap, "allow_overlap = true, overlap_reason = ?", []any{reason}, "id = ?", b.ID); err != nil {
					return err
				}
				overlapped++
			default:
				// Busy and no reason: the date lost its room, the leader's to re-pick.
				if _, err := changeBookings(tx, policy.BookingCannotConfirm, "request_id = NULL", nil, "id = ?", b.ID); err != nil {
					return err
				}
				lost = append(lost, models.FormatCalendarDate(occ.OriginalDate))
			}
		}
		if req.Series && ev.DefaultRoomID != nil && *ev.DefaultRoomID == req.RoomID {
			// The series room is approved: its new dates confirm directly.
			if err := tx.Model(ev).Update("default_room_pending", false).Error; err != nil {
				return err
			}
		}
		now := time.Now().UTC()
		if _, err := moveRoomRequest(tx, req, policy.RoomRequestConfirm, map[string]any{"decided_by": me.ID, "decided_at": now, "updated_at": now}); err != nil {
			return err
		}
		meta := map[string]any{"eventId": ev.ID, "roomId": req.RoomID, "confirmed": confirmed, "overlapped": overlapped, "lost": lost}
		if reason != "" {
			meta["reason"] = reason
		}
		if err := audit.Write(tx, audit.Entry{Actor: &me.ID, Action: audit.RoomRequestConfirmed, TargetType: "room_request", TargetID: req.ID,
			Meta: meta, IP: c.ClientIP()}); err != nil {
			return err
		}
		var room models.Room
		if err := tx.First(&room, req.RoomID).Error; err != nil {
			return err
		}
		note := fmt.Sprintf("%s is booked for %s.", room.Name, dateCount(confirmed+overlapped))
		if len(lost) > 0 {
			note += fmt.Sprintf(" %s busy now: pick another room for %s on the event page.", isAre(len(lost)), themIt(len(lost)))
		}
		// Tell the leaders (which locks their user rows) BEFORE closing the admins'
		// tasks: see settleRoomDecision.
		return settleRoomDecision(tx, ev, req.ID, me.ID, "approved", note, "confirmed")
	}, readCommitted)
	if a.requestFailed(c, id, err) {
		return
	}
	c.JSON(http.StatusOK, roomRequestConfirmationJSON{Confirmed: confirmed, Overlapped: overlapped, Lost: lost})
}

// apiRejectRoomRequest declines a request: its dates hold nothing, show as
// not approved, and a series request stops being the series room.
func (a *App) apiRejectRoomRequest(c *gin.Context) {
	id, ok := requestID(c)
	if !ok {
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if err := c.ShouldBindJSON(&body); err != nil && !errors.Is(err, io.EOF) {
		jsonError(c, http.StatusBadRequest, "invalid request")
		return
	}
	reason := strings.TrimSpace(body.Reason)
	if utf8.RuneCountInString(reason) > maxOverlapReason {
		fail(c, errOverlapReasonTooLong)
		return
	}
	err := a.DB.Transaction(func(tx *gorm.DB) error {
		me, req, ev, err := lockRoomDecisionIn(tx, apiUser(c), id)
		if err != nil {
			return err
		}
		if _, _, err := requestBookings(tx, req); err != nil {
			return err
		}
		rejected, err := changeBookings(tx, policy.BookingReject, "", nil, "request_id = ?", req.ID)
		if err != nil {
			return err
		}
		if req.Series && ev.DefaultRoomID != nil && *ev.DefaultRoomID == req.RoomID {
			if err := tx.Model(ev).Updates(map[string]any{"default_room_id": nil, "default_room_pending": false}).Error; err != nil {
				return err
			}
		}
		now := time.Now().UTC()
		if _, err := moveRoomRequest(tx, req, policy.RoomRequestReject, map[string]any{"decided_by": me.ID, "decided_at": now,
			"reject_reason": reason, "updated_at": now}); err != nil {
			return err
		}
		if err := audit.Write(tx, audit.Entry{Actor: &me.ID, Action: audit.RoomRequestRejected, TargetType: "room_request", TargetID: req.ID,
			Meta: map[string]any{"eventId": ev.ID, "roomId": req.RoomID, "dates": rejected, "reason": reason}, IP: c.ClientIP()}); err != nil {
			return err
		}
		var room models.Room
		if err := tx.First(&room, req.RoomID).Error; err != nil {
			return err
		}
		note := room.Name + " wasn't approved. Pick another room on the event page."
		if reason != "" {
			note = room.Name + " wasn't approved: " + reason + " Pick another room on the event page."
		}
		return settleRoomDecision(tx, ev, req.ID, me.ID, "not approved", note, "rejected")
	}, readCommitted)
	if a.requestFailed(c, id, err) {
		return
	}
	c.JSON(http.StatusOK, gin.H{})
}

func (a *App) requestFailed(c *gin.Context, id uint, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, gorm.ErrRecordNotFound):
		jsonError(c, http.StatusNotFound, "room request not found")
	case errors.Is(err, errRequestDecided):
		a.alreadyHandled(c, notify.RoomRequested, id)
	case isAPIError(err):
		fail(c, err)
	default:
		jsonError(c, http.StatusInternalServerError, "could not save the room request")
	}
	return true
}

// settleRoomDecision ends a decided request's notices in the one order that
// cannot deadlock against an erasure (M7.4 R4-N, N-23): first it tells the
// event's leaders (or hosts), which takes their user rows FOR KEY SHARE; only
// then does it close the admins' open "room requested" tasks, which row-locks
// each admin's task. An admin can also be a leader or host of the event, and an
// erasure takes that person's user row FOR UPDATE and then deletes their
// notices, task included. Closing the task first would have the decision hold
// the task row while it waits for the user row, and the erasure hold the user
// row while it waits for the task row. This way round both want the user row
// first, and whoever gets it finishes before the other needs anything else.
// Keep the send before the resolve.
func settleRoomDecision(tx *gorm.DB, ev *models.Event, requestID, actor uint, change, note, resolution string) error {
	if err := notifyRoomDecided(tx, ev, actor, change, note); err != nil {
		return err
	}
	_, err := notify.Resolve(tx, notify.RoomRequested, requestID, &actor, resolution)
	return err
}
