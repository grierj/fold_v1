import { isoTimestamp, literal, minLength, null_, object, optional, pipe, safeParse, string, undefined_, union, url } from "valibot";
import type {
  AdminRoomsResponse,
  DateRoomsResponse,
  MatchLocationResponse,
  RoomApprovalResponse,
  RoomBookingResponse,
  RoomFeedResponse,
  RoomLocationsResponse,
  RoomRequestConfirmation,
  RoomRequestsResponse,
  RoomResponse,
  RoomsCheckResponse,
  RoomsEnabledResponse,
  SeriesBooking,
  SeriesRoomsResponse,
} from "../types";
import { api, ApiError, credentialCurrent, currentCredential, path, recheckCredential, type Credential, type Id } from "./core";
import { Site } from "./site";

// Rooms (M2.1a). Everything answers 404 while rooms are off. On update, absent
// fields keep their value and a null capacity clears the limit.
export interface RoomInput {
  name?: string;
  building?: string;
  capacity?: number | null;
  features?: string;
  notes?: string;
  active?: boolean;
  restricted?: boolean;
  defaultSetupMinutes?: number;
  defaultTeardownMinutes?: number;
}

const revision = pipe(string(), minLength(1));
const liveFeed = { hasToken: literal(true), createdAt: pipe(string(), isoTimestamp()), revision };
const feedOff = object({ hasToken: literal(false), createdAt: null_(), revision, url: optional(undefined_()) });

const feedAnswers = {
  GET: object({ feed: union([feedOff, object({ ...liveFeed, url: optional(undefined_()) })]) }),
  POST: object({ feed: object({ ...liveFeed, url: pipe(string(), url()) }) }),
  DELETE: object({ feed: feedOff }),
};

async function roomFeed(id: Id, request: { method: "GET" } | { method: "POST" | "DELETE"; body: { revision: string } }, pinned: Credential | null): Promise<RoomFeedResponse> {
  const answer = await api<unknown>(path`/admin/rooms/${id}/feed`, { ...request, pinned }).catch((cause: unknown) => {
    // A pre-dispatch refusal can be the first sign that another tab changed the cookie.
    if (pinned && currentCredential() === pinned && !credentialCurrent(pinned)) recheckCredential();

    throw cause;
  });

  const parsed = safeParse(feedAnswers[request.method], answer);

  if (!parsed.success) throw new ApiError(502, "unreadable room calendar link", {}, true);

  return parsed.output;
}

async function getRoomFeed(id: Id, pinned: Credential | null): Promise<RoomFeedResponse> {
  try {
    return await roomFeed(id, { method: "GET" }, pinned);
  } catch (cause) {
    if (!(cause instanceof ApiError) || cause.code !== "session_expired" || !cause.unreadable || !credentialCurrent(pinned)) throw cause;

    // The pre-authentication switch returns an unacknowledged 404. Only a fresh public
    // settings read can confirm that the feed settings are unavailable on this credential.
    const site = await Site.get().catch(() => null);

    if (!credentialCurrent(pinned)) throw cause;

    if (site?.roomsEnabled === false) throw new ApiError(404, "room booking is turned off", { code: "rooms_off" });

    throw new ApiError(502, "unconfirmed room calendar link status", {}, true);
  }
}

export const AdminRooms = {
  list: () => api<AdminRoomsResponse>("/admin/rooms"),
  create: (body: RoomInput) => api<RoomResponse>("/admin/rooms", { method: "POST", body }),
  // An edit names the read it was made from (DEC-171): an older one is 409 room_changed.
  update: (id: Id, body: RoomInput, revision: string | undefined) =>
    api<RoomResponse>(path`/admin/rooms/${id}`, { method: "PUT", body: { ...body, revision: revision ?? "" } }),
  setEnabled: (enabled: boolean) => api<RoomsEnabledResponse>("/admin/settings/rooms", { method: "PUT", body: { enabled } }),
  // Whether leaders' and hosts' bookings wait for an admin (M2.1d, DEC-102).
  setApproval: (enabled: boolean) => api<RoomApprovalResponse>("/admin/settings/room-approval", { method: "PUT", body: { enabled } }),
  // Typed places on upcoming dates without a room, and booking one place's dates into a room (M2.1e).
  locations: () => api<RoomLocationsResponse>("/admin/rooms/locations"),
  matchLocation: (location: string, roomId: number) =>
    api<MatchLocationResponse>("/admin/rooms/locations/match", { method: "POST", body: { location, roomId } }),
  // A room's calendar-feed link (M2.2b): makeFeed returns the link once and
  // turns any old one off.
  getFeed: getRoomFeed,
  makeFeed: (id: Id, revision: string, pinned: Credential | null) =>
    roomFeed(id, { method: "POST", body: { revision } }, pinned),
  revokeFeed: (id: Id, revision: string, pinned: Credential | null) =>
    roomFeed(id, { method: "DELETE", body: { revision } }, pinned),
};

// Open room requests (M2.1d, DEC-106): one per booking action, a date or a whole
// series. Confirming makes the free dates confirmed; busy dates are
// double-booked with overlapReason or go back to the leader (`lost`).
export const RoomRequests = {
  list: () => api<RoomRequestsResponse>("/admin/room-requests"),
  confirm: (id: Id, overlapReason?: string) =>
    api<RoomRequestConfirmation>(path`/admin/room-requests/${id}/confirm`, { method: "POST", body: overlapReason ? { overlapReason } : {} }),
  reject: (id: Id, reason?: string) =>
    api<Record<string, never>>(path`/admin/room-requests/${id}/reject`, { method: "POST", body: reason ? { reason } : {} }),
};

// Booking a room for one date (M2.1b). `:date` is the occurrence's original
// YYYY-MM-DD. Every route answers 404 while rooms are off. Only an admin may
// send overlapReason (to book over a conflict) or the two padding overrides.
// A pick names the read it was made from (`revision`, the read's dateRoom
// revision); an older one is refused with date_room_changed (DEC-167).
export interface DateRoomInput {
  roomId: number | null;
  overlapReason?: string;
  setupMinutes?: number;
  teardownMinutes?: number;
}

export const DateRooms = {
  list: (eventId: Id, date: string, pinned?: Credential) => api<DateRoomsResponse>(path`/events/${eventId}/dates/${date}/rooms`, { pinned }),
  set: (eventId: Id, date: string, body: DateRoomInput, revision: string | undefined) =>
    api<RoomBookingResponse>(path`/events/${eventId}/dates/${date}/room`, { method: "PUT", body: { ...body, revision } }),
};

// A room for the whole series (M2.1c). Booking it books every scheduled
// future date; dates the room is busy on come back in `lost` and in the
// event's needsRoom list (DEC-103). roomId null stops the series room. Only
// an admin's overlapReason double-books busy dates instead. A pick names the
// event's read it was made from (`revision`); an older one is refused with
// event_changed (DEC-167).
interface RoomAvailabilityInput {
  startsLocal: string;
  durationMinutes: number;
  tzid: string;
  recurrenceRule: string;
  capacity: number | null;
  groupId?: number;
  visibility?: string;
}

export const SeriesRooms = {
  list: (eventId: Id, pinned?: Credential) => api<SeriesRoomsResponse>(path`/events/${eventId}/rooms`, { pinned }),
  // `includeDates`: the dates with a room of their own the control showed, to change too (DEC-168).
  set: (eventId: Id, body: { roomId: number | null; overlapReason?: string; includeDates?: string[]; reviewedBusyDates?: string[] }, revision: string | undefined) =>
    api<SeriesBooking>(path`/events/${eventId}/room`, { method: "PUT", body: { ...body, revision } }),
  // Rooms for an event not yet created: the create form sends its date
  // fields (and groupId for a group event, and the visibility chosen, which
  // room suggestions use) and books with roomId on create.
  check: (body: RoomAvailabilityInput) => api<RoomsCheckResponse>("/rooms/check", { method: "POST", body }),
};
