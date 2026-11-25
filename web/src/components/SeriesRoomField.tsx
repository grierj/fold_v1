import { useEffect, useRef, useState } from "react";
import { ApiError } from "../api/core";
import { SeriesRooms } from "../api/rooms";
import { useAuth } from "../auth";
import { roomName } from "../lib/place";
import { RefusedSaveLine, useRefusedSave } from "../lib/refusedSave";
import {
  busyDatesNote, includeLabel, NO_ROOM, ownRoomNote, ownRoomsFor, ownRoomsLead, seriesBookingMessage, seriesRoomLabel, seriesStoredMessage,
} from "../lib/roomLabels";
import { count } from "../lib/plural";
import { useAction } from "../lib/useAction";
import { answerNames, useRevisionSave } from "../lib/useRevisionSave";
import { formatDayLong } from "../lib/when";
import type { OccurrenceRoom, SeriesBooking, SeriesDates, SeriesRoomsResponse } from "../types";
import { Checkbox, Field, FormError, Select } from "./ui/Field";
import { Button } from "./ui/Button";
import { DoubleBookPrompt, useOverlapReason } from "./DoubleBookPrompt";
import { RoomOptions } from "./RoomOptions";
import { pickGone, RoomLoadStatus, RoomsOffLine, roomsTurnedOff, useRoomPicker } from "./useRoomPicker";
import { useShowAll } from "./useShowAll";
import { AlertCircleIcon } from "./icons";

// What a series pick writes: the series room, and, booked anyway over busy dates, those dates (`needs`: the dates that
// need a room, in date order), which a double-booking leaves with none.
interface SeriesRoomFields {
  roomId: number | null;
  needs: string[];
}

const fieldsOf = (defaultRoom: OccurrenceRoom | null, dates: SeriesDates): SeriesRoomFields => ({
  roomId: defaultRoom?.id ?? null,
  needs: dates.needsRoom.map((n) => n.date).sort(),
});

const readFields = (read: SeriesRoomsResponse) => fieldsOf(read.defaultRoom, read);

// What a pick carries that no read shows: an admin's reason, or the dates it names, so a pick whose answer was lost
// counts as this one only when it carried the same.
const unseenOf = (reason: string, dates: string[] = []) => [reason, ...[...dates].sort()].join("\n");

// What a pick compares: the dates that need a room only for "Book anyway", which books over them. A plain pick leaves
// each date to the server (booked, waiting, busy or kept), and intends whatever it stored. The dates with a room of
// their own are never compared: without the box ticked the server keeps every one of them, and with it the request
// names the dates shown, so a list that changed since can only leave more of them as they are (DEC-168).
const compared = (value: SeriesRoomFields, overBusy: boolean): Partial<SeriesRoomFields> => (overBusy ? value : { roomId: value.roomId });

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
  // The series room may have changed (a pick landed, or someone else's change refused it): the page behind reads again
  // quietly and may show new places.
  onChanged?: () => void;
  // This person may no longer manage the event: the host does what it does then.
  onGone: () => void;
}) {
  // A church with rooms off, or a site we can't read, sees no rooms control at all.
  const { enabled, off, list, loading, loadError } = useRoomPicker(() => SeriesRooms.list(eventId), [eventId]);

  if (off) return <RoomsOffLine />;

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
  // Room booking was turned off since the control loaded: it says so in place of the picker.
  const [off, setOff] = useState(false);
  // Whether the pick on its way books over busy dates, and so whether those dates are compared.
  const overBusy = useRef(false);
  const current = read.defaultRoom;
  const { rooms, dates } = read;
  const suggestions = read.suggestions ?? null;
  // The select's choice, not yet booked; a read that shows another series room starts it again.
  const [pick, setPick] = useState(current ? String(current.id) : NO_ROOM);
  // Also change the listed dates this person may change: each pick is its own choice, so a new read clears it.
  const [include, setInclude] = useState(false);
  const [shown, setShown] = useState(read);

  if (read !== shown) {
    setShown(read);
    setPick(current ? String(current.id) : NO_ROOM);
    setInclude(false);
  }

  // An admin's answer for dates that are busy: the room to resend with a reason.
  const [pending, setPending] = useState<{ roomId: number; dates: number } | null>(null);
  const overlap = useOverlapReason(reasonId);
  // The last read the protocol made and the last answer that landed, so the control shows what it judged.
  const lastRead = useRef<SeriesRoomsResponse | null>(null);
  const lastAnswer = useRef<SeriesBooking | null>(null);
  const landedAnswer = () => lastAnswer.current;

  // Only an admin may double-book the dates a read shows busy in the room it booked, and only with a reason.
  const overBusyOffer = (shown: SeriesRoomsResponse, roomId: number | null) =>
    isAdmin && roomId !== null && shown.defaultRoom?.id === roomId && shown.needsRoom.length > 0 ? { roomId, dates: shown.needsRoom.length } : null;

  const save = useRevisionSave<SeriesRoomFields, Partial<SeriesRoomFields>>({
    opened: readFields(read),
    revision: read.revision,
    read: async () => {
      const now = await SeriesRooms.list(eventId);
      lastRead.current = now;

      return { allowed: true, revision: now.revision, value: readFields(now) };
    },
    fields: (value) => compared(value, overBusy.current),
    answers: answerNames("event", eventId),
    stays: () => {
      const res = lastAnswer.current;

      return res ? fieldsOf(res.defaultRoom, res.after) : { roomId: null, needs: [] };
    },
    writesMore: true,
    gone: pickGone,
    onConflict: (why) => {
      if (why === "gone") {
        onGone();

        return;
      }

      const now = lastRead.current;

      if (now) show(now);
      // Booking over busy dates, the read's busy dates in the same room are asked about again; the reason stays.
      setPending(now && overBusy.current && pending ? overBusyOffer(now, pending.roomId) : null);
      refusal.refused(why);
      onChanged?.();
    },
  });

  // An admin's busy-dates answer moves focus to the reason it asks for.
  useEffect(() => {
    if (pending) document.getElementById(reasonId)?.focus();
  }, [pending, reasonId]);

  function send(roomId: number | null, overlapReason?: string, includeDates?: string[]) {
    refusal.clear();
    void action.run(async () => {
      lastAnswer.current = null;
      overBusy.current = overlapReason !== undefined;
      let sent;

      try {
        sent = await save(async (revision) => {
          const body = overlapReason ? { roomId, overlapReason } : includeDates ? { roomId, includeDates } : { roomId };
          const res = await SeriesRooms.set(eventId, body, revision);
          lastAnswer.current = res;

          return res;
        }, { roomId, needs: [] }, unseenOf(overlapReason ?? "", includeDates));
      } catch (e) {
        if (roomsTurnedOff(e)) {
          setOff(true);

          return false;
        }

        throw e;
      }

      if (sent === "refused") throw refusedEverySend();

      if (sent !== "saved") return false;

      overlap.reset();
      onChanged?.();
      const res = landedAnswer();

      if (!res) {
        // Already stored by a send whose answer was lost: the read shows it, and says what each date has.
        const now = lastRead.current;

        if (now) show(now);
        setPending(now && !overlapReason ? overBusyOffer(now, roomId) : null);

        return now ? seriesStoredMessage(now) : roomId === null ? "Series room released." : "The series room is saved.";
      }

      // The next pick is chosen from the dates as the answer read them after this one, never from its counts.
      show({ ...read, revision: res.event.revision, defaultRoom: res.defaultRoom, ...res.after });
      // Only an admin may double-book what stayed busy, and only with a reason.
      setPending(isAdmin && roomId !== null && !overlapReason && res.lost.length > 0 ? { roomId, dates: res.lost.length } : null);

      return seriesBookingMessage(res);
    }, { fallback: "We couldn't update the room." });
  }

  // The box is a choice about the option chosen: a new option starts it clear. The last pick's result goes too: beside
  // another option its counts would read as what that option will do.
  function choose(value: string) {
    setPick(value);
    setInclude(false);
    setPending(null);
    overlap.clearError();
    action.reset();
  }

  function apply() {
    if (action.running || (pick === value && !including)) return;

    const dates = including ? changeable.map((o) => o.date) : undefined;

    if (pick === NO_ROOM) {
      send(null, undefined, dates);

      return;
    }

    if (chosen) send(chosen.id, undefined, dates);
  }

  function bookAnyway() {
    if (!pending) return;
    const why = overlap.read();

    if (why === null) return;
    send(pending.roomId, why);
  }

  const busy = action.running;
  // A series room an admin set that this person can't list (a restricted room) still shows by name.
  const hidden = current && !rooms.some((r) => r.id === current.id) ? current : null;
  const value = current ? String(current.id) : NO_ROOM;
  const chosen = pick === NO_ROOM ? null : (rooms.find((r) => String(r.id) === pick) ?? null);
  const needsCount = read.needsRoom.length;
  const status = action.state.status === "done" ? action.state.text : "";
  const listed = ownRoomsFor(read.ownRooms, pick === NO_ROOM ? null : Number(pick));
  const changeable = listed.filter((o) => o.mayChange);
  const more = useShowAll(listed);
  // The box changes the listed dates to a room this person may book, or, stopping the series room, releases them; with
  // no series room and none chosen there is nothing to do. Book anyway names no dates, so the box goes while it is asked.
  const offerInclude = changeable.length > 0 && !pending && (pick === NO_ROOM ? value !== NO_ROOM : chosen !== null);
  const including = include && offerInclude;
  const busyNote = chosen && pick !== value ? busyDatesNote(chosen, dates, "edit", read.ownRooms.length > 0) : null;

  if (off) return <RoomsOffLine refused />;

  return (
    <>
      <Field
        id={id}
        label="Room for every date"
        optional
        hint="Choose a room, then book it for every upcoming date. If bookings need an admin's approval, they wait for one. A date with a room of its own, booked or waiting for approval, keeps it unless you choose to change it too. “No series room” releases the rooms the series booked."
      >
        <Select value={pick} disabled={busy} aria-busy={busy || undefined} onChange={(e) => choose(e.target.value)}>
          <option value={NO_ROOM}>No series room</option>
          {hidden && <option value={String(hidden.id)}>{roomName(hidden)}</option>}
          <RoomOptions rooms={rooms} suggestions={suggestions} label={(r, reason) => seriesRoomLabel(r, dates, reason)} />
        </Select>
      </Field>
      <RefusedSaveLine save={refusal} noun="room" choosing="The room for every date" />
      {listed.length > 0 && (
        <div className="space-y-2 text-sm">
          <p>{ownRoomsLead(listed.length, including)}</p>
          <ul className="space-y-1">
            {more.rows.map((o, i) => (
              <li key={o.date} {...more.row(i)} className="wrap-anywhere">
                <span className="font-semibold">{formatDayLong(o.date)}</span>: <span className="text-text-muted">{ownRoomNote(o)}</span>
              </li>
            ))}
          </ul>
          {more.button}
          {offerInclude && (
            <Checkbox label={includeLabel(chosen, changeable.length, listed.length)} checked={include} disabled={busy} onChange={(e) => setInclude(e.target.checked)} />
          )}
        </div>
      )}
      <Button variant="secondary" disabled={busy || (pick === value && !including)} onClick={apply}>
        {pick === NO_ROOM ? "Stop the series room" : "Book for every date"}
      </Button>
      <div role="status" className="space-y-2 empty:hidden">
        {busy && <p className="text-sm text-text-muted">Saving…</p>}
        {busyNote && (
          <p className="text-sm text-warning-on-subtle"><AlertCircleIcon className="mr-1 inline-block size-4 align-text-bottom" />{busyNote}</p>
        )}
        {status && <p className="text-sm text-text-muted">{status}</p>}
        {!status && current && read.pending > 0 && (
          <p className="text-sm text-text-muted">Waiting for an admin to approve {roomName(current)} for {count(read.pending, "date")}.</p>
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
