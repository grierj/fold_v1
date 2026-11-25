import { describe, expect, it } from "vitest";
import type { OwnRoom, RoomPickerItem, SeriesBooking, SeriesRoom } from "../types";
import { busyDatesNote, includeLabel, ownRoomNote, ownRoomsFor, ownRoomsLead, parseRoomLabel, roomOptionLabel, seriesBookingMessage, seriesRoomLabel } from "./roomLabels";

const hall = { id: 1, name: "Fellowship Hall", building: "Main", capacity: 80, tooSmall: false, restricted: false, features: "" } as const;

describe("room option labels", () => {
  it("writes the name, seats, a busy note, a size note and the suggestion, in that order", () => {
    const room = { ...hall, busy: true, tooSmall: true } satisfies RoomPickerItem;
    expect(roomOptionLabel(room, "used by this group")).toBe("Fellowship Hall · Main (80 seats) — in use at this time — may be too small — used by this group");
    expect(roomOptionLabel({ ...hall, capacity: 1, busy: false } satisfies RoomPickerItem)).toBe("Fellowship Hall · Main (1 seat)");
    expect(roomOptionLabel({ ...hall, capacity: null, busy: false } satisfies RoomPickerItem)).toBe("Fellowship Hall · Main");
  });

  it("counts the busy dates of a series", () => {
    const room = { ...hall, busyDates: 3 } satisfies SeriesRoom;
    expect(seriesRoomLabel(room, 8)).toBe("Fellowship Hall · Main (80 seats) — in use on 3 of 8 dates");
    expect(seriesRoomLabel(room, 1)).toBe("Fellowship Hall · Main (80 seats) — in use at this time");
    expect(seriesRoomLabel({ ...room, busyDates: 0 }, 8)).toBe("Fellowship Hall · Main (80 seats)");
  });
});

describe("parseRoomLabel", () => {
  it("reads back what the labels write", () => {
    const room = { ...hall, busy: true, tooSmall: true } satisfies RoomPickerItem;
    expect(parseRoomLabel(roomOptionLabel(room, "used by this group"))).toEqual({
      room: "Fellowship Hall · Main", seats: 80, notes: ["in use at this time", "may be too small", "used by this group"],
    });
    expect(parseRoomLabel(roomOptionLabel({ ...hall, capacity: 1, busy: false } satisfies RoomPickerItem))).toEqual({ room: "Fellowship Hall · Main", seats: 1, notes: [] });
    expect(parseRoomLabel(roomOptionLabel({ ...hall, capacity: null, busy: false } satisfies RoomPickerItem))).toEqual({ room: "Fellowship Hall · Main", seats: null, notes: [] });
    expect(parseRoomLabel(seriesRoomLabel({ ...hall, busyDates: 3 } satisfies SeriesRoom, 8))).toEqual({ room: "Fellowship Hall · Main", seats: 80, notes: ["in use on 3 of 8 dates"] });
  });
});

describe("busyDatesNote", () => {
  const room = (busyDates: number) => ({ ...hall, busyDates }) satisfies SeriesRoom;
  it("says nothing for a free room", () => {
    expect(busyDatesNote(room(0), 8, "edit")).toBeNull();
  });
  it("words the edit form", () => {
    expect(busyDatesNote(room(1), 1, "edit")).toContain("so the date will need a room");
    expect(busyDatesNote(room(8), 8, "edit")).toContain("in use on all 8 dates, so none can be booked yet;");
    expect(busyDatesNote(room(3), 8, "edit")).toContain("booked on the other 5, and the busy dates will be listed");
  });
  it("words the create form", () => {
    expect(busyDatesNote(room(1), 1, "create")).toContain("the event will start without a room");
    expect(busyDatesNote(room(8), 8, "create")).toContain("The event page will list them");
    expect(busyDatesNote(room(3), 8, "create")).toContain("the event page will list the busy dates");
  });
});

describe("seriesBookingMessage", () => {
  const res = (over: Partial<SeriesBooking>): SeriesBooking => ({ event: { id: 7, revision: "r2" }, defaultRoom: hall, booked: 0, pending: 0, lost: [], kept: 0, keptRooms: [], changed: 0, doubleBooked: 0, ...over });
  it("tells what happened", () => {
    expect(seriesBookingMessage(res({ booked: 1 }))).toBe("Booked for 1 date.");
    expect(seriesBookingMessage(res({ booked: 8, doubleBooked: 2 }))).toBe("Booked for 8 dates, 2 of them over another booking.");
    expect(seriesBookingMessage(res({ defaultRoom: null }))).toBe("Series room released.");
    // A confirmation sent again after its answer was lost finds every date already in the room.
    expect(seriesBookingMessage(res({}))).toBe("The series room is saved.");
    expect(seriesBookingMessage(res({ booked: 1, lost: ["2026-12-08"] }))).toBe("Booked for 1 date. 1 date still needs a room; pick one for it under “Dates that need a room” on the event page.");
    expect(seriesBookingMessage(res({ booked: 3, pending: 2, kept: 1 }))).toBe("Booked for 3 dates. Sent to an admin for approval: 2 dates. 1 date keeps its own room.");
  });
});

// DEC-168: what the series room control says about dates with a room of their own.
describe("dates with a room of their own", () => {
  const own = (over: Partial<OwnRoom>): OwnRoom => ({ date: "2026-12-08", room: { id: 2, name: "Chapel", building: "" }, waiting: false, mayChange: true, ...over });
  const res = (over: Partial<SeriesBooking>): SeriesBooking => ({ event: { id: 7, revision: "r2" }, defaultRoom: hall, booked: 0, pending: 0, lost: [], kept: 0, keptRooms: [], changed: 0, doubleBooked: 0, ...over });

  it("leaves out a date that already holds the room picked, but not one still waiting for it", () => {
    const holds = own({ date: "2026-12-08" });
    const waits = own({ date: "2026-12-15", waiting: true });
    const elsewhere = own({ date: "2026-12-22", room: { id: 3, name: "Annex", building: "East" } });
    expect(ownRoomsFor([holds, waits, elsewhere], 2)).toEqual([waits, elsewhere]);
    expect(ownRoomsFor([holds, waits, elsewhere], null)).toEqual([holds, waits, elsewhere]);
  });

  it("words the list, each date's room and the box", () => {
    expect(ownRoomsLead(1, false)).toBe("1 date has a room of its own and will keep it:");
    expect(ownRoomsLead(3, false)).toBe("3 dates have a room of their own and will keep it:");
    expect(ownRoomsLead(3, true)).toBe("3 dates have a room of their own:");
    expect(ownRoomNote(own({ room: { id: 3, name: "Annex", building: "East" } }))).toBe("Annex · East");
    expect(ownRoomNote(own({ waiting: true }))).toBe("Chapel — waiting for an admin's approval");
    expect(ownRoomNote(own({ mayChange: false }))).toBe("Chapel — stays: only an admin can change it");
    expect(includeLabel(hall, 2, 2)).toBe("Also change these dates to Fellowship Hall · Main");
    expect(includeLabel(hall, 1, 2)).toBe("Also change 1 of these dates to Fellowship Hall · Main");
    expect(includeLabel(null, 2, 2)).toBe("Also release the rooms of these dates");
  });

  it("says what a pick kept and changed, from its answer", () => {
    expect(seriesBookingMessage(res({ booked: 2, kept: 2, keptRooms: [own({}), own({ date: "2026-12-15" })] }))).toBe("Booked for 2 dates. 2 dates keep their own room.");
    expect(seriesBookingMessage(res({ booked: 2, kept: 1, keptRooms: [own({ waiting: true })] }))).toBe("Booked for 2 dates. 1 date keeps its own room request, still waiting for an admin's approval.");
    expect(seriesBookingMessage(res({ kept: 3, keptRooms: [own({ waiting: true }), own({}), own({})] }))).toBe("3 dates keep their own room; 1 of them is still waiting for an admin's approval.");
    expect(seriesBookingMessage(res({ booked: 4, changed: 2 }))).toBe("Booked for 4 dates. Also changed 2 dates that had a room of their own.");
    expect(seriesBookingMessage(res({ pending: 1, changed: 1 }))).toBe("Sent to an admin for approval: 1 date. Also changed 1 date that had a room of its own.");
    expect(seriesBookingMessage(res({ defaultRoom: null, changed: 1 }))).toBe("Series room released. Also released the room of 1 date that had its own.");
    expect(seriesBookingMessage(res({ defaultRoom: null, changed: 2 }))).toBe("Series room released. Also released the rooms of 2 dates that had their own.");
  });
});
