import { createRosterWrites, RosterWritesProvider } from "../lib/rosterWrites";
import { renderWithAuth } from "../test/auth";
import { stubService } from "../test/service";
import { act, cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Outlet, Route, RouterProvider, Routes, createMemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError } from "../api/core";
import { Dates, Events } from "../api/events";
import { Site } from "../api/site";
import type { Attendee, EventDate, EventDateRead, EventDateAttendee, EventDateSeries, EventDateUsual, EventOccurrence, OccurrenceResponse, RegistrationResponse, SeriesRegistrationResponse } from "../types";
import { coverSrc } from "../lib/covers";
import { invalidateSite } from "../lib/useSite";
import { occurrence as rosterOccurrence } from "../test/occurrence";
import { Messages } from "../api/messages";
import { UnsavedChangesBlocker } from "../lib/unsavedChanges";
import { EventDatePage } from "./EventDate";
import { stubEditSends, verifiedCredential, type EditSends } from "../test/guardedEdits";

type TestSession = { user: { id: number } | null; canCompose: boolean };

const actor: TestSession = { user: { id: 2 }, canCompose: false };

const render = renderWithAuth(() => actor);

stubService(Events, () => ({ cancelOccurrence: vi.fn(), signedUp: vi.fn(), register: vi.fn(), registerSeries: vi.fn(), undoSeriesRegistration: vi.fn(), managedDate: vi.fn(), dateVisibilityImpact: vi.fn(), removeRegistration: vi.fn(), promoteRegistration: vi.fn(), restoreRegistration: vi.fn(), addRegistration: vi.fn() }));

stubService(Dates, () => ({ get: vi.fn(), register: vi.fn(), end: vi.fn() }));

stubService(Messages, () => ({ options: vi.fn() }));

const occurrence = (overrides: Partial<EventDate> = {}): EventDate => ({ id: 51, eventId: 7, date: "2026-12-01", dateUrl: "/events/7/dates/2026-12-01", title: "One invited date", location: "Room 2", startsAt: "2026-12-01T19:00:00Z", endsAt: "2026-12-01T20:00:00Z", startsLocal: "2026-12-01T19:00", endsLocal: "2026-12-01T20:00", tzid: "UTC", durationMinutes: 60, remoteAvailable: true, guestsAllowed: 3, myRegistration: "going", myAttendanceMode: "remote", myGuestCount: 1, videoProvider: "link", videoUrl: "https://example.com/private-call", videoHidden: false, canSeeVideo: true, canSeeAttendees: false, canDecline: true, canBlock: true, ...overrides });
const registration: RegistrationResponse = { registrationId: 8, userId: 2, eventId: 7, occurrenceId: 51, date: "2026-12-01", status: "going", guestCount: 1, attendanceMode: "remote", childcareCount: 2, childcareAgesNote: "Preserve this", waitlistPosition: null };

const page = () => <MemoryRouter initialEntries={["/events/7/dates/2026-12-01"]}><Routes><Route path="/events/:id/dates/:date" element={<EventDatePage />} /><Route path="/login" element={<p>Sign in first</p>} /></Routes></MemoryRouter>;

function mount() { return render(page()); }

beforeEach(() => { vi.resetAllMocks(); actor.user = { id: 2 }; actor.canCompose = false; vi.mocked(Dates.get).mockResolvedValue({ occurrence: occurrence() }); vi.mocked(Dates.register).mockResolvedValue({ registration }); vi.mocked(Dates.end).mockResolvedValue({}); nativePromptsFail(); });

// Every question on this page is the app's dialog: the browser's prompts fail the test if anything reaches them.
function nativePromptsFail() {
  vi.spyOn(window, "confirm").mockImplementation(() => { throw new Error("native confirm on the exact-date page"); });
  vi.spyOn(window, "alert").mockImplementation(() => { throw new Error("native alert on the exact-date page"); });
}

const QUESTIONS = { "Can't go": "Can't go to this date?", "Don't add me again": "Don't add me again?" } as const;

it.each([true, false])("offers cancellation when opening a manager's date that is still underway: %s", async (underway) => {
  const end = Date.now() + (underway ? 45 * 60_000 : 0);
  const row = occurrence({ visibility: "public", canManage: true, startsAt: new Date(end - 60 * 60_000).toISOString(), endsAt: new Date(end).toISOString() });
  vi.mocked(Dates.get).mockResolvedValue({ occurrence: row });
  mount();
  const manage = await screen.findByRole("region", { name: "Manage this date" });

  if (!underway) {
    expect(within(manage).queryByRole("button", { name: "Cancel this date" })).not.toBeInTheDocument();
    expect(Events.cancelOccurrence).not.toHaveBeenCalled();

    return;
  }

  vi.mocked(Events.cancelOccurrence).mockImplementation(async () => {
    vi.mocked(Dates.get).mockRejectedValue(new ApiError(404, "Date unavailable"));

    return {};
  });
  await userEvent.click(within(manage).getByRole("button", { name: "Cancel this date" }));
  const confirmation = await screen.findByRole("dialog", { name: /^Cancel this date on / });
  expect(Events.cancelOccurrence).not.toHaveBeenCalled();
  await userEvent.click(within(confirmation).getByRole("button", { name: "Cancel this date" }));
  expect(await screen.findByText("This date was cancelled")).toBeInTheDocument();
  expect(Events.cancelOccurrence).toHaveBeenCalledExactlyOnceWith(7, "2026-12-01");
});

it.each([
  [0, "No one else will be notified."],
  [1, "1 other person will be notified."],
  [2, "2 other people will be notified."],
])("asks to cancel the date saying who is told when %s people are signed up", async (people, line) => {
  const end = Date.now() + 45 * 60_000;
  vi.mocked(Dates.get).mockResolvedValue({ occurrence: occurrence({ visibility: "public", canManage: true, startsAt: new Date(end - 60 * 60_000).toISOString(), endsAt: new Date(end).toISOString() }) });
  vi.mocked(Events.signedUp).mockResolvedValue({ people });
  vi.mocked(Events.cancelOccurrence).mockResolvedValue({});
  mount();
  const manage = await screen.findByRole("region", { name: "Manage this date" });
  await userEvent.click(within(manage).getByRole("button", { name: "Cancel this date" }));
  const confirmation = await screen.findByRole("dialog", { name: /^Cancel this date on / });
  expect(confirmation).toHaveAccessibleDescription(line);
  expect(Events.signedUp).toHaveBeenCalledExactlyOnceWith(7, "2026-12-01");
  await userEvent.click(within(confirmation).getByRole("button", { name: "Cancel this date" }));
  await waitFor(() => expect(Events.cancelOccurrence).toHaveBeenCalledExactlyOnceWith(7, "2026-12-01"));
});

it("renders only date disclosures, never hidden parent/group/host/roster/household navigation", async () => {
  const row = { ...occurrence(), groupName: "SECRET GROUP", groupId: 88, hosts: [{ name: "SECRET HOST" }], attendees: [{ name: "SECRET ATTENDEE" }], description: "SECRET DESCRIPTION" };
  vi.mocked(Dates.get).mockResolvedValue({ occurrence: row });
  mount(); await screen.findByText("One invited date");
  expect(screen.getByText("You're going to this date.")).toBeInTheDocument();
  expect(screen.queryByText(/Your answer:/)).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Going" })).toHaveAttribute("aria-pressed", "true");
  expect(screen.getByRole("button", { name: "Maybe" })).toHaveAttribute("aria-pressed", "false");
  expect(screen.queryByText(/SECRET/)).not.toBeInTheDocument();
  expect(screen.getAllByRole("link").map((a) => a.getAttribute("href"))).toEqual(["/calendar", "https://example.com/private-call", "/api/v1/events/7/dates/2026-12-01/calendar.ics"]);
  expect(screen.queryByRole("button", { name: /Manage|household|contact|all dates|Maybe for all|Can't go to any/i })).not.toBeInTheDocument();
});

it("shows an authorized ordinary group's intro under the single page heading", async () => {
  vi.mocked(Dates.get).mockResolvedValue({ occurrence: occurrence({ visibility: "private", groupId: 88, groupName: "Choir" }) });
  mount();
  expect(await screen.findByRole("heading", { level: 1, name: "One invited date" })).toBeInTheDocument();
  expect(screen.queryByRole("heading", { level: 2, name: "One invited date" })).not.toBeInTheDocument();
  expect(screen.getByRole("link", { name: "Choir" })).toHaveAttribute("href", "/groups/88");
  expect(screen.getByRole("link", { name: "Back to calendar" })).toHaveAttribute("href", "/calendar");
  expect(screen.queryByRole("button", { name: "Refresh" })).not.toBeInTheDocument();
});

it("saves only own scoped answer/mode/guest fields, preserves unsupported care by omission and reflects server waitlisting", async () => {
  mount(); await screen.findByText("One invited date");
  expect(screen.getByLabelText("Remote")).toBeChecked(); expect(screen.getByLabelText("Guests")).toHaveValue(1);
  await userEvent.click(screen.getByLabelText("In person"));
  fireEvent.change(screen.getByLabelText("Guests"), { target: { value: "2" } });
  vi.mocked(Dates.get).mockResolvedValue({ occurrence: occurrence({ myRegistration: "waitlisted", myAttendanceMode: "in_person", myGuestCount: 2, canSeeVideo: false, videoUrl: undefined, videoHidden: true }) });
  await userEvent.click(screen.getByRole("button", { name: "Going" }));
  expect(Dates.register).toHaveBeenCalledWith("7", "2026-12-01", { status: "going", attendanceMode: "in_person", guestCount: 2 });
  await screen.findByText(/^You're on the waitlist/);
  expect(screen.getByRole("button", { name: "Going" })).toHaveAttribute("aria-pressed", "true");
  expect(screen.queryByRole("link", { name: /Join video call/ })).not.toBeInTheDocument();
  expect(screen.getByLabelText("Guests")).toHaveValue(2);
});

it("preserves valid values on capacity error, validates guests, and keeps Maybe explicit", async () => {
  vi.mocked(Dates.register).mockRejectedValue(new ApiError(409, "SECRET", { code: "event_full" }));
  mount(); await screen.findByText("One invited date");
  fireEvent.change(screen.getByLabelText("Guests"), { target: { value: "4" } });
  // An invalid field is checked when an answer is chosen: it takes focus and nothing is sent.
  await userEvent.click(screen.getByRole("button", { name: "Going" }));
  expect(screen.getByLabelText("Guests")).toHaveFocus(); expect(Dates.register).not.toHaveBeenCalled();
  fireEvent.change(screen.getByLabelText("Guests"), { target: { value: "2" } });
  await userEvent.click(screen.getByRole("button", { name: "Maybe" }));
  expect(Dates.register).toHaveBeenCalledWith("7", "2026-12-01", { status: "maybe", attendanceMode: "remote", guestCount: 2 });
  expect(screen.getByRole("alert")).not.toHaveTextContent("SECRET");
  expect(screen.getByLabelText("Guests")).toHaveValue(2); expect(screen.getByLabelText("Remote")).toBeChecked();
});

it("keeps native keyboard activation and blocks disabled segment answers", async () => {
  mount(); await screen.findByText("One invited date");
  const maybe = screen.getByRole("button", { name: "Maybe" });
  maybe.focus();
  await userEvent.keyboard("{Enter}");
  expect(Dates.register).toHaveBeenCalledWith("7", "2026-12-01", { status: "maybe", attendanceMode: "remote", guestCount: 1 });
  await waitFor(() => expect(Dates.get).toHaveBeenCalledTimes(2));

  fireEvent.change(await screen.findByLabelText("Guests"), { target: { value: "4" } });
  const going = screen.getByRole("button", { name: "Going" });
  going.focus(); await userEvent.keyboard("{Enter}");
  expect(screen.getByLabelText("Guests")).toHaveFocus();
  expect(Dates.register).toHaveBeenCalledTimes(1);
});

it.each([
  ["going", "Going"], ["waitlisted", "Going"], ["maybe", "Maybe"], ["declined", "Can't go"], [null, null],
] as const)("maps %s to only the matching exact-date segment", async (status, selected) => {
  vi.mocked(Dates.get).mockResolvedValue({ occurrence: occurrence({ canRegister: true, canDecline: false, canBlock: false, myRegistration: status, myAttendanceMode: undefined, myGuestCount: undefined }) });
  mount(); await screen.findByText("One invited date");
  expect(screen.getByRole("status")).toHaveTextContent(status === "waitlisted" ? /^You're on the waitlist\./ : status === "going" ? "You're going to this date." : status === "maybe" ? "You said maybe. Change it anytime." : status === "declined" ? "You said you can't go." : "You're not in yet.");

  for (const name of ["Going", "Maybe", "Can't go"]) {
    expect(screen.getByRole("button", { name })).toHaveAttribute("aria-pressed", String(name === selected));
  }
});

it("never invents unsupported options or edit authority for an ordinary/guardian read", async () => {
  vi.mocked(Dates.get).mockResolvedValue({ occurrence: occurrence({ canDecline: false, canBlock: false, canSeeVideo: false, videoHidden: false, myAttendanceMode: undefined, myGuestCount: undefined }) });
  mount(); await screen.findByText("One invited date");
  expect(screen.queryByRole("button", { name: /Going|Maybe|Can't go|Don't add/ })).not.toBeInTheDocument();
  expect(screen.queryByRole("link", { name: /Join video call/ })).not.toBeInTheDocument();
});

it("omits missing mode and guest values instead of manufacturing defaults", async () => {
  vi.mocked(Dates.get).mockResolvedValue({ occurrence: occurrence({ remoteAvailable: false, myAttendanceMode: undefined, myGuestCount: undefined }) });
  mount(); await userEvent.click(await screen.findByRole("button", { name: "Going" }));
  expect(Dates.register).toHaveBeenCalledWith("7", "2026-12-01", { status: "going" });
});

it.each([["Can't go", "decline"], ["Don't add me again", "block"]] as const)("confirms %s in the app's dialog with safe scope copy, clears every disclosure, and never falls back", async (label, action) => {
  mount(); await screen.findByText("One invited date");
  const opener = screen.getByRole("button", { name: label });
  await userEvent.click(opener);
  const asked = screen.getByRole("dialog", { name: QUESTIONS[label] });
  expect(asked).toHaveTextContent("Other accepted dates stay yours");
  expect(within(asked).getByRole("button", { name: "Cancel" })).toHaveFocus();
  await userEvent.keyboard("{Escape}");
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument(); expect(opener).toHaveFocus(); expect(Dates.end).not.toHaveBeenCalled();
  await userEvent.click(opener);
  const yes = within(screen.getByRole("dialog", { name: QUESTIONS[label] })).getByRole("button", { name: label });
  act(() => { yes.click(); yes.click(); });
  await waitFor(() => expect(Dates.end).toHaveBeenCalledWith("7", "2026-12-01", action));
  expect(Dates.end).toHaveBeenCalledOnce();
  await screen.findByText("This date is not available");
  expect(screen.queryByText("Room 2")).not.toBeInTheDocument(); expect(screen.queryByText("One invited date")).not.toBeInTheDocument();
  expect(screen.queryByRole("link", { name: /Join video call/ })).not.toBeInTheDocument();
  expect(Dates.get).toHaveBeenCalledTimes(1); expect(Dates.register).not.toHaveBeenCalled();
});

it.each([403, 404])("clears all prior date/video data on mutation %s without an ordinary RSVP retry", async (status) => {
  vi.mocked(Dates.get).mockResolvedValueOnce({ occurrence: occurrence() }).mockRejectedValue(new ApiError(404, "missing", { code: "not_found" }));
  vi.mocked(Dates.register).mockRejectedValue(new ApiError(status, "SECRET", { code: status === 403 ? "not_allowed" : "not_found" }));
  mount(); await userEvent.click(await screen.findByRole("button", { name: "Going" }));
  await screen.findByText("This date is not available"); expect(screen.queryByText("One invited date")).not.toBeInTheDocument();
  expect(screen.queryByRole("link", { name: /Join video call/ })).not.toBeInTheDocument(); expect(Dates.register).toHaveBeenCalledTimes(1);
});

it.each([403, 404, 500])("fails closed on focus refresh %s and ignores a late mutation", async (status) => {
  let finish!: (value: { registration: RegistrationResponse }) => void;
  vi.mocked(Dates.register).mockImplementation(() => new Promise((resolve) => { finish = resolve; }));
  mount(); await userEvent.click(await screen.findByRole("button", { name: "Going" }));
  vi.mocked(Dates.get).mockRejectedValue(new ApiError(status, "SECRET"));
  fireEvent(window, new Event("focus"));
  expect(screen.queryByText("One invited date")).not.toBeInTheDocument();
  await screen.findByText("This date is not available");
  await act(async () => finish({ registration }));
  expect(screen.queryByText("One invited date")).not.toBeInTheDocument(); expect(Dates.get).toHaveBeenCalledTimes(2);
});

it("ignores late reads after a newer focus refresh or unmount", async () => {
  let finish!: (value: { occurrence: EventDate }) => void;
  vi.mocked(Dates.get).mockImplementationOnce(() => new Promise((resolve) => { finish = resolve; })).mockRejectedValue(new ApiError(404, "Ended"));
  const view = mount(); fireEvent(window, new Event("focus"));
  await screen.findByText("This date is not available");
  await act(async () => finish({ occurrence: occurrence() })); expect(screen.queryByText("One invited date")).not.toBeInTheDocument();
  view.unmount(); fireEvent(window, new Event("focus")); expect(Dates.get).toHaveBeenCalledTimes(2);
});

it("loads a public exact date signed out without invitation mutations", async () => {
  actor.user = null;
  vi.mocked(Dates.get).mockResolvedValue({ occurrence: occurrence({ visibility: "public", hostedBy: "Church", canDecline: false, canBlock: false, canSeeVideo: false, videoUrl: undefined, myRegistration: null }) });
  mount(); await screen.findByText("One invited date");
  expect(Dates.get).toHaveBeenCalledWith("7", "2026-12-01");
  expect(screen.getByText("Hosted by Church")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Register as a guest" })).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Don't add me again" })).not.toBeInTheDocument();
});

it("a confirmed end clears a newer focus read that completed before the end committed", async () => {
  let finish!: (value: Record<string, never>) => void;
  vi.mocked(Dates.end).mockImplementation(() => new Promise((resolve) => { finish = resolve; }));
  mount(); await userEvent.click(await screen.findByRole("button", { name: "Can't go" }));
  await userEvent.click(within(screen.getByRole("dialog", { name: "Can't go to this date?" })).getByRole("button", { name: "Can't go" }));
  fireEvent(window, new Event("focus")); await screen.findByText("One invited date");
  await act(async () => finish({})); await screen.findByText("This date is not available");
  expect(screen.queryByRole("link", { name: /Join video call/ })).not.toBeInTheDocument();
});

it("a refresh while the question is open closes it, and the stale dialog cannot end the invitation", async () => {
  mount(); await userEvent.click(await screen.findByRole("button", { name: "Don't add me again" }));
  const yes = within(screen.getByRole("dialog", { name: "Don't add me again?" })).getByRole("button", { name: "Don't add me again" });
  fireEvent(window, new Event("focus"));
  await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  act(() => { yes.click(); });
  await screen.findByText("One invited date");
  expect(Dates.end).not.toHaveBeenCalled(); expect(Dates.get).toHaveBeenCalledTimes(2);
  // The refreshed page asks again, under its own load.
  await userEvent.click(screen.getByRole("button", { name: "Can't go" }));
  await userEvent.click(within(screen.getByRole("dialog", { name: "Can't go to this date?" })).getByRole("button", { name: "Can't go" }));
  await waitFor(() => expect(Dates.end).toHaveBeenCalledWith("7", "2026-12-01", "decline"));
});

it("leaving the page while the question is open answers no and leaves nothing behind", async () => {
  const view = mount(); await userEvent.click(await screen.findByRole("button", { name: "Can't go" }));
  expect(screen.getByRole("dialog")).toBeInTheDocument();
  view.unmount();
  await act(async () => {});
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument(); expect(Dates.end).not.toHaveBeenCalled();
  expect(document.body.querySelector("[inert]")).toBeNull();
});

it("a member with nothing unsaved moves to another page without a question", async () => {
  const router = createMemoryRouter([{
    element: <><UnsavedChangesBlocker /><Outlet /></>,
    children: [
      { path: "/events/:id/dates/:date", element: <EventDatePage /> },
      { path: "/calendar", element: <p>Calendar page</p> },
    ],
  }], { initialEntries: ["/events/7/dates/2026-12-01"] });

  render(<RouterProvider router={router} />);
  await screen.findByText("One invited date");
  await userEvent.click(screen.getByRole("link", { name: "Back to calendar" }));
  expect(await screen.findByText("Calendar page")).toBeInTheDocument();
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
});

it("a late authority refusal clears a newer read without exposing server text", async () => {
  let fail!: (cause: unknown) => void;
  vi.mocked(Dates.register).mockImplementation(() => new Promise((_resolve, reject) => { fail = reject; }));
  mount(); await userEvent.click(await screen.findByRole("button", { name: "Maybe" }));
  fireEvent(window, new Event("focus")); await screen.findByText("One invited date");
  vi.mocked(Dates.get).mockRejectedValueOnce(new ApiError(404, "missing", { code: "not_found" }));
  await act(async () => fail(new ApiError(404, "SECRET", { code: "not_found" })));
  await screen.findByText("This date is not available");
  expect(screen.getByText("Your last action could not be completed.")).toHaveFocus();
  expect(Dates.get).toHaveBeenCalledTimes(3);
  expect(screen.queryByText(/SECRET/)).not.toBeInTheDocument();
});

it("does not start another load or restore disclosures from a late mutation after unmount", async () => {
  let finish!: (value: { registration: RegistrationResponse }) => void;
  vi.mocked(Dates.register).mockImplementation(() => new Promise((resolve) => { finish = resolve; }));
  const view = mount(); await userEvent.click(await screen.findByRole("button", { name: "Going" }));
  view.unmount(); await act(async () => finish({ registration })); expect(Dates.get).toHaveBeenCalledTimes(1);
});

it("lets an existing remote attendee switch in person when remote is no longer available", async () => {
  vi.mocked(Dates.get).mockResolvedValue({ occurrence: occurrence({ remoteAvailable: false }) });
  mount(); await screen.findByText("One invited date");
  expect(screen.getByLabelText(/Remote/)).toBeDisabled();
  await userEvent.click(screen.getByLabelText("In person")); await userEvent.click(screen.getByRole("button", { name: "Going" }));
  expect(Dates.register).toHaveBeenCalledWith("7", "2026-12-01", { status: "going", attendanceMode: "in_person", guestCount: 1 });
});

it("ignores an initial read that resolves after unmount", async () => {
  let finish!: (value: { occurrence: EventDate }) => void;
  vi.mocked(Dates.get).mockImplementation(() => new Promise((resolve) => { finish = resolve; }));
  const view = mount(); view.unmount();
  await act(async () => finish({ occurrence: occurrence() }));
  expect(screen.queryByText("One invited date")).not.toBeInTheDocument(); expect(Dates.get).toHaveBeenCalledTimes(1);
});

 it("offers first ordinary RSVP options and withdraws without ending an invitation", async () => {
   vi.mocked(Dates.get).mockResolvedValue({ occurrence: occurrence({ canRegister: true, canDecline: false, canBlock: false, myRegistration: null, myAttendanceMode: undefined, myGuestCount: undefined }) });
   mount();
   await screen.findByText("One invited date");
   expect(screen.getByLabelText("In person")).toBeChecked();
   expect(screen.getByLabelText("Guests")).toHaveValue(0);
   await userEvent.click(screen.getByLabelText("Remote"));
   fireEvent.change(screen.getByLabelText("Guests"), {target:{value:"2"}});
   vi.mocked(Dates.get).mockResolvedValue({ occurrence: occurrence({canRegister:true,canDecline:false,canBlock:false,myAttendanceMode:"remote",myGuestCount:2}) });
   await userEvent.click(screen.getByRole("button",{name:"Going"}));
   await screen.findByText("You're going to this date.");
   expect(Events.register).toHaveBeenCalledWith(7,{date:"2026-12-01",status:"going",attendanceMode:"remote",guestCount:2});
   vi.mocked(Dates.get).mockResolvedValue({ occurrence: occurrence({canRegister:true,canDecline:false,canBlock:false,myRegistration:"declined"}) });
   await userEvent.click(screen.getByRole("button",{name:"Can't go"}));
   await screen.findByText("You said you can't go.");
   expect(Events.register).toHaveBeenLastCalledWith(7,{date:"2026-12-01",status:"declined"});
   expect(Dates.end).not.toHaveBeenCalled();
   expect(Dates.register).not.toHaveBeenCalled();
   expect(window.confirm).not.toHaveBeenCalled();
 });

it("shows a cancelled own answer on a scheduled date as history, with the controls of an unanswered date", async () => {
  // The server sends a cancelled row's status alone: no mode or guests to show or prefill (M8.1 brief 2.1).
  const ordinary = { canRegister: true, canDecline: false, canBlock: false, myAttendanceMode: undefined, myGuestCount: undefined } as const;
  vi.mocked(Dates.get).mockResolvedValue({ occurrence: occurrence({ ...ordinary, myRegistration: "cancelled" }) });
  const view = mount(); await screen.findByText("One invited date");
  expect(screen.getByRole("status")).toHaveTextContent("Your sign-up was cancelled.");
  const cancelled = { mode: screen.getByLabelText("In person"), guests: screen.getByLabelText("Guests") };
  expect(cancelled.mode).toBeChecked(); expect(cancelled.guests).toHaveValue(0);
  expect(screen.queryByText(/Attending:/)).not.toBeInTheDocument();

  for (const name of ["Going", "Maybe", "Can't go"]) expect(screen.getByRole("button", { name })).toHaveAttribute("aria-pressed", "false");

  expect(screen.queryByText(/This date was cancelled/)).not.toBeInTheDocument();
  view.unmount(); cleanup();

  vi.mocked(Dates.get).mockResolvedValue({ occurrence: occurrence({ ...ordinary, myRegistration: null }) });
  mount(); await screen.findByText("One invited date");
  expect(screen.getByRole("status")).toHaveTextContent(/^You're not in yet\.$/);
  expect(screen.getByLabelText("In person")).toBeChecked(); expect(screen.getByLabelText("Guests")).toHaveValue(0);

  for (const name of ["Going", "Maybe", "Can't go"]) expect(screen.getByRole("button", { name })).toHaveAttribute("aria-pressed", "false");
});

it("shows a cancelled date's tombstone: its title, date and the cancellation, with no controls", async () => {
  // A viewer who may write to people elsewhere still gets no messaging lookup on a tombstone.
  actor.canCompose = true;
  vi.mocked(Messages.options).mockResolvedValue({ canCompose: true, canMessageEveryone: false, pushEnabled: true, groups: [], events: [{ id: 7, title: "Prayer night", churchWide: true, dates: [] }] });
  vi.mocked(Dates.get).mockResolvedValue({ occurrence: { title: "Prayer night", date: "2025-12-01", cancelled: true } });
  mount();
  expect(await screen.findByRole("heading", { level: 1, name: "Prayer night" })).toBeInTheDocument();
  const date = screen.getByRole("region", { name: "This event date" });
  expect(within(date).getByText("Monday, December 1, 2025")).toBeInTheDocument();
  expect(within(date).getByText(/This date was cancelled\./)).toBeInTheDocument();
  expect(screen.getAllByRole("link").map((a) => a.getAttribute("href"))).toEqual(["/calendar", "/calendar"]);
  expect(screen.getByRole("link", { name: "Open your calendar" })).toBeInTheDocument();
  expect(screen.queryAllByRole("button")).toEqual([]);
  expect(screen.queryByRole("status")).not.toBeInTheDocument();
  expect(screen.queryByText(/Hosted by|invited|Attending|Guests|video|Reminder|waitlist/i)).not.toBeInTheDocument();
  expect(Messages.options).not.toHaveBeenCalled();
  expect(Dates.register).not.toHaveBeenCalled(); expect(Events.register).not.toHaveBeenCalled();
});

it("asks no messaging capability once a refresh turns a scheduled date into a tombstone, even when composing is newly allowed", async () => {
  vi.mocked(Messages.options).mockResolvedValue({ canCompose: true, canMessageEveryone: false, pushEnabled: true, groups: [], events: [{ id: 7, title: "One invited date", churchWide: true, dates: [] }] });
  const view = mount(); await screen.findByText("One invited date");
  vi.mocked(Dates.get).mockResolvedValue({ occurrence: { title: "Prayer night", date: "2026-12-01", cancelled: true } });
  fireEvent(window, new Event("focus"));
  expect(await screen.findByRole("heading", { level: 1, name: "Prayer night" })).toBeInTheDocument();
  // The same user may now compose (an auth refresh) while this page still shows the tombstone.
  actor.canCompose = true;
  view.rerender(page());
  await act(async () => { await Promise.resolve(); });
  expect(screen.getByRole("heading", { level: 1, name: "Prayer night" })).toBeInTheDocument();
  expect(Messages.options).not.toHaveBeenCalled();
  // A later refresh back to the scheduled date asks again, so the rerender did reach the page.
  vi.mocked(Dates.get).mockResolvedValue({ occurrence: occurrence() });
  fireEvent(window, new Event("focus"));
  expect(await screen.findByRole("link", { name: "Email people going" })).toHaveAttribute("href", "/messages/new?occurrence=51");
  expect(Messages.options).toHaveBeenCalledTimes(1); expect(Messages.options).toHaveBeenCalledWith(7);
});

it("lays the date out: back link, title, when line, place, then the date actions", async () => {
  vi.mocked(Dates.get).mockResolvedValue({ occurrence: occurrence({ startsAt: "2026-12-01T12:00:00Z", endsAt: "2026-12-01T13:30:00Z", startsLocal: "2026-12-01T12:00", endsLocal: "2026-12-01T13:30" }) });
  mount();
  const title = await screen.findByRole("heading", { level: 1, name: "One invited date" });
  const back = screen.getByRole("link", { name: "Back to calendar" });
  expect(back).toHaveTextContent("CalendarBack to calendar");
  expect(back.compareDocumentPosition(title) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  const card = screen.getByRole("region", { name: "This event date" });
  expect(within(card).getByText(/^Tuesday, Dec 1(, \d{4})? · \d/)).toBeInTheDocument();
  expect(within(card).getByText("Room 2")).toBeInTheDocument();
  expect(within(card).getByRole("link", { name: "Add to calendar" })).toHaveAttribute("href", "/api/v1/events/7/dates/2026-12-01/calendar.ics");
  expect(within(card).getByRole("button", { name: "Share" })).toBeInTheDocument();
  // The date JSON has no counts, so none are shown.
  expect(within(card).queryByText(/seats|spots|\bgoing\b.*\d/i)).not.toBeInTheDocument();
});

it("says \"invited\" only for a real exact-date invitation", async () => {
  mount();
  expect(await screen.findByText("You're invited to this date.")).toBeInTheDocument();
  cleanup();
  vi.mocked(Dates.get).mockResolvedValue({ occurrence: occurrence({ canRegister: true, canDecline: false, canBlock: false, visibility: "public", hostedBy: "Church" }) });
  mount(); await screen.findByText("One invited date");
  expect(screen.queryByText(/invited to this date/)).not.toBeInTheDocument();
});

it("shows the waitlist hint only before Going is saved, and only where the read shows a waitlist", async () => {
  const seats = { inPerson: { capacity: 4, used: 1, left: 3 }, waitlistEnabled: true };
  vi.mocked(Dates.get).mockResolvedValue({ occurrence: occurrence({ visibility: "public", seats, myRegistration: "maybe" }) });
  mount();
  expect(await screen.findByText("If it’s full, Going puts you on the waitlist.")).toBeInTheDocument();
  cleanup();
  vi.mocked(Dates.get).mockResolvedValue({ occurrence: occurrence({ visibility: "public", seats, myRegistration: "going" }) });
  mount(); await screen.findByText("One invited date");
  expect(screen.queryByText(/Going puts you on the waitlist/)).not.toBeInTheDocument();
  cleanup();
  // A grant-only read has no pools: choosing Going is never promised a waitlist place.
  vi.mocked(Dates.get).mockResolvedValue({ occurrence: occurrence({ myRegistration: "maybe" }) });
  mount(); await screen.findByText("One invited date");
  expect(document.body).not.toHaveTextContent(/waitlist/i);
});

it("shows the booked room as the place, with the typed location after it (M2.1b)", async () => {
  vi.mocked(Dates.get).mockResolvedValue({ occurrence: occurrence({ room: { id: 1, name: "Fellowship Hall", building: "Main" } }) });
  mount();
  expect(await screen.findByText("Fellowship Hall · Main · Room 2")).toBeInTheDocument();
});

it("shows only the typed location when no room is booked (M2.1b)", async () => {
  mount();
  expect(await screen.findByText("Room 2")).toBeInTheDocument();
});

it("offers Email people going for this date to someone who may write to the event (M3.4c)", async () => {
  actor.canCompose = true;
  vi.mocked(Messages.options).mockResolvedValue({ canCompose: true, canMessageEveryone: false, pushEnabled: true, groups: [], events: [{ id: 7, title: "One invited date", churchWide: true, dates: [] }] });
  mount();
  expect(await screen.findByRole("link", { name: "Email people going" })).toHaveAttribute("href", "/messages/new?occurrence=51");
  expect(Messages.options).toHaveBeenCalledWith(7);
});

it("doesn't offer it for an event the viewer can't write to, or ask when they can't compose", async () => {
  actor.canCompose = true;
  vi.mocked(Messages.options).mockResolvedValue({ canCompose: true, canMessageEveryone: false, pushEnabled: true, groups: [], events: [{ id: 8, title: "Other", churchWide: true, dates: [] }] });
  mount(); await screen.findByText("One invited date");
  await waitFor(() => expect(Messages.options).toHaveBeenCalled());
  expect(screen.queryByRole("link", { name: "Email people going" })).not.toBeInTheDocument();
  cleanup(); vi.mocked(Messages.options).mockClear(); actor.canCompose = false;
  mount(); await screen.findByText("One invited date");
  expect(Messages.options).not.toHaveBeenCalled();
});

it("shows its event's managers the date's reminder lines, each sent one linking to its history (M3.6b)", async () => {
  vi.spyOn(Site, "get").mockResolvedValue({ name: "Grace", timezone: "America/Boise", selfSignup: "off", roomsEnabled: false, pushEnabled: true });
  vi.mocked(Dates.get).mockResolvedValue({ occurrence: occurrence({ canDecline: false, canBlock: false, reminders: {
    minutes: 1440, dueAt: "2026-01-30T19:00:00Z", covered: true,
    messages: [
      { messageId: 61, kind: "going", status: "sent", sentAt: "2026-01-30T19:00:00Z", reached: 12, failed: 0, skipped: 0 },
      { messageId: 62, kind: "waitlist", status: "sent", sentAt: "2026-01-30T19:00:00Z", reached: 3, failed: 0, skipped: 0 },
    ],
  } }) });
  mount();
  expect(await screen.findByRole("heading", { level: 2, name: "Reminders" })).toBeInTheDocument();
  expect(screen.getByRole("link", { name: "Reminder emailed to 12 people · Fri, Jan 30 at 12:00 PM" })).toHaveAttribute("href", "/messages/61");
  expect(screen.getByRole("link", { name: "Waitlist note emailed to 3 people · Fri, Jan 30 at 12:00 PM" })).toHaveAttribute("href", "/messages/62");
});

it("adds the push reminder's count, linking to its history, beside the email (M3.13d3)", async () => {
  vi.spyOn(Site, "get").mockResolvedValue({ name: "Grace", timezone: "America/Boise", selfSignup: "off", roomsEnabled: false, pushEnabled: true });
  vi.mocked(Dates.get).mockResolvedValue({ occurrence: occurrence({ canDecline: false, canBlock: false, reminders: {
    minutes: 1440, dueAt: "2026-01-30T19:00:00Z", covered: true, push: true, pushDueAt: null,
    messages: [
      { messageId: 61, kind: "going", status: "sent", sentAt: "2026-01-30T19:00:00Z", reached: 12, failed: 0, skipped: 0 },
      { messageId: 63, kind: "push", status: "sent", sentAt: "2026-01-31T17:00:00Z", reached: 5, failed: 0, skipped: 1, waiting: 0 },
    ],
  } }) });
  mount();
  expect(await screen.findByRole("link", { name: "Reminder emailed to 12 people · Fri, Jan 30 at 12:00 PM" })).toHaveAttribute("href", "/messages/61");
  expect(screen.getByRole("link", { name: "Reminder pushed to 5 people · Sat, Jan 31 at 10:00 AM · 1 skipped" })).toHaveAttribute("href", "/messages/63");
});

it("says before it goes when the reminder will, and when reminders are off, without a link", async () => {
  vi.spyOn(Site, "get").mockResolvedValue({ name: "Grace", timezone: "America/Boise", selfSignup: "off", roomsEnabled: false, pushEnabled: true });
  vi.mocked(Dates.get).mockResolvedValue({ occurrence: occurrence({ startsAt: "2030-12-03T02:00:00Z", reminders: { minutes: 2880, dueAt: "2030-12-01T02:00:00Z", covered: false, messages: [] } }) });
  mount();
  expect(await screen.findByText("Reminder goes out Sat, Nov 30 at 7:00 PM")).toBeInTheDocument();
  cleanup();
  vi.mocked(Dates.get).mockResolvedValue({ occurrence: occurrence({ reminders: { minutes: 0, dueAt: null, covered: false, messages: [] } }) });
  mount();
  expect(await screen.findByText("Reminders are off for this event")).toBeInTheDocument();
  expect(screen.queryByRole("link", { name: /Reminder/ })).not.toBeInTheDocument();
});

it("shows no reminder lines to anyone the API doesn't send them to", async () => {
  mount(); await screen.findByText("One invited date");
  expect(screen.queryByText("Reminders")).not.toBeInTheDocument();
});

it("a mutation's error does not stay under the fresh data a later focus refresh brings", async () => {
  vi.mocked(Dates.register).mockRejectedValue(new ApiError(409, "SECRET", { code: "event_full" }));
  mount(); await userEvent.click(await screen.findByRole("button", { name: "Going" }));
  await screen.findByRole("alert");
  fireEvent(window, new Event("focus"));
  await screen.findByText("One invited date");
  await waitFor(() => expect(screen.queryByRole("alert")).not.toBeInTheDocument());
});

// The first screen's six answers (M8.1 brief 1, 4, 5.1). The browser is in Boise (npm test's TZ) and the event
// in New York, so the shown clock and the event's clock differ.
describe("the date's six answers", () => {
  const usual: EventDateUsual = { startsLocal: "2026-12-01T18:30", endsLocal: "2026-12-01T20:00", durationMinutes: 90, location: "", room: { id: 4, name: "Fellowship Hall", building: "" }, capacity: 12, remoteCapacity: 5, visibility: "public" };
  const series = (over: Partial<EventDateSeries> = {}): EventDateSeries => ({ recurrenceRule: "FREQ=WEEKLY;COUNT=6", upcomingDates: 4, coveredDates: 2, canAnswerSeries: true, usual, changes: ["time"], ...over });
  const names: EventDateAttendee[] = [{ name: "Ann Going", status: "going", guestCount: 1 }, { name: "Bo Going", status: "going", guestCount: 0 }, { name: "Cy Maybe", status: "maybe", guestCount: 0 }, { name: "Di Maybe", status: "maybe", guestCount: 2 }];

  const changed = (over: Partial<EventDate> = {}): EventDate => occurrence({
    title: "Tuesday Bible Study", visibility: "public", groupId: 88, groupName: "Choir", canRegister: true, canDecline: false, canBlock: false,
    tzid: "America/New_York", startsAt: "2026-12-02T00:30:00Z", endsAt: "2026-12-02T02:00:00Z", startsLocal: "2026-12-01T19:30", endsLocal: "2026-12-01T21:00", durationMinutes: 90,
    location: "", room: { id: 4, name: "Fellowship Hall", building: "" }, myRegistration: "going", myAttendanceMode: "in_person", myGuestCount: 1,
    groupHeader: { imageUrl: "", coverFocalX: 30, coverFocalY: 70, leaderName: "Lee Leader" }, canSeeAttendees: true,
    attendance: { registrations: { going: 3, maybe: 2 }, attendees: names },
    seats: { inPerson: { capacity: 12, used: 5, left: 7 }, remote: { capacity: 5, used: 1, left: 4 }, waitlistEnabled: true },
    remoteRsvp: "offered", series: series(), ...over,
  });

  const answer = (name: string) => document.querySelector<HTMLElement>(`[data-date-answer="${name}"]`);

  async function show(date: EventDate) {
    vi.mocked(Dates.get).mockResolvedValue({ occurrence: date });
    mount();
    await screen.findByRole("heading", { level: 1, name: date.title });
  }

  it("answers all six, in order, on a changed date of a shown photo-less group", async () => {
    await show(changed({ description: "Bring a notebook." }));
    expect([...document.querySelectorAll("[data-date-answer]")].map((e) => e.getAttribute("data-date-answer"))).toEqual(["identity", "when", "where", "standing", "attendance", "change"]);
    const identity = answer("identity")!;
    const cover = identity.querySelector("img")!;
    expect(cover).toHaveAttribute("src", coverSrc({ id: 88 })); expect(cover).toHaveAttribute("alt", "");
    expect(cover.style.objectPosition).toBe("30% 70%");
    expect(within(identity).getByRole("link", { name: "Choir" })).toHaveAttribute("href", "/groups/88");
    expect(identity).toHaveTextContent("Led by Lee Leader");
    expect(answer("when")).toHaveTextContent(/^Tuesday, Dec 1(, 2026)? · 5:30–7:00 PMHeld in Eastern time \(7:30 PM EST\)$/);
    expect(answer("where")).toHaveTextContent(/^Fellowship HallRemote attendance availableJoin video call \(opens in a new tab\)$/);
    expect(within(answer("where")!).getByRole("link", { name: /Join video call/ })).toHaveAttribute("href", "https://example.com/private-call");
    expect(screen.getByRole("status")).toBe(answer("standing"));
    expect(answer("standing")).toHaveTextContent(/^You're in for this date only\.You've answered 2 of 4 upcoming dates\.Attending: In personGuests: 1$/);
    expect(answer("attendance")).toHaveTextContent(/^3 going, 2 maybeShow namesSome going names are private\.7 of 12 seats left\. 4 of 5 remote seats left\.$/);
    expect(answer("change")).toHaveTextContent(/^This date starts at 5:30 PM, a different time from usual\.$/);
    const description = screen.getByText("Bring a notebook.");
    expect(description.compareDocumentPosition(answer("change")!) & Node.DOCUMENT_POSITION_PRECEDING).toBeTruthy();
    // Seats taken count guests, so they can exceed the Going rows.
    expect(screen.getByText("Seats and answers").parentElement).toHaveTextContent("In person: 5 of 12 takenRemote: 1 of 5 taken");
  });

  it("discloses names on request only, by keyboard too, and closes them when a refresh stops carrying them", async () => {
    await show(changed());
    expect(screen.queryByText("Ann Going")).not.toBeInTheDocument();
    const toggle = screen.getByRole("button", { name: "Show names" });
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    toggle.focus(); await userEvent.keyboard("{Enter}");
    expect(toggle).toHaveAttribute("aria-expanded", "true"); expect(toggle).toHaveFocus(); expect(toggle).toHaveTextContent("Hide names");
    const list = document.getElementById(toggle.getAttribute("aria-controls")!)!;
    // The names open below all six answers, the difference included.
    expect(answer("change")!.compareDocumentPosition(list) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(within(list).getByRole("region", { name: "Going names" })).toHaveTextContent("Going (2)Ann Going (+1 guest)Bo Going");
    expect(within(list).getByRole("region", { name: "Maybe names" })).toHaveTextContent("Maybe (2)Cy MaybeDi Maybe (+2 guests)");
    expect(list).not.toHaveTextContent(/declined|waitlist/i);

    // An answer's refresh that carries counts only: no names, no operable empty disclosure.
    vi.mocked(Dates.get).mockResolvedValue({ occurrence: changed({ canSeeAttendees: false, attendance: { registrations: { going: 4, maybe: 2 } } }) });
    await userEvent.click(screen.getByRole("button", { name: "Maybe" }));
    await userEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Just this date" }));
    await waitFor(() => expect(answer("attendance")).toHaveTextContent(/^4 going, 2 maybe/));
    expect(screen.queryByRole("button", { name: /Show names|Hide names/ })).not.toBeInTheDocument();
    expect(screen.queryByText("Ann Going")).not.toBeInTheDocument(); expect(answer("attendance")).not.toHaveTextContent("private");

    // Names coming back start closed.
    vi.mocked(Dates.get).mockResolvedValue({ occurrence: changed() });
    await userEvent.click(screen.getByRole("button", { name: "Going" }));
    await userEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Just this date" }));
    expect(await screen.findByRole("button", { name: "Show names" })).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByText("Ann Going")).not.toBeInTheDocument();
  });

  it("says which answer's names are private, each against its own rows", async () => {
    await show(changed({ attendance: { registrations: { going: 2, maybe: 5 }, attendees: names } }));
    expect(answer("attendance")).toHaveTextContent("Some maybe names are private.");
    cleanup();
    await show(changed({ attendance: { registrations: { going: 0, maybe: 0 }, attendees: [] } }));
    expect(answer("attendance")).toHaveTextContent(/^0 going, 0 maybe/);
    expect(screen.queryByRole("button", { name: "Show names" })).not.toBeInTheDocument(); expect(answer("attendance")).not.toHaveTextContent("private");
  });

  it("uses an uploaded cover with its focal point, zero included", async () => {
    await show(changed({ groupHeader: { imageUrl: "/uploads/cover-key.jpg", coverFocalX: 0, coverFocalY: 100, leaderName: "  " } }));
    const cover = answer("identity")!.querySelector("img")!;
    expect(cover).toHaveAttribute("src", "/uploads/cover-key.jpg"); expect(cover.style.objectPosition).toBe("0% 100%");
    expect(answer("identity")).not.toHaveTextContent("Led by");
  });

  it.each<[string, Partial<EventDate>]>([
    ["church-wide", { groupId: undefined, groupName: undefined, groupHeader: undefined }],
    // A church label wins over any group facts that arrive beside it: no cover, group link or leader.
    ["hidden-group", {}],
  ])("gives an ordinary %s date the church label and no cover, group or leader", async (_, over) => {
    await show(changed({ hostedBy: "Grace Church", ...over }));
    expect(answer("identity")).toHaveTextContent(/^Tuesday Bible StudyHosted by Grace Church$/);
    expect(document.querySelector("img")).toBeNull(); expect(screen.queryByRole("link", { name: "Choir" })).not.toBeInTheDocument();
    expect(document.body).not.toHaveTextContent("Lee Leader");
  });

  it("shows a grant-only reader its narrow facts only, however much more the read carries", async () => {
    const overfilled = changed({ visibility: undefined, canRegister: false, canDecline: true, canBlock: true, remoteAvailable: false, title: "One invited date",
      hostedBy: "SECRET HOST", groupName: "SECRET GROUP", groupId: 99, groupHeader: { imageUrl: "/uploads/SECRET.jpg", coverFocalX: 1, coverFocalY: 1, leaderName: "SECRET LEADER" },
      attendance: { registrations: { going: 9, maybe: 8, declined: 7 }, attendees: [{ name: "SECRET ATTENDEE", status: "going", guestCount: 0 }] },
      seats: { inPerson: { capacity: 3, used: 3, left: 0 }, waitlistEnabled: true, childcare: { full: true, totals: { capacity: 4, used: 4, left: 0 } } }, remoteRsvp: "closed",
      myRegistration: "waitlisted", myWaitlistPosition: 2, series: series({ coveredDates: 4, changes: ["time", "room", "visibility"] }), videoUrl: undefined, canSeeVideo: false, videoHidden: true });

    await show(overfilled);
    expect(document.body).not.toHaveTextContent(/SECRET|going,|maybe|seats|Seats|Childcare|Remote|In person only|differ|usual|series|position|Led by|Hosted by|Part of/);
    expect(document.querySelector("img")).toBeNull();
    expect(screen.getAllByRole("link").map((a) => a.getAttribute("href"))).toEqual(["/calendar", "/api/v1/events/7/dates/2026-12-01/calendar.ics"]);
    expect(screen.queryByRole("button", { name: /names/ })).not.toBeInTheDocument();
    expect(answer("standing")).toHaveTextContent(/^You're on the waitlist\.If a space opens/);
    expect(answer("where")).toHaveTextContent(/^Fellowship HallThe video link is not available to you\./);
    expect(answer("attendance")).toBeNull(); expect(answer("change")).toBeNull();
    expect(Dates.get).toHaveBeenCalledTimes(1); expect(Events.managedDate).not.toHaveBeenCalled();
  });

  it("drops every ordinary fact when a focus refresh narrows the read to a grant", async () => {
    await show(changed());
    await userEvent.click(screen.getByRole("button", { name: "Show names" }));
    vi.mocked(Dates.get).mockResolvedValue({ occurrence: changed({ visibility: undefined, canDecline: true, canRegister: false, remoteAvailable: false }) });
    fireEvent(window, new Event("focus"));
    await waitFor(() => expect(answer("standing")).toHaveTextContent(/^You're going to this date\./));
    expect(document.body).not.toHaveTextContent(/Ann Going|Choir|Lee Leader|3 going|seats|differ|Remote/);
    expect(document.querySelector("img")).toBeNull(); expect(answer("attendance")).toBeNull();
  });

  it.each<[string, Partial<EventDate>, RegExp]>([
    ["Going on every upcoming date of a visible series", { series: series({ coveredDates: 4 }) }, /^You're in for the series\.Attending/],
    ["Going with one date of four", { series: series({ coveredDates: 1 }) }, /^You're in for this date only\.You've answered 1 of 4 upcoming dates\./],
    ["Going on a one-off, even with full coverage", { series: series({ recurrenceRule: "", upcomingDates: 1, coveredDates: 1 }) }, /^You're going to this date\.Attending/],
    ["Going with no upcoming dates left", { series: series({ upcomingDates: 0, coveredDates: 0 }) }, /^You're going to this date\.Attending/],
    ["Going on an opened date of a hidden series", { series: undefined }, /^You're going to this date\.Attending/],
    ["Maybe on every upcoming date", { myRegistration: "maybe", series: series({ coveredDates: 4 }) }, /^You said maybe\. Change it anytime\.You've answered all 4 upcoming dates\./],
    ["no answer here with the other dates answered", { myRegistration: null, myAttendanceMode: undefined, myGuestCount: undefined, series: series({ coveredDates: 3 }) }, /^You're not in yet\.You've answered 3 of 4 upcoming dates\.$/],
    ["Can't go", { myRegistration: "declined", myAttendanceMode: undefined, myGuestCount: undefined }, /^You said you can't go\.$/],
    ["removed", { myRegistration: "removed", myAttendanceMode: undefined, myGuestCount: undefined }, /^Your sign-up for this date was removed\.$/],
    ["an own cancelled row", { myRegistration: "cancelled", myAttendanceMode: undefined, myGuestCount: undefined }, /^Your sign-up was cancelled\.$/],
    ["waiting with a known position and mode", { myRegistration: "waitlisted", myWaitlistPosition: 2, myAttendanceMode: "remote" }, /^You're on the waitlist\.Your position on this date's waitlist is 2\.If a space opens and your party fits, you may be moved to Going\. Fold will let you know if that happens\.Attending: Remote/],
    ["waiting with no position", { myRegistration: "waitlisted", myAttendanceMode: undefined, myGuestCount: undefined }, /^You're on the waitlist\.If a space opens and your party fits, you may be moved to Going\. Fold will let you know if that happens\.$/],
  ])("states %s in words", async (_, over, expected) => {
    await show(changed(over));
    expect(screen.getByRole("status")).toHaveTextContent(expected);
    // Waitlist copy names no deadline, channel, rank or promise (brief 2.4).
    expect(answer("standing")).not.toHaveTextContent(/email|deadline|by \d|next|guarantee|confirm|leader will/i);
  });

  it("shows a promotion as Going once the read says so, with no position left over", async () => {
    await show(changed({ myRegistration: "waitlisted", myWaitlistPosition: 1 }));
    expect(answer("standing")).toHaveTextContent("Your position on this date's waitlist is 1.");
    vi.mocked(Dates.get).mockResolvedValue({ occurrence: changed() });
    fireEvent(window, new Event("focus"));
    await waitFor(() => expect(answer("standing")).toHaveTextContent(/^You're in for this date only\./));
    expect(answer("standing")).not.toHaveTextContent(/waitlist|position/);
  });

  it("never offers the waitlist where the date has none", async () => {
    await show(changed({ myRegistration: null, myAttendanceMode: undefined, myGuestCount: undefined, seats: { inPerson: { capacity: 4, used: 4, left: 0 }, waitlistEnabled: false } }));
    expect(answer("attendance")).toHaveTextContent(/Seats full\.$/);
    expect(document.body).not.toHaveTextContent(/waitlist/i);
  });

  it("makes no personal claim to a signed-out visitor", async () => {
    actor.user = null;
    await show(changed({ myRegistration: null, myAttendanceMode: undefined, myGuestCount: undefined, canRegister: false, series: series({ upcomingDates: undefined, coveredDates: undefined }) }));
    expect(answer("standing")).toBeNull(); expect(screen.queryByRole("status")).not.toBeInTheDocument();
    expect(answer("attendance")).toHaveTextContent(/^3 going, 2 maybe/);
  });

  it.each<[string, Partial<EventDate>, string]>([
    ["day", { series: series({ changes: ["day"] }) }, "This date is on Tuesday, Dec 1, a different day from usual."],
    ["time", {}, "This date starts at 5:30 PM, a different time from usual."],
    ["duration", { durationMinutes: 150, series: series({ changes: ["duration"] }) }, "This date lasts 2 hours 30 minutes, a different length from usual."],
    ["place", { location: "Back garden", series: series({ changes: ["place"] }) }, "This date meets at Back garden, a different place from usual."],
    ["room", { series: series({ changes: ["room"] }) }, "This date is in Fellowship Hall, a different room from usual."],
    ["no room", { room: undefined, location: "Hall", series: series({ changes: ["room"] }) }, "This date has no room booked, unlike usual."],
    ["capacity", { series: series({ changes: ["capacity"] }) }, "This date has 12 seats, a different limit from usual."],
    ["unlimited capacity", { seats: { inPerson: { capacity: null, used: 9, left: null }, waitlistEnabled: false }, series: series({ changes: ["capacity"] }) }, "This date has no seat limit, unlike usual."],
    ["remote capacity", { series: series({ changes: ["remoteCapacity"] }) }, "This date has 5 remote seats, a different limit from usual."],
    ["closed remote", { remoteRsvp: "closed", seats: { inPerson: { capacity: 12, used: 5, left: 7 }, remote: { capacity: 0, used: 0, left: 0 }, waitlistEnabled: true }, series: series({ changes: ["remoteCapacity"] }) }, "This date takes no remote sign-ups, unlike usual."],
    ["visibility", { visibility: "invite_only", series: series({ changes: ["visibility"] }) }, "This date is invite-only, unlike usual."],
    ["several", { series: series({ changes: ["time", "room", "remoteCapacity"] }) }, "The start time, room and remote seat limit differ for this date."],
    ["two", { series: series({ changes: ["day", "time"] }) }, "The day and start time differ for this date."],
    ["moved day with an unknown usual clock", { series: series({ usual: { ...usual, startsLocal: "", endsLocal: "" }, changes: ["day"] }) }, "This date is on Tuesday, Dec 1, a different day from usual."],
    ["duration with an unknown clock (never a time claim)", { durationMinutes: 45, series: series({ usual: { ...usual, startsLocal: "", endsLocal: "" }, changes: ["time", "duration"] }) }, "This date lasts 45 minutes, a different length from usual."],
  ])("says one sentence for a %s change", async (_, over, sentence) => {
    await show(changed(over));
    expect(answer("change")).toHaveTextContent(new RegExp(`^${sentence.replace(/[.*+?^${}()|[\]\\]/g, "\\$&").replace("Tuesday, Dec 1", "Tuesday, Dec 1(, 2026)?")}$`));
  });

  it.each<[string, Partial<EventDate>]>([
    ["no changes or equal-valued overrides", { series: series({ changes: [] }) }],
    ["a one-off event", { series: series({ recurrenceRule: "", changes: ["time"] }) }],
    ["no series at all", { series: undefined }],
    ["only a time change against an unknown clock", { series: series({ usual: { ...usual, startsLocal: "", endsLocal: "" }, changes: ["time"] }) }],
  ])("says nothing about %s", async (_, over) => {
    await show(changed(over));
    expect(answer("change")).toBeNull(); expect(document.body).not.toHaveTextContent(/usual|differ for this date/);
  });

  it("renders the supplied clock and duration as they are, even when they disagree", async () => {
    // The page renders the supplied end and duration and recomputes neither (which one a date should show is an open choice).
    await show(changed({ endsAt: "2026-12-02T03:00:00Z", durationMinutes: 90, series: series({ changes: ["duration"] }) }));
    expect(answer("when")).toHaveTextContent(/5:30–8:00 PM/);
    expect(answer("change")).toHaveTextContent("This date lasts 1 hour 30 minutes, a different length from usual.");
  });

  it.each<[string, Partial<EventDate>, RegExp, RegExp | null]>([
    ["low in-person seats", { seats: { inPerson: { capacity: 12, used: 9, left: 3 }, waitlistEnabled: false } }, /Only 3 seats left\.$/, /In person: 9 of 12 taken$/],
    ["a full pool with a waitlist", { seats: { inPerson: { capacity: 4, used: 4, left: 0 }, waitlistEnabled: true } }, /Seats full, waitlist open\.$/, /In person: 4 of 4 taken$/],
    ["a full pool without one, overbooked", { seats: { inPerson: { capacity: 12, used: 14, left: 0 }, waitlistEnabled: false } }, /Seats full\.$/, /In person: 14 of 12 taken$/],
    ["an unlimited pool", { remoteRsvp: "none", seats: { inPerson: { capacity: null, used: 9, left: null }, waitlistEnabled: false } }, /In person: 9 taken, no limit\.$/, /In person: 9 taken, no limit$/],
    ["unlimited remote seats", { seats: { inPerson: { capacity: 12, used: 5, left: 7 }, remote: { capacity: null, used: 4, left: null }, waitlistEnabled: false } }, /Remote: 4 taken, no limit\.$/, /Remote: 4 taken, no limit$/],
    ["full remote seats", { seats: { inPerson: { capacity: null, used: 2, left: null }, remote: { capacity: 2, used: 2, left: 0 }, waitlistEnabled: false } }, /Remote seats full\.$/, /Remote: 2 of 2 taken$/],
    ["closed remote sign-up", { remoteRsvp: "closed", seats: { inPerson: { capacity: 12, used: 5, left: 7 }, remote: { capacity: 0, used: 0, left: 0 }, waitlistEnabled: true } }, /7 of 12 seats left\.$/, /In person: 5 of 12 taken$/],
    ["full childcare", { seats: { inPerson: { capacity: 12, used: 5, left: 7 }, waitlistEnabled: false, childcare: { full: true } } }, /7 of 12 seats left\. Childcare is full\.$/, null],
    ["open childcare", { seats: { inPerson: { capacity: 12, used: 5, left: 7 }, waitlistEnabled: false, childcare: { full: false } } }, /Childcare available\.$/, null],
    ["a manager's childcare totals and declined count", { attendance: { registrations: { going: 3, maybe: 2, declined: 1 }, attendees: names }, seats: { inPerson: { capacity: 12, used: 5, left: 7 }, waitlistEnabled: false, childcare: { full: false, totals: { capacity: 10, used: 3, left: 7 } } } }, /7 of 10 childcare spots left\.$/, /Childcare: 3 of 10 takenCan't go: 1$/],
  ])("summarizes %s per pool", async (_, over, summary, detail) => {
    await show(changed(over));
    expect(answer("attendance")).toHaveTextContent(summary);

    if (detail) expect(screen.getByText("Seats and answers").parentElement).toHaveTextContent(detail);
    expect(answer("attendance")).not.toHaveTextContent(/can't go/i);
  });

  it.each<[string, Partial<EventDate>, RegExp]>([
    ["closed remote with a link this reader may use", { remoteRsvp: "closed" }, /^Fellowship HallRemote sign-up is closed for this dateJoin video call/],
    ["in person only", { remoteRsvp: "none", remoteAvailable: false, videoProvider: "none", videoUrl: undefined, canSeeVideo: false }, /^Fellowship HallIn person only$/],
    ["a link kept for people marked Going", { myRegistration: "maybe", videoUrl: undefined, canSeeVideo: false, videoHidden: true }, /^Fellowship HallRemote attendance availableThe video link is shown to people marked Going\.$/],
    ["remote with no link", { videoProvider: "none", videoUrl: undefined, canSeeVideo: false }, /^Fellowship HallRemote attendance availableNo video link listed\.$/],
    ["no place at all", { room: undefined, location: "", remoteRsvp: "none", videoUrl: undefined, canSeeVideo: false }, /^No meeting place listedIn person only$/],
  ])("says where, with remote sign-up and link access apart: %s", async (_, over, where) => {
    await show(changed(over));
    expect(answer("where")).toHaveTextContent(where);

    if (!(over.canSeeVideo ?? true)) expect(document.body.innerHTML).not.toContain("private-call");
  });
});

// W7: the manager section behind the authority gate (design 8d, 8j, 8k). The page renders inside the session's
// providers, as App puts it, with a test roster-writes store.
const oldAttendee: Attendee = { registrationId: 31, userId: 5, name: "Old Name", status: "going", attendanceMode: "in_person", guestCount: 0, email: "old@example.com" };
const waiter: Attendee = { registrationId: 32, userId: 6, name: "Wait Ward", status: "waitlisted", attendanceMode: "in_person", guestCount: 0 };
const careChild = (until: string): Attendee => ({ registrationId: 33, name: "Kit", status: "going", attendanceMode: "in_person", guestCount: 0, householdPerson: { personId: 3, name: "Kit", kind: "child", age: 6, householdName: "H", registeredBy: { userId: 7, name: "Holder" }, careNotes: "Old care note", careNotesVisibleUntil: until } });
const removedRow: Attendee = { registrationId: 34, userId: 8, name: "Lou Back", status: "removed", attendanceMode: "in_person", guestCount: 0 };
const careUntil = () => new Date(Date.now() + 10 * 60_000).toISOString();

// The managed read of the page's own date (id 51), with its edit token.
const managedRead = (over: Partial<EventOccurrence> = {}): OccurrenceResponse => ({ occurrence: rosterOccurrence({ id: 51, revision: "r0", attendeesVisible: true, childcareAvailable: true, attendees: [oldAttendee, waiter, careChild(careUntil())], removedRegistrations: [removedRow], visibility: "public", seriesVisibility: "public", groupId: 88, groupName: "Choir", groupVisibility: "public", ...over }) });
const newRead = () => managedRead({ attendees: [{ ...oldAttendee, name: "New Name", email: "new@example.com" }] });
const OLD = ["Old Name", "old@example.com", "Old care note", "Kept after leaving"];

function later<T>() {
  let resolve: (value: T) => void = () => {};
  let reject: (cause: unknown) => void = () => {};
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });

  return { promise, resolve, reject };
}

// Every text the page shows after it starts, one per DOM batch.
function recordTexts() {
  const seen: string[] = [];
  const observer = new MutationObserver(() => seen.push(document.body.textContent ?? ""));
  observer.observe(document.body, { subtree: true, childList: true, characterData: true });

  return { seen, stop: () => observer.disconnect(), sawAny: (words: string[]) => seen.some((t) => words.some((w) => t.includes(w))) };
}

function expectNone(words: string[]) {
  for (const w of words) expect(document.body.textContent).not.toContain(w);
}

// The date saves the manager section sends.
let edits: EditSends;

function mountManager() {
  const store = createRosterWrites();
  const view = render(<RosterWritesProvider rosterWrites={store}>{page()}</RosterWritesProvider>);

  return { store, view };
}

async function openManagerTools({ minutes = true, roster = true } = {}) {
  await screen.findByRole("heading", { name: "Manage this date" });

  if (minutes) {
    await userEvent.click(screen.getByRole("button", { name: "Edit this date" }));
    const field = await screen.findByRole("spinbutton", { name: "Minutes" });
    await userEvent.clear(field);
    await userEvent.type(field, "150");
  }

  if (roster) {
    await userEvent.click(screen.getByRole("button", { name: "Manage sign-ups" }));
    await screen.findByText("Old Name");
  }
}

describe("the manager section", () => {
  beforeEach(async () => {
    await verifiedCredential();
    edits = stubEditSends(51);
    invalidateSite();
    vi.spyOn(Site, "get").mockResolvedValue({ name: "Grace", timezone: "UTC", selfSignup: "off", roomsEnabled: false, pushEnabled: true });
    vi.mocked(Dates.get).mockResolvedValue({ occurrence: occurrence({ canManage: true, canDecline: false, canBlock: false, myRegistration: null }) });
    vi.mocked(Events.managedDate).mockResolvedValue(managedRead());
  });

  const managerAnswer = () => ({ occurrence: occurrence({ canManage: true, canDecline: false, canBlock: false, myRegistration: null }) });

  it.each([403, 404])("reads from empty after a cancellation refusal (%s), including an open roster", async (status) => {
    const row = occurrence({ canManage: true, visibility: "private", endsAt: new Date(Date.now() + 60 * 60_000).toISOString() });
    vi.mocked(Dates.get).mockResolvedValue({ occurrence: row });
    vi.mocked(Events.cancelOccurrence).mockRejectedValue(new ApiError(status, "Refused"));
    mountManager();
    await openManagerTools({ minutes: false });
    expect(screen.getByText("Old care note", { exact: false })).toBeVisible();
    const reread = later<EventDateRead>();
    vi.mocked(Dates.get).mockReturnValue(reread.promise);

    await userEvent.click(screen.getByRole("button", { name: "Cancel this date" }));
    const question = screen.getByRole("dialog");
    await userEvent.click(within(question).getByRole("button", { name: "Cancel this date" }));

    expectNone(["One invited date", "Room 2", "Old Name", "old@example.com", "Old care note"]);
    expect(screen.queryByRole("button", { name: "Manage sign-ups" })).not.toBeInTheDocument();
    expect(screen.getByText("Checking that you can still manage this date…")).toBeVisible();
    expect(Dates.get).toHaveBeenCalledTimes(2);

    await act(async () => reread.reject(new ApiError(404, "Unavailable")));
    expect(await screen.findByText("This date is not available")).toBeVisible();
    expect(screen.queryByRole("region", { name: "Manage this date" })).not.toBeInTheDocument();
    expect(screen.getByText("Your changes could not be saved.")).toHaveFocus();
  });

  it("dismisses the cancellation question at the end and sends no cancellation", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });

    try {
      const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
      const end = Date.now() + 60_000;
      vi.mocked(Dates.get).mockResolvedValue({ occurrence: { ...managerAnswer().occurrence, startsAt: new Date(end - 10 * 60_000).toISOString(), endsAt: new Date(end).toISOString() } });
      mountManager();
      await user.click(await screen.findByRole("button", { name: "Cancel this date" }));
      const yes = within(screen.getByRole("dialog")).getByRole("button", { name: "Cancel this date" });

      act(() => { vi.advanceTimersByTime(end - Date.now()); });

      expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
      expect(screen.queryByRole("button", { name: "Cancel this date" })).not.toBeInTheDocument();
      fireEvent.click(yes);
      expect(Events.cancelOccurrence).not.toHaveBeenCalled();
    } finally {
      vi.useRealTimers();
    }
  });

  it("names the rescheduled day and submits the original occurrence date", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });

    try {
      vi.setSystemTime(new Date("2026-12-03T19:15:00Z"));
      const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
      const row = { ...managerAnswer().occurrence, startsAt: "2026-12-03T19:00:00Z", endsAt: "2026-12-03T20:00:00Z" };
      vi.mocked(Dates.get).mockResolvedValue({ occurrence: row });
      vi.mocked(Events.cancelOccurrence).mockResolvedValue({});
      mountManager();
      expect(await screen.findByText(/^Thursday, Dec 3/)).toBeVisible();

      await user.click(await screen.findByRole("button", { name: "Cancel this date" }));
      const question = screen.getByRole("dialog", { name: /^Cancel this date on Thursday, Dec 3/ });
      await user.click(within(question).getByRole("button", { name: "Cancel this date" }));
      expect(Events.cancelOccurrence).toHaveBeenCalledExactlyOnceWith(7, "2026-12-01");
    } finally {
      vi.useRealTimers();
    }
  });

  it("shows no section without canManage, and the section with it", async () => {
    vi.mocked(Dates.get).mockResolvedValue({ occurrence: occurrence() });
    const first = mountManager();
    await screen.findByText("One invited date");
    expect(screen.queryByRole("heading", { name: "Manage this date" })).not.toBeInTheDocument();
    first.view.unmount();
    vi.mocked(Dates.get).mockResolvedValue(managerAnswer());
    mountManager();
    expect(await screen.findByRole("heading", { name: "Manage this date" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Open the event page" })).toHaveAttribute("href", "/events/7");
  });

  it("(a) a pending focus refresh shows only the checking line, then a fresh read restores the held draft", async () => {
    mountManager();
    await openManagerTools();
    await userEvent.click(screen.getByRole("button", { name: "More for Old Name" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Remove Old Name" }));
    expect(screen.getByRole("dialog")).toBeInTheDocument();
    const refresh = later<{ occurrence: EventDate }>();
    vi.mocked(Dates.get).mockReturnValue(refresh.promise);
    vi.mocked(Events.managedDate).mockResolvedValue(newRead());
    const texts = recordTexts();
    fireEvent(window, new Event("focus"));
    expect(screen.getByText("Checking that you can still manage this date…")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Edit this date" })).not.toBeInTheDocument();
    expect(screen.queryByRole("spinbutton", { name: "Minutes" })).not.toBeInTheDocument();
    expectNone(OLD);
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    const before = vi.mocked(Events.managedDate).mock.calls.length;
    await act(async () => { refresh.resolve(managerAnswer()); await refresh.promise; });
    expect(await screen.findByText("New Name")).toBeInTheDocument();
    expect(vi.mocked(Events.managedDate).mock.calls.length).toBe(before + 1);
    expect(screen.getByRole("spinbutton", { name: "Minutes" })).toHaveValue(150);
    texts.stop();
    expect(texts.sawAny(["Old Name"])).toBe(false);
  });

  it.each([["a network error", new TypeError("Failed to fetch")], ["a 500", new ApiError(500, "down")]])("(b) a focus refresh failing with %s shows the unknown line, and Try again restores the draft", async (_, cause) => {
    mountManager();
    await openManagerTools();
    vi.mocked(Dates.get).mockRejectedValueOnce(cause);
    const texts = recordTexts();
    fireEvent(window, new Event("focus"));
    expect(await screen.findByText("We couldn't check that you can still manage this date. What you typed is kept.")).toBeInTheDocument();
    expectNone(OLD);
    expect(screen.queryByRole("button", { name: "Edit this date" })).not.toBeInTheDocument();
    texts.stop();
    expect(texts.sawAny(["Old Name"])).toBe(false);
    vi.mocked(Dates.get).mockResolvedValue(managerAnswer());
    await userEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByRole("spinbutton", { name: "Minutes" })).toHaveValue(150);
  });

  it("(c) a failed quiet reload that still holds an old canManage reads unknown and shows no tools", async () => {
    mountManager();
    await openManagerTools({ roster: false });
    vi.mocked(Dates.get).mockRejectedValue(new TypeError("Failed to fetch"));
    await userEvent.click(screen.getByRole("button", { name: "Save this date" }));
    expect(await screen.findByText("We couldn't check that you can still manage this date. What you typed is kept.")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Edit this date" })).not.toBeInTheDocument();
    // The page's own answer is still the date.
    expect(screen.getByText("One invited date")).toBeInTheDocument();
  });

  it("(d) a demotion removes the section without the old roster, and a later grant starts with both tools closed", async () => {
    mountManager();
    await openManagerTools();
    vi.mocked(Dates.get).mockResolvedValue({ occurrence: occurrence({ canDecline: false, canBlock: false }) });
    const texts = recordTexts();
    fireEvent(window, new Event("focus"));
    await waitFor(() => expect(screen.queryByRole("heading", { name: "Manage this date" })).not.toBeInTheDocument());
    texts.stop();
    expect(texts.sawAny(["Old Name"])).toBe(false);
    vi.mocked(Dates.get).mockResolvedValue(managerAnswer());
    fireEvent(window, new Event("focus"));
    expect(await screen.findByRole("heading", { name: "Manage this date" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Edit this date" })).toHaveAttribute("aria-expanded", "false");
    expect(screen.getByRole("button", { name: "Manage sign-ups" })).toHaveAttribute("aria-expanded", "false");
    await userEvent.click(screen.getByRole("button", { name: "Edit this date" }));
    expect(await screen.findByRole("spinbutton", { name: "Minutes" })).toHaveValue(60);
  });

  it.each([["403", () => new ApiError(403, "no")], ["404", () => new ApiError(404, "no")]])("(e) a %s refresh reads lost and removes the section", async (_, cause) => {
    mountManager();
    await openManagerTools();
    vi.mocked(Dates.get).mockRejectedValue(cause());
    fireEvent(window, new Event("focus"));
    await waitFor(() => expect(screen.queryByRole("heading", { name: "Manage this date" })).not.toBeInTheDocument());
    expectNone(OLD);
  });

  it("(e) a tombstone answer removes the section", async () => {
    mountManager();
    await openManagerTools();
    vi.mocked(Dates.get).mockResolvedValue({ occurrence: { title: "One invited date", date: "2026-12-01", cancelled: true } });
    fireEvent(window, new Event("focus"));
    expect(await screen.findByText(/This date was cancelled/)).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Manage this date" })).not.toBeInTheDocument();
    expectNone(OLD);
  });

  it("(f) a refused managed read closes the tools and retries the page; a failed one offers its own retry with the draft held", async () => {
    vi.mocked(Events.managedDate).mockRejectedValueOnce(new ApiError(404, "gone"));
    mountManager();
    await screen.findByRole("heading", { name: "Manage this date" });
    const pageReads = vi.mocked(Dates.get).mock.calls.length;
    await userEvent.click(screen.getByRole("button", { name: "Manage sign-ups" }));
    await waitFor(() => expect(vi.mocked(Dates.get).mock.calls.length).toBe(pageReads + 1));
    expect(await screen.findByRole("button", { name: "Manage sign-ups" })).toHaveAttribute("aria-expanded", "false");

    await openManagerTools({ roster: false });
    vi.mocked(Events.managedDate).mockRejectedValueOnce(new TypeError("Failed to fetch"));
    // A care boundary or another recheck would retry; a focus here retries the page and then the managed read.
    fireEvent(window, new Event("focus"));
    expect(await screen.findByText("We couldn't load this date's details. What you typed is kept.")).toBeInTheDocument();
    expect(screen.queryByRole("spinbutton", { name: "Minutes" })).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByRole("spinbutton", { name: "Minutes" })).toHaveValue(150);
  });

  it("(g) a save reloads the managed read and the page, closes the editor and clears the hold", async () => {
    mountManager();
    await openManagerTools({ roster: false });
    const pageReads = vi.mocked(Dates.get).mock.calls.length;
    const managedReads = vi.mocked(Events.managedDate).mock.calls.length;
    await userEvent.click(screen.getByRole("button", { name: "Save this date" }));
    await waitFor(() => expect(screen.queryByRole("spinbutton", { name: "Minutes" })).not.toBeInTheDocument());
    expect(edits.sent.map((sent) => [sent.eventId, sent.date, sent.revision, sent.changes])).toEqual([[7, "2026-12-01", "r0", { durationMinutes: 150 }]]);
    expect(vi.mocked(Dates.get).mock.calls.length).toBe(pageReads + 1);
    await userEvent.click(screen.getByRole("button", { name: "Edit this date" }));
    expect(await screen.findByRole("spinbutton", { name: "Minutes" })).toHaveValue(60);
    expect(vi.mocked(Events.managedDate).mock.calls.length).toBeGreaterThan(managedReads);
  });

  it("carries only the open names list through the page's own read, never the roster, which comes back from a fresh managed read", async () => {
    const answering = (over: Partial<EventDate> = {}) => ({ occurrence: occurrence({ canManage: true, canDecline: false, canBlock: false, canRegister: true, myRegistration: null, visibility: "public", canSeeAttendees: true, attendance: { registrations: { going: 1, maybe: 0 }, attendees: [{ name: "Ann Going", status: "going", guestCount: 0 }] }, ...over }) });
    vi.mocked(Dates.get).mockResolvedValue(answering());
    mountManager();
    await openManagerTools({ minutes: false });
    await userEvent.click(screen.getByRole("button", { name: "Show names" }));
    expect(screen.getByText("Ann Going")).toBeInTheDocument();
    const managedReads = vi.mocked(Events.managedDate).mock.calls.length;
    let answer!: (read: { occurrence: EventDate }) => void;
    vi.mocked(Dates.get).mockReturnValue(new Promise((done) => { answer = done; }));
    await userEvent.click(within(screen.getByRole("region", { name: "Answer this date" })).getByRole("button", { name: "Going" }));
    await screen.findByText("Updating your answer…");
    expectNone([...OLD, "Ann Going"]);
    await act(async () => { answer(answering({ myRegistration: "going" })); });
    expect(await screen.findByText("Ann Going")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Hide names" })).toHaveAttribute("aria-expanded", "true");
    expect(await screen.findByText("Old Name")).toBeInTheDocument();
    expect(vi.mocked(Events.managedDate).mock.calls.length).toBeGreaterThan(managedReads);
  });

  it("(g) a quiet reload that stops carrying names closes them, and names coming back start closed", async () => {
    const read = (names: boolean) => ({ occurrence: occurrence({ canManage: true, canDecline: false, canBlock: false, myRegistration: null, visibility: "public", canSeeAttendees: names, attendance: { registrations: { going: 1, maybe: 0 }, attendees: names ? [{ name: "Ann Going", status: "going", guestCount: 0 }] : undefined } }) });

    const save = async () => {
      await userEvent.click(screen.getByRole("button", { name: "Save this date" }));
      await waitFor(() => expect(screen.queryByRole("spinbutton", { name: "Minutes" })).not.toBeInTheDocument());
    };

    vi.mocked(Dates.get).mockResolvedValue(read(true));
    mountManager();
    await openManagerTools({ roster: false });
    await userEvent.click(screen.getByRole("button", { name: "Show names" }));
    expect(screen.getByText("Ann Going")).toBeInTheDocument();
    vi.mocked(Dates.get).mockResolvedValue(read(false));
    await save();
    await waitFor(() => expect(screen.queryByRole("button", { name: /names/ })).not.toBeInTheDocument());
    expect(screen.queryByText("Ann Going")).not.toBeInTheDocument();

    vi.mocked(Dates.get).mockResolvedValue(read(true));
    await openManagerTools({ roster: false });
    await save();
    expect(await screen.findByRole("button", { name: "Show names" })).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByText("Ann Going")).not.toBeInTheDocument();
  });

  it("(h) a care boundary empties the roster at once and keeps the draft", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });

    try {
      const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
      vi.mocked(Events.managedDate).mockResolvedValue(managedRead({ attendees: [oldAttendee, careChild(new Date(Date.now() + 60_000).toISOString())] }));
      mountManager();
      await screen.findByRole("heading", { name: "Manage this date" });
      await user.click(screen.getByRole("button", { name: "Edit this date" }));
      const field = await screen.findByRole("spinbutton", { name: "Minutes" });
      await user.clear(field);
      await user.type(field, "150");
      await user.click(screen.getByRole("button", { name: "Manage sign-ups" }));
      await screen.findByText(/Old care note/);
      const held = later<OccurrenceResponse>();
      vi.mocked(Events.managedDate).mockReturnValue(held.promise);
      const texts = recordTexts();
      act(() => { vi.advanceTimersByTime(61_000); });
      expectNone(["Old Name", "Old care note"]);
      await act(async () => { held.resolve(newRead()); await held.promise; });
      texts.stop();
      expect(texts.sawAny(["Old Name", "Old care note"])).toBe(false);
      expect(await screen.findByRole("spinbutton", { name: "Minutes" })).toHaveValue(150);
    } finally { vi.useRealTimers(); }
  });

  it("(i) an ended or cancelled date's managed read shows the no-roster line and no roster", async () => {
    vi.mocked(Events.managedDate).mockResolvedValue({ occurrence: rosterOccurrence({ attendeesVisible: false }) });
    mountManager();
    await screen.findByRole("heading", { name: "Manage this date" });
    await userEvent.click(screen.getByRole("button", { name: "Manage sign-ups" }));
    expect(await screen.findByText("This date has ended or was cancelled, so its sign-ups can't be changed here.")).toBeInTheDocument();
    expect(screen.queryByRole("region", { name: "Manage sign-ups" })).not.toBeInTheDocument();
  });

  type Write = "remove" | "promote" | "restore" | "add";

  // Read when called: the service stub is set up for each test.
  const requestOf = (write: Write) => ({ remove: Events.removeRegistration, promote: Events.promoteRegistration, restore: Events.restoreRegistration, add: Events.addRegistration })[write];

  async function startWrite(write: Write, hold: Promise<unknown>) {
    vi.mocked(requestOf(write)).mockReturnValue(hold);

    if (write === "remove") {
      await userEvent.click(screen.getByRole("button", { name: "More for Old Name" }));
      await userEvent.click(screen.getByRole("menuitem", { name: "Remove Old Name" }));
      await userEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Remove" }));
    } else if (write === "promote") await userEvent.click(screen.getByRole("button", { name: "Promote" }));
    else if (write === "restore") await userEvent.click(screen.getByRole("button", { name: "Add back Lou Back" }));
    else {
      await userEvent.type(screen.getByLabelText("Add someone"), "known@example.com");
      await userEvent.click(screen.getByRole("button", { name: "Add" }));
    }

    await waitFor(() => expect(requestOf(write)).toHaveBeenCalledTimes(1));
  }

  // What a write's request rejects with, or undefined for a success.
  type Failure = ApiError | TypeError | undefined;

  const failures: Array<[string, () => Failure, boolean]> = [
    ["succeeds", () => undefined, false],
    ["fails 401", () => new ApiError(401, "x"), true], ["fails 403", () => new ApiError(403, "x"), true], ["fails 404", () => new ApiError(404, "x"), true],
    ["fails on the network", () => new TypeError("Failed to fetch"), true], ["fails 500", () => new ApiError(500, "x"), true],
  ];

  const writes: Write[] = ["remove", "promote", "restore", "add"];

  it.each(writes.flatMap((w) => failures.map(([name, cause, note]) => [w, name, cause, note] as const)))("(j) a %s that %s fails closed until the gate's one fresh read answers", async (write, _, cause, note) => {
    mountManager();
    await openManagerTools();
    const call = later<unknown>();
    await startWrite(write, call.promise);
    const held = later<OccurrenceResponse>();
    vi.mocked(Events.managedDate).mockReturnValue(held.promise);
    const reads = vi.mocked(Events.managedDate).mock.calls.length;
    const texts = recordTexts();
    const failure = cause();
    await act(async () => {
      if (failure === undefined) call.resolve({}); else call.reject(failure);
      await call.promise.catch(() => {});
      expectNone(OLD);
      expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    });
    expect(vi.mocked(Events.managedDate).mock.calls.length).toBe(reads + 1);
    await act(async () => { held.resolve(newRead()); await held.promise; });
    texts.stop();
    expect(texts.sawAny(OLD)).toBe(false);
    expect(await screen.findByText("New Name")).toBeInTheDocument();
    expect(screen.queryByText("Your last change may not have gone through. Here's the list as it is now.") !== null).toBe(note);
    expect(screen.getByRole("spinbutton", { name: "Minutes" })).toHaveValue(150);
    expect(vi.mocked(Events.managedDate).mock.calls.length).toBe(reads + 1);
  });

  it.each([["403", () => new ApiError(403, "x"), "gone"], ["404", () => new ApiError(404, "x"), "gone"], ["a network error", () => new TypeError("Failed to fetch"), "retry"], ["500", () => new ApiError(500, "x"), "retry"]] as const)("(j) the gate's fresh read answering %s hands the page the decision or offers a retry", async (_, cause, kind) => {
    mountManager();
    await openManagerTools();
    const call = later<unknown>();
    await startWrite("remove", call.promise);
    const held = later<OccurrenceResponse>();
    vi.mocked(Events.managedDate).mockReturnValue(held.promise);
    const pageReads = vi.mocked(Dates.get).mock.calls.length;
    await act(async () => { call.resolve({}); await call.promise; });
    await act(async () => { held.reject(cause()); await held.promise.catch(() => {}); });

    if (kind === "gone") await waitFor(() => expect(vi.mocked(Dates.get).mock.calls.length).toBe(pageReads + 1));
    else {
      expect(await screen.findByText("We couldn't load this date's details. What you typed is kept.")).toBeInTheDocument();
      expect(vi.mocked(Dates.get).mock.calls.length).toBe(pageReads);
    }

    expectNone(OLD);
  });

  it.each(["restore", "add"] as const)("(j) an answered %s (full, over capacity declined) starts no read and keeps the roster's message", async (write) => {
    mountManager();
    await openManagerTools();
    const reads = vi.mocked(Events.managedDate).mock.calls.length;
    vi.mocked(requestOf(write)).mockRejectedValue(new ApiError(409, "full", { code: "event_full" }));

    if (write === "restore") await userEvent.click(screen.getByRole("button", { name: "Add back Lou Back" }));
    else {
      await userEvent.type(screen.getByLabelText("Add someone"), "known@example.com");
      await userEvent.click(screen.getByRole("button", { name: "Add" }));
    }

    await userEvent.click(await screen.findByRole("button", { name: "Cancel" }));
    expect(await screen.findByRole("alert")).toBeInTheDocument();
    expect(screen.getByText("Old Name")).toBeInTheDocument();
    expect(vi.mocked(Events.managedDate).mock.calls.length).toBe(reads);
  });

  const settles: Array<[string, () => Failure, "fresh" | "gone" | "answered"]> = [
    ["succeeds", () => undefined, "fresh"], ["fails 403", () => new ApiError(403, "x"), "fresh"], ["fails 500", () => new ApiError(500, "x"), "fresh"], ["is full", () => new ApiError(409, "full", { code: "event_full" }), "answered"],
  ];

  it.each(writes.flatMap((w) => (["focus", "care"] as const).flatMap((recheck) => settles.map(([name, cause, then]) => [w, recheck, name, cause, then] as const))))("(k) a %s held through a %s recheck that %s keeps the roster closed until the gate allows it", async (write, recheck, _, cause, then) => {
    vi.useFakeTimers({ shouldAdvanceTime: true });

    try {
      const soon = new Date(Date.now() + 120_000).toISOString();
      vi.mocked(Events.managedDate).mockResolvedValue(managedRead({ attendees: [oldAttendee, waiter, careChild(soon)] }));
      mountManager();
      await openManagerTools();
      const call = later<unknown>();
      await startWrite(write, call.promise);
      const texts = recordTexts();

      // Every read the recheck starts answers at once, with the roster as it was before the write.
      if (recheck === "focus") act(() => { fireEvent(window, new Event("focus")); });
      else act(() => { vi.advanceTimersByTime(121_000); });
      await act(async () => { await Promise.resolve(); });
      await waitFor(() => expect(screen.queryByText("Checking that you can still manage this date…")).not.toBeInTheDocument());
      await act(async () => { await new Promise((r) => setTimeout(r, 20)); });
      expect(screen.queryByRole("region", { name: "Manage sign-ups" })).not.toBeInTheDocument();
      expect(screen.queryByRole("spinbutton", { name: "Minutes" })).not.toBeInTheDocument();

      for (const name of ["Promote", "More for Old Name", "Add back Lou Back", "Add person", "Add"]) expect(screen.queryByRole("button", { name })).not.toBeInTheDocument();
      expect(requestOf(write)).toHaveBeenCalledTimes(1);
      const reads = vi.mocked(Events.managedDate).mock.calls.length;
      const held = later<OccurrenceResponse>();
      vi.mocked(Events.managedDate).mockReturnValue(held.promise);
      const failure = cause();

      // Nothing changed after an answered write, so the recheck's own answer is the roster to show.
      if (then === "answered") {
        texts.stop();
        expect(texts.sawAny(OLD)).toBe(false);
      }

      await act(async () => {
        if (failure === undefined) call.resolve({}); else call.reject(failure);
        await call.promise.catch(() => {});
      });

      if (then === "answered") {
        expect(vi.mocked(Events.managedDate).mock.calls.length).toBe(reads);
        expect(await screen.findByRole("region", { name: "Manage sign-ups" })).toBeInTheDocument();
        expect(screen.getByText("Old Name")).toBeInTheDocument();
        expect(screen.queryByText("Your last change may not have gone through. Here's the list as it is now.")).not.toBeInTheDocument();
      } else {
        expect(vi.mocked(Events.managedDate).mock.calls.length).toBe(reads + 1);
        expect(screen.queryByRole("region", { name: "Manage sign-ups" })).not.toBeInTheDocument();
        await act(async () => { held.resolve(newRead()); await held.promise; });
        expect(await screen.findByText("New Name")).toBeInTheDocument();
        expect(screen.queryByText("Your last change may not have gone through. Here's the list as it is now.") !== null).toBe(failure !== undefined);
      }

      texts.stop();
      expect(texts.sawAny(OLD)).toBe(false);
    } finally { vi.useRealTimers(); }
  });
});

// DEC-164: the date page's "Edit this date" uses the one conflict protocol. Refused as stale over another change, the
// editor closes, the page reads again from nothing (failing closed, as any recheck) and the line above the manager
// tools takes focus.
describe("the date page when a date's save meets someone else's change", () => {
  const NOTICE = "This date changed while you were editing, so your changes could not be saved. This is the date as it is now.";
  const changed = () => new ApiError(409, "x", { code: "date_changed" });
  const managerAnswer = () => ({ occurrence: occurrence({ canManage: true, canDecline: false, canBlock: false, myRegistration: null }) });

  beforeEach(async () => {
    await verifiedCredential();
    edits = stubEditSends(51);
    invalidateSite();
    vi.spyOn(Site, "get").mockResolvedValue({ name: "Grace", timezone: "UTC", selfSignup: "off", roomsEnabled: false, pushEnabled: true });
    vi.mocked(Dates.get).mockResolvedValue(managerAnswer());
  });

  // Minutes 150, saved into date_changed; the read after it shows `stored`.
  async function saveInto(stored: Partial<EventOccurrence>, answer: Error = changed()) {
    vi.mocked(Events.managedDate).mockResolvedValueOnce(managedRead()).mockResolvedValue(managedRead({ revision: "r1", ...stored }));
    edits.answer(answer);
    mountManager();
    await openManagerTools({ roster: false });
    await userEvent.click(screen.getByRole("button", { name: "Save this date" }));
  }

  it("sends the save again under the new revision when nothing it can write changed, and closes as a save does", async () => {
    await saveInto({});
    await waitFor(() => expect(screen.queryByRole("spinbutton", { name: "Minutes" })).not.toBeInTheDocument());
    expect(edits.sent.map((s) => [s.revision, s.changes])).toEqual([["r0", { durationMinutes: 150 }], ["r1", { durationMinutes: 150 }]]);
    expect(screen.queryByText(/could not be saved/)).not.toBeInTheDocument();
  });

  it("closes as a save does, with no line, when the read shows the save already stored", async () => {
    await saveInto({ durationMinutes: 150, own: { durationMinutes: 150 } });
    await waitFor(() => expect(screen.queryByRole("spinbutton", { name: "Minutes" })).not.toBeInTheDocument());
    expect(edits.sent).toHaveLength(1);
    expect(screen.queryByText(/could not be saved/)).not.toBeInTheDocument();
  });

  it("closes the editor over another change, reads the page from nothing and says so above the tools, focused; opening the editor clears it", async () => {
    const after = later<{ occurrence: EventDate }>();
    vi.mocked(Dates.get).mockResolvedValueOnce(managerAnswer()).mockReturnValueOnce(after.promise);
    await saveInto({ location: "Annex", own: { location: "Annex" } });
    expect(await screen.findByText("Loading this date…")).toBeInTheDocument();
    expect(screen.queryByRole("spinbutton", { name: "Minutes" })).not.toBeInTheDocument();
    expect(screen.queryByText(NOTICE)).not.toBeInTheDocument();
    await act(async () => { after.resolve(managerAnswer()); await after.promise; });
    const notice = await screen.findByText(NOTICE);
    await waitFor(() => expect(notice).toHaveFocus());
    expect(screen.getByRole("region", { name: "Manage this date" }).contains(notice)).toBe(false);
    expect(edits.sent).toHaveLength(1);
    await userEvent.click(screen.getByRole("button", { name: "Edit this date" }));
    expect(screen.queryByText(NOTICE)).not.toBeInTheDocument();
    expect(await screen.findByRole("textbox", { name: /^Location/ })).toHaveValue("Annex");
  });

  it("says only that the changes weren't saved, on the cancelled date's page, when the date was cancelled meanwhile", async () => {
    vi.mocked(Dates.get).mockResolvedValueOnce(managerAnswer()).mockResolvedValue({ occurrence: { cancelled: true, date: "2026-12-01", title: "One invited date" } });
    await saveInto({ cancelled: true });
    expect(await screen.findByText("This date was cancelled. Your calendar shows the dates that are still on.")).toBeInTheDocument();
    const line = await screen.findByText("Your changes could not be saved.");
    await waitFor(() => expect(line).toHaveFocus());
    expect(edits.sent).toHaveLength(1);
  });

  it("says the person can no longer manage the date when a send is refused 403 and the page agrees", async () => {
    vi.mocked(Dates.get).mockResolvedValueOnce(managerAnswer()).mockResolvedValue({ occurrence: occurrence({ canManage: false, canDecline: false, canBlock: false, myRegistration: null }) });
    await saveInto({}, new ApiError(403, "SECRET", { code: "forbidden" }));
    const line = await screen.findByText("You can no longer manage this date, so your changes could not be saved.");
    await waitFor(() => expect(line).toHaveFocus());
    expect(screen.queryByRole("heading", { name: "Manage this date" })).not.toBeInTheDocument();
    expect(document.body.textContent).not.toContain("SECRET");
  });
});

describe("the answer bar", () => {
  const usual: EventDateUsual = { startsLocal: "2026-12-01T18:30", endsLocal: "2026-12-01T19:30", durationMinutes: 60, location: "", room: null, capacity: 12, remoteCapacity: 5, visibility: "public" };
  const seriesOf = (over: Partial<EventDateSeries> = {}): EventDateSeries => ({ recurrenceRule: "FREQ=WEEKLY;COUNT=6", upcomingDates: 4, coveredDates: 1, canAnswerSeries: true, usual, changes: ["time"], ...over });

  // An ordinary member's read of a date moved a day later (shown Dec 2 UTC); its address keeps the original Dec 1.
  const member = (over: Partial<EventDate> = {}): EventDate => occurrence({
    title: "Romans 8", visibility: "public", groupId: 88, groupName: "Choir", canRegister: true, canDecline: false, canBlock: false,
    startsAt: "2026-12-02T19:00:00Z", endsAt: "2026-12-02T20:00:00Z", startsLocal: "2026-12-02T19:00", endsLocal: "2026-12-02T20:00",
    myRegistration: "going", myAttendanceMode: "in_person", myGuestCount: 1, canSeeAttendees: true,
    groupHeader: { imageUrl: "", coverFocalX: 50, coverFocalY: 50, leaderName: "Lee Leader" },
    attendance: { registrations: { going: 2, maybe: 0 }, attendees: [{ name: "PRIVATE NAME", status: "going", guestCount: 0 }] },
    seats: { inPerson: { capacity: 12, used: 3, left: 9 }, remote: { capacity: 5, used: 0, left: 5 }, waitlistEnabled: true },
    remoteRsvp: "offered", series: seriesOf(), ...over,
  });

  const batch = (over: Partial<SeriesRegistrationResponse> = {}): SeriesRegistrationResponse => ({
    eventId: 7, scope: "all_future", status: "going",
    results: [{ occurrenceId: 51, date: "2026-12-01", before: {}, after: { status: "going" } }, { occurrenceId: 52, date: "2026-12-08", before: {}, after: { status: "going" } }],
    refusals: [], undoToken: "receipt-token", undoExpiresAt: new Date(Date.now() + 60_000).toISOString(), ...over,
  });

  const bar = () => screen.getByRole("region", { name: "Answer this date" });
  const inBar = (name: string) => within(bar()).getByRole("button", { name });
  const scopeDialog = () => screen.getByRole("dialog", { name: /How far should/ });

  async function show(read: EventDate) {
    vi.mocked(Dates.get).mockResolvedValue({ occurrence: read });
    mount();
    await screen.findByRole("heading", { level: 1, name: read.title });
  }

  function deferred<T>() {
    let resolve!: (value: T) => void;
    let reject!: (cause: unknown) => void;
    const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej; });

    return { promise, resolve, reject };
  }

  it("is the one answer group and one status, with the details in the page's flow", async () => {
    await show(member());
    expect(screen.getAllByRole("group", { name: "Your answer" })).toHaveLength(1);
    expect(within(bar()).getByRole("group", { name: "Your answer" })).toBeInTheDocument();
    expect(screen.getAllByRole("button", { name: "Going" })).toHaveLength(1);
    expect(screen.getAllByRole("status")).toHaveLength(1);
    // Mode and guests are in the page, not the bar, which keeps compact actions only.
    expect(within(bar()).queryByLabelText("Guests")).not.toBeInTheDocument();
    expect(screen.getByLabelText("Guests")).toHaveValue(1);
    expect(inBar("Going")).toHaveAttribute("aria-pressed", "true");
  });

  it("shows no bar to a read-only reader, an unavailable date or a tombstone", async () => {
    await show(member({ canRegister: false }));
    expect(screen.queryByRole("region", { name: "Answer this date" })).not.toBeInTheDocument();
    cleanup();
    vi.mocked(Dates.get).mockResolvedValue({ occurrence: { title: "Romans 8", date: "2026-12-01", cancelled: true } });
    mount(); await screen.findByText("This date was cancelled. Your calendar shows the dates that are still on.");
    expect(screen.queryByRole("region", { name: "Answer this date" })).not.toBeInTheDocument();
    cleanup();
    vi.mocked(Dates.get).mockRejectedValue(new ApiError(404, "Not found"));
    mount(); await screen.findByText("This date is not available");
    expect(screen.queryByRole("region", { name: "Answer this date" })).not.toBeInTheDocument();
  });

  it("answers just this date through the ordinary writer with the original date and only known fields", async () => {
    await show(member({ series: seriesOf({ canAnswerSeries: false }) }));
    vi.mocked(Events.register).mockResolvedValue({ registration, people: [] });
    await userEvent.click(inBar("Maybe"));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(Events.register).toHaveBeenCalledOnce();
    const [eventId, body] = vi.mocked(Events.register).mock.calls[0];
    expect(eventId).toBe(7);
    expect(body).toStrictEqual({ date: "2026-12-01", status: "maybe", attendanceMode: "in_person", guestCount: 1 });
    expect(Object.keys(body)).not.toContain("childcareCount");
    await waitFor(() => expect(Dates.get).toHaveBeenCalledTimes(2));
  });

  it.each([
    ["This and following dates", "this_and_following", { scope: "this_and_following", status: "going", date: "2026-12-01", attendanceMode: "in_person", guestCount: 1 }],
    ["All future dates", "all_future", { scope: "all_future", status: "going", attendanceMode: "in_person", guestCount: 1 }],
  ] as const)("sends %s for a moved date by its original date, without childcare", async (label, _scope, expected) => {
    await show(member());
    vi.mocked(Events.registerSeries).mockResolvedValue(batch());
    await userEvent.click(inBar("Going"));
    await userEvent.click(within(scopeDialog()).getByRole("button", { name: label }));
    await waitFor(() => expect(Events.registerSeries).toHaveBeenCalledOnce());
    expect(vi.mocked(Events.registerSeries).mock.calls[0]).toStrictEqual([7, expected]);
    expect(Events.register).not.toHaveBeenCalled(); expect(Dates.register).not.toHaveBeenCalled();
  });

  it("Just this date in the picker uses the single-date writer", async () => {
    await show(member());
    vi.mocked(Events.register).mockResolvedValue({ registration, people: [] });
    await userEvent.click(inBar("Going"));
    await userEvent.click(within(scopeDialog()).getByRole("button", { name: "Just this date" }));
    await waitFor(() => expect(Events.register).toHaveBeenCalledWith(7, { date: "2026-12-01", status: "going", attendanceMode: "in_person", guestCount: 1 }));
    expect(Events.registerSeries).not.toHaveBeenCalled();
  });

  it("Escape and Cancel write nothing and give focus back to the answer", async () => {
    await show(member());
    const going = inBar("Going");
    await userEvent.click(going);
    expect(scopeDialog()).toBeInTheDocument();
    await userEvent.keyboard("{Escape}");
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument(); expect(going).toHaveFocus();
    await userEvent.click(going);
    await userEvent.click(within(scopeDialog()).getByRole("button", { name: "Cancel" }));
    expect(going).toHaveFocus();
    // A series Can't go asks once more, and Escape there writes nothing either.
    await userEvent.click(inBar("Can't go"));
    await userEvent.click(within(scopeDialog()).getByRole("button", { name: "All future dates" }));
    const ask = await screen.findByRole("dialog", { name: "Can't go to these dates?" });
    expect(within(ask).getByRole("button", { name: "Can't go to these dates" })).toHaveFocus();
    await userEvent.keyboard("{Escape}");
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(Events.registerSeries).not.toHaveBeenCalled(); expect(Events.register).not.toHaveBeenCalled();
  });

  it("a confirmed series Can't go sends one write with no guests or mode, and its receipt offers Undo", async () => {
    await show(member());
    vi.mocked(Events.registerSeries).mockResolvedValue(batch({ status: "declined", results: [{ occurrenceId: 51, date: "2026-12-01", before: { status: "going" }, after: { status: "declined" } }] }));
    vi.mocked(Dates.get).mockResolvedValue({ occurrence: member({ myRegistration: "declined", myAttendanceMode: undefined, myGuestCount: undefined }) });
    await userEvent.click(inBar("Can't go"));
    await userEvent.click(within(scopeDialog()).getByRole("button", { name: "This and following dates" }));
    await userEvent.click(within(await screen.findByRole("dialog", { name: "Can't go to these dates?" })).getByRole("button", { name: "Can't go to these dates" }));
    await waitFor(() => expect(Events.registerSeries).toHaveBeenCalledOnce());
    expect(vi.mocked(Events.registerSeries).mock.calls[0][1]).toStrictEqual({ scope: "this_and_following", status: "declined", date: "2026-12-01" });
    expect(await within(bar()).findByRole("button", { name: "Undo" })).toBeInTheDocument();
    expect(await screen.findByText("Can't go to 1 date")).toBeInTheDocument();
  });

  it("a repeated click, Enter or another answer while a write is pending sends nothing more", async () => {
    await show(member());
    const write = deferred<SeriesRegistrationResponse>();
    vi.mocked(Events.registerSeries).mockReturnValue(write.promise);
    await userEvent.click(inBar("Going"));
    const following = within(scopeDialog()).getByRole("button", { name: "This and following dates" });
    act(() => { following.click(); following.click(); });
    const maybe = inBar("Maybe");
    expect(maybe).toHaveAttribute("aria-disabled", "true");
    maybe.focus(); await userEvent.keyboard("{Enter}"); await userEvent.click(inBar("Going"));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(Events.registerSeries).toHaveBeenCalledOnce(); expect(Events.register).not.toHaveBeenCalled();
    await act(async () => { write.resolve(batch()); await write.promise; });
    await waitFor(() => expect(inBar("Maybe")).not.toHaveAttribute("aria-disabled"));
  });

  it("a refresh closes an open picker, and its stale buttons send nothing", async () => {
    await show(member());
    await userEvent.click(inBar("Going"));
    const stale = within(scopeDialog()).getByRole("button", { name: "All future dates" });
    fireEvent(window, new Event("focus"));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    act(() => { stale.click(); });
    await screen.findByRole("heading", { level: 1, name: "Romans 8" });
    expect(Events.registerSeries).not.toHaveBeenCalled(); expect(Events.register).not.toHaveBeenCalled();
  });

  // The reader keeps their place through the page's own read after a save, with Undo as reachable as the
  // answer (M8.1 brief 5.2): an open names list stays open across that read, shows nothing until the fresh
  // read lands, and any other read closes it.
  describe("an open names list", () => {
    type Held = ReturnType<typeof deferred<EventDateRead>>;

    const toggle = () => screen.getByRole("button", { name: /^(Show|Hide) names$/ });
    const noNames = () => member({ canSeeAttendees: false, attendance: { registrations: { going: 2, maybe: 0 } } });

    async function openNames(read: EventDate = member()) {
      await show(read);
      await userEvent.click(screen.getByRole("button", { name: "Show names" }));
      expect(screen.getByText("PRIVATE NAME")).toBeInTheDocument();
    }

    // Answers Going for every future date and holds the page's own read until the test answers it.
    async function applyHeld() {
      const reread = deferred<EventDateRead>();
      vi.mocked(Events.registerSeries).mockResolvedValue(batch());
      vi.mocked(Dates.get).mockReturnValue(reread.promise);
      await userEvent.click(inBar("Going"));
      await userEvent.click(within(scopeDialog()).getByRole("button", { name: "All future dates" }));
      await screen.findByText("Updating your answer…");

      return reread;
    }

    function expectNothingProtected() {
      expect(screen.queryByText("PRIVATE NAME")).not.toBeInTheDocument();
      expect(screen.queryByRole("button", { name: /names$/ })).not.toBeInTheDocument();
      expect(screen.queryByRole("region", { name: "Going names" })).not.toBeInTheDocument();
    }

    it("stays open through a series answer's own read and its Undo's, with focus kept in the bar", async () => {
      await openNames();
      const applied = await applyHeld();
      expectNothingProtected();
      const focused = document.activeElement;
      expect(bar().contains(focused)).toBe(true);
      await act(async () => { applied.resolve({ occurrence: member({ series: seriesOf({ coveredDates: 4 }) }) }); await applied.promise; });
      expect(await screen.findByText("PRIVATE NAME")).toBeInTheDocument();
      expect(toggle()).toHaveAttribute("aria-expanded", "true");
      expect(toggle()).toHaveTextContent("Hide names");
      expect(document.activeElement).toBe(focused);

      const undone = deferred<{ occurrence: EventDate }>();
      vi.mocked(Dates.get).mockReturnValue(undone.promise);
      vi.mocked(Events.undoSeriesRegistration).mockResolvedValue(batch({ undoToken: undefined, undoExpiresAt: undefined }));
      await userEvent.click(within(bar()).getByRole("button", { name: "Undo" }));
      await screen.findByText("Updating your answer…");
      expectNothingProtected();
      await act(async () => { undone.resolve({ occurrence: member() }); await undone.promise; });
      expect(await screen.findByText("PRIVATE NAME")).toBeInTheDocument();
      expect(toggle()).toHaveAttribute("aria-expanded", "true");
      // Undo went away with focus on it: focus is on the answer, still in the bar.
      expect(within(bar()).getByRole("group", { name: "Your answer" }).contains(document.activeElement)).toBe(true);
    });

    it("stays open through a single-date answer's own read", async () => {
      await openNames(member({ series: undefined }));
      const reread = deferred<{ occurrence: EventDate }>();
      vi.mocked(Dates.get).mockReturnValue(reread.promise);
      await userEvent.click(inBar("Maybe"));
      await screen.findByText("Updating your answer…");
      expectNothingProtected();
      await act(async () => { reread.resolve({ occurrence: member({ series: undefined, myRegistration: "maybe" }) }); await reread.promise; });
      expect(await screen.findByText("PRIVATE NAME")).toBeInTheDocument();
      expect(toggle()).toHaveAttribute("aria-expanded", "true");
    });

    it.each([
      ["a focus recheck during the page's own read", async (reread: Held) => {
        vi.mocked(Dates.get).mockResolvedValue({ occurrence: member() });
        fireEvent(window, new Event("focus"));
        await act(async () => { reread.resolve({ occurrence: member() }); await reread.promise; });
      }],
      ["a failed own read, then a recheck", async (reread: Held) => {
        await act(async () => { reread.reject(new ApiError(500, "SECRET")); await reread.promise.catch(() => {}); });
        await screen.findByText("This date is not available");
        vi.mocked(Dates.get).mockResolvedValue({ occurrence: member() });
        fireEvent(window, new Event("focus"));
      }],
      ["a tombstone, then the date again", async (reread: Held) => {
        await act(async () => { reread.resolve({ occurrence: { title: "Romans 8", date: "2026-12-01", cancelled: true } }); await reread.promise; });
        await screen.findByText(/This date was cancelled/);
        vi.mocked(Dates.get).mockResolvedValue({ occurrence: member() });
        fireEvent(window, new Event("focus"));
      }],
      ["an own read that carries no names, then one that does", async (reread: Held) => {
        await act(async () => { reread.resolve({ occurrence: noNames() }); await reread.promise; });
        await waitFor(() => expect(screen.queryByText("Updating your answer…")).not.toBeInTheDocument());
        expectNothingProtected();
        vi.mocked(Dates.get).mockResolvedValue({ occurrence: member() });
        await userEvent.click(inBar("Maybe"));
        await userEvent.click(within(scopeDialog()).getByRole("button", { name: "Just this date" }));
      }],
      // A grant-only read may carry attendees, but never shows them: it ends the open list as surely as no names.
      ["a narrower read without the ordinary view that still carries attendees, then the ordinary read", async (reread: Held) => {
        await act(async () => { reread.resolve({ occurrence: member({ visibility: undefined, series: undefined, canDecline: true }) }); await reread.promise; });
        await waitFor(() => expect(screen.queryByText("Updating your answer…")).not.toBeInTheDocument());
        expectNothingProtected();
        vi.mocked(Dates.get).mockResolvedValue({ occurrence: member() });
        await userEvent.click(inBar("Maybe"));
      }],
    ])("closes after %s", async (_name, then) => {
      await openNames();
      const reread = await applyHeld();
      await then(reread);
      expect(await screen.findByRole("button", { name: "Show names" })).toHaveAttribute("aria-expanded", "false");
      expect(screen.queryByText("PRIVATE NAME")).not.toBeInTheDocument();
    });

    it("closes when a write's read moved on before the write answered, so the page is checked again", async () => {
      await openNames();
      const write = deferred<SeriesRegistrationResponse>();
      vi.mocked(Events.registerSeries).mockReturnValue(write.promise);
      await userEvent.click(inBar("Going"));
      await userEvent.click(within(scopeDialog()).getByRole("button", { name: "All future dates" }));
      // A read this page did not start lands while the write is out.
      fireEvent(window, new Event("focus"));
      await screen.findByRole("button", { name: "Show names" });
      await act(async () => { write.resolve(batch()); await write.promise; });
      expect(await screen.findByRole("button", { name: "Show names" })).toHaveAttribute("aria-expanded", "false");
      expect(screen.queryByText("PRIVATE NAME")).not.toBeInTheDocument();
    });

    it("closes when an Undo refusal sends the page to check authority again", async () => {
      await openNames();
      const applied = await applyHeld();
      await act(async () => { applied.resolve({ occurrence: member() }); await applied.promise; });
      expect(toggle()).toHaveAttribute("aria-expanded", "true");
      vi.mocked(Dates.get).mockResolvedValue({ occurrence: member() });
      vi.mocked(Events.undoSeriesRegistration).mockRejectedValue(new ApiError(409, "SECRET", { code: "undo_authority_revoked" }));
      await userEvent.click(within(bar()).getByRole("button", { name: "Undo" }));
      expect(await screen.findByRole("alert")).toBeInTheDocument();
      expect(toggle()).toHaveAttribute("aria-expanded", "false");
      expect(screen.queryByText("PRIVATE NAME")).not.toBeInTheDocument();
    });
  });

  it("keeps the status and a generic Undo through its own read, and shows nothing protected until it lands", async () => {
    await show(member({ canSeeVideo: true, videoUrl: "https://example.com/private-call" }));
    const reread = deferred<{ occurrence: EventDate }>();
    vi.mocked(Events.registerSeries).mockResolvedValue(batch());
    vi.mocked(Dates.get).mockReturnValue(reread.promise);
    const status = screen.getByRole("status");
    await userEvent.click(screen.getByRole("button", { name: "Show names" }));
    expect(screen.getByText("PRIVATE NAME")).toBeInTheDocument();
    await userEvent.click(inBar("Going"));
    await userEvent.click(within(scopeDialog()).getByRole("button", { name: "All future dates" }));
    // During the page's own read: the same status element, a generic Undo, no old answer, names or link.
    expect(await screen.findByText("Updating your answer…")).toBeInTheDocument();
    expect(screen.getByRole("status")).toBe(status);
    expect(screen.getAllByRole("status")).toHaveLength(1);
    expect(within(bar()).getByRole("button", { name: "Undo" })).toBeInTheDocument();

    for (const name of ["Going", "Maybe", "Can't go"]) expect(inBar(name)).toHaveAttribute("aria-pressed", "false");
    expect(inBar("Going")).toHaveAttribute("aria-disabled", "true");
    expect(screen.queryByText("PRIVATE NAME")).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: /Join video call/ })).not.toBeInTheDocument();
    expect(screen.queryByText("Going to 2 dates")).not.toBeInTheDocument();
    // The fresh read decides standing; the request's count never says "in for the series".
    await act(async () => { reread.resolve({ occurrence: member({ series: seriesOf({ coveredDates: 2 }) }) }); await reread.promise; });
    expect(await screen.findByText("Going to 2 dates")).toBeInTheDocument();
    expect(screen.getByRole("status")).toBe(status);
    expect(status).toHaveTextContent("You're in for this date only.");
    expect(status).not.toHaveTextContent("in for the series");
    expect(within(bar()).getByRole("button", { name: "Undo" })).toBeInTheDocument();
  });

  it.each([
    ["no token", { undoToken: undefined }],
    ["an expiry already past", { undoExpiresAt: new Date(Date.now() - 1000).toISOString() }],
    ["a malformed expiry", { undoExpiresAt: "not a time" }],
    ["no expiry", { undoExpiresAt: undefined }],
  ] as const)("offers no Undo for a reply with %s", async (_label, over) => {
    await show(member());
    vi.mocked(Events.registerSeries).mockResolvedValue(batch(over));
    await userEvent.click(inBar("Going"));
    await userEvent.click(within(scopeDialog()).getByRole("button", { name: "All future dates" }));
    expect(await screen.findByText("Going to 2 dates")).toBeInTheDocument();
    expect(within(bar()).queryByRole("button", { name: "Undo" })).not.toBeInTheDocument();
  });

  it("offers Undo after a partial batch with refusals and waitlisted dates, and never from coverage alone", async () => {
    await show(member({ series: seriesOf({ coveredDates: 4 }) }));
    expect(within(bar()).queryByRole("button", { name: "Undo" })).not.toBeInTheDocument();
    vi.mocked(Events.registerSeries).mockResolvedValue(batch({
      results: [{ occurrenceId: 51, date: "2026-12-01", before: {}, after: { status: "going" } }, { occurrenceId: 53, date: "2026-12-15", before: {}, after: { status: "waitlisted", waitlistPosition: 2 } }],
      refusals: [{ occurrenceId: 52, date: "2026-12-08", code: "event_full" }],
    }));
    await userEvent.click(inBar("Going"));
    await userEvent.click(within(scopeDialog()).getByRole("button", { name: "This and following dates" }));
    expect(await screen.findByText(/^Going to 1 date · waitlisted for .+ · 1 date was left unchanged$/)).toBeInTheDocument();
    expect(within(bar()).getByRole("button", { name: "Undo" })).toBeInTheDocument();
  });

  it("Undo sends the receipt once, reads the date again and keeps the person's place on the answer", async () => {
    await show(member());
    vi.mocked(Events.registerSeries).mockResolvedValue(batch());
    const undo = deferred<SeriesRegistrationResponse>();
    vi.mocked(Events.undoSeriesRegistration).mockReturnValue(undo.promise);
    await userEvent.click(inBar("Going"));
    await userEvent.click(within(scopeDialog()).getByRole("button", { name: "All future dates" }));
    const button = await within(bar()).findByRole("button", { name: "Undo" });
    await screen.findByText("Going to 2 dates");
    const reads = vi.mocked(Dates.get).mock.calls.length;
    button.focus();
    act(() => { button.click(); button.click(); });
    await userEvent.keyboard("{Enter}");
    expect(Events.undoSeriesRegistration).toHaveBeenCalledOnce();
    expect(Events.undoSeriesRegistration).toHaveBeenCalledWith(7, "receipt-token");
    await act(async () => { undo.resolve(batch({ undoToken: undefined, results: [{ occurrenceId: 51, date: "2026-12-01", before: { status: "going" }, after: {} }], capacityLoss: ["2026-12-08"] })); await undo.promise; });
    expect(await screen.findByText(/^Undo: a seat was no longer available for .+$/)).toBeInTheDocument();
    expect(within(bar()).queryByRole("button", { name: "Undo" })).not.toBeInTheDocument();
    expect(vi.mocked(Dates.get).mock.calls.length).toBe(reads + 1);
    expect(inBar("Going")).toHaveFocus();
  });

  it("an Undo past its deadline sends nothing, and the timer retires it", async () => {
    const undoWindow = 60_000;
    const start = new Date("2026-11-26T19:00:00Z");
    const deadline = new Date(start.getTime() + undoWindow);
    vi.useFakeTimers();

    try {
      async function answerSeries() {
        vi.setSystemTime(start);
        vi.mocked(Dates.get).mockResolvedValue({ occurrence: member() });
        vi.mocked(Events.registerSeries).mockResolvedValue(batch({ undoExpiresAt: deadline.toISOString() }));
        await act(async () => { mount(); });
        fireEvent.click(inBar("Going"));
        await act(async () => { fireEvent.click(within(scopeDialog()).getByRole("button", { name: "All future dates" })); });

        return inBar("Undo");
      }

      const button = await answerSeries();
      // Move the clock without running the timer so the click reaches the deadline check.
      vi.setSystemTime(deadline);
      await act(async () => { button.click(); });
      expect(Events.undoSeriesRegistration).not.toHaveBeenCalled();
      expect(screen.getByRole("status")).toHaveTextContent("The Undo time has ended.");
      expect(within(bar()).queryByRole("button", { name: "Undo" })).not.toBeInTheDocument();
      cleanup();

      await answerSeries();
      act(() => { vi.advanceTimersByTime(undoWindow - 1); });
      expect(inBar("Undo")).toBeVisible();
      act(() => { vi.advanceTimersByTime(1); });
      expect(within(bar()).queryByRole("button", { name: "Undo" })).not.toBeInTheDocument();
      expect(screen.getByRole("status")).toHaveTextContent("The Undo time has ended.");
      expect(Events.undoSeriesRegistration).not.toHaveBeenCalled();
    } finally {
      vi.useRealTimers();
    }
  });

  it("a timer that fires before the deadline waits for the rest of it, then retires Undo", async () => {
    await show(member());
    const deadline = Date.now() + 60_000;
    vi.mocked(Events.registerSeries).mockResolvedValue(batch({ undoExpiresAt: new Date(deadline).toISOString() }));
    // The wall clock stands 1ms short of the deadline whenever the timer fires, as when a timer fires early.
    const clock = vi.spyOn(Date, "now").mockReturnValue(deadline - 1);

    try {
      await userEvent.click(inBar("Going"));
      await userEvent.click(within(scopeDialog()).getByRole("button", { name: "All future dates" }));
      await within(bar()).findByRole("button", { name: "Undo" });
      // The page's 1ms timer is due before this wait ends, so it has fired by now.
      await act(async () => { await new Promise((r) => setTimeout(r, 50)); });
      expect(within(bar()).getByRole("button", { name: "Undo" })).toBeInTheDocument();
      expect(screen.queryByText(/The Undo time has ended\.$/)).not.toBeInTheDocument();
      clock.mockReturnValue(deadline);
      await waitFor(() => expect(within(bar()).queryByRole("button", { name: "Undo" })).not.toBeInTheDocument());
      expect(screen.getByRole("status")).toHaveTextContent("The Undo time has ended.");
    } finally { clock.mockRestore(); }
  });

  it("a transient Undo failure keeps the receipt and says so safely", async () => {
    await show(member());
    vi.mocked(Events.registerSeries).mockResolvedValue(batch());
    vi.mocked(Events.undoSeriesRegistration).mockRejectedValueOnce(new ApiError(500, "SECRET"));
    await userEvent.click(inBar("Going"));
    await userEvent.click(within(scopeDialog()).getByRole("button", { name: "All future dates" }));
    await userEvent.click(await within(bar()).findByRole("button", { name: "Undo" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("We couldn't undo that answer.");
    expect(screen.queryByText(/SECRET/)).not.toBeInTheDocument();
    expect(within(bar()).getByRole("button", { name: "Undo" })).toBeInTheDocument();
  });

  it.each(["undo_authority_revoked", "undo_used", "undo_changed", "undo_date_ended", "undo_not_found"] as const)("a terminal %s retires Undo and reads the date again, fail closed", async (code) => {
    await show(member());
    vi.mocked(Events.registerSeries).mockResolvedValue(batch());
    await userEvent.click(inBar("Going"));
    await userEvent.click(within(scopeDialog()).getByRole("button", { name: "All future dates" }));
    await screen.findByText("Going to 2 dates");
    const reread = deferred<{ occurrence: EventDate }>();
    vi.mocked(Dates.get).mockReturnValue(reread.promise);
    vi.mocked(Events.undoSeriesRegistration).mockRejectedValue(new ApiError(409, "SECRET", { code }));
    await userEvent.click(within(bar()).getByRole("button", { name: "Undo" }));
    // While the authority recheck is out: nothing protected, no Undo, no bar.
    await screen.findByText("Loading this date…");
    expect(screen.queryByText("PRIVATE NAME")).not.toBeInTheDocument();
    expect(screen.queryByRole("region", { name: "Answer this date" })).not.toBeInTheDocument();
    // A narrower read without the series: the date stays, its series facts and the receipt do not.
    await act(async () => { reread.resolve({ occurrence: member({ series: undefined, attendance: undefined }) }); await reread.promise; });
    await screen.findByRole("heading", { level: 1, name: "Romans 8" });
    expect(within(bar()).queryByRole("button", { name: "Undo" })).not.toBeInTheDocument();
    expect(screen.queryByText("Going to 2 dates")).not.toBeInTheDocument();
    expect(await screen.findByRole("alert")).not.toHaveTextContent("SECRET");
    expect(Events.undoSeriesRegistration).toHaveBeenCalledOnce();
  });

  describe("when a terminal Undo refusal takes Undo away while it has focus", () => {
    async function refuseUndo() {
      await show(member());
      vi.mocked(Events.registerSeries).mockResolvedValue(batch());
      await userEvent.click(inBar("Going"));
      await userEvent.click(within(scopeDialog()).getByRole("button", { name: "All future dates" }));
      await screen.findByText("Going to 2 dates");
      const reread = deferred<EventDateRead>();
      vi.mocked(Dates.get).mockReturnValue(reread.promise);
      vi.mocked(Events.undoSeriesRegistration).mockRejectedValue(new ApiError(409, "SECRET", { code: "undo_authority_revoked" }));
      const undoButton = within(bar()).getByRole("button", { name: "Undo" });
      undoButton.focus();
      await userEvent.keyboard("{Enter}");
      await screen.findByText("Loading this date…");
      expect(undoButton).not.toBeInTheDocument();

      return reread;
    }

    it.each([
      ["a narrower scheduled read with answers", () => member({ series: undefined, attendance: undefined }), () => inBar("Going")],
      ["a narrower scheduled read with no answer to give", () => member({ series: undefined, attendance: undefined, canRegister: false }), () => screen.getByRole("heading", { level: 1, name: "Romans 8" })],
      // Going is disabled for a waiter on a full pool with no waitlist, so the first answer that can take focus does.
      ["a narrower read where Going can't be chosen", () => member({ series: undefined, attendance: undefined, myRegistration: "waitlisted", seats: { inPerson: { capacity: 4, used: 4, left: 0 }, remote: { capacity: 5, used: 0, left: 5 }, waitlistEnabled: false } }), () => inBar("Maybe")],
    ])("focus waits for %s and lands on what remains, without scrolling", async (_name, narrower, target) => {
      const reread = await refuseUndo();
      const focus = vi.spyOn(HTMLElement.prototype, "focus");
      await act(async () => { reread.resolve({ occurrence: narrower() }); await reread.promise; });
      await screen.findByRole("heading", { level: 1, name: "Romans 8" });
      expect(target()).toHaveFocus();

      if (_name === "a narrower scheduled read with no answer to give") expect(screen.getByRole("status")).not.toHaveFocus();
      expect(focus).toHaveBeenLastCalledWith({ preventScroll: true });
      focus.mockRestore();
    });

    it("focus lands on the page heading when the date is no longer available", async () => {
      const reread = await refuseUndo();
      await act(async () => { reread.reject(new ApiError(404, "SECRET")); await reread.promise.catch(() => {}); });
      await screen.findByText("This date is not available");
      expect(screen.getByRole("heading", { level: 1, name: "Event date" })).toHaveFocus();
      expect(screen.queryByRole("region", { name: "Answer this date" })).not.toBeInTheDocument();
      expect(document.body).not.toHaveTextContent("SECRET");
    });

    it("focus lands on the heading when the date was cancelled meanwhile", async () => {
      const reread = await refuseUndo();
      await act(async () => { reread.resolve({ occurrence: { title: "Romans 8", date: "2026-12-01", cancelled: true } }); await reread.promise; });
      await screen.findByText(/This date was cancelled\./);
      expect(screen.getByRole("heading", { level: 1, name: "Romans 8" })).toHaveFocus();
    });

    it("focus the person moved meanwhile stays where they put it", async () => {
      const reread = await refuseUndo();
      const back = screen.getByRole("link", { name: "Back to calendar" });
      back.focus();
      await act(async () => { reread.resolve({ occurrence: member({ series: undefined, attendance: undefined }) }); await reread.promise; });
      await screen.findByRole("heading", { level: 1, name: "Romans 8" });
      expect(back).toHaveFocus();
    });
  });

  it("a series refusal rereads the date fail closed and keeps what a narrower read still shows", async () => {
    await show(member());
    vi.mocked(Events.registerSeries).mockRejectedValue(new ApiError(403, "SECRET"));
    vi.mocked(Dates.get).mockResolvedValue({ occurrence: member({ series: undefined }) });
    await userEvent.click(inBar("Going"));
    await userEvent.click(within(scopeDialog()).getByRole("button", { name: "All future dates" }));
    await waitFor(() => expect(Dates.get).toHaveBeenCalledTimes(2));
    await screen.findByRole("heading", { level: 1, name: "Romans 8" });
    expect(screen.queryByText(/SECRET/)).not.toBeInTheDocument();
    // Without series authority the bar answers this date alone.
    vi.mocked(Events.register).mockResolvedValue({ registration, people: [] });
    await userEvent.click(inBar("Maybe"));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(Events.register).toHaveBeenCalledOnce();
  });

  it("a focus recheck hides Undo and the result, and the same authorized read brings the receipt back", async () => {
    await show(member());
    vi.mocked(Events.registerSeries).mockResolvedValue(batch());
    await userEvent.click(inBar("Going"));
    await userEvent.click(within(scopeDialog()).getByRole("button", { name: "All future dates" }));
    await screen.findByText("Going to 2 dates");
    const reread = deferred<{ occurrence: EventDate }>();
    vi.mocked(Dates.get).mockReturnValue(reread.promise);
    fireEvent(window, new Event("focus"));
    expect(screen.queryByRole("region", { name: "Answer this date" })).not.toBeInTheDocument();
    expect(screen.queryByText("Going to 2 dates")).not.toBeInTheDocument();
    await act(async () => { reread.resolve({ occurrence: member() }); await reread.promise; });
    expect(await within(bar()).findByRole("button", { name: "Undo" })).toBeInTheDocument();
    expect(screen.queryByText("Going to 2 dates")).not.toBeInTheDocument();
  });

  it("a settled read without the series discards the receipt for good: a later regrant starts clean", async () => {
    await show(member());
    vi.mocked(Events.registerSeries).mockResolvedValue(batch());
    await userEvent.click(inBar("Going"));
    await userEvent.click(within(scopeDialog()).getByRole("button", { name: "All future dates" }));
    await screen.findByText("Going to 2 dates");
    vi.mocked(Dates.get).mockResolvedValue({ occurrence: member({ series: undefined }) });
    fireEvent(window, new Event("focus"));
    await waitFor(() => expect(Dates.get).toHaveBeenCalledTimes(3));
    await screen.findByRole("heading", { level: 1, name: "Romans 8" });
    expect(within(bar()).queryByRole("button", { name: "Undo" })).not.toBeInTheDocument();
    vi.mocked(Dates.get).mockResolvedValue({ occurrence: member() });
    fireEvent(window, new Event("focus"));
    await waitFor(() => expect(Dates.get).toHaveBeenCalledTimes(4));
    await screen.findByRole("heading", { level: 1, name: "Romans 8" });
    expect(within(bar()).queryByRole("button", { name: "Undo" })).not.toBeInTheDocument();
    expect(Events.undoSeriesRegistration).not.toHaveBeenCalled();
  });

  it("an Undo during the page's own read wins over that older read, even when it lands late", async () => {
    await show(member());
    const first = deferred<{ occurrence: EventDate }>();
    const second = deferred<{ occurrence: EventDate }>();
    vi.mocked(Dates.get).mockReturnValueOnce(first.promise).mockReturnValueOnce(second.promise);
    vi.mocked(Events.registerSeries).mockResolvedValue(batch());
    vi.mocked(Events.undoSeriesRegistration).mockResolvedValue(batch({ undoToken: undefined, results: [{ occurrenceId: 52, date: "2026-12-08", before: { status: "going" }, after: {} }] }));
    await userEvent.click(inBar("Going"));
    await userEvent.click(within(scopeDialog()).getByRole("button", { name: "All future dates" }));
    // The generic Undo of the page's own read is live: use it before that read lands.
    await userEvent.click(await within(bar()).findByRole("button", { name: "Undo" }));
    await waitFor(() => expect(Dates.get).toHaveBeenCalledTimes(3));
    await act(async () => { first.resolve({ occurrence: member({ series: seriesOf({ coveredDates: 4 }) }) }); await first.promise; });
    expect(screen.getByText("Updating your answer…")).toBeInTheDocument();
    expect(screen.queryByText(/answered all 4/)).not.toBeInTheDocument();
    await act(async () => { second.resolve({ occurrence: member({ series: seriesOf({ coveredDates: 1 }) }) }); await second.promise; });
    expect(await screen.findByText("You've answered 1 of 4 upcoming dates.")).toBeInTheDocument();
    expect(screen.getByText(/^Undo: /)).toBeInTheDocument();
    expect(within(bar()).queryByRole("button", { name: "Undo" })).not.toBeInTheDocument();
  });

  it.each(["single", "series"] as const)("a %s write that succeeds after a newer focus read checks again once, fail closed, with no summary", async (kind) => {
    await show(member({ series: seriesOf({ canAnswerSeries: kind === "series" }) }));
    const write = deferred<SeriesRegistrationResponse>();
    const single = deferred<{ registration: RegistrationResponse; people: [] }>();
    vi.mocked(Events.registerSeries).mockReturnValue(write.promise);
    vi.mocked(Events.register).mockReturnValue(single.promise);
    await userEvent.click(inBar("Maybe"));

    if (kind === "series") await userEvent.click(within(scopeDialog()).getByRole("button", { name: "All future dates" }));
    fireEvent(window, new Event("focus"));
    await waitFor(() => expect(Dates.get).toHaveBeenCalledTimes(2));
    await screen.findByRole("heading", { level: 1, name: "Romans 8" });
    const recheck = deferred<{ occurrence: EventDate }>();
    vi.mocked(Dates.get).mockReturnValue(recheck.promise);
    await act(async () => {
      if (kind === "series") write.resolve(batch()); else single.resolve({ registration, people: [] });
      await Promise.resolve();
    });
    await waitFor(() => expect(Dates.get).toHaveBeenCalledTimes(3));
    // The recheck is not this page's own read: nothing is held, no Undo shows while it is out.
    expect(screen.getByText("Loading this date…")).toBeInTheDocument();
    expect(screen.queryByRole("region", { name: "Answer this date" })).not.toBeInTheDocument();
    await act(async () => { recheck.resolve({ occurrence: member({ myRegistration: "maybe", series: seriesOf({ canAnswerSeries: kind === "series" }) }) }); await recheck.promise; });
    await screen.findByText("You said maybe. Change it anytime.");
    expect(Dates.get).toHaveBeenCalledTimes(3);
    expect(screen.queryByText(/Maybe for|Going to/)).not.toBeInTheDocument();
    // The newest receipt is still this person's on this date, so it is kept.
    expect(within(bar()).queryByRole("button", { name: "Undo" }) !== null).toBe(kind === "series");
  });

  it("a different person on the same address starts with no receipt or result", async () => {
    vi.mocked(Dates.get).mockResolvedValue({ occurrence: member() });
    const view = mount(); await screen.findByRole("heading", { level: 1, name: "Romans 8" });
    vi.mocked(Events.registerSeries).mockResolvedValue(batch());
    await userEvent.click(inBar("Going"));
    await userEvent.click(within(scopeDialog()).getByRole("button", { name: "All future dates" }));
    await screen.findByText("Going to 2 dates");
    expect(within(bar()).getByRole("button", { name: "Undo" })).toBeInTheDocument();
    actor.user = { id: 3 };
    view.rerender(page());
    await screen.findByRole("heading", { level: 1, name: "Romans 8" });
    expect(within(bar()).queryByRole("button", { name: "Undo" })).not.toBeInTheDocument();
    expect(screen.queryByText("Going to 2 dates")).not.toBeInTheDocument();
    expect(Events.undoSeriesRegistration).not.toHaveBeenCalled();
  });

  it("a late series reply after leaving the page writes nothing into the next one", async () => {
    vi.mocked(Dates.get).mockResolvedValue({ occurrence: member() });
    const view = mount(); await screen.findByRole("heading", { level: 1, name: "Romans 8" });
    const write = deferred<SeriesRegistrationResponse>();
    vi.mocked(Events.registerSeries).mockReturnValue(write.promise);
    await userEvent.click(inBar("Going"));
    await userEvent.click(within(scopeDialog()).getByRole("button", { name: "All future dates" }));
    view.unmount();
    await act(async () => { write.resolve(batch()); await write.promise; });
    expect(Dates.get).toHaveBeenCalledTimes(1);
    mount(); await screen.findByRole("heading", { level: 1, name: "Romans 8" });
    expect(within(bar()).queryByRole("button", { name: "Undo" })).not.toBeInTheDocument();
  });

  describe("a reader with the ordinary view and a live invitation", () => {
    const dual = (over: Partial<EventDate> = {}) => member({ canDecline: true, canBlock: true, ...over });

    it("answers just this date through the invitation and a wider scope through the series", async () => {
      await show(dual());
      await userEvent.click(inBar("Maybe"));
      await userEvent.click(within(scopeDialog()).getByRole("button", { name: "Just this date" }));
      await waitFor(() => expect(Dates.register).toHaveBeenCalledWith("7", "2026-12-01", { status: "maybe", attendanceMode: "in_person", guestCount: 1 }));
      expect(Events.register).not.toHaveBeenCalled();
      await waitFor(() => expect(inBar("Maybe")).not.toHaveAttribute("aria-disabled"));
      vi.mocked(Events.registerSeries).mockResolvedValue(batch());
      await userEvent.click(inBar("Going"));
      await userEvent.click(within(scopeDialog()).getByRole("button", { name: "All future dates" }));
      await waitFor(() => expect(Events.registerSeries).toHaveBeenCalledWith(7, { scope: "all_future", status: "going", attendanceMode: "in_person", guestCount: 1 }));
      expect(Dates.register).toHaveBeenCalledOnce();
    });

    it("Can't go for just this date ends the invitation after its own question, and keeps the ordinary view", async () => {
      await show(dual());
      await userEvent.click(inBar("Can't go"));
      await userEvent.click(within(scopeDialog()).getByRole("button", { name: "Just this date" }));
      const ask = await screen.findByRole("dialog", { name: "Can't go to this date?" });
      expect(within(ask).getByRole("button", { name: "Cancel" })).toHaveFocus();
      vi.mocked(Dates.get).mockResolvedValue({ occurrence: member({ myRegistration: "declined", myAttendanceMode: undefined, myGuestCount: undefined }) });
      await userEvent.click(within(ask).getByRole("button", { name: "Can't go" }));
      await waitFor(() => expect(Dates.end).toHaveBeenCalledWith("7", "2026-12-01", "decline"));
      expect(await screen.findByText("You said you can't go.")).toBeInTheDocument();
      expect(screen.getByRole("heading", { level: 1, name: "Romans 8" })).toBeInTheDocument();
      expect(Events.register).not.toHaveBeenCalled(); expect(Events.registerSeries).not.toHaveBeenCalled();
    });

    it("Don't add me again never opens the scope picker", async () => {
      await show(dual());
      await userEvent.click(screen.getByRole("button", { name: "Don't add me again" }));
      expect(screen.queryByRole("dialog", { name: /How far should/ })).not.toBeInTheDocument();
      expect(screen.getByRole("dialog", { name: "Don't add me again?" })).toBeInTheDocument();
    });
  });

  describe("full pools", () => {
    const fullSeats = (waitlistEnabled: boolean) => ({ inPerson: { capacity: 4, used: 4, left: 0 }, remote: { capacity: 5, used: 0, left: 5 }, waitlistEnabled });

    it("without a waitlist, keeps Going from someone with no seat on this date alone, and Maybe and Can't go open", async () => {
      await show(member({ myRegistration: null, myAttendanceMode: undefined, myGuestCount: undefined, seats: fullSeats(false), series: seriesOf({ canAnswerSeries: false }) }));
      expect(inBar("Going")).toBeDisabled();
      expect(inBar("Maybe")).toBeEnabled(); expect(inBar("Can't go")).toBeEnabled();
      expect(screen.getByText(/^This date is full\. Going isn't open for this date alone\./)).toBeInTheDocument();
      // Another mode with room is the person's own choice.
      await userEvent.click(screen.getByLabelText("Remote"));
      expect(inBar("Going")).toBeEnabled();
    });

    it("judges the mode the person chooses: a Going seat in person holds none in a full remote pool", async () => {
      await show(member({ seats: { inPerson: { capacity: 12, used: 3, left: 9 }, remote: { capacity: 5, used: 5, left: 0 }, waitlistEnabled: false }, series: seriesOf({ canAnswerSeries: false }) }));
      expect(inBar("Going")).toBeEnabled();
      await userEvent.click(screen.getByLabelText("Remote"));
      expect(inBar("Going")).toBeDisabled();
      expect(screen.getByText(/^Remote seats are full for this date\. Going isn't open for this date alone\./)).toBeInTheDocument();
      expect(inBar("Maybe")).toBeEnabled(); expect(inBar("Can't go")).toBeEnabled();
    });

    it("keeps a Going member's own seat open to them", async () => {
      await show(member({ seats: fullSeats(false), series: seriesOf({ canAnswerSeries: false }) }));
      expect(inBar("Going")).toBeEnabled();
      vi.mocked(Events.register).mockResolvedValue({ registration, people: [] });
      await userEvent.click(inBar("Going"));
      expect(Events.register).toHaveBeenCalledOnce();
    });

    it("leaves the wider scopes open when this date is full", async () => {
      await show(member({ myRegistration: null, myAttendanceMode: undefined, myGuestCount: undefined, seats: fullSeats(false) }));
      await userEvent.click(inBar("Going"));
      const dialog = scopeDialog();
      expect(within(dialog).getByRole("button", { name: "Just this date" })).toBeDisabled();
      expect(within(dialog).getByRole("button", { name: "Just this date" })).toHaveAccessibleDescription("This date is full.");
      vi.mocked(Events.registerSeries).mockResolvedValue(batch({ refusals: [{ occurrenceId: 51, date: "2026-12-01", code: "event_full" }] }));
      await userEvent.click(within(dialog).getByRole("button", { name: "All future dates" }));
      await waitFor(() => expect(Events.registerSeries).toHaveBeenCalledOnce());
    });

    // Only a Going row in the same pool holds a seat: a waitlisted row would only be saved as waiting again.
    const waiting = (over: Partial<EventDate> = {}) => member({ myRegistration: "waitlisted", myGuestCount: 0, seats: fullSeats(false), ...over });

    it("keeps Going from a waitlisted person once the waitlist is off, with Maybe and Can't go open", async () => {
      await show(waiting({ series: seriesOf({ canAnswerSeries: false }) }));
      expect(inBar("Going")).toBeDisabled();
      expect(inBar("Maybe")).toBeEnabled(); expect(inBar("Can't go")).toBeEnabled();
      expect(screen.getByText(/^This date is full\. Going isn't open for this date alone\. You can still answer Maybe or Can't go\.$/)).toBeInTheDocument();
      vi.mocked(Events.register).mockResolvedValue({ registration, people: [] });
      await userEvent.click(inBar("Maybe"));
      await waitFor(() => expect(Events.register).toHaveBeenCalledWith(7, { date: "2026-12-01", status: "maybe", attendanceMode: "in_person", guestCount: 0 }));
      expect(Events.register).toHaveBeenCalledOnce();
    });

    it("leaves a waitlisted person the wider scopes, and this date's Maybe and Can't go, once the waitlist is off", async () => {
      await show(waiting());
      expect(screen.getByText(/^This date is full\. Going isn't open for this date alone\. You can still answer Maybe or Can't go, or choose other dates in the series\.$/)).toBeInTheDocument();
      await userEvent.click(inBar("Going"));
      let dialog = scopeDialog();
      expect(within(dialog).getByRole("button", { name: "Just this date" })).toBeDisabled();
      expect(within(dialog).getByRole("button", { name: "Just this date" })).toHaveAccessibleDescription("This date is full.");
      expect(within(dialog).getByRole("button", { name: "This and following dates" })).toBeEnabled();
      expect(within(dialog).getByRole("button", { name: "All future dates" })).toBeEnabled();
      await userEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));

      for (const answer of ["Maybe", "Can't go"]) {
        await userEvent.click(inBar(answer));
        dialog = scopeDialog();
        expect(within(dialog).getByRole("button", { name: "Just this date" })).toBeEnabled();
        await userEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
      }

      vi.mocked(Events.registerSeries).mockResolvedValue(batch({ refusals: [{ occurrenceId: 51, date: "2026-12-01", code: "event_full" }] }));
      await userEvent.click(inBar("Going"));
      await userEvent.click(within(scopeDialog()).getByRole("button", { name: "This and following dates" }));
      await waitFor(() => expect(Events.registerSeries).toHaveBeenCalledWith(7, { scope: "this_and_following", status: "going", date: "2026-12-01", attendanceMode: "in_person", guestCount: 0 }));
      expect(Events.register).not.toHaveBeenCalled();
    });

    it("with a waitlist, lets Going ask, and the waiting copy makes no promise", async () => {
      await show(member({ myRegistration: null, myAttendanceMode: undefined, myGuestCount: undefined, seats: fullSeats(true), series: seriesOf({ canAnswerSeries: false }) }));
      expect(inBar("Going")).toBeEnabled();
      vi.mocked(Events.register).mockResolvedValue({ registration: { ...registration, status: "waitlisted", waitlistPosition: null }, people: [] });
      vi.mocked(Dates.get).mockResolvedValue({ occurrence: member({ myRegistration: "waitlisted", seats: fullSeats(true), series: seriesOf({ canAnswerSeries: false }) }) });
      await userEvent.click(inBar("Going"));
      const status = await screen.findByText("You're on the waitlist.");
      const region = status.closest("[role=status]")!;
      expect(region).not.toHaveTextContent(/position/i);
      expect(region).toHaveTextContent("If a space opens and your party fits, you may be moved to Going. Fold will let you know if that happens.");
      expect(document.body).not.toHaveTextContent(/deadline|by \w+day|within \d|guarantee|leader will|we'll email|next in line|confirm your/i);
      expect(inBar("Going")).toHaveAttribute("aria-pressed", "true");
    });
  });

  describe("a signed-out visitor", () => {
    beforeEach(() => { actor.user = null; });

    it("opens the guest form from the bar's one opener, starts on its first field and comes back on Never mind", async () => {
      await show(member({ canRegister: false, myRegistration: null, myAttendanceMode: undefined, myGuestCount: undefined, series: seriesOf({ upcomingDates: undefined, coveredDates: undefined, canAnswerSeries: false }) }));
      expect(screen.getAllByRole("button", { name: "Register as a guest" })).toHaveLength(1);
      const opener = inBar("Register as a guest");
      await userEvent.click(opener);
      expect(screen.queryByRole("button", { name: "Register as a guest" })).not.toBeInTheDocument();
      expect(screen.getByRole("textbox", { name: "Full name" })).toHaveFocus();
      await userEvent.type(screen.getByRole("textbox", { name: "Full name" }), "Pat Guest");
      await userEvent.click(screen.getByRole("button", { name: "Never mind" }));
      expect(inBar("Register as a guest")).toHaveFocus();
      expect(screen.getByRole("link", { name: "Log in to answer" })).toHaveAttribute("href", "/login");
      expect(screen.queryByRole("group", { name: "Your answer" })).not.toBeInTheDocument();
      expect(screen.queryByRole("status")).not.toBeInTheDocument();
    });
  });
});
