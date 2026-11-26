import { canRegisterAsGuest } from "../lib/can";
import { Form } from "../components/ui/Form";
import { useHousehold } from "../lib/useHousehold";
import { SeriesEditor, type SeriesDraftHold } from "../components/SeriesEditor";
import { lazy, Suspense, useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { useNavigate, useParams, useSearchParams } from "react-router-dom";
import { ApiError, bootstrapCredential, credentialCurrent } from "../api/core";
import { Events } from "../api/events";
import { useAuth } from "../auth";
import { OccurrenceCard } from "../components/OccurrenceCard";
import { EventIntro } from "../components/EventIntro";
import { CompactDates } from "../components/CompactDates";
import { Attendance } from "../components/Attendance";
import { ManagedDateEditor, useDateEditorToggle } from "../components/ManagedDateEditor";
import { NeedsRoomList } from "../components/NeedsRoomList";
import { Menu } from "../components/ui/Menu";
import { composePath, EMAIL_GOING } from "../components/messages/composeLinks";
import { Field, FormError, Input } from "../components/ui/Field";
import { Button, ButtonLink } from "../components/ui/Button";
import { ExternalLinkIcon } from "../components/icons";
import { EventVisibilityControl, type VisibilityDraftHold } from "../components/EventVisibility";
import type { RevisionConflict } from "../lib/useRevisionSave";
import { RefusedSaveLine, ResourceRefusal, resourceRefused, useRefusedSave, useResourceRefusal, type ResourceReadOutcome } from "../lib/refusedSave";
import { RsvpButtons } from "../components/RsvpButtons";
import { GuestRegistrationForm } from "../components/GuestRegistrationForm";
import { DateActions } from "../components/DateActions";
import { formatDayFull, formatEventDay, formatTime, heldIn } from "../lib/when";
import { placeLine } from "../lib/place";
import { dateLine, whenLine } from "../lib/repeat";
import type { EventHostRow, EventOccurrence, HouseholdPerson, RestorableDate } from "../types";
import { errorText } from "../lib/errors";
import { useLoad } from "../lib/useLoad";
import { useBeforeDeadline } from "../lib/useBeforeDeadline";
import { useAction } from "../lib/useAction";
import { ErrorNotice } from "../components/ErrorNotice";
import { Missing } from "../components/Missing";

import { EmptyState } from "../components/ui/EmptyState";
import { Skeleton } from "../components/ui/Skeleton";
import { applyActorRsvpResults, type ActorRsvpResult, type SeriesFeedbackSignal } from "../lib/rsvpFeedback";
import { useRsvpMutationGate, type RsvpMutationGate } from "../lib/useRsvpMutationGate";
import { ConfirmProvider, useConfirm, usePageFacts } from "../components/ui/ConfirmDialog";
import { useKeptDraft } from "../lib/unsavedChanges";
import { useLeaveQuestion } from "../lib/leaveQuestion";
import { ManagerSaveStatus } from "../components/ui/ManagerSaveStatus";
import { useRosterSettled } from "../lib/rosterWrites";
import { seriesSchedules } from "../lib/registration";
import { EventReadPin } from "../lib/eventReadPin";
import { signedUpNotice } from "../lib/signedUp";

const ManageRoster = lazy(() => import("../components/ManageRoster").then((module) => ({ default: module.ManageRoster })));

type EditorRead = "visibility" | "room";

// React generates new ids for unnamed choices on remount. Their labels and the button names survive the read.
function editorFocusKey(control: Element): string {
  if (control instanceof HTMLInputElement && (control.type === "checkbox" || control.type === "radio")) {
    const label = control.getAttribute("aria-label") || control.labels?.[0]?.textContent;

    if (label) return `${control.type}:${control.name}:${label}`;
  }

  if (control instanceof HTMLButtonElement) {
    const label = control.getAttribute("aria-label") || control.textContent;

    if (label) return `button:${label}`;
  }

  return control.id;
}

export function EventDetail() {
  const { id } = useParams<{ id: string }>();
  const { user } = useAuth();

  // Another event must never inherit the editor, feedback, pending loads or an open question.
  // Questions on this page ask in the app's dialog, never the browser's.
  return <ConfirmProvider key={`${user?.id ?? "anonymous"}:${id}`}><EventDetailContent /></ConfirmProvider>;
}

function EventDetailContent() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const [searchParams] = useSearchParams();
  // The group page's "Manage sign-ups" links here with ?manage=<original date> (DEC-99).
  const manageDate = searchParams.get("manage");
  // Its "Edit series" / "Edit event" links here with ?edit=series (DEC-161). Read once, so neither a reload after a
  // save nor Close opens the editor again while the person stays here.
  const editAsked = useRef(searchParams.get("edit") === "series");
  const { user, loading: authLoading, isAdmin, canCompose, credential } = useAuth();
  const pin = !authLoading && credential?.verified && credential.viewer === (user?.id ?? null) && credentialCurrent(credential) ? credential : null;
  const scope = useMemo(() => pin && { pin, role: user?.role }, [pin, user?.role]);
  const { people, error: householdError, retry: retryHousehold } = useHousehold(user, pin);
  const [deleteError, setDeleteError] = useState<string | null>(null);
  // From the first ask until the DELETE answers: the menu cannot start a second one.
  const [deleting, setDeleting] = useState(false);
  const deletingNow = useRef(false);
  const [editing, setEditing] = useState(false);
  const seriesDraft = useRef<SeriesDraftHold | null>(null);
  const visibilityDraft = useRef<VisibilityDraftHold | null>(null);
  const refusedEditorReads = useRef(new Set<EditorRead>());
  useKeptDraft(() => seriesDraft.current !== null || visibilityDraft.current !== null, editing);

  const closeEditors = useCallback(() => {
    seriesDraft.current = null;
    visibilityDraft.current = null;
    refusedEditorReads.current.clear();
    setEditing(false);
  }, []);

  // The sibling is locked while a save runs so the read can restore exactly the draft the person last saw.
  const seriesAction = useAction();
  const visibilityAction = useAction();
  const panelBusy = seriesAction.running || visibilityAction.running;
  const [panelUpdated, setPanelUpdated] = useState(false);
  const [restoredDate, setRestoredDate] = useState<RestorableDate | null>(null);
  const [updatedDateId, setUpdatedDateId] = useState<number | null>(null);
  const managerStatus = <ManagerSaveStatus saving={panelBusy} updated={panelUpdated} />;
  useEffect(() => {
    if (panelBusy) setPanelUpdated(false);
  }, [panelBusy]);
  const editButton = useRef<HTMLButtonElement>(null);
  // An empty key returns to the opener. Only a control present in the restored editor can take focus.
  const returnToEdit = useRef<string | null>(null);
  // Close took the focused control with the panel: focus goes back to Edit series, never the page body.
  const closedPanel = useRef(false);
  const [seriesFeedback, setSeriesFeedback] = useState<SeriesFeedbackSignal | null>(null);
  const seriesFeedbackRevision = useRef(0);
  const rsvpMutationGate = useRsvpMutationGate();
  const editorRef = useRef<HTMLDivElement | null>(null);

  // Rooms changed (a series room, a date's room, a cancelled or moved date): ask for the dates that need one again.
  const [roomsRevision, setRoomsRevision] = useState(0);
  // "Pick a room" asks one date's card to open its editor; seq makes a second ask count.
  const [pickRequest, setPickRequest] = useState<{ date: string; seq: number } | null>(null);

  const attachEditor = useCallback((editor: HTMLDivElement | null) => {
    const active = document.activeElement;

    if (!editor && active && editorRef.current?.contains(active)) {
      returnToEdit.current = editorFocusKey(active);
      document.getElementById("main")?.focus({ preventScroll: true });
    }

    editorRef.current = editor;
  }, []);

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
  const load = useLoad(async () => {
    if (!pin || !id) throw new Error("An event read needs a verified session and an event.");

    return Events.get(id, pin);
  }, [id], { enabled: !!id && !!pin, scope });

  const { retry, close } = load;

  const retryEvent = useCallback(() => {
    refusedEditorReads.current.clear();

    if (pin) void retry();
    else void bootstrapCredential().catch(() => {});
  }, [pin, retry]);

  const status = load.error instanceof ApiError ? load.error.status : null;
  const sessionLost = load.error instanceof ApiError && load.error.code === "session_expired";
  const missing = status === 404;
  const denied = status === 401 || status === 403;
  const page = !pin || missing || denied ? null : load.data ?? null;
  const loading = authLoading || load.loading;
  useLayoutEffect(() => {
    if (!pin || loading || sessionLost) return;

    if (missing || denied || (page && !page.canManage)) {
      closeEditors();
    } else if (page && !page.canSetVisibility) {
      visibilityDraft.current = null;
      refusedEditorReads.current.delete("visibility");
    }
  }, [pin, loading, sessionLost, missing, denied, page, closeEditors]);
  useLayoutEffect(() => {
    if (!editAsked.current || !page || loading || authLoading) return;
    editAsked.current = false;

    if (page.canManage) setEditing(true);
  }, [page, loading, authLoading]);
  useLayoutEffect(() => {
    if (returnToEdit.current === null || loading || authLoading || !page) return;
    const key = returnToEdit.current;
    returnToEdit.current = null;
    const active = document.activeElement;

    if (!active || active === document.body || active.id === "main") {
      const controls = editorRef.current?.querySelectorAll<HTMLElement>("input, select, textarea, button, [tabindex]") ?? [];
      const matches = key ? Array.from(controls).filter((el) => editorFocusKey(el) === key && !el.matches(":disabled")) : [];

      if (matches.length === 1) matches[0].focus();
      else editButton.current?.focus({ preventScroll: true });
    }
  }, [loading, authLoading, editing, page]);
  useLayoutEffect(() => {
    if (editing || !closedPanel.current) return;
    closedPanel.current = false;

    if (!document.activeElement || document.activeElement === document.body) editButton.current?.focus();
  }, [editing]);
  const refusedSave = useRefusedSave(load);
  const { refused } = refusedSave;
  const roomListRefused = useRef(false);
  // A date's save refused and not sent again (DEC-164): which date, for the line above the dates.
  const [refusedDate, setRefusedDate] = useState<{ eventId: number; date: string } | null>(null);

  useLayoutEffect(() => {
    refusedEditorReads.current.clear();
    roomListRefused.current = false;
    setPickRequest(null);
    setSeriesFeedback(null);
    setRefusedDate(null);
    setUpdatedDateId(null);
  }, [scope]);

  const refreshRefusal = useCallback((intent: "read" | "write") => {
    if (!credentialCurrent(pin)) return;
    setPickRequest(null);
    setRoomsRevision((n) => n + 1);

    if (intent === "write") {
      closeEditors();
      refused("gone", "action");
    } else void retry();
  }, [pin, closeEditors, refused, retry]);

  const editorRead = (source: EditorRead, outcome: ResourceReadOutcome) => {
    if (outcome === "loaded") refusedEditorReads.current.delete(source);
    else if (refusedEditorReads.current.has(source)) {
      // Remounting the editor asks this child again. A second refusal supersedes the page read without starting another one.
      close();
    } else {
      refusedEditorReads.current.add(source);
      refreshRefusal("read");
    }
  };

  const roomListRead = useCallback((outcome: ResourceReadOutcome) => {
    if (outcome === "loaded") roomListRefused.current = false;
    else if (!roomListRefused.current) {
      // This list opens by itself after a page read. A second refusal stays with the list.
      roomListRefused.current = true;
      refreshRefusal("read");
    }
  }, [refreshRefusal]);

  // A roster write holds that date's Edit until the page shows a read asked for after the write.
  const settling = useRosterSettled({ epoch: load.epoch, dataEpoch: load.dataEpoch, reload: () => refreshDates() }, (eventId) => eventId === Number(id));

  const dateRefused = useRefusedSave(load);
  const error = load.error && !missing ? errorText(load.error, "We couldn't load this event.") : null;
  const setPage = load.setData;

  // After a date changes, here or from another page: the dates that need a room are asked for again with the
  // event, since a moved date can lose its booking (NeedsRoomList keeps its own request).
  const refreshDates = () => {
    if (!credentialCurrent(pin)) return;
    setRoomsRevision((n) => n + 1);
    void load.reload();
  };

  // Every question on this page is about what it shows: any change to the loaded event, its dates or what
  // this person may do (a refresh, a sibling's answer) makes a pending question stale, even if it changes back.
  usePageFacts(useMemo(() => JSON.stringify(page), [page]));
  const beginDelete = useConfirm();
  useLeaveQuestion();

  function handleRsvp() { if (credentialCurrent(pin)) void load.reload(); }

  function parkFocus() {
    returnToEdit.current = "";

    if (editorRef.current?.contains(document.activeElement)) document.getElementById("main")?.focus({ preventScroll: true });
  }

  // A save in the panel (the series editor or the visibility control) was refused and not sent again: the panel closes
  // on its own (DEC-161, DEC-163).
  function closeOnConflict(why: RevisionConflict) {
    parkFocus();
    closeEditors();
    setRoomsRevision((n) => n + 1);
    dateRefused.clear();
    refusedSave.refused(why);
  }

  // A date editor's save was refused and not sent again: the row closed it, and the page reads from nothing and says so.
  function dateConflict(why: RevisionConflict, eventId: number, date: string) {
    setRoomsRevision((n) => n + 1);
    refusedSave.clear();
    setRefusedDate({ eventId, date });
    dateRefused.refused(why);
  }

  function reportSeriesFeedback(eventId: number, date: string, actorResults: ActorRsvpResult[]) {
    if (!credentialCurrent(pin)) return;
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
        {managerStatus}
        <Skeleton kind="page" />
      </div>
    );
  }

  if (missing) {
    return <Missing title="Event not found" body="It may have been cancelled or removed, or it may be private." notice={<>{managerStatus}<RefusedSaveLine save={refusedSave} noun="event" /><RefusedSaveLine save={dateRefused} noun="date" /></>} />;
  }

  if (!page || !pin) {
    return (
      <div className="fold-page fold-page-narrow">
        {managerStatus}
        <h1 className="text-3xl font-bold mb-6">Event unavailable</h1>
        <RefusedSaveLine save={refusedSave} noun="event" />
        <RefusedSaveLine save={dateRefused} noun="date" />
        <ErrorNotice message={error ?? (pin ? "We couldn't load this event." : "We couldn't check your session.")} onRetry={retryEvent} />
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

    try {
      const consequence = `This removes every date and its registrations. ${await signedUpNotice(event.id)}`;

      if (!(await action.ask({ title: question, body: consequence, confirmLabel: "Delete event", tone: "danger", native: `${question} ${consequence}` })) || !credentialCurrent(pin)) return;
      setDeleteError(null);
      await Events.delete(event.id);

      // Only the page that asked moves on: someone who went elsewhere meanwhile stays there.
      if (action.here()) navigate("/", { replace: true });
    } catch (e) {
      if (!action.here()) return;

      if (resourceRefused(e)) refreshRefusal("write");
      else setDeleteError(errorText(e, "We couldn't delete the event."));
    } finally {
      deletingNow.current = false;
      setDeleting(false);
    }
  }

  return (
    <EventReadPin.Provider value={{ pin: [pin], close }}><ResourceRefusal.Provider value={refreshRefusal}><div className="fold-page fold-page-narrow">
      {managerStatus}
      {/* Edit series and More sit beside the title as secondary actions; an event page has no primary one (DEC-63, DEC-65). */}
      <div className="fold-page-header">
        <div className="min-w-0">
          <h1 className="text-3xl font-bold mb-2 wrap-anywhere">{title}</h1>
          <EventIntro hostedBy={event.hostedBy} groupName={event.groupName} groupId={event.groupId} churchWide={event.churchWide} />
        </div>
        {canManage && (
          <div className="flex w-full gap-2 sm:w-auto sm:shrink-0">
            <Button ref={editButton} variant="secondary" layout="flex-1 sm:flex-none" aria-expanded={editing} disabled={panelBusy} onClick={() => {
              refusedSave.clear();
              setPanelUpdated(false);
              setUpdatedDateId(null);

              if (editing) closeEditors();
              else setEditing(true);
            }}>
              {event.recurrenceRule ? "Edit series" : "Edit event"}
            </Button>
            <Menu label="More event actions" items={[
              ...(canCompose ? [{ label: EMAIL_GOING, onSelect: () => navigate(composePath({ event: event.id })) }] : []),
              { label: "Delete event", onSelect: () => void deleteEvent(), danger: true, disabled: deleting },
            ]} />
          </div>
        )}
      </div>
      <RefusedSaveLine save={refusedSave} noun="event" canManage={canManage} />
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
        <div ref={attachEditor} className="mt-4">
          <SeriesEditor
            series={{ ...event, eventId: event.id }}
            hold={seriesDraft}
            idPrefix={`ev${event.id}`}
            locationHint={event.churchWide ? undefined : "Leave it blank to use the group's location."}
            roomDate={single ? next.date : undefined}
            onRoomChanged={() => { setRoomsRevision((n) => n + 1); void load.reload(); }}
            onRoomRead={(outcome) => editorRead("room", outcome)}
            onSaved={() => {
              parkFocus();
              setPanelUpdated(true);

              if (!visibilityDraft.current) closeEditors();
              setRoomsRevision((n) => n + 1);
              void load.retry();
            }}
            onConflict={closeOnConflict}
            onClose={() => {
              closedPanel.current = true;
              closeEditors();
            }}
            action={seriesAction}
            locked={visibilityAction.running}
          />
          {canSetVisibility && (
            <EventVisibilityControl
              // A new audience refreshes the control. An unsaved choice still names the read it opened with.
              key={`${event.id}-${event.visibility}-${event.leaderOpened}-${event.groupVisibility ?? ""}`}
              eventId={event.id}
              hold={visibilityDraft}
              churchWide={event.churchWide}
              groupName={event.groupName}
              groupVisibility={event.groupVisibility}
              visibility={event.visibility}
              leaderOpened={event.leaderOpened}
              revision={event.revision}
              onRead={(outcome) => editorRead("visibility", outcome)}
              onSaved={() => {
                parkFocus();
                setPanelUpdated(true);
                void load.retry();
              }}
              onConflict={closeOnConflict}
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
          onGone={() => closeOnConflict("gone")}
          onRead={roomListRead}
          onRecheckPage={() => refreshRefusal("read")}
        />
      )}

      {canManage && page.restorableDates?.map((date) => <RestoreDate key={date.revision} date={date} eventId={event.id}
        onChanged={() => { setRestoredDate(date); refreshDates(); }} onRefused={(why) => refusedSave.refused(why, "action")} />)}
      {canManage && <div role="status" aria-atomic="true">
        {restoredDate && <p className="mt-2">
          You brought back {formatEventDay(restoredDate.startsAt)} at {formatTime(restoredDate.startsAt)}.{" "}
          <ButtonLink variant="link" to={`/events/${event.id}/dates/${restoredDate.date}`}>View date</ButtonLink>
        </p>}
      </div>}
      {!single && <h2 className="text-xl font-semibold mt-6 mb-4">Upcoming dates</h2>}
      {householdError && <ErrorNotice message={householdError} onRetry={retryHousehold} />}
      <ManagerSaveStatus saving={false} updated={updatedDateId !== null} updateKey={updatedDateId} />
      {/* Above the list, never in a row a long list may hide: focus brings it into view, and it names the day. */}
      {refusedDate && <RefusedSaveLine save={dateRefused} noun="date" about={formatDayFull(refusedDate.date)}
        canManage={occurrences.some((o) => o.eventId === refusedDate.eventId && o.date === refusedDate.date) ? canManage : undefined} />}
      {occurrences.length === 0 && (
        <EmptyState title="No upcoming dates." body="There are no dates to show in the next 90 days." />
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
            settlingDate={settling(m.eventId, m.date)}
            pageFailed={load.error != null}
            editRequest={pickRequest?.date === m.date ? pickRequest.seq : 0}
            onRsvp={handleRsvp}
            people={people}
            seriesFeedback={seriesFeedback}
            mutationGate={rsvpMutationGate}
            onSeriesFeedback={reportSeriesFeedback}
            onTerminalRefresh={() => void load.retry()}
            onChanged={refreshDates}
            onDateUpdated={(updated) => setUpdatedDateId(updated ? m.id : null)}
            onEditDate={() => {
              dateRefused.clear();
              setUpdatedDateId(null);
              setPanelUpdated(false);
            }}
            onDateConflict={(why) => dateConflict(why, m.eventId, m.date)}
            onNeedLogin={() => navigate("/login", { state: { from: `/events/${id}` } })}
          />}
        </CompactDates>
      </div>
    </div></ResourceRefusal.Provider></EventReadPin.Provider>
  );
}

function RestoreDate({ date, eventId, onChanged, onRefused }: {
  date: RestorableDate;
  eventId: number;
  onChanged: () => void;
  onRefused: (why: RevisionConflict) => void;
}) {
  const deadline = Date.parse(date.startsAt);
  const future = useBeforeDeadline(deadline);
  const begin = useConfirm(future ? date.revision : null);
  const restore = useAction();

  if (!future) return null;

  const day = formatEventDay(date.startsAt);
  const title = `Bring back ${day} at ${formatTime(date.startsAt)}?`;
  const body = "People who can still attend keep their sign-ups. We'll tell those going or on the waitlist that the date is back on.";

  function bringBack() {
    const action = begin();
    void restore.run(async () => {
      if (!(await action.ask({ title, body, tone: "normal", confirmLabel: "Bring this date back", cancelLabel: "Keep cancelled", native: `${title} ${body}` })) || Date.now() >= deadline) return false;

      try {
        await Events.restoreOccurrence(eventId, date.date, date.revision);
      } catch (cause) {
        if (!(cause instanceof ApiError) || (cause.status !== 409 && !resourceRefused(cause))) throw cause;

        if (action.here()) onRefused(cause.status === 409 ? "changed" : "gone");

        return false;
      }

      if (action.here()) onChanged();
    }, { fallback: "We couldn't bring this date back." });
  }

  return <div className="fold-list-row">
    <p className="font-medium">{day} · {formatTime(date.startsAt)} · Cancelled</p>
    <Button variant="secondary" layout="mt-2" disabled={restore.running || restore.state.status === "done"} onClick={bringBack}>Bring this date back</Button>
    {restore.state.status === "failed" && <FormError>{restore.state.text}</FormError>}
  </div>;
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
  settlingDate,
  pageFailed,
  editRequest,
  onRsvp,
  people,
  seriesFeedback,
  mutationGate,
  onSeriesFeedback,
  onTerminalRefresh,
  onChanged,
  onDateUpdated,
  onEditDate,
  onDateConflict,
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
  // A roster write on this date ended and the page does not show an answer asked for after it (useRosterSettled).
  settlingDate: boolean;
  // While the page's latest read failed, the editor shows nothing of the date.
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
  onDateUpdated: (updated: boolean) => void;
  // The date's editor opened, which clears what the page said about its last refused save.
  onEditDate: () => void;
  onDateConflict: (why: RevisionConflict) => void;
  onNeedLogin: () => void;
}) {
  const navigate = useNavigate();
  const { canCompose } = useAuth();
  const dateEditor = useDateEditorToggle({ onEditDate, onChanged, onDateUpdated, onDateConflict, onTerminalRefresh });
  const refreshRefusal = useResourceRefusal();
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
      dateEditor.show();
      setFocusRoom(true);
    }
  }, [editRequest]);
  useEffect(() => {
    if (editRequest === 0 || editRequest === handledRequest.current || !dateEditor.open) return;
    handledRequest.current = editRequest;
    const editor = editorRef.current;
    editor?.scrollIntoView?.({ block: "start" });
  }, [editRequest, dateEditor.open]);
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
    const told = await signedUpNotice(instance.eventId, instance.date);

    if (!(await action.ask({ title: question, body: told, confirmLabel: "Cancel this date", cancelLabel: "Keep this date", tone: "danger", native: `${question} ${told}` }))) {
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
      if (!refreshRefusal(e)) setCancelError(errorText(e, "We couldn't cancel that date."));
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
          { label: "Edit this date", onSelect: () => { setFocusRoom(false); dateEditor.toggle(); }, disabled: settlingDate || dateEditor.saving },
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
        {dateEditor.status}
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
        {canManage && dateEditor.open && (
          <div ref={editorRef} className="scroll-mt-4">
          <ManagedDateEditor
            opener={{ eventId: instance.eventId, date: instance.date, occurrenceId: instance.id }}
            pageFailed={pageFailed}
            focusRoom={focusRoom}
            {...dateEditor.editor}
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

  const refreshRefusal = useResourceRefusal();

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
      if (!refreshRefusal(err)) setError(errorText(err, "We couldn't add that host."));
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
      if (!refreshRefusal(err)) setError(errorText(err, "We couldn't remove that host."));
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

