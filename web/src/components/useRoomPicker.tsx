import type { ApiError } from "../api/core";
import { errorCode, errorText } from "../lib/errors";
import { useLoad } from "../lib/useLoad";
import { useRoomsEnabled } from "../lib/useSite";
import { Button } from "./ui/Button";
import { FormError } from "./ui/Field";
import { Skeleton } from "./ui/Skeleton";

// What the room pickers share: a church with rooms off, or a site we can't
// read, shows no rooms control at all; and the rooms load only while it is on.
// `off`: room booking was turned off after the page read the site settings (the route's rooms_off).
export function useRoomPicker<T>(fetch: () => Promise<T>, deps: readonly unknown[]) {
  const roomsOn = useRoomsEnabled();
  const enabled = roomsOn === true;
  const list = useLoad(fetch, deps, { enabled });
  const off = roomsTurnedOff(list.error);

  return {
    enabled,
    off,
    list,
    loading: roomsOn === null || list.loading,
    loadError: list.error && !off ? errorText(list.error, "We couldn't load the rooms.") : null,
  };
}

// The rooms routes say room booking was turned off (rooms_off), as opposed to this person losing the event or date.
export const roomsTurnedOff = (cause: unknown) => errorCode(cause) === "rooms_off";

// What a room control shows in place of its picker once room booking was turned off: nothing to retry, and the editor
// around it carries on.
export function RoomsOffLine() {
  return <p role="status" className="text-sm text-text-muted">Room booking has been turned off, so rooms can't be picked here now.</p>;
}

export function RoomLoadStatus({ loading, error, onRetry }: { loading: boolean; error: string | null; onRetry: () => void }) {
  return <>
    {loading && <Skeleton kind="lines" count={1} label="Loading rooms…" />}
    {error && (
      <div className="space-y-2">
        <FormError>{error}</FormError>
        <Button variant="secondary" onClick={onRetry}>Try again</Button>
      </div>
    )}
  </>;
}

// A pick's refusal that says this person may no longer manage the date or event (the event's 403, a missing event or
// date's 404), as opposed to one about the room they chose (room_restricted, room_gone, booking_admin_only) or room
// booking being turned off (rooms_off).
export const pickGone = (err: ApiError) => (err.status === 403 && err.code === "not_allowed") || (err.status === 404 && err.code === "not_found");
