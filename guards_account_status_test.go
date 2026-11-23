package main

import (
	"go/ast"
	"go/token"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"fold/internal/guardkit"
)

// ---------------------------------------------------------------------------
// users.status has one writer, setAccountStatusIn, which owns the move's
// session bump, seat work and token revoke (#428, docs/agents/lifecycles.md).
// upgradeGuestIn turns a guest row into an account, and two statements keep the
// status in their own UPDATE: replacePasswordIn (with the password, through
// endAllSessionsIn) and the erasure (tombstoneUserIn, and scrubTombstoneRowSQL
// for the sweep).
//
// Parse-only, root package. A users value is a name a function binds to
// models.User (modelNames), a models.User literal, or an element of a slice or
// map of them. On a chain with Model(<users value>) or Table("users"), it
// reports Update or UpdateColumn of "status" or of a column that is not a
// string literal; Updates or UpdateColumns of a users struct, of a parameter,
// or of a map[...]{...} literal, inline or a local name assigned one (:= or
// var), that has a "status" key or any key that is not a string literal, or of
// a local map given such a key by an indexed assignment (m["status"], m[k]).
// It reports the same maps passed to endAllSessionsIn, whatever the chain;
// Save of a users value; and raw SQL "UPDATE users SET" that sets status or
// whose column list follows at run time.
//
// It does not see a users value reached through a struct field, an interface
// or a helper's result the root package does not declare; a map of a named map
// type (gin.H, or a type declared elsewhere), a map built by a call (maps.Clone,
// a helper), copied from another name, or held in a struct field; a write made
// in another package; an upsert (clause.OnConflict) of a users row; or SQL
// assembled without the "UPDATE users SET" literal.
// ---------------------------------------------------------------------------

// accountStatusWriterCensus is every write of users.status the guard sees.
// apiAdminUpdateUser's map gets "status" only on the edit that changes it, and
// that edit goes to setAccountStatusIn; its own Updates runs when the map has
// no status, which parse-only analysis cannot tell apart.
var accountStatusWriterCensus = []string{
	"apiAdminUpdateUser: Updates of a map given \"status\"",
	"endAllSessionsIn: Updates of a map with computed keys",
	"replacePasswordIn: endAllSessionsIn with a map with \"status\"",
	"scrubTombstoneRowSQL: raw UPDATE users with a column list built at run time",
	"setAccountStatusIn: Updates of a parameter's map",
	"tombstoneUserIn: Updates of a map with computed keys",
	"upgradeGuestIn: Updates of a users struct",
}

var (
	updateUsersRe = regexp.MustCompile(`(?is)\bUPDATE\s+(?:public\.)?"?users"?\s+(?:AS\s+\w+\s+)?SET\b(.*?)(?:\bWHERE\b|\bFROM\b|\bRETURNING\b|$)`)
	setsStatusRe  = regexp.MustCompile(`(?i)(?:^|[\s,."])status"?\s*=`)
)

type statusWrite struct{ at, fn, shape string }

// localMaps is what fd does to each local map name: given a literal "status"
// key, or given a key that is not a string literal, in its map literal or by
// an indexed assignment.
func localMaps(fd ast.Node) (status, computed map[string]bool) {
	status, computed = map[string]bool{}, map[string]bool{}
	mark := func(name string, e ast.Expr) {
		hasStatus, hasComputed := mapLitKeys(e)
		status[name] = status[name] || hasStatus
		computed[name] = computed[name] || hasComputed
	}
	ast.Inspect(fd, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, l := range as.Lhs {
			switch v := l.(type) {
			case *ast.Ident:
				if i < len(as.Rhs) {
					mark(v.Name, as.Rhs[i])
				}
			case *ast.IndexExpr:
				x, ok := guardkit.Unparen(v.X).(*ast.Ident)
				if !ok {
					continue
				}
				if s, ok := stringLit(v.Index); !ok {
					computed[x.Name] = true
				} else if s == "status" {
					status[x.Name] = true
				}
			}
		}
		return true
	})
	ast.Inspect(fd, func(n ast.Node) bool {
		if vs, ok := n.(*ast.ValueSpec); ok {
			for i, name := range vs.Names {
				if i < len(vs.Values) {
					mark(name.Name, vs.Values[i])
				}
			}
		}
		return true
	})
	return status, computed
}

// mapLitKeys reports whether e is a map[...]{...} literal with a "status" key,
// and whether it has a key that is not a string literal (a constant, a
// variable, a call), which may name status.
func mapLitKeys(e ast.Expr) (status, computed bool) {
	lit, ok := guardkit.Unparen(e).(*ast.CompositeLit)
	if !ok {
		return false, false
	}
	if _, ok := lit.Type.(*ast.MapType); !ok {
		return false, false
	}
	for _, el := range lit.Elts {
		if kv, ok := el.(*ast.KeyValueExpr); ok {
			s, lit := stringLit(kv.Key)
			status = status || (lit && s == "status")
			computed = computed || !lit
		}
	}
	return status, computed
}

// paramNames is every parameter name of fd and its function literals.
func paramNames(fd *ast.FuncDecl) map[string]bool {
	out := map[string]bool{}
	add := func(fl *ast.FieldList) {
		if fl == nil {
			return
		}
		for _, f := range fl.List {
			for _, n := range f.Names {
				out[n.Name] = true
			}
		}
	}
	add(fd.Type.Params)
	ast.Inspect(fd, func(n ast.Node) bool {
		if fl, ok := n.(*ast.FuncLit); ok {
			add(fl.Type.Params)
		}
		return true
	})
	return out
}

// usersChain reports whether the receiver chain of a gorm call names the users
// table: a Model of a users value or Table("users") somewhere before it.
func usersChain(x ast.Expr, users modelBindings) bool {
	for {
		call, ok := guardkit.Unparen(x).(*ast.CallExpr)
		if !ok {
			return false
		}
		sel, ok := guardkit.Unparen(call.Fun).(*ast.SelectorExpr)
		if !ok {
			return false
		}
		switch sel.Sel.Name {
		case "Model":
			if len(call.Args) == 1 && modelValue(call.Args[0], "User", users) {
				return true
			}
		case "Table":
			if len(call.Args) > 0 {
				if s, ok := stringLit(call.Args[0]); ok && len(strings.Fields(s)) > 0 && strings.Fields(s)[0] == "users" {
					return true
				}
			}
		}
		x = sel.X
	}
}

// mapShape is how a map argument carries "status", or "" when it does not.
func mapShape(e ast.Expr, status, computed, params map[string]bool) string {
	switch hasStatus, hasComputed := mapLitKeys(e); {
	case hasStatus:
		return `a map with "status"`
	case hasComputed:
		return "a map with computed keys"
	}
	id, ok := guardkit.Unparen(e).(*ast.Ident)
	if !ok {
		return ""
	}
	switch {
	case params[id.Name]:
		return "a parameter's map"
	case status[id.Name]:
		return `a map given "status"`
	case computed[id.Name]:
		return "a map with computed keys"
	}
	return ""
}

// accountStatusWrites finds every write of users.status in the root package.
func accountStatusWrites(p *guardkit.Parsed) []statusWrite {
	var out []statusWrite
	files := p.Package("main")
	funcs := modelFuncs(p, "User")
	add := func(n ast.Node, fn, shape string) {
		out = append(out, statusWrite{p.Fset.Position(n.Pos()).String(), fn, shape})
	}
	rawSQL := func(root ast.Node, fn string) {
		ast.Inspect(root, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok {
				s, _ := stringLit(lit)
				for _, m := range updateUsersRe.FindAllStringSubmatch(s, -1) {
					if setsStatusRe.MatchString(m[1]) {
						add(n, fn, "raw UPDATE of users.status")
					} else if strings.TrimSpace(m[1]) == "" {
						add(n, fn, "raw UPDATE users with a column list built at run time")
					}
				}
			}
			return true
		})
	}
	for _, f := range files {
		if strings.Contains(f.Path, "/") {
			continue
		}
		for _, d := range f.AST.Decls {
			if gd, ok := d.(*ast.GenDecl); ok && (gd.Tok == token.VAR || gd.Tok == token.CONST) {
				for _, spec := range gd.Specs {
					vs := spec.(*ast.ValueSpec)
					rawSQL(vs, vs.Names[0].Name)
				}
				continue
			}
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			name := fd.Name.Name
			rawSQL(fd.Body, name)
			users := modelNames(fd, "User", funcs)
			status, computed := localMaps(fd)
			params := paramNames(fd)
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if guardkit.Callee(call) == "endAllSessionsIn" && len(call.Args) == 3 {
					if shape := mapShape(call.Args[2], status, computed, params); shape != "" {
						add(call, name, "endAllSessionsIn with "+shape)
					}
					return true
				}
				sel, ok := guardkit.Unparen(call.Fun).(*ast.SelectorExpr)
				if !ok || len(call.Args) == 0 {
					return true
				}
				method, arg := sel.Sel.Name, call.Args[0]
				onUsers := usersChain(sel.X, users)
				switch method {
				case "Update", "UpdateColumn":
					if s, lit := stringLit(arg); onUsers && lit && s == "status" {
						add(call, name, method+`("status")`)
					} else if onUsers && !lit {
						add(call, name, method+" of a computed column")
					}
				case "Updates", "UpdateColumns":
					if modelValue(arg, "User", users) {
						add(call, name, method+" of a users struct")
					} else if shape := mapShape(arg, status, computed, params); shape != "" && onUsers {
						add(call, name, method+" of "+shape)
					}
				case "Save":
					if modelValue(arg, "User", users) {
						add(call, name, "Save of a users row")
					}
				}
				return true
			})
		}
	}
	return out
}

func statusWriteCensus(writes []statusWrite) []string {
	got := []string{}
	for _, w := range writes {
		got = append(got, w.fn+": "+w.shape)
	}
	sort.Strings(got)
	return got
}

func TestAccountStatusHasOneWriter(t *testing.T) {
	parsed := guardkit.Production(t).Parse(0)
	for _, p := range parsed.Problems {
		t.Error(p)
	}
	writes := accountStatusWrites(parsed)
	if got := statusWriteCensus(writes); !reflect.DeepEqual(got, accountStatusWriterCensus) {
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
}`, "roll: Save of a users row"},
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
			got := strings.Join(statusWriteCensus(accountStatusWrites(p)), "\n")
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
