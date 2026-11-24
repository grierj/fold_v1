import { createRosterWrites, RosterWritesProvider, type RosterWriteEnd, type RosterWrites } from "../lib/rosterWrites";
import { renderWithAuth } from "../test/auth";
import { stubService } from "../test/service";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createRef, useLayoutEffect, useRef, type RefObject } from "react";
import { MemoryRouter, Route, Routes, useLocation, useNavigate, type NavigateFunction } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError, type Credential } from "../api/core";
import { Events } from "../api/events";
import { DateRooms } from "../api/rooms";
import { Site } from "../api/site";
import type { ManageAuthority } from "../lib/manageAuthority";
import { invalidateSite } from "../lib/useSite";
import { occurrence } from "../test/occurrence";
import type { Attendee, EventOccurrence, OccurrenceResponse } from "../types";
import { ConfirmProvider, useConfirm } from "./ui/ConfirmDialog";
import { ManageDate } from "./ManageDate";
import { KEPT, kept409, keptForbiddenNow, keptRead, observeKept } from "../test/keptPanel";
import { endsFor, removedRow, ROSTER_ENDS, ROSTER_OPS, startHeld } from "../test/rosterOps";
import { stubEditSends, verifiedCredential, type EditSends } from "../test/guardedEdits";
import { setMarkerCookie } from "../test/credential";
import { erasureCases, SignedInSession } from "../test/session";
import { ERROR_COPY } from "../lib/errors";

const actor = { user: { id: 10, status: "active" as const, accountType: "adult" as const } };
const render = renderWithAuth(() => actor);

stubService(Events, () => ({ managedDate: vi.fn(), dateVisibilityImpact: vi.fn(), removeRegistration: vi.fn(), promoteRegistration: vi.fn(), restoreRegistration: vi.fn(), addRegistration: vi.fn() }));

stubService(Site, () => ({ get: vi.fn() }));

stubService(DateRooms, () => ({ list: vi.fn(), set: vi.fn() }));

const DATE = "2026-12-01";
const KEY = `7:${DATE}`;
const oldName: Attendee = { registrationId: 31, userId: 5, name: "Old Name", status: "going", attendanceMode: "in_person", guestCount: 0, email: "old@example.com" };
const managed = (over: Partial<EventOccurrence> = {}): EventOccurrence => occurrence({ revision: "r0", attendeesVisible: true, attendees: [oldName], visibility: "public", seriesVisibility: "public", groupId: 4, groupVisibility: "public", groupName: "Choir", ...over });
const answer = (over: Partial<EventOccurrence> = {}): OccurrenceResponse => ({ occurrence: managed(over) });

function deferred<T>() {
  let resolve: (value: T) => void = () => {};
  let reject: (cause: unknown) => void = () => {};
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });

  return { promise, resolve, reject };
}

let credential: Credential;
let edits: EditSends;

beforeEach(async () => {
  vi.resetAllMocks();
  invalidateSite();
  vi.mocked(Site.get).mockResolvedValue({ name: "Fold", timezone: "UTC", selfSignup: "off", roomsEnabled: false, pushEnabled: true });
  vi.mocked(Events.managedDate).mockResolvedValue(answer());
  vi.spyOn(window, "confirm").mockImplementation(() => { throw new Error("native confirm on the date page"); });
  credential = await verifiedCredential();
  edits = stubEditSends();
});

// The writer's question for a tightening of this date that cancels `registrants` people (DEC-158).
const challenge = (registrants: number, consent = "c".repeat(32)) => new ApiError(409, "x", { code: "date_needs_consent", impact: { visibility: "private", currentVisibility: "public", tightened: [{ id: 7, title: "Supper", visibility: "public", registrants, occurrenceId: 1 }], leaderOpened: [], consent } });

// What EventDate hands ManageDate, as spies.
const pageCallbacks = () => ({ onDateChanged: vi.fn(), onConflict: vi.fn(), onOpenEditor: vi.fn(), onGone: vi.fn(), onRetryPage: vi.fn() });

// A question asked under the page's own provider, as EventDate's own actions ask.
interface Probe { ask: () => Promise<boolean> }

function PageProbe({ out }: { out: RefObject<Probe | null> }) {
  const begin = useConfirm();
  const latest = useRef(begin);
  latest.current = begin;
  out.current = { ask: () => latest.current().ask({ title: "A page question", confirmLabel: "Yes", tone: "normal", native: "A page question" }) };

  return null;
}

interface Mounted {
  store: RosterWrites;
  probe: RefObject<Probe | null>;
  callbacks: { onDateChanged: ReturnType<typeof vi.fn>; onConflict: ReturnType<typeof vi.fn>; onOpenEditor: ReturnType<typeof vi.fn>; onGone: ReturnType<typeof vi.fn>; onRetryPage: ReturnType<typeof vi.fn> };
  setAuthority: (authority: ManageAuthority) => void;
}

// ManageDate in EventDate's shape: an outer provider with no usePageFacts, inside the session's providers.
function mount(authority: ManageAuthority = "granted"): Mounted {
  const store = createRosterWrites();
  const probe = createRef<Probe>();
  const callbacks = pageCallbacks();

  const ui = (a: ManageAuthority) => <MemoryRouter><RosterWritesProvider rosterWrites={store}><ConfirmProvider>
    <PageProbe out={probe} />
    <ManageDate eventId={7} date={DATE} occurrenceId={1} authority={a} {...callbacks} />
  </ConfirmProvider></RosterWritesProvider></MemoryRouter>;

  const view = render(ui(authority));

  return { store, probe, callbacks, setAuthority: (a) => view.rerender(ui(a)) };
}

async function openEditor() {
  await userEvent.click(screen.getByRole("button", { name: "Edit this date" }));

  return screen.findByRole("spinbutton", { name: "Minutes" });
}

async function openRoster() {
  await userEvent.click(screen.getByRole("button", { name: "Manage sign-ups" }));
  await screen.findByText("Old Name");
}

async function typeMinutes(value: string) {
  const minutes = await openEditor();
  await userEvent.clear(minutes);
  await userEvent.type(minutes, value);
}

it("loads the managed read only while a tool is open, and never while authority is checked or unknown", async () => {
  const { setAuthority } = mount("checking");
  expect(screen.getByRole("status")).toHaveTextContent("Checking that you can still manage this date…");
  expect(screen.queryByRole("button", { name: "Edit this date" })).not.toBeInTheDocument();
  setAuthority("unknown");
  expect(screen.getByText("We couldn't check that you can still manage this date. What you typed is kept.")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Try again" })).toBeInTheDocument();
  setAuthority("granted");
  expect(Events.managedDate).not.toHaveBeenCalled();
  await openRoster();
  expect(Events.managedDate).toHaveBeenCalledTimes(1);
  await userEvent.click(screen.getByRole("button", { name: "Manage sign-ups" }));
  expect(screen.queryByText("Old Name")).not.toBeInTheDocument();
  setAuthority("checking");
  setAuthority("unknown");
  expect(screen.queryByRole("spinbutton", { name: "Minutes" })).not.toBeInTheDocument();
  expect(Events.managedDate).toHaveBeenCalledTimes(1);
});

it("asks in its own dialog, and a roster Remove retries only the managed read through the gate", async () => {
  const { callbacks } = mount();
  await openRoster();
  const write = deferred<unknown>();
  vi.mocked(Events.removeRegistration).mockReturnValue(write.promise);
  await userEvent.click(screen.getByRole("button", { name: "More for Old Name" }));
  await userEvent.click(screen.getByRole("menuitem", { name: "Remove Old Name" }));
  await userEvent.click(screen.getByRole("button", { name: "Remove" }));
  const next = deferred<OccurrenceResponse>();
  vi.mocked(Events.managedDate).mockReturnValue(next.promise);
  await act(async () => { write.resolve({}); await write.promise; });
  // The render the gate's retry starts already has no roster.
  expect(screen.queryByText("Old Name")).not.toBeInTheDocument();
  expect(Events.managedDate).toHaveBeenCalledTimes(2);
  await act(async () => { next.resolve(answer({ attendees: [{ ...oldName, name: "New Name" }] })); await next.promise; });
  expect(await screen.findByText("New Name")).toBeInTheDocument();
  expect(callbacks.onDateChanged).not.toHaveBeenCalled();
  expect(callbacks.onGone).not.toHaveBeenCalled();
  expect(callbacks.onRetryPage).not.toHaveBeenCalled();
});

it("with the roster open, a save empties the roster in the render it starts, closes the editor and reads the page again", async () => {
  const { callbacks } = mount();
  await typeMinutes("150");
  await openRoster();
  const after = deferred<OccurrenceResponse>();
  vi.mocked(Events.managedDate).mockReturnValue(after.promise);
  await userEvent.click(screen.getByRole("button", { name: "Save this date" }));
  await waitFor(() => expect(edits.sent).toHaveLength(1));
  expect(edits.sent[0].changes).toEqual({ durationMinutes: 150 });
  await waitFor(() => expect(screen.queryByText("Old Name")).not.toBeInTheDocument());
  expect(screen.queryByRole("spinbutton", { name: "Minutes" })).not.toBeInTheDocument();
  expect(callbacks.onDateChanged).toHaveBeenCalledOnce();
  await act(async () => { after.resolve(answer()); await after.promise; });
  expect(await screen.findByText("Old Name")).toBeInTheDocument();
  expect(screen.queryByRole("spinbutton", { name: "Minutes" })).not.toBeInTheDocument();
});

it("with the roster open, a room change empties the roster in the render it starts", async () => {
  invalidateSite();
  vi.mocked(Site.get).mockResolvedValue({ name: "Fold", timezone: "UTC", selfSignup: "off", roomsEnabled: true, pushEnabled: true });
  vi.mocked(DateRooms.list).mockResolvedValue({ rooms: [{ id: 1, name: "Fellowship Hall", building: "Main", capacity: 80, features: "", restricted: false, busy: false, tooSmall: false }], booking: null });
  vi.mocked(DateRooms.set).mockResolvedValue({ booking: null });
  mount();
  await openEditor();
  await openRoster();
  const next = deferred<OccurrenceResponse>();
  vi.mocked(Events.managedDate).mockReturnValue(next.promise);
  await userEvent.selectOptions(await screen.findByRole("combobox", { name: /^Room/ }), "1");
  await waitFor(() => expect(DateRooms.set).toHaveBeenCalled());
  await waitFor(() => expect(screen.queryByText("Old Name")).not.toBeInTheDocument());
  expect(Events.managedDate).toHaveBeenCalledTimes(2);
});

it("(p) an authority recheck closes the writer's question as no and brings the draft back", async () => {
  edits.answer(challenge(2));
  const { setAuthority } = mount();
  await openEditor();
  await userEvent.click(screen.getByRole("radio", { name: /^Private/ }));
  await userEvent.click(screen.getByRole("button", { name: "Save this date" }));
  expect(await screen.findByRole("dialog", { name: "Save this date?" })).toBeInTheDocument();
  setAuthority("checking");
  await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  setAuthority("granted");
  expect(await screen.findByRole("radio", { name: /^Private/ })).toBeChecked();
  expect(edits.sent).toHaveLength(1);
  // The next save is a new press, without the old token.
  await waitFor(() => expect(screen.getByRole("button", { name: "Save this date" })).toBeEnabled());
  await userEvent.click(screen.getByRole("button", { name: "Save this date" }));
  await waitFor(() => expect(edits.sent).toHaveLength(2));
  expect(edits.sent[1].consent).toBeUndefined();
  expect(edits.sent[1].attempt).not.toBe(edits.sent[0].attempt);
});

it("(p) Save and notify adds only the writer's token to the same save, once", async () => {
  const token = "d".repeat(32);
  edits.answer(challenge(2, token));
  mount();
  await openEditor();
  await userEvent.click(screen.getByRole("radio", { name: /^Private/ }));
  await userEvent.click(screen.getByRole("button", { name: "Save this date" }));
  const asked = await screen.findByRole("dialog", { name: "Save this date?" });
  expect(asked).toHaveTextContent("This cancels and notifies 2 registered people");
  await userEvent.click(within(asked).getByRole("button", { name: "Save and notify" }));
  await waitFor(() => expect(edits.sent).toHaveLength(2));
  expect(edits.sent[1]).toMatchObject({ revision: edits.sent[0].revision, changes: { visibility: "private" }, consent: token });
  expect(Events.dateVisibilityImpact).not.toHaveBeenCalled();
});

it("(q) leaves the page's own question open while the managed answer changes", async () => {
  const { store, probe, setAuthority } = mount();
  await openEditor();
  await openRoster();
  let asked: Promise<boolean> = Promise.resolve(false);
  act(() => { asked = probe.current?.ask() ?? asked; });
  expect(await screen.findByRole("dialog", { name: "A page question" })).toBeInTheDocument();
  vi.mocked(Events.managedDate).mockResolvedValue(answer({ title: "Changed one" }));
  await act(async () => { store.begin(KEY)("stored"); });
  await waitFor(() => expect(Events.managedDate).toHaveBeenCalledTimes(2));
  setAuthority("checking");
  vi.mocked(Events.managedDate).mockResolvedValue(answer({ title: "Changed three" }));
  setAuthority("granted");
  await waitFor(() => expect(Events.managedDate).toHaveBeenCalledTimes(3));
  expect(screen.getByRole("dialog", { name: "A page question" })).toBeInTheDocument();
  await userEvent.click(screen.getByRole("button", { name: "Yes" }));
  await expect(asked).resolves.toBe(true);
});

it("(r) keeps keyboard focus on the section through a recheck, a care boundary, a write's retry and a closed gate", async () => {
  const { store, setAuthority } = mount();
  await openRoster();
  const section = screen.getByRole("region", { name: "Manage this date" });
  const more = () => screen.getByRole("button", { name: "More for Old Name" });

  // A window focus: the page reads authority again (checking) and the roster asks for a recheck.
  more().focus();
  const recheck = deferred<OccurrenceResponse>();
  vi.mocked(Events.managedDate).mockReturnValue(recheck.promise);
  act(() => { window.dispatchEvent(new Event("focus")); setAuthority("checking"); });
  expect(document.activeElement).toBe(section);
  setAuthority("granted");
  expect(document.activeElement).toBe(section);
  await act(async () => { recheck.resolve(answer()); await recheck.promise; });
  await screen.findByText("Old Name");
  expect(document.activeElement).toBe(section);
  await userEvent.tab();
  expect(document.activeElement).toBe(screen.getByRole("link", { name: "Open the event page" }));

  // A write's retry, and the gate closed by a recheck during a write.
  more().focus();
  vi.mocked(Events.managedDate).mockReturnValue(deferred<OccurrenceResponse>().promise);
  await act(async () => { store.begin(KEY)("stored"); });
  expect(document.activeElement).toBe(section);
});

it("(r) keeps keyboard focus on the section at a care boundary", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });

  try {
    const soon = new Date(Date.now() + 60_000).toISOString();
    vi.mocked(Events.managedDate).mockResolvedValue(answer({ childcareAvailable: true, attendees: [oldName, { registrationId: 40, name: "Kit", status: "going", attendanceMode: "in_person", guestCount: 0, householdPerson: { personId: 3, name: "Kit", kind: "child", age: 6, householdName: "H", registeredBy: { userId: 7, name: "Holder" }, careNotes: "Old care note", careNotesVisibleUntil: soon } }] }));
    mount();
    await openRoster();
    expect(screen.getByText(/Old care note/)).toBeInTheDocument();
    const section = screen.getByRole("region", { name: "Manage this date" });
    screen.getByRole("button", { name: "More for Old Name" }).focus();
    vi.mocked(Events.managedDate).mockReturnValue(deferred<OccurrenceResponse>().promise);
    act(() => { vi.advanceTimersByTime(61_000); });
    expect(document.body.textContent).not.toContain("Old care note");
    expect(document.body.textContent).not.toContain("Old Name");
    expect(document.activeElement).toBe(section);
  } finally { vi.useRealTimers(); }
});

// W6: the kept-dates panel inside the date page's editor.
describe("ManageDate and the kept-dates panel (DEC-156)", () => {
  const panel = () => screen.queryByRole("region", { name: KEPT.home });

  async function refuseSave() {
    vi.mocked(Events.managedDate).mockImplementation(() => Promise.resolve(keptRead(DATE)));
    edits.answer(kept409([{ date: DATE, keptCount: 1 }]));
    const mounted = mount();
    await typeMinutes("150");
    await userEvent.click(screen.getByRole("button", { name: "Save this date" }));
    await screen.findByText("Old Name");

    return mounted;
  }

  it("a focus refresh hides the editor and the panel while pending, then reopens the panel checking with the draft", async () => {
    const { setAuthority } = await refuseSave();
    await userEvent.click(screen.getByRole("button", { name: "More for Old Name" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Remove Old Name" }));
    await screen.findByRole("dialog");
    const seen = observeKept([DATE]);
    act(() => { window.dispatchEvent(new Event("focus")); setAuthority("checking"); });
    expect(keptForbiddenNow([DATE])).toEqual([]);
    expect(screen.queryByRole("spinbutton", { name: "Minutes" })).not.toBeInTheDocument();
    const page = deferred<OccurrenceResponse>();
    const panelRead = deferred<OccurrenceResponse>();
    vi.mocked(Events.managedDate).mockReturnValueOnce(page.promise).mockReturnValueOnce(panelRead.promise);
    setAuthority("granted");
    expect(keptForbiddenNow([DATE])).toEqual([]);
    await act(async () => { page.resolve(keptRead(DATE)); await page.promise; });
    expect(await screen.findByRole("spinbutton", { name: "Minutes" })).toHaveValue(150);
    await waitFor(() => expect(panel()).toHaveTextContent(KEPT.checking));
    seen.stop();
    seen.expectClean();
    await act(async () => { panelRead.resolve(keptRead(DATE, { attendees: [{ ...oldName, name: "New Name", keptAfterLeaving: true }] })); await panelRead.promise; });
    expect(await screen.findByText("New Name")).toBeInTheDocument();
    expect(screen.getByRole("spinbutton", { name: "Minutes" })).toHaveValue(150);
  });

  // After a 409, the date's consent question is open under the managed answer read before it, and then a roster
  // write on the date begins (through the session's store: the question's dialog leaves the page inert). The begin
  // closes the question as no and nothing is sent (VC2); how the write ends decides what is read next.
  async function questionThenBegin() {
    const mounted = await refuseSave();
    edits.answer(challenge(2));
    await userEvent.click(screen.getByRole("radio", { name: /^Private/ }));
    await userEvent.click(screen.getByRole("button", { name: "Save this date" }));
    expect(await screen.findByRole("dialog", { name: "Save this date?" })).toBeInTheDocument();
    let end: (e: RosterWriteEnd) => void = () => {};

    await act(async () => { end = mounted.store.begin(KEY); });
    expect(screen.queryByRole("dialog", { name: "Save this date?" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Save this date" })).toBeDisabled();
    expect(edits.sent).toHaveLength(2);

    return { ...mounted, end };
  }

  it("a roster write refused 403 whose managed reread is refused too hands the page its retry once and shows nothing kept", async () => {
    const { end, callbacks } = await questionThenBegin();
    vi.mocked(Events.managedDate).mockRejectedValueOnce(new ApiError(403, "SECRET", { code: "forbidden" }));
    const seen = observeKept([DATE]);
    await act(async () => { end("lost"); });
    await waitFor(() => expect(callbacks.onGone).toHaveBeenCalledTimes(1));
    await act(async () => {});
    seen.stop();
    seen.expectClean();
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(screen.queryByRole("spinbutton", { name: "Minutes" })).not.toBeInTheDocument();
    expect(edits.sent).toHaveLength(2);
    expect(callbacks.onGone).toHaveBeenCalledTimes(1);
  });

  it("an authority recheck during a held roster write keeps the editor closed until the write ends and a later read answers", async () => {
    const { end, setAuthority } = await questionThenBegin();
    setAuthority("checking");
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    const recheck = deferred<OccurrenceResponse>();
    const after = deferred<OccurrenceResponse>();
    const panelRead = deferred<OccurrenceResponse>();
    vi.mocked(Events.managedDate).mockReturnValueOnce(recheck.promise).mockReturnValueOnce(after.promise).mockReturnValueOnce(panelRead.promise);
    const reads = vi.mocked(Events.managedDate).mock.calls.length;
    const seen = observeKept([DATE]);
    setAuthority("granted");
    await act(async () => { recheck.resolve(keptRead(DATE)); await recheck.promise; });
    // Asked while the write is on its way: the editor and the panel stay closed.
    expect(screen.queryByRole("spinbutton", { name: "Minutes" })).not.toBeInTheDocument();
    expect(screen.getByText("Loading this date…")).toBeInTheDocument();
    expect(Events.managedDate).toHaveBeenCalledTimes(reads + 1);
    await act(async () => { end("stored"); });
    expect(Events.managedDate).toHaveBeenCalledTimes(reads + 2);
    expect(screen.queryByRole("spinbutton", { name: "Minutes" })).not.toBeInTheDocument();
    await act(async () => { after.resolve(keptRead(DATE)); await after.promise; });
    expect(await screen.findByRole("spinbutton", { name: "Minutes" })).toHaveValue(150);
    expect(screen.getByRole("radio", { name: /^Private/ })).toBeChecked();
    await waitFor(() => expect(panel()).toHaveTextContent(KEPT.checking));
    seen.stop();
    seen.expectClean();
    await act(async () => { panelRead.resolve(keptRead(DATE, { keptCount: 0, attendees: [] })); await panelRead.promise; });
    expect(await screen.findByText(KEPT.nobodyOne)).toBeInTheDocument();
    expect(Events.managedDate).toHaveBeenCalledTimes(reads + 3);
    expect(edits.sent).toHaveLength(2);
  });

  // The reviewer's race, under VC2: the begin has already closed the question; the end rereads the managed date
  // once (useLoad asks from its effect, so a panel gate unmounted in the commit that records the end sends nothing).
  it.each(["stored", "lost", "unsure"] as const)("a roster write that ends %s after its begin closed the question rereads the managed date, and brings back the draft and the panel", async (ending) => {
    const { end } = await questionThenBegin();
    const reads = vi.mocked(Events.managedDate).mock.calls.length;
    const outer = deferred<OccurrenceResponse>();
    const panelRead = deferred<OccurrenceResponse>();
    vi.mocked(Events.managedDate).mockReturnValueOnce(outer.promise).mockReturnValueOnce(panelRead.promise);
    const seen = observeKept([DATE]);
    await act(async () => { end(ending); });

    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(document.activeElement).toBe(screen.getByRole("region", { name: "Manage this date" }));
    expect(screen.queryByRole("spinbutton", { name: "Minutes" })).not.toBeInTheDocument();
    expect(screen.getByText("Loading this date…")).toBeInTheDocument();
    expect(Events.managedDate).toHaveBeenCalledTimes(reads + 1);
    expect(edits.sent).toHaveLength(2);

    await act(async () => { outer.resolve(keptRead(DATE)); await outer.promise; });
    expect(await screen.findByRole("spinbutton", { name: "Minutes" })).toHaveValue(150);
    expect(screen.getByRole("radio", { name: /^Private/ })).toBeChecked();
    await waitFor(() => expect(panel()).toHaveTextContent(KEPT.checking));
    expect(Events.managedDate).toHaveBeenCalledTimes(reads + 2);
    seen.stop();
    seen.expectClean();

    await act(async () => { panelRead.resolve(keptRead(DATE, { keptCount: 0, attendees: [] })); await panelRead.promise; });
    expect(await screen.findByText(KEPT.nobodyOne)).toBeInTheDocument();
    expect(Events.managedDate).toHaveBeenCalledTimes(reads + 2);
    expect(edits.sent).toHaveLength(2);

    // Asked again under the fresh answer, the question submits the draft once.
    edits.answer(challenge(2, "e".repeat(32)));
    await userEvent.click(screen.getByRole("button", { name: "Save this date" }));
    expect(await screen.findByRole("dialog", { name: "Save this date?" })).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Save and notify" }));
    await waitFor(() => expect(edits.sent).toHaveLength(4));
    expect(edits.sent[3]).toMatchObject({ revision: edits.sent[2].revision, changes: { durationMinutes: 150, visibility: "private" }, consent: "e".repeat(32) });
  });

  // A write the server answered changed nothing: nothing is read, Save is back at once, and asking again submits once.
  it("a roster write answered with a refusal reads nothing and turns Save back on at once", async () => {
    const { end } = await questionThenBegin();
    const reads = vi.mocked(Events.managedDate).mock.calls.length;
    await act(async () => { end("answered"); });
    expect(Events.managedDate).toHaveBeenCalledTimes(reads);
    expect(screen.getByRole("button", { name: "Save this date" })).toBeEnabled();
    expect(screen.getByRole("spinbutton", { name: "Minutes" })).toHaveValue(150);
    edits.answer(challenge(2, "e".repeat(32)));
    await userEvent.click(screen.getByRole("button", { name: "Save this date" }));
    await userEvent.click(within(await screen.findByRole("dialog", { name: "Save this date?" })).getByRole("button", { name: "Save and notify" }));
    await waitFor(() => expect(edits.sent).toHaveLength(4));
    expect(edits.sent[3]).toMatchObject({ changes: { visibility: "private" }, consent: "e".repeat(32) });
  });

  it("with only the editor open, a roster write on this date rereads the managed date fail-closed and the editor comes back with the draft", async () => {
    vi.mocked(Events.managedDate).mockImplementation(() => Promise.resolve(keptRead(DATE)));
    const { store } = mount();
    await typeMinutes("150");
    await waitFor(() => expect(Events.managedDate).toHaveBeenCalledTimes(1));
    const fresh = deferred<OccurrenceResponse>();
    vi.mocked(Events.managedDate).mockReturnValueOnce(fresh.promise);
    await act(async () => { store.begin(KEY)("stored"); });
    expect(Events.managedDate).toHaveBeenCalledTimes(2);
    expect(screen.queryByRole("spinbutton", { name: "Minutes" })).not.toBeInTheDocument();
    await act(async () => { fresh.resolve(keptRead(DATE, { attendees: [{ ...oldName, name: "New Name" }] })); await fresh.promise; });
    expect(await screen.findByRole("spinbutton", { name: "Minutes" })).toHaveValue(150);
    // Opening the roster shows the answer asked after the write, without asking again.
    await userEvent.click(screen.getByRole("button", { name: "Manage sign-ups" }));
    expect(await screen.findByText("New Name")).toBeInTheDocument();
    expect(screen.queryByText("Old Name")).not.toBeInTheDocument();
    expect(Events.managedDate).toHaveBeenCalledTimes(2);
    // A write the server answered starts no read.
    await act(async () => { store.begin(KEY)("answered"); });
    expect(Events.managedDate).toHaveBeenCalledTimes(2);
    expect(screen.getByRole("spinbutton", { name: "Minutes" })).toHaveValue(150);
  });

  it("asks before the tab closes while a typed change waits, off the screen, for the date to be read again; a date nobody changed does not ask", async () => {
    vi.mocked(Events.managedDate).mockImplementation(() => Promise.resolve(keptRead(DATE)));
    const { store } = mount();
    const leaving = () => !window.dispatchEvent(new Event("beforeunload", { cancelable: true }));
    const minutes = await openEditor();
    expect(leaving()).toBe(false);
    await userEvent.clear(minutes);
    await userEvent.type(minutes, "150");
    expect(leaving()).toBe(true);
    const fresh = deferred<OccurrenceResponse>();
    vi.mocked(Events.managedDate).mockReturnValueOnce(fresh.promise);
    await act(async () => { store.begin(KEY)("stored"); });
    expect(screen.queryByRole("spinbutton", { name: "Minutes" })).not.toBeInTheDocument();
    expect(leaving()).toBe(true);
    await act(async () => { fresh.resolve(keptRead(DATE)); await fresh.promise; });
    expect(await screen.findByRole("spinbutton", { name: "Minutes" })).toHaveValue(150);
    expect(leaving()).toBe(true);
    await userEvent.click(screen.getByRole("button", { name: "Edit this date" }));
    expect(leaving()).toBe(false);
  });

  it("a panel refusal calls onGone once, and the editor comes back with the draft and no panel", async () => {
    vi.mocked(Events.managedDate).mockImplementation(() => Promise.resolve(keptRead(DATE)));
    edits.answer(kept409([{ date: DATE, keptCount: 1 }]));
    const { callbacks, setAuthority } = mount();
    await typeMinutes("150");
    // The editor's base answers first; the panel's read is refused.
    vi.mocked(Events.managedDate).mockRejectedValueOnce(new ApiError(403, "SECRET", { code: "forbidden" }));
    await userEvent.click(screen.getByRole("button", { name: "Save this date" }));
    await waitFor(() => expect(callbacks.onGone).toHaveBeenCalledTimes(1));
    setAuthority("checking");
    setAuthority("granted");
    expect(await screen.findByRole("spinbutton", { name: "Minutes" })).toHaveValue(150);
    await act(async () => {});
    expect(panel()).not.toBeInTheDocument();
    expect(callbacks.onGone).toHaveBeenCalledTimes(1);
  });
});

// VC2's held-request matrix on the date page: a roster write on this date (Add, restore or Remove, from the date's
// own roster or the editor's kept-dates panel) holds the editor's Save from its begin to its end. Nothing is
// previewed or sent while it is on its way; a begin while the count is read opens no question. A write that may
// have stored something rereads the managed date fail-closed, and the next Save counts afresh with that answer's
// consent; an answered one turns Save back on at once, with no read.
describe("ManageDate: a roster write on the date holds the editor's Save", () => {
  const STALE = "The page changed while we were asking, so your changes weren't saved. You can save again.";
  const FRESH = "ffffffffffffffffffffffffffffffff";
  const removed = { removedRegistrations: [removedRow()] };
  const save = () => screen.getByRole("button", { name: "Save this date" });
  const minutes = () => screen.getByRole("spinbutton", { name: "Minutes" });

  const rosterOf = (name: string) => () => {
    const section = screen.getByText(name, { selector: "p, span, div, strong" }).closest("section[aria-label='Manage sign-ups']");

    if (!(section instanceof HTMLElement)) throw new Error(`no roster with ${name}`);

    return section;
  };

  // The source's roster beside an editor drafting Minutes 150 and Private.
  async function editorBeside(source: "roster" | "panel") {
    if (source === "panel") {
      vi.mocked(Events.managedDate).mockImplementation(() => Promise.resolve(keptRead(DATE, removed)));
      edits.answer(kept409([{ date: DATE, keptCount: 1 }]));
      mount();
      await typeMinutes("150");
      await userEvent.click(save());
      await screen.findByText("Old Name");
    } else {
      vi.mocked(Events.managedDate).mockImplementation(() => Promise.resolve(answer(removed)));
      mount();
      await typeMinutes("150");
      await openRoster();
    }

    await userEvent.click(screen.getByRole("radio", { name: /^Private/ }));
  }

  for (const source of ["roster", "panel"] as const) {
    for (const op of ROSTER_OPS) {
      for (const end of endsFor(op)) {
        const answered = end === "answered" || end === "event_full";

        it(`${source}: a held ${op} disables Save and sends nothing; ending ${end} ${answered ? "turns Save on with no read" : "rereads, then one save asks the writer's fresh question"}`, async () => {
          await editorBeside(source);
          const puts = edits.sent.length;
          const write = await startHeld(op, rosterOf("Old Name"), "Old Name");
          const reads = vi.mocked(Events.managedDate).mock.calls.length;
          expect(save()).toBeDisabled();
          await userEvent.click(save());
          expect(edits.sent).toHaveLength(puts);
          expect(minutes()).toBeEnabled();
          expect(minutes()).toHaveValue(150);

          await act(async () => { ROSTER_ENDS[end](write); await write.promise.catch(() => {}); });

          if (answered) {
            expect(save()).toBeEnabled();
            expect(Events.managedDate).toHaveBeenCalledTimes(reads);
          } else {
            // The gate is the one starter of the read after the write: exactly one, and the editor comes back with it.
            await waitFor(() => expect(save()).toBeEnabled());
            expect(Events.managedDate).toHaveBeenCalledTimes(reads + (source === "panel" ? 2 : 1));
          }

          expect(minutes()).toHaveValue(150);
          expect(screen.getByRole("radio", { name: /^Private/ })).toBeChecked();
          edits.answer(challenge(3, FRESH));
          await userEvent.click(save());
          const asked = await screen.findByRole("dialog", { name: "Save this date?" });
          expect(asked).toHaveTextContent("This cancels and notifies 3 registered people");
          await userEvent.click(within(asked).getByRole("button", { name: "Save and notify" }));
          await waitFor(() => expect(edits.sent).toHaveLength(puts + 2));
          expect(edits.sent[edits.sent.length - 1]).toMatchObject({ changes: { durationMinutes: 150, visibility: "private" }, consent: FRESH });
        });
      }

      it(`${source}: a ${op} that begins while the save is on its way opens no question and sends nothing more`, async () => {
        await editorBeside(source);
        const puts = edits.sent.length;
        const sent = deferred<OccurrenceResponse>();
        edits.answer(sent.promise);
        await userEvent.click(save());
        await waitFor(() => expect(edits.sent).toHaveLength(puts + 1));
        const write = await startHeld(op, rosterOf("Old Name"), "Old Name");
        await act(async () => { sent.reject(challenge(2)); await sent.promise.catch(() => {}); });
        expect(screen.queryByRole("dialog", { name: "Save this date?" })).not.toBeInTheDocument();
        expect(await screen.findByText(STALE)).toBeInTheDocument();
        expect(edits.sent).toHaveLength(puts + 1);
        expect(save()).toBeDisabled();
        expect(minutes()).toHaveValue(150);
        await act(async () => { ROSTER_ENDS.answered(write); await write.promise.catch(() => {}); });
        expect(save()).toBeEnabled();
        expect(edits.sent).toHaveLength(puts + 1);
      });
    }
  }
});

describe("ManageDate's owner: the route visit, the date and the row", () => {
  const PATH = `/events/7/dates/${DATE}`;

  // EventDate's shape, keyed as EventDate keys it (person, event, date): a second visit to the same address
  // keeps this component mounted.
  function mountOwned() {
    const nav = createRef<NavigateFunction>();
    const alertsAtCommit: string[] = [];

    function Navigator() {
      nav.current = useNavigate();
      const location = useLocation();

      useLayoutEffect(() => {
        alertsAtCommit.push(document.querySelector('[role="alert"]')?.textContent ?? "");
      }, [location.key]);

      return null;
    }

    const ui = (row = 1) => <MemoryRouter initialEntries={[PATH]}><RosterWritesProvider><ConfirmProvider>
      <Routes><Route path="/events/:id/dates/:date" element={<ManageDate eventId={7} date={DATE} occurrenceId={row} authority="granted" {...pageCallbacks()} />} /></Routes>
      <Navigator />
    </ConfirmProvider></RosterWritesProvider></MemoryRouter>;

    const view = render(ui());

    return {
      visitAgain: () => act(async () => { await nav.current?.(PATH); }),
      replaceOccurrence: (row: number) => view.rerender(ui(row)),
      alertsAtCommit,
    };
  }

  const saveButton = () => screen.getByRole("button", { name: "Save this date" });

  it("a failed read from the previous visit is hidden at the successor's first commit", async () => {
    const { visitAgain, alertsAtCommit } = mountOwned();
    vi.mocked(Events.managedDate).mockRejectedValueOnce(new Error("offline"));
    await userEvent.click(screen.getByRole("button", { name: "Edit this date" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("We couldn't load this date's details");
    const fresh = deferred<OccurrenceResponse>();
    vi.mocked(Events.managedDate).mockReturnValueOnce(fresh.promise);
    await visitAgain();
    expect(alertsAtCommit[alertsAtCommit.length - 1]).toBe("");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(screen.queryByRole("textbox", { name: "Title" })).not.toBeInTheDocument();
    expect(edits.sent).toHaveLength(0);
    await act(async () => { fresh.resolve(answer({ revision: "r-new" })); await fresh.promise; });
    expect(await screen.findByRole("textbox", { name: "Title" })).toHaveValue("Supper");
  });

  it("a replacement occurrence drops the draft and does not use the earlier row's held read", async () => {
    const { replaceOccurrence } = mountOwned();
    await typeMinutes("150");
    const old = deferred<OccurrenceResponse>();
    const fresh = deferred<OccurrenceResponse>();
    vi.mocked(Events.managedDate).mockReturnValueOnce(old.promise).mockReturnValueOnce(fresh.promise);
    await userEvent.click(screen.getByRole("button", { name: "Close" }));
    await userEvent.click(screen.getByRole("button", { name: "Edit this date" }));
    replaceOccurrence(2);
    await act(async () => { old.resolve(answer({ title: "Old row", revision: "r-old" })); await old.promise; });
    expect(document.body.textContent).not.toContain("Old row");
    await act(async () => { fresh.resolve(answer({ id: 2, revision: "r-new" })); await fresh.promise; });
    expect(await screen.findByRole("spinbutton", { name: "Minutes" })).toHaveValue(60);
    await waitFor(() => expect(saveButton()).toBeEnabled());
    await userEvent.type(screen.getByRole("textbox", { name: "Title" }), "!");
    await userEvent.click(saveButton());
    await waitFor(() => expect(edits.sent).toHaveLength(1));
    expect(edits.sent[0]).toMatchObject({ revision: "r-new", changes: { title: "Supper!" } });
  });

  it("an earlier owner's failed read cannot replace a successor's answer", async () => {
    const { visitAgain } = mountOwned();
    const old = deferred<OccurrenceResponse>();
    const fresh = deferred<OccurrenceResponse>();
    vi.mocked(Events.managedDate).mockReturnValueOnce(old.promise).mockReturnValueOnce(fresh.promise);
    await userEvent.click(screen.getByRole("button", { name: "Edit this date" }));
    await visitAgain();
    await act(async () => { fresh.resolve(answer({ revision: "r-new" })); await fresh.promise; });
    expect(await screen.findByRole("textbox", { name: "Title" })).toHaveValue("Supper");
    await act(async () => { old.reject(new Error("old owner failure")); await old.promise.catch(() => {}); });
    expect(screen.getByRole("textbox", { name: "Title" })).toHaveValue("Supper");
    expect(document.body.textContent).not.toContain("old owner failure");
    await waitFor(() => expect(saveButton()).toBeEnabled());
  });

  it("a second visit to the same address reads the date again and never brings back what was typed", async () => {
    const { visitAgain } = mountOwned();
    await typeMinutes("150");
    expect(Events.managedDate).toHaveBeenCalledTimes(1);
    const next = deferred<OccurrenceResponse>();
    vi.mocked(Events.managedDate).mockReturnValueOnce(next.promise);
    await visitAgain();
    expect(Events.managedDate).toHaveBeenCalledTimes(2);
    expect(screen.queryByRole("spinbutton", { name: "Minutes" })).not.toBeInTheDocument();
    await act(async () => { next.resolve(answer({ revision: "r-visit" })); await next.promise; });
    const minutes = await screen.findByRole("spinbutton", { name: "Minutes" });
    expect(minutes).toHaveValue(60);
    await waitFor(() => expect(saveButton()).toBeEnabled());
    await userEvent.type(screen.getByRole("textbox", { name: "Title" }), "!");
    await userEvent.click(saveButton());
    await waitFor(() => expect(edits.sent).toHaveLength(1));
    expect(edits.sent[0]).toMatchObject({ revision: "r-visit", changes: { title: "Supper!" } });
  });

  it("a read still on its way when the address is visited again is never shown or used", async () => {
    const { visitAgain } = mountOwned();
    const first = deferred<OccurrenceResponse>();
    const second = deferred<OccurrenceResponse>();
    vi.mocked(Events.managedDate).mockReturnValueOnce(first.promise).mockReturnValueOnce(second.promise);
    await userEvent.click(screen.getByRole("button", { name: "Edit this date" }));
    await visitAgain();
    await act(async () => { first.resolve(answer({ title: "Stale title", revision: "r-stale" })); await first.promise; });
    expect(screen.queryByRole("textbox", { name: "Title" })).not.toBeInTheDocument();
    expect(document.body.textContent).not.toContain("Stale title");
    await act(async () => { second.resolve(answer({ revision: "r-fresh" })); await second.promise; });
    expect(await screen.findByRole("textbox", { name: "Title" })).toHaveValue("Supper");
    await waitFor(() => expect(saveButton()).toBeEnabled());
    await userEvent.type(screen.getByRole("textbox", { name: "Title" }), "!");
    await userEvent.click(saveButton());
    await waitFor(() => expect(edits.sent).toHaveLength(1));
    expect(edits.sent[0].revision).toBe("r-fresh");
  });

  it.each(["first", "second"] as const)("open, close and open again: with the %s read answering first, only the second open's read is shown and used", async (order) => {
    mountOwned();
    const reads = { first: deferred<OccurrenceResponse>(), second: deferred<OccurrenceResponse>() };
    vi.mocked(Events.managedDate).mockReturnValueOnce(reads.first.promise).mockReturnValueOnce(reads.second.promise);
    const edit = () => userEvent.click(screen.getByRole("button", { name: "Edit this date" }));
    await edit();
    await edit();
    await edit();
    expect(Events.managedDate).toHaveBeenCalledTimes(2);
    const titles = { first: "First open", second: "Second open" };

    for (const which of order === "first" ? (["first", "second"] as const) : (["second", "first"] as const)) {
      await act(async () => { reads[which].resolve(answer({ title: titles[which], revision: `r-${which}` })); await reads[which].promise; });
    }

    expect(await screen.findByRole("textbox", { name: "Title" })).toHaveValue("Second open");
    expect(document.body.textContent).not.toContain("First open");
    await waitFor(() => expect(saveButton()).toBeEnabled());
    await userEvent.type(screen.getByRole("textbox", { name: "Title" }), "!");
    await userEvent.click(saveButton());
    await waitFor(() => expect(edits.sent).toHaveLength(1));
    expect(edits.sent[0]).toMatchObject({ revision: "r-second", changes: { title: "Second open!" } });
  });

  it("a question open when the address is visited again closes, and its Yes sends nothing", async () => {
    const { visitAgain } = mountOwned();
    await openEditor();
    await userEvent.click(screen.getByRole("radio", { name: /^Private/ }));
    edits.answer(challenge(2));
    await waitFor(() => expect(saveButton()).toBeEnabled());
    await userEvent.click(saveButton());
    const asked = await screen.findByRole("dialog", { name: "Save this date?" });
    const yes = within(asked).getByRole("button", { name: "Save and notify" });
    const later = deferred<OccurrenceResponse>();
    vi.mocked(Events.managedDate).mockReturnValueOnce(later.promise);
    await visitAgain();
    await waitFor(() => expect(screen.queryByRole("dialog", { name: "Save this date?" })).not.toBeInTheDocument());

    // A Yes captured before the visit, should it still be in the page.
    if (yes.isConnected) await userEvent.click(yes);
    await act(async () => { later.resolve(answer({ revision: "r-visit" })); await later.promise; });
    expect(await screen.findByRole("spinbutton", { name: "Minutes" })).toHaveValue(60);
    expect(edits.sent).toHaveLength(1);
    expect(edits.sent[0].consent).toBeUndefined();
  });

  it("a roster write that ends unsure while the editor's read is held: only the read asked after the end is shown", async () => {
    const store = createRosterWrites();
    render(<MemoryRouter><RosterWritesProvider rosterWrites={store}><ConfirmProvider>
      <ManageDate eventId={7} date={DATE} occurrenceId={1} authority="granted" {...pageCallbacks()} />
    </ConfirmProvider></RosterWritesProvider></MemoryRouter>);
    await openRoster();
    const heldRead = deferred<OccurrenceResponse>();
    vi.mocked(Events.managedDate).mockReturnValueOnce(heldRead.promise).mockResolvedValue(answer({ title: "Fresh title", revision: "r-after" }));
    // Opening the editor reads the date again while the roster is open.
    await userEvent.click(screen.getByRole("button", { name: "Edit this date" }));
    const reads = vi.mocked(Events.managedDate).mock.calls.length;
    const end = store.begin(KEY);
    act(() => end("unsure"));
    await waitFor(() => expect(vi.mocked(Events.managedDate).mock.calls.length).toBe(reads + 1));
    await act(async () => { heldRead.resolve(answer({ title: "Held title", revision: "r-held" })); await heldRead.promise; });
    expect(await screen.findByRole("textbox", { name: "Title" })).toHaveValue("Fresh title");
    expect(document.body.textContent).not.toContain("Held title");
    await waitFor(() => expect(saveButton()).toBeEnabled());
    await userEvent.type(screen.getByRole("textbox", { name: "Title" }), "!");
    await userEvent.click(saveButton());
    await waitFor(() => expect(edits.sent).toHaveLength(1));
    expect(edits.sent[0].revision).toBe("r-after");
  });

});

// B2q on the date page: ManageDate's own dialog asks only the writer's question for exactly this save, and closes
// it as no when what asked it is retired: the page's authority, a roster write, the marker, the credential.
describe("ManageDate: the date question is bound to what asked it", () => {
  const T1 = "11111111111111111111111111111111";
  const T2 = "22222222222222222222222222222222";
  const asked = () => screen.queryByRole("dialog", { name: "Save this date?" });
  const saveButton = () => screen.getByRole("button", { name: "Save this date" });

  afterEach(() => { setMarkerCookie(credential.marker); });

  async function ask(answer: Error = challenge(2, T1)) {
    await openEditor();
    await userEvent.click(screen.getByRole("radio", { name: /^Private/ }));
    edits.answer(answer);
    await waitFor(() => expect(saveButton()).toBeEnabled());
    await userEvent.click(saveButton());
  }

  const yesOf = async () => within(await screen.findByRole("dialog", { name: "Save this date?" })).getByRole("button", { name: "Save and notify" });

  it("names the date, the change and the count; a Yes kept from a closed question answers nothing once it opens again", async () => {
    mount();
    await ask();
    const first = await screen.findByRole("dialog", { name: "Save this date?" });
    expect(first).toHaveAccessibleDescription("Tuesday, December 1, 2026. This date is public now. Saving makes it private, so fewer people will be able to see it. This cancels and notifies 2 registered people who can no longer see this date.");
    const kept = within(first).getByRole("button", { name: "Save and notify" });
    await userEvent.click(within(first).getByRole("button", { name: "Cancel" }));
    edits.answer(challenge(2, T2));
    await userEvent.click(saveButton());
    const again = await yesOf();
    await userEvent.click(kept);
    expect(edits.sent).toHaveLength(2);
    await userEvent.click(again);
    await waitFor(() => expect(edits.sent).toHaveLength(3));
    expect(edits.sent.map((s) => s.consent)).toEqual([undefined, undefined, T2]);
    expect(edits.sent[2].attempt).not.toBe(edits.sent[1].attempt);
  });

  // An administrator erasing this manager from their own browser (not a sign-out in another tab), in App's session.
  // The date page also reads the date again, fail closed, whenever the window gets focus back (EventDate's
  // refreshOnFocus), which closes any question on it; this is the editor's own check, under the page's provider.
  describe("when an administrator erases this manager from their own browser", () => {
    erasureCases(() => ({
      credential: () => credential,
      sent: () => edits.sent,
      mount: (onCredential) => {
        render(<MemoryRouter><SignedInSession onCredential={onCredential}><ConfirmProvider>
          <ManageDate eventId={7} date={DATE} occurrenceId={1} authority="granted" {...pageCallbacks()} />
        </ConfirmProvider></SignedInSession></MemoryRouter>);
      },
      draft: async () => {
        await userEvent.click(await screen.findByRole("button", { name: "Edit this date" }));
        await userEvent.click(await screen.findByRole("radio", { name: /^Private/ }));
      },
      drafted: () => screen.getByRole("radio", { name: /^Private/, hidden: true }),
      ask: async () => {
        edits.answer(challenge(2, T1));
        await waitFor(() => expect(saveButton()).toBeEnabled());
        await userEvent.click(saveButton());
      },
    }));
  });

  const invalidations: Array<[string, (m: Mounted) => void]> = [
    ["the page checking this person's authority again", ({ setAuthority }) => { act(() => setAuthority("checking")); }],
    ["the page losing this person's authority", ({ setAuthority }) => { act(() => setAuthority("lost")); }],
    ["a roster write on the date beginning", ({ store }) => { act(() => { store.begin(KEY); }); }],
    ["a roster write on the date beginning and ending in one task", ({ store }) => { act(() => store.begin(KEY)("answered")); }],
  ];

  for (const [name, invalidate] of invalidations) {
    it(`closes the question as no on ${name}, and a Yes kept from it sends nothing`, async () => {
      const m = mount();
      await ask();
      const yes = await yesOf();
      invalidate(m);
      expect(asked()).not.toBeInTheDocument();
      await userEvent.click(yes);
      expect(edits.sent).toHaveLength(1);
      expect(document.activeElement?.closest("[role='dialog']") ?? null).toBeNull();
    });
  }

  it("a Yes after this tab's marker changed sends nothing, and says the changes weren't saved", async () => {
    mount();
    await ask();
    const yes = await yesOf();
    setMarkerCookie("m-another-tab");
    await userEvent.click(yes);
    expect(asked()).not.toBeInTheDocument();
    expect(await screen.findByText("The page changed while we were asking, so your changes weren't saved. You can save again.")).toBeInTheDocument();
    expect(edits.sent).toHaveLength(1);
  });

  it.each([
    ["another date's row", new ApiError(409, "x", { code: "date_needs_consent", impact: { visibility: "private", currentVisibility: "public", tightened: [{ id: 7, title: "Supper", visibility: "public", registrants: 2, occurrenceId: 9 }], leaderOpened: [], consent: T1 } })],
    ["a negative count", challenge(-1, T1)],
    ["a missing token", new ApiError(409, "x", { code: "date_needs_consent", impact: { visibility: "private", currentVisibility: "public", tightened: [{ id: 7, title: "Supper", visibility: "public", registrants: 2, occurrenceId: 1 }], leaderOpened: [] } })],
  ])("never asks for %s", async (_, answer) => {
    mount();
    await ask(answer);
    expect(await screen.findByText(ERROR_COPY.visibility_review_outdated)).toBeInTheDocument();
    expect(asked()).not.toBeInTheDocument();
    expect(edits.sent).toHaveLength(1);
  });
});
