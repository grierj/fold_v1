import { stubService } from "../../test/service";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { AdminRooms } from "../../api/rooms";
import { ApiError } from "../../api/core";
import { ERROR_COPY } from "../../lib/errors";
import type { Room } from "../../types";
import { RoomsTab, sortRooms } from "./RoomsTab";

stubService(AdminRooms, () => ({ list: vi.fn(), create: vi.fn(), update: vi.fn(), setEnabled: vi.fn(), getFeed: vi.fn(), makeFeed: vi.fn(), revokeFeed: vi.fn() }));

const room = (id: number, name: string, extra: Partial<Room> = {}): Room => ({
  id, name, building: "", capacity: null, features: "", notes: "", active: true, restricted: false,
  defaultSetupMinutes: 0, defaultTeardownMinutes: 0, revision: `rev-${id}`, ...extra,
});

const hall = room(1, "Fellowship Hall", { building: "Main", capacity: 80, features: "kitchen, projector", notes: "Ask Dana", defaultSetupMinutes: 30, defaultTeardownMinutes: 15 });

// The page owns the list; this stands in for it. Like the page's read, `retry` shows nothing until the rooms answer.
function Harness({ initial }: { initial: Room[] | null }) {
  const [rooms, setRooms] = useState(initial);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const load = {
    loading,
    data: rooms,
    retry: async () => {
      setRooms(null);
      setError(null);
      setLoading(true);

      try {
        setRooms(sortRooms((await AdminRooms.list()).rooms));

        return "loaded" as const;
      } catch {
        setError("We couldn't load the rooms.");

        return "failed" as const;
      } finally {
        setLoading(false);
      }
    },
  };

  return <RoomsTab rooms={rooms} error={error} reload={() => {}} load={load}
    onSaved={(r) => setRooms((prev) => sortRooms([...(prev ?? []).filter((x) => x.id !== r.id), r]))}
    onShown={(list) => setRooms(sortRooms(list.rooms))} />;
}

beforeEach(() => {
  vi.mocked(AdminRooms.list).mockReset();
  vi.mocked(AdminRooms.create).mockReset();
  vi.mocked(AdminRooms.update).mockReset();
});

const changed = () => new ApiError(409, "room changed", { code: "room_changed" });

// A send whose answer never came: it may have been stored.
const lost = () => new ApiError(502, "bad gateway");
const listOf = (...rooms: Room[]) => ({ rooms, needApproval: false });

// The form's own submit button; the tab's "Add room" button is hidden while it is open.
const submitAdd = () => within(screen.getByRole("form", { name: "Add room" })).getByRole("button", { name: "Add room" });

describe("room list", () => {
  it("shows name, building, seats, features, and labels restricted and inactive rooms", () => {
    render(<Harness initial={[
      hall,
      room(2, "Nursery", { restricted: true }),
      room(3, "Old Annex", { active: false }),
    ]} />);
    expect(screen.getByText("Fellowship Hall")).toBeInTheDocument();
    expect(screen.getByText("Main · Seats 80 · kitchen, projector")).toBeInTheDocument();
    const nursery = screen.getByRole("group", { name: "Nursery" });
    expect(within(nursery).getByText("Restricted")).toBeInTheDocument();
    expect(within(nursery).queryByText("Inactive")).not.toBeInTheDocument();
    const annex = screen.getByRole("group", { name: "Old Annex" });
    expect(within(annex).getByText("Inactive")).toBeInTheDocument();
    expect(within(annex).queryByText("Restricted")).not.toBeInTheDocument();
    expect(within(screen.getByRole("group", { name: "Fellowship Hall" })).queryByText("Restricted")).not.toBeInTheDocument();
  });

  it("puts active rooms first", () => {
    render(<Harness initial={sortRooms([room(3, "Annex", { active: false }), room(2, "Zeta")])} />);
    const names = screen.getAllByText(/^(Annex|Zeta)$/).map((n) => n.textContent);
    expect(names).toEqual(["Zeta", "Annex"]);
  });

  it("explains an empty list and offers to add the first room", async () => {
    render(<Harness initial={[]} />);
    expect(screen.getByText("No rooms yet")).toBeInTheDocument();
    expect(screen.getByText("Add the rooms your church books, like the fellowship hall or classrooms.")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Add room" }));
    expect(screen.queryByText("No rooms yet")).not.toBeInTheDocument();
    expect(screen.getByRole("form", { name: "Add room" })).toBeInTheDocument();
  });
});

describe("adding a room", () => {
  async function open() {
    render(<Harness initial={[hall]} />);
    await userEvent.click(screen.getByRole("button", { name: "Add room" }));
  }

  it("shows every field with its hint", async () => {
    await open();
    expect(screen.getByLabelText(/^Name/)).toBeRequired();
    expect(screen.getByLabelText("Building (optional)")).toBeInTheDocument();
    expect(screen.getByLabelText("Capacity (optional)")).toHaveAccessibleDescription("Leave blank for no limit");
    expect(screen.getByLabelText("Features (optional)")).toHaveAccessibleDescription("For example: projector, piano, kitchen");
    expect(screen.getByLabelText("Notes for admins (optional)")).toHaveAccessibleDescription("Only admins see this");
    expect(screen.getByRole("checkbox", { name: /Only admins can book this room/ })).toBeInTheDocument();
    expect(screen.getByText("For rooms like the nursery")).toBeInTheDocument();
    expect(screen.getByLabelText("Setup minutes (optional)")).toHaveAccessibleDescription("Held before each booking. Leave blank for none.");
    expect(screen.getByLabelText("Cleanup minutes (optional)")).toHaveAccessibleDescription("Held after each booking. Leave blank for none.");
  });

  it("asks for a name before saving", async () => {
    await open();
    await userEvent.click(submitAdd());
    expect(AdminRooms.create).not.toHaveBeenCalled();
    expect(screen.getByLabelText(/^Name/)).toBeInvalid();
    expect(screen.getByText("Enter a name.")).toBeInTheDocument();
  });

  it("rejects a capacity that is not a whole number", async () => {
    await open();
    await userEvent.type(screen.getByLabelText(/^Name/), "Chapel");
    await userEvent.type(screen.getByLabelText("Capacity (optional)"), "2.5");
    await userEvent.click(submitAdd());
    expect(AdminRooms.create).not.toHaveBeenCalled();
    expect(screen.getByText("Enter a whole number.")).toBeInTheDocument();
  });

  it("puts a taken name under the Name field", async () => {
    vi.mocked(AdminRooms.create).mockRejectedValue(new ApiError(409, "taken", { code: "room_name_taken" }));
    await open();
    await userEvent.type(screen.getByLabelText(/^Name/), "Fellowship Hall");
    await userEvent.click(submitAdd());
    const name = screen.getByLabelText(/^Name/);
    expect(name).toBeInvalid();
    expect(name).toHaveAccessibleDescription(ERROR_COPY.room_name_taken);
    expect(name).toHaveFocus();
    expect(screen.getByRole("form", { name: "Add room" })).toBeInTheDocument();
  });

  it("shows other failures in words, never the server's text", async () => {
    vi.mocked(AdminRooms.create).mockRejectedValue(new ApiError(500, "pq: private"));
    await open();
    await userEvent.type(screen.getByLabelText(/^Name/), "Chapel");
    await userEvent.click(submitAdd());
    expect(await screen.findByRole("alert")).toHaveTextContent("We couldn't save that room.");
    expect(screen.queryByText(/private/)).not.toBeInTheDocument();
  });

  it("adds the room, closes the form, lists it and says so", async () => {
    vi.mocked(AdminRooms.create).mockResolvedValue({ room: room(9, "Chapel", { building: "North", capacity: 40, restricted: true, defaultSetupMinutes: 10 }) });
    await open();
    await userEvent.type(screen.getByLabelText(/^Name/), "  Chapel ");
    await userEvent.type(screen.getByLabelText("Building (optional)"), "North");
    await userEvent.type(screen.getByLabelText("Capacity (optional)"), "40");
    await userEvent.click(screen.getByRole("checkbox", { name: /Only admins can book this room/ }));
    await userEvent.type(screen.getByLabelText("Setup minutes (optional)"), "10");
    await userEvent.click(submitAdd());
    expect(AdminRooms.create).toHaveBeenCalledWith({
      name: "Chapel", building: "North", features: "", notes: "", restricted: true, capacity: 40, defaultSetupMinutes: 10,
    });
    expect(await screen.findByText("“Chapel” added.")).toBeInTheDocument();
    expect(screen.queryByRole("form")).not.toBeInTheDocument();
    expect(screen.getByText("North · Seats 40")).toBeInTheDocument();
  });

  it("closes the form on Cancel without saving", async () => {
    await open();
    await userEvent.type(screen.getByLabelText(/^Name/), "Chapel");
    await userEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("form")).not.toBeInTheDocument();
    expect(AdminRooms.create).not.toHaveBeenCalled();
  });

  it("moves focus to the first field when the form opens, and back to Add room on Cancel", async () => {
    await open();
    expect(screen.getByLabelText(/^Name/)).toHaveFocus();
    await userEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.getByRole("button", { name: "Add room" })).toHaveFocus();
  });

  it("returns focus to the empty state's Add room button on Cancel", async () => {
    render(<Harness initial={[]} />);
    await userEvent.click(screen.getByRole("button", { name: "Add room" }));
    expect(screen.getByLabelText(/^Name/)).toHaveFocus();
    await userEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.getByRole("button", { name: "Add room" })).toHaveFocus();
  });

  it("holds the other openers while the form is open, so edits are never replaced", async () => {
    render(<Harness initial={[hall, room(2, "Nursery")]} />);
    await userEvent.click(screen.getByRole("button", { name: "Add room" }));
    expect(screen.getByRole("button", { name: "Edit Fellowship Hall" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Edit Nursery" })).toBeDisabled();
    await userEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.getByRole("button", { name: "Edit Nursery" })).toBeEnabled();
  });
});

describe("editing a room", () => {
  async function open() {
    render(<Harness initial={[hall, room(2, "Nursery", { restricted: true })]} />);
    await userEvent.click(screen.getByRole("button", { name: "Edit Fellowship Hall" }));

    return screen.getByRole("form", { name: "Edit Fellowship Hall" });
  }

  it("moves focus to the first field, and back to that room's Edit button on Cancel", async () => {
    await open();
    expect(screen.getByLabelText(/^Name/)).toHaveFocus();
    await userEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.getByRole("button", { name: "Edit Fellowship Hall" })).toHaveFocus();
  });

  it("keeps unsaved edits when another room's Edit or Add room is tried", async () => {
    await open();
    await userEvent.type(screen.getByLabelText(/^Name/), " 2");
    expect(screen.getByRole("button", { name: "Edit Nursery" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Add room" })).toBeDisabled();
    await userEvent.click(screen.getByRole("button", { name: "Edit Nursery" }));
    expect(screen.getByRole("form", { name: "Edit Fellowship Hall" })).toBeInTheDocument();
    expect(screen.getByLabelText(/^Name/)).toHaveValue("Fellowship Hall 2");
  });

  it("opens the same form with the room's values", async () => {
    await open();
    expect(screen.getByLabelText(/^Name/)).toHaveValue("Fellowship Hall");
    expect(screen.getByLabelText("Building (optional)")).toHaveValue("Main");
    expect(screen.getByLabelText("Capacity (optional)")).toHaveValue(80);
    expect(screen.getByLabelText("Features (optional)")).toHaveValue("kitchen, projector");
    expect(screen.getByLabelText("Notes for admins (optional)")).toHaveValue("Ask Dana");
    expect(screen.getByRole("checkbox", { name: /Only admins can book this room/ })).not.toBeChecked();
    expect(screen.getByLabelText("Setup minutes (optional)")).toHaveValue(30);
    expect(screen.getByLabelText("Cleanup minutes (optional)")).toHaveValue(15);
    expect(screen.getByRole("button", { name: "Save room" })).toBeInTheDocument();
  });

  it("saves changes and says so", async () => {
    vi.mocked(AdminRooms.update).mockResolvedValue({ room: { ...hall, name: "Great Hall" } });
    await open();
    await userEvent.clear(screen.getByLabelText(/^Name/));
    await userEvent.type(screen.getByLabelText(/^Name/), "Great Hall");
    await userEvent.click(screen.getByRole("button", { name: "Save room" }));
    expect(AdminRooms.update).toHaveBeenCalledWith(1, expect.objectContaining({ name: "Great Hall", capacity: 80, defaultSetupMinutes: 30, defaultTeardownMinutes: 15 }), "rev-1");
    expect(await screen.findByText("“Great Hall” saved.")).toBeInTheDocument();
    expect(screen.queryByRole("form")).not.toBeInTheDocument();
    expect(screen.getByText("Great Hall")).toBeInTheDocument();
  });

  it("sends a null capacity when it is cleared", async () => {
    vi.mocked(AdminRooms.update).mockResolvedValue({ room: { ...hall, capacity: null } });
    await open();
    await userEvent.clear(screen.getByLabelText("Capacity (optional)"));
    await userEvent.click(screen.getByRole("button", { name: "Save room" }));
    expect(AdminRooms.update).toHaveBeenCalledWith(1, expect.objectContaining({ capacity: null }), "rev-1");
    expect(await screen.findByText("“Fellowship Hall” saved.")).toBeInTheDocument();
    expect(screen.getByText("Main · kitchen, projector")).toBeInTheDocument();
  });
});

describe("deactivating and reactivating", () => {
  it("deactivates from More without a confirmation", async () => {
    vi.mocked(AdminRooms.update).mockResolvedValue({ room: { ...hall, active: false } });
    const confirmSpy = vi.spyOn(window, "confirm");
    render(<Harness initial={[hall]} />);
    await userEvent.click(screen.getByRole("button", { name: "More for Fellowship Hall" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Deactivate Fellowship Hall" }));
    expect(AdminRooms.update).toHaveBeenCalledWith(1, { active: false }, "rev-1");
    expect(confirmSpy).not.toHaveBeenCalled();
    expect(await screen.findByText("“Fellowship Hall” deactivated.")).toBeInTheDocument();
    expect(screen.getByText("Inactive")).toBeInTheDocument();
    confirmSpy.mockRestore();
  });

  it("reactivates an inactive room", async () => {
    const off = { ...hall, active: false };
    vi.mocked(AdminRooms.update).mockResolvedValue({ room: hall });
    render(<Harness initial={[off]} />);
    await userEvent.click(screen.getByRole("button", { name: "More for Fellowship Hall" }));
    expect(screen.queryByRole("menuitem", { name: "Deactivate Fellowship Hall" })).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("menuitem", { name: "Reactivate Fellowship Hall" }));
    expect(AdminRooms.update).toHaveBeenCalledWith(1, { active: true }, "rev-1");
    expect(await screen.findByText("“Fellowship Hall” reactivated.")).toBeInTheDocument();
    expect(screen.queryByText("Inactive")).not.toBeInTheDocument();
  });

  it("says so when it can't change the room", async () => {
    vi.mocked(AdminRooms.update).mockRejectedValue(new ApiError(500, "x"));
    render(<Harness initial={[hall]} />);
    await userEvent.click(screen.getByRole("button", { name: "More for Fellowship Hall" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Deactivate Fellowship Hall" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("We couldn't deactivate that room.");
    expect(screen.queryByText("Inactive")).not.toBeInTheDocument();
  });
});

// DEC-171: the edit form is an editor that closes on a conflict and says so; the tab reads the rooms from nothing.
describe("an edit someone else's change refused", () => {
  async function editName(to: string) {
    await userEvent.click(screen.getByRole("button", { name: "Edit Fellowship Hall" }));
    await userEvent.clear(screen.getByLabelText(/^Name/));
    await userEvent.type(screen.getByLabelText(/^Name/), to);
    await userEvent.click(screen.getByRole("button", { name: "Save room" }));
  }

  it("closes the form, shows the room as it is now and says so", async () => {
    const theirs = { ...hall, name: "Main Hall", notes: "Use the side door", revision: "rev-1b" };
    vi.mocked(AdminRooms.update).mockRejectedValue(changed());
    vi.mocked(AdminRooms.list).mockResolvedValue(listOf(theirs));
    render(<Harness initial={[hall]} />);
    await editName("Great Hall");
    const line = await screen.findByText("Main Hall. This room changed while you were editing, so your changes could not be saved. This is the room as it is now.");
    expect(line).toHaveFocus();
    expect(screen.queryByRole("form")).not.toBeInTheDocument();
    expect(screen.getByRole("group", { name: "Main Hall" })).toBeInTheDocument();
    expect(screen.queryByText("Great Hall")).not.toBeInTheDocument();
    expect(AdminRooms.update).toHaveBeenCalledTimes(1);
    expect(screen.queryByText(/close|reopen|reload/i)).not.toBeInTheDocument();
    // Opening the form again starts from the room as it is now, and the line goes.
    await userEvent.click(screen.getByRole("button", { name: "Edit Main Hall" }));
    expect(screen.getByLabelText(/^Name/)).toHaveValue("Main Hall");
    expect(screen.queryByText(/This room changed/)).not.toBeInTheDocument();
  });

  it("sends the same save again under the new revision when nothing it writes changed", async () => {
    vi.mocked(AdminRooms.update).mockRejectedValueOnce(changed()).mockResolvedValueOnce({ room: { ...hall, active: false, name: "Great Hall", revision: "rev-1c" } });
    vi.mocked(AdminRooms.list).mockResolvedValue(listOf({ ...hall, active: false, revision: "rev-1b" }));
    render(<Harness initial={[hall]} />);
    await editName("Great Hall");
    expect(await screen.findByText("“Great Hall” saved.")).toBeInTheDocument();
    expect(AdminRooms.update).toHaveBeenNthCalledWith(1, 1, expect.objectContaining({ name: "Great Hall" }), "rev-1");
    expect(AdminRooms.update).toHaveBeenNthCalledWith(2, 1, expect.objectContaining({ name: "Great Hall" }), "rev-1b");
    expect(screen.queryByText(/could not be saved/)).not.toBeInTheDocument();
  });

  it("ends as saved when a send whose answer was lost is what the read shows", async () => {
    vi.mocked(AdminRooms.update).mockRejectedValueOnce(lost()).mockRejectedValueOnce(changed());
    vi.mocked(AdminRooms.list).mockResolvedValue(listOf({ ...hall, name: "Great Hall", revision: "rev-1b" }));
    render(<Harness initial={[hall]} />);
    await editName("Great Hall");
    expect(await screen.findByRole("alert")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Save room" }));
    expect(await screen.findByText("“Great Hall” saved.")).toBeInTheDocument();
    expect(AdminRooms.update).toHaveBeenCalledTimes(2);
    expect(screen.queryByRole("form")).not.toBeInTheDocument();
  });

  it("is a conflict, never a second send, when a lost send's room was put back", async () => {
    vi.mocked(AdminRooms.update).mockRejectedValueOnce(lost()).mockRejectedValueOnce(changed());
    vi.mocked(AdminRooms.list).mockResolvedValue(listOf({ ...hall, revision: "rev-1c" }));
    render(<Harness initial={[hall]} />);
    await editName("Great Hall");
    await screen.findByRole("alert");
    await userEvent.click(screen.getByRole("button", { name: "Save room" }));
    expect(await screen.findByText(/This room changed while you were editing/)).toBeInTheDocument();
    expect(AdminRooms.update).toHaveBeenCalledTimes(2);
  });

  it("names the last save's revision on the next one", async () => {
    vi.mocked(AdminRooms.update).mockResolvedValueOnce({ room: { ...hall, name: "Great Hall", revision: "rev-1b" } })
      .mockResolvedValueOnce({ room: { ...hall, name: "Grand Hall", revision: "rev-1c" } });
    render(<Harness initial={[hall]} />);
    await editName("Great Hall");
    await screen.findByText("“Great Hall” saved.");
    await userEvent.click(screen.getByRole("button", { name: "Edit Great Hall" }));
    await userEvent.clear(screen.getByLabelText(/^Name/));
    await userEvent.type(screen.getByLabelText(/^Name/), "Grand Hall");
    await userEvent.click(screen.getByRole("button", { name: "Save room" }));
    await screen.findByText("“Grand Hall” saved.");
    expect(AdminRooms.update).toHaveBeenNthCalledWith(2, 1, expect.objectContaining({ name: "Grand Hall" }), "rev-1b");
  });

  it("closes and says the changes could not be saved when the person may no longer manage rooms", async () => {
    vi.mocked(AdminRooms.update).mockRejectedValue(changed());
    vi.mocked(AdminRooms.list).mockRejectedValue(new ApiError(403, "admin required"));
    render(<Harness initial={[hall]} />);
    await editName("Great Hall");
    expect(await screen.findByText("Fellowship Hall. Your changes could not be saved.")).toHaveFocus();
    expect(screen.queryByRole("form")).not.toBeInTheDocument();
    expect(screen.getByText("We couldn't load the rooms.")).toBeInTheDocument();
    expect(AdminRooms.update).toHaveBeenCalledTimes(1);
  });

  it("keeps Cancel and the other openers waiting while a save is on its way", async () => {
    let answer: (r: { room: Room }) => void = () => {};

    vi.mocked(AdminRooms.update).mockReturnValue(new Promise((resolve) => { answer = resolve; }));
    render(<Harness initial={[hall, room(2, "Nursery")]} />);
    await editName("Great Hall");
    expect(screen.getByRole("button", { name: "Cancel" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Edit Nursery" })).toBeDisabled();
    answer({ room: { ...hall, name: "Great Hall", revision: "rev-1b" } });
    expect(await screen.findByText("“Great Hall” saved.")).toBeInTheDocument();
  });
});

// DEC-171: Deactivate and Reactivate are a control that stays: a conflict shows the rooms as the read found them and
// says so at the row.
describe("a deactivation someone else's change refused", () => {
  async function deactivate() {
    await userEvent.click(screen.getByRole("button", { name: "More for Fellowship Hall" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Deactivate Fellowship Hall" }));
  }

  it("sends it again when only another field changed, and shows that field", async () => {
    vi.mocked(AdminRooms.update).mockRejectedValueOnce(changed())
      .mockResolvedValueOnce({ room: { ...hall, active: false, notes: "Theirs", revision: "rev-1c" } });
    vi.mocked(AdminRooms.list).mockResolvedValue(listOf({ ...hall, notes: "Theirs", revision: "rev-1b" }));
    render(<Harness initial={[hall]} />);
    await deactivate();
    expect(await screen.findByText("“Fellowship Hall” deactivated.")).toBeInTheDocument();
    expect(AdminRooms.update).toHaveBeenNthCalledWith(2, 1, { active: false }, "rev-1b");
    expect(screen.getByText("Inactive")).toBeInTheDocument();
  });

  it("ends as saved when a lost press is what the read shows", async () => {
    vi.mocked(AdminRooms.update).mockRejectedValueOnce(lost()).mockRejectedValueOnce(changed());
    vi.mocked(AdminRooms.list).mockResolvedValue(listOf({ ...hall, active: false, revision: "rev-1b" }));
    render(<Harness initial={[hall]} />);
    await deactivate();
    expect(await screen.findByRole("alert")).toHaveTextContent("We couldn't deactivate that room.");
    await deactivate();
    expect(await screen.findByText("“Fellowship Hall” deactivated.")).toBeInTheDocument();
    expect(screen.getByText("Inactive")).toBeInTheDocument();
    expect(AdminRooms.update).toHaveBeenCalledTimes(2);
  });

  it("says so at the row, showing the room as it is now, when a lost press was undone", async () => {
    vi.mocked(AdminRooms.update).mockRejectedValueOnce(lost()).mockRejectedValueOnce(changed());
    vi.mocked(AdminRooms.list).mockResolvedValue(listOf({ ...hall, name: "Main Hall", revision: "rev-1c" }));
    render(<Harness initial={[hall]} />);
    await deactivate();
    await screen.findByRole("alert");
    await deactivate();
    const row = await screen.findByRole("group", { name: "Main Hall" });
    expect(within(row).getByText("This room changed while you were choosing, so your choice could not be saved. This is the room as it is now.")).toHaveFocus();
    expect(within(row).queryByText("Inactive")).not.toBeInTheDocument();
    expect(AdminRooms.update).toHaveBeenCalledTimes(2);
  });

  it("names the last press's revision on the next one", async () => {
    vi.mocked(AdminRooms.update).mockResolvedValueOnce({ room: { ...hall, active: false, revision: "rev-1b" } })
      .mockResolvedValueOnce({ room: { ...hall, revision: "rev-1c" } });
    render(<Harness initial={[hall]} />);
    await deactivate();
    await screen.findByText("“Fellowship Hall” deactivated.");
    await userEvent.click(screen.getByRole("button", { name: "More for Fellowship Hall" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Reactivate Fellowship Hall" }));
    await screen.findByText("“Fellowship Hall” reactivated.");
    expect(AdminRooms.update).toHaveBeenNthCalledWith(2, 1, { active: true }, "rev-1b");
  });

  it("says the change could not be saved when the person may no longer manage rooms", async () => {
    vi.mocked(AdminRooms.update).mockRejectedValue(changed());
    vi.mocked(AdminRooms.list).mockRejectedValue(new ApiError(404, "not found"));
    render(<Harness initial={[hall]} />);
    await deactivate();
    expect(await screen.findByText("Fellowship Hall. Your changes could not be saved.")).toHaveFocus();
    expect(screen.getByText("We couldn't load the rooms.")).toBeInTheDocument();
  });

  it("keeps Edit and Add room waiting while it is on its way", async () => {
    let answer: (r: { room: Room }) => void = () => {};

    vi.mocked(AdminRooms.update).mockReturnValue(new Promise((resolve) => { answer = resolve; }));
    render(<Harness initial={[hall, room(2, "Nursery")]} />);
    await deactivate();
    expect(screen.getByRole("button", { name: "Edit Nursery" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Edit Fellowship Hall" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Add room" })).toBeDisabled();
    answer({ room: { ...hall, active: false, revision: "rev-1b" } });
    expect(await screen.findByText("“Fellowship Hall” deactivated.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Edit Nursery" })).toBeEnabled();
  });
});

describe("room calendar feed (M2.2b)", () => {
  it("active rooms get a Calendar feed panel; Done gives focus back", async () => {
    vi.mocked(AdminRooms.getFeed).mockResolvedValue({ feed: { hasToken: false, createdAt: null } });
    render(<Harness initial={[hall, room(3, "Old Annex", { active: false })]} />);
    await userEvent.click(screen.getByRole("button", { name: "More for Old Annex" }));
    expect(screen.queryByRole("menuitem", { name: "Calendar feed for Old Annex" })).toBeNull();
    await userEvent.keyboard("{Escape}");
    const opener = screen.getByRole("button", { name: "More for Fellowship Hall" });
    await userEvent.click(opener);
    await userEvent.click(screen.getByRole("menuitem", { name: "Calendar feed for Fellowship Hall" }));
    const panel = screen.getByRole("region", { name: "Calendar feed for Fellowship Hall" });
    expect(await within(panel).findByRole("button", { name: "Create calendar link" })).toBeInTheDocument();
    expect(AdminRooms.getFeed).toHaveBeenCalledWith(1);
    await userEvent.click(within(panel).getByRole("button", { name: "Done" }));
    expect(screen.queryByRole("region", { name: "Calendar feed for Fellowship Hall" })).toBeNull();
    expect(screen.getByRole("button", { name: "More for Fellowship Hall" })).toHaveFocus();
  });
});
