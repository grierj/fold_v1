package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"fold/internal/notify"
	"fold/internal/policy"
	"fold/models"

	"gorm.io/gorm"
)

// pushRowsOf is a message's push rows by the recipient's address.
func pushRowsOf(t *testing.T, app *App, id uint) map[string]models.MessageRecipient {
	t.Helper()
	var rows []models.MessageRecipient
	app.DB.Where("message_id = ? AND channel = ?", id, models.DeliveryPush).Find(&rows)
	out := map[string]models.MessageRecipient{}
	for _, r := range rows {
		var u models.User
		app.DB.First(&u, r.UserID)
		if r.CopyOfUserID != nil {
			t.Fatalf("a guardian copy has a push row: %+v", r)
		}
		out[u.Email] = r
	}
	return out
}

func outboxCount(app *App) int64 {
	var n int64
	app.DB.Model(&models.MailOutbox{}).Count(&n)
	return n
}

func groupAudience(f composeFixture) map[string]any {
	return map[string]any{"type": "group", "groupId": f.group.ID}
}

// One email and one push for each recipient with a device, even across a
// restart (M3.13 acceptance): the teen's two devices each get the push
// once and it is recorded once; the leader, push on but no device, gets
// the email only and no row; the message's own count and status follow
// the email. Running every job again sends nothing twice.
func TestAnnouncementSendsOneEmailAndOnePush(t *testing.T) {
	f := newComposeFixture(t)
	fp := useFakePush(t, f.app)
	pushConsentRow(t, f.app, f.teen, "all", models.ConsentOptedIn)
	pushConsentRow(t, f.app, f.leaderUser, "all", models.ConsentOptedIn)
	phone, laptop := newPushDevice(t, f.app, f.teen, "phone"), newPushDevice(t, f.app, f.teen, "laptop")
	id := f.scheduleNow(t, f.leader, "Snow day", "<p>Hi {{first_name}}, no meeting tonight.</p>", groupAudience(f))
	runMessageJobs(t, f.app)

	var m models.Message
	f.app.DB.First(&m, id)
	if m.Status != models.MessageSent || m.RecipientCount != 2 || !m.PushAlert {
		t.Fatalf("message: %+v", m)
	}
	emails := recipientsOf(t, f.app, id)
	if emails[f.teen.Email].Status != models.RecipientSent || emails[f.leaderUser.Email].Status != models.RecipientSent {
		t.Fatalf("emails: %+v", emails)
	}
	pushes := pushRowsOf(t, f.app, id)
	if len(pushes) != 1 || pushes[f.teen.Email].Status != models.RecipientSent || pushes[f.teen.Email].SentAt == nil || pushes[f.teen.Email].Reason != "" {
		t.Fatalf("push rows: %+v", pushes)
	}
	if fp.count(phone.path) != 1 || fp.count(laptop.path) != 1 || fp.total() != 2 {
		t.Fatalf("pushes: %v", fp.sent)
	}
	// One job per device, keyed by row and device, with ids only.
	var js []models.Job
	f.app.DB.Where("kind = ?", kindPushDeliver).Order("id").Find(&js)
	if len(js) != 2 {
		t.Fatalf("%d push jobs", len(js))
	}
	want := map[string]bool{}
	for _, d := range []testDevice{phone, laptop} {
		want[fmt.Sprintf("push:mr:%d:d:%d", pushes[f.teen.Email].ID, d.ID)] = true
	}
	for _, j := range js {
		if j.UniqueKey == nil || !want[*j.UniqueKey] || strings.Contains(j.Payload, "fcm") || strings.Contains(j.Payload, "secret") {
			t.Fatalf("job %v %s", j.UniqueKey, j.Payload)
		}
		delete(want, *j.UniqueKey)
	}
	// A restart: message.send again, and the push jobs queued again.
	before := outboxCount(f.app)
	if _, err := enqueueJob(f.app.DB, kindMessageSend, messageSendJob{MessageID: id}, time.Now(), "", 0); err != nil {
		t.Fatal(err)
	}
	if err := enqueueMessagePushes(f.app.DB, id); err != nil {
		t.Fatal(err)
	}
	for _, d := range []testDevice{phone, laptop} {
		if added, err := enqueueJob(f.app.DB, kindPushDeliver, pushJob{MessageRecipientID: pushes[f.teen.Email].ID, DeviceID: d.ID}, time.Now(),
			fmt.Sprintf("push:mr:%d:d:%d", pushes[f.teen.Email].ID, d.ID), 0); err != nil || added {
			t.Fatalf("queued twice: %v %v", added, err)
		}
	}
	runMessageJobs(t, f.app)
	if outboxCount(f.app) != before || fp.total() != 2 || len(pushRowsOf(t, f.app, id)) != 1 {
		t.Fatalf("sent again: emails %d→%d, pushes %v", before, outboxCount(f.app), fp.sent)
	}
}

// The push is the subject and the first 120 characters of the plain text,
// as the reader's email says them, and opens the message in Fold. No link
// of any kind reaches the lock screen.
func TestMessagePushText(t *testing.T) {
	m := &models.Message{ID: 7, Subject: "Hi {{first_name}}: snow day",
		BodyText: "No meeting tonight, {{first_name}}. Details (https://example.org/x?token=abc) and your date ({{manage_link}}) " +
			"or www.example.com/page. " + strings.Repeat("word ", 40)}
	title, body := messagePushText(m, map[string]string{"first_name": "Sam", "manage_link": "https://fold.example/events/1/dates/2026-11-05"})
	if title != "Hi Sam: snow day" {
		t.Fatalf("title %q", title)
	}
	snippet, open, ok := strings.Cut(body, "\n")
	if !ok || open != messagePushOpen {
		t.Fatalf("body %q", body)
	}
	if !strings.HasPrefix(snippet, "No meeting tonight, Sam. Details and your date or ") || !strings.HasSuffix(snippet, "…") ||
		len([]rune(snippet)) > messagePushSnippet+1 {
		t.Fatalf("snippet %q (%d)", snippet, len([]rune(snippet)))
	}
	for _, bad := range []string{"http", "www.", "token", "{{", "fold.example"} {
		if strings.Contains(body, bad) || strings.Contains(title, bad) {
			t.Fatalf("%q leaked: %q / %q", bad, title, body)
		}
	}
	if _, body := messagePushText(&models.Message{Subject: "Short"}, nil); body != messagePushOpen {
		t.Fatalf("an empty body: %q", body)
	}
}

// No contact detail reaches a lock screen (DEC-124): email addresses (with
// or without mailto:) and phone numbers are taken out of the subject and
// the text; dates, times and ordinary numbers stay.
func TestMessagePushTextLeavesOutContactDetails(t *testing.T) {
	m := &models.Message{Subject: "Questions? pat@example.org or 555-123-4567",
		BodyText: "Write to Pat.Lee+church@mail.example.co.uk, mailto:office@example.org or call (208) 555-0199, +44 20 7946 0958 or 555 0100. " +
			"We meet 2026-11-05 at 6:45 in Room 104 with 12 people."}
	title, body := messagePushText(m, nil)
	for _, bad := range []string{"@", "mailto", "555", "0199", "7946", "0100"} {
		if strings.Contains(title, bad) || strings.Contains(body, bad) {
			t.Fatalf("%q reached the lock screen: %q / %q", bad, title, body)
		}
	}
	for _, keep := range []string{"Write to", "2026-11-05", "6:45", "Room 104", "12 people"} {
		if !strings.Contains(body, keep) {
			t.Fatalf("%q was taken out: %q", keep, body)
		}
	}
	if title != "Questions? or" {
		t.Fatalf("title %q", title)
	}
}

// Push follows push consent, not the email row (DEC-122): someone who
// turned off email from leaders, or whose email bounces, still gets the
// push when push is on, and no email. Someone with push off for messages
// from leaders gets the email only; a guardian's copy gets no push;
// push_alert off sends none; and with the church's switch off no push row
// is recorded at all.
func TestAnnouncementPushSkips(t *testing.T) {
	f := newComposeFixture(t)
	fp := useFakePush(t, f.app)
	for _, u := range []*models.User{f.teen, f.opted, f.bouncing} {
		pushConsentRow(t, f.app, u, "all", models.ConsentOptedIn)
		newPushDevice(t, f.app, u, u.Name+"-phone")
	}
	// The leader turned push off for messages from leaders.
	pushConsentRow(t, f.app, f.leaderUser, "all", models.ConsentOptedIn)
	pushConsentRow(t, f.app, f.leaderUser, "category:"+notify.CatMessages, models.ConsentOptedOut)
	newPushDevice(t, f.app, f.leaderUser, "leader-phone")
	// The teen's guardian has push on, but copies are email only.
	parent := pushMember(t, f.app, "parent")
	newPushDevice(t, f.app, parent, "parent-phone")
	f.app.DB.Create(&models.GuardianLink{GuardianUserID: parent.ID, TeenUserID: &f.teen.ID, Status: models.GuardianConfirmed})
	f.app.DB.Create(&models.Setting{Key: settingGuardianCopies, Value: "true"})

	id := f.scheduleNow(t, f.leader, "One", "<p>Hi</p>", groupAudience(f))
	runMessageJobs(t, f.app)
	pushes := pushRowsOf(t, f.app, id)
	if len(pushes) != 3 || pushes[f.teen.Email].Status != models.RecipientSent || pushes[f.opted.Email].Status != models.RecipientSent ||
		pushes[f.bouncing.Email].Status != models.RecipientSent {
		t.Fatalf("push rows: %+v", pushes)
	}
	emails := recipientsOf(t, f.app, id)
	if emails[f.leaderUser.Email].Status != models.RecipientSent || emails["copy:"+parent.Email].Status != models.RecipientSent ||
		emails[f.opted.Email].Status != models.RecipientSuppressed || emails[f.bouncing.Email].Status != models.RecipientSuppressed {
		t.Fatalf("emails: %+v", emails)
	}
	if _, err := devOutbox(t, f.app).LatestTo(f.opted.Email); err == nil {
		t.Fatal("someone who turned off email from leaders got the email")
	}
	if fp.total() != 3 {
		t.Fatalf("pushes %v", fp.sent)
	}
	// The history shows it on the person's own row: missed the email,
	// pushed. Each person once, guardian copies apart.
	var h messageRecipientsJSON
	decodeJSON(t, f.admin.do("GET", "/api/v1/messages/"+itoa(id)+"/recipients", nil, false), &h)
	for _, r := range h.Recipients {
		if r.Name == f.opted.PublicName() && (r.Status != historyMissed || r.Push == nil || r.Push.Status != historySent) {
			t.Fatalf("opted: %+v", r)
		}
		if r.CopyOf != "" && r.Push != nil {
			t.Fatalf("a copy shows a push: %+v", r)
		}
	}
	if len(h.Recipients) != 5 || h.Counts.Push.Sent != 3 {
		t.Fatalf("history: %+v", h)
	}

	// push_alert off: email only.
	var out struct {
		Message messageJSON `json:"message"`
	}
	w := f.leader.do("POST", "/api/v1/messages", map[string]any{"subject": "Two", "bodyHtml": "<p>Hi</p>", "audience": groupAudience(f), "pushAlert": false}, true)
	if w.Code != http.StatusCreated {
		t.Fatalf("draft: %d %s", w.Code, w.Body.String())
	}
	decodeJSON(t, w, &out)
	if out.Message.PushAlert {
		t.Fatal("pushAlert false was saved as true")
	}
	if w := f.leader.do("POST", "/api/v1/messages/"+itoa(out.Message.ID)+"/schedule", confirmedSend(t, f.leader, "/api/v1/messages/"+itoa(out.Message.ID), map[string]any{}), true); w.Code != http.StatusOK {
		t.Fatalf("schedule: %d", w.Code)
	}
	runMessageJobs(t, f.app)
	if rows := pushRowsOf(t, f.app, out.Message.ID); len(rows) != 0 || recipientsOf(t, f.app, out.Message.ID)[f.teen.Email].Status != models.RecipientSent {
		t.Fatalf("push_alert off: %+v", rows)
	}

	// The church's switch off: no push row recorded.
	f.app.DB.Create(&models.Setting{Key: settingPushEnabled, Value: "false"})
	off := f.scheduleNow(t, f.leader, "Three", "<p>Hi</p>", groupAudience(f))
	runMessageJobs(t, f.app)
	if rows := pushRowsOf(t, f.app, off); len(rows) != 0 || fp.total() != 3 {
		t.Fatalf("church off: %+v, pushes %v", rows, fp.sent)
	}
	var m models.Message
	if f.app.DB.First(&m, off); m.Status != models.MessageSent {
		t.Fatalf("church off: %+v", m)
	}
}

// Turned off by the church after the snapshot: the waiting push is
// recorded "push turned off by the church", never sent.
func TestAnnouncementPushChecksTheChurchSwitchAtSendTime(t *testing.T) {
	f := newComposeFixture(t)
	fp := useFakePush(t, f.app)
	pushConsentRow(t, f.app, f.teen, "all", models.ConsentOptedIn)
	newPushDevice(t, f.app, f.teen, "phone")
	id := f.scheduleNow(t, f.leader, "One", "<p>Hi</p>", groupAudience(f))
	// Prepare only, then turn push off before the device's job runs.
	if err := f.app.prepareMessageOnce(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	f.app.DB.Create(&models.Setting{Key: settingPushEnabled, Value: "false"})
	runMessageJobs(t, f.app)
	if r := pushRowsOf(t, f.app, id)[f.teen.Email]; r.Status != models.RecipientSuppressed || r.Reason != pushWhyChurchOff || fp.total() != 0 {
		t.Fatalf("row %+v, pushes %v", r, fp.sent)
	}
}

// At 11 PM the push waits for 8 AM on the recipient's clock: the message
// is sent (the email went), the push row stays queued with when it goes,
// and the history says so. In the morning it sends once. A cancelled
// message's waiting push doesn't send.
func TestAnnouncementPushWaitsForTheMorning(t *testing.T) {
	f := newComposeFixture(t)
	fp := useFakePush(t, f.app)
	clock := quietHours(f.app)
	pushConsentRow(t, f.app, f.teen, "all", models.ConsentOptedIn)
	f.app.DB.Model(f.teen).Update("timezone", "America/Denver")
	phone := newPushDevice(t, f.app, f.teen, "phone")
	id := f.scheduleNow(t, f.leader, "Late", "<p>Hi</p>", groupAudience(f))
	runMessageJobs(t, f.app)
	var m models.Message
	f.app.DB.First(&m, id)
	row := pushRowsOf(t, f.app, id)[f.teen.Email]
	if m.Status != models.MessageSent || row.Status != models.RecipientQueued || row.Reason != "outside quiet hours · sending at 8:00 AM" || fp.total() != 0 {
		t.Fatalf("message %s, row %+v, pushes %v", m.Status, row, fp.sent)
	}
	var h messageRecipientsJSON
	w := f.leader.do("GET", "/api/v1/messages/"+itoa(id)+"/recipients", nil, false)
	decodeJSON(t, w, &h)
	if h.Counts.Push.Sent != 0 || len(h.Counts.Push.Waiting) != 1 || h.Counts.Push.Waiting[0] != (pushWaitJSON{Until: "8:00 AM", N: 1}) ||
		h.Counts.Sent != 2 || h.Counts.Queued != 0 || h.CanRetry {
		t.Fatalf("history counts: %+v", h.Counts)
	}
	// One row a person: the teen's push is on their own row, with when it
	// goes (as the counts say it) and no reason for a leader.
	waiting := 0
	for _, r := range h.Recipients {
		if r.Push != nil {
			waiting++
			if r.Name != f.teen.PublicName() || *r.Push != (historyPushJSON{Status: historyWaiting, Until: "8:00 AM"}) || r.Status != historySent {
				t.Fatalf("push on %+v: %+v", r, *r.Push)
			}
		}
	}
	if waiting != 1 || len(h.Recipients) != 4 {
		t.Fatalf("history rows: %+v", h.Recipients)
	}
	clock.set(8, 0, 16)
	makeDue(f.app)
	drainJobs(t, f.app)
	if r := pushRowsOf(t, f.app, id)[f.teen.Email]; r.Status != models.RecipientSent || r.Reason != "" || fp.count(phone.path) != 1 {
		t.Fatalf("in the morning: %+v, pushes %v", r, fp.sent)
	}

	// Cancelled while it waits: nothing goes.
	clock.set(23, 0, 16)
	late := f.scheduleNow(t, f.leader, "Later", "<p>Hi</p>", groupAudience(f))
	runMessageJobs(t, f.app)
	f.app.DB.Model(&models.Message{}).Where("id = ?", late).Update("status", models.MessageCancelled)
	clock.set(8, 0, 17)
	makeDue(f.app)
	drainJobs(t, f.app)
	if r := pushRowsOf(t, f.app, late)[f.teen.Email]; r.Status != models.RecipientSuppressed || r.Reason != pushWhyStale || fp.count(phone.path) != 1 {
		t.Fatalf("cancelled: %+v, pushes %v", r, fp.sent)
	}
}

// The hourly limit counts announcement and notification pushes together:
// 19 notifications and one announcement fill the hour, so the next
// notification is held back, and so is the next announcement.
func TestPushHourlyLimitCountsAnnouncements(t *testing.T) {
	f := newComposeFixture(t)
	fp := useFakePush(t, f.app)
	clock := quietHours(f.app)
	clock.set(10, 0, 16)
	pushConsentRow(t, f.app, f.teen, "all", models.ConsentOptedIn)
	f.app.DB.Model(f.teen).Update("timezone", "America/Denver")
	phone := newPushDevice(t, f.app, f.teen, "phone")
	for i := 0; i < pushHourlyLimit-1; i++ {
		n := sendNotice(t, f.app, notify.GroupApproved, f.teen)
		if err := recordPushIn(f.app.DB, models.Notification{ID: n, UserID: f.teen.ID}, models.DeliverySent, "", clock.at); err != nil {
			t.Fatal(err)
		}
	}
	// Those aggregate rows represent past sends. Settle their new outbox
	// jobs too, so the message runner cannot fan them out as fresh notices.
	if err := f.app.DB.Model(&models.Job{}).Where("kind = ?", notify.DeliverKind).
		Update("status", models.JobDone).Error; err != nil {
		t.Fatal(err)
	}

	first := f.scheduleNow(t, f.leader, "Twentieth", "<p>Hi</p>", groupAudience(f))
	runMessageJobs(t, f.app)
	if r := pushRowsOf(t, f.app, first)[f.teen.Email]; r.Status != models.RecipientSent || fp.count(phone.path) != 1 {
		t.Fatalf("the 20th: %+v", r)
	}
	n := sendNotice(t, f.app, notify.GroupApproved, f.teen)
	runPushJobs(t, f.app, n, phone)
	if status, reason := pushDelivery(t, f.app, n); status != models.DeliverySuppressed || reason != pushWhyHourly {
		t.Fatalf("a notification past the limit: %q %q", status, reason)
	}
	second := f.scheduleNow(t, f.leader, "Twenty-first", "<p>Hi</p>", groupAudience(f))
	runMessageJobs(t, f.app)
	if r := pushRowsOf(t, f.app, second)[f.teen.Email]; r.Status != models.RecipientSuppressed || r.Reason != pushWhyHourly || fp.count(phone.path) != 1 {
		t.Fatalf("an announcement past the limit: %+v, pushes %v", r, fp.sent)
	}
}

// "Try again" resends failed emails only: someone already pushed is never
// pushed again, and push rows never offer it.
func TestRetryNeverPushesAgain(t *testing.T) {
	f := newComposeFixture(t)
	fp := useFakePush(t, f.app)
	pushConsentRow(t, f.app, f.teen, "all", models.ConsentOptedIn)
	newPushDevice(t, f.app, f.teen, "phone")
	id := f.scheduleNow(t, f.leader, "Once", "<p>Hi</p>", groupAudience(f))
	runMessageJobs(t, f.app)
	// The leader's email failed.
	f.app.DB.Model(&models.MessageRecipient{}).Where("message_id = ? AND user_id = ? AND channel = ?", id, f.leaderUser.ID, models.DeliveryEmail).
		Updates(map[string]any{"status": models.RecipientFailed, "error": whyProvider})
	var h messageRecipientsJSON
	decodeJSON(t, f.leader.do("GET", "/api/v1/messages/"+itoa(id)+"/recipients", nil, false), &h)
	if !h.CanRetry || h.Counts.Failed != 1 || h.Counts.Push.Sent != 1 {
		t.Fatalf("history: %+v", h.Counts)
	}
	if w := f.leader.do("POST", "/api/v1/messages/"+itoa(id)+"/retry", map[string]any{}, true); w.Code != http.StatusOK {
		t.Fatalf("retry: %d %s", w.Code, w.Body.String())
	}
	runMessageJobs(t, f.app)
	if fp.total() != 1 || len(pushRowsOf(t, f.app, id)) != 1 {
		t.Fatalf("pushed again: %v", fp.sent)
	}
	// A failed push alone is nothing to retry.
	f.app.DB.Model(&models.MessageRecipient{}).Where("message_id = ? AND channel = ?", id, models.DeliveryPush).Update("status", models.RecipientFailed)
	decodeJSON(t, f.leader.do("GET", "/api/v1/messages/"+itoa(id)+"/recipients", nil, false), &h)
	if h.CanRetry || h.Counts.Failed != 0 || h.Counts.Push.Missed[""] != 1 {
		t.Fatalf("a failed push offers Try again: %+v", h)
	}
}

// The daily reaper stops a push row that no job will send (its device
// was added after the snapshot), so a history never waits forever.
func TestReaperStopsAPushWithNoJob(t *testing.T) {
	f := newComposeFixture(t)
	useFakePush(t, f.app)
	id := f.scheduleNow(t, f.leader, "Once", "<p>Hi</p>", groupAudience(f))
	runMessageJobs(t, f.app)
	row := models.MessageRecipient{MessageID: id, UserID: f.teen.ID, Channel: models.DeliveryPush, Status: models.RecipientQueued,
		UpdatedAt: time.Now().Add(-2 * time.Hour)}
	if err := f.app.DB.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	waiting := models.MessageRecipient{MessageID: id, UserID: f.leaderUser.ID, Channel: models.DeliveryPush, Status: models.RecipientQueued,
		UpdatedAt: time.Now().Add(-2 * time.Hour)}
	if err := f.app.DB.Create(&waiting).Error; err != nil {
		t.Fatal(err)
	}
	// This one's job is still waiting for the morning.
	if _, err := enqueueJob(f.app.DB, kindPushDeliver, pushJob{MessageRecipientID: waiting.ID, DeviceID: 1}, time.Now().Add(8*time.Hour), "", 0); err != nil {
		t.Fatal(err)
	}
	if err := f.app.reapMessages(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	var got, kept models.MessageRecipient
	f.app.DB.First(&got, row.ID)
	f.app.DB.First(&kept, waiting.ID)
	if got.Status != models.RecipientSuppressed || got.Reason != pushWhyStopped || kept.Status != models.RecipientQueued {
		t.Fatalf("reaped %+v, kept %+v", got, kept)
	}
}

// A queued push row whose person has no device left when the jobs are
// queued is "device gone" at once, not left waiting for the reaper; one
// with devices gets a job per device.
func TestQueuingPushesSettlesARowWithNoDevice(t *testing.T) {
	f := newComposeFixture(t)
	useFakePush(t, f.app)
	id := f.scheduleNow(t, f.leader, "Once", "<p>Hi</p>", groupAudience(f))
	phone, laptop := newPushDevice(t, f.app, f.teen, "phone"), newPushDevice(t, f.app, f.teen, "laptop")
	withDevices := models.MessageRecipient{MessageID: id, UserID: f.teen.ID, Channel: models.DeliveryPush, Status: models.RecipientQueued}
	gone := models.MessageRecipient{MessageID: id, UserID: f.leaderUser.ID, Channel: models.DeliveryPush, Status: models.RecipientQueued}
	for _, r := range []*models.MessageRecipient{&withDevices, &gone} {
		if err := f.app.DB.Create(r).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := enqueueMessagePushes(f.app.DB, id); err != nil {
		t.Fatal(err)
	}
	var keys []string
	f.app.DB.Model(&models.Job{}).Where("kind = ?", kindPushDeliver).Order("unique_key").Pluck("unique_key", &keys)
	want := []string{fmt.Sprintf("push:mr:%d:d:%d", withDevices.ID, phone.ID), fmt.Sprintf("push:mr:%d:d:%d", withDevices.ID, laptop.ID)}
	if len(keys) != 2 || keys[0] != min(want[0], want[1]) || keys[1] != max(want[0], want[1]) {
		t.Fatalf("jobs %v, want %v", keys, want)
	}
	var got, left models.MessageRecipient
	f.app.DB.First(&got, gone.ID)
	f.app.DB.First(&left, withDevices.ID)
	if got.Status != models.RecipientSuppressed || got.Reason != pushWhyGone || left.Status != models.RecipientQueued {
		t.Fatalf("no device: %+v; with devices: %+v", got, left)
	}
}

// The phone pass takes only phone-shaped runs: dates, times, room lists,
// hymn numbers and years stay word for word.
func TestLockScreenSafeTakesOnlyPhoneNumbers(t *testing.T) {
	for _, keep := range []string{
		"Meeting 2026-10-31 18:00",
		"Meeting 2026-10-31T18:00:00 in the hall",
		"Rooms 101 102 103 104",
		"Hymns 12 45 301 22",
		"2026 2027 2028",
		"Doors at 6:45, start 7:00",
		"Bring $25 and 3 friends",
		"Psalm 23:1-6",
	} {
		if got := lockScreenSafe(keep); got != keep {
			t.Errorf("%q became %q", keep, got)
		}
	}
	for in, want := range map[string]string{
		"Call 555 0123 today":                   "Call  today",
		"Call 555-0123 today":                   "Call  today",
		"Call 555.0123 today":                   "Call  today",
		"Call (208) 555-0123 today":             "Call  today",
		"Call 208-555-0123 today":               "Call  today",
		"Call 1-208-555-0123 today":             "Call  today",
		"Call 2085550123 today":                 "Call  today",
		"Call +44 20 7946 0958 today":           "Call  today",
		"Call +1 (208) 555-0123 today":          "Call  today",
		"On 2026-10-31 18:00 call 555-0123":     "On 2026-10-31 18:00 call ",
		"Rooms 101 102, or call 555 0123 first": "Rooms 101 102, or call  first",
	} {
		if got := lockScreenSafe(in); got != want {
			t.Errorf("%q became %q, want %q", in, got, want)
		}
	}
}

// seedDatePush is a sending announcement to the departure fixture's kept
// date, whose plain text is a literal sentinel, with a queued push row and a
// device for the departed person, who has push on.
func seedDatePush(t *testing.T, f occurrenceReadFixture, text string) (models.MessageRecipient, testDevice, *fakePush) {
	t.Helper()
	fp := useFakePush(t, f.app)
	pushConsentRow(t, f.app, f.recipient, "all", models.ConsentOptedIn)
	dev := newPushDevice(t, f.app, f.recipient, "phone")
	aud, _ := json.Marshal(audienceJSON{Type: policy.AudienceOccurrence, OccurrenceID: f.dates[1].ID})
	now := time.Now().UTC()
	m := models.Message{AuthorID: &f.issuer.ID, Status: models.MessageSending, Category: models.MessageCategoryAnnouncement,
		Subject: "Date news", BodyHTML: "<p>" + text + "</p>", BodyText: text, Audience: string(aud), ScheduledAt: &now}
	gormMust(t, f.app.DB.Create(&m))
	row := models.MessageRecipient{MessageID: m.ID, UserID: f.recipient.ID, Channel: models.DeliveryPush, Status: models.RecipientQueued}
	gormMust(t, f.app.DB.Create(&row))
	return row, dev, fp
}

// TestAnnouncementPushNeverStraddlesAnEdit: an announcement push decides
// whether it still goes, and fills its lock screen, in one snapshot. The
// departed person's kept date reaches its old end and the real guarded edit
// ends the grant and moves the date in one commit, at the "grants" seam of
// that decision. Before the edit the grant had lapsed and after it the grant is
// stamped, so in either committed state the push does not go: the author's
// literal words never reach the lock screen. A decision read over separate
// statements would pair the live grant with the moved date and push them.
func TestAnnouncementPushNeverStraddlesAnEdit(t *testing.T) {
	const sentinel = "Literal push sentinel"
	f := newDepartureReadFixture(t, models.VisibilityInviteOnly, models.RegGoing, policy.OccurrenceOriginDepartureSelf)
	target := f.dates[1]
	date := models.FormatCalendarDate(target.OriginalDate)
	row, dev, fp := seedDatePush(t, f, sentinel)
	base := editBaseOf(t, f.admin, f.event.ID, date)
	newMutableClock(f.app, target.EndsAt)
	var fired atomic.Bool
	f.app.hooks.invitationReadStep = func(step string) {
		if step != "grants" || !fired.CompareAndSwap(false, true) {
			return
		}
		w := postDateEdit(f.admin, f.event.ID, date, dateEditBody(base.Revision,
			map[string]any{"startsLocal": target.StartsAt.Add(2 * time.Hour).UTC().Format("2006-01-02T15:04"), "title": "Moved secret title"}))
		if w.Code != http.StatusOK {
			t.Errorf("the edit at the seam: %d %s", w.Code, w.Body.String())
		}
	}
	if err := f.app.deliverMessagePush(context.Background(), row.ID, dev.ID, 0); err != nil {
		t.Fatal(err)
	}
	if !fired.Load() {
		t.Fatal("the push never reached the grants step: no straddle was tried")
	}
	var got models.MessageRecipient
	gormMust(t, f.app.DB.First(&got, row.ID))
	if got.Status != models.RecipientSuppressed || got.Reason != skipCantSeeDate || fp.total() != 0 {
		t.Fatalf("the push went, or was not skipped as hidden: %+v, pushes %v", got, fp.sent)
	}
}

// TestPushesDecideInOneSnapshotAndSendAfter: the announcement push and the
// push reminder read every fact of their decision and payload through their
// one snapshot, and call the push service and record the row only after it
// ends: on a one-connection pool any read outside the snapshot, or a send or
// record inside it, would wait for the connection the snapshot holds.
func TestPushesDecideInOneSnapshotAndSendAfter(t *testing.T) {
	t.Run("announcement", func(t *testing.T) {
		f := newDepartureReadFixture(t, models.VisibilityInviteOnly, models.RegGoing, policy.OccurrenceOriginDepartureSelf)
		row, dev, fp := seedDatePush(t, f, "Hello from {{event_title}}")
		gormMust(t, f.app.DB.Model(&models.Message{}).Where("id = ?", row.MessageID).Update("subject", "{{event_title}} news"))
		one := oneConnectionPool(t, f.app)
		ctx := oneConnection(t, f.app, one)
		if err := f.app.deliverMessagePush(ctx, row.ID, dev.ID, 0); err != nil {
			t.Fatalf("on one connection: %v", err)
		}
		f.app.DB = one
		var got models.MessageRecipient
		gormMust(t, f.app.DB.First(&got, row.ID))
		if got.Status != models.RecipientSent || fp.count(dev.path) != 1 {
			t.Fatalf("the push: %+v, pushes %v", got, fp.sent)
		}
		if note := pushPayload(t, fp, dev); note.Title != "Retained date title news" {
			t.Fatalf("lock screen %q", note.Title)
		}
	})
	t.Run("reminder", func(t *testing.T) {
		f := newDepartureReadFixture(t, models.VisibilityInviteOnly, models.RegGoing, policy.OccurrenceOriginDepartureSelf)
		row, dev, fp, planned := seedReminderPush(t, f)
		one := oneConnectionPool(t, f.app)
		ctx := oneConnection(t, f.app, one)
		if err := f.app.deliverMessagePush(ctx, row.ID, dev.ID, planned); err != nil {
			t.Fatalf("on one connection: %v", err)
		}
		f.app.DB = one
		var got models.MessageRecipient
		gormMust(t, f.app.DB.First(&got, row.ID))
		if got.Status != models.RecipientSent || fp.count(dev.path) != 1 {
			t.Fatalf("the push reminder: %+v, pushes %v", got, fp.sent)
		}
		if note := pushPayload(t, fp, dev); !strings.Contains(note.Title, "Retained date title") {
			t.Fatalf("lock screen %q", note.Title)
		}
	})
}

// seedReminderPush is a queued 2-hour push reminder for the departure
// fixture's kept date, with a device for the departed person, who has push
// on, and the app's clock at the planned time it returns.
func seedReminderPush(t *testing.T, f occurrenceReadFixture) (models.MessageRecipient, testDevice, *fakePush, int64) {
	t.Helper()
	fp := useFakePush(t, f.app)
	pushConsentRow(t, f.app, f.recipient, "all", models.ConsentOptedIn)
	dev := newPushDevice(t, f.app, f.recipient, "phone")
	occ := f.dates[1]
	key := pushReminderKey(occ.ID, occ.StartsAt)
	aud, _ := json.Marshal(audienceJSON{Type: policy.AudienceOccurrence, OccurrenceID: occ.ID, Statuses: []string{models.RegGoing}})
	now := time.Now().UTC()
	m := models.Message{Channel: models.DeliveryPush, Status: models.MessageSending, Category: models.MessageCategoryReminder,
		Subject: "Reminder", Audience: string(aud), ScheduledAt: &now, OccurrenceID: &occ.ID, ReminderKey: &key}
	gormMust(t, f.app.DB.Create(&m))
	row := models.MessageRecipient{MessageID: m.ID, UserID: f.recipient.ID, Channel: models.DeliveryPush, Status: models.RecipientQueued}
	gormMust(t, f.app.DB.Create(&row))
	planned := occ.StartsAt.Add(-2 * time.Hour)
	f.app.clock = func() time.Time { return planned }
	return row, dev, fp, planned.Unix()
}

// TestPushDestinationIsReadInThePayloadSnapshot: the device a push goes to,
// its owner's account, the church's switch and consent are read in the
// snapshot that builds the payload. A change committed after the early
// checks and before that snapshot (pushStep "snapshot") stops the push: a shared
// browser rebound to another account (same endpoint and keys) never gets the
// first person's payload, and consent turned off or a church switch turned
// off before the snapshot is honored. Checked for the announcement push and
// the push reminder.
func TestPushDestinationIsReadInThePayloadSnapshot(t *testing.T) {
	type seeded struct {
		f       occurrenceReadFixture
		row     models.MessageRecipient
		dev     testDevice
		fp      *fakePush
		planned int64
	}
	paths := map[string]func(t *testing.T) seeded{
		"announcement": func(t *testing.T) seeded {
			f := newDepartureReadFixture(t, models.VisibilityInviteOnly, models.RegGoing, policy.OccurrenceOriginDepartureSelf)
			row, dev, fp := seedDatePush(t, f, "Literal push sentinel")
			return seeded{f: f, row: row, dev: dev, fp: fp}
		},
		"reminder": func(t *testing.T) seeded {
			f := newDepartureReadFixture(t, models.VisibilityInviteOnly, models.RegGoing, policy.OccurrenceOriginDepartureSelf)
			row, dev, fp, planned := seedReminderPush(t, f)
			return seeded{f: f, row: row, dev: dev, fp: fp, planned: planned}
		},
	}
	changes := map[string]struct {
		commit func(t *testing.T, s seeded)
		reason string
	}{
		"device rebound": {func(t *testing.T, s seeded) {
			other := newTestUser(t, s.f.app, "newholder", "password123456", models.RoleMember)
			gormMust(t, s.f.app.DB.Model(&models.PushSubscription{}).Where("id = ?", s.dev.ID).Update("user_id", other.ID))
		}, pushWhyGone},
		"consent off": {func(t *testing.T, s seeded) {
			pushConsentRow(t, s.f.app, s.f.recipient, "all", models.ConsentOptedOut)
		}, pushWhyNotOn},
		"church switch off": {func(t *testing.T, s seeded) {
			if err := setSettingIn(s.f.app.DB, settingPushEnabled, "false"); err != nil {
				t.Fatal(err)
			}
		}, pushWhyChurchOff},
	}
	for path, seed := range paths {
		for name, ch := range changes {
			t.Run(path+"/"+name, func(t *testing.T) {
				s := seed(t)
				var fired atomic.Bool
				s.f.app.hooks.pushStep = func(step string) {
					if step == "snapshot" && fired.CompareAndSwap(false, true) {
						ch.commit(t, s)
					}
				}
				if err := s.f.app.deliverMessagePush(context.Background(), s.row.ID, s.dev.ID, s.planned); err != nil {
					t.Fatal(err)
				}
				if !fired.Load() {
					t.Fatal("the push never reached its snapshot")
				}
				var got models.MessageRecipient
				gormMust(t, s.f.app.DB.First(&got, s.row.ID))
				if s.fp.total() != 0 || got.Status != models.RecipientSuppressed || got.Reason != ch.reason {
					t.Fatalf("the push went or was recorded wrongly: %+v, pushes %v", got, s.fp.sent)
				}
			})
		}
	}
}

// TestDelayedSeriesPushCountsFromTheScheduledTime: a series announcement's
// push that waits (quiet hours, a backlog) still goes when the reader's only
// date they may see started while it waited, because a series audience
// counts dates from the message's stored scheduled time, as the email send
// and the read page count them. The reader's other date is hidden from them;
// a second reader, the control, holds only that hidden date and gets no push,
// no email and no read page. Push, email and the read page agree for both.
func TestDelayedSeriesPushCountsFromTheScheduledTime(t *testing.T) {
	f := newReadVisFixture(t, models.VisibilityPublic)
	control := newTestUser(t, f.app, "control", "password1234", models.RoleMember)
	controlC := loginAPI(t, f.app, "control", "password1234")
	started, hidden := f.occs[0], f.occs[1]
	now := time.Now().UTC()
	f.setDate(t, started, "starts_at", now.Add(-30*time.Minute))
	f.setDate(t, started, "ends_at", now.Add(30*time.Minute))
	f.setDate(t, hidden, "visibility_override", models.VisibilityInviteOnly)
	f.register(t, f.reader, started)
	f.register(t, f.reader, hidden)
	f.register(t, control, hidden)

	fp := useFakePush(t, f.app)
	id, emailRows := seedSending(t, f.app, f.leaderU, f.seriesAudience(), toRow(f.reader), toRow(control))
	gormMust(t, f.app.DB.Model(&models.Message{}).Where("id = ?", id).Update("scheduled_at", now.Add(-2*time.Hour)))
	devs := map[uint]testDevice{}
	pushRows := map[uint]models.MessageRecipient{}
	for _, u := range []*models.User{f.reader, control} {
		pushConsentRow(t, f.app, u, "all", models.ConsentOptedIn)
		devs[u.ID] = newPushDevice(t, f.app, u, u.Name)
		row := models.MessageRecipient{MessageID: id, UserID: u.ID, Channel: models.DeliveryPush, Status: models.RecipientQueued}
		gormMust(t, f.app.DB.Create(&row))
		pushRows[u.ID] = row
	}
	for uid, row := range pushRows {
		if err := f.app.deliverMessagePush(context.Background(), row.ID, devs[uid].ID, 0); err != nil {
			t.Fatal(err)
		}
	}
	readOf := func(c *apiClient) int {
		return c.do("GET", "/api/v1/messages/"+itoa(id)+"/read", nil, false).Code
	}
	// The read page judges a queued email row live, before the send decides.
	if code := readOf(f.readerC); code != http.StatusOK {
		t.Fatalf("the reader's read page before the email: %d", code)
	}
	if code := readOf(controlC); code != http.StatusNotFound {
		t.Fatalf("the control's read page before the email: %d", code)
	}
	s := &deliverySender{}
	if err := deliverRows(f.app, s, id, emailRows); err != nil {
		t.Fatal(err)
	}
	var reader, ctl models.MessageRecipient
	gormMust(t, f.app.DB.First(&reader, pushRows[f.reader.ID].ID))
	gormMust(t, f.app.DB.First(&ctl, pushRows[control.ID].ID))
	if reader.Status != models.RecipientSent || fp.count(devs[f.reader.ID].path) != 1 {
		t.Fatalf("the reader's delayed push: %+v, pushes %v", reader, fp.sent)
	}
	if ctl.Status != models.RecipientSuppressed || ctl.Reason != skipCantSeeDate || fp.count(devs[control.ID].path) != 0 {
		t.Fatalf("the control's push: %+v, pushes %v", ctl, fp.sent)
	}
	var readerEmail, controlEmail models.MessageRecipient
	gormMust(t, f.app.DB.First(&readerEmail, emailRows[0]))
	gormMust(t, f.app.DB.First(&controlEmail, emailRows[1]))
	if readerEmail.Status != models.RecipientSent || controlEmail.Status != models.RecipientSuppressed || controlEmail.Reason != skipCantSeeDate {
		t.Fatalf("email disagrees with push: reader %+v, control %+v", readerEmail, controlEmail)
	}
	if code := readOf(f.readerC); code != http.StatusOK {
		t.Fatalf("the reader's read page: %d", code)
	}
	if code := readOf(controlC); code != http.StatusNotFound {
		t.Fatalf("the control's read page: %d", code)
	}
}

// TestPushPayloadComesFromTheDestinationsSnapshot: an announcement push and a
// push reminder read their device, its owner and what the payload says in one
// Repeatable Read snapshot. Right after the device is read there ("destination"),
// a shared browser is rebound to another account and the date retitled in one
// commit. The push already chose its device in the earlier state, so it goes,
// and it carries that state's title: it never pairs a device as it was with a
// title written after the device changed hands.
func TestPushPayloadComesFromTheDestinationsSnapshot(t *testing.T) {
	const retitled = "Retitled after the device moved"
	for _, path := range []string{"announcement", "reminder"} {
		t.Run(path, func(t *testing.T) {
			f := newDepartureReadFixture(t, models.VisibilityInviteOnly, models.RegGoing, policy.OccurrenceOriginDepartureSelf)
			var row models.MessageRecipient
			var dev testDevice
			var fp *fakePush
			var planned int64
			if path == "announcement" {
				row, dev, fp = seedDatePush(t, f, "Hello from {{event_title}}")
				gormMust(t, f.app.DB.Model(&models.Message{}).Where("id = ?", row.MessageID).Update("subject", "{{event_title}} news"))
			} else {
				row, dev, fp, planned = seedReminderPush(t, f)
			}
			other := newTestUser(t, f.app, "next holder", "password1234", models.RoleMember)
			var fired atomic.Bool
			f.app.hooks.pushStep = func(step string) {
				if step != "destination" || !fired.CompareAndSwap(false, true) {
					return
				}
				if err := f.app.DB.Transaction(func(tx *gorm.DB) error {
					if err := tx.Model(&models.PushSubscription{}).Where("id = ?", dev.ID).Update("user_id", other.ID).Error; err != nil {
						return err
					}
					return tx.Model(&models.EventOccurrence{}).Where("id = ?", f.dates[1].ID).Update("title_override", retitled).Error
				}); err != nil {
					t.Error(err)
				}
			}
			if err := f.app.deliverMessagePush(context.Background(), row.ID, dev.ID, planned); err != nil {
				t.Fatal(err)
			}
			if !fired.Load() {
				t.Fatal("the push never reached its destination step")
			}
			if fp.count(dev.path) != 1 {
				t.Fatalf("the push chose its device before the commit and should go: %v", fp.sent)
			}
			if note := pushPayload(t, fp, dev); strings.Contains(note.Title, retitled) || !strings.Contains(note.Title, "Retained date title") {
				t.Fatalf("the payload paired the device as read with a later title: %q", note.Title)
			}
		})
	}
}

// TestQueuedSeriesPushCountsFromTheCutoffTryAgainSet: a series announcement's
// push waits (here, until its job runs) while the author presses Try again for
// a failed email. Try again moves the message's scheduled time, which is the
// series cutoff, and never touches push rows. The push reads the message again
// inside its snapshot, so, like the retried email and the read page, it counts
// from the new cutoff: the reader's only date started before it, so the push
// doesn't go and the read page closes. The control (no Try again) pushes from
// the original cutoff, as TestDelayedSeriesPushCountsFromTheScheduledTime does.
// In "between clocks" the date starts after the push read its clock for quiet
// hours and the hourly limit and before Try again's cutoff: a cutoff later than
// that stale clock would count from the clock and admit the date, so the push
// must decide on a clock read in its snapshot.
func TestQueuedSeriesPushCountsFromTheCutoffTryAgainSet(t *testing.T) {
	for _, tc := range []struct {
		name         string
		retry, fresh bool
	}{{"control", false, false}, {"retry", true, false}, {"between clocks", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReadVisFixture(t, models.VisibilityPublic)
			started := f.occs[0]
			now := time.Now().UTC()
			// The push's earlier clock, held until Try again is pressed.
			preflight := now.Add(-10 * time.Minute)
			startsAt := now.Add(-30 * time.Minute)
			if tc.fresh {
				startsAt = preflight.Add(5 * time.Minute)
			}
			f.setDate(t, started, "starts_at", startsAt)
			f.setDate(t, started, "ends_at", now.Add(30*time.Minute))
			f.register(t, f.reader, started)
			fp := useFakePush(t, f.app)
			id, emailRows := seedSending(t, f.app, f.leaderU, f.seriesAudience(), toRow(f.reader))
			gormMust(t, f.app.DB.Model(&models.Message{}).Where("id = ?", id).Updates(map[string]any{"scheduled_at": now.Add(-2 * time.Hour), "status": models.MessageSent}))
			gormMust(t, f.app.DB.Model(&models.MessageRecipient{}).Where("id = ?", emailRows[0]).Updates(map[string]any{"status": models.RecipientFailed, "error": whyProvider}))
			pushConsentRow(t, f.app, f.reader, "all", models.ConsentOptedIn)
			dev := newPushDevice(t, f.app, f.reader, "phone")
			row := models.MessageRecipient{MessageID: id, UserID: f.reader.ID, Channel: models.DeliveryPush, Status: models.RecipientQueued}
			gormMust(t, f.app.DB.Create(&row))
			var pressed atomic.Bool
			if tc.fresh {
				f.app.clock = func() time.Time {
					if pressed.Load() {
						return time.Now()
					}
					return preflight
				}
			}
			f.app.hooks.pushStep = func(step string) {
				if !tc.retry || step != "snapshot" || !pressed.CompareAndSwap(false, true) {
					return
				}
				if w := f.leader.do("POST", "/api/v1/messages/"+itoa(id)+"/retry", nil, true); w.Code != http.StatusOK {
					t.Errorf("Try again at the seam: %d %s", w.Code, w.Body.String())
				}
			}
			if err := f.app.deliverMessagePush(context.Background(), row.ID, dev.ID, 0); err != nil {
				t.Fatal(err)
			}
			if tc.retry && !pressed.Load() {
				t.Fatal("the push never reached its snapshot seam: no Try again was pressed")
			}
			gormMust(t, f.app.DB.First(&row, row.ID))
			var email models.MessageRecipient
			gormMust(t, f.app.DB.First(&email, emailRows[0]))
			read := f.readerC.do("GET", "/api/v1/messages/"+itoa(id)+"/read", nil, false).Code
			if !tc.retry {
				if row.Status != models.RecipientSent || fp.count(dev.path) != 1 {
					t.Fatalf("the control's push from the original cutoff: %+v, pushes %v", row, fp.sent)
				}
				return
			}
			var msg models.Message
			gormMust(t, f.app.DB.First(&msg, id))
			if msg.ScheduledAt == nil || !msg.ScheduledAt.After(startsAt) || (tc.fresh && !msg.ScheduledAt.After(preflight)) {
				t.Fatalf("Try again's cutoff %v isn't after the date's start %v (preflight %v)", msg.ScheduledAt, startsAt, preflight)
			}
			if row.Status != models.RecipientSuppressed || fp.count(dev.path) != 0 {
				t.Fatalf("the push after Try again moved the cutoff: %+v, pushes %v", row, fp.sent)
			}
			if email.Status != models.RecipientSuppressed || email.Reason != skipNoLongerOnList || read != http.StatusNotFound {
				t.Fatalf("email and the read page after Try again: %+v, read %d", email, read)
			}
		})
	}
}
