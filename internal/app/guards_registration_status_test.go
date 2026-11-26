package app

import (
	"reflect"
	"strings"
	"testing"

	"fold/internal/guardkit"
)

// registrations.status has two writers: saveRegistration (with
// saveRegistrationStep, through writeRegistration), which checks the caller's
// policy.RegistrationMove, and promoteRegistration, whose WHERE is the
// promotion's from-set (docs/agents/lifecycles.md). An insert is a Save of a
// row with no id, so there is no other creator.
//
// The census follows direct fields of named structs through local assignments.

var registrationStatusWriterCensus = []string{
	`promoteRegistration: Updates of a map with "status"`,
	"writeRegistration: Save of a registration",
}

var registrationStatusGuard = newTableStatusGuard("Registration", "registrations", "a registration")

func TestRegistrationStatusHasTwoWriters(t *testing.T) {
	parsed := guardkit.Production(t).Parse(0)
	for _, p := range parsed.Problems {
		t.Error(p)
	}
	writes := registrationStatusGuard.writes(parsed)
	if got := tableStatusCensus(writes); !reflect.DeepEqual(got, registrationStatusWriterCensus) {
		var where []string
		for _, w := range writes {
			where = append(where, w.at+" "+w.fn+": "+w.shape)
		}
		t.Errorf("the writes of registrations.status are\n%s\nwant exactly %v; change a registration's status with saveRegistration and a policy.RegistrationMove", strings.Join(where, "\n"), registrationStatusWriterCensus)
	}
}

func TestRegistrationStatusGuardCatchesBypasses(t *testing.T) {
	const decls = `
func loadReg(tx *gorm.DB, id uint) (*models.Registration, error) { return nil, nil }
func cancelColumns() map[string]any { return nil }
`
	for _, c := range []struct{ name, src, want string }{
		{"a registration change field", `package app
type registrationChange struct { next models.Registration }
type otherChange struct { next models.Membership }
func write(tx *gorm.DB, changes []registrationChange, other otherChange) error {
	ch := &changes[0]
	tx.Save(&other.next)
	return tx.Save(&ch.next).Error
}`, "write: Save of a registration"},
		{"a save through registerLocked's local variable", `package app
type registrationChange struct { next models.Registration }
type otherChange struct { next models.Membership }
func (a *App) registerLocked(tx *gorm.DB, ch *registrationChange, other *otherChange) error {
	otherRow := &other.next
	tx.Save(otherRow)
	r := &ch.next
	return tx.Save(r).Error
}`, "registerLocked: Save of a registration"},
		{"a new batch given inline", `package app
func create(tx *gorm.DB) error {
	return tx.Create(&[]models.Registration{{}}).Error
}`, "create: Create of a registration"},
		{"a cancel written beside the writer", `package app
func (a *App) cancel(tx *gorm.DB, r *models.Registration) error {
	return tx.Model(r).Update("status", models.RegCancelled).Error
}`, `cancel: Update("status")`},
		{"the field's name, as gorm resolves it", `package app
func (a *App) cancel(tx *gorm.DB, r *models.Registration) error {
	tx.Model(r).UpdateColumn("Status", models.RegCancelled)
	return tx.Model(r).Updates(map[string]any{"Status": models.RegCancelled}).Error
}`, strings.Join([]string{`cancel: UpdateColumn("status")`, `cancel: Updates of a map with "status"`}, "\n")},
		{"a conditional update on the table", `package app
func (a *App) promote(tx *gorm.DB, id uint) error {
	return tx.Model(&models.Registration{}).Where("id = ? AND status = ?", id, models.RegWaitlisted).
		Updates(map[string]any{"status": models.RegGoing, "promoted_at": time.Now()}).Error
}`, `promote: Updates of a map with "status"`},
		{"a map given status later", `package app
func (a *App) cancel(tx *gorm.DB, id uint) error {
	cols := map[string]any{"removal_note": ""}
	cols["status"] = models.RegCancelled
	return tx.Model(&models.Registration{}).Where("id = ?", id).Updates(cols).Error
}`, `cancel: Updates of a map given "status"`},
		{"a pointer to a local map", `package app
func (a *App) cancel(tx *gorm.DB, r *models.Registration) error {
	cols := map[string]any{"status": models.RegCancelled}
	return tx.Model(r).Updates(&cols).Error
}`, `cancel: Updates of a map given "status"`},
		{"a pointer to a map literal", `package app
func (a *App) cancel(tx *gorm.DB, r *models.Registration) error {
	return tx.Model(r).Updates(&map[string]any{"status": models.RegCancelled}).Error
}`, `cancel: Updates of a map with "status"`},
		{"a map with a computed key", `package app
func (a *App) set(tx *gorm.DB, r *models.Registration, col string) error {
	return tx.Model(r).Updates(map[string]any{col: models.RegGoing}).Error
}`, "set: Updates of a map with computed keys"},
		{"a map with a quoted key", `package app
func (a *App) set(tx *gorm.DB, r *models.Registration) error {
	return tx.Model(r).Updates(map[string]any{"\"status\"": models.RegGoing}).Error
}`, "set: Updates of a map with computed keys"},
		{"a constant key", `package app
const statusColumn = "status"
func (a *App) set(tx *gorm.DB, r *models.Registration) error {
	return tx.Model(r).UpdateColumns(map[string]any{statusColumn: models.RegGoing}).Error
}`, "set: UpdateColumns of a map with computed keys"},
		{"a map handed to another call", `package app
func (a *App) set(tx *gorm.DB, r *models.Registration, extra map[string]any) error {
	cols := map[string]any{"removal_note": ""}
	maps.Copy(cols, extra)
	return tx.Model(r).Updates(cols).Error
}`, "set: Updates of a map with computed keys"},
		{"a map under a second name", `package app
func (a *App) set(tx *gorm.DB, r *models.Registration) error {
	cols := map[string]any{"removal_note": ""}
	more := cols
	more["status"] = models.RegGoing
	return tx.Model(r).Updates(cols).Error
}`, "set: Updates of a map with computed keys"},
		{"a pointer to a map under a second name", `package app
func (a *App) set(tx *gorm.DB, r *models.Registration) error {
	cols := map[string]any{"removal_note": ""}
	p := &cols
	(*p)["status"] = models.RegGoing
	return tx.Model(r).Updates(p).Error
}`, strings.Join([]string{"set: Updates of a map with computed keys"}, "\n")},
		{"a map built elsewhere", `package app
func (a *App) set(tx *gorm.DB, r *models.Registration) error {
	cols := cancelColumns()
	return tx.Model(r).Updates(cols).Error
}` + decls, "set: Updates of a map with computed keys"},
		{"a helper that writes its caller's columns", `package app
func writeReg(tx *gorm.DB, id uint, cols map[string]any) error {
	return tx.Model(&models.Registration{}).Where("id = ?", id).Updates(cols).Error
}`, "writeReg: Updates of a parameter's map"},
		{"a column named at run time", `package app
func (a *App) set(tx *gorm.DB, r *models.Registration, col string) error {
	return tx.Model(r).Update(col, models.RegGoing).Error
}`, "set: Update of a computed column"},
		{"a registration struct, whatever Select names", `package app
func (a *App) set(tx *gorm.DB, id uint) error {
	tx.Model(&models.Registration{}).Where("id = ?", id).Select("status").Updates(models.Registration{Status: models.RegGoing})
	return tx.Where("id = ?", id).Updates(&models.Registration{Status: models.RegGoing}).Error
}`, strings.Join([]string{"set: Updates of a registration struct", "set: Updates of a registration struct"}, "\n")},
		{"a struct of another type", `package app
func (a *App) set(tx *gorm.DB, r *models.Registration) error {
	return tx.Model(r).Updates(struct{ Status string }{models.RegGoing}).Error
}`, "set: Updates of a value it cannot read"},
		{"a saved row", `package app
func (a *App) set(tx *gorm.DB, r *models.Registration) error {
	r.Status = models.RegGoing
	return tx.Omit("access_origin").Save(r).Error
}`, "set: Save of a registration"},
		{"a new row and a batch of them", `package app
func (a *App) add(tx *gorm.DB, uid, oid uint) error {
	r := models.Registration{UserID: &uid, OccurrenceID: oid}
	tx.Create(&r)
	rows := []models.Registration{r}
	tx.CreateInBatches(&rows, 10)
	return tx.FirstOrCreate(&models.Registration{}, "user_id = ?", uid).Error
}`, strings.Join([]string{"add: Create of a registration", "add: Create of a registration", "add: Create of a registration"}, "\n")},
		{"a loaded row, a range and a query held in a name", `package app
func (a *App) set(tx *gorm.DB, id uint, rows []models.Registration) error {
	r, _ := loadReg(tx, id)
	tx.Model(r).Update("status", models.RegGoing)
	for _, row := range rows {
		tx.Model(&row).Update("status", models.RegGoing)
	}
	q := tx.Model(&models.Registration{}).Where("id = ?", id)
	q.Update("status", models.RegGoing)
	return tx.Table("registrations").Where("id = ?", id).Update("status", models.RegGoing).Error
}` + decls, strings.Join([]string{`set: Update("status")`, `set: Update("status")`, `set: Update("status")`, `set: Update("status")`}, "\n")},
		{"raw SQL", `package app
func (a *App) set(tx *gorm.DB, id uint) error {
	return tx.Exec("UPDATE registrations SET removal_note = '', status = 'cancelled' WHERE id = ?", id).Error
}`, "set: raw UPDATE of registrations.status"},
		{"raw SQL in a constant with an alias", `package app
const cancelSQL = ` + "`UPDATE registrations AS r SET \"status\" = 'cancelled' WHERE r.id = ?`" + `
`, "cancelSQL: raw UPDATE of registrations.status"},
		{"raw SQL whose SET list is built at run time", `package app
func (a *App) set(tx *gorm.DB, cols string) error {
	return tx.Exec("UPDATE registrations SET removal_note = '', " + cols + " WHERE id = 1").Error
}`, "set: raw UPDATE registrations with a column list built at run time"},
		{"a raw insert", `package app
func (a *App) add(tx *gorm.DB) error {
	return tx.Exec("INSERT INTO registrations (occurrence_id, user_id) VALUES (1, 1)").Error
}`, "add: raw INSERT INTO registrations"},
		{"other columns and other tables", `package app
func (a *App) scrub(tx *gorm.DB, r *models.Registration, m *models.Membership, id uint) error {
	tx.Exec("UPDATE registrations SET manage_token_hash = '' WHERE status = 'going'")
	tx.Exec("UPDATE registration_notes SET status = 'x' WHERE id = 1")
	tx.Model(r).Update("manage_token_hash", "")
	tx.Model(&models.Registration{}).Where("id = ?", id).UpdateColumns(map[string]any{"user_id": id, "household_person_id": nil})
	cols := map[string]any{"access_ended_at": nil}
	cols["access_end_reason"] = ""
	tx.Model(r).UpdateColumns(&cols)
	tx.Model(m).Update("status", models.MembershipLeft)
	tx.Save(m)
	var n int64
	tx.Model(&models.Registration{}).Where("status = ?", models.RegGoing).Count(&n)
	return tx.Where("id = ?", id).Delete(&models.Registration{}).Error
}`, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := guardkit.Sources{"internal/app/x.go": c.src}.Parse(0)
			if len(p.Problems) > 0 {
				t.Fatal(p.Problems)
			}
			got := strings.Join(tableStatusCensus(registrationStatusGuard.writes(p)), "\n")
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
