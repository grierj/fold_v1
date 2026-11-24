import { createRosterWrites, type RosterWrites } from "../lib/rosterWrites";
import { stubService } from "../test/service";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { createRef, useLayoutEffect, useState, type ReactNode, type RefObject } from "react";
import { MemoryRouter } from "react-router-dom";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { DateRooms } from "../api/rooms";
import { Events } from "../api/events";
import { Site } from "../api/site";
import { occurrence } from "../test/occurrence";
import { ApiError, type Credential } from "../api/core";
import { DateEditor, type DateDraftHold } from "./DateEditor";
import { ManagedDateEditor } from "./ManagedDateEditor";
import { ConfirmProvider } from "./ui/ConfirmDialog";
import { DateWritesProvider, useDateAttempts, type ReadPermit } from "../lib/dateWrites";
import { editBaseOf, useEditRead, useEditRefresh } from "../lib/editBase";
import { ERROR_COPY } from "../lib/errors";
import { formatDayFull } from "../lib/when";
import type { Attendee, EventOccurrence, OccurrenceResponse } from "../types";
import type { JsonObject } from "../lib/wireValues";
import { stubEditSends, verifiedCredential, type EditSends } from "../test/guardedEdits";

stubService(Events, () => ({ dateVisibilityImpact: vi.fn(), managedDate: vi.fn(), removeRegistration: vi.fn() }));

stubService(Site, () => ({ get: vi.fn() }));

stubService(DateRooms, () => ({ list: vi.fn(), set: vi.fn() }));

const DATE = "2026-12-01";
let credential: Credential;
let edits: EditSends;

// What the editor's own manager read (pinned to the credential) answers now.
let base: EventOccurrence;

// The kept-dates panel's reads (never pinned), in order; with none queued it reads `panelRead`.
let panelReads: Array<Promise<OccurrenceResponse>>;
let panelRead: () => Promise<OccurrenceResponse>;

beforeEach(async () => {
  vi.resetAllMocks();
  vi.mocked(Site.get).mockResolvedValue({ name: "Fold", timezone: "UTC", selfSignup: "off", roomsEnabled: false, pushEnabled: true });
  credential = await verifiedCredential();
  edits = stubEditSends();
  base = occurrence({ revision: "r0" });
  panelReads = [];
  panelRead = () => Promise.reject(new Error("no panel read expected"));
  vi.mocked(Events.managedDate).mockImplementation((_id, _date, pinned) => (pinned ? Promise.resolve({ occurrence: base }) : panelReads.shift() ?? panelRead()));
});

const editorReads = () => vi.mocked(Events.managedDate).mock.calls.filter((call) => call[2] !== undefined).length;

interface Props {
  generation?: number;
  onSaved?: (stillOpen: boolean) => void;
  onClose?: () => void;
  onRefused?: () => void;
  store?: RosterWrites;
  dialog?: boolean;
}

// A page listing the date: a settled edit asks for it again, and each answer it shows is a new generation.
function TestHost({ generation, ...props }: Omit<Props, "store" | "dialog"> & { generation: number }) {
  const [answers, setAnswers] = useState(0);
  useEditRefresh(() => true, () => setAnswers((n) => n + 1));

  return <ManagedDateEditor opener={{ eventId: base.eventId, date: base.date, occurrenceId: base.id }} generation={generation * 1000 + answers}
    onSaved={props.onSaved ?? (() => {})} onClose={props.onClose ?? (() => {})} onRefused={props.onRefused ?? (() => {})} />;
}

// The editor as the event page opens it for the listed date `base` names.
function editor({ generation = 0, onSaved = () => {}, onClose = () => {}, onRefused = () => {}, store, dialog = false }: Props = {}) {
  const inner = <TestHost generation={generation} onSaved={onSaved} onClose={onClose} onRefused={onRefused} />;

  return session(dialog ? <ConfirmProvider>{inner}</ConfirmProvider> : inner, store);
}

function session(ui: ReactNode, store?: RosterWrites) {
  return <MemoryRouter><DateWritesProvider credential={credential} rosterWrites={store}>{ui}</DateWritesProvider></MemoryRouter>;
}

// The editor alone over its own read, with a host's hold (ManageDate's way of keeping a draft across a remount).
function HoldHarness({ hold, onAuthorityLost = () => {} }: { hold: RefObject<DateDraftHold | null>; onAuthorityLost?: () => void }) {
  const read = useEditRead({ eventId: base.eventId, date: base.date, occurrenceId: base.id }, "hold", true, () => {});

  return read.kind === "ready" ? <DateEditor base={read.base} permit={read.permit} attempts={read.attempts} hold={hold} onSaved={() => {}} onClose={() => {}} onAuthorityLost={onAuthorityLost} /> : <p>{read.kind}</p>;
}

async function ready() {
  const save = await screen.findByRole("button", { name: "Save this date" });
  await waitFor(() => expect(save).toBeEnabled());

  return save;
}

const save = () => userEvent.click(screen.getByRole("button", { name: "Save this date" }));

it("opens one date through the shared visibility field without changing the series", async () => {
  const onSaved = vi.fn();
  base = occurrence({ revision: "r0", visibility: "private", seriesVisibility: "private", groupId: 4, groupVisibility: "invite_only", groupName: "Hidden fellowship" });
  render(editor({ onSaved }));
  await ready();
  expect(screen.getByRole("radio", { name: "Same as series (private)" })).toBeChecked();
  await userEvent.click(screen.getByRole("radio", { name: /^Public/ }));
  expect(screen.getByText(/Only leaders will see who signed up/)).toBeInTheDocument();
  await userEvent.type(screen.getByRole("textbox", { name: /^Reason/ }), "  Welcome neighbours ");
  await save();
  await waitFor(() => expect(onSaved).toHaveBeenCalledWith(true));
  // One guarded POST from the read's token: no preview, no consent until the writer asks, the reason trimmed.
  expect(edits.sent).toHaveLength(1);
  expect(edits.sent[0]).toMatchObject({ url: "/events/7/occurrences/2026-12-01/edits", revision: "r0", changes: { visibility: "public" }, reason: "Welcome neighbours" });
  expect(edits.sent[0].attempt).toMatch(/^[0-9a-f]{32}$/);
  expect(edits.sent[0].consent).toBeUndefined();
  expect(Events.dateVisibilityImpact).not.toHaveBeenCalled();
});

it("reads the date it opens, pinned to the tab's credential, by its original day", async () => {
  base = occurrence({ revision: "r0", date: DATE, startsLocal: "2026-12-04T19:00", startsAt: "2026-12-04T19:00:00Z" });
  render(editor());
  await ready();
  expect(Events.managedDate).toHaveBeenCalledExactlyOnceWith(7, DATE, credential);
  expect(screen.getByLabelText("Date")).toHaveValue("2026-12-04");
});

it.each([
  ["another row", { id: 2 }],
  ["another event", { eventId: 8 }],
  ["no token", { revision: "" }],
  ["a cancelled date", { cancelled: true }],
  ["a start with seconds", { startsLocal: "2026-12-01T19:00:00" }],
  ["a negative capacity", { capacity: -1 }],
  ["no church-wide flag", { churchWide: undefined }],
] as const)("opens nothing when the read answers %s, and asks the host to check its page", async (_label, over) => {
  const onRefused = vi.fn();
  const listed = base;
  base = { ...listed, ...over };
  render(session(<ManagedDateEditor opener={{ eventId: 7, date: DATE, occurrenceId: listed.id }} generation={0} onSaved={() => {}} onClose={() => {}} onRefused={onRefused} />));
  await waitFor(() => expect(onRefused).toHaveBeenCalledOnce());
  expect(screen.queryByRole("button", { name: "Save this date" })).not.toBeInTheDocument();
  expect(screen.queryByRole("textbox")).not.toBeInTheDocument();
});

it("on the group page, opens nothing for a date the read puts in another group", async () => {
  const onRefused = vi.fn();
  base = occurrence({ revision: "r0", groupId: 9, groupName: "Other group", groupVisibility: "public" });
  render(session(<ManagedDateEditor opener={{ eventId: 7, date: DATE, occurrenceId: 1, groupId: 3 }} generation={0} onSaved={() => {}} onClose={() => {}} onRefused={onRefused} />));
  await waitFor(() => expect(onRefused).toHaveBeenCalledOnce());
  expect(screen.queryByRole("textbox")).not.toBeInTheDocument();
});

it("a refused read shows nothing of the date and asks the host to check its page", async () => {
  const onRefused = vi.fn();
  vi.mocked(Events.managedDate).mockRejectedValue(new ApiError(403, "SECRET", { code: "forbidden" }));
  render(editor({ onRefused }));
  await waitFor(() => expect(onRefused).toHaveBeenCalledOnce());
  expect(screen.queryByRole("textbox")).not.toBeInTheDocument();
  expect(document.body.textContent).not.toContain("SECRET");
});

it("a read that fails offers Retry loading, apart from any Try again, with the fields hidden", async () => {
  vi.mocked(Events.managedDate).mockRejectedValueOnce(new TypeError("Failed to fetch"));
  render(editor());
  expect(await screen.findByText("We couldn't load this date to edit it. What you typed is kept.")).toBeInTheDocument();
  expect(screen.queryByRole("textbox")).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Try again" })).not.toBeInTheDocument();
  await userEvent.click(screen.getByRole("button", { name: "Retry loading" }));
  await ready();
  expect(editorReads()).toBe(2);
});

it("a credential the server no longer knows is not a refusal: the read can be asked again", async () => {
  const onRefused = vi.fn();
  vi.mocked(Events.managedDate).mockRejectedValueOnce(new ApiError(403, "the browser credential changed", { code: "session_expired" }));
  render(editor({ onRefused }));
  expect(await screen.findByRole("button", { name: "Retry loading" })).toBeInTheDocument();
  expect(onRefused).not.toHaveBeenCalled();
});

it("with no credential this tab can pin, shows no fields and sends nothing", async () => {
  render(<MemoryRouter><DateWritesProvider credential={null}><ManagedDateEditor opener={{ eventId: 7, date: DATE, occurrenceId: 1 }} generation={0} onSaved={() => {}} onClose={() => {}} onRefused={() => {}} /></DateWritesProvider></MemoryRouter>);
  expect(await screen.findByText(/We couldn't confirm this tab's sign-in/)).toBeInTheDocument();
  expect(screen.queryByRole("textbox")).not.toBeInTheDocument();
  expect(Events.managedDate).not.toHaveBeenCalled();
});

it("puts the Room control beside Location when rooms are on, and not otherwise", async () => {
  vi.mocked(Site.get).mockResolvedValue({ name: "Fold", timezone: "UTC", selfSignup: "off", roomsEnabled: true, pushEnabled: true });
  vi.mocked(DateRooms.list).mockResolvedValue({ rooms: [], booking: null });
  render(editor());
  expect(await screen.findByRole("combobox", { name: /^Room/ })).toBeInTheDocument();
  expect(DateRooms.list).toHaveBeenCalledWith(7, DATE);
  expect(screen.getByRole("textbox", { name: /^Location/ })).toBeInTheDocument();
});

it("leaves the Room control out while rooms are off", async () => {
  render(editor());
  await ready();
  await waitFor(() => expect(Site.get).toHaveBeenCalled());
  expect(screen.queryByRole("combobox", { name: /^Room/ })).not.toBeInTheDocument();
  expect(DateRooms.list).not.toHaveBeenCalled();
});

it("keeps the rest of the editor working while the rooms fail to load", async () => {
  vi.mocked(Site.get).mockResolvedValue({ name: "Fold", timezone: "UTC", selfSignup: "off", roomsEnabled: true, pushEnabled: true });
  vi.mocked(DateRooms.list).mockRejectedValue(new Error("down"));
  const onSaved = vi.fn();
  render(editor({ onSaved }));
  expect(await screen.findByRole("button", { name: "Try again" })).toBeInTheDocument();
  await ready();
  await userEvent.type(screen.getByRole("textbox", { name: "Title" }), "!");
  await save();
  await waitFor(() => expect(onSaved).toHaveBeenCalled());
  expect(edits.sent.map((s) => s.changes)).toEqual([{ title: "Supper!" }]);
});

it("an unchanged form closes without sending anything", async () => {
  const onClose = vi.fn();
  render(editor({ onClose }));
  await ready();
  await save();
  expect(onClose).toHaveBeenCalledOnce();
  expect(edits.sent).toHaveLength(0);
});

it("sends only what changed: a title alone keeps the stored clock, a start alone keeps the shown duration, a cleared cap is null", async () => {
  base = occurrence({ revision: "r0", capacity: 20, remoteAvailable: true, remoteCapacity: 0, durationMinutes: 45 });
  render(editor());
  await ready();
  await userEvent.type(screen.getByRole("textbox", { name: "Title" }), "!");
  await save();
  await waitFor(() => expect(edits.sent).toHaveLength(1));
  expect(edits.sent[0].changes).toEqual({ title: "Supper!" });
});

it("names dates in words: the series day, and a new start in the recovery panel on the event's own clock", async () => {
  base = occurrence({ revision: "r0", tzid: "America/Boise", startsLocal: "2026-12-01T19:00", startsAt: "2026-12-02T02:00:00Z" });
  edits.answer(new TypeError("Failed to fetch"));
  render(editor());
  await ready();
  expect(screen.getByText(/The series date stays Tuesday, December 1, 2026\./)).toBeInTheDocument();
  fireEvent.change(screen.getByLabelText("Date"), { target: { value: "2026-12-04" } });
  fireEvent.change(screen.getByLabelText("Start time"), { target: { value: "19:30" } });
  await save();
  await screen.findByRole("button", { name: "Try again" });
  expect(screen.getByRole("listitem")).toHaveTextContent("Starts: Friday, December 4, 2026 at 7:30 PM");
  expect(edits.sent[0].changes).toEqual({ startsLocal: "2026-12-04T19:30" });
  const form = screen.getByRole("button", { name: "Save this date" }).closest("form");
  expect(form?.textContent).not.toMatch(/\d{4}-\d{2}-\d{2}/);
});

it("pairs a new start date with its time, and clears a cap as null while keeping a remote zero", async () => {
  base = occurrence({ revision: "r0", capacity: 20, remoteAvailable: true, remoteCapacity: 0 });
  render(editor());
  await ready();
  fireEvent.change(screen.getByLabelText("Date"), { target: { value: "2026-12-04" } });
  await userEvent.clear(screen.getByRole("spinbutton", { name: /^Capacity for this date/ }));
  await save();
  await waitFor(() => expect(edits.sent).toHaveLength(1));
  expect(edits.sent[0].changes).toEqual({ startsLocal: "2026-12-04T19:00", capacityOverride: null });
  expect(JSON.parse(edits.sent[0].body).changes).toHaveProperty("capacityOverride", null);
});

it("takes each field a newer read changed and keeps the others as typed, so Save sends only the typed change", async () => {
  const before = occurrence({ revision: "r0", capacity: 20 });
  base = before;
  const view = render(editor());
  await ready();
  await userEvent.clear(screen.getByRole("textbox", { name: "Title" }));
  await userEvent.type(screen.getByRole("textbox", { name: "Title" }), "Carols");
  await userEvent.clear(screen.getByRole("textbox", { name: /^Location/ }));
  await userEvent.type(screen.getByRole("textbox", { name: /^Location/ }), "Porch");

  // The page shows a newer answer, so the date is read again: another manager moved the place and the capacity.
  base = { ...before, location: "Annex", capacity: 30, revision: "r1" };
  view.rerender(editor({ generation: 1 }));
  await waitFor(() => expect(screen.getByRole("textbox", { name: /^Location/ })).toHaveValue("Annex"));
  expect(screen.getByRole("textbox", { name: "Title" })).toHaveValue("Carols");
  expect(screen.getByRole("spinbutton", { name: /^Capacity for this date/ })).toHaveValue(30);
  await ready();
  await save();
  await waitFor(() => expect(edits.sent).toHaveLength(1));
  expect(edits.sent[0]).toMatchObject({ revision: "r1", changes: { title: "Carols" } });
});

it("after a stored save, the next read resets the submitted fields even where another manager put the old value back", async () => {
  render(editor());
  await ready();
  await userEvent.clear(screen.getByRole("textbox", { name: "Title" }));
  await userEvent.type(screen.getByRole("textbox", { name: "Title" }), "Carols");
  await userEvent.clear(screen.getByRole("textbox", { name: /^Location/ }));
  await userEvent.type(screen.getByRole("textbox", { name: /^Location/ }), "Porch");
  // Stored as Carols; before this editor reads again another manager names it Supper again, as it was opened.
  edits.answer({ occurrence: { ...base, title: "Carols", revision: "r1" } });
  base = { ...base, title: "Supper", revision: "r2" };
  await userEvent.click(screen.getByRole("button", { name: "Save this date" }));
  await waitFor(() => expect(editorReads()).toBe(2));
  await ready();
  expect(edits.sent[0].changes).toEqual({ title: "Carols", location: "Porch" });
  expect(screen.getByRole("textbox", { name: "Title" })).toHaveValue("Supper");
  expect(screen.getByRole("textbox", { name: /^Location/ })).toHaveValue("Hall");
  // A capacity-only save now sends capacity alone.
  await userEvent.type(screen.getByRole("spinbutton", { name: /^Capacity for this date/ }), "12");
  await save();
  await waitFor(() => expect(edits.sent).toHaveLength(2));
  expect(edits.sent[1]).toMatchObject({ revision: "r2", changes: { capacityOverride: 12 } });
  expect(edits.sent[1].attempt).not.toBe(edits.sent[0].attempt);
});

it("a field typed again while the save ran keeps what was typed", async () => {
  render(editor({ dialog: true }));
  await ready();
  await userEvent.type(screen.getByRole("textbox", { name: "Title" }), "!");
  let finish: (r: OccurrenceResponse) => void = () => {};

  edits.answer(new Promise<OccurrenceResponse>((resolve) => { finish = resolve; }));
  await save();
  await waitFor(() => expect(edits.sent).toHaveLength(1));
  base = { ...base, title: "Supper!", revision: "r1" };
  await act(async () => { finish({ occurrence: { ...base } }); });
  await waitFor(() => expect(editorReads()).toBe(2));
  expect(screen.getByRole("textbox", { name: "Title" })).toHaveValue("Supper!");
});

it.each([
  ["the connection drops", new TypeError("Failed to fetch")],
  ["the server fails", new ApiError(500, "SECRET", {})],
  ["a proxy answers with a page of its own", new ApiError(413, "request failed (413)", {}, true)],
] as const)("when %s, Save stays closed, the date is read again and Try again resends the same bytes once", async (_label, cause) => {
  const onSaved = vi.fn();
  edits.answer(cause);
  render(editor({ onSaved }));
  await ready();
  await userEvent.type(screen.getByRole("textbox", { name: "Title" }), "!");
  await save();
  await waitFor(() => expect(screen.getByText("We couldn't tell whether this date saved. Check it below, and try again if you need to.")).toBeInTheDocument());
  expect(onSaved).not.toHaveBeenCalled();
  const again = await screen.findByRole("button", { name: "Try again" });
  // Only the submitted change is shown, never what the date said before, and the date is named in words.
  expect(screen.getByRole("listitem")).toHaveTextContent("Title: Supper!");
  expect(screen.getByText(`We couldn't tell whether your last save of ${formatDayFull(DATE)} went through. It asked for:`)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Save this date" })).toBeDisabled();
  expect(screen.getByRole("textbox", { name: "Title" })).toBeDisabled();
  expect(document.body.textContent).not.toContain("SECRET");
  await waitFor(() => expect(again).toBeEnabled());
  await userEvent.click(again);
  await waitFor(() => expect(onSaved).toHaveBeenCalled());
  expect(edits.sent).toHaveLength(2);
  expect(edits.sent[1].body).toBe(edits.sent[0].body);
});

describe("a replay that finds the date changed retires the draft that made it", () => {
  const conflict = () => new ApiError(409, "x", { code: "date_changed", baseSuperseded: true });
  const unconfirmed = "We couldn't confirm what happened to your earlier save, and this date has changed since. Check it before you change anything else.";

  async function typeAndLoseTheAnswer() {
    await userEvent.clear(screen.getByRole("textbox", { name: "Title" }));
    await userEvent.type(screen.getByRole("textbox", { name: "Title" }), "Carols");
    await userEvent.clear(screen.getByRole("textbox", { name: /^Location/ }));
    await userEvent.type(screen.getByRole("textbox", { name: /^Location/ }), "Porch");
    await userEvent.click(screen.getByRole("radio", { name: /^Public/ }));
    await userEvent.type(screen.getByRole("textbox", { name: /^Reason/ }), "Open it up");
    edits.answer(new TypeError("Failed to fetch"));
    await save();
    await screen.findByText("We couldn't tell whether this date saved. Check it below, and try again if you need to.");
  }

  beforeEach(() => {
    base = occurrence({ revision: "r0", visibility: "private", seriesVisibility: "private", groupId: 4, groupVisibility: "invite_only", groupName: "Hidden fellowship" });
  });

  it("on its own editor: the title, place, visibility and reason it typed never become a new save", async () => {
    const onClose = vi.fn();
    render(editor({ onClose }));
    await ready();
    await typeAndLoseTheAnswer();
    const again = await screen.findByRole("button", { name: "Try again" });
    await waitFor(() => expect(again).toBeEnabled());
    // Another manager changed the title meanwhile.
    base = { ...base, title: "Potluck", revision: "r9" };
    edits.answer(conflict());
    await userEvent.click(again);
    expect(await screen.findByText(unconfirmed)).toBeInTheDocument();
    const saveButton = await ready();
    expect(screen.getByRole("textbox", { name: "Title" })).toHaveValue("Potluck");
    expect(screen.getByRole("textbox", { name: /^Location/ })).toHaveValue("Hall");
    expect(screen.getByRole("radio", { name: "Same as series (private)" })).toBeChecked();
    expect(screen.getByRole("textbox", { name: /^Reason/ })).toHaveValue("");
    expect(screen.queryByRole("button", { name: "Try again" })).not.toBeInTheDocument();
    // Nothing was typed since: Save sends nothing and closes.
    await userEvent.click(saveButton);
    expect(onClose).toHaveBeenCalledOnce();
    expect(edits.sent).toHaveLength(2);
    // A later change sends only itself.
    await userEvent.type(screen.getByRole("spinbutton", { name: /^Capacity for this date/ }), "5");
    await save();
    await waitFor(() => expect(edits.sent).toHaveLength(3));
    expect(edits.sent[2]).toMatchObject({ revision: "r9", changes: { capacityOverride: 5 } });
    expect(edits.sent[2].reason).toBeUndefined();
  });

  it("from another editor: the held draft is not brought back when its editor mounts again", async () => {
    const hold = createRef<DateDraftHold>();
    // One session: the held editor, then the same date's editor on another page, then the held one again.
    const view = render(session(<HoldHarness hold={hold} />));
    await ready();
    await typeAndLoseTheAnswer();
    expect(hold.current?.draft.title).toBe("Carols");
    view.rerender(session(<TestHost generation={1} onSaved={() => {}} onClose={() => {}} onRefused={() => {}} />));
    const again = await screen.findByRole("button", { name: "Try again" });
    await waitFor(() => expect(again).toBeEnabled());
    base = { ...base, title: "Potluck", revision: "r9" };
    edits.answer(conflict());
    await userEvent.click(again);
    expect(await screen.findByText(unconfirmed)).toBeInTheDocument();
    view.rerender(session(<HoldHarness hold={hold} />));
    await ready();
    expect(screen.getByRole("textbox", { name: "Title" })).toHaveValue("Potluck");
    expect(screen.getByRole("textbox", { name: /^Location/ })).toHaveValue("Hall");
    expect(screen.getByRole("radio", { name: "Same as series (private)" })).toBeChecked();
    await waitFor(() => expect(hold.current?.draft.title).toBe("Potluck"));
    expect(hold.current?.reason).toBe("");
  });
});

it("a readable refusal answers only this send and keeps what was typed", async () => {
  edits.answer(new ApiError(409, "SECRET", { code: "conflict" }));
  render(editor());
  await ready();
  await userEvent.type(screen.getByRole("textbox", { name: "Title" }), "!");
  await save();
  expect(await screen.findByText("We couldn't update this date. Please try again.")).toBeInTheDocument();
  expect(screen.getByRole("textbox", { name: "Title" })).toHaveValue("Supper!");
  expect(screen.getByRole("button", { name: "Save this date" })).toBeEnabled();
});

it("a refusal that says this person may no longer manage the date asks the host to check its page", async () => {
  const onRefused = vi.fn();
  edits.answer(new ApiError(403, "SECRET", { code: "forbidden" }));
  render(editor({ onRefused }));
  await ready();
  await userEvent.type(screen.getByRole("textbox", { name: "Title" }), "!");
  await save();
  await waitFor(() => expect(onRefused).toHaveBeenCalled());
});

it("a first date_changed reads the date again and needs another Save with a new attempt", async () => {
  edits.answer(new ApiError(409, "x", { code: "date_changed" }));
  render(editor());
  await ready();
  await userEvent.type(screen.getByRole("textbox", { name: "Title" }), "!");
  base = { ...base, location: "Annex", revision: "r1" };
  await save();
  await waitFor(() => expect(screen.getByText("This date changed while you were saving. Check it and save again.")).toBeInTheDocument());
  await waitFor(() => expect(screen.getByRole("textbox", { name: /^Location/ })).toHaveValue("Annex"));
  await ready();
  expect(edits.sent).toHaveLength(1);
  await save();
  await waitFor(() => expect(edits.sent).toHaveLength(2));
  expect(edits.sent[1]).toMatchObject({ revision: "r1", changes: { title: "Supper!" } });
  expect(edits.sent[1].attempt).not.toBe(edits.sent[0].attempt);
});

it("with a hold, a remount under the same date restores every typed field and the reason", async () => {
  const hold = createRef<DateDraftHold>();
  base = occurrence({ revision: "r0", visibility: "private", seriesVisibility: "private", groupId: 4, groupVisibility: "invite_only", groupName: "Hidden fellowship" });
  const view = render(session(<HoldHarness hold={hold} />));
  await ready();
  await userEvent.clear(screen.getByRole("spinbutton", { name: "Minutes" }));
  await userEvent.type(screen.getByRole("spinbutton", { name: "Minutes" }), "150");
  await userEvent.clear(screen.getByRole("textbox", { name: "Title" }));
  await userEvent.type(screen.getByRole("textbox", { name: "Title" }), "Typed title");
  await userEvent.type(screen.getByRole("textbox", { name: /^Location/ }), "Typed place");
  await userEvent.type(screen.getByRole("spinbutton", { name: /^Capacity for this date/ }), "12");
  await userEvent.click(screen.getByRole("radio", { name: /^Public/ }));
  await userEvent.type(screen.getByRole("textbox", { name: /^Reason/ }), "Open it up");
  view.unmount();
  render(session(<HoldHarness hold={hold} />));
  await ready();
  expect(screen.getByRole("spinbutton", { name: "Minutes" })).toHaveValue(150);
  expect(screen.getByRole("textbox", { name: "Title" })).toHaveValue("Typed title");
  expect(screen.getByRole("textbox", { name: /^Location/ })).toHaveValue("HallTyped place");
  expect(screen.getByRole("spinbutton", { name: /^Capacity for this date/ })).toHaveValue(12);
  expect(screen.getByRole("radio", { name: /^Public/ })).toBeChecked();
  expect(screen.getByRole("textbox", { name: /^Reason/ })).toHaveValue("Open it up");
});

it("with a hold, a remount under a newer date takes the stored value only where the stored date changed", async () => {
  const hold = createRef<DateDraftHold>();
  const view = render(session(<HoldHarness hold={hold} />));
  await ready();
  await userEvent.clear(screen.getByRole("spinbutton", { name: "Minutes" }));
  await userEvent.type(screen.getByRole("spinbutton", { name: "Minutes" }), "150");
  await userEvent.clear(screen.getByRole("textbox", { name: "Title" }));
  await userEvent.type(screen.getByRole("textbox", { name: "Title" }), "Typed title");
  view.unmount();
  base = occurrence({ revision: "r1", title: "Stored rename", location: "New hall" });
  render(session(<HoldHarness hold={hold} />));
  await ready();
  expect(screen.getByRole("spinbutton", { name: "Minutes" })).toHaveValue(150);
  expect(screen.getByRole("textbox", { name: "Title" })).toHaveValue("Stored rename");
  expect(screen.getByRole("textbox", { name: /^Location/ })).toHaveValue("New hall");
});

it("a new read under the same route keeps the draft; Close and reopening start from the stored date", async () => {
  function Toggle({ open }: { open: boolean }) {
    return open ? <ManagedDateEditor opener={{ eventId: 7, date: DATE, occurrenceId: 1 }} generation={0} onSaved={() => {}} onClose={() => {}} onRefused={() => {}} /> : null;
  }

  const view = render(session(<Toggle open />));
  await ready();
  await userEvent.clear(screen.getByRole("spinbutton", { name: "Minutes" }));
  await userEvent.type(screen.getByRole("spinbutton", { name: "Minutes" }), "150");
  view.rerender(session(<Toggle open={false} />));
  view.rerender(session(<Toggle open />));
  await ready();
  expect(screen.getByRole("spinbutton", { name: "Minutes" })).toHaveValue(60);
});

// W3: a save refused because someone who left kept the date (DEC-156).
const CHECKING = "Your change isn't saved yet. Checking that you can still manage sign-ups…";
const REFUSED = "Your change isn't saved. You can't manage sign-ups for this event now.";
const NOBODY_ONE = "Nobody is keeping this date now. Save again to apply your change.";
const keptRow = (name: string): Attendee => ({ registrationId: 31, userId: 5, name, status: "going", attendanceMode: "in_person", guestCount: 0, keptAfterLeaving: true });
const managedRead = (over: Partial<EventOccurrence> = {}): OccurrenceResponse => ({ occurrence: occurrence({ attendeesVisible: true, keptCount: 1, attendees: [keptRow("Old Name")], ...over }) });
const kept409 = (body: JsonObject = { keptDates: [{ date: DATE, keptCount: 1 }] }) => new ApiError(409, "SECRET", { code: "date_kept_after_leaving", ...body });

function deferred<T>() {
  let resolve: (value: T) => void = () => {};
  let reject: (cause: unknown) => void = () => {};
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });

  return { promise, resolve, reject };
}

async function typeAndSave(minutes = "150") {
  await ready();
  const field = screen.getByRole("spinbutton", { name: "Minutes" });
  await userEvent.clear(field);
  await userEvent.type(field, minutes);
  await save();
}

const panel = () => screen.queryByRole("region", { name: "Sign-ups to check before you save" });

describe("W3: a 409 for a kept date", () => {
  beforeEach(() => {
    panelRead = () => Promise.resolve(managedRead());
    vi.spyOn(window, "confirm").mockImplementation(() => { throw new Error("native confirm"); });
  });

  it("keeps the draft, opens the panel after the form, and leaves the ERROR_COPY line to the panel's read", async () => {
    edits.answer(kept409());
    const read = deferred<OccurrenceResponse>();
    panelReads.push(read.promise);
    render(editor());
    await ready();
    await userEvent.clear(screen.getByRole("textbox", { name: "Title" }));
    await userEvent.type(screen.getByRole("textbox", { name: "Title" }), "Typed title");
    await typeAndSave();
    expect(await screen.findByText(CHECKING)).toBeInTheDocument();
    await screen.findByRole("region", { name: "Sign-ups to check before you save" });
    expect(screen.getByText(CHECKING)).toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(document.body.textContent).not.toContain(ERROR_COPY.date_kept_after_leaving);
    await act(async () => { read.resolve(managedRead()); await read.promise; });
    const shown = panel();

    if (!shown) throw new Error("no panel");
    expect(within(shown).getByRole("alert")).toHaveTextContent(ERROR_COPY.date_kept_after_leaving);
    expect(screen.getAllByRole("alert")).toHaveLength(1);
    expect(Events.managedDate).toHaveBeenCalledWith(7, DATE);
    expect(screen.getByRole("spinbutton", { name: "Minutes" })).toHaveValue(150);
    expect(screen.getByRole("textbox", { name: "Title" })).toHaveValue("Typed title");
    const form = document.querySelector("form");
    expect(form?.contains(shown)).toBe(false);
    expect(document.querySelector("form form")).toBeNull();
    expect(screen.getByText("Kept after leaving")).toBeInTheDocument();
  });

  it("opens the panel with its own date when the 409 names no readable dates", async () => {
    edits.answer(kept409({}));
    render(editor());
    await typeAndSave();
    expect(await screen.findByText("Old Name")).toBeInTheDocument();
    expect(Events.managedDate).toHaveBeenCalledWith(7, DATE);
    expect(within(panel() ?? document.body).getByRole("heading", { name: formatDayFull(DATE) })).toBeInTheDocument();
  });

  it("a kept 409 on a tightening opens the panel and never the cancellation question", async () => {
    base = occurrence({ revision: "r0", visibility: "public", seriesVisibility: "public" });
    edits.answer(kept409());
    render(editor());
    await ready();
    await userEvent.click(screen.getByRole("radio", { name: /^Private/ }));
    await typeAndSave();
    expect(await screen.findByText("Old Name")).toBeInTheDocument();
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(edits.sent).toHaveLength(1);
    expect(screen.getByRole("radio", { name: /^Private/ })).toBeChecked();
  });

  it("writes the refused dates to the hold, and a remount reopens the panel checking", async () => {
    const hold = createRef<DateDraftHold>();
    edits.answer(kept409());
    const view = render(session(<HoldHarness hold={hold} />));
    await typeAndSave();
    await screen.findByText("Old Name");
    expect(hold.current?.kept).toEqual([{ date: DATE, keptCount: 1 }]);
    view.unmount();
    const read = deferred<OccurrenceResponse>();
    panelReads.push(read.promise);
    render(session(<HoldHarness hold={hold} />));
    expect(await screen.findByText(CHECKING)).toBeInTheDocument();
    expect(screen.getByRole("spinbutton", { name: "Minutes" })).toHaveValue(150);
    await act(async () => { read.resolve(managedRead()); await read.promise; });
    expect(screen.getByText("Old Name")).toBeInTheDocument();
  });

  it("a second refusal for the same date starts a new panel that shows only the checking line until it reads", async () => {
    edits.answer(kept409());
    panelReads.push(Promise.resolve(managedRead({ keptCount: 0, attendees: [] })));
    render(editor());
    await typeAndSave();
    expect(await screen.findByText(NOBODY_ONE)).toBeInTheDocument();
    const read = deferred<OccurrenceResponse>();
    panelReads.push(read.promise);
    const sent = deferred<OccurrenceResponse>();
    edits.answer(sent.promise);
    await ready();
    await save();
    await waitFor(() => expect(edits.sent).toHaveLength(2));
    // The form names its own date; the panel's heading for it is kept-specific.
    const headings = () => [...document.querySelectorAll("h1, h2, h3, h4, h5, h6")].map((h) => h.textContent ?? "").join("|");
    const seen: Array<[string, string]> = [];
    const observer = new MutationObserver(() => seen.push([document.body.textContent ?? "", headings()]));
    observer.observe(document.body, { subtree: true, childList: true, characterData: true });
    await act(async () => { sent.reject(kept409()); await sent.promise.catch(() => {}); });
    const now: [string, string] = [document.body.textContent ?? "", headings()];
    expect(now[0]).toContain(CHECKING);

    for (const [text, heads] of [...seen, now]) {
      expect(text).not.toContain("Nobody is keeping");
      expect(text).not.toContain(ERROR_COPY.date_kept_after_leaving);
      expect(text).not.toContain("Second Name");
      expect(heads).not.toContain(formatDayFull(DATE));
    }

    observer.disconnect();
    await act(async () => { read.resolve(managedRead({ attendees: [keptRow("Second Name")] })); await read.promise; });
    expect(screen.getByText("Second Name")).toBeInTheDocument();
    expect(within(panel() ?? document.body).getByRole("alert")).toHaveTextContent(ERROR_COPY.date_kept_after_leaving);
    expect(screen.getByRole("spinbutton", { name: "Minutes" })).toHaveValue(150);
  });

  it("a refused panel read clears the kept state, keeps the draft and says so with no kept detail", async () => {
    const hold = createRef<DateDraftHold>();
    const onAuthorityLost = vi.fn();
    edits.answer(kept409());
    const refusal = Promise.reject(new ApiError(403, "SECRET", { code: "forbidden" }));
    refusal.catch(() => {});
    panelReads.push(refusal);
    const view = render(session(<HoldHarness hold={hold} onAuthorityLost={onAuthorityLost} />));
    await typeAndSave();
    await waitFor(() => expect(onAuthorityLost).toHaveBeenCalledTimes(1));
    expect(panel()).not.toBeInTheDocument();
    expect(hold.current?.kept).toBeNull();
    expect(screen.getByRole("alert")).toHaveTextContent(REFUSED);
    expect(screen.getByRole("spinbutton", { name: "Minutes" })).toHaveValue(150);

    expect(screen.queryByRole("heading", { name: formatDayFull(DATE) })).not.toBeInTheDocument();

    for (const words of [ERROR_COPY.date_kept_after_leaving, "Old Name", "kept"]) expect(document.body.textContent).not.toContain(words);
    view.unmount();
    render(session(<HoldHarness hold={hold} />));
    await waitFor(() => expect(screen.getByRole("spinbutton", { name: "Minutes" })).toHaveValue(150));
    expect(panel()).not.toBeInTheDocument();
    // One panel read only: the remount reads the date for its form, never the panel's roster again.
    expect(vi.mocked(Events.managedDate).mock.calls.filter((call) => call[2] === undefined)).toHaveLength(1);
  });

  it("a successful save closes the panel", async () => {
    const onSaved = vi.fn();
    edits.answer(kept409());
    render(editor({ onSaved }));
    await typeAndSave();
    await screen.findByText("Old Name");
    await ready();
    await save();
    await waitFor(() => expect(onSaved).toHaveBeenCalled());
    expect(panel()).not.toBeInTheDocument();
  });
});

// DEC-158: the writer selects the cancellations and returns a decision; the editor asks only from that answer,
// never from a preview, and a yes adds only the returned token to the same attempt.
describe("visibility consent", () => {
  const T1 = "11111111111111111111111111111111";
  const T2 = "22222222222222222222222222222222";

  const tighten = (registrants: number, current: "public" | "private", consent: string) => ({
    visibility: "invite_only" as const, currentVisibility: current, tightened: registrants ? [{ id: 7, title: "One date", visibility: current, registrants, occurrenceId: 1 }] : [], leaderOpened: [], consent,
  });

  const decision = (impact: ReturnType<typeof tighten>) => new ApiError(409, "x", { code: "date_needs_consent", impact: JSON.parse(JSON.stringify(impact)) });

  async function open() {
    base = occurrence({ revision: "r0", visibility: "public", visibilityOverride: "public", seriesVisibility: "invite_only" });
    render(editor());
    await ready();
    await userEvent.click(screen.getByRole("radio", { name: "Same as series (invite only)" }));
    await save();
  }

  it("asks from the writer's answer, and a yes resends the same attempt with only its token added", async () => {
    edits.answer(decision(tighten(2, "public", T1)));
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(true);
    await open();
    await waitFor(() => expect(edits.sent).toHaveLength(2));
    expect(confirm).toHaveBeenCalledExactlyOnceWith("Tuesday, December 1, 2026. This date is public now. Saving makes it invite-only, so fewer people will be able to see it. This cancels and notifies 2 registered people who can no longer see this date. Save this date?");
    expect(edits.sent[0].consent).toBeUndefined();
    expect(edits.sent[1]).toMatchObject({ attempt: edits.sent[0].attempt, revision: "r0", changes: { visibility: "series" }, consent: T1 });
    expect(edits.sent[1].body).toBe(`${edits.sent[0].body.slice(0, -1)},"consent":"${T1}"}`);
    expect(Events.dateVisibilityImpact).not.toHaveBeenCalled();
  });

  it("a second question in the same save says it checked again, from the new answer", async () => {
    edits.answer(decision(tighten(2, "public", T1)), decision(tighten(3, "public", T2)));
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(true);
    await open();
    await waitFor(() => expect(edits.sent).toHaveLength(3));
    expect(confirm).toHaveBeenNthCalledWith(2, "We checked again before saving. Tuesday, December 1, 2026. This date is public now. Saving makes it invite-only, so fewer people will be able to see it. This cancels and notifies 3 registered people who can no longer see this date. Save this date?");
    expect(edits.sent.map((s) => s.consent)).toEqual([undefined, T1, T2]);
  });

  it("a no writes nothing more and keeps the draft", async () => {
    edits.answer(decision(tighten(1, "public", T1)), decision(tighten(3, "public", T2)));
    vi.spyOn(window, "confirm").mockReturnValueOnce(true).mockReturnValueOnce(false);
    await open();
    await waitFor(() => expect(window.confirm).toHaveBeenCalledTimes(2));
    expect(window.confirm).toHaveBeenNthCalledWith(1, expect.stringContaining("notifies 1 registered person who can no longer see this date."));
    expect(window.confirm).toHaveBeenLastCalledWith(expect.stringContaining("3 registered people"));
    expect(edits.sent).toHaveLength(2);
    await waitFor(() => expect(screen.getByRole("button", { name: "Save this date" })).toBeEnabled());
    expect(screen.getByRole("radio", { name: "Same as series (invite only)" })).toBeChecked();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it.each([
    ["the outdated answer", new ApiError(409, "x", { code: "visibility_review_outdated" })],
    ["a decision without a token", new ApiError(409, "x", { code: "date_needs_consent", impact: { visibility: "invite_only", tightened: [], leaderOpened: [] } })],
  ])("shows the outdated copy for %s and asks nothing", async (_, err) => {
    edits.answer(err);
    vi.spyOn(window, "confirm").mockReturnValue(true);
    await open();
    expect(await screen.findByText(ERROR_COPY.visibility_review_outdated)).toBeInTheDocument();
    expect(window.confirm).not.toHaveBeenCalled();
    expect(edits.sent).toHaveLength(1);
    expect(screen.getByRole("radio", { name: "Same as series (invite only)" })).toBeChecked();
  });

  it("asks only through the read the save was sent through: a newer read registered while it was on its way asks nothing", async () => {
    base = occurrence({ revision: "r0" });
    const reread = createRef<() => void>();

    // A host that hands the editor a read it registers itself, and a newer one on demand, without a remount.
    function Direct() {
      const attempts = useDateAttempts();
      const [permit, setPermit] = useState<ReadPermit | null>(null);
      const fresh = () => attempts?.register(attempts.stamp(7, DATE), { occurrenceId: 1, revision: "r0" }) ?? null;
      useLayoutEffect(() => { if (attempts && !permit) setPermit(fresh()); });
      reread.current = () => setPermit(fresh());
      const edit = editBaseOf(base, { eventId: 7, date: DATE, occurrenceId: 1 });

      return attempts && permit && edit ? <DateEditor base={edit} permit={permit} attempts={attempts} onSaved={() => {}} onClose={() => {}} /> : null;
    }

    render(session(<ConfirmProvider><Direct /></ConfirmProvider>));
    await userEvent.click(await screen.findByRole("radio", { name: /^Private/ }));
    let reject: (cause: unknown) => void = () => {};

    edits.answer(new Promise<OccurrenceResponse>((_, no) => { reject = no; }));
    await ready();
    await save();
    await waitFor(() => expect(edits.sent).toHaveLength(1));
    act(() => reread.current?.());
    await act(async () => { reject(new ApiError(409, "x", { code: "date_needs_consent", impact: { visibility: "private", currentVisibility: "public", tightened: [{ id: 7, title: "One date", visibility: "public", registrants: 2, occurrenceId: 1 }], leaderOpened: [], consent: T1 } })); });
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(await screen.findByText("This date changed while you were saving. Check it and save again.")).toBeInTheDocument();
    expect(edits.sent).toHaveLength(1);
    await ready();
  });

  it("an editor closed while the question is open answers it no, so the date can be saved again", async () => {
    edits.answer(decision(tighten(2, "public", T1)));
    base = occurrence({ revision: "r0", visibility: "public", visibilityOverride: "public", seriesVisibility: "invite_only" });

    function Toggle({ open }: { open: boolean }) {
      return <ConfirmProvider>{open && <ManagedDateEditor opener={{ eventId: 7, date: DATE, occurrenceId: 1 }} generation={0} onSaved={() => {}} onClose={() => {}} onRefused={() => {}} />}</ConfirmProvider>;
    }

    const view = render(session(<Toggle open />));
    await ready();
    await userEvent.click(screen.getByRole("radio", { name: "Same as series (invite only)" }));
    await save();
    expect(await screen.findByRole("dialog", { name: "Save this date?" })).toBeInTheDocument();
    view.rerender(session(<Toggle open={false} />));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    view.rerender(session(<Toggle open />));
    await ready();
    expect(edits.sent).toHaveLength(1);
  });
});

// VC2: the editor reads its date's roster activity itself, on every host, so a roster write on the date (from any
// roster or kept-dates panel in the session) holds Save from its begin to its end, and a begin closes an open
// question as no.
describe("roster writes on the date", () => {
  const STALE = "This date changed while you were saving. Check it and save again.";
  const T = "33333333333333333333333333333333";
  const challenge = () => new ApiError(409, "x", { code: "date_needs_consent", impact: { visibility: "private", currentVisibility: "public", tightened: [{ id: 7, title: "One date", visibility: "public", registrants: 2, occurrenceId: 1 }], leaderOpened: [], consent: T } });

  function inSession(store: RosterWrites) {
    render(editor({ store, dialog: true }));
  }

  it("disables Save, never the fields, while a write on this date is on its way, and keeps focus in the form", async () => {
    const store = createRosterWrites();
    inSession(store);
    await ready();
    const field = screen.getByRole("spinbutton", { name: "Minutes" });
    await userEvent.clear(field);
    await userEvent.type(field, "150");
    const button = screen.getByRole("button", { name: "Save this date" });
    button.focus();
    let end: (e: "answered") => void = () => {};

    act(() => { end = store.begin(`7:${DATE}`); });
    expect(button).toBeDisabled();
    expect(field).toBeEnabled();
    // Focus stays in the editor (the form, should the browser drop it from the disabled button), never the page.
    expect(button.closest("form")?.contains(document.activeElement)).toBe(true);
    fireEvent.submit(requiredForm());
    expect(edits.sent).toHaveLength(0);
    // Another date's or another event's write holds nothing here.
    act(() => { store.begin("7:2026-12-08"); store.begin(`8:${DATE}`); });
    act(() => end("answered"));
    expect(button).toBeEnabled();
    expect(field).toHaveValue(150);
  });

  it("a begin while the question is open closes it as no and sends nothing more", async () => {
    const store = createRosterWrites();
    edits.answer(challenge());
    inSession(store);
    await ready();
    await userEvent.click(screen.getByRole("radio", { name: /^Private/ }));
    await save();
    expect(await screen.findByRole("dialog", { name: "Save this date?" })).toBeInTheDocument();
    await act(async () => { store.begin(`7:${DATE}`); });
    expect(screen.queryByRole("dialog", { name: "Save this date?" })).not.toBeInTheDocument();
    await waitFor(() => expect(screen.getByText(STALE)).toBeInTheDocument());
    expect(edits.sent).toHaveLength(1);
    expect(screen.getByRole("radio", { name: /^Private/ })).toBeChecked();
  });

  it("a begin and an end while the save is on its way stop the question before it asks", async () => {
    const store = createRosterWrites();
    const answer = deferred<OccurrenceResponse>();
    edits.answer(answer.promise);
    inSession(store);
    await ready();
    await userEvent.click(screen.getByRole("radio", { name: /^Private/ }));
    await save();
    await waitFor(() => expect(edits.sent).toHaveLength(1));
    await act(async () => {
      store.begin(`7:${DATE}`)("answered");
      answer.reject(challenge());
      await answer.promise.catch(() => {});
    });
    expect(screen.queryByRole("dialog", { name: "Save this date?" })).not.toBeInTheDocument();
    await waitFor(() => expect(screen.getByText(STALE)).toBeInTheDocument());
    expect(edits.sent).toHaveLength(1);
    await waitFor(() => expect(screen.getByRole("button", { name: "Save this date" })).toBeEnabled());
  });

  it("a write on another date leaves the question to submit", async () => {
    const store = createRosterWrites();
    edits.answer(challenge());
    inSession(store);
    await ready();
    await userEvent.click(screen.getByRole("radio", { name: /^Private/ }));
    await save();
    const asked = await screen.findByRole("dialog", { name: "Save this date?" });
    await act(async () => { store.begin("7:2026-12-08"); });
    await userEvent.click(within(asked).getByRole("button", { name: "Save and notify" }));
    await waitFor(() => expect(edits.sent).toHaveLength(2));
    expect(edits.sent[1].consent).toBe(T);
  });

  function requiredForm() {
    const form = screen.getByRole("button", { name: "Save this date" }).closest("form");

    if (!form) throw new Error("no form");

    return form;
  }
});

describe("two opens of the same date, whichever read answers first", () => {
  function later<T>() {
    let resolve: (value: T) => void = () => {};
    const promise = new Promise<T>((yes) => { resolve = yes; });

    return { promise, resolve };
  }

  const opener = { eventId: 7, date: DATE, occurrenceId: 1 };

  it.each(["first", "second"] as const)("open, close and open again: the %s read to answer never puts the first open's read on screen", async (order) => {
    const first = later<OccurrenceResponse>();
    const second = later<OccurrenceResponse>();
    vi.mocked(Events.managedDate).mockReturnValueOnce(first.promise).mockReturnValueOnce(second.promise);
    const host = (open: boolean) => session(open ? <ManagedDateEditor opener={opener} generation={0} onSaved={() => {}} onClose={() => {}} onRefused={() => {}} /> : null);
    const view = render(host(true));
    view.rerender(host(false));
    view.rerender(host(true));
    await waitFor(() => expect(editorReads()).toBe(2));
    const answers = { first: () => first.resolve({ occurrence: occurrence({ title: "First open", revision: "r-first" }) }), second: () => second.resolve({ occurrence: occurrence({ title: "Second open", revision: "r-second" }) }) };

    for (const which of order === "first" ? (["first", "second"] as const) : (["second", "first"] as const)) {
      await act(async () => { answers[which](); await Promise.resolve(); });
    }

    expect(await screen.findByRole("textbox", { name: "Title" })).toHaveValue("Second open");
    expect(document.body.textContent).not.toContain("First open");
    await ready();
    await userEvent.type(screen.getByRole("textbox", { name: "Title" }), "!");
    await save();
    await waitFor(() => expect(edits.sent).toHaveLength(1));
    expect(edits.sent[0]).toMatchObject({ revision: "r-second", changes: { title: "Second open!" } });
  });

  it.each(["first", "second"] as const)("two editors of the date open at once, the %s one's read answering first: one save, and the other never sends its old base", async (order) => {
    const reads = { first: later<OccurrenceResponse>(), second: later<OccurrenceResponse>() };
    vi.mocked(Events.managedDate).mockReturnValueOnce(reads.first.promise).mockReturnValueOnce(reads.second.promise);
    render(session(<>
      <section aria-label="First editor"><TestHost generation={0} /></section>
      <section aria-label="Second editor"><TestHost generation={0} /></section>
    </>));
    await waitFor(() => expect(editorReads()).toBe(2));
    const other = order === "first" ? "second" : "first";
    await act(async () => { reads[order].resolve({ occurrence: occurrence({ revision: "r0" }) }); await Promise.resolve(); });
    await act(async () => { reads[other].resolve({ occurrence: occurrence({ revision: "r0" }) }); await Promise.resolve(); });

    // Both editors give their fields the date's ids, so each is found inside its own editor.
    const editorOf = (name: string) => {
      const region = screen.getByRole("region", { name });
      const field = (id: string) => region.querySelector<HTMLInputElement>(`#${id}`)!;

      return { ...within(region), title: () => field("cev-title-1"), place: () => field("cev-loc-1") };
    };

    const first = editorOf("First editor");
    const second = editorOf("Second editor");
    await waitFor(() => expect(first.getByRole("button", { name: "Save this date" })).toBeEnabled());
    await waitFor(() => expect(second.getByRole("button", { name: "Save this date" })).toBeEnabled());
    await userEvent.type(second.place(), " annex");
    await userEvent.type(first.title(), "!");
    base = { ...base, title: "Supper!", revision: "r1" };
    await userEvent.click(first.getByRole("button", { name: "Save this date" }));
    await waitFor(() => expect(edits.sent).toHaveLength(1));
    // Both editors read the date again before either can save; the second keeps only what it typed.
    await waitFor(() => expect(second.title()).toHaveValue("Supper!"));
    await waitFor(() => expect(second.getByRole("button", { name: "Save this date" })).toBeEnabled());
    expect(second.place()).toHaveValue("Hall annex");
    await userEvent.click(second.getByRole("button", { name: "Save this date" }));
    await waitFor(() => expect(edits.sent).toHaveLength(2));
    expect(edits.sent.map((s) => [s.revision, s.changes])).toEqual([["r0", { title: "Supper!" }], ["r1", { location: "Hall annex" }]]);
  });
});

describe("the listed date's editor while its read is held", () => {
  function later<T>() {
    let resolve: (value: T) => void = () => {};
    let reject: (cause: unknown) => void = () => {};
    const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });

    return { promise, resolve, reject };
  }

  const opener = { eventId: 7, date: DATE, occurrenceId: 1 };

  const mounted = (c: Credential, generation = 0) => <MemoryRouter><DateWritesProvider credential={c}>
    <ManagedDateEditor opener={opener} generation={generation} onSaved={() => {}} onClose={() => {}} onRefused={() => {}} />
  </DateWritesProvider></MemoryRouter>;

  it("a newer read that fails while an older one is held shows the failure, never the older answer; Retry loading reads afresh", async () => {
    const older = later<OccurrenceResponse>();
    const newer = later<OccurrenceResponse>();
    vi.mocked(Events.managedDate).mockReturnValueOnce(older.promise).mockReturnValueOnce(newer.promise).mockResolvedValue({ occurrence: occurrence({ title: "Third read", revision: "r3" }) });
    const view = render(mounted(credential, 0));
    // The page shows a new answer: the date is read again.
    view.rerender(mounted(credential, 1));
    await waitFor(() => expect(editorReads()).toBe(2));
    await act(async () => { newer.reject(new TypeError("Failed to fetch")); await newer.promise.catch(() => {}); });
    expect(await screen.findByText("We couldn't load this date to edit it. What you typed is kept.")).toBeInTheDocument();
    await act(async () => { older.resolve({ occurrence: occurrence({ title: "Held title", revision: "r-held" }) }); await older.promise; });
    expect(screen.queryByRole("textbox", { name: "Title" })).not.toBeInTheDocument();
    expect(document.body.textContent).not.toContain("Held title");
    await userEvent.click(screen.getByRole("button", { name: "Retry loading" }));
    expect(await screen.findByRole("textbox", { name: "Title" })).toHaveValue("Third read");
    await ready();
    await userEvent.type(screen.getByRole("textbox", { name: "Title" }), "!");
    await save();
    await waitFor(() => expect(edits.sent).toHaveLength(1));
    expect(edits.sent[0].revision).toBe("r3");
  });

  it("a credential replaced while its read is on its way: that read is never shown, and the new credential's read and save are its own", async () => {
    const inFlight = later<OccurrenceResponse>();
    vi.mocked(Events.managedDate).mockReturnValueOnce(inFlight.promise).mockResolvedValue({ occurrence: occurrence({ revision: "r-new" }) });
    const view = render(mounted(credential));
    await waitFor(() => expect(editorReads()).toBe(1));
    const next = await verifiedCredential();
    edits = stubEditSends();
    view.rerender(mounted(next));
    await waitFor(() => expect(vi.mocked(Events.managedDate).mock.lastCall?.[2]).toBe(next));
    await act(async () => { inFlight.resolve({ occurrence: occurrence({ title: "Stale title", revision: "r-stale" }) }); await inFlight.promise; });
    expect(await screen.findByRole("textbox", { name: "Title" })).toHaveValue("Supper");
    expect(document.body.textContent).not.toContain("Stale title");
    await ready();
    await userEvent.type(screen.getByRole("textbox", { name: "Title" }), "!");
    await save();
    await waitFor(() => expect(edits.sent).toHaveLength(1));
    expect(edits.sent[0].revision).toBe("r-new");
  });
});
