import { useEffect, useLayoutEffect, useRef, useState } from "react";
import { ApiError } from "../api/core";
import { Events } from "../api/events";
import type { EventOccurrence, Visibility } from "../types";
import { useDateAttempts, useEditBase, type DateAttempts, type ReadPermit, type ReadStamp } from "./dateWrites";
import { isBoolean, isNumber, isString, type JsonValue } from "./wireValues";

// What a listed date names to open its editor (M8.1): which date, never what it says. Nothing here is shown,
// sent as a change or taken as the right to edit; the exact manager read decides all of that.
export interface DateOpener {
  eventId: number;
  // The original day, also for a moved date.
  date: string;
  occurrenceId: number;
  // The group page's own group: the date must still belong to it.
  groupId?: number;
}

// The one date a manager edits, from a fresh manager read: its own form inputs and token, and nothing else (no
// roster, counts or other people). Each host's editor is built from this alone.
export interface EditBase {
  id: number;
  eventId: number;
  date: string;
  revision: string;
  startsLocal: string;
  durationMinutes: number;
  tzid: string;
  title: string;
  location: string;
  capacity: number | null;
  remoteCapacity: number | null;
  remoteAvailable: boolean;
  visibility: Visibility;
  visibilityOverride?: Visibility;
  seriesVisibility: Visibility;
  churchWide: boolean;
  groupName?: string;
  groupVisibility?: Visibility;
}

// The read's fields are checked here, where its JSON enters the editor, whatever its static type says: the
// server's own formats (a wall-clock minute) and what its schema can hold (a positive duration, no negative seats),
// so nothing the editor shows or diffs against differs from what the server stored. Upper limits are the
// writer's to enforce (events.ValidDuration, maxSeatLimit): a date stored before one would still open.
const visibilities: readonly JsonValue[] = ["public", "private", "invite_only"];
const isVisibility = (value: JsonValue | undefined): value is Visibility => value !== undefined && visibilities.includes(value);
const optionalVisibility = (value: JsonValue | undefined) => value === undefined || visibilities.includes(value);
const atLeast = (value: JsonValue | undefined, min: number) => isNumber(value) && Number.isSafeInteger(value) && value >= min;
const isSeats = (value: JsonValue | undefined) => value === null || atLeast(value, 0);
const isId = (value: JsonValue | undefined) => atLeast(value, 1);
const dayPattern = /^(\d{4})-(\d{2})-(\d{2})$/;
const minutePattern = /^(\d{4}-\d{2}-\d{2})T(\d{2}):(\d{2})$/;

function isDay(value: JsonValue | undefined): value is string {
  const m = isString(value) ? dayPattern.exec(value) : null;

  if (!m) return false;
  const [y, mo, d] = [Number(m[1]), Number(m[2]), Number(m[3])];
  const at = new Date(Date.UTC(y, mo - 1, d));

  return at.getUTCFullYear() === y && at.getUTCMonth() === mo - 1 && at.getUTCDate() === d;
}

// The event's own clock to the minute ("2026-12-01T19:00"), as the server writes it.
function isWallMinute(value: JsonValue | undefined): value is string {
  const m = isString(value) ? minutePattern.exec(value) : null;

  return !!m && isDay(m[1]) && Number(m[2]) < 24 && Number(m[3]) < 60;
}

// The read answers the date the opener names, scheduled, on the expected row and (on the group page) still in
// that group, with a token and every form field well formed; anything else is null and opens nothing.
export function editBaseOf(o: EventOccurrence | undefined, opener: DateOpener): EditBase | null {
  if (!o || o.cancelled === true) return null;

  if (o.id !== opener.occurrenceId || o.eventId !== opener.eventId || o.date !== opener.date || !isId(o.id) || !isDay(o.date)) return null;

  if (opener.groupId !== undefined && o.groupId !== opener.groupId) return null;

  if (!isString(o.revision) || !o.revision || !isWallMinute(o.startsLocal) || !atLeast(o.durationMinutes, 1)) return null;

  if (!isString(o.tzid) || !o.tzid || o.tzid.length > 64 || !isString(o.title) || !isString(o.location)) return null;

  if (!(o.capacity === null || atLeast(o.capacity, 1)) || !isSeats(o.remoteCapacity) || !isBoolean(o.remoteAvailable) || !isBoolean(o.churchWide)) return null;

  if (!isVisibility(o.visibility) || !optionalVisibility(o.visibilityOverride) || !isVisibility(o.seriesVisibility) || !optionalVisibility(o.groupVisibility)) return null;

  if (o.groupName !== undefined && !isString(o.groupName)) return null;

  if (o.groupId !== undefined && !isId(o.groupId)) return null;

  // A shown group's id, name and visibility come together (labelGroup), and a church-wide event has none.
  const group = [o.groupId, o.groupName, o.groupVisibility].filter((v) => v !== undefined).length;

  if (group !== 0 && (group !== 3 || o.churchWide)) return null;

  return {
    id: o.id, eventId: o.eventId, date: o.date, revision: o.revision,
    startsLocal: o.startsLocal, durationMinutes: o.durationMinutes, tzid: o.tzid, title: o.title, location: o.location,
    capacity: o.capacity, remoteCapacity: o.remoteCapacity, remoteAvailable: o.remoteAvailable,
    visibility: o.visibility, visibilityOverride: o.visibilityOverride, seriesVisibility: o.seriesVisibility,
    churchWide: o.churchWide, groupName: o.groupName, groupVisibility: o.groupVisibility,
  };
}

// The read answered, but not the date the list named (moved to another row, another group, cancelled).
class NotThisDate extends Error {}

// A manager read of one date, stamped before it is sent and pinned to the credential its attempts belong to.
async function readEditBase(attempts: DateAttempts, opener: DateOpener): Promise<{ stamp: ReadStamp; base: EditBase }> {
  const stamp = attempts.stamp(opener.eventId, opener.date);
  const read = await Events.managedDate(opener.eventId, opener.date, attempts.credential);
  const base = editBaseOf(read.occurrence, opener);

  if (!base) throw new NotThisDate();

  return { stamp, base };
}

// "denied": the server said this person may not manage the date (a readable 401, 403 or 404), or it answered
// another date than the list named; the host checks its page again. A credential the server no longer
// recognizes is not a refusal of the date: the tab checks its credential, and the read can be asked again.
export function editReadRefused(cause: unknown): boolean {
  if (cause instanceof NotThisDate) return true;

  return cause instanceof ApiError && !cause.unreadable && cause.code !== "session_expired" && [401, 403, 404].includes(cause.status);
}

export type EditRead =
  | { kind: "loading" }
  | { kind: "ready"; base: EditBase; permit: ReadPermit | null; attempts: DateAttempts; reading: boolean }
  | { kind: "failed"; retry: () => void }
  | { kind: "denied" }
  // No credential this tab can pin a read to.
  | { kind: "unavailable" };

interface Answer {
  owner: string;
  attempts: DateAttempts;
  result: { stamp: ReadStamp; base: EditBase } | { refused: boolean };
}

export const dateKey = (eventId: number, date: string) => `${eventId}:${date}`;

// One editor's exact read (EventDetail, GroupDetail). It reads when asked to `open` and again whenever `owner`
// changes (the host's route, its shown answer, the row being hidden and shown again), so nothing read under one
// owner is shown or registered under another; meanwhile it is loading and shows nothing. A settlement or a roster
// write that may have changed the date asks the host for its page (useEditRefresh, useDateWrites); the date is
// read again only once that answer is shown, and until then the old read no longer counts, so Save stays closed.
// The answer is registered once it is committed on screen and retired when it is replaced.
export function useEditRead(opener: DateOpener, owner: string, open: boolean, onRefused: () => void): EditRead {
  const attempts = useDateAttempts();
  const key = dateKey(opener.eventId, opener.date);
  const want = open && attempts ? `${owner}|${key}|${opener.occurrenceId}|${opener.groupId ?? ""}` : null;
  const [asked, setAsked] = useState(0);
  const [answer, setAnswer] = useState<Answer | null>(null);
  const [reading, setReading] = useState(false);
  const latest = useRef({ opener, onRefused });
  const seq = useRef(0);

  useLayoutEffect(() => {
    latest.current = { opener, onRefused };
  });

  useEffect(() => {
    if (!want || !attempts) return;
    const mine = ++seq.current;
    setReading(true);
    readEditBase(attempts, latest.current.opener).then(
      (result) => { if (seq.current === mine) { setAnswer({ owner: want, attempts, result }); setReading(false); } },
      (cause: unknown) => { if (seq.current === mine) { setAnswer({ owner: want, attempts, result: { refused: editReadRefused(cause) } }); setReading(false); } },
    );

    return () => { seq.current++; };
  }, [want, attempts, asked]);

  const mine = answer && answer.owner === want && answer.attempts === attempts ? answer : null;
  const read = mine && "stamp" in mine.result ? mine.result : null;
  const refused = !!mine && "refused" in mine.result && mine.result.refused;

  useEffect(() => {
    if (refused) latest.current.onRefused();
  }, [refused]);

  const permit = useEditBase(attempts, read?.stamp ?? null, read ? { occurrenceId: read.base.id, revision: read.base.revision } : null);

  if (!attempts) return { kind: "unavailable" };

  if (!mine) return { kind: "loading" };

  if (read) return { kind: "ready", base: read.base, permit, attempts, reading };

  if (reading) return { kind: "loading" };

  return refused ? { kind: "denied" } : { kind: "failed", retry: () => setAsked((n) => n + 1) };
}

// A host's page shows these dates' events: after a guarded edit of one of them settles, from this page or
// another, it is asked for again (once per batch of dates). A roster write's end is left to the host's own rule
// (useDateWrites' rosters, ManageDate's gate).
export function useEditRefresh(shows: (eventId: number) => boolean, reload: () => void) {
  const attempts = useDateAttempts();
  const latest = useRef({ shows, reload });

  useLayoutEffect(() => {
    latest.current = { shows, reload };
  });

  useEffect(() => attempts?.onRefresh((_keys, settled) => {
    if ([...settled].some((key) => latest.current.shows(Number(key.slice(0, key.indexOf(":")))))) latest.current.reload();
  }), [attempts]);
}
