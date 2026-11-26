package app

import (
	"errors"
	"time"

	"fold/internal/policy"
	"fold/models"

	"gorm.io/gorm"
)

// dateFactSource is where a message's date decisions read their facts: who an
// event audience still reaches (audienceSeersFrom), who may see a reminder's
// date (reminderReadersFrom) and the event fields a reader is shown
// (readerEventValuesFrom). Each decision is written once, against this
// interface. dbDates runs a query per answer on one handle: the read page and
// a push use it inside a read-only snapshot. memDates answers from the rows
// one statement returned (loadDateFacts), for a claimed email row and Try
// again, which decide on a Read Committed transaction where only a single
// statement sees one snapshot.
type dateFactSource interface {
	// viewer is who a date is authorized for when u reads it: eventViewer(u),
	// or nil (a signed-out visitor) for a person the source was told signs in
	// with no authority of their own.
	viewer(u *models.User) *models.User
	// audienceEventIDs is the events au reaches (errAudienceGone when its
	// date is gone).
	audienceEventIDs(au audienceJSON) ([]uint, error)
	// holdings is each (holder, date) pair: a registration in statuses held by
	// one of holders (their own, or one they made for a household person) on
	// au's date or, for a series or several events, on a scheduled date of
	// eventIDs starting at or after since. Sorted by holder, then date id.
	holdings(au audienceJSON, eventIDs, holders []uint, statuses []string, since time.Time) ([]dateHolding, error)
	// occurrences are the dates with these ids that exist.
	occurrences(ids []uint) (map[uint]*models.EventOccurrence, error)
	// liveEvents are the events with these ids that are not deleted, each
	// once, in the order their ids first appear in ids.
	liveEvents(ids []uint) ([]models.Event, error)
	// liveUsers are the accounts with these ids that are not deleted.
	liveUsers(ids []uint) (map[uint]*models.User, error)
	// factSets is loadFactSets: each viewer's group, membership and host facts
	// over evs (viewer 0 has none).
	factSets(viewerIDs []uint, evs []models.Event) (map[uint]factSet, error)
	// candidates is dateCandidatesIn for viewer over one event.
	candidates(viewer *models.User, eventID uint) (visibleSet, error)
	// scoped is scopedOccurrenceReadsIn over the dates ids (only the
	// scheduled ones when scheduledOnly), keyed by date id.
	scoped(viewer *models.User, ids []uint, scheduledOnly bool, now time.Time) (map[uint]*occurrenceRead, error)
	// registeredDate is audienceRegisteredDates for one reader.
	registeredDate(eventID uint, au audienceJSON, readerID uint, now time.Time) (registeredDate, bool, error)
	// untitledOpenDate is whether eventID has a scheduled date with no title
	// of its own whose visibility override is one of open.
	untitledOpenDate(eventID uint, open []string) (bool, error)
	// untitledGrant is hasScopedUntitledRead.
	untitledGrant(viewer *models.User, eventID uint, now time.Time) (bool, error)
	// liveGroup is the group with id unless it is deleted (nil then).
	liveGroup(id uint) (*models.Group, error)
	// groupRel is the user's membership in the group (any status).
	groupRel(userID, groupID uint) (policy.Rel, error)
}

// dateHolding is one registration's (holder, date) pair in an audience.
type dateHolding struct{ UID, OccurrenceID uint }

// dbDates answers with a query each on db, the caller's one handle.
type dbDates struct {
	a  *App
	db *gorm.DB
}

func (a *App) dbDates(db *gorm.DB) dbDates { return dbDates{a: a, db: db} }

func (s dbDates) viewer(u *models.User) *models.User { return eventViewer(u) }

func (s dbDates) audienceEventIDs(au audienceJSON) ([]uint, error) {
	return s.a.audienceEventIDs(s.db, au)
}

func (s dbDates) holdings(au audienceJSON, eventIDs, holders []uint, statuses []string, since time.Time) ([]dateHolding, error) {
	var held []dateHolding
	err := eachIDChunk(holders, func(chunk []uint) error {
		q := s.db.Table("registrations r").Select("DISTINCT COALESCE(r.user_id, r.registered_by) AS uid, r.occurrence_id").
			Joins("JOIN event_occurrences o ON o.id = r.occurrence_id").
			Where("r.status IN ? AND COALESCE(r.user_id, r.registered_by) IN ?", statuses, chunk)
		if au.Type == policy.AudienceOccurrence {
			q = q.Where("o.id = ?", au.OccurrenceID)
		} else {
			q = q.Where("o.event_id IN ? AND o.status = ? AND o.starts_at >= ?", eventIDs, models.OccurrenceScheduled, since)
		}
		var part []dateHolding
		if err := q.Order("uid, r.occurrence_id").Scan(&part).Error; err != nil {
			return err
		}
		held = append(held, part...)
		return nil
	})
	return held, err
}

func (s dbDates) occurrences(ids []uint) (map[uint]*models.EventOccurrence, error) {
	out := map[uint]*models.EventOccurrence{}
	err := eachIDChunk(dedupeIDs(ids), func(chunk []uint) error {
		var part []models.EventOccurrence
		if err := s.db.Where("id IN ?", chunk).Find(&part).Error; err != nil {
			return err
		}
		for i := range part {
			out[part[i].ID] = &part[i]
		}
		return nil
	})
	return out, err
}

func (s dbDates) liveEvents(ids []uint) ([]models.Event, error) {
	ids = dedupeIDs(ids)
	var found []models.Event
	if err := s.db.Where("id IN ?", ids).Find(&found).Error; err != nil {
		return nil, err
	}
	byID := make(map[uint]*models.Event, len(found))
	for i := range found {
		byID[found[i].ID] = &found[i]
	}
	evs := make([]models.Event, 0, len(found))
	for _, id := range ids {
		if ev := byID[id]; ev != nil {
			evs = append(evs, *ev)
		}
	}
	return evs, nil
}

func (s dbDates) liveUsers(ids []uint) (map[uint]*models.User, error) {
	out := map[uint]*models.User{}
	err := eachIDChunk(dedupeIDs(ids), func(chunk []uint) error {
		var part []models.User
		if err := s.db.Where("id IN ?", chunk).Find(&part).Error; err != nil {
			return err
		}
		for i := range part {
			out[part[i].ID] = &part[i]
		}
		return nil
	})
	return out, err
}

func (s dbDates) factSets(viewerIDs []uint, evs []models.Event) (map[uint]factSet, error) {
	return loadFactSets(s.db, viewerIDs, evs)
}

func (s dbDates) candidates(viewer *models.User, eventID uint) (visibleSet, error) {
	return dateCandidatesIn(s.db, viewer, s.db.Where("id = ?", eventID))
}

func (s dbDates) scoped(viewer *models.User, ids []uint, scheduledOnly bool, now time.Time) (map[uint]*occurrenceRead, error) {
	q := s.db.Model(&models.EventOccurrence{}).Where("id IN ?", ids)
	if scheduledOnly {
		q = q.Where("status = ?", models.OccurrenceScheduled)
	}
	return s.a.scopedOccurrenceReadsIn(s.db, viewer, q, now)
}

func (s dbDates) registeredDate(eventID uint, au audienceJSON, readerID uint, now time.Time) (registeredDate, bool, error) {
	regs, err := audienceRegisteredDates(s.db, eventID, au, []uint{readerID}, now)
	if err != nil {
		return registeredDate{}, false, err
	}
	d, ok := regs[readerID]
	return d, ok, nil
}

func (s dbDates) untitledOpenDate(eventID uint, open []string) (bool, error) {
	var hit []models.EventOccurrence
	if err := s.db.Select("id").
		Where("event_id = ? AND status = ? AND title_override IS NULL AND visibility_override IN ?", eventID, models.OccurrenceScheduled, open).
		Limit(1).Find(&hit).Error; err != nil {
		return false, err
	}
	return len(hit) > 0, nil
}

func (s dbDates) untitledGrant(viewer *models.User, eventID uint, now time.Time) (bool, error) {
	return s.a.hasScopedUntitledRead(s.db, viewer, eventID, now)
}

func (s dbDates) liveGroup(id uint) (*models.Group, error) {
	groups, err := groupsByID(s.db, []uint{id}, false)
	return groups[id], err
}

func (s dbDates) groupRel(userID, groupID uint) (policy.Rel, error) {
	return groupRel(s.db, userID, groupID)
}

// errDateFactsGap: a memDates question reached past the rows its statement
// loaded. It is a programming error, answered as a failed read so the send
// rolls back instead of reading a missing row as "may not see".
var errDateFactsGap = errors.New("date facts: asked about rows the statement did not load")
