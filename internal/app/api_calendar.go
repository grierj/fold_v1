package main

import (
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"fold/internal/events"
	"fold/internal/policy"
	"fold/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// The calendar (M1.6, DEC-67) lists every occurrence the viewer may see
// over a date range. No existing endpoint does that: /events is the home
// list of open events only, and /groups/:id/events is one group. The
// response is a slim row per occurrence — never attendees, counts, seats
// or declined data — so DEC-64 can't leak in bulk.
type calendarOccurrenceJSON struct {
	EventID            uint      `json:"eventId"`
	Date               string    `json:"date"`
	DateURL            string    `json:"dateUrl,omitempty"`
	StartsAt           time.Time `json:"startsAt"`
	EndsAt             time.Time `json:"endsAt"`
	Title              string    `json:"title"`
	Location           string    `json:"location"`
	GroupID            *uint     `json:"groupId,omitempty"`
	GroupName          string    `json:"groupName,omitempty"`
	ChurchWide         bool      `json:"churchWide"`
	RemoteAvailable    bool      `json:"remoteAvailable"`
	ChildcareAvailable bool      `json:"childcareAvailable"`
	HostedBy           string    `json:"hostedBy,omitempty"`
	// MyStatus is only the viewer's own answer. Household answers never
	// populate it. Declined is serialized so Everything can label Can't go.
	MyStatus *string `json:"myStatus"`
	// OwnScope drives RSVP-aware styling without granting any access.
	OwnScope bool `json:"ownScope"`
	// GroupDeleted marks a deleted group's event, sent only to an admin
	// who asked for deleted events (DEC-68).
	GroupDeleted bool `json:"groupDeleted,omitempty"`
}

const maxCalendarOccurrences = 500

func (a *App) calendarTime() time.Time { return a.now().UTC() }

// errCalendarGroupHidden answers a group filter that names a group the viewer
// may not see (or none), as on /groups/:id/events.
var errCalendarGroupHidden = errors.New("calendar group filter not visible")

// apiCalendar reads in one read-only Repeatable Read snapshot: the viewer, the
// group filter's authorization, the candidates, the exact-date grants, the
// dates, the viewer's own answers and the church's name. A grant, a group
// filter or a role is then never paired with dates or names committed after
// it was judged.
func (a *App) apiCalendar(c *gin.Context) {
	from, to, ok := parseDateWindow(c)
	if !ok {
		return
	}
	var tzZone *time.Location
	if tzName := strings.TrimSpace(c.Query("tz")); tzName != "" {
		var err error
		if tzZone, err = events.LoadZone(tzName); err != nil {
			jsonError(c, http.StatusBadRequest, "tz must be an IANA name, for example America/Denver")
			return
		}
	}
	var out calendarResponseJSON
	now := a.calendarTime()
	err := a.DB.Transaction(func(tx *gorm.DB) error {
		var err error
		out, err = a.calendarIn(tx, c, from, to, tzZone, now)
		return err
	}, readSnapshot)
	switch {
	case errors.Is(err, errCalendarGroupHidden):
		jsonError(c, http.StatusNotFound, "group not found")
	case err != nil:
		jsonError(c, http.StatusInternalServerError, "could not load the calendar")
	default:
		c.JSON(http.StatusOK, out)
	}
}

func (a *App) calendarIn(tx *gorm.DB, c *gin.Context, from, to string, tzZone *time.Location, now time.Time) (calendarResponseJSON, error) {
	me, err := snapshotViewerIn(tx, apiUser(c))
	if err != nil {
		return calendarResponseJSON{}, err
	}
	// The from/to dates are the viewer's calendar days (DEC-57): the
	// window on starts_at runs from their local midnight to the next
	// midnight after `to`, so an event near midnight in its own zone
	// lands on the viewer's day even at a week or month edge.
	zone := tzZone
	if zone == nil {
		zone = time.UTC
		lenientReadIn(tx, func(sp *gorm.DB) { zone = churchZoneOrUTC(sp) })
	}
	fd, _ := models.ParseCalendarDate(from)
	td, _ := models.ParseCalendarDate(to)
	windowStart := time.Date(fd.Year(), fd.Month(), fd.Day(), 0, 0, 0, 0, zone)
	windowEnd := time.Date(td.Year(), td.Month(), td.Day()+1, 0, 0, 0, 0, zone)

	// A group filter names one group the viewer may see, as on
	// /groups/:id/events; anything else is a 404. It is judged in the
	// snapshot that lists the dates, since the filter tells which events
	// are the group's.
	var onlyGroup *uint
	if raw := strings.TrimSpace(c.Query("group")); raw != "" {
		id, _ := strconv.Atoi(raw)
		var grp models.Group
		if err := tx.Where("id = ?", id).Limit(1).Find(&grp).Error; err != nil {
			return calendarResponseJSON{}, err
		}
		var rel policy.Rel
		if me != nil && grp.ID != 0 {
			if rel, err = groupRel(tx, me.ID, grp.ID); err != nil {
				return calendarResponseJSON{}, err
			}
		}
		if grp.ID == 0 || !policy.CanSeeGroup(asPerson(me), asGroup(&grp), rel) {
			return calendarResponseJSON{}, errCalendarGroupHidden
		}
		onlyGroup = &grp.ID
		if a.hooks.readerAuthorized != nil {
			a.hooks.readerAuthorized("calendar group")
		}
	}

	justMine := me != nil && c.Query("mine") == "1"
	// Everything, signed in: an underway date someone kept after leaving that
	// began before the range is carried in for its managers (DEC-157).
	carryKept := me != nil && !justMine
	carry, carryArgs := carriedKeptDateWhere(windowStart, now)
	var q *gorm.DB
	if carryKept {
		// One parenthesised disjunction inside the first Where, never GORM's
		// Or, so the filters below still apply to carried events.
		q = tx.Where("(id IN (SELECT event_id FROM event_occurrences WHERE starts_at >= ? AND starts_at < ?) OR id IN (SELECT o.event_id FROM event_occurrences o WHERE "+carry+"))",
			append([]any{windowStart.UTC(), windowEnd.UTC()}, carryArgs...)...)
	} else {
		q = tx.Where("id IN (SELECT event_id FROM event_occurrences WHERE starts_at >= ? AND starts_at < ?)",
			windowStart.UTC(), windowEnd.UTC())
	}
	if onlyGroup != nil {
		q = q.Where("group_id = ?", *onlyGroup)
	}
	// Filter candidates before the date cap; ordinary visibility policy still
	// applies below, including to private church-wide events.
	if c.Query("church") == "1" {
		q = q.Where("group_id IS NULL")
	}
	// remote=1 keeps only events that offer remote attendance (M1.8
	// slice 2). Filtering on the event row here means a hidden event is
	// dropped by the candidates exactly as without the filter.
	if c.Query("remote") == "1" {
		q = q.Where("remote_available = ?", true)
	}
	if c.Query("childcare") == "1" {
		q = q.Where("childcare_available = ?", true)
	}
	// deleted=1 lists deleted groups' events too, for admins only (DEC-68).
	candidates := dateCandidatesIn
	if c.Query("deleted") == "1" {
		candidates = deletedEventCandidatesIn
	}
	vs, err := candidates(tx, me, q)
	if err != nil {
		return calendarResponseJSON{}, err
	}
	out := []calendarOccurrenceJSON{}
	byID, hosted, visibleIDs := vs.ByID, vs.Hosted, vs.IDs
	scoped, err := a.scopedOccurrenceReadsIn(tx, me, tx.Where("starts_at >= ? AND starts_at < ? AND event_id IN (?)",
		windowStart.UTC(), windowEnd.UTC(), q.Model(&models.Event{}).Select("id")), now)
	if err != nil {
		return calendarResponseJSON{}, err
	}
	var occs []models.EventOccurrence
	// Chunked so a huge event list stays under the bind-parameter limit; the
	// merged rows are sorted below. A failed read is a 500, not an empty month.
	if err := eachIDChunk(visibleIDs, func(chunk []uint) error {
		var part []models.EventOccurrence
		if err := tx.Where("event_id IN ? AND status = ? AND starts_at >= ? AND starts_at < ?",
			chunk, models.OccurrenceScheduled, windowStart.UTC(), windowEnd.UTC()).Order("starts_at").Find(&part).Error; err != nil {
			return err
		}
		occs = append(occs, part...)
		return nil
	}); err != nil {
		return calendarResponseJSON{}, err
	}
	for _, read := range scoped {
		if byID[read.Event.ID] == nil {
			occs = append(occs, read.Occurrence)
		}
	}
	// The managers' kept dates (DEC-157): which loaded dates are kept, and the
	// carried dates themselves, read on this snapshot after the dates in range.
	carried := map[uint]bool{}
	kept := map[uint]int{}
	if carryKept {
		if a.hooks.readerAuthorized != nil {
			a.hooks.readerAuthorized("calendar kept dates")
		}
		if err := eachIDChunk(visibleIDs, func(chunk []uint) error {
			var part []models.EventOccurrence
			if err := tx.Where("event_id IN ? AND id IN (SELECT o.id FROM event_occurrences o WHERE "+carry+")",
				append([]any{chunk}, carryArgs...)...).Order("starts_at").Find(&part).Error; err != nil {
				return err
			}
			for _, occ := range part {
				carried[occ.ID] = true
			}
			occs = append(occs, part...)
			return nil
		}); err != nil {
			return calendarResponseJSON{}, err
		}
		var managed []uint
		for i := range occs {
			if ev := byID[occs[i].EventID]; ev != nil && vs.of(ev).canManage(me) {
				managed = append(managed, occs[i].ID)
			}
		}
		if err := eachIDChunk(managed, func(chunk []uint) error {
			part, err := keptDatesIn(tx, chunk)
			for id, n := range part {
				kept[id] = n
			}
			return err
		}); err != nil {
			return calendarResponseJSON{}, err
		}
	}
	sort.Slice(occs, func(i, j int) bool {
		if occs[i].StartsAt.Equal(occs[j].StartsAt) {
			return occs[i].ID < occs[j].ID
		}
		return occs[i].StartsAt.Before(occs[j].StartsAt)
	})

	occIDs := visibleOccurrenceIDs(vs, me, occs, scoped)
	scopeFacts, err := loadCalendarScopeBatchIn(tx, me, occIDs)
	if err != nil {
		return calendarResponseJSON{}, err
	}
	scope := policy.CalendarScopeAll
	if justMine {
		scope = policy.CalendarScopeMine
	}

	truncated := false
	siteName := &siteNameOnce{a: a, db: tx}
	for i := range occs {
		occ := &occs[i]
		date, ok := a.projectDate(me, vs, occ, scoped[occ.ID])
		if !ok {
			continue
		}
		// A carried date is only ever a manager's (DEC-157).
		if carried[occ.ID] && !date.Manager {
			continue
		}
		status := scopeFacts.ownStatus[occ.ID]
		managerKept := carryKept && date.Manager && kept[occ.ID] > 0 && now.Before(occ.EndsAt)
		decision := calendarScopeDecision(scope, me != nil, date.Event, occ, date.Rel, hosted[date.Event.ID], scopeFacts.household[occ.ID], scoped[occ.ID] != nil, managerKept, status, now)
		if !decision.Include {
			continue
		}
		if c.Query("remote") == "1" && !date.RemoteOffered() {
			continue
		}
		if len(out) >= maxCalendarOccurrences {
			truncated = true
			break
		}
		out = append(out, date.calendarRow(siteName, status, decision.OwnScope, now))
	}
	return calendarResponseJSON{Occurrences: out, Truncated: truncated}, nil
}

// carriedKeptDateWhere is the carry-in (DEC-157) over event_occurrences o: a
// scheduled date that is underway on the calendar's clock (started at or
// before now, not ended), began before the range (within the longest
// duration, so the scan stays on the starts_at index), reaches into it, and
// holds a live departure grant (keptDateSQL). A kept date that starts later,
// even before a future range's first day, is never carried. now is bound, never
// SQL now(), and the candidate clause and the date load both use this one form.
func carriedKeptDateWhere(windowStart, now time.Time) (string, []any) {
	return "o.status = ? AND o.starts_at <= ? AND o.starts_at >= ? AND o.starts_at < ? AND o.ends_at > ? AND o.ends_at > ?" +
			" AND EXISTS (SELECT 1 FROM registrations WHERE registrations.occurrence_id = o.id AND registrations.user_id IS NOT NULL AND " + keptDateSQL + ")",
		[]any{models.OccurrenceScheduled, now.UTC(), windowStart.Add(-events.MaxDurationMinutes * time.Minute).UTC(), windowStart.UTC(), now.UTC(), windowStart.UTC()}
}
