# Fresh start: what I would build if Fold began today

Written 2026-11-26 against master `f69b436d`. Grier asked: with every decision, the architecture,
the language and the feature list open, and with what the project has learned, how would you start
again to make the best version of what Fold is trying to be?

This is a research document. It owns no product policy: `PLAN.md` and the decision log win until
Grier decides otherwise. Section 9 lists the main decisions it would overturn; I read the decision
log as a digest, so a smaller one may be missing.

**How to read the evidence.** "Measured" means counted in this repo on the day of writing (the
command or file is named). "Inferred" means my judgement from reading. Nothing here is usage data,
because there is none: **I am assuming no church runs Fold yet.** `FIRST-CHURCH-CHECKS.md` still
describes its checks as ones to run "when the first church starts using Fold", and 73 of its 94
boxes are unticked. If a church is live, sections 7 and 8 change.

---

## 1. Verdict

Fold's product idea is right and its decisions are mostly good. The codebase is the wrong shape for
the product, and the way the work is reviewed keeps making it bigger.

I would start again, in Go, carrying over the decisions and the small pure packages, and change
four things:

1. **One writer.** A single SQLite file, written by one process, one write at a time. Much of
   Fold's hardest code exists to make concurrent writers safe. A church of 500 does not need
   concurrent writers.
2. **One program that renders pages.** Server-rendered HTML with small page updates. No JSON API,
   no separate React client, no second copy of every type, error and rule.
3. **Personal data in one place.** So deleting a person is one small function, not a subsystem.
4. **A church before more depth.** Deploy in the first slice, put a pilot church on it within about
   a month, and let what they hit decide what gets built.

Then I would cut the feature list to what a member, a leader and a volunteer admin touch every
week, and add the four things that stand between a volunteer and a working site today: a setup
wizard that configures email, a spreadsheet import, attendance, and a group finder.

My target for the result (a target, not a measurement): under 25,000 lines of Go and templates,
about 25 tables, about 100 routes, one container, and a test suite that runs in under three
minutes with no database container. Today's figures are in section 3.

---

## 2. What Fold is trying to be

The yardstick recorded from Grier's direction on 2026-11-23 is a simple-to-deploy, all-in-one
church social event management platform. The plan's goals say the same at more length: one place for a church's groups, events,
RSVPs, rooms and group messages; free and MIT; one deployment per church; installable by a
volunteer on a small VPS; few outside dependencies; a familiar, Meetup-like feel; and safeguarding
(teens, children, contact privacy) taken seriously.

On scale, in Grier's words: "A church with 500 people and 6 staff members would be considered VERY
LARGE for this software."

**Built:** groups with approval and three visibility levels, recurring events in wall-clock time,
RSVP with capacity, waitlist, remote seats and childcare, guests by emailed link, households, teen
accounts with guardian consent, contact privacy with audited reveal, calendar views and ICS feeds,
rooms with conflicts, approval and suggestions, an email composer with scheduled sends and
reminders, stored notifications, web push, account erasure, backups, restore and upgrade tooling,
and two published releases.

**Not started** (`STATUS.md`): the browser setup wizard, provider settings stored in the app, email
provisioning, people import, admin two-factor, legal pages, SMS (a headline goal), attendance, the
people directory (M6.1) and the group finder's filters (M8.3), sign-up sheets, and people export.

**Next in the plan:** Phase 8, the "balance pass" from the Nov 3 competitor teardown. M8.1 (the
event date page) is done. Guest and token flows, the directory finder, attendance, a group "pulse",
announcements versus discussion, calendar-subscribe surfacing and naming Fold's strengths in the
product are queued.

---

## 3. What seven weeks taught

Real work began on 2026-10-06. By 2026-11-26 there were 2,646 commits and 393 merged pull requests
(`git rev-list --count HEAD`, `gh pr list --state merged`). That pace is itself a finding: code is
cheap here. Deciding what to build, and keeping it small, is the expensive part.

### 3.1 Size (measured)

| What | Lines |
|---|---|
| Go, not tests | 76,231 (59,324 in `internal/app`) |
| Go tests | 186,499 (2,857 test functions) |
| Web client source | 40,532 |
| Web client tests | 44,057, plus 14,751 of Playwright |
| Shell in `deploy/` and `scripts/` | 25,266 (7,557 of it installed on the church's server) |
| Schema | 40 tables, 486 columns, 148 CHECKs, 20 triggers |
| HTTP routes | 231 |
| Decisions recorded | 177 |

That is roughly 390,000 lines for a product whose largest customer is 500 people.

### 3.2 Where the effort went (measured, sorted by keywords in PR titles, so approximate)

| Theme | PRs | Lines added | Share of all added lines |
|---|---|---|---|
| Account erasure, restore, recovery levels | 39 | 128,000 | 21% |
| Core: events, RSVP, calendar, groups, guests | 43 | 122,000 | 20% |
| Refactors: untangle, lifecycles, guards | 45 | 80,000 (39,000 deleted) | 13% |
| Security, privacy, sessions | 20 | 43,000 | 7% |
| Messaging, email, notifications, jobs | 41 | 41,000 | 7% |
| Deploy, release, backup, CI | 30 | 33,000 | 6% |
| Save conflicts and revisions | 29 | 23,000 | 4% |

Three things stand out:

- **Deleting an account drew about as many added lines as events, RSVP, the calendar, groups and
  guests together.** The PR bucket is rough (it includes the portable backup work). The firmer
  figure is the tree itself: the erasure, restore-fence and reason-review files are 6,191 lines of
  Go and 40,868 lines of tests, beside a 5,755-line inventory document.
- **Each simplification effort ended with more code than it began with** (summed by PR range, not
  by keyword). Untangle, #316 to #345: +55,275 and -32,295. Lifecycles, #426 and #435 to #444:
  +10,656 and -1,250. The conflict protocol, 23 PRs from #453: +27,930 and -10,603, even though
  its best slice (#467) deleted 4,703 lines. The lifecycle and conflict work also fixed defects and
  added behaviour, so not all of that growth is overhead; the point is that none of the three made
  the codebase smaller.
- **After the Nov 1 audit said "freeze new surface until a first church is live"**, the next three
  weeks (PRs #268 to #449) added 318,000 lines. One product milestone is in there: M8.1, the date
  page with its migrations and API changes, 65,000 added lines in one pull request. Most of the
  rest is erasure, refactoring and hardening. The setup wizard and the import are still not
  started.

### 3.3 What one small change costs (measured: a trace of a member tapping "Going")

- About 66 functions in 20 files run on the server, with 17 SQL statements and two row locks. The
  permission rule runs twice, once before the locks and once under them.
- 146 Go tests reach that one route.
- Adding one optional text field to an RSVP must also change: a migration, the model, the schema
  drift test, the erasure inventory (every one of the 486 columns is classified), a fact-column
  list, the Go response struct, `types.ts`, the type-pairing guard, two golden files, and by review
  rule `docs/API.md`.

### 3.4 What the API-plus-client split costs (measured)

- `web/src/types.ts` has 243 exported types, kept in step with Go by a hand-written list of 220
  pairs and about 670 lines of Go tests.
- 235 error messages in the client mirror 297 error values in Go.
- `docs/API.md` is 3,960 lines describing 213 endpoints whose only real consumer is Fold's own
  client.
- Of 34 pages, 8 are read-only, 24 are plain forms, and 2 need rich client behaviour (the message
  composer, and the delete-account review panel, which exists only because of the erasure design).
- More than half of the client is tests, largely of state that exists only because the page and
  the data are in different programs: loading, reloading, refusals, drafts, cross-tab sign-out.

### 3.5 What Postgres is used for (measured)

The Postgres-only features in use are almost entirely concurrency control: 84 GORM row-lock clauses
and 27 written by hand, 12 advisory-lock call sites, 4 `SKIP LOCKED`, 8 explicit isolation levels,
lock order discussed in 31 files, a 335-line TLA+ model of the job queue, and a 763-line Postgres wire
proxy used by tests to drop a COMMIT reply. The features that are about *data* are few: one
exclusion constraint (room overlap), four `citext` columns, eight `jsonb` columns.

### 3.6 What deployment is (measured)

- The README path is six steps. The DigitalOcean guide is 1,305 lines and 24 steps to a site that
  can send mail.
- Four containers (database, app, Caddy, backup), two published images, seven compose files.
- Mail can only be configured by editing `.env` and recreating the container.
- `restore.sh` is 1,547 lines. Releases carry a "recovery level" (now 4); scripts refuse to go
  below it and there is no override. The full restore drill has 35 phases and its recorded runs
  took from about 54 minutes to several hours.
- CI: the median successful run is 7.7 minutes. Of the last 100 runs, 67 succeeded, 15 failed and
  18 were cancelled.

### 3.7 What Grier has already said

> "This codebase is too large and too complicated and it comes from some early, bad, architecture
> decisions… There's been some repeated attempts to simplify but it never quite holds."

> "I believe this concept was inadvertently added by 'correctness' reviews that were supposed to
> review that the code was correct, not that all interactions were provable."

> (of cross-page recovery of an uncertain save) "get rid of it, that should have never been built".

I agree with all three. The rest of this brief is what follows from taking them seriously.

---

## 4. Why it got this big (inferred)

**Cause 1: the data layer is built for many writers.** Postgres lets any number of requests write
at once, so every write in Fold must decide what to lock, in what order, and what to re-check
after waiting. Each of those decisions is a place for a reviewer to find a race, and each race
found became code, a test, a guard and a paragraph in `AGENTS.md`. Seat counting, the contact
gate, session re-checks under lock, revision tokens, the consent epoch and the job-claim sequence
are all answers to "what if two things happen at once". The product has perhaps six people who
ever write at the same moment.

**Cause 2: two programs.** The React client must be told everything the server knows, in a second
language, and then must handle the server changing its mind while a page is open. The conflict
protocol (DEC-162 to DEC-177), the refused-save hook, the lost-answer receipts and the
"read again and compare" logic are the price of rendering in a different place from where the
decision is made. A form posted to a server that re-renders the page can still be stale, but it
has one state to handle (section 5.2 B), not a protocol.

**Cause 3: personal data is everywhere.** Names and addresses live in accounts, household rows,
guest rows, request text, audit metadata, stored notification text and mail rows. So erasure had to
classify 486 columns, review free text other people wrote, and then defend all of that against a
backup being restored. The restore fence, the recovery levels and the release start-fence all come
from that one root.

**Cause 4: review that rewards finding, with no budget.** Five concerns, a separate reviewer, every
head. Reviewers are good at multi-actor interleavings, and nothing in the process says "this
cannot matter at a 6-staff church". Grier's scale yardstick and the narrowed review brief arrived
on Nov 23, after most of the code. Rules became guards (100 source-scanning tests, 12,433 lines)
and the guards made the code harder to change, which made simplification not hold.

**Cause 5: depth before use.** Rooms with a 13-case suggestion rule, web push written from the
RFCs, the contact-request machinery, per-date visibility with consent, kept dates and household
conversion were all built before anyone could install the app without editing a file. The Nov 1
audit named this; the freeze it recommended lasted 17 days (DEC-153).

None of this is carelessness. Every piece answers a real question correctly. The trouble is that
the questions were generated by the architecture and the review loop, not by a church.

---

## 5. The fresh start

### 5.1 Keep

| Keep | Why |
|---|---|
| **Go, one static binary** (DEC-22, the One Rule) | I tested the alternatives against "a volunteer runs it for ten years". Rails 8 with SQLite is the serious rival: mail, jobs, forms and auth come free. But it brings a runtime, a larger image, and framework upgrades every couple of years, on software that is given away and maintained by one person. Go's compatibility promise and standard-library crypto win on that horizon, and the pure packages below carry over unchanged. |
| **Single-tenant** (DEC-139 item 1) | Hard isolation is the safeguarding story. It gets cheaper with one file per church. |
| **Pure policy functions with table tests** (DEC-139 item 4) | The best idea in the codebase. `internal/policy` is 2,822 lines and has no database in it. |
| **Wall-clock recurrence and stored dates** (C2, C4, DEC-89) | Right, and hard-won. `internal/events` is 985 lines of pure clock. |
| **Stored notifications written with the change** (DEC-42) | Right. Store the kind and the ids, render the words when read (section 5.5). |
| **Approval of new accounts and new groups** (DEC-4, DEC-30, DEC-34) | "Trust the church body on workflow; never relax security" is the correct line. I would not reopen open signup (R8). |
| **Under-13s have no account; teens need consent; teens cannot lead** (DEC-8, DEC-18) | Unchanged. |
| **Uniform answers and no unactionable errors** (DEC-52) | Unchanged. |
| **The design system** (DEC-60 to DEC-66, `design.md`) and the accessibility and layout gates | They work on any HTML. Keep the tokens, the copy rules, and Playwright with axe at 390 and 1280. |
| **One adapter per kind** (DEC-140), **VPS first** (DEC-142), **no Python in what ships** (DEC-151) | Unchanged. |
| **The decision log** | It is the real asset. A fresh start is cheap *because* 177 decisions are written down. |

Packages I would lift nearly as they are: `internal/policy`, `internal/events`, `internal/rooms`,
`internal/images`, `internal/mail`, `internal/content`, `storage`. About 8,000 lines.

### 5.2 Change

**A. SQLite, one file, one writer** (overturns DEC-1, DEC-24, DEC-139 item 2).

- Every write runs alone. Lock order, row locks, advisory locks, isolation levels, "re-check after
  waiting", the seat race and the wire proxy stop existing as topics. A save conflict is one
  integer compared inside the only write in progress.
- The room-overlap exclusion constraint becomes one query inside that write, which is correct
  because nothing else can write. `citext` becomes `COLLATE NOCASE`. `jsonb` becomes SQLite's JSON
  functions.
- The church's whole database is one file. Backup is a consistent copy of that file plus the
  uploads folder. Restore is putting the file back. No database container, no backup container,
  no `pg_dump`, and no Postgres major-version upgrade waiting for a volunteer in year three.
- Tests open a fresh in-memory database in milliseconds. No template databases, no shards.
- Costs, honestly: schema changes are clumsier in SQLite; dates need care (store UTC integers and
  wall-clock text deliberately); one long write blocks others, so a 500-recipient send must work
  in small batches; and there is no path to several app servers, which single-tenancy already
  rules out.
- DEC-139 said to reopen this bet only on "a church that cannot run Docker at all, or a Postgres
  limit hit at real church scale". Neither has happened. My evidence is of a different kind: the
  measured cost in section 3.5 of making many writers safe. DEC-139's argument was that reversing
  is "effort L for zero first-church value" in the existing code. In a fresh start that cost is
  zero.
- **If Grier keeps Postgres:** route every write through one serialized executor in the app (one
  write connection). That keeps most of the gain and loses the deployment gain.

**B. Server-rendered pages, no SPA and no JSON API** (overturns "React + Vite SPA", kept by default
under DEC-22).

- Go templates render each page. Small updates (an RSVP button, a filter, "load more") swap a
  fragment of HTML. JavaScript is a few small files: the rich-text editor on the composer, image
  focal point and resize, and the service worker if push returns.
- Gone: `types.ts` and its pairing guard, the mirrored error copy, `API.md`, the client data
  layer, the CSRF rotation and cross-tab credential code, Vitest, and Node in the release build.
- The date projector already decides everything about how one date looks to one viewer. Today it
  hands that to a second program. Here it hands it to a template.
- The conflict rule Grier chose (DEC-163: refuse the stale save, show the latest, say so plainly)
  becomes: the form carries the row's version; if it is stale, the server renders the form again
  with current values and one line saying someone else changed it. About thirty lines, once.
- Cost: no instant optimistic updates, and no API for a future phone app. Neither is a goal. ICS
  feeds stay.

**C. One door for writes.** Every change is a named command run by one executor: begin the write,
load the actor fresh, ask policy, apply, write the audit row and any notifications and jobs in the
same transaction, commit. The Nov 25 consolidation (#496) tried a server helper for "lock, check,
write, new token" and reverted it because the handlers did not read better with it. With one
writer there are no locks to thread through it, and the helper is just the executor.

**D. SQL first, no ORM** (drops GORM and Gin). Plain SQL with generated or hand-written Go, and the
standard library's router. The schema drift test and the model-matching rules exist only because
of the ORM. The plan's own stack note (foundations 3.3) already said SQL-first fits this schema
better.

**E. Sign in by emailed link** (overturns passwords as the default; DEC-23 "the email is the login"
stays, DEC-90 becomes moot).

- One mechanism covers invitation, sign-in, guest RSVP, email verification and "manage my RSVP".
  A guest is simply a person an admin has not yet approved as a member.
- Gone: password reset, set-password, password rules, the lockout flow, and most of the token
  pages (set password, reset, verify, guest confirm and manage) that M8.2 exists to repair.
- Admins add a second factor (M4.9). Sessions are long-lived so members rarely need a new link.
- Cost: sign-in depends on mail arriving. It already does (invitations, resets), and the setup
  wizard will not finish until a test message is delivered.

**F. Forward-only migrations with an automatic backup first** (overturns the expand-then-contract
half of DEC-138, the recovery levels, and DEC-158's start fence). The binary copies the database
file before it migrates. Rollback is the old image plus that copy. Expand-then-contract is priced
for several replicas of a server; this is one binary that restarts. The Nov 1 audit (R9) said the
same.

### 5.3 Kill or defer

"Defer" means: not in the first version, and it returns when a church asks.

| Item | Do | Why, and what it reverses |
|---|---|---|
| Restore fence, recovery levels, restore drills | Kill | Replace with a short list of erased people kept beside the database and never inside a backup, keyed by a random identifier that is never reused (not the row number, which a restore can hand to someone new). On boot the app scrubs anyone on the list, so `fold restore` cannot bring a person back. A restore done by hand from a whole-disk snapshot rolls the list back too; that limit is stated in the delete dialog and the restore guide, as DEC-158 already states its own. |
| Review of free-text "reasons" left by deleted accounts, and its scale tooling | Kill | Do not collect free text about other people in workflows; offer fixed choices. No residue, nothing to review. |
| Receipts for lost answers (`admin_operation_attempts`, undo receipts as proof) | Kill | Post, redirect, read. A lost answer is a page reload. |
| Contact requests, grants and audited reveal (DEC-31's workflow half) | Simplify | Leaders never see a teen's contact details and reach teens only through Fold (keep). Adults choose whether their leaders see their email and phone. No request, grant or reveal workflow. |
| Per-date visibility that *narrows*, consent tokens, kept dates (DEC-46 in part, DEC-74, DEC-156 to DEC-158) | Simplify | One date may be made *more open* than its series (Grier's actual example: "take a private recurring series and make one public"). Making anything more private changes who can newly see and join; it ejects nobody. The confirmation lists who is signed up and offers Remove (DEC-34: remove bad actors outright). |
| Adding one person to one date (DEC-75) as its own access table | Simplify | An invitation link for that date. Once they answer, the registration is the access. |
| Web push (DEC-121) | Defer | It is 16 unticked device checks and a six-step iPhone install most members will not finish. This re-proposes R7, which Grier rejected, so I flag it as his call. The transport package is written and can return. |
| SMS as a headline goal (F5, F6, F9) | Defer, with the channel seam built in | US texting needs brand and campaign registration by the church (`design/12-providers.md`). Build it with the first church that has a registered number. Also part of rejected R7. |
| Room suggestions (F4) | Keep the pure function, drop the tuning | `internal/rooms` is 365 lines. The cost was in approval and series-request plumbing; keep "free means confirmed, or pending when the church requires approval". |
| Household conversion, guardian copies, guest upgrade | Defer | Rare paths with large surface (`api_household_conversion.go` alone is 693 lines). |
| Group discussion (M8.6's second half, M6.6) | Kill for now | A moderated conversation space that includes minors is a product of its own. Announcements, with replies going to the leader by email, cover the need. |
| Jitsi link generation, tags as room "types", member limits, per-category mail switches beyond one | Defer | Small, but each is a setting, a test and a page. |
| `docs/API.md`, the erasure inventory document, source guards for mirrored code | Kill | Their subjects no longer exist. |

### 5.4 Add

| Add | Why |
|---|---|
| **Setup wizard in the browser, including email with a test send** (M4.1, M4.2, M4.4) | Today a "successful" install cannot invite anyone until someone edits `.env`. This is the single biggest gap between the One Rule and reality. |
| **People import from a spreadsheet** (M4.5, DEC-143) | A church's first hour. Every church has a spreadsheet. |
| **Attendance roll** (M6.3, M8.4) | The number pastors ask for weekly. |
| **Group finder with day, time, childcare and remote filters** (M8.3) | How a newcomer finds a group. |
| **Built-in backups and `fold restore`** | A daily job in the binary and one command, in place of a container and 1,547 lines of shell. |
| **People and own-data export** (M7.8) | A church must be able to leave. |
| **Sign-up sheets** (M6.2) | After the pilot is live; the most common coordination job in small groups. |
| **Built-in HTTPS** (optional) | The binary can obtain its own certificate, making the install one container. Caddy stays as the alternative. |

### 5.5 Shape

**Data, about 25 tables.**

- `people`: every human, with a kind (member, guest, teen, child) and an optional sign-in. Children
  and guests are rows here, not separate tables.
- **Personal data lives in `people` and in columns explicitly tagged as personal.** Everything else
  holds an id. Audit rows and notifications store ids and a kind; the words are rendered when read,
  through the same visibility rules as everything else. Erasure is then: blank the person's row to
  a tombstone, delete their files and tagged text, add their random identifier to the erased list. One function, one
  test that walks the schema for tagged columns.
- `households`, `guardians`, `groups`, `memberships`, `events`, `occurrences`, `registrations`,
  `attendance`, `rooms`, `bookings`, `messages`, `message_recipients`, `notifications`, `jobs`,
  `audit`, `settings`, `sessions`.
- One `approvals` table for anything an admin or a person must decide (new account, new group,
  teen consent, a room that needs approval), in place of two request tables with 53 columns.
- One `tokens` table for every emailed or feed link.

**Jobs.** One goroutine reads due rows from `jobs`. With one writer there is no claim race, no
lease to take back and no fencing sequence, so nothing to model-check.

**Pages.** One read transaction per page: load facts, ask policy, build the view, render. The
"one snapshot per read" rule (`docs/agents/snapshots.md`) holds without anyone having to remember
it.

**Hosting several churches.** One process can serve several churches as several database files
keyed by hostname, with no `tenant_id`. I would not build it, but DEC-139's "fleet tooling at about
five churches" becomes nearly free.

### 5.6 Deployment

```
docker run -d -p 80:80 -p 443:443 -v fold:/data -e DOMAIN=groups.example.org ghcr.io/…/fold
```

- `/data` holds `fold.db`, `uploads/`, `backups/` and the erased list. Copy that folder and you
  have the church. The built-in backup and `fold restore` leave the erased list alone, so a
  restore never forgets a deletion (the limit for whole-disk snapshots is in 5.3).
- First visit: enter the setup code from the log, then the wizard: church name, time zone, first
  admin, mail settings, test message. Nothing else needs a file edited.
- Upgrade: pull the new image. The binary backs up, migrates, starts.
- Target: a volunteer goes from a new VPS to an invited first member in under fifteen minutes, and
  the install guide fits on two pages.

### 5.7 Process

- **Deploy first, then a pilot church.** The first merged slice runs on a real server. Every
  priority after the pilot goes live comes from that church.
- **Budgets in `AGENTS.md`.** Tables, routes and lines of code have ceilings. A change that would
  cross one names what it removes.
- **State the scale in every brief.** "Six staff, 500 people, one writer." A review finding needs
  an ordinary scenario with one person, or one ordinary change by someone else, unless it is
  safeguarding, privacy or consent. This is the scope line that already worked on #486 and #487.
- **One review pass by a different model**, with the five concerns as a checklist, not five
  investigations. Keep the security scoring and its threshold. This changes Grier's merge rule in
  `AGENTS.md`, so it is his call.
- **Structure before guards.** With one door for writes and no mirrored code, most of the 100
  source guards have nothing to guard. Target an `AGENTS.md` of about 150 lines.
- **DEC numbers for product decisions only.** Implementation choices go in the code or the PR.

---

## 6. Build order

Each slice is deployable and ends on the pilot server.

| Slice | Contents |
|---|---|
| 0 | Binary, SQLite, migrations, the write executor, email-link sign-in, setup wizard with test mail, built-in backup, the install on a real VPS, accessibility gates |
| 1 | People, groups, memberships, new-group approval, account approval, group finder |
| 2 | Events, recurrence, dates, RSVP with capacity and waitlist, the date page, calendar, ICS feeds |
| 3 | Notifications (bell and email), reminders, announcements to a group or an event |
| 4 | Spreadsheet import, attendance, admin basics, erasure, export, admin second factor. **Pilot church goes live.** |
| 5 | Households and children as RSVP subjects, childcare counts, teens with guardian consent |
| 6 | Rooms: bookings, conflicts, by-room calendar, suggestions |
| 7 onward | Chosen by the pilot: sign-up sheets, SMS, push, group pulse, per-date opening |

What I think of Phase 8 as it stands: M8.3 (finder) and M8.4 (attendance) move before the pilot.
M8.2 (token pages) mostly dissolves into email-link sign-in. M8.7 (calendar subscribe) is part of
slice 2. M8.5 (pulse) and M8.8 (naming strengths) wait for a church to tell us what it values.
M8.6 keeps announcements and drops discussion.

---

## 7. Rewrite or redirect?

**I would start fresh.** Four reasons:

1. **Switching will never be cheaper.** With no church running Fold there is no data to carry.
2. **Redirecting has been tried.** The Nov 1 audit said to stop adding depth and ship the
   activation spine. It was approved, the freeze lasted 17 days, and most of what followed was
   depth. Each of the three simplification efforts left the code larger (section 3.2). The pull
   comes from the structure, so the structure has to change.
3. **The knowledge moves without the code.** The decisions, the policy tables, the copy, the design
   tokens, the hazard list and the browser journeys all transfer. At the pace this project has
   shown, slices 0 to 4 are weeks, not months (inferred, and uncertain).
4. **Someone has to own what ships.** Roughly 390,000 lines is more than one person with agents
   can keep true for a decade. A tenth of that is.

**Test it cheaply before committing.** One week on slices 0 to 2 in a new repository, then
measure: lines of code, tables, test time, and a timed install from a blank VPS. If the result is
not at least three times smaller for the same behaviour, stop and keep the current code.

**If the goal is a church live within two weeks instead:** the current code is the faster path. It
works and v0.1.1 is published. Then build only the wizard, the import and attendance, and accept
no hardening work at all until that church is using it.

---

## 8. What I am not sure of

- **No usage data exists.** Every product claim here is judgement. A pilot church could show that
  push matters more than I think, or that email-link sign-in confuses older members.
- **The size targets are targets.** I have not built the fresh version.
- **Library facts need checking before anything is committed**, because my knowledge has a cutoff:
  a pure-Go SQLite driver that keeps `CGO_ENABLED=0`, SQL code generation for SQLite, embedded
  certificate handling, and whether a fragment-swapping library runs under Fold's strict CSP
  (DEC-37) with script evaluation turned off.
- **I traced one write end to end**, not all of them. The diagnosis in section 4 rests on that
  trace, the counts in section 3, and the history of the conflict and lifecycle work.
- **The PR themes are sorted by title keywords.** The order of magnitude is reliable; the exact
  shares are not.
- **Two things I propose were already rejected by Grier** (deferring push and SMS, R7). I include
  them because the question asked what I would do, and I mark them as his call.
- **The simpler visibility rule in 5.3** (tightening ejects nobody) is a product change with
  safeguarding weight. It needs Grier's decision, not mine.

---

## 9. Decisions this would overturn

| Decision | Today | Fresh start |
|---|---|---|
| DEC-1, DEC-24, DEC-139 (2) | PostgreSQL only | SQLite, one writer (fallback: Postgres with one serialized write path) |
| DEC-22 default stack; DEC-139 (3) in part | Go + Gin + GORM + React SPA | Go, standard router, SQL first, server-rendered pages. Still one binary. |
| DEC-90 and the password flows | Passwords, 10 to 72 characters | Email-link sign-in; second factor for admins |
| DEC-138 (expand then contract), recovery levels, DEC-158 start fence | Staged migrations, levels 1 to 4, refuse older releases | Forward-only with an automatic backup first |
| DEC-144 mechanism (not the promise) | Per-column inventory, reason review, restore fence | Personal data in one place; erased list re-applied at boot |
| DEC-31 workflow half | Request, grant, audited reveal | Teens only through Fold; adults choose; no workflow |
| DEC-46 in part, DEC-74, DEC-156, DEC-157, DEC-158 | Per-date overrides both ways, consent tokens, kept dates | A date can be opened wider; tightening ejects nobody |
| DEC-75 mechanism | Exact-date access rows | An invitation link; the registration is the access |
| DEC-78 in part | Name search, then "Ask to add" through the contact-request flow | Name search stays. Adding an adult tells them and they can leave. Adding a teen still needs the teen and an admin, as one row in `approvals`. |
| DEC-48 in part | A tightened group cancels the registrations of people it shuts out | The event still stores its own visibility. Tightening ejects nobody (see DEC-46 row). |
| C5 and the rule "never load a guest row as an actor" | Guests cannot sign in | A guest signs in by link and can do only what a guest may: see and manage their own RSVPs |
| DEC-53, DEC-87 | No reset for guests; an email change needs the current password and sends a cancel link | No passwords to reset. An email change is confirmed from the new address, and the old address still gets the cancel link. |
| DEC-19, DEC-29, DEC-132, DEC-149 (guardian copies) | Built, off by default | Deferred until a church asks |
| The five-concern merge rule (`AGENTS.md`) | Five concerns reviewed on every head | One pass with the five as a checklist, scale stated in the brief |
| DEC-121 to DEC-127, DEC-134 (push) | Built | Deferred until a church asks (rejected before as R7) |
| F5, F6, F9 SMS as headline | Planned, not started | Deferred, channel seam kept (rejected before as R7) |
| DEC-162 to DEC-177 mechanism (not the behaviour) | Revision tokens, client compare, refused-save hooks | Version in the form, server re-renders with a plain line |
| DEC-153 | New surface permitted before a church is live | Pilot church first, then surface |
| M8.6 discussion, M6.6 | Planned | Announcements only |

Kept without change, among the ones that shape the product: DEC-2 to DEC-8, DEC-10 to DEC-14,
DEC-17, DEC-18, DEC-20, DEC-23, DEC-30, DEC-34 to DEC-36, DEC-42, DEC-52, DEC-57 to DEC-66, DEC-89,
DEC-139 (1), (4), (5), DEC-140, DEC-142, DEC-143, DEC-151, and the product behaviour of every
decision whose mechanism changes above.

---

## Addendum, 2026-11-26: what Grier decided

Grier read this brief and chose to start fresh in a new repository. It has a working name for now
and will take the name Fold again once the repositories are sorted out. His rulings, recorded
there in `docs/DECISIONS.md`:

- **SQLite, one writer:** accepted. "SQLite is what I started with… I love sqlite."
- **Push and SMS:** deferred until the first church is live, then ask whether they want push, SMS
  or neither.
- **Making something more private removes nobody:** accepted as the provisional starting rule; he
  expects it to change when a church asks.
- **One review pass:** accepted, with security as the primary concern because the site is public
  and strangers type into it.
- **The tamper-evident audit log is built from the start**, not deferred. This reverses the cut in
  DEC-140 that section 9 lists as kept. Nobody can view the log unless it is requested for a
  forensic purpose, and requesting it tells every admin and needs a second admin. The case of a
  church with one admin is still open.
- **Sign-in by emailed link:** the link lasts 24 hours; a session lasts a week and is renewed when
  the person visits again from the same browser.
- **Caddy stays as the proxy.** Section 5.4 proposed letting the binary obtain its own certificate;
  he chose Caddy because it handles Let's Encrypt and keeps the project from owning HTTP edge
  cases. A church therefore runs two containers, not one.
- **No component framework to start with**, forward-only migrations, and budgets of 35 tables and
  200 routes: "I would rather have more of these than try to work with less than we need."
- **A pilot church is named** in the new project's plan.

What is still only proposed there: guests signing in by link, the contact rules, the erased list,
and the remaining budgets. One correction made here after review: the erased list is keyed by a
random identifier, not a row number.

The carry-over turned out smaller than section 5.1 says. Only the image, storage, sanitizer,
provider and event-clock packages copy cleanly (about 3,300 lines); policy, rooms, mail and the
HTTP middleware need adapting.
