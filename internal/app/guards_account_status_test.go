package main

import (
	"reflect"
	"strings"
	"testing"

	"fold/internal/guardkit"
)

// users.status has one writer, setAccountStatusIn, which owns the move's
// session bump, seat work and token revoke (DEC-159, docs/agents/lifecycles.md).
// upgradeGuestIn turns a guest row into an account, and two statements keep the
// status in their own UPDATE: replacePasswordIn (with the password, through
// endAllSessionsIn) and the erasure (tombstoneUserIn, and scrubTombstoneRowSQL
// for the sweep).
//
// The guard checks changes to existing accounts and calls to endAllSessionsIn.
// newAccountStatus owns initial status.

var accountStatusWriterCensus = []string{
	"apiAdminUpdateUser: Updates of a map given \"status\"",
	"endAllSessionsIn: Updates of a map with computed keys",
	"replacePasswordIn: endAllSessionsIn with a map with \"status\"",
	"scrubTombstoneRowSQL: raw UPDATE users with a column list built at run time",
	"setAccountStatusIn: Updates of a parameter's map",
	"tombstoneUserIn: Updates of a map with computed keys",
	"upgradeGuestIn: Updates of a user struct",
}

var accountStatusGuard = func() tableStatusGuard {
	g := newTableStatusGuard("User", "users", "a user")
	g.skipInserts = true
	g.updateHelper = "endAllSessionsIn"
	return g
}()

func TestAccountStatusHasOneWriter(t *testing.T) {
	parsed := guardkit.Production(t).Parse(0)
	for _, p := range parsed.Problems {
		t.Error(p)
	}
	writes := accountStatusGuard.writes(parsed)
	if got := tableStatusCensus(writes); !reflect.DeepEqual(got, accountStatusWriterCensus) {
		var where []string
		for _, w := range writes {
			where = append(where, w.at+" "+w.fn+": "+w.shape)
		}
		t.Errorf("the writes of users.status are\n%s\nwant exactly %v; change an account's status with setAccountStatusIn", strings.Join(where, "\n"), accountStatusWriterCensus)
	}
}

func TestAccountStatusGuardCatchesBypasses(t *testing.T) {
	const decls = `
func lockUsersByID(tx *gorm.DB, ids []uint, all bool) (map[uint]*models.User, error) { return nil, nil }
func loadUser(tx *gorm.DB, id uint) (*models.User, error) { return nil, nil }
func lockConsentUsers(tx *gorm.DB, actorID, teenID uint) (*models.User, *models.User, error) { return nil, nil, nil }
`
	for _, c := range []struct{ name, src, want string }{
		{"a bare Update, as the teen conversion did", `package main
func (a *App) wait(tx *gorm.DB, user *models.User) error {
	return tx.Model(user).Update("status", models.UserPendingConsent).Error
}`, `wait: Update("status")`},
		{"a local map, as the age rollover did", `package main
func (a *App) roll(tx *gorm.DB, user *models.User, status string) error {
	updates := map[string]any{"status": status}
	updates["session_version"] = sessionVersionBump()
	return tx.Model(user).Updates(updates).Error
}`, `roll: Updates of a map given "status"`},
		{"a pointer to a local map", `package main
func (a *App) roll(tx *gorm.DB, u *models.User) error {
	cols := map[string]any{"Status": models.UserActive}
	return tx.Model(u).Updates(&cols).Error
}`, `roll: Updates of a map given "status"`},
		{"a map given status later", `package main
func (a *App) roll(tx *gorm.DB, id uint) error {
	var u models.User
	updates := map[string]any{}
	updates["status"] = models.UserActive
	return tx.Model(&u).Where("id = ?", id).Updates(updates).Error
}`, `roll: Updates of a map given "status"`},
		{"a locked user from a map", `package main
func (a *App) roll(tx *gorm.DB, id uint) error {
	users, _ := lockUsersByID(tx, []uint{id}, false)
	user := users[id]
	return tx.Model(user).UpdateColumn("status", models.UserActive).Error
}` + decls, `roll: UpdateColumn("status")`},
		{"a loaded user in a callback", `package main
func (a *App) roll(id uint) error {
	return a.DB.Transaction(func(tx *gorm.DB) error {
		u, err := loadUser(tx, id)
		if err != nil { return err }
		return tx.Model(u).Updates(map[string]any{"status": models.UserActive}).Error
	})
}` + decls, `roll: Updates of a map with "status"`},
		{"a range over users", `package main
func (a *App) roll(tx *gorm.DB, users []models.User) error {
	for _, u := range users {
		if err := tx.Model(&u).Update("status", models.UserActive).Error; err != nil { return err }
	}
	return nil
}`, `roll: Update("status")`},
		{"the table by name", `package main
func (a *App) roll(tx *gorm.DB, id uint) error {
	return tx.Table("users").Where("id = ?", id).Update("status", models.UserActive).Error
}`, `roll: Update("status")`},
		{"a model literal", `package main
func (a *App) roll(tx *gorm.DB, id uint) error {
	return tx.Model(&models.User{}).Where("id = ?", id).Updates(map[string]any{"status": models.UserActive}).Error
}`, `roll: Updates of a map with "status"`},
		{"a saved row", `package main
func (a *App) roll(tx *gorm.DB, u *models.User) error {
	u.Status = models.UserActive
	return tx.Save(u).Error
}`, "roll: Save of a user"},
		{"raw SQL", `package main
func (a *App) roll(tx *gorm.DB, id uint) error {
	return tx.Exec("UPDATE users SET name = name, status = 'active' WHERE id = ?", id).Error
}`, "roll: raw UPDATE of users.status"},
		{"raw SQL in a constant", `package main
const activateSQL = ` + "`UPDATE users AS u SET \"status\" = 'active' WHERE u.id = ?`" + `
`, "activateSQL: raw UPDATE of users.status"},
		{"a session end that also sets status", `package main
func (a *App) roll(tx *gorm.DB, u *models.User) error {
	return endAllSessionsIn(tx, u, map[string]any{"status": models.UserActive})
}`, `roll: endAllSessionsIn with a map with "status"`},
		{"a column named at run time", `package main
func (a *App) roll(tx *gorm.DB, u *models.User, col string) error {
	return tx.Model(u).Update(col, models.UserActive).Error
}`, "roll: Update of a computed column"},
		{"a helper that writes its caller's columns", `package main
func writeUser(tx *gorm.DB, u *models.User, cols map[string]any) error {
	return tx.Model(u).Updates(cols).Error
}`, "writeUser: Updates of a parameter's map"},
		{"a computed key in an inline map", `package main
func (a *App) roll(tx *gorm.DB, user *models.User, key string, value any) error {
	return tx.Model(user).Updates(map[string]any{key: value}).Error
}`, "roll: Updates of a map with computed keys"},
		{"a constant key in an inline map", `package main
const statusColumn = "status"
func (a *App) roll(tx *gorm.DB, id uint) error {
	return tx.Model(&models.User{}).Where("id = ?", id).UpdateColumns(map[string]any{"name": "x", statusColumn: models.UserActive}).Error
}`, "roll: UpdateColumns of a map with computed keys"},
		{"a computed key in a local map literal", `package main
func (a *App) roll(tx *gorm.DB, user *models.User, key string) error {
	cols := map[string]any{key: models.UserActive}
	return tx.Model(user).Updates(cols).Error
}`, "roll: Updates of a map with computed keys"},
		{"a computed key in a var map literal", `package main
func (a *App) roll(tx *gorm.DB, user *models.User, key string) error {
	var cols = map[string]any{key: models.UserActive}
	return tx.Model(user).Updates(cols).Error
}`, "roll: Updates of a map with computed keys"},
		{"a session end with a computed key", `package main
func (a *App) roll(tx *gorm.DB, u *models.User, key string) error {
	return endAllSessionsIn(tx, u, map[string]any{key: models.UserActive})
}`, "roll: endAllSessionsIn with a map with computed keys"},
		{"every way a function names a users value", `package main
func inferred(tx *gorm.DB, id uint, key string, value any) error {
	var u = models.User{ID: id}
	return tx.Model(&u).Updates(map[string]any{key: value}).Error
}
func inferredPointer(tx *gorm.DB, id uint) error {
	var u = &models.User{ID: id}
	return tx.Model(u).Update("status", models.UserActive).Error
}
func built(tx *gorm.DB) error {
	u := new(models.User)
	var v = new(models.User)
	tx.Model(v).Update("status", models.UserActive)
	return tx.Model(u).UpdateColumn("status", models.UserActive).Error
}
func typed(tx *gorm.DB) error {
	var u models.User
	var v *models.User
	tx.Model(v).Update("status", models.UserActive)
	return tx.Model(&u).Update("status", models.UserActive).Error
}
func loaded(tx *gorm.DB, id uint) error {
	var u, err = loadUser(tx, id)
	if err != nil { return err }
	return tx.Model(u).Update("status", models.UserActive).Error
}
func second(tx *gorm.DB, me, id uint) error {
	actor, teen, err := lockConsentUsers(tx, me, id)
	if err != nil || actor == nil { return err }
	return tx.Model(teen).Update("status", models.UserActive).Error
}
func named(tx *gorm.DB) (u *models.User, err error) {
	return u, tx.Model(u).Update("status", models.UserActive).Error
}
func asserted(tx *gorm.DB, v any) error {
	u, ok := v.(*models.User)
	if !ok { return nil }
	return tx.Model(u).Update("status", models.UserActive).Error
}` + decls, strings.Join([]string{
			`asserted: Update("status")`, `built: Update("status")`, `built: UpdateColumn("status")`,
			"inferred: Updates of a map with computed keys", `inferredPointer: Update("status")`,
			`loaded: Update("status")`, `named: Update("status")`, `second: Update("status")`,
			`typed: Update("status")`, `typed: Update("status")`}, "\n")},
		{"collections of users", `package main
func made(tx *gorm.DB) error {
	var us = []models.User{}
	vs := make([]models.User, 0)
	for _, v := range vs { tx.Model(&v).Update("status", models.UserActive) }
	for _, u := range us { tx.Model(&u).Update("status", models.UserActive) }
	return nil
}
func batch(tx *gorm.DB, users []models.User) error {
	tx.Save(&users)
	return tx.Model(&users).Update("status", models.UserActive).Error
}`, strings.Join([]string{`batch: Save of a user`, `batch: Update("status")`, `made: Update("status")`, `made: Update("status")`}, "\n")},
		{"a saved row from an inferred var", `package main
func roll(tx *gorm.DB, id uint) error {
	var u = models.User{ID: id}
	u.Status = models.UserActive
	return tx.Save(&u).Error
}`, "roll: Save of a user"},
		{"the Go field name and every spelling GORM resolves", `package main
func roll(tx *gorm.DB, u *models.User) error {
	tx.Model(u).Updates(map[string]any{"Status": models.UserActive})
	tx.Model(u).Update("Status", models.UserActive)
	tx.Model(u).UpdateColumn("STATUS", models.UserActive)
	cols := map[string]any{}
	cols["Status"] = models.UserActive
	return tx.Model(u).Updates(cols).Error
}`, strings.Join([]string{`roll: Update("status")`, `roll: UpdateColumn("status")`, `roll: Updates of a map given "status"`, `roll: Updates of a map with "status"`}, "\n")},
		{"a users struct, whatever Select names", `package main
func roll(tx *gorm.DB, u *models.User) error {
	tx.Model(u).Select("Status").Updates(u)
	tx.Model(u).Select("status").Updates(models.User{Status: models.UserActive})
	return tx.Model(&models.User{}).Where("id = ?", u.ID).Updates(&models.User{Status: models.UserActive}).Error
}`, strings.Join([]string{"roll: Updates of a user struct", "roll: Updates of a user struct", "roll: Updates of a user struct"}, "\n")},
		{"a struct of another type", `package main
func roll(tx *gorm.DB, u *models.User) error {
	return tx.Model(u).Updates(struct{ Status string }{models.UserActive}).Error
}`, "roll: Updates of a value it cannot read"},
		{"a query held in a name", `package main
func roll(tx *gorm.DB, id uint) error {
	q := tx.Model(&models.User{}).Where("id = ?", id)
	var r = q.Where("deleted_at IS NULL")
	r.Update("status", models.UserActive)
	return tx.Table("public.users").Where("id = ?", id).Update("status", models.UserActive).Error
}`, strings.Join([]string{`roll: Update("status")`, `roll: Update("status")`}, "\n")},
		{"a type switch", `package main
func roll(tx *gorm.DB, v any) error {
	switch u := v.(type) {
	case *models.User:
		return tx.Model(u).Update("status", models.UserActive).Error
	}
	return nil
}`, `roll: Update("status")`},
		{"names that do not hold a users row or write its status", `package main
func photo(tx *gorm.DB, id uint) error {
	var u = models.User{ID: id}
	var g = models.Group{ID: id}
	tx.Model(&g).Update("Status", models.GroupApproved)
	q := tx.Model(&models.Membership{}).Where("user_id = ?", id)
	q.Update("status", models.MembershipLeft)
	var n int64
	tx.Model(&models.User{}).Where("status = ?", models.UserActive).Count(&n)
	tx.Model(&u).Updates(map[string]any{"statuses": "x", "status_note": "y", "sTatus": "z"})
	return tx.Model(&u).Update("Statuses", "x").Error
}`, ""},
		{"literal keys that are not status", `package main
func (a *App) photo(tx *gorm.DB, u *models.User) error {
	cols := map[string]any{"avatar_key": ""}
	tx.Model(u).Updates(cols)
	return tx.Model(u).Updates(map[string]any{"name": "x"}).Error
}`, ""},
		{"another table's status", `package main
func (a *App) leave(tx *gorm.DB, m *models.Membership) error {
	tx.Model(m).Update("status", models.MembershipLeft)
	return tx.Model(&models.Registration{}).Updates(map[string]any{"status": "cancelled"}).Error
}`, ""},
		{"another users column", `package main
func (a *App) photo(tx *gorm.DB, u *models.User) error {
	tx.Exec("UPDATE users SET avatar_key = '' WHERE status = 'active'")
	return tx.Model(u).Update("avatar_key", "").Error
}`, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := guardkit.Sources{"x.go": c.src}.Parse(0)
			if len(p.Problems) > 0 {
				t.Fatal(p.Problems)
			}
			got := strings.Join(tableStatusCensus(accountStatusGuard.writes(p)), "\n")
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
