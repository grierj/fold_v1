import { isNotTeen } from "./can";
import { Households } from "../api/households";
import type { User } from "../types";
import { errorText } from "./errors";
import { useLoad } from "./useLoad";
import type { Credential } from "../api/core";

// One read per page/viewer; date cards share the same selectable people.
export function useHousehold(user: User | null, pinned?: Credential | null) {
  const load = useLoad(
    async () => (await Households.mine(pinned ?? undefined)).household?.people ?? [],
    [user?.id, user?.accountType],
    { enabled: pinned !== null && isNotTeen(user), scope: pinned },
  );

  return { people: load.data ?? [], error: load.error ? errorText(load.error, "We couldn't load your household.") : null, retry: load.retry };
}
