import { describe, expect, it } from "vitest";
import { occurrence } from "../test/occurrence";
import type { EventOccurrence } from "../types";
import { dateFields, editBaseOf, intendedDateFields, type EditBase } from "./editBase";
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
      visibilityOverride: undefined, seriesVisibility: "public", churchWide: false, groupName: "Choir", groupVisibility: "public",
      seriesStartsLocal: "2026-12-01T19:00", seriesDurationMinutes: 60, seriesTitle: "Supper", seriesLocation: "Hall",
    });
    // A fallback the read does not carry well formed is unknown, never a refusal.
    expect(editBaseOf(wire({ seriesTitle: undefined, seriesStartsLocal: "soon", seriesDurationMinutes: 0 }), opener)).toMatchObject({ seriesTitle: null, seriesStartsLocal: null, seriesDurationMinutes: null });
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
    ["an unknown override", { visibilityOverride: "secret" }],
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

// DEC-164: the one comparison over what a single-date save can write. A read shows each field's value that applies;
// the field counts as the date's own only where the read proves it (the value differs from the fallback the read
// shows beside it). What a save would leave counts as own for each field it writes, so a read can match it only
// where the read proves the value own: anything else fails safe (never "already stored").
describe("dateFields and intendedDateFields", () => {
  const read = (over: Partial<EventOccurrence> = {}): EditBase => {
    const base = editBaseOf(good(over), opener);

    if (!base) throw new Error("not a manager read");

    return base;
  };

  const same = (a: unknown, b: unknown) => JSON.stringify(a) === JSON.stringify(b);
  // Opened inherited everywhere (the date shows the event's values), then saved with `changes`; `stored` is what a
  // later read shows: whether the read counts as "already stored", and whether it counts as "as it opened".
  const opened = read();

  it.each<[string, DateEditChanges, Partial<EventOccurrence>, boolean]>([
    ["an own title", { title: "Carols" }, { title: "Carols" }, true],
    ["an own title equal to the series title", { title: "Supper" }, { title: "Supper" }, false],
    ["an own title the server trimmed", { title: "  Carols " }, { title: "Carols" }, true],
    ["an own place", { location: "Annex" }, { location: "Annex" }, true],
    ["a place where the event has none (the group's may show)", { location: "Annex" }, { location: "Annex", seriesLocation: "" }, false],
    ["an own duration", { durationMinutes: 90 }, { durationMinutes: 90 }, true],
    ["a duration equal to the series duration", { durationMinutes: 60 }, { durationMinutes: 60 }, false],
    ["an own start", { startsLocal: "2026-12-02T18:00" }, { startsLocal: "2026-12-02T18:00" }, true],
    ["a start back on the series clock", { startsLocal: "2026-12-01T19:00" }, { startsLocal: "2026-12-01T19:00" }, false],
    ["a seat limit (the read shows no fallback)", { capacityOverride: 12 }, { capacity: 12 }, false],
    ["a cleared seat limit", { capacityOverride: null }, { capacity: 20 }, false],
    ["a cleared seat limit that reads as none", { capacityOverride: null }, { capacity: null }, false],
    ["a remote seat limit", { remoteCapacityOverride: 5 }, { remoteAvailable: true, remoteCapacity: 5 }, false],
    ["a visibility of its own", { visibility: "private" }, { visibilityOverride: "private", visibility: "private" }, true],
    ["same as the series", { visibility: "series" }, {}, true],
  ])("%s: a read showing it counts as stored only where it proves it", (_label, changes, stored, done) => {
    const goal = intendedDateFields(dateFields(opened), changes);
    expect(same(dateFields(read(stored)), goal)).toBe(done);
  });

  it("a visibility of its own is told apart from the same value inherited", () => {
    const own = intendedDateFields(dateFields(read({ visibility: "private", seriesVisibility: "private" })), { visibility: "private" });
    expect(same(dateFields(read({ visibility: "private", seriesVisibility: "private" })), own)).toBe(false);
    expect(same(dateFields(read({ visibility: "private", seriesVisibility: "private", visibilityOverride: "private" })), own)).toBe(true);
  });

  it.each<[string, Partial<EventOccurrence>, boolean]>([
    ["only the revision", { revision: "r9" }, true],
    ["another manager's own title equal to the shown one", { title: "Supper" }, true],
    ["a new title", { title: "Potluck" }, false],
    ["a new place", { location: "Annex" }, false],
    ["a new duration", { durationMinutes: 90 }, false],
    ["a new start", { startsLocal: "2026-12-01T18:00" }, false],
    ["a new seat limit", { capacity: 20 }, false],
    ["a new remote seat limit", { remoteCapacity: 3 }, false],
    ["an own visibility over the inherited one", { visibilityOverride: "public" }, false],
    ["what the series visibility is", { seriesVisibility: "private", visibility: "private" }, false],
    ["the time zone", { tzid: "America/Boise" }, false],
    ["remote seats offered", { remoteAvailable: true }, false],
    ["the room, the roster or the counts (not written by the save)", { seatsUsed: 9, room: { id: 1, name: "Hall", building: "Main" } }, true],
  ])("against the read the form opened with, %s reads as %s", (_label, over, asOpened) => {
    expect(same(dateFields(read(over)), dateFields(opened))).toBe(asOpened);
  });
});
