# M8.1 brief — EventDate passes the 3-second test

Milestone: [M8.1 in `phase-8.md`](../milestones/phase-8.md) (teardown P0, Luma).
The milestone owns the end goal. This brief owns the visual and interaction
end state, the viewer cases, the API boundaries, the slices and the proof.
The plan behind it was accepted on Nov 18, 2026, together with Grier's answers
recorded in section 2. The two documents must agree; when one changes, change
the other in the same PR. Nothing here overrides `AGENTS.md` or a DEC. Line
numbers drift, so the brief cites functions and files, as of `15ab2d6b`.

Prerequisite: R15-1 (M7.9, [`R15-R16-frontend.md`](audit-2026-11/R15-R16-frontend.md))
makes `EventDetail` and `GroupDetail` lazy chunks, adds the route error
boundary and sets the measured bundle budget. It lands before any M8.1 page
edit, so the before/after bundle measurement is clean. M7.10 (generated types)
is out of this milestone; it lands between feature waves, not during these
page edits.

## 1. Goal: six answers in the first viewport

A member opens one date of a series (`/events/:id/dates/:date`) on a 390px
phone. Without scrolling, the page answers:

1. **What is this?** A cover (see 2.3), the event title, and the host line.
   For a shown group that is the group's name with its existing
   display-safe leader name. For an ordinary reader of a church-wide date,
   or of a date whose group is hidden from them, it is the church label the
   date projector already gives ordinary readers (`HostedBy` through
   `labelGroup`), never a named host person. An invitation-only reader gets
   no host line at all, as today: the projector sets `HostedBy` only for
   ordinary readers, and that stays. The series page keeps its existing
   named-host disclosure unchanged.
2. **When?** The date and start–end time. The event's zone is named when it
   differs from the viewer's (the `heldIn` line).
3. **Where?** The confirmed room or the effective place
   (`events.EffectivePlace`), and the remote state as the date actually
   offers it (see 4.3). This is never shown only in the description.
4. **Am I in?** The member's own answer is the page's main status (see 2.1).
5. **Who else is coming?** The count first, then names only to the extent
   the existing attendee rules allow that viewer.
6. **What is different about this date?** One plain sentence when this date's
   time, duration, place or room, in-person or remote capacity, or visibility
   differs from the series' usual values. A cancelled date has its own view
   (see 2.2).

The RSVP action stays reachable in a sticky bar while the rest of the page
scrolls (section 5). The series page (`/events/:id`) and the date page stay
separate routes and are never merged into one.

Capacity is shown honestly: in-person seats, remote seats and childcare are
separate counts when they exist, never one merged number. A full date shows
the viewer's own waitlist state on the page (section 3).

Deliberately avoided: one-off party framing, hype copy and themed event
styling. Fold dates recur and are pastoral, so the wording stays warm and
plain (DEC-52, DEC-60–DEC-66).

## 2. Settled decisions (Grier, Nov 18, 2026)

These were asked and answered while the plan was accepted. Do not reopen
them in a slice PR or a review.

### 2.1 Series standing is inferred from coverage, with no provenance

Registrations carry no series-batch marker and none is added: no column, no
migration, no stored "answered for the series" flag. "You're in for the
series" is inferred. It applies only to a recurring series that has a
positive number of eligible upcoming dates and an applicable answer on this
date, when the viewer's own Going or Maybe answers cover every one of those
dates. The eligible dates are counted with the same time and date-visibility
boundaries as series RSVP (`api_series_registrations.go`). A partial batch or
a zero denominator never establishes full coverage.

The three plain sentences are "in for the series", "in for this date only"
and "not in". The other answers stay distinct and are never merged into
those three: Maybe reads as tentative, declined as declined, waitlisted as
waiting, and a cancelled personal answer as cancelled. A cancelled personal
answer on a scheduled date is not the same as a cancelled date, nor the same
as no answer. Today the scheduled read drops a cancelled row before it
projects `myRegistration` (`loadOccurrenceDate`, `api_occurrence_access.go`),
and `TestDatePageIgnoresCancelledRegistration` (`api_events_test.go`,
added in `d11ca18d`) pins that. No DEC or PLAN section was found behind the
drop (the filter is on the ordinary path only, with no comment), so the
cancelled-date slice (section 7,
slice 3, where the accepted plan puts this distinction) deliberately
replaces that test with the three-way assertion in section 8 and makes the
ordinary viewer's own cancelled answer observable:

- A cancelled own row adds only its status (`myRegistration` is
  `"cancelled"`). It is history, not a current RSVP: the read omits
  `myAttendanceMode` and `myGuestCount` for it, and every capability
  (`canRegister`, `canDecline`, video, guests and the rest) is exactly what
  an unanswered viewer of that date gets. The page therefore never shows
  the old mode or guest count as attendance and never prefills the next
  answer from them.
- This is for the ordinary read only. The invitation payload stays as it
  is, and `calendar.ics` is unchanged.

An invitation-only reader sees their own answer with no implication about a
parent series.

Series standing never decides whether Undo shows: Undo follows the server's
receipt (5.2).

### 2.2 A cancelled date shows a minimal tombstone to qualified viewers only

A cancelled date's JSON read gives a qualified viewer exactly three facts:
the effective title, the original date, and that it is cancelled. The page
says the date was cancelled and links to the calendar. It shows no RSVP,
series, video, attendee, reminder, guest, share or calendar-download
controls.

Entitlement is decided in `internal/policy`. The handler matches the viewer's
own row to the **stable occurrence id** of that exact date, never a date
string or a sibling, and loads the current account (`ContactAccount`), consent and context facts
on every path. "Own row" means a registration whose subject is the
signed-in viewer (`userSubject`: `user_id` is the viewer and there is no
household person). A household person's registration is that person's
row, not the holder's, even when the viewer holds it (`registered_by`); it
entitles nobody to the tombstone. A row qualifies on one of two paths,
each by its own authority:

- **Ordinary registration.** The viewer's own row, still standing
  (`models.CurrentRegStatuses`: Going, Maybe or waitlisted), and the viewer
  holds now, apart from any exact-date grant, the ordinary access to that
  date that the scheduled read would require.
- **Exact-date grant.** The viewer's own grant on that row, valid for the
  cancellation being shown. Two things must both hold:
  - **Now:** the current facts pass every existing grant check in
    `occurrenceInvitationFacts.endReason` other than the date being
    scheduled and not yet over. That means the account can sign in, a
    teen's consent is active, the event and its group still exist, the
    context is live (`ContextActive`: church-wide, or the group is
    active), and the row still stands (`models.CurrentRegStatuses`). An
    `occurrence_cancelled` end reason left on the row never excuses a
    later consent loss, a disabled account or a changed context.
  - **At this cancellation:** the grant was valid when this cancellation
    of the date happened. The existing records establish that in one of
    two ways, and anything else is a denial.

    *How a cancellation ends grants (as of `15ab2d6b`).* Both writers
    that cancel a date (the series edit's `ActionCancel` and the manual
    date cancel in `api_events.go`) run in `contactMutation` with the
    event's scope, under the exclusive contact gate. Before the change,
    `reconcileAccessesBefore` ends every grant already past its DEC-74
    expiry with `occurrence_ended` (`OccurrenceAccessCurrent`: a departure
    grant ends with the date's end; a manager add has no time limit).
    After the change, `reconcileContacts` ends every other live grant on
    the event's dates in the same transaction. `endReason` puts
    "not scheduled" before expiry, so on the cancelled date that end is
    `occurrence_cancelled`. A committed cancellation therefore leaves no
    live grant on the date. The same holds at `v0.1.0`, the only tagged
    release, so no release commits an unstamped grant on a cancelled date.

    *Which instant "at cancellation" means.* It is the cancelling
    transaction's lifecycle clock: the `now` that `contactMutationOn`
    samples once, after taking the gate, and passes to both
    reconciliations. That is the existing convention for every grant end,
    the DEC-74 expiry included, and this brief keeps it. The date's update
    can land later, after a row-lock wait, and no record holds that later
    instant; the brief does not claim it. So an `occurrence_cancelled` end
    written by that transaction proves the grant was live and unexpired at
    that clock.

    - **Ended path.** The grant was ended with exactly
      `occurrence_cancelled` (`policy.OccurrenceCancelled`) by **this**
      cancellation, not by an earlier one that a restore undid. DEC-91
      restores the same occurrence id and leaves the grant ended
      (`api_occurrence_mutations_test.go`). So cancel, restore and cancel
      again leaves a grant whose id and end reason match a cancellation
      under which it was never live. As of `15ab2d6b` the only writer that
      returns an existing date to scheduled is the series restore
      (`ActionRestore` in `api_events.go`). In the same transaction it
      writes `event.dates_restored` (`restoreSeriesDates`) with the
      occurrence ids. The link is decided by **audit row id order**,
      never by a timestamp. `audit_logs.at` defaults to the transaction
      start, and `contactMutationOn` begins the transaction before it
      waits for the gate and samples `a.now()` after. So a restore that
      queued behind a cancellation can commit later with an earlier `at`
      than the grant's `ended_at`. The ids are ordered:
      - `audit.Write` leaves the id to `audit_logs_id_seq`, which is
        `CACHE 1`.
      - Both rows are inserted while their transaction holds the contact
        gate. The end snapshot is written by `endAccess`, called from
        `reconcileOccurrenceAccesses`. That is the only caller passing an
        `occurrence_cancelled` reason (from `endReason`). Every other
        `endAccess` caller passes a fixed reason:
        - declined, removed or superseded, with an actor;
        - `context_changed`;
        - `event_deleted` (`endLiveAccessOnDates`).

        Every path into `reconcileOccurrenceAccesses` holds the gate:
        - `contactMutationOn`;
        - the contact operations;
        - the teen lock;
        - the try-locked sweep.

        The restore row is written inside the series edit's
        `contactMutation`.
      - `pg_advisory_xact_lock` is held until commit, and the restore
        takes it exclusively.

      So whichever of the two transactions took the gate second has the
      larger id. The check:
      1. Find this grant's own end snapshot: exactly one
         `occurrence_access.ended` row on this registration whose
         `occurrenceId`, `grantedAtMicro`, `endedAtMicro` and
         `endReason` equal the row's current grant.
      2. Read every `event.dates_restored` row for the date's event whose
         `occurrenceIds` names this occurrence.
      3. Qualify only when no such restore row has a larger id than that
         snapshot.

      These are two narrow lookups, not `loadAccessHistory`, which keeps
      its contract for repeat adds (it drops the audit id and the end
      fields, and fails on corruption anywhere in its scope):
      - The snapshot lookup reads only `occurrence_access.ended` rows
        whose target is this registration (`target_type`, `target_id`).
        It applies the existing `accessSnapshotValidSQL` predicate to
        those rows alone, not a second parser, and returns each row's
        audit id and the fields compared in step 1.
      - The restore lookup reads only `event.dates_restored` rows whose
        target is the date's event, and returns their ids and
        `occurrenceIds`.

      Deny when there is no matching snapshot, more than one, or a row
      on this registration's target that fails the predicate. Also deny
      when a restore row for the event has unreadable `occurrenceIds`.
      That is unreadable evidence, a denial. A failed query is an error.
      A corrupt snapshot on another registration does not affect this
      date. `at` and `ended_at` are never compared with the restore. The
      slice confirms these points:
      - each ordering premise above still holds;
      - no other writer returns a date to scheduled;
      - no grant starts on a date that is not scheduled, so a grant that
        this cancellation stamped was live before it. Both callers of
        `grantRegistrationAccess` check the status:
        - the departure flow keeps only scheduled dates
          (`api_departure.go`, the `event_occurrences.status` filter and
          `Keepable`);
        - a manager add refuses a locked occurrence that is not scheduled
          (`api_registrations.go`), and an add request ends when its date
          is not scheduled (`AddIneligibility`);
      - both rows survive every erasure and cleanup path with the fields
        read here intact. Erasure blanks the restorer's IP and scrubs no
        key of either action, `account_erasure_audit.go`.

      If any of these fails, the ended path is denied and the slice
      reports it rather than guessing. The "qualified after cleanup" case
      would then need Grier's decision, because no schema change for the
      tombstone is in scope.
    - **Live path (before cleanup).** The grant has no end stamp on the
      cancelled date. No writer commits this state as of `15ab2d6b`, and
      the occurrence records no cancellation time. Validity at the
      cancellation is established only from facts that cannot have
      changed since:
      - The provenance and stamp checks of `OccurrenceAccessCurrent` pass,
        with only the date's `Scheduled` fact waived. The grant writers
        refuse a date that is not scheduled, so a live grant started
        before this cancellation. The slice confirms this and otherwise
        denies the path.
      - A manager-add grant qualifies, because it has no expiry.
      - A departure grant (`departure_self`, `departure_removal`)
        qualifies only while `now` is before the date's stored end. Then
        the cancellation's lifecycle clock, which was before `now`, was
        before the DEC-74 expiry too. The `ends_at` writers found at `15ab2d6b` are
        the series edit's move and restore and the per-date edit
        (`api_events.go`). Each runs in `contactMutation` with the event's
        scope, so its "before" run would have stamped this grant. The slice
        confirms there is no other writer and otherwise denies this
        case.
      - Once the date's end has passed, a departure grant that is still
        unstamped is **denied**. The records cannot tell whether it
        expired before the cancellation.
    - **The tombstone outlives the date.** A grant that qualified on the
      ended path stays readable after the date's end. Its stamp fixed its
      validity, and the date-end check is excluded from the "now" checks
      above.
    - **The ended path never reads the date's current end.** The per-date
      editor can retime a cancelled date without restoring it, so
      `ends_at` is no longer the end the grant had when it was cancelled.
      The stamp from the cancelling transaction is the proof, and nothing
      compares it with the stored end.
    - **What the records cannot place.** If a live grant on a cancelled
      date ever existed, a later reconcile that reached it would stamp it
      `occurrence_cancelled`, because `endReason` puts "not scheduled"
      before expiry. That stamp looks the same as one the cancellation
      wrote, and no record tells the two apart. This brief promises only
      the live path's rule for such a grant while it is still unstamped.
      It promises nothing about it after a later stamp. No tagged release
      commits that state. The slice confirms with a test that both cancel
      writers leave no live grant on the date. If the slice finds a writer
      that commits that state, it stops and reports it for Grier's
      decision rather than adding a comparison. Telling the two apart
      needs a record that does not exist, and no schema change for the
      tombstone is in scope.

  The row's RSVP status never stands in for the grant.

These are denied the existing unavailable response:

- a retained row whose only authority was a grant ended for any other
  reason, even with its RSVP status intact: declined, blocked, removed,
  superseded, `consent_lost`, `context_changed` or any other end
  (`endAccess` keeps the row, so the row alone is never authority);
- a grant ended with `occurrence_cancelled` by an earlier cancellation
  that a restore undid (a restore row with a larger audit id, whatever
  its `at`), and an ended grant with no matching end snapshot, more than
  one, a row on its registration that fails the predicate, or a restore
  row for the event with unreadable `occurrenceIds`;
- a departure grant still unstamped on a cancelled date whose end has
  passed;
- a grant whose end reason matches but whose holder now fails a current
  account, consent or context check;
- a viewer whose only link to the date is holding a household person's
  registration;
- a row cancelled by a visibility loss (`cancelDateLosers`,
  `api_group_visibility.go`), which exists so a retained row never exposes a
  later private edit, and any other cancelled row;
- ordinary public-date access, or a manager role, on its own;
- a pending request, someone else's row, a sibling date, an invalid or a
  deleted date.

A failed facts query, including the snapshot and restore-record reads, is
an error, never "not qualified". Evidence the check reads and finds
missing, duplicated or unreadable is a denial, not an error.

Every fact the cancelled outcome uses comes from one snapshot. That means
one read-only Repeatable Read transaction (the pattern in
`nextVisibleDates`, `api_events.go`), with every query on it. It covers:

- the date's status that selects the cancelled outcome;
- the viewer's current account and teen consent, re-read in the
  transaction rather than taken from the request's session load;
- the ordinary path's current-access facts (the in-transaction
  `loadCandidates` form, not `dateCandidates` on the shared handle);
- the event and group context;
- the row and its end snapshot;
- the restore rows;
- the effective title projected into the answer.

If that snapshot does not show the date cancelled, no tombstone is given.
Separate reads are not a substitute, whatever their order. A disable or a
title edit committed between two of them could pair a title that no
committed state authorized with an entitlement that has since ended. The
snapshot takes no contact gate, so page reads never queue behind lifecycle
writers.

Grier settled the tombstone, its three facts, the qualifying invitation
(valid at cancellation: live, or ended only by that cancellation) and the
exclusions. These points make that decision exact and are not new policy:

- the meaning of "standing" (`models.CurrentRegStatuses`);
- the ordinary path's need for current ordinary access;
- "own row" as the viewer's own subject (`userSubject`), which follows the
  accepted plan's "the viewer's own qualifying registration" and its denial
  of other people's rows;
- the grant path's current checks;
- tying an ended grant to the cancellation it ended;
- "at cancellation" as the existing lifecycle clock of the cancelling
  transaction.

The tombstone is JSON-only. `calendar.ics` for a cancelled date stays 404,
including for a qualified viewer. Feeds, discovery and every write keep
their existing authority and scheduled-date behaviour.

### 2.3 No cover for hidden-group or church-wide dates

There is no event-level cover and none is introduced, including any church
or site image. A date whose group is shown to the viewer gets that group's
cover through `coverSrc` (`web/src/lib/covers.ts`), with its focal point
applied, so a photo-less shown group still gets its standard generated
cover (DEC-66). A church-wide date, or a date whose group is hidden from the
viewer, has no cover block: the title and date lead. A missing photo on a
shown group is not the same as a missing group.

### 2.4 Waitlist copy has no deadline and describes what actually happens

The page's waitlist wording names no deadline, because the current
promotion has none. It describes the observed outcome in section 3 and
nothing more. It promises no confirmation step, no "by when", and no
time-triggered release.

**Explicit confirmation is an outstanding follow-up.** The milestone's
criterion that promotion needs the member's explicit confirmation stays as
written in `phase-8.md` and is **unmet** by this work. M8.1's completion
record names it as unmet and outstanding, and does not claim it. It does
not block this flow's merge. The outstanding end state is the criterion as
written: explicit confirmation together with when it is due ("and by when").
This brief neither authorizes a rewrite of the promotion path nor designs
that follow-up; its design sits outside M8.1. What ships now is the
deadline-free copy above, true to today's behaviour. Today's
behaviour is the "automatic waitlist backfill without confirmation" that
`phase-8.md`'s "Not promoted" table names as an anti-pattern.

### 2.5 Custom confirms are confined to EventDate and EventDetail

M8.1's "no `window.confirm`/`alert`" criterion covers the
`/events/:id/dates/:date` and `/events/:id` routes. Shared components keep
their native confirmation on other routes. `GroupDetail`'s native confirms
stay with R15-4.

## 3. Observed waitlist promotion (current behaviour, not a decision)

This section records what the code does today, separately from 2.4. Copy
written from it must stay true if it is read on any date.

- When a seat is freed outside the cutoff window, `promoteRows`
  (`api_registrations.go`) moves the chosen waiting rows straight to
  **Going**. `promoteRegistration` (`registration_rows.go`) stamps
  `promoted_at` and rotates the manage link. Every promoted member ends up
  Going and is notified; there is no confirmation step and no deadline.
  The notice depends on the row (`notifyRegistrationPromoted`):
  - An ordinary promotion sends "You're in: …" (`RegistrationPromoted` in
    `internal/notify/kinds.go`), and its email carries the manage link with
    its "Can't come after all?" cancel.
  - When the promoted row carries the recipient's live exact-date grant,
    they get the safe `OccurrenceChanged` date notice instead, with no
    manage link.
  `web/e2e/waitlist.spec.ts` asserts the ordinary promotion, and that test
  keeps its behavioural assertion. Page copy never names the notice, its
  wording or its channel, so it holds for both.
- Who is promoted goes through eligibility and the seat tally
  (`tally.pick`), not position alone. Copy never says "you're next" from a
  position number.
- Inside `events.CutoffWindow` (2 hours before the start), nobody is
  promoted automatically. The leaders or hosts are told when someone in
  line could take a freed seat, and they may promote by hand. Copy never
  says the member will be moved in automatically without that qualifier.

Illustrative wording (the sticky-bar slice settles the exact copy): you're on the waitlist (with your position when
the read gives it); if a seat opens, you may be moved to Going and Fold will
let you know. Close to the start, a leader decides.

## 4. Viewer variants

Every viewer decision stays in `internal/policy` and `projectDate`
(`date_projector.go`). The page renders what the read gives it and decides
nothing about who may see what.

| Viewer | Gets | Never gets |
|---|---|---|
| Ordinary member, series visible | All six answers, series standing and coverage, series scope choices, the difference sentence against series values | Declined counts, exact childcare usage unless `CanSeeChildcare`, others' contact details or care notes |
| Manager (group leader, church-wide host, admin) | The member view plus manager-only declined data (`registrationCounts`) and childcare totals | Nothing a policy function doesn't already grant |
| Ordinary reader of one opened date of a hidden series (DEC-46 override) | The ordinary date facts | Recurrence, series coverage, comparison with series values, series actions: all need `ShowsSeries()` |
| Invitation-only reader (exact-date grant) | The existing invitation payload (its `remoteAvailable` and video fields as today), their own answer, Going/Maybe through `Dates.register`, decline/block through `Dates.end` | `HostedBy` or a church label, and any new organizer, cover, attendance, capacity, childcare, remote-state or series key |
| Signed-out visitor on a public date | The public facts, public guest signup, names only as the existing attendee rules allow | A personal answer, manager data |
| Qualified viewer of a cancelled date | The tombstone (2.2) | Anything else |
| Anyone else | The existing unavailable response | — |

### 4.1 Attendance (answer 5)

Going and Maybe **row counts** are separate from **seats used** (seats
include guests). The count comes first. Names show only where
`canSeeAttendees` allows, with the existing subject rules for minors,
households and guests (`registration_subjects.go`). The read returns only
the fields the name summary needs. Where some names are private, the page
compares the names shown with the row counts for Going and Maybe
separately to say so. A hidden sibling date changes no count.

### 4.2 Capacity

In-person and remote pools are reported independently: capacity, usage and
remaining seats. An unlimited pool is null where the pool is disclosed, and
is never "full". Exact childcare usage and remaining places need
`CanSeeChildcare`, and hidden-parent childcare redactions stay. Other
readers get the existing availability and full signal. A failed count read
is an error, never apparent availability. A Going member keeps the seat they
already hold when a pool is full.

### 4.3 Remote state (part of answer 3)

The date states the actual remote option on two independent axes. They are
separate rules in the code and the page never derives one from the other.

- **Remote RSVP** (`events.RemoteOffered` over the effective remote cap):
  offered; closed for this date (remote on for the event, effective remote
  cap 0); or not remote (in person only). A full remote pool is a capacity
  state (4.2), not closed.
- **Link access** (`pageVideoRight`, `policy.CanSeeOccurrenceVideo`,
  `videoWhen`): the authorized URL is shown with the join action; the link
  is hidden until the viewer is exactly Going (`registered_only`; Maybe is
  not enough); or no link is configured. An ordinary manager sees a
  `registered_only` link without a Going answer. That exception comes from
  ordinary access and never from an exact-date invitation.

The remote RSVP distinction (offered, closed for this date, not remote) is
for ordinary readers only. It tells them the effective remote cap is zero,
which is capacity, so an invitation-only reader never gets it: their payload
keeps today's `remoteAvailable`, false for both closed and not remote, and
the page never infers "closed" for them from `remoteAvailable` being false.
Their link access is unchanged (`pageVideoRight` and `videoWhen` as today).

Closing remote registration does not withdraw a link the viewer may already
see, so "remote closed for this date" and a visible join action can appear
together. Fold has no time-triggered link release (`VideoRevealAlways` and
`VideoRevealRegisteredOnly` are the only reveal values), so the page never
says a link will appear at meeting time.

### 4.4 Differences (answer 6)

The date is compared with the series' usual wall clock on its original
local date (`starts_local` and `tzid`; event-zone DST included), usual
duration, authorized place including usual versus confirmed room, in-person
and remote caps, and series visibility. Real values are compared, not
`Customized()`. Equal-valued overrides produce no sentence, and an
unchanged date has none. A change to any single one of these produces one,
even when it is the only change. Only authorized differences are sent.

The usual clock is what the series writer generated for the original day
when it wrote the date, stored beside the date (DEC-155,
`occurrence_series_starts`); the read never walks the rule.

- **Writer observation.** Creation (`Ensure`), the daily extension and a
  series edit record, from the same walk that writes the dates, every
  distinct instant the rule makes on each written date's original day, the
  first in the walk's order marked. A series edit writes it for every date
  that was upcoming before the edit and every date it adds; a past date
  keeps what it had while upcoming, in the zone it was written in, so a
  later change of the series zone alone is no move. An edit that makes the
  event one-time instead drops the evidence of the dates that were
  upcoming, so the old series' clock never comes back as a past date's usual
  clock if the event later repeats again. A per-date edit leaves it alone.
- **Several times on one day.** The date is compared with the time it
  starts at, else the earliest in the same UTC minute (the date editor saves
  whole minutes; the two passes through a repeated hour are different
  minutes), else the first. A series edit adds a new multi-time day once, at
  its last time, as it moves such a day.
- **Daylight saving.** A generated time keeps the instant the walk gave it,
  including one moved onto the day before. A kept day the rule does not
  generate uses the series start's written wall clock on that day, kept as
  written even in a gap, with the instant `time.Date` resolves it to.
- **Edges.** The walk continues a few days past the stored horizon for
  evidence only, so a day the horizon splits still has all its times. A day
  the walk does not cover is unknown, never a guessed fallback, and the
  daily run tries it again.
- **Zero DTSTART.** rrule-go walks a series starting at Go's zero instant
  from the current time, so only the walk that wrote a date can vouch for
  its clock: later runs never replace it and never rebuild a missing one.
  A new series edit writes fresh evidence with the dates it moves.
- **Unknown history and repair.** Dates written before the store, and dates
  of a series a previous binary edited (`series_clock_dirty`), read with an
  empty clock and claim no time change until the `occurrences.extend` repair
  rebuilds upcoming dates from the current rule. Past ones stay unknown. An
  empty clock still reports a `day` change when the shown day is not the
  original day.

## 5. Phone priority, sticky action and mobile navigation

### 5.1 Priority at 390px

The first viewport holds, in this order: a compact header (cover when shown,
title, group and leader or host line), when, where with the remote state,
the viewer's answer as the main status, the attendance count with its
optional name disclosure beside it, then the difference sentence when there
is one. The cover is short and never pushes the answers below the fold. The
first-viewport slice may adjust the order within the first viewport against
the 390px screenshot, as long as all six answers stay inside it. Everything
else sits below the summary: the description, the capacity detail beyond
the at-a-glance counts, video detail, reminders, invitation and date
actions, and the guest form. It uses existing tokens and composition classes (`design.md`) and the
shared `components/ui` controls. Dates are formatted only in `lib/when.ts`.

### 5.2 The sticky answer bar

- There is one operable "Your answer" group, one mutation gate and one live
  status. The sticky bar is the one presentation of the member action, not a
  second set of buttons. Mode and guest inputs share its state and
  validation.
- Signed in at phone width, the fixed tab bar (`Layout.tsx`, `nav
  aria-label="Tabs"`, `3.5rem` plus `env(safe-area-inset-bottom)`) is at the
  bottom edge. The bar sits directly above it, accounting for its height and
  the safe-area inset, so neither intercepts taps on the other. Signed out
  there are no tabs; the bar respects the safe-area inset alone. The page
  reserves space so the last content and any focused control are never
  hidden behind the bar or the tabs. The tabs and the footer stay usable,
  including by keyboard.
- Actions follow existing capabilities. Invitees use Going and Maybe
  through `Dates.register` and decline or block through `Dates.end`,
  omitting unknown mode, guests and care options. Ordinary per-date answers
  use `Events.register`. Signed-out visitors get public guest signup.
  Read-only states show no action. The tombstone view has no bar.
- Authorized recurring-series answers reuse the existing scope choices
  (`SeriesRsvpDialog`: just this date, this and following, all future), with the
  extra consent for a series decline. "This and following" uses the original
  date. The batch result and refusal handling, `undoToken` and
  `undoExpiresAt` come from the server. Standing is derived from refreshed
  coverage after a save or an undo, so a successful request with partial
  coverage never reports "in for the series".
- Undo shows whenever the server returned a valid receipt (`undoToken`,
  `undoExpiresAt`), whatever standing the page displays. The series writer
  returns one whenever any date changed (`api_series_registrations.go`),
  including a partial batch, one with refusals, and a series decline, so a
  partial or declined answer still gets Undo. While it exists it is as
  reachable and prominent as the RSVP action. It clears on expiry, terminal
  error, session change or route change.
- Full and available wording is per pool. Waitlist copy follows sections
  2.4 and 3.

## 6. API boundaries

- **Changed read**: `GET /api/v1/events/:id/dates/:date` (JSON only).
  - It gains the cancelled-date tombstone (2.2) as a named wire shape with
    exactly three keys.
  - It gains the scheduled-date facts above: shown-group cover, image,
    focal point and leader name through the shown-group presentation path;
    row counts and seats; per-pool capacity; the viewer's own waitlist
    position; series coverage; the authorized series-action signal;
    difference facts; and, for ordinary readers only, the remote RSVP state
    (4.3), including remote closed for this date as distinct from not
    remote (today `remoteAvailable` is false for both). The cancelled-date
    slice has already added the ordinary viewer's own cancelled answer
    (2.1).
  - An invitation-only reader's scheduled payload is unchanged by both
    slices. It has no `HostedBy`, and none of the new organizer, cover,
    attendance, capacity, childcare, remote-state or series keys.
  - The response is typed so the page narrows to the scheduled or cancelled
    form without casts, and the page never invents scheduled values for a
    tombstone. Exact key names are fixed by the slice that adds them, in
    `api_response_shapes.go`, `web/src/types.ts` (paired in `tsContracts`)
    and `docs/API.md`, in the same PR.
- **Unchanged**: `GET …/dates/:date/calendar.ics`, feeds and discovery.
  The date read never fetches a rich parent event to render an exact date.
  None of them reads the clock evidence; the JSON read picks one row of it
  in three indexed probes, whatever the rule.
- **Writes reused, none added**: `PUT /events/:id/dates/:date/registration`,
  `POST /events/:id/dates/:date/decline` and `/block`,
  `PUT /events/:id/registrations`, `POST /events/:id/series-registrations`
  and `/undo`, and `POST /events/:id/guest-registrations`. No CSRF exemption
  is added.
- **Hidden groups**: event output never shows a hidden group's name, place or
  id (`a.groupShownTo`, `labelGroup`). A new error a person can act on
  follows DEC-61 (`apiErr` plus `ERROR_COPY`); everything else uses `fail`.
- **One additive migration**, `00009_occurrence_series_starts.sql`
  (DEC-155): the clock evidence table and the events flag with its
  trigger. Up backfills nothing; Down drops the evidence, and the past and
  zero-DTSTART values it held come back only from a matching backup. No new
  Go or npm dependency, one embedded binary, the recovery level unchanged.

## 7. Slices (in order, each checked and reviewed on its own)

1. **R15-1 lazy split** (M7.9). Measure, make `GroupDetail` and
   `EventDetail` lazy, add the route error boundary, measure again and set
   the budget. Done before any M8.1 page edit.
2. **This brief**: this file, plus the M8.1 link in `phase-8.md`.
3. **Cancelled-date read and page** (2.2, 2.1). The tombstone in policy,
   the handler and the projector; the narrowed type; the page's tombstone
   view; the JSON-only exception in `docs/API.md`. It also exposes the
   ordinary viewer's own cancelled answer on a scheduled date (2.1),
   replaces `TestDatePageIgnoresCancelledRegistration`, and proves the
   three-way distinction (section 8).
4. **Authorized facts for the scheduled date** (sections 4 and 6). The date
   read's enrichment through `projectDate`, its date-page goldens, and its
   Go and TS contracts and docs.
5. **Confirm dialogs on the two routes** (2.5). Extract the
   `SeriesRsvpDialog` modal shell into `Dialog` and `ConfirmDialog`, and
   cover every confirm reached from `EventDate` and `EventDetail`, including
   the dirty-editor navigation block. Shared components keep native confirms
   elsewhere.
6. **First viewport** (sections 1, 4 and 5.1). The header, remote state,
   main status, attendance, separate capacity pools and the difference
   sentence.
7. **Sticky answer bar** (section 5.2). Series scopes and Undo, waitlist
   state and position.
8. **Running-app proof** (section 8).
9. **Merge gate**: `STATUS.md`, `docs/FIRST-CHURCH-CHECKS.md` and the M8.1
   completion record in `phase-8.md`, naming exactly which criteria are met
   and naming explicit confirmation (2.4) as unmet and outstanding. M7.9's
   status in `STATUS.md` names R15-1 as the completed slice; M7.10 stays
   untouched. `FIRST-CHURCH-CHECKS.md` gains the real-phone
   safe-area check and the live-redeploy check (DEC-128). The waitlist
   discrepancy is a product follow-up, not a device check.

## 8. Proof

- **Handler tests (PostgreSQL)** compare real JSON keys and values for an
  ordinary member, a manager, a signed-out reader, an invitation-only
  reader, an opened hidden-series date, and a visible series with a hidden
  sibling. They assert absences as well as presences:
  - The hidden sibling changes neither coverage count (the eligible
    denominator nor the viewer's covered dates).
  - An unauthorized reader's JSON has no protected attendee names, declined
    counts, exact childcare usage or remaining places, hidden-group identity
    (name, id, cover, leader) or organizer data.
  - The invitation-only payload has none of the new organizer, cover,
    attendance, capacity, childcare or series keys, even where the fixture
    has data for them.
  - For an ordinary reader, a church-wide or hidden-group date carries the
    church label and no named host person.
  - An invitation-only read of a hidden-group date with remote on for the
    event and an effective remote cap of 0 keeps today's `remoteAvailable`
    and video fields. It has no `HostedBy` and none of the new remote-state
    or capacity keys.
  - Row counts are separate from seats including guests, unlimited from
    full, remote from in-person, childcare-full from manager-only totals,
    and the viewer's own waitlist position from other rows. A failed count
    read is an error, never a successful zero.
  - Remote RSVP and link access are independent: one fixture closes remote
    registration for the date (effective remote cap 0) while the viewer can
    still see the link, and an ordinary manager sees a `registered_only`
    link without a Going answer.
- **Tombstone tests** (cancelled-date slice) compare its exact three keys.
  - Qualified: a registered reader, and a qualified invitee before and
    after cleanup. "Before cleanup" is a seeded unstamped manager-add
    grant on a cancelled date, plus an unstamped departure grant while the
    date's end is still ahead, because no writer commits that state.
    "After cleanup" goes through the real cancel. Also qualified: a
    departure grant ended by the real cancel, read again after the date's
    end has passed.
  - Denied with the existing unavailable response: an unrelated member; a
    signed-out visitor; a role alone; a pending request; someone else's
    row; a viewer whose only link is holding a household person's
    registration; a retained row whose only authority was a grant ended
    independently, with its RSVP status intact; a row cancelled by a
    visibility loss (`cancelDateLosers`); and invalid, deleted and sibling
    dates.
  - Also denied: cancel, restore and cancel again with no new grant. The
    restore goes through the real series edit, so `event.dates_restored`
    is written; a fixture that flips the status directly, as the existing
    lifecycle test does, proves nothing here. The same case still denies
    after two changes: the restore row's `at` is set earlier than the
    grant's `ended_at` while keeping its larger id, matching a restore that
    queued behind the cancellation; and the restorer's account is then
    erased.
  - Also denied: an ended grant with no matching end snapshot, or with
    two, or with a snapshot on its registration that fails
    `accessSnapshotValidSQL`; and a seeded departure grant still unstamped
    on a cancelled date, read after the date's end.
  - Evidence reader: a corrupt access snapshot on another registration
    leaves this qualified read unchanged. A failed snapshot or restore
    query answers the error, never the unavailable response.
  - Qualified after a retime: the real cancel stamps a departure grant,
    then the real per-date editor moves the cancelled date's end to
    before that stamp. The grant still qualifies.
  - Clock boundary, with an injected `App.clock`: a cancellation whose
    lifecycle clock is just before a departure date's end stamps
    `occurrence_cancelled` and qualifies. Through the manual date cancel,
    one whose clock is at the end either is refused or stamps
    `occurrence_ended`; no tombstone is given either way.
  - Both cancel writers (the series edit's cancel and the manual date
    cancel) leave no live grant on the date after commit.
  - One snapshot: a test hook between two queries of the cancelled
    outcome commits, from another connection, the viewer's account
    disable and a title edit on the cancelled date. The answer carries
    the snapshot's old title or is unavailable, never the new title.
  - Qualified again: cancel, restore, a new grant, then cancel again.
    The newest end snapshot is later than the restore, so a check that
    denies on any restore fails this case.
  - Also denied: an `occurrence_cancelled` grant whose holder has since
    lost teen consent, or whose invitation context has since changed, with
    the end reason still in place.
  - No denied case reveals the title or the cancellation. The qualified
    viewer still gets 404 on the ICS download.
- **Cancelled answer** (cancelled-date slice): the real JSON tells three
  cases apart: a scheduled date with the viewer's own cancelled answer, an
  unanswered scheduled date, and the tombstone. The cancelled row carries
  an attendance mode and guests in the fixture. Its JSON gives only the
  cancelled status, with no `myAttendanceMode` or `myGuestCount`, and the
  same capabilities as the unanswered date. The invitation payload and
  `calendar.ics` are unchanged.
- **Difference fixtures** cover later recurrences, a moved day, a
  duration-only change, a room replacement, a location fallback, either
  capacity override alone, equal-valued overrides, visibility changes and an
  event-zone DST boundary.
- **Vitest** covers each answer and each viewer variant, including a
  photo-less shown group with a leader, the no-cover cases, a differing
  viewer zone, every remote RSVP and link state on both axes (4.3), an
  invitee whose `remoteAvailable` is false with no "closed" wording, private
  names, both pools, permitted childcare, full and partial coverage, a
  one-off date, zero upcoming, Maybe, declined, waitlisted, both meanings of
  cancellation, and an overfilled invite-only fixture that still renders
  nothing restricted. Waitlist tests cover known and unknown position, both
  pools, full with no waitlist and the real promotion outcome. Their copy
  contains no deadline and no confirmation step.
- **Answers and Undo** (sticky-bar slice): invited edits, ordinary
  single-date answers, all three series scopes, a scoped series decline,
  refusals, mixed and waitlisted outcomes, successful Undo, Undo expiry and
  Undo after authority was revoked. Undo shows for a partial, refused or
  declined batch whenever a receipt came back. A successful batch with
  partial coverage never reports full coverage. A failure keeps the valid
  inputs.
- **Dialogs**: Escape writes nothing; a confirm writes exactly once; focus
  returns; busy and repeated activation are handled; a stale dialog is
  invalidated after navigation or an authority refresh.
- **Confirmation routes**: a source guard (`guards_web_test.go`) refuses
  bare and `window.`-qualified `confirm` and `alert` at the page sites of
  `/events/:id/dates/:date` and `/events/:id`. Route tests stub native
  `confirm` and `alert` to fail and exercise the shared confirmation
  paths through the member and manager views of those routes, including
  in-app navigation away from a dirty event editor (`unsavedChanges.tsx`),
  the over-capacity retry after an `event_full` conflict (`ManageRoster`),
  and the over-capacity approval prompt (`AddRequestForm`) where reachable.
  `GroupDetail`'s existing native-confirm tests still pass unchanged.
  In-app navigation uses the app's dialog; the browser's own tab-close
  prompt (`beforeunload`) stays the browser's.
- **Running app**: `web/e2e/run.sh` passes, including `smoke`, `deeplink`,
  `nav-shell`, `exact-date`, `waitlist`, `event-pages`, `a11y` and `layout`.
  A signed-in ordinary EventDate is registered in the shared page list
  (`web/e2e/ui-pages.ts`), so `a11y` and `layout` cover it the way they
  cover EventDetail. The exact-date scenarios (recurring event, visibly
  changed date, full date, invited viewer, cancelled date) are addressable
  from the independent verify-ui driver
  (`.cursor/skills/verify-ui/drive-ui.mjs`).
  At a documented 390px viewport, starting at `scrollY=0`, the bounds of all
  six answers lie inside the unobscured viewport on a visibly changed date.
  The bar is hit-tested and operated above the tabs after scrolling, and the
  last control is focused. The separate series URL (`/events/:id`) loads on
  its own and lists its dates. A waitlisted reader goes through the actual
  promotion, a qualified and an unqualified reader read a cancelled date, and
  a restricted invitee sees their own answer. verify-fold and verify-ui
  evidence names the role, the fixture and the viewport width and height,
  with first-screen and mid-scroll captures at 390, 768 and 1280. At all
  three widths there is no horizontal overflow, clipped control or
  target-size regression, and axe is clean at moderate and above for the
  touched routes and dialog states. Permission and tombstone checks inspect
  the real JSON as well as the page.
- **Gates**: the AGENTS.md pre-push commands, `make test`, web Vitest,
  contrast, the production build, bundle-size, `TestUIRatchets`,
  `TestWebTypesMatchGoJSON`, `TestDateProjectorGoldens`,
  `TestSeatSQLMatchesGoRules` and `TestCSRFExemptRoutesAreExact`. The
  real-phone safe-area check and the live-redeploy check go to
  `docs/FIRST-CHURCH-CHECKS.md` (DEC-128).

## 9. Open questions

None open. One outstanding follow-up: explicit confirmation, and when it is
due, before a waitlist promotion (2.4). It is tracked for M8.1's full completion and does
not block this flow.
