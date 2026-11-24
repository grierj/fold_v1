import { act, render, screen } from "@testing-library/react";
import { createRef, useState, type RefObject } from "react";
import { describe, expect, it, vi } from "vitest";
import { ApiError } from "../api/core";
import { createRosterWrites, RosterWritesProvider, outcomeOf, rosterWriteFailure, useRosterActivity, useRosterActivityOf, useRosterGate, useRosterSettled, useRosterWrites, type RosterWrites } from "./rosterWrites";
import type { LoadOutcome } from "./useLoad";

const DATE = "2026-12-01";

// Roster writes in flight and the gate (design 8j). A host's useLoad is played by the test: `epoch` is what
// epoch() reads now, `dataEpoch` what the data shown was asked at, and retry is a spy that moves the epoch.
interface FakeLoad { epoch: number; dataEpoch: number; retry: ReturnType<typeof vi.fn<() => Promise<LoadOutcome>>> }

function fakeLoad(): FakeLoad {
  const load: FakeLoad = { epoch: 1, dataEpoch: 1, retry: vi.fn<() => Promise<LoadOutcome>>() };
  load.retry.mockImplementation(() => { load.epoch += 2;

    return Promise.resolve("loaded"); });

  return load;
}

type Gate = ReturnType<typeof useRosterGate>;

function GateHost({ load, out, date = DATE, renders }: { load: FakeLoad; out: RefObject<Gate | null>; date?: string; renders?: boolean[] }) {
  const gate = useRosterGate(7, date, { epoch: () => load.epoch, dataEpoch: load.dataEpoch, retry: load.retry });
  out.current = gate;
  renders?.push(gate.closed);

  return <p>{gate.closed ? "closed" : "open"}{gate.note ? " note" : ""}</p>;
}

function gateOf(out: RefObject<Gate | null>): Gate {
  if (!out.current) throw new Error("no gate");

  return out.current;
}

// Lands data asked now (a recheck's or the gate's read answering).
function land(load: FakeLoad, rerender: () => void) {
  load.epoch += 1;
  load.dataEpoch = load.epoch;
  act(rerender);
}

function gateSetup(store = createRosterWrites()) {
  const load = fakeLoad();
  const out = createRef<Gate>();
  const tree = () => <RosterWritesProvider rosterWrites={store}><GateHost load={load} out={out} /></RosterWritesProvider>;
  const view = render(tree());

  return { store, load, out, rerender: () => view.rerender(tree()), view };
}

it("counts roster writes per date and ends each write once, telling listeners", () => {
  const store = createRosterWrites();
  const heard: Array<{ key: string; eventId: number; end?: string }> = [];
  store.subscribe((event) => heard.push(event));
  const end = store.begin("7:a");
  const other = store.begin("7:b");
  expect([store.pending("7:a"), store.pending("7:b")]).toEqual([1, 1]);
  end("stored");
  end("lost");
  expect([store.pending("7:a"), store.pending("7:b")]).toEqual([0, 1]);
  other("answered");
  expect(heard).toEqual([{ key: "7:a", eventId: 7 }, { key: "7:b", eventId: 7 }, { key: "7:a", eventId: 7, end: "stored" }, { key: "7:b", eventId: 7, end: "answered" }]);
});

it("moves changes, mark, changedSince and pendingFor at every begin and end, before any listener hears", () => {
  const store = createRosterWrites();
  const seen: Array<[number, number, boolean, number]> = [];
  store.subscribe(() => seen.push([store.changes("7:a"), store.mark(), store.changedSince(0, [7]), store.pendingFor([7])]));
  expect([store.changes("7:a"), store.mark(), store.changedSince(0, [7]), store.pendingFor([7])]).toEqual([0, 0, false, 0]);
  const m0 = store.mark();
  const end = store.begin("7:a");
  // Synchronously, with no render or tick in between.
  expect(store.changes("7:a")).toBe(1);
  expect(store.mark()).toBe(1);
  expect(store.changedSince(m0, [7])).toBe(true);
  expect(store.pendingFor([7])).toBe(1);
  const m1 = store.mark();
  expect(store.changedSince(m1, [7])).toBe(false);
  end("answered");
  expect(store.changes("7:a")).toBe(2);
  expect(store.changedSince(m1, [7])).toBe(true);
  expect(store.pendingFor([7])).toBe(0);
  // A second ending does nothing.
  end("stored");
  expect(store.mark()).toBe(2);
  expect(seen).toEqual([[1, 1, true, 1], [2, 2, true, 0]]);
});

it("keeps a second date and a second event apart, and changedSince ignores events not listed", () => {
  const store = createRosterWrites();
  const m = store.mark();
  const a = store.begin("7:2026-12-01");
  const b = store.begin("7:2026-12-08");
  const c = store.begin("8:2026-12-01");
  expect([store.changes("7:2026-12-01"), store.changes("7:2026-12-08"), store.changes("8:2026-12-01"), store.changes("9:2026-12-01")]).toEqual([1, 2, 3, 0]);
  expect([store.pending("7:2026-12-01"), store.pending("7:2026-12-08"), store.pending("8:2026-12-01")]).toEqual([1, 1, 1]);
  expect(store.pendingFor([7])).toBe(2);
  expect(store.pendingFor([8])).toBe(1);
  expect(store.pendingFor([7, 8])).toBe(3);
  // A listed id named twice counts once.
  expect(store.pendingFor([7, 7])).toBe(2);
  expect(store.pendingFor([9])).toBe(0);
  expect(store.pendingFor([])).toBe(0);
  expect(store.changedSince(m, [9])).toBe(false);
  expect(store.changedSince(m, [])).toBe(false);
  const after = store.mark();
  c("stored");
  expect(store.changedSince(after, [7])).toBe(false);
  expect(store.changedSince(after, [8])).toBe(true);
  expect(store.changedSince(after, [9, 8])).toBe(true);
  expect(store.changes("7:2026-12-01")).toBe(1);
  a("lost");
  b("unsure");
  expect(store.pendingFor([7, 8])).toBe(0);
  expect([store.changes("7:2026-12-01"), store.changes("7:2026-12-08"), store.changes("8:2026-12-01")]).toEqual([5, 6, 4]);
});

// What ActivityProbe saw at its last render.
interface ActivitySeen { writing?: boolean; changes?: () => number; renders: number; pending?: number }

function ActivityProbe({ out }: { out: ActivitySeen }) {
  const date = useRosterActivity(7, DATE);
  const events = useRosterActivityOf();
  out.renders++;
  out.writing = date.writing;
  out.changes = date.changes;
  out.pending = events.pendingFor([7]);

  return <p>{date.writing ? "writing" : "idle"} {events.pendingFor([8])}</p>;
}

it("useRosterActivity gives the date's writing and changes, and useRosterActivityOf renders at every begin and end", () => {
  const store: RosterWrites = createRosterWrites();
  const out: ActivitySeen = { renders: 0 };
  render(<RosterWritesProvider rosterWrites={store}><ActivityProbe out={out} /></RosterWritesProvider>);
  expect(screen.getByText("idle 0")).toBeInTheDocument();
  expect(out.changes?.()).toBe(0);
  let end: (e: "stored" | "answered") => void = () => {};

  act(() => { end = store.begin(`7:${DATE}`); });
  expect(screen.getByText("writing 0")).toBeInTheDocument();
  expect(out.writing).toBe(true);
  expect(out.pending).toBe(1);
  expect(out.changes?.()).toBe(1);
  // Another date of the event is not this date's write, but is the event's.
  let other: (e: "stored") => void = () => {};

  act(() => { other = store.begin("7:2026-12-08"); });
  expect(out.pending).toBe(2);
  expect(out.changes?.()).toBe(1);
  act(() => end("answered"));
  expect(out.writing).toBe(false);
  expect(out.changes?.()).toBe(3);
  act(() => other("stored"));
  expect(out.pending).toBe(0);
  // A second event's write renders the event hook too.
  const renders = out.renders;
  let eight: (e: "stored") => void = () => {};

  act(() => { eight = store.begin("8:2026-12-01"); });
  expect(screen.getByText("idle 1")).toBeInTheDocument();
  expect(out.renders).toBeGreaterThan(renders);
  act(() => eight("stored"));
  expect(screen.getByText("idle 0")).toBeInTheDocument();
});

it("outside a provider the activity hooks read a private store where nothing is ever in flight", () => {
  const out: ActivitySeen = { renders: 0 };
  render(<ActivityProbe out={out} />);
  expect(out.writing).toBe(false);
  expect(out.pending).toBe(0);
  expect(out.changes?.()).toBe(0);
});

it("(a) stays open during a write nobody rechecked, then retries once after stored and closes until newer data", () => {
  const { store, load, out, rerender } = gateSetup();
  let end: (e: "stored") => void = () => {};

  act(() => { end = store.begin(`7:${DATE}`); });
  expect(gateOf(out).closed).toBe(false);
  expect(gateOf(out).gate.writing).toBe(true);
  act(() => end("stored"));
  expect(load.retry).toHaveBeenCalledTimes(1);
  expect(gateOf(out).closed).toBe(true);
  land(load, rerender);
  expect(gateOf(out).closed).toBe(false);
  expect(gateOf(out).note).toBe(false);
});

it("(b) after a recheck during the write, newer data keeps it closed until the write ends and data newer than the end lands", () => {
  const { store, load, out, rerender } = gateSetup();
  let end: (e: "stored") => void = () => {};

  act(() => { end = store.begin(`7:${DATE}`); });
  land(load, rerender);
  expect(gateOf(out).closed).toBe(true);
  act(() => end("stored"));
  expect(load.retry).toHaveBeenCalledTimes(1);
  expect(gateOf(out).closed).toBe(true);
  land(load, rerender);
  expect(gateOf(out).closed).toBe(false);
});

it("(c) a gate mounted during a write is closed for any data until the end and newer data, and retries once itself", () => {
  const store = createRosterWrites();
  let end: (e: "stored") => void = () => {};

  end = store.begin(`7:${DATE}`);
  const load = fakeLoad();
  const out = createRef<Gate>();
  // Every render, the first included, is closed: the gate never shows data from before it saw the write.
  const renders: boolean[] = [];
  const tree = () => <RosterWritesProvider rosterWrites={store}><GateHost load={load} out={out} renders={renders} /></RosterWritesProvider>;
  const view = render(tree());
  expect(renders.length).toBeGreaterThan(0);
  expect(renders.every(Boolean)).toBe(true);
  expect(gateOf(out).closed).toBe(true);
  land(load, () => view.rerender(tree()));
  expect(gateOf(out).closed).toBe(true);
  act(() => end("stored"));
  expect(load.retry).toHaveBeenCalledTimes(1);
  land(load, () => view.rerender(tree()));
  expect(gateOf(out).closed).toBe(false);
});

it("(d) answered never retries or notes, and after a recheck the recheck's data opens it once nothing is in flight", () => {
  const { store, load, out, rerender } = gateSetup();
  let end: (e: "answered") => void = () => {};

  act(() => { end = store.begin(`7:${DATE}`); });
  land(load, rerender);
  expect(gateOf(out).closed).toBe(true);
  act(() => end("answered"));
  expect(load.retry).not.toHaveBeenCalled();
  expect(gateOf(out).closed).toBe(false);
  expect(gateOf(out).note).toBe(false);
});

it("(e) lost and unsure retry once and note only data asked after the end, cleared by clearNote and the next begin", () => {
  for (const kind of ["lost", "unsure"] as const) {
    const { store, load, out, rerender, view } = gateSetup();
    let end: (e: typeof kind) => void = () => {};

    act(() => { end = store.begin(`7:${DATE}`); });
    act(() => end(kind));
    expect(load.retry).toHaveBeenCalledTimes(1);
    expect(gateOf(out).note).toBe(false);
    land(load, rerender);
    expect(gateOf(out).note).toBe(true);
    act(() => gateOf(out).clearNote());
    expect(gateOf(out).note).toBe(false);
    act(() => { end = store.begin(`7:${DATE}`); });
    act(() => end(kind));
    land(load, rerender);
    expect(gateOf(out).note).toBe(true);
    act(() => { store.begin(`7:${DATE}`); });
    expect(gateOf(out).note).toBe(false);
    view.unmount();
  }
});

it("(f) overlapping writes open only after both end and newer data lands; another date's write never closes it", () => {
  const { store, load, out, rerender } = gateSetup();
  let first: (e: "stored" | "answered") => void = () => {};
  let second: (e: "stored" | "answered") => void = () => {};

  act(() => { first = store.begin(`7:${DATE}`); second = store.begin(`7:${DATE}`); });
  land(load, rerender);
  act(() => first("stored"));
  land(load, rerender);
  expect(gateOf(out).closed).toBe(true);
  act(() => second("answered"));
  expect(gateOf(out).closed).toBe(false);
  expect(load.retry).toHaveBeenCalledTimes(1);
  let elsewhere: (e: "stored") => void = () => {};

  act(() => { elsewhere = store.begin("7:2026-12-08"); });
  land(load, rerender);
  expect(gateOf(out).closed).toBe(false);
  expect(gateOf(out).gate.writing).toBe(false);
  act(() => elsewhere("stored"));
  expect(load.retry).toHaveBeenCalledTimes(1);
});

it("(g) outside a provider the gate keeps its own store, and a roster's own registration registers nothing", () => {
  const load = fakeLoad();
  const out = createRef<Gate>();
  const own = createRef<ReturnType<typeof useRosterWrites>>();

  function Own() { own.current = useRosterWrites(7, DATE);

 return null; }

  render(<><GateHost load={load} out={out} /><Own /></>);
  let end: (e: "stored") => void = () => {};

  act(() => { end = gateOf(out).gate.begin(); });
  expect(gateOf(out).gate.writing).toBe(true);
  expect(own.current?.writing).toBe(false);
  act(() => end("stored"));
  expect(load.retry).toHaveBeenCalledTimes(1);
  act(() => { own.current?.begin()("stored"); });
  expect(own.current?.writing).toBe(false);
  expect(load.retry).toHaveBeenCalledTimes(1);
});

it("registers a roster's own writes inside a provider, for every roster of the date", () => {
  const store = createRosterWrites();
  const own = createRef<ReturnType<typeof useRosterWrites>>();

  function Own() { own.current = useRosterWrites(7, DATE);

 return null; }

  render(<RosterWritesProvider rosterWrites={store}><Own /></RosterWritesProvider>);
  let end: (e: "stored") => void = () => {};

  act(() => { end = store.begin(`7:${DATE}`); });
  expect(own.current?.writing).toBe(true);
  act(() => end("stored"));
  act(() => { end = own.current?.begin() ?? end; });
  expect(store.pending(`7:${DATE}`)).toBe(1);
});

it("keeps outcomeOf as it was and sorts roster write failures", () => {
  expect(outcomeOf(new ApiError(409, "changed"))).toBe("refused");
  expect(outcomeOf(new ApiError(404, "gone"))).toBe("refused");
  expect(outcomeOf(new ApiError(400, "bad", {}, true))).toBe("unsure");
  expect(outcomeOf(new ApiError(502, "proxy"))).toBe("unsure");
  expect(outcomeOf(new TypeError("Failed to fetch"))).toBe("unsure");

  for (const status of [401, 403, 404]) expect(rosterWriteFailure(new ApiError(status, "no"))).toBe("lost");
  expect(rosterWriteFailure(new TypeError("Failed to fetch"))).toBe("unsure");
  expect(rosterWriteFailure(new ApiError(500, "down"))).toBe("unsure");
  expect(rosterWriteFailure(new ApiError(503, "down"))).toBe("unsure");
  expect(rosterWriteFailure(new ApiError(403, "no", {}, true))).toBe("unsure");
  expect(rosterWriteFailure(new ApiError(400, "bad"))).toBe("answered");
  expect(rosterWriteFailure(new ApiError(409, "full", { code: "event_full" }))).toBe("answered");
  expect(rosterWriteFailure(new ApiError(409, "ask", { code: "date_add_approval_required" }))).toBe("answered");
});

// A page listing event 7's dates with no roster gate of its own (EventDetail, GroupDetail): its Edit this date waits on
// a roster write's end until it shows a read asked for after it.
function ListingPage({ onReload }: { onReload: () => void }) {
  const [epoch, setEpoch] = useState(0);
  const [dataEpoch, setDataEpoch] = useState(0);
  const settling = useRosterSettled({ epoch: () => epoch, dataEpoch, reload: () => { onReload(); setEpoch((n) => n + 1); } }, (eventId) => eventId === 7);

  return <>
    <p>{settling(7, DATE) ? "settling" : "free"} {settling(7, "2026-12-08") ? "other settling" : "other free"}</p>
    <button type="button" onClick={() => setDataEpoch(epoch)}>Show the read</button>
  </>;
}

describe("useRosterSettled", () => {
  function setup() {
    const store = createRosterWrites();
    const onReload = vi.fn();
    render(<RosterWritesProvider rosterWrites={store}><ListingPage onReload={onReload} /></RosterWritesProvider>);

    return { store, onReload };
  }

  it("an answered roster write and another event's write hold nothing and ask for nothing", () => {
    const { store, onReload } = setup();
    act(() => store.begin(`7:${DATE}`)("answered"));
    act(() => store.begin(`8:${DATE}`)("stored"));
    expect(onReload).not.toHaveBeenCalled();
    expect(screen.getByText("free other free")).toBeInTheDocument();
  });

  for (const end of ["stored", "lost", "unsure"] as const) {
    it(`a roster write that ends ${end} holds that date until the page shows a read asked for after it, and asks for the page once`, () => {
      const { store, onReload } = setup();
      const release = store.begin(`7:${DATE}`);
      expect(screen.getByText("free other free")).toBeInTheDocument();
      act(() => release(end));
      expect(onReload).toHaveBeenCalledOnce();
      expect(screen.getByText("settling other free")).toBeInTheDocument();
      act(() => screen.getByRole("button", { name: "Show the read" }).click());
      expect(screen.getByText("free other free")).toBeInTheDocument();
    });
  }
});
