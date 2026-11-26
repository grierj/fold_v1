import type { ComponentProps } from "react";
import type { OccurrenceRoom, RoomSuggestions } from "../types";
import { roomName } from "../lib/place";
import { suggestedRooms } from "../lib/roomSuggestions";
import { Select } from "./ui/Field";

type NamedRoom = Pick<OccurrenceRoom, "id" | "name" | "building">;

type RoomSelectProps<R extends NamedRoom> = Omit<ComponentProps<typeof Select>, "options"> & {
  rooms: R[];
  suggestions?: RoomSuggestions | null;
  label: (room: R, reason?: string) => string;
  unavailable?: (room: R) => boolean;
  reasonBeside?: number;
  fallback?: NamedRoom | null;
};

export function RoomSelect<R extends NamedRoom>({ rooms, suggestions, label, unavailable, reasonBeside, fallback, value, children, ...select }: RoomSelectProps<R>) {
  const option = (r: R, reason?: string) => (
    <option key={r.id} value={String(r.id)} disabled={unavailable?.(r)}>{label(r, r.id === reasonBeside ? undefined : reason)}</option>
  );

  const groups = suggestedRooms(rooms, suggestions);
  const selectedId = String(value);
  const selected = rooms.find((room) => String(room.id) === selectedId) ?? (fallback && String(fallback.id) === selectedId ? fallback : null);

  return (
    <>
      <Select {...select} value={value}>
        {children}
        {fallback && <option value={String(fallback.id)}>{roomName(fallback)}</option>}
        {groups ? (
          <>
            <optgroup label={groups.label}>{groups.suggested.map(({ room, reason }) => option(room, reason))}</optgroup>
            {groups.others.length > 0 && <optgroup label="Other rooms">{groups.others.map((r) => option(r))}</optgroup>}
          </>
        ) : rooms.map((room) => option(room))}
      </Select>
      {selected && <p aria-hidden="true" className="mt-1 text-sm text-text-muted wrap-anywhere">{roomName(selected)}</p>}
    </>
  );
}
