package policy

import "slices"

// Message recipient statuses (PLAN 5.3): the values of
// message_recipients_status_check. Deferred is a value the schema allows and
// no move reaches.
const (
	RecipientQueued     = "queued"
	RecipientSuppressed = "suppressed"
	RecipientDeferred   = "deferred"
	RecipientSent       = "sent"
	RecipientDelivered  = "delivered"
	RecipientBounced    = "bounced"
	RecipientComplained = "complained"
	RecipientFailed     = "failed"
)

var recipientStatuses = []string{RecipientQueued, RecipientSuppressed, RecipientDeferred, RecipientSent,
	RecipientDelivered, RecipientBounced, RecipientComplained, RecipientFailed}

// The channels of the column's CHECK. Nothing writes an sms row (SMS is not
// built); only the erasure, which names no channel, would move one.
const (
	RecipientChannelEmail = "email"
	RecipientChannelPush  = "push"
	RecipientChannelSMS   = "sms"
)

// RecipientMove is one way a message recipient row's status changes, named by
// what causes it. Email and push share the column, each with its own machine;
// a move names the channels it moves. The table below is the whole machine
// (docs/agents/lifecycles.md); the root package's moveRecipient writes every
// move of an existing row, and a move that makes a row is the status its
// creator inserts.
type RecipientMove string

const (
	RecipientSnapshotQueued   RecipientMove = "snapshot a recipient"
	RecipientSnapshotSkipped  RecipientMove = "snapshot a skip"
	RecipientEmailSent        RecipientMove = "send the email"
	RecipientEmailSkipped     RecipientMove = "skip at the send"
	RecipientEmailFailed      RecipientMove = "fail at the send"
	RecipientBatchGivenUp     RecipientMove = "give up on the batch"
	RecipientSendReaped       RecipientMove = "reap a stopped send"
	RecipientReportDelivered  RecipientMove = "report delivered"
	RecipientReportBounced    RecipientMove = "report bounced"
	RecipientReportComplained RecipientMove = "report complained"
	RecipientRetryResend      RecipientMove = "retry the email"
	RecipientRetryDropped     RecipientMove = "drop at the retry"
	RecipientPushNoDevice     RecipientMove = "find no device"
	RecipientPushSent         RecipientMove = "push sent"
	RecipientPushSuppressed   RecipientMove = "push suppressed"
	RecipientPushFailed       RecipientMove = "push failed"
	RecipientPushWaits        RecipientMove = "wait for quiet hours"
	RecipientPushReaped       RecipientMove = "reap a stopped push"
	RecipientEraseRecipient   RecipientMove = "erase the recipient"
)

type recipientRule struct {
	channels []string
	from     []string
	to       string
}

var (
	emailOnly = []string{RecipientChannelEmail}
	pushOnly  = []string{RecipientChannelPush}
	bothKinds = []string{RecipientChannelEmail, RecipientChannelPush}
	everyKind = []string{RecipientChannelEmail, RecipientChannelPush, RecipientChannelSMS}
)

// Recorded as built, odd moves included: a provider's bounce or complaint
// still lands after a delivered report; a push that went replaces any other
// outcome (another device's miss, or a status no push row holds); a push row
// that missed on one device goes back to queued when another device's job
// waits for quiet hours, which the caller allows only for a device miss of a
// person still active; and erasing a person suppresses their queued rows on
// every channel, since its statement names none.
var recipientRules = map[RecipientMove]recipientRule{
	RecipientSnapshotQueued:   {channels: bothKinds, to: RecipientQueued},
	RecipientSnapshotSkipped:  {channels: bothKinds, to: RecipientSuppressed},
	RecipientEmailSent:        {channels: emailOnly, from: []string{RecipientQueued}, to: RecipientSent},
	RecipientEmailSkipped:     {channels: emailOnly, from: []string{RecipientQueued}, to: RecipientSuppressed},
	RecipientEmailFailed:      {channels: emailOnly, from: []string{RecipientQueued}, to: RecipientFailed},
	RecipientBatchGivenUp:     {channels: emailOnly, from: []string{RecipientQueued}, to: RecipientFailed},
	RecipientSendReaped:       {channels: emailOnly, from: []string{RecipientQueued}, to: RecipientFailed},
	RecipientReportDelivered:  {channels: emailOnly, from: []string{RecipientSent}, to: RecipientDelivered},
	RecipientReportBounced:    {channels: emailOnly, from: []string{RecipientSent, RecipientDelivered}, to: RecipientBounced},
	RecipientReportComplained: {channels: emailOnly, from: []string{RecipientSent, RecipientDelivered}, to: RecipientComplained},
	RecipientRetryResend:      {channels: emailOnly, from: []string{RecipientFailed}, to: RecipientQueued},
	RecipientRetryDropped:     {channels: emailOnly, from: []string{RecipientFailed}, to: RecipientSuppressed},
	RecipientPushNoDevice:     {channels: pushOnly, from: []string{RecipientQueued}, to: RecipientSuppressed},
	RecipientPushSent:         {channels: pushOnly, from: allRecipientStatusesBut(RecipientSent), to: RecipientSent},
	RecipientPushSuppressed:   {channels: pushOnly, from: []string{RecipientQueued}, to: RecipientSuppressed},
	RecipientPushFailed:       {channels: pushOnly, from: []string{RecipientQueued}, to: RecipientFailed},
	RecipientPushWaits:        {channels: pushOnly, from: []string{RecipientQueued, RecipientSuppressed, RecipientFailed}, to: RecipientQueued},
	RecipientPushReaped:       {channels: pushOnly, from: []string{RecipientQueued}, to: RecipientSuppressed},
	RecipientEraseRecipient:   {channels: everyKind, from: []string{RecipientQueued}, to: RecipientSuppressed},
}

func allRecipientStatusesBut(s string) []string {
	out := []string{}
	for _, v := range recipientStatuses {
		if v != s {
			out = append(out, v)
		}
	}
	return out
}

func RecipientMoves() []RecipientMove {
	out := make([]RecipientMove, 0, len(recipientRules))
	for m := range recipientRules {
		out = append(out, m)
	}
	slices.Sort(out)
	return out
}

// From is the statuses the move starts from; none for a move that makes the
// row, or for a name the table does not hold.
func (m RecipientMove) From() []string { return slices.Clone(recipientRules[m].from) }

// Channels is the channels whose rows the move moves (or makes).
func (m RecipientMove) Channels() []string { return slices.Clone(recipientRules[m].channels) }

func (m RecipientMove) To() string { return recipientRules[m].to }

func (m RecipientMove) Allows(channel, status string) bool {
	r := recipientRules[m]
	return slices.Contains(r.channels, channel) && slices.Contains(r.from, status)
}

// RecipientPushOutcome is the move that records a push's outcome on its row.
// The push path shared with notification deliveries speaks that table's
// statuses, whose spellings these targets share.
func RecipientPushOutcome(status string) (RecipientMove, bool) {
	for _, m := range []RecipientMove{RecipientPushSent, RecipientPushSuppressed, RecipientPushFailed} {
		if m.To() == status {
			return m, true
		}
	}
	return "", false
}

// RecipientReachedStatuses are the outcomes of a row whose message went out:
// sent, or what a provider reported after it.
func RecipientReachedStatuses() []string {
	return []string{RecipientEmailSent.To(), RecipientReportDelivered.To(), RecipientReportBounced.To(), RecipientReportComplained.To()}
}
