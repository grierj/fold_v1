package app

import (
	"database/sql"
	"encoding/json"
	"errors"
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
	Event      models.Event
	Occurrence models.EventOccurrence
	Access     policy.OccurrenceAccess
	Facts      policy.OccurrenceFacts
	// Actor is the recipient as exact-date authority reads them: no age, so
	// no right that needs an adult can come from a read.
	Actor        policy.OccurrenceActor
	Registration models.Registration
}

// scopedOccurrenceReadsIn loads exact candidates independently of
// series-filtered eventCandidates, every read on db: db and q must come from
// the same transaction or handle, and no read here touches another connection.
// q selects occurrence IDs (window/date and event filters). Parent IDs NEVER
// enter the ordinary visible set. All related facts are batched, including live
// registration provenance; no attendee/contact directory is loaded here. The
// grants and the dates that judge them are separate statements, so a caller
// that shows what a grant authorizes reads both in one snapshot: the edit that
// stamps an expired grant ended can move its date in the same commit.
func (a *App) scopedOccurrenceReadsIn(db *gorm.DB, viewer *models.User, q *gorm.DB, now time.Time) (map[uint]*occurrenceRead, error) {
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
	if a.hooks.invitationReadStep != nil {
		a.hooks.invitationReadStep("grants")
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
// each page with scopedOccurrenceReadsIn, the same loader and policy the date
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
func (a *App) hasScopedUntitledRead(db *gorm.DB, viewer *models.User, eventID uint, now time.Time) (bool, error) {
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
		scoped, err := a.scopedOccurrenceReadsIn(db, viewer, db.Where("id IN ?", ids), now)
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
	return facts.reads(grants, now), nil
}

// reads judges grants by facts already loaded (loadOccurrenceInvitationFacts,
// or one statement's rows for a claimed email, dateFacts): the one evaluation
// of exact-date authority, keyed by registration id.
func (f *occurrenceInvitationFacts) reads(grants []models.Registration, now time.Time) map[uint]*occurrenceRead {
	out := map[uint]*occurrenceRead{}
	for _, row := range grants {
		if row.UserID == nil || !currentDepartureRegistration(row.Status) {
			continue
		}
		occ := f.occurrences[row.OccurrenceID]
		ev := f.events[occ.EventID]
		actor := f.actors[*row.UserID]
		if occ.ID == 0 || ev.ID == 0 || occ.EventID != ev.ID {
			continue
		}
		scope := accessScopeOf(&ev)
		lifecycle := occurrenceFactsOf(&occ, &ev, f.group(&ev))
		access := occurrenceAccessPolicy(row, scope)
		if policy.CanSeeOccurrence(actor, false, access, lifecycle, now) {
			out[row.ID] = &occurrenceRead{Event: ev, Occurrence: occ, Registration: row, Actor: actor, Access: access, Facts: lifecycle}
		}
	}
	return out
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
	actors      map[uint]policy.OccurrenceActor
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
	actors := make(map[uint]policy.OccurrenceActor, len(accounts))
	for id, a := range accounts {
		actors[id] = a.OccurrenceActor()
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
	return &occurrenceInvitationFacts{users, actors, occByID, evByID, groupByID}, nil
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
	if u.AccountType == models.AccountTeen && !f.actors[u.ID].Active {
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
	if policy.AccessEndsWithDate(row.AccessOrigin) && !occ.EndsAt.IsZero() && !now.Before(occ.EndsAt) {
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

// dateOccurrenceJSON never carries host people, contact or management data,
// except canManage, a capability flag (the date page's manager tools read
// their data from the managed read). Only ordinary reads may attach a group label/link, after an independent
// groupShownTo check, and only the ordinary JSON read carries the date page's
// own facts (datePage); invitation-only reads omit all of that.
type dateOccurrenceJSON struct {
	ID               uint      `json:"id"`
	EventID          uint      `json:"eventId"`
	Date             string    `json:"date"`
	DateURL          string    `json:"dateUrl"`
	Title            string    `json:"title"`
	Description      string    `json:"description,omitempty"`
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
	// CanManage is true only for an ordinary reader who manages the event.
	CanManage bool   `json:"canManage,omitempty"`
	HostedBy  string `json:"hostedBy,omitempty"`
	GroupID   *uint  `json:"groupId,omitempty"`
	GroupName string `json:"groupName,omitempty"`
	// Room is the date's confirmed room, shown as its place (9.2, M2.1b).
	Room *occurrenceRoomJSON `json:"room,omitempty"`
	// Reminders is this date's email reminders (M3.6b), only for its
	// event's managers; absent for everyone else.
	Reminders *dateRemindersJSON `json:"reminders,omitempty"`
	// The date page's own facts, as policy.DatePage allows (M8.1 brief 4, 6).
	// RemoteRSVP is offered, closed (remote is on but this date's remote cap
	// is 0) or none; a full remote pool is still offered.
	GroupHeader        *dateGroupHeaderJSON `json:"groupHeader,omitempty"`
	Attendance         *dateAttendanceJSON  `json:"attendance,omitempty"`
	Seats              *dateSeatsJSON       `json:"seats,omitempty"`
	RemoteRSVP         string               `json:"remoteRsvp,omitempty"`
	MyWaitlistPosition *int                 `json:"myWaitlistPosition,omitempty"`
	Series             *dateSeriesJSON      `json:"series,omitempty"`
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
	read, judged, err := a.scheduledDatePage(me, id, date)
	if err != nil {
		fail(c, err)
		return
	}
	if read != nil {
		if !a.answerDatePage(c, judged) {
			return
		}
		c.JSON(http.StatusOK, eventDateResponseJSON{Occurrence: read.out})
		return
	}
	if me != nil {
		var tomb *cancelledDateJSON
		tomb, judged, err = a.cancelledDate(me, id, date)
		if err != nil {
			fail(c, err)
			return
		}
		if tomb != nil {
			if !a.answerDatePage(c, judged) {
				return
			}
			c.JSON(http.StatusOK, eventDateCancelledResponseJSON{Occurrence: *tomb})
			return
		}
	}
	if !a.answerDatePage(c, judged) {
		return
	}
	jsonError(c, http.StatusNotFound, "event not found")
}

// answerDatePage acknowledges a participating date page read for the viewer
// judged by the snapshot that decided the answer: the scheduled date, the
// tombstone, or, for the 404, the last read that ran. false means it answered
// the 500 itself.
func (a *App) answerDatePage(c *gin.Context, judged sessionJudgment) bool {
	if err := a.acknowledgeDecision(c, judged, true); err != nil {
		fail(c, err)
		return false
	}
	return true
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

// loadOccurrenceDate is scheduledDateIn for the calendar download and its
// room as the event pages show it (a failed room read leaves the room out),
// in one read-only Repeatable Read snapshot like the date page's, so the grant
// that admits the date and the date it shows come from one committed state.
// It answers the error itself when ok is false.
func (a *App) loadOccurrenceDate(c *gin.Context) (dateOccurrenceJSON, bool) {
	id, date, ok := occurrenceDateParams(c)
	if !ok {
		return dateOccurrenceJSON{}, false
	}
	var read *scheduledRead
	now := a.now().UTC()
	err := a.DB.Transaction(func(tx *gorm.DB) error {
		viewer, err := snapshotViewerIn(tx, apiUser(c))
		if err != nil {
			return err
		}
		if read, err = a.scheduledDateIn(tx, viewer, id, date, now); err != nil || read == nil {
			return err
		}
		read.out.Room, _ = dateRoomIn(tx, read.date.Occ.ID)
		return nil
	}, readSnapshot)
	if err != nil {
		fail(c, err)
		return dateOccurrenceJSON{}, false
	}
	if read == nil {
		jsonError(c, http.StatusNotFound, "event not found")
		return dateOccurrenceJSON{}, false
	}
	return read.out, true
}

// readSnapshot is the read-only Repeatable Read transaction a page reads in
// when what it shows must come from one committed state: every statement sees
// the same snapshot, and it takes no lock and never waits on a writer.
var readSnapshot = &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true}

// scheduledRead is one scheduled date as scheduledDateIn authorized and
// presented it, with what datePageIn builds on and what it decided and loaded
// for the date page (show, page).
type scheduledRead struct {
	date *dateProjection
	vs   visibleSet
	reg  models.Registration
	out  dateOccurrenceJSON
	now  time.Time
	show policy.DatePageFacts
	page datePageLoad
}

// scheduledDatePage is the date page's scheduled JSON read: scheduledDateIn,
// the date's room and datePageIn, all in one read-only Repeatable Read
// snapshot, so a visibility, membership or roster change committed during the
// read never pairs the authority that admits a fact with newer facts. The
// viewer is re-read in the snapshot (sessionCurrent): a session ended before
// it reads like a signed-out visitor, as the next request would. The room is
// read in a savepoint, so a failed room read leaves the snapshot usable and
// the date shows without its room, as on the event pages, unless the series
// comparison needs it (datePageIn); the reads that show a default when they
// fail do the same (lenientReadIn). The page is built from what the snapshot
// decided and loaded after it ends: that projection reads nothing and never
// walks the recurrence rule; the series clock is the writer's stored evidence
// (usualClockIn).
func (a *App) scheduledDatePage(me *models.User, id uint, date string) (*scheduledRead, sessionJudgment, error) {
	var read *scheduledRead
	var judged sessionJudgment
	now := a.now().UTC()
	err := a.DB.Transaction(func(tx *gorm.DB) error {
		a.scheduledDateStep("begin")
		viewer, err := snapshotViewerIn(tx, me)
		if err != nil {
			return err
		}
		judged.judge(viewer)
		if read, err = a.scheduledDateIn(tx, viewer, id, date, now); err != nil || read == nil {
			return err
		}
		a.scheduledDateStep("authorized")
		var roomErr error
		read.out.Room, roomErr = dateRoomIn(tx, read.date.Occ.ID)
		return a.datePageIn(tx, viewer, read, roomErr)
	}, readSnapshot)
	if err != nil {
		return nil, sessionJudgment{}, err
	}
	if read == nil {
		return nil, judged, nil
	}
	read.date.datePage(&read.out, read.show, read.page, read.reg)
	return read, judged, nil
}

// scheduledDateStep runs the test hook at a named point of the scheduled
// date page's read.
func (a *App) scheduledDateStep(step string) {
	if a.hooks.scheduledDateStep != nil {
		a.hooks.scheduledDateStep(step)
	}
}

// snapshotViewerIn re-reads the signed-in viewer on tx: nil for a signed-out
// visitor and for a session that is not current there (sessionCurrent).
func snapshotViewerIn(tx *gorm.DB, me *models.User) (*models.User, error) {
	if me == nil {
		return nil, nil
	}
	var fresh models.User
	if err := tx.Where("id = ?", me.ID).Limit(1).Find(&fresh).Error; err != nil {
		return nil, err
	}
	if !sessionCurrent(&fresh, me) {
		return nil, nil
	}
	return &fresh, nil
}

// dateRoomIn is one date's confirmed room (roomsFor), read in a savepoint of
// tx so a failed read leaves the transaction usable. A failed read returns no
// room and its error, for the caller to judge.
func dateRoomIn(tx *gorm.DB, occurrenceID uint) (*occurrenceRoomJSON, error) {
	var room *occurrenceRoomJSON
	err := tx.Transaction(func(sp *gorm.DB) error {
		rooms, err := roomsFor(sp, []uint{occurrenceID})
		room = rooms[occurrenceID]
		return err
	})
	if err != nil {
		return nil, err
	}
	return room, nil
}

// errLenientReadDone rolls back lenientReadIn's savepoint.
var errLenientReadDone = errors.New("lenient read done")

// lenientReadIn runs read, whose caller shows a default when it fails (the
// church name, the push switch, the church zone, whether the viewer may
// answer). Inside a transaction a failed statement would abort it and fail
// every later read, so there read runs in a savepoint that is always rolled
// back: only reads belong here. On a plain handle read runs as is. The
// caller sets its default first: read does not run when the savepoint cannot
// be made.
func lenientReadIn(db *gorm.DB, read func(*gorm.DB)) {
	if c, ok := db.Statement.ConnPool.(gorm.TxCommitter); !ok || c == nil {
		read(db)
		return
	}
	_ = db.Transaction(func(sp *gorm.DB) error {
		read(sp)
		return errLenientReadDone
	})
}

// scheduledDateIn authorizes and presents one scheduled date for the date page
// and its calendar download, every read on db: nil when the date is not
// scheduled or me may not see it. Each caller adds the room its own way.
func (a *App) scheduledDateIn(db *gorm.DB, me *models.User, id uint, dateStr string, now time.Time) (*scheduledRead, error) {
	vs, err := dateCandidatesIn(db, me, db.Where("id = ?", id))
	if err != nil {
		return nil, err
	}
	q := db.Where("event_id = ? AND original_date = ? AND status = ?", id, dateStr, models.OccurrenceScheduled)
	scoped, err := a.scopedOccurrenceReadsIn(db, me, q, now)
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
			if err := db.Where("user_id = ? AND occurrence_id = ?", me.ID, occ.ID).Limit(1).Find(&reg).Error; err != nil {
				return nil, err
			}
		}
	} else {
		reg = date.Invited.Registration
	}
	out := date.dateJSON(&siteNameOnce{a: a, db: db}, reg, now)
	if date.Ordinary && date.Manager {
		if out.Reminders, err = a.dateReminders(db, date.Event, &occ); err != nil {
			return nil, err
		}
	}
	if date.Ordinary && me != nil {
		lenientReadIn(db, func(sp *gorm.DB) { out.CanRegister = a.canRegisterDateIn(sp, me, date.Event, occ.ID) })
	}
	return &scheduledRead{date: date, vs: vs, reg: reg, out: out, now: now}, nil
}

// datePageIn decides and loads the date page's own facts for an ordinary JSON
// read on tx (r.show, r.page; scheduledDatePage builds the page from them),
// loading only what policy.DatePage lets this reader see: nothing for an
// invitation-only reader. Every count is read in the read's snapshot, and a
// failed read is an error, never an empty or available pool. roomErr is the
// date's failed room read, if any: only the series comparison, which must not
// claim a room change from a room it could not read, fails on it. The .ics
// download and the feeds never come here.
func (a *App) datePageIn(tx *gorm.DB, me *models.User, r *scheduledRead, roomErr error) error {
	p, occ := r.date, r.date.Occ
	show := p.pageFacts(me)
	r.show = show
	if !show.Header && !show.Attendance && !show.Capacity && !show.Series {
		return nil
	}
	var d datePageLoad
	var err error
	if show.Header && p.Group != nil {
		names, err := leaderNames(tx, []uint{p.Group.LeaderID})
		if err != nil {
			return err
		}
		d.LeaderName = names[p.Group.LeaderID]
	}
	if show.Attendance {
		counts, err := registrationCountsByDate(tx, []uint{occ.ID})
		if err != nil {
			return err
		}
		d.Counts = counts[occ.ID]
	}
	if show.Names {
		if d.Attendees, err = dateAttendeesIn(tx, me, p, r.now); err != nil {
			return err
		}
	}
	if show.Capacity {
		if d.Seats, err = seatsUsedForMode(tx, occ.ID, models.AttendanceInPerson, 0); err != nil {
			return err
		}
		if p.Event.RemoteAvailable {
			if d.RemoteSeats, err = seatsUsedForMode(tx, occ.ID, models.AttendanceRemote, 0); err != nil {
				return err
			}
		}
		if p.Event.ChildcareAvailable && (p.Event.ChildcareCapacity != nil || show.ChildcareTotals) {
			if d.Childcare, err = childcareUsedFor(tx, occ.ID, 0); err != nil {
				return err
			}
		}
		if r.reg.ID > 0 && r.reg.Status == models.RegWaitlisted {
			if d.WaitlistPosition, err = waitlistPosition(tx, occ.ID, r.reg.ID); err != nil {
				return err
			}
		}
	}
	if show.Series && p.Event.RRule != "" {
		if roomErr != nil {
			return roomErr
		}
		if d.UsualRoom, err = usualRoomIn(tx, p.Event); err != nil {
			return err
		}
		// A dirty event (DEC-155) may hold evidence a previous binary made
		// stale; read in this same snapshot, its clock is unknown.
		if !p.Event.SeriesClockDirty {
			if d.UsualClock, err = usualClockIn(tx, occ.ID, occ.StartsAt); err != nil {
				return err
			}
		}
	}
	if show.Coverage {
		if d.Upcoming, d.Covered, d.SeriesAuthority, err = seriesCoverageIn(tx, me, r.vs, p.Event, r.now); err != nil {
			return err
		}
	}
	r.page = d
	return nil
}

// usualClockIn picks a date's usual clock from the series writer's evidence
// (models.OccurrenceSeriesStart, DEC-155) on tx, in three probes of its
// primary key or first-row index whatever the series' age, density or number
// of times a day: the time the date starts at; else the earliest in the UTC
// minute it starts in (the date editor saves whole minutes, and the two passes
// through a repeated hour are different UTC minutes); else the first the
// writer generated. nil when the date has no evidence: its clock is unknown.
// A failed read is an error. Only the scheduled date page's JSON read calls
// it; no list, feed or calendar reads the evidence.
func usualClockIn(tx *gorm.DB, occurrenceID uint, startsAt time.Time) (*seriesClock, error) {
	at := startsAt.UTC()
	minute := at.Truncate(time.Minute)
	var rows []seriesClock
	if err := tx.Raw(`SELECT starts_at, tzid, written_local FROM (
		(SELECT starts_at, tzid, written_local, 1 AS pick FROM occurrence_series_starts
			WHERE occurrence_id = ? AND starts_at = ?)
		UNION ALL
		(SELECT starts_at, tzid, written_local, 2 AS pick FROM occurrence_series_starts
			WHERE occurrence_id = ? AND starts_at >= ? AND starts_at < ? ORDER BY starts_at LIMIT 1)
		UNION ALL
		(SELECT starts_at, tzid, written_local, 3 AS pick FROM occurrence_series_starts
			WHERE occurrence_id = ? AND is_first)
	) picked ORDER BY pick LIMIT 1`,
		occurrenceID, at, occurrenceID, minute, minute.Add(time.Minute), occurrenceID).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

// dateAttendeesIn are the date's Going and Maybe names as subjectFacts.attendee
// lets this viewer see them (pending teens, household people), reduced to what
// the page's name summary shows: no contact, care or management fields. The
// household clock is the church day at now, read from tx only when a
// household person needs it.
func dateAttendeesIn(tx *gorm.DB, me *models.User, p *dateProjection, now time.Time) ([]dateAttendeeJSON, error) {
	var rows []models.Registration
	if err := tx.Model(&models.Registration{}).Select("registrations.*").Joins(listedRegistrations()).
		Where("registrations.occurrence_id = ? AND registrations.status IN ?", p.Occ.ID, []string{models.RegGoing, models.RegMaybe}).
		Order("registrations.waitlisted_at ASC NULLS FIRST, registrations.id").Find(&rows).Error; err != nil {
		return nil, err
	}
	facts := newSubjectFacts(time.Time{})
	var zoneErr error
	facts.clock = func() time.Time {
		clock, err := churchClockIn(tx, now)
		if err != nil {
			zoneErr = err
			return now
		}
		return clock
	}
	if err := facts.load(tx, rows, me); err != nil {
		return nil, err
	}
	out := []dateAttendeeJSON{}
	for _, r := range rows {
		if at := facts.attendee(r, *p.Occ, me, p.Manager, false, now); at != nil {
			out = append(out, dateAttendeeJSON{Name: at.Name, Status: at.Status, GuestCount: at.GuestCount})
		}
	}
	if zoneErr != nil {
		return nil, zoneErr
	}
	return out, nil
}

// seriesCoverageIn counts the series' upcoming dates me could answer for, with
// apiRegisterSeries' own boundaries (scheduled, starting after now, and
// registrable at the date's own visibility, which also needs canSeeDate here),
// and me's own Going or Maybe answers among exactly those dates, so a date me
// cannot see moves neither count. authority is that writer's series check.
// Nothing here marks how an answer was made (M8.1 brief 2.1). The caller asks
// only for a repeating event (policy.DatePage).
func seriesCoverageIn(tx *gorm.DB, me *models.User, vs visibleSet, ev *models.Event, now time.Time) (upcoming, covered int, authority bool, err error) {
	f := vs.of(ev)
	gp := asGroup(f.group)
	authority = canRegisterWith(me, viewOf(ev, false), gp, f.rel)
	var occs []models.EventOccurrence
	if err := tx.Where("event_id = ? AND status = ? AND starts_at > ?", ev.ID, models.OccurrenceScheduled, now).
		Order("original_date, id").Find(&occs).Error; err != nil {
		return 0, 0, false, err
	}
	ids := []uint{}
	for i := range occs {
		o := &occs[i]
		if vs.canSeeDate(me, ev, o) && canRegisterWith(me, viewOf(dateEvent(ev, o), false), gp, f.rel) {
			ids = append(ids, o.ID)
		}
	}
	err = eachIDChunk(ids, func(chunk []uint) error {
		var n int64
		if err := tx.Model(&models.Registration{}).
			Where("user_id = ? AND household_person_id IS NULL AND occurrence_id IN ? AND status IN ?", me.ID, chunk, []string{models.RegGoing, models.RegMaybe}).
			Count(&n).Error; err != nil {
			return err
		}
		covered += int(n)
		return nil
	})
	if err != nil {
		return 0, 0, false, err
	}
	return len(ids), covered, authority, nil
}

// cancelledDate is the cancelled-date tombstone for me (M8.1 brief 2.2), or
// nil when the date is not cancelled or me does not qualify. Every fact comes
// from one read-only Repeatable Read snapshot that takes no contact gate, so a
// disable or a title edit committed during the read never pairs with facts it
// did not commit with, and page reads never queue behind lifecycle writers. A
// failed read is an error, never "not qualified". The viewer is judged last,
// on the same snapshot, so its first statement stays the date's.
func (a *App) cancelledDate(me *models.User, eventID uint, date string) (*cancelledDateJSON, sessionJudgment, error) {
	var out *cancelledDateJSON
	var judged sessionJudgment
	now := a.now().UTC()
	err := a.DB.Transaction(func(tx *gorm.DB) error {
		var err error
		if out, err = a.cancelledDateIn(tx, me, eventID, date, now); err != nil {
			return err
		}
		viewer, err := snapshotViewerIn(tx, me)
		if err != nil {
			return err
		}
		judged.judge(viewer)
		return nil
	}, readSnapshot)
	if err != nil {
		return nil, sessionJudgment{}, err
	}
	return out, judged, nil
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
	ok, err := a.cancelledDateEntitledIn(tx, me.ID, row, facts, &ev, &occ, vs.canSeeDate(viewer, vs.ByID[ev.ID], &occ), now)
	if err != nil || !ok {
		return nil, err
	}
	out := cancelledDateOf(&ev, &occ)
	return &out, nil
}

// cancelledDateEntitledIn is policy.CanSeeCancelledOccurrence for viewerID on
// the cancelled date occ, judged by row (the viewer's own registration on it),
// facts (loadOccurrenceInvitationFacts over row) and ordinary, the viewer's
// ordinary access to the date, which the caller decides. The evidence of a
// grant ended as occurrence_cancelled is read here, on tx.
func (a *App) cancelledDateEntitledIn(tx *gorm.DB, viewerID uint, row models.Registration, facts *occurrenceInvitationFacts, ev *models.Event, occ *models.EventOccurrence, ordinary bool, now time.Time) (bool, error) {
	f := policy.CancelledDateFacts{
		Viewer:     facts.actors[viewerID],
		Own:        row.UserID != nil && *row.UserID == viewerID && row.HouseholdPersonID == nil && row.OccurrenceID == occ.ID,
		Standing:   currentDepartureRegistration(row.Status),
		Ordinary:   ordinary,
		Access:     occurrenceAccessPolicy(row, accessScopeOf(ev)),
		Occurrence: occurrenceFactsOf(occ, ev, facts.group(ev)),
		Now:        now,
	}
	if row.AccessEndedAt != nil && row.AccessEndReason == policy.OccurrenceCancelled {
		var err error
		if f.Evidence, err = a.cancelEvidenceIn(tx, row, ev.ID); err != nil {
			return false, err
		}
	}
	return policy.CanSeeCancelledOccurrence(f), nil
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
		if rows[i].AccountType == models.AccountTeen {
			teens = append(teens, rows[i].ID)
		}
	}
	granted := map[uint]bool{}
	if len(teens) > 0 {
		var consents []models.TeenConsent
		if err := tx.Select("DISTINCT ON (teen_user_id) *").Where("teen_user_id IN ? AND scope = ?", teens, models.TeenConsentAccount).Order("teen_user_id, at DESC, id DESC").Find(&consents).Error; err != nil {
			return nil, nil, err
		}
		for _, c := range consents {
			granted[c.TeenUserID] = c.Status == models.ConsentGranted
		}
	}
	for i := range rows {
		u := &rows[i]
		users[u.ID] = u
		a := contactAccount(u, now, zone)
		a.Active = occurrenceActorOf(u, granted[u.ID]).Active
		accounts[u.ID] = a
	}
	return users, accounts, nil
}

// occurrenceActorOf is the one rule for whether u is an active exact-date
// recipient: a live account that can sign in and, for a teen, whose latest
// account consent is granted (consented). It reads no age or contact
// preference, so it can be built from rows that carry neither.
func occurrenceActorOf(u *models.User, consented bool) policy.OccurrenceActor {
	return policy.OccurrenceActor{Person: asPerson(u),
		Active: !u.DeletedAt.Valid && u.CanSignIn() && (u.AccountType != models.AccountTeen || consented)}
}
