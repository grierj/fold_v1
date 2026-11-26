package app

import (
	"encoding/json"
	"errors"
	"net/http"

	"fold/internal/audit"
	"fold/internal/consent"
	"fold/internal/events"
	"fold/internal/policy"
	"fold/models"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"strconv"
	"time"
)

// canSeeDate is the ordinary, batched date filter. Candidate parents are never
// series authority: callers must separately use eventCandidates/loadVisibleEvent.
func (vs visibleSet) canSeeDate(viewer *models.User, ev *models.Event, occ *models.EventOccurrence) bool {
	if ev == nil {
		return false
	}
	f := vs.of(ev)
	if ev.GroupID != nil && f.group == nil {
		return false
	}
	f.view.Visibility = policy.EffectiveVisibility(ev.Visibility, occ.VisibilityOverride)
	return f.canSee(viewer)
}

func dateEvent(ev *models.Event, occ *models.EventOccurrence) *models.Event {
	copy := *ev
	copy.Visibility = policy.EffectiveVisibility(ev.Visibility, occ.VisibilityOverride)
	return &copy
}

// canRegisterDateIn is canRegisterDateErrIn where a failed read can only say no.
func (a *App) canRegisterDateIn(tx *gorm.DB, u *models.User, ev *models.Event, occurrenceID uint) bool {
	ok, _ := a.canRegisterDateErrIn(tx, u, ev, occurrenceID)
	return ok
}

// canRegisterDateErrIn is whether u may register for the scheduled date
// occurrenceID of ev; a date that is not scheduled says no, and a failed read
// is its error.
func (a *App) canRegisterDateErrIn(tx *gorm.DB, u *models.User, ev *models.Event, occurrenceID uint) (bool, error) {
	var occ models.EventOccurrence
	if err := tx.Where("id = ? AND event_id = ? AND status = ?", occurrenceID, ev.ID, models.OccurrenceScheduled).First(&occ).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return false, err
	}
	return a.canRegisterErrIn(tx, u, dateEvent(ev, &occ))
}

func (a *App) canRegisterDate(tx *gorm.DB, u *models.User, ev *models.Event, occ *models.EventOccurrence) bool {
	if occ.EventID != ev.ID || occ.Status != models.OccurrenceScheduled {
		return false
	}
	return a.canRegisterIn(tx, u, dateEvent(ev, occ))
}

var errDateVisibility = errors.New("invalid date visibility")

func parseDateVisibility(raw json.RawMessage) (*string, bool, error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	if string(raw) == "null" {
		return nil, true, nil
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return nil, true, errDateVisibility
	}
	if value == "series" {
		return nil, true, nil
	}
	if !policy.ValidVisibility(value) {
		return nil, true, errDateVisibility
	}
	return &value, true, nil
}

// Caller holds the contact gate, event and occurrence locks, and ran
// checkConsentIn earlier in the same transaction. Cancellation, audit and
// stored notices share the same transaction as the date edit.
//
// group is the event's group as it will be once the surrounding change is
// saved (a group edit passes its in-memory copy), or nil to read it. keep
// records the date as deliberately kept open: it audits and notifies as an
// override even though nothing changes. c is the checked selection of the
// date's losers (consent.Plan.Take): exactly those are cancelled when the date
// becomes more private. The zero Checked cancels nobody (a kept date, or a
// change with nobody to cancel).
func (a *App) changeDateVisibilitySelected(tx *gorm.DB, actor *models.User, ip string, ev *models.Event, occ *models.EventOccurrence, group *models.Group, value *string, reason string, keep bool, c consent.Checked[dateLosers]) error {
	old := occ.VisibilityOverride
	if !keep && ((old == nil && value == nil) || (old != nil && value != nil && *old == *value)) {
		return nil
	}
	before, after := policy.EffectiveVisibility(ev.Visibility, old), policy.EffectiveVisibility(ev.Visibility, value)

	if group == nil {
		f, err := eventFactsIn(tx, actor, ev)
		if err != nil {
			return err
		}
		group = f.group
	}
	action := audit.VisibilityChanged
	if group != nil && policy.MoreOpen(after, group.Visibility) {
		action = audit.VisibilityOverride
	}
	meta := map[string]any{"original_date": models.FormatCalendarDate(occ.OriginalDate), "oldVisibility": old, "newVisibility": value, "reason": reason}
	if group != nil {
		meta["groupVisibility"] = group.Visibility
	}
	if err := audit.Write(tx, audit.Entry{Actor: &actor.ID, Action: action, TargetType: "event", TargetID: ev.ID, Meta: meta, IP: ip}); err != nil {
		return err
	}
	occ.VisibilityOverride = value
	if err := tx.Model(occ).Update("visibility_override", value).Error; err != nil {
		return err
	}
	if policy.MoreOpen(before, after) && !keep {
		if err := a.cancelSelectedLosers(tx, c, actor, ip, ev, after, occ.ID); err != nil {
			return err
		}
	}
	if action == audit.VisibilityOverride && group != nil {
		date := *ev
		date.Title = ev.Title + " · " + models.FormatCalendarDate(occ.OriginalDate)
		if err := notifyVisibilityOverride(tx, actor.ID, &date, group, after, reason); err != nil {
			return err
		}
	}
	return nil
}

// apiDateVisibilityImpact previews a date's visibility change: GET
// /events/:id/occurrences/:date/visibility-impact?visibility=...
// [&startsLocal=...][&durationMinutes=...]. The optional times are the ones
// the same save would set, so the count is the one the PUT asks consent for:
// the date as it would be saved. The viewer, their authority and the count
// are read in one snapshot.
func (a *App) apiDateVisibilityImpact(c *gin.Context) {
	ev, ok := a.loadManagedEvent(c)
	if !ok {
		return
	}
	value, _, err := parseDateVisibility(json.RawMessage(strconv.Quote(c.Query("visibility"))))
	if err != nil {
		fail(c, errVisibilityNotAllowed)
		return
	}
	var startsLocal *string
	if raw, ok := c.GetQuery("startsLocal"); ok {
		startsLocal = &raw
	}
	var duration *int
	if raw, ok := c.GetQuery("durationMinutes"); ok {
		n, err := strconv.Atoi(raw)
		if err != nil || !events.ValidDuration(n) {
			fail(c, errInvalidDuration)
			return
		}
		duration = &n
	}
	date := c.Param("date")
	if !isCalendarDate(date) {
		jsonError(c, http.StatusNotFound, "event not found")
		return
	}
	var impact visibilityImpactJSON
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		me, err := snapshotViewerIn(tx, apiUser(c))
		if err != nil {
			return err
		}
		if me == nil {
			return errSecuritySession
		}
		var current models.Event
		if err := tx.First(&current, ev.ID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errEventNotFound
			}
			return err
		}
		f, err := eventFactsIn(tx, me, &current)
		if err != nil {
			return err
		}
		if !f.canSee(me) {
			return errEventNotFound
		}
		if !f.canManage(me) {
			return errCannotChangeEvent
		}
		if a.hooks.readerAuthorized != nil {
			a.hooks.readerAuthorized("date visibility preview")
		}
		var occ models.EventOccurrence
		if err := tx.Where("event_id = ? AND original_date = ?", current.ID, date).First(&occ).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errEventNotFound
			}
			return err
		}
		start, end, err := previewDateTimes(&current, &occ, startsLocal, duration)
		if err != nil {
			return err
		}
		now := a.now().UTC()
		// DEC-156: the save would refuse a later end on a kept date, so the
		// preview answers that refusal instead of a question it can't save.
		if end.After(occ.EndsAt) {
			n, err := keptDatesPreviewIn(tx, &occ, now)
			if err != nil {
				return err
			}
			if n > 0 {
				return keptDatesConflict{keptDatesConflictJSON{KeptDates: []keptDateCountJSON{{Date: models.FormatCalendarDate(occ.OriginalDate), KeptCount: n}}}}
			}
		}
		before := policy.EffectiveVisibility(current.Visibility, occ.VisibilityOverride)
		next := policy.EffectiveVisibility(current.Visibility, value)
		var sel *dateLosers
		if policy.MoreOpen(before, next) {
			s, err := previewDateLosersAt(tx, &current, before, next, now, occ.ID, start)
			if err != nil {
				return err
			}
			sel = &s
		}
		b, h := dateImpactIn(me.ID, &current, &occ, before, next, sel)
		impact = b.impact
		impact.Consent, err = a.consentTokenIn(tx, h, b.entries)
		return err
	}, readSnapshot)
	switch {
	case isAPIError(err):
		fail(c, err)
	case err != nil:
		jsonError(c, http.StatusInternalServerError, "could not preview visibility")
	default:
		c.JSON(http.StatusOK, impact)
	}
}

// openedDatesAt is a group's future dates with their own visibility wider
// than next, with their events: each is reviewed on its own, whatever its
// event's opened marker.
func openedDatesAt(tx *gorm.DB, now time.Time, groupID uint, oldVis, next string) ([]models.EventOccurrence, map[uint]*models.Event, error) {
	evs := map[uint]*models.Event{}
	if !policy.MoreOpen(oldVis, next) {
		return nil, evs, nil
	}
	var rows []models.Event
	if err := tx.Where("group_id = ?", groupID).Order("id").Find(&rows).Error; err != nil {
		return nil, nil, err
	}
	ids := []uint{}
	for i := range rows {
		evs[rows[i].ID] = &rows[i]
		ids = append(ids, rows[i].ID)
	}
	if len(ids) == 0 {
		return nil, evs, nil
	}
	var dates []models.EventOccurrence
	if err := futureOverridesAt(tx, now, ids...).Order("id").Find(&dates).Error; err != nil {
		return nil, nil, err
	}
	out := []models.EventOccurrence{}
	for _, date := range dates {
		if policy.MoreOpen(*date.VisibilityOverride, next) {
			out = append(out, date)
		}
	}
	return out, evs, nil
}

func dateDecisionKey(id uint) string { return "date:" + idString(id) }

func (vs visibleSet) canSeeSeries(viewer *models.User, ev *models.Event) bool {
	return vs.canSeeDate(viewer, ev, &models.EventOccurrence{})
}
