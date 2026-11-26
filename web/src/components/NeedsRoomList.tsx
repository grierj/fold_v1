import { useEventReadPin } from "../lib/eventReadPin";
import { useEffect, useRef, useState } from "react";
import { SeriesRooms } from "../api/rooms";
import { errorText } from "../lib/errors";
import { choiceRefused, resourceRefused, type ResourceReadOutcome } from "../lib/refusedSave";
import { useLoad } from "../lib/useLoad";
import { useRoomsEnabled } from "../lib/useSite";
import { formatDayLong } from "../lib/when";
import type { NeedsRoom, RoomBooking } from "../types";
import { RoomField } from "./RoomField";
import { readRooms, roomsTurnedOff } from "./useRoomPicker";
import { useShowAll } from "./useShowAll";
import { Button } from "./ui/Button";
import { FormError } from "./ui/Field";
import { AlertCircleIcon } from "./icons";

const OFF_LIST = "It is no longer listed under “Dates that need a room”.";

// What a row whose Room control was open says when it leaves the list: what the last pick there left, or only that it
// left, when that pick was refused or none was made.
function leftList(day: string, pick: { refused: boolean; stored: RoomBooking | null } | null): string {
  if (pick?.refused) return `${day}. ${choiceRefused("This date's room")} ${OFF_LIST}`;

  const b = pick?.stored;

  if (pick && b === null) return `Room released for ${day}.`;

  if (b?.pending) return `${day} is waiting for an admin to approve ${b.roomName}.`;

  if (b?.status === "confirmed") return `Booked ${b.roomName} for ${day}.`;

  return `${day}. ${OFF_LIST}`;
}

// "Dates that need a room" on the event page (M2.1c, DEC-103): dates whose
// room was taken when they moved, or was busy when the series was booked.
// "Pick a room" opens that date's "Edit this date" when the page lists the
// date, and otherwise the one-date Room control right under the row (the page
// lists only the next few months; this list reaches further). It shows
// nothing while rooms are off or when every date has its room.
export function NeedsRoomList({ eventId, revision, hasDate, onPick, onGone, onRead, onRecheckPage }: {
  eventId: number;
  // Changes whenever a room or date on the page changes, to ask again.
  revision: number;
  // Whether the page lists this date, so its editor can open there.
  hasDate: (date: string) => boolean;
  onPick: (date: string) => void;
  // A row's Room control found this person may no longer manage the date: the page does what it does then.
  onGone: () => void;
  onRead: (outcome: ResourceReadOutcome) => void;
  onRecheckPage: () => void;
}) {
  // Null until the site settings are read; they don't change while this is open.
  // A church with rooms off, or a site we can't read, sees no list at all.
  const enabled = useRoomsEnabled() === true;
  const [own, setOwn] = useState(0);
  // The off-page date whose Room control is open under its row.
  const [open, setOpen] = useState<string | null>(null);
  // What the inline control just did, so the row that leaves the list is not silent.
  const [notice, setNotice] = useState("");
  const watching = useRef<string | null>(null);
  // What the open row's last pick did: refused by someone else's change, or the booking it left. A row that leaves the
  // list says what that was, never that the date has a room it does not hold.
  const lastPick = useRef<{ refused: boolean; stored: RoomBooking | null } | null>(null);
  const noticeRef = useRef<HTMLParagraphElement>(null);

  const pin = useEventReadPin();
  const list = useLoad(() => readRooms(SeriesRooms.list(eventId, ...pin), pin[0]), [eventId], { enabled });
  // A change on the page, or in a row's own control, asks again without blanking the list.
  const stamp = `${revision}:${own}`;
  const asked = useRef(stamp);
  const { reload } = list;
  useEffect(() => {
    if (asked.current === stamp) return;
    asked.current = stamp;

    if (enabled) reload();
  }, [stamp, enabled, reload]);
  const needs: NeedsRoom[] = list.data?.needsRoom ?? [];
  const more = useShowAll(needs);
  // Room booking turned off since the page read the site settings: no list, as with rooms off.
  const error = list.error && !roomsTurnedOff(list.error) ? errorText(list.error, "We couldn't load the dates that need a room.") : null;
  const loaded = list.data;
  useEffect(() => {
    if (resourceRefused(list.error)) onRead("refused");
    else if (!list.error && loaded) onRead("loaded");
  }, [list.error, loaded, onRead]);
  useEffect(() => {
    if (!loaded) return;
    const was = watching.current;

    if (was && !loaded.needsRoom.some((n) => n.date === was)) {
      watching.current = null;
      setOpen(null);
      setNotice(leftList(formatDayLong(was), lastPick.current));
    }
  }, [loaded]);

  useEffect(() => {
    if (notice) noticeRef.current?.focus();
  }, [notice]);

  const notes = notice && <p ref={noticeRef} tabIndex={-1} role="status" className="mt-6 text-sm text-text-muted outline-none">{notice}</p>;

  if (!enabled) return null;

  if (roomsTurnedOff(list.error)) return <>{notes}</>;

  if (error) {
    return (
      <div className="mt-6 space-y-2">
        <FormError>{error}</FormError>
        <Button variant="secondary" onClick={resourceRefused(list.error) ? onRecheckPage : list.retry}>Try again</Button>
      </div>
    );
  }

  if (needs.length === 0) return <>{notes}</>;

  return (
    <>
      {notes}
      <section aria-labelledby="needs-room-title" className="mt-6 rounded-md border border-warning-border bg-warning-subtle p-4">
        <h2 id="needs-room-title" className="text-lg font-bold text-warning-on-subtle">
          <AlertCircleIcon className="mr-1 inline-block size-4 align-text-bottom" />Dates that need a room ({needs.length})
        </h2>
        <p className="mt-1 text-sm text-warning-on-subtle">These dates have no room right now. Pick another one for each.</p>
        <ul className="mt-3 space-y-3">
          {more.rows.map((n, i) => {
            const day = formatDayLong(n.date);
            const inline = !hasDate(n.date);
            const panel = `needs-room-${n.date}`;

            return (
              <li key={n.date} {...more.row(i)} className="space-y-2">
                <div className="flex flex-col gap-2 sm:flex-row sm:items-center sm:justify-between">
                  <div className="min-w-0">
                    <div className="font-semibold">{day}</div>
                    <div className="text-sm text-warning-on-subtle wrap-anywhere">{n.status === "rejected" ? `An admin didn't approve ${n.roomName} for this date.` : `${n.roomName} isn't free then.`}</div>
                  </div>
                  <Button
                    variant="secondary"
                    layout="w-full sm:w-auto"
                    aria-expanded={inline ? open === n.date : undefined}
                    aria-controls={inline ? panel : undefined}
                    onClick={() => {
                      if (!inline) {
                        onPick(n.date);

                        return;
                      }

                      setNotice("");
                      lastPick.current = null;
                      watching.current = open === n.date ? null : n.date;
                      setOpen(open === n.date ? null : n.date);
                    }}
                  >
                    Pick a room{" "}<span className="sr-only">for {day}</span>
                  </Button>
                </div>
                {inline && open === n.date && (
                  <div id={panel} className="rounded-md border border-border bg-surface p-3">
                    <RoomField eventId={eventId} date={n.date} idPrefix={`needs-${n.date}`} focusOnLoad onChanged={(refused, stored) => {
                      // An earlier date's answer can land after another date's picker opened: it is not that date's pick.
                      if (watching.current === n.date) lastPick.current = { refused: refused === true, stored: stored ?? null };

                      setOwn((c) => c + 1);
                    }}
                      onRead={(outcome) => { if (outcome === "refused") onRecheckPage(); }}
                      onGone={() => {
                        watching.current = null;
                        setOpen(null);
                        onGone();
                      }} />
                  </div>
                )}
              </li>
            );
          })}
        </ul>
        {more.button && <div className="mt-3">{more.button}</div>}
      </section>
    </>
  );
}
