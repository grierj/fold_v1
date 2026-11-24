import { createRosterWrites, RosterWritesProvider, type RosterWrites } from "../lib/rosterWrites";
import { stubService } from "../test/service";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { createRef, type ReactNode, type RefObject } from "react";
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
import { useEditRead } from "../lib/editBase";
import type { RevisionConflict } from "../lib/useRevisionSave";
import { ERROR_COPY } from "../lib/errors";
import { formatDayFull } from "../lib/when";
import type { Attendee, EventOccurrence, OccurrenceResponse } from "../types";
import type { JsonObject } from "../lib/wireValues";
import { stubEditSends, verifiedCredential, type EditSends } from "../test/guardedEdits";

stubService(Events, () => ({ dateVisibilityImpact: vi.fn(), managedDate: vi.fn(), removeRegistration: vi.fn() }));

stubService(Site, () => ({ get: vi.fn() }));

stubService(DateRooms, () => ({ list: vi.fn(), set: vi.fn() }));

const DATE = "2026-12-01";
const OPENER = { eventId: 7, date: DATE, occurrenceId: 1 };
const NETWORK = "We couldn't reach Fold. Check your connection and try again.";
let credential: Credential;
let edits: EditSends;

// What the editor's own manager read (pinned to the credential) answers now: the read it opens from, and the read
// after a refused save.
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
  onSaved?: () => void;
  onConflict?: (why: RevisionConflict) => void;
  onClose?: () => void;
  onRefused?: () => void;
  store?: RosterWrites;
  dialog?: boolean;
  pageFailed?: boolean;
}

// The editor as the event page opens it for the listed date OPENER names.
function editor({ onSaved = () => {}, onConflict = () => {}, onClose = () => {}, onRefused = () => {}, store, dialog = false, pageFailed = false }: Props = {}) {
  const inner = <ManagedDateEditor opener={OPENER} pageFailed={pageFailed} onSaved={onSaved} onConflict={onConflict} onClose={onClose} onRefused={onRefused} />;

  return session(dialog ? <ConfirmProvider>{inner}</ConfirmProvider> : inner, store);
}

function session(ui: ReactNode, store?: RosterWrites) {
  return <MemoryRouter><RosterWritesProvider rosterWrites={store}>{ui}</RosterWritesProvider></MemoryRouter>;
}

// The editor alone over its own read, with a host's hold (ManageDate's way of keeping a draft across a remount).
function HoldHarness({ hold, onAuthorityLost = () => {}, onSaved = () => {}, onConflict = () => {} }: {
  hold: RefObject<DateDraftHold | null>;
  onAuthorityLost?: () => void;
  onSaved?: () => void;
  onConflict?: (why: RevisionConflict) => void;
}) {
  const read = useEditRead(OPENER, "hold", true, () => {});

  return read.kind === "ready" ? <DateEditor opener={OPENER} base={read.base} hold={hold} onSaved={onSaved} onConflict={onConflict} onClose={() => {}} onAuthorityLost={onAuthorityLost} /> : <p>{read.kind}</p>;
}

async function ready() {
  const save = await screen.findByRole("button", { name: "Save this date" });
  await waitFor(() => expect(save).toBeEnabled());

  return save;
}

const save = () => userEvent.click(screen.getByRole("button", { name: "Save this date" }));

async function typeTitle(text = "Carols") {
  await ready();
  const title = screen.getByRole("textbox", { name: "Title" });
  await userEvent.clear(title);
  await userEvent.type(title, text);
}

const changed = () => new ApiError(409, "x", { code: "date_changed" });

function deferred<T>() {
  let resolve: (value: T) => void = () => {};
  let reject: (cause: unknown) => void = () => {};
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });

  return { promise, resolve, reject };
}

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
  await waitFor(() => expect(onSaved).toHaveBeenCalledOnce());
  // One POST from the read's revision: no preview, no consent until the writer asks, the reason trimmed.
  expect(edits.sent).toHaveLength(1);
  expect(edits.sent[0]).toMatchObject({ eventId: 7, date: DATE, revision: "r0", changes: { visibility: "public" }, reason: "Welcome neighbours" });
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
  base = { ...base, ...over };
  render(editor({ onRefused }));
  await waitFor(() => expect(onRefused).toHaveBeenCalledOnce());
  expect(screen.queryByRole("button", { name: "Save this date" })).not.toBeInTheDocument();
  expect(screen.queryByRole("textbox")).not.toBeInTheDocument();
});

it("on the group page, opens nothing for a date the read puts in another group", async () => {
  const onRefused = vi.fn();
  base = occurrence({ revision: "r0", groupId: 9, groupName: "Other group", groupVisibility: "public" });
  render(session(<ManagedDateEditor opener={{ ...OPENER, groupId: 3 }} onSaved={() => {}} onConflict={() => {}} onClose={() => {}} onRefused={onRefused} />));
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

it("a read that fails offers Retry loading, with the fields hidden", async () => {
  vi.mocked(Events.managedDate).mockRejectedValueOnce(new TypeError("Failed to fetch"));
  render(editor());
  expect(await screen.findByText("We couldn't load this date to edit it.")).toBeInTheDocument();
  expect(screen.queryByRole("textbox")).not.toBeInTheDocument();
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

it("puts the Room control beside Location when rooms are on, and not otherwise", async () => {
  vi.mocked(Site.get).mockResolvedValue({ name: "Fold", timezone: "UTC", selfSignup: "off", roomsEnabled: true, pushEnabled: true });
  vi.mocked(DateRooms.list).mockResolvedValue({ rooms: [], booking: null, dateRoom: { id: 70, revision: "r1" } });
  render(editor());
  expect(await screen.findByRole("combobox", { name: /^Room/ })).toBeInTheDocument();
  expect(DateRooms.list).toHaveBeenCalledWith(7, DATE);
  expect(screen.getByRole("textbox", { name: /^Location/ })).toBeInTheDocument();
});

// DEC-167: the Room control is its own action. When the date's room changed under a pick, the control shows the room
// as it is now and says so at the field; the editor stays open with its draft.
it("keeps the editor and its draft when the date's room changed under a pick, and says so at the Room field", async () => {
  vi.mocked(Site.get).mockResolvedValue({ name: "Fold", timezone: "UTC", selfSignup: "off", roomsEnabled: true, pushEnabled: true });
  const hall = { id: 1, name: "Fellowship Hall", building: "Main", capacity: 80, features: "", restricted: false, busy: false, tooSmall: false };
  const chapel = { ...hall, id: 2, name: "Chapel", building: "" };
  vi.mocked(DateRooms.list)
    .mockResolvedValueOnce({ rooms: [hall, chapel], booking: null, dateRoom: { id: 70, revision: "r1" } })
    .mockResolvedValueOnce({
      rooms: [hall, chapel], dateRoom: { id: 70, revision: "r3" },
      booking: { roomId: 2, roomName: "Chapel", building: "", status: "confirmed", setupMinutes: 0, teardownMinutes: 0, doubleBooked: false, tooSmall: false, pending: false },
    });
  vi.mocked(DateRooms.set).mockRejectedValueOnce(new ApiError(409, "changed", { code: "date_room_changed" })).mockRejectedValueOnce(new ApiError(403, "no", { code: "not_allowed" }));
  const onConflict = vi.fn();
  render(editor({ onConflict }));
  const title = await screen.findByRole("textbox", { name: /^Title/ });
  await userEvent.clear(title);
  await userEvent.type(title, "Supper night");
  await userEvent.selectOptions(await screen.findByRole("combobox", { name: /^Room/ }), "1");
  const line = await screen.findByText("This date's room changed while you were choosing, so your choice could not be saved. This is the room as it is now.");
  await waitFor(() => expect(line).toHaveFocus());
  expect(screen.getByRole("combobox", { name: /^Room/ })).toHaveValue("2");
  expect(screen.getByRole("textbox", { name: /^Title/ })).toHaveValue("Supper night");
  expect(screen.getByRole("button", { name: /^Save/ })).toBeInTheDocument();
  expect(onConflict).not.toHaveBeenCalled();
  // A pick refused because this person may no longer manage the date closes the editor the way its own save does.
  await userEvent.selectOptions(screen.getByRole("combobox", { name: /^Room/ }), "1");
  await waitFor(() => expect(onConflict).toHaveBeenCalledExactlyOnceWith("gone"));
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

it("an unchanged form closes without sending anything, and so does one changed only by spaces around text", async () => {
  const onClose = vi.fn();
  render(editor({ onClose }));
  await ready();
  await save();
  expect(onClose).toHaveBeenCalledOnce();
  await userEvent.type(screen.getByRole("textbox", { name: "Title" }), "  ");
  await save();
  expect(onClose).toHaveBeenCalledTimes(2);
  expect(edits.sent).toHaveLength(0);
});

it("sends only what changed, text as the server stores it: a title alone keeps the stored clock and the caps", async () => {
  base = occurrence({ revision: "r0", capacity: 20, remoteAvailable: true, remoteCapacity: 0, durationMinutes: 45 });
  render(editor());
  await ready();
  await userEvent.type(screen.getByRole("textbox", { name: "Title" }), " night  ");
  await save();
  await waitFor(() => expect(edits.sent).toHaveLength(1));
  expect(edits.sent[0].changes).toEqual({ title: "Supper night" });
});

it("names the series day in words, and no date in the form as digits", async () => {
  base = occurrence({ revision: "r0", tzid: "America/Boise", startsLocal: "2026-12-01T19:00", startsAt: "2026-12-02T02:00:00Z" });
  render(editor());
  await ready();
  expect(screen.getByText(/The series date stays Tuesday, December 1, 2026\./)).toBeInTheDocument();
  const form = screen.getByRole("button", { name: "Save this date" }).closest("form");
  expect(form?.querySelector("p")?.textContent).not.toMatch(/\d{4}-\d{2}-\d{2}/);
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

// DEC-164: refused as stale, the editor reads the date once and compares what the save can write (lib/editBase
// dateFields): already stored, as it opened, or changed.
describe("a save refused as stale (date_changed)", () => {
  it("is done when the date already shows what the save would leave: an earlier press stored it", async () => {
    const onSaved = vi.fn();
    const onConflict = vi.fn();
    edits.answer(changed());
    render(editor({ onSaved, onConflict }));
    await typeTitle();
    base = { ...base, title: "Carols", own: { title: "Carols" }, revision: "r1" };
    await save();
    await waitFor(() => expect(onSaved).toHaveBeenCalledOnce());
    expect(edits.sent).toHaveLength(1);
    expect(editorReads()).toBe(2);
    expect(onConflict).not.toHaveBeenCalled();
  });

  it("sends the same save again under the new revision, unseen, when nothing it can write changed", async () => {
    const onSaved = vi.fn();
    const onConflict = vi.fn();
    edits.answer(changed());
    render(editor({ onSaved, onConflict }));
    await typeTitle();
    // A room booking or a restart moved the revision only.
    base = { ...base, revision: "r1" };
    await save();
    await waitFor(() => expect(onSaved).toHaveBeenCalledOnce());
    expect(edits.sent.map((s) => [s.revision, s.changes])).toEqual([["r0", { title: "Carols" }], ["r1", { title: "Carols" }]]);
    expect(onConflict).not.toHaveBeenCalled();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it.each([
    ["the place", { location: "Annex", own: { location: "Annex" } }],
    ["the seat limit", { capacity: 12, own: { capacityOverride: 12 } }],
    ["the visibility the date follows", { seriesVisibility: "private" as const, visibility: "private" as const }],
    ["the time zone its clock is on", { tzid: "America/Boise" }],
    ["whether it has remote seats", { remoteAvailable: true }],
    ["another manager's own title over the inherited one", { title: "Potluck", own: { title: "Potluck" } }],
  ])("closes as a conflict when %s changed, and sends nothing more", async (_label, over) => {
    const onSaved = vi.fn();
    const onConflict = vi.fn();
    edits.answer(changed());
    render(editor({ onSaved, onConflict }));
    await ready();
    fireEvent.change(screen.getByLabelText("Start time"), { target: { value: "20:00" } });
    base = { ...base, ...over, revision: "r1" };
    await save();
    await waitFor(() => expect(onConflict).toHaveBeenCalledExactlyOnceWith("changed"));
    expect(edits.sent).toHaveLength(1);
    expect(onSaved).not.toHaveBeenCalled();
  });

  it("closes as a conflict when the date was cancelled meanwhile", async () => {
    const onConflict = vi.fn();
    edits.answer(changed());
    render(editor({ onConflict }));
    await typeTitle();
    base = { ...base, cancelled: true, revision: "r1" };
    await save();
    await waitFor(() => expect(onConflict).toHaveBeenCalledExactlyOnceWith("changed"));
    expect(edits.sent).toHaveLength(1);
  });

  // A rule's own hour can give a date a clock that differs from the series start with no start of its own: that is not
  // this save's start stored, so the save goes again and the start becomes the date's own.
  it("sends a start change again when the read shows the start only as a clock the rule gives the date", async () => {
    const onSaved = vi.fn();
    const onConflict = vi.fn();
    edits.answer(changed());
    render(editor({ onSaved, onConflict }));
    await ready();
    fireEvent.change(screen.getByLabelText("Start time"), { target: { value: "20:00" } });
    base = { ...base, startsLocal: "2026-12-01T20:00", startsAt: "2026-12-01T20:00:00Z", revision: "r1" };
    await save();
    await waitFor(() => expect(onSaved).toHaveBeenCalledOnce());
    expect(edits.sent.map((s) => [s.revision, s.changes])).toEqual([["r0", { startsLocal: "2026-12-01T20:00" }], ["r1", { startsLocal: "2026-12-01T20:00" }]]);
    expect(onConflict).not.toHaveBeenCalled();
  });

  it("sends a change to the event's own value again: the date still falls back to it, and the save leaves it so", async () => {
    const onSaved = vi.fn();
    edits.answer(changed());
    render(editor({ onSaved }));
    await typeTitle();
    base = { ...base, location: "New hall", seriesLocation: "New hall", revision: "r1" };
    await save();
    await waitFor(() => expect(onSaved).toHaveBeenCalledOnce());
    expect(edits.sent.map((s) => s.revision)).toEqual(["r0", "r1"]);
  });

  it.each([403, 404])("the read after the refusal answering %i: this person may no longer manage the date", async (status) => {
    const onConflict = vi.fn();
    edits.answer(changed());
    render(editor({ onConflict }));
    await typeTitle();
    vi.mocked(Events.managedDate).mockRejectedValue(new ApiError(status, "SECRET", { code: "x" }));
    await save();
    await waitFor(() => expect(onConflict).toHaveBeenCalledExactlyOnceWith("gone"));
    expect(edits.sent).toHaveLength(1);
  });

  it.each([403, 404])("a send answered %i, first or sent again: this person may no longer manage the date", async (status) => {
    for (const answers of [[new ApiError(status, "SECRET", { code: "x" })], [changed(), new ApiError(status, "SECRET", { code: "x" })]]) {
      const onConflict = vi.fn();
      edits = stubEditSends();
      edits.answer(...answers);
      base = occurrence({ revision: "r0" });
      const view = render(editor({ onConflict }));
      await typeTitle();
      base = { ...base, revision: "r1" };
      await save();
      await waitFor(() => expect(onConflict).toHaveBeenCalledExactlyOnceWith("gone"));
      expect(edits.sent).toHaveLength(answers.length);
      view.unmount();
    }
  });

  it("refused three times with nothing it can write changed: the ordinary error, the draft kept", async () => {
    const onConflict = vi.fn();
    edits.answer(changed(), changed(), changed());
    render(editor({ onConflict }));
    await typeTitle();
    base = { ...base, revision: "r1" };
    await save();
    expect(await screen.findByText("We couldn't update this date. Please try again.")).toBeInTheDocument();
    expect(edits.sent).toHaveLength(3);
    expect(onConflict).not.toHaveBeenCalled();
    expect(screen.getByRole("textbox", { name: "Title" })).toHaveValue("Carols");
  });

  it("a refusal that lands after the editor went away reads and sends nothing more", async () => {
    const onConflict = vi.fn();
    const answer = deferred<OccurrenceResponse>();
    edits.answer(answer.promise);
    const view = render(editor({ onConflict }));
    await typeTitle();
    await save();
    await waitFor(() => expect(edits.sent).toHaveLength(1));
    view.rerender(session(null));
    await act(async () => { answer.reject(changed()); await answer.promise.catch(() => {}); });
    expect(editorReads()).toBe(1);
    expect(edits.sent).toHaveLength(1);
    expect(onConflict).not.toHaveBeenCalled();
  });
});

// DEC-164: a reply that never arrived is the ordinary save error; the next press settles it through the refusal.
describe("a lost reply", () => {
  it.each([
    ["a dropped connection", () => new TypeError("Failed to fetch"), NETWORK],
    ["a server failure", () => new ApiError(502, "proxy", {}, true), "We couldn't update this date. Please try again in a minute."],
  ])("after %s, the editor stays open with the draft; the next press finds the first one stored and is done", async (_label, lost, line) => {
    const onSaved = vi.fn();
    const onConflict = vi.fn();
    edits.answer(lost(), changed());
    render(editor({ onSaved, onConflict }));
    await typeTitle();
    await save();
    expect(await screen.findByText(line)).toBeInTheDocument();
    expect(screen.getByRole("textbox", { name: "Title" })).toHaveValue("Carols");
    expect(screen.getByRole("button", { name: "Save this date" })).toBeEnabled();
    expect(onSaved).not.toHaveBeenCalled();
    // The first press was stored; its answer was lost.
    base = { ...base, title: "Carols", own: { title: "Carols" }, revision: "r1" };
    await save();
    await waitFor(() => expect(onSaved).toHaveBeenCalledOnce());
    expect(edits.sent.map((s) => s.revision)).toEqual(["r0", "r0"]);
    expect(onConflict).not.toHaveBeenCalled();
  });

  it("a reply lost before anything was stored: the next press saves", async () => {
    const onSaved = vi.fn();
    edits.answer(new TypeError("Failed to fetch"));
    render(editor({ onSaved }));
    await typeTitle();
    await save();
    expect(await screen.findByText(NETWORK)).toBeInTheDocument();
    await save();
    await waitFor(() => expect(onSaved).toHaveBeenCalledOnce());
    expect(edits.sent.map((s) => s.revision)).toEqual(["r0", "r0"]);
    expect(editorReads()).toBe(1);
  });

  it.each([
    ["a seat limit", async () => { await userEvent.type(screen.getByRole("spinbutton", { name: /^Capacity for this date/ }), "12"); }, { capacity: 12, own: { capacityOverride: 12 } }],
    ["a start time", async () => { fireEvent.change(screen.getByLabelText("Start time"), { target: { value: "20:00" } }); },
      { startsLocal: "2026-12-01T20:00", startsAt: "2026-12-01T20:00:00Z", own: { startsLocal: "2026-12-01T20:00" } }],
  ])("%s stored with its answer lost: the next press finds it stored and is done", async (_label, change, stored) => {
    const onSaved = vi.fn();
    const onConflict = vi.fn();
    edits.answer(new TypeError("Failed to fetch"), changed());
    render(editor({ onSaved, onConflict }));
    await ready();
    await change();
    await save();
    expect(await screen.findByText(NETWORK)).toBeInTheDocument();
    base = { ...base, ...stored, revision: "r1" };
    await save();
    await waitFor(() => expect(onSaved).toHaveBeenCalledOnce());
    expect(edits.sent.map((s) => s.revision)).toEqual(["r0", "r0"]);
    expect(onConflict).not.toHaveBeenCalled();
  });

  // #455: a lost send may have been stored and then undone by someone else; sending again would redo it.
  it("after a lost reply, a refusal finding the date as it opened closes as a conflict, sending the opened revision only", async () => {
    const onSaved = vi.fn();
    const onConflict = vi.fn();
    edits.answer(new TypeError("Failed to fetch"), changed());
    render(editor({ onSaved, onConflict }));
    await typeTitle();
    await save();
    expect(await screen.findByText(NETWORK)).toBeInTheDocument();
    // Stored at r1, then another manager put the opening title back (r2).
    base = { ...base, revision: "r2" };
    await save();
    await waitFor(() => expect(onConflict).toHaveBeenCalledExactlyOnceWith("changed"));
    expect(edits.sent.map((s) => s.revision)).toEqual(["r0", "r0"]);
    expect(onSaved).not.toHaveBeenCalled();
  });

  // The reason goes to the audit row and the notice, and no read returns it.
  it("a visibility change stored with its answer lost, then a new reason: the next press is a conflict, never a save that drops the reason", async () => {
    const onSaved = vi.fn();
    const onConflict = vi.fn();
    edits.answer(new TypeError("Failed to fetch"), changed());
    base = occurrence({ revision: "r0", visibility: "private", seriesVisibility: "private", groupId: 4, groupVisibility: "invite_only", groupName: "Hidden fellowship" });
    const view = render(editor({ onSaved, onConflict }));
    await ready();
    await userEvent.click(screen.getByRole("radio", { name: /^Public/ }));
    await userEvent.type(screen.getByRole("textbox", { name: /^Reason/ }), "Choir");
    await save();
    expect(await screen.findByText(NETWORK)).toBeInTheDocument();
    base = { ...base, visibility: "public", own: { visibility: "public" }, revision: "r1" };
    await userEvent.type(screen.getByRole("textbox", { name: /^Reason/ }), " and friends");
    await save();
    await waitFor(() => expect(onConflict).toHaveBeenCalledExactlyOnceWith("changed"));
    expect(edits.sent.map((s) => s.reason)).toEqual(["Choir", "Choir and friends"]);
    expect(onSaved).not.toHaveBeenCalled();
    view.unmount();

    // The same reason both times: the first press stored it, and the second is done.
    edits = stubEditSends();
    edits.answer(new TypeError("Failed to fetch"), changed());
    base = occurrence({ revision: "r0", visibility: "private", seriesVisibility: "private", groupId: 4, groupVisibility: "invite_only", groupName: "Hidden fellowship" });
    render(editor({ onSaved, onConflict }));
    await ready();
    await userEvent.click(screen.getByRole("radio", { name: /^Public/ }));
    await userEvent.type(screen.getByRole("textbox", { name: /^Reason/ }), "Choir");
    await save();
    expect(await screen.findByText(NETWORK)).toBeInTheDocument();
    base = { ...base, visibility: "public", own: { visibility: "public" }, revision: "r1" };
    await save();
    await waitFor(() => expect(onSaved).toHaveBeenCalledOnce());
    expect(onConflict).toHaveBeenCalledOnce();
  });

  it("remembers a lost reply across a remount from the hold", async () => {
    const hold = createRef<DateDraftHold>();
    const onConflict = vi.fn();
    edits.answer(new TypeError("Failed to fetch"), changed());
    const view = render(session(<HoldHarness hold={hold} onConflict={onConflict} />));
    await typeTitle();
    await save();
    expect(await screen.findByText(NETWORK)).toBeInTheDocument();
    view.unmount();
    base = { ...base, revision: "r2" };
    render(session(<HoldHarness hold={hold} onConflict={onConflict} />));
    await ready();
    await save();
    await waitFor(() => expect(onConflict).toHaveBeenCalledExactlyOnceWith("changed"));
    expect(edits.sent.map((s) => s.revision)).toEqual(["r0", "r0"]);
  });

  it.each([
    ["an empty object (a page from something in between)", {}],
    ["another date's answer", { occurrence: occurrence({ id: 2, revision: "r1" }) }],
    ["an answer with no revision", { occurrence: occurrence({ id: 1 }) }],
  ])("keeps the draft and says the ordinary error, closing nothing, when the answer is %s", async (_label, answer) => {
    const onSaved = vi.fn();
    const onConflict = vi.fn();
    // SAFETY: a wire answer the static type would not allow, as something in between could send it.
    edits.answer(answer as OccurrenceResponse);
    render(editor({ onSaved, onConflict }));
    await typeTitle();
    await save();
    expect(await screen.findByText("We couldn't update this date. Please try again in a minute.")).toBeInTheDocument();
    expect(screen.getByRole("textbox", { name: "Title" })).toHaveValue("Carols");
    expect(screen.getByRole("button", { name: "Save this date" })).toBeEnabled();
    expect(onSaved).not.toHaveBeenCalled();
    expect(onConflict).not.toHaveBeenCalled();
  });
});

describe("the hold", () => {
  it("brings back every typed field and the reason when the editor is mounted again", async () => {
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

  it("keeps the read the draft opened from: under a newer stored date it merges nothing and closes nothing, and the next Save decides", async () => {
    const hold = createRef<DateDraftHold>();
    const onConflict = vi.fn();
    const view = render(session(<HoldHarness hold={hold} onConflict={onConflict} />));
    await ready();
    await userEvent.clear(screen.getByRole("spinbutton", { name: "Minutes" }));
    await userEvent.type(screen.getByRole("spinbutton", { name: "Minutes" }), "150");
    view.unmount();
    base = occurrence({ revision: "r1", title: "Stored rename", location: "New hall", own: { title: "Stored rename", location: "New hall" } });
    render(session(<HoldHarness hold={hold} onConflict={onConflict} />));
    await ready();
    expect(screen.getByRole("spinbutton", { name: "Minutes" })).toHaveValue(150);
    expect(screen.getByRole("textbox", { name: "Title" })).toHaveValue("Supper");
    expect(screen.getByRole("textbox", { name: /^Location/ })).toHaveValue("Hall");
    expect(onConflict).not.toHaveBeenCalled();
    edits.answer(changed());
    await save();
    expect(edits.sent[0]).toMatchObject({ revision: "r0", changes: { durationMinutes: 150 } });
    await waitFor(() => expect(onConflict).toHaveBeenCalledExactlyOnceWith("changed"));
  });

  it("a save stored while the editor is off the screen ends the draft and tells the host", async () => {
    const hold = createRef<DateDraftHold>();
    const onSaved = vi.fn();
    const answer = deferred<OccurrenceResponse>();
    edits.answer(answer.promise);
    const view = render(session(<HoldHarness hold={hold} onSaved={onSaved} />));
    await typeTitle();
    await save();
    await waitFor(() => expect(edits.sent).toHaveLength(1));
    view.unmount();
    expect(hold.current?.draft.title).toBe("Carols");
    await act(async () => { answer.resolve({ occurrence: occurrence({ title: "Carols", revision: "r1" }) }); await answer.promise; });
    expect(onSaved).toHaveBeenCalledOnce();
    expect(hold.current).toBeNull();
  });

  it("a question withdrawn while the editor is off the screen says so when it comes back", async () => {
    const hold = createRef<DateDraftHold>();
    const answer = deferred<OccurrenceResponse>();
    edits.answer(answer.promise);
    const view = render(session(<ConfirmProvider><HoldHarness hold={hold} /></ConfirmProvider>));
    await ready();
    await userEvent.click(screen.getByRole("radio", { name: /^Private/ }));
    await save();
    await waitFor(() => expect(edits.sent).toHaveLength(1));
    view.unmount();
    await act(async () => {
      answer.reject(new ApiError(409, "x", { code: "date_needs_consent", impact: { visibility: "private", currentVisibility: "public", tightened: [{ id: 7, title: "One date", visibility: "public", registrants: 2, occurrenceId: 1 }], leaderOpened: [], consent: "1".repeat(32) } }));
      await answer.promise.catch(() => {});
    });
    // Leaving the page while the editor is off the screen asks first: the change was never saved.
    expect(hold.current?.unsent).toBe(true);
    render(session(<ConfirmProvider><HoldHarness hold={hold} /></ConfirmProvider>));
    expect(await screen.findByText("The page changed while we were asking, so your changes could not be saved. You can save again.")).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: /^Private/ })).toBeChecked();
    expect(edits.sent).toHaveLength(1);
  });

  it("the listed date's draft waits off the screen while the page's read has failed, and comes back without a new read", async () => {
    const view = render(editor());
    await typeTitle();
    view.rerender(editor({ pageFailed: true }));
    expect(screen.getByText("This date can be edited again once the page loads. What you typed is kept.")).toBeInTheDocument();
    expect(screen.queryByRole("textbox", { name: "Title" })).not.toBeInTheDocument();
    view.rerender(editor());
    expect(await screen.findByRole("textbox", { name: "Title" })).toHaveValue("Carols");
    expect(editorReads()).toBe(1);
  });

  it("Close and opening again start from a new read of the stored date", async () => {
    function Toggle({ open }: { open: boolean }) {
      return open ? <ManagedDateEditor opener={OPENER} onSaved={() => {}} onConflict={() => {}} onClose={() => {}} onRefused={() => {}} /> : null;
    }

    const view = render(session(<Toggle open />));
    await ready();
    await userEvent.clear(screen.getByRole("spinbutton", { name: "Minutes" }));
    await userEvent.type(screen.getByRole("spinbutton", { name: "Minutes" }), "150");
    base = { ...base, revision: "r1", title: "Carols" };
    view.rerender(session(<Toggle open={false} />));
    view.rerender(session(<Toggle open />));
    await ready();
    expect(screen.getByRole("spinbutton", { name: "Minutes" })).toHaveValue(60);
    expect(screen.getByRole("textbox", { name: "Title" })).toHaveValue("Carols");
    expect(editorReads()).toBe(2);
  });

  it.each(["first", "second"] as const)("open, close and open again: the %s read to answer never puts the first open's read on screen", async (order) => {
    const first = deferred<OccurrenceResponse>();
    const second = deferred<OccurrenceResponse>();
    vi.mocked(Events.managedDate).mockReturnValueOnce(first.promise).mockReturnValueOnce(second.promise);
    const host = (open: boolean) => session(open ? <ManagedDateEditor opener={OPENER} onSaved={() => {}} onConflict={() => {}} onClose={() => {}} onRefused={() => {}} /> : null);
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
});

it("two managers on one date: the second save, made from the read before the first, closes as a conflict", async () => {
  const conflicts = { first: vi.fn(), second: vi.fn() };
  const saves = { first: vi.fn(), second: vi.fn() };
  render(session(<>
    <section aria-label="First editor"><ManagedDateEditor opener={OPENER} onSaved={saves.first} onConflict={conflicts.first} onClose={() => {}} onRefused={() => {}} /></section>
    <section aria-label="Second editor"><ManagedDateEditor opener={OPENER} onSaved={saves.second} onConflict={conflicts.second} onClose={() => {}} onRefused={() => {}} /></section>
  </>));

  // Both editors give their fields the date's ids, so each is found inside its own editor.
  const editorOf = (name: string) => {
    const region = screen.getByRole("region", { name });
    const field = (id: string) => region.querySelector<HTMLInputElement>(`#${id}`)!;

    return { ...within(region), title: () => field("cev-title-1"), place: () => field("cev-loc-1") };
  };

  await waitFor(() => expect(screen.getAllByRole("button", { name: "Save this date" })).toHaveLength(2));
  const first = editorOf("First editor");
  const second = editorOf("Second editor");
  await userEvent.type(first.title(), "!");
  await userEvent.click(first.getByRole("button", { name: "Save this date" }));
  await waitFor(() => expect(saves.first).toHaveBeenCalledOnce());
  base = { ...base, title: "Supper!", own: { title: "Supper!" }, revision: "r1" };
  edits.answer(changed());
  await userEvent.type(second.place(), " annex");
  await userEvent.click(second.getByRole("button", { name: "Save this date" }));
  await waitFor(() => expect(conflicts.second).toHaveBeenCalledExactlyOnceWith("changed"));
  expect(edits.sent.map((s) => [s.revision, s.changes])).toEqual([["r0", { title: "Supper!" }], ["r0", { location: "Hall annex" }]]);
  expect(saves.second).not.toHaveBeenCalled();
});

// Grier (DEC-161, DEC-163): nothing tells a person to close, reopen, reload or refresh, and nothing claims proof.
it("says nothing about closing, reopening, reloading or refreshing, and claims no proof, on any of its own lines", async () => {
  const lines: string[] = [];
  const said = () => [...document.querySelectorAll("[role='status'], [role='alert'], p")].map((el) => el.textContent ?? "");
  vi.mocked(Events.managedDate).mockRejectedValueOnce(new TypeError("Failed to fetch"));
  const view = render(editor({ dialog: true }));
  await screen.findByRole("button", { name: "Retry loading" });
  lines.push(...said());
  await userEvent.click(screen.getByRole("button", { name: "Retry loading" }));
  edits.answer(changed(), changed(), changed(), new TypeError("Failed to fetch"));
  await typeTitle();
  base = { ...base, revision: "r1" };
  await save();
  await screen.findByText("We couldn't update this date. Please try again.");
  lines.push(...said());
  await save();
  await screen.findByText(NETWORK);
  lines.push(...said());
  view.rerender(editor({ dialog: true, pageFailed: true }));
  lines.push(...said());
  expect(lines.length).toBeGreaterThan(3);

  for (const line of lines) expect(line).not.toMatch(/\b(re-?open|reload|refresh)\b|\bclose (it|this|the)\b|now shows your changes|went through|tell whether/i);
});

// W3: a save refused because someone who left kept the date (DEC-156).
const CHECKING = "Your change isn't saved yet. Checking that you can still manage sign-ups…";
const REFUSED = "Your change isn't saved. You can't manage sign-ups for this event now.";
const NOBODY_ONE = "Nobody is keeping this date now. Save again to apply your change.";
const keptRow = (name: string): Attendee => ({ registrationId: 31, userId: 5, name, status: "going", attendanceMode: "in_person", guestCount: 0, keptAfterLeaving: true });
const managedRead = (over: Partial<EventOccurrence> = {}): OccurrenceResponse => ({ occurrence: occurrence({ attendeesVisible: true, keptCount: 1, attendees: [keptRow("Old Name")], ...over }) });
const kept409 = (body: JsonObject = { keptDates: [{ date: DATE, keptCount: 1 }] }) => new ApiError(409, "SECRET", { code: "date_kept_after_leaving", ...body });

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
// never from a preview, and a yes sends the same save with only the returned token added.
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

  it("asks from the writer's answer, and a yes sends the same save with only its token added", async () => {
    edits.answer(decision(tighten(2, "public", T1)));
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(true);
    await open();
    await waitFor(() => expect(edits.sent).toHaveLength(2));
    expect(confirm).toHaveBeenCalledExactlyOnceWith("Tuesday, December 1, 2026. This date is public now. Saving makes it invite-only, so fewer people will be able to see it. This cancels and notifies 2 registered people who can no longer see this date. Save this date?");
    expect(edits.sent[0].consent).toBeUndefined();
    expect(edits.sent[1]).toEqual({ ...edits.sent[0], consent: T1 });
    expect(edits.sent[1]).toMatchObject({ revision: "r0", changes: { visibility: "series" } });
    expect(Events.dateVisibilityImpact).not.toHaveBeenCalled();
  });

  it("a no is not a lost reply: the next press, refused after a room booking, sends again", async () => {
    edits.answer(decision(tighten(1, "public", T1)), changed());
    vi.spyOn(window, "confirm").mockReturnValueOnce(false).mockReturnValue(true);
    await open();
    await waitFor(() => expect(window.confirm).toHaveBeenCalledOnce());
    await ready();
    base = { ...base, revision: "r1" };
    await save();
    await waitFor(() => expect(edits.sent).toHaveLength(3));
    expect(edits.sent.map((s) => s.revision)).toEqual(["r0", "r0", "r1"]);
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

  it("a yes refused as stale whose date is as it opened sends the save again under the new revision, with the token", async () => {
    edits.answer(decision(tighten(2, "public", T1)), changed());
    // The yes is answered, as it lands, while another manager's restart moved the revision only.
    vi.spyOn(window, "confirm").mockImplementation(() => {
      base = { ...base, revision: "r1" };

      return true;
    });
    await open();
    await waitFor(() => expect(edits.sent).toHaveLength(3));
    expect(edits.sent.map((s) => [s.revision, s.consent])).toEqual([["r0", undefined], ["r0", T1], ["r1", T1]]);
  });

  it("an editor closed while the question is open answers it no, so the date can be saved again", async () => {
    edits.answer(decision(tighten(2, "public", T1)));
    base = occurrence({ revision: "r0", visibility: "public", visibilityOverride: "public", seriesVisibility: "invite_only" });

    function Toggle({ open }: { open: boolean }) {
      return <ConfirmProvider>{open && <ManagedDateEditor opener={OPENER} onSaved={() => {}} onConflict={() => {}} onClose={() => {}} onRefused={() => {}} />}</ConfirmProvider>;
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
  const STALE = "The page changed while we were asking, so your changes could not be saved. You can save again.";
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
