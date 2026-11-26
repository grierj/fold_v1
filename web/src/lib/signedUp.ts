import { Events } from "../api/events";
import type { Id } from "../api/core";
import { people } from "./plural";

// The line a delete or cancel confirmation shows about who is told. null is a count that could not be read.
function signedUpLine(n: number | null): string {
  if (n === null) return "Everyone signed up will be notified.";

  if (n === 0) return "Nobody is signed up.";

  return `${people(n)} ${n === 1 ? "is" : "are"} signed up and will be notified.`;
}

// The line for deleting the event (no date) or cancelling one date. The count is shown, never sent back or checked:
// a failed read still lets the person decide.
export async function signedUpNotice(eventId: Id, date?: string): Promise<string> {
  try {
    return signedUpLine((await Events.signedUp(eventId, date)).people);
  } catch {
    return signedUpLine(null);
  }
}
