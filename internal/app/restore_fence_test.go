package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"io"
	"io/fs"
	"maps"
	"math/rand"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	neturl "net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"fold/internal/guardkit"
	"fold/internal/mail"

	"fold/internal/testdb"
	"fold/migrations"
	"fold/models"
	"fold/storage"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

func (a *App) restoreFenceFloor() int64 { return a.fence.floor.Load() }

// fenceTurnWaiters is how many callers wait for the fence turn: a test seam.
func (a *App) fenceTurnWaiters() int { return int(a.fence.waiters.Load()) }

func recoveryPolicyMismatch(file string, level int) error {
	got, err := strconv.Atoi(strings.TrimSpace(file))
	if err != nil {
		return fmt.Errorf("deploy/recovery-policy holds %q, want an integer: %w", file, err)
	}
	if got != level {
		return fmt.Errorf("deploy/recovery-policy holds %d but recoveryPolicyLevel is %d: change both together", got, level)
	}
	return nil
}

func TestRecoveryPolicyFileMatchesConstant(t *testing.T) {
	b, err := os.ReadFile("deploy/recovery-policy")
	if err != nil {
		t.Fatal(err)
	}
	if err := recoveryPolicyMismatch(string(b), recoveryPolicyLevel); err != nil {
		t.Error(err)
	}
	// The check fails when either side changes alone, and for anything that is
	// not an integer.
	for _, tc := range []struct {
		file  string
		level int
	}{
		{"1\n", 2}, {"2\n", 1}, {"3\n", 2}, {"one", 2}, {"", 2}, {"2.0\n", 2}, {"0\n", 2},
	} {
		if recoveryPolicyMismatch(tc.file, tc.level) == nil {
			t.Errorf("the mismatch check accepted file %q with level %d", tc.file, tc.level)
		}
	}
	if recoveryPolicyMismatch("2\n", 2) != nil || recoveryPolicyMismatch(" 2 \n", 2) != nil {
		t.Error("the mismatch check refused a file that equals the constant")
	}
}

// deploy/restore-uploads holds the integer `fold version --json` reports as
// restoreUploads; restore.sh refuses to bring uploads into a target without it.
func restoreUploadsMismatch(file string, level int) error {
	got, err := strconv.Atoi(strings.TrimSpace(file))
	if err != nil {
		return fmt.Errorf("deploy/restore-uploads holds %q, want an integer: %w", file, err)
	}
	if got != level {
		return fmt.Errorf("deploy/restore-uploads holds %d but restoreUploadsLevel is %d: change both together", got, level)
	}
	return nil
}

func TestRestoreUploadsFileMatchesConstant(t *testing.T) {
	b, err := os.ReadFile("deploy/restore-uploads")
	if err != nil {
		t.Fatal(err)
	}
	if err := restoreUploadsMismatch(string(b), restoreUploadsLevel); err != nil {
		t.Error(err)
	}
	if string(b) != "1\n" || restoreUploadsLevel != 1 {
		t.Errorf("deploy/restore-uploads = %q and level %d, want a one-line 1 for both", b, restoreUploadsLevel)
	}
	for _, tc := range []struct {
		file  string
		level int
	}{
		{"0\n", 1}, {"1\n", 2}, {"2\n", 1}, {"one", 1}, {"", 1}, {"1.0\n", 1},
	} {
		if restoreUploadsMismatch(tc.file, tc.level) == nil {
			t.Errorf("the mismatch check accepted file %q with level %d", tc.file, tc.level)
		}
	}
	if restoreUploadsMismatch("1\n", 1) != nil || restoreUploadsMismatch(" 1 \n", 1) != nil {
		t.Error("the mismatch check refused a file that equals the constant")
	}
}

var fenceWriterFuncs = map[string]bool{"bootRestoreFence": true, "takeRestoreTicket": true}

func sqlWritesWords(s string) bool {
	u := strings.ToUpper(s)
	for _, w := range []string{"INSERT", "UPDATE", "DELETE", "TRUNCATE", "MERGE", "COPY "} {
		if strings.Contains(u, w) {
			return true
		}
	}
	return false
}

// fenceKeyUsers may name restoreFenceKey: the two writers, the refusal every
// other settings writer calls, and the boot step's error text.
var fenceKeyUsers = map[string]bool{"bootRestoreFence": true, "takeRestoreTicket": true, "refuseRestoreFenceKey": true}

// fenceWriterProblems parses production sources. The row's key is spelled as a
// string only once, as the value of the restoreFenceKey constant in
// restore_fence.go; restoreFenceKey is used only in restore_fence.go, by the two
// writers and the refusal; and a statement that writes (an inline literal, or
// a constant of restore_fence.go whose value writes) is used only by the boot
// step and takeRestoreTicket.
func fenceWriterProblems(sources map[string]string) []string {
	parsed := guardkit.Sources(sources).Parse(0)
	bad := append([]string(nil), parsed.Problems...)
	fset := parsed.Fset
	// The write statements restore_fence.go declares as constants.
	writeConsts := map[string]bool{}
	if f := parsed.File("internal/app/restore_fence.go"); f != nil {
		for _, d := range f.AST.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, sp := range gd.Specs {
				vs := sp.(*ast.ValueSpec)
				for i, v := range vs.Values {
					if lit, ok := v.(*ast.BasicLit); ok && lit.Kind == token.STRING && sqlWritesWords(lit.Value) && i < len(vs.Names) {
						writeConsts[vs.Names[i].Name] = true
					}
				}
			}
		}
	}
	for _, pf := range parsed.Files {
		path, tree := pf.Path, pf.AST
		for _, d := range tree.Decls {
			fd, isFunc := d.(*ast.FuncDecl)
			keyDecl := false
			if gd, ok := d.(*ast.GenDecl); ok && gd.Tok == token.CONST && path == "internal/app/restore_fence.go" {
				for _, sp := range gd.Specs {
					for _, n := range sp.(*ast.ValueSpec).Names {
						if n.Name == "restoreFenceKey" {
							keyDecl = true
						}
					}
				}
			}
			writer := isFunc && fenceWriterFuncs[fd.Name.Name] && path == "internal/app/restore_fence.go"
			ast.Inspect(d, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.BasicLit:
					if x.Kind != token.STRING {
						return true
					}
					if strings.Contains(x.Value, "restore_fence") && !keyDecl {
						bad = append(bad, fmt.Sprintf("%s:%d: names the restore_fence row in a string; only the restoreFenceKey constant may", path, fset.Position(x.Pos()).Line))
						return true
					}
					if isFunc && path == "internal/app/restore_fence.go" && sqlWritesWords(x.Value) && !writer {
						bad = append(bad, fmt.Sprintf("%s:%d: writes in internal/app/restore_fence.go outside the boot step and takeRestoreTicket", path, fset.Position(x.Pos()).Line))
					}
				case *ast.Ident:
					if x.Name == "restoreFenceKey" && !keyDecl && (path != "internal/app/restore_fence.go" || !isFunc || !fenceKeyUsers[fd.Name.Name]) {
						bad = append(bad, fmt.Sprintf("%s:%d: uses restoreFenceKey outside the boot step, takeRestoreTicket and refuseRestoreFenceKey", path, fset.Position(x.Pos()).Line))
					}
					if writeConsts[x.Name] && isFunc && !writer {
						bad = append(bad, fmt.Sprintf("%s:%d: runs the restore fence's write statement %s outside the boot step and takeRestoreTicket", path, fset.Position(x.Pos()).Line, x.Name))
					}
				}
				return true
			})
		}
	}
	return bad
}

func TestRestoreFenceHasTwoWriters(t *testing.T) {
	sources := guardkit.Production(t)
	for _, v := range fenceWriterProblems(sources) {
		t.Error(v)
	}
	// Negative fixtures.
	clean := map[string]string{"internal/app/restore_fence.go": `package app
const restoreFenceKey = "restore_fence"
const (
	restoreFenceCreateSQL  = "INSERT INTO settings (key, value) VALUES (?, '0') ON CONFLICT (key) DO NOTHING"
	restoreFenceAdvanceSQL = "UPDATE settings SET value = (value::bigint + 1)::text WHERE key = ? RETURNING value::bigint"
)
func refuseRestoreFenceKey(key string) error { if key == restoreFenceKey { return errRestoreFenceKey }; return nil }
func (a *App) bootRestoreFence() error { tx.Exec(restoreFenceCreateSQL, restoreFenceKey); tx.Raw(restoreFenceAdvanceSQL, restoreFenceKey); return nil }
func (a *App) takeRestoreTicket() { tx.Raw(restoreFenceAdvanceSQL, restoreFenceKey) }
`, "internal/app/other.go": "package app\nfunc f() {}\n"}
	if bad := fenceWriterProblems(clean); len(bad) != 0 {
		t.Fatalf("the clean fixture failed: %v", bad)
	}
	for name, files := range map[string]map[string]string{
		"a writer in another file": {"internal/app/restore_fence.go": clean["internal/app/restore_fence.go"],
			"internal/app/other.go": "package app\nfunc f() { tx.Exec(\"UPDATE settings SET value = '0' WHERE key = 'restore_fence'\") }\n"},
		"a third writer in the file": {"internal/app/restore_fence.go": clean["internal/app/restore_fence.go"] +
			"func (a *App) other() { tx.Exec(\"DELETE FROM settings WHERE key = 'restore_fence'\") }\n", "internal/app/other.go": clean["internal/app/other.go"]},
		"a third writer through the key": {"internal/app/restore_fence.go": clean["internal/app/restore_fence.go"] +
			"func (a *App) other() { tx.Exec(\"DELETE FROM settings WHERE key = ?\", restoreFenceKey) }\n", "internal/app/other.go": clean["internal/app/other.go"]},
		"a third writer through the advance statement": {"internal/app/restore_fence.go": clean["internal/app/restore_fence.go"] +
			"func (a *App) other(k string) { tx.Raw(restoreFenceAdvanceSQL, k) }\n", "internal/app/other.go": clean["internal/app/other.go"]},
		"the key spelled again in the file": {"internal/app/restore_fence.go": clean["internal/app/restore_fence.go"] +
			"func (a *App) bootCopy() string { return \"restore_fence\" }\n", "internal/app/other.go": clean["internal/app/other.go"]},
		"the key constant used elsewhere": {"internal/app/restore_fence.go": clean["internal/app/restore_fence.go"],
			"internal/app/other.go": "package app\nfunc f() { _ = models.Setting{Key: restoreFenceKey} }\n"},
	} {
		if len(fenceWriterProblems(files)) == 0 {
			t.Errorf("%s: the guard accepted it", name)
		}
	}
}

// settingsWriteRe finds raw SQL that writes the settings table.
var settingsWriteRe = regexp.MustCompile(`(?i)(insert\s+into|update|delete\s+from)\s+(public\.)?settings\b`)

// settingsWriterProblems is the census of every writer of the settings table
// through models.Setting: a key spelled as a literal or a package constant is
// fixed; any other key (a parameter, a field, a variable) is a non-literal
// writer and its function must refuse the barrier's row with
// refuseRestoreFenceKey. The one such writer is setSettingIn (settings.go),
// which every settings setter calls; when settings.go is among the sources it
// must declare setSettingIn and that function must make the refusal. Raw SQL
// that writes settings is only restore_fence.go's.
func settingsWriterProblems(sources map[string]string) []string {
	parsed := guardkit.Sources(sources).Parse(0)
	bad := append([]string(nil), parsed.Problems...)
	fset := parsed.Fset
	consts := map[string]bool{}
	for _, pf := range parsed.Files {
		tree := pf.AST
		for _, d := range tree.Decls {
			if gd, ok := d.(*ast.GenDecl); ok && gd.Tok == token.CONST {
				for _, sp := range gd.Specs {
					for _, n := range sp.(*ast.ValueSpec).Names {
						consts[n.Name] = true
					}
				}
			}
		}
	}
	isSetting := func(e ast.Expr) bool {
		sel, ok := e.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Setting" {
			return false
		}
		id, ok := sel.X.(*ast.Ident)
		return ok && id.Name == "models"
	}
	sawSetter := false
	for _, pf := range parsed.Files {
		path, tree := pf.Path, pf.AST
		for _, d := range tree.Decls {
			fd, isFunc := d.(*ast.FuncDecl)
			refuses := false
			if isFunc && fd.Body != nil {
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					if c, ok := n.(*ast.CallExpr); ok {
						if id, ok := c.Fun.(*ast.Ident); ok && id.Name == "refuseRestoreFenceKey" {
							refuses = true
						}
					}
					return true
				})
			}
			if path == "internal/app/settings.go" && isFunc && fd.Name.Name == "setSettingIn" {
				sawSetter = true
				if !refuses {
					bad = append(bad, fmt.Sprintf("%s:%d: setSettingIn does not refuse restore_fence", path, fset.Position(fd.Pos()).Line))
				}
			}
			guardkit.InspectStack(d, func(n ast.Node, stack []ast.Node) bool {
				switch x := n.(type) {
				case *ast.BasicLit:
					if path != "internal/app/restore_fence.go" && x.Kind == token.STRING && settingsWriteRe.MatchString(x.Value) {
						bad = append(bad, fmt.Sprintf("%s:%d: raw SQL writes settings outside internal/app/restore_fence.go", path, fset.Position(x.Pos()).Line))
					}
				case *ast.CompositeLit:
					typed := x.Type != nil && isSetting(x.Type)
					if x.Type == nil && len(stack) >= 2 {
						if parent, ok := stack[len(stack)-2].(*ast.CompositeLit); ok {
							if at, ok := parent.Type.(*ast.ArrayType); ok && isSetting(at.Elt) {
								typed = true
							}
						}
					}
					if !typed {
						return true
					}
					for _, el := range x.Elts {
						kv, ok := el.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						if k, ok := kv.Key.(*ast.Ident); !ok || k.Name != "Key" {
							continue
						}
						switch v := kv.Value.(type) {
						case *ast.BasicLit:
							if strings.Contains(v.Value, "restore_fence") {
								bad = append(bad, fmt.Sprintf("%s:%d: writes the restore_fence key", path, fset.Position(v.Pos()).Line))
							}
						case *ast.Ident:
							if !consts[v.Name] && !refuses {
								bad = append(bad, fmt.Sprintf("%s:%d: a settings writer with variable key %s does not refuse restore_fence", path, fset.Position(v.Pos()).Line, v.Name))
							}
						default:
							if !refuses {
								bad = append(bad, fmt.Sprintf("%s:%d: a settings writer with a non-literal key does not refuse restore_fence", path, fset.Position(kv.Pos()).Line))
							}
						}
					}
				}
				return true
			})
		}
	}
	if _, ok := sources["internal/app/settings.go"]; ok && !sawSetter {
		bad = append(bad, "internal/app/settings.go does not declare setSettingIn, the one settings writer")
	}
	return bad
}

func TestSettingsWritersRefuseTheFenceKey(t *testing.T) {
	sources := guardkit.Production(t)
	if _, ok := sources["internal/app/settings.go"]; !ok {
		t.Fatal("internal/app/settings.go, home of setSettingIn, is not in the production tree")
	}
	for _, v := range settingsWriterProblems(sources) {
		t.Error(v)
	}
	const head = "package app\nconst settingFixed = \"fixed\"\n"
	good := head + "func a(tx *gorm.DB) { tx.Where(models.Setting{Key: settingFixed}).FirstOrCreate(nil) }\n" +
		"func b(tx *gorm.DB, key string) error { if err := refuseRestoreFenceKey(key); err != nil { return err }\n tx.Where(models.Setting{Key: key}).FirstOrCreate(nil); return nil }\n" +
		"func c(tx *gorm.DB) error { for _, s := range []models.Setting{{Key: \"site_name\"}} { if err := refuseRestoreFenceKey(s.Key); err != nil { return err }\n tx.Where(models.Setting{Key: s.Key}).FirstOrCreate(nil) }; return nil }\n"
	if bad := settingsWriterProblems(map[string]string{"internal/app/x.go": good}); len(bad) != 0 {
		t.Fatalf("the clean fixture failed: %v", bad)
	}
	for name, src := range map[string]string{
		"a variable key without the refusal":  head + "func f(tx *gorm.DB, key string) { tx.Where(models.Setting{Key: key}).FirstOrCreate(nil) }\n",
		"a field key without the refusal":     head + "func f(tx *gorm.DB, s S) { tx.Where(models.Setting{Key: s.Key}).FirstOrCreate(nil) }\n",
		"the literal key":                     head + "func f(tx *gorm.DB) { tx.Create(&models.Setting{Key: \"restore_fence\"}) }\n",
		"a slice element without the refusal": head + "func f(tx *gorm.DB, k string) { tx.Create(&[]models.Setting{{Key: k}}) }\n",
		"raw SQL that writes settings":        head + "func f(tx *gorm.DB) { tx.Exec(\"UPDATE settings SET value = '1' WHERE key = ?\", k) }\n",
	} {
		if len(settingsWriterProblems(map[string]string{"internal/app/x.go": src})) == 0 {
			t.Errorf("%s: the census accepted it", name)
		}
	}
	for name, src := range map[string]string{
		"setSettingIn without the refusal": "package app\nfunc setSettingIn(tx *gorm.DB, key, value string) error { return tx.Create(&models.Setting{Key: key, Value: value}).Error }\n",
		"no setSettingIn":                  "package app\nfunc other() {}\n",
	} {
		if len(settingsWriterProblems(map[string]string{"internal/app/settings.go": src})) == 0 {
			t.Errorf("%s: the census accepted internal/app/settings.go", name)
		}
	}
}

func TestRestoreFenceBoot(t *testing.T) {
	app := testApp(t)
	var n int64
	if err := app.DB.Raw("SELECT value::bigint FROM settings WHERE key = 'restore_fence'").Scan(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n < 1 || app.restoreFenceFloor() != n {
		t.Fatalf("after boot: counter %d, floor %d; want the floor on the boot's own value", n, app.restoreFenceFloor())
	}
	second := &App{DB: app.DB}
	if err := second.bootRestoreFence(); err != nil {
		t.Fatal(err)
	}
	if second.restoreFenceFloor() != n+1 || app.restoreFenceFloor() != n {
		t.Fatalf("a second process: floor %d (want %d), the first keeps %d", second.restoreFenceFloor(), n+1, app.restoreFenceFloor())
	}
	// An unparsable value stops the boot and names the row.
	if err := app.DB.Transaction(func(tx *gorm.DB) error {
		if err := declareRecoveryLevelIn(tx); err != nil {
			return err
		}
		return tx.Exec("UPDATE settings SET value = 'abc' WHERE key = 'restore_fence'").Error
	}); err != nil {
		t.Fatal(err)
	}
	err := (&App{DB: app.DB}).bootRestoreFence()
	if err == nil || !strings.Contains(err.Error(), "restore_fence") {
		t.Fatalf("boot over an unparsable value: %v, want an error naming restore_fence", err)
	}
	if app.DB.Exec("DELETE FROM settings WHERE key = 'restore_fence'").Error != nil {
		t.Fatal("cannot clear the row")
	}
	fresh := &App{DB: app.DB}
	if err := fresh.bootRestoreFence(); err != nil || fresh.restoreFenceFloor() != 1 {
		t.Fatalf("boot over a deleted row: %v, floor %d; a new process has no floor to protect", err, fresh.restoreFenceFloor())
	}
}

func TestRestoreTicketMethods(t *testing.T) {
	app := testApp(t)
	floor := app.restoreFenceFloor()
	take := func(db *gorm.DB) (restoreTicket, error) {
		var tk restoreTicket
		err := db.Transaction(func(tx *gorm.DB) error {
			var e error
			tk, e = app.takeRestoreTicket(tx)
			return e
		})
		return tk, err
	}
	// An error raises nothing and releases the turn.
	tk, err := take(app.DB)
	if err != nil || !tk.held || tk.value != floor+1 {
		t.Fatalf("first ticket: %+v, %v; want value %d held", tk, err, floor+1)
	}
	app.acknowledgeRestoreTicket(&tk, errors.New("rolled back"))
	if tk.held || app.restoreFenceFloor() != floor {
		t.Fatalf("an error acknowledgment: held %v floor %d, want released and floor %d", tk.held, app.restoreFenceFloor(), floor)
	}
	// Abandon raises nothing either, and is idempotent.
	tk, err = take(app.DB)
	if err != nil || tk.value != floor+2 {
		t.Fatalf("second ticket: %+v, %v", tk, err)
	}
	app.abandonRestoreTicket(&tk)
	app.abandonRestoreTicket(&tk)
	if app.restoreFenceFloor() != floor {
		t.Fatalf("abandon raised the floor to %d", app.restoreFenceFloor())
	}
	// A nil acknowledgment raises it, once.
	tk, err = take(app.DB)
	if err != nil || tk.value != floor+3 {
		t.Fatalf("third ticket: %+v, %v", tk, err)
	}
	app.acknowledgeRestoreTicket(&tk, nil)
	app.acknowledgeRestoreTicket(&tk, nil)
	if app.restoreFenceFloor() != floor+3 || tk.held {
		t.Fatalf("acknowledgment: floor %d held %v, want %d released", app.restoreFenceFloor(), tk.held, floor+3)
	}
	// A context that ends while another holder has the turn: its error, no ticket,
	// the counter untouched.
	holder, err := take(app.DB)
	if err != nil {
		t.Fatal(err)
	}
	var before int64
	app.DB.Raw("SELECT value::bigint FROM settings WHERE key = 'restore_fence'").Scan(&before)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	waiting, err := take(app.DB.WithContext(ctx))
	if !errors.Is(err, context.DeadlineExceeded) || waiting.held {
		t.Fatalf("waiting for the turn past the deadline: %+v, %v; want the context's error and no ticket", waiting, err)
	}
	var after int64
	app.DB.Raw("SELECT value::bigint FROM settings WHERE key = 'restore_fence'").Scan(&after)
	if after != before || app.fenceTurnWaiters() != 0 {
		t.Fatalf("the counter moved %d -> %d or %d waiters remain", before, after, app.fenceTurnWaiters())
	}
	app.abandonRestoreTicket(&holder)
}

// nextDeleteSucceeds deletes a fresh account and checks its ticket is one or
// two above the floor, that the recorder is silent and that the floor lands on
// the new counter.
func (s *attemptScene) nextDeleteSucceeds(name string) {
	s.t.Helper()
	target := s.target(name)
	floor := s.app.restoreFenceFloor()
	if w := s.del("", target.ID); w.Code != http.StatusOK {
		s.t.Fatalf("the next delete: %d %s", w.Code, w.Body.String())
	}
	got := s.counter()
	if got < floor+1 || got > floor+2 || s.app.restoreFenceFloor() != got {
		s.t.Fatalf("after the next delete: counter %d, floor %d (was %d); want a ticket one or two above the floor", got, s.app.restoreFenceFloor(), floor)
	}
	if n := s.restarts.Load(); n != 0 {
		s.t.Fatalf("the restart recorder fired %d times", n)
	}
}

func TestRestoreFenceNoFalseDetection(t *testing.T) {
	t.Run("a: validation fails after the fence statement", func(t *testing.T) {
		s := newAttemptScene(t)
		a := s.target("a")
		createGroupAs(t, loginAPI(t, s.app, "a", "pw"), "Sole group", "public")
		before, floor := s.counter(), s.app.restoreFenceFloor()
		if w := s.del("", a.ID); w.Code != http.StatusBadRequest {
			t.Fatalf("delete: %d %s, want 400 (a successor is needed)", w.Code, w.Body.String())
		}
		if s.counter() != before || s.app.restoreFenceFloor() != floor {
			t.Fatalf("a rolled-back ticket changed the counter %d -> %d or the floor %d -> %d", before, s.counter(), floor, s.app.restoreFenceFloor())
		}
		s.nextDeleteSucceeds("next")
	})

	t.Run("e: a COMMIT whose reply is lost raises only later tickets", func(t *testing.T) {
		for name, tc := range map[string]struct {
			fault     testdb.Faults
			committed bool
		}{
			"forwarded then closed": {testdb.Faults{Match: "idle_in_transaction_session_timeout", Commit: testdb.CommitForwardThenClose}, true},
			"closed unforwarded":    {testdb.Faults{Match: "idle_in_transaction_session_timeout", Commit: testdb.CommitCloseUnforwarded}, false},
		} {
			t.Run(name, func(t *testing.T) {
				setAdminDeleteBounds(t, 4*time.Second, 300*time.Millisecond)
				app := testApp(t)
				var restarts atomic.Int32
				newTestUser(t, app, "admin", "pw", "admin")
				target := newTestUser(t, app, "target", "pw", "member")
				r := newCtxRigFor(t, app)
				r.papp.requestRestart = func() { restarts.Add(1) }
				c := loginAPI(t, r.papp, "admin", "pw")
				floor := r.papp.restoreFenceFloor()
				r.proxy.Arm(tc.fault)
				w := c.do("DELETE", "/api/v1/admin/users/"+itoa(target.ID), nil, true)
				if w.Code != http.StatusInternalServerError {
					t.Fatalf("delete: %d %s, want 500 delete_outcome_unknown", w.Code, w.Body.String())
				}
				if r.papp.restoreFenceFloor() != floor {
					t.Fatal("an unknown COMMIT raised the floor")
				}
				if tc.committed {
					deadline := time.Now().Add(5 * time.Second)
					for !r.deletedUser(target.ID) && time.Now().Before(deadline) {
						time.Sleep(10 * time.Millisecond)
					}
				} else {
					r.awaitReleased(r.pidOf("idle_in_transaction_session_timeout"), 10*time.Second)
				}
				next := newTestUser(t, app, "next", "pw", "member")
				w = c.do("DELETE", "/api/v1/admin/users/"+itoa(next.ID), nil, true)
				if w.Code != http.StatusOK {
					t.Fatalf("the next delete: %d %s, want 200", w.Code, w.Body.String())
				}
				if restarts.Load() != 0 {
					t.Error("the restart recorder fired")
				}
				var counter int64
				app.DB.Raw("SELECT value::bigint FROM settings WHERE key = 'restore_fence'").Scan(&counter)
				if r.papp.restoreFenceFloor() != counter {
					t.Errorf("floor %d, counter %d: the acknowledged commit should be the largest", r.papp.restoreFenceFloor(), counter)
				}
			})
		}
	})

	t.Run("g: B waits on the turn while A is held, then commits with N+2", func(t *testing.T) {
		s := newAttemptScene(t)
		a, b := s.target("a"), s.target("b")
		cB := loginAPI(t, s.app, "admin", "pw")
		base := s.counter()
		entered, release := make(chan struct{}), make(chan struct{})
		var armed atomic.Bool
		armed.Store(true)
		s.app.hooks.restoreAckPause = func() {
			if armed.CompareAndSwap(true, false) {
				close(entered)
				<-release
			}
		}
		aDone, bDone := make(chan *httptest.ResponseRecorder, 1), make(chan *httptest.ResponseRecorder, 1)
		go func() { aDone <- s.deleteAs(s.c, "", a.ID) }()
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatal("A never reached its acknowledgment")
		}
		go func() { bDone <- s.deleteAs(cB, "", b.ID) }()
		testdb.WaitUntil(t, "B waiting on the turn", 10*time.Second, func() bool { return s.app.fenceTurnWaiters() == 1 })
		if s.app.restoreFenceFloor() != base {
			t.Fatal("A raised the floor before its acknowledgment")
		}
		close(release)
		for _, ch := range []chan *httptest.ResponseRecorder{aDone, bDone} {
			select {
			case w := <-ch:
				if w.Code != http.StatusOK {
					t.Fatalf("delete: %d %s", w.Code, w.Body.String())
				}
			case <-time.After(15 * time.Second):
				t.Fatal("a delete never returned")
			}
		}
		if got := s.counter(); got != base+2 || s.app.restoreFenceFloor() != base+2 {
			t.Fatalf("counter %d floor %d, want both %d", got, s.app.restoreFenceFloor(), base+2)
		}
		if s.restarts.Load() != 0 {
			t.Fatal("no restore happened, yet the recorder fired")
		}
	})

	t.Run("h: B's deadline ends while it waits on the turn", func(t *testing.T) {
		setAdminDeleteBounds(t, time.Second, 100*time.Millisecond)
		s := newAttemptScene(t)
		b := s.target("b")
		before := s.counter()
		s.app.fence.turnChan() <- struct{}{} // another holder has the turn
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() { done <- s.del("", b.ID) }()
		testdb.WaitUntil(t, "B waiting on the turn", 5*time.Second, func() bool { return s.app.fenceTurnWaiters() == 1 })
		select {
		case w := <-done:
			if w.Code != http.StatusServiceUnavailable || codeOf(t, w) != "delete_timed_out" {
				t.Fatalf("delete: %d %s, want 503 delete_timed_out", w.Code, w.Body.String())
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the delete never returned")
		}
		<-s.app.fence.turnChan()
		if s.counter() != before || s.deleted(b.ID) {
			t.Fatal("a delete that timed out waiting for the turn changed something")
		}
		adminDeleteBudget = 40 * time.Second
		s.nextDeleteSucceeds("next")
	})
}

// pgClients maps a client tool to the command that runs it: the tool itself, or
// scripts/pg-client.sh running it from the server's own image.
var pgClients = map[string][]string{}

// pgFallbackImageID is the local image ID scripts/pg-client.sh resolved for this
// process; every container run of the tests uses that one image.
var pgFallbackImageID string

var pgMajorRE = regexp.MustCompile(`\) (\d+)(?:\.\d+)?`)

// pgDiscoveryLimit stops a wrapper that hangs outside its own bounds (its
// stages add up to about five minutes in the worst case).
const pgDiscoveryLimit = 6 * time.Minute

// pgCleanupGrace is how long a command that was told to stop (SIGTERM) gets to
// finish before it is killed. The wrapper, told to stop, ends its own commands
// and removes their containers inside one deadline of its kill grace plus its
// daemon timeout (3s + 20s by default, and a second of rounding), so the grace has
// to outlast that. TestPGClientWrapperCleanupHoldsWhenRemovalStalls holds it to that.
const pgCleanupGrace = 45 * time.Second

// pgRun runs argv for at most limit with the given output writers. The command
// gets its own process group, so the deadline reaches what it started: SIGTERM
// to the group first (the wrapper's trap then stops its commands and removes its
// containers), SIGKILL to the group once grace has passed. exec's own kill hits
// only the command's pid, so a wrapper killed that way would leave its docker
// and a container behind.
func pgRun(limit, grace time.Duration, stdout, stderr io.Writer, argv ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = grace
	err := cmd.Run()
	if ctx.Err() != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // whatever ignored the TERM
	}
	return err
}

// The wrapper's exit codes for "no usable container runtime" (see
// scripts/pg-client.sh). Locally these read as "Docker is not available" and skip,
// like a missing docker; under CI they fail.
const (
	pgWrapperNoRuntime = 70
	pgWrapperNoDaemon  = 71
)

func pgWrapperPath() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "scripts", "pg-client.sh")
}

// pgCandidates lists the commands to try for one tool, in order. FOLD_PG_BIN
// (bin) stands alone, with no fallback.
func pgCandidates(tool string, major int, wrapper, bin string, lookPath func(string) (string, error)) [][]string {
	if bin != "" {
		return [][]string{{filepath.Join(bin, tool)}}
	}
	var candidates [][]string
	if p, err := lookPath(tool); err == nil {
		candidates = append(candidates, []string{p})
	}
	if _, err := lookPath("docker"); err == nil {
		candidates = append(candidates, []string{wrapper, strconv.Itoa(major), tool})
	}
	return candidates
}

// pgFallbackArgv runs the wrapper on one image ID, so the whole run uses the
// image that was recorded rather than whatever its tag points at later.
func pgFallbackArgv(wrapper string, major int, tool, imageID string) []string {
	return []string{"env", "FOLD_PG_IMAGE_ID=" + imageID, wrapper, strconv.Itoa(major), tool}
}

func exitCodeOf(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// resolvePGImage asks the wrapper for the local ID of the image it would use,
// pulling it (bounded) when it is not there.
func resolvePGImage(wrapper string, major int) (id, note string, unavailable bool) {
	var stdout, stderr strings.Builder
	if err := pgRun(pgDiscoveryLimit, pgCleanupGrace, &stdout, &stderr, wrapper, "--resolve", strconv.Itoa(major)); err != nil {
		code := exitCodeOf(err)
		return "", fmt.Sprintf("%s --resolve %d: %v: %s", wrapper, major, err, strings.TrimSpace(stderr.String())),
			code == pgWrapperNoRuntime || code == pgWrapperNoDaemon
	}
	return strings.TrimSpace(stdout.String()), "", false
}

// checkPGClient runs `<argv> --version` and accepts it only when it reports
// the server's major version. unavailable is set when the wrapper says there is
// no container runtime to run it with.
func checkPGClient(argv []string, major int, fromWrapper bool) (note string, ok, unavailable bool) {
	var buf bytes.Buffer
	err := pgRun(pgDiscoveryLimit, pgCleanupGrace, &buf, &buf, append(argv[:len(argv):len(argv)], "--version")...)
	out := buf.Bytes()
	m := pgMajorRE.FindSubmatch(out)
	switch {
	case err != nil:
		code := exitCodeOf(err)
		return fmt.Sprintf("%s: %v: %s", strings.Join(argv, " "), err, strings.TrimSpace(string(out))),
			false, fromWrapper && (code == pgWrapperNoRuntime || code == pgWrapperNoDaemon)
	case m == nil || string(m[1]) != strconv.Itoa(major):
		return fmt.Sprintf("%s reports %q, not PostgreSQL %d", strings.Join(argv, " "), strings.TrimSpace(string(out)), major), false, false
	}
	return "", true, false
}

// fenceTools finds a pg_dump and a pg_restore of the SERVER's major version. A
// newer client writes settings an older server rejects (a PostgreSQL 18
// pg_restore sends "SET transaction_timeout" to a server 16), and the restore
// then fails. The errors stay errors: the tests never ignore them. Order:
//   - FOLD_PG_BIN, a directory of tools (alone, no fallback);
//   - the tool on PATH, if its major matches;
//   - scripts/pg-client.sh, which runs the tool from the postgres:<major> image,
//     by the image's local ID (needs a running Docker, which CI and make dev
//     already use for Postgres).
//
// With no client and no running Docker the test skips locally and fails under
// CI; a client that exists but does not match, or an image that cannot be
// pulled or run, always fails, with what to do.
func fenceTools(t *testing.T) {
	t.Helper()
	findPGClients(t, "pg_dump", "pg_restore")
}

func findPGClients(t *testing.T, tools ...string) {
	t.Helper()
	var num int
	if err := testdb.Shared(t).Raw("SHOW server_version_num").Scan(&num).Error; err != nil {
		t.Fatal(err)
	}
	major := num / 10000
	wrapper := pgWrapperPath()
	for _, tool := range tools {
		if pgClients[tool] != nil {
			continue // found by an earlier test of this process
		}
		candidates := pgCandidates(tool, major, wrapper, os.Getenv("FOLD_PG_BIN"), exec.LookPath)
		var notes []string
		unusable := 0 // candidates that only say "no container runtime"
		for _, argv := range candidates {
			fromWrapper := argv[0] == wrapper
			if fromWrapper {
				if pgFallbackImageID == "" {
					id, note, unavailable := resolvePGImage(wrapper, major)
					if id == "" {
						notes = append(notes, note)
						if unavailable {
							unusable++
						}
						continue
					}
					pgFallbackImageID = id
					t.Logf("restore tests: PostgreSQL %d client tools run from container image %s", major, id)
				}
				argv = pgFallbackArgv(wrapper, major, tool, pgFallbackImageID)
			}
			note, ok, unavailable := checkPGClient(argv, major, fromWrapper)
			if ok {
				pgClients[tool] = argv
				t.Logf("restore tests: %s is %s", tool, strings.Join(argv, " "))
				break
			}
			notes = append(notes, note)
			if unavailable {
				unusable++
			}
		}
		if pgClients[tool] != nil {
			continue
		}
		if unusable == len(candidates) && os.Getenv("CI") == "" {
			t.Skipf("no %s of PostgreSQL %d is installed and Docker is not usable; the restore tests need PostgreSQL %d client tools:\n%s",
				tool, major, major, strings.Join(notes, "\n"))
		}
		t.Fatalf("no %s of PostgreSQL %d (the server's version) is usable; set FOLD_PG_BIN to a directory of PostgreSQL %d client tools, or make a running Docker available for scripts/pg-client.sh:\n%s",
			tool, major, major, strings.Join(notes, "\n"))
	}
}

func fenceDatabaseURL(t *testing.T, db *gorm.DB) string {
	t.Helper()
	var name string
	if err := db.Raw("SELECT current_database()").Scan(&name).Error; err != nil {
		t.Fatal(err)
	}
	raw, err := testdb.URL(name)
	if err != nil {
		t.Fatal(err)
	}
	// testdb.URL adds the session setting synchronous_commit as a query
	// parameter, which libpq tools (pg_dump, pg_restore) reject in a URI.
	u, err := neturl.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Del("synchronous_commit")
	u.RawQuery = q.Encode()
	return u.String()
}

func fenceRun(t *testing.T, args ...string) {
	t.Helper()
	argv := append(append([]string{}, pgClients[args[0]]...), args[1:]...)
	cmd := exec.Command(argv[0], argv[1:]...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", strings.Join(args[:2], " "), err, out)
	}
}

func fenceDump(t *testing.T, db *gorm.DB) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fence.dump")
	fenceRun(t, "pg_dump", "-Fc", "-f", path, fenceDatabaseURL(t, db))
	return path
}

// fenceRestore puts the dump back while the process runs: a full restore
// (--clean --if-exists --no-owner) or, for dataOnly, only the rows of settings
// and admin_operation_attempts.
func fenceRestore(t *testing.T, db *gorm.DB, dump string, dataOnly bool, others ...*gorm.DB) {
	t.Helper()
	defer recycleConnections(t, append([]*gorm.DB{db}, others...)...)
	url := fenceDatabaseURL(t, db)
	if !dataOnly {
		fenceRun(t, "pg_restore", "--clean", "--if-exists", "--no-owner", "-d", url, dump)
		return
	}
	if err := db.Exec("TRUNCATE settings, admin_operation_attempts").Error; err != nil {
		t.Fatal(err)
	}
	// The data-only COPY writes the restore_fence row with no declared
	// recovery level, which the startup triggers refuse. The test owns the
	// table, so it turns the fence off around the COPY: it stands in for an
	// operator who bypasses the fence, and the barrier must still detect the
	// restore.
	if err := db.Exec("ALTER TABLE settings DISABLE TRIGGER settings_recovery_floor, DISABLE TRIGGER settings_recovery_floor_invitations").Error; err != nil {
		t.Fatal(err)
	}
	fenceRun(t, "pg_restore", "--data-only", "--no-owner", "-t", "settings", "-t", "admin_operation_attempts", "-d", url, dump)
	if err := db.Exec("ALTER TABLE settings ENABLE TRIGGER settings_recovery_floor, ENABLE TRIGGER settings_recovery_floor_invitations").Error; err != nil {
		t.Fatal(err)
	}
}

// recycleConnections closes the idle connections of pools that were open while
// a --clean restore dropped and recreated the tables. A surviving backend keeps
// cached type and relation state for the dropped objects, and its next use (in
// this test or in the next one that shares the database) fails with "cache
// lookup failed for type". New connections start clean.
func recycleConnections(t *testing.T, dbs ...*gorm.DB) {
	t.Helper()
	for _, db := range dbs {
		sqlDB, err := db.DB()
		if err != nil {
			t.Fatal(err)
		}
		sqlDB.SetMaxIdleConns(0) // closes every idle connection now
		sqlDB.SetMaxIdleConns(2) // database/sql's default, which testdb does not change
	}
}

func TestRestoreFenceSameLifetime(t *testing.T) {
	fenceTools(t)
	for _, dataOnly := range []bool{false, true} {
		name := "pg_restore --clean --if-exists --no-owner"
		if dataOnly {
			name = "pg_restore --data-only of settings and admin_operation_attempts"
		}
		t.Run(name, func(t *testing.T) {
			s := newAttemptScene(t)
			a, b, other := s.target("a"), s.target("b"), s.target("other")
			idA, idB := s.mint(a.ID), s.mint(b.ID)
			if w := s.del(idA, a.ID); w.Code != http.StatusOK {
				t.Fatalf("delete A: %d %s", w.Code, w.Body.String())
			}
			dump := fenceDump(t, s.app.DB)
			atDump := s.counter()
			if w := s.del(idB, b.ID); w.Code != http.StatusOK {
				t.Fatalf("delete B: %d %s", w.Code, w.Body.String())
			}
			if s.app.restoreFenceFloor() != atDump+1 {
				t.Fatalf("floor %d, want %d", s.app.restoreFenceFloor(), atDump+1)
			}
			fenceRestore(t, s.app.DB, dump, dataOnly)
			if got := s.counter(); got != atDump {
				t.Fatalf("after the restore the counter is %d, want the dump's %d", got, atDump)
			}
			w := s.settle(idB, b.ID)
			if w.Code != http.StatusServiceUnavailable || codeOf(t, w) != "restore_detected" {
				t.Fatalf("settle B after the restore: %d %s, want 503 restore_detected", w.Code, w.Body.String())
			}
			if s.counter() != atDump {
				t.Error("a detected ticket changed the counter")
			}
			if n := s.restarts.Load(); n != 1 {
				t.Fatalf("the restart recorder fired %d times, want once", n)
			}
			// A later delete refuses before any transaction.
			w = s.del("", other.ID)
			if w.Code != http.StatusServiceUnavailable || codeOf(t, w) != "restore_detected" {
				t.Fatalf("a later delete: %d %s, want 503 restore_detected", w.Code, w.Body.String())
			}
			if s.counter() != atDump || s.deleted(other.ID) {
				t.Error("a delete after the detection changed something")
			}
			if s.restarts.Load() != 1 {
				t.Error("the recorder fires once")
			}
		})
	}
}

// A restore that lands between a COMMIT and its acknowledgment is still found
// by the next ticket, because the next ticket waits for the turn. Three
// schedules, each with real minted ids:
//   - A's acknowledgment is held by restoreAckPause and B deletes;
//   - the same, but B settles an unrelated valid id (a settle is a fenced
//     transaction too);
//   - A's COMMIT reply is held by pgproxy (no seam), and B deletes.
//
// In each, the restore of the dump taken at counter N lands while A (committed
// at N+1) has not raised the floor. B holds the gate (pg_locks), waits on the
// turn (fenceTurnWaiters = 1) and has sent no counter UPDATE (pg_stat_activity).
// A is released: A answers 200 and the floor is N+1; B reads N+1, answers 503
// restore_detected, changes no row, the database counter stays N and the
// recorder fires once. Settle of A's id in the same process is 503 before any
// transaction; a fresh App answers history_unavailable to A's settle and
// attempt_expired to its resubmission, A is active again and can be deleted
// again with a fresh id; nothing is ever answered not_applied.
func TestRestoreFenceDelayedAcknowledgment(t *testing.T) {
	fenceTools(t)
	for _, v := range []struct {
		name         string
		unrelatedOp  bool // B settles an unrelated id instead of deleting
		proxyHeldAck bool // A's COMMIT reply is held by pgproxy instead of restoreAckPause
	}{
		{name: "B deletes, A held by restoreAckPause"},
		{name: "B settles an unrelated id, A held by restoreAckPause", unrelatedOp: true},
		{name: "B deletes, A's COMMIT reply held by pgproxy", proxyHeldAck: true},
	} {
		t.Run(v.name, func(t *testing.T) {
			var s *attemptScene
			if v.proxyHeldAck {
				s = newProxiedAttemptScene(t)
			} else {
				s = newAttemptScene(t)
			}
			a, b, u := s.target("a"), s.target("b"), s.target("unrelated")
			cB := loginAPI(t, s.app, "admin", "pw")
			cB.holdNotices, s.c.holdNotices = true, true
			idA, idB, idU := s.mint(a.ID), s.mint(b.ID), s.mint(u.ID)
			base := s.counter()
			dump := fenceDump(t, s.app.DB)

			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			releaseA := func() { releaseOnce.Do(func() { close(release) }) }
			// A failed run must not leave A parked (and B holding the gate) for the
			// subtests and tests that share this database.
			t.Cleanup(func() { releaseA() })
			if v.proxyHeldAck {
				s.rig.proxy.Arm(testdb.Faults{Match: "idle_in_transaction_session_timeout", Commit: testdb.CommitHoldReply})
				releaseA = func() { releaseOnce.Do(s.rig.proxy.ReleaseReply) }
				t.Cleanup(s.rig.proxy.ReleaseReply)
			} else {
				var armed atomic.Bool
				armed.Store(true)
				s.app.hooks.restoreAckPause = func() {
					if armed.CompareAndSwap(true, false) {
						close(entered)
						<-release
					}
				}
			}
			aDone, bDone := make(chan *httptest.ResponseRecorder, 1), make(chan *httptest.ResponseRecorder, 1)
			go func() { aDone <- s.deleteAs(s.c, idA, a.ID) }()
			if v.proxyHeldAck {
				testdb.WaitUntil(t, "A's COMMIT to be sent and held", 15*time.Second, func() bool {
					return len(s.rig.proxy.Find(testdb.KindCommit, "")) > 0 && s.deleted(a.ID)
				})
			} else {
				select {
				case <-entered:
				case <-time.After(15 * time.Second):
					t.Fatal("A never reached its acknowledgment")
				}
			}
			if s.app.restoreFenceFloor() != base {
				t.Fatalf("floor %d before A's acknowledgment, want %d", s.app.restoreFenceFloor(), base)
			}
			// A is committed at N+1 and has not raised the floor: the N dump comes back.
			fenceRestore(t, s.app.DB, dump, false, s.direct)
			if got := s.counter(); got != base {
				t.Fatalf("after the restore the counter is %d, want %d", got, base)
			}
			if v.unrelatedOp {
				go func() { bDone <- s.settleAs(cB, idU, u.ID) }()
			} else {
				go func() { bDone <- s.deleteAs(cB, idB, b.ID) }()
			}
			testdb.WaitUntil(t, "B waiting on the turn", 15*time.Second, func() bool { return s.app.fenceTurnWaiters() == 1 })
			var holders, sentUpdate int64
			if err := s.direct.Raw("SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND granted AND " + advisoryInThisDatabase).Scan(&holders).Error; err != nil {
				t.Fatal(err)
			}
			if err := s.direct.Raw(`SELECT count(*) FROM pg_stat_activity WHERE datname = current_database()
				AND state = 'idle in transaction' AND query ILIKE '%restore_fence%'`).Scan(&sentUpdate).Error; err != nil {
				t.Fatal(err)
			}
			if holders != 1 {
				t.Fatalf("%d advisory locks are held, want B holding the gate alone", holders)
			}
			if sentUpdate != 0 {
				t.Fatalf("%d backends sent the counter UPDATE while B waited on the turn", sentUpdate)
			}
			releaseA()
			select {
			case w := <-aDone:
				if w.Code != http.StatusOK {
					t.Fatalf("A: %d %s, want 200", w.Code, w.Body.String())
				}
			case <-time.After(20 * time.Second):
				t.Fatal("A never returned")
			}
			if s.app.restoreFenceFloor() != base+1 {
				t.Fatalf("floor %d, want %d", s.app.restoreFenceFloor(), base+1)
			}
			bw := awaitRec(t, bDone, "B")
			if bw.Code != http.StatusServiceUnavailable || codeOf(t, bw) != "restore_detected" {
				t.Fatalf("B: %d %s, want 503 restore_detected", bw.Code, bw.Body.String())
			}
			if got := s.counter(); got != base || s.deleted(b.ID) || s.deleted(u.ID) {
				t.Fatalf("B changed something: counter %d (want %d)", got, base)
			}
			if n := s.restarts.Load(); n != 1 {
				t.Fatalf("the restart recorder fired %d times, want once", n)
			}
			// In-process: settle of A's real id is 503 before any transaction.
			var mark int
			if s.rig != nil {
				mark = len(s.rig.proxy.Events())
			}
			if w := s.settle(idA, a.ID); w.Code != http.StatusServiceUnavailable || codeOf(t, w) != "restore_detected" {
				t.Fatalf("settle of A after the detection: %d %s, want 503 restore_detected", w.Code, w.Body.String())
			}
			if s.rig != nil {
				for _, e := range s.rig.proxy.Events()[mark:] {
					if strings.Contains(e.SQL, "pg_advisory_xact_lock") || isRestoreFenceStatement(e.SQL) {
						t.Errorf("settle after the detection sent %q", e.SQL)
					}
				}
			}
			// A fresh App (new key, boot step) over the restored database.
			fresh := &App{DB: s.direct, Store: s.app.Store, Config: s.app.Config, Uploads: s.app.Uploads, Mailer: s.app.Mailer}
			bootFence(t, fresh)
			fc := loginAPI(t, fresh, "admin", "pw")
			fc.holdNotices = true
			bootCounter := s.counter()
			sw := fc.do("POST", "/api/v1/admin/operation-attempts/settle",
				map[string]any{"attemptId": idA, "kind": attemptKindAccountDelete, "subjectUserId": a.ID}, true)
			var got attemptSettleJSON
			decodeJSON(t, sw, &got)
			wantSettle(t, got, settleHistoryUnavailable, nil)
			if w := fc.do("DELETE", "/api/v1/admin/users/"+itoa(a.ID), map[string]any{"attemptId": idA}, true); w.Code != http.StatusConflict || codeOf(t, w) != "attempt_expired" {
				t.Fatalf("resubmitting A's id to a fresh App: %d %s, want 409 attempt_expired", w.Code, w.Body.String())
			}
			if s.counter() != bootCounter || s.deleted(a.ID) {
				t.Fatalf("a fresh App's refusals changed the counter or A is deleted (A should be active again)")
			}
			var minted attemptMintJSON
			decodeJSON(t, fc.do("POST", "/api/v1/admin/operation-attempts", map[string]any{"kind": attemptKindAccountDelete, "subjectUserId": a.ID}, true), &minted)
			if w := fc.do("DELETE", "/api/v1/admin/users/"+itoa(a.ID), map[string]any{"attemptId": minted.AttemptID}, true); w.Code != http.StatusOK || !s.deleted(a.ID) {
				t.Fatalf("re-deleting A with a fresh id: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

// Server outlives handler: A's deadline expires after its ticket while
// reconcileContacts waits on a test-held row, pgproxy dropping the cancel and
// withholding Terminate and the FIN. A answers 503 delete_timed_out; B waits on
// the gate (pg_blocking_pids names A's backend) and takes its ticket only after
// A's server transaction ends; B's ticket is one above the floor and B
// succeeds with the recorder silent.
func TestRestoreFenceServerOutlivesHandler(t *testing.T) {
	setAdminDeleteBounds(t, time.Second, 100*time.Millisecond)
	setContactBounds(t, 100*time.Millisecond, 5*time.Second)
	f := newContactAPIFixture(t, false)
	var restarts atomic.Int32
	req := f.create(t, "email", "")
	r := newCtxRigFor(t, f.app)
	// Both requests go through ONE process: the proxied App, booted like serve,
	// with its own floor, fence turn and restart recorder. The direct handle only
	// observes.
	bootFence(t, r.papp)
	r.papp.requestRestart = func() { restarts.Add(1) }
	ac := loginAPI(t, r.papp, f.admin.Email, "password1234")
	bClient := loginAPI(t, r.papp, f.admin.Email, "password1234")
	next := newTestUser(t, f.app, "next", "password1234", models.RoleMember)
	blocker, _ := beginOwnedTx(t, f.app.DB)
	if err := blocker.Exec("SELECT id FROM contact_requests WHERE id = ? FOR UPDATE", req.ID).Error; err != nil {
		t.Fatal(err)
	}
	counter := func() (n int64) {
		f.app.DB.Raw("SELECT value::bigint FROM settings WHERE key = 'restore_fence'").Scan(&n)
		return n
	}
	before := counter()
	floor := r.papp.restoreFenceFloor()
	r.proxy.Arm(testdb.Faults{Match: "idle_in_transaction_session_timeout", Cancel: testdb.CancelDrop, Close: testdb.CloseWithholdAll})
	t.Cleanup(r.proxy.ReleaseClose)

	aDone := make(chan *httptest.ResponseRecorder, 1)
	ac.holdNotices = true
	go func() { aDone <- ac.do("DELETE", "/api/v1/admin/users/"+itoa(f.subject.ID), nil, true) }()
	apid := r.pidOf(`FROM "contact_requests"`)
	r.awaitBlocked(apid)
	w := awaitRec(t, aDone, "A")
	if w.Code != http.StatusServiceUnavailable || codeOf(t, w) != "delete_timed_out" {
		t.Fatalf("A: %d %s, want 503 delete_timed_out", w.Code, w.Body.String())
	}
	// A's backend still holds the gate and its callback's writes.
	if r.lockRows(apid, "locktype = 'advisory' AND granted") != 1 {
		t.Fatal("after A's return its backend should still hold the gate")
	}
	adminDeleteBudget = 10 * time.Second
	bClient.holdNotices = true
	if r.papp.fenceTurnWaiters() != 0 {
		t.Fatal("A's return left a waiter or its turn held")
	}
	bDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { bDone <- bClient.do("DELETE", "/api/v1/admin/users/"+itoa(next.ID), nil, true) }()
	testdb.WaitUntil(t, "B blocked by A's backend", 10*time.Second, func() bool {
		var n int64
		f.app.DB.Raw(`SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'
			AND query ILIKE '%pg_advisory_xact_lock%' AND ? = ANY(pg_blocking_pids(pid))`, apid).Scan(&n)
		return n == 1
	})
	if counter() != before || r.papp.restoreFenceFloor() != floor {
		t.Fatal("B moved the counter before A's server transaction ended")
	}
	blocker.Rollback()
	bw := awaitRec(t, bDone, "B")
	if bw.Code != http.StatusOK {
		t.Fatalf("B: %d %s, want 200", bw.Code, bw.Body.String())
	}
	if got := counter(); got != before+1 || restarts.Load() != 0 || r.papp.restoreFenceFloor() != got {
		t.Fatalf("counter %d (want %d: A's rolled-back ticket counts for nothing), the process's floor %d, restarts %d", got, before+1, r.papp.restoreFenceFloor(), restarts.Load())
	}
	if r.papp.restoreDetected() {
		t.Fatal("the process marked a restore although none happened")
	}
	var deleted int64
	f.app.DB.Raw("SELECT count(*) FROM users WHERE id = ? AND deleted_at IS NOT NULL", next.ID).Scan(&deleted)
	if deleted != 1 {
		t.Fatal("B did not delete its account")
	}
}

// The stated limits: a restore of a dump taken after the last acknowledged
// fenced commit is not detected, and every acknowledged receipt still
// resolves; neither is anything answered wrongly.
func TestRestoreFenceUndetectedCases(t *testing.T) {
	fenceTools(t)
	t.Run("a dump after the last acknowledged commit", func(t *testing.T) {
		s := newAttemptScene(t)
		a, b := s.target("a"), s.target("b")
		idA := s.mint(a.ID)
		if w := s.del(idA, a.ID); w.Code != http.StatusOK {
			t.Fatalf("delete: %d %s", w.Code, w.Body.String())
		}
		dump := fenceDump(t, s.app.DB)
		fenceRestore(t, s.app.DB, dump, false)
		if s.restarts.Load() != 0 || s.app.restoreDetected() {
			t.Fatal("a restore that lost no acknowledged commit was reported")
		}
		wantSettle(t, s.settled(idA, a.ID), settleApplied, nil)
		if w := s.del("", b.ID); w.Code != http.StatusOK {
			t.Fatalf("a delete after the undetected restore: %d %s", w.Code, w.Body.String())
		}
	})

	// s2: A's COMMIT is forwarded and its reply dropped (unknown); a dump taken
	// just before it is restored. Nothing detects it, A's receipt is gone,
	// settle says history_unavailable (never not_applied) and the id stays
	// closed.
	t.Run("an unknown commit lost by a restore", func(t *testing.T) {
		setAdminDeleteBounds(t, 4*time.Second, 300*time.Millisecond)
		app := testApp(t)
		admin := newTestUser(t, app, "admin", "pw", models.RoleAdmin)
		target := newTestUser(t, app, "target", "pw", models.RoleMember)
		r := newCtxRigFor(t, app)
		var restarts atomic.Int32
		r.papp.requestRestart = func() { restarts.Add(1) }
		// The proxied App runs the boot step too, so its floor is the counter the
		// dump holds and a real detection would be possible.
		bootFence(t, r.papp)
		c := loginAPI(t, r.papp, "admin", "pw")
		var minted attemptMintJSON
		decodeJSON(t, c.do("POST", "/api/v1/admin/operation-attempts", map[string]any{"kind": attemptKindAccountDelete, "subjectUserId": target.ID}, true), &minted)
		dump := fenceDump(t, app.DB)
		r.proxy.Arm(testdb.Faults{Match: "idle_in_transaction_session_timeout", Commit: testdb.CommitForwardThenClose})
		w := c.do("DELETE", "/api/v1/admin/users/"+itoa(target.ID), map[string]any{"attemptId": minted.AttemptID}, true)
		if w.Code != http.StatusInternalServerError || codeOf(t, w) != "delete_outcome_unknown" {
			t.Fatalf("delete: %d %s, want 500 delete_outcome_unknown", w.Code, w.Body.String())
		}
		testdb.WaitUntil(t, "the commit to reach the server", 5*time.Second, func() bool { return r.deletedUser(target.ID) })
		fenceRestore(t, app.DB, dump, false)
		recycleConnections(t, r.papp.DB)
		if r.deletedUser(target.ID) {
			t.Fatal("the restore did not undo the deletion")
		}
		sw := c.do("POST", "/api/v1/admin/operation-attempts/settle",
			map[string]any{"attemptId": minted.AttemptID, "kind": attemptKindAccountDelete, "subjectUserId": target.ID}, true)
		var got attemptSettleJSON
		decodeJSON(t, sw, &got)
		wantSettle(t, got, settleHistoryUnavailable, nil)
		if restarts.Load() != 0 || r.papp.restoreDetected() {
			t.Fatal("a restore that lost only an unknown commit was reported")
		}
		again := c.do("DELETE", "/api/v1/admin/users/"+itoa(target.ID), map[string]any{"attemptId": minted.AttemptID}, true)
		if again.Code != http.StatusConflict || codeOf(t, again) != "attempt_closed" {
			t.Fatalf("resubmission: %d %s, want 409 attempt_closed (the id never runs twice)", again.Code, again.Body.String())
		}
		_ = admin
	})
}

// After a detected restore, a fresh App (new key, boot step) answers a settle or
// a submission of either id as history_unavailable or attempt_expired before
// any transaction, leaving the counter and the receipts alone.
func TestRestoreFenceFreshAppAfterARestore(t *testing.T) {
	fenceTools(t)
	s := newAttemptScene(t)
	a, b := s.target("a"), s.target("b")
	idA, idB := s.mint(a.ID), s.mint(b.ID)
	if w := s.del(idA, a.ID); w.Code != http.StatusOK {
		t.Fatalf("delete A: %d %s", w.Code, w.Body.String())
	}
	dump := fenceDump(t, s.app.DB)
	if w := s.del(idB, b.ID); w.Code != http.StatusOK {
		t.Fatalf("delete B: %d %s", w.Code, w.Body.String())
	}
	fenceRestore(t, s.app.DB, dump, false)
	fresh := &App{DB: s.app.DB, Store: s.app.Store, Config: s.app.Config, Uploads: s.app.Uploads, Mailer: s.app.Mailer}
	bootFence(t, fresh)
	fc := loginAPI(t, fresh, "admin", "pw")
	counter, receipts := s.counter(), s.receipts()
	for name, id := range map[string]string{"A": idA, "B": idB} {
		subject := a.ID
		if name == "B" {
			subject = b.ID
		}
		w := fc.do("DELETE", "/api/v1/admin/users/"+itoa(subject), map[string]any{"attemptId": id}, true)
		if w.Code != http.StatusConflict || codeOf(t, w) != "attempt_expired" {
			t.Errorf("submitting %s's id to a fresh App: %d %s, want 409 attempt_expired", name, w.Code, w.Body.String())
		}
		sw := fc.do("POST", "/api/v1/admin/operation-attempts/settle",
			map[string]any{"attemptId": id, "kind": attemptKindAccountDelete, "subjectUserId": subject}, true)
		var got attemptSettleJSON
		decodeJSON(t, sw, &got)
		wantSettle(t, got, settleHistoryUnavailable, nil)
	}
	if s.counter() != counter || s.receipts() != receipts {
		t.Error("a fresh App's refusals changed the counter or the receipts")
	}
	if s.deleted(b.ID) {
		t.Error("B should be active again after the restore")
	}
}

// s3: an id is minted and fenced by a settle, the database is dumped, the App
// is stopped, the dump is restored and a new App starts. The new App answers
// attempt_expired to a submission and history_unavailable to a settle, before
// any transaction: the counter and the receipts do not move after its boot.
func TestRestoreFenceFencedIdAcrossARestart(t *testing.T) {
	fenceTools(t)
	s := newAttemptScene(t)
	a := s.target("a")
	id := s.mint(a.ID)
	wantSettle(t, s.settled(id, a.ID), settleNotApplied, nil)
	if s.state(id, a.ID) != attemptFenced {
		t.Fatal("settle should leave the unclaimed id fenced")
	}
	dump := fenceDump(t, s.app.DB)
	// The App stops here; its fence and ledger are gone with it.
	fenceRestore(t, s.app.DB, dump, false, s.direct)
	fresh := &App{DB: s.direct, Store: s.app.Store, Config: s.app.Config, Uploads: s.app.Uploads, Mailer: s.app.Mailer}
	bootFence(t, fresh)
	fc := loginAPI(t, fresh, "admin", "pw")
	counter, receipts := s.counter(), s.receipts()
	mark := fresh.restoreFenceFloor()
	w := fc.do("DELETE", "/api/v1/admin/users/"+itoa(a.ID), map[string]any{"attemptId": id}, true)
	if w.Code != http.StatusConflict || codeOf(t, w) != "attempt_expired" {
		t.Fatalf("submission to the new App: %d %s, want 409 attempt_expired", w.Code, w.Body.String())
	}
	sw := fc.do("POST", "/api/v1/admin/operation-attempts/settle",
		map[string]any{"attemptId": id, "kind": attemptKindAccountDelete, "subjectUserId": a.ID}, true)
	var got attemptSettleJSON
	decodeJSON(t, sw, &got)
	wantSettle(t, got, settleHistoryUnavailable, nil)
	if s.counter() != counter || s.receipts() != receipts || fresh.restoreFenceFloor() != mark || s.deleted(a.ID) {
		t.Fatal("the new App's answers moved the counter, the receipts or the account before any transaction")
	}
}

// isRestoreFenceStatement reports whether wire SQL is one of the restore
// fence's two statements. The key travels as a bind parameter, so the text is
// recognised by the statements themselves.
func isRestoreFenceStatement(sql string) bool {
	return strings.Contains(sql, "restore_fence") || strings.Contains(sql, "(value::bigint + 1)::text") ||
		strings.Contains(sql, "INSERT INTO settings (key, value) VALUES (")
}

// fakeDockerScript stands in for docker in the wrapper's argument-level tests.
// It logs every call (a "---" line, then one argument per line) and answers from
// FAKE_* variables: FAKE_DOCKER_INFO (ok, down, hang), FAKE_DOCKER_PULL (ok,
// fail, hang), FAKE_DOCKER_PRESENT=1 (the image is already local),
// FAKE_DOCKER_ID, FAKE_PGREADY_RC, FAKE_PGREADY_FIRST_RC, FAKE_TOOL_RC, FAKE_PG_MAJOR.
//
// FAKE_STUBBORN (info, pull, probe, tool) makes that call the worst case for a
// deadline: the process ignores SIGTERM, has a child in its own group that
// does too, and starts a "container" that only `docker rm -f <name>` ends (as
// the daemon owns a real one; it has a group of its own). Every pid goes to
// pids and "started" appears once they are up. Each ends itself after a minute,
// so a test that fails leaves nothing behind for long.
//
// FAKE_ORPHAN (probe, tool) is the other failure: the docker client starts the
// "container" and then dies with FAKE_ORPHAN_RC (default 5), leaving the daemon's
// container running. A container is the file c.<name> holding the pid of its
// group; "docker ps" lists, and "docker rm -f" ends, only those by name, and
// rm of a name nobody has says so and exits 1, as docker does.
// FAKE_RM=fail makes every rm refuse and leave the container alone;
// FAKE_RM=slow_first makes the first rm hang (it writes rm.began), the later ones work;
// FAKE_RM=hang and FAKE_PS=hang make every rm, or every ps, hang. A hung call's pid
// goes to the file hung, not pids, because the container it was to remove stays.
const fakeDockerScript = `#!/usr/bin/env bash
{ echo "---"; for a in "$@"; do printf '%s\n' "$a"; done; } >>"$FAKE_DOCKER_DIR/log"
id=${FAKE_DOCKER_ID:-sha256:feedbeef}
spin='trap "" TERM; echo $$ >>"$1"; i=0; while [ $i -lt 60 ]; do sleep 1; i=$((i + 1)); done'
container() { # name: a "container" in a process group of its own, as the daemon owns one
	set -m
	bash -c "$spin" spin "$FAKE_DOCKER_DIR/pids" >/dev/null 2>&1 </dev/null &
	echo $! >"$FAKE_DOCKER_DIR/c.$1"
	echo $! >>"$FAKE_DOCKER_DIR/pids"
	set +m
}
stubborn() { # container name, or "" for none
	trap '' TERM
	bash -c "$spin" spin "$FAKE_DOCKER_DIR/pids" >/dev/null 2>&1 </dev/null &
	if [[ -n $1 ]]; then container "$1"; fi
	echo $$ >>"$FAKE_DOCKER_DIR/pids"
	: >"$FAKE_DOCKER_DIR/started"
	wait
	exit 1
}
orphan() { # container name: the client starts the container and dies
	container "$1"
	: >"$FAKE_DOCKER_DIR/started"
	exit "${FAKE_ORPHAN_RC:-5}"
}
case $1 in
info)
	case ${FAKE_DOCKER_INFO:-ok} in
	down) echo "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?" >&2; exit 1 ;;
	hang) exec sleep 30 ;;
	esac
	if [[ ${FAKE_STUBBORN:-} == info ]]; then stubborn ""; fi
	;;
image)
	if [[ ${FAKE_DOCKER_PRESENT:-} == 1 || -e $FAKE_DOCKER_DIR/pulled || ${@: -1} == "$id" ]]; then
		[[ ${@: -1} == "$id" && -n ${FAKE_DOCKER_GONE:-} ]] && { echo "Error: No such image" >&2; exit 1; }
		echo "$id"
	else
		echo "Error: No such image: ${@: -1}" >&2
		exit 1
	fi
	;;
pull)
	case ${FAKE_DOCKER_PULL:-ok} in
	fail) echo "Error response from daemon: manifest unknown" >&2; exit 1 ;;
	hang) exec sleep 30 ;;
	esac
	if [[ ${FAKE_STUBBORN:-} == pull ]]; then stubborn ""; fi
	: >"$FAKE_DOCKER_DIR/pulled"
	;;
rm)
	# rm -f <name>: the daemon ends the container, group and all.
	name=${@: -1}
	if [[ ${FAKE_RM:-} == hang ]]; then
		echo $$ >>"$FAKE_DOCKER_DIR/hung"
		exec sleep 60
	fi
	if [[ ${FAKE_RM:-} == fail ]]; then
		echo "Error response from daemon: permission denied" >&2
		exit 1
	fi
	if [[ ${FAKE_RM:-} == slow_first && ! -e $FAKE_DOCKER_DIR/rm.slow ]]; then
		: >"$FAKE_DOCKER_DIR/rm.slow"
		echo $$ >>"$FAKE_DOCKER_DIR/pids"
		: >"$FAKE_DOCKER_DIR/rm.began"
		exec sleep 60
	fi
	if [[ ! -f $FAKE_DOCKER_DIR/c.$name ]]; then
		echo "Error response from daemon: No such container: $name" >&2
		exit 1
	fi
	p=$(cat "$FAKE_DOCKER_DIR/c.$name")
	kill -KILL -- "-$p" 2>/dev/null
	kill -KILL "$p" 2>/dev/null
	rm -f "$FAKE_DOCKER_DIR/c.$name"
	exit 0
	;;
ps)
	# ps -a --filter name=<part> --format {{.Names}}: the names that contain it.
	if [[ ${FAKE_PS:-} == hang ]]; then
		echo $$ >>"$FAKE_DOCKER_DIR/hung"
		exec sleep 60
	fi
	part=
	for a in "$@"; do [[ $a == name=* ]] && part=${a#name=}; done
	for f in "$FAKE_DOCKER_DIR"/c.*; do
		[[ -e $f ]] || continue
		n=${f##*/c.}
		[[ $n == *"$part"* ]] && echo "$n"
	done
	exit 0
	;;
run)
	shift
	cname=
	while [[ $1 != --entrypoint ]]; do
		[[ $1 == --name ]] && cname=$2
		shift
	done
	entry=$2
	shift 3 # --entrypoint, its value, the image
	if [[ $entry == pg_isready ]]; then
		if [[ ${FAKE_STUBBORN:-} == probe ]]; then stubborn "$cname"; fi
		if [[ ${FAKE_ORPHAN:-} == probe ]]; then FAKE_ORPHAN_RC=${FAKE_ORPHAN_RC:-2} orphan "$cname"; fi
		# Like the real one run as a uid with no passwd entry: without -U it
		# cannot name a user and exits 3, "no attempt made".
		named=
		for a in "$@"; do [[ $a == -U ]] && named=1; done
		[[ -n $named ]] || exit 3
		if [[ -n ${FAKE_PGREADY_FIRST_RC:-} && ! -e $FAKE_DOCKER_DIR/probed ]]; then
			: >"$FAKE_DOCKER_DIR/probed"
			exit "$FAKE_PGREADY_FIRST_RC"
		fi
		if [[ ${FAKE_PGREADY_RC:-} == 125 ]]; then
			printf "%s\n\n%s\n" "docker: host networking unavailable" "Run 'docker run --help' for more information" >&2
		fi
		exit "${FAKE_PGREADY_RC:-0}"
	fi
	if [[ ${FAKE_STUBBORN:-} == tool ]]; then stubborn "$cname"; fi
	if [[ ${FAKE_ORPHAN:-} == tool ]]; then orphan "$cname"; fi
	if [[ ${FAKE_TOOL_RC:-0} != 0 ]]; then exit "$FAKE_TOOL_RC"; fi
	if [[ ${1:-} == --version ]]; then echo "$entry (PostgreSQL) ${FAKE_PG_MAJOR:-16}.4"; fi
	;;
esac
`

type dockerCall []string

type fakeDocker struct {
	dir  string
	path string // PATH for the wrapper
}

// newFakeDocker returns a PATH holding the fake docker (when withDocker) and
// just the tools the wrapper uses, so a missing docker is really missing.
func newFakeDocker(t *testing.T, withDocker bool) *fakeDocker {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"bash", "env", "id", "dirname", "mktemp", "rm", "tail", "head", "cut", "cat", "sleep"} {
		p, err := exec.LookPath(tool)
		if err != nil {
			t.Fatalf("the wrapper tests need %s: %v", tool, err)
		}
		if err := os.Symlink(p, filepath.Join(bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	if withDocker {
		if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(fakeDockerScript), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return &fakeDocker{dir: dir, path: bin}
}

// wrap runs scripts/pg-client.sh with env (KEY=value pairs) and returns its
// combined output, its exit code and the docker calls it made.
func (f *fakeDocker) wrap(t *testing.T, env []string, args ...string) (string, int, []dockerCall) {
	t.Helper()
	cmd := exec.Command(pgWrapperPath(), args...)
	cmd.Env = []string{"PATH=" + f.path, "TMPDIR=" + f.dir, "FAKE_DOCKER_DIR=" + f.dir}
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		if code = exitCodeOf(err); code < 0 {
			t.Fatalf("running the wrapper: %v", err)
		}
	}
	return string(out), code, f.calls(t)
}

func (f *fakeDocker) calls(t *testing.T) []dockerCall {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir, "log"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var calls []dockerCall
	for _, block := range strings.Split(string(b), "---\n")[1:] {
		calls = append(calls, dockerCall(strings.Split(strings.TrimSuffix(block, "\n"), "\n")))
	}
	return calls
}

// has reports whether the call carries these arguments next to each other.
func (c dockerCall) has(seq ...string) bool {
	for i := 0; i+len(seq) <= len(c); i++ {
		if strings.Join(c[i:i+len(seq)], "\x00") == strings.Join(seq, "\x00") {
			return true
		}
	}
	return false
}

func (c dockerCall) count(arg string) int {
	n := 0
	for _, a := range c {
		if a == arg {
			n++
		}
	}
	return n
}

// entrypoint is the tool a `docker run` call starts, "" for any other call.
func (c dockerCall) entrypoint() string {
	for i, a := range c {
		if a == "--entrypoint" && i+1 < len(c) {
			return c[i+1]
		}
	}
	return ""
}

// afterImage is what follows the image in a `docker run` call: the tool's arguments.
func (c dockerCall) afterImage() []string {
	for i, a := range c {
		if a == "--entrypoint" && i+3 <= len(c) {
			return c[i+3:]
		}
	}
	return nil
}

func callsFor(calls []dockerCall, verb string) []dockerCall {
	var out []dockerCall
	for _, c := range calls {
		if c[0] == verb {
			out = append(out, c)
		}
	}
	return out
}

// toolRun is the last container call, the one that runs the tool itself.
func toolRun(t *testing.T, calls []dockerCall, tool string) dockerCall {
	t.Helper()
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i][0] == "run" && calls[i].entrypoint() == tool {
			return calls[i]
		}
	}
	t.Fatalf("no container run of %s in %v", tool, calls)
	return nil
}

func TestPGClientWrapperMountsEachDirectoryOnceAndKeepsSpaces(t *testing.T) {
	f := newFakeDocker(t, true)
	root := t.TempDir()
	spaced := filepath.Join(root, "a b")
	other := filepath.Join(root, "c  d")
	for _, d := range []string{spaced, other} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	args := []string{"-Fc", "-f", filepath.Join(spaced, "one.dump"), "--file=" + filepath.Join(spaced, "two.dump"),
		filepath.Join(other, "three.dump"), filepath.Join(spaced, "four.dump"), "name with spaces"}
	out, code, calls := f.wrap(t, nil, append([]string{"16", "pg_dump"}, args...)...)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	run := toolRun(t, calls, "pg_dump")
	mounts := 0
	for i, a := range run {
		if a == "-v" {
			mounts++
			if run[i+1] != spaced+":"+spaced && run[i+1] != other+":"+other {
				t.Errorf("unexpected mount %q", run[i+1])
			}
		}
	}
	if mounts != 2 || !run.has("-v", spaced+":"+spaced) || !run.has("-v", other+":"+other) {
		t.Errorf("want each directory mounted once, got %d mounts in %v", mounts, run)
	}
	if got := run.afterImage(); strings.Join(got, "\x00") != strings.Join(args, "\x00") {
		t.Errorf("the tool's arguments changed: %q, want %q", got, args)
	}
	if !run.has("--network", "host") {
		t.Errorf("no connection in the arguments should keep --network host: %v", run)
	}
}

func TestPGClientWrapperWithoutMountsRuns(t *testing.T) {
	f := newFakeDocker(t, true)
	out, code, calls := f.wrap(t, nil, "16", "pg_dump", "--version")
	if code != 0 || !strings.Contains(out, "pg_dump (PostgreSQL) 16.4") {
		t.Fatalf("exit %d: %s", code, out)
	}
	if run := toolRun(t, calls, "pg_dump"); run.count("-v") != 0 {
		t.Errorf("no path among the arguments, yet mounts: %v", run)
	}
}

func TestPGClientWrapperRefusesAColonInAMountedPath(t *testing.T) {
	f := newFakeDocker(t, true)
	dir := filepath.Join(t.TempDir(), "a:b")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	out, code, calls := f.wrap(t, nil, "16", "pg_dump", "-f", filepath.Join(dir, "x.dump"))
	if code != 74 || !strings.Contains(out, "colon") {
		t.Errorf("exit %d, want 74 naming the colon: %s", code, out)
	}
	if len(callsFor(calls, "run")) != 0 {
		t.Error("the tool ran despite the bad path")
	}
}

// TestPGClientWrapperIsBash32Compatible: macOS ships Bash 3.2, which has no
// associative arrays and fails on an empty array under `set -u`. No Bash 3.2 runs
// in CI, so this reads the script for what that Bash lacks.
func TestPGClientWrapperIsBash32Compatible(t *testing.T) {
	b, err := os.ReadFile(pgWrapperPath())
	if err != nil {
		t.Fatal(err)
	}
	var code []string
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") {
			code = append(code, line)
		}
	}
	src := strings.Join(code, "\n")
	for _, bad := range []*regexp.Regexp{
		regexp.MustCompile(`\b(declare|typeset|local)\s+-[a-zA-Z]*[AnN]`),
		regexp.MustCompile(`\b(mapfile|readarray|coproc)\b`),
		regexp.MustCompile(`\$\{[^}]*(,,|\^\^)[^}]*\}`),
		regexp.MustCompile(`&>>|\|&|;;&|;&|\[\[\s+-v\b`),
		regexp.MustCompile(`\bwait\s+-[a-z]*n|\$\{?BASHPID\b|\$\{?EPOCHSECONDS\b`), // 4.3, 4.0, 5.0
	} {
		if m := bad.FindString(src); m != "" {
			t.Errorf("scripts/pg-client.sh uses %q, which Bash 3.2 does not have", m)
		}
	}
	// An array may be expanded only as ${a[@]+"${a[@]}"}: a bare "${a[@]}" is an
	// unbound variable under `set -u` when the array is empty.
	guarded := regexp.MustCompile(`\$\{(\w+)\[@\]\+"\$\{(\w+)\[@\]\}"\}`)
	for _, m := range guarded.FindAllStringSubmatch(src, -1) {
		if m[1] != m[2] {
			t.Errorf("%s guards a different array than it expands", m[0])
		}
	}
	rest := guarded.ReplaceAllString(src, "")
	for _, m := range regexp.MustCompile(`"?\$\{(\w+)\[@\]\}"?`).FindAllStringSubmatch(rest, -1) {
		if m[1] != "net" { // net always holds --network and its value
			t.Errorf("%s can be empty under set -u: expand it as ${%s[@]+\"${%s[@]}\"}", m[0], m[1], m[1])
		}
	}
	if out, err := exec.Command("bash", "-n", pgWrapperPath()).CombinedOutput(); err != nil {
		t.Errorf("bash -n: %v\n%s", err, out)
	}
}

func TestPGClientWrapperDiagnosesEachStage(t *testing.T) {
	const password = "s3cretpw"
	url := "postgres://app:" + password + "@localhost:5433/db"
	for _, tc := range []struct {
		name       string
		noDocker   bool
		env        []string
		args       []string
		wantCode   int
		wantText   string
		wantNoTool bool // the tool itself must not have run
		wantMax    time.Duration
	}{
		{name: "no container runtime", noDocker: true, args: []string{"16", "pg_dump", "--version"},
			wantCode: 70, wantText: "no container runtime", wantNoTool: true},
		{name: "daemon stopped", env: []string{"FAKE_DOCKER_INFO=down"}, args: []string{"16", "pg_dump", "--version"},
			wantCode: 71, wantText: "daemon is not reachable", wantNoTool: true},
		{name: "daemon silent", env: []string{"FAKE_DOCKER_INFO=hang", "FOLD_PG_INFO_TIMEOUT=1"}, args: []string{"16", "pg_dump", "--version"},
			wantCode: 71, wantText: "did not answer within 1s", wantNoTool: true, wantMax: 15 * time.Second},
		{name: "pull fails", env: []string{"FAKE_DOCKER_PULL=fail"}, args: []string{"16", "pg_dump", "--version"},
			wantCode: 72, wantText: "pulling image postgres:16 failed (Error response from daemon: manifest unknown)", wantNoTool: true},
		{name: "pull is bounded", env: []string{"FAKE_DOCKER_PULL=hang", "FOLD_PG_PULL_TIMEOUT=1"}, args: []string{"16", "pg_dump", "--version"},
			wantCode: 72, wantText: "pulling image postgres:16 did not finish within 1s", wantNoTool: true, wantMax: 15 * time.Second},
		{name: "tool missing from the image", env: []string{"FAKE_TOOL_RC=127"}, args: []string{"16", "pg_dump", "--version"},
			wantCode: 72, wantText: "could not start pg_dump"},
		{name: "database unreachable", env: []string{"FAKE_PGREADY_RC=2"}, args: []string{"16", "pg_dump", url},
			wantCode: 73, wantText: "localhost port 5433 does not answer from inside the container after two pg_isready attempts", wantNoTool: true},
		{name: "host network refused by docker", env: []string{"FAKE_PGREADY_RC=125"}, args: []string{"16", "pg_dump", url},
			wantCode: 72, wantText: "Docker could not start the pg_isready container (network mode host): docker: host networking unavailable", wantNoTool: true},
		{name: "bad timeout", env: []string{"FOLD_PG_PULL_TIMEOUT=soon"}, args: []string{"16", "pg_dump", "--version"},
			wantCode: 74, wantText: "FOLD_PG_PULL_TIMEOUT must be a whole number", wantNoTool: true},
		{name: "no host", args: []string{"16", "pg_dump", "postgres://app:" + password + "@/db"},
			wantCode: 74, wantText: "names no host", wantNoTool: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeDocker(t, !tc.noDocker)
			start := time.Now()
			out, code, calls := f.wrap(t, tc.env, tc.args...)
			if code != tc.wantCode || !strings.Contains(out, tc.wantText) {
				t.Errorf("exit %d: %s\nwant exit %d containing %q", code, out, tc.wantCode, tc.wantText)
			}
			if tc.wantMax > 0 && time.Since(start) > tc.wantMax {
				t.Errorf("took %v, want under %v", time.Since(start), tc.wantMax)
			}
			if strings.Contains(out, password) {
				t.Errorf("the output carries the password: %s", out)
			}
			if tc.wantNoTool {
				for _, c := range callsFor(calls, "run") {
					if c.entrypoint() == "pg_dump" {
						t.Errorf("pg_dump ran despite the failure: %v", c)
					}
				}
			}
		})
	}
}

func TestPGClientWrapperUsesTheLocalImageWithoutPulling(t *testing.T) {
	f := newFakeDocker(t, true)
	out, code, calls := f.wrap(t, []string{"FAKE_DOCKER_PRESENT=1"}, "16", "pg_dump", "--version")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if n := len(callsFor(calls, "pull")); n != 0 {
		t.Errorf("pulled %d times although the image is local", n)
	}
}

func TestPGClientWrapperRunsByImageID(t *testing.T) {
	t.Run("pulls once and runs the ID, not the tag", func(t *testing.T) {
		f := newFakeDocker(t, true)
		_, code, calls := f.wrap(t, nil, "16", "pg_dump", "--version")
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
		pulls := callsFor(calls, "pull")
		if len(pulls) != 1 || !pulls[0].has("postgres:16") {
			t.Errorf("pulls %v, want one of postgres:16", pulls)
		}
		run := toolRun(t, calls, "pg_dump")
		if !run.has("--entrypoint", "pg_dump", "sha256:feedbeef") || run.count("postgres:16") != 0 {
			t.Errorf("the tool should run on the image ID: %v", run)
		}
	})
	t.Run("--resolve prints the ID", func(t *testing.T) {
		f := newFakeDocker(t, true)
		out, code, calls := f.wrap(t, nil, "--resolve", "16")
		if code != 0 || !strings.Contains(out, "sha256:feedbeef") {
			t.Fatalf("exit %d: %s", code, out)
		}
		for _, c := range callsFor(calls, "run") {
			t.Errorf("--resolve started a container: %v", c)
		}
	})
	t.Run("a recorded ID is used as it is", func(t *testing.T) {
		f := newFakeDocker(t, true)
		_, code, calls := f.wrap(t, []string{"FOLD_PG_IMAGE_ID=sha256:feedbeef"}, "16", "pg_dump", "--version")
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
		if n := len(callsFor(calls, "pull")); n != 0 {
			t.Errorf("pulled %d times although an ID was recorded", n)
		}
		if run := toolRun(t, calls, "pg_dump"); !run.has("--entrypoint", "pg_dump", "sha256:feedbeef") {
			t.Errorf("the recorded ID is not the image run: %v", run)
		}
	})
	t.Run("a recorded ID that is gone is an error, not a pull", func(t *testing.T) {
		f := newFakeDocker(t, true)
		out, code, calls := f.wrap(t, []string{"FOLD_PG_IMAGE_ID=sha256:feedbeef", "FAKE_DOCKER_GONE=1"}, "16", "pg_dump", "--version")
		if code != 72 || !strings.Contains(out, "is not available locally") {
			t.Errorf("exit %d: %s", code, out)
		}
		if len(callsFor(calls, "pull")) != 0 {
			t.Error("pulled instead of failing")
		}
	})
	t.Run("an explicit digest is kept as given", func(t *testing.T) {
		f := newFakeDocker(t, true)
		digest := "postgres@sha256:" + strings.Repeat("ab", 32)
		_, code, calls := f.wrap(t, []string{"FOLD_PG_IMAGE=" + digest}, "16", "pg_dump", "--version")
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
		if pulls := callsFor(calls, "pull"); len(pulls) != 1 || !pulls[0].has(digest) {
			t.Errorf("pulls %v, want the digest %s exactly", pulls, digest)
		}
	})
}

// pctEncode percent-encodes every byte of s but letters, digits and "-._", the
// way a client would write a socket directory into a URL.
func pctEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func TestPGClientWrapperConnectionForms(t *testing.T) {
	root := t.TempDir()
	sock := filepath.Join(root, "sock")
	spacedSock := filepath.Join(root, "sock dir")
	for _, d := range []string{sock, spacedSock} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	type probe struct{ host, port string }
	for _, tc := range []struct {
		name    string
		args    []string
		network string // the container network, for the probe and the tool
		mount   string // the one directory mounted, "" for none
		probe   probe
	}{
		{name: "TCP URL", args: []string{"postgres://u:p@localhost:5433/db"}, network: "host", probe: probe{"localhost", "5433"}},
		{name: "TCP URL default port", args: []string{"postgres://u@127.0.0.1/db"}, network: "host", probe: probe{"127.0.0.1", "5432"}},
		{name: "TCP IPv6", args: []string{"postgres://u@[::1]:5434/db"}, network: "host", probe: probe{"::1", "5434"}},
		{name: "TCP -h and -p", args: []string{"-h", "db.internal", "-p", "5435", "dbname"}, network: "host", probe: probe{"db.internal", "5435"}},
		{name: "TCP conninfo", args: []string{"-d", "host=db.internal port=5436 dbname=x"}, network: "host", probe: probe{"db.internal", "5436"}},
		{name: "socket in the URL query", args: []string{"postgres://u@localhost/db?host=" + sock}, network: "none", mount: sock, probe: probe{sock, "5432"}},
		{name: "socket in the URL authority", args: []string{"postgres://u@" + strings.ReplaceAll(sock, "/", "%2F") + ":5437/db"}, network: "none", mount: sock, probe: probe{sock, "5437"}},
		{name: "socket with -h", args: []string{"-h", spacedSock, "-p", "5438", "dbname"}, network: "none", mount: spacedSock, probe: probe{spacedSock, "5438"}},
		{name: "socket conninfo", args: []string{"-d", "host=" + sock + " dbname=x"}, network: "none", mount: sock, probe: probe{sock, "5432"}},
		// libpq percent-decodes a URL's parts; the probe, the network and the mount
		// follow the decoded values, the tool keeps the URL as written.
		{name: "TCP URL with an encoded host and port", args: []string{"postgres://u:p@%31%32%37.0.0.1:%35433/db"}, network: "host", probe: probe{"127.0.0.1", "5433"}},
		{name: "TCP URL with an encoded host and port in the query", args: []string{"postgres://u@/db?host=db%2Einternal&port=%35434"}, network: "host", probe: probe{"db.internal", "5434"}},
		{name: "TCP URL with an encoded colon and at sign in the password", args: []string{"postgres://u:p%40ss%3A1@localhost:5433/db"}, network: "host", probe: probe{"localhost", "5433"}},
		{name: "TCP URL with host-like query in password", args: []string{"postgres://u:secret?host=hidden@localhost:5433/db"}, network: "host", probe: probe{"localhost", "5433"}},
		{name: "TCP URL with port-like query in password", args: []string{"postgres://u:secret?port=9999@localhost:5433/db"}, network: "host", probe: probe{"localhost", "5433"}},
		{name: "TCP URL with a literal percent sign", args: []string{"postgres://u@db%25x.internal/db"}, network: "host", probe: probe{"db%x.internal", "5432"}},
		{name: "socket with a blank, in the URL query", args: []string{"postgres://u@/db?host=" + pctEncode(spacedSock)}, network: "none", mount: spacedSock, probe: probe{spacedSock, "5432"}},
		{name: "socket with a blank, in the URL authority", args: []string{"postgres://u@" + pctEncode(spacedSock) + ":5439/db"}, network: "none", mount: spacedSock, probe: probe{spacedSock, "5439"}},
		{name: "socket with a blank, in an encoded query key", args: []string{"postgres://u@/db?%68ost=" + pctEncode(spacedSock)}, network: "none", mount: spacedSock, probe: probe{spacedSock, "5432"}},
		{name: "socket with a blank, in lower-case hex", args: []string{"postgres://u@/db?host=" + strings.ReplaceAll(pctEncode(spacedSock), "%2F", "%2f")}, network: "none", mount: spacedSock, probe: probe{spacedSock, "5432"}},
		{name: "repeated host socket to TCP", args: []string{"postgres://u@/db?host=" + pctEncode(sock) + "&host=127.0.0.1"}, network: "host", probe: probe{"127.0.0.1", "5432"}},
		{name: "repeated host TCP to socket", args: []string{"postgres://u@/db?host=127.0.0.1&%68ost=" + pctEncode(spacedSock)}, network: "none", mount: spacedSock, probe: probe{spacedSock, "5432"}},
		{name: "repeated port", args: []string{"postgres://u@localhost/db?port=5433&%70ort=5434"}, network: "host", probe: probe{"localhost", "5434"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeDocker(t, true)
			out, code, calls := f.wrap(t, nil, append([]string{"16", "pg_dump"}, tc.args...)...)
			if code != 0 {
				t.Fatalf("exit %d: %s", code, out)
			}
			if strings.Contains(tc.name, "query in password") && strings.Contains(out, "secret") {
				t.Error("wrapper output contained a password fragment")
			}
			ready := toolRun(t, calls, "pg_isready")
			run := toolRun(t, calls, "pg_dump")
			for name, c := range map[string]dockerCall{"probe": ready, "tool": run} {
				if !c.has("--network", tc.network) {
					t.Errorf("%s: want --network %s in %v", name, tc.network, c)
				}
				if tc.mount == "" && c.count("-v") != 0 {
					t.Errorf("%s: unexpected mount in %v", name, c)
				}
				if tc.mount != "" && (c.count("-v") != 1 || !c.has("-v", tc.mount+":"+tc.mount)) {
					t.Errorf("%s: want only %s mounted, got %v", name, tc.mount, c)
				}
			}
			if !ready.has("-h", tc.probe.host, "-p", tc.probe.port) {
				t.Errorf("probe asked the wrong place: %v, want -h %s -p %s", ready, tc.probe.host, tc.probe.port)
			}
			if got := run.afterImage(); strings.Join(got, "\x00") != strings.Join(tc.args, "\x00") {
				t.Errorf("the tool's arguments changed: %q, want %q", got, tc.args)
			}
		})
	}

	t.Run("a socket directory that is not there", func(t *testing.T) {
		f := newFakeDocker(t, true)
		out, code, _ := f.wrap(t, nil, "16", "pg_dump", "-h", filepath.Join(root, "missing"), "dbname")
		if code != 74 || !strings.Contains(out, "does not exist") {
			t.Errorf("exit %d: %s", code, out)
		}
	})
}

// TestPGClientWrapperProbeNamesAUser: the container runs as the caller's uid,
// which the image has no passwd entry for, so a pg_isready with no -U exits 3
// ("no attempt") before it connects. The fake docker reproduces that, so a probe
// without -U fails here as it did against a real server.
func TestPGClientWrapperProbeNamesAUser(t *testing.T) {
	f := newFakeDocker(t, true)
	out, code, calls := f.wrap(t, nil, "16", "pg_dump", "postgres://u:p@127.0.0.1:5433/db")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if ready := toolRun(t, calls, "pg_isready"); !ready.has("-U", "probe") {
		t.Errorf("the probe names no user: %v", ready)
	}

	// Exit 3 from a probe is not a silent database: it gets its own message and
	// never the "unreachable" diagnosis (73).
	out, code, _ = f.wrap(t, []string{"FAKE_PGREADY_RC=3"}, "16", "pg_dump", "postgres://u:p@127.0.0.1:5433/db")
	if code != 74 || !strings.Contains(out, "made no attempt (exit 3)") || strings.Contains(out, "rejected") {
		t.Errorf("exit %d: %s", code, out)
	}
}

func TestPGClientWrapperRetriesOnlyTheProbe(t *testing.T) {
	f := newFakeDocker(t, true)
	out, code, calls := f.wrap(t, []string{"FAKE_PGREADY_FIRST_RC=2", "FAKE_TOOL_RC=5"},
		"16", "pg_restore", "--exit-on-error", "-d", "postgres://u:p@127.0.0.1:5433/db", "x.dump")
	if code != 5 {
		t.Errorf("exit %d: %s\nwant the restore's exit 5 after the second probe answers", code, out)
	}
	var runs []string
	for _, c := range callsFor(calls, "run") {
		runs = append(runs, c.entrypoint())
	}
	if want := []string{"pg_isready", "pg_isready", "pg_restore"}; !slices.Equal(runs, want) {
		t.Errorf("ran %v, want %v", runs, want)
	}
}

// TestPGClientWrapperProbeWithRealDocker runs the real wrapper, docker and
// pg_isready against a local port nothing listens on. The probe must get as far
// as connecting and report silence (73), not "no attempt" (74). It skips where
// Docker is not usable, as the restore tests do, and fails there under CI.
func TestPGClientWrapperProbeWithRealDocker(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("docker is not on PATH")
		}
		t.Skip("docker is not on PATH")
	}
	wrapper := pgWrapperPath()
	id, note, unavailable := resolvePGImage(wrapper, 16)
	if id == "" {
		if unavailable && os.Getenv("CI") == "" {
			t.Skipf("Docker is not usable: %s", note)
		}
		t.Fatalf("no image to probe with: %s", note)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close() // the port is now closed and, for the next seconds, unused

	cmd := exec.Command(wrapper, "16", "pg_dump", "-f", filepath.Join(t.TempDir(), "x.dump"),
		fmt.Sprintf("postgres://probe:probe@127.0.0.1:%d/probe", port))
	cmd.Env = append(os.Environ(), "FOLD_PG_IMAGE_ID="+id, "FOLD_PG_INFO_TIMEOUT=60")
	out, err := cmd.CombinedOutput()
	if code := exitCodeOf(err); code != 73 || !strings.Contains(string(out), "does not answer from inside the container") {
		t.Errorf("exit %d: %s\nwant 73, the probe saw a closed port", code, out)
	}
}

// TestPGClientWrapperRefusesConninfoItCannotRead: a connection string that
// libpq reads with quotes, backslashes or blanks around "=" cannot be split on
// blanks, because a password can hold "port=5432" or "host=/tmp". The wrapper
// refuses it before taking a word from it, with a message that says nothing of
// its content, and starts no container.
func TestPGClientWrapperRefusesConninfoItCannotRead(t *testing.T) {
	root := t.TempDir()
	sock := filepath.Join(root, "sock")
	if err := os.Mkdir(sock, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
		leak []string // fragments of the string the output must not repeat
	}{
		{"port inside a quoted password", []string{"-d", "host=127.0.0.1 user=app dbname=db password='prefix port=pa55w0rd'"}, []string{"pa55w0rd", "prefix"}},
		{"host inside a quoted password", []string{"-d", "dbname=db password='prefix host=" + sock + " suffix'"}, []string{"prefix", "suffix"}},
		{"host inside a double-quoted password", []string{"-d", `user=app password="pa55 host=` + sock + `" host=db.internal`}, []string{"pa55"}},
		{"port first inside a quoted password", []string{"-d", "port='1 pa55w0rd' host=db.internal"}, []string{"pa55w0rd"}},
		{"a backslash-escaped blank", []string{"-d", `host=db.internal password=pa55\ port=5432`}, []string{"pa55"}},
		{"an escaped quote", []string{"-d", `host=db.internal password=pa55\'w0rd`}, []string{"pa55", "w0rd"}},
		{"blanks around =", []string{"-d", "host = db.internal port=5432"}, []string{"db.internal"}},
		{"a blank after =", []string{"-d", "host= db.internal port=5432"}, []string{"db.internal"}},
		{"a blank before =", []string{"-d", "host =db.internal port=5432"}, []string{"db.internal"}},
		{"an empty value", []string{"-d", "dbname= host=db.internal"}, []string{"db.internal"}},
		{"a carriage return", []string{"-d", "host=db.internal\rport=5432"}, []string{"db.internal"}},
		{"a bare string", []string{"host=db.internal password='pa55w0rd'"}, []string{"pa55w0rd"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeDocker(t, true)
			out, code, calls := f.wrap(t, nil, append([]string{"16", "pg_dump"}, tc.args...)...)
			if code != 74 || !strings.Contains(out, "FOLD_PG_BIN") || !strings.Contains(out, "does not read") {
				t.Errorf("exit %d: %s\nwant 74 pointing at a URL or FOLD_PG_BIN", code, out)
			}
			for _, leak := range tc.leak {
				if strings.Contains(out, leak) {
					t.Errorf("the output repeats %q from the connection string: %s", leak, out)
				}
			}
			for _, c := range callsFor(calls, "run") {
				t.Errorf("a container started for a connection string that was refused: %v", c)
			}
		})
	}

	// Plain strings keep working; a word that only looks like a setting inside
	// another setting's value is not one.
	t.Run("host= inside an unquoted value", func(t *testing.T) {
		f := newFakeDocker(t, true)
		out, code, calls := f.wrap(t, nil, "16", "pg_dump", "-d", "host=db.internal password=xhost="+sock+" port=5436 dbname=x")
		if code != 0 {
			t.Fatalf("exit %d: %s", code, out)
		}
		ready := toolRun(t, calls, "pg_isready")
		if !ready.has("-h", "db.internal", "-p", "5436") || !ready.has("--network", "host") || ready.count("-v") != 0 {
			t.Errorf("the probe followed a word inside a password: %v", ready)
		}
	})
	t.Run("a port that is not a number is not repeated", func(t *testing.T) {
		f := newFakeDocker(t, true)
		out, code, _ := f.wrap(t, nil, "16", "pg_dump", "postgres://u:pw@localhost:pa55w0rd/db")
		if code != 74 || !strings.Contains(out, "not a number") || strings.Contains(out, "pa55w0rd") {
			t.Errorf("exit %d: %s", code, out)
		}
	})
	t.Run("a path named like a host setting is a path", func(t *testing.T) {
		f := newFakeDocker(t, true)
		out, code, calls := f.wrap(t, nil, "16", "pg_dump", "-f", filepath.Join(sock, "host=a b'c.dump"))
		if code != 0 {
			t.Fatalf("exit %d: %s", code, out)
		}
		run := toolRun(t, calls, "pg_dump")
		if len(callsFor(calls, "run")) != 1 || !run.has("-v", sock+":"+sock) || !run.has("--network", "host") {
			t.Errorf("a file argument was read as a connection: %v", calls)
		}
	})
}

// TestPGClientWrapperRefusesURLEncodingsItCannotTrust: a "%" without two hex
// digits, %00, a control character in the host or port, and a list of hosts are
// refused (74) before any container starts, and the refusal repeats nothing of the
// URL: libpq's own complaint about a bad escape repeats the part it was decoding,
// and the password is one.
func TestPGClientWrapperRefusesURLEncodingsItCannotTrust(t *testing.T) {
	for _, tc := range []struct {
		name string
		url  string
		want string
	}{
		{"a bad escape in the password", "postgres://u:pa55%zzw0rd@localhost/db", "two hex digits"},
		{"a one-digit escape in the password", "postgres://u:pa55w0rd%2@localhost/db", "two hex digits"},
		{"a trailing percent sign", "postgres://u:pa55w0rd%@localhost/db", "two hex digits"},
		{"a bad escape in the database name", "postgres://u:pa55w0rd@localhost/d%G0", "two hex digits"},
		{"a bad escape in an unrelated parameter", "postgres://u:pa55w0rd@localhost/db?sslmode=%zz", "two hex digits"},
		{"a NUL in the password", "postgres://u:pa55%00w0rd@localhost/db", "two hex digits"},
		{"a NUL in the host", "postgres://u:pa55w0rd@loc%00alhost/db", "two hex digits"},
		{"a bad escape in the host", "postgres://u:pa55w0rd@loc%4Xlhost/db", "two hex digits"},
		{"a newline in the host", "postgres://u:pa55w0rd@local%0Ahost/db", "control character"},
		{"a tab in the port", "postgres://u:pa55w0rd@localhost:54%0932/db", "control character"},
		{"a control character in the query host", "postgres://u:pa55w0rd@/db?host=%2Ftmp%1B", "control character"},
		{"DEL in the query port", "postgres://u:pa55w0rd@localhost/db?port=54%7F32", "control character"},
		{"an encoded comma in the host", "postgres://u:pa55w0rd@a%2Cb/db", "list of hosts"},
		{"a comma in the host", "postgres://u:pa55w0rd@a,b:5432/db", "list of hosts"},
		{"a comma in the query host", "postgres://u:pa55w0rd@/db?host=a%2Cb", "list of hosts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeDocker(t, true)
			out, code, calls := f.wrap(t, nil, "16", "pg_dump", tc.url)
			if code != 74 || !strings.Contains(out, tc.want) || !strings.Contains(out, "FOLD_PG_BIN") {
				t.Errorf("exit %d: %s\nwant 74 containing %q and a pointer to FOLD_PG_BIN", code, out, tc.want)
			}
			for _, leak := range []string{"pa55", "w0rd", "localhost", "%zz", "tmp"} {
				if strings.Contains(out, leak) {
					t.Errorf("the output repeats %q from the URL: %s", leak, out)
				}
			}
			for _, c := range callsFor(calls, "run") {
				t.Errorf("a container started for a URL that was refused: %v", c)
			}
		})
	}

	// A "+" is not a blank and "%2B" is a plus: libpq decodes nothing else.
	t.Run("a plus sign stays a plus sign", func(t *testing.T) {
		f := newFakeDocker(t, true)
		out, code, calls := f.wrap(t, nil, "16", "pg_dump", "postgres://u@a+b%2Bc.internal/db")
		if code != 0 {
			t.Fatalf("exit %d: %s", code, out)
		}
		if ready := toolRun(t, calls, "pg_isready"); !ready.has("-h", "a+b+c.internal") {
			t.Errorf("the probe asked the wrong host: %v", ready)
		}
	})
}

// pids reads the process ids the fake docker recorded.
func (f *fakeDocker) pids(t *testing.T) []int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir, "pids"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var pids []int
	for _, s := range strings.Fields(string(b)) {
		if n, err := strconv.Atoi(s); err == nil {
			pids = append(pids, n)
		}
	}
	return pids
}

// alive reports whether the process runs. A zombie is dead: it waits only to be
// reaped.
func alive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		if i := bytes.LastIndexByte(b, ')'); i >= 0 && i+2 < len(b) && b[i+2] == 'Z' {
			return false
		}
	}
	return true
}

// expectStopped fails unless every recorded process is gone within a second
// (they were killed already; this only waits for the kernel), and kills any that
// is not, so a failure does not leave them running.
func (f *fakeDocker) expectStopped(t *testing.T) {
	t.Helper()
	pids := f.pids(t)
	if len(pids) == 0 {
		t.Fatal("the fake docker started no process to stop: the scenario did not run")
	}
	deadline := time.Now().Add(time.Second)
	for _, pid := range pids {
		for alive(pid) && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if alive(pid) {
			t.Errorf("process %d is still running", pid)
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

// sawRemove reports whether the wrapper removed the container its run was
// given: a `docker rm -f <name>` for the --name of a `docker run`.
func sawRemove(calls []dockerCall) bool {
	names := runNames(calls)
	for _, c := range callsFor(calls, "rm") {
		if c.has("-f") && names[c[len(c)-1]] {
			return true
		}
	}
	return false
}

// runNames is the --name of every `docker run` the wrapper made.
func runNames(calls []dockerCall) map[string]bool {
	names := map[string]bool{}
	for _, c := range callsFor(calls, "run") {
		for i, a := range c {
			if a == "--name" && i+1 < len(c) {
				names[c[i+1]] = true
			}
		}
	}
	return names
}

// onlyRemovedItsOwn fails if the wrapper removed anything but a container it
// named in a `docker run`, or listed anything but by such a name.
func onlyRemovedItsOwn(t *testing.T, calls []dockerCall) {
	t.Helper()
	names := runNames(calls)
	for _, c := range callsFor(calls, "rm") {
		if !names[c[len(c)-1]] {
			t.Errorf("the wrapper removed something it did not start: %v", c)
		}
	}
	for _, c := range callsFor(calls, "ps") {
		if !c.has("-a") || strings.Count(strings.Join(c, " "), "name=pg-client-") != 1 {
			t.Errorf("the wrapper listed containers it did not start: %v", c)
		}
	}
}

// bystanders starts what a cleanup must never touch: a process in a group of its
// own, and a fake "container" under a name the wrapper did not make. The returned
// check fails if either was ended or named in a docker call.
func (f *fakeDocker) bystanders(t *testing.T) (check func(t *testing.T)) {
	t.Helper()
	start := func() *exec.Cmd {
		cmd := exec.Command("sleep", "60")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Wait()
		})
		return cmd
	}
	process, container := start(), start()
	const name = "bystander-postgres"
	marker := filepath.Join(f.dir, "c."+name)
	if err := os.WriteFile(marker, []byte(strconv.Itoa(container.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}
	return func(t *testing.T) {
		t.Helper()
		if !alive(process.Process.Pid) {
			t.Error("an unrelated process was killed")
		}
		if !alive(container.Process.Pid) {
			t.Error("an unrelated container's process was killed")
		}
		if _, err := os.Stat(marker); err != nil {
			t.Errorf("an unrelated container was removed: %v", err)
		}
		for _, c := range f.calls(t) {
			if strings.Contains(strings.Join(c, " "), name) {
				t.Errorf("a docker call named an unrelated container: %v", c)
			}
		}
	}
}

// TestPGClientWrapperDeadlinesHoldAgainstStubbornCommands: a command that ignores
// SIGTERM and starts children (one of them a container the daemon owns) must
// still end at its deadline plus the grace, with nothing of it left running and
// its container removed. A fixture that dies on TERM cannot show this.
func TestPGClientWrapperDeadlinesHoldAgainstStubbornCommands(t *testing.T) {
	url := "postgres://u:p@127.0.0.1:5433/db"
	for _, tc := range []struct {
		stage     string
		timeout   string
		args      []string
		wantCode  int
		wantText  string
		container bool
	}{
		{"info", "FOLD_PG_INFO_TIMEOUT", []string{"16", "pg_dump", "--version"}, 71, "did not answer within 1s", false},
		{"pull", "FOLD_PG_PULL_TIMEOUT", []string{"16", "pg_dump", "--version"}, 72, "did not finish within 1s", false},
		{"probe", "FOLD_PG_DISCOVERY_TIMEOUT", []string{"16", "pg_dump", url}, 72, "Docker did not finish running pg_isready for 127.0.0.1 port 5433 within 1s", true},
		{"tool", "FOLD_PG_DISCOVERY_TIMEOUT", []string{"16", "pg_dump", "--version"}, 72, "did not finish within 1s", true},
	} {
		t.Run(tc.stage, func(t *testing.T) {
			f := newFakeDocker(t, true)
			start := time.Now()
			out, code, calls := f.wrap(t, []string{"FAKE_STUBBORN=" + tc.stage, tc.timeout + "=1", "FOLD_PG_KILL_GRACE=1"}, tc.args...)
			if code != tc.wantCode || !strings.Contains(out, tc.wantText) {
				t.Errorf("exit %d: %s\nwant exit %d containing %q", code, out, tc.wantCode, tc.wantText)
			}
			// 1s limit + 1s grace, and the poll of each; far under what a command
			// that is merely waited for would take (it sleeps a minute).
			if d := time.Since(start); d > 15*time.Second {
				t.Errorf("took %v: the deadline did not hold", d)
			}
			f.expectStopped(t)
			if tc.container && !sawRemove(calls) {
				t.Errorf("the container was not removed by name: %v", calls)
			}
		})
	}
}

// TestPGClientWrapperCleansUpWhenToldToStop: a caller that gives up (a test
// deadline, Ctrl-C, a terminal closing) signals the wrapper, and the dump or
// restore it started must not be left running with its container.
func TestPGClientWrapperCleansUpWhenToldToStop(t *testing.T) {
	for _, tc := range []struct {
		sig  syscall.Signal
		want int
	}{
		{syscall.SIGTERM, 143},
		{syscall.SIGINT, 130},
		{syscall.SIGHUP, 129},
	} {
		t.Run(tc.sig.String(), func(t *testing.T) {
			if signal.Ignored(tc.sig) {
				t.Skipf("%v is ignored in this process, so a shell it starts cannot trap it", tc.sig)
			}
			f := newFakeDocker(t, true)
			cmd := exec.Command(pgWrapperPath(), "16", "pg_dump", "-Fc")
			cmd.Env = []string{"PATH=" + f.path, "TMPDIR=" + f.dir, "FAKE_DOCKER_DIR=" + f.dir,
				"FAKE_STUBBORN=tool", "FOLD_PG_KILL_GRACE=1"}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			t.Cleanup(func() {
				_ = cmd.Process.Kill()
				for _, pid := range f.pids(t) {
					_ = syscall.Kill(-pid, syscall.SIGKILL)
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
			})
			deadline := time.Now().Add(30 * time.Second)
			for {
				if _, err := os.Stat(filepath.Join(f.dir, "started")); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the tool never started")
				}
				time.Sleep(20 * time.Millisecond)
			}
			stopped := time.Now()
			if err := cmd.Process.Signal(tc.sig); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if code := exitCodeOf(err); code != tc.want {
					t.Errorf("exit %d, want %d", code, tc.want)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("the wrapper did not end after the signal")
			}
			if d := time.Since(stopped); d > 15*time.Second {
				t.Errorf("took %v to end", d)
			}
			f.expectStopped(t)
			if !sawRemove(f.calls(t)) {
				t.Errorf("the container was not removed by name: %v", f.calls(t))
			}
			if left, _ := filepath.Glob(filepath.Join(f.dir, "pg-client.*")); len(left) != 0 {
				t.Errorf("the wrapper left its work directory: %v", left)
			}
		})
	}
}

// TestPGClientWrapperRemovesAContainerItsClientLeftBehind: the docker client can
// die after the daemon started the container (a lost connection, an OOM kill),
// and --rm acts only when the container exits, so the tool would keep running
// with nobody to stop it. Whatever the client's status, the wrapper removes the
// container it named, keeps that status, and touches nothing else.
func TestPGClientWrapperRemovesAContainerItsClientLeftBehind(t *testing.T) {
	url := "postgres://u:p@127.0.0.1:5433/db"
	for _, tc := range []struct {
		name     string
		orphan   string
		rc       string
		args     []string
		wantCode int
		wantText string
	}{
		{"a dump whose client fails", "tool", "5", []string{"16", "pg_dump", "-Fc"}, 5, ""},
		{"a dump whose client reports success", "tool", "0", []string{"16", "pg_dump", "-Fc"}, 0, ""},
		{"a discovery run", "tool", "5", []string{"16", "pg_dump", "--version"}, 5, ""},
		{"the probe", "probe", "2", []string{"16", "pg_dump", url}, 73, "does not answer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeDocker(t, true)
			bystanders := f.bystanders(t)
			start := time.Now()
			out, code, calls := f.wrap(t, []string{"FAKE_ORPHAN=" + tc.orphan, "FAKE_ORPHAN_RC=" + tc.rc}, tc.args...)
			if code != tc.wantCode || !strings.Contains(out, tc.wantText) {
				t.Errorf("exit %d: %s\nwant exit %d containing %q", code, out, tc.wantCode, tc.wantText)
			}
			if strings.Contains(out, "could not remove") {
				t.Errorf("the removal worked, yet the wrapper said it did not: %s", out)
			}
			if d := time.Since(start); d > 15*time.Second {
				t.Errorf("took %v", d)
			}
			f.expectStopped(t)
			if !sawRemove(calls) {
				t.Errorf("the container was not removed by name: %v", calls)
			}
			onlyRemovedItsOwn(t, calls)
			bystanders(t)
		})
	}
}

// TestPGClientWrapperNamesAContainerItCannotRemove: when the daemon refuses, the
// wrapper still ends in bounded time, says which container to remove by hand,
// and its exit status is still the tool's.
func TestPGClientWrapperNamesAContainerItCannotRemove(t *testing.T) {
	for _, rc := range []string{"5", "0"} {
		t.Run("client status "+rc, func(t *testing.T) {
			f := newFakeDocker(t, true)
			bystanders := f.bystanders(t)
			start := time.Now()
			out, code, calls := f.wrap(t, []string{"FAKE_ORPHAN=tool", "FAKE_ORPHAN_RC=" + rc, "FAKE_RM=fail", "FOLD_PG_INFO_TIMEOUT=1"}, "16", "pg_dump", "-Fc")
			if want, _ := strconv.Atoi(rc); code != want {
				t.Errorf("exit %d, want the tool's %s: %s", code, rc, out)
			}
			names := runNames(calls)
			if len(names) != 1 {
				t.Fatalf("want one container run, got %v", calls)
			}
			for name := range names {
				if !strings.Contains(out, "could not remove container "+name) || !strings.Contains(out, "docker rm -f "+name) {
					t.Errorf("the output does not name the container %s: %s", name, out)
				}
				if b, err := os.ReadFile(filepath.Join(f.dir, "c."+name)); err == nil {
					if pid, _ := strconv.Atoi(strings.TrimSpace(string(b))); pid > 0 {
						_ = syscall.Kill(-pid, syscall.SIGKILL) // the fake refused to; do not leave it
					}
				}
			}
			if d := time.Since(start); d > 15*time.Second {
				t.Errorf("took %v: the removal was not bounded", d)
			}
			onlyRemovedItsOwn(t, calls)
			bystanders(t)
		})
	}
}

// TestPGClientWrapperCleanupHoldsWhenRemovalStalls: the Go side signals the
// wrapper's group at its deadline and kills it a grace later. The wrapper's own
// cleanup (ending the stubborn tool, then rm and ps on a daemon that answers neither)
// must finish inside that grace, by one absolute deadline for all of it. The docker
// commands it starts have groups of their own, which the Go side's final kill does
// not reach, so any still running when the wrapper is gone were left by it. With
// separate allowances for rm and ps, the two stall in turn and the run takes
// 1s (the tool) + 5s + 5s here, past the 10s grace.
func TestPGClientWrapperCleanupHoldsWhenRemovalStalls(t *testing.T) {
	if signal.Ignored(syscall.SIGTERM) {
		t.Skip("SIGTERM is ignored in this process, so a shell it starts cannot trap it")
	}
	const limit, grace = 3 * time.Second, 10 * time.Second
	for _, tc := range []struct {
		name string
		env  []string
	}{
		{"rm and ps both stall", []string{"FAKE_RM=hang", "FAKE_PS=hang"}},
		{"rm stalls", []string{"FAKE_RM=hang"}},
		{"ps stalls after a refused rm", []string{"FAKE_RM=fail", "FAKE_PS=hang"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeDocker(t, true)
			bystanders := f.bystanders(t)
			t.Cleanup(func() {
				for _, file := range []string{"pids", "hung"} {
					b, _ := os.ReadFile(filepath.Join(f.dir, file))
					for _, s := range strings.Fields(string(b)) {
						if pid, err := strconv.Atoi(s); err == nil {
							_ = syscall.Kill(-pid, syscall.SIGKILL)
							_ = syscall.Kill(pid, syscall.SIGKILL)
						}
					}
				}
			})
			argv := append([]string{"env", "PATH=" + f.path, "TMPDIR=" + f.dir, "FAKE_DOCKER_DIR=" + f.dir,
				"FAKE_DOCKER_PRESENT=1", "FAKE_STUBBORN=tool", "FOLD_PG_INFO_TIMEOUT=4", "FOLD_PG_KILL_GRACE=1"}, tc.env...)
			argv = append(argv, pgWrapperPath(), "16", "pg_dump", "-Fc")
			var out bytes.Buffer
			start := time.Now()
			err := pgRun(limit, grace, &out, &out, argv...)
			took := time.Since(start)
			if code := exitCodeOf(err); code != 143 {
				t.Errorf("exit %d after %v, want 143 (the wrapper ended itself, not killed by the Go side): %v\n%s", code, took, err, out.String())
			}
			if took > limit+grace {
				t.Errorf("took %v: the cleanup outlasted the Go side's grace", took)
			}
			hung, _ := os.ReadFile(filepath.Join(f.dir, "hung"))
			if len(strings.Fields(string(hung))) == 0 {
				t.Fatal("no docker rm or ps stalled: the scenario did not run")
			}
			for _, s := range strings.Fields(string(hung)) {
				pid, _ := strconv.Atoi(s)
				deadline := time.Now().Add(time.Second)
				for alive(pid) && time.Now().Before(deadline) {
					time.Sleep(20 * time.Millisecond)
				}
				if alive(pid) {
					t.Errorf("a stalled docker command (pid %d) outlived the wrapper", pid)
				}
			}
			if !strings.Contains(out.String(), "could not remove container") {
				t.Errorf("the output does not name the container it could not remove: %s", out.String())
			}
			calls := f.calls(t)
			onlyRemovedItsOwn(t, calls)
			bystanders(t)
			if left, _ := filepath.Glob(filepath.Join(f.dir, "pg-client.*")); len(left) != 0 {
				t.Errorf("the wrapper left its work directory: %v", left)
			}
		})
	}
}

// TestPGClientWrapperFinishesRemovalWhenToldToStop: a stop that arrives while the
// container is being removed must not abandon it. The first `docker rm` hangs;
// the signal ends it, and the wrapper removes the container again before it exits.
func TestPGClientWrapperFinishesRemovalWhenToldToStop(t *testing.T) {
	if signal.Ignored(syscall.SIGTERM) {
		t.Skip("SIGTERM is ignored in this process, so a shell it starts cannot trap it")
	}
	f := newFakeDocker(t, true)
	bystanders := f.bystanders(t)
	cmd := exec.Command(pgWrapperPath(), "16", "pg_dump", "-Fc")
	cmd.Env = []string{"PATH=" + f.path, "TMPDIR=" + f.dir, "FAKE_DOCKER_DIR=" + f.dir,
		"FAKE_ORPHAN=tool", "FAKE_RM=slow_first", "FOLD_PG_KILL_GRACE=1"}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		for _, pid := range f.pids(t) {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(f.dir, "rm.began")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the wrapper never began removing the container")
		}
		time.Sleep(20 * time.Millisecond)
	}
	stopped := time.Now()
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if code := exitCodeOf(err); code != 143 {
			t.Errorf("exit %d, want 143", code)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the wrapper did not end after the signal")
	}
	if d := time.Since(stopped); d > 15*time.Second {
		t.Errorf("took %v to end", d)
	}
	f.expectStopped(t) // the hung rm and the container alike
	calls := f.calls(t)
	if n := len(callsFor(calls, "rm")); n < 2 {
		t.Errorf("want the removal retried after the interrupted one, got %d rm calls: %v", n, calls)
	}
	for name := range runNames(calls) {
		if _, err := os.Stat(filepath.Join(f.dir, "c."+name)); err == nil {
			t.Errorf("the container %s was abandoned", name)
		}
	}
	onlyRemovedItsOwn(t, calls)
	bystanders(t)
	if left, _ := filepath.Glob(filepath.Join(f.dir, "pg-client.*")); len(left) != 0 {
		t.Errorf("the wrapper left its work directory: %v", left)
	}
}

// TestPGRunStopsTheWholeGroup: the Go side of the deadline. A wrapper killed by
// exec's own deadline (the pid alone) would leave its docker behind, so pgRun
// signals the process group, and kills what ignored the TERM.
func TestPGRunStopsTheWholeGroup(t *testing.T) {
	dir := t.TempDir()
	pids := filepath.Join(dir, "pids")
	script := filepath.Join(dir, "stubborn")
	body := `#!/usr/bin/env bash
trap '' TERM
bash -c 'trap "" TERM; echo $$ >>"$1"; sleep 60' child "$1" &
echo $$ >>"$1"
wait
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	var out bytes.Buffer
	err := pgRun(700*time.Millisecond, 500*time.Millisecond, &out, &out, script, pids)
	if err == nil {
		t.Fatal("a command that overran its limit reported success")
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("took %v", d)
	}
	b, _ := os.ReadFile(pids)
	got := strings.Fields(string(b))
	if len(got) != 2 {
		t.Fatalf("the script recorded %v, want its pid and its child's", got)
	}
	for _, s := range got {
		pid, _ := strconv.Atoi(s)
		deadline := time.Now().Add(time.Second)
		for alive(pid) && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if alive(pid) {
			t.Errorf("process %d survived the deadline", pid)
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

func TestPGCandidatesOrder(t *testing.T) {
	found := func(string) (string, error) { return "/usr/bin/pg_dump", nil }
	missing := func(string) (string, error) { return "", exec.ErrNotFound }
	only := func(name string) func(string) (string, error) {
		return func(n string) (string, error) {
			if n == name {
				return "/usr/bin/" + n, nil
			}
			return "", exec.ErrNotFound
		}
	}
	const wrapper = "/repo/scripts/pg-client.sh"
	join := func(c [][]string) string {
		var parts []string
		for _, argv := range c {
			parts = append(parts, strings.Join(argv, " "))
		}
		return strings.Join(parts, " | ")
	}
	for _, tc := range []struct {
		name string
		bin  string
		find func(string) (string, error)
		want string
	}{
		{"FOLD_PG_BIN stands alone", "/opt/pg16/bin", found, "/opt/pg16/bin/pg_dump"},
		{"native then the container", "", found, "/usr/bin/pg_dump | /repo/scripts/pg-client.sh 16 pg_dump"},
		{"container only", "", only("docker"), "/repo/scripts/pg-client.sh 16 pg_dump"},
		{"native only", "", only("pg_dump"), "/usr/bin/pg_dump"},
		{"nothing", "", missing, ""},
	} {
		if got := join(pgCandidates("pg_dump", 16, wrapper, tc.bin, tc.find)); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestCheckPGClientKeepsTheMajorStrict(t *testing.T) {
	script := func(t *testing.T, body string) []string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "client")
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return []string{p}
	}
	if note, ok, _ := checkPGClient(script(t, `echo "pg_dump (PostgreSQL) 16.4"`), 16, false); !ok {
		t.Errorf("a matching client was refused: %s", note)
	}
	note, ok, unavailable := checkPGClient(script(t, `echo "pg_dump (PostgreSQL) 18.1"`), 16, false)
	if ok || unavailable || !strings.Contains(note, "not PostgreSQL 16") {
		t.Errorf("a PostgreSQL 18 client passed for a 16 server: ok=%v unavailable=%v note=%q", ok, unavailable, note)
	}
	note, ok, unavailable = checkPGClient(script(t, `echo "boom" >&2; exit 3`), 16, false)
	if ok || unavailable || !strings.Contains(note, "boom") {
		t.Errorf("a failing client passed or counted as unavailable: ok=%v unavailable=%v note=%q", ok, unavailable, note)
	}
	// Only the wrapper's own "no runtime" codes read as unavailable, and a native
	// client that happens to exit 71 does not.
	if _, ok, unavailable = checkPGClient(script(t, `echo "no daemon" >&2; exit 71`), 16, true); ok || !unavailable {
		t.Errorf("the wrapper's exit 71 should read as unavailable: ok=%v unavailable=%v", ok, unavailable)
	}
	if _, ok, unavailable = checkPGClient(script(t, `exit 71`), 16, false); ok || unavailable {
		t.Errorf("a native client's exit 71 must stay a failure: ok=%v unavailable=%v", ok, unavailable)
	}
	if _, ok, unavailable = checkPGClient(script(t, `echo "pulling failed" >&2; exit 72`), 16, true); ok || unavailable {
		t.Errorf("an unusable image must fail, not skip: ok=%v unavailable=%v", ok, unavailable)
	}
}

// The in-memory race tests for the fence's process state. They use no database
// and prove only that the shared memory is touched safely; the barrier's
// PostgreSQL ordering is decided by the lock and schedule tests above, which a
// clean race run does not replace. scripts/reason-review-hardening.sh runs
// them under the race detector.

// Raising the floor from many goroutines keeps the largest value ever offered,
// and no goroutine ever sees it go down.
func TestRaceFenceStateRaiseKeepsTheMaximum(t *testing.T) {
	seed := raceSeed(t)
	var f fenceState
	const workers, raises = 16, 2000
	offered := make([][]int64, workers)
	var stop atomic.Bool
	var observer sync.WaitGroup
	observer.Add(1)
	go func() {
		defer observer.Done()
		var prev int64
		for !stop.Load() {
			cur := f.floor.Load()
			if cur < prev {
				t.Errorf("the floor went from %d down to %d", prev, cur)
				return
			}
			prev = cur
		}
	}()
	raceAll(workers, func(w int) {
		rng := rand.New(rand.NewSource(seed + int64(w)))
		for i := 0; i < raises; i++ {
			v := rng.Int63n(1<<40) - 1<<20
			offered[w] = append(offered[w], v)
			f.raise(v)
		}
	})
	stop.Store(true)
	observer.Wait()
	want := int64(0) // the zero value is the starting floor; a lower offer changes nothing
	for _, vs := range offered {
		for _, v := range vs {
			want = max(want, v)
		}
	}
	if got := f.floor.Load(); got != want {
		t.Fatalf("the floor is %d, want the largest offered value %d", got, want)
	}
}

// Every caller gets the one turn channel, and it is a mutual exclusion: at most
// one goroutine is inside at a time, and what they do inside is ordered by it
// (the race detector would flag the counter otherwise).
func TestRaceFenceStateTurnIsExclusive(t *testing.T) {
	var f fenceState
	const workers, rounds = 16, 300
	chans := make([]chan struct{}, workers)
	raceAll(workers, func(k int) { chans[k] = f.turnChan() })
	for k, c := range chans {
		if c != chans[0] || c == nil {
			t.Fatalf("caller %d got a different turn channel", k)
		}
	}
	var inside atomic.Int32
	counter := 0
	raceAll(workers, func(int) {
		turn := f.turnChan()
		for i := 0; i < rounds; i++ {
			turn <- struct{}{}
			if n := inside.Add(1); n != 1 {
				t.Errorf("%d goroutines hold the turn at once", n)
			}
			counter++
			inside.Add(-1)
			<-turn
		}
	})
	if counter != workers*rounds {
		t.Fatalf("the counter is %d, want %d", counter, workers*rounds)
	}
}

// Many goroutines notice the same restore: the process is asked to restart
// once, and everyone sees it marked afterwards. With nothing to ask, noticing
// still marks it.
func TestRaceNoteRestoreDetectedRestartsOnce(t *testing.T) {
	var restarts atomic.Int32
	a := &App{requestRestart: func() { restarts.Add(1) }}
	raceAll(32, func(int) { a.noteRestoreDetected() })
	if !a.restoreDetected() || restarts.Load() != 1 {
		t.Fatalf("detected %v, restarts %d, want true and 1", a.restoreDetected(), restarts.Load())
	}
	bare := &App{}
	raceAll(32, func(int) { bare.noteRestoreDetected() })
	if !bare.restoreDetected() {
		t.Fatal("a restore noticed with no restart hook was not marked")
	}
}

// --- Real binaries (DEC-158): the current build and the tagged v0.1.0 ---

var (
	foldBuildsMu  sync.Mutex
	foldBuilds    = map[string]string{}
	foldBuildDirs []string
)

// removeFoldBuilds deletes the directories buildFold made; TestMain calls it
// once the tests are done.
func removeFoldBuilds() {
	foldBuildsMu.Lock()
	defer foldBuildsMu.Unlock()
	for _, d := range foldBuildDirs {
		os.RemoveAll(d)
	}
}

// buildFold builds the current tree (ref "") or a tagged release from git
// (git archive, web/dist holding only its README), CGO off, once per process.
// A missing tag or toolchain fails under CI and skips locally. scripts/go-test.sh
// builds them before its shards start and names them in FOLD_TEST_BUILD_CURRENT
// and the versioned FOLD_TEST_BUILD variables, so shards need no compiler.
func buildFold(t *testing.T, ref string) string {
	t.Helper()
	foldBuildsMu.Lock()
	defer foldBuildsMu.Unlock()
	if bin, ok := foldBuilds[ref]; ok {
		return bin
	}
	env := "FOLD_TEST_BUILD_CURRENT"
	if ref != "" {
		env = "FOLD_TEST_BUILD_" + strings.NewReplacer(".", "_").Replace(strings.ToUpper(ref))
	}
	if bin := os.Getenv(env); bin != "" {
		if _, err := os.Stat(bin); err != nil {
			t.Fatalf("%s names %s: %v", env, bin, err)
		}
		foldBuilds[ref] = bin
		return bin
	}
	unavailable := func(format string, args ...any) {
		t.Helper()
		if os.Getenv("CI") != "" {
			t.Fatalf(format, args...)
		}
		t.Skipf(format, args...)
	}
	dir, err := os.MkdirTemp("", "fold-build-")
	if err != nil {
		t.Fatal(err)
	}
	foldBuildDirs = append(foldBuildDirs, dir)
	bin := filepath.Join(dir, "fold")
	src, _ := os.Getwd()
	if ref != "" {
		src = filepath.Join(dir, "src")
		if err := os.MkdirAll(src, 0o755); err != nil {
			t.Fatal(err)
		}
		archive := exec.Command("sh", "-c", "git archive "+ref+" | tar -x -C "+src)
		if out, err := archive.CombinedOutput(); err != nil {
			unavailable("git archive %s (needs the tag and full history): %v %s", ref, err, out)
		}
	}
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = src
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		unavailable("build %q: %v %s", ref, err, out)
	}
	foldBuilds[ref] = bin
	return bin
}

// foldProc is one running fold serve.
type foldProc struct {
	cmd    *exec.Cmd
	port   string
	out    *syncBuffer
	exited chan error
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}

// runFold runs bin with args against database name and returns its output
// and exit error.
func runFold(t *testing.T, bin, name string, args ...string) (string, error) {
	t.Helper()
	dsn, err := testdb.URL(name)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "APP_ENV=dev", "DATABASE_URL="+dsn, "SESSION_SECRET=real-binary-secret-real-binary-secret", "UPLOAD_DIR="+t.TempDir(), "PORT="+freePort(t))
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// startFold starts bin serve on database name. It returns once /healthz
// answers, or once the process exits (exited then holds its error).
func startFold(t *testing.T, bin, name string) *foldProc {
	t.Helper()
	dsn, err := testdb.URL(name)
	if err != nil {
		t.Fatal(err)
	}
	p := &foldProc{port: freePort(t), out: &syncBuffer{}, exited: make(chan error, 1)}
	p.cmd = exec.Command(bin, "serve")
	p.cmd.Env = append(os.Environ(), "APP_ENV=dev", "DATABASE_URL="+dsn, "SESSION_SECRET=real-binary-secret-real-binary-secret", "UPLOAD_DIR="+t.TempDir(), "PORT="+p.port)
	p.cmd.Stdout, p.cmd.Stderr = p.out, p.out
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { p.exited <- p.cmd.Wait() }()
	t.Cleanup(p.stop)
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-p.exited:
			p.exited <- err
			return p
		default:
		}
		if p.healthy() {
			return p
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("fold serve neither answered nor exited in 60s:\n%s", p.out.String())
	return p
}

func (p *foldProc) healthy() bool {
	c := http.Client{Timeout: time.Second}
	res, err := c.Get("http://127.0.0.1:" + p.port + "/healthz")
	if err != nil {
		return false
	}
	res.Body.Close()
	return res.StatusCode == http.StatusOK
}

// exitedWith is the process's exit error once it has ended, or nil, false.
func (p *foldProc) exitedWith() (bool, error) {
	select {
	case err := <-p.exited:
		p.exited <- err
		return true, err
	default:
		return false, nil
	}
}

func (p *foldProc) stop() {
	if done, _ := p.exitedWith(); done {
		return
	}
	_ = p.cmd.Process.Signal(os.Interrupt)
	select {
	case <-p.exited:
	case <-time.After(10 * time.Second):
		_ = p.cmd.Process.Kill()
	}
}

// foldSession is a signed-in browser of one running process.
type foldSession struct {
	t      *testing.T
	base   string
	client *http.Client
	csrf   string
}

func (p *foldProc) session(t *testing.T, email string) *foldSession {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	s := &foldSession{t: t, base: "http://127.0.0.1:" + p.port, client: &http.Client{Jar: jar, Timeout: 30 * time.Second}}
	var tok struct {
		CSRFToken string `json:"csrfToken"`
	}
	if code, body := s.call("GET", "/api/v1/auth/csrf", nil, &tok); code != http.StatusOK {
		t.Fatalf("csrf: %d %s", code, body)
	}
	s.csrf = tok.CSRFToken
	if code, body := s.call("POST", "/api/v1/auth/login", map[string]any{"email": email, "password": "password1234"}, nil); code != http.StatusOK {
		t.Fatalf("login %s: %d %s", email, code, body)
	}
	s.call("GET", "/api/v1/auth/csrf", nil, &tok)
	s.csrf = tok.CSRFToken
	return s
}

func (s *foldSession) call(method, path string, body any, into any) (int, string) {
	s.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, s.base+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if s.csrf != "" {
		req.Header.Set("X-CSRF-Token", s.csrf)
	}
	res, err := s.client.Do(req)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if into != nil && res.StatusCode < 300 {
		if err := json.Unmarshal(b, into); err != nil {
			s.t.Fatalf("%s %s: %v %s", method, path, err, b)
		}
	}
	return res.StatusCode, string(b)
}

// visibilityBody is visibilityBody (testhelpers_test.go) for a real binary's
// session: body with the revision this binary's event page gives s, read just
// before.
func (s *foldSession) visibilityBody(path string, body map[string]any) map[string]any {
	s.t.Helper()
	m := eventVisibilityPath.FindStringSubmatch(path)
	if m == nil {
		s.t.Fatalf("%s is not an event's visibility", path)
	}
	var page struct {
		Event eventJSON `json:"event"`
	}
	s.want("GET", "/api/v1/events/"+m[1], nil, http.StatusOK, &page)
	out := maps.Clone(body)
	out["revision"] = page.Event.Revision
	return out
}

func (s *foldSession) want(method, path string, body any, want int, into any) string {
	s.t.Helper()
	code, out := s.call(method, path, body, into)
	if code != want {
		s.t.Fatalf("%s %s: %d %s, want %d", method, path, code, out, want)
	}
	return out
}

// seedUsers inserts an admin and a member with SQL (the release has no
// admin user-create route, and its setup counts users only).
func seedUsers(t *testing.T, db *gorm.DB) {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte("password1234"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	gormMust(t, db.Exec(`INSERT INTO users (name, email, password_hash, role, status) VALUES ('Admin', 'admin@example.com', ?, 'admin', 'active'), ('Member', 'member@example.com', ?, 'member', 'active')`, string(hash), string(hash)))
}

const startRefusal = "this database needs Fold at recovery level 3 or later (exact visibility consent, DEC-158)"

func wantRefusedStart(t *testing.T, p *foldProc, label string) {
	t.Helper()
	done, err := p.exitedWith()
	if !done || err == nil || !strings.Contains(p.out.String(), startRefusal) {
		t.Fatalf("%s: exited %v (%v), want a non-zero exit naming recovery level 3:\n%s", label, done, err, p.out.String())
	}
	if p.healthy() {
		t.Fatalf("%s: the refused process answered", label)
	}
}

// headVersion is the version the current binary migrates to: one per file,
// since TestBaselineFileShape keeps the files numbered without gaps.
func headVersion(t *testing.T) int64 {
	t.Helper()
	files, err := fs.Glob(migrations.Files, "*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("migration files = %v (%v)", files, err)
	}
	return int64(len(files))
}

func goose(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var v int64
	gormMust(t, db.Raw("SELECT coalesce(max(version_id), 0) FROM goose_db_version WHERE is_applied").Scan(&v))
	return v
}

// priorStartRefused starts the earlier release on database name and requires
// the start fence's refusal: a non-zero exit naming recovery level 3, nothing
// served, the restore_fence counter and the users unchanged, and the goose
// version wantVersion afterwards.
func priorStartRefused(t *testing.T, prior, name string, db *gorm.DB, label string, wantVersion int64) {
	t.Helper()
	counter := func() string {
		var v string
		gormMust(t, db.Raw("SELECT coalesce((SELECT value FROM settings WHERE key = 'restore_fence'), '')").Scan(&v))
		return v
	}
	users := func() int64 {
		var n int64
		gormMust(t, db.Raw("SELECT count(*) FROM users").Scan(&n))
		return n
	}
	before, people := counter(), users()
	wantRefusedStart(t, startFold(t, prior, name), label)
	if counter() != before || users() != people {
		t.Fatalf("%s: the refused start wrote (counter %s -> %s, users %d -> %d)", label, before, counter(), people, users())
	}
	if v := goose(t, db); v != wantVersion {
		t.Fatalf("%s: version %d, want %d", label, v, wantVersion)
	}
}

// restoreInto restores dump into a fresh database with restore.sh's flags
// and returns its name and a handle.
func restoreInto(t *testing.T, dump string) (string, *gorm.DB) {
	t.Helper()
	name := testdb.Fresh(t, false)
	db, err := testdb.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	fenceRun(t, "pg_restore", "--no-owner", "--no-acl", "--single-transaction", "--exit-on-error", "-d", fenceDatabaseURL(t, db), dump)
	return name, db
}

// TestPriorReleaseRefusesToStart (C17): v0.1.0 started on a database this
// release ran exits non-zero with the level-3 message and serves nothing:
// after setup, after the current binary's live Down, after a restore of that
// database, and on a database before setup walked down below 7. Its own
// down-one above its files fails. A dump taken before the upgrade still
// starts it (L1, pinned so the docs stay honest). The current binary then
// starts each time.
func TestPriorReleaseRefusesToStart(t *testing.T) {
	cur, prior := buildFold(t, ""), buildFold(t, "v0.1.0")
	migrated := func(t *testing.T, bin string) (string, *gorm.DB) {
		t.Helper()
		name := testdb.Fresh(t, false)
		db, err := testdb.Open(name)
		if err != nil {
			t.Fatal(err)
		}
		if out, err := runFold(t, bin, name, "migrate", "up"); err != nil {
			t.Fatalf("migrate up: %v %s", err, out)
		}
		return name, db
	}
	downOne := func(t *testing.T, bin, name string) {
		t.Helper()
		if out, err := runFold(t, bin, name, "migrate", "down-one"); err != nil {
			t.Fatalf("down-one: %v %s", err, out)
		}
	}
	t.Run("after setup, the live Down and a restore", func(t *testing.T) {
		fenceTools(t)
		name, db := migrated(t, cur)
		if p := startFold(t, cur, name); !p.healthy() {
			t.Fatalf("the current binary did not start:\n%s", p.out.String())
		} else {
			p.stop()
		}
		seedUsers(t, db)
		priorStartRefused(t, prior, name, db, "v0.1.0 at the head after setup", headVersion(t))
		dump := fenceDump(t, db)
		for goose(t, db) > 14 {
			downOne(t, cur, name)
		}
		priorStartRefused(t, prior, name, db, "v0.1.0 after the live Down", 14)
		if out, err := runFold(t, prior, name, "migrate", "down-one"); err == nil || goose(t, db) != 14 {
			t.Fatalf("v0.1.0's own down-one above its files: %v, version %d: %s", err, goose(t, db), out)
		}
		restored, rdb := restoreInto(t, dump)
		// C14's restore case: both fences come back with the dump.
		var fences int64
		gormMust(t, rdb.Raw("SELECT count(*) FROM pg_trigger WHERE NOT tgisinternal AND tgname IN ('settings_recovery_floor', 'audit_logs_visibility_consent')").Scan(&fences))
		if fences != 2 {
			t.Fatalf("the restored copy has %d of the two fences", fences)
		}
		priorStartRefused(t, prior, restored, rdb, "v0.1.0 after a restore", headVersion(t))
		if p := startFold(t, cur, restored); !p.healthy() {
			t.Fatalf("the current binary on the restored copy:\n%s", p.out.String())
		}
	})
	t.Run("L1: a dump taken before the upgrade", func(t *testing.T) {
		fenceTools(t)
		_, db := migrated(t, prior)
		seedUsers(t, db)
		dump := fenceDump(t, db)
		restored, rdb := restoreInto(t, dump)
		old := startFold(t, prior, restored)
		if !old.healthy() {
			t.Fatalf("v0.1.0 on its own pre-upgrade dump did not start (L1 says it does):\n%s", old.out.String())
		}
		old.stop()
		if p := startFold(t, cur, restored); !p.healthy() {
			t.Fatalf("the current binary on the pre-upgrade dump:\n%s", p.out.String())
		} else {
			p.stop()
		}
		priorStartRefused(t, prior, restored, rdb, "v0.1.0 once the current binary ran", headVersion(t))
	})
	t.Run("before first-run setup, walked down", func(t *testing.T) {
		name, db := migrated(t, cur)
		first := startFold(t, cur, name)
		if !first.healthy() {
			t.Fatalf("the current binary did not start:\n%s", first.out.String())
		}
		first.stop()
		check := func(label string, want int64) {
			t.Helper()
			priorStartRefused(t, prior, name, db, label, want)
			var n int64
			gormMust(t, db.Raw("SELECT count(*) FROM users").Scan(&n))
			if n != 0 {
				t.Fatalf("%s: %d users before setup", label, n)
			}
		}
		check("v0.1.0 at the head, before setup", headVersion(t))
		for goose(t, db) > 14 {
			downOne(t, cur, name)
		}
		check("v0.1.0 at 14", 14)
		if out, err := runFold(t, prior, name, "migrate", "down-one"); err == nil || goose(t, db) != 14 {
			t.Fatalf("v0.1.0's own down-one above its files: %v, version %d: %s", err, goose(t, db), out)
		}
		for goose(t, db) > 7 {
			downOne(t, cur, name)
		}
		check("v0.1.0 at 7", 7)
		for goose(t, db) > 1 {
			downOne(t, cur, name)
		}
		for _, bin := range []string{cur, prior} {
			out, err := runFold(t, bin, name, "migrate", "down-one")
			if err == nil || !strings.Contains(out, "audit_logs") || goose(t, db) != 1 {
				t.Fatalf("the baseline down-one: %v, version %d: %s, want a refusal naming audit_logs", err, goose(t, db), out)
			}
		}
		// v0.1.0's own Up returns the database to 7 before its boot is refused.
		check("v0.1.0 at 1", 7)
		if p := startFold(t, cur, name); !p.healthy() {
			t.Fatalf("the current binary after the walk:\n%s", p.out.String())
		}
	})
}

// TestRealServeRestartRotatesTheEpoch: two real starts rotate the epoch; a
// token from before the second start gets one 409, and its fresh token saves.
func TestRealServeRestartRotatesTheEpoch(t *testing.T) {
	cur := buildFold(t, "")
	name := testdb.Fresh(t, false)
	db, err := testdb.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := runFold(t, cur, name, "migrate", "up"); err != nil {
		t.Fatalf("migrate up: %v %s", err, out)
	}
	seedUsers(t, db)
	p := startFold(t, cur, name)
	admin := p.session(t, "admin@example.com")
	var ev struct {
		Event struct {
			ID uint `json:"id"`
		} `json:"event"`
	}
	admin.want("POST", "/api/v1/events", map[string]any{"title": "Restart supper", "startsLocal": time.Now().UTC().AddDate(0, 0, 7).Format("2006-01-02") + "T19:00", "durationMinutes": 60, "tzid": "UTC"}, http.StatusCreated, &ev)
	path := fmt.Sprintf("/api/v1/events/%d/visibility", ev.Event.ID)
	var impact visibilityImpactJSON
	admin.want("GET", path+"-impact?visibility=private&resetOverrides=1", nil, http.StatusOK, &impact)
	e1 := ""
	gormMust(t, db.Raw("SELECT value FROM settings WHERE key = 'visibility_consent_epoch'").Scan(&e1))
	p.stop()
	q := startFold(t, cur, name)
	e2 := ""
	gormMust(t, db.Raw("SELECT value FROM settings WHERE key = 'visibility_consent_epoch'").Scan(&e2))
	if e1 == "" || e1 == e2 {
		t.Fatalf("a real restart did not rotate the epoch: %q -> %q", e1, e2)
	}
	admin2 := q.session(t, "admin@example.com")
	code, body := admin2.call("PUT", path, admin2.visibilityBody(path, map[string]any{"visibility": "private", "resetOverrides": true, "consent": impact.Consent}), nil)
	if code != http.StatusConflict || !strings.Contains(body, `"needsDecision":true`) {
		t.Fatalf("a token from before the restart: %d %s", code, body)
	}
	var answer struct {
		Impact visibilityImpactJSON `json:"impact"`
	}
	_ = json.Unmarshal([]byte(body), &answer)
	admin2.want("PUT", path, admin2.visibilityBody(path, map[string]any{"visibility": "private", "resetOverrides": true, "consent": answer.Impact.Consent}), http.StatusOK, nil)
}

// TestPriorProcessRunningAcrossTheUpgrade (C18): v0.1.0, already serving when
// the current binary applies 00015, changes no event's or date's visibility
// and cancels nobody. Each protected write reaches the audit fence and answers
// 500. The old binary still reads and edits an event, then refuses to restart.
func TestPriorProcessRunningAcrossTheUpgrade(t *testing.T) {
	cur, prior := buildFold(t, ""), buildFold(t, "v0.1.0")
	name := testdb.Fresh(t, false)
	db, err := testdb.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := runFold(t, prior, name, "migrate", "up"); err != nil {
		t.Fatalf("v0.1.0 migrate up: %v %s", err, out)
	}
	seedUsers(t, db)
	// Apply 00008-00014 before v0.1.0 can cache queries over the schema at 00007.
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	provider, err := migrations.NewProvider(sqlDB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(context.Background(), 14); err != nil {
		t.Fatal(err)
	}
	old := startFold(t, prior, name)
	if !old.healthy() {
		t.Fatalf("v0.1.0 did not start:\n%s", old.out.String())
	}
	admin := old.session(t, "admin@example.com")
	member := old.session(t, "member@example.com")
	day := func(n int) string { return time.Now().UTC().AddDate(0, 0, n).Format("2006-01-02") }
	type created struct {
		Event struct {
			ID uint `json:"id"`
		} `json:"event"`
		Group struct {
			ID uint `json:"id"`
		} `json:"group"`
	}
	var e1, e2, e3, e5, e6 created
	// These requests go to v0.1.0, so its date writer is its own PUT
	// /events/:id/occurrences/:date, which this binary retired.
	admin.want("POST", "/api/v1/events", map[string]any{"title": "E1", "startsLocal": day(3) + "T19:00", "durationMinutes": 60, "tzid": "UTC", "recurrenceRule": "FREQ=WEEKLY;COUNT=3"}, http.StatusCreated, &e1)
	admin.want("PUT", fmt.Sprintf("/api/v1/events/%d/occurrences/%s", e1.Event.ID, day(3)), map[string]any{"visibility": "private"}, http.StatusOK, nil)
	admin.want("POST", "/api/v1/events", map[string]any{"title": "E2", "startsLocal": day(4) + "T19:00", "durationMinutes": 60, "tzid": "UTC"}, http.StatusCreated, &e2)
	admin.want("PUT", fmt.Sprintf("/api/v1/events/%d/visibility", e2.Event.ID), map[string]any{"visibility": "private"}, http.StatusOK, nil)
	var g, g2, g3 created
	for _, x := range []*created{&g, &g2, &g3} {
		admin.want("POST", "/api/v1/groups", map[string]any{"name": fmt.Sprintf("Group %p", x), "visibility": "public", "location": "Hall"}, http.StatusCreated, x)
	}
	admin.want("POST", fmt.Sprintf("/api/v1/groups/%d/events", g.Group.ID), map[string]any{"title": "E3", "startsLocal": day(5) + "T19:00", "durationMinutes": 60, "tzid": "UTC"}, http.StatusCreated, &e3)
	admin.want("POST", fmt.Sprintf("/api/v1/groups/%d/events", g2.Group.ID), map[string]any{"title": "E5", "startsLocal": day(5) + "T19:00", "durationMinutes": 60, "tzid": "UTC"}, http.StatusCreated, &e5)
	admin.want("POST", fmt.Sprintf("/api/v1/groups/%d/events", g3.Group.ID), map[string]any{"title": "E6", "startsLocal": day(6) + "T19:00", "durationMinutes": 60, "tzid": "UTC"}, http.StatusCreated, &e6)
	member.want("PUT", fmt.Sprintf("/api/v1/events/%d/registrations", e5.Event.ID), map[string]any{"status": "going", "date": day(5)}, http.StatusOK, nil)

	// The current binary starts beside it and applies 00015 and later.
	next := startFold(t, cur, name)
	if !next.healthy() || goose(t, db) != headVersion(t) {
		t.Fatalf("the current binary beside v0.1.0: %d\n%s", goose(t, db), next.out.String())
	}
	curAdmin := next.session(t, "admin@example.com")
	var impact visibilityImpactJSON
	p1 := fmt.Sprintf("/api/v1/events/%d/visibility", e1.Event.ID)
	curAdmin.want("GET", p1+"-impact?visibility=private&resetOverrides=1", nil, http.StatusOK, &impact)
	curAdmin.want("PUT", p1, curAdmin.visibilityBody(p1, map[string]any{"visibility": "private", "resetOverrides": true, "consent": impact.Consent}), http.StatusOK, nil)

	snapshot := func() string {
		var rows string
		gormMust(t, db.Raw(`SELECT jsonb_build_object(
			'events', (SELECT jsonb_agg(e ORDER BY id) FROM events e),
			'dates', (SELECT jsonb_agg(d ORDER BY id) FROM event_occurrences d),
			'registrations', (SELECT jsonb_agg(r ORDER BY id) FROM registrations r),
			'groups', (SELECT jsonb_agg(g ORDER BY id) FROM groups g),
			'audit', (SELECT jsonb_agg(a ORDER BY id) FROM audit_logs a),
			'notices', (SELECT jsonb_agg(n ORDER BY id) FROM notifications n)
		)::text`).Scan(&rows))
		return rows
	}
	before := snapshot()
	dateEditPath := fmt.Sprintf("/api/v1/events/%d/occurrences/%s", e1.Event.ID, day(10))
	for _, r := range []struct {
		label        string
		method, path string
		body         map[string]any
	}{
		{"the stale empty reset", "PUT", p1, map[string]any{"visibility": "public", "resetOverrides": true, "confirm": false}},
		{"a series opening", "PUT", fmt.Sprintf("/api/v1/events/%d/visibility", e2.Event.ID), map[string]any{"visibility": "public"}},
		{"a date's own value", "PUT", dateEditPath, map[string]any{"visibility": "public"}},
		{"a group event with its own visibility", "POST", fmt.Sprintf("/api/v1/groups/%d/events", g.Group.ID), map[string]any{"title": "E4", "startsLocal": day(8) + "T19:00", "durationMinutes": 60, "tzid": "UTC", "visibility": "private"}},
		{"a group tightening with nobody to cancel", "PUT", fmt.Sprintf("/api/v1/groups/%d", g3.Group.ID), map[string]any{"visibility": "private"}},
		{"a tightening that cancels", "PUT", fmt.Sprintf("/api/v1/events/%d/visibility", e5.Event.ID), map[string]any{"visibility": "invite_only", "confirm": true}},
	} {
		const maxAttempts = 5
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			mark := len(old.out.String())
			code, body := admin.call(r.method, r.path, r.body, nil)
			if after := snapshot(); after != before {
				t.Fatalf("v0.1.0 %s attempt %d changed rows:\n%s\nwant\n%s", r.label, attempt, after, before)
			}
			// v0.1.0 maps any failed date lookup to this 404, including a stale plan.
			dateLookupFailed := r.path == dateEditPath && code == http.StatusNotFound &&
				strings.Contains(body, `"error":"that date is not on this event"`)
			if code != http.StatusInternalServerError && !dateLookupFailed {
				t.Fatalf("v0.1.0 %s attempt %d: %d %s, want 500", r.label, attempt, code, body)
			}
			var logged string
			var fenced, stale bool
			testdb.WaitUntil(t, fmt.Sprintf("v0.1.0 to log the database error for %s attempt %d", r.label, attempt), 5*time.Second, func() bool {
				logged = old.out.String()[mark:]
				for line := range strings.SplitSeq(logged, "\n") {
					fenced = fenced || (strings.Contains(line, "checks visibility consent (DEC-158)") && strings.Contains(line, "SQLSTATE 23514"))
					stale = stale || strings.Contains(line, "cached plan must not change result type (SQLSTATE 0A000)")
				}
				return fenced || stale
			})
			if fenced && code == http.StatusInternalServerError {
				break
			}
			// 00018 changes SELECT * on dates. pgx discards each stale cached plan after 0A000.
			if fenced || !stale || attempt == maxAttempts {
				t.Fatalf("v0.1.0 %s attempt %d: %d %s, want the audit fence's 500:\n%s", r.label, attempt, code, body, logged)
			}
			t.Logf("v0.1.0 %s attempt %d returned %d with a stale plan, retrying", r.label, attempt, code)
		}
	}
	if !old.healthy() {
		t.Fatal("v0.1.0 stopped answering /healthz")
	}
	path := fmt.Sprintf("/api/v1/events/%d", e6.Event.ID)
	var page struct {
		Event eventJSON `json:"event"`
	}
	admin.want("GET", path, nil, http.StatusOK, &page)
	if page.Event.ID != e6.Event.ID {
		t.Fatalf("v0.1.0 read event %d, want %d", page.Event.ID, e6.Event.ID)
	}
	admin.want("PUT", path, map[string]any{"title": "E6 renamed", "startsLocal": day(6) + "T19:00", "durationMinutes": 60, "tzid": "UTC", "revision": page.Event.Revision}, http.StatusOK, nil)
	var title string
	gormMust(t, db.Raw("SELECT title FROM events WHERE id = ?", e6.Event.ID).Scan(&title))
	if title != "E6 renamed" {
		t.Fatalf("title after v0.1.0 edit: %q", title)
	}
	old.stop()
	wantRefusedStart(t, startFold(t, prior, name), "v0.1.0's restart")
}

// TestRealServeBootWaitsForAnOpenConfirmation (C26): the real binary's boot
// rotation waits for an in-test App's checked confirmation that holds the
// epoch FOR SHARE, and serves only after it commits. A token minted before
// that start then gets a 409 decision, and its fresh token saves through the
// binary's own API.
func TestRealServeBootWaitsForAnOpenConfirmation(t *testing.T) {
	cur := buildFold(t, "")
	name := testdb.Fresh(t, false)
	if out, err := runFold(t, cur, name, "migrate", "up"); err != nil {
		t.Fatalf("migrate up: %v %s", err, out)
	}
	db, err := testdb.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	seedUsers(t, db)
	app := appOn(t, db, []byte("real-binary-secret-real-binary-secret"))
	admin := loginAPI(t, app, "admin@example.com", "password1234")
	member := loginAPI(t, app, "member@example.com", "password1234")
	body := map[string]any{"visibility": models.VisibilityInviteOnly}
	var paths [2]string
	var consents [2]string
	gid := createGroupAs(t, admin, "Boot supper", models.VisibilityPublic)
	for i := range paths {
		ev := requireCreatedEvent(t, app, admin.do("POST", "/api/v1/groups/"+itoa(gid)+"/events", createEventBodyForTest(), true))
		var occ models.EventOccurrence
		gormMust(t, db.Where("event_id = ?", ev.ID).First(&occ))
		apiDo(t, member, "PUT", fmt.Sprintf("/api/v1/events/%d/registrations", ev.ID), map[string]any{"status": models.RegGoing, "date": models.FormatCalendarDate(occ.OriginalDate)}, http.StatusOK)
		paths[i] = fmt.Sprintf("/api/v1/events/%d/visibility", ev.ID)
		consents[i] = confirmVisibility(t, admin, paths[i], body)["consent"].(string)
	}
	e1 := ""
	gormMust(t, db.Raw("SELECT value FROM settings WHERE key = 'visibility_consent_epoch'").Scan(&e1))
	parked := make(chan int, 1)
	release := make(chan struct{})
	t.Cleanup(func() { closeOnce(release) })
	var once sync.Once
	app.hooks.consentEpochLocked = func(tx *gorm.DB, epoch string) {
		once.Do(func() {
			var pid int
			if err := tx.Raw("SELECT pg_backend_pid()").Scan(&pid).Error; err != nil {
				t.Error(err)
			}
			if epoch != e1 {
				t.Errorf("the confirmation read %q, want %q", epoch, e1)
			}
			parked <- pid
			<-release
		})
	}
	answer := make(chan *httptest.ResponseRecorder, 1)
	first := visibilityBody(t, admin, paths[0], with(body, "consent", consents[0]))
	go func() { answer <- admin.do("PUT", paths[0], first, true) }()
	var holder int
	select {
	case holder = <-parked:
	case w := <-answer:
		t.Fatalf("the confirmation answered before it held the epoch: %d %s", w.Code, w.Body.String())
	}
	started := make(chan *foldProc, 1)
	go func() { started <- startFold(t, cur, name) }()
	waitingOn(t, db, "%ON CONFLICT (key) DO UPDATE%", holder)
	time.Sleep(300 * time.Millisecond)
	select {
	case <-started:
		t.Fatal("the boot finished while a confirmation held the epoch")
	default:
	}
	if now := ""; db.Raw("SELECT value FROM settings WHERE key = 'visibility_consent_epoch'").Scan(&now).Error != nil || now != e1 {
		t.Fatalf("the epoch moved under the open confirmation: %q", now)
	}
	closeOnce(release)
	if w := <-answer; w.Code != http.StatusOK {
		t.Fatalf("the open confirmation: %d %s", w.Code, w.Body.String())
	}
	var p *foldProc
	select {
	case p = <-started:
	case <-time.After(70 * time.Second):
		t.Fatal("the boot never finished after the confirmation ended")
	}
	if !p.healthy() {
		t.Fatalf("the boot after the confirmation ended:\n%s", p.out.String())
	}
	e2 := ""
	gormMust(t, db.Raw("SELECT value FROM settings WHERE key = 'visibility_consent_epoch'").Scan(&e2))
	if e2 == e1 {
		t.Fatal("the boot did not rotate the epoch")
	}
	stale := admin.do("PUT", paths[1], visibilityBody(t, admin, paths[1], with(body, "consent", consents[1])), true)
	fresh := wantDecision(t, stale)
	served := p.session(t, "admin@example.com")
	served.want("PUT", paths[1], served.visibilityBody(paths[1], with(body, "consent", fresh.Consent)), http.StatusOK, nil)
}

// appOn is testApp's App on another database, with the given secret: an
// in-test instance beside a real binary.
func appOn(t *testing.T, db *gorm.DB, secret []byte) *App {
	t.Helper()
	gin.SetMode(gin.TestMode)
	uploads, err := storage.NewLocalStore(t.TempDir(), uploadsURLPrefix)
	if err != nil {
		t.Fatal(err)
	}
	app := &App{
		DB: db, Store: newSessionStore(secret, false), Uploads: uploads,
		Config: Config{Env: "dev", Port: "8080", SessionSecret: secret, SetupToken: strings.Repeat("a", 64)},
		Mailer: mail.WithSuppressions(db, mail.NewDevOutbox(db, "Fold <noreply@example.com>")),
	}
	bootFence(t, app)
	return app
}
