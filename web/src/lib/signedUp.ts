import { Events } from "../api/events";
import type { Id } from "../api/core";

// The line a delete or cancel confirmation shows about who is told. The count leaves out the person pressing, who is
// never told of their own change, so every line says "other". null is a count that could not be read.
function signedUpLine(n: number | null): string {
  if (n === null) return "Everyone else signed up will be notified.";

  if (n === 0) return "No one else is signed up.";

  return `${n} other ${n === 1 ? "person is" : "people are"} signed up and will be notified.`;
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
