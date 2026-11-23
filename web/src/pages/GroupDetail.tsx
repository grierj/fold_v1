import { Tabs } from "../components/ui/Tabs";
import { ManagedDateEditor } from "../components/ManagedDateEditor";
import { ConfirmProvider } from "../components/ui/ConfirmDialog";
import { AddPeople } from "../components/AddPeople";
import { RequestContactButton } from "../components/RequestContactButton";
import { ContactDetails } from "../components/ContactDetails";
import { Badge } from "../components/ui/Badge";
import { useHousehold } from "../lib/useHousehold";
import { canDemoteLeader, canMakeLeader, canAskForContact, canRegisterAsGuest, canRemoveMember, groupCanSchedule, isActiveAdult } from "../lib/can";
import { SeriesEditor } from "../components/SeriesEditor";
import { useEffect, useLayoutEffect, useRef, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { ApiError } from "../api/core";
import { Events } from "../api/events";
import { GroupCover, Groups } from "../api/groups";
import type { DepartureConfirmation, EventOccurrence, GroupDetailResponse, OpenedDecision, HouseholdPerson, Visibility, VisibilityImpact } from "../types";
import { useAuth } from "../auth";
import { OccurrenceCard } from "../components/OccurrenceCard";
import { PeopleList } from "../components/PeopleList";
import { CompactDates } from "../components/CompactDates";
import { Attendance } from "../components/Attendance";
import { Menu } from "../components/ui/Menu";
import { composePath, EMAIL_GOING, EMAIL_MEMBERS } from "../components/messages/composeLinks";
import { placeLine } from "../lib/place";
import { EventVisibilityControl } from "../components/EventVisibility";
import { ROSTER_STALE, visibilityRank } from "../lib/visibilityChange";
import { useFocusHome, useKeepFocusHome } from "../lib/focusHome";
import { decisionImpact, reviewOutdated } from "../lib/visibilityConsent";
import { GroupVisibilityReview } from "../components/GroupVisibilityReview";
import { GroupDepartureDialog } from "../components/GroupDepartureDialog";
import { RsvpButtons } from "../components/RsvpButtons";
import { GuestRegistrationForm } from "../components/GuestRegistrationForm";
import { Avatar } from "../components/Avatar";
import { coverSrc } from "../lib/covers";
import { CalendarIcon } from "../components/icons";
import { ErrorNotice } from "../components/ErrorNotice";
import { Missing } from "../components/Missing";
import { GroupForm, type GroupFormData } from "../components/GroupForm";
import { EmptyState } from "../components/ui/EmptyState";
import { Illustration } from "../components/ui/Illustration";
import { Skeleton } from "../components/ui/Skeleton";
import { focalPosition, prepareUpload } from "../lib/image";
import { formatTime, formatWhen } from "../lib/when";
import { errorText } from "../lib/errors";
import { useLoad } from "../lib/useLoad";
import { useAction } from "../lib/useAction";
import { FormError, HiddenFileInput } from "../components/ui/Field";
import { Button, ButtonLink } from "../components/ui/Button";
import { applyActorRsvpResults, type ActorRsvpResult, type SeriesFeedbackSignal } from "../lib/rsvpFeedback";
import { useRsvpMutationGate, type RsvpMutationGate } from "../lib/useRsvpMutationGate";
import { useDateAttempts, useDateWrites, useRosterActivityOf } from "../lib/dateWrites";
import { dateKey, useEditRefresh } from "../lib/editBase";
import { seriesSchedules } from "../lib/registration";

type Tab = "events" | "about" | "members";


function EventRow({
  instance,
  seriesDates,
  groupName,
  groupVisibility,
  canManage,
  showDescription,
  moreDates,
  totalKnown,
  loggedIn,
  onRsvp,
  people,
  seriesFeedback,
  mutationGate,
  onSeriesFeedback,
  onTerminalRefresh,
  onCancelled,
  onChanged,
  writingDate,
  settlingDate,
  groupId,
  generation,
  pageFailed,
}: {
  instance: EventOccurrence;
  // Every loaded date of this series, though only the next is shown (seriesSchedules).
  seriesDates: string;
  groupName: string;
  groupVisibility: Visibility;
  canManage: boolean;
  showDescription: boolean;
  // A series shows its next date only, plus a link to the rest (DEC-63).
  // moreDates counts the loaded rest; totalKnown says the loaded dates are all of them.
  moreDates: number;
  totalKnown: boolean;
  loggedIn: boolean;
  onRsvp: () => void;
  people: HouseholdPerson[];
  seriesFeedback: SeriesFeedbackSignal | null;
  mutationGate: RsvpMutationGate;
  onSeriesFeedback: (eventId: number, date: string, actorResults: ActorRsvpResult[]) => void;
  onTerminalRefresh: () => void;
  onCancelled: (eventId: number, date: string) => void;
  onChanged: () => void;
  // A roster write on this date is on its way, or answered and this page does not show it yet
  // (GroupDetailContent's dateWrites).
  writingDate: boolean;
  settlingDate: boolean;
  // The group this page is for, which the date must still belong to, and the page's shown answer: the date's
  // editor reads the date again for each.
  groupId: number;
  generation: number;
  // The page's latest read failed: the editor shows nothing of the date until one succeeds.
  pageFailed: boolean;
}) {
  const navigate = useNavigate();
  const { canCompose } = useAuth();
  const [cancelling, setCancelling] = useState(false);
  const [cancelError, setCancelError] = useState<string | null>(null);
  const [editor, setEditor] = useState<null | "date" | "series">(null);
  // This date's guarded edit: anything but idle closes Edit; a save whose answer never arrived is reached
  // through its own opener, outside the menu.
  const attempts = useDateAttempts();
  const editStatus = attempts?.status(dateKey(instance.eventId, instance.date)) ?? "idle";
  const time = formatTime(instance.startsAt);
  const loc = placeLine(instance.room, instance.location);
  const name = instance.title || groupName;

  async function cancel() {
    if (!confirm(`Cancel this date on ${instance.date}?`)) return;
    setCancelling(true);
    setCancelError(null);

    try {
      await Events.cancelOccurrence(instance.eventId, instance.date);
      onCancelled(instance.eventId, instance.date);
    } catch (e) {
      setCancelError(errorText(e, "We couldn't cancel that date."));
    } finally {
      setCancelling(false);
    }
  }

  return (
    <OccurrenceCard at={instance.startsAt} title={name}
      notice={canManage && instance.visibilityOverride && <p className="text-xs text-text-muted">Visibility changed for this date</p>}
      meta={<div className="text-sm text-text-muted">{time}{loc ? ` · ${loc}` : ""}</div>}
      menu={canManage && <Menu
        tone="quiet"
        label={`More for ${name} on ${instance.date}`}
        items={[
          { label: "Edit this date", onSelect: () => setEditor(editor === "date" ? null : "date"), disabled: writingDate || settlingDate || editStatus !== "idle" },
          // The event page opens its Manage sign-ups panel for this date (DEC-99).
          { label: "Manage sign-ups", onSelect: () => navigate(`/events/${instance.eventId}?manage=${instance.date}`) },
          ...(canCompose ? [{ label: EMAIL_GOING, onSelect: () => navigate(composePath({ occurrence: instance.id })) }] : []),
          { label: instance.recurrenceRule ? "Edit series" : "Edit event", onSelect: () => setEditor(editor === "series" ? null : "series") },
          { label: cancelling ? "Cancelling…" : "Cancel this date", onSelect: () => void cancel(), danger: true, disabled: cancelling },
        ]}
      />}
    >
        {showDescription && instance.description && <p className="text-sm text-text-muted whitespace-pre-wrap">{instance.description}</p>}
        {cancelError && <FormError>{cancelError}</FormError>}
        {instance.overridden && <div className="text-sm text-text-muted">This date was edited on its own, so series changes leave it alone.</div>}
        <Attendance instance={instance} />
        {loggedIn && (
          <div className="mt-2">
            <RsvpButtons instance={instance} seriesDates={seriesDates} people={people} onChange={onRsvp} seriesFeedback={seriesFeedback}
              mutationGate={mutationGate} onSeriesFeedback={onSeriesFeedback} onTerminalRefresh={onTerminalRefresh} />
          </div>
        )}
        {canRegisterAsGuest(loggedIn, instance.visibility) && (
          <div className="mt-2">
            <GuestRegistrationForm instance={instance} />
          </div>
        )}
        {/* Below the answer controls (DEC-99). "See all N" only when the page holds every
            date; a truncated list or an open-ended series says "See more dates". */}
        {moreDates > 0 && (
          <Link to={`/events/${instance.eventId}`} className="inline-flex min-h-11 items-center text-sm font-medium text-primary">
            {totalKnown ? `See all ${moreDates + 1} dates` : "See more dates"}
          </Link>
        )}
        {canManage && editor !== "date" && editStatus === "unsure" && (
          <Button variant="secondary" layout="w-full sm:w-auto" onClick={() => setEditor("date")}>Check your last change to this date</Button>
        )}
        {/* The date's own question asks in the app's dialog; every other question on this page stays the browser's. */}
        {canManage && editor === "date" && <ConfirmProvider><ManagedDateEditor opener={{ eventId: instance.eventId, date: instance.date, occurrenceId: instance.id, groupId }} generation={generation} pageFailed={pageFailed}
          onSaved={() => {}} onRoomChanged={onChanged} onClose={() => setEditor(null)}
          onRefused={() => {
            setEditor(null);
            onTerminalRefresh();
          }} /></ConfirmProvider>}
        {editor === "series" && (
          <>
            <SeriesEditor
              series={{
                eventId: instance.eventId,
                startsLocal: instance.seriesStartsLocal,
                durationMinutes: instance.seriesDurationMinutes,
                tzid: instance.tzid,
                title: instance.seriesTitle,
                description: instance.description,
                location: instance.seriesLocation,
                recurrenceRule: instance.recurrenceRule,
                rruleUntil: instance.rruleUntil,
                capacity: instance.capacity,
                waitlistEnabled: instance.waitlistEnabled,
                guestsAllowed: instance.guestsAllowed,
                reminderEmailMinutes: instance.reminderEmailMinutes,
                reminderPush: instance.reminderPush,
                childcareAvailable: instance.childcareAvailable, childcareCapacity: instance.childcareCapacity,
                childcareAges: instance.childcareAges, childcareNotes: instance.childcareNotes,
                remoteAvailable: instance.remoteAvailable,
                remoteCapacity: instance.remoteCapacity,
                videoProvider: instance.videoProvider,
                videoUrl: instance.videoUrl,
                videoReveal: instance.videoReveal,
              }}
              idPrefix={`${instance.id}`}
              locationHint="Leave it blank to use the group's location."
              roomDate={instance.recurrenceRule ? undefined : instance.date}
              onRoomChanged={onChanged}
              onSaved={onChanged}
              onClose={() => setEditor(null)}
            />
            <EventVisibilityControl
              key={`${instance.eventId}-${instance.visibility}-${instance.leaderOpened}`}
              eventId={instance.eventId}
              groupName={groupName}
              groupVisibility={groupVisibility}
              visibility={instance.seriesVisibility ?? instance.visibility ?? groupVisibility}
              leaderOpened={instance.leaderOpened}
              onSaved={onChanged}
            />
          </>
        )}
    </OccurrenceCard>
  );
}

// The description above the tabs, clamped to three lines. "Read more" shows
// only when the text really overflows, and is re-checked when the width
// changes (rotating a phone, resizing the window).
function ClampedDescription({ text, onReadMore }: { text: string; onReadMore: () => void }) {
  const ref = useRef<HTMLParagraphElement>(null);
  const [overflows, setOverflows] = useState(false);
  useLayoutEffect(() => {
    const el = ref.current;

    if (!el) return;
    const check = () => setOverflows(el.scrollHeight > el.clientHeight);
    check();

    if (typeof ResizeObserver !== "undefined") {
      const observer = new ResizeObserver(check);
      observer.observe(el);

      return () => observer.disconnect();
    }

    window.addEventListener("resize", check);

    return () => window.removeEventListener("resize", check);
  }, [text]);

  return (
    <div className="mb-6 max-w-3xl">
      <p ref={ref} className="text-text-muted whitespace-pre-wrap wrap-anywhere line-clamp-3">{text}</p>
      {overflows && (
        <Button variant="link" onClick={onReadMore}>
          Read more
        </Button>
      )}
    </div>
  );
}

export function GroupDetail() {
  const { id } = useParams<{ id: string }>();
  const { user, loading } = useAuth();

  // A new group or viewer must never inherit dates, cursors or pending loads.
  return <GroupDetailContent key={`${id}:${loading}:${user?.id}:${user?.role}:${user?.status}:${user?.accountType}`} />;
}

// Dates keyed by id, soonest first; a date in both lists keeps the newer row.
function mergeDates(old: EventOccurrence[], fresh: EventOccurrence[]): EventOccurrence[] {
  const dates = new Map(old.map((date) => [date.id, date]));

  for (const date of fresh) dates.set(date.id, date);

  return [...dates.values()].sort((a, b) => Date.parse(a.startsAt) - Date.parse(b.startsAt) || a.id - b.id);
}

// The group with its first page of dates, then as many more pages as were
// already on screen (`loaded`), so a refresh after an RSVP or edit renews every
// shown date, not just the first. Continuation metadata belongs to the last
// page fetched; the rows describe all loaded dates, not a series total.
async function fetchGroup(id: string, loaded: number): Promise<GroupDetailResponse> {
  let next = await Groups.get(id);
  let dates = mergeDates([], next.upcomingEvents);

  while (next.hasMore && next.nextCursor && dates.length < loaded) {
    next = await Groups.get(id, { cursor: next.nextCursor });
    dates = mergeDates(dates, next.upcomingEvents);
  }

  return { ...next, upcomingEvents: dates };
}

// The events a group review lists, whose sign-up updates withdraw it.
const impactEventIds = (impact: VisibilityImpact) => [...impact.tightened, ...impact.leaderOpened].map((e) => e.id);

function GroupDetailContent() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const { user, loading: authLoading, isAdmin, canCompose } = useAuth();
  const { people, error: householdError, retry: retryHousehold } = useHousehold(user);
  const [tab, setTab] = useState<Tab>("events");
  const sectionsRef = useRef<HTMLDivElement>(null);
  const [editing, setEditing] = useState(false);
  // `mark` is the session roster sequence the request behind the review was sent at.
  const [review, setReview] = useState<{ data: GroupFormData; impact: VisibilityImpact; mark: number } | null>(null);
  // One run for Save group (its preview and save) and the review's confirm, so neither starts while the other runs.
  const groupSaveAction = useAction();
  const [reviewError, setReviewError] = useState<string | null>(null);
  const groupSave = useRef<HTMLButtonElement>(null);
  // Holds the review and its lines while editing; keeps keyboard focus when a sign-up update withdraws a review.
  const reviewHome = useFocusHome<HTMLDivElement>();
  useKeepFocusHome(reviewHome);
  const activity = useRosterActivityOf();
  const reviewCommitting = useRef(false);
  const [joinBusy, setJoinBusy] = useState(false);
  const [coverBusy, setCoverBusy] = useState(false);
  const [memberBusy, setMemberBusy] = useState(false);
  const [memberError, setMemberError] = useState<string | null>(null);
  const [departureTarget, setDepartureTarget] = useState<{ subjectId?: number; allowNote: boolean } | null>(null);
  const [departureStatus, setDepartureStatus] = useState<string | null>(null);
  const [seriesFeedback, setSeriesFeedback] = useState<SeriesFeedbackSignal | null>(null);
  const seriesFeedbackRevision = useRef(0);
  const coverRef = useRef<HTMLInputElement>(null);
  const [loadingMore, setLoadingMore] = useState(false);
  // A failed "Load more" is shown until the page is asked for again (the epoch moves).
  const [moreFailure, setMoreFailure] = useState<{ text: string; epoch: number } | null>(null);
  const [joinError, setJoinError] = useState<string | null>(null);
  const [coverError, setCoverError] = useState<string | null>(null);
  // A refresh keeps as many dates as are on screen, not just the first page.
  const loadedDates = useRef(0);
  // A "Load more" is on its way: a refresh meanwhile reads its page too, from fresh cursors.
  const continuing = useRef(false);
  const rsvpMutationGate = useRsvpMutationGate();

  // `reload` keeps ordinary RSVP drafts mounted; `retry` (also what a
  // terminal refresh calls) drops the page at once, so stale authority cannot
  // reappear, and only the newest request may commit anything.
  const load = useLoad(() => fetchGroup(id ?? "", loadedDates.current + (continuing.current ? 1 : 0)), [id], { enabled: !!id });

  // The same hold on a date's update as the event page's (lib/dateWrites); questions here stay the browser's
  // own, except a date editor's, which asks in its own ConfirmProvider. This page lists every date of its group's events; an update whose group the
  // editor was not shown counts too, and so does a kept-dates panel's roster write inside one of its editors.
  const dateWrites = useDateWrites({
    epoch: load.epoch,
    dataEpoch: load.dataEpoch,
    shows: (target) => target.groupId === undefined || target.groupId === Number(id),
    reload: () => void load.reload(),
    rosters: true,
  });

  const detail = load.data ?? null;
  useEffect(() => { if (detail) loadedDates.current = detail.upcomingEvents.length; }, [detail]);
  // A guarded edit settled (from here or the event page): the page is asked for again. Any date may be one this
  // group lists, as a read on its way may hold it, so this is decided from the address, never from what is loaded.
  useEditRefresh(() => true, () => void load.reload());

  // A sign-up update on one of these events began, ended or is in flight since `mark`: a review's counts may
  // no longer be who is signed up. A write on any other event leaves the review alone.
  const moved = (mark: number, ids: number[]) => activity.changedSince(mark, ids) || activity.pendingFor(ids) > 0;

  // Withdraws the review (or never opens one) and says so; focus on the form or the review goes to the review's
  // home, so a keyboard user hears the line and Save group is the next stop.
  function staleReview() {
    const active = document.activeElement;
    const home = reviewHome.ref.current;

    if (!active || active === document.body || home?.contains(active) || groupSave.current === active) reviewHome.moveHere();
    setReview(null);
    setReviewError(ROSTER_STALE.group);
  }

  // A review is judged from its request's mark until a listener is armed: a write that began (and maybe ended)
  // after the request's last check, before this commit, is never shown and withdraws it. After that, a begin on
  // one of its events withdraws it. A commit on its way decides from its own answer.
  const reviewIds = review ? impactEventIds(review.impact) : null;
  const reviewMoved = review !== null && !reviewCommitting.current && moved(review.mark, reviewIds ?? []);
  useLayoutEffect(() => {
    if (!review) return;
    const ids = impactEventIds(review.impact);

    if (!reviewCommitting.current && moved(review.mark, ids)) {
      staleReview();

      return;
    }

    return activity.subscribe((event) => {
      if (event.end === undefined && ids.includes(event.eventId) && !reviewCommitting.current) {
        setReview(null);
        setReviewError(ROSTER_STALE.group);
      }
    });
  }, [activity, review]);

  const loading = load.loading;
  const notFound = load.error instanceof ApiError && load.error.status === 404;
  const error = load.error && !notFound ? errorText(load.error, "We couldn't load this group.") : null;
  const setDetail = load.setData;
  const moreError = moreFailure && moreFailure.epoch === load.epoch() ? moreFailure.text : null;
  const refresh = load.reload;

  // Adds the next page of dates to the ones on screen.
  async function loadMore() {
    if (!id || !detail?.hasMore || !detail.nextCursor) return;
    const cursor = detail.nextCursor;
    // The committed answer this cursor came from; a later answer (even with the same cursor) is not extended.
    const source = load.dataEpoch;
    const stamp = load.epoch();
    continuing.current = true;
    setLoadingMore(true);
    setMoreFailure(null);

    try {
      const next = await Groups.get(id, { cursor });

      // Anything asked for since (a refresh, a date's update answering) replaces this page.
      if (load.epoch() !== stamp) return;
      // A refresh asked before these dates are shown still reads them.
      loadedDates.current += next.upcomingEvents.length;
      // Only dates and paging come from the continuation: group, roster and authority stay the committed answer's.
      setDetail((old, shown) => old && shown === source && old.nextCursor === cursor ? {
        ...old,
        upcomingEvents: mergeDates(old.upcomingEvents, next.upcomingEvents),
        truncated: next.truncated,
        hasMore: next.hasMore,
        nextCursor: next.nextCursor,
      } : old);
    } catch (e) {
      if (load.epoch() === stamp) setMoreFailure({ text: errorText(e, "We couldn't load this group."), epoch: stamp });
    } finally {
      continuing.current = false;
      setLoadingMore(false);
    }
  }

  if (loading || authLoading) {
    return (
      <div className="fold-page">
        <Skeleton kind="cover" />
        <div className="mt-6">
          <Skeleton kind="lines" />
        </div>
      </div>
    );
  }

  if (notFound) {
    return (
      <Missing
        title="Group not found"
        body="It may have been removed, or it may be private. If someone invited you, ask them for the link again."
      />
    );
  }

  if (!detail) {
    return (
      <div className="fold-page-narrow">
        <h1 className="text-3xl font-bold mb-6">Group unavailable</h1>
        <ErrorNotice message={error ?? "We couldn't load this group."} onRetry={() => void load.retry()} />
      </div>
    );
  }

  const { group, members, upcomingEvents, canManage, isMember } = detail;
  const canSchedule = groupCanSchedule(group.status);

  const scheduleReason = group.status === "pending"
    ? "You can schedule events once an admin approves this group."
    : "This group is archived, so it can't schedule new events.";

  const events: EventOccurrence[] = upcomingEvents ?? [];
  // Count only loaded, visible dates: a capped page is not a series total.
  const nextDates: EventOccurrence[] = [];
  const datesPerEvent = new Map<number, number>();

  for (const m of events) {
    if (!datesPerEvent.has(m.eventId)) nextDates.push(m);
    datesPerEvent.set(m.eventId, (datesPerEvent.get(m.eventId) ?? 0) + 1);
  }

  const schedules = seriesSchedules(events);

  // Leadership comes from membership roles (several leaders allowed).
  const leaderCount = (members ?? []).filter((m) => m.role === "leader").length;
  const isLeader = !!user && (members ?? []).some((m) => m.id === user.id && m.role === "leader");

  // "Read more" under the clamped description opens About and moves focus to
  // that tab, so a keyboard or screen-reader user lands where the text is.
  function showAbout() {
    setTab("about");
    requestAnimationFrame(() => sectionsRef.current?.querySelector<HTMLElement>('[role="tab"][aria-selected="true"]')?.focus());
  }

  async function toggleJoin() {
    if (!user) {
      navigate("/login", { state: { from: `/groups/${id}` } });

      return;
    }

    if (isMember) {
      setDepartureStatus(null);
      setDepartureTarget({ allowNote: false });

      return;
    }

    setJoinBusy(true);

    try {
      setJoinError(null);
      await Groups.join(id!);
      await load.retry();
    } catch (e) {
      setJoinError(errorText(e, "That didn't work."));
    } finally {
      setJoinBusy(false);
    }
  }

  function departureResult(result: DepartureConfirmation) {
    return `${result.keptCount} ${result.keptCount === 1 ? "date" : "dates"} kept · ${result.endedCount} ${result.endedCount === 1 ? "date" : "dates"} ended.`;
  }

  async function completedDeparture(result: DepartureConfirmation) {
    setDepartureTarget(null);
    const summary = departureResult(result);

    if (result.action === "remove") {
      setDepartureStatus(summary);
      await refresh();

      return;
    }

    const outcome = await load.retry();

    if (outcome === "loaded") setDepartureStatus(summary);
    else if (outcome === "failed") navigate("/my-groups", { replace: true, state: { departureStatus: summary } });
  }

  function handleRsvp() { void refresh(); }

  function reportSeriesFeedback(eventId: number, date: string, actorResults: ActorRsvpResult[]) {
    const signal = { eventId, date, actorResults, revision: ++seriesFeedbackRevision.current };
    setDetail((current) => current ? {
      ...current,
      upcomingEvents: (current.upcomingEvents ?? []).map((instance) => applyActorRsvpResults(instance, eventId, actorResults)),
    } : current);
    setSeriesFeedback(signal);
  }

  function handleCancelled(eventId: number, date: string) {
    setDetail((d) =>
      d
        ? {
            ...d,
            upcomingEvents: (d.upcomingEvents ?? []).filter((m) => !(m.eventId === eventId && m.date === date)),
          }
        : d
    );
  }

  async function saveEdit(data: GroupFormData): Promise<void | false> {
    // Making the group more private can change its events (DEC-48): show
    // the leader the list and get a decision before anything is saved. The
    // save carries the preview's consent, so a change since the preview is
    // shown again from the server's own answer (DEC-158). A sign-up update on
    // one of the listed events while either was asked withdraws it.
    let impact: VisibilityImpact | null = null;
    setReviewError(null);

    if (visibilityRank(data.visibility) < visibilityRank(group.visibility)) {
      const mark = activity.mark();
      impact = await Groups.visibilityImpact(id!, data.visibility);

      if (moved(mark, impactEventIds(impact))) {
        staleReview();

        return false;
      }

      if (impact.tightened.length > 0 || impact.leaderOpened.length > 0) {
        setReview({ data, impact, mark });

        return false;
      }
    }

    const at = { mark: activity.mark() };

    try {
      await commitEdit(data, undefined, impact?.consent ?? "", at);
    } catch (e) {
      const fresh = decisionImpact(e);

      // A refusal this page cannot review again shows the outdated copy.
      if (!fresh && reviewOutdated(e)) throw new ApiError(409, "visibility review outdated", { code: "visibility_review_outdated" });

      if (!fresh) throw e;

      if (moved(at.mark, impactEventIds(fresh))) {
        staleReview();

        return false;
      }

      setReview({ data, impact: fresh, mark: at.mark });
      setReviewError("We checked again before saving. Please review the group's events again.");

      return false;
    }
  }

  async function confirmReview(decisions: Record<string, OpenedDecision>): Promise<false | void> {
    if (!review || reviewCommitting.current || activity.pendingFor(impactEventIds(review.impact)) > 0) return false;
    reviewCommitting.current = true;
    setReviewError(null);
    const at = { mark: activity.mark() };

    try {
      await commitEdit(review.data, decisions, review.impact.consent ?? "", at);
      setReview(null);

      return;
    } catch (e) {
      const fresh = decisionImpact(e);

      if (fresh && moved(at.mark, [...impactEventIds(fresh), ...impactEventIds(review.impact)])) {
        staleReview();
      } else if (fresh) {
        setReview({ data: review.data, impact: fresh, mark: at.mark });
        setReviewError("We checked again before saving. Please review the group's events again.");
      } else {
        // After a sign-up update on its events since the review's own request (a cover upload's await included)
        // the review is withdrawn too: the next Save previews again.
        if (moved(review.mark, impactEventIds(review.impact))) setReview(null);
        setReviewError(reviewOutdated(e) ?? errorText(e, "We couldn't save your changes."));
      }

      return false;
    } finally {
      reviewCommitting.current = false;
    }
  }

  // `at.mark` is taken again just before the update, so a 409's review is judged from the request that answered it.
  async function commitEdit(data: GroupFormData, openedEvents: Record<string, OpenedDecision> | undefined, consent: string, at: { mark: number }) {
    if (data.coverFile) {
      await GroupCover.upload(id!, data.coverFile);
    } else if (data.removeCover) {
      await GroupCover.remove(id!);
    }

    at.mark = activity.mark();
    await Groups.update(id!, {
      name: data.name,
      description: data.description,
      tags: data.tags,
      location: data.location,
      coverFocal: data.coverFocal || undefined,
      visibility: data.visibility,
      memberLimit: data.memberLimit,
      openedEvents,
      consent,
    });
    setEditing(false);
    setReviewError(null);
    await load.retry();
  }

  async function setRole(memberId: number, name: string, role: "leader" | "member") {
    const question = role === "leader"
      ? `Make ${name} a leader of “${group.name}”? Church admins will be notified.`
      : `Remove ${name} as a leader of “${group.name}”?`;

    if (!confirm(question)) return;
    setMemberBusy(true);
    setMemberError(null);

    try {
      await Groups.setRole(id!, memberId, role);
      await load.retry();
    } catch (e) {
      setMemberError(errorText(e, "We couldn't change that role."));
    } finally {
      setMemberBusy(false);
    }
  }

  function removeMember(memberId: number) {
    setMemberError(null);
    setDepartureStatus(null);
    setDepartureTarget({ subjectId: memberId, allowNote: true });
  }

  async function requestDeletion() {
    if (!confirm("Ask an admin to delete this group? It stays up until they approve.")) return;
    setMemberBusy(true);
    setMemberError(null);

    try {
      await Groups.requestDeletion(id!);
      await load.retry();
    } catch (e) {
      setMemberError(errorText(e, "We couldn't send the deletion request."));
    } finally {
      setMemberBusy(false);
    }
  }

  async function cancelDeletion() {
    setMemberBusy(true);
    setMemberError(null);

    try {
      await Groups.cancelDeletion(id!);
      await load.retry();
    } catch (e) {
      setMemberError(errorText(e, "We couldn't cancel the request."));
    } finally {
      setMemberBusy(false);
    }
  }

  async function uploadCover(file: File) {
    setCoverBusy(true);

    try {
      const resized = await prepareUpload(file, 1600);
      setCoverError(null);
      await GroupCover.upload(id!, resized);
      await load.retry();
    } catch (e) {
      setCoverError(errorText(e, "We couldn't upload the cover photo."));
    } finally {
      setCoverBusy(false);
    }
  }

  return (
    <div className="fold-page">
      {/* No overflow-hidden: group menus can extend beyond the photo header. */}
      <div className="fold-cover relative flex min-h-48 items-end rounded-lg bg-inverse sm:min-h-56">
        <img
          src={coverSrc(group)}
          alt=""
          className="absolute inset-0 w-full h-full rounded-lg object-cover"
          style={{ objectPosition: focalPosition(group.coverFocalX, group.coverFocalY) }}
        />
        {/* A uniform 75% inverse scrim keeps even white covers legible behind every line. */}
        <div className="absolute inset-0 rounded-lg bg-inverse/75" />
        <div className="relative w-full p-4 sm:p-6">
          <div className="flex flex-col gap-6 lg:flex-row lg:items-end lg:justify-between">
            <div className="grow min-w-0">
              {(group.status === "pending" || group.visibility !== "public") && (
                <div className="flex flex-wrap gap-2 mb-2">
                  {group.status === "pending" && (
                    <span className="inline-block text-sm font-semibold bg-warning-subtle text-warning-on-subtle rounded-full px-3 py-1">
                      Pending approval
                    </span>
                  )}
                  {group.visibility === "private" && (
                    <span className="inline-block text-sm font-semibold bg-surface/90 text-text rounded-full px-3 py-1">
                      Private
                    </span>
                  )}
                  {group.visibility === "invite_only" && (
                    <span className="inline-block text-sm font-semibold bg-inverse text-inverse-foreground rounded-full px-3 py-1">
                      Invite only
                    </span>
                  )}
                </div>
              )}
              <h1 className="text-3xl font-bold text-inverse-foreground wrap-anywhere">{group.name}</h1>
              <div className="mt-3 flex flex-wrap gap-x-6 gap-y-1 text-sm text-inverse-foreground">
                <span>{canManage && group.memberLimit
                  ? `${group.memberCount} of ${group.memberLimit} members`
                  : `${group.memberCount} ${group.memberCount === 1 ? "member" : "members"}`}</span>
                {group.leaderName && <span className="text-inverse-foreground">Organized by {group.leaderName}</span>}
                {group.location && <span>{group.location}</span>}
              </div>
            </div>
            <div className="flex shrink-0 flex-wrap gap-2">
              {canManage && (
                <>
                  {canSchedule && (
                    <ButtonLink to={`/groups/${group.id}/events/new`} variant="primary-inverse">
                      Schedule event
                    </ButtonLink>
                  )}
                  {/* One primary action once the group is active (Schedule event); the rest live in More (DEC-63, DEC-147). */}
                  <Menu
                    label="More group actions"
                    items={[
                      { label: "Edit group", onSelect: () => { groupSaveAction.reset(); setEditing(true); } },
                      ...(canCompose && group.status === "active" ? [{ label: EMAIL_MEMBERS, onSelect: () => navigate(composePath({ group: group.id })) }] : []),
                      { label: coverBusy ? "Uploading cover…" : "Change cover photo", onSelect: () => coverRef.current?.click(), disabled: coverBusy },
                      ...(isMember ? [{ label: "Leave group", onSelect: () => void toggleJoin(), disabled: !!departureTarget }] : []),
                    ]}
                  />
                  <HiddenFileInput
                    ref={coverRef}
                    accept="image/jpeg,image/png,image/gif"
                    onChange={(e) => {
                      const f = e.target.files?.[0];

                      if (f) uploadCover(f);
                      e.target.value = "";
                    }}
                  />
                </>
              )}
              {isMember && !canManage && (
                /* A plain member's cover offers "You're a member" with Leave group inside it,
                   the menu leaders use, so a stray tap can't start leaving (DEC-92; the
                   DEC-74 confirmation is unchanged). */
                <Menu
                  label="You're a member"
                  text="✓ You're a member"
                  items={[{ label: "Leave group", onSelect: () => void toggleJoin(), disabled: joinBusy || !!departureTarget }]}
                />
              )}
              {group.visibility !== "invite_only" && !isMember && (
                <Button variant="primary" onClick={toggleJoin} disabled={joinBusy || !!departureTarget}>
                  {joinBusy ? "…" : user ? "Join this group" : "Log in to join"}
                </Button>
              )}
            </div>
          </div>
        </div>
      </div>

      <div className="pt-6">
        {joinError && <div className="mb-4"><ErrorNotice message={joinError} /></div>}
        {coverError && <div className="mb-4"><ErrorNotice message={coverError} /></div>}
        {departureStatus && <p role="status" className="mb-4 rounded-lg border border-border bg-sunken px-4 py-3 text-sm">{departureStatus}</p>}
        {editing && (
          <div ref={reviewHome.ref} tabIndex={-1}>
            {reviewError && <FormError layout="mb-2">{reviewError}</FormError>}
            {review && !reviewMoved && <GroupVisibilityReview
              key={JSON.stringify(review.impact)}
              impact={review.impact}
              busy={groupSaveAction.running}
              waiting={activity.pendingFor(reviewIds ?? []) > 0}
              onConfirm={(d) => void groupSaveAction.run(() => confirmReview(d))}
              onCancel={() => {
                setReview(null);
                setReviewError(null);
              }}
              returnFocus={groupSave}
            />}
          </div>
        )}
        {editing ? (
          <GroupForm
            initial={{
              name: group.name,
              description: group.description,
              location: group.location,
              tags: group.tags,
              imageUrl: group.imageUrl,
              coverFocalX: group.coverFocalX,
              coverFocalY: group.coverFocalY,
              coverId: group.id,
              moreOpen: true,
              visibility: group.visibility,
              memberLimit: group.memberLimit,
            }}
            saveLabel="Save group"
            saveRef={groupSave}
            action={groupSaveAction}
            onSaved={saveEdit}
            onCancel={() => {
              groupSaveAction.reset();
              setEditing(false);
              setReview(null);
              setReviewError(null);
            }}
          />
        ) : (
          <>
            {group.description.trim() && tab !== "about" && (
              <ClampedDescription text={group.description} onReadMore={showAbout} />
            )}
            <div ref={sectionsRef}>
            <Tabs label="Group sections" value={tab} onChange={setTab} options={[
              { id: "events", label: "Upcoming events" },
              { id: "about", label: "About" },
              { id: "members", label: "Members" },
            ]}>
            {tab === "events" && (
              <div className="space-y-4">
                <h2 className="sr-only">Upcoming events</h2>
                {canManage && !canSchedule && events.length > 0 && <p className="text-text-muted">{scheduleReason}</p>}
                {events.length === 0 && (
                  <EmptyState
                    illustration={<Illustration name="no-events" />}
                    title={canManage && canSchedule ? "Schedule your group's first meeting" : "No upcoming events"}
                    body={
                      canManage
                        ? canSchedule
                          ? "It takes about a minute."
                          : scheduleReason
                        : "When a leader schedules something, it will show up here."
                    }
                    action={
                      canManage && canSchedule ? (
                        <ButtonLink to={`/groups/${group.id}/events/new`} variant="primary" layout="w-full sm:w-auto">
                          Schedule event
                        </ButtonLink>
                      ) : undefined
                    }
                  />
                )}
                {detail.truncated && (
                  <div className="flex flex-wrap items-center gap-x-2">
                    <span className="text-sm text-text-muted">Showing the next {events.length} dates.</span>
                    <ButtonLink variant="link" to={`/calendar?group=${group.id}`}>
                      See all dates in the calendar
                    </ButtonLink>
                  </div>
                )}
                {(error ?? moreError) && <ErrorNotice message={(error ?? moreError)!} onRetry={() => void refresh()} />}
                {householdError && <ErrorNotice message={householdError} onRetry={retryHousehold} />}
                <CompactDates dates={nextDates} loadedOnly={detail.hasMore} noun="events">
                  {(m) => <EventRow
                    key={`${m.eventId}-${m.date}`}
                    instance={m}
                    seriesDates={schedules.get(m.eventId) ?? ""}
                    groupName={group.name}
                    groupVisibility={group.visibility}
                    canManage={canManage}
                    showDescription
                    moreDates={(datesPerEvent.get(m.eventId) ?? 1) - 1}
                    totalKnown={!detail.hasMore && !detail.truncated && (!m.recurrenceRule || !!m.rruleUntil || /(^|;)COUNT=/i.test(m.recurrenceRule))}
                    loggedIn={!!user}
                    onRsvp={handleRsvp}
                    people={people}
                    seriesFeedback={seriesFeedback}
                    mutationGate={rsvpMutationGate}
                    onSeriesFeedback={reportSeriesFeedback}
                    onTerminalRefresh={() => void load.retry()}
                    onCancelled={handleCancelled}
                    onChanged={() => void refresh()}
                    writingDate={dateWrites.writing(m.eventId, m.date)}
                    settlingDate={dateWrites.settling(m.eventId, m.date)}
                    groupId={group.id}
                    generation={load.dataEpoch}
                    pageFailed={load.error != null}
                  />}
                </CompactDates>
                {detail.hasMore && <Button variant="secondary" disabled={loadingMore} onClick={() => void loadMore()}>{loadingMore ? "Loading dates…" : "Load more dates"}</Button>}
                {group.visibility === "public" && group.status === "active" && (
                  <div className="flex flex-wrap items-center gap-x-2">
                    <ButtonLink variant="link" to={`/cal/groups/${group.id}.ics`} reloadDocument>
                      Subscribe in your calendar
                    </ButtonLink>
                    <span className="text-sm text-text-muted">Adds this group&rsquo;s public events to your calendar app.</span>
                  </div>
                )}
              </div>
            )}

            {tab === "about" && (
              <div className="grid max-w-3xl gap-8 sm:grid-cols-2">
                <div>
                  <h2 className="font-bold mb-1">About this group</h2>
                  <p className="text-text-muted whitespace-pre-wrap">
                    {group.description || "No description yet."}
                  </p>
                </div>
                {group.location && (
                  <div>
                    <h2 className="font-bold mb-1">Location</h2>
                    <p className="text-text-muted">{group.location}</p>
                  </div>
                )}
                {group.tags.length > 0 && (
                  <div>
                    <h2 className="font-bold mb-1">Tags</h2>
                    <div className="flex flex-wrap gap-2">
                      {group.tags.map((t) => (
                        <Link
                          key={t}
                          to={`/?tag=${encodeURIComponent(t)}`}
                          className="inline-flex min-h-11 items-center rounded-lg bg-sunken px-3 py-2 text-sm text-text-muted hover:bg-border"
                        >
                          {t}
                        </Link>
                      ))}
                    </div>
                  </div>
                )}
                <div>
                  <h2 className="font-bold mb-1">Next event</h2>
                  <p className="text-text-muted">
                    {group.nextEventAt ? (
                      formatWhen(group.nextEventAt)
                    ) : (
                      <span className="inline-flex items-center gap-1">
                        <CalendarIcon className="h-4 w-4" />
                        No upcoming events
                      </span>
                    )}
                  </p>
                </div>
              </div>
            )}

            {tab === "members" && (
              <div className="max-w-3xl">
                {!user ? (
                  <p className="text-text-muted">
                    <Link to="/login" className="text-primary font-semibold">
                      Log in
                    </Link>{" "}
                    to see the member roster.
                  </p>
                ) : (
                  <>
                    {memberError && <FormError layout="mb-3">{memberError}</FormError>}
                    {canManage && <div className="mb-4"><AddPeople
                      groupId={group.id} groupName={group.name} memberCount={group.memberCount} memberLimit={group.memberLimit}
                      canSearch={isActiveAdult(user)} onAdded={async () => { await refresh(); }}
                    /></div>}
                    <PeopleList rows={members ?? []} noun="members" filters={[
                      { value: "leader", label: "Leaders", matches: (m) => m.role === "leader" },
                      { value: "member", label: "Members", matches: (m) => m.role === "member" },
                    ]}>
                        {(m) => (
                          <li key={m.id} className="fold-list-row">
                            <div className="flex items-center gap-3">
                              <Avatar
                                url={m.avatarUrl || undefined}
                                name={m.name}
                                focalX={m.avatarFocalX}
                                focalY={m.avatarFocalY}
                              />
                              <div className="grow min-w-0">
                                <div className="font-semibold">{m.name}
                                  {canManage && m.accountType === "teen" && <Badge>Teen</Badge>}
                                  {m.admin && (
                                    <span className="ml-2 inline-block rounded-full bg-primary-subtle px-2 py-0.5 text-xs font-semibold text-primary-on-subtle">
                                      Admin
                                    </span>
                                  )}
                                </div>
                                <div className="text-sm text-text-muted capitalize">{m.role}</div>
                                <ContactDetails email={m.email} phone={m.phone} />
                              </div>
                              {canManage && (
                                <Menu
                                  tone="quiet"
                                  label={`More for ${m.name}`}
                                  items={[
                                    ...(canMakeLeader(m)
                                      ? [{ label: "Make leader", onSelect: () => void setRole(m.id, m.name, "leader"), disabled: memberBusy }]
                                      : []),
                                    ...(canDemoteLeader(m, leaderCount, isAdmin)
                                      ? [{ label: "Remove as leader", onSelect: () => void setRole(m.id, m.name, "member"), disabled: memberBusy }]
                                      : []),
                                    ...(canRemoveMember(user, m, leaderCount, isAdmin)
                                      ? [{ label: "Remove from group", onSelect: () => removeMember(m.id), danger: true, disabled: memberBusy || !!departureTarget }]
                                      : []),
                                  ]}
                                />
                              )}
                            </div>
                            {canManage && isLeader && group.status === "active" && canAskForContact(user, m) && <div className="w-full"><RequestContactButton subjectUserId={m.id} subjectName={m.name} contextType="group" contextId={group.id} /></div>}
                          </li>
                        )}
                    </PeopleList>
                    {isLeader && (
                      <div className="mt-6 border-t border-border pt-4">
                        {group.memberCount <= 1 ? (
                          group.deletionRequested ? (
                            <div className="space-y-2">
                              <p className="text-sm text-text-muted">
                                An admin still has to approve deleting this group.
                              </p>
                              <Button variant="secondary" onClick={cancelDeletion} disabled={memberBusy}>
                                Cancel deletion request
                              </Button>
                            </div>
                          ) : (
                            <Button variant="danger-outline" onClick={requestDeletion} disabled={memberBusy}>
                              Request deletion
                            </Button>
                          )
                        ) : (
                          <p className="text-sm text-text-muted">
                            Hand leadership to someone else. You can ask an admin to delete the group once you are the only member.
                          </p>
                        )}
                      </div>
                    )}
                  </>
                )}
              </div>
            )}
            </Tabs>
            </div>
          </>
        )}
      </div>
      {departureTarget && (
        <GroupDepartureDialog
          groupId={id!}
          subjectId={departureTarget.subjectId}
          allowNote={departureTarget.allowNote}
          onCancel={() => setDepartureTarget(null)}
          onUnavailable={async () => { await refresh(); }}
          onCompleted={completedDeparture}
        />
      )}
    </div>
  );
}
