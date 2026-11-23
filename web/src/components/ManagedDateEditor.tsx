import { useLayoutEffect, useRef, useState, type MutableRefObject } from "react";
import { useLocation } from "react-router-dom";
import { DateEditor, type DateDraftHold } from "./DateEditor";
import { Button } from "./ui/Button";
import { FormActions } from "./ui/FormActions";
import { Skeleton } from "./ui/Skeleton";
import { useShownInList } from "./ui/ConfirmDialog";
import type { DateAttempts } from "../lib/dateWrites";
import { useEditRead, type DateOpener } from "../lib/editBase";
import { useFocusHome, useKeepFocusHome } from "../lib/focusHome";
import { useKeptDraft } from "../lib/unsavedChanges";

interface ManagedDateEditorProps {
  opener: DateOpener;
  // The host's shown answer (its useLoad dataEpoch): each new one reads the date again before it is edited.
  generation: number;
  // The host's latest read of its page failed: nothing of the date is shown or sent until a read succeeds.
  pageFailed?: boolean;
  onSaved: (stillOpen: boolean) => void;
  onClose: () => void;
  onRoomChanged?: () => void;
  focusRoom?: boolean;
  // The read or a save was refused, or answered another date than the list named: the host checks its page.
  onRefused: () => void;
}

// A listed date's editor on the event and group pages (M8.1). The list names the date; a fresh manager read
// supplies the form and its token, under the host's route, its shown answer, the row being on screen and this
// tab's credential. A change of any of them reads again and shows nothing meanwhile. What was typed survives a
// new read under the same route, credential and date, never another; Close drops it.
export function ManagedDateEditor({ opener, generation, pageFailed = false, onSaved, onClose, onRoomChanged, focusRoom, onRefused }: ManagedDateEditorProps) {
  const location = useLocation();
  const shown = useShownInList();
  // Hiding the row, even if it is shown again, retires its read.
  const [hides, setHides] = useState(0);
  useLayoutEffect(() => {
    if (!shown) setHides((n) => n + 1);
  }, [shown]);
  const scope = `${location.key}|${opener.eventId}|${opener.date}|${opener.occurrenceId}|${opener.groupId ?? ""}`;
  const read = useEditRead(opener, `${scope}|${generation}|${hides}`, shown && !pageFailed, onRefused);
  const held = useRef<{ scope: string; attempts: DateAttempts; hold: MutableRefObject<DateDraftHold | null> } | null>(null);
  // A remount while the date is read again keeps focus here, never on the page; while the editor is on screen
  // its own form keeps it.
  const editorShown = useRef(false);
  editorShown.current = read.kind === "ready";
  const home = useFocusHome<HTMLDivElement>(() => editorShown.current);
  useKeepFocusHome(home);

  // Another route, date or credential, even for a moment (A to B and back to A), ends the draft.
  if (held.current && (held.current.scope !== scope || (read.kind === "ready" && held.current.attempts !== read.attempts))) held.current = null;

  // While the date is read again the editor is off the screen and its draft is kept here.
  useKeptDraft(() => held.current?.hold.current?.unsent === true, true);

  if (!shown) return null;

  let hold: MutableRefObject<DateDraftHold | null> | undefined;

  if (read.kind === "ready") {
    held.current ??= { scope, attempts: read.attempts, hold: { current: null } };
    hold = held.current.hold;
  }

  return <div ref={home.ref} tabIndex={-1} className="outline-none">
    {pageFailed
      ? <div className="mt-3 space-y-3 border-t border-border pt-3">
        <p role="status">This date can be edited again once the page loads. What you typed is kept.</p>
        <Button variant="secondary" layout="w-full sm:w-auto" onClick={onClose}>Close</Button>
      </div>
      : read.kind === "ready" && hold
      ? <DateEditor base={read.base} permit={read.permit} attempts={read.attempts} hold={hold} onSaved={onSaved} onClose={onClose}
        onRoomChanged={onRoomChanged} focusRoom={focusRoom} onLost={onRefused} />
      : read.kind === "loading"
        ? <Skeleton kind="lines" label="Loading this date…" />
        : read.kind === "denied"
          ? null
          : <div role="alert" className="mt-3 space-y-3 border-t border-border pt-3">
            <p>{read.kind === "unavailable"
              ? "We couldn't confirm this tab's sign-in, so this date can't be edited here. Reload the page to try again."
              : "We couldn't load this date to edit it. What you typed is kept."}</p>
            <FormActions>
              {read.kind === "failed" && <Button variant="secondary" layout="w-full sm:w-auto" onClick={read.retry}>Retry loading</Button>}
              <Button variant="secondary" layout="w-full sm:w-auto" onClick={onClose}>Close</Button>
            </FormActions>
          </div>}
  </div>;
}
