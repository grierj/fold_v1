import { Form } from "./ui/Form";
import { childcareBody, childcareFrom } from "./ChildcareField";
import { useLayoutEffect, useRef, useState, type RefObject } from "react";
import { Events } from "../api/events";
import { bydays, isSimpleWeekly, isWeekly, weekdayCode, weeklyRule } from "../lib/rrule";
import { useUnsavedChanges } from "../lib/unsavedChanges";
import { useWhenState } from "../lib/useWhenState";
import { Field, FormError, Input, Textarea, focusFirstInvalid, showFieldErrors } from "./ui/Field";
import { FormActions } from "./ui/FormActions";
import { Button } from "./ui/Button";
import { EventOptions } from "./EventOptions";
import { DEFAULT_REMINDER_MINUTES } from "./ReminderField";
import { WhenFields } from "./WhenFields";
import { RoomField } from "./RoomField";
import { SeriesRoomField } from "./SeriesRoomField";
import { seatsFrom } from "./SeatsField";
import { videoBody } from "./VideoField";
import { remoteBody, remoteFrom } from "./RemoteField";
import { errorCode, errorText, keptDatesOf } from "../lib/errors";
import { useAction, type Action } from "../lib/useAction";
import { useFocusHome, useLockedFocus } from "../lib/focusHome";
import { KeptDates } from "./KeptDates";
import { useKeptRefusal } from "./keptRefusal";
import { optionFieldOfError, validateEventOptions, type EventOptionValues, type OptionErrors } from "./eventOptionValues";
import type { EventInput, VideoProvider, VideoReveal } from "../types";


export interface SeriesFields {
  eventId: number;
  startsLocal: string;
  durationMinutes: number;
  tzid: string;
  title: string;
  description: string;
  location: string;
  recurrenceRule: string;
  rruleUntil?: string;
  capacity: number | null;
  waitlistEnabled: boolean;
  guestsAllowed: number;
  // The reminder time (M3.6b); absent reads as the default.
  reminderEmailMinutes?: number;
  // The push reminder switch (M3.13d3); absent reads as on.
  reminderPush?: boolean;
  videoProvider?: VideoProvider;
  videoUrl?: string;
  videoReveal?: VideoReveal;
  childcareAvailable?: boolean;
  childcareCapacity?: number | null;
  childcareAges?: string;
  childcareNotes?: string;
  remoteAvailable?: boolean;
  remoteCapacity?: number | null;
  // The read's series token, which the save names (DEC-162).
  revision?: string;
}

const EDITING = "This event changed while you were editing. Check it before you save.";

// Whether a rule repeats and on which weekdays, as the form opens it.
function repeatOf(rule: string, date: string) {
  const picked = bydays(rule);

  return { recurring: rule !== "", days: picked.length > 0 ? picked : isWeekly(rule) ? [weekdayCode(date)] : [] };
}

// The rule the weekday chips save. With no day picked it is a plain weekly rule, which opens on the first date's weekday.
function chipsRule(recurring: boolean, days: string[]) {
  if (!recurring) return "";

  return days.length > 0 ? weeklyRule(days) : "FREQ=WEEKLY";
}

// What the form shows for a read of the event before anything is typed. The repeat is one field, its rule: whether it
// repeats, its weekdays and a custom rule are taken or kept together. Its keys are in the order of the draft's.
function formOf(s: SeriesFields) {
  const options: EventOptionValues = {
    seats: seatsFrom(s.capacity, s.waitlistEnabled, s.guestsAllowed),
    video: {
      provider: s.videoProvider ?? "none",
      url: s.videoUrl ?? "",
      reveal: s.videoReveal ?? "registered_only",
    },
    childcare: childcareFrom(s.childcareAvailable, s.childcareCapacity, s.childcareAges, s.childcareNotes),
    remote: remoteFrom(s.remoteAvailable, s.remoteCapacity),
    reminder: s.reminderEmailMinutes ?? DEFAULT_REMINDER_MINUTES,
    reminderPush: s.reminderPush ?? true,
  };

  return {
    date: s.startsLocal.slice(0, 10), time: s.startsLocal.slice(11, 16) || "19:00", duration: s.durationMinutes,
    tzid: s.tzid, title: s.title, description: s.description, location: s.location,
    rule: s.recurrenceRule, until: s.rruleUntil ?? "", options,
  };
}

type Draft = ReturnType<typeof formOf>;

const same = <T,>(a: T, b: T) => JSON.stringify(a) === JSON.stringify(b);

// Two drafts show the same thing when a weekly rule names the same days the chips show for it ("FREQ=WEEKLY" opens on
// the first date's weekday, so an untouched form over it is unchanged).
function shown(f: Draft): Draft {
  const rule = f.rule.trim();

  if (!isSimpleWeekly(rule)) return { ...f, rule };
  const { recurring, days } = repeatOf(rule, f.date);

  return { ...f, rule: chipsRule(recurring, days) };
}

const sameForm = (a: Draft, b: Draft) => same(shown(a), shown(b));

// Each field the stored event changed since `was` takes the stored value; the others keep what was typed.
function rebased<T extends object>(draft: T, was: T, now: T): T {
  const out = { ...draft };
  // SAFETY: every T here is a plain object of form values built by formOf or a field's helper, so its own keys are T's keys.
  const keys = Object.keys(now) as Array<keyof T>;

  for (const key of keys) if (!same(was[key], now[key])) out[key] = now[key];

  return out;
}

// The options go field by field inside each group (a changed seat limit leaves typed guests alone).
function rebasedDraft(draft: Draft, was: Draft, now: Draft): Draft {
  const options = {
    ...rebased(draft.options, was.options, now.options),
    seats: rebased(draft.options.seats, was.options.seats, now.options.seats),
    video: rebased(draft.options.video, was.options.video, now.options.video),
    remote: rebased(draft.options.remote, was.options.remote, now.options.remote),
    childcare: rebased(draft.options.childcare, was.options.childcare, now.options.childcare),
  };

  return { ...rebased(draft, was, now), options };
}

interface EditorProps {
  series: SeriesFields;
  idPrefix: string;
  locationHint?: string;
  // The original date of a single-date event's one occurrence. It turns on the
  // one-date Room control (M2.1b); a series gets the whole-series one (M2.1c).
  roomDate?: string;
  onSaved: () => void;
  onClose?: () => void;
  // A save was refused because the event changed since the read this editor follows: read it again.
  onStale?: () => void;
  // The room was booked or released (its own action, not part of Save).
  onRoomChanged?: () => void;
  // The kept-dates panel's read was refused: this person may no longer manage the event.
  onAuthorityLost?: () => void;
  // The page's run for this editor, so it can lock what shares the panel (the visibility control) while it runs.
  action?: Action;
  // Another run in the same panel is on its way.
  locked?: boolean;
}

// SeriesEditor is the one "Edit series" / "Edit event" form, on the event page (DEC-161). It follows the page's newest
// read: the save sends what differs from it, with its revision, and the server refuses a save made from an older read
// (DEC-162). A read that changes what the form shows starts the form again from the rebased draft, so nothing a field
// worked out when it opened (the Other duration, a Jitsi link on its way) outlives the values it was worked out from.
export function SeriesEditor({ series, roomDate, onStale, action: shared, locked = false, ...rest }: EditorProps) {
  const stored = formOf(series);
  const [seen, setSeen] = useState({ form: stored, revision: series.revision });
  const [mount, setMount] = useState({ key: 0, start: stored });
  const draft = useRef(stored);
  const [notice, setNotice] = useState<{ text: string } | null>(null);
  const noticeRef = useRef<HTMLDivElement>(null);
  // A read that lands while this editor is open never adds or removes the room field under the person.
  const [openedRoom] = useState({ date: roomDate, series: series.recurrenceRule !== "" });
  const own = useAction();
  const action = shared ?? own;
  const frozen = action.running || locked;

  // Not while a save is on its way: its answer belongs to the form that sent it. The render after it catches up.
  if (!frozen && (series.revision !== seen.revision || !same(stored, seen.form))) {
    const next = rebasedDraft(draft.current, seen.form, stored);

    if (!sameForm(next, draft.current)) {
      setMount({ key: mount.key + 1, start: next });
      setNotice({ text: notice?.text ?? EDITING });
    }

    setSeen({ form: stored, revision: series.revision });
  }

  // Each notice is brought into view, and takes focus when focus was in the form: a restart removes the focused field
  // (focus is then nowhere), and after a refused save it is on Save.
  useLayoutEffect(() => {
    const el = noticeRef.current;

    if (!notice || !el) return;
    el.scrollIntoView?.({ block: "nearest" });
    const active = document.activeElement;

    if (!active || active === document.body || el.closest("form")?.contains(active)) el.focus({ preventScroll: true });
  }, [notice]);

  return (
    <SeriesForm
      key={mount.key}
      {...rest}
      eventId={series.eventId}
      openedRoom={openedRoom}
      start={mount.start}
      base={seen.form}
      revision={seen.revision}
      draft={draft}
      notice={notice?.text ?? null}
      noticeRef={noticeRef}
      onEdit={() => { if (notice) setNotice(null); }}
      onChanged={(text) => {
        setNotice({ text });
        onStale?.();
      }}
      action={action}
      locked={locked}
    />
  );
}

function SeriesForm({
  eventId,
  idPrefix,
  locationHint,
  openedRoom,
  start,
  base,
  revision,
  draft: reported,
  notice,
  noticeRef,
  onEdit,
  onChanged,
  onSaved,
  onClose,
  onRoomChanged,
  onAuthorityLost,
  action,
  locked,
}: Omit<EditorProps, "series" | "roomDate" | "onStale" | "action" | "locked"> & {
  eventId: number;
  openedRoom: { date?: string; series: boolean };
  // The draft this form opens with; `base` is the newest read, which the save compares with.
  start: Draft;
  base: Draft;
  revision?: string;
  // Written every render, so the editor rebases what is on screen.
  draft: RefObject<Draft>;
  notice: string | null;
  noticeRef: RefObject<HTMLDivElement | null>;
  // The person changed a field: the notice has done its job.
  onEdit: () => void;
  // The save was refused because the event changed (DEC-162).
  onChanged: (message: string) => void;
  action: Action;
  locked: boolean;
}) {
  const simple = isSimpleWeekly(start.rule);
  const when = useWhenState({ date: start.date, time: start.time, duration: start.duration, ...repeatOf(start.rule, start.date) });
  const { date, time, duration, recurring, days } = when;
  const [tzid, setTzid] = useState(start.tzid);
  const [title, setTitle] = useState(start.title);
  const [description, setDescription] = useState(start.description);
  const [location, setLocation] = useState(start.location);
  const [rule, setRule] = useState(start.rule);
  const [until, setUntil] = useState(start.until);
  const [options, setOptions] = useState(start.options);
  const recurrenceRule = simple ? chipsRule(recurring, days) : rule.trim();
  const current: Draft = { date, time, duration, tzid, title, description, location, rule: recurrenceRule, until, options };
  reported.current = current;

  // Open on edit, so what is already set is in view (DEC-95).
  const [moreOpen, setMoreOpen] = useState(true);
  // Nothing in the form changes while a save is on its way: the save sends what was there when it began, so a later
  // edit would be lost without a word.
  const frozen = action.running || locked;
  // Read when an answer lands (a Jitsi link): one landing on a locked form is dropped. Set in save() too, before
  // the first render that disables the fields.
  const frozenNow = useRef(frozen);
  frozenNow.current = frozen;
  const [error, setError] = useState<string | null>(null);
  const [optionErrors, setOptionErrors] = useState<OptionErrors>({});
  const home = useFocusHome<HTMLFormElement>();
  const root = home.ref;
  const lockFocus = useLockedFocus(home, frozen);
  // A refused option is focused once the save ends and the fields can take focus again, if focus is still in the form.
  const focusInvalid = useRef(false);
  const submit = useRef<HTMLButtonElement>(null);
  const kept = useKeptRefusal({ initial: null, submit, onRefused: setError, onAuthorityLost });

  const { guard, markSaved } = useUnsavedChanges(!sameForm(current, base));

  useLayoutEffect(() => {
    if (frozen || !focusInvalid.current) return;
    focusInvalid.current = false;

    if (root.current?.contains(document.activeElement)) focusFirstInvalid(root.current);
  }, [frozen, root]);

  async function save(e: React.FormEvent) {
    e.preventDefault();

    if (frozenNow.current) return;
    onEdit();

    if (simple && recurring && days.length === 0) {
      setError("Pick at least one day to repeat weekly.");

      return;
    }

    const problem = validateEventOptions(options);

    if (problem) {
      showFieldErrors(root.current, () => {
        setError(problem.message);
        setOptionErrors({ [problem.field]: problem.message });
        setMoreOpen(true);
      });

      return;
    }

    setOptionErrors({});
    setError(null);
    const { seats, video, childcare, remote, reminder, reminderPush } = options;
    const capTrimmed = seats.capacity.trim();
    const was = base.options;

    const body: EventInput = {
      title: title.trim(),
      description: description.trim(),
      startsLocal: `${date}T${time}`,
      durationMinutes: duration,
      tzid: tzid.trim(),
      location: location.trim(),
      recurrenceRule,
      rruleUntil: until,
      revision,
    };

    if (!same(childcare, was.childcare)) Object.assign(body, childcareBody(childcare));

    if (capTrimmed !== was.seats.capacity) body.capacity = capTrimmed === "" ? null : Number(capTrimmed);

    if (seats.waitlist !== was.seats.waitlist) body.waitlistEnabled = seats.waitlist;

    if (seats.guests !== was.seats.guests) body.guestsAllowed = seats.guests;

    if (reminder !== was.reminder) body.reminderEmailMinutes = reminder;

    if (reminderPush !== was.reminderPush) body.reminderPush = reminderPush;

    if (remote.available !== was.remote.available || remote.capacity.trim() !== was.remote.capacity) {
      Object.assign(body, remoteBody(remote));
    }

    if (
      video.provider !== was.video.provider ||
      video.url.trim() !== was.video.url ||
      video.reveal !== was.video.reveal
    ) {
      Object.assign(body, videoBody(video));
    }

    frozenNow.current = true;
    await action.run(async () => {
      try {
        await Events.update(eventId, body);
        markSaved();
        kept.close();
      } catch (err) {
        const msg = errorText(err, "We couldn't update the series.");
        const field = optionFieldOfError(err);
        const keptDates = keptDatesOf(err);

        // A refusal for kept dates is told by the panel (DEC-156); with no date to read, the error line says it alone.
        if (keptDates && keptDates.length > 0) {
          kept.open(keptDates);
        } else if (errorCode(err) === "event_changed") {
          // The draft stays; the page's next read brings the new revision and the stored values it changed.
          onChanged(msg);
        } else if (field) {
          lockFocus.forget();
          focusInvalid.current = true;
          setError(msg);
          setOptionErrors({ [field]: msg });
          setMoreOpen(true);
        } else {
          setError(msg);
        }

        return false;
      }
    }, { followUp: async () => { onSaved(); } });
  }

  return (<>
    <Form ref={root} tabIndex={-1} noValidate onSubmit={save} onChange={onEdit} className="mt-3 space-y-4 border-t border-border pt-3">
      <p className="text-sm text-text-muted">
        This changes every future date that has not been edited on its own. Customized dates stay as they are. Dates
        the new schedule drops are cancelled, and people who were going or maybe are told.
      </p>
      {guard}
      {notice && <div ref={noticeRef} tabIndex={-1}><FormError>{notice}</FormError></div>}
      {error && <FormError>{error}</FormError>}
      <fieldset disabled={frozen} className="min-w-0 space-y-4">
        <Field id={`s-title-${idPrefix}`} label="Title">
          <Input value={title} onChange={(e) => setTitle(e.target.value)} />
        </Field>
        <Field id={`s-desc-${idPrefix}`} label="Description" optional>
          <Textarea
            rows={3}
            maxLength={5000}
            value={description}
            onChange={(e) => setDescription(e.target.value)}
          />
        </Field>
        <WhenFields
          idPrefix={`s-${idPrefix}`}
          when={when}
          tzid={tzid}
          onTzid={setTzid}
          zoneHint="The time zone the times above are in. The clock stays put across daylight saving."
          dateLabel="First date"
          customRule={simple ? undefined : { value: rule, onChange: setRule }}
          until={{ value: until, onChange: setUntil }}
        />
        <Field id={`s-loc-${idPrefix}`} label="Location" optional hint={locationHint}>
          <Input value={location} onChange={(e) => setLocation(e.target.value)} />
        </Field>
        {openedRoom.series && <SeriesRoomField eventId={eventId} idPrefix={idPrefix} onChanged={onRoomChanged} />}
        {openedRoom.date && !openedRoom.series && (
          <RoomField eventId={eventId} date={openedRoom.date} idPrefix={idPrefix} onChanged={onRoomChanged} />
        )}
        <EventOptions
          idPrefix={idPrefix}
          open={moreOpen}
          onOpenChange={setMoreOpen}
          values={options}
          onChange={(next) => { if (!frozenNow.current) setOptions(next); }}
          errors={optionErrors}
        />
      </fieldset>
      <FormActions>
        <Button ref={submit} variant="primary" layout="w-full sm:w-auto" type="submit" disabled={frozen}>
          {action.running ? "Saving…" : "Save series"}
        </Button>
        {onClose && (
          <Button variant="secondary" layout="w-full sm:w-auto" disabled={action.running || locked} onClick={onClose}>
            Close
          </Button>
        )}
      </FormActions>
    </Form>
    {/* After the form, never inside it: the panel's roster has forms of its own. */}
    {kept.refusal && <KeptDates key={kept.refusal.seq} eventId={eventId} keptDates={kept.refusal.dates} onRefused={kept.refused} onClose={kept.close} />}
  </>);
}
