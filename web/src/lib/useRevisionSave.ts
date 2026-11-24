import { useEffect, useRef, useState } from "react";
import { ApiError, credentialCurrent, currentCredential } from "../api/core";
import { errorCode } from "./errors";

// changed: what is stored differs from what the form opened with. gone: this person may no longer make this save.
export type RevisionConflict = "changed" | "gone";

// saved: the write landed. refused: refused every time, with nothing the form can write changed (the form says its
// ordinary error). left: the form went away mid-way; nothing more was sent. conflict: onConflict was told.
export type RevisionSent = "saved" | "refused" | "left" | "conflict";

export interface RevisionRead<T> {
  // Whether the read still lets this person make the save.
  allowed: boolean;
  revision?: string;
  value: T;
}

// A refused save is sent again at most this many times in all, while the stored value still matches the form.
const MAX_SENDS = 3;

const lostAuthority = (err: ApiError) => err.status === 403 || err.status === 404;

// useRevisionSave is the one conflict protocol for a manager's save (DEC-162, DEC-163). The save names the read the
// form opened with (`revision`, kept from the first render, like `opened`). On a refusal (`event_changed`) the value
// is read once: while nothing the save can write has changed (the person's own room booking, a restart), the same
// save goes again under the new revision; anything else goes to `onConflict`, and the page shows what is stored.
// Repeated refusals alone are never a conflict. Only the form and the sign-in that pressed Save send again: an answer
// that lands after the form went away, or after this tab took another account, sends nothing more.
//
// `fields` is everything the save can write, as the form reads it (an absent value read as the form reads it). A
// field left out would be overwritten without a word. It answers null when a read cannot show that the save is still
// the same decision, which is a conflict.
//
// Every other answer, on the first send or a later one, is thrown for the form to handle as it always has, except a
// 403 or 404 on a send this hook made by itself: a read had said the person still could, so that is "gone".
export function useRevisionSave<T, F>({
  opened,
  revision,
  read,
  fields,
  onConflict,
}: {
  opened: T;
  revision: string | undefined;
  read: () => Promise<RevisionRead<T>>;
  fields: (value: T) => F | null;
  onConflict?: (why: RevisionConflict) => void;
}) {
  const [base] = useState(() => ({ opened, revision }));
  const mounted = useRef(true);

  useEffect(() => {
    mounted.current = true;

    return () => { mounted.current = false; };
  }, []);

  return async function send<R>(write: (revision: string | undefined) => Promise<R>): Promise<RevisionSent> {
    const conflict = (why: RevisionConflict): RevisionSent => {
      onConflict?.(why);

      return "conflict";
    };

    const was = fields(base.opened);
    const credential = currentCredential();
    const sameSignIn = () => (credential === null ? currentCredential() === null : credentialCurrent(credential));
    let current = base.revision;

    for (let sends = 1; ; sends++) {
      try {
        await write(current);

        return "saved";
      } catch (err) {
        if (sends > 1 && err instanceof ApiError && lostAuthority(err)) return conflict("gone");

        if (errorCode(err) !== "event_changed") throw err;
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

      if (was === null || now === null || JSON.stringify(now) !== JSON.stringify(was)) return conflict("changed");

      if (sends === MAX_SENDS) return "refused";

      current = stored.revision;
    }
  }
}
