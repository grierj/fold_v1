import { describe, expect, it } from "vitest";
import { occurrence } from "../test/occurrence";
import type { EventOccurrence } from "../types";
import { editBaseOf } from "./editBase";
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
