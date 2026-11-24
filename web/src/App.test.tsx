import { renderWithAuth } from "./test/auth";
import { stubService } from "./test/service";
import { act, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { RouterProvider, createMemoryRouter } from "react-router-dom";
import { beforeAll, beforeEach, expect, it, vi } from "vitest";
import type { Credential } from "./api/core";
import { Events } from "./api/events";
import { Groups } from "./api/groups";
import { Households } from "./api/households";
import { Notifications } from "./api/notifications";
import { Site } from "./api/site";
import type { EventDetailResponse, GroupDetailResponse, OccurrenceResponse, User } from "./types";
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

// One date both pages show and a leader may edit from either.
const date = occurrence({ groupId: 1, groupName: "Family group", groupVisibility: "public" });
const eventPage: EventDetailResponse = { event: { ...testEvent(7), title: "Supper" }, occurrences: [date], canManage: true, canSetVisibility: true };
const groupPage: GroupDetailResponse = { ...testGroupDetail({ id: 1, name: "Family group" }), upcomingEvents: [date] };

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((accept) => { resolve = accept; });

  return { promise, resolve };
}

let edits: EditSends;

// The production tree: App under the one splat route main.tsx gives it.
function renderApp(path: string) {
  const router = createMemoryRouter([{ path: "*", element: <App /> }], { initialEntries: [path] });
  render(<RouterProvider router={router} />);

  return router;
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
  vi.mocked(Events.managedDate).mockResolvedValue({ occurrence: { ...date, revision: "r0" } });
  vi.mocked(Site.get).mockResolvedValue({ name: "Fold", timezone: "UTC", selfSignup: "off", roomsEnabled: false, pushEnabled: false });
  vi.mocked(Notifications.badge).mockResolvedValue(null);
  vi.mocked(Households.mine).mockResolvedValue({ household: null });
  vi.mocked(Groups.get).mockResolvedValue(groupPage);
  vi.mocked(Events.get).mockResolvedValue(eventPage);
});

// DEC-164: a date's save belongs to the editor that pressed Save. Leaving its page while it is on its way, nothing
// follows the person: the other page opens the same date at once, from its own read, and the late answer asks
// nothing and changes nothing there.
it("lets the group page edit a date at once while the event page's save of it is still on its way", async () => {
  const put = deferred<OccurrenceResponse>();
  edits.answer(put.promise);
  const router = renderApp("/events/7");
  await userEvent.click(await screen.findByRole("button", { name: EVENT_MENU }));
  await userEvent.click(screen.getByRole("menuitem", { name: "Edit this date" }));
  const title = await screen.findByLabelText("Title");
  await userEvent.clear(title);
  await userEvent.type(title, "Carols");
  await waitFor(() => expect(screen.getByRole("button", { name: "Save this date" })).toBeEnabled());
  await userEvent.click(screen.getByRole("button", { name: "Save this date" }));
  await waitFor(() => expect(edits.sent).toHaveLength(1));

  await act(() => router.navigate("/groups/1"));
  await userEvent.click(await screen.findByRole("button", { name: GROUP_MENU }));
  await userEvent.click(screen.getByRole("menuitem", { name: "Edit this date" }));
  expect(await screen.findByLabelText("Title")).toHaveValue("Supper");
  await waitFor(() => expect(screen.getByRole("button", { name: "Save this date" })).toBeEnabled());
  const groupReads = vi.mocked(Groups.get).mock.calls.length;

  await act(async () => put.resolve({ occurrence: { ...date, title: "Carols", revision: "r1" } }));
  expect(screen.getByLabelText("Title")).toHaveValue("Supper");
  expect(screen.getByRole("button", { name: "Save this date" })).toBeEnabled();
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  expect(vi.mocked(Groups.get).mock.calls.length).toBe(groupReads);
  expect(screen.queryByText(/Check your last change|couldn't tell whether/)).not.toBeInTheDocument();
  expect(edits.sent).toHaveLength(1);
});
