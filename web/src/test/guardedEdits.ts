import { vi } from "vitest";
import { bootstrapCredential, type Credential } from "../api/core";
import { DateEdits, type DateEditChanges } from "../api/events";
import type { OccurrenceResponse } from "../types";
import { bootstrapAnswer } from "./credential";
import { occurrence } from "./occurrence";

let boots = 0;

// A signed-in tab holding a verified credential, as App hands it to DateWritesProvider. Only the bootstrap is
// answered; every other request in a test goes through a stubbed service.
export async function verifiedCredential(userId = 5): Promise<Credential> {
  const token = `guarded-${++boots}`;
  vi.stubGlobal("fetch", vi.fn(async (url: RequestInfo | URL) => (String(url).endsWith("/auth/bootstrap") ? bootstrapAnswer({ token, user: { id: userId } }) : new Response("{}", { status: 503 }))));

  return (await bootstrapCredential(true)).credential;
}

// One guarded edit as it reached the transport: its exact bytes and what they say.
export interface SentEdit {
  url: string;
  body: string;
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

// Stands in for the guarded edit's transport (DateEdits.send) for a test: it honours the store's last check
// before fetch, says when it dispatched and records the bytes, then answers as queued. Call it in each test (or
// its beforeEach), after any vi.resetAllMocks.
export function stubEditSends(): EditSends {
  const sent: SentEdit[] = [];
  const queued: Answer[] = [];
  const spy = vi.isMockFunction(DateEdits.send) ? vi.mocked(DateEdits.send) : vi.spyOn(DateEdits, "send");

  spy.mockImplementation(async (envelope, _pinned, gate) => {
    if (!gate.permit()) throw new Error("withheld before fetch");
    gate.onDispatch();
    const body: Omit<SentEdit, "url" | "body"> = JSON.parse(envelope.body);
    sent.push({ url: envelope.url, body: envelope.body, ...body });
    const next = queued.shift() ?? { occurrence: occurrence({ id: envelope.occurrenceId, revision: `r-stored-${sent.length}` }) };

    if (next instanceof Error) throw next;

    return next;
  });

  return { sent, answer: (...answers) => { queued.push(...answers); } };
}

// A stored answer naming the submitted date.
export const storedEdit = (id: number, revision: string): OccurrenceResponse => ({ occurrence: occurrence({ id, revision }) });
