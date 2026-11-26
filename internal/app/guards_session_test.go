package app

import (
	"fmt"
	"go/ast"
	"go/token"
	"testing"

	"fold/internal/guardkit"
)

// ---------------------------------------------------------------------------
// "This session is still current" is decided by sessionCurrent (and the actor
// locks built on it) alone: no other production code compares a row's
// session_version or logout_version by hand.
//
// Parse-only: it flags ==/!= with a .SessionVersion or .LogoutVersion operand in
// package app outside sessionEpochExempt, matching by field name; it does
// not see comparisons in other packages, through helpers, or in SQL.
// ---------------------------------------------------------------------------

// sessionEpochExempt are the files that may compare an epoch themselves, each
// for a reason that is not a session recheck.
var sessionEpochExempt = map[string]string{
	"internal/app/session_state.go":      "the one predicate",
	"internal/app/api_guardian_email.go": "a signed emailed link binds the guardian's credential epoch at issue; there is no session",
}

// sessionEpochProblems reports every ==/!= with a .SessionVersion or
// .LogoutVersion operand in package app outside the exempt files.
func sessionEpochProblems(sources guardkit.Sources) []string {
	parsed := sources.Parse(0)
	bad := append([]string{}, parsed.Problems...)
	for _, f := range parsed.Package("app") {
		if f.Dir() != "internal/app" || sessionEpochExempt[f.Path] != "" {
			continue
		}
		ast.Inspect(f.AST, func(n ast.Node) bool {
			b, ok := n.(*ast.BinaryExpr)
			if !ok || (b.Op != token.EQL && b.Op != token.NEQ) {
				return true
			}
			for _, side := range []ast.Expr{b.X, b.Y} {
				if sel, ok := guardkit.Unparen(side).(*ast.SelectorExpr); ok && (sel.Sel.Name == "SessionVersion" || sel.Sel.Name == "LogoutVersion") {
					bad = append(bad, fmt.Sprintf("%s:%d: compares %s by hand; decide with sessionCurrent or lockActorIn", f.Path, parsed.Fset.Position(b.Pos()).Line, sel.Sel.Name))
					break
				}
			}
			return true
		})
	}
	return bad
}

func TestSessionEpochsAreComparedOnlyBySessionCurrent(t *testing.T) {
	for _, p := range sessionEpochProblems(guardkit.Production(t)) {
		t.Error(p)
	}
	for name, src := range map[string]string{
		"session version": "package app\nfunc f(a, b *U) bool { return a.SessionVersion != b.SessionVersion }\ntype U struct{ SessionVersion int }",
		"logout version":  "package app\nfunc f(a, b *U) bool { return (a.LogoutVersion) == b.LogoutVersion }\ntype U struct{ LogoutVersion int }",
	} {
		if len(sessionEpochProblems(guardkit.Sources{"internal/app/x.go": src})) == 0 {
			t.Errorf("%s: the guard accepted a hand-written epoch comparison", name)
		}
	}
	if p := sessionEpochProblems(guardkit.Sources{"internal/app/x.go": "package app\nfunc f(a, b *U) bool { return a.ID == b.ID }\ntype U struct{ ID int }"}); len(p) != 0 {
		t.Errorf("the guard flagged an unrelated comparison: %v", p)
	}
}

// ---------------------------------------------------------------------------
// The browser credential has one writer (M8.1): only session_state.go saves
// the session cookie, sets a cookie, or writes the login keys and the CSRF
// token, and the login setters run only inside a replaceCredential change, so
// every credential replacement mints a fresh T and its marker.
//
// Parse-only, package app: it flags a .Save call with two or more arguments
// (gorm's Save takes one), any SetCookie or NewCookie call, an assignment to
// or delete of a session's credential key or its whole Values, and a call of
// setSessionLogin, clearSessionLogin or setSessionVersion outside a function
// literal passed to replaceCredential. It does not see writes through other
// helpers or packages.
// ---------------------------------------------------------------------------

var credentialKeys = map[string]bool{"sessionUserKey": true, "sessionVersionKey": true, "sessionLogoutKey": true, "csrfSessionKey": true}

var loginSetters = map[string]bool{"setSessionLogin": true, "clearSessionLogin": true, "setSessionVersion": true}

func credentialWriterProblems(sources guardkit.Sources) []string {
	parsed := sources.Parse(0)
	bad := append([]string{}, parsed.Problems...)
	for _, f := range parsed.Package("app") {
		if f.Dir() != "internal/app" || f.Path == "internal/app/session_state.go" {
			continue
		}
		at := func(n ast.Node, what string) {
			bad = append(bad, fmt.Sprintf("%s:%d: %s; go through replaceCredential or writeSession (internal/app/session_state.go)", f.Path, parsed.Fset.Position(n.Pos()).Line, what))
		}
		credentialKey := func(e ast.Expr) bool {
			idx, ok := guardkit.Unparen(e).(*ast.IndexExpr)
			if !ok {
				return false
			}
			sel, ok := guardkit.Unparen(idx.X).(*ast.SelectorExpr)
			key, isIdent := guardkit.Unparen(idx.Index).(*ast.Ident)
			return ok && sel.Sel.Name == "Values" && isIdent && credentialKeys[key.Name]
		}
		guardkit.InspectStack(f.AST, func(n ast.Node, stack []ast.Node) bool {
			switch n := n.(type) {
			case *ast.AssignStmt:
				for _, lhs := range n.Lhs {
					if sel, ok := guardkit.Unparen(lhs).(*ast.SelectorExpr); ok && sel.Sel.Name == "Values" {
						at(n, "replaces a session's Values")
					} else if credentialKey(lhs) {
						at(n, "writes a credential key")
					}
				}
			case *ast.CallExpr:
				switch name := guardkit.Callee(n); {
				case name == "Save" && len(n.Args) >= 2:
					at(n, "saves a session")
				case name == "SetCookie" || name == "NewCookie":
					at(n, "sets a cookie")
				case name == "delete" && len(n.Args) == 2 && credentialKey(&ast.IndexExpr{X: n.Args[0], Index: n.Args[1]}):
					at(n, "deletes a credential key")
				case loginSetters[name] && !insideReplaceCredential(stack):
					at(n, "changes the login outside replaceCredential")
				}
			}
			return true
		})
	}
	return bad
}

// insideReplaceCredential: some enclosing function literal is an argument of
// a replaceCredential call.
func insideReplaceCredential(stack []ast.Node) bool {
	for i := len(stack) - 1; i > 0; i-- {
		lit, ok := stack[i].(*ast.FuncLit)
		if !ok {
			continue
		}
		if call, ok := stack[i-1].(*ast.CallExpr); ok && guardkit.Callee(call) == "replaceCredential" {
			for _, arg := range call.Args {
				if arg == lit {
					return true
				}
			}
		}
	}
	return false
}

func TestBrowserCredentialHasOneWriter(t *testing.T) {
	for _, p := range credentialWriterProblems(guardkit.Production(t)) {
		t.Error(p)
	}
	for name, body := range map[string]string{
		"save":           "s.Save(r, w)",
		"store save":     "a.Store.Save(r, w, s)",
		"set cookie":     "http.SetCookie(w, ck)",
		"epoch":          "s.Values[sessionVersionKey] = 2",
		"token":          "s.Values[csrfSessionKey] = t",
		"delete token":   "delete(s.Values, csrfSessionKey)",
		"whole values":   "s.Values = nil",
		"bare login":     "setSessionLogin(s, u)",
		"other callback": "a.other(c, func(s *S) bool { setSessionVersion(s, 2); return true })",
	} {
		src := "package app\nfunc f() { " + body + " }"
		if len(credentialWriterProblems(guardkit.Sources{"internal/app/x.go": src})) == 0 {
			t.Errorf("%s: the guard accepted %q", name, body)
		}
	}
	for _, body := range []string{
		"a.replaceCredential(c, func(s *S) bool { setSessionLogin(s, u); return true })",
		"tx.Save(&row)",
		"s.Values[setupSessionKey] = h",
	} {
		if p := credentialWriterProblems(guardkit.Sources{"internal/app/x.go": "package app\nfunc f() { " + body + " }"}); len(p) != 0 {
			t.Errorf("the guard flagged %q: %v", body, p)
		}
	}
}
