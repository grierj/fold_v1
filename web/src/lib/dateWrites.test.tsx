import { act, render, screen } from "@testing-library/react";
import { createRef, StrictMode, Suspense, use, useEffect, useRef, useState, type ReactNode, type RefObject } from "react";
import { flushSync } from "react-dom";
import { afterEach, describe, expect, it, vi } from "vitest";
import { setFlagsFromString } from "node:v8";
import { runInNewContext } from "node:vm";
import { bootstrapCredential, credentialCurrent, currentCredential, type Credential } from "../api/core";
import { DateEdits, type DateEditChanges, type DateEditEnvelope } from "../api/events";
import { bootstrapAnswer, clearMarkerCookie, setMarkerCookie } from "../test/credential";
import type { JsonObject } from "./wireValues";
import type { Visibility } from "../types";
import { DateWritesProvider, useDateWrites } from "./dateWrites";
import { createDateAttempts, useDateAttempts, useEditBase, type DateAttempts, type EditVerdict, type ReadPermit, type ReadStamp } from "./dateWrites";
import { createRosterWrites, type RosterWriteEnd, type RosterWrites, type WriteOutcome } from "./rosterWrites";
import { useLoad } from "./useLoad";

// What a test page shows, as an event or group page's useLoad data would be.
interface Answer { n: number }

// One page using the store, as the test drives it: every read it started, in order, answered when the test says.
interface TestPage {
  writes: ReturnType<typeof useDateWrites>;
  reads: Array<(value: Answer) => void>;
  reload: () => void;
  setData: (value: Answer) => void;
}

interface WriteHold { release: ((outcome: WriteOutcome) => void) | null }

const DATE = "2026-12-01";

// A page listing event 7 of group 1, with its own useLoad, that shows the date's state.
function Page({ name, pages, onReload }: { name: string; pages: Map<string, TestPage>; onReload: () => void }) {
  const reads = useRef<Array<(value: Answer) => void>>([]);
  const load = useLoad(() => new Promise<Answer>((accept) => { reads.current.push(accept); }), []);

  const writes = useDateWrites({
    epoch: load.epoch,
    dataEpoch: load.dataEpoch,
    shows: (target) => target.eventId === 7,
    reload: () => {
      onReload();
      void load.reload();
    },
  });

  useEffect(() => {
    pages.set(name, { writes, reads: reads.current, reload: () => void load.reload(), setData: load.setData });
  });

  const state = writes.writing(7, DATE) ? "held" : writes.settling(7, DATE) ? "settling" : "free";

  return <p>{name}: {state} {load.data?.n ?? "-"}</p>;
}

function pageOf(pages: Map<string, TestPage>, name: string): TestPage {
  const page = pages.get(name);

  if (!page) throw new Error(`${name} is not mounted`);

  return page;
}

// Takes the date's one update from a page, as a save does just before it sends.
function acquireOn(pages: Map<string, TestPage>, name: string): (outcome: WriteOutcome) => void {
  const taken: WriteHold = { release: null };
  act(() => { taken.release = pageOf(pages, name).writes.acquire({ eventId: 7, groupId: 1 }, DATE); });

  if (!taken.release) throw new Error(`${name} could not take the date`);

  return taken.release;
}

// Answers the page's `index`th read (0 is its first load).
async function answer(pages: Map<string, TestPage>, name: string, index: number, n: number) {
  await act(async () => pageOf(pages, name).reads[index]({ n }));
}

async function renderPages(names: string[], reloads: Record<string, () => void>) {
  const pages = new Map<string, TestPage>();
  render(<DateWritesProvider>
    {names.map((name) => <Page key={name} name={name} pages={pages} onReload={reloads[name] ?? (() => {})} />)}
  </DateWritesProvider>);

  for (const name of names) await answer(pages, name, 0, 0);

  return pages;
}

it("keeps a date closed on every page until the release that took it, then until each page shows a read asked for after it", async () => {
  const event = vi.fn();
  const group = vi.fn();
  const pages = await renderPages(["Event", "Group"], { Event: event, Group: group });

  const release = acquireOn(pages, "Event");
  expect(screen.getByText("Group: held 0")).toBeInTheDocument();
  expect(pageOf(pages, "Group").writes.acquire({ eventId: 7 }, DATE)).toBeNull();
  expect(pageOf(pages, "Group").writes.writing(7, "2026-12-08")).toBe(false);
  // A refresh the group asked for before the update answered may hold the old date.
  act(() => pageOf(pages, "Group").reload());
  expect(pageOf(pages, "Group").reads).toHaveLength(2);

  act(() => release("stored"));
  expect(screen.getByText("Group: settling 0")).toBeInTheDocument();
  expect(screen.getByText("Event: settling 0")).toBeInTheDocument();
  // Only the other page is asked to read again; the saving page's editor asks for its own (onSaved).
  expect(group).toHaveBeenCalledOnce();
  expect(event).not.toHaveBeenCalled();
  expect(pageOf(pages, "Group").reads).toHaveLength(3);

  // The earlier refresh answers last of all, and is never shown.
  await answer(pages, "Group", 2, 2);
  expect(screen.getByText("Group: free 2")).toBeInTheDocument();
  await answer(pages, "Group", 1, 1);
  expect(screen.getByText("Group: free 2")).toBeInTheDocument();
  expect(screen.getByText("Event: settling 0")).toBeInTheDocument();
  act(() => pageOf(pages, "Event").reload());
  await answer(pages, "Event", 1, 3);
  expect(screen.getByText("Event: free 3")).toBeInTheDocument();

  // A second call of a used release ends nothing, even a newer hold on the same date.
  const newer = acquireOn(pages, "Group");
  act(() => release("stored"));
  expect(screen.getByText("Event: held 3")).toBeInTheDocument();
  expect(group).toHaveBeenCalledOnce();

  act(() => newer("stored"));
  expect(event).toHaveBeenCalledOnce();
  expect(group).toHaveBeenCalledOnce();
});

it("keeps the date closed when a page replaces an older answer with a local change of it, until a read asked for after the update", async () => {
  const pages = await renderPages(["Event", "Group"], {});
  const release = acquireOn(pages, "Event");
  act(() => release("stored"));

  // An answer's reply changed on the page (an RSVP), made from what was read before the update.
  act(() => pageOf(pages, "Group").setData({ n: 9 }));
  expect(screen.getByText("Group: settling 9")).toBeInTheDocument();
  await answer(pages, "Group", 1, 4);
  expect(screen.getByText("Group: free 4")).toBeInTheDocument();
});

it("frees the date on every page after a refusal, and asks no page to read again", async () => {
  const event = vi.fn();
  const group = vi.fn();
  const pages = await renderPages(["Event", "Group"], { Event: event, Group: group });

  const release = acquireOn(pages, "Event");
  act(() => release("refused"));
  expect(screen.getByText("Event: free 0")).toBeInTheDocument();
  expect(screen.getByText("Group: free 0")).toBeInTheDocument();
  expect(event).not.toHaveBeenCalled();
  expect(group).not.toHaveBeenCalled();
});

it("keeps the date closed on every page after an update whose answer was lost, and asks the saving page to read again too", async () => {
  const event = vi.fn();
  const group = vi.fn();
  const pages = await renderPages(["Event", "Group"], { Event: event, Group: group });

  const release = acquireOn(pages, "Event");
  act(() => release("unsure"));
  expect(screen.getByText("Event: settling 0")).toBeInTheDocument();
  expect(screen.getByText("Group: settling 0")).toBeInTheDocument();
  expect(event).toHaveBeenCalledOnce();
  expect(group).toHaveBeenCalledOnce();

  await answer(pages, "Event", 1, 5);
  expect(screen.getByText("Event: free 5")).toBeInTheDocument();
  expect(screen.getByText("Group: settling 0")).toBeInTheDocument();
});

it("tells only the pages still open: one that a refresh takes away while the store is telling pages hears nothing", () => {
  const pages = new Map<string, TestPage>();
  const group = vi.fn();

  function Host() {
    const [groupShown, setGroupShown] = useState(true);

    return <DateWritesProvider>
      <Page name="Event" pages={pages} onReload={() => flushSync(() => setGroupShown(false))} />
      {groupShown && <Page name="Group" pages={pages} onReload={group} />}
      <Page name="Calendar" pages={pages} onReload={() => {}} />
    </DateWritesProvider>;
  }

  render(<Host />);
  const release = acquireOn(pages, "Calendar");
  act(() => release("stored"));
  expect(screen.queryByText(/^Group:/)).not.toBeInTheDocument();
  expect(group).not.toHaveBeenCalled();
});

// A page that counts roster writes (EventDetail, GroupDetail) or not (ManageDate), over a store the test ends
// writes through.
function RosterPage({ rosters, onReload, probe }: { rosters?: boolean; onReload: () => void; probe: RefObject<ReturnType<typeof useDateWrites> | null> }) {
  const load = useLoad(() => Promise.resolve({ n: 0 }), []);
  const writes = useDateWrites({ epoch: load.epoch, dataEpoch: load.dataEpoch, shows: (target) => target.eventId === 7, reload: onReload, rosters });
  probe.current = writes;

  return <p>{writes.settling(7, DATE) ? "settling" : "free"} {writes.settling(7, "2026-12-08") ? "other settling" : "other free"}</p>;
}

async function renderRosterPage(rosters?: boolean) {
  const store = createRosterWrites();
  const onReload = vi.fn();
  const probe = createRef<ReturnType<typeof useDateWrites>>();
  render(<DateWritesProvider rosterWrites={store}><RosterPage rosters={rosters} onReload={onReload} probe={probe} /></DateWritesProvider>);
  await act(async () => {});

  return { store, onReload };
}

it("without rosters a roster write's end holds nothing and asks for nothing: a gate starts that read (ManageDate)", async () => {
  const { store, onReload } = await renderRosterPage();
  const end = store.begin(`7:${DATE}`);
  act(() => end("stored"));
  expect(onReload).not.toHaveBeenCalled();
  expect(screen.getByText("free other free")).toBeInTheDocument();
});

it("with rosters an answered roster write and another event's write change nothing", async () => {
  const { store, onReload } = await renderRosterPage(true);
  const answered = store.begin(`7:${DATE}`);
  act(() => answered("answered"));
  const elsewhere = store.begin(`8:${DATE}`);
  act(() => elsewhere("stored"));
  expect(onReload).not.toHaveBeenCalled();
  expect(screen.getByText("free other free")).toBeInTheDocument();
});

for (const outcome of ["stored", "lost", "unsure"] as const) {
  it(`with rosters a roster write that ends ${outcome} holds that date and asks for the page once`, async () => {
    const { store, onReload } = await renderRosterPage(true);
    const end = store.begin(`7:${DATE}`);
    end(outcome);
    expect(onReload).toHaveBeenCalledTimes(1);
    await act(async () => {});
    expect(screen.getByText("settling other free")).toBeInTheDocument();
    const again = store.begin(`7:${DATE}`);
    act(() => again(outcome));
    expect(onReload).toHaveBeenCalledTimes(2);
  });
}

// ---- Guarded date edits (M8.1) ----

const EDIT_DATE = "2026-12-08";
const OCC = 41;

interface EditWire {
  // Every POST to an edits address: its URL, exact body bytes and session check headers.
  sent: Array<{ url: string; body: string; check: string | null; token: string | null; signal: AbortSignal | null }>;
  answer: (url: string, body: string, signal: AbortSignal | null) => Promise<Response> | Response;
  // What the next bootstrap adopts.
  boot: string;
}

type EditServer = EditWire & { credential: Credential };

let tokens = 0;
let ackHeader = "";

// A signed-in tab with a verified credential whose server answers each edit as the test says.
async function editServer(): Promise<EditServer> {
  const token = `edit-t${++tokens}`;
  const server: EditWire = { sent: [], answer: () => stored("r1"), boot: token };

  vi.stubGlobal("fetch", vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
    if (String(url).endsWith("/auth/bootstrap")) return bootstrapAnswer({ token: server.boot, user: { id: 5 } });
    const headers = new Headers(init?.headers);
    const body = String(init?.body);
    const signal = init?.signal ?? null;
    server.sent.push({ url: String(url), body, check: headers.get("Fold-Session-Check"), token: headers.get("X-CSRF-Token"), signal });

    return server.answer(String(url), body, signal);
  }));
  ackHeader = `v2 m-${token} 5`;
  const { credential } = await bootstrapCredential(true);

  return Object.assign(server, { credential });
}

// Answers carrying the latest credential's acknowledgment, as a participating server sends them.
const acked = (status: number, body: JsonObject) => Response.json(body, { status, headers: { "Fold-Session-Checked": ackHeader } });
const stored = (revision: string, id = OCC) => acked(200, { occurrence: { id, revision } });

function changed(superseded?: boolean): Response {
  const body: JsonObject = { error: "x", code: "date_changed" };

  if (superseded) body.baseSuperseded = true;

  return acked(409, body);
}

// The writer's question for event 7's date OCC, public to invite-only, cancelling `registrants` (DEC-158).
const challengeImpact = (token: string, registrants = 2): JsonObject => ({
  visibility: "invite_only", currentVisibility: "public", consent: token,
  tightened: [{ id: 7, occurrenceId: OCC, title: "Supper · 2026-12-08", visibility: "public", registrants }], leaderOpened: [],
});

const needsConsent = (token: string, impact: JsonObject = challengeImpact(token)) => acked(409, { error: "x", code: "date_needs_consent", impact });

// What the read the save was made from showed of the date's visibility.
interface Shown { visibility: Visibility; seriesVisibility: Visibility }

const SHOWN: Shown = { visibility: "public", seriesVisibility: "invite_only" };

// The object without one key, as a server that leaves a field out sends it.
function without(object: JsonObject, key: string): JsonObject {
  const { [key]: _gone, ...rest } = object;

  return rest;
}

afterEach(() => {
  vi.unstubAllGlobals();
  clearMarkerCookie();
});

// A read of the date as a host makes it: stamped before it is sent, registered once it answered.
function read(a: DateAttempts, revision: string, occurrenceId = OCC, date = EDIT_DATE): ReadPermit {
  const permit = a.register(a.stamp(7, date), { occurrenceId, revision });

  if (!permit) throw new Error("the read was not registered");

  return permit;
}

function setup(server: EditServer, rosters = createRosterWrites()) {
  return { a: createDateAttempts(server.credential, rosters), rosters };
}

const title = { title: "Harvest supper" };

// A roster write a listener starts once.
interface RosterHold {
  begun: boolean;
  end?: (e: RosterWriteEnd) => void;
}

const tick = () => new Promise<void>((resolve) => { setTimeout(resolve, 0); });

it("claims a date once: a second click, a sibling host and a reentrant listener get nothing; another date stays usable", async () => {
  const server = await editServer();
  const { a } = setup(server);
  const mine = read(a, "r0");
  const sibling = read(a, "r0");
  const other = read(a, "s0", OCC + 1, "2026-12-15");
  let reentrant: Promise<EditVerdict> | null | undefined;
  const stop = a.subscribe(() => { if (reentrant === undefined) reentrant = a.save(sibling, 7, EDIT_DATE, { title: "Theirs" }); });
  let release: (r: Response) => void = () => {};

  server.answer = () => new Promise((resolve) => { release = resolve; });

  const first = a.save(mine, 7, EDIT_DATE, title);
  stop();
  expect(first).not.toBeNull();
  expect(reentrant).toBeNull();
  expect(a.save(mine, 7, EDIT_DATE, title)).toBeNull();
  expect(a.save(sibling, 7, EDIT_DATE, title)).toBeNull();
  expect(a.canSave(sibling)).toBe(false);
  expect(server.sent).toHaveLength(1);
  expect(server.sent[0]).toMatchObject({ url: `/api/v1/events/7/occurrences/${EDIT_DATE}/edits`, check: "v2" });

  server.answer = () => stored("s1", OCC + 1);
  await expect(a.save(other, 7, "2026-12-15", { location: "Hall" })).resolves.toEqual({ kind: "stored", revision: "s1" });

  release(stored("r1"));
  await expect(first).resolves.toEqual({ kind: "stored", revision: "r1" });
  expect(server.sent).toHaveLength(2);
});

it("sends nothing and releases the claim when the permit fails just before fetch, and an empty change never claims", async () => {
  const server = await editServer();
  const { a, rosters } = setup(server);
  const p = read(a, "r0");
  expect(a.save(p, 7, EDIT_DATE, {})).toBeNull();

  // A roster write begins during the claim's own notification, before the request is handed to fetch.
  const roster: RosterHold = { begun: false };

  const stop = a.subscribe(() => {
    if (roster.begun) return;
    roster.begun = true;
    roster.end = rosters.begin(`7:${EDIT_DATE}`);
  });

  await expect(a.save(p, 7, EDIT_DATE, title)).resolves.toEqual({ kind: "notSent" });
  stop();
  expect(server.sent).toHaveLength(0);
  expect(a.status(`7:${EDIT_DATE}`)).toBe("idle");
  expect(a.canSave(p)).toBe(false);
  roster.end?.("answered");
  // An answered roster refusal ends the hold without a new read.
  expect(a.canSave(p)).toBe(true);
  const stale = rosters.begin(`7:${EDIT_DATE}`);
  stale("stored");
  expect(a.canSave(p)).toBe(false);
  expect(a.canSave(read(a, "r0"))).toBe(true);
});

it("a stored answer settles once; only a read stamped after it arms Save again, whatever revision it shows", async () => {
  const server = await editServer();
  const { a } = setup(server);
  const p = read(a, "r0");
  const before = a.stamp(7, EDIT_DATE);
  const failedReload = a.stamp(7, EDIT_DATE);
  void failedReload;
  await expect(a.save(p, 7, EDIT_DATE, title, { draft: { id: "d1", versions: { title: 3 } } })).resolves.toEqual({ kind: "stored", revision: "r1" });
  expect(a.canSave(p)).toBe(false);
  expect(a.takeSettledDraft(p, "d1")).toBeNull();
  // A read sent before the answer and committed after it, the old base, and the same token again are not enough.
  const late = a.register(before, { occurrenceId: OCC, revision: "r1" });
  expect(late && a.canSave(late)).toBe(false);
  const fresh = read(a, "r1");
  expect(a.canSave(fresh)).toBe(true);
  expect(a.takeSettledDraft(fresh, "other")).toBeNull();
  expect(a.takeSettledDraft(fresh, "d1")).toEqual({ title: 3 });
  expect(a.takeSettledDraft(fresh, "d1")).toBeNull();
  a.retire(fresh);
  expect(a.canSave(fresh)).toBe(false);
});

it("an answer it cannot trust leaves the date unsure on every host; a fresh read only enables Try again, which sends the same bytes", async () => {
  const lostAnswers: Array<[string, () => Response | Promise<Response>]> = [
    ["the network", () => Promise.reject(new TypeError("network"))],
    ["a server error", () => acked(500, { error: "x" })],
    ["a gateway page", () => new Response("<html>bad gateway</html>", { status: 502 })],
    ["a missing acknowledgment", () => Response.json({ occurrence: { id: OCC, revision: "r1" } })],
    ["an unreadable success", () => new Response("<html>", { headers: { "Fold-Session-Checked": ackHeader } })],
    ["another occurrence", () => stored("r1", OCC + 9)],
    ["an empty revision", () => stored("")],
  ];

  for (const [what, lost] of lostAnswers) {
    const server = await editServer();
    const { a } = setup(server);
    const host = read(a, "r0");
    const sibling = read(a, "r0");
    server.answer = lost;
    await expect(a.save(host, 7, EDIT_DATE, title, { reason: "ignored" }), what).resolves.toEqual({ kind: "unsure" });
    expect(a.status(`7:${EDIT_DATE}`), what).toBe("unsure");
    const after = read(a, "r0");

    for (const p of [host, sibling, after]) expect(a.canSave(p), what).toBe(false);
    expect(a.canReplay(host), what).toBe(false);
    expect(a.canReplay(after), what).toBe(true);
    expect(a.recovery(host), what).toBeNull();
    expect(a.recovery(after), what).toEqual({ changes: title, reason: undefined });
    server.answer = () => stored("r1");
    await expect(a.replay(after), what).resolves.toEqual({ kind: "stored", revision: "r1" });
    expect(server.sent.map((s) => s.body), what).toEqual([server.sent[0].body, server.sent[0].body]);
    expect(JSON.parse(server.sent[0].body), what).toEqual({ attempt: expect.stringMatching(/^[0-9a-f]{32}$/), revision: "r0", changes: title });
    vi.unstubAllGlobals();
  }
});

it("a first date_changed or lost answer needs a new read; a field refusal keeps the base", async () => {
  const server = await editServer();
  const { a } = setup(server);
  let p = read(a, "r0");
  server.answer = () => acked(400, { error: "x", code: "invalid_duration" });
  await expect(a.save(p, 7, EDIT_DATE, { durationMinutes: 0 })).resolves.toMatchObject({ kind: "refused", code: "invalid_duration", lost: false });
  expect(a.canSave(p)).toBe(true);
  server.answer = () => changed(true);
  await expect(a.save(p, 7, EDIT_DATE, title)).resolves.toEqual({ kind: "changed", superseded: true });
  expect(a.status(`7:${EDIT_DATE}`)).toBe("idle");
  expect(a.canSave(p)).toBe(false);
  p = read(a, "r5");
  server.answer = () => acked(403, { error: "x", code: "not_leader" });
  await expect(a.save(p, 7, EDIT_DATE, title)).resolves.toMatchObject({ kind: "refused", code: "not_leader", lost: true });
  expect(a.canSave(p)).toBe(false);
  const attempts = server.sent.map((s) => JSON.parse(s.body).attempt);
  expect(new Set(attempts).size).toBe(3);
});

it("an inherited place moved A to B to A: a conflict and a different read never end the doubt; only the original's answer does", async () => {
  // The server's answers in TestDateEditHeldOriginalAcrossAnInheritedPlaceABA, through the real classifier.
  const server = await editServer();
  const { a } = setup(server);
  const key = `7:${EDIT_DATE}`;
  server.answer = () => Promise.reject(new TypeError("network"));
  await expect(a.save(read(a, "rA"), 7, EDIT_DATE, title)).resolves.toEqual({ kind: "unsure" });
  server.answer = () => changed();
  await expect(a.replay(read(a, "rB"))).resolves.toEqual({ kind: "changed", superseded: false });
  const underB = read(a, "rB");
  expect(a.status(key)).toBe("unsure");
  expect(a.canSave(underB)).toBe(false);
  expect(a.save(underB, 7, EDIT_DATE, { title: "A new edit" })).toBeNull();
  // Place A again: the held original applies, and the replay of its bytes is answered from the receipt.
  const underA = read(a, "rA");
  expect(a.canSave(underA)).toBe(false);
  server.answer = () => stored("rA2");
  await expect(a.replay(underA)).resolves.toEqual({ kind: "stored", revision: "rA2" });
  expect(new Set(server.sent.map((s) => s.body)).size).toBe(1);
  expect(a.canSave(underA)).toBe(false);
  expect(a.canSave(read(a, "rA2"))).toBe(true);
});

it("a proved supersession ends the doubt only with a read made after it that shows another base", async () => {
  const server = await editServer();
  const { a } = setup(server);
  const key = `7:${EDIT_DATE}`;
  server.answer = () => acked(503, { error: "x" });
  await expect(a.save(read(a, "r0"), 7, EDIT_DATE, title)).resolves.toEqual({ kind: "unsure" });
  const early = a.stamp(7, EDIT_DATE);
  server.answer = () => changed(true);
  await expect(a.replay(read(a, "r3"))).resolves.toEqual({ kind: "changed", superseded: true });
  expect(a.register(early, { occurrenceId: OCC, revision: "r3" })).not.toBeNull();
  expect(a.status(key)).toBe("unsure");
  read(a, "r0");
  expect(a.status(key)).toBe("unsure");
  // A repairable refusal of a later replay answers only that send.
  server.answer = () => acked(400, { error: "x", code: "invalid_duration" });
  await expect(a.replay(read(a, "r0"))).resolves.toMatchObject({ kind: "refused" });
  expect(a.status(key)).toBe("unsure");
  const after = read(a, "r3");
  expect(a.status(key)).toBe("idle");
  expect(a.canSave(after)).toBe(true);
});

it("a replay that finds the date changed retires the draft that made the doubt, never a first send's or another draft", async () => {
  const server = await editServer();
  const { a } = setup(server);
  const owner = { id: "draft-owner", versions: { title: 2 } };
  // A first send's conflict leaves its draft to rebase.
  server.answer = () => changed(true);
  await expect(a.save(read(a, "r0"), 7, EDIT_DATE, title, { draft: { id: "draft-first", versions: { title: 1 } } })).resolves.toMatchObject({ kind: "changed" });
  expect(a.draftRetired("draft-first")).toBe(false);
  server.answer = () => Promise.reject(new TypeError("network"));
  await expect(a.save(read(a, "r1"), 7, EDIT_DATE, title, { draft: owner })).resolves.toEqual({ kind: "unsure" });
  expect(a.draftRetired(owner.id)).toBe(false);
  // Still unsure: the draft stays until a replay answers.
  server.answer = () => Promise.reject(new TypeError("network"));
  await expect(a.replay(read(a, "r1"))).resolves.toEqual({ kind: "unsure" });
  expect(a.draftRetired(owner.id)).toBe(false);
  server.answer = () => changed(true);
  await expect(a.replay(read(a, "r1"))).resolves.toEqual({ kind: "changed", superseded: true });
  expect(a.draftRetired(owner.id)).toBe(true);
  expect(a.draftRetired("draft-first")).toBe(false);
  // A different base read after that ends the doubt; the retired draft stays retired, and nothing it sent can be replayed.
  const fresh = read(a, "r9");
  expect(a.status(`7:${EDIT_DATE}`)).toBe("idle");
  expect(a.recovery(fresh)).toBeNull();
  expect(a.draftRetired(owner.id)).toBe(true);
  a.dispose();
  expect(a.draftRetired(owner.id)).toBe(false);
});

it("a stored replay settles the draft that made the doubt, once, for a read made after it", async () => {
  const server = await editServer();
  const { a } = setup(server);
  const owner = { id: "draft-replayed", versions: { title: 3 } };
  server.answer = () => Promise.reject(new TypeError("network"));
  await expect(a.save(read(a, "r0"), 7, EDIT_DATE, title, { draft: owner })).resolves.toEqual({ kind: "unsure" });
  server.answer = () => stored("r1");
  await expect(a.replay(read(a, "r0"))).resolves.toEqual({ kind: "stored", revision: "r1" });
  const after = read(a, "r1");
  expect(a.takeSettledDraft(after, "draft-other")).toBeNull();
  expect(a.takeSettledDraft(after, owner.id)).toEqual({ title: 3 });
  expect(a.takeSettledDraft(after, owner.id)).toBeNull();
  expect(a.draftRetired(owner.id)).toBe(false);
});

// The verdict's round, for a consent answer.
function roundOf(v: EditVerdict) {
  if (v.kind !== "consent") throw new Error(`no question: ${v.kind}`);

  return v.round;
}

it("asks only a readable, acknowledged challenge; Yes adds only its token, a roster write withdraws it, No restores the doubt", async () => {
  const server = await editServer();
  const { a, rosters } = setup(server);
  const key = `7:${EDIT_DATE}`;
  const consent = "0123456789abcdef0123456789abcdef";
  const tighten = { visibility: "invite_only" as const };
  let p = read(a, "r0");
  server.answer = () => needsConsent("not a token");
  await expect(a.save(p, 7, EDIT_DATE, tighten, { shown: SHOWN })).resolves.toMatchObject({ kind: "refused", code: "date_needs_consent" });
  server.answer = () => Response.json({ error: "x", code: "date_needs_consent", impact: challengeImpact(consent) }, { status: 409 });
  await expect(a.save(p, 7, EDIT_DATE, tighten, { shown: SHOWN })).resolves.toEqual({ kind: "unsure" });
  // Unsure now: settle it, then ask again.
  server.answer = () => stored("r1");
  await a.replay(p = read(a, "r0"));
  p = read(a, "r1");
  server.answer = () => needsConsent(consent);
  const v = await a.save(p, 7, EDIT_DATE, tighten, { reason: "  Keep it small  ", shown: SHOWN });
  expect(v).toMatchObject({ kind: "consent", token: consent, count: 2, current: "public", target: "invite_only" });
  const round = roundOf(v!);
  expect(a.question(p, round)).toBe(true);
  expect(a.question(p, { key })).toBe(false);
  expect(a.canSave(p)).toBe(false);
  server.answer = () => stored("r2");
  await expect(a.answer(round, true, p)).resolves.toEqual({ kind: "stored", revision: "r2" });
  const [asked, yes] = server.sent.slice(-2).map((s) => s.body);
  expect(yes).toBe(`${asked.slice(0, -1)},"consent":"${consent}"}`);
  expect(JSON.parse(asked).reason).toBe("Keep it small");
  // The answered round answers nothing again.
  expect(a.answer(round, true, p)).toBeNull();

  p = read(a, "r2");
  server.answer = () => needsConsent(consent);
  const withdrawn = roundOf((await a.save(p, 7, EDIT_DATE, tighten, { shown: SHOWN }))!);
  rosters.begin(key)("answered");
  expect(a.question(p, withdrawn)).toBe(false);
  expect(a.status(key)).toBe("idle");
  expect(a.answer(withdrawn, true, p)).toBeNull();

  // A challenged replay: No puts back the doubt, with the bytes last possibly sent.
  server.answer = () => Promise.reject(new TypeError("network"));
  await a.save(p, 7, EDIT_DATE, tighten, { shown: SHOWN });
  const doubtful = server.sent[server.sent.length - 1].body;
  server.answer = () => needsConsent(consent);
  const challenged = await a.replay(p = read(a, "r2"));
  expect(challenged).toMatchObject({ kind: "consent", token: consent });
  expect(a.answer(roundOf(challenged!), false, null)).toBeNull();
  expect(a.status(key)).toBe("unsure");
  server.answer = () => stored("r3");
  await a.replay(p);
  expect(server.sent[server.sent.length - 1].body).toBe(doubtful);
});

// DEC-158: the only question a date edit asks is the writer's own for exactly this save; anything else is a
// refusal with no Yes, and leaves the date as it was.
describe("the writer's question must be about this save", () => {
  const consent = "0123456789abcdef0123456789abcdef";
  const row = (extra: JsonObject = {}) => ({ id: 7, occurrenceId: OCC, title: "Supper · 2026-12-08", visibility: "public", registrants: 2, ...extra });
  const impact = (extra: JsonObject = {}): JsonObject => ({ ...challengeImpact(consent), ...extra });

  const cases: Array<[string, JsonObject, { changes?: DateEditChanges; shown?: Shown | null }?]> = [
    ["no token", without(impact(), "consent")],
    ["an upper-case token", impact({ consent: consent.toUpperCase() })],
    ["a short token", impact({ consent: consent.slice(1) })],
    ["no tightened row", impact({ tightened: [] })],
    ["two tightened rows", impact({ tightened: [row(), row({ occurrenceId: OCC + 1 })] })],
    ["another event's row", impact({ tightened: [row({ id: 8 })] })],
    ["another date's row", impact({ tightened: [row({ occurrenceId: OCC + 1 })] })],
    ["a row with no date", impact({ tightened: [without(row(), "occurrenceId")] })],
    ["a count of zero", impact({ tightened: [row({ registrants: 0 })] })],
    ["a negative count", impact({ tightened: [row({ registrants: -1 })] })],
    ["a fractional count", impact({ tightened: [row({ registrants: 1.5 })] })],
    ["a count past the safe range", impact({ tightened: [row({ registrants: 2 ** 53 })] })],
    ["a count written as text", impact({ tightened: [row({ registrants: "2" })] })],
    ["a row at the target's visibility", impact({ tightened: [row({ visibility: "invite_only" })] })],
    ["a decision row", impact({ tightened: [row({ decisionKey: "date:41" })] })],
    ["a leader-opened row", impact({ leaderOpened: [row()] })],
    ["no leader-opened list", without(impact(), "leaderOpened")],
    ["reset dates", impact({ resetDates: [] })],
    ["future overrides", impact({ futureOverrides: 1 })],
    ["another current value", impact({ currentVisibility: "private" })],
    ["another target", impact({ visibility: "private" })],
    ["a loosening", impact({ visibility: "public", currentVisibility: "invite_only", tightened: [row({ visibility: "invite_only" })] }), { shown: { visibility: "invite_only", seriesVisibility: "public" }, changes: { visibility: "public" } }],
    ["an unchanged value", impact({ visibility: "public", currentVisibility: "public" }), { changes: { visibility: "public" } }],
    ["a save that changes no visibility", impact(), { changes: { title: "Later" } }],
    ["a save that did not say what it showed", impact(), { shown: null }],
    ["a series value the read did not show", impact(), { changes: { visibility: "series" }, shown: { visibility: "public", seriesVisibility: "private" } }],
  ];

  for (const [name, body, opts] of cases) {
    it(`never asks for ${name}`, async () => {
      const server = await editServer();
      const { a } = setup(server);
      const p = read(a, "r0");
      server.answer = () => needsConsent(consent, body);
      const shown = opts?.shown === null ? undefined : opts?.shown ?? SHOWN;
      await expect(a.save(p, 7, EDIT_DATE, opts?.changes ?? { visibility: "invite_only" }, { shown })).resolves.toMatchObject({ kind: "refused", code: "date_needs_consent" });
      expect(a.status(`7:${EDIT_DATE}`)).toBe("idle");
      expect(a.canSave(p)).toBe(true);
      expect(server.sent).toHaveLength(1);
    });
  }

  it("resolves the series value, or a clear, from the read the save was made from", async () => {
    for (const visibility of ["series", null] as const) {
      const server = await editServer();
      const { a } = setup(server);
      const p = read(a, "r0");
      server.answer = () => needsConsent(consent);
      await expect(a.save(p, 7, EDIT_DATE, { visibility }, { shown: SHOWN })).resolves.toMatchObject({ kind: "consent", count: 2, current: "public", target: "invite_only" });
    }
  });

  it("an unsure attempt's replay is judged against the read its first send was made from", async () => {
    const server = await editServer();
    const { a } = setup(server);
    server.answer = () => Promise.reject(new TypeError("network"));
    await a.save(read(a, "r0"), 7, EDIT_DATE, { visibility: "series" }, { shown: SHOWN });
    server.answer = () => needsConsent(consent);
    await expect(a.replay(read(a, "r0"))).resolves.toMatchObject({ kind: "consent", target: "invite_only" });
  });

  it("a malformed question on a replay restores the doubt with the bytes it may have sent", async () => {
    const server = await editServer();
    const { a } = setup(server);
    server.answer = () => Promise.reject(new TypeError("network"));
    await a.save(read(a, "r0"), 7, EDIT_DATE, { visibility: "invite_only" }, { shown: SHOWN });
    const doubtful = server.sent[0].body;
    server.answer = () => needsConsent(consent, impact({ tightened: [row({ registrants: 0 })] }));
    await expect(a.replay(read(a, "r0"))).resolves.toMatchObject({ kind: "refused" });
    expect(a.status(`7:${EDIT_DATE}`)).toBe("unsure");
    server.answer = () => stored("r1");
    await a.replay(read(a, "r0"));
    expect(server.sent.map((s) => s.body)).toEqual([doubtful, doubtful, doubtful]);
  });
});

it("a question's answer is its round's alone: a kept Yes never answers the next round, and each new 409 needs its own", async () => {
  const server = await editServer();
  const { a } = setup(server);
  const key = `7:${EDIT_DATE}`;
  const consent = "0123456789abcdef0123456789abcdef";
  const p = read(a, "r0");
  server.answer = () => needsConsent(consent);
  const first = roundOf((await a.save(p, 7, EDIT_DATE, { visibility: "invite_only" }, { shown: SHOWN }))!);
  // The same count and token again: a new round.
  const second = await a.answer(first, true, p);
  const next = roundOf(second!);
  expect(next).not.toBe(first);
  expect(second).toMatchObject({ token: consent, count: 2 });
  expect(a.answer(first, true, p)).toBeNull();
  expect(a.answer(first, false, null)).toBeNull();
  expect(a.status(key)).toBe("consent");
  expect(a.question(p, next)).toBe(true);
  expect(server.sent).toHaveLength(2);
  // Another read may not send this read's question either.
  const other = read(a, "r0");
  expect(a.answer(next, true, { ...other, key: "7:2026-12-15" })).toBeNull();
  expect(a.status(key)).toBe("idle");
  expect(server.sent).toHaveLength(2);
});

it("holds only the fields the person submitted, and nothing of the old credential acts again", async () => {
  const server = await editServer();
  const { a } = setup(server);
  const p = read(a, "r0");
  server.answer = () => Promise.reject(new TypeError("network"));
  await a.save(p, 7, EDIT_DATE, { title: "New title", location: undefined, visibility: "series" }, { reason: " why " });
  const after = read(a, "r0");
  expect(a.recovery(after)).toEqual({ changes: { title: "New title", visibility: "series" }, reason: "why" });
  expect(Object.isFrozen(a.recovery(after)?.changes)).toBe(true);
  // Another tab signed in as someone else: this credential's store can do nothing.
  setMarkerCookie("m-someone-else");
  expect(a.canReplay(after)).toBe(false);
  expect(a.recovery(after)).toBeNull();
  expect(a.replay(after)).toBeNull();
  expect(a.register(a.stamp(7, EDIT_DATE), { occurrenceId: OCC, revision: "r0" })).toBeNull();
});

it("coalesces the refreshes one task asks for", async () => {
  const server = await editServer();
  const { a, rosters } = setup(server);
  const asked: string[][] = [];
  const settled: string[][] = [];
  a.onRefresh((keys, answered) => { asked.push([...keys]); settled.push([...answered]); });
  await a.save(read(a, "r0"), 7, EDIT_DATE, title);
  await tick();
  expect(asked).toEqual([[`7:${EDIT_DATE}`]]);
  rosters.begin(`7:${EDIT_DATE}`)("unsure");
  rosters.begin("7:2026-12-15")("stored");
  rosters.begin(`7:${EDIT_DATE}`)("lost");
  rosters.begin("7:2026-12-22")("answered");
  await tick();
  expect(asked).toEqual([[`7:${EDIT_DATE}`], [`7:${EDIT_DATE}`, "7:2026-12-15"]]);
  // Only a send's answer is a settlement: a roster write's end is the host page's own rule to read again.
  expect(settled).toEqual([[`7:${EDIT_DATE}`], []]);
});

it("the provider keeps one store per credential, shares the date with the legacy hold, and starts empty for a new credential", async () => {
  const server = await editServer();
  const seen: Array<DateAttempts | null> = [];
  const legacy: Array<ReturnType<typeof useDateWrites>> = [];

  function Probe() {
    seen.push(useDateAttempts());
    legacy.push(useDateWrites({ epoch: () => 0, dataEpoch: 0, shows: () => true, reload: () => {} }));

    return null;
  }

  const { rerender } = render(<DateWritesProvider credential={server.credential}><Probe /></DateWritesProvider>);
  rerender(<DateWritesProvider credential={server.credential}><Probe /></DateWritesProvider>);
  const store = seen[seen.length - 1];
  expect(store).not.toBeNull();
  // The store is made after the first commit, so only the first render sees none; every later one sees the same.
  expect(seen[0]).toBeNull();
  expect(new Set(seen.slice(1))).toEqual(new Set([store]));

  if (!store) return;
  const p = read(store, "r0");
  let release: (r: Response) => void = () => {};

  server.answer = () => new Promise((resolve) => { release = resolve; });
  let pending: Promise<EditVerdict> | null = null;
  act(() => { pending = store.save(p, 7, EDIT_DATE, title); });
  expect(legacy[legacy.length - 1].acquire({ eventId: 7 }, EDIT_DATE)).toBeNull();
  release(stored("r1"));
  await act(async () => { await pending; });
  const end = legacy[legacy.length - 1].acquire({ eventId: 7 }, EDIT_DATE);
  expect(end).not.toBeNull();
  expect(store.canSave(read(store, "r1"))).toBe(false);
  act(() => end?.("refused"));
  expect(store.canSave(read(store, "r1"))).toBe(true);

  const next = await editServer();
  rerender(<DateWritesProvider credential={next.credential}><Probe /></DateWritesProvider>);
  expect(seen[seen.length - 1]).not.toBe(store);
  expect(store.register(store.stamp(7, EDIT_DATE), { occurrenceId: OCC, revision: "r1" })).toBeNull();
  expect(store.canSave(p)).toBe(false);
  rerender(<DateWritesProvider credential={null}><Probe /></DateWritesProvider>);
  expect(seen[seen.length - 1]).toBeNull();
});

it("useEditBase registers the committed read and retires it when it is replaced or leaves", async () => {
  const server = await editServer();
  const { a } = setup(server);
  const permits: Array<ReadPermit | null> = [];

  function Host({ stamp, revision }: { stamp: ReadStamp; revision: string }) {
    permits.push(useEditBase(a, stamp, { occurrenceId: OCC, revision }));

    return null;
  }

  const first = a.stamp(7, EDIT_DATE);
  const { rerender, unmount } = render(<Host stamp={first} revision="r0" />);
  const p = permits[permits.length - 1];
  expect(p && a.canSave(p)).toBe(true);
  const second = a.stamp(7, EDIT_DATE);
  rerender(<Host stamp={second} revision="r1" />);
  const q = permits[permits.length - 1];
  expect(q?.revision).toBe("r1");
  expect(p && a.canSave(p)).toBe(false);
  unmount();
  expect(q && a.canSave(q)).toBe(false);
});

it("a replay withheld before fetch puts the doubt back, and a credential change while it waits ends it", async () => {
  const server = await editServer();
  const { a, rosters } = setup(server);
  const key = `7:${EDIT_DATE}`;
  server.answer = () => Promise.reject(new TypeError("network"));
  await a.save(read(a, "r0"), 7, EDIT_DATE, title);
  const fresh = read(a, "r0");
  const flag = { begun: false };

  const stop = a.subscribe(() => {
    if (flag.begun) return;
    flag.begun = true;
    rosters.begin(key)("answered");
  });

  await expect(a.replay(fresh)).resolves.toEqual({ kind: "notSent" });
  stop();
  expect(a.status(key)).toBe("unsure");
  expect(server.sent).toHaveLength(1);
  // An ambiguous answer arriving after the credential changed is dropped with the store.
  let release: (r: Response) => void = () => {};

  server.answer = () => new Promise((resolve) => { release = resolve; });
  const pending = a.replay(read(a, "r0"));
  expect(pending).not.toBeNull();
  server.boot = "someone-else";
  setMarkerCookie("m-someone-else");
  release(stored("r1"));
  await expect(pending).resolves.toEqual({ kind: "unsure" });
  await vi.waitFor(() => expect(currentCredential()?.token).toBe("someone-else"));
  expect(a.status(key)).toBe("unsure");
  expect(a.register(a.stamp(7, EDIT_DATE), { occurrenceId: OCC, revision: "r1" })).toBeNull();
  expect(a.replay(fresh)).toBeNull();
});

it("an answer that lands while the marker is briefly unreadable still settles, so the same credential comes back to it", async () => {
  const server = await editServer();
  const { a, rosters } = setup(server);
  const key = `7:${EDIT_DATE}`;
  const consent = "0123456789abcdef0123456789abcdef";
  let release: (r: Response) => void = () => {};

  server.answer = () => new Promise((resolve) => { release = resolve; });
  const pending = a.save(read(a, "r0"), 7, EDIT_DATE, title);
  // The bootstrap the lost answer starts adopts the same token, marker and viewer: the same lifetime.
  clearMarkerCookie();
  release(stored("r1"));
  await expect(pending).resolves.toEqual({ kind: "unsure" });
  await vi.waitFor(() => expect(currentCredential()).toBe(server.credential));
  expect(a.status(key)).toBe("unsure");
  const fresh = read(a, "r0");
  expect(a.canReplay(fresh)).toBe(true);
  server.answer = () => stored("r1");
  await a.replay(fresh);
  expect(new Set(server.sent.map((s) => s.body)).size).toBe(1);

  // A roster write that begins while the marker is unreadable still withdraws a standing question.
  server.answer = () => needsConsent(consent);
  const p = read(a, "r1");
  const round = roundOf((await a.save(p, 7, EDIT_DATE, { visibility: "invite_only" }, { shown: SHOWN }))!);
  clearMarkerCookie();
  rosters.begin(key)("answered");
  setMarkerCookie(server.credential.marker);
  expect(a.status(key)).toBe("idle");
  expect(a.question(p, round)).toBe(false);
});

it("a send keeps going when its host leaves: retiring the read does not stop the answer settling", async () => {
  for (const [answer, want] of [[() => stored("r1"), "idle"], [() => Promise.reject(new TypeError("network")), "unsure"]] as const) {
    const server = await editServer();
    const { a } = setup(server);
    let release: () => void = () => {};
    const gate = new Promise<void>((resolve) => { release = resolve; });

    server.answer = () => gate.then(answer);
    const p = read(a, "r0");
    const pending = a.save(p, 7, EDIT_DATE, title);
    a.retire(p);
    release();
    await pending;
    expect(a.status(`7:${EDIT_DATE}`)).toBe(want);
    expect(server.sent).toHaveLength(1);
  }
});

// ---- A credential's store ends with its lifetime ----

// The session's roster store, counting the listeners subscribed to it now and ever.
function countedRosters() {
  const inner = createRosterWrites();
  const count = { listening: 0, ever: 0 };

  const rosters: RosterWrites = {
    ...inner,
    subscribe: (listener) => {
      const stop = inner.subscribe(listener);
      let stopped = false;
      count.listening++;
      count.ever++;

      return () => {
        if (!stopped) count.listening--;
        stopped = true;
        stop();
      };
    },
  };

  return { rosters, count };
}

// The verdict if it has already been decided by the next task, otherwise "still pending".
async function settledBy(verdict: Promise<EditVerdict> | null): Promise<EditVerdict | "still pending"> {
  return Promise.race([verdict ?? Promise.reject(new Error("nothing was sent")), tick().then(() => "still pending" as const)]);
}

// What the provider's consumers were handed, render by render.
function AttemptsProbe({ seen }: { seen: Array<DateAttempts | null> }) {
  seen.push(useDateAttempts());

  return null;
}

function lastStore(seen: Array<DateAttempts | null>): DateAttempts {
  const store = seen[seen.length - 1];

  if (!store) throw new Error("the provider gave no store");

  return store;
}

// A store's own traffic after the test stops looking: what its subscribers and refresh listeners heard.
function heardOn(store: DateAttempts) {
  const heard = { notified: 0, refreshed: 0 };
  store.subscribe(() => { heard.notified++; });
  store.onRefresh(() => { heard.refreshed++; });

  return heard;
}

it("a replaced or ended credential disposes its store: the roster listener goes, the envelope is forgotten, a late answer publishes nothing", async () => {
  const server = await editServer();
  const { rosters, count } = countedRosters();
  const seen: Array<DateAttempts | null> = [];
  const key = `7:${EDIT_DATE}`;
  const providing = (credential: Credential | null) => <DateWritesProvider credential={credential} rosterWrites={rosters}><AttemptsProbe seen={seen} /></DateWritesProvider>;
  const { rerender } = render(providing(server.credential));
  const old = lastStore(seen);
  expect(count.listening).toBe(1);

  // An uncertain private envelope, then a replay of it whose answer is still on its way.
  server.answer = () => Promise.reject(new TypeError("network"));
  await act(async () => { await old.save(read(old, "r0"), 7, EDIT_DATE, { title: "Private title", location: "Room 4" }); });
  expect(old.recovery(read(old, "r0"))).toEqual({ changes: { title: "Private title", location: "Room 4" }, reason: undefined });
  let release: (r: Response) => void = () => {};

  server.answer = () => new Promise((resolve) => { release = resolve; });
  let replaying: Promise<EditVerdict> | null = null;
  act(() => { replaying = old.replay(read(old, "r0")); });
  expect(replaying).not.toBeNull();
  expect(old.status(key)).toBe("sending");
  const heard = heardOn(old);

  const next = await editServer();
  const before = seen.length;
  rerender(providing(next.credential));
  const fresh = lastStore(seen);
  expect(fresh).not.toBe(old);
  // No render under the new credential was handed the old store.
  expect(seen.slice(before)).not.toContain(old);
  // One listener: the old store's is gone, the new one's is attached.
  expect(count).toEqual({ listening: 1, ever: 2 });
  expect(old.status(key)).toBe("idle");
  expect(fresh.status(key)).toBe("idle");
  // Disposal cancelled the replay and answered its caller: it reached fetch, so it may have been stored.
  expect(server.sent[server.sent.length - 1].signal?.aborted).toBe(true);
  await expect(settledBy(replaying)).resolves.toEqual({ kind: "unsure" });

  // The old send's answer lands: nothing of the old store moves, and the new one never hears of it.
  release(stored("r1"));
  await act(async () => { await replaying; });
  await act(tick);
  expect(heard).toEqual({ notified: 0, refreshed: 0 });
  expect(old.status(key)).toBe("idle");
  expect(fresh.status(key)).toBe("idle");
  expect(fresh.recovery(read(fresh, "r0"))).toBeNull();
  // A roster write after disposal reaches only the live store.
  rosters.begin(key)("stored");
  await act(tick);
  expect(heard).toEqual({ notified: 0, refreshed: 0 });

  rerender(providing(null));
  expect(seen[seen.length - 1]).toBeNull();
  expect(count.listening).toBe(0);
  expect(fresh.register(fresh.stamp(7, EDIT_DATE), { occurrenceId: OCC, revision: "r1" })).toBeNull();
});

it("unmounting disposes the store while its credential is still current: a refresh queued just before is dropped, a late answer settles nothing", async () => {
  const server = await editServer();
  const { rosters, count } = countedRosters();
  const seen: Array<DateAttempts | null> = [];
  const key = `7:${EDIT_DATE}`;
  const { unmount } = render(<DateWritesProvider credential={server.credential} rosterWrites={rosters}><AttemptsProbe seen={seen} /></DateWritesProvider>);
  const store = lastStore(seen);
  let lose: () => void = () => {};
  const lost = new Promise<void>((resolve) => { lose = resolve; });

  server.answer = () => lost.then(() => Promise.reject(new TypeError("network")));
  let pending: Promise<EditVerdict> | null = null;
  act(() => { pending = store.save(read(store, "r0"), 7, EDIT_DATE, { title: "Private title" }); });
  expect(server.sent).toHaveLength(1);
  const heard = heardOn(store);

  // A roster end on another date queues a refresh; the unmount comes before the task that would deliver it.
  rosters.begin("7:2026-12-15")("stored");
  unmount();
  expect(count.listening).toBe(0);
  expect(currentCredential()).toBe(server.credential);
  await tick();
  expect(heard.refreshed).toBe(0);
  const notifiedAtUnmount = heard.notified;

  // Its caller was answered at the unmount, before the transport failed: sent, so unsure.
  expect(server.sent[0].signal?.aborted).toBe(true);
  await expect(settledBy(pending)).resolves.toEqual({ kind: "unsure" });

  // Nor does a later failure schedule a refresh for nobody.
  const scheduled = vi.spyOn(globalThis, "queueMicrotask");
  lose();
  await expect(pending).resolves.toEqual({ kind: "unsure" });
  await tick();
  expect(scheduled).not.toHaveBeenCalled();
  scheduled.mockRestore();
  expect(heard).toEqual({ notified: notifiedAtUnmount, refreshed: 0 });
  // Nothing of it can act again, though the credential is current: no read registers, no envelope remains.
  expect(store.status(key)).toBe("idle");
  expect(store.register(store.stamp(7, EDIT_DATE), { occurrenceId: OCC, revision: "r0" })).toBeNull();
  expect(server.sent).toHaveLength(1);
});

it("a send whose claim's own notification disposes the store is never handed to fetch", async () => {
  const server = await editServer();
  const { a } = setup(server);
  const p = read(a, "r0");
  const stop = a.subscribe(() => a.dispose());
  const original = DateEdits.send;
  let handed = 0;
  // The envelope never even reaches core, where a hung wait would hold its bytes.
  DateEdits.send = (...args) => {
    handed++;

    return original(...args);
  };

  try {
    await expect(a.save(p, 7, EDIT_DATE, title)).resolves.toEqual({ kind: "notSent" });
  } finally {
    DateEdits.send = original;
  }

  expect(handed).toBe(0);
  stop();
  expect(server.sent).toHaveLength(0);
  expect(a.status(`7:${EDIT_DATE}`)).toBe("idle");
  // A second dispose changes nothing, and a getter asked afterwards keeps no record.
  a.dispose();
  expect(a.question(p, { key: p.key })).toBe(false);
  expect(a.recovery(p)).toBeNull();
  expect(a.answer({ key: p.key }, true, p)).toBeNull();
  expect(a.save(p, 7, EDIT_DATE, title)).toBeNull();
});

it("under Strict Mode the provider leaves one live store and one roster listener, and none after unmount", async () => {
  const server = await editServer();
  const { rosters, count } = countedRosters();
  const seen: Array<DateAttempts | null> = [];
  const { unmount } = render(<StrictMode><DateWritesProvider credential={server.credential} rosterWrites={rosters}><AttemptsProbe seen={seen} /></DateWritesProvider></StrictMode>);
  const store = lastStore(seen);
  expect(count.listening).toBe(1);
  const p = read(store, "r0");
  expect(store.canSave(p)).toBe(true);
  await act(async () => { await store.save(p, 7, EDIT_DATE, title); });
  expect(server.sent).toHaveLength(1);
  unmount();
  expect(count.listening).toBe(0);
});

it("a render React abandons before commit leaves no store listening", async () => {
  const server = await editServer();
  const { rosters, count } = countedRosters();
  const never = new Promise<never>(() => {});

  function Waits({ children }: { children: ReactNode }) {
    use(never);

    return children;
  }

  const { unmount } = render(<Suspense fallback={<p>loading</p>}><DateWritesProvider credential={server.credential} rosterWrites={rosters}><Waits><p>never shown</p></Waits></DateWritesProvider></Suspense>);
  await act(tick);
  expect(screen.getByText("loading")).toBeInTheDocument();
  expect(count.ever).toBe(0);
  unmount();
  expect(count.listening).toBe(0);
});

it("a routine recheck that adopts the same credential keeps its store and its uncertain envelope", async () => {
  const server = await editServer();
  const { rosters, count } = countedRosters();
  const seen: Array<DateAttempts | null> = [];
  const providing = (credential: Credential | null) => <DateWritesProvider credential={credential} rosterWrites={rosters}><AttemptsProbe seen={seen} /></DateWritesProvider>;
  const { rerender } = render(providing(server.credential));
  const store = lastStore(seen);
  server.answer = () => Promise.reject(new TypeError("network"));
  await act(async () => { await store.save(read(store, "r0"), 7, EDIT_DATE, title); });

  const { credential } = await bootstrapCredential(true);
  expect(credential).toBe(server.credential);
  rerender(providing(credential));
  expect(lastStore(seen)).toBe(store);
  expect(count).toEqual({ listening: 1, ever: 1 });
  expect(store.status(`7:${EDIT_DATE}`)).toBe("unsure");
  expect(store.recovery(read(store, "r0"))).toEqual({ changes: title, reason: undefined });
});

// ---- Disposal owns every send in flight ----

// The stored 200 the "answers stored later anyway" transport holds until the test lets it land.
let answerLate: () => void = () => {};

// How a transport treats a send while its store is disposed: it rejects on the abort as fetch does, never
// settles at all, answers a stored 200 regardless, or sends its headers and then never finishes the body.
const transports = {
  "honours the abort": (signal: AbortSignal | null) => new Promise<Response>((_resolve, reject) => {
    signal?.addEventListener("abort", () => reject(new DOMException("aborted", "AbortError")), { once: true });
  }),
  "never settles": () => new Promise<Response>(() => {}),
  "answers stored later anyway": () => new Promise<Response>((resolve) => { answerLate = () => resolve(stored("r1")); }),
  "stalls its body": () => {
    const res = stored("r1");
    // The headers arrived; the body never finishes.
    res.text = () => new Promise<string>(() => {});

    return res;
  },
} as const;

for (const [transport, answer] of Object.entries(transports)) {
  for (const route of ["the credential is replaced", "the provider unmounts while the credential is current"] as const) {
    it(`a send in flight when ${route}, on a transport that ${transport}, is aborted and answered unsure at once, publishing nothing`, async () => {
      const server = await editServer();
      const { rosters, count } = countedRosters();
      const seen: Array<DateAttempts | null> = [];
      const key = `7:${EDIT_DATE}`;
      const providing = (credential: Credential | null) => <DateWritesProvider credential={credential} rosterWrites={rosters}><AttemptsProbe seen={seen} /></DateWritesProvider>;
      const view = render(providing(server.credential));
      const store = lastStore(seen);
      server.answer = (_url, _body, signal) => answer(signal);
      let pending: Promise<EditVerdict> | null = null;
      const p = read(store, "r0");
      act(() => { pending = store.save(p, 7, EDIT_DATE, { title: "Private title", location: "Room 4" }); });
      expect(server.sent).toHaveLength(1);
      const signal = server.sent[0].signal;
      // Whatever the transport has already answered (headers included) is in before the lifetime ends.
      await act(tick);
      expect(signal?.aborted).toBe(false);
      expect(store.status(key)).toBe("sending");
      const heard = heardOn(store);
      let fresh: DateAttempts | null = null;

      if (route === "the credential is replaced") {
        const next = await editServer();
        view.rerender(providing(next.credential));
        fresh = lastStore(seen);
        expect(fresh).not.toBe(store);
      } else {
        view.unmount();
        expect(credentialCurrent(server.credential)).toBe(true);
      }

      expect(signal?.aborted).toBe(true);
      expect(count.listening).toBe(fresh ? 1 : 0);
      // Answered within a task, whatever the transport does: it reached fetch, so it may have been stored.
      await expect(settledBy(pending)).resolves.toEqual({ kind: "unsure" });
      answerLate();
      await expect(pending).resolves.toEqual({ kind: "unsure" });
      await act(() => new Promise<void>((resolve) => { setTimeout(resolve, 20); }));
      expect(heard).toEqual({ notified: 0, refreshed: 0 });
      expect(store.status(key)).toBe("idle");
      expect(store.recovery(p)).toBeNull();
      expect(store.canReplay(p)).toBe(false);
      expect(store.register(store.stamp(7, EDIT_DATE), { occurrenceId: OCC, revision: "r1" })).toBeNull();

      if (fresh) {
        expect(fresh.status(key)).toBe("idle");
        expect(fresh.recovery(read(fresh, "r0"))).toBeNull();
        view.unmount();
      }
    });
  }
}

it("a send not yet handed to fetch when its store is disposed sends nothing and answers notSent", async () => {
  const server = await editServer();
  const { a } = setup(server);
  const p = read(a, "r0");
  const asked = vi.fn(() => true);
  const original = DateEdits.send;
  // The transport is still awaiting something of its own (a credential change) when the store goes.
  let hold: () => void = () => {};

  DateEdits.send = (sent, pinned, gate) => new Promise<void>((resolve) => { hold = resolve; }).then(() => {
    asked();

    return gate.permit() ? original(sent, pinned, gate) : Promise.reject(new Error("withheld"));
  });

  try {
    const pending = a.save(p, 7, EDIT_DATE, title);
    a.dispose();
    await expect(settledBy(pending)).resolves.toEqual({ kind: "notSent" });
    hold();
    await tick();
    expect(asked).toHaveBeenCalledTimes(1);
    expect(server.sent).toHaveLength(0);
  } finally {
    DateEdits.send = original;
  }
});

it("an answer whose own notification disposes the store reaches its caller as unsure, never stored", async () => {
  const server = await editServer();
  const { a } = setup(server);
  let release: (r: Response) => void = () => {};

  server.answer = () => new Promise((resolve) => { release = resolve; });
  const pending = a.save(read(a, "r0"), 7, EDIT_DATE, title);
  a.subscribe(() => { if (a.status(`7:${EDIT_DATE}`) === "idle") a.dispose(); });
  release(stored("r1"));
  await expect(pending).resolves.toEqual({ kind: "unsure" });
});

it("retiring the read, a routine recheck of the same credential and Strict Mode's rehearsal never cancel a send in flight", async () => {
  const server = await editServer();
  const { rosters } = countedRosters();
  const seen: Array<DateAttempts | null> = [];
  const key = `7:${EDIT_DATE}`;
  const providing = (credential: Credential | null) => <StrictMode><DateWritesProvider credential={credential} rosterWrites={rosters}><AttemptsProbe seen={seen} /></DateWritesProvider></StrictMode>;
  const view = render(providing(server.credential));
  const store = lastStore(seen);
  let release: (r: Response) => void = () => {};

  server.answer = () => new Promise((resolve) => { release = resolve; });
  const p = read(store, "r0");
  let pending: Promise<EditVerdict> | null = null;
  act(() => { pending = store.save(p, 7, EDIT_DATE, title); });
  const signal = server.sent[0].signal;
  const heard = heardOn(store);

  // The host's read leaves (a route change), and a recheck adopts the same credential again.
  store.retire(p);
  const { credential } = await bootstrapCredential(true);
  expect(credential).toBe(server.credential);
  view.rerender(providing(credential));
  expect(lastStore(seen)).toBe(store);
  expect(signal?.aborted).toBe(false);
  expect(store.status(key)).toBe("sending");

  release(stored("r1"));
  await act(async () => { await pending; });
  await expect(pending).resolves.toEqual({ kind: "stored", revision: "r1" });
  await act(tick);
  expect(heard.notified).toBeGreaterThan(0);
  expect(heard.refreshed).toBe(1);
  expect(store.status(key)).toBe("idle");
  view.unmount();
});

// The web project's lib stops at ES2020; Node has WeakRef, and only this test uses it.
declare const WeakRef: new <T extends object>(target: T) => { deref(): T | undefined };

type WeakRef<T extends object> = { deref(): T | undefined };

// A real collection, so the test sees what the heap still holds; the flag lets a fresh context reach gc().
function collector(): () => Promise<void> {
  setFlagsFromString("--expose-gc");
  const gc = runInNewContext("gc");

  return async () => {
    // A WeakRef keeps its target until the job that made or read it ends.
    await tick();
    gc();
    await tick();
    gc();
  };
}

for (const transport of ["honours the abort", "never settles"] as const) {
  it(`a disposed store keeps no envelope of a send whose transport ${transport}`, async () => {
    const collect = collector();
    const server = await editServer();
    const { rosters } = countedRosters();
    const seen: Array<DateAttempts | null> = [];
    const view = render(<DateWritesProvider credential={server.credential} rosterWrites={rosters}><AttemptsProbe seen={seen} /></DateWritesProvider>);
    const store = lastStore(seen);
    server.answer = (_url, _body, signal) => transports[transport](signal);
    const original = DateEdits.send;
    let envelope: WeakRef<DateEditEnvelope> | null = null;
    // Not vi.spyOn: its recorded calls would hold the envelope themselves.
    DateEdits.send = (sent, pinned, gate) => {
      envelope = new WeakRef(sent);

      return original(sent, pinned, gate);
    };

    try {
      let pending: Promise<EditVerdict> | null = null;
      act(() => { pending = store.save(read(store, "r0"), 7, EDIT_DATE, { title: "Private title" }); });
      const sending = () => envelope?.deref();
      await collect();
      // While the store is alive it holds the envelope: the check can see one.
      expect(sending()).toBeDefined();

      view.unmount();
      await expect(settledBy(pending)).resolves.toEqual({ kind: "unsure" });
      await collect();
      expect(sending()).toBeUndefined();
      // The store object itself is still reachable here (the probe saw it).
      expect(seen).toContain(store);
    } finally {
      DateEdits.send = original;
    }
  });
}
