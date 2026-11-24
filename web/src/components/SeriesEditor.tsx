import { Form } from "./ui/Form";
import { childcareBody, childcareFrom } from "./ChildcareField";
import { useEffect, useLayoutEffect, useRef, useState } from "react";
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
import { errorCode, errorText, keptDatesOf } from "../lib/errors";
import { ApiError, credentialCurrent, currentCredential } from "../api/core";
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

// A refused save is sent again at most this many times in all, while the stored event still matches the form.
const MAX_SENDS = 3;

// changed: the stored event differs from what the editor opened with. gone: this person may no longer manage it.
type SeriesConflict = "changed" | "gone";

const lostAuthority = (err: ApiError) => err.status === 403 || err.status === 404;

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
  onConflict?: (why: SeriesConflict) => void;
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
  const mounted = useRef(true);
  useEffect(() => {
    mounted.current = true;

    return () => { mounted.current = false; };
  }, []);
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

    frozenNow.current = true;
    let outcome: "saved" | SeriesConflict = "saved";
    await action.run(async () => {
      try {
        const sent = await send(body);

        if (sent === "refused") {
          setError("We couldn't update the series. Please try again.");

          return false;
        }

        if (sent === "left") return false;

        if (sent !== "saved") {
          outcome = sent;

          return;
        }

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
    }, { followUp: async () => { if (outcome === "saved") onSaved(); else onConflict?.(outcome); } });
  }

  // The save names the read the editor opened with (DEC-162). On a refusal the event is read once: while nothing this
  // form can write has changed (the person's own room booking, a visibility change, a restart) the same save goes
  // again under the new revision; anything else is the page's to show. Repeated refusals alone are never a conflict.
  // Only the editor and the sign-in that pressed Save send again: an answer that lands after the person left the
  // page, or after this tab took another account, sends nothing more.
  async function send(body: EventInput): Promise<"saved" | "refused" | "left" | SeriesConflict> {
    const opened = JSON.stringify(savedFields(instance));
    const credential = currentCredential();
    const sameSignIn = () => (credential === null ? currentCredential() === null : credentialCurrent(credential));
    let revision = instance.revision;

    for (let sends = 1; ; sends++) {
      try {
        await Events.update(instance.eventId, { ...body, revision });

        return "saved";
      } catch (err) {
        // The first save's refusal for lost authority is the form's error, as it always was; a save this editor
        // sent again by itself was let through by a read that said the person still managed the event.
        if (sends > 1 && err instanceof ApiError && lostAuthority(err)) return "gone";

        if (errorCode(err) !== "event_changed") throw err;
      }

      if (!mounted.current) return "left";

      if (!sameSignIn()) return "gone";

      let stored;

      try {
        stored = await Events.get(instance.eventId);
      } catch (err) {
        if (err instanceof ApiError && lostAuthority(err)) return "gone";
        throw err;
      }

      if (!mounted.current) return "left";

      if (!sameSignIn() || !stored.canManage || !stored.event.revision) return "gone";

      if (JSON.stringify(savedFields(stored.event)) !== opened) return "changed";

      if (sends === MAX_SENDS) return "refused";
      revision = stored.event.revision;
    }
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
