package main

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"time"

	"fold/internal/audit"
	"fold/internal/consent"
	"fold/internal/policy"
	"fold/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Group visibility and its events (DEC-48). An event keeps its own
// visibility. When a group becomes more private, in the same transaction:
//   - events more open than the new value that only inherited the group's
//     visibility are tightened to match, one audit row each;
//   - leader-opened events are not changed automatically: the leader
//     decides per event to keep it open (a logged override) or tighten it;
//   - registrants of a tightened event who can no longer see it are
//     cancelled on upcoming dates and get a notice (M1.13's "made
//     stricter" rule, with the PR #41 privacy rules).
// Making a group more open never changes its events.

// Decisions for leader-opened events when their group becomes more private.
const (
	openedKeep    = "keep"
	openedTighten = "tighten"
)

type impactEventJSON struct {
	ID           uint   `json:"id"`
	Date         string `json:"date,omitempty"`
	OccurrenceID uint   `json:"occurrenceId,omitempty"`
	DecisionKey  string `json:"decisionKey,omitempty"`
	Title        string `json:"title"`
	Visibility   string `json:"visibility"`
	// Registrants: people registered for an upcoming date who could no
	// longer see the event if it is tightened.
	Registrants int64 `json:"registrants"`
}

// visibilityImpactJSON is what a visibility change would do. Both lists are
// empty unless the change is more private. CurrentVisibility is the scope's
// own visibility before the change; ResetDates (an event reset only) are the
// dates the reset changes; Consent is the token that confirms exactly this
// impact (DEC-158).
type visibilityImpactJSON struct {
	FutureOverrides   int64                 `json:"futureOverrides,omitempty"`
	Visibility        string                `json:"visibility"`
	CurrentVisibility string                `json:"currentVisibility"`
	Tightened         []impactEventJSON     `json:"tightened"`
	LeaderOpened      []impactEventJSON     `json:"leaderOpened"`
	ResetDates        []impactResetDateJSON `json:"resetDates,omitempty"`
	Consent           string                `json:"consent,omitempty"`
}

func (v visibilityImpactJSON) empty() bool {
	return len(v.Tightened) == 0 && len(v.LeaderOpened) == 0
}

var errNeedDecision = errors.New("leader-opened events need a decision")

// affectedEvents lists g's events more open than newVis, split into the
// ones that inherited the group's value and the leader-opened ones. Empty
// unless newVis is more private than oldVis.
func affectedEvents(tx *gorm.DB, groupID uint, oldVis, newVis string) (auto, opened []models.Event, err error) {
	if !policy.MoreOpen(oldVis, newVis) {
		return nil, nil, nil
	}
	var evs []models.Event
	if err := tx.Where("group_id = ?", groupID).Order("id").Find(&evs).Error; err != nil {
		return nil, nil, err
	}
	for _, ev := range evs {
		if !policy.MoreOpen(ev.Visibility, newVis) {
			continue
		}
		if ev.LeaderOpened {
			opened = append(opened, ev)
		} else {
			auto = append(auto, ev)
		}
	}
	return auto, opened, nil
}

// seriesDates is the occurrence id that names every upcoming date of an event
// that follows its series (no override of its own) rather than one date.
const seriesDates = uint(0)

// losersForDate selects registrations on upcoming dates of ev whose people
// could not see ev at newVis: occurrenceID's date, or the series dates. It is
// policy.CanSeeEvent as SQL (TestLosersMatchPolicy keeps the two in step).
// Only invite_only shuts signed-in people out, and private shuts out guests
// (M1.4); admins (active, as asPerson sees them) see every event. Waitlisted
// rows are included: a waitlisted person who loses access must not be
// promoted into the event later.
func losersForDate(tx *gorm.DB, ev *models.Event, newVis string, now time.Time, occurrenceID uint) *gorm.DB {
	return losersWhere(tx, ev, newVis, now, occurrenceID, clause.Expr{SQL: "event_occurrences.starts_at > ?", Vars: []any{now}})
}

// losersForDateAt is losersForDate for one date judged at startsAt, the
// date's start as it would be saved: the date is upcoming when startsAt is
// after now, decided here in place of the stored starts_at predicate. A
// preview passes the prospective start; once that start is saved the two
// forms agree (TestLosersAtMatchTheStoredForm).
func losersForDateAt(tx *gorm.DB, ev *models.Event, newVis string, now time.Time, occurrenceID uint, startsAt time.Time) *gorm.DB {
	upcoming := clause.Expr{SQL: "FALSE"}
	if startsAt.After(now) {
		upcoming = clause.Expr{SQL: "TRUE"}
	}
	return losersWhere(tx, ev, newVis, now, occurrenceID, upcoming)
}

// losersWhere is losersForDate with the "the date is upcoming" condition
// given by the caller.
func losersWhere(tx *gorm.DB, ev *models.Event, newVis string, now time.Time, occurrenceID uint, upcoming clause.Expr) *gorm.DB {
	q := tx.Table("registrations").
		Joins("JOIN event_occurrences ON event_occurrences.id = registrations.occurrence_id").
		Joins("JOIN users ON users.id = COALESCE(registrations.user_id,registrations.registered_by)").
		Where("event_occurrences.event_id = ?", ev.ID).Where(upcoming).
		Where("registrations.status IN ?", models.CurrentRegStatuses)
	if occurrenceID != seriesDates {
		q = q.Where("event_occurrences.id = ?", occurrenceID)
	} else {
		q = q.Where("event_occurrences.visibility_override IS NULL")
	}
	// Only a current explicit invitation preserves its own registration. Parent
	// event visibility never widens, and household/ordinary siblings still lose.
	var grants []models.Registration
	if err := tx.Where(registrationAccessLiveSQL+" AND registrations.id IN (?)",
		q.Session(&gorm.Session{}).Select("registrations.id")).Order("registrations.id").Find(&grants).Error; err != nil {
		q.AddError(err)
		return q
	}
	reads, err := occurrenceInvitationReadsIn(tx, grants, now)
	if err != nil {
		q.AddError(err)
		return q
	}
	kept := []uint{}
	for _, grant := range grants {
		if reads[grant.ID] != nil {
			kept = append(kept, grant.ID)
		}
	}
	if len(kept) > 0 {
		q = q.Where("registrations.id NOT IN ?", kept)
	}

	switch newVis {
	case models.VisibilityPrivate:
		// Signed-in people keep their registrations at private. Guests
		// (M1.4) can no longer see the event at all, so theirs go.
		return q.Where("users.status = ?", models.UserGuest)
	case models.VisibilityInviteOnly:
		q = q.Where("NOT (users.role = ? AND users.status = ?)", models.RoleAdmin, models.UserActive)
		if ev.GroupID == nil {
			return q // a church-wide event has no members: only admins see it
		}
		return q.Where("NOT EXISTS (SELECT 1 FROM memberships WHERE memberships.user_id = users.id AND memberships.group_id = ? AND memberships.status = ?)", *ev.GroupID, models.MembershipActive)
	default:
		return q.Where("1 = 0")
	}
}

// groupPreviewRefused: the viewer may not see the group (404) or may not
// manage it (403), judged in the preview's snapshot.
var (
	errGroupPreviewHidden    = errors.New("group not found")
	errGroupPreviewForbidden = errors.New("group preview forbidden")
)

// apiGroupVisibilityImpact previews a visibility change so the SPA can show
// the leader which events tighten and ask about leader-opened ones before
// saving. GET /groups/:id/visibility-impact?visibility=... The viewer, their
// authority, the rows and the consent token are read in one snapshot, on one
// clock, with the selections a write would cancel (DEC-158).
func (a *App) apiGroupVisibilityImpact(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	g, _, ok := a.loadManagedGroupID(c, uint(id))
	if !ok {
		return
	}
	vis, err := normalizeVisibility(c.Query("visibility"))
	if err != nil {
		fail(c, err)
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
		var group models.Group
		if err := tx.First(&group, g.ID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errGroupPreviewHidden
			}
			return err
		}
		rel, err := groupRel(tx, me.ID, group.ID)
		if err != nil {
			return err
		}
		if !policy.CanSeeGroup(asPerson(me), asGroup(&group), rel) {
			return errGroupPreviewHidden
		}
		if !policy.CanManageGroup(asPerson(me), rel) {
			return errGroupPreviewForbidden
		}
		now := a.now().UTC()
		b, h, _, err := groupImpactIn(tx, previewSelector(tx, now), now, me.ID, group.ID, group.Visibility, vis)
		if err != nil {
			return err
		}
		impact = b.impact
		impact.Consent, err = a.consentTokenIn(tx, h, b.entries)
		return err
	}, readSnapshot)
	switch {
	case errors.Is(err, errGroupPreviewHidden):
		jsonError(c, http.StatusNotFound, "group not found")
	case errors.Is(err, errGroupPreviewForbidden):
		jsonError(c, http.StatusForbidden, "forbidden")
	case isAPIError(err):
		fail(c, err)
	case err != nil:
		jsonError(c, http.StatusInternalServerError, "could not check the group's events")
	default:
		c.JSON(http.StatusOK, impact)
	}
}

func eventRowKey(list uint8, ev *models.Event) consent.Key {
	return consent.Key{List: list, Shape: consent.ShapeEvent, Event: uint64(ev.ID)}
}

func dateRowKey(list uint8, ev *models.Event, occ *models.EventOccurrence) consent.Key {
	return consent.Key{List: list, Shape: consent.ShapeDate, Event: uint64(ev.ID), Occurrence: uint64(occ.ID)}
}

// applyGroupTightening runs inside the group update transaction, after the
// group row is locked and before it is saved. oldVis is the stored value.
// decisions maps event id to keep or tighten; a leader-opened event without
// one returns errNeedDecision and impact, and nothing is written. Every row is
// selected under its locks on one clock before anything is written, and the
// change goes on only with the consent of that exact impact when it cancels
// anyone (checkConsentIn); a stale consent also answers errNeedDecision, with
// the fresh impact and token.
func (a *App) applyGroupTightening(tx *gorm.DB, actor *models.User, ip string, g *models.Group, oldVis string, decisions map[string]string, given *string) (visibilityImpactJSON, error) {
	newVis := g.Visibility
	if !policy.MoreOpen(oldVis, newVis) {
		return newImpactBuilder(newVis, oldVis).impact, nil
	}
	now := a.now().UTC()
	if err := lockGroupDates(tx, now, g.ID, oldVis, newVis); err != nil {
		return visibilityImpactJSON{}, err
	}
	b, h, rows, err := groupImpactIn(tx, lockingSelector(tx, now), now, actor.ID, g.ID, oldVis, newVis)
	if err != nil {
		return visibilityImpactJSON{}, err
	}
	impact := b.impact
	if impact.empty() {
		return impact, nil
	}
	for _, row := range impact.LeaderOpened {
		key := row.DecisionKey
		if key == "" {
			key = eventDecisionKey(row.ID)
		}
		if d := decisions[key]; d != openedKeep && d != openedTighten {
			if impact.Consent, err = a.consentTokenIn(tx, h, b.entries); err != nil {
				return impact, err
			}
			return impact, errNeedDecision
		}
	}
	for i := range rows.opened {
		ev := &rows.opened[i]
		b.setCancels(eventRowKey(consent.ListLeaderOpened, ev), decisions[eventDecisionKey(ev.ID)] == openedTighten)
	}
	for i := range rows.dates {
		d := &rows.dates[i]
		b.setCancels(dateRowKey(consent.ListLeaderOpened, rows.evs[d.EventID], d), decisions[dateDecisionKey(d.ID)] != openedKeep)
	}
	plan, token, err := a.checkConsentIn(tx, given, h, b.entries)
	if errors.Is(err, errConsentMismatch) {
		impact.Consent = token
		return impact, errNeedDecision
	}
	if err != nil {
		return impact, err
	}
	tighten := func(ev *models.Event, list uint8, reason string) error {
		if h := a.hooks.consentSkipTake; h != nil && h(eventRowKey(list, ev)) {
			return nil
		}
		c, err := plan.Take(eventRowKey(list, ev))
		if err != nil {
			return err
		}
		old, oldOpened := ev.Visibility, ev.LeaderOpened
		if err := tx.Model(ev).Updates(map[string]any{"visibility": newVis, "leader_opened": false}).Error; err != nil {
			return err
		}
		if err := a.cancelSelectedLosers(tx, c, actor, ip, ev, newVis, seriesDates); err != nil {
			return err
		}
		return audit.Write(tx, audit.Entry{Actor: &actor.ID, Action: audit.VisibilityChanged, TargetType: "event", TargetID: ev.ID,
			Meta: map[string]any{"oldVisibility": old, "newVisibility": newVis, "groupVisibility": newVis,
				"oldLeaderOpened": oldOpened, "leaderOpened": false, "reason": reason},
			IP: ip})
	}
	for i := range rows.auto {
		if err := tighten(&rows.auto[i], consent.ListTightened, "group_more_private"); err != nil {
			return impact, err
		}
	}
	for i := range rows.opened {
		ev := &rows.opened[i]
		if decisions[eventDecisionKey(ev.ID)] == openedTighten {
			if err := tighten(ev, consent.ListLeaderOpened, "group_more_private_leader_chose_tighten"); err != nil {
				return impact, err
			}
			continue
		}
		// Kept open: a logged override, with the usual notice (DEC-17, DEC-28).
		reason := keptOpenReason(newVis)
		if err := audit.Write(tx, audit.Entry{Actor: &actor.ID, Action: audit.VisibilityOverride, TargetType: "event", TargetID: ev.ID,
			Meta: map[string]any{"oldVisibility": ev.Visibility, "newVisibility": ev.Visibility, "groupVisibility": newVis,
				"oldLeaderOpened": true, "leaderOpened": true, "reason": reason},
			IP: ip}); err != nil {
			return impact, err
		}
		if err := notifyVisibilityOverride(tx, actor.ID, ev, g, ev.Visibility, reason); err != nil {
			return impact, err
		}
	}
	for i := range rows.dates {
		date := &rows.dates[i]
		ev := rows.evs[date.EventID]
		if decisions[dateDecisionKey(date.ID)] == openedKeep {
			if err := a.changeDateVisibilitySelected(tx, actor, ip, ev, date, g, date.VisibilityOverride, "group_more_private", true, consent.Checked[dateLosers]{}); err != nil {
				return impact, err
			}
			continue
		}
		c, err := plan.Take(dateRowKey(consent.ListLeaderOpened, ev, date))
		if err != nil {
			return impact, err
		}
		if err := a.changeDateVisibilitySelected(tx, actor, ip, ev, date, g, &newVis, "group_more_private", false, c); err != nil {
			return impact, err
		}
	}
	return impact, plan.Done()
}

// keptOpenReason is the audit and notice reason for a leader-opened event the
// leader chose to keep open when the group became more private.
func keptOpenReason(groupVis string) string {
	return "Kept open when the group became " + visWord(groupVis) + "."
}

// eventDecisionKey is the key of an event's keep-or-tighten decision in the
// group edit's openedEvents, and of dateDecisionKey for a date. The format is
// a contract with the SPA.
func eventDecisionKey(id uint) string { return strconv.FormatUint(uint64(id), 10) }

// lockGroupDates locks, in order, a group's events and their upcoming dates
// that carry an override, for a change to newVis from oldVis. Reads that
// follow in the same transaction see them as locked. Nothing is locked unless
// the change is more private.
func lockGroupDates(tx *gorm.DB, now time.Time, groupID uint, oldVis, newVis string) error {
	if !policy.MoreOpen(oldVis, newVis) {
		return nil
	}
	var ids []uint
	if err := tx.Model(&models.Event{}).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("group_id = ?", groupID).Order("id").Pluck("id", &ids).Error; err != nil || len(ids) == 0 {
		return err
	}
	var dates []uint
	return futureOverridesAt(tx, now, ids...).Clauses(lockForUpdate).
		Order("id").Pluck("id", &dates).Error
}

// lockForUpdate is FOR UPDATE on the statement's table.
var lockForUpdate = clause.Locking{Strength: "UPDATE"}

// dateLosers is one selection, under one clock, of the registrations a
// tightening cancels. A write selects it under its locks, checks consent over
// it, and cancels exactly the copy its checked plan hands back
// (cancelSelectedLosers). Every field is copied by Clone.
type dateLosers struct {
	now    time.Time
	occIDs []uint
	dates  map[uint]*models.EventOccurrence // locked
	rows   []models.Registration            // locked
	// oldVis is the selected dates' effective visibility before the change.
	// Only a loser who could see a date at it is told its title.
	oldVis string
	// prior is a date as it was before the change, for a caller that saved
	// the date before selecting: a title set in the same change as the
	// tightening was never a loser's to see.
	prior map[uint]*models.EventOccurrence
	// callerPromotes: the caller runs the one promotion pass itself, on its
	// own final copy of the date, after its other writes. dates holds each
	// date as locked at the selection, before the caller's visibility write
	// (and, for the guarded edit, its cap write), so promoting from it would
	// judge and notify on the old date.
	callerPromotes bool
}

// dateBefore is date id as it was before the change.
func (sel dateLosers) dateBefore(id uint) *models.EventOccurrence {
	if o := sel.prior[id]; o != nil {
		return o
	}
	return sel.dates[id]
}

// selectDateLosers locks the affected occurrences, then selects the losers,
// locking their rows in id order, so the set cannot change before
// cancelSelectedLosers cancels it.
func selectDateLosers(tx *gorm.DB, ev *models.Event, oldVis, newVis string, now time.Time, occurrenceID uint) (dateLosers, error) {
	// Lock all affected occurrences before re-reading losers, so a concurrent
	// batch either sees the tightened visibility or is included here.
	sel := dateLosers{now: now, oldVis: oldVis, dates: map[uint]*models.EventOccurrence{}}
	q := tx.Model(&models.EventOccurrence{}).Where("event_id = ? AND starts_at > ?", ev.ID, now)
	if occurrenceID != seriesDates {
		q = q.Where("id = ?", occurrenceID)
	}
	if err := q.Order("id").Pluck("id", &sel.occIDs).Error; err != nil {
		return sel, err
	}
	for _, id := range sel.occIDs {
		o, err := lockOccurrence(tx, id)
		if err != nil {
			return sel, err
		}
		sel.dates[id] = o
	}
	err := losersForDate(tx, ev, newVis, now, occurrenceID).Select("registrations.*").Order("registrations.id").
		Clauses(clause.Locking{Strength: "UPDATE", Table: clause.Table{Name: "registrations"}}).Scan(&sel.rows).Error
	return sel, err
}

// cancelSelectedLosers cancels exactly the checked selection's rows, audits
// and notifies their holders, then, unless callerPromotes, promotes on the
// dates that lost someone. It verifies the copy it was handed against the
// checked identities before any write, and reads nothing else of the plan.
func (a *App) cancelSelectedLosers(tx *gorm.DB, c consent.Checked[dateLosers], actor *models.User, ip string, ev *models.Event, newVis string, occurrenceID uint) error {
	sel := c.Selection()
	if h := a.hooks.consentSelection; h != nil {
		h(&sel)
	}
	if err := c.Verify(sel.ConsentIdentities()); err != nil {
		return err
	}
	dates, rows, now := sel.dates, sel.rows, sel.now
	knew, err := a.knewDateTitles(tx, ev, sel)
	if err != nil {
		return err
	}
	titleOf := func(r models.Registration) string {
		if knew[holderDate{registrationHolder(r), r.OccurrenceID}] {
			return effectiveOccurrenceTitle(ev, sel.dateBefore(r.OccurrenceID))
		}
		return ""
	}
	// The nameless notice adds nothing for a holder already told by name
	// that their registrations for dates they can no longer access went.
	named := map[uint]bool{}
	for _, r := range rows {
		named[registrationHolder(r)] = named[registrationHolder(r)] || titleOf(r) != ""
	}
	holders := map[string]bool{}
	affected := map[uint]bool{}
	for _, r := range rows {
		affected[r.OccurrenceID] = true
		// Losing visibility ends the bearer credential as well as the seat:
		// a retained history row must not expose later private date edits.
		// Ordinary self-cancellation keeps its separate idempotent token path.
		cancelled := r
		cancelled.Status = models.RegCancelled
		if err := saveRegistration(tx, policy.RegMorePrivate, r.Status, &cancelled); err != nil {
			return err
		}
		meta := registrationMeta(r)
		meta["newVisibility"] = newVis
		meta["reason"] = "event_more_private"
		if err := audit.Write(tx, audit.Entry{Actor: &actor.ID, Action: audit.RegistrationCancelled, TargetType: "event", TargetID: ev.ID, Meta: meta, IP: ip}); err != nil {
			return err
		}
		holder := registrationHolder(r)
		title := titleOf(r)
		key := fmt.Sprintf("%d:%s", holder, title)
		if holders[key] || (title == "" && named[holder]) {
			continue
		}
		holders[key] = true
		noticeEvent := *ev
		noticeEvent.Title = title
		day := ""
		if occurrenceID != seriesDates {
			day = models.FormatCalendarDate(dates[occurrenceID].OriginalDate)
		}
		if err := notifyRegistrationCancelled(tx, actor.ID, &noticeEvent, holder, day); err != nil {
			return err
		}
	}
	if sel.callerPromotes {
		return nil
	}
	// All losers are cancelled before any survivor competes for the freed seats.
	for _, id := range sel.occIDs {
		if !affected[id] {
			continue
		}
		if err := a.promoteLocked(tx, ev, dates[id], now, &actor.ID); err != nil {
			return err
		}
	}
	return nil
}

// holderDate is one holder of a selected registration and its date.
type holderDate struct{ holder, occurrenceID uint }

// knewDateTitles is, per holder and date of sel's rows, whether the holder
// could see that date just before the change: the same facts (membership,
// group status, account) at sel.oldVis. Nothing but the visibility changes in
// a tightening, so those facts, read now under its locks, are the ones before
// it. A cancelled date showed its title only to someone entitled to its
// tombstone (policy.CanSeeCancelledOccurrence): their own standing row with
// that ordinary access, or a grant valid at the cancellation. Anyone else (a
// retained registrant of an archived group, a holder whose only row on a
// cancelled date is a household person's) is told no title, since it may have
// been set while the date was hidden from them. It reads before any row is
// cancelled.
func (a *App) knewDateTitles(tx *gorm.DB, ev *models.Event, sel dateLosers) (map[holderDate]bool, error) {
	if len(sel.rows) == 0 {
		return nil, nil
	}
	ids := make([]uint, 0, len(sel.rows))
	var cancelled []uint
	for _, r := range sel.rows {
		ids = append(ids, registrationHolder(r))
		if o := sel.dateBefore(r.OccurrenceID); o != nil && o.Status == models.OccurrenceCancelled && !slices.Contains(cancelled, o.ID) {
			cancelled = append(cancelled, o.ID)
		}
	}
	facts, err := loadNoticeFacts(tx, eventGroupIDs(ev), ids)
	if err != nil {
		return nil, err
	}
	before := *ev
	before.Visibility = sel.oldVis
	ordinary := func(holder uint) bool { return facts.canSeeDate(holder, &before, &models.EventOccurrence{}) }
	knew := make(map[holderDate]bool, len(sel.rows))
	for _, r := range sel.rows {
		holder := registrationHolder(r)
		if o := sel.dateBefore(r.OccurrenceID); o == nil || o.Status != models.OccurrenceCancelled {
			knew[holderDate{holder, r.OccurrenceID}] = ordinary(holder)
		}
	}
	if len(cancelled) == 0 {
		return knew, nil
	}
	var own []models.Registration
	if err := tx.Where("user_id IN ? AND occurrence_id IN ? AND household_person_id IS NULL", ids, cancelled).Order("id").Find(&own).Error; err != nil {
		return nil, err
	}
	grants, err := loadOccurrenceInvitationFacts(tx, own, sel.now)
	if err != nil {
		return nil, err
	}
	for _, row := range own {
		holder := *row.UserID
		ok, err := a.cancelledDateEntitledIn(tx, holder, row, grants, ev, sel.dateBefore(row.OccurrenceID), ordinary(holder), sel.now)
		if err != nil {
			return nil, err
		}
		knew[holderDate{holder, row.OccurrenceID}] = ok
	}
	return knew, nil
}

// apiEventVisibilityImpact reads the manager's authority, impact and consent
// token in one snapshot. A participating request gets an acknowledgment
// for that viewer after the snapshot's commit or a decided refusal's rollback.
func (a *App) apiEventVisibilityImpact(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	var impact visibilityImpactJSON
	var judged sessionJudgment
	err := decidingSnapshot(a.DB.WithContext(c.Request.Context()), func(tx *gorm.DB) error {
		if a.hooks.readerBegin != nil {
			a.hooks.readerBegin("event visibility preview")
		}
		t, err := a.managedEventTargetIn(tx, apiUser(c), id, &judged)
		if err != nil {
			return err
		}
		vis, err := normalizeVisibility(c.Query("visibility"))
		if err != nil {
			return err
		}
		if a.hooks.readerAuthorized != nil {
			a.hooks.readerAuthorized("event visibility preview")
		}
		now := a.now().UTC()
		reset := c.Query("resetOverrides") == "1"
		b, h, _, err := eventImpactIn(tx, previewSelector(tx, now), now, t.me.ID, &t.ev, vis, reset, false)
		if err != nil {
			return err
		}
		impact = b.impact
		impact.Consent, err = a.consentTokenIn(tx, h, b.entries)
		if err != nil {
			return err
		}
		if a.hooks.readerEnd != nil {
			a.hooks.readerEnd("event visibility preview", tx)
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
	c.JSON(http.StatusOK, impact)
}
