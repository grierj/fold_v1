package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"fold/internal/content"
	"fold/internal/jobs"
	"fold/internal/policy"
	"fold/internal/push"
	"fold/models"

	"gorm.io/gorm"
)

// Announcement pushes (M3.13d2, PLAN 7.1 step 7, DEC-123). An announcement
// always goes by email; with "Push notification too" on (push_alert, the
// default) message.send also snapshots one message_recipients row with
// channel 'push' for each direct recipient who is active and has push on
// for messages, and queues one push.deliver job per device of theirs, in
// the same transaction as the snapshot. Push follows push consent, not
// the email row (DEC-122): someone whose email was skipped for an email
// reason (turned off email from leaders, a bounce, no address) still gets
// the push, and reads the message in Fold (/messages/:id/read, DEC-135).
// Guardian copies, guests, household people and accounts that aren't
// active get no push. The push is a short note: the subject and the first
// 120 characters of the plain text. The push rows never decide the
// message's status or counts: those follow its email rows
// (settleMessage), and "Try again" resends only email.

const (
	// pushWaitPrefix starts a waiting push row's reason; the rest is when
	// quiet hours end, on the recipient's clock: "outside quiet hours ·
	// sending at 8:00 AM" (research 2.6). Use pushWaitReason and
	// pushWaitUntil, which write and read it.
	pushWaitPrefix = "outside quiet hours · sending at "
	// pushWhyStopped: the daily reaper found a push row still queued with
	// no job left to send it.
	pushWhyStopped = "didn't go out"
	// messagePushSnippet is how much of the plain text a push carries.
	messagePushSnippet = 120
	// messagePushOpen ends every announcement push (PLAN 7.3).
	messagePushOpen = "Open to read."
)

// pushReady says who among ids would get an optional push of category
// now: their latest (push, all) consent is opted in, their latest
// (push, category:<category>) consent isn't opted out, and they have at
// least one device (DEC-122). It reads in chunks, like recipientFacts.
func pushReady(db *gorm.DB, ids []uint, category string) (map[uint]bool, error) {
	out := map[uint]bool{}
	err := eachIDChunk(ids, func(chunk []uint) error {
		var on []uint
		if err := db.Raw(`SELECT c.user_id FROM (
				SELECT DISTINCT ON (user_id, scope) user_id, scope, status FROM consents
				WHERE user_id IN ? AND channel = ? AND scope IN ('all', ?) ORDER BY user_id, scope, at DESC, id DESC) c
			WHERE EXISTS (SELECT 1 FROM push_subscriptions d WHERE d.user_id = c.user_id)
			GROUP BY c.user_id
			HAVING bool_or(c.scope = 'all' AND c.status = ?) AND NOT bool_or(c.scope <> 'all' AND c.status = ?)`,
			chunk, models.ConsentPush, categoryScope(category), models.ConsentOptedIn, models.ConsentOptedOut).Scan(&on).Error; err != nil {
			return err
		}
		for _, id := range on {
			out[id] = true
		}
		return nil
	})
	return out, err
}

// pushCandidates are the people among facts who may get a push: active
// accounts. A guest never has a device, and an account that is disabled
// or waiting for consent gets neither the email nor the push. An email
// skip (turned off email from leaders, a bounce, no address) doesn't
// matter here: push follows its own consent (DEC-122).
func pushCandidates(facts []recipientFact) []uint {
	out := []uint{}
	for _, f := range facts {
		if f.User.Status == models.UserActive {
			out = append(out, f.User.ID)
		}
	}
	return out
}

// messagePushRows are message.send's push rows for m: one queued row per
// candidate among facts who has push on for m's category and a device,
// when m asks for it, is an announcement, and the church has push on. A
// date's push reminder is its own message, written by the sweep
// (reminders_push.go).
func (a *App) messagePushRows(db *gorm.DB, m *models.Message, facts []recipientFact) ([]models.MessageRecipient, error) {
	if !m.PushAlert || m.Category != models.MessageCategoryAnnouncement {
		return nil, nil
	}
	if on, err := boolSettingErrIn(db, settingPushEnabled, true); err != nil || !on {
		return nil, err
	}
	ids := pushCandidates(facts)
	if len(ids) == 0 {
		return nil, nil
	}
	on, err := pushReady(db, ids, messageCategory(m))
	if err != nil {
		return nil, err
	}
	rows := []models.MessageRecipient{}
	for _, id := range ids {
		if on[id] {
			rows = append(rows, models.MessageRecipient{MessageID: m.ID, UserID: id, Channel: models.DeliveryPush, Status: policy.RecipientSnapshotQueued.To()})
		}
	}
	return rows, nil
}

// enqueueMessagePushes queues one push.deliver per device of each queued
// push row of the message, in batched INSERTs. Its key
// (push:mr:<row>:d:<device>) makes it once per device, however often it
// runs; the payload holds ids only. A queued row whose devices are all
// gone by now is "device gone" at once, rather than waiting for the
// reaper.
func enqueueMessagePushes(tx *gorm.DB, messageID uint) error {
	var pairs []struct {
		RecipientID uint
		DeviceID    uint
	}
	if err := tx.Raw(`SELECT r.id AS recipient_id, d.id AS device_id FROM message_recipients r
		JOIN push_subscriptions d ON d.user_id = r.user_id
		WHERE r.message_id = ? AND r.channel = ? AND r.status = ? ORDER BY r.id, d.id`,
		messageID, models.DeliveryPush, models.RecipientQueued).Scan(&pairs).Error; err != nil {
		return err
	}
	now := time.Now()
	specs := make([]jobs.Spec, 0, len(pairs))
	for _, p := range pairs {
		specs = append(specs, pushDeliverType.Spec(pushJob{MessageRecipientID: p.RecipientID, DeviceID: p.DeviceID}, now,
			pushRecipientKey(p.RecipientID, p.DeviceID)))
	}
	if _, err := jobs.EnqueueMany(tx, specs); err != nil {
		return err
	}
	_, err := moveRecipientsWhere(tx, policy.RecipientPushNoDevice, map[string]any{"reason": pushWhyGone, "updated_at": gorm.Expr("now()")},
		"r.message_id = ? AND NOT EXISTS (SELECT 1 FROM push_subscriptions d WHERE d.user_id = r.user_id)", messageID)
	return err
}

// messagePushRecorder writes an announcement push's outcome on its row,
// as recordPushIn does for a notification: a sent row is final, and any
// other first outcome stays unless a later device takes it, which makes
// the row sent. Only another device's job going back to wait for quiet
// hours moves one device's miss back to queued (pushWaitIn). status is the
// outcome as the push path shared with notification deliveries says it
// (policy.RecipientPushOutcome).
func messagePushRecorder(id uint) pushRecorder {
	return func(db *gorm.DB, status, reason string, now time.Time) error {
		move, ok := policy.RecipientPushOutcome(status)
		if !ok {
			return fmt.Errorf("push row %d: %q is not a push outcome", id, status)
		}
		fields := map[string]any{"reason": reason, "updated_at": time.Now()}
		if move.To() == models.RecipientSent {
			fields["sent_at"] = now.UTC()
		}
		_, err := moveRecipient(db, id, move, fields, "")
		return err
	}
}

// messagePushColumns are the message columns a push decides and speaks from.
var messagePushColumns = []string{"id", "status", "category", "subject", "body_text", "audience", "occurrence_id", "reminder_key", "scheduled_at"}

// deliverMessagePush sends one announcement push row to one device. It
// checks, in order: the church's switch, the account, the person's push
// consent for the message's category, the device, that the message is
// still going out, quiet hours (announcements are optional, so they
// wait), and the hourly limit, which counts announcement and notification
// pushes together (DEC-126). A push reminder row goes on to
// deliverReminderPush after the device check. It returns an error only
// when a retry may succeed.
func (a *App) deliverMessagePush(ctx context.Context, rowID, deviceID uint, plannedAt int64) error {
	db := a.DB.WithContext(ctx)
	var row models.MessageRecipient
	if err := db.First(&row, rowID).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	if row.Channel != models.DeliveryPush || row.CopyOfUserID != nil {
		return nil
	}
	t := pushTarget{rec: messagePushRecorder(row.ID), row: pushRow{recipientID: row.ID}}
	var u models.User
	if err := db.Select("id", "name", "display_name", "status", "timezone").First(&u, row.UserID).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	var m models.Message
	if err := db.Select(messagePushColumns).First(&m, row.MessageID).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	// These checks only spare quiet hours and the hourly limit a push that
	// can't go; the send's own snapshot reads them again.
	if dest, err := a.pushDestinationIn(db, u.ID, deviceID, messageCategory(&m), false); err != nil {
		return err
	} else if !dest.sends() {
		return a.pushDestOutcome(db, t, dest, deviceID)
	}
	if m.Status != models.MessageSending && m.Status != models.MessageSent {
		return t.record(a, models.RecipientSuppressed, pushWhyStale)
	}
	if m.Category == models.MessageCategoryReminder {
		// The 2-hour push reminder (M3.13d3) has its own checks; the
		// sweep timed it, so it neither waits nor meets the hourly limit.
		return a.deliverReminderPush(ctx, t, &row, &u, &m, deviceID, plannedAt)
	}
	now := a.pushNow()
	loc := userZoneIn(db, u.Timezone)
	if opens, wait := a.pushOpensAt(now, loc); wait {
		if a.hooks.pushStep != nil {
			a.hooks.pushStep("wait")
		}
		if err := db.Transaction(func(tx *gorm.DB) error {
			return pushWaitIn(ctx, tx, row.ID, opens, loc)
		}, pushSettleTx); err != nil {
			return err
		}
		if a.hooks.pushStep != nil {
			a.hooks.pushStep("waited")
		}
		return jobs.Requeued()
	}
	over, err := a.overHourlyLimit(ctx, pushItem{UserID: u.ID, RecipientID: row.ID}, now)
	if err != nil {
		return err
	}
	if over {
		return t.record(a, models.RecipientSuppressed, pushWhyHourly)
	}
	var vals map[string]string
	var skip string
	var dest pushDest
	// The destination, the final decision and the payload's event values
	// come from one read-only snapshot, read after quiet hours and the
	// hourly limit let it go, on the push's own clock; the provider is
	// called after it ends, holding nothing.
	if a.hooks.pushStep != nil {
		a.hooks.pushStep("snapshot")
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		var err error
		if dest, err = a.pushDestinationIn(tx, u.ID, deviceID, messageCategory(&m), false); err != nil || !dest.sends() {
			return err
		}
		if a.hooks.pushStep != nil {
			a.hooks.pushStep("destination")
		}
		// The message again, in this snapshot: Try again moves an
		// announcement's scheduled_at (its series cutoff) without touching
		// its push rows, so the cutoff read before quiet hours or the
		// hourly limit may be stale. Email and the read page count from the
		// current one, and so does the push.
		var cur models.Message
		if err := tx.Select(messagePushColumns).Where("id = ?", row.MessageID).Limit(1).Find(&cur).Error; err != nil {
			return err
		}
		if cur.ID == 0 || (cur.Status != models.MessageSending && cur.Status != models.MessageSent) {
			skip = pushWhyStale
			return nil
		}
		m = cur
		skip, vals, err = a.announcementPushIn(tx, &m, &dest.user, a.pushNow().UTC())
		return err
	}, readSnapshot); err != nil {
		return err
	}
	if !dest.sends() {
		return a.pushDestOutcome(db, t, dest, deviceID)
	}
	if skip != "" {
		return t.record(a, models.RecipientSuppressed, skip)
	}
	// The snapshot let the person through the event audience's date check:
	// recorded before the provider call, apart from the row's status, so a
	// later outcome on this or another device keeps it (isMessageRecipientIn).
	if err := markPushAdmitted(db, &m, row.ID); err != nil {
		return err
	}
	t.dev = dest.dev
	title, text := messagePushText(&m, vals)
	t.what = fmt.Sprintf("message %d recipient %d", m.ID, row.ID)
	body, err := push.MarshalPayload(push.NewPayload(push.Notification{
		Title: title, Body: text, Navigate: a.emailLink(messageReadPath(m.ID)), Tag: "m" + idString(m.ID),
	}))
	if err != nil {
		return t.tooLarge(a)
	}
	return a.sendPush(ctx, t, body, recipientPushOptions(row.ID, pushTTL), "")
}

// markPushAdmitted records, once, that an announcement push row's person
// passed its event audience's date check (audience_admitted_at). Any other
// audience has no such check, and its rows record nothing.
func markPushAdmitted(db *gorm.DB, m *models.Message, rowID uint) error {
	var au audienceJSON
	if err := json.Unmarshal([]byte(m.Audience), &au); err != nil || !eventAudience(au.Type) {
		return err
	}
	return db.Model(&models.MessageRecipient{}).Where("id = ? AND channel = ? AND audience_admitted_at IS NULL", rowID, models.DeliveryPush).
		Update("audience_admitted_at", time.Now().UTC()).Error
}

// announcementPushIn decides, on tx (the push's snapshot), whether m's push
// still goes to the user (why not, else "") and with which merge values. An
// event audience's push goes only while a date the person may still see puts
// them in it (audienceSkip), as the email send decides. The lock screen never carries a
// link, so only the first name and the event's title and time matter; the
// live access checks behind those run only when the subject or text uses one,
// and fill only what the person may see of the event or date now
// (message_event_visibility.go). live is the person as the same snapshot
// read them (pushDestinationIn). An event audience's dates count from the
// message's stored scheduled time (messageSince), as email and the read page
// count them.
func (a *App) announcementPushIn(tx *gorm.DB, m *models.Message, live *models.User, now time.Time) (string, map[string]string, error) {
	var au audienceJSON
	_ = json.Unmarshal([]byte(m.Audience), &au)
	if eventAudience(au.Type) {
		seers, err := a.audienceSeers(tx, au, []uint{live.ID}, messageSince(m, now), now)
		if err != nil {
			return "", nil, err
		}
		if why := audienceSkip(seers, live.ID); why != "" {
			return why, nil, nil
		}
	}
	needsEvent := false
	for _, f := range content.FieldsIn(m.Subject + "\n" + m.BodyText) {
		if f == content.FieldEventTitle || f == content.FieldEventTime {
			needsEvent = true
		}
	}
	if !needsEvent {
		return "", map[string]string{content.FieldFirstName: firstName(live.PublicName())}, nil
	}
	vals, err := a.readerEventValues(tx, live, au, now)
	return "", vals, err
}

// pushWaitIn, on a pushSettleTx, says the row waits for quiet hours to end,
// on the recipient's clock, for the history, and queues this device's job
// again for then (jobs.LaterIn) in the same commit; when its claim is no
// longer current, nothing commits. The account is locked first (FOR
// SHARE, in the account-first order erasure and status changes take), so
// the person's status is read after any erasure or status change that holds
// it has committed. The row's lock then orders it with another device's
// settlement (settlePushMissIn): one that settles after sees this job queued
// and leaves the row waiting; one that settled first recorded a miss about
// its own device alone (pushDeviceMisses), which goes back to waiting while
// the person is still active. A sent row, and any other outcome, stays.
func pushWaitIn(ctx context.Context, tx *gorm.DB, rowID uint, opens time.Time, loc *time.Location) error {
	var account []uint
	if err := tx.Raw(`SELECT u.id FROM users u JOIN message_recipients r ON r.user_id = u.id WHERE r.id = ? FOR SHARE OF u`, rowID).
		Scan(&account).Error; err != nil {
		return err
	}
	if _, err := moveRecipient(tx, rowID, policy.RecipientPushWaits, map[string]any{"reason": pushWaitReason(opens, loc), "updated_at": gorm.Expr("now()")},
		"r.status = ? OR (r.reason IN ? AND EXISTS (SELECT 1 FROM users u WHERE u.id = r.user_id AND u.status = ?))",
		models.RecipientQueued, pushDeviceMisses, models.UserActive); err != nil {
		return err
	}
	return jobs.LaterIn(ctx, tx, opens)
}

// pushWaitReason is a waiting push row's reason: when quiet hours end on the
// recipient's clock.
func pushWaitReason(opens time.Time, loc *time.Location) string {
	return pushWaitPrefix + opens.In(loc).Format("3:04 PM")
}

func pushWaitUntil(reason string) string { return strings.TrimPrefix(reason, pushWaitPrefix) }

func messageReadPath(id uint) string { return "/messages/" + idString(id) + "/read" }

var (
	// A link's address after its text in the plain-text part, as
	// content.PlainText writes it: " (https://…)" or " ({{event_link}})".
	pushLinkAddressRe = regexp.MustCompile(`\s*\((?:https?://|\{\{)[^()\s]*\)`)
	pushBareURLRe     = regexp.MustCompile(`(?i)\b(?:https?://|www\.)\S+`)
	// Contact details (DEC-124): an email address, with or without
	// mailto:, and a phone number.
	pushEmailRe = regexp.MustCompile(`(?i)(?:\bmailto:\S*)|(?:[a-z0-9._%+-]+@[a-z0-9-]+(?:\.[a-z0-9-]+)+)`)
	// pushPhoneRe matches phone-shaped runs only: "+44 20 7946 0958" (a
	// leading + with groups of 1–4 digits), "(208) 555-0123",
	// "1-208-555-0123" and "555-0123" (3-3-4 or 3-4 groups joined by one
	// space, dot or dash), and ten unbroken digits. Room lists, hymn
	// numbers and years ("101 102 103", "12 45 301", "2026 2027") have no
	// such shape and stay; lockScreenSafe also checks for 7 to 15 digits.
	pushPhoneRe = regexp.MustCompile(`\+\d{1,3}(?:[ .-]\(?\d{1,4}\)?){1,5}` +
		`|\(\d{3}\)[ .-]?\d{3}[ .-]\d{4}\b` +
		`|\b(?:1[ .-])?\d{3}[ .-]\d{3}[ .-]\d{4}\b` +
		`|\b\d{3}[ .-]\d{4}\b` +
		`|\b1?\d{10}\b`)
	// pushDateTimeRe is an ISO date (with its time, if any) or a clock
	// time: never a phone number, so the phone pass skips it.
	pushDateTimeRe = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}(?:[T ]\d{1,2}:\d{2}(?::\d{2})?)?\b|\b\d{1,2}:\d{2}(?::\d{2})?\b`)
)

// lockScreenSafe takes out of s what a lock screen must never show
// (DEC-124): link addresses, URLs, email addresses and phone-shaped
// numbers, leaving dates, times and other numbers. It is best effort: a
// number spelled out in words, or written in an odd shape, isn't caught.
func lockScreenSafe(s string) string {
	s = pushLinkAddressRe.ReplaceAllString(s, "")
	s = pushBareURLRe.ReplaceAllString(s, "")
	s = pushEmailRe.ReplaceAllString(s, "")
	// The phone pass runs only between the dates and times.
	var b strings.Builder
	at := 0
	for _, span := range pushDateTimeRe.FindAllStringIndex(s, -1) {
		b.WriteString(stripPhones(s[at:span[0]]))
		b.WriteString(s[span[0]:span[1]])
		at = span[1]
	}
	b.WriteString(stripPhones(s[at:]))
	return b.String()
}

// stripPhones removes the phone-shaped runs in s that hold 7 to 15 digits.
func stripPhones(s string) string {
	return pushPhoneRe.ReplaceAllStringFunc(s, func(run string) string {
		digits := 0
		for _, r := range run {
			if r >= '0' && r <= '9' {
				digits++
			}
		}
		if digits < 7 || digits > 15 {
			return run
		}
		return ""
	})
}

// messagePushText is an announcement's lock-screen text for one reader
// (DEC-124, research 2.4): the subject as their email says it, and the
// first 120 characters of the plain text then "Open to read.". It is never
// the HTML, and carries no link or contact detail of any kind: link
// addresses, URLs, the link fields, email addresses and phone numbers are
// left out, so no signed or manage link can reach a lock screen. vals are
// the reader's merge values; any field without one (a link field, a stray
// field) becomes nothing.
func messagePushText(m *models.Message, vals map[string]string) (title, body string) {
	text := map[string]string{}
	for k, v := range vals {
		if k != content.FieldEventLink && k != content.FieldManageLink {
			text[k] = v
		}
	}
	oneLine := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	title = clipRunes(oneLine(lockScreenSafe(oneLine(content.Merge(m.Subject, text, false)))), maxMessageSubject)
	if title == "" {
		title = "New message"
	}
	// Link addresses go before the merge, while a link field still reads as
	// one; lines are joined before the contact pass, so a number split
	// across lines is caught.
	s := oneLine(lockScreenSafe(oneLine(content.Merge(pushLinkAddressRe.ReplaceAllString(m.BodyText, ""), text, false))))
	if r := []rune(s); len(r) > messagePushSnippet {
		cut := string(r[:messagePushSnippet])
		if i := strings.LastIndex(cut, " "); i > messagePushSnippet/2 {
			cut = cut[:i]
		}
		s = strings.TrimRight(cut, " ,;:.-–—") + "…"
	}
	if s == "" {
		return title, messagePushOpen
	}
	return title, s + "\n" + messagePushOpen
}

// reapMessagePushes is the daily backstop for announcement pushes: a
// push row still queued an hour on with no push.deliver job left for it
// (its devices were added after the snapshot, or its job stopped) is
// suppressed, so a history never waits forever.
func reapMessagePushes(db *gorm.DB, now time.Time) (int64, error) {
	return moveRecipientsWhere(db, policy.RecipientPushReaped, map[string]any{"reason": pushWhyStopped, "updated_at": now},
		`r.updated_at < ? AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.kind = ? AND j.status IN ('queued', 'running', 'failed')
			AND (j.payload->>?)::bigint = r.id)`,
		now.Add(-time.Hour), kindPushDeliver, pushJobRecipientField)
}
