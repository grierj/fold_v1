import type { Visibility } from "../types";

export const VISIBILITY_WORD: Record<Visibility, string> = {
  public: "public",
  private: "private",
  invite_only: "invite-only",
};

export function visibilityRank(v: string): number {
  if (v === "public") return 2;

  if (v === "private") return 1;

  return 0;
}

const SUBJECT = { event: "This event", date: "This date", group: "The group" } as const;

// changeSentence says what a visibility save does, from the scope's current
// value to the one being saved. Every visibility review and the date question
// open with it, so the value the consent binds is on screen (DEC-158).
export function changeSentence(subject: keyof typeof SUBJECT, current: Visibility, next: Visibility): string {
  const who = SUBJECT[subject];
  const cur = VISIBILITY_WORD[current];

  if (visibilityRank(next) === visibilityRank(current)) return `${who} is ${cur} now and will stay ${cur}.`;
  const more = visibilityRank(next) > visibilityRank(current) ? "more" : "fewer";

  return `${who} is ${cur} now. Saving makes it ${VISIBILITY_WORD[next]}, so ${more} people will be able to see it.`;
}

// A visibility review waits for, and is withdrawn by, a sign-up update on its events: its counts may no longer
// be who is signed up. The stale lines say only that one started, true whether it is still on its way, was
// stored or was turned down.
export const ROSTER_WAITING = "Saving waits for a sign-up update to finish.";

export const ROSTER_STALE = {
  event: "A sign-up update started while we were checking. Please review again once it finishes.",
  group: "A sign-up update started on one of the group's events while we were checking. Please review again once it finishes.",
} as const;
