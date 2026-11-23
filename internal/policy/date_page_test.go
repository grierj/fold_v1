package policy

import "testing"

// One row per viewer of M8.1 brief section 4.
func TestDatePage(t *testing.T) {
	all := DatePageFacts{Header: true, Attendance: true, Names: true, Declined: true, Capacity: true, ChildcareTotals: true, Series: true, Coverage: true}
	for _, tt := range []struct {
		name string
		r    DateReader
		want DatePageFacts
	}{
		{"invitation only, even with every other fact true", DateReader{SeriesVisible: true, AttendeeNames: true, Manager: true, Childcare: true, Recurring: true, SignedIn: true}, DatePageFacts{}},
		{"nobody", DateReader{}, DatePageFacts{}},
		{"ordinary member, series visible", DateReader{Ordinary: true, SeriesVisible: true, AttendeeNames: true, Recurring: true, SignedIn: true},
			DatePageFacts{Header: true, Attendance: true, Names: true, Capacity: true, Series: true, Coverage: true}},
		{"ordinary member of a one-time event: no coverage", DateReader{Ordinary: true, SeriesVisible: true, AttendeeNames: true, SignedIn: true},
			DatePageFacts{Header: true, Attendance: true, Names: true, Capacity: true, Series: true}},
		{"outsider of an opened invite-only group's event: no names", DateReader{Ordinary: true, SeriesVisible: true, Recurring: true, SignedIn: true},
			DatePageFacts{Header: true, Attendance: true, Capacity: true, Series: true, Coverage: true}},
		{"manager", DateReader{Ordinary: true, SeriesVisible: true, AttendeeNames: true, Manager: true, Childcare: true, Recurring: true, SignedIn: true}, all},
		{"manager without the childcare right gets no totals", DateReader{Ordinary: true, SeriesVisible: true, AttendeeNames: true, Manager: true, Recurring: true, SignedIn: true},
			DatePageFacts{Header: true, Attendance: true, Names: true, Declined: true, Capacity: true, Series: true, Coverage: true}},
		{"opened date of a hidden series", DateReader{Ordinary: true, AttendeeNames: true, Recurring: true, SignedIn: true},
			DatePageFacts{Header: true, Attendance: true, Names: true, Capacity: true}},
		{"manager reaching only the opened date gets no childcare totals or series", DateReader{Ordinary: true, AttendeeNames: true, Manager: true, Childcare: true, Recurring: true, SignedIn: true},
			DatePageFacts{Header: true, Attendance: true, Names: true, Declined: true, Capacity: true}},
		{"signed out on a public date", DateReader{Ordinary: true, SeriesVisible: true, Recurring: true},
			DatePageFacts{Header: true, Attendance: true, Capacity: true, Series: true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := DatePage(tt.r); got != tt.want {
				t.Fatalf("DatePage(%+v) = %+v, want %+v", tt.r, got, tt.want)
			}
		})
	}
}
