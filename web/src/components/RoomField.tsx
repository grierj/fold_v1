import { useEffect, useRef, useState, type KeyboardEvent } from "react";
import { DateRooms, type DateRoomInput } from "../api/rooms";
import { useAuth } from "../auth";
import { errorCode } from "../lib/errors";
import { roomName } from "../lib/place";
import { RefusedSaveLine, useRefusedSave } from "../lib/refusedSave";
import { NO_ROOM, roomOptionLabel } from "../lib/roomLabels";
import { outcomeOf } from "../lib/rosterWrites";
import { useAction } from "../lib/useAction";
import { answerNames, refusedAgain, useManagedSave } from "../lib/useRevisionSave";
import type { DateRoomsResponse, RoomBooking, RoomBookingResponse } from "../types";
import { Field, FormError, Input, Select } from "./ui/Field";
import { Button } from "./ui/Button";
import { DoubleBookPrompt, useOverlapReason } from "./DoubleBookPrompt";
import { RoomOptions } from "./RoomOptions";
import { pickGone, RoomLoadStatus, RoomsOffLine, roomsTurnedOff, useRoomPicker } from "./useRoomPicker";
import { AlertCircleIcon } from "./icons";

// What a pick of a date's room writes, as the server stores it: the room the date holds or waits for, or none. A date
// with no booking and one whose booking lost its room or was not approved are different (`lost`), so a release is
// already stored only when the date has no booking. The padding is what the server stored.
interface DateRoomFields {
  roomId: number | null;
  lost?: true;
  setup?: number;
  teardown?: number;
}

function roomFields(booking: RoomBooking | null): DateRoomFields {
  if (booking === null) return { roomId: null };

  if (!(booking.status === "confirmed" || booking.pending)) return { roomId: null, lost: true };

  return { roomId: booking.roomId, setup: booking.setupMinutes, teardown: booking.teardownMinutes };
}

// What a pick compares: the padding only for "Save times", which sets it. A pick that leaves the padding to the server
// (the room's usual minutes, or the same room's own) intends whatever the server chose for that room.
function compared(value: DateRoomFields, padding: boolean): DateRoomFields {
  if (padding) return value;

  const { roomId, lost } = value;

  return lost ? { roomId, lost } : { roomId };
}

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
  // The date's room may have changed (a pick landed, with the booking it left, `stored`; or someone else's change refused
  // it, `refused`): the page behind reads again quietly and may show the new place.
  onChanged?: (refused?: boolean, stored?: RoomBooking | null) => void;
  // This person may no longer manage the date: the host does what it does then.
  onGone: () => void;
  // Put the cursor in the Room select once the rooms load (a "Pick a room" action opened this).
  focusOnLoad?: boolean;
}) {
  const id = `${idPrefix}-room`;
  // A church with rooms off, or a site we can't read, sees no rooms control at all.
  const { enabled, off, list, loading, loadError } = useRoomPicker(() => DateRooms.list(eventId, date), [eventId, date]);

  const focused = useRef(false);
  useEffect(() => {
    if (!focusOnLoad || focused.current || loading || loadError || !enabled) return;
    focused.current = true;
    document.getElementById(id)?.focus();
  }, [focusOnLoad, loading, loadError, enabled, id]);

  if (off) return <RoomsOffLine />;

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
  onChanged?: (refused?: boolean, stored?: RoomBooking | null) => void;
  onGone: () => void;
}) {
  const { isAdmin } = useAuth();
  // Room booking was turned off since the control loaded: it says so in place of the picker.
  const [off, setOff] = useState(false);
  // Whether the pick on its way sets the padding ("Save times"), and so whether the padding is compared.
  const padding = useRef(false);
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

  const save = useManagedSave<DateRoomsResponse, DateRoomFields, DateRoomFields, RoomBookingResponse>({
    opened: read,
    revision: read.dateRoom.revision,
    read: async () => {
      const now = await DateRooms.list(eventId, date);

      return { allowed: true, revision: now.dateRoom.revision, row: now };
    },
    value: (r) => roomFields(r.booking),
    stored: (res) => ({ ...read, booking: res.booking, dateRoom: res.dateRoom }),
    fields: (value) => compared(value, padding.current),
    answers: answerNames("dateRoom", read.dateRoom.id),
    stays: true,
    gone: pickGone,
    changedCode: "date_room_changed",
    onConflict: (why, now) => {
      if (why === "gone") {
        onGone();

        return;
      }

      if (now) show(now);
      setPending(null);
      refusal.refused(why);
      onChanged?.(true);
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
      padding.current = body.setupMinutes !== undefined;
      let stored: DateRoomsResponse | undefined;

      try {
        const { sent, row } = await save((revision) => DateRooms.set(eventId, date, body, revision), intended, body.overlapReason ?? "");

        if (sent === "refused") throw refusedAgain("date_room_changed");

        if (sent !== "saved") return false;
        stored = row;
      } catch (e) {
        if (roomsTurnedOff(e)) {
          setOff(true);

          return false;
        }

        // An admin may book over a conflict, with a reason.
        if (errorCode(e) === "room_busy" && isAdmin && body.roomId !== null) {
          setPending(body);

          return false;
        }

        // A "Book anyway" that may have been stored keeps its prompt and reason, so the next press settles it.
        if (!(body.overlapReason && outcomeOf(e) === "unsure")) setPending(null);
        throw e;
      }

      // Landed, the answer is what was stored; already stored by a send whose answer was lost, the read is.
      if (stored) show(stored);
      setPending(null);
      overlap.reset();
      onChanged?.(false, stored?.booking ?? null);

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
    send({ roomId: room.id }, { roomId: room.id }, (b) => (b?.pending ? "" : `Booked ${room.name}.`));
  }

  function bookAnyway() {
    if (!pending || pending.roomId === null) return;
    const why = overlap.read();

    if (why === null) return;
    const room = rooms.find((r) => r.id === pending.roomId);

    // "Save times" asked: the minutes it sent are what the booking would hold.
    const intended = pending.setupMinutes !== undefined && pending.teardownMinutes !== undefined
      ? { roomId: pending.roomId, setup: pending.setupMinutes, teardown: pending.teardownMinutes }
      : { roomId: pending.roomId };

    send({ ...pending, overlapReason: why }, intended, () => `Booked ${room?.name ?? "the room"} over another booking.`);
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

  if (off) return <RoomsOffLine refused />;

  const busy = action.running;
  const waiting = booking?.pending === true;
  const rejected = booking?.status === "rejected";
  const requested = booking?.status === "requested" && !waiting;
  const value = booking && (booking.status === "confirmed" || waiting) ? String(booking.roomId) : requested ? "" : NO_ROOM;
  const selectedSuggestion = suggestions?.rooms.find((suggestion) => String(suggestion.roomId) === value);
  const suggestionId = selectedSuggestion ? `${id}-suggestion` : undefined;
  const anyBusy = rooms.some((r) => r.busy);
  // A room the date holds that this person can't book (a restricted room or a room turned off) still shows, by the
  // name the date's place gives it.
  const held = value !== "" && value !== NO_ROOM && booking && !rooms.some((r) => r.id === booking.roomId) ? booking : null;
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
        <Select value={value} aria-describedby={suggestionId} aria-busy={busy || undefined} onChange={(e) => choose(e.target.value)}>
          {requested && <option value="">Pick a room</option>}
          <option value={NO_ROOM}>No room</option>
          {held && <option value={String(held.roomId)}>{roomName({ name: held.roomName, building: held.building })}</option>}
          <RoomOptions rooms={rooms} suggestions={suggestions} label={roomOptionLabel} reasonBeside={selectedSuggestion?.roomId} disabled={(r) => r.busy && !isAdmin} />
        </Select>
        {selectedSuggestion && <p id={suggestionId} className="mt-1 text-sm text-text-muted wrap-anywhere">{selectedSuggestion.reason}</p>}
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
