package policy

import "slices"

// Registration statuses (PLAN 5.3, 5.4): the values of
// registrations_status_check. Cancelled is a person withdrawing a
// registration they made, which is not the answer "declined"; removed is a
// leader or host pruning it.
const (
	RegGoing      = "going"
	RegMaybe      = "maybe"
	RegDeclined   = "declined"
	RegCancelled  = "cancelled"
	RegWaitlisted = "waitlisted"
	RegRemoved    = "removed"
)

// RegNone is the from-status of a move that makes the row.
const RegNone = ""

// CurrentRegStatuses are the statuses that still stand: a seat held, a maybe,
// or a place on the waitlist. Declined, cancelled and removed rows are history.
// Read-only; pass it to a query as an IN list.
var CurrentRegStatuses = []string{RegGoing, RegMaybe, RegWaitlisted}

// RegistrationMove is one way a registration's status changes, named by what
// causes it. The table below is the whole machine (docs/agents/lifecycles.md):
// the root package's saveRegistration writes every move but promotion, which
// promoteRegistration writes. Where a move may end going or waitlisted, the
// seat ledger (registration_seats.go) picks between them.
type RegistrationMove string

const (
	// A person's own answer for a date, for themselves or a household person,
	// one date at a time or across a series (registration.changed).
	RegAnswer RegistrationMove = "answer"
	// The series undo puts back each date's answer from before the series
	// answer, or declines a row the series answer made before deleting it.
	RegUndoSeriesAnswer RegistrationMove = "undo a series answer"
	// A guest opens their emailed link; their seat is checked like an answer's.
	RegConfirmGuest RegistrationMove = "confirm a guest signup"
	// A guest's "I can't come" link (registration.cancelled).
	RegCancelFromLink RegistrationMove = "cancel from the manage link"
	// A freed seat or a leader's promote (registration.promoted).
	RegPromote RegistrationMove = "promote from the waitlist"
	// A leader, host or admin adds a person or a household person; it brings
	// back a removed or cancelled row (registration.added).
	RegAddByManager RegistrationMove = "add by a leader or host"
	// registration.removed by a leader, host or admin.
	RegRemoveByManager RegistrationMove = "remove by a leader or host"
	// A member leaves a group: their answers on its dates they do not keep.
	RegLeaveGroup RegistrationMove = "leave the group"
	// A leader removes a member from a group: the same, as removed.
	RegRemoveFromGroup RegistrationMove = "removed from the group"
	// A group, event or date became more private and the registrant can no
	// longer see it (registration.cancelled, reason event_more_private).
	RegMorePrivate RegistrationMove = "event made more private"
	// A household person was changed so they no longer qualify, or deleted.
	RegHouseholdPersonChanged RegistrationMove = "household person changed or deleted"
)

type registrationRule struct {
	// from includes RegNone where the move makes the row.
	from, to []string
	// keeps: the move may also leave the status as it is and write other
	// columns (a household person's childcare cleared).
	keeps bool
	// releasesSeat allows an interim going → declined write in the same
	// transaction before the final move.
	releasesSeat bool
}

var (
	allRegStatuses = []string{RegGoing, RegMaybe, RegDeclined, RegCancelled, RegWaitlisted, RegRemoved}
	answerOutcomes = []string{RegGoing, RegMaybe, RegDeclined, RegWaitlisted}
)

// Recorded as built, odd moves included: a manager's add brings back a removed
// person, a series undo restores any earlier answer (cancelled too), a guest's
// confirm turns a waiter into going when a seat is free.
var registrationRules = map[RegistrationMove]registrationRule{
	RegAnswer: {
		from: []string{RegNone, RegGoing, RegMaybe, RegDeclined, RegCancelled, RegWaitlisted},
		to:   answerOutcomes,
	},
	RegUndoSeriesAnswer: {
		from: answerOutcomes,
		to:   []string{RegGoing, RegMaybe, RegDeclined, RegCancelled, RegWaitlisted},
	},
	RegConfirmGuest: {
		from: []string{RegNone, RegGoing, RegMaybe, RegDeclined, RegCancelled, RegWaitlisted},
		to:   []string{RegGoing, RegWaitlisted},
	},
	RegCancelFromLink:  {from: CurrentRegStatuses, to: []string{RegCancelled}},
	RegPromote:         {from: []string{RegWaitlisted}, to: []string{RegGoing}},
	RegAddByManager:    {from: append([]string{RegNone}, allRegStatuses...), to: []string{RegGoing}},
	RegRemoveByManager: {from: []string{RegGoing, RegMaybe, RegDeclined, RegWaitlisted}, to: []string{RegRemoved}},
	RegLeaveGroup:      {from: CurrentRegStatuses, to: []string{RegCancelled}, releasesSeat: true},
	RegRemoveFromGroup: {from: CurrentRegStatuses, to: []string{RegRemoved}, releasesSeat: true},
	RegMorePrivate:     {from: CurrentRegStatuses, to: []string{RegCancelled}},
	RegHouseholdPersonChanged: {
		from:  CurrentRegStatuses,
		to:    []string{RegCancelled},
		keeps: true,
	},
}

func RegistrationMoves() []RegistrationMove {
	out := make([]RegistrationMove, 0, len(registrationRules))
	for m := range registrationRules {
		out = append(out, m)
	}
	slices.Sort(out)
	return out
}

// From is the statuses the move starts from (RegNone where it makes the row);
// none for a name the table does not hold.
func (m RegistrationMove) From() []string { return slices.Clone(registrationRules[m].from) }

func (m RegistrationMove) StartsFrom(status string) bool {
	return slices.Contains(registrationRules[m].from, status)
}

// Allows: the move may take a row in from (RegNone: no row) to status to as
// its last write.
func (m RegistrationMove) Allows(from, to string) bool {
	r := registrationRules[m]
	return slices.Contains(r.from, from) && to != RegNone &&
		(slices.Contains(r.to, to) || (r.keeps && to == from))
}

// AllowsStep: the move may write to before its last write (applyRegistrationBatch's
// seat release), which is anything it may end in or pass through.
func (m RegistrationMove) AllowsStep(from, to string) bool {
	return m.Allows(from, to) || (registrationRules[m].releasesSeat && from == RegGoing && to == RegDeclined)
}
