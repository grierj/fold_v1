import { useLayoutEffect, useRef, useState } from "react";
import { ApiError } from "../api/core";
import { Groups } from "../api/groups";
import type { GroupSummary, OpenedDecision, VisibilityImpact } from "../types";
import type { Action } from "../lib/useAction";
import { useRosterActivityOf } from "../lib/rosterWrites";
import { useFocusHome, useKeepFocusHome, useLockedFocus } from "../lib/focusHome";
import { decisionImpact, reviewOutdated } from "../lib/visibilityConsent";
import { ROSTER_STALE, visibilityRank } from "../lib/visibilityChange";
import { errorText } from "../lib/errors";
import { answerNames, lostAuthority, useRevisionSave, type RevisionConflict } from "../lib/useRevisionSave";
import { FormError } from "./ui/Field";
import { GroupForm, type GroupFormData } from "./GroupForm";
import { GroupVisibilityReview } from "./GroupVisibilityReview";

// What the group form's save can write, and the cover it shows, as the group page reads them.
export type GroupValue = Pick<GroupSummary, "name" | "description" | "location" | "tags" | "visibility" | "memberLimit" | "coverFocalX" | "coverFocalY" | "imageUrl">;

// The cover change the form asked for, made only once its details are saved.
// A new file's focal point goes with it, never with the details: the details are saved first, and a failed upload
// would otherwise leave the old photo cropped for the new one.
export type CoverStep = { file: File; focal?: { x: number; y: number } } | { remove: true };

const clampFocal = (v: number) => Math.min(100, Math.max(0, v));

// groupFields is a group value as PUT /groups/:id stores it: name, description and location trimmed, tags trimmed and
// joined without the empty ones (joinTags), the focal point clamped (clampFocal). The cover's address is not the save's
// to write, but the form shows the cover, so a cover someone changed while it was open is a conflict.
export const groupFields = (g: GroupValue) => ({
  name: g.name.trim(),
  description: g.description.trim(),
  location: g.location.trim(),
  tags: g.tags.map((t) => t.trim()).filter(Boolean).join(","),
  visibility: g.visibility,
  memberLimit: g.memberLimit ?? null,
  coverFocalX: clampFocal(g.coverFocalX),
  coverFocalY: clampFocal(g.coverFocalY),
  imageUrl: g.imageUrl,
});

// A leader's keep/tighten decisions go to the events, the audit rows and the notices, and no read of the group shows
// them: what is stored counts as this save only when the send that got no answer carried the same decisions.
/** @public tested directly: the canonical form of the decisions. */
export const decisionsKey = (decisions?: Record<string, OpenedDecision>) =>
  decisions ? JSON.stringify(Object.entries(decisions).sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))) : "";

// The events a group review lists, whose sign-up updates withdraw it.
const impactEventIds = (impact: VisibilityImpact) => [...impact.tightened, ...impact.leaderOpened].map((e) => e.id);

// Every send was refused while nothing the form writes changed: the form says its ordinary refusal.
const refusedEverySend = () => new ApiError(409, "group changed", { code: "group_changed" });

// GroupEditor is one opening of the group form on the group page, with the visibility review its save may need
// (DEC-48, DEC-158). It mounts once per opening: the save names the read it opened with (DEC-166), and a refusal is
// the shared protocol's (useRevisionSave). The details are saved first; only after they are saved does the page make
// the cover change the form asked for (`onSaved`), so a refused save never leaves a changed cover behind.
export function GroupEditor({ group, action, onSaved, onConflict, onCancel }: {
  // The page's read when the editor opened.
  group: GroupSummary;
  // The page's run, so it can keep what would read the page again from starting while a save is on its way.
  action: Action;
  // The details are saved: the page closes the editor, makes the cover change if there is one, and reads again.
  onSaved: (cover: CoverStep | null) => void;
  // The save was refused and not sent again: the page closes the editor and shows the group as it is now.
  onConflict: (why: RevisionConflict) => void;
  onCancel: () => void;
}) {
  const [opened] = useState(group);
  // `mark` is the session roster sequence the request behind the review was sent at.
  const [review, setReview] = useState<{ data: GroupFormData; impact: VisibilityImpact; mark: number } | null>(null);
  const [reviewError, setReviewError] = useState<string | null>(null);
  const groupSave = useRef<HTMLButtonElement>(null);
  // Holds the review and its lines; keeps keyboard focus when a sign-up update withdraws a review.
  const reviewHome = useFocusHome<HTMLDivElement>();
  useKeepFocusHome(reviewHome);
  useLockedFocus(reviewHome, action.running);
  const activity = useRosterActivityOf();
  const reviewCommitting = useRef(false);

  const send = useRevisionSave<GroupValue, ReturnType<typeof groupFields>>({
    opened,
    revision: opened.revision,
    read: async () => {
      const stored = await Groups.get(opened.id);

      return { allowed: stored.canManage, revision: stored.group.revision, value: stored.group };
    },
    fields: groupFields,
    answers: answerNames("group", opened.id),
    onConflict: (why) => {
      parkFocus();
      onConflict(why);
    },
    changedCode: "group_changed",
  });

  // The form goes, and the page with it until its read answers: focus still in the form or the review waits on the
  // main region.
  function parkFocus() {
    const active = document.activeElement;

    if (reviewHome.ref.current?.contains(active) || groupSave.current?.form?.contains(active)) document.getElementById("main")?.focus({ preventScroll: true });
  }

  // A sign-up update on one of these events began, ended or is in flight since `mark`: a review's counts may
  // no longer be who is signed up. A write on any other event leaves the review alone.
  const moved = (mark: number, ids: number[]) => activity.changedSince(mark, ids) || activity.pendingFor(ids) > 0;

  // Withdraws the review (or never opens one) and says so; focus on Save, on the form (which holds it while a save
  // is on its way) or on the review goes to the review's home, so a keyboard user hears the line and Save group is
  // the next stop.
  function staleReview() {
    const active = document.activeElement;
    const home = reviewHome.ref.current;
    const save = groupSave.current;

    if (!active || active === document.body || home?.contains(active) || active === save || active === save?.form) reviewHome.moveHere();
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

  async function saveEdit(data: GroupFormData): Promise<void | false> {
    // Making the group more private can change its events (DEC-48): show the leader the list and get a decision
    // before anything is saved. The save carries the preview's consent, so a change since the preview is shown again
    // from the server's own answer (DEC-158). A sign-up update on one of the listed events while either was asked
    // withdraws it.
    let impact: VisibilityImpact | null = null;
    setReviewError(null);

    if (visibilityRank(data.visibility) < visibilityRank(opened.visibility)) {
      const mark = activity.mark();

      try {
        impact = await Groups.visibilityImpact(opened.id, data.visibility);
      } catch (err) {
        // The preview is refused like the save would be: the editor closes and the page says so.
        if (err instanceof ApiError && lostAuthority(err)) {
          parkFocus();
          onConflict("gone");

          return false;
        }

        throw err;
      }

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
      if (!(await commit(data, undefined, impact?.consent ?? "", at))) return false;
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
      if (!(await commit(review.data, decisions, review.impact.consent ?? "", at))) return false;
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
        // After a sign-up update on its events since the review's own request the review is withdrawn too: the
        // next Save previews again.
        if (moved(review.mark, impactEventIds(review.impact))) setReview(null);
        setReviewError(reviewOutdated(e) ?? errorText(e, "We couldn't save your changes."));
      }

      return false;
    } finally {
      reviewCommitting.current = false;
    }
  }

  // Saves the details through the shared protocol; true once they are saved. `at.mark` is taken again just before each
  // send, so a 409's review is judged from the request that answered it. The cover change waits for the save.
  async function commit(data: GroupFormData, openedEvents: Record<string, OpenedDecision> | undefined, consent: string, at: { mark: number }) {
    const detailsFocal = data.coverFile ? undefined : data.coverFocal;

    const intended: GroupValue = {
      ...opened,
      name: data.name,
      description: data.description,
      location: data.location,
      tags: data.tags,
      visibility: data.visibility,
      memberLimit: data.memberLimit,
      coverFocalX: detailsFocal?.x ?? opened.coverFocalX,
      coverFocalY: detailsFocal?.y ?? opened.coverFocalY,
    };

    const sent = await send((revision) => {
      at.mark = activity.mark();

      return Groups.update(opened.id, {
        name: data.name,
        description: data.description,
        tags: data.tags,
        location: data.location,
        coverFocal: detailsFocal,
        visibility: data.visibility,
        memberLimit: data.memberLimit,
        openedEvents,
        consent,
        revision: revision ?? "",
      });
    }, intended, decisionsKey(openedEvents));

    if (sent === "refused") throw refusedEverySend();

    if (sent !== "saved") return false;
    parkFocus();
    setReviewError(null);
    onSaved(data.coverFile ? { file: data.coverFile, focal: data.coverFocal } : data.removeCover ? { remove: true } : null);

    return true;
  }

  return (
    <>
      <div ref={reviewHome.ref} tabIndex={-1}>
        {reviewError && <FormError layout="mb-2">{reviewError}</FormError>}
        {review && !reviewMoved && <GroupVisibilityReview
          key={JSON.stringify(review.impact)}
          impact={review.impact}
          busy={action.running}
          waiting={activity.pendingFor(reviewIds ?? []) > 0}
          onConfirm={(d) => void action.run(() => confirmReview(d))}
          onCancel={() => {
            setReview(null);
            setReviewError(null);
          }}
          returnFocus={groupSave}
        />}
      </div>
      <GroupForm
        initial={{
          name: opened.name,
          description: opened.description,
          location: opened.location,
          tags: opened.tags,
          imageUrl: opened.imageUrl,
          coverFocalX: opened.coverFocalX,
          coverFocalY: opened.coverFocalY,
          coverId: opened.id,
          moreOpen: true,
          visibility: opened.visibility,
          memberLimit: opened.memberLimit,
        }}
        saveLabel="Save group"
        saveRef={groupSave}
        action={action}
        onSaved={saveEdit}
        onCancel={() => {
          action.reset();
          onCancel();
        }}
      />
    </>
  );
}
