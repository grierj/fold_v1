import { useEffect, useRef, useState } from "react";
import { ApiError } from "../api/core";
import { SeriesRooms } from "../api/rooms";
import { useAuth } from "../auth";
import { roomName } from "../lib/place";
import { RefusedSaveLine, useRefusedSave } from "../lib/refusedSave";
import { busyDatesNote, NO_ROOM, seriesBookingMessage, seriesRoomLabel } from "../lib/roomLabels";
import { count } from "../lib/plural";
import { useAction } from "../lib/useAction";
import { answerNames, useRevisionSave } from "../lib/useRevisionSave";
import type { OccurrenceRoom, SeriesBooking, SeriesRoomsResponse } from "../types";
import { Field, FormError, Select } from "./ui/Field";
import { Button } from "./ui/Button";
import { DoubleBookPrompt, useOverlapReason } from "./DoubleBookPrompt";
import { RoomOptions } from "./RoomOptions";
import { pickGone, RoomLoadStatus, useRoomPicker } from "./useRoomPicker";
import { AlertCircleIcon } from "./icons";

// What a series pick writes: the series room.
interface SeriesRoomFields {
  roomId: number | null;
}

const seriesFields = (room: OccurrenceRoom | null): SeriesRoomFields => ({ roomId: room?.id ?? null });

// Every send was refused while the series room stayed as it was: the control says its ordinary refusal.
const refusedEverySend = () => new ApiError(409, "event changed", { code: "event_changed" });

// The Room control of "Edit series" for a recurring series (M2.1c, DEC-95:
// beside Location, not under More options). Booking is its own action and
// never waits for the form's Save. A date that is busy keeps its
// place on the event's "Dates that need a room" list (DEC-103). It shows
// nothing while rooms are off.
// Unlike RoomField it does not book when the select changes, but on its own
// button: arrow keys on a closed select fire change, and one keypress must
// never book or release dozens of dates.
export function SeriesRoomField({ eventId, idPrefix, onChanged, onGone }: {
  eventId: number;
  idPrefix: string;
  // The series room was booked, changed or released: the page behind may show new places.
  onChanged?: () => void;
  // This person may no longer manage the event: the host does what it does then.
  onGone: () => void;
}) {
  // A church with rooms off, or a site we can't read, sees no rooms control at all.
  const { enabled, list, loading, loadError } = useRoomPicker(() => SeriesRooms.list(eventId), [eventId]);

  if (!enabled && !loadError) return null;

  return (
    <div className="space-y-2">
      <RoomLoadStatus loading={loading} error={loadError} onRetry={list.retry} />
      {!loading && !loadError && list.data && (
        <SeriesRoomControl key={eventId} eventId={eventId} id={`${idPrefix}-series-room`} read={list.data} show={list.setData} onChanged={onChanged} onGone={onGone} />
      )}
    </div>
  );
}

// The control once its read answered: each pick names the event's read it was made from (DEC-167), and the control
// stays on screen after every outcome, so the next pick names what that outcome left (`stays`).
function SeriesRoomControl({ eventId, id, read, show, onChanged, onGone }: {
  eventId: number;
  id: string;
  read: SeriesRoomsResponse;
  show: (next: SeriesRoomsResponse) => void;
  onChanged?: () => void;
  onGone: () => void;
}) {
  const { isAdmin } = useAuth();
  const reasonId = `${id}-reason`;
  const action = useAction();
  const refusal = useRefusedSave();
  const current = read.defaultRoom;
  const { rooms, dates } = read;
  const suggestions = read.suggestions ?? null;
  // The select's choice, not yet booked; a read that shows another series room starts it again.
  const [pick, setPick] = useState(current ? String(current.id) : NO_ROOM);
  const [shown, setShown] = useState(read);

  if (read !== shown) {
    setShown(read);
    setPick(current ? String(current.id) : NO_ROOM);
  }

  // An admin's answer for dates that are busy: the room to resend with a reason.
  const [pending, setPending] = useState<{ roomId: number; dates: number } | null>(null);
  const overlap = useOverlapReason(reasonId);
  // The last read the protocol made and the last answer that landed, so the control shows what it judged.
  const lastRead = useRef<SeriesRoomsResponse | null>(null);
  const lastAnswer = useRef<SeriesBooking | null>(null);
  const landedAnswer = () => lastAnswer.current;

  const save = useRevisionSave<SeriesRoomFields, SeriesRoomFields>({
    opened: seriesFields(current),
    revision: read.revision,
    read: async () => {
      const now = await SeriesRooms.list(eventId);
      lastRead.current = now;

      return { allowed: true, revision: now.revision, value: seriesFields(now.defaultRoom) };
    },
    fields: (value) => value,
    answers: answerNames("event", eventId),
    stays: () => seriesFields(lastAnswer.current?.defaultRoom ?? null),
    gone: pickGone,
    onConflict: (why) => {
      if (why === "gone") {
        onGone();

        return;
      }

      if (lastRead.current) show(lastRead.current);
      setPending(null);
      refusal.refused(why);
    },
  });

  // An admin's busy-dates answer moves focus to the reason it asks for.
  useEffect(() => {
    if (pending) document.getElementById(reasonId)?.focus();
  }, [pending, reasonId]);

  function send(roomId: number | null, overlapReason?: string, over = 0) {
    refusal.clear();
    void action.run(async () => {
      lastAnswer.current = null;

      const sent = await save(async (revision) => {
        const res = await SeriesRooms.set(eventId, overlapReason ? { roomId, overlapReason } : { roomId }, revision);
        lastAnswer.current = res;

        return res;
      }, { roomId }, overlapReason ?? "");

      if (sent === "refused") throw refusedEverySend();

      if (sent !== "saved") return false;

      overlap.reset();
      onChanged?.();
      const res = landedAnswer();

      if (!res) {
        // Already stored by a send whose answer was lost: the read shows it.
        if (lastRead.current) show(lastRead.current);
        setPending(null);

        return roomId === null ? "Series room released." : "The room for every date is saved.";
      }

      show({
        ...read,
        revision: res.event.revision,
        defaultRoom: res.defaultRoom,
        needsRoom: res.defaultRoom ? res.lost.map((date) => ({ date, roomName: res.defaultRoom?.name ?? "", status: "requested" })) : [],
        pending: res.defaultRoom ? res.pending : 0,
      });
      // Only an admin may double-book what stayed busy, and only with a reason.
      setPending(isAdmin && roomId !== null && !overlapReason && res.lost.length > 0 ? { roomId, dates: res.lost.length } : null);

      return seriesBookingMessage(res, over);
    }, { fallback: "We couldn't update the room." });
  }

  function choose(value: string) {
    setPick(value);
    setPending(null);
    overlap.clearError();
  }

  function apply() {
    if (action.running || pick === value) return;

    if (pick === NO_ROOM) {
      send(null);

      return;
    }

    const room = rooms.find((r) => String(r.id) === pick);

    if (room) send(room.id);
  }

  function bookAnyway() {
    if (!pending) return;
    const why = overlap.read();

    if (why === null) return;
    send(pending.roomId, why, pending.dates);
  }

  const busy = action.running;
  // A series room an admin set that this person can't list (a restricted room) still shows by name.
  const hidden = current && !rooms.some((r) => r.id === current.id) ? current : null;
  const value = current ? String(current.id) : NO_ROOM;
  const chosen = pick === NO_ROOM ? null : (rooms.find((r) => String(r.id) === pick) ?? null);
  const needsCount = read.needsRoom.length;
  const status = action.state.status === "done" ? action.state.text : "";

  return (
    <>
      <Field
        id={id}
        label="Room for every date"
        optional
        hint="Choose a room, then book it for every upcoming date. If bookings need an admin's approval, they wait for one. A date you gave its own room keeps it. “No series room” releases the rooms it booked."
      >
        <Select value={pick} aria-busy={busy || undefined} onChange={(e) => choose(e.target.value)}>
          <option value={NO_ROOM}>No series room</option>
          {hidden && <option value={String(hidden.id)}>{roomName(hidden)}</option>}
          <RoomOptions rooms={rooms} suggestions={suggestions} label={(r, reason) => seriesRoomLabel(r, dates, reason)} />
        </Select>
      </Field>
      <RefusedSaveLine save={refusal} noun="room" choosing="The room for every date" />
      <Button variant="secondary" disabled={busy || pick === value} onClick={apply}>
        {pick === NO_ROOM ? "Stop the series room" : "Book for every date"}
      </Button>
      <div role="status" className="space-y-2 empty:hidden">
        {busy && <p className="text-sm text-text-muted">Saving…</p>}
        {chosen && pick !== value && busyDatesNote(chosen, dates, "edit") && (
          <p className="text-sm text-warning-on-subtle"><AlertCircleIcon className="mr-1 inline-block size-4 align-text-bottom" />{busyDatesNote(chosen, dates, "edit")}</p>
        )}
        {status && <p className="text-sm text-text-muted">{status}</p>}
        {!status && current && read.pending > 0 && (
          <p className="text-sm text-text-muted">Waiting for an admin to approve this room for {count(read.pending, "date")}.</p>
        )}
        {current && dates > 0 && needsCount >= dates && (
          <p className="text-sm text-warning-on-subtle"><AlertCircleIcon className="mr-1 inline-block size-4 align-text-bottom" />Not booked on any date yet: every date is busy.</p>
        )}
      </div>
      {action.state.status === "failed" && <FormError>{action.state.text}</FormError>}
      {pending && (
        <DoubleBookPrompt reasonId={reasonId} prompt={overlap} busy={busy} onBookAnyway={bookAnyway}
          message={`${pending.dates === 1 ? "1 date is" : `${pending.dates} dates are`} in use. Give a reason to book ${pending.dates === 1 ? "it" : "them"} anyway.`} />
      )}
    </>
  );
}
