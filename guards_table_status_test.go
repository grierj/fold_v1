package main

import (
	"go/ast"
	"go/token"
	"regexp"
	"sort"
	"strings"

	"fold/internal/guardkit"
)

// tableStatusGuard finds the writes of one table's status column: the
// machinery behind TestMessageStatusHasOneWriter and
// TestRecipientStatusHasOneWriter. Each guard's header states its rule and
// what it cannot see.
type tableStatusGuard struct {
	model, table, noun string
	updateRe, insertRe *regexp.Regexp
}

// newTableStatusGuard: model is the models type's name, table its table, and
// noun how the census names a value of it ("a message").
func newTableStatusGuard(model, table, noun string) tableStatusGuard {
	t := regexp.QuoteMeta(table)
	return tableStatusGuard{model: model, table: table, noun: noun,
		updateRe: regexp.MustCompile(`(?is)\bUPDATE\s+(?:ONLY\s+)?(?:public\.)?"?` + t + `"?(?:\s+(?:AS\s+)?\w+)?\s+SET\b(.*?)(\bWHERE\b|\bFROM\b|\bRETURNING\b|;|$)`),
		insertRe: regexp.MustCompile(`(?is)\bINSERT\s+INTO\s+(?:public\.)?"?` + t + `"?(?:\s|\(|$)`),
	}
}

var (
	tableStatusPlainColumnRe = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
	tableStatusSetsStatusRe  = regexp.MustCompile(`(?i)(?:^|[\s,."])status"?\s*=`)
)

type tableStatusWrite struct{ at, fn, shape string }

// typeExpr: models.<model>, a pointer to it, or a slice of either.
func (g tableStatusGuard) typeExpr(e ast.Expr) bool {
	switch v := guardkit.Unparen(e).(type) {
	case *ast.StarExpr:
		return g.typeExpr(v.X)
	case *ast.ArrayType:
		return g.typeExpr(v.Elt)
	case *ast.SelectorExpr:
		x, ok := v.X.(*ast.Ident)
		return ok && x.Name == "models" && v.Sel.Name == g.model
	}
	return false
}

// value: e is a literal of the model or a name bound to one, or the address
// or an element of one, or made by new or make.
func (g tableStatusGuard) value(e ast.Expr, names map[string]bool) bool {
	switch v := guardkit.Unparen(e).(type) {
	case *ast.CompositeLit:
		return g.typeExpr(v.Type)
	case *ast.UnaryExpr:
		return v.Op == token.AND && g.value(v.X, names)
	case *ast.StarExpr:
		return g.value(v.X, names)
	case *ast.IndexExpr:
		return g.value(v.X, names)
	case *ast.Ident:
		return names[v.Name]
	case *ast.CallExpr:
		fn, ok := v.Fun.(*ast.Ident)
		return ok && (fn.Name == "new" || fn.Name == "make") && len(v.Args) >= 1 && g.typeExpr(v.Args[0])
	}
	return false
}

// funcs is every declared function whose first result is a value of the
// model.
func (g tableStatusGuard) funcs(p *guardkit.Parsed) map[string]bool {
	out := map[string]bool{}
	for _, f := range p.Files {
		for _, d := range f.AST.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Type.Results != nil && len(fd.Type.Results.List) > 0 && g.typeExpr(fd.Type.Results.List[0].Type) {
				out[fd.Name.Name] = true
			}
		}
	}
	return out
}

// names is every name fd binds to a value of the model, and every name it
// assigns a chain over the table.
func (g tableStatusGuard) names(fd *ast.FuncDecl, funcs map[string]bool) (vals, chains map[string]bool) {
	vals, chains = map[string]bool{}, map[string]bool{}
	params := func(ft *ast.FuncType) {
		for _, fl := range []*ast.FieldList{ft.Params, ft.Results} {
			if fl == nil {
				continue
			}
			for _, f := range fl.List {
				if g.typeExpr(f.Type) {
					for _, n := range f.Names {
						vals[n.Name] = true
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
					if (v.Type != nil && g.typeExpr(v.Type)) || (i < len(v.Values) && g.value(v.Values[i], vals)) {
						vals[name.Name] = true
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
					if g.value(rhs, vals) {
						vals[id.Name] = true
					} else if call, ok := guardkit.Unparen(rhs).(*ast.CallExpr); ok && funcs[guardkit.Callee(call)] {
						vals[id.Name] = true
					} else if g.chain(rhs, vals, chains) {
						chains[id.Name] = true
					}
				}
			case *ast.RangeStmt:
				if g.value(v.X, vals) {
					if id, ok := v.Value.(*ast.Ident); ok {
						vals[id.Name] = true
					}
				}
			}
			return true
		})
	}
	return vals, chains
}

// chain reports whether a gorm call chain names the table.
func (g tableStatusGuard) chain(x ast.Expr, vals, chains map[string]bool) bool {
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
				if len(v.Args) == 1 && g.value(v.Args[0], vals) {
					return true
				}
			case "Table":
				if len(v.Args) > 0 {
					if s, ok := stringLit(v.Args[0]); ok && len(strings.Fields(s)) > 0 && strings.Fields(s)[0] == g.table {
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

// tableStatusKey: gorm resolves a column by its name or by the field's, so
// "Status" names the status column too.
func tableStatusKey(s string) bool { return strings.EqualFold(s, "status") }

// tableStatusMapLit reads a map literal: whether it holds a status key, and
// whether it holds a key that is not a string literal.
func tableStatusMapLit(e ast.Expr) (status, computed bool) {
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
		} else if tableStatusKey(s) {
			status = true
		} else if !tableStatusPlainColumnRe.MatchString(s) {
			// A quoted identifier or an expression: gorm writes it as given.
			computed = true
		}
	}
	return status, computed
}

// tableStatusMapMade: e makes a map the guard can read, a map literal or make.
func tableStatusMapMade(e ast.Expr) bool {
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

// tableStatusLocalMaps is what fd does to each local map: given a "status"
// key (in its literal or by assignment), or given a computed key; every name
// it binds to a map it makes itself; and every parameter name of fd and its
// function literals.
func tableStatusLocalMaps(fd *ast.FuncDecl) (status, computed, made, params map[string]bool) {
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
				switch {
				case len(v.Values) == len(v.Names):
					st, comp := tableStatusMapLit(v.Values[i])
					status[name.Name] = status[name.Name] || st
					if tableStatusMapMade(v.Values[i]) {
						made[name.Name] = true
					} else {
						comp = true
					}
					computed[name.Name] = computed[name.Name] || comp
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
						// One call fills several names.
						computed[lv.Name] = true
					} else {
						st, comp := tableStatusMapLit(v.Rhs[i])
						status[lv.Name] = status[lv.Name] || st
						computed[lv.Name] = computed[lv.Name] || comp
						if tableStatusMapMade(v.Rhs[i]) {
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
					} else if tableStatusKey(s) {
						status[x.Name] = true
					} else if !tableStatusPlainColumnRe.MatchString(s) {
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

// tableStatusMapShape names what an Updates argument is, or "" when the guard
// can read it and it leaves the status alone: a map literal, or a local map
// the function made, whose keys are all string literals other than status.
func tableStatusMapShape(e ast.Expr, status, computed, made, params map[string]bool) string {
	if st, comp := tableStatusMapLit(e); st {
		return `a map with "status"`
	} else if comp {
		return "a map with computed keys"
	} else if tableStatusMapMade(e) {
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

// writes finds every write of the table's status in p.
func (g tableStatusGuard) writes(p *guardkit.Parsed) []tableStatusWrite {
	var out []tableStatusWrite
	funcs := g.funcs(p)
	add := func(n ast.Node, fn, shape string) {
		out = append(out, tableStatusWrite{p.Fset.Position(n.Pos()).String(), fn, shape})
	}
	rawSQL := func(root ast.Node, fn string) {
		ast.Inspect(root, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok {
				return true
			}
			s, _ := stringLit(lit)
			if g.insertRe.MatchString(s) {
				add(n, fn, "raw INSERT INTO "+g.table)
			}
			for _, m := range g.updateRe.FindAllStringSubmatch(s, -1) {
				if tableStatusSetsStatusRe.MatchString(m[1]) {
					add(n, fn, "raw UPDATE of "+g.table+".status")
				} else if m[2] == "" {
					add(n, fn, "raw UPDATE "+g.table+" with a column list built at run time")
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
			vals, chains := g.names(fd, funcs)
			status, computed, made, params := tableStatusLocalMaps(fd)
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
					if g.value(arg, vals) {
						verb := method
						if method != "Save" {
							verb = "Create"
						}
						add(call, name, verb+" of "+g.noun)
					}
					return true
				}
				if !g.chain(sel.X, vals, chains) {
					return true
				}
				switch method {
				case "Update", "UpdateColumn":
					if s, lit := stringLit(arg); lit && tableStatusKey(s) {
						add(call, name, method+`("status")`)
					} else if !lit {
						add(call, name, method+" of a computed column")
					}
				case "Updates", "UpdateColumns":
					if g.value(arg, vals) {
						add(call, name, method+" of "+g.noun+" struct")
					} else if shape := tableStatusMapShape(arg, status, computed, made, params); shape != "" {
						add(call, name, method+" of "+shape)
					}
				}
				return true
			})
		}
	}
	return out
}

func tableStatusCensus(writes []tableStatusWrite) []string {
	got := []string{}
	for _, w := range writes {
		got = append(got, w.fn+": "+w.shape)
	}
	sort.Strings(got)
	return got
}
