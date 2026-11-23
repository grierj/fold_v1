import { testEvent, testUser } from "../test/fixtures";
import { renderWithAuth } from "../test/auth";
import { stubService } from "../test/service";
import { act, cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useLayoutEffect } from "react";
import { Link, MemoryRouter, Route, RouterProvider, Routes, createMemoryRouter, useLocation, useNavigate } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError, type Credential } from "../api/core";
import { Events } from "../api/events";
import { Groups } from "../api/groups";
import { Households } from "../api/households";
import { People } from "../api/people";
import type { GroupDetailResponse, GroupSummary, Household, OccurrenceResponse, User, Visibility, VisibilityImpact } from "../types";
import { occurrence } from "../test/occurrence";
import { Site } from "../api/site";
import { createRosterWrites, DateWritesProvider, type RosterWriteEnd, type RosterWrites } from "../lib/dateWrites";
import { HOST_EVENTS, kept409, keptRead, READ_OUTCOMES, runHostCase } from "../test/keptPanel";
import { UnsavedChangesBlocker } from "../lib/unsavedChanges";
import { endsFor, removedRow, ROSTER_ENDS, ROSTER_OPS, startHeld, watchChecks, writeAfterTheCheck } from "../test/rosterOps";
import { GroupDetail } from "./GroupDetail";
import { stubEditSends, verifiedCredential, type EditSends } from "../test/guardedEdits";
import { setMarkerCookie } from "../test/credential";
import { erasureCases, SignedInSession } from "../test/session";
import { ERROR_COPY } from "../lib/errors";
import type { JsonObject } from "../lib/wireValues";

type TestSession = { user: User | null; canCompose: boolean };

const actor: TestSession = {
  user: testUser({ id: 2, accountType: "adult", hideContactFromLeaders: false }),
  canCompose: false,
};

const render = renderWithAuth(() => ({ user: actor.user, loading: false, isAdmin: false, canCompose: actor.canCompose }));

stubService(People, () => ({ search: vi.fn() }));

stubService(Groups, () => ({ get: vi.fn(), update: vi.fn(), visibilityImpact: vi.fn(), addMember: vi.fn(), join: vi.fn(), departurePreview: vi.fn(), confirmDeparture: vi.fn(), memberDeparturePreview: vi.fn(), confirmMemberDeparture: vi.fn() }));

stubService(Events, () => ({ register: vi.fn(), registerSeries: vi.fn(), undoSeriesRegistration: vi.fn(), dateVisibilityImpact: vi.fn(), managedDate: vi.fn(), removeRegistration: vi.fn(), addRegistration: vi.fn(), restoreRegistration: vi.fn(), update: vi.fn(), visibilityImpact: vi.fn(), setVisibility: vi.fn(), cancelOccurrence: vi.fn() }));

stubService(Households, () => ({ mine: vi.fn() }));

const family: Household = { id: 1, name: "Family", address: "", createdBy: 2, accounts: [], people: [{ id: 3, householdId: 1, canConvert: false, firstName: "Mary", lastName: "", name: "Mary", kind: "adult", photoConsent: false, linkStatus: "none", registrationEligibility: "eligible", createdBy: 2 }] };
const page: GroupDetailResponse = { group: { id: 1, name: "Family group", description: "", tags: [], imageUrl: "", coverFocalX: 50, coverFocalY: 50, location: "Hall", leaderName: "Leader", memberCount: 1, nextEventAt: null, status: "active", leaderId: 1, visibility: "public", deletionRequested: false, memberLimit: null }, members: [], upcomingEvents: [occurrence({})], truncated: false, hasMore: false, nextCursor: "", canManage: false, isMember: true };

function open() { render(<MemoryRouter initialEntries={["/groups/1"]}><Routes><Route path="/groups/:id" element={<GroupDetail />} /></Routes></MemoryRouter>); }

function GroupRouteWithSwitcher() {
  const navigate = useNavigate();

  return <><button onClick={() => navigate("/groups/2")}>Open next group</button><GroupDetail /></>;
}

function openWithSwitcher() {
  render(<MemoryRouter initialEntries={["/groups/1"]}><Routes>
    <Route path="/groups/:id" element={<GroupRouteWithSwitcher />} />
    <Route path="/my-groups" element={<h1>My groups destination</h1>} />
  </Routes></MemoryRouter>);
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (cause: unknown) => void;
  const promise = new Promise<T>((accept, decline) => { resolve = accept; reject = decline; });

  return { promise, resolve, reject };
}

beforeEach(() => { vi.resetAllMocks(); actor.user = testUser({ id: 2, accountType: "adult", hideContactFromLeaders: false }); vi.mocked(Groups.get).mockResolvedValue(page); vi.mocked(Households.mine).mockResolvedValue({ household: family }); vi.mocked(Groups.departurePreview).mockResolvedValue({ departure: { action: "leave", groupId: 1, groupName: "Family group", subjectId: 2, subjectName: "Member", affectedCount: 0, goingCount: 0, maybeCount: 0, waitlistedCount: 0, keepableCount: 0, defaultKeepCount: 0, dates: [], snapshot: "self" } }); vi.mocked(Groups.confirmDeparture).mockResolvedValue({ departure: { action: "leave", groupId: 1, subjectId: 2, keptCount: 0, endedCount: 0 } }); });

// The session's credential and its guarded edits, as App provides them.
let credential: Credential;
let edits: EditSends;

beforeEach(async () => {
  credential = await verifiedCredential();
  edits = stubEditSends();
});

// The editor's own manager read of a date this group lists: the panel's read plus the group it belongs to.
const forGroup = (read: OccurrenceResponse): OccurrenceResponse => ({ occurrence: { ...read.occurrence, groupId: 1, groupName: "Family group", groupVisibility: "public" } });

it("fetches the household once and refetches all date state after a batch", async () => {
  vi.mocked(Events.register).mockResolvedValue({ registration: null, people: [{ registrationId: 9, personId: 3, registeredBy: 2, status: "going", attendanceMode: "in_person", waitlistPosition: null, childcareRequested: false }] });
  open(); await userEvent.click(await screen.findByRole("checkbox", { name: "Mary" }));
  await userEvent.click(screen.getByRole("button", { name: "Save answers" }));
  await waitFor(() => expect(Groups.get).toHaveBeenCalledTimes(2));
  expect(Households.mine).toHaveBeenCalledTimes(1);
  expect(Events.register).toHaveBeenCalledWith(7, { date: "2026-12-01", people: [{ personId: 3, status: "going", attendanceMode: "in_person", childcareRequested: false }] });
});

it("serializes household RSVP saves across sibling events and releases without resetting drafts", async () => {
  const events = [occurrence({ eventId: 7 }), occurrence({ id: 2, eventId: 8, date: "2026-12-08" })];
  vi.mocked(Groups.get).mockResolvedValue({ ...page, upcomingEvents: events });
  const pending = deferred<Awaited<ReturnType<typeof Events.register>>>();
  vi.mocked(Events.register).mockReturnValue(pending.promise);

  open();
  const forms = await screen.findAllByRole("form", { name: "Household answers" });
  await userEvent.click(within(forms[0]).getByRole("checkbox", { name: "Mary" }));
  await userEvent.selectOptions(within(forms[0]).getByLabelText("Answer for Mary"), "maybe");
  await userEvent.click(within(forms[1]).getByRole("checkbox", { name: "Mary" }));
  await userEvent.selectOptions(within(forms[1]).getByLabelText("Answer for Mary"), "maybe");

  await act(() => {
    within(forms[0]).getByRole("button", { name: "Save answers" }).click();
    within(forms[1]).getByRole("button", { name: "Save answers" }).click();
  });
  expect(Events.register).toHaveBeenCalledTimes(1);
  expect(within(forms[1]).getByRole("checkbox", { name: "Mary" })).toBeDisabled();
  expect(within(forms[1]).getByLabelText("Answer for Mary")).toHaveValue("maybe");

  await act(async () => pending.reject(new Error("offline")));
  await waitFor(() => expect(within(forms[1]).getByRole("checkbox", { name: "Mary" })).toBeEnabled());
  expect(within(forms[1]).getByRole("checkbox", { name: "Mary" })).toBeChecked();
  expect(within(forms[1]).getByLabelText("Answer for Mary")).toHaveValue("maybe");

  vi.mocked(Events.register).mockResolvedValue({ registration: null, people: [{ registrationId: 9, personId: 3, registeredBy: 2, status: "maybe", attendanceMode: "in_person", waitlistPosition: null, childcareRequested: false }] });
  await userEvent.click(within(forms[1]).getByRole("button", { name: "Save answers" }));
  expect(Events.register).toHaveBeenLastCalledWith(8, {
    date: "2026-12-08", people: [{ personId: 3, status: "maybe", attendanceMode: "in_person", childcareRequested: false }],
  });
});

it("opens the same series scope dialog from a group date for a solo viewer", async () => {
  vi.mocked(Households.mine).mockResolvedValue({ household: null });
  vi.mocked(Groups.get).mockResolvedValue({ ...page, upcomingEvents: [occurrence({ recurrenceRule: "FREQ=WEEKLY" })] });
  open();
  await userEvent.click(await screen.findByRole("button", { name: "Going" }));
  expect(screen.getByRole("dialog", { name: /How far should “Going” apply/ })).toBeInTheDocument();
});

it("closes a group date's scope picker when a held refresh moves a later date of its series that the card does not show, and sends nothing", async () => {
  vi.mocked(Households.mine).mockResolvedValue({ household: null });
  const weekly = (id: number, date: string, hour = "19") => occurrence({ id, date, startsAt: `${date}T${hour}:00:00Z`, endsAt: `${date}T${hour}:30:00Z`, recurrenceRule: "FREQ=WEEKLY" });
  const loaded = { ...page, upcomingEvents: [weekly(1, "2026-12-01"), occurrence({ id: 3, eventId: 8, date: "2026-12-03", title: "Picnic" }), weekly(2, "2026-12-08")] };
  const refresh = deferred<GroupDetailResponse>();
  vi.mocked(Groups.get).mockResolvedValueOnce(loaded).mockReturnValueOnce(refresh.promise);
  vi.mocked(Events.register).mockResolvedValue({ registration: {
    registrationId: 12, userId: 2, eventId: 8, occurrenceId: 3, date: "2026-12-03", status: "going",
    guestCount: 0, childcareCount: 0, childcareAgesNote: "", waitlistPosition: null, attendanceMode: "in_person",
  }, people: [] });
  open();
  // The picnic's answer starts a quiet refresh, held here; the series card shows only its next date.
  const answerGroups = await screen.findAllByRole("group", { name: "Your answer" });
  expect(answerGroups).toHaveLength(2);
  await userEvent.click(within(answerGroups[1]).getByRole("button", { name: "Going" }));
  await waitFor(() => expect(Groups.get).toHaveBeenCalledTimes(2));
  await userEvent.click(within(answerGroups[0]).getByRole("button", { name: "Going" }));
  const choice = within(screen.getByRole("dialog", { name: /How far should “Going” apply/ })).getByRole("button", { name: "All future dates" });

  await act(async () => refresh.resolve({ ...loaded, upcomingEvents: [loaded.upcomingEvents[0], loaded.upcomingEvents[1], { ...weekly(2, "2026-12-08", "09"), overridden: true }] }));
  await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  act(() => { choice.click(); });
  expect(Events.register).toHaveBeenCalledOnce();
  expect(Events.registerSeries).not.toHaveBeenCalled();
  expect(within(screen.getAllByRole("group", { name: "Your answer" })[0]).getByRole("button", { name: "Going" })).toBeEnabled();
});

it("keeps account-only series scope available on a group date with household people", async () => {
  vi.mocked(Groups.get).mockResolvedValue({ ...page, upcomingEvents: [occurrence({ recurrenceRule: "FREQ=WEEKLY" })] });
  vi.mocked(Events.registerSeries).mockResolvedValue({
    eventId: 7, scope: "this_and_following", status: "going",
    results: [{ occurrenceId: 1, date: "2026-12-01", before: {}, after: { registrationId: 4, status: "going" } }],
    refusals: [],
  });
  open();
  await userEvent.click(await screen.findByRole("checkbox", { name: "Me" }));
  await userEvent.click(screen.getByRole("checkbox", { name: "Mary" }));
  await userEvent.click(screen.getByRole("button", { name: "Choose dates for my answer" }));
  await userEvent.click(screen.getByRole("button", { name: "This and following dates" }));
  expect(Events.registerSeries).toHaveBeenCalledWith(7, expect.objectContaining({ scope: "this_and_following", date: "2026-12-01", status: "going" }));
  expect(screen.getByRole("checkbox", { name: "Mary" })).toBeChecked();
});

it("shows the authoritative series result on a group card while its parent GET is held", async () => {
  vi.mocked(Households.mine).mockResolvedValue({ household: null });
  const initial = { ...page, upcomingEvents: [occurrence({ recurrenceRule: "FREQ=WEEKLY", myRegistration: null })] };
  const held = deferred<GroupDetailResponse>();
  vi.mocked(Groups.get).mockResolvedValueOnce(initial).mockReturnValueOnce(held.promise);
  vi.mocked(Events.registerSeries).mockResolvedValue({
    eventId: 7, scope: "all_future", status: "waitlisted",
    results: [{ occurrenceId: 1, date: "2026-12-01", before: {}, after: {
      registrationId: 4, status: "waitlisted", waitlistPosition: 3, attendanceMode: "in_person",
      guestCount: 0, childcareCount: 0, childcareAgesNote: "",
    } }], refusals: [],
  });

  open();
  await userEvent.click(await screen.findByRole("button", { name: "Going" }));
  await userEvent.click(screen.getByRole("button", { name: "All future dates" }));
  await waitFor(() => expect(Groups.get).toHaveBeenCalledTimes(2));
  expect(screen.getByRole("button", { name: "Going" })).toHaveAttribute("aria-pressed", "true");
  expect(screen.getByText(/Waitlisted — position 3/)).toBeInTheDocument();
  await act(async () => held.resolve({ ...initial, upcomingEvents: [occurrence({ recurrenceRule: "FREQ=WEEKLY",
    myRegistration: "waitlisted", myWaitlistPosition: 3, myAttendanceMode: "in_person", myGuestCount: 0 })] }));
  expect(screen.getByRole("button", { name: "Going" })).toHaveAttribute("aria-pressed", "true");
});

it("clears protected group data while a terminal Undo refresh is still pending", async () => {
  const recurring = { ...page, upcomingEvents: [occurrence({ recurrenceRule: "FREQ=WEEKLY" })] };
  const terminal = deferred<GroupDetailResponse>();
  vi.mocked(Groups.get)
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
  open();
  await userEvent.click(await screen.findByRole("button", { name: "Maybe" }));
  await userEvent.click(screen.getByRole("button", { name: "All future dates" }));
  await userEvent.click(await screen.findByRole("button", { name: "Undo" }));
  await waitFor(() => expect(Groups.get).toHaveBeenCalledTimes(3));
  expect(screen.queryByRole("heading", { name: "Family group" })).not.toBeInTheDocument();
  await act(async () => terminal.reject(new ApiError(404, "hidden", { code: "not_found" })));
  expect(await screen.findByRole("heading", { name: "Group not found" })).toBeInTheDocument();
});

it("keeps the newest group refresh when an older quiet request resolves last", async () => {
  const recurring = { ...page, upcomingEvents: [occurrence({ recurrenceRule: "FREQ=WEEKLY" })] };
  const old = deferred<GroupDetailResponse>();
  const terminal = deferred<GroupDetailResponse>();
  const stale = { ...recurring, group: { ...recurring.group, name: "Stale group details" } };
  const current = { ...recurring, group: { ...recurring.group, name: "Current group details" } };
  vi.mocked(Groups.get)
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
  open();
  await userEvent.click(await screen.findByRole("button", { name: "Maybe" }));
  await userEvent.click(screen.getByRole("button", { name: "All future dates" }));
  await userEvent.click(await screen.findByRole("button", { name: "Undo" }));
  await waitFor(() => expect(Groups.get).toHaveBeenCalledTimes(3));
  expect(screen.queryByRole("heading", { name: "Family group" })).not.toBeInTheDocument();
  await act(async () => terminal.resolve(current));
  expect(await screen.findByRole("heading", { name: "Current group details" })).toBeInTheDocument();
  await act(async () => old.resolve(stale));
  expect(screen.getByRole("heading", { name: "Current group details" })).toBeInTheDocument();
  expect(screen.queryByRole("heading", { name: "Stale group details" })).not.toBeInTheDocument();
});

it.each([null, testUser({ id: 2, accountType: "teen", hideContactFromLeaders: true })])("does not fetch household data for an unsigned or teen viewer", async (user) => {
  actor.user = user; open(); await screen.findByRole("heading", { name: "Family group" });
  expect(Households.mine).not.toHaveBeenCalled();
});

it("shows supplied roster contact without reconstructing hidden members' details", async () => {
  const member = { avatarUrl: "", avatarFocalX: 50, avatarFocalY: 50, role: "member", joinedAt: "2026-10-01T00:00:00Z", admin: false };
  vi.mocked(Groups.get).mockResolvedValue({ ...page, members: [
    { ...member, id: 3, name: "Visible adult", email: "visible@example.com", phone: "208-555-0100" },
    { ...member, id: 4, name: "Hidden adult" },
    { ...member, id: 5, name: "Teen member", accountType: "teen" },
  ] });
  open();
  await userEvent.click(await screen.findByRole("tab", { name: "Members" }));
  expect(screen.getByRole("link", { name: "visible@example.com" })).toBeInTheDocument();
  expect(screen.getByText("Phone: 208-555-0100")).toBeInTheDocument();
  expect(screen.getAllByRole("link", { name: /@/ })).toHaveLength(1);
  expect(screen.getAllByText(/@/)).toHaveLength(1); // no plain-text fallback for hidden contacts either
  expect(screen.queryByText(/Email:/)).not.toBeInTheDocument();
  expect(screen.getAllByText(/Phone:/)).toHaveLength(1);
  expect(screen.getByText("Hidden adult")).toBeInTheDocument();
  expect(screen.getByText("Teen member")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /contact/i })).not.toBeInTheDocument();
});

it.each(["group_full", "teen_add_approval_required", "people_selection_unavailable"])("adds by name and only retries the actual member limit: %s", async (code) => {
  actor.user = testUser({ id: 2, status: "active", accountType: "adult" });
  vi.mocked(Groups.get).mockResolvedValue({ ...page, canManage: true });
  vi.mocked(People.search).mockResolvedValue({ state: "results", people: [{ selectionId: "opaque", displayName: "Pat", teen: false, sharedGroups: [], otherGroups: [] }] });
  vi.mocked(Groups.addMember).mockRejectedValueOnce(new ApiError(409, "private", { code })).mockResolvedValueOnce({});
  vi.spyOn(window, "confirm").mockReturnValue(true);
  open(); await userEvent.click(await screen.findByRole("tab", { name: "Members" }));
  await userEvent.click(screen.getByRole("button", { name: "Add people" }));
  await userEvent.type(within(screen.getByRole("region", { name: "Add people" })).getByRole("combobox"), "Pat"); await userEvent.click(await within(screen.getByRole("region", { name: "Add people" })).findByRole("option"));
  await userEvent.click(screen.getByRole("button", { name: "Add member" }));
  expect(Groups.addMember).toHaveBeenNthCalledWith(1, 1, { selectionId: "opaque" });

  if (code === "group_full") expect(Groups.addMember).toHaveBeenLastCalledWith(1, { selectionId: "opaque" }, true);
  else { expect(Groups.addMember).toHaveBeenCalledTimes(1); expect(screen.getByRole("alert")).not.toHaveTextContent("private"); }
});

it("offers an explicit Ask to add for a selected teen instead of direct add", async () => {
  actor.user = testUser({ id: 2, status: "active", accountType: "adult" });
  vi.mocked(Groups.get).mockResolvedValue({ ...page, canManage: true });
  vi.mocked(People.search).mockResolvedValue({ state: "results", people: [{ selectionId: "teen", displayName: "Teen Pat", teen: true, sharedGroups: [], otherGroups: [] }] });
  open(); await userEvent.click(await screen.findByRole("tab", { name: "Members" }));
  await userEvent.click(screen.getByRole("button", { name: "Add people" }));
  await userEvent.type(within(screen.getByRole("region", { name: "Add people" })).getByRole("combobox"), "Pat"); await userEvent.click(await within(screen.getByRole("region", { name: "Add people" })).findByRole("option"));
  expect(screen.getByText(/Adding a teen needs their approval/)).toHaveAttribute("role", "status");
  expect(screen.getByRole("button", { name: "Ask to add" })).toBeEnabled();
  await userEvent.click(screen.getByRole("button", { name: "Ask to add" }));
  expect(screen.getByRole("button", { name: "Send ask to add" })).toBeInTheDocument();
  expect(Groups.addMember).not.toHaveBeenCalled();
});

it("routes self-leave through preview and explicit snapshot confirmation", async () => {
  vi.mocked(Groups.get).mockResolvedValueOnce(page).mockResolvedValueOnce({ ...page, isMember: false });
  open();
  await userEvent.click(await screen.findByRole("button", { name: "You're a member" }));
  await userEvent.click(screen.getByRole("menuitem", { name: "Leave group" }));
  const dialog = await screen.findByRole("dialog", { name: "Leave Family group?" });
  expect(Groups.confirmDeparture).not.toHaveBeenCalled();
  await userEvent.click(within(dialog).getByRole("button", { name: "Leave group" }));
  expect(Groups.confirmDeparture).toHaveBeenCalledWith("1", "self", []);
  expect(await screen.findByText("0 dates kept · 0 dates ended.")).toHaveAttribute("role", "status");
});

it("does not let a deferred pre-leave refresh restore protected group authority", async () => {
  const leader = { id: 2, name: "Current Leader", email: "leader@example.com", phone: "208-555-0101", avatarUrl: "", avatarFocalX: 50, avatarFocalY: 50, role: "leader" as const, joinedAt: "2026-10-01T00:00:00Z", admin: false };
  const protectedMember = { ...leader, id: 4, name: "Protected Member", email: "protected@example.com", role: "member" as const };
  const privileged = { ...page, canManage: true, isMember: true, members: [leader, protectedMember] };
  const departed = { ...page, canManage: false, isMember: false, members: [], upcomingEvents: [] };
  const oldRefresh = deferred<GroupDetailResponse>();
  const postDeparture = deferred<GroupDetailResponse>();
  vi.mocked(Groups.get)
    .mockResolvedValueOnce(privileged)
    .mockReturnValueOnce(oldRefresh.promise)
    .mockReturnValueOnce(postDeparture.promise);
  vi.mocked(Events.register).mockResolvedValue({ registration: null, people: [{ registrationId: 9, personId: 3, registeredBy: 2, status: "going", attendanceMode: "in_person", waitlistPosition: null, childcareRequested: false }] });

  open();
  await userEvent.click(await screen.findByRole("checkbox", { name: "Mary" }));
  await userEvent.click(screen.getByRole("button", { name: "Save answers" }));
  await waitFor(() => expect(Groups.get).toHaveBeenCalledTimes(2));
  await userEvent.click(screen.getByRole("button", { name: "More group actions" }));
  await userEvent.click(screen.getByRole("menuitem", { name: "Leave group" }));
  await userEvent.click(within(await screen.findByRole("dialog", { name: "Leave Family group?" })).getByRole("button", { name: "Leave group" }));
  await waitFor(() => expect(Groups.get).toHaveBeenCalledTimes(3));
  expect(screen.queryByRole("heading", { name: "Family group" })).not.toBeInTheDocument();

  await act(async () => postDeparture.resolve(departed));
  expect(await screen.findByRole("button", { name: "Join this group" })).toBeInTheDocument();
  await act(async () => oldRefresh.resolve(privileged));
  expect(screen.getByRole("button", { name: "Join this group" })).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Leave group" })).not.toBeInTheDocument();
  expect(screen.queryByText("Protected Member")).not.toBeInTheDocument();
  expect(screen.queryByText("protected@example.com")).not.toBeInTheDocument();
});

it("does not let a stale post-leave rejection redirect a newer group view", async () => {
  const postDeparture = deferred<GroupDetailResponse>();
  const nextGroup = { ...page, group: { ...page.group, id: 2, name: "Newer group view" }, isMember: false, upcomingEvents: [] };
  vi.mocked(Groups.get)
    .mockResolvedValueOnce(page)
    .mockReturnValueOnce(postDeparture.promise)
    .mockResolvedValueOnce(nextGroup);

  openWithSwitcher();
  await userEvent.click(await screen.findByRole("button", { name: "You're a member" }));
  await userEvent.click(screen.getByRole("menuitem", { name: "Leave group" }));
  await userEvent.click(within(await screen.findByRole("dialog", { name: "Leave Family group?" })).getByRole("button", { name: "Leave group" }));
  await waitFor(() => expect(Groups.get).toHaveBeenCalledTimes(2));
  await userEvent.click(screen.getByRole("button", { name: "Open next group" }));
  expect(await screen.findByRole("heading", { name: "Newer group view" })).toBeInTheDocument();

  await act(async () => postDeparture.reject(new ApiError(404, "hidden", { code: "not_found" })));
  expect(screen.getByRole("heading", { name: "Newer group view" })).toBeInTheDocument();
  expect(screen.queryByRole("heading", { name: "My groups destination" })).not.toBeInTheDocument();
});

it("routes leader removal through the same chooser and preserves its optional note", async () => {
  const member = { id: 3, name: "Pat Person", avatarUrl: "", avatarFocalX: 50, avatarFocalY: 50, role: "member", joinedAt: "2026-10-01T00:00:00Z", admin: false };
  vi.mocked(Groups.get).mockResolvedValue({ ...page, canManage: true, members: [member] });
  vi.mocked(Groups.memberDeparturePreview).mockResolvedValue({ departure: { action: "remove", groupId: 1, groupName: "Family group", subjectId: 3, subjectName: "Pat Person", affectedCount: 0, goingCount: 0, maybeCount: 0, waitlistedCount: 0, keepableCount: 0, defaultKeepCount: 0, dates: [], snapshot: "remove" } });
  vi.mocked(Groups.confirmMemberDeparture).mockResolvedValue({ departure: { action: "remove", groupId: 1, subjectId: 3, keptCount: 0, endedCount: 0 } });
  open();
  await userEvent.click(await screen.findByRole("tab", { name: "Members" }));
  await userEvent.click(screen.getByRole("button", { name: "More for Pat Person" }));
  await userEvent.click(screen.getByRole("menuitem", { name: "Remove from group" }));
  const dialog = await screen.findByRole("dialog", { name: "Remove Pat Person?" });
  await userEvent.type(within(dialog).getByLabelText("Note for church admins (optional)"), "Requested by pastor");
  await userEvent.click(within(dialog).getByRole("button", { name: "Remove from group" }));
  await waitFor(() => expect(Groups.confirmMemberDeparture).toHaveBeenCalledWith("1", 3, "remove", [], "Requested by pastor"));
});

it("notes on a date whose visibility was changed", async () => {
  vi.mocked(Groups.get).mockResolvedValue({ ...page, canManage: true, upcomingEvents: [occurrence({ visibilityOverride: "public" })] });
  open();
  expect(await screen.findByText("Visibility changed for this date")).toBeInTheDocument();
});

it("links to the full group calendar when upcoming dates are truncated", async () => {
  const upcomingEvents = Array.from({ length: 25 }, (_, i) => occurrence({ id: i + 1, eventId: i + 1 }));
  vi.mocked(Groups.get).mockResolvedValue({ ...page, upcomingEvents, truncated: true });
  open();
  expect(await screen.findByText("Showing the next 25 dates.")).toBeVisible();
  expect(screen.getByRole("link", { name: "See all dates in the calendar" })).toHaveAttribute("href", "/calendar?group=1");
});

it("says how many dates a complete bounded series has", async () => {
  const dates = [occurrence({ id: 1, date: "2026-12-01" }), occurrence({ id: 2, date: "2026-12-08" }), occurrence({ id: 3, date: "2026-12-15" })]
    .map((d) => ({ ...d, recurrenceRule: "FREQ=WEEKLY", rruleUntil: "2026-12-15" }));

  vi.mocked(Groups.get).mockResolvedValue({ ...page, upcomingEvents: dates });
  open();
  expect(await screen.findByRole("link", { name: "See all 3 dates" })).toHaveAttribute("href", "/events/7");
});

it("offers a plain member a You're a member menu and a leader the nudge on an empty group", async () => {
  open();
  const menu = await screen.findByRole("button", { name: "You're a member" });
  expect(menu).toHaveAttribute("aria-haspopup", "menu");
  expect(screen.queryByRole("button", { name: "Leave group" })).not.toBeInTheDocument();
});

it("nudges a leader to schedule the first meeting on a group with no dates", async () => {
  vi.mocked(Groups.get).mockResolvedValue({ ...page, canManage: true, upcomingEvents: [] });
  open();
  expect(await screen.findByText("Schedule your group's first meeting")).toBeVisible();
  expect(screen.getByText("It takes about a minute.")).toBeVisible();
  const links = screen.getAllByRole("link", { name: "Schedule event" });
  expect(links.some((a) => a.classList.contains("btn-primary") && a.getAttribute("href") === "/groups/1/events/new")).toBe(true);
});

it("keeps the plain empty state for a member", async () => {
  vi.mocked(Groups.get).mockResolvedValue({ ...page, upcomingEvents: [] });
  open();
  expect(await screen.findByText("No upcoming events")).toBeVisible();
  expect(screen.queryByText("Schedule your group's first meeting")).not.toBeInTheDocument();
});

it("does not show a truncation notice for a complete upcoming list", async () => {
  open();
  await screen.findByRole("heading", { name: "Family group" });
  expect(screen.queryByRole("link", { name: "See all dates in the calendar" })).not.toBeInTheDocument();
});

it("merges continuation pages without duplicate dates and counts only loaded series dates", async () => {
  const first = { ...page, upcomingEvents: [occurrence({ recurrenceRule: "FREQ=WEEKLY" })], truncated: true, hasMore: true, nextCursor: "page-two" };
  const second = { ...page, upcomingEvents: [occurrence({ recurrenceRule: "FREQ=WEEKLY" }), occurrence({ id: 2, date: "2026-12-08", recurrenceRule: "FREQ=WEEKLY" })] };
  vi.mocked(Groups.get).mockResolvedValueOnce(first).mockResolvedValueOnce(second);
  open();
  await userEvent.click(await screen.findByRole("button", { name: "Load more dates" }));
  expect(Groups.get).toHaveBeenLastCalledWith("1", { cursor: "page-two" });
  expect(await screen.findByRole("link", { name: "See more dates" })).toHaveAttribute("href", "/events/7");
  expect(screen.queryByText(/next 90 days/)).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Load more dates" })).not.toBeInTheDocument();
  expect(screen.getAllByRole("form", { name: "Household answers" })).toHaveLength(1);
});

it("keeps loaded pages on continuation failure and retries without losing the cursor", async () => {
  const first = { ...page, truncated: true, hasMore: true, nextCursor: "retry-page" };
  vi.mocked(Groups.get).mockResolvedValueOnce(first).mockRejectedValueOnce(new Error("offline")).mockResolvedValueOnce(page);
  open();
  await userEvent.click(await screen.findByRole("button", { name: "Load more dates" }));
  expect(await screen.findByRole("alert")).toBeInTheDocument();
  expect(screen.getByRole("heading", { name: "Family group" })).toBeInTheDocument();
  await userEvent.click(screen.getByRole("button", { name: "Load more dates" }));
  expect(Groups.get).toHaveBeenLastCalledWith("1", { cursor: "retry-page" });
  expect(screen.queryByRole("alert")).not.toBeInTheDocument();
});

it("refreshes every loaded page after an RSVP", async () => {
  const first = { ...page, hasMore: true, truncated: true, nextCursor: "next" };
  const second = { ...page, upcomingEvents: [occurrence({ id: 2, eventId: 8 })] };
  vi.mocked(Groups.get).mockResolvedValueOnce(first).mockResolvedValueOnce(second).mockResolvedValueOnce(first).mockResolvedValueOnce(second);
  vi.mocked(Events.register).mockResolvedValue({ registration: null, people: [] });
  open();
  await userEvent.click(await screen.findByRole("button", { name: "Load more dates" }));
  const forms = await screen.findAllByRole("form", { name: "Household answers" });
  expect(forms).toHaveLength(2);
  await userEvent.click(within(forms[0]).getByRole("checkbox", { name: "Mary" }));
  await userEvent.click(within(forms[0]).getByRole("button", { name: "Save answers" }));
  await waitFor(() => expect(Groups.get).toHaveBeenCalledTimes(4));
  expect(Groups.get).toHaveBeenNthCalledWith(3, "1");
  expect(Groups.get).toHaveBeenNthCalledWith(4, "1", { cursor: "next" });
  expect(screen.getAllByRole("form", { name: "Household answers" })).toHaveLength(2);
});

it("resets continuation on group change and ignores an old pending page", async () => {
  const pending = deferred<GroupDetailResponse>();
  const nextGroup = { ...page, group: { ...page.group, id: 2, name: "Next group" }, upcomingEvents: [] };
  vi.mocked(Groups.get).mockResolvedValueOnce({ ...page, hasMore: true, truncated: true, nextCursor: "old" }).mockReturnValueOnce(pending.promise).mockResolvedValueOnce(nextGroup);
  openWithSwitcher();
  await userEvent.click(await screen.findByRole("button", { name: "Load more dates" }));
  expect(screen.getByRole("button", { name: "Loading dates…" })).toBeDisabled();
  await userEvent.click(screen.getByRole("button", { name: "Open next group" }));
  expect(await screen.findByRole("heading", { name: "Next group" })).toBeInTheDocument();
  expect(Groups.get).toHaveBeenLastCalledWith("2");
  await act(async () => pending.resolve(page));
  expect(screen.getByRole("heading", { name: "Next group" })).toBeInTheDocument();
  expect(screen.queryByRole("form", { name: "Household answers" })).not.toBeInTheDocument();
});

it("clears dates and cursor immediately when the viewer changes", async () => {
  const pending = deferred<GroupDetailResponse>();
  const publicPage = deferred<GroupDetailResponse>();
  vi.mocked(Groups.get).mockResolvedValueOnce({ ...page, hasMore: true, truncated: true, nextCursor: "member-only" }).mockReturnValueOnce(pending.promise).mockReturnValueOnce(publicPage.promise);
  const view = () => <MemoryRouter initialEntries={["/groups/1"]}><Routes><Route path="/groups/:id" element={<GroupDetail />} /></Routes></MemoryRouter>;
  const rendered = render(view());
  await userEvent.click(await screen.findByRole("button", { name: "Load more dates" }));
  actor.user = null;
  rendered.rerender(view());
  await waitFor(() => expect(Groups.get).toHaveBeenCalledTimes(3));
  expect(Groups.get).toHaveBeenLastCalledWith("1");
  expect(screen.queryByRole("heading", { name: "Family group" })).not.toBeInTheDocument();
  await act(async () => pending.resolve(page));
  expect(screen.queryByRole("heading", { name: "Family group" })).not.toBeInTheDocument();
  await act(async () => publicPage.resolve({ ...page, isMember: false, upcomingEvents: [] }));
  expect(await screen.findByRole("heading", { name: "Family group" })).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Load more dates" })).not.toBeInTheDocument();
});

const leaderRow = { id: 2, name: "Current Leader", email: "leader@example.com", phone: "208-555-0101", avatarUrl: "", avatarFocalX: 50, avatarFocalY: 50, role: "leader" as const, joinedAt: "2026-10-01T00:00:00Z", admin: false };
const protectedTeen = { ...leaderRow, id: 4, name: "Protected Teen", email: "protected@example.com", phone: "208-555-0144", role: "member" as const, accountType: "teen" as const };

const managerOnlyDate = occurrence({
  id: 9, eventId: 9, date: "2026-12-15", title: "Leaders planning", seriesTitle: "Leaders planning", attendeesVisible: true,
  attendees: [{ registrationId: 5, userId: 4, name: "Protected Teen", email: "protected@example.com", accountType: "teen", status: "going", guestCount: 0, attendanceMode: "in_person" }],
});

it("reads no date for an editor because dates were listed or more were loaded: only Edit this date reads one, then its page's answers", async () => {
  const second = occurrence({ id: 2, eventId: 8, date: "2026-12-08", startsAt: "2026-12-08T19:00:00Z", endsAt: "2026-12-08T20:00:00Z", startsLocal: "2026-12-08T19:00", groupId: 1 });
  vi.mocked(Groups.get).mockResolvedValueOnce({ ...page, canManage: true, upcomingEvents: [occurrence({ groupId: 1 })], hasMore: true, nextCursor: "c1" })
    .mockResolvedValueOnce({ ...page, canManage: true, upcomingEvents: [second], hasMore: false, nextCursor: "" });
  vi.mocked(Events.managedDate).mockImplementation(async (_id, date) => forGroup({ occurrence: { ...(date === second.date ? second : occurrence({})), revision: "r0" } }));
  render(<MemoryRouter initialEntries={["/groups/1"]}><DateWritesProvider credential={credential}><Routes><Route path="/groups/:id" element={<GroupDetail />} /></Routes></DateWritesProvider></MemoryRouter>);
  await userEvent.click(await screen.findByRole("button", { name: "Load more dates" }));
  await screen.findByRole("button", { name: /^More for .* on 2026-12-08$/ });
  expect(Events.managedDate).not.toHaveBeenCalled();
  await userEvent.click(screen.getByRole("button", { name: /^More for .* on 2026-12-08$/ }));
  await userEvent.click(screen.getByRole("menuitem", { name: "Edit this date" }));
  await screen.findByLabelText("Title");
  expect(Events.managedDate).toHaveBeenCalledExactlyOnceWith(8, "2026-12-08", credential);
  // The date the second page names is the one saved, from its own read.
  await waitFor(() => expect(screen.getByRole("button", { name: "Save this date" })).toBeEnabled());
  await userEvent.type(screen.getByLabelText("Title"), "!");
  await userEvent.click(screen.getByRole("button", { name: "Save this date" }));
  await waitFor(() => expect(edits.sent).toHaveLength(1));
  expect(edits.sent[0]).toMatchObject({ url: "/events/8/occurrences/2026-12-08/edits", revision: "r0", changes: { title: "Supper!" } });
  expect(vi.mocked(Groups.get).mock.calls.map((call) => call.slice(1))).toContainEqual([expect.objectContaining({ cursor: "c1" })]);
});

describe("the date editor while other reads are held or fail", () => {
  const managing = { ...page, canManage: true, upcomingEvents: [occurrence({ groupId: 1 })] };

  function later<T>() {
    let resolve: (value: T) => void = () => {};
    const promise = new Promise<T>((yes) => { resolve = yes; });

    return { promise, resolve };
  }

  function renderInSession(store = createRosterWrites()) {
    render(<MemoryRouter initialEntries={["/groups/1"]}><DateWritesProvider rosterWrites={store} credential={credential}><Routes><Route path="/groups/:id" element={<GroupDetail />} /></Routes></DateWritesProvider></MemoryRouter>);

    return store;
  }

  async function openEditor() {
    await userEvent.click(await screen.findByRole("button", { name: /^More for .* on 2026-12-01$/ }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Edit this date" }));
  }

  it("a page read that fails while the date's read is held shows nothing of that read; the page's Try again reads the date afresh", async () => {
    vi.mocked(Groups.get).mockResolvedValue(managing);
    const heldRead = later<OccurrenceResponse>();
    vi.mocked(Events.managedDate).mockReturnValueOnce(heldRead.promise).mockResolvedValue(forGroup({ occurrence: occurrence({ title: "Fresh title", revision: "r1" }) }));
    const store = renderInSession();
    await openEditor();
    expect(await screen.findByText("Loading this date…")).toBeInTheDocument();
    vi.mocked(Groups.get).mockRejectedValueOnce(new ApiError(500, "SECRET"));
    // A roster write on the date ends: the page is asked again, and that read fails.
    act(() => store.begin("7:2026-12-01")("stored"));
    expect(await screen.findByText("This date can be edited again once the page loads. What you typed is kept.")).toBeInTheDocument();
    await act(async () => { heldRead.resolve(forGroup({ occurrence: occurrence({ title: "Held title", revision: "r0" }) })); await heldRead.promise; });
    expect(screen.queryByLabelText("Title")).not.toBeInTheDocument();
    expect(document.body.textContent).not.toContain("Held title");
    expect(document.body.textContent).not.toContain("SECRET");
    await userEvent.click(await screen.findByRole("button", { name: "Try again" }));
    expect(await screen.findByLabelText("Title")).toHaveValue("Fresh title");
    await waitFor(() => expect(screen.getByRole("button", { name: "Save this date" })).toBeEnabled());
    await userEvent.type(screen.getByLabelText("Title"), "!");
    await userEvent.click(screen.getByRole("button", { name: "Save this date" }));
    await waitFor(() => expect(edits.sent).toHaveLength(1));
    expect(edits.sent[0]).toMatchObject({ revision: "r1", changes: { title: "Fresh title!" } });
  });

  it("a household read that fails leaves the date's own read in charge of the editor, and puts no household detail in it", async () => {
    vi.mocked(Groups.get).mockResolvedValue(managing);
    vi.mocked(Households.mine).mockRejectedValue(new TypeError("Failed to fetch"));
    const heldRead = later<OccurrenceResponse>();
    vi.mocked(Events.managedDate).mockReturnValueOnce(heldRead.promise);
    renderInSession();
    await openEditor();
    await act(async () => { heldRead.resolve(forGroup({ occurrence: occurrence({ title: "From the date read", revision: "r7" }) })); await heldRead.promise; });
    const title = await screen.findByLabelText("Title");
    expect(title).toHaveValue("From the date read");
    const form = title.closest("form");
    expect(form?.textContent).not.toContain("Mary");
    await waitFor(() => expect(screen.getByRole("button", { name: "Save this date" })).toBeEnabled());
    await userEvent.type(title, "!");
    await userEvent.click(screen.getByRole("button", { name: "Save this date" }));
    await waitFor(() => expect(edits.sent).toHaveLength(1));
    expect(edits.sent[0].revision).toBe("r7");
  });
});

it("never lets a delayed continuation restore authority a newer answer with the same cursor removed", async () => {
  const privileged = { ...page, canManage: true, members: [leaderRow, protectedTeen], hasMore: true, truncated: true, nextCursor: "after-first" };
  const demoted = { ...page, canManage: false, members: [], hasMore: true, truncated: true, nextCursor: "after-first" };
  const replacement = deferred<GroupDetailResponse>();
  const continuation = deferred<GroupDetailResponse>();
  vi.mocked(Groups.get).mockResolvedValueOnce(privileged).mockReturnValueOnce(replacement.promise).mockReturnValueOnce(continuation.promise);
  vi.mocked(Events.register).mockResolvedValue({ registration: null, people: [] });
  open();
  await userEvent.click(await screen.findByRole("tab", { name: "Members" }));
  expect(screen.getByText("Protected Teen")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Add people" })).toBeInTheDocument();
  await userEvent.click(screen.getByRole("tab", { name: "Upcoming events" }));

  // A refresh is on its way while the old page is still shown; Load more is asked from that old page.
  await userEvent.click(screen.getByRole("checkbox", { name: "Mary" }));
  await userEvent.click(screen.getByRole("button", { name: "Save answers" }));
  await waitFor(() => expect(Groups.get).toHaveBeenCalledTimes(2));
  await userEvent.click(screen.getByRole("button", { name: "Load more dates" }));
  expect(Groups.get).toHaveBeenLastCalledWith("1", { cursor: "after-first" });

  await act(async () => replacement.resolve(demoted));
  await act(async () => continuation.resolve({ ...privileged, upcomingEvents: [managerOnlyDate], hasMore: false, truncated: false, nextCursor: "" }));

  expect(await screen.findByRole("button", { name: "Load more dates" })).toBeEnabled();
  expect(screen.queryByText(/Leaders planning/)).not.toBeInTheDocument();
  expect(screen.queryByText("Protected Teen")).not.toBeInTheDocument();
  expect(screen.getAllByRole("form", { name: "Household answers" })).toHaveLength(1);
  await userEvent.click(screen.getByRole("tab", { name: "Members" }));
  expect(screen.queryByText("Protected Teen")).not.toBeInTheDocument();
  expect(screen.queryByText("protected@example.com")).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Add people" })).not.toBeInTheDocument();
});

it("adds only a continuation's dates: the group, roster and authority stay the shown answer's", async () => {
  const first = { ...page, hasMore: true, truncated: true, nextCursor: "after-first" };
  const later = { ...page, group: { ...page.group, name: "Continuation group name" }, canManage: true, members: [leaderRow, protectedTeen], upcomingEvents: [occurrence({ id: 2, eventId: 8, date: "2026-12-08" })] };
  vi.mocked(Groups.get).mockResolvedValueOnce(first).mockResolvedValueOnce(later);
  open();
  await userEvent.click(await screen.findByRole("button", { name: "Load more dates" }));
  expect(await screen.findAllByRole("form", { name: "Household answers" })).toHaveLength(2);
  expect(screen.queryByRole("button", { name: "Load more dates" })).not.toBeInTheDocument();
  expect(screen.getByRole("heading", { name: "Family group" })).toBeInTheDocument();
  expect(screen.queryByText("Continuation group name")).not.toBeInTheDocument();
  await userEvent.click(screen.getByRole("tab", { name: "Members" }));
  expect(screen.queryByText("Protected Teen")).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Add people" })).not.toBeInTheDocument();
});

it("keeps loading a series beyond 100 dates using the latest page metadata", async () => {
  const dates = Array.from({ length: 126 }, (_, i) => occurrence({ id: i + 1, recurrenceRule: "FREQ=DAILY" }));
  vi.mocked(Groups.get)
    .mockResolvedValueOnce({ ...page, upcomingEvents: dates.slice(0, 25), truncated: true, hasMore: true, nextCursor: "after-25" })
    .mockResolvedValueOnce({ ...page, upcomingEvents: dates.slice(24, 100), truncated: true, hasMore: true, nextCursor: "after-100" })
    .mockResolvedValueOnce({ ...page, upcomingEvents: dates.slice(100) });
  open();
  await userEvent.click(await screen.findByRole("button", { name: "Load more dates" }));
  expect(await screen.findByText("Showing the next 100 dates.")).toBeVisible();
  expect(screen.getByRole("link", { name: "See more dates" })).toBeVisible();
  await userEvent.click(screen.getByRole("button", { name: "Load more dates" }));
  expect(Groups.get).toHaveBeenLastCalledWith("1", { cursor: "after-100" });
  expect(await screen.findByRole("link", { name: "See more dates" })).toBeVisible();
  expect(screen.queryByRole("button", { name: "Load more dates" })).not.toBeInTheDocument();
  expect(screen.queryByText("Showing the next 100 dates.")).not.toBeInTheDocument();
  expect(screen.getAllByRole("form", { name: "Household answers" })).toHaveLength(1);
});

it("switches group sections by arrow keys and labels the active panel", async () => {
  open();
  const events = await screen.findByRole("tab", { name: "Upcoming events" });
  events.focus();
  await userEvent.keyboard("{ArrowRight}");
  expect(screen.getByRole("tab", { name: "About" })).toHaveFocus();
  expect(screen.getByRole("tab", { name: "About" })).toHaveAttribute("aria-selected", "true");
  expect(screen.getByRole("tabpanel", { name: "About" })).toHaveTextContent("About this group");
});

it("shows the title, organizer and an inverse primary action over the cover", async () => {
  vi.mocked(Groups.get).mockResolvedValue({ ...page, canManage: true });
  open();
  expect(await screen.findByRole("heading", { name: "Family group" })).toBeInTheDocument();
  expect(screen.getByText(/Organized by Leader/)).toBeInTheDocument();
  expect(screen.getByRole("link", { name: "Schedule event" })).toHaveClass("btn-primary-inverse");
});

afterEach(() => vi.restoreAllMocks());

// jsdom has no layout, so a test says whether the paragraph overflows its clamp.
function stubOverflow(overflowing: boolean) {
  vi.spyOn(HTMLElement.prototype, "scrollHeight", "get").mockReturnValue(overflowing ? 120 : 60);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(60);
}

it("clamps the description above the tabs and Read more opens About", async () => {
  stubOverflow(true);
  vi.mocked(Groups.get).mockResolvedValue({ ...page, group: { ...page.group, description: "We meet to read and pray together." } });
  open();
  const text = await screen.findByText("We meet to read and pray together.");
  expect(text.compareDocumentPosition(screen.getByRole("tablist")) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  await userEvent.click(screen.getByRole("button", { name: "Read more" }));
  expect(screen.getByRole("tab", { name: "About" })).toHaveAttribute("aria-selected", "true");
  expect(screen.getByRole("heading", { name: "About this group" })).toBeInTheDocument();
  await waitFor(() => expect(screen.getByRole("tab", { name: "About" })).toHaveFocus());
  // The About tab shows the full text, so the clamped copy above is gone.
  expect(screen.getAllByText("We meet to read and pray together.")).toHaveLength(1);
  expect(screen.queryByRole("button", { name: "Read more" })).not.toBeInTheDocument();
});

it("shows no Read more when the description fits in the clamp", async () => {
  stubOverflow(false);
  vi.mocked(Groups.get).mockResolvedValue({ ...page, group: { ...page.group, description: "Short and sweet." } });
  open();
  expect(await screen.findByText("Short and sweet.")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Read more" })).not.toBeInTheDocument();
});

it("shows neither the clamp nor Read more when the description is empty", async () => {
  open();
  await screen.findByRole("tablist");
  expect(screen.queryByRole("button", { name: "Read more" })).not.toBeInTheDocument();
});

it("puts the calendar subscription after the date cards", async () => {
  open();
  const link = await screen.findByRole("link", { name: "Subscribe in your calendar" });
  const form = screen.getByRole("form", { name: "Household answers" });
  expect(form.compareDocumentPosition(link) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
});

it("invites a signed-out visitor to log in to join, and sends them to log in", async () => {
  actor.user = null;
  vi.mocked(Groups.get).mockResolvedValue({ ...page, isMember: false });
  render(<MemoryRouter initialEntries={["/groups/1"]}><Routes>
    <Route path="/groups/:id" element={<GroupDetail />} />
    <Route path="/login" element={<h1>Login page</h1>} />
  </Routes></MemoryRouter>);
  expect(screen.queryByRole("button", { name: "Join this group" })).not.toBeInTheDocument();
  await userEvent.click(await screen.findByRole("button", { name: "Log in to join" }));
  expect(await screen.findByRole("heading", { name: "Login page" })).toBeInTheDocument();
});

function Where() {
  const l = useLocation();

  return <p>At {l.pathname}{l.search}</p>;
}

it("adds Manage sign-ups to each date's More menu for leaders, opening that date on the event page", async () => {
  vi.mocked(Groups.get).mockResolvedValue({ ...page, canManage: true });
  render(<MemoryRouter initialEntries={["/groups/1"]}><Routes>
    <Route path="/groups/:id" element={<GroupDetail />} />
    <Route path="/events/:id" element={<Where />} />
  </Routes></MemoryRouter>);
  await userEvent.click(await screen.findByRole("button", { name: "More for Supper on 2026-12-01" }));
  expect(screen.getAllByRole("menuitem").map((i) => i.textContent)).toEqual(["Edit this date", "Manage sign-ups", "Edit event", "Cancel this date"]);
  await userEvent.click(screen.getByRole("menuitem", { name: "Manage sign-ups" }));
  expect(await screen.findByText("At /events/7?manage=2026-12-01")).toBeInTheDocument();
});

it("finishes the series editor after a save: locked as sent, Saved, Close focused, the visibility choice kept, leaving asks nothing", async () => {
  vi.mocked(Groups.get).mockResolvedValue({ ...page, canManage: true });
  vi.mocked(Events.update).mockResolvedValue({ event: testEvent(7) });
  const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);

  const router = createMemoryRouter([
    { path: "/groups/:id", element: <><UnsavedChangesBlocker /><Link to="/elsewhere">Elsewhere</Link><GroupDetail /></> },
    { path: "/elsewhere", element: <p>Elsewhere page</p> },
  ], { initialEntries: ["/groups/1"] });

  render(<RouterProvider router={router} />);
  await userEvent.click(await screen.findByRole("button", { name: "More for Supper on 2026-12-01" }));
  await userEvent.click(screen.getByRole("menuitem", { name: "Edit event" }));
  const title = await screen.findByRole("textbox", { name: "Title" });
  await userEvent.click(screen.getByRole("radio", { name: /^Invite only/ }));
  await userEvent.type(title, " night");
  screen.getByRole("button", { name: "Save series" }).focus();
  await userEvent.keyboard("{Enter}");
  await waitFor(() => expect(screen.getByText("Saved")).toBeInTheDocument());
  expect(screen.getByText("Saved")).toHaveAttribute("role", "status");
  expect(Events.update).toHaveBeenLastCalledWith(7, expect.objectContaining({ title: "Supper night" }));
  await waitFor(() => expect(Groups.get).toHaveBeenCalledTimes(2));
  expect(title).toBeDisabled();
  expect(title).toHaveValue("Supper night");
  expect(screen.getByLabelText(/^Seat limit/)).toBeDisabled();
  expect(screen.queryByRole("button", { name: "Save series" })).not.toBeInTheDocument();
  const close = screen.getByRole("button", { name: "Close" });
  expect(close).toBeEnabled();
  expect(document.activeElement).toBe(close);
  expect(screen.getByRole("radio", { name: /^Invite only/ })).toBeChecked();
  expect(screen.getByRole("radio", { name: /^Invite only/ })).toBeEnabled();
  fireEvent.submit(title.closest("form")!);
  expect(Events.update).toHaveBeenCalledOnce();

  const leaving = new Event("beforeunload", { cancelable: true });
  window.dispatchEvent(leaving);
  expect(leaving.defaultPrevented).toBe(false);
  await userEvent.click(screen.getByRole("link", { name: "Elsewhere" }));
  expect(await screen.findByText("Elsewhere page")).toBeInTheDocument();
  expect(confirm).not.toHaveBeenCalled();
  confirm.mockRestore();
});

it("tracks focus for an event row only while its series panel is open", async () => {
  const rows = [1, 2, 3, 4].map((n) => occurrence({ id: n, eventId: 6 + n, date: `2026-12-0${n}`, title: `Night ${n}` }));
  vi.mocked(Groups.get).mockResolvedValue({ ...page, canManage: true, upcomingEvents: rows });
  const added = vi.spyOn(document, "addEventListener");
  const focusins = () => added.mock.calls.filter(([type]) => type === "focusin").length;
  open();
  await screen.findByRole("button", { name: "More for Night 4 on 2026-12-04" });
  const withRows = focusins();
  cleanup();
  added.mockClear();
  vi.mocked(Groups.get).mockResolvedValue({ ...page, canManage: true, upcomingEvents: rows.slice(0, 1) });
  open();
  await screen.findByRole("button", { name: "More for Night 1 on 2026-12-01" });
  expect(withRows).toBe(focusins());
  added.mockRestore();
});

it("opens the series editor again from the saved values, and its save sends the next change", async () => {
  vi.mocked(Groups.get).mockResolvedValue({ ...page, canManage: true });
  vi.mocked(Events.update).mockResolvedValue({ event: testEvent(7) });
  open();
  await userEvent.click(await screen.findByRole("button", { name: "More for Supper on 2026-12-01" }));
  await userEvent.click(screen.getByRole("menuitem", { name: "Edit event" }));
  await userEvent.type(screen.getByLabelText(/^Seat limit/), "12");
  vi.mocked(Groups.get).mockResolvedValue({ ...page, canManage: true, upcomingEvents: [occurrence({ capacity: 12 })] });
  await userEvent.click(screen.getByRole("button", { name: "Save series" }));
  expect(Events.update).toHaveBeenLastCalledWith(7, expect.objectContaining({ capacity: 12 }));
  await waitFor(() => expect(Groups.get).toHaveBeenCalledTimes(2));
  await userEvent.click(await screen.findByRole("button", { name: "Close" }));
  expect(document.activeElement).toBe(screen.getByRole("button", { name: "More for Supper on 2026-12-01" }));
  await userEvent.click(screen.getByRole("button", { name: "More for Supper on 2026-12-01" }));
  await userEvent.click(screen.getByRole("menuitem", { name: "Edit event" }));
  const seats = await screen.findByLabelText(/^Seat limit/);
  expect(seats).toHaveValue(12);
  expect(screen.queryByText("Saved")).not.toBeInTheDocument();
  await userEvent.clear(seats);
  await userEvent.click(screen.getByRole("button", { name: "Save series" }));
  await waitFor(() => expect(Events.update).toHaveBeenCalledTimes(2));
  expect(Events.update).toHaveBeenLastCalledWith(7, expect.objectContaining({ capacity: null }));
});

describe("the series panel while a save is on its way", () => {
  const unchanged = (visibility: Visibility): VisibilityImpact => ({ visibility, currentVisibility: "public", tightened: [], leaderOpened: [], consent: "c", futureOverrides: 0 });

  async function openSeries() {
    vi.mocked(Groups.get).mockResolvedValue({ ...page, canManage: true });
    open();
    await userEvent.click(await screen.findByRole("button", { name: "More for Supper on 2026-12-01" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Edit event" }));

    return screen.findByRole("textbox", { name: "Title" });
  }

  it("locks the editor and the visibility control, keeps focus in the editor, and gives it to Close after", async () => {
    const put = deferred<{ event: ReturnType<typeof testEvent> }>();
    vi.mocked(Events.update).mockReturnValueOnce(put.promise);
    const title = await openSeries();
    await userEvent.type(title, " night");
    const save = screen.getByRole("button", { name: "Save series" });
    save.focus();
    await userEvent.keyboard("{Enter}");
    expect(Events.update).toHaveBeenCalledWith(7, expect.objectContaining({ title: "Supper night" }));
    expect(document.activeElement).toBe(title.closest("form"));
    expect(title).toBeDisabled();
    expect(screen.getByRole("radio", { name: /^Private/ })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Save visibility" })).toBeDisabled();
    await userEvent.type(title, " and songs");
    expect(title).toHaveValue("Supper night");
    await act(async () => { put.resolve({ event: testEvent(7) }); await put.promise; });
    const close = screen.getByRole("button", { name: "Close" });
    expect(document.activeElement).toBe(close);
    await waitFor(() => expect(Groups.get).toHaveBeenCalledTimes(2));
    expect(Events.update).toHaveBeenCalledOnce();
    expect(document.activeElement).toBe(close);
    expect(screen.getByRole("button", { name: "Save visibility" })).toBeEnabled();
  });

  it("leaves focus where the person moved it while the save was on its way", async () => {
    vi.mocked(Groups.get).mockResolvedValue({ ...page, canManage: true, upcomingEvents: [occurrence({}), occurrence({ id: 2, eventId: 8, date: "2026-12-08", title: "Plain night" })] });
    const put = deferred<{ event: ReturnType<typeof testEvent> }>();
    vi.mocked(Events.update).mockReturnValueOnce(put.promise);
    open();
    await userEvent.click(await screen.findByRole("button", { name: "More for Supper on 2026-12-01" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Edit event" }));
    screen.getByRole("button", { name: "Save series" }).focus();
    await userEvent.keyboard("{Enter}");
    const elsewhere = screen.getByRole("button", { name: "More for Plain night on 2026-12-08" });
    elsewhere.focus();
    await act(async () => { put.resolve({ event: testEvent(7) }); await put.promise; });
    await waitFor(() => expect(Groups.get).toHaveBeenCalledTimes(2));
    expect(document.activeElement).toBe(elsewhere);
  });

  it("gives focus to the event's new row when the reload replaces the saved one", async () => {
    vi.mocked(Events.update).mockResolvedValue({ event: testEvent(7) });
    await openSeries();
    const moved = occurrence({ id: 2, date: "2026-12-08", startsAt: "2026-12-08T19:00:00Z", endsAt: "2026-12-08T20:00:00Z", startsLocal: "2026-12-08T19:00" });
    vi.mocked(Groups.get).mockResolvedValue({ ...page, canManage: true, upcomingEvents: [moved] });
    screen.getByRole("button", { name: "Save series" }).focus();
    await userEvent.keyboard("{Enter}");
    const next = await screen.findByRole("button", { name: "More for Supper on 2026-12-08" });
    await waitFor(() => expect(document.activeElement).toBe(next));
  });

  it("gives focus to the event's new row when the save's own reload replaces the row after another change to the page", async () => {
    const other = occurrence({ id: 2, eventId: 8, date: "2026-12-08", title: "Plain night" });
    vi.mocked(Groups.get).mockResolvedValue({ ...page, canManage: true, upcomingEvents: [occurrence({}), other] });
    const cancelled = deferred<void>();
    vi.mocked(Events.cancelOccurrence).mockReturnValueOnce(cancelled.promise);
    vi.mocked(Events.update).mockResolvedValue({ event: testEvent(7) });
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(true);
    open();
    await userEvent.click(await screen.findByRole("button", { name: "More for Plain night on 2026-12-08" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Cancel this date" }));
    await userEvent.click(screen.getByRole("button", { name: "More for Supper on 2026-12-01" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Edit event" }));
    const reload = deferred<GroupDetailResponse>();
    vi.mocked(Groups.get).mockReturnValueOnce(reload.promise);
    screen.getByRole("button", { name: "Save series" }).focus();
    await userEvent.keyboard("{Enter}");
    await waitFor(() => expect(Groups.get).toHaveBeenCalledTimes(2));
    await act(async () => { cancelled.resolve(); await cancelled.promise; });
    expect(screen.queryByRole("button", { name: "More for Plain night on 2026-12-08" })).not.toBeInTheDocument();
    const moved = occurrence({ id: 3, date: "2026-12-15", startsAt: "2026-12-15T19:00:00Z", endsAt: "2026-12-15T20:00:00Z", startsLocal: "2026-12-15T19:00" });
    await act(async () => { reload.resolve({ ...page, canManage: true, upcomingEvents: [moved] }); await reload.promise; });
    const next = await screen.findByRole("button", { name: "More for Supper on 2026-12-15" });
    expect(document.activeElement).toBe(next);
    confirm.mockRestore();
  });

  it("keeps focus in the panel when a saved visibility replaces its control", async () => {
    vi.mocked(Events.visibilityImpact).mockImplementation((_, visibility) => Promise.resolve(unchanged(visibility)));
    vi.mocked(Events.setVisibility).mockResolvedValue({ event: testEvent(7) });
    await openSeries();
    await userEvent.click(screen.getByRole("radio", { name: /^Private/ }));
    vi.mocked(Groups.get).mockResolvedValue({ ...page, canManage: true, upcomingEvents: [occurrence({ visibility: "private", seriesVisibility: "private" })] });
    const save = screen.getByRole("button", { name: "Save visibility" });
    save.focus();
    await userEvent.keyboard("{Enter}");
    await waitFor(() => expect(Groups.get).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(screen.getByRole("button", { name: "Save visibility" })).not.toBe(save));
    await waitFor(() => expect(document.activeElement).toBe(screen.getByRole("textbox", { name: "Title" }).closest("form")!.parentElement));
  });

  it("leaves focus where the person moved it when a saved visibility replaces its control", async () => {
    vi.mocked(Events.visibilityImpact).mockImplementation((_, visibility) => Promise.resolve(unchanged(visibility)));
    vi.mocked(Events.setVisibility).mockResolvedValue({ event: testEvent(7) });
    await openSeries();
    await userEvent.click(screen.getByRole("radio", { name: /^Private/ }));
    const reload = deferred<GroupDetailResponse>();
    vi.mocked(Groups.get).mockReturnValueOnce(reload.promise);
    const save = screen.getByRole("button", { name: "Save visibility" });
    await userEvent.click(save);
    await waitFor(() => expect(Groups.get).toHaveBeenCalledTimes(2));
    const elsewhere = screen.getByRole("button", { name: "More for Supper on 2026-12-01" });
    elsewhere.focus();
    await act(async () => { reload.resolve({ ...page, canManage: true, upcomingEvents: [occurrence({ visibility: "private", seriesVisibility: "private" })] }); await reload.promise; });
    await waitFor(() => expect(screen.getByRole("button", { name: "Save visibility" })).not.toBe(save));
    expect(document.activeElement).toBe(elsewhere);
  });

  it("locks the editor while the visibility control's save is on its way", async () => {
    const preview = deferred<VisibilityImpact>();
    vi.mocked(Events.visibilityImpact).mockImplementation((_, visibility) => (visibility === "private" ? preview.promise : Promise.resolve(unchanged(visibility))));
    const title = await openSeries();
    await userEvent.click(screen.getByRole("radio", { name: /^Private/ }));
    await userEvent.click(screen.getByRole("button", { name: "Save visibility" }));
    expect(title).toBeDisabled();
    expect(screen.getByRole("button", { name: "Save series" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Close" })).toBeDisabled();
    await userEvent.click(screen.getByRole("button", { name: "More for Supper on 2026-12-01" }));
    expect(screen.getByRole("menuitem", { name: "Edit event" })).toBeDisabled();
    await userEvent.keyboard("{Escape}");
    await act(async () => { preview.reject(new Error("offline")); await preview.promise.catch(() => {}); });
    expect(title).toBeEnabled();
  });
});

it("keeps the group form, its Cancel off, until a save on its way settles; its failure shows there and not in the next edit", async () => {
  vi.mocked(Groups.get).mockResolvedValue({ ...page, canManage: true });
  const saving = deferred<{ group: typeof page.group }>();
  vi.mocked(Groups.update).mockReturnValueOnce(saving.promise);
  open();
  await userEvent.click(await screen.findByRole("button", { name: "More group actions" }));
  await userEvent.click(screen.getByRole("menuitem", { name: "Edit group" }));
  await userEvent.click(await screen.findByRole("button", { name: "Save group" }));
  const form = screen.getByLabelText("Group name").closest("form")!;
  const cancel = within(form).getByRole("button", { name: "Cancel" });
  expect(cancel).toBeDisabled();
  fireEvent.click(cancel);
  expect(screen.getByLabelText("Group name")).toBeInTheDocument();
  await act(async () => { saving.reject(new ApiError(500, "private", { code: "server_error" })); await saving.promise.catch(() => {}); });
  expect(await within(form).findByRole("alert")).toHaveTextContent("We couldn't save your changes.");
  await userEvent.click(within(form).getByRole("button", { name: "Cancel" }));
  await userEvent.click(await screen.findByRole("button", { name: "More group actions" }));
  await userEvent.click(screen.getByRole("menuitem", { name: "Edit group" }));
  expect(await screen.findByLabelText("Group name")).toHaveValue("Family group");
  expect(screen.queryByRole("alert")).not.toBeInTheDocument();
});

it("leaves focus where the person moved it while Save group was on its way", async () => {
  vi.mocked(Groups.get).mockResolvedValue({ ...page, canManage: true });
  const saving = deferred<{ group: typeof page.group }>();
  vi.mocked(Groups.update).mockReturnValueOnce(saving.promise);
  render(<main id="main" tabIndex={-1}><button>Elsewhere</button><MemoryRouter initialEntries={["/groups/1"]}><Routes><Route path="/groups/:id" element={<GroupDetail />} /></Routes></MemoryRouter></main>);
  await userEvent.click(await screen.findByRole("button", { name: "More group actions" }));
  await userEvent.click(screen.getByRole("menuitem", { name: "Edit group" }));
  await userEvent.click(await screen.findByRole("button", { name: "Save group" }));
  const elsewhere = screen.getByRole("button", { name: "Elsewhere" });
  elsewhere.focus();
  await act(async () => { saving.resolve({ group: page.group }); await saving.promise; });
  await waitFor(() => expect(screen.queryByLabelText("Group name")).not.toBeInTheDocument());
  await waitFor(() => expect(Groups.get).toHaveBeenCalledTimes(2));
  expect(await screen.findByRole("button", { name: "More group actions" })).toBeInTheDocument();
  expect(document.activeElement).toBe(elsewhere);
});

it("keeps focus off the page body while Save group reads the page again, then gives it to the group's menu", async () => {
  vi.mocked(Groups.get).mockResolvedValue({ ...page, canManage: true });
  vi.mocked(Groups.update).mockResolvedValue({ group: page.group });
  render(<main id="main" tabIndex={-1}><MemoryRouter initialEntries={["/groups/1"]}><Routes><Route path="/groups/:id" element={<GroupDetail />} /></Routes></MemoryRouter></main>);
  await userEvent.click(await screen.findByRole("button", { name: "More group actions" }));
  await userEvent.click(screen.getByRole("menuitem", { name: "Edit group" }));
  const reload = deferred<GroupDetailResponse>();
  vi.mocked(Groups.get).mockReturnValueOnce(reload.promise);
  await userEvent.click(await screen.findByRole("button", { name: "Save group" }));
  await waitFor(() => expect(Groups.get).toHaveBeenCalledTimes(2));
  expect(document.activeElement).toBe(document.getElementById("main"));
  await act(async () => { reload.resolve({ ...page, canManage: true }); await reload.promise; });
  expect(document.activeElement).toBe(await screen.findByRole("button", { name: "More group actions" }));
});

it("gives focus to the group's menu when Save group's reload removes the control the person moved to", async () => {
  vi.mocked(Groups.get).mockResolvedValue({ ...page, canManage: true });
  const saving = deferred<{ group: typeof page.group }>();
  vi.mocked(Groups.update).mockReturnValueOnce(saving.promise);
  render(<main id="main" tabIndex={-1}><MemoryRouter initialEntries={["/groups/1"]}><Routes><Route path="/groups/:id" element={<GroupDetail />} /></Routes></MemoryRouter></main>);
  await userEvent.click(await screen.findByRole("button", { name: "More group actions" }));
  await userEvent.click(screen.getByRole("menuitem", { name: "Edit group" }));
  await userEvent.click(await screen.findByRole("button", { name: "Save group" }));
  screen.getByRole("button", { name: "More group actions" }).focus();
  await act(async () => { saving.resolve({ group: page.group }); await saving.promise; });
  await waitFor(() => expect(Groups.get).toHaveBeenCalledTimes(2));
  const menu = await screen.findByRole("button", { name: "More group actions" });
  await waitFor(() => expect(document.activeElement).toBe(menu));
});

it("keeps Manage sign-ups away from people who can't manage the group", async () => {
  open();
  await screen.findByRole("tablist");
  expect(screen.queryByRole("button", { name: /^More for Supper/ })).not.toBeInTheDocument();
});

it("replaces the three add forms with one Add people button on the Members tab", async () => {
  actor.user = testUser({ id: 2, status: "active", accountType: "adult" });
  vi.mocked(Groups.get).mockResolvedValue({ ...page, canManage: true });
  open(); await userEvent.click(await screen.findByRole("tab", { name: "Members" }));
  expect(screen.getAllByRole("button", { name: "Add people" })).toHaveLength(1);

  for (const old of ["Invite someone to Fold", "Add a member by email", "Find a person by name"]) expect(screen.queryByLabelText(old)).not.toBeInTheDocument();
  await userEvent.click(screen.getByRole("button", { name: "Add people" }));
  expect(screen.getByRole("combobox", { name: "Find a person by name" })).toBeInTheDocument();
});

it("shows no Add people to someone who can't manage the group", async () => {
  open(); await userEvent.click(await screen.findByRole("tab", { name: "Members" }));
  expect(screen.queryByRole("button", { name: "Add people" })).not.toBeInTheDocument();
});

async function openEditor() {
  await userEvent.click(await screen.findByRole("button", { name: "More group actions" }));
  await userEvent.click(screen.getByRole("menuitem", { name: "Edit group" }));
}

it("opens More options when editing, with tags, member limit and cover inside", async () => {
  vi.mocked(Groups.get).mockResolvedValue({ ...page, canManage: true, group: { ...page.group, tags: ["young-adults"], memberLimit: 12 } });
  open(); await openEditor();
  const more = screen.getByRole("button", { name: "More options" });
  expect(more).toHaveAttribute("aria-expanded", "true");
  expect(screen.getByLabelText(/Tags/)).toBeVisible();
  expect(screen.getByLabelText(/Member limit/)).toHaveValue(12);
  expect(screen.getByText("Cover image")).toBeVisible();
  await userEvent.click(more);
  expect(more).toHaveAttribute("aria-expanded", "false");
  expect(screen.getByLabelText(/Tags/)).not.toBeVisible();
  expect(screen.getByLabelText("Group name")).toBeVisible();
});

it("asks before leaving an edited group form, and not once saved", async () => {
  vi.mocked(Groups.get).mockResolvedValue({ ...page, canManage: true });
  vi.mocked(Groups.update).mockResolvedValue({ group: page.group });
  const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);

  const router = createMemoryRouter([
    { path: "/groups/:id", element: <><UnsavedChangesBlocker /><Link to="/elsewhere">Elsewhere</Link><GroupDetail /></> },
    { path: "/elsewhere", element: <p>Elsewhere page</p> },
  ], { initialEntries: ["/groups/1"] });

  render(<RouterProvider router={router} />);
  await userEvent.click(await screen.findByRole("button", { name: "More group actions" }));
  await userEvent.click(screen.getByRole("menuitem", { name: "Edit group" }));
  await userEvent.click(screen.getByRole("link", { name: "Elsewhere" }));
  expect(confirm).not.toHaveBeenCalled(); // nothing edited yet
  await router.navigate("/groups/1");
  await userEvent.click(await screen.findByRole("button", { name: "More group actions" }));
  await userEvent.click(screen.getByRole("menuitem", { name: "Edit group" }));
  await userEvent.type(screen.getByLabelText("Group name"), " Two");
  await userEvent.click(screen.getByRole("link", { name: "Elsewhere" }));
  expect(confirm).toHaveBeenCalledWith("Leave without saving? Your changes will be lost.");
  expect(screen.queryByText("Elsewhere page")).not.toBeInTheDocument();
  await userEvent.click(screen.getByRole("button", { name: "Save group" }));
  await waitFor(() => expect(Groups.update).toHaveBeenCalled());
  confirm.mockClear();
  await userEvent.click(await screen.findByRole("link", { name: "Elsewhere" }));
  expect(screen.getByText("Elsewhere page")).toBeInTheDocument();
  expect(confirm).not.toHaveBeenCalled();
});

it("shows a booked room as the place on a date card, and the typed location without one (M2.1b)", async () => {
  vi.mocked(Groups.get).mockResolvedValue({ ...page, upcomingEvents: [
    occurrence({ room: { id: 1, name: "Fellowship Hall", building: "Main" } }),
    occurrence({ id: 2, eventId: 8, date: "2026-12-08", title: "Plain night" }),
  ] });
  open();
  const cards = await screen.findAllByRole("article");
  expect(within(cards[0]).getByText(/ · Fellowship Hall · Main · Hall$/)).toBeInTheDocument();
  expect(within(cards[1]).getByText(/ · Hall$/)).not.toHaveTextContent("Fellowship");
});

it.each<[GroupSummary["status"], string]>([
  ["pending", "You can schedule events once an admin approves this group."],
  ["archived", "This group is archived, so it can't schedule new events."],
])("offers a managing viewer of a %s group no Schedule event and says why (DEC-147)", async (status, sentence) => {
  vi.mocked(Groups.get).mockResolvedValue({ ...page, canManage: true, upcomingEvents: [], group: { ...page.group, status } });
  open();
  expect(await screen.findByText(sentence)).toBeVisible();
  expect(screen.getByText("No upcoming events")).toBeVisible();
  expect(screen.queryByRole("link", { name: "Schedule event" })).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: "More group actions" })).toBeInTheDocument();
});

it.each<[GroupSummary["status"], string]>([
  ["pending", "You can schedule events once an admin approves this group."],
  ["archived", "This group is archived, so it can't schedule new events."],
])("explains scheduling for a %s group with upcoming events", async (status, sentence) => {
  vi.mocked(Groups.get).mockResolvedValue({ ...page, canManage: true, group: { ...page.group, status } });
  open();
  expect(await screen.findByText(sentence)).toBeVisible();
  expect(screen.queryByRole("link", { name: "Schedule event" })).not.toBeInTheDocument();
});

it("offers Schedule event in the cover and the empty state of an active group", async () => {
  vi.mocked(Groups.get).mockResolvedValue({ ...page, canManage: true, upcomingEvents: [] });
  open();
  expect(await screen.findByText("Schedule your group's first meeting")).toBeVisible();
  expect(screen.getAllByRole("link", { name: "Schedule event" })).toHaveLength(2);
});

it("offers a leader who can compose Email members and Email people going (M3.4c)", async () => {
  actor.canCompose = true;

  try {
    vi.mocked(Groups.get).mockResolvedValue({ ...page, canManage: true });
    const router = createMemoryRouter([{ path: "/groups/:id", element: <GroupDetail /> }, { path: "/messages/new", element: <p>Composer</p> }], { initialEntries: ["/groups/1"] });
    render(<RouterProvider router={router} />);
    await userEvent.click(await screen.findByRole("button", { name: "More group actions" }));
    expect(screen.getByRole("menuitem", { name: "Email members" })).toBeInTheDocument();
    await userEvent.keyboard("{Escape}");
    await userEvent.click(screen.getByRole("button", { name: /^More for Supper on / }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Email people going" }));
    expect(router.state.location.pathname + router.state.location.search).toBe("/messages/new?occurrence=1");
  } finally {
    actor.canCompose = false;
  }
});

it("offers no Email members when the leader can't compose or the group isn't approved", async () => {
  vi.mocked(Groups.get).mockResolvedValue({ ...page, canManage: true });
  open();
  await userEvent.click(await screen.findByRole("button", { name: "More group actions" }));
  expect(screen.queryByRole("menuitem", { name: "Email members" })).not.toBeInTheDocument();
  cleanup();
  actor.canCompose = true;

  try {
    vi.mocked(Groups.get).mockResolvedValue({ ...page, canManage: true, group: { ...page.group, status: "pending" } });
    open();
    await userEvent.click(await screen.findByRole("button", { name: "More group actions" }));
    expect(screen.queryByRole("menuitem", { name: "Email members" })).not.toBeInTheDocument();
  } finally {
    actor.canCompose = false;
  }
});

// W6b: the group page has no dialog provider of its own, so the panel's questions are its own dialog.
describe("GroupDetail and the kept-dates panel (DEC-156)", () => {
  beforeEach(() => {
    vi.mocked(Groups.get).mockResolvedValue({ ...page, canManage: true, upcomingEvents: [occurrence({ attendeesVisible: true })] });
    vi.mocked(Events.managedDate).mockImplementation((_id, date, pinned) => Promise.resolve(pinned ? forGroup(keptRead(date)) : keptRead(date)));
    vi.mocked(Events.removeRegistration).mockResolvedValue({});
    vi.spyOn(Site, "get").mockResolvedValue({ name: "Fold", timezone: "UTC", selfSignup: "off", roomsEnabled: false, pushEnabled: true });
    vi.spyOn(window, "confirm").mockImplementation(() => { throw new Error("native confirm on the group page"); });
  });

  afterEach(() => { vi.useRealTimers(); });

  async function openAndSave() {
    render(<MemoryRouter initialEntries={["/groups/1"]}><DateWritesProvider credential={credential}><Routes><Route path="/groups/:id" element={<GroupDetail />} /></Routes></DateWritesProvider></MemoryRouter>);
    await userEvent.click(await screen.findByRole("button", { name: /^More for .* on 2026-12-01$/ }));
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
        await runHostCase(event, outcome, { openAndSave, pageLoads: () => vi.mocked(Groups.get).mock.calls.length, edits, base: () => forGroup(keptRead("2026-12-01")), remounts: true });
        // A recheck leaves the page alone; a Remove that ended stored or lost asks for it again, once.
        expect(Groups.get).toHaveBeenCalledTimes(event === "iv" || event.startsWith("lost-") ? 2 : 1);
        expect(window.confirm).not.toHaveBeenCalled();
      });
    }
  }

  it("after a 409, a Remove in the panel's own dialog and the retry, the editor keeps its draft and the page is asked again once", async () => {
    edits.answer(kept409([{ date: "2026-12-01", keptCount: 1 }]));
    await openAndSave();
    await screen.findByText("Old Name");
    // After the Remove, the panel's reads (its own retry, and the one its remount makes once the page answers) find nobody kept.
    vi.mocked(Events.managedDate).mockImplementation((_id, date, pinned) => Promise.resolve(pinned ? forGroup(keptRead(date)) : keptRead(date, { keptCount: 0, attendees: [] })));
    await userEvent.click(screen.getByRole("button", { name: "More for Old Name" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Remove Old Name" }));
    await userEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Remove" }));
    await waitFor(() => expect(Groups.get).toHaveBeenCalledTimes(2));
    expect(await screen.findByText("Nobody is keeping this date now. Save again to apply your change.")).toBeInTheDocument();
    expect(screen.getByRole("spinbutton", { name: "Minutes" })).toHaveValue(150);
    expect(Groups.get).toHaveBeenCalledTimes(2);
    expect(window.confirm).not.toHaveBeenCalled();
  });
});

// The group page asks the date's consent question in the app's dialog, the editor's own (every other question
// on the page stays the browser's). A roster write on the date (Add, restore or Remove, from the date editor's
// kept-dates panel or the series editor's; the group page has no date row roster) holds the editor's Save from
// its begin to its end (DEC-158, VC2); one that begins while the count is read, or while the question is up,
// stops the save before anything is sent and closes the question. A write that may have stored
// something asks for the page again, and the next Save counts afresh; an answered one turns Save back on at once.
describe("GroupDetail: a roster write on the date holds its editor's Save", () => {
  const STALE = "This date changed while you were saving. Check it and save again.";
  const FRESH = "ffffffffffffffffffffffffffffffff";
  // The writer's question for a tightening of the date that cancels `registrants` people (DEC-158).
  const challenge = (registrants: number, consent = "c".repeat(32)) => new ApiError(409, "x", { code: "date_needs_consent", impact: { visibility: "private", currentVisibility: "public", tightened: [{ id: 7, title: "Supper", visibility: "public", registrants, occurrenceId: 1 }], leaderOpened: [], consent } });
  const roster = { removedRegistrations: [removedRow()] };
  const groupPage = { ...page, canManage: true, upcomingEvents: [occurrence({ attendeesVisible: true })] };

  beforeEach(() => {
    vi.mocked(Groups.get).mockResolvedValue(groupPage);
    vi.mocked(Events.managedDate).mockImplementation((_id, date, pinned) => Promise.resolve(pinned ? forGroup(keptRead(date, roster)) : keptRead(date, roster)));
    vi.spyOn(Site, "get").mockResolvedValue({ name: "Fold", timezone: "UTC", selfSignup: "off", roomsEnabled: false, pushEnabled: true });
    vi.spyOn(window, "confirm").mockImplementation(() => { throw new Error("the prompt was asked"); });
  });

  const minutes = () => screen.getByRole("spinbutton", { name: "Minutes" });
  const save = () => screen.getByRole("button", { name: "Save this date" });

  const panelRoster = () => {
    const section = screen.getByText("Old Name", { selector: "p, span, div, strong" }).closest("section[aria-label='Manage sign-ups']");

    if (!(section instanceof HTMLElement)) throw new Error("no panel roster");

    return section;
  };

  function renderInSession(store?: RosterWrites) {
    render(<MemoryRouter initialEntries={["/groups/1"]}><DateWritesProvider rosterWrites={store} credential={credential}><Routes><Route path="/groups/:id" element={<GroupDetail />} /></Routes></DateWritesProvider></MemoryRouter>);
  }

  async function openDateEditor() {
    await userEvent.click(await screen.findByRole("button", { name: /^More for .* on 2026-12-01$/ }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Edit this date" }));
    await userEvent.clear(await screen.findByRole("spinbutton", { name: "Minutes" }));
    await userEvent.type(minutes(), "150");
  }

  // The source's kept-dates panel is open (after its 409) beside a date editor drafting Minutes 150 and Private.
  async function editorBeside(source: "panel" | "series panel") {
    if (source === "series panel") {
      await userEvent.click(await screen.findByRole("button", { name: /^More for .* on 2026-12-01$/ }));
      await userEvent.click(screen.getByRole("menuitem", { name: "Edit event" }));
      await userEvent.clear(requiredTitle());
      await userEvent.type(requiredTitle(), "Series draft");
      vi.mocked(Events.update).mockRejectedValueOnce(kept409([{ date: "2026-12-01", keptCount: 1 }]));
      await userEvent.click(screen.getByRole("button", { name: "Save series" }));
      await screen.findByText("Old Name");

      return;
    }

    await openDateEditor();
    edits.answer(kept409([{ date: "2026-12-01", keptCount: 1 }]));
    await waitFor(() => expect(save()).toBeEnabled());
    await userEvent.click(save());
    await screen.findByText("Old Name");
    await userEvent.click(screen.getByRole("radio", { name: /^Private/ }));
  }

  for (const op of ROSTER_OPS) {
    for (const end of endsFor(op)) {
      const answered = end === "answered" || end === "event_full";

      it(`panel: a held ${op} disables Save and sends nothing; ending ${end} ${answered ? "turns Save on with no reread" : "rereads, then one save asks the writer's fresh question"}`, async () => {
        renderInSession();
        await editorBeside("panel");
        const puts = edits.sent.length;
        const write = await startHeld(op, panelRoster, "Old Name");
        const loads = vi.mocked(Groups.get).mock.calls.length;
        expect(save()).toBeDisabled();
        await userEvent.click(save());
        expect(edits.sent).toHaveLength(puts);
        expect(minutes()).toBeEnabled();
        expect(minutes()).toHaveValue(150);

        const reread = deferred<GroupDetailResponse>();

        if (!answered) vi.mocked(Groups.get).mockReturnValueOnce(reread.promise);
        await act(async () => { ROSTER_ENDS[end](write); await write.promise.catch(() => {}); });

        if (answered) {
          expect(save()).toBeEnabled();
          expect(Groups.get).toHaveBeenCalledTimes(loads);
        } else {
          expect(Groups.get).toHaveBeenCalledTimes(loads + 1);
          expect(save()).toBeDisabled();
          await act(async () => { reread.resolve(groupPage); await reread.promise; });
          // The page's answer reads the date again for the editor, which comes back with the draft.
          await waitFor(() => expect(save()).toBeEnabled());
          expect(Groups.get).toHaveBeenCalledTimes(loads + 1);
        }

        expect(minutes()).toHaveValue(150);
        expect(screen.getByRole("radio", { name: /^Private/ })).toBeChecked();
        edits.answer(challenge(3, FRESH));
        await userEvent.click(save());
        const asked = await screen.findByRole("dialog", { name: "Save this date?" });
        expect(asked).toHaveAccessibleDescription(expect.stringContaining("notifies 3 registered people"));
        await userEvent.click(within(asked).getByRole("button", { name: "Save and notify" }));
        await waitFor(() => expect(edits.sent).toHaveLength(puts + 2));
        expect(window.confirm).not.toHaveBeenCalled();
        expect(edits.sent[puts + 1]).toMatchObject({ attempt: edits.sent[puts].attempt, changes: { durationMinutes: 150, visibility: "private" }, consent: FRESH });
      });
    }

    it(`panel: a ${op} that begins while the save is on its way stops it before the question`, async () => {
      renderInSession();
      await editorBeside("panel");
      const puts = edits.sent.length;
      const sent = deferred<OccurrenceResponse>();
      edits.answer(sent.promise);
      await userEvent.click(save());
      await waitFor(() => expect(edits.sent).toHaveLength(puts + 1));
      const write = await startHeld(op, panelRoster, "Old Name");
      await act(async () => { sent.reject(challenge(2)); await sent.promise.catch(() => {}); });
      expect(screen.queryByRole("dialog", { name: "Save this date?" })).not.toBeInTheDocument();
      expect(window.confirm).not.toHaveBeenCalled();
      expect(edits.sent).toHaveLength(puts + 1);
      expect(await screen.findByText(STALE)).toBeInTheDocument();
      expect(save()).toBeDisabled();
      expect(minutes()).toHaveValue(150);
      await act(async () => { ROSTER_ENDS.answered(write); await write.promise.catch(() => {}); });
      expect(save()).toBeEnabled();
    });
  }

  it("a write that begins while the question is up closes it and stops the save, and an answered end turns Save on with no reread", async () => {
    const store = createRosterWrites();
    renderInSession(store);
    await openDateEditor();
    await userEvent.click(screen.getByRole("radio", { name: /^Private/ }));
    const loads = vi.mocked(Groups.get).mock.calls.length;
    edits.answer(challenge(2));
    await waitFor(() => expect(save()).toBeEnabled());
    await userEvent.click(save());
    const asked = await screen.findByRole("dialog", { name: "Save this date?" });
    const yes = within(asked).getByRole("button", { name: "Save and notify" });
    let end: (e: "answered") => void = () => {};

    act(() => { end = store.begin("7:2026-12-01"); });
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    // The Yes kept from the closed question answers nothing.
    await userEvent.click(yes);
    await waitFor(() => expect(screen.getByText(STALE)).toBeInTheDocument());
    // Only the first send, which the writer answered with its question; the yes never reached it.
    expect(edits.sent).toHaveLength(1);
    expect(save()).toBeDisabled();
    act(() => end("answered"));
    expect(save()).toBeEnabled();
    expect(Groups.get).toHaveBeenCalledTimes(loads);
    expect(minutes()).toHaveValue(150);
    await userEvent.click(save());
    await waitFor(() => expect(edits.sent).toHaveLength(2));
    expect(edits.sent[1]).toMatchObject({ changes: { durationMinutes: 150, visibility: "private" } });
    expect(edits.sent[1].consent).toBeUndefined();
  });

  it("a Remove that begins and ends while the count is read, before the page renders again, still stops the save before the question", async () => {
    renderInSession();
    await editorBeside("panel");
    const puts = edits.sent.length;
    const sent = deferred<OccurrenceResponse>();
    edits.answer(sent.promise);
    vi.mocked(Groups.get).mockReturnValueOnce(deferred<GroupDetailResponse>().promise);
    await userEvent.click(save());
    await waitFor(() => expect(edits.sent).toHaveLength(puts + 1));
    const write = await startHeld("remove", panelRoster, "Old Name");
    await act(async () => {
      write.resolve({});
      await write.promise;
      sent.reject(challenge(2));
      await sent.promise.catch(() => {});
    });
    expect(screen.queryByRole("dialog", { name: "Save this date?" })).not.toBeInTheDocument();
    expect(window.confirm).not.toHaveBeenCalled();
    expect(edits.sent).toHaveLength(puts + 1);
    expect(await screen.findByText(STALE)).toBeInTheDocument();
  });

  // A card shows its date editor or its series editor, never both: a write held in the series editor's panel
  // holds the date editor opened after it, and a stored end asks for the page once.
  for (const op of ROSTER_OPS) {
    it(`series panel: a held ${op} keeps the series draft and holds the date editor opened next; a stored end asks for the page once`, async () => {
      renderInSession();
      await editorBeside("series panel");
      const loads = vi.mocked(Groups.get).mock.calls.length;
      const write = await startHeld(op, panelRoster, "Old Name");
      expect(Events.update).toHaveBeenCalledTimes(1);
      expect(requiredTitle()).toHaveValue("Series draft");
      await openDateEditor();
      expect(save()).toBeDisabled();
      await userEvent.click(save());
      expect(edits.sent).toHaveLength(0);
      const reread = deferred<GroupDetailResponse>();
      vi.mocked(Groups.get).mockReturnValueOnce(reread.promise);
      await act(async () => { write.resolve({}); await write.promise; });
      expect(Groups.get).toHaveBeenCalledTimes(loads + 1);
      await act(async () => { reread.resolve(groupPage); await reread.promise; });
      await waitFor(() => expect(save()).toBeEnabled());
      expect(minutes()).toHaveValue(150);
      expect(Groups.get).toHaveBeenCalledTimes(loads + 1);
      expect(edits.sent).toHaveLength(0);
    });
  }

  it("series panel: a held Remove that ends stored keeps the series draft and asks for the page once", async () => {
    renderInSession();
    await editorBeside("series panel");
    const loads = vi.mocked(Groups.get).mock.calls.length;
    const write = await startHeld("remove", panelRoster, "Old Name");
    await act(async () => { write.resolve({}); await write.promise; });
    await waitFor(() => expect(Groups.get).toHaveBeenCalledTimes(loads + 1));
    expect(requiredTitle()).toHaveValue("Series draft");
    expect(Events.update).toHaveBeenCalledTimes(1);
  });

  function requiredTitle() {
    const input = document.getElementById("s-title-1");

    if (!(input instanceof HTMLInputElement)) throw new Error("no series title");

    return input;
  }
});

// B2q: the group page asks a date edit's question in the app's dialog (ConfirmProvider) around the date editor
// alone, only from the writer's own answer for exactly this save, bound to the read, route, row, credential, round
// and roster sequence that asked it. Anything that retires one of them closes it as no before the next paint.
describe("GroupDetail: the date question in the app's dialog", () => {
  const T1 = "11111111111111111111111111111111";
  const T2 = "22222222222222222222222222222222";
  const DAY = "Tuesday, December 1, 2026";
  const row = { id: 7, title: "Supper · 2026-12-01", visibility: "public", registrants: 3, occurrenceId: 1 };
  const challenge = (consent = T1, over: JsonObject = {}) => new ApiError(409, "x", { code: "date_needs_consent", impact: { visibility: "private", currentVisibility: "public", tightened: [row], leaderOpened: [], consent, ...over } });
  const question = `${DAY}. This date is public now. Saving makes it private, so fewer people will be able to see it. This cancels and notifies 3 registered people who can no longer see this date.`;
  const groupPage = { ...page, canManage: true, upcomingEvents: [occurrence({}), occurrence({ id: 2, date: "2026-12-08", startsAt: "2026-12-08T19:00:00Z", endsAt: "2026-12-08T20:00:00Z" })] };
  const asked = () => screen.queryByRole("dialog", { name: "Save this date?" });
  const save = () => screen.getByRole("button", { name: "Save this date" });

  beforeEach(() => {
    vi.mocked(Groups.get).mockResolvedValue(groupPage);
    vi.mocked(Events.managedDate).mockImplementation((_id, date) => Promise.resolve(forGroup({ occurrence: occurrence({ date, id: date === "2026-12-01" ? 1 : 2, revision: "r0" }) })));
    vi.spyOn(Site, "get").mockResolvedValue({ name: "Fold", timezone: "UTC", selfSignup: "off", roomsEnabled: false, pushEnabled: true });
    vi.spyOn(window, "confirm").mockImplementation(() => { throw new Error("the browser's prompt was asked"); });
  });

  afterEach(() => { setMarkerCookie(credential.marker); });

  // What a commit on a new address saw: queued as its layout effects run, a microtask runs after React's
  // synchronous commit work (a layout effect's update included) and before the browser can paint.
  let onCommit: (() => void) | null = null;

  function KeyProbe() {
    const { key } = useLocation();
    useLayoutEffect(() => { onCommit?.(); }, [key]);

    return null;
  }

  // In a session, under a router the test can move (the same address again, another page and back).
  function host(store?: RosterWrites) {
    const router = createMemoryRouter([
      { path: "/groups/:id", element: <><GroupDetail /><KeyProbe /></> },
      { path: "/elsewhere", element: <p>Elsewhere</p> },
    ], { initialEntries: ["/groups/1"] });

    const tree = (c: Credential | null) => <DateWritesProvider rosterWrites={store} credential={c}><RouterProvider router={router} /></DateWritesProvider>;
    const view = render(tree(credential));

    return { router, swap: (c: Credential | null) => view.rerender(tree(c)) };
  }

  // Edit this date → Private → Save, with the writer's answer queued; resolves once the question is up.
  async function ask(answer: Error | Promise<OccurrenceResponse> = challenge()) {
    await userEvent.click(await screen.findByRole("button", { name: /^More for .* on 2026-12-01$/ }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Edit this date" }));
    await userEvent.click(await screen.findByRole("radio", { name: /^Private/ }));
    edits.answer(answer);
    await waitFor(() => expect(save()).toBeEnabled());
    await userEvent.click(save());
  }

  it("waits for the writer's answer, names the date, the change and the count, and Escape or No sends nothing", async () => {
    host();
    const held = deferred<OccurrenceResponse>();
    await ask(held.promise);
    await waitFor(() => expect(edits.sent).toHaveLength(1));
    expect(asked()).not.toBeInTheDocument();
    await act(async () => { held.reject(challenge()); await held.promise.catch(() => {}); });
    const dialog = await screen.findByRole("dialog", { name: "Save this date?" });
    expect(dialog).toHaveAccessibleDescription(question);
    // The question names no title or place from the writer's answer.
    expect(dialog).not.toHaveTextContent("Supper");
    await userEvent.keyboard("{Escape}");
    expect(asked()).not.toBeInTheDocument();
    expect(edits.sent).toHaveLength(1);
    await waitFor(() => expect(save()).toBeEnabled());
    // Focus comes back to the editor (Save, or its form while Save is still disabled), never the page.
    expect(screen.getByRole("spinbutton", { name: "Minutes" }).closest("form")?.contains(document.activeElement)).toBe(true);
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(screen.getByRole("radio", { name: /^Private/ })).toBeChecked();

    edits.answer(challenge(T2));
    await userEvent.click(save());
    await userEvent.click(within(await screen.findByRole("dialog", { name: "Save this date?" })).getByRole("button", { name: "Cancel" }));
    expect(edits.sent).toHaveLength(2);
    expect(edits.sent.map((s) => s.consent)).toEqual([undefined, undefined]);
    expect(window.confirm).not.toHaveBeenCalled();
  });

  it("a stable Yes sends the same attempt once with only the token added; each new 409, even the same count, asks again", async () => {
    host();
    await ask();
    const first = await screen.findByRole("dialog", { name: "Save this date?" });
    const yes = within(first).getByRole("button", { name: "Save and notify" });
    edits.answer(challenge(T2));
    await userEvent.dblClick(yes);
    const again = await screen.findByRole("dialog", { name: "Save this date?" });
    expect(again).toHaveAccessibleDescription(`We checked again before saving. ${question}`);
    expect(edits.sent).toHaveLength(2);
    expect(edits.sent[1]).toMatchObject({ attempt: edits.sent[0].attempt, revision: "r0", changes: { visibility: "private" }, consent: T1 });
    expect(edits.sent[1].body).toBe(`${edits.sent[0].body.slice(0, -1)},"consent":"${T1}"}`);
    // The first question's Yes is gone with it: it answers nothing now.
    await userEvent.click(yes);
    expect(edits.sent).toHaveLength(2);
    await userEvent.click(within(again).getByRole("button", { name: "Save and notify" }));
    await waitFor(() => expect(edits.sent).toHaveLength(3));
    expect(edits.sent[2]).toMatchObject({ attempt: edits.sent[0].attempt, consent: T2 });
    expect(asked()).not.toBeInTheDocument();
    expect(window.confirm).not.toHaveBeenCalled();
  });

  it("a Yes kept from a question that closed answers nothing once the question opens again", async () => {
    host();
    await ask();
    const kept = within(await screen.findByRole("dialog", { name: "Save this date?" })).getByRole("button", { name: "Save and notify" });
    await userEvent.click(screen.getByRole("button", { name: "Close" }));
    expect(asked()).not.toBeInTheDocument();
    await ask(challenge(T2));
    const reopened = await screen.findByRole("dialog", { name: "Save this date?" });
    await userEvent.click(kept);
    expect(reopened).toBeInTheDocument();
    expect(edits.sent).toHaveLength(2);
    await userEvent.click(within(reopened).getByRole("button", { name: "Save and notify" }));
    await waitFor(() => expect(edits.sent).toHaveLength(3));
    expect(edits.sent[2]).toMatchObject({ attempt: edits.sent[1].attempt, consent: T2 });
    expect(edits.sent[1].attempt).not.toBe(edits.sent[0].attempt);
  });

  // Each retires the read, the route, the row or the credential the question was asked under.
  const invalidations: Array<[string, (h: ReturnType<typeof host>, store: RosterWrites) => Promise<void> | void]> = [
    ["the same address again", async ({ router }) => { await act(() => router.navigate("/groups/1")); }],
    ["another page and back", async ({ router }) => { await act(() => router.navigate("/elsewhere")); await act(() => router.navigate("/groups/1")); }],
    ["the page's answer after a sign-up update elsewhere", async (_, store) => {
      await act(async () => store.begin("7:2026-12-08")("stored"));
    }],
    ["the page reads again and this person no longer manages the group", async (_, store) => {
      vi.mocked(Groups.get).mockResolvedValue({ ...groupPage, canManage: false });
      await act(async () => store.begin("7:2026-12-08")("stored"));
    }],
    ["a roster write on the date begins", (_, store) => { act(() => { store.begin("7:2026-12-01"); }); }],
    ["a roster write on the date begins and ends before the page renders", (_, store) => { act(() => store.begin("7:2026-12-01")("answered")); }],
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
      await ask();
      const yes = within(await screen.findByRole("dialog", { name: "Save this date?" })).getByRole("button", { name: "Save and notify" });
      await invalidate(h, store);
      expect(asked()).not.toBeInTheDocument();
      expect(screen.queryByText(question)).not.toBeInTheDocument();
      await userEvent.click(yes);
      expect(edits.sent).toHaveLength(1);
      expect(edits.sent[0].consent).toBeUndefined();
      expect(document.activeElement?.closest("[role='dialog']") ?? null).toBeNull();
      expect(window.confirm).not.toHaveBeenCalled();
    });
  }

  // Seven dates: the page lists them compactly, with only the first date's details open. The open question makes
  // the page behind it inert, so each change comes from the page itself (`hidden: true`), as a refresh's would.
  const sevenDates = { ...groupPage, upcomingEvents: [0, 7, 14, 21, 28, 35, 42].map((days, i) => {
    const day = new Date(Date.UTC(2026, 11, 1 + days)).toISOString().slice(0, 10);

    return occurrence({ id: i + 1, eventId: 7 + i, date: day, startsAt: `${day}T19:00:00Z`, endsAt: `${day}T20:00:00Z`, startsLocal: `${day}T19:00` });
  }) };

  // The third value: whether the editor is still open once the date is shown again (a tab switch closes it).
  const hides: Array<[string, () => Promise<void>, () => Promise<void>, boolean]> = [
    ["hiding the date's details", async () => { act(() => screen.getAllByRole("button", { name: /^Hide details: /, hidden: true })[0].click()); },
      async () => { await userEvent.click(screen.getAllByRole("button", { name: /^Details and RSVP: / })[0]); }, true],
    ["a search that leaves the date out", async () => { fireEvent.change(screen.getByRole("searchbox", { name: /^Search/, hidden: true }), { target: { value: "2026-12-15" } }); },
      async () => { fireEvent.change(screen.getByRole("searchbox", { name: /^Search/ }), { target: { value: "" } }); }, true],
    ["another tab", async () => { act(() => screen.getByRole("tab", { name: "Members", hidden: true }).click()); },
      async () => { act(() => screen.getByRole("tab", { name: "Upcoming events" }).click()); }, false],
  ];

  for (const [name, hide, show, reopens] of hides) {
    it(`closes the question as no on ${name}, and once shown again the editor reads the date afresh and a kept Yes sends nothing`, async () => {
      vi.mocked(Groups.get).mockResolvedValue(sevenDates);
      host();
      await ask();
      const yes = within(await screen.findByRole("dialog", { name: "Save this date?" })).getByRole("button", { name: "Save and notify" });
      const reads = vi.mocked(Events.managedDate).mock.calls.length;
      await hide();
      expect(asked()).not.toBeInTheDocument();
      await userEvent.click(yes);
      await show();

      if (reopens) {
        // Shown again, the editor reads the date before anything can be saved.
        await waitFor(() => expect(Events.managedDate).toHaveBeenCalledTimes(reads + 1));
        await waitFor(() => expect(save()).toBeEnabled());
      } else {
        expect(await screen.findByRole("button", { name: /^More for .* on 2026-12-01$/ })).toBeInTheDocument();
        expect(screen.queryByRole("button", { name: "Save this date" })).not.toBeInTheDocument();
        expect(Events.managedDate).toHaveBeenCalledTimes(reads);
      }

      expect(asked()).not.toBeInTheDocument();
      expect(edits.sent).toHaveLength(1);
      expect(edits.sent[0].consent).toBeUndefined();
    });
  }

  it("closes the question in the commit that moves to the same address, before anything else can run", async () => {
    const { router } = host();
    await ask();
    await screen.findByRole("dialog", { name: "Save this date?" });
    const seen: boolean[] = [];
    onCommit = () => queueMicrotask(() => seen.push(!!asked()));

    try {
      await act(() => router.navigate("/groups/1"));
    } finally {
      onCommit = null;
    }

    expect(seen).toEqual([false]);
  });

  it("a Yes after this tab's marker changed sends nothing, and the new credential's read can save again", async () => {
    const { swap } = host();
    await ask();
    const dialog = await screen.findByRole("dialog", { name: "Save this date?" });
    setMarkerCookie("m-another-tab");
    await userEvent.click(within(dialog).getByRole("button", { name: "Save and notify" }));
    expect(asked()).not.toBeInTheDocument();
    expect(edits.sent).toHaveLength(1);
    const next = await verifiedCredential();
    act(() => swap(next));
    // The new credential's editor starts again from its own read: the draft never crosses credentials.
    expect(await screen.findByRole("radio", { name: /^Private/ })).not.toBeChecked();
    await userEvent.click(screen.getByRole("radio", { name: /^Private/ }));
    await waitFor(() => expect(save()).toBeEnabled());
    await userEvent.click(save());
    await waitFor(() => expect(edits.sent).toHaveLength(2));
    expect(edits.sent[1].consent).toBeUndefined();
    expect(edits.sent[1].attempt).not.toBe(edits.sent[0].attempt);
  });

  it.each([
    ["another date's row", challenge(T1, { tightened: [{ ...row, occurrenceId: 2 }] })],
    ["another event's row", challenge(T1, { tightened: [{ ...row, id: 8 }] })],
    ["two rows", challenge(T1, { tightened: [row, { ...row, occurrenceId: 2 }] })],
    ["a count of zero", challenge(T1, { tightened: [{ ...row, registrants: 0 }] })],
    ["a count that is not a whole number", challenge(T1, { tightened: [{ ...row, registrants: 2.5 }] })],
    ["another current value", challenge(T1, { currentVisibility: "invite_only", tightened: [{ ...row, visibility: "invite_only" }] })],
    ["another target", challenge(T1, { visibility: "invite_only" })],
    ["no token", new ApiError(409, "x", { code: "date_needs_consent", impact: { visibility: "private", currentVisibility: "public", tightened: [row], leaderOpened: [] } })],
  ])("never asks for %s, and says to reload", async (_, answer) => {
    host();
    await ask(answer);
    expect(await screen.findByText(ERROR_COPY.visibility_review_outdated)).toBeInTheDocument();
    expect(asked()).not.toBeInTheDocument();
    expect(edits.sent).toHaveLength(1);
    await waitFor(() => expect(save()).toBeEnabled());
    expect(window.confirm).not.toHaveBeenCalled();
  });

  // An administrator erasing this manager from their own browser (not a sign-out in another tab), in App's session.
  describe("when an administrator erases this manager from their own browser", () => {
    erasureCases(() => ({
      credential: () => credential,
      sent: () => edits.sent,
      mount: (onCredential) => {
        const router = createMemoryRouter([{ path: "/groups/:id", element: <GroupDetail /> }], { initialEntries: ["/groups/1"] });
        render(<SignedInSession onCredential={onCredential}><RouterProvider router={router} /></SignedInSession>);
      },
      draft: async () => {
        await userEvent.click(await screen.findByRole("button", { name: /^More for .* on 2026-12-01$/ }));
        await userEvent.click(screen.getByRole("menuitem", { name: "Edit this date" }));
        await userEvent.click(await screen.findByRole("radio", { name: /^Private/ }));
      },
      drafted: () => screen.getByRole("radio", { name: /^Private/, hidden: true }),
      ask: async () => {
        edits.answer(challenge());
        await waitFor(() => expect(save()).toBeEnabled());
        await userEvent.click(save());
      },
    }));
  });

  it("leaves the page's other questions with the browser's prompt", async () => {
    host();
    vi.mocked(window.confirm).mockReturnValue(false);
    await userEvent.click(await screen.findByRole("button", { name: /^More for .* on 2026-12-01$/ }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Cancel this date" }));
    expect(window.confirm).toHaveBeenCalledExactlyOnceWith("Cancel this date on 2026-12-01?");
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });
});

// DEC-158: the group review names the group's current value, confirms with the consent of the impact it shows,
// re-opens from a 409's own impact, and Cancel hands the keyboard back to Save group with the draft kept.
describe("the group visibility review", () => {
  const A = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
  const B = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb";

  const impact = (current: "public" | "private", consent: string) => ({
    visibility: "invite_only" as const, currentVisibility: current, tightened: [{ id: 7, title: "Supper", visibility: current, registrants: 2 }], leaderOpened: [], consent,
  });

  async function openReview() {
    vi.mocked(Groups.get).mockResolvedValue({ ...page, canManage: true });
    vi.mocked(Groups.visibilityImpact).mockResolvedValue(impact("public", A));
    open();
    await userEvent.click(await screen.findByRole("button", { name: "More group actions" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Edit group" }));
    await userEvent.type(screen.getByLabelText("Group name"), " Two");
    await userEvent.click(screen.getByRole("radio", { name: /^Invite only/ }));
    await userEvent.click(screen.getByRole("button", { name: "Save group" }));

    return screen.findByRole("dialog", { name: "Making the group invite-only changes some events" });
  }

  it("names the current value and confirms with the review's consent", async () => {
    vi.mocked(Groups.update).mockResolvedValue({ group: page.group });
    const dialog = await openReview();
    expect(dialog).toHaveAccessibleDescription("The group is public now. Saving makes it invite-only, so fewer people will be able to see it.");
    expect(Groups.update).not.toHaveBeenCalled();
    await userEvent.click(within(dialog).getByRole("button", { name: "Make the group invite-only" }));
    await waitFor(() => expect(Groups.update).toHaveBeenCalledWith("1", expect.objectContaining({ visibility: "invite_only", consent: A })));
  });

  it("re-opens from a 409 that changed only the current value, with its consent, and never previews again", async () => {
    vi.mocked(Groups.update).mockRejectedValueOnce(new ApiError(409, "x", { needsDecision: true, impact: JSON.parse(JSON.stringify(impact("private", B))) })).mockResolvedValueOnce({ group: page.group });
    const dialog = await openReview();
    await userEvent.click(within(dialog).getByRole("button", { name: "Make the group invite-only" }));
    expect(await screen.findByText("We checked again before saving. Please review the group's events again.")).toBeInTheDocument();
    const fresh = screen.getByRole("dialog", { name: "Making the group invite-only changes some events" });
    expect(fresh).toHaveAccessibleDescription("The group is private now. Saving makes it invite-only, so fewer people will be able to see it.");
    expect(document.activeElement).toBe(within(fresh).getByRole("heading", { name: "Making the group invite-only changes some events" }));
    expect(Groups.visibilityImpact).toHaveBeenCalledOnce();
    await userEvent.click(within(fresh).getByRole("button", { name: "Make the group invite-only" }));
    await waitFor(() => expect(Groups.update).toHaveBeenLastCalledWith("1", expect.objectContaining({ consent: B })));
  });

  it("refuses another Save group while the review's confirm is saving (#431)", async () => {
    const saving = deferred<{ group: typeof page.group }>();
    vi.mocked(Groups.update).mockReturnValueOnce(saving.promise);
    const dialog = await openReview();
    await userEvent.click(within(dialog).getByRole("button", { name: "Make the group invite-only" }));
    await waitFor(() => expect(Groups.update).toHaveBeenCalledOnce());
    const form = screen.getByLabelText("Group name").closest("form")!;
    expect(within(form).getByRole("button", { name: /^(Save group|Saving…)$/ })).toBeDisabled();
    fireEvent.submit(form);
    expect(Groups.visibilityImpact).toHaveBeenCalledOnce();
    expect(Groups.update).toHaveBeenCalledOnce();
    await act(async () => { saving.resolve({ group: page.group }); await saving.promise; });
  });

  it("keeps focus in the review while its confirm saves, and gives it back to confirm when the save fails", async () => {
    const saving = deferred<{ group: typeof page.group }>();
    vi.mocked(Groups.update).mockReturnValueOnce(saving.promise);
    const dialog = await openReview();
    const confirm = within(dialog).getByRole("button", { name: "Make the group invite-only" });
    confirm.focus();
    await userEvent.keyboard("{Enter}");
    await waitFor(() => expect(Groups.update).toHaveBeenCalledOnce());
    expect(document.activeElement).toBe(dialog.parentElement);
    await act(async () => { saving.reject(new ApiError(500, "private", { code: "server_error" })); await saving.promise.catch(() => {}); });
    expect(document.activeElement).toBe(confirm);
  });

  it("refuses the review's confirm while Save group is checking again", async () => {
    const dialog = await openReview();
    const again = deferred<ReturnType<typeof impact>>();
    vi.mocked(Groups.visibilityImpact).mockReturnValueOnce(again.promise);
    await userEvent.click(screen.getByRole("button", { name: "Save group" }));
    expect(Groups.visibilityImpact).toHaveBeenCalledTimes(2);
    const confirm = within(dialog).getByRole("button", { name: /^(Make the group invite-only|Saving…)$/ });
    expect(confirm).toBeDisabled();
    fireEvent.click(confirm);
    expect(Groups.update).not.toHaveBeenCalled();
    await act(async () => { again.resolve(impact("public", A)); await again.promise; });
  });

  it("a failed save is not shown again after Cancel and Edit group", async () => {
    vi.mocked(Groups.update).mockRejectedValueOnce(new ApiError(500, "private", { code: "server_error" }));
    const dialog = await openReview();
    vi.mocked(Groups.visibilityImpact).mockResolvedValue({ ...impact("public", A), tightened: [] });
    await userEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    await userEvent.click(screen.getByRole("radio", { name: /^Private/ }));
    await userEvent.click(screen.getByRole("button", { name: "Save group" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("We couldn't save your changes.");
    await userEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await userEvent.click(await screen.findByRole("button", { name: "More group actions" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Edit group" }));
    expect(await screen.findByLabelText("Group name")).toHaveValue("Family group");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("Cancel from the keyboard lands on Save group with the draft kept", async () => {
    const dialog = await openReview();
    expect(document.activeElement).toBe(within(dialog).getByRole("heading", { name: "Making the group invite-only changes some events" }));
    within(dialog).getByRole("button", { name: "Cancel" }).focus();
    await userEvent.keyboard("{Enter}");
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Save group" })).toHaveFocus();
    expect(screen.getByLabelText("Group name")).toHaveValue("Family group Two");
    expect(screen.getByRole("radio", { name: /^Invite only/ })).toBeChecked();
  });
});

// VC2: the group review opens (from a preview or a 409) and is confirmed only when no sign-up update on one of
// the events it lists began, ended or is on its way since the request it comes from was sent. The updates come
// through the session's store, as a roster's Add, Add back or Remove would start them (the date cards are not on
// screen while the group form is open, so only a write already on its way, or one from another card's panel,
// can reach a review).
describe("the group visibility review and sign-up updates", () => {
  const STALE = "A sign-up update started on one of the group's events while we were checking. Please review again once it finishes.";
  const A = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
  const B = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb";
  const KEY = "7:2026-12-01";

  const impact = (consent: string, registrants = 2) => ({
    visibility: "invite_only" as const, currentVisibility: "public" as const, tightened: [{ id: 7, title: "Supper", visibility: "public" as const, registrants }], leaderOpened: [], consent,
  });

  const decision = (i: ReturnType<typeof impact>) => new ApiError(409, "x", { needsDecision: true, impact: JSON.parse(JSON.stringify(i)) });
  const wrapper = () => requiredWrapper();

  function requiredWrapper() {
    const el = screen.getByRole("button", { name: "Save group" }).closest("div.pt-6")?.querySelector(":scope > div[tabindex='-1']");

    if (!(el instanceof HTMLElement)) throw new Error("no review home");

    return el;
  }

  async function editGroup(store: RosterWrites) {
    vi.mocked(Groups.get).mockResolvedValue({ ...page, canManage: true });
    render(<MemoryRouter initialEntries={["/groups/1"]}><DateWritesProvider rosterWrites={store} credential={credential}><Routes><Route path="/groups/:id" element={<GroupDetail />} /></Routes></DateWritesProvider></MemoryRouter>);
    await userEvent.click(await screen.findByRole("button", { name: "More group actions" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Edit group" }));
    await userEvent.type(screen.getByLabelText("Group name"), " Two");
    await userEvent.click(screen.getByRole("radio", { name: /^Invite only/ }));
  }

  function expectDraft() {
    expect(screen.getByLabelText("Group name")).toHaveValue("Family group Two");
    expect(screen.getByRole("radio", { name: /^Invite only/ })).toBeChecked();
  }

  it("a begin on a listed event during the preview's await opens no review, saves nothing and keeps the form", async () => {
    const store = createRosterWrites();
    const preview = deferred<ReturnType<typeof impact>>();
    vi.mocked(Groups.visibilityImpact).mockReturnValueOnce(preview.promise);
    await editGroup(store);
    screen.getByRole("button", { name: "Save group" }).focus();
    await userEvent.keyboard("{Enter}");
    let end: (e: RosterWriteEnd) => void = () => {};

    await act(async () => { end = store.begin(KEY); preview.resolve(impact(A)); await preview.promise; });
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(Groups.update).not.toHaveBeenCalled();
    expect(screen.getByText(STALE)).toBeInTheDocument();
    expect(document.activeElement).toBe(wrapper());
    expectDraft();
    // Once it ends, the next Save previews again and shows the current impact.
    act(() => end("stored"));
    vi.mocked(Groups.visibilityImpact).mockResolvedValueOnce(impact(B, 3));
    await userEvent.click(screen.getByRole("button", { name: "Save group" }));
    const review = await screen.findByRole("dialog", { name: "Making the group invite-only changes some events" });
    expect(review).toHaveTextContent("3 people registered will be cancelled and notified");
    expect(screen.queryByText(STALE)).not.toBeInTheDocument();
    vi.mocked(Groups.update).mockResolvedValueOnce({ group: page.group });
    await userEvent.click(within(review).getByRole("button", { name: "Make the group invite-only" }));
    await waitFor(() => expect(Groups.update).toHaveBeenCalledExactlyOnceWith("1", expect.objectContaining({ visibility: "invite_only", consent: B })));
  });

  for (const ending of ["stored", "answered"] as const) {
    it(`a write already on its way that ends ${ending} during the preview's await opens no review`, async () => {
      const store = createRosterWrites();
      const end = store.begin(KEY);
      const preview = deferred<ReturnType<typeof impact>>();
      vi.mocked(Groups.visibilityImpact).mockReturnValueOnce(preview.promise);
      await editGroup(store);
      await userEvent.click(screen.getByRole("button", { name: "Save group" }));
      await act(async () => { end(ending); preview.resolve(impact(A)); await preview.promise; });
      expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
      expect(screen.getByText(STALE)).toBeInTheDocument();
      expect(Groups.update).not.toHaveBeenCalled();
      expectDraft();
    });
  }

  it("a write still on its way when the preview answers opens no review", async () => {
    const store = createRosterWrites();
    store.begin(KEY);
    vi.mocked(Groups.visibilityImpact).mockResolvedValueOnce(impact(A));
    await editGroup(store);
    await userEvent.click(screen.getByRole("button", { name: "Save group" }));
    expect(await screen.findByText(STALE)).toBeInTheDocument();
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(Groups.update).not.toHaveBeenCalled();
    expectDraft();
  });

  it("a write on an event the impact does not list leaves the review to open as usual", async () => {
    const store = createRosterWrites();
    const preview = deferred<ReturnType<typeof impact>>();
    vi.mocked(Groups.visibilityImpact).mockReturnValueOnce(preview.promise);
    await editGroup(store);
    await userEvent.click(screen.getByRole("button", { name: "Save group" }));
    await act(async () => { store.begin("8:2026-12-01"); preview.resolve(impact(A)); await preview.promise; });
    const review = await screen.findByRole("dialog", { name: "Making the group invite-only changes some events" });
    expect(within(review).getByRole("button", { name: "Make the group invite-only" })).toBeEnabled();
    expect(screen.queryByText(STALE)).not.toBeInTheDocument();
  });

  it("a begin while the review is open closes it with the stale line and sends nothing", async () => {
    const store = createRosterWrites();
    vi.mocked(Groups.visibilityImpact).mockResolvedValueOnce(impact(A));
    await editGroup(store);
    await userEvent.click(screen.getByRole("button", { name: "Save group" }));
    await screen.findByRole("dialog", { name: "Making the group invite-only changes some events" });
    act(() => { store.begin(KEY); });
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(screen.getByText(STALE)).toBeInTheDocument();
    expect(Groups.update).not.toHaveBeenCalled();
    // Focus was on the review's heading, which went with it: the review's home keeps it, never the page.
    expect(document.activeElement).toBe(wrapper());
    expectDraft();
  });

  it("a begin during the confirm's await, then a 409 decision: the review closes, none opens from the 409, no second update", async () => {
    const store = createRosterWrites();
    vi.mocked(Groups.visibilityImpact).mockResolvedValueOnce(impact(A));
    const put = deferred<{ group: typeof page.group }>();
    vi.mocked(Groups.update).mockReturnValueOnce(put.promise);
    await editGroup(store);
    await userEvent.click(screen.getByRole("button", { name: "Save group" }));
    await userEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Make the group invite-only" }));
    act(() => { store.begin(KEY); });
    await act(async () => { put.reject(decision(impact(B))); await put.promise.catch(() => {}); });
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(screen.getByText(STALE)).toBeInTheDocument();
    expect(screen.queryByText("We checked again before saving. Please review the group's events again.")).not.toBeInTheDocument();
    expect(document.activeElement).toBe(wrapper());
    expect(Groups.update).toHaveBeenCalledOnce();
    expectDraft();
  });

  it("a begin on an event only the 409 lists, during a save that needed no review, opens no review", async () => {
    const store = createRosterWrites();
    // No tightening: nothing to preview, so the save goes straight to the update.
    vi.mocked(Groups.visibilityImpact).mockResolvedValueOnce({ ...impact(A), tightened: [] });
    const put = deferred<{ group: typeof page.group }>();
    vi.mocked(Groups.update).mockReturnValueOnce(put.promise);
    await editGroup(store);
    await userEvent.click(screen.getByRole("button", { name: "Save group" }));
    await waitFor(() => expect(Groups.update).toHaveBeenCalledOnce());
    act(() => { store.begin(KEY); });
    await act(async () => { put.reject(decision(impact(B))); await put.promise.catch(() => {}); });
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(await screen.findByText(STALE)).toBeInTheDocument();
    expect(Groups.update).toHaveBeenCalledOnce();
    expectDraft();
  });

  it("a begin during the confirm's await, then a 200: saved, the form closes, and the next Edit shows no stale line", async () => {
    const store = createRosterWrites();
    vi.mocked(Groups.visibilityImpact).mockResolvedValueOnce(impact(A));
    const put = deferred<{ group: typeof page.group }>();
    vi.mocked(Groups.update).mockReturnValueOnce(put.promise);
    await editGroup(store);
    await userEvent.click(screen.getByRole("button", { name: "Save group" }));
    await userEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Make the group invite-only" }));
    act(() => { store.begin(KEY); });
    await act(async () => { put.resolve({ group: page.group }); await put.promise; });
    await waitFor(() => expect(screen.queryByLabelText("Group name")).not.toBeInTheDocument());
    expect(screen.queryByText(STALE)).not.toBeInTheDocument();
    await userEvent.click(await screen.findByRole("button", { name: "More group actions" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Edit group" }));
    expect(screen.queryByText(STALE)).not.toBeInTheDocument();
    expect(Groups.update).toHaveBeenCalledOnce();
  });

  // The seam between the page accepting an answer and watching for writes: a write that begins and ends there
  // has to withdraw the review before anyone sees it.
  for (const ending of ["stored", "answered"] as const) {
    it(`a write that begins and ends ${ending} after the preview's check, before the review is watched, never shows it`, async () => {
      const store = createRosterWrites();
      const checks = watchChecks(store);
      const preview = deferred<ReturnType<typeof impact>>();
      vi.mocked(Groups.visibilityImpact).mockReturnValueOnce(preview.promise);
      await editGroup(store);
      screen.getByRole("button", { name: "Save group" }).focus();
      await userEvent.keyboard("{Enter}");
      await writeAfterTheCheck(checks, async () => { preview.resolve(impact(A)); await preview.promise; }, store, KEY, ending);
      expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
      expect(screen.getByText(STALE)).toBeInTheDocument();
      expect(document.activeElement).toBe(wrapper());
      expect(Groups.update).not.toHaveBeenCalled();
      expectDraft();
      // The next Save previews again and confirms the current impact, once.
      vi.mocked(Groups.visibilityImpact).mockResolvedValueOnce(impact(B, 3));
      await userEvent.click(screen.getByRole("button", { name: "Save group" }));
      const review = await screen.findByRole("dialog", { name: "Making the group invite-only changes some events" });
      expect(review).toHaveTextContent("3 people registered will be cancelled and notified");
      expect(screen.queryByText(STALE)).not.toBeInTheDocument();
      vi.mocked(Groups.update).mockResolvedValueOnce({ group: page.group });
      await userEvent.click(within(review).getByRole("button", { name: "Make the group invite-only" }));
      await waitFor(() => expect(Groups.update).toHaveBeenCalledExactlyOnceWith("1", expect.objectContaining({ visibility: "invite_only", consent: B })));
    });
  }

  it("a write that begins and ends after a 409's check, on a save that opened no review, never shows the 409's review", async () => {
    const store = createRosterWrites();
    const checks = watchChecks(store);
    vi.mocked(Groups.visibilityImpact).mockResolvedValueOnce({ ...impact(A), tightened: [] });
    const put = deferred<{ group: typeof page.group }>();
    vi.mocked(Groups.update).mockReturnValueOnce(put.promise);
    await editGroup(store);
    screen.getByRole("button", { name: "Save group" }).focus();
    await userEvent.keyboard("{Enter}");
    await waitFor(() => expect(Groups.update).toHaveBeenCalledOnce());
    await writeAfterTheCheck(checks, async () => { put.reject(decision(impact(B))); await put.promise.catch(() => {}); }, store, KEY, "stored");
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(screen.getByText(STALE)).toBeInTheDocument();
    expect(screen.queryByText("We checked again before saving. Please review the group's events again.")).not.toBeInTheDocument();
    expect(document.activeElement).toBe(wrapper());
    expect(Groups.update).toHaveBeenCalledOnce();
    expectDraft();
  });

  // A 409 that replaces an open review is watched again from the confirm's own mark, whether it lists the same
  // events or new ones.
  for (const [label, eventId] of [["the same event", 7], ["an event only the 409 lists", 9]] as const) {
    it(`a write on ${label} after a confirm's 409 is checked withdraws the 409's review, with no second update`, async () => {
      const store = createRosterWrites();
      const checks = watchChecks(store);
      vi.mocked(Groups.visibilityImpact).mockResolvedValueOnce(impact(A));
      const put = deferred<{ group: typeof page.group }>();
      vi.mocked(Groups.update).mockReturnValueOnce(put.promise);
      await editGroup(store);
      await userEvent.click(screen.getByRole("button", { name: "Save group" }));
      await userEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Make the group invite-only" }));
      const fresh = { ...impact(B, 3), tightened: [{ ...impact(B).tightened[0], id: eventId }] };
      await writeAfterTheCheck(checks, async () => { put.reject(decision(fresh)); await put.promise.catch(() => {}); }, store, `${eventId}:2026-12-01`, "answered");
      expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
      expect(screen.getByText(STALE)).toBeInTheDocument();
      expect(Groups.update).toHaveBeenCalledOnce();
      expectDraft();
    });
  }

  it("a write in the preview's seam after Enter in a field gives focus back to that field", async () => {
    const store = createRosterWrites();
    const checks = watchChecks(store);
    const preview = deferred<ReturnType<typeof impact>>();
    vi.mocked(Groups.visibilityImpact).mockReturnValueOnce(preview.promise);
    await editGroup(store);
    const name = screen.getByLabelText("Group name");
    name.focus();
    await userEvent.keyboard("{Enter}");
    expect(name).toBeDisabled();
    expect(document.activeElement).toBe(name.closest("form"));
    await writeAfterTheCheck(checks, async () => { preview.resolve(impact(A)); await preview.promise; }, store, KEY, "answered");
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(screen.getByText(STALE)).toBeInTheDocument();
    expect(name).toBeEnabled();
    expect(document.activeElement).toBe(name);
    expect(Groups.update).not.toHaveBeenCalled();
    expectDraft();
  });

  it("the stale line goes with Cancel and with a new Save", async () => {
    const store = createRosterWrites();
    vi.mocked(Groups.visibilityImpact).mockResolvedValueOnce(impact(A));
    await editGroup(store);
    await userEvent.click(screen.getByRole("button", { name: "Save group" }));
    await screen.findByRole("dialog");
    act(() => { store.begin(KEY)("answered"); });
    expect(screen.getByText(STALE)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await userEvent.click(await screen.findByRole("button", { name: "More group actions" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Edit group" }));
    expect(screen.queryByText(STALE)).not.toBeInTheDocument();
  });
});

// VC2 on a group date card: the event's visibility control sits under the series editor, whose kept-dates panel
// can Add, Add back or Remove on one of the event's dates. A write that begins while the preview is on its way
// opens no review; a write that begins while the review is open closes it.
describe("GroupDetail: the event visibility review and the series editor's panel", () => {
  const STALE = "A sign-up update started while we were checking. Please review again once it finishes.";
  const A = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
  const tightening = { visibility: "invite_only" as const, currentVisibility: "public" as const, tightened: [{ id: 7, title: "Supper", visibility: "public" as const, registrants: 1 }], leaderOpened: [], consent: A };
  const groupPage = { ...page, canManage: true, upcomingEvents: [occurrence({ attendeesVisible: true })] };

  beforeEach(() => {
    vi.mocked(Groups.get).mockResolvedValue(groupPage);
    vi.mocked(Events.managedDate).mockImplementation((_id, date) => Promise.resolve(keptRead(date, { removedRegistrations: [removedRow()] })));
    vi.mocked(Events.visibilityImpact).mockResolvedValue({ visibility: "public", currentVisibility: "public", tightened: [], leaderOpened: [] });
    vi.spyOn(Site, "get").mockResolvedValue({ name: "Fold", timezone: "UTC", selfSignup: "off", roomsEnabled: false, pushEnabled: true });
    vi.spyOn(window, "confirm").mockImplementation(() => { throw new Error("native confirm"); });
  });

  const save = () => screen.getByRole("button", { name: "Save visibility" });

  const panelRoster = () => {
    const section = screen.getByText("Old Name", { selector: "p, span, div, strong" }).closest("section[aria-label='Manage sign-ups']");

    if (!(section instanceof HTMLElement)) throw new Error("no panel roster");

    return section;
  };

  async function openSeries() {
    render(<MemoryRouter initialEntries={["/groups/1"]}><DateWritesProvider credential={credential}><Routes><Route path="/groups/:id" element={<GroupDetail />} /></Routes></DateWritesProvider></MemoryRouter>);
    await userEvent.click(await screen.findByRole("button", { name: /^More for .* on 2026-12-01$/ }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Edit event" }));
    vi.mocked(Events.update).mockRejectedValueOnce(kept409([{ date: "2026-12-01", keptCount: 1 }]));
    await userEvent.click(screen.getByRole("button", { name: "Save series" }));
    await screen.findByText("Old Name");
    await userEvent.click(screen.getByRole("radio", { name: /^Invite only/ }));
  }

  for (const op of ROSTER_OPS) {
    it(`a panel ${op} that begins during the preview's await opens no review and sends nothing`, async () => {
      await openSeries();
      const preview = deferred<typeof tightening>();
      vi.mocked(Events.visibilityImpact).mockReturnValueOnce(preview.promise);
      await userEvent.click(save());
      const write = await startHeld(op, panelRoster, "Old Name");
      await act(async () => { preview.resolve(tightening); await preview.promise; });
      expect(screen.queryByRole("dialog", { name: "Making this event invite-only" })).not.toBeInTheDocument();
      expect(screen.getByText(STALE)).toBeInTheDocument();
      expect(save()).toBeDisabled();
      expect(Events.setVisibility).not.toHaveBeenCalled();
      await act(async () => { ROSTER_ENDS.answered(write); await write.promise.catch(() => {}); });
      expect(save()).toBeEnabled();
      expect(screen.getByRole("radio", { name: /^Invite only/ })).toBeChecked();
    });

    it(`a panel ${op} that begins while the review is open closes it and sends nothing`, async () => {
      await openSeries();
      vi.mocked(Events.visibilityImpact).mockResolvedValueOnce(tightening);
      await userEvent.click(save());
      await screen.findByRole("dialog", { name: "Making this event invite-only" });
      const write = await startHeld(op, panelRoster, "Old Name");
      expect(screen.queryByRole("dialog", { name: "Making this event invite-only" })).not.toBeInTheDocument();
      expect(screen.getByText(STALE)).toBeInTheDocument();
      await act(async () => { ROSTER_ENDS.answered(write); await write.promise.catch(() => {}); });
      expect(Events.setVisibility).not.toHaveBeenCalled();
    });
  }
});
