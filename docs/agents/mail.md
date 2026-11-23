# Email: sending, choices, suppressions and direct account mail

Read this before touching anything that sends email, reads or writes an email choice, or touches
suppressions. The guards named here are parse-only; each test file states its own limits.

## How mail reaches a provider

- Application mail reaches a provider only through `sendTracked` (`mail_send.go`), which
  classifies a send as sent, suppressed, not configured, permanent or transient; each ledger
  (notices, announcements, direct mail, the composer test send) decides what that means for
  itself.
- `mail.Sender` is `providers.EmailSender`: one interface (`SendEmail`, `Capabilities`) for every
  adapter, the suppression wrapper and `NoSender`. A nil `App.Mailer` counts as `NoSender`
  everywhere.
- Suppression reasons rank in one place, `mail.StrongestReason` (hard bounce, then manual, then
  complaint; an unknown reason counts as manual).
- Suppression SQL lives only in `mail.OpenSuppressionReasons`, the admin helpers and the webhook
  writer, and matches by Go-computed hash. `suppressions` has no address column: it stores `address_hash`
  (`mail.AddressHash`) and nothing else, and a `webhook.process` job carries the same hash
  (`emailEventJob`), never the address (`TestSuppressionSQLStaysInItsHelpers`,
  `TestSuppressionSQLGuardCatchesBypasses`, `TestEmailReportJobsCarryNoAddress`). SQL and Go hashing agree
  on a UTF8 database (`TestSuppressionMatchingBasis`).
- Notices are delivered by the `notify.deliver` job that `notify.Send` queues, never by their
  callers (`TestNoticesAreDeliveredBySendNotByCallers`).

## Optional email choices

- One effective-choice reader, `readEmailOff` (`email_optouts.go`): the account's latest email
  choice or the actual destination address's retained opt-out can turn a category off. Settings
  expose only effective choices.
- `email_address_optouts` is written only by `exportEmailOptOutsIn` during erasure with a held
  address, and deleted only by the current holder's affirmative turn-on under the user lock. Keep
  the original choice time; never reconstruct a legacy tombstone address.
- Version 2 links bind the destination address for turn-on; legacy and moved-address links still
  allow turn-off, and erased-account links stay invalid. A session older than an address change
  cannot turn email on.
- An event audience's announcement (a date, a series or several events) reaches a registrant only
  through a date they may still see (`audienceSeers`): resolution (preview, `message.send`, Try
  again) leaves the others out, and every batch row decides again on its claim, a guardian copy by
  its teen ("can't see this date anymore"). The composer's preview and send count (with the
  names they show), the read page and a push each decide in one read-only snapshot. Preparation
  decides the audience and builds every row it will write (email facts, the guardian copies switch
  and links, push choices) in one read-only snapshot (`preparedAudienceIn`, `preparedRowsIn`), then
  claims in a separate write transaction (users FOR KEY SHARE, then the message).
- **A claimed batch row** (a reminder or an event audience's announcement) decides and fills from
  one statement (`loadDateFacts`, `decideDatedRow`) on its claim transaction, which is pinned to
  Read Committed (`readCommitted`): the claim, the early status check, the choice
  (`readEmailOff`), then that statement, read and closed before rendering. It reads the
  message's current status and cutoff (Try again moves `scheduled_at`), the recipient and the
  subject (a copy's teen), the subject's memberships, host rows, registrations and grants, and
  the dates, events and groups they name; the same decisions as the read page and a push judge
  it (`audienceSeersFrom`, `reminderReadersFrom`, `readerEventValuesFrom`, over `memDates`). An
  edit committed before the statement counts for both the gate and the fields, one after for
  neither; that one email may still go with the old whole payload, and the next row or retry
  decides again. A copy's teen who cannot sign in lends no signed-in authority: the copy says
  only what a signed-out visitor could see. No user or date lock is taken after the claim.
- **The facts statement** (`dateFactsSQL`) reads only the columns in `factColumns`, each named
  with the decision or merge field that reads it (`factColumnReaders`): never a password or
  manage-link hash (a password only as `has_password`), an email or phone, a note, a
  description or a video link, and never a place (an event's, a date's or a group's location, a
  room), a group's name, a household person or event settings no decision reads, since those
  belong to events and groups the reader may not see (`TestFactColumnsCoverTheModels`,
  `TestDateFactsStatementSelectsNoSecrets`, `TestDateFactsStatementReturnsNoSecrets`). It reads
  no birth date or contact preference either: an exact-date grant judges its recipient as a
  `policy.OccurrenceActor` (`occurrenceActorOf`: whose account, and whether it is active), which
  has no age, so a loaded row can never pass for an adult by a missing birth date; code that
  reads the statement's rows never builds a `ContactAccount`
  (`TestFactsStatementRowsNeverBecomeAContactAccount`). The parity tests
  (`TestDateFactsMatchTheQueries*`, `TestRetryFactsMatchTheAudienceQueries`) first give people,
  events, groups and dates non-default ages, contact preferences, places, names and settings in
  the left-out columns (`fillUnreadFactColumns`), and judge each source by its own row of the
  reader, so a decision that read one would disagree; the room columns are kept out by
  `factNeverRead`. Each id
  list is one `bigint[]` parameter, so the bind count is the same for any audience
  (`TestDateFactsBindsTheSameParametersForAnyCount`); its rows are indexed once by person, so
  each decision reads only that person's rows.
- **Try again** is pinned to Read Committed too. After the presser's users row, the message row
  and the daily count, one statement (`decideRetryIn`) decides who pressed it
  (`policy.CanSeeMessageHistory`, a reminder's manager facts included), the author's right to the
  audience or the reminder's date, start and switch, and which failed people `message.send`
  would still reach, a row skipped for a hidden date in the batch's words (`audienceSkip`). A
  guardian copy, of a reminder as of an announcement, goes again only under the copy rule
  preparation builds it by (`copyRule`: copies on, link confirmed, guardian not in the audience,
  teen an active teen who would get it). The teen's email filters (`recipientFacts`) are read
  just after the statement, by their own helpers: they can only skip a copy, so an opt-out
  committed before they are read is honored (`TestTryAgainKeepsAReminderCopyOnlyUnderTheCopyRule`).
  The batch then checks the guardian's own choice at the claim, not the teen's. At 22,000 failed
  rows it stays one statement (`TestTryAgainAtChurchScale` logs the lock time and plan).
- **Historical read access** (`isMessageRecipientIn`, DEC-135): for an event audience a direct
  row lets its person read for good only when it was delivered (sent, delivered, bounced,
  complained) or carries `audience_admitted_at` (migration `00013`), which the writers that just
  made the date check set: preparation for the email skips it finishes, a claimed email row for
  whatever it records after the check (an eligible opt-out included), and an announcement push
  before its provider call. It is set once, never cleared, and never backfilled, and it outlasts
  every later outcome, "can't see this date anymore" included. Any other row (queued, skipped
  before the check, failed by the last-attempt cleanup or the reaper, or written before `00013`)
  needs a date the person may still see, read live; one skipped as "can't see this date anymore"
  never counts.
- Every optional or broadcast email checks the effective choice at its send boundary:
  - Batch rows recheck `message_recipients.address` inside each row's claim transaction, after
    its claim and before rendering and dispatch; optional notifications check inside their claim
    too.
  - The claim runs at Read Committed whatever the database's default, so a choice committed
    before the choice statement starts is honored. A later change may not stop that one in-flight
    send; every later row and retry rechecks.
- Required mail bypasses choices; provider suppressions remain separate.
- Enforced by `TestEmailChoiceSQLStaysInOneHelper`, `TestEmailChoiceGuardCatchesBypasses`,
  `TestReadEmailOffCombinesAccountAndDestination`, `TestEmailSettingsHideChoiceSource`,
  `TestReusedAddressKeepsErasedChoicesUntilHolderTurnsOn`,
  `TestOptionalMailChecksTheEffectiveChoice`, `TestOptionalMailGuardCatchesBypasses`,
  `TestBatchRechecksTheChoiceAtEachRow`, `TestBatchHonorsAChoiceCommittedRightAfterTheClaim`,
  `TestTestSendHonorsTheEffectiveChoice`, `TestTestSendInFlightWindow`. The batch and Try again
  boundaries: `TestBatchEmailNeverPairsALapsedGrantWithAMovedDate`,
  `TestBatchEmailInFlightSendsOnlyTheOldWholePayload`,
  `TestGuardianCopyOfADisabledTeenSaysOnlyWhatASignedOutVisitorSees`,
  `TestBatchEmailCountsFromTheCutoffAsItStandsNow`, `TestTryAgainRechecksAReminderAfterTheLocks`,
  `TestTryAgainRefusesAnAuthorDemotedAfterTheLocks`, `TestTryAgainNeverPairsALapsedGrantWithAMovedDate`,
  `TestDateFactsMatchTheQueries*`, `TestRetryFactsMatchTheAudienceQueries`; read access:
  `TestReadEntitlementNeedsProofTheCheckLetThemIn`, `TestReadEntitlementEvidenceSurvivesLaterOutcomes`.

## Direct account mail (M7.5, R6-4)

- Reconstructible account mail uses `mailKinds` on the existing job runner, with eight attempts.
  Enqueue with the domain change and audit in one transaction; standalone hints and guardian
  resends must durably enqueue before answering. Keep public answers uniform.
- Payloads contain only `userId` (reset and existing-account hints), `requestId` plus a fixed
  `case` (received, approved, exists, conversion_approved, rejected), `invitationId`,
  `accountTokenId` (email verification), `guardianLinkId`, or `guestSignupId`. Never queue an
  address, note, rendered message or raw capability.
- Prepare from current stored facts in a transaction, mint or rotate the capability there,
  commit, then send through the suppression-aware mailer without holding database locks. Match
  redemption's lock order: contact gate then user/link/token for account flows; invitation and
  guest mail each lock only their own row and read authority facts without earlier-order locks.
  Claims recheck the submitted hash, unused state and wall-clock expiry after locking.
- Delivery is at least once at the provider boundary. A retry may replace an already accepted
  email's link; only the newest capability works. Do not promise exactly-once email delivery.
- Direct account mail finishes quietly (logged) for a suppressed address, a permanent refusal or
  no email service (`NoSender`, warned at startup); only transient errors retry.
- Delivery jobs use the 60-second `deliverSendTimeout`. Failure logs use `jobs.CleanError` and
  never add a recipient address or capability.

## Guardian copies of notices (#364, DEC-19, DEC-29, DEC-149)

`notify.guardian_copy` (`notify_guardian_copy.go`) sends a confirmed adult guardian a marked copy of
an optional notice email sent to their teen. Plan section 7.4 has the rules; the hazards are:

- **Queued inside the claim, before dispatch, under a savepoint.** `sendHeldNotification` reads the
  switch with `boolSettingErrIn` (never `boolSettingIn`, which swallows a read error and would let the
  claim carry on in an aborted transaction) and the guardians with `contactGuardians` (the one
  eligibility join; never write a second), enqueues one job per guardian, and rolls the savepoint back
  for every outcome but sent. **Add no statement after dispatch on the sent path**, not an enqueue and
  not `RELEASE SAVEPOINT`: a failure there rolls back the teen's sent status and mails the teen again.
  Copy jobs therefore commit with the teen's sent status and only with it.
- **Ids only** (`guardianCopyJob`), `unique_key` per notice and guardian. The job reads every fact
  again in one read-only Repeatable Read snapshot (no lock) (notice, kind, teen's sent delivery, switch, teen, guardian link,
  direct-recipient dedupe, the teen's live access to the subject, the guardian's own choice and
  suppression), then sends outside it. A failed read is an error and retries; a "no" is a quiet
  finish. A change committed between the reads and the provider may not stop that one copy. Delivery
  is at least once.
- **The text is the bell title and body** (`Kind.CopyText`), never `EmailTitle`/`EmailBody`, which
  carry the teen's manage and video links; `CopyTitleOnly` kinds show the title alone because their
  body can carry a leader's note. The copy has no link and uses the guardian's own unsubscribe.
- **Live access follows the subject** (`teenSeesNoticeSubject`: occurrence, event or group; any other
  subject type is not copied). `Kind.CopyWithoutSubjectAccess` skips only that check and is set on
  `registration.cancelled` alone (DEC-149). That copy is **not** `CopyText` of the stored notice: the
  teen's title can carry the date (the single-date path), and a title can hold any separator, so the
  copy is `Kind.LostAccessCopyText` of the plain title the writer stored in the notice's `data`
  (`notify.DataEventTitle`). A notice without it gets no copy. Never parse the stored title. The
  cancellation writer (`cancelSelectedLosers`) stores no title, and names no event, for a holder
  who could not see the date just before the tightening (a retained registrant of an archived
  group): the title may have been set while it was hidden from them, so neither they nor their
  guardians learn it. A tightened cancelled date showed its title only to someone entitled to its
  tombstone (`policy.CanSeeCancelledOccurrence`), so a holder whose only row on it is a household
  person's is told no title either. Its siblings `event.cancelled` and `event.restored`
  (`notifyOccurrences`; DEC-91 keeps "back on" for everyone still signed up), and
  `registration.removed`, `registration.not_restored` (`registrationNoticeIn`) and
  `registration.promoted` (`registrationNotice`, which adds the manage and video links only when
  shown), do the same for a registrant who cannot see the date now. Eligibility counts an invited or
  pending-consent account as signed in; notices judge it as signed out, so a promotion into a
  private date tells it nothing about the date. Such a row goes through `withheldDate`: the kind's
  nameless text (no title, time, place or link) and no subject, expiry or data, so the feed's
  `subjectId`, its archiving and push urgency follow nothing about the date. With no subject the
  teen's live access can't be shown, so no copy goes. The direct-recipient dedupe matches the kind
  and dedupe key alone, since a withheld row has no subject columns to match.
- A new optional email kind fails `TestNoticeGuardianCopyKindsAreClassified` until someone decides
  whether its guardians are copied.
- Guards: `notify_guardian_copy.go` is a delivery file in `mailerSendFiles`;
  `guardianCopyEmailMessage` is the one place that builds its optional message, and
  `prepareGuardianCopy` must call `readEmailRecipient` and `.Off` (`optionalMailViolations`).

## Synchronous exceptions

Only these send outside a job:

- `App.sendEmailChangeNotice` (`api_profile.go`) needs the old address and cancellation
  capability, which cannot be reconstructed after verification.
- `App.sendGuestConfirmationEmail` (`api_guest.go`) shares its manage capability with the
  browser, so delayed rotation would invalidate the displayed link.
- Both use the 20-second `syncMailTimeout`, leaving 40 seconds of the server's 60-second write
  budget for transaction work and the response.
- The composer's test send (`api_message_preview.go`) is synchronous by design. It checks the
  author's session and effective email choice under the user row lock, records the attempt,
  commits, then calls the provider with a 30-second context and no user lock held. A change
  committed after that commit may not stop that one test send; the next request rechecks.

## The mail guard (`TestMailGoesThroughJobs`)

It parses the production tree; only `internal/mail` (which owns the sender implementations) is
exempt from the send checks. Sends are allowed only in the delivery files (`mailerSendFiles` in
`guards_mail_test.go`) and as immediate calls in the named synchronous methods, and calls to those
methods must be immediate too (never in a closure, `go`, `defer` or a method value). `api_dev.go`
may unwrap and read the outbox, never send. **When you add a sender type, add it to the guard's
list in the same change.** Its reach and limits are documented above `TestMailGoesThroughJobs`.
