import { createRosterWrites, RosterWritesProvider, type RosterWrites } from "../lib/rosterWrites";
import { renderWithAuth } from "../test/auth";
import { stubService } from "../test/service";
import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createRef, useRef, type RefObject } from "react";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError } from "../api/core";
import { Events } from "../api/events";
import { People } from "../api/people";
import { DateRooms } from "../api/rooms";
import { Site } from "../api/site";
import { ERROR_COPY, errorText } from "../lib/errors";
import { invalidateSite } from "../lib/useSite";
import { formatDayFull } from "../lib/when";
import { occurrence } from "../test/occurrence";
import type { Attendee, EventOccurrence, OccurrenceResponse } from "../types";
import { careKid, deferred, KEPT, KEPT_COUNT, keptForbiddenNow, keptPerson, observeKept, READ_OUTCOMES, readFailures, type ReadOutcome } from "../test/keptPanel";
import { DateEditor } from "./DateEditor";
import { useEditRead } from "../lib/editBase";
import { stubEditSends, verifiedCredential } from "../test/guardedEdits";
import { KeptDatesPanel } from "./KeptDatesPanel";
import { ConfirmProvider, useConfirm, usePageFacts } from "./ui/ConfirmDialog";

const actor = { user: { id: 10, status: "active" as const, accountType: "adult" as const } };
const render = renderWithAuth(() => actor);

stubService(Events, () => ({ managedDate: vi.fn(), dateVisibilityImpact: vi.fn(), removeRegistration: vi.fn(), promoteRegistration: vi.fn(), restoreRegistration: vi.fn(), addRegistration: vi.fn() }));

stubService(People, () => ({ search: vi.fn() }));

stubService(Site, () => ({ get: vi.fn() }));

stubService(DateRooms, () => ({ list: vi.fn(), set: vi.fn() }));

const A = "2026-12-01";
const B = "2026-12-08";
const HEADINGS = [A, B].map(formatDayFull);
const { checking: CHECKING, unknown: UNKNOWN, refused: REFUSED, nobodyOne: NOBODY_ONE, nobodyHere: NOBODY_HERE, saveLine: SAVE_LINE, writeNote: WRITE_NOTE } = KEPT;
const COUNT = KEPT_COUNT;
const person = keptPerson;
const kit = careKid;
const waiting: Attendee = { registrationId: 60, userId: 8, name: "Wait Person", status: "waitlisted", attendanceMode: "in_person", guestCount: 0 };
const gone: Attendee = { registrationId: 50, userId: 6, name: "Gone Person", status: "removed", attendanceMode: "in_person", guestCount: 0 };

// A manager's read of one date with a kept row (the fixture W4b names), or what `over` changes.
const roster = (date: string, over: Partial<EventOccurrence> = {}): EventOccurrence => occurrence({ date, startsLocal: `${date}T19:00`, attendeesVisible: true, keptCount: 1, attendees: [person("Old Name"), kit()], ...over });
const read = (date: string, over: Partial<EventOccurrence> = {}): OccurrenceResponse => ({ occurrence: roster(date, over) });
const fresh = (date: string) => read(date, { attendees: [person("New Name")] });

const failures = readFailures;

type Outcome = ReadOutcome;

const OUTCOMES = READ_OUTCOMES;

// Holds the next managed read.
function holdRead() {
  const held = deferred<OccurrenceResponse>();
  vi.mocked(Events.managedDate).mockReturnValueOnce(held.promise);

  return held;
}

async function settle(held: ReturnType<typeof holdRead>, outcome: Outcome, date = A) {
  await act(async () => {
    if (outcome === "success") held.resolve(fresh(date));
    else held.reject(failures[outcome]());
    await held.promise.catch(() => {});
  });
}

const home = () => screen.getByRole("region", { name: "Sign-ups to check before you save" });

const forbiddenNow = () => keptForbiddenNow([A, B]);
const observe = () => observeKept([A, B]);

interface Mounted {
  store: RosterWrites;
  onRefused: ReturnType<typeof vi.fn>;
  onClose: ReturnType<typeof vi.fn>;
}

function mount(dates: string[] = [A], wrap: (panel: React.ReactNode) => React.ReactNode = (p) => p): Mounted {
  const store = createRosterWrites();
  const onRefused = vi.fn();
  const onClose = vi.fn();
  render(<MemoryRouter><RosterWritesProvider rosterWrites={store}>{wrap(<KeptDatesPanel eventId={7} keptDates={dates.map((date) => ({ date, keptCount: 1 }))} onRefused={onRefused} onClose={onClose} />)}</RosterWritesProvider></MemoryRouter>);

  return { store, onRefused, onClose };
}

async function openRemoveQuestion(name = "Old Name") {
  await userEvent.click(screen.getByRole("button", { name: `More for ${name}` }));
  await userEvent.click(screen.getByRole("menuitem", { name: `Remove ${name}` }));

  return screen.findByRole("dialog", { name: `Remove ${name}'s registration for this date?` });
}

const focusWindow = () => act(() => { window.dispatchEvent(new Event("focus")); });

beforeEach(() => {
  vi.resetAllMocks();
  invalidateSite();
  vi.mocked(Site.get).mockResolvedValue({ name: "Fold", timezone: "UTC", selfSignup: "off", roomsEnabled: false, pushEnabled: true });
  vi.mocked(Events.managedDate).mockImplementation((_id, date) => Promise.resolve(read(date)));
  vi.mocked(Events.removeRegistration).mockResolvedValue({});
  vi.mocked(Events.promoteRegistration).mockResolvedValue({});
  vi.mocked(Events.restoreRegistration).mockResolvedValue({});
  vi.mocked(Events.addRegistration).mockResolvedValue({});
  vi.spyOn(window, "confirm").mockImplementation(() => { throw new Error("native confirm inside the panel"); });
});

afterEach(() => { vi.useRealTimers(); });

describe("W4: the panel's own read", () => {
  it("reads the first refused date at mount and shows only the checking line until it answers", async () => {
    const first = holdRead();
    const seen = observe();
    mount([A]);
    expect(home()).toHaveTextContent(CHECKING);
    expect(Events.managedDate).toHaveBeenCalledWith(7, A);
    seen.stop();
    seen.expectClean();
    await act(async () => { first.resolve(read(A)); await first.promise; });
    expect(screen.getByRole("alert")).toHaveTextContent(ERROR_COPY.date_kept_after_leaving);
    expect(within(home()).getByRole("heading", { name: formatDayFull(A) })).toBeInTheDocument();
    expect(screen.getByText("Old Name")).toBeInTheDocument();
    expect(screen.getAllByText("Kept after leaving").some((el) => el.tagName === "P")).toBe(true);
    expect(document.body.textContent).not.toMatch(COUNT);
  });

  it.each([403, 404] as const)("a first read answering %s shows only the refused line and refuses once", async (status) => {
    vi.mocked(Events.managedDate).mockRejectedValueOnce(failures[status]());
    const { onRefused } = mount([A]);
    await waitFor(() => expect(onRefused).toHaveBeenCalledTimes(1));
    expect(home().textContent).toBe(REFUSED);
    expect(onRefused).toHaveBeenCalledWith(false);
    const calls = vi.mocked(Events.managedDate).mock.calls.length;
    focusWindow();
    expect(Events.managedDate).toHaveBeenCalledTimes(calls);
  });

  it.each(["network", 500] as const)("a first read failing (%s) shows only the unknown copy, and Try again recovers", async (kind) => {
    vi.mocked(Events.managedDate).mockRejectedValueOnce(failures[kind]());
    const { onRefused } = mount([A]);
    await screen.findByText(UNKNOWN);
    expect(forbiddenNow()).toEqual([]);
    const next = holdRead();
    await userEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(home()).toHaveTextContent(CHECKING);
    await act(async () => { next.resolve(read(A)); await next.promise; });
    expect(screen.getByText("Old Name")).toBeInTheDocument();
    expect(onRefused).not.toHaveBeenCalled();
  });

  it("Remove retries only the panel's read, through the gate, then says nobody is keeping the date", async () => {
    mount([A]);
    await screen.findByText("Old Name");
    await openRemoveQuestion();
    expect(screen.getByRole("dialog")).toHaveTextContent("They left the group and kept this date. Removing them ends that and tells them. They won't be able to re-register on their own.");
    expect(screen.getByRole("dialog").textContent).not.toContain("frees their place");
    const next = holdRead();
    await userEvent.click(screen.getByRole("button", { name: "Remove" }));
    await waitFor(() => expect(Events.managedDate).toHaveBeenCalledTimes(2));
    expect(home()).toHaveTextContent(CHECKING);
    expect(screen.queryByText("Old Name")).not.toBeInTheDocument();
    await act(async () => { next.resolve(read(A, { keptCount: 0, attendees: [kit()] })); await next.promise; });
    expect(screen.getByRole("status")).toHaveTextContent(NOBODY_ONE);
    expect(document.body.textContent).not.toContain(ERROR_COPY.date_kept_after_leaving);
    expect(document.body.textContent).not.toMatch(COUNT);
    expect(Events.managedDate).toHaveBeenCalledTimes(2);
  });

  it("(a) a first read already clear shows the nobody line and no ERROR_COPY line", async () => {
    vi.mocked(Events.managedDate).mockResolvedValueOnce(read(A, { keptCount: 0, attendees: [person("Old Name", { keptAfterLeaving: false })] }));
    mount([A]);
    expect(await screen.findByText(NOBODY_ONE)).toBeInTheDocument();
    expect(document.body.textContent).not.toContain(ERROR_COPY.date_kept_after_leaving);
  });

  it("(b) a recheck counting more kept rows shows each labelled row and no number", async () => {
    mount([A]);
    await screen.findByText("Old Name");
    vi.mocked(Events.managedDate).mockResolvedValueOnce(read(A, { keptCount: 2, attendees: [person("Old Name"), person("Second Name", { registrationId: 32, userId: 9 })] }));
    focusWindow();
    await screen.findByText("Second Name");
    expect(screen.getAllByText("Kept after leaving").filter((el) => el.tagName === "P")).toHaveLength(2);
    expect(screen.getByRole("alert")).toHaveTextContent(ERROR_COPY.date_kept_after_leaving);
    expect(home().textContent).not.toMatch(COUNT);
    expect(home().textContent).not.toMatch(/\b2 (kept|people|person)\b/);
  });

  it("(c) a recheck dropping to nobody replaces the ERROR_COPY line with the nobody line", async () => {
    mount([A]);
    await screen.findByText("Old Name");
    vi.mocked(Events.managedDate).mockResolvedValueOnce(read(A, { keptCount: 0, attendees: [] }));
    focusWindow();
    expect(await screen.findByText(NOBODY_ONE)).toBeInTheDocument();
    expect(document.body.textContent).not.toContain(ERROR_COPY.date_kept_after_leaving);
  });

  it("(d) a counted grant this roster does not list keeps the ERROR_COPY line, never the nobody line", async () => {
    vi.mocked(Events.managedDate).mockResolvedValueOnce(read(A, { keptCount: 1, attendees: [person("Old Name", { keptAfterLeaving: false })] }));
    mount([A]);
    await screen.findByText("Old Name");
    expect(screen.getByRole("alert")).toHaveTextContent(ERROR_COPY.date_kept_after_leaving);
    expect(document.body.textContent).not.toContain("Nobody is keeping");
  });

  it("two dates: only A is read, the save line stays, and nothing carries across dates", async () => {
    const seen = observe();
    mount([A, B]);
    await screen.findByText("Old Name");
    expect(Events.managedDate).toHaveBeenCalledTimes(1);
    expect(screen.getByRole("alert")).toHaveTextContent(SAVE_LINE);
    vi.mocked(Events.managedDate).mockResolvedValueOnce(read(A, { keptCount: 0, attendees: [] }));
    await openRemoveQuestion();
    await userEvent.click(screen.getByRole("button", { name: "Remove" }));
    expect(await screen.findByText(NOBODY_HERE)).toBeInTheDocument();
    expect(screen.getByRole("alert")).toHaveTextContent(SAVE_LINE);
    const holdB = holdRead();
    await userEvent.click(screen.getByRole("button", { name: "Manage sign-ups" }));
    expect(home()).toHaveTextContent(CHECKING);
    await act(async () => { holdB.resolve(read(B, { attendees: [person("Name B")] })); await holdB.promise; });
    const sectionA = within(home()).getByRole("heading", { name: formatDayFull(A) }).parentElement;

    if (!sectionA) throw new Error("missing section");
    expect(sectionA.textContent).toBe(`${formatDayFull(A)}Manage sign-ups`);
    expect(document.body.textContent).not.toContain("Nobody is keeping");
    vi.mocked(Events.managedDate).mockResolvedValueOnce(read(B, { keptCount: 0, attendees: [] }));
    await openRemoveQuestion("Name B");
    await userEvent.click(screen.getByRole("button", { name: "Remove" }));
    expect(await screen.findByText(NOBODY_HERE)).toBeInTheDocument();
    expect(screen.getByRole("alert")).toHaveTextContent(SAVE_LINE);
    seen.stop();

    for (const s of seen.states) {
      expect(s.text).not.toMatch(/these dates now|Save again to apply your change/);
      expect(s.text).not.toContain(ERROR_COPY.date_kept_after_leaving);
    }
  });

  it("a failed refresh after Remove shows the unknown copy and no roster", async () => {
    mount([A]);
    await screen.findByText("Old Name");
    vi.mocked(Events.managedDate).mockRejectedValueOnce(failures[500]());
    await openRemoveQuestion();
    await userEvent.click(screen.getByRole("button", { name: "Remove" }));
    expect(await screen.findByText(UNKNOWN)).toBeInTheDocument();
    expect(forbiddenNow()).toEqual([]);
  });

  it("one window focus with the roster mounted makes exactly one read", async () => {
    mount([A]);
    await screen.findByText("Old Name");
    focusWindow();
    await screen.findByText("Old Name");
    expect(Events.managedDate).toHaveBeenCalledTimes(2);
  });

  it("asks in its own dialog with no outer provider", async () => {
    mount([A]);
    await screen.findByText("Old Name");
    expect(await openRemoveQuestion()).toBeInTheDocument();
    expect(window.confirm).not.toHaveBeenCalled();
  });

  it("with no roster to show, one date says nobody is keeping it and two dates keep the save line", async () => {
    vi.mocked(Events.managedDate).mockResolvedValueOnce(read(A, { attendeesVisible: false, attendees: undefined }));
    mount([A]);
    expect(await screen.findByText(NOBODY_ONE)).toBeInTheDocument();
    expect(screen.queryByRole("region", { name: "Manage sign-ups" })).not.toBeInTheDocument();
  });

  it("with two dates and no roster to show, the selected section says nobody and the save line stays", async () => {
    vi.mocked(Events.managedDate).mockResolvedValueOnce(read(A, { attendeesVisible: false, attendees: undefined }));
    mount([A, B]);
    expect(await screen.findByText(NOBODY_HERE)).toBeInTheDocument();
    expect(screen.getByRole("alert")).toHaveTextContent(SAVE_LINE);
    expect(screen.queryByRole("region", { name: "Manage sign-ups" })).not.toBeInTheDocument();
  });
});

// W4b: every refresh fails closed. Each event runs with the next read held, then settles each way.
type Event = "i" | "ii" | "iii-a" | "iii-b" | "iv" | "v";

const EVENTS: Event[] = ["i", "ii", "iii-a", "iii-b", "iv", "v"];

// Sets up the event's starting state and returns what fires it, and the date its held read is for.
async function arrange(event: Event): Promise<{ fire: () => Promise<void> | void; date: string; mounted: Mounted }> {
  if (event === "ii") {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const until = new Date(Date.now() + 60_000).toISOString();
    vi.mocked(Events.managedDate).mockResolvedValueOnce(read(A, { attendees: [person("Old Name"), kit(until)] }));
  }

  if (event === "iii-a") vi.mocked(Events.managedDate).mockResolvedValueOnce(read(A, { keptCount: 0, attendees: [] }));

  if (event === "iii-b") vi.mocked(Events.managedDate).mockResolvedValueOnce(read(A, { attendeesVisible: false, attendees: undefined }));
  const mounted = mount(event === "iii-b" || event === "v" ? [A, B] : [A]);

  if (event === "iii-a") {
    await screen.findByText(NOBODY_ONE);

    return { mounted, date: A, fire: focusWindow };
  }

  if (event === "iii-b") {
    await screen.findByText(NOBODY_HERE);
    expect(screen.getByRole("button", { name: "Manage sign-ups" })).toBeInTheDocument();

    return { mounted, date: A, fire: focusWindow };
  }

  await screen.findByText("Old Name");
  expect(screen.getByText("Care notes: Old care note")).toBeInTheDocument();
  expect(document.querySelector('a[href^="mailto:old"]')).not.toBeNull();

  if (event === "iv") {
    const write = deferred<unknown>();
    vi.mocked(Events.removeRegistration).mockReturnValueOnce(write.promise);
    await openRemoveQuestion();
    await userEvent.click(screen.getByRole("button", { name: "Remove" }));

    return { mounted, date: A, fire: async () => { await act(async () => { write.resolve({}); await write.promise; }); } };
  }

  await openRemoveQuestion();

  if (event === "i") return { mounted, date: A, fire: focusWindow };

  if (event === "ii") return { mounted, date: A, fire: () => act(() => { vi.advanceTimersByTime(61_000); }) };

  // The page is inert under the open question; the press is dispatched, as a stale tap would be.
  return { mounted, date: B, fire: () => act(() => { fireEvent.click(screen.getByRole("button", { name: "Manage sign-ups" })); }) };
}

describe("W4b: every refresh fails closed", () => {
  for (const event of EVENTS) {
    for (const outcome of OUTCOMES) {
      it(`event (${event}) settled ${outcome}`, async () => {
        const { fire, date, mounted } = await arrange(event);
        const held = holdRead();
        const before = vi.mocked(Events.managedDate).mock.calls.length;
        const seen = observe();
        await fire();
        // Synchronously after the event.
        expect(forbiddenNow()).toEqual([]);
        expect(home()).toHaveTextContent(CHECKING);
        expect(vi.mocked(Events.managedDate).mock.calls.length).toBe(before + 1);
        expect(vi.mocked(Events.managedDate).mock.lastCall).toEqual([7, date]);
        seen.stop();
        seen.expectClean();
        await settle(held, outcome, date);

        if (outcome === "success") {
          expect(await screen.findByText("New Name")).toBeInTheDocument();
          expect(screen.queryByText("Old Name")).not.toBeInTheDocument();
          expect(within(home()).getByRole("heading", { name: formatDayFull(date) })).toBeInTheDocument();

          if (event === "v") {
            const sectionA = within(home()).getByRole("heading", { name: formatDayFull(A) }).parentElement;
            expect(sectionA?.textContent).toBe(`${formatDayFull(A)}Manage sign-ups`);
          }

          expect(mounted.onRefused).not.toHaveBeenCalled();
        } else if (outcome === 403 || outcome === 404) {
          await waitFor(() => expect(mounted.onRefused).toHaveBeenCalledTimes(1));
          expect(home().textContent).toBe(REFUSED);
          const calls = vi.mocked(Events.managedDate).mock.calls.length;
          focusWindow();
          expect(Events.managedDate).toHaveBeenCalledTimes(calls);
        } else {
          expect(home()).toHaveTextContent(UNKNOWN);
          expect(forbiddenNow()).toEqual([]);
          vi.mocked(Events.managedDate).mockResolvedValueOnce(fresh(date));
          await userEvent.click(screen.getByRole("button", { name: "Try again" }));
          expect(await screen.findByText("New Name")).toBeInTheDocument();
          expect(mounted.onRefused).not.toHaveBeenCalled();
        }
      });
    }
  }
});

describe("W4h: date switches and late answers", () => {
  const nameA = () => read(A, { attendees: [person("Name A"), kit()] });

  it("(a) the switch render shows nothing of A, and only B's read is in flight", async () => {
    vi.mocked(Events.managedDate).mockResolvedValueOnce(nameA());
    const { onRefused } = mount([A, B]);
    await screen.findByText("Name A");
    holdRead();
    const seen = observe();
    act(() => { fireEvent.click(screen.getByRole("button", { name: "Manage sign-ups" })); });

    for (const words of ["Name A", "Old care note", "Kept after leaving", SAVE_LINE, ...HEADINGS]) expect(document.body.textContent).not.toContain(words);
    expect(document.querySelector('a[href^="mailto:"]')).toBeNull();
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    seen.stop();
    seen.expectClean(["Name A"]);
    expect(onRefused).not.toHaveBeenCalled();
    expect(vi.mocked(Events.managedDate).mock.calls).toEqual([[7, A], [7, B]]);
  });

  // A switch control renders only while the selected date's read is granted, so no read of A is in flight
  // when B is chosen. The nearest ordering: a window focus and the switch in one act. A's retry never starts
  // a request, and B alone decides, also when it refuses.
  it.each(["answer", "403"] as const)("(b) a focus and a switch to B in one act read only B, which decides (%s)", async (answer) => {
    vi.mocked(Events.managedDate).mockResolvedValueOnce(nameA());
    const { onRefused } = mount([A, B]);
    await screen.findByText("Name A");
    const heldB = holdRead();
    const seen = observe();
    act(() => {
      window.dispatchEvent(new Event("focus"));
      fireEvent.click(screen.getByRole("button", { name: "Manage sign-ups" }));
    });
    expect(vi.mocked(Events.managedDate).mock.calls).toEqual([[7, A], [7, B]]);
    await act(async () => {
      if (answer === "answer") heldB.resolve(read(B, { attendees: [person("Name B")] }));
      else heldB.reject(failures[403]());
      await heldB.promise.catch(() => {});
    });
    seen.stop();

    for (const s of seen.states) expect(s.text).not.toContain("Name A");

    if (answer === "answer") {
      expect(within(home()).getByRole("heading", { name: formatDayFull(B) }).parentElement).toHaveTextContent("Name B");
      expect(onRefused).not.toHaveBeenCalled();
    } else {
      await waitFor(() => expect(onRefused).toHaveBeenCalledTimes(1));
      expect(home().textContent).toBe(REFUSED);
    }
  });

  it("(c) A → B → A: the second read of A decides, and nothing of B stays", async () => {
    const { onRefused } = mount([A, B]);
    await screen.findByText("Old Name");
    vi.mocked(Events.managedDate).mockResolvedValueOnce(read(B, { attendees: [person("Name B")] }));
    await userEvent.click(screen.getByRole("button", { name: "Manage sign-ups" }));
    await screen.findByText("Name B");
    const heldA = holdRead();
    const seen = observe();
    act(() => { fireEvent.click(screen.getByRole("button", { name: "Manage sign-ups" })); });
    expect(document.body.textContent).not.toContain("Name B");
    await act(async () => { heldA.resolve(read(A, { attendees: [person("Name A2")] })); await heldA.promise; });
    seen.stop();

    for (const s of seen.states.slice(0, -1)) expect(s.text).not.toContain("Name B");
    expect(within(home()).getByRole("heading", { name: formatDayFull(A) }).parentElement).toHaveTextContent("Name A2");
    expect(document.body.textContent).not.toContain("Name B");
    expect(onRefused).not.toHaveBeenCalled();
  });
});

// W4i: writes fail closed. The roster has a kept row, a waitlisted row, a removed row and the Add field.
type Write = "remove" | "promote" | "restore" | "add";

const WRITES: Write[] = ["remove", "promote", "restore", "add"];
const writeApi = { remove: () => Events.removeRegistration, promote: () => Events.promoteRegistration, restore: () => Events.restoreRegistration, add: () => Events.addRegistration };

// Holds the next request of `write`, each of which answers api<unknown>.
function holdWrite(write: Write) {
  const held = deferred<unknown>();
  vi.mocked(writeApi[write]()).mockReturnValueOnce(held.promise);

  return held;
}

const full = (date = A) => read(date, { attendees: [person("Old Name"), kit(), waiting], removedRegistrations: [gone] });

async function startWrite(write: Write) {
  if (write === "remove") {
    await openRemoveQuestion();
    await userEvent.click(screen.getByRole("button", { name: "Remove" }));
  } else if (write === "promote") {
    await userEvent.click(screen.getByRole("button", { name: "Promote" }));
  } else if (write === "restore") {
    await userEvent.click(screen.getByRole("button", { name: "Add back Gone Person" }));
  } else {
    await userEvent.type(screen.getByRole("textbox", { name: /^Add someone/ }), "new@example.com");
    await userEvent.click(screen.getByRole("button", { name: "Add" }));
  }
}

type Ending = "success" | 401 | 403 | 404 | "network" | 500;

const ENDINGS: Ending[] = ["success", 401, 403, 404, "network", 500];
const endingError = (ending: Exclude<Ending, "success">) => (ending === 401 ? new ApiError(401, "SECRET", { code: "unauthorized" }) : failures[ending]());

describe("W4i: writes fail closed", () => {
  for (const write of WRITES) {
    for (const ending of ENDINGS) {
      for (const outcome of OUTCOMES) {
        it(`${write} ending ${ending}, read settled ${outcome}`, async () => {
          vi.mocked(Events.managedDate).mockResolvedValueOnce(full());
          const { onRefused } = mount([A]);
          await screen.findByText("Old Name");
          const api = writeApi[write]();
          const held = holdWrite(write);
          await startWrite(write);
          expect(api).toHaveBeenCalledTimes(1);
          const next = holdRead();
          const before = vi.mocked(Events.managedDate).mock.calls.length;
          const seen = observe();
          await act(async () => {
            if (ending === "success") held.resolve({});
            else held.reject(endingError(ending));
            await held.promise.catch(() => {});
          });
          expect(forbiddenNow()).toEqual([]);
          expect(home()).toHaveTextContent(CHECKING);
          expect(Events.managedDate).toHaveBeenCalledTimes(before + 1);
          seen.stop();
          seen.expectClean();
          await settle(next, outcome);

          if (outcome === "success") {
            expect(await screen.findByText("New Name")).toBeInTheDocument();

            if (ending === "success") expect(screen.queryByText(WRITE_NOTE)).not.toBeInTheDocument();
            else expect(screen.getByText(WRITE_NOTE)).toBeInTheDocument();
          } else if (outcome === 403 || outcome === 404) {
            await waitFor(() => expect(onRefused).toHaveBeenCalledTimes(1));
            expect(home().textContent).toBe(REFUSED);
          } else {
            expect(home()).toHaveTextContent(UNKNOWN);
            expect(forbiddenNow()).toEqual([]);
          }

          expect(api).toHaveBeenCalledTimes(1);
        });
      }
    }
  }

  it.each(["restore", "add"] as const)("%s answered event_full, its over-capacity question declined, starts no read", async (write) => {
    vi.mocked(Events.managedDate).mockResolvedValueOnce(full());
    mount([A]);
    await screen.findByText("Old Name");
    vi.mocked(writeApi[write]()).mockRejectedValueOnce(new ApiError(409, "SECRET", { code: "event_full" }));
    await startWrite(write);
    const question = await screen.findByRole("dialog", { name: write === "restore" ? "Add Gone Person over capacity?" : "Add them over capacity?" });
    await userEvent.click(within(question).getByRole("button", { name: "Cancel" }));
    expect(await screen.findByText(errorText(new ApiError(409, "SECRET", { code: "event_full" })))).toBeInTheDocument();
    expect(screen.getByText("Old Name")).toBeInTheDocument();
    expect(Events.managedDate).toHaveBeenCalledTimes(1);
    expect(writeApi[write]()).toHaveBeenCalledTimes(1);
  });

  it("an add that needs the person's approval starts no read and keeps Ask to add", async () => {
    vi.mocked(Events.managedDate).mockResolvedValueOnce(full());
    vi.mocked(People.search).mockResolvedValue({ state: "results", people: [{ selectionId: "adult-opaque", displayName: "Pat", teen: false, sharedGroups: [], otherGroups: [] }] });
    vi.mocked(Events.addRegistration).mockRejectedValueOnce(new ApiError(409, "SECRET", { code: "date_add_approval_required" }));
    mount([A]);
    await screen.findByText("Old Name");
    await userEvent.type(screen.getByRole("combobox", { name: "Find a person by name" }), "Pat");
    await userEvent.click(await screen.findByRole("option", { name: /Pat/ }));
    await userEvent.click(screen.getByRole("button", { name: "Add" }));
    expect(await screen.findByRole("button", { name: "Ask to add" })).toBeEnabled();
    expect(screen.getByText(/They need to approve this date/)).toBeInTheDocument();
    expect(Events.managedDate).toHaveBeenCalledTimes(1);
  });
});

describe("W4j: a recheck during a write", () => {
  const WRITE_CONTROLS = ["Promote", "More for Old Name", "Add back Gone Person", "Add person", "Close add person", "Add"];

  for (const write of WRITES) {
    for (const recheck of ["focus", "care"] as const) {
      for (const ending of ["success", 403, 500] as const) {
        it(`(a) ${write} held, a ${recheck} recheck answered first, then the write ends ${ending}`, async () => {
          if (recheck === "care") vi.useFakeTimers({ shouldAdvanceTime: true });
          const until = recheck === "care" ? new Date(Date.now() + 60_000).toISOString() : undefined;
          vi.mocked(Events.managedDate).mockResolvedValueOnce(read(A, { attendees: [person("Old Name"), kit(until), waiting], removedRegistrations: [gone] }));
          const { onRefused } = mount([A]);
          await screen.findByText("Old Name");
          const api = writeApi[write]();
          const held = holdWrite(write);
          await startWrite(write);
          const recheckRead = holdRead();
          const seen = observe();

          if (recheck === "focus") focusWindow();
          else act(() => { vi.advanceTimersByTime(61_000); });
          // The recheck's read answers with the roster as it was before the write.
          await act(async () => { recheckRead.resolve(full()); await recheckRead.promise; });
          expect(home()).toHaveTextContent(CHECKING);

          for (const name of WRITE_CONTROLS) expect(within(home()).queryByRole("button", { name })).not.toBeInTheDocument();
          expect(api).toHaveBeenCalledTimes(1);
          const before = vi.mocked(Events.managedDate).mock.calls.length;
          const after = holdRead();
          await act(async () => {
            if (ending === "success") held.resolve({});
            else held.reject(endingError(ending));
            await held.promise.catch(() => {});
          });
          expect(Events.managedDate).toHaveBeenCalledTimes(before + 1);
          expect(home()).toHaveTextContent(CHECKING);
          seen.stop();
          seen.expectClean();
          await act(async () => { after.resolve(fresh(A)); await after.promise; });
          expect(screen.getByText("New Name")).toBeInTheDocument();

          if (ending === "success") expect(screen.queryByText(WRITE_NOTE)).not.toBeInTheDocument();
          else expect(screen.getByText(WRITE_NOTE)).toBeInTheDocument();
          expect(onRefused).not.toHaveBeenCalled();
        });
      }
    }
  }

  it("(a) a read after the write that answers 403 refuses the panel once", async () => {
    const { onRefused } = mount([A]);
    await screen.findByText("Old Name");
    const held = deferred<unknown>();
    vi.mocked(Events.removeRegistration).mockReturnValueOnce(held.promise);
    await startWrite("remove");
    vi.mocked(Events.managedDate).mockResolvedValueOnce(read(A));
    focusWindow();
    await waitFor(() => expect(Events.managedDate).toHaveBeenCalledTimes(2));
    const after = holdRead();
    await act(async () => { held.resolve({}); await held.promise; });
    await settle(after, 403);
    expect(home().textContent).toBe(REFUSED);
    expect(onRefused).toHaveBeenCalledTimes(1);
  });

  it("(b) A → B → A with A's Remove held: A stays checking until the Remove ends and a later read answers", async () => {
    mount([A, B]);
    await screen.findByText("Old Name");
    const held = deferred<unknown>();
    vi.mocked(Events.removeRegistration).mockReturnValueOnce(held.promise);
    await startWrite("remove");
    vi.mocked(Events.managedDate).mockResolvedValueOnce(read(B, { attendees: [person("Name B")] }));
    act(() => { fireEvent.click(screen.getByRole("button", { name: "Manage sign-ups" })); });
    expect(await screen.findByText("Name B")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "More for Name B" })).toBeEnabled();
    vi.mocked(Events.managedDate).mockResolvedValueOnce(read(A));
    await userEvent.click(screen.getByRole("button", { name: "Manage sign-ups" }));
    await waitFor(() => expect(Events.managedDate).toHaveBeenCalledTimes(3));
    await act(async () => {});
    expect(home()).toHaveTextContent(CHECKING);
    expect(screen.queryByText("Old Name")).not.toBeInTheDocument();
    const after = holdRead();
    await act(async () => { held.resolve({}); await held.promise; });
    expect(Events.managedDate).toHaveBeenCalledTimes(4);
    expect(home()).toHaveTextContent(CHECKING);
    await act(async () => { after.resolve(fresh(A)); await after.promise; });
    expect(screen.getByText("New Name")).toBeInTheDocument();
  });

  it("(c) a focus during restore's over-capacity question closes it as no and ends the write answered", async () => {
    vi.mocked(Events.managedDate).mockResolvedValueOnce(full());
    const { store } = mount([A]);
    await screen.findByText("Old Name");
    vi.mocked(Events.restoreRegistration).mockRejectedValueOnce(new ApiError(409, "SECRET", { code: "event_full" }));
    await startWrite("restore");
    await screen.findByRole("dialog", { name: "Add Gone Person over capacity?" });
    expect(store.pending(`7:${A}`)).toBe(1);
    vi.mocked(Events.managedDate).mockResolvedValueOnce(full());
    focusWindow();
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(await screen.findByText("Old Name")).toBeInTheDocument();
    expect(store.pending(`7:${A}`)).toBe(0);
    expect(Events.restoreRegistration).toHaveBeenCalledTimes(1);
    expect(Events.managedDate).toHaveBeenCalledTimes(2);
    expect(screen.queryByText(WRITE_NOTE)).not.toBeInTheDocument();
  });
});

describe("W4k: keyboard focus", () => {
  async function tabTo(name: string) {
    for (let i = 0; i < 40; i++) {
      await userEvent.tab();

      if (document.activeElement?.textContent === name && document.activeElement.tagName === "BUTTON") return;
    }

    throw new Error(`never reached ${name}`);
  }

  async function switchToB() {
    mount([A, B]);
    await screen.findByText("Old Name");
    await tabTo("Manage sign-ups");
    const heldB = holdRead();
    const seen = observe();
    await userEvent.keyboard("{Enter}");

    return { heldB, seen };
  }

  it("(a) the switch keeps focus on the panel's home while B is held", async () => {
    const { seen } = await switchToB();
    expect(document.activeElement).toBe(home());
    seen.stop();

    for (const s of seen.states) expect(s.active).toBe(home());
  });

  it.each([500, "network"] as const)("(b) B failing (%s) keeps focus on the home, and Tab reaches Try again", async (kind) => {
    const { heldB, seen } = await switchToB();
    await settle(heldB, kind, B);
    seen.stop();

    for (const s of seen.states) expect(s.active).not.toBe(document.body);
    expect(document.activeElement).toBe(home());
    await userEvent.tab();
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Try again" }));
  });

  it("(c) B answering keeps focus on the home, and Tab reaches B's first roster control", async () => {
    const { heldB, seen } = await switchToB();
    await settle(heldB, "success", B);
    seen.stop();

    for (const s of seen.states) expect(s.active).not.toBe(document.body);
    expect(document.activeElement).toBe(home());
    await userEvent.tab();
    expect(screen.getByRole("region", { name: "Manage sign-ups" }).contains(document.activeElement)).toBe(true);
  });

  // The date editor over its own read, as a host with a kept-panel recheck of its own (ManageDate) mounts it.
  function Editor({ onAuthorityLost }: { onAuthorityLost: () => void }) {
    const read = useEditRead({ eventId: 7, date: A, occurrenceId: 1 }, "panel", true, () => {});

    return read.kind === "ready" ? <DateEditor opener={{ eventId: 7, date: A, occurrenceId: 1 }} base={read.base} onSaved={() => {}} onConflict={() => {}} onClose={() => {}} onAuthorityLost={onAuthorityLost} /> : null;
  }

  it("(d) B refusing closes the panel and focuses the editor's Save; with focus elsewhere it stays there", async () => {
    await verifiedCredential();
    const edits = stubEditSends();
    // The editor's own read is pinned to the credential; the panel's reads are not.
    vi.mocked(Events.managedDate).mockImplementation((_id, date, pinned) => Promise.resolve(pinned ? { occurrence: occurrence({ revision: "r0", attendeesVisible: true }) } : read(date)));

    for (const inside of [true, false]) {
      edits.answer(new ApiError(409, "SECRET", { code: "date_kept_after_leaving", keptDates: [{ date: A, keptCount: 1 }, { date: B, keptCount: 1 }] }));
      const onAuthorityLost = vi.fn();
      const view = render(<MemoryRouter><RosterWritesProvider><Editor onAuthorityLost={onAuthorityLost} /></RosterWritesProvider></MemoryRouter>);
      const minutes = await screen.findByRole("spinbutton", { name: "Minutes" });
      await waitFor(() => expect(screen.getByRole("button", { name: "Save this date" })).toBeEnabled());
      await userEvent.clear(minutes);
      await userEvent.type(minutes, "150");
      await userEvent.click(screen.getByRole("button", { name: "Save this date" }));
      await screen.findByText("Old Name");

      if (inside) {
        await tabTo("Manage sign-ups");
      } else {
        minutes.focus();
      }

      const heldB = holdRead();

      if (inside) await userEvent.keyboard("{Enter}");
      else act(() => { fireEvent.click(screen.getByRole("button", { name: "Manage sign-ups" })); minutes.focus(); });
      await settle(heldB, 403, B);
      await waitFor(() => expect(onAuthorityLost).toHaveBeenCalledTimes(1));
      expect(screen.queryByRole("region", { name: "Sign-ups to check before you save" })).not.toBeInTheDocument();
      expect(screen.getByRole("alert")).toHaveTextContent(REFUSED);
      expect(document.activeElement).toBe(inside ? screen.getByRole("button", { name: "Save this date" }) : minutes);
      expect(minutes).toHaveValue(150);
      view.unmount();
    }
  });

  it("(e) a care boundary, a window focus and a confirmed Remove each leave focus on the home", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const until = new Date(Date.now() + 60_000).toISOString();
    const first = holdRead();
    mount([A]);
    await act(async () => { first.resolve(read(A, { attendees: [person("Old Name"), kit(until)] })); await first.promise; });
    const more = () => screen.getByRole("button", { name: "More for Old Name" });

    more().focus();
    act(() => { vi.advanceTimersByTime(61_000); });
    expect(document.activeElement).toBe(home());
    await screen.findByText("Old Name");

    more().focus();
    focusWindow();
    expect(document.activeElement).toBe(home());
    await screen.findByText("Old Name");

    more().focus();
    await userEvent.keyboard("{Enter}");
    await userEvent.keyboard("{Enter}");
    const question = await screen.findByRole("dialog");
    within(question).getByRole("button", { name: "Remove" }).focus();
    const next = holdRead();
    await userEvent.keyboard("{Enter}");
    await waitFor(() => expect(Events.managedDate).toHaveBeenCalledTimes(4));
    await waitFor(() => expect(document.activeElement).toBe(home()));
    await act(async () => { next.resolve(fresh(A)); await next.promise; });
    expect(document.activeElement).not.toBe(document.body);
  });
});

describe("W4l: the panel's questions stay in its own provider", () => {
  interface Probe { ask: () => Promise<boolean> }

  function PageFacts() {
    usePageFacts("constant");

    return null;
  }

  function PageProbe({ out }: { out: RefObject<Probe | null> }) {
    const begin = useConfirm();
    const latest = useRef(begin);
    latest.current = begin;
    out.current = { ask: () => latest.current().ask({ title: "A page question", confirmLabel: "Yes", tone: "normal", native: "A page question" }) };

    return null;
  }

  it("keeps an outer question open while the panel reads, rechecks and refreshes after a write", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const until = new Date(Date.now() + 60_000).toISOString();
    const probe = createRef<Probe>();
    const first = holdRead();
    const { store } = mount([A], (panel) => <ConfirmProvider><PageFacts /><PageProbe out={probe} />{panel}</ConfirmProvider>);
    let asked: Promise<boolean> = Promise.resolve(false);
    act(() => { asked = probe.current?.ask() ?? asked; });
    expect(await screen.findByRole("dialog", { name: "A page question" })).toBeInTheDocument();
    await act(async () => { first.resolve(read(A, { attendees: [person("Old Name"), kit(until)] })); await first.promise; });
    await screen.findByText("Old Name");
    vi.mocked(Events.managedDate).mockResolvedValueOnce(read(A, { title: "Changed one" }));
    act(() => { vi.advanceTimersByTime(61_000); });
    await waitFor(() => expect(Events.managedDate).toHaveBeenCalledTimes(2));
    vi.mocked(Events.managedDate).mockResolvedValueOnce(read(A, { title: "Changed two" }));
    focusWindow();
    await waitFor(() => expect(Events.managedDate).toHaveBeenCalledTimes(3));
    vi.mocked(Events.managedDate).mockResolvedValueOnce(read(A, { title: "Changed three" }));
    await act(async () => { store.begin(`7:${A}`)("stored"); });
    await waitFor(() => expect(Events.managedDate).toHaveBeenCalledTimes(4));
    await screen.findByText("Old Name");
    expect(screen.getByRole("dialog", { name: "A page question" })).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Yes" }));
    await expect(asked).resolves.toBe(true);
  });
});
