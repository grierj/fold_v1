package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"fold/internal/audit"
	"fold/internal/content"
	"fold/internal/policy"
	"fold/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// The composer's API (M3.4, PLAN 7.1–7.3). Leaders, hosts and admins write
// announcements to the people they may reach (policy.CanMessageAudience),
// save drafts, and schedule them for now or later; M3.5 sends them. Email
// only for now: texting is not built (DEC-117).

type messageJSON struct {
	ID          uint         `json:"id"`
	Channel     string       `json:"channel"`
	Status      string       `json:"status"`
	Subject     string       `json:"subject"`
	BodyHTML    string       `json:"bodyHtml"`
	Audience    audienceJSON `json:"audience"`
	ScheduledAt *time.Time   `json:"scheduledAt"`
	SentAt      *time.Time   `json:"sentAt"`
	AuthorName  string       `json:"authorName"`
	// Editable: still a draft or scheduled, and the viewer may change it.
	Editable  bool      `json:"editable"`
	UpdatedAt time.Time `json:"updatedAt"`
	// Error says why it failed, or why some people didn't get it (M3.5):
	// the pipeline's words or the provider's error, never an address.
	Error       string `json:"error"`
	FailedCount int    `json:"failedCount"`
	// Category is announcement, or reminder for an automatic event
	// reminder (M3.6), which has no author and is never editable.
	Category string `json:"category"`
	// Reminder names a reminder's date; absent for an announcement, and for
	// a reminder whose date is gone.
	Reminder *messageReminderJSON `json:"reminder,omitempty"`
	// DisplaySubject is a reminder's subject with its event fields filled
	// as its emails said them ("Reminder: Book night on Tue, Nov 3 · 7:00
	// PM MST"), for its page's heading; "" for an announcement, whose
	// subject is shown as written.
	DisplaySubject string `json:"displaySubject"`
	// PushAlert is "Push notification too" (M3.13d2, DEC-123): the people
	// who get the email and have push on also get a short notification.
	PushAlert bool `json:"pushAlert"`
	// PushWaiting counts a push reminder's rows still to go (M3.13d3), so
	// its status reads "Waiting to go out" only while some are; 0 for
	// anything else.
	PushWaiting int `json:"pushWaiting"`
}

// messageReminderJSON is the date an automatic reminder is for: "Automatic
// reminder for Book night, Tue, Nov 3". Kind is going (the reminder),
// waitlist (the "still on the waitlist" note, DEC-133) or push (the 2-hour
// push reminder, M3.13d3).
type messageReminderJSON struct {
	Kind         string    `json:"kind"`
	OccurrenceID uint      `json:"occurrenceId"`
	EventID      uint      `json:"eventId"`
	EventTitle   string    `json:"eventTitle"`
	Date         string    `json:"date"`
	StartsAt     time.Time `json:"startsAt"`
	// TZID is the event's zone, which its emails' {{event_time}} reads in,
	// and its date is shown in.
	TZID string `json:"tzid" gorm:"column:tzid"`
}

// reminderSubject fills a reminder's event fields as message.send does
// (the date's title, its time on the event's clock); any other field
// stays as written.
func reminderSubject(subject string, d messageReminderJSON) string {
	vals := messageEvent{ID: d.EventID, Title: d.EventTitle, TZID: d.TZID}.fields("", &d.StartsAt)
	return strings.NewReplacer("{{"+content.FieldEventTitle+"}}", vals[content.FieldEventTitle], "{{"+content.FieldEventTime+"}}", vals[content.FieldEventTime]).Replace(subject)
}

// reminderKind is going, waitlist or push, from a reminder's key (a
// reminderKey).
func reminderKind(m *models.Message) string {
	if m.ReminderKey != nil {
		if k, ok := parseReminderKey(*m.ReminderKey); ok && (k.Kind == reminderWaitlist || k.Kind == reminderPushKind) {
			return k.Kind
		}
	}
	return reminderGoing
}

var (
	errMessageReminder      = apiErr(http.StatusConflict, "message_reminder", "automatic reminders can't be changed here; change the event's reminder time instead")
	errReminderRetryRefused = apiErr(http.StatusForbidden, "reminder_retry_refused", "this date has started or was cancelled, or its event's reminders are off, so its reminder can't be sent again")
)

// Composer limits, matching the CHECKs on messages (subject_check,
// body_html_check, body_text_check); the errors below quote them.
const (
	maxMessageSubject = 200
	maxMessageHTML    = 100000
	maxMessageText    = 50000
	// A message can be scheduled up to a year ahead.
	maxScheduleAhead = 366 * 24 * time.Hour
)

var (
	errMessageSubject  = apiErr(http.StatusBadRequest, "message_subject", fmt.Sprintf("add a subject on one line, up to %d characters", maxMessageSubject))
	errMessageBody     = apiErr(http.StatusBadRequest, "message_body", "write a message of under about "+thousands(maxMessageText)+" characters")
	errMessageNobody   = apiErr(http.StatusBadRequest, "message_nobody", "no one would get this message; choose people who can get email")
	errMessageLocked   = apiErr(http.StatusConflict, "message_locked", "this message is already sending or sent, so it can't change")
	errMessageChanged  = apiErr(http.StatusConflict, "message_changed", "this message was changed somewhere else; check it and try again")
	errMessageWhen     = apiErr(http.StatusBadRequest, "message_when", "pick a time from now to a year ahead")
	errMessageChannel  = apiErr(http.StatusBadRequest, "message_channel", "texting isn't set up yet; send this as an email")
	errMessageNotYours = apiErr(http.StatusForbidden, "message_not_yours", "you can't send to those people")
	// errMessageImage refuses a body whose picture is not one uploaded to this
	// message (DEC-118): an upload belongs to the message it was made for.
	errMessageImage = apiErr(http.StatusBadRequest, "message_image", "a picture in this message wasn't added to it here; add it again")
	// errMessagePushOnly refuses an announcement asked to go by push
	// alone (DEC-123): push is a companion to the email, never instead of it.
	errMessagePushOnly = apiErr(http.StatusBadRequest, "message_push_only", "an announcement always goes by email")
)

// messageFacts is what a list of messages shows beyond their own rows:
// each author's name, how many people each send failed for, and each
// reminder's date.
type messageFacts struct {
	authors map[uint]string
	failed  map[uint]int
	// pushWaiting: a push reminder's rows still to go (M3.13d3).
	pushWaiting map[uint]int
	dates       map[uint]messageReminderJSON
}

// loadMessageFactsIn reads them for msgs on db in four queries, whatever the
// count.
func loadMessageFactsIn(db *gorm.DB, msgs []models.Message) (messageFacts, error) {
	out := messageFacts{authors: map[uint]string{}, failed: map[uint]int{}, pushWaiting: map[uint]int{}, dates: map[uint]messageReminderJSON{}}
	ids, settled, occs, pushes := []uint{}, []uint{}, []uint{}, []uint{}
	for _, m := range msgs {
		if m.AuthorID != nil {
			ids = append(ids, *m.AuthorID)
		}
		if m.Status == models.MessageSent || m.Status == models.MessageFailed {
			settled = append(settled, m.ID)
		}
		if m.Category == models.MessageCategoryReminder && m.OccurrenceID != nil {
			occs = append(occs, *m.OccurrenceID)
		}
		if m.Channel == models.DeliveryPush {
			pushes = append(pushes, m.ID)
		}
	}
	if len(occs) > 0 {
		// A deleted event keeps its title here: an admin's list still says
		// what the reminder was for.
		var rows []messageReminderJSON
		if err := db.Raw(`SELECT o.id AS occurrence_id, o.event_id, COALESCE(o.title_override, e.title) AS event_title,
			to_char(o.original_date, 'YYYY-MM-DD') AS date, o.starts_at, e.tzid FROM event_occurrences o JOIN events e ON e.id = o.event_id WHERE o.id IN ?`,
			dedupeIDs(occs)).Scan(&rows).Error; err != nil {
			return out, err
		}
		for _, r := range rows {
			out.dates[r.OccurrenceID] = r
		}
	}
	if len(ids) > 0 {
		var users []models.User
		if err := db.Select("id, name, display_name").Where("id IN ?", dedupeIDs(ids)).Find(&users).Error; err != nil {
			return out, err
		}
		for _, u := range users {
			out.authors[u.ID] = u.PublicName()
		}
	}
	if len(settled) > 0 {
		var counts []struct {
			MessageID uint
			N         int
		}
		// A message's counts follow its email; push rows are companions.
		if err := db.Model(&models.MessageRecipient{}).Select("message_id, count(*) AS n").
			Where("message_id IN ? AND status = ? AND channel = ?", settled, models.RecipientFailed, models.DeliveryEmail).Group("message_id").Scan(&counts).Error; err != nil {
			return out, err
		}
		for _, c := range counts {
			out.failed[c.MessageID] = c.N
		}
	}
	if len(pushes) > 0 {
		var counts []struct {
			MessageID uint
			N         int
		}
		// A push reminder's rows still to go (M3.13d3).
		if err := db.Model(&models.MessageRecipient{}).Select("message_id, count(*) AS n").
			Where("message_id IN ? AND status = ? AND channel = ?", pushes, models.RecipientQueued, models.DeliveryPush).Group("message_id").Scan(&counts).Error; err != nil {
			return out, err
		}
		for _, c := range counts {
			out.pushWaiting[c.MessageID] = c.N
		}
	}
	return out, nil
}

func toMessageJSON(m *models.Message, me *models.User, facts messageFacts) messageJSON {
	var au audienceJSON
	_ = json.Unmarshal([]byte(m.Audience), &au)
	isAuthor := m.AuthorID != nil && *m.AuthorID == me.ID
	name := ""
	if m.AuthorID != nil {
		name = facts.authors[*m.AuthorID]
	}
	reminder := m.Category == models.MessageCategoryReminder
	out := messageJSON{ID: m.ID, Channel: m.Channel, Status: m.Status, Subject: m.Subject, BodyHTML: m.BodyHTML,
		Audience: au, ScheduledAt: m.ScheduledAt, SentAt: m.SentAt, AuthorName: name,
		Editable:  policy.MessageEditable(m.Status) && policy.CanEditMessage(asPerson(me), isAuthor, reminder),
		UpdatedAt: m.UpdatedAt, Error: messageErrorFor(m.Error, policy.IsAdmin(asPerson(me))), FailedCount: facts.failed[m.ID],
		Category: m.Category, PushAlert: m.PushAlert, PushWaiting: facts.pushWaiting[m.ID]}
	if reminder && m.OccurrenceID != nil {
		if d, ok := facts.dates[*m.OccurrenceID]; ok {
			d.Kind = reminderKind(m)
			out.Reminder = &d
			out.DisplaySubject = reminderSubject(m.Subject, d)
		}
	}
	return out
}

// messageErrorFor is a stored error as the viewer reads it: the
// pipeline's own plain texts (ownMessageErrors) as they are, anything else
// (a provider's error) in plain words unless the viewer is an admin.
func messageErrorFor(stored string, admin bool) string {
	if stored == "" || ownMessageErrors[stored] || admin {
		return stored
	}
	return whyProvider
}

// freshMessageJSON is the reply to a write on m: the message as it stands
// after the write, presented to the session's user as they stand now
// (presentMessage, at the "message reply" seam), never by the role the
// request began with.
func (a *App) freshMessageJSON(c *gin.Context, m *models.Message) (messageJSON, bool) {
	return a.presentMessage(c, m.ID, "message reply")
}

// presentMessage reads the session's user, their right to message id
// (messageHistoryAllowedIn) and what it shows (a reminder's event title and
// date, counts, the author's name) in one read-only snapshot. It answers the
// error itself (false): 401 when the session is no longer current, 404 when
// the viewer may not see the message, 500 when a read fails. surface names
// its readerAuthorized seam.
func (a *App) presentMessage(c *gin.Context, id uint, surface string) (messageJSON, bool) {
	var out messageJSON
	var me *models.User
	err := a.DB.Transaction(func(tx *gorm.DB) error {
		var err error
		if me, err = snapshotViewerIn(tx, apiUser(c)); err != nil || me == nil {
			return err
		}
		var m models.Message
		if err := tx.First(&m, id).Error; err != nil {
			return err
		}
		if allowed, err := messageHistoryAllowedIn(tx, me, &m); err != nil {
			return err
		} else if !allowed {
			return gorm.ErrRecordNotFound
		}
		if a.hooks.readerAuthorized != nil {
			a.hooks.readerAuthorized(surface)
		}
		facts, err := loadMessageFactsIn(tx, []models.Message{m})
		out = toMessageJSON(&m, me, facts)
		return err
	}, readSnapshot)
	switch {
	case me == nil && err == nil:
		fail(c, errSecuritySession)
	case errors.Is(err, gorm.ErrRecordNotFound):
		jsonError(c, http.StatusNotFound, "message not found")
	case err != nil:
		jsonError(c, http.StatusInternalServerError, "could not load the message")
	default:
		return out, true
	}
	return messageJSON{}, false
}

func (a *App) respondMessage(c *gin.Context, status int, m *models.Message) {
	if out, ok := a.freshMessageJSON(c, m); ok {
		c.JSON(status, messageResponseJSON{Message: out})
	}
}

// canCompose: the composer is for admins and anyone who leads an approved
// group or hosts a church-wide event.
func (a *App) canCompose(me *models.User) (bool, error) { return canComposeIn(a.DB, me) }

// canComposeIn is canCompose read on db.
func canComposeIn(db *gorm.DB, me *models.User) (bool, error) {
	leads, err := leadsApprovedGroup(db, me.ID)
	if err != nil {
		return false, err
	}
	var hosts int64
	if err := db.Model(&models.EventHost{}).Joins("JOIN events e ON e.id = event_hosts.event_id AND e.deleted_at IS NULL").
		Where("event_hosts.user_id = ?", me.ID).Count(&hosts).Error; err != nil {
		return false, err
	}
	return policy.CanComposeMessages(asPerson(me), leads, hosts > 0), nil
}

// checkAudience normalizes au and answers whether me may send to it, and
// who it reaches; false means the error response was sent.
func (a *App) checkAudience(c *gin.Context, me *models.User, au *audienceJSON) ([]uint, bool) {
	ids, err := a.resolveAudience(me, au, time.Now())
	return ids, answerAudience(c, me, err)
}

// answerAudience answers a resolveAudience error for me; true means there was
// none.
func answerAudience(c *gin.Context, me *models.User, err error) bool {
	switch {
	case errors.Is(err, errAudienceInvalid):
		fail(c, errAudienceInvalid)
	case errors.Is(err, errAudienceGone) && policy.IsAdmin(asPerson(me)):
		fail(c, errAudienceGone)
	case errors.Is(err, errAudienceGone), errors.Is(err, errAudienceRefused):
		// Only an admin learns a target is gone; anyone else hears what a
		// target that isn't theirs answers, so ids can't be probed.
		fail(c, errMessageNotYours)
	case err != nil:
		jsonError(c, http.StatusInternalServerError, "could not check who gets this message")
	default:
		return true
	}
	return false
}

// summarizeAudienceFor checks that the session's user may message au and
// counts who it reaches, with their names, in one read-only Repeatable Read
// snapshot: the author's rights, the audience's targets, the dates that put
// each registrant in it, the grants behind those dates and the names are one
// committed state, so a preview never names someone no state admits. false
// means the error response was sent.
func (a *App) summarizeAudienceFor(c *gin.Context, au *audienceJSON) (audienceSummaryJSON, bool) {
	var s audienceSummaryJSON
	var me *models.User
	err := a.DB.Transaction(func(tx *gorm.DB) error {
		var err error
		if me, err = snapshotViewerIn(tx, apiUser(c)); err != nil || me == nil {
			return err
		}
		ids, err := a.resolveAudienceIn(tx, me, au, a.now())
		if err != nil {
			return err
		}
		s, err = a.summarizeAudience(tx, ids, policy.IsAdmin(asPerson(me)))
		return err
	}, readSnapshot)
	switch {
	case me == nil && err == nil:
		fail(c, errSecuritySession)
		return s, false
	case errors.Is(err, errAudienceInvalid), errors.Is(err, errAudienceGone), errors.Is(err, errAudienceRefused):
		return s, answerAudience(c, me, err)
	case err != nil:
		jsonError(c, http.StatusInternalServerError, "could not count who gets this message")
		return s, false
	}
	return s, true
}

// errAudienceRefused: me may not message that audience (policy).
var errAudienceRefused = errors.New("not allowed to message that audience")

// resolveAudience normalizes au, checks that me may message it
// (policy.CanMessageAudience) and returns who it reaches; an event audience
// reaches only the registrants a date they may still see puts in it
// (audienceSeers). The handlers and
// message.send (at send time, PLAN 7.1 step 1) share it. Errors are
// errAudienceInvalid, errAudienceGone, errAudienceRefused or a database
// error.
func (a *App) resolveAudience(me *models.User, au *audienceJSON, now time.Time) ([]uint, error) {
	return a.resolveAudienceIn(a.DB, me, au, now)
}

// resolveAudienceIn is resolveAudience on db, so a caller inside a
// transaction reads the audience on its own connection.
func (a *App) resolveAudienceIn(db *gorm.DB, me *models.User, au *audienceJSON, now time.Time) ([]uint, error) {
	if err := au.normalize(); err != nil {
		return nil, errAudienceInvalid
	}
	targets, err := a.audienceTargets(db, me, *au)
	if err != nil {
		return nil, err
	}
	if !policy.CanMessageAudience(asPerson(me), au.Type, targets) {
		return nil, errAudienceRefused
	}
	ids, err := a.audienceUserIDs(db, *au, now)
	if err != nil || !eventAudience(au.Type) {
		return ids, err
	}
	// A registrant reaches an event audience only through a date they may
	// still see (audienceSeers): the preview, the send and Try again agree.
	seers, err := a.audienceSeers(db, *au, ids, now, now)
	if err != nil {
		return nil, err
	}
	kept := make([]uint, 0, len(ids))
	for _, id := range ids {
		if seers[id] {
			kept = append(kept, id)
		}
	}
	return kept, nil
}

// composeGroupJSON / composeEventJSON are what the picker offers.
type composeGroupJSON struct {
	ID   uint   `json:"id"`
	Name string `json:"name"`
}

type composeDateJSON struct {
	OccurrenceID uint      `json:"occurrenceId"`
	Date         string    `json:"date"`
	StartsAt     time.Time `json:"startsAt"`
}

type composeEventJSON struct {
	ID         uint              `json:"id"`
	Title      string            `json:"title"`
	GroupName  string            `json:"groupName,omitempty"`
	ChurchWide bool              `json:"churchWide"`
	Dates      []composeDateJSON `json:"dates"`
}

// composeOptionsJSON is GET /messages/compose: who the author may write to.
type composeOptionsJSON struct {
	CanCompose         bool `json:"canCompose"`
	CanMessageEveryone bool `json:"canMessageEveryone"`
	// PushEnabled shows "Push notification too": the church's switch for
	// notifications on devices (DEC-124). Off, no announcement pushes.
	PushEnabled bool               `json:"pushEnabled"`
	Groups      []composeGroupJSON `json:"groups"`
	Events      []composeEventJSON `json:"events"`
}

// How much the picker lists.
const (
	maxComposeEvents = 200
	maxComposeDates  = 10
)

// apiComposeOptions lists only what the author may message (M3.4
// acceptance): admins every active group and event with upcoming dates,
// leaders their own active groups' groups and events, hosts the church-wide
// events they host. ?event=<id> narrows it to that one event, for a page
// that only asks whether the viewer may write to it (the date page): the
// same rules, no groups, and no events when they may not.
func (a *App) apiComposeOptions(c *gin.Context) {
	var onlyEvent uint64
	if raw := c.Query("event"); raw != "" {
		n, err := strconv.ParseUint(raw, 10, 63)
		if err != nil || n == 0 {
			jsonError(c, http.StatusBadRequest, "invalid request")
			return
		}
		onlyEvent = n
	}
	var out composeOptionsJSON
	var me *models.User
	// The session's user, what they lead and host, and the events and dates
	// offered come from one read-only snapshot, so a leader who loses a group
	// is never offered its events as they stood after.
	err := a.DB.Transaction(func(tx *gorm.DB) error {
		var err error
		if me, err = snapshotViewerIn(tx, apiUser(c)); err != nil || me == nil {
			return err
		}
		out, err = a.composeOptionsIn(tx, me, onlyEvent)
		return err
	}, readSnapshot)
	switch {
	case me == nil && err == nil:
		fail(c, errSecuritySession)
	case err != nil:
		jsonError(c, http.StatusInternalServerError, "could not load the composer")
	default:
		c.JSON(http.StatusOK, out)
	}
}

// composeOptionsIn is the picker for me, read on tx.
func (a *App) composeOptionsIn(tx *gorm.DB, me *models.User, onlyEvent uint64) (composeOptionsJSON, error) {
	p := asPerson(me)
	ok, err := canComposeIn(tx, me)
	if err != nil {
		return composeOptionsJSON{}, err
	}
	pushOn, err := boolSettingErrIn(tx, settingPushEnabled, true)
	if err != nil {
		return composeOptionsJSON{}, err
	}
	out := composeOptionsJSON{CanCompose: ok, CanMessageEveryone: policy.IsAdmin(p), PushEnabled: pushOn, Groups: []composeGroupJSON{}, Events: []composeEventJSON{}}
	if !ok {
		return out, nil
	}
	groups := []models.Group{}
	gq := tx.Where("status = ?", models.GroupActive).Order("lower(name)")
	if !p.Admin {
		gq = gq.Where("id IN (SELECT group_id FROM memberships WHERE user_id = ? AND role = ? AND status = ?)", me.ID, models.MembershipLeader, models.MembershipActive)
	}
	if err := gq.Find(&groups).Error; err != nil {
		return out, err
	}
	names := map[uint]string{}
	gjson := []composeGroupJSON{}
	gids := []uint{}
	for _, g := range groups {
		gjson = append(gjson, composeGroupJSON{ID: g.ID, Name: g.Name}) // group-ok: groups the author leads, or an admin's
		names[g.ID] = g.Name                                            // group-ok: same
		gids = append(gids, g.ID)
	}
	if a.hooks.readerAuthorized != nil {
		a.hooks.readerAuthorized("compose picker")
	}
	now := a.now()
	eq := tx.Where("id IN (SELECT event_id FROM event_occurrences WHERE status = ? AND starts_at >= ?)", models.OccurrenceScheduled, now)
	if !p.Admin {
		eq = eq.Where("(group_id IN ?) OR (group_id IS NULL AND id IN (SELECT event_id FROM event_hosts WHERE user_id = ?))", append(gids, 0), me.ID)
	} else {
		eq = eq.Where("group_id IS NULL OR group_id IN ?", append(gids, 0))
	}
	if onlyEvent > 0 {
		eq = eq.Where("id = ?", onlyEvent)
	}
	var evs []models.Event
	if err := eq.Order("lower(title), id").Limit(maxComposeEvents).Find(&evs).Error; err != nil {
		return out, err
	}
	// Each event's next few dates, in one read.
	eids := make([]uint, 0, len(evs))
	for _, ev := range evs {
		eids = append(eids, ev.ID)
	}
	var dates []struct {
		EventID      uint
		ID           uint
		OriginalDate string
		StartsAt     time.Time
	}
	if err := tx.Raw(`SELECT event_id, id, to_char(original_date, 'YYYY-MM-DD') AS original_date, starts_at FROM (
		SELECT event_id, id, original_date, starts_at, row_number() OVER (PARTITION BY event_id ORDER BY starts_at, id) AS n
		FROM event_occurrences WHERE event_id IN ? AND status = ? AND starts_at >= ?) d WHERE n <= ? ORDER BY starts_at, id`,
		append(eids, 0), models.OccurrenceScheduled, now, maxComposeDates).Scan(&dates).Error; err != nil {
		return out, err
	}
	byEvent := map[uint][]composeDateJSON{}
	for _, d := range dates {
		byEvent[d.EventID] = append(byEvent[d.EventID], composeDateJSON{OccurrenceID: d.ID, Date: d.OriginalDate, StartsAt: d.StartsAt})
	}
	ejson := []composeEventJSON{}
	for _, ev := range evs {
		e := composeEventJSON{ID: ev.ID, Title: ev.Title, ChurchWide: ev.GroupID == nil, Dates: []composeDateJSON{}}
		if ev.GroupID != nil {
			e.GroupName = names[*ev.GroupID]
		}
		if d := byEvent[ev.ID]; d != nil {
			e.Dates = d
		}
		ejson = append(ejson, e)
	}
	out.Groups, out.Events = gjson, ejson
	if onlyEvent > 0 {
		out.Groups = []composeGroupJSON{}
	}
	return out, nil
}

// apiAudiencePreview counts who an audience reaches and who will be
// skipped, with Teen badges and a teen count (DEC-20).
func (a *App) apiAudiencePreview(c *gin.Context) {
	var body struct {
		Audience audienceJSON `json:"audience"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		fail(c, errAudienceInvalid)
		return
	}
	s, ok := a.summarizeAudienceFor(c, &body.Audience)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, audienceSummaryResponseJSON{Summary: s})
}

type messageBody struct {
	Channel  string       `json:"channel"`
	Subject  string       `json:"subject"`
	BodyHTML string       `json:"bodyHtml"`
	Audience audienceJSON `json:"audience"`
	// PushAlert is "Push notification too"; missing means on (DEC-123).
	PushAlert *bool `json:"pushAlert"`
}

// apply validates and copies a draft's fields onto m; false means the
// error response was sent. The HTML is sanitized before it is stored.
func (a *App) applyMessageBody(c *gin.Context, me *models.User, m *models.Message) bool {
	var body messageBody
	if err := c.ShouldBindJSON(&body); err != nil {
		jsonError(c, http.StatusBadRequest, "invalid request")
		return false
	}
	if body.Channel == "" {
		body.Channel = "email"
	}
	// Push-only (DEC-123): push is a switch beside the email, never a
	// channel of its own, so asking for channel "push" is refused in
	// words. Any other channel isn't set up yet.
	if strings.EqualFold(strings.TrimSpace(body.Channel), models.DeliveryPush) {
		fail(c, errMessagePushOnly)
		return false
	}
	if body.Channel != "email" {
		fail(c, errMessageChannel)
		return false
	}
	subject := strings.TrimSpace(body.Subject)
	if utf8.RuneCountInString(subject) > maxMessageSubject || strings.ContainsAny(subject, "\r\n") {
		fail(c, errMessageSubject)
		return false
	}
	// Bound the input before sanitizing it, then what is stored: escaping
	// and link addresses can make either longer (the CHECKs count characters).
	if len(body.BodyHTML) > 4*maxMessageHTML {
		fail(c, errMessageBody)
		return false
	}
	clean := content.SanitizeHTML(body.BodyHTML)
	text := content.PlainText(clean)
	if utf8.RuneCountInString(clean) > maxMessageHTML || utf8.RuneCountInString(text) > maxMessageText {
		fail(c, errMessageBody)
		return false
	}
	if !ownPictures(clean, m.ID) {
		fail(c, errMessageImage)
		return false
	}
	if _, ok := a.checkAudience(c, me, &body.Audience); !ok {
		return false
	}
	if err := mergeFieldError(subject+"\n"+clean, body.Audience); err != nil {
		fail(c, err)
		return false
	}
	aud, _ := json.Marshal(body.Audience)
	m.Channel, m.Subject, m.BodyHTML, m.BodyText, m.Audience = body.Channel, subject, clean, text, string(aud)
	// Left out, a new message has it on and a saved one keeps its own.
	switch {
	case body.PushAlert != nil:
		m.PushAlert = *body.PushAlert
	case m.ID == 0:
		m.PushAlert = true
	}
	return true
}

// ownPictures: every picture in sanitized HTML was uploaded to message id.
// The sanitizer keeps only an upload's address; this keeps it to this message's
// own, so a body can't show (or, later, keep alive) another message's picture.
// A message not yet saved (id 0) has none.
func ownPictures(clean string, id uint) bool {
	for _, src := range content.ImageSources(clean) {
		if n, ok := content.ImageMessageID(src); !ok || id == 0 || n != uint64(id) {
			return false
		}
	}
	return true
}

// sendable checks that m could go out now: a subject, a body and at least
// one person who can get it. It answers who it reaches; false means the
// error response was sent.
func (a *App) sendable(c *gin.Context, m *models.Message) (audienceJSON, audienceSummaryJSON, bool) {
	var au audienceJSON
	if m.Subject == "" {
		fail(c, errMessageSubject)
		return au, audienceSummaryJSON{}, false
	}
	if strings.TrimSpace(m.BodyText) == "" {
		fail(c, errMessageBody)
		return au, audienceSummaryJSON{}, false
	}
	_ = json.Unmarshal([]byte(m.Audience), &au)
	s, ok := a.summarizeAudienceFor(c, &au)
	if !ok {
		return au, s, false
	}
	if s.Eligible == 0 {
		fail(c, errMessageNobody)
		return au, s, false
	}
	return au, s, true
}

func (a *App) apiCreateMessage(c *gin.Context) {
	me := apiUser(c)
	if ok, err := a.canCompose(me); err != nil || !ok {
		fail(c, errMessageNotYours)
		return
	}
	m := models.Message{AuthorID: &me.ID, Status: policy.MessageCreateDraft.To(), Category: models.MessageCategoryAnnouncement}
	if !a.applyMessageBody(c, me, &m) {
		return
	}
	pushAlert := m.PushAlert
	err := a.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&m).Error; err != nil {
			return err
		}
		// GORM leaves a false bool with a default out of the INSERT (and
		// reads the column's true back into m).
		if !pushAlert {
			return tx.Model(&models.Message{}).Where("id = ?", m.ID).Update("push_alert", false).Error
		}
		return nil
	})
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "could not save the message")
		return
	}
	m.PushAlert = pushAlert
	a.respondMessage(c, http.StatusCreated, &m)
}

// loadMessage finds a message whose page and history the viewer may see
// (policy.CanSeeMessageHistory): its author's, any for an admin, and an
// automatic reminder for anyone who manages its event. Anyone else gets
// 404; a recipient reads it at /messages/:id/read (apiReadMessage).
func (a *App) loadMessage(c *gin.Context) (*models.Message, bool) {
	me := apiUser(c)
	id, err := strconv.ParseUint(c.Param("id"), 10, 63)
	var m models.Message
	if err != nil || a.DB.First(&m, id).Error != nil {
		jsonError(c, http.StatusNotFound, "message not found")
		return nil, false
	}
	allowed, err := messageHistoryAllowedIn(a.DB, me, &m)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "could not load the message")
		return nil, false
	}
	if !allowed {
		jsonError(c, http.StatusNotFound, "message not found")
		return nil, false
	}
	return &m, true
}

// messageHistoryAllowedIn: me may see m and its history
// (policy.CanSeeMessageHistory), a reminder's management read on db.
func messageHistoryAllowedIn(db *gorm.DB, me *models.User, m *models.Message) (bool, error) {
	p := asPerson(me)
	isAuthor := m.AuthorID != nil && *m.AuthorID == me.ID
	reminder := m.Category == models.MessageCategoryReminder
	manages := false
	if reminder && !p.Admin && m.OccurrenceID != nil {
		var err error
		if manages, err = managesOccurrenceEventIn(db, me, *m.OccurrenceID); err != nil {
			return false, err
		}
	}
	return policy.CanSeeMessageHistory(p, isAuthor, reminder, manages), nil
}

// managesOccurrenceEventIn: me manages the event a date belongs to, by the
// facts the event page uses (policy.CanManageEvent), read on db. A deleted
// event or date is managed by no one.
func managesOccurrenceEventIn(db *gorm.DB, me *models.User, occID uint) (bool, error) {
	var occ models.EventOccurrence
	if err := db.Select("id, event_id").First(&occ, occID).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	var ev models.Event
	if err := db.First(&ev, occ.EventID).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	f, err := eventFactsIn(db, me, &ev)
	if err != nil {
		return false, err
	}
	return f.canManage(me), nil
}

// loadOwnMessage is loadMessage for the composer's own actions (edit,
// schedule, cancel, preview, test): the author or an admin
// (policy.CanEditMessage). An automatic reminder is read only: 409.
func (a *App) loadOwnMessage(c *gin.Context) (*models.Message, bool) {
	m, ok := a.loadMessage(c)
	if !ok {
		return nil, false
	}
	me := apiUser(c)
	reminder := m.Category == models.MessageCategoryReminder
	if !policy.CanEditMessage(asPerson(me), m.AuthorID != nil && *m.AuthorID == me.ID, reminder) {
		if reminder {
			fail(c, errMessageReminder)
		} else {
			jsonError(c, http.StatusNotFound, "message not found")
		}
		return nil, false
	}
	return m, true
}

// messageIDParam is the :id a message route names; a malformed one is
// answered 404, as a message that isn't there is. It judges nothing: the
// reader's snapshot decides the session, the message and the right to it.
func messageIDParam(c *gin.Context) (uint, bool) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 63)
	if err != nil {
		jsonError(c, http.StatusNotFound, "message not found")
		return 0, false
	}
	return uint(id), true
}

// apiGetMessage shows one message to whoever may read its history. The
// session, the message's existence, the right to it and what it shows are all
// decided in one snapshot (presentMessage), so a session revoked after the
// middleware gets 401 whether or not the id is there.
func (a *App) apiGetMessage(c *gin.Context) {
	id, ok := messageIDParam(c)
	if !ok {
		return
	}
	if out, ok := a.presentMessage(c, id, "message"); ok {
		c.JSON(http.StatusOK, messageResponseJSON{Message: out})
	}
}

type messageListJSON struct {
	Messages   []messageJSON `json:"messages"`
	NextCursor string        `json:"nextCursor,omitempty"`
}

type messageListCursor struct {
	UpdatedAt time.Time `json:"updatedAt"`
	ID        uint      `json:"id"`
}

// Keyset pages retain the original author/category boundary on every read.
// Cursors are positions, never authority. Existing clients still get 200 rows.
// The session's user, the author filter their role decides, the rows and
// their facts (authors, counts, a reminder's date) are read in one read-only
// snapshot, so a list never pairs a role with rows or titles committed after
// it was lost.
func (a *App) apiListMessages(c *gin.Context) {
	limit := 200
	if raw := c.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 200 {
			jsonError(c, http.StatusBadRequest, "invalid request")
			return
		}
		limit = n
	}
	var cursor *messageListCursor
	if raw := c.Query("cursor"); raw != "" {
		var cur messageListCursor
		data, err := base64.RawURLEncoding.DecodeString(raw)
		if len(raw) > 512 || err != nil || json.Unmarshal(data, &cur) != nil || cur.ID == 0 || cur.UpdatedAt.IsZero() {
			jsonError(c, http.StatusBadRequest, "invalid request")
			return
		}
		cursor = &cur
	}
	search := strings.TrimSpace(c.Query("search"))
	status := c.Query("status")
	switch status {
	case "", models.MessageDraft, models.MessageScheduled, models.MessageSending, models.MessageSent, models.MessageFailed, models.MessageCancelled:
	default:
		jsonError(c, http.StatusBadRequest, "invalid request")
		return
	}
	category := c.Query("category")
	switch category {
	case "", models.MessageCategoryAnnouncement, models.MessageCategoryReminder:
	default:
		jsonError(c, http.StatusBadRequest, "invalid request")
		return
	}
	var out []messageJSON
	nextCursor := ""
	var me *models.User
	err := a.DB.Transaction(func(tx *gorm.DB) error {
		var err error
		if me, err = snapshotViewerIn(tx, apiUser(c)); err != nil || me == nil {
			return err
		}
		q := tx.Order("updated_at DESC, id DESC").Limit(limit + 1)
		if cursor != nil {
			q = q.Where("(updated_at, id) < (?, ?)", cursor.UpdatedAt, cursor.ID)
		}
		if search != "" {
			q = q.Where("LOWER(subject) LIKE ? ESCAPE '\\'", likeContains(strings.ToLower(search)))
		}
		if status != "" {
			q = q.Where("status = ?", status)
		}
		if category != "" {
			q = q.Where("category = ?", category)
		}
		if !policy.IsAdmin(asPerson(me)) {
			q = q.Where("author_id = ?", me.ID)
		}
		if a.hooks.readerAuthorized != nil {
			a.hooks.readerAuthorized("message list")
		}
		var rows []models.Message
		if err := q.Find(&rows).Error; err != nil {
			return err
		}
		if len(rows) > limit {
			rows = rows[:limit]
			last := rows[len(rows)-1]
			data, _ := json.Marshal(messageListCursor{UpdatedAt: last.UpdatedAt, ID: last.ID})
			nextCursor = base64.RawURLEncoding.EncodeToString(data)
		}
		facts, err := loadMessageFactsIn(tx, rows)
		if err != nil {
			return err
		}
		out = make([]messageJSON, 0, len(rows))
		for i := range rows {
			out = append(out, toMessageJSON(&rows[i], me, facts))
		}
		return nil
	}, readSnapshot)
	switch {
	case me == nil && err == nil:
		fail(c, errSecuritySession)
	case err != nil:
		jsonError(c, http.StatusInternalServerError, "could not list messages")
	default:
		c.JSON(http.StatusOK, messageListJSON{Messages: out, NextCursor: nextCursor})
	}
}

// editable reports whether m can still change; false means 409 was sent.
func editable(c *gin.Context, m *models.Message) bool {
	if !policy.MessageEditable(m.Status) {
		fail(c, errMessageLocked)
		return false
	}
	return true
}

// apiUpdateMessage edits a draft or a scheduled message; the audience is
// checked again, since roles change.
func (a *App) apiUpdateMessage(c *gin.Context) {
	m, ok := a.loadOwnMessage(c)
	if !ok || !editable(c, m) {
		return
	}
	me := apiUser(c)
	if !a.applyMessageBody(c, me, m) {
		return
	}
	fields := map[string]any{"channel": m.Channel, "subject": m.Subject, "body_html": m.BodyHTML, "body_text": m.BodyText,
		"audience": m.Audience, "push_alert": m.PushAlert, "updated_at": time.Now()}
	// A scheduled message stays scheduled only while it could still go out,
	// and its count follows the new audience.
	scheduled := m.Status == models.MessageScheduled
	var (
		au audienceJSON
		s  audienceSummaryJSON
	)
	if scheduled {
		au, s, ok = a.sendable(c, m)
		if !ok {
			return
		}
		fields["recipient_count"] = s.Eligible
	}
	isAuthor := m.AuthorID != nil && *m.AuthorID == me.ID
	err := a.DB.Transaction(func(tx *gorm.DB) error {
		if scheduled {
			// The author's row serializes the daily limit's count with the
			// write (#260); the message is read again under its own lock, and
			// the new audience counts from the date it is now scheduled for.
			fresh, err := lockActorIn(tx, me, actorNoKeyUpdate)
			if err != nil {
				return err
			}
			var cur models.Message
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("id, status, scheduled_at").First(&cur, m.ID).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return errLocked
				}
				return err
			}
			if cur.Status != m.Status || cur.ScheduledAt == nil {
				return errLocked
			}
			if full, err := a.dailyLimitReached(tx, fresh, m.ID, au, *cur.ScheduledAt); err != nil {
				return err
			} else if full {
				return errMessageDailyLimit
			}
			if a.hooks.messageLimitAfterCount != nil {
				a.hooks.messageLimitAfterCount(tx)
			}
		}
		if moved, err := moveMessage(tx, m.ID, policy.MessageEdit, fields, "status = ?", m.Status); err != nil {
			return err
		} else if !moved {
			return errLocked
		}
		// Changing what is already scheduled, or someone else's message, is
		// worth a record; a draft's own edits are not.
		if !scheduled && isAuthor {
			return nil
		}
		return audit.Write(tx, audit.Entry{Actor: &me.ID, Action: audit.MessageEdited, TargetType: "message", TargetID: m.ID,
			Meta: map[string]any{"status": m.Status, "byOther": !isAuthor}, IP: c.ClientIP()})
	})
	if errors.Is(err, errLocked) {
		fail(c, errMessageLocked)
		return
	}
	var ae *apiError
	if errors.As(err, &ae) {
		fail(c, err)
		return
	}
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "could not save the message")
		return
	}
	a.respondMessage(c, http.StatusOK, m)
}

// apiScheduleMessage sets a draft (or a scheduled message) to go out at a
// time, or now. It needs a subject, a body and at least one person who can
// get it; M3.5 sends it.
func (a *App) apiScheduleMessage(c *gin.Context) {
	m, ok := a.loadOwnMessage(c)
	if !ok || !editable(c, m) {
		return
	}
	var body struct {
		ScheduledAt *time.Time `json:"scheduledAt"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		jsonError(c, http.StatusBadRequest, "invalid request")
		return
	}
	now := time.Now()
	when := now
	if body.ScheduledAt != nil {
		when = body.ScheduledAt.UTC()
		if when.Before(now.Add(-time.Minute)) || when.After(now.Add(maxScheduleAhead)) {
			fail(c, errMessageWhen)
			return
		}
		if when.Before(now) {
			when = now
		}
	}
	me := apiUser(c)
	au, s, ok := a.sendable(c, m)
	if !ok {
		return
	}
	err := a.DB.Transaction(func(tx *gorm.DB) error {
		// The author's row serializes the daily limit's count with the write
		// (#260); no messages row is locked or written before it.
		fresh, err := lockActorIn(tx, me, actorNoKeyUpdate)
		if err != nil {
			return err
		}
		var cur models.Message
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("id, status, audience").First(&cur, m.ID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errLocked
			}
			return err
		}
		if !policy.MessageSchedule.Allows(cur.Status) {
			return errLocked
		}
		// The count below judges the audience that was validated.
		if cur.Audience != m.Audience {
			return errMessageChanged
		}
		if full, err := a.dailyLimitReached(tx, fresh, m.ID, au, when); err != nil {
			return err
		} else if full {
			return errMessageDailyLimit
		}
		if a.hooks.messageLimitAfterCount != nil {
			a.hooks.messageLimitAfterCount(tx)
		}
		if moved, err := moveMessage(tx, m.ID, policy.MessageSchedule,
			map[string]any{"scheduled_at": when, "recipient_count": s.Eligible, "updated_at": now}, ""); err != nil {
			return err
		} else if !moved {
			return errLocked
		}
		if err := enqueueMessageSend(tx, m.ID, when); err != nil {
			return err
		}
		return audit.Write(tx, audit.Entry{Actor: &me.ID, Action: audit.MessageScheduled, TargetType: "message", TargetID: m.ID,
			Meta: map[string]any{"audience": au.Type, "recipients": s.Eligible, "at": when}, IP: c.ClientIP()})
	})
	if errors.Is(err, errLocked) {
		fail(c, errMessageLocked)
		return
	}
	var ae *apiError
	if errors.As(err, &ae) {
		fail(c, err)
		return
	}
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "could not schedule the message")
		return
	}
	if out, ok := a.freshMessageJSON(c, m); ok {
		c.JSON(http.StatusOK, messageScheduledResponseJSON{Message: out, Summary: s})
	}
}

var errLocked = errors.New("message locked")

func (a *App) apiCancelMessage(c *gin.Context) {
	m, ok := a.loadOwnMessage(c)
	if !ok || !editable(c, m) {
		return
	}
	me := apiUser(c)
	err := a.DB.Transaction(func(tx *gorm.DB) error {
		if moved, err := moveMessage(tx, m.ID, policy.MessageCancel, map[string]any{"updated_at": time.Now()}, ""); err != nil {
			return err
		} else if !moved {
			return errLocked
		}
		return audit.Write(tx, audit.Entry{Actor: &me.ID, Action: audit.MessageCancelled, TargetType: "message", TargetID: m.ID, IP: c.ClientIP()})
	})
	if errors.Is(err, errLocked) {
		fail(c, errMessageLocked)
		return
	}
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "could not cancel the message")
		return
	}
	// A discarded draft's pictures go with it. A cancelled scheduled message
	// keeps its own: the page still shows it as written.
	if m.Status == models.MessageDraft {
		a.pruneMessageImages(c.Request.Context(), m.ID, "")
	}
	a.respondMessage(c, http.StatusOK, m)
}
