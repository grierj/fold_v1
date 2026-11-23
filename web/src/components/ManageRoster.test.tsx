import { renderWithAuth } from "../test/auth";
import { stubService } from "../test/service";
import { act, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import userEvent from "@testing-library/user-event";
import { beforeEach, expect, it, vi } from "vitest";
import { ApiError } from "../api/core";
import { Events } from "../api/events";
import { People } from "../api/people";
import { occurrence } from "../test/occurrence";
import type { Attendee, EventOccurrence, User } from "../types";
import { createRosterWrites, DateWritesProvider, useRosterWrites, type RosterGate, type RosterWriteEnd, type RosterWrites } from "../lib/dateWrites";
import { ConfirmProvider, usePageFacts } from "./ui/ConfirmDialog";
import { ManageRoster, type RosterRecheck } from "./ManageRoster";

type TestSession = { user: Pick<User, "id" | "status" | "accountType"> | null };

const actor: TestSession = { user: null };

const render = renderWithAuth(() => actor);

stubService(People, () => ({ search: vi.fn() }));

stubService(Events, () => ({ removeRegistration: vi.fn(), promoteRegistration: vi.fn(), restoreRegistration: vi.fn(), addRegistration: vi.fn() }));

const child: Attendee = { registrationId: 91, name: "Kit", status: "waitlisted", guestCount: 0, attendanceMode: "remote", householdPerson: { personId: 3, name: "Kit", kind: "child", age: 6, householdName: "Leader household", registeredBy: { userId: 7, name: "Holder" } } };

beforeEach(() => { vi.resetAllMocks(); actor.user = null; vi.spyOn(window, "confirm").mockReturnValue(true); });

it("acts on the registration ID when user and person IDs overlap", async () => {
  const onChanged = vi.fn();
  render(<ManageRoster instance={occurrence({ attendees: [child, { registrationId: 92, userId: 3, name: "Sam", status: "going", attendanceMode: "in_person", guestCount: 0 }] })} onChanged={onChanged} onClose={() => {}} />);
  await userEvent.click(screen.getByRole("button", { name: "Promote" }));
  expect(Events.promoteRegistration).toHaveBeenCalledWith(7, "2026-12-01", 91);
  await userEvent.click(screen.getByRole("button", { name: "More for Kit" }));
  await userEvent.click(screen.getByRole("menuitem", { name: "Remove Kit" }));
  expect(Events.removeRegistration).toHaveBeenCalledWith(7, "2026-12-01", 91);
  await userEvent.click(screen.getByRole("button", { name: "More for Sam" }));
  expect(screen.getByRole("menuitem", { name: "Remove Sam" })).toBeInTheDocument();
  expect(window.confirm).toHaveBeenCalledWith(expect.stringContaining("Remove Kit's registration"));
});

it("recovers the named person with the original holder and confirms their own pool", async () => {
  render(<ManageRoster instance={occurrence({ remoteAvailable: true, remoteCapacity: 1, remoteSpotsLeft: 0, spotsLeft: 4, removedRegistrations: [{ ...child, status: "removed" }] })} onChanged={() => {}} onClose={() => {}} />);
  await userEvent.click(screen.getByRole("button", { name: "Add back Kit" }));
  expect(window.confirm).toHaveBeenCalledWith(expect.stringContaining("remote seats are full"));
  expect(Events.restoreRegistration).toHaveBeenCalledWith(7, "2026-12-01", { personId: 3, registeredBy: 7 }, "remote", true);
});

it("shows childcare refusal without pretending the restore succeeded", async () => {
  vi.mocked(Events.restoreRegistration).mockRejectedValue(new ApiError(409, "private server wording", { code: "childcare_full" }));
  const changed = vi.fn();
  render(<ManageRoster instance={occurrence({ removedRegistrations: [{ ...child, status: "not_restored" }] })} onChanged={changed} onClose={() => {}} />);
  await userEvent.click(screen.getByRole("button", { name: "Add back Kit" }));
  expect(screen.getByRole("alert")).toHaveTextContent("Childcare is full"); expect(changed).not.toHaveBeenCalled();
});

it("keeps actions beside compact details and names statuses in words", () => {
  render(<ManageRoster instance={occurrence({ attendees: [{ ...child, attendanceMode: "in_person" }, { ...child, registrationId: 93, name: "Mary", attendanceMode: "in_person" }, { registrationId: 92, userId: 3, name: "Sam", status: "going", attendanceMode: "in_person", guestCount: 0 }] })} onChanged={() => {}} onClose={() => {}} />);
  expect(screen.getByText("Waitlist position 1")).toBeInTheDocument();
  expect(screen.getByText("Waitlist position 2")).toBeInTheDocument();
  const row = screen.getByRole("button", { name: "More for Sam" }).closest("li")!;
  expect(within(row).getByText("Going")).toBeInTheDocument();
});

it("keeps waitlist positions per pool and places status before care notes", () => {
  render(<ManageRoster instance={occurrence({ childcareAvailable: true, attendees: [
    { ...child, registrationId: 101, attendanceMode: "remote", householdPerson: { ...child.householdPerson!, careNotes: "First note" } },
    { ...child, registrationId: 102, attendanceMode: "in_person" },
    { ...child, registrationId: 103, attendanceMode: "remote" },
  ] })} onChanged={() => {}} onClose={() => {}} />);
  expect(screen.getAllByText("Waitlist position 1")).toHaveLength(2);
  expect(screen.getByText("Waitlist position 2")).toBeInTheDocument();
  const note = screen.getByText("Care notes: First note");
  const row = note.closest("li");

  if (!row) throw new Error("missing roster row");
  const signer = within(row).getByText(/Signed up by/);
  expect(signer.nextElementSibling).toHaveTextContent("Waitlist position 1");
  expect(row.textContent?.indexOf("Waitlist position 1")).toBeLessThan(row.textContent?.indexOf("Care notes") ?? 0);
});

it.each([null, { id: 10, status: "active", accountType: "teen" }, { id: 10, status: "disabled", accountType: "adult" }] as const)("offers no request to an ineligible actor %j", (user) => {
  actor.user = user;
  render(<ManageRoster instance={occurrence({ attendees: [{ registrationId: 92, userId: 3, name: "Hidden Pat", status: "going", attendanceMode: "in_person", guestCount: 0 }] })} onChanged={() => {}} onClose={() => {}} />);
  expect(screen.queryByRole("button", { name: "Request direct contact" })).not.toBeInTheDocument();
});

it("offers contact compose only for another real hidden account, not household people, missing IDs, self or visible contacts", () => {
  actor.user = { id: 10, status: "active", accountType: "adult" };
  const base: Attendee = { registrationId: 92, userId: 3, name: "Hidden Pat", status: "going", attendanceMode: "in_person", guestCount: 0 };
  render(<ManageRoster instance={occurrence({ attendees: [base, { ...base, registrationId: 93, userId: 10, name: "Self" }, { ...base, registrationId: 94, userId: 4, name: "Visible Sam", email: "sam@example.com", phone: "" }, { ...base, registrationId: 95, userId: undefined, name: "No account ID" }, { ...child, status: "going" }] })} onChanged={() => {}} onClose={() => {}} />);
  expect(screen.getAllByRole("button", { name: "Request direct contact" })).toHaveLength(1);
  expect(within(screen.getByText("Hidden Pat").closest("li")!).getByRole("button", { name: "Request direct contact" })).toBeInTheDocument();
});

it("adds an opaque adult selection and retains email as an alternative", async () => {
  actor.user = { id: 10, status: "active", accountType: "adult" };
  vi.mocked(People.search).mockResolvedValue({ state: "results", people: [{ selectionId: "opaque", displayName: "Pat", teen: false, sharedGroups: [], otherGroups: [] }] });
  const changed = vi.fn();
  render(<ManageRoster instance={occurrence({})} onChanged={changed} onClose={() => {}} />);
  await userEvent.type(screen.getByRole("combobox"), "Pat");
  await userEvent.click(await screen.findByRole("option"));
  await userEvent.click(screen.getByRole("button", { name: "Add" }));
  expect(Events.addRegistration).toHaveBeenCalledWith(7, "2026-12-01", { selectionId: "opaque" }, undefined);
  await userEvent.type(screen.getByLabelText("Add someone"), "known@example.com");
  await userEvent.click(screen.getByRole("button", { name: "Add" }));
  expect(Events.addRegistration).toHaveBeenLastCalledWith(7, "2026-12-01", "known@example.com", undefined);
  expect(changed).toHaveBeenCalledTimes(2);
});

it("offers a teen Ask instead of direct add, and no search after role loss", async () => {
  actor.user = { id: 10, status: "active", accountType: "adult" };
  vi.mocked(People.search).mockResolvedValue({ state: "results", people: [{ selectionId: "teen", displayName: "Teen Pat", teen: true, sharedGroups: [], otherGroups: [] }] });
  const view = render(<ManageRoster instance={occurrence({})} onChanged={() => {}} onClose={() => {}} />);
  await userEvent.type(screen.getByRole("combobox"), "Pat"); await userEvent.click(await screen.findByRole("option"));
  expect(screen.getByRole("status")).toHaveTextContent("Adding a teen needs their approval");
  expect(screen.getByRole("button", { name: "Ask to add" })).toBeEnabled();
  await userEvent.click(screen.getByRole("button", { name: "Ask to add" }));
  expect(screen.getByRole("button", { name: "Send ask to add" })).toBeInTheDocument();
  expect(Events.addRegistration).not.toHaveBeenCalled();
  actor.user = { id: 10, status: "active", accountType: "teen" };
  view.rerender(<ManageRoster instance={occurrence({})} onChanged={() => {}} onClose={() => {}} />);
  expect(screen.queryByRole("combobox")).not.toBeInTheDocument();
});

it.each(["childcare_full", "people_selection_unavailable", "forbidden", "teen_add_approval_required"])("does not bypass %s through an over-capacity retry", async (code) => {
  vi.mocked(Events.addRegistration).mockRejectedValue(new ApiError(409, "private text", { code }));
  render(<ManageRoster instance={occurrence({})} onChanged={() => {}} onClose={() => {}} />);
  await userEvent.type(screen.getByLabelText("Add someone"), "known@example.com"); await userEvent.click(screen.getByRole("button", { name: "Add" }));
  expect(screen.getByRole("alert")).not.toHaveTextContent("private text");
  expect(Events.addRegistration).toHaveBeenCalledTimes(1); expect(window.confirm).not.toHaveBeenCalled();
});

it("preserves explicit over-capacity confirmation and handles seats filled during add", async () => {
  vi.mocked(Events.addRegistration).mockRejectedValueOnce(new ApiError(409, "full", { code: "event_full" })).mockResolvedValueOnce({});
  render(<ManageRoster instance={occurrence({})} onChanged={() => {}} onClose={() => {}} />);
  await userEvent.type(screen.getByLabelText("Add someone"), "known@example.com"); await userEvent.click(screen.getByRole("button", { name: "Add" }));
  expect(window.confirm).toHaveBeenCalledWith("This date filled up. Add them over capacity?");
  expect(Events.addRegistration).toHaveBeenLastCalledWith(7, "2026-12-01", "known@example.com", true);
});

it("offers an explicit adult ask only after approval-needed, preserving the selected token and confirmed override", async () => {
  actor.user = { id: 10, status: "active", accountType: "adult" };
  vi.mocked(People.search).mockResolvedValue({ state: "results", people: [{ selectionId: "adult-opaque", displayName: "Pat", teen: false, sharedGroups: [], otherGroups: [] }] });
  vi.mocked(Events.addRegistration).mockRejectedValue(new ApiError(409, "SECRET", { code: "date_add_approval_required" }));
  render(<ManageRoster instance={occurrence({ capacity: 1, spotsLeft: 0 })} onChanged={() => {}} onClose={() => {}} />);
  await userEvent.type(screen.getByRole("combobox"), "Pat"); await userEvent.click(await screen.findByRole("option"));
  await userEvent.click(screen.getByRole("button", { name: "Add" }));
  expect(screen.queryByRole("button", { name: "Send ask to add" })).not.toBeInTheDocument();
  expect(Events.addRegistration).toHaveBeenCalledWith(7, "2026-12-01", { selectionId: "adult-opaque" }, true);
  await userEvent.click(screen.getByRole("button", { name: "Ask to add" }));
  expect(screen.getByRole("checkbox", { name: /Allow adding over capacity/ })).toBeChecked();
  expect(screen.getByRole("button", { name: "Send ask to add" })).toBeInTheDocument();
  expect(Events.addRegistration).toHaveBeenCalledTimes(1);
});

it("asks email approval refusals to use name selection, without reconstructing a token", async () => {
  vi.mocked(Events.addRegistration).mockRejectedValue(new ApiError(409, "SECRET", { code: "date_add_approval_required" }));
  render(<ManageRoster instance={occurrence({})} onChanged={() => {}} onClose={() => {}} />);
  await userEvent.type(screen.getByLabelText("Add someone"), "known@example.com"); await userEvent.click(screen.getByRole("button", { name: "Add" }));
  expect(screen.getByRole("alert")).toHaveTextContent("Select this person by name");
  expect(screen.getByLabelText("Add someone")).toHaveValue("known@example.com");
  expect(screen.queryByRole("button", { name: "Ask to add" })).not.toBeInTheDocument();
});

it("labels manager provenance only when the server explicitly supplies addedByLeader true", () => {
  const base: Attendee = { registrationId: 1, userId: 3, name: "Invited", status: "going", guestCount: 0, attendanceMode: "in_person" };
  const inferred = { ...base, registrationId: 2, name: "No inference", source: "manager_add" };
  render(<ManageRoster instance={occurrence({ attendees: [{ ...base, addedByLeader: true }, inferred, { ...base, registrationId: 3, name: "False", addedByLeader: false }] })} onChanged={() => {}} onClose={() => {}} />);
  expect(screen.getAllByText("Added by a leader")).toHaveLength(1);
  expect(within(screen.getByText("Invited").closest("li")!).getByText("Added by a leader")).toBeInTheDocument();
});

// A gate as a fail-closed host passes it (lib/dateWrites), over a test store. Its begin and end are mocks, so the
// log puts them in call order beside the requests the roster sends (vitest numbers every mock call).
const KEY = "7:2026-12-01";

interface WriteLog { begin: ReturnType<typeof vi.fn<() => void>>; end: ReturnType<typeof vi.fn<(end: RosterWriteEnd) => void>> }

function GatedRoster({ store, spies, instance, onRecheck }: { store: RosterWrites; spies: WriteLog; instance: EventOccurrence; onRecheck: (why: RosterRecheck) => void }) {
  const writes = useRosterWrites(7, "2026-12-01");

  const gate: RosterGate = { writing: writes.writing, begin: () => {
    spies.begin();
    const release = store.begin(KEY);

    return (end: RosterWriteEnd) => { spies.end(end); release(end); };
  } };

  return <ManageRoster instance={instance} gate={gate} onRecheck={onRecheck} onClose={() => {}} />;
}

const REQUESTS = ["removeRegistration", "promoteRegistration", "restoreRegistration", "addRegistration"] as const;

function logOf(spies: WriteLog): string[] {
  const calls: Array<[number, string]> = [
    ...spies.begin.mock.invocationCallOrder.map((n): [number, string] => [n, "begin"]),
    ...spies.end.mock.invocationCallOrder.map((n, i): [number, string] => [n, `end:${spies.end.mock.calls[i][0]}`]),
    ...REQUESTS.flatMap((name) => vi.mocked(Events[name]).mock.invocationCallOrder.map((n): [number, string] => [n, `request:${name}`])),
  ];

  return calls.sort((a, b) => a[0] - b[0]).map(([, entry]) => entry);
}

const sam: Attendee = { registrationId: 92, userId: 3, name: "Sam", status: "going", attendanceMode: "in_person", guestCount: 0 };
const rosterWith = (over: Partial<EventOccurrence> = {}) => occurrence({ attendees: [child, sam], removedRegistrations: [{ ...sam, registrationId: 95, userId: 5, name: "Lou", status: "removed" }], ...over });

function renderGated(instance = rosterWith(), store = createRosterWrites()) {
  const spies: WriteLog = { begin: vi.fn<() => void>(), end: vi.fn<(end: RosterWriteEnd) => void>() };
  const onRecheck = vi.fn<(why: RosterRecheck) => void>();

  for (const name of REQUESTS) vi.mocked(Events[name]).mockClear();
  const view = render(<DateWritesProvider rosterWrites={store}><GatedRoster store={store} spies={spies} instance={instance} onRecheck={onRecheck} /></DateWritesProvider>);

  return { log: () => logOf(spies), onRecheck, store, view };
}

type Write = "remove" | "promote" | "restore" | "add";

async function startWrite(write: Write) {
  if (write === "remove") {
    await userEvent.click(screen.getByRole("button", { name: "More for Sam" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Remove Sam" }));
  } else if (write === "promote") await userEvent.click(screen.getByRole("button", { name: "Promote" }));
  else if (write === "restore") await userEvent.click(screen.getByRole("button", { name: "Add back Lou" }));
  else {
    await userEvent.type(screen.getByLabelText("Add someone"), "known@example.com");
    await userEvent.click(screen.getByRole("button", { name: "Add" }));
  }
}

const requestOf: Record<Write, "removeRegistration" | "promoteRegistration" | "restoreRegistration" | "addRegistration"> = { remove: "removeRegistration", promote: "promoteRegistration", restore: "restoreRegistration", add: "addRegistration" };

it("with a gate, a window focus and a care boundary ask the host to recheck, saying why", () => {
  vi.useFakeTimers();

  try {
    const soon = new Date(Date.now() + 60_000).toISOString();
    const { onRecheck } = renderGated(rosterWith({ attendees: [{ ...child, householdPerson: { ...child.householdPerson!, careNotes: "Note", careNotesVisibleUntil: soon } }] }));
    window.dispatchEvent(new Event("focus"));
    expect(onRecheck).toHaveBeenLastCalledWith("focus");
    vi.advanceTimersByTime(61_000);
    expect(onRecheck).toHaveBeenLastCalledWith("care");
    expect(onRecheck).toHaveBeenCalledTimes(2);
  } finally { vi.useRealTimers(); }
});

it.each<Write>(["remove", "promote", "restore", "add"])("with a gate, %s begins one write just before its request and ends it stored, calling nothing else", async (write) => {
  const { log, onRecheck } = renderGated();
  await startWrite(write);
  expect(log()).toEqual(["begin", `request:${requestOf[write]}`, "end:stored"]);
  expect(onRecheck).not.toHaveBeenCalled();
});

it.each<[string, string, unknown]>([
  ["401", "lost", new ApiError(401, "x")], ["403", "lost", new ApiError(403, "x")], ["404", "lost", new ApiError(404, "x")],
  ["a network failure", "unsure", new TypeError("Failed to fetch")], ["500", "unsure", new ApiError(500, "x")], ["an unreadable body", "unsure", new ApiError(409, "x", {}, true)],
  ["400", "answered", new ApiError(400, "x")],
])("with a gate, a write failing with %s ends %s and keeps the roster's own message", async (_, end, cause) => {
  for (const write of ["remove", "promote", "restore", "add"] as const) {
    vi.mocked(Events[requestOf[write]]).mockRejectedValue(cause);
    const { log, onRecheck, view } = renderGated();
    await startWrite(write);
    expect(log()).toEqual(["begin", `request:${requestOf[write]}`, `end:${end}`]);
    expect(screen.getByRole("alert")).toBeInTheDocument();
    expect(onRecheck).not.toHaveBeenCalled();
    view.unmount();
    vi.mocked(Events[requestOf[write]]).mockReset();
  }
});

it("with a gate, a declined over-capacity retry ends answered with the roster's message, and a confirmed one is one write over two requests", async () => {
  vi.mocked(window.confirm).mockReturnValue(false);
  vi.mocked(Events.restoreRegistration).mockRejectedValue(new ApiError(409, "full", { code: "event_full" }));
  let run = renderGated();
  await startWrite("restore");
  expect(run.log()).toEqual(["begin", "request:restoreRegistration", "end:answered"]);
  expect(screen.getByRole("alert")).toBeInTheDocument();
  run.view.unmount();

  vi.mocked(Events.addRegistration).mockRejectedValue(new ApiError(409, "full", { code: "event_full" }));
  run = renderGated();
  await startWrite("add");
  expect(run.log()).toEqual(["begin", "request:addRegistration", "end:answered"]);
  run.view.unmount();

  vi.mocked(window.confirm).mockReturnValue(true);
  vi.mocked(Events.restoreRegistration).mockReset().mockRejectedValueOnce(new ApiError(409, "full", { code: "event_full" })).mockResolvedValueOnce({});
  run = renderGated();
  await startWrite("restore");
  expect(run.log()).toEqual(["begin", "request:restoreRegistration", "request:restoreRegistration", "end:stored"]);
});

it("with a gate, an add that needs the person's approval ends answered and keeps Ask to add", async () => {
  actor.user = { id: 10, status: "active", accountType: "adult" };
  vi.mocked(People.search).mockResolvedValue({ state: "results", people: [{ selectionId: "adult-opaque", displayName: "Pat", teen: false, sharedGroups: [], otherGroups: [] }] });
  vi.mocked(Events.addRegistration).mockRejectedValue(new ApiError(409, "SECRET", { code: "date_add_approval_required" }));
  const { log } = renderGated();
  await userEvent.type(screen.getByRole("combobox", { name: "Find a person by name" }), "Pat"); await userEvent.click(await screen.findByRole("option", { name: /Pat/ }));
  await userEvent.click(screen.getByRole("button", { name: "Add" }));
  expect(log()).toEqual(["begin", "request:addRegistration", "end:answered"]);
  expect(screen.getByRole("button", { name: "Ask to add" })).toBeEnabled();
});

it("begins nothing for a declined question, a stale question or a restore with no subject", async () => {
  vi.mocked(window.confirm).mockReturnValue(false);
  let run = renderGated();
  await startWrite("remove");
  expect(run.log()).toEqual([]);
  run.view.unmount();

  vi.mocked(window.confirm).mockReturnValue(true);
  run = renderGated(rosterWith({ removedRegistrations: [{ registrationId: 96, name: "Nobody", status: "removed", guestCount: 0, attendanceMode: "in_person" }] }));
  await userEvent.click(screen.getByRole("button", { name: "Add back Nobody" }));
  expect(run.log()).toEqual([]);
  run.view.unmount();

  // Inside the app's dialog, a question the page's facts moved past answers no.
  const store = createRosterWrites();
  const spies: WriteLog = { begin: vi.fn<() => void>(), end: vi.fn<(end: RosterWriteEnd) => void>() };

  for (const name of REQUESTS) vi.mocked(Events[name]).mockClear();

  function Facts({ value }: { value: string }) { usePageFacts(value);

 return null; }

  const ui = (facts: string) => <MemoryRouter><ConfirmProvider><Facts value={facts} /><DateWritesProvider rosterWrites={store}><GatedRoster store={store} spies={spies} instance={rosterWith()} onRecheck={() => {}} /></DateWritesProvider></ConfirmProvider></MemoryRouter>;
  const view = render(ui("first"));
  await userEvent.click(screen.getByRole("button", { name: "More for Sam" }));
  await userEvent.click(screen.getByRole("menuitem", { name: "Remove Sam" }));
  expect(screen.getByRole("dialog")).toBeInTheDocument();
  view.rerender(ui("second"));
  await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  expect(logOf(spies)).toEqual([]);
});

it("disables every write control while any roster write on the date is in flight, and a click sends nothing", async () => {
  actor.user = { id: 10, status: "active", accountType: "adult" };
  const store = createRosterWrites();
  let release: (end: RosterWriteEnd) => void = () => {};

  act(() => { release = store.begin(KEY); });
  const { log } = renderGated(rosterWith(), store);
  expect(screen.getByRole("button", { name: "Promote" })).toBeDisabled();
  expect(screen.getByRole("button", { name: "Add back Lou" })).toBeDisabled();
  expect(screen.getByLabelText("Add someone")).toBeDisabled();
  expect(screen.getByRole("combobox", { name: "Find a person by name" })).toBeDisabled();
  expect(screen.getByRole("button", { name: "Add" })).toBeDisabled();
  await userEvent.click(screen.getByRole("button", { name: "More for Sam" }));
  expect(screen.getByRole("menuitem", { name: "Remove Sam" })).toBeDisabled();
  await userEvent.click(screen.getByRole("menuitem", { name: "Remove Sam" }));
  await userEvent.click(screen.getByRole("button", { name: "Promote" }));
  expect(log()).toEqual([]);
  act(() => release("answered"));
  expect(screen.getByRole("button", { name: "Promote" })).toBeEnabled();
});

it("without a gate, rechecks and a success call onChanged as before, failures only set the error, and writes still register", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });

  try {
    const store = createRosterWrites();
    const heard: string[] = [];
    store.subscribe((e) => heard.push(e.end ?? "begin"));
    const onChanged = vi.fn();
    const soon = new Date(Date.now() + 60_000).toISOString();
    const instance = rosterWith({ attendees: [{ ...child, householdPerson: { ...child.householdPerson!, careNotes: "Note", careNotesVisibleUntil: soon } }, sam] });
    render(<DateWritesProvider rosterWrites={store}><ManageRoster instance={instance} onChanged={onChanged} onClose={() => {}} /></DateWritesProvider>);
    act(() => { window.dispatchEvent(new Event("focus")); });
    expect(onChanged).toHaveBeenCalledTimes(1);
    act(() => { vi.advanceTimersByTime(61_000); });
    expect(onChanged).toHaveBeenCalledTimes(2);
    await userEvent.click(screen.getByRole("button", { name: "Promote" }));
    expect(onChanged).toHaveBeenCalledTimes(3);
    vi.mocked(Events.promoteRegistration).mockRejectedValue(new ApiError(500, "x"));
    await userEvent.click(screen.getByRole("button", { name: "Promote" }));
    expect(onChanged).toHaveBeenCalledTimes(3);
    expect(screen.getByRole("alert")).toBeInTheDocument();
    expect(heard).toEqual(["begin", "stored", "begin", "unsure"]);
  } finally { vi.useRealTimers(); }
});

it("labels a row kept after leaving only when the flag is true", () => {
  render(<ManageRoster instance={occurrence({ attendees: [
    { registrationId: 92, userId: 3, name: "Sam", status: "going", attendanceMode: "in_person", guestCount: 0, keptAfterLeaving: true },
    { registrationId: 93, userId: 4, name: "Ana", status: "going", attendanceMode: "in_person", guestCount: 0, keptAfterLeaving: false },
    { registrationId: 94, userId: 6, name: "Lee", status: "going", attendanceMode: "in_person", guestCount: 0 },
  ] })} onChanged={() => {}} onClose={() => {}} />);
  const label = screen.getByText("Kept after leaving");
  expect(label.closest("li")).toHaveTextContent("Sam");
  expect(label).toHaveClass("text-text-muted");
});

it("a kept row's Remove says it ends what they kept; other rows keep today's copy", async () => {
  render(<ManageRoster instance={occurrence({ attendees: [
    { registrationId: 92, userId: 3, name: "Sam", status: "going", attendanceMode: "in_person", guestCount: 0, keptAfterLeaving: true },
    { registrationId: 93, userId: 4, name: "Ana", status: "going", attendanceMode: "in_person", guestCount: 0 },
  ] })} onChanged={() => {}} onClose={() => {}} />);
  await userEvent.click(screen.getByRole("button", { name: "More for Sam" }));
  await userEvent.click(screen.getByRole("menuitem", { name: "Remove Sam" }));
  const kept = vi.mocked(window.confirm).mock.lastCall?.[0] ?? "";
  expect(kept).toBe("Remove Sam's registration for this date? They left the group and kept this date. Removing them ends that and tells them. They won't be able to re-register on their own.");
  expect(kept).not.toContain("frees their place");
  await userEvent.click(screen.getByRole("button", { name: "More for Ana" }));
  await userEvent.click(screen.getByRole("menuitem", { name: "Remove Ana" }));
  expect(vi.mocked(window.confirm).mock.lastCall?.[0]).toBe("Remove Ana's registration for this date? They won't be able to re-register on their own.");
});

it("a kept row's Remove asks in the app dialog with the kept copy as its body", async () => {
  render(<MemoryRouter><ConfirmProvider><ManageRoster instance={occurrence({ attendees: [{ registrationId: 92, userId: 3, name: "Sam", status: "going", attendanceMode: "in_person", guestCount: 0, keptAfterLeaving: true }] })} onChanged={() => {}} onClose={() => {}} /></ConfirmProvider></MemoryRouter>);
  await userEvent.click(screen.getByRole("button", { name: "More for Sam" }));
  await userEvent.click(screen.getByRole("menuitem", { name: "Remove Sam" }));
  const dialog = await screen.findByRole("dialog", { name: "Remove Sam's registration for this date?" });
  expect(dialog).toHaveTextContent("They left the group and kept this date. Removing them ends that and tells them. They won't be able to re-register on their own.");
  expect(dialog).not.toHaveTextContent("frees their place");
});
