import type { OccurrenceRoom, OwnRoom, RoomPickerItem, SeriesBooking, SeriesRoom, SeriesRoomsResponse } from "../types";
import { roomName } from "./place";
import { count } from "./plural";

// The words the room pickers use, in one place. `parseRoomLabel` reads a label
// back (e2e/helpers.ts `pickRoom` uses it), so the shape of a label and the
// reader of it change together.

export const NO_ROOM = "none";

// A long list of dates shows its first rows; the rest wait behind one button.
export const LIST_SHOWN = 10;

// An admin's reason for double-booking may be this long.
// Mirrors maxOverlapReason in api_room_bookings.go.
export const OVERLAP_REASON_MAX = 500;

// What separates the room from each note that follows it.
const NOTE_SEPARATOR = " — ";
const note = (text: string) => `${NOTE_SEPARATOR}${text}`;

// " (80 seats)", or nothing for a room with no size.
function seatsSuffix(room: { capacity: number | null }): string {
  return room.capacity === null ? "" : ` (${count(room.capacity, "seat")})`;
}

// A room as the single-date picker words it: "Fellowship Hall · Main (80 seats)",
// then what a leader should know before choosing it, then why it was suggested,
// if it was.
export function roomOptionLabel(room: RoomPickerItem, reason?: string): string {
  return `${roomName(room)}${seatsSuffix(room)}${room.busy ? note("in use at this time") : ""}${room.tooSmall ? note("may be too small") : ""}${reason ? note(reason) : ""}`;
}

// A picker label read back: the room as `roomName` words it, its seats (null for
// a room with no size) and the notes after it, in order. The notes depend on
// what else is booked, so a caller who wants "which room" compares `room` only.
/** @public e2e/helpers.ts pickRoom reads the pickers' labels back with it. */
export function parseRoomLabel(label: string): { room: string; seats: number | null; notes: string[] } {
  const [head, ...notes] = label.split(NOTE_SEPARATOR);
  const sized = /^(.*) \((\d+) seats?\)$/.exec(head);

  return sized ? { room: sized[1], seats: Number(sized[2]), notes } : { room: head, seats: null, notes };
}

// The same for the whole-series picker: how many of the dates something else
// holds the room on. `dates` is how many dates the booking would cover.
export function seriesRoomLabel(room: SeriesRoom, dates: number, reason?: string): string {
  let busy = "";

  if (room.busyDates > 0) busy = note(dates <= 1 ? "in use at this time" : `in use on ${room.busyDates} of ${dates} dates`);

  return `${roomName(room)}${seatsSuffix(room)}${busy}${room.tooSmall ? note("may be too small") : ""}${reason ? note(reason) : ""}`;
}

// What picking a room will do to the dates it is busy on, said before it is
// booked. `where` is the page that lists those dates afterwards. With dates
// that keep a room of their own (`kept`), the busy count may include them, so
// the note says only what was measured: the pick's answer says what it did.
export function busyDatesNote(room: SeriesRoom, dates: number, where: "edit" | "create", kept = false): string | null {
  if (room.busyDates <= 0) return null;
  const name = roomName(room);

  if (kept) return dates <= 1 ? `${name} is in use at this time.` : `${name} is in use on ${room.busyDates >= dates ? `all ${dates}` : `${room.busyDates} of ${dates}`} dates.`;
  const listed = where === "edit" ? "the busy dates will be listed under “Dates that need a room”." : "the event page will list the busy dates under “Dates that need a room”.";

  if (dates <= 1) {
    return where === "edit"
      ? `${name} is in use at this time, so the date will need a room; ${listed}`
      : `${name} is in use at this time, so the event will start without a room. The event page will ask you to pick one.`;
  }

  if (room.busyDates >= dates) {
    return where === "edit"
      ? `${name} is in use on all ${dates} dates, so none can be booked yet; ${listed}`
      : `${name} is in use on all ${dates} dates, so none can be booked yet. The event page will list them under “Dates that need a room”.`;
  }

  return `${name} is in use on ${room.busyDates} of ${dates} dates. It will be booked on the other ${dates - room.busyDates}, and ${listed}`;
}

const dates = (n: number) => count(n, "date");

// The dates of a series room that wait for an admin or still need a room (busy, or an admin didn't approve it), in the
// words a booking's answer and a read share.
function waitingAndNeeding(pending: number, needing: number): string[] {
  const parts: string[] = [];

  if (pending > 0) parts.push(`Sent to an admin for approval: ${dates(pending)}.`);

  if (needing > 0) {
    parts.push(`${dates(needing)} still ${needing === 1 ? "needs" : "need"} a room; pick one for ${needing === 1 ? "it" : "them"} under “Dates that need a room” on the event page.`);
  }

  return parts;
}

// The dates the series room control lists for a pick of `roomId` (null: no series room) as keeping their room unless
// the person includes them (DEC-168): the read's list less the dates that already hold the room picked, which a pick
// leaves as they are (policy.SeriesRoomChange on the server).
export function ownRoomsFor(list: OwnRoom[], roomId: number | null): OwnRoom[] {
  return list.filter((o) => o.waiting || o.room.id !== roomId);
}

// What the list says before a pick: how many dates have their own room and, unless they are included, that they keep it.
export function ownRoomsLead(n: number, including: boolean): string {
  const have = n === 1 ? "1 date has a room of its own" : `${n} dates have a room of their own`;

  return including ? `${have}:` : `${have} and will keep it:`;
}

// One listed date's room, as the date's own place names it, and what holds it back.
export function ownRoomNote(o: OwnRoom): string {
  const notes = [o.waiting ? "waiting for an admin's approval" : "", o.mayChange ? "" : "stays: only an admin can change it"].filter(Boolean);

  return `${roomName(o.room)}${notes.map(note).join("")}`;
}

// The box that includes the listed dates this person may change: into `room`, or, stopping the series room, released.
export function includeLabel(room: OccurrenceRoom | null, changeable: number, listed: number): string {
  const one = changeable === 1;
  const all = changeable === listed;

  if (room === null) {
    if (all) return one ? "Also release this date's room" : "Also release the rooms of these dates";

    return `Also release the ${one ? "room" : "rooms"} of ${changeable} of these dates`;
  }

  return `Also change ${all ? (one ? "this date" : "these dates") : `${changeable} of these dates`} to ${roomName(room)}`;
}

// The dates a series pick left with their own room (`kept`), `waiting` of them for an admin's approval.
function keptLine(kept: number, waiting: number): string {
  const one = kept === 1;

  if (waiting === 0) return `${dates(kept)} ${one ? "keeps its" : "keep their"} own room.`;

  if (waiting === kept) return `${dates(kept)} ${one ? "keeps its own room request" : "keep their own room requests"}, still waiting for an admin's approval.`;

  return `${dates(kept)} keep their own room; ${waiting} of them ${waiting === 1 ? "is" : "are"} still waiting for an admin's approval.`;
}

// What a series booking did, in words, from the counts its answer measured.
export function seriesBookingMessage(res: SeriesBooking): string {
  const parts: string[] = [];

  if (res.defaultRoom === null) parts.push("Series room released.");
  else if (res.booked > 0) {
    parts.push(res.doubleBooked > 0
      ? `Booked for ${dates(res.booked)}, ${res.doubleBooked} of them over another booking.`
      : `Booked for ${dates(res.booked)}.`);
  }

  if (res.defaultRoom !== null) parts.push(...waitingAndNeeding(res.pending, res.lost.length));

  if (res.changed > 0) {
    const their = res.changed === 1 ? "its" : "their";

    parts.push(res.defaultRoom === null
      ? `Also released the ${res.changed === 1 ? "room" : "rooms"} of ${dates(res.changed)} that had ${their} own.`
      : `Also changed ${dates(res.changed)} that had a room of ${their} own.`);
  }

  if (res.keptInUse > 0 && res.defaultRoom !== null) {
    // "Left as it is", not "keeps its room": the date may hold a room or still wait for one.
    parts.push(`${dates(res.keptInUse)} ${res.keptInUse === 1 ? "was left as it is" : "were left as they are"} because ${roomName(res.defaultRoom)} is in use then.`);
  }

  if (res.kept > 0) parts.push(keptLine(res.kept, res.keptWaiting));

  // A series room with nothing to book, send, lose, change or keep: every upcoming date already holds it or waits for it
  // on the request a re-pick keeps (which sends none of them again), or there is none.
  if (parts.length === 0) {
    parts.push(res.after.pending > 0 ? `Still waiting for an admin's approval: ${dates(res.after.pending)}.` : "The series room is saved.");
  }

  return parts.join(" ");
}

// What a series pick that was already stored (its answer was lost) left, as the read shows it: the read shows the
// series room and the dates that wait or need a room, not each date's booking.
export function seriesStoredMessage(read: SeriesRoomsResponse): string {
  if (read.defaultRoom === null) return "Series room released.";

  // "Still waiting", not "sent": the read shows every date that waits, and this pick may have sent only some of them.
  const parts = read.pending > 0 ? [`Still waiting for an admin's approval: ${dates(read.pending)}.`] : [];
  parts.push(...waitingAndNeeding(0, read.needsRoom.length));

  return parts.length > 0 ? parts.join(" ") : "The series room is saved.";
}
