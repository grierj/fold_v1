package main

import (
	"slices"
	"sort"
	"time"

	"fold/internal/policy"
	"fold/models"
)

// memDates answers dateFactSource from one loadDateFacts statement, applying
// the same filters and soft-delete rules each dbDates query applies. A
// question about rows the statement was not asked to load is errDateFactsGap.
type memDates struct {
	f *dateFacts
	a *App
	// signedOut are people judged as a signed-out visitor whatever their
	// status: a guardian copy's teen who cannot sign in supplies no
	// membership, role, host row or grant of their own.
	signedOut map[uint]bool
}

func (f *dateFacts) source(a *App) memDates { return memDates{f: f, a: a} }

func (m memDates) viewer(u *models.User) *models.User {
	if m.signedOut[u.ID] {
		return nil
	}
	return eventViewer(u)
}

// eventCovered: the holders' registrations on every date of the event, and
// those dates, were loaded.
func (f *dateFacts) eventCovered(id uint) bool {
	if f.asked.events[id] {
		return true
	}
	for _, oid := range f.q.OccurrenceIDs {
		if o := f.occs[oid]; o != nil && o.EventID == id {
			return true
		}
	}
	return false
}

// eventLoaded: the event's own row, if it exists, was loaded.
func (f *dateFacts) eventLoaded(id uint) bool {
	return f.eventCovered(id) || f.occEvents[id]
}

func (f *dateFacts) liveEvent(id uint) *models.Event {
	if ev := f.events[id]; ev != nil && !ev.DeletedAt.Valid {
		return ev
	}
	return nil
}

func (f *dateFacts) liveGroup(id uint) *models.Group {
	if g := f.groups[id]; g != nil && !g.DeletedAt.Valid {
		return g
	}
	return nil
}

func (f *dateFacts) liveUser(id uint) *models.User {
	if u := f.users[id]; u != nil && !u.DeletedAt.Valid {
		return u
	}
	return nil
}

func holderOf(r models.Registration) uint {
	if r.UserID != nil {
		return *r.UserID
	}
	if r.RegisteredBy != nil {
		return *r.RegisteredBy
	}
	return 0
}

func (m memDates) audienceEventIDs(au audienceJSON) ([]uint, error) {
	switch au.Type {
	case policy.AudienceEventSeries:
		return []uint{au.EventID}, nil
	case policy.AudienceEvents:
		return au.EventIDs, nil
	case policy.AudienceOccurrence:
	default:
		return nil, nil
	}
	if !m.f.asked.occurrences[au.OccurrenceID] {
		return nil, errDateFactsGap
	}
	occ := m.f.occs[au.OccurrenceID]
	if occ == nil {
		return nil, errAudienceGone
	}
	return []uint{occ.EventID}, nil
}

func (m memDates) holdings(au audienceJSON, eventIDs, holders []uint, statuses []string, since time.Time) ([]dateHolding, error) {
	for _, h := range holders {
		if !m.f.asked.holders[h] {
			return nil, errDateFactsGap
		}
	}
	if au.Type == policy.AudienceOccurrence {
		if !m.f.asked.occurrences[au.OccurrenceID] {
			return nil, errDateFactsGap
		}
	} else {
		for _, id := range eventIDs {
			if !m.f.eventCovered(id) {
				return nil, errDateFactsGap
			}
		}
	}
	seen := map[dateHolding]bool{}
	var out []dateHolding
	eventSet := idSet(eventIDs)
	for _, uid := range dedupeIDs(holders) {
		for _, r := range m.f.heldRegs[uid] {
			if !slices.Contains(statuses, r.Status) {
				continue
			}
			o := m.f.occs[r.OccurrenceID]
			if o == nil {
				continue
			}
			if au.Type == policy.AudienceOccurrence {
				if o.ID != au.OccurrenceID {
					continue
				}
			} else if !eventSet[o.EventID] || o.Status != models.OccurrenceScheduled || o.StartsAt.Before(since) {
				continue
			}
			h := dateHolding{UID: uid, OccurrenceID: o.ID}
			if !seen[h] {
				seen[h] = true
				out = append(out, h)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UID != out[j].UID {
			return out[i].UID < out[j].UID
		}
		return out[i].OccurrenceID < out[j].OccurrenceID
	})
	return out, nil
}

func (m memDates) occurrences(ids []uint) (map[uint]*models.EventOccurrence, error) {
	out := map[uint]*models.EventOccurrence{}
	for _, id := range ids {
		o := m.f.occs[id]
		if o == nil && !m.f.asked.occurrences[id] {
			return nil, errDateFactsGap
		}
		if o != nil {
			out[id] = o
		}
	}
	return out, nil
}

func (m memDates) liveEvents(ids []uint) ([]models.Event, error) {
	var out []models.Event
	for _, id := range dedupeIDs(ids) {
		if !m.f.eventLoaded(id) {
			return nil, errDateFactsGap
		}
		if ev := m.f.liveEvent(id); ev != nil {
			out = append(out, *ev)
		}
	}
	return out, nil
}

func (m memDates) liveUsers(ids []uint) (map[uint]*models.User, error) {
	out := map[uint]*models.User{}
	for _, id := range ids {
		if !m.f.asked.people[id] {
			return nil, errDateFactsGap
		}
		if u := m.f.liveUser(id); u != nil {
			out[id] = u
		}
	}
	return out, nil
}

// factSetFor is loadFactSet for one viewer (0: none) over evs: their live
// groups, the viewer's membership in each (any status) and the church-wide
// ones they host.
func (m memDates) factSetFor(viewerID uint, evs []models.Event) (factSet, error) {
	s := factSet{Groups: map[uint]*models.Group{}, Rels: map[uint]policy.Rel{}, Hosted: map[uint]bool{}}
	if viewerID != 0 && !m.f.asked.viewers[viewerID] {
		return s, errDateFactsGap
	}
	for i := range evs {
		ev := &evs[i]
		if !m.f.eventLoaded(ev.ID) {
			return s, errDateFactsGap
		}
		if ev.GroupID == nil {
			if viewerID != 0 && m.f.hosts[viewerID][ev.ID] {
				s.Hosted[ev.ID] = true
			}
			continue
		}
		if g := m.f.liveGroup(*ev.GroupID); g != nil {
			s.Groups[g.ID] = g
		}
	}
	if viewerID != 0 {
		groups := map[uint]bool{}
		for i := range evs {
			if evs[i].GroupID != nil {
				groups[*evs[i].GroupID] = true
			}
		}
		for _, mem := range m.f.membersOf[viewerID] {
			if groups[mem.GroupID] {
				s.Rels[mem.GroupID] = membershipRel(*mem)
			}
		}
	}
	return s, nil
}

func (m memDates) factSets(viewerIDs []uint, evs []models.Event) (map[uint]factSet, error) {
	out := map[uint]factSet{}
	for _, id := range viewerIDs {
		if _, ok := out[id]; ok {
			continue
		}
		s, err := m.factSetFor(id, evs)
		if err != nil {
			return nil, err
		}
		out[id] = s
	}
	return out, nil
}

func (m memDates) candidates(viewer *models.User, eventID uint) (visibleSet, error) {
	vs := visibleSet{ByID: map[uint]*models.Event{}}
	evs, err := m.liveEvents([]uint{eventID})
	if err != nil {
		return vs, err
	}
	var viewerID uint
	if viewer != nil {
		viewerID = viewer.ID
	}
	if vs.factSet, err = m.factSetFor(viewerID, evs); err != nil || len(evs) == 0 {
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

// invitations judges grants as occurrenceInvitationReadsIn does, from the
// statement's rows: each grant holder's exact-date actor (occurrenceActorOf:
// live, able to sign in, a teen's latest consent), and the live dates, events
// and groups.
func (m memDates) invitations(grants []models.Registration, now time.Time) (map[uint]*occurrenceRead, error) {
	if len(grants) == 0 {
		return map[uint]*occurrenceRead{}, nil
	}
	v, found := m.f.settings[settingTimezone]
	if !slices.Contains(m.f.q.Settings, settingTimezone) {
		return nil, errDateFactsGap
	}
	// The decision reads no clock of the church's, but a bad zone fails here
	// as occurrenceAccountsIn's read fails on it.
	if _, err := churchZoneOf(v, found); err != nil {
		return nil, err
	}
	facts := &occurrenceInvitationFacts{users: map[uint]*models.User{}, actors: map[uint]policy.OccurrenceActor{},
		occurrences: map[uint]models.EventOccurrence{}, events: map[uint]models.Event{}, groups: map[uint]*models.Group{}}
	for _, g := range grants {
		uid := *g.UserID
		if !m.f.asked.people[uid] {
			return nil, errDateFactsGap
		}
		if u := m.f.liveUser(uid); u != nil {
			facts.users[uid] = u
			c, ok := m.f.consents[uid]
			facts.actors[uid] = occurrenceActorOf(u, ok && c.Status == models.ConsentGranted)
		}
		o := m.f.occs[g.OccurrenceID]
		if o == nil {
			continue
		}
		facts.occurrences[o.ID] = *o
		if ev := m.f.liveEvent(o.EventID); ev != nil {
			facts.events[ev.ID] = *ev
			if ev.GroupID != nil {
				if gr := m.f.liveGroup(*ev.GroupID); gr != nil {
					facts.groups[gr.ID] = gr
				}
			}
		}
	}
	return facts.reads(grants, now), nil
}

// liveGrants are viewer's own registrations with live grant columns on dates
// that pass keep.
func (m memDates) liveGrants(viewerID uint, keep func(*models.EventOccurrence) bool) []models.Registration {
	var out []models.Registration
	for _, r := range m.f.ownRegs[viewerID] {
		if !m.f.live[r.ID] {
			continue
		}
		if o := m.f.occs[r.OccurrenceID]; o != nil && keep(o) {
			out = append(out, *r)
		}
	}
	return out
}

func (m memDates) scoped(viewer *models.User, ids []uint, scheduledOnly bool, now time.Time) (map[uint]*occurrenceRead, error) {
	out := map[uint]*occurrenceRead{}
	if viewer == nil {
		return out, nil
	}
	if !m.f.asked.holders[viewer.ID] {
		return nil, errDateFactsGap
	}
	for _, id := range ids {
		o := m.f.occs[id]
		if o == nil && !m.f.asked.occurrences[id] || o != nil && !m.f.eventCovered(o.EventID) {
			return nil, errDateFactsGap
		}
	}
	want := idSet(ids)
	grants := m.liveGrants(viewer.ID, func(o *models.EventOccurrence) bool {
		return want[o.ID] && (!scheduledOnly || o.Status == models.OccurrenceScheduled)
	})
	reads, err := m.invitations(grants, now)
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

func (m memDates) registeredDate(eventID uint, au audienceJSON, readerID uint, now time.Time) (registeredDate, bool, error) {
	if !m.f.asked.holders[readerID] || !m.f.eventCovered(eventID) {
		return registeredDate{}, false, errDateFactsGap
	}
	statuses := au.Statuses
	if len(statuses) == 0 {
		statuses = defaultRegStatuses
	}
	var best *models.EventOccurrence
	for _, r := range m.f.heldRegs[readerID] {
		if !slices.Contains(statuses, r.Status) {
			continue
		}
		o := m.f.occs[r.OccurrenceID]
		if o == nil || o.EventID != eventID {
			continue
		}
		if au.Type == policy.AudienceOccurrence {
			if o.ID != au.OccurrenceID {
				continue
			}
		} else if o.Status != models.OccurrenceScheduled || o.StartsAt.Before(now) {
			continue
		}
		if best == nil || o.StartsAt.Before(best.StartsAt) || o.StartsAt.Equal(best.StartsAt) && o.ID < best.ID {
			best = o
		}
	}
	if best == nil {
		return registeredDate{}, false, nil
	}
	return registeredDate{UID: readerID, OccurrenceID: best.ID, OriginalDate: best.OriginalDate, StartsAt: best.StartsAt,
		VisibilityOverride: best.VisibilityOverride}, true, nil
}

func (m memDates) untitledOpenDate(eventID uint, open []string) (bool, error) {
	if !m.f.asked.series[eventID] {
		return false, errDateFactsGap
	}
	for _, v := range m.f.untitled[eventID] {
		if slices.Contains(open, v) {
			return true, nil
		}
	}
	return false, nil
}

func (m memDates) untitledGrant(viewer *models.User, eventID uint, now time.Time) (bool, error) {
	if viewer == nil {
		return false, nil
	}
	if !m.f.asked.holders[viewer.ID] || !m.f.eventCovered(eventID) {
		return false, errDateFactsGap
	}
	grants := m.liveGrants(viewer.ID, func(o *models.EventOccurrence) bool {
		return o.EventID == eventID && o.Status == models.OccurrenceScheduled && o.TitleOverride == nil
	})
	reads, err := m.invitations(grants, now)
	return len(reads) > 0, err
}

func (m memDates) liveGroup(id uint) (*models.Group, error) {
	if !m.f.asked.groups[id] {
		return nil, errDateFactsGap
	}
	return m.f.liveGroup(id), nil
}

func (m memDates) groupRel(userID, groupID uint) (policy.Rel, error) {
	if !m.f.asked.viewers[userID] || m.f.groups[groupID] == nil && !m.f.asked.groups[groupID] {
		return policy.Rel{}, errDateFactsGap
	}
	for _, mem := range m.f.membersOf[userID] {
		if mem.GroupID == groupID {
			return membershipRel(*mem), nil
		}
	}
	return policy.Rel{}, nil
}
