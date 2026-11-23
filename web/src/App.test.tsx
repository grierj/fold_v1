import { renderWithAuth } from "./test/auth";
import { stubService } from "./test/service";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { RouterProvider, createMemoryRouter } from "react-router-dom";
import { beforeAll, beforeEach, expect, it, vi } from "vitest";
import { ApiError, type Credential } from "./api/core";
import { Events } from "./api/events";
import { Groups } from "./api/groups";
import { Households } from "./api/households";
import { Notifications } from "./api/notifications";
import { Site } from "./api/site";
import type { EventDetailResponse, EventOccurrence, GroupDetailResponse, OccurrenceResponse, User } from "./types";
import { occurrence } from "./test/occurrence";
import { testEvent, testGroupDetail } from "./test/fixtures";
import { eventDetailChunk } from "./pages/Home";
import { groupDetailChunk } from "./components/GroupCard";
import App from "./App";
import { stubEditSends, verifiedCredential, type EditSends } from "./test/guardedEdits";

// The signed-in person App sees; a test changes it and renders again, as the auth provider would.
interface TestSession { user: Partial<User> | null; credential: Credential | null }

const session: TestSession = { user: { id: 2 }, credential: null };

const render = renderWithAuth(() => ({ user: session.user, loading: false, isAdmin: false, canCompose: false, credential: session.credential }));

stubService(Site, () => ({ get: vi.fn() }));

stubService(Notifications, () => ({ badge: vi.fn() }));

stubService(Households, () => ({ mine: vi.fn() }));

stubService(Groups, () => ({ get: vi.fn() }));

stubService(Events, () => ({ get: vi.fn(), managedDate: vi.fn(), dateVisibilityImpact: vi.fn() }));

// One date both pages show and a leader may edit from either: making it follow its private series
// cancels and tells two people, which the page asks about first.
const date = occurrence({ groupId: 1, groupName: "Family group", groupVisibility: "public", visibility: "public", visibilityOverride: "public", seriesVisibility: "private" });
const eventPage: EventDetailResponse = { event: { ...testEvent(7), title: "Supper" }, occurrences: [date], canManage: true, canSetVisibility: true };
const groupPage: GroupDetailResponse = { ...testGroupDetail({ id: 1, name: "Family group" }), upcomingEvents: [date] };

// The question opens with the date's current and new value (DEC-158).
const dateQuestion = "Tuesday, December 1, 2026. This date is public now. Saving makes it private, so fewer people will be able to see it. This cancels and notifies 2 registered people who can no longer see this date.";
const UNSURE = "We couldn't tell whether this date saved. Check it below, and try again if you need to.";
const T = "c".repeat(32);

// The writer's question when this date follows its private series (DEC-158).
const challenge = () => new ApiError(409, "x", { code: "date_needs_consent", impact: { visibility: "private", currentVisibility: "public", tightened: [{ id: 7, title: "Supper", visibility: "public", registrants: 2, occurrenceId: 1 }], leaderOpened: [], consent: T } });

// A dated event of the same group, on the group page's first page.
const picnic = occurrence({ id: 9, eventId: 8, groupId: 1, groupName: "Family group", groupVisibility: "public", date: "2026-11-28", startsAt: "2026-11-28T19:00:00Z", endsAt: "2026-11-28T20:00:00Z", startsLocal: "2026-11-28T19:00", title: "Picnic" });

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (cause: Error) => void;
  const promise = new Promise<T>((accept, decline) => { resolve = accept; reject = decline; });

  return { promise, resolve, reject };
}

type Put = OccurrenceResponse;

let edits: EditSends;

// What each date's own manager read answers now, by `${eventId}:${date}`: a stored edit updates its date.
let stored: Map<string, EventOccurrence>;
const keyOf = (o: EventOccurrence) => `${o.eventId}:${o.date}`;

// The guarded edit's answer, stored as `saved`: each editor's next read of the date shows it.
function settle(put: ReturnType<typeof deferred<Put>>, saved: EventOccurrence) {
  stored.set(keyOf(saved), saved);
  put.resolve({ occurrence: { ...saved, revision: `r-${saved.title}` } });
}

// Lets every answer already given run its code (a page's read resolves, its state is set) before what follows.
// Inside one act nothing is shown until the act ends, so an answer can be in but not yet on screen.
const answersRun = () => new Promise((done) => { setTimeout(done, 0); });

// The production tree: App under the one splat route main.tsx gives it.
function renderApp(path: string) {
  const router = createMemoryRouter([{ path: "*", element: <App /> }], { initialEntries: [path] });
  const view = render(<RouterProvider router={router} />);

  return { router, rerender: () => view.rerender(<RouterProvider router={router} />) };
}

async function openMenu(name: string) {
  await userEvent.click(await screen.findByRole("button", { name }));

  return screen.getByRole("menuitem", { name: "Edit this date" });
}

// Opens a date's menu once the page shows what was stored, and Edit this date opens.
async function editableMenu(name: string) {
  await userEvent.click(await screen.findByRole("button", { name }));
  await waitFor(() => expect(screen.getByRole("menuitem", { name: "Edit this date" })).toBeEnabled());

  return screen.getByRole("menuitem", { name: "Edit this date" });
}

async function saveTitle(menu: string) {
  await userEvent.click(await openMenu(menu));
  const title = await screen.findByLabelText("Title");
  await userEvent.clear(title);
  await userEvent.type(title, "Carols");
  await waitFor(() => expect(screen.getByRole("button", { name: "Save this date" })).toBeEnabled());
  await userEvent.click(screen.getByRole("button", { name: "Save this date" }));
}

const EVENT_MENU = "More for 2026-12-01";
const GROUP_MENU = "More for Supper on 2026-12-01";

// Both pages are App's lazy routes: load their code once, so a first visit waits only for its data.
beforeAll(async () => {
  await Promise.all([eventDetailChunk.route(), groupDetailChunk.route()]);
});

beforeEach(async () => {
  vi.resetAllMocks();
  vi.restoreAllMocks();
  session.user = { id: 2 };
  session.credential = await verifiedCredential(2);
  edits = stubEditSends();
  stored = new Map([date, picnic].map((o) => [keyOf(o), o]));
  vi.mocked(Events.managedDate).mockImplementation(async (id, day) => {
    const o = stored.get(`${id}:${day}`);

    if (!o) throw new ApiError(404, "x", { code: "not_found" });

    return { occurrence: { ...o, revision: `r-${o.title}` } };
  });
  vi.mocked(Site.get).mockResolvedValue({ name: "Fold", timezone: "UTC", selfSignup: "off", roomsEnabled: false, pushEnabled: false });
  vi.mocked(Notifications.badge).mockResolvedValue(null);
  vi.mocked(Households.mine).mockResolvedValue({ household: null });
  vi.mocked(Groups.get).mockResolvedValue(groupPage);
  vi.mocked(Events.get).mockResolvedValue(eventPage);
});

it("holds a date's update from the event page on the group page, through an auth refresh, and reloads the group once it answers", async () => {
  const put = deferred<Put>();
  edits.answer(put.promise);
  const app = renderApp("/events/7");
  await saveTitle(EVENT_MENU);
  expect(edits.sent).toHaveLength(1);

  // The group's page lists the same date: its editor stays closed while the event page's update is on its way.
  await act(() => app.router.navigate("/groups/1"));
  expect(await openMenu(GROUP_MENU)).toBeDisabled();
  await userEvent.keyboard("{Escape}");

  // A session check that finds the same person keeps every page and the hold.
  session.user = { id: 2 };
  app.rerender();
  expect(await openMenu(GROUP_MENU)).toBeDisabled();
  await userEvent.keyboard("{Escape}");
  const groupLoads = vi.mocked(Groups.get).mock.calls.length;

  await act(async () => settle(put, date));
  await waitFor(() => expect(Groups.get).toHaveBeenCalledTimes(groupLoads + 1));
  expect(await editableMenu(GROUP_MENU)).toBeEnabled();
  expect(edits.sent).toHaveLength(1);
});

it("drops a group read that was on its way when the event page's update answered, and shows what was saved", async () => {
  const put = deferred<Put>();
  edits.answer(put.promise);
  const saved = { ...date, title: "Carols" };
  // The group page's first read was answered by the server before the update was stored.
  const beforeSave = deferred<GroupDetailResponse>();
  vi.mocked(Groups.get).mockReturnValueOnce(beforeSave.promise).mockResolvedValue({ ...groupPage, upcomingEvents: [saved] });
  const app = renderApp("/events/7");
  await saveTitle(EVENT_MENU);
  expect(edits.sent).toHaveLength(1);

  await act(() => app.router.navigate("/groups/1"));
  await waitFor(() => expect(Groups.get).toHaveBeenCalledOnce());
  await act(async () => settle(put, saved));
  await waitFor(() => expect(Groups.get).toHaveBeenCalledTimes(2));
  const menu = "More for Carols on 2026-12-01";
  expect(await editableMenu(menu)).toBeEnabled();
  await userEvent.keyboard("{Escape}");

  await act(async () => beforeSave.resolve(groupPage));
  expect(screen.getByRole("button", { name: menu })).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: GROUP_MENU })).not.toBeInTheDocument();
  expect(Groups.get).toHaveBeenCalledTimes(2);
  expect(edits.sent).toHaveLength(1);
});

it("holds a date's update from the group page, asked in the app's dialog there, on the event page, and reloads the event once it answers", async () => {
  const confirm = vi.spyOn(window, "confirm").mockImplementation(() => { throw new Error("the browser's prompt was asked"); });
  const put = deferred<Put>();
  // The writer asks first; the yes sends the same attempt with its token, held.
  edits.answer(challenge(), put.promise);
  const app = renderApp("/groups/1");
  await userEvent.click(await openMenu(GROUP_MENU));
  await userEvent.click(await screen.findByRole("radio", { name: "Same as series (private)" }));
  await waitFor(() => expect(screen.getByRole("button", { name: "Save this date" })).toBeEnabled());
  await userEvent.click(screen.getByRole("button", { name: "Save this date" }));
  const asked = await screen.findByRole("dialog", { name: "Save this date?" });
  expect(asked).toHaveAccessibleDescription(dateQuestion);
  await userEvent.click(within(asked).getByRole("button", { name: "Save and notify" }));
  await waitFor(() => expect(edits.sent).toHaveLength(2));
  expect(edits.sent[1]).toMatchObject({ attempt: edits.sent[0].attempt, consent: T });
  expect(confirm).not.toHaveBeenCalled();
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();

  await act(() => app.router.navigate("/events/7"));
  expect(await openMenu(EVENT_MENU)).toBeDisabled();
  await userEvent.keyboard("{Escape}");
  const eventLoads = vi.mocked(Events.get).mock.calls.length;

  await act(async () => settle(put, date));
  await waitFor(() => expect(Events.get).toHaveBeenCalledTimes(eventLoads + 1));
  expect(await editableMenu(EVENT_MENU)).toBeEnabled();
  expect(edits.sent).toHaveLength(2);
  expect(confirm).not.toHaveBeenCalled();
});

it("opens the group page's editor on the stored title from its own read once the event page's update answers, while the group's read is held", async () => {
  const put = deferred<Put>();
  edits.answer(put.promise);
  const saved = { ...date, title: "Carols" };
  const app = renderApp("/events/7");
  await saveTitle(EVENT_MENU);
  await act(() => app.router.navigate("/groups/1"));
  expect(await openMenu(GROUP_MENU)).toBeDisabled();
  await userEvent.keyboard("{Escape}");
  const groupLoads = vi.mocked(Groups.get).mock.calls.length;

  // The group's read after the update is held: the page still lists the old title, but the editor reads the date.
  const reread = deferred<GroupDetailResponse>();
  vi.mocked(Groups.get).mockReturnValueOnce(reread.promise);
  await act(async () => settle(put, saved));
  await waitFor(() => expect(Groups.get).toHaveBeenCalledTimes(groupLoads + 1));
  await userEvent.click(await editableMenu(GROUP_MENU));
  expect(await screen.findByLabelText("Title")).toHaveValue("Carols");

  await act(async () => reread.resolve({ ...groupPage, upcomingEvents: [saved] }));
  expect(await screen.findByLabelText("Title")).toHaveValue("Carols");
  expect(edits.sent).toHaveLength(1);
});

it("shows nothing of a date on the group page while its read after the event page's update failed, then opens it on the stored date", async () => {
  const put = deferred<Put>();
  edits.answer(put.promise);
  const saved = { ...date, title: "Carols" };
  const app = renderApp("/events/7");
  await saveTitle(EVENT_MENU);
  await act(() => app.router.navigate("/groups/1"));
  expect(await openMenu(GROUP_MENU)).toBeDisabled();
  await userEvent.keyboard("{Escape}");

  vi.mocked(Groups.get).mockRejectedValueOnce(new Error("offline"));
  await act(async () => settle(put, saved));
  const tryAgain = await screen.findByRole("button", { name: "Try again" });
  // A failed page read is no authority: an editor opened now shows nothing of the date.
  await userEvent.click(await editableMenu(GROUP_MENU));
  expect(await screen.findByText("This date can be edited again once the page loads. What you typed is kept.")).toBeInTheDocument();
  expect(screen.queryByLabelText("Title")).not.toBeInTheDocument();
  vi.mocked(Groups.get).mockResolvedValue({ ...groupPage, upcomingEvents: [saved] });
  await userEvent.click(tryAgain);
  expect(await screen.findByLabelText("Title")).toHaveValue("Carols");
  expect(edits.sent).toHaveLength(1);
});

it("shows nothing of a date on the event page while its read after the group page's update failed, then opens it on the stored date", async () => {
  const put = deferred<Put>();
  edits.answer(put.promise);
  const saved = { ...date, title: "Carols" };
  const app = renderApp("/groups/1");
  await saveTitle(GROUP_MENU);
  await waitFor(() => expect(edits.sent).toHaveLength(1));
  await act(() => app.router.navigate("/events/7"));
  expect(await openMenu(EVENT_MENU)).toBeDisabled();
  await userEvent.keyboard("{Escape}");

  const reread = deferred<EventDetailResponse>();
  vi.mocked(Events.get).mockReturnValueOnce(reread.promise);
  await act(async () => settle(put, saved));
  await act(async () => reread.reject(new Error("offline")));
  const tryAgain = await screen.findByRole("button", { name: "Try again" });
  await userEvent.click(await editableMenu(EVENT_MENU));
  expect(await screen.findByText("This date can be edited again once the page loads. What you typed is kept.")).toBeInTheDocument();
  expect(screen.queryByLabelText("Title")).not.toBeInTheDocument();
  vi.mocked(Events.get).mockResolvedValue({ ...eventPage, occurrences: [saved] });
  await userEvent.click(tryAgain);
  expect(await screen.findByLabelText("Title")).toHaveValue("Carols");
  expect(edits.sent).toHaveLength(1);
});

it("drops a group Load more read before the event page's update answered, and reads that page again from fresh cursors", async () => {
  const put = deferred<Put>();
  edits.answer(put.promise);
  const saved = { ...date, title: "Carols" };
  // The group's first page leaves out the edited series; Load more brings it, read before the update was stored.
  const firstPage: GroupDetailResponse = { ...groupPage, upcomingEvents: [picnic], hasMore: true, nextCursor: "c1" };
  const staleMore = deferred<GroupDetailResponse>();

  vi.mocked(Groups.get)
    .mockResolvedValueOnce(firstPage)
    .mockReturnValueOnce(staleMore.promise)
    .mockResolvedValueOnce({ ...firstPage, nextCursor: "c2" })
    .mockResolvedValueOnce({ ...groupPage, upcomingEvents: [saved], hasMore: false, nextCursor: "" });

  const app = renderApp("/events/7");
  await saveTitle(EVENT_MENU);
  await act(() => app.router.navigate("/groups/1"));
  await userEvent.click(await screen.findByRole("button", { name: "Load more dates" }));
  await waitFor(() => expect(Groups.get).toHaveBeenCalledWith("1", { cursor: "c1" }));

  await act(async () => settle(put, saved));
  await waitFor(() => expect(Groups.get).toHaveBeenCalledWith("1", { cursor: "c2" }));
  expect(await screen.findByRole("button", { name: "More for Carols on 2026-12-01" })).toBeInTheDocument();

  await act(async () => staleMore.resolve({ ...groupPage, upcomingEvents: [date], hasMore: false, nextCursor: "" }));
  expect(screen.queryByRole("button", { name: GROUP_MENU })).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: "More for Picnic on 2026-11-28" })).toBeInTheDocument();
  await userEvent.click(await editableMenu("More for Carols on 2026-12-01"));
  expect(await screen.findByLabelText("Title")).toHaveValue("Carols");
  expect(Groups.get).toHaveBeenCalledTimes(4);
  expect(edits.sent).toHaveLength(1);
});

it("keeps a group date closed when the group's first read was answered before the event page's update, even if it is shown after", async () => {
  const put = deferred<Put>();
  edits.answer(put.promise);
  const saved = { ...date, title: "Carols" };
  const first = deferred<GroupDetailResponse>();
  const reread = deferred<GroupDetailResponse>();
  vi.mocked(Groups.get).mockReturnValueOnce(first.promise).mockReturnValueOnce(reread.promise);
  const app = renderApp("/events/7");
  await saveTitle(EVENT_MENU);
  await act(() => app.router.navigate("/groups/1"));
  await waitFor(() => expect(Groups.get).toHaveBeenCalledOnce());

  await act(async () => {
    first.resolve(groupPage);
    await answersRun();
    settle(put, saved);
  });
  expect(Groups.get).toHaveBeenCalledTimes(2);
  // The old row opens the stored date: the editor's base is its own read, never the row.
  await userEvent.click(await editableMenu(GROUP_MENU));
  expect(await screen.findByLabelText("Title")).toHaveValue("Carols");
  await userEvent.click(screen.getByRole("button", { name: "Close" }));

  await act(async () => reread.resolve({ ...groupPage, upcomingEvents: [saved] }));
  await userEvent.click(await editableMenu("More for Carols on 2026-12-01"));
  expect(await screen.findByLabelText("Title")).toHaveValue("Carols");
  expect(edits.sent).toHaveLength(1);
});

it.each([
  ["shown before the update answers", true],
  ["in but not shown yet as the update answers", false],
] as const)("keeps an old group row from Load more closed when that read is %s, then reads both pages again from fresh cursors", async (_label, shownFirst) => {
  const put = deferred<Put>();
  edits.answer(put.promise);
  const saved = { ...date, title: "Carols" };
  const firstPage: GroupDetailResponse = { ...groupPage, upcomingEvents: [picnic], hasMore: true, nextCursor: "c1" };
  const staleMore = deferred<GroupDetailResponse>();
  const reread = deferred<GroupDetailResponse>();

  vi.mocked(Groups.get)
    .mockResolvedValueOnce(firstPage)
    .mockReturnValueOnce(staleMore.promise)
    .mockReturnValueOnce(reread.promise)
    .mockResolvedValueOnce({ ...groupPage, upcomingEvents: [saved], hasMore: false, nextCursor: "" });

  const app = renderApp("/events/7");
  await saveTitle(EVENT_MENU);
  await act(() => app.router.navigate("/groups/1"));
  await userEvent.click(await screen.findByRole("button", { name: "Load more dates" }));
  await waitFor(() => expect(Groups.get).toHaveBeenCalledWith("1", { cursor: "c1" }));
  const stale: GroupDetailResponse = { ...groupPage, upcomingEvents: [date], hasMore: false, nextCursor: "" };

  if (shownFirst) {
    await act(async () => staleMore.resolve(stale));
    expect(await openMenu(GROUP_MENU)).toBeDisabled();
    await userEvent.keyboard("{Escape}");
    await act(async () => settle(put, saved));
  } else {
    await act(async () => {
      staleMore.resolve(stale);
      await answersRun();
      settle(put, saved);
    });
  }

  // The old row opens the stored date from its own read while the group reads its first page again, then the page past it.
  await userEvent.click(await editableMenu(GROUP_MENU));
  expect(await screen.findByLabelText("Title")).toHaveValue("Carols");
  await userEvent.click(screen.getByRole("button", { name: "Close" }));
  await act(async () => reread.resolve({ ...firstPage, nextCursor: "c2" }));
  await waitFor(() => expect(Groups.get).toHaveBeenLastCalledWith("1", { cursor: "c2" }));
  await userEvent.click(await editableMenu("More for Carols on 2026-12-01"));
  expect(await screen.findByLabelText("Title")).toHaveValue("Carols");
  expect(screen.getByRole("button", { name: "More for Picnic on 2026-11-28" })).toBeInTheDocument();
  expect(Groups.get).toHaveBeenCalledTimes(4);
  expect(edits.sent).toHaveLength(1);
});

it.each([
  ["the group's read lands first", true],
  ["Load more lands first", false],
] as const)("never leaves a group date stuck or stale when Load more is asked for during the read after the event page's update: %s", async (_label, rereadFirst) => {
  const put = deferred<Put>();
  edits.answer(put.promise);
  const saved = { ...date, title: "Carols" };
  const firstPage: GroupDetailResponse = { ...groupPage, upcomingEvents: [picnic], hasMore: true, nextCursor: "c1" };
  const more: GroupDetailResponse = { ...groupPage, upcomingEvents: [saved], hasMore: false, nextCursor: "" };
  const reread = deferred<GroupDetailResponse>();
  const moreRead = deferred<GroupDetailResponse>();
  vi.mocked(Groups.get).mockResolvedValueOnce(firstPage).mockReturnValueOnce(reread.promise).mockReturnValueOnce(moreRead.promise).mockResolvedValue(more);
  const app = renderApp("/events/7");
  await saveTitle(EVENT_MENU);
  await act(() => app.router.navigate("/groups/1"));
  await screen.findByRole("button", { name: "More for Picnic on 2026-11-28" });
  await act(async () => settle(put, saved));
  expect(Groups.get).toHaveBeenCalledTimes(2);
  await userEvent.click(screen.getByRole("button", { name: "Load more dates" }));
  expect(Groups.get).toHaveBeenLastCalledWith("1", { cursor: "c1" });

  // The fresh first page ends on the same date, so it carries the same cursor.
  await act(async () => {
    if (rereadFirst) {
      reread.resolve(firstPage);
      await answersRun();
      moreRead.resolve(more);
    } else {
      moreRead.resolve(more);
      await answersRun();
      reread.resolve(firstPage);
    }
  });

  // Load more was asked from the answer the fresh first page replaces, so it never extends that page, whichever
  // lands first (even with the same cursor); asking again brings the date back.
  expect(screen.queryByRole("button", { name: "More for Carols on 2026-12-01" })).not.toBeInTheDocument();
  await userEvent.click(screen.getByRole("button", { name: "Load more dates" }));

  expect(screen.queryByRole("button", { name: GROUP_MENU })).not.toBeInTheDocument();
  await userEvent.click(await editableMenu("More for Carols on 2026-12-01"));
  expect(await screen.findByLabelText("Title")).toHaveValue("Carols");
  expect(edits.sent).toHaveLength(1);
});

it("keeps the event page's date closed after an update whose answer was lost, offers the same bytes once, and a later save sends only what changed", async () => {
  edits.answer(new TypeError("Failed to fetch"));
  renderApp("/events/7");
  await screen.findByRole("button", { name: EVENT_MENU });
  const loads = vi.mocked(Events.get).mock.calls.length;
  const reread = deferred<EventDetailResponse>();
  vi.mocked(Events.get).mockReturnValueOnce(reread.promise);
  await saveTitle(EVENT_MENU);

  // The update may have been stored: nothing can be saved, and the page is asked again.
  await waitFor(() => expect(screen.getByText(UNSURE)).toBeInTheDocument());
  expect(Events.get).toHaveBeenCalledTimes(loads + 1);
  expect(screen.getByLabelText("Title")).toBeDisabled();
  expect(screen.getByRole("button", { name: "Save this date" })).toBeDisabled();
  expect(screen.getByRole("button", { name: "Close" })).toBeEnabled();

  await act(async () => reread.reject(new Error("offline")));
  await userEvent.click(await screen.findByRole("button", { name: "Try again" }));
  // The page's answer reads the date again; only the same bytes can go, once.
  const again = await screen.findByRole("button", { name: "Try again" });
  expect(screen.getByText("Title: Carols")).toBeInTheDocument();
  await waitFor(() => expect(again).toBeEnabled());
  stored.set(keyOf(date), { ...date, title: "Carols" });
  await userEvent.click(again);
  await waitFor(() => expect(edits.sent).toHaveLength(2));
  expect(edits.sent[1].body).toBe(edits.sent[0].body);
  // Stored: the editor closes, and opens again on the stored date.
  await waitFor(() => expect(screen.queryByLabelText("Title")).not.toBeInTheDocument());
  await userEvent.click(await editableMenu(EVENT_MENU));
  expect(await screen.findByLabelText("Title")).toHaveValue("Carols");
  await waitFor(() => expect(screen.getByRole("button", { name: "Save this date" })).toBeEnabled());
  await userEvent.type(screen.getByLabelText(/^Capacity for this date/), "12");
  await userEvent.click(screen.getByRole("button", { name: "Save this date" }));
  await waitFor(() => expect(edits.sent).toHaveLength(3));
  expect(edits.sent.map((sent) => sent.changes)).toEqual([{ title: "Carols" }, { title: "Carols" }, { capacityOverride: 12 }]);
});

it("recovers an event page save whose answer was lost from the group page's own opener, with the same bytes once", async () => {
  edits.answer(new TypeError("Failed to fetch"));
  const app = renderApp("/events/7");
  await saveTitle(EVENT_MENU);
  await waitFor(() => expect(screen.getByText(UNSURE)).toBeInTheDocument());

  // On the group page the date's Edit stays closed; its own opener reads the date and offers only the same bytes.
  await act(() => app.router.navigate("/groups/1"));
  expect(await openMenu(GROUP_MENU)).toBeDisabled();
  await userEvent.keyboard("{Escape}");
  await userEvent.click(await screen.findByRole("button", { name: "Check your last change to this date" }));
  const again = await screen.findByRole("button", { name: "Try again" });
  expect(screen.getByText("Title: Carols")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Save this date" })).toBeDisabled();
  await waitFor(() => expect(again).toBeEnabled());
  stored.set(keyOf(date), { ...date, title: "Carols" });
  await userEvent.click(again);
  await waitFor(() => expect(edits.sent).toHaveLength(2));
  expect(edits.sent[1].body).toBe(edits.sent[0].body);
  await waitFor(() => expect(screen.queryByRole("button", { name: "Check your last change to this date" })).not.toBeInTheDocument());
});

it("frees the date on the group page with no second read when the event page's update is refused", async () => {
  const put = deferred<Put>();
  edits.answer(put.promise);
  const app = renderApp("/events/7");
  await saveTitle(EVENT_MENU);
  await act(() => app.router.navigate("/groups/1"));
  expect(await openMenu(GROUP_MENU)).toBeDisabled();
  await userEvent.keyboard("{Escape}");
  const groupLoads = vi.mocked(Groups.get).mock.calls.length;

  await act(async () => put.reject(new ApiError(409, "changed", { code: "conflict" })));
  expect(await editableMenu(GROUP_MENU)).toBeEnabled();
  expect(Groups.get).toHaveBeenCalledTimes(groupLoads);
  expect(edits.sent).toHaveLength(1);
});

it("catches the group page's editor, still open after its own save, up to the stored date, so a later save sends only what changed", async () => {
  // Another manager moved the date while the title was being saved.
  const moved = { ...date, title: "Carols", location: "Annex" };
  const put = deferred<Put>();
  edits.answer(put.promise);
  vi.mocked(Groups.get).mockResolvedValueOnce(groupPage).mockResolvedValue({ ...groupPage, upcomingEvents: [moved] });
  renderApp("/groups/1");
  await saveTitle(GROUP_MENU);
  await waitFor(() => expect(edits.sent).toHaveLength(1));
  await act(async () => settle(put, moved));

  await waitFor(() => expect(screen.getByRole("textbox", { name: /^Location/ })).toHaveValue("Annex"));
  expect(screen.getByLabelText("Title")).toHaveValue("Carols");
  await waitFor(() => expect(screen.getByRole("button", { name: "Save this date" })).toBeEnabled());
  await userEvent.type(screen.getByLabelText(/^Capacity for this date/), "12");
  await userEvent.click(screen.getByRole("button", { name: "Save this date" }));
  await waitFor(() => expect(edits.sent).toHaveLength(2));
  expect(edits.sent.map((sent) => sent.changes)).toEqual([{ title: "Carols" }, { capacityOverride: 12 }]);
});

it("keeps what was typed in another date's editor on the group page when the event page's update answers and the group reads again", async () => {
  const put = deferred<Put>();
  edits.answer(put.promise);
  const saved = { ...date, title: "Carols" };
  const later = { ...picnic, date: "2026-12-08", startsAt: "2026-12-08T19:00:00Z", endsAt: "2026-12-08T20:00:00Z", startsLocal: "2026-12-08T19:00" };
  stored.set(keyOf(later), later);
  vi.mocked(Groups.get).mockResolvedValueOnce({ ...groupPage, upcomingEvents: [date, later] }).mockResolvedValue({ ...groupPage, upcomingEvents: [saved, later] });
  const app = renderApp("/events/7");
  await saveTitle(EVENT_MENU);
  await act(() => app.router.navigate("/groups/1"));
  await userEvent.click(await openMenu("More for Picnic on 2026-12-08"));
  const title = await screen.findByLabelText("Title");
  await userEvent.clear(title);
  await userEvent.type(title, "Potluck");

  // The group reads again, so that editor reads its date again and keeps what was typed.
  await act(async () => settle(put, saved));
  expect(await screen.findByRole("button", { name: "More for Carols on 2026-12-01" })).toBeInTheDocument();
  await waitFor(() => expect(screen.getByRole("button", { name: "Save this date" })).toBeEnabled());
  expect(screen.getByLabelText("Title")).toHaveValue("Potluck");
  expect(screen.getByLabelText("Title")).toBeEnabled();
});

it.each([
  ["someone else signs in", [{ id: 3 }]],
  ["the person signs out and back in", [null, { id: 2 }]],
] as const)("starts with no held dates when %s, and an earlier person's late update changes nothing", async (_label, sessions) => {
  const put = deferred<Put>();
  edits.answer(put.promise);
  const app = renderApp("/events/7");
  await saveTitle(EVENT_MENU);
  expect(await openMenu(EVENT_MENU)).toBeDisabled();
  await userEvent.keyboard("{Escape}");

  // Each sign-in is a new browser credential, as AuthProvider adopts it.
  for (const user of sessions) {
    session.user = user;
    session.credential = user ? await verifiedCredential(user.id) : null;
    app.rerender();
  }

  expect(await openMenu(EVENT_MENU)).toBeEnabled();
  await userEvent.keyboard("{Escape}");
  const loads = vi.mocked(Events.get).mock.calls.length;

  await act(async () => settle(put, date));
  expect(Events.get).toHaveBeenCalledTimes(loads);
  expect(await openMenu(EVENT_MENU)).toBeEnabled();
  expect(edits.sent).toHaveLength(1);
});
