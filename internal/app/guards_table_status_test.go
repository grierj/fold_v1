package app

import (
	"go/ast"
	"go/token"
	"regexp"
	"sort"
	"strings"

	"fold/internal/guardkit"

	"gorm.io/gorm/schema"
)

// tableStatusGuard finds status writes. Updates must identify the columns
// they write with literal names or a local map whose keys the guard can read.
// It parses declarations and GORM calls without running a type checker.
// Dynamic SQL, conflict clauses, pre-scoped database handles and model
// values returned by undeclared functions are outside its coverage.
type tableStatusGuard struct {
	model, table, noun string
	updateRe, insertRe *regexp.Regexp
	updateHelper       string
	skipInserts        bool
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
	tableStatusPlainColumnRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	tableStatusSetsStatusRe  = regexp.MustCompile(`(?i)(?:^|[\s,."])status"?\s*=`)
)

type tableStatusWrite struct{ at, fn, shape string }

func (g tableStatusGuard) value(e ast.Expr, bindings modelBindings, fields tableStatusFields) bool {
	e = tableStatusIndirect(e)
	return exprKind(e, g.model, bindings, nil) != notModel ||
		(len(fields) > 0 && typeKind(tableStatusExprType(e, fields), g.model) != notModel)
}

func tableStatusIndirect(e ast.Expr) ast.Expr {
	for {
		switch v := guardkit.Unparen(e).(type) {
		case *ast.StarExpr:
			e = v.X
		case *ast.UnaryExpr:
			if v.Op != token.AND {
				return v
			}
			e = v.X
		default:
			return v
		}
	}
}

// tableStatusExprType follows declarations, indexing into typed collections,
// and direct model fields of named structs.
func tableStatusExprType(e ast.Expr, fields tableStatusFields) ast.Expr {
	switch v := tableStatusIndirect(e).(type) {
	case *ast.CompositeLit:
		return v.Type
	case *ast.IndexExpr:
		switch t := tableStatusIndirect(tableStatusExprType(v.X, fields)).(type) {
		case *ast.ArrayType:
			return t.Elt
		case *ast.MapType:
			return t.Value
		}
	case *ast.SelectorExpr:
		if owner, ok := tableStatusIndirect(tableStatusExprType(v.X, fields)).(*ast.Ident); ok {
			return fields[owner.Name][v.Sel.Name]
		}
	case *ast.Ident:
		if v.Obj == nil {
			return nil
		}
		switch d := v.Obj.Decl.(type) {
		case *ast.Field:
			return d.Type
		case *ast.AssignStmt:
			if len(d.Lhs) == len(d.Rhs) {
				for i, lhs := range d.Lhs {
					if id, ok := lhs.(*ast.Ident); ok && id.Obj == v.Obj {
						return tableStatusExprType(d.Rhs[i], fields)
					}
				}
			}
		case *ast.ValueSpec:
			return d.Type
		}
	}
	return nil
}

type tableStatusPackage struct{ dir, name string }

type tableStatusFields map[string]map[string]ast.Expr

func (g tableStatusGuard) structFields(p *guardkit.Parsed) map[tableStatusPackage]tableStatusFields {
	packages := map[tableStatusPackage]tableStatusFields{}
	for _, f := range p.Files {
		key := tableStatusPackage{f.Dir(), f.AST.Name.Name}
		for _, d := range f.AST.Decls {
			decl, ok := d.(*ast.GenDecl)
			if !ok || decl.Tok != token.TYPE {
				continue
			}
			for _, spec := range decl.Specs {
				typ := spec.(*ast.TypeSpec)
				st, ok := typ.Type.(*ast.StructType)
				if !ok {
					continue
				}
				for _, field := range st.Fields.List {
					if typeKind(field.Type, g.model) == notModel {
						continue
					}
					if packages[key] == nil {
						packages[key] = tableStatusFields{}
					}
					if packages[key][typ.Name.Name] == nil {
						packages[key][typ.Name.Name] = map[string]ast.Expr{}
					}
					for _, name := range field.Names {
						packages[key][typ.Name.Name][name.Name] = field.Type
					}
				}
			}
		}
	}
	return packages
}

func tableStatusChains(fd *ast.FuncDecl, find func(ast.Expr, map[string]bool) bool) map[string]bool {
	chains := map[string]bool{}
	for changed := true; changed; {
		changed = false
		bind := func(name string, e ast.Expr) {
			if name != "_" && !chains[name] && find(e, chains) {
				chains[name] = true
				changed = true
			}
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.ValueSpec:
				if len(v.Values) == len(v.Names) {
					for i, name := range v.Names {
						bind(name.Name, v.Values[i])
					}
				}
			case *ast.AssignStmt:
				if len(v.Lhs) == len(v.Rhs) {
					for i, lhs := range v.Lhs {
						if id, ok := lhs.(*ast.Ident); ok {
							bind(id.Name, v.Rhs[i])
						}
					}
				}
			}
			return true
		})
	}
	return chains
}

// chain reports whether a gorm call chain names the table.
func (g tableStatusGuard) chain(x ast.Expr, value func(ast.Expr) bool, chains map[string]bool) bool {
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
				if len(v.Args) == 1 && value(v.Args[0]) {
					return true
				}
			case "Table":
				if len(v.Args) > 0 {
					if s, ok := stringLit(v.Args[0]); ok {
						parts := strings.Fields(s)
						if len(parts) > 0 && strings.TrimPrefix(strings.ReplaceAll(parts[0], `"`, ""), "public.") == g.table {
							return true
						}
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
func tableStatusKey(s string) bool { return (schema.NamingStrategy{}).ColumnName("", s) == "status" }

// tableStatusMapLit reads a map literal: whether it holds a status key, and
// whether it holds a key that is not a string literal.
func tableStatusMapLit(e ast.Expr) (status, computed bool) {
	lit, ok := tableStatusIndirect(e).(*ast.CompositeLit)
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
	switch v := tableStatusIndirect(e).(type) {
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
func (g tableStatusGuard) localMaps(fd *ast.FuncDecl) (status, computed, made, params map[string]bool) {
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
	// A local map stays readable while it is assigned, indexed, measured
	// with len, pruned with delete, or passed directly to an update.
	guardkit.InspectStack(fd.Body, func(n ast.Node, stack []ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok || !made[id.Name] || len(stack) < 2 {
			return true
		}
		parentIndex := len(stack) - 2
		if u, ok := stack[parentIndex].(*ast.UnaryExpr); ok && u.Op == token.AND && parentIndex > 0 {
			parentIndex--
		}
		switch parent := stack[parentIndex].(type) {
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
			if parent.X == ast.Expr(id) {
				return true
			}
		case *ast.CallExpr:
			if fn, ok := guardkit.Unparen(parent.Fun).(*ast.Ident); ok && fn.Obj == nil &&
				((fn.Name == "delete" && len(parent.Args) == 2) || (fn.Name == "len" && len(parent.Args) == 1)) &&
				parent.Args[0] == ast.Expr(id) {
				return true
			}
			if method := guardkit.Callee(parent); method == "Updates" || method == "UpdateColumns" || (g.updateHelper != "" && method == g.updateHelper) {
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
	id, ok := tableStatusIndirect(e).(*ast.Ident)
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
	funcs := modelFuncs(p, g.model)
	fields := g.structFields(p)
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
			if !g.skipInserts && g.insertRe.MatchString(s) {
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
			bindings := modelNames(fd, g.model, funcs)
			fieldTypes := fields[tableStatusPackage{f.Dir(), f.AST.Name.Name}]
			value := func(e ast.Expr) bool { return g.value(e, bindings, fieldTypes) }
			chains := tableStatusChains(fd, func(e ast.Expr, chains map[string]bool) bool {
				return g.chain(e, value, chains)
			})
			status, computed, made, params := g.localMaps(fd)
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if g.updateHelper != "" && guardkit.Callee(call) == g.updateHelper && len(call.Args) == 3 {
					columns := call.Args[2]
					if id, ok := columns.(*ast.Ident); !ok || id.Name != "nil" {
						if shape := tableStatusMapShape(columns, status, computed, made, params); shape != "" {
							add(call, name, g.updateHelper+" with "+shape)
						}
					}
					return true
				}
				sel, ok := guardkit.Unparen(call.Fun).(*ast.SelectorExpr)
				if !ok || len(call.Args) == 0 {
					return true
				}
				method, arg := sel.Sel.Name, call.Args[0]
				switch method {
				case "Create", "CreateInBatches", "FirstOrCreate", "Save":
					if (method == "Save" || !g.skipInserts) && value(arg) {
						verb := method
						if method != "Save" {
							verb = "Create"
						}
						add(call, name, verb+" of "+g.noun)
					}
					return true
				case "Updates", "UpdateColumns":
					if value(arg) {
						add(call, name, method+" of "+g.noun+" struct")
						return true
					}
				}
				if !g.chain(sel.X, value, chains) {
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
					if shape := tableStatusMapShape(arg, status, computed, made, params); shape != "" {
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
