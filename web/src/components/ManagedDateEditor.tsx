import { useRef, useState, type MutableRefObject } from "react";
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

// A listed date's "Edit this date" toggle and its editor's endings, the same on the event and group pages. The toggle is
// off while the date's save is on its way, as the editor's own Close is: closing and opening it again would show a new
// draft that the earlier save's answer then closes.
export function useListedDateEditor({ onEditDate, onChanged, onDateConflict, onTerminalRefresh }: {
  // The editor opened, which clears what the page said about the date's last refused save.
  onEditDate: () => void;
  // Saved, or the room changed: the host reads its page again.
  onChanged: () => void;
  onDateConflict: (why: RevisionConflict) => void;
  // The read was refused: the host checks its page.
  onTerminalRefresh: () => void;
}) {
  // `saving` can outlast `open`: an editor closed while its save is on its way still answers it.
  const [row, setRow] = useState({ open: false, saving: false });
  const close = () => setRow((r) => ({ ...r, open: false }));

  return {
    open: row.open,
    saving: row.saving,
    toggle: () => { onEditDate(); setRow((r) => ({ ...r, open: !r.open })); },
    show: () => { onEditDate(); setRow((r) => ({ ...r, open: true })); },
    editor: {
      saving: row.saving,
      onSaving: (saving: boolean) => setRow((r) => ({ ...r, saving })),
      onSaved: () => { close(); onChanged(); },
      onConflict: (why: RevisionConflict) => { close(); onDateConflict(why); },
      onClose: close,
      onRoomChanged: onChanged,
      onRefused: () => { close(); onTerminalRefresh(); },
    },
  };
}

interface ManagedDateEditorProps {
  opener: DateOpener;
  // The host's latest read of its page failed: nothing of the date is shown until a read succeeds.
  pageFailed?: boolean;
  // This date's save is on its way, also from an editor the host has since taken off the screen.
  saving?: boolean;
  onSaving?: (saving: boolean) => void;
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
export function ManagedDateEditor({ opener, pageFailed = false, saving = false, onSaving, onSaved, onConflict, onClose, onRoomChanged, focusRoom, onRefused }: ManagedDateEditorProps) {
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
      ? <DateEditor opener={opener} base={read.base} hold={hold} onSaved={onSaved} onSaving={onSaving} pending={saving} onConflict={onConflict} onClose={onClose}
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
