import { createContext, useContext, useLayoutEffect } from "react";
import { credentialCurrent, recheckCredential, type Credential } from "../api/core";

interface EventReadScope {
  pin: readonly [Credential];
  close: () => void;
}

const unpinned = [] as const;
export const EventReadPin = createContext<EventReadScope | null>(null);

export function useEventReadPin() {
  const scope = useContext(EventReadPin);

  // A room or visibility answer can update its control without rendering the event page.
  // Withdraw the whole page before that update paints if the browser credential changed.
  useLayoutEffect(() => {
    if (scope && !credentialCurrent(scope.pin[0])) {
      scope.close();
      recheckCredential();
    }
  });

  return scope?.pin ?? unpinned;
}
