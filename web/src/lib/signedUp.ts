import { Admin } from "../api/admin";
import { Events } from "../api/events";
import type { Id } from "../api/core";

// The line a delete or cancel confirmation shows about who is told. The count is the people who will be notified, not
// who is going: it leaves out the person pressing, who is never told of their own change (their own sign-up, their
// membership and a sign-up they made for someone in their household included), so every line says "other". null is a
// count that could not be read, and then the line is unread: who the action tells, in words that stay true for the
// people it does not reach.
function notifiedLine(n: number | null, unread: string): string {
  if (n === null) return unread;

  if (n === 0) return "No one else will be notified.";

  return `${n} other ${n === 1 ? "person" : "people"} will be notified.`;
}

// The line for deleting the event (no date) or cancelling one date: a delete tells the people on the event's upcoming
// dates, a cancel the people on its one date. The count is shown, never sent back or checked: a failed read still lets
// the person decide.
export async function signedUpNotice(eventId: Id, date?: string): Promise<string> {
  const unread = `Anyone else signed up for ${date !== undefined ? "this date" : "an upcoming date"} will be notified.`;

  try {
    return notifiedLine((await Events.signedUp(eventId, date)).people, unread);
  } catch {
    return unread;
  }
}

// The line for an admin deleting a group, which tells its members and the people signed up for an upcoming date of
// its events (DEC-177). Shown the same way: never sent back or checked.
export async function groupDeleteNotice(groupId: Id): Promise<string> {
  const unread = "Anyone else who is a member or signed up for an upcoming date will be notified.";

  try {
    return notifiedLine((await Admin.deleteCount(groupId)).people, unread);
  } catch {
    return unread;
  }
}
