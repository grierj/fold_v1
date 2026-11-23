package main

import (
	"go/ast"
	"go/token"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"fold/internal/guardkit"
)

// Repo guards from the /correct pass (Oct 2026). Each one fails on a real
// past mistake; see the rule table in AGENTS.md.

// groupDetailRe matches reading a group's name or place, the facts an
// invite-only group must not leak through its opened events (PR #41).
var groupDetailRe = regexp.MustCompile(`\b(g|f\.group|group)\.(Location|Name)\b|\br\.Group(Location|Name)\b`)

// eventOutputFiles build event and notice output for viewers who may not
// see the event's group.
var eventOutputFiles = []string{"api_occurrence_access.go", "api_events.go", "api_event_access.go", "api_notifications.go", "notices_groups.go", "notices_events.go", "api_group_visibility.go", "api_guest.go", "api_registrations.go", "api_calendar.go", "api_feeds.go", "date_projector.go"}

func groupDetailViolations(src string) []string {
	var bad []string
	for i, line := range strings.Split(src, "\n") {
		if groupDetailRe.MatchString(line) && !strings.Contains(line, "// group-ok:") {
			bad = append(bad, strings.TrimSpace(line)+" (line "+itoa(uint(i+1))+")")
		}
	}
	return bad
}

// rawGroupRe matches the raw sources of a group in the date projector: the
// unfiltered group of the viewer's facts and the loaded groups map. Neither may
// reach output; only a.groupShownTo's result may.
var rawGroupRe = regexp.MustCompile(`\bf\.group\b|\.Groups\[`)

// projectorFiles hold the one place a date's group is decided, so a raw group
// read there is checked line by line, not only by the field names above.
var projectorFiles = []string{"date_projector.go"}

// rawGroupViolations flags a raw group source on a line that neither passes it
// to groupShownTo (the filter) or asGroup (a policy input) nor says why it may
// be read with `// group-ok:`.
func rawGroupViolations(src string) []string {
	var bad []string
	for i, line := range strings.Split(src, "\n") {
		if !rawGroupRe.MatchString(line) || strings.Contains(line, "// group-ok:") ||
			strings.Contains(line, "groupShownTo(") || strings.Contains(line, "asGroup(") {
			continue
		}
		bad = append(bad, strings.TrimSpace(line)+" (line "+itoa(uint(i+1))+")")
	}
	return bad
}

func TestGroupDetailsGoThroughGroupShownTo(t *testing.T) {
	for _, f := range projectorFiles {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range rawGroupViolations(string(src)) {
			t.Errorf("%s: %s\n  a date's group is p.Group, set from a.groupShownTo only; "+
				"or end the line with `// group-ok: <why this viewer may see the group>`", f, v)
		}
	}
	for _, f := range eventOutputFiles {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range groupDetailViolations(string(src)) {
			t.Errorf("%s: %s\n  read the group through a.groupShownTo(me, g, rel) (nil when hidden), "+
				"or end the line with `// group-ok: <why this viewer may see the group>`", f, v)
		}
	}
}

// The guard catches the location leak fixed in 7b50f43 (PR #41).
func TestGroupDetailGuardCatchesPR41Leak(t *testing.T) {
	before := "\t\tif g != nil {\n\t\t\tloc = g.Location\n\t\t}\n"
	if len(groupDetailViolations(before)) != 1 {
		t.Fatal("guard missed the pre-#41 code")
	}
}

// The projector guard catches a group taken from the facts without the filter.
func TestRawGroupGuardCatchesUnfilteredGroup(t *testing.T) {
	for _, bad := range []string{"\tp.Group = f.group\n", "\tp.Group = vs.Groups[*ev.GroupID]\n"} {
		if len(rawGroupViolations(bad)) != 1 {
			t.Fatalf("guard missed %q", bad)
		}
	}
	for _, ok := range []string{"\tp.Group = a.groupShownTo(viewer, f.group, f.rel)\n", "\tasGroup(f.group)\n", "\tg := f.group // group-ok: manager only\n"} {
		if len(rawGroupViolations(ok)) != 0 {
			t.Fatalf("guard flagged %q", ok)
		}
	}
}

// --- One list filter for event visibility ---
//
// Event lists load candidates (dateCandidates or eventCandidates) then apply
// visibleSet.canSeeDate. Candidate parents never authorize a series, so no
// function loops over events calling policy.CanSeeEvent; the loader itself
// skips events whose group is missing.

func TestOneEventVisibilityFilter(t *testing.T) {
	parsed := guardkit.Production(t).Parse(0)
	for _, problem := range parsed.Problems {
		t.Error(problem)
	}
	fset := parsed.Fset
	for _, pf := range parsed.Files {
		f := pf.AST
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				var body *ast.BlockStmt
				switch l := n.(type) {
				case *ast.RangeStmt:
					body = l.Body
				case *ast.ForStmt:
					body = l.Body
				default:
					return true
				}
				ast.Inspect(body, func(m ast.Node) bool {
					if call, ok := m.(*ast.CallExpr); ok {
						if sel, ok := call.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "CanSeeEvent" || sel.Sel.Name == "canSee") {
							t.Errorf("%s: %s filters a list with CanSeeEvent (policy.CanSeeEvent or eventFacts.canSee); load candidates with dateCandidates or eventCandidates and filter dates with canSeeDate", fset.Position(call.Pos()), fn.Name.Name)
						}
					}
					return true
				})
				return false
			})
		}
	}
}

// Registration subjects must pass through the mixed holder scope; membership
// joins remain account-only.
func TestRegistrationQueriesUseSubjectScope(t *testing.T) {
	for file, src := range guardkit.Production(t) {
		if strings.Contains(src, `listedUsers("registrations")`) {
			t.Errorf("%s uses the account-only registration join", file)
		}
	}
}

// --- DEC-156: what 00014's trigger relies on ---
//
// The trigger refuses a raise that would pair a later end with a departure
// grant committed while the raise waited, but only for issuers that lock the
// date row before the grant goes live (G1), and the app never relies on it:
// every write of a date's end checks the kept grants first, inside a gated
// transaction (G2). Both guards are parse-only: they follow names, not types,
// within one function, so a value reached through a struct field, a map or a
// helper that returns it from another package is not seen.

// managerAddGrant reports whether a grantRegistrationAccess grant argument is
// a literal accessGrant whose Origin is policy.OccurrenceOriginManagerAdd.
// Anything else (a variable origin, a departure constant, a value built
// elsewhere) may be a departure grant.
func managerAddGrant(e ast.Expr) bool {
	e = guardkit.Unparen(e)
	if u, ok := e.(*ast.UnaryExpr); ok {
		e = u.X
	}
	lit, ok := e.(*ast.CompositeLit)
	if !ok {
		return false
	}
	if id, ok := lit.Type.(*ast.Ident); !ok || id.Name != "accessGrant" {
		return false
	}
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Origin" {
			sel, ok := guardkit.Unparen(kv.Value).(*ast.SelectorExpr)
			x, _ := sel.X.(*ast.Ident)
			return ok && x != nil && x.Name == "policy" && sel.Sel.Name == "OccurrenceOriginManagerAdd"
		}
	}
	return false
}

// innermostBody is the closest function literal or declaration in stack.
func innermostBody(stack []ast.Node) ast.Node {
	for i := len(stack) - 1; i >= 0; i-- {
		switch stack[i].(type) {
		case *ast.FuncLit, *ast.FuncDecl:
			return stack[i]
		}
	}
	return nil
}

// callSite is one call of a named function: the declaration it is in, the
// closest function body around it and where.
type callSite struct {
	fn   string
	body ast.Node
	pos  token.Pos
	at   string
}

// callSites lists every call of each name in names across p.
func callSites(p *guardkit.Parsed, names ...string) map[string][]callSite {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	out := map[string][]callSite{}
	for _, f := range p.Files {
		for _, d := range f.AST.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			guardkit.InspectStack(fd, func(n ast.Node, stack []ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok && want[guardkit.Callee(call)] {
					name := guardkit.Callee(call)
					out[name] = append(out[name], callSite{fn: fd.Name.Name, body: innermostBody(stack), pos: call.Pos(), at: p.Fset.Position(call.Pos()).String()})
				}
				return true
			})
		}
	}
	return out
}

// departureGrantViolations is G1: a grant that may carry a departure origin is
// written only by recordDepartureOccurrenceAccess, whose only caller is
// applyDeparture, whose only caller is departureConfirm, and there only after
// lockDepartureOccurrences in the same transaction body.
func departureGrantViolations(p *guardkit.Parsed) []string {
	bad := append([]string(nil), p.Problems...)
	for _, f := range p.Files {
		for _, d := range f.AST.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || guardkit.Callee(call) != "grantRegistrationAccess" {
					return true
				}
				if (len(call.Args) < 4 || !managerAddGrant(call.Args[3])) && fd.Name.Name != "recordDepartureOccurrenceAccess" {
					bad = append(bad, p.Fset.Position(call.Pos()).String()+": "+fd.Name.Name+" calls grantRegistrationAccess with a grant that may be a departure; only recordDepartureOccurrenceAccess may")
				}
				return true
			})
		}
	}
	sites := callSites(p, "recordDepartureOccurrenceAccess", "applyDeparture", "lockDepartureOccurrences")
	for callee, caller := range map[string]string{"recordDepartureOccurrenceAccess": "applyDeparture", "applyDeparture": "departureConfirm"} {
		if len(sites[callee]) == 0 {
			bad = append(bad, callee+" is never called; the guard no longer sees the departure path")
		}
		for _, s := range sites[callee] {
			if s.fn != caller {
				bad = append(bad, s.at+": "+s.fn+" calls "+callee+"; only "+caller+" may")
			}
		}
	}
	for _, apply := range sites["applyDeparture"] {
		locked := false
		for _, lock := range sites["lockDepartureOccurrences"] {
			if lock.body == apply.body && lock.pos < apply.pos {
				locked = true
			}
		}
		if !locked {
			bad = append(bad, apply.at+": applyDeparture runs without lockDepartureOccurrences before it in the same transaction body")
		}
	}
	return bad
}

func TestDepartureGrantsAreIssuedUnderTheDateLock(t *testing.T) {
	for _, v := range departureGrantViolations(guardkit.Production(t).Parse(0)) {
		t.Error(v)
	}
}

func TestDepartureGrantGuardCatchesUnlockedIssuers(t *testing.T) {
	const tail = `
func recordDepartureOccurrenceAccess(tx *gorm.DB, facts departureFacts) error {
	_, _, err := grantRegistrationAccess(tx, 1, scope, accessGrant{Origin: facts.Origin}, nil, now, "")
	return err
}
func (a *App) applyDeparture(tx *gorm.DB) error { return recordDepartureOccurrenceAccess(tx, facts) }
func (a *App) addByManager(tx *gorm.DB) {
	grantRegistrationAccess(tx, 1, scope, accessGrant{Origin: policy.OccurrenceOriginManagerAdd}, nil, now, "")
}
`
	for _, c := range []struct {
		name, src string
		want      string
	}{
		{"locked first", `package main
func (a *App) departureConfirm() {
	a.contactMutation(change, func(tx *gorm.DB) error {
		locked, _ := lockDepartureOccurrences(tx, facts)
		return a.applyDeparture(tx, locked)
	})
}` + tail, ""},
		{"applied before the lock", `package main
func (a *App) departureConfirm() {
	a.contactMutation(change, func(tx *gorm.DB) error {
		err := a.applyDeparture(tx, nil)
		lockDepartureOccurrences(tx, facts)
		return err
	})
}` + tail, "without lockDepartureOccurrences"},
		{"a second issuer", `package main
func (a *App) departureConfirm() {
	a.contactMutation(change, func(tx *gorm.DB) error {
		lockDepartureOccurrences(tx, facts)
		return a.applyDeparture(tx, nil)
	})
}
func (a *App) rawKeep(tx *gorm.DB) {
	grantRegistrationAccess(tx, 1, scope, accessGrant{Origin: policy.OccurrenceOriginDepartureSelf}, nil, now, "")
}` + tail, "rawKeep calls grantRegistrationAccess"},
		{"another caller of applyDeparture", `package main
func (a *App) departureConfirm() {
	a.contactMutation(change, func(tx *gorm.DB) error {
		lockDepartureOccurrences(tx, facts)
		return a.applyDeparture(tx, nil)
	})
}
func (a *App) bulkDepart(tx *gorm.DB) { a.applyDeparture(tx, nil) }` + tail, "bulkDepart calls applyDeparture"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := strings.Join(departureGrantViolations(guardkit.Sources{"fixture.go": c.src}.Parse(0)), "\n")
			if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
				t.Fatalf("violations %q, want one containing %q", got, c.want)
			}
		})
	}
}

// updateSetRe finds raw SQL that updates event_occurrences, with or without
// an alias, and captures its SET list (up to WHERE, FROM, RETURNING or the
// end); endsAtColumnRe then asks whether that list names ends_at.
var (
	updateSetRe    = regexp.MustCompile(`(?is)\bupdate\s+(?:only\s+)?(?:public\.)?"?event_occurrences"?(?:\s+(?:as\s+)?\w+)?\s+set\s+(.*?)(?:\bwhere\b|\bfrom\b|\breturning\b|;|$)`)
	endsAtColumnRe = regexp.MustCompile(`(?i)\bends_at\b`)
)

// endsAtWrite is one statement that writes a date's end: where, the function
// declaring it, and its shape.
type endsAtWrite struct {
	at, fn, shape string
	body          ast.Node
	pos           token.Pos
	stack         []ast.Node
}

// modelBindings is what a function binds to one GORM model, models.<model>:
// names holding a value (the model or a pointer to it) and names holding a
// collection (a slice or map of either). For functions it is those whose first
// result is one.
type modelBindings struct{ values, collections map[string]bool }

// modelValue reports whether e is a models.<model> value: a name in b.values,
// an element of a name in b.collections, or a composite literal of the type,
// behind &, * or neither.
func modelValue(e ast.Expr, model string, b modelBindings) bool {
	e = guardkit.Unparen(e)
	if u, ok := e.(*ast.UnaryExpr); ok {
		e = guardkit.Unparen(u.X)
	}
	if s, ok := e.(*ast.StarExpr); ok {
		e = guardkit.Unparen(s.X)
	}
	switch v := e.(type) {
	case *ast.Ident:
		return b.values[v.Name]
	case *ast.CompositeLit:
		return modelType(v.Type, model)
	case *ast.IndexExpr:
		x, ok := guardkit.Unparen(v.X).(*ast.Ident)
		return ok && b.collections[x.Name]
	}
	return false
}

func modelType(e ast.Expr, model string) bool {
	if s, ok := e.(*ast.StarExpr); ok {
		e = s.X
	}
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && x.Name == "models" && sel.Sel.Name == model
}

func modelCollectionType(e ast.Expr, model string) bool {
	switch t := e.(type) {
	case *ast.ArrayType:
		return modelType(t.Elt, model)
	case *ast.MapType:
		return modelType(t.Value, model)
	}
	return false
}

// modelFuncs is every declared function in p whose first result is a
// models.<model> value or a collection of them.
func modelFuncs(p *guardkit.Parsed, model string) modelBindings {
	out := modelBindings{values: map[string]bool{}, collections: map[string]bool{}}
	for _, f := range p.Files {
		for _, d := range f.AST.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Type.Results == nil || len(fd.Type.Results.List) == 0 {
				continue
			}
			if first := fd.Type.Results.List[0].Type; modelType(first, model) {
				out.values[fd.Name.Name] = true
			} else if modelCollectionType(first, model) {
				out.collections[fd.Name.Name] = true
			}
		}
	}
	return out
}

// modelNames is every name fd binds to a models.<model> value or collection:
// its parameters and those of its function literals, var declarations, := of
// a literal, of another such name or element, of a call to one of funcs, and
// range values over a collection.
func modelNames(fd *ast.FuncDecl, model string, funcs modelBindings) modelBindings {
	b := modelBindings{values: map[string]bool{}, collections: map[string]bool{}}
	changed := false
	mark := func(set map[string]bool, name string) {
		if name != "_" && !set[name] {
			set[name] = true
			changed = true
		}
	}
	typed := func(name string, t ast.Expr) {
		if modelType(t, model) {
			mark(b.values, name)
		} else if modelCollectionType(t, model) {
			mark(b.collections, name)
		}
	}
	params := func(fl *ast.FieldList) {
		if fl == nil {
			return
		}
		for _, field := range fl.List {
			for _, n := range field.Names {
				typed(n.Name, field.Type)
			}
		}
	}
	params(fd.Type.Params)
	for changed = true; changed; {
		changed = false
		ast.Inspect(fd, func(n ast.Node) bool {
			switch s := n.(type) {
			case *ast.FuncLit:
				params(s.Type.Params)
			case *ast.ValueSpec:
				for _, name := range s.Names {
					if s.Type != nil {
						typed(name.Name, s.Type)
					}
				}
			case *ast.AssignStmt:
				if len(s.Rhs) == 0 {
					return true
				}
				id, ok := s.Lhs[0].(*ast.Ident)
				if !ok {
					return true
				}
				rhs := guardkit.Unparen(s.Rhs[0])
				if call, ok := rhs.(*ast.CallExpr); ok && funcs.values[guardkit.Callee(call)] {
					mark(b.values, id.Name)
				} else if ok && funcs.collections[guardkit.Callee(call)] {
					mark(b.collections, id.Name)
				} else if len(s.Lhs) == len(s.Rhs) && modelValue(rhs, model, b) {
					mark(b.values, id.Name)
				}
			case *ast.RangeStmt:
				if x, ok := guardkit.Unparen(s.X).(*ast.Ident); ok && b.collections[x.Name] {
					if v, ok := s.Value.(*ast.Ident); ok {
						mark(b.values, v.Name)
					}
				}
			}
			return true
		})
	}
	return b
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := guardkit.Unparen(e).(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}

// endsAtWrites finds every write of event_occurrences.ends_at in p: a Save,
// Updates or UpdateColumns of a models.EventOccurrence value (each writes the
// whole row or its set fields), an Update or UpdateColumn of "ends_at", a map
// literal or map assignment keyed "ends_at", and raw SQL that sets ends_at on
// event_occurrences.
func endsAtWrites(p *guardkit.Parsed) []endsAtWrite {
	occFuncs := modelFuncs(p, "EventOccurrence")
	var out []endsAtWrite
	for _, f := range p.Files {
		for _, d := range f.AST.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			occ := modelNames(fd, "EventOccurrence", occFuncs)
			guardkit.InspectStack(fd, func(n ast.Node, stack []ast.Node) bool {
				shape := ""
				switch v := n.(type) {
				case *ast.CallExpr:
					switch guardkit.Callee(v) {
					case "Save", "Updates", "UpdateColumns":
						if len(v.Args) == 1 && modelValue(v.Args[0], "EventOccurrence", occ) {
							shape = guardkit.Callee(v) + " of a date"
						}
					case "Update", "UpdateColumn":
						if len(v.Args) == 2 {
							if s, ok := stringLit(v.Args[0]); ok && s == "ends_at" {
								shape = guardkit.Callee(v) + `("ends_at")`
							}
						}
					}
				case *ast.KeyValueExpr:
					if s, ok := stringLit(v.Key); ok && s == "ends_at" {
						shape = `map key "ends_at"`
					}
				case *ast.AssignStmt:
					for _, l := range v.Lhs {
						if ix, ok := l.(*ast.IndexExpr); ok {
							if s, ok := stringLit(ix.Index); ok && s == "ends_at" {
								shape = `map assignment "ends_at"`
							}
						}
					}
				case *ast.BasicLit:
					if s, ok := stringLit(v); ok {
						for _, m := range updateSetRe.FindAllStringSubmatch(s, -1) {
							if endsAtColumnRe.MatchString(m[1]) {
								shape = "raw UPDATE of ends_at"
							}
						}
					}
				}
				if shape != "" {
					out = append(out, endsAtWrite{at: p.Fset.Position(n.Pos()).String(), fn: fd.Name.Name, shape: shape,
						body: innermostBody(stack), pos: n.Pos(), stack: append([]ast.Node(nil), stack...)})
				}
				return true
			})
		}
	}
	return out
}

// gatedBody reports whether body is a function literal passed straight to
// contactMutation or singleDateMutation, given the stack that reaches it.
func gatedBody(body ast.Node, stack []ast.Node) bool {
	for i := len(stack) - 1; i > 0; i-- {
		if stack[i] != body {
			continue
		}
		call, ok := stack[i-1].(*ast.CallExpr)
		if !ok {
			return false
		}
		name := guardkit.Callee(call)
		return name == "contactMutation" || name == "singleDateMutation"
	}
	return false
}

// endsAtWriterViolations is G2's rule for each write: the same transaction
// body counts the kept grants (refuseKeptDates, or keptDatesIn itself) before
// it, and that body is a contactMutation or singleDateMutation callback, or a
// function every caller of which calls it from one. refuseKeptDates must count
// with keptDatesIn.
func endsAtWriterViolations(p *guardkit.Parsed, writes []endsAtWrite) []string {
	bad := append([]string(nil), p.Problems...)
	sites := callSites(p, "refuseKeptDates", "keptDatesIn")
	checked := func(w endsAtWrite) bool {
		for _, name := range []string{"refuseKeptDates", "keptDatesIn"} {
			for _, s := range sites[name] {
				if s.body == w.body && s.pos < w.pos {
					return true
				}
			}
		}
		return false
	}
	// Every call of a declared function, with the stack that reaches it, so a
	// writer declared on its own is judged by where it is called.
	type caller struct {
		at    string
		stack []ast.Node
	}
	callers := map[string][]caller{}
	for _, f := range p.Files {
		for _, d := range f.AST.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
				guardkit.InspectStack(fd, func(n ast.Node, stack []ast.Node) bool {
					if call, ok := n.(*ast.CallExpr); ok {
						name := guardkit.Callee(call)
						callers[name] = append(callers[name], caller{p.Fset.Position(call.Pos()).String(), append([]ast.Node(nil), stack...)})
					}
					return true
				})
			}
		}
	}
	for _, w := range writes {
		if !checked(w) {
			bad = append(bad, w.at+": "+w.fn+" writes a date's end ("+w.shape+") without refuseKeptDates or keptDatesIn before it in the same body")
		}
		switch body := w.body.(type) {
		case *ast.FuncLit:
			if !gatedBody(body, w.stack) {
				bad = append(bad, w.at+": "+w.fn+" writes a date's end outside a contactMutation or singleDateMutation callback")
			}
		case *ast.FuncDecl:
			cs := callers[body.Name.Name]
			if len(cs) == 0 {
				bad = append(bad, w.at+": "+w.fn+" writes a date's end and is never called")
			}
			for _, c := range cs {
				if outer := innermostBody(c.stack); outer == nil || !gatedBody(outer, c.stack) {
					bad = append(bad, c.at+": "+w.fn+", which writes a date's end, is called outside a contactMutation or singleDateMutation callback")
				}
			}
		}
	}
	for _, f := range p.Files {
		if fd := guardkit.FindFunc(f.AST, "refuseKeptDates"); fd != nil {
			counts := false
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok && guardkit.Callee(call) == "keptDatesIn" {
					counts = true
				}
				return true
			})
			if !counts {
				bad = append(bad, f.Path+": refuseKeptDates no longer counts with keptDatesIn")
			}
		}
	}
	return bad
}

// endsAtWriterCensus is the writes of a date's end design section 6b names
// (DEC-156): the guarded single-date save, and the series edit's move and
// retimed restore.
var endsAtWriterCensus = []string{
	"apiUpdateEvent: map assignment \"ends_at\"",
	"apiUpdateEvent: map key \"ends_at\"",
	"editDateIn: Save of a date",
}

func TestEndsAtWritersCheckKeptDates(t *testing.T) {
	parsed := guardkit.Production(t).Parse(0)
	writes := endsAtWrites(parsed)
	var got []string
	for _, w := range writes {
		got = append(got, w.fn+": "+w.shape)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, endsAtWriterCensus) {
		var where []string
		for _, w := range writes {
			where = append(where, w.at+" "+w.fn+": "+w.shape)
		}
		t.Errorf("the writes of a date's end are\n%s\nwant exactly %v; a new writer must check kept dates (refuseKeptDates) under the gate and join the census", strings.Join(where, "\n"), endsAtWriterCensus)
	}
	for _, v := range endsAtWriterViolations(parsed, writes) {
		t.Error(v)
	}
}

func TestEndsAtWriterGuardCatchesUncheckedWriters(t *testing.T) {
	const helpers = `
func refuseKeptDates(tx *gorm.DB, raised map[uint]string) error { _, err := keptDatesIn(tx, nil); return err }
`
	for _, c := range []struct {
		name, src  string
		shape, bad string
	}{
		{"checked under the gate", `package main
func (a *App) move(id uint) error {
	return a.contactMutation(change, func(tx *gorm.DB) error {
		var o models.EventOccurrence
		next := o
		if err := refuseKeptDates(tx, nil); err != nil { return err }
		o = next
		return tx.Save(&o).Error
	})
}` + helpers, "Save of a date", ""},
		{"no kept check", `package main
func (a *App) move(id uint) error {
	return a.contactMutation(change, func(tx *gorm.DB) error {
		return tx.Model(&models.EventOccurrence{}).Where("id = ?", id).Update("ends_at", later).Error
	})
}` + helpers, `Update("ends_at")`, "without refuseKeptDates"},
		{"checked after the write", `package main
func (a *App) move(id uint) error {
	return a.singleDateMutation(id, func(tx *gorm.DB, d *singleDate) error {
		tx.Exec("UPDATE event_occurrences SET ends_at = ? WHERE id = ?", later, id)
		return refuseKeptDates(tx, nil)
	})
}` + helpers, "raw UPDATE of ends_at", "without refuseKeptDates"},
		{"every row, aliased", `package main
func (a *App) shift() error {
	return a.contactMutation(change, func(tx *gorm.DB) error {
		return tx.Exec(` + "`" + `UPDATE event_occurrences AS o SET starts_at = o.starts_at + interval '1 day', "ends_at" = o.ends_at + interval '1 day'` + "`" + `).Error
	})
}` + helpers, "raw UPDATE of ends_at", "without refuseKeptDates"},
		{"a receipt write is not an end", `package main
func (a *App) move(id uint) error {
	return a.contactMutation(change, func(tx *gorm.DB) error {
		if err := refuseKeptDates(tx, nil); err != nil { return err }
		tx.Exec("UPDATE event_occurrences SET edit_receipt = ? WHERE ends_at > now()", nil)
		return tx.Model(&models.EventOccurrence{}).Where("id = ?", id).UpdateColumn("ends_at", later).Error
	})
}` + helpers, `UpdateColumn("ends_at")`, ""},
		{"outside the gate", `package main
func (a *App) move(id uint) error {
	return a.DB.Transaction(func(tx *gorm.DB) error {
		if err := refuseKeptDates(tx, nil); err != nil { return err }
		return tx.Model(&models.EventOccurrence{}).Where("id = ?", id).Updates(map[string]any{"ends_at": later}).Error
	})
}` + helpers, `map key "ends_at"`, "outside a contactMutation"},
		{"a declared writer called outside the gate", `package main
func (a *App) moveIn(tx *gorm.DB, o *models.EventOccurrence) error {
	if err := refuseKeptDates(tx, nil); err != nil { return err }
	return tx.Save(o).Error
}
func (a *App) handler(o *models.EventOccurrence) { a.moveIn(a.DB, o) }` + helpers, "Save of a date", "is called outside"},
		{"a date read back from a helper", `package main
func loadDate(tx *gorm.DB) (*models.EventOccurrence, error) { return nil, nil }
func (a *App) move() error {
	return a.contactMutation(change, func(tx *gorm.DB) error {
		o, _ := loadDate(tx)
		return tx.Save(o).Error
	})
}` + helpers, "Save of a date", "without refuseKeptDates"},
		{"a refusal that no longer counts", `package main
func refuseKeptDates(tx *gorm.DB, raised map[uint]string) error { return nil }
func (a *App) move(id uint) error {
	return a.contactMutation(change, func(tx *gorm.DB) error {
		if err := refuseKeptDates(tx, nil); err != nil { return err }
		return tx.Model(&models.EventOccurrence{}).Where("id = ?", id).UpdateColumn("ends_at", later).Error
	})
}`, `UpdateColumn("ends_at")`, "no longer counts"},
	} {
		t.Run(c.name, func(t *testing.T) {
			parsed := guardkit.Sources{"fixture.go": c.src}.Parse(0)
			writes := endsAtWrites(parsed)
			if len(writes) != 1 || writes[0].shape != c.shape {
				t.Fatalf("writes %+v, want one %q", writes, c.shape)
			}
			got := strings.Join(endsAtWriterViolations(parsed, writes), "\n")
			if (c.bad == "") != (got == "") || !strings.Contains(got, c.bad) {
				t.Fatalf("violations %q, want one containing %q", got, c.bad)
			}
		})
	}
}
