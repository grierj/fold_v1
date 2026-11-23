import { expect, it } from "vitest";
import { ApiError } from "../api/core";
import type { CancelledEventDate, EventDate } from "../types";
import { manageAuthority } from "./manageAuthority";

const base: EventDate = { id: 1, eventId: 7, date: "2026-12-01", dateUrl: "/events/7/dates/2026-12-01", title: "Date", location: "", startsAt: "2026-12-01T19:00:00Z", endsAt: "2026-12-01T20:00:00Z", startsLocal: "2026-12-01T19:00", endsLocal: "2026-12-01T20:00", tzid: "UTC", durationMinutes: 60, remoteAvailable: false, guestsAllowed: 0, myRegistration: null, videoProvider: "link", videoHidden: false, canSeeVideo: false, canSeeAttendees: false, canDecline: false, canBlock: false };
const date = (canManage?: boolean): EventDate => (canManage === undefined ? base : { ...base, canManage });
const tombstone: CancelledEventDate = { title: "Date", date: "2026-12-01", cancelled: true };

type Failure = ApiError | TypeError | null;

const settled = (data: EventDate | CancelledEventDate | undefined, error: Failure = null) => ({ loading: false, error, data });

it("is checking while a load is in flight, whatever it held before", () => {
  expect(manageAuthority({ loading: true, error: null, data: undefined })).toBe("checking");
  expect(manageAuthority({ loading: true, error: null, data: date(true) })).toBe("checking");
});

it("is granted only for a settled scheduled date with canManage", () => {
  expect(manageAuthority(settled(date(true)))).toBe("granted");
  expect(manageAuthority(settled(date(false)))).toBe("lost");
  expect(manageAuthority(settled(date()))).toBe("lost");
  expect(manageAuthority(settled(tombstone))).toBe("lost");
  // A closed load (the page's gone()) has no data, no error and is not loading.
  expect(manageAuthority(settled(undefined))).toBe("lost");
});

it("reads a readable refusal as lost and anything else as unknown", () => {
  for (const status of [401, 403, 404, 400]) expect(manageAuthority(settled(undefined, new ApiError(status, "no")))).toBe("lost");
  expect(manageAuthority(settled(undefined, new ApiError(403, "no", {}, true)))).toBe("unknown");
  expect(manageAuthority(settled(undefined, new ApiError(500, "down")))).toBe("unknown");
  expect(manageAuthority(settled(undefined, new TypeError("Failed to fetch")))).toBe("unknown");
});

it("reads the error before the data: a failed quiet reload holding an old canManage is unknown", () => {
  expect(manageAuthority(settled(date(true), new TypeError("Failed to fetch")))).toBe("unknown");
  expect(manageAuthority(settled(date(true), new ApiError(503, "down")))).toBe("unknown");
  expect(manageAuthority(settled(date(true), new ApiError(403, "no")))).toBe("lost");
});
