import { act, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, vi } from "vitest";
import { ApiError } from "../api/core";
import { Events } from "../api/events";
import { formatDayFull } from "../lib/when";
import { occurrence } from "./occurrence";
import type { Attendee, EventOccurrence, OccurrenceResponse } from "../types";
import type { JsonValue } from "../lib/wireValues";
import type { EditSends } from "./guardedEdits";

// The kept-dates panel's own lines (KeptDatesPanel.tsx), as a person reads them.
export const KEPT = {
  checking: "Your change isn't saved yet. Checking that you can still manage sign-ups…",
  unknown: "Your change isn't saved yet. We couldn't check that you can still manage sign-ups. What you changed is still in the form.",
  refused: "Your change isn't saved. You can't manage sign-ups for this event now.",
  nobodyOne: "Nobody is keeping this date now. Save again to apply your change.",
  nobodyHere: "Nobody is keeping this date now.",
  saveLine: "Your change wasn't saved: when you saved, someone who left the group had kept at least one of these dates. Remove anyone marked “Kept after leaving” under each date, then save again. Saving checks every date again.",
  writeNote: "Your last change may not have gone through. Here's the list as it is now.",
  home: "Sign-ups to check before you save",
};

// A kept count rendered as a number.
export const KEPT_COUNT = /\d+ (person|people) kept/;

export const keptPerson = (name: string, over: Partial<Attendee> = {}): Attendee => ({ registrationId: 31, userId: 5, name, status: "going", attendanceMode: "in_person", guestCount: 0, email: `${name.toLowerCase().replace(" ", ".")}@example.com`, keptAfterLeaving: true, ...over });

// A household person with the care note W4b forbids while a read is pending.
export const careKid = (careNotesVisibleUntil?: string): Attendee => ({ registrationId: 40, name: "Kit", status: "going", attendanceMode: "in_person", guestCount: 0, householdPerson: { personId: 3, name: "Kit", kind: "child", age: 6, householdName: "Holder household", registeredBy: { userId: 7, name: "Holder" }, careNotes: "Old care note", careNotesVisibleUntil } });

export const readFailures = {
  403: () => new ApiError(403, "SECRET", { code: "forbidden" }),
  404: () => new ApiError(404, "SECRET", { code: "not_found" }),
  network: () => new TypeError("Failed to fetch"),
  500: () => new ApiError(500, "SECRET"),
};

export type ReadOutcome = "success" | keyof typeof readFailures;

export const READ_OUTCOMES: ReadOutcome[] = ["success", 403, 404, "network", 500];

export function deferred<T>() {
  let resolve: (value: T) => void = () => {};
  let reject: (cause: unknown) => void = () => {};
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });

  return { promise, resolve, reject };
}

const FORBIDDEN_WORDS = ["Old Name", "Old care note", "Someone who left the group kept this date", "when you saved, someone who left", "kept this date", "Nobody is keeping", "Late A"];

const forbiddenWords = (text: string) => FORBIDDEN_WORDS.filter((words) => text.includes(words));

// What W4b forbids while the panel's read is pending, refused or unsure, for the panel's refused `dates`.
export function keptForbiddenNow(dates: string[]): string[] {
  const headings = dates.map(formatDayFull);
  const found = forbiddenWords(document.body.textContent ?? "");

  if (document.querySelector('a[href^="mailto:old"]')) found.push("contact control");

  if ([...document.querySelectorAll("body *")].some((el) => el.children.length === 0 && el.textContent === "Kept after leaving")) found.push("label");
  const panel = document.querySelector(`[aria-label="${KEPT.home}"]`);

  if (panel && [...panel.querySelectorAll("h1, h2, h3, h4, h5, h6")].some((h) => headings.includes(h.textContent ?? ""))) found.push("date heading");

  if (panel && [...panel.querySelectorAll("button")].some((b) => b.textContent === "Manage sign-ups")) found.push("Manage sign-ups");

  if (document.querySelector('[role="dialog"]')) found.push("dialog");

  return found;
}

// Every DOM state from now until stop(): what it showed, what it showed of the forbidden set, and where focus was.
export function observeKept(dates: string[]) {
  const states: Array<{ forbidden: string[]; text: string; active: Element | null }> = [];
  const record = () => states.push({ forbidden: keptForbiddenNow(dates), text: document.body.textContent ?? "", active: document.activeElement });

  // A commit replaced by another in the same act never reaches a callback as a whole page, so the nodes it added
  // are read from the records too.
  const observer = new MutationObserver((records) => {
    const passed = records.flatMap((r) => [...r.addedNodes].map((n) => n.textContent ?? "")).join("\n");
    const forbidden = forbiddenWords(passed);

    if (forbidden.length > 0) states.push({ forbidden, text: passed, active: document.activeElement });
    record();
  });

  observer.observe(document.body, { subtree: true, childList: true, characterData: true, attributes: true });

  return {
    states,
    stop: () => { record(); observer.disconnect(); },
    expectClean: (also: string[] = []) => {
      for (const s of states) {
        expect(s.forbidden).toEqual([]);

        for (const words of also) expect(s.text).not.toContain(words);
      }
    },
  };
}

// A 409 date_kept_after_leaving; `keptDates` is left out when undefined, as an unreadable body would be.
export const kept409 = (keptDates?: JsonValue) => new ApiError(409, "SECRET", keptDates === undefined ? { code: "date_kept_after_leaving" } : { code: "date_kept_after_leaving", keptDates });

// A manager's read of `date` with W4b's fixture: a kept row with a contact control, and a care note. It carries
// the date's edit token, so a host that shares its managed read with the editor (ManageDate) can edit from it.
export const keptRead = (date: string, over: Partial<EventOccurrence> = {}): OccurrenceResponse => ({ occurrence: occurrence({ date, revision: "r0", attendeesVisible: true, keptCount: 1, attendees: [keptPerson("Old Name"), careKid()], ...over }) });

// W6's events on a host: W4b's (i), (ii), (iii-a) and (iv), and W4i's lost Remove.
export type HostEvent = "i" | "ii" | "iii-a" | "iv" | "lost-401" | "lost-403" | "lost-404";

export const HOST_EVENTS: HostEvent[] = ["i", "ii", "iii-a", "iv", "lost-401", "lost-403", "lost-404"];

interface Host {
  // Opens the host's date editor, types Minutes 150 and presses Save.
  openAndSave: () => Promise<void>;
  // How many times the host page's own load ran.
  pageLoads: () => number;
  date?: string;
  // The guarded edits' transport, whose first answer is the kept 409.
  edits: EditSends;
  // What the editor's own (pinned) manager read answers; the panel's reads are never pinned.
  base: () => OccurrenceResponse;
  // The host reads the date again for its editor after each answer of its page (EventDetail, GroupDetail), so a
  // write that asks for the page remounts the editor and its panel, which reads once more.
  remounts?: boolean;
}

// The panel's reads, apart from the editor's own.
const panelReads = () => vi.mocked(Events.managedDate).mock.calls.filter((call) => call[2] === undefined).length;

async function openRemove() {
  await userEvent.click(screen.getByRole("button", { name: "More for Old Name" }));
  await userEvent.click(screen.getByRole("menuitem", { name: "Remove Old Name" }));
  await screen.findByRole("dialog", { name: "Remove Old Name's registration for this date?" });
}

// One W6 case: a 409 opens the panel on the host, the event fires with the next read held, and the read
// settles with `outcome`. The editor's draft and the host's own load are checked after every step: a recheck
// leaves the host's load alone, and a Remove that ended stored or lost asks the host again exactly once.
export async function runHostCase(event: HostEvent, outcome: ReadOutcome, host: Host) {
  const date = host.date ?? "2026-12-01";
  const minutes = () => screen.getByRole("spinbutton", { name: "Minutes" });

  if (event === "ii") vi.useFakeTimers({ shouldAdvanceTime: true });
  const until = event === "ii" ? new Date(Date.now() + 60_000).toISOString() : undefined;
  host.edits.answer(kept409([{ date, keptCount: 1 }]));
  const reads: Array<Promise<OccurrenceResponse>> = [Promise.resolve(event === "iii-a" ? keptRead(date, { keptCount: 0, attendees: [] }) : keptRead(date, { attendees: [keptPerson("Old Name"), careKid(until)] }))];
  vi.mocked(Events.managedDate).mockImplementation((_id, _date, pinned) => (pinned ? Promise.resolve(host.base()) : reads.shift() ?? Promise.reject(new Error("no panel read expected"))));
  await host.openAndSave();
  const loads = host.pageLoads();
  await screen.findByText(event === "iii-a" ? KEPT.nobodyOne : "Old Name");

  if (event !== "iii-a") {
    expect(screen.getByText("Care notes: Old care note")).toBeInTheDocument();
    expect(document.querySelector('a[href^="mailto:old"]')).not.toBeNull();
  }

  let fire: () => Promise<void> | void = () => act(() => { window.dispatchEvent(new Event("focus")); });

  if (event === "ii") fire = () => act(() => { vi.advanceTimersByTime(61_000); });

  if (event === "iv" || event.startsWith("lost-")) {
    const write = deferred<unknown>();
    vi.mocked(Events.removeRegistration).mockReturnValueOnce(write.promise);
    await openRemove();
    await userEvent.click(screen.getByRole("button", { name: "Remove" }));
    fire = async () => {
      await act(async () => {
        if (event === "iv") write.resolve({});
        else write.reject(new ApiError(Number(event.slice(5)), "SECRET", { code: "forbidden" }));
        await write.promise.catch(() => {});
      });
    };
  } else if (event !== "iii-a") {
    await openRemove();
  }

  const held = deferred<OccurrenceResponse>();
  const asksPage = event === "iv" || event.startsWith("lost-");
  const remount = !!host.remounts && asksPage;
  reads.push(held.promise);

  if (remount) reads.push(held.promise);
  const before = panelReads();
  const seen = observeKept([date]);
  await fire();
  const reread = asksPage ? loads + 1 : loads;
  expect(host.pageLoads()).toBe(reread);
  expect(keptForbiddenNow([date])).toEqual([]);

  if (remount) await waitFor(() => expect(panelReads()).toBe(before + 2));
  expect(await screen.findByRole("region", { name: KEPT.home })).toHaveTextContent(KEPT.checking);
  expect(panelReads()).toBe(before + (remount ? 2 : 1));
  expect(minutes()).toHaveValue(150);
  seen.stop();
  seen.expectClean();
  await act(async () => {
    if (outcome === "success") held.resolve(keptRead(date, { attendees: [keptPerson("New Name")] }));
    else held.reject(readFailures[outcome]());
    await held.promise.catch(() => {});
  });

  if (outcome === "success") {
    expect(await screen.findByText("New Name")).toBeInTheDocument();
    expect(screen.queryByText("Old Name")).not.toBeInTheDocument();
  } else if (outcome === 403 || outcome === 404) {
    await waitFor(() => expect(screen.queryByRole("region", { name: KEPT.home })).not.toBeInTheDocument());
    expect(screen.getByText(KEPT.refused)).toBeInTheDocument();
    expect(keptForbiddenNow([date])).toEqual([]);
  } else {
    expect(screen.getByRole("region", { name: KEPT.home })).toHaveTextContent(KEPT.unknown);
    expect(keptForbiddenNow([date])).toEqual([]);
  }

  expect(minutes()).toHaveValue(150);
  expect(host.pageLoads()).toBe(reread);
}
