import { Events } from "../api/events";
import type { Id } from "../api/core";

// The line a delete or cancel confirmation shows about who is told. The count is the people who will be notified, not
// who is going: it leaves out the person pressing, who is never told of their own change (their own sign-up and one
// they made for someone in their household included), so every line says "other". null is a count that could not be
// read: a delete tells the people on the event's upcoming dates, a cancel the people on its one date.
function signedUpLine(n: number | null, oneDate: boolean): string {
  if (n === null) return `Anyone else signed up for ${oneDate ? "this date" : "an upcoming date"} will be notified.`;

  if (n === 0) return "No one else will be notified.";

  return `${n} other ${n === 1 ? "person" : "people"} will be notified.`;
}

// The line for deleting the event (no date) or cancelling one date. The count is shown, never sent back or checked:
// a failed read still lets the person decide.
export async function signedUpNotice(eventId: Id, date?: string): Promise<string> {
  try {
    return signedUpLine((await Events.signedUp(eventId, date)).people, date !== undefined);
  } catch {
    return signedUpLine(null, date !== undefined);
  }
}
