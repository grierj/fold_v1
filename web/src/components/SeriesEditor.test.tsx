import { testEvent } from "../test/fixtures";
import { stubService } from "../test/service";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { RouterProvider, createMemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { RosterWritesProvider } from "../lib/rosterWrites";
import { ERROR_COPY } from "../lib/errors";
import { formatDayFull } from "../lib/when";
import { occurrence } from "../test/occurrence";
import { careKid, deferred, KEPT, KEPT_COUNT, kept409, keptForbiddenNow, keptPerson, observeKept, READ_OUTCOMES, readFailures, type ReadOutcome } from "../test/keptPanel";
import type { Attendee, EventDetailResponse, EventInput, EventOccurrence, EventSeries, OccurrenceResponse } from "../types";
import { savedFields, SeriesEditor } from "./SeriesEditor";
import { UnsavedChangesBlocker } from "../lib/unsavedChanges";
import { DateRooms, SeriesRooms } from "../api/rooms";
import { Events } from "../api/events";
import * as core from "../api/core";
import { ApiError } from "../api/core";
import { Site } from "../api/site";
import { forgetSite } from "../lib/useSite";

stubService(DateRooms, () => ({ list: vi.fn() }));

stubService(SeriesRooms, () => ({ list: vi.fn(), set: vi.fn() }));

stubService(Events, () => ({ update: vi.fn(), generateJitsi: vi.fn().mockResolvedValue({ url: "https://meet.jit.si/fold-abc123def4" }), managedDate: vi.fn(), removeRegistration: vi.fn() }));

stubService(Site, () => ({ get: vi.fn().mockResolvedValue({ timezone: "UTC" }) }));

describe("SeriesEditor", () => {
  it.each(["1e", "-2", "0"])("focuses the remote seat limit error for %s and keeps the other fields", async (capacity) => {
    render(<SeriesEditor series={{
      eventId: 7, startsLocal: "2026-12-01T19:00", durationMinutes: 60, tzid: "UTC",
      title: "Supper", description: "", location: "Hall", recurrenceRule: "",
      capacity: null, waitlistEnabled: false, guestsAllowed: 0, remoteAvailable: true,
    }} idPrefix="invalid" onSaved={() => {}} />);
    const input = screen.getByRole("textbox", { name: /^Remote seat limit/ });
    await userEvent.type(input, capacity);
    await userEvent.click(screen.getByRole("button", { name: "Save series" }));
    expect(Events.update).not.toHaveBeenCalled();
    expect(input).toHaveValue(capacity);
    expect(input).toHaveAttribute("aria-invalid", "true");
    await waitFor(() => expect(input).toHaveFocus());
    expect(screen.getByRole("textbox", { name: "Title" })).toHaveValue("Supper");
  });

  it("carries the capacity, waitlist, and guest fields with their limit text", () => {
    render(
      <SeriesEditor
        series={{
          eventId: 7,
          startsLocal: "2026-12-01T19:00",
          durationMinutes: 60,
          tzid: "UTC",
          title: "Supper",
          description: "",
          location: "Hall",
          recurrenceRule: "",
          capacity: 20,
          waitlistEnabled: true,
          guestsAllowed: 2,
          remoteAvailable: true,
          remoteCapacity: 15,
          videoProvider: "link",
          videoUrl: "https://zoom.example.org/j/123",
          videoReveal: "registered_only",
        }}
        idPrefix="test"
        onSaved={() => {}}
      />
    );
    const capacity = screen.getByLabelText(/^Seat limit/);
    expect(capacity).toHaveValue(20);
    expect(screen.getByText(/Seats per date/)).toBeInTheDocument();
    const guests = screen.getByLabelText("Guests allowed");
    expect(guests).toHaveValue(2);
    expect(screen.getByText(/0–5/)).toBeInTheDocument();
    expect(screen.getByLabelText(/^Waitlist when full/)).toBeChecked();
    expect(screen.getByText(/refuses new registrations/)).toBeInTheDocument();
    // Remote attendance carries its own cap, separate from the room.
    expect(screen.getByLabelText(/^Remote attendance/)).toBeChecked();
    expect(screen.getByLabelText(/^Remote seat limit/)).toHaveValue("15");
    // The Video section carries the series' link and who may see it.
    expect(screen.getByLabelText("Video link")).toHaveValue(
      "https://zoom.example.org/j/123"
    );
    expect(screen.getByLabelText("Who sees the link")).toHaveValue("registered_only");
  });

  it("shows an empty capacity as no limit", () => {
    render(
      <SeriesEditor
        series={{
          eventId: 7,
          startsLocal: "2026-12-01T19:00",
          durationMinutes: 60,
          tzid: "UTC",
          title: "Supper",
          description: "",
          location: "Hall",
          recurrenceRule: "",
          capacity: null,
          waitlistEnabled: false,
          guestsAllowed: 0,
        }}
        idPrefix="test2"
        onSaved={() => {}}
      />
    );
    const capacity = screen.getByLabelText(/^Seat limit/);
    expect(capacity).toHaveValue(null);
    expect(capacity).toHaveAttribute("placeholder", "No limit");
  });
});

it("validates the childcare limit and saves the parent information", async () => {
  vi.mocked(Events.update).mockClear();
  render(<SeriesEditor series={{ eventId: 7, startsLocal: "2026-12-01T19:00", durationMinutes: 60, tzid: "UTC",
    title: "Family night", description: "", location: "Hall", recurrenceRule: "", capacity: null,
    waitlistEnabled: false, guestsAllowed: 0, childcareAvailable: true, childcareCapacity: 4, childcareAges: "2–5", childcareNotes: "Nursery",
  }} idPrefix="childcare" onSaved={() => {}} />);
  const cap = screen.getByLabelText(/^Childcare limit/);
  expect(cap).toHaveValue("4");
  await userEvent.clear(cap);
  await userEvent.type(cap, "0");
  await userEvent.click(screen.getByRole("button", { name: "Save series" }));
  await waitFor(() => expect(cap).toHaveFocus());
  expect(cap).toHaveAttribute("aria-invalid", "true");
  expect(Events.update).not.toHaveBeenCalled();
  await userEvent.clear(cap);
  await userEvent.type(cap, "5");
  await userEvent.click(screen.getByRole("button", { name: "Save series" }));
  expect(Events.update).toHaveBeenCalledWith(7, expect.objectContaining({ childcareAvailable: true, childcareCapacity: 5, childcareAges: "2–5", childcareNotes: "Nursery" }));
});

const baseSeries = {
  eventId: 7, startsLocal: "2026-12-02T19:00", durationMinutes: 90, tzid: "America/Denver",
  title: "Supper", description: "", location: "Hall", recurrenceRule: "FREQ=WEEKLY;BYDAY=WE",
  capacity: null, waitlistEnabled: false, guestsAllowed: 0,
};

describe("SeriesEditor while a save is on its way", () => {
  it("locks every field and keeps focus in the form until the answer, then gives both back", async () => {
    const put = deferred<{ event: ReturnType<typeof testEvent> }>();
    vi.mocked(Events.update).mockReset().mockReturnValueOnce(put.promise);
    const onSaved = vi.fn();
    render(<SeriesEditor series={baseSeries} idPrefix="lock" onSaved={onSaved} onClose={() => {}} />);
    const title = screen.getByRole("textbox", { name: "Title" });
    await userEvent.type(title, " night{Enter}");
    expect(Events.update).toHaveBeenCalledWith(7, expect.objectContaining({ title: "Supper night" }));
    const form = title.closest("form")!;
    expect(document.activeElement).toBe(form);

    for (const control of [title, screen.getByRole("textbox", { name: /^Description/ }), screen.getByLabelText("First date"),
      screen.getByLabelText("Reminder email"), screen.getByRole("checkbox", { name: /^Push reminder/ }), screen.getByLabelText(/^Seat limit/),
      screen.getByRole("button", { name: "More options" }), screen.getByRole("button", { name: "Saving…" }), screen.getByRole("button", { name: "Close" })]) {
      expect(control).toBeDisabled();
    }

    await userEvent.type(title, " and songs");
    expect(title).toHaveValue("Supper night");
    await act(async () => { put.reject(new Error("offline")); await put.promise.catch(() => {}); });
    expect(onSaved).not.toHaveBeenCalled();
    expect(title).toBeEnabled();
    expect(title).toHaveValue("Supper night");
    expect(document.activeElement).toBe(title);
    await userEvent.type(title, " and songs");
    expect(title).toHaveValue("Supper night and songs");
  });

  it("focuses the field a refused save names once the fields can take focus again", async () => {
    vi.mocked(Events.update).mockReset().mockRejectedValueOnce(new ApiError(400, "bad seats", { code: "invalid_capacity" }));
    render(<SeriesEditor series={baseSeries} idPrefix="refused" onSaved={() => {}} />);
    await userEvent.click(screen.getByRole("button", { name: "Save series" }));
    const seats = screen.getByLabelText(/^Seat limit/);
    await waitFor(() => expect(seats).toHaveFocus());
    expect(seats).toBeEnabled();
  });

  it("leaves focus where the person moved it when a refused save names a field", async () => {
    const put = deferred<{ event: ReturnType<typeof testEvent> }>();
    vi.mocked(Events.update).mockReset().mockReturnValueOnce(put.promise);
    render(<><button>Elsewhere</button><SeriesEditor series={baseSeries} idPrefix="moved" onSaved={() => {}} /></>);
    await userEvent.click(screen.getByRole("button", { name: "Save series" }));
    const elsewhere = screen.getByRole("button", { name: "Elsewhere" });
    elsewhere.focus();
    await act(async () => { put.reject(new ApiError(400, "bad seats", { code: "invalid_capacity" })); await put.promise.catch(() => {}); });
    expect(screen.getByLabelText(/^Seat limit/)).toBeEnabled();
    expect(document.activeElement).toBe(elsewhere);
  });

  it("is locked, with no save, while another run in its panel is on its way", async () => {
    vi.mocked(Events.update).mockReset();
    render(<SeriesEditor series={baseSeries} idPrefix="locked" onSaved={() => {}} onClose={() => {}} locked />);
    expect(screen.getByRole("textbox", { name: "Title" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Save series" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Close" })).toBeDisabled();
  });
});

describe("SeriesEditor form layout (DEC-95)", () => {
  it("shares the create forms' order, names the event's zone and opens More options", () => {
    render(<SeriesEditor series={baseSeries} idPrefix="order" onSaved={() => {}} />);
    expect(screen.getByRole("button", { name: "More options" })).toHaveAttribute("aria-expanded", "true");
    expect(screen.getByText("Mountain time")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Every week" })).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByRole("group", { name: "Repeat on" })).toBeInTheDocument();
    const form = document.querySelector("form")!;
    const order = ["Title", "Description", "When", "First date", "Repeats", "Location", "More options", "Seat limit", "Online", "Childcare"];
    const nodes = order.map((t) => Array.from(form.querySelectorAll("label, h2, h3, p, button")).find((n) => n.textContent?.trim().startsWith(t))!);
    nodes.forEach((n, i) => {
      expect(n, order[i]).toBeTruthy();

      if (i > 0) expect(nodes[i - 1].compareDocumentPosition(n) & Node.DOCUMENT_POSITION_FOLLOWING, order[i]).toBeTruthy();
    });
  });

  it("opens on Other, with its minutes, when the length is not 1, 1½ or 2 hours", async () => {
    vi.mocked(Events.update).mockClear();
    render(<SeriesEditor series={{ ...baseSeries, durationMinutes: 45 }} idPrefix="other" onSaved={() => {}} />);
    expect(screen.getByRole("button", { name: "Other" })).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByLabelText("Duration (minutes)")).toHaveValue(45);
    await userEvent.click(screen.getByRole("button", { name: "2 hr" }));
    await userEvent.click(screen.getByRole("button", { name: "Save series" }));
    expect(Events.update).toHaveBeenCalledWith(7, expect.objectContaining({ durationMinutes: 120, tzid: "America/Denver" }));
  });

  it("leaves the seat fields out of the body until they change", async () => {
    vi.mocked(Events.update).mockClear();
    render(<SeriesEditor series={baseSeries} idPrefix="seats0" onSaved={() => {}} />);
    await userEvent.click(screen.getByRole("button", { name: "Save series" }));
    const untouched = vi.mocked(Events.update).mock.calls[0][1];
    expect(untouched).not.toHaveProperty("capacity");
    expect(untouched).not.toHaveProperty("waitlistEnabled");
  });

  it("sends the seat limit and waitlist once changed", async () => {
    vi.mocked(Events.update).mockClear();
    render(<SeriesEditor series={baseSeries} idPrefix="seats" onSaved={() => {}} />);
    await userEvent.type(screen.getByLabelText(/^Seat limit/), "12");
    await userEvent.click(screen.getByLabelText(/^Waitlist when full/));
    await userEvent.click(screen.getByRole("button", { name: "Save series" }));
    expect(Events.update).toHaveBeenLastCalledWith(7, expect.objectContaining({ capacity: 12, waitlistEnabled: true }));
  });

  it("shows the series' reminder time and sends it only once changed (M3.6b)", async () => {
    vi.mocked(Events.update).mockClear();
    const first = render(<SeriesEditor series={{ ...baseSeries, reminderEmailMinutes: 120 }} idPrefix="rem" onSaved={() => {}} />);
    expect(screen.getByLabelText("Reminder email")).toHaveValue("120");
    await userEvent.click(screen.getByRole("button", { name: "Save series" }));
    expect(vi.mocked(Events.update).mock.calls[0][1]).not.toHaveProperty("reminderEmailMinutes");
    first.unmount();
    render(<SeriesEditor series={{ ...baseSeries, reminderEmailMinutes: 120 }} idPrefix="rem2" onSaved={() => {}} />);
    await userEvent.selectOptions(screen.getByLabelText("Reminder email"), "Off");
    await userEvent.click(screen.getByRole("button", { name: "Save series" }));
    expect(Events.update).toHaveBeenLastCalledWith(7, expect.objectContaining({ reminderEmailMinutes: 0 }));
  });

  it("shows the series' push reminder switch and sends it only once changed (M3.13d3)", async () => {
    vi.mocked(Events.update).mockClear();
    const first = render(<SeriesEditor series={{ ...baseSeries, reminderPush: false }} idPrefix="push" onSaved={() => {}} />);
    expect(await screen.findByRole("checkbox", { name: /^Push reminder 2 hours before/ })).not.toBeChecked();
    await userEvent.click(screen.getByRole("button", { name: "Save series" }));
    expect(vi.mocked(Events.update).mock.calls[0][1]).not.toHaveProperty("reminderPush");
    first.unmount();
    render(<SeriesEditor series={baseSeries} idPrefix="push2" onSaved={() => {}} />);
    const push = await screen.findByRole("checkbox", { name: /^Push reminder 2 hours before/ });
    expect(push).toBeChecked(); // absent reads as on
    await userEvent.click(push);
    await userEvent.click(screen.getByRole("button", { name: "Save series" }));
    expect(Events.update).toHaveBeenLastCalledWith(7, expect.objectContaining({ reminderPush: false }));
  });

  it("sends nothing more after a save its page has not closed it for yet: nothing to change, no Saving… left (#427)", async () => {
    vi.mocked(Events.update).mockReset().mockResolvedValue({ event: { ...testEvent(7), revision: "r9" } });
    const onSaved = vi.fn();
    render(<SeriesEditor series={baseSeries} idPrefix="again" onSaved={onSaved} onClose={() => {}} />);
    const title = screen.getByRole("textbox", { name: "Title" });
    await userEvent.type(title, " night");
    await userEvent.click(screen.getByRole("button", { name: "Save series" }));
    await waitFor(() => expect(onSaved).toHaveBeenCalledOnce());
    expect(screen.getByRole("button", { name: "Save series" })).toBeDisabled();
    expect(title).toBeDisabled();
    expect(title).toHaveValue("Supper night");
    expect(screen.getByRole("button", { name: "Close" })).toBeEnabled();
    fireEvent.submit(title.closest("form")!);
    expect(Events.update).toHaveBeenCalledOnce();
  });

  it("keeps showing what it sent when the page's reload brings new values", async () => {
    vi.mocked(Events.update).mockReset().mockResolvedValue({ event: { ...testEvent(7), revision: "r9" } });
    const series = { ...baseSeries, recurrenceRule: "FREQ=WEEKLY;COUNT=4" };
    const onSaved = vi.fn();
    const view = render(<SeriesEditor series={series} idPrefix="flip" onSaved={onSaved} onClose={() => {}} />);
    const rule = screen.getByRole("textbox", { name: "Recurrence rule" });
    await userEvent.clear(rule);
    await userEvent.type(rule, "FREQ=WEEKLY;BYDAY=WE,TH");
    await userEvent.click(screen.getByRole("button", { name: "Save series" }));
    await waitFor(() => expect(onSaved).toHaveBeenCalledOnce());
    view.rerender(<SeriesEditor series={{ ...series, recurrenceRule: "FREQ=WEEKLY;BYDAY=WE,TH", title: "Renamed" }} idPrefix="flip" onSaved={onSaved} onClose={() => {}} />);
    expect(screen.getByRole("textbox", { name: "Recurrence rule" })).toHaveValue("FREQ=WEEKLY;BYDAY=WE,TH");
    expect(screen.getByRole("textbox", { name: "Title" })).toHaveValue("Supper");
  });

  it("leaves the Jitsi link as sent when a new one lands after the save began", async () => {
    const jitsi = deferred<{ url: string }>();
    vi.mocked(Events.generateJitsi).mockReturnValueOnce(jitsi.promise);
    const put = deferred<{ event: ReturnType<typeof testEvent> }>();
    vi.mocked(Events.update).mockReset().mockReturnValueOnce(put.promise);
    const sent = "https://meet.jit.si/fold-old000link";
    const onSaved = vi.fn();
    render(<SeriesEditor series={{ ...baseSeries, videoProvider: "jitsi", videoUrl: sent }} idPrefix="jitsi" onSaved={onSaved} onClose={() => {}} />);
    await userEvent.click(screen.getByRole("button", { name: "Generate a new Jitsi link" }));
    await userEvent.click(screen.getByRole("button", { name: "Save series" }));
    await act(async () => { jitsi.resolve({ url: "https://meet.jit.si/fold-new111link" }); await jitsi.promise; });
    const link = screen.getByRole("textbox", { name: /^Video link/ });
    expect(link).toHaveValue(sent);
    await act(async () => { put.resolve({ event: { ...testEvent(7), revision: "r9" } }); await put.promise; });
    expect(onSaved).toHaveBeenCalledOnce();
    expect(link).toHaveValue(sent);
  });

  it("asks nothing when leaving after a save of an edit", async () => {
    vi.mocked(Events.update).mockReset().mockResolvedValue({ event: { ...testEvent(7), revision: "r9" } });
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
    const onSaved = vi.fn();

    const router = createMemoryRouter([
      { path: "/", element: <><UnsavedChangesBlocker /><SeriesEditor series={baseSeries} idPrefix="stay" onSaved={onSaved} onClose={() => {}} /></> },
      { path: "/elsewhere", element: <p>Elsewhere page</p> },
    ]);

    render(<RouterProvider router={router} />);
    await userEvent.type(screen.getByRole("textbox", { name: "Title" }), " night");
    await userEvent.click(screen.getByRole("button", { name: "Save series" }));
    await waitFor(() => expect(onSaved).toHaveBeenCalledOnce());
    const leaving = new Event("beforeunload", { cancelable: true });
    window.dispatchEvent(leaving);
    expect(leaving.defaultPrevented).toBe(false);
    void router.navigate("/elsewhere");
    expect(await screen.findByText("Elsewhere page")).toBeInTheDocument();
    expect(confirm).not.toHaveBeenCalled();
    confirm.mockRestore();
  });

  it("reads a missing reminder time as the default, 1 day before", () => {
    render(<SeriesEditor series={baseSeries} idPrefix="rem0" onSaved={() => {}} />);
    expect(screen.getByLabelText("Reminder email")).toHaveValue("1440");
  });

  it("opens a closed More options and focuses the field with the problem", async () => {
    vi.mocked(Events.update).mockClear();
    render(<SeriesEditor series={{ ...baseSeries, remoteAvailable: true, remoteCapacity: 0 }} idPrefix="closed" onSaved={() => {}} />);
    const toggle = screen.getByRole("button", { name: "More options" });
    await userEvent.click(toggle);
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    await userEvent.click(screen.getByRole("button", { name: "Save series" }));
    expect(toggle).toHaveAttribute("aria-expanded", "true");
    const cap = screen.getByLabelText(/^Remote seat limit/);
    await waitFor(() => expect(cap).toHaveFocus());
    expect(Events.update).not.toHaveBeenCalled();
  });

  it("asks before leaving unsaved edits, and not after a save", async () => {
    vi.mocked(Events.update).mockReset().mockResolvedValue({ event: { ...testEvent(7), revision: "r9" } });
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);

    const router = createMemoryRouter(
      [
        { path: "/", element: <><UnsavedChangesBlocker /><SeriesEditor series={baseSeries} idPrefix="guard" onSaved={() => { void router.navigate("/done"); }} /></> },
        { path: "/elsewhere", element: <p>Elsewhere page</p> },
        { path: "/done", element: <p>Done page</p> },
      ],
    );

    render(<RouterProvider router={router} />);
    await userEvent.type(screen.getByRole("textbox", { name: "Title" }), "!");
    void router.navigate("/elsewhere");
    await waitFor(() => expect(confirm).toHaveBeenCalledWith("Leave without saving? Your changes will be lost."));
    expect(screen.queryByText("Elsewhere page")).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Save series" }));
    expect(await screen.findByText("Done page")).toBeInTheDocument();
    expect(confirm).toHaveBeenCalledTimes(1);
    confirm.mockRestore();
  });

  describe("Room control (M2.1b)", () => {
    const single = { eventId: 7, startsLocal: "2026-12-01T19:00", durationMinutes: 60, tzid: "UTC", title: "Supper", description: "", location: "Hall", recurrenceRule: "", capacity: null, waitlistEnabled: false, guestsAllowed: 0 };

    const roomsOn = () => {
      vi.mocked(Site.get).mockResolvedValue({ name: "Fold", timezone: "UTC", selfSignup: "off", roomsEnabled: true, pushEnabled: true });
      vi.mocked(DateRooms.list).mockResolvedValue({ rooms: [], booking: null, dateRoom: { id: 70, revision: "r1" } });
    };

    it("sits with Location for a single-date event, outside More options", async () => {
      roomsOn();
      render(<SeriesEditor series={single} roomDate="2026-12-01" idPrefix="room" onSaved={() => {}} />);
      const room = await screen.findByRole("combobox", { name: /^Room/ });
      expect(DateRooms.list).toHaveBeenCalledWith(7, "2026-12-01");
      const more = screen.getByRole("button", { name: "More options" });
      expect(screen.getByRole("textbox", { name: /^Location/ }).compareDocumentPosition(room) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
      expect(room.compareDocumentPosition(more) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    });

    it("has no one-date Room for a series, whatever the date", async () => {
      roomsOn();
      vi.mocked(SeriesRooms.list).mockResolvedValue({ revision: "r1", rooms: [], dates: 3, defaultRoom: null, ownRooms: [], needsRoom: [], pending: 0 });
      vi.mocked(DateRooms.list).mockClear();
      render(<SeriesEditor series={{ ...single, recurrenceRule: "FREQ=WEEKLY;BYDAY=TU" }} roomDate="2026-12-01" idPrefix="room-series" onSaved={() => {}} />);
      await screen.findByRole("button", { name: "Save series" });
      expect(screen.queryByRole("combobox", { name: /^Room \(optional\)/ })).not.toBeInTheDocument();
      expect(DateRooms.list).not.toHaveBeenCalled();
    });

    it("gives a series Room for every date beside Location, only while rooms are on", async () => {
      roomsOn();
      vi.mocked(SeriesRooms.list).mockResolvedValue({ revision: "r1", rooms: [], dates: 10, defaultRoom: null, ownRooms: [], needsRoom: [], pending: 0 });
      const series = { ...single, recurrenceRule: "FREQ=WEEKLY;BYDAY=TU" };
      const { unmount } = render(<SeriesEditor series={series} idPrefix="room-hint" onSaved={() => {}} />);
      const room = await screen.findByRole("combobox", { name: /^Room for every date/ });
      expect(SeriesRooms.list).toHaveBeenCalledWith(7);
      const more = screen.getByRole("button", { name: "More options" });
      expect(screen.getByRole("textbox", { name: /^Location/ }).compareDocumentPosition(room) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
      expect(room.compareDocumentPosition(more) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
      unmount();
      forgetSite();
      vi.mocked(Site.get).mockResolvedValue({ name: "Fold", timezone: "UTC", selfSignup: "off", roomsEnabled: false, pushEnabled: true });
      vi.mocked(SeriesRooms.list).mockClear();
      render(<SeriesEditor series={series} idPrefix="room-hint-off" onSaved={() => {}} />);
      await screen.findByRole("button", { name: "Save series" });
      expect(screen.queryByRole("combobox", { name: /^Room/ })).not.toBeInTheDocument();
      expect(SeriesRooms.list).not.toHaveBeenCalled();
    });

    it("is hidden when rooms are off", async () => {
      vi.mocked(Site.get).mockResolvedValue({ name: "Fold", timezone: "UTC", selfSignup: "off", roomsEnabled: false, pushEnabled: true });
      vi.mocked(DateRooms.list).mockClear();
      render(<SeriesEditor series={single} roomDate="2026-12-01" idPrefix="room-off" onSaved={() => {}} />);
      await waitFor(() => expect(Site.get).toHaveBeenCalled());
      expect(screen.queryByRole("combobox", { name: /^Room/ })).not.toBeInTheDocument();
      expect(DateRooms.list).not.toHaveBeenCalled();
    });
  });
});

// W5: a series save refused for dates someone who left kept (DEC-156).
describe("SeriesEditor and kept dates", () => {
  const A = "2026-12-01";
  const B = "2026-12-08";
  const series = { eventId: 7, startsLocal: "2026-12-01T19:00", durationMinutes: 60, tzid: "UTC", title: "Supper", description: "", location: "Hall", recurrenceRule: "FREQ=WEEKLY;BYDAY=TU", capacity: null, waitlistEnabled: false, guestsAllowed: 0 };
  const read = (date: string, over: Partial<EventOccurrence> = {}): OccurrenceResponse => ({ occurrence: occurrence({ date, attendeesVisible: true, keptCount: 1, attendees: [keptPerson("Old Name"), careKid()], ...over }) });
  const both = [{ date: A, keptCount: 1 }, { date: B, keptCount: 1 }];
  const home = () => screen.getByRole("region", { name: KEPT.home });

  function mount(onAuthorityLost = vi.fn()) {
    const router = createMemoryRouter([{ path: "/", element: <RosterWritesProvider><SeriesEditor series={series} idPrefix="kept" onSaved={() => {}} onAuthorityLost={onAuthorityLost} /></RosterWritesProvider> }]);
    render(<RouterProvider router={router} />);

    return { onAuthorityLost };
  }

  async function typeTitleAndSave() {
    const title = screen.getByRole("textbox", { name: "Title" });
    await userEvent.clear(title);
    await userEvent.type(title, "Typed title");
    await userEvent.click(screen.getByRole("button", { name: "Save series" }));
  }

  const draftKept = () => expect(screen.getByRole("textbox", { name: "Title" })).toHaveValue("Typed title");

  beforeEach(() => {
    vi.mocked(Events.update).mockReset();
    vi.mocked(Events.managedDate).mockReset();
    vi.mocked(Events.removeRegistration).mockReset();
    vi.mocked(Events.managedDate).mockImplementation((_id, date) => Promise.resolve(read(date)));
    vi.mocked(Events.removeRegistration).mockResolvedValue({});
  });

  it("shows one date-only section per date, no count and the save line", async () => {
    vi.mocked(Events.update).mockRejectedValueOnce(kept409(both));
    mount();
    await typeTitleAndSave();
    await screen.findByText("Old Name");
    expect(within(home()).getByRole("heading", { name: formatDayFull(A) })).toBeInTheDocument();
    expect(within(home()).getByRole("heading", { name: formatDayFull(B) })).toBeInTheDocument();
    expect(within(home()).getByRole("alert")).toHaveTextContent(KEPT.saveLine);
    expect(document.body.textContent).not.toMatch(KEPT_COUNT);
    expect(document.body.textContent).not.toContain(ERROR_COPY.date_kept_after_leaving);
    expect(document.querySelector("form")?.contains(home())).toBe(false);
    draftKept();
  });

  it.each([["missing", undefined], ["empty", []], ["unreadable", [{ when: A }]]])("with a %s list shows the ERROR_COPY line alone and no panel", async (_name, list) => {
    vi.mocked(Events.update).mockRejectedValueOnce(kept409(list));
    mount();
    await typeTitleAndSave();
    expect(await screen.findByRole("alert")).toHaveTextContent(ERROR_COPY.date_kept_after_leaving);
    expect(screen.queryByRole("region", { name: KEPT.home })).not.toBeInTheDocument();
    expect(document.body.textContent).not.toContain(formatDayFull(A));
    expect(Events.managedDate).not.toHaveBeenCalled();
    draftKept();
  });

  it("a refused panel read clears the kept state and focuses Save when focus was in the panel", async () => {
    vi.mocked(Events.update).mockRejectedValueOnce(kept409(both));
    const { onAuthorityLost } = mount();
    await typeTitleAndSave();
    await screen.findByText("Old Name");
    screen.getByRole("button", { name: "Manage sign-ups" }).focus();
    vi.mocked(Events.managedDate).mockRejectedValueOnce(readFailures[403]());
    await userEvent.keyboard("{Enter}");
    await waitFor(() => expect(onAuthorityLost).toHaveBeenCalledTimes(1));
    expect(screen.queryByRole("region", { name: KEPT.home })).not.toBeInTheDocument();
    expect(screen.getByRole("alert")).toHaveTextContent(KEPT.refused);
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Save series" }));
    draftKept();
  });

  // W4b's events (iii-b) and (v) on the series host.
  for (const event of ["iii-b", "v"] as const) {
    for (const outcome of READ_OUTCOMES) {
      it(`event (${event}) settled ${outcome} keeps the draft and shows nothing kept-specific while pending`, async () => {
        if (event === "iii-b") vi.mocked(Events.managedDate).mockResolvedValueOnce(read(A, { attendeesVisible: false, attendees: undefined }));
        vi.mocked(Events.update).mockRejectedValueOnce(kept409(both));
        const { onAuthorityLost } = mount();
        await typeTitleAndSave();
        await screen.findByText(event === "iii-b" ? KEPT.nobodyHere : "Old Name");
        const held = deferred<OccurrenceResponse>();
        vi.mocked(Events.managedDate).mockReturnValueOnce(held.promise);
        const seen = observeKept([A, B]);

        if (event === "iii-b") act(() => { window.dispatchEvent(new Event("focus")); });
        else await userEvent.click(screen.getByRole("button", { name: "Manage sign-ups" }));
        expect(keptForbiddenNow([A, B])).toEqual([]);
        expect(home()).toHaveTextContent(KEPT.checking);
        seen.stop();
        seen.expectClean();
        const date = event === "v" ? B : A;
        await settleRead(held, outcome, date);

        if (outcome === "success") expect(await screen.findByText("New Name")).toBeInTheDocument();
        else if (outcome === 403 || outcome === 404) {
          await waitFor(() => expect(onAuthorityLost).toHaveBeenCalledTimes(1));
          expect(screen.getByRole("alert")).toHaveTextContent(KEPT.refused);
        } else expect(home()).toHaveTextContent(KEPT.unknown);
        draftKept();
      });
    }
  }

  async function settleRead(held: ReturnType<typeof deferred<OccurrenceResponse>>, outcome: ReadOutcome, date: string) {
    await act(async () => {
      if (outcome === "success") held.resolve(read(date, { attendees: [keptPerson("New Name")] }));
      else held.reject(readFailures[outcome]());
      await held.promise.catch(() => {});
    });
  }

  it("the race across dates: no line ever calls the dates clear together, and a new refusal starts fresh", async () => {
    // The server's state, which the mock reads: who keeps each date.
    const server = new Map<string, Attendee[]>([[A, [keptPerson("Old Name")]], [B, []]]);
    vi.mocked(Events.managedDate).mockImplementation((_id, date) => Promise.resolve(read(date, { attendees: server.get(date) ?? [], keptCount: (server.get(date) ?? []).filter((a) => a.keptAfterLeaving).length })));
    vi.mocked(Events.removeRegistration).mockImplementation(() => { server.set(A, []);

 return Promise.resolve({}); });
    vi.mocked(Events.update).mockRejectedValueOnce(kept409(both));
    mount();
    const seen = observeKept([A, B]);
    await typeTitleAndSave();
    await screen.findByText("Old Name");
    await userEvent.click(screen.getByRole("button", { name: "More for Old Name" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Remove Old Name" }));
    await userEvent.click(screen.getByRole("button", { name: "Remove" }));
    expect(await screen.findByText(KEPT.nobodyHere)).toBeInTheDocument();
    // Another member leaves and keeps A.
    server.set(A, [keptPerson("Third Name", { registrationId: 33, userId: 12 })]);
    await userEvent.click(screen.getByRole("button", { name: "Manage sign-ups" }));
    expect(await screen.findByText(KEPT.nobodyHere)).toBeInTheDocument();
    const sectionA = within(home()).getByRole("heading", { name: formatDayFull(A) }).parentElement;
    expect(sectionA?.textContent).toBe(`${formatDayFull(A)}Manage sign-ups`);
    seen.stop();

    for (const s of seen.states) expect(s.text).not.toMatch(/these dates now|Save again to apply your change/);
    const held = deferred<OccurrenceResponse>();
    vi.mocked(Events.managedDate).mockReturnValueOnce(held.promise);
    const save = deferred<Awaited<ReturnType<typeof Events.update>>>();
    vi.mocked(Events.update).mockReturnValueOnce(save.promise);
    await userEvent.click(screen.getByRole("button", { name: "Save series" }));
    const again = observeKept([A, B]);
    await act(async () => { save.reject(kept409([{ date: A, keptCount: 1 }])); await save.promise.catch(() => {}); });
    await waitFor(() => expect(Events.managedDate).toHaveBeenLastCalledWith(7, A));
    expect(document.body.textContent).toContain(KEPT.checking);
    again.stop();
    again.expectClean();
    await act(async () => { held.resolve(read(A, { attendees: server.get(A) ?? [], keptCount: 1 })); await held.promise; });
    expect(screen.getByText("Third Name")).toBeInTheDocument();
    expect(within(home()).getByRole("alert")).toHaveTextContent(ERROR_COPY.date_kept_after_leaving);
    draftKept();
  });
});

describe("SeriesEditor after a save", () => {
  const single = { eventId: 7, startsLocal: "2026-12-01T19:00", durationMinutes: 60, tzid: "UTC", title: "Supper", description: "", location: "Hall", recurrenceRule: "", capacity: null, waitlistEnabled: false, guestsAllowed: 0 };

  beforeEach(() => {
    forgetSite();
    vi.mocked(Events.update).mockReset().mockResolvedValue({ event: { ...testEvent(7), revision: "r9" } });
  });

  it("keeps the room field it opened with when a read makes the event a series", async () => {
    vi.mocked(Site.get).mockResolvedValue({ name: "Fold", timezone: "UTC", selfSignup: "off", roomsEnabled: true, pushEnabled: true });
    vi.mocked(DateRooms.list).mockResolvedValue({ rooms: [], booking: null, dateRoom: { id: 70, revision: "r1" } });
    const onSaved = vi.fn();
    const view = render(<SeriesEditor series={single} roomDate="2026-12-01" idPrefix="weekly" onSaved={onSaved} onClose={() => {}} />);
    expect(await screen.findByRole("combobox", { name: /^Room/ })).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Every week" }));
    await userEvent.click(screen.getByRole("button", { name: "Save series" }));
    expect(Events.update).toHaveBeenLastCalledWith(7, expect.objectContaining({ recurrenceRule: expect.stringMatching(/^FREQ=WEEKLY/) }));
    await waitFor(() => expect(onSaved).toHaveBeenCalledOnce());
    view.rerender(<SeriesEditor series={{ ...single, recurrenceRule: "FREQ=WEEKLY;BYDAY=TU" }} idPrefix="weekly" onSaved={onSaved} onClose={() => {}} />);
    expect(screen.getByRole("combobox", { name: /^Room/ })).toBeInTheDocument();
    expect(SeriesRooms.list).not.toHaveBeenCalled();
  });
});

stubService(Events, () => ({ get: vi.fn() }));

// DEC-162: the save names the read the editor opened with; a refusal is checked against the event as stored.
describe("SeriesEditor and the read it opened with", () => {
  const opened = { ...baseSeries, revision: "r1" };

  const stored: EventSeries = {
    ...testEvent(7), ...baseSeries, startsAt: "2026-12-03T02:00:00Z", endsAt: "2026-12-03T03:30:00Z", revision: "r2",
  };

  const readOf = (event: EventSeries, canManage = true): EventDetailResponse => ({ event, occurrences: [], canManage, canSetVisibility: true });
  const changed = () => new ApiError(409, "changed", { code: "event_changed" });
  const saveButton = () => screen.getByRole("button", { name: "Save series" });

  beforeEach(() => {
    vi.mocked(Events.update).mockReset();
    vi.mocked(Events.get).mockReset();
  });

  it("sends the revision it opened with, also after the page hands it a newer read", async () => {
    vi.mocked(Events.update).mockResolvedValue({ event: stored });
    const { rerender } = render(<SeriesEditor series={opened} idPrefix="rev" onSaved={() => {}} />);
    rerender(<SeriesEditor series={{ ...opened, title: "Brunch", capacity: 9, revision: "r2" }} idPrefix="rev" onSaved={() => {}} />);
    expect(screen.getByRole("textbox", { name: "Title" })).toHaveValue("Supper");
    expect(screen.getByLabelText(/^Seat limit/)).toHaveValue(null);
    await userEvent.click(saveButton());
    expect(Events.update).toHaveBeenCalledExactlyOnceWith(7, expect.objectContaining({ title: "Supper", revision: "r1" }));
    expect(vi.mocked(Events.update).mock.calls[0][1]).not.toHaveProperty("capacity");
  });

  it("saves again under the new revision, once, when nothing it can write was changed", async () => {
    vi.mocked(Events.update).mockRejectedValueOnce(changed()).mockResolvedValueOnce({ event: stored });
    // The editor opened with every optional value absent; the stored read spells each one out as its default.
    vi.mocked(Events.get).mockResolvedValue(readOf(stored));
    const onSaved = vi.fn();
    const onConflict = vi.fn();
    render(<SeriesEditor series={opened} idPrefix="again" onSaved={onSaved} onConflict={onConflict} />);
    await userEvent.type(screen.getByRole("textbox", { name: "Title" }), " night");
    await userEvent.click(saveButton());
    await waitFor(() => expect(onSaved).toHaveBeenCalledOnce());
    expect(Events.get).toHaveBeenCalledExactlyOnceWith(7);
    expect(Events.update).toHaveBeenCalledTimes(2);
    const [first, second] = vi.mocked(Events.update).mock.calls.map(([, body]) => body);
    expect(first).toMatchObject({ title: "Supper night", revision: "r1" });
    expect(second).toEqual({ ...first, revision: "r2" });
    expect(onConflict).not.toHaveBeenCalled();
    expect(screen.queryByText(/couldn't/)).not.toBeInTheDocument();
  });

  // DEC-162, DEC-167: the series room moves the event's revision, and is not a field this editor writes.
  it("saves by itself after its own series room pick moved the event's revision", async () => {
    forgetSite();
    vi.mocked(Site.get).mockResolvedValue({ name: "Fold", timezone: "UTC", selfSignup: "off", roomsEnabled: true, pushEnabled: true });
    const hall = { id: 1, name: "Fellowship Hall", building: "Main", capacity: 80, features: "", restricted: false, busyDates: 0, tooSmall: false };
    vi.mocked(SeriesRooms.list).mockResolvedValue({ revision: "r1", rooms: [hall], dates: 10, defaultRoom: null, ownRooms: [], needsRoom: [], pending: 0 });
    vi.mocked(SeriesRooms.set).mockResolvedValue({ event: { id: 7, revision: "r2" }, defaultRoom: { id: 1, name: "Fellowship Hall", building: "Main" }, booked: 10, lost: [], kept: 0, keptWaiting: 0, keptInUse: 0, changed: 0, pending: 0, doubleBooked: 0, after: { ownRooms: [], needsRoom: [], pending: 0 } });
    vi.mocked(Events.update).mockRejectedValueOnce(changed()).mockResolvedValueOnce({ event: stored });
    vi.mocked(Events.get).mockResolvedValue(readOf(stored));
    const onSaved = vi.fn();
    const onConflict = vi.fn();
    render(<SeriesEditor series={opened} idPrefix="own-room" onSaved={onSaved} onConflict={onConflict} />);
    await userEvent.type(screen.getByRole("textbox", { name: "Title" }), " night");
    await userEvent.selectOptions(await screen.findByRole("combobox", { name: /^Room for every date/ }), "1");
    await userEvent.click(screen.getByRole("button", { name: "Book for every date" }));
    await waitFor(() => expect(SeriesRooms.set).toHaveBeenCalledExactlyOnceWith(7, { roomId: 1 }, "r1"));
    expect(await screen.findByText("Booked for 10 dates.")).toBeInTheDocument();
    await userEvent.click(saveButton());
    await waitFor(() => expect(onSaved).toHaveBeenCalledOnce());
    expect(vi.mocked(Events.update).mock.calls.map(([, body]) => body.revision)).toEqual(["r1", "r2"]);
    expect(onConflict).not.toHaveBeenCalled();
  });

  it.each([
    ["an empty object (a page from something in between)", {}],
    ["another event's answer", { event: { ...stored, id: 8 } }],
    ["an answer with no revision", { event: { ...stored, revision: undefined } }],
  ])("keeps the draft and says the ordinary error, closing nothing, when the answer is %s", async (_, answer) => {
    // SAFETY: a wire answer the static type would not allow, as something in between could send it.
    vi.mocked(Events.update).mockResolvedValue(answer as Awaited<ReturnType<typeof Events.update>>);
    const onSaved = vi.fn();
    const onConflict = vi.fn();
    render(<SeriesEditor series={opened} idPrefix="answer" onSaved={onSaved} onConflict={onConflict} />);
    await userEvent.type(screen.getByRole("textbox", { name: "Title" }), " night");
    await userEvent.click(saveButton());
    expect(await screen.findByText("We couldn't update the series. Please try again in a minute.")).toBeInTheDocument();
    expect(onSaved).not.toHaveBeenCalled();
    expect(onConflict).not.toHaveBeenCalled();
    expect(screen.getByRole("textbox", { name: "Title" })).toHaveValue("Supper night");
    expect(saveButton()).toBeEnabled();
  });

  // #455: a send with no answer may have been stored, and someone may have put the opening value back since.
  it("after a send with no answer, a refusal finding the series as it opened is a conflict, sending the opened revision only", async () => {
    vi.mocked(Events.update).mockRejectedValueOnce(new TypeError("Failed to fetch")).mockRejectedValueOnce(changed()).mockResolvedValue({ event: stored });
    vi.mocked(Events.get).mockResolvedValue(readOf(stored));
    const onSaved = vi.fn();
    const onConflict = vi.fn();
    render(<SeriesEditor series={opened} idPrefix="lost" onSaved={onSaved} onConflict={onConflict} />);
    await userEvent.type(screen.getByRole("textbox", { name: "Title" }), " night");
    await userEvent.click(saveButton());
    expect(await screen.findByText("We couldn't reach Fold. Check your connection and try again.")).toBeInTheDocument();
    await userEvent.click(saveButton());
    await waitFor(() => expect(onConflict).toHaveBeenCalledExactlyOnceWith("changed"));
    expect(vi.mocked(Events.update).mock.calls.map(([, body]) => body.revision)).toEqual(["r1", "r1"]);
    expect(onSaved).not.toHaveBeenCalled();
  });

  it("compares every field a save can write", () => {
    type Writable = Exclude<keyof EventInput, "visibility" | "visibilityReason" | "roomId" | "hosts" | "revision">;

    type Compared = keyof ReturnType<typeof savedFields>;

    const exact: [Writable] extends [Compared] ? ([Compared] extends [Writable] ? true : false) : false = true;

    expect(exact).toBe(true);
  });

  // One stored value changed per row; typed as the normaliser's answer, so a field it compares cannot lack a row.
  const CHANGED: ReturnType<typeof savedFields> = {
    title: "Brunch", description: "Bring a dish", startsLocal: "2026-12-09T19:00", durationMinutes: 60, tzid: "UTC",
    location: "Library", recurrenceRule: "FREQ=WEEKLY;BYDAY=TH", rruleUntil: "2027-03-01", capacity: 12,
    waitlistEnabled: true, guestsAllowed: 2, reminderEmailMinutes: 120, reminderPush: false, videoProvider: "link",
    videoUrl: "https://meet.example.org/supper", videoReveal: "always", remoteAvailable: true, remoteCapacity: 8,
    childcareAvailable: true, childcareCapacity: 4, childcareAges: "2–5", childcareNotes: "Nursery",
  };

  it.each(Object.entries(CHANGED))("hands the page a conflict, sending nothing more, when %s was changed", async (key, value) => {
    const now: EventSeries = Object.assign({}, stored, { [key]: value });
    expect(savedFields(now)).not.toEqual(savedFields(stored));
    vi.mocked(Events.update).mockRejectedValue(changed());
    vi.mocked(Events.get).mockResolvedValue(readOf(now));
    const onSaved = vi.fn();
    const onConflict = vi.fn();
    render(<SeriesEditor series={opened} idPrefix={`conflict-${key}`} onSaved={onSaved} onConflict={onConflict} />);
    await userEvent.click(saveButton());
    await waitFor(() => expect(onConflict).toHaveBeenCalledExactlyOnceWith("changed"));
    expect(Events.update).toHaveBeenCalledOnce();
    expect(Events.get).toHaveBeenCalledOnce();
    expect(onSaved).not.toHaveBeenCalled();
  });

  it("shows the ordinary error, keeping the draft open, after three refusals with nothing changed", async () => {
    vi.mocked(Events.update).mockRejectedValue(changed());
    vi.mocked(Events.get).mockResolvedValue(readOf(stored));
    const onSaved = vi.fn();
    const onConflict = vi.fn();
    render(<SeriesEditor series={opened} idPrefix="bound" onSaved={onSaved} onConflict={onConflict} />);
    const title = screen.getByRole("textbox", { name: "Title" });
    await userEvent.type(title, " night");
    await userEvent.click(saveButton());
    expect(await screen.findByText("We couldn't update the series. Please try again.")).toBeInTheDocument();
    expect(Events.update).toHaveBeenCalledTimes(3);
    expect(Events.get).toHaveBeenCalledTimes(3);
    expect(onConflict).not.toHaveBeenCalled();
    expect(onSaved).not.toHaveBeenCalled();
    expect(title).toBeEnabled();
    expect(title).toHaveValue("Supper night");
    expect(saveButton()).toBeEnabled();
  });

  it.each([
    ["the server fails", new ApiError(500, "boom", { code: "server_error" }), "We couldn't update the series. Please try again in a minute."],
    ["the network fails", new TypeError("offline"), "We couldn't reach Fold. Check your connection and try again."],
  ])("shows the ordinary error, keeping the draft open, when the read after a refusal fails: %s", async (_name, cause, text) => {
    vi.mocked(Events.update).mockRejectedValue(changed());
    vi.mocked(Events.get).mockRejectedValue(cause);
    const onConflict = vi.fn();
    render(<SeriesEditor series={opened} idPrefix="read-fails" onSaved={() => {}} onConflict={onConflict} />);
    const title = screen.getByRole("textbox", { name: "Title" });
    await userEvent.type(title, " night");
    await userEvent.click(saveButton());
    expect(await screen.findByText(text)).toBeInTheDocument();
    expect(Events.update).toHaveBeenCalledOnce();
    expect(onConflict).not.toHaveBeenCalled();
    expect(title).toBeEnabled();
    expect(title).toHaveValue("Supper night");
  });

  it.each([
    ["answers 403", () => vi.mocked(Events.get).mockRejectedValue(new ApiError(403, "no", { code: "forbidden" }))],
    ["answers 404", () => vi.mocked(Events.get).mockRejectedValue(new ApiError(404, "gone", { code: "not_found" }))],
    ["no longer lets this person manage it", () => vi.mocked(Events.get).mockResolvedValue(readOf({ ...stored, revision: undefined }, false))],
  ])("hands the page the closing path with no notice when the read after a refusal %s", async (_name, answer) => {
    vi.mocked(Events.update).mockRejectedValue(changed());
    answer();
    const onSaved = vi.fn();
    const onConflict = vi.fn();
    render(<SeriesEditor series={opened} idPrefix="gone" onSaved={onSaved} onConflict={onConflict} />);
    await userEvent.click(saveButton());
    await waitFor(() => expect(onConflict).toHaveBeenCalledExactlyOnceWith("gone"));
    expect(Events.update).toHaveBeenCalledOnce();
    expect(onSaved).not.toHaveBeenCalled();
  });
  it("sends nothing more when the person has left the editor before the read after a refusal lands", async () => {
    vi.mocked(Events.update).mockRejectedValueOnce(changed()).mockResolvedValue({ event: stored });
    const read = deferred<EventDetailResponse>();
    vi.mocked(Events.get).mockReturnValue(read.promise);
    const onSaved = vi.fn();
    const onConflict = vi.fn();
    const view = render(<SeriesEditor series={opened} idPrefix="left" onSaved={onSaved} onConflict={onConflict} />);
    await userEvent.type(screen.getByRole("textbox", { name: "Title" }), " night");
    await userEvent.click(saveButton());
    await waitFor(() => expect(Events.get).toHaveBeenCalledOnce());
    view.unmount();
    await act(async () => { read.resolve(readOf(stored)); await read.promise; });
    expect(Events.update).toHaveBeenCalledOnce();
    expect(onSaved).not.toHaveBeenCalled();
    expect(onConflict).not.toHaveBeenCalled();
  });

  it("sends nothing more, and hands the page the closing path, when the tab's sign-in changed before the read after a refusal lands", async () => {
    const mine: core.Credential = { token: "t1", marker: "m1", viewer: 1, seen: "m1", verified: true };
    const held = vi.spyOn(core, "currentCredential").mockReturnValue(mine);
    const still = vi.spyOn(core, "credentialCurrent").mockReturnValue(true);
    vi.mocked(Events.update).mockRejectedValueOnce(changed()).mockResolvedValue({ event: stored });
    const read = deferred<EventDetailResponse>();
    vi.mocked(Events.get).mockReturnValue(read.promise);
    const onSaved = vi.fn();
    const onConflict = vi.fn();
    render(<SeriesEditor series={opened} idPrefix="other" onSaved={onSaved} onConflict={onConflict} />);
    await userEvent.click(saveButton());
    await waitFor(() => expect(Events.get).toHaveBeenCalledOnce());
    still.mockReturnValue(false);
    await act(async () => { read.resolve(readOf(stored)); await read.promise; });
    await waitFor(() => expect(onConflict).toHaveBeenCalledExactlyOnceWith("gone"));
    expect(still).toHaveBeenCalledWith(mine);
    expect(Events.update).toHaveBeenCalledOnce();
    expect(onSaved).not.toHaveBeenCalled();
    held.mockRestore();
    still.mockRestore();
  });

  it.each([403, 404])("hands the page the closing path when the save it sent again by itself answers %i", async (status) => {
    vi.mocked(Events.update).mockRejectedValueOnce(changed()).mockRejectedValueOnce(new ApiError(status, "no", { code: status === 403 ? "forbidden" : "not_found" }));
    vi.mocked(Events.get).mockResolvedValue(readOf(stored));
    const onSaved = vi.fn();
    const onConflict = vi.fn();
    render(<SeriesEditor series={opened} idPrefix={`resent-${status}`} onSaved={onSaved} onConflict={onConflict} />);
    await userEvent.type(screen.getByRole("textbox", { name: "Title" }), " night");
    await userEvent.click(saveButton());
    await waitFor(() => expect(onConflict).toHaveBeenCalledExactlyOnceWith("gone"));
    expect(Events.update).toHaveBeenCalledTimes(2);
    expect(onSaved).not.toHaveBeenCalled();
    expect(screen.queryByText(/couldn't/)).not.toBeInTheDocument();
  });
});

// DEC-163: a save whose answer was lost was stored; pressing Save again is refused, and the read shows the save done.
describe("SeriesEditor when a save it pressed before was stored and its answer lost", () => {
  const full = {
    ...baseSeries, revision: "r1", capacity: 12, videoProvider: "link" as const, videoUrl: "https://meet.example.org/supper",
    videoReveal: "registered_only" as const, childcareAvailable: true, childcareCapacity: 4, childcareAges: "2–5", childcareNotes: "Nursery",
  };

  const stored: EventSeries = { ...testEvent(7), ...full, startsAt: "2026-12-03T02:00:00Z", endsAt: "2026-12-03T03:30:00Z", revision: "r2" };
  const readOf = (event: EventSeries): EventDetailResponse => ({ event, occurrences: [], canManage: true, canSetVisibility: true });
  const changed = () => new ApiError(409, "changed", { code: "event_changed" });
  const saveButton = () => screen.getByRole("button", { name: "Save series" });
  const offline = "We couldn't reach Fold. Check your connection and try again.";

  beforeEach(() => {
    vi.mocked(Events.update).mockReset().mockRejectedValueOnce(new TypeError("Failed to fetch")).mockRejectedValueOnce(changed());
    vi.mocked(Events.get).mockReset();
  });

  async function pressTwice(edit: () => Promise<void>, read: EventSeries, idPrefix: string) {
    vi.mocked(Events.get).mockResolvedValue(readOf(read));
    const onSaved = vi.fn();
    const onConflict = vi.fn();
    render(<SeriesEditor series={full} idPrefix={idPrefix} onSaved={onSaved} onConflict={onConflict} />);
    await edit();
    await userEvent.click(saveButton());
    expect(await screen.findByText(offline)).toBeInTheDocument();
    expect(onSaved).not.toHaveBeenCalled();
    await userEvent.click(saveButton());

    return { onSaved, onConflict };
  }

  // Each row is a draft and the read the server would answer once that save is stored, normalised as it stores it.
  it.each([
    ["a title, trimmed", async () => {
      const title = screen.getByRole("textbox", { name: "Title" });
      await userEvent.clear(title);
      await userEvent.type(title, "  Supper night  ");
    }, { title: "Supper night" }],
    ["a cleared seat limit", () => userEvent.clear(screen.getByLabelText(/^Seat limit/)), { capacity: null }],
    ["a video link removed", () => userEvent.click(screen.getByRole("radio", { name: "No video link" })), { videoProvider: "none" as const, videoUrl: "" }],
    ["childcare switched off", () => userEvent.click(screen.getByRole("checkbox", { name: /^Childcare available/ })), { childcareAvailable: false, childcareCapacity: null }],
  ])("counts the second press as saved, sending nothing more, when the read shows %s stored", async (key, edit, over) => {
    const { onSaved, onConflict } = await pressTwice(edit, { ...stored, ...over, revision: "r3" }, `stored-${key}`);
    await waitFor(() => expect(onSaved).toHaveBeenCalledOnce());
    expect(onConflict).not.toHaveBeenCalled();
    expect(Events.update).toHaveBeenCalledTimes(2);
    expect(vi.mocked(Events.update).mock.calls.map(([, body]) => body.revision)).toEqual(["r1", "r1"]);
    expect(Events.get).toHaveBeenCalledExactlyOnceWith(7);
    expect(screen.queryByText(/couldn't/)).not.toBeInTheDocument();
  });

  it("hands the page a conflict when the read shows the draft's change and someone else's", async () => {
    const edit = () => userEvent.type(screen.getByRole("textbox", { name: "Title" }), " night");
    const { onSaved, onConflict } = await pressTwice(edit, { ...stored, title: "Supper night", location: "Library", revision: "r3" }, "theirs");
    await waitFor(() => expect(onConflict).toHaveBeenCalledExactlyOnceWith("changed"));
    expect(onSaved).not.toHaveBeenCalled();
    expect(Events.update).toHaveBeenCalledTimes(2);
  });
});
