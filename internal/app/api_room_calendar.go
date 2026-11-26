package app

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"fold/internal/policy"
	"fold/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// The by-room view (M2.2a, 8.1): per active room, its confirmed bookings
// over one day or one week of the church's calendar. A booking whose date
// the viewer can't see is a "Reserved" block: its times and nothing else,
// so the room's use never says whose it is (section 6).

// roomSlotJSON is one booking in the by-room view. A reserved slot carries
// only startsAt, endsAt and reserved (TestRoomCalendarReservedKeys).
type roomSlotJSON struct {
	StartsAt time.Time `json:"startsAt"`
	EndsAt   time.Time `json:"endsAt"`
	Reserved bool      `json:"reserved,omitempty"`

	EventID    uint   `json:"eventId,omitempty"`
	Date       string `json:"date,omitempty"`
	DateURL    string `json:"dateUrl,omitempty"`
	Title      string `json:"title,omitempty"`
	GroupID    *uint  `json:"groupId,omitempty"`
	GroupName  string `json:"groupName,omitempty"`
	HostedBy   string `json:"hostedBy,omitempty"`
	ChurchWide bool   `json:"churchWide,omitempty"`
	// Waiting: a booking waiting for an admin's approval, which holds
	// nothing yet. Admins only (DEC-109).
	Waiting bool `json:"waiting,omitempty"`
}

type roomCalendarRoomJSON struct {
	ID         uint           `json:"id"`
	Name       string         `json:"name"`
	Building   string         `json:"building"`
	Capacity   *int           `json:"capacity"`
	Restricted bool           `json:"restricted"`
	Bookings   []roomSlotJSON `json:"bookings"`
}

type roomCalendarJSON struct {
	Timezone  string                 `json:"timezone"`
	From      string                 `json:"from"`
	To        string                 `json:"to"`
	Rooms     []roomCalendarRoomJSON `json:"rooms"`
	Truncated bool                   `json:"truncated"`
}

const maxRoomCalendarSlots = 2000

// roomCalendarWindow is the church-calendar days the view covers: one day,
// or the Sunday-to-Saturday week holding date (web/src/lib/calendar.ts).
func roomCalendarWindow(view, date string, zone *time.Location, now time.Time) (first, last time.Time, ok bool) {
	day := now.In(zone)
	if date != "" {
		d, err := models.ParseCalendarDate(date)
		if err != nil {
			return time.Time{}, time.Time{}, false
		}
		day = d
	}
	first = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, zone)
	switch view {
	case "", "day":
		return first, first, true
	case "week":
		first = first.AddDate(0, 0, -int(first.Weekday()))
		return first, first.AddDate(0, 0, 6), true
	}
	return time.Time{}, time.Time{}, false
}

// Answers the room calendar's snapshot can end with besides a database error.
var (
	errRoomCalendarHidden = errors.New("room calendar not shown")
	errRoomCalendarWindow = errors.New("room calendar window")
)

// apiRoomCalendar reads in one read-only Repeatable Read snapshot: the rooms
// switch, the viewer and what they may see, the rooms, the bookings, the candidates, the
// exact-date grants and the dates, so a booking's title is shown only by
// authority from the same committed state.
func (a *App) apiRoomCalendar(c *gin.Context) {
	var out roomCalendarJSON
	err := a.DB.Transaction(func(tx *gorm.DB) error {
		var err error
		out, err = a.roomCalendarIn(tx, c)
		return err
	}, readSnapshot)
	switch {
	case errors.Is(err, errRoomCalendarHidden):
		// Teens, and rooms turned off, get roomsOn's 404: nothing to find here.
		jsonError(c, http.StatusNotFound, "not found")
	case errors.Is(err, errRoomCalendarWindow):
		jsonError(c, http.StatusBadRequest, "view must be day or week, and date YYYY-MM-DD")
	case err != nil:
		jsonError(c, http.StatusInternalServerError, "could not load the room calendar")
	default:
		c.JSON(http.StatusOK, out)
	}
}

func (a *App) roomCalendarIn(tx *gorm.DB, c *gin.Context) (roomCalendarJSON, error) {
	// roomsOn already turned the route away while rooms were off; the switch
	// is read again here so the bookings come from a state it was on in.
	if on, err := boolSettingErrIn(tx, settingRoomsEnabled, false); err != nil {
		return roomCalendarJSON{}, err
	} else if !on {
		return roomCalendarJSON{}, errRoomCalendarHidden
	}
	if a.hooks.readerAuthorized != nil {
		a.hooks.readerAuthorized("room calendar")
	}
	me, err := snapshotViewerIn(tx, apiUser(c))
	if err != nil {
		return roomCalendarJSON{}, err
	}
	p := asPerson(me)
	if !policy.CanSeeRoomCalendar(p) {
		return roomCalendarJSON{}, errRoomCalendarHidden
	}
	zone := time.UTC
	lenientReadIn(tx, func(sp *gorm.DB) { zone = churchZoneOrUTC(sp) })
	now := a.calendarTime()
	first, last, ok := roomCalendarWindow(strings.TrimSpace(c.Query("view")), strings.TrimSpace(c.Query("date")), zone, now)
	if !ok {
		return roomCalendarJSON{}, errRoomCalendarWindow
	}
	windowStart, windowEnd := first.UTC(), last.AddDate(0, 0, 1).UTC()

	var rooms []models.Room
	if err := tx.Where("active").Order("lower(building), lower(name), id").Find(&rooms).Error; err != nil {
		return roomCalendarJSON{}, err
	}
	out := roomCalendarJSON{Timezone: zone.String(), From: first.Format("2006-01-02"), To: last.Format("2006-01-02"), Rooms: []roomCalendarRoomJSON{}}
	index := map[uint]int{}
	roomIDs := []uint{}
	for _, r := range rooms {
		if !policy.CanListRoom(p, r.Restricted) {
			continue
		}
		index[r.ID] = len(out.Rooms)
		roomIDs = append(roomIDs, r.ID)
		out.Rooms = append(out.Rooms, roomCalendarRoomJSON{ID: r.ID, Name: r.Name, Building: r.Building,
			Capacity: r.Capacity, Restricted: r.Restricted, Bookings: []roomSlotJSON{}})
	}
	if len(roomIDs) == 0 {
		return out, nil
	}

	// Confirmed bookings hold the room; an admin also sees the requested
	// ones waiting on a pending approval request (DEC-109). A requested
	// booking with no pending request lost its room and holds nothing.
	type bookingRow struct {
		models.RoomBooking
		EventID uint
		Waiting bool
	}
	var rows []bookingRow
	q := tx.Table("room_bookings b").
		Select(`b.*, o.event_id, (b.status = ?) AS waiting`, models.BookingRequested).
		Joins("JOIN event_occurrences o ON o.id = b.occurrence_id").
		Where("b.room_id IN ? AND b.starts_at < ? AND b.ends_at > ? AND o.status = ?",
			roomIDs, windowEnd, windowStart, models.OccurrenceScheduled)
	if policy.CanSeeWaitingBookings(p) {
		q = q.Where(`(b.status = ? OR (b.status = ? AND EXISTS (SELECT 1 FROM room_requests rr
			WHERE rr.id = b.request_id AND rr.status = ?)))`,
			models.BookingConfirmed, models.BookingRequested, models.RoomRequestPending)
	} else {
		q = q.Where("b.status = ?", models.BookingConfirmed)
	}
	if err := q.Order("b.starts_at, b.id").Limit(maxRoomCalendarSlots + 1).Scan(&rows).Error; err != nil {
		return roomCalendarJSON{}, err
	}
	if len(rows) > maxRoomCalendarSlots {
		rows, out.Truncated = rows[:maxRoomCalendarSlots], true
	}
	if len(rows) == 0 {
		return out, nil
	}

	// Each date goes through the ordinary date filter, then the viewer's
	// exact-date invitations; never the series alone (AGENTS.md).
	eventIDs, occIDs := []uint{}, []uint{}
	seen := map[uint]bool{}
	for _, r := range rows {
		occIDs = append(occIDs, r.OccurrenceID)
		if !seen[r.EventID] {
			seen[r.EventID] = true
			eventIDs = append(eventIDs, r.EventID)
		}
	}
	vs, err := dateCandidatesIn(tx, me, tx.Where("id IN ?", eventIDs))
	if err != nil {
		return roomCalendarJSON{}, err
	}
	scoped, err := a.scopedOccurrenceReadsIn(tx, me, tx.Where("id IN ?", occIDs), now)
	if err != nil {
		return roomCalendarJSON{}, err
	}
	var occs []models.EventOccurrence
	if err := tx.Where("id IN ?", occIDs).Find(&occs).Error; err != nil {
		return roomCalendarJSON{}, err
	}
	occByID := make(map[uint]*models.EventOccurrence, len(occs))
	for i := range occs {
		occByID[occs[i].ID] = &occs[i]
	}

	siteName := &siteNameOnce{a: a, db: tx}
	for _, r := range rows {
		slot := roomSlotJSON{StartsAt: r.StartsAt, EndsAt: r.EndsAt, Reserved: true}
		occ := occByID[r.OccurrenceID]
		ev := vs.ByID[r.EventID]
		switch {
		case occ == nil:
		case ev != nil && vs.canSeeDate(me, ev, occ):
			full := occurrenceToJSON(ev, occ, "")
			slot = roomSlotJSON{StartsAt: r.StartsAt, EndsAt: r.EndsAt, EventID: ev.ID, Date: models.FormatCalendarDate(occ.OriginalDate),
				Title: full.Title, ChurchWide: ev.GroupID == nil}
			if !vs.canSeeSeries(me, ev) {
				slot.DateURL = occurrenceDatePath(ev.ID, models.FormatCalendarDate(occ.OriginalDate))
			}
			grp, rel := groupAndRel(ev, vs.Groups, vs.Rels)
			// An opened event of a group this viewer can't see names nobody:
			// the church hosts it (DEC-17).
			l := labelGroup(a.groupShownTo(me, grp, rel), ev.GroupID != nil, siteName)
			slot.GroupID, slot.GroupName, slot.HostedBy = l.ID, l.Name, l.HostedBy
		case scoped[occ.ID] != nil:
			// An exact-date invitation shows that date and its title, never
			// the series or its group.
			read := scoped[occ.ID]
			full := occurrenceToJSON(&read.Event, occ, "")
			slot = roomSlotJSON{StartsAt: r.StartsAt, EndsAt: r.EndsAt, EventID: read.Event.ID, Date: models.FormatCalendarDate(occ.OriginalDate),
				DateURL: occurrenceDatePath(read.Event.ID, models.FormatCalendarDate(occ.OriginalDate)), Title: full.Title,
				ChurchWide: read.Event.GroupID == nil}
		}
		// Only an admin gets waiting rows. One for a date even an admin
		// can't open (a deleted group's) holds nothing and isn't shown.
		if r.Waiting && slot.Reserved {
			continue
		}
		slot.Waiting = r.Waiting
		room := &out.Rooms[index[r.RoomID]]
		room.Bookings = append(room.Bookings, slot)
	}
	return out, nil
}
