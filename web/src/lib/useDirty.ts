import { useRef } from "react";
import type { EventOptionValues } from "../components/eventOptionValues";
import type { JsonValue } from "./wireValues";

interface FormSnapshot {
  [field: string]: JsonValue | EventOptionValues | undefined;
}

// Whether a form differs from what it started as (feeds useUnsavedChanges).
// Pass a plain-data snapshot of every field. The baseline is the first
// snapshot taken once `ready` is true, so values that arrive after mount (the
// group's location, the church time zone) don't count as edits. A form that
// stays open after a save passes a new `savedAt` with the snapshot it saved,
// which becomes the baseline.
export function useDirty(snapshot: FormSnapshot, ready = true, savedAt = 0): boolean {
  const base = useRef<string | null>(null);
  const baseAt = useRef(savedAt);
  const now = JSON.stringify(snapshot);

  if (ready && (base.current === null || baseAt.current !== savedAt)) {
    base.current = now;
    baseAt.current = savedAt;
  }

  return base.current !== null && base.current !== now;
}
