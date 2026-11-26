package app

import (
	"errors"
	"net/http"
	"reflect"
	"slices"
	"time"

	"fold/internal/consent"
	"fold/internal/policy"
	"fold/models"

	"gorm.io/gorm"
)

// Exact visibility consent (DEC-158). A change that cancels anyone, or is a
// reset of an event's dates, is saved only with the token of the impact its
// reviewer was shown: an HMAC (internal/consent) over the scope, the actor,
// the consent epoch, the scope's current visibility, every displayed row and
// reset date, and the identities of the registrations each row cancels. A
// write rebuilds that impact from its locked selections and goes on only when
// the tokens match; it then cancels exactly the checked selections, and the
// event route resets exactly the checked dates. Everything else (a change
// that cancels nobody and is not a reset) needs no token.
//
// checkConsentIn also declares the outcome in the transaction, which 00015's
// audit fence reads: 'v1' after a passing check, 'v1:no-review' when nothing
// needed one. Every writer of an event visibility audit row calls it first.

var errVisibilityReviewOutdated = apiErr(http.StatusConflict, "visibility_review_outdated", "the page that made this review is out of date")

// errConsentMismatch: a review-required change's consent does not match its
// impact now. The route answers its decision with a fresh impact and token.
var errConsentMismatch = errors.New("the reviewed visibility change no longer matches")

// consentEpochLockSQL is the checked confirmation's one epoch read. FOR SHARE
// conflicts with the boot's rotation (consentEpochRotateSQL, FOR NO KEY
// UPDATE), so the two are ordered: FOR KEY SHARE would not be, and a plain
// read takes no lock (design 2.5.1).
const consentEpochLockSQL = "SELECT value FROM settings WHERE key = ? FOR SHARE"

const (
	consentChecked  = "v1"
	consentNoReview = "v1:no-review"
)

func (a *App) consentKey() ([]byte, error) {
	return deriveKey(a.Config.SessionSecret, "visibility-consent:v1")
}

// consentEpochIn is the plain read of the epoch, for a token that is only
// offered (consentTokenIn). A missing row is an error.
func consentEpochIn(tx *gorm.DB) (string, error) {
	var v []string
	if err := tx.Raw("SELECT value FROM settings WHERE key = ?", consentEpochKey).Scan(&v).Error; err != nil {
		return "", err
	}
	if len(v) != 1 {
		return "", errors.New("the visibility consent epoch is missing")
	}
	return v[0], nil
}

// lockConsentEpochIn is the epoch a checked confirmation compares, held FOR
// SHARE until its transaction ends.
func (a *App) lockConsentEpochIn(tx *gorm.DB) (string, error) {
	var v []string
	if err := tx.Raw(consentEpochLockSQL, consentEpochKey).Scan(&v).Error; err != nil {
		return "", err
	}
	if len(v) != 1 {
		return "", errors.New("the visibility consent epoch is missing")
	}
	if h := a.hooks.consentEpochLocked; h != nil {
		h(tx, v[0])
	}
	return v[0], nil
}

// consentTokenIn mints the token of a change without checking one: the
// previews and the group's missing-decision answer.
func (a *App) consentTokenIn(tx *gorm.DB, h consent.Header, entries []consent.Entry[dateLosers]) (string, error) {
	key, err := a.consentKey()
	if err != nil {
		return "", err
	}
	if h.Epoch, err = consentEpochIn(tx); err != nil {
		return "", err
	}
	return consent.TokenFor(key, h, entries)
}

// checkConsentIn runs the consent rule once, at its caller's top level, and
// declares the outcome in tx. A review-required change (consent.ReviewRequired)
// is checked against given: absent is errVisibilityReviewOutdated (a page
// from before this rule), anything else that does not match is
// errConsentMismatch with the fresh token. Any other change passes unchecked.
// The caller cancels only what plan.Take hands out and returns plan.Done().
func (a *App) checkConsentIn(tx *gorm.DB, given *string, h consent.Header, entries []consent.Entry[dateLosers]) (*consent.Plan[dateLosers], string, error) {
	var plan *consent.Plan[dateLosers]
	var token string
	declared := consentNoReview
	if consent.ReviewRequired(h, entries) {
		key, err := a.consentKey()
		if err != nil {
			return nil, "", err
		}
		if h.Epoch, err = a.lockConsentEpochIn(tx); err != nil {
			return nil, "", err
		}
		value := ""
		if given != nil {
			value = *given
		}
		plan, token, err = consent.Check(key, value, h, entries)
		switch {
		case errors.Is(err, consent.ErrMismatch) && given == nil:
			return nil, token, errVisibilityReviewOutdated
		case errors.Is(err, consent.ErrMismatch):
			return nil, token, errConsentMismatch
		case err != nil:
			return nil, "", err
		}
		declared = consentChecked
		if h := a.hooks.consentPassed; h != nil {
			h(entries)
		}
	} else {
		var err error
		if plan, _, err = consent.Unchecked(h, entries); err != nil {
			return nil, "", err
		}
	}
	if err := tx.Exec("SELECT set_config('fold.visibility_consent', ?, true)", declared).Error; err != nil {
		return nil, "", err
	}
	if hook := a.hooks.consentDeclared; hook != nil {
		var got string
		if err := tx.Raw("SELECT coalesce(current_setting('fold.visibility_consent', true), '')").Scan(&got).Error; err != nil {
			return nil, "", err
		}
		hook(got)
	}
	return plan, token, nil
}

// ConsentIdentities lists the selection's rows as the token binds them.
func (s dateLosers) ConsentIdentities() []consent.Identity {
	out := make([]consent.Identity, 0, len(s.rows))
	for _, r := range s.rows {
		out = append(out, consent.Identity{Registration: uint64(r.ID), Occurrence: uint64(r.OccurrenceID),
			User: optID(r.UserID), RegisteredBy: optID(r.RegisteredBy), HouseholdPerson: optID(r.HouseholdPersonID)})
	}
	return out
}

func optID(p *uint) *uint64 {
	if p == nil {
		return nil
	}
	v := uint64(*p)
	return &v
}

// Clone is a deep copy of the selection: the plan keeps one and hands out
// another on every Selection call, so nothing outside internal/consent holds
// a reference into what was checked (TestDateLosersCloneIsDeep).
func (s dateLosers) Clone() dateLosers {
	return dateLosers{
		now:            s.now,
		occIDs:         slices.Clone(s.occIDs),
		dates:          deepCopy(s.dates),
		rows:           deepCopy(s.rows),
		oldVis:         s.oldVis,
		prior:          deepCopy(s.prior),
		callerPromotes: s.callerPromotes,
	}
}

// deepCopy copies v through every pointer, slice, map and struct field. A
// time.Time is copied as a value (its *time.Location is immutable and
// shared).
func deepCopy[T any](v T) T {
	out := deepValue(reflect.ValueOf(&v).Elem())
	return out.Interface().(T)
}

func deepValue(v reflect.Value) reflect.Value {
	out := reflect.New(v.Type()).Elem()
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			p := reflect.New(v.Type().Elem())
			p.Elem().Set(deepValue(v.Elem()))
			out.Set(p)
		}
	case reflect.Slice:
		if !v.IsNil() {
			s := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
			for i := 0; i < v.Len(); i++ {
				s.Index(i).Set(deepValue(v.Index(i)))
			}
			out.Set(s)
		}
	case reflect.Map:
		if !v.IsNil() {
			m := reflect.MakeMapWithSize(v.Type(), v.Len())
			for _, k := range v.MapKeys() {
				m.SetMapIndex(deepValue(k), deepValue(v.MapIndex(k)))
			}
			out.Set(m)
		}
	case reflect.Struct:
		out.Set(v)
		if v.Type() == timeType {
			break
		}
		for i := 0; i < v.NumField(); i++ {
			if out.Field(i).CanSet() {
				out.Field(i).Set(deepValue(v.Field(i)))
			}
		}
	default:
		out.Set(v)
	}
	return out
}

// impactRowFrom is the one builder of an impact row: the JSON the reviewer
// sees and the key and display the token binds. occ nil is an event row over
// the series dates, counted as distinct people (user and household person,
// NULL a value of its own); otherwise a date row counted by its registrations.
// vis is the row's visibility before the change. A leader-opened date row
// carries its date and decision key; a tightened date row shows its date in
// its title, as the date's question always has.
func impactRowFrom(list uint8, ev *models.Event, occ *models.EventOccurrence, vis string, regs []models.Registration) (impactEventJSON, consent.Key, consent.RowDisplay) {
	if occ == nil {
		row := impactEventJSON{ID: ev.ID, Title: ev.Title, Visibility: vis, Registrants: distinctSubjects(regs)}
		return row, consent.Key{List: list, Shape: consent.ShapeEvent, Event: uint64(ev.ID)},
			consent.RowDisplay{Title: row.Title, Visibility: row.Visibility, Registrants: uint64(row.Registrants)}
	}
	date := models.FormatCalendarDate(occ.OriginalDate)
	row := impactEventJSON{ID: ev.ID, OccurrenceID: occ.ID, Visibility: vis, Registrants: int64(len(regs))}
	if list == consent.ListLeaderOpened {
		row.Title, row.Date, row.DecisionKey = ev.Title, date, dateDecisionKey(occ.ID)
	} else {
		row.Title = ev.Title + " · " + date
	}
	return row, consent.Key{List: list, Shape: consent.ShapeDate, Event: uint64(ev.ID), Occurrence: uint64(occ.ID)},
		consent.RowDisplay{Date: date, DecisionKey: row.DecisionKey, Title: row.Title, Visibility: row.Visibility, Registrants: uint64(row.Registrants)}
}

func distinctSubjects(regs []models.Registration) int64 {
	type subject struct {
		user, person       uint
		hasUser, hasPerson bool
	}
	seen := map[subject]bool{}
	for _, r := range regs {
		var s subject
		if r.UserID != nil {
			s.user, s.hasUser = *r.UserID, true
		}
		if r.HouseholdPersonID != nil {
			s.person, s.hasPerson = *r.HouseholdPersonID, true
		}
		seen[s] = true
	}
	return int64(len(seen))
}

// resetDateFrom is the one builder of a reset date: the JSON row and the
// reset the token binds, from an override row futureOverridesAt yielded.
func resetDateFrom(ev *models.Event, occ *models.EventOccurrence) (impactResetDateJSON, consent.Reset) {
	vis := ""
	if occ.VisibilityOverride != nil {
		vis = *occ.VisibilityOverride
	}
	row := impactResetDateJSON{OccurrenceID: occ.ID, Date: models.FormatCalendarDate(occ.OriginalDate), Title: effectiveOccurrenceTitle(ev, occ), Visibility: vis}
	return row, consent.Reset{Occurrence: uint64(row.OccurrenceID), Date: row.Date, Title: row.Title, Visibility: row.Visibility}
}

// impactBuilder collects the rows of one impact with their entries.
type impactBuilder struct {
	impact  visibilityImpactJSON
	entries []consent.Entry[dateLosers]
}

func newImpactBuilder(target, current string) *impactBuilder {
	return &impactBuilder{impact: visibilityImpactJSON{Visibility: target, CurrentVisibility: current, Tightened: []impactEventJSON{}, LeaderOpened: []impactEventJSON{}}}
}

// add appends a row. Cancels is decided later for the group's leader-opened
// rows (setCancels); every other row cancels its selection when written.
func (b *impactBuilder) add(list uint8, ev *models.Event, occ *models.EventOccurrence, vis string, sel dateLosers) consent.Key {
	row, key, display := impactRowFrom(list, ev, occ, vis, sel.rows)
	if list == consent.ListTightened {
		b.impact.Tightened = append(b.impact.Tightened, row)
	} else {
		b.impact.LeaderOpened = append(b.impact.LeaderOpened, row)
	}
	b.entries = append(b.entries, consent.Entry[dateLosers]{Key: key, Row: display, Sel: sel, Cancels: list == consent.ListTightened})
	return key
}

func (b *impactBuilder) setCancels(k consent.Key, cancels bool) {
	for i := range b.entries {
		if b.entries[i].Key == k {
			b.entries[i].Cancels = cancels
		}
	}
}

// previewDateLosers is selectDateLosers' preview form: the same predicate
// (losersWhere), no lock, only the identity columns, in id order. A READ ONLY
// snapshot cannot lock.
func previewDateLosers(tx *gorm.DB, ev *models.Event, oldVis, newVis string, now time.Time, occurrenceID uint) (dateLosers, error) {
	return previewSelection(losersForDate(tx, ev, newVis, now, occurrenceID), oldVis, now)
}

// previewDateLosersAt is previewDateLosers for one date judged at startsAt,
// the start the same save would set.
func previewDateLosersAt(tx *gorm.DB, ev *models.Event, oldVis, newVis string, now time.Time, occurrenceID uint, startsAt time.Time) (dateLosers, error) {
	return previewSelection(losersForDateAt(tx, ev, newVis, now, occurrenceID, startsAt), oldVis, now)
}

func previewSelection(q *gorm.DB, oldVis string, now time.Time) (dateLosers, error) {
	sel := dateLosers{now: now, oldVis: oldVis}
	err := q.Select("registrations.id, registrations.occurrence_id, registrations.user_id, registrations.registered_by, registrations.household_person_id").
		Order("registrations.id").Scan(&sel.rows).Error
	return sel, err
}

// futureOverridesAt is the dates of eventIDs that start after now and carry
// their own visibility: the list an event reset changes and a group review
// shows.
func futureOverridesAt(tx *gorm.DB, now time.Time, eventIDs ...uint) *gorm.DB {
	return tx.Model(&models.EventOccurrence{}).
		Where("event_id IN ? AND starts_at > ? AND visibility_override IS NOT NULL", eventIDs, now)
}

// selector selects the losers of one row: selectDateLosers on a write, the
// preview form in a snapshot.
type selector func(ev *models.Event, oldVis, newVis string, occurrenceID uint) (dateLosers, error)

func lockingSelector(tx *gorm.DB, now time.Time) selector {
	return func(ev *models.Event, oldVis, newVis string, occurrenceID uint) (dateLosers, error) {
		return selectDateLosers(tx, ev, oldVis, newVis, now, occurrenceID)
	}
}

func previewSelector(tx *gorm.DB, now time.Time) selector {
	return func(ev *models.Event, oldVis, newVis string, occurrenceID uint) (dateLosers, error) {
		return previewDateLosers(tx, ev, oldVis, newVis, now, occurrenceID)
	}
}

// eventImpactIn is what setting ev (its stored row) to vis would do, with the
// entries a write cancels and the header the token binds: the series row when
// vis is more private, every future override counted and, with reset, each
// override listed as a reset date and as a tightened date row when it closes.
// dates is the override rows, locked by the caller when it writes.
func eventImpactIn(tx *gorm.DB, sel selector, now time.Time, actor uint, ev *models.Event, vis string, reset, lock bool) (*impactBuilder, consent.Header, []models.EventOccurrence, error) {
	b := newImpactBuilder(vis, ev.Visibility)
	h := consent.Header{Kind: consent.KindEvent, Actor: uint64(actor), Event: uint64(ev.ID), Target: vis, Current: ev.Visibility, Reset: reset}
	if policy.MoreOpen(ev.Visibility, vis) {
		s, err := sel(ev, ev.Visibility, vis, seriesDates)
		if err != nil {
			return nil, h, nil, err
		}
		b.add(consent.ListTightened, ev, nil, ev.Visibility, s)
	}
	q := futureOverridesAt(tx, now, ev.ID).Order("id")
	if lock {
		q = q.Clauses(lockForUpdate)
	}
	var dates []models.EventOccurrence
	if err := q.Find(&dates).Error; err != nil {
		return nil, h, nil, err
	}
	b.impact.FutureOverrides = int64(len(dates))
	h.FutureOverrides = uint64(len(dates))
	if reset {
		b.impact.ResetDates = []impactResetDateJSON{}
		for i := range dates {
			d := &dates[i]
			row, r := resetDateFrom(ev, d)
			b.impact.ResetDates = append(b.impact.ResetDates, row)
			h.ResetDates = append(h.ResetDates, r)
			if policy.MoreOpen(*d.VisibilityOverride, vis) {
				s, err := sel(ev, *d.VisibilityOverride, vis, d.ID)
				if err != nil {
					return nil, h, nil, err
				}
				b.add(consent.ListTightened, ev, d, *d.VisibilityOverride, s)
			}
		}
	}
	return b, h, dates, nil
}

// groupImpactIn is what making the group (stored at oldVis) newVis would do,
// with every row's entry (their Cancels set later from the decisions).
func groupImpactIn(tx *gorm.DB, sel selector, now time.Time, actor uint, groupID uint, oldVis, newVis string) (*impactBuilder, consent.Header, groupRows, error) {
	b := newImpactBuilder(newVis, oldVis)
	h := consent.Header{Kind: consent.KindGroup, Actor: uint64(actor), Group: uint64(groupID), Target: newVis, Current: oldVis}
	var rows groupRows
	var err error
	if rows.auto, rows.opened, err = affectedEvents(tx, groupID, oldVis, newVis); err != nil {
		return nil, h, rows, err
	}
	for _, list := range []struct {
		evs  []models.Event
		kind uint8
	}{{rows.auto, consent.ListTightened}, {rows.opened, consent.ListLeaderOpened}} {
		for i := range list.evs {
			ev := &list.evs[i]
			s, err := sel(ev, ev.Visibility, newVis, seriesDates)
			if err != nil {
				return nil, h, rows, err
			}
			b.add(list.kind, ev, nil, ev.Visibility, s)
		}
	}
	if rows.dates, rows.evs, err = openedDatesAt(tx, now, groupID, oldVis, newVis); err != nil {
		return nil, h, rows, err
	}
	for i := range rows.dates {
		d := &rows.dates[i]
		ev := rows.evs[d.EventID]
		s, err := sel(ev, *d.VisibilityOverride, newVis, d.ID)
		if err != nil {
			return nil, h, rows, err
		}
		b.add(consent.ListLeaderOpened, ev, d, *d.VisibilityOverride, s)
	}
	return b, h, rows, nil
}

// groupRows is what a group tightening reads: its events by kind, and the
// opened dates with their events.
type groupRows struct {
	auto, opened []models.Event
	dates        []models.EventOccurrence
	evs          map[uint]*models.Event
}

// dateImpactIn is a date's impact and its one entry: what making occ (as
// stored before the change, at before) after would do, given its losers.
func dateImpactIn(actor uint, ev *models.Event, occ *models.EventOccurrence, before, after string, sel *dateLosers) (*impactBuilder, consent.Header) {
	b := newImpactBuilder(after, before)
	h := consent.Header{Kind: consent.KindDate, Actor: uint64(actor), Event: uint64(ev.ID), Occurrence: uint64(occ.ID), Target: after, Current: before}
	if sel != nil {
		b.add(consent.ListTightened, ev, occ, before, *sel)
	}
	return b, h
}
