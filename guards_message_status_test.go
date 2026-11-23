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
// messages.status has one writer of an existing row's status, moveMessage
// (moveMessagesWhere for a set), whose WHERE is the move's from-set in
// policy.MessageMove; three creators insert a move's first status
// (docs/agents/lifecycles.md).
//
// Parse-only, every production package. A message value is a name a function
// binds to models.Message (or a pointer or slice of it): a parameter, a var
// of that type, an assignment from a models.Message literal, from another
// message value or from a declared function whose first result is one, or a
// range over a slice of them. A messages chain is a gorm call chain with
// Model(<message value>) or Table("messages"), or a name assigned one. On a
// messages chain the guard reports Update or UpdateColumn of "status" or of a
// column it cannot read, and every Updates or UpdateColumns except of a map
// it can read whole: a map literal, or a local map the function makes and
// only sets, keys and passes to Updates (no other call, no second name for
// it), whose keys are all plain column names in string literals, none of
// them status. So a message struct, any
// other struct, a parameter's map, a map with a computed key and a map built
// elsewhere are all reported, whatever Select precedes them. "Status", the
// field's name, counts as the column, as gorm resolves it. Anywhere it reports a
// Create, CreateInBatches, FirstOrCreate or Save of a message value (an
// insert writes the status, the column's default included), and raw SQL that
// inserts into messages, or updates messages and sets status, or whose SET
// list runs to the end of its literal (the rest is built at run time).
//
// It does not see a message reached through a struct field, an interface or
// a result of a function the scanned tree does not declare; a package that
// imports models under another name; an upsert's DoUpdates; gorm calls in a
// function literal at package level (there it reads only raw SQL); or SQL
// assembled without the "UPDATE messages SET" or "INSERT INTO messages"
// literal. It does not judge which status a creator inserts or which move a
// caller passes: the policy table and moveMessage's refusals decide those.
// ---------------------------------------------------------------------------

// messageStatusWriterCensus is every write of messages.status the guard sees.
var messageStatusWriterCensus = []string{
	"apiCreateMessage: Create of a message",
	"moveMessage: Updates of a map given \"status\"",
	"moveMessagesWhere: raw UPDATE of messages.status",
	"writePushReminder: Create of a message",
	"writeReminder: Create of a message",
}

var (
	messageUpdateRe      = regexp.MustCompile(`(?is)\bUPDATE\s+(?:ONLY\s+)?(?:public\.)?"?messages"?(?:\s+(?:AS\s+)?\w+)?\s+SET\b(.*?)(\bWHERE\b|\bFROM\b|\bRETURNING\b|;|$)`)
	messageInsertRe      = regexp.MustCompile(`(?is)\bINSERT\s+INTO\s+(?:public\.)?"?messages"?(?:\s|\(|$)`)
	messagePlainColumnRe = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
	messageSetsStatusRe  = regexp.MustCompile(`(?i)(?:^|[\s,."])status"?\s*=`)
)

type messageStatusWrite struct{ at, fn, shape string }

// messageTypeExpr: models.Message, a pointer to it, or a slice of either.
func messageTypeExpr(e ast.Expr) bool {
	switch v := guardkit.Unparen(e).(type) {
	case *ast.StarExpr:
		return messageTypeExpr(v.X)
	case *ast.ArrayType:
		return messageTypeExpr(v.Elt)
	case *ast.SelectorExpr:
		x, ok := v.X.(*ast.Ident)
		return ok && x.Name == "models" && v.Sel.Name == "Message"
	}
	return false
}

// messageValue: e is a message literal or a message name, or the address or
// an element of one.
func messageValue(e ast.Expr, names map[string]bool) bool {
	switch v := guardkit.Unparen(e).(type) {
	case *ast.CompositeLit:
		return messageTypeExpr(v.Type)
	case *ast.UnaryExpr:
		return v.Op == token.AND && messageValue(v.X, names)
	case *ast.StarExpr:
		return messageValue(v.X, names)
	case *ast.IndexExpr:
		return messageValue(v.X, names)
	case *ast.Ident:
		return names[v.Name]
	case *ast.CallExpr:
		fn, ok := v.Fun.(*ast.Ident)
		return ok && fn.Name == "new" && len(v.Args) == 1 && messageTypeExpr(v.Args[0])
	}
	return false
}

// messageFuncs is every declared function whose first result is a message.
func messageFuncs(p *guardkit.Parsed) map[string]bool {
	out := map[string]bool{}
	for _, f := range p.Files {
		for _, d := range f.AST.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Type.Results != nil && len(fd.Type.Results.List) > 0 && messageTypeExpr(fd.Type.Results.List[0].Type) {
				out[fd.Name.Name] = true
			}
		}
	}
	return out
}

// messageNames is every name fd binds to a message value, and every name it
// assigns a messages chain.
func messageNames(fd *ast.FuncDecl, funcs map[string]bool) (msgs, chains map[string]bool) {
	msgs, chains = map[string]bool{}, map[string]bool{}
	params := func(ft *ast.FuncType) {
		for _, fl := range []*ast.FieldList{ft.Params, ft.Results} {
			if fl == nil {
				continue
			}
			for _, f := range fl.List {
				if messageTypeExpr(f.Type) {
					for _, n := range f.Names {
						msgs[n.Name] = true
					}
				}
			}
		}
	}
	params(fd.Type)
	if fd.Recv != nil {
		params(&ast.FuncType{Params: fd.Recv})
	}
	// Twice, so a name bound from one bound later in the source still counts.
	for range 2 {
		ast.Inspect(fd, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.FuncLit:
				params(v.Type)
			case *ast.ValueSpec:
				for i, name := range v.Names {
					if (v.Type != nil && messageTypeExpr(v.Type)) || (i < len(v.Values) && messageValue(v.Values[i], msgs)) {
						msgs[name.Name] = true
					}
				}
			case *ast.AssignStmt:
				for i, l := range v.Lhs {
					id, ok := l.(*ast.Ident)
					if !ok {
						continue
					}
					var rhs ast.Expr
					if len(v.Rhs) == len(v.Lhs) {
						rhs = v.Rhs[i]
					} else if len(v.Rhs) == 1 && i == 0 {
						rhs = v.Rhs[0]
					}
					if rhs == nil {
						continue
					}
					if messageValue(rhs, msgs) {
						msgs[id.Name] = true
					} else if call, ok := guardkit.Unparen(rhs).(*ast.CallExpr); ok && funcs[guardkit.Callee(call)] {
						msgs[id.Name] = true
					} else if messageChain(rhs, msgs, chains) {
						chains[id.Name] = true
					}
				}
			case *ast.RangeStmt:
				if messageValue(v.X, msgs) {
					if id, ok := v.Value.(*ast.Ident); ok {
						msgs[id.Name] = true
					}
				}
			}
			return true
		})
	}
	return msgs, chains
}

// messageChain reports whether a gorm call chain names the messages table.
func messageChain(x ast.Expr, msgs, chains map[string]bool) bool {
	for {
		switch v := guardkit.Unparen(x).(type) {
		case *ast.Ident:
			return chains[v.Name]
		case *ast.CallExpr:
			sel, ok := guardkit.Unparen(v.Fun).(*ast.SelectorExpr)
			if !ok {
				return false
			}
			switch sel.Sel.Name {
			case "Model":
				if len(v.Args) == 1 && messageValue(v.Args[0], msgs) {
					return true
				}
			case "Table":
				if len(v.Args) > 0 {
					if s, ok := stringLit(v.Args[0]); ok && len(strings.Fields(s)) > 0 && strings.Fields(s)[0] == "messages" {
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

// messageStatusKey: gorm resolves a column by its name or by the field's, so
// "Status" names the status column too.
func messageStatusKey(s string) bool { return strings.EqualFold(s, "status") }

// messageMapLit reads a map literal: whether it holds a status key, and
// whether it holds a key that is not a string literal.
func messageMapLit(e ast.Expr) (status, computed bool) {
	lit, ok := guardkit.Unparen(e).(*ast.CompositeLit)
	if !ok {
		return false, false
	}
	if _, ok := lit.Type.(*ast.MapType); !ok {
		return false, false
	}
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if s, ok := stringLit(kv.Key); !ok {
			computed = true
		} else if messageStatusKey(s) {
			status = true
		} else if !messagePlainColumnRe.MatchString(s) {
			// A quoted identifier or an expression: gorm writes it as given.
			computed = true
		}
	}
	return status, computed
}

// messageMapMade: e makes a map the guard can read, a map literal or make.
func messageMapMade(e ast.Expr) bool {
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

// messageLocalMaps is what fd does to each local map: given a "status" key
// (in its literal or by assignment), or given a computed key; every name it
// binds to a map it makes itself; and every parameter name of fd and its
// function literals.
func messageLocalMaps(fd *ast.FuncDecl) (status, computed, made, params map[string]bool) {
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
	ast.Inspect(fd, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.FuncLit:
			addParams(v.Type.Params)
		case *ast.ValueSpec:
			for i, name := range v.Names {
				if i < len(v.Values) {
					st, comp := messageMapLit(v.Values[i])
					status[name.Name] = status[name.Name] || st
					computed[name.Name] = computed[name.Name] || comp
					made[name.Name] = made[name.Name] || messageMapMade(v.Values[i])
				}
			}
		case *ast.AssignStmt:
			for i, l := range v.Lhs {
				switch lv := l.(type) {
				case *ast.Ident:
					if i < len(v.Rhs) {
						st, comp := messageMapLit(v.Rhs[i])
						status[lv.Name] = status[lv.Name] || st
						computed[lv.Name] = computed[lv.Name] || comp
						if messageMapMade(v.Rhs[i]) {
							made[lv.Name] = true
						} else {
							// Bound to something the guard did not see made.
							computed[lv.Name] = true
						}
					}
				case *ast.IndexExpr:
					x, ok := guardkit.Unparen(lv.X).(*ast.Ident)
					if !ok {
						continue
					}
					if s, ok := stringLit(lv.Index); !ok {
						computed[x.Name] = true
					} else if messageStatusKey(s) {
						status[x.Name] = true
					} else if !messagePlainColumnRe.MatchString(s) {
						computed[x.Name] = true
					}
				}
			}
		}
		return true
	})
	// A made map stays readable only while its name is used in three ways: set
	// (it is declared or assigned), given a key (m[k] = v) and passed to
	// Updates. Any other use (another call such as maps.Copy, a second name
	// for it, a range, a return) may add keys the guard does not see.
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
		case *ast.CallExpr:
			if sel, ok := guardkit.Unparen(parent.Fun).(*ast.SelectorExpr); ok && (sel.Sel.Name == "Updates" || sel.Sel.Name == "UpdateColumns") {
				return true
			}
		}
		computed[id.Name] = true
		return true
	})
	return status, computed, made, params
}

// messageMapShape names what an Updates argument is, or "" when the guard can
// read it and it leaves the status alone: a map literal, or a local map the
// function made, whose keys are all string literals other than status.
func messageMapShape(e ast.Expr, status, computed, made, params map[string]bool) string {
	if st, comp := messageMapLit(e); st {
		return `a map with "status"`
	} else if comp {
		return "a map with computed keys"
	} else if messageMapMade(e) {
		return ""
	}
	id, ok := guardkit.Unparen(e).(*ast.Ident)
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

// messageStatusWrites finds every write of messages.status in p.
func messageStatusWrites(p *guardkit.Parsed) []messageStatusWrite {
	var out []messageStatusWrite
	funcs := messageFuncs(p)
	add := func(n ast.Node, fn, shape string) {
		out = append(out, messageStatusWrite{p.Fset.Position(n.Pos()).String(), fn, shape})
	}
	rawSQL := func(root ast.Node, fn string) {
		ast.Inspect(root, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok {
				return true
			}
			s, _ := stringLit(lit)
			if messageInsertRe.MatchString(s) {
				add(n, fn, "raw INSERT INTO messages")
			}
			for _, m := range messageUpdateRe.FindAllStringSubmatch(s, -1) {
				if messageSetsStatusRe.MatchString(m[1]) {
					add(n, fn, "raw UPDATE of messages.status")
				} else if m[2] == "" {
					add(n, fn, "raw UPDATE messages with a column list built at run time")
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
			msgs, chains := messageNames(fd, funcs)
			status, computed, made, params := messageLocalMaps(fd)
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
					if messageValue(arg, msgs) {
						verb := method
						if method != "Save" {
							verb = "Create"
						}
						add(call, name, verb+" of a message")
					}
					return true
				}
				if !messageChain(sel.X, msgs, chains) {
					return true
				}
				switch method {
				case "Update", "UpdateColumn":
					if s, lit := stringLit(arg); lit && messageStatusKey(s) {
						add(call, name, method+`("status")`)
					} else if !lit {
						add(call, name, method+" of a computed column")
					}
				case "Updates", "UpdateColumns":
					if messageValue(arg, msgs) {
						add(call, name, method+" of a message struct")
					} else if shape := messageMapShape(arg, status, computed, made, params); shape != "" {
						add(call, name, method+" of "+shape)
					}
				}
				return true
			})
		}
	}
	return out
}

func messageStatusCensus(writes []messageStatusWrite) []string {
	got := []string{}
	for _, w := range writes {
		got = append(got, w.fn+": "+w.shape)
	}
	sort.Strings(got)
	return got
}

func TestMessageStatusHasOneWriter(t *testing.T) {
	parsed := guardkit.Production(t).Parse(0)
	for _, p := range parsed.Problems {
		t.Error(p)
	}
	writes := messageStatusWrites(parsed)
	if got := messageStatusCensus(writes); !reflect.DeepEqual(got, messageStatusWriterCensus) {
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
		{"a cancel as it was written", `package main
func (a *App) cancel(tx *gorm.DB, m *models.Message) error {
	return tx.Model(&models.Message{}).Where("id = ? AND status IN ?", m.ID, []string{"draft", "scheduled"}).
		Updates(map[string]any{"status": models.MessageCancelled, "updated_at": time.Now()}).Error
}`, `cancel: Updates of a map with "status"`},
		{"a query held in a variable, as the refusal was", `package main
func (a *App) refuse(tx *gorm.DB, id uint) error {
	q := tx.Model(&models.Message{}).Where("id = ?", id)
	q = q.Where("updated_at = ?", time.Now())
	return q.Updates(map[string]any{"status": models.MessageFailed}).Error
}`, `refuse: Updates of a map with "status"`},
		{"a map given status later, as the retry's was", `package main
func (a *App) retry(tx *gorm.DB, id uint) error {
	fields := map[string]any{"error": ""}
	fields["status"] = models.MessageSending
	return tx.Model(&models.Message{}).Where("id = ?", id).Updates(fields).Error
}`, `retry: Updates of a map given "status"`},
		{"a bare Update on a loaded message", `package main
func (a *App) send(tx *gorm.DB, id uint) error {
	m, _ := loadOwn(tx, id)
	return tx.Model(m).Update("status", models.MessageSent).Error
}` + decls, `send: Update("status")`},
		{"a range over messages", `package main
func (a *App) fail(tx *gorm.DB) error {
	var stuck []models.Message
	for _, m := range stuck {
		tx.Model(&m).UpdateColumn("status", models.MessageFailed)
	}
	return nil
}`, `fail: UpdateColumn("status")`},
		{"the table by name", `package main
func (a *App) fail(tx *gorm.DB, id uint) error {
	return tx.Table("messages").Where("id = ?", id).Update("status", "failed").Error
}`, `fail: Update("status")`},
		{"a column named at run time", `package main
func (a *App) set(tx *gorm.DB, m *models.Message, col string) error {
	return tx.Model(m).Update(col, "sent").Error
}`, "set: Update of a computed column"},
		{"a helper that writes its caller's columns", `package main
func writeMessage(tx *gorm.DB, id uint, cols map[string]any) error {
	return tx.Model(&models.Message{}).Where("id = ?", id).Updates(cols).Error
}`, "writeMessage: Updates of a parameter's map"},
		{"a saved message", `package main
func (a *App) send(tx *gorm.DB, m *models.Message) error {
	m.Status = models.MessageSent
	return tx.Save(m).Error
}`, "send: Save of a message"},
		{"a message struct as the update", `package main
func (a *App) send(tx *gorm.DB, id uint) error {
	return tx.Model(&models.Message{}).Where("id = ?", id).Updates(models.Message{Status: "sent"}).Error
}`, "send: Updates of a message struct"},
		{"a new message", `package main
func (a *App) draft(tx *gorm.DB) error {
	m := models.Message{Subject: "Hi"}
	return tx.Create(&m).Error
}`, "draft: Create of a message"},
		{"raw SQL", `package main
func (a *App) fail(tx *gorm.DB, id uint) error {
	return tx.Exec("UPDATE messages SET error = '', status = 'failed' WHERE id = ?", id).Error
}`, "fail: raw UPDATE of messages.status"},
		{"raw SQL in a constant", `package main
const failSQL = ` + "`UPDATE messages AS m SET \"status\" = 'failed' WHERE m.id = ?`" + `
`, "failSQL: raw UPDATE of messages.status"},
		{"raw SQL with its columns built at run time", `package main
func (a *App) set(tx *gorm.DB, cols string) error {
	return tx.Exec("UPDATE messages SET " + cols + " WHERE id = 1").Error
}`, "set: raw UPDATE messages with a column list built at run time"},
		{"raw SQL that names its status column at run time", `package main
func (a *App) set(tx *gorm.DB, col string) error {
	return tx.Exec("UPDATE messages SET error = '', " + col + " = 'sent' WHERE id = 1").Error
}`, "set: raw UPDATE messages with a column list built at run time"},
		{"a map whose key is computed", `package main
func (a *App) set(tx *gorm.DB, m *models.Message, col string) error {
	return tx.Model(m).Updates(map[string]any{col: "sent"}).Error
}`, "set: Updates of a map with computed keys"},
		{"a local map whose key is computed", `package main
func (a *App) set(tx *gorm.DB, m *models.Message, col string) error {
	cols := map[string]any{"error": "", col: "sent"}
	return tx.Model(m).Updates(cols).Error
}`, "set: Updates of a map with computed keys"},
		{"the field's name as the key", `package main
func (a *App) send(tx *gorm.DB, m *models.Message) error {
	return tx.Model(m).Updates(map[string]any{"Status": "sent"}).Error
}`, `send: Updates of a map with "status"`},
		{"the field's name as the column", `package main
func (a *App) send(tx *gorm.DB, m *models.Message) error {
	return tx.Model(m).Update("Status", "sent").Error
}`, `send: Update("status")`},
		{"a message made with new", `package main
func (a *App) send(tx *gorm.DB) error {
	m := new(models.Message)
	return tx.Model(m).Update("status", "sent").Error
}`, `send: Update("status")`},
		{"a selected field and a struct that is not a message", `package main
func (a *App) send(tx *gorm.DB, m *models.Message) error {
	return tx.Model(m).Select("Status").Updates(struct{ Status string }{"sent"}).Error
}`, "send: Updates of a value it cannot read"},
		{"a local map filled by another call", `package main
func (a *App) send(tx *gorm.DB, m *models.Message, cols map[string]any) error {
	fields := map[string]any{"error": ""}
	maps.Copy(fields, cols)
	return tx.Model(m).Updates(fields).Error
}`, "send: Updates of a map with computed keys"},
		{"a local map under a second name", `package main
func (a *App) send(tx *gorm.DB, m *models.Message) error {
	fields := map[string]any{"error": ""}
	extra := fields
	extra["status"] = "sent"
	return tx.Model(m).Updates(fields).Error
}`, "send: Updates of a map with computed keys"},
		{"a quoted identifier as the key", `package main
func (a *App) send(tx *gorm.DB, m *models.Message) error {
	return tx.Model(m).Updates(map[string]any{"\"status\"": "sent"}).Error
}`, "send: Updates of a map with computed keys"},
		{"a map built elsewhere", `package main
func (a *App) send(tx *gorm.DB, m *models.Message) error {
	cols := sentColumns()
	return tx.Model(m).Updates(cols).Error
}`, "send: Updates of a map with computed keys"},
		{"a raw insert", `package main
func (a *App) draft(tx *gorm.DB) error {
	return tx.Exec("INSERT INTO messages (subject) VALUES ('Hi')").Error
}`, "draft: raw INSERT INTO messages"},
		{"another table's status", `package main
func (a *App) mark(tx *gorm.DB, m *models.Membership, id uint) error {
	tx.Model(&m).Updates(map[string]any{"status": "active"})
	tx.Model(&models.MessageRecipient{}).Where("message_id = ?", id).Updates(map[string]any{"status": "failed"})
	tx.Exec("UPDATE message_recipients SET status = 'suppressed' WHERE message_id = ?", id)
	return tx.Create(&models.MessageRecipient{MessageID: id}).Error
}`, ""},
		{"another messages column", `package main
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
			p := guardkit.Sources{"x.go": c.src}.Parse(0)
			if len(p.Problems) > 0 {
				t.Fatal(p.Problems)
			}
			got := strings.Join(messageStatusCensus(messageStatusWrites(p)), "\n")
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
