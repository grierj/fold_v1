import { useEffect, useLayoutEffect, useRef, useState } from "react";
import { ApiError, currentCredential, type Credential } from "../api/core";
import { Events, type DateEditChanges } from "../api/events";
import type { DateOwn, EventOccurrence, Visibility } from "../types";
import type { RevisionRead } from "./useRevisionSave";
import { isBoolean, isNumber, isObject, isString, type JsonValue } from "./wireValues";

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

// What a date holds of its own (the manager read's `own`, DEC-164), each null while the date falls back to the event's
// value (or, for the place, the group's). Every key is always present, in this order, so two of them compare as JSON.
interface OwnValues {
  startsLocal: string | null;
  durationMinutes: number | null;
  title: string | null;
  location: string | null;
  capacity: number | null;
  remoteCapacity: number | null;
  visibility: Visibility | null;
}

// The one date a manager edits, from a fresh manager read: its form inputs and token, what it holds of its own, and
// nothing else (no roster, counts or other people). Each host's editor is built from this alone.
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
  seriesVisibility: Visibility;
  churchWide: boolean;
  groupName?: string;
  groupVisibility?: Visibility;
  own: OwnValues;
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

// The date's own values, each absent or well formed; a read without them, or with one malformed, is null. Never read
// as "everything falls back": a save that clears a value would then look stored.
function ownOf(value: DateOwn | undefined): OwnValues | null {
  if (!isObject(value) || Array.isArray(value)) return null;
  const { startsLocal, durationMinutes, title, location, capacityOverride, remoteCapacityOverride, visibility } = value;
  const absentOr = (v: JsonValue | undefined, ok: (v: JsonValue) => boolean) => v === undefined || ok(v);

  if (!absentOr(startsLocal, isWallMinute) || !absentOr(durationMinutes, (v) => atLeast(v, 1)) || !absentOr(title, isString) ||
    !absentOr(location, isString) || !absentOr(capacityOverride, (v) => atLeast(v, 1)) || !absentOr(remoteCapacityOverride, (v) => atLeast(v, 0)) ||
    !absentOr(visibility, isVisibility)) return null;

  return {
    startsLocal: isString(startsLocal) ? startsLocal : null,
    durationMinutes: isNumber(durationMinutes) ? durationMinutes : null,
    title: isString(title) ? title : null,
    location: isString(location) ? location : null,
    capacity: isNumber(capacityOverride) ? capacityOverride : null,
    remoteCapacity: isNumber(remoteCapacityOverride) ? remoteCapacityOverride : null,
    visibility: isVisibility(visibility) ? visibility : null,
  };
}

// The read answers the date the opener names, scheduled, on the expected row and (on the group page) still in
// that group, with a token, its own values and every form field well formed; anything else is null and opens nothing.
export function editBaseOf(o: EventOccurrence | undefined, opener: DateOpener): EditBase | null {
  if (!o || o.cancelled === true) return null;

  if (o.id !== opener.occurrenceId || o.eventId !== opener.eventId || o.date !== opener.date || !isId(o.id) || !isDay(o.date)) return null;

  if (opener.groupId !== undefined && o.groupId !== opener.groupId) return null;

  if (!isString(o.revision) || !o.revision || !isWallMinute(o.startsLocal) || !atLeast(o.durationMinutes, 1)) return null;

  if (!isString(o.tzid) || !o.tzid || o.tzid.length > 64 || !isString(o.title) || !isString(o.location)) return null;

  if (!(o.capacity === null || atLeast(o.capacity, 1)) || !isSeats(o.remoteCapacity) || !isBoolean(o.remoteAvailable) || !isBoolean(o.churchWide)) return null;

  if (!isVisibility(o.visibility) || !isVisibility(o.seriesVisibility) || !optionalVisibility(o.groupVisibility)) return null;

  if (o.groupName !== undefined && !isString(o.groupName)) return null;

  if (o.groupId !== undefined && !isId(o.groupId)) return null;

  // A shown group's id, name and visibility come together (labelGroup), and a church-wide event has none.
  const group = [o.groupId, o.groupName, o.groupVisibility].filter((v) => v !== undefined).length;

  if (group !== 0 && (group !== 3 || o.churchWide)) return null;
  const own = ownOf(o.own);

  if (!own) return null;

  return {
    id: o.id, eventId: o.eventId, date: o.date, revision: o.revision,
    startsLocal: o.startsLocal, durationMinutes: o.durationMinutes, tzid: o.tzid, title: o.title, location: o.location,
    capacity: o.capacity, remoteCapacity: o.remoteCapacity, remoteAvailable: o.remoteAvailable,
    visibility: o.visibility, seriesVisibility: o.seriesVisibility,
    churchWide: o.churchWide, groupName: o.groupName, groupVisibility: o.groupVisibility,
    own,
  };
}

// The manager's read of one date: a participating read pinned to the credential this tab holds, when it holds one.
const managedRead = (opener: DateOpener, pinned?: Credential) => Events.managedDate(opener.eventId, opener.date, pinned ?? currentCredential() ?? undefined);

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
export function useEditRead(opener: DateOpener, owner: string, open: boolean, onRefused: () => void, pinned?: Credential): EditRead {
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
    managedRead(latest.current.opener, pinned).then(
      (read) => { if (current) setAnswer({ owner, asked, result: editBaseOf(read.occurrence, latest.current.opener) ?? { refused: true } }); },
      (cause: unknown) => { if (current) setAnswer({ owner, asked, result: { refused: editReadRefused(cause) } }); },
    );

    return () => { current = false; };
  }, [open, owner, asked, answered, pinned]);

  const refused = !!mine && "refused" in mine.result && mine.result.refused;

  useEffect(() => {
    if (refused) latest.current.onRefused();
  }, [refused]);

  if (!mine) return { kind: "loading" };

  if (!("refused" in mine.result)) return { kind: "ready", base: mine.result };

  return refused ? { kind: "denied" } : { kind: "failed", retry: () => setAsked((n) => n + 1) };
}

/** @public what a single-date save can write, as dateFields reads it; its test compares one field at a time. */
export interface DateFields {
  own: OwnValues;
  // What gives the values above their meaning: the clock they are on, what "same as the series" is, and whether the
  // date has remote seats at all.
  tzid: string;
  seriesVisibility: Visibility;
  remoteAvailable: boolean;
}

// The one normaliser over everything a single-date save can write (DEC-164), for the read the editor opened with and
// for the read after a refusal, so useRevisionSave can tell "already stored", "as it opened" and "changed" apart: the
// date's own values as the read gives them. A value the date falls back to is not the save's to keep: a save leaves it
// falling back, so a change to the event's own value is not this save's conflict.
export function dateFields(b: EditBase): DateFields {
  return { own: b.own, tzid: b.tzid, seriesVisibility: b.seriesVisibility, remoteAvailable: b.remoteAvailable };
}

// What dateFields reads once `changes` is stored over the date the editor opened with: each value the save writes
// becomes the date's own, as the server stores it (text trimmed); a cleared limit, and "same as the series", fall back.
export function intendedDateFields(opened: DateFields, changes: DateEditChanges): DateFields {
  const own = { ...opened.own };

  if (changes.startsLocal !== undefined) own.startsLocal = changes.startsLocal;

  if (changes.durationMinutes !== undefined) own.durationMinutes = changes.durationMinutes;

  if (changes.title !== undefined) own.title = changes.title.trim();

  if (changes.location !== undefined) own.location = changes.location.trim();

  if (changes.capacityOverride !== undefined) own.capacity = changes.capacityOverride;

  if (changes.remoteCapacityOverride !== undefined) own.remoteCapacity = changes.remoteCapacityOverride;

  if (changes.visibility !== undefined) own.visibility = changes.visibility === "series" ? null : changes.visibility;

  return { ...opened, own };
}

// The manager's read again after a refused save, as useRevisionSave compares it: null for a read that opens no editor
// (cancelled, moved to another row or group, malformed), which is a conflict.
export async function readDateFields(opener: DateOpener, pinned?: Credential): Promise<RevisionRead<DateFields | null>> {
  const { occurrence } = await managedRead(opener, pinned);
  const base = editBaseOf(occurrence, opener);

  return { allowed: true, revision: occurrence?.revision, value: base && dateFields(base) };
}
