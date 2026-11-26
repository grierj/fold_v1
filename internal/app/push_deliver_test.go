package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fold/internal/jobs"
	"fold/internal/notify"
	"fold/internal/push"
	"fold/internal/testdb"
	"fold/models"

	"gorm.io/gorm"
)

// fakePush is a push service behind the App's push client: it answers
// each device (by endpoint path) with the status the test set, and records
// every request it got.
type fakePush struct {
	mu      sync.Mutex
	status  map[string]int // by path; default 201
	err     map[string]bool
	sent    map[string]int
	headers []http.Header
	// bodies is each path's last encrypted body (pushPayload decrypts it).
	bodies map[string][]byte
	// onSend, set before any send, runs while the push service holds a
	// request: a test commits a change there, between dispatch and outcome.
	onSend func(path string)
}

func (f *fakePush) RoundTrip(r *http.Request) (*http.Response, error) {
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
	}
	if f.onSend != nil {
		f.onSend(r.URL.Path)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.bodies == nil {
		f.bodies = map[string][]byte{}
	}
	f.bodies[r.URL.Path] = body
	f.sent[r.URL.Path]++
	f.headers = append(f.headers, r.Header.Clone())
	if f.err[r.URL.Path] {
		// A transport error that, like many, repeats the URL.
		return nil, fmt.Errorf("connection reset talking to %s", r.URL)
	}
	code := http.StatusCreated
	if c, ok := f.status[r.URL.Path]; ok {
		code = c
	}
	return &http.Response{StatusCode: code, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
}

func (f *fakePush) set(path string, code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status[path] = code
}

func (f *fakePush) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sent[path]
}

func (f *fakePush) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.sent {
		n += c
	}
	return n
}

// pushTestApp is a test App with VAPID keys and the fake push service.
// Quiet hours are off, so a test's result never depends on the hour it
// runs; the quiet-hours tests (push_quiet_test.go) turn them on.
func pushTestApp(t *testing.T) (*App, *fakePush) {
	t.Helper()
	app := testApp(t)
	return app, useFakePush(t, app)
}

// useFakePush gives app VAPID keys and the fake push service, with quiet
// hours off.
func useFakePush(t *testing.T, app *App) *fakePush {
	t.Helper()
	if err := ensureVAPIDKeys(app.DB, app.Config.SessionSecret); err != nil {
		t.Fatal(err)
	}
	f := &fakePush{status: map[string]int{}, err: map[string]bool{}, sent: map[string]int{}}
	app.Push = &push.Client{HTTP: &http.Client{Transport: f}, Subject: "mailto:admin@example.com"}
	open := push.Window{StartHour: 0, EndHour: 24} // never waits
	app.hooks.pushWindow = &open
	return f
}

// testDevice is a device with real browser keys; its endpoint path is
// /fcm/send/<name>-secret.
type testDevice struct {
	models.PushSubscription
	path string
	key  *ecdh.PrivateKey
	auth []byte
}

func newPushDevice(t *testing.T, app *App, u *models.User, name string) testDevice {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	_, _ = rand.Read(auth)
	path := "/fcm/send/" + name + "-secret"
	d := models.PushSubscription{UserID: u.ID, Endpoint: "https://fcm.googleapis.com" + path,
		P256dh: base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()), Auth: base64.RawURLEncoding.EncodeToString(auth), Label: name}
	if err := app.DB.Create(&d).Error; err != nil {
		t.Fatal(err)
	}
	return testDevice{d, path, key, auth}
}

func pushConsentRow(t *testing.T, app *App, u *models.User, scope, status string) {
	t.Helper()
	if err := app.DB.Create(&models.Consent{UserID: u.ID, Channel: models.ConsentPush, Scope: scope, Status: status, Source: models.ConsentSourceWebForm}).Error; err != nil {
		t.Fatal(err)
	}
}

// pushMember is an active member who turned push on.
func pushMember(t *testing.T, app *App, name string) *models.User {
	t.Helper()
	u := newTestUser(t, app, name, "password123456", models.RoleMember)
	pushConsentRow(t, app, u, "all", models.ConsentOptedIn)
	return u
}

func sendNotice(t *testing.T, app *App, kind string, u *models.User) uint {
	t.Helper()
	ids, err := notify.Send(app.DB, notify.Msg{Kind: kind, To: []uint{u.ID}, Link: "/groups/1", Vars: notify.Vars{Group: "Youth"}})
	if err != nil || len(ids) != 1 {
		t.Fatalf("notify: %v %v", ids, err)
	}
	return ids[0]
}

func TestQueuePushForSkipsNoticesThatDoNotPush(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind string
	}{
		{"email only", notify.MembershipLeft},
		{"empty push title", notify.GroupApproved},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := testApp(t)
			u := newTestUser(t, app, "pat", "password123456", models.RoleMember)
			newPushDevice(t, app, u, "phone")
			newPushDevice(t, app, u, "laptop")
			id := sendNotice(t, app, tc.kind, u)
			if tc.kind == notify.GroupApproved {
				if err := app.DB.Model(&models.Notification{}).Where("id = ?", id).Update("push_title", "").Error; err != nil {
					t.Fatal(err)
				}
			}
			var n models.Notification
			if err := app.DB.First(&n, id).Error; err != nil {
				t.Fatal(err)
			}
			// A nil database proves the skip does not issue any statement.
			if err := queuePushFor(nil, &n); err != nil {
				t.Fatal(err)
			}
			if err := app.DB.Transaction(func(tx *gorm.DB) error { return queuePushFor(tx, &n) }); err != nil {
				t.Fatal(err)
			}
			if rows := pushJobsFor(t, app, id); len(rows) != 0 {
				t.Fatalf("queued %d push jobs", len(rows))
			}
			if err := app.deliverOneNotification(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			if d := emailDeliveryOf(t, app, id); d.Status != models.DeliverySent {
				t.Fatalf("email status %q", d.Status)
			}
			if rows := pushJobsFor(t, app, id); len(rows) != 0 {
				t.Fatalf("queued %d push jobs after email delivery", len(rows))
			}
		})
	}
}

func TestQueuePushForQueuesOneJobPerDeviceOnce(t *testing.T) {
	t.Run("repeated fan-out", func(t *testing.T) {
		app := testApp(t)
		u := newTestUser(t, app, "pat", "password123456", models.RoleMember)
		phone := newPushDevice(t, app, u, "phone")
		laptop := newPushDevice(t, app, u, "laptop")
		other := newTestUser(t, app, "other", "password123456", models.RoleMember)
		newPushDevice(t, app, other, "other-phone")
		id, _ := seedNotifyDeliver(t, app, u, notify.DeliverJob.MaxAttempts)
		var n models.Notification
		if err := app.DB.First(&n, id).Error; err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if err := app.DB.Transaction(func(tx *gorm.DB) error { return queuePushFor(tx, &n) }); err != nil {
				t.Fatal(err)
			}
		}
		if err := app.deliverOneNotification(context.Background(), id); err != nil {
			t.Fatal(err)
		}
		assertOnePushEach(t, app, id, phone.ID, laptop.ID)
		for _, row := range pushJobsFor(t, app, id) {
			var payload pushJob
			if err := json.Unmarshal([]byte(row.Payload), &payload); err != nil {
				t.Fatal(err)
			}
			if payload.NotificationID != id || (payload.DeviceID != phone.ID && payload.DeviceID != laptop.ID) ||
				*row.UniqueKey != fmt.Sprintf("push:n:%d:d:%d", id, payload.DeviceID) {
				t.Fatalf("unexpected push payload %+v for key %s", payload, *row.UniqueKey)
			}
		}
		var count int64
		if err := app.DB.Model(&models.Job{}).Where("kind = ?", kindPushDeliver).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 2 {
			t.Fatalf("push job count %d, want 2", count)
		}
	})
	t.Run("cancelled context", func(t *testing.T) {
		app := testApp(t)
		u := newTestUser(t, app, "pat", "password123456", models.RoleMember)
		newPushDevice(t, app, u, "phone")
		id := sendNotice(t, app, notify.GroupApproved, u)
		var n models.Notification
		if err := app.DB.First(&n, id).Error; err != nil {
			t.Fatal(err)
		}
		err := app.DB.Transaction(func(tx *gorm.DB) error {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return queuePushFor(tx.WithContext(ctx), &n)
		})
		if err == nil {
			t.Fatal("cancelled device query succeeded")
		}
		if rows := pushJobsFor(t, app, id); len(rows) != 0 {
			t.Fatalf("queued %d push jobs with cancelled context", len(rows))
		}
	})
}

func pushDelivery(t *testing.T, app *App, notificationID uint) (status, reason string) {
	t.Helper()
	var d models.NotificationDelivery
	if err := app.DB.Where("notification_id = ? AND channel = ?", notificationID, models.DeliveryPush).First(&d).Error; err != nil {
		return "", ""
	}
	return d.Status, d.Reason
}

// runPushJobs queues one push.deliver per device and runs every due job.
func runPushJobs(t *testing.T, app *App, notificationID uint, devices ...testDevice) {
	t.Helper()
	for _, d := range devices {
		if err := enqueuePushDelivery(app.DB, notificationID, d.ID, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	drainJobs(t, app)
}

func drainJobs(t *testing.T, app *App) {
	t.Helper()
	r := jobs.NewRunner(app.DB, app.pushKinds()...)
	for {
		ran, err := r.RunOnce(context.Background(), "test")
		if err != nil {
			t.Fatal(err)
		}
		if !ran {
			return
		}
	}
}

func deviceExists(app *App, id uint) bool {
	var n int64
	app.DB.Model(&models.PushSubscription{}).Where("id = ?", id).Count(&n)
	return n == 1
}

func TestPushDeliverRecordsOneSentRowForSeveralDevices(t *testing.T) {
	app, f := pushTestApp(t)
	u := pushMember(t, app, "pat")
	phone, laptop := newPushDevice(t, app, u, "phone"), newPushDevice(t, app, u, "laptop")
	n := sendNotice(t, app, notify.GroupApproved, u)
	runPushJobs(t, app, n, phone, laptop)
	if f.count(phone.path) != 1 || f.count(laptop.path) != 1 {
		t.Fatalf("sends: %v", f.sent)
	}
	if status, _ := pushDelivery(t, app, n); status != models.DeliverySent {
		t.Fatalf("delivery %q", status)
	}
	var rows int64
	app.DB.Model(&models.NotificationDelivery{}).Where("notification_id = ? AND channel = ?", n, models.DeliveryPush).Count(&rows)
	if rows != 1 {
		t.Fatalf("%d delivery rows", rows)
	}
	h := f.headers[0]
	if h.Get("Content-Encoding") != "aes128gcm" || h.Get("TTL") != "86400" || h.Get("Topic") != fmt.Sprintf("n%d", n) || !strings.HasPrefix(h.Get("Authorization"), "vapid t=") {
		t.Fatalf("headers %v", h)
	}
	// Queuing the same device again adds nothing: one job per device.
	runPushJobs(t, app, n, phone)
	if f.count(phone.path) != 1 {
		t.Fatalf("phone got it %d times", f.count(phone.path))
	}
}

func TestPushDeliverGoneDeletesTheDevice(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusGone} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			app, f := pushTestApp(t)
			u := pushMember(t, app, "pat")
			phone := newPushDevice(t, app, u, "phone")
			f.set(phone.path, code)
			n := sendNotice(t, app, notify.GroupApproved, u)
			runPushJobs(t, app, n, phone)
			if deviceExists(app, phone.ID) {
				t.Fatal("the device is still there")
			}
			if status, reason := pushDelivery(t, app, n); status != models.DeliverySuppressed || reason != "device gone" {
				t.Fatalf("delivery %q %q", status, reason)
			}
		})
	}
	// With another device that takes it, the row is sent, whatever order.
	app, f := pushTestApp(t)
	u := pushMember(t, app, "pat")
	old, phone := newPushDevice(t, app, u, "old"), newPushDevice(t, app, u, "phone")
	f.set(old.path, http.StatusGone)
	n := sendNotice(t, app, notify.GroupApproved, u)
	runPushJobs(t, app, n, old, phone)
	if status, _ := pushDelivery(t, app, n); status != models.DeliverySent || deviceExists(app, old.ID) || !deviceExists(app, phone.ID) {
		t.Fatalf("delivery %q", status)
	}
}

// TestPushNoticeGoneWaitsOnlyForARealJob: a notice's push to a device that is
// gone leaves its delivery to another device only while that device's job is
// still to run, and that job still delivers it, with no job added; a device
// saved after the notice's jobs were queued has none, so the delivery is
// "device gone" at once.
func TestPushNoticeGoneWaitsOnlyForARealJob(t *testing.T) {
	for _, sibling := range []string{"job queued", "no job"} {
		t.Run(sibling, func(t *testing.T) {
			app, f := pushTestApp(t)
			u := pushMember(t, app, "pat")
			old := newPushDevice(t, app, u, "old")
			var phone testDevice
			if sibling == "job queued" {
				phone = newPushDevice(t, app, u, "phone")
			}
			id := sendNotice(t, app, notify.GroupApproved, u)
			var n models.Notification
			gormMust(t, app.DB.First(&n, id))
			gormMust(t, app.DB.Model(&models.Job{}).Where("kind = ?", kindPushDeliver).Delete(&models.Job{}))
			if err := app.DB.Transaction(func(tx *gorm.DB) error { return queuePushFor(tx, &n) }); err != nil {
				t.Fatal(err)
			}
			if sibling == "no job" {
				phone = newPushDevice(t, app, u, "phone")
			}
			jobsBefore := pushJobCount(t, app)
			f.set(old.path, http.StatusGone)
			if err := app.deliverPush(context.Background(), pushJob{NotificationID: id, DeviceID: old.ID}); err != nil {
				t.Fatal(err)
			}
			if deviceExists(app, old.ID) {
				t.Fatal("the gone device is still there")
			}
			status, reason := pushDelivery(t, app, id)
			if sibling == "no job" {
				if status != models.DeliverySuppressed || reason != pushWhyGone || f.count(phone.path) != 0 {
					t.Fatalf("with a device that has no job: delivery %q %q, sends %v", status, reason, f.sent)
				}
				return
			}
			if status != "" {
				t.Fatalf("with the phone's job still to run: delivery %q %q; want none yet", status, reason)
			}
			if err := app.deliverPush(context.Background(), pushJob{NotificationID: id, DeviceID: phone.ID}); err != nil {
				t.Fatal(err)
			}
			if status, _ := pushDelivery(t, app, id); status != models.DeliverySent || f.count(phone.path) != 1 || pushJobCount(t, app) != jobsBefore {
				t.Fatalf("the phone's job: delivery %q, sends %v, jobs %d then %d", status, f.sent, jobsBefore, pushJobCount(t, app))
			}
		})
	}
}

func TestPushDeliverRetriesWithoutSendingTwice(t *testing.T) {
	for _, code := range []int{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			app, f := pushTestApp(t)
			u := pushMember(t, app, "pat")
			phone, laptop := newPushDevice(t, app, u, "phone"), newPushDevice(t, app, u, "laptop")
			f.set(laptop.path, code)
			n := sendNotice(t, app, notify.GroupApproved, u)
			runPushJobs(t, app, n, phone, laptop)
			var failed models.Job
			if err := app.DB.Where("kind = ? AND status = ?", kindPushDeliver, models.JobFailed).First(&failed).Error; err != nil {
				t.Fatalf("no retrying job: %v", err)
			}
			if failed.UniqueKey == nil || *failed.UniqueKey != fmt.Sprintf("push:n:%d:d:%d", n, laptop.ID) || failed.Attempts != 1 {
				t.Fatalf("job %+v", failed)
			}
			// The service recovers; the retry repeats only the laptop.
			f.set(laptop.path, http.StatusCreated)
			app.DB.Model(&models.Job{}).Where("id = ?", failed.ID).Update("run_at", time.Now().Add(-time.Second))
			drainJobs(t, app)
			if f.count(phone.path) != 1 || f.count(laptop.path) != 2 {
				t.Fatalf("sends %v", f.sent)
			}
			if status, _ := pushDelivery(t, app, n); status != models.DeliverySent {
				t.Fatalf("delivery %q", status)
			}
		})
	}
}

func TestPushDeliverLastAttemptRecordsFailure(t *testing.T) {
	app, f := pushTestApp(t)
	u := pushMember(t, app, "pat")
	phone := newPushDevice(t, app, u, "phone")
	f.set(phone.path, http.StatusInternalServerError)
	n := sendNotice(t, app, notify.GroupApproved, u)
	runPushJobs(t, app, n, phone)
	for range pushDeliverType.Attempts() {
		app.DB.Model(&models.Job{}).Where("kind = ?", kindPushDeliver).Update("run_at", time.Now().Add(-time.Second))
		drainJobs(t, app)
	}
	var j models.Job
	app.DB.Where("kind = ?", kindPushDeliver).First(&j)
	if j.Status != models.JobDead || f.count(phone.path) != pushDeliverType.Attempts() {
		t.Fatalf("job %s after %d sends", j.Status, f.count(phone.path))
	}
	var dev models.PushSubscription
	app.DB.First(&dev, phone.ID)
	if status, reason := pushDelivery(t, app, n); status != models.DeliveryFailed || reason != pushWhyNotReached || dev.FailedAt == nil {
		t.Fatalf("delivery %q %q, failed at %v", status, reason, dev.FailedAt)
	}
}

func TestPushDeliverFailures(t *testing.T) {
	for code, want := range map[int]string{http.StatusForbidden: pushWhyRefused, http.StatusRequestEntityTooLarge: pushWhyTooLarge} {
		app, f := pushTestApp(t)
		u := pushMember(t, app, "pat")
		phone := newPushDevice(t, app, u, "phone")
		f.set(phone.path, code)
		n := sendNotice(t, app, notify.GroupApproved, u)
		runPushJobs(t, app, n, phone)
		if status, reason := pushDelivery(t, app, n); status != models.DeliveryFailed || reason != want {
			t.Errorf("%d: delivery %q %q", code, status, reason)
		}
		if f.count(phone.path) != 1 || !deviceExists(app, phone.ID) {
			t.Errorf("%d: %d sends", code, f.count(phone.path))
		}
	}
	// A rendering past the cap is refused before any send.
	app, f := pushTestApp(t)
	app.hooks.pushRender = func(*models.Notification) (push.Notification, bool) {
		return push.Notification{Title: strings.Repeat("a", push.MaxPlaintext)}, true
	}
	u := pushMember(t, app, "pat")
	phone := newPushDevice(t, app, u, "phone")
	n := sendNotice(t, app, notify.GroupApproved, u)
	runPushJobs(t, app, n, phone)
	if status, reason := pushDelivery(t, app, n); status != models.DeliveryFailed || reason != pushWhyTooLarge || f.total() != 0 {
		t.Fatalf("delivery %q %q after %d sends", status, reason, f.total())
	}
}

func TestPushDeliverChecksConsentStatusAndTheChurchSwitch(t *testing.T) {
	app, f := pushTestApp(t)
	// Groups turned off: an optional groups kind is skipped, a required
	// (safety) kind still goes.
	pat := pushMember(t, app, "pat")
	pushConsentRow(t, app, pat, "category:groups", models.ConsentOptedOut)
	phone := newPushDevice(t, app, pat, "phone")
	optional := sendNotice(t, app, notify.GroupApproved, pat)
	required := sendNotice(t, app, notify.AccountTurned18, pat)
	runPushJobs(t, app, optional, phone)
	runPushJobs(t, app, required, phone)
	if status, reason := pushDelivery(t, app, optional); status != models.DeliverySuppressed || reason != "push turned off for group updates" {
		t.Errorf("optional: %q %q", status, reason)
	}
	if status, _ := pushDelivery(t, app, required); status != models.DeliverySent || f.count(phone.path) != 1 {
		t.Errorf("required: %q after %d sends", status, f.count(phone.path))
	}
	// Turning the category back on (a later row) lets it through.
	pushConsentRow(t, app, pat, "category:groups", models.ConsentOptedIn)
	again := sendNotice(t, app, notify.GroupRejected, pat)
	runPushJobs(t, app, again, phone)
	if status, _ := pushDelivery(t, app, again); status != models.DeliverySent {
		t.Errorf("after opting back in: %q", status)
	}

	// "Turn off notifications everywhere" overrides everything, required too.
	lee := pushMember(t, app, "lee")
	pushConsentRow(t, app, lee, "all", models.ConsentOptedOut)
	leePhone := newPushDevice(t, app, lee, "lee-phone")
	n := sendNotice(t, app, notify.AccountTurned18, lee)
	runPushJobs(t, app, n, leePhone)
	if status, reason := pushDelivery(t, app, n); status != models.DeliverySuppressed || reason != pushWhyNotOn {
		t.Errorf("all off: %q %q", status, reason)
	}

	// A disabled account gets nothing. (Disabling deletes its devices; this
	// one is written after, as a restore could leave one.)
	sam := pushMember(t, app, "sam")
	app.DB.Model(sam).Update("status", models.UserDisabled)
	samPhone := newPushDevice(t, app, sam, "sam-phone")
	n = sendNotice(t, app, notify.AccountTurned18, sam)
	runPushJobs(t, app, n, samPhone)
	if status, reason := pushDelivery(t, app, n); status != models.DeliverySuppressed || reason != pushWhyInactive {
		t.Errorf("disabled: %q %q", status, reason)
	}

	// The church turned push off: nothing sends.
	app.DB.Create(&models.Setting{Key: settingPushEnabled, Value: "false"})
	n = sendNotice(t, app, notify.AccountTurned18, pat)
	runPushJobs(t, app, n, phone)
	if status, reason := pushDelivery(t, app, n); status != models.DeliverySuppressed || reason != pushWhyChurchOff {
		t.Errorf("church off: %q %q", status, reason)
	}
	if f.count(leePhone.path)+f.count(samPhone.path) != 0 || f.count(phone.path) != 2 {
		t.Errorf("sends %v", f.sent)
	}
}

func TestPushDeliverSkipsWhatIsNoLongerThere(t *testing.T) {
	app, f := pushTestApp(t)
	pat, lee := pushMember(t, app, "pat"), pushMember(t, app, "lee")
	phone := newPushDevice(t, app, pat, "phone")
	n := sendNotice(t, app, notify.GroupApproved, pat)
	// The device changed hands after the job was queued.
	enqueuePushDelivery(app.DB, n, phone.ID, time.Now())
	app.DB.Model(&models.PushSubscription{}).Where("id = ?", phone.ID).Update("user_id", lee.ID)
	drainJobs(t, app)
	// A device or notification that's gone.
	runPushJobs(t, app, n, testDevice{PushSubscription: models.PushSubscription{ID: 99999}})
	if err := app.deliverPush(context.Background(), pushJob{NotificationID: 99999, DeviceID: phone.ID}); err != nil {
		t.Fatal(err)
	}
	if f.total() != 0 {
		t.Fatalf("sends %v", f.sent)
	}
	var dead int64
	app.DB.Model(&models.Job{}).Where("kind = ? AND status <> ?", kindPushDeliver, models.JobDone).Count(&dead)
	if dead != 0 {
		t.Fatalf("%d jobs not done", dead)
	}
}

// A push that can't say what it's about is titled with the church's name
// (DEC-134 B); every other kind keeps its own title.
func TestSafePushesAreTitledWithTheChurch(t *testing.T) {
	app := testApp(t)
	app.DB.Create(&models.Setting{Key: "site_name", Value: "First Church"})
	safe := &models.Notification{Kind: notify.TeenConsentChanged, PushTitle: notify.SafePushTitle, PushBody: notify.SafePushBody}
	if note, ok := app.lockScreen(safe); !ok || note.Title != "First Church" || note.Body != notify.SafePushBody {
		t.Fatalf("safe push %+v", note)
	}
	// A household row of an event kind uses the safe line too.
	household := &models.Notification{Kind: notify.RegistrationPromoted, PushTitle: notify.SafePushTitle, PushBody: notify.SafePushBody}
	if note, _ := app.lockScreen(household); note.Title != "First Church" {
		t.Fatalf("household push %+v", note)
	}
	// A kind that never pushes the safe text keeps its own title even if its
	// stored text happens to equal it.
	lookalike := &models.Notification{Kind: notify.GroupApproved, PushTitle: notify.SafePushTitle, PushBody: notify.SafePushBody}
	if note, _ := app.lockScreen(lookalike); note.Title != notify.SafePushTitle {
		t.Fatalf("lookalike push %+v", note)
	}
	own := &models.Notification{Kind: notify.GroupApproved, PushTitle: "Youth was approved", PushBody: "It's live now."}
	if note, _ := app.lockScreen(own); note.Title != "Youth was approved" {
		t.Fatalf("specific push %+v", note)
	}
}

// The lock-screen text is the row's stored push text, and only for kinds
// that push; never the bell title or body.
func TestRenderPushUsesTheStoredPushText(t *testing.T) {
	for _, k := range notify.Kinds() {
		got, ok := renderPush(&models.Notification{Kind: k.Key, Title: "Bell title", Body: "Bell body", PushTitle: "Push title", PushBody: "Push body", Link: "/"})
		if ok != k.AllowPush {
			t.Errorf("%s: renders a push %v, AllowPush %v", k.Key, ok, k.AllowPush)
		}
		if ok && (got.Title != "Push title" || got.Body != "Push body") {
			t.Errorf("%s: pushed %+v", k.Key, got)
		}
	}
	if _, ok := renderPush(&models.Notification{Kind: notify.GroupApproved, Title: "Bell title"}); ok {
		t.Error("a row with no push text pushed")
	}
}

// After commit, every notify.Send caller's rows of a pushed kind queue one
// push.deliver per device through notify.deliver, with no delay; a kind that doesn't push, or a person with no
// device, queues nothing.
func TestNotificationsQueueOnePushPerDevice(t *testing.T) {
	app, f := pushTestApp(t)
	pat, lee := pushMember(t, app, "pat"), pushMember(t, app, "lee")
	phone, laptop := newPushDevice(t, app, pat, "phone"), newPushDevice(t, app, pat, "laptop")
	pushed := sendNotice(t, app, notify.GroupApproved, pat)
	quiet := sendNotice(t, app, notify.MembershipLeft, pat)
	sendNotice(t, app, notify.GroupApproved, lee) // lee has no device
	drainNotices(t, app)

	var queued []models.Job
	app.DB.Where("kind = ?", kindPushDeliver).Order("id").Find(&queued)
	if len(queued) != 2 {
		t.Fatalf("%d jobs queued, want one per device", len(queued))
	}
	want := map[string]bool{fmt.Sprintf("push:n:%d:d:%d", pushed, phone.ID): true, fmt.Sprintf("push:n:%d:d:%d", pushed, laptop.ID): true}
	for _, j := range queued {
		if j.UniqueKey == nil || !want[*j.UniqueKey] || j.RunAt.After(time.Now()) {
			t.Errorf("job %v at %v", j.UniqueKey, j.RunAt)
		}
	}
	// Delivering the same rows again (a merge, a retry) queues nothing new.
	drainNotices(t, app)
	var again int64
	app.DB.Model(&models.Job{}).Where("kind = ?", kindPushDeliver).Count(&again)
	if again != 2 {
		t.Fatalf("%d jobs after a second delivery", again)
	}
	drainJobs(t, app)
	if f.count(phone.path) != 1 || f.count(laptop.path) != 1 || f.total() != 2 {
		t.Fatalf("sends %v", f.sent)
	}
	if status, _ := pushDelivery(t, app, pushed); status != models.DeliverySent {
		t.Fatalf("delivery %q", status)
	}
	if status, _ := pushDelivery(t, app, quiet); status != "" {
		t.Fatalf("a kind that doesn't push has a push delivery %q", status)
	}
}

// A real cancellation, through the handler that writes it, reaches a
// registrant's phone: notify.Send callers need no change to push.
func TestCancellingADatePushesToRegistrants(t *testing.T) {
	app, f := pushTestApp(t)
	leader := newTestUser(t, app, "leader", "password123456", models.RoleMember)
	g := newTestGroup(t, app, leader, "Supper club", models.GroupActive)
	_, occ := seedEvent(t, app, g, leader.ID, time.Now().Add(5*time.Hour).Truncate(time.Minute))
	app.DB.Model(&models.Event{}).Where("id = ?", occ.EventID).Update("location", "14 Elm Street")
	pat := pushMember(t, app, "pat")
	phone := newPushDevice(t, app, pat, "phone")
	if err := app.DB.Create(&models.Registration{OccurrenceID: occ.ID, UserID: &pat.ID, Status: models.RegGoing}).Error; err != nil {
		t.Fatal(err)
	}
	before := maxNoticeID(t, app.DB)
	err := app.DB.Transaction(func(tx *gorm.DB) error {
		return notifyOccurrences(tx, leader.ID, []uint{occ.ID}, notify.OccurrenceCancelled)
	})
	ids := noticeIDsAfter(t, app.DB, before)
	if err != nil || len(ids) != 1 {
		t.Fatalf("cancel notices %v %v", ids, err)
	}
	drainNotices(t, app)
	drainJobs(t, app)
	if f.count(phone.path) != 1 {
		t.Fatalf("sends %v", f.sent)
	}
	// Today's date: urgent and short-lived (research 2.4).
	h := f.headers[0]
	if h.Get("TTL") != "21600" || h.Get("Urgency") != "high" {
		t.Fatalf("headers %v", h)
	}
	var n models.Notification
	app.DB.First(&n, ids[0])
	// The bell has the place; the lock screen doesn't.
	if n.PushTitle != "Cancelled: Supper club" || n.PushBody == "" || strings.Contains(n.PushBody, "Elm") || !strings.Contains(n.Body, "Elm") {
		t.Fatalf("push text %q %q (bell %q)", n.PushTitle, n.PushBody, n.Body)
	}
}

// TTL and urgency follow the kind (research 2.4): a change to a date in
// the next 24 hours is high for 6 hours, a later one normal for a day, and
// an admin task low for 3 days.
func TestPushOptionsFollowTheKind(t *testing.T) {
	app := testApp(t)
	leader := newTestUser(t, app, "leader", "password123456", models.RoleMember)
	g := newTestGroup(t, app, leader, "Supper club", models.GroupActive)
	_, soon := seedEvent(t, app, g, leader.ID, time.Now().Add(23*time.Hour))
	_, later := seedEvent(t, app, g, leader.ID, time.Now().Add(72*time.Hour))
	for _, c := range []struct {
		kind    string
		subject *uint
		ttl     time.Duration
		urgency push.Urgency
	}{
		{notify.OccurrenceCancelled, &soon.ID, 6 * time.Hour, push.UrgencyHigh},
		{notify.EventChanged, &soon.ID, 6 * time.Hour, push.UrgencyHigh},
		{notify.OccurrenceCancelled, &later.ID, 24 * time.Hour, push.UrgencyNormal},
		{notify.RegistrationPromoted, &soon.ID, 6 * time.Hour, push.UrgencyHigh},
		{notify.RegistrationPromoted, &later.ID, 24 * time.Hour, push.UrgencyNormal},
		{notify.GroupApproved, &soon.ID, 24 * time.Hour, push.UrgencyNormal},
		{notify.GroupApproved, nil, 24 * time.Hour, push.UrgencyNormal},
		{notify.GroupPending, nil, 72 * time.Hour, push.UrgencyLow},
		{notify.AccountRequested, nil, 72 * time.Hour, push.UrgencyLow},
	} {
		k, _ := notify.Lookup(c.kind)
		n := &models.Notification{ID: 42, Kind: c.kind, SubjectType: "occurrence", SubjectID: c.subject}
		soon, _, err := app.startsSoon(context.Background(), k, n, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		got := pushOptions(k, n, soon)
		if got.TTL != c.ttl || got.Urgency != c.urgency || got.Topic != "n42" {
			t.Errorf("%s: %+v", c.kind, got)
		}
	}
}

func TestPushBodyIsDeclarativeWebPush(t *testing.T) {
	app := testApp(t)
	app.Config.SiteURL = "https://church.example/"
	n := &models.Notification{ID: 7, Title: "Youth was approved", Link: "/groups/1"}
	b, err := app.pushBody(n, push.Notification{Title: n.Title, Body: "Tonight", Navigate: "https://elsewhere.example/"})
	want := `{"web_push":8030,"notification":{"title":"Youth was approved","body":"Tonight","navigate":"https://church.example/groups/1","tag":"n7"}}`
	if err != nil || string(b) != want {
		t.Fatalf("payload %s (%v)", b, err)
	}
	if _, err := app.pushBody(n, push.Notification{Title: strings.Repeat("a", push.MaxPlaintext)}); !errors.Is(err, push.ErrTooLarge) {
		t.Fatalf("over the cap: %v", err)
	}
}

// TestPushLogsKeepSecretsOut: sends that fail every way leave no endpoint,
// key, auth secret or token in the log, the jobs table or the delivery rows.
func TestPushLogsKeepSecretsOut(t *testing.T) {
	var logs bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(prev) })

	app, f := pushTestApp(t)
	u := pushMember(t, app, "pat")
	var devices []testDevice
	for i, code := range []int{500, 429, 403, 404, 413, 0} {
		d := newPushDevice(t, app, u, fmt.Sprintf("device%d", i))
		if code == 0 {
			f.err[d.path] = true
		} else {
			f.set(d.path, code)
		}
		devices = append(devices, d)
	}
	n := sendNotice(t, app, notify.GroupApproved, u)
	runPushJobs(t, app, n, devices...)
	// Through to the last attempt, so dead jobs keep their final error.
	for range pushDeliverType.Attempts() {
		app.DB.Model(&models.Job{}).Where("status = ?", models.JobFailed).Update("run_at", time.Now().Add(-time.Second))
		drainJobs(t, app)
	}
	var stored []string
	var js []models.Job
	app.DB.Find(&js)
	for _, j := range js {
		stored = append(stored, j.LastError, j.Payload)
	}
	var ds []models.NotificationDelivery
	app.DB.Find(&ds)
	for _, d := range ds {
		stored = append(stored, d.Reason)
	}
	var as []models.AuditLog
	app.DB.Find(&as)
	for _, a := range as {
		stored = append(stored, a.MetaJSON)
	}
	haystack := logs.String() + "\n" + strings.Join(stored, "\n")
	if !strings.Contains(haystack, "push") {
		t.Fatalf("nothing was logged or stored; the test isn't looking at anything:\n%s", haystack)
	}
	var secrets []string
	for _, d := range devices {
		secrets = append(secrets, d.Endpoint, strings.TrimPrefix(d.path, "/fcm/send/"), d.P256dh, d.Auth)
	}
	for _, h := range f.headers {
		token, _, _ := strings.Cut(strings.TrimPrefix(h.Get("Authorization"), "vapid t="), ",")
		// The token, and its claims and signature on their own.
		secrets = append(append(secrets, token), strings.Split(token, ".")[1:]...)
	}
	for _, s := range secrets {
		if s != "" && strings.Contains(haystack, s) {
			t.Errorf("a secret reached the logs or the database: %q", s)
		}
	}
	if !strings.Contains(logs.String(), "device gone for user") {
		t.Errorf("expected the gone log line:\n%s", logs.String())
	}
	if t.Failed() {
		t.Logf("logs and stored text:\n%s", haystack)
	}
}

func TestPushDevicesArePruned(t *testing.T) {
	app := testApp(t)
	u := newTestUser(t, app, "pat", "password123456", models.RoleMember)
	now := time.Now()
	keep, recent := newPushDevice(t, app, u, "keep"), newPushDevice(t, app, u, "recent")
	failing, expired := newPushDevice(t, app, u, "failing"), newPushDevice(t, app, u, "expired")
	app.DB.Model(&models.PushSubscription{}).Where("id = ?", recent.ID).Update("failed_at", now.Add(-29*24*time.Hour))
	app.DB.Model(&models.PushSubscription{}).Where("id = ?", failing.ID).Update("failed_at", now.Add(-31*24*time.Hour))
	app.DB.Model(&models.PushSubscription{}).Where("id = ?", expired.ID).Update("expires_at", now.Add(-time.Minute))
	app.DB.Model(&models.PushSubscription{}).Where("id = ?", keep.ID).Update("expires_at", now.Add(time.Hour))
	n, err := prunePushDevices(app.DB, now)
	if err != nil || n != 2 || !deviceExists(app, keep.ID) || !deviceExists(app, recent.ID) || deviceExists(app, failing.ID) || deviceExists(app, expired.ID) {
		t.Fatalf("pruned %d (%v)", n, err)
	}
	// It runs as part of the daily notifications.cleanup.
	gone := newPushDevice(t, app, u, "gone")
	app.DB.Model(&models.PushSubscription{}).Where("id = ?", gone.ID).Update("failed_at", now.Add(-40*24*time.Hour))
	for _, k := range app.dailyKinds() {
		if k.Name == "notifications.cleanup" {
			if err := k.Run(context.Background(), nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	if deviceExists(app, gone.ID) {
		t.Fatal("the daily cleanup kept a device failing for 40 days")
	}
}

func TestPushDeliverIsABackgroundKind(t *testing.T) {
	app := testApp(t)
	if _, ok := app.jobKinds()[kindPushDeliver]; !ok {
		t.Fatal("push.deliver isn't registered with the runner")
	}
}

// quietClock is a push test's fake time (DEC-126): quiet hours on, the
// clock at 11 PM in Denver on a night in the real future, so a job that
// waits for 8 AM is never due by the database's clock until a test says
// so (makeDue).
type quietClock struct{ at time.Time }

var denverZone = func() *time.Location {
	loc, err := time.LoadLocation("America/Denver")
	if err != nil {
		panic(err)
	}
	return loc
}()

func quietHours(app *App) *quietClock {
	c := &quietClock{at: time.Date(2030, 1, 15, 23, 0, 0, 0, denverZone)}
	w := push.DefaultWindow
	app.hooks.pushWindow = &w
	app.clock = func() time.Time { return c.at }
	return c
}

func (c *quietClock) set(h, m int, day int) { c.at = time.Date(2030, 1, day, h, m, 0, 0, denverZone) }

// makeDue moves every waiting push job's run_at into the database's past.
func makeDue(app *App) {
	app.DB.Model(&models.Job{}).Where("kind = ? AND status = ?", kindPushDeliver, models.JobQueued).Update("run_at", time.Now().Add(-time.Second))
}

func denverMember(t *testing.T, app *App, name string) *models.User {
	t.Helper()
	u := pushMember(t, app, name)
	if err := app.DB.Model(u).Update("timezone", "America/Denver").Error; err != nil {
		t.Fatal(err)
	}
	return u
}

func pushJobFor(t *testing.T, app *App, notificationID uint, device testDevice) models.Job {
	t.Helper()
	var j models.Job
	if err := app.DB.Where("unique_key = ?", fmt.Sprintf("push:n:%d:d:%d", notificationID, device.ID)).First(&j).Error; err != nil {
		t.Fatal(err)
	}
	return j
}

// An optional push at 11 PM in the recipient's zone waits, without using
// an attempt, and sends at 8 AM; running it again in the night still
// waits, and it never sends twice. A person with no zone of their own
// waits for the church's 8 AM.
func TestOptionalPushWaitsForTheMorning(t *testing.T) {
	app, f := pushTestApp(t)
	clock := quietHours(app)
	app.DB.Create(&models.Setting{Key: settingTimezone, Value: "America/Denver"})
	pat := denverMember(t, app, "pat")
	lee := pushMember(t, app, "lee") // no zone of their own: the church's
	phone, leePhone := newPushDevice(t, app, pat, "phone"), newPushDevice(t, app, lee, "lee-phone")
	n := sendNotice(t, app, notify.GroupApproved, pat)
	m := sendNotice(t, app, notify.GroupApproved, lee)
	runPushJobs(t, app, n, phone)
	runPushJobs(t, app, m, leePhone)
	morning := time.Date(2030, 1, 16, 8, 0, 0, 0, denverZone)
	for _, c := range []struct {
		id  uint
		dev testDevice
	}{{n, phone}, {m, leePhone}} {
		j := pushJobFor(t, app, c.id, c.dev)
		if j.Status != models.JobQueued || !j.RunAt.Equal(morning) || j.Attempts != 0 || j.LastError != "" {
			t.Fatalf("waiting job %+v, want queued for %s", j, morning)
		}
		if status, _ := pushDelivery(t, app, c.id); status != "" {
			t.Fatalf("a waiting push has a delivery row %q", status)
		}
	}
	// Due again at 11:30 PM (a clock or a lease, say): it waits again.
	clock.set(23, 30, 15)
	makeDue(app)
	drainJobs(t, app)
	if f.total() != 0 {
		t.Fatalf("sent in the night: %v", f.sent)
	}
	if j := pushJobFor(t, app, n, phone); !j.RunAt.Equal(morning) || j.Status != models.JobQueued {
		t.Fatalf("after a second run in the night: %+v", j)
	}
	clock.set(8, 0, 16)
	makeDue(app)
	drainJobs(t, app)
	if f.count(phone.path) != 1 || f.count(leePhone.path) != 1 {
		t.Fatalf("sends at 8 AM %v", f.sent)
	}
	if status, reason := pushDelivery(t, app, n); status != models.DeliverySent || reason != "" {
		t.Fatalf("delivery %q %q", status, reason)
	}
	// Queuing and running it again sends nothing more.
	runPushJobs(t, app, n, phone)
	makeDue(app)
	drainJobs(t, app)
	if f.count(phone.path) != 1 {
		t.Fatalf("phone got it %d times", f.count(phone.path))
	}
}

// Quiet hours are the recipient's own: 11 PM in Denver is mid-afternoon
// in Tokyo.
func TestQuietHoursAreTheRecipients(t *testing.T) {
	app, f := pushTestApp(t)
	quietHours(app)
	app.DB.Create(&models.Setting{Key: settingTimezone, Value: "America/Denver"})
	kenji := pushMember(t, app, "kenji")
	app.DB.Model(kenji).Update("timezone", "Asia/Tokyo")
	phone := newPushDevice(t, app, kenji, "phone")
	n := sendNotice(t, app, notify.GroupApproved, kenji)
	runPushJobs(t, app, n, phone)
	if f.count(phone.path) != 1 {
		t.Fatalf("sends %v", f.sent)
	}
}

// A required kind, and a change to a date starting within 24 hours, go at
// 11 PM; a change to a date three days away waits for 8 AM.
func TestUrgentPushesSkipQuietHours(t *testing.T) {
	app, f := pushTestApp(t)
	clock := quietHours(app)
	pat := denverMember(t, app, "pat")
	phone := newPushDevice(t, app, pat, "phone")

	required := sendNotice(t, app, notify.AccountTurned18, pat)
	runPushJobs(t, app, required, phone)
	if status, reason := pushDelivery(t, app, required); status != models.DeliverySent || reason != pushSentRequired || f.count(phone.path) != 1 {
		t.Fatalf("required: %q %q after %d sends", status, reason, f.count(phone.path))
	}

	leader := newTestUser(t, app, "leader", "password123456", models.RoleMember)
	g := newTestGroup(t, app, leader, "Supper club", models.GroupActive)
	_, tonight := seedEvent(t, app, g, leader.ID, clock.at.Add(3*time.Hour))
	_, later := seedEvent(t, app, g, leader.ID, clock.at.Add(72*time.Hour))
	change := func(occ models.EventOccurrence) uint {
		ids, err := notify.Send(app.DB, notify.Msg{Kind: notify.EventChanged, To: []uint{pat.ID}, SubjectType: "occurrence", SubjectID: occ.ID,
			Link: "/events/1", Vars: notify.Vars{Event: "Supper club", Change: "time"}})
		if err != nil || len(ids) != 1 {
			t.Fatalf("notify: %v %v", ids, err)
		}
		return ids[0]
	}
	soon := change(tonight)
	runPushJobs(t, app, soon, phone)
	if status, reason := pushDelivery(t, app, soon); status != models.DeliverySent || reason != pushSentSoon || f.count(phone.path) != 2 {
		t.Fatalf("3 hours away: %q %q after %d sends", status, reason, f.count(phone.path))
	}
	if h := f.headers[len(f.headers)-1]; h.Get("Urgency") != "high" {
		t.Fatalf("3 hours away is urgent: %v", h)
	}
	waits := change(later)
	runPushJobs(t, app, waits, phone)
	if f.count(phone.path) != 2 {
		t.Fatalf("3 days away sent in the night: %v", f.sent)
	}
	if j := pushJobFor(t, app, waits, phone); j.Status != models.JobQueued || !j.RunAt.Equal(time.Date(2030, 1, 16, 8, 0, 0, 0, denverZone)) {
		t.Fatalf("3 days away: %+v", j)
	}
	// Cancelled and restored dates, "you're in" and a removal are changes
	// to that date too; an approved group names no date.
	for kind, exempt := range map[string]bool{notify.OccurrenceCancelled: true, notify.OccurrenceRestored: true,
		notify.RegistrationPromoted: true, notify.RegistrationRemoved: true, notify.GroupApproved: false} {
		k, _ := notify.Lookup(kind)
		got, _, err := app.startsSoon(context.Background(), k, &models.Notification{Kind: kind, SubjectType: "occurrence", SubjectID: &tonight.ID}, clock.at)
		if err != nil || got != exempt {
			t.Errorf("%s 3 hours away: soon %v (%v), want %v", kind, got, err, exempt)
		}
		if got, _, _ := app.startsSoon(context.Background(), k, &models.Notification{Kind: kind, SubjectType: "occurrence", SubjectID: &later.ID}, clock.at); got {
			t.Errorf("%s 3 days away is soon", kind)
		}
	}
}

// A "you're in" off the waitlist at 11 PM for a 7 AM date goes at once,
// as does a lost room whose earliest date is that morning; a lost room
// only on later dates waits (DEC-126).
func TestDateChangesForTheMorningSkipQuietHours(t *testing.T) {
	app, f := pushTestApp(t)
	clock := quietHours(app)
	pat := denverMember(t, app, "pat")
	phone := newPushDevice(t, app, pat, "phone")
	leader := newTestUser(t, app, "leader", "password123456", models.RoleMember)
	g := newTestGroup(t, app, leader, "Supper club", models.GroupActive)
	ev, morning := seedEvent(t, app, g, leader.ID, time.Date(2030, 1, 16, 7, 0, 0, 0, denverZone))
	_, far := seedEvent(t, app, g, leader.ID, clock.at.Add(72*time.Hour))
	farLater := models.EventOccurrence{EventID: ev.ID, OriginalDate: calDate("2030-01-25"), StartsAt: clock.at.Add(10 * 24 * time.Hour),
		EndsAt: clock.at.Add(10*24*time.Hour + time.Hour), Status: models.OccurrenceScheduled}
	if err := app.DB.Create(&farLater).Error; err != nil {
		t.Fatal(err)
	}
	send := func(m notify.Msg) uint {
		m.To, m.Link = []uint{pat.ID}, "/events/1"
		m.Vars = notify.Vars{Event: "Supper club", Change: "1 date"}
		ids, err := notify.Send(app.DB, m)
		if err != nil || len(ids) != 1 {
			t.Fatalf("notify: %v %v", ids, err)
		}
		return ids[0]
	}
	in := send(notify.Msg{Kind: notify.RegistrationPromoted, SubjectType: "occurrence", SubjectID: morning.ID})
	roomSoon := send(notify.Msg{Kind: notify.RoomLost, SubjectType: "event", SubjectID: ev.ID,
		Data: map[string]any{"eventId": ev.ID, "dates": []string{models.FormatCalendarDate(farLater.OriginalDate), models.FormatCalendarDate(morning.OriginalDate)}}})
	roomLater := send(notify.Msg{Kind: notify.RoomLost, SubjectType: "event", SubjectID: far.EventID,
		Data: map[string]any{"eventId": far.EventID, "dates": []string{models.FormatCalendarDate(far.OriginalDate)}}})
	for _, id := range []uint{in, roomSoon, roomLater} {
		runPushJobs(t, app, id, phone)
	}
	for _, id := range []uint{in, roomSoon} {
		if status, reason := pushDelivery(t, app, id); status != models.DeliverySent || reason != pushSentSoon {
			t.Errorf("notification %d: %q %q, want sent at once", id, status, reason)
		}
	}
	if f.count(phone.path) != 2 {
		t.Fatalf("sends %v", f.sent)
	}
	if j := pushJobFor(t, app, roomLater, phone); j.Status != models.JobQueued || !j.RunAt.Equal(time.Date(2030, 1, 16, 8, 0, 0, 0, denverZone)) {
		t.Fatalf("a lost room 3 days away: %+v", j)
	}
}

// A push that waited the night rechecks everything at 8 AM: push turned
// off, a device gone, an item read, archived or expired since sends
// nothing, and the delivery row says why (a device that's gone leaves the
// row alone, as before quiet hours).
func TestWaitingPushRechecksInTheMorning(t *testing.T) {
	app, f := pushTestApp(t)
	clock := quietHours(app)
	off, gone, reader := denverMember(t, app, "off"), denverMember(t, app, "gone"), denverMember(t, app, "reader")
	offPhone, gonePhone, readerPhone := newPushDevice(t, app, off, "off-phone"), newPushDevice(t, app, gone, "gone-phone"), newPushDevice(t, app, reader, "reader-phone")
	nOff := sendNotice(t, app, notify.GroupApproved, off)
	nGone := sendNotice(t, app, notify.GroupApproved, gone)
	nRead := sendNotice(t, app, notify.GroupApproved, reader)
	nArchived := sendNotice(t, app, notify.GroupApproved, reader)
	nExpired := sendNotice(t, app, notify.GroupApproved, reader)
	runPushJobs(t, app, nOff, offPhone)
	runPushJobs(t, app, nGone, gonePhone)
	for _, id := range []uint{nRead, nArchived, nExpired} {
		runPushJobs(t, app, id, readerPhone)
	}
	if f.total() != 0 {
		t.Fatalf("sent in the night: %v", f.sent)
	}
	// Overnight.
	pushConsentRow(t, app, off, "all", models.ConsentOptedOut)
	app.DB.Delete(&models.PushSubscription{}, gonePhone.ID)
	app.DB.Model(&models.Notification{}).Where("id = ?", nRead).Update("read_at", clock.at)
	app.DB.Model(&models.Notification{}).Where("id = ?", nArchived).Update("archived_at", clock.at)
	app.DB.Model(&models.Notification{}).Where("id = ?", nExpired).Update("expires_at", clock.at.Add(time.Hour))

	clock.set(8, 0, 16)
	makeDue(app)
	drainJobs(t, app)
	if f.total() != 0 {
		t.Fatalf("sends %v", f.sent)
	}
	// A device gone overnight, with no other device to take it, settles its
	// notice's push as "device gone".
	for id, want := range map[uint]string{nOff: pushWhyNotOn, nGone: pushWhyGone, nRead: pushWhyRead, nArchived: pushWhyStale, nExpired: pushWhyStale} {
		if status, reason := pushDelivery(t, app, id); status != models.DeliverySuppressed || reason != want {
			t.Errorf("notification %d: %q %q, want suppressed %q", id, status, reason, want)
		}
	}
	var notDone int64
	app.DB.Model(&models.Job{}).Where("kind = ? AND status <> ?", kindPushDeliver, models.JobDone).Count(&notDone)
	if notDone != 0 {
		t.Fatalf("%d push jobs not done", notDone)
	}
}

// At most 20 optional pushes an hour: one notification on two devices
// counts once, the 21st is suppressed "hourly limit" with its bell row
// untouched, required kinds and urgent event changes still go and aren't
// counted, and an hour later pushes flow again.
func TestPushHourlyLimit(t *testing.T) {
	app, f := pushTestApp(t)
	clock := quietHours(app)
	clock.set(10, 0, 16)
	pat := denverMember(t, app, "pat")
	phone, laptop := newPushDevice(t, app, pat, "phone"), newPushDevice(t, app, pat, "laptop")
	required := sendNotice(t, app, notify.AccountTurned18, pat)
	runPushJobs(t, app, required, phone, laptop)
	for i := 1; i <= pushHourlyLimit; i++ {
		clock.at = clock.at.Add(time.Minute)
		n := sendNotice(t, app, notify.GroupApproved, pat)
		runPushJobs(t, app, n, phone, laptop)
		if status, _ := pushDelivery(t, app, n); status != models.DeliverySent {
			t.Fatalf("push %d: %q", i, status)
		}
	}
	if f.count(phone.path) != pushHourlyLimit+1 || f.count(laptop.path) != pushHourlyLimit+1 {
		t.Fatalf("both devices get the first 20 and the required one: %v", f.sent)
	}
	over := sendNotice(t, app, notify.GroupApproved, pat)
	runPushJobs(t, app, over, phone, laptop)
	if status, reason := pushDelivery(t, app, over); status != models.DeliverySuppressed || reason != pushWhyHourly {
		t.Fatalf("21st: %q %q", status, reason)
	}
	if f.count(phone.path) != pushHourlyLimit+1 || f.count(laptop.path) != pushHourlyLimit+1 {
		t.Fatalf("the 21st was sent: %v", f.sent)
	}
	var bell models.Notification
	if err := app.DB.First(&bell, over).Error; err != nil || bell.ReadAt != nil || bell.ArchivedAt != nil {
		t.Fatalf("the bell row: %+v (%v)", bell, err)
	}
	// Not limited: a required kind, and a change to a date tonight.
	again := sendNotice(t, app, notify.AccountTurned18, pat)
	runPushJobs(t, app, again, phone)
	leader := newTestUser(t, app, "leader", "password123456", models.RoleMember)
	g := newTestGroup(t, app, leader, "Supper club", models.GroupActive)
	_, tonight := seedEvent(t, app, g, leader.ID, clock.at.Add(3*time.Hour))
	ids, err := notify.Send(app.DB, notify.Msg{Kind: notify.OccurrenceCancelled, To: []uint{pat.ID}, SubjectType: "occurrence", SubjectID: tonight.ID,
		Link: "/events/1", Vars: notify.Vars{Event: "Supper club"}})
	if err != nil || len(ids) != 1 {
		t.Fatalf("notify: %v %v", ids, err)
	}
	runPushJobs(t, app, ids[0], phone)
	for _, id := range []uint{again, ids[0]} {
		if status, _ := pushDelivery(t, app, id); status != models.DeliverySent {
			t.Fatalf("exempt push %d: %q", id, status)
		}
	}
	// An hour after the first optional push, one slot is free again.
	clock.at = clock.at.Add(41 * time.Minute)
	later := sendNotice(t, app, notify.GroupApproved, pat)
	runPushJobs(t, app, later, phone)
	if status, _ := pushDelivery(t, app, later); status != models.DeliverySent {
		t.Fatalf("an hour later: %q", status)
	}
}

// A change to a date that's already over doesn't push, urgently or in the
// morning: at 3 AM it is suppressed "no longer current" and nothing waits.
// A lost room with one date over and one still ahead still counts the
// one ahead.
func TestPushForADateAlreadyOverIsStale(t *testing.T) {
	app, f := pushTestApp(t)
	clock := quietHours(app)
	clock.set(3, 0, 16)
	pat := denverMember(t, app, "pat")
	phone := newPushDevice(t, app, pat, "phone")
	leader := newTestUser(t, app, "leader", "password123456", models.RoleMember)
	g := newTestGroup(t, app, leader, "Supper club", models.GroupActive)
	ev, done := seedEvent(t, app, g, leader.ID, clock.at.Add(-3*time.Hour)) // ended at 1 AM
	ahead := models.EventOccurrence{EventID: ev.ID, OriginalDate: calDate("2030-01-17"), StartsAt: clock.at.Add(5 * time.Hour),
		EndsAt: clock.at.Add(6 * time.Hour), Status: models.OccurrenceScheduled}
	if err := app.DB.Create(&ahead).Error; err != nil {
		t.Fatal(err)
	}
	ids, err := notify.Send(app.DB, notify.Msg{Kind: notify.EventChanged, To: []uint{pat.ID}, SubjectType: "occurrence", SubjectID: done.ID,
		Link: "/events/1", Vars: notify.Vars{Event: "Supper club", Change: "place"}})
	if err != nil || len(ids) != 1 {
		t.Fatalf("notify: %v %v", ids, err)
	}
	runPushJobs(t, app, ids[0], phone)
	if status, reason := pushDelivery(t, app, ids[0]); status != models.DeliverySuppressed || reason != pushWhyStale {
		t.Fatalf("a finished date: %q %q", status, reason)
	}
	if j := pushJobFor(t, app, ids[0], phone); j.Status != models.JobDone || f.total() != 0 {
		t.Fatalf("a finished date: job %+v, sends %v", j, f.sent)
	}
	k, _ := notify.Lookup(notify.RoomLost)
	both := &models.Notification{Kind: notify.RoomLost, SubjectType: "event", SubjectID: &ev.ID,
		Data: fmt.Sprintf(`{"dates": [%q, %q]}`, models.FormatCalendarDate(done.OriginalDate), models.FormatCalendarDate(ahead.OriginalDate))}
	if soon, over, err := app.startsSoon(context.Background(), k, both, clock.at); err != nil || !soon || over {
		t.Fatalf("one date over, one ahead: soon %v over %v (%v)", soon, over, err)
	}
	onlyDone := &models.Notification{Kind: notify.RoomLost, SubjectType: "event", SubjectID: &ev.ID,
		Data: fmt.Sprintf(`{"dates": [%q]}`, models.FormatCalendarDate(done.OriginalDate))}
	if soon, over, err := app.startsSoon(context.Background(), k, onlyDone, clock.at); err != nil || soon || !over {
		t.Fatalf("only a date over: soon %v over %v (%v)", soon, over, err)
	}
}

// When a notification's push row is already sent, its other devices get
// it even if, by then, the hour's other pushes reached 20 (workers raced
// past the limit): a second device is never silently skipped. A new
// notification is still limited.
func TestPushSecondDeviceIsNeverLimited(t *testing.T) {
	app, f := pushTestApp(t)
	clock := quietHours(app)
	clock.set(10, 0, 16)
	pat := denverMember(t, app, "pat")
	laptop := newPushDevice(t, app, pat, "laptop")
	for i := 0; i < pushHourlyLimit; i++ {
		other := sendNotice(t, app, notify.GroupApproved, pat)
		if err := recordPushIn(app.DB, models.Notification{ID: other, UserID: pat.ID}, models.DeliverySent, "", clock.at); err != nil {
			t.Fatal(err)
		}
	}
	n := sendNotice(t, app, notify.GroupApproved, pat)
	// The phone took n in the race.
	if err := recordPushIn(app.DB, models.Notification{ID: n, UserID: pat.ID}, models.DeliverySent, "", clock.at); err != nil {
		t.Fatal(err)
	}
	runPushJobs(t, app, n, laptop)
	if f.count(laptop.path) != 1 {
		t.Fatalf("the second device was skipped: %v", f.sent)
	}
	fresh := sendNotice(t, app, notify.GroupApproved, pat)
	runPushJobs(t, app, fresh, laptop)
	if status, reason := pushDelivery(t, app, fresh); status != models.DeliverySuppressed || reason != pushWhyHourly || f.count(laptop.path) != 1 {
		t.Fatalf("a new notification past the limit: %q %q, sends %v", status, reason, f.sent)
	}
}

// pushPayload decrypts the last push the fake service got for dev (RFC
// 8291, the receiver's side, written apart from internal/push's encrypt),
// so a test can read the lock screen Fold really sent.
func pushPayload(t *testing.T, f *fakePush, dev testDevice) push.Notification {
	t.Helper()
	f.mu.Lock()
	body := f.bodies[dev.path]
	f.mu.Unlock()
	if len(body) < 21 || len(body) < 21+int(body[20]) {
		t.Fatalf("no push body for %s", dev.Label)
	}
	salt, idlen := body[:16], int(body[20])
	if binary.BigEndian.Uint32(body[16:20]) < 18 {
		t.Fatal("bad record size")
	}
	asPublic, ciphertext := body[21:21+idlen], body[21+idlen:]
	sender, err := ecdh.P256().NewPublicKey(asPublic)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := dev.key.ECDH(sender)
	if err != nil {
		t.Fatal(err)
	}
	info := append(append([]byte("WebPush: info\x00"), dev.key.PublicKey().Bytes()...), asPublic...)
	ikm, err := hkdf.Key(sha256.New, shared, dev.auth, string(info), 32)
	if err != nil {
		t.Fatal(err)
	}
	cek, _ := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	nonce, _ := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
	block, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(block)
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		t.Fatal(err)
	}
	i := len(plain) - 1
	for i >= 0 && plain[i] == 0 {
		i--
	}
	if i < 0 || plain[i] != 0x02 {
		t.Fatal("no last-record delimiter")
	}
	var p push.Payload
	if err := json.Unmarshal(plain[:i], &p); err != nil {
		t.Fatal(err)
	}
	return p.Notification
}

// N-06 (inventory 9.2.2, F-3): push.devices_off used to check the user and
// write the notice as separate autocommitted statements. The check, the
// notify.Send (which locks the recipient and rechecks deleted_at) and the
// delivery intent now share one transaction. A person who is living or merely
// disabled is told once; one erased before, or while the job is between its
// check and its write, is not; and an erasure that starts while the notice is
// being written waits for it and then removes it.
func TestNoticeDevicesOffRunsInOneTransaction(t *testing.T) {
	run := func(job devicesOffJob) func(*gorm.DB) error {
		return func(db *gorm.DB) error { return (&App{DB: db}).noticeDevicesOff(db.Statement.Context, job) }
	}
	noticesFor := func(app *App, u *models.User) int64 {
		return rowsFor(t, app.DB, "notifications", u.ID)
	}
	t.Run("living and disabled people are told once", func(t *testing.T) {
		app := testApp(t)
		living := newTestUser(t, app, "living", "password123456", models.RoleMember)
		disabled := newTestUser(t, app, "disabled", "password123456", models.RoleMember)
		if err := app.DB.Model(disabled).Update("status", models.UserDisabled).Error; err != nil {
			t.Fatal(err)
		}
		for _, u := range []*models.User{living, disabled} {
			for range 2 {
				if err := app.noticeDevicesOff(context.Background(), devicesOffJob{UserID: u.ID, Devices: 2, Key: "k-" + u.Name}); err != nil {
					t.Fatal(err)
				}
			}
			if n := noticesFor(app, u); n != 1 {
				t.Fatalf("%s has %d notices, want 1", u.Name, n)
			}
		}
		if deliveries := totalRows(t, app.DB, "notification_deliveries"); deliveries != 0 {
			t.Fatalf("the notice is bell only: %d delivery rows", deliveries)
		}
	})
	t.Run("erased before the job", func(t *testing.T) {
		app := testApp(t)
		u := newTestUser(t, app, "gone", "password123456", models.RoleMember)
		eraseNoticesNow(t, app.DB, u.ID)
		if err := app.noticeDevicesOff(context.Background(), devicesOffJob{UserID: u.ID, Devices: 1, Key: "k"}); err != nil {
			t.Fatal(err)
		}
		if n := noticesFor(app, u); n != 0 {
			t.Fatalf("%d notices for an erased person", n)
		}
	})
	t.Run("erasure commits between the check and the write", func(t *testing.T) {
		app := testApp(t)
		u := newTestUser(t, app, "racer", "password123456", models.RoleMember)
		eraser, ePID := beginOwnedTx(t, app.DB)
		if err := eraseNoticesTx(eraser, u.ID); err != nil {
			t.Fatal(err)
		}
		// The job's read of the user sees the committed, living row; only the
		// lock inside Send meets the pending erasure.
		pid, done := startDBSession(t, app.DB, "devices-off", run(devicesOffJob{UserID: u.ID, Devices: 1, Key: "k"}))
		waitDBBlocked(t, app.DB, pid, ePID)
		if err := eraser.Commit().Error; err != nil {
			t.Fatal(err)
		}
		if err := awaitDBResult(t, done); err != nil {
			t.Fatal(err)
		}
		if n := noticesFor(app, u); n != 0 {
			t.Fatalf("a notice was written for the person erased mid-job: %d", n)
		}
	})
	t.Run("erasure starts while the notice is written", func(t *testing.T) {
		app := testApp(t)
		u := newTestUser(t, app, "racer", "password123456", models.RoleMember)
		const key = 7401002
		pauseOnStatement(t, app.DB, "AFTER INSERT", "notifications", key)
		release, hPID := holdAdvisory(t, app.DB, key)
		pid, done := startDBSession(t, app.DB, "devices-off", run(devicesOffJob{UserID: u.ID, Devices: 1, Key: "k"}))
		waitDBBlocked(t, app.DB, pid, hPID) // the notice is inserted; the job's transaction is open
		eraser, ePID := beginOwnedTx(t, app.DB)
		erased := make(chan error, 1)
		go func() { erased <- eraseNoticesTx(eraser, u.ID) }()
		waitDBBlocked(t, app.DB, ePID, pid)
		release()
		if err := awaitDBResult(t, done); err != nil {
			t.Fatal(err)
		}
		if err := awaitDBResult(t, erased); err != nil {
			t.Fatal(err)
		}
		if err := eraser.Commit().Error; err != nil {
			t.Fatal(err)
		}
		if n := noticesFor(app, u); n != 0 {
			t.Fatalf("the erasure left %d notices", n)
		}
	})
}

// TestPushOutcomeLeavesAChangedDeviceAlone: an announcement push's outcome is
// written only to the device as it was dispatched (row, owner and keys). While
// the push service holds the request, the shared browser is rebound to another
// account, or its owner renews the subscription with new keys, and commits.
// "Gone" must not delete the changed device, a refusal must not mark it
// failed, and a delivery must not clear its failure; the old attempt's row
// still gets its own outcome. A Gone answer about a changed device retries,
// so a renewed subscription gets the push and a rebound one is decided as a
// moved device; about a device already deleted meanwhile, it is "device gone"
// at once. "rebind back" hands the browser to the next holder and back to its
// owner through the real subscribe route before the answer comes: row, owner
// and keys match the dispatch again, but it is a new binding, so the stale
// answer still changes nothing and the retry goes to the owner's binding now.
// "renew back" is the same for one owner: the route saves new keys and then
// the old pair again. "same clock" is "rebind back" with created_at set back
// to its value at dispatch, so only the binding generation tells them apart.
func TestPushOutcomeLeavesAChangedDeviceAlone(t *testing.T) {
	type outcome struct {
		code   int
		change string
	}
	for _, tc := range []outcome{
		{http.StatusGone, "rebind"}, {http.StatusGone, "renew"}, {http.StatusGone, "remove"}, {http.StatusGone, "rebind back"},
		{http.StatusGone, "renew back"}, {http.StatusGone, "same clock"}, {http.StatusGone, "saved again"},
		{http.StatusBadRequest, "rebind"}, {http.StatusBadRequest, "rebind back"}, {http.StatusBadRequest, "renew back"},
		{http.StatusBadRequest, "same clock"}, {http.StatusCreated, "rebind"},
	} {
		t.Run(fmt.Sprintf("%d after %s", tc.code, tc.change), func(t *testing.T) {
			app, fp := pushTestApp(t)
			u := pushMember(t, app, "owner")
			other := pushMember(t, app, "nextholder")
			dev := newPushDevice(t, app, u, "shared")
			ownerC, otherC := loginAPI(t, app, u.Name, "password123456"), loginAPI(t, app, other.Name, "password123456")
			var stored models.PushSubscription
			gormMust(t, app.DB.First(&stored, dev.ID))
			failedAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
			if tc.code == http.StatusCreated {
				gormMust(t, app.DB.Model(&models.PushSubscription{}).Where("id = ?", dev.ID).UpdateColumn("failed_at", failedAt))
			}
			now := time.Now().UTC()
			m := models.Message{Status: models.MessageSending, Category: models.MessageCategoryAnnouncement, Subject: "News",
				BodyHTML: "<p>Hello</p>", BodyText: "Hello", Audience: `{"type":"group","groupId":1}`, ScheduledAt: &now}
			gormMust(t, app.DB.Create(&m))
			row := models.MessageRecipient{MessageID: m.ID, UserID: u.ID, Channel: models.DeliveryPush, Status: models.RecipientQueued}
			gormMust(t, app.DB.Create(&row))
			newKey, err := ecdh.P256().GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			newAuth := make([]byte, 16)
			_, _ = rand.Read(newAuth)
			renewedKey, renewedAuth := base64.RawURLEncoding.EncodeToString(newKey.PublicKey().Bytes()), base64.RawURLEncoding.EncodeToString(newAuth)
			var changed atomic.Bool
			fp.onSend = func(string) {
				if !changed.CompareAndSwap(false, true) {
					return
				}
				if tc.change == "remove" {
					if err := app.DB.Delete(&models.PushSubscription{}, dev.ID).Error; err != nil {
						t.Error(err)
					}
					return
				}
				if tc.change == "rebind back" || tc.change == "same clock" {
					sub := browserSub{Endpoint: dev.Endpoint, Keys: map[string]string{"p256dh": dev.P256dh, "auth": dev.Auth}}
					for _, c := range []*apiClient{otherC, ownerC} {
						if w := c.do("PUT", "/api/v1/push/subscriptions", sub, true); w.Code != http.StatusOK {
							t.Errorf("rebinding the browser: %d %s", w.Code, w.Body.String())
						}
					}
					if tc.change == "same clock" {
						if err := app.DB.Model(&models.PushSubscription{}).Where("id = ?", dev.ID).UpdateColumn("created_at", stored.CreatedAt).Error; err != nil {
							t.Error(err)
						}
					}
					return
				}
				if tc.change == "saved again" {
					// The owner's own browser saves the same subscription: the
					// same binding, so the answer about it still counts.
					sub := browserSub{Endpoint: dev.Endpoint, Keys: map[string]string{"p256dh": dev.P256dh, "auth": dev.Auth}}
					if w := ownerC.do("PUT", "/api/v1/push/subscriptions", sub, true); w.Code != http.StatusOK {
						t.Errorf("saving the subscription again: %d %s", w.Code, w.Body.String())
					}
					return
				}
				if tc.change == "renew back" {
					for _, keys := range []map[string]string{{"p256dh": renewedKey, "auth": renewedAuth}, {"p256dh": dev.P256dh, "auth": dev.Auth}} {
						if w := ownerC.do("PUT", "/api/v1/push/subscriptions", browserSub{Endpoint: dev.Endpoint, Keys: keys}, true); w.Code != http.StatusOK {
							t.Errorf("renewing the subscription: %d %s", w.Code, w.Body.String())
						}
					}
					return
				}
				set := map[string]any{"user_id": other.ID}
				if tc.change == "renew" {
					set = map[string]any{"p256dh": renewedKey, "auth": renewedAuth}
				}
				if err := app.DB.Model(&models.PushSubscription{}).Where("id = ?", dev.ID).UpdateColumns(set).Error; err != nil {
					t.Error(err)
				}
			}
			fp.set(dev.path, tc.code)
			err = app.deliverMessagePush(context.Background(), row.ID, dev.ID, 0)
			if tc.change == "saved again" {
				gormMust(t, app.DB.First(&row, row.ID))
				if err != nil || deviceExists(app, dev.ID) || row.Status != models.RecipientSuppressed || row.Reason != pushWhyGone {
					t.Fatalf("Gone about a device only saved again: err %v, device kept %v, row %+v; want it deleted and device gone", err, deviceExists(app, dev.ID), row)
				}
				return
			}
			if tc.change == "remove" {
				gormMust(t, app.DB.First(&row, row.ID))
				if err != nil || row.Status != models.RecipientSuppressed || row.Reason != pushWhyGone {
					t.Fatalf("Gone about a device deleted meanwhile: err %v, row %+v; want device gone at once", err, row)
				}
				return
			}
			var after models.PushSubscription
			if res := app.DB.Where("id = ?", dev.ID).Limit(1).Find(&after); res.Error != nil || res.RowsAffected != 1 {
				t.Fatalf("the changed device is gone after the old attempt's %d (%v)", tc.code, res.Error)
			}
			gormMust(t, app.DB.First(&row, row.ID))
			switch tc.code {
			case http.StatusGone:
				if err == nil || row.Status != models.RecipientQueued {
					t.Fatalf("Gone about a changed device: err %v, row %+v; want a retry with the row queued", err, row)
				}
				fp.set(dev.path, http.StatusCreated)
				if err := app.deliverMessagePush(context.Background(), row.ID, dev.ID, 0); err != nil {
					t.Fatal(err)
				}
				gormMust(t, app.DB.First(&row, row.ID))
				if tc.change != "rebind" && (row.Status != models.RecipientSent || fp.count(dev.path) != 2) {
					t.Fatalf("the %s subscription's retry: row %+v, sends %d", tc.change, row, fp.count(dev.path))
				}
				if tc.change == "rebind" && (row.Status != models.RecipientSuppressed || row.Reason != pushWhyGone || fp.count(dev.path) != 1) {
					t.Fatalf("the rebound device's retry: row %+v, sends %d", row, fp.count(dev.path))
				}
			case http.StatusBadRequest:
				if after.FailedAt != nil || row.Status != models.RecipientFailed {
					t.Fatalf("a refusal of the old attempt: device failed_at %v, row %+v", after.FailedAt, row)
				}
			case http.StatusCreated:
				if err != nil || after.FailedAt == nil || !after.FailedAt.Equal(failedAt) || row.Status != models.RecipientSent {
					t.Fatalf("a delivery of the old attempt: err %v, device failed_at %v, row %+v", err, after.FailedAt, row)
				}
			}
			if tc.change == "rebind" && after.UserID != other.ID {
				t.Fatalf("the device's owner is %d, want the next holder", after.UserID)
			}
			switch tc.change {
			case "rebind back", "renew back", "same clock":
				// Every field the dispatch read is back, but the binding is new.
				if after.UserID != u.ID || after.P256dh != dev.P256dh || after.Auth != dev.Auth || after.BindingGeneration <= stored.BindingGeneration {
					t.Fatalf("the device after its round trip: owner %d, generation %d (dispatched %d)", after.UserID, after.BindingGeneration, stored.BindingGeneration)
				}
				if tc.change == "same clock" && !after.CreatedAt.Equal(stored.CreatedAt) {
					t.Fatalf("created_at %v, want the dispatched %v", after.CreatedAt, stored.CreatedAt)
				}
			}
		})
	}
}

// pushLastAttemptFixture is an announcement push row for owner, with a job
// for dev queued on its last attempt.
func pushLastAttemptFixture(t *testing.T, app *App, u *models.User, devs ...testDevice) models.MessageRecipient {
	t.Helper()
	now := time.Now().UTC()
	m := models.Message{Status: models.MessageSending, Category: models.MessageCategoryAnnouncement, Subject: "News",
		BodyHTML: "<p>Hello</p>", BodyText: "Hello", Audience: `{"type":"group","groupId":1}`, ScheduledAt: &now}
	gormMust(t, app.DB.Create(&m))
	row := models.MessageRecipient{MessageID: m.ID, UserID: u.ID, Channel: models.DeliveryPush, Status: models.RecipientQueued}
	gormMust(t, app.DB.Create(&row))
	for _, dev := range devs {
		key := pushRecipientKey(row.ID, dev.ID)
		if ok, err := pushDeliverType.Enqueue(app.DB, pushJob{MessageRecipientID: row.ID, DeviceID: dev.ID}, now.Add(-time.Second), key); err != nil || !ok {
			t.Fatalf("queued %v: %v", ok, err)
		}
		gormMust(t, app.DB.Model(&models.Job{}).Where("unique_key = ?", key).Update("attempts", pushDeliverType.Attempts()-1))
	}
	return row
}

func renewedKeys(t *testing.T) (string, string) {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	_, _ = rand.Read(auth)
	return base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()), base64.RawURLEncoding.EncodeToString(auth)
}

func pushJobCount(t *testing.T, app *App) int64 {
	t.Helper()
	var n int64
	gormMust(t, app.DB.Model(&models.Job{}).Where("kind = ?", kindPushDeliver).Count(&n))
	return n
}

// TestPushGoneAboutAChangedDeviceOnItsLastAttempt: a Gone answer about a
// device that changed while the push service answered retries while attempts
// remain; on the job's last attempt (run through the jobs runner) it must
// still leave the row honest. The person's renewed subscription was never
// reached as it is now: the row fails. A browser rebound to someone else is
// the moved-device rule: device gone. The row waits only for another device's
// job that will still run (queued or waiting to retry), and that job can
// still deliver it, with no job added; a device saved after the jobs were
// queued has no job, and a job that already ended takes nothing, so neither
// keeps the row queued.
func TestPushGoneAboutAChangedDeviceOnItsLastAttempt(t *testing.T) {
	for _, tc := range []struct {
		change, sibling string
	}{
		{"renew", ""}, {"renew", "no job"}, {"renew", "job ended"}, {"renew", "job queued"}, {"renew", "job retrying"},
		{"rebind", ""}, {"rebind", "no job"}, {"rebind", "job queued"},
	} {
		t.Run(strings.TrimSpace(tc.change+" "+tc.sibling), func(t *testing.T) {
			app, fp := pushTestApp(t)
			u := pushMember(t, app, "owner")
			other := pushMember(t, app, "nextholder")
			dev := newPushDevice(t, app, u, "shared")
			row := pushLastAttemptFixture(t, app, u, dev)
			var laptop testDevice
			if tc.sibling != "" {
				// Saved after the push's jobs were queued: no job of its own.
				laptop = newPushDevice(t, app, u, "laptop")
			}
			laptopKey := pushRecipientKey(row.ID, laptop.ID)
			switch tc.sibling {
			case "job ended":
				gormMust(t, app.DB.Create(&models.Job{Kind: kindPushDeliver, Payload: fmt.Sprintf(`{"messageRecipientId":%d,"deviceId":%d}`, row.ID, laptop.ID),
					RunAt: time.Now(), Status: models.JobDead, MaxAttempts: pushDeliverType.Attempts(), UniqueKey: &laptopKey}))
			case "job queued", "job retrying":
				status := models.JobQueued
				if tc.sibling == "job retrying" {
					status = models.JobFailed
				}
				// Due later than the shared device's, so the runner takes that first.
				gormMust(t, app.DB.Create(&models.Job{Kind: kindPushDeliver, Payload: fmt.Sprintf(`{"messageRecipientId":%d,"deviceId":%d}`, row.ID, laptop.ID),
					RunAt: time.Now(), Status: status, MaxAttempts: pushDeliverType.Attempts(), UniqueKey: &laptopKey}))
			}
			p256dh, auth := renewedKeys(t)
			var changed atomic.Bool
			fp.onSend = func(path string) {
				if path != dev.path || !changed.CompareAndSwap(false, true) {
					return
				}
				set := map[string]any{"user_id": other.ID}
				if tc.change == "renew" {
					set = map[string]any{"p256dh": p256dh, "auth": auth}
				}
				if err := app.DB.Model(&models.PushSubscription{}).Where("id = ?", dev.ID).UpdateColumns(set).Error; err != nil {
					t.Error(err)
				}
			}
			fp.set(dev.path, http.StatusGone)
			jobsBefore := pushJobCount(t, app)
			if ran, err := jobs.NewRunner(app.DB, app.pushKinds()...).RunOnce(context.Background(), "test"); err != nil || !ran {
				t.Fatalf("ran %v: %v", ran, err)
			}
			if !changed.Load() || fp.count(dev.path) != 1 || fp.count(laptop.path) != 0 {
				t.Fatalf("the runner took another job first: %v", fp.sent)
			}
			gormMust(t, app.DB.First(&row, row.ID))
			var j models.Job
			gormMust(t, app.DB.Where("unique_key = ?", pushRecipientKey(row.ID, dev.ID)).First(&j))
			if j.Status == models.JobQueued || j.Status == models.JobFailed || j.Status == models.JobRunning {
				t.Fatalf("the last attempt left its job %s", j.Status)
			}
			if !deviceExists(app, dev.ID) {
				t.Fatal("the stale Gone deleted the changed device")
			}
			switch {
			case tc.sibling == "job queued" || tc.sibling == "job retrying":
				if row.Status != models.RecipientQueued {
					t.Fatalf("with the laptop's job still to run: row %+v; want queued", row)
				}
				// That job still delivers it, and nothing queued another.
				if err := app.deliverMessagePush(context.Background(), row.ID, laptop.ID, 0); err != nil {
					t.Fatal(err)
				}
				gormMust(t, app.DB.First(&row, row.ID))
				if row.Status != models.RecipientSent || fp.count(laptop.path) != 1 || pushJobCount(t, app) != jobsBefore {
					t.Fatalf("the laptop's job: row %+v, sends %v, jobs %d then %d", row, fp.sent, jobsBefore, pushJobCount(t, app))
				}
			case tc.change == "renew":
				if row.Status != models.RecipientFailed || row.Reason != pushWhyNotReached {
					t.Fatalf("the renewed device's last attempt: row %+v; want failed, not reached", row)
				}
			default:
				if row.Status != models.RecipientSuppressed || row.Reason != pushWhyGone {
					t.Fatalf("the rebound device's last attempt: row %+v; want device gone", row)
				}
			}
			if tc.sibling != "job queued" && tc.sibling != "job retrying" && pushJobCount(t, app) != jobsBefore {
				t.Fatalf("settling the row queued a job: %d then %d", jobsBefore, pushJobCount(t, app))
			}
		})
	}
}

// TestPushTwoLastAttemptsNeverWaitForEachOther: two of the person's devices
// are both renewed while the push service answers each one's last attempt,
// and the two jobs run together. Each sees the other's job running, which
// decides for itself, so neither leaves the row queued for the other.
func TestPushTwoLastAttemptsNeverWaitForEachOther(t *testing.T) {
	app, fp := pushTestApp(t)
	u := pushMember(t, app, "owner")
	phone, laptop := newPushDevice(t, app, u, "phone"), newPushDevice(t, app, u, "laptop")
	row := pushLastAttemptFixture(t, app, u, phone, laptop)
	devs := map[string]testDevice{phone.path: phone, laptop.path: laptop}
	renewed := map[string][2]string{}
	for path := range devs {
		p256dh, auth := renewedKeys(t)
		renewed[path] = [2]string{p256dh, auth}
	}
	var arrived sync.WaitGroup
	arrived.Add(2)
	both := make(chan struct{})
	go func() { arrived.Wait(); close(both) }()
	fp.onSend = func(path string) {
		keys := renewed[path]
		if err := app.DB.Model(&models.PushSubscription{}).Where("id = ?", devs[path].ID).UpdateColumns(map[string]any{"p256dh": keys[0], "auth": keys[1]}).Error; err != nil {
			t.Error(err)
		}
		arrived.Done()
		select {
		case <-both:
		case <-time.After(10 * time.Second):
			t.Error("the two last attempts never ran together")
		}
	}
	fp.set(phone.path, http.StatusGone)
	fp.set(laptop.path, http.StatusGone)
	runner := jobs.NewRunner(app.DB, app.pushKinds()...)
	var wg sync.WaitGroup
	for _, w := range []string{"one", "two"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ran, err := runner.RunOnce(context.Background(), w); err != nil || !ran {
				t.Errorf("worker %s ran %v: %v", w, ran, err)
			}
		}()
	}
	wg.Wait()
	gormMust(t, app.DB.First(&row, row.ID))
	if row.Status != models.RecipientFailed || row.Reason != pushWhyNotReached {
		t.Fatalf("after both last attempts: row %+v; want failed, not reached", row)
	}
}

// TestPushMovedDeviceWaitsOnlyForARealJob: a push whose device moved to
// another account before it ran leaves the row to the person's other device
// only while that device's job is still to run; a device with no job (saved
// after the push was queued) is no reason to wait.
func TestPushMovedDeviceWaitsOnlyForARealJob(t *testing.T) {
	for _, sibling := range []string{"no job", "job queued"} {
		t.Run(sibling, func(t *testing.T) {
			app, fp := pushTestApp(t)
			u := pushMember(t, app, "owner")
			other := pushMember(t, app, "nextholder")
			dev := newPushDevice(t, app, u, "shared")
			row := pushLastAttemptFixture(t, app, u, dev)
			laptop := newPushDevice(t, app, u, "laptop")
			if sibling == "job queued" {
				if _, err := pushDeliverType.Enqueue(app.DB, pushJob{MessageRecipientID: row.ID, DeviceID: laptop.ID}, time.Now().Add(time.Hour), pushRecipientKey(row.ID, laptop.ID)); err != nil {
					t.Fatal(err)
				}
			}
			gormMust(t, app.DB.Model(&models.PushSubscription{}).Where("id = ?", dev.ID).Update("user_id", other.ID))
			if err := app.deliverMessagePush(context.Background(), row.ID, dev.ID, 0); err != nil {
				t.Fatal(err)
			}
			gormMust(t, app.DB.First(&row, row.ID))
			want := models.RecipientSuppressed
			if sibling == "job queued" {
				want = models.RecipientQueued
			}
			if fp.total() != 0 || row.Status != want {
				t.Fatalf("pushes %v, row %+v; want %s", fp.sent, row, want)
			}
		})
	}
}

// TestPushNoticeSiblingThatMovesStillSettles: a notice's push to a gone device
// leaves its delivery to the person's other device while that device's job is
// still queued. That device is then turned off, or rebound to another account,
// before its job runs. Its job settles the delivery as "device gone": both jobs
// finish, nothing is sent to the device or its new holder, and no job is
// added.
func TestPushNoticeSiblingThatMovesStillSettles(t *testing.T) {
	for _, change := range []string{"removed", "rebound"} {
		t.Run(change, func(t *testing.T) {
			app, f := pushTestApp(t)
			u := pushMember(t, app, "pat")
			holder := pushMember(t, app, "next holder")
			old, phone := newPushDevice(t, app, u, "old"), newPushDevice(t, app, u, "phone")
			id := sendNotice(t, app, notify.GroupApproved, u)
			gormMust(t, app.DB.Where("kind = ?", kindPushDeliver).Delete(&models.Job{}))
			// The old device's job is due first, so the runner takes it first.
			if err := enqueuePushDelivery(app.DB, id, old.ID, time.Now().Add(-time.Minute)); err != nil {
				t.Fatal(err)
			}
			if err := enqueuePushDelivery(app.DB, id, phone.ID, time.Now().Add(-time.Second)); err != nil {
				t.Fatal(err)
			}
			jobsBefore := pushJobCount(t, app)
			f.set(old.path, http.StatusGone)
			runner := jobs.NewRunner(app.DB, app.pushKinds()...)
			if ran, err := runner.RunOnce(context.Background(), "test"); err != nil || !ran {
				t.Fatalf("ran %v: %v", ran, err)
			}
			if status, reason := pushDelivery(t, app, id); status != "" || f.count(old.path) != 1 || f.count(phone.path) != 0 {
				t.Fatalf("the gone device's job: delivery %q %q, sends %v; want it left to the phone's job", status, reason, f.sent)
			}
			if change == "removed" {
				gormMust(t, app.DB.Delete(&models.PushSubscription{}, phone.ID))
			} else {
				gormMust(t, app.DB.Model(&models.PushSubscription{}).Where("id = ?", phone.ID).Update("user_id", holder.ID))
			}
			drainJobs(t, app)
			if status, reason := pushDelivery(t, app, id); status != models.DeliverySuppressed || reason != pushWhyGone {
				t.Fatalf("after the phone %s: delivery %q %q; want suppressed %q", change, status, reason, pushWhyGone)
			}
			if f.total() != 1 || f.count(phone.path) != 0 {
				t.Fatalf("sends %v; want only the gone device's one", f.sent)
			}
			var rows, holderRows, notDone int64
			gormMust(t, app.DB.Model(&models.NotificationDelivery{}).Where("notification_id = ? AND channel = ?", id, models.DeliveryPush).Count(&rows))
			gormMust(t, app.DB.Model(&models.NotificationDelivery{}).Where("user_id = ?", holder.ID).Count(&holderRows))
			gormMust(t, app.DB.Model(&models.Job{}).Where("kind = ? AND status <> ?", kindPushDeliver, models.JobDone).Count(&notDone))
			if rows != 1 || holderRows != 0 || notDone != 0 || pushJobCount(t, app) != jobsBefore {
				t.Fatalf("delivery rows %d, the holder's %d, jobs not done %d, jobs %d then %d", rows, holderRows, notDone, jobsBefore, pushJobCount(t, app))
			}
		})
	}
}

// claimPushJobByHand puts the job with key in the state the runner's claim
// leaves it: running, held by worker, one more attempt, a new claim number.
// It returns the context the runner would run the handler under for that
// claim.
func claimPushJobByHand(t *testing.T, app *App, key, worker string) context.Context {
	t.Helper()
	var c jobs.Claim
	res := app.DB.Raw(`UPDATE jobs SET status = ?, locked_at = now(), locked_by = ?, attempts = attempts + 1,
			claim_seq = nextval('jobs_claim_seq'), updated_at = now()
		WHERE unique_key = ? AND status IN (?, ?) AND run_at <= now() RETURNING id, claim_seq AS seq`,
		models.JobRunning, worker, key, models.JobQueued, models.JobFailed).Scan(&c)
	if res.Error != nil || c.ID == 0 {
		t.Fatalf("claiming %s: %+v, %v", key, c, res.Error)
	}
	return jobs.WithClaim(context.Background(), c)
}

// pushJobWaitsUntil checks that the job with key went back to wait in its
// handler's wait commit (jobs.LaterIn): queued for opens, unheld, with the
// attempt its claim took given back, so the runner writes nothing when the
// handler returns jobs.Requeued.
func pushJobWaitsUntil(t *testing.T, app *App, key string, opens time.Time, attempts int) {
	t.Helper()
	var j models.Job
	gormMust(t, app.DB.Where("unique_key = ?", key).First(&j))
	if j.Status != models.JobQueued || !j.RunAt.Equal(opens.UTC()) || j.LockedAt != nil || j.Attempts != attempts {
		t.Fatalf("after its wait committed, job %s is %s due %v, locked %v, %d attempts; want queued for %v, unheld, %d attempts",
			key, j.Status, j.RunAt, j.LockedAt, j.Attempts, opens.UTC(), attempts)
	}
}

// TestPushGoneRacingASiblingThatWaits: the person's phone takes an
// announcement's push just before quiet hours and answers "gone" after they
// start, while the job for their laptop is running and goes back to wait for
// the morning. Whichever of the two settles the row first, the history says
// the push waits until the morning, never "device gone", through the night;
// in the morning the laptop's job delivers it, with no job added. When the
// laptop waited first, the phone's job sees it will run again and leaves the
// row; when the phone settled first, the laptop's wait puts the row back to
// waiting. When the laptop's wait commits while the phone's settlement waits on
// the row's lock, the settlement sees it. In the last arm both jobs waited in
// the night and wake in the morning: the laptop's job is running, about to
// send, when the phone answers "gone". A job that woke is not waiting, so the
// row is "device gone" at once, as for two last attempts.
func TestPushGoneRacingASiblingThatWaits(t *testing.T) {
	for _, order := range []string{"laptop waits first", "phone settles first", "laptop waits during the settlement", "both wake gone"} {
		t.Run(order, func(t *testing.T) {
			app, fp := pushTestApp(t)
			clock := quietHours(app)
			clock.set(19, 59, 15)
			u := denverMember(t, app, "owner")
			phone, laptop := newPushDevice(t, app, u, "phone"), newPushDevice(t, app, u, "laptop")
			row := pushLastAttemptFixture(t, app, u, phone, laptop)
			phoneKey, laptopKey := pushRecipientKey(row.ID, phone.ID), pushRecipientKey(row.ID, laptop.ID)
			jobsBefore := pushJobCount(t, app)
			fp.set(phone.path, http.StatusGone)
			if order == "both wake gone" {
				fp.set(laptop.path, http.StatusGone)
			}
			reload := func() {
				t.Helper()
				gormMust(t, app.DB.First(&row, row.ID))
			}
			waitingOvernight := func(when string) {
				t.Helper()
				reload()
				if row.Status != models.RecipientQueued || !strings.HasPrefix(row.Reason, pushWaitPrefix) {
					t.Fatalf("%s: row %s %q; want it waiting for the morning", when, row.Status, row.Reason)
				}
			}
			var opens time.Time
			var laptopCtx context.Context
			laptopWaits := func() {
				t.Helper()
				// The runner has claimed the laptop's job; its handler finds
				// quiet hours have begun and goes back to wait.
				err := app.deliverMessagePush(laptopCtx, row.ID, laptop.ID, 0)
				if !errors.Is(err, jobs.Requeued()) {
					t.Fatalf("the laptop's job in quiet hours: %v; want it to wait (jobs.Requeued)", err)
				}
				opens, _ = app.pushOpensAt(app.pushNow(), denverZone)
			}
			switch order {
			case "laptop waits during the settlement":
				laptopCtx = claimPushJobByHand(t, app, laptopKey, "laptop worker")
				// The laptop's wait holds the row, as pushWaitIn's UPDATE does,
				// while the phone's job sends, hears "gone" and settles.
				opens = time.Date(2030, 1, 16, 8, 0, 0, 0, denverZone)
				hold := app.DB.Begin(pushSettleTx)
				gormMust(t, hold)
				gormMust(t, hold.Exec("SELECT id FROM message_recipients WHERE id = ? FOR UPDATE", row.ID))
				done := make(chan error, 1)
				go func() {
					_, err := jobs.NewRunner(app.DB, app.pushKinds()...).RunOnce(context.Background(), "phone worker")
					done <- err
				}()
				testdb.WaitUntil(t, "the phone's settlement to wait on the push row", 10*time.Second, func() bool {
					var n int64
					if err := app.DB.Raw(`SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'
						AND state = 'active' AND query LIKE '%message_recipients%'`).Scan(&n).Error; err != nil {
						t.Fatal(err)
					}
					return n > 0
				})
				if err := pushWaitIn(laptopCtx, hold, row.ID, opens, denverZone); err != nil {
					hold.Rollback()
					t.Fatal(err)
				}
				gormMust(t, hold.Commit())
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				if fp.count(phone.path) != 1 || deviceExists(app, phone.ID) {
					t.Fatalf("the phone's job: sends %v", fp.sent)
				}
				waitingOvernight("after the phone settled behind the laptop's wait")
				pushJobWaitsUntil(t, app, laptopKey, opens, pushDeliverType.Attempts()-1)
				waitingOvernight("overnight")
			case "laptop waits first", "phone settles first":
				laptopCtx = claimPushJobByHand(t, app, laptopKey, "laptop worker")
				var raced atomic.Bool
				fp.onSend = func(path string) {
					if path != phone.path || !raced.CompareAndSwap(false, true) {
						return
					}
					clock.set(20, 1, 15)
					if order == "laptop waits first" {
						laptopWaits()
					}
				}
				if ran, err := jobs.NewRunner(app.DB, app.pushKinds()...).RunOnce(context.Background(), "phone worker"); err != nil || !ran {
					t.Fatalf("ran %v: %v", ran, err)
				}
				if !raced.Load() || fp.count(phone.path) != 1 || fp.count(laptop.path) != 0 {
					t.Fatalf("the phone's job: sends %v", fp.sent)
				}
				if deviceExists(app, phone.ID) {
					t.Fatal("the gone phone is still there")
				}
				if order == "phone settles first" {
					reload()
					if row.Status != models.RecipientSuppressed || row.Reason != pushWhyGone {
						t.Fatalf("the phone settled with the laptop's job running and not yet waiting: row %s %q; want device gone", row.Status, row.Reason)
					}
					laptopWaits()
				}
				waitingOvernight("after the laptop's job went back to wait")
				pushJobWaitsUntil(t, app, laptopKey, opens, pushDeliverType.Attempts()-1)
				waitingOvernight("overnight")
			case "both wake gone":
				// Both jobs waited in the night: the row waits, and both are
				// queued for the morning.
				clock.set(20, 1, 15)
				drainJobs(t, app)
				waitingOvernight("overnight")
				if fp.total() != 0 {
					t.Fatalf("sent in the night: %v", fp.sent)
				}
			}

			clock.set(8, 0, 16)
			makeDue(app)
			if order != "both wake gone" {
				drainJobs(t, app)
				reload()
				if row.Status != models.RecipientSent || fp.count(laptop.path) != 1 || fp.count(phone.path) != 1 || pushJobCount(t, app) != jobsBefore {
					t.Fatalf("in the morning: row %s %q, sends %v, jobs %d then %d; want sent by the laptop with no job added", row.Status, row.Reason, fp.sent, jobsBefore, pushJobCount(t, app))
				}
				return
			}
			// The laptop's job woke too and is running, about to send.
			claimPushJobByHand(t, app, laptopKey, "laptop worker")
			if ran, err := jobs.NewRunner(app.DB, app.pushKinds()...).RunOnce(context.Background(), "phone worker"); err != nil || !ran {
				t.Fatalf("ran %v: %v", ran, err)
			}
			reload()
			if row.Status != models.RecipientSuppressed || row.Reason != pushWhyGone || fp.count(phone.path) != 1 {
				t.Fatalf("the phone answered gone while the woken laptop job ran: row %s %q, sends %v; want device gone", row.Status, row.Reason, fp.sent)
			}
			var j models.Job
			gormMust(t, app.DB.Where("unique_key = ?", phoneKey).First(&j))
			if j.Status != models.JobDone {
				t.Fatalf("the phone's job is %s", j.Status)
			}
			if err := app.deliverMessagePush(context.Background(), row.ID, laptop.ID, 0); err != nil {
				t.Fatal(err)
			}
			reload()
			if row.Status != models.RecipientSuppressed || row.Reason != pushWhyGone || fp.count(laptop.path) != 1 {
				t.Fatalf("the laptop answered gone too: row %s %q, sends %v; want device gone", row.Status, row.Reason, fp.sent)
			}
		})
	}
}

// pushStepGates stops an announcement push at step: the i-th time any push
// reaches it, gates[i].reached closes and that push waits until
// gates[i].release closes (the cleanup closes any left). Later arrivals pass.
type pushStepGate struct{ reached, release chan struct{} }

func pushStepGates(t *testing.T, app *App, step string, n int) []pushStepGate {
	t.Helper()
	gates := make([]pushStepGate, n)
	for i := range gates {
		gates[i] = pushStepGate{reached: make(chan struct{}), release: make(chan struct{})}
	}
	var arrived atomic.Int64
	app.hooks.pushStep = func(s string) {
		if s != step {
			return
		}
		i := int(arrived.Add(1)) - 1
		if i >= n {
			return
		}
		close(gates[i].reached)
		<-gates[i].release
	}
	var once sync.Once
	t.Cleanup(func() {
		once.Do(func() {
			for i := range gates {
				select {
				case <-gates[i].release:
				default:
					close(gates[i].release)
				}
			}
		})
	})
	return gates
}

func waitClosed(t *testing.T, what string, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func pushJobByKey(t *testing.T, app *App, key string) models.Job {
	t.Helper()
	var j models.Job
	gormMust(t, app.DB.Where("unique_key = ?", key).First(&j))
	return j
}

// TestPushWaitNeverTouchesAReplacementsClaim: a worker's announcement push
// passes its checks in quiet hours and stalls before its wait commits; its
// lease runs out and a second worker takes the job back and is running it
// when the first one reaches its wait. The stale attempt changes neither the
// job nor the row (jobs.LaterIn finds its claim gone and the whole wait
// rolls back), and its finish records nothing; the replacement's own wait
// stands, with its attempt given back, and in the morning it sends once.
func TestPushWaitNeverTouchesAReplacementsClaim(t *testing.T) {
	app, fp := pushTestApp(t)
	clock := quietHours(app)
	u := denverMember(t, app, "owner")
	laptop := newPushDevice(t, app, u, "laptop")
	row := pushLastAttemptFixture(t, app, u, laptop)
	key := pushRecipientKey(row.ID, laptop.ID)
	gormMust(t, app.DB.Model(&models.Job{}).Where("unique_key = ?", key).Update("attempts", 0))
	gates := pushStepGates(t, app, "wait", 2)

	first := jobs.NewRunner(app.DB, app.pushKinds()...)
	firstDone := make(chan error, 1)
	go func() {
		_, err := first.RunOnce(context.Background(), "worker one")
		firstDone <- err
	}()
	waitClosed(t, "the first worker to reach its wait", gates[0].reached)

	// The first worker's lease has run out: a second worker takes the job.
	second := jobs.NewRunner(app.DB, app.pushKinds()...)
	second.Lease = time.Nanosecond
	secondDone := make(chan error, 1)
	go func() {
		ran, err := second.RunOnce(context.Background(), "worker two")
		if err == nil && !ran {
			err = errors.New("the second worker found nothing to take back")
		}
		secondDone <- err
	}()
	waitClosed(t, "the replacement to reach its wait", gates[1].reached)
	replaced := pushJobByKey(t, app, key)
	if replaced.Status != models.JobRunning || replaced.LockedBy != "worker two" || replaced.Attempts != 2 {
		t.Fatalf("the replacement's claim: %+v", replaced)
	}

	close(gates[0].release)
	if err := <-firstDone; err != nil {
		t.Fatalf("the stale worker: %v", err)
	}
	after := pushJobByKey(t, app, key)
	if after.Status != models.JobRunning || after.LockedBy != "worker two" || after.Attempts != 2 ||
		!after.RunAt.Equal(replaced.RunAt) || after.LockedAt == nil || !after.LockedAt.Equal(*replaced.LockedAt) || after.LastError != "" {
		t.Fatalf("the stale worker's wait changed the replacement's job: before %+v, after %+v", replaced, after)
	}
	gormMust(t, app.DB.First(&row, row.ID))
	if row.Status != models.RecipientQueued || row.Reason != "" {
		t.Fatalf("the stale worker's wait wrote the row: %s %q", row.Status, row.Reason)
	}

	close(gates[1].release)
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	opens, _ := app.pushOpensAt(app.pushNow(), denverZone)
	pushJobWaitsUntil(t, app, key, opens, 1)
	gormMust(t, app.DB.First(&row, row.ID))
	if row.Status != models.RecipientQueued || !strings.HasPrefix(row.Reason, pushWaitPrefix) || fp.total() != 0 {
		t.Fatalf("overnight: row %s %q, sends %v", row.Status, row.Reason, fp.sent)
	}

	clock.set(8, 0, 16)
	makeDue(app)
	drainJobs(t, app)
	gormMust(t, app.DB.First(&row, row.ID))
	if j := pushJobByKey(t, app, key); row.Status != models.RecipientSent || fp.count(laptop.path) != 1 || j.Status != models.JobDone || j.Attempts != 2 {
		t.Fatalf("in the morning: row %s %q, sends %v, job %s after %d attempts", row.Status, row.Reason, fp.sent, j.Status, j.Attempts)
	}
}

// TestPushWaitsFinishNeverTouchesTheNextClaim: an announcement push goes back
// to wait for quiet hours and its worker is slow to return after the wait
// commits. Morning comes and a worker with the same label (a restarted
// process can reuse one) claims the job again, on the same attempt count the
// wait gave back, and is about to send when the first handler returns. The
// first worker's finish changes nothing (its wait is jobs.Requeued, and its
// claim is not the current one): the morning claim keeps the job, no third
// claim takes it, and the push is sent once.
func TestPushWaitsFinishNeverTouchesTheNextClaim(t *testing.T) {
	app, fp := pushTestApp(t)
	clock := quietHours(app)
	u := denverMember(t, app, "owner")
	laptop := newPushDevice(t, app, u, "laptop")
	row := pushLastAttemptFixture(t, app, u, laptop)
	key := pushRecipientKey(row.ID, laptop.ID)
	gormMust(t, app.DB.Model(&models.Job{}).Where("unique_key = ?", key).Update("attempts", 0))
	waited, morning := pushStepGate{make(chan struct{}), make(chan struct{})}, pushStepGate{make(chan struct{}), make(chan struct{})}
	var waits, snapshots atomic.Int64
	app.hooks.pushStep = func(s string) {
		switch {
		case s == "waited" && waits.Add(1) == 1:
			close(waited.reached)
			<-waited.release
		case s == "snapshot" && snapshots.Add(1) == 1:
			close(morning.reached)
			<-morning.release
		}
	}
	t.Cleanup(func() {
		for _, g := range []pushStepGate{waited, morning} {
			select {
			case <-g.release:
			default:
				close(g.release)
			}
		}
	})
	run := func() <-chan error {
		done := make(chan error, 1)
		go func() {
			ran, err := jobs.NewRunner(app.DB, app.pushKinds()...).RunOnce(context.Background(), "worker")
			if err == nil && !ran {
				err = errors.New("found no job to claim")
			}
			done <- err
		}()
		return done
	}

	firstDone := run()
	waitClosed(t, "the push's wait to commit", waited.reached)
	opens, _ := app.pushOpensAt(app.pushNow(), denverZone)
	pushJobWaitsUntil(t, app, key, opens, 0)

	clock.set(8, 0, 16)
	makeDue(app)
	secondDone := run()
	waitClosed(t, "the same worker label to claim the job in the morning", morning.reached)
	claim := pushJobByKey(t, app, key)
	if claim.Status != models.JobRunning || claim.LockedBy != "worker" || claim.Attempts != 1 || claim.ClaimSeq == nil {
		t.Fatalf("the morning claim: %+v", claim)
	}

	close(waited.release)
	if err := <-firstDone; err != nil {
		t.Fatalf("the slow worker: %v", err)
	}
	after := pushJobByKey(t, app, key)
	if after.Status != models.JobRunning || after.ClaimSeq == nil || *after.ClaimSeq != *claim.ClaimSeq || after.Attempts != 1 ||
		after.LockedAt == nil || !after.LockedAt.Equal(*claim.LockedAt) || !after.RunAt.Equal(claim.RunAt) || after.LastError != "" {
		t.Fatalf("the slow worker's finish changed the morning claim: before %+v, after %+v", claim, after)
	}
	if ran, err := jobs.NewRunner(app.DB, app.pushKinds()...).RunOnce(context.Background(), "worker"); ran || err != nil {
		t.Fatalf("a third claim while the morning one runs: ran %v, %v", ran, err)
	}

	close(morning.release)
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	gormMust(t, app.DB.First(&row, row.ID))
	if j := pushJobByKey(t, app, key); row.Status != models.RecipientSent || fp.count(laptop.path) != 1 || fp.total() != 1 || j.Status != models.JobDone || j.Attempts != 1 {
		t.Fatalf("in the morning: row %s %q, sends %v, job %s after %d attempts", row.Status, row.Reason, fp.sent, j.Status, j.Attempts)
	}
}

// TestPushWaitOutlivesAWorkerThatStopsAfterIt: on its last attempt, an
// announcement push goes back to wait for quiet hours and its worker stops
// right after the wait commits, before the runner hears jobs.Requeued. The job
// is already queued for the morning with its attempt given back, so lease
// recovery finds nothing to take back overnight and never marks it dead; in
// the morning it sends once, and the stopped worker's late finish changes
// nothing.
func TestPushWaitOutlivesAWorkerThatStopsAfterIt(t *testing.T) {
	app, fp := pushTestApp(t)
	clock := quietHours(app)
	u := denverMember(t, app, "owner")
	laptop := newPushDevice(t, app, u, "laptop")
	row := pushLastAttemptFixture(t, app, u, laptop)
	key := pushRecipientKey(row.ID, laptop.ID)
	last := pushDeliverType.Attempts()
	stopped := pushStepGates(t, app, "waited", 1)[0]

	stoppedDone := make(chan error, 1)
	go func() {
		_, err := jobs.NewRunner(app.DB, app.pushKinds()...).RunOnce(context.Background(), "stopping worker")
		stoppedDone <- err
	}()
	waitClosed(t, "the worker's wait to commit", stopped.reached)

	recovery := jobs.NewRunner(app.DB, app.pushKinds()...)
	recovery.Lease = time.Nanosecond
	if ran, err := recovery.RunOnce(context.Background(), "recovery"); err != nil || ran {
		t.Fatalf("lease recovery overnight: ran %v, %v, job %+v; want nothing to take back", ran, err, pushJobByKey(t, app, key))
	}
	opens, _ := app.pushOpensAt(app.pushNow(), denverZone)
	pushJobWaitsUntil(t, app, key, opens, last-1)
	gormMust(t, app.DB.First(&row, row.ID))
	if row.Status != models.RecipientQueued || !strings.HasPrefix(row.Reason, pushWaitPrefix) || fp.total() != 0 {
		t.Fatalf("overnight: row %s %q, sends %v", row.Status, row.Reason, fp.sent)
	}

	clock.set(8, 0, 16)
	makeDue(app)
	drainJobs(t, app)
	gormMust(t, app.DB.First(&row, row.ID))
	j := pushJobByKey(t, app, key)
	if row.Status != models.RecipientSent || fp.count(laptop.path) != 1 || j.Status != models.JobDone || j.Attempts != last {
		t.Fatalf("in the morning: row %s %q, sends %v, job %s after %d attempts", row.Status, row.Reason, fp.sent, j.Status, j.Attempts)
	}

	close(stopped.release)
	if err := <-stoppedDone; err != nil {
		t.Fatal(err)
	}
	if late := pushJobByKey(t, app, key); late.Status != models.JobDone || late.Attempts != last || !late.RunAt.Equal(j.RunAt) || fp.total() != 1 {
		t.Fatalf("the stopped worker's late finish: job %+v, sends %v", late, fp.sent)
	}
}

// TestPushWaitSerializesWithErasure: an announcement push to the person's
// laptop passes its checks in quiet hours, after their phone's job recorded
// "device gone" on the row, and reaches its wait while an admin's erasure of
// the person holds their account and has already scrubbed their message rows
// (a settled row has nothing to scrub, so the scrub passes it). The wait
// takes the account first, so it waits for the erasure to commit and then
// sees the person is no longer active: the row is never put back in the
// queue for an erased account, and nothing is sent in the morning.
func TestPushWaitSerializesWithErasure(t *testing.T) {
	app, fp := pushTestApp(t)
	clock := quietHours(app)
	newTestUser(t, app, "root", "password1234", models.RoleAdmin)
	admin := loginAPI(t, app, "root", "password1234")
	u := denverMember(t, app, "owner")
	laptop := newPushDevice(t, app, u, "laptop")
	row := pushLastAttemptFixture(t, app, u, laptop)
	key := pushRecipientKey(row.ID, laptop.ID)
	gormMust(t, app.DB.Model(&models.MessageRecipient{}).Where("id = ?", row.ID).
		UpdateColumns(map[string]any{"status": models.RecipientSuppressed, "reason": pushWhyGone, "address": "", "error": ""}))
	gate := pushStepGates(t, app, "wait", 1)[0]

	pushDone := make(chan error, 1)
	go func() {
		_, err := jobs.NewRunner(app.DB, app.pushKinds()...).RunOnce(context.Background(), "laptop worker")
		pushDone <- err
	}()
	waitClosed(t, "the laptop's push to pass its checks and reach its wait", gate.reached)

	scrubbed, releaseErasure := erasureTestPause(t, app.DB, erasureTestPoint{Processor: "raw", After: true, Step: "gorm:raw",
		SQLContains: "UPDATE message_recipients SET address = ''"})
	erased := make(chan int, 1)
	go func() { erased <- admin.do("DELETE", "/api/v1/admin/users/"+itoa(u.ID), nil, true).Code }()
	waitClosed(t, "the erasure to pass its message row scrub", scrubbed)

	close(gate.release)
	testdb.WaitUntil(t, "the push's wait to block on the account or finish", 10*time.Second, func() bool {
		if len(pushDone) > 0 {
			return true
		}
		var n int64
		if err := app.DB.Raw(`SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'
			AND state = 'active' AND query LIKE '%FOR SHARE OF u%'`).Scan(&n).Error; err != nil {
			t.Fatal(err)
		}
		return n > 0
	})
	releaseErasure()
	if code := <-erased; code != http.StatusOK {
		t.Fatalf("the erasure: %d", code)
	}
	if err := <-pushDone; err != nil {
		t.Fatal(err)
	}
	gormMust(t, app.DB.First(&row, row.ID))
	if row.Status != models.RecipientSuppressed || row.Reason != pushWhyGone || row.Address != "" || row.Error != "" {
		t.Fatalf("after the erasure and the wait: row %s %q (address %q, error %q); want the erased person's device miss left settled",
			row.Status, row.Reason, row.Address, row.Error)
	}

	clock.set(8, 0, 16)
	makeDue(app)
	drainJobs(t, app)
	gormMust(t, app.DB.First(&row, row.ID))
	if j := pushJobByKey(t, app, key); row.Status != models.RecipientSuppressed || fp.total() != 0 || j.Status != models.JobDone {
		t.Fatalf("in the morning: row %s %q, sends %v, job %s", row.Status, row.Reason, fp.sent, j.Status)
	}
}
