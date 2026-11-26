package app

import (
	"encoding/json"
	"errors"
	"slices"
	"time"

	"fold/internal/policy"
	"fold/models"

	"gorm.io/gorm"
)

// errRetryNotShown: the person pressing Try again may no longer see the
// message (CanSeeMessageHistory, judged again after the locks): the same 404
// loadMessage answers.
var errRetryNotShown = errors.New("message not shown to the retry's presser")

// retryDecision is what a Try again does to the failed rows it was given:
// the ones it sends again, and the ones it skips, by reason.
type retryDecision struct {
	keep []uint
	drop map[string][]uint
}

// decideRetryIn decides a Try again on tx, its Read Committed write
// transaction, after the presser's and the message's locks: who presses it
// (policy.CanSeeMessageHistory, a reminder's manager facts included), whether
// the message may go again (an announcement's author and their right to the
// audience; a reminder's date, start and switch), and which of rows (the
// message's failed email rows) message.send would still build. Every fact
// that decides comes from one statement (loadDateFacts), so none is paired
// with another committed after it. The one exception is a guardian copy's
// teen's email filters (recipientFacts: suppressions and choices, each read
// only by its own helper), read just after it: they can only skip a copy,
// never send one, so reading them last honors every opt-out committed before
// the statement, and any committed before they are read. me is the presser's
// locked row.
func (a *App) decideRetryIn(tx *gorm.DB, me *models.User, m *models.Message, rows []models.MessageRecipient, now time.Time) (retryDecision, error) {
	var au audienceJSON
	if err := json.Unmarshal([]byte(m.Audience), &au); err != nil {
		return retryDecision{}, err
	}
	q := dateFactsQuery{MessageID: m.ID, People: []uint{me.ID}, Viewers: []uint{me.ID}, Settings: []string{settingTimezone, settingGuardianCopies}}
	if m.AuthorID != nil {
		q.People, q.Viewers = append(q.People, *m.AuthorID), append(q.Viewers, *m.AuthorID)
	}
	var teens []uint
	for _, r := range rows {
		q.People, q.Viewers, q.Holders = append(q.People, r.UserID), append(q.Viewers, r.UserID), append(q.Holders, r.UserID)
		if r.CopyOfUserID != nil {
			t := *r.CopyOfUserID
			q.People, q.Viewers, q.Holders, teens = append(q.People, t), append(q.Viewers, t), append(q.Holders, t), append(teens, t)
		}
	}
	q.People, q.Viewers, q.Holders, q.Teens = dedupeIDs(q.People), dedupeIDs(q.Viewers), dedupeIDs(q.Holders), dedupeIDs(teens)
	switch au.Type {
	case policy.AudienceOccurrence:
		q.OccurrenceIDs = append(q.OccurrenceIDs, au.OccurrenceID)
	case policy.AudienceEventSeries:
		q.EventIDs = []uint{au.EventID}
	case policy.AudienceEvents:
		q.EventIDs = au.EventIDs
	case policy.AudienceGroup:
		q.GroupIDs = []uint{au.GroupID}
	}
	if m.OccurrenceID != nil {
		q.OccurrenceIDs = append(q.OccurrenceIDs, *m.OccurrenceID)
	}
	facts, err := a.loadDateFacts(tx, q)
	if err != nil {
		return retryDecision{}, err
	}
	teenFacts := map[uint]recipientFact{}
	if len(q.Teens) > 0 {
		filters, err := recipientFacts(tx, q.Teens, messageCategory(m))
		if err != nil {
			return retryDecision{}, err
		}
		for _, f := range filters {
			teenFacts[f.User.ID] = f
		}
	}
	return a.retryDecisionFrom(facts, me, m, au, rows, teenFacts, now)
}

// retryDecisionFrom is decideRetryIn's judgment, from the statement's rows.
func (a *App) retryDecisionFrom(facts *dateFacts, me *models.User, m *models.Message, au audienceJSON, rows []models.MessageRecipient, teenFacts map[uint]recipientFact, now time.Time) (retryDecision, error) {
	src := facts.source(a)
	reminder := m.Category == models.MessageCategoryReminder
	presser := facts.liveUser(me.ID)
	if presser == nil {
		return retryDecision{}, errRetryNotShown
	}
	manages := false
	if reminder && !policy.IsAdmin(asPerson(presser)) && m.OccurrenceID != nil {
		var err error
		if manages, err = managesDateFrom(src, presser, *m.OccurrenceID); err != nil {
			return retryDecision{}, err
		}
	}
	isAuthor := m.AuthorID != nil && *m.AuthorID == presser.ID
	if !policy.CanSeeMessageHistory(asPerson(presser), isAuthor, reminder, manages) {
		return retryDecision{}, errRetryNotShown
	}
	d := retryDecision{drop: map[string][]uint{}}
	if reminder {
		return d, a.retryReminderFrom(src, m, au, rows, teenFacts, now, &d)
	}
	return d, a.retryAnnouncementFrom(src, m, au, rows, teenFacts, now, &d)
}

// managesDateFrom is managesOccurrenceEventIn on src.
func managesDateFrom(src memDates, me *models.User, occID uint) (bool, error) {
	occs, err := src.occurrences([]uint{occID})
	if err != nil || occs[occID] == nil {
		return false, err
	}
	evs, err := src.liveEvents([]uint{occs[occID].EventID})
	if err != nil || len(evs) == 0 {
		return false, err
	}
	sets, err := src.factSets([]uint{me.ID}, evs)
	if err != nil {
		return false, err
	}
	return sets[me.ID].of(&evs[0]).canManage(me), nil
}

// retryReminderFrom: a reminder goes again only while its date is scheduled
// and ahead and its event's reminders are on (reminderMayResend), and only to
// a row whose person (a copy's teen) is still on its audience and may still
// see the date; a guardian copy also only while the copy rule still holds
// (copyRule), as preparation builds it.
func (a *App) retryReminderFrom(src memDates, m *models.Message, au audienceJSON, rows []models.MessageRecipient, teenFacts map[uint]recipientFact, now time.Time, d *retryDecision) error {
	if m.OccurrenceID == nil || reminderKind(m) == reminderPushKind || au.normalize() != nil {
		return errReminderRetryRefused
	}
	occs, err := src.occurrences([]uint{*m.OccurrenceID})
	if err != nil {
		return err
	}
	occ := occs[*m.OccurrenceID]
	if occ == nil {
		return errReminderRetryRefused
	}
	evs, err := src.liveEvents([]uint{occ.EventID})
	if err != nil {
		return err
	}
	if len(evs) == 0 || evs[0].ReminderEmailMinutes <= 0 || occ.Status != models.OccurrenceScheduled || !occ.StartsAt.After(now) {
		return errReminderRetryRefused
	}
	subjects, people := make([]uint, 0, len(rows)), make([]uint, 0, len(rows))
	for _, r := range rows {
		subjects, people = append(subjects, rowSubject(r)), append(people, r.UserID)
	}
	subjects = dedupeIDs(subjects)
	current, err := audienceHolders(src, au, dedupeIDs(append(people, subjects...)), now)
	if err != nil {
		return err
	}
	can, err := reminderReadersFrom(src, *m.OccurrenceID, subjects, now)
	if err != nil && !errors.Is(err, errAudienceGone) {
		return err
	}
	answer := skipNoLongerGoing
	if reminderKind(m) == reminderWaitlist {
		answer = skipNoLongerWaitlist
	}
	copies := newCopyRule(src, teenFacts)
	for _, r := range rows {
		switch {
		case !current[rowSubject(r)]:
			d.drop[answer] = append(d.drop[answer], r.ID)
		case !can[rowSubject(r)]:
			d.drop[skipCantSeeDate] = append(d.drop[skipCantSeeDate], r.ID)
		case r.CopyOfUserID != nil && !copies.goes(r, current[r.UserID]):
			d.drop[skipNoGuardianCopy] = append(d.drop[skipNoGuardianCopy], r.ID)
		default:
			d.keep = append(d.keep, r.ID)
		}
	}
	return nil
}

// copyRule is whether message.send would still build a guardian copy
// (preparedRowsIn, guardianCopyRows): the church still sends copies, the link
// is still confirmed, the teen is still an active teen account who would get
// the message under the usual email filters (recipientFacts, read for the
// teen), and the guardian is not in the audience themselves.
type copyRule struct {
	src   memDates
	on    bool
	pairs map[[2]uint]bool
	teens map[uint]recipientFact
}

func newCopyRule(src memDates, teens map[uint]recipientFact) copyRule {
	c := copyRule{src: src, pairs: map[[2]uint]bool{}, teens: teens}
	if v, found := src.f.settings[settingGuardianCopies]; slices.Contains(src.f.q.Settings, settingGuardianCopies) {
		c.on = boolSettingOf(v, found, false)
	}
	for _, l := range src.f.links {
		if l.TeenUserID != nil {
			c.pairs[[2]uint{l.GuardianUserID, *l.TeenUserID}] = true
		}
	}
	return c
}

// goes judges copy row r; guardianDirect is whether its guardian is in the
// audience themselves.
func (c copyRule) goes(r models.MessageRecipient, guardianDirect bool) bool {
	teenID := *r.CopyOfUserID
	teen := c.src.f.liveUser(teenID)
	filters, known := c.teens[teenID]
	return c.on && !guardianDirect && known && filters.Skip == "" && c.pairs[[2]uint{r.UserID, teenID}] &&
		teen != nil && teen.AccountType == models.AccountTeen && teen.Status == models.UserActive
}

// retryAnnouncementFrom: an announcement goes again, in its author's name,
// only while the author is active and may still write to its audience, and
// only to a row message.send would build today: a direct row's person is
// still in the audience; a guardian copy's teen is, and the copy rule still
// holds (copyRule).
func (a *App) retryAnnouncementFrom(src memDates, m *models.Message, au audienceJSON, rows []models.MessageRecipient, teenFacts map[uint]recipientFact, now time.Time, d *retryDecision) error {
	if m.AuthorID == nil {
		return errMessageRetryRefused
	}
	author := src.f.liveUser(*m.AuthorID)
	if author == nil || author.Status != models.UserActive || au.normalize() != nil {
		return errMessageRetryRefused
	}
	targets, err := audienceTargetsFrom(src, author, au)
	switch {
	case errors.Is(err, errAudienceGone):
		return errMessageRetryRefused
	case err != nil:
		return err
	}
	if !policy.CanMessageAudience(asPerson(author), au.Type, targets) {
		return errMessageRetryRefused
	}
	people := []uint{}
	for _, r := range rows {
		people = append(people, r.UserID)
		if r.CopyOfUserID != nil {
			people = append(people, *r.CopyOfUserID)
		}
	}
	current, err := audienceMembersAmong(src, au, dedupeIDs(people), now)
	if err != nil {
		return err
	}
	copies := newCopyRule(src, teenFacts)
	for _, r := range rows {
		switch why := retryAudienceSkip(au, current, rowSubject(r)); {
		case why != "":
			d.drop[why] = append(d.drop[why], r.ID)
		case r.CopyOfUserID != nil && !copies.goes(r, current[r.UserID]):
			d.drop[skipNoGuardianCopy] = append(d.drop[skipNoGuardianCopy], r.ID)
		default:
			d.keep = append(d.keep, r.ID)
		}
	}
	return nil
}

// retryAudienceSkip is why a failed row about id no longer goes by
// audienceMembersAmong's answer, "" when it still does: for an event audience
// the batch's own words (audienceSkip: no longer on the list, or every date
// hidden), else no longer on the list.
func retryAudienceSkip(au audienceJSON, current map[uint]bool, id uint) string {
	if eventAudience(au.Type) {
		return audienceSkip(current, id)
	}
	if !current[id] {
		return skipNoLongerOnList
	}
	return ""
}

// audienceMembersAmong is which of people audienceUserIDs, filtered for an
// event audience by audienceSeers as resolveAudienceIn filters it, would
// name today, answered from src for those people alone.
func audienceMembersAmong(src memDates, au audienceJSON, people []uint, now time.Time) (map[uint]bool, error) {
	out := map[uint]bool{}
	switch au.Type {
	case policy.AudienceOccurrence, policy.AudienceEventSeries, policy.AudienceEvents:
		return audienceSeersFrom(src, au, people, now, now)
	case policy.AudienceGroup:
		for _, id := range people {
			if !src.f.asked.viewers[id] || !src.f.asked.groups[au.GroupID] {
				return nil, errDateFactsGap
			}
		}
		for _, id := range people {
			for _, mem := range src.f.membersOf[id] {
				if mem.GroupID == au.GroupID && mem.Status == models.MembershipActive && slices.Contains(au.Roles, mem.Role) {
					out[id] = true
				}
			}
		}
	case policy.AudienceAllMembers:
		users, err := src.liveUsers(people)
		if err != nil {
			return nil, err
		}
		for id, u := range users {
			out[id] = u.Status == models.UserActive
		}
	case policy.AudienceUsers:
		listed := idSet(au.UserIDs)
		for _, id := range people {
			out[id] = id != 0 && listed[id]
		}
	}
	return out, nil
}

// audienceHolders is which of people hold a registration in au's statuses on
// its date or an upcoming date of its events (audienceUserIDs for an event
// audience), whether or not they may still see it.
func audienceHolders(src memDates, au audienceJSON, people []uint, now time.Time) (map[uint]bool, error) {
	var eventIDs []uint
	if au.Type != policy.AudienceOccurrence {
		var err error
		if eventIDs, err = src.audienceEventIDs(au); err != nil {
			return nil, err
		}
	}
	held, err := src.holdings(au, eventIDs, people, au.Statuses, now)
	if err != nil {
		return nil, err
	}
	out := map[uint]bool{}
	for _, h := range held {
		out[h.UID] = true
	}
	return out, nil
}
