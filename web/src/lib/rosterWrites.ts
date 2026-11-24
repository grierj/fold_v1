import { createContext, useCallback, useContext, useLayoutEffect, useMemo, useRef, useState, useSyncExternalStore } from "react";
import { ApiError } from "../api/core";
import type { LoadOutcome } from "./useLoad";

// How a date's update ended. "refused": the server answered no, so nothing was stored. "unsure": the answer
// never arrived or could not be read (the connection dropped, a server or proxy failure), so it may have been.
export type WriteOutcome = "stored" | "refused" | "unsure";

// Only an answer the server gave and we could read says nothing was stored.
export function outcomeOf(cause: unknown): WriteOutcome {
  return cause instanceof ApiError && cause.status < 500 && !cause.unreadable ? "refused" : "unsure";
}

// How a roster write (Remove, promote, restore, add) failed. "lost": a readable 401, 403 or 404, so this person
// may no longer manage the date. "unsure": what outcomeOf calls unsure, so it may have been stored. "answered":
// any other readable answer (event_full, date_add_approval_required, a validation error): nothing changed.
export type RosterWriteFailure = "lost" | "unsure" | "answered";

/** @public how a roster write ended, as tests end one through a store. */
export type RosterWriteEnd = "stored" | RosterWriteFailure;

export function rosterWriteFailure(cause: unknown): RosterWriteFailure {
  if (outcomeOf(cause) === "unsure") return "unsure";

  return cause instanceof ApiError && [401, 403, 404].includes(cause.status) ? "lost" : "answered";
}

// A roster write started, or ended (`end` set), on `key` (`${eventId}:${date}`) of event `eventId`.
interface RosterWriteEvent {
  key: string;
  eventId: number;
  end?: RosterWriteEnd;
}

/** @public the store's shape; hosts reach it only through useRosterWrites, useRosterGate and the activity hooks. */
export interface RosterWrites {
  version: () => number;
  // Writes in flight for `${eventId}:${date}`.
  pending: (key: string) => number;
  // Moves at every begin and every end on `${eventId}:${date}`: the session sequence of its last one, 0 before any.
  changes: (key: string) => number;
  // The session sequence now; a begin or end anywhere moves it.
  mark: () => number;
  // The session sequence of the last end on `${eventId}:${date}` other than "answered", 0 before any.
  staleAt: (key: string) => number;
  // Some listed event had a roster write begin or end after `mark`.
  changedSince: (mark: number, eventIds: readonly number[]) => boolean;
  // Writes in flight on any listed event.
  pendingFor: (eventIds: readonly number[]) => number;
  // Starts one write; the release it returns ends it once (a second call does nothing).
  begin: (key: string) => (end: RosterWriteEnd) => void;
  subscribe: (listener: (event: RosterWriteEvent) => void) => () => void;
}

const eventOfKey = (key: string) => Number(key.slice(0, key.indexOf(":")));

// Every count moves before listeners hear of it, so a check made straight after an await sees a begin or an
// end that happened during it, before React renders.
/** @public tests drive a provider's store directly. */
export function createRosterWrites(): RosterWrites {
  const pending = new Map<string, number>();
  const pendingByEvent = new Map<number, number>();
  const lastByKey = new Map<string, number>();
  const lastByEvent = new Map<number, number>();
  const staleByKey = new Map<string, number>();
  const listeners = new Set<(event: RosterWriteEvent) => void>();
  let version = 0;
  let seq = 0;

  const count = <K,>(map: Map<K, number>, key: K, by: number) => {
    const left = (map.get(key) ?? 0) + by;

    if (left > 0) map.set(key, left);
    else map.delete(key);
  };

  const record = (key: string, eventId: number, by: number) => {
    seq++;
    count(pending, key, by);
    count(pendingByEvent, eventId, by);
    lastByKey.set(key, seq);
    lastByEvent.set(eventId, seq);
  };

  const notify = (event: RosterWriteEvent) => {
    version++;

    for (const listener of listeners) listener(event);
  };

  return {
    version: () => version,
    pending: (key) => pending.get(key) ?? 0,
    changes: (key) => lastByKey.get(key) ?? 0,
    mark: () => seq,
    staleAt: (key) => staleByKey.get(key) ?? 0,
    changedSince: (mark, eventIds) => eventIds.some((id) => (lastByEvent.get(id) ?? 0) > mark),
    pendingFor: (eventIds) => [...new Set(eventIds)].reduce((n, id) => n + (pendingByEvent.get(id) ?? 0), 0),
    begin: (key) => {
      const eventId = eventOfKey(key);
      record(key, eventId, 1);
      notify({ key, eventId });
      let ended = false;

      return (end) => {
        if (ended) return;
        ended = true;
        record(key, eventId, -1);

        if (end !== "answered") staleByKey.set(key, seq);
        notify({ key, eventId, end });
      };
    },
    subscribe: (listener) => {
      listeners.add(listener);

      return () => { listeners.delete(listener); };
    },
  };
}

export const RosterWritesContext = createContext<RosterWrites | null>(null);

export const rosterKey = (eventId: number, date: string) => `${eventId}:${date}`;

// What ManageRoster uses: whether any roster write on this date is in flight, and how to start one.
export interface RosterGate {
  writing: boolean;
  begin: () => (end: RosterWriteEnd) => void;
}

function usePending(store: RosterWrites, key: string): number {
  const subscribe = useCallback((changed: () => void) => store.subscribe(() => changed()), [store]);

  return useSyncExternalStore(subscribe, () => store.pending(key));
}

const unregistered = () => {};

// A roster's own registration, for a host with no gate (EventDetail's date rows). Outside a provider it
// registers nothing and nothing is ever in flight.
export function useRosterWrites(eventId: number, date: string): RosterGate {
  const shared = useContext(RosterWritesContext);
  const [none] = useState(createRosterWrites);
  const store = shared ?? none;
  const key = rosterKey(eventId, date);
  const writing = usePending(store, key) > 0;

  return useMemo(() => ({ writing, begin: shared ? () => shared.begin(key) : () => unregistered }), [writing, shared, key]);
}

// The roster store this component shares: the session's, or outside a provider one of its own.
function useRosterStore(): RosterWrites {
  const shared = useContext(RosterWritesContext);
  const [own] = useState(createRosterWrites);

  return shared ?? own;
}

// One date's roster activity, for its editor (DateEditor, on every host): `writing` while a write on the date is
// in flight, and `changes` (read when called) moves at every begin and every end, so a question asked before
// one goes stale and a save checking straight after its own await sees it.
export function useRosterActivity(eventId: number, date: string): { writing: boolean; changes: () => number } {
  const store = useRosterStore();
  const key = rosterKey(eventId, date);
  const subscribe = useCallback((changed: () => void) => store.subscribe(() => changed()), [store]);
  const writing = useSyncExternalStore(subscribe, () => store.pending(key)) > 0;
  useSyncExternalStore(subscribe, () => store.changes(key));

  return useMemo(() => ({ writing, changes: () => store.changes(key) }), [writing, store, key]);
}

// Roster activity over whole events, for the event and group visibility reviews: take a `mark` before each
// request and, after it, ask whether a matching write began or ended since (`changedSince`) or is still in
// flight (`pendingFor`). The component renders again at every begin and end anywhere in the session.
export function useRosterActivityOf() {
  const store = useRosterStore();
  useSyncExternalStore(store.subscribe, store.version);

  return useMemo(() => ({
    mark: store.mark,
    changedSince: store.changedSince,
    pendingFor: store.pendingFor,
    subscribe: store.subscribe,
  }), [store]);
}

// What a host that fails closed around a roster tells the gate about its one useLoad.
interface GatedLoad {
  epoch: () => number;
  dataEpoch: number;
  retry: () => Promise<LoadOutcome>;
}

// A host that fails closed around a roster (ManageDateTools, the kept-dates panel). Once a recheck replaces a
// roster whose write is in flight, `closed` keeps every roster of the date unmounted until that write has ended
// and a read asked after the end has answered; a host that mounts while a write is in flight starts closed.
// The gate is the one starter of the read after a write: one retry after "stored", "lost" or "unsure", none
// after "answered". `note` says the last write ended "lost" or "unsure" and the answer shown was asked after
// it. A host reads after a write even with no roster of its own on screen: ManageDateTools with only its editor
// open still holds the managed facts the editor's questions were asked under, and a panel's write inside that
// editor changes them. useLoad sends a request from its effect, so a gate whose host unmounts in the commit that
// records the end (a panel inside an editor its outer gate closes) sends nothing; the host that mounts it again reads.
// Outside a provider the gate keeps its own store, which only this host and its roster share.
export function useRosterGate(eventId: number, date: string, load: GatedLoad) {
  const shared = useContext(RosterWritesContext);
  const [own] = useState(createRosterWrites);
  const store = shared ?? own;
  const key = rosterKey(eventId, date);
  const pending = usePending(store, key);
  const latest = useRef(load);
  // Every read this host makes was asked after a write already in flight when it mounted began.
  const [since, setSince] = useState<number | undefined>(() => (store.pending(key) > 0 ? -Infinity : undefined));
  const [settledAt, setSettledAt] = useState<number | undefined>(undefined);
  const [noteAfter, setNoteAfter] = useState<number | undefined>(undefined);

  useLayoutEffect(() => {
    latest.current = load;
  });

  useLayoutEffect(() => {
    // A write that began between this host's first render and now.
    if (store.pending(key) > 0) setSince((s) => s ?? -Infinity);

    return store.subscribe((event) => {
      if (event.key !== key) return;
      const left = store.pending(key);

      if (event.end === undefined) {
        setNoteAfter(undefined);

        if (left === 1) {
          // Read now: a retry later in the same batch moves the epoch before an updater would run.
          const at = latest.current.epoch();
          setSince((s) => s ?? at);
        }

        return;
      }

      if (left === 0) setSince(undefined);

      if (event.end === "answered") return;
      // Read the epoch, then ask: the read the retry starts is newer than settledAt.
      const at = latest.current.epoch();
      setSettledAt(at);

      if (event.end !== "stored") setNoteAfter(at);
      void latest.current.retry();
    });
  }, [store, key]);

  const stale = settledAt !== undefined && load.dataEpoch <= settledAt;
  const closed = (pending > 0 && since !== undefined && load.dataEpoch > since) || stale;
  const gate = useMemo<RosterGate>(() => ({ writing: pending > 0, begin: () => store.begin(key) }), [pending, store, key]);

  return {
    gate,
    closed,
    note: noteAfter !== undefined && load.dataEpoch > noteAfter && !closed,
    clearNote: useCallback(() => setNoteAfter(undefined), []),
  };
}
