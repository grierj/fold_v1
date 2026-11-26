package app

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"gorm.io/gorm"

	"fold/internal/policy"
	"fold/internal/testdb"
	"fold/models"
)

func TestRecipientStatusesMatchTheSchema(t *testing.T) {
	check := enumeratedChecks(t)["message_recipients.status"]
	named := map[string]bool{}
	for _, m := range policy.RecipientMoves() {
		for _, s := range append(m.From(), m.To()) {
			named[s] = true
		}
	}
	var fromTable []string
	for s := range named {
		fromTable = append(fromTable, s)
	}
	slices.Sort(fromTable)
	consts := []string{models.RecipientQueued, models.RecipientSuppressed, models.RecipientDeferred, models.RecipientSent,
		models.RecipientDelivered, models.RecipientBounced, models.RecipientComplained, models.RecipientFailed}
	slices.Sort(consts)
	if len(check) == 0 || !reflect.DeepEqual(fromTable, check) || !reflect.DeepEqual(consts, check) {
		t.Fatalf("message_recipients_status_check allows %v; the moves name %v and models names %v", check, fromTable, consts)
	}
}

// The push path shared with notification deliveries hands a message's push
// recorder its outcome as a notification_deliveries status; each one it hands
// over is recorded as the same value on the recipient row, and the channels
// the moves name are the values the creators insert.
func TestRecipientPushOutcomesMatchDeliveryStatuses(t *testing.T) {
	for _, outcome := range []string{models.DeliverySent, models.DeliverySuppressed, models.DeliveryFailed} {
		move, ok := policy.RecipientPushOutcome(outcome)
		if !ok || move.To() != outcome {
			t.Errorf("the push outcome %q is recorded by %q as %q", outcome, move, move.To())
		}
	}
	if policy.RecipientChannelEmail != models.DeliveryEmail || policy.RecipientChannelPush != models.DeliveryPush {
		t.Errorf("the moves name channels %q and %q; the creators insert %q and %q",
			policy.RecipientChannelEmail, policy.RecipientChannelPush, models.DeliveryEmail, models.DeliveryPush)
	}
}

var recipientTestChannels = []string{models.DeliveryEmail, models.DeliveryPush, "sms"}

type recipientTestRow struct{ channel, status string }

// recipientsInEveryStatus writes a message with one row in each status on
// each channel, one person per status (the rows' unique key).
func recipientsInEveryStatus(t *testing.T, app *App, statuses []string, people []uint) (uint, map[uint]recipientTestRow) {
	t.Helper()
	m := models.Message{Status: models.MessageSending, Category: models.MessageCategoryAnnouncement, Subject: "rows"}
	if err := app.DB.Create(&m).Error; err != nil {
		t.Fatal(err)
	}
	out := map[uint]recipientTestRow{}
	for i, s := range statuses {
		for _, ch := range recipientTestChannels {
			r := models.MessageRecipient{MessageID: m.ID, UserID: people[i], Channel: ch, Status: s}
			if err := app.DB.Create(&r).Error; err != nil {
				t.Fatal(err)
			}
			out[r.ID] = recipientTestRow{ch, s}
		}
	}
	return m.ID, out
}

func recipientRowOf(t *testing.T, app *App, id uint) models.MessageRecipient {
	t.Helper()
	var r models.MessageRecipient
	if err := app.DB.First(&r, id).Error; err != nil {
		t.Fatal(err)
	}
	return r
}

func recipientTestPeople(t *testing.T, app *App, n int) []uint {
	t.Helper()
	out := make([]uint, n)
	for i := range out {
		out[i] = newTestUser(t, app, "person"+itoa(uint(i)), "password1234", models.RoleMember).ID
	}
	return out
}

// moveRecipient's SQL moves a row exactly when the move's Go rule allows its
// channel and status, to the move's status with the fields, and a false
// extra condition moves nothing.
func TestMoveRecipientSQLMatchesTheRule(t *testing.T) {
	statuses := enumeratedChecks(t)["message_recipients.status"] // before testApp: it clears the database
	app := testApp(t)
	people := recipientTestPeople(t, app, len(statuses))
	for _, move := range policy.RecipientMoves() {
		if len(move.From()) == 0 {
			continue
		}
		_, rows := recipientsInEveryStatus(t, app, statuses, people)
		for id, r := range rows {
			fields := map[string]any{"reason": "missed", "updated_at": time.Now()}
			if moved, err := moveRecipient(app.DB, id, move, fields, "r.reason <> ?", ""); err != nil || moved {
				t.Fatalf("%q on %s from %s with a false condition: moved %v, %v", move, r.channel, r.status, moved, err)
			}
			moved, err := moveRecipient(app.DB, id, move, map[string]any{"reason": "moved", "updated_at": gorm.Expr("now()")}, "")
			if err != nil {
				t.Fatal(err)
			}
			allows := move.Allows(r.channel, r.status)
			want, wantReason := r.status, ""
			if allows {
				want, wantReason = move.To(), "moved"
			}
			got := recipientRowOf(t, app, id)
			if moved != allows || got.Status != want || got.Reason != wantReason {
				t.Errorf("%q on %s from %s: moved %v to %s (%q), want moved %v to %s (%q)",
					move, r.channel, r.status, moved, got.Status, got.Reason, allows, want, wantReason)
			}
		}
	}
}

// The set form moves exactly the matching rows whose channel and status the
// move allows, and counts them; another message's rows stay.
func TestMoveRecipientsWhereSQLMatchesTheRule(t *testing.T) {
	statuses := enumeratedChecks(t)["message_recipients.status"] // before testApp: it clears the database
	app := testApp(t)
	people := recipientTestPeople(t, app, len(statuses))
	for _, move := range policy.RecipientMoves() {
		if len(move.From()) == 0 {
			continue
		}
		mid, rows := recipientsInEveryStatus(t, app, statuses, people)
		_, bystanders := recipientsInEveryStatus(t, app, statuses, people)
		n, err := moveRecipientsWhere(app.DB, move, map[string]any{"error": "moved", "updated_at": time.Now()}, "r.message_id = ?", mid)
		if err != nil {
			t.Fatal(err)
		}
		want := 0
		for id, r := range rows {
			got := recipientRowOf(t, app, id)
			if move.Allows(r.channel, r.status) {
				want++
				if got.Status != move.To() || got.Error != "moved" {
					t.Errorf("%q on %s from %s: now %s (%q), want %s", move, r.channel, r.status, got.Status, got.Error, move.To())
				}
			} else if got.Status != r.status || got.Error != "" {
				t.Errorf("%q on %s moved a %s row it may not: now %s (%q)", move, r.channel, r.status, got.Status, got.Error)
			}
		}
		if n != int64(want) {
			t.Errorf("%q counted %d moved, want %d", move, n, want)
		}
		for id, r := range bystanders {
			if got := recipientRowOf(t, app, id); got.Status != r.status || got.Error != "" {
				t.Errorf("%q moved another message's %s %s row to %s", move, r.channel, r.status, got.Status)
			}
		}
	}
}

// A field given as an expression is written as SQL: the email send's
// audience_admitted_at is set once and never moved after.
func TestMoveRecipientWritesExpressions(t *testing.T) {
	app := testApp(t)
	people := recipientTestPeople(t, app, 2)
	mid, _ := recipientsInEveryStatus(t, app, []string{models.RecipientQueued, models.RecipientQueued}, people)
	var rows []models.MessageRecipient
	if err := app.DB.Where("message_id = ? AND channel = ?", mid, models.DeliveryEmail).Order("id").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	earlier := time.Now().Add(-48 * time.Hour).UTC().Truncate(time.Second)
	if err := app.DB.Model(&models.MessageRecipient{}).Where("id = ?", rows[1].ID).Update("audience_admitted_at", earlier).Error; err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if err := markRecipient(app.DB, r.ID, policy.RecipientEmailSent, "", "pm-1", "", true); err != nil {
			t.Fatal(err)
		}
	}
	first, second := recipientRowOf(t, app, rows[0].ID), recipientRowOf(t, app, rows[1].ID)
	if first.Status != models.RecipientSent || first.AudienceAdmittedAt == nil || first.SentAt == nil || first.ProviderMessageID != "pm-1" {
		t.Errorf("the first row: %+v", first)
	}
	if second.AudienceAdmittedAt == nil || !second.AudienceAdmittedAt.Equal(earlier) {
		t.Errorf("the second row's admission moved: %v, want %v", second.AudienceAdmittedAt, earlier)
	}
}

func TestMoveRecipientRefusesWhatTheRuleDoesNotHold(t *testing.T) {
	app := testApp(t)
	people := recipientTestPeople(t, app, 1)
	mid, rows := recipientsInEveryStatus(t, app, []string{models.RecipientQueued}, people)
	var queued uint
	for id, r := range rows {
		if r.channel == models.DeliveryEmail {
			queued = id
		}
	}
	stamp := func(m map[string]any) map[string]any {
		m["updated_at"] = time.Now()
		return m
	}
	for name, try := range map[string]func() error{
		"a creation move": func() error {
			_, err := moveRecipient(app.DB, queued, policy.RecipientSnapshotQueued, stamp(map[string]any{}), "")
			return err
		},
		"an unknown move": func() error {
			_, err := moveRecipient(app.DB, queued, policy.RecipientMove("bounce twice"), stamp(map[string]any{}), "")
			return err
		},
		"a status in the fields": func() error {
			_, err := moveRecipient(app.DB, queued, policy.RecipientEmailFailed, stamp(map[string]any{"status": models.RecipientSent}), "")
			return err
		},
		"the status by its field name": func() error {
			_, err := moveRecipient(app.DB, queued, policy.RecipientEmailFailed, stamp(map[string]any{"Status": models.RecipientSent}), "")
			return err
		},
		"the status as a quoted column": func() error {
			_, err := moveRecipient(app.DB, queued, policy.RecipientEmailFailed, stamp(map[string]any{`"status"`: models.RecipientSent}), "")
			return err
		},
		"a column no move writes": func() error {
			_, err := moveRecipient(app.DB, queued, policy.RecipientEmailFailed, stamp(map[string]any{"address": ""}), "")
			return err
		},
		"a move that does not stamp updated_at": func() error {
			_, err := moveRecipient(app.DB, queued, policy.RecipientEmailFailed, map[string]any{"error": "x"}, "")
			return err
		},
		"a set with no condition": func() error {
			_, err := moveRecipientsWhere(app.DB, policy.RecipientEmailFailed, stamp(map[string]any{}), " ")
			return err
		},
	} {
		if try() == nil {
			t.Errorf("%s was written", name)
		}
	}
	var n int64
	if err := app.DB.Model(&models.MessageRecipient{}).Where("message_id = ? AND status = ?", mid, models.RecipientQueued).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != int64(len(rows)) {
		t.Fatalf("%d of %d rows are still queued", n, len(rows))
	}
}

// A send's own outcome is written on the plain handle, where a gorm write is
// its own transaction: the push recorder keeps that, so a COMMIT the server
// never confirms is the recorder's error. After a device miss the recorder is
// handed that step's transaction and writes inside it, with no COMMIT of its
// own.
func TestPushRecorderKeepsItsTransactionBoundary(t *testing.T) {
	app := testApp(t)
	people := recipientTestPeople(t, app, 2)
	m := models.Message{Status: models.MessageSending, Category: models.MessageCategoryAnnouncement, Subject: "push"}
	if err := app.DB.Create(&m).Error; err != nil {
		t.Fatal(err)
	}
	rows := make([]models.MessageRecipient, len(people))
	for i, id := range people {
		rows[i] = models.MessageRecipient{MessageID: m.ID, UserID: id, Channel: models.DeliveryPush, Status: models.RecipientQueued}
		if err := app.DB.Create(&rows[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	var name string
	if err := app.DB.Raw("SELECT current_database()").Scan(&name).Error; err != nil {
		t.Fatal(err)
	}
	proxy := testdb.NewProxy(t)
	db, err := proxy.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	commits := func() int { return len(proxy.Find(testdb.KindCommit, "")) }

	before := commits()
	if err := messagePushRecorder(rows[0].ID)(db, models.RecipientSent, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := commits() - before; got != 1 {
		t.Errorf("a send's own outcome committed %d times, want its own transaction", got)
	}
	if r := recipientRowOf(t, app, rows[0].ID); r.Status != models.RecipientSent {
		t.Fatalf("the row is %s", r.Status)
	}

	before = commits()
	err = db.Transaction(func(tx *gorm.DB) error {
		if err := messagePushRecorder(rows[1].ID)(tx, models.RecipientSuppressed, pushWhyGone, time.Now()); err != nil {
			return err
		}
		if got := commits() - before; got != 0 {
			t.Errorf("inside a transaction the recorder committed %d times", got)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if r := recipientRowOf(t, app, rows[1].ID); r.Status != models.RecipientSuppressed {
		t.Fatalf("the row is %s", r.Status)
	}
}
