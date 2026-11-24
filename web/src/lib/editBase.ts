import { useEffect, useLayoutEffect, useRef, useState } from "react";
import { ApiError, currentCredential } from "../api/core";
import { Events, type DateEditChanges } from "../api/events";
import type { EventOccurrence, Visibility } from "../types";
import type { RevisionRead } from "./useRevisionSave";
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

// The one date a manager edits, from a fresh manager read: its own form inputs and token, the event's values the date
// falls back to (null where the read does not carry one), and nothing else (no roster, counts or other people). Each
// host's editor is built from this alone.
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
  seriesStartsLocal: string | null;
  seriesDurationMinutes: number | null;
  seriesTitle: string | null;
  seriesLocation: string | null;
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
    seriesStartsLocal: isWallMinute(o.seriesStartsLocal) ? o.seriesStartsLocal : null,
    seriesDurationMinutes: atLeast(o.seriesDurationMinutes, 1) ? o.seriesDurationMinutes : null,
    seriesTitle: isString(o.seriesTitle) ? o.seriesTitle : null,
    seriesLocation: isString(o.seriesLocation) ? o.seriesLocation : null,
  };
}

// The manager's read of one date: a participating read pinned to the credential this tab holds, when it holds one.
const managedRead = (opener: DateOpener) => Events.managedDate(opener.eventId, opener.date, currentCredential() ?? undefined);

// The server said this person may not manage the date (a readable 401, 403 or 404). A credential the server no longer
// recognizes is not a refusal of the date: the tab checks its credential, and the read can be asked again.
export function editReadRefused(cause: unknown): boolean {
  return cause instanceof ApiError && !cause.unreadable && cause.code !== "session_expired" && [401, 403, 404].includes(cause.status);
}

type EditRead =
  | { kind: "loading" }
  | { kind: "ready"; base: EditBase }
  | { kind: "failed"; retry: () => void }
  | { kind: "denied" };

// A listed date's read for its editor (EventDetail, GroupDetail), once while `open` for each `owner` (the host's route,
// the date, its row, its group): nothing read under one owner is shown under another, and a row hidden and shown
// again, or a page that failed and loaded again, keeps the read it has. "denied": the server refused it, or it answered
// another date than the list named (moved to another row or group, cancelled); the host checks its page again.
export function useEditRead(opener: DateOpener, owner: string, open: boolean, onRefused: () => void): EditRead {
  const [asked, setAsked] = useState(0);
  const [answer, setAnswer] = useState<{ owner: string; asked: number; result: EditBase | { refused: boolean } } | null>(null);
  const latest = useRef({ opener, onRefused });
  const mine = answer?.owner === owner && answer.asked === asked ? answer : null;
  const answered = mine !== null;

  useLayoutEffect(() => {
    latest.current = { opener, onRefused };
  });

  useEffect(() => {
    if (!open || answered) return;
    let current = true;
    managedRead(latest.current.opener).then(
      (read) => { if (current) setAnswer({ owner, asked, result: editBaseOf(read.occurrence, latest.current.opener) ?? { refused: true } }); },
      (cause: unknown) => { if (current) setAnswer({ owner, asked, result: { refused: editReadRefused(cause) } }); },
    );

    return () => { current = false; };
  }, [open, owner, asked, answered]);

  const refused = !!mine && "refused" in mine.result && mine.result.refused;

  useEffect(() => {
    if (refused) latest.current.onRefused();
  }, [refused]);

  if (!mine) return { kind: "loading" };

  if (!("refused" in mine.result)) return { kind: "ready", base: mine.result };

  return refused ? { kind: "denied" } : { kind: "failed", retry: () => setAsked((n) => n + 1) };
}

// One field a date save can write, as a read shows it: its value as the form reads it, and whether the read proves the
// value is the date's own rather than one it falls back to (the event's, or the group's place), which a later series
// change would move.
interface Held<V> {
  value: V;
  own: boolean;
}

/** @public what a single-date save can write, as dateFields reads it; its test compares one field at a time. */
export interface DateFields {
  startsLocal: Held<string>;
  durationMinutes: Held<number>;
  title: Held<string>;
  location: Held<string>;
  // "event": a cleared limit, which falls back to the event's, a value this read cannot show.
  capacity: Held<number | null | "event">;
  remoteCapacity: Held<number | null | "event">;
  visibility: Visibility | "series";
  // What gives the values above their meaning: the clock they are on, what "same as the series" is, and whether the
  // date has remote seats at all.
  tzid: string;
  seriesVisibility: Visibility;
  remoteAvailable: boolean;
}

const held = <V,>(value: V, own: boolean): Held<V> => ({ value, own });

// The one normaliser over everything a single-date save can write (DEC-164), for the read the editor opened with and
// for the read after a refusal, so useRevisionSave can tell "already stored", "as it opened" and "changed" apart. A date
// field is either the date's own or the value it falls back to, and the manager's read shows only the value that
// applies, beside some of the fallbacks: a field counts as the date's own only where the read proves it (the value
// differs from the fallback it shows). The capacities have no fallback in the read, so they never count as own.
export function dateFields(b: EditBase): DateFields {
  const seriesTime = b.seriesStartsLocal?.slice(11);

  return {
    startsLocal: held(b.startsLocal, seriesTime !== undefined && b.startsLocal !== `${b.date}T${seriesTime}`),
    durationMinutes: held(b.durationMinutes, b.seriesDurationMinutes !== null && b.durationMinutes !== b.seriesDurationMinutes),
    title: held(b.title, b.seriesTitle !== null && b.title !== b.seriesTitle),
    // An event with no place of its own falls back to its group's, which the read does not show.
    location: held(b.location, !!b.seriesLocation && b.location !== b.seriesLocation),
    capacity: held(b.capacity, false),
    remoteCapacity: held(b.remoteCapacity, false),
    visibility: b.visibilityOverride ?? "series",
    tzid: b.tzid,
    seriesVisibility: b.seriesVisibility,
    remoteAvailable: b.remoteAvailable,
  };
}

// What dateFields reads once `changes` is stored over the date the editor opened with: each value the save writes is
// the date's own (the server trims text, as the form sends it); a cleared limit falls back to the event's, which no read
// shows. A value a read cannot prove own never reads back as this save stored, so a lost answer is never claimed
// saved by mistake: the refusal path sends again or closes instead.
export function intendedDateFields(opened: DateFields, changes: DateEditChanges): DateFields {
  const out = { ...opened };
  const limit = (value: number | null) => (value === null ? held<number | null | "event">("event", false) : held(value, true));

  if (changes.startsLocal !== undefined) out.startsLocal = held(changes.startsLocal, true);

  if (changes.durationMinutes !== undefined) out.durationMinutes = held(changes.durationMinutes, true);

  if (changes.title !== undefined) out.title = held(changes.title.trim(), true);

  if (changes.location !== undefined) out.location = held(changes.location.trim(), true);

  if (changes.capacityOverride !== undefined) out.capacity = limit(changes.capacityOverride);

  if (changes.remoteCapacityOverride !== undefined) out.remoteCapacity = limit(changes.remoteCapacityOverride);

  if (changes.visibility !== undefined) out.visibility = changes.visibility ?? "series";

  return out;
}

// The manager's read again after a refused save, as useRevisionSave compares it: null for a read that opens no editor
// (cancelled, moved to another row or group, malformed), which is a conflict.
export async function readDateFields(opener: DateOpener): Promise<RevisionRead<DateFields | null>> {
  const { occurrence } = await managedRead(opener);
  const base = editBaseOf(occurrence, opener);

  return { allowed: true, revision: occurrence?.revision, value: base && dateFields(base) };
}
