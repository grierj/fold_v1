package events

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func mustExpand(t *testing.T, spec Spec, horizon time.Time) Expansion {
	t.Helper()
	x, err := Expand(spec, horizon)
	if err != nil {
		t.Fatalf("%+v: %v", spec, err)
	}
	return x
}

func instants(cs []Clock) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.StartsAt.UTC().Format(time.RFC3339)
		if c.First {
			out[i] += " first"
		}
		if c.WrittenLocal != "" {
			out[i] += " written " + c.WrittenLocal
		}
	}
	return out
}

// The evidence walk past the horizon is never stored: for every kind of rule
// the writer meets, including one that ends inside the evidence walk and one
// whose dates are dense, the stored dates stop at the horizon (or the end
// date), and the first time the walk saw past them is not among them.
func TestExpandStoresNothingFromItsEvidenceWalk(t *testing.T) {
	horizon := time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC)
	for _, spec := range []Spec{
		{StartsLocal: "2026-10-21T19:00", DurationMinutes: 60, TZID: "America/Boise", RRule: "FREQ=WEEKLY"},
		{StartsLocal: "2026-10-21T08:00", DurationMinutes: 60, TZID: "UTC", RRule: "FREQ=WEEKLY;BYHOUR=9,10;BYSECOND=30"},
		{StartsLocal: "2026-10-21T19:00", DurationMinutes: 60, TZID: "UTC", RRule: "FREQ=DAILY", Until: "2027-06-02"},
		{StartsLocal: "2026-10-21T19:00", DurationMinutes: 60, TZID: "UTC", RRule: "FREQ=DAILY;BYHOUR=9,10"},
	} {
		x := mustExpand(t, spec, horizon)
		if len(x.Whens) == 0 || len(x.tail) == 0 {
			t.Fatalf("%s: %d stored, %d walked past the horizon", spec.RRule, len(x.Whens), len(x.tail))
		}
		for _, w := range x.Whens {
			if w.StartsAt.After(horizon) {
				t.Fatalf("%s: stored %s past the horizon", spec.RRule, w.StartsAt)
			}
		}
		if !x.tail[0].StartsAt.After(horizon) {
			t.Fatalf("%s: the evidence walk began at %s, inside the horizon", spec.RRule, x.tail[0].StartsAt)
		}
	}
	// A rule that ends before the horizon walks nothing further.
	x := mustExpand(t, Spec{StartsLocal: "2011-03-06T00:00", DurationMinutes: 60, TZID: "America/St_Johns", RRule: "FREQ=WEEKLY;BYMINUTE=0,30;COUNT=4"}, horizon)
	if len(x.Whens) != 4 || len(x.tail) != 0 {
		t.Fatalf("COUNT=4: %d stored, %d walked past", len(x.Whens), len(x.tail))
	}
}

// A dense day is deduplicated in one pass in walk order. Every second of
// America/Boise's 2021-03-14 is generated, and the 3,600 seconds in its
// spring-forward gap resolve onto instants the day already has, so the day
// keeps its 82,800 distinct instants, in the order the walk first made each,
// with only the first marked. The day is historical, so the density cap does
// not apply to it; Clocks must stay linear in it.
func TestClocksDeduplicateADenseDayInOnePass(t *testing.T) {
	if _, err := LoadZone("America/Boise"); err != nil {
		t.Skip("no zone data")
	}
	var sixty []string
	for i := 0; i < 60; i++ {
		sixty = append(sixty, strconv.Itoa(i))
	}
	all := strings.Join(sixty, ",")
	rule := "FREQ=DAILY;COUNT=86400;BYHOUR=" + strings.Join(sixty[:24], ",") + ";BYMINUTE=" + all + ";BYSECOND=" + all
	spec := Spec{StartsLocal: "2021-03-14T00:00", DurationMinutes: 60, TZID: "America/Boise", RRule: rule}
	x := mustExpand(t, spec, time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC))
	const day = "2021-03-14"
	var order []time.Time
	first := map[time.Time]bool{}
	for _, w := range x.Whens {
		if w.OriginalDate != day {
			t.Fatalf("fixture: stored %s outside %s", w.StartsLocal, day)
		}
		if k := w.StartsAt.UTC(); !first[k] {
			first[k] = true
			order = append(order, k)
		}
	}
	if len(x.Whens) != 86400 || len(order) != 86400-3600 {
		t.Fatalf("fixture: %d times, %d distinct, want 86400 and 82800", len(x.Whens), len(order))
	}
	began := time.Now()
	cs := x.Clocks([]string{day})[day]
	if took := time.Since(began); took > 2*time.Second {
		t.Fatalf("Clocks took %s over one dense day: deduplication is not one pass", took)
	}
	if len(cs) != len(order) {
		t.Fatalf("%d clocks, want %d distinct instants", len(cs), len(order))
	}
	for i, c := range cs {
		if !c.StartsAt.Equal(order[i]) || c.First != (i == 0) || c.WrittenLocal != "" {
			t.Fatalf("clock %d = %+v, want %s first=%v", i, c, order[i], i == 0)
		}
	}
}

// A day keeps every distinct time the writer generated, in the walk's order,
// with its seconds; the first is marked once.
func TestClocksKeepEveryTimeOfADay(t *testing.T) {
	spec := Spec{StartsLocal: "2026-10-21T08:00", DurationMinutes: 60, TZID: "UTC", RRule: "FREQ=WEEKLY;BYHOUR=9,10;BYSECOND=30"}
	x := mustExpand(t, spec, time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC))
	got := instants(x.Clocks([]string{"2026-11-04"})["2026-11-04"])
	want := []string{"2026-11-04T09:00:30Z first", "2026-11-04T10:00:30Z"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("clocks %v, want %v", got, want)
	}
	if x.TZID() != "UTC" || !x.Replayable() {
		t.Fatalf("zone %q, replayable %v", x.TZID(), x.Replayable())
	}
}

// A kept day the rule does not generate gets the series start's written
// clock on that day, resolved in the series zone, but only where the walk
// proves the rule makes nothing there.
func TestClocksKeptDayUsesTheWrittenClock(t *testing.T) {
	spec := Spec{StartsLocal: "2026-10-21T19:00", DurationMinutes: 60, TZID: "America/Boise", RRule: "FREQ=WEEKLY;BYDAY=TH"}
	x := mustExpand(t, spec, time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC))
	got := instants(x.Clocks([]string{"2026-11-04"})["2026-11-04"])
	want := []string{"2026-11-05T02:00:00Z first written 2026-11-04T19:00"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("clocks %v, want %v", got, want)
	}
}

// In a midnight daylight-saving gap the written 00:30 keeps its day and clock
// while its instant is where time.Date resolves it.
func TestClocksKeptDayInAMidnightGap(t *testing.T) {
	loc, err := LoadZone("America/Havana")
	if err != nil {
		t.Skip("no zone data")
	}
	// Havana skipped 00:00-01:00 on 2022-03-13.
	day := "2022-03-13"
	resolved := time.Date(2022, 3, 13, 0, 30, 0, 0, loc)
	if resolved.Format(WallLayout) == day+"T00:30" {
		t.Skip("fixture: no gap at 00:30 in this zone data")
	}
	spec := Spec{StartsLocal: "2022-01-05T00:30", DurationMinutes: 60, TZID: "America/Havana", RRule: "FREQ=WEEKLY;COUNT=1"}
	x := mustExpand(t, spec, time.Date(2023, 6, 1, 0, 0, 0, 0, time.UTC))
	cs := x.Clocks([]string{day})[day]
	if len(cs) != 1 || cs[0].WrittenLocal != day+"T00:30" || !cs[0].StartsAt.Equal(resolved) || !cs[0].First {
		t.Fatalf("clocks %v, want written %sT00:30 at %s", instants(cs), day, resolved.UTC())
	}
}

// A time daylight saving moves from the next day's skipped clock is the
// earlier day's own evidence, as the writer stored it there.
func TestClocksStJohnsNextDayNormalization(t *testing.T) {
	spec := Spec{StartsLocal: "2011-03-06T00:00", DurationMinutes: 60, TZID: "America/St_Johns", RRule: "FREQ=WEEKLY;BYMINUTE=0,30;COUNT=4"}
	x := mustExpand(t, spec, time.Date(2012, 1, 1, 0, 0, 0, 0, time.UTC))
	loc, _ := LoadZone(spec.TZID)
	var stored []string
	for _, w := range x.Whens {
		if w.OriginalDate == "2011-03-12" {
			stored = append(stored, w.StartsLocal)
		}
	}
	cs := x.Clocks([]string{"2011-03-12"})["2011-03-12"]
	var walls []string
	for _, c := range cs {
		walls = append(walls, FormatWall(c.StartsAt, loc))
	}
	if !reflect.DeepEqual(stored, []string{"2011-03-12T23:30"}) || !reflect.DeepEqual(walls, stored) || !cs[0].First {
		t.Fatalf("stored %v, evidence %v", stored, walls)
	}
}

// The review's Kathmandu rule (a 1986 start, the year the zone moved to
// +05:45, twelve times a day): the 2027 date's evidence starts at 09:00.
func TestClocksKathmanduTransition(t *testing.T) {
	spec := Spec{StartsLocal: "1986-02-01T09:00", DurationMinutes: 60, TZID: "Asia/Kathmandu", RRule: "FREQ=YEARLY;BYHOUR=9,10,11,12,13,14,15,16,17,18,19,20"}
	x := mustExpand(t, spec, time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC))
	loc, _ := LoadZone(spec.TZID)
	cs := x.Clocks([]string{"2027-02-01"})["2027-02-01"]
	if len(cs) != 12 || FormatWall(cs[0].StartsAt, loc) != "2027-02-01T09:00" || !cs[0].First || cs[1].First {
		t.Fatalf("evidence %v", instants(cs))
	}
}

// The two passes through a repeated hour are two instants with one wall
// clock; the writer's pass is the evidence, never the other.
func TestClocksRepeatedHourKeepsTheWritersPass(t *testing.T) {
	spec := Spec{StartsLocal: "2026-10-25T01:30", DurationMinutes: 60, TZID: "America/Boise", RRule: "FREQ=WEEKLY"}
	x := mustExpand(t, spec, time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC))
	var w When
	for _, s := range x.Whens {
		if s.OriginalDate == "2026-11-01" {
			w = s
		}
	}
	cs := x.Clocks([]string{"2026-11-01"})["2026-11-01"]
	if len(cs) != 1 || !cs[0].StartsAt.Equal(w.StartsAt) {
		t.Fatalf("evidence %v, stored %v", instants(cs), w.StartsAt)
	}
}

// A day past what the walk proves gets no evidence, never an invented
// fallback: a missing day beyond a short horizon, and a day the horizon
// splits proves both of its times through the evidence walk.
func TestClocksNeverInventPastTheWalk(t *testing.T) {
	spec := Spec{StartsLocal: "2026-10-21T08:00", DurationMinutes: 60, TZID: "UTC", RRule: "FREQ=WEEKLY;BYHOUR=9,10"}
	// The horizon falls between the two times of Nov 4.
	horizon := time.Date(2026, 11, 4, 9, 30, 0, 0, time.UTC)
	x := mustExpand(t, spec, horizon)
	last := x.Whens[len(x.Whens)-1]
	if last.OriginalDate != "2026-11-04" || last.StartsLocal != "2026-11-04T09:00" {
		t.Fatalf("fixture: last stored date %+v", last)
	}
	far := "2027-03-03"
	got := x.Clocks([]string{"2026-11-04", far, "2026-11-05"})
	if want := []string{"2026-11-04T09:00:00Z first", "2026-11-04T10:00:00Z"}; !reflect.DeepEqual(instants(got["2026-11-04"]), want) {
		t.Fatalf("boundary day %v, want %v", instants(got["2026-11-04"]), want)
	}
	if _, ok := got[far]; ok || x.Proves(far) {
		t.Fatalf("a day past the walk has evidence %v", instants(got[far]))
	}
	// Nov 5 is inside the evidence walk and the rule makes nothing there:
	// that is proven, so a kept date there takes the written clock.
	if cs := got["2026-11-05"]; len(cs) != 1 || cs[0].WrittenLocal != "2026-11-05T08:00" {
		t.Fatalf("a proven empty day %v", instants(cs))
	}
}

// When the evidence walk spends its MaxOccurrences budget partway through a
// day, that day was only partly seen: it is not proven and gets no evidence,
// neither the times seen nor a written-clock fallback. A rule making every
// minute starts at noon, so the stored walk (720 times) is under the density
// cap, and the walk past a horizon just before midnight sees all 1,440 times
// of the next day and 560 of the one after.
func TestClocksTailBudgetLeavesAPartDayUnknown(t *testing.T) {
	var hours, minutes []string
	for i := 0; i < 60; i++ {
		if i < 24 {
			hours = append(hours, strconv.Itoa(i))
		}
		minutes = append(minutes, strconv.Itoa(i))
	}
	spec := Spec{StartsLocal: "2026-03-01T12:00", DurationMinutes: 30, TZID: "UTC",
		RRule: "FREQ=DAILY;BYHOUR=" + strings.Join(hours, ",") + ";BYMINUTE=" + strings.Join(minutes, ",")}
	x := mustExpand(t, spec, time.Date(2026, 3, 1, 23, 59, 30, 0, time.UTC))
	if len(x.Whens) != 720 || len(x.tail) != MaxOccurrences || x.ended {
		t.Fatalf("fixture: %d stored, %d walked past the horizon (want %d), ended %v", len(x.Whens), len(x.tail), MaxOccurrences, x.ended)
	}
	const part = "2026-03-03"
	if last := x.tail[len(x.tail)-1]; last.OriginalDate != part || last.StartsLocal != part+"T09:19" {
		t.Fatalf("fixture: the walk stopped at %s, want partway through %s", last.StartsLocal, part)
	}
	if x.Proves(part) {
		t.Fatalf("a day the walk saw %s of is proven (through %s)", "560 minutes", x.through)
	}
	if cs, ok := x.Clocks([]string{part})[part]; ok {
		t.Fatalf("a partly seen day has evidence: %d clocks, first %v", len(cs), instants(cs[:1]))
	}
}

// A rule that ends (COUNT, UNTIL) proves every later day.
func TestClocksEndedRuleProvesEveryDay(t *testing.T) {
	for _, spec := range []Spec{
		{StartsLocal: "2026-10-21T19:00", DurationMinutes: 60, TZID: "UTC", RRule: "FREQ=WEEKLY;COUNT=2"},
		{StartsLocal: "2026-10-21T19:00", DurationMinutes: 60, TZID: "UTC", RRule: "FREQ=WEEKLY", Until: "2026-10-28"},
	} {
		x := mustExpand(t, spec, time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC))
		if !x.Proves("2030-01-01") {
			t.Errorf("%s until %q: an ended rule proves nothing late", spec.RRule, spec.Until)
		}
	}
}

// A series repeating from Go's zero instant is walked from the current time:
// its evidence comes from the same walk as its stored dates, and it is not
// replayable.
func TestClocksZeroStartComeFromTheSameWalk(t *testing.T) {
	spec := Spec{StartsLocal: "0001-01-01T00:00", DurationMinutes: 60, TZID: "UTC", RRule: "FREQ=YEARLY;BYMONTH=1;BYMONTHDAY=1;BYHOUR=9"}
	x := mustExpand(t, spec, WindowEnd(time.Time{}, time.Now().UTC()))
	if x.Replayable() || len(x.Whens) == 0 {
		t.Fatalf("replayable %v, %d dates", x.Replayable(), len(x.Whens))
	}
	w := x.Whens[0]
	cs := x.Clocks([]string{w.OriginalDate})[w.OriginalDate]
	if len(cs) != 1 || !cs[0].StartsAt.Equal(w.StartsAt) || !cs[0].First {
		t.Fatalf("evidence %v, stored %v", instants(cs), w.StartsAt)
	}
}

// The long America/Lima BYSETPOS rule of review 7 is accepted by the writer,
// and its evidence for a 2027 day is that walk's own time.
func TestClocksLongLimaBySetPos(t *testing.T) {
	rule := "FREQ=YEARLY;BYDAY=MO,TU,WE,TH,FR,SA,SU;BYSETPOS=" + strings.TrimSuffix(strings.Repeat("1,", 100), ",")
	if len(rule) > 300 {
		rule = rule[:strings.LastIndex(rule[:300], ",")]
	}
	spec := Spec{StartsLocal: "1827-01-01T09:00", DurationMinutes: 60, TZID: "America/Lima", RRule: rule}
	x := mustExpand(t, spec, time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC))
	var w When
	for _, s := range x.Whens {
		if strings.HasPrefix(s.OriginalDate, "2027-") {
			w = s
		}
	}
	if w.OriginalDate == "" {
		t.Fatal("fixture: no 2027 date")
	}
	cs := x.Clocks([]string{w.OriginalDate})[w.OriginalDate]
	if len(cs) != 1 || !cs[0].StartsAt.Equal(w.StartsAt) {
		t.Fatalf("evidence %v, stored %v", instants(cs), w)
	}
}
