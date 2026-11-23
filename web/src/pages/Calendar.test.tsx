import { renderWithAuth } from "../test/auth";
import { stubService } from "../test/service";
import { Calendar } from "../api/calendar";
import { Groups } from "../api/groups";
import { Site } from "../api/site";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { CalendarOccurrence } from "../types";
import { CalendarPage } from "./Calendar";
import { addDays } from "../lib/days";
import { weekRange } from "../lib/calendar";
import { dayOf, formatDayHeading } from "../lib/when";

const { list, groups, auth, site } = vi.hoisted(() => ({ list: vi.fn(), groups: vi.fn(), auth: vi.fn(), site: vi.fn() }));

const render = renderWithAuth(auth);

stubService(Calendar, () => ({ list }));

stubService(Groups, () => ({ list: groups }));

stubService(Site, () => ({ get: site }));

const occ = (eventId: number, o: Partial<CalendarOccurrence> = {}): CalendarOccurrence => ({
  eventId, date: "2026-10-21", startsAt: "2026-10-21T18:00:00Z", endsAt: "2026-10-21T19:00:00Z", title: `Event ${eventId}`,
  location: "", churchWide: true, remoteAvailable: false, childcareAvailable: false, myStatus: null, ownScope: false, ...o,
});

const show = (occurrences: CalendarOccurrence[], path = "/calendar") => {
  list.mockResolvedValue({ occurrences, truncated: false });

  return render(<MemoryRouter initialEntries={[path]}><CalendarPage /></MemoryRouter>);
};

beforeEach(() => {
  list.mockReset();
  groups.mockReset();
  groups.mockResolvedValue({ groups: [] });
  site.mockReset();
  site.mockResolvedValue({ roomsEnabled: false });
  auth.mockReturnValue({ user: { id: 1 }, isAdmin: false });
});

it("bounds a busy agenda and preserves a URL page without refetching", async () => {
  show(Array.from({ length: 60 }, (_, i) => occ(i + 1)), "/calendar?date=2026-10-21&page=2&church=1");
  expect(await screen.findByText("Event 26")).toBeInTheDocument();
  expect(screen.queryByText("Event 1")).not.toBeInTheDocument();
  expect(screen.queryByText("Event 51")).not.toBeInTheDocument();
  expect(list).toHaveBeenCalledWith(expect.objectContaining({ church: true }));
  await userEvent.click(screen.getByRole("button", { name: "Next events" }));
  expect(screen.getByText("Event 60")).toBeInTheDocument();
  expect(list).toHaveBeenCalledTimes(1);
  await userEvent.click(screen.getByRole("button", { name: "Filters" }));
  await userEvent.selectOptions(screen.getByRole("combobox"), "");
  expect(await screen.findByText("Event 1")).toBeInTheDocument();
  expect(list).toHaveBeenLastCalledWith(expect.objectContaining({ church: false }));
});

it("uses a safe first page for a malformed or negative page", async () => {
  show(Array.from({ length: 30 }, (_, i) => occ(i + 1)), "/calendar?date=2026-10-21&page=-4");
  expect(await screen.findByText("Event 1")).toBeInTheDocument();
  expect(screen.queryByText("Event 26")).not.toBeInTheDocument();
});

describe("filter chips (DEC-99)", () => {
  it("hides Remote seats and Childcare when nothing in view has them", async () => {
    show([occ(1)]);
    await screen.findByText("Event 1");
    await userEvent.click(screen.getByRole("button", { name: "Filters" }));
    expect(screen.getByRole("button", { name: "Just mine" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Remote seats" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Childcare" })).not.toBeInTheDocument();
  });

  it("shows each chip only when something in view has that", async () => {
    show([occ(1, { remoteAvailable: true }), occ(2)]);
    await userEvent.click(screen.getByRole("button", { name: "Filters" }));
    expect(await screen.findByRole("button", { name: "Remote seats" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Childcare" })).not.toBeInTheDocument();
  });

  it("shows Childcare when a date offers it", async () => {
    show([occ(1, { childcareAvailable: true })]);
    await userEvent.click(screen.getByRole("button", { name: "Filters" }));
    expect(await screen.findByRole("button", { name: "Childcare" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Remote seats" })).not.toBeInTheDocument();
  });

  it("keeps a selected chip even when nothing in view has it, so it can be turned off", async () => {
    show([], "/calendar?remote=1&childcare=1");
    await userEvent.click(screen.getByRole("button", { name: "Filters" }));
    expect(await screen.findByRole("button", { name: "Remote seats", pressed: true })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Childcare", pressed: true })).toBeInTheDocument();
  });

  it("keeps Just mine and, for admins, Show deleted events", async () => {
    auth.mockReturnValue({ user: { id: 1 }, isAdmin: true });
    show([]);
    await waitFor(() => expect(list).toHaveBeenCalled());
    await userEvent.click(screen.getByRole("button", { name: "Filters" }));
    expect(screen.getByRole("button", { name: "Just mine" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Show deleted events" })).toBeInTheDocument();
  });
});

describe("Filters disclosure", () => {
  it("starts collapsed, reveals controls and returns focus when done", async () => {
    show([occ(1)]);
    await screen.findByText("Event 1");
    const filters = screen.getByRole("button", { name: "Filters" });
    expect(filters).toHaveAttribute("aria-expanded", "false");
    expect(screen.getByText("No filters")).toBeVisible();
    expect(screen.queryByRole("button", { name: "Just mine" })).not.toBeInTheDocument();
    expect(screen.queryByRole("combobox", { name: "Group" })).not.toBeInTheDocument();
    const panel = document.getElementById(filters.getAttribute("aria-controls")!)!;
    expect(panel).toHaveAttribute("hidden");
    await userEvent.click(filters);
    expect(filters).toHaveAttribute("aria-expanded", "true");
    expect(panel).not.toHaveAttribute("hidden");
    expect(screen.getByLabelText("From date")).toBeVisible();
    expect(screen.getByRole("combobox", { name: "Group" })).toBeVisible();
    await userEvent.click(screen.getByRole("button", { name: "Just mine" }));
    await waitFor(() => expect(list).toHaveBeenLastCalledWith(expect.objectContaining({ mine: true })));
    await userEvent.click(screen.getByRole("button", { name: "Done with filters" }));
    expect(filters).toHaveFocus();
    expect(filters).toHaveAttribute("aria-expanded", "false");
    expect(screen.getByTitle("Just mine")).toBeVisible();
  });

  it("summarizes active URL filters with the group's name", async () => {
    groups.mockResolvedValue({ groups: [{ id: 3, name: "Sunrise study" }] });
    show([], "/calendar?mine=1&childcare=1&group=3");
    expect(await screen.findByText("Just mine · Childcare · Sunrise study")).toBeVisible();
    await userEvent.click(screen.getByRole("button", { name: "Filters" }));
    expect(screen.getByRole("button", { name: "Just mine", pressed: true })).toBeVisible();
    expect(screen.getByRole("button", { name: "Childcare", pressed: true })).toBeVisible();
    expect(screen.getByRole("combobox", { name: "Group" })).toHaveValue("3");
    expect(list).toHaveBeenLastCalledWith(expect.objectContaining({ mine: true, childcare: true, group: "3" }));
  });

  it.each(["week", "month"])("keeps %s navigation outside the disclosure", async (view) => {
    show([], `/calendar?view=${view}&date=2026-10-21`);
    await waitFor(() => expect(list).toHaveBeenCalled());
    expect(screen.getByRole("button", { name: `Previous ${view}` })).toBeVisible();
    expect(screen.getByRole("button", { name: `Next ${view}` })).toBeVisible();
    expect(screen.getByRole("button", { name: "Today" })).toBeVisible();
    expect(screen.getByRole("button", { name: "Filters" })).toHaveAttribute("aria-expanded", "false");
  });
});

describe("week count line", () => {
  const week = "/calendar?view=week&date=2026-10-21";
  it.each([
    [[], "No events this week"],
    [[occ(1)], "1 event this week"],
    [[occ(1), occ(2, { date: "2026-10-22" })], "2 events this week"],
  ])("says %j as %s", async (occs, text) => {
    show(occs, week);
    expect(await screen.findByText(text)).toBeInTheDocument();
  });
});

describe("the Rooms view item (M2.2a)", () => {
  it("shows Rooms to signed-in adults when rooms are on", async () => {
    site.mockResolvedValue({ roomsEnabled: true });
    auth.mockReturnValue({ user: { id: 1, accountType: "adult" }, isAdmin: false });
    show([]);
    expect(await screen.findByRole("link", { name: "Rooms" })).toHaveAttribute("href", "/calendar/rooms");
  });

  it("hides Rooms when rooms are off", async () => {
    auth.mockReturnValue({ user: { id: 1, accountType: "adult" }, isAdmin: false });
    show([]);
    await waitFor(() => expect(site).toHaveBeenCalled());
    await screen.findByRole("link", { name: "Month" });
    expect(screen.queryByRole("link", { name: "Rooms" })).not.toBeInTheDocument();
  });

  it("hides Rooms from teens and signed-out visitors", async () => {
    site.mockResolvedValue({ roomsEnabled: true });
    auth.mockReturnValue({ user: { id: 1, accountType: "teen" }, isAdmin: false });
    const { unmount } = show([]);
    await screen.findByRole("link", { name: "Month" });
    expect(screen.queryByRole("link", { name: "Rooms" })).not.toBeInTheDocument();
    unmount();
    auth.mockReturnValue({ user: null, isAdmin: false });
    show([]);
    await screen.findByRole("link", { name: "Month" });
    expect(screen.queryByRole("link", { name: "Rooms" })).not.toBeInTheDocument();
  });
});

// W11 (design 8b-4): every row the calendar returns is on screen in every view. A row that starts before the
// grid's first day (DEC-157's carried kept date) shows above Week and Month under "Already underway".
describe("rows that started before the grid", () => {
  const zone = "America/Boise";
  // 13:00 or 14:00 in Boise on `day`.
  const at = (day: string) => `${day}T20:00:00Z`;
  const carried = (day: string) => occ(70, { date: day, startsAt: at(day), endsAt: at(addDays(day, 3)), title: "Carried retreat", dateUrl: `/events/70/dates/${day}`, churchWide: false, groupName: "Retreat team" });
  const onGrid = (day: string) => occ(71, { date: day, startsAt: at(day), endsAt: at(day).replace("T20", "T21"), title: "Grid supper", churchWide: false, groupName: "Supper club" });
  const section = () => screen.getByRole("region", { name: "Already underway" });
  const linksTo = (href: string) => screen.queryAllByRole("link").filter((a) => a.getAttribute("href") === href);

  it("(a) a future Week lists the carried row under its start day and counts it", async () => {
    const sunday = "2026-11-29";
    show([carried("2026-11-28")], "/calendar?view=week&date=2026-12-02");
    expect(await screen.findByText("Started before this week and still going.")).toBeInTheDocument();
    expect(within(section()).getByRole("heading", { level: 4, name: formatDayHeading("2026-11-28", zone) })).toBeInTheDocument();
    expect(within(section()).getByRole("link", { name: /Carried retreat/ })).toHaveAttribute("href", "/events/70/dates/2026-11-28");
    expect(screen.getByText("1 event this week")).toBeInTheDocument();
    expect(screen.queryByText("No events this week")).not.toBeInTheDocument();
    expect(list).toHaveBeenCalledWith(expect.objectContaining({ from: sunday, to: "2026-12-05", tz: zone }));
  });

  it("(b) the current Week does the same for a row that began last Saturday", async () => {
    const today = dayOf(new Date().toISOString(), zone);
    const saturday = addDays(weekRange(today)[0], -1);
    show([carried(saturday)], "/calendar?view=week");
    expect(await screen.findByRole("region", { name: "Already underway" })).toBeInTheDocument();
    expect(within(section()).getByRole("link", { name: /Carried retreat/ })).toBeInTheDocument();
    expect(screen.getByText("1 event this week")).toBeInTheDocument();
  });

  it("(c) a future Month lists it once, says this month, and keeps the selected day's empty line", async () => {
    show([carried("2026-12-26")], "/calendar?view=month&date=2027-01-15");
    expect(await screen.findByText("Started before this month and still going.")).toBeInTheDocument();
    expect(linksTo("/events/70/dates/2026-12-26")).toHaveLength(1);
    expect(screen.queryByText("No events this month.")).not.toBeInTheDocument();
    expect(screen.getByText("Nothing on this day.")).toBeInTheDocument();
  });

  it.each([["week", "2026-12-02", "2026-11-29"], ["month", "2027-01-15", "2026-12-27"]])("(d) in %s, the carried row is only in the section and a first-day row only on the grid", async (view, date, first) => {
    show([carried(addDays(first, -1)), onGrid(first)], `/calendar?view=${view}&date=${date}`);
    await screen.findByRole("region", { name: "Already underway" });
    expect(linksTo(`/events/70/dates/${addDays(first, -1)}`)).toHaveLength(1);
    expect(within(section()).queryByRole("link", { name: /Grid supper/ })).not.toBeInTheDocument();
    expect(screen.getAllByRole("link", { name: /Grid supper/ }).length).toBeGreaterThan(0);

    if (view === "week") expect(screen.getByText("2 events this week")).toBeInTheDocument();
  });

  it("(e) Agenda is unchanged: the carried row sits under its own day's heading and there is no section", async () => {
    const today = dayOf(new Date().toISOString(), zone);
    const yesterday = addDays(today, -1);
    show([carried(yesterday), onGrid(today)], `/calendar?date=${today}`);
    expect(await screen.findByRole("heading", { level: 2, name: formatDayHeading(yesterday, zone) })).toBeInTheDocument();
    expect(screen.queryByRole("region", { name: "Already underway" })).not.toBeInTheDocument();
  });

  it("(f) nothing carried, no section; an empty week still says so", async () => {
    const first = show([onGrid("2026-11-29")], "/calendar?view=week&date=2026-12-02");
    expect(await screen.findByText("1 event this week")).toBeInTheDocument();
    expect(screen.queryByRole("region", { name: "Already underway" })).not.toBeInTheDocument();
    first.unmount();
    show([], "/calendar?view=week&date=2026-12-02");
    expect(await screen.findByText("No events this week")).toBeInTheDocument();
    expect(screen.queryByRole("region", { name: "Already underway" })).not.toBeInTheDocument();
  });

  it("(g) the section's day headings are h4 under its h3, below the period's h2", async () => {
    show([carried("2026-11-28")], "/calendar?view=week&date=2026-12-02");
    await screen.findByRole("region", { name: "Already underway" });
    expect(within(section()).getByRole("heading", { level: 3, name: "Already underway" })).toBeInTheDocument();
    expect(within(section()).getAllByRole("heading", { level: 4 })).toHaveLength(1);
    expect(screen.getAllByRole("heading", { level: 2 }).length).toBeGreaterThan(0);
  });

  it("links a manager's row with dateUrl to the date page in every view", async () => {
    show([carried("2026-11-30")], "/calendar?view=week&date=2026-12-02");
    expect((await screen.findAllByRole("link", { name: /Carried retreat/ })).every((a) => a.getAttribute("href") === "/events/70/dates/2026-11-30")).toBe(true);
  });
});
