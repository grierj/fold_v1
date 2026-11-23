package policy

import (
	"testing"
	"time"
)

func TestDecideCalendar(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	base := CalendarFacts{Authorized: true, PublicDate: true, StartsAt: now.AddDate(0, 0, 7)}
	tests := []struct {
		name  string
		scope CalendarScope
		facts CalendarFacts
		want  CalendarDecision
	}{
		{"authorization remains the gate", CalendarScopeMine, CalendarFacts{GroupMember: true}, CalendarDecision{}},
		{"outsider public unanswered absent from mine", CalendarScopeMine, base, CalendarDecision{}},
		{"outsider public going joins mine", CalendarScopeMine, withCalendar(base, func(f *CalendarFacts) { f.OwnStatus = "going" }), CalendarDecision{Include: true, OwnScope: true}},
		{"outsider public declined absent from mine", CalendarScopeMine, withCalendar(base, func(f *CalendarFacts) { f.OwnStatus = "declined" }), CalendarDecision{OwnScope: true}},
		{"outsider public declined stays in all", CalendarScopeAll, withCalendar(base, func(f *CalendarFacts) { f.OwnStatus = "declined" }), CalendarDecision{Include: true, OwnScope: true}},
		{"member date exempt from window", CalendarScopeMine, withCalendar(base, func(f *CalendarFacts) { f.GroupMember = true; f.StartsAt = now.AddDate(0, 0, 90) }), CalendarDecision{Include: true, OwnScope: true}},
		{"host date exempt from window", CalendarScopeMine, withCalendar(base, func(f *CalendarFacts) { f.Host = true; f.StartsAt = now.AddDate(0, 0, 90) }), CalendarDecision{Include: true, OwnScope: true}},
		{"household date is scope but not an own answer", CalendarScopeMine, withCalendar(base, func(f *CalendarFacts) { f.HouseholdDate = true }), CalendarDecision{Include: true, OwnScope: true}},
		{"household does not override own declined in mine", CalendarScopeMine, withCalendar(base, func(f *CalendarFacts) { f.HouseholdDate = true; f.OwnStatus = "declined" }), CalendarDecision{OwnScope: true}},
		{"household own declined remains discoverable in all", CalendarScopeAll, withCalendar(base, func(f *CalendarFacts) { f.HouseholdDate = true; f.OwnStatus = "declined" }), CalendarDecision{Include: true, OwnScope: true}},
		{"household scope survives discovery window", CalendarScopeAll, withCalendar(base, func(f *CalendarFacts) { f.HouseholdDate = true; f.StartsAt = now.AddDate(0, 0, 90) }), CalendarDecision{Include: true, OwnScope: true}},
		{"exact date is scope", CalendarScopeMine, withCalendar(base, func(f *CalendarFacts) { f.ExactDate = true }), CalendarDecision{Include: true, OwnScope: true}},
		{"church wide inside mine window", CalendarScopeMine, withCalendar(base, func(f *CalendarFacts) { f.ChurchWide = true }), CalendarDecision{Include: true, OwnScope: true}},
		{"church wide beyond mine window", CalendarScopeMine, withCalendar(base, func(f *CalendarFacts) { f.ChurchWide = true; f.StartsAt = now.AddDate(0, 0, 31) }), CalendarDecision{OwnScope: true}},
		{"public inside all window", CalendarScopeAll, base, CalendarDecision{Include: true}},
		{"public at all boundary", CalendarScopeAll, withCalendar(base, func(f *CalendarFacts) { f.StartsAt = now.AddDate(0, 0, 30) }), CalendarDecision{Include: true}},
		{"public one instant past boundary", CalendarScopeAll, withCalendar(base, func(f *CalendarFacts) { f.StartsAt = now.AddDate(0, 0, 30).Add(time.Nanosecond) }), CalendarDecision{}},
		{"past unanswered public omitted", CalendarScopeAll, withCalendar(base, func(f *CalendarFacts) { f.StartsAt = now.Add(-time.Nanosecond) }), CalendarDecision{}},
		{"anonymous church-wide at all boundary", CalendarScopeAll, withCalendar(base, func(f *CalendarFacts) { f.PublicDate = false; f.ChurchWide = true; f.StartsAt = now.AddDate(0, 0, 30) }), CalendarDecision{Include: true, OwnScope: true}},
		{"anonymous church-wide past omitted", CalendarScopeAll, withCalendar(base, func(f *CalendarFacts) {
			f.PublicDate = false
			f.ChurchWide = true
			f.StartsAt = now.Add(-time.Nanosecond)
		}), CalendarDecision{OwnScope: true}},
		{"anonymous church-wide beyond boundary omitted", CalendarScopeAll, withCalendar(base, func(f *CalendarFacts) {
			f.PublicDate = false
			f.ChurchWide = true
			f.StartsAt = now.AddDate(0, 0, 30).Add(time.Nanosecond)
		}), CalendarDecision{OwnScope: true}},
		{"signed-out church-wide date is shown but never theirs", CalendarScopeAll, withCalendar(base, func(f *CalendarFacts) {
			f.ChurchWide = true
			f.SignedOut = true
			f.StartsAt = now.AddDate(0, 0, 3)
		}), CalendarDecision{Include: true}},
		{"visible private date remains in all", CalendarScopeAll, withCalendar(base, func(f *CalendarFacts) { f.PublicDate = false; f.StartsAt = now.AddDate(0, 0, 90) }), CalendarDecision{Include: true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := DecideCalendar(tc.scope, tc.facts, now); got != tc.want {
				t.Fatalf("got %+v want %+v", got, tc.want)
			}
		})
	}
}

func withCalendar(f CalendarFacts, change func(*CalendarFacts)) CalendarFacts {
	change(&f)
	return f
}

// TestDecideCalendarManagerKept (DEC-157): a managed kept date stays in
// Everything outside DEC-77's window, at any distance and while underway, and
// changes nothing in Just mine (church-wide dates included, since own scope
// covers them) or in own scope.
func TestDecideCalendarManagerKept(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	starts := map[string]time.Time{"-1h": now.Add(-time.Hour), "+45d": now.AddDate(0, 0, 45), "+121d": now.AddDate(0, 0, 121)}
	shapes := []struct {
		name  string
		facts CalendarFacts
	}{
		{"public", CalendarFacts{Authorized: true, PublicDate: true}},
		{"church-wide", CalendarFacts{Authorized: true, ChurchWide: true}},
		{"church-wide public", CalendarFacts{Authorized: true, ChurchWide: true, PublicDate: true}},
		{"member", CalendarFacts{Authorized: true, PublicDate: true, GroupMember: true}},
		{"host", CalendarFacts{Authorized: true, ChurchWide: true, Host: true}},
		{"going", CalendarFacts{Authorized: true, PublicDate: true, OwnStatus: "going"}},
		{"declined", CalendarFacts{Authorized: true, PublicDate: true, OwnStatus: "declined"}},
	}
	for _, shape := range shapes {
		for at, start := range starts {
			for _, scope := range []CalendarScope{CalendarScopeAll, CalendarScopeMine} {
				t.Run(shape.name+" "+at+" "+string(scope), func(t *testing.T) {
					facts := shape.facts
					facts.StartsAt = start
					without := DecideCalendar(scope, facts, now)
					facts.ManagerKept = true
					with := DecideCalendar(scope, facts, now)
					if with.OwnScope != without.OwnScope {
						t.Fatalf("ManagerKept changed own scope: %+v, without %+v", with, without)
					}
					switch scope {
					case CalendarScopeAll:
						if !with.Include {
							t.Fatalf("Everything left out a managed kept date: %+v", with)
						}
					case CalendarScopeMine:
						if with != without {
							t.Fatalf("Just mine changed with ManagerKept: %+v, without %+v", with, without)
						}
					}
				})
			}
		}
	}
	declined := CalendarFacts{Authorized: true, PublicDate: true, OwnStatus: "declined", ManagerKept: true, StartsAt: now.AddDate(0, 0, 3)}
	if got := DecideCalendar(CalendarScopeMine, declined, now); got.Include {
		t.Fatalf("a declined answer joined Just mine through ManagerKept: %+v", got)
	}
	unauthorized := CalendarFacts{PublicDate: true, ManagerKept: true, StartsAt: now.AddDate(0, 0, 3)}
	if got := DecideCalendar(CalendarScopeAll, unauthorized, now); got.Include {
		t.Fatalf("ManagerKept authorized a date: %+v", got)
	}
	// The church-wide Just mine case beyond the window is the one an own-scope
	// placement of ManagerKept would break.
	cw := CalendarFacts{Authorized: true, ChurchWide: true, ManagerKept: true, StartsAt: now.AddDate(0, 0, 45)}
	if got := DecideCalendar(CalendarScopeMine, cw, now); got.Include {
		t.Fatalf("an unhosted church-wide kept date 45 days ahead joined Just mine: %+v", got)
	}
	cw.StartsAt = now.Add(-time.Hour)
	if got := DecideCalendar(CalendarScopeMine, cw, now); got.Include {
		t.Fatalf("an underway unhosted church-wide kept date joined Just mine: %+v", got)
	}
}
