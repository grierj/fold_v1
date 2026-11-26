package policy

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

var regStatuses = []string{RegGoing, RegMaybe, RegDeclined, RegCancelled, RegWaitlisted, RegRemoved}

// regFroms is every status a move may start from, "no row" first.
var regFroms = append([]string{RegNone}, regStatuses...)

func regName(s string) string {
	if s == RegNone {
		return "none"
	}
	return s
}

// TestRegistrationMoves pins every (move, from, to) triple: for each move, the
// "from>to" pairs it allows as its last write, and the further pairs it allows
// only as a step before it (the batch's seat release).
func TestRegistrationMoves(t *testing.T) {
	pairs := func(froms, tos []string) []string {
		var out []string
		for _, f := range froms {
			for _, to := range tos {
				out = append(out, regName(f)+">"+to)
			}
		}
		return out
	}
	answerFrom := []string{RegNone, RegGoing, RegMaybe, RegDeclined, RegCancelled, RegWaitlisted}
	answerTo := []string{RegGoing, RegMaybe, RegDeclined, RegWaitlisted}
	current := []string{RegGoing, RegMaybe, RegWaitlisted}
	want := map[RegistrationMove]struct{ last, step []string }{
		RegAnswer:           {last: pairs(answerFrom, answerTo)},
		RegUndoSeriesAnswer: {last: pairs(answerTo, []string{RegGoing, RegMaybe, RegDeclined, RegCancelled, RegWaitlisted})},
		RegConfirmGuest:     {last: pairs(answerFrom, []string{RegGoing, RegWaitlisted})},
		RegCancelFromLink:   {last: pairs(current, []string{RegCancelled})},
		RegPromote:          {last: []string{"waitlisted>going"}},
		RegAddByManager:     {last: pairs(regFroms, []string{RegGoing})},
		RegRemoveByManager:  {last: pairs([]string{RegGoing, RegMaybe, RegDeclined, RegWaitlisted}, []string{RegRemoved})},
		RegLeaveGroup:       {last: pairs(current, []string{RegCancelled}), step: []string{"going>declined"}},
		RegRemoveFromGroup:  {last: pairs(current, []string{RegRemoved}), step: []string{"going>declined"}},
		RegMorePrivate:      {last: pairs(current, []string{RegCancelled})},
		RegHouseholdPersonChanged: {last: append(pairs(current, []string{RegCancelled}),
			"going>going", "maybe>maybe", "waitlisted>waitlisted")},
	}
	moves := RegistrationMoves()
	if len(moves) != len(want) {
		t.Fatalf("the table has %d moves, the test %d: %v", len(moves), len(want), moves)
	}
	for _, m := range moves {
		w, ok := want[m]
		if !ok {
			t.Errorf("%q has no row in this test", m)
			continue
		}
		var last, step []string
		for _, from := range regFroms {
			for _, to := range regStatuses {
				pair := regName(from) + ">" + to
				switch {
				case m.Allows(from, to):
					last = append(last, pair)
					if !m.AllowsStep(from, to) {
						t.Errorf("%q allows %s last but not as a step", m, pair)
					}
				case m.AllowsStep(from, to):
					step = append(step, pair)
				}
			}
			if m.Allows(from, RegNone) || m.AllowsStep(from, RegNone) {
				t.Errorf("%q ends %s in no row", m, regName(from))
			}
		}
		slices.Sort(last)
		slices.Sort(step)
		slices.Sort(w.last)
		slices.Sort(w.step)
		if !slices.Equal(last, w.last) {
			t.Errorf("%q last writes:\n got %s\nwant %s", m, strings.Join(last, " "), strings.Join(w.last, " "))
		}
		if !slices.Equal(step, w.step) {
			t.Errorf("%q steps:\n got %s\nwant %s", m, strings.Join(step, " "), strings.Join(w.step, " "))
		}
		for _, from := range regFroms {
			starts := false
			for _, to := range regStatuses {
				starts = starts || m.AllowsStep(from, to)
			}
			if m.StartsFrom(from) != starts {
				t.Errorf("%q StartsFrom(%s) = %v, but it allows a write from it: %v", m, regName(from), m.StartsFrom(from), starts)
			}
		}
	}
}

// Every status the table names is a registration status, and every one is
// named by some move; root's TestRegistrationStatusesMatchTheSchema ties the
// list to the CHECK.
func TestRegistrationMovesNameEveryStatus(t *testing.T) {
	named := map[string]bool{}
	for _, m := range RegistrationMoves() {
		for _, s := range m.From() {
			if s != RegNone && !slices.Contains(regStatuses, s) {
				t.Errorf("%q starts from %q, which is not a registration status", m, s)
			}
			named[s] = true
		}
		for _, s := range regStatuses {
			if m.AllowsStep(RegGoing, s) || m.AllowsStep(RegWaitlisted, s) || m.AllowsStep(RegNone, s) {
				named[s] = true
			}
		}
	}
	for _, s := range regStatuses {
		if !named[s] {
			t.Errorf("no move names %q", s)
		}
	}
}

func TestUnknownRegistrationMoveAllowsNothing(t *testing.T) {
	m := RegistrationMove("publish")
	for _, from := range regFroms {
		if m.StartsFrom(from) {
			t.Errorf("an unknown move starts from %s", regName(from))
		}
		for _, to := range regStatuses {
			if m.AllowsStep(from, to) {
				t.Errorf("an unknown move takes %s to %s", regName(from), to)
			}
		}
	}
	if len(m.From()) != 0 {
		t.Errorf("an unknown move starts from %v", m.From())
	}
}

func TestRegistrationFromIsACopy(t *testing.T) {
	from := RegCancelFromLink.From()
	from[0] = RegRemoved
	if !reflect.DeepEqual(RegCancelFromLink.From(), []string{RegGoing, RegMaybe, RegWaitlisted}) {
		t.Fatal("changing From's result changed the table")
	}
	if !reflect.DeepEqual(CurrentRegStatuses, []string{RegGoing, RegMaybe, RegWaitlisted}) {
		t.Fatalf("CurrentRegStatuses is %v", CurrentRegStatuses)
	}
}

// The from-sets are exactly the status checks they replaced in the root
// package, for every status and for no row.
func TestRegistrationStartsFromReplacesTheOldChecks(t *testing.T) {
	for _, s := range regFroms {
		for _, c := range []struct {
			name      string
			got, want bool
		}{
			// prepareRegistration: a found row that is removed cannot answer.
			{"answer", RegAnswer.StartsFrom(s), s != RegRemoved},
			// apiConfirmGuestRegistration: a prior removed row is refused.
			{"guest confirm", RegConfirmGuest.StartsFrom(s), s != RegRemoved},
			// apiCancelGuestRegistration: going, maybe or waitlisted.
			{"link cancel", RegCancelFromLink.StartsFrom(s), s == RegGoing || s == RegMaybe || s == RegWaitlisted},
			// apiRemoveRegistration: status NOT IN (removed, cancelled), on a row.
			{"manager removal", RegRemoveByManager.StartsFrom(s), s != RegNone && s != RegRemoved && s != RegCancelled},
			// apiPromoteRegistration and promoteRegistration: status = waitlisted.
			{"promotion", RegPromote.StartsFrom(s), s == RegWaitlisted},
		} {
			if c.got != c.want {
				t.Errorf("%s from %s: the rule says %v, the old check %v", c.name, regName(s), c.got, c.want)
			}
		}
	}
}
