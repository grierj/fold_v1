import { useEffect, useRef, useState } from "react";
import { ApiError, credentialCurrent, currentCredential } from "../api/core";
import { errorCode } from "./errors";

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

// useRevisionSave is the one conflict protocol for a manager's save (DEC-162, DEC-163, DEC-164). The save names the read
// the form opened with (`revision`, kept from the first render, like `opened`). On a refusal (`changedCode`: an event's
// `event_changed`, a single date's `date_changed`) the value is read once and compared, in this order: what is stored
// is what this save would leave (`intended`: an earlier press stored it and its answer was lost), so the save is done
// and nothing more is sent; nothing the save can write has changed since the form opened (the person's own room
// booking, a restart), so the same save goes again under the new revision; anything else goes to `onConflict`, and
// the page shows what is stored.
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
  onConflict,
  changedCode = "event_changed",
}: {
  opened: T;
  revision: string | undefined;
  read: () => Promise<RevisionRead<T>>;
  fields: (value: T) => F | null;
  onConflict?: (why: RevisionConflict) => void;
  changedCode?: "event_changed" | "date_changed";
}) {
  const [base] = useState(() => ({ opened, revision }));
  const mounted = useRef(true);

  useEffect(() => {
    mounted.current = true;

    return () => { mounted.current = false; };
  }, []);

  return async function send<R>(write: (revision: string | undefined) => Promise<R>, intended: T): Promise<RevisionSent> {
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

    for (let sends = 1; ; sends++) {
      try {
        await write(current);

        return "saved";
      } catch (err) {
        if (err instanceof ApiError && lostAuthority(err)) return conflict("gone");

        if (errorCode(err) !== changedCode) throw err;
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

      if (same(now, goal)) return "saved";

      if (!same(now, was)) return conflict("changed");

      if (sends === MAX_SENDS) return "refused";

      current = stored.revision;
    }
  }
}
