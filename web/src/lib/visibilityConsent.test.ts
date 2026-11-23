import { describe, expect, it } from "vitest";
import { ApiError } from "../api/core";
import { dateDecision, decisionImpact, reviewOutdated } from "./visibilityConsent";
import type { JsonObject } from "./wireValues";

const token = "0123456789abcdef0123456789abcdef";
const impact = { visibility: "private", currentVisibility: "public", tightened: [], leaderOpened: [], consent: token };

describe("decisionImpact", () => {
  it("reads the impact of a decision only when it carries a token", () => {
    expect(decisionImpact(new ApiError(409, "x", { needsDecision: true, impact }))).toEqual(impact);
    // A date edit's question is dateDecision's alone.
    expect(decisionImpact(new ApiError(409, "x", { code: "date_needs_consent", impact }))).toBeNull();
    expect(decisionImpact(new ApiError(409, "x", { needsDecision: true, impact: { ...impact, consent: undefined } }))).toBeNull();
    expect(decisionImpact(new ApiError(409, "x", { needsDecision: true, impact: { ...impact, consent: "ABC" } }))).toBeNull();
    expect(decisionImpact(new ApiError(409, "x", { code: "visibility_review_outdated" }))).toBeNull();
    expect(decisionImpact(new ApiError(400, "x", { needsDecision: true, impact }))).toBeNull();
    expect(decisionImpact(new Error("x"))).toBeNull();
  });
});

describe("reviewOutdated", () => {
  it("is the outdated copy for an outdated answer or a decision without a token", () => {
    const copy = "Fold was updated while this page was open. Reload this page, then review the change again.";
    expect(reviewOutdated(new ApiError(409, "x", { code: "visibility_review_outdated" }))).toBe(copy);
    expect(reviewOutdated(new ApiError(409, "x", { needsDecision: true, impact: { ...impact, consent: undefined } }))).toBe(copy);
    expect(reviewOutdated(new ApiError(409, "x", { needsDecision: true, impact }))).toBeNull();
    expect(reviewOutdated(new ApiError(409, "x", { code: "conflict" }))).toBeNull();
    // A date question the editor may not ask, however well formed, is outdated too: it never has a Yes.
    expect(reviewOutdated(new ApiError(409, "x", { code: "date_needs_consent", impact }))).toBe(copy);
  });
});

describe("dateDecision", () => {
  const expected = { eventId: 7, occurrenceId: 41, current: "public", target: "private" } as const;
  const row = { id: 7, occurrenceId: 41, title: "Supper · 2026-12-08", visibility: "public", registrants: 3 };
  const asked = (extra: JsonObject = {}) => new ApiError(409, "x", { code: "date_needs_consent", impact: { ...impact, tightened: [row], ...extra } });
  const { consent: _token, ...tokenless } = impact;
  const { registrants: _count, ...uncounted } = row;

  it("reads exactly one tightened row of this date, its count and the change saved", () => {
    expect(dateDecision(asked(), expected)).toEqual({ token, count: 3, current: "public", target: "private" });
  });

  it.each([
    ["another code", new ApiError(409, "x", { code: "visibility_review_outdated", impact: { ...impact, tightened: [row] } })],
    ["another status", new ApiError(400, "x", { code: "date_needs_consent", impact: { ...impact, tightened: [row] } })],
    ["no impact", new ApiError(409, "x", { code: "date_needs_consent" })],
    ["a group decision", new ApiError(409, "x", { needsDecision: true, impact: { ...impact, tightened: [row] } })],
    ["no token", new ApiError(409, "x", { code: "date_needs_consent", impact: { ...tokenless, tightened: [row] } })],
    ["a bad token", asked({ consent: "z".repeat(32) })],
    ["no rows", asked({ tightened: [] })],
    ["two rows", asked({ tightened: [row, row] })],
    ["a row that is not an object", asked({ tightened: [7] })],
    ["another event", asked({ tightened: [{ ...row, id: 8 }] })],
    ["another date", asked({ tightened: [{ ...row, occurrenceId: 42 }] })],
    ["no count", asked({ tightened: [uncounted] })],
    ["a zero count", asked({ tightened: [{ ...row, registrants: 0 }] })],
    ["a row at another visibility", asked({ tightened: [{ ...row, visibility: "private" }] })],
    ["a decision key", asked({ tightened: [{ ...row, decisionKey: "x" }] })],
    ["a leader-opened row", asked({ leaderOpened: [row] })],
    ["reset dates", asked({ resetDates: [] })],
    ["future overrides", asked({ futureOverrides: 2 })],
    ["another current value", asked({ currentVisibility: "invite_only" })],
    ["another target", asked({ visibility: "invite_only" })],
    ["a plain error", new Error("x")],
  ])("is null for %s", (_, cause) => {
    expect(dateDecision(cause, expected)).toBeNull();
  });

  it("is null unless the saved change is more private", () => {
    const same = new ApiError(409, "x", { code: "date_needs_consent", impact: { ...impact, visibility: "public", tightened: [row] } });
    expect(dateDecision(same, { ...expected, target: "public" })).toBeNull();
    const looser = new ApiError(409, "x", { code: "date_needs_consent", impact: { ...impact, visibility: "public", currentVisibility: "private", tightened: [{ ...row, visibility: "private" }] } });
    expect(dateDecision(looser, { ...expected, current: "private", target: "public" })).toBeNull();
  });
});
