package main

import (
	"fmt"

	"fold/internal/notify"
	"fold/models"

	"gorm.io/gorm"
)

// notifyOccurrences tells everyone going or waitlisted on these dates that
// the date was cancelled (OccurrenceCancelled; M1.5: maybe no longer gets one)
// or, after a series edit had cancelled it, is back on (OccurrenceRestored,
// DEC-91; their registrations were never touched, so that notice says the
// sign-up is still there). A registrant who can't see the group gets no group
// name or group place; one who can't see the date gets the kind's nameless
// text through withheldDate. One stored item per person per date.
func notifyOccurrences(tx *gorm.DB, actor uint, occIDs []uint, kind string) error {
	if len(occIDs) == 0 {
		return nil
	}
	var rows []struct {
		UserID uint
		OccID  uint
	}
	if err := tx.Table("registrations").Joins(listedRegistrations()).Distinct("users.id AS user_id, registrations.occurrence_id AS occ_id").
		Where("registrations.occurrence_id IN ? AND registrations.status IN ?", occIDs,
			[]string{models.RegGoing, models.RegWaitlisted}).
		Order("occ_id, user_id").Scan(&rows).Error; err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	// Load the occurrences, their events and every recipient's facts
	// once (issue #76); the loop below only writes notices.
	var occRows []models.EventOccurrence
	if err := tx.Where("id IN ?", occIDs).Find(&occRows).Error; err != nil {
		return err
	}
	occByID := make(map[uint]*models.EventOccurrence, len(occRows))
	eventIDs := make([]uint, 0, len(occRows))
	for i := range occRows {
		occByID[occRows[i].ID] = &occRows[i]
		eventIDs = append(eventIDs, occRows[i].EventID)
	}
	evByID := map[uint]*models.Event{}
	var groupIDs []uint
	if len(eventIDs) > 0 {
		var evRows []models.Event
		if err := tx.Where("id IN ?", eventIDs).Find(&evRows).Error; err != nil {
			return err
		}
		for i := range evRows {
			evByID[evRows[i].ID] = &evRows[i]
			if evRows[i].GroupID != nil {
				groupIDs = append(groupIDs, *evRows[i].GroupID)
			}
		}
	}
	userIDs := make([]uint, 0, len(rows))
	for _, r := range rows {
		userIDs = append(userIDs, r.UserID)
	}
	facts, err := loadNoticeFacts(tx, groupIDs, userIDs)
	if err != nil {
		return err
	}
	accesses, err := occurrenceNoticeAccesses(tx, occIDs)
	if err != nil {
		return err
	}
	// One bulk lock for the whole fan-out (ascending user ID), rather than a
	// lock query inside each per-person Send below.
	return notify.WithRecipients(tx, userIDs, func(scope *notify.Scope) error {
		for _, r := range rows {
			o := occByID[r.OccID]
			if o == nil {
				continue
			}
			ev := evByID[o.EventID]
			if ev == nil {
				continue
			}
			if _, ok := accesses[o.ID][r.UserID]; ok {
				// Lifecycle reconciliation owns the single invitation-ended notice.
				// Ordinary registrants still receive the cancellation notice below.
				continue
			}
			// The title, time and place may have been set while the date was
			// hidden from this person (a series edit's new title, a date edit
			// in an archived group), so only someone who may see the date now
			// is told them. Everyone else still hears that something they
			// signed up for is off, or back on, naming nothing.
			end := noticeEnd(o)
			m := notify.Msg{
				Kind: kind, To: []uint{r.UserID}, Actor: &actor,
				// A date can be cancelled, restored and cancelled again, so the key
				// carries the moment of this change (the row was just updated).
				DedupeKey: fmt.Sprintf("%s:%d:%d", kind, o.ID, o.UpdatedAt.UnixNano()),
				Scope:     scope,
			}
			if facts.canSeeDate(r.UserID, ev, o) {
				m.Vars, m.Link = eventNoticeVars(ev, o, r.UserID, facts)
				m.SubjectType, m.SubjectID, m.ExpiresAt = "occurrence", o.ID, &end
				m.Data = map[string]any{"eventId": ev.ID, "date": models.FormatCalendarDate(o.OriginalDate)}
			} else {
				m = withheldDate(m)
			}
			_, err := notify.Send(tx, m)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// notifyRegistrationCancelled tells someone who can no longer see an event
// that their registrations were cancelled. It names neither the group nor
// the place and links nowhere private. ev.Title is the title the
// person knew the cancelled dates by, a date's own title included. date is
// the calendar day of the one date cancelled, or "" for a series change: it is
// joined to the title the person sees, and kept out of the plain title stored
// in Data, which is all a guardian's copy shows (DEC-149). An empty ev.Title
// is the caller withholding it because the date was hidden from the person
// just before the change: the notice names no event and no date, and stores
// no plain title, so it has no guardian copy.
func notifyRegistrationCancelled(tx *gorm.DB, actor uint, ev *models.Event, userID uint, date string) error {
	u, err := userIn(tx, userID)
	if err != nil || u == nil || u.Status == models.UserDisabled {
		return err
	}
	var shown string
	var data map[string]any
	if ev.Title != "" {
		shown = ev.Title
		if date != "" {
			shown += " · " + date
		}
		data = map[string]any{notify.DataEventTitle: ev.Title}
	}
	m := notify.Msg{
		Kind: notify.RegistrationCancelled, To: []uint{userID}, Actor: &actor,
		SubjectType: "event", SubjectID: ev.ID, Link: "/", Vars: notify.Vars{Event: shown},
		Data: data,
	}
	if ev.Title == "" {
		m = withheldDate(m)
	}
	_, err = notify.Send(tx, m)
	return err
}

// withheldDate is m for a recipient who may not see the date it is about:
// the kind's nameless text with no title, group, time, place or link. The row
// keeps no subject, expiry or data either, because GET /notifications shows
// the subject id, the feed archives a row at its expiry, and a PushSoon push
// reads its subject's clock for urgency and quiet hours: none of them may
// follow a date the recipient cannot see. The private dedupe key stays. Only
// Vars that are the recipient's own survive: a household person they
// registered, that person's badge and a note written to them.
func withheldDate(m notify.Msg) notify.Msg {
	m.Vars = notify.Vars{Person: m.Vars.Person, SubjectKind: m.Vars.SubjectKind, Note: m.Vars.Note, Change: m.Vars.Change}
	m.SubjectType, m.SubjectID, m.Link, m.Data, m.ExpiresAt = "", 0, "/", nil, nil
	return m
}
