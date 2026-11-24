import { vi } from "vitest";
import { bootstrapCredential, type Credential } from "../api/core";
import { Events, type DateEditChanges } from "../api/events";
import type { OccurrenceResponse } from "../types";
import { bootstrapAnswer } from "./credential";
import { occurrence } from "./occurrence";

let boots = 0;

// A signed-in tab holding a verified credential. Only the bootstrap is answered; every other request in a test goes
// through a stubbed service.
export async function verifiedCredential(userId = 5): Promise<Credential> {
  const token = `guarded-${++boots}`;
  vi.stubGlobal("fetch", vi.fn(async (url: RequestInfo | URL) => (String(url).endsWith("/auth/bootstrap") ? bootstrapAnswer({ token, user: { id: userId } }) : new Response("{}", { status: 503 }))));

  return (await bootstrapCredential(true)).credential;
}

// One date save as it reached the transport (Events.editDate), with the attempt id the transport adds.
export interface SentEdit {
  eventId: number | string;
  date: string;
  attempt: string;
  revision: string;
  changes: DateEditChanges;
  reason?: string;
  consent?: string;
}

type Answer = OccurrenceResponse | Error | Promise<OccurrenceResponse>;

export interface EditSends {
  sent: SentEdit[];
  // The answer to the next send (in order); a send with none queued is stored with a new revision.
  answer: (...answers: Answer[]) => void;
}

let attempts = 0;

// Stands in for the date save's transport (Events.editDate) for a test: it records each send, then answers as queued,
// or as date `occurrenceId` stored. Call it in each test (or its beforeEach), after any vi.resetAllMocks.
export function stubEditSends(occurrenceId = 1): EditSends {
  const sent: SentEdit[] = [];
  const queued: Answer[] = [];
  const spy = vi.isMockFunction(Events.editDate) ? vi.mocked(Events.editDate) : vi.spyOn(Events, "editDate");

  spy.mockImplementation(async (eventId, date, body) => {
    const attempt = (++attempts).toString(16).padStart(32, "0");
    sent.push(JSON.parse(JSON.stringify({ eventId, date, attempt, ...body })));
    const next = queued.shift() ?? storedEdit(occurrenceId, `r-stored-${sent.length}`);

    if (next instanceof Error) throw next;

    return next;
  });

  return { sent, answer: (...answers) => { queued.push(...answers); } };
}

// A stored answer naming the submitted date.
export const storedEdit = (id: number, revision: string): OccurrenceResponse => ({ occurrence: occurrence({ id, revision }) });
