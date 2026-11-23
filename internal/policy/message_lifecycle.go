package policy

import "slices"

// Message statuses (PLAN 5.3): the values of messages_status_check.
const (
	MessageDraft     = "draft"
	MessageScheduled = "scheduled"
	MessageSending   = "sending"
	MessageSent      = "sent"
	MessageCancelled = "cancelled"
	MessageFailed    = "failed"
)

// MessageMove is one way a message's status changes, named by what causes
// it. The table below is the whole machine (docs/agents/lifecycles.md); the
// root package's moveMessage writes every move of an existing row, and a
// move that makes a row is the status its creator inserts.
type MessageMove string

const (
	MessageCreateDraft        MessageMove = "create a draft"
	MessageWriteEmailReminder MessageMove = "write an email reminder"
	MessageWritePushReminder  MessageMove = "write a push reminder"
	MessageEdit               MessageMove = "edit"
	MessageSchedule           MessageMove = "schedule"
	MessageCancel             MessageMove = "cancel"
	MessageEraseAuthor        MessageMove = "erase the author"
	MessageClaim              MessageMove = "claim for sending"
	MessageRefuse             MessageMove = "refuse"
	MessageSettleSent         MessageMove = "settle as sent"
	MessageSettleFailed       MessageMove = "settle as failed"
	MessageRetryScheduled     MessageMove = "retry as scheduled"
	MessageRetrySending       MessageMove = "retry as sending"
)

type messageRule struct {
	from []string
	// to is "" for a move that keeps the status it starts from.
	to string
}

// Recorded as built, odd moves included: a push reminder is created sent
// (its rows never settle it), and a retry reopens a settled message. A
// retry that schedules again is the one for a message that failed before
// anyone was chosen; one with failed rows to resend starts sending.
var messageRules = map[MessageMove]messageRule{
	MessageCreateDraft:        {to: MessageDraft},
	MessageWriteEmailReminder: {to: MessageScheduled},
	MessageWritePushReminder:  {to: MessageSent},
	MessageEdit:               {from: []string{MessageDraft, MessageScheduled}},
	MessageSchedule:           {from: []string{MessageDraft, MessageScheduled}, to: MessageScheduled},
	MessageCancel:             {from: []string{MessageDraft, MessageScheduled}, to: MessageCancelled},
	MessageEraseAuthor:        {from: []string{MessageScheduled}, to: MessageCancelled},
	MessageClaim:              {from: []string{MessageScheduled}, to: MessageSending},
	MessageRefuse:             {from: []string{MessageScheduled}, to: MessageFailed},
	MessageSettleSent:         {from: []string{MessageSending}, to: MessageSent},
	MessageSettleFailed:       {from: []string{MessageSending}, to: MessageFailed},
	MessageRetryScheduled:     {from: []string{MessageFailed}, to: MessageScheduled},
	MessageRetrySending:       {from: []string{MessageSent, MessageFailed}, to: MessageSending},
}

func MessageMoves() []MessageMove {
	out := make([]MessageMove, 0, len(messageRules))
	for m := range messageRules {
		out = append(out, m)
	}
	slices.Sort(out)
	return out
}

// From is the statuses the move starts from; none for a move that makes the
// row, or for a name the table does not hold.
func (m MessageMove) From() []string { return slices.Clone(messageRules[m].from) }

// To is the status the move ends in, or "" when it keeps the one it starts
// from (an edit).
func (m MessageMove) To() string { return messageRules[m].to }

func (m MessageMove) Allows(status string) bool { return slices.Contains(messageRules[m].from, status) }

// MessageEditable: the message's author or an admin may still change it
// (CanEditMessage decides who).
func MessageEditable(status string) bool { return MessageEdit.Allows(status) }

// MessageGoingOut: it is sending, and only a settle moves it now.
func MessageGoingOut(status string) bool { return MessageSettleSent.Allows(status) }
