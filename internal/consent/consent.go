// Package consent is the exact visibility consent rule (DEC-158): the one
// encoder of a reviewed impact, its keyed token, and the checked plan that is
// the only thing a visibility cancellation or a date reset may consume.
//
// It is pure, like internal/events and internal/policy: no database and no
// clock. A separate package gives the plan unexported fields that package
// main cannot fill in, so a cancellation can only use a selection that a
// passing Check (or, when nothing needs a review, Unchecked) handed out.
package consent

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
)

// Scope kinds, impact lists and row shapes, as the message tags them.
const (
	KindDate  uint8 = 1
	KindEvent uint8 = 2
	KindGroup uint8 = 3

	ListTightened    uint8 = 1
	ListLeaderOpened uint8 = 2

	ShapeEvent uint8 = 1
	ShapeDate  uint8 = 2
)

var (
	// ErrMismatch: the given token is not the one this change's impact makes.
	ErrMismatch = errors.New("consent: the reviewed change no longer matches")
	// ErrNeedsConsent: Unchecked was asked to pass a review-required change.
	ErrNeedsConsent = errors.New("consent: this change needs a reviewed consent")
	// ErrPlan: the plan was used outside its rules (a key taken twice, a key
	// with nothing to cancel, an entry never taken, a duplicate key).
	ErrPlan = errors.New("consent: the checked plan was not followed")
	// ErrVerify: the rows a canceller is about to cancel are not the checked
	// ones.
	ErrVerify = errors.New("consent: the rows to cancel are not the checked rows")
)

// Identity is one registration as the token binds it. Nil and a pointer to 0
// differ.
type Identity struct {
	Registration, Occurrence            uint64
	User, RegisteredBy, HouseholdPerson *uint64
}

// Selection is what a plan can hold.
type Selection[S any] interface {
	// ConsentIdentities lists the selection's rows in registration id order.
	ConsentIdentities() []Identity
	// Clone is a deep copy: no slice, map or pointer shared with the receiver.
	Clone() S
}

// Key names one impact row.
type Key struct {
	List, Shape       uint8
	Event, Occurrence uint64
}

// RowDisplay is what the reviewer sees of one impact row besides its key.
type RowDisplay struct {
	Date, DecisionKey, Title, Visibility string
	Registrants                          uint64
}

// Reset is one date a reset changes, as the review shows it.
type Reset struct {
	Occurrence              uint64
	Date, Title, Visibility string
}

// Header is the scope of one change. Current is the scope's own stored
// visibility before the change. Epoch is filled by the caller that reads it.
type Header struct {
	Kind                            uint8
	Actor, Event, Occurrence, Group uint64
	Target, Current                 string
	Reset                           bool
	FutureOverrides                 uint64
	ResetDates                      []Reset
	Epoch                           string
}

// Entry is one impact row with the selection it would cancel. Cancels says
// the decided change cancels this row's selection.
type Entry[S Selection[S]] struct {
	Key     Key
	Row     RowDisplay
	Sel     S
	Cancels bool
}

// ReviewRequired: some Cancels entry has identities, or the request is a
// reset (even one that resets nothing).
func ReviewRequired[S Selection[S]](h Header, entries []Entry[S]) bool {
	if h.Reset {
		return true
	}
	for _, e := range entries {
		if e.Cancels && len(e.Sel.ConsentIdentities()) > 0 {
			return true
		}
	}
	return false
}

type checkedEntry[S Selection[S]] struct {
	entry Entry[S]
	ids   []Identity
	taken bool
}

// Plan is a checked (or unchecked, when nothing needed a review) set of
// entries. Its fields are unexported, so only this package makes one.
type Plan[S Selection[S]] struct {
	entries []checkedEntry[S]
	reset   []uint64
}

// Check clones each entry's selection, reads the identities from that clone,
// encodes the header and the entries, and compares the MAC with given. The
// returned string is the token the comparison used, so a mismatch answers
// with it.
func Check[S Selection[S]](key []byte, given string, h Header, entries []Entry[S]) (*Plan[S], string, error) {
	plan, rows, err := build(h, entries)
	if err != nil {
		return nil, "", err
	}
	token := Token(key, encode(h, rows))
	if !Matches(given, token) {
		return nil, token, ErrMismatch
	}
	for _, r := range h.ResetDates {
		plan.reset = append(plan.reset, r.Occurrence)
	}
	slices.Sort(plan.reset)
	return plan, token, nil
}

// Unchecked returns a plan only when the change is not review-required: it
// cancels nobody and is not a reset. Its ResetDates is always empty.
func Unchecked[S Selection[S]](h Header, entries []Entry[S]) (*Plan[S], string, error) {
	if ReviewRequired(h, entries) {
		return nil, "", ErrNeedsConsent
	}
	plan, _, err := build(h, entries)
	if err != nil {
		return nil, "", err
	}
	return plan, "", nil
}

// TokenFor is the token of a change, for a caller that offers one (a preview,
// a decision answer) and checks none.
func TokenFor[S Selection[S]](key []byte, h Header, entries []Entry[S]) (string, error) {
	_, rows, err := build(h, entries)
	if err != nil {
		return "", err
	}
	return Token(key, encode(h, rows)), nil
}

// Row is one encoded impact row: its key, what it shows and its identities.
type Row struct {
	Key        Key
	Display    RowDisplay
	Identities []Identity
}

func build[S Selection[S]](h Header, entries []Entry[S]) (*Plan[S], []Row, error) {
	plan := &Plan[S]{entries: make([]checkedEntry[S], 0, len(entries))}
	rows := make([]Row, 0, len(entries))
	seen := map[Key]bool{}
	for _, e := range entries {
		if seen[e.Key] {
			return nil, nil, fmt.Errorf("%w: duplicate key %+v", ErrPlan, e.Key)
		}
		seen[e.Key] = true
		sel := e.Sel.Clone()
		ids := copyIdentities(sel.ConsentIdentities())
		copied := e
		copied.Sel = sel
		plan.entries = append(plan.entries, checkedEntry[S]{entry: copied, ids: ids})
		rows = append(rows, Row{Key: e.Key, Display: e.Row, Identities: ids})
	}
	return plan, rows, nil
}

// CancelsAny is whether some Cancels entry has identities.
func (p *Plan[S]) CancelsAny() bool {
	for _, e := range p.entries {
		if e.entry.Cancels && len(e.ids) > 0 {
			return true
		}
	}
	return false
}

// ResetDates is the checked reset dates' occurrence ids, ascending. It is
// empty unless Check passed.
func (p *Plan[S]) ResetDates() []uint64 { return slices.Clone(p.reset) }

func (p *Plan[S]) find(k Key) *checkedEntry[S] {
	for i := range p.entries {
		if p.entries[i].entry.Key == k {
			return &p.entries[i]
		}
	}
	return nil
}

// Has is whether the plan holds an entry with Cancels for k.
func (p *Plan[S]) Has(k Key) bool {
	e := p.find(k)
	return e != nil && e.entry.Cancels
}

// Take hands out k's checked selection, once, and only for an entry the
// decided change cancels.
func (p *Plan[S]) Take(k Key) (Checked[S], error) {
	e := p.find(k)
	if e == nil || !e.entry.Cancels {
		return Checked[S]{}, fmt.Errorf("%w: nothing to cancel for %+v", ErrPlan, k)
	}
	if e.taken {
		return Checked[S]{}, fmt.Errorf("%w: %+v taken twice", ErrPlan, k)
	}
	e.taken = true
	return Checked[S]{sel: e.entry.Sel.Clone(), ids: copyIdentities(e.ids), ok: true}, nil
}

// Done is nil when every Cancels entry was taken.
func (p *Plan[S]) Done() error {
	for _, e := range p.entries {
		if e.entry.Cancels && !e.taken {
			return fmt.Errorf("%w: %+v was checked and never cancelled", ErrPlan, e.entry.Key)
		}
	}
	return nil
}

// Checked is one taken entry. The zero Checked holds no selection and
// verifies only the empty list, so it cancels nothing wherever it is passed.
type Checked[S Selection[S]] struct {
	sel S
	ids []Identity
	ok  bool
}

// Selection is a fresh copy of the checked selection on every call.
func (c Checked[S]) Selection() S {
	if !c.ok {
		var zero S
		return zero
	}
	return c.sel.Clone()
}

// Verify is nil when got is exactly the checked identities: the same length,
// order and every field (nil and a value differ).
func (c Checked[S]) Verify(got []Identity) error {
	if len(got) != len(c.ids) {
		return fmt.Errorf("%w: %d rows, %d checked", ErrVerify, len(got), len(c.ids))
	}
	for i := range got {
		if !sameIdentity(got[i], c.ids[i]) {
			return fmt.Errorf("%w: row %d differs", ErrVerify, i)
		}
	}
	return nil
}

func sameIdentity(a, b Identity) bool {
	return a.Registration == b.Registration && a.Occurrence == b.Occurrence &&
		sameOpt(a.User, b.User) && sameOpt(a.RegisteredBy, b.RegisteredBy) && sameOpt(a.HouseholdPerson, b.HouseholdPerson)
}

func sameOpt(a, b *uint64) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

func copyIdentities(ids []Identity) []Identity {
	out := make([]Identity, len(ids))
	for i, id := range ids {
		out[i] = Identity{Registration: id.Registration, Occurrence: id.Occurrence,
			User: copyOpt(id.User), RegisteredBy: copyOpt(id.RegisteredBy), HouseholdPerson: copyOpt(id.HouseholdPerson)}
	}
	return out
}

func copyOpt(p *uint64) *uint64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// encode is the canonical message of a change and its rows: fixed-width
// big-endian integers, length-prefixed strings, counted lists and tagged
// variants, so two different reviews never share bytes (TestMessageIsInjective).
func encode(h Header, rows []Row) []byte {
	var b []byte
	u8 := func(v uint8) { b = append(b, v) }
	u32 := func(v uint32) { b = binary.BigEndian.AppendUint32(b, v) }
	u64 := func(v uint64) { b = binary.BigEndian.AppendUint64(b, v) }
	str := func(s string) { u32(uint32(len(s))); b = append(b, s...) }
	opt := func(p *uint64) {
		if p == nil {
			u8(0)
			return
		}
		u8(1)
		u64(*p)
	}
	b = append(b, "fold.visibility-consent.v1"...)
	u8(0)
	u8(h.Kind)
	u64(h.Actor)
	u64(h.Event)
	u64(h.Occurrence)
	u64(h.Group)
	str(h.Target)
	str(h.Current)
	if h.Reset {
		u8(1)
	} else {
		u8(0)
	}
	str(h.Epoch)
	u64(h.FutureOverrides)
	resets := slices.Clone(h.ResetDates)
	if !h.Reset {
		resets = nil
	}
	slices.SortFunc(resets, func(x, y Reset) int {
		switch {
		case x.Occurrence < y.Occurrence:
			return -1
		case x.Occurrence > y.Occurrence:
			return 1
		}
		return 0
	})
	u32(uint32(len(resets)))
	for _, r := range resets {
		u64(r.Occurrence)
		str(r.Date)
		str(r.Title)
		str(r.Visibility)
	}
	u32(uint32(len(rows)))
	for _, r := range rows {
		u8(r.Key.List)
		u8(r.Key.Shape)
		u64(r.Key.Event)
		u64(r.Key.Occurrence)
		str(r.Display.Date)
		str(r.Display.DecisionKey)
		str(r.Display.Title)
		str(r.Display.Visibility)
		u64(r.Display.Registrants)
		u32(uint32(len(r.Identities)))
		for _, id := range r.Identities {
			u64(id.Registration)
			u64(id.Occurrence)
			opt(id.User)
			opt(id.RegisteredBy)
			opt(id.HouseholdPerson)
		}
	}
	return b
}

// Token is HMAC-SHA256(key, message), its first 16 bytes as 32 lowercase hex.
func Token(key, message []byte) string {
	m := hmac.New(sha256.New, key)
	m.Write(message)
	return hex.EncodeToString(m.Sum(nil)[:16])
}

// WellFormed is whether s has a token's shape: 32 lowercase hex characters.
func WellFormed(s string) bool {
	if len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Matches refuses any shape other than a token's, then compares in constant
// time.
func Matches(given, want string) bool {
	return WellFormed(given) && WellFormed(want) && hmac.Equal([]byte(given), []byte(want))
}
