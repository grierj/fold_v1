import { requiredElement } from "../test/dom";
import { renderWithAuth } from "../test/auth";
import { stubService } from "../test/service";
import { act, cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Outlet, Route, RouterProvider, Routes, createMemoryRouter } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError, type Credential } from "../api/core";
import { DateRooms, SeriesRooms } from "../api/rooms";
import { Events } from "../api/events";
import { Households } from "../api/households";
import { Site } from "../api/site";
import { Adds } from "../api/adds";
import { People } from "../api/people";
import { UnsavedChangesBlocker } from "../lib/unsavedChanges";
import { createRosterWrites, DateWritesProvider, type RosterWrites } from "../lib/dateWrites";
import { addRequest } from "../test/addRequest";
import type { EventDetailResponse, EventOccurrence, NeedsRoom, OccurrenceResponse } from "../types";
import { occurrence } from "../test/occurrence";
import { deferred as held, HOST_EVENTS, kept409, keptRead, READ_OUTCOMES, runHostCase } from "../test/keptPanel";
import { endsFor, removedRow, ROSTER_ENDS, ROSTER_OPS, startHeld } from "../test/rosterOps";
import { EventDetail } from "./EventDetail";
import { ERROR_COPY } from "../lib/errors";
import { storedEdit, stubEditSends, verifiedCredential, type EditSends } from "../test/guardedEdits";
import { setMarkerCookie } from "../test/credential";
import { erasureCases, SignedInSession } from "../test/session";

const composer = vi.hoisted(() => ({ on: false, admin: false }));

const render = renderWithAuth(() => ({ user: { id: 2 }, loading: false, isAdmin: composer.admin, canCompose: composer.on }));

stubService(Households, () => ({ mine: vi.fn().mockResolvedValue({ household: null }) }));

stubService(Events, () => ({
  get: vi.fn(), register: vi.fn(), registerSeries: vi.fn(), undoSeriesRegistration: vi.fn(),
  delete: vi.fn(), cancelOccurrence: vi.fn(), dateVisibilityImpact: vi.fn(), update: vi.fn(),
  removeRegistration: vi.fn(), promoteRegistration: vi.fn(), restoreRegistration: vi.fn(), addRegistration: vi.fn(), addHost: vi.fn(),
  managedDate: vi.fn(), visibilityImpact: vi.fn(), setVisibility: vi.fn(),
}));

stubService(People, () => ({ search: vi.fn() }));

stubService(Adds, () => ({ create: vi.fn() }));

// Every question on this page is the app's dialog: the browser's prompts fail the test if anything reaches them.
function nativePromptsFail() {
  vi.spyOn(window, "confirm").mockImplementation(() => { throw new Error("native confirm on the event page"); });
  vi.spyOn(window, "alert").mockImplementation(() => { throw new Error("native alert on the event page"); });
}

const page: EventDetailResponse = {
  event: {
    id: 7, title: "Hybrid study", description: "", startsLocal: "2026-12-01T19:00", durationMinutes: 60,
    tzid: "UTC", startsAt: "2026-12-01T19:00:00Z", endsAt: "2026-12-01T20:00:00Z", location: "Hall",
    recurrenceRule: "", visibility: "public", leaderOpened: false, churchWide: false,
    capacity: null, waitlistEnabled: false, guestsAllowed: 0, reminderEmailMinutes: 1440, reminderPush: true,
    childcareAvailable: false, childcareCapacity: null, childcareAges: "", childcareNotes: "",
    remoteAvailable: true, remoteCapacity: 5, videoProvider: "none", videoReveal: "registered_only",
  },
  occurrences: [occurrence({
    remoteAvailable: true, remoteCapacity: 5, remoteSpotsLeft: 4,
    attendeesVisible: true,
    attendees: [{ registrationId: 31, userId: 3, name: "Sam", status: "going", guestCount: 0, attendanceMode: "remote" }],
  })],
  canManage: true, canSetVisibility: false,
};

// The session's credential and its guarded date edits, as App provides them around every route.
let credential: Credential;
let edits: EditSends;

// What a date editor's own manager read answers: the date as the page last showed it, with its edit token.
function stubEditorReads() {
  vi.mocked(Events.managedDate).mockImplementation(async (_id, date) => {
    const shown = vi.mocked(Events.get).mock.settledResults.flatMap((r) => (r.type === "fulfilled" ? [r.value] : []));
    const latest = shown[shown.length - 1] ?? page;
    const listed = latest.occurrences.find((o) => o.date === date) ?? page.occurrences[0];

    return { occurrence: { ...listed, revision: "r0" } };
  });
}

// After a describe's vi.resetAllMocks/restoreAllMocks: the guarded transport and the editors' reads again.
function guarded() {
  edits = stubEditSends();
  stubEditorReads();
}

beforeEach(async () => {
  credential = await verifiedCredential();
  guarded();
});

// The writer's question for a tightening of the date that cancels `registrants` people (DEC-158).
const challenge = (registrants: number, consent = "c".repeat(32), current: "public" | "private" = "public", target: "private" | "invite_only" = "private") => new ApiError(409, "x", { code: "date_needs_consent", impact: { visibility: target, currentVisibility: current, tightened: [{ id: 7, title: "One date", visibility: current, registrants, occurrenceId: 1 }], leaderOpened: [], consent } });

function renderPage(path = "/events/7") {
  render(<MemoryRouter initialEntries={[path]}><DateWritesProvider credential={credential}><Routes><Route path="/events/:id" element={<EventDetail />} /></Routes></DateWritesProvider></MemoryRouter>);
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (cause: unknown) => void;
  const promise = new Promise<T>((accept, decline) => { resolve = accept; reject = decline; });

  return { promise, resolve, reject };
}

// A series date has no whole-series buttons (DEC-92): its answer opens the
// scope dialog, and "All future dates" answers every date.
async function answerAll(group: HTMLElement, name: "Going" | "Maybe" | "Can't go") {
  await userEvent.click(within(group).getByRole("button", { name }));
  await userEvent.click(screen.getByRole("button", { name: "All future dates" }));
}

async function chooseDatesForMe(form: HTMLElement) {
  await userEvent.click(within(form).getByRole("button", { name: "Choose dates for my answer" }));
  await userEvent.click(screen.getByRole("button", { name: "All future dates" }));
}

async function editDate() {
  renderPage();
  await userEvent.click(await screen.findByRole("button", { name: "More for 2026-12-01" }));
  await userEvent.click(screen.getByRole("menuitem", { name: "Edit this date" }));
  const input = await screen.findByRole("textbox", { name: /^Remote seat limit for this date/ });
  await waitFor(() => expect(screen.getByRole("button", { name: "Save this date" })).toBeEnabled());

  return input;
}

describe("EventDetail remote seats", () => {
  beforeEach(() => {
    vi.resetAllMocks();
    guarded();
    vi.mocked(Households.mine).mockResolvedValue({ household: null });
    vi.mocked(Events.get).mockResolvedValue(page);
  });

  it.each(["1e", "-2", "1.5"])("preserves and focuses invalid per-date text %s without saving", async (text) => {
    const input = await editDate();
    expect(input).toHaveAttribute("inputmode", "numeric");
    await userEvent.clear(input);
    await userEvent.type(input, text);
    await userEvent.click(screen.getByRole("button", { name: "Save this date" }));
    expect(edits.sent).toHaveLength(0);
    expect(input).toHaveValue(text);
    expect(input).toHaveAttribute("aria-invalid", "true");
    expect(input).toHaveAccessibleDescription(expect.stringContaining("whole number from 0"));
    await waitFor(() => expect(input).toHaveFocus());
  });

  it.each([["0", 0], ["8", 8], ["", null]] as const)("saves the per-date limit %s", async (text, limit) => {
    const input = await editDate();
    await userEvent.clear(input);

    if (text) await userEvent.type(input, text);
    await userEvent.click(screen.getByRole("button", { name: "Save this date" }));
    await waitFor(() => expect(edits.sent.map((sent) => sent.changes)).toEqual([{ remoteCapacityOverride: limit }]));
  });

  it("marks remote people in the leader roster", async () => {
    renderPage();
    await userEvent.click(await screen.findByRole("button", { name: "More for 2026-12-01" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Manage sign-ups" }));
    const roster = within(await screen.findByRole("region", { name: "Manage sign-ups" }));
    expect(roster.getByText("Sam", { selector: "span:not(.sr-only)" })).toBeInTheDocument();
    expect(roster.getByText("Remote")).toBeInTheDocument();
  });
});

it("uses the shared scope dialog for a recurring date", async () => {
  vi.mocked(Events.get).mockResolvedValue({ ...page, event: { ...page.event, recurrenceRule: "FREQ=WEEKLY" }, occurrences: [occurrence({ recurrenceRule: "FREQ=WEEKLY" })] });
  renderPage();
  await userEvent.click((await screen.findAllByRole("button", { name: "Maybe" }))[0]);
  expect(screen.getByRole("dialog", { name: /How far should “Maybe” apply/ })).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Just this date" })).toHaveFocus();
});

it("serializes sibling series actions synchronously and releases after rejection or cancelled confirmation", async () => {
  const dates = [occurrence({ recurrenceRule: "FREQ=WEEKLY" }), occurrence({ id: 2, date: "2026-12-08", recurrenceRule: "FREQ=WEEKLY" })];
  vi.mocked(Events.get).mockResolvedValue({ ...page, event: { ...page.event, recurrenceRule: "FREQ=WEEKLY" }, occurrences: dates });
  const pending = deferred<Awaited<ReturnType<typeof Events.registerSeries>>>();
  vi.mocked(Events.registerSeries).mockReturnValue(pending.promise);

  renderPage();
  const answerGroups = await screen.findAllByRole("group", { name: "Your answer" });
  await userEvent.click(within(answerGroups[0]).getByRole("button", { name: "Maybe" }));
  await userEvent.click(within(answerGroups[1]).getByRole("button", { name: "Maybe" }));
  const allFuture = screen.getAllByRole("button", { name: "All future dates" });
  await act(() => {
    allFuture[0].click();
    allFuture[1].click();
  });
  expect(Events.registerSeries).toHaveBeenCalledTimes(1);
  expect(within(answerGroups[1]).getByRole("button", { name: "Going" })).toBeDisabled();

  await act(async () => pending.reject(new Error("offline")));
  await waitFor(() => expect(within(answerGroups[1]).getByRole("button", { name: "Going" })).toBeEnabled());

  nativePromptsFail();
  await answerAll(answerGroups[0], "Can't go");
  const consent = screen.getByRole("dialog", { name: "Can't go to these dates?" });
  expect(consent).toHaveTextContent("They will be removed from your calendar and personal feed.");
  await userEvent.click(within(consent).getByRole("button", { name: "Cancel" }));
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  expect(Events.registerSeries).toHaveBeenCalledTimes(1);
  expect(within(answerGroups[1]).getByRole("button", { name: "Maybe" })).toBeEnabled();

  vi.mocked(Events.registerSeries).mockResolvedValue({
    eventId: 7, scope: "all_future", status: "maybe",
    results: dates.map((row) => ({ occurrenceId: row.id, date: row.date, before: {}, after: { registrationId: row.id + 10, status: "maybe" as const } })),
    refusals: [],
  });
  await answerAll(answerGroups[1], "Maybe");
  expect(Events.registerSeries).toHaveBeenCalledTimes(2);
});

it("blocks sibling date and Undo mutations while their requests are pending, then releases the page gate", async () => {
  const dates = [occurrence({ recurrenceRule: "FREQ=WEEKLY" }), occurrence({ id: 2, date: "2026-12-08", recurrenceRule: "FREQ=WEEKLY" })];
  vi.mocked(Events.get).mockResolvedValue({ ...page, event: { ...page.event, recurrenceRule: "FREQ=WEEKLY" }, occurrences: dates });
  const datePending = deferred<Awaited<ReturnType<typeof Events.register>>>();
  vi.mocked(Events.register).mockReturnValueOnce(datePending.promise);
  vi.mocked(Events.registerSeries).mockResolvedValue({
    eventId: 7, scope: "all_future", status: "maybe",
    results: dates.map((row) => ({ occurrenceId: row.id, date: row.date, before: {}, after: { registrationId: row.id + 10, status: "maybe" as const } })),
    refusals: [], undoToken: "undo-token", undoExpiresAt: new Date(Date.now() + 10_000).toISOString(),
  });
  const undoPending = deferred<Awaited<ReturnType<typeof Events.undoSeriesRegistration>>>();
  vi.mocked(Events.undoSeriesRegistration).mockReturnValue(undoPending.promise);

  renderPage();
  const answerGroups = await screen.findAllByRole("group", { name: "Your answer" });
  await userEvent.click(within(answerGroups[0]).getByRole("button", { name: "Maybe" }));
  await userEvent.click(screen.getByRole("button", { name: "Just this date" }));
  expect(within(answerGroups[1]).getByRole("button", { name: "Going" })).toBeDisabled();
  expect(within(answerGroups[1]).getByRole("button", { name: "Maybe" })).toBeDisabled();
  await act(async () => datePending.reject(new Error("offline")));
  await waitFor(() => expect(within(answerGroups[1]).getByRole("button", { name: "Going" })).toBeEnabled());

  await answerAll(answerGroups[0], "Maybe");
  const undo = await screen.findByRole("button", { name: "Undo" });
  await userEvent.click(undo);
  expect(within(answerGroups[1]).getByRole("button", { name: "Going" })).toBeDisabled();
  await act(async () => undoPending.reject(new Error("offline")));
  await waitFor(() => expect(within(answerGroups[1]).getByRole("button", { name: "Going" })).toBeEnabled());
});

it("clears a sibling card's series Undo after a one-date answer", async () => {
  const dates = [occurrence({ recurrenceRule: "FREQ=WEEKLY" }), occurrence({ id: 2, date: "2026-12-08", recurrenceRule: "FREQ=WEEKLY" })];
  vi.mocked(Events.get).mockResolvedValue({ ...page, event: { ...page.event, recurrenceRule: "FREQ=WEEKLY" }, occurrences: dates });
  vi.mocked(Events.registerSeries).mockResolvedValue({
    eventId: 7, scope: "all_future", status: "maybe",
    results: dates.map((row) => ({ occurrenceId: row.id, date: row.date, before: {}, after: { registrationId: row.id + 10, status: "maybe" as const } })),
    refusals: [], undoToken: "undo-token", undoExpiresAt: new Date(Date.now() + 10_000).toISOString(),
  });
  vi.mocked(Events.register).mockResolvedValue({ registration: {
    registrationId: 12, userId: 2, eventId: 7, occurrenceId: 2, date: "2026-12-08", status: "going",
    guestCount: 0, childcareCount: 0, childcareAgesNote: "", waitlistPosition: null, attendanceMode: "in_person",
  }, people: [] });
  renderPage();
  const answerGroups = await screen.findAllByRole("group", { name: "Your answer" });
  await answerAll(answerGroups[0], "Maybe");
  expect(await screen.findByRole("button", { name: "Undo" })).toBeInTheDocument();
  await userEvent.click(within(answerGroups[1]).getByRole("button", { name: "Going" }));
  await userEvent.click(screen.getByRole("button", { name: "Just this date" }));
  await waitFor(() => expect(screen.queryByRole("button", { name: "Undo" })).not.toBeInTheDocument());
});

it("uses series Apply results before its parent GET and rejects that older GET after a sibling save", async () => {
  const first = occurrence({ recurrenceRule: "FREQ=WEEKLY", remoteAvailable: true, remoteCapacity: 10,
    childcareAvailable: true, guestsAllowed: 2, myRegistration: "going", myAttendanceMode: "remote",
    myGuestCount: 2, myChildcareCount: 2, myChildcareAgesNote: "4 and 6" });

  const second = occurrence({ id: 2, date: "2026-12-08", recurrenceRule: "FREQ=WEEKLY", remoteAvailable: true,
    remoteCapacity: 10, childcareAvailable: true, guestsAllowed: 2, myRegistration: "going",
    myAttendanceMode: "in_person", myGuestCount: 0, myChildcareCount: 0, myChildcareAgesNote: "" });

  const stalePage = { ...page, event: { ...page.event, recurrenceRule: "FREQ=WEEKLY", childcareAvailable: true,
    guestsAllowed: 2, remoteAvailable: true, remoteCapacity: 10 }, occurrences: [first, second] };

  const appliedRows = [first, second].map((row) => ({ ...row, myAttendanceMode: "remote" as const,
    myGuestCount: 2, myChildcareCount: 2, myChildcareAgesNote: "4 and 6" }));

  const appliedPage = { ...stalePage, occurrences: appliedRows };
  const held = deferred<EventDetailResponse>();
  vi.mocked(Events.get).mockResolvedValueOnce(stalePage).mockReturnValueOnce(held.promise).mockResolvedValue(appliedPage);
  vi.mocked(Events.registerSeries).mockResolvedValue({
    eventId: 7, scope: "all_future", status: "going",
    results: appliedRows.map((row) => ({ occurrenceId: row.id, date: row.date, before: {}, after: {
      registrationId: row.id + 10, status: "going", attendanceMode: "remote", guestCount: 2,
      childcareCount: 2, childcareAgesNote: "4 and 6",
    } })), refusals: [],
  });
  vi.mocked(Events.register).mockResolvedValue({ registration: {
    registrationId: 12, userId: 2, eventId: 7, occurrenceId: 2, date: second.date, status: "going",
    guestCount: 2, childcareCount: 2, childcareAgesNote: "4 and 6", waitlistPosition: null, attendanceMode: "remote",
  }, people: [] });

  renderPage();
  const cards = await screen.findAllByRole("group", { name: "Your answer" });
  await answerAll(cards[0], "Going");
  await waitFor(() => expect(Events.get).toHaveBeenCalledTimes(2));
  const sibling = within(cards[1].parentElement!.parentElement!);
  expect(sibling.getByRole("radio", { name: "Remote" })).toBeChecked();
  expect(sibling.getByLabelText("Extra guests")).toHaveValue(2);
  // Childcare is folded into one summary line until changed (DEC-92).
  expect(sibling.getByText(/Childcare for 2 \(ages 4 and 6\)/)).toBeInTheDocument();
  await userEvent.click(sibling.getByRole("button", { name: "Change childcare request" }));
  expect(sibling.getByLabelText(/^Children needing childcare/)).toHaveValue("2");
  expect(sibling.getByLabelText(/^Ages/)).toHaveValue("4 and 6");

  await userEvent.click(sibling.getByRole("button", { name: "Save childcare" }));
  await userEvent.click(screen.getByRole("button", { name: "Just this date" }));
  expect(Events.register).toHaveBeenLastCalledWith(7, {
    status: "going", date: second.date, guestCount: 2, attendanceMode: "remote",
    childcareCount: 2, childcareAgesNote: "4 and 6",
  });
  await waitFor(() => expect(Events.get).toHaveBeenCalledTimes(3));
  await act(async () => held.resolve(stalePage));
  expect(sibling.getByRole("radio", { name: "Remote" })).toBeChecked();
  expect(sibling.getByLabelText("Extra guests")).toHaveValue(2);
});

it("maps series Undo to not answered before its parent GET and cannot stale-save applied values", async () => {
  const appliedRows = [
    occurrence({ recurrenceRule: "FREQ=WEEKLY", remoteAvailable: true, remoteCapacity: 10, childcareAvailable: true,
      guestsAllowed: 2, myRegistration: "going", myAttendanceMode: "remote", myGuestCount: 2,
      myChildcareCount: 2, myChildcareAgesNote: "4 and 6" }),
    occurrence({ id: 2, date: "2026-12-08", recurrenceRule: "FREQ=WEEKLY", remoteAvailable: true,
      remoteCapacity: 10, childcareAvailable: true, guestsAllowed: 2, myRegistration: "going",
      myAttendanceMode: "remote", myGuestCount: 2, myChildcareCount: 2, myChildcareAgesNote: "4 and 6" }),
  ];

  const appliedPage = { ...page, event: { ...page.event, recurrenceRule: "FREQ=WEEKLY", childcareAvailable: true,
    guestsAllowed: 2, remoteAvailable: true, remoteCapacity: 10 }, occurrences: appliedRows };

  const maybeRows = appliedRows.map((row, index) => index === 1 ? ({ ...row, myRegistration: "maybe" as const,
    myAttendanceMode: "in_person" as const, myGuestCount: 0, myChildcareCount: 0, myChildcareAgesNote: "" }) : row);

  const held = deferred<EventDetailResponse>();
  vi.mocked(Events.get).mockResolvedValueOnce(appliedPage).mockResolvedValueOnce(appliedPage)
    .mockReturnValueOnce(held.promise).mockResolvedValue({ ...appliedPage, occurrences: maybeRows });
  vi.mocked(Events.registerSeries).mockResolvedValue({
    eventId: 7, scope: "all_future", status: "going",
    results: appliedRows.map((row) => ({ occurrenceId: row.id, date: row.date, before: {}, after: {
      registrationId: row.id + 10, status: "going", attendanceMode: "remote", guestCount: 2,
      childcareCount: 2, childcareAgesNote: "4 and 6",
    } })), refusals: [], undoToken: "undo-token", undoExpiresAt: new Date(Date.now() + 10_000).toISOString(),
  });
  vi.mocked(Events.undoSeriesRegistration).mockResolvedValue({
    eventId: 7, scope: "", status: "", results: appliedRows.map((row) => ({
      occurrenceId: row.id, date: row.date, before: { status: "going" as const }, after: {},
    })), refusals: [],
  });
  vi.mocked(Events.register).mockResolvedValue({ registration: {
    registrationId: 12, userId: 2, eventId: 7, occurrenceId: 2, date: appliedRows[1].date, status: "maybe",
    guestCount: 0, childcareCount: 0, childcareAgesNote: "", waitlistPosition: null, attendanceMode: "in_person",
  }, people: [] });

  renderPage();
  const cards = await screen.findAllByRole("group", { name: "Your answer" });
  const first = within(cards[0].parentElement!.parentElement!);
  const sibling = within(cards[1].parentElement!.parentElement!);
  await answerAll(cards[0], "Going");
  await userEvent.click(await first.findByRole("button", { name: "Undo" }));
  await waitFor(() => expect(Events.get).toHaveBeenCalledTimes(3));

  for (const name of ["Going", "Maybe", "Can't go"]) {
    expect(sibling.getByRole("button", { name })).toHaveAttribute("aria-pressed", "false");
  }

  expect(sibling.queryByLabelText(/^Children needing childcare/)).not.toBeInTheDocument();

  await userEvent.click(sibling.getByRole("button", { name: "Maybe" }));
  await userEvent.click(screen.getByRole("button", { name: "Just this date" }));
  expect(Events.register).toHaveBeenLastCalledWith(7, {
    status: "maybe", date: appliedRows[1].date, guestCount: 0, attendanceMode: "in_person",
  });
  await waitFor(() => expect(Events.get).toHaveBeenCalledTimes(4));
  await act(async () => held.resolve(appliedPage));
  expect(sibling.getByRole("button", { name: "Maybe" })).toHaveAttribute("aria-pressed", "true");
  expect(sibling.getByRole("radio", { name: "In person" })).toBeChecked();
});

it("uses series Apply results in household forms while the parent GET is held and preserves proxy drafts", async () => {
  vi.mocked(Households.mine).mockResolvedValue({ household: { id: 1, name: "Family", address: "", createdBy: 2,
    accounts: [], people: [{ id: 3, householdId: 1, canConvert: false, firstName: "Mary", lastName: "", name: "Mary",
      kind: "adult", photoConsent: false, linkStatus: "none", registrationEligibility: "eligible", createdBy: 2 }] } });

  const first = occurrence({ recurrenceRule: "FREQ=WEEKLY", remoteAvailable: true, remoteCapacity: 10,
    childcareAvailable: true, guestsAllowed: 2, myRegistration: "going", myAttendanceMode: "remote",
    myGuestCount: 2, myChildcareCount: 2, myChildcareAgesNote: "4 and 6" });

  const second = occurrence({ id: 2, date: "2026-12-08", recurrenceRule: "FREQ=WEEKLY", remoteAvailable: true,
    remoteCapacity: 10, childcareAvailable: true, guestsAllowed: 2, myRegistration: "going",
    myAttendanceMode: "in_person", myGuestCount: 0, myChildcareCount: 0, myChildcareAgesNote: "" });

  const stalePage = { ...page, event: { ...page.event, recurrenceRule: "FREQ=WEEKLY", childcareAvailable: true,
    guestsAllowed: 2, remoteAvailable: true, remoteCapacity: 10 }, occurrences: [first, second] };

  const appliedRows = [first, second].map((row) => ({ ...row, myAttendanceMode: "remote" as const,
    myGuestCount: 2, myChildcareCount: 2, myChildcareAgesNote: "4 and 6" }));

  const appliedPage = { ...stalePage, occurrences: appliedRows };
  const held = deferred<EventDetailResponse>();
  vi.mocked(Events.get).mockResolvedValueOnce(stalePage).mockReturnValueOnce(held.promise).mockResolvedValue(appliedPage);
  vi.mocked(Events.registerSeries).mockResolvedValue({
    eventId: 7, scope: "all_future", status: "going",
    results: appliedRows.map((row) => ({ occurrenceId: row.id, date: row.date, before: {}, after: {
      registrationId: row.id + 10, status: "going", attendanceMode: "remote", guestCount: 2,
      childcareCount: 2, childcareAgesNote: "4 and 6",
    } })), refusals: [],
  });
  vi.mocked(Events.register).mockResolvedValue({ registration: {
    registrationId: 12, userId: 2, eventId: 7, occurrenceId: 2, date: second.date, status: "going",
    guestCount: 2, childcareCount: 2, childcareAgesNote: "4 and 6", waitlistPosition: null, attendanceMode: "remote",
  }, people: [] });

  renderPage();
  const forms = await screen.findAllByRole("form", { name: "Household answers" });
  const firstForm = within(forms[0]);
  const sibling = within(forms[1]);
  await userEvent.click(firstForm.getByRole("checkbox", { name: "Mary" }));
  await userEvent.selectOptions(firstForm.getByLabelText("Answer for Mary"), "maybe");
  await chooseDatesForMe(forms[0]);
  await waitFor(() => expect(Events.get).toHaveBeenCalledTimes(2));
  expect(within(sibling.getByRole("group", { name: "How you will attend" })).getByRole("radio", { name: "Remote" })).toBeChecked();
  expect(sibling.getByLabelText("Extra guests")).toHaveValue(2);
  expect(sibling.getByLabelText(/^Other children needing childcare/)).toHaveValue("2");
  expect(sibling.getByRole("textbox", { name: /^Ages/ })).toHaveValue("4 and 6");
  expect(firstForm.getByRole("checkbox", { name: "Mary" })).toBeChecked();
  expect(firstForm.getByLabelText("Answer for Mary")).toHaveValue("maybe");

  await userEvent.click(sibling.getByRole("button", { name: "Save answers" }));
  expect(Events.register).toHaveBeenLastCalledWith(7, {
    date: second.date, people: [], status: "going", attendanceMode: "remote", guestCount: 2,
    childcareCount: 2, childcareAgesNote: "4 and 6",
  });
  await waitFor(() => expect(Events.get).toHaveBeenCalledTimes(3));
  await act(async () => held.resolve(stalePage));
  expect(within(sibling.getByRole("group", { name: "How you will attend" })).getByRole("radio", { name: "Remote" })).toBeChecked();
  expect(firstForm.getByRole("checkbox", { name: "Mary" })).toBeChecked();
});

it("maps series Undo to no answer in household forms before the parent GET and saves fresh defaults", async () => {
  vi.mocked(Households.mine).mockResolvedValue({ household: { id: 1, name: "Family", address: "", createdBy: 2,
    accounts: [], people: [{ id: 3, householdId: 1, canConvert: false, firstName: "Mary", lastName: "", name: "Mary",
      kind: "adult", photoConsent: false, linkStatus: "none", registrationEligibility: "eligible", createdBy: 2 }] } });

  const appliedRows = [
    occurrence({ recurrenceRule: "FREQ=WEEKLY", remoteAvailable: true, remoteCapacity: 10, childcareAvailable: true,
      guestsAllowed: 2, myRegistration: "going", myAttendanceMode: "remote", myGuestCount: 2,
      myChildcareCount: 2, myChildcareAgesNote: "4 and 6" }),
    occurrence({ id: 2, date: "2026-12-08", recurrenceRule: "FREQ=WEEKLY", remoteAvailable: true,
      remoteCapacity: 10, childcareAvailable: true, guestsAllowed: 2, myRegistration: "going",
      myAttendanceMode: "remote", myGuestCount: 2, myChildcareCount: 2, myChildcareAgesNote: "4 and 6" }),
  ];

  const appliedPage = { ...page, event: { ...page.event, recurrenceRule: "FREQ=WEEKLY", childcareAvailable: true,
    guestsAllowed: 2, remoteAvailable: true, remoteCapacity: 10 }, occurrences: appliedRows };

  const savedRows = appliedRows.map((row, index) => index === 1 ? ({ ...row, myRegistration: "going" as const,
    myAttendanceMode: "in_person" as const, myGuestCount: 0, myChildcareCount: 0, myChildcareAgesNote: "" }) : row);

  const held = deferred<EventDetailResponse>();
  vi.mocked(Events.get).mockResolvedValueOnce(appliedPage).mockResolvedValueOnce(appliedPage)
    .mockReturnValueOnce(held.promise).mockResolvedValue({ ...appliedPage, occurrences: savedRows });
  vi.mocked(Events.registerSeries).mockResolvedValue({
    eventId: 7, scope: "all_future", status: "going",
    results: appliedRows.map((row) => ({ occurrenceId: row.id, date: row.date, before: {}, after: {
      registrationId: row.id + 10, status: "going", attendanceMode: "remote", guestCount: 2,
      childcareCount: 2, childcareAgesNote: "4 and 6",
    } })), refusals: [], undoToken: "undo-token", undoExpiresAt: new Date(Date.now() + 10_000).toISOString(),
  });
  vi.mocked(Events.undoSeriesRegistration).mockResolvedValue({
    eventId: 7, scope: "", status: "", results: appliedRows.map((row) => ({
      occurrenceId: row.id, date: row.date, before: { status: "going" as const }, after: {},
    })), refusals: [],
  });
  vi.mocked(Events.register).mockResolvedValue({ registration: {
    registrationId: 12, userId: 2, eventId: 7, occurrenceId: 2, date: appliedRows[1].date, status: "going",
    guestCount: 0, childcareCount: 0, childcareAgesNote: "", waitlistPosition: null, attendanceMode: "in_person",
  }, people: [] });

  renderPage();
  const forms = await screen.findAllByRole("form", { name: "Household answers" });
  const first = within(forms[0]);
  const sibling = within(forms[1]);
  await userEvent.click(first.getByRole("checkbox", { name: "Mary" }));
  await userEvent.selectOptions(first.getByLabelText("Answer for Mary"), "maybe");
  await chooseDatesForMe(forms[0]);
  await userEvent.click(await first.findByRole("button", { name: "Undo" }));
  await waitFor(() => expect(Events.get).toHaveBeenCalledTimes(3));
  expect(sibling.getByRole("checkbox", { name: "Me" })).not.toBeChecked();
  expect(first.getByRole("checkbox", { name: "Mary" })).toBeChecked();
  expect(first.getByLabelText("Answer for Mary")).toHaveValue("maybe");

  await userEvent.click(sibling.getByRole("checkbox", { name: "Me" }));
  await userEvent.click(sibling.getByRole("button", { name: "Save answers" }));
  expect(Events.register).toHaveBeenLastCalledWith(7, {
    date: appliedRows[1].date, people: [], status: "going", attendanceMode: "in_person", guestCount: 0,
    childcareCount: 0, childcareAgesNote: "",
  });
  await waitFor(() => expect(Events.get).toHaveBeenCalledTimes(4));
  await act(async () => held.resolve(appliedPage));
  expect(sibling.getByRole("checkbox", { name: "Me" })).toBeChecked();
  expect(within(sibling.getByRole("group", { name: "How you will attend" })).getByRole("radio", { name: "In person" })).toBeChecked();
});

it("clears protected event data while a terminal Undo refresh is still pending", async () => {
  const recurring = { ...page, event: { ...page.event, recurrenceRule: "FREQ=WEEKLY" }, occurrences: [occurrence({ recurrenceRule: "FREQ=WEEKLY" })] };
  const terminal = deferred<EventDetailResponse>();
  vi.mocked(Events.get)
    .mockResolvedValueOnce(recurring)
    .mockResolvedValueOnce(recurring)
    .mockReturnValueOnce(terminal.promise);
  vi.mocked(Events.registerSeries).mockResolvedValue({
    eventId: 7, scope: "all_future", status: "maybe",
    results: [{ occurrenceId: 1, date: "2026-12-01", before: {}, after: { registrationId: 4, status: "maybe" } }],
    refusals: [], undoToken: "undo-token", undoExpiresAt: new Date(Date.now() + 10_000).toISOString(),
  });
  vi.mocked(Events.undoSeriesRegistration).mockRejectedValue(new ApiError(409, "revoked", { code: "undo_authority_revoked" }));
  vi.mocked(Households.mine).mockResolvedValue({ household: null });
  renderPage();
  await answerAll(await screen.findByRole("group", { name: "Your answer" }), "Maybe");
  await userEvent.click(await screen.findByRole("button", { name: "Undo" }));
  await waitFor(() => expect(Events.get).toHaveBeenCalledTimes(3));
  expect(screen.queryByRole("heading", { name: "Hybrid study" })).not.toBeInTheDocument();
  await act(async () => terminal.reject(new ApiError(404, "hidden", { code: "not_found" })));
  expect(await screen.findByRole("heading", { name: "Event not found" })).toBeInTheDocument();
});

it("keeps the newest event refresh when an older quiet request resolves last", async () => {
  const recurring = { ...page, event: { ...page.event, recurrenceRule: "FREQ=WEEKLY" }, occurrences: [occurrence({ recurrenceRule: "FREQ=WEEKLY" })] };
  const old = deferred<EventDetailResponse>();
  const terminal = deferred<EventDetailResponse>();
  const stale = { ...recurring, event: { ...recurring.event, title: "Stale event details" } };
  const current = { ...recurring, event: { ...recurring.event, title: "Current event details" } };
  vi.mocked(Events.get)
    .mockResolvedValueOnce(recurring)
    .mockReturnValueOnce(old.promise)
    .mockReturnValueOnce(terminal.promise);
  vi.mocked(Events.registerSeries).mockResolvedValue({
    eventId: 7, scope: "all_future", status: "maybe",
    results: [{ occurrenceId: 1, date: "2026-12-01", before: {}, after: { registrationId: 4, status: "maybe" } }],
    refusals: [], undoToken: "undo-token", undoExpiresAt: new Date(Date.now() + 10_000).toISOString(),
  });
  vi.mocked(Events.undoSeriesRegistration).mockRejectedValue(new ApiError(409, "changed", { code: "undo_changed" }));
  vi.mocked(Households.mine).mockResolvedValue({ household: null });
  renderPage();
  await answerAll(await screen.findByRole("group", { name: "Your answer" }), "Maybe");
  await userEvent.click(await screen.findByRole("button", { name: "Undo" }));
  await waitFor(() => expect(Events.get).toHaveBeenCalledTimes(3));
  expect(screen.queryByRole("heading", { name: "Hybrid study" })).not.toBeInTheDocument();
  await act(async () => terminal.resolve(current));
  expect(await screen.findByRole("heading", { name: "Current event details" })).toBeInTheDocument();
  await act(async () => old.resolve(stale));
  expect(screen.getByRole("heading", { name: "Current event details" })).toBeInTheDocument();
  expect(screen.queryByRole("heading", { name: "Stale event details" })).not.toBeInTheDocument();
});

it("shows the public childcare ages and notes", async () => {
  vi.mocked(Events.get).mockResolvedValue({ ...page, event: { ...page.event, childcareAvailable: true, childcareAges: "2–5", childcareNotes: "Check in at the nursery." } });
  renderPage();
  expect(await screen.findByText("Childcare available · 2–5")).toBeInTheDocument();
  expect(screen.getByText("Check in at the nursery.")).toBeInTheDocument();
});

it("shares one household fetch across dates and refreshes server state after child-only answers", async () => {
  vi.mocked(Households.mine).mockResolvedValue({ household: { id: 1, name: "Family", address: "", createdBy: 2, accounts: [], people: [{ id: 3, householdId: 1, canConvert: false, firstName: "Mary", lastName: "", name: "Mary", kind: "adult", photoConsent: false, linkStatus: "none", registrationEligibility: "eligible", createdBy: 2 }] } });
  vi.mocked(Events.get).mockResolvedValue({ ...page, occurrences: [page.occurrences[0], { ...page.occurrences[0], id: 2, date: "2026-12-08" }] });
  vi.mocked(Events.register).mockResolvedValue({ registration: null, people: [{ registrationId: 12, personId: 3, registeredBy: 2, status: "going", attendanceMode: "in_person", waitlistPosition: null, childcareRequested: false }] });
  const before = vi.mocked(Events.get).mock.calls.length;
  const householdBefore = vi.mocked(Households.mine).mock.calls.length;
  renderPage(); const editors = await screen.findAllByRole("form", { name: "Household answers" });
  await userEvent.click(within(editors[0]).getByRole("checkbox", { name: "Mary" }));
  await userEvent.click(within(editors[0]).getByRole("button", { name: "Save answers" }));
  await waitFor(() => expect(Events.get).toHaveBeenCalledTimes(before + 2));
  expect(Households.mine).toHaveBeenCalledTimes(householdBefore + 1);
});

describe("EventDetail when line and date actions (DEC-92, DEC-93)", () => {
  // Noon UTC keeps the day and weekday the same in any real zone.
  const noon = { startsAt: "2026-12-01T12:00:00Z", endsAt: "2026-12-01T13:30:00Z", startsLocal: "2026-12-01T12:00", endsLocal: "2026-12-01T13:30", tzid: "UTC" };
  const series = { ...page.event, ...noon, recurrenceRule: "FREQ=WEEKLY;BYDAY=TU" };

  beforeEach(() => {
    vi.resetAllMocks();
    guarded();
    vi.mocked(Households.mine).mockResolvedValue({ household: null });
  });

  it("says when under the title, drops the list heading and slims the card for a single date", async () => {
    vi.mocked(Events.get).mockResolvedValue({ ...page, event: { ...page.event, ...noon }, occurrences: [occurrence({ ...noon, title: "Hybrid study", location: "Hall" })], canManage: false });
    renderPage();
    expect(await screen.findByText(/^Tuesday, Dec 1(, \d{4})? · \d/)).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Upcoming dates" })).not.toBeInTheDocument();
    // The card does not say the title or place again; only the header does.
    const card = screen.getByRole("article", { name: "Hybrid study" });
    expect(within(card).getByRole("heading", { name: "Hybrid study", level: 2 })).toHaveClass("sr-only");
    expect(within(card).queryByText("Hall")).not.toBeInTheDocument();
    expect(screen.getAllByText("Hall")).toHaveLength(1);
    expect(within(card).getByRole("group", { name: "Your answer" })).toBeInTheDocument();
    // One set of date actions, in the header, linking to the event page.
    expect(screen.getAllByRole("link", { name: /^Add to calendar/ })).toHaveLength(1);
    expect(screen.getByRole("link", { name: "Add to calendar" })).toHaveAttribute("href", "/api/v1/events/7/dates/2026-12-01/calendar.ics");
    expect(within(card).queryByRole("link", { name: /^Add to calendar/ })).not.toBeInTheDocument();
  });

  it("keeps a single date's leader More menu", async () => {
    vi.mocked(Events.get).mockResolvedValue({ ...page, event: { ...page.event, ...noon }, occurrences: [occurrence(noon)], canManage: true });
    renderPage();
    expect(await screen.findByRole("button", { name: "More for 2026-12-01" })).toBeInTheDocument();
  });

  it("shows a date's own title or place when it differs from the header", async () => {
    vi.mocked(Events.get).mockResolvedValue({ ...page, event: { ...page.event, ...noon }, occurrences: [occurrence({ ...noon, title: "Hybrid study, special", location: "Chapel" })] });
    renderPage();
    const card = await screen.findByRole("article", { name: "Hybrid study, special" });
    expect(within(card).getByText("Chapel")).toBeInTheDocument();
  });

  it("describes a series and keeps the list, with actions inside each card", async () => {
    const dates = [occurrence({ ...noon, recurrenceRule: series.recurrenceRule }), occurrence({ id: 2, date: "2026-12-08", startsAt: "2026-12-08T12:00:00Z", endsAt: "2026-12-08T13:30:00Z", recurrenceRule: series.recurrenceRule, tzid: "UTC" })];
    vi.mocked(Events.get).mockResolvedValue({ ...page, event: series, occurrences: dates, canManage: false });
    renderPage();
    expect(await screen.findByText(/^Every Tuesday · .+ · next Dec 1/)).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Upcoming dates" })).toBeInTheDocument();
    const links = screen.getAllByRole("link", { name: /^Add to calendar/ });
    expect(links.map((l) => l.getAttribute("href"))).toEqual([
      "/api/v1/events/7/dates/2026-12-01/calendar.ics",
      "/api/v1/events/7/dates/2026-12-08/calendar.ics",
    ]);

    for (const link of links) expect(link.closest("article")).not.toBeNull();
  });

  it("keeps the list for an event with no recurrence rule but several dates", async () => {
    vi.mocked(Events.get).mockResolvedValue({ ...page, event: { ...page.event, ...noon }, occurrences: [occurrence(noon), occurrence({ id: 2, date: "2026-12-08", startsAt: "2026-12-08T12:00:00Z", endsAt: "2026-12-08T13:00:00Z", tzid: "UTC" })] });
    renderPage();
    expect(await screen.findByRole("heading", { name: "Upcoming dates" })).toBeInTheDocument();
  });

  it("calls a church-wide event church-wide", async () => {
    vi.mocked(Events.get).mockResolvedValue({ ...page, event: { ...page.event, ...noon, churchWide: true, hostedBy: "Fold" } });
    renderPage();
    expect(await screen.findByText("Church-wide event")).toBeInTheDocument();
    expect(screen.queryByText(/Hosted by/)).not.toBeInTheDocument();
  });
});

describe("EventDetail header actions and ?manage (DEC-99)", () => {
  beforeEach(() => {
    vi.resetAllMocks();
    guarded();
    vi.mocked(Households.mine).mockResolvedValue({ household: null });
  });

  it("puts Edit series and More beside the title as secondary buttons for a manager", async () => {
    vi.mocked(Events.get).mockResolvedValue({ ...page, event: { ...page.event, recurrenceRule: "FREQ=WEEKLY" }, occurrences: [occurrence({ recurrenceRule: "FREQ=WEEKLY" })] });
    renderPage();
    const edit = await screen.findByRole("button", { name: "Edit series" });
    expect(edit).toHaveClass("btn-secondary");
    const title = screen.getByRole("heading", { level: 1, name: "Hybrid study" });
    expect(title.parentElement?.parentElement).toContainElement(edit);
    const more = screen.getByRole("button", { name: "More event actions" });
    expect(more).toHaveClass("btn-secondary");
    expect(title.parentElement?.parentElement).toContainElement(more);
    await userEvent.click(more);
    expect(screen.getByRole("menuitem", { name: "Delete event" })).toBeInTheDocument();
  });

  it("calls the button Edit event for a single date and toggles the editor", async () => {
    vi.mocked(Events.get).mockResolvedValue(page);
    renderPage();
    const edit = await screen.findByRole("button", { name: "Edit event" });
    expect(screen.queryByRole("link", { name: "Edit event" })).not.toBeInTheDocument();
    expect(edit).toHaveAttribute("aria-expanded", "false");
    await userEvent.click(edit);
    expect(screen.getByRole("button", { name: "Edit event" })).toHaveAttribute("aria-expanded", "true");
    expect(screen.queryByRole("button", { name: "Close editor" })).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Edit event" }));
    expect(screen.getByRole("button", { name: "Edit event" })).toHaveAttribute("aria-expanded", "false");
  });

  it("scrolls the editor into view and focuses its first field when it opens", async () => {
    vi.mocked(Events.get).mockResolvedValue(page);
    const scroll = vi.fn();
    Element.prototype.scrollIntoView = scroll;
    renderPage();
    await userEvent.click(await screen.findByRole("button", { name: "Edit event" }));
    expect(scroll).toHaveBeenCalled();
    expect(screen.getByLabelText("Title")).toHaveFocus();
  });

  it("keeps focus off the page body while a saved series reads the page again, then gives it to Edit event", async () => {
    vi.mocked(Events.get).mockResolvedValue(page);
    const put = deferred<{ event: typeof page.event }>();
    vi.mocked(Events.update).mockReturnValueOnce(put.promise);
    render(<main id="main" tabIndex={-1}><MemoryRouter initialEntries={["/events/7"]}><DateWritesProvider credential={credential}><Routes><Route path="/events/:id" element={<EventDetail />} /></Routes></DateWritesProvider></MemoryRouter></main>);
    await userEvent.click(await screen.findByRole("button", { name: "Edit event" }));
    const title = screen.getByLabelText("Title");
    await userEvent.type(title, "{Enter}");
    expect(title).toBeDisabled();
    expect(document.activeElement).toBe(title.closest("form"));
    expect(screen.getByRole("button", { name: "Edit event" })).toBeDisabled();
    const again = deferred<typeof page>();
    vi.mocked(Events.get).mockReturnValueOnce(again.promise);
    await act(async () => { put.resolve({ event: page.event }); await put.promise; });
    expect(document.activeElement).toBe(document.getElementById("main"));
    await act(async () => { again.resolve(page); await again.promise; });
    const edit = await screen.findByRole("button", { name: "Edit event" });
    expect(edit).toHaveAttribute("aria-expanded", "false");
    expect(document.activeElement).toBe(edit);
  });

  it("leaves focus where the person moved it while a series save was on its way", async () => {
    vi.mocked(Events.get).mockResolvedValue(page);
    const put = deferred<{ event: typeof page.event }>();
    vi.mocked(Events.update).mockReturnValueOnce(put.promise);
    render(<main id="main" tabIndex={-1}><button>Elsewhere</button><MemoryRouter initialEntries={["/events/7"]}><DateWritesProvider credential={credential}><Routes><Route path="/events/:id" element={<EventDetail />} /></Routes></DateWritesProvider></MemoryRouter></main>);
    await userEvent.click(await screen.findByRole("button", { name: "Edit event" }));
    await userEvent.type(screen.getByLabelText("Title"), "{Enter}");
    const elsewhere = screen.getByRole("button", { name: "Elsewhere" });
    elsewhere.focus();
    await act(async () => { put.resolve({ event: page.event }); await put.promise; });
    await waitFor(() => expect(screen.getByRole("button", { name: "Edit event" })).toHaveAttribute("aria-expanded", "false"));
    expect(document.activeElement).toBe(elsewhere);
  });

  it("gives focus to Edit event when the save's reload removes the control the person moved to", async () => {
    vi.mocked(Events.get).mockResolvedValue(page);
    const put = deferred<{ event: typeof page.event }>();
    vi.mocked(Events.update).mockReturnValueOnce(put.promise);
    render(<main id="main" tabIndex={-1}><MemoryRouter initialEntries={["/events/7"]}><DateWritesProvider credential={credential}><Routes><Route path="/events/:id" element={<EventDetail />} /></Routes></DateWritesProvider></MemoryRouter></main>);
    await userEvent.click(await screen.findByRole("button", { name: "Edit event" }));
    await userEvent.type(screen.getByLabelText("Title"), "{Enter}");
    screen.getByRole("button", { name: "More event actions" }).focus();
    await act(async () => { put.resolve({ event: page.event }); await put.promise; });
    const edit = await screen.findByRole("button", { name: "Edit event" });
    await waitFor(() => expect(edit).toHaveAttribute("aria-expanded", "false"));
    await waitFor(() => expect(document.activeElement).toBe(screen.getByRole("button", { name: "Edit event" })));
  });

  it("puts Edit and More together in one row", async () => {
    vi.mocked(Events.get).mockResolvedValue(page);
    renderPage();
    const edit = await screen.findByRole("button", { name: "Edit event" });
    const more = screen.getByRole("button", { name: "More event actions" });
    const row = requiredElement(edit.parentElement, HTMLElement);
    expect(row).toContainElement(more);
  });

  it("shows neither to someone who cannot manage the event", async () => {
    vi.mocked(Events.get).mockResolvedValue({ ...page, canManage: false });
    renderPage();
    await screen.findByRole("heading", { level: 1, name: "Hybrid study" });
    expect(screen.queryByRole("button", { name: /^Edit (series|event)$/ })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "More event actions" })).not.toBeInTheDocument();
  });

  it("opens Manage sign-ups for the ?manage date and scrolls it into view", async () => {
    vi.mocked(Events.get).mockResolvedValue(page);
    const scroll = vi.fn();
    Element.prototype.scrollIntoView = scroll;
    renderPage("/events/7?manage=2026-12-01");
    expect(await screen.findByRole("region", { name: "Manage sign-ups" })).toBeInTheDocument();
    await waitFor(() => expect(scroll).toHaveBeenCalledTimes(1));
    expect(scroll.mock.contexts[0]).toBe(screen.getByRole("region", { name: "Manage sign-ups" }).parentElement);
  });

  it("ignores ?manage for someone who cannot manage, or for a date that is not listed", async () => {
    vi.mocked(Events.get).mockResolvedValue({ ...page, canManage: false });
    renderPage("/events/7?manage=2026-12-01");
    await screen.findByRole("heading", { level: 1, name: "Hybrid study" });
    expect(screen.queryByRole("region", { name: "Manage sign-ups" })).not.toBeInTheDocument();
    cleanup();
    vi.mocked(Events.get).mockResolvedValue(page);
    renderPage("/events/7?manage=2031-01-01");
    await screen.findByRole("button", { name: "More for 2026-12-01" });
    expect(screen.queryByRole("region", { name: "Manage sign-ups" })).not.toBeInTheDocument();
  });
});

describe("EventDetail shows a booked room as the place (M2.1b)", () => {
  const room = { id: 1, name: "Fellowship Hall", building: "Main" };
  beforeEach(() => {
    vi.resetAllMocks();
    guarded();
    vi.mocked(Households.mine).mockResolvedValue({ household: null });
  });

  it("puts the room in a single date's header, with the typed location after it", async () => {
    vi.mocked(Events.get).mockResolvedValue({ ...page, occurrences: [occurrence({ room })], canManage: false });
    renderPage();
    expect(await screen.findByText("Fellowship Hall · Main · Hall")).toBeInTheDocument();
  });

  it("puts the room on a series date's card and leaves the series header's typed location", async () => {
    vi.mocked(Events.get).mockResolvedValue({
      ...page, event: { ...page.event, recurrenceRule: "FREQ=WEEKLY;BYDAY=TU" },
      occurrences: [occurrence({ recurrenceRule: "FREQ=WEEKLY;BYDAY=TU", room }), occurrence({ id: 2, date: "2026-12-08", recurrenceRule: "FREQ=WEEKLY;BYDAY=TU" })],
      canManage: false,
    });
    renderPage();
    const cards = await screen.findAllByRole("article");
    expect(within(cards[0]).getByText(/ · Fellowship Hall · Main · Hall$/)).toBeInTheDocument();
    expect(within(cards[1]).getByText(/ · Hall$/)).not.toHaveTextContent("Fellowship Hall");
    // The header names the series' typed location only.
    expect(screen.getByText("Hall", { selector: "p" })).toBeInTheDocument();
  });

  it("shows the typed location exactly as before without a room", async () => {
    vi.mocked(Events.get).mockResolvedValue({ ...page, canManage: false });
    renderPage();
    expect(await screen.findByText("Hall")).toBeInTheDocument();
    expect(screen.queryByText(/Fellowship Hall/)).not.toBeInTheDocument();
  });
});

describe("EventDetail dates that need a room", () => {
  const series = {
    ...page,
    event: { ...page.event, recurrenceRule: "FREQ=WEEKLY;BYDAY=TU" },
    occurrences: [
      occurrence({ recurrenceRule: "FREQ=WEEKLY;BYDAY=TU" }),
      occurrence({ id: 2, date: "2026-12-08", startsLocal: "2026-12-08T19:00", recurrenceRule: "FREQ=WEEKLY;BYDAY=TU" }),
    ],
  };

  const needs = (...rows: NeedsRoom[]) =>
    vi.spyOn(SeriesRooms, "list").mockResolvedValue({ rooms: [], dates: 2, defaultRoom: null, needsRoom: rows, pending: 0 });

  beforeEach(() => {
    vi.resetAllMocks();
    vi.restoreAllMocks();
    guarded();
    vi.mocked(Households.mine).mockResolvedValue({ household: null });
    vi.mocked(Events.get).mockResolvedValue(series);
    vi.spyOn(Site, "get").mockResolvedValue({ name: "Fold", timezone: "UTC", selfSignup: "off", roomsEnabled: true, pushEnabled: true });
    vi.spyOn(DateRooms, "list").mockResolvedValue({ rooms: [], booking: null });
  });

  it("lists them for a manager and opens that date's editor from Pick a room", async () => {
    needs({ date: "2026-12-08", roomName: "Fellowship Hall", status: "requested" });
    renderPage();
    expect(await screen.findByRole("heading", { name: /Dates that need a room/ })).toBeInTheDocument();
    expect(SeriesRooms.list).toHaveBeenCalledWith(7);
    expect(screen.getByText("Fellowship Hall isn't free then.")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: /^Pick a room for / }));
    // The second date's own editor, with its own Room control.
    await waitFor(() => expect(DateRooms.list).toHaveBeenCalledWith(7, "2026-12-08"));
    expect(DateRooms.list).not.toHaveBeenCalledWith(7, "2026-12-01");
    const room = await screen.findByRole("combobox", { name: /^Room/ });
    expect(room).toBeInTheDocument();
    // The cursor goes to the Room control, not the date field.
    await waitFor(() => expect(room).toHaveFocus());
  });

  it("shows nothing when no date needs a room", async () => {
    needs();
    renderPage();
    await waitFor(() => expect(SeriesRooms.list).toHaveBeenCalled());
    await screen.findAllByRole("article");
    expect(screen.queryByRole("heading", { name: /Dates that need a room/ })).not.toBeInTheDocument();
  });

  it("is not shown to people who can't manage the event, and asks for nothing", async () => {
    needs({ date: "2026-12-08", roomName: "Fellowship Hall", status: "requested" });
    vi.mocked(Events.get).mockResolvedValue({ ...series, canManage: false });
    renderPage();
    await screen.findAllByRole("article");
    expect(screen.queryByRole("heading", { name: /Dates that need a room/ })).not.toBeInTheDocument();
    expect(SeriesRooms.list).not.toHaveBeenCalled();
  });

  it("asks again for them when a move saved before leaving the event and coming back answers", async () => {
    const list = needs();
    const answer = deferred<OccurrenceResponse>();
    edits.answer(answer.promise);

    const router = createMemoryRouter([
      { path: "/events/:id", element: <EventDetail /> },
      { path: "/", element: <p>Home page</p> },
    ], { initialEntries: ["/events/7"] });

    render(<DateWritesProvider credential={credential}><RouterProvider router={router} /></DateWritesProvider>);
    await userEvent.click(await screen.findByRole("button", { name: "More for 2026-12-08" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Edit this date" }));
    await screen.findByLabelText("Title");
    const editor = within(requiredElement(screen.getByLabelText("Title").closest("form"), HTMLFormElement));
    fireEvent.change(editor.getByLabelText("Date"), { target: { value: "2026-12-09" } });
    await waitFor(() => expect(editor.getByRole("button", { name: "Save this date" })).toBeEnabled());
    await userEvent.click(editor.getByRole("button", { name: "Save this date" }));
    await waitFor(() => expect(edits.sent).toHaveLength(1));
    expect(edits.sent[0]).toMatchObject({ url: "/events/7/occurrences/2026-12-08/edits", changes: { startsLocal: "2026-12-09T19:00" } });

    await act(() => router.navigate("/"));
    expect(await screen.findByText("Home page")).toBeInTheDocument();
    // A new page for the event, with the room list as it was before the move reached the server.
    await act(() => router.navigate("/events/7"));
    expect(await screen.findByRole("button", { name: "More for 2026-12-08" })).toBeInTheDocument();
    await waitFor(() => expect(list.mock.calls.length).toBeGreaterThanOrEqual(2));
    expect(screen.queryByRole("heading", { name: /Dates that need a room/ })).not.toBeInTheDocument();
    const asked = list.mock.calls.length;

    // The move lost the date's booking; its answer, held for the session, asks the new page again.
    list.mockResolvedValue({ rooms: [], dates: 2, defaultRoom: null, needsRoom: [{ date: "2026-12-08", roomName: "Fellowship Hall", status: "requested" }], pending: 0 });
    await act(async () => answer.resolve({ occurrence: { ...series.occurrences[1], revision: "r1" } }));
    expect(await screen.findByRole("heading", { name: /Dates that need a room/ })).toBeInTheDocument();
    expect(screen.getByText("Fellowship Hall isn't free then.")).toBeInTheDocument();
    expect(list).toHaveBeenCalledTimes(asked + 1);
  });

  it("asks nothing and shows nothing while rooms are off", async () => {
    needs({ date: "2026-12-08", roomName: "Fellowship Hall", status: "requested" });
    vi.mocked(Site.get).mockResolvedValue({ name: "Fold", timezone: "UTC", selfSignup: "off", roomsEnabled: false, pushEnabled: true });
    renderPage();
    await screen.findAllByRole("article");
    expect(screen.queryByRole("heading", { name: /Dates that need a room/ })).not.toBeInTheDocument();
    expect(SeriesRooms.list).not.toHaveBeenCalled();
  });
});

describe("Email people going (M3.4c)", () => {
  beforeEach(() => { vi.mocked(Households.mine).mockResolvedValue({ household: null }); });

  it("writes to the whole event from its menu, and to one date from that date's menu", async () => {
    composer.on = true;

    try {
      vi.mocked(Events.get).mockResolvedValue({ ...page, event: { ...page.event, recurrenceRule: "FREQ=WEEKLY" }, occurrences: [occurrence({ id: 41, recurrenceRule: "FREQ=WEEKLY" }), occurrence({ id: 42, date: "2026-12-08", recurrenceRule: "FREQ=WEEKLY" })] });
      const router = createMemoryRouter([{ path: "/events/:id", element: <EventDetail /> }, { path: "/messages/new", element: <p>Composer</p> }], { initialEntries: ["/events/7"] });
      render(<RouterProvider router={router} />);
      await userEvent.click(await screen.findByRole("button", { name: "More event actions" }));
      await userEvent.click(screen.getByRole("menuitem", { name: "Email people going" }));
      expect(router.state.location.search).toBe("?event=7");
      cleanup();
      const again = createMemoryRouter([{ path: "/events/:id", element: <EventDetail /> }, { path: "/messages/new", element: <p>Composer</p> }], { initialEntries: ["/events/7"] });
      render(<RouterProvider router={again} />);
      await userEvent.click(await screen.findByRole("button", { name: "More for 2026-12-08" }));
      await userEvent.click(screen.getByRole("menuitem", { name: "Email people going" }));
      expect(again.state.location.search).toBe("?occurrence=42");
    } finally {
      composer.on = false;
    }
  });

  it("isn't offered to someone who can't compose", async () => {
    vi.mocked(Events.get).mockResolvedValue(page);
    renderPage();
    await userEvent.click(await screen.findByRole("button", { name: "More event actions" }));
    expect(screen.queryByRole("menuitem", { name: "Email people going" })).not.toBeInTheDocument();
  });
});

describe("EventDetail hosts (M7.4, R4-H)", () => {
  const host = (userId: number, name: string) => ({ userId, name, email: `${name.toLowerCase()}@example.com` });
  const churchWide = (hosts: ReturnType<typeof host>[]): EventDetailResponse => ({ ...page, event: { ...page.event, churchWide: true, hosts } });

  beforeEach(() => {
    vi.resetAllMocks();
    guarded();
    vi.mocked(Households.mine).mockResolvedValue({ household: null });
    composer.admin = true;
  });
  afterEach(() => { composer.admin = false; });

  it("says nobody hosts the event, calmly, and keeps the Add host form", async () => {
    vi.mocked(Events.get).mockResolvedValue(churchWide([]));
    renderPage();
    const line = await screen.findByText("No one hosts this event right now. Add a host below.");
    expect(line).not.toHaveAttribute("role", "alert");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(screen.getByLabelText("Host email", { exact: false })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Add host" })).toBeInTheDocument();
  });

  it("shows no host-needed line while someone hosts it, and offers Remove only beside a second host", async () => {
    vi.mocked(Events.get).mockResolvedValue(churchWide([host(3, "Ana")]));
    renderPage();
    expect(await screen.findByText("Ana")).toBeInTheDocument();
    expect(screen.queryByText("No one hosts this event right now. Add a host below.")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Remove" })).not.toBeInTheDocument();
    cleanup();
    vi.mocked(Events.get).mockResolvedValue(churchWide([host(3, "Ana"), host(4, "Ben")]));
    renderPage();
    expect(await screen.findByText("Ben")).toBeInTheDocument();
    expect(screen.getAllByRole("button", { name: "Remove" })).toHaveLength(2);
  });
});

describe("EventDetail asks every question in the app's dialog", () => {
  const roster = async () => {
    await userEvent.click(await screen.findByRole("button", { name: "More for 2026-12-01" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Manage sign-ups" }));

    return within(await screen.findByRole("region", { name: "Manage sign-ups" }));
  };

  // Seven dates: CompactDates lists them compactly, with only 2026-12-01's details open.
  const longList = (first: EventOccurrence): EventDetailResponse => ({ ...page, occurrences: [first, ...[8, 15, 22, 29, 36, 43].map((days, i) => {
    const day = new Date(Date.UTC(2026, 11, 1 + days)).toISOString().slice(0, 10);

    return occurrence({ id: i + 2, date: day, startsAt: `${day}T19:00:00Z`, endsAt: `${day}T20:00:00Z`, startsLocal: `${day}T19:00` });
  })] });

  const showFirstDate = () => userEvent.click(screen.getAllByRole("button", { name: /^Details and RSVP: / })[0]);

  beforeEach(() => {
    vi.resetAllMocks();
    vi.restoreAllMocks();
    guarded();
    composer.admin = false;
    nativePromptsFail();
    vi.mocked(Households.mine).mockResolvedValue({ household: null });
    vi.mocked(Events.get).mockResolvedValue(page);
    vi.mocked(Events.delete).mockResolvedValue({});
    vi.mocked(Events.cancelOccurrence).mockResolvedValue({});
    vi.mocked(Events.removeRegistration).mockResolvedValue({});
    vi.mocked(Events.restoreRegistration).mockResolvedValue({});
    vi.spyOn(Site, "get").mockResolvedValue({ name: "Fold", timezone: "UTC", selfSignup: "off", roomsEnabled: false, pushEnabled: true });
  });

  it("deletes the event only after its dialog says yes, once, and Escape writes nothing", async () => {
    renderPage();
    const more = await screen.findByRole("button", { name: "More event actions" });
    await userEvent.click(more);
    await userEvent.click(screen.getByRole("menuitem", { name: "Delete event" }));
    const asked = screen.getByRole("dialog", { name: "Delete “Hybrid study”?" });
    expect(asked).toHaveAccessibleDescription("This removes every date and its registrations.");
    expect(within(asked).getByRole("button", { name: "Cancel" })).toHaveFocus();
    await userEvent.keyboard("{Escape}");
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(more).toHaveFocus();
    expect(Events.delete).not.toHaveBeenCalled();

    await userEvent.click(more);
    await userEvent.click(screen.getByRole("menuitem", { name: "Delete event" }));
    const yes = within(screen.getByRole("dialog")).getByRole("button", { name: "Delete event" });
    act(() => { yes.click(); yes.click(); });
    await waitFor(() => expect(Events.delete).toHaveBeenCalledWith(7));
    expect(Events.delete).toHaveBeenCalledOnce();
  });

  it("cancels a date only after its dialog, and keeps it when told to", async () => {
    renderPage();
    const more = await screen.findByRole("button", { name: "More for 2026-12-01" });
    await userEvent.click(more);
    await userEvent.click(screen.getByRole("menuitem", { name: "Cancel this date" }));
    await userEvent.click(within(screen.getByRole("dialog", { name: "Cancel this date on 2026-12-01?" })).getByRole("button", { name: "Keep this date" }));
    expect(Events.cancelOccurrence).not.toHaveBeenCalled();
    expect(more).toHaveFocus();

    await userEvent.click(more);
    await userEvent.click(screen.getByRole("menuitem", { name: "Cancel this date" }));
    await userEvent.click(within(screen.getByRole("dialog", { name: "Cancel this date on 2026-12-01?" })).getByRole("button", { name: "Cancel this date" }));
    await waitFor(() => expect(Events.cancelOccurrence).toHaveBeenCalledWith(7, "2026-12-01"));
    expect(Events.cancelOccurrence).toHaveBeenCalledOnce();
  });

  it("closes an open question when a refresh takes the manager's rights away, and the stale dialog cannot submit", async () => {
    renderPage();
    await roster();
    await userEvent.click(screen.getByRole("button", { name: "More for 2026-12-01" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Cancel this date" }));
    const yes = within(screen.getByRole("dialog", { name: "Cancel this date on 2026-12-01?" })).getByRole("button", { name: "Cancel this date" });
    vi.mocked(Events.get).mockResolvedValue({ ...page, canManage: false });
    // Manage sign-ups refreshes the page when the window comes back.
    act(() => { window.dispatchEvent(new Event("focus")); });
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    act(() => { yes.click(); });
    expect(screen.queryByRole("region", { name: "Manage sign-ups" })).not.toBeInTheDocument();
    expect(Events.cancelOccurrence).not.toHaveBeenCalled();
  });

  it("removes a sign-up and adds a removed one back over capacity only after each dialog", async () => {
    const lee = { registrationId: 40, userId: 4, name: "Lee", status: "removed" as const, guestCount: 0, attendanceMode: "in_person" as const };
    vi.mocked(Events.get).mockResolvedValue({ ...page, occurrences: [{ ...page.occurrences[0], capacity: 1, spotsLeft: 0, removedRegistrations: [lee] }] });
    renderPage();
    const people = await roster();
    await userEvent.click(people.getByRole("button", { name: "More for Sam" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Remove Sam" }));
    let asked = screen.getByRole("dialog", { name: "Remove Sam's registration for this date?" });
    expect(asked).toHaveTextContent("They won't be able to re-register on their own.");
    await userEvent.keyboard("{Escape}");
    expect(Events.removeRegistration).not.toHaveBeenCalled();
    await userEvent.click(people.getByRole("button", { name: "More for Sam" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Remove Sam" }));
    await userEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Remove" }));
    await waitFor(() => expect(Events.removeRegistration).toHaveBeenCalledWith(7, "2026-12-01", 31));

    await userEvent.click(people.getByRole("button", { name: "Add back Lee" }));
    asked = screen.getByRole("dialog", { name: "Add Lee over capacity?" });
    expect(asked).toHaveTextContent("This date's in-person seats are full.");
    await userEvent.click(within(asked).getByRole("button", { name: "Add over capacity" }));
    await waitFor(() => expect(Events.restoreRegistration).toHaveBeenCalledWith(7, "2026-12-01", { userId: 4 }, "in_person", true));
    expect(Events.restoreRegistration).toHaveBeenCalledOnce();
  });

  it("asks again after seats fill during an add, and a no keeps the refusal", async () => {
    const full = new ApiError(409, "full", { code: "event_full" });
    vi.mocked(Events.addRegistration).mockRejectedValueOnce(full).mockRejectedValueOnce(full).mockResolvedValue({});
    renderPage();
    const people = await roster();
    await userEvent.type(people.getByLabelText("Add someone"), "pat@example.com");
    await userEvent.click(people.getByRole("button", { name: "Add" }));
    let asked = await screen.findByRole("dialog", { name: "Add them over capacity?" });
    expect(asked).toHaveTextContent("This date filled up.");
    await userEvent.click(within(asked).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(people.getByRole("alert")).toBeInTheDocument());
    expect(Events.addRegistration).toHaveBeenCalledTimes(1);

    await userEvent.click(people.getByRole("button", { name: "Add" }));
    asked = await screen.findByRole("dialog", { name: "Add them over capacity?" });
    const yes = within(asked).getByRole("button", { name: "Add over capacity" });
    act(() => { yes.click(); yes.click(); });
    await waitFor(() => expect(Events.addRegistration).toHaveBeenCalledTimes(3));
    expect(Events.addRegistration).toHaveBeenLastCalledWith(7, "2026-12-01", "pat@example.com", true);
  });

  it("asks before an ask to add goes over this date's capacity", async () => {
    vi.mocked(Events.get).mockResolvedValue({ ...page, occurrences: [{ ...page.occurrences[0], capacity: 1, spotsLeft: 0 }] });
    vi.mocked(People.search).mockResolvedValue({ state: "results", people: [{ selectionId: "adult-opaque", displayName: "Pat", teen: false, sharedGroups: [], otherGroups: [] }] });
    vi.mocked(Events.addRegistration).mockRejectedValue(new ApiError(409, "SECRET", { code: "date_add_approval_required" }));
    vi.mocked(Adds.create).mockResolvedValue({ request: addRequest() });
    renderPage();
    const people = await roster();
    await userEvent.type(people.getByRole("combobox", { name: "Find a person by name" }), "Pat");
    await userEvent.click(within(await people.findByRole("listbox", { name: "People" })).getByRole("option", { name: /Pat/ }));
    await userEvent.click(people.getByRole("button", { name: "Add" }));
    await userEvent.click(within(screen.getByRole("dialog", { name: "Add them over capacity?" })).getByRole("button", { name: "Add over capacity" }));
    await userEvent.click(await people.findByRole("button", { name: "Ask to add" }));
    await userEvent.click(people.getByRole("button", { name: "Send ask to add" }));
    const asked = screen.getByRole("dialog", { name: "Allow this ask to add someone over this date's capacity?" });
    expect(asked).toHaveTextContent("This choice is logged and cannot be changed on this request.");
    await userEvent.keyboard("{Escape}");
    expect(Adds.create).not.toHaveBeenCalled();
    await userEvent.click(people.getByRole("button", { name: "Send ask to add" }));
    await userEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Allow and send" }));
    await waitFor(() => expect(Adds.create).toHaveBeenCalledOnce());
    expect(Adds.create).toHaveBeenCalledWith(expect.objectContaining({ selectionId: "adult-opaque", overCapacity: true }));
  });

  it("saves a date that shuts people out only after its dialog counts them", async () => {
    vi.mocked(Events.get).mockResolvedValue({ ...page, occurrences: [{ ...page.occurrences[0], visibility: "public", visibilityOverride: "public", seriesVisibility: "private" }] });
    const token = "c0ffeec0ffeec0ffeec0ffeec0ffee00";
    edits.answer(challenge(2, token), challenge(2, token));
    renderPage();
    await userEvent.click(await screen.findByRole("button", { name: "More for 2026-12-01" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Edit this date" }));
    await userEvent.click(await screen.findByRole("radio", { name: "Same as series (private)" }));
    const save = screen.getByRole("button", { name: "Save this date" });
    await waitFor(() => expect(save).toBeEnabled());
    await userEvent.click(save);
    const asked = await screen.findByRole("dialog", { name: "Save this date?" });
    // The question names the date's current and new value (DEC-158), from the writer's own answer.
    expect(asked).toHaveAccessibleDescription("Tuesday, December 1, 2026. This date is public now. Saving makes it private, so fewer people will be able to see it. This cancels and notifies 2 registered people who can no longer see this date.");
    await userEvent.click(within(asked).getByRole("button", { name: "Cancel" }));
    expect(edits.sent).toHaveLength(1);
    await waitFor(() => expect(save).toBeEnabled());

    await userEvent.click(save);
    await userEvent.click(within(await screen.findByRole("dialog", { name: "Save this date?" })).getByRole("button", { name: "Save and notify" }));
    await waitFor(() => expect(edits.sent).toHaveLength(3));
    expect(edits.sent[2]).toMatchObject({ attempt: edits.sent[1].attempt, changes: { visibility: "series" }, consent: token });
    expect(edits.sent[1].attempt).not.toBe(edits.sent[0].attempt);
    expect(Events.dateVisibilityImpact).not.toHaveBeenCalled();
  });

  it("asks a manager with unsaved edits in the app's dialog before moving to another page", async () => {
    const router = createMemoryRouter([{
      element: <><UnsavedChangesBlocker /><Outlet /></>,
      children: [
        { path: "/events/:id", element: <EventDetail /> },
        { path: "/elsewhere", element: <p>Elsewhere page</p> },
      ],
    }], { initialEntries: ["/events/7"] });

    render(<RouterProvider router={router} />);
    await userEvent.click(await screen.findByRole("button", { name: "Edit event" }));
    const title = screen.getByLabelText("Title");
    await userEvent.type(title, " night");

    await act(() => router.navigate("/elsewhere"));
    const asked = await screen.findByRole("dialog", { name: "Leave without saving?" });
    expect(asked).toHaveTextContent("Your changes will be lost.");
    await userEvent.click(within(asked).getByRole("button", { name: "Keep editing" }));
    expect(screen.queryByText("Elsewhere page")).not.toBeInTheDocument();
    expect(title).toHaveValue("Hybrid study night");
    expect(title).toHaveFocus();

    await act(() => router.navigate("/elsewhere"));
    await userEvent.click(within(await screen.findByRole("dialog", { name: "Leave without saving?" })).getByRole("button", { name: "Leave without saving" }));
    expect(await screen.findByText("Elsewhere page")).toBeInTheDocument();
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });
  it("holds Delete from the first ask until the DELETE answers, and a late success never moves someone who went elsewhere", async () => {
    const held = deferred<unknown>();
    vi.mocked(Events.delete).mockReturnValue(held.promise);

    const router = createMemoryRouter([
      { path: "/events/:id", element: <EventDetail /> },
      { path: "/elsewhere", element: <p>Elsewhere page</p> },
      { path: "/", element: <p>Home page</p> },
    ], { initialEntries: ["/events/7"] });

    render(<RouterProvider router={router} />);
    const more = await screen.findByRole("button", { name: "More event actions" });
    await userEvent.click(more);
    await userEvent.click(screen.getByRole("menuitem", { name: "Delete event" }));
    await userEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Delete event" }));
    expect(Events.delete).toHaveBeenCalledOnce();

    await userEvent.click(more);
    const again = screen.getByRole("menuitem", { name: "Delete event" });
    expect(again).toBeDisabled();
    await userEvent.click(again);
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(Events.delete).toHaveBeenCalledOnce();

    await act(() => router.navigate("/elsewhere"));
    await act(async () => held.resolve({}));
    expect(router.state.location.pathname).toBe("/elsewhere");
    expect(screen.getByText("Elsewhere page")).toBeInTheDocument();
  });

  it.each([2, 0])("never asks the question of a save whose editor closed while it was on its way (%i shut out)", async (registrants) => {
    vi.mocked(Events.get).mockResolvedValue({ ...page, occurrences: [{ ...page.occurrences[0], visibility: "public", visibilityOverride: "public", seriesVisibility: "private" }] });
    const answer = deferred<OccurrenceResponse>();
    edits.answer(answer.promise);
    renderPage();
    await userEvent.click(await screen.findByRole("button", { name: "More for 2026-12-01" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Edit this date" }));
    await userEvent.click(await screen.findByRole("radio", { name: "Same as series (private)" }));
    await waitFor(() => expect(screen.getByRole("button", { name: "Save this date" })).toBeEnabled());
    await userEvent.click(screen.getByRole("button", { name: "Save this date" }));
    await waitFor(() => expect(edits.sent).toHaveLength(1));
    // The draft being saved cannot change under it.
    expect(screen.getByLabelText("Title")).toBeDisabled();
    await userEvent.click(screen.getByRole("button", { name: "Close" }));

    await act(async () => {
      if (registrants) answer.reject(challenge(registrants));
      else answer.resolve(storedEdit(1, "r1"));
      await answer.promise.catch(() => {});
    });
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(edits.sent).toHaveLength(1);
  });

  it("closes the impact question when a refresh keeps the ids and rights but changes the visibility the date inherits", async () => {
    const shown = { ...page, occurrences: [{ ...page.occurrences[0], visibility: "public" as const, visibilityOverride: "public" as const, seriesVisibility: "private" as const }] };
    vi.mocked(Events.get).mockResolvedValue(shown);
    edits.answer(challenge(2));
    renderPage();
    await roster();
    await userEvent.click(screen.getByRole("button", { name: "More for 2026-12-01" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Edit this date" }));
    await userEvent.click(await screen.findByRole("radio", { name: "Same as series (private)" }));
    await waitFor(() => expect(screen.getByRole("button", { name: "Save this date" })).toBeEnabled());
    await userEvent.click(screen.getByRole("button", { name: "Save this date" }));
    const yes = within(await screen.findByRole("dialog", { name: "Save this date?" })).getByRole("button", { name: "Save and notify" });

    vi.mocked(Events.get).mockResolvedValue({ ...shown, occurrences: [{ ...shown.occurrences[0], seriesVisibility: "invite_only" }] });
    // Manage sign-ups refreshes the page when the window comes back; its answer reads the date again for the editor.
    act(() => { window.dispatchEvent(new Event("focus")); });
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    act(() => { yes.click(); });
    await waitFor(() => expect(screen.getByText("This date changed while you were saving. Check it and save again.")).toBeInTheDocument());
    await waitFor(() => expect(screen.getByRole("button", { name: "Save this date" })).toBeEnabled());
    expect(edits.sent).toHaveLength(1);
  });

  it("closes a member's series Can't go question when a delayed refresh changes which dates the series runs to", async () => {
    const dates = [occurrence({ recurrenceRule: "FREQ=WEEKLY" }), occurrence({ id: 2, date: "2026-12-08", recurrenceRule: "FREQ=WEEKLY" })];
    const series = { ...page, canManage: false, event: { ...page.event, recurrenceRule: "FREQ=WEEKLY" }, occurrences: dates };
    const refresh = deferred<EventDetailResponse>();
    vi.mocked(Events.get).mockResolvedValueOnce(series).mockReturnValueOnce(refresh.promise);
    vi.mocked(Events.register).mockResolvedValue({ registration: {
      registrationId: 12, userId: 2, eventId: 7, occurrenceId: 2, date: "2026-12-08", status: "going",
      guestCount: 0, childcareCount: 0, childcareAgesNote: "", waitlistPosition: null, attendanceMode: "in_person",
    }, people: [] });
    renderPage();
    const answerGroups = await screen.findAllByRole("group", { name: "Your answer" });
    await userEvent.click(within(answerGroups[1]).getByRole("button", { name: "Going" }));
    await userEvent.click(screen.getByRole("button", { name: "Just this date" }));
    await waitFor(() => expect(Events.get).toHaveBeenCalledTimes(2));

    await answerAll(answerGroups[0], "Can't go");
    const yes = within(screen.getByRole("dialog", { name: "Can't go to these dates?" })).getByRole("button", { name: "Can't go to these dates" });
    await act(async () => refresh.resolve({ ...series, occurrences: dates.map((row) => ({ ...row, rruleUntil: "2026-12-08" })) }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    act(() => { yes.click(); });
    expect(Events.registerSeries).not.toHaveBeenCalled();
  });

  it("asks again when seats fill during an Add back: no sends nothing more and gives focus back, yes sends one override for the same person and mode", async () => {
    const lee = { registrationId: 40, userId: 4, name: "Lee", status: "removed" as const, guestCount: 0, attendanceMode: "in_person" as const };
    vi.mocked(Events.get).mockResolvedValue({ ...page, occurrences: [{ ...page.occurrences[0], removedRegistrations: [lee] }] });
    const full = new ApiError(409, "full", { code: "event_full" });
    vi.mocked(Events.restoreRegistration).mockRejectedValueOnce(full).mockRejectedValueOnce(full).mockResolvedValue({});
    renderPage();
    const people = await roster();
    const addBack = people.getByRole("button", { name: "Add back Lee" });
    await userEvent.click(addBack);
    let asked = await screen.findByRole("dialog", { name: "Add Lee over capacity?" });
    expect(asked).toHaveTextContent("This date's in-person seats filled.");
    expect(within(asked).getByRole("button", { name: "Add over capacity" })).toHaveFocus();
    await userEvent.keyboard("{Escape}");
    await waitFor(() => expect(people.getByRole("alert")).toBeInTheDocument());
    expect(Events.restoreRegistration).toHaveBeenCalledOnce();
    await waitFor(() => expect(addBack).toHaveFocus());

    await userEvent.click(addBack);
    asked = await screen.findByRole("dialog", { name: "Add Lee over capacity?" });
    const yes = within(asked).getByRole("button", { name: "Add over capacity" });
    act(() => { yes.click(); yes.click(); });
    await waitFor(() => expect(Events.restoreRegistration).toHaveBeenCalledTimes(3));
    expect(Events.restoreRegistration).toHaveBeenNthCalledWith(2, 7, "2026-12-01", { userId: 4 }, "in_person", false);
    expect(Events.restoreRegistration).toHaveBeenLastCalledWith(7, "2026-12-01", { userId: 4 }, "in_person", true);
  });

  it("asks nothing when seats fill after the roster was closed: the refusal stands and nothing more is sent", async () => {
    const lee = { registrationId: 40, userId: 4, name: "Lee", status: "removed" as const, guestCount: 0, attendanceMode: "in_person" as const };
    vi.mocked(Events.get).mockResolvedValue({ ...page, occurrences: [{ ...page.occurrences[0], removedRegistrations: [lee] }] });
    const restore = deferred<unknown>();
    vi.mocked(Events.restoreRegistration).mockReturnValue(restore.promise);
    renderPage();
    const people = await roster();
    await userEvent.click(people.getByRole("button", { name: "Add back Lee" }));
    await userEvent.click(people.getByRole("button", { name: "Close" }));
    expect(screen.queryByRole("region", { name: "Manage sign-ups" })).not.toBeInTheDocument();
    await act(async () => restore.reject(new ApiError(409, "full", { code: "event_full" })));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(Events.restoreRegistration).toHaveBeenCalledOnce();
  });

  it("asks once when Back is pressed again while the leave question is open; Keep editing keeps the draft and Leave goes to the latest destination", async () => {
    const router = createMemoryRouter([{
      element: <><UnsavedChangesBlocker /><Outlet /></>,
      children: [
        { path: "/events/:id", element: <EventDetail /> },
        { path: "/elsewhere", element: <p>Elsewhere page</p> },
        { path: "/other", element: <p>Other page</p> },
      ],
    }], { initialEntries: ["/elsewhere", "/events/7"], initialIndex: 1 });

    render(<RouterProvider router={router} />);
    await userEvent.click(await screen.findByRole("button", { name: "Edit event" }));
    const title = screen.getByLabelText("Title");
    await userEvent.type(title, " night");

    await act(() => router.navigate(-1));
    const asked = await screen.findByRole("dialog", { name: "Leave without saving?" });
    expect(within(asked).getByRole("button", { name: "Leave without saving" })).toHaveFocus();
    await act(() => router.navigate(-1));
    expect(screen.getAllByRole("dialog")).toHaveLength(1);
    await userEvent.click(within(asked).getByRole("button", { name: "Keep editing" }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(router.state.location.pathname).toBe("/events/7");
    expect(title).toHaveValue("Hybrid study night");
    expect(title).toHaveFocus();

    await act(() => router.navigate(-1));
    await screen.findByRole("dialog", { name: "Leave without saving?" });
    await act(() => router.navigate("/other"));
    expect(screen.getAllByRole("dialog")).toHaveLength(1);
    await userEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Leave without saving" }));
    expect(await screen.findByText("Other page")).toBeInTheDocument();
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("never asks the question of a save whose address changed and came back to the same entry while it was on its way", async () => {
    vi.mocked(Events.get).mockResolvedValue({ ...page, occurrences: [{ ...page.occurrences[0], visibility: "public", visibilityOverride: "public", seriesVisibility: "private" }] });
    const answer = deferred<OccurrenceResponse>();
    edits.answer(answer.promise);
    const router = createMemoryRouter([{ path: "/events/:id", element: <EventDetail /> }], { initialEntries: ["/events/7"] });
    render(<DateWritesProvider credential={credential}><RouterProvider router={router} /></DateWritesProvider>);
    const key = router.state.location.key;
    await userEvent.click(await screen.findByRole("button", { name: "More for 2026-12-01" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Edit this date" }));
    await userEvent.click(await screen.findByRole("radio", { name: "Same as series (private)" }));
    await waitFor(() => expect(screen.getByRole("button", { name: "Save this date" })).toBeEnabled());
    await userEvent.click(screen.getByRole("button", { name: "Save this date" }));
    await waitFor(() => expect(edits.sent).toHaveLength(1));

    await act(() => router.navigate("/events/7?manage=2026-12-01"));
    await act(() => router.navigate(-1));
    expect(router.state.location.key).toBe(key);
    // Another address read the date afresh, without the draft; the save stays on its way for the session.
    expect(await screen.findByRole("button", { name: "Saving…" })).toBeDisabled();
    expect(screen.getByRole("radio", { name: "Same as series (private)" })).not.toBeChecked();
    await act(async () => { answer.reject(challenge(2)); await answer.promise.catch(() => {}); });
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(edits.sent).toHaveLength(1);
    await waitFor(() => expect(screen.getByRole("button", { name: "Save this date" })).toBeEnabled());
  });

  it("never moves home for a held DELETE whose page went to another address and came back", async () => {
    const held = deferred<unknown>();
    vi.mocked(Events.delete).mockReturnValue(held.promise);

    const router = createMemoryRouter([
      { path: "/events/:id", element: <EventDetail /> },
      { path: "/", element: <p>Home page</p> },
    ], { initialEntries: ["/events/7"] });

    render(<RouterProvider router={router} />);
    await userEvent.click(await screen.findByRole("button", { name: "More event actions" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Delete event" }));
    await userEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Delete event" }));
    await act(() => router.navigate("/events/7?manage=2026-12-01"));
    await act(() => router.navigate(-1));
    await act(async () => held.resolve({}));
    expect(router.state.location.pathname).toBe("/events/7");
    expect(screen.queryByText("Home page")).not.toBeInTheDocument();
  });

  it("closes a member's bulk Can't go question when a refresh adds a date to the same series rule", async () => {
    const dates = [occurrence({ recurrenceRule: "FREQ=WEEKLY" }), occurrence({ id: 2, date: "2026-12-08", recurrenceRule: "FREQ=WEEKLY" })];
    const series = { ...page, canManage: false, event: { ...page.event, recurrenceRule: "FREQ=WEEKLY" }, occurrences: dates };
    const refresh = deferred<EventDetailResponse>();
    vi.mocked(Events.get).mockResolvedValueOnce(series).mockReturnValueOnce(refresh.promise);
    vi.mocked(Events.register).mockResolvedValue({ registration: {
      registrationId: 12, userId: 2, eventId: 7, occurrenceId: 2, date: "2026-12-08", status: "going",
      guestCount: 0, childcareCount: 0, childcareAgesNote: "", waitlistPosition: null, attendanceMode: "in_person",
    }, people: [] });
    renderPage();
    const answerGroups = await screen.findAllByRole("group", { name: "Your answer" });
    await userEvent.click(within(answerGroups[1]).getByRole("button", { name: "Going" }));
    await userEvent.click(screen.getByRole("button", { name: "Just this date" }));
    await waitFor(() => expect(Events.get).toHaveBeenCalledTimes(2));

    await answerAll(answerGroups[0], "Can't go");
    const yes = within(screen.getByRole("dialog", { name: "Can't go to these dates?" })).getByRole("button", { name: "Can't go to these dates" });
    // The rule, its end and the first date's own answer are unchanged; only the dates "these dates" covers grew.
    const added = occurrence({ id: 3, date: "2026-12-15", startsAt: "2026-12-15T19:00:00Z", endsAt: "2026-12-15T20:00:00Z", recurrenceRule: "FREQ=WEEKLY" });
    await act(async () => refresh.resolve({ ...series, occurrences: [...dates, added] }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    act(() => { yes.click(); });
    expect(Events.registerSeries).not.toHaveBeenCalled();
  });

  it("closes the impact question when a refresh moves the date's time zone and keeps every visibility fact", async () => {
    const shown = { ...page, occurrences: [{ ...page.occurrences[0], visibility: "public" as const, visibilityOverride: "public" as const, seriesVisibility: "private" as const }] };
    vi.mocked(Events.get).mockResolvedValue(shown);
    edits.answer(challenge(2));
    renderPage();
    await roster();
    await userEvent.click(screen.getByRole("button", { name: "More for 2026-12-01" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Edit this date" }));
    await userEvent.click(await screen.findByRole("radio", { name: "Same as series (private)" }));
    await waitFor(() => expect(screen.getByRole("button", { name: "Save this date" })).toBeEnabled());
    await userEvent.click(screen.getByRole("button", { name: "Save this date" }));
    const yes = within(await screen.findByRole("dialog", { name: "Save this date?" })).getByRole("button", { name: "Save and notify" });

    vi.mocked(Events.get).mockResolvedValue({ ...shown, occurrences: [{ ...shown.occurrences[0], tzid: "America/Chicago", startsAt: "2026-12-02T01:00:00Z", endsAt: "2026-12-02T02:00:00Z" }] });
    // Manage sign-ups refreshes the page when the window comes back; its answer reads the date again for the editor.
    act(() => { window.dispatchEvent(new Event("focus")); });
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    act(() => { yes.click(); });
    await waitFor(() => expect(screen.getByText("This date changed while you were saving. Check it and save again.")).toBeInTheDocument());
    await waitFor(() => expect(screen.getByRole("button", { name: "Save this date" })).toBeEnabled());
    expect(edits.sent).toHaveLength(1);
  });

  it.each([["closed", false], ["closed and opened again", true]] as const)("asks nothing when seats fill after the add form was %s: the draft and the refusal stay", async (_label, reopen) => {
    const add = deferred<unknown>();
    vi.mocked(Events.addRegistration).mockReturnValue(add.promise);
    renderPage();
    const people = await roster();
    await userEvent.type(people.getByLabelText("Add someone"), "pat@example.com");
    await userEvent.click(people.getByRole("button", { name: "Add" }));
    const toggle = people.getByRole("button", { name: "Close add person" });
    await userEvent.click(toggle);
    expect(people.getByRole("button", { name: "Add person" })).toHaveAttribute("aria-expanded", "false");

    if (reopen) await userEvent.click(people.getByRole("button", { name: "Add person" }));
    await act(async () => add.reject(new ApiError(409, "full", { code: "event_full" })));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(Events.addRegistration).toHaveBeenCalledOnce();
    expect(people.getByRole("alert")).toHaveTextContent("This event is full.");
    // Focus stays on the visible toggle the person used, never on the hidden Add button.
    expect(toggle).toHaveFocus();

    if (!reopen) await userEvent.click(people.getByRole("button", { name: "Add person" }));
    expect(people.getByLabelText("Add someone")).toHaveValue("pat@example.com");
    expect(people.getByRole("button", { name: "Add" })).toBeEnabled();
  });

  it("holds a date while its save is on its way, also after its editor closes, so no second save can start, and the answer frees it", async () => {
    const answer = deferred<OccurrenceResponse>();
    edits.answer(answer.promise);
    renderPage();
    const more = await screen.findByRole("button", { name: "More for 2026-12-01" });
    await userEvent.click(more);
    await userEvent.click(screen.getByRole("menuitem", { name: "Edit this date" }));
    const title = await screen.findByLabelText("Title");
    await userEvent.clear(title);
    await userEvent.type(title, "Carols");
    await waitFor(() => expect(screen.getByRole("button", { name: "Save this date" })).toBeEnabled());
    await userEvent.click(screen.getByRole("button", { name: "Save this date" }));
    await waitFor(() => expect(edits.sent).toHaveLength(1));
    expect(screen.getByRole("button", { name: "Saving…" })).toBeDisabled();
    expect(title).toBeDisabled();

    // Closing never ends a save already sent: the date stays closed to another.
    await userEvent.click(screen.getByRole("button", { name: "Close" }));
    await userEvent.click(more);
    expect(screen.getByRole("menuitem", { name: "Edit this date" })).toBeDisabled();
    await userEvent.keyboard("{Escape}");

    await act(async () => answer.resolve(storedEdit(1, "r1")));
    expect(edits.sent).toHaveLength(1);

    if (!screen.queryByRole("menuitem", { name: "Edit this date" })) await userEvent.click(more);
    await waitFor(() => expect(screen.getByRole("menuitem", { name: "Edit this date" })).toBeEnabled());
  });

  it("shows nothing of a saved date while the page's refresh fails, and opens it on the stored title once the page loads", async () => {
    renderPage();
    const more = await screen.findByRole("button", { name: "More for 2026-12-01" });
    await userEvent.click(more);
    await userEvent.click(screen.getByRole("menuitem", { name: "Edit this date" }));
    await userEvent.clear(await screen.findByLabelText("Title"));
    await userEvent.type(screen.getByLabelText("Title"), "Carols");
    const refresh = deferred<EventDetailResponse>();
    vi.mocked(Events.get).mockReturnValueOnce(refresh.promise);
    await waitFor(() => expect(screen.getByRole("button", { name: "Save this date" })).toBeEnabled());
    await userEvent.click(screen.getByRole("button", { name: "Save this date" }));
    await waitFor(() => expect(screen.queryByLabelText("Title")).not.toBeInTheDocument());
    expect(edits.sent).toHaveLength(1);

    await act(async () => refresh.reject(new Error("offline")));
    const tryAgain = await screen.findByRole("button", { name: "Try again" });
    vi.mocked(Events.managedDate).mockResolvedValue({ occurrence: { ...page.occurrences[0], title: "Carols", revision: "r1" } });
    await userEvent.click(more);
    await userEvent.click(screen.getByRole("menuitem", { name: "Edit this date" }));
    // A failed page read is no authority: nothing of the date, and nothing read for it.
    expect(await screen.findByText("This date can be edited again once the page loads. What you typed is kept.")).toBeInTheDocument();
    expect(screen.queryByLabelText("Title")).not.toBeInTheDocument();
    await userEvent.click(tryAgain);
    // The page still lists the old title; the editor reads the date itself.
    expect(await screen.findByLabelText("Title")).toHaveValue("Carols");
    expect(edits.sent).toHaveLength(1);
  });

  it("leaves Edit this date open through a held refresh after an answer, when no date update ran", async () => {
    vi.mocked(Events.register).mockResolvedValue({ registration: {
      registrationId: 12, userId: 2, eventId: 7, occurrenceId: 1, date: "2026-12-01", status: "going",
      guestCount: 0, childcareCount: 0, childcareAgesNote: "", waitlistPosition: null, attendanceMode: "in_person",
    }, people: [] });
    renderPage();
    const more = await screen.findByRole("button", { name: "More for 2026-12-01" });
    const refresh = deferred<EventDetailResponse>();
    vi.mocked(Events.get).mockReturnValueOnce(refresh.promise);
    await userEvent.click(within(screen.getByRole("group", { name: "Your answer" })).getByRole("button", { name: "Going" }));
    await waitFor(() => expect(Events.get).toHaveBeenCalledTimes(2));

    await userEvent.click(more);
    const edit = screen.getByRole("menuitem", { name: "Edit this date" });
    expect(edit).toBeEnabled();
    await userEvent.click(edit);
    expect(await screen.findByLabelText("Title")).toBeEnabled();
    await waitFor(() => expect(screen.getByRole("button", { name: "Save this date" })).toBeEnabled());

    // The page's answer reads the date again for the editor, which comes back ready.
    await act(async () => refresh.resolve(page));
    expect(await screen.findByLabelText("Title")).toBeEnabled();
    await waitFor(() => expect(screen.getByRole("button", { name: "Save this date" })).toBeEnabled());
    expect(edits.sent).toHaveLength(0);
  });

  it("lets a late update close nothing when its editor went away and came back meanwhile, and the new editor cannot start a second one", async () => {
    const answer = deferred<OccurrenceResponse>();
    edits.answer(answer.promise);
    vi.mocked(Events.register).mockResolvedValue({ registration: {
      registrationId: 12, userId: 2, eventId: 7, occurrenceId: 1, date: "2026-12-01", status: "going",
      guestCount: 0, childcareCount: 0, childcareAgesNote: "", waitlistPosition: null, attendanceMode: "in_person",
    }, people: [] });
    renderPage();
    await roster();
    await userEvent.click(screen.getByRole("button", { name: "More for 2026-12-01" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Edit this date" }));
    await userEvent.clear(await screen.findByLabelText("Title"));
    await userEvent.type(screen.getByLabelText("Title"), "Carols");
    await waitFor(() => expect(screen.getByRole("button", { name: "Save this date" })).toBeEnabled());
    await userEvent.click(screen.getByRole("button", { name: "Save this date" }));
    await waitFor(() => expect(edits.sent).toHaveLength(1));

    // Managing rights go (the editor unmounts) and come back (a fresh editor for the same date) while the update is held.
    vi.mocked(Events.get).mockResolvedValue({ ...page, canManage: false });
    act(() => { window.dispatchEvent(new Event("focus")); });
    await waitFor(() => expect(screen.queryByLabelText("Title")).not.toBeInTheDocument());
    vi.mocked(Events.get).mockResolvedValue(page);
    await userEvent.click(within(screen.getByRole("group", { name: "Your answer" })).getByRole("button", { name: "Going" }));
    const reopened = await screen.findByLabelText("Title");
    const editor = within(requiredElement(reopened.closest("form"), HTMLFormElement));
    expect(editor.getByRole("button", { name: "Saving…" })).toBeDisabled();
    expect(reopened).toBeDisabled();
    // The reopened editor read the date as it was before the update.
    expect(reopened).toHaveValue("Supper");

    const stored = { ...page.occurrences[0], title: "Carols" };
    vi.mocked(Events.get).mockResolvedValue({ ...page, occurrences: [stored] });
    await act(async () => answer.resolve({ occurrence: { ...stored, revision: "r1" } }));
    // The late answer closes nothing; the page's answer reads the date again, so the editor that missed the
    // update shows the stored title and a save cannot send the old one back.
    await waitFor(() => expect(screen.getByRole("button", { name: "Save this date" })).toBeEnabled());
    expect(screen.getByLabelText("Title")).toHaveValue("Carols");
    expect(screen.getByLabelText("Title")).toBeEnabled();
    expect(edits.sent).toHaveLength(1);
  });

  it.each([["hidden", false], ["hidden and shown again", true]] as const)("asks nothing when seats fill after a long list's date was %s during an add: the draft and the refusal stay", async (_label, reshow) => {
    vi.mocked(Events.get).mockResolvedValue(longList(page.occurrences[0]));
    const add = deferred<unknown>();
    vi.mocked(Events.addRegistration).mockReturnValue(add.promise);
    renderPage();
    const people = await roster();
    await userEvent.type(people.getByLabelText("Add someone"), "pat@example.com");
    await userEvent.click(people.getByRole("button", { name: "Add" }));
    // The add form itself stays open; only the date's details are hidden.
    await userEvent.click(screen.getByRole("button", { name: /^Hide details: / }));
    expect(screen.queryByRole("region", { name: "Manage sign-ups" })).not.toBeInTheDocument();

    if (reshow) await showFirstDate();
    await act(async () => add.reject(new ApiError(409, "full", { code: "event_full" })));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(Events.addRegistration).toHaveBeenCalledOnce();

    if (!reshow) await showFirstDate();
    expect(people.getByRole("alert")).toHaveTextContent("This event is full.");
    expect(people.getByLabelText("Add someone")).toHaveValue("pat@example.com");
    expect(people.getByRole("button", { name: "Add" })).toBeEnabled();
  });

  it.each([["hidden", false], ["hidden and shown again", true]] as const)("never asks the question of a save whose date a long list %s while it was on its way", async (_label, reshow) => {
    vi.mocked(Events.get).mockResolvedValue(longList({ ...page.occurrences[0], visibility: "public", visibilityOverride: "public", seriesVisibility: "private" }));
    const answer = deferred<OccurrenceResponse>();
    edits.answer(answer.promise);
    renderPage();
    await userEvent.click(await screen.findByRole("button", { name: "More for 2026-12-01" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Edit this date" }));
    await userEvent.click(await screen.findByRole("radio", { name: "Same as series (private)" }));
    await waitFor(() => expect(screen.getByRole("button", { name: "Save this date" })).toBeEnabled());
    await userEvent.click(screen.getByRole("button", { name: "Save this date" }));
    await waitFor(() => expect(edits.sent).toHaveLength(1));
    await userEvent.click(screen.getByRole("button", { name: /^Hide details: / }));
    // Hiding the date retires its editor: nothing of it stays in the page.
    expect(screen.queryByLabelText("Title")).not.toBeInTheDocument();

    if (reshow) await showFirstDate();
    await act(async () => { answer.reject(challenge(2)); await answer.promise.catch(() => {}); });
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(edits.sent).toHaveLength(1);

    if (!reshow) {
      await showFirstDate();
      // The withdrawn question's line comes back with the draft.
      await waitFor(() => expect(screen.getByText("This date changed while you were saving. Check it and save again.")).toBeInTheDocument());
      expect(screen.getByRole("radio", { name: "Same as series (private)" })).toBeChecked();
    }

    await waitFor(() => expect(screen.getByRole("button", { name: "Save this date" })).toBeEnabled());
  });

  it("keeps a date closed to a second save while its update is on its way, across a reload that remounts every date", async () => {
    composer.admin = true;
    const churchWide: EventDetailResponse = { ...page, event: { ...page.event, churchWide: true, hosts: [] } };
    vi.mocked(Events.get).mockResolvedValue(churchWide);
    vi.mocked(Events.addHost).mockResolvedValue({ hosts: [] });
    const answer = deferred<OccurrenceResponse>();
    edits.answer(answer.promise);
    renderPage();
    await userEvent.click(await screen.findByRole("button", { name: "More for 2026-12-01" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Edit this date" }));
    await userEvent.clear(await screen.findByLabelText("Title"));
    await userEvent.type(screen.getByLabelText("Title"), "Carols");
    await waitFor(() => expect(screen.getByRole("button", { name: "Save this date" })).toBeEnabled());
    await userEvent.click(screen.getByRole("button", { name: "Save this date" }));
    await waitFor(() => expect(edits.sent).toHaveLength(1));

    // Adding a host reloads the whole page (load.retry): every date row goes away and comes back new.
    const refresh = deferred<EventDetailResponse>();
    vi.mocked(Events.get).mockReturnValueOnce(refresh.promise);
    await userEvent.type(screen.getByLabelText("Host email", { exact: false }), "ana@example.com");
    await userEvent.click(screen.getByRole("button", { name: "Add host" }));
    await waitFor(() => expect(screen.queryByRole("button", { name: "More for 2026-12-01" })).not.toBeInTheDocument());
    await act(async () => refresh.resolve(churchWide));

    const more = await screen.findByRole("button", { name: "More for 2026-12-01" });
    expect(screen.queryByLabelText("Title")).not.toBeInTheDocument();
    await userEvent.click(more);
    expect(screen.getByRole("menuitem", { name: "Edit this date" })).toBeDisabled();
    expect(edits.sent).toHaveLength(1);

    await act(async () => answer.resolve(storedEdit(1, "r1")));
    expect(edits.sent).toHaveLength(1);

    if (!screen.queryByRole("menuitem", { name: "Edit this date" })) await userEvent.click(more);
    await waitFor(() => expect(screen.getByRole("menuitem", { name: "Edit this date" })).toBeEnabled());
  });

  it("keeps a date whose answer never arrived closed on every editor, and its own opener resends the same bytes once", async () => {
    edits.answer(new TypeError("Failed to fetch"));
    renderPage();
    const more = await screen.findByRole("button", { name: "More for 2026-12-01" });
    await userEvent.click(more);
    await userEvent.click(screen.getByRole("menuitem", { name: "Edit this date" }));
    await userEvent.clear(await screen.findByLabelText("Title"));
    await userEvent.type(screen.getByLabelText("Title"), "Carols");
    await waitFor(() => expect(screen.getByRole("button", { name: "Save this date" })).toBeEnabled());
    await userEvent.click(screen.getByRole("button", { name: "Save this date" }));
    await waitFor(() => expect(screen.getByText("We couldn't tell whether this date saved. Check it below, and try again if you need to.")).toBeInTheDocument());
    // The page's answer reads the date again, and the doubt shows beside that read.
    await screen.findByRole("button", { name: "Try again" });
    await userEvent.click(screen.getByRole("button", { name: "Close" }));

    // Edit stays closed; the doubt is reached through its own opener, outside the menu.
    await userEvent.click(more);
    expect(screen.getByRole("menuitem", { name: "Edit this date" })).toBeDisabled();
    await userEvent.keyboard("{Escape}");
    await userEvent.click(screen.getByRole("button", { name: "Check your last change to this date" }));
    const again = await screen.findByRole("button", { name: "Try again" });
    expect(screen.getByText("Title: Carols")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Save this date" })).toBeDisabled();
    await waitFor(() => expect(again).toBeEnabled());
    await userEvent.click(again);
    await waitFor(() => expect(edits.sent).toHaveLength(2));
    expect(edits.sent[1].body).toBe(edits.sent[0].body);
    await waitFor(() => expect(screen.queryByRole("button", { name: "Check your last change to this date" })).not.toBeInTheDocument());
  });

  it("keeps a date closed to a second update after leaving the event and coming back while the first is on its way, and loads the event again once it answers", async () => {
    const answer = deferred<OccurrenceResponse>();
    edits.answer(answer.promise);

    const router = createMemoryRouter([
      { path: "/events/:id", element: <EventDetail /> },
      { path: "/", element: <p>Home page</p> },
    ], { initialEntries: ["/events/7"] });

    // App holds the date updates for the signed-in session, outside every route.
    render(<DateWritesProvider credential={credential}><RouterProvider router={router} /></DateWritesProvider>);
    await userEvent.click(await screen.findByRole("button", { name: "More for 2026-12-01" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Edit this date" }));
    await userEvent.clear(await screen.findByLabelText("Title"));
    await userEvent.type(screen.getByLabelText("Title"), "Carols");
    await waitFor(() => expect(screen.getByRole("button", { name: "Save this date" })).toBeEnabled());
    await userEvent.click(screen.getByRole("button", { name: "Save this date" }));
    await waitFor(() => expect(edits.sent).toHaveLength(1));

    await act(() => router.navigate("/"));
    expect(await screen.findByText("Home page")).toBeInTheDocument();
    // A new page for the event, loaded before the update reached the server.
    await act(() => router.navigate("/events/7"));
    await userEvent.click(await screen.findByRole("button", { name: "More for 2026-12-01" }));
    expect(screen.getByRole("menuitem", { name: "Edit this date" })).toBeDisabled();
    await userEvent.keyboard("{Escape}");
    expect(screen.getByRole("heading", { level: 2, name: "Supper" })).toBeInTheDocument();
    const loads = vi.mocked(Events.get).mock.calls.length;

    const stored = { ...page.occurrences[0], title: "Carols" };
    vi.mocked(Events.get).mockResolvedValue({ ...page, occurrences: [stored] });
    await act(async () => answer.resolve({ occurrence: { ...stored, revision: "r1" } }));
    expect(await screen.findByRole("heading", { level: 2, name: "Carols" })).toBeInTheDocument();
    expect(Events.get).toHaveBeenCalledTimes(loads + 1);
    expect(edits.sent).toHaveLength(1);

    if (!screen.queryByRole("menuitem", { name: "Edit this date" })) await userEvent.click(screen.getByRole("button", { name: "More for 2026-12-01" }));
    await waitFor(() => expect(screen.getByRole("menuitem", { name: "Edit this date" })).toBeEnabled());
  });

  it.each([["Going", "Just this date"], ["Can't go", "All future dates"]] as const)("closes the %s scope picker when a same-event address change hides its date: %s sends nothing and the draft stays", async (answer, scope) => {
    const dates = [0, 7, 14, 21, 28, 35, 42].map((days, i) => {
      const day = new Date(Date.UTC(2026, 11, 1 + days)).toISOString().slice(0, 10);

      return occurrence({ id: i + 1, date: day, startsAt: `${day}T19:00:00Z`, endsAt: `${day}T20:00:00Z`, startsLocal: `${day}T19:00`, recurrenceRule: "FREQ=WEEKLY", guestsAllowed: 2 });
    });

    vi.mocked(Events.get).mockResolvedValue({ ...page, event: { ...page.event, recurrenceRule: "FREQ=WEEKLY", guestsAllowed: 2 }, occurrences: dates });
    const router = createMemoryRouter([{ path: "/events/:id", element: <EventDetail /> }], { initialEntries: ["/events/7?manage=2026-12-01"] });
    render(<RouterProvider router={router} />);
    const guests = await screen.findByRole("spinbutton", { name: /^Extra guests/ });
    await userEvent.clear(guests);
    await userEvent.type(guests, "2");
    await userEvent.click(within(screen.getByRole("group", { name: "Your answer" })).getByRole("button", { name: answer }));
    const choice = within(screen.getByRole("dialog", { name: /^How far should/ })).getByRole("button", { name: scope });

    // History moves to the same event with another date opened: the long list hides the picker's date.
    await act(() => router.navigate("/events/7?manage=2026-12-08"));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    act(() => { choice.click(); });
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(Events.register).not.toHaveBeenCalled();
    expect(Events.registerSeries).not.toHaveBeenCalled();

    await act(() => router.navigate(-1));
    expect(await screen.findByRole("spinbutton", { name: /^Extra guests/ })).toHaveValue(2);
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(Events.register).not.toHaveBeenCalled();
    expect(Events.registerSeries).not.toHaveBeenCalled();
  });

  it.each(["Just this date", "All future dates"] as const)("closes the Going scope picker when a held refresh stops offering remote on its date, which stays on screen: %s sends nothing", async (scope) => {
    const remote = { recurrenceRule: "FREQ=WEEKLY", remoteAvailable: true, remoteCapacity: 5, remoteSpotsLeft: 5 };

    const dates = [occurrence(remote), occurrence({
      ...remote, id: 2, date: "2026-12-08", startsAt: "2026-12-08T19:00:00Z", endsAt: "2026-12-08T20:00:00Z", startsLocal: "2026-12-08T19:00",
    })];

    const series = { ...page, canManage: false, event: { ...page.event, recurrenceRule: "FREQ=WEEKLY" }, occurrences: dates };
    const refresh = deferred<EventDetailResponse>();
    vi.mocked(Events.get).mockResolvedValueOnce(series).mockReturnValueOnce(refresh.promise);
    vi.mocked(Events.register).mockResolvedValue({ registration: {
      registrationId: 12, userId: 2, eventId: 7, occurrenceId: 2, date: "2026-12-08", status: "going",
      guestCount: 0, childcareCount: 0, childcareAgesNote: "", waitlistPosition: null, attendanceMode: "in_person",
    }, people: [] });
    renderPage();
    // An answer on the second date starts a quiet refresh, held here; both dates stay mounted.
    const answerGroups = await screen.findAllByRole("group", { name: "Your answer" });
    await userEvent.click(within(answerGroups[1]).getByRole("button", { name: "Going" }));
    await userEvent.click(screen.getByRole("button", { name: "Just this date" }));
    await waitFor(() => expect(Events.get).toHaveBeenCalledTimes(2));
    expect(Events.register).toHaveBeenCalledOnce();

    // The first date, with no saved answer: Remote, then Going opens the picker.
    await userEvent.click(screen.getAllByRole("radio", { name: "Remote" })[0]);
    await userEvent.click(within(answerGroups[0]).getByRole("button", { name: "Going" }));
    const choice = within(screen.getByRole("dialog", { name: /^How far should/ })).getByRole("button", { name: scope });

    // A Going sent now would leave out the remote answer, and the server would record in person.
    await act(async () => refresh.resolve({ ...series, occurrences: dates.map((row) => ({ ...row, remoteAvailable: false, remoteCapacity: null, remoteSpotsLeft: null })) }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    act(() => { choice.click(); });
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(Events.register).toHaveBeenCalledOnce();
    expect(Events.registerSeries).not.toHaveBeenCalled();
    expect(screen.queryByRole("radio", { name: "Remote" })).not.toBeInTheDocument();
    expect(within(screen.getAllByRole("group", { name: "Your answer" })[0]).getByRole("button", { name: "Going" })).toBeEnabled();
  });

  // Three weekly dates; an answer on the second holds a quiet refresh while the first date's picker is open.
  async function pickerDuringHeldRefresh(scope: "Just this date" | "This and following dates" | "All future dates") {
    const dates = ["2026-12-01", "2026-12-08", "2026-12-15"].map((day, i) => occurrence({
      id: i + 1, date: day, startsAt: `${day}T19:00:00Z`, endsAt: `${day}T20:00:00Z`, startsLocal: `${day}T19:00`, endsLocal: `${day}T20:00`, recurrenceRule: "FREQ=WEEKLY",
    }));

    const series = { ...page, canManage: false, event: { ...page.event, recurrenceRule: "FREQ=WEEKLY" }, occurrences: dates };
    const refresh = deferred<EventDetailResponse>();
    // Loads after the held refresh (a sent series answer) get the series again.
    vi.mocked(Events.get).mockResolvedValue(series).mockResolvedValueOnce(series).mockReturnValueOnce(refresh.promise);
    vi.mocked(Events.register).mockResolvedValue({ registration: {
      registrationId: 12, userId: 2, eventId: 7, occurrenceId: 2, date: "2026-12-08", status: "going",
      guestCount: 0, childcareCount: 0, childcareAgesNote: "", waitlistPosition: null, attendanceMode: "in_person",
    }, people: [] });
    vi.mocked(Events.registerSeries).mockResolvedValue({
      eventId: 7, scope: "all_future", status: "going",
      results: dates.map((row) => ({ occurrenceId: row.id, date: row.date, before: {}, after: { registrationId: row.id + 10, status: "going" as const } })),
      refusals: [],
    });
    renderPage();
    const answerGroups = await screen.findAllByRole("group", { name: "Your answer" });
    await userEvent.click(within(answerGroups[1]).getByRole("button", { name: "Going" }));
    await userEvent.click(screen.getByRole("button", { name: "Just this date" }));
    await waitFor(() => expect(Events.get).toHaveBeenCalledTimes(2));
    expect(Events.register).toHaveBeenCalledOnce();

    await userEvent.click(within(answerGroups[0]).getByRole("button", { name: "Going" }));
    const choice = within(screen.getByRole("dialog", { name: /^How far should/ })).getByRole("button", { name: scope });

    return { dates, series, refresh, choice };
  }

  const moved = (row: EventOccurrence, day: string, hour: number) => {
    const [start, end] = [hour, hour + 1].map((h) => `${day}T${String(h).padStart(2, "0")}:00`);

    return { ...row, startsAt: `${start}:00Z`, endsAt: `${end}:00Z`, startsLocal: start, endsLocal: end, overridden: true };
  };

  it.each([
    ["this date moves to another day, keeping its id and series date", "Just this date", (dates: EventOccurrence[]) => [moved(dates[0], "2026-12-02", 19), dates[1], dates[2]]],
    ["this date moves to another time", "All future dates", (dates: EventOccurrence[]) => [moved(dates[0], "2026-12-01", 9), dates[1], dates[2]]],
    ["a later date in the series moves", "This and following dates", (dates: EventOccurrence[]) => [dates[0], dates[1], moved(dates[2], "2026-12-16", 19)]],
    ["a later date in the series is cancelled", "All future dates", (dates: EventOccurrence[]) => [dates[0], dates[1]]],
  ] as const)("closes the Going scope picker when a held refresh shows %s, with the date still on screen: %s sends nothing", async (_change, scope, change) => {
    const { dates, series, refresh, choice } = await pickerDuringHeldRefresh(scope);

    await act(async () => refresh.resolve({ ...series, occurrences: change(dates) }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    act(() => { choice.click(); });
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(Events.register).toHaveBeenCalledOnce();
    expect(Events.registerSeries).not.toHaveBeenCalled();
    // The first date is still on screen, free to answer again.
    const answerGroups = screen.getAllByRole("group", { name: "Your answer" });
    expect(answerGroups).toHaveLength(change(dates).length);
    expect(within(answerGroups[0]).getByRole("button", { name: "Going" })).toBeEnabled();
  });

  it("closes the Going scope picker when a held refresh shows a leader restored this person on a later date, with every date and time unchanged: All future dates sends nothing", async () => {
    const dates = ["2026-12-01", "2026-12-08", "2026-12-15"].map((day, i) => occurrence({
      id: i + 1, date: day, startsAt: `${day}T19:00:00Z`, endsAt: `${day}T20:00:00Z`, startsLocal: `${day}T19:00`, endsLocal: `${day}T20:00`, recurrenceRule: "FREQ=WEEKLY",
    }));

    // A leader removed this person from the third date: a series answer cannot reach it.
    const removedLater = dates.map((row) => (row.id === 3 ? { ...row, myRegistration: "removed" as const } : row));
    const series = { ...page, canManage: false, event: { ...page.event, recurrenceRule: "FREQ=WEEKLY" }, occurrences: removedLater };
    const refresh = deferred<EventDetailResponse>();
    vi.mocked(Events.get).mockResolvedValue(series).mockResolvedValueOnce(series).mockReturnValueOnce(refresh.promise);
    vi.mocked(Events.register).mockResolvedValue({ registration: {
      registrationId: 12, userId: 2, eventId: 7, occurrenceId: 2, date: "2026-12-08", status: "going",
      guestCount: 0, childcareCount: 0, childcareAgesNote: "", waitlistPosition: null, attendanceMode: "in_person",
    }, people: [] });
    renderPage();
    const answerGroups = await screen.findAllByRole("group", { name: "Your answer" });
    expect(answerGroups).toHaveLength(2);
    expect(screen.getByText("A leader or host removed your registration for this date. Ask them to add you back.")).toBeInTheDocument();
    await userEvent.click(within(answerGroups[1]).getByRole("button", { name: "Going" }));
    await userEvent.click(screen.getByRole("button", { name: "Just this date" }));
    await waitFor(() => expect(Events.get).toHaveBeenCalledTimes(2));

    // The first date stays mounted with the same terms while the picker is open.
    await userEvent.click(within(answerGroups[0]).getByRole("button", { name: "Going" }));
    const choice = within(screen.getByRole("dialog", { name: /^How far should/ })).getByRole("button", { name: "All future dates" });

    // A leader restored the third date: All future dates would now answer it too, which the open picker never covered.
    await act(async () => refresh.resolve({ ...series, occurrences: dates }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    act(() => { choice.click(); });
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(Events.register).toHaveBeenCalledOnce();
    expect(Events.registerSeries).not.toHaveBeenCalled();
    const after = screen.getAllByRole("group", { name: "Your answer" });
    expect(after).toHaveLength(3);
    expect(within(after[0]).getByRole("button", { name: "Going" })).toBeEnabled();
  });

  it("keeps the Going scope picker open through a held refresh that changes only answers and seat counts, and sends the choice", async () => {
    const { dates, series, refresh, choice } = await pickerDuringHeldRefresh("All future dates");

    await act(async () => refresh.resolve({ ...series, occurrences: dates.map((row) => row.id === 2
      ? { ...row, myRegistration: "going" as const, seatsUsed: 1, registrations: { ...row.registrations, going: 1 } }
      : { ...row, seatsUsed: 3 }) }));
    expect(screen.getByRole("dialog", { name: /^How far should/ })).toBeInTheDocument();
    await userEvent.click(choice);
    await waitFor(() => expect(Events.registerSeries).toHaveBeenCalledOnce());
    expect(Events.registerSeries).toHaveBeenCalledWith(7, expect.objectContaining({ status: "going", scope: "all_future" }));
  });

  it("lets an editor that a same-event address change kept open save again once its update answers, sending only what changed since", async () => {
    const answer = deferred<OccurrenceResponse>();
    edits.answer(answer.promise);
    const router = createMemoryRouter([{ path: "/events/:id", element: <EventDetail /> }], { initialEntries: ["/events/7"] });
    render(<DateWritesProvider credential={credential}><RouterProvider router={router} /></DateWritesProvider>);
    await userEvent.click(await screen.findByRole("button", { name: "More for 2026-12-01" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Edit this date" }));
    const title = await screen.findByLabelText("Title");
    await userEvent.clear(title);
    await userEvent.type(title, "Carols");
    await waitFor(() => expect(screen.getByRole("button", { name: "Save this date" })).toBeEnabled());
    await userEvent.click(screen.getByRole("button", { name: "Save this date" }));
    await act(() => router.navigate("/events/7?manage=2026-12-01"));
    expect(await screen.findByRole("button", { name: "Saving…" })).toBeDisabled();

    // Another manager moved the date while the title was being saved.
    const stored = { ...page.occurrences[0], title: "Carols", location: "Annex" };
    vi.mocked(Events.get).mockResolvedValue({ ...page, occurrences: [stored] });
    await act(async () => answer.resolve({ occurrence: { ...stored, revision: "r1" } }));
    expect(edits.sent).toHaveLength(1);
    // The address it saved from is gone, so the editor stays open, read afresh on the stored date.
    await waitFor(() => expect(screen.getByRole("button", { name: "Save this date" })).toBeEnabled());
    expect(screen.getByLabelText("Title")).toBeEnabled();
    expect(screen.getByLabelText("Title")).toHaveValue("Carols");
    expect(screen.getByRole("textbox", { name: /^Location/ })).toHaveValue("Annex");

    await userEvent.type(screen.getByLabelText(/^Capacity for this date/), "12");
    await userEvent.click(screen.getByRole("button", { name: "Save this date" }));
    await waitFor(() => expect(edits.sent).toHaveLength(2));
    expect(edits.sent.map((sent) => sent.changes)).toEqual([{ title: "Carols" }, { capacityOverride: 12 }]);
  });

  it("enables the fields and focuses the remote limit when the server refuses it", async () => {
    edits.answer(new ApiError(422, "SECRET", { code: "invalid_remote_capacity_override" }));
    const input = await editDate();
    await userEvent.clear(input);
    await userEvent.type(input, "7");
    await userEvent.click(screen.getByRole("button", { name: "Save this date" }));
    await waitFor(() => expect(input).toHaveAttribute("aria-invalid", "true"));
    expect(input).toBeEnabled();
    expect(input).toHaveFocus();
    expect(input).toHaveValue("7");
  });
});

// W6a: the kept-dates panel fails closed on the event page and never loses the draft; a panel write that may
// have changed the date asks for the page again once, so a question about the date never outlives it.
describe("EventDetail and the kept-dates panel (DEC-156)", () => {
  beforeEach(() => {
    vi.resetAllMocks();
    vi.restoreAllMocks();
    guarded();
    nativePromptsFail();
    vi.mocked(Households.mine).mockResolvedValue({ household: null });
    vi.mocked(Events.get).mockResolvedValue(page);
    vi.mocked(Events.managedDate).mockImplementation((_id, date) => Promise.resolve(keptRead(date)));
    vi.mocked(Events.removeRegistration).mockResolvedValue({});
    vi.spyOn(Site, "get").mockResolvedValue({ name: "Fold", timezone: "UTC", selfSignup: "off", roomsEnabled: false, pushEnabled: true });
  });

  afterEach(() => { vi.useRealTimers(); });

  function renderInSession() {
    render(<MemoryRouter initialEntries={["/events/7"]}><DateWritesProvider credential={credential}><Routes><Route path="/events/:id" element={<EventDetail />} /></Routes></DateWritesProvider></MemoryRouter>);
  }

  async function openAndSave() {
    renderInSession();
    await userEvent.click(await screen.findByRole("button", { name: "More for 2026-12-01" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Edit this date" }));
    const minutes = await screen.findByRole("spinbutton", { name: "Minutes" });
    await userEvent.clear(minutes);
    await userEvent.type(minutes, "150");
    await waitFor(() => expect(screen.getByRole("button", { name: "Save this date" })).toBeEnabled());
    await userEvent.click(screen.getByRole("button", { name: "Save this date" }));
  }

  for (const event of HOST_EVENTS) {
    for (const outcome of READ_OUTCOMES) {
      it(`event (${event}) settled ${outcome}`, async () => {
        await runHostCase(event, outcome, { openAndSave, pageLoads: () => vi.mocked(Events.get).mock.calls.length, edits, base: () => keptRead("2026-12-01"), remounts: true });
        // A recheck leaves the page alone; a Remove that ended stored or lost asks for it again, once.
        expect(Events.get).toHaveBeenCalledTimes(event === "iv" || event.startsWith("lost-") ? 2 : 1);
      });
    }
  }

  it("after a 409, a Remove and the panel's retry, the editor keeps its draft and the page is asked again once", async () => {
    edits.answer(kept409([{ date: "2026-12-01", keptCount: 1 }]));
    await openAndSave();
    await screen.findByText("Old Name");
    // After the Remove, the panel's reads (its own retry, and the one its remount makes once the page answers) find nobody kept.
    vi.mocked(Events.managedDate).mockImplementation((_id, date, pinned) => Promise.resolve(pinned ? keptRead(date) : keptRead(date, { keptCount: 0, attendees: [] })));
    await userEvent.click(screen.getByRole("button", { name: "More for Old Name" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Remove Old Name" }));
    await userEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Remove" }));
    await waitFor(() => expect(Events.get).toHaveBeenCalledTimes(2));
    expect(await screen.findByText("Nobody is keeping this date now. Save again to apply your change.")).toBeInTheDocument();
    expect(screen.getByRole("spinbutton", { name: "Minutes" })).toHaveValue(150);
    expect(Events.get).toHaveBeenCalledTimes(2);
  });

  it("W4j (d): a held panel Remove disables the date row roster's writes until it ends, then the panel reads once", async () => {
    edits.answer(kept409([{ date: "2026-12-01", keptCount: 1 }]));
    await openAndSave();
    await screen.findByText("Old Name");
    await userEvent.click(screen.getByRole("button", { name: "More for 2026-12-01" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Manage sign-ups" }));
    await screen.findByRole("button", { name: "More for Sam" });
    const write = held<unknown>();
    vi.mocked(Events.removeRegistration).mockReturnValueOnce(write.promise);
    await userEvent.click(screen.getByRole("button", { name: "More for Old Name" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Remove Old Name" }));
    await userEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Remove" }));

    const rowRoster = () => {
      const roster = screen.getByRole("button", { name: "More for Sam" }).closest("section[aria-label='Manage sign-ups']");

      if (!(roster instanceof HTMLElement)) throw new Error("no row roster");

      return roster;
    };

    expect(within(rowRoster()).getByRole("textbox", { name: /^Add someone/ })).toBeDisabled();
    await userEvent.click(screen.getByRole("button", { name: "More for Sam" }));
    expect(screen.getByRole("menuitem", { name: "Remove Sam" })).toBeDisabled();
    await userEvent.keyboard("{Escape}");
    const panelReads = () => vi.mocked(Events.managedDate).mock.calls.filter((call) => call[2] === undefined).length;
    const reads = panelReads();
    await act(async () => { write.resolve({}); await write.promise; });
    await waitFor(() => expect(within(rowRoster()).getByRole("textbox", { name: /^Add someone/ })).toBeEnabled());
    // The panel's own read after the write, and the one its editor's remount makes once the page answers.
    await waitFor(() => expect(panelReads()).toBe(reads + 2));
    expect(Events.removeRegistration).toHaveBeenCalledTimes(1);
  });
});

// A roster write on a date (Add, restore or Remove, from the date row's roster, the date editor's kept-dates panel
// or the series editor's) holds its editor's Save from its begin to its end (DEC-158, VC2): nothing is previewed or
// sent while it is on its way, a begin closes an open question as no, and a begin during the count's read opens
// nothing. A write that may have stored something asks for the page again, and the next Save counts afresh; an
// answered one turns Save back on at once, with no reread. The editor keeps its draft throughout.
describe("EventDetail: a roster write on the date holds its editor's Save", () => {
  const STALE = "This date changed while you were saving. Check it and save again.";
  const FRESH = "ffffffffffffffffffffffffffffffff";
  const roster = { removedRegistrations: [removedRow()] };
  const rowPage: EventDetailResponse = { ...page, occurrences: [{ ...page.occurrences[0], ...roster }] };

  type Source = "panel" | "series panel" | "row roster";

  const second = occurrence({ id: 2, date: "2026-12-08", startsAt: "2026-12-08T19:00:00Z", endsAt: "2026-12-08T20:00:00Z", startsLocal: "2026-12-08T19:00", endsLocal: "2026-12-08T20:00", attendeesVisible: true, attendees: [{ registrationId: 41, userId: 4, name: "Pat", status: "going", guestCount: 0, attendanceMode: "in_person" }] });
  const twoDates: EventDetailResponse = { ...page, occurrences: [...page.occurrences, second] };

  beforeEach(() => {
    vi.resetAllMocks();
    vi.restoreAllMocks();
    guarded();
    nativePromptsFail();
    vi.mocked(Households.mine).mockResolvedValue({ household: null });
    vi.mocked(Events.get).mockResolvedValue(rowPage);
    vi.mocked(Events.managedDate).mockImplementation((_id, date) => Promise.resolve(keptRead(date, roster)));
    vi.spyOn(Site, "get").mockResolvedValue({ name: "Fold", timezone: "UTC", selfSignup: "off", roomsEnabled: false, pushEnabled: true });
  });

  const minutes = () => screen.getByRole("spinbutton", { name: "Minutes" });
  const save = () => screen.getByRole("button", { name: "Save this date" });
  const seriesTitle = () => requiredElement(document.getElementById("s-title-ev7"), HTMLInputElement);
  const rosterOf = (name: string) => () => requiredElement(screen.getByText(name, { selector: "p, span, div, strong" }).closest("section[aria-label='Manage sign-ups']"), HTMLElement);

  function renderInSession(store?: RosterWrites) {
    render(<MemoryRouter initialEntries={["/events/7"]}><DateWritesProvider rosterWrites={store} credential={credential}><Routes><Route path="/events/:id" element={<EventDetail />} /></Routes></DateWritesProvider></MemoryRouter>);
  }

  async function openDateEditor(date = "2026-12-01") {
    await userEvent.click(await screen.findByRole("button", { name: `More for ${date}` }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Edit this date" }));
    await userEvent.clear(await screen.findByRole("spinbutton", { name: "Minutes" }));
    await userEvent.type(minutes(), "150");
    await waitFor(() => expect(save()).toBeEnabled());
  }

  // Opens the source's roster beside an editor drafting Minutes 150 and Private, and returns that roster and the
  // sign-up its Remove removes.
  async function editorBeside(source: Source, onDate = "2026-12-01") {
    if (source === "series panel") {
      vi.mocked(Events.update).mockRejectedValueOnce(kept409([{ date: "2026-12-01", keptCount: 1 }]));
      await userEvent.click(await screen.findByRole("button", { name: "Edit event" }));
      await userEvent.clear(seriesTitle());
      await userEvent.type(seriesTitle(), "Series draft");
      await userEvent.click(screen.getByRole("button", { name: "Save series" }));
      await screen.findByText("Old Name");
    }

    await openDateEditor();

    if (source === "panel") {
      edits.answer(kept409([{ date: "2026-12-01", keptCount: 1 }]));
      await userEvent.click(save());
      await screen.findByText("Old Name");
    }

    await userEvent.click(screen.getByRole("radio", { name: /^Private/ }));

    if (source === "row roster") {
      await userEvent.click(screen.getByRole("button", { name: `More for ${onDate}` }));
      await userEvent.click(screen.getByRole("menuitem", { name: "Manage sign-ups" }));
      const name = onDate === "2026-12-01" ? "Sam" : "Pat";
      await screen.findByRole("button", { name: `More for ${name}` });

      return { scope: rosterOf(name), name };
    }

    return { scope: rosterOf("Old Name"), name: "Old Name" };
  }

  function expectDraft(source: Source) {
    expect(minutes()).toHaveValue(150);
    expect(screen.getByRole("radio", { name: /^Private/ })).toBeChecked();

    if (source === "series panel") expect(seriesTitle()).toHaveValue("Series draft");
  }

  for (const source of ["panel", "series panel", "row roster"] as const) {
    for (const op of ROSTER_OPS) {
      const ends = endsFor(op);

      for (const end of ends) {
        it(`${source}: a held ${op} disables Save and sends nothing; ending ${end} ${end === "answered" || end === "event_full" ? "turns Save on with no reread" : "rereads, then one save asks the writer's fresh question"}`, async () => {
          renderInSession();
          const { scope, name } = await editorBeside(source);
          const puts = edits.sent.length;
          const loads = vi.mocked(Events.get).mock.calls.length;
          const write = await startHeld(op, scope, name);

          expect(save()).toBeDisabled();
          await userEvent.click(save());
          expect(edits.sent).toHaveLength(puts);
          expect(screen.queryByRole("dialog", { name: "Save this date?" })).not.toBeInTheDocument();
          // Typing goes on: only Save waits.
          expect(minutes()).toBeEnabled();
          expectDraft(source);

          const reread = held<EventDetailResponse>();

          const answered = end === "answered" || end === "event_full";

          if (!answered) vi.mocked(Events.get).mockReturnValueOnce(reread.promise);
          await act(async () => { ROSTER_ENDS[end](write); await write.promise.catch(() => {}); });

          if (answered) {
            expect(save()).toBeEnabled();
            expect(Events.get).toHaveBeenCalledTimes(loads);
          } else {
            expect(Events.get).toHaveBeenCalledTimes(loads + 1);
            expect(save()).toBeDisabled();
            expectDraft(source);
            await act(async () => { reread.resolve(rowPage); await reread.promise; });
            // The page's answer reads the date again for the editor, which comes back with the draft.
            await waitFor(() => expect(save()).toBeEnabled());
            expect(Events.get).toHaveBeenCalledTimes(loads + 1);
          }

          expectDraft(source);
          expect(edits.sent).toHaveLength(puts);
          edits.answer(challenge(3, FRESH));
          await userEvent.click(save());
          const asked = await screen.findByRole("dialog", { name: "Save this date?" });
          expect(asked).toHaveTextContent("This cancels and notifies 3 registered people");
          await userEvent.click(within(asked).getByRole("button", { name: "Save and notify" }));
          await waitFor(() => expect(edits.sent).toHaveLength(puts + 2));
          expect(edits.sent[puts + 1]).toMatchObject({ attempt: edits.sent[puts].attempt, changes: { durationMinutes: 150, visibility: "private" }, consent: FRESH });
        });
      }

      it(`${source}: a ${op} that begins while the save is on its way opens no question and sends nothing more`, async () => {
        renderInSession();
        const { scope, name } = await editorBeside(source);
        const puts = edits.sent.length;
        const sent = held<OccurrenceResponse>();
        edits.answer(sent.promise);
        await userEvent.click(save());
        await waitFor(() => expect(edits.sent).toHaveLength(puts + 1));
        const write = await startHeld(op, scope, name);
        await act(async () => { sent.reject(challenge(2)); await sent.promise.catch(() => {}); });
        expect(screen.queryByRole("dialog", { name: "Save this date?" })).not.toBeInTheDocument();
        await waitFor(() => expect(screen.getByText(STALE)).toBeInTheDocument());
        expect(edits.sent).toHaveLength(puts + 1);
        expect(save()).toBeDisabled();
        expectDraft(source);
        await act(async () => { ROSTER_ENDS.answered(write); await write.promise.catch(() => {}); });
        expect(save()).toBeEnabled();
        expect(edits.sent).toHaveLength(puts + 1);
      });
    }
  }

  it("a write that begins while the question is open closes it as no, sends nothing and keeps focus in the editor", async () => {
    const store = createRosterWrites();
    renderInSession(store);
    await openDateEditor();
    await userEvent.click(screen.getByRole("radio", { name: /^Private/ }));
    const loads = vi.mocked(Events.get).mock.calls.length;
    edits.answer(challenge(2));
    await userEvent.click(save());
    expect(await screen.findByRole("dialog", { name: "Save this date?" })).toHaveTextContent("This cancels and notifies 2 registered people");
    let end: (e: "answered") => void = () => {};

    await act(async () => { end = store.begin("7:2026-12-01"); });
    expect(screen.queryByRole("dialog", { name: "Save this date?" })).not.toBeInTheDocument();
    expect(screen.getByText(STALE)).toBeInTheDocument();
    expect(save()).toBeDisabled();
    expect(document.activeElement).toBe(save().closest("form"));
    expect(edits.sent).toHaveLength(1);
    expectDraft("panel");
    // event_full: nothing changed, so Save is back at once with no reread, and the next Save asks once and sends once.
    act(() => end("answered"));
    expect(save()).toBeEnabled();
    expect(Events.get).toHaveBeenCalledTimes(loads);
    edits.answer(challenge(2, FRESH));
    await userEvent.click(save());
    await userEvent.click(within(await screen.findByRole("dialog", { name: "Save this date?" })).getByRole("button", { name: "Save and notify" }));
    await waitFor(() => expect(edits.sent).toHaveLength(3));
    expect(edits.sent[2]).toMatchObject({ attempt: edits.sent[1].attempt, changes: { durationMinutes: 150, visibility: "private" }, consent: FRESH });
  });

  it("a page answer that no longer lets this person manage the event takes the editor away, and nothing is sent", async () => {
    renderInSession();
    const { scope, name } = await editorBeside("panel");
    const write = await startHeld("remove", scope, name);
    const reread = held<EventDetailResponse>();
    vi.mocked(Events.get).mockReturnValueOnce(reread.promise);
    await act(async () => { write.resolve({}); await write.promise; });
    await act(async () => { reread.resolve({ ...page, canManage: false }); await reread.promise; });
    await waitFor(() => expect(screen.queryByRole("spinbutton", { name: "Minutes" })).not.toBeInTheDocument());
    expect(edits.sent).toHaveLength(1);
  });

  it("a page answer that fails keeps the date closed, and nothing is sent", async () => {
    renderInSession();
    const { scope, name } = await editorBeside("row roster");
    const write = await startHeld("remove", scope, name);
    const reread = held<EventDetailResponse>();
    vi.mocked(Events.get).mockReturnValueOnce(reread.promise);
    await act(async () => { write.resolve({}); await write.promise; });
    await act(async () => { reread.reject(new ApiError(500, "SECRET")); await reread.promise.catch(() => {}); });
    expect(await screen.findByRole("button", { name: "Try again" })).toBeInTheDocument();
    expect(await screen.findByText("This date can be edited again once the page loads. What you typed is kept.")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Save this date" })).not.toBeInTheDocument();
    expect(edits.sent).toHaveLength(0);
    // The page's Try again brings the date back with the draft.
    await userEvent.click(screen.getByRole("button", { name: "Try again" }));
    await waitFor(() => expect(save()).toBeEnabled());
    expectDraft("row roster");
  });

  it("control: a write on another date leaves this date's Save alone until the page answers, then reads it again", async () => {
    vi.mocked(Events.get).mockResolvedValue(twoDates);
    renderInSession();
    const { scope, name } = await editorBeside("row roster", "2026-12-08");
    const write = await startHeld("remove", scope, name);
    expect(save()).toBeEnabled();
    await act(async () => { write.resolve({}); await write.promise; });
    // The page asks again after that write; its answer reads this date again, keeping the draft.
    await waitFor(() => expect(Events.get).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(save()).toBeEnabled());
    expectDraft("row roster");
    edits.answer(challenge(2));
    await userEvent.click(save());
    const asked = await screen.findByRole("dialog", { name: "Save this date?" });
    await userEvent.click(within(asked).getByRole("button", { name: "Save and notify" }));
    await waitFor(() => expect(edits.sent).toHaveLength(2));
  });

  it("a Remove that begins and ends while the save is on its way, before the page renders again, never shows the question", async () => {
    renderInSession();
    const { scope, name } = await editorBeside("row roster");
    const sent = held<OccurrenceResponse>();
    edits.answer(sent.promise);
    vi.mocked(Events.get).mockReturnValueOnce(held<EventDetailResponse>().promise);
    await userEvent.click(save());
    await waitFor(() => expect(edits.sent).toHaveLength(1));
    const write = await startHeld("remove", scope, name);
    const dialogs: boolean[] = [];
    const observer = new MutationObserver(() => dialogs.push(screen.queryByRole("dialog", { name: "Save this date?" }) !== null));
    observer.observe(document.body, { subtree: true, childList: true });
    await act(async () => {
      write.resolve({});
      await write.promise;
      sent.reject(challenge(2));
      await sent.promise.catch(() => {});
    });
    observer.disconnect();
    expect(dialogs).not.toContain(true);
    await waitFor(() => expect(screen.getByText(STALE)).toBeInTheDocument());
    expect(edits.sent).toHaveLength(1);
  });

  it("a row roster's stored Remove still asks for the page once", async () => {
    renderInSession();
    await userEvent.click(await screen.findByRole("button", { name: "More for 2026-12-01" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Manage sign-ups" }));
    await screen.findByRole("button", { name: "More for Sam" });
    const write = await startHeld("remove", rosterOf("Sam"), "Sam");
    await act(async () => { write.resolve({}); await write.promise; });
    await waitFor(() => expect(Events.get).toHaveBeenCalledTimes(2));
    await act(async () => {});
    expect(Events.get).toHaveBeenCalledTimes(2);
  });
});

// VC2 on the event page: the event's visibility review opens and is confirmed only when no sign-up update on the
// event began, ended or is on its way since its request was sent, with Add, Add back and Remove from the date
// row's roster beside it. Save waits, saying why, while one is on its way.
describe("EventDetail: the event visibility review and the date row's roster", () => {
  const STALE = "A sign-up update started while we were checking. Please review again once it finishes.";
  const WAITING = "Saving waits for a sign-up update to finish.";
  const A = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
  const B = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb";
  const tightening = (consent: string, registrants = 1) => ({ visibility: "invite_only" as const, currentVisibility: "public" as const, tightened: [{ id: 7, title: "Hybrid study", visibility: "public" as const, registrants }], leaderOpened: [], consent });
  const managing: EventDetailResponse = { ...page, canSetVisibility: true, occurrences: [{ ...page.occurrences[0], removedRegistrations: [removedRow()] }] };

  beforeEach(() => {
    vi.resetAllMocks();
    vi.restoreAllMocks();
    guarded();
    nativePromptsFail();
    vi.mocked(Households.mine).mockResolvedValue({ household: null });
    vi.mocked(Events.get).mockResolvedValue(managing);
    vi.mocked(Events.visibilityImpact).mockResolvedValue({ visibility: "public", currentVisibility: "public", tightened: [], leaderOpened: [] });
    vi.spyOn(Site, "get").mockResolvedValue({ name: "Fold", timezone: "UTC", selfSignup: "off", roomsEnabled: false, pushEnabled: true });
  });

  const save = () => screen.getByRole("button", { name: "Save visibility" });
  const sam = () => requiredElement(screen.getByRole("button", { name: "More for Sam" }).closest("section[aria-label='Manage sign-ups']"), HTMLElement);

  async function openBoth() {
    render(<MemoryRouter initialEntries={["/events/7"]}><DateWritesProvider credential={credential}><Routes><Route path="/events/:id" element={<EventDetail />} /></Routes></DateWritesProvider></MemoryRouter>);
    await userEvent.click(await screen.findByRole("button", { name: "Edit event" }));
    await userEvent.click(screen.getByRole("radio", { name: /^Invite only/ }));
    await userEvent.click(screen.getByRole("button", { name: "More for 2026-12-01" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Manage sign-ups" }));
    await screen.findByRole("button", { name: "More for Sam" });
  }

  for (const op of ROSTER_OPS) {
    it(`a ${op} that begins during the preview's await opens no review; Save waits, then previews again after a stored end`, async () => {
      await openBoth();
      const preview = held<ReturnType<typeof tightening>>();
      vi.mocked(Events.visibilityImpact).mockReturnValueOnce(preview.promise);
      await userEvent.click(save());
      const write = await startHeld(op, sam, "Sam");
      await act(async () => { preview.resolve(tightening(A)); await preview.promise; });
      expect(screen.queryByRole("dialog", { name: "Making this event invite-only" })).not.toBeInTheDocument();
      expect(screen.getByText(STALE)).toBeInTheDocument();
      expect(Events.setVisibility).not.toHaveBeenCalled();
      expect(save()).toBeDisabled();
      expect(screen.getByText(WAITING)).toBeInTheDocument();
      expect(screen.getByRole("radio", { name: /^Invite only/ })).toBeChecked();
      await act(async () => { ROSTER_ENDS.stored(write); await write.promise; });
      await waitFor(() => expect(Events.get).toHaveBeenCalledTimes(2));
      await waitFor(() => expect(save()).toBeEnabled());
      expect(screen.queryByText(WAITING)).not.toBeInTheDocument();
      expect(screen.getByRole("radio", { name: /^Invite only/ })).toBeChecked();
      vi.mocked(Events.visibilityImpact).mockResolvedValueOnce(tightening(B, 2));
      await userEvent.click(save());
      const review = await screen.findByRole("dialog", { name: "Making this event invite-only" });
      expect(review).toHaveTextContent("2 people registered will be cancelled and notified");
      vi.mocked(Events.setVisibility).mockResolvedValueOnce({ event: managing.event });
      await userEvent.click(within(review).getByRole("button", { name: "Make this event invite-only" }));
      await waitFor(() => expect(Events.setVisibility).toHaveBeenCalledExactlyOnceWith(7, { visibility: "invite_only", reason: undefined, consent: B, resetOverrides: false }));
    });

    it(`a ${op} that begins while the review is open closes it with the stale line and sends nothing; an answered end turns Save on at once`, async () => {
      await openBoth();
      vi.mocked(Events.visibilityImpact).mockResolvedValueOnce(tightening(A));
      await userEvent.click(save());
      await screen.findByRole("dialog", { name: "Making this event invite-only" });
      const write = await startHeld(op, sam, "Sam");
      expect(screen.queryByRole("dialog", { name: "Making this event invite-only" })).not.toBeInTheDocument();
      expect(screen.getByText(STALE)).toBeInTheDocument();
      expect(save()).toBeDisabled();
      // The person's own roster flow keeps its focus: the review's close never pulls it to the visibility form.
      expect(document.activeElement).not.toBe(document.body);
      expect(save().closest("form")?.contains(document.activeElement)).toBe(false);
      const loads = vi.mocked(Events.get).mock.calls.length;
      await act(async () => { ROSTER_ENDS.answered(write); await write.promise.catch(() => {}); });
      expect(save()).toBeEnabled();
      expect(Events.get).toHaveBeenCalledTimes(loads);
      expect(Events.setVisibility).not.toHaveBeenCalled();
      // The stale line said only that an update started, which stays true.
      expect(screen.getByText(STALE)).toBeInTheDocument();
      expect(screen.queryByText("Registrations changed. Please review again.")).not.toBeInTheDocument();
    });
  }
});

// B2q on the event page: the page's own dialog asks a date edit's question only from the writer's answer for
// exactly this save, and anything that retires what asked it closes it as no; a Yes kept from it sends nothing.
describe("EventDetail: the date question is bound to what asked it", () => {
  const T1 = "11111111111111111111111111111111";
  const T2 = "22222222222222222222222222222222";
  const asked = () => screen.queryByRole("dialog", { name: "Save this date?" });
  const saveButton = () => screen.getByRole("button", { name: "Save this date" });
  const twoDates: EventDetailResponse = { ...page, occurrences: [page.occurrences[0], occurrence({ id: 2, date: "2026-12-08", startsAt: "2026-12-08T19:00:00Z", endsAt: "2026-12-08T20:00:00Z", startsLocal: "2026-12-08T19:00" })] };

  beforeEach(() => {
    vi.mocked(Events.get).mockResolvedValue(twoDates);
    vi.mocked(Households.mine).mockResolvedValue({ household: null });
    vi.spyOn(Site, "get").mockResolvedValue({ name: "Fold", timezone: "UTC", selfSignup: "off", roomsEnabled: false, pushEnabled: true });
    vi.spyOn(window, "confirm").mockImplementation(() => { throw new Error("the browser's prompt was asked"); });
  });

  afterEach(() => { setMarkerCookie(credential.marker); });

  function host(store?: RosterWrites) {
    const router = createMemoryRouter([
      { path: "/events/:id", element: <EventDetail /> },
      { path: "/elsewhere", element: <p>Elsewhere</p> },
    ], { initialEntries: ["/events/7"] });

    const tree = (c: Credential | null) => <DateWritesProvider rosterWrites={store} credential={c}><RouterProvider router={router} /></DateWritesProvider>;
    const view = render(tree(credential));

    return { router, swap: (c: Credential | null) => view.rerender(tree(c)) };
  }

  async function ask(answer: Error = challenge(2, T1)) {
    await userEvent.click(await screen.findByRole("button", { name: "More for 2026-12-01" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Edit this date" }));
    await userEvent.click(await screen.findByRole("radio", { name: /^Private/ }));
    edits.answer(answer);
    await waitFor(() => expect(saveButton()).toBeEnabled());
    await userEvent.click(saveButton());

    return within(await screen.findByRole("dialog", { name: "Save this date?" })).getByRole("button", { name: "Save and notify" });
  }

  it("a Yes kept from a question that closed answers nothing once it opens again; a second 409 of the same count needs its own Yes", async () => {
    host();
    const kept = await ask();
    await userEvent.keyboard("{Escape}");
    expect(asked()).not.toBeInTheDocument();
    edits.answer(challenge(2, T1));
    await userEvent.click(saveButton());
    const reopened = await screen.findByRole("dialog", { name: "Save this date?" });
    await userEvent.click(kept);
    expect(edits.sent).toHaveLength(2);
    edits.answer(challenge(2, T2));
    await userEvent.click(within(reopened).getByRole("button", { name: "Save and notify" }));
    const third = await screen.findByRole("dialog", { name: "Save this date?" });
    expect(third).toHaveAccessibleDescription(expect.stringMatching(/^We checked again before saving\. Tuesday, December 1, 2026\. /));
    expect(edits.sent).toHaveLength(3);
    await userEvent.click(within(third).getByRole("button", { name: "Save and notify" }));
    await waitFor(() => expect(edits.sent).toHaveLength(4));
    expect(edits.sent.map((s) => s.consent)).toEqual([undefined, undefined, T1, T2]);
    expect(new Set(edits.sent.slice(1).map((s) => s.attempt)).size).toBe(1);
  });

  const invalidations: Array<[string, (h: ReturnType<typeof host>, store: RosterWrites) => Promise<void> | void]> = [
    ["the same address again", async ({ router }) => { await act(() => router.navigate("/events/7")); }],
    ["another page and back", async ({ router }) => { await act(() => router.navigate("/elsewhere")); await act(() => router.navigate("/events/7")); }],
    ["the page's answer after a sign-up update on another date", async (_, store) => { await act(async () => store.begin("7:2026-12-08")("stored")); }],
    ["the page's answer that this person no longer manages the event", async (_, store) => {
      vi.mocked(Events.get).mockResolvedValue({ ...twoDates, canManage: false });
      await act(async () => store.begin("7:2026-12-08")("stored"));
    }],
    ["this tab's marker changing, once the session hears of anything", (_, store) => {
      setMarkerCookie("m-another-tab");
      act(() => { store.begin("7:2026-12-08"); });
    }],
    ["another credential", async ({ swap }) => { const next = await verifiedCredential(); act(() => swap(next)); }],
    ["signing out", ({ swap }) => { act(() => swap(null)); }],
  ];

  for (const [name, invalidate] of invalidations) {
    it(`closes the question as no on ${name}, and a Yes kept from it sends nothing`, async () => {
      const store = createRosterWrites();
      const h = host(store);
      const yes = await ask();
      await invalidate(h, store);
      expect(asked()).not.toBeInTheDocument();
      await userEvent.click(yes);
      expect(edits.sent).toHaveLength(1);
      expect(document.activeElement?.closest("[role='dialog']") ?? null).toBeNull();
      expect(window.confirm).not.toHaveBeenCalled();
    });
  }

  // An administrator erasing this manager from their own browser (not a sign-out in another tab), in App's session.
  describe("when an administrator erases this manager from their own browser", () => {
    erasureCases(() => ({
      credential: () => credential,
      sent: () => edits.sent,
      mount: (onCredential) => {
        const router = createMemoryRouter([{ path: "/events/:id", element: <EventDetail /> }], { initialEntries: ["/events/7"] });
        render(<SignedInSession onCredential={onCredential}><RouterProvider router={router} /></SignedInSession>);
      },
      draft: async () => {
        await userEvent.click(await screen.findByRole("button", { name: "More for 2026-12-01" }));
        await userEvent.click(screen.getByRole("menuitem", { name: "Edit this date" }));
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

  it.each([
    ["another date's row", new ApiError(409, "x", { code: "date_needs_consent", impact: { visibility: "private", currentVisibility: "public", tightened: [{ id: 7, title: "One date", visibility: "public", registrants: 2, occurrenceId: 2 }], leaderOpened: [], consent: T1 } })],
    ["a count of zero", challenge(0, T1)],
    ["a transition the save did not ask for", challenge(2, T1, "public", "invite_only")],
    ["a token that is not one", challenge(2, "not-a-token")],
  ])("never asks for %s", async (_, answer) => {
    host();
    await userEvent.click(await screen.findByRole("button", { name: "More for 2026-12-01" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Edit this date" }));
    await userEvent.click(await screen.findByRole("radio", { name: /^Private/ }));
    edits.answer(answer);
    await waitFor(() => expect(saveButton()).toBeEnabled());
    await userEvent.click(saveButton());
    expect(await screen.findByText(ERROR_COPY.visibility_review_outdated)).toBeInTheDocument();
    expect(asked()).not.toBeInTheDocument();
    expect(edits.sent).toHaveLength(1);
  });
});
