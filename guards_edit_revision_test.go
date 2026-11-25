package main

import (
	"fmt"
	"go/ast"
	"slices"
	"strings"
	"testing"

	"fold/internal/guardkit"
)

// ---------------------------------------------------------------------------
// A manager's save names the read it was made from (DEC-162, DEC-163): each
// handler that takes a revision compares it with the shared check
// (edit_revision.go) inside its write transaction, under the row lock, so a
// refusal writes nothing.
//
// Parse-only, root package. A check is a call named in editRevisionChecks
// outside edit_revision.go. It is inside the transaction when
// the nearest function literal around it is an argument of a call named
// contactMutation or Transaction. The guard does not see a check reached
// through a helper the handler calls, a transaction opened another way, or
// whether the call comes after the row lock; the handlers' tests cover that.
// A check that reads its row itself (editRevisionReaders) must get that
// function literal's transaction as its first argument: the handle outside it
// would read a row the lock does not hold.
// ---------------------------------------------------------------------------

// editRevisionCheckers is every handler that checks a save's revision, exactly.
var editRevisionCheckers = []string{"apiScheduleMessage", "apiSetDateRoom", "apiSetEventVisibility", "apiSetSeriesRoom", "apiUpdateEvent", "apiUpdateGroup", "apiUpdateMessage"}

var editRevisionChecks = map[string]bool{"checkEventRevision": true, "checkGroupRevision": true, "checkDateRoomRevision": true, "checkMessageRevision": true, "checkEditRevision": true}

// editRevisionReaders are the checks that read their row on the handle given.
var editRevisionReaders = map[string]bool{"checkGroupRevision": true, "checkDateRoomRevision": true, "checkMessageRevision": true}

var editRevisionTransactions = map[string]bool{"contactMutation": true, "Transaction": true}

// editRevisionCensus is the sorted handlers whose checks all sit in a write
// transaction, and a problem for each check that does not.
func editRevisionCensus(p *guardkit.Parsed) (checkers, bad []string) {
	bad = append(bad, p.Problems...)
	for _, f := range p.Package("main") {
		if f.Path == "edit_revision.go" {
			continue
		}
		for _, d := range f.AST.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			guardkit.InspectStack(fd.Body, func(n ast.Node, stack []ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || !editRevisionChecks[guardkit.Callee(call)] {
					return true
				}
				if editRevisionReaders[guardkit.Callee(call)] && !readsThroughItsTransaction(call, stack) {
					bad = append(bad, fmt.Sprintf("%s:%d: %s reads the row for its revision check outside the transaction's handle", f.Path, p.Fset.Position(call.Pos()).Line, fd.Name.Name))
				}
				if inWriteTransaction(stack) {
					if !slices.Contains(checkers, fd.Name.Name) {
						checkers = append(checkers, fd.Name.Name)
					}
				} else {
					bad = append(bad, fmt.Sprintf("%s:%d: %s checks a revision outside its write transaction", f.Path, p.Fset.Position(call.Pos()).Line, fd.Name.Name))
				}
				return true
			})
		}
	}
	slices.Sort(checkers)
	return checkers, bad
}

// inWriteTransaction is whether the nearest function literal in stack (the
// call's ancestors, the call last) is passed to a transaction.
func inWriteTransaction(stack []ast.Node) bool {
	for i := len(stack) - 2; i > 0; i-- {
		lit, ok := stack[i].(*ast.FuncLit)
		if !ok {
			continue
		}
		outer, ok := stack[i-1].(*ast.CallExpr)
		return ok && editRevisionTransactions[guardkit.Callee(outer)] && slices.Contains(outer.Args, ast.Expr(lit))
	}
	return false
}

// readsThroughItsTransaction is whether call's first argument is the first
// parameter of the nearest function literal around it.
func readsThroughItsTransaction(call *ast.CallExpr, stack []ast.Node) bool {
	for i := len(stack) - 2; i >= 0; i-- {
		lit, ok := stack[i].(*ast.FuncLit)
		if !ok {
			continue
		}
		params := lit.Type.Params.List
		if len(call.Args) == 0 || len(params) == 0 || len(params[0].Names) == 0 {
			return false
		}
		arg, ok := call.Args[0].(*ast.Ident)
		return ok && arg.Name == params[0].Names[0].Name
	}
	return false
}

func editRevisionProblems(p *guardkit.Parsed) []string {
	checkers, bad := editRevisionCensus(p)
	if !slices.Equal(checkers, editRevisionCheckers) {
		bad = append(bad, fmt.Sprintf("the handlers that check a revision in their write transaction are %v, want exactly %v (editRevisionCheckers)", checkers, editRevisionCheckers))
	}
	return bad
}

func TestSavesCheckTheirRevisionInTheirTransaction(t *testing.T) {
	if bad := editRevisionProblems(guardkit.Production(t).Parse(0)); len(bad) > 0 {
		t.Fatalf("revision checks (DEC-163):\n%v", bad)
	}
}

func TestEditRevisionGuardCatchesABypass(t *testing.T) {
	const visibility = `package main
func (a *App) apiSetEventVisibility(c *gin.Context) {
	a.contactMutation(contactChange{}, func(tx *gorm.DB) error {
		return a.checkEventRevision(locked, body.Revision)
	})
}
`
	const series = `package main
func (a *App) apiUpdateEvent(c *gin.Context) {
	err := a.contactMutation(contactChange{}, func(tx *gorm.DB) error {
		if err := a.checkEventRevision(lockedEv, body.Revision); err != nil {
			return err
		}
		return nil
	})
}
`
	const group = `package main
func (a *App) apiUpdateGroup(c *gin.Context) {
	err := a.contactMutation(contactChange{}, func(tx *gorm.DB) error {
		if err := a.checkGroupRevision(tx, locked.ID, body.Revision); err != nil {
			return err
		}
		return nil
	})
}
`
	const rooms = `package main
func (a *App) apiSetDateRoom(c *gin.Context) {
	err := a.DB.Transaction(func(tx *gorm.DB) error {
		if err := a.checkDateRoomRevision(tx, occ.ID, body.Revision); err != nil {
			return err
		}
		return nil
	})
}
func (a *App) apiSetSeriesRoom(c *gin.Context) {
	err := a.DB.Transaction(func(tx *gorm.DB) error {
		return a.checkEventRevision(locked, f.group, body.Revision)
	})
}
`
	const messages = `package main
func (a *App) apiUpdateMessage(c *gin.Context) {
	err := a.DB.Transaction(func(tx *gorm.DB) error {
		return a.checkMessageRevision(tx, m.ID, body.Revision)
	}, readCommitted)
}
func (a *App) apiScheduleMessage(c *gin.Context) {
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		if err := a.checkMessageRevision(tx, m.ID, body.Revision); err != nil {
			return err
		}
		return nil
	}, readCommitted)
}
`
	sound := guardkit.Sources{"a.go": visibility, "b.go": series, "c.go": group, "d.go": rooms, "e.go": messages}
	if bad := editRevisionProblems(sound.Parse(0)); len(bad) > 0 {
		t.Fatalf("the sound shape was flagged: %v", bad)
	}
	for name, src := range map[string]string{
		"a date pick that skips the check": `package main
func (a *App) apiSetDateRoom(c *gin.Context) {
	a.DB.Transaction(func(tx *gorm.DB) error { return nil })
}
func (a *App) apiSetSeriesRoom(c *gin.Context) {
	a.DB.Transaction(func(tx *gorm.DB) error { return a.checkEventRevision(locked, f.group, body.Revision) })
}
`,
		"a date pick that reads its booking outside its transaction's handle": `package main
func (a *App) apiSetDateRoom(c *gin.Context) {
	a.DB.Transaction(func(tx *gorm.DB) error { return a.checkDateRoomRevision(a.DB, occ.ID, body.Revision) })
}
func (a *App) apiSetSeriesRoom(c *gin.Context) {
	a.DB.Transaction(func(tx *gorm.DB) error { return a.checkEventRevision(locked, f.group, body.Revision) })
}
`,
		"a date pick checked before its transaction": `package main
func (a *App) apiSetDateRoom(c *gin.Context) {
	if err := a.checkDateRoomRevision(a.DB, occ.ID, body.Revision); err != nil {
		return
	}
	a.DB.Transaction(func(tx *gorm.DB) error { return nil })
}
func (a *App) apiSetSeriesRoom(c *gin.Context) {
	a.DB.Transaction(func(tx *gorm.DB) error { return a.checkEventRevision(locked, f.group, body.Revision) })
}
`,
		"a series pick checked in the helper it calls": `package main
func (a *App) apiSetDateRoom(c *gin.Context) {
	a.DB.Transaction(func(tx *gorm.DB) error { return a.checkDateRoomRevision(tx, occ.ID, body.Revision) })
}
func (a *App) apiSetSeriesRoom(c *gin.Context) {
	a.DB.Transaction(func(tx *gorm.DB) error { return bookWholeSeries(tx, locked, body) })
}
func bookWholeSeries(tx *gorm.DB, ev *models.Event, body seriesBody) error {
	return a.checkEventRevision(ev, nil, body.Revision)
}
`,
	} {
		sources := guardkit.Sources{"a.go": visibility, "b.go": series, "c.go": group, "d.go": src, "e.go": messages}
		if bad := editRevisionProblems(sources.Parse(0)); len(bad) == 0 {
			t.Errorf("%s passed the guard", name)
		}
	}
	for name, src := range map[string]string{
		"a message edit that skips the check": `package main
func (a *App) apiUpdateMessage(c *gin.Context) {
	a.DB.Transaction(func(tx *gorm.DB) error { return nil }, readCommitted)
}
func (a *App) apiScheduleMessage(c *gin.Context) {
	a.DB.Transaction(func(tx *gorm.DB) error { return a.checkMessageRevision(tx, m.ID, body.Revision) }, readCommitted)
}
`,
		"a send checked before its transaction": `package main
func (a *App) apiUpdateMessage(c *gin.Context) {
	a.DB.Transaction(func(tx *gorm.DB) error { return a.checkMessageRevision(tx, m.ID, body.Revision) }, readCommitted)
}
func (a *App) apiScheduleMessage(c *gin.Context) {
	if err := a.checkMessageRevision(a.DB, m.ID, body.Revision); err != nil {
		return
	}
	a.DB.Transaction(func(tx *gorm.DB) error { return nil }, readCommitted)
}
`,
		"a send that reads its message outside its transaction's handle": `package main
func (a *App) apiUpdateMessage(c *gin.Context) {
	a.DB.Transaction(func(tx *gorm.DB) error { return a.checkMessageRevision(tx, m.ID, body.Revision) }, readCommitted)
}
func (a *App) apiScheduleMessage(c *gin.Context) {
	a.DB.Transaction(func(tx *gorm.DB) error { return a.checkMessageRevision(a.DB, m.ID, body.Revision) }, readCommitted)
}
`,
	} {
		sources := guardkit.Sources{"a.go": visibility, "b.go": series, "c.go": group, "d.go": rooms, "e.go": src}
		if bad := editRevisionProblems(sources.Parse(0)); len(bad) == 0 {
			t.Errorf("%s passed the guard", name)
		}
	}
	for name, src := range map[string]string{
		"a handler that skips the check": `package main
func (a *App) apiSetEventVisibility(c *gin.Context) {
	a.contactMutation(contactChange{}, func(tx *gorm.DB) error { return nil })
}
`,
		"a check before the transaction": `package main
func (a *App) apiSetEventVisibility(c *gin.Context) {
	if err := a.checkEventRevision(ev, body.Revision); err != nil {
		return
	}
	a.contactMutation(contactChange{}, func(tx *gorm.DB) error { return nil })
}
`,
		"a check in another closure": `package main
func (a *App) apiSetEventVisibility(c *gin.Context) {
	check := func() error { return a.checkEventRevision(ev, body.Revision) }
	a.contactMutation(contactChange{}, func(tx *gorm.DB) error { return check() })
}
`,
		"a group save that skips the check": `package main
func (a *App) apiSetEventVisibility(c *gin.Context) {
	a.contactMutation(contactChange{}, func(tx *gorm.DB) error { return a.checkEventRevision(locked, body.Revision) })
}
func (a *App) apiUpdateGroup(c *gin.Context) {
	a.contactMutation(contactChange{}, func(tx *gorm.DB) error { return nil })
}
`,
		"a group check before the transaction": `package main
func (a *App) apiSetEventVisibility(c *gin.Context) {
	a.contactMutation(contactChange{}, func(tx *gorm.DB) error { return a.checkEventRevision(locked, body.Revision) })
}
func (a *App) apiUpdateGroup(c *gin.Context) {
	if err := a.checkGroupRevision(a.DB, g.ID, body.Revision); err != nil {
		return
	}
	a.contactMutation(contactChange{}, func(tx *gorm.DB) error { return nil })
}
`,
		"a group check that reads outside its transaction's handle": `package main
func (a *App) apiSetEventVisibility(c *gin.Context) {
	a.contactMutation(contactChange{}, func(tx *gorm.DB) error { return a.checkEventRevision(locked, body.Revision) })
}
func (a *App) apiUpdateGroup(c *gin.Context) {
	a.contactMutation(contactChange{}, func(tx *gorm.DB) error { return a.checkGroupRevision(a.DB, locked.ID, body.Revision) })
}
`,
		"an unlisted handler that checks": `package main
func (a *App) apiSetEventVisibility(c *gin.Context) {
	a.contactMutation(contactChange{}, func(tx *gorm.DB) error { return a.checkEventRevision(locked, body.Revision) })
}
func (a *App) apiSetGroup(c *gin.Context) {
	a.DB.Transaction(func(tx *gorm.DB) error { return checkEditRevision(body.Revision, current, errEventChanged) })
}
`,
	} {
		sources := guardkit.Sources{"a.go": src, "b.go": series, "d.go": rooms, "e.go": messages}
		if !strings.Contains(src, "apiUpdateGroup") {
			sources["c.go"] = group
		}
		if bad := editRevisionProblems(sources.Parse(0)); len(bad) == 0 {
			t.Errorf("%s passed the guard", name)
		}
	}
}
