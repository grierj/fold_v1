import { canRegisterAsGuest } from "../lib/can";
import { Form } from "../components/ui/Form";
import { useHousehold } from "../lib/useHousehold";
import { SeriesEditor } from "../components/SeriesEditor";
import { lazy, Suspense, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { useNavigate, useParams, useSearchParams } from "react-router-dom";
import { ApiError } from "../api/core";
import { Events } from "../api/events";
import { useAuth } from "../auth";
import { OccurrenceCard } from "../components/OccurrenceCard";
import { EventIntro } from "../components/EventIntro";
import { CompactDates } from "../components/CompactDates";
import { Attendance } from "../components/Attendance";
import { ManagedDateEditor } from "../components/ManagedDateEditor";
import { NeedsRoomList } from "../components/NeedsRoomList";
import { Menu } from "../components/ui/Menu";
import { composePath, EMAIL_GOING } from "../components/messages/composeLinks";
import { Field, FormError, Input } from "../components/ui/Field";
import { Button, ButtonLink } from "../components/ui/Button";
import { ExternalLinkIcon } from "../components/icons";
import { EventVisibilityControl } from "../components/EventVisibility";
import { RsvpButtons } from "../components/RsvpButtons";
import { GuestRegistrationForm } from "../components/GuestRegistrationForm";
import { DateActions } from "../components/DateActions";
import { formatTime, heldIn } from "../lib/when";
import { placeLine } from "../lib/place";
import { dateLine, whenLine } from "../lib/repeat";
import type { EventHostRow, EventOccurrence, HouseholdPerson } from "../types";
import { errorText } from "../lib/errors";
import { useLoad } from "../lib/useLoad";
import { useAction } from "../lib/useAction";
import { ErrorNotice } from "../components/ErrorNotice";
import { Missing } from "../components/Missing";

import { EmptyState } from "../components/ui/EmptyState";
import { Skeleton } from "../components/ui/Skeleton";
import { applyActorRsvpResults, type ActorRsvpResult, type SeriesFeedbackSignal } from "../lib/rsvpFeedback";
import { useRsvpMutationGate, type RsvpMutationGate } from "../lib/useRsvpMutationGate";
import { ConfirmProvider, useConfirm, usePageFacts } from "../components/ui/ConfirmDialog";
import { useLeaveQuestion } from "../lib/leaveQuestion";
import { useDateAttempts, useDateWrites } from "../lib/dateWrites";
import { dateKey, useEditRefresh } from "../lib/editBase";
import { seriesSchedules } from "../lib/registration";

const ManageRoster = lazy(() => import("../components/ManageRoster").then((module) => ({ default: module.ManageRoster })));

export function EventDetail() {
  const { id } = useParams<{ id: string }>();

  // Another event must never inherit the editor, feedback, pending loads or an open question.
  // Questions on this page ask in the app's dialog, never the browser's.
  return <ConfirmProvider key={id}><EventDetailContent /></ConfirmProvider>;
}

function EventDetailContent() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  // The group page's "Manage sign-ups" links here with ?manage=<original date> (DEC-99).
  const manageDate = useSearchParams()[0].get("manage");
  const { user, loading: authLoading, isAdmin, canCompose } = useAuth();
  const { people, error: householdError, retry: retryHousehold } = useHousehold(user);
  const [deleteError, setDeleteError] = useState<string | null>(null);
  // From the first ask until the DELETE answers: the menu cannot start a second one.
  const [deleting, setDeleting] = useState(false);
  const deletingNow = useRef(false);
  const [editing, setEditing] = useState(false);
  // The series editor and the visibility control share a panel that a save closes: while either runs, the other
  // is locked and the panel stays open, so neither's answer is lost.
  const seriesAction = useAction();
  const visibilityAction = useAction();
  const panelBusy = seriesAction.running || visibilityAction.running;
  const editButton = useRef<HTMLButtonElement>(null);
  // A save in the panel reads the page again from nothing: focus waits on the main region and goes back to Edit
  // series, the panel's opener, once the page is shown.
  const returnToEdit = useRef(false);
  const [seriesFeedback, setSeriesFeedback] = useState<SeriesFeedbackSignal | null>(null);
  const seriesFeedbackRevision = useRef(0);
  const rsvpMutationGate = useRsvpMutationGate();
  const editorRef = useRef<HTMLDivElement>(null);
  // Rooms changed (a series room, a date's room, a cancelled or moved date): ask for the dates that need one again.
  const [roomsRevision, setRoomsRevision] = useState(0);
  // "Pick a room" asks one date's card to open its editor; seq makes a second ask count.
  const [pickRequest, setPickRequest] = useState<{ date: string; seq: number } | null>(null);

  // Opening the editor brings it into view and puts the cursor in its first
  // field, so a screen reader and a keyboard follow the button they pressed.
  useEffect(() => {
    if (!editing) return;
    const editor = editorRef.current;
    editor?.scrollIntoView?.({ block: "start" });
    editor?.querySelector<HTMLElement>("input, textarea, select")?.focus({ preventScroll: true });
  }, [editing]);

  // `reload` keeps ordinary RSVP drafts mounted; `retry` (also what a terminal
  // refresh calls) drops the page at once, so protected data cannot linger, and
  // only the newest request may commit anything.
  const load = useLoad(() => Events.get(id ?? ""), [id], { enabled: !!id });

  // A roster write on one of its dates (a row's, or a kept-dates panel's inside an editor) holds that date's
  // Edit until this page shows an answer asked for after it. A date's own edits are held per credential by the
  // guarded attempts (lib/dateWrites), which ask this page and the date's editor again when one settles.
  const dateWrites = useDateWrites({
    epoch: load.epoch,
    dataEpoch: load.dataEpoch,
    shows: (target) => target.eventId === Number(id),
    reload: () => refreshDates(),
    rosters: true,
  });

  useEditRefresh((eventId) => eventId === Number(id), () => refreshDates());

  const page = load.data ?? null;
  const loading = load.loading;
  useLayoutEffect(() => {
    if (!returnToEdit.current || loading) return;
    returnToEdit.current = false;
    const active = document.activeElement;

    if (!active || active === document.body || active.id === "main") editButton.current?.focus();
  }, [loading, page]);
  const notFound = load.error instanceof ApiError && load.error.status === 404;
  const error = load.error && !notFound ? errorText(load.error, "We couldn't load this event.") : null;
  const setPage = load.setData;

  // After a date changes, here or from another page: the dates that need a room are asked for again with the
  // event, since a moved date can lose its booking (NeedsRoomList keeps its own request).
  const refreshDates = () => {
    setRoomsRevision((n) => n + 1);
    void load.reload();
  };

  // Every question on this page is about what it shows: any change to the loaded event, its dates or what
  // this person may do (a refresh, a sibling's answer) makes a pending question stale, even if it changes back.
  usePageFacts(useMemo(() => JSON.stringify(page), [page]));
  const beginDelete = useConfirm();
  useLeaveQuestion();

  function handleRsvp() { void load.reload(); }

  function parkFocus() {
    returnToEdit.current = true;
    document.getElementById("main")?.focus({ preventScroll: true });
  }

  function reportSeriesFeedback(eventId: number, date: string, actorResults: ActorRsvpResult[]) {
    const signal = { eventId, date, actorResults, revision: ++seriesFeedbackRevision.current };
    setPage((current) => current ? {
      ...current,
      occurrences: current.occurrences.map((instance) => applyActorRsvpResults(instance, eventId, actorResults)),
    } : current);
    setSeriesFeedback(signal);
  }

  if (loading || authLoading) {
    return (
      <div className="fold-page fold-page-narrow">
        <Skeleton kind="page" />
      </div>
    );
  }

  if (notFound) {
    return <Missing title="Event not found" body="It may have been cancelled or removed, or it may be private." />;
  }

  if (!page) {
    return (
      <div className="fold-page fold-page-narrow">
        <h1 className="text-3xl font-bold mb-6">Event unavailable</h1>
        <ErrorNotice message={error ?? "We couldn't load this event."} onRetry={() => void load.retry()} />
      </div>
    );
  }

  const { event, occurrences, canManage, canSetVisibility } = page;
  const title = event.title || (event.hostedBy ? "Church event" : event.groupName || "Event");
  // A single date reads as one fact in the header, with no list around it (DEC-92).
  const single = !event.recurrenceRule && occurrences.length === 1;
  const next = occurrences[0];
  const line = whenLine(event.recurrenceRule, occurrences);
  const schedules = seriesSchedules(occurrences);
  // A single date's confirmed room is its place; a series header keeps the typed location (M2.1b).
  const headerPlace = placeLine(single ? next.room : undefined, event.location);

  async function deleteEvent() {
    if (deletingNow.current) return;
    const action = beginDelete();
    deletingNow.current = true;
    setDeleting(true);
    const question = `Delete “${title}”?`;
    const consequence = "This removes every date and its registrations.";

    try {
      if (!(await action.ask({ title: question, body: consequence, confirmLabel: "Delete event", tone: "danger", native: `${question} ${consequence}` }))) return;
      setDeleteError(null);
      await Events.delete(event.id);

      // Only the page that asked moves on: someone who went elsewhere meanwhile stays there.
      if (action.here()) navigate("/", { replace: true });
    } catch (e) {
      if (action.here()) setDeleteError(errorText(e, "We couldn't delete the event."));
    } finally {
      deletingNow.current = false;
      setDeleting(false);
    }
  }

  return (
    <div className="fold-page fold-page-narrow">
      {/* Edit series and More sit beside the title as secondary actions; an event page has no primary one (DEC-63, DEC-65). */}
      <div className="fold-page-header">
        <div className="min-w-0">
          <h1 className="text-3xl font-bold mb-2 wrap-anywhere">{title}</h1>
          <EventIntro hostedBy={event.hostedBy} groupName={event.groupName} groupId={event.groupId} churchWide={event.churchWide} />
        </div>
        {canManage && (
          <div className="flex w-full gap-2 sm:w-auto sm:shrink-0">
            <Button ref={editButton} variant="secondary" layout="flex-1 sm:flex-none" aria-expanded={editing} disabled={panelBusy} onClick={() => setEditing((v) => !v)}>
              {event.recurrenceRule ? "Edit series" : "Edit event"}
            </Button>
            <Menu label="More event actions" items={[
              ...(canCompose ? [{ label: EMAIL_GOING, onSelect: () => navigate(composePath({ event: event.id })) }] : []),
              { label: "Delete event", onSelect: () => void deleteEvent(), danger: true, disabled: deleting },
            ]} />
          </div>
        )}
      </div>
      <div className="grid gap-4 pb-4 sm:grid-cols-2 sm:gap-8">
        <div className="space-y-2">
          {line && <p className="font-semibold wrap-anywhere">{line}</p>}
          {single && heldIn(next.startsAt, next.tzid) && <p className="text-sm text-text-muted">{heldIn(next.startsAt, next.tzid)}</p>}
          {headerPlace && <p className="text-text-muted mt-1 wrap-anywhere">{headerPlace}</p>}
          {single && line && <DateActions eventId={event.id} date={next.date} sharePath={`/events/${event.id}`} title={title} when={line} />}
          {event.childcareAvailable && <div className="mt-3 text-text-muted">
            <p>Childcare available{event.childcareAges && ` · ${event.childcareAges}`}</p>
            {event.childcareNotes && <p className="whitespace-pre-wrap">{event.childcareNotes}</p>}
          </div>}
        </div>
        {event.description && <p className="text-text-muted whitespace-pre-wrap">{event.description}</p>}
      </div>
      {deleteError && <div className="text-danger mt-3">{deleteError}</div>}
      {/* A refresh that failed keeps what was shown; a date it was asked for after an update stays closed until Try again lands. */}
      {error && <div className="mt-3"><ErrorNotice message={error} onRetry={refreshDates} /></div>}


      {isAdmin && event.churchWide && (
        <HostEditor eventId={event.id} hosts={event.hosts ?? []} onChanged={() => void load.retry()} />
      )}

      {canManage && editing && (
        <div ref={editorRef} className="mt-4">
          <SeriesEditor
            series={{ ...event, eventId: event.id }}
            idPrefix={`ev${event.id}`}
            roomDate={single ? next.date : undefined}
            onRoomChanged={() => { setRoomsRevision((n) => n + 1); void load.reload(); }}
            onSaved={() => {
              parkFocus();
              setEditing(false);
              setRoomsRevision((n) => n + 1);
              void load.retry();
            }}
            onClose={() => setEditing(false)}
            action={seriesAction}
            locked={visibilityAction.running}
          />
          {canSetVisibility && (
            <EventVisibilityControl
              key={`${event.id}-${event.visibility}-${event.leaderOpened}`}
              eventId={event.id}
              churchWide={event.churchWide}
              groupName={event.groupName}
              groupVisibility={event.groupVisibility}
              visibility={event.visibility}
              leaderOpened={event.leaderOpened}
              onSaved={() => {
                parkFocus();
                void load.retry();
              }}
              action={visibilityAction}
              locked={seriesAction.running}
            />
          )}
        </div>
      )}

      {canManage && (
        <NeedsRoomList
          eventId={event.id}
          revision={roomsRevision}
          hasDate={(d) => occurrences.some((o) => o.date === d)}
          onPick={(d) => setPickRequest((r) => ({ date: d, seq: (r?.seq ?? 0) + 1 }))}
        />
      )}

      {!single && <h2 className="text-xl font-semibold mt-6 mb-4">Upcoming dates</h2>}
      {householdError && <ErrorNotice message={householdError} onRetry={retryHousehold} />}
      {occurrences.length === 0 && (
        <EmptyState title="No upcoming dates." body="This event has no more dates on the calendar." />
      )}
      <div className={single ? "mt-6 fold-list" : "fold-list"}>
        <CompactDates dates={occurrences} requested={pickRequest?.date ?? (canManage ? manageDate ?? undefined : undefined)} requestVersion={pickRequest?.seq}>
          {(m) => <DateRow
            key={`${m.eventId}-${m.date}`}
            instance={m}
            seriesDates={schedules.get(m.eventId) ?? ""}
            single={single}
            eventTitle={title}
            headerLocation={event.location}
            loggedIn={!!user}
            canManage={canManage}
            manageRequested={canManage && manageDate === m.date}
            writingDate={dateWrites.writing(m.eventId, m.date)}
            settlingDate={dateWrites.settling(m.eventId, m.date)}
            generation={load.dataEpoch}
            pageFailed={load.error != null}
            editRequest={pickRequest?.date === m.date ? pickRequest.seq : 0}
            onRsvp={handleRsvp}
            people={people}
            seriesFeedback={seriesFeedback}
            mutationGate={rsvpMutationGate}
            onSeriesFeedback={reportSeriesFeedback}
            onTerminalRefresh={() => void load.retry()}
            onChanged={refreshDates}
            onNeedLogin={() => navigate("/login", { state: { from: `/events/${id}` } })}
          />}
        </CompactDates>
      </div>
    </div>
  );
}

function DateRow({
  instance,
  seriesDates,
  single,
  eventTitle,
  headerLocation,
  loggedIn,
  canManage,
  manageRequested,
  writingDate,
  settlingDate,
  generation,
  pageFailed,
  editRequest,
  onRsvp,
  people,
  seriesFeedback,
  mutationGate,
  onSeriesFeedback,
  onTerminalRefresh,
  onChanged,
  onNeedLogin,
}: {
  instance: EventOccurrence;
  seriesDates: string;
  // The page header already says this date's title, place and time.
  single: boolean;
  eventTitle: string;
  headerLocation: string;
  loggedIn: boolean;
  canManage: boolean;
  // ?manage=<this date> asked for the Manage sign-ups panel to open (DEC-99).
  manageRequested: boolean;
  // A roster write on this date is on its way, or answered and the page does not show it yet (EventDetailContent's dateWrites).
  writingDate: boolean;
  settlingDate: boolean;
  // The page's shown answer: the date's editor reads the date again for each. While the page's latest read
  // failed, the editor shows nothing of the date.
  generation: number;
  pageFailed: boolean;
  // Counts up each time "Pick a room" asks for this date's editor; 0 when it hasn't.
  editRequest: number;
  onRsvp: () => void;
  people: HouseholdPerson[];
  seriesFeedback: SeriesFeedbackSignal | null;
  mutationGate: RsvpMutationGate;
  onSeriesFeedback: (eventId: number, date: string, actorResults: ActorRsvpResult[]) => void;
  onTerminalRefresh: () => void;
  onChanged: () => void;
  onNeedLogin: () => void;
}) {
  const navigate = useNavigate();
  const { canCompose } = useAuth();
  const [editingDate, setEditingDate] = useState(false);
  // This date's guarded edit: anything but idle closes Edit; a save whose answer never arrived is reached
  // through its own opener, outside the menu, so Close or leaving never strands it.
  const attempts = useDateAttempts();
  const editStatus = attempts?.status(dateKey(instance.eventId, instance.date)) ?? "idle";
  const [managing, setManaging] = useState(manageRequested);
  const manageRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!manageRequested) return;
    setManaging(true);
    manageRef.current?.scrollIntoView?.({ block: "start" });
  }, [manageRequested]);
  // "Pick a room" on the dates-that-need-a-room list opens this date's editor and moves to it, once per ask.
  const editorRef = useRef<HTMLDivElement>(null);
  const handledRequest = useRef(0);
  // The editor was opened by "Pick a room", so its Room control takes the cursor.
  const [focusRoom, setFocusRoom] = useState(false);
  useEffect(() => {
    if (editRequest > 0 && editRequest !== handledRequest.current) {
      setEditingDate(true);
      setFocusRoom(true);
    }
  }, [editRequest]);
  useEffect(() => {
    if (editRequest === 0 || editRequest === handledRequest.current || !editingDate) return;
    handledRequest.current = editRequest;
    const editor = editorRef.current;
    editor?.scrollIntoView?.({ block: "start" });
  }, [editRequest, editingDate]);
  // "asking" from the first ask, "cancelling" while the request runs: either way the menu cannot start another.
  const [cancelStep, setCancelStep] = useState<"asking" | "cancelling" | null>(null);
  const cancelNow = useRef(false);
  const [cancelError, setCancelError] = useState<string | null>(null);
  const beginCancel = useConfirm();
  const time = formatTime(instance.startsAt);
  const held = heldIn(instance.startsAt, instance.tzid);
  const place = placeLine(instance.room, instance.location);

  async function cancel() {
    if (cancelNow.current) return;
    const action = beginCancel();
    cancelNow.current = true;
    setCancelStep("asking");
    const question = `Cancel this date on ${instance.date}?`;

    if (!(await action.ask({ title: question, confirmLabel: "Cancel this date", cancelLabel: "Keep this date", tone: "danger", native: question }))) {
      cancelNow.current = false;
      setCancelStep(null);

      return;
    }

    setCancelStep("cancelling");
    setCancelError(null);

    try {
      await Events.cancelOccurrence(instance.eventId, instance.date);
      // Stays "Cancelling…" until the refresh takes the date away.
      onChanged();
    } catch (e) {
      setCancelError(errorText(e, "We couldn't cancel that date."));
      cancelNow.current = false;
      setCancelStep(null);
    }
  }

  return (
    <OccurrenceCard at={instance.startsAt} title={instance.title || "Church event"} compact={single} headingLevel={single ? 2 : 3}
      notice={canManage && instance.visibilityOverride && <p className="text-xs text-text-muted">Visibility changed for this date</p>}
      meta={single ? (
        // Only what this date changed from the header.
        (instance.title && instance.title !== eventTitle) || (instance.location && instance.location !== headerLocation) ? <>
          {instance.title && instance.title !== eventTitle && <div className="font-semibold">{instance.title}</div>}
          {instance.location && instance.location !== headerLocation && <div className="text-sm text-text-muted">{instance.location}</div>}
        </> : null
      ) : <>
        <div className="text-sm text-text-muted">{time}{place ? ` · ${place}` : ""}</div>
        {held && <div className="text-sm text-text-muted">{held}</div>}
      </>}
      menu={canManage && <Menu
        tone="quiet"
        label={`More for ${instance.date}`}
        items={[
          { label: "Edit this date", onSelect: () => { setFocusRoom(false); setEditingDate((v) => !v); }, disabled: writingDate || settlingDate || editStatus !== "idle" },
          { label: "Manage sign-ups", onSelect: () => setManaging((v) => !v) },
          // A single date's people are the event's: the header's menu has it.
          ...(canCompose && !single ? [{ label: EMAIL_GOING, onSelect: () => navigate(composePath({ occurrence: instance.id })) }] : []),
          { label: cancelStep === "cancelling" ? "Cancelling…" : "Cancel this date", onSelect: () => void cancel(), danger: true, disabled: cancelStep !== null },
        ]}
      />}
    >
        {instance.videoUrl ? (
          <div>
            <ButtonLink to={instance.videoUrl} external variant="secondary">
              Join by video
              <span className="sr-only"> (opens in a new tab)</span>
              <ExternalLinkIcon className="h-4 w-4" />
            </ButtonLink>
          </div>
        ) : instance.videoHidden ? (
          <div className="text-sm text-text-muted">Tap Going below to get the video link.</div>
        ) : null}
        {cancelError && <FormError>{cancelError}</FormError>}
        {instance.overridden && (
          <div className="text-sm text-text-muted">This date was edited on its own, so series changes leave it alone.</div>
        )}
        <Attendance instance={instance} headingLevel={single ? 3 : 4} />
    <div>
        <div>
          {loggedIn ? (
            <RsvpButtons instance={instance} seriesDates={seriesDates} people={people} headingLevel={single ? 3 : 4} onChange={onRsvp} seriesFeedback={seriesFeedback}
              mutationGate={mutationGate} onSeriesFeedback={onSeriesFeedback} onTerminalRefresh={onTerminalRefresh} />
          ) : canRegisterAsGuest(loggedIn, instance.visibility) ? (
            <div className="space-y-2">
              <GuestRegistrationForm instance={instance} />
              <p className="text-sm text-text-muted">Already have an account?</p>
              <Button variant="secondary" onClick={onNeedLogin} layout="w-full sm:w-auto">
                Log in to register
              </Button>
            </div>
          ) : (
            <Button variant="primary" onClick={onNeedLogin} layout="w-full sm:w-auto">
              Log in to register
            </Button>
          )}
        </div>
        {canManage && !editingDate && editStatus === "unsure" && (
          <Button variant="secondary" layout="w-full sm:w-auto" onClick={() => { setFocusRoom(false); setEditingDate(true); }}>Check your last change to this date</Button>
        )}
        {canManage && editingDate && (
          <div ref={editorRef} className="scroll-mt-4">
          <ManagedDateEditor
            opener={{ eventId: instance.eventId, date: instance.date, occurrenceId: instance.id }}
            generation={generation}
            pageFailed={pageFailed}
            onRoomChanged={onChanged}
            focusRoom={focusRoom}
            onSaved={(stillOpen) => {
              // An editor that went away while its save ran (managing rights came and went) closes nothing it did not own.
              if (stillOpen) setEditingDate(false);
            }}
            onClose={() => setEditingDate(false)}
            onRefused={() => {
              setEditingDate(false);
              onTerminalRefresh();
            }}
          />
          </div>
        )}
        {canManage && managing && <div ref={manageRef} className="scroll-mt-4"><Suspense fallback={<Skeleton kind="lines" />}><ManageRoster instance={instance} headingLevel={single ? 3 : 4} onChanged={onChanged} onClose={() => setManaging(false)} /></Suspense></div>}
    </div>
        {!single && (
          <DateActions eventId={instance.eventId} date={instance.date} sharePath={`/events/${instance.eventId}/dates/${instance.date}`}
            title={instance.title || eventTitle} when={dateLine(instance.startsAt, instance.endsAt)} nameDate />
        )}
    </OccurrenceCard>
  );
}

function HostEditor({
  eventId,
  hosts,
  onChanged,
}: {
  eventId: number;
  hosts: EventHostRow[];
  onChanged: () => void;
}) {
  const [email, setEmail] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function add(e: React.FormEvent) {
    e.preventDefault();

    if (!email.trim()) return;
    setBusy(true);
    setError(null);

    try {
      await Events.addHost(eventId, email.trim());
      setEmail("");
      onChanged();
    } catch (err) {
      setError(errorText(err, "We couldn't add that host."));
    } finally {
      setBusy(false);
    }
  }

  async function remove(userId: number) {
    setBusy(true);
    setError(null);

    try {
      await Events.removeHost(eventId, userId);
      onChanged();
    } catch (err) {
      setError(errorText(err, "We couldn't remove that host."));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="fold-section mt-4">
      <div className="font-semibold mb-2">Hosts</div>
      {error && <FormError layout="mb-2">{error}</FormError>}
      {hosts.length === 0 && <p className="text-sm text-text-muted mb-3">No one hosts this event right now. Add a host below.</p>}
      <ul className="text-sm space-y-1 mb-3">
        {hosts.map((h) => (
          <li key={h.userId} className="flex items-center justify-between gap-3">
            <span>
              {h.name} <span className="text-text-muted">{h.email}</span>
            </span>
            {hosts.length > 1 && (
              <Button type="button" variant="danger-outline" disabled={busy} onClick={() => void remove(h.userId)}>
                Remove
              </Button>
            )}
          </li>
        ))}
      </ul>
      <Form noValidate onSubmit={add} className="flex flex-col gap-3 sm:flex-row sm:items-end">
        <div className="min-w-0 flex-1">
          <Field label="Host email" hint="They must already have an account.">
            <Input type="email" value={email} onChange={(e) => setEmail(e.target.value)} required />
          </Field>
        </div>
        <Button variant="secondary" layout="w-full sm:w-auto" type="submit" disabled={busy}>
          Add host
        </Button>
      </Form>
    </div>
  );
}

