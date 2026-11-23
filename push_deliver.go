package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"

	"fold/internal/jobs"
	"fold/internal/notify"
	"fold/internal/push"
	"fold/models"

	"gorm.io/gorm"
)

// Web push delivery (M3.13a, research 2.2, PLAN 5.12). One push.deliver job
// per device, so a retry only ever repeats that device and can never send
// twice to another one that already took it. The payload holds ids, never
// an endpoint. One aggregate notification_deliveries row per notification
// records the push channel, whatever the device count.

const (
	kindPushDeliver = "push.deliver"
	// How long a push service keeps an undelivered message (research 2.4):
	// a change to a date in the next 24 hours, an ordinary notice, and an
	// admin task. The bell and email are the record, so an expired push is
	// harmless.
	pushTTLSoon = 6 * time.Hour
	pushTTL     = 24 * time.Hour
	pushTTLTask = 72 * time.Hour
	// pushDeviceFailedFor is how long a failing device is kept before the
	// daily cleanup removes it.
	pushDeviceFailedFor = 30 * 24 * time.Hour
)

// pushJob is push.deliver's payload. NotificationID is set for a
// notification; MessageRecipientID for an announcement or reminder row (key
// push:mr:<id>:d:<device>). PlannedAt (Unix seconds) is when the sweep
// planned a push reminder to go; the job gives it 15 minutes' grace into
// quiet hours.
type pushJob struct {
	NotificationID     uint  `json:"notificationId,omitempty"`
	MessageRecipientID uint  `json:"messageRecipientId,omitempty"`
	DeviceID           uint  `json:"deviceId"`
	PlannedAt          int64 `json:"plannedAt,omitempty"`
}

func enqueuePushDelivery(tx *gorm.DB, notificationID, deviceID uint, at time.Time) error {
	_, err := pushDeliverType.Enqueue(tx, pushJob{NotificationID: notificationID, DeviceID: deviceID}, at, pushNotificationKey(notificationID, deviceID))
	return err
}

func (a *App) pushKinds() []jobs.Kind {
	return []jobs.Kind{{Type: pushDeliverType, Label: "Sending a notification to a device",
		Run: func(ctx context.Context, payload json.RawMessage) error {
			var p pushJob
			if err := json.Unmarshal(payload, &p); err != nil {
				return err
			}
			return a.deliverPush(ctx, p)
		}},
		{Type: pushDevicesOffType, Label: "Telling someone their devices' notifications were turned off",
			Run: func(ctx context.Context, payload json.RawMessage) error {
				var p devicesOffJob
				if err := json.Unmarshal(payload, &p); err != nil {
					return err
				}
				return a.noticeDevicesOff(ctx, p)
			}},
	}
}

// kindPushDevicesOff is queued by the users trigger (migration 00059) in
// the transaction that revoked someone's sessions or took the account out
// of active, when that removed any of their devices (DEC-134 C).
const kindPushDevicesOff = "push.devices_off"

// devicesOffJob is the trigger's payload: Key is unique to that change.
type devicesOffJob struct {
	UserID  uint   `json:"userId"`
	Devices int    `json:"devices"`
	Key     string `json:"key"`
}

// noticeDevicesOff writes the person one bell notice that their devices'
// notifications were turned off. Safe to run twice (the dedupe key); a
// deleted account gets nothing. Turning devices off yourself never queues
// this.
//
// The liveness read, notify.Send (which locks the recipient FOR KEY SHARE and
// rechecks deleted_at) and the delivery intent run in one transaction, so an
// erasure that commits first leaves no notice, and one that starts later waits
// and removes it (M7.4 R4-N, inventory N-06, F-3). A disabled account is still
// told: devices go off exactly when an account leaves active.
func (a *App) noticeDevicesOff(ctx context.Context, p devicesOffJob) error {
	return a.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var u models.User
		if err := tx.Select("id").First(&u, p.UserID).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		} else if err != nil {
			return err
		}
		_, err := notify.Send(tx, notify.Msg{
			Kind: notify.PushDevicesOff, To: []uint{u.ID}, SubjectType: "user", SubjectID: u.ID,
			Link: "/profile/notifications", DedupeKey: "push.devices_off:" + p.Key,
		})
		return err
	})
}

// lockScreen is what the device shows for n: its stored push text, with
// the safe line titled with the church's name, since it can't say what it
// is about (DEC-134 B). False for a row that doesn't push.
func (a *App) lockScreen(n *models.Notification) (push.Notification, bool) {
	render := renderPush
	if a.hooks.pushRender != nil {
		render = a.hooks.pushRender
	}
	note, ok := render(n)
	if k, known := notify.Lookup(n.Kind); ok && known && k.IsSafePush(note.Title, note.Body) {
		note.Title = a.siteName()
	}
	return note, ok
}

// queuePushFor queues one push.deliver per device for a single notice.
// It skips kinds without push or push text, then reads devices by user_id
// for the loaded notice. Errors are returned so the job retries; nothing
// is logged.
func queuePushFor(tx *gorm.DB, n *models.Notification) error {
	if n == nil || n.ID == 0 {
		return nil
	}
	k, ok := notify.Lookup(n.Kind)
	if !ok || !k.AllowPush || n.PushTitle == "" {
		return nil
	}
	var devices []uint
	if err := tx.Model(&models.PushSubscription{}).Where("user_id = ?", n.UserID).Order("id").Pluck("id", &devices).Error; err != nil {
		return err
	}
	for _, deviceID := range devices {
		if err := enqueuePushDelivery(tx, n.ID, deviceID, time.Now()); err != nil {
			return err
		}
	}
	return nil
}

// renderPush is a notification's lock-screen text (DEC-124): the kind's
// own short PushRender, stored on the row when it was written, never the
// bell body cut short. False for a kind that doesn't push.
func renderPush(n *models.Notification) (push.Notification, bool) {
	k, ok := notify.Lookup(n.Kind)
	if !ok || !k.AllowPush || n.PushTitle == "" {
		return push.Notification{}, false
	}
	return push.Notification{Title: n.PushTitle, Body: n.PushBody}, true
}

// pushOptions are the delivery headers for one row (research 2.4): a
// change to a date that starts within 24 hours is urgent and short-lived,
// a task waits up to 3 days at low urgency, and anything else keeps a day.
// The topic (n<id>) only collapses retries of the same notification still
// waiting at the push service; different rows never replace each other.
// soon is startsSoon's answer for the row.
func pushOptions(k notify.Kind, n *models.Notification, soon bool) push.Options {
	opts := push.Options{TTL: pushTTL, Urgency: push.UrgencyNormal, Topic: "n" + strconv.FormatUint(uint64(n.ID), 10)}
	switch {
	case k.Waiting:
		opts.TTL, opts.Urgency = pushTTLTask, push.UrgencyLow
	case soon:
		opts.TTL, opts.Urgency = pushTTLSoon, push.UrgencyHigh
	}
	return opts
}

// startsSoon reports, for a row about event dates (a PushSoon kind),
// whether the earliest date not yet over starts within 24 hours of now
// (soon), and whether every date it names is already over (over). The row
// names its dates by its occurrence subject, or by its event subject plus
// Data "dates" (the original dates). A soon push is urgent and goes
// whatever the hour (DEC-126); an over one doesn't push at all, so a
// change to a finished date never wakes anyone at 3 AM. A row that names
// no date, or dates that no longer exist, is neither.
func (a *App) startsSoon(ctx context.Context, k notify.Kind, n *models.Notification, now time.Time) (soon, over bool, err error) {
	if !k.PushSoon || n.SubjectID == nil {
		return false, false, nil
	}
	db := a.DB.WithContext(ctx).Model(&models.EventOccurrence{})
	switch n.SubjectType {
	case "occurrence":
		db = db.Where("id = ?", *n.SubjectID)
	case "event":
		var data struct {
			Dates []string `json:"dates"`
		}
		if json.Unmarshal([]byte(n.Data), &data) != nil || len(data.Dates) == 0 {
			return false, false, nil
		}
		// original_date is a date column: a stored value that is not a real
		// date names no date, rather than failing the whole read.
		dates := make([]string, 0, len(data.Dates))
		for _, d := range data.Dates {
			if isCalendarDate(d) {
				dates = append(dates, d)
			}
		}
		if len(dates) == 0 {
			return false, false, nil
		}
		db = db.Where("event_id = ? AND original_date IN ?", *n.SubjectID, dates)
	default:
		return false, false, nil
	}
	// A date is over once it ends (greatest() in case an old row's end
	// isn't after its start). bool_and over no rows is NULL: not over.
	var first sql.NullTime
	var allOver sql.NullBool
	if err := db.Select("min(starts_at) FILTER (WHERE greatest(ends_at, starts_at) > ?), bool_and(greatest(ends_at, starts_at) <= ?)", now, now).
		Row().Scan(&first, &allOver); err != nil {
		return false, false, err
	}
	return first.Valid && first.Time.Before(now.Add(24*time.Hour)), allOver.Valid && allOver.Bool, nil
}

// reasonRecipientNotActive is the delivery reason, on a notice's email row
// and its push row alike, when the recipient's account isn't active.
const reasonRecipientNotActive = "recipient not active"

// Delivery reasons a push row can carry (notification_deliveries.reason).
const (
	pushWhyChurchOff  = "push turned off by the church"
	pushWhyInactive   = reasonRecipientNotActive
	pushWhyNotOn      = "push not turned on"
	pushWhyGone       = "device gone"
	pushWhyTooLarge   = "the notification was too large to push"
	pushWhyRefused    = "the push service refused it"
	pushWhyNotReached = "the push service couldn't be reached"
	pushWhyRead       = "already read"
	pushWhyStale      = "no longer current"
	pushWhyHourly     = "hourly limit"
)

// Reasons on a sent row: why it went whatever the hour. An optional push's
// sent row has no reason, and only those count toward the hourly limit.
const (
	pushSentRequired = "required"
	pushSentSoon     = "event starting within 24 hours"
)

// pushHourlyLimit is how many optional pushes a person gets in an hour
// (DEC-126); past it they are suppressed and the bell still has them.
const pushHourlyLimit = 20

func (a *App) pushNow() time.Time { return a.now() }

// pushQuietHours is the window optional pushes wait for (DEC-126):
// push.DefaultWindow, 8 AM to 8 PM in the recipient's zone, until M3.8
// brings the shared, configurable one.
func (a *App) pushQuietHours() push.Window {
	if a.hooks.pushWindow != nil {
		return *a.hooks.pushWindow
	}
	return push.DefaultWindow
}

// pushStale says why n should no longer push, or "": it was archived, has
// expired, or is a task someone resolved (no longer current), or it is an
// ordinary item the person already read in Fold. A task is never "read"
// (it stays counted until resolved), so reading one doesn't stop its push.
// This matters most for a push that waited out quiet hours.
func pushStale(n *models.Notification, now time.Time) string {
	switch {
	case notify.Settled(n, now):
		return pushWhyStale
	case !n.Waiting && n.ReadAt != nil:
		return pushWhyRead
	}
	return ""
}

// pushItem is what one push is for: a notification (NotificationID) or
// an announcement's push row (RecipientID, M3.13d2), and whose it is.
type pushItem struct {
	UserID, NotificationID, RecipientID uint
}

// overHourlyLimit reports whether the person already had pushHourlyLimit
// optional pushes in the hour before now: notification pushes and
// announcement pushes together. It counts items, not devices: a
// notification or an announcement has one push row however many devices
// took it. Once the item's own row is sent (another device took it), it
// is never limited, so a person's second device always gets what the
// first did. Required pushes and urgent event changes carry a reason on
// their sent row and aren't counted; announcement pushes are always
// optional, and so are push reminders (M3.13d3): they count here, but
// the limit never stops one. idx_notification_deliveries_push_sent (migration 00060) and
// idx_message_recipients_push_sent (00061) cover the counts; their
// predicates are written as literals so a cached generic plan still
// matches the partial indexes.
//
// There's no lock: workers racing for the last slot can each see 19 and
// send, so a burst can overshoot by up to about the number of workers.
// A night's backlog wakes together at 8:00, and only the first 20 go; the
// rest are suppressed and stay in the bell or the inbox (DEC-126 as
// written).
func (a *App) overHourlyLimit(ctx context.Context, it pushItem, now time.Time) (bool, error) {
	var r struct {
		OwnSent bool
		Recent  int64
	}
	since := now.Add(-time.Hour)
	err := a.DB.WithContext(ctx).Raw(`SELECT
		EXISTS (SELECT 1 FROM notification_deliveries WHERE notification_id = ? AND channel = 'push' AND status = 'sent')
			OR EXISTS (SELECT 1 FROM message_recipients WHERE id = ? AND channel = 'push' AND status = 'sent') AS own_sent,
		(SELECT count(*) FROM notification_deliveries
			WHERE user_id = ? AND channel = 'push' AND status = 'sent' AND reason = '' AND sent_at > ? AND notification_id <> ?)
		+ (SELECT count(*) FROM message_recipients
			WHERE user_id = ? AND channel = 'push' AND status = 'sent' AND sent_at > ? AND id <> ?) AS recent`,
		it.NotificationID, it.RecipientID, it.UserID, since, it.NotificationID, it.UserID, since, it.RecipientID).Scan(&r).Error
	return !r.OwnSent && r.Recent >= pushHourlyLimit, err
}

// deliverPush sends one notification to one device. It returns an error
// only when a retry may succeed (429, 5xx, a timeout).
func (a *App) deliverPush(ctx context.Context, p pushJob) error {
	if p.MessageRecipientID != 0 && p.NotificationID == 0 && p.DeviceID != 0 {
		return a.deliverMessagePush(ctx, p.MessageRecipientID, p.DeviceID, p.PlannedAt)
	}
	if p.NotificationID == 0 || p.DeviceID == 0 {
		return nil // nothing else is valid
	}
	db := a.DB.WithContext(ctx)
	var n models.Notification
	if err := db.First(&n, p.NotificationID).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	kind, ok := notify.Lookup(n.Kind)
	if !ok {
		return nil
	}
	var u models.User
	if err := db.Select("id", "status", "timezone").First(&u, n.UserID).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	// The account and its choices come before the device, as for an
	// announcement (pushDestinationIn): devices go when an account stops
	// being active, and that is "recipient not active", not "device gone".
	t := pushTarget{what: fmt.Sprintf("notification %d (%s)", n.ID, n.Kind), rec: notificationRecorder(n), row: pushRow{notificationID: n.ID}}
	if why, err := a.pushHeld(db, &u, kind.Category, kind.Required); err != nil {
		return err
	} else if why != "" {
		return t.record(a, models.DeliverySuppressed, why)
	}
	if err := db.Where("id = ?", p.DeviceID).Limit(1).Find(&t.dev).Error; err != nil {
		return err
	}
	if t.dev.ID == 0 || t.dev.UserID != n.UserID {
		// Turned off, gone, or another account's since this was queued: it is
		// never sent there, and the delivery is settled as a device that moved
		// (settlePushMiss), so the notice's push is never left undecided.
		return a.settlePushMiss(db, t, n.UserID, p.DeviceID, models.DeliverySuppressed, pushWhyGone)
	}
	now := a.pushNow()
	if why := pushStale(&n, now); why != "" {
		return a.recordPush(n, models.DeliverySuppressed, why)
	}
	// Quiet hours and the hourly limit (DEC-126): a required kind, or a
	// change to a date starting within 24 hours, goes whatever the hour
	// and isn't counted. Anything else waits for the recipient's window
	// (this job runs again from the top then, rechecking all of the
	// above), and past the limit it is dropped; the bell still has it.
	soon, over, err := a.startsSoon(ctx, kind, &n, now)
	if err != nil {
		return err
	}
	if over {
		return a.recordPush(n, models.DeliverySuppressed, pushWhyStale)
	}
	sentWhy := ""
	switch {
	case kind.Required:
		sentWhy = pushSentRequired
	case soon:
		sentWhy = pushSentSoon
	default:
		// The zone is resolved as emails resolve it (userZoneIn reads the
		// church's only when the person's own doesn't load).
		if opens, wait := a.pushOpensAt(now, userZoneIn(a.DB.WithContext(ctx), u.Timezone)); wait {
			return jobs.Later(opens)
		}
		over, err := a.overHourlyLimit(ctx, pushItem{UserID: n.UserID, NotificationID: n.ID}, now)
		if err != nil {
			return err
		}
		if over {
			return a.recordPush(n, models.DeliverySuppressed, pushWhyHourly)
		}
	}
	note, ok := a.lockScreen(&n)
	if !ok {
		return nil
	}
	body, err := a.pushBody(&n, note)
	if err != nil {
		return t.tooLarge(a)
	}
	return a.sendPush(ctx, t, body, pushOptions(kind, &n, soon), sentWhy)
}

// pushTarget is one push to one device: what it is for in logs (never a
// device's endpoint), the device, and the writer of the one row its outcome
// goes on (a notification's push delivery, or an announcement's or
// reminder's push row). The three kinds of push differ in what they check
// and say; they share the checks below and this send.
type pushTarget struct {
	what string
	dev  models.PushSubscription
	rec  pushRecorder
	row  pushRow
}

func (t pushTarget) record(a *App, status, reason string) error {
	return t.rec(a.DB, status, reason, a.pushNow())
}

// tooLarge records a payload past the cap: a bug in the push text, not
// something a retry fixes.
func (t pushTarget) tooLarge(a *App) error {
	log.Printf("push: %s is too large to push: a bug in its push text", t.what)
	return t.record(a, models.DeliveryFailed, pushWhyTooLarge)
}

// pushHeld runs the checks every push shares, in order: the church's
// switch, that the account is active, and the person's consent (DEC-122):
// push on, and category not turned off unless the kind is required. It
// returns why the push must not go, or "" when it may.
func (a *App) pushHeld(db *gorm.DB, u *models.User, category string, required bool) (string, error) {
	if on, err := boolSettingErrIn(db, settingPushEnabled, true); err != nil {
		return "", err
	} else if !on {
		return pushWhyChurchOff, nil
	}
	if u.Status != models.UserActive {
		return pushWhyInactive, nil
	}
	on, categoryOff, err := pushConsent(db, u.ID, category)
	switch {
	case err != nil:
		return "", err
	case !on:
		return pushWhyNotOn, nil
	case categoryOff && !required:
		return pushWhyCategoryOff(category), nil
	}
	return "", nil
}

// pushDest is where an announcement or reminder push goes, as the snapshot
// that builds its payload sees it: the person, the device it is sent to, and
// why it must not go. moved means the device is gone or belongs to someone
// else now; gone means the account is.
type pushDest struct {
	user  models.User
	dev   models.PushSubscription
	why   string
	moved bool
	gone  bool
}

func (d pushDest) sends() bool { return d.why == "" && !d.moved && !d.gone }

// pushDestinationIn reads, on tx (the snapshot that builds the payload), the
// account, the church's switch, consent and the device's owner. The device is
// what is sent to, so a device rebound to another account (same endpoint and
// keys, api_push.go) or turned off before the snapshot is never sent this
// person's payload. The provider is still called after the snapshot ends: a
// change committed after it began can't stop that one send.
func (a *App) pushDestinationIn(tx *gorm.DB, userID, deviceID uint, category string, required bool) (pushDest, error) {
	var d pushDest
	if err := tx.Where("id = ?", userID).Limit(1).Find(&d.user).Error; err != nil {
		return d, err
	}
	if d.user.ID == 0 {
		d.gone = true
		return d, nil
	}
	why, err := a.pushHeld(tx, &d.user, category, required)
	if err != nil || why != "" {
		d.why = why
		return d, err
	}
	if err := tx.Where("id = ?", deviceID).Limit(1).Find(&d.dev).Error; err != nil {
		return d, err
	}
	d.moved = d.dev.ID == 0 || d.dev.UserID != d.user.ID
	return d, nil
}

// pushDestOutcome records what a destination that doesn't send means: a
// device that moved is a miss for that device alone (settlePushMiss).
func (a *App) pushDestOutcome(db *gorm.DB, t pushTarget, d pushDest, deviceID uint) error {
	switch {
	case d.gone:
		return nil
	case d.why != "":
		return t.record(a, models.RecipientSuppressed, d.why)
	}
	return a.settlePushMiss(db, t, d.user.ID, deviceID, models.RecipientSuppressed, pushWhyGone)
}

// pushSettleTx is a push settlement's transaction: each statement sees what
// committed while the one before waited on the row lock, such as a sibling
// that went back to waiting for quiet hours.
var pushSettleTx = &sql.TxOptions{Isolation: sql.LevelReadCommitted}

// settlePushMiss records, on t's row, an attempt that ended without
// delivering for a reason about its device alone (gone, moved, refused, not
// reached), unless another of userID's devices' jobs for the row will still
// run and decide it.
func (a *App) settlePushMiss(db *gorm.DB, t pushTarget, userID, deviceID uint, status, reason string) error {
	return db.Transaction(func(tx *gorm.DB) error {
		return a.settlePushMissIn(tx, t, userID, deviceID, status, reason)
	}, pushSettleTx)
}

// settlePushMissIn is settlePushMiss on tx, which must be a pushSettleTx. An
// announcement's or reminder's row is locked first, which orders this with a
// sibling's wait for quiet hours (pushWaitIn): one that waited first is seen
// as a job that will run again, and one that waits after moves this miss
// back to waiting.
func (a *App) settlePushMissIn(tx *gorm.DB, t pushTarget, userID, deviceID uint, status, reason string) error {
	if t.row.recipientID != 0 {
		var locked []uint
		if err := tx.Raw("SELECT id FROM message_recipients WHERE id = ? FOR UPDATE", t.row.recipientID).Scan(&locked).Error; err != nil {
			return err
		}
	}
	if waiting, err := pushSiblingWaiting(tx, t.row, userID, deviceID); err != nil || waiting {
		return err
	}
	return t.rec(tx, status, reason, a.pushNow())
}

// pushDeviceMisses are the reasons settlePushMiss records: each is about one
// device, so another of the person's devices that is still to run may yet
// deliver the row (pushWaitIn).
var pushDeviceMisses = []string{pushWhyGone, pushWhyNotReached, pushWhyRefused}

func pushWhyCategoryOff(category string) string {
	return "push turned off for " + notify.CategoryEmailName(category)
}

// pushOpensAt reports, when now is outside the person's window (DEC-126),
// when it opens. The answer is at least a second out, so the app's clock
// running a little behind the database's can't make a job spin.
func (a *App) pushOpensAt(now time.Time, loc *time.Location) (opens time.Time, wait bool) {
	inside, opens := a.pushQuietHours().Check(now, loc)
	if inside {
		return time.Time{}, false
	}
	if soonest := now.Add(time.Second); opens.Before(soonest) {
		opens = soonest
	}
	return opens, true
}

func pushSubscription(d models.PushSubscription) push.Subscription {
	return push.Subscription{Endpoint: d.Endpoint, P256dh: d.P256dh, Auth: d.Auth}
}

// recipientPushOptions are the delivery headers for an announcement's or
// reminder's row; the topic only collapses retries of that row.
func recipientPushOptions(rowID uint, ttl time.Duration) push.Options {
	return push.Options{TTL: ttl, Urgency: push.UrgencyNormal, Topic: "mr" + idString(rowID)}
}

// sendPush sends body to t's device and records what the push service said.
// sentWhy is the sent row's reason ("" for an optional push).
func (a *App) sendPush(ctx context.Context, t pushTarget, body []byte, opts push.Options, sentWhy string) error {
	return a.pushOutcome(ctx, t, a.pushClient().Send(ctx, pushSubscription(t.dev), body, opts), sentWhy, false)
}

// pushRow names the one row a push's outcome goes on, for finding the jobs
// that may still decide it: a notification's push delivery, or an
// announcement's or reminder's push row. The person's own test has neither.
type pushRow struct{ notificationID, recipientID uint }

func (r pushRow) key(deviceID uint) string {
	if r.recipientID != 0 {
		return pushRecipientKey(r.recipientID, deviceID)
	}
	return pushNotificationKey(r.notificationID, deviceID)
}

// pushSiblingWaiting reports whether a push.deliver job for row, to another of
// userID's devices than except, will run again (jobs.WillRunAgain): queued,
// including one gone back to wait for quiet hours (pushWaitIn), or waiting
// to retry. It will decide the row, so an attempt that took nothing may leave
// it. Any running sibling decides for itself, and a delivery upgrades whatever
// this attempt records to sent (messagePushRecorder, recordPushIn); a device
// added after the jobs were queued, or whose job has ended, takes nothing.
// Only real jobs count, so two last attempts never each leave the row to the
// other.
func pushSiblingWaiting(db *gorm.DB, row pushRow, userID, except uint) (bool, error) {
	if row == (pushRow{}) {
		return false, nil
	}
	var devices []uint
	if err := db.Model(&models.PushSubscription{}).Where("user_id = ? AND id <> ?", userID, except).Order("id").Pluck("id", &devices).Error; err != nil || len(devices) == 0 {
		return false, err
	}
	keys := make([]string, len(devices))
	for i, d := range devices {
		keys[i] = row.key(d)
	}
	var waiting int64
	err := jobs.WillRunAgain(db.Model(&models.Job{}).Where("unique_key IN ?", keys)).Limit(1).Count(&waiting).Error
	return waiting > 0, err
}

// dispatchedDevice narrows q to the device as it was when the push went: the
// same row, owner, keys and binding generation. A shared browser rebound to
// another account, or the same owner's renewed subscription, saved while the
// push service was answering, is a different subscription, and the old
// attempt's outcome must not stamp, fail or delete it. The generation goes up
// on every change of owner or key (migration 00011) and never repeats for a
// row, so a browser handed away and back, or renewed and given its old keys
// again, before the answer is not the device as sent either. No timestamp
// stands in for it: a clock reading can repeat.
func dispatchedDevice(q *gorm.DB, d models.PushSubscription) *gorm.DB {
	return q.Where("id = ? AND user_id = ? AND p256dh = ? AND auth = ? AND binding_generation = ?", d.ID, d.UserID, d.P256dh, d.Auth, d.BindingGeneration)
}

// markPushUsed stamps a device that took a push (at most once a day, and
// clearing an earlier failure). It never turns a delivery into a retry.
func (a *App) markPushUsed(d models.PushSubscription) {
	if err := dispatchedDevice(a.DB.Model(&models.PushSubscription{}), d).
		Where("last_used_at < now() - interval '1 day' OR failed_at IS NOT NULL").
		UpdateColumns(map[string]any{"last_used_at": gorm.Expr("now()"), "failed_at": nil}).Error; err != nil {
		log.Printf("push: device %d: marking it used: %v", d.ID, err)
	}
}

// stampPushUsed stamps a device that took the person's own test, every
// time, and clears an earlier failure.
func (a *App) stampPushUsed(d models.PushSubscription) {
	if err := dispatchedDevice(a.DB.Model(&models.PushSubscription{}), d).
		UpdateColumns(map[string]any{"last_used_at": time.Now().UTC(), "failed_at": nil}).Error; err != nil {
		log.Printf("push: device %d: marking it used: %v", d.ID, err)
	}
}

// pushRecorder writes a push's outcome on its one row: a notification's
// push delivery, or an announcement's push row. now is the send time on
// a sent row, which the hourly limit counts by.
type pushRecorder func(db *gorm.DB, status, reason string, now time.Time) error

func notificationRecorder(n models.Notification) pushRecorder {
	return func(db *gorm.DB, status, reason string, now time.Time) error {
		return recordPushIn(db, n, status, reason, now)
	}
}

// pushBody is the Declarative Web Push JSON for n: the rendered text, a
// tap that opens the notification's own link on SITE_URL, and a tag so a
// second copy replaces the first on the device. It is refused past the
// 3,000-byte cap.
func (a *App) pushBody(n *models.Notification, note push.Notification) ([]byte, error) {
	note.Navigate = a.emailLink(n.Link)
	if note.Tag == "" {
		note.Tag = "n" + strconv.FormatUint(uint64(n.ID), 10)
	}
	return push.MarshalPayload(push.NewPayload(note))
}

// pushOutcome records what the push service said about t's device;
// sentWhy is the sent row's reason ("" for an optional push). ownTest is
// the person's own test send: it is the proof the device works, and the
// device cap evicts by last use, so it stamps the device every time (a
// delivery stamps at most once a day); and a test nobody retries, so a
// refusal or a failure is logged and leaves the device as it was. A device
// the push service says is gone is dropped the same way for both.
func (a *App) pushOutcome(ctx context.Context, t pushTarget, res push.Result, sentWhy string, ownTest bool) error {
	dev, what := t.dev, t.what
	record := func(status, reason string) error { return t.record(a, status, reason) }
	if ownTest && res.Status != push.Accepted && res.Status != push.Gone {
		log.Printf("push: %s to device %d for user %d: %s (%d)", what, dev.ID, dev.UserID, res.Status, res.Code)
		return nil
	}
	switch res.Status {
	case push.Accepted:
		// It went: nothing below may turn this into a retry, which would
		// send it again.
		if ownTest {
			a.stampPushUsed(dev)
		} else {
			a.markPushUsed(dev)
		}
		if err := record(models.DeliverySent, sentWhy); err != nil {
			log.Printf("push: %s: recording the delivery: %v", what, err)
		}
		return nil
	case push.Gone:
		log.Printf("push: device gone for user %d (device %d, %d)", dev.UserID, dev.ID, res.Code)
		return a.DB.Transaction(func(tx *gorm.DB) error {
			res := dispatchedDevice(tx, dev).Delete(&models.PushSubscription{})
			if res.Error != nil {
				return res.Error
			}
			// Not the device as sent: rebound, renewed or bound back while the
			// push service answered (still there), or already deleted (gone).
			var changed models.PushSubscription
			if res.RowsAffected == 0 && !ownTest {
				if err := tx.Select("id", "user_id").Where("id = ?", dev.ID).Limit(1).Find(&changed).Error; err != nil {
					return err
				}
			}
			if changed.ID != 0 && !jobs.LastAttempt(ctx) {
				// The next attempt reads it again: still the person's, it gets
				// the push; someone else's, it is decided as a moved device.
				return fmt.Errorf("push: device %d changed while its push was answered (retrying)", dev.ID)
			}
			// This device's job ends here: the row waits only for another of
			// the person's devices' jobs that will still run.
			if changed.ID != 0 && changed.UserID == dev.UserID {
				// Still the person's device, but its last attempt never reached
				// it as it is now.
				return a.settlePushMissIn(tx, t, dev.UserID, dev.ID, models.DeliveryFailed, pushWhyNotReached)
			}
			return a.settlePushMissIn(tx, t, dev.UserID, dev.ID, models.DeliverySuppressed, pushWhyGone)
		}, pushSettleTx)
	case push.TooLarge:
		log.Printf("push: %s was too large for the push service (%d): a bug in its push text", what, res.Code)
		return record(models.DeliveryFailed, pushWhyTooLarge)
	case push.RateLimited, push.Temporary:
		if !jobs.LastAttempt(ctx) {
			return fmt.Errorf("push: device %d: %s (retrying): %w", dev.ID, res.Status, res.Err)
		}
		a.markPushFailed(dev)
		if err := a.settlePushMiss(a.DB, t, dev.UserID, dev.ID, models.DeliveryFailed, pushWhyNotReached); err != nil {
			return err
		}
		return fmt.Errorf("push: device %d: %s: %w", dev.ID, res.Status, res.Err)
	default:
		log.Printf("push: %s to device %d: %s (%d)", what, dev.ID, res.Status, res.Code)
		a.markPushFailed(dev)
		return a.settlePushMiss(a.DB, t, dev.UserID, dev.ID, models.DeliveryFailed, pushWhyRefused)
	}
}

// markPushFailed stamps a device's first failure; the daily cleanup
// removes it 30 days later unless a send succeeds first.
func (a *App) markPushFailed(d models.PushSubscription) {
	if err := dispatchedDevice(a.DB.Model(&models.PushSubscription{}), d).Where("failed_at IS NULL").
		UpdateColumn("failed_at", gorm.Expr("now()")).Error; err != nil {
		log.Printf("push: device %d: marking it failed: %v", d.ID, err)
	}
}

// pushConsent reads the person's latest push consent (DEC-122): on is the
// (push, all) row opting in; categoryOff is category's row opting out.
func pushConsent(db *gorm.DB, userID uint, category string) (on, categoryOff bool, err error) {
	var rows []models.Consent
	if err := db.Raw(`SELECT DISTINCT ON (scope) scope, status FROM consents
		WHERE user_id = ? AND channel = ? AND scope IN ('all', ?) ORDER BY scope, at DESC, id DESC`,
		userID, models.ConsentPush, categoryScope(category)).Scan(&rows).Error; err != nil {
		return false, false, err
	}
	for _, r := range rows {
		switch r.Scope {
		case "all":
			on = r.Status == models.ConsentOptedIn
		default:
			categoryOff = r.Status == models.ConsentOptedOut
		}
	}
	return on, categoryOff, nil
}

func (a *App) recordPush(n models.Notification, status, reason string) error {
	return recordPushIn(a.DB, n, status, reason, a.pushNow())
}

// recordPushIn writes the notification's one push delivery row. A sent
// row is final; any other first outcome stays unless a later device takes
// it, which upgrades the row to sent. now is the send time on a sent row,
// which the hourly limit counts by.
func recordPushIn(db *gorm.DB, n models.Notification, status, reason string, now time.Time) error {
	var sentAt *time.Time
	if status == models.DeliverySent {
		at := now.UTC()
		sentAt = &at
	}
	return db.Exec(`INSERT INTO notification_deliveries (notification_id, user_id, channel, status, reason, sent_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (notification_id, channel) DO UPDATE SET status = excluded.status, reason = excluded.reason, sent_at = excluded.sent_at, updated_at = now()
		WHERE excluded.status = 'sent' AND notification_deliveries.status <> 'sent'`,
		n.ID, n.UserID, models.DeliveryPush, status, reason, sentAt).Error
}

// prunePushDevices removes devices that have been failing for 30 days or
// whose subscription has expired (part of the daily notifications.cleanup).
func prunePushDevices(db *gorm.DB, now time.Time) (int64, error) {
	res := db.Exec("DELETE FROM push_subscriptions WHERE failed_at < ? OR expires_at <= ?", now.Add(-pushDeviceFailedFor), now)
	return res.RowsAffected, res.Error
}
