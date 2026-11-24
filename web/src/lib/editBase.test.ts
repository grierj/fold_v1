import { describe, expect, it } from "vitest";
import { occurrence } from "../test/occurrence";
import type { EventOccurrence } from "../types";
import { dateFields, editBaseOf, intendedDateFields, type DateFields, type EditBase } from "./editBase";
import type { DateEditChanges } from "../api/events";
import type { JsonValue } from "./wireValues";

const opener = { eventId: 7, date: "2026-12-01", occurrenceId: 1 };
const good = (over: Partial<EventOccurrence> = {}) => occurrence({ revision: "r0", groupId: 4, groupName: "Choir", groupVisibility: "public", ...over });

// A wire value the static type would not allow, as a broken or older server could send it: an undefined field
// is left out, as JSON leaves it out.
function wire(over: Readonly<Record<string, JsonValue | undefined>>): EventOccurrence {
  return JSON.parse(JSON.stringify({ ...good(), ...over }));
}

describe("editBaseOf", () => {
  it("takes a well-formed manager read, and only its form fields", () => {
    const base = editBaseOf(good({ capacity: 1, remoteAvailable: true, remoteCapacity: 0 }), opener);
    expect(base).toEqual({
      id: 1, eventId: 7, date: "2026-12-01", revision: "r0", startsLocal: "2026-12-01T19:00", durationMinutes: 60, tzid: "UTC",
      title: "Supper", location: "Hall", capacity: 1, remoteCapacity: 0, remoteAvailable: true, visibility: "public",
      seriesVisibility: "public", churchWide: false, groupName: "Choir", groupVisibility: "public",
      own: { startsLocal: null, durationMinutes: null, title: null, location: null, capacity: null, remoteCapacity: null, visibility: null },
    });
    const own = { startsLocal: "2026-12-02T18:30", durationMinutes: 45, title: "Carols", location: "", capacityOverride: 12, remoteCapacityOverride: 0, visibility: "private" as const };
    expect(editBaseOf(good({ own }), opener)?.own).toEqual({
      startsLocal: "2026-12-02T18:30", durationMinutes: 45, title: "Carols", location: "", capacity: 12, remoteCapacity: 0, visibility: "private",
    });
    expect(editBaseOf(good({ capacity: 100000, durationMinutes: 10080, startsLocal: "2028-02-29T23:59" }), opener)).not.toBeNull();
    // Limits the writer enforces now, which a date stored earlier may exceed, still open it.
    expect(editBaseOf(good({ capacity: 250000, remoteCapacity: 250000, durationMinutes: 20160 }), opener)).not.toBeNull();
    expect(editBaseOf(occurrence({ revision: "r0", churchWide: true }), opener)?.churchWide).toBe(true);
    // A group the viewer may not see is left out whole.
    expect(editBaseOf(occurrence({ revision: "r0" }), opener)).not.toBeNull();
  });

  it.each([
    ["a start with seconds", { startsLocal: "2026-12-01T19:00:00" }],
    ["a start with trailing text", { startsLocal: "2026-12-01T19:00Z" }],
    ["a start with no time", { startsLocal: "2026-12-01" }],
    ["a start at hour 24", { startsLocal: "2026-12-01T24:00" }],
    ["a start at minute 60", { startsLocal: "2026-12-01T19:60" }],
    ["a start on a day that does not exist", { startsLocal: "2026-02-30T19:00" }],
    ["a start that is not text", { startsLocal: 1 }],
    ["a malformed original day", { date: "2026-12-1" }, { date: "2026-12-1" }],
    ["a zero duration", { durationMinutes: 0 }],
    ["a negative duration", { durationMinutes: -30 }],
    ["a fractional duration", { durationMinutes: 60.5 }],
    ["a negative capacity", { capacity: -1 }],
    ["a zero capacity", { capacity: 0 }],
    ["an unsafe capacity", { capacity: 2 ** 53 }],
    ["a fractional capacity", { capacity: 2.5 }],
    ["a capacity as text", { capacity: "5" }],
    ["a missing capacity", { capacity: undefined }],
    ["a negative remote seat limit", { remoteCapacity: -1 }],
    ["a fractional remote seat limit", { remoteCapacity: 0.5 }],
    ["remote seats that are not a yes or no", { remoteAvailable: "true" }],
    ["no church-wide flag", { churchWide: undefined }],
    ["a church-wide flag as text", { churchWide: "false" }],
    ["a church-wide event with a group", { churchWide: true }],
    ["a group id with no name", { groupName: undefined, groupVisibility: undefined }],
    ["a group name with no id", { groupId: undefined }],
    ["a group with no visibility", { groupVisibility: undefined }],
    ["a group id that is not a positive whole number", { groupId: 0 }],
    ["an unknown group visibility", { groupVisibility: "secret" }],
    ["an unknown visibility", { visibility: "secret" }],
    ["a missing visibility", { visibility: undefined }],
    ["a missing series visibility", { seriesVisibility: undefined }],
    ["an unknown series visibility", { seriesVisibility: "secret" }],
    ["no own values", { own: undefined }],
    ["own values that are not an object", { own: "none" }],
    ["own values as a list", { own: [] }],
    ["an own start with seconds", { own: { startsLocal: "2026-12-01T19:00:00" } }],
    ["an own duration of zero", { own: { durationMinutes: 0 } }],
    ["an own title that is not text", { own: { title: 5 } }],
    ["an own place given as null", { own: { location: null } }],
    ["an own capacity of zero", { own: { capacityOverride: 0 } }],
    ["a negative own remote seat limit", { own: { remoteCapacityOverride: -1 } }],
    ["an unknown own visibility", { own: { visibility: "secret" } }],
    ["a group name that is not text", { groupName: 7 }],
    ["no time zone", { tzid: "" }],
    ["an overlong time zone", { tzid: "x".repeat(65) }],
    ["a title that is not text", { title: null }],
    ["a location that is not text", { location: null }],
    ["an empty token", { revision: "" }],
    ["a token that is not text", { revision: 5 }],
    ["a row id that is not a positive whole number", { id: 0 }, { occurrenceId: 0 }],
  ] satisfies ReadonlyArray<[string, Readonly<Record<string, JsonValue | undefined>>, Partial<typeof opener>?]>)("refuses %s", (_label, over, at?: Partial<typeof opener>) => {
    // The opener names the same row and day, so only the malformed field decides.
    expect(editBaseOf(wire(over), { ...opener, ...at })).toBeNull();
  });
});

// DEC-164: the one comparison over what a single-date save can write: the date's own values, as the manager's read
// gives them (each null while the date falls back), and what gives them their meaning.
describe("dateFields and intendedDateFields", () => {
  const read = (over: Partial<EventOccurrence> = {}): EditBase => {
    const base = editBaseOf(good(over), opener);

    if (!base) throw new Error("not a manager read");

    return base;
  };

  const same = (a: DateFields, b: DateFields) => JSON.stringify(a) === JSON.stringify(b);

  // Opened with `opened` (its own values), saved with `changes`; `stored` is a later read: whether it counts as
  // "already stored", true only when the read's own values are what the save would leave.
  it.each<[string, Partial<EventOccurrence>, DateEditChanges, Partial<EventOccurrence>, boolean]>([
    ["an own start", {}, { startsLocal: "2026-12-02T18:00" }, { startsLocal: "2026-12-02T18:00", own: { startsLocal: "2026-12-02T18:00" } }, true],
    // A rule's own hour gives the date a clock that differs from the series start, with no start of its own.
    ["a start a rule gives the date, not its own", {}, { startsLocal: "2026-12-01T20:00" }, { startsLocal: "2026-12-01T20:00", own: {} }, false],
    ["an own duration", {}, { durationMinutes: 90 }, { durationMinutes: 90, own: { durationMinutes: 90 } }, true],
    ["an own duration equal to the event's", {}, { durationMinutes: 60 }, { own: { durationMinutes: 60 } }, true],
    ["the event's duration, not its own", { own: { durationMinutes: 90 } }, { durationMinutes: 60 }, { own: {} }, false],
    ["an own title", {}, { title: "Carols" }, { title: "Carols", own: { title: "Carols" } }, true],
    ["an own title the server trimmed", {}, { title: "  Carols " }, { title: "Carols", own: { title: "Carols" } }, true],
    ["an own title equal to the event's", {}, { title: "Supper" }, { own: { title: "Supper" } }, true],
    ["the event's title, not its own", { own: { title: "Carols" } }, { title: "Supper" }, { own: {} }, false],
    ["an own place", {}, { location: "Annex" }, { location: "Annex", own: { location: "Annex" } }, true],
    ["an own empty place", {}, { location: "" }, { location: "", own: { location: "" } }, true],
    ["no place shown, none of its own", {}, { location: "" }, { location: "", own: {} }, false],
    ["an own seat limit", {}, { capacityOverride: 12 }, { capacity: 12, own: { capacityOverride: 12 } }, true],
    ["the event's seat limit, not its own", {}, { capacityOverride: 12 }, { capacity: 12, own: {} }, false],
    ["a seat limit cleared back to the event's", { own: { capacityOverride: 12 } }, { capacityOverride: null }, { capacity: 20, own: {} }, true],
    ["a seat limit still its own after a clear", { own: { capacityOverride: 12 } }, { capacityOverride: null }, { capacity: 12, own: { capacityOverride: 12 } }, false],
    ["an own remote seat limit of zero", {}, { remoteCapacityOverride: 0 }, { remoteCapacity: 0, own: { remoteCapacityOverride: 0 } }, true],
    ["a remote seat limit cleared back to the event's", { own: { remoteCapacityOverride: 5 } }, { remoteCapacityOverride: null }, { own: {} }, true],
    ["an own visibility", {}, { visibility: "private" }, { visibility: "private", own: { visibility: "private" } }, true],
    ["the same visibility inherited", {}, { visibility: "private" }, { visibility: "private", seriesVisibility: "private", own: {} }, false],
    ["same as the series again", { own: { visibility: "private" } }, { visibility: "series" }, { own: {} }, true],
    ["an own value the save leaves alone, kept", { own: { title: "Carols" } }, { durationMinutes: 90 }, { own: { title: "Carols", durationMinutes: 90 } }, true],
    ["an own value the save leaves alone, cleared meanwhile", { own: { title: "Carols" } }, { durationMinutes: 90 }, { own: { durationMinutes: 90 } }, false],
  ])("%s: already stored is %s", (_label, opened, changes, stored, done) => {
    const goal = intendedDateFields(dateFields(read(opened)), changes);
    expect(same(dateFields(read(stored)), goal)).toBe(done);
  });

  // Against the read the form opened with (nothing of its own): which reads count as unchanged, and which as changed by
  // someone else.
  it.each<[string, Partial<EventOccurrence>, boolean]>([
    ["only the revision", { revision: "r9" }, true],
    ["the event's own title, which the date falls back to", { title: "Potluck", seriesTitle: "Potluck" }, true],
    ["a clock a rule gives the date", { startsLocal: "2026-12-01T20:00" }, true],
    ["the room, the roster or the counts (not written by the save)", { seatsUsed: 9, room: { id: 1, name: "Hall", building: "Main" } }, true],
    ["an own start", { own: { startsLocal: "2026-12-01T18:00" } }, false],
    ["an own duration", { own: { durationMinutes: 90 } }, false],
    ["an own title, even the shown one", { own: { title: "Supper" } }, false],
    ["an own place", { own: { location: "Annex" } }, false],
    ["an own seat limit", { own: { capacityOverride: 20 } }, false],
    ["an own remote seat limit", { own: { remoteCapacityOverride: 3 } }, false],
    ["an own visibility", { own: { visibility: "public" } }, false],
    ["what the series visibility is", { seriesVisibility: "private", visibility: "private" }, false],
    ["the time zone", { tzid: "America/Boise" }, false],
    ["remote seats offered", { remoteAvailable: true }, false],
  ])("against the read the form opened with, %s reads as unchanged: %s", (_label, over, asOpened) => {
    expect(same(dateFields(read(over)), dateFields(read()))).toBe(asOpened);
  });
});
