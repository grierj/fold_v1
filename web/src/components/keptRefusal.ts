import { useCallback, useLayoutEffect, useRef, useState, type RefObject } from "react";
import type { KeptDateCount } from "../types";

const REFUSED = "Your change isn't saved. You can't manage sign-ups for this event now.";

interface Refusal {
  // Every refusal is a new panel (its key), even for the same dates: nothing an earlier panel read carries over.
  seq: number;
  dates: KeptDateCount[];
}

interface Options {
  // Dates a host held across a remount: a new refusal, read again from the start.
  initial: KeptDateCount[] | null;
  // The editor's Save, where focus goes when the panel closes on a refusal it had focus in.
  submit: RefObject<HTMLButtonElement | null>;
  onChange?: (dates: KeptDateCount[] | null) => void;
  // The editor's own error line, for the panel's refusal.
  onRefused: (message: string) => void;
  onAuthorityLost?: () => void;
}

// An editor's save refused because someone who left kept a date (DEC-156, the kept-dates panel). When the
// panel's own read is refused, the kept state is cleared, not hidden, and the draft stays.
export function useKeptRefusal({ initial, submit, onChange, onRefused, onAuthorityLost }: Options) {
  const seq = useRef(0);
  const [refusal, setRefusal] = useState<Refusal | null>(() => (initial && initial.length > 0 ? { seq: ++seq.current, dates: initial } : null));
  const latest = useRef({ onChange, onRefused, onAuthorityLost });
  const focusSave = useRef(false);

  useLayoutEffect(() => {
    latest.current = { onChange, onRefused, onAuthorityLost };
  });

  // After the commit that closes the panel, so focus never falls to the page.
  useLayoutEffect(() => {
    if (!focusSave.current || refusal) return;
    focusSave.current = false;
    submit.current?.focus();
  });

  const open = useCallback((dates: KeptDateCount[]) => {
    seq.current++;
    setRefusal({ seq: seq.current, dates });
    latest.current.onChange?.(dates);
  }, []);

  const close = useCallback(() => {
    setRefusal(null);
    latest.current.onChange?.(null);
  }, []);

  const refused = useCallback((focusWasInside: boolean) => {
    focusSave.current = focusWasInside;
    setRefusal(null);
    latest.current.onChange?.(null);
    latest.current.onRefused(REFUSED);
    latest.current.onAuthorityLost?.();
  }, []);

  return { refusal, open, close, refused };
}
