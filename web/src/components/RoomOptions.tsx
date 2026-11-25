import type { RoomSuggestions } from "../types";
import { suggestedRooms } from "../lib/roomSuggestions";

// The room <option>s of a Room field's Select. With suggestions, the
// suggested rooms come first in their own group, with their reasons, and the
// rest follow under "Other rooms"; without, the rooms are listed as before.
// `label` words one room, with the suggestion's reason when it has one.
export function RoomOptions<R extends { id: number }>({ rooms, suggestions, label, disabled, reasonBeside }: {
  rooms: R[];
  suggestions?: RoomSuggestions | null;
  label: (room: R, reason?: string) => string;
  disabled?: (room: R) => boolean;
  reasonBeside?: number;
}) {
  const option = (r: R, reason?: string) => (
    <option key={r.id} value={String(r.id)} disabled={disabled?.(r)}>{label(r, r.id === reasonBeside ? undefined : reason)}</option>
  );

  const groups = suggestedRooms(rooms, suggestions);

  if (!groups) return <>{rooms.map((r) => option(r))}</>;

  return (
    <>
      <optgroup label={groups.label}>{groups.suggested.map(({ room, reason }) => option(room, reason))}</optgroup>
      {groups.others.length > 0 && <optgroup label="Other rooms">{groups.others.map((r) => option(r))}</optgroup>}
    </>
  );
}
