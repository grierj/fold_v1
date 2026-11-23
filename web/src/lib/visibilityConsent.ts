import { ApiError } from "../api/core";
import type { ImpactResetDate, Visibility, VisibilityImpact, VisibilityImpactEvent } from "../types";
import { ERROR_COPY } from "./errors";
import { visibilityRank } from "./visibilityChange";
import { isJsonObject, isNumber, isString, type JsonValue } from "./wireValues";

const TOKEN = /^[0-9a-f]{32}$/;

function visibility(value: JsonValue | undefined): Visibility | null {
  return value === "public" || value === "private" || value === "invite_only" ? value : null;
}

function list<T>(value: JsonValue | undefined, read: (item: JsonValue) => T | null): T[] | null {
  if (value === undefined) return [];

  if (!Array.isArray(value)) return null;
  const out: T[] = [];

  for (const item of value) {
    const read1 = read(item);

    if (!read1) return null;
    out.push(read1);
  }

  return out;
}

function row(item: JsonValue): VisibilityImpactEvent | null {
  if (!isJsonObject(item)) return null;
  const vis = visibility(item.visibility);

  if (!isNumber(item.id) || !isString(item.title) || !vis || !isNumber(item.registrants)) return null;

  return {
    id: item.id, title: item.title, visibility: vis, registrants: item.registrants,
    date: isString(item.date) ? item.date : undefined,
    occurrenceId: isNumber(item.occurrenceId) ? item.occurrenceId : undefined,
    decisionKey: isString(item.decisionKey) ? item.decisionKey : undefined,
  };
}

function resetDate(item: JsonValue): ImpactResetDate | null {
  if (!isJsonObject(item)) return null;
  const vis = visibility(item.visibility);

  if (!isNumber(item.occurrenceId) || !isString(item.date) || !isString(item.title) || !vis) return null;

  return { occurrenceId: item.occurrenceId, date: item.date, title: item.title, visibility: vis };
}

// decisionImpact is the impact of a 409 that asks the person to review a
// group or event visibility change again (needsDecision), only when it
// carries the token that confirms it (DEC-158). Anything else is null: an
// answer without a token comes from an older server, and the page then shows
// the outdated copy instead of asking again, so it never loops. A date edit's
// question is dateDecision's alone.
export function decisionImpact(cause: unknown): VisibilityImpact | null {
  if (!(cause instanceof ApiError) || cause.status !== 409 || cause.data.needsDecision !== true) return null;
  const impact = cause.data.impact;

  if (!isJsonObject(impact) || !isString(impact.consent) || !TOKEN.test(impact.consent)) return null;
  const target = visibility(impact.visibility);
  const current = visibility(impact.currentVisibility);
  const tightened = list(impact.tightened, row);
  const leaderOpened = list(impact.leaderOpened, row);
  const resetDates = list(impact.resetDates, resetDate);

  if (!target || !current || !tightened || !leaderOpened || !resetDates) return null;

  return {
    visibility: target, currentVisibility: current, tightened, leaderOpened, consent: impact.consent,
    futureOverrides: isNumber(impact.futureOverrides) ? impact.futureOverrides : undefined,
    resetDates: impact.resetDates === undefined ? undefined : resetDates,
  };
}

// The one question a guarded date edit may ask: how many people saving it cancels, under the token that
// confirms exactly that, for a change from `current` to `target`.
export interface DateDecision {
  token: string;
  count: number;
  current: Visibility;
  target: Visibility;
}

// What the editor saved, which the writer's question must be about: the date's row, the visibility it showed
// and the one the save asks for ("series" already resolved from the same read).
interface DateDecisionExpected {
  eventId: number;
  occurrenceId: number;
  current: Visibility;
  target: Visibility;
}

const isCount = (value: JsonValue | undefined): value is number => isNumber(value) && Number.isSafeInteger(value) && value > 0;

// dateDecision is a 409 date_needs_consent the editor may ask about, or null. The writer's answer for one date
// is one tightened row, for this event and this date's row, cancelling at least one person, at the visibility the
// date showed, for a change to a more private value that is exactly the one saved, with a well-formed token and
// nothing else (DEC-158). Any other shape is never asked: a Yes could only confirm something nobody was shown.
export function dateDecision(cause: unknown, expected: DateDecisionExpected): DateDecision | null {
  if (!(cause instanceof ApiError) || cause.status !== 409 || cause.code !== "date_needs_consent") return null;
  const impact = cause.data.impact;

  if (!isJsonObject(impact) || !isString(impact.consent) || !TOKEN.test(impact.consent)) return null;

  if (impact.futureOverrides !== undefined || impact.resetDates !== undefined) return null;
  const { tightened, leaderOpened } = impact;

  if (!Array.isArray(tightened) || tightened.length !== 1 || !Array.isArray(leaderOpened) || leaderOpened.length !== 0) return null;
  const only = tightened[0];

  if (!isJsonObject(only) || only.id !== expected.eventId || only.occurrenceId !== expected.occurrenceId || !isCount(only.registrants)) return null;

  if (only.decisionKey !== undefined || only.visibility !== expected.current) return null;

  if (impact.currentVisibility !== expected.current || impact.visibility !== expected.target) return null;

  if (visibilityRank(expected.target) >= visibilityRank(expected.current)) return null;

  return { token: impact.consent, count: only.registrants, current: expected.current, target: expected.target };
}

// reviewOutdated is the outdated copy for an answer that refuses a review the
// page cannot redo: visibility_review_outdated, a decision without a token
// (an older server), or a date question dateDecision refused. null for
// anything else.
export function reviewOutdated(cause: unknown): string | null {
  if (!(cause instanceof ApiError) || cause.status !== 409 || decisionImpact(cause)) return null;

  if (cause.code === "visibility_review_outdated" || cause.data.needsDecision === true || cause.code === "date_needs_consent") {
    return ERROR_COPY.visibility_review_outdated;
  }

  return null;
}
