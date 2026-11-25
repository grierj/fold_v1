import { requiredElement } from "../test/dom";
import { renderWithAuth } from "../test/auth";
import { stubService } from "../test/service";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, expect, it, vi } from "vitest";
import { ApiError } from "../api/core";
import { SeriesRooms } from "../api/rooms";
import { Site } from "../api/site";
import type { OwnRoom, SeriesBooking, SeriesRoom, SeriesRoomsResponse } from "../types";
import { Form } from "./ui/Form";
import { SeriesRoomField } from "./SeriesRoomField";

const actor = { isAdmin: false };

const render = renderWithAuth(() => actor);

stubService(Site, () => ({ get: vi.fn() }));

stubService(SeriesRooms, () => ({ list: vi.fn(), set: vi.fn() }));

const room = (over: Partial<SeriesRoom>): SeriesRoom => ({ id: 1, name: "Fellowship Hall", building: "Main", capacity: 80, features: "", restricted: false, busyDates: 0, tooSmall: false, ...over });
const hall = room({});
const chapel = room({ id: 2, name: "Chapel", building: "", capacity: null, busyDates: 2 });
const closet = room({ id: 3, name: "Closet", building: "Annex", capacity: 1, tooSmall: true });

// An answer whose dates afterwards are, unless the test says, its busy dates needing a room and its waiting ones.
function booking(over: Partial<SeriesBooking> = {}): SeriesBooking {
  const res: SeriesBooking = { event: { id: 7, revision: "r2" }, defaultRoom: { id: 1, name: "Fellowship Hall", building: "Main" }, booked: 10, lost: [], kept: 0, keptWaiting: 0, keptInUse: 0, changed: 0, pending: 0, doubleBooked: 0, after: { ownRooms: [], needsRoom: [], pending: 0 }, ...over };
  const room = res.defaultRoom;

  return over.after || !room ? res : { ...res, after: { ownRooms: [], needsRoom: res.lost.map((date) => ({ date, roomName: room.name, status: "requested" })), pending: res.pending } };
}

const onGone = vi.fn();
const changed = () => new ApiError(409, "changed", { code: "event_changed" });
const waitForStatus = (text: string | RegExp) => waitFor(() => expect(screen.getAllByRole("status")[0]).toHaveTextContent(text));

beforeEach(() => {
  vi.resetAllMocks();
  actor.isAdmin = false;
  vi.mocked(Site.get).mockResolvedValue({ name: "Fold", timezone: "UTC", selfSignup: "off", roomsEnabled: true, pushEnabled: true });
  vi.mocked(SeriesRooms.list).mockResolvedValue({ revision: "r1", rooms: [hall, chapel, closet], dates: 10, defaultRoom: null, ownRooms: [], needsRoom: [], pending: 0 });
});

function show(onChanged = vi.fn()) {
  render(<Form noValidate onSubmit={(e) => e.preventDefault()}><SeriesRoomField eventId={7} idPrefix="t" onChanged={onChanged} onGone={onGone} /></Form>);

  return onChanged;
}

const select = () => screen.findByRole("combobox", { name: /^Room for every date/ });

// Choosing is not booking: the button after the select books.
async function pickAndBook(value: string, button = "Book for every date") {
  await userEvent.selectOptions(await select(), value);
  await userEvent.click(screen.getByRole("button", { name: button }));
}

it("words each option like the one-date Room, with how many dates are busy", async () => {
  show();
  const box = await select();
  expect(SeriesRooms.list).toHaveBeenCalledWith(7);
  expect(Array.from(requiredElement(box, HTMLSelectElement).options).map((o) => o.textContent)).toEqual([
    "No series room",
    "Fellowship Hall · Main (80 seats)",
    "Chapel — in use on 2 of 10 dates",
    "Closet · Annex (1 seat) — may be too small",
  ]);
  expect(box).toHaveValue("none");
  expect(screen.getByText(/Choose a room, then book it for every upcoming date./)).toBeInTheDocument();
  expect(screen.getByText(/“No series room” releases the rooms the series booked./)).toBeInTheDocument();
});

it("shows the series room that is already set", async () => {
  vi.mocked(SeriesRooms.list).mockResolvedValue({ revision: "r1", rooms: [hall], dates: 10, defaultRoom: { id: 1, name: "Fellowship Hall", building: "Main" }, ownRooms: [], needsRoom: [], pending: 0 });
  show();
  expect(await select()).toHaveValue("1");
});

it("books every date at once and says how many", async () => {
  vi.mocked(SeriesRooms.set).mockResolvedValue(booking({ booked: 8 }));
  const onChanged = show();
  await pickAndBook("1");
  expect(SeriesRooms.set).toHaveBeenCalledWith(7, { roomId: 1 }, "r1");
  await waitForStatus("Booked for 8 dates.");
  expect(await select()).toHaveValue("1");
  expect(onChanged).toHaveBeenCalledTimes(1);
});

it("says which dates are busy and which keep their own room", async () => {
  vi.mocked(SeriesRooms.set).mockResolvedValue(booking({ booked: 7, lost: ["2026-12-08", "2026-12-15"], kept: 1 }));
  show();
  await pickAndBook("2");
  await waitForStatus(
    "Booked for 7 dates. 2 dates still need a room; pick one for them under “Dates that need a room” on the event page. 1 date keeps its own room.",
  );
  // A leader has no way to double-book.
  expect(screen.queryByRole("button", { name: "Book anyway" })).not.toBeInTheDocument();
});

it("uses the singular for one date", async () => {
  vi.mocked(SeriesRooms.set).mockResolvedValue(booking({ booked: 1, lost: ["2026-12-08"] }));
  show();
  await pickAndBook("1");
  await waitForStatus("Booked for 1 date. 1 date still needs a room; pick one for it");
});

it("releases the series room and says what stays", async () => {
  vi.mocked(SeriesRooms.list).mockResolvedValue({ revision: "r1", rooms: [hall], dates: 10, defaultRoom: { id: 1, name: "Fellowship Hall", building: "Main" }, ownRooms: [], needsRoom: [], pending: 0 });
  vi.mocked(SeriesRooms.set).mockResolvedValue(booking({ defaultRoom: null, booked: 0, lost: [], kept: 2, pending: 0 }));
  show();
  await pickAndBook("none", "Stop the series room");
  expect(SeriesRooms.set).toHaveBeenCalledWith(7, { roomId: null }, "r1");
  await waitForStatus("Series room released. 2 dates keep their own room.");
  expect(await select()).toHaveValue("none");
});

it("shows the error, books nothing, and leaves the button to try again", async () => {
  vi.mocked(SeriesRooms.set).mockRejectedValue(new ApiError(403, "no", { code: "room_restricted" }));
  show();
  await pickAndBook("1");
  expect(await screen.findByRole("alert")).toHaveTextContent("Only an admin can book that room.");
  expect(screen.getByRole("button", { name: "Book for every date" })).toBeEnabled();
  expect(screen.getByRole("status")).toBeEmptyDOMElement();
});

it("offers an admin Book anyway for the busy dates, with a required reason", async () => {
  actor.isAdmin = true;
  vi.mocked(SeriesRooms.set)
    .mockResolvedValueOnce(booking({ defaultRoom: { id: 2, name: "Chapel", building: "" }, booked: 8, lost: ["2026-12-08", "2026-12-15"] }))
    .mockResolvedValueOnce(booking({ defaultRoom: { id: 2, name: "Chapel", building: "" }, booked: 10, doubleBooked: 2 }));
  show();
  await pickAndBook("2");
  expect(SeriesRooms.set).toHaveBeenLastCalledWith(7, { roomId: 2 }, "r1");
  const reason = await screen.findByRole("textbox", { name: /^Reason for double-booking/ });
  expect(reason).toHaveAttribute("maxlength", "500");
  expect(screen.getByRole("alert")).toHaveTextContent("2 dates are in use. Give a reason to book them anyway.");
  await waitFor(() => expect(reason).toHaveFocus());
  await userEvent.click(screen.getByRole("button", { name: "Book anyway" }));
  expect(await screen.findByText("Say why you are double-booking this room.")).toBeInTheDocument();
  expect(reason).toHaveFocus();
  expect(SeriesRooms.set).toHaveBeenCalledTimes(1);
  await userEvent.type(reason, "Choir moved");
  await userEvent.click(screen.getByRole("button", { name: "Book anyway" }));
  await waitFor(() => expect(SeriesRooms.set).toHaveBeenLastCalledWith(7, { roomId: 2, overlapReason: "Choir moved" }, "r2"));
  await waitForStatus("Booked for 10 dates, 2 of them over another booking.");
  expect(screen.queryByRole("textbox", { name: /^Reason/ })).not.toBeInTheDocument();
});

it("asks an admin for nothing when every date was free", async () => {
  actor.isAdmin = true;
  vi.mocked(SeriesRooms.set).mockResolvedValue(booking());
  show();
  await pickAndBook("1");
  await waitForStatus("Booked for 10 dates.");
  expect(screen.queryByRole("button", { name: "Book anyway" })).not.toBeInTheDocument();
});

it("names a series room this person can't list", async () => {
  vi.mocked(SeriesRooms.list).mockResolvedValue({ revision: "r1", rooms: [hall], dates: 3, defaultRoom: { id: 9, name: "Nursery", building: "Annex" }, ownRooms: [], needsRoom: [], pending: 0 });
  show();
  const box = await select();
  expect(box).toHaveValue("9");
  expect(screen.getByRole("option", { name: "Nursery · Annex" })).toBeInTheDocument();
});

it("shows nothing, and asks for no rooms, while rooms are off", async () => {
  vi.mocked(Site.get).mockResolvedValue({ name: "Fold", timezone: "UTC", selfSignup: "off", roomsEnabled: false, pushEnabled: true });
  const { container } = render(<SeriesRoomField eventId={7} idPrefix="t" onGone={onGone} />);
  await waitFor(() => expect(Site.get).toHaveBeenCalled());
  expect(container).toBeEmptyDOMElement();
  expect(SeriesRooms.list).not.toHaveBeenCalled();
});

it("shows nothing, not an error, when the site settings fail to load", async () => {
  vi.mocked(Site.get).mockRejectedValue(new ApiError(500, "down", { code: "server_error" }));
  const { container } = render(<SeriesRoomField eventId={7} idPrefix="t" onGone={onGone} />);
  await waitFor(() => expect(Site.get).toHaveBeenCalled());
  expect(container).toBeEmptyDOMElement();
});

it("offers a retry when the rooms fail to load", async () => {
  vi.mocked(SeriesRooms.list).mockRejectedValueOnce(new ApiError(500, "down", { code: "server_error" }));
  show();
  await userEvent.click(await screen.findByRole("button", { name: "Try again" }));
  expect(await select()).toBeInTheDocument();
  expect(SeriesRooms.list).toHaveBeenCalledTimes(2);
});

it("books nothing when the select changes, however many keys are pressed", async () => {
  show();
  const box = await select();
  await userEvent.selectOptions(box, "1");
  await userEvent.selectOptions(box, "3");
  await userEvent.selectOptions(box, "none");
  expect(SeriesRooms.set).not.toHaveBeenCalled();
});

it("names the button for what it does, and disables it when the choice is what is booked", async () => {
  vi.mocked(SeriesRooms.list).mockResolvedValue({ revision: "r1", rooms: [hall, closet], dates: 10, defaultRoom: { id: 1, name: "Fellowship Hall", building: "Main" }, ownRooms: [], needsRoom: [], pending: 0 });
  show();
  const box = await select();
  expect(screen.getByRole("button", { name: "Book for every date" })).toBeDisabled();
  await userEvent.selectOptions(box, "3");
  expect(screen.getByRole("button", { name: "Book for every date" })).toBeEnabled();
  await userEvent.selectOptions(box, "none");
  expect(screen.queryByRole("button", { name: "Book for every date" })).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Stop the series room" })).toBeEnabled();
  await userEvent.selectOptions(box, "1");
  expect(screen.getByRole("button", { name: "Book for every date" })).toBeDisabled();
});

it("disables the button with no series room and No series room chosen", async () => {
  show();
  await select();
  expect(screen.getByRole("button", { name: "Stop the series room" })).toBeDisabled();
});

it("says before booking how many dates the chosen room is busy on, to anyone", async () => {
  for (const admin of [false, true]) {
    actor.isAdmin = admin;
    const { unmount } = render(<SeriesRoomField eventId={7} idPrefix="t" onGone={onGone} />);
    await userEvent.selectOptions(await select(), "2");
    expect(screen.getByRole("status")).toHaveTextContent(
      "Chapel is in use on 2 of 10 dates. It will be booked on the other 8, and the busy dates will be listed under “Dates that need a room”.",
    );
    expect(SeriesRooms.set).not.toHaveBeenCalled();
    await userEvent.selectOptions(screen.getByRole("combobox"), "1");
    expect(screen.getByRole("status")).toBeEmptyDOMElement();
    unmount();
  }
});

it("says when the series room holds no date", async () => {
  vi.mocked(SeriesRooms.list).mockResolvedValue({
    revision: "r1", rooms: [chapel], dates: 2, defaultRoom: { id: 2, name: "Chapel", building: "" },
    ownRooms: [], needsRoom: [{ date: "2026-12-08", roomName: "Chapel", status: "requested" }, { date: "2026-12-15", roomName: "Chapel", status: "requested" }],
    pending: 0,
  });
  show();
  await select();
  expect(screen.getByRole("status")).toHaveTextContent("Not booked on any date yet: every date is busy.");
});

it("says so after a booking that every date refused", async () => {
  vi.mocked(SeriesRooms.list).mockResolvedValue({ revision: "r1", rooms: [room({ id: 2, name: "Chapel", building: "", busyDates: 2 })], dates: 2, defaultRoom: null, ownRooms: [], needsRoom: [], pending: 0 });
  vi.mocked(SeriesRooms.set).mockResolvedValue(booking({ defaultRoom: { id: 2, name: "Chapel", building: "" }, booked: 0, lost: ["2026-12-08", "2026-12-15"] }));
  show();
  await pickAndBook("2");
  await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("Not booked on any date yet: every date is busy."));
});

it("says nothing about an empty room while some date has it", async () => {
  vi.mocked(SeriesRooms.list).mockResolvedValue({ revision: "r1", rooms: [chapel], dates: 3, defaultRoom: { id: 2, name: "Chapel", building: "" }, ownRooms: [], needsRoom: [{ date: "2026-12-08", roomName: "Chapel", status: "requested" }], pending: 0 });
  show();
  await select();
  expect(screen.getByRole("status")).toBeEmptyDOMElement();
});

it("says dates were sent to an admin, alongside the busy and kept lines", async () => {
  vi.mocked(SeriesRooms.set).mockResolvedValue(booking({ booked: 0, pending: 8, lost: ["2026-12-08"], kept: 1 }));
  show();
  await pickAndBook("1");
  await waitForStatus(
    "Sent to an admin for approval: 8 dates. 1 date still needs a room; pick one for it under “Dates that need a room” on the event page. 1 date keeps its own room.",
  );
  expect(screen.getByRole("status")).not.toHaveTextContent("Booked for");
});

it("says dates are waiting for an admin when the field loads", async () => {
  vi.mocked(SeriesRooms.list).mockResolvedValue({ revision: "r1", rooms: [hall], dates: 10, defaultRoom: { id: 1, name: "Fellowship Hall", building: "Main" }, ownRooms: [], needsRoom: [], pending: 8 });
  show();
  await select();
  expect(screen.getByRole("status")).toHaveTextContent("Waiting for an admin to approve this room for 8 dates.");
});

it("does not repeat the waiting line right after a booking said it", async () => {
  vi.mocked(SeriesRooms.set).mockResolvedValue(booking({ booked: 0, pending: 10 }));
  show();
  await pickAndBook("1");
  await waitForStatus("Sent to an admin for approval: 10 dates.");
  expect(screen.getByRole("status")).not.toHaveTextContent("Waiting for an admin");
});

it("lists suggested rooms first with why and busy dates, then the rest", async () => {
  vi.mocked(SeriesRooms.list).mockResolvedValue({
    revision: "r1", rooms: [hall, chapel, closet], dates: 10, defaultRoom: null, ownRooms: [], needsRoom: [], pending: 0,
    suggestions: { rooms: [{ roomId: 2, uses: 4, reason: "Used by this event 4 times", busyDates: 2 }], otherKinds: false, dates: 8 },
  });
  show();
  const box = await select();
  const groups = Array.from(box.querySelectorAll("optgroup")).map((g) => [g.label, Array.from(g.querySelectorAll("option")).map((o) => o.textContent)]);
  expect(groups).toEqual([
    ["Suggested", ["Chapel — in use on 2 of 10 dates — Used by this event 4 times"]],
    ["Other rooms", ["Fellowship Hall · Main (80 seats)", "Closet · Annex (1 seat) — may be too small"]],
  ]);
  expect(requiredElement(box, HTMLSelectElement).options[0].textContent).toBe("No series room");
});

// DEC-167: each series pick names the event's read it was made from, and the control stays on screen to be used again.
it("sends the read's revision, then the revision each answer names", async () => {
  vi.mocked(SeriesRooms.set)
    .mockResolvedValueOnce(booking({ event: { id: 7, revision: "r2" } }))
    .mockResolvedValueOnce(booking({ event: { id: 7, revision: "r3" }, defaultRoom: { id: 3, name: "Closet", building: "Annex" } }));
  show();
  await pickAndBook("1");
  await waitForStatus("Booked for 10 dates.");
  await pickAndBook("3");
  await waitFor(() => expect(SeriesRooms.set).toHaveBeenCalledTimes(2));
  expect(vi.mocked(SeriesRooms.set).mock.calls.map((c) => c[2])).toEqual(["r1", "r2"]);
  expect(SeriesRooms.list).toHaveBeenCalledOnce();
});

it("shows the series room as it is now and says so at the field when someone else changed it", async () => {
  vi.mocked(SeriesRooms.list)
    .mockResolvedValueOnce({ revision: "r1", rooms: [hall, chapel, closet], dates: 10, defaultRoom: null, ownRooms: [], needsRoom: [], pending: 0 })
    .mockResolvedValueOnce({ revision: "r3", rooms: [hall, chapel, closet], dates: 10, defaultRoom: { id: 3, name: "Closet", building: "Annex" }, ownRooms: [], needsRoom: [], pending: 0 });
  vi.mocked(SeriesRooms.set).mockRejectedValueOnce(changed());
  const onChanged = show();
  await pickAndBook("1");
  const line = await screen.findByText("The room for every date changed while you were choosing, so your choice could not be saved. This is the room as it is now.");
  await waitFor(() => expect(line).toHaveFocus());
  expect(await select()).toHaveValue("3");
  expect(SeriesRooms.set).toHaveBeenCalledOnce();
  // The page behind reads again quietly, so its dates show the room too.
  expect(onChanged).toHaveBeenCalledOnce();
  vi.mocked(SeriesRooms.set).mockResolvedValueOnce(booking({ defaultRoom: null, booked: 0 }));
  await pickAndBook("none", "Stop the series room");
  await waitForStatus("Series room released.");
  expect(vi.mocked(SeriesRooms.set).mock.calls[1][2]).toBe("r3");
  expect(screen.queryByText(/changed while you were choosing/)).not.toBeInTheDocument();
});

it("sends the pick again by itself when the event changed but its series room did not", async () => {
  vi.mocked(SeriesRooms.list)
    .mockResolvedValueOnce({ revision: "r1", rooms: [hall, chapel, closet], dates: 10, defaultRoom: null, ownRooms: [], needsRoom: [], pending: 0 })
    .mockResolvedValueOnce({ revision: "r3", rooms: [hall, chapel, closet], dates: 10, defaultRoom: null, ownRooms: [], needsRoom: [], pending: 0 });
  vi.mocked(SeriesRooms.set).mockRejectedValueOnce(changed()).mockResolvedValueOnce(booking());
  show();
  await pickAndBook("1");
  await waitForStatus("Booked for 10 dates.");
  expect(vi.mocked(SeriesRooms.set).mock.calls.map((c) => c[2])).toEqual(["r1", "r3"]);
  expect(screen.queryByText(/changed while you were choosing/)).not.toBeInTheDocument();
});

it("counts a series pick whose answer was lost as saved when the read shows it stored", async () => {
  vi.mocked(SeriesRooms.list)
    .mockResolvedValueOnce({ revision: "r1", rooms: [hall, closet], dates: 10, defaultRoom: null, ownRooms: [], needsRoom: [], pending: 0 })
    .mockResolvedValue({ revision: "r3", rooms: [hall, closet], dates: 10, defaultRoom: { id: 1, name: "Fellowship Hall", building: "Main" }, ownRooms: [], needsRoom: [], pending: 0 });
  vi.mocked(SeriesRooms.set).mockRejectedValueOnce(new TypeError("Failed to fetch")).mockRejectedValue(changed());
  show();
  await pickAndBook("1");
  expect(await screen.findByRole("alert")).toBeInTheDocument();
  await userEvent.click(screen.getByRole("button", { name: "Book for every date" }));
  await waitForStatus("The series room is saved.");
  expect(await select()).toHaveValue("1");
  expect(SeriesRooms.set).toHaveBeenCalledTimes(2);
});

it("hands over to the host when the person may no longer manage the event, and starts no second pick while one is on its way", async () => {
  let fail!: (err: ApiError) => void;
  vi.mocked(SeriesRooms.set).mockReturnValueOnce(new Promise((_, reject) => { fail = reject; }));
  show();
  await pickAndBook("1");
  await waitForStatus("Saving…");
  await userEvent.click(screen.getByRole("button", { name: "Book for every date" }));
  expect(SeriesRooms.set).toHaveBeenCalledOnce();
  fail(new ApiError(403, "no", { code: "not_allowed" }));
  await waitFor(() => expect(onGone).toHaveBeenCalledOnce());
});

const read = (over: Partial<SeriesRoomsResponse> = {}): SeriesRoomsResponse => ({ revision: "r1", rooms: [hall, chapel, closet], dates: 10, defaultRoom: null, ownRooms: [], needsRoom: [], pending: 0, ...over });
const chapelRoom = { id: 2, name: "Chapel", building: "" };
const busyDates = (...dates: string[]) => dates.map((date) => ({ date, roomName: "Chapel", status: "requested" as const }));

it("says what each date has when a pick whose answer was lost is found stored, as a landed pick would", async () => {
  vi.mocked(SeriesRooms.list)
    .mockResolvedValueOnce(read())
    .mockResolvedValue(read({ revision: "r3", defaultRoom: chapelRoom, ownRooms: [], needsRoom: busyDates("2026-12-15", "2026-12-08"), pending: 8 }));
  vi.mocked(SeriesRooms.set).mockRejectedValueOnce(new TypeError("Failed to fetch")).mockRejectedValue(changed());
  show();
  await pickAndBook("2");
  expect(await screen.findByRole("alert")).toBeInTheDocument();
  await userEvent.click(screen.getByRole("button", { name: "Book for every date" }));
  await waitForStatus("Sent to an admin for approval: 8 dates. 2 dates still need a room; pick one for them under “Dates that need a room” on the event page.");
  expect(screen.queryByText(/The series room is saved/)).not.toBeInTheDocument();
});

it("books every date again, and says what its answer did, when someone else already set the same series room", async () => {
  // Another session booked the hall for every date, then took the first date's booking away: the read shows the series
  // room and nothing waiting or needing a room, which says nothing about the bare date.
  vi.mocked(SeriesRooms.list)
    .mockResolvedValueOnce(read())
    .mockResolvedValue(read({ revision: "r3", defaultRoom: { id: 1, name: "Fellowship Hall", building: "Main" } }));
  vi.mocked(SeriesRooms.set).mockRejectedValueOnce(changed()).mockResolvedValueOnce(booking({ event: { id: 7, revision: "r4" }, booked: 1 }));
  show();
  await pickAndBook("1");
  await waitForStatus("Booked for 1 date.");
  expect(vi.mocked(SeriesRooms.set).mock.calls.map((c) => c[2])).toEqual(["r1", "r3"]);
  expect(screen.queryByText(/is saved|changed while you were choosing/)).not.toBeInTheDocument();
});

it("says what a Book anyway sent again after its answer was lost found, never the prompt's count", async () => {
  actor.isAdmin = true;
  vi.mocked(SeriesRooms.set)
    .mockResolvedValueOnce(booking({ defaultRoom: chapelRoom, booked: 8, lost: ["2026-12-08", "2026-12-15"] }))
    .mockRejectedValueOnce(new TypeError("Failed to fetch"))
    // The lost send double-booked both dates: the same series room, so the revision did not move and this one lands.
    .mockResolvedValueOnce(booking({ defaultRoom: chapelRoom, booked: 0 }));
  show();
  await pickAndBook("2");
  const reason = await screen.findByRole("textbox", { name: /^Reason for double-booking/ });
  await userEvent.type(reason, "Choir moved");
  await userEvent.click(screen.getByRole("button", { name: "Book anyway" }));
  await waitFor(() => expect(SeriesRooms.set).toHaveBeenCalledTimes(2));
  await waitFor(() => expect(screen.getByRole("button", { name: "Book anyway" })).toBeEnabled());
  expect(screen.getByRole("textbox", { name: /^Reason for double-booking/ })).toHaveValue("Choir moved");
  await userEvent.click(screen.getByRole("button", { name: "Book anyway" }));
  await waitForStatus("The series room is saved.");
  expect(vi.mocked(SeriesRooms.set).mock.calls[2][1]).toEqual({ roomId: 2, overlapReason: "Choir moved" });
  expect(screen.queryByText(/over another booking|no upcoming dates/)).not.toBeInTheDocument();
});

it("offers an admin Book anyway for the busy dates a stored pick's read shows", async () => {
  actor.isAdmin = true;
  vi.mocked(SeriesRooms.list)
    .mockResolvedValueOnce(read())
    .mockResolvedValue(read({ revision: "r3", defaultRoom: chapelRoom, ownRooms: [], needsRoom: busyDates("2026-12-08", "2026-12-15") }));
  vi.mocked(SeriesRooms.set).mockRejectedValueOnce(new TypeError("Failed to fetch")).mockRejectedValue(changed());
  show();
  await pickAndBook("2");
  expect(await screen.findByRole("alert")).toBeInTheDocument();
  await userEvent.click(screen.getByRole("button", { name: "Book for every date" }));
  expect(await screen.findByRole("textbox", { name: /^Reason for double-booking/ })).toBeInTheDocument();
  expect(screen.getByText("2 dates are in use. Give a reason to book them anyway.")).toBeInTheDocument();
});

it("keeps Book anyway through a change that leaves the room and its busy dates as they were", async () => {
  actor.isAdmin = true;
  vi.mocked(SeriesRooms.list)
    .mockResolvedValueOnce(read())
    // Someone retitled the event: the same room, the same busy dates.
    .mockResolvedValue(read({ revision: "r3", defaultRoom: chapelRoom, ownRooms: [], needsRoom: busyDates("2026-12-15", "2026-12-08") }));
  vi.mocked(SeriesRooms.set)
    .mockResolvedValueOnce(booking({ defaultRoom: chapelRoom, booked: 8, lost: ["2026-12-08", "2026-12-15"] }))
    .mockRejectedValueOnce(changed())
    .mockResolvedValueOnce(booking({ event: { id: 7, revision: "r4" }, defaultRoom: chapelRoom, booked: 10, doubleBooked: 2 }));
  show();
  await pickAndBook("2");
  const reason = await screen.findByRole("textbox", { name: /^Reason for double-booking/ });
  await userEvent.type(reason, "Choir moved");
  await userEvent.click(screen.getByRole("button", { name: "Book anyway" }));
  await waitForStatus("Booked for 10 dates, 2 of them over another booking.");
  expect(vi.mocked(SeriesRooms.set).mock.calls.map((c) => c[2])).toEqual(["r1", "r2", "r3"]);
  expect(screen.queryByText(/changed while you were choosing/)).not.toBeInTheDocument();
});

it("asks again about the busy dates the read shows when they changed while an admin gave a reason", async () => {
  actor.isAdmin = true;
  vi.mocked(SeriesRooms.list)
    .mockResolvedValueOnce(read())
    .mockResolvedValue(read({ revision: "r3", defaultRoom: chapelRoom, ownRooms: [], needsRoom: busyDates("2026-12-08", "2026-12-15", "2026-12-22") }));
  vi.mocked(SeriesRooms.set)
    .mockResolvedValueOnce(booking({ defaultRoom: chapelRoom, booked: 8, lost: ["2026-12-08", "2026-12-15"] }))
    .mockRejectedValueOnce(changed());
  show();
  await pickAndBook("2");
  const reason = await screen.findByRole("textbox", { name: /^Reason for double-booking/ });
  await userEvent.type(reason, "Choir moved");
  await userEvent.click(screen.getByRole("button", { name: "Book anyway" }));
  await screen.findByText(/The room for every date changed while you were choosing/);
  expect(screen.getByText("3 dates are in use. Give a reason to book them anyway.")).toBeInTheDocument();
  expect(screen.getByRole("textbox", { name: /^Reason for double-booking/ })).toHaveValue("Choir moved");
  expect(SeriesRooms.set).toHaveBeenCalledTimes(2);
});

it("keeps the select as it is while a pick is on its way", async () => {
  let land!: (b: SeriesBooking) => void;
  vi.mocked(SeriesRooms.set).mockReturnValueOnce(new Promise((r) => { land = r; }));
  show();
  await pickAndBook("1");
  await waitForStatus("Saving…");
  expect(await select()).toBeDisabled();
  land(booking());
  await waitForStatus("Booked for 10 dates.");
  expect(await select()).toBeEnabled();
});

it("says plainly that a date with its own room keeps it, waiting or not, unless the person changes it too", async () => {
  show();
  await select();
  expect(screen.getByText(/A date with a room of its own, booked or waiting for approval, keeps it unless you choose to change it too\./)).toBeInTheDocument();
});

it("says rooms were turned off without handing over to the host", async () => {
  const onChanged = show();
  vi.mocked(SeriesRooms.set).mockRejectedValue(new ApiError(404, "off", { code: "rooms_off" }));
  await pickAndBook("1");
  expect(await screen.findByText("Room booking has been turned off, so your choice could not be saved.")).toBeInTheDocument();
  expect(onGone).not.toHaveBeenCalled();
  expect(onChanged).not.toHaveBeenCalled();
});

// DEC-168: a change of the series room leaves alone the dates with a room of their own, lists them first, and can
// include them.
const ownRoom = (date: string, over: Partial<OwnRoom> = {}): OwnRoom => ({ date, room: { id: 2, name: "Chapel", building: "" }, waiting: false, mayChange: true, ...over });
const ownList = () => screen.queryByText(/ha(s|ve) a room of (its|their) own/);

it("lists the dates with a room of their own, and offers the box, only when there are some", async () => {
  show();
  await select();
  expect(ownList()).not.toBeInTheDocument();
  expect(screen.queryByRole("checkbox")).not.toBeInTheDocument();
  await userEvent.selectOptions(await select(), "1");
  expect(screen.queryByRole("checkbox")).not.toBeInTheDocument();
});

it("says before the pick that the dates with their own room keep it, waiting ones included", async () => {
  vi.mocked(SeriesRooms.list).mockResolvedValue(read({ ownRooms: [ownRoom("2026-12-08"), ownRoom("2026-12-15", { waiting: true, room: { id: 3, name: "Closet", building: "Annex" } })] }));
  show();
  await userEvent.selectOptions(await select(), "1");
  expect(screen.getByText("2 dates have a room of their own and will keep it:")).toBeInTheDocument();
  const rows = screen.getAllByRole("listitem").map((li) => li.textContent);
  expect(rows).toEqual(["Tuesday, December 8: Chapel", "Tuesday, December 15: Closet · Annex — waiting for an admin's approval"]);
  const box = screen.getByRole("checkbox", { name: "Also change these dates to Fellowship Hall · Main" });
  expect(box).not.toBeChecked();
  await userEvent.click(screen.getByRole("button", { name: "Book for every date" }));
  expect(SeriesRooms.set).toHaveBeenCalledWith(7, { roomId: 1 }, "r1");
});

it("names the dates shown when the box is ticked, and leaves out one already in the room picked", async () => {
  vi.mocked(SeriesRooms.list).mockResolvedValue(read({ ownRooms: [ownRoom("2026-12-08"), ownRoom("2026-12-15", { room: { id: 3, name: "Closet", building: "Annex" } })] }));
  vi.mocked(SeriesRooms.set).mockResolvedValue(booking({ defaultRoom: { id: 2, name: "Chapel", building: "" }, booked: 9, changed: 1 }));
  show();
  await userEvent.selectOptions(await select(), "2");
  // The chapel date already holds the room picked: it is not listed, and not named.
  expect(screen.getByText("1 date has a room of its own and will keep it:")).toBeInTheDocument();
  expect(screen.getAllByRole("listitem").map((li) => li.textContent)).toEqual(["Tuesday, December 15: Closet · Annex"]);
  await userEvent.click(screen.getByRole("checkbox", { name: "Also change this date to Chapel" }));
  expect(screen.getByText("1 date has a room of its own:")).toBeInTheDocument();
  await userEvent.click(screen.getByRole("button", { name: "Book for every date" }));
  expect(SeriesRooms.set).toHaveBeenCalledWith(7, { roomId: 2, includeDates: ["2026-12-15"] }, "r1");
  await waitForStatus("Booked for 9 dates. Also changed 1 date that had a room of its own.");
});

it("says the rooms will be released when the series room stops, and names the dates ticked", async () => {
  vi.mocked(SeriesRooms.list).mockResolvedValue(read({ defaultRoom: { id: 1, name: "Fellowship Hall", building: "Main" }, ownRooms: [ownRoom("2026-12-08", { waiting: true })] }));
  vi.mocked(SeriesRooms.set).mockResolvedValue(booking({ defaultRoom: null, booked: 0, changed: 1 }));
  show();
  await userEvent.selectOptions(await select(), "none");
  await userEvent.click(screen.getByRole("checkbox", { name: "Also release this date's room" }));
  await userEvent.click(screen.getByRole("button", { name: "Stop the series room" }));
  expect(SeriesRooms.set).toHaveBeenCalledWith(7, { roomId: null, includeDates: ["2026-12-08"] }, "r1");
  await waitForStatus("Series room released. Also released the room of 1 date that had its own.");
});

it("lists a leader's admin-only dates as staying, and the box counts only the dates the leader may change", async () => {
  const nursery = { id: 9, name: "Nursery", building: "" };
  vi.mocked(SeriesRooms.list).mockResolvedValue(read({ ownRooms: [ownRoom("2026-12-08", { room: nursery, mayChange: false }), ownRoom("2026-12-15")] }));
  vi.mocked(SeriesRooms.set).mockResolvedValue(booking({ booked: 9, changed: 1, kept: 1 }));
  show();
  await userEvent.selectOptions(await select(), "1");
  expect(screen.getByText("Nursery — stays: only an admin can change it")).toBeInTheDocument();
  await userEvent.click(screen.getByRole("checkbox", { name: "Also change 1 of these dates to Fellowship Hall · Main" }));
  await userEvent.click(screen.getByRole("button", { name: "Book for every date" }));
  expect(SeriesRooms.set).toHaveBeenCalledWith(7, { roomId: 1, includeDates: ["2026-12-15"] }, "r1");
  await waitForStatus("Booked for 9 dates. Also changed 1 date that had a room of its own. 1 date keeps its own room.");
});

it("offers no box when every listed date is only an admin's to change", async () => {
  vi.mocked(SeriesRooms.list).mockResolvedValue(read({ ownRooms: [ownRoom("2026-12-08", { mayChange: false })] }));
  show();
  await userEvent.selectOptions(await select(), "1");
  expect(screen.getByText("Chapel — stays: only an admin can change it")).toBeInTheDocument();
  expect(screen.queryByRole("checkbox")).not.toBeInTheDocument();
});

it("books the series room again for the listed dates when only the box is ticked, and then lists what it kept", async () => {
  vi.mocked(SeriesRooms.list).mockResolvedValue(read({ defaultRoom: { id: 1, name: "Fellowship Hall", building: "Main" }, ownRooms: [ownRoom("2026-12-08"), ownRoom("2026-12-15")] }));
  vi.mocked(SeriesRooms.set)
    .mockResolvedValueOnce(booking({ booked: 1, changed: 1, kept: 1, after: { ownRooms: [ownRoom("2026-12-22")], needsRoom: [], pending: 0 } }))
    .mockResolvedValueOnce(booking({ event: { id: 7, revision: "r3" }, defaultRoom: { id: 3, name: "Closet", building: "Annex" }, booked: 10, kept: 1 }));
  show();
  await select();
  const book = screen.getByRole("button", { name: "Book for every date" });
  expect(book).toBeDisabled();
  await userEvent.click(screen.getByRole("checkbox", { name: "Also change these dates to Fellowship Hall · Main" }));
  expect(book).toBeEnabled();
  await userEvent.click(book);
  expect(SeriesRooms.set).toHaveBeenCalledWith(7, { roomId: 1, includeDates: ["2026-12-08", "2026-12-15"] }, "r1");
  await waitForStatus("Booked for 1 date. Also changed 1 date that had a room of its own. 1 date keeps its own room.");
  // The list follows the answer, and the box starts clear for the next pick.
  expect(screen.getAllByRole("listitem").map((li) => li.textContent)).toEqual(["Tuesday, December 22: Chapel"]);
  expect(screen.getByRole("checkbox")).not.toBeChecked();
  expect(screen.getByRole("button", { name: "Book for every date" })).toBeDisabled();
  await pickAndBook("3");
  await waitFor(() => expect(SeriesRooms.set).toHaveBeenCalledTimes(2));
  expect(vi.mocked(SeriesRooms.set).mock.calls[1]).toEqual([7, { roomId: 3 }, "r2"]);
});

it("after someone else changed the series room, says so and lists the dates the read shows", async () => {
  vi.mocked(SeriesRooms.list)
    .mockResolvedValueOnce(read({ ownRooms: [ownRoom("2026-12-08")] }))
    .mockResolvedValueOnce(read({ revision: "r3", defaultRoom: { id: 3, name: "Closet", building: "Annex" }, ownRooms: [ownRoom("2026-12-08"), ownRoom("2026-12-29")] }));
  vi.mocked(SeriesRooms.set).mockRejectedValueOnce(changed());
  show();
  await userEvent.selectOptions(await select(), "1");
  await userEvent.click(screen.getByRole("checkbox", { name: "Also change this date to Fellowship Hall · Main" }));
  await userEvent.click(screen.getByRole("button", { name: "Book for every date" }));
  expect(await screen.findByText("The room for every date changed while you were choosing, so your choice could not be saved. This is the room as it is now.")).toBeInTheDocument();
  expect(await select()).toHaveValue("3");
  expect(screen.getAllByRole("listitem").map((li) => li.textContent)).toEqual(["Tuesday, December 8: Chapel", "Tuesday, December 29: Chapel"]);
  expect(screen.getByRole("checkbox")).not.toBeChecked();
  expect(SeriesRooms.set).toHaveBeenCalledOnce();
});

it("sends the same named dates again by itself when the event changed but its series room did not", async () => {
  vi.mocked(SeriesRooms.list)
    .mockResolvedValueOnce(read({ ownRooms: [ownRoom("2026-12-08")] }))
    // A date picked its own room since: it is not named, so the server keeps it.
    .mockResolvedValueOnce(read({ revision: "r3", ownRooms: [ownRoom("2026-12-08"), ownRoom("2026-12-29")] }));
  vi.mocked(SeriesRooms.set).mockRejectedValueOnce(changed()).mockResolvedValueOnce(booking({ booked: 9, changed: 1, kept: 1 }));
  show();
  await userEvent.selectOptions(await select(), "1");
  await userEvent.click(screen.getByRole("checkbox"));
  await userEvent.click(screen.getByRole("button", { name: "Book for every date" }));
  await waitForStatus("Booked for 9 dates. Also changed 1 date that had a room of its own. 1 date keeps its own room.");
  expect(vi.mocked(SeriesRooms.set).mock.calls.map((c) => [c[1], c[2]])).toEqual([
    [{ roomId: 1, includeDates: ["2026-12-08"] }, "r1"],
    [{ roomId: 1, includeDates: ["2026-12-08"] }, "r3"],
  ]);
});

it("shows a long list's first dates and the rest behind one button", async () => {
  const many = Array.from({ length: 12 }, (_, i) => ownRoom(`2027-01-${String(i + 1).padStart(2, "0")}`));
  vi.mocked(SeriesRooms.list).mockResolvedValue(read({ ownRooms: many }));
  show();
  await userEvent.selectOptions(await select(), "1");
  expect(screen.getAllByRole("listitem")).toHaveLength(10);
  await userEvent.click(screen.getByRole("button", { name: "Show all 12" }));
  expect(screen.getAllByRole("listitem")).toHaveLength(12);
  expect(screen.queryByRole("button", { name: /^Show all/ })).not.toBeInTheDocument();
});

it("never carries the named dates into Book anyway", async () => {
  actor.isAdmin = true;
  vi.mocked(SeriesRooms.list).mockResolvedValue(read({ ownRooms: [ownRoom("2026-12-08", { room: { id: 3, name: "Closet", building: "Annex" } })] }));
  vi.mocked(SeriesRooms.set)
    .mockResolvedValueOnce(booking({ defaultRoom: chapelRoom, booked: 7, changed: 1, lost: ["2026-12-15", "2026-12-22"] }))
    .mockResolvedValueOnce(booking({ event: { id: 7, revision: "r3" }, defaultRoom: chapelRoom, booked: 2, doubleBooked: 2 }));
  show();
  await userEvent.selectOptions(await select(), "2");
  await userEvent.click(screen.getByRole("checkbox"));
  await userEvent.click(screen.getByRole("button", { name: "Book for every date" }));
  await userEvent.type(await screen.findByRole("textbox", { name: /^Reason for double-booking/ }), "Choir moved");
  await userEvent.click(screen.getByRole("button", { name: "Book anyway" }));
  await waitForStatus("Booked for 2 dates, 2 of them over another booking.");
  expect(vi.mocked(SeriesRooms.set).mock.calls.map((c) => c[1])).toEqual([
    { roomId: 2, includeDates: ["2026-12-08"] },
    { roomId: 2, overlapReason: "Choir moved" },
  ]);
});

it("keeps the keyboard's place when the rest of the dates are shown: on the first one shown", async () => {
  const many = Array.from({ length: 12 }, (_, i) => ownRoom(`2027-01-${String(i + 1).padStart(2, "0")}`));
  vi.mocked(SeriesRooms.list).mockResolvedValue(read({ ownRooms: many }));
  show();
  await userEvent.selectOptions(await select(), "1");
  screen.getByRole("button", { name: "Show all 12" }).focus();
  await userEvent.keyboard("{Enter}");
  expect(document.activeElement?.tagName).toBe("LI");
  expect(document.activeElement).toHaveTextContent("Monday, January 11: Chapel");
  await userEvent.tab();
  expect(screen.getByRole("checkbox")).toHaveFocus();
});

// The next choice is made from the dates as the answer read them after the pick, never from what it kept: once the
// hall is the series room, a date waiting for the hall is the series' own, and stopping the series room releases it.
it("lists for the next choice the dates the answer read after the pick", async () => {
  actor.isAdmin = true;
  vi.mocked(SeriesRooms.list).mockResolvedValue(read({ ownRooms: [ownRoom("2026-12-08", { waiting: true, room: { id: 1, name: "Fellowship Hall", building: "Main" } })] }));
  vi.mocked(SeriesRooms.set).mockResolvedValue(booking({ booked: 9, kept: 1, keptWaiting: 1, after: { ownRooms: [], needsRoom: [], pending: 1 } }));
  show();
  await userEvent.selectOptions(await select(), "1");
  expect(screen.getByText("1 date has a room of its own and will keep it:")).toBeInTheDocument();
  await userEvent.click(screen.getByRole("button", { name: "Book for every date" }));
  await waitForStatus("Booked for 9 dates. 1 date keeps its own room request, still waiting for an admin's approval.");
  await userEvent.selectOptions(await select(), "none");
  expect(ownList()).not.toBeInTheDocument();
  expect(screen.queryByRole("checkbox")).not.toBeInTheDocument();
});

// A pick whose answer was lost, then the box ticked and pressed again: the dates it now names were never sent, so the
// stored series room is not this pick.
it("says the choice could not be saved when the box changed after a pick whose answer was lost", async () => {
  vi.mocked(SeriesRooms.list)
    .mockResolvedValueOnce(read({ ownRooms: [ownRoom("2026-12-08")] }))
    .mockResolvedValue(read({ revision: "r3", defaultRoom: { id: 1, name: "Fellowship Hall", building: "Main" }, ownRooms: [ownRoom("2026-12-08")] }));
  vi.mocked(SeriesRooms.set).mockRejectedValueOnce(new TypeError("Failed to fetch")).mockRejectedValue(changed());
  show();
  await pickAndBook("1");
  expect(await screen.findByRole("alert")).toBeInTheDocument();
  await userEvent.click(screen.getByRole("checkbox", { name: "Also change this date to Fellowship Hall · Main" }));
  await userEvent.click(screen.getByRole("button", { name: "Book for every date" }));
  expect(await screen.findByText("The room for every date changed while you were choosing, so your choice could not be saved. This is the room as it is now.")).toBeInTheDocument();
  expect(screen.queryByText(/The series room is saved/)).not.toBeInTheDocument();
  expect(screen.getByRole("checkbox")).not.toBeChecked();
});

it("says only how many dates the room is in use on while some dates keep their own room", async () => {
  vi.mocked(SeriesRooms.list).mockResolvedValue(read({ ownRooms: [ownRoom("2026-12-08", { room: { id: 3, name: "Closet", building: "Annex" } })] }));
  show();
  await userEvent.selectOptions(await select(), "2");
  expect(screen.getByText("Chapel is in use on 2 of 10 dates.")).toBeInTheDocument();
  expect(screen.queryByText(/It will be booked|will be listed/)).not.toBeInTheDocument();
});

it("clears the box when the choice changes, so a tick for one room never releases rooms", async () => {
  vi.mocked(SeriesRooms.list).mockResolvedValue(read({ defaultRoom: { id: 1, name: "Fellowship Hall", building: "Main" }, ownRooms: [ownRoom("2026-12-08")] }));
  vi.mocked(SeriesRooms.set).mockResolvedValue(booking({ defaultRoom: null, booked: 0, kept: 1 }));
  show();
  await userEvent.selectOptions(await select(), "3");
  await userEvent.click(screen.getByRole("checkbox", { name: "Also change this date to Closet · Annex" }));
  await userEvent.selectOptions(await select(), "none");
  expect(screen.getByRole("checkbox", { name: "Also release this date's room" })).not.toBeChecked();
  await userEvent.click(screen.getByRole("button", { name: "Stop the series room" }));
  expect(SeriesRooms.set).toHaveBeenCalledWith(7, { roomId: null }, "r1");
});

it("offers no box while it asks an admin to book the busy dates anyway, which names no dates", async () => {
  actor.isAdmin = true;
  const closetDate = ownRoom("2026-12-08", { room: { id: 3, name: "Closet", building: "Annex" } });
  vi.mocked(SeriesRooms.list).mockResolvedValue(read({ ownRooms: [closetDate] }));
  vi.mocked(SeriesRooms.set).mockResolvedValue(booking({ defaultRoom: chapelRoom, booked: 7, kept: 1, lost: ["2026-12-15", "2026-12-22"],
    after: { ownRooms: [closetDate], needsRoom: busyDates("2026-12-15", "2026-12-22"), pending: 0 } }));
  show();
  await pickAndBook("2");
  expect(await screen.findByRole("textbox", { name: /^Reason for double-booking/ })).toBeInTheDocument();
  expect(screen.getByText("Tuesday, December 8")).toBeInTheDocument();
  expect(screen.queryByRole("checkbox")).not.toBeInTheDocument();
});
