package main

import (
	"go/ast"
	"go/token"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"fold/internal/guardkit"
	"fold/models"

	"gorm.io/gorm/schema"
)

// ---------------------------------------------------------------------------
// registrations.status has two writers: saveRegistration (with
// saveRegistrationStep, through writeRegistration), which checks the caller's
// policy.RegistrationMove, and promoteRegistration, whose WHERE is the
// promotion's from-set (docs/agents/lifecycles.md). An insert is a Save of a
// row with no id, so there is no other creator.
//
// Parse-only, every production package. A registration value is a name a
// function binds to models.Registration or a collection of them (modelNames:
// by declared type, initializer, new, a declared function's result, a range or
// a type switch), a models.Registration literal, or an element of a
// collection. A registrations query is a gorm chain with Model(<registration
// value or collection>) or Table("registrations"), or a local name assigned
// one. A key names the status column when GORM's schema lookup resolves it
// there ("status", "Status"; regStatusKey).
//
// The guard reports, anywhere, a Create, CreateInBatches, FirstOrCreate or
// Save of a registration value or collection (an insert writes the status,
// the column's default included), and Updates or UpdateColumns of one (a
// struct writes its set fields, Status among them, whatever Select names). On
// a registrations query it reports Update or UpdateColumn of the status column
// or of a column that is not a string literal, and every Updates or
// UpdateColumns except of a map it can read whole: a map literal, behind & or
// not, or a local map the function makes and only sets, keys and passes to
// Updates (behind & or not; no other call, no second name), whose keys are all
// plain column names in string literals, none of them status. So a struct of
// another type, a parameter's map, a map with a computed or quoted key and a
// map built elsewhere are all reported. A name counts once per function,
// whatever scope binds it. It reports raw SQL that inserts into registrations,
// or updates registrations and sets status, or whose SET list runs to the end
// of its literal (the rest is built at run time).
//
// It does not see a registration reached through a struct field, an interface,
// a parameter (a *gorm.DB already scoped to registrations) or a result of a
// function the scanned tree does not declare; a package that imports models
// under another name; an upsert's DoUpdates or an Assign(...).FirstOrCreate on
// a chain; gorm calls in a function literal at package level (there it reads
// only raw SQL); or SQL assembled without the "UPDATE registrations ... SET" or
// "INSERT INTO registrations" literal. It does not see deletes: the three
// statements that delete rows are named in docs/agents/lifecycles.md. It does
// not judge which move a caller passes; the policy table and the writers'
// refusals decide that.
// ---------------------------------------------------------------------------

// registrationStatusWriterCensus is every write of registrations.status the
// guard sees.
var registrationStatusWriterCensus = []string{
	`promoteRegistration: Updates of a map with "status"`,
	"writeRegistration: Save of a registration",
}

var (
	regUpdateRe      = regexp.MustCompile(`(?is)\bUPDATE\s+(?:ONLY\s+)?(?:public\.)?"?registrations"?(?:\s+(?:AS\s+)?\w+)?\s+SET\b(.*?)(\bWHERE\b|\bFROM\b|\bRETURNING\b|;|$)`)
	regInsertRe      = regexp.MustCompile(`(?is)\bINSERT\s+INTO\s+(?:public\.)?"?registrations"?(?:\s|\(|$)`)
	regPlainColumnRe = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
	regSetsStatusRe  = regexp.MustCompile(`(?i)(?:^|[\s,."])status"?\s*=`)
)

type regStatusWrite struct{ at, fn, shape string }

var registrationSchema = sync.OnceValues(func() (*schema.Schema, error) {
	return schema.Parse(&models.Registration{}, &sync.Map{}, schema.NamingStrategy{})
})

// regStatusKey reports whether GORM resolves the map key or Update column k
// to registrations.status (LookUpField: the column name, the field name, then
// the naming strategy's column for k).
func regStatusKey(k string) bool {
	s, err := registrationSchema()
	if err != nil {
		panic(err)
	}
	f := s.LookUpField(k)
	return f != nil && f.DBName == "status"
}

// regRef reports whether e is a registration value or, behind & or not, a name
// holding a collection of them.
func regRef(e ast.Expr, regs modelBindings) bool {
	if modelValue(e, "Registration", regs) {
		return true
	}
	e = guardkit.Unparen(e)
	if u, ok := e.(*ast.UnaryExpr); ok {
		e = guardkit.Unparen(u.X)
	}
	id, ok := e.(*ast.Ident)
	return ok && regs.collections[id.Name]
}

// regChain reports whether a gorm call chain names the registrations table.
func regChain(x ast.Expr, regs modelBindings, queries map[string]bool) bool {
	for {
		switch v := guardkit.Unparen(x).(type) {
		case *ast.Ident:
			return queries[v.Name]
		case *ast.CallExpr:
			sel, ok := guardkit.Unparen(v.Fun).(*ast.SelectorExpr)
			if !ok {
				return false
			}
			switch sel.Sel.Name {
			case "Model":
				if len(v.Args) == 1 && regRef(v.Args[0], regs) {
					return true
				}
			case "Table":
				if len(v.Args) > 0 {
					if s, ok := stringLit(v.Args[0]); ok && len(strings.Fields(s)) > 0 &&
						strings.Trim(strings.TrimPrefix(strings.Fields(s)[0], "public."), `"`) == "registrations" {
						return true
					}
				}
			}
			x = sel.X
		default:
			return false
		}
	}
}

// regQueries is every local name fd assigns a registrations query, directly or
// from another such name.
func regQueries(fd *ast.FuncDecl, regs modelBindings) map[string]bool {
	queries := map[string]bool{}
	for changed := true; changed; {
		changed = false
		mark := func(name string, e ast.Expr) {
			if name != "_" && !queries[name] && regChain(e, regs, queries) {
				queries[name] = true
				changed = true
			}
		}
		ast.Inspect(fd, func(n ast.Node) bool {
			switch s := n.(type) {
			case *ast.AssignStmt:
				for i, l := range s.Lhs {
					if id, ok := l.(*ast.Ident); ok && len(s.Lhs) == len(s.Rhs) {
						mark(id.Name, s.Rhs[i])
					}
				}
			case *ast.ValueSpec:
				for i, name := range s.Names {
					if len(s.Names) == len(s.Values) {
						mark(name.Name, s.Values[i])
					}
				}
			}
			return true
		})
	}
	return queries
}

// regKeyShape reads one map key: whether it names the status column, and
// whether the guard cannot read it (not a string literal, or not a plain
// column name, which gorm writes as given).
func regKeyShape(k ast.Expr) (status, computed bool) {
	s, ok := stringLit(k)
	switch {
	case !ok:
		return false, true
	case regStatusKey(s):
		return true, false
	case !regPlainColumnRe.MatchString(s):
		return false, true
	}
	return false, false
}

// regMapLit reads a map literal, behind & or not.
func regMapLit(e ast.Expr) (isMap, status, computed bool) {
	e = guardkit.Unparen(e)
	if u, ok := e.(*ast.UnaryExpr); ok && u.Op == token.AND {
		e = guardkit.Unparen(u.X)
	}
	lit, ok := e.(*ast.CompositeLit)
	if !ok {
		return false, false, false
	}
	if _, ok := lit.Type.(*ast.MapType); !ok {
		return false, false, false
	}
	for _, el := range lit.Elts {
		if kv, ok := el.(*ast.KeyValueExpr); ok {
			st, comp := regKeyShape(kv.Key)
			status, computed = status || st, computed || comp
		}
	}
	return true, status, computed
}

// regMapMade: e makes a map the guard can read, a map literal or make.
func regMapMade(e ast.Expr) bool {
	switch v := guardkit.Unparen(e).(type) {
	case *ast.CompositeLit:
		_, ok := v.Type.(*ast.MapType)
		return ok
	case *ast.CallExpr:
		fn, ok := v.Fun.(*ast.Ident)
		if !ok || fn.Name != "make" || len(v.Args) == 0 {
			return false
		}
		_, ok = v.Args[0].(*ast.MapType)
		return ok
	}
	return false
}

func regUpdatesCall(n ast.Node) bool {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := guardkit.Unparen(call.Fun).(*ast.SelectorExpr)
	return ok && (sel.Sel.Name == "Updates" || sel.Sel.Name == "UpdateColumns")
}

// regLocalMaps is what fd does to each local map name: given a status key,
// given a key the guard cannot read, or bound to a map it made; and every
// parameter name of fd and its function literals.
func regLocalMaps(fd *ast.FuncDecl) (status, computed, made, params map[string]bool) {
	status, computed, made, params = map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	addParams := func(fl *ast.FieldList) {
		if fl == nil {
			return
		}
		for _, f := range fl.List {
			for _, n := range f.Names {
				params[n.Name] = true
			}
		}
	}
	addParams(fd.Type.Params)
	bind := func(name string, e ast.Expr) {
		_, st, comp := regMapLit(e)
		status[name] = status[name] || st
		computed[name] = computed[name] || comp
		if regMapMade(e) {
			made[name] = true
		} else {
			// Bound to something the guard did not see made.
			computed[name] = true
		}
	}
	ast.Inspect(fd, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.FuncLit:
			addParams(v.Type.Params)
		case *ast.ValueSpec:
			for i, name := range v.Names {
				switch {
				case len(v.Values) == len(v.Names):
					bind(name.Name, v.Values[i])
				case len(v.Values) > 0:
					// One call fills several names.
					computed[name.Name] = true
				}
			}
		case *ast.AssignStmt:
			for i, l := range v.Lhs {
				switch lv := l.(type) {
				case *ast.Ident:
					if len(v.Lhs) != len(v.Rhs) {
						computed[lv.Name] = true
					} else {
						bind(lv.Name, v.Rhs[i])
					}
				case *ast.IndexExpr:
					if x, ok := guardkit.Unparen(lv.X).(*ast.Ident); ok {
						st, comp := regKeyShape(lv.Index)
						status[x.Name] = status[x.Name] || st
						computed[x.Name] = computed[x.Name] || comp
					}
				}
			}
		}
		return true
	})
	// A made map stays readable only while its name is set (declared or
	// assigned), given a key (m[k] = v) or passed to Updates, behind & or not.
	// Any other use (another call such as maps.Copy, a second name, a range, a
	// return) may add keys the guard does not see.
	guardkit.InspectStack(fd.Body, func(n ast.Node, stack []ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok || !made[id.Name] || len(stack) < 2 {
			return true
		}
		switch parent := stack[len(stack)-2].(type) {
		case *ast.ValueSpec:
			for _, name := range parent.Names {
				if name == id {
					return true
				}
			}
		case *ast.AssignStmt:
			for _, l := range parent.Lhs {
				if l == ast.Expr(id) {
					return true
				}
			}
		case *ast.IndexExpr:
			if parent.X == ast.Expr(id) && len(stack) >= 3 {
				if as, ok := stack[len(stack)-3].(*ast.AssignStmt); ok {
					for _, l := range as.Lhs {
						if l == ast.Expr(parent) {
							return true
						}
					}
				}
			}
		case *ast.UnaryExpr:
			if parent.Op == token.AND && len(stack) >= 3 && regUpdatesCall(stack[len(stack)-3]) {
				return true
			}
		case *ast.CallExpr:
			if regUpdatesCall(parent) {
				return true
			}
		}
		computed[id.Name] = true
		return true
	})
	return status, computed, made, params
}

// regMapShape names what an Updates argument is, or "" when the guard reads it
// whole and it leaves the status alone.
func regMapShape(e ast.Expr, status, computed, made, params map[string]bool) string {
	if isMap, st, comp := regMapLit(e); isMap {
		switch {
		case st:
			return `a map with "status"`
		case comp:
			return "a map with computed keys"
		}
		return ""
	}
	e = guardkit.Unparen(e)
	if u, ok := e.(*ast.UnaryExpr); ok && u.Op == token.AND {
		e = guardkit.Unparen(u.X)
	}
	id, ok := e.(*ast.Ident)
	if !ok {
		return "a value it cannot read"
	}
	switch {
	case status[id.Name]:
		return `a map given "status"`
	case params[id.Name]:
		return "a parameter's map"
	case computed[id.Name]:
		return "a map with computed keys"
	case made[id.Name]:
		return ""
	}
	return "a value it cannot read"
}

// registrationStatusWrites finds every write of registrations.status in p.
func registrationStatusWrites(p *guardkit.Parsed) []regStatusWrite {
	var out []regStatusWrite
	funcs := modelFuncs(p, "Registration")
	add := func(n ast.Node, fn, shape string) {
		out = append(out, regStatusWrite{p.Fset.Position(n.Pos()).String(), fn, shape})
	}
	rawSQL := func(root ast.Node, fn string) {
		ast.Inspect(root, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok {
				return true
			}
			s, _ := stringLit(lit)
			if regInsertRe.MatchString(s) {
				add(n, fn, "raw INSERT INTO registrations")
			}
			for _, m := range regUpdateRe.FindAllStringSubmatch(s, -1) {
				if regSetsStatusRe.MatchString(m[1]) {
					add(n, fn, "raw UPDATE of registrations.status")
				} else if m[2] == "" {
					add(n, fn, "raw UPDATE registrations with a column list built at run time")
				}
			}
			return true
		})
	}
	for _, f := range p.Files {
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
			regs := modelNames(fd, "Registration", funcs)
			queries := regQueries(fd, regs)
			status, computed, made, params := regLocalMaps(fd)
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := guardkit.Unparen(call.Fun).(*ast.SelectorExpr)
				if !ok || len(call.Args) == 0 {
					return true
				}
				method, arg := sel.Sel.Name, call.Args[0]
				switch method {
				case "Create", "CreateInBatches", "FirstOrCreate", "Save":
					if regRef(arg, regs) {
						verb := method
						if method != "Save" {
							verb = "Create"
						}
						add(call, name, verb+" of a registration")
					}
					return true
				case "Updates", "UpdateColumns":
					if regRef(arg, regs) {
						add(call, name, method+" of a registration struct")
						return true
					}
				}
				if !regChain(sel.X, regs, queries) {
					return true
				}
				switch method {
				case "Update", "UpdateColumn":
					if s, lit := stringLit(arg); lit && regStatusKey(s) {
						add(call, name, method+`("status")`)
					} else if !lit {
						add(call, name, method+" of a computed column")
					}
				case "Updates", "UpdateColumns":
					if shape := regMapShape(arg, status, computed, made, params); shape != "" {
						add(call, name, method+" of "+shape)
					}
				}
				return true
			})
		}
	}
	return out
}

func regStatusCensus(writes []regStatusWrite) []string {
	got := []string{}
	for _, w := range writes {
		got = append(got, w.fn+": "+w.shape)
	}
	sort.Strings(got)
	return got
}

func TestRegistrationStatusHasTwoWriters(t *testing.T) {
	parsed := guardkit.Production(t).Parse(0)
	for _, p := range parsed.Problems {
		t.Error(p)
	}
	writes := registrationStatusWrites(parsed)
	if got := regStatusCensus(writes); !reflect.DeepEqual(got, registrationStatusWriterCensus) {
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
		{"a cancel written beside the writer", `package main
func (a *App) cancel(tx *gorm.DB, r *models.Registration) error {
	return tx.Model(r).Update("status", models.RegCancelled).Error
}`, `cancel: Update("status")`},
		{"the field's name, as gorm resolves it", `package main
func (a *App) cancel(tx *gorm.DB, r *models.Registration) error {
	tx.Model(r).UpdateColumn("Status", models.RegCancelled)
	return tx.Model(r).Updates(map[string]any{"Status": models.RegCancelled}).Error
}`, strings.Join([]string{`cancel: UpdateColumn("status")`, `cancel: Updates of a map with "status"`}, "\n")},
		{"a conditional update on the table", `package main
func (a *App) promote(tx *gorm.DB, id uint) error {
	return tx.Model(&models.Registration{}).Where("id = ? AND status = ?", id, models.RegWaitlisted).
		Updates(map[string]any{"status": models.RegGoing, "promoted_at": time.Now()}).Error
}`, `promote: Updates of a map with "status"`},
		{"a map given status later", `package main
func (a *App) cancel(tx *gorm.DB, id uint) error {
	cols := map[string]any{"removal_note": ""}
	cols["status"] = models.RegCancelled
	return tx.Model(&models.Registration{}).Where("id = ?", id).Updates(cols).Error
}`, `cancel: Updates of a map given "status"`},
		{"a pointer to a local map", `package main
func (a *App) cancel(tx *gorm.DB, r *models.Registration) error {
	cols := map[string]any{"status": models.RegCancelled}
	return tx.Model(r).Updates(&cols).Error
}`, `cancel: Updates of a map given "status"`},
		{"a pointer to a map literal", `package main
func (a *App) cancel(tx *gorm.DB, r *models.Registration) error {
	return tx.Model(r).Updates(&map[string]any{"status": models.RegCancelled}).Error
}`, `cancel: Updates of a map with "status"`},
		{"a map with a computed key", `package main
func (a *App) set(tx *gorm.DB, r *models.Registration, col string) error {
	return tx.Model(r).Updates(map[string]any{col: models.RegGoing}).Error
}`, "set: Updates of a map with computed keys"},
		{"a map with a quoted key", `package main
func (a *App) set(tx *gorm.DB, r *models.Registration) error {
	return tx.Model(r).Updates(map[string]any{"\"status\"": models.RegGoing}).Error
}`, "set: Updates of a map with computed keys"},
		{"a constant key", `package main
const statusColumn = "status"
func (a *App) set(tx *gorm.DB, r *models.Registration) error {
	return tx.Model(r).UpdateColumns(map[string]any{statusColumn: models.RegGoing}).Error
}`, "set: UpdateColumns of a map with computed keys"},
		{"a map handed to another call", `package main
func (a *App) set(tx *gorm.DB, r *models.Registration, extra map[string]any) error {
	cols := map[string]any{"removal_note": ""}
	maps.Copy(cols, extra)
	return tx.Model(r).Updates(cols).Error
}`, "set: Updates of a map with computed keys"},
		{"a map under a second name", `package main
func (a *App) set(tx *gorm.DB, r *models.Registration) error {
	cols := map[string]any{"removal_note": ""}
	more := cols
	more["status"] = models.RegGoing
	return tx.Model(r).Updates(cols).Error
}`, "set: Updates of a map with computed keys"},
		{"a pointer to a map under a second name", `package main
func (a *App) set(tx *gorm.DB, r *models.Registration) error {
	cols := map[string]any{"removal_note": ""}
	p := &cols
	(*p)["status"] = models.RegGoing
	return tx.Model(r).Updates(p).Error
}`, strings.Join([]string{"set: Updates of a map with computed keys"}, "\n")},
		{"a map built elsewhere", `package main
func (a *App) set(tx *gorm.DB, r *models.Registration) error {
	cols := cancelColumns()
	return tx.Model(r).Updates(cols).Error
}` + decls, "set: Updates of a map with computed keys"},
		{"a helper that writes its caller's columns", `package main
func writeReg(tx *gorm.DB, id uint, cols map[string]any) error {
	return tx.Model(&models.Registration{}).Where("id = ?", id).Updates(cols).Error
}`, "writeReg: Updates of a parameter's map"},
		{"a column named at run time", `package main
func (a *App) set(tx *gorm.DB, r *models.Registration, col string) error {
	return tx.Model(r).Update(col, models.RegGoing).Error
}`, "set: Update of a computed column"},
		{"a registration struct, whatever Select names", `package main
func (a *App) set(tx *gorm.DB, id uint) error {
	tx.Model(&models.Registration{}).Where("id = ?", id).Select("status").Updates(models.Registration{Status: models.RegGoing})
	return tx.Where("id = ?", id).Updates(&models.Registration{Status: models.RegGoing}).Error
}`, strings.Join([]string{"set: Updates of a registration struct", "set: Updates of a registration struct"}, "\n")},
		{"a struct of another type", `package main
func (a *App) set(tx *gorm.DB, r *models.Registration) error {
	return tx.Model(r).Updates(struct{ Status string }{models.RegGoing}).Error
}`, "set: Updates of a value it cannot read"},
		{"a saved row", `package main
func (a *App) set(tx *gorm.DB, r *models.Registration) error {
	r.Status = models.RegGoing
	return tx.Omit("access_origin").Save(r).Error
}`, "set: Save of a registration"},
		{"a new row and a batch of them", `package main
func (a *App) add(tx *gorm.DB, uid, oid uint) error {
	r := models.Registration{UserID: &uid, OccurrenceID: oid}
	tx.Create(&r)
	rows := []models.Registration{r}
	tx.CreateInBatches(&rows, 10)
	return tx.FirstOrCreate(&models.Registration{}, "user_id = ?", uid).Error
}`, strings.Join([]string{"add: Create of a registration", "add: Create of a registration", "add: Create of a registration"}, "\n")},
		{"a loaded row, a range and a query held in a name", `package main
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
		{"raw SQL", `package main
func (a *App) set(tx *gorm.DB, id uint) error {
	return tx.Exec("UPDATE registrations SET removal_note = '', status = 'cancelled' WHERE id = ?", id).Error
}`, "set: raw UPDATE of registrations.status"},
		{"raw SQL in a constant with an alias", `package main
const cancelSQL = ` + "`UPDATE registrations AS r SET \"status\" = 'cancelled' WHERE r.id = ?`" + `
`, "cancelSQL: raw UPDATE of registrations.status"},
		{"raw SQL whose SET list is built at run time", `package main
func (a *App) set(tx *gorm.DB, cols string) error {
	return tx.Exec("UPDATE registrations SET removal_note = '', " + cols + " WHERE id = 1").Error
}`, "set: raw UPDATE registrations with a column list built at run time"},
		{"a raw insert", `package main
func (a *App) add(tx *gorm.DB) error {
	return tx.Exec("INSERT INTO registrations (occurrence_id, user_id) VALUES (1, 1)").Error
}`, "add: raw INSERT INTO registrations"},
		{"other columns and other tables", `package main
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
			p := guardkit.Sources{"x.go": c.src}.Parse(0)
			if len(p.Problems) > 0 {
				t.Fatal(p.Problems)
			}
			got := strings.Join(regStatusCensus(registrationStatusWrites(p)), "\n")
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
