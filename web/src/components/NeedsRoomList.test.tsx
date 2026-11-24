import { stubService } from "../test/service";
import { renderWithAuth } from "../test/auth";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, expect, it, vi } from "vitest";
import { ApiError } from "../api/core";
import { DateRooms, SeriesRooms } from "../api/rooms";
import { Site } from "../api/site";
import type { NeedsRoom, RoomPickerItem } from "../types";
import { NeedsRoomList } from "./NeedsRoomList";

stubService(Site, () => ({ get: vi.fn() }));

stubService(SeriesRooms, () => ({ list: vi.fn() }));

stubService(DateRooms, () => ({ list: vi.fn(), set: vi.fn() }));

const render = renderWithAuth(() => ({ isAdmin: false }));

const hall: RoomPickerItem = { id: 1, name: "Fellowship Hall", building: "Main", capacity: 80, features: "", restricted: false, busy: false, tooSmall: false };
const needs: NeedsRoom[] = [{ date: "2026-12-08", roomName: "Fellowship Hall", status: "requested" }, { date: "2026-12-15", roomName: "Chapel", status: "requested" }];

beforeEach(() => {
  vi.resetAllMocks();
  vi.mocked(Site.get).mockResolvedValue({ name: "Fold", timezone: "UTC", selfSignup: "off", roomsEnabled: true, pushEnabled: true });
  vi.mocked(SeriesRooms.list).mockResolvedValue({ revision: "r1", rooms: [], dates: 10, defaultRoom: null, needsRoom: needs, pending: 0 });
});

function show(props: Partial<Parameters<typeof NeedsRoomList>[0]> = {}) {
  const onPick = vi.fn();

  const view = render(
    <MemoryRouter>
      <NeedsRoomList eventId={7} revision={0} hasDate={() => true} onPick={onPick} onGone={vi.fn()} {...props} />
    </MemoryRouter>,
  );

  return { onPick, ...view };
}

it("lists each date with the room it lost and a Pick a room action", async () => {
  const { onPick } = show();
  expect(await screen.findByRole("heading", { name: /Dates that need a room/ })).toBeInTheDocument();
  expect(SeriesRooms.list).toHaveBeenCalledWith(7);
  expect(screen.getByText("Tuesday, December 8")).toBeInTheDocument();
  expect(screen.getByText("Fellowship Hall isn't free then.")).toBeInTheDocument();
  expect(screen.getByText("Chapel isn't free then.")).toBeInTheDocument();
  await userEvent.click(screen.getByRole("button", { name: "Pick a room for Tuesday, December 15" }));
  expect(onPick).toHaveBeenCalledWith("2026-12-15");
});

it("opens the one-date Room control under a date the page doesn't list, and not for one it does", async () => {
  vi.mocked(DateRooms.list).mockResolvedValue({ rooms: [hall], booking: null, dateRoom: { id: 70, revision: "r1" } });
  const { onPick } = show({ hasDate: (d) => d === "2026-12-08" });
  const far = await screen.findByRole("button", { name: "Pick a room for Tuesday, December 15" });
  expect(screen.queryByRole("link")).not.toBeInTheDocument();
  expect(far).toHaveAttribute("aria-expanded", "false");
  await userEvent.click(far);
  expect(far).toHaveAttribute("aria-expanded", "true");
  const room = await screen.findByRole("combobox", { name: /^Room/ });
  expect(DateRooms.list).toHaveBeenCalledWith(7, "2026-12-15");
  await waitFor(() => expect(room).toHaveFocus());
  expect(onPick).not.toHaveBeenCalled();
  // A date the page lists opens its own editor instead, with no control here.
  const near = screen.getByRole("button", { name: "Pick a room for Tuesday, December 8" });
  expect(near).not.toHaveAttribute("aria-expanded");
  await userEvent.click(near);
  expect(onPick).toHaveBeenCalledWith("2026-12-08");
  // Closing it again.
  await userEvent.click(far);
  expect(screen.queryByRole("combobox", { name: /^Room/ })).not.toBeInTheDocument();
});

it("asks again after the inline control books, and says what it booked", async () => {
  vi.mocked(DateRooms.list).mockResolvedValue({ rooms: [hall], booking: null, dateRoom: { id: 70, revision: "r1" } });
  vi.mocked(DateRooms.set).mockResolvedValue({ booking: { roomId: 1, roomName: "Fellowship Hall", building: "Main", status: "confirmed", setupMinutes: 0, teardownMinutes: 0, doubleBooked: false, tooSmall: false, pending: false }, dateRoom: { id: 70, revision: "r2" } });
  show({ hasDate: () => false });
  await userEvent.click(await screen.findByRole("button", { name: "Pick a room for Tuesday, December 15" }));
  vi.mocked(SeriesRooms.list).mockResolvedValue({ revision: "r1", rooms: [], dates: 10, defaultRoom: null, needsRoom: [needs[0]], pending: 0 });
  await userEvent.selectOptions(await screen.findByRole("combobox", { name: /^Room/ }), "1");
  await waitFor(() => expect(screen.queryByText("Chapel isn't free then.")).not.toBeInTheDocument());
  expect(SeriesRooms.list).toHaveBeenCalledTimes(2);
  expect(await screen.findByText("Booked Fellowship Hall for Tuesday, December 15.")).toBeInTheDocument();
});

// A pick waiting for an admin leaves the list too (waiting dates are not listed): it says so, never that the date has
// a room.
it("says a pick that waits for an admin's approval is waiting, not that the date has a room", async () => {
  vi.mocked(DateRooms.list).mockResolvedValue({ rooms: [hall], booking: null, dateRoom: { id: 70, revision: "r1" } });
  vi.mocked(DateRooms.set).mockResolvedValue({ booking: { roomId: 1, roomName: "Fellowship Hall", building: "Main", status: "requested", setupMinutes: 0, teardownMinutes: 0, doubleBooked: false, tooSmall: false, pending: true }, dateRoom: { id: 70, revision: "r2" } });
  show({ hasDate: () => false });
  await userEvent.click(await screen.findByRole("button", { name: "Pick a room for Tuesday, December 15" }));
  vi.mocked(SeriesRooms.list).mockResolvedValue({ revision: "r1", rooms: [], dates: 10, defaultRoom: null, needsRoom: [needs[0]], pending: 1 });
  await userEvent.selectOptions(await screen.findByRole("combobox", { name: /^Room/ }), "1");
  expect(await screen.findByText("Tuesday, December 15 is waiting for an admin to approve Fellowship Hall.")).toBeInTheDocument();
  expect(screen.queryByText(/has a room|Booked/)).not.toBeInTheDocument();
});

// A pick refused because someone else gave the date a room: the list asks again and the date leaves it, saying the
// choice could not be saved, never that the date has the room this person picked.
it("asks again after the inline control's pick is refused, and says the choice could not be saved", async () => {
  const chapel: RoomPickerItem = { ...hall, id: 2, name: "Chapel", building: "" };
  vi.mocked(DateRooms.list)
    .mockResolvedValueOnce({ rooms: [hall, chapel], booking: null, dateRoom: { id: 70, revision: "r1" } })
    .mockResolvedValue({ rooms: [hall, chapel], dateRoom: { id: 70, revision: "r3" },
      booking: { roomId: 2, roomName: "Chapel", building: "", status: "confirmed", setupMinutes: 0, teardownMinutes: 0, doubleBooked: false, tooSmall: false, pending: false } });
  vi.mocked(DateRooms.set).mockRejectedValue(new ApiError(409, "changed", { code: "date_room_changed" }));
  show({ hasDate: () => false });
  await userEvent.click(await screen.findByRole("button", { name: "Pick a room for Tuesday, December 15" }));
  vi.mocked(SeriesRooms.list).mockResolvedValue({ revision: "r1", rooms: [], dates: 10, defaultRoom: null, needsRoom: [needs[0]], pending: 0 });
  await userEvent.selectOptions(await screen.findByRole("combobox", { name: /^Room/ }), "1");
  expect(await screen.findByText("Tuesday, December 15. This date's room changed while you were choosing, so your choice could not be saved. It is no longer listed under “Dates that need a room”.")).toBeInTheDocument();
  expect(SeriesRooms.list).toHaveBeenCalledTimes(2);
  expect(screen.queryByText(/has a room|Booked/)).not.toBeInTheDocument();
});

it("shows nothing when rooms were turned off after the page loaded", async () => {
  vi.mocked(SeriesRooms.list).mockRejectedValue(new ApiError(404, "off", { code: "rooms_off" }));
  show();
  await waitFor(() => expect(SeriesRooms.list).toHaveBeenCalled());
  expect(screen.queryByRole("heading", { name: /Dates that need a room/ })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Try again" })).not.toBeInTheDocument();
});

it("shows the first 10 dates with the count in the heading, and the rest on request", async () => {
  const many: NeedsRoom[] = Array.from({ length: 14 }, (_, i) => ({ date: `2027-01-${`${i + 1}`.padStart(2, "0")}`, roomName: "Chapel", status: "requested" }));
  vi.mocked(SeriesRooms.list).mockResolvedValue({ revision: "r1", rooms: [], dates: 30, defaultRoom: null, needsRoom: many, pending: 0 });
  show();
  expect(await screen.findByRole("heading", { name: /Dates that need a room \(14\)/ })).toBeInTheDocument();
  expect(screen.getAllByRole("button", { name: /^Pick a room/ })).toHaveLength(10);
  await userEvent.click(screen.getByRole("button", { name: "Show all 14" }));
  expect(screen.getAllByRole("button", { name: /^Pick a room/ })).toHaveLength(14);
  expect(screen.queryByRole("button", { name: /^Show all/ })).not.toBeInTheDocument();
});

it("shows nothing when every date has its room", async () => {
  vi.mocked(SeriesRooms.list).mockResolvedValue({ revision: "r1", rooms: [], dates: 10, defaultRoom: null, needsRoom: [], pending: 0 });
  const { container } = show();
  await waitFor(() => expect(SeriesRooms.list).toHaveBeenCalled());
  expect(container).toBeEmptyDOMElement();
});

it("shows nothing, and asks for no rooms, while rooms are off", async () => {
  vi.mocked(Site.get).mockResolvedValue({ name: "Fold", timezone: "UTC", selfSignup: "off", roomsEnabled: false, pushEnabled: true });
  const { container } = show();
  await waitFor(() => expect(Site.get).toHaveBeenCalled());
  expect(container).toBeEmptyDOMElement();
  expect(SeriesRooms.list).not.toHaveBeenCalled();
});

it("asks again when a room changed, and drops a date that now has one", async () => {
  const { rerender } = show();
  await screen.findByText("Chapel isn't free then.");
  vi.mocked(SeriesRooms.list).mockResolvedValue({ revision: "r1", rooms: [], dates: 10, defaultRoom: null, needsRoom: [needs[0]], pending: 0 });
  rerender(<MemoryRouter><NeedsRoomList eventId={7} revision={1} hasDate={() => true} onPick={() => {}} onGone={vi.fn()} /></MemoryRouter>);
  await waitFor(() => expect(screen.queryByText("Chapel isn't free then.")).not.toBeInTheDocument());
  expect(screen.getByText("Fellowship Hall isn't free then.")).toBeInTheDocument();
});

it("offers a retry when the list fails to load", async () => {
  vi.mocked(SeriesRooms.list).mockRejectedValueOnce(new ApiError(500, "down", { code: "server_error" }));
  show();
  await userEvent.click(await screen.findByRole("button", { name: "Try again" }));
  expect(await screen.findByText("Chapel isn't free then.")).toBeInTheDocument();
});

it("words a date an admin didn't approve differently from one that lost its room", async () => {
  vi.mocked(SeriesRooms.list).mockResolvedValue({
    revision: "r1",
    rooms: [], dates: 10, defaultRoom: null, pending: 0,
    needsRoom: [{ date: "2026-12-08", roomName: "Fellowship Hall", status: "rejected" }, { date: "2026-12-15", roomName: "Chapel", status: "requested" }],
  });
  show();
  expect(await screen.findByRole("heading", { name: /^⚠ Dates that need a room|^Dates that need a room/ })).toBeInTheDocument();
  expect(screen.getByText("An admin didn't approve Fellowship Hall for this date.")).toBeInTheDocument();
  expect(screen.getByText("Chapel isn't free then.")).toBeInTheDocument();
});
