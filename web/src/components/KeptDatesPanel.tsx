import { useCallback, useEffect, useState } from "react";
import { Events } from "../api/events";
import { outcomeOf, useRosterGate } from "../lib/dateWrites";
import { ERROR_COPY } from "../lib/errors";
import { useFocusHome, useKeepFocusHome, type FocusHome } from "../lib/focusHome";
import { useLoad } from "../lib/useLoad";
import { formatDayFull } from "../lib/when";
import type { KeptDateCount } from "../types";
import { KEPT_CHECKING, type KeptDatesProps } from "./KeptDates";
import { ManageRoster, type RosterRecheck } from "./ManageRoster";
import { Button } from "./ui/Button";
import { ConfirmProvider, usePageFacts } from "./ui/ConfirmDialog";
import { FormError } from "./ui/Field";

const REFUSED = "Your change isn't saved. You can't manage sign-ups for this event now.";
const UNKNOWN = "Your change isn't saved yet. We couldn't check that you can still manage sign-ups. What you changed is still in the form.";
const NOBODY_ONE = "Nobody is keeping this date now. Save again to apply your change.";
const NOBODY_HERE = "Nobody is keeping this date now.";
const SAVE_LINE = "Your change wasn't saved: when you saved, someone who left the group had kept at least one of these dates. Remove anyone marked “Kept after leaving” under each date, then save again. Saving checks every date again.";
const WRITE_NOTE = "Your last change may not have gone through. Here's the list as it is now.";

// The dates a save was refused for because someone who left kept them (DEC-156), with the roster to remove
// them from. The shell owns which date is selected, the refusal latch and the focus home, and shows nothing
// about who kept what: that comes only from one keyed read of the selected date (KeptDateRead), and no line
// ever claims a set of dates is clear. No count is shown, so nothing on screen goes stale against the roster.
export function KeptDatesPanel({ eventId, keptDates, onRefused, onClose }: KeptDatesProps) {
  const [date, setDate] = useState(keptDates[0].date);
  const [refused, setRefused] = useState(false);
  const home = useFocusHome<HTMLElement>();
  const key = `${eventId}:${date}`;

  // Only the read of the selected date may refuse the panel.
  const refuse = useCallback((from: string) => {
    if (from !== key) return;
    const inside = home.ref.current?.contains(document.activeElement) ?? false;
    setRefused(true);
    onRefused(inside);
  }, [key, onRefused, home.ref]);

  // Focus moves to the home before the pressed button unmounts with the old read.
  const select = useCallback((next: string) => {
    home.moveHere();
    setDate(next);
  }, [home]);

  return <section ref={home.ref} tabIndex={-1} aria-label="Sign-ups to check before you save" className="mt-4 space-y-3">
    {refused
      ? <p role="status">{REFUSED}</p>
      // A new key is a new instance: no data, no error, no roster and no open question.
      : <KeptDateRead key={key} readKey={key} eventId={eventId} date={date} keptDates={keptDates} home={home} onSelect={select} onRefused={refuse} onClose={onClose} />}
  </section>;
}

interface ReadProps {
  readKey: string;
  eventId: number;
  date: string;
  keptDates: KeptDateCount[];
  home: FocusHome<HTMLElement>;
  onSelect: (date: string) => void;
  onRefused: (readKey: string) => void;
  onClose: () => void;
}

// One read of one date. It only returns the questions' provider, so the one usePageFacts beneath it writes
// here and never into the host page's provider.
function KeptDateRead(props: ReadProps) {
  return <ConfirmProvider><KeptDateBody {...props} /></ConfirmProvider>;
}

function KeptDateBody({ readKey, eventId, date, keptDates, home, onSelect, onRefused, onClose }: ReadProps) {
  // eventId and date are fixed for this instance: the shell's key holds both.
  const load = useLoad(() => Events.managedDate(eventId, date).then((r) => r.occurrence), [eventId, date], { refreshOnFocus: true });
  // Roster writes on this date in flight for the session: the one starter of the read after a write.
  const gate = useRosterGate(eventId, date, load);
  const refusedNow = load.error != null && outcomeOf(load.error) === "refused";

  useEffect(() => {
    if (refusedNow) onRefused(readKey);
  }, [refusedNow, readKey, onRefused]);

  // Decided here in render: the error before the data, and a closed gate reads as a pending read.
  const phase = refusedNow ? "refused" : load.error != null ? "unknown" : load.data === undefined || gate.closed ? "checking" : "granted";
  usePageFacts(JSON.stringify({ readKey, phase, data: load.data ?? null }));
  // A recheck, the gate closing or a write's retry removes the focused control in this render.
  useKeepFocusHome(home);

  const { clearNote } = gate;
  const { retry } = load;

  // A focus is already this load's own refreshOnFocus: acting on it too would read twice.
  const recheck = useCallback((why: RosterRecheck) => {
    if (why === "focus") return;
    clearNote();
    void retry();
  }, [clearNote, retry]);

  if (phase === "refused") return <p role="status">{REFUSED}</p>;

  if (phase === "unknown") return <>
    <p role="status">{UNKNOWN}</p>
    <Button variant="secondary" onClick={() => void retry()}>Try again</Button>
  </>;
  const data = load.data;

  if (phase === "checking" || data === undefined) return <p role="status">{KEPT_CHECKING}</p>;
  const several = keptDates.length > 1;
  // A fact about this one read (its keptCount is the refusal's own test, in the read's snapshot); it never
  // predicts the next save, and no other date's answer is kept.
  const clear = data.attendeesVisible !== true || data.keptCount === 0;

  return <>
    {several
      ? <FormError>{SAVE_LINE}</FormError>
      : clear ? <p role="status">{NOBODY_ONE}</p> : <FormError>{ERROR_COPY.date_kept_after_leaving}</FormError>}
    {/* The selected date first, so the next Tab from the panel's home reaches its roster. */}
    {[...keptDates.filter((k) => k.date === date), ...keptDates.filter((k) => k.date !== date)].map((k) => <div key={k.date} className="space-y-2 border-t border-border pt-3">
      <h3 className="font-semibold">{formatDayFull(k.date)}</h3>
      {k.date !== date
        ? <Button variant="secondary" onClick={() => onSelect(k.date)}>Manage sign-ups</Button>
        : clear
          ? several && <p className="text-sm text-text-muted">{NOBODY_HERE}</p>
          : <>
            {gate.note && <p role="status" className="text-sm">{WRITE_NOTE}</p>}
            <ManageRoster instance={data} headingLevel={4} gate={gate.gate} onRecheck={recheck} onClose={onClose} />
          </>}
    </div>)}
  </>;
}
