import { useId } from "react";
import { Button } from "./ui/Button";
import { Dialog } from "./ui/Dialog";

export type SeriesRsvpScope = "just_this_date" | "this_and_following" | "all_future";

// `justThisDate`, when given, says why this one date can't take the answer (it is full with no
// waitlist): that choice is disabled and the wider scopes stay open, so other dates can still be answered.
export function SeriesRsvpDialog({ answer, disabled = false, justThisDate, onChoose, onCancel }: {
  answer: string;
  disabled?: boolean;
  justThisDate?: string;
  onChoose: (scope: SeriesRsvpScope) => void;
  onCancel: () => void;
}) {
  const note = useId();

  return <Dialog title={<>How far should “{answer}” apply?</>} description="Choose which dates in this series you want to answer." onDismiss={onCancel}>
    <div className="grid gap-2">
      <Button variant="secondary" layout="w-full min-h-11 justify-start text-left" disabled={disabled || !!justThisDate} aria-describedby={justThisDate ? note : undefined} onClick={() => onChoose("just_this_date")}>Just this date</Button>
      {justThisDate && <p id={note} className="text-sm text-text-muted">{justThisDate}</p>}
      <Button variant="secondary" layout="w-full min-h-11 justify-start text-left" disabled={disabled} onClick={() => onChoose("this_and_following")}>This and following dates</Button>
      <Button variant="secondary" layout="w-full min-h-11 justify-start text-left" disabled={disabled} onClick={() => onChoose("all_future")}>All future dates</Button>
    </div>
    <p className="text-sm text-text-muted">New dates added later won&apos;t be answered for you.</p>
    <Button variant="secondary" layout="w-full" onClick={onCancel}>Cancel</Button>
  </Dialog>;
}
