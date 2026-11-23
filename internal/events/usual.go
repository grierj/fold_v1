package events

import (
	"strings"
	"time"

	"github.com/teambition/rrule-go"
)

// The series clock evidence (DEC-155). The writers record, for each date they
// write, the instants their own walk of the rule generated for its original
// day, so the date page can compare a date with its series without walking
// the rule again.

// Expansion is one walk of a series by the writer (Expand). Whens is exactly
// what the writers store. When the walk stops at the horizon, Expand keeps
// pulling the same iterator a few days further for evidence only: those times
// are never stored and never count toward the density cap.
type Expansion struct {
	Whens []When
	tail  []When
	loc   *time.Location
	// written is the series start's HH:MM as written, without the zone.
	written string
	// ended: the walk reached the rule's end (its COUNT, or the first time
	// past UNTIL, after which the writer stores nothing), so every day is
	// proven. Otherwise through is the last original day proven complete
	// ("" for none).
	ended   bool
	through string
	// replayable is false for a series starting at Go's zero instant:
	// rrule-go walks that from the current time (buildRRule), so only the
	// walk that wrote a date can vouch for its clock.
	replayable bool
}

// Clock is one instant the writer generated for a date's original day.
// WrittenLocal is set only for a kept day the rule does not generate: the
// series start's written wall clock on that day, which StartsAt resolves.
type Clock struct {
	StartsAt     time.Time // UTC
	First        bool
	WrittenLocal string
}

// tailSpan is how far past the horizon the evidence walk goes. tailSlack
// bounds how far before an emitted time a later one can resolve: rrule-go
// emits times in wall-clock order, and a time in a daylight-saving gap or a
// skipped day (no zone has skipped more than one) resolves up to a day off its
// wall clock, onto another original day.
const (
	tailSpan  = 4 * 24 * time.Hour
	tailSlack = 2 * 24 * time.Hour
)

// walkTail continues the writer's walk from first, the time that passed the
// horizon. It proves every original day up to the one before first's
// stopping point less tailSlack. A rule that keeps making times inside the
// span stops after MaxOccurrences of them, proving less; it never fails the
// write.
func (x *Expansion) walkTail(next rrule.Next, first, horizonEnd time.Time, dur time.Duration, until string) {
	stop := horizonEnd.Add(tailSpan)
	t := first
	for steps := 0; steps < MaxOccurrences && !t.After(stop); steps++ {
		w := whenOf(t.In(x.loc), dur, x.loc)
		if until != "" && w.OriginalDate > until {
			x.ended = true
			return
		}
		x.tail = append(x.tail, w)
		var ok bool
		if t, ok = next(); !ok {
			x.ended = true
			return
		}
	}
	x.through = t.Add(-tailSlack).In(x.loc).AddDate(0, 0, -1).Format("2006-01-02")
}

// Replayable is whether a later walk of this series makes the same times, so
// a repair may rebuild a date's evidence from it. A series starting at Go's
// zero instant is not.
func (x Expansion) Replayable() bool { return x.replayable }

// TZID is the zone the walk generated its instants in.
func (x Expansion) TZID() string { return x.loc.String() }

// Proves is whether this walk saw every time the rule makes on day.
func (x Expansion) Proves(day string) bool {
	return x.ended || (x.through != "" && day <= x.through)
}

// Clocks is the evidence this walk gives each of days (original dates,
// YYYY-MM-DD) it proves: every distinct instant it generated for the day, in
// the walk's order, the first marked First; or, for a day it generates no
// time on, the series start's written clock on that day (one Clock, with
// WrittenLocal). A day the walk does not prove, because the walk stopped
// before seeing all of it, is absent: its clock is unknown. One pass over the
// walk; duplicate instants on a day are kept once.
func (x Expansion) Clocks(days []string) map[string][]Clock {
	want := make(map[string]bool, len(days))
	for _, d := range days {
		want[d] = true
	}
	out := make(map[string][]Clock, len(days))
	seen := make(map[string]map[time.Time]bool, len(days))
	add := func(w When) {
		if !want[w.OriginalDate] {
			return
		}
		got := out[w.OriginalDate]
		key := w.StartsAt.UTC()
		if seen[w.OriginalDate][key] {
			return
		}
		if seen[w.OriginalDate] == nil {
			seen[w.OriginalDate] = make(map[time.Time]bool)
		}
		seen[w.OriginalDate][key] = true
		out[w.OriginalDate] = append(got, Clock{StartsAt: w.StartsAt, First: len(got) == 0})
	}
	for _, w := range x.Whens {
		add(w)
	}
	for _, w := range x.tail {
		add(w)
	}
	for d := range want {
		if !x.Proves(d) {
			delete(out, d)
			continue
		}
		if len(out[d]) > 0 {
			continue
		}
		if c, ok := x.keptDay(d); ok {
			out[d] = []Clock{c}
		} else {
			delete(out, d)
		}
	}
	return out
}

// keptDay is the series start's written HH:MM on day, resolved in the series
// zone. In a daylight-saving gap time.Date moves the instant; WrittenLocal
// keeps the day and the clock as written.
func (x Expansion) keptDay(day string) (Clock, bool) {
	d, err := time.Parse("2006-01-02", day)
	if err != nil || x.written == "" {
		return Clock{}, false
	}
	hm, err := time.Parse("15:04", x.written)
	if err != nil {
		return Clock{}, false
	}
	at := time.Date(d.Year(), d.Month(), d.Day(), hm.Hour(), hm.Minute(), 0, 0, x.loc)
	return Clock{StartsAt: at.UTC(), First: true, WrittenLocal: day + "T" + x.written}, true
}

// writtenClock is a startsLocal's HH:MM as written, read without a zone, so a
// series start that is itself in a gap keeps its written hour; "" when it
// does not parse.
func writtenClock(startsLocal string) string {
	t, err := time.Parse(WallLayout, strings.TrimSpace(startsLocal))
	if err != nil {
		return ""
	}
	return t.Format("15:04")
}
