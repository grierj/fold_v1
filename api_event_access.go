package main

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"fold/internal/audit"
	"fold/internal/consent"
	"fold/internal/mail"
	"fold/internal/policy"
	"fold/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// groupShownTo returns g when the viewer may see the group, else nil.
// Read a group's name, place or id for event and notice output only through
// its result (TestGroupDetailsGoThroughGroupShownTo enforces this).
func (a *App) groupShownTo(me *models.User, g *models.Group, rel policy.Rel) *models.Group {
	if g == nil || !policy.CanSeeGroup(asPerson(me), asGroup(g), rel) {
		return nil
	}
	return g
}

func (a *App) presentEvent(ev *models.Event, me *models.User, f eventFacts) (eventJSON, error) {
	return a.presentEventIn(a.DB, ev, me, f, &siteNameOnce{a: a})
}

// presentEventIn is presentEvent with the church-wide hosts read on db and the
// site name from site, so the event page reads them in its snapshot.
func (a *App) presentEventIn(db *gorm.DB, ev *models.Event, me *models.User, f eventFacts, site *siteNameOnce) (eventJSON, error) {
	j := toEventJSON(ev)
	if f.canManage(me) {
		applyEventVideoReveal(&j, ev, false, true)
	}
	l := labelGroup(a.groupShownTo(me, f.group, f.rel), true, site)
	j.GroupID, j.GroupName, j.GroupVisibility, j.HostedBy = l.ID, l.Name, l.Visibility, l.HostedBy
	if policy.IsAdmin(asPerson(me)) && ev.GroupID == nil {
		hosts, err := eventHostsIn(db, ev.ID)
		if err != nil {
			return j, err
		}
		j.Hosts = hosts
	}
	return j, nil
}

func (a *App) respondEvent(c *gin.Context, status int, ev *models.Event, me *models.User, f eventFacts) {
	j, err := a.presentEvent(ev, me, f)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(status, eventSeriesResponseJSON{Event: j})
}

func (a *App) eventHosts(eventID uint) ([]hostJSON, error) {
	return eventHostsIn(a.DB, eventID)
}

func eventHostsIn(db *gorm.DB, eventID uint) ([]hostJSON, error) {
	var rows []models.EventHost
	if err := db.Preload("User").Where("event_id = ?", eventID).Order("id").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]hostJSON, 0, len(rows))
	for _, h := range rows {
		if h.User.ID == 0 {
			continue
		}
		out = append(out, hostJSON{UserID: h.UserID, Name: h.User.PublicName(), Email: h.User.Email})
	}
	return out, nil
}

func (a *App) respondHosts(c *gin.Context, eventID uint) {
	hosts, err := a.eventHosts(eventID)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, eventHostsResponseJSON{Hosts: hosts})
}

func (a *App) eventPermissions(me *models.User, f eventFacts) (manage, setVis bool) {
	p := asPerson(me)
	manage = policy.CanManageEvent(p, f.view, f.rel)
	setVis = policy.CanSetEventVisibility(p, f.view, f.rel, models.VisibilityPublic) ||
		policy.CanSetEventVisibility(p, f.view, f.rel, models.VisibilityPrivate)
	return
}

// upcomingWindow is the original-date range the "upcoming" views list: from
// yesterday, so a date whose original day (UTC) has passed but that starts
// later today in the viewer's zone is not lost, through 90 days out. Those
// views always pair it with a start-after-now filter, which drops what already
// began. parseDateWindow's default begins today instead because the explicit
// range endpoints it serves have no such filter and must not list past dates.
func upcomingWindow(now time.Time) (from, to string) {
	return now.Add(-24 * time.Hour).Format("2006-01-02"), now.AddDate(0, 0, 90).Format("2006-01-02")
}

// eventPageLists is whether the event and group pages list this date: the Go
// form of occurrencesForEventsErr's filter as those pages call it (scheduled,
// original date inside upcomingWindow(now), not yet started). A manager's
// calendar row links any other date to its date page (DEC-157).
// TestManagerCalendarLinkMatchesTheEventPageList keeps the two equal.
func eventPageLists(o *models.EventOccurrence, now time.Time) bool {
	from, to := upcomingWindow(now)
	day := models.FormatCalendarDate(o.OriginalDate)
	return o.Status == models.OccurrenceScheduled && day >= from && day <= to && !o.StartsAt.Before(now)
}

// loadEventFor loads the event named by :id with the caller's facts. The error
// is errEventNotFound when it is missing or the caller cannot see it (never a
// 403 for what they may not know exists), errCannotChangeEvent from
// loadManagedEventFor when they can see but not manage it, else a failure.
func (a *App) loadEventFor(c *gin.Context) (*models.Event, eventFacts, error) {
	id, _ := strconv.Atoi(c.Param("id"))
	var ev models.Event
	if err := a.DB.First(&ev, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, eventFacts{}, errEventNotFound
		}
		return nil, eventFacts{}, err
	}
	f, err := eventFactsIn(a.DB, apiUser(c), &ev)
	if err != nil {
		return nil, eventFacts{}, err
	}
	if !f.canSee(apiUser(c)) {
		return nil, eventFacts{}, errEventNotFound
	}
	return &ev, f, nil
}

func (a *App) loadManagedEventFor(c *gin.Context) (*models.Event, eventFacts, error) {
	ev, f, err := a.loadEventFor(c)
	if err == nil && !f.canManage(apiUser(c)) {
		return nil, eventFacts{}, errCannotChangeEvent
	}
	return ev, f, err
}

// loadVisibleEvent answers 404 when the event is missing or the caller cannot
// see it. Managing it is a separate 403.
func (a *App) loadVisibleEvent(c *gin.Context) (*models.Event, eventFacts, bool) {
	ev, f, err := a.loadEventFor(c)
	if err != nil {
		fail(c, err)
	}
	return ev, f, err == nil
}

// openEventIDs is church-wide events the viewer can see, plus group events
// they can see whose group they cannot. Admins already see every group, so
// an opened group event stays on that group's page for them.
func (a *App) openEventIDs(u *models.User) ([]uint, error) {
	now := time.Now().UTC()
	from, to := upcomingWindow(now)
	vs, err := a.dateCandidates(u, a.DB.Where(
		"id IN (SELECT event_id FROM event_occurrences WHERE original_date >= ? AND original_date <= ?)", from, to))
	if err != nil {
		return nil, err
	}
	p := asPerson(u)
	var out []uint
	for i := range vs.Events {
		ev := &vs.Events[i]
		g, rel := groupAndRel(ev, vs.Groups, vs.Rels)
		if ev.GroupID == nil || !policy.CanSeeGroup(p, asGroup(g), rel) {
			out = append(out, ev.ID)
		}
	}
	return out, nil
}

func (a *App) apiListOpenEvents(c *gin.Context) {
	me := apiUser(c)
	now := time.Now().UTC()
	from, to := upcomingWindow(now)
	ids, err := a.openEventIDs(me)
	var listed []occurrenceJSON
	if err == nil {
		listed, err = a.occurrencesForEventsErr(ids, from, to, &now, me)
	}
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "could not load events")
		return
	}
	c.JSON(http.StatusOK, openEventsResponseJSON{Events: listed})
}

// apiGetEvent is the event page: the event, its upcoming dates and the
// viewer's permissions, with the group label, the site name and an admin's
// church-wide hosts, all read in one read-only Repeatable Read snapshot on one
// clock with the viewer re-read in it (snapshotViewerIn), so a demotion, a
// removed host or an erasure committed during the read never pairs old
// authority with newer rows. A participating request is acknowledged for that
// viewer once the snapshot has ended (decidingSnapshot), on the page and on
// its 404 once that rollback was confirmed.
func (a *App) apiGetEvent(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	var out gin.H
	var judged sessionJudgment
	err := decidingSnapshot(a.DB.WithContext(c.Request.Context()), func(tx *gorm.DB) error {
		if a.hooks.readerBegin != nil {
			a.hooks.readerBegin("event detail")
		}
		now := a.now().UTC()
		me, err := snapshotViewerIn(tx, apiUser(c))
		if err != nil {
			return err
		}
		judged.judge(me)
		var ev models.Event
		if err := tx.First(&ev, id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errEventNotFound
			}
			return err
		}
		f, err := eventFactsIn(tx, me, &ev)
		if err != nil {
			return err
		}
		if !f.canSee(me) {
			return errEventNotFound
		}
		if a.hooks.readerAuthorized != nil {
			a.hooks.readerAuthorized("event detail")
		}
		manage, setVis := a.eventPermissions(me, f)
		from, to := upcomingWindow(now)
		listed, _, err := a.listDatesIn(dateListRead{db: tx, now: now, strict: true}, []uint{ev.ID}, from, to, &now, me, 0, nil)
		if err != nil {
			return err
		}
		event, err := a.presentEventIn(tx, &ev, me, f, &siteNameOnce{a: a, db: tx})
		if err != nil {
			return err
		}
		if f.canManage(me) {
			event.Revision = a.eventRevision(&ev)
		}
		out = gin.H{
			"event":            event,
			"occurrences":      listed,
			"canManage":        manage,
			"canSetVisibility": setVis,
		}
		if a.hooks.readerEnd != nil {
			a.hooks.readerEnd("event detail", tx)
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
	c.JSON(http.StatusOK, out)
}

func (a *App) hostCandidate(actor *models.User, email string) (*models.User, error) {
	email = mail.NormalizeAddress(email)
	if email == "" {
		return nil, errEmailRequired
	}
	var u models.User
	var found *models.User
	if err := a.DB.Where("email = ?", email).First(&u).Error; err == nil {
		found = &u
	}
	// The unlocked preflight for a fast answer; the transaction rechecks the
	// same rule under the user's row lock (lockHostUsers).
	if r := hostEligibility(actor, found); r != nil {
		return nil, r
	}
	return found, nil
}

func (a *App) apiCreateChurchEvent(c *gin.Context) {
	var body struct {
		eventBody
		Visibility *string  `json:"visibility"`
		Hosts      []string `json:"hosts"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		fail(c, errStartRequired)
		return
	}
	if body.Title == nil || strings.TrimSpace(*body.Title) == "" {
		fail(c, errTitleRequired)
		return
	}
	creator := apiUser(c)
	vis := models.VisibilityPublic
	if body.Visibility != nil && strings.TrimSpace(*body.Visibility) != "" {
		vis = strings.TrimSpace(*body.Visibility)
	}
	// The creator hosts the event they make (a church-wide event has no group
	// to lead it), so policy decides on their host facts.
	if !policy.CanSetEventVisibility(asPerson(creator), policy.EventView{ChurchWide: true, Host: true}, policy.Rel{}, vis) {
		if vis == models.VisibilityInviteOnly {
			fail(c, errChurchEventInviteOnly)
		} else {
			fail(c, errChurchEventVisibility)
		}
		return
	}
	ev, err := a.draftEvent(body.eventBody, models.Event{Visibility: vis, CreatedBy: creator.ID})
	if err != nil {
		fail(c, err)
		return
	}
	hostIDs := []uint{creator.ID}
	seen := map[uint]bool{creator.ID: true}
	for _, email := range body.Hosts {
		if strings.TrimSpace(email) == "" {
			continue
		}
		u, err := a.hostCandidate(creator, email)
		if err != nil {
			fail(c, err)
			return
		}
		if seen[u.ID] {
			continue
		}
		seen[u.ID] = true
		hostIDs = append(hostIDs, u.ID)
	}
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		// The creator and every listed host are locked FOR SHARE, in ascending
		// id, before anything is written, and the rules are checked again on
		// the locked rows: a disabling, demotion or deletion that committed
		// since the preflight cannot be followed by a host row for it.
		locked, err := lockHostUsers(tx, hostIDs)
		if err != nil {
			return err
		}
		if err := lockedHostActor(locked, creator); err != nil {
			return err
		}
		for _, id := range hostIDs[1:] {
			if r := hostEligibility(locked[creator.ID], locked[id]); r != nil {
				return r
			}
		}
		meta := map[string]any{"churchWide": true, "visibility": ev.Visibility, "hostIds": hostIDs}
		// Church-wide events are created by admins, whose bookings never wait.
		return a.createEventIn(tx, &ev, creator, c.ClientIP(), body.RoomID, meta, func() error {
			for _, id := range hostIDs {
				if err := tx.Create(&models.EventHost{EventID: ev.ID, UserID: id, AddedBy: creator.ID}).Error; err != nil {
					return err
				}
			}
			return nil
		})
	})
	if err != nil {
		fail(c, err)
		return
	}
	a.respondEvent(c, http.StatusCreated, &ev, creator, eventFacts{view: viewOf(&ev, true)})
}

// leaderOpens reports whether setting the event to vis makes it wider than its
// group, which marks it leader-opened (DEC-48).
func leaderOpens(f eventFacts, vis string) bool {
	return f.group != nil && policy.MoreOpen(vis, f.group.Visibility)
}

// apiSetEventVisibility sets an event's own visibility (DEC-48). On a group
// event, a value wider than the group marks it leader-opened and is audited
// as an override with a notice to the other leaders and admins (DEC-17,
// DEC-28); anything else clears the marker and is audited as a change.
func (a *App) apiSetEventVisibility(c *gin.Context) {
	ev, f, ok := a.loadVisibleEvent(c)
	if !ok {
		return
	}
	var body struct {
		Visibility string `json:"visibility"`
		Reason     string `json:"reason"`
		// Consent is the token of the impact the person reviewed (the preview
		// or a 409's). A change that cancels anyone, and every reset, needs
		// the token of the impact as the write finds it (DEC-158).
		Consent        *string `json:"consent"`
		ResetOverrides bool    `json:"resetOverrides"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		jsonError(c, http.StatusBadRequest, "invalid body")
		return
	}
	vis := strings.TrimSpace(body.Visibility)
	reason := strings.TrimSpace(body.Reason)
	if len(reason) > 500 {
		fail(c, errReasonTooLong)
		return
	}
	me := apiUser(c)
	if !policy.CanManageEvent(asPerson(me), f.view, f.rel) {
		fail(c, errCannotChangeEvent)
		return
	}
	if !policy.CanSetEventVisibility(asPerson(me), f.view, f.rel, vis) {
		fail(c, errVisibilityNotAllowed)
		return
	}
	var impact visibilityImpactJSON
	err := a.contactMutation(contactChange{EventID: ev.ID}, func(tx *gorm.DB) error {
		actor, locked, fresh, err := lockManagedEventIn(tx, me, ev.ID)
		if err != nil {
			return err
		}
		me, f = actor, fresh
		if !policy.CanSetEventVisibility(asPerson(me), f.view, f.rel, vis) {
			return errVisibilityNotAllowed
		}
		opened := leaderOpens(f, vis)
		if locked.Visibility == vis && locked.LeaderOpened == opened && !body.ResetOverrides {
			*ev = *locked
			return nil
		}
		oldVis, oldOpened := locked.Visibility, locked.LeaderOpened
		// Every selection first, on one clock, under the locks; then the
		// consent check over exactly them; only then any write.
		now := a.now().UTC()
		b, h, dates, err := eventImpactIn(tx, lockingSelector(tx, now), now, me.ID, locked, vis, body.ResetOverrides, body.ResetOverrides)
		if err != nil {
			return err
		}
		impact = b.impact
		plan, token, err := a.checkConsentIn(tx, body.Consent, h, b.entries)
		if errors.Is(err, errConsentMismatch) {
			impact.Consent = token
			return errNeedDecision
		}
		if err != nil {
			return err
		}
		action := audit.VisibilityChanged
		if opened {
			action = audit.VisibilityOverride
		}
		if err := tx.Model(locked).Updates(map[string]any{"visibility": vis, "leader_opened": opened}).Error; err != nil {
			return err
		}
		locked.Visibility, locked.LeaderOpened = vis, opened
		// More private: the same cancel-and-notify as a group tightening (DEC-48).
		if series := eventRowKey(consent.ListTightened, locked); plan.Has(series) {
			cs, err := plan.Take(series)
			if err != nil {
				return err
			}
			if err := a.cancelSelectedLosers(tx, cs, me, c.ClientIP(), locked, vis, seriesDates); err != nil {
				return err
			}
		}
		// Exactly the dates the reviewed impact listed (empty unless the
		// check passed).
		byID := map[uint64]*models.EventOccurrence{}
		for i := range dates {
			byID[uint64(dates[i].ID)] = &dates[i]
		}
		for _, id := range plan.ResetDates() {
			d := byID[id]
			if d == nil {
				return fmt.Errorf("reset date %d is not among the locked dates", id)
			}
			var cd consent.Checked[dateLosers]
			if k := dateRowKey(consent.ListTightened, locked, d); plan.Has(k) {
				if cd, err = plan.Take(k); err != nil {
					return err
				}
			}
			if err := a.changeDateVisibilitySelected(tx, me, c.ClientIP(), locked, d, f.group, nil, "series_reset", false, cd); err != nil {
				return err
			}
		}
		*ev = *locked
		meta := map[string]any{
			"oldVisibility":   oldVis,
			"newVisibility":   vis,
			"oldLeaderOpened": oldOpened,
			"leaderOpened":    opened,
		}
		if f.group != nil {
			meta["groupVisibility"] = f.group.Visibility
		}
		if reason != "" {
			meta["reason"] = reason
		}
		if err := audit.Write(tx, audit.Entry{
			Actor: &me.ID, Action: action, TargetType: "event", TargetID: ev.ID,
			Meta: meta, IP: c.ClientIP(),
		}); err != nil {
			return err
		}
		if action == audit.VisibilityOverride && f.group != nil {
			if err := notifyVisibilityOverride(tx, me.ID, ev, f.group, vis, reason); err != nil {
				return err
			}
		}
		return plan.Done()
	})
	if errors.Is(err, errNeedDecision) {
		c.JSON(http.StatusConflict, gin.H{
			"error":         "Review this change before saving",
			"needsDecision": true,
			"impact":        impact,
		})
		return
	}
	if err != nil {
		answerEventWrite(c, err, "could not update visibility")
		return
	}
	f.view.Visibility = vis
	a.respondEvent(c, http.StatusOK, ev, me, f)
}

func (a *App) loadChurchEvent(c *gin.Context) (*models.Event, bool) {
	id, _ := strconv.Atoi(c.Param("id"))
	var ev models.Event
	if err := a.DB.First(&ev, id).Error; err != nil {
		jsonError(c, http.StatusNotFound, "event not found")
		return nil, false
	}
	if ev.GroupID != nil {
		jsonError(c, http.StatusBadRequest, "hosts are only assigned on church-wide events")
		return nil, false
	}
	return &ev, true
}

func (a *App) apiAddEventHost(c *gin.Context) {
	ev, ok := a.loadChurchEvent(c)
	if !ok {
		return
	}
	var body struct {
		Email string `json:"email"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || strings.TrimSpace(body.Email) == "" {
		fail(c, errEmailRequired)
		return
	}
	target, err := a.hostCandidate(apiUser(c), body.Email)
	if err != nil {
		fail(c, err)
		return
	}
	me := apiUser(c)
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		locked, err := lockHostUsers(tx, []uint{me.ID, target.ID})
		if err != nil {
			return err
		}
		if err := lockedHostActor(locked, me); err != nil {
			return err
		}
		if r := hostEligibility(locked[me.ID], locked[target.ID]); r != nil {
			return r
		}
		// FOR KEY SHARE is what the foreign key insert takes anyway, taken
		// here so a concurrent event deletion (FOR UPDATE) is found first.
		var event models.Event
		if err := tx.Clauses(clause.Locking{Strength: "KEY SHARE"}).Where("id = ? AND group_id IS NULL", ev.ID).First(&event).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errEventNotFound
			}
			return err
		}
		// Two concurrent adds of one person give one row and one audit.
		res := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "event_id"}, {Name: "user_id"}}, DoNothing: true}).
			Create(&models.EventHost{EventID: ev.ID, UserID: target.ID, AddedBy: me.ID})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 1 {
			if err := audit.Write(tx, audit.Entry{Actor: &me.ID, Action: audit.EventHostAdded, TargetType: "event", TargetID: ev.ID,
				Meta: map[string]any{"userId": target.ID}, IP: c.ClientIP()}); err != nil {
				return err
			}
		}
		// The event now has a live host, so any tombstone row can go. Doing
		// it here also repairs a legacy {live, tombstone} event when the live
		// host is added again.
		return removeDeletedHostRows(tx, me.ID, []uint{ev.ID}, c.ClientIP())
	})
	if err != nil {
		fail(c, err)
		return
	}
	a.respondHosts(c, ev.ID)
}

var errEventNotFound = apiErr(http.StatusNotFound, "", "event not found")

func (a *App) apiRemoveEventHost(c *gin.Context) {
	ev, ok := a.loadChurchEvent(c)
	if !ok {
		return
	}
	uid, _ := strconv.Atoi(c.Param("userId"))
	me := apiUser(c)
	err := a.contactMutation(contactChange{EventID: ev.ID}, func(tx *gorm.DB) error {
		var locked models.Event
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&locked, ev.ID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errEventNotFound
			}
			return err
		}
		// A plain read under the gate: the actor's own ungated registration
		// change could hold their row while waiting on a row this transaction
		// already locked, so a share lock here could cycle with it.
		if _, err := lockAdminIn(tx, me, actorRead); err != nil {
			return err
		}
		var seats []policy.HostSeat
		if err := tx.Raw(`SELECT eh.user_id, (u.deleted_at IS NOT NULL) AS deleted
			FROM event_hosts eh JOIN users u ON u.id = eh.user_id
			WHERE eh.event_id = ? ORDER BY eh.user_id`, ev.ID).Scan(&seats).Error; err != nil {
			return err
		}
		found := false
		for _, h := range seats {
			if h.UserID == uint(uid) {
				found = true
			}
		}
		if !found {
			return errNotAHost
		}
		if !policy.EventKeepsHost(seats, uint(uid)) {
			return errNeedsHost
		}
		if err := tx.Where("event_id = ? AND user_id = ?", ev.ID, uid).Delete(&models.EventHost{}).Error; err != nil {
			return err
		}
		return audit.Write(tx, audit.Entry{Actor: &me.ID, Action: audit.EventHostRemoved, TargetType: "event", TargetID: ev.ID,
			Meta: map[string]any{"userId": uid}, IP: c.ClientIP()})
	})
	if err != nil {
		fail(c, err)
		return
	}
	a.respondHosts(c, ev.ID)
}

var errNotAHost = apiErr(http.StatusNotFound, "", "that person isn't a host")

// visibleSet is the events one viewer may see out of a query, with the
// lookups callers need afterwards. Events keeps the query's order.
type visibleSet struct {
	Events []models.Event
	IDs    []uint
	ByID   map[uint]*models.Event
	factSet
}

// dateCandidates loads candidate series facts without granting series rights.
// Every date list must filter each date through visibleSet.canSeeDate.
func (a *App) dateCandidates(viewer *models.User, q *gorm.DB) (visibleSet, error) {
	return a.eventCandidates(viewer, q)
}

// eventCandidates is the shared event loader. It only loads candidates: callers
// must separately authorize each date, never the series. Events whose group is
// missing are skipped, and deleted groups stay hidden.
func (a *App) eventCandidates(viewer *models.User, q *gorm.DB) (visibleSet, error) {
	return loadCandidates(a.DB, viewer, q, false)
}

// dateCandidatesIn is dateCandidates on db, for a caller that holds a
// transaction or a context-bound handle: db and q must come from the same one.
func dateCandidatesIn(db *gorm.DB, viewer *models.User, q *gorm.DB) (visibleSet, error) {
	return loadCandidates(db, viewer, q, false)
}

// deletedEventCandidatesIn is dateCandidatesIn for the admin-only "show deleted
// events" calendar switch (DEC-68): an admin also gets the events of
// soft-deleted groups. Anyone else gets the ordinary candidates.
func deletedEventCandidatesIn(db *gorm.DB, viewer *models.User, q *gorm.DB) (visibleSet, error) {
	return loadCandidates(db, viewer, q, policy.IsAdmin(asPerson(viewer)))
}

// When reading in a transaction, db and q must both come from that transaction.
func loadCandidates(db *gorm.DB, viewer *models.User, q *gorm.DB, withDeleted bool) (visibleSet, error) {
	vs := visibleSet{ByID: map[uint]*models.Event{}}
	var evs []models.Event
	if err := q.Find(&evs).Error; err != nil || len(evs) == 0 {
		vs.factSet = factSet{Groups: map[uint]*models.Group{}, Rels: map[uint]policy.Rel{}, Hosted: map[uint]bool{}}
		return vs, err
	}
	var err error
	if vs.factSet, err = loadFactSet(db, viewer, evs, withDeleted); err != nil {
		return vs, err
	}
	for i := range evs {
		if evs[i].GroupID != nil && vs.Groups[*evs[i].GroupID] == nil {
			continue
		}
		vs.Events = append(vs.Events, evs[i])
	}
	for i := range vs.Events {
		vs.ByID[vs.Events[i].ID] = &vs.Events[i]
		vs.IDs = append(vs.IDs, vs.Events[i].ID)
	}
	return vs, nil
}

type hostJSON struct {
	UserID uint   `json:"userId"`
	Name   string `json:"name"`
	Email  string `json:"email"`
}
