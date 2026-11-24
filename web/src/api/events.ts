import { isString } from "../lib/wireValues";
import type {
  AttendanceMode,
  DateRegistrationResponse,
  EventDateRead,
  EventDetailResponse,
  EventHostsResponse,
  EventInput,
  EventSeriesResponse,
  GuestConfirmation,
  GuestRegistrationResponse,
  JitsiLinkResponse,
  NewEventInput,
  NoticeResponse,
  OccurrenceResponse,
  OpenEventsResponse,
  RegistrationBatch,
  RegistrationCommand,
  SeriesRegistrationResponse,
  Visibility,
  VisibilityImpact,
} from "../types";
import { api, path, query, type Credential, type Id } from "./core";

export const Events = {
  listOpen: () => api<OpenEventsResponse>("/events"),
  // pinned (M8.1): a participating read, answered only for this credential (api/core.ts).
  get: (id: Id, pinned?: Credential) => api<EventDetailResponse>(path`/events/${id}`, { pinned }),
  createChurch: (data: NewEventInput & { title: string }) => api<EventSeriesResponse>("/events", { method: "POST", body: data }),
  setVisibility: (
    id: Id,
    // consent: the token of the impact the leader reviewed ("" when none ran). revision: the event page's read the
    // control opened with (DEC-163).
    data: { visibility: Visibility; reason?: string; consent: string; resetOverrides?: boolean; revision?: string }
  ) => api<EventSeriesResponse>(path`/events/${id}/visibility`, { method: "PUT", body: data }),
  visibilityImpact: (id: Id, visibility: Visibility, resetOverrides = false) =>
    api<VisibilityImpact>(`${path`/events/${id}/visibility-impact`}${query({ visibility, resetOverrides: resetOverrides ? "1" : "0" })}`),
  addHost: (id: Id, email: string) => api<EventHostsResponse>(path`/events/${id}/hosts`, { method: "POST", body: { email } }),
  removeHost: (id: Id, userId: Id) => api<EventHostsResponse>(path`/events/${id}/hosts/${userId}`, { method: "DELETE" }),
  update: (id: Id, data: EventInput) => api<EventSeriesResponse>(path`/events/${id}`, { method: "PUT", body: data }),
  generateJitsi: () => api<JitsiLinkResponse>("/events/video/jitsi", { method: "POST", body: {} }),
  register: (id: Id, data: RegistrationCommand) => api<RegistrationBatch>(path`/events/${id}/registrations`, { method: "PUT", body: data }),
  registerSeries: (id: Id, data: {
    date?: string;
    scope: "all_future" | "this_and_following";
    status: "going" | "maybe" | "declined";
    guestCount?: number;
    attendanceMode?: AttendanceMode;
    childcareCount?: number;
    childcareAgesNote?: string;
  }) => api<SeriesRegistrationResponse>(path`/events/${id}/series-registrations`, { method: "POST", body: data }),
  undoSeriesRegistration: (id: Id, token: string) =>
    api<SeriesRegistrationResponse>(path`/events/${id}/series-registrations/undo`, { method: "POST", body: { token } }),
  removeRegistration: (id: Id, date: string, registrationId: number, note?: string) =>
    api<unknown>(path`/events/${id}/registrations/remove`, { method: "POST", body: { date, registrationId, note } }),
  promoteRegistration: (id: Id, date: string, registrationId: number) =>
    api<unknown>(path`/events/${id}/registrations/promote`, { method: "POST", body: { date, registrationId } }),
  addRegistration: (id: Id, date: string, target: string | { selectionId: string }, overCapacity?: boolean, childcare?: { childcareCount: number; childcareAgesNote: string }) =>
    api<unknown>(path`/events/${id}/registrations/add`, { method: "POST", body: { date, ...(isString(target) ? { email: target } : target), overCapacity, ...childcare } }),
  restoreRegistration: (id: Id, date: string, target: { userId: number } | { personId: number; registeredBy: number }, attendanceMode: AttendanceMode, overCapacity: boolean) =>
    api<unknown>(path`/events/${id}/registrations/add`, { method: "POST", body: { date, ...target, attendanceMode, overCapacity } }),
  cancelOccurrence: (id: Id, date: string) => api<unknown>(path`/events/${id}/cancel`, { method: "POST", body: { date } }),
  // `time` is the start and duration the same save sends, so the count is for the date as it would be saved.
  dateVisibilityImpact: (id: Id, date: string, visibility: Visibility | "series", time?: { startsLocal?: string; durationMinutes?: number }) =>
    api<VisibilityImpact>(`${path`/events/${id}/occurrences/${date}/visibility-impact`}${query({ visibility, startsLocal: time?.startsLocal, durationMinutes: time?.durationMinutes })}`),
  // The manager's edit base for one date. A scheduled date that has not ended carries its roster (attendeesVisible).
  managedDate: (id: Id, date: string, pinned?: Credential) => api<OccurrenceResponse>(path`/events/${id}/occurrences/${date}`, { pinned }),
  delete: (id: Id) => api<unknown>(path`/events/${id}`, { method: "DELETE" }),
};

/** @public what a guarded date edit may change (POST .../occurrences/:date/edits); the guarded editor sends it once a host adopts it. */
export interface DateEditChanges {
  startsLocal?: string;
  durationMinutes?: number;
  title?: string;
  location?: string;
  capacityOverride?: number | null;
  remoteCapacityOverride?: number | null;
  visibility?: Visibility | "series" | null;
}

const changeKeys = ["startsLocal", "durationMinutes", "title", "location", "capacityOverride", "remoteCapacityOverride", "visibility"] as const;

/** @public one guarded date edit attempt as it may have been sent: its exact address and body, and the fields the person submitted. */
export interface DateEditEnvelope {
  readonly url: string;
  readonly body: string;
  readonly attempt: string;
  readonly revision: string;
  readonly occurrenceId: number;
  readonly changes: Readonly<DateEditChanges>;
  readonly reason?: string;
}

function attemptId(): string {
  return Array.from(crypto.getRandomValues(new Uint8Array(16)), (b) => b.toString(16).padStart(2, "0")).join("");
}

// A guarded date edit's request, frozen once (DateWritesProvider keeps it in memory only): a replay sends these
// bytes again, and an answered consent question adds only its token.
export const DateEdits = {
  // null when nothing would change or the base is not a manager read's (no occurrence id or revision). The
  // reason is trimmed and sent only with a visibility change.
  freeze: (eventId: Id, date: string, occurrenceId: number, revision: string, changes: DateEditChanges, reason?: string): DateEditEnvelope | null => {
    const kept: DateEditChanges = {};

    for (const key of changeKeys) if (changes[key] !== undefined) Object.assign(kept, { [key]: changes[key] });
    const why = "visibility" in kept ? reason?.trim() || undefined : undefined;

    if (!Object.keys(kept).length || !revision || !Number.isSafeInteger(occurrenceId) || occurrenceId <= 0) return null;
    const attempt = attemptId();

    return Object.freeze({
      url: path`/events/${eventId}/occurrences/${date}/edits`,
      body: JSON.stringify({ attempt, revision, changes: kept, reason: why }),
      attempt, revision, occurrenceId, changes: Object.freeze(kept), reason: why,
    });
  },
  withConsent: (sent: DateEditEnvelope, consent: string): DateEditEnvelope =>
    Object.freeze({ ...sent, body: JSON.stringify({ attempt: sent.attempt, revision: sent.revision, changes: sent.changes, reason: sent.reason, consent }) }),
  // Pinned to the credential the attempt belongs to; permit, onDispatch and signal as in core's RequestOptions.
  send: (sent: DateEditEnvelope, pinned: Credential, gate: { permit: () => boolean; onDispatch: () => void; signal: AbortSignal }) =>
    api<OccurrenceResponse>(sent.url, { method: "POST", json: sent.body, pinned, ...gate }),
};

// One date of an event: /events/:id/dates/:date, where :date is the
// occurrence's original YYYY-MM-DD.
export const Dates = {
  get: (id: Id, date: string, pinned?: Credential) => api<EventDateRead>(path`/events/${id}/dates/${date}`, { pinned }),
  // Care options are deliberately absent: the date DTO cannot describe them.
  register: (id: Id, date: string, answer: { status: "going" | "maybe"; attendanceMode?: AttendanceMode; guestCount?: number }) =>
    api<DateRegistrationResponse>(path`/events/${id}/dates/${date}/registration`, { method: "PUT", body: answer }),
  end: (id: Id, date: string, action: "decline" | "block") =>
    api<Record<string, never>>(path`/events/${id}/dates/${date}/${action}`, { method: "POST" }),
};

// Guest registration (M1.4): a signed-out adult registers for a public
// event by name and email. The server answers the same way whether or
// not the address already has an account. Nothing is registered until
// the emailed link is confirmed on /guest/confirm.
export interface GuestRegistrationInput {
  name: string;
  email: string;
  date: string;
  over18: boolean;
  createAccount?: boolean;
  // The browser's zone, for the guest's emails (DEC-58).
  timezone?: string;
  // Honeypot. People never fill this in.
  website?: string;
}

export const Guests = {
  register: (eventId: Id, data: GuestRegistrationInput) =>
    api<NoticeResponse>(path`/events/${eventId}/guest-registrations`, { method: "POST", body: data }),
  confirm: (token: string) => api<GuestConfirmation>("/guest-registrations/confirm", { method: "POST", body: { token } }),
  getManage: (token: string) => api<GuestRegistrationResponse>(path`/guest-registrations/manage/${token}`),
  cancelManage: (token: string) =>
    api<GuestRegistrationResponse>(path`/guest-registrations/manage/${token}/cancel`, { method: "POST" }),
};
