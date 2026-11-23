import { people } from "../lib/plural";
import { useEffect, useId, useRef, useState, type RefObject } from "react";
import type { OpenedDecision, VisibilityImpact } from "../types";
import { formatDayLong } from "../lib/when";
import { changeSentence, ROSTER_WAITING, VISIBILITY_WORD as WORD, visibilityRank } from "../lib/visibilityChange";
import { Button } from "./ui/Button";
import { Choices, Radio } from "./ui/Field";

// GroupVisibilityReview shows a leader what making the group more private
// does to its events before anything is saved (DEC-48). Events that only
// inherited the group's visibility are tightened. Events a leader opened
// wider need a choice: keep open (logged) or match the group. Registrants
// who could no longer see a tightened event are cancelled and told.
// subject "event" reuses the same panel for one event (the impact then lists
// only that event and, with reset, its reset dates). Every line comes from the
// impact the server answered, which its consent token confirms (DEC-158).
export function GroupVisibilityReview({
  impact,
  subject = "group",
  reset,
  busy,
  waiting = false,
  onConfirm,
  onCancel,
  returnFocus,
}: {
  impact: VisibilityImpact;
  subject?: "group" | "event";
  reset?: boolean;
  busy: boolean;
  // A sign-up update on one of the impact's events is on its way: confirm waits, keeping its label.
  waiting?: boolean;
  onConfirm: (decisions: Record<string, OpenedDecision>) => void;
  onCancel: () => void;
  // The control that opened the review: Cancel gives it focus back.
  returnFocus: RefObject<HTMLElement | null>;
}) {
  const [decisions, setDecisions] = useState<Record<string, OpenedDecision>>({});
  const titleId = useId();
  const changeId = useId();
  const heading = useRef<HTMLHeadingElement>(null);
  useEffect(() => {
    heading.current?.focus();
  }, []);
  const next = WORD[impact.visibility];
  const current = impact.currentVisibility;
  const ready = impact.leaderOpened.every((e) => decisions[(e.decisionKey || String(e.id))]);
  const label = (title: string) => title || "Untitled event";
  const who = subject === "group" ? "the group" : "this event";
  const resetDates = reset ? (impact.resetDates ?? []) : [];
  const n = resetDates.length;
  const resetOnly = !!reset && current === impact.visibility;
  const overrides = impact.futureOverrides ?? 0;
  let title = subject === "group" ? `Making the group ${next} changes some events` : `Making this event ${next}`;
  let confirmLabel = `Make ${who} ${next}`;

  if (resetOnly && n > 0) {
    title = "Reset this event's dates to the series setting";
    confirmLabel = `Reset ${n} ${n === 1 ? "date" : "dates"}`;
  } else if (resetOnly) {
    title = "No dates to reset";
    confirmLabel = `Keep this event ${WORD[current]}`;
  }

  function cancel() {
    const target = returnFocus.current;

    if (target?.isConnected && !(target instanceof HTMLButtonElement && target.disabled)) target.focus({ preventScroll: true });
    onCancel();
  }

  return (
    <section
      role="dialog"
      aria-labelledby={titleId}
      aria-describedby={changeId}
      className="bg-surface rounded-lg border border-warning-border p-6 space-y-4 mb-4"
    >
      <h2 id={titleId} ref={heading} tabIndex={-1} className="text-lg font-bold outline-hidden">
        {title}
      </h2>
      <p id={changeId} className="text-sm">{changeSentence(subject, current, impact.visibility)}</p>
      {impact.tightened.length > 0 && (
        <div>
          <h3 className="font-semibold text-sm">{subject === "group" ? `These events will become ${next} too` : `This event will become ${next}`}</h3>
          <ul
            className="mt-1 text-sm list-disc pl-5"
            aria-label={subject === "group" ? "Events that will match the group" : "Event that will change"}
          >
            {impact.tightened.map((e) => (
              <li key={e.decisionKey || `${e.id}-${e.occurrenceId ?? 0}-${e.title}`}>
                {label(e.title)}{e.date ? ` · ${formatDayLong(e.date)}` : ""} <span className="text-text-muted">(now {WORD[e.visibility]})</span>
                {e.registrants > 0 && (
                  <span className="text-danger"> · {people(e.registrants)} registered will be cancelled and notified</span>
                )}
              </li>
            ))}
          </ul>
        </div>
      )}
      {reset && n > 0 && (
        <div className="space-y-2">
          <p className="text-sm">This resets {n} {n === 1 ? "date" : "dates"} to the series setting ({next}):</p>
          <div role="region" aria-label="Dates that will be reset" tabIndex={0} className="max-h-64 overflow-y-auto">
            <ul className="space-y-2 text-sm">
              {resetDates.map((d) => (
                <li key={d.occurrenceId}>
                  <span className="block font-medium">{label(d.title)}</span>
                  <span className="block text-text-muted">{formatDayLong(d.date)}. Now {WORD[d.visibility]}.</span>
                  {visibilityRank(d.visibility) < visibilityRank(impact.visibility) && (
                    <span className="block">After the reset, more people will be able to see this date.</span>
                  )}
                </li>
              ))}
            </ul>
          </div>
        </div>
      )}
      {reset && n === 0 && <p className="text-sm">No future dates have their own visibility, so no date will be reset.</p>}
      {!reset && subject === "event" && overrides > 0 && (
        <p className="text-sm text-text-muted">{overrides} {overrides === 1 ? "date has its" : "dates have their"} own visibility and won't change.</p>
      )}
      {impact.leaderOpened.length > 0 && (
        <fieldset>
          <legend className="font-semibold text-sm">A leader opened these events up. Choose for each one.</legend>
          <ul className="mt-2 space-y-3">
            {impact.leaderOpened.map((e) => {
              const key = (e.decisionKey || String(e.id));

              return (
                <li key={e.decisionKey || e.id} data-testid={`opened-${e.id}`}>
                  <Choices
                    legend={
                      <>
                        {label(e.title)}{e.date ? ` · ${formatDayLong(e.date)}` : ""} <span className="text-text-muted font-normal">(now {WORD[e.visibility]})</span>
                      </>
                    }
                  >
                    <Radio
                      name={`opened-${key}`}
                      label={`Keep it ${WORD[e.visibility]}`}
                      description="Logged, and other leaders and admins are told."
                      checked={decisions[key] === "keep"}
                      onChange={() => setDecisions((d) => ({ ...d, [key]: "keep" }))}
                    />
                    <Radio
                      name={`opened-${key}`}
                      label={`Make it ${next}`}
                      description={e.registrants > 0 && <span className="text-danger">Cancels {people(e.registrants)} and notifies them.</span>}
                      checked={decisions[key] === "tighten"}
                      onChange={() => setDecisions((d) => ({ ...d, [key]: "tighten" }))}
                    />
                  </Choices>
                </li>
              );
            })}
          </ul>
        </fieldset>
      )}
      <div>
        <p aria-live="polite" className="text-sm text-text-muted mb-2 empty:mb-0">{waiting && !busy && ROSTER_WAITING}</p>
        <div className="flex gap-2">
          <Button variant="primary" disabled={busy || waiting || !ready} onClick={() => onConfirm(decisions)}>
            {busy ? "Saving…" : confirmLabel}
          </Button>
          <Button variant="secondary" disabled={busy} onClick={cancel}>
            Cancel
          </Button>
        </div>
      </div>
    </section>
  );
}
