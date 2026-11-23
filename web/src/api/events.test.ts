import { afterEach, expect, it, vi } from "vitest";
import { DateEdits, Dates, Events } from "./events";
import { bootstrapCredential } from "./core";
import { bootstrapAnswer, participatingAnswers, sentChecks } from "../test/credential";

afterEach(() => vi.unstubAllGlobals());

it("independently encodes date path segments and uses only scoped mutations with omitted care fields", async () => {
  const fetcher = vi.fn<typeof fetch>(async (url) => String(url).endsWith("/auth/bootstrap") ? bootstrapAnswer({ token: "date-csrf" }) : new Response("{}"));
  vi.stubGlobal("fetch", fetcher);
  const id = "7/8?x", date = "2026-12-01/#?";
  await Dates.get(id, date);
  await Dates.register(id, date, { status: "going", attendanceMode: "remote", guestCount: 2 });
  await Dates.register(id, date, { status: "maybe" });
  await Dates.end(id, date, "decline"); await Dates.end(id, date, "block");
  const calls = fetcher.mock.calls;
  const path = "/api/v1/events/7%2F8%3Fx/dates/2026-12-01%2F%23%3F";
  expect(calls.map(([url]) => url)).toEqual([path, "/api/v1/auth/bootstrap", `${path}/registration`, `${path}/registration`, `${path}/decline`, `${path}/block`]);
  expect(JSON.parse(String(calls[2][1]?.body))).toEqual({ status: "going", attendanceMode: "remote", guestCount: 2 });
  expect(JSON.parse(String(calls[3][1]?.body))).toEqual({ status: "maybe" });

  for (const [, options] of calls.filter(([, options]) => options?.method === "PUT" || options?.method === "POST")) {
    expect(options?.body || "").not.toMatch(/childcare|personId|registeredBy|overCapacity/);
  }
});

it("encodes the date of every other per-date call the same way", async () => {
  const fetcher = vi.fn<typeof fetch>(async () => new Response("{}"));
  vi.stubGlobal("fetch", fetcher);
  await Events.dateVisibilityImpact(7, "2026-12-01/#", "series");
  await Events.dateVisibilityImpact(7, "2026-12-01", "series", { startsLocal: "2026-12-04T19:00", durationMinutes: 90 });
  await Events.managedDate(7, "2026-12-01/#");
  expect(fetcher.mock.calls.map(([url]) => url)).toContain("/api/v1/events/7/occurrences/2026-12-01%2F%23/visibility-impact?visibility=series");
  expect(fetcher.mock.calls.map(([url]) => url)).toContain("/api/v1/events/7/occurrences/2026-12-01/visibility-impact?visibility=series&startsLocal=2026-12-04T19%3A00&durationMinutes=90");
  expect(fetcher.mock.calls.map(([url]) => url)).toContain("/api/v1/events/7/occurrences/2026-12-01%2F%23");
});

it("sends an add's opaque selection and options, never an email, for a selected person", async () => {
  const fetcher = vi.fn<typeof fetch>(async () => new Response(JSON.stringify({ csrfToken: "csrf" })));
  vi.stubGlobal("fetch", fetcher);
  await Events.addRegistration(7, "2026-12-01", { selectionId: "opaque" }, true, { childcareCount: 2, childcareAgesNote: "3 and 5" });
  const post = fetcher.mock.calls.find(([, opts]) => opts?.method === "POST");
  expect(JSON.parse(String(post?.[1]?.body))).toEqual({ date: "2026-12-01", selectionId: "opaque", overCapacity: true, childcareCount: 2, childcareAgesNote: "3 and 5" });
});

it("pins the event page, the date page and the managed read to a credential only when one is passed", async () => {
  const fetcher = vi.fn<typeof fetch>(participatingAnswers("t-events"));
  vi.stubGlobal("fetch", fetcher);
  const { credential } = await bootstrapCredential(true);
  await Events.get(7, credential);
  await Dates.get(7, "2026-12-01", credential);
  await Events.managedDate(7, "2026-12-01", credential);
  await Events.get(7);
  await Dates.get(7, "2026-12-01");
  await Events.managedDate(7, "2026-12-01");
  const pinned = ["/api/v1/events/7", "/api/v1/events/7/dates/2026-12-01", "/api/v1/events/7/occurrences/2026-12-01"];
  expect(sentChecks(fetcher.mock.calls)).toEqual([...pinned.map((url) => [url, "v2", "t-events"]), ...pinned.map((url) => [url, null, null])]);
});

it("refuses a pinned date answer with no acknowledgment as the credential's loss", async () => {
  vi.stubGlobal("fetch", vi.fn<typeof fetch>(async (url) => String(url).endsWith("/auth/bootstrap") ? bootstrapAnswer({ token: "t-bare" }) : new Response("{}")));
  const { credential } = await bootstrapCredential(true);
  await expect(Dates.get(7, "2026-12-01", credential)).rejects.toMatchObject({ code: "session_expired", unreadable: true });
});

it("freezes a guarded date edit once: fixed key order, only submitted fields, a trimmed reason only with visibility", () => {
  expect(DateEdits.freeze(7, "2026-12-01", 41, "r0", {})).toBeNull();
  expect(DateEdits.freeze(7, "2026-12-01", 0, "r0", { title: "x" })).toBeNull();
  expect(DateEdits.freeze(7, "2026-12-01", 41, "", { title: "x" })).toBeNull();
  const sent = DateEdits.freeze("7/8", "2026-12-01/#", 41, "r0", { visibility: "series", title: "Supper", capacityOverride: null, location: undefined }, "  Too many  ");

  if (!sent) throw new Error("not frozen");
  expect(sent.url).toBe("/events/7%2F8/occurrences/2026-12-01%2F%23/edits");
  expect(sent.attempt).toMatch(/^[0-9a-f]{32}$/);
  expect(sent.body).toBe(`{"attempt":"${sent.attempt}","revision":"r0","changes":{"title":"Supper","capacityOverride":null,"visibility":"series"},"reason":"Too many"}`);
  expect(Object.isFrozen(sent) && Object.isFrozen(sent.changes)).toBe(true);
  expect(DateEdits.freeze(7, "2026-12-01", 41, "r0", { title: "x" }, "a reason")?.body).not.toContain("reason");
  expect(DateEdits.freeze(7, "2026-12-01", 41, "r0", { visibility: null }, "   ")?.body).not.toContain("reason");
  expect(DateEdits.freeze(7, "2026-12-01", 41, "r0", { title: "x" })?.attempt).not.toBe(sent.attempt);

  const consented = DateEdits.withConsent(sent, "0123456789abcdef0123456789abcdef");
  expect(consented.body).toBe(`${sent.body.slice(0, -1)},"consent":"0123456789abcdef0123456789abcdef"}`);
  expect(DateEdits.withConsent(consented, "fedcba9876543210fedcba9876543210").body).toBe(`${sent.body.slice(0, -1)},"consent":"fedcba9876543210fedcba9876543210"}`);
  expect({ ...consented, body: sent.body }).toEqual(sent);
});

it("sends the frozen bytes to the edits address, pinned, and nowhere else", async () => {
  const fetcher = vi.fn<typeof fetch>(participatingAnswers("edit-csrf", "v2 m-edit-csrf anon"));
  vi.stubGlobal("fetch", fetcher);
  const { credential } = await bootstrapCredential(true);
  const sent = DateEdits.freeze(7, "2026-12-01", 41, "r0", { title: "x" });

  if (!sent) throw new Error("not frozen");
  const { signal } = new AbortController();
  await DateEdits.send(sent, credential, { permit: () => true, onDispatch: () => {}, signal });
  const calls = fetcher.mock.calls.filter(([url]) => !String(url).endsWith("/auth/bootstrap"));
  expect(sentChecks(calls)).toEqual([["/api/v1/events/7/occurrences/2026-12-01/edits", "v2", "edit-csrf"]]);
  // The caller's signal reaches fetch, so ending the attempt's lifetime can cancel the request.
  expect(calls[0][1]).toMatchObject({ method: "POST", body: sent.body, signal });
});
