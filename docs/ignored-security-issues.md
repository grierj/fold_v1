# Intentionally ignored security issues

Record security review findings here when likelihood multiplied by severity is 4 or less.
For each finding, include its date, affected code or behavior, evidence, likelihood and
severity scores with their reasoning, the product, and the reviewed revision. Do not include
credentials, personal data, or exploit instructions.

## Request body retained by a non-cooperating transport

- Date: 2026-11-23 (first recorded 2026-11-21). Reviewed revision:
  `74a31f4b34715056167cf6e7e508c80f52fc692f` (PR #467; this entry was added in the commit after it, which changes
  nothing else).
- A `fetch` that never settles can keep the serialized date-edit body (`Events.editDate`) in its pending frames for as
  long as the page lives. The body can contain private text (a title, a place, a visibility reason). No abort signal is
  supplied now, so nothing asks the transport to drop it. The attempts store and the cross-page recovery the first
  record described are gone (DEC-164), so nothing else in the app keeps or resends the body. The review found no
  cross-user disclosure or continuing authority.
- Likelihood: **1/10**. A browser's own fetch settles or fails; this needs a transport that holds a request forever.
- Severity: **4/10**. Private text may remain in the same browser process, but there is no publication or replay path.
  Product: **4**. The issue is intentionally not fixed.
- Evidence: `web/src/api/events.ts` (`editDate`) and `web/src/api/core.ts` (`api`, `send`).

## A redundant group status is accepted from a leader

- Date: 2026-11-22. Reviewed revision: `b2303a962528189bd66cea4fd0ea277d4336f6da`.
- A leader who sends the status their group already has, along with other edits, gets 200.
  Before DEC-160 any status a leader could not set answered 403, the current one included.
  The edit already succeeded with the status left out, so the request gains nothing and reads
  nothing new.
- Likelihood: **1/10**. Only a hand-made request sends it; the web client sends no group status.
- Severity: **1/10**. No capability or disclosure: the caller already manages the group.
  Product: **1**. The issue is intentionally not fixed.
- Evidence: `policy.DecideGroupStatus` in `internal/policy/policy.go` and `apiUpdateGroup` in
  `api_groups.go`.

## Custom booking times accepted from an admin demoted while the pick waits

- Date: 2026-11-24. Reviewed revision: `bee5ff83b539e4a52c42b86a1a0c5bfee2b95c43` (PR #483).
- `apiSetDateRoom` judges `policy.CanAdjustBookingPadding` on the request's actor before it waits for the
  event's lock. An admin who is demoted during that wait and still leads or hosts the event keeps
  event-management authority under the lock, so the setup and cleanup minutes already accepted are written
  for that one booking.
- Likelihood: **1/10**. It needs a demotion committed while a custom-times pick is waiting on the lock, and
  the person must remain a leader or host of that event.
- Severity: **3/10**. One booking's setup and cleanup window; no disclosure, and the person may still manage
  the date's room. Product: **3**. The issue is intentionally not fixed.
- Evidence: `apiSetDateRoom` in `api_room_bookings.go`; `lockManagedEventIn` in `api_events.go` reads the
  actor after the lock for the authority decision only.

## Repeated room changes send repeated notices to an event's other runners

- Date: 2026-11-24. Reviewed revision: `57cbf523d7b6ea966b294cee1afc2f2ae760f526` (PR #491).
- Each real change of an event's room plans writes one `room.changed` notice to the event's other leaders
  or hosts, and its email where they allow event email. A leader, host or admin who changes a room over and
  over sends as many. Push has its own limits; email is not coalesced.
- Likelihood: **2/10**. The sender must be someone trusted to run the event and must make real changes
  repeatedly.
- Severity: **2/10**. The recipients are the event's few runners, and every change is audited.
  Product: **4**. The issue is intentionally not fixed.
- Evidence: `notifyDateRoomChanged` and `notifySeriesRoomChanged` in `notices_rooms.go`.

## A disabled host of a church-wide event still gets a stored room notice

- Date: 2026-11-24. Reviewed revision: `57cbf523d7b6ea966b294cee1afc2f2ae760f526` (PR #491).
- `eventRunnerIDs` leaves out deleted hosts but not disabled ones, so a disabled host of a church-wide event
  gets a stored `room.changed` row, as they already get `room.lost`. Email and push check the account's
  status at delivery and send nothing.
- Likelihood: **3/10**. A disabled account can keep its host rows while another runner changes the room.
- Severity: **1/10**. The person cannot sign in; if the account is enabled again they are still a host of
  that event. Product: **3**. The issue is intentionally not fixed.
- Evidence: `eventRunnerIDs` in `api_registrations.go`; `lockRecipients` in `internal/notify/notify.go`.

## A leader removed while a room change commits can get that one notice

- Date: 2026-11-24. Reviewed revision: `57cbf523d7b6ea966b294cee1afc2f2ae760f526` (PR #491).
- The recipients of `room.changed` are read inside the room change's transaction. A leader whose removal
  commits after that read and before the change commits still gets the notice.
- Likelihood: **1/10**. The removal must land inside one request.
- Severity: **2/10**. The person ran the event a moment earlier and learns its title, who changed the room
  and the date. Product: **2**. The issue is intentionally not fixed.
- Evidence: `roomChangedTo` in `notices_rooms.go`.

## A household adult can tell that an unseen part of a person's row changed

- Date: 2026-11-25. Reviewed revision: `597decf654aa0cde98f305de00123ef99aa03d7b` (PR #552, DEC-174).
- A household adult who is not a person's confirmed guardian cannot read the person's care notes but holds the
  person's revision. If the revision moves, or a save is refused with `household_person_changed`, while every field
  they can see is unchanged, they learn that something they cannot see changed. It reveals no content. It cannot tell
  a care-note change from any other write of the row, since the notes stay out of the digest and every write stamps
  `updated_at`.
- Likelihood: **2/10**. It needs a second adult watching the revision or saving while a guardian edits the notes.
- Severity: **2/10**. The adult learns only that the row was written, not what changed. Product: **4**. The issue is
  intentionally not fixed.
- Evidence: `householdPersonRevisionRows` in `edit_revision.go`; `checkHouseholdPersonRevision` in
  `apiUpdateHouseholdPerson` (`api_households.go`).
