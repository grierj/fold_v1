import { useMemo } from "react";
import { useLoad } from "../../lib/useLoad";
import { useAuth } from "../../auth";
import { Messages } from "../../api/messages";

// Where a page sends a leader to write to its people (M3.4c). Every entry
// point uses these words and paths, so a group, an event and a date read
// the same (AGENTS.md, Consistency).
export const EMAIL_MEMBERS = "Email members";
export const EMAIL_GOING = "Email people going";

export function composePath(to: { group: number } | { event: number } | { occurrence: number }): string {
  const [key, id] = Object.entries(to)[0];

  return `/messages/new?${key}=${id}`;
}

export function useCanEmailEvent(eventId: number | undefined, dateEpoch: number): boolean {
  const { canCompose } = useAuth();
  // A fresh date read can reflect a host change even when the event ID stays the same.
  const scope = useMemo(() => canCompose && eventId !== undefined ? { eventId, dateEpoch } : null, [canCompose, eventId, dateEpoch]);
  const load = useLoad(async () => scope !== null && (await Messages.options(scope.eventId)).events.some((e) => e.id === scope.eventId), [], { enabled: scope !== null, scope });

  return load.data === true;
}
