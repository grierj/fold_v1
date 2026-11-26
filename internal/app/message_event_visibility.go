package main

import (
	"time"

	"fold/internal/policy"
	"fold/models"

	"gorm.io/gorm"
)

// The event fields of an announcement a person is sent are filled when they
// are sent, by what that person may see of the event or date at that moment,
// through readerEventValues: the same per-field rules as the read page
// (exact-date invitations, occurrence overrides, cancellation, date-specific
// links). The recipient snapshot decides who is sent a message and never what
// event data they see; a rename, a tightened visibility or a lost membership
// between preparation and delivery reaches the next send, retry or push.
//
// An event audience's message also goes only while a date the person may
// still see puts them in the audience (audienceSeers), decided again at every
// send. A push and the read page decide and fill in one read-only snapshot,
// then dispatch or answer holding nothing. A claimed email row of a reminder
// or an event audience decides and fills from one statement on its Read
// Committed claim transaction (decideDatedRow, loadDateFacts), so no grant is
// paired with a date committed after it. No lock is held across network
// delivery beyond the row's own claim, so a change committed after the
// decision may not stop that one send; the next row, retry or push rechecks.

// rowReaders loads the live people one claimed email row concerns: the
// recipient, and for a guardian copy the teen whose message it copies. The
// recipient is nil when that account is gone; the teen is absent from the map
// then too. Role and status come from this read, never from a batch-wide
// cache, because an administrator may have been demoted since an earlier row
// loaded.
func rowReaders(db *gorm.DB, row models.MessageRecipient) (*models.User, map[uint]*models.User, error) {
	ids := []uint{row.UserID}
	if row.CopyOfUserID != nil {
		ids = append(ids, *row.CopyOfUserID)
	}
	var users []models.User
	if err := db.Where("id IN ?", ids).Find(&users).Error; err != nil {
		return nil, nil, err
	}
	readers := make(map[uint]*models.User, len(users))
	for i := range users {
		readers[users[i].ID] = &users[i]
	}
	return readers[row.UserID], readers, nil
}

// rowValues are the merge values one email row of an audience with no dates
// (a group, everyone, a list) renders with, keyed by the user whose values
// they are: the recipient, or for a guardian copy the teen. Such an audience
// fills the first name alone; a dated row's values come from decideDatedRow.
// A person who is gone gets no entry.
func (a *App) rowValues(db *gorm.DB, row models.MessageRecipient, readers map[uint]*models.User, au audienceJSON, now time.Time) (map[uint]map[string]string, error) {
	subject := row.UserID
	if row.CopyOfUserID != nil {
		subject = *row.CopyOfUserID
	}
	out := map[uint]map[string]string{}
	u := readers[subject]
	if u == nil {
		return out, nil
	}
	vals, err := a.readerEventValues(db, u, au, now)
	if err != nil {
		return nil, err
	}
	out[subject] = vals
	return out, nil
}

// rowSubject is whose access and values a claimed email row follows: the
// recipient, or for a guardian copy the teen whose message it copies.
func rowSubject(row models.MessageRecipient) uint {
	if row.CopyOfUserID != nil {
		return *row.CopyOfUserID
	}
	return row.UserID
}

// rowFactsQuery is what one claimed email row's facts statement reads: the
// recipient and subject, the subject's memberships, host rows and
// registrations over the audience's events and dates (and a reminder's own
// date), the series' untitled dates, the church's zone, and the message as it
// now stands.
func rowFactsQuery(m *models.Message, au audienceJSON, row models.MessageRecipient) dateFactsQuery {
	subject := rowSubject(row)
	q := dateFactsQuery{MessageID: m.ID, People: dedupeIDs([]uint{row.UserID, subject}), Viewers: []uint{subject},
		Holders: []uint{subject}, Settings: []string{settingTimezone}}
	switch au.Type {
	case policy.AudienceOccurrence:
		q.OccurrenceIDs = append(q.OccurrenceIDs, au.OccurrenceID)
	case policy.AudienceEventSeries:
		q.EventIDs, q.SeriesTitles = []uint{au.EventID}, []uint{au.EventID}
	case policy.AudienceEvents:
		q.EventIDs = au.EventIDs
	}
	if m.OccurrenceID != nil {
		q.OccurrenceIDs = append(q.OccurrenceIDs, *m.OccurrenceID)
	}
	return q
}
