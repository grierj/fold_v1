package app

import (
	"reflect"
	"strings"
	"testing"

	"fold/internal/guardkit"
)

// ---------------------------------------------------------------------------
// message_recipients.status has one writer of an existing row's status,
// moveRecipientsWhere (moveRecipient for one row), whose WHERE is the move's
// from-set and channels in policy.RecipientMove; two creators insert rows in
// a creating move's status; and the erasure's scrub (eraseMessagesIn) stays
// its own statement, building its statuses from the rule, because it also
// scrubs rows outside the move's from-set, on every channel, in the one
// statement its timeout bounds (docs/agents/lifecycles.md).
//
// The shared tableStatusGuard describes the source forms in the census.
// ---------------------------------------------------------------------------

// recipientStatusWriterCensus is every write of message_recipients.status the
// guard sees.
var recipientStatusWriterCensus = []string{
	// Left in place: see the header.
	"eraseMessagesIn: raw UPDATE of message_recipients.status",
	"moveRecipientsWhere: raw UPDATE of message_recipients.status",
	"prepareMessageOnce: Create of a recipient",
	"writePushReminderPlans: Create of a recipient",
}

var recipientStatusGuard = newTableStatusGuard("MessageRecipient", "message_recipients", "a recipient")

func TestRecipientStatusHasOneWriter(t *testing.T) {
	parsed := guardkit.Production(t).Parse(0)
	for _, p := range parsed.Problems {
		t.Error(p)
	}
	writes := recipientStatusGuard.writes(parsed)
	if got := tableStatusCensus(writes); !reflect.DeepEqual(got, recipientStatusWriterCensus) {
		var where []string
		for _, w := range writes {
			where = append(where, w.at+" "+w.fn+": "+w.shape)
		}
		t.Errorf("the writes of message_recipients.status are\n%s\nwant exactly %v; change a recipient's status with moveRecipient and a policy.RecipientMove", strings.Join(where, "\n"), recipientStatusWriterCensus)
	}
}

func TestRecipientStatusGuardCatchesBypasses(t *testing.T) {
	for _, c := range []struct{ name, src, want string }{
		{"the email send's mark as it was written", `package app
func markRecipient(tx *gorm.DB, id uint, status, reason string) error {
	updates := map[string]any{"status": status, "reason": reason, "updated_at": time.Now()}
	if status == models.RecipientSent {
		updates["sent_at"] = time.Now()
	}
	return tx.Model(&models.MessageRecipient{}).Where("id = ? AND status = ?", id, models.RecipientQueued).Updates(updates).Error
}`, `markRecipient: Updates of a map given "status"`},
		{"the batch's give-up and the reaper as they were written", `package app
func (a *App) giveUp(tx *gorm.DB, ids []uint, why string) error {
	return tx.Model(&models.MessageRecipient{}).Where("id IN ? AND status = ? AND channel = ?", ids, models.RecipientQueued, models.DeliveryEmail).
		Updates(map[string]any{"status": models.RecipientFailed, "error": why, "updated_at": time.Now()}).Error
}`, `giveUp: Updates of a map with "status"`},
		{"the webhook's chain held in a variable", `package app
func (a *App) report(tx *gorm.DB, id string) error {
	recipients := tx.Model(&models.MessageRecipient{}).Where("channel = ? AND provider_message_id = ?", "email", id)
	return recipients.Where("status = ?", models.RecipientSent).Updates(map[string]any{"status": models.RecipientDelivered, "updated_at": time.Now()}).Error
}`, `report: Updates of a map with "status"`},
		{"the retry's helper of its caller's columns", `package app
func (a *App) retry(tx *gorm.DB, ids []uint) error {
	mark := func(list []uint, fields map[string]any) error {
		return tx.Model(&models.MessageRecipient{}).Where("id IN ? AND status = ?", list, models.RecipientFailed).Updates(fields).Error
	}
	return mark(ids, map[string]any{"status": models.RecipientQueued})
}`, "retry: Updates of a parameter's map"},
		{"the push recorder's query held in a variable", `package app
func messagePushRecorder(id uint) func(db *gorm.DB, status string) error {
	return func(db *gorm.DB, status string) error {
		updates := map[string]any{"status": status, "updated_at": time.Now()}
		q := db.Model(&models.MessageRecipient{}).Where("id = ? AND channel = ?", id, "push")
		q = q.Where("status = ?", models.RecipientQueued)
		return q.Updates(updates).Error
	}
}`, `messagePushRecorder: Updates of a map given "status"`},
		{"a push sweep's raw SQL", `package app
func reap(db *gorm.DB) error {
	return db.Exec(` + "`UPDATE message_recipients r SET status = ?, reason = ?, updated_at = now()\n\t\tWHERE r.channel = 'push' AND r.status = 'queued'`" + `, "suppressed", "stopped").Error
}`, "reap: raw UPDATE of message_recipients.status"},
		{"a status set by CASE", `package app
func erase(tx *gorm.DB, id uint) error {
	return tx.Exec(` + "`UPDATE message_recipients SET address = '', status = CASE WHEN status = 'queued' THEN 'suppressed' ELSE status END WHERE user_id = ?`" + `, id).Error
}`, "erase: raw UPDATE of message_recipients.status"},
		{"raw SQL with its columns built at run time", `package app
func set(tx *gorm.DB, cols string) error {
	return tx.Exec("UPDATE message_recipients SET " + cols + " WHERE id = 1").Error
}`, "set: raw UPDATE message_recipients with a column list built at run time"},
		{"rows made with make", `package app
func plan(tx *gorm.DB, people []uint) error {
	rows := make([]models.MessageRecipient, 0, len(people))
	for _, p := range people {
		rows = append(rows, models.MessageRecipient{UserID: p})
	}
	return tx.Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(&rows, 50).Error
}`, "plan: Create of a recipient"},
		{"rows from a declared function", `package app
func rowsFor(ids []uint) []models.MessageRecipient { return nil }
func (a *App) prepare(tx *gorm.DB, ids []uint) error {
	var rows []models.MessageRecipient
	rows = rowsFor(ids)
	return tx.CreateInBatches(&rows, 50).Error
}`, "prepare: Create of a recipient"},
		{"a saved row", `package app
func send(tx *gorm.DB, r *models.MessageRecipient) error {
	r.Status = models.RecipientSent
	return tx.Save(r).Error
}`, "send: Save of a recipient"},
		{"the table by name and alias", `package app
func fail(tx *gorm.DB, id uint) error {
	return tx.Table("message_recipients r").Where("r.id = ?", id).Update("Status", "failed").Error
}`, `fail: Update("status")`},
		{"a raw insert", `package app
func add(tx *gorm.DB) error {
	return tx.Exec("INSERT INTO message_recipients (message_id, user_id) VALUES (1, 2)").Error
}`, "add: raw INSERT INTO message_recipients"},
		{"another table's status", `package app
func mark(tx *gorm.DB, m *models.Message, id uint) error {
	tx.Model(m).Updates(map[string]any{"status": "sent"})
	tx.Exec("UPDATE messages SET status = 'failed' WHERE id = ?", id)
	tx.Model(&models.NotificationDelivery{}).Where("id = ?", id).Update("status", "delivered")
	return tx.Create(&models.Message{}).Error
}`, ""},
		{"another recipient column", `package app
func admit(db *gorm.DB, id uint) error {
	db.Exec("UPDATE message_recipients SET address = '', error = '' WHERE user_id = ?", id)
	db.Model(&models.MessageRecipient{}).Where("id = ?", id).Updates(map[string]any{"error": "", "updated_at": time.Now()})
	return db.Model(&models.MessageRecipient{}).Where("id = ? AND audience_admitted_at IS NULL", id).Update("audience_admitted_at", time.Now()).Error
}`, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := guardkit.Sources{"internal/app/x.go": c.src}.Parse(0)
			if len(p.Problems) > 0 {
				t.Fatal(p.Problems)
			}
			got := strings.Join(tableStatusCensus(recipientStatusGuard.writes(p)), "\n")
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
