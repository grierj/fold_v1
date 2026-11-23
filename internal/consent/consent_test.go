package consent

import (
	"bytes"
	"errors"
	"slices"
	"testing"
)

// sel is a test selection: its identities, copied deeply by Clone.
type sel struct{ ids []Identity }

func (s sel) ConsentIdentities() []Identity { return s.ids }
func (s sel) Clone() sel                    { return sel{ids: copyIdentities(s.ids)} }

func u(v uint64) *uint64 { return &v }

func ident(reg, occ uint64, user, by, hp *uint64) Identity {
	return Identity{Registration: reg, Occurrence: occ, User: user, RegisteredBy: by, HouseholdPerson: hp}
}

var key = []byte("0123456789abcdef0123456789abcdef")

func header() Header {
	return Header{Kind: KindEvent, Actor: 7, Event: 3, Target: "invite_only", Current: "public", Epoch: "e1", FutureOverrides: 1}
}

func entry(ids ...Identity) Entry[sel] {
	return Entry[sel]{Key: Key{List: ListTightened, Shape: ShapeEvent, Event: 3}, Row: RowDisplay{Title: "Supper", Visibility: "public", Registrants: uint64(len(ids))}, Sel: sel{ids: ids}, Cancels: true}
}

func mustToken(t *testing.T, h Header, es []Entry[sel]) string {
	t.Helper()
	tok, err := TokenFor(key, h, es)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestCheckPassesOnlyTheMatchingToken(t *testing.T) {
	es := []Entry[sel]{entry(ident(1, 9, u(4), u(4), nil))}
	tok := mustToken(t, header(), es)
	if !WellFormed(tok) {
		t.Fatalf("token %q is not 32 lowercase hex", tok)
	}
	plan, got, err := Check(key, tok, header(), es)
	if err != nil || plan == nil || got != tok {
		t.Fatalf("Check = %v, %q, %v", plan, got, err)
	}
	for _, bad := range []string{"", "garbage", tok[:31], tok + "0", "ABCDEF0123456789ABCDEF0123456789", "00000000000000000000000000000000"} {
		plan, got, err := Check(key, bad, header(), es)
		if !errors.Is(err, ErrMismatch) || plan != nil || got != tok {
			t.Errorf("Check(%q) = %v, %q, %v; want a mismatch carrying the fresh token", bad, plan, got, err)
		}
	}
	if _, _, err := Check([]byte("another key"), tok, header(), es); !errors.Is(err, ErrMismatch) {
		t.Errorf("another key accepted the token: %v", err)
	}
}

func TestReviewRequiredAndUnchecked(t *testing.T) {
	none := []Entry[sel]{entry()}
	if ReviewRequired(header(), none) {
		t.Error("a change that cancels nobody and is not a reset is review-required")
	}
	for _, h := range []Header{
		{Kind: KindDate, Target: "public", Current: "invite_only"},
		{Kind: KindGroup, Target: "invite_only", Current: "public"},
		{Kind: KindEvent, Target: "private", Current: "private"},
	} {
		if ReviewRequired[sel](h, nil) {
			t.Errorf("%+v with no identities is review-required", h)
		}
		plan, tok, err := Unchecked[sel](h, nil)
		if err != nil || plan == nil || tok != "" || len(plan.ResetDates()) != 0 {
			t.Errorf("Unchecked(%+v) = %v, %q, %v", h, plan, tok, err)
		}
	}
	notCancelled := entry(ident(1, 9, u(4), nil, nil))
	notCancelled.Cancels = false
	if ReviewRequired(header(), []Entry[sel]{notCancelled}) {
		t.Error("a kept row with identities made the change review-required")
	}
	if !ReviewRequired(header(), []Entry[sel]{entry(ident(1, 9, u(4), nil, nil))}) {
		t.Error("a cancellation is not review-required")
	}
	reset := header()
	reset.Reset = true
	if !ReviewRequired[sel](reset, nil) {
		t.Error("a reset with no dates is not review-required")
	}
	if _, _, err := Unchecked[sel](reset, nil); !errors.Is(err, ErrNeedsConsent) {
		t.Errorf("Unchecked passed a reset with no dates: %v", err)
	}
	reset.ResetDates = []Reset{{Occurrence: 5, Date: "2026-12-01", Title: "Supper", Visibility: "invite_only"}}
	if _, _, err := Unchecked[sel](reset, nil); !errors.Is(err, ErrNeedsConsent) {
		t.Errorf("Unchecked passed a reset with dates: %v", err)
	}
	if _, _, err := Unchecked(header(), []Entry[sel]{entry(ident(1, 9, u(4), nil, nil))}); !errors.Is(err, ErrNeedsConsent) {
		t.Errorf("Unchecked passed a cancellation: %v", err)
	}
	plan, _, err := Check(key, mustToken(t, reset, nil), reset, []Entry[sel]{})
	if err != nil || !slices.Equal(plan.ResetDates(), []uint64{5}) {
		t.Fatalf("checked reset = %v, %v", plan, err)
	}
	plain, _, _ := Unchecked(header(), none)
	if plain.CancelsAny() || len(plain.ResetDates()) != 0 {
		t.Error("an unchecked plan cancels or resets something")
	}
}

func TestPlanTakeHasDone(t *testing.T) {
	k1 := Key{List: ListTightened, Shape: ShapeEvent, Event: 3}
	k2 := Key{List: ListLeaderOpened, Shape: ShapeDate, Event: 3, Occurrence: 8}
	k3 := Key{List: ListLeaderOpened, Shape: ShapeEvent, Event: 4}
	es := []Entry[sel]{
		{Key: k1, Sel: sel{ids: []Identity{ident(1, 9, u(4), nil, nil)}}, Cancels: true},
		{Key: k2, Sel: sel{ids: []Identity{ident(2, 8, u(5), nil, nil)}}, Cancels: true},
		{Key: k3, Sel: sel{ids: []Identity{ident(3, 10, u(6), nil, nil)}}, Cancels: false},
	}
	plan, _, err := Check(key, mustToken(t, header(), es), header(), es)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.CancelsAny() || !plan.Has(k1) || !plan.Has(k2) || plan.Has(k3) || plan.Has(Key{Event: 99}) {
		t.Fatal("Has does not follow Cancels")
	}
	if _, err := plan.Take(k3); !errors.Is(err, ErrPlan) {
		t.Errorf("Take of a kept row: %v", err)
	}
	if _, err := plan.Take(k1); err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Take(k1); !errors.Is(err, ErrPlan) {
		t.Errorf("a key taken twice: %v", err)
	}
	if err := plan.Done(); !errors.Is(err, ErrPlan) {
		t.Errorf("Done with k2 never taken: %v", err)
	}
	if _, err := plan.Take(k2); err != nil {
		t.Fatal(err)
	}
	if err := plan.Done(); err != nil {
		t.Errorf("Done after every take: %v", err)
	}
	dup := []Entry[sel]{es[0], es[0]}
	if _, err := TokenFor(key, header(), dup); !errors.Is(err, ErrPlan) {
		t.Errorf("a duplicate key was encoded: %v", err)
	}
}

func TestVerifyBindsEveryIdentityField(t *testing.T) {
	base := []Identity{ident(1, 9, u(4), u(4), nil), ident(2, 9, nil, u(4), u(11))}
	es := []Entry[sel]{entry(base...)}
	plan, _, err := Check(key, mustToken(t, header(), es), header(), es)
	if err != nil {
		t.Fatal(err)
	}
	c, err := plan.Take(es[0].Key)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Verify(c.Selection().ConsentIdentities()); err != nil {
		t.Fatalf("the checked selection fails its own check: %v", err)
	}
	mutate := map[string]func([]Identity) []Identity{
		"added":   func(ids []Identity) []Identity { return append(ids, ident(3, 9, u(5), nil, nil)) },
		"removed": func(ids []Identity) []Identity { return ids[:1] },
		"swapped": func(ids []Identity) []Identity { ids[1] = ident(3, 9, nil, u(4), u(11)); return ids },
		"reordered": func(ids []Identity) []Identity {
			ids[0], ids[1] = ids[1], ids[0]
			return ids
		},
		"occurrence":      func(ids []Identity) []Identity { ids[0].Occurrence = 10; return ids },
		"user":            func(ids []Identity) []Identity { ids[0].User = u(5); return ids },
		"user to null":    func(ids []Identity) []Identity { ids[0].User = nil; return ids },
		"user from null":  func(ids []Identity) []Identity { ids[1].User = u(4); return ids },
		"registered_by":   func(ids []Identity) []Identity { ids[0].RegisteredBy = u(6); return ids },
		"by to null":      func(ids []Identity) []Identity { ids[0].RegisteredBy = nil; return ids },
		"household":       func(ids []Identity) []Identity { ids[1].HouseholdPerson = u(12); return ids },
		"household null":  func(ids []Identity) []Identity { ids[1].HouseholdPerson = nil; return ids },
		"household value": func(ids []Identity) []Identity { ids[0].HouseholdPerson = u(0); return ids },
	}
	for name, m := range mutate {
		got := m(copyIdentities(base))
		if err := c.Verify(got); !errors.Is(err, ErrVerify) {
			t.Errorf("%s: Verify = %v", name, err)
		}
	}
	var zero Checked[sel]
	if err := zero.Verify(nil); err != nil {
		t.Errorf("the zero Checked refuses the empty list: %v", err)
	}
	if err := zero.Verify(base[:1]); !errors.Is(err, ErrVerify) {
		t.Errorf("the zero Checked verifies a row: %v", err)
	}
	if len(zero.Selection().ids) != 0 {
		t.Error("the zero Checked hands out rows")
	}
}

func TestPlanKeepsItsOwnCopy(t *testing.T) {
	ids := []Identity{ident(1, 9, u(4), nil, nil)}
	es := []Entry[sel]{entry(ids...)}
	plan, _, err := Check(key, mustToken(t, header(), es), header(), es)
	if err != nil {
		t.Fatal(err)
	}
	// The caller edits its own selection after Check.
	*ids[0].User = 99
	es[0].Sel.ids[0].Registration = 42
	c, err := plan.Take(es[0].Key)
	if err != nil {
		t.Fatal(err)
	}
	first := c.Selection()
	first.ids[0].Occurrence = 77
	*first.ids[0].User = 55
	second := c.Selection()
	want := []Identity{ident(1, 9, u(4), nil, nil)}
	if err := c.Verify(want); err != nil {
		t.Errorf("Verify moved with the caller's edit: %v", err)
	}
	if !sameIdentity(second.ids[0], want[0]) {
		t.Errorf("Selection() moved with an edit of an earlier copy: %+v", second.ids[0])
	}
}

// TestMessageIsInjective pins the encoding's boundaries: integer widths,
// adjacent strings, list counts, tags, NULL against 0 and the reset rows.
func TestMessageIsInjective(t *testing.T) {
	row := func(title, vis string, ids ...Identity) Row {
		return Row{Key: Key{List: ListTightened, Shape: ShapeEvent, Event: 3}, Display: RowDisplay{Title: title, Visibility: vis}, Identities: ids}
	}
	h := header()
	pairs := map[string][2][]byte{}
	add := func(name string, a, b []byte) { pairs[name] = [2][]byte{a, b} }
	h1, h2 := h, h
	h1.Actor, h1.Event = 1, 23
	h2.Actor, h2.Event = 12, 3
	add("(1,23) vs (12,3)", encode(h1, nil), encode(h2, nil))
	add("adjacent strings", encode(h, []Row{row("ab", "c")}), encode(h, []Row{row("a", "bc")}))
	add("a NUL inside", encode(h, []Row{row("a\x00", "b")}), encode(h, []Row{row("a", "\x00b")}))
	ta, tb := h, h
	ta.Target, ta.Current = "public", "private"
	tb.Target, tb.Current = "private", "public"
	add("target and current swapped", encode(ta, nil), encode(tb, nil))
	tc := h
	tc.Target, tc.Current = "publicp", "rivate"
	td := h
	td.Target, td.Current = "public", "private"
	add("target and current concatenation", encode(tc, nil), encode(td, nil))
	two := []Row{row("a", "b", ident(1, 9, u(4), nil, nil)), row("a", "b", ident(2, 9, u(5), nil, nil))}
	one := []Row{row("a", "b", ident(1, 9, u(4), nil, nil), ident(2, 9, u(5), nil, nil))}
	add("one row [a,b] vs two rows", encode(h, one), encode(h, two))
	add("empty row vs none", encode(h, []Row{row("", "")}), encode(h, nil))
	listed := row("a", "b")
	listed.Key.List = ListLeaderOpened
	shaped := row("a", "b")
	shaped.Key.Shape = ShapeDate
	add("list vs shape", encode(h, []Row{listed}), encode(h, []Row{shaped}))
	add("NULL vs 0", encode(h, []Row{row("a", "b", ident(1, 9, nil, nil, nil))}), encode(h, []Row{row("a", "b", ident(1, 9, u(0), nil, nil))}))
	k1, k2 := h, h
	k2.Kind = KindDate
	add("scope kind", encode(k1, nil), encode(k2, nil))
	r1, r2 := h, h
	r1.Reset, r2.Reset = true, true
	r1.ResetDates = []Reset{{Occurrence: 5, Date: "2026-12-01", Title: "Supper", Visibility: "invite_only"}}
	r2.ResetDates = []Reset{{Occurrence: 6, Date: "2026-12-01", Title: "Supper", Visibility: "invite_only"}}
	add("a same-count reset swap", encode(r1, nil), encode(r2, nil))
	r3 := r1
	r3.ResetDates = []Reset{{Occurrence: 5, Date: "2026-12-01", Title: "Supper", Visibility: "private"}}
	add("a reset date's visibility", encode(r1, nil), encode(r3, nil))
	r4 := r1
	r4.ResetDates = append(slices.Clone(r1.ResetDates), Reset{Occurrence: 7, Date: "2026-12-08", Title: "Supper", Visibility: "private"})
	add("a reset date added", encode(r1, nil), encode(r4, nil))
	r5 := h
	r5.Reset = true
	add("the reset flag", encode(h, nil), encode(r5, nil))
	e1, e2 := h, h
	e2.Epoch = "e2"
	add("the epoch", encode(e1, nil), encode(e2, nil))
	f1, f2 := h, h
	f2.FutureOverrides = 2
	add("futureOverrides", encode(f1, nil), encode(f2, nil))
	for name, p := range pairs {
		if bytes.Equal(p[0], p[1]) {
			t.Errorf("%s: the two messages are equal", name)
		}
	}
	if !bytes.Equal(encode(h, one), encode(h, one)) {
		t.Error("the message is not stable")
	}
	shuffled := r4
	shuffled.ResetDates = []Reset{r4.ResetDates[1], r4.ResetDates[0]}
	if !bytes.Equal(encode(r4, nil), encode(shuffled, nil)) {
		t.Error("reset dates are not encoded in occurrence id order")
	}
}

func TestMatchesRefusesOtherShapes(t *testing.T) {
	want := Token(key, []byte("m"))
	for _, s := range []string{"", want[:31], want + "a", "g" + want[1:], "A" + want[1:], " " + want[1:]} {
		if Matches(s, want) {
			t.Errorf("Matches(%q) is true", s)
		}
	}
	if !Matches(want, want) {
		t.Error("Matches refuses the token itself")
	}
}
