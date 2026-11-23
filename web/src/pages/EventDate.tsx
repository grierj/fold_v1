import { canRegisterAsGuest } from "../lib/can";
import { lazy, Suspense, useCallback, useEffect, useId, useLayoutEffect, useRef, useState, type RefObject } from "react";
import { Link, useParams } from "react-router-dom";
import { ApiError } from "../api/core";
import { Dates, Events } from "../api/events";
import { GuestRegistrationForm } from "../components/GuestRegistrationForm";
import { useAuth } from "../auth";
import type { AttendanceMode, CancelledEventDate, EventDate } from "../types";
import { Button, ButtonLink } from "../components/ui/Button";
import { EmptyState } from "../components/ui/EmptyState";
import { Skeleton } from "../components/ui/Skeleton";
import { Choices, Field, focusFirstInvalid, FormError, Input, Radio } from "../components/ui/Field";
import { SegmentButton } from "../components/ui/SegmentButton";
import { errorCode, errorText } from "../lib/errors";
import { useLoad, type LoadOutcome } from "../lib/useLoad";
import { formatDayFull } from "../lib/when";
import { dateLine } from "../lib/repeat";
import { registrationStatusLabel } from "../lib/status";
import type { AnswerStatus } from "../lib/registration";
import { REFRESH_UNDO_CODES, seriesResultSummary, TERMINAL_UNDO_CODES, undoReceiptOf, type UndoReceipt } from "../lib/rsvpFeedback";
import { useRsvpMutationGate } from "../lib/useRsvpMutationGate";
import { ChevronLeftIcon } from "../components/icons";
import { DateActions } from "../components/DateActions";
import { carriesNames, DateComing, DateIdentity, DateSeats, DateStanding, DateWhenWhere } from "../components/DateSummary";
import { SeriesRsvpDialog, type SeriesRsvpScope } from "../components/SeriesRsvpDialog";
import { StickyRsvpBar } from "../components/StickyRsvpBar";
import { composePath, EMAIL_GOING, useCanEmailEvent } from "../components/messages/composeLinks";
import { DateReminderLines } from "../components/DateReminders";
import { ConfirmProvider, useConfirm, type PendingAction } from "../components/ui/ConfirmDialog";
import { useLeaveQuestion } from "../lib/leaveQuestion";
import { manageAuthority } from "../lib/manageAuthority";

const ManageDate = lazy(() => import("../components/ManageDate").then((module) => ({ default: module.ManageDate })));

export function EventDatePage() {
  const { user } = useAuth();
  const { id = "", date = "" } = useParams();

  // Questions on this page ask in the app's dialog, never the browser's.
  return <ConfirmProvider><DateDetail key={`${user?.id ?? "anonymous"}:${id}:${date}`} id={id} date={date} /></ConfirmProvider>;
}

// The answer draft: what the in-flow fields hold. It is kept while the saved answer and the terms the
// fields are drawn from stay the same (a failed save, a recheck that changes nothing), and starts again
// from the saved answer when they change.
type Draft = { mode: AttendanceMode | undefined; guests: string };

const draftTerms = (o: EventDate) => JSON.stringify([o.myRegistration, o.myAttendanceMode ?? null, o.myGuestCount ?? null, !!o.canRegister, o.canDecline, o.remoteAvailable, o.guestsAllowed]);

// An ordinary first answer starts in person with no guests; an invitation leaves what it doesn't know unset.
const ordinaryOnly = (o: EventDate) => !!o.canRegister && !o.canDecline;

const draftOf = (o: EventDate): Draft => ({
  mode: o.myAttendanceMode ?? (ordinaryOnly(o) ? "in_person" : undefined),
  guests: o.myGuestCount === undefined ? (ordinaryOnly(o) ? "0" : "") : String(o.myGuestCount),
});

const guestsShown = (o: EventDate) => (o.myGuestCount !== undefined || ordinaryOnly(o)) && (o.guestsAllowed > 0 || (o.myGuestCount ?? 0) > 0);

const guestsValid = (o: EventDate, d: Draft) => d.guests !== "" && Number.isInteger(Number(d.guests)) && Number(d.guests) >= 0 && Number(d.guests) <= o.guestsAllowed;

// The fields a single-date answer sends: only what the page knows. A missing key keeps the server's value.
type AnswerBody = { status: AnswerStatus; attendanceMode?: AttendanceMode; guestCount?: number };

function answerBody(o: EventDate, d: Draft, status: AnswerStatus): AnswerBody {
  const body: AnswerBody = { status };

  if (status === "declined") return body;

  if (d.mode !== undefined) body.attendanceMode = d.mode;

  if (guestsShown(o)) body.guestCount = Number(d.guests);

  return body;
}

// A series answer from this page carries the person's shown choices and never childcare, which the date
// read can't describe: an omitted childcare field keeps each date's own value.
function seriesBody(o: EventDate, d: Draft, status: AnswerStatus, scope: Exclude<SeriesRsvpScope, "just_this_date">, date: string) {
  const body: AnswerBody & { scope: typeof scope; date?: string } = { scope, status };

  if (scope === "this_and_following") body.date = date;

  if (status !== "declined" && d.mode !== undefined) body.attendanceMode = d.mode;

  if (status === "going" && guestsShown(o)) body.guestCount = Number(d.guests);

  return body;
}

// Why Going can't be asked for this date alone: the chosen pool is full, there is no waitlist, and the
// person holds no seat in it. Only a Going row in the same pool holds one (registration_seats.go): a
// waitlisted row would only be saved as waiting again. Only an ordinary read has pools; a grant-only read
// is never judged here.
function fullFor(o: EventDate, mode: AttendanceMode | undefined): string | null {
  const seats = o.visibility ? o.seats : undefined;

  if (!seats || seats.waitlistEnabled) return null;
  const chosen = mode ?? "in_person";
  const pool = chosen === "remote" ? seats.remote : seats.inPerson;

  if (!pool || pool.capacity === null || (pool.left ?? 1) > 0) return null;
  const held = o.myRegistration === "going" && (o.myAttendanceMode ?? "in_person") === chosen;

  if (held) return null;

  return chosen === "remote" ? "Remote seats are full for this date." : "This date is full.";
}

const ANSWERS: AnswerStatus[] = ["going", "maybe", "declined"];

// A visible repeating series this person may answer from here: the server's own series authority on an ordinary read.
const answersSeries = (o: EventDate) => !!o.visibility && !!o.series?.recurrenceRule && o.series.canAnswerSeries && !!(o.canRegister || o.canDecline);

function DateDetail({ id, date }: { id: string; date: string }) {
  const { user } = useAuth();
  // Failed authority refreshes always fail closed, including network errors, and so does
  // coming back to the window: what this person may do here may have changed meanwhile.
  const load = useLoad(() => Dates.get(id, date).then((r) => r.occurrence), [id, date], { refreshOnFocus: true });
  const read = load.data ?? null;
  // A cancelled date's tombstone has three facts and nothing scheduled: it branches before any time, place or capability is read.
  const tombstone = read && "cancelled" in read ? read : null;
  const occurrence = read && !("cancelled" in read) ? read : null;
  const loaded = !load.loading;
  const loadEpoch = load.epoch();
  // A question asked under one load answers no once a refresh moves it on: what this person may do may have changed.
  const beginEnd = useConfirm(loadEpoch);
  const beginScope = useConfirm(loadEpoch, { followsPage: false });
  useLeaveQuestion();
  // One writer for every member control here: a single-date answer, an invitation end, a series answer and its Undo.
  const gate = useRsvpMutationGate();
  const busy = gate.busy;
  // What the last write's failure said, tied to the load generation it failed under: any later read makes it history.
  const [failure, setFailure] = useState<{ epoch: number; text: string } | null>(null);
  useEffect(() => { if (failure && failure.epoch !== loadEpoch) setFailure(null); });
  const error = failure && failure.epoch === loadEpoch ? failure.text : "";
  const mounted = useRef(false);
  useEffect(() => {
    mounted.current = true;

    return () => { mounted.current = false; };
  }, []);
  // What async work reads after an await: the page as it is now, not as it was when the work began.
  const latest = useRef({ occurrence, loading: load.loading });
  latest.current = { occurrence, loading: load.loading };

  // The series receipt lives here, outside the branch a refresh unmounts, for this actor, event and
  // original date only (the key above): a route or session change starts clean.
  const [receipt, setReceipt] = useState<UndoReceipt | null>(null);
  // What the last series answer or Undo did, in words: shown only under a settled read that still shows the series.
  const [result, setResult] = useState<string | null>(null);
  // This page's own read after a write it made: the page keeps its height, and only the answer bar and
  // a neutral line show until the fresh read lands. Any other read (a focus recheck) is not one.
  const [hold, setHold] = useState<{ token: object; height: number } | null>(null);
  const selfReading = !!hold && load.loading;
  // The names list starts closed and is the person's to open. It stays open through this page's own read
  // after a write it made, so the page keeps its length and the reader keeps their place; it shows nothing
  // until that read lands. Any other read that empties the page (a focus or authority recheck, a failure,
  // a tombstone) and any read that carries no names close it, so a later grant starts closed.
  const [namesOpen, setNamesOpen] = useState(false);

  if (namesOpen && (occurrence ? !carriesNames(occurrence) : !selfReading)) setNamesOpen(false);
  const page = useRef<HTMLDivElement>(null);
  const [scope, setScope] = useState<{ status: AnswerStatus; action: PendingAction } | null>(null);
  useEffect(() => scope?.action.whenStale(() => setScope(null)), [scope]);
  const [draft, setDraft] = useState<{ terms: string; values: Draft } | null>(null);
  const terms = occurrence ? draftTerms(occurrence) : "";
  const values = occurrence ? (draft && draft.terms === terms ? draft.values : draftOf(occurrence)) : null;
  const edit = (patch: Partial<Draft>) => { if (occurrence && values) setDraft({ terms, values: { ...values, ...patch } }); };
  const details = useRef<HTMLDivElement>(null);
  const answerGroup = useRef<HTMLDivElement>(null);
  const statusRef = useRef<HTMLDivElement>(null);
  const undoFocused = useRef(false);
  // Undo went away while focused and nothing could take focus yet (a fail-closed read is out): settle it once that read lands.
  const refocus = useRef(false);
  const heading = useRef<HTMLHeadingElement>(null);
  const [guestOpen, setGuestOpen] = useState(false);
  const guestOpener = useRef<HTMLButtonElement>(null);
  const guestReturn = useRef(false);

  // The date is gone for this person: drop it and anything still on its way.
  const gone = () => load.close();
  // Manager tools follow the latest answer (lib/manageAuthority): shown from a grant, through a recheck or an
  // unknown answer, and gone with any other settled answer, in the render it lands, before the effect runs.
  const authority = manageAuthority(load);
  const [manages, setManages] = useState(false);
  useEffect(() => { if (authority === "granted") setManages(true); else if (authority === "lost") setManages(false); }, [authority]);
  const showManage = manages && authority !== "lost";
  const { reload, retry } = load;

  // A recheck of authority fails closed and shows nothing it can't vouch for: no earlier result, no held page.
  const recheck = useCallback((): Promise<LoadOutcome> => {
    setHold(null);
    setResult(null);

    return retry();
  }, [retry]);

  const reloadPage = useCallback(() => void reload(), [reload]);
  const retryPage = useCallback(() => void recheck(), [recheck]);

  // Coming back to the window is a recheck too (useLoad retries on focus): it hides results and the held page.
  useEffect(() => {
    const away = () => { setHold(null); setResult(null); };

    window.addEventListener("focus", away);

    return () => window.removeEventListener("focus", away);
  }, []);

  // A settled read that no longer shows this series (narrower, a tombstone, gone) ends the receipt and its result.
  const seriesGone = loaded && !(occurrence?.visibility && occurrence.series);
  useEffect(() => {
    if (!seriesGone) return;

    if (receipt) setReceipt(null);

    if (result) setResult(null);
  });

  const expire = useCallback((ended: UndoReceipt) => {
    setReceipt((r) => (r === ended ? null : r));
    setResult((r) => (r ? `${r} · The Undo time has ended.` : "The Undo time has ended."));
  }, []);

  // The deadline is the server's absolute time, checked by a timer, on return to the tab, and again on use.
  // A timer can fire a millisecond before Date.now() reaches the deadline, so a check that finds time left
  // waits again for what is left.
  useEffect(() => {
    if (!receipt) return;
    let timer = 0;

    const check = () => {
      const left = receipt.expiresAt - Date.now();

      window.clearTimeout(timer);

      if (left <= 0) expire(receipt);
      else timer = window.setTimeout(check, left);
    };

    timer = window.setTimeout(check, Math.max(0, receipt.expiresAt - Date.now()));
    document.addEventListener("visibilitychange", check);
    window.addEventListener("focus", check);

    return () => {
      window.clearTimeout(timer);
      document.removeEventListener("visibilitychange", check);
      window.removeEventListener("focus", check);
    };
  }, [receipt, expire]);

  // Starts this page's own read after its write, keeping the page's height meanwhile.
  function selfRefresh() {
    const token = {};
    setHold({ token, height: page.current?.offsetHeight ?? 0 });
    void load.retry().then(() => { if (mounted.current) setHold((h) => (h?.token === token ? null : h)); });
  }

  // After a write succeeded: read the date again. If a read moved on meanwhile, the page may show what was
  // true before the write, so check again (fail closed) unless the page already shows nothing.
  function afterWrite(epoch: number) {
    if (load.epoch() === epoch) selfRefresh();
    else if (latest.current.occurrence || latest.current.loading) void recheck();
  }

  // Undo is gone (used, expired, refused): keep the person's place on the answer, without scrolling.
  // A disabled answer can't take focus, so it is passed over for the next one that can.
  function focusAnswer(): boolean {
    const group = answerGroup.current;
    const target = group?.querySelector<HTMLElement>('[aria-pressed="true"]:not(:disabled)') ?? group?.querySelector<HTMLElement>("button:not(:disabled)");
    target?.focus({ preventScroll: true });

    return !!target && document.activeElement === target;
  }

  async function single(status: AnswerStatus) {
    if (!occurrence || !values) return;
    const o = occurrence;
    const body = answerBody(o, values, status);
    const epoch = load.epoch();
    const release = gate.acquire();

    if (!release) return;
    setFailure(null);

    try {
      if (o.canDecline && body.status !== "declined") await Dates.register(id, date, { ...body, status: body.status });
      else await Events.register(Number(id), { date, ...body });

      if (!mounted.current) return;
      setResult(null);
      afterWrite(epoch);
    } catch (e) {
      if (!mounted.current) return;

      if (e instanceof ApiError && [401, 403, 404].includes(e.status)) gone();
      else if (load.epoch() === epoch) setFailure({ epoch, text: errorText(e) });
    } finally {
      release();
    }
  }

  async function end(action: "decline" | "block") {
    if (!occurrence || busy || !(action === "block" ? occurrence.canBlock : occurrence.canDecline)) return;
    const ordinary = !!occurrence.visibility;

    const question = action === "decline"
      ? { title: "Can't go to this date?", body: "This ends your invitation and access to this date only. Other accepted dates stay yours.", confirmLabel: "Can't go" }
      : { title: "Don't add me again?", body: "This ends your invitation to this date and stops future manager adds from this organizer (the group's leaders, or this event's hosts). Other accepted dates stay yours. It does not stop you choosing to register where you are eligible.", confirmLabel: "Don't add me again" };

    const ending = beginEnd();
    const epoch = load.epoch();

    if (!(await ending.ask({ ...question, tone: "danger", native: `${question.title} ${question.body}` })) || load.epoch() !== epoch || !mounted.current) return;
    const release = gate.acquire();

    if (!release) return;
    setFailure(null);

    try {
      await Dates.end(id, date, action);

      if (!mounted.current) return;

      // A successful end revokes the invitation even if a focus read raced it, so no earlier read may stand.
      // A reader who can also see the date the ordinary way keeps whatever a fresh read still shows them.
      if (ordinary) void recheck();
      else gone();
    } catch (e) {
      if (!mounted.current) return;

      if (e instanceof ApiError && [401, 403, 404].includes(e.status)) gone();
      else if (load.epoch() === epoch) setFailure({ epoch, text: errorText(e) });
    } finally {
      release();
    }
  }

  async function series(status: AnswerStatus, chosen: Exclude<SeriesRsvpScope, "just_this_date">) {
    if (!occurrence || !values) return;
    const body = seriesBody(occurrence, values, status, chosen, date);
    const epoch = load.epoch();

    if (status === "declined" && !(await beginEnd().ask({
      title: "Can't go to these dates?",
      body: "They will be removed from your calendar and personal feed.",
      confirmLabel: "Can't go to these dates",
      tone: "normal",
      native: "Can't go to these dates? They will be removed from your calendar and personal feed.",
    }) && load.epoch() === epoch && mounted.current)) return;
    const release = gate.acquire();

    if (!release) return;
    setFailure(null);

    try {
      const answered = await Events.registerSeries(Number(id), body);

      if (!mounted.current) return;
      // A new receipt replaces the last; an answer that changed nothing issues none and leaves a valid one in place.
      const issued = undoReceiptOf(answered);

      if (issued) setReceipt(issued);

      if (load.epoch() === epoch) {
        setResult(seriesResultSummary(answered));
        selfRefresh();
      } else afterWrite(epoch);
    } catch (e) {
      if (!mounted.current) return;

      if (e instanceof ApiError && e.status === 401) gone();
      // A refusal can take away the series alone: forget what the series showed and read this date again, fail closed.
      else if (e instanceof ApiError && [403, 404].includes(e.status)) void recheck();
      else if (load.epoch() === epoch) setFailure({ epoch, text: errorText(e, "We couldn't save your answer for those dates.") });
    } finally {
      release();
    }
  }

  async function undo() {
    const used = receipt;

    if (!used || busy) return;

    if (Date.now() >= used.expiresAt) {
      expire(used);

      return;
    }

    const epoch = load.epoch();
    const release = gate.acquire();

    if (!release) return;
    setFailure(null);

    try {
      const undone = await Events.undoSeriesRegistration(Number(id), used.token);

      if (!mounted.current) return;
      setReceipt((r) => (r === used ? null : r));

      if (load.epoch() === epoch) {
        setResult(seriesResultSummary(undone, "Undo: "));
        selfRefresh();
      } else afterWrite(epoch);
    } catch (e) {
      if (!mounted.current) return;
      const code = errorCode(e);

      if (e instanceof ApiError && e.status === 401) {
        gone();

        return;
      }

      if (TERMINAL_UNDO_CODES.has(code)) setReceipt((r) => (r === used ? null : r));
      const text = errorText(e, "We couldn't undo that answer.");

      // What may have changed is read again, fail closed; its message waits for that read so it stays on screen.
      if (REFRESH_UNDO_CODES.has(code) || (e instanceof ApiError && [403, 404].includes(e.status))) {
        void recheck().then((outcome) => { if (outcome === "loaded" && mounted.current) setFailure({ epoch: load.epoch(), text }); });
      } else if (load.epoch() === epoch) setFailure({ epoch, text });
    } finally {
      release();
    }
  }

  const full = occurrence && values ? fullFor(occurrence, values.mode) : null;

  // Every answer starts here, from the bar. A field that would be sent is checked first: the first
  // problem takes focus and nothing is sent.
  function choose(status: AnswerStatus) {
    if (!occurrence || !values || busy || selfReading) return;

    if (status !== "declined" && guestsShown(occurrence) && !guestsValid(occurrence, values)) {
      focusFirstInvalid(details.current);

      return;
    }

    if (answersSeries(occurrence)) {
      setScope({ status, action: beginScope() });

      return;
    }

    justThisDate(status);
  }

  function justThisDate(status: AnswerStatus) {
    if (!occurrence) return;

    if (status === "declined" && occurrence.canDecline) void end("decline");
    else if (!(status === "going" && full)) void single(status);
  }

  function chooseScope(chosen: SeriesRsvpScope) {
    const pending = scope;
    setScope(null);

    // A picker that went stale (a refresh, another page) sends nothing.
    if (!pending?.action.current()) return;

    if (chosen === "just_this_date") justThisDate(pending.status);
    else void series(pending.status, chosen);
  }

  // Messaging capability is asked only while the current answer is a scheduled date, and stays
  // asked while a refresh of it is in flight rather than again after each one. Any other settled
  // answer (a tombstone, including one a refresh brings, or not available) turns it off, already in
  // the render before the effect clears the flag.
  const [scheduled, setScheduled] = useState(false);
  useEffect(() => { if (occurrence) setScheduled(true); else if (loaded) setScheduled(false); }, [occurrence, loaded]);
  const canEmail = useCanEmailEvent(scheduled && (occurrence || !loaded) ? Number(id) : undefined);

  // The bar's buttons, as the last scheduled read offered them: during this page's own read they stay in
  // place (so focus stays where it was) but answer nothing and show no saved answer.
  const offered = occurrence ? (occurrence.canRegister || occurrence.canDecline ? ANSWERS : []) : null;
  const lastOffered = useRef<AnswerStatus[]>([]);

  if (offered) lastOffered.current = offered;
  const actions = user ? offered ?? (selfReading ? lastOffered.current : []) : [];
  const receiptLive = !!receipt && receipt.expiresAt > Date.now();
  // A generic Undo stays through this page's own read; any other read hides it until it settles.
  const undoShown = !!user && receiptLive && (selfReading || !!(occurrence?.visibility && occurrence.series));
  const resultShown = !!result && !!occurrence?.visibility && !!occurrence.series;
  const guestDoor = !user && !!occurrence && canRegisterAsGuest(false, occurrence.visibility);
  const barShown = actions.length > 0 || undoShown || (guestDoor && !guestOpen);

  useLayoutEffect(() => {
    if (!undoShown && undoFocused.current) {
      undoFocused.current = false;
      refocus.current = !focusAnswer();
    }
  }, [undoShown]);

  // Once the read settles, focus lands on what remains: the answer, else the page heading. Focus the
  // person moved elsewhere meanwhile is theirs to keep.
  useLayoutEffect(() => {
    if (!refocus.current || !loaded) return;
    refocus.current = false;
    const active = document.activeElement;

    if (active && active !== document.body) return;

    if (!focusAnswer()) heading.current?.focus({ preventScroll: true });
  });

  useLayoutEffect(() => {
    if (!guestOpen && guestReturn.current) {
      guestReturn.current = false;
      guestOpener.current?.focus();
    }
  }, [guestOpen]);

  if (tombstone) return <CancelledDate cancelled={tombstone} heading={heading} />;

  const when = occurrence ? dateLine(occurrence.startsAt, occurrence.endsAt) : "";
  const pressed = (status: AnswerStatus) => !!occurrence && (occurrence.myRegistration === status || (status === "going" && occurrence.myRegistration === "waitlisted"));

  return <div className="fold-page fold-page-narrow space-y-6 wrap-anywhere">
    <div ref={page} className="space-y-6" style={selfReading && hold ? { minHeight: hold.height } : undefined}>
      <div className="fold-page-header">
        <div className="min-w-0 grow">
          {/* The link says where it goes for screen readers and is also not confused with the nav's Calendar tab. */}
          <ButtonLink variant="link" to="/calendar"><ChevronLeftIcon className="h-4 w-4 shrink-0" /><span aria-hidden="true">Calendar</span><span className="sr-only">Back to calendar</span></ButtonLink>
          {occurrence ? <DateIdentity occurrence={occurrence} heading={heading} /> : <h1 ref={heading} tabIndex={-1} className="text-3xl font-bold">Event date</h1>}
        </div>
      </div>
      {!loaded && !selfReading && <p role="status">Loading this date…</p>}
      {loaded && !occurrence && <EmptyState title="This date is not available" body="Your invitation may have ended, or this date may not be available to you. Check your calendar for dates you can open." />}
      {(occurrence || selfReading) && <section aria-label="This event date" className="space-y-6">
        {/* The six answers come first (M8.1 brief 5.1); everything else follows them. */}
        <div className="space-y-3">
          {occurrence && <DateWhenWhere occurrence={occurrence} />}
          {/* The one RSVP status: the person's standing and what their last answer did, kept through this page's own read. */}
          {user && <div ref={statusRef} data-date-answer="standing" role="status" tabIndex={-1}>
            {occurrence ? <DateStanding occurrence={occurrence} /> : <p className="text-text-muted">Updating your answer…</p>}
            {resultShown && <p className="text-sm text-text-muted">{result}</p>}
          </div>}
          {occurrence && <DateComing occurrence={occurrence} namesOpen={namesOpen} onNamesOpenChange={setNamesOpen} />}
        </div>
        {occurrence && <div className="fold-section space-y-4">
          {occurrence.visibility && occurrence.series && occurrence.description &&
            <p className="text-text-muted whitespace-pre-wrap">{occurrence.description}</p>}
          {guestDoor && <>
            <GuestRegistrationForm instance={occurrence} open={guestOpen} onOpenChange={(open) => { guestReturn.current = !open; setGuestOpen(open); }} />
            <p className="text-sm text-text-muted">Have an account? <Link to="/login" state={{ from: `/events/${id}/dates/${date}` }} className="text-link">Log in to answer</Link></p>
          </>}
          {/* Only a real exact-date invitation (DEC-75): canDecline is true for those reads alone. */}
          {occurrence.canDecline && <p className="text-text-muted">You're invited to this date.</p>}
          {values && (occurrence.canDecline || occurrence.canRegister) && <AnswerDetails occurrence={occurrence} values={values} busy={busy} full={full} root={details} onEdit={edit} />}
          {error && <FormError>{error}</FormError>}
          <div className="flex flex-wrap gap-2 empty:hidden">
            {occurrence.canBlock && <Button variant="danger-outline" disabled={busy} onClick={() => void end("block")}>Don't add me again</Button>}
            {canEmail && <ButtonLink variant="secondary" to={composePath({ occurrence: occurrence.id })}>{EMAIL_GOING}</ButtonLink>}
          </div>
          <DateActions eventId={occurrence.eventId} date={occurrence.date} sharePath={occurrence.dateUrl} title={occurrence.title} when={when} />
          <DateSeats occurrence={occurrence} />
          {occurrence.reminders && <DateReminderLines reminders={occurrence.reminders} />}
        </div>}
      </section>}
    </div>
    {showManage && <Suspense fallback={<Skeleton kind="lines" label="Loading the manager tools…" />}>
      <ManageDate eventId={Number(id)} date={date} occurrenceId={occurrence?.id} authority={authority} onDateChanged={reloadPage} onGone={retryPage} onRetryPage={retryPage} />
    </Suspense>}
    {barShown && <StickyRsvpBar label="Answer this date">
      {actions.length > 0 && <div ref={answerGroup} role="group" aria-label="Your answer" className="flex min-w-0 flex-1 sm:max-w-sm">
        {actions.map((status) => <SegmentButton key={status} pressed={!!occurrence && pressed(status)}
          // Busy keeps the button focusable (aria-disabled), so a keyboard user's place survives a save.
          aria-disabled={busy || selfReading || undefined}
          disabled={status === "going" && !!full && !!occurrence && !answersSeries(occurrence)}
          onClick={() => choose(status)}>{registrationStatusLabel(status)}</SegmentButton>)}
      </div>}
      {undoShown && <Button variant="secondary" layout="shrink-0" aria-disabled={busy || undefined}
        onFocus={() => { undoFocused.current = true; }} onBlur={() => { undoFocused.current = false; }}
        onClick={() => void undo()}>Undo</Button>}
      {guestDoor && !guestOpen && <Button ref={guestOpener} variant="primary" layout="w-full sm:w-auto" onClick={() => setGuestOpen(true)}>Register as a guest</Button>}
    </StickyRsvpBar>}
    {scope && occurrence && <SeriesRsvpDialog answer={registrationStatusLabel(scope.status)} disabled={busy}
      justThisDate={scope.status === "going" && full ? full : undefined} onCancel={() => setScope(null)} onChoose={chooseScope} />}
  </div>;
}

// How the person will attend and with how many guests: the one draft every answer from the bar sends.
function AnswerDetails({ occurrence, values, busy, full, root, onEdit }: {
  occurrence: EventDate;
  values: Draft;
  busy: boolean;
  full: string | null;
  root: RefObject<HTMLDivElement | null>;
  onEdit: (patch: Partial<Draft>) => void;
}) {
  const modeName = useId();
  const guestMax = occurrence.guestsAllowed;
  const showGuests = guestsShown(occurrence);
  const mode = values.mode;
  const mine = occurrence.myRegistration;

  return <div ref={root} className="space-y-3">
    {/* Said only where an ordinary read shows the date has a waitlist; a grant-only read promises nothing. */}
    {mine !== "going" && mine !== "waitlisted" && occurrence.visibility && occurrence.seats?.waitlistEnabled === true && <p className="text-sm text-text-muted">If it’s full, Going puts you on the waitlist.</p>}
    {full && <p className="text-sm text-text-muted">{full} Going isn't open for this date alone. You can still answer Maybe or Can't go{answersSeries(occurrence) ? ", or choose other dates in the series" : ""}.</p>}
    {(occurrence.remoteAvailable || mode === "remote") && mode !== undefined && <Choices legend="How will you attend?" hint="Choose in person or remotely. Space is checked when you save.">
      <Radio name={modeName} label="In person" checked={mode === "in_person"} disabled={busy} onChange={() => onEdit({ mode: "in_person" })} />
      <Radio name={modeName} label="Remote" description={!occurrence.remoteAvailable ? "No longer available. Choose in person to change your answer." : undefined} checked={mode === "remote"} disabled={busy || !occurrence.remoteAvailable} onChange={() => onEdit({ mode: "remote" })} />
    </Choices>}
    {showGuests && <Field label="Guests" hint={`Whole number from 0 to ${guestMax}. Guests use spaces too.`} error={!guestsValid(occurrence, values) ? `Enter a whole number from 0 to ${guestMax}.` : undefined}><Input type="number" min={0} max={guestMax} step={1} value={values.guests} disabled={busy} onChange={(e) => onEdit({ guests: e.target.value })} /></Field>}
  </div>;
}

// A cancelled date as the server lets a qualified viewer see it (M8.1 brief 2.2): its title, its
// date and that it was cancelled, with no answer, series, video, attendee, reminder, guest, share or
// calendar-download controls.
function CancelledDate({ cancelled, heading }: { cancelled: CancelledEventDate; heading: RefObject<HTMLHeadingElement | null> }) {
  return <div className="fold-page fold-page-narrow space-y-6 wrap-anywhere">
    <div className="fold-page-header">
      <div>
        <ButtonLink variant="link" to="/calendar"><ChevronLeftIcon className="h-4 w-4 shrink-0" /><span aria-hidden="true">Calendar</span><span className="sr-only">Back to calendar</span></ButtonLink>
        <h1 ref={heading} tabIndex={-1} className="text-3xl font-bold">{cancelled.title}</h1>
      </div>
    </div>
    <section aria-label="This event date" className="space-y-3">
      <p className="font-medium">{formatDayFull(cancelled.date)}</p>
      <p>This date was cancelled. Your calendar shows the dates that are still on.</p>
      <ButtonLink variant="secondary" to="/calendar">Open your calendar</ButtonLink>
    </section>
  </div>;
}
