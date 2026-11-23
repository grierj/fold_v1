import { Form } from "./ui/Form";
import { useEffect, useId, useLayoutEffect, useRef, useState } from "react";
import { Events } from "../api/events";
import type { Visibility, VisibilityImpact } from "../types";
import { GroupVisibilityReview } from "./GroupVisibilityReview";
import { openedEventWarning } from "./EventIntro";
import { errorText } from "../lib/errors";
import { decisionImpact, reviewOutdated } from "../lib/visibilityConsent";
import { ROSTER_STALE, ROSTER_WAITING, visibilityRank } from "../lib/visibilityChange";
import { useRosterActivityOf } from "../lib/dateWrites";
import { useFocusHome, useKeepFocusHome } from "../lib/focusHome";
import { Checkbox, Choices, Field, FormError, Input, Radio } from "./ui/Field";
import { Button } from "./ui/Button";

const CHOICES: { value: Visibility; label: string; description: string }[] = [
  { value: "public", label: "Public", description: "Anyone can see it." },
  { value: "private", label: "Private", description: "Only signed-in people can see it." },
  { value: "invite_only", label: "Invite only", description: "Only the group's members can see it." },
];

// EventVisibilityControl sets who can see one event. Every event keeps its
// own value, copied from the group when it was created (DEC-48). A group
// event set wider than its group is marked as opened by a leader. A
// church-wide event is public or private. A change that cancels anyone, and
// every reset of its dates, is saved only with the consent of the impact the
// server showed (DEC-158). A review opens, and is confirmed, only when no
// sign-up update on the event began, ended or is in flight since the request
// it comes from was sent; while one is in flight Save waits.
export function EventVisibilityControl({
  eventId,
  groupName,
  groupVisibility,
  visibility,
  leaderOpened,
  churchWide,
  onSaved,
}: {
  eventId: number;
  groupName?: string;
  groupVisibility?: Visibility;
  visibility: Visibility;
  leaderOpened?: boolean;
  churchWide?: boolean;
  onSaved: () => void;
}) {
  const [value, setValue] = useState<Visibility>(visibility === "invite_only" && churchWide ? "public" : visibility);
  const [reason, setReason] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [resetOverrides, setResetOverrides] = useState(false);
  const [futureOverrides, setFutureOverrides] = useState(0);
  const saveButton = useRef<HTMLButtonElement>(null);
  // Keeps keyboard focus when a stale answer or a sign-up update closes the review, or disables Save under it.
  const home = useFocusHome<HTMLFormElement>();
  useKeepFocusHome(home);
  const activity = useRosterActivityOf();
  const waiting = activity.pendingFor([eventId]) > 0;
  const committing = useRef(false);
  const waitingId = useId();
  // Save disabled under the keyboard (a sign-up update began) hands focus to the form, never the page.
  useLayoutEffect(() => {
    if (waiting && document.activeElement === saveButton.current) home.moveHere();
  }, [waiting, home]);
  useEffect(() => {
    let live = true;
    Promise.resolve(Events.visibilityImpact(eventId, visibility)).then((impact) => { if (live) setFutureOverrides(impact?.futureOverrides ?? 0); }).catch(() => {});

    return () => { live = false; };
  }, [eventId, visibility]);
  // `mark` is the session roster sequence the request behind the review was sent at.
  const [review, setReview] = useState<{ impact: VisibilityImpact; mark: number } | null>(null);

  function show(impact: VisibilityImpact, mark: number) {
    setFutureOverrides(impact.futureOverrides ?? 0);
    setReview({ impact, mark });
  }

  // A sign-up update on this event began, ended or is in flight since `mark`: the answer may count people who
  // are no longer signed up, or miss some who are.
  const moved = (mark: number) => activity.changedSince(mark, [eventId]) || activity.pendingFor([eventId]) > 0;

  // Focus that was on this form (Save, the review's buttons) goes to its home; focus elsewhere (a roster the
  // person is using) stays where it is.
  function stale() {
    const active = document.activeElement;

    if (!active || active === document.body || home.ref.current?.contains(active)) home.moveHere();
    setReview(null);
    setError(ROSTER_STALE.event);
  }

  // A review is judged from its request's mark until a listener is armed: a write that began (and maybe ended)
  // after the request's last check, before this commit, is never shown and withdraws it. After that, a begin
  // withdraws it. A commit on its way decides from its own answer.
  const reviewMoved = review !== null && !busy && !committing.current && moved(review.mark);
  useLayoutEffect(() => {
    if (!review) return;

    if (!committing.current && moved(review.mark)) {
      stale();

      return;
    }

    return activity.subscribe((event) => {
      if (event.end === undefined && event.eventId === eventId && !committing.current) {
        setReview(null);
        setError(ROSTER_STALE.event);
      }
    });
  }, [activity, eventId, review]);

  // The server's preview decides, never the page's props: a reset always
  // shows what it changes unless nothing changes at all, and a more private
  // choice shows who it cancels (DEC-48).
  async function save(e: React.FormEvent) {
    e.preventDefault();

    if (busy || activity.pendingFor([eventId]) > 0) return;
    setError(null);

    if (resetOverrides || visibilityRank(value) < visibilityRank(visibility)) {
      let impact: VisibilityImpact;
      const mark = activity.mark();

      try {
        impact = await Events.visibilityImpact(eventId, value, resetOverrides);
      } catch (err) {
        setError(errorText(err, "We couldn't check this event."));

        return;
      }

      if (moved(mark)) {
        stale();

        return;
      }

      setFutureOverrides(impact.futureOverrides ?? 0);
      const changes = impact.tightened.length > 0 || (resetOverrides && ((impact.resetDates ?? []).length > 0 || impact.currentVisibility !== impact.visibility));

      if (changes) {
        show(impact, mark);

        return;
      }

      await commit(impact.consent ?? "");

      return;
    }

    await commit("");
  }

  async function commit(consent: string) {
    if (committing.current || activity.pendingFor([eventId]) > 0) return;
    committing.current = true;
    const mark = activity.mark();
    setBusy(true);
    setError(null);

    try {
      await Events.setVisibility(eventId, { visibility: value, reason: reason.trim() || undefined, consent, resetOverrides });
      setReview(null);
      onSaved();
    } catch (err) {
      const fresh = decisionImpact(err);

      if (fresh && moved(mark)) {
        stale();

        return;
      }

      // Any other answer after a sign-up update withdraws the review too: the next Save previews again.
      if (moved(mark)) setReview(null);

      if (fresh) {
        show(fresh, mark);
        setError("We checked again before saving. Please review the changes below.");

        return;
      }

      setError(reviewOutdated(err) ?? errorText(err, "We couldn't change who can see this."));
    } finally {
      committing.current = false;
      setBusy(false);
    }
  }

  return (
    <Form ref={home.ref} tabIndex={-1} noValidate onSubmit={save} className="mt-3 space-y-3 border-t border-border pt-3">
      {error && <FormError>{error}</FormError>}
      <VisibilityField
        id={`event-visibility-${eventId}`}
        value={value}
        onChange={setValue}
        churchWide={churchWide}
        groupName={groupName}
        groupVisibility={groupVisibility}
        leaderOpened={leaderOpened}
        reason={reason}
        onReason={setReason}
      />
      {futureOverrides > 0 && <div className="space-y-2"><p className="text-sm text-text-muted">{futureOverrides} {futureOverrides === 1 ? "date has its" : "dates have their"} own visibility and won't change.</p><Checkbox label="Also reset those dates to the series setting" checked={resetOverrides} onChange={(e) => setResetOverrides(e.target.checked)} /></div>}
      {review && !reviewMoved && (
        <GroupVisibilityReview
          key={JSON.stringify(review.impact)}
          impact={review.impact}
          subject="event"
          reset={resetOverrides}
          busy={busy}
          waiting={waiting}
          onConfirm={() => void commit(review.impact.consent ?? "")}
          onCancel={() => setReview(null)}
          returnFocus={saveButton}
        />
      )}
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
        <Button ref={saveButton} variant="secondary" type="submit" disabled={busy || waiting} aria-describedby={waiting && !busy ? waitingId : undefined}>
          {busy ? "Saving…" : "Save visibility"}
        </Button>
        <p id={waitingId} aria-live="polite" className="text-sm text-text-muted">{waiting && !busy && ROSTER_WAITING}</p>
      </div>
    </Form>
  );
}

// VisibilityField is the one "Who can see this event" control, used on every
// event create form and inside every event/series editor. A group event
// defaults to its group's value; choosing wider than the group asks for an
// optional reason and is saved as a leader-opened override (DEC-17/28).
// A church-wide event is public or private.
export function VisibilityField({
  seriesVisibility,
  inheritSelected,
  onInherit,
  id,
  value,
  onChange,
  churchWide,
  groupName,
  groupVisibility,
  leaderOpened,
  reason,
  onReason,
}: {
  seriesVisibility?: Visibility;
  inheritSelected?: boolean;
  onInherit?: () => void;
  id: string;
  value: Visibility;
  onChange: (v: Visibility) => void;
  churchWide?: boolean;
  groupName?: string;
  groupVisibility?: Visibility;
  leaderOpened?: boolean;
  reason: string;
  onReason: (v: string) => void;
}) {
  const opening = !churchWide && groupVisibility != null && visibilityRank(value) > visibilityRank(groupVisibility);
  const choices = churchWide ? CHOICES.filter((c) => c.value !== "invite_only") : CHOICES;

  return (
    <div className="space-y-3">
      <Choices legend={seriesVisibility ? "Who can see this date" : "Who can see this event"}>
        {seriesVisibility && <Radio name={id} label={`Same as series (${seriesVisibility === "invite_only" ? "invite only" : seriesVisibility})`} checked={inheritSelected} onChange={onInherit} />}
        {choices.map((c) => (
          <Radio
            key={c.value}
            name={id}
            value={c.value}
            label={`${c.label}${!churchWide && c.value === groupVisibility ? " (same as the group)" : ""}`}
            description={c.description}
            checked={!inheritSelected && value === c.value}
            onChange={() => onChange(c.value)}
          />
        ))}
      </Choices>
      {leaderOpened && !churchWide && (
        <p className="text-xs text-text-muted">
          A leader opened this event wider than the group. If the group becomes more private, you'll be asked whether to keep it open.
        </p>
      )}
      {opening && groupName && (
        <p className="text-sm text-text-muted bg-warning-subtle border border-warning-border rounded-md p-3">{openedEventWarning(groupName)}</p>
      )}
      {opening && (
        <Field id={`${id}-reason`} label="Reason" optional>
          <Input value={reason} onChange={(e) => onReason(e.target.value)} maxLength={500} />
        </Field>
      )}
    </div>
  );
}
