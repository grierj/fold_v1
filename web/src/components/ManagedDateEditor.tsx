import { useRef, type MutableRefObject } from "react";
import { useLocation } from "react-router-dom";
import { DateEditor, type DateDraftHold } from "./DateEditor";
import { Button } from "./ui/Button";
import { FormActions } from "./ui/FormActions";
import { Skeleton } from "./ui/Skeleton";
import { useShownInList } from "./ui/ConfirmDialog";
import { useEditRead, type DateOpener } from "../lib/editBase";
import type { RevisionConflict } from "../lib/useRevisionSave";
import { useFocusHome, useKeepFocusHome } from "../lib/focusHome";
import { useKeptDraft } from "../lib/unsavedChanges";

interface ManagedDateEditorProps {
  opener: DateOpener;
  // The host's latest read of its page failed: nothing of the date is shown until a read succeeds.
  pageFailed?: boolean;
  onSaved: () => void;
  onConflict: (why: RevisionConflict) => void;
  onClose: () => void;
  onRoomChanged?: () => void;
  focusRoom?: boolean;
  // The read was refused, or answered another date than the list named: the host checks its page.
  onRefused: () => void;
}

// A listed date's editor on the event and group pages (M8.1). The list names the date; a fresh manager read supplies
// the form and its revision, once for the host's route and the date. While the row is hidden or the page's read has
// failed the editor is off the screen and its draft is kept here; a new route or date ends the draft, and so does Close.
export function ManagedDateEditor({ opener, pageFailed = false, onSaved, onConflict, onClose, onRoomChanged, focusRoom, onRefused }: ManagedDateEditorProps) {
  const location = useLocation();
  const shown = useShownInList();
  const owner = `${location.key}|${opener.eventId}|${opener.date}|${opener.occurrenceId}|${opener.groupId ?? ""}`;
  const read = useEditRead(opener, owner, shown && !pageFailed, onRefused);
  const held = useRef<{ owner: string; hold: MutableRefObject<DateDraftHold | null> } | null>(null);
  // A remount keeps focus here, never on the page; while the editor is on screen its own form keeps it.
  const editorShown = useRef(false);
  editorShown.current = read.kind === "ready";
  const home = useFocusHome<HTMLDivElement>(() => editorShown.current);
  useKeepFocusHome(home);

  if (held.current?.owner !== owner) held.current = { owner, hold: { current: null } };
  const hold = held.current.hold;

  // While the editor is off the screen its draft is kept here.
  useKeptDraft(() => hold.current?.unsent === true, true);

  if (!shown) return null;

  return <div ref={home.ref} tabIndex={-1} className="outline-none">
    {pageFailed
      ? <div className="mt-3 space-y-3 border-t border-border pt-3">
        <p role="status">This date can be edited again once the page loads. What you typed is kept.</p>
        <Button variant="secondary" layout="w-full sm:w-auto" onClick={onClose}>Close</Button>
      </div>
      : read.kind === "ready"
      ? <DateEditor opener={opener} base={read.base} hold={hold} onSaved={onSaved} onConflict={onConflict} onClose={onClose}
        onRoomChanged={onRoomChanged} focusRoom={focusRoom} />
      : read.kind === "loading"
        ? <Skeleton kind="lines" label="Loading this date…" />
        : read.kind === "denied"
          ? null
          : <div role="alert" className="mt-3 space-y-3 border-t border-border pt-3">
            <p>We couldn't load this date to edit it.</p>
            <FormActions>
              <Button variant="secondary" layout="w-full sm:w-auto" onClick={read.retry}>Retry loading</Button>
              <Button variant="secondary" layout="w-full sm:w-auto" onClick={onClose}>Close</Button>
            </FormActions>
          </div>}
  </div>;
}
