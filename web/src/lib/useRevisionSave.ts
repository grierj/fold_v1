import { useEffect, useRef, useState } from "react";
import { ApiError, credentialCurrent, currentCredential } from "../api/core";
import { errorCode } from "./errors";
import { outcomeOf } from "./rosterWrites";
import { isNumber, isObject, isString } from "./wireValues";

// changed: what is stored differs from what the form opened with and from what the save would leave. gone: this
// person may no longer make this save.
export type RevisionConflict = "changed" | "gone";

// saved: the write landed, or a refused one found what it would leave already stored. refused: refused every time,
// with nothing the form can write changed (the form says its ordinary error). left: the form went away mid-way;
// nothing more was sent. conflict: onConflict was told.
export type RevisionSent = "saved" | "refused" | "left" | "conflict";

export interface RevisionRead<T> {
  // Whether the read still lets this person make the save.
  allowed: boolean;
  revision?: string;
  value: T;
}

// A refused save is sent again at most this many times in all, while the stored value still matches the form.
const MAX_SENDS = 3;

// The server says the person may no longer manage or see what they were editing.
export const lostAuthority = (err: ApiError) => err.status === 403 || err.status === 404;

// A send that may have been stored with its answer lost (outcomeOf's "unsure"). A form's own reason to stop, which is
// no Error (a question answered no), sent nothing.
const unanswered = (cause: unknown) => cause instanceof Error && outcomeOf(cause) === "unsure";

// What the transport throws for a body it cannot read (api/core.ts), so the form says what it always says then.
const unreadableAnswer = () => new ApiError(502, "unreadable answer", {}, true);

interface SavedThing {
  id: number;
  revision: string;
}

// A send of this draft got no answer (`current`), and what it carried that no read shows (`unseen`).
export interface LostSend {
  current: boolean;
  unseen?: string;
}

// What a save that landed answers: the thing it saved, with its new revision.
type SavedAnswer = { event: SavedThing } | { occurrence: SavedThing } | { group: SavedThing };

// A save's answer names the thing saved: `{ [key]: { id, revision } }` with this id and a revision. Anything else (a
// page from something in between, which reads as {}, or another thing's answer) is no answer.
export const answerNames = (key: "event" | "occurrence" | "group", id: number) => (answer: unknown): answer is SavedAnswer => {
  if (!isObject(answer)) return false;

  const thing = key === "event" ? "event" in answer && answer.event
    : key === "occurrence" ? "occurrence" in answer && answer.occurrence
      : "group" in answer && answer.group;

  return isObject(thing) && "id" in thing && "revision" in thing && isNumber(thing.id) && thing.id === id && isString(thing.revision) && thing.revision !== "";
};

// useRevisionSave is the one conflict protocol for a manager's save (DEC-162, DEC-163, DEC-164, DEC-166). The save names
// the read the form opened with (`revision`, kept from the first render, like `opened`). On a refusal (`changedCode`: an
// event's `event_changed`, a single date's `date_changed`, a group's `group_changed`) the value is read once and
// compared, in this order: what is stored
// is what this save would leave (`intended`: an earlier press stored it and its answer was lost), so the save is done
// and nothing more is sent; nothing the save can write has changed since the form opened (the person's own room
// booking, a restart), so the same save goes again under the new revision; anything else goes to `onConflict`, and
// the page shows what is stored.
// A save counts only when its answer names the thing saved (`answers`, usually `answerNames`); any other answer, like a
// dropped connection or a 5xx, leaves the outcome unknown and is thrown for the form's ordinary error. Once a send from
// this form got no answer, until a save lands, a refusal ends only as already stored or as a conflict: the form never
// sends again under a newer revision, which could re-apply a change someone deliberately undid after the lost send
// stored it (issue 455). `lost` keeps that for a form that may be unmounted and mounted again with the same draft.
// A save may carry what no read shows (`unseen`: the reason a visibility change is recorded and announced with). What
// is stored then counts as this save only when the send that got no answer carried the same: a reason typed since
// was never stored, so that is a conflict, not a save.
// Repeated refusals alone are never a conflict. Only the form and the sign-in that pressed Save send again: an answer
// that lands after the form went away, or after this tab took another account, sends nothing more. A 403 or 404 to
// any send or to the read says the person may no longer manage or see what they were editing: that is `onConflict`
// too ("gone"), so the page stops showing it.
//
// `fields` is everything the save can write, as the form reads it (an absent value read as the form reads it), and
// the one normaliser for the value opened, the value intended and the value read. A field left out would be
// overwritten without a word. It answers null when a read cannot show that the save is still the same decision, or
// already stored, which is a conflict. `intended` is the value as the server stores it: where the form cannot tell
// what that is, it must differ from any read, so a stored save is never claimed by mistake.
//
// Every other answer, on the first send or a later one, is thrown for the form to handle as it always has.
export function useRevisionSave<T, F>({
  opened,
  revision,
  read,
  fields,
  answers,
  lost: kept,
  onConflict,
  changedCode = "event_changed",
}: {
  opened: T;
  revision: string | undefined;
  read: () => Promise<RevisionRead<T>>;
  fields: (value: T) => F | null;
  answers: (answer: unknown) => answer is SavedAnswer;
  lost?: LostSend;
  onConflict?: (why: RevisionConflict) => void;
  changedCode?: "event_changed" | "date_changed" | "group_changed";
}) {
  const [base] = useState(() => ({ opened, revision }));
  const own = useRef<LostSend>({ current: false });
  const lost = kept ?? own.current;
  const mounted = useRef(true);

  useEffect(() => {
    mounted.current = true;

    return () => { mounted.current = false; };
  }, []);

  return async function send<R>(write: (revision: string | undefined) => Promise<R>, intended: T, unseen = ""): Promise<RevisionSent> {
    const conflict = (why: RevisionConflict): RevisionSent => {
      onConflict?.(why);

      return "conflict";
    };

    const same = (a: F | null, b: F | null) => a !== null && b !== null && JSON.stringify(a) === JSON.stringify(b);
    const was = fields(base.opened);
    const goal = fields(intended);
    const credential = currentCredential();
    const sameSignIn = () => (credential === null ? currentCredential() === null : credentialCurrent(credential));
    let current = base.revision;

    const noAnswer = () => {
      lost.current = true;
      lost.unseen = unseen;
    };

    const landed = () => {
      lost.current = false;
      lost.unseen = undefined;
    };

    for (let sends = 1; ; sends++) {
      let answer: R | undefined;
      let refused = false;

      try {
        answer = await write(current);
      } catch (err) {
        if (unanswered(err)) noAnswer();

        if (err instanceof ApiError && lostAuthority(err)) return conflict("gone");

        if (errorCode(err) !== changedCode) throw err;

        refused = true;
      }

      if (!refused) {
        if (!answers(answer)) {
          noAnswer();

          throw unreadableAnswer();
        }

        landed();

        return "saved";
      }

      if (!mounted.current) return "left";

      if (!sameSignIn()) return conflict("gone");

      let stored: RevisionRead<T>;

      try {
        stored = await read();
      } catch (err) {
        if (err instanceof ApiError && lostAuthority(err)) return conflict("gone");

        throw err;
      }

      if (!mounted.current) return "left";

      if (!sameSignIn() || !stored.allowed || !stored.revision) return conflict("gone");

      const now = fields(stored.value);

      if (same(now, goal)) {
        if (unseen !== (lost.unseen ?? "")) return conflict("changed");

        landed();

        return "saved";
      }

      if (!same(now, was) || lost.current) return conflict("changed");

      if (sends === MAX_SENDS) return "refused";

      current = stored.revision;
    }
  }
}
