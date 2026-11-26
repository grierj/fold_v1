package app

import (
	"go/ast"
	"go/token"
	"strconv"
	"strings"
	"testing"

	"fold/internal/guardkit"
)

// emailChoiceViolations is parse-only. It cannot see SQL assembled from
// non-literal pieces, reflection, SQL sent around GORM, dot-imports of
// fold/models, or cross-package helpers. Inventory Table literals are data,
// not queries, and use the same narrowly parsed exception as suppression SQL.
func emailChoiceViolations(sources map[string]string) []string {
	parsed := guardkit.Sources(sources).Parse(0)
	bad := append([]string(nil), parsed.Problems...)
	positions := parsed.Fset
	for _, pf := range parsed.Files {
		path, tree := pf.Path, pf.AST
		if pf.Dir() != "." && !strings.HasPrefix(path, "internal/") {
			continue
		}
		models := guardkit.ImportNames(tree, "fold/models")
		for _, decl := range tree.Decls {
			owner := ""
			if f, ok := decl.(*ast.FuncDecl); ok {
				owner = f.Name.Name
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				flagged := false
				switch n := n.(type) {
				case *ast.BasicLit:
					if n.Kind == token.STRING {
						s, _ := strconv.Unquote(n.Value)
						flagged = strings.Contains(strings.ToLower(s), "email_address_optouts") && path != "internal/app/email_optouts.go"
					}
				case *ast.SelectorExpr:
					id, ok := n.X.(*ast.Ident)
					if !ok || !models[id.Name] {
						break
					}
					switch n.Sel.Name {
					case "EmailAddressOptOut":
						flagged = path != "internal/app/email_optouts.go"
					case "ConsentEmail":
						flagged = path != "internal/app/email_optouts.go" && !(path == "internal/app/email_unsubscribe.go" && owner == "setEmailConsent") && !(path == "internal/app/api_notification_settings.go" && owner == "apiUpdateNotificationSettings")
					}
				}
				if flagged {
					bad = append(bad, positions.Position(n.Pos()).String()+": email choices must use readEmailOff")
				}
				return true
			})
		}
	}
	return bad
}

func TestEmailChoiceSQLStaysInOneHelper(t *testing.T) {
	for _, bad := range emailChoiceViolations(guardkit.Production(t)) {
		t.Error(bad)
	}
}

func TestEmailChoiceGuardCatchesBypasses(t *testing.T) {
	for name, src := range map[string]string{
		"table":    `package app; func x() { _ = "SELECT * FROM email_address_optouts" }`,
		"model":    `package app; import m "fold/models"; func x() { _ = m.EmailAddressOptOut{} }`,
		"consents": `package app; import "fold/models"; func x() { _ = models.ConsentEmail }`,
	} {
		t.Run(name, func(t *testing.T) {
			path := "internal/app/messages_send.go"
			if name == "model" {
				path = "internal/app/api_admin_suppressions.go"
			}
			if bad := emailChoiceViolations(map[string]string{path: src}); len(bad) == 0 {
				t.Fatal("bypass accepted")
			}
		})
	}
	if bad := emailChoiceViolations(map[string]string{"internal/app/email_optouts.go": `package app; import "fold/models"; func readEmailOff() { _ = models.ConsentEmail; _ = "SELECT * FROM email_address_optouts" }`}); len(bad) != 0 {
		t.Fatal(bad)
	}
}

// optionalMailViolations is parse-only. It cannot see messages built in other
// packages, reflection, values copied through untyped containers, or a sender
// that ignores Optional. Elided literals such as []mail.Message{{Optional: true}}
// are not matched. It checks send-boundary call locations, the batch's lexical
// claim/read/render order, and a notification .Off call, as well as explicit
// optional/broadcast constructors and assignments, following import aliases.
// Lexical order does not prove execution order, that the SQL literal claims a
// row, or that the choice result controls sending; behavioral tests cover those.
func optionalMailViolations(sources map[string]string) []string {
	parsed := guardkit.Sources(sources).Parse(0)
	positions := parsed.Fset
	bad := append([]string(nil), parsed.Problems...)
	functions := map[string]*ast.FuncDecl{}
	allowed := map[string]string{"internal/app/notice_email.go": "notificationEmailMessage", "internal/app/messages_send.go": "messageEmail", "internal/app/api_message_preview.go": "apiTestMessage", "internal/app/notify_guardian_copy.go": "guardianCopyEmailMessage"}
	for _, pf := range parsed.Files {
		path, tree := pf.Path, pf.AST
		if pf.Dir() != "." && (!strings.HasPrefix(path, "internal/") || strings.HasPrefix(path, "internal/mail/") || strings.HasPrefix(path, "internal/providers/")) {
			continue
		}
		providers := guardkit.ImportNames(tree, "fold/internal/providers")
		mailPkg := guardkit.ImportNames(tree, "fold/internal/mail")
		isFalse := func(e ast.Expr) bool { id, ok := e.(*ast.Ident); return ok && id.Name == "false" }
		isBroadcast := func(e ast.Expr) bool {
			s, ok := e.(*ast.SelectorExpr)
			if !ok {
				return false
			}
			id, ok := s.X.(*ast.Ident)
			return ok && providers[id.Name] && s.Sel.Name == "StreamBroadcast"
		}
		inspect := func(node ast.Node, owner string, appMethod bool) {
			ast.Inspect(node, func(n ast.Node) bool {
				flagged := false
				switch n := n.(type) {
				case *ast.CompositeLit:
					sel, ok := n.Type.(*ast.SelectorExpr)
					if !ok {
						break
					}
					id, ok := sel.X.(*ast.Ident)
					if !ok || !mailPkg[id.Name] || sel.Sel.Name != "Message" {
						break
					}
					for _, elt := range n.Elts {
						kv, ok := elt.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						key, ok := kv.Key.(*ast.Ident)
						if !ok {
							continue
						}
						flagged = flagged || key.Name == "Optional" && !isFalse(kv.Value) || key.Name == "Stream" && isBroadcast(kv.Value)
					}
				case *ast.AssignStmt:
					for i, lhs := range n.Lhs {
						sel, ok := lhs.(*ast.SelectorExpr)
						if !ok || i >= len(n.Rhs) {
							continue
						}
						flagged = flagged || sel.Sel.Name == "Optional" && !isFalse(n.Rhs[i]) || sel.Sel.Name == "Stream" && isBroadcast(n.Rhs[i])
					}
				}
				if flagged && (allowed[path] != owner || !appMethod) {
					bad = append(bad, positions.Position(n.Pos()).String()+": optional mail needs an effective-choice send boundary")
				}
				return true
			})
		}
		for _, decl := range tree.Decls {
			f, ok := decl.(*ast.FuncDecl)
			if !ok {
				inspect(decl, "", false)
				continue
			}
			appMethod := false
			if f.Recv != nil && len(f.Recv.List) == 1 {
				if p, ok := f.Recv.List[0].Type.(*ast.StarExpr); ok {
					if id, ok := p.X.(*ast.Ident); ok {
						appMethod = id.Name == "App"
					}
				}
			}
			functions[path+":"+f.Name.Name] = f
			inspect(f, f.Name.Name, appMethod)
		}
	}
	hasCall := func(node ast.Node, name string) bool {
		found := false
		ast.Inspect(node, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := c.Fun.(*ast.Ident); ok && id.Name == name {
				found = true
			}
			return true
		})
		return found
	}
	for _, edge := range [][3]string{
		{"internal/app/notice_email.go:readEmailRecipients", "readEmailOff", "effective recipient choices"},
		{"internal/app/notice_email.go:readEmailRecipient", "readEmailRecipients", "effective recipient read"},
		{"internal/app/notify_deliver.go:sendHeldNotification", "readEmailRecipient", "notification boundary"},
		{"internal/app/notify_guardian_copy.go:prepareGuardianCopy", "readEmailRecipient", "guardian copy boundary"},
	} {
		if f := functions[edge[0]]; f == nil || !hasCall(f, edge[1]) {
			bad = append(bad, edge[0]+": missing "+edge[2])
		}
	}
	for _, key := range []string{"internal/app/notify_deliver.go:sendHeldNotification", "internal/app/notify_guardian_copy.go:prepareGuardianCopy"} {
		f := functions[key]
		if f == nil {
			continue // the edge check above names a missing function
		}
		usesChoice := false
		ast.Inspect(f, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Off" {
					usesChoice = true
				}
			}
			return true
		})
		if !usesChoice {
			bad = append(bad, key+": missing effective-choice .Off call")
		}
	}
	for _, key := range []string{"internal/app/messages_send.go:deliverMessageBatchOnce", "internal/app/api_message_preview.go:apiTestMessage"} {
		f := functions[key]
		if f == nil {
			bad = append(bad, key+": missing send boundary")
			continue
		}
		inside, outside := 0, 0
		guardkit.InspectStack(f, func(n ast.Node, stack []ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			id, ok := call.Fun.(*ast.Ident)
			if !ok || id.Name != "readEmailOff" {
				return true
			}
			inRange := false
			var claimTransaction *ast.FuncLit
			for i, parent := range stack {
				if _, ok := parent.(*ast.RangeStmt); ok {
					inRange = true
				}
				literal, ok := parent.(*ast.FuncLit)
				if !ok || i == 0 {
					continue
				}
				transaction, ok := stack[i-1].(*ast.CallExpr)
				if !ok {
					continue
				}
				sel, ok := transaction.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Transaction" {
					continue
				}
				for _, arg := range transaction.Args {
					if arg == literal {
						claimTransaction = literal
					}
				}
			}
			if claimTransaction != nil && (key != "internal/app/messages_send.go:deliverMessageBatchOnce" || inRange) {
				inside++
				if key == "internal/app/messages_send.go:deliverMessageBatchOnce" {
					claimedBefore, renderedAfter := false, false
					ast.Inspect(claimTransaction.Body, func(n ast.Node) bool {
						// A nested closure cannot supply this transaction's claim
						// or rendering evidence merely by containing the tokens.
						if _, ok := n.(*ast.FuncLit); ok {
							return false
						}
						if literal, ok := n.(*ast.BasicLit); ok && literal.Kind == token.STRING {
							text, err := strconv.Unquote(literal.Value)
							if err == nil && strings.Contains(text, "FOR UPDATE SKIP LOCKED") && literal.End() < call.Pos() {
								claimedBefore = true
							}
						}
						if render, ok := n.(*ast.CallExpr); ok {
							if sel, ok := render.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "messageEmail" && render.Pos() > call.End() {
								renderedAfter = true
							}
						}
						return true
					})
					if !claimedBefore || !renderedAfter {
						bad = append(bad, key+": readEmailOff must follow the claim and precede messageEmail in the same transaction")
					}
				}
			} else {
				outside++
			}
			return true
		})
		if inside == 0 || outside != 0 {
			bad = append(bad, key+": readEmailOff must run only inside each claim transaction")
		}
	}
	return bad
}

func TestOptionalMailChecksTheEffectiveChoice(t *testing.T) {
	for _, bad := range optionalMailViolations(guardkit.Production(t)) {
		t.Error(bad)
	}
}

func cleanOptionalMailSources() map[string]string {
	return map[string]string{
		"internal/app/notice_email.go": `package app
func readEmailRecipients() { readEmailOff() }
func readEmailRecipient() { readEmailRecipients() }
func (a *App) notificationEmailMessage() {}`,
		"internal/app/notify_deliver.go":       `package app; func (a *App) sendHeldNotification() { readEmailRecipient(); off.Off() }`,
		"internal/app/notify_guardian_copy.go": `package app; func (a *App) prepareGuardianCopy() { readEmailRecipient(); off.Off() }; func (a *App) guardianCopyEmailMessage() {}`,
		"internal/app/messages_send.go":        `package app; func (a *App) deliverMessageBatchOnce() { for range rows { db.Transaction(func() { tx.Raw("FOR UPDATE SKIP LOCKED"); readEmailOff(); a.messageEmail() }) } }`,
		"internal/app/api_message_preview.go":  `package app; func (a *App) apiTestMessage() { db.Transaction(func() { readEmailOff() }) }`,
		"internal/app/mail_jobs.go":            `package app; func sendDirect() { msg.Optional = false }`,
	}
}

func TestOptionalMailGuardCatchesBypasses(t *testing.T) {
	if bad := optionalMailViolations(cleanOptionalMailSources()); len(bad) != 0 {
		t.Fatal(bad)
	}
	for name, src := range map[string]string{
		"optional constructor":                   `package app; import m "fold/internal/mail"; func other() { _ = m.Message{Optional: true} }`,
		"broadcast constructor":                  `package app; import m "fold/internal/mail"; import p "fold/internal/providers"; func other() { _ = m.Message{Stream: p.StreamBroadcast} }`,
		"optional assignment":                    `package app; func other() { msg.Optional = true }`,
		"broadcast assignment":                   `package app; import p "fold/internal/providers"; func other() { msg.Stream = p.StreamBroadcast }`,
		"batch cache":                            `package app; func (a *App) deliverMessageBatchOnce() { readEmailOff(); for range rows { db.Transaction(func() {}) } }`,
		"batch choice before claim":              `package app; func (a *App) deliverMessageBatchOnce() { for range rows { db.Transaction(func() { readEmailOff(); tx.Raw("FOR UPDATE SKIP LOCKED"); a.messageEmail() }) } }`,
		"batch choice after render":              `package app; func (a *App) deliverMessageBatchOnce() { for range rows { db.Transaction(func() { tx.Raw("FOR UPDATE SKIP LOCKED"); a.messageEmail(); readEmailOff() }) } }`,
		"batch claim in other closure":           `package app; func (a *App) deliverMessageBatchOnce() { for range rows { db.Transaction(func() { _ = func() { tx.Raw("FOR UPDATE SKIP LOCKED") }; readEmailOff(); a.messageEmail() }) } }`,
		"notification ignores choice":            `package app; func (a *App) sendHeldNotification() { readEmailRecipient() }`,
		"missing composer choice":                `package app; func (a *App) apiTestMessage() { db.Transaction(func() {}) }`,
		"guardian copy ignores choice":           `package app; func (a *App) prepareGuardianCopy() { readEmailRecipient() }`,
		"guardian copy skips the recipient read": `package app; func (a *App) prepareGuardianCopy() { off.Off() }`,
	} {
		t.Run(name, func(t *testing.T) {
			sources := cleanOptionalMailSources()
			path := "internal/app/other.go"
			if strings.HasPrefix(name, "batch ") {
				path = "internal/app/messages_send.go"
			}
			if name == "notification ignores choice" {
				path = "internal/app/notify_deliver.go"
			}
			if strings.HasPrefix(name, "guardian copy") {
				path = "internal/app/notify_guardian_copy.go"
			}
			if name == "missing composer choice" {
				path = "internal/app/api_message_preview.go"
			}
			sources[path] = src
			if bad := optionalMailViolations(sources); len(bad) == 0 {
				t.Fatal("bypass accepted")
			}
		})
	}
}
