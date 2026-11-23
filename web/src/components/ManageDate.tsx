import { lazy, Suspense, useCallback, useEffect, useId, useLayoutEffect, useRef, useState, type MutableRefObject } from "react";
import { useLocation } from "react-router-dom";
import { Events } from "../api/events";
import { outcomeOf, useDateAttempts, useEditBase, useRosterGate, type DateAttempts } from "../lib/dateWrites";
import { dateKey, editBaseOf, editReadRefused, useEditRefresh } from "../lib/editBase";
import { useFocusHome, useKeepFocusHome } from "../lib/focusHome";
import type { ManageAuthority } from "../lib/manageAuthority";
import { useKeptDraft } from "../lib/unsavedChanges";
import { useLoad } from "../lib/useLoad";
import { isObject } from "../lib/wireValues";
import { DateEditor, type DateDraftHold } from "./DateEditor";
import { ErrorNotice } from "./ErrorNotice";
import { Button, ButtonLink } from "./ui/Button";
import { ConfirmProvider, usePageFacts } from "./ui/ConfirmDialog";
import { Skeleton } from "./ui/Skeleton";

const ManageRoster = lazy(() => import("./ManageRoster").then((module) => ({ default: module.ManageRoster })));

interface ManageDateProps {
  eventId: number;
  date: string;
  // The date the page shows (its occurrence id), which the managed read must answer; absent while the page has none.
  occurrenceId?: number;
  // What the date page's latest load says (lib/manageAuthority); manager content renders only while granted.
  authority: ManageAuthority;
  // The page's quiet reload, after this date changed here.
  onDateChanged: () => void;
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

// What a managed read was asked under: the route (a visit to the same address is another), the date the page
// shows and this tab's credential's attempts. Nothing read, typed or asked under one is shown or sent under another.
interface Owner {
  key: string;
  attempts: DateAttempts | null;
}

const sameOwner = (a: Owner, b: Owner) => a.key === b.key && a.attempts === b.attempts;

// The owner each failed managed read was asked under.
const failedUnder = new WeakMap<object, Owner>();

function ManageDateTools({ eventId, date, occurrenceId, authority, onDateChanged, onGone, onRetryPage }: ManageDateProps) {
  const headingId = useId();
  const home = useFocusHome<HTMLElement>();
  const location = useLocation();
  const [editing, setEditing] = useState(false);
  const [managing, setManaging] = useState(false);
  const granted = authority === "granted";
  // The managed read never stands in for authority: it loads only while granted, and leaving granted drops it.
  // The roster and the editor share it: stamped before it is sent, and pinned to this tab's credential when
  // there is one, so the editor's base is the same answer the roster shows.
  const attempts = useDateAttempts();
  // The page has no occurrence while it checks authority again (nothing here is shown meanwhile); only another
  // row for the date is another owner.
  const shownRow = useRef(occurrenceId);

  if (occurrenceId !== undefined) shownRow.current = occurrenceId;
  const ownerKey = `${location.key}|${eventId}|${date}|${shownRow.current ?? ""}`;
  const owner: Owner = { key: ownerKey, attempts };

  const managed = useLoad(async () => {
    const asked = owner;

    try {
      const stamp = attempts?.stamp(eventId, date) ?? null;
      const read = await Events.managedDate(eventId, date, attempts?.credential);

      return { owner: asked, stamp, occurrence: read.occurrence };
    } catch (cause: unknown) {
      const failure = isObject(cause) ? cause : new Error("managed read failed");
      failedUnder.set(failure, asked);
      throw failure;
    }
  }, [ownerKey, attempts], { enabled: granted && (editing || managing) });

  // useLoad still holds the last owner's answer in the render where the owner changes: it is never shown, read
  // by the editor or registered under this one.
  const current = managed.data && sameOwner(managed.data.owner, owner) ? managed.data : undefined;
  const failedOwner = isObject(managed.error) ? failedUnder.get(managed.error) : undefined;
  const failure = failedOwner && sameOwner(failedOwner, owner) ? managed.error : null;

  // The editor's draft, held while authority is checked so the editor can unmount and come back with it, for
  // this owner only: another route visit or credential, even for a moment, ends it.
  const held = useRef<{ owner: Owner; hold: MutableRefObject<DateDraftHold | null> } | null>(null);

  if (held.current && !sameOwner(held.current.owner, owner)) held.current = null;
  held.current ??= { owner, hold: { current: null } };
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
  // A guarded edit of this date settled, from here or another page: the managed read and the page again. A
  // roster write's read is the gate's.
  useEditRefresh((id) => id === eventId, () => {
    refresh();
    onDateChanged();
  });
  // The one facts call under this provider: a question started under one answer never submits under another.
  usePageFacts(JSON.stringify({ authority, owner: ownerKey, data: current?.occurrence ?? null }));
  // A recheck, the gate closing or a write's retry removes the focused control in this component's render.
  useKeepFocusHome(home);

  // A credential the server no longer recognizes is not a refusal of the date: the tab checks its credential.
  const refused = failure != null && outcomeOf(failure) === "refused" && editReadRefused(failure);

  useEffect(() => {
    if (!refused) return;
    setEditing(false);
    setManaging(false);
    onGone();
  }, [refused, onGone]);

  const recheck = useCallback(() => {
    gate.clearNote();
    void managed.retry();
  }, [gate.clearNote, managed.retry]);

  const closeEditor = () => {
    hold.current = null;
    setEditing(false);
  };

  // Opening the editor reads the date again even when the roster already shows it: the form is the read's.
  const openEditor = () => {
    setEditing(true);

    if (managing) refresh();
  };

  const closeRoster = () => {
    gate.clearNote();
    setManaging(false);
  };

  const data = current?.occurrence;
  // The editor's base: the managed read, for the date the page shows, with its token.
  const base = data && occurrenceId !== undefined ? editBaseOf(data, { eventId, date, occurrenceId }) : null;
  const permit = useEditBase(attempts, editing && base ? current?.stamp ?? null : null, editing && base ? { occurrenceId: base.id, revision: base.revision } : null);
  const editStatus = attempts?.status(dateKey(eventId, date)) ?? "idle";

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
        <Button variant="secondary" aria-expanded={editing} disabled={!editing && editStatus !== "idle"} onClick={() => (editing ? closeEditor() : openEditor())}>Edit this date</Button>
        {!editing && editStatus === "unsure" && <Button variant="secondary" onClick={openEditor}>Check your last change to this date</Button>}
        <Button variant="secondary" aria-expanded={managing} onClick={() => (managing ? closeRoster() : setManaging(true))}>Manage sign-ups</Button>
      </div>
      {(editing || managing) && (failure != null
        ? <ErrorNotice message="We couldn't load this date's details. What you typed is kept." onRetry={() => void managed.retry()} />
        : !data || gate.closed
          ? <Skeleton kind="lines" label="Loading this date…" />
          : <>
            {editing && (attempts && base
              ? <DateEditor
                base={base}
                permit={permit}
                attempts={attempts}
                hold={hold}
                onSaved={closeEditor}
                onRoomChanged={refresh}
                onClose={closeEditor}
                onAuthorityLost={onGone}
                onLost={onGone}
              />
              : <p role="alert">{attempts
                ? "This date changed since the page loaded. Reload the page to edit it."
                : "We couldn't confirm this tab's sign-in, so this date can't be edited here. Reload the page to try again."}</p>)}
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
