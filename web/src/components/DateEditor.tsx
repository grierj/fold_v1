import { Form } from "./ui/Form";
import { useCallback, useEffect, useLayoutEffect, useRef, useState, useSyncExternalStore, type MutableRefObject } from "react";
import { bootstrapCredential } from "../api/core";
import type { DateEditChanges } from "../api/events";
import type { KeptDateCount, Visibility } from "../types";
import { errorText, keptDatesOf } from "../lib/errors";
import { formatDayFull, formatWallClock, zoneNote } from "../lib/when";
import { isRemoteError, remoteProblem } from "./RemoteField";
import { Field, FormError, Input, showFieldErrors } from "./ui/Field";
import { FormActions } from "./ui/FormActions";
import { Button } from "./ui/Button";
import { VisibilityField } from "./EventVisibility";
import { RoomField } from "./RoomField";
import { useConfirm, type PendingAction } from "./ui/ConfirmDialog";
import { useRosterActivity, type ConsentRound, type DateAttempts, type EditVerdict, type ReadPermit } from "../lib/dateWrites";
import { dateKey, type EditBase } from "../lib/editBase";
import { KeptDates } from "./KeptDates";
import { useKeptRefusal } from "./keptRefusal";
import { useFocusHome, useKeepFocusHome } from "../lib/focusHome";
import { changeSentence, VISIBILITY_WORD } from "../lib/visibilityChange";
import { count as plural } from "../lib/plural";
import { reviewOutdated } from "../lib/visibilityConsent";

const STALE_SAVE = "This date changed while you were saving. Check it and save again.";
const UNSURE_SAVE = "We couldn't tell whether this date saved. Check it below, and try again if you need to.";
const UNCONFIRMED = "We couldn't confirm what happened to your earlier save, and this date has changed since. Check it before you change anything else.";

// The fields a save can change, written the way the inputs hold them.
interface Fields {
  date: string;
  time: string;
  duration: number;
  title: string;
  location: string;
  capacity: string;
  remoteCapacity: string;
  visibility: Visibility | "series";
}

// Each draft field, and the reason, has a version that moves on every edit: a stored save marks the versions it
// submitted, and the next read resets exactly those fields to what is stored, even where the stored value went
// back to what this editor first saw (another manager restored it).
type VersionKey = keyof DateEditChanges | "reason";

type Versions = Partial<Record<VersionKey, number>>;

const versionOf: Record<keyof Fields, VersionKey> = {
  date: "startsLocal", time: "startsLocal", duration: "durationMinutes", title: "title", location: "location",
  capacity: "capacityOverride", remoteCapacity: "remoteCapacityOverride", visibility: "visibility",
};

function fieldsOf(base: EditBase): Fields {
  return {
    date: base.startsLocal.slice(0, 10),
    time: base.startsLocal.slice(11, 16),
    duration: base.durationMinutes,
    title: base.title,
    location: base.location,
    capacity: base.capacity === null ? "" : String(base.capacity),
    remoteCapacity: base.remoteCapacity === null ? "" : String(base.remoteCapacity),
    visibility: base.visibilityOverride ?? "series",
  };
}

const changeKeys: ReadonlyArray<keyof DateEditChanges> = ["startsLocal", "durationMinutes", "title", "location", "capacityOverride", "remoteCapacityOverride", "visibility"];
const fieldKeys: ReadonlyArray<keyof Fields> = ["date", "time", "duration", "title", "location", "capacity", "remoteCapacity", "visibility"];

// Each field the stored date changed since `was` takes the stored value; the others keep what was typed.
function rebased(draft: Fields, was: Fields, now: Fields): Fields {
  const out = { ...draft };

  for (const key of fieldKeys) if (was[key] !== now[key]) Object.assign(out, { [key]: now[key] });

  return out;
}

// What Save sends: each change from the date as read, start date and time together, the shown duration kept,
// an explicit clear as null. Remote seats only where the date has them.
function changesOf(draft: Fields, stored: Fields, base: EditBase): DateEditChanges {
  const changes: DateEditChanges = {};
  const startsLocal = `${draft.date}T${draft.time}`;

  if (startsLocal !== base.startsLocal) changes.startsLocal = startsLocal;

  if (draft.duration !== stored.duration) changes.durationMinutes = draft.duration;

  if (draft.title !== stored.title) changes.title = draft.title;

  if (draft.location !== stored.location) changes.location = draft.location;
  const cap = draft.capacity.trim();

  if (cap !== stored.capacity) changes.capacityOverride = cap === "" ? null : Number(cap);

  if (base.remoteAvailable) {
    const remote = draft.remoteCapacity.trim();

    if (remote !== stored.remoteCapacity) changes.remoteCapacityOverride = remote === "" ? null : Number(remote);
  }

  if (draft.visibility !== stored.visibility) changes.visibility = draft.visibility;

  return changes;
}

const visibilityLabel = (v: Visibility | "series" | null) => (v === "series" || v === null ? "same as the series" : VISIBILITY_WORD[v]);

// The submitted fields of a save whose answer never arrived, as the person typed them: no stored value is quoted.
function proposedLines(changes: Readonly<DateEditChanges>, reason?: string): string[] {
  const lines: string[] = [];

  if (changes.startsLocal !== undefined) lines.push(`Starts: ${formatWallClock(changes.startsLocal)}`);

  if (changes.durationMinutes !== undefined) lines.push(`Minutes: ${changes.durationMinutes}`);

  if (changes.title !== undefined) lines.push(`Title: ${changes.title || "(none)"}`);

  if (changes.location !== undefined) lines.push(`Location: ${changes.location || "(none)"}`);

  if (changes.capacityOverride !== undefined) lines.push(`Capacity: ${changes.capacityOverride === null ? "same as the event" : changes.capacityOverride}`);

  if (changes.remoteCapacityOverride !== undefined) lines.push(`Remote seat limit: ${changes.remoteCapacityOverride === null ? "same as the event" : changes.remoteCapacityOverride}`);

  if (changes.visibility !== undefined) lines.push(`Who can see it: ${visibilityLabel(changes.visibility)}`);

  if (reason) lines.push(`Reason: ${reason}`);

  return lines;
}

// What a host holds for an editor it may unmount and mount again (while it checks that the manager still
// manages the date, or reads the date again): the draft, the stored date as the editor last saw it, the reason,
// the draft's identity and field versions, what the last send's answer said, and the dates its last save was
// refused for because someone who left kept them (DEC-156), which a remount opens again.
export interface DateDraftHold {
  draft: Fields;
  seen: Fields;
  reason: string;
  draftId: string;
  versions: Versions;
  notice: string | null;
  kept: KeptDateCount[] | null;
}

interface DateEditorProps {
  // The date as a fresh manager read gives it (lib/editBase): the form's values and its token.
  base: EditBase;
  // That read, registered with the credential's attempts once on screen; null until then. Save, Try again and
  // the question act only through it, and only while the attempts say it may.
  permit: ReadPermit | null;
  attempts: DateAttempts;
  // When present, the editor starts from it and writes back to it after every render, so the host may unmount
  // the editor and mount it again with the same draft; a newer stored date rebases it as on every host.
  hold?: MutableRefObject<DateDraftHold | null>;
  // The update was stored; `stillOpen` is false when this editor went away while it ran. The attempts ask the
  // host for its page and this date again; this only says what the editor itself does next.
  onSaved: (stillOpen: boolean) => void;
  onClose: () => void;
  // The room was booked or released (its own action, not part of Save).
  onRoomChanged?: () => void;
  // Opened by "Pick a room": the cursor goes to the Room control.
  focusRoom?: boolean;
  // The kept-dates panel's read was refused: this person may no longer manage the event's sign-ups.
  onAuthorityLost?: () => void;
  // A save was refused with a readable 401, 403 or 404: this person may no longer manage the date.
  onLost?: () => void;
}

let drafts = 0;

// DateEditor changes one occurrence only (time, duration, title, location, caps, visibility), through the
// guarded date edit (M8.1): Save freezes what differs from the read it was opened from and sends it once, with
// that read's token. Later series changes leave an edited date alone.
//
// When a newer read arrives (this editor's own save, another editor's, a refresh), each field the stored date
// changed takes the stored value and the others keep what was typed, so a later save never sends an old value
// back as a change; after a stored save, the fields it submitted take the stored value.
export function DateEditor({
  base,
  permit,
  attempts,
  hold,
  onSaved,
  onClose,
  onRoomChanged,
  focusRoom,
  onAuthorityLost,
  onLost,
}: DateEditorProps) {
  const stored = fieldsOf(base);
  const key = dateKey(base.eventId, base.date);
  const [draft, setDraft] = useState(() => hold?.current?.draft ?? stored);
  // The stored date as this editor last saw it.
  const [seen, setSeen] = useState(() => hold?.current?.seen ?? stored);
  const [reason, setReasonValue] = useState(() => hold?.current?.reason ?? "");
  const [draftId, setDraftId] = useState(() => hold?.current?.draftId ?? `draft-${++drafts}`);
  const versions = useRef<Versions>(hold?.current?.versions ?? {});
  const bump = (k: VersionKey) => { versions.current = { ...versions.current, [k]: (versions.current[k] ?? 0) + 1 }; };

  const edit = <K extends keyof Fields>(k: K) => (value: Fields[K]) => {
    bump(versionOf[k]);
    setDraft((d) => ({ ...d, [k]: value }));
  };

  const setReason = (value: string) => {
    bump("reason");
    setReasonValue(value);
  };

  // The replay of this draft's uncertain save answered that the date changed (it may or may not have saved), here
  // or on another page: what was typed and the reason go before any render shows them, also for a draft brought
  // back from a hold, and a new draft starts from the date as read.
  if (attempts.draftRetired(draftId)) {
    versions.current = {};
    setDraftId(`draft-${++drafts}`);
    setSeen(stored);
    setDraft(stored);
    setReasonValue("");
  } else if (JSON.stringify(seen) !== JSON.stringify(stored)) {
    // A newer read: catch the draft up before this render shows it.
    setSeen(stored);
    setDraft(rebased(draft, seen, stored));
  }

  // A stored save's fields take the stored value on the first read made after it, before Save can be pressed
  // again: a field typed again since keeps what was typed.
  useLayoutEffect(() => {
    if (!permit) return;
    const submitted = attempts.takeSettledDraft(permit, draftId);

    if (!submitted) return;
    const now = versions.current;
    setDraft((d) => {
      const out = { ...d };

      for (const k of fieldKeys) if (submitted[versionOf[k]] !== undefined && submitted[versionOf[k]] === now[versionOf[k]]) Object.assign(out, { [k]: stored[k] });

      return out;
    });

    if (submitted.reason !== undefined && submitted.reason === now.reason) setReasonValue("");
  }, [permit, attempts, draftId]);

  const [remoteError, setRemoteError] = useState<string | null>(null);
  const remotePrefix = `date-${base.id}`;
  // The form keeps keyboard focus when a question closes over a Save this render disables (a roster write on
  // the date began, or ended and the date waits for a new read).
  const home = useFocusHome<HTMLFormElement>();
  const root = home.ref;
  useKeepFocusHome(home);
  const [busy, setBusy] = useState(false);
  const [error, setErrorState] = useState<string | null>(() => hold?.current?.notice ?? null);

  // The host may unmount this editor in the commit that shows an answer (its page reads again), so the hold
  // hears of a message at once, not after the render that would show it.
  const setError = (message: string | null) => {
    setErrorState(message);

    if (hold?.current) hold.current.notice = message;
  };

  const mounted = useRef(true);
  useLayoutEffect(() => {
    mounted.current = true;

    return () => { mounted.current = false; };
  }, []);
  // Roster writes on this date, from any roster or kept-dates panel in the session: while one is in flight
  // nothing can be saved, and one that begins closes an open question as no (the attempts withdraw it too).
  const roster = useRosterActivity(base.eventId, base.date);
  const subscribe = useCallback((changed: () => void) => attempts.subscribe(changed), [attempts]);
  useSyncExternalStore(subscribe, attempts.version);
  // The writer's question this editor holds, with the read that sent the save, from the answer until it closes.
  const asking = useRef<{ round: ConsentRound; permit: ReadPermit } | null>(null);
  // The question closes as no in the commit after it stops standing, whatever ended it (a roster write on the
  // date, that read retired or replaced, this tab's credential or marker gone, another answer): this editor
  // renders on every change of the attempts, and `withdrawn` changes the save's authority.
  const withdrawn = asking.current !== null && !attempts.question(asking.current.permit, asking.current.round);
  // An administrator can erase this person from their own browser, which leaves this tab's marker cookie as it
  // was, so the credential's own focus check sees nothing. Coming back to the window while the question is open
  // asks the server again who this tab signs in as; an answer for anyone else replaces the credential, which
  // retires the question as above. A failed check changes nothing: the writer still refuses a stale actor.
  useEffect(() => {
    let checking = false;

    const check = () => {
      if (checking || !asking.current || document.visibilityState !== "visible") return;
      checking = true;
      void bootstrapCredential(true).catch(() => {}).finally(() => { checking = false; });
    };

    window.addEventListener("focus", check);
    document.addEventListener("visibilitychange", check);

    return () => {
      window.removeEventListener("focus", check);
      document.removeEventListener("visibilitychange", check);
    };
  }, []);
  const beginSave = useConfirm(`${roster.changes()}|${withdrawn ? "withdrawn" : ""}`);
  const latestPermit = useRef(permit);
  useLayoutEffect(() => {
    latestPermit.current = permit;
  });

  const submit = useRef<HTMLButtonElement>(null);

  // The host may unmount this editor in the commit that changes these, so the hold is written at once too.
  const kept = useKeptRefusal({
    initial: hold?.current?.kept ?? null,
    submit,
    onChange: (dates) => { if (hold?.current) hold.current.kept = dates; },
    onRefused: setError,
    onAuthorityLost,
  });

  useLayoutEffect(() => {
    if (hold) hold.current = { draft, seen, reason, draftId, versions: versions.current, notice: error, kept: kept.refusal?.dates ?? null };
  });

  const status = attempts.status(key);
  const canSave = !!permit && attempts.canSave(permit);
  const recovery = permit ? attempts.recovery(permit) : null;
  const canReplay = !!permit && attempts.canReplay(permit);

  // Settles one send's answer, asking the writer's question when it returns one. The question, and every
  // follow-up send, belong to `via`, the read the save was sent through, while it is still this editor's; a
  // question is answered no whenever this editor can no longer ask it. `at` is the date's roster sequence when
  // Save was pressed: a write that began or ended since stops the question.
  async function settle(first: EditVerdict, action: PendingAction, replaying: boolean, at: number, via: ReadPermit) {
    let v = first;
    let asked = 0;

    for (;;) {
      if (v.kind === "consent") {
        const round = v.round;
        const p = via;
        const current = () => mounted.current && action.current() && roster.changes() === at && latestPermit.current === p && attempts.question(p, round);
        // Every question comes from the writer's own answer, about this date and the change saved (the attempts
        // asked only one that is); a second one in the same save says it checked again, and needs its own yes.
        const again = asked++ > 0 ? "We checked again before saving. " : "";
        const question = `${again}${formatDayFull(base.date)}. ${changeSentence("date", v.current, v.target)} This cancels and notifies ${plural(v.count, "registered person", "registered people")} who can no longer see this date.`;
        let yes = false;

        if (current()) {
          asking.current = { round, permit: p };
          yes = await action.ask({
            title: "Save this date?",
            body: question,
            confirmLabel: "Save and notify",
            tone: "normal",
            native: `${question} Save this date?`,
          });
        }

        // Checked once more as the answer lands: the read, the roster sequence and the question must still hold.
        const live = current();

        if (asking.current?.round === round) asking.current = null;
        const next = attempts.answer(round, yes && live, p);

        if (!next) {
          // A no the person chose says nothing; a question this editor could no longer ask says why, also to the
          // hold, for the editor a new read of the date mounts in its place.
          if (yes || !live) setError(STALE_SAVE);

          if (mounted.current) setBusy(false);

          return;
        }

        v = await next;
        continue;
      }

      break;
    }

    if (!mounted.current) return;

    switch (v.kind) {
      case "stored":
        kept.close();
        setBusy(false);
        setError(null);
        onSaved(action.here());

        return;
      case "notSent":
        setBusy(false);
        setError(STALE_SAVE);

        return;
      case "unsure":
        setBusy(false);
        setError(UNSURE_SAVE);

        return;
      case "changed":
        setBusy(false);
        setError(replaying ? UNCONFIRMED : STALE_SAVE);

        return;
      case "refused": {
        const keptDates = keptDatesOf(v.cause);
        // A refusal for a kept date is told by the kept-dates panel alone (DEC-156); this editor edits one date,
        // so an unreadable list still names it.
        const msg = keptDates ? null : reviewOutdated(v.cause) ?? errorText(v.cause, "We couldn't update this date.");

        if (keptDates) kept.open(keptDates.length > 0 ? keptDates : [{ date: base.date, keptCount: 0 }]);

        const done = () => {
          setBusy(false);
          setError(msg);
        };

        if (msg && isRemoteError(v.cause)) {
          showFieldErrors(root.current, () => {
            done();
            setRemoteError(msg);
          });
        } else {
          done();
        }

        if (v.lost) onLost?.();
      }
    }
  }

  async function save(e: React.FormEvent) {
    e.preventDefault();
    const remoteMsg = remoteProblem({ available: base.remoteAvailable, capacity: draft.remoteCapacity }, true);

    if (remoteMsg) {
      showFieldErrors(root.current, () => setRemoteError(remoteMsg));

      return;
    }

    setRemoteError(null);
    const changes = changesOf(draft, stored, base);

    if (Object.keys(changes).length === 0) {
      onClose();

      return;
    }

    if (!permit || roster.writing) return;
    const submitted: Versions = {};

    for (const k of changeKeys) if (changes[k] !== undefined) submitted[k] = versions.current[k] ?? 0;
    const why = changes.visibility !== undefined ? reason : undefined;

    if (why?.trim()) submitted.reason = versions.current.reason ?? 0;
    const action = beginSave();
    const at = roster.changes();

    // The claim is made here, before any await: null when this read may not save now (another send, a doubt,
    // a roster write, a read made before the date's last change), and nothing was sent.
    const sending = attempts.save(permit, base.eventId, base.date, changes, {
      reason: why, draft: { id: draftId, versions: submitted }, shown: { visibility: base.visibility, seriesVisibility: base.seriesVisibility },
    });

    if (!sending) return;
    setBusy(true);
    setError(null);
    await settle(await sending, action, false, at, permit);
  }

  // Try again: the same bytes as the save whose answer never arrived, once, through this read.
  async function replay() {
    if (!permit) return;
    const action = beginSave();
    const at = roster.changes();
    const sending = attempts.replay(permit);

    if (!sending) return;
    setBusy(true);
    setError(null);
    await settle(await sending, action, true, at, permit);
  }

  const locked = busy || status !== "idle";

  return (<>
    <Form ref={root} tabIndex={-1} noValidate onSubmit={save} className="mt-3 space-y-3 border-t border-border pt-3">
      <p className="text-sm text-text-muted">
        This date only. Later series changes leave it alone. The series date stays {formatDayFull(base.date)}. {zoneNote(base.tzid)}
      </p>
      {error && <FormError>{error}</FormError>}
      {recovery && <div className="space-y-2 rounded-lg border border-border p-3">
        <p role="status" className="text-sm">We couldn't tell whether your last save of {formatDayFull(base.date)} went through. It asked for:</p>
        <ul className="list-disc pl-5 text-sm">{proposedLines(recovery.changes, recovery.reason).map((line) => <li key={line}>{line}</li>)}</ul>
        <p className="text-sm text-text-muted">Try again sends the same change once more. It never applies twice.</p>
        <Button variant="secondary" layout="w-full sm:w-auto" disabled={busy || !canReplay || roster.writing} onClick={() => void replay()}>Try again</Button>
      </div>}
      {!busy && !recovery && status === "unsure" && <p role="status" className="text-sm">Checking this date's last save…</p>}
      {/* The draft a save sends cannot change while it is being saved, or while an earlier save is unsettled. */}
      <fieldset disabled={locked} className="min-w-0 space-y-3">
      <div className="grid gap-3">
        <Field id={`cev-date-${base.id}`} label="Date">
          <Input type="date" value={draft.date} onChange={(e) => edit("date")(e.target.value)} required />
        </Field>
        <Field id={`cev-time-${base.id}`} label="Start time">
          <Input type="time" value={draft.time} onChange={(e) => edit("time")(e.target.value)} required />
        </Field>
        <Field id={`cev-dur-${base.id}`} label="Minutes">
          <Input
            type="number"
            min={1}
            max={10080}
            value={draft.duration}
            onChange={(e) => edit("duration")(Number(e.target.value))}
          />
        </Field>
      </div>
      <div className="grid gap-3">
        <Field id={`cev-title-${base.id}`} label="Title">
          <Input value={draft.title} onChange={(e) => edit("title")(e.target.value)} />
        </Field>
        <Field id={`cev-loc-${base.id}`} label="Location" optional>
          <Input value={draft.location} onChange={(e) => edit("location")(e.target.value)} />
        </Field>
      </div>
      <RoomField eventId={base.eventId} date={base.date} idPrefix={`cev-${base.id}`} onChanged={onRoomChanged} focusOnLoad={focusRoom} />
      <Field id={`cev-cap-${base.id}`} label="Capacity for this date" optional hint="Leave it blank to use the event’s capacity.">
        <Input
          type="number"
          min={1}
          placeholder="Same as the event"
          value={draft.capacity}
          onChange={(e) => edit("capacity")(e.target.value)}
        />
      </Field>
      {base.remoteAvailable && (
        <Field
          id={`remote-capacity-${remotePrefix}`}
          label="Remote seat limit for this date"
          optional
          hint="Enter a whole number from 0 to 100,000. Leave it blank to use the event’s remote seat limit, or 0 to close remote seats for this date."
          error={remoteError}
        >
          <Input
            type="text"
            inputMode="numeric"
            placeholder="Same as the event"
            value={draft.remoteCapacity}
            onChange={(e) => edit("remoteCapacity")(e.target.value)}
          />
        </Field>
      )}
      <VisibilityField id={`date-visibility-${base.id}`} value={draft.visibility === "series" ? (base.seriesVisibility ?? base.visibility ?? "public") : draft.visibility} onChange={edit("visibility")}
        inheritSelected={draft.visibility === "series"} onInherit={() => edit("visibility")("series")}
        seriesVisibility={base.seriesVisibility ?? base.visibility ?? "public"}
        churchWide={base.churchWide} groupName={base.groupName} groupVisibility={base.groupVisibility}
        reason={reason} onReason={setReason} />
      </fieldset>
      <FormActions>
        <Button ref={submit} variant="primary" layout="w-full sm:w-auto" type="submit" disabled={locked || !canSave || roster.writing}>
          {busy || status === "sending" ? "Saving…" : "Save this date"}
        </Button>
        {/* Close stays available: a save already sent is held by the attempts, not by this editor. */}
        <Button variant="secondary" layout="w-full sm:w-auto" onClick={onClose}>
          Close
        </Button>
      </FormActions>
    </Form>
    {/* After the form, never inside it: the panel's roster has forms of its own. */}
    {kept.refusal && <KeptDates key={kept.refusal.seq} eventId={base.eventId} keptDates={kept.refusal.dates} onRefused={kept.refused} onClose={kept.close} />}
  </>);
}
