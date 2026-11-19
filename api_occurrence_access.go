package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"time"

	"fold/internal/audit"
	"fold/internal/events"
	"fold/internal/policy"
	"fold/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// occurrenceRead holds current facts for ONE invitation, never series authority.
// Mutation owners must reload these facts under the shared contact gate and their
// domain locks; this read loader does not authorize or serialize any mutation.
type occurrenceRead struct {
	Event        models.Event
	Occurrence   models.EventOccurrence
	Access       policy.OccurrenceAccess
	Facts        policy.OccurrenceFacts
	Account      policy.ContactAccount
	Registration models.Registration
}

// scopedOccurrenceReads loads exact candidates independently of series-filtered eventCandidates.
// q selects occurrence IDs (window/date and event filters). Parent IDs NEVER
// enter the ordinary visible set. All related facts are batched, including live
// registration provenance; no attendee/contact directory is loaded here.
func (a *App) scopedOccurrenceReads(viewer *models.User, q *gorm.DB, now time.Time) (map[uint]*occurrenceRead, error) {
	return scopedOccurrenceReadsIn(a.DB, viewer, q, now)
}

// scopedOccurrenceReadsIn is scopedOccurrenceReads on db, for a caller that
// holds a transaction or a context-bound handle: db and q must come from the
// same one, and no read here touches another connection.
func scopedOccurrenceReadsIn(db *gorm.DB, viewer *models.User, q *gorm.DB, now time.Time) (map[uint]*occurrenceRead, error) {
	out := map[uint]*occurrenceRead{}
	if viewer == nil {
		return out, nil
	}
	var grants []models.Registration
	err := db.Where("registrations.user_id = ? AND "+registrationAccessLiveSQL+" AND registrations.occurrence_id IN (?)", viewer.ID,
		q.Session(&gorm.Session{}).Model(&models.EventOccurrence{}).Select("id")).Find(&grants).Error
	if err != nil || len(grants) == 0 {
		return out, err
	}
	reads, err := occurrenceInvitationReadsIn(db, grants, now)
	if err != nil {
		return nil, err
	}
	for _, row := range grants {
		if read := reads[row.ID]; read != nil {
			out[row.OccurrenceID] = read
		}
	}
	return out, nil
}

// scopedInviteProbeBatch bounds one statement of the hidden-series existence
// probe below: at most this many occurrence ids are listed, and at most this
// many of the viewer's own live grants are validated, per round.
const scopedInviteProbeBatch = 50

// hasScopedUntitledRead reports whether any scheduled occurrence of eventID
// with no title of its own is visible to viewer through an exact-date
// invitation. It pages only the viewer's live-grant candidates by date id and validates
// each page with scopedOccurrenceReads, the same loader and policy the date
// page uses, stopping at the first page with a hit. A load error is returned,
// never read as "not visible". Paging the dates (never capping the grants)
// keeps every statement bounded however many dates the event keeps, and every
// candidate is still fully validated, so an invalid earlier grant never hides
// a valid later one; only exhausting every page answers false. Like the other
// read loaders here, it claims no transaction or snapshot: a grant committed
// while the pages load may or may not affect the answer, and the next load
// rechecks.
// Every read goes through db (a transaction or a context-bound handle, with
// no second connection), as in scopedOccurrenceReadsIn.
func hasScopedUntitledRead(db *gorm.DB, viewer *models.User, eventID uint, now time.Time) (bool, error) {
	if viewer == nil {
		return false, nil
	}
	var lastID uint
	for {
		var ids []uint
		grants := db.Model(&models.Registration{}).Select("occurrence_id").
			Where("registrations.user_id = ? AND "+registrationAccessLiveSQL, viewer.ID)
		if err := db.Model(&models.EventOccurrence{}).
			Where("event_id = ? AND status = ? AND title_override IS NULL AND id > ? AND id IN (?)", eventID, models.OccurrenceScheduled, lastID, grants).
			Order("id").Limit(scopedInviteProbeBatch).Pluck("id", &ids).Error; err != nil {
			return false, err
		}
		if len(ids) == 0 {
			return false, nil
		}
		scoped, err := scopedOccurrenceReadsIn(db, viewer, db.Where("id IN ?", ids), now)
		if err != nil {
			return false, err
		}
		if len(scoped) > 0 {
			return true, nil
		}
		lastID = ids[len(ids)-1]
		if len(ids) < scopedInviteProbeBatch {
			return false, nil
		}
	}
}

// occurrenceInvitationReadsIn batches current exact-date authority. Its keys are
// registration IDs, so a collection of different recipients never shares
// authority. The rows are registrations carrying live provenance; event and
// organizer are derived from each row's date, never stored twice. Mutation
// callers still own the contact gate and relevant domain locks.
func occurrenceInvitationReadsIn(tx *gorm.DB, grants []models.Registration, now time.Time) (map[uint]*occurrenceRead, error) {
	out := map[uint]*occurrenceRead{}
	if len(grants) == 0 {
		return out, nil
	}
	facts, err := loadOccurrenceInvitationFacts(tx, grants, now)
	if err != nil {
		return nil, err
	}
	for _, row := range grants {
		if row.UserID == nil || !currentDepartureRegistration(row.Status) {
			continue
		}
		occ := facts.occurrences[row.OccurrenceID]
		ev := facts.events[occ.EventID]
		actor := facts.accounts[*row.UserID]
		if occ.ID == 0 || ev.ID == 0 || occ.EventID != ev.ID {
			continue
		}
		scope := accessScopeOf(&ev)
		f := occurrenceFactsOf(&occ, &ev, facts.group(&ev))
		access := occurrenceAccessPolicy(row, scope)
		if policy.CanSeeOccurrence(actor, false, access, f, now) {
			out[row.ID] = &occurrenceRead{Event: ev, Occurrence: occ, Registration: row, Account: actor, Access: access, Facts: f}
		}
	}
	return out, nil
}

// occurrenceFactsOf is the one builder of the lifecycle facts policy judges an
// exact date by, from rows already loaded: the date, its event, and the
// event's group (nil for a church-wide event, and for a group that is gone).
func occurrenceFactsOf(occ *models.EventOccurrence, ev *models.Event, group *models.Group) policy.OccurrenceFacts {
	f := accessScopeOf(ev)
	live := ev.GroupID == nil || (group != nil && group.Status == models.GroupActive)
	return policy.OccurrenceFacts{
		ID: occ.ID, EventID: ev.ID, Organizer: f.Organizer, ContextActive: live,
		Scheduled: occ.Status == models.OccurrenceScheduled, EndsAt: occ.EndsAt,
	}
}

type occurrenceInvitationFacts struct {
	users       map[uint]*models.User
	accounts    map[uint]policy.ContactAccount
	occurrences map[uint]models.EventOccurrence
	events      map[uint]models.Event
	groups      map[uint]*models.Group
}

func loadOccurrenceInvitationFacts(tx *gorm.DB, grants []models.Registration, now time.Time) (*occurrenceInvitationFacts, error) {
	subjectIDs, ids := []uint{}, []uint{}
	for _, row := range grants {
		if row.UserID != nil {
			subjectIDs = append(subjectIDs, *row.UserID)
		}
		ids = append(ids, row.OccurrenceID)
	}
	users, accounts, err := occurrenceAccountsIn(tx, subjectIDs, now)
	if err != nil {
		return nil, err
	}
	var occs []models.EventOccurrence
	if err := tx.Where("id IN ?", ids).Find(&occs).Error; err != nil {
		return nil, err
	}
	occByID, eventIDs := map[uint]models.EventOccurrence{}, []uint{}
	for _, row := range occs {
		occByID[row.ID] = row
		eventIDs = append(eventIDs, row.EventID)
	}
	var evs []models.Event
	if len(eventIDs) > 0 {
		if err := tx.Where("id IN ?", eventIDs).Find(&evs).Error; err != nil {
			return nil, err
		}
	}
	evByID, groupIDs := map[uint]models.Event{}, []uint{}
	for _, row := range evs {
		evByID[row.ID] = row
		if row.GroupID != nil {
			groupIDs = append(groupIDs, *row.GroupID)
		}
	}
	groupByID, err := groupsByID(tx, groupIDs, false)
	if err != nil {
		return nil, err
	}
	return &occurrenceInvitationFacts{users, accounts, occByID, evByID, groupByID}, nil
}

func (f *occurrenceInvitationFacts) group(ev *models.Event) *models.Group {
	if ev.GroupID == nil {
		return nil
	}
	return f.groups[*ev.GroupID]
}

// endReason preserves the scalar lifecycle precedence, including retained past
// dates. It does not reconsider issuer authority or a recipient's add block.
// The organizer cannot disagree with the event any more (it is derived from the
// event), so the deleted and context-changed reasons come from the event, group
// and registration liveness checks alone.
func (f *occurrenceInvitationFacts) endReason(row models.Registration, now time.Time) string {
	if row.UserID == nil {
		return policy.OccurrenceContextChanged
	}
	u := f.users[*row.UserID]
	if u == nil || !u.CanSignIn() {
		return policy.ContactAccountDisabled
	}
	if u.AccountType == models.AccountTeen && !f.accounts[u.ID].Active {
		return policy.OccurrenceConsentLost
	}
	occ := f.occurrences[row.OccurrenceID]
	if occ.ID == 0 {
		return policy.OccurrenceDeleted
	}
	ev := f.events[occ.EventID]
	if ev.ID == 0 {
		return policy.OccurrenceEventDeleted
	}
	group := f.group(&ev)
	if ev.GroupID != nil && group == nil {
		return policy.OccurrenceGroupDeleted
	}
	lifecycle := occurrenceFactsOf(&occ, &ev, group)
	if !lifecycle.ContextActive {
		return policy.OccurrenceContextChanged
	}
	if !lifecycle.Scheduled {
		return policy.OccurrenceCancelled
	}
	if row.AccessOrigin != policy.OccurrenceOriginManagerAdd && !occ.EndsAt.IsZero() && !now.Before(occ.EndsAt) {
		return policy.OccurrenceEnded
	}
	if !currentDepartureRegistration(row.Status) {
		return policy.OccurrenceContextChanged
	}
	return ""
}

// isCalendarDate is true for a real YYYY-MM-DD. Date inputs that go into a
// lookup check it first: events.original_date is a date column, so a
// shape-only string such as 2026-02-30 would fail inside Postgres (SQLSTATE
// 22008) instead of simply matching no row.
func isCalendarDate(s string) bool {
	_, err := models.ParseCalendarDate(s)
	return err == nil
}

// firstScheduledOn loads the scheduled date of an event, or reports not found
// for a date that cannot exist.
func firstScheduledOn(db *gorm.DB, eventID uint, date string, out *models.EventOccurrence) error {
	if !isCalendarDate(date) {
		return gorm.ErrRecordNotFound
	}
	return db.Where("event_id = ? AND original_date = ? AND status = ?", eventID, date, models.OccurrenceScheduled).First(out).Error
}

func occurrenceDatePath(eventID uint, date string) string {
	return "/events/" + strconv.FormatUint(uint64(eventID), 10) + "/dates/" + date
}

// dateOccurrenceJSON never carries host people, series, attendee, contact or
// management data. Only ordinary reads may attach a group label/link, after an
// independent groupShownTo check; invitation-only reads omit that metadata.
type dateOccurrenceJSON struct {
	ID               uint      `json:"id"`
	EventID          uint      `json:"eventId"`
	Date             string    `json:"date"`
	DateURL          string    `json:"dateUrl"`
	Title            string    `json:"title"`
	Location         string    `json:"location"`
	StartsAt         time.Time `json:"startsAt"`
	EndsAt           time.Time `json:"endsAt"`
	StartsLocal      string    `json:"startsLocal"`
	EndsLocal        string    `json:"endsLocal"`
	TZID             string    `json:"tzid"`
	DurationMinutes  int       `json:"durationMinutes"`
	RemoteAvailable  bool      `json:"remoteAvailable"`
	GuestsAllowed    int       `json:"guestsAllowed"`
	MyRegistration   *string   `json:"myRegistration"`
	MyAttendanceMode *string   `json:"myAttendanceMode,omitempty"`
	MyGuestCount     *int      `json:"myGuestCount,omitempty"`
	VideoProvider    string    `json:"videoProvider"`
	VideoURL         string    `json:"videoUrl,omitempty"`
	VideoHidden      bool      `json:"videoHidden"`
	CanSeeVideo      bool      `json:"canSeeVideo"`
	CanSeeAttendees  bool      `json:"canSeeAttendees"`
	CanDecline       bool      `json:"canDecline"`
	CanBlock         bool      `json:"canBlock"`
	Visibility       string    `json:"visibility,omitempty"`
	CanRegister      bool      `json:"canRegister,omitempty"`
	HostedBy         string    `json:"hostedBy,omitempty"`
	GroupID          *uint     `json:"groupId,omitempty"`
	GroupName        string    `json:"groupName,omitempty"`
	// Room is the date's confirmed room, shown as its place (9.2, M2.1b).
	Room *occurrenceRoomJSON `json:"room,omitempty"`
	// Reminders is this date's email reminders (M3.6b), only for its
	// event's managers; absent for everyone else.
	Reminders *dateRemindersJSON `json:"reminders,omitempty"`
}

// apiGetOccurrenceDate is the date page's read. A cancelled date answers its
// tombstone to a qualified viewer (M8.1 brief 2.2); this JSON read is the only
// surface that does. Everyone else gets the same 404 as a missing date.
func (a *App) apiGetOccurrenceDate(c *gin.Context) {
	id, date, ok := occurrenceDateParams(c)
	if !ok {
		return
	}
	me := apiUser(c)
	out, err := a.scheduledDate(me, id, date)
	if err != nil {
		fail(c, err)
		return
	}
	if out != nil {
		c.JSON(http.StatusOK, eventDateResponseJSON{Occurrence: *out})
		return
	}
	if me != nil {
		tomb, err := a.cancelledDate(me, id, date)
		if err != nil {
			fail(c, err)
			return
		}
		if tomb != nil {
			c.JSON(http.StatusOK, eventDateCancelledResponseJSON{Occurrence: *tomb})
			return
		}
	}
	jsonError(c, http.StatusNotFound, "event not found")
}

// apiOccurrenceCalendar downloads one date as an .ics file (DEC-93). It goes
// through the date page's scheduled load, so it shows exactly what that page
// may show: the video link only when the page would, and no description,
// hosts or attendees. A cancelled date has no download, even for a viewer
// who may see its tombstone. The UID matches the feeds', so a calendar that
// also subscribes to a feed keeps one copy.
func (a *App) apiOccurrenceCalendar(c *gin.Context) {
	out, ok := a.loadOccurrenceDate(c)
	if !ok {
		return
	}
	status := ""
	if out.MyRegistration != nil {
		status = calendarStatus(*out.MyRegistration, false)
	}
	body := events.RenderFeed(events.Feed{
		Host:        a.feedHost(),
		WindowStart: out.StartsAt.Add(-24 * time.Hour),
		WindowEnd:   out.EndsAt.Add(24 * time.Hour),
		Singles:     []events.FeedSingle{dateFeedSingle(out, a.emailBaseURL(), time.Time{}, status)},
	})
	c.Header("Content-Disposition", `attachment; filename="event-`+out.Date+`.ics"`)
	c.Data(http.StatusOK, "text/calendar; charset=utf-8", body)
}

// occurrenceDateParams reads :id and :date, answering 404 itself for a shape
// that cannot name a date.
func occurrenceDateParams(c *gin.Context) (uint, string, bool) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	date := c.Param("date")
	if _, dateErr := models.ParseCalendarDate(date); err != nil || id == 0 || dateErr != nil {
		jsonError(c, http.StatusNotFound, "event not found")
		return 0, "", false
	}
	return uint(id), date, true
}

// loadOccurrenceDate is scheduledDate for the calendar download. It answers
// the error itself when ok is false.
func (a *App) loadOccurrenceDate(c *gin.Context) (dateOccurrenceJSON, bool) {
	id, date, ok := occurrenceDateParams(c)
	if !ok {
		return dateOccurrenceJSON{}, false
	}
	out, err := a.scheduledDate(apiUser(c), id, date)
	if err != nil {
		fail(c, err)
		return dateOccurrenceJSON{}, false
	}
	if out == nil {
		jsonError(c, http.StatusNotFound, "event not found")
		return dateOccurrenceJSON{}, false
	}
	return *out, true
}

// scheduledDate authorizes and presents one scheduled date for the date page
// and its calendar download: nil when the date is not scheduled or me may not
// see it.
func (a *App) scheduledDate(me *models.User, id uint, dateStr string) (*dateOccurrenceJSON, error) {
	now := time.Now().UTC()
	vs, err := a.dateCandidates(me, a.DB.Where("id = ?", id))
	if err != nil {
		return nil, err
	}
	q := a.DB.Where("event_id = ? AND original_date = ? AND status = ?", id, dateStr, models.OccurrenceScheduled)
	scoped, err := a.scopedOccurrenceReads(me, q, now)
	if err != nil {
		return nil, err
	}
	var occ models.EventOccurrence
	if err := q.Limit(1).Find(&occ).Error; err != nil || occ.ID == 0 {
		return nil, err
	}
	date, ok := a.projectDate(me, vs, &occ, scoped[occ.ID])
	if !ok {
		return nil, nil
	}
	// An ordinary reader's own row of any status, a cancelled one included
	// (M8.1 brief 2.1): dateJSON shows that one as history, not an answer.
	var reg models.Registration
	if date.Ordinary {
		if me != nil {
			if err := a.DB.Where("user_id = ? AND occurrence_id = ?", me.ID, occ.ID).Limit(1).Find(&reg).Error; err != nil {
				return nil, err
			}
		}
	} else {
		reg = date.Invited.Registration
	}
	out := date.dateJSON(&siteNameOnce{a: a}, reg, now)
	out.Room = a.occurrenceRooms(a.DB, []uint{occ.ID})[occ.ID]
	if date.Ordinary && date.Manager {
		if out.Reminders, err = a.dateReminders(date.Event, &occ); err != nil {
			return nil, err
		}
	}
	if date.Ordinary {
		out.CanRegister = me != nil && a.canRegisterDateIn(a.DB, me, date.Event, occ.ID)
	}
	return &out, nil
}

// cancelledDate is the cancelled-date tombstone for me (M8.1 brief 2.2), or
// nil when the date is not cancelled or me does not qualify. Every fact comes
// from one read-only Repeatable Read snapshot that takes no contact gate, so a
// disable or a title edit committed during the read never pairs with facts it
// did not commit with, and page reads never queue behind lifecycle writers. A
// failed read is an error, never "not qualified".
func (a *App) cancelledDate(me *models.User, eventID uint, date string) (*cancelledDateJSON, error) {
	var out *cancelledDateJSON
	now := a.now().UTC()
	err := a.DB.Transaction(func(tx *gorm.DB) error {
		var err error
		out, err = a.cancelledDateIn(tx, me, eventID, date, now)
		return err
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// cancelledDateStep runs the test hook at a named point of the tombstone read.
func (a *App) cancelledDateStep(step string) {
	if a.hooks.cancelledDateStep != nil {
		a.hooks.cancelledDateStep(step)
	}
}

// cancelledDateIn loads the facts policy.CanSeeCancelledOccurrence judges, all
// on tx: the exact stable date (the snapshot's first read), the viewer's own
// row on it, the viewer re-read with current consent, the event and its group,
// the viewer's ordinary access to the date, and, for a grant ended as
// occurrence_cancelled, the evidence tying that end to this cancellation. The
// session is rechecked against the snapshot's row (sessionCurrent): a session
// ended before the snapshot reads like a signed-out visitor, as the next
// request would.
func (a *App) cancelledDateIn(tx *gorm.DB, me *models.User, eventID uint, date string, now time.Time) (*cancelledDateJSON, error) {
	a.cancelledDateStep("begin")
	var occ models.EventOccurrence
	if err := tx.Where("event_id = ? AND original_date = ? AND status = ?", eventID, date, models.OccurrenceCancelled).Limit(1).Find(&occ).Error; err != nil || occ.ID == 0 {
		return nil, err
	}
	a.cancelledDateStep("date")
	var row models.Registration
	if err := tx.Where("user_id = ? AND occurrence_id = ?", me.ID, occ.ID).Limit(1).Find(&row).Error; err != nil || row.ID == 0 {
		return nil, err
	}
	facts, err := loadOccurrenceInvitationFacts(tx, []models.Registration{row}, now)
	if err != nil {
		return nil, err
	}
	viewer, ev := facts.users[me.ID], facts.events[occ.EventID]
	if !sessionCurrent(viewer, me) || ev.ID == 0 {
		return nil, nil
	}
	vs, err := loadCandidates(tx, viewer, tx.Where("id = ?", ev.ID), false)
	if err != nil {
		return nil, err
	}
	f := policy.CancelledDateFacts{
		Viewer:     facts.accounts[me.ID],
		Own:        row.UserID != nil && *row.UserID == me.ID && row.HouseholdPersonID == nil,
		Standing:   currentDepartureRegistration(row.Status),
		Ordinary:   vs.canSeeDate(viewer, vs.ByID[ev.ID], &occ),
		Access:     occurrenceAccessPolicy(row, accessScopeOf(&ev)),
		Occurrence: occurrenceFactsOf(&occ, &ev, facts.group(&ev)),
		Now:        now,
	}
	if row.AccessEndedAt != nil && row.AccessEndReason == policy.OccurrenceCancelled {
		if f.Evidence, err = a.cancelEvidenceIn(tx, row, ev.ID); err != nil {
			return nil, err
		}
	}
	if !policy.CanSeeCancelledOccurrence(f) {
		return nil, nil
	}
	out := cancelledDateOf(&ev, &occ)
	return &out, nil
}

// cancelEvidenceIn reads the two narrow records that tie a grant ended as
// occurrence_cancelled to the cancellation that ended it (M8.1 brief 2.2):
// this registration's end snapshots, judged by accessSnapshotValidSQL on its
// rows alone, and the event's restore records. Audit ids order the two, never
// their times: both are written under the exclusive contact gate from a
// CACHE 1 sequence, so the later of the two transactions has the larger id.
// Missing, duplicated or unreadable evidence is a denial for the policy to
// make; only a failed query is an error.
func (a *App) cancelEvidenceIn(tx *gorm.DB, row models.Registration, eventID uint) (policy.CancelEvidence, error) {
	var out policy.CancelEvidence
	if row.AccessGrantedAt == nil || row.AccessEndedAt == nil {
		return out, nil
	}
	var snapshots []struct {
		ID         uint
		Valid      bool
		Occurrence *int64
		Granted    *int64
		Ended      *int64
		Reason     *string
	}
	query := "SELECT l.id AS id, " + accessSnapshotValidSQL() + " AS valid, " + historyMetaID("occurrenceId") + " AS occurrence, " +
		historyMetaID("grantedAtMicro") + " AS granted, " + historyMetaID("endedAtMicro") + " AS ended, " +
		"CASE WHEN jsonb_typeof(l.meta_json->'endReason') = 'string' THEN l.meta_json->>'endReason' END AS reason " +
		"FROM audit_logs l WHERE l.action = ? AND l.target_type = ? AND l.target_id = ? ORDER BY l.id"
	a.cancelledDateStep("snapshots")
	if err := tx.Raw(query, audit.OccurrenceAccessEnded, accessTargetType, row.ID).Scan(&snapshots).Error; err != nil {
		return out, err
	}
	var snapshotID uint
	for _, s := range snapshots {
		if !s.Valid {
			out.Unreadable = true
			continue
		}
		if s.Occurrence != nil && *s.Occurrence == int64(row.OccurrenceID) && s.Granted != nil && *s.Granted == accessMicro(*row.AccessGrantedAt) &&
			s.Ended != nil && *s.Ended == accessMicro(*row.AccessEndedAt) && s.Reason != nil && *s.Reason == row.AccessEndReason {
			out.Snapshots++
			snapshotID = s.ID
		}
	}
	var restores []struct {
		ID  uint
		IDs *string
	}
	a.cancelledDateStep("restores")
	if err := tx.Raw("SELECT l.id AS id, (l.meta_json->'occurrenceIds')::text AS ids FROM audit_logs l WHERE l.action = ? AND l.target_type = 'event' AND l.target_id = ? ORDER BY l.id",
		audit.EventDatesRestored, eventID).Scan(&restores).Error; err != nil {
		return out, err
	}
	for _, r := range restores {
		ids, ok := restoredOccurrenceIDs(r.IDs)
		if !ok {
			out.Unreadable = true
			continue
		}
		if r.ID > snapshotID && slices.Contains(ids, row.OccurrenceID) {
			out.RestoredAfter = true
		}
	}
	return out, nil
}

// restoredOccurrenceIDs reads a restore record's occurrenceIds as its writer
// (restoreSeriesDates) leaves it: a nonempty array of positive ids. Anything
// else (absent, not an array, empty, or an element that is null, zero,
// negative, fractional or not a number) is unreadable, and the caller denies:
// a list Go would read with a zero in place of a null could otherwise hide the
// restore of this very date.
func restoredOccurrenceIDs(raw *string) ([]uint, bool) {
	if raw == nil {
		return nil, false
	}
	var ids []*int64
	if json.Unmarshal([]byte(*raw), &ids) != nil || len(ids) == 0 {
		return nil, false
	}
	out := make([]uint, 0, len(ids))
	for _, id := range ids {
		if id == nil || *id <= 0 {
			return nil, false
		}
		out = append(out, uint(*id))
	}
	return out, true
}

// occurrenceAccountsIn uses the same latest-consent ordering as the single-user
// loader. Facts are local to the caller's transaction, never cached authority.
func occurrenceAccountsIn(tx *gorm.DB, ids []uint, now time.Time) (map[uint]*models.User, map[uint]policy.ContactAccount, error) {
	if len(ids) == 0 {
		return map[uint]*models.User{}, map[uint]policy.ContactAccount{}, nil
	}
	zone, err := churchZoneIn(tx)
	if err != nil {
		return nil, nil, err
	}
	return occurrenceAccountsInZone(tx, ids, now, zone)
}

func occurrenceAccountsInZone(tx *gorm.DB, ids []uint, now time.Time, zone *time.Location) (map[uint]*models.User, map[uint]policy.ContactAccount, error) {
	users := map[uint]*models.User{}
	accounts := map[uint]policy.ContactAccount{}
	if len(ids) == 0 {
		return users, accounts, nil
	}
	var rows []models.User
	if err := tx.Where("id IN ?", ids).Find(&rows).Error; err != nil {
		return nil, nil, err
	}
	teens := []uint{}
	for i := range rows {
		u := &rows[i]
		users[u.ID] = u
		accounts[u.ID] = contactAccount(u, now, zone)
		if u.AccountType == models.AccountTeen {
			teens = append(teens, u.ID)
		}
	}
	if len(teens) > 0 {
		var consents []models.TeenConsent
		if err := tx.Select("DISTINCT ON (teen_user_id) *").Where("teen_user_id IN ? AND scope = ?", teens, models.TeenConsentAccount).Order("teen_user_id, at DESC, id DESC").Find(&consents).Error; err != nil {
			return nil, nil, err
		}
		granted := map[uint]bool{}
		for _, c := range consents {
			granted[c.TeenUserID] = c.Status == models.ConsentGranted
		}
		for _, id := range teens {
			a := accounts[id]
			a.Active = a.Active && granted[id]
			accounts[id] = a
		}
	}
	return users, accounts, nil
}
