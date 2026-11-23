import type { EventOccurrence, RegistrationResponse, SeriesRegistrationResponse, SeriesRegistrationState } from "../types";
import { savedAnswerFromState } from "./registration";
import { formatCalendarDay } from "./when";

export type ActorRsvpResult = { date: string; after: SeriesRegistrationState };

export type SeriesFeedbackSignal = {
  eventId: number;
  date: string;
  revision: number;
  actorResults: ActorRsvpResult[];
};

export function actorResultFromRegistration(registration: RegistrationResponse): ActorRsvpResult {
  return {
    date: registration.date,
    after: {
      registrationId: registration.registrationId,
      status: registration.status,
      guestCount: registration.guestCount,
      attendanceMode: registration.attendanceMode,
      childcareCount: registration.childcareCount,
      childcareAgesNote: registration.childcareAgesNote,
      waitlistPosition: registration.waitlistPosition ?? undefined,
    },
  };
}

export function applyActorRsvpResults(instance: EventOccurrence, eventId: number, actorResults: ActorRsvpResult[]): EventOccurrence {
  if (instance.eventId !== eventId) return instance;
  const result = actorResults.find((row) => row.date === instance.date);

  if (!result) return instance;
  const { after } = result;
  const saved = savedAnswerFromState(after);
  const hasRegistration = saved.status !== null;

  return {
    ...instance,
    myRegistration: saved.status,
    myAttendanceMode: hasRegistration ? saved.mode : undefined,
    myGuestCount: hasRegistration ? saved.guests : undefined,
    myChildcareCount: hasRegistration ? saved.children : undefined,
    myChildcareAgesNote: hasRegistration ? saved.ages : undefined,
    myWaitlistPosition: saved.status === "waitlisted" ? after.waitlistPosition : undefined,
  };
}

const humanDays = (days: string[]) => days.map(formatCalendarDay).join(" and ");

// What a series answer or its Undo did, from the server's own results: never from the request.
export function seriesResultSummary(result: SeriesRegistrationResponse, prefix = "") {
  const going = result.results.filter((row) => row.after.status === "going").length;
  const maybe = result.results.filter((row) => row.after.status === "maybe").length;
  const declined = result.results.filter((row) => row.after.status === "declined").length;
  const waitlisted = result.results.filter((row) => row.after.status === "waitlisted").map((row) => row.date);
  const parts = [going ? `Going to ${going} ${going === 1 ? "date" : "dates"}` : "", maybe ? `Maybe for ${maybe} ${maybe === 1 ? "date" : "dates"}` : "", declined ? `Can't go to ${declined} ${declined === 1 ? "date" : "dates"}` : ""].filter(Boolean);

  if (waitlisted.length) parts.push(`waitlisted for ${humanDays(waitlisted)}`);

  if (result.refusals.length) parts.push(`${result.refusals.length} ${result.refusals.length === 1 ? "date was" : "dates were"} left unchanged`);

  if (result.capacityLoss?.length) parts.push(`a seat was no longer available for ${humanDays(result.capacityLoss)}`);
  const fallback = prefix && result.results.length ? `Restored previous answers for ${result.results.length} ${result.results.length === 1 ? "date" : "dates"}.` : "No dates changed.";

  return `${prefix}${parts.join(" · ") || fallback}`;
}

export const TERMINAL_UNDO_CODES = new Set([
  "undo_changed", "undo_authority_revoked", "undo_date_ended", "undo_not_found", "undo_expired", "undo_used",
]);

export const REFRESH_UNDO_CODES = new Set([
  "undo_changed", "undo_authority_revoked", "undo_date_ended", "undo_not_found", "undo_used",
]);

export type UndoReceipt = { token: string; expiresAt: number };

// A series answer's Undo exists only as the server issued it: a token and a finite deadline still ahead.
export function undoReceiptOf(result: SeriesRegistrationResponse, now = Date.now()): UndoReceipt | null {
  const expiresAt = result.undoExpiresAt ? Date.parse(result.undoExpiresAt) : NaN;

  return result.undoToken && Number.isFinite(expiresAt) && expiresAt > now ? { token: result.undoToken, expiresAt } : null;
}
