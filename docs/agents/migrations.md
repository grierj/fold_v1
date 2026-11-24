# Migrations

Read this before adding or changing a file in `migrations/`.

## Rules

- Every schema change is a **new** goose file `NNNNN_name.sql`, with Up and Down. Never edit a
  migration any tagged release has applied. The only exception was the pre-live baseline window in
  DEC-138; it closed with the first tagged release, `v0.1.0`, so no exception remains.
- The model change and `models.All()` go in the same PR. The migration files are the source of
  truth; GORM models must match them (the drift check in `migrations/migrations_test.go`).
- **Expand, then contract (DEC-138).** A release may add tables, columns, indexes and constraints
  and backfill data, but never drops or renames anything the previous release still reads or
  writes. Code in that release writes the new shape and reads new-then-old. The next release may
  drop the old shape: its migration has a `-- contracts: NNNNN_name.sql` header naming the expand
  it contracts, and CI fails when that expand is not in an earlier tagged release. There are no
  audit triggers on old columns and no waiting period: one binary runs at a time, so the previous
  release is the only old version that can still be running.
- Contract gate: `scripts/contract-gate.sh` in the test job checks the tag and file half (a `v*`
  tag merged into HEAD holds the expand and not the contract; the expand unchanged since).
  `TestCIRunsTheContractGate` keeps it in CI and `TestContractGateScript` proves it. The
  publication half is review only: a `release.yml` run, the finalize step summary, GHCR digest,
  release section and attestation naming one digest. A tag alone does not satisfy it.
  `00008_suppression_address_contract.sql` is the first contract file (it contracts `00002`, held by
  `v0.1.0`); the release evidence it relied on is in `docs/plan/M7.4-ERASURE-INVENTORY.md` 15.13.

## Two PRs with the same number

Two open PRs can pick the same next number; git merges both without a conflict and goose then
refuses to boot. Master's ruleset requires a PR to be up to date before it merges (DEC-146), so the
later PR merges master, fails `TestBaselineFileShape` with "duplicate version" and renumbers its
file. `TestDuplicateVersionNeverBoots` proves a duplicated tree cannot boot, migrate or pass the
release smoke.

If a duplicate reaches master anyway, renumber the file that merged later, in a new PR (never a
file a tagged release holds), and recreate every database that applied it under the old number:
goose tracks versions by number alone, so it would count the other file as applied and skip it.

## The chain

The chain is the generated baseline `00001_baseline.sql` (M7.7, DEC-138) plus additive numbered
files. `TestBaselineFileShape` checks it through `migrationFileShapeProblems` (baseline first,
then consecutive `NNNNN_name.sql`, each with Up and Down). Each numbered file's header says what
it is for.

- The baseline's Down destroys the schema and refuses while any user exists; it is for scratch
  databases and the down/up test, never application rollback. On any database that has run
  `00015` it fails with or without users (`recovery_floor_hold`); drop such a database to discard
  it.
- `00008` makes `suppressions.address_hash` NOT NULL and drops `address`. Its Down refuses while any
  suppression exists, because the addresses are gone; rolling back to a release that reads `address`
  restores the dump `upgrade.sh` takes (`docs/DEPLOY-DIGITALOCEAN.md` section 6).
- `00009` adds `occurrence_series_starts` (the series writer's clock evidence, DEC-155) and
  `events.series_clock_dirty` with the trigger that raises it on any UPDATE naming a rule column. It
  backfills nothing: the `occurrences.extend` job repairs upcoming dates once a worker runs it. Its
  Down drops all of it, even when populated; the past and zero-DTSTART evidence is then lost, and
  only a matching backup restores it. Direct reuse of the upgraded database requires a previous
  binary that already holds `00008` and meets the recovery floor in `docs/DEPLOY-DIGITALOCEAN.md`
  (from `00015` on, no earlier release starts on the upgraded database).
- `00010` adds `events.edit_revision`, `event_occurrences.edit_revision` and
  `event_occurrences.edit_receipt` for the guarded date edit, with BEFORE UPDATE
  triggers that add one to a revision when any tracked column changes (every
  column but the bookkeeping ones each trigger names) and clear the receipt. The
  counters are kept in SQL so a previous binary's writes count too. Up backfills
  nothing (existing rows read 1). Down drops only this bookkeeping, even when
  populated; counters then restart at 1, which is safe because the server binds
  every token to its own process, so no token from before the Down can match.
  Direct reuse requires a previous binary that already holds `00008` and meets
  the recovery floor in `docs/DEPLOY-DIGITALOCEAN.md` (from `00015` on, no
  earlier release starts on the upgraded database). Its writes advance the
  counters (proven with raw SQL in the migration test, not a previous binary).
  A binary that predates `00008`, including v0.1.0, requires the pre-upgrade dump.
  The server no longer reads or writes `edit_receipt` (DEC-164); the column and the trigger's
  clearing of it stay until a contract migration drops them, which the contract gate allows only
  once a tagged release holds `00010`.
- `00011` adds `push_subscriptions.binding_generation` with a BEFORE UPDATE trigger that adds one
  whenever the owner or a key changes and keeps the stored value otherwise, overwriting what a
  writer supplies, so a previous binary's writes count too. A push service's answer changes a
  device only while the generation it was sent at still matches (`docs/agents/push.md`). Up
  backfills nothing (existing devices read 1). Down drops only the counter and its trigger, even
  when populated; an answer in flight then belongs to a process that is gone, so nothing can
  match a stale generation. Direct reuse requires a previous binary that already holds `00008`
  and meets the recovery floor in `docs/DEPLOY-DIGITALOCEAN.md` (from `00015` on, no earlier
  release starts on the upgraded database).
- `00012` adds `jobs.claim_seq` and the sequence `jobs_claim_seq`. The runner's claim draws a new
  number every time it takes a job, and a job's outcome lands only for the claim that holds that
  number (`jobs.Claim`, PLAN 5.5); the worker label and the attempt count can repeat, so neither
  stands in for it. The sequence rewinds only with the database: `restore.sh` stops the app
  first, and Down is offline maintenance that must run with every server stopped. Up backfills nothing (a job a stopped process was running
  is taken back with a new number). A previous binary leaves the column alone and fences on
  `locked_by` as it did. Down drops the column and the sequence, even when populated, and changes
  no job. Direct reuse requires a previous binary that already holds `00008` and meets the
  recovery floor in `docs/DEPLOY-DIGITALOCEAN.md` (from `00015` on, no earlier release starts on
  the upgraded database).
- `00013` adds `message_recipients.audience_admitted_at`, set once by the writer whose date check
  let a direct recipient into an event audience (`isMessageRecipientIn`). Up backfills nothing:
  no stored row proves which check it passed, so every earlier row reads NULL, and a previous
  binary neither writes nor reads it (on a rollback its read page keeps its old rule). Down drops
  the column, even when populated, and changes no row. Direct reuse requires a previous binary
  that already holds `00008` and meets the recovery floor in `docs/DEPLOY-DIGITALOCEAN.md` (from
  `00015` on, no earlier release starts on the upgraded database).
- `00014` adds the `event_occurrences_kept_date_end` trigger (DEC-156). An UPDATE that makes a
  date end later fails with 23514 while the date has an unstamped access grant of any origin but
  `manager_add`. At any isolation level other than Read Committed it fails whatever the grants.
  It reads no clock and runs at the end of the statement, so it cannot tell a grant whose date
  has ended from a live one, and a writer that stamps the grant first in the same transaction
  passes. The guarantee covers writers that lock the date row before making a departure grant
  live, as Fold's departure does (`TestDepartureGrantsAreIssuedUnderTheDateLock`); it claims
  nothing about one that does not. The app never relies on it: every writer of a date's end
  refuses first with 409 `date_kept_after_leaving` (`TestEndsAtWritersCheckKeptDates`). Up
  stores and backfills nothing. Down drops only the trigger and its function. Direct reuse
  requires a previous binary that already holds `00008` and meets the recovery floor in
  `docs/DEPLOY-DIGITALOCEAN.md` (from `00015` on, no earlier release starts on the upgraded
  database), but that is not enough here:
  - Every earlier build lacks this release's date-privacy fixes.
  - On the upgraded database, the trigger refuses an earlier build's later end only while a
    departure grant on that date is live.
  - After the date's stored end, an earlier build stamps the lapsed grant and raises the end in
    one transaction, which the trigger permits. Its notices and readers can then show the moved
    date to the person who left.
  - After `fold migrate down-one` the trigger is gone, so an earlier build that then starts can
    move a kept date later while the person still holds it.
  - `upgrade.sh --rollback` restores a dump that never had the trigger, and people removed from a
    kept date since that dump have it again.

  When that privacy matters, none of these is recommended: direct reuse with an earlier binary,
  `fold migrate down-one` followed by an earlier binary, or `upgrade.sh --rollback`. Keep this
  release. `fold migrate down-one` followed by this release's binary is different: the binary
  adds the trigger back when it starts (`openDatabase` runs the migrations), and its own check
  refuses the change before then. While the trigger is present, it refuses every later end an
  earlier build attempts on a database whose default isolation is Repeatable Read or Serializable,
  Remove or not. That restriction no longer applies after down-one or scripted rollback removes
  the trigger. `TestKeptDateEndMigration` proves the trigger with raw
  SQL standing in for each writer's shape; no earlier binary runs in it.
- `00015` keeps every earlier release off a database this release ran (DEC-158). Direct reuse
  with an earlier binary is impossible by design: it exits at start. This deliberately ends the
  expand-then-contract premise that the previous release may start again on the upgraded
  database; `00015` adds and drops nothing a previous release reads or writes, so the contract
  gate does not apply. Do not "fix" the start fence to restore direct reuse.
  - **Start fence** (`settings_recovery_floor`, BEFORE INSERT OR UPDATE on `settings`): a write
    of the `restore_fence` row (new or old key) fails with 55000 and the operator message
    ("recovery level 3 or later (exact visibility consent, DEC-158)", "restore data only with its
    restore.sh --dump") unless the transaction set `fold.recovery_level` to an integer of 3 or more.
    Every `fold serve` since the restore barrier writes that row before it serves, and this
    release declares its level first (`declareRecoveryLevelIn`); so an earlier release exits at
    start. It does not reach a process that already booted.
  - **Audit fence** (`audit_logs_visibility_consent`, AFTER INSERT on `audit_logs`): it fires for
    a visibility cancellation (`registration.cancelled` with reason `event_more_private`) and for
    every `event.visibility_changed` or `event.visibility_override` row, whatever its target,
    reason or meta. A cancellation passes only when the transaction declared
    `fold.visibility_consent = 'v1'` (a consent check passed); a visibility row passes with `'v1'`
    or `'v1:no-review'` (the consent rule ran and nothing needed a review). Anything else is 23514
    "a visibility change can be saved only by a Fold release that checks visibility consent
    (DEC-158) …", so an earlier process still running changes no event or date visibility and
    cancels nobody for one. Every writer of such a row in this release runs `checkConsentIn`
    first (`TestEventVisibilityAuditWritersDeclareConsent`); a narrower shape (v6 matched only a
    date reset) left an earlier process's stale empty reset free to open a series. Both settings
    are declarations, not secrets; neither proves what a check compared (the server's checked
    plan does).
  - **The hold** (`recovery_floor_hold`, a view over both tables, always empty, no reader): the
    baseline Down's plain `DROP TABLE` of `audit_logs` fails with 2BP01 ("cannot drop table
    audit_logs because other objects depend on it") and rolls back whole, with or without users.
    It references no column, so later column changes are free; a later migration that must
    rebuild `settings` or `audit_logs` recreates the hold in the same file. Drift, the erasure
    scans and the table counts ignore it (it is a view).
  - **The epoch** row `visibility_consent_epoch` is inserted here and rotated by every boot.
  - **Down** has no statements, on every database, users or none: a user count cannot tell a
    scratch database from an install before its first setup. goose deletes version 15 and keeps
    everything; the next Up (`CREATE OR REPLACE` throughout, `ON CONFLICT DO NOTHING`) finds it in
    place. A database that ran `00015` is discarded by dropping it. Tests that need a pristine
    schema call `releaseRecoveryFloor` (`migrations/testhelpers_test.go`), which refuses any
    database `testdb.Fresh` did not create; `TestRecoveryFloorIsDroppedOnlyByTheTestCleanup`
    keeps every other file from dropping or disabling the fences.
  - **For developers:** an older checkout, or a branch from before `00015`, refuses to start on
    a dev database that ran this branch, even after `fold migrate down-one`. Recreate that
    database (`docker compose -f deploy/docker-compose.dev.yml down -v`) or run this branch.
  - `TestVisibilityConsentFenceMigration`, `TestVisibilityConsentFenceInFlight`,
    `TestRecoveryFloorSurvivesTheWholeChainDown` and the real-binary
    `TestPriorReleaseRefusesToStart` and `TestPriorProcessRunningAcrossTheUpgrade` prove it.
- `00016` adds `groups_status_check`: `groups.status` is `pending`, `active` or `archived` (NULL stays
  allowed). A plain `ADD CONSTRAINT` checks existing rows under the table lock it already holds; a
  row with any other value fails Up, which rolls back whole at version 15. Every release writes only
  these values, so a previous binary keeps working on the new schema. Down drops only the CHECK and
  changes no row. Direct reuse with an earlier binary is still ruled out by `00015`.
  `TestGroupStatusCheckMigration`, `TestGroupStatusCheckRefusesAnUnknownStoredValue` and
  `TestGroupStatusCheckWaitsAtMostFiveSeconds` prove it.
- `00017` adds `groups.edit_revision` for the group form's save (DEC-166), with a BEFORE UPDATE
  trigger that adds one when any column but `updated_at` and the counter changes and keeps the
  stored value otherwise, overwriting what a writer supplies. The counter is kept in SQL so a
  previous binary's writes, and the cover endpoints' `image_url` writes, count too. Up backfills
  nothing (existing groups read 1). Down drops only the counter and its trigger, even when
  populated; counters then restart at 1, which is safe because the server binds every token to its
  own process, so no token from before the Down can match. Direct reuse with an earlier binary is
  still ruled out by `00015`. `TestGroupEditRevisionMigration` proves it with raw SQL standing in
  for each writer.
- A development database migrated by the pre-squash chain must be recreated before a new binary
  runs on it; there is no converter.
- `scripts/baseline-proof.sh proof` re-proves a tree against the frozen pre-squash chain with a
  normalized `pg_dump` diff, on a scratch server only. Its `--candidate-tree` must hold only
  `migrations/00001_baseline.sql` (for example a `git archive` of a commit before `00002`).
  `--help` lists the arguments. Background: `docs/plan/briefs/audit-2026-11/R9-baseline-squash.md`.
