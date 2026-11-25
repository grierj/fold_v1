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

## A review's names stay on screen after the sender loses the right to send

- Date: 2026-11-24. Reviewed revision: `9bef52e2a9d13cdfc4272534d862691d540a1517` (PR #486).
- The composer keeps the review it last took (who gets the message, by name) so the picker and the
  confirmation show one list. When a leader or host loses the right to message that audience and the
  confirmation answers `message_not_yours`, the question closes but the picker goes on showing that
  review until the page is left.
- Likelihood: **2/10**. The right must be taken away between the review and the confirmation, and the
  person must stay on the page.
- Severity: **2/10**. The person saw those names while they were allowed to; nothing new comes from the
  server, and nothing is stored in the browser. Product: **4**. The issue is intentionally not fixed.
- Evidence: `reviewed` in `web/src/pages/MessageComposer.tsx`; `web/src/components/messages/AudiencePicker.tsx`.
