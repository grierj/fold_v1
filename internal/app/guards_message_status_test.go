package app

import (
	"reflect"
	"strings"
	"testing"

	"fold/internal/guardkit"
)

// ---------------------------------------------------------------------------
// messages.status has one writer of an existing row's status, moveMessage
// (moveMessagesWhere for a set), whose WHERE is the move's from-set in
// policy.MessageMove; three creators insert a move's first status
// (docs/agents/lifecycles.md).
//
// The shared tableStatusGuard describes the source forms in the census.
// ---------------------------------------------------------------------------

// messageStatusWriterCensus is every write of messages.status the guard sees.
var messageStatusWriterCensus = []string{
	"apiCreateMessage: Create of a message",
	"moveMessage: Updates of a map given \"status\"",
	"moveMessagesWhere: raw UPDATE of messages.status",
	"writePushReminder: Create of a message",
	"writeReminder: Create of a message",
}

var messageStatusGuard = newTableStatusGuard("Message", "messages", "a message")

func TestMessageStatusHasOneWriter(t *testing.T) {
	parsed := guardkit.Production(t).Parse(0)
	for _, p := range parsed.Problems {
		t.Error(p)
	}
	writes := messageStatusGuard.writes(parsed)
	if got := tableStatusCensus(writes); !reflect.DeepEqual(got, messageStatusWriterCensus) {
		var where []string
		for _, w := range writes {
			where = append(where, w.at+" "+w.fn+": "+w.shape)
		}
		t.Errorf("the writes of messages.status are\n%s\nwant exactly %v; change a message's status with moveMessage and a policy.MessageMove", strings.Join(where, "\n"), messageStatusWriterCensus)
	}
}

func TestMessageStatusGuardCatchesBypasses(t *testing.T) {
	const decls = `
func loadOwn(tx *gorm.DB, id uint) (*models.Message, bool) { return nil, false }
`
	for _, c := range []struct{ name, src, want string }{
		{"a schema-qualified table", `package app
func retry(tx *gorm.DB) error {
	return tx.Table("public.messages").Update("status", "scheduled").Error
}`, `retry: Update("status")`},
		{"a cancel as it was written", `package app
func (a *App) cancel(tx *gorm.DB, m *models.Message) error {
	return tx.Model(&models.Message{}).Where("id = ? AND status IN ?", m.ID, []string{"draft", "scheduled"}).
		Updates(map[string]any{"status": models.MessageCancelled, "updated_at": time.Now()}).Error
}`, `cancel: Updates of a map with "status"`},
		{"a query held in a variable, as the refusal was", `package app
func (a *App) refuse(tx *gorm.DB, id uint) error {
	q := tx.Model(&models.Message{}).Where("id = ?", id)
	q = q.Where("updated_at = ?", time.Now())
	return q.Updates(map[string]any{"status": models.MessageFailed}).Error
}`, `refuse: Updates of a map with "status"`},
		{"a map given status later, as the retry's was", `package app
func (a *App) retry(tx *gorm.DB, id uint) error {
	fields := map[string]any{"error": ""}
	fields["status"] = models.MessageSending
	return tx.Model(&models.Message{}).Where("id = ?", id).Updates(fields).Error
}`, `retry: Updates of a map given "status"`},
		{"a bare Update on a loaded message", `package app
func (a *App) send(tx *gorm.DB, id uint) error {
	m, _ := loadOwn(tx, id)
	return tx.Model(m).Update("status", models.MessageSent).Error
}` + decls, `send: Update("status")`},
		{"a range over messages", `package app
func (a *App) fail(tx *gorm.DB) error {
	var stuck []models.Message
	for _, m := range stuck {
		tx.Model(&m).UpdateColumn("status", models.MessageFailed)
	}
	return nil
}`, `fail: UpdateColumn("status")`},
		{"the table by name", `package app
func (a *App) fail(tx *gorm.DB, id uint) error {
	return tx.Table("messages").Where("id = ?", id).Update("status", "failed").Error
}`, `fail: Update("status")`},
		{"a column named at run time", `package app
func (a *App) set(tx *gorm.DB, m *models.Message, col string) error {
	return tx.Model(m).Update(col, "sent").Error
}`, "set: Update of a computed column"},
		{"a helper that writes its caller's columns", `package app
func writeMessage(tx *gorm.DB, id uint, cols map[string]any) error {
	return tx.Model(&models.Message{}).Where("id = ?", id).Updates(cols).Error
}`, "writeMessage: Updates of a parameter's map"},
		{"a saved message", `package app
func (a *App) send(tx *gorm.DB, m *models.Message) error {
	m.Status = models.MessageSent
	return tx.Save(m).Error
}`, "send: Save of a message"},
		{"a message struct as the update", `package app
func (a *App) send(tx *gorm.DB, id uint) error {
	return tx.Model(&models.Message{}).Where("id = ?", id).Updates(models.Message{Status: "sent"}).Error
}`, "send: Updates of a message struct"},
		{"a new message", `package app
func (a *App) draft(tx *gorm.DB) error {
	m := models.Message{Subject: "Hi"}
	return tx.Create(&m).Error
}`, "draft: Create of a message"},
		{"raw SQL", `package app
func (a *App) fail(tx *gorm.DB, id uint) error {
	return tx.Exec("UPDATE messages SET error = '', status = 'failed' WHERE id = ?", id).Error
}`, "fail: raw UPDATE of messages.status"},
		{"raw SQL in a constant", `package app
const failSQL = ` + "`UPDATE messages AS m SET \"status\" = 'failed' WHERE m.id = ?`" + `
`, "failSQL: raw UPDATE of messages.status"},
		{"raw SQL with its columns built at run time", `package app
func (a *App) set(tx *gorm.DB, cols string) error {
	return tx.Exec("UPDATE messages SET " + cols + " WHERE id = 1").Error
}`, "set: raw UPDATE messages with a column list built at run time"},
		{"raw SQL that names its status column at run time", `package app
func (a *App) set(tx *gorm.DB, col string) error {
	return tx.Exec("UPDATE messages SET error = '', " + col + " = 'sent' WHERE id = 1").Error
}`, "set: raw UPDATE messages with a column list built at run time"},
		{"a map whose key is computed", `package app
func (a *App) set(tx *gorm.DB, m *models.Message, col string) error {
	return tx.Model(m).Updates(map[string]any{col: "sent"}).Error
}`, "set: Updates of a map with computed keys"},
		{"a local map whose key is computed", `package app
func (a *App) set(tx *gorm.DB, m *models.Message, col string) error {
	cols := map[string]any{"error": "", col: "sent"}
	return tx.Model(m).Updates(cols).Error
}`, "set: Updates of a map with computed keys"},
		{"the field's name as the key", `package app
func (a *App) send(tx *gorm.DB, m *models.Message) error {
	return tx.Model(m).Updates(map[string]any{"Status": "sent"}).Error
}`, `send: Updates of a map with "status"`},
		{"the field's name as the column", `package app
func (a *App) send(tx *gorm.DB, m *models.Message) error {
	return tx.Model(m).Update("Status", "sent").Error
}`, `send: Update("status")`},
		{"a message made with new", `package app
func (a *App) send(tx *gorm.DB) error {
	m := new(models.Message)
	return tx.Model(m).Update("status", "sent").Error
}`, `send: Update("status")`},
		{"a selected field and a struct that is not a message", `package app
func (a *App) send(tx *gorm.DB, m *models.Message) error {
	return tx.Model(m).Select("Status").Updates(struct{ Status string }{"sent"}).Error
}`, "send: Updates of a value it cannot read"},
		{"a local map filled by another call", `package app
func (a *App) send(tx *gorm.DB, m *models.Message, cols map[string]any) error {
	fields := map[string]any{"error": ""}
	maps.Copy(fields, cols)
	return tx.Model(m).Updates(fields).Error
}`, "send: Updates of a map with computed keys"},
		{"a shadowing name bound to a call's map", `package app
func (a *App) send(tx *gorm.DB, m *models.Message, late bool) error {
	fields := map[string]any{"error": ""}
	if late {
		var fields = sentColumns()
		return tx.Model(m).Updates(fields).Error
	}
	return nil
}`, "send: Updates of a map with computed keys"},
		{"a map assigned from a call's second result", `package app
func (a *App) send(tx *gorm.DB, m *models.Message) error {
	fields := map[string]any{"error": ""}
	_, fields = sentColumns()
	return tx.Model(m).Updates(fields).Error
}`, "send: Updates of a map with computed keys"},
		{"a local map under a second name", `package app
func (a *App) send(tx *gorm.DB, m *models.Message) error {
	fields := map[string]any{"error": ""}
	extra := fields
	extra["status"] = "sent"
	return tx.Model(m).Updates(fields).Error
}`, "send: Updates of a map with computed keys"},
		{"a quoted identifier as the key", `package app
func (a *App) send(tx *gorm.DB, m *models.Message) error {
	return tx.Model(m).Updates(map[string]any{"\"status\"": "sent"}).Error
}`, "send: Updates of a map with computed keys"},
		{"a map built elsewhere", `package app
func (a *App) send(tx *gorm.DB, m *models.Message) error {
	cols := sentColumns()
	return tx.Model(m).Updates(cols).Error
}`, "send: Updates of a map with computed keys"},
		{"a raw insert", `package app
func (a *App) draft(tx *gorm.DB) error {
	return tx.Exec("INSERT INTO messages (subject) VALUES ('Hi')").Error
}`, "draft: raw INSERT INTO messages"},
		{"messages made with make", `package app
func (a *App) drafts(tx *gorm.DB) error {
	ms := make([]models.Message, 0, 2)
	return tx.CreateInBatches(&ms, 50).Error
}`, "drafts: Create of a message"},
		{"another table's status", `package app
func (a *App) mark(tx *gorm.DB, m *models.Membership, id uint) error {
	tx.Model(&m).Updates(map[string]any{"status": "active"})
	tx.Model(&models.MessageRecipient{}).Where("message_id = ?", id).Updates(map[string]any{"status": "failed"})
	tx.Exec("UPDATE message_recipients SET status = 'suppressed' WHERE message_id = ?", id)
	return tx.Create(&models.MessageRecipient{MessageID: id}).Error
}`, ""},
		{"another messages column", `package app
func (a *App) count(tx *gorm.DB, m *models.Message) error {
	tx.Exec("UPDATE messages SET recipient_count = 0 WHERE status = 'sending'")
	cols := map[string]any{"error": ""}
	cols["recipient_count"] = 0
	tx.Model(m).Updates(cols)
	tx.Model(m).Updates(map[string]any{"error": "", "recipient_count": 0})
	return tx.Model(m).Where("status = ?", "sending").Update("recipient_count", 0).Error
}`, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := guardkit.Sources{"internal/app/x.go": c.src}.Parse(0)
			if len(p.Problems) > 0 {
				t.Fatal(p.Problems)
			}
			got := strings.Join(tableStatusCensus(messageStatusGuard.writes(p)), "\n")
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
