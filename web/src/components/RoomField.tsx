import { useEffect, useRef, useState, type KeyboardEvent } from "react";
import { ApiError } from "../api/core";
import { DateRooms, type DateRoomInput } from "../api/rooms";
import { useAuth } from "../auth";
import { errorCode } from "../lib/errors";
import { RefusedSaveLine, useRefusedSave } from "../lib/refusedSave";
import { NO_ROOM, roomOptionLabel } from "../lib/roomLabels";
import { useAction } from "../lib/useAction";
import { answerNames, useRevisionSave } from "../lib/useRevisionSave";
import type { DateRoomsResponse, RoomBooking, RoomBookingResponse } from "../types";
import { Field, FormError, Input, Select } from "./ui/Field";
import { Button } from "./ui/Button";
import { DoubleBookPrompt, useOverlapReason } from "./DoubleBookPrompt";
import { RoomOptions } from "./RoomOptions";
import { pickGone, RoomLoadStatus, useRoomPicker } from "./useRoomPicker";
import { AlertCircleIcon } from "./icons";

// What a pick of a date's room writes, as the server stores it: the room the date holds or waits for (null when it
// holds none, so a room someone booked on a date that had lost its room is a change), and for an admin the padding.
interface DateRoomFields {
  roomId: number | null;
  setup?: number;
  teardown?: number;
}

// Padding a pick leaves that the control cannot know (another room's usual minutes): never equal to a read.
const UNKNOWN_MINUTES = -1;

function roomFields(booking: RoomBooking | null, admin: boolean): DateRoomFields {
  if (booking === null || !(booking.status === "confirmed" || booking.pending)) return { roomId: null };

  return admin ? { roomId: booking.roomId, setup: booking.setupMinutes, teardown: booking.teardownMinutes } : { roomId: booking.roomId };
}

// Every send was refused while nothing the pick writes changed: the control says its ordinary refusal.
const refusedEverySend = () => new ApiError(409, "date room changed", { code: "date_room_changed" });

// The Room control of "Edit this date" and the single-date "Edit event"
// (M2.1b, DEC-95: beside Location, not under More options). Booking is its own
// action with its own conflict answer, so a choice is saved at once and never
// waits for the form's Save. While it loads or fails, the rest of the form
// still works. It shows nothing while rooms are off.
export function RoomField({ eventId, date, idPrefix, onChanged, onGone, focusOnLoad }: {
  eventId: number;
  // The occurrence's original YYYY-MM-DD.
  date: string;
  idPrefix: string;
  // A booking was made, changed or released: the page behind may show the new place.
  onChanged?: () => void;
  // This person may no longer manage the date: the host does what it does then.
  onGone: () => void;
  // Put the cursor in the Room select once the rooms load (a "Pick a room" action opened this).
  focusOnLoad?: boolean;
}) {
  const id = `${idPrefix}-room`;
  // A church with rooms off, or a site we can't read, sees no rooms control at all.
  const { enabled, list, loading, loadError } = useRoomPicker(() => DateRooms.list(eventId, date), [eventId, date]);

  const focused = useRef(false);
  useEffect(() => {
    if (!focusOnLoad || focused.current || loading || loadError || !enabled) return;
    focused.current = true;
    document.getElementById(id)?.focus();
  }, [focusOnLoad, loading, loadError, enabled, id]);

  if (!enabled && !loadError) return null;

  return (
    <div className="space-y-2">
      <RoomLoadStatus loading={loading} error={loadError} onRetry={list.retry} />
      {!loading && !loadError && list.data && (
        <DateRoomControl key={`${eventId}:${date}`} eventId={eventId} date={date} id={id} read={list.data} show={list.setData} onChanged={onChanged} onGone={onGone} />
      )}
    </div>
  );
}

// The control once its read answered: each pick names the read it was made from (DEC-167), and the control stays on
// screen after every outcome, so the next pick names what that outcome left (`stays`).
function DateRoomControl({ eventId, date, id, read, show, onChanged, onGone }: {
  eventId: number;
  date: string;
  id: string;
  read: DateRoomsResponse;
  show: (next: DateRoomsResponse) => void;
  onChanged?: () => void;
  onGone: () => void;
}) {
  const { isAdmin } = useAuth();
  const reasonId = `${id}-reason`;
  const action = useAction();
  const refusal = useRefusedSave();
  // An admin's conflict answer: the booking to resend with a reason.
  const [pending, setPending] = useState<DateRoomInput | null>(null);
  const overlap = useOverlapReason(reasonId);
  const [padOpen, setPadOpen] = useState(false);
  const [setup, setSetup] = useState("");
  const [teardown, setTeardown] = useState("");
  const [padError, setPadError] = useState<{ field: "setup" | "teardown"; text: string } | null>(null);
  const setupRef = useRef<HTMLInputElement>(null);
  const teardownRef = useRef<HTMLInputElement>(null);
  const { rooms, booking } = read;
  const suggestions = read.suggestions ?? null;
  // The last read the protocol made and the last answer that landed, so the control shows what it judged.
  const lastRead = useRef<DateRoomsResponse | null>(null);
  const lastAnswer = useRef<RoomBookingResponse | null>(null);
  const landedAnswer = () => lastAnswer.current;

  const save = useRevisionSave<DateRoomFields, DateRoomFields>({
    opened: roomFields(booking, isAdmin),
    revision: read.dateRoom.revision,
    read: async () => {
      const now = await DateRooms.list(eventId, date);
      lastRead.current = now;

      return { allowed: true, revision: now.dateRoom.revision, value: roomFields(now.booking, isAdmin) };
    },
    fields: (value) => value,
    answers: answerNames("dateRoom", read.dateRoom.id),
    stays: () => roomFields(lastAnswer.current?.booking ?? null, isAdmin),
    gone: pickGone,
    changedCode: "date_room_changed",
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

  // An admin's conflict answer moves focus to the reason it asks for.
  useEffect(() => {
    if (pending) document.getElementById(reasonId)?.focus();
  }, [pending, reasonId]);

  // A new booking prefills the padding override with what it holds.
  useEffect(() => {
    setSetup(booking ? String(booking.setupMinutes) : "");
    setTeardown(booking ? String(booking.teardownMinutes) : "");
    setPadError(null);
  }, [booking]);

  function send(body: DateRoomInput, intended: DateRoomFields, done: (b: RoomBooking | null) => string) {
    refusal.clear();
    void action.run(async () => {
      lastAnswer.current = null;

      try {
        const sent = await save(async (revision) => {
          const res = await DateRooms.set(eventId, date, body, revision);
          lastAnswer.current = res;

          return res;
        }, intended, body.overlapReason ?? "");

        if (sent === "refused") throw refusedEverySend();

        if (sent !== "saved") return false;
      } catch (e) {
        // An admin may book over a conflict, with a reason.
        if (errorCode(e) === "room_busy" && isAdmin && body.roomId !== null) {
          setPending(body);

          return false;
        }

        setPending(null);
        throw e;
      }

      // Landed, the answer is what was stored; already stored by a send whose answer was lost, the read is.
      const res = landedAnswer();
      const stored = res ? { ...read, booking: res.booking, dateRoom: res.dateRoom } : lastRead.current;

      if (stored) show(stored);
      setPending(null);
      overlap.reset();
      onChanged?.();

      return done(stored?.booking ?? null);
    }, { fallback: "We couldn't update the room." });
  }

  function choose(value: string) {
    if (action.running) return;
    setPending(null);
    overlap.clearError();

    if (value === "") return;

    if (value === NO_ROOM) {
      send({ roomId: null }, { roomId: null }, () => "Room released.");

      return;
    }

    const room = rooms.find((r) => String(r.id) === value);

    if (!room) return;
    send({ roomId: room.id }, intendedPick(room.id), (b) => (b?.pending ? "" : `Booked ${room.name}.`));
  }

  // The room as the server stores the pick: the same room keeps its padding; another room's usual minutes are unknown.
  function intendedPick(roomId: number): DateRoomFields {
    if (!isAdmin) return { roomId };

    const held = roomFields(booking, true);

    return held.roomId === roomId ? held : { roomId, setup: UNKNOWN_MINUTES, teardown: UNKNOWN_MINUTES };
  }

  function bookAnyway() {
    if (!pending || pending.roomId === null) return;
    const why = overlap.read();

    if (why === null) return;
    const room = rooms.find((r) => r.id === pending.roomId);
    send({ ...pending, overlapReason: why }, intendedPick(pending.roomId), () => `Booked ${room?.name ?? "the room"} over another booking.`);
  }

  function savePadding() {
    if (!booking) return;
    const s = Number(setup);
    const t = Number(teardown);
    const ok = (n: number, raw: string) => raw.trim() !== "" && Number.isInteger(n) && n >= 0 && n <= 240;

    if (!ok(s, setup) || !ok(t, teardown)) {
      const field = ok(s, setup) ? "teardown" : "setup";
      setPadError({ field, text: "Enter a whole number of minutes from 0 to 240 for both." });
      (field === "setup" ? setupRef : teardownRef).current?.focus();

      return;
    }

    setPadError(null);
    send({ roomId: booking.roomId, setupMinutes: s, teardownMinutes: t }, { roomId: booking.roomId, setup: s, teardown: t }, () => "Setup and cleanup times saved.");
  }

  const busy = action.running;
  const waiting = booking?.pending === true;
  const rejected = booking?.status === "rejected";
  const requested = booking?.status === "requested" && !waiting;
  const value = booking && (booking.status === "confirmed" || waiting) ? String(booking.roomId) : requested ? "" : NO_ROOM;
  const anyBusy = rooms.some((r) => r.busy);
  const done = action.state.status === "done" ? action.state.text : "";

  const onPaddingEnter = (e: KeyboardEvent<HTMLInputElement>) => {
    // Enter here is "save the times", not "submit the editor around this field".
    if (e.key !== "Enter") return;
    e.preventDefault();
    savePadding();
  };

  return (
    <>
      <Field id={id} label="Room" optional hint="Saved as soon as you choose it, so you don't need to press Save. If bookings need an admin's approval, it waits for one.">
        <Select value={value} aria-busy={busy || undefined} onChange={(e) => choose(e.target.value)}>
          {requested && <option value="">Pick a room</option>}
          <option value={NO_ROOM}>No room</option>
          <RoomOptions rooms={rooms} suggestions={suggestions} label={roomOptionLabel} disabled={(r) => r.busy && !isAdmin} />
        </Select>
      </Field>
      <RefusedSaveLine save={refusal} noun="room" choosing="This date's room" />
      {!isAdmin && anyBusy && (
        <p className="text-sm text-text-muted">A room that's in use can only be booked by an admin. Ask an admin if you need it.</p>
      )}
      <div role="status" className="space-y-2 empty:hidden">
        {busy && <p className="text-sm text-text-muted">Saving…</p>}
        {done && <p className="text-sm text-text-muted">{done}</p>}
        {waiting && <p className="text-sm text-text-muted">Waiting for an admin to approve {booking?.roomName ?? "the room"}.</p>}
        {rejected && (
          <p className="rounded-md border border-warning-border bg-warning-subtle p-3 text-sm text-warning-on-subtle">
            <AlertCircleIcon className="mr-1 inline-block size-4 align-text-bottom" />An admin didn't approve {booking?.roomName} for this date. Pick another room.
          </p>
        )}
        {requested && (
          <p className="rounded-md border border-warning-border bg-warning-subtle p-3 text-sm text-warning-on-subtle">
            <AlertCircleIcon className="mr-1 inline-block size-4 align-text-bottom" />{booking?.roomName} was taken at the new time, so this date has no room. Pick another room.
          </p>
        )}
        {(booking?.status === "confirmed" || waiting) && booking?.tooSmall && (
          <p className="text-sm text-warning-on-subtle">
            <AlertCircleIcon className="mr-1 inline-block size-4 align-text-bottom" />This room seats fewer people than this date allows.
          </p>
        )}
      </div>
      {action.state.status === "failed" && <FormError>{action.state.text}</FormError>}
      {pending && <DoubleBookPrompt reasonId={reasonId} prompt={overlap} busy={busy} onBookAnyway={bookAnyway} message="That room is in use at this time. Give a reason to book it anyway." />}
      {isAdmin && booking?.status === "confirmed" && (
        <div>
          <Button variant="link" aria-expanded={padOpen} aria-controls={`${id}-padding`} onClick={() => setPadOpen((v) => !v)}>
            Setup and cleanup times
          </Button>
          <div id={`${id}-padding`} hidden={!padOpen} className="mt-2 space-y-2">
            {padOpen && (
              <>
                <p className="text-sm text-text-muted">These start as the room's usual times. Changing them affects this booking only.</p>
                <div className="grid gap-3 sm:grid-cols-2">
                  <Field id={`${id}-setup`} label="Setup minutes" error={padError?.field === "setup" ? padError.text : null}>
                    <Input ref={setupRef} type="number" inputMode="numeric" min={0} max={240} value={setup} onChange={(e) => setSetup(e.target.value)} onKeyDown={onPaddingEnter} />
                  </Field>
                  <Field id={`${id}-teardown`} label="Cleanup minutes" error={padError?.field === "teardown" ? padError.text : null}>
                    <Input ref={teardownRef} type="number" inputMode="numeric" min={0} max={240} value={teardown} onChange={(e) => setTeardown(e.target.value)} onKeyDown={onPaddingEnter} />
                  </Field>
                </div>
                <Button variant="secondary" disabled={busy} onClick={savePadding}>Save times</Button>
              </>
            )}
          </div>
        </div>
      )}
    </>
  );
}
