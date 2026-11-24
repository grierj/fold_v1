import { useEffect, useLayoutEffect, useRef, useState } from "react";
import type { Load } from "./useLoad";
import type { RevisionConflict } from "./useRevisionSave";

// A save was refused and its editor closed on its own (DEC-161, DEC-163): the thing changed (another person, another
// tab or this person's own action), or this person may no longer make the save. The page reads from nothing and, once
// that read answers (even when it finds nothing), says the changes weren't saved, until `clear` (the editor opens
// again). No live region outlasts a read from nothing, so the line takes focus, which reads it out and brings it into
// view whatever else the page scrolled to. Focus follows the reason, not the line: a later read that shows the line
// again does not move focus.
export function useRefusedSave(load: Pick<Load<unknown>, "loading" | "data" | "retry">) {
  const pending = useRef<RevisionConflict | null>(null);
  const [why, setWhy] = useState<RevisionConflict | null>(null);
  const line = useRef<HTMLParagraphElement>(null);
  useEffect(() => {
    if (!pending.current || load.loading) return;
    setWhy(pending.current);
    pending.current = null;
  }, [load.loading, load.data]);
  useLayoutEffect(() => {
    if (why) line.current?.focus();
  }, [why]);

  return {
    why,
    line,
    refused(reason: RevisionConflict) {
      pending.current = reason;
      void load.retry();
    },
    clear() {
      pending.current = null;
      setWhy(null);
    },
  };
}

// The line for the screen the page shows: `canManage` is whether the viewer may still manage the `noun` shown; leave it
// out on a screen that shows none (not found, unavailable), where the line says nothing about it.
export function RefusedSaveLine({ save, noun, canManage }: { save: ReturnType<typeof useRefusedSave>; noun: string; canManage?: boolean }) {
  if (!save.why) return null;

  const text = canManage === undefined
    ? "Your changes could not be saved."
    : save.why === "changed"
      ? `This ${noun} changed while you were editing, so your changes could not be saved. This is the ${noun} as it is now.`
      : canManage
        ? `Your changes could not be saved. This is the ${noun} as it is now.`
        : `You can no longer manage this ${noun}, so your changes could not be saved.`;

  return (
    <p ref={save.line} role="status" tabIndex={-1} className="mb-4 scroll-mt-4 rounded-lg border border-border bg-sunken px-4 py-3 text-sm outline-none">
      {text}
    </p>
  );
}
