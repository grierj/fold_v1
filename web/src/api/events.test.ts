import { afterEach, expect, it, vi } from "vitest";
import { Dates, Events } from "./events";
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

it("sends each date save to the edits address with a fresh attempt id, never pinned", async () => {
  const fetcher = vi.fn<typeof fetch>(async (url) => String(url).endsWith("/auth/bootstrap") ? bootstrapAnswer({ token: "edit-csrf" }) : new Response(JSON.stringify({ occurrence: {} })));
  vi.stubGlobal("fetch", fetcher);
  await bootstrapCredential(true);
  const body = { revision: "r0", changes: { title: "Supper", capacityOverride: null }, reason: undefined };
  await Events.editDate("7/8", "2026-12-01/#", body);
  await Events.editDate(7, "2026-12-01", { ...body, consent: "0123456789abcdef0123456789abcdef" });
  const calls = fetcher.mock.calls.filter(([url]) => !String(url).endsWith("/auth/bootstrap"));
  expect(calls.map(([url]) => String(url))).toEqual(["/api/v1/events/7%2F8/occurrences/2026-12-01%2F%23/edits", "/api/v1/events/7/occurrences/2026-12-01/edits"]);
  const sent = calls.map(([, init]) => JSON.parse(String(init?.body)));
  expect(sent[0]).toEqual({ attempt: expect.stringMatching(/^[0-9a-f]{32}$/), revision: "r0", changes: { title: "Supper", capacityOverride: null } });
  expect(sent[1]).toMatchObject({ revision: "r0", consent: "0123456789abcdef0123456789abcdef" });
  expect(sent[1].attempt).not.toBe(sent[0].attempt);
  expect(calls.map(([, init]) => new Headers(init?.headers).get("Fold-Session-Check"))).toEqual([null, null]);
});

