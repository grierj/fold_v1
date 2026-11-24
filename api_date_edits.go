package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"fold/internal/consent"
	"fold/internal/events"
	"fold/internal/policy"
	"fold/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// The guarded date edit (M8.1, DEC-164). A manager's editor holds the date's
// revision token, read from a manager surface; POST
// /events/:id/occurrences/:date/edits names it, and is refused with
// date_changed when the date, its series or the inherited group place changed
// since. A resend of an applied edit names a base the edit itself moved on, so
// it is refused the same way and the client reads the date again.
//
// Restore safety: the token is bound to a per-process generation
// (processKey). Every restart, and so every scripted restore, rollback and
// re-upgrade, starts a new generation, so a token from before it is refused,
// never matched against restored rows. A database restored under a
// still-running process keeps the generation; that is the same limit the
// restore barrier states (docs/agents/erasure.md).

var (
	errDateChanged     = apiErr(http.StatusConflict, "date_changed", "the date changed since this edit was based on it")
	errDateEditInvalid = apiErr(http.StatusBadRequest, "date_edit_invalid", "that date edit isn't one we can read")
	// errDatePageOutdated answers the retired date PUT (apiRetiredDatePut):
	// the page that sent it is from before the guarded edit.
	errDatePageOutdated = apiErr(http.StatusConflict, "date_page_outdated", "this page is out of date; reload it to edit the date")
	// errDateNeedsConsent carries the impact (dateNeedsConsent).
	errDateNeedsConsent = apiErr(http.StatusConflict, "date_needs_consent", "saving this date would cancel registrations")
	// errDateEndedMidEdit: a stored end of the edited date, before or after
	// the edit, fell between the clock the edit judged on and the clock read
	// after its last locking work (closeSingleDate). Internal only:
	// singleDateMutation reruns the edit once and answers errDateChanged on a
	// second crossing.
	errDateEndedMidEdit = errors.New("the date ended during the edit")
	// errDateKeptAfterLeaving carries the refused dates (keptDatesConflict).
	errDateKeptAfterLeaving = apiErr(http.StatusConflict, "date_kept_after_leaving",
		"someone who left the group kept this date; remove them from it before making it end later")
)

var dateTokenRe = regexp.MustCompile(`^1\.([0-9a-f]{16})\.[0-9]{1,19}\.[0-9]{1,19}\.[0-9]{1,19}\.(?:-|[0-9a-f]{16})$`)

// dateEditSettled is the callback's answer for a no-op: the whole transaction
// rolls back, both reconcile passes included, and the handler answers 200
// only when that rollback is confirmed.
type dateEditSettled struct{ resp occurrenceJSON }

func (*dateEditSettled) Error() string { return "date edit settled without a write" }

// dateNeedsConsent: tightening the date cancels people the request did not
// agree to (its consent is not the token of the impact as the write finds
// it). The impact carries the fresh token.
type dateNeedsConsent struct{ body dateNeedsConsentJSON }

func (dateNeedsConsent) Error() string { return errDateNeedsConsent.Message }

func (dateNeedsConsent) Is(target error) bool { return target == errDateNeedsConsent }

func (e dateNeedsConsent) apiAnswer() *apiError {
	return errDateNeedsConsent.with(gin.H{"impact": e.body.Impact})
}

// keptDatesConflict: the change makes a date end later while someone who left
// still holds it (DEC-156). Counts only, so the answer names nobody.
type keptDatesConflict struct{ body keptDatesConflictJSON }

func (keptDatesConflict) Error() string { return errDateKeptAfterLeaving.Message }

func (keptDatesConflict) Is(target error) bool { return target == errDateKeptAfterLeaving }

func (e keptDatesConflict) apiAnswer() *apiError {
	return errDateKeptAfterLeaving.with(gin.H{"keptDates": e.body.KeptDates})
}

// refuseKeptDates answers keptDatesConflict when any of raised (each date's id
// and original date) still has a live kept-date grant, counted by keptDatesIn
// after the writer's before pass; otherwise nil.
func refuseKeptDates(tx *gorm.DB, raised map[uint]string) error {
	if len(raised) == 0 {
		return nil
	}
	ids := make([]uint, 0, len(raised))
	for id := range raised {
		ids = append(ids, id)
	}
	counts, err := keptDatesIn(tx, ids)
	if err != nil {
		return err
	}
	kept := []keptDateCountJSON{}
	for id, n := range counts {
		if n > 0 {
			kept = append(kept, keptDateCountJSON{Date: raised[id], KeptCount: n})
		}
	}
	if len(kept) == 0 {
		return nil
	}
	slices.SortFunc(kept, func(a, b keptDateCountJSON) int { return strings.Compare(a.Date, b.Date) })
	return keptDatesConflict{keptDatesConflictJSON{KeptDates: kept}}
}

// dateToken is the revision an editor based on occ holds:
// 1.<generation>.<occurrence id>.<event revision>.<date revision>.<place>.
// place tags the group's place when it is what the date shows (the date and
// the event have none of their own and shown, the group as this viewer may see
// it, has one), so a change to that inherited default conflicts too; it is "-"
// otherwise. Every writer of groups.location holds the exclusive contact gate,
// which the guarded edit also holds, so the group is not locked here (series
// writes lock the group before the event).
func (a *App) dateToken(ev *models.Event, occ *models.EventOccurrence, shown *models.Group) string {
	place := "-"
	if occ.LocationOverride == nil && ev.Location == "" && shown != nil && shown.Location != "" {
		m := hmac.New(sha256.New, a.processKey("date-edit-place:v1"))
		m.Write([]byte(shown.Location))
		place = hex.EncodeToString(m.Sum(nil)[:8])
	}
	return a.editRevision(occ.ID, ev.EditRevision, occ.EditRevision, place)
}

// dateEditChanges is the request's changes object; each field is raw so its
// presence is explicit.
type dateEditChanges struct {
	StartsLocal            json.RawMessage `json:"startsLocal"`
	DurationMinutes        json.RawMessage `json:"durationMinutes"`
	Title                  json.RawMessage `json:"title"`
	Location               json.RawMessage `json:"location"`
	CapacityOverride       json.RawMessage `json:"capacityOverride"`
	RemoteCapacityOverride json.RawMessage `json:"remoteCapacityOverride"`
	Visibility             json.RawMessage `json:"visibility"`
}

func (ch dateEditChanges) empty() bool {
	return len(ch.StartsLocal)+len(ch.DurationMinutes)+len(ch.Title)+len(ch.Location)+
		len(ch.CapacityOverride)+len(ch.RemoteCapacityOverride)+len(ch.Visibility) == 0
}

// dateEdit is a parsed, normalized request.
type dateEdit struct {
	base string
	// consent is the reviewed impact's token, "" when absent.
	consent       string
	reason        string
	startsLocal   *string
	duration      *int
	title         *string
	location      *string
	capacity      *int
	capacitySet   bool
	remote        *int
	remoteSet     bool
	visibility    *string
	visibilitySet bool
}

// decodeStrict decodes raw into v, refusing unknown keys and trailing data.
func decodeStrict(raw []byte, v any) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(v) != nil {
		return false
	}
	_, err := d.Token()
	return err == io.EOF
}

// rawString reads a present field as a trimmed string; null and other types
// are not one.
func rawString(raw json.RawMessage) (*string, bool) {
	if len(raw) == 0 {
		return nil, true
	}
	var s string
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &s) != nil {
		return nil, false
	}
	s = strings.TrimSpace(s)
	return &s, true
}

// parseDateEdit reads the body. A malformed request is errDateEditInvalid;
// a value that fails a field's own rule gets that field's answer.
func parseDateEdit(raw []byte) (*dateEdit, error) {
	var body struct {
		// Attempt is optional, unvalidated and ignored: a page from the
		// earlier release and the restore drill's candidate writer still send
		// one, and decodeStrict refuses an unknown key.
		Attempt  json.RawMessage `json:"attempt"`
		Revision string          `json:"revision"`
		Changes  json.RawMessage `json:"changes"`
		Reason   *string         `json:"reason"`
		Consent  *string         `json:"consent"`
	}
	var ch dateEditChanges
	if !decodeStrict(raw, &body) || !dateTokenRe.MatchString(body.Revision) ||
		!decodeStrict(body.Changes, &ch) || ch.empty() {
		return nil, errDateEditInvalid
	}
	if body.Consent != nil && !consent.WellFormed(*body.Consent) {
		return nil, errDateEditInvalid
	}
	e := &dateEdit{base: body.Revision}
	if body.Consent != nil {
		e.consent = *body.Consent
	}
	var ok bool
	if e.startsLocal, ok = rawString(ch.StartsLocal); !ok {
		return nil, errDateEditInvalid
	}
	if e.title, ok = rawString(ch.Title); !ok {
		return nil, errDateEditInvalid
	}
	if e.location, ok = rawString(ch.Location); !ok {
		return nil, errDateEditInvalid
	}
	if len(ch.DurationMinutes) != 0 {
		var d int
		if bytes.Equal(bytes.TrimSpace(ch.DurationMinutes), []byte("null")) || json.Unmarshal(ch.DurationMinutes, &d) != nil {
			return nil, errDateEditInvalid
		}
		e.duration = &d
	}
	var err error
	if e.visibility, e.visibilitySet, err = parseDateVisibility(ch.Visibility); err != nil {
		return nil, errVisibilityNotAllowed
	}
	if body.Reason != nil && !e.visibilitySet {
		return nil, errDateEditInvalid
	}
	if e.visibilitySet && body.Reason != nil {
		e.reason = strings.TrimSpace(*body.Reason)
	}
	if len(e.reason) > 500 {
		return nil, errReasonTooLong
	}
	if e.capacity, e.capacitySet, err = parseCapacity(ch.CapacityOverride); err != nil {
		return nil, err
	}
	if e.remote, e.remoteSet, err = parseRemoteCapacity(ch.RemoteCapacityOverride, true); err != nil {
		return nil, err
	}
	if e.duration != nil && !events.ValidDuration(*e.duration) {
		return nil, errInvalidDuration
	}
	if err := validateEventText(strOrEmpty(e.title), "", strOrEmpty(e.location)); err != nil {
		return nil, err
	}
	return e, nil
}

// dateEditStep runs the test hook at a named point of the guarded edit:
// "selected" after the losers are selected, "effects" after every effect.
func (a *App) dateEditStep(tx *gorm.DB, step string) error {
	if a.hooks.dateEditStep == nil {
		return nil
	}
	return a.hooks.dateEditStep(tx, step)
}

// singleDate is what one attempt of a single-date edit tells the wrapper
// (contactChange.SingleDate): the date it holds, the clock its expiry,
// consent and cancellation were judged on, and the date's stored end before
// and after the edit. fn fills it in once the date is saved.
type singleDate struct {
	occurrenceID uint
	judged       time.Time
	ends         [2]time.Time
}

func (d *singleDate) saved(occurrenceID uint, judged, before, after time.Time) {
	d.occurrenceID, d.judged, d.ends = occurrenceID, judged, [2]time.Time{before, after}
}

// endCrossed is whether one of ends lies in (judged, fin]: a date that had
// not ended on the clock the edit judged on and has ended by fin.
func endCrossed(judged, fin time.Time, ends ...time.Time) bool {
	for _, e := range ends {
		if judged.Before(e) && !fin.Before(e) {
			return true
		}
	}
	return false
}

// closeSingleDate is a single-date edit's last locking work, at the end of
// reconcileContacts: the date's own "after" access pass, then a fresh clock.
// The edit's later locks (rooms, notice recipients, waiters) and the wrapper's
// request reconciliation before this (notification tasks, recipients) may
// each wait on a writer that holds no contact gate. If a stored end of the
// date fell inside the attempt, its expiry, notices and promotion were judged
// on a clock that no longer holds, so the attempt rolls back. After this the
// wrapper runs only its seal (NOWAIT, when set), the reserve check and
// COMMIT, so the window left is that statement time, with no lock wait.
func (a *App) closeSingleDate(tx *gorm.DB, d *singleDate) error {
	if d.occurrenceID == 0 {
		return errors.New("a single-date edit returned without saving its date")
	}
	if _, err := a.reconcileDateAccesses(tx, d.occurrenceID); err != nil {
		return err
	}
	if endCrossed(d.judged, a.now().UTC(), d.ends[:]...) {
		return errDateEndedMidEdit
	}
	return nil
}

// singleDateMutation runs a single-date edit under the contact gate and runs
// it once more when the date's end passed during the first attempt. The rerun
// judges on a clock past that end, so its before pass (or its saved-end pass)
// ends the grant; only a second end crossing inside the rerun is answered
// date_changed, with nothing written. fn must reread everything it decides on,
// since each attempt is a fresh transaction.
// begin, when set, runs before each attempt opens, so what an attempt records
// starts empty even when the attempt fails before fn runs.
func (a *App) singleDateMutation(eventID uint, begin func(), fn func(tx *gorm.DB, d *singleDate) error) error {
	var err error
	for attempt := range 2 {
		if begin != nil {
			begin()
		}
		if a.hooks.singleDateAttempts != nil {
			a.hooks.singleDateAttempts.Add(1)
		}
		d := &singleDate{}
		err = a.contactMutation(contactChange{EventID: eventID, SingleDate: d}, func(tx *gorm.DB) error { return fn(tx, d) })
		if !errors.Is(err, errDateEndedMidEdit) || !rollbackConfirmed(err) {
			return err
		}
		if attempt == 0 && a.hooks.singleDateBeforeRetry != nil {
			a.hooks.singleDateBeforeRetry()
		}
	}
	return errDateChanged
}

// apiEditDate is POST /events/:id/occurrences/:date/edits.
func (a *App) apiEditDate(c *gin.Context) {
	ev, ok := a.loadManagedEvent(c)
	if !ok {
		return
	}
	date := c.Param("date")
	if _, err := models.ParseCalendarDate(date); err != nil {
		fail(c, errSeriesDateInvalid)
		return
	}
	raw, err := c.GetRawData()
	if err != nil {
		fail(c, errDateEditInvalid)
		return
	}
	edit, err := parseDateEdit(raw)
	if err != nil {
		fail(c, err)
		return
	}
	if a.restoreDetected() {
		fail(c, errRestoreDetected)
		return
	}
	var resp occurrenceJSON
	var judged sessionJudgment
	err = a.singleDateMutation(ev.ID, func() { judged = sessionJudgment{} }, func(tx *gorm.DB, d *singleDate) error {
		// The check relied on, under the contact gate and on every attempt: a
		// fenced writer marks the process restored before its rollback
		// releases the gate, so this sees a detection made between a
		// rollback and the rerun. Nothing runs between the gate and here.
		if a.restoreDetected() {
			return errRestoreDetected
		}
		var err error
		resp, err = a.editDateIn(tx, d, apiUser(c), c.ClientIP(), ev.ID, date, edit, &judged)
		return err
	})
	// A participating edit is acknowledged for the actor the attempt that
	// decided it locked, only once its outcome is known: committed, or
	// rolled back with the rollback confirmed (a settled no-op, a refusal).
	// An unknown COMMIT, an unconfirmed rollback, the refusals made before
	// the writer (the preflight, the body, a detected restore) and the 401
	// carry none.
	var settled *dateEditSettled
	decided := err == nil || (rollbackConfirmed(err) && (errors.As(err, &settled) || errors.Is(err, errDateVisibility) || decidedRefusal(err)))
	if ackErr := a.acknowledgeDecision(c, judged, decided); ackErr != nil {
		jsonError(c, http.StatusInternalServerError, "could not save this date")
		return
	}
	switch {
	case errors.As(err, &settled) && rollbackConfirmed(err):
		c.JSON(http.StatusOK, occurrenceResponseJSON{Occurrence: settled.resp})
	case errors.As(err, &settled):
		// The answer was ready but the rollback was not acknowledged.
		jsonError(c, http.StatusInternalServerError, "could not save this date")
	case errors.Is(err, errDateVisibility):
		fail(c, errVisibilityNotAllowed)
	case isAPIError(err):
		fail(c, err)
	case err != nil:
		jsonError(c, http.StatusInternalServerError, "could not save this date")
	default:
		c.JSON(http.StatusOK, occurrenceResponseJSON{Occurrence: resp})
	}
}

// editDateIn is one attempt of the guarded edit (singleDateMutation). Lock
// order: actor, event, date, and only then this date's registration rows, as
// every registration writer takes them (registration_locks.go). It locks no
// other date's rows: a self-service RSVP or a household change holds a date
// while it promotes a waiter, so a grant row of another date could close a
// cycle with it.
func (a *App) editDateIn(tx *gorm.DB, d *singleDate, me *models.User, ip string, eventID uint, date string, edit *dateEdit, judged *sessionJudgment) (occurrenceJSON, error) {
	me, err := lockActorIn(tx, me, actorRead)
	if err != nil {
		return occurrenceJSON{}, err
	}
	judged.judge(me)
	var ev models.Event
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&ev, eventID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return occurrenceJSON{}, errEventNotFound
		}
		return occurrenceJSON{}, err
	}
	f, err := eventFactsIn(tx, me, &ev)
	if err != nil {
		return occurrenceJSON{}, err
	}
	if !f.canManage(me) {
		return occurrenceJSON{}, errOccurrenceNotFound
	}
	var o models.EventOccurrence
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("event_id = ? AND original_date = ?", ev.ID, date).First(&o).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return occurrenceJSON{}, errOccurrenceNotFound
		}
		return occurrenceJSON{}, err
	}
	// The date's "before" access pass, under the date lock, and the clock of
	// this edit's expiry, consent and cancellation, read only once the date's
	// grant rows are locked: a retained departure grant whose date
	// ended, even while this request waited for any of its locks, is ended
	// against the stored end before the edit can move the date and revive it.
	now, err := a.reconcileDateAccesses(tx, o.ID)
	if err != nil {
		return occurrenceJSON{}, err
	}
	shown := a.groupShownTo(me, f.group, f.rel)
	current := a.dateToken(&ev, &o, shown)
	if edit.base[2:18] != a.editGeneration() || edit.base != current {
		return occurrenceJSON{}, errDateChanged
	}
	var wallStart *time.Time
	if edit.startsLocal != nil {
		t, _, err := events.ParseWall(*edit.startsLocal, ev.TZID)
		if err != nil {
			return occurrenceJSON{}, specAnswer(err)
		}
		u := t.UTC()
		wallStart = &u
	}
	before := policy.EffectiveVisibility(ev.Visibility, o.VisibilityOverride)
	after := before
	if edit.visibilitySet {
		after = policy.EffectiveVisibility(ev.Visibility, edit.visibility)
		if !policy.CanSetEventVisibility(asPerson(me), f.view, f.rel, after) {
			return occurrenceJSON{}, errDateVisibility
		}
	}

	next := o
	if edit.duration != nil {
		next.DurationOverride = edit.duration
	}
	if wallStart != nil {
		next.StartsAtOverride = wallStart
	}
	next.StartsAt, next.EndsAt = dateClock(&ev, &o, wallStart, edit.duration)
	if edit.location != nil {
		next.LocationOverride = edit.location
	}
	if edit.title != nil {
		next.TitleOverride = edit.title
	}
	if edit.capacitySet {
		next.CapacityOverride = edit.capacity
	}
	if edit.remoteSet {
		next.RemoteCapacityOverride = edit.remote
	}
	if sameDateColumns(&o, &next) && (!edit.visibilitySet || strPtrEqual(o.VisibilityOverride, edit.visibility)) {
		resp, err := a.managedDateIn(tx, me, &ev, &o, shown, current)
		if err != nil {
			return occurrenceJSON{}, err
		}
		return occurrenceJSON{}, &dateEditSettled{resp}
	}
	// DEC-156: the before pass stamped every grant that lapsed on now, so a
	// kept grant still live refuses a later end before anything is written
	// or asked.
	if next.EndsAt.After(o.EndsAt) {
		if err := refuseKeptDates(tx, map[uint]string{o.ID: models.FormatCalendarDate(o.OriginalDate)}); err != nil {
			return occurrenceJSON{}, err
		}
	}

	// prior is the date as stored before this edit: the cancellation copy and
	// each holder's before view of the place are judged on it.
	prior := o
	oldStart, oldEnd := o.StartsAt, o.EndsAt
	oldOverride, oldRemoteOverride := o.CapacityOverride, o.RemoteCapacityOverride
	o = next
	if err := tx.Save(&o).Error; err != nil {
		return occurrenceJSON{}, err
	}
	// The saved end is judged before anything that depends on an invitation
	// (the change notices, promotion): a grant whose date, as saved, has
	// already ended is ended here, so nothing is sent through it.
	if _, err := a.reconcileDateAccesses(tx, o.ID); err != nil {
		return occurrenceJSON{}, err
	}
	d.saved(o.ID, now, oldEnd, o.EndsAt)
	// The losers are selected on the date as saved (its new start), locked;
	// the consent rule runs for every visibility change and binds the date as
	// it was shown before this edit (prior), and exactly the checked rows are
	// cancelled.
	cancelled := false
	if edit.visibilitySet {
		var sel *dateLosers
		if policy.MoreOpen(before, after) {
			s, err := selectDateLosers(tx, &ev, before, after, now, o.ID)
			if err != nil {
				return occurrenceJSON{}, err
			}
			// Cancelled people who could see the date are told the title it
			// had, not one this edit sets on a date they may no longer see.
			s.prior = map[uint]*models.EventOccurrence{o.ID: &prior}
			s.callerPromotes = true
			if err := a.dateEditStep(tx, "selected"); err != nil {
				return occurrenceJSON{}, err
			}
			sel = &s
			cancelled = len(s.rows) > 0
		}
		b, h := dateImpactIn(me.ID, &ev, &prior, before, after, sel)
		plan, token, err := a.checkConsentIn(tx, &edit.consent, h, b.entries)
		if errors.Is(err, errConsentMismatch) {
			impact := b.impact
			impact.Consent = token
			return occurrenceJSON{}, dateNeedsConsent{dateNeedsConsentJSON{Impact: impact}}
		}
		if err != nil {
			return occurrenceJSON{}, err
		}
		var cd consent.Checked[dateLosers]
		if k := dateRowKey(consent.ListTightened, &ev, &prior); plan.Has(k) {
			if cd, err = plan.Take(k); err != nil {
				return occurrenceJSON{}, err
			}
		}
		if err := a.changeDateVisibilitySelected(tx, me, ip, &ev, &o, f.group, edit.visibility, edit.reason, false, cd); err != nil {
			return occurrenceJSON{}, err
		}
		if err := plan.Done(); err != nil {
			return occurrenceJSON{}, err
		}
	}
	timeChanged := !o.StartsAt.Equal(oldStart) || !o.EndsAt.Equal(oldEnd)
	if timeChanged {
		if err := settleDateRooms(tx, &ev, []uint{o.ID}, nil, &me.ID); err != nil {
			return occurrenceJSON{}, err
		}
	}
	// Whether the place changed is each recipient's own answer: pinning the
	// group's place on a date shows it to someone who may not see the group.
	if timeChanged || !strPtrEqual(prior.LocationOverride, o.LocationOverride) {
		if err := notifyDateChanged(tx, &ev, &prior, &o, timeChanged, &me.ID); err != nil {
			return occurrenceJSON{}, err
		}
	}
	// One promotion pass, on the date as saved (its visibility, time and
	// caps), once every cancellation and cap is in: a seat freed by a
	// cancelled loser and one opened by a raised cap are offered together,
	// and inside the cutoff the runners are told once. Its own clock, read
	// here: a room or recipient wait above may have carried the date inside
	// the promotion cutoff (PLAN 5.4).
	if cancelled || poolMoved(ev.Capacity, oldOverride, o.CapacityOverride) || poolMoved(ev.RemoteCapacity, oldRemoteOverride, o.RemoteCapacityOverride) {
		if err := a.promoteLocked(tx, &ev, &o, a.now().UTC(), &me.ID); err != nil {
			return occurrenceJSON{}, err
		}
	}
	if err := a.dateEditStep(tx, "effects"); err != nil {
		return occurrenceJSON{}, err
	}
	// The answer's token is read after every effect: the triggers of 00010
	// raised the counters, and no reconcile pass after the callback writes the
	// event or the date again.
	if err := tx.Raw("SELECT e.edit_revision AS event, o.edit_revision AS date FROM events e JOIN event_occurrences o ON o.event_id = e.id WHERE o.id = ?", o.ID).
		Row().Scan(&ev.EditRevision, &o.EditRevision); err != nil {
		return occurrenceJSON{}, err
	}
	return a.managedDateIn(tx, me, &ev, &o, shown, a.dateToken(&ev, &o, shown))
}

// sameDateColumns is whether next writes nothing the guarded edit writes
// that o does not already hold.
func sameDateColumns(o, next *models.EventOccurrence) bool {
	return timePtrEqual(o.StartsAtOverride, next.StartsAtOverride) && o.StartsAt.Equal(next.StartsAt) && o.EndsAt.Equal(next.EndsAt) &&
		intPtrEqual(o.DurationOverride, next.DurationOverride) && strPtrEqual(o.LocationOverride, next.LocationOverride) &&
		strPtrEqual(o.TitleOverride, next.TitleOverride) && intPtrEqual(o.CapacityOverride, next.CapacityOverride) &&
		intPtrEqual(o.RemoteCapacityOverride, next.RemoteCapacityOverride)
}

func strPtrEqual(a, b *string) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

func timePtrEqual(a, b *time.Time) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && a.Equal(*b))
}

// poolMoved is whether a per-date override change moved the effective cap of
// the pool, which promotes into new seats (a lowered cap removes nobody).
func poolMoved(series, old, next *int) bool {
	return !intPtrEqual(old, next) && !intPtrEqual(events.EffectiveCapacity(series, old), events.EffectiveCapacity(series, next))
}

// dateOwn is the date's own values, the columns the guarded edit writes.
func dateOwn(ev *models.Event, o *models.EventOccurrence) *dateOwnJSON {
	own := &dateOwnJSON{
		DurationMinutes: o.DurationOverride, Title: o.TitleOverride, Location: o.LocationOverride,
		CapacityOverride: o.CapacityOverride, RemoteCapacityOverride: o.RemoteCapacityOverride, Visibility: o.VisibilityOverride,
	}
	if o.StartsAtOverride != nil {
		loc := eventZone(ev.TZID)
		start := events.FormatWall(o.StartsAtOverride.In(loc), loc)
		own.StartsLocal = &start
	}
	return own
}

// managedDateIn reads the date a manager edits, including seats, childcare and
// their own answer. A cancelled date carries no group label.
func (a *App) managedDateIn(tx *gorm.DB, me *models.User, ev *models.Event, o *models.EventOccurrence, shown *models.Group, revision string) (occurrenceJSON, error) {
	shownPlace := ""
	if shown != nil {
		shownPlace = shown.Location
	}
	inst := occurrenceToJSON(ev, o, shownPlace)
	inst.Revision = revision
	inst.Own = dateOwn(ev, o)
	inst.Cancelled = o.Status == models.OccurrenceCancelled
	if o.Status == models.OccurrenceScheduled {
		l := labelGroup(shown, true, &siteNameOnce{a: a, db: tx})
		inst.GroupID, inst.GroupName, inst.GroupVisibility, inst.HostedBy = l.ID, l.Name, l.Visibility, l.HostedBy
	}
	inst.Room, _ = dateRoomIn(tx, o.ID)
	applyVideoReveal(&inst, ev, false, true) // managers see the link
	inst.Visibility = policy.EffectiveVisibility(ev.Visibility, o.VisibilityOverride)
	inst.LeaderOpened = ev.LeaderOpened
	inst.ChurchWide = ev.GroupID == nil
	used, err := seatsUsedForMode(tx, o.ID, models.AttendanceInPerson, 0)
	if err != nil {
		return inst, err
	}
	inst.SeatsUsed = used
	inst.SpotsLeft = spotsLeftFor(inst.Capacity, used)
	remoteUsed, err := seatsUsedForMode(tx, o.ID, models.AttendanceRemote, 0)
	if err != nil {
		return inst, err
	}
	inst.RemoteSeatsUsed = remoteUsed
	inst.RemoteSpotsLeft = spotsLeftFor(inst.RemoteCapacity, remoteUsed)
	if ev.ChildcareAvailable {
		n, err := childcareUsedFor(tx, o.ID, 0)
		if err != nil {
			return inst, err
		}
		inst.ChildcareUsed = &n
		inst.ChildcareSpotsLeft = spotsLeftFor(ev.ChildcareCapacity, n)
	}
	var mine models.Registration
	if err := tx.Where("user_id = ? AND occurrence_id = ? AND status <> ?", me.ID, o.ID, models.RegCancelled).
		Limit(1).Find(&mine).Error; err != nil {
		return inst, err
	}
	if mine.ID != 0 {
		if err := inst.setMine(tx, &mine); err != nil {
			return inst, err
		}
	}
	return inst, nil
}

// apiGetManagedDate is GET /events/:id/occurrences/:date: one date of an event
// as its manager edits it, scheduled or cancelled, with its revision token.
// It is the edit base for a date no list shows (a cancelled one, or one that
// has started). The same 404 and 403 as the edit; the cancelled-date
// tombstone on /events/:id/dates/:date is unchanged and carries no token.
// Everything is read in one read-only Repeatable Read snapshot, with the
// viewer and their authority re-read in it, so the token describes the rows
// the answer shows. A scheduled date that has not ended on the snapshot's one
// clock also carries the roster the event list gives a manager, with its kept
// count (managedRosterIn); a cancelled or ended date is history and carries
// none. The snapshot alone decides every refusal (managedDateTargetIn), so a
// participating request is acknowledged for the viewer judged there, on the
// answer and on each refusal whose rollback was confirmed (decidingSnapshot).
func (a *App) apiGetManagedDate(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	var inst occurrenceJSON
	var judged sessionJudgment
	err := decidingSnapshot(a.DB.WithContext(c.Request.Context()), func(tx *gorm.DB) error {
		t, err := a.managedDateTargetIn(tx, apiUser(c), id, c.Param("date"), &judged)
		if err != nil {
			return err
		}
		shown := a.groupShownTo(t.me, t.f.group, t.f.rel)
		inst, err = a.managedDateIn(tx, t.me, &t.ev, &t.o, shown, a.dateToken(&t.ev, &t.o, shown))
		if err != nil {
			return err
		}
		if now := a.now().UTC(); t.o.Status == models.OccurrenceScheduled && now.Before(t.o.EndsAt) {
			return a.managedRosterIn(tx, t.me, &t.ev, t.o, t.f, &inst, now)
		}
		return nil
	})
	if ackErr := a.acknowledgeDecision(c, judged, err == nil || decidedRefusal(err)); ackErr != nil {
		err = ackErr
	}
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, occurrenceResponseJSON{Occurrence: inst})
}

// managedDateTarget is one date of an event and the manager reaching it, as
// managedDateTargetIn found them in its snapshot.
type managedDateTarget struct {
	me *models.User
	ev models.Event
	f  eventFacts
	o  models.EventOccurrence
}

// managedDateTargetIn decides, on tx (a read-only snapshot), whether viewer
// reaches one date of event id as its manager, in the edit's order: the
// viewer re-read in the snapshot and judged (401 when no longer signed in),
// a hidden or missing event (the same 404), a viewer who can't manage it
// (403), a malformed date (400), then a date the event doesn't have (404).
// Only a manager learns whether the date exists.
func (a *App) managedDateTargetIn(tx *gorm.DB, viewer *models.User, id int, date string, judged *sessionJudgment) (managedDateTarget, error) {
	var t managedDateTarget
	me, err := snapshotViewerIn(tx, viewer)
	if err != nil {
		return t, err
	}
	if me == nil {
		return t, errSecuritySession
	}
	judged.judge(me)
	t.me = me
	if err := tx.First(&t.ev, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return t, errEventNotFound
		}
		return t, err
	}
	if t.f, err = eventFactsIn(tx, me, &t.ev); err != nil {
		return t, err
	}
	if !t.f.canSee(me) {
		return t, errEventNotFound
	}
	if !t.f.canManage(me) {
		return t, errCannotChangeEvent
	}
	if !isCalendarDate(date) {
		return t, errSeriesDateInvalid
	}
	if a.hooks.managedDateStep != nil {
		a.hooks.managedDateStep("authorized")
	}
	if err := tx.Where("event_id = ? AND original_date = ?", t.ev.ID, date).First(&t.o).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return t, errOccurrenceNotFound
		}
		return t, err
	}
	return t, nil
}

// managedRosterIn adds to inst the roster the event list gives a manager for
// date o: attendees with the manager flags, removed and recoverable rows,
// counts with "can't go", contact details by the list's rule, and the date's
// kept count. Every read is on tx, the managed read's snapshot, and every
// decision on its one clock now: the care-notes window, recoverable-row
// eligibility, and household visibility and ages, which count from the
// church zone read on tx (requestFactsAt), so a zone that cannot be read
// fails the read instead of deciding on UTC.
func (a *App) managedRosterIn(tx *gorm.DB, me *models.User, ev *models.Event, o models.EventOccurrence, f eventFacts, inst *occurrenceJSON, now time.Time) error {
	facts, err := requestFactsAt(tx, now)
	if err != nil {
		return err
	}
	o.Event = *ev
	p := asPerson(me)
	show := map[uint]bool{ev.ID: policy.CanSeeAttendeeNames(p, f.view, asGroup(f.group), f.rel)}
	childcare := map[uint]bool{ev.ID: policy.CanSeeChildcare(p, f.view, f.rel)}
	names, removed, err := a.attendeeNames(rosterRead{db: tx, now: now, snapshot: true}, []models.EventOccurrence{o}, show, childcare, map[uint]bool{ev.ID: true}, me, facts)
	if err != nil {
		return err
	}
	guardians := confirmedContactGuardians(tx, me, facts.users)
	presentAttendeeContacts(names[o.ID], me, facts.users, guardians, true)
	presentAttendeeContacts(removed[o.ID], me, facts.users, guardians, true)
	counts, err := registrationCountsByDate(tx, []uint{o.ID})
	if err != nil {
		return err
	}
	kept, err := keptDatesIn(tx, []uint{o.ID})
	if err != nil {
		return err
	}
	n := kept[o.ID]
	inst.Attendees, inst.RemovedRegistrations = names[o.ID], removed[o.ID]
	inst.Registrations = registrationCounts(counts[o.ID], true)
	inst.AttendeesVisible = true
	inst.KeptCount = &n
	return nil
}
