import type { ComponentProps } from "react";
import type { OccurrenceRoom, RoomSuggestions } from "../types";
import { roomName } from "../lib/place";
import { suggestedRooms } from "../lib/roomSuggestions";
import { ChevronDownIcon } from "./icons";
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
  const optionLabel = (room: R, reason?: string) => label(room, room.id === reasonBeside ? undefined : reason);

  const option = (r: R, reason?: string) => (
    <option key={r.id} value={String(r.id)} disabled={unavailable?.(r)}>{optionLabel(r, reason)}</option>
  );

  const groups = suggestedRooms(rooms, suggestions);
  const selectedId = String(value);
  const selected = rooms.find((room) => String(room.id) === selectedId);
  const fallbackLabel = fallback ? roomName(fallback) : null;

  const selectedLabel = selected
    ? optionLabel(selected, groups?.suggested.find(({ room }) => room.id === selected.id)?.reason)
    : fallback && String(fallback.id) === selectedId ? fallbackLabel : null;

  return (
    <div className={selectedLabel ? "room-select" : undefined}>
      <Select {...select} value={value}>
        {children}
        {fallback && <option value={String(fallback.id)}>{fallbackLabel}</option>}
        {groups ? (
          <>
            <optgroup label={groups.label}>{groups.suggested.map(({ room, reason }) => option(room, reason))}</optgroup>
            {groups.others.length > 0 && <optgroup label="Other rooms">{groups.others.map((r) => option(r))}</optgroup>}
          </>
        ) : rooms.map((room) => option(room))}
      </Select>
      {selectedLabel && (
        <div aria-hidden="true" className="input room-select-value">
          <span data-selected-option className="min-w-0 wrap-anywhere">{selectedLabel}</span>
          <ChevronDownIcon className="size-4 shrink-0" />
        </div>
      )}
    </div>
  );
}
