import { Form } from "./ui/Form";
import { childcareBody, childcareFrom } from "./ChildcareField";
import { useLayoutEffect, useRef, useState } from "react";
import { Events } from "../api/events";
import { bydays, isSimpleWeekly, isWeekly, weekdayCode, weeklyRule } from "../lib/rrule";
import { useDirty } from "../lib/useDirty";
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
import { errorText, keptDatesOf } from "../lib/errors";
import { useAction, type Action } from "../lib/useAction";
import { answerNames, useRevisionSave, type RevisionConflict } from "../lib/useRevisionSave";
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

// Everything a series save can write, as one read of the event holds it, with an absent value read as the form reads
// it. Two reads that agree here open the same form, so a save made from one overwrites nothing the other changed
// (DEC-162): a field left out would be overwritten without a word.
/** @public its test changes each field in turn. */
export function savedFields(s: Omit<SeriesFields, "eventId" | "revision">) {
  return {
    title: s.title,
    description: s.description,
    startsLocal: s.startsLocal,
    durationMinutes: s.durationMinutes,
    tzid: s.tzid,
    location: s.location,
    recurrenceRule: s.recurrenceRule,
    rruleUntil: s.rruleUntil ?? "",
    capacity: s.capacity ?? null,
    waitlistEnabled: s.waitlistEnabled,
    guestsAllowed: s.guestsAllowed,
    reminderEmailMinutes: s.reminderEmailMinutes ?? DEFAULT_REMINDER_MINUTES,
    reminderPush: s.reminderPush ?? true,
    videoProvider: s.videoProvider ?? "none",
    videoUrl: s.videoUrl ?? "",
    videoReveal: s.videoReveal ?? "registered_only",
    remoteAvailable: s.remoteAvailable ?? false,
    remoteCapacity: s.remoteCapacity ?? null,
    childcareAvailable: s.childcareAvailable ?? false,
    childcareCapacity: s.childcareCapacity ?? null,
    childcareAges: s.childcareAges ?? "",
    childcareNotes: s.childcareNotes ?? "",
  };
}

// SeriesEditor is the one "Edit series" / "Edit event" form, on the event page (DEC-161).
export function SeriesEditor({
  series,
  idPrefix,
  locationHint,
  roomDate,
  onSaved,
  onClose,
  onRoomChanged,
  onAuthorityLost,
  onConflict,
  action: shared,
  locked = false,
}: {
  series: SeriesFields;
  idPrefix: string;
  locationHint?: string;
  // The original date of a single-date event's one occurrence. It turns on the
  // one-date Room control (M2.1b); a series gets the whole-series one (M2.1c).
  roomDate?: string;
  onSaved: () => void;
  onClose?: () => void;
  // The room was booked or released (its own action, not part of Save).
  onRoomChanged?: () => void;
  // The kept-dates panel's read was refused: this person may no longer manage the event.
  onAuthorityLost?: () => void;
  // The save was refused and not sent again: the page shows the event as it is now (DEC-161, DEC-162).
  onConflict?: (why: RevisionConflict) => void;
  // The page's run for this editor, so it can lock what shares the panel (the visibility control) while it runs.
  action?: Action;
  // Another run in the same panel is on its way.
  locked?: boolean;
}) {
  // The save sends the difference from what the editor opened with, so that stays its baseline through any reload:
  // one save per mount (a second one would diff against values it did not show), and a locked editor never changes.
  const [instance] = useState(series);
  // Like `series`: a read that lands while this editor is open never adds or removes a field.
  const [openedRoomDate] = useState(roomDate);
  const simple = isSimpleWeekly(instance.recurrenceRule);

  const when = useWhenState({
    date: instance.startsLocal.slice(0, 10),
    time: instance.startsLocal.slice(11, 16) || "19:00",
    duration: instance.durationMinutes,
    recurring: instance.recurrenceRule !== "",
    days: (() => {
      const picked = bydays(instance.recurrenceRule);

      if (picked.length > 0) return picked;

      if (isWeekly(instance.recurrenceRule)) return [weekdayCode(instance.startsLocal.slice(0, 10))];

      return [];
    })(),
  });

  const { date, time, duration, recurring, days } = when;
  const [tzid, setTzid] = useState(instance.tzid);
  const [title, setTitle] = useState(instance.title);
  const [description, setDescription] = useState(instance.description);
  const [location, setLocation] = useState(instance.location);
  const [rule, setRule] = useState(instance.recurrenceRule);
  const [until, setUntil] = useState(instance.rruleUntil ?? "");

  const initial: EventOptionValues = {
    seats: seatsFrom(instance.capacity, instance.waitlistEnabled, instance.guestsAllowed),
    video: {
      provider: instance.videoProvider ?? "none",
      url: instance.videoUrl ?? "",
      reveal: instance.videoReveal ?? "registered_only",
    },
    childcare: childcareFrom(instance.childcareAvailable, instance.childcareCapacity, instance.childcareAges, instance.childcareNotes),
    remote: remoteFrom(instance.remoteAvailable, instance.remoteCapacity),
    reminder: instance.reminderEmailMinutes ?? DEFAULT_REMINDER_MINUTES,
    reminderPush: instance.reminderPush ?? true,
  };

  const [options, setOptions] = useState(initial);
  // Open on edit, so what is already set is in view (DEC-95).
  const [moreOpen, setMoreOpen] = useState(true);
  const own = useAction();
  const action = shared ?? own;
  const [saved, setSaved] = useState(false);

  // The save names the read the editor opened with (DEC-162); a refusal is the shared protocol's (DEC-163).
  const send = useRevisionSave({
    opened: instance,
    revision: instance.revision,
    read: async () => {
      const stored = await Events.get(instance.eventId);

      return { allowed: stored.canManage, revision: stored.event.revision, value: stored.event };
    },
    fields: savedFields,
    answers: answerNames("event", instance.eventId),
    onConflict,
  });

  // Nothing in the form changes while a save is on its way or after one worked: the save sends what was there when
  // it began, so a later edit would be lost without a word.
  const frozen = action.running || locked || saved;
  // Read when an answer lands (a Jitsi link): one landing on a locked or saved form is dropped. Set in save()
  // too, before the first render that disables the fields.
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

  const dirty = useDirty({ date, time, duration, recurring, days, tzid, title, description, location, rule, until, options });
  const { guard, markSaved } = useUnsavedChanges(dirty);

  useLayoutEffect(() => {
    if (frozen || !focusInvalid.current) return;
    focusInvalid.current = false;

    if (root.current?.contains(document.activeElement)) focusFirstInvalid(root.current);
  }, [frozen, root]);

  async function save(e: React.FormEvent) {
    e.preventDefault();

    if (frozenNow.current) return;

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
    const recurrenceRule = simple ? (recurring ? weeklyRule(days) : "") : rule.trim();
    const { seats, video, childcare, remote, reminder, reminderPush } = options;
    const capTrimmed = seats.capacity.trim();
    const capInitial = initial.seats.capacity;

    const body: EventInput = {
      title: title.trim(),
      description: description.trim(),
      startsLocal: `${date}T${time}`,
      durationMinutes: duration,
      tzid: tzid.trim(),
      location: location.trim(),
      recurrenceRule,
      rruleUntil: until,
    };

    if (JSON.stringify(childcare) !== JSON.stringify(initial.childcare)) Object.assign(body, childcareBody(childcare));

    if (capTrimmed !== capInitial) body.capacity = capTrimmed === "" ? null : Number(capTrimmed);

    if (seats.waitlist !== instance.waitlistEnabled) body.waitlistEnabled = seats.waitlist;

    if (seats.guests !== instance.guestsAllowed) body.guestsAllowed = seats.guests;

    if (reminder !== initial.reminder) body.reminderEmailMinutes = reminder;

    if (reminderPush !== initial.reminderPush) body.reminderPush = reminderPush;

    if (remote.available !== initial.remote.available || remote.capacity.trim() !== initial.remote.capacity) {
      Object.assign(body, remoteBody(remote));
    }

    if (
      video.provider !== initial.video.provider ||
      video.url.trim() !== initial.video.url ||
      video.reveal !== initial.video.reveal
    ) {
      Object.assign(body, videoBody(video));
    }

    // The event as the read shows it once this save is stored: the body over what the editor opened with, plus what
    // PUT /events/:id does beyond the body (it trims the end date and keeps the zone when the body's is empty).
    const intended: SeriesFields = { ...instance, ...body, guestsAllowed: seats.guests, tzid: body.tzid || instance.tzid, rruleUntil: until.trim() };

    frozenNow.current = true;
    await action.run(async () => {
      try {
        const sent = await send((revision) => Events.update(instance.eventId, { ...body, revision }), intended);

        if (sent === "refused") {
          setError("We couldn't update the series. Please try again.");

          return false;
        }

        if (sent !== "saved") return false;

        markSaved();
        setSaved(true);
        kept.close();
      } catch (err) {
        const msg = errorText(err, "We couldn't update the series.");
        const field = optionFieldOfError(err);
        const keptDates = keptDatesOf(err);

        // A refusal for kept dates is told by the panel (DEC-156); with no date to read, the error line says it alone.
        if (keptDates && keptDates.length > 0) {
          kept.open(keptDates);
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
    }, { followUp: async () => onSaved() });
  }

  return (<>
    <Form ref={root} tabIndex={-1} noValidate onSubmit={save} className="mt-3 space-y-4 border-t border-border pt-3">
      <p className="text-sm text-text-muted">
        This changes every future date that has not been edited on its own. Customized dates stay as they are. Dates
        the new schedule drops are cancelled, and people who were going or maybe are told.
      </p>
      {guard}
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
        {instance.recurrenceRule !== "" && <SeriesRoomField eventId={instance.eventId} idPrefix={idPrefix} onChanged={onRoomChanged} />}
        {openedRoomDate && instance.recurrenceRule === "" && (
          <RoomField eventId={instance.eventId} date={openedRoomDate} idPrefix={idPrefix} onChanged={onRoomChanged} />
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
    {kept.refusal && <KeptDates key={kept.refusal.seq} eventId={instance.eventId} keptDates={kept.refusal.dates} onRefused={kept.refused} onClose={kept.close} />}
  </>);
}
