package policy

import (
	"reflect"
	"slices"
	"testing"
)

var recipientChannels = []string{RecipientChannelEmail, RecipientChannelPush, RecipientChannelSMS}

// TestRecipientMoves pins every (move, channel, from) triple: the status each
// move ends in from each status on each channel, or "" where it may not
// start. sms is in the loop so a move never reaches a channel nothing writes.
func TestRecipientMoves(t *testing.T) {
	const (
		q, su, de, se  = RecipientQueued, RecipientSuppressed, RecipientDeferred, RecipientSent
		dl, bo, co, fa = RecipientDelivered, RecipientBounced, RecipientComplained, RecipientFailed
	)
	email := func(m map[string]string) map[string]map[string]string {
		return map[string]map[string]string{RecipientChannelEmail: m}
	}
	push := func(m map[string]string) map[string]map[string]string {
		return map[string]map[string]string{RecipientChannelPush: m}
	}
	want := map[RecipientMove]map[string]map[string]string{
		RecipientSnapshotQueued:   {},
		RecipientSnapshotSkipped:  {},
		RecipientEmailSent:        email(map[string]string{q: se}),
		RecipientEmailSkipped:     email(map[string]string{q: su}),
		RecipientEmailFailed:      email(map[string]string{q: fa}),
		RecipientBatchGivenUp:     email(map[string]string{q: fa}),
		RecipientSendReaped:       email(map[string]string{q: fa}),
		RecipientReportDelivered:  email(map[string]string{se: dl}),
		RecipientReportBounced:    email(map[string]string{se: bo, dl: bo}),
		RecipientReportComplained: email(map[string]string{se: co, dl: co}),
		RecipientRetryResend:      email(map[string]string{fa: q}),
		RecipientRetryDropped:     email(map[string]string{fa: su}),
		RecipientPushNoDevice:     push(map[string]string{q: su}),
		RecipientPushSent:         push(map[string]string{q: se, su: se, de: se, dl: se, bo: se, co: se, fa: se}),
		RecipientPushSuppressed:   push(map[string]string{q: su}),
		RecipientPushFailed:       push(map[string]string{q: fa}),
		RecipientPushWaits:        push(map[string]string{q: q, su: q, fa: q}),
		RecipientPushReaped:       push(map[string]string{q: su}),
		RecipientEraseRecipient:   {RecipientChannelEmail: {q: su}, RecipientChannelPush: {q: su}, RecipientChannelSMS: {q: su}},
	}
	moves := RecipientMoves()
	if len(moves) != len(want) {
		t.Fatalf("the table has %d moves, the test %d: %v", len(moves), len(want), moves)
	}
	for _, m := range moves {
		cases, ok := want[m]
		if !ok {
			t.Errorf("%q has no row in this test", m)
			continue
		}
		for _, ch := range recipientChannels {
			for _, from := range recipientStatuses {
				got := ""
				if m.Allows(ch, from) {
					got = m.To()
				}
				if got != cases[ch][from] {
					t.Errorf("%q on %s from %s: got %q, want %q", m, ch, from, got, cases[ch][from])
				}
			}
		}
	}
}

func TestRecipientMoveChannels(t *testing.T) {
	both := []string{RecipientChannelEmail, RecipientChannelPush}
	for _, m := range RecipientMoves() {
		got := m.Channels()
		switch m {
		case RecipientSnapshotQueued, RecipientSnapshotSkipped:
			if !reflect.DeepEqual(got, both) {
				t.Errorf("%q moves %v, want both channels", m, got)
			}
		case RecipientEraseRecipient:
			if !reflect.DeepEqual(got, recipientChannels) {
				t.Errorf("%q moves %v, want every channel", m, got)
			}
		default:
			if len(got) != 1 {
				t.Errorf("%q moves %v, want one channel", m, got)
			}
		}
	}
}

func TestRecipientCreationMoves(t *testing.T) {
	for m, want := range map[RecipientMove]string{
		RecipientSnapshotQueued:  RecipientQueued,
		RecipientSnapshotSkipped: RecipientSuppressed,
	} {
		if len(m.From()) != 0 || m.To() != want {
			t.Errorf("%q: from %v to %q, want a new row in %q", m, m.From(), m.To(), want)
		}
	}
}

// Every status the table names is a recipient status, and every recipient
// status is named by some move; root's TestRecipientStatusesMatchTheSchema
// ties the list to the CHECK. No move ends in deferred.
func TestRecipientMovesNameEveryStatus(t *testing.T) {
	named := map[string]bool{}
	for _, m := range RecipientMoves() {
		if m.To() == RecipientDeferred {
			t.Errorf("%q ends in deferred, which no writer sets", m)
		}
		for _, s := range append(m.From(), m.To()) {
			if !slices.Contains(recipientStatuses, s) {
				t.Errorf("%q names %q, which is not a recipient status", m, s)
			}
			named[s] = true
		}
	}
	for _, s := range recipientStatuses {
		if !named[s] {
			t.Errorf("no move names %q", s)
		}
	}
}

func TestUnknownRecipientMoveAllowsNothing(t *testing.T) {
	m := RecipientMove("bounce twice")
	for _, ch := range recipientChannels {
		for _, s := range recipientStatuses {
			if m.Allows(ch, s) {
				t.Errorf("an unknown move starts from %s on %s", s, ch)
			}
		}
	}
	if m.To() != "" || len(m.From()) != 0 || len(m.Channels()) != 0 {
		t.Errorf("an unknown move has from %v, channels %v and to %q", m.From(), m.Channels(), m.To())
	}
}

func TestRecipientFromIsACopy(t *testing.T) {
	from := RecipientReportBounced.From()
	from[0] = RecipientFailed
	channels := RecipientReportBounced.Channels()
	channels[0] = RecipientChannelPush
	if !reflect.DeepEqual(RecipientReportBounced.From(), []string{RecipientSent, RecipientDelivered}) ||
		!reflect.DeepEqual(RecipientReportBounced.Channels(), []string{RecipientChannelEmail}) {
		t.Fatal("changing a result changed the table")
	}
}

// The push recorder's outcomes are exactly sent, suppressed and failed, each
// recorded by the push move that ends in it.
func TestRecipientPushOutcome(t *testing.T) {
	for _, s := range recipientStatuses {
		m, ok := RecipientPushOutcome(s)
		want := map[string]RecipientMove{RecipientSent: RecipientPushSent, RecipientSuppressed: RecipientPushSuppressed, RecipientFailed: RecipientPushFailed}[s]
		if m != want || ok != (want != "") {
			t.Errorf("RecipientPushOutcome(%s) = %q, %v; want %q", s, m, ok, want)
		}
		if ok && (m.To() != s || !slices.Equal(m.Channels(), []string{RecipientChannelPush})) {
			t.Errorf("%q records %s as %s on %v", m, s, m.To(), m.Channels())
		}
	}
}

// The reached statuses are exactly the list the read page used.
func TestRecipientReachedStatuses(t *testing.T) {
	want := []string{RecipientSent, RecipientDelivered, RecipientBounced, RecipientComplained}
	if got := RecipientReachedStatuses(); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
