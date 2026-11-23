import { act, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { vi } from "vitest";
import { ApiError } from "../api/core";
import { Events } from "../api/events";
import type { Attendee } from "../types";
import type { RosterWriteEnd, RosterWrites } from "../lib/dateWrites";
import { deferred } from "./keptPanel";

// The roster writes a manager starts on a date (ManageRoster): each holds the date for the session while its
// request is on its way (lib/dateWrites).
export type RosterOp = "add" | "restore" | "remove";

export const ROSTER_OPS: RosterOp[] = ["add", "restore", "remove"];

type Held = ReturnType<typeof deferred<unknown>>;

// How a held roster write ends, as rosterWriteFailure sorts it.
export const ROSTER_ENDS = {
  stored: (w: Held) => w.resolve({}),
  "lost-403": (w: Held) => w.reject(new ApiError(403, "SECRET", { code: "forbidden" })),
  "unsure-500": (w: Held) => w.reject(new ApiError(500, "SECRET")),
  "unsure-network": (w: Held) => w.reject(new TypeError("Failed to fetch")),
  // A readable answer that changed nothing (a Remove's event_full; an Add or Add back that asks for over
  // capacity first would hold the write open, so they are answered with a refusal).
  answered: (w: Held) => w.reject(new ApiError(400, "SECRET")),
  "event_full": (w: Held) => w.reject(new ApiError(409, "SECRET", { code: "event_full" })),
};

export type RosterEnd = keyof typeof ROSTER_ENDS;

// The ends a held op is tried with: every way a Remove can end, and a stored and an answered Add or Add back.
export function endsFor(op: RosterOp): RosterEnd[] {
  return op === "remove" ? ["stored", "lost-403", "unsure-500", "unsure-network", "event_full"] : ["stored", "answered"];
}

// A sign-up that was removed, which the roster offers to add back.
export const removedRow = (over: Partial<Attendee> = {}): Attendee => ({ registrationId: 52, userId: 9, name: "Gone Person", status: "cancelled", attendanceMode: "in_person", guestCount: 0, ...over });

// Starts `op` on the roster `scope` returns, with its request held; `name` is the sign-up a Remove removes. The
// write has begun when this returns.
export async function startHeld(op: RosterOp, scope: () => HTMLElement, name: string): Promise<Held> {
  const write = deferred<unknown>();

  if (op === "remove") {
    vi.mocked(Events.removeRegistration).mockReturnValueOnce(write.promise);
    await userEvent.click(within(scope()).getByRole("button", { name: `More for ${name}` }));
    await userEvent.click(screen.getByRole("menuitem", { name: `Remove ${name}` }));
    await userEvent.click(within(await screen.findByRole("dialog", { name: `Remove ${name}'s registration for this date?` })).getByRole("button", { name: "Remove" }));
  } else if (op === "add") {
    vi.mocked(Events.addRegistration).mockReturnValueOnce(write.promise);
    await userEvent.type(within(scope()).getByRole("textbox", { name: /^Add someone/ }), "new.person@example.com");
    await userEvent.click(within(scope()).getByRole("button", { name: "Add" }));
  } else {
    vi.mocked(Events.restoreRegistration).mockReturnValueOnce(write.promise);
    await userEvent.click(within(scope()).getByRole("button", { name: "Add back Gone Person" }));
  }

  const sent = { add: Events.addRegistration, restore: Events.restoreRegistration, remove: Events.removeRegistration }[op];

  if (vi.mocked(sent).mock.calls.length === 0) throw new Error(`the ${op} was not sent`);

  return write;
}

// Watches a store's changedSince, so a test can act straight after a component's own check of an answer. Call it
// before the component mounts: the activity hooks keep the function they were handed.
export function watchChecks(store: RosterWrites) {
  return vi.spyOn(store, "changedSince");
}

// Settles a held answer, lets the component's own check of it run, then begins and ends one write on `key` before
// React commits what the component made of the answer: the seam between accepting a review and watching it.
export async function writeAfterTheCheck(checks: ReturnType<typeof watchChecks>, settle: () => Promise<void>, store: RosterWrites, key: string, end: RosterWriteEnd) {
  await act(async () => {
    const before = checks.mock.calls.length;
    await settle();

    for (let i = 0; i < 50 && checks.mock.calls.length === before; i++) await Promise.resolve();

    if (checks.mock.calls.length === before) throw new Error("the component never checked its answer");
    store.begin(key)(end);
  });
}
