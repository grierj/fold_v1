import { createContext, useContext } from "react";
import type { Credential } from "../api/core";

// The event page renders these readers only while its own answer holds for this credential. The same components
// can also run on other pages, which have their own read protocol.
export const EventReadPin = createContext<readonly [Credential] | readonly []>([]);

export function useEventReadPin() {
  return useContext(EventReadPin);
}
