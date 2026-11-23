package main

import (
	"fmt"
	"go/ast"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"fold/internal/guardkit"
	"fold/internal/testdb"
)

// docs/agents/lifecycles.md is the one map of the rows that move through
// states. It is only worth reading while it is true, so this guard ties it to
// the schema and the source:
//
//   - every column whose CHECK lists its values has a row in the map with
//     exactly those values, or a line under "Enumerated columns that are not
//     lifecycles";
//   - every `file.go:Name` the map names is declared in that file, every
//     `TestName` is a Go test, and every `web/...` path is a file.
//
// Known limits: it reads the states of a lifecycle only when one column holds
// them under such a CHECK. A lifecycle without one (groups.status, users.role,
// nullable timestamps, counters, a state derived from other rows or the clock)
// is held to the names check alone. It does not read transitions, and it
// cannot tell whether a row's writer list is complete.
const lifecycleMapPath = "docs/agents/lifecycles.md"

const lifecycleExemptHeading = "## Enumerated columns that are not lifecycles"

var (
	lifecycleSpanRe   = regexp.MustCompile("`([^`]+)`")
	lifecycleColumnRe = regexp.MustCompile(`^[a-z_]+\.[a-z_]+$`)
	lifecycleGoNameRe = regexp.MustCompile(`^([A-Za-z0-9_/.-]+\.go):([A-Za-z_][A-Za-z0-9_]*)$`)
	lifecycleTestRe   = regexp.MustCompile(`^Test[A-Z][A-Za-z0-9_]*$`)
	lifecycleWebRe    = regexp.MustCompile(`^web/[A-Za-z0-9_/.-]+$`)
	lifecycleValueRe  = regexp.MustCompile(`'((?:[^']|'')*)'::text`)
)

// lifecycleFacts is what the map is checked against. checks maps
// "table.column" to the values its CHECK allows.
type lifecycleFacts struct {
	checks   map[string][]string
	declared func(file, name string) bool
	isTest   func(name string) bool
	exists   func(path string) bool
}

func lifecycleSpans(s string) []string {
	var out []string
	for _, m := range lifecycleSpanRe.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	return out
}

func lifecycleMapProblems(doc string, facts lifecycleFacts) []string {
	var bad []string
	mapped := map[string]bool{}
	exempt := map[string]bool{}
	inExempt := false
	for i, line := range strings.Split(doc, "\n") {
		at := fmt.Sprintf("%s:%d", lifecycleMapPath, i+1)
		if strings.HasPrefix(line, "## ") {
			inExempt = strings.TrimSpace(line) == lifecycleExemptHeading
		}
		for _, span := range lifecycleSpans(line) {
			if m := lifecycleGoNameRe.FindStringSubmatch(span); m != nil {
				if !facts.declared(m[1], m[2]) {
					bad = append(bad, fmt.Sprintf("%s: %s is not declared in %s; name the function that holds the rule or does the write now", at, m[2], m[1]))
				}
			} else if lifecycleTestRe.MatchString(span) {
				if !facts.isTest(span) {
					bad = append(bad, fmt.Sprintf("%s: %s is not a Go test", at, span))
				}
			} else if lifecycleWebRe.MatchString(span) && !facts.exists(span) {
				bad = append(bad, fmt.Sprintf("%s: %s does not exist", at, span))
			}
		}
		if inExempt {
			for _, span := range lifecycleSpans(line) {
				if !lifecycleColumnRe.MatchString(span) {
					continue
				}
				if _, ok := facts.checks[span]; !ok {
					bad = append(bad, fmt.Sprintf("%s: %s has no CHECK that lists its values; drop the line", at, span))
				}
				exempt[span] = true
			}
			continue
		}
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(line, "|")
		if len(cells) < 4 {
			continue
		}
		var states []string
		for _, s := range lifecycleSpans(cells[2]) {
			if s == "''" {
				s = ""
			}
			states = append(states, s)
		}
		sort.Strings(states)
		for _, column := range lifecycleSpans(cells[1]) {
			want, ok := facts.checks[column]
			if !ok {
				continue
			}
			if mapped[column] {
				bad = append(bad, fmt.Sprintf("%s: %s has a second row; keep one", at, column))
			}
			mapped[column] = true
			if strings.Join(states, ",") != strings.Join(want, ",") {
				bad = append(bad, fmt.Sprintf("%s: %s lists states %q but its CHECK allows %q", at, column, states, want))
			}
		}
	}
	for column := range facts.checks {
		switch {
		case mapped[column] && exempt[column]:
			bad = append(bad, fmt.Sprintf("%s: %s is both a lifecycle row and listed as not a lifecycle", lifecycleMapPath, column))
		case !mapped[column] && !exempt[column]:
			bad = append(bad, fmt.Sprintf("%s: %s has a CHECK that lists its values and no row. Add its lifecycle (states, where a move is decided, writers), or list it under %q when its value never depends on the one before", lifecycleMapPath, column, lifecycleExemptHeading))
		}
	}
	sort.Strings(bad)
	return bad
}

// enumeratedChecks reads every single-column CHECK that lists text values
// from the migrated schema.
func enumeratedChecks(t *testing.T) map[string][]string {
	t.Helper()
	var rows []struct {
		Col string
		Def string
	}
	err := testdb.Shared(t).Raw(`
		SELECT c.conrelid::regclass::text || '.' || a.attname AS col, pg_get_constraintdef(c.oid) AS def
		FROM pg_constraint c
		JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = c.conkey[1]
		WHERE c.contype = 'c' AND cardinality(c.conkey) = 1 AND c.connamespace = 'public'::regnamespace`).Scan(&rows).Error
	if err != nil {
		t.Fatal(err)
	}
	checks := map[string][]string{}
	for _, r := range rows {
		if !strings.Contains(r.Def, "= ANY (ARRAY[") {
			continue
		}
		for _, m := range lifecycleValueRe.FindAllStringSubmatch(r.Def, -1) {
			checks[r.Col] = append(checks[r.Col], strings.ReplaceAll(m[1], "''", "'"))
		}
		sort.Strings(checks[r.Col])
	}
	return checks
}

// declaredNames maps each production Go file to the names it declares at the
// top level: functions, methods (by method name), types, constants, variables.
func declaredNames(sources guardkit.Sources) (map[string]map[string]bool, []string) {
	parsed := sources.Parse(0)
	out := map[string]map[string]bool{}
	for _, f := range parsed.Files {
		names := map[string]bool{}
		for _, decl := range f.AST.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				names[d.Name.Name] = true
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						names[s.Name.Name] = true
					case *ast.ValueSpec:
						for _, n := range s.Names {
							names[n.Name] = true
						}
					}
				}
			}
		}
		out[f.Path] = names
	}
	return out, parsed.Problems
}

func TestLifecycleMapMatchesSchemaAndSource(t *testing.T) {
	doc, err := os.ReadFile(lifecycleMapPath)
	if err != nil {
		t.Fatal(err)
	}
	declared, problems := declaredNames(guardkit.Production(t))
	for _, p := range problems {
		t.Error(p)
	}
	tests := repoTestFuncs(t)
	checks := enumeratedChecks(t)
	if len(checks) == 0 {
		t.Fatal("the schema query found no CHECK that lists values; the guard would pass anything")
	}
	for _, p := range lifecycleMapProblems(string(doc), lifecycleFacts{
		checks:   checks,
		declared: func(file, name string) bool { return declared[file][name] },
		isTest:   func(name string) bool { return tests[name] },
		exists:   func(path string) bool { _, err := os.Stat(path); return err == nil },
	}) {
		t.Error(p)
	}
}

func TestLifecycleMapGuardCatchesMistakes(t *testing.T) {
	const clean = "# Lifecycles\n\n" +
		"| Lifecycle | States | Decided in | Writers |\n" +
		"|---|---|---|---|\n" +
		"| `things.status` | `open` `closed` | `things.go:canClose` | `things.go:closeThing`, `TestThingsClose` |\n" +
		"| `things.note_source` | `''` `typed` | inline | `web/src/Thing.tsx` |\n" +
		"| `things.phase` (no CHECK) | `new` `old` | the clock | none |\n\n" +
		lifecycleExemptHeading + "\n\n- `things.kind`: set at insert.\n"
	facts := func() lifecycleFacts {
		return lifecycleFacts{
			checks: map[string][]string{
				"things.status":      {"closed", "open"},
				"things.note_source": {"", "typed"},
				"things.kind":        {"a", "b"},
			},
			declared: func(file, name string) bool {
				return file == "things.go" && (name == "canClose" || name == "closeThing")
			},
			isTest: func(name string) bool { return name == "TestThingsClose" },
			exists: func(path string) bool { return path == "web/src/Thing.tsx" },
		}
	}
	if bad := lifecycleMapProblems(clean, facts()); len(bad) != 0 {
		t.Fatalf("the clean map failed: %v", bad)
	}
	for name, tc := range map[string]struct {
		doc   func(string) string
		facts func(*lifecycleFacts)
		want  string
	}{
		"a new status value in the schema": {
			facts: func(f *lifecycleFacts) { f.checks["things.status"] = []string{"closed", "open", "paused"} },
			want:  "things.status lists states",
		},
		"a state the schema dropped": {
			doc:  func(d string) string { return strings.Replace(d, "`open` `closed`", "`open` `closed` `gone`", 1) },
			want: "things.status lists states",
		},
		"a new enumerated column with no row": {
			facts: func(f *lifecycleFacts) { f.checks["widgets.status"] = []string{"off", "on"} },
			want:  "widgets.status has a CHECK that lists its values and no row",
		},
		"a column in both lists": {
			doc:  func(d string) string { return d + "- `things.status`: also here.\n" },
			want: "things.status is both",
		},
		"a second row for one column": {
			doc: func(d string) string {
				return strings.Replace(d, "| `things.note_source`", "| `things.status` | `open` `closed` | x | y |\n| `things.note_source`", 1)
			},
			want: "things.status has a second row",
		},
		"an exemption for a column with no CHECK": {
			doc:  func(d string) string { return d + "- `things.colour`: free.\n" },
			want: "things.colour has no CHECK",
		},
		"a renamed function": {
			doc:  func(d string) string { return strings.Replace(d, "things.go:closeThing", "things.go:shutThing", 1) },
			want: "shutThing is not declared in things.go",
		},
		"a function that moved file": {
			doc:  func(d string) string { return strings.Replace(d, "things.go:canClose", "other.go:canClose", 1) },
			want: "canClose is not declared in other.go",
		},
		"a test that is gone": {
			doc:  func(d string) string { return strings.Replace(d, "TestThingsClose", "TestThingsShut", 1) },
			want: "TestThingsShut is not a Go test",
		},
		"a web file that moved": {
			doc:  func(d string) string { return strings.Replace(d, "web/src/Thing.tsx", "web/src/Things.tsx", 1) },
			want: "web/src/Things.tsx does not exist",
		},
	} {
		t.Run(name, func(t *testing.T) {
			doc, f := clean, facts()
			if tc.doc != nil {
				doc = tc.doc(doc)
			}
			if tc.facts != nil {
				tc.facts(&f)
			}
			bad := lifecycleMapProblems(doc, f)
			if !strings.Contains(strings.Join(bad, "\n"), tc.want) {
				t.Errorf("want a problem containing %q, got %v", tc.want, bad)
			}
		})
	}
}
