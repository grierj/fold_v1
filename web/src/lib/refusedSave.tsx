import { createContext, useCallback, useContext, useEffect, useLayoutEffect, useRef, useState } from "react";
import { ApiError } from "../api/core";
import type { Load } from "./useLoad";
import type { RevisionConflict } from "./useRevisionSave";

// A save was refused and its editor closed on its own (DEC-161, DEC-163): the thing changed (another person, another
// tab or this person's own action), or this person may no longer make the save. The page reads from nothing and, once
// that read answers (even when it finds nothing), says the changes weren't saved, until `clear` (the editor opens
// again). No live region outlasts a read from nothing, so the line takes focus, which reads it out and brings it into
// view whatever else the page scrolled to. Focus follows the reason, not the line: a later read that shows the line
// again does not move focus.
//
// A control that stays on screen (a room control) passes no `load`: it already shows what its own read found when it
// calls `refused`, so the line comes at once.
//
// What was refused is a save, or a send whose form was saved before it (the message composer's confirmation): the line
// then says the message was not sent, never that changes could not be saved.
export function useRefusedSave(load?: Pick<Load<unknown>, "loading" | "data" | "retry">) {
  const pending = useRef<Refusal | null>(null);
  const [refusal, setRefusal] = useState<Refusal | null>(null);
  const line = useRef<HTMLParagraphElement>(null);
  useEffect(() => {
    if (!pending.current || load?.loading) return;
    setRefusal(pending.current);
    pending.current = null;
  }, [load?.loading, load?.data]);
  useLayoutEffect(() => {
    if (refusal) line.current?.focus();
  }, [refusal]);

  const retry = load?.retry;

  const refused = useCallback((why: RevisionConflict, what: Refused = "save") => {
    if (!retry) {
      setRefusal({ why, what });

      return;
    }

    pending.current = { why, what };
    void retry();
  }, [retry]);

  const clear = useCallback(() => {
    pending.current = null;
    setRefusal(null);
  }, []);

  return { why: refusal?.why ?? null, what: refusal?.what ?? "save", line, refused, clear };
}

type Refused = "save" | "send" | "action" | "role" | "unconfirmed";

// Use for a request about the page's resource. Feature-specific refusals such as room_gone or rooms_off stay with the control.
export function resourceRefused(cause: unknown): boolean {
  return cause instanceof ApiError && (
    (cause.status === 403 && (cause.code === "not_allowed" || cause.code === "add_request_unavailable")) ||
    (cause.status === 404 && (cause.code === "not_found" || cause.code === "invalid"))
  );
}

export type ResourceReadOutcome = "loaded" | "refused";

type Refresh = (intent: "read" | "write") => void;

export const ResourceRefusal = createContext<Refresh | null>(null);

export function useResourceRefusal() {
  const refresh = useContext(ResourceRefusal);

  return useCallback((cause: unknown, intent: "read" | "write" = "write") => {
    if (!refresh || !resourceRefused(cause)) return false;
    refresh(intent);

    return true;
  }, [refresh]);
}

interface Refusal {
  why: RevisionConflict;
  what: Refused;
}

// The line for the screen the page shows: `canManage` is whether the viewer may still manage the `noun` shown; leave it
// out on a screen that shows none (not found, unavailable), where the line says nothing about it. `about` names which
// one, on a page that lists several (a date's day on the event and group pages). A control that stays on screen and
// shows what changed under a choice names it (`choosing`, as "This date's room"); its host handles "gone".
export function RefusedSaveLine({ save, noun, canManage, about, choosing }: { save: ReturnType<typeof useRefusedSave>; noun: string; canManage?: boolean; about?: string; choosing?: string }) {
  if (!save.why) return null;

  const text = choosing
    ? `${choiceRefused(choosing)} This is the ${noun} as it is now.`
    : save.what === "unconfirmed"
      ? "We couldn't confirm your changes."
    : save.what === "role"
      ? "We couldn't confirm the role change."
      : save.what === "action"
      ? "Your last action could not be completed."
      : save.what === "send"
      ? sendRefused(save.why, noun, canManage)
      : canManage === undefined
        ? "Your changes could not be saved."
        : save.why === "changed"
          ? `This ${noun} changed while you were editing, so your changes could not be saved. This is the ${noun} as it is now.`
          : canManage
            ? `Your changes could not be saved. This is the ${noun} as it is now.`
            : `You can no longer manage this ${noun}, so your changes could not be saved.`;

  return (
    <p ref={save.line} role="status" tabIndex={-1} className="mb-4 scroll-mt-4 rounded-lg border border-border bg-sunken px-4 py-3 text-sm outline-none">
      {about ? `${about}. ${text}` : text}
    </p>
  );
}

// A send refused after its form was saved: nothing the person typed was lost, so the line says only that it did not go.
function sendRefused(why: RevisionConflict, noun: string, canManage: boolean | undefined): string {
  if (canManage === undefined) return `This ${noun} could not be sent.`;

  if (why === "changed") return `This ${noun} changed somewhere else, so it was not sent. This is the ${noun} as it is now.`;

  return canManage ? `This ${noun} could not be sent. This is the ${noun} as it is now.` : `You can no longer manage this ${noun}, so it was not sent.`;
}

// What a control that stays on screen says when someone else's change refused a choice (`choosing`, as "This date's
// room"): the one wording, for a control still showing what is stored and for a list whose row went with the control.
export function choiceRefused(choosing: string): string {
  return `${choosing} changed while you were choosing, so your choice could not be saved.`;
}
