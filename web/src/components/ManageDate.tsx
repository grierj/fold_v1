import { outcomeOf, useRosterGate } from "../lib/rosterWrites";
import { lazy, Suspense, useCallback, useEffect, useId, useLayoutEffect, useRef, useState, type MutableRefObject } from "react";
import { useLocation } from "react-router-dom";
import { currentCredential } from "../api/core";
import { Events } from "../api/events";
import { editBaseOf, editReadRefused } from "../lib/editBase";
import { useFocusHome, useKeepFocusHome } from "../lib/focusHome";
import type { ManageAuthority } from "../lib/manageAuthority";
import { useAction } from "../lib/useAction";
import { useKeptDraft } from "../lib/unsavedChanges";
import { useLoad } from "../lib/useLoad";
import type { RevisionConflict } from "../lib/useRevisionSave";
import { isObject } from "../lib/wireValues";
import { DateEditor, type DateDraftHold } from "./DateEditor";
import { useDateEditorToggle } from "./ManagedDateEditor";
import { ErrorNotice } from "./ErrorNotice";
import { Button, ButtonLink } from "./ui/Button";
import { ConfirmProvider, useConfirm, usePageFacts } from "./ui/ConfirmDialog";
import { Skeleton } from "./ui/Skeleton";
import { FormError } from "./ui/Field";

const ManageRoster = lazy(() => import("./ManageRoster").then((module) => ({ default: module.ManageRoster })));

interface ManageDateProps {
  eventId: number;
  date: string;
  // The date the page shows (its occurrence id), which the managed read must answer; absent while the page has none.
  occurrenceId?: number;
  endsAt?: string;
  // What the date page's latest load says (lib/manageAuthority); manager content renders only while granted.
  authority: ManageAuthority;
  // The page's quiet reload, after this date changed here.
  onDateChanged: () => void;
  onCancelled: () => void;
  // A save was refused and not sent again: the editor closed, and the page reads from nothing and says so (DEC-164),
  // until the editor opens again (onOpenEditor).
  onConflict: (why: RevisionConflict) => void;
  onOpenEditor: () => void;
  // The page's retry, when a manager read was refused: the page's answer decides.
  onGone: () => void;
  // The page's "Try again" while authority is unknown.
  onRetryPage: () => void;
}

// The date page's manager tools (DEC-157: a manager reaches a date the event page does not list here). The
// wrapper only returns the questions' provider; everything else lives in ManageDateTools beneath it, so the
// facts its one usePageFacts names reach this provider and never the page's own.
export function ManageDate(props: ManageDateProps) {
  return <ConfirmProvider><ManageDateTools {...props} /></ConfirmProvider>;
}

// The owner each failed managed read was asked under: the route (a visit to the same address is another), the date
// and the row the page shows. Nothing read, typed or asked under one is shown or sent under another.
const failedUnder = new WeakMap<object, string>();

function ManageDateTools({ eventId, date, occurrenceId, endsAt, authority, onDateChanged, onCancelled, onConflict, onOpenEditor, onGone, onRetryPage }: ManageDateProps) {
  const headingId = useId();
  const home = useFocusHome<HTMLElement>();
  const location = useLocation();
  const beginCancel = useConfirm();
  const cancellation = useAction();

  // The toggle and the editor's endings, as on the event and group pages: while the date's save is on its way (also
  // with the editor off the screen for an authority check) the toggle is off and an editor shown again stays as it was.
  const dateEditor = useDateEditorToggle({
    onEditDate: onOpenEditor,
    // A stored save reads the date and the page again, as after any change made here.
    onChanged: () => { refresh(); onDateChanged(); },
    onDateConflict: onConflict,
    onTerminalRefresh: onGone,
  });

  const editing = dateEditor.open;
  const [managing, setManaging] = useState(false);
  const granted = authority === "granted";
  // The managed read never stands in for authority: it loads only while granted, and leaving granted drops it.
  // The roster and the editor share it, so the editor's base is the same answer the roster shows.
  // The page has no occurrence while it checks authority again (nothing here is shown meanwhile); only another
  // row for the date is another owner.
  const shownRow = useRef(occurrenceId);

  if (occurrenceId !== undefined) shownRow.current = occurrenceId;
  const owner = `${location.key}|${eventId}|${date}|${shownRow.current ?? ""}`;

  const managed = useLoad(async () => {
    const asked = owner;

    try {
      const read = await Events.managedDate(eventId, date, currentCredential() ?? undefined);

      return { owner: asked, occurrence: read.occurrence };
    } catch (cause: unknown) {
      const failure = isObject(cause) ? cause : new Error("managed read failed");
      failedUnder.set(failure, asked);
      throw failure;
    }
  }, [owner], { enabled: granted && (editing || managing) });

  // useLoad still holds the last owner's answer in the render where the owner changes: it is never shown or read by
  // the editor.
  const current = managed.data?.owner === owner ? managed.data : undefined;
  const failure = isObject(managed.error) && failedUnder.get(managed.error) === owner ? managed.error : null;

  // The editor's draft, held while authority is checked so the editor can unmount and come back with it, for this
  // owner only: another route visit or row, even for a moment, ends it.
  const held = useRef<{ owner: string; hold: MutableRefObject<DateDraftHold | null> } | null>(null);

  if (held.current?.owner !== owner) held.current = { owner, hold: { current: null } };
  const hold = held.current.hold;
  // While authority is checked or the date is read again the editor is off the screen and its draft is kept here.
  useKeptDraft(() => hold.current?.unsent === true, editing);

  // Roster writes on this date in flight for the session: the one starter of the read after a write, the
  // kept-dates panel's inside the editor included, so the editor's questions never outlive the facts they asked under.
  const gate = useRosterGate(eventId, date, managed);
  const rosterOpen = useRef(managing);

  useLayoutEffect(() => {
    rosterOpen.current = managing;
  });

  // While the roster is on screen no refresh of the managed read is quiet: the old names, contacts and care
  // notes go in the render the refresh starts.
  const refresh = useCallback(() => void (rosterOpen.current ? managed.retry() : managed.reload()), [managed.retry, managed.reload]);

  // A room pick that landed reads the date and the page again, as after any change made here, so the page's place
  // shows the room. One refused by someone else's change wrote nothing: the page reads again quietly and the managed
  // read, with any roster on screen, stays under the line that says so.
  const roomChanged = useCallback((refused?: boolean) => {
    if (!refused) refresh();

    onDateChanged();
  }, [refresh, onDateChanged]);

  // The one facts call under this provider: a question started under one answer never submits under another.
  usePageFacts(JSON.stringify({ authority, owner, endsAt, data: current?.occurrence ?? null }));
  // A recheck, the gate closing or a write's retry removes the focused control in this component's render.
  useKeepFocusHome(home);

  // A credential the server no longer recognizes is not a refusal of the date: the tab checks its credential.
  const refused = failure != null && outcomeOf(failure) === "refused" && editReadRefused(failure);

  const closeEditorRow = dateEditor.close;

  useEffect(() => {
    if (!refused) return;
    closeEditorRow();
    setManaging(false);
    onGone();
  }, [refused, onGone, closeEditorRow]);

  const recheck = useCallback(() => {
    gate.clearNote();
    void managed.retry();
  }, [gate.clearNote, managed.retry]);

  // Closing ends the draft. Opening reads the date again even when the roster already shows it: the form is the read's.
  const toggleEditor = () => {
    if (editing) hold.current = null;
    else if (managing) refresh();

    dateEditor.toggle();
  };

  const closeEditor = () => {
    hold.current = null;
    dateEditor.editor.onClose();
  };

  const closeRoster = () => {
    gate.clearNote();
    setManaging(false);
  };

  const canCancel = endsAt !== undefined && Date.parse(endsAt) > Date.now();

  function cancelDate() {
    const action = beginCancel();
    const title = `Cancel this date on ${date}?`;
    void cancellation.run(async () => {
      if (!(await action.ask({ title, confirmLabel: "Cancel this date", cancelLabel: "Keep this date", tone: "danger", native: title })) || !action.current()) return false;

      await Events.cancelOccurrence(eventId, date);

      if (action.here()) onCancelled();
    }, { fallback: "We couldn't cancel that date." });
  }

  const data = current?.occurrence;
  // The editor's base: the managed read, for the date the page shows, with its revision.
  const opener = occurrenceId !== undefined ? { eventId, date, occurrenceId } : null;
  const base = data && opener ? editBaseOf(data, opener) : null;

  return <section ref={home.ref} tabIndex={-1} aria-labelledby={headingId} className="fold-section space-y-3 pt-6">
    <h2 id={headingId} className="text-xl font-semibold">Manage this date</h2>
    {(authority === "checking" || (granted && refused)) && <p role="status">Checking that you can still manage this date…</p>}
    {authority === "unknown" && <>
      <p role="status">We couldn't check that you can still manage this date. What you typed is kept.</p>
      <Button variant="secondary" onClick={onRetryPage}>Try again</Button>
    </>}
    {granted && !refused && <>
      <p className="text-text-muted">Change this date or who's signed up. Series changes are on the event page.</p>
      <div className="flex flex-wrap items-center gap-2">
        <ButtonLink variant="link" to={`/events/${eventId}`}>Open the event page</ButtonLink>
        <Button variant="secondary" aria-expanded={editing} disabled={dateEditor.saving} onClick={toggleEditor}>Edit this date</Button>
        <Button variant="secondary" aria-expanded={managing} onClick={() => (managing ? closeRoster() : setManaging(true))}>Manage sign-ups</Button>
        {canCancel && <Button variant="danger-outline" onClick={cancelDate} disabled={cancellation.running || dateEditor.saving}>Cancel this date</Button>}
      </div>
      {cancellation.state.status === "failed" && <FormError>{cancellation.state.text}</FormError>}
      {(editing || managing) && (failure != null
        ? <ErrorNotice message="We couldn't load this date's details. What you typed is kept." onRetry={() => void managed.retry()} />
        : !data || gate.closed
          ? <Skeleton kind="lines" label="Loading this date…" />
          : <>
            {editing && (opener && base
              ? <DateEditor
                opener={opener}
                base={base}
                hold={hold}
                onSaved={dateEditor.editor.onSaved}
                onSaving={dateEditor.editor.onSaving}
                pending={dateEditor.saving}
                onConflict={dateEditor.editor.onConflict}
                onRoomChanged={roomChanged}
                onClose={closeEditor}
                onAuthorityLost={onGone}
              />
              : <p role="alert">This date can't be edited now.</p>)}
            {managing && (data.attendeesVisible === true
              ? <>
                {gate.note && <p role="status" className="text-sm">Your last change may not have gone through. Here's the list as it is now.</p>}
                <Suspense fallback={<Skeleton kind="lines" />}>
                  <ManageRoster instance={data} headingLevel={3} gate={gate.gate} onRecheck={recheck} onClose={closeRoster} />
                </Suspense>
              </>
              : <p className="text-sm text-text-muted">This date has ended or was cancelled, so its sign-ups can't be changed here.</p>)}
          </>)}
    </>}
  </section>;
}
