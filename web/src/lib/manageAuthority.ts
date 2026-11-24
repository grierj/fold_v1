import type { CancelledEventDate, EventDate } from "../types";
import { outcomeOf } from "./rosterWrites";

// What the date page's latest load says about managing this date. "granted": the latest settled answer is a
// scheduled date with canManage. "checking": a load is in flight. "unknown": the latest attempt failed without
// a readable refusal. "lost": any other settled answer (a tombstone, no canManage, a readable refusal, closed).
export type ManageAuthority = "granted" | "checking" | "unknown" | "lost";

interface DateLoad {
  loading: boolean;
  error: unknown;
  data: EventDate | CancelledEventDate | undefined;
}

// The error is read before the data: a failed quiet reload that still holds an old canManage is not evidence,
// so it reads unknown.
export function manageAuthority(load: DateLoad): ManageAuthority {
  if (load.loading) return "checking";

  if (load.error != null) return outcomeOf(load.error) === "refused" ? "lost" : "unknown";
  const d = load.data;

  return d && !("cancelled" in d) && d.canManage === true ? "granted" : "lost";
}
