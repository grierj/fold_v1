import { useId, useRef, useState, type ReactNode, type RefObject } from "react";
import type { EventDate, EventDateChange, EventDatePool } from "../types";
import { Button, ButtonLink } from "./ui/Button";
import { EventIntro } from "./EventIntro";
import { seatsLeft } from "./Attendance";
import { coverSrc } from "../lib/covers";
import { focalPosition } from "../lib/image";
import { formatEventDay, formatTime, heldIn } from "../lib/when";
import { dateLine } from "../lib/repeat";
import { placeLine, roomName } from "../lib/place";
import { VISIBILITY_WORD } from "../lib/visibilityChange";
import { answerLine } from "./AnswerLine";

// The exact-date page's six answers (M8.1 brief 1, 5.1): what it is, when, where, whether you're in,
// who else is coming and what is different about this date. Everything here renders what the date
// read carries; who may see what is decided by the server (projectDate). Facts only an ordinary
// reader gets are read only when the read carries the ordinary visibility, so a grant-only read
// shows its narrow facts even if more arrived (brief 4).

// The page's heading: a shown group's cover beside the title, then the group and its leader, or the
// church label. A grant-only reader gets the title alone. The heading takes focus when the page has nothing nearer to offer.
export function DateIdentity({ occurrence, heading }: { occurrence: EventDate; heading?: RefObject<HTMLHeadingElement | null> }) {
  const ordinary = !!occurrence.visibility;
  // Only a group the reader may see has a header (brief 2.3): no cover for a church-wide or hidden-group date.
  const header = ordinary && !occurrence.hostedBy && occurrence.groupId && occurrence.groupName ? occurrence.groupHeader : undefined;
  const leader = header?.leaderName.trim();

  return <div data-date-answer="identity" className="flex items-start gap-3">
    {header && occurrence.groupId && <img src={coverSrc({ id: occurrence.groupId, imageUrl: header.imageUrl })} alt="" width={64} height={64}
      className="size-16 shrink-0 rounded-lg bg-sunken object-cover" style={{ objectPosition: focalPosition(header.coverFocalX, header.coverFocalY) }} />}
    <div className="min-w-0">
      <h1 ref={heading} tabIndex={-1} className="text-3xl font-bold">{occurrence.title}</h1>
      {/* hostedBy with no group can be a church-wide event or a hidden group's fallback, so it keeps "Hosted by" (DEC-99). */}
      {ordinary && <EventIntro hostedBy={occurrence.hostedBy} groupName={occurrence.groupName} groupId={occurrence.groupId} />}
      {header && leader && <p className="text-sm text-text-muted">Led by {leader}</p>}
    </div>
  </div>;
}

// Answers 2 and 3: when, and where with the remote state.
export function DateWhenWhere({ occurrence }: { occurrence: EventDate }) {
  const zone = heldIn(occurrence.startsAt, occurrence.tzid);
  const place = placeLine(occurrence.room, occurrence.location);

  return <>
    <div data-date-answer="when">
      <p className="font-medium">{dateLine(occurrence.startsAt, occurrence.endsAt)}</p>
      {zone && <p className="text-sm text-text-muted">{zone}</p>}
    </div>
    <div data-date-answer="where">
      <p>{place || "No meeting place listed"}</p>
      <RemoteState occurrence={occurrence} ordinary={!!occurrence.visibility} />
    </div>
  </>;
}

// Whether a read carries names the list may show: an ordinary read with at least one attendee name. The page
// decides from this whether an open list may stay open, so the list and the page never disagree.
export const carriesNames = (occurrence: EventDate) => !!occurrence.visibility && !!comingOf(occurrence)?.lists.some((l) => l.people.length > 0);

// Answers 5 and 6: who else is coming and what is different, then the names, opened on request. Whether the
// list is open is the page's: it outlives this component through the page's own read after a write.
export function DateComing({ occurrence, namesOpen, onNamesOpenChange }: { occurrence: EventDate; namesOpen: boolean; onNamesOpenChange: (open: boolean) => void }) {
  const [shownNames, setShownNames] = useState(25);
  const ordinary = !!occurrence.visibility;
  const change = ordinary ? changeSentence(occurrence) : null;
  const coming = ordinary ? comingOf(occurrence) : null;
  const hasNames = carriesNames(occurrence);
  const open = namesOpen && hasNames;
  const listId = useId();
  const namesList = useRef<HTMLDivElement>(null);

  let count = 0;

  const lists = coming?.lists.map((list) => {
    const shown = list.people.slice(0, Math.max(0, shownNames - count));
    count += list.people.length;

    return { ...list, shown };
  }) ?? [];

  return <>
    {coming && <div data-date-answer="attendance">
      <div className="flex flex-wrap items-center gap-x-3">
        <p>{coming.going} going, {coming.maybe} maybe</p>
        {hasNames && <Button variant="link" aria-expanded={open} aria-controls={listId} onClick={() => {
          if (!open) setShownNames(25);
          onNamesOpenChange(!open);
        }}>{open ? "Hide names" : "Show names"}</Button>}
      </div>
      {coming.hidden.length > 0 && <p className="text-sm text-text-muted">Some {coming.hidden.join(" and ")} names are private.</p>}
      {coming.pools.length > 0 && <p className="text-sm text-text-muted">{coming.pools.map((p, i) => <span key={i}>{i > 0 && " "}{p}</span>)}</p>}
    </div>}
    {change && <p data-date-answer="change" className="rounded-md bg-sunken px-3 py-2 text-sm">{change}</p>}
    {/* The names open after the six answers, so opening them never pushes the difference down. */}
    {coming && open && <div id={listId} ref={namesList} role="group" aria-label="Attendee names" tabIndex={-1} className="space-y-3">
      {lists.flatMap((l) => l.shown.length > 0 ? [<section key={l.label} aria-label={`${l.label} names`}>
        <h2 className="text-sm font-semibold">{l.label} ({l.people.length})</h2>
        <ul className="text-sm">{l.shown.map((a, i) => <li key={i}>{a.name}{a.guestCount > 0 && ` (+${a.guestCount} ${a.guestCount === 1 ? "guest" : "guests"})`}</li>)}</ul>
      </section>] : [])}
      {count > shownNames && <Button variant="link" onClick={() => {
        if (shownNames + 25 >= count) namesList.current?.focus({ preventScroll: true });
        setShownNames((current) => current + 25);
      }}>Show more names</Button>}
    </div>}
  </>;
}

// Remote RSVP and link access are two separate answers (brief 4.3): one never implies the other.
function RemoteState({ occurrence, ordinary }: { occurrence: EventDate; ordinary: boolean }) {
  const join = occurrence.canSeeVideo && occurrence.videoUrl;

  // Closed and in-person-only are capacity facts only an ordinary read carries; a grant-only read
  // says remote is available when it is, and nothing when it isn't.
  const remote = ordinary && occurrence.remoteRsvp
    ? { offered: "Remote attendance available", closed: "Remote sign-up is closed for this date", none: "In person only" }[occurrence.remoteRsvp]
    : occurrence.remoteAvailable ? "Remote attendance available" : "";

  const takesRemote = ordinary && (occurrence.remoteRsvp === "offered" || occurrence.remoteRsvp === "closed");

  return <>
    {remote && <p className="text-sm text-text-muted">{remote}</p>}
    {join && <div className="mt-2"><ButtonLink variant="secondary" size="sm" to={occurrence.videoUrl!} external>Join video call (opens in a new tab)</ButtonLink></div>}
    {!join && occurrence.videoHidden && <p className="text-sm text-text-muted">{ordinary
      // An ordinary reader is shown a registered-only link once they are Going (or manage the event).
      ? "The video link is shown to people marked Going."
      : "The video link is not available to you. Check your answer or return to this page to check again."}</p>}
    {!join && !occurrence.videoHidden && takesRemote && <p className="text-sm text-text-muted">No video link listed.</p>}
  </>;
}

// The viewer's own answer, in words, for the page's one RSVP status (DEC-92), which the page owns. Series standing is
// inferred from coverage alone (brief 2.1): Going on a visible repeating series whose every
// eligible upcoming date the viewer answered Going or Maybe.
export function DateStanding({ occurrence }: { occurrence: EventDate }) {
  const ordinary = !!occurrence.visibility;
  const status = occurrence.myRegistration;
  const series = ordinary && occurrence.series?.recurrenceRule ? occurrence.series : undefined;
  const upcoming = series?.upcomingDates ?? 0;
  const covered = series?.coveredDates ?? 0;
  const full = upcoming > 0 && covered === upcoming;
  const position = ordinary && status === "waitlisted" ? occurrence.myWaitlistPosition : undefined;
  const lines: ReactNode[] = [];

  if (series && upcoming > 0 && covered > 0 && (status === "going" || status === "maybe" || status === null) && !(status === "going" && full)) {
    lines.push(covered === upcoming ? `You've answered all ${upcoming} upcoming dates.` : `You've answered ${covered} of ${upcoming} upcoming dates.`);
  }

  if (status === "waitlisted") {
    if (position && position > 0) lines.push(`Your position on this date's waitlist is ${position}.`);
    // What happens today (brief 2.4, 3): no deadline, no confirmation, no promise a leader acts.
    lines.push("If a space opens and your party fits, you may be moved to Going. Fold will let you know if that happens.");
  }

  // A cancelled row carries no mode or guests (brief 2.1), so nothing old shows as attendance.
  if (occurrence.myAttendanceMode) {
    lines.push(<>Attending: {occurrence.myAttendanceMode === "remote" ? "Remote" : "In person"}{occurrence.myGuestCount !== undefined && <span className="block">Guests: {occurrence.myGuestCount}</span>}</>);
  }

  return <>
    <p className="font-semibold">{standingSentence(status, series ? (full ? "full" : upcoming > 0 ? "partial" : "none") : "none")}</p>
    {lines.map((line, i) => <p key={i} className="text-sm text-text-muted">{line}</p>)}
  </>;
}

function standingSentence(status: EventDate["myRegistration"], coverage: "full" | "partial" | "none"): ReactNode {
  switch (status) {
    case "going": return coverage === "full" ? "You're in for the series." : coverage === "partial" ? "You're in for this date only." : "You're going to this date.";
    case "maybe": return answerLine(status);
    case "declined": return "You said you can't go.";
    case "waitlisted": return "You're on the waitlist.";
    case "removed": return "Your sign-up for this date was removed.";
    case "cancelled": return "Your sign-up was cancelled.";
    default: return "You're not in yet.";
  }
}

// Answer rows first (people who answered, not seats), then the pools, then names only where the
// read carries them. Each answer's names are compared with its own row count.
function comingOf(occurrence: EventDate) {
  const attendance = occurrence.attendance;

  if (!attendance) return null;
  const going = attendance.registrations.going ?? 0;
  const maybe = attendance.registrations.maybe ?? 0;
  const named = occurrence.canSeeAttendees ? attendance.attendees ?? [] : [];

  const lists = [
    { label: "Going", people: named.filter((a) => a.status === "going"), rows: going },
    { label: "Maybe", people: named.filter((a) => a.status === "maybe"), rows: maybe },
  ];

  const hidden = occurrence.canSeeAttendees ? lists.flatMap((l) => l.rows > l.people.length ? [l.label.toLowerCase()] : []) : [];

  return { going, maybe, lists, hidden, pools: poolSummary(occurrence) };
}

// A closed remote pool is said once, in the where row, and never offers its waitlist.
function poolSummary(occurrence: EventDate): ReactNode[] {
  const seats = occurrence.seats;

  if (!seats) return [];
  const out: ReactNode[] = [];

  const summarize = (pool: EventDatePool, kind: "" | "remote ") => {
    if (pool.capacity === null) {
      out.push(`${kind ? "Remote: " : "In person: "}${pool.used} taken, no limit.`);

      return;
    }

    const left = pool.left ?? 0;
    out.push(<>{seatsLeft(left, pool.capacity, kind)}{left <= 0 && seats.waitlistEnabled ? ", waitlist open." : "."}</>);
  };

  summarize(seats.inPerson, "");

  if (seats.remote && occurrence.remoteRsvp !== "closed") summarize(seats.remote, "remote ");

  if (seats.childcare) {
    const totals = seats.childcare.totals;

    if (seats.childcare.full) out.push("Childcare is full.");
    else if (totals?.capacity != null) out.push(`${totals.left ?? 0} of ${totals.capacity} childcare spots left.`);
    else out.push("Childcare available.");
  }

  return out;
}

// Each pool's usage beside its limit, for a reader the read gives seats; the manager's declined
// count stays here, below the first screen, never as names.
export function DateSeats({ occurrence }: { occurrence: EventDate }) {
  const seats = occurrence.visibility ? occurrence.seats : undefined;
  const declined = occurrence.visibility ? occurrence.attendance?.registrations.declined : undefined;

  if (!seats && declined === undefined) return null;
  const used = (pool: EventDatePool) => pool.capacity === null ? `${pool.used} taken, no limit` : `${pool.used} of ${pool.capacity} taken`;
  const rows: [string, string][] = [];

  if (seats) {
    rows.push(["In person", used(seats.inPerson)]);

    if (seats.remote && occurrence.remoteRsvp !== "closed") rows.push(["Remote", used(seats.remote)]);

    if (seats.childcare?.totals) rows.push(["Childcare", used(seats.childcare.totals)]);
  }

  if (declined !== undefined) rows.push(["Can't go", String(declined)]);

  return <div>
    <h2 className="font-semibold">Seats and answers</h2>
    <ul className="text-sm text-text-muted">{rows.map(([label, value]) => <li key={label}>{label}: {value}</li>)}</ul>
  </div>;
}

const SUBJECTS: Record<EventDateChange, string> = {
  day: "day", time: "start time", duration: "length", place: "place", room: "room",
  capacity: "seat limit", remoteCapacity: "remote seat limit", visibility: "visibility",
};

// One sentence for what differs from the series' usual values (brief 4.4). The server decides what
// changed; this only says it, with the value the page shows. An unknown usual clock never yields a
// time claim, so a supplied time change without one is dropped.
function changeSentence(occurrence: EventDate): string | null {
  const series = occurrence.series;

  if (!series?.recurrenceRule) return null;
  const changes = [...new Set(series.changes)].filter((c) => c in SUBJECTS && (c !== "time" || !!series.usual?.startsLocal));

  if (changes.length === 0) return null;

  if (changes.length > 1) {
    const words = changes.map((c) => SUBJECTS[c]);

    return `The ${words.slice(0, -1).join(", ")} and ${words[words.length - 1]} differ for this date.`;
  }

  return oneChange(changes[0], occurrence);
}

function oneChange(change: EventDateChange, occurrence: EventDate): string {
  const seats = occurrence.seats;

  const limit = (capacity: number | null | undefined, kind: string, subject: string) => {
    if (capacity === undefined) return `This date's ${subject} is different from usual.`;

    if (capacity === null) return `This date has no ${subject}, unlike usual.`;

    return `This date has ${capacity} ${kind}${capacity === 1 ? "seat" : "seats"}, a different limit from usual.`;
  };

  switch (change) {
    case "day": return `This date is on ${formatEventDay(occurrence.startsAt)}, a different day from usual.`;
    case "time": return `This date starts at ${formatTime(occurrence.startsAt)}, a different time from usual.`;
    case "duration": return `This date lasts ${minutes(occurrence.durationMinutes)}, a different length from usual.`;
    case "place": return occurrence.location.trim() ? `This date meets at ${occurrence.location.trim()}, a different place from usual.` : "This date's place is different from usual.";
    case "room": return occurrence.room ? `This date is in ${roomName(occurrence.room)}, a different room from usual.` : "This date has no room booked, unlike usual.";
    case "capacity": return limit(seats ? seats.inPerson.capacity : undefined, "", "seat limit");
    case "remoteCapacity": return seats?.remote?.capacity === 0 ? "This date takes no remote sign-ups, unlike usual." : limit(seats ? seats.remote?.capacity : undefined, "remote ", "remote seat limit");
    case "visibility": return `This date is ${VISIBILITY_WORD[occurrence.visibility!]}, unlike usual.`;
  }
}

function minutes(total: number): string {
  const hours = Math.floor(total / 60);
  const rest = total % 60;
  const part = (n: number, unit: string) => `${n} ${unit}${n === 1 ? "" : "s"}`;

  if (hours === 0) return part(rest, "minute");

  return rest === 0 ? part(hours, "hour") : `${part(hours, "hour")} ${part(rest, "minute")}`;
}
