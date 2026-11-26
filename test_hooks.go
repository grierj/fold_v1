package main

import (
	"context"
	"sync/atomic"
	"time"

	"fold/internal/consent"
	"fold/internal/push"
	"fold/models"

	"gorm.io/gorm"
)

// testHooks are the App's test seams, all nil in production (T07B-1b): a nil
// seam sends no statement and takes no time. They are one struct so production
// code names them the same way (a.hooks.x) and nothing else on App is a test
// control. The time seam is App.clock.
type testHooks struct {
	// restoreAckPause runs inside acknowledgeRestoreTicket before it raises the
	// floor. settleBeforeTx runs before settle's first transaction;
	// settleBeforeFence and settleAfterFence run inside its seal, around the
	// ledger transition, given the open transaction. deleteBeforeSeal runs in
	// the deletion's seal just before currentAdminIn, given the open
	// transaction; deleteBeforeRetry runs between a busy attempt's rollback and
	// the next attempt, with no transaction open. conversionAfterUserLocks runs
	// in acceptHouseholdConversion right after its users statement.
	// readBeforeSeal runs in a reason-review page transaction between its view
	// audit and the actor seal, given the open transaction (T07B-2b).
	// legacySaveBeforeSeal runs in the deleted-accounts save's seal just before
	// currentAdminIn, given the open transaction; legacySaveBeforeRetry runs
	// between a busy attempt's acknowledged rollback and the next attempt, with
	// no transaction open (T-301).
	// sealAttempts counts seal entries of the deletion, settle and the save.
	// wrapDeleteContext lets a test expire the deletion's budget at an observed phase.
	restoreAckPause          func()
	settleBeforeTx           func()
	settleBeforeFence        func(*gorm.DB)
	settleAfterFence         func(*gorm.DB)
	deleteBeforeSeal         func(*gorm.DB)
	deleteBeforeRetry        func()
	wrapDeleteContext        func(context.Context) context.Context
	readBeforeSeal           func(*gorm.DB)
	legacySaveBeforeSeal     func(*gorm.DB)
	legacySaveBeforeRetry    func()
	conversionAfterUserLocks func(*gorm.DB)
	sealAttempts             *atomic.Int64
	// messageLimitAfterCount runs inside the schedule, scheduled-edit and
	// Try again transactions, after a daily-limit count that did not refuse
	// and before the message write or claim, given the open transaction
	// (#260).
	messageLimitAfterCount func(*gorm.DB)
	// guestVerifyAfterCount runs inside the guest registration transaction
	// after a per-address count that did not refuse and before the signup
	// insert, given the open transaction (#263). It mirrors
	// messageLimitAfterCount (#260). It is nil in production and sends no
	// statement.
	guestVerifyAfterCount  func(*gorm.DB)
	wrapGuestPasswordCheck func(func(string) bool) func(string) bool
	// invitationBeforeHash runs in apiAcceptInvitation after the early
	// full-group refusal and before the password is hashed, with no
	// transaction open.
	invitationBeforeHash func()
	// cancelledDateStep runs inside the cancelled-date read's read-only
	// transaction, given the point reached: "begin" before its first
	// query (no snapshot yet), "date" right after that query fixed the
	// snapshot, and "snapshots" and "restores" before each evidence lookup.
	cancelledDateStep func(step string)
	// scheduledDateStep runs inside the scheduled date page's read-only
	// transaction: "begin" before its first query (no snapshot yet) and
	// "authorized" once the date is authorized and presented, before the
	// room and the page's own counts, names and coverage are read.
	scheduledDateStep func(step string)
	// managedDateStep runs inside managedDateTargetIn, on the read-only
	// snapshot of the manager's exact-date read (GET
	// /events/:id/occurrences/:date) and of the retired PUT: "authorized" once
	// the viewer's authority to manage the event is judged, before the date,
	// its roster and the church zone are read.
	managedDateStep func(step string)
	// groupReadBegin runs inside a group read's read-only snapshot (readGroup:
	// the group page, its dates, its roster and a role change's roster reply)
	// before its first query, so no snapshot is fixed yet and the session the
	// middleware accepted is not judged yet.
	groupReadBegin func()
	// groupReadEnd runs once a group read has built its whole answer, given
	// its transaction, before COMMIT is sent. A test ends the backend there to
	// prove a failed commit answers nothing of what was read.
	groupReadEnd func(tx *gorm.DB)
	// readerBegin runs inside a snapshot reader's transaction before its first
	// query, before it judges the session the middleware accepted.
	readerBegin func(surface string)
	// readerEnd runs once a snapshot reader has built its whole answer, given
	// its surface and transaction, before COMMIT is sent. A test ends the
	// backend there to check the response to a failed commit.
	readerEnd func(surface string, tx *gorm.DB)
	// departureStep runs inside a departure confirmation's transaction, given
	// that transaction: "granted" right after applyDeparture, while the gate,
	// the group, the users and every kept date stay locked and nothing is
	// committed. A test parks a departure there to race a raw date write
	// against the kept grant (00014).
	departureStep func(tx *gorm.DB, step string)
	// guardianCopyBegin runs inside a guardian copy's read-only snapshot
	// transaction before its first query, so no snapshot is fixed yet.
	guardianCopyBegin func()
	// dateEditStep runs inside the guarded date edit's transaction, given
	// the open transaction and the point reached: "selected" after the
	// losers of a tightening are selected, "effects" after every effect. A
	// returned error rolls the edit back.
	dateEditStep func(tx *gorm.DB, step string) error
	// singleDateAttempts counts the attempts of a single-date edit
	// (singleDateMutation), guarded and legacy: two means the date's end
	// passed during the first and the edit ran again. singleDateBeforeRetry
	// runs between the first attempt's acknowledged rollback and the rerun,
	// with no transaction open, only when a rerun follows.
	singleDateAttempts    *atomic.Int64
	singleDateBeforeRetry func()
	// invitationReadStep runs in scopedOccurrenceReadsIn at "grants", after
	// the viewer's live grant rows are read and before the dates, events and
	// accounts that judge them, on whatever handle the reader holds. A test
	// commits an edit there to prove a reader sees one committed state.
	invitationReadStep func(step string)
	// dateFactsRead runs in loadDateFacts once its one statement has been read
	// and closed, before anything is decided from it. A test commits an edit
	// there to prove the decision and the fields come from that statement
	// alone.
	dateFactsRead func()
	// retryLocked runs in Try again's transaction once the presser and the
	// message are locked (and an announcement's daily count is taken), before
	// anything else is read.
	retryLocked func(*gorm.DB)
	// batchRowClaimed runs in a batch email row's transaction right after its
	// claim (FOR UPDATE SKIP LOCKED), on that transaction, before the choice is
	// read.
	batchRowClaimed func(tx *gorm.DB, rowID uint)
	// batchChoiceRead runs in a batch email row's transaction once its email
	// choice is read, before its facts statement.
	batchChoiceRead func(rowID uint)
	// readerAuthorized runs right after a snapshot reader judges what
	// authorizes it, wherever that judgment is made: "room feed" and
	// "personal feed" once the link, its room or owner and scope are read,
	// "calendar group" once the calendar's group filter is judged, "calendar
	// kept dates" once the calendar's dates in range are loaded, before its
	// managers' kept-date counts and carried dates are read (DEC-157), "date
	// visibility preview" and "event visibility preview" once manager
	// authority is judged, "room calendar" once the rooms switch is read (the snapshot's first
	// statement), "preparation" once message.send has decided the audience,
	// "message preview", "compose picker", "message", "message recipients",
	// "message list" and "message reply" once the viewer's right to that
	// message view is judged, and "group detail", "group events", "group
	// members" and "group roster reply" once a group read (readGroup) has
	// judged the group, its roster and its management, "event detail" once
	// the event page has judged the event, and "my household" once the
	// household read has judged the viewer's seat. A test
	// commits an authority change and new data there; read in the snapshot,
	// neither shows, while a judgment moved before the snapshot began would
	// pair the old authority with the new data.
	readerAuthorized func(surface string)
	// pushStep runs in an announcement's or a reminder's push: "snapshot"
	// just before it opens the snapshot that decides it and builds its
	// payload (after the checks that only spare it quiet hours and the hourly
	// limit), and "destination" inside that snapshot once the device, account,
	// switch and consent are read. A test commits a change at either. An
	// announcement push that goes back to wait for quiet hours runs "wait"
	// before its wait transaction (after every earlier check) and "waited"
	// once that transaction committed, before it returns jobs.Requeued.
	pushStep func(step string)
	// consentDeclared receives the visibility consent declaration
	// checkConsentIn made, read back from the transaction (DEC-158).
	// consentEpochLocked runs in lockConsentEpochIn right after its FOR SHARE
	// read, given the open transaction and the epoch read; a test records the
	// backend or parks a confirmation that holds the lock there. bootRotated
	// runs in bootRestoreFence right after the epoch rotation, given the boot's
	// transaction; an error it returns rolls the boot back. consentSelection
	// runs in cancelSelectedLosers between taking its copy of the checked
	// selection and verifying it, given that copy. consentPassed runs in
	// checkConsentIn right after a check passed, given the caller's own
	// entries, so a test can edit the route's selections after the check
	// (design C13). consentSkipTake, answering true for a key, makes the group
	// route skip that tightened event, its Take and its writes, so plan.Done
	// must refuse the omission.
	consentDeclared    func(value string)
	consentEpochLocked func(tx *gorm.DB, epoch string)
	bootRotated        func(tx *gorm.DB) error
	consentSelection   func(sel *dateLosers)
	consentPassed      func(entries []consent.Entry[dateLosers])
	consentSkipTake    func(k consent.Key) bool

	// pushRender overrides renderPush in tests (a rendering past the payload
	// cap); nil uses the stored push text. pushWindow fixes the quiet hours
	// (DEC-126); nil is push.DefaultWindow.
	pushRender func(*models.Notification) (push.Notification, bool)
	pushWindow *push.Window

	// credentialEntropy replaces crypto/rand.Read when a browser credential is
	// minted, so a test can make the mint fail (rand.Read itself cannot).
	credentialEntropy func([]byte) (int, error)
}

// now is the current time. Production reads the wall clock; tests and the e2e
// build inject App.clock, so every time-dependent decision (the calendar
// window, series undo, push quiet hours, an attempt id's age) moves together.
func (a *App) now() time.Time {
	if a.clock != nil {
		return a.clock()
	}
	return time.Now()
}
