# Web push

Read this before touching push keys, devices, push delivery, quiet hours, announcement pushes,
push reminders or the service worker. Background: M3.13, DEC-121 to DEC-129,
`docs/plan/research/web-push.md`.

## Keys and sending

- The VAPID key pair is made on boot under the migrations advisory lock and kept in `settings`
  (`vapid_public_key`, `vapid_private_key_enc`, AES-GCM with a `SESSION_SECRET`-derived key;
  `push_keys.go`). A secret change or `fold push keys rotate` makes a new pair and deletes every
  `push_subscriptions` row; a running server rereads the row on each send.
- `internal/push` is stdlib only and sends only to the allow-listed push hosts
  (`push.DefaultHosts` plus optional `FOLD_PUSH_HOSTS`) over https, with resolved-IP checks, no
  proxy and no redirects.
- **An endpoint is a capability:** never log, return or put in an error an endpoint, `p256dh`,
  `auth`, the VAPID token or a body (`jobs.CleanError` strips URL paths and tokens as a backstop;
  `TestPushLogsKeepSecretsOut`).
- Pushes share `pushTarget`, `pushHeld` (church switch, active account, consent), `pushOpensAt`
  (quiet hours, at least a second out) and `sendPush` (`push_deliver.go`). A notice's, an
  announcement's and a reminder's push differ only in what they check before and what they say.
- An announcement push (after quiet hours and the hourly limit) and a push reminder read their
  destination (`pushDestinationIn`: the account, the church's switch, consent and the device's
  owner), decide their date authority and build their payload in one read-only Repeatable Read
  snapshot (`announcementPushIn`, `planReminderPushIn`), then record or call the push service
  after it ends: no network call under a transaction, and no lock beyond the claim. The device
  sent to is the one that snapshot read, so a shared browser rebound to another account, or a
  device or consent turned off, before it began never gets the payload; a change committed after
  it began can't stop that one send. The earlier checks only spare quiet hours and the hourly
  limit a push that can't go. An event audience's push goes only while a date the person may
  still see puts them in it (`audienceSeers`), counting a series from the message's stored
  scheduled time, as email and the read page do. An announcement push reads the message again
  in its snapshot (`messagePushColumns`): Try again moves `scheduled_at` without touching push
  rows, so a push still waiting counts from the cutoff Try again set. A reminder keeps its stored
  date reference; its push reads the current date and effective title in the snapshot.
- What the push service answers changes a device only as it was dispatched (`dispatchedDevice`:
  row, owner, keys and `binding_generation`). The generation goes up by one on every change of
  owner or key, kept by a trigger (migration `00011`) so every writer advances it; it never
  repeats for a row. A browser handed to another account and back, or renewed and given its old
  keys again, counts as changed; no timestamp stands in for it, because a clock reading can
  repeat. A browser rebound, bound back or renewed with new keys while the answer was on its way
  is not deleted on "gone", marked failed or stamped used. A "gone" about a changed device
  retries while attempts remain.
- An attempt that ends without delivering for a reason about its device alone (gone, moved or
  turned off before the job ran, refused, a last attempt that never reached it) goes through
  `settlePushMiss`, for notices, announcements and reminders alike: it leaves the row only to
  another of the person's devices' `push.deliver` jobs for that row that will still run
  (`pushSiblingWaiting`, `jobs.WillRunAgain`: queued, including one gone back to wait for quiet
  hours, or waiting to retry). A device saved after the jobs were queued has no job, and a job that
  ended takes nothing, so neither keeps the row waiting. Any other running sibling decides for
  itself; a delivery upgrades any earlier outcome to sent, so two last attempts never each wait
  for the other. Otherwise the row is failed when the device is still theirs, and "device gone"
  when it moved to someone else or is gone. A notice's push to a device that is gone or another
  account's is never sent there and is settled the same way.
- An announcement job that goes back to wait for quiet hours (`pushWaitIn`) locks the person's
  account first (`FOR SHARE`, the account-first order erasure and status changes take), then
  writes the row's "sending at" reason and queues its own job again for the opening
  (`jobs.LaterIn`: queued, attempt given back) in one Read Committed commit. It puts a device miss
  (`pushDeviceMisses`) another job recorded back to queued only while the person is still active,
  read after any erasure or status change holding the account has committed; a sent row never
  changes. `jobs.LaterIn` changes the job only while the handler's own claim holds (`jobs.Claim`:
  the row and its `claim_seq`, which never repeats, even when a worker label is reused, a wait
  gives its attempt back or an admin retries); after a lost lease it returns
  `jobs.ErrClaimLost`, nothing commits and the runner records nothing. Once the wait commits the
  handler returns `jobs.Requeued`, and the runner writes nothing: the job may already be claimed
  again for the opening. A worker that stops after the commit leaves a queued job, not a
  running one for lease recovery to spend an attempt on. Settlement locks the row first
  (`settlePushMissIn`), so whichever commits first, the other sees it. A notice has no queued
  state: a notice job waiting for quiet hours returns `jobs.Later` alone, so a sibling's miss can
  show until the morning delivery upgrades it to sent. The daily `reapMessagePushes` remains the
  backstop for an announcement row with no recorded outcome or job left.
  It reports delivery as unconfirmed because the service can accept the push
  before Fold saves the outcome. Fold does not resend it.
- `push.deliver` is one job per device.

## What a push may say

- A kind pushes only with `AllowPush` and its own short `PushRender` (stored on the row, never the
  bell body cut short). Safety kinds get the safe line, titled with `site_name` at send (DEC-134).
- A `PushRender` that returns no title means that row does not push: `room.changed` pushes only
  when dates were left needing a room (`Vars.NeedRoom`, DEC-170).
- No push shows a place (DEC-136), contact details, signed links or a hidden group's name
  (`TestEveryKindsPushTextIsLockScreenSafe`).

## Timing

- Quiet hours (DEC-126): an optional push outside the recipient-local window (`push.Window`)
  returns `jobs.Later` and reruns every check when it opens. Required kinds and `PushSoon` date
  changes within 24 hours go at once. More than 20 optional pushes an hour are `suppressed`
  "hourly limit".
- Announcement pushes (`messages_push.go`) are `message_recipients` rows with
  `channel = 'push'`. A message's status, counts, settling, reaper and Try again read its email
  rows only, so **filter `channel = 'email'` in any new query over a message's rows.**
- The 2-hour push reminder (`reminders_push.go`) is its own reminder message (key
  `push:<occ>:<start>:120`) with only push rows and `messages.channel = 'push'`.
  `reminders.sweep` plans each person with `Window.LastAllowed`, queues `push.deliver` at that
  time, and the job rechecks the date, the event's `reminder_push`, going, visibility and the
  window (15 minutes' grace); the hourly limit never stops it. Reminder keys are `reminderKey`
  values (`reminder_key.go`) and the one SQL expression `reminderKeySQL`.

## Devices

- A trigger deletes an account's devices when its sessions are revoked or it stops being active,
  and queues `push.devices_off`, which writes the person a bell notice (renaming that job type
  needs a new migration; `job_identity_test.go` pins it).
- Holding an endpoint alone never moves a device to another account: its keys must match.
- A device belongs to the person, not the session: an expired session clears the icon count but
  leaves the subscription, and the card re-saves it on load only for the user in
  `localStorage["fold.pushOwner"]` (wiped at logout).

## Browser side

- `web/src/sw.ts` builds to `/sw.js` (served `no-cache`, registered only by the card's "Turn on
  notifications", never on load; it skips waiting and claims tabs). It has **no `fetch` handler**,
  so it can never serve a stale shell after logout.
- `/manifest.webmanifest` comes from Go (`internal/httpx/manifest.go`, church name); the icons
  come from `npm run icons`.
- The card's state code is `lib/pushState.ts`; `lib/push.ts` is the eager part (logout, the icon
  badge).
- The church's switch (`push_enabled`, Admin → Settings) hides the card and the public
  `/notifications/setup` page (via `/push/config` and `/site` `pushEnabled`) and stops every send.
- To try push under `make dev`, run `cd web && npm run build` once first: the Go server serves
  `/sw.js` from `web/dist`.
