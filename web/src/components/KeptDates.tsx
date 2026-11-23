import { lazy, Suspense } from "react";
import type { KeptDateCount } from "../types";

// The panel's checking line, also shown while its code loads.
export const KEPT_CHECKING = "Your change isn't saved yet. Checking that you can still manage sign-ups…";

const KeptDatesPanel = lazy(() => import("./KeptDatesPanel").then((module) => ({ default: module.KeptDatesPanel })));

export interface KeptDatesProps {
  eventId: number;
  // The dates a save was refused for (DEC-156), never empty.
  keptDates: KeptDateCount[];
  // A read of the selected date was refused; `focusWasInside` says whether keyboard focus was in the panel.
  onRefused: (focusWasInside: boolean) => void;
  onClose: () => void;
}

// An editor's kept-dates panel, loaded when a save is first refused.
export function KeptDates(props: KeptDatesProps) {
  return <Suspense fallback={<p role="status" className="mt-4">{KEPT_CHECKING}</p>}><KeptDatesPanel {...props} /></Suspense>;
}
