package policy

import (
	"reflect"
	"slices"
	"testing"
)

var messageStatuses = []string{MessageDraft, MessageScheduled, MessageSending, MessageSent, MessageCancelled, MessageFailed}

// TestMessageMoves pins every (move, from) pair: the status each move ends
// in from each status, or "" where it may not start.
func TestMessageMoves(t *testing.T) {
	const no = ""
	want := map[MessageMove]map[string]string{
		MessageCreateDraft:        {},
		MessageWriteEmailReminder: {},
		MessageWritePushReminder:  {},
		MessageEdit:               {MessageDraft: MessageDraft, MessageScheduled: MessageScheduled},
		MessageSchedule:           {MessageDraft: MessageScheduled, MessageScheduled: MessageScheduled},
		MessageCancel:             {MessageDraft: MessageCancelled, MessageScheduled: MessageCancelled},
		MessageEraseAuthor:        {MessageScheduled: MessageCancelled},
		MessageClaim:              {MessageScheduled: MessageSending},
		MessageRefuse:             {MessageScheduled: MessageFailed},
		MessageSettleSent:         {MessageSending: MessageSent},
		MessageSettleFailed:       {MessageSending: MessageFailed},
		MessageRetryScheduled:     {MessageFailed: MessageScheduled},
		MessageRetrySending:       {MessageSent: MessageSending, MessageFailed: MessageSending},
	}
	moves := MessageMoves()
	if len(moves) != len(want) {
		t.Fatalf("the table has %d moves, the test %d: %v", len(moves), len(want), moves)
	}
	for _, m := range moves {
		cases, ok := want[m]
		if !ok {
			t.Errorf("%q has no row in this test", m)
			continue
		}
		for _, from := range messageStatuses {
			got := no
			if m.Allows(from) {
				got = m.To()
				if got == "" {
					got = from
				}
			}
			if got != cases[from] {
				t.Errorf("%q from %s: got %q, want %q", m, from, got, cases[from])
			}
		}
	}
}

func TestMessageCreationMoves(t *testing.T) {
	for m, want := range map[MessageMove]string{
		MessageCreateDraft:        MessageDraft,
		MessageWriteEmailReminder: MessageScheduled,
		MessageWritePushReminder:  MessageSent,
	} {
		if len(m.From()) != 0 || m.To() != want {
			t.Errorf("%q: from %v to %q, want a new row in %q", m, m.From(), m.To(), want)
		}
	}
}

// Every status the table names is a message status, and every message
// status is named by some move; root's TestMessageStatusesMatchTheSchema
// ties the list to the CHECK.
func TestMessageMovesNameEveryStatus(t *testing.T) {
	named := map[string]bool{}
	for _, m := range MessageMoves() {
		for _, s := range append(m.From(), m.To()) {
			if s == "" {
				continue
			}
			if !slices.Contains(messageStatuses, s) {
				t.Errorf("%q names %q, which is not a message status", m, s)
			}
			named[s] = true
		}
	}
	for _, s := range messageStatuses {
		if !named[s] {
			t.Errorf("no move names %q", s)
		}
	}
}

func TestUnknownMessageMoveAllowsNothing(t *testing.T) {
	m := MessageMove("publish")
	for _, s := range messageStatuses {
		if m.Allows(s) {
			t.Errorf("an unknown move starts from %s", s)
		}
	}
	if m.To() != "" || len(m.From()) != 0 {
		t.Errorf("an unknown move has from %v and to %q", m.From(), m.To())
	}
}

func TestMessageFromIsACopy(t *testing.T) {
	from := MessageCancel.From()
	from[0] = MessageSent
	if !reflect.DeepEqual(MessageCancel.From(), []string{MessageDraft, MessageScheduled}) {
		t.Fatal("changing From's result changed the table")
	}
}

// The predicates are exactly the status lists they replaced in the root
// package.
func TestMessageStatusPredicates(t *testing.T) {
	for _, s := range messageStatuses {
		if got, want := MessageEditable(s), s == MessageDraft || s == MessageScheduled; got != want {
			t.Errorf("MessageEditable(%s) = %v, want %v", s, got, want)
		}
		if got, want := MessageGoingOut(s), s == MessageSending; got != want {
			t.Errorf("MessageGoingOut(%s) = %v, want %v", s, got, want)
		}
	}
}
