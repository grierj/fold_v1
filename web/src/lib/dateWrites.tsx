import { createContext, useCallback, useContext, useEffect, useLayoutEffect, useRef, useState, useSyncExternalStore, type ReactNode } from "react";
import { ApiError, credentialCurrent, type Credential } from "../api/core";
import { DateEdits, type DateEditChanges, type DateEditEnvelope } from "../api/events";
import type { OccurrenceResponse, Visibility } from "../types";
import { createRosterWrites, rosterKey, RosterWritesContext, type RosterWrites, type WriteOutcome } from "./rosterWrites";
import { dateDecision, type DateDecision } from "./visibilityConsent";
import { isObject, isString } from "./wireValues";

// The event and group a date belongs to (groupId is absent for a church-wide event or a group the editor
// was not shown).
interface DateTarget {
  eventId: number;
  groupId?: number;
}

// A date's update that answered, and the page that started it.
interface Settled extends DateTarget {
  date: string;
  owner: symbol;
  outcome: WriteOutcome;
}

type Listener = (settled: Settled | null) => void;

interface DateWrites {
  version: () => number;
  held: (eventId: number, date: string) => boolean;
  // The one update for this date, or null while another is on its way. Only the release it returns ends it.
  acquire: (target: DateTarget, date: string, owner: symbol) => ((outcome: WriteOutcome) => void) | null;
  subscribe: (listener: Listener) => () => void;
}

function createDateWrites(guarded: (key: string) => boolean = () => false): DateWrites {
  const held = new Map<string, symbol>();
  const listeners = new Set<Listener>();
  let version = 0;

  const notify = (settled: Settled | null) => {
    version++;

    for (const listener of listeners) listener(settled);
  };

  return {
    version: () => version,
    held: (eventId, date) => held.has(`${eventId}:${date}`),
    acquire: (target, date, owner) => {
      const key = `${target.eventId}:${date}`;

      if (held.has(key) || guarded(key)) return null;
      const token = Symbol(key);
      held.set(key, token);
      notify(null);

      return (outcome) => {
        if (held.get(key) !== token) return;
        held.delete(key);
        notify({ eventId: target.eventId, groupId: target.groupId, date, owner, outcome });
      };
    },
    subscribe: (listener) => {
      listeners.add(listener);

      return () => { listeners.delete(listener); };
    },
  };
}

// ---- Guarded date edits (M8.1): one record per event and original day, for one browser credential ----

// One session-wide order for read stamps and settlements: loaders' own epochs are not comparable.
let attemptSeq = 0;

/** @public taken by an exact manager read before it is sent (DateAttempts.stamp). */
export interface ReadStamp {
  readonly key: string;
  readonly seq: number;
  readonly rosterMark: number;
}

/** @public a committed manager read a host shows (DateAttempts.register), until it retires it. */
export interface ReadPermit extends ReadStamp {
  readonly occurrenceId: number;
  readonly revision: string;
}

/** @public one question the writer asked one send: only the editor that holds it may answer it, once. */
export interface ConsentRound {
  readonly key: string;
}

// The visibility the read a save was made from showed, and its series value: what the writer's question must
// be about (lib/visibilityConsent dateDecision), never part of the bytes sent.
interface ShownVisibility {
  readonly visibility: Visibility;
  readonly seriesVisibility: Visibility;
}

// What answered one send. "lost": a readable 401, 403 or 404. "changed": date_changed, with baseSuperseded.
// "consent": a question dateDecision accepted for this send, under its own round. `cause` is the readable
// answer, for the editor that sent it alone (its field copy, the kept dates' counts): the record keeps only
// the token.
/** @public how one send of a guarded date edit ended, as DateAttempts' sends resolve. */
export type EditVerdict =
  | { kind: "stored"; revision: string }
  | { kind: "notSent" }
  | { kind: "unsure" }
  | ({ kind: "consent"; round: ConsentRound } & DateDecision)
  | { kind: "changed"; superseded: boolean }
  | { kind: "refused"; code: string; lost: boolean; cause: ApiError };

interface DraftMark {
  readonly id: string;
  readonly versions: Readonly<Record<string, number>>;
}

interface Unsure {
  kind: "unsure";
  envelope: DateEditEnvelope;
  shown?: ShownVisibility;
  // The draft that submitted these bytes: a replay the date has moved past retires it.
  draft?: DraftMark;
  // The settlement that proved this base superseded: a later read with another base ends the doubt.
  supersededAt?: number;
}

interface Sending {
  kind: "sending";
  envelope: DateEditEnvelope;
  shown?: ShownVisibility;
  via: ReadPermit;
  rosterMark: number;
  // The uncertain attempt this send replays, or answers a question for; null for a first send.
  prior: Unsure | null;
  draft?: DraftMark;
  dispatched: boolean;
}

interface Asking {
  kind: "consent";
  envelope: DateEditEnvelope;
  shown?: ShownVisibility;
  round: ConsentRound;
  token: string;
  rosterMark: number;
  prior: Unsure | null;
  draft?: DraftMark;
}

type Phase = { kind: "idle" } | Unsure | Sending | Asking;

// One send in flight. Everything the transport's callbacks reach goes through `live`, which disposal empties:
// a fetch that never settles then holds no envelope, record or caller.
interface Flight {
  live: { key: string; r: DateRecord; s: Sending; resolve: (v: EditVerdict) => void } | null;
  readonly abort: AbortController;
}

interface DateRecord {
  phase: Phase;
  // Only a read stamped after this may arm Save or Try again.
  readMin: number;
  occurrenceId?: number;
  reads: Set<ReadPermit>;
  settled?: { draft: DraftMark; after: number };
}

/** @public a credential's guarded date edits; hosts reach it through useDateAttempts once they adopt the guarded editor. */
export interface DateAttempts {
  readonly credential: Credential;
  version: () => number;
  subscribe: (listener: () => void) => () => void;
  // Dates whose hosts should read again (a settlement, a roster write that may have changed something),
  // gathered into one call per task; `settled` names those a send's answer asked for, since a host's page
  // already reads again after a roster write by its own rule.
  onRefresh: (listener: (keys: ReadonlySet<string>, settled: ReadonlySet<string>) => void) => () => void;
  stamp: (eventId: number, date: string) => ReadStamp;
  register: (stamp: ReadStamp, base: { occurrenceId: number; revision: string }) => ReadPermit | null;
  retire: (permit: ReadPermit) => void;
  status: (key: string) => Phase["kind"];
  canSave: (permit: ReadPermit) => boolean;
  canReplay: (permit: ReadPermit) => boolean;
  // Whether the writer's question `round` still stands, for this read to answer: no roster write on the date
  // began or ended since it was asked, nobody answered it, and the read may act.
  question: (permit: ReadPermit, round: ConsentRound) => boolean;
  // The uncertain attempt's submitted fields, shown only beside a current eligible read.
  recovery: (permit: ReadPermit) => Pick<DateEditEnvelope, "changes" | "reason"> | null;
  // The draft fields the last stored send submitted, once, to a read made after it.
  takeSettledDraft: (permit: ReadPermit, draftId: string) => Readonly<Record<string, number>> | null;
  // A replay of this draft's uncertain save answered date_changed: what it typed (and its reason) must never
  // become a new save. Its editor, mounted now or later from a hold, starts again from the date as read.
  draftRetired: (draftId: string) => boolean;
  // Each returns null, having sent nothing, when this date cannot send now; the claim is made before any await.
  // `shown` is what the read showed of the date's visibility; without it no question is ever asked.
  save: (permit: ReadPermit, eventId: number, date: string, changes: DateEditChanges, opts?: { reason?: string; draft?: DraftMark; shown?: ShownVisibility }) => Promise<EditVerdict> | null;
  replay: (permit: ReadPermit) => Promise<EditVerdict> | null;
  // Answers question `round` alone: any other round, or one that no longer stands, is left as it is. A yes
  // sends the same attempt with only the question's token added.
  // A no needs no read: it only releases the question.
  answer: (round: ConsentRound, yes: boolean, permit: ReadPermit | null) => Promise<EditVerdict> | null;
  // Ends this lifetime for good (idempotent): it forgets every envelope and read, stops listening to roster
  // writes, sends nothing more and publishes nothing, even for an answer that lands later. Each send in flight
  // is aborted and answers its caller at once: notSent if it never reached fetch, otherwise unsure (it may
  // have been stored), never stored. Only disposal aborts; a host retiring its read does not.
  dispose: () => void;
}

// A success is a settlement only when it names the submitted occurrence and a new revision.
function storedVerdict(value: OccurrenceResponse, sent: DateEditEnvelope): EditVerdict {
  const o = isObject(value) ? value.occurrence : undefined;

  return isObject(o) && o.id === sent.occurrenceId && isString(o.revision) && o.revision ? { kind: "stored", revision: o.revision } : { kind: "unsure" };
}

// The question the send's own save must get: its date's row, the visibility its read showed, and the value
// it asks for, "series" (or null, which also clears the date's own value) resolved from that read. A save that changes no visibility is never asked anything.
function expectedDecision(key: string, s: Sending) {
  const target = s.envelope.changes.visibility;

  if (!s.shown || target === undefined) return null;

  return {
    eventId: Number(key.slice(0, key.indexOf(":"))), occurrenceId: s.envelope.occurrenceId,
    current: s.shown.visibility, target: target === "series" || target === null ? s.shown.seriesVisibility : target,
  };
}

// A question that is not exactly about this send is a refusal: it has no Yes.
function answerVerdict(cause: unknown, key: string, s: Sending): EditVerdict {
  if (!(cause instanceof ApiError) || cause.unreadable || cause.status >= 500) return { kind: "unsure" };
  const { status, code, data } = cause;

  if (status === 409 && code === "date_needs_consent") {
    const expected = expectedDecision(key, s);
    const decision = expected && dateDecision(cause, expected);

    if (decision) return { kind: "consent", round: { key }, ...decision };
  }

  if (status === 409 && code === "date_changed") return { kind: "changed", superseded: data.baseSuperseded === true };

  return { kind: "refused", code, lost: [401, 403, 404].includes(status), cause };
}

/** @public tests drive a credential's store directly; the provider makes one per credential. */
export function createDateAttempts(credential: Credential, rosters: RosterWrites, legacy?: Pick<DateWrites, "held">): DateAttempts {
  const records = new Map<string, DateRecord>();
  // Only draft ids: nothing typed is kept here.
  const retired = new Set<string>();
  const listeners = new Set<() => void>();
  const refreshers = new Set<(keys: ReadonlySet<string>, settled: ReadonlySet<string>) => void>();
  let due: { keys: Set<string>; settled: Set<string> } | null = null;
  let version = 0;
  let disposed = false;

  // Disposal is separate from the credential: the provider can unmount while the credential is still current.
  const alive = () => !disposed && credentialCurrent(credential);

  const blank = (): DateRecord => ({ phase: { kind: "idle" }, readMin: 0, reads: new Set() });

  // A disposed store keeps no record: a getter asked afterwards sees an idle date and stores nothing.
  const rec = (key: string) => {
    if (disposed) return blank();
    let r = records.get(key);

    if (!r) records.set(key, r = blank());

    return r;
  };

  const notify = () => {
    version++;

    for (const listener of listeners) listener();
  };

  const refresh = (key: string, settled = true) => {
    if (!due) {
      due = { keys: new Set(), settled: new Set() };
      queueMicrotask(() => {
        const batch = due ?? { keys: new Set<string>(), settled: new Set<string>() };
        due = null;

        for (const listener of refreshers) listener(batch.keys, batch.settled);
      });
    }

    due.keys.add(key);

    if (settled) due.settled.add(key);
  };

  const bump = (r: DateRecord) => (r.readMin = ++attemptSeq);
  const held = (key: string) => !!legacy?.held(Number(key.slice(0, key.indexOf(":"))), key.slice(key.indexOf(":") + 1));

  // A read that may act now: still shown, stamped after every settlement and every roster write that may
  // have changed the date, for the pinned occurrence, with no roster write in flight.
  const eligible = (p: ReadPermit) => {
    const r = records.get(p.key);

    return !!r && alive() && r.reads.has(p) && p.seq > r.readMin && p.occurrenceId === r.occurrenceId &&
      rosters.pending(p.key) === 0 && rosters.staleAt(p.key) <= p.rosterMark && !held(p.key);
  };

  const flights = new Set<Flight>();

  // What a send's caller hears once this lifetime has ended: never stored, and unsure once it reached fetch.
  const ended = (s: Sending): EditVerdict => (s.dispatched ? { kind: "unsure" } : { kind: "notSent" });

  const settle = (f: Flight, v: EditVerdict) => {
    const live = f.live;

    // Disposal already answered this send's caller and keeps nothing of it.
    if (!live) return;
    f.live = null;
    flights.delete(f);
    const { key, r, s } = live;

    // Settled even while the credential is not current: a dead store's state is inert (every action checks
    // alive), and a recheck that adopts the same credential again must find the answer, not a send in flight.
    if (r.phase === s) {
      const back: Phase = s.prior ?? { kind: "idle" };

      switch (v.kind) {
        case "notSent":
          r.phase = back;
          break;
        case "stored":
          r.phase = { kind: "idle" };
          bump(r);

          if (s.draft) r.settled = { draft: s.draft, after: r.readMin };
          refresh(key);
          break;
        case "unsure":
          r.phase = { kind: "unsure", envelope: s.envelope, shown: s.shown, draft: s.draft };
          bump(r);
          refresh(key);
          break;
        case "consent":
          r.phase = { kind: "consent", envelope: s.envelope, shown: s.shown, round: v.round, token: v.token, rosterMark: rosters.changes(key), prior: s.prior, draft: s.draft };
          break;
        case "changed":
          bump(r);

          // A first send's conflict rebases its draft; an uncertain attempt's retires the draft that made it.
          if (s.prior?.draft) retired.add(s.prior.draft.id);
          r.phase = s.prior ? { ...s.prior, supersededAt: v.superseded ? r.readMin : s.prior.supersededAt } : { kind: "idle" };
          refresh(key);
          break;
        case "refused":
          r.phase = back;

          if (v.lost) {
            bump(r);
            refresh(key);
          }
      }

      notify();
    }

    // A listener may have disposed the store while hearing of this answer.
    live.resolve(disposed ? ended(s) : v);
  };

  // Hands a claimed send to the transport. Its callbacks see only the flight, never the record or envelope
  // directly, so disposal severs them all.
  const dispatch = (f: Flight, envelope: DateEditEnvelope) => {
    // The same check as the claim, once more after CSRF preparation and every await, immediately before fetch.
    const permit = () => {
      const live = f.live;

      return !!live && live.r.phase === live.s && !live.s.dispatched && eligible(live.s.via) && rosters.changes(live.key) === live.s.rosterMark;
    };

    const onDispatch = () => { if (f.live) f.live.s.dispatched = true; };

    DateEdits.send(envelope, credential, { permit, onDispatch, signal: f.abort.signal }).then(
      (value) => { if (f.live) settle(f, storedVerdict(value, f.live.s.envelope)); },
      (cause: unknown) => { if (f.live) settle(f, f.live.s.dispatched ? answerVerdict(cause, f.live.key, f.live.s) : { kind: "notSent" }); },
    );
  };

  const run = (key: string, r: DateRecord, s: Sending): Promise<EditVerdict> => {
    let resolve: (v: EditVerdict) => void = () => {};
    const verdict = new Promise<EditVerdict>((done) => { resolve = done; });
    // Registered before anyone hears of the claim, so a disposal inside that notification still owns it.
    const f: Flight = { live: { key, r, s, resolve }, abort: new AbortController() };
    flights.add(f);
    r.phase = s;
    notify();

    if (f.live) dispatch(f, s.envelope);

    return verdict;
  };

  // A question stands while no roster write on the date began or ended since it was asked.
  const standing = (key: string, r: DateRecord): r is DateRecord & { phase: Asking } =>
    r.phase.kind === "consent" && rosters.pending(key) === 0 && rosters.changes(key) === r.phase.rosterMark;

  // Reads need no bump here: eligible compares each read's stamp with the roster's last end (staleAt).
  const unsubscribe = rosters.subscribe((event) => {
    const r = records.get(event.key);

    if (r?.phase.kind === "consent" && !standing(event.key, r)) r.phase = r.phase.prior ?? { kind: "idle" };

    if (event.end && event.end !== "answered") refresh(event.key, false);
    notify();
  });

  return {
    credential,
    version: () => version,
    subscribe: (listener) => {
      if (disposed) return () => {};

      listeners.add(listener);

      return () => { listeners.delete(listener); };
    },
    onRefresh: (listener) => {
      if (disposed) return () => {};

      refreshers.add(listener);

      return () => { refreshers.delete(listener); };
    },
    stamp: (eventId, date) => ({ key: rosterKey(eventId, date), seq: ++attemptSeq, rosterMark: rosters.mark() }),
    register: (stamp, base) => {
      if (!alive() || !base.revision || !Number.isSafeInteger(base.occurrenceId) || base.occurrenceId <= 0) return null;
      const r = rec(stamp.key);

      if (r.occurrenceId !== base.occurrenceId) {
        // Another row now holds this day: only an idle date takes it, and reads of the old one stop counting.
        if (r.phase.kind !== "idle" && r.occurrenceId !== undefined) return null;
        r.occurrenceId = base.occurrenceId;
      }

      const permit: ReadPermit = { ...stamp, occurrenceId: base.occurrenceId, revision: base.revision };
      r.reads.add(permit);
      const p = r.phase;

      // A replay proved the doubtful base older than the date, and this read, made after that, shows another.
      if (p.kind === "unsure" && p.supersededAt !== undefined && stamp.seq > p.supersededAt && base.revision !== p.envelope.revision) {
        r.phase = { kind: "idle" };
      }

      notify();

      return permit;
    },
    retire: (permit) => {
      if (records.get(permit.key)?.reads.delete(permit)) notify();
    },
    status: (key) => records.get(key)?.phase.kind ?? "idle",
    canSave: (p) => eligible(p) && rec(p.key).phase.kind === "idle",
    canReplay: (p) => eligible(p) && rec(p.key).phase.kind === "unsure",
    question: (p, round) => {
      const r = rec(p.key);

      return standing(p.key, r) && r.phase.round === round && eligible(p);
    },
    recovery: (p) => {
      const ph = rec(p.key).phase;
      const doubt = ph.kind === "unsure" ? ph : ph.kind === "idle" ? null : ph.prior;

      return doubt && eligible(p) ? { changes: doubt.envelope.changes, reason: doubt.envelope.reason } : null;
    },
    takeSettledDraft: (p, draftId) => {
      const r = rec(p.key);
      const s = r.settled;

      if (!s || s.draft.id !== draftId || p.seq <= s.after || !eligible(p)) return null;
      r.settled = undefined;

      return s.draft.versions;
    },
    draftRetired: (draftId) => retired.has(draftId),
    save: (p, eventId, date, changes, opts = {}) => {
      if (rosterKey(eventId, date) !== p.key || !eligible(p) || rec(p.key).phase.kind !== "idle") return null;
      const envelope = DateEdits.freeze(eventId, date, p.occurrenceId, p.revision, changes, opts.reason);
      const shown = opts.shown && Object.freeze({ visibility: opts.shown.visibility, seriesVisibility: opts.shown.seriesVisibility });

      return envelope && run(p.key, rec(p.key), { kind: "sending", envelope, shown, via: p, rosterMark: rosters.changes(p.key), prior: null, draft: opts.draft, dispatched: false });
    },
    replay: (p) => {
      const r = rec(p.key);
      const ph = r.phase;

      // A stored replay settles the draft that made the attempt, as its first send would have.
      return ph.kind === "unsure" && eligible(p) ? run(p.key, r, { kind: "sending", envelope: ph.envelope, shown: ph.shown, via: p, rosterMark: rosters.changes(p.key), prior: ph, draft: ph.draft, dispatched: false }) : null;
    },
    answer: (round, yes, p) => {
      const r = records.get(round.key);
      const ph = r?.phase;

      // Another round's answer (a Yes kept from a question that closed) never answers this one.
      if (!r || ph?.kind !== "consent" || ph.round !== round) return null;

      // A no, or a question that no longer stands, restores what was there before it: a first save's claim is
      // released, a replay's doubt keeps the bytes it may have sent.
      if (!yes || !p || p.key !== round.key || !standing(round.key, r) || !eligible(p)) {
        r.phase = ph.prior ?? { kind: "idle" };
        notify();

        return null;
      }

      const envelope = DateEdits.withConsent(ph.envelope, ph.token);

      return run(round.key, r, { kind: "sending", envelope, shown: ph.shown, via: p, rosterMark: ph.rosterMark, prior: ph.prior, draft: ph.draft, dispatched: false });
    },
    dispose: () => {
      if (disposed) return;
      disposed = true;
      unsubscribe();

      for (const f of flights) {
        const live = f.live;
        f.live = null;
        f.abort.abort();
        live?.resolve(ended(live.s));
      }

      flights.clear();
      records.clear();
      retired.clear();
      listeners.clear();
      refreshers.clear();
      due = null;
      version++;
    },
  };
}

const DateWritesContext = createContext<DateWrites | null>(null);
const DateAttemptsContext = createContext<DateAttempts | null>(null);

// Held for the signed-in session (App puts it inside the user-keyed Layout, around every route): leaving a
// page while a date's update is on its way and coming back, or opening the same date from the event page or
// its group's page, keeps that date closed to a second update until it answers. Roster writes in flight are
// counted here too, per date, so they outlive the roster that started them and every host that remounts.
// Guarded date edits are held per browser credential object: a routine recheck adopts the same object and keeps
// them. core never adopts a replaced or suspended object again, so a change of object (or none) disposes the old
// store, as does a tab close, reload or remount. The store is made after commit, so a render React abandons or
// Strict Mode's rehearsal leaves nothing listening.
export function DateWritesProvider({ children, rosterWrites, credential = null }: { children: ReactNode; rosterWrites?: RosterWrites; credential?: Credential | null }) {
  const live = useRef<DateAttempts | null>(null);
  const [store] = useState(() => createDateWrites((key) => (live.current?.status(key) ?? "idle") !== "idle"));
  const [ownRosters] = useState(createRosterWrites);
  const rosters = rosterWrites ?? ownRosters;
  const [made, setMade] = useState<DateAttempts | null>(null);

  useLayoutEffect(() => {
    if (!credential) return;
    const next = createDateAttempts(credential, rosters, store);
    live.current = next;
    setMade(next);

    return () => {
      next.dispose();

      if (live.current === next) live.current = null;
    };
  }, [credential, rosters, store]);

  // Until the layout effect for this credential has run, the old store (already disposed, or about to be) is never shown.
  const attempts = made && credential && made.credential === credential ? made : null;

  return <DateWritesContext.Provider value={store}><RosterWritesContext.Provider value={rosters}><DateAttemptsContext.Provider value={attempts}>{children}</DateAttemptsContext.Provider></RosterWritesContext.Provider></DateWritesContext.Provider>;
}

/** @public the guarded date edits of this tab's credential, or null with none; re-renders on every change. */
export function useDateAttempts(): DateAttempts | null {
  const attempts = useContext(DateAttemptsContext);
  const subscribe = useCallback((changed: () => void) => attempts?.subscribe(changed) ?? (() => {}), [attempts]);
  useSyncExternalStore(subscribe, () => attempts?.version() ?? 0);

  return attempts;
}

/** @public registers a manager read once it is committed on screen and retires it when it is replaced or
 * leaves: `stamp` was taken before the read was sent. */
export function useEditBase(attempts: DateAttempts | null, stamp: ReadStamp | null, base: { occurrenceId: number; revision: string } | null): ReadPermit | null {
  const [permit, setPermit] = useState<ReadPermit | null>(null);
  const occurrenceId = base?.occurrenceId;
  const revision = base?.revision;

  useLayoutEffect(() => {
    const p = attempts && stamp && occurrenceId !== undefined && revision !== undefined ? attempts.register(stamp, { occurrenceId, revision }) : null;
    setPermit(p);

    return () => { if (p) attempts?.retire(p); };
  }, [attempts, stamp, occurrenceId, revision]);

  return permit && attempts?.credential && credentialCurrent(attempts.credential) ? permit : null;
}

// What a page that edits dates tells the store about itself: its one useLoad, and which dates it lists.
interface DatePage {
  epoch: () => number;
  dataEpoch: number;
  // Whether this page lists dates of the target's event. Decided from the page's address, not from what it
  // has loaded: a read on its way, or one answered and not yet shown, may hold the date.
  shows: (target: DateTarget) => boolean;
  // Ask for the page again, keeping what it shows meanwhile (useLoad's reload).
  reload: () => void;
  // A page with no roster gate of its own (EventDetail, GroupDetail): a roster write on a date it shows that
  // may have changed something (any end but "answered") holds the date as an update does, and the page is
  // asked again. ManageDate leaves it off: its gate is the one starter of the read after a roster write.
  rosters?: boolean;
}

// One page's date updates (EventDetail, GroupDetail). A date stays closed (`writing`, then `settling`) from
// the moment its update is sent until the page shows an answer it asked for after that update answered: an
// editor opened on the old answer would send its old values back. When an update that may have stored
// something answers, the page notes its epoch and is asked again. Every ask moves the epoch, so no read
// started before can be shown after it (useLoad drops it, and Load more checks the epoch), and a local
// change made from an older answer keeps that answer's dataEpoch. After an update this page stored, its
// editor asks for the page (DateEditor's onSaved); every other one is asked for here. A failed read keeps the
// date closed until a later one lands, such as the page's Try again. Outside a DateWritesProvider (a page
// rendered on its own) the page holds its own. With `rosters`, a roster write's end on a date the page shows
// that may have changed something counts as that date's update answering, from another owner. An editor's own
// question about who is signed up follows the write itself (DateEditor's useRosterActivity).
export function useDateWrites(page: DatePage) {
  const shared = useContext(DateWritesContext);
  const [own] = useState(createDateWrites);
  const store = shared ?? own;
  const sharedRosters = useContext(RosterWritesContext);
  const [ownRosters] = useState(createRosterWrites);
  const rosters = sharedRosters ?? ownRosters;
  const watchRosters = page.rosters === true;
  const [owner] = useState(() => Symbol("date page"));
  const latest = useRef(page);
  useSyncExternalStore(store.subscribe, store.version);
  // For each date, the epoch its last update answered at.
  const [answered, setAnswered] = useState(() => new Map<string, number>());

  useLayoutEffect(() => {
    latest.current = page;
  });

  useEffect(() => store.subscribe((settled) => {
    if (!settled || settled.outcome === "refused") return;
    const mine = settled.owner === owner;

    if (!mine && !latest.current.shows(settled)) return;
    const at = latest.current.epoch();
    setAnswered((old) => new Map(old).set(`${settled.eventId}:${settled.date}`, at));

    if (!mine || settled.outcome === "unsure") latest.current.reload();
  }), [store, owner]);

  useEffect(() => {
    if (!watchRosters) return;

    return rosters.subscribe((event) => {
      if (event.end === undefined || event.end === "answered") return;

      if (!latest.current.shows({ eventId: event.eventId })) return;
      const at = latest.current.epoch();
      setAnswered((old) => new Map(old).set(event.key, at));
      latest.current.reload();
    });
  }, [rosters, watchRosters]);

  return {
    writing: (eventId: number, date: string) => store.held(eventId, date),
    // The update answered, and the page does not yet show an answer asked for after it.
    settling: (eventId: number, date: string) => {
      const at = answered.get(`${eventId}:${date}`);

      return at !== undefined && page.dataEpoch <= at;
    },
    acquire: (target: DateTarget, date: string) => store.acquire(target, date, owner),
  };
}
