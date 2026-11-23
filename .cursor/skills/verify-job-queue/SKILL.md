---
name: verify-job-queue
description: Model-check Fold's background job queue (internal/jobs) with TLA+ and TLC. It covers claims, leases taken back, claim_seq fencing, Later/LaterIn/Requeued, death and OnDead, and an admin's Retry and Dismiss. Use when a change touches internal/jobs/jobs.go or dead.go, the admin Retry/Dismiss path, a handler that calls jobs.LaterIn or returns jobs.Requeued, anything else that writes the jobs table, or a migration that names it.
---

# Verify the job queue

Every email, push, notice, reminder and erasure step in Fold runs as a row in
the `jobs` table (PLAN 5.5). Any number of workers, in any number of processes,
claim rows. A worker that stops or stalls loses its lease, and another worker
takes the job back. The tests in `internal/jobs/jobs_test.go` pin specific
orders of events. This model checks **every** order of claims, lease expiry,
handler outcomes, crashes and admin actions for one job, within the bounds
below.

## Run it

From anywhere in the repo:

```bash
.cursor/skills/verify-job-queue/check          # drift, then the model
.cursor/skills/verify-job-queue/check breaks   # each guard's break must fail
```

It needs Java 11 or later on PATH. On the first run it downloads
`tla2tools.jar` 1.7.4 from the tlaplus GitHub release into `~/.cache/fold/`
and publishes the download only after its SHA-256 matches the pin in `check`.
Set `FOLD_TLA_JAR` to an absolute path or a path relative to your working
directory to use an existing copy. The same pin applies. TLC runs in a scratch folder under
`~/.cache/fold/`, never in the repo or `/tmp`: liveness checking writes its
graph to disk and can fill a tmpfs quota.

Other subcommands: `check drift`, `check model`, `check accept` (see
"Updating the model").

## When to run it

Run `check` when a change touches any of these:

- a function listed in `coverage.txt` (the `func` lines);
- the set of functions in `internal/jobs` (`decl` lines): a new state change
  there shows up even before anyone adds it to the model;
- a file that writes the `jobs` table;
- a migration that names `jobs` (`migration` lines);
- a handler that waits with `jobs.LaterIn` or returns `jobs.Requeued`.

The drift check hashes each covered function's `go doc -u -src` text, including
comments. The writer and waiter scans are heuristics. A `writer` line counts
lines matching common SQL writes or a Go job literal. A read can match, and a
write with a different spelling can pass unnoticed. A `waiter` line records a
matching filename, so a second caller in the same file can pass unnoticed.
Review changes to these paths even when the manifest matches.

## The rules

Each rule is an INVARIANT or PROPERTY in `JobQueue.tla`:

| Rule | In plain words | Harm if broken |
|---|---|---|
| `TwoRunsOnlyAfterALeaseRunsOut` | A job's handler runs in two places at once only when the earlier run's lease ran out and another worker took the job back. | A notice or email goes out twice with no slow worker to blame: a stale worker's outcome reopened a job someone else still held. |
| `AttemptBudget` | Each retry round has a budget of `max_attempts`. A start counts against the round that issued its claim, and a wait refunds it. A stalled earlier claim can start during a later round. | A failing job keeps retrying (and keeps sending) past its limit. |
| `OnDeadAtMostOncePerDeath` | Each committed death permits at most one `Runner.OnDead` call. A crash after the commit can lose the call. | Duplicate or false "a task failed" alerts for admins. |
| `FinishedStaysFinished` | A done or dismissed row stays terminal until pruning deletes it. A worker with an earlier claim can still start its handler. | A terminal row becomes eligible for a new claim. |
| `RunningEnds` (liveness) | No job stays `running` forever: a stopped worker's lease runs out and the job is taken back. | Notices silently never send. |
| `WaitingIsPickedUp` (liveness) | A queued or failed job whose time comes is claimed. | Same as above. |

`TypeOK` and `AttemptsNeverNegative` (the `jobs_attempts_check` CHECK) are
sanity checks, not rules: every give-back lands only on a running row, and a
row is running only after a claim added one, so no single removed guard breaks
`AttemptsNeverNegative`. A model change that does is a modeling error.

## What each action stands for

One action is one statement or one transaction in the code. If the code splits
or merges one, the model must change to match.

| Action in `JobQueue.tla` | Code |
|---|---|
| `Claim(w)` | `Runner.RunOnce`'s claim `UPDATE jobs ... WHERE id = (SELECT ... FOR UPDATE SKIP LOCKED)`. Queued/failed and due, or running with an expired lease. `attempts + 1`, new `claim_seq`. |
| `Held(w)`, `Land` | `heldBy`: `id = ? AND status = 'running' AND claim_seq = ?`. `WithClaim` passes the claim to the handler so `LaterIn` checks the same identity. `holder` stands for `claim_seq` (see the module header). |
| `DieOnClaim(w)` | `RunOnce`'s `job.Attempts > job.MaxAttempts` case and its unknown-kind-past-grace case, both `finish(..., true)`. |
| `WaitForABuild(w)` | `RunOnce`'s `!known` case: requeue an hour out, `attempts - 1`. |
| `StartHandler(w)` | `safeRun` calling `Kind.Run`. |
| `HandlerReturns(w)` | The handler returns nil, an error or `jobs.Later(at)`. |
| `HandlerWaitsIn(w)` | The handler's transaction with `jobs.LaterIn` (`laterUpdates`), e.g. `pushWaitIn` from `App.deliverMessagePush`. On `ErrClaimLost` it rolls back. |
| `ReturnRequeued(w)` | The handler returns `jobs.Requeued()` after that commit; `finish` writes nothing. |
| `Finish(w)` | `Runner.finish`: done, failed (backoff), dead at `max_attempts`, Later, or nothing for `ErrClaimLost`. |
| `Dies(w)`, `CallOnDead(w)` | The dead update commits first. When it affected one row, the worker can then call `Runner.OnDead`. Retry or Dismiss may intervene. |
| `Crash(w)` | The worker's process stops after its claim, including between the committed death and the hook. A restarted process is an idle worker again (`Runner.Start` keeps polling; `safeRun` turns a panic into an error). |
| `LeaseRunsOut` | `locked_at < now() - Lease`. |
| `RunAtArrives` | `run_at <= now()` (backoff, Later's time, an hour for an unknown kind). |
| `GraceEnds` | An unknown kind's `created_at` falls a week behind (`unknownKindGrace`). |
| `AdminRetries` / `AdminDismisses` | `jobs.Retry` / `jobs.Dismiss` via `decideDead`, called from `App.decideDeadJob`. |
| `PruneDone` | `jobs.PruneDone`. |

## Bounds and what is not covered

`JobQueue.cfg` checks every rule, liveness included: 3 workers, `max_attempts`
2, 1 admin retry, 1 crash, 2 leases running out under a live worker. `JobQueueDeep.cfg` checks the safety rules with worker
symmetry: `max_attempts` 3, 2 retries, 2 crashes, 3 slow-worker lease losses. Each worker independently does or doesn't know the
job's kind (`known[w]`), as when an upgrade is rolled back on some processes. A pass covers those bounds, not every size.

Assumed, not checked:

- **One job.** Rules are per job, and claims of other rows don't touch this
  one (`SKIP LOCKED`, one row per `UPDATE`). Enqueue, `unique_key` dedupe
  (`EnqueueMany`, the `push_devices_end_with_sessions` trigger), ordering
  between jobs and the payload rewrite in `00008` are not modeled.
- **Each statement is atomic** as Postgres runs it (Read Committed, the claim
  subquery's `FOR UPDATE` rechecks the row).
- **Handlers keep their contract**: they eventually return or their process
  stops, and they return `Requeued()` only after the `LaterIn` transaction
  committed. A handler that breaks this is not caught here.
- **Time is abstract.** Backoff lengths, the 15-minute lease and the hour/week
  for unknown kinds are only "it arrives eventually". A stalled worker's lease
  runs out at most `MaxSlow` times, or attempts would be unbounded (two stalled
  claimers could trade the job forever). A stopped worker's lease always runs
  out.
- **Handler side effects are out of scope.** Two runs after a lease runs out
  are allowed by design (`Kind`: "must be safe to run twice"). That includes a
  stalled worker that starts its handler after the job was already finished by
  the worker that took it back, and a stale run still sending while an admin
  dismisses the job (`OnDismiss` settles the notice row meanwhile). Whether
  each handler is idempotent is that handler's tests' job.
- **The alert itself.** `App.alertDeadJob` (its `LockDead` recheck and open
  task check) is not modeled; the model counts `OnDead` calls only.
- Not a rule: `attempts` can pass `max_attempts` by the number of claimers that
  stalled between a lease takeover and their dead write. Such a claim never
  runs the handler.

## Reading a result

Read **every** TLC result with the pstack `/statechart` skill. A failure gives
the shortest path to the broken rule in plain words. A pass can be drawn as a
chart of how a job moves between states. Point it at `JobQueue.tla` and the
config that ran.

- **`drift: ...`**: see "Updating the model".
- **`a rule broke`**: TLC prints the trace. Turn each step into the code's
  words with the action table. Then decide:
  - The path is possible in the code, and the model matches the code at every
    step: **it is a real bug.** Report it with the path (for example "worker A
    claims; its lease runs out; B takes the job back; A's stale write lands").
    Do not weaken the rule or the model, and do not change product code unless
    asked.
  - A step does something the code cannot (two statements merged, a guard the
    code has but the model lacks, a bound too small for the path): **it is a
    modeling error.** Fix the model to match the code, and cite the line.
- **`TLC did not finish`**: a parse error, a missing Java, a full disk. Fix
  the setup or the spec text; no rule was judged.

## Updating the model

When a change to covered code is intended:

1. `git diff 25c4e87bcaf501e281ac04a660a238c244580a35 -- <file>` for each
   drifted item. Find its action in the table and change the action to match
   the code: the same guard, the same atomic step.
2. If the change adds a guard or a promise, add a rule for it, and a `Break`
   value that removes the guard, with an entry in `breaks_list` in `check`.
3. `check model` must pass, or report what it found (above). `check breaks`
   must show every break failing its rule. A break that now passes means the
   model stopped seeing that guard.
4. `check accept` rewrites `coverage.txt`. Then move the sync commit below to
   the commit you confirmed against. Move it **only** after confirming that the
   model describes the code at that commit. Never run `accept` just to clear
   drift.

For a new writer, migration or waiting handler, check the model assumptions
and update the covered function list when an action depends on its body.

## Breaks (each must fail)

| `Break` | Go guard it removes | Rule that fails |
|---|---|---|
| `no_claim_seq` | `claim_seq = ?` in `heldBy` | `TwoRunsOnlyAfterALeaseRunsOut` |
| `no_stopped_check` | `job.Attempts > job.MaxAttempts` in `RunOnce` | `AttemptBudget` |
| `on_dead_unchecked` | `res.RowsAffected == 1` before `callOnDead` in `finish` | `OnDeadAtMostOncePerDeath` |
| `retry_unchecked` | `status = 'dead'` in `decideDead` | `FinishedStaysFinished` |
| `no_lease_recovery` | the expired-lease branch of the claim's `WHERE` | `RunningEnds` |
| `no_failed_claim` | `failed` in the claim's `status IN (queued, failed)` | `WaitingIsPickedUp` |

The real-code path was proven at `19d6879a`: dropping `claim_seq = ?` from
`heldBy` in `internal/jobs/jobs.go` made `check drift` fail on `heldBy`, and
mirroring it in `Held(w)` made `check model` fail `TwoRunsOnlyAfterALeaseRunsOut`
in 7 steps. The pstack `/statechart` skill read that break's spec and config
unchanged and gave the same path.

## Coverage record

- Sync commit: `25c4e87bcaf501e281ac04a660a238c244580a35`. The model was
  confirmed against the code at this commit. `AttemptsUsed` caps the number
  reported by the dead-job hook and log. The stored claim count, transitions
  and conditions for invoking the hook are unchanged.
- Covered items, with hashes: `coverage.txt`, written by `check accept`.
- Result at the sync commit: every rule holds on both configs, and all six
  breaks fail.
