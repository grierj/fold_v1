import { useEventReadPin } from "../lib/eventReadPin";
import { useRosterActivity } from "../lib/rosterWrites";
import { Form } from "./ui/Form";
import { useEffect, useLayoutEffect, useRef, useState, type MutableRefObject } from "react";
import { bootstrapCredential, credentialCurrent, currentCredential, type Credential } from "../api/core";
import { Events, type DateEditChanges } from "../api/events";
import type { KeptDateCount, Visibility } from "../types";
import { errorText, keptDatesOf } from "../lib/errors";
import { formatDayFull, zoneNote } from "../lib/when";
import { isRemoteError, remoteProblem } from "./RemoteField";
import { Field, FormError, Input, showFieldErrors } from "./ui/Field";
import { FormActions } from "./ui/FormActions";
import { Button } from "./ui/Button";
import { VisibilityField } from "./EventVisibility";
import { RoomField } from "./RoomField";
import { useConfirm } from "./ui/ConfirmDialog";
import { dateFields, intendedDateFields, readDateFields, type DateFields, type DateOpener, type EditBase } from "../lib/editBase";
import { KeptDates } from "./KeptDates";
import { useKeptRefusal } from "./keptRefusal";
import { useFocusHome, useKeepFocusHome } from "../lib/focusHome";
import { useKeptDraft, useUnsavedChanges } from "../lib/unsavedChanges";
import { changeSentence } from "../lib/visibilityChange";
import { count as plural } from "../lib/plural";
import { dateDecision, reviewOutdated } from "../lib/visibilityConsent";
import { answerNames, useRevisionSave, type LostSend, type RevisionConflict } from "../lib/useRevisionSave";

const STALE_QUESTION = "The page changed while we were asking, so your changes could not be saved. You can save again.";

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

function fieldsOf(base: EditBase): Fields {
  return {
    date: base.startsLocal.slice(0, 10),
    time: base.startsLocal.slice(11, 16),
    duration: base.durationMinutes,
    title: base.title,
    location: base.location,
    capacity: base.capacity === null ? "" : String(base.capacity),
    remoteCapacity: base.remoteCapacity === null ? "" : String(base.remoteCapacity),
    visibility: base.own.visibility ?? "series",
  };
}

// What Save sends: each change from the date as the editor opened it, start date and time together, the shown
// duration kept, text as the server stores it (trimmed), an explicit clear as null. Remote seats only where the date
// has them.
function changesOf(draft: Fields, base: EditBase): DateEditChanges {
  const opened = fieldsOf(base);
  const changes: DateEditChanges = {};
  const startsLocal = `${draft.date}T${draft.time}`;

  if (startsLocal !== base.startsLocal) changes.startsLocal = startsLocal;

  if (draft.duration !== opened.duration) changes.durationMinutes = draft.duration;

  if (draft.title.trim() !== opened.title) changes.title = draft.title.trim();

  if (draft.location.trim() !== opened.location) changes.location = draft.location.trim();
  const cap = draft.capacity.trim();

  if (cap !== opened.capacity) changes.capacityOverride = cap === "" ? null : Number(cap);

  if (base.remoteAvailable) {
    const remote = draft.remoteCapacity.trim();

    if (remote !== opened.remoteCapacity) changes.remoteCapacityOverride = remote === "" ? null : Number(remote);
  }

  if (draft.visibility !== opened.visibility) changes.visibility = draft.visibility;

  return changes;
}

// What a host holds for an editor it may take off the screen and bring back (while it checks that the manager
// still manages the date, or reads the page again): the read the draft opened from, with its revision, the draft
// and the reason, and the dates its last save was refused for because someone who left kept them (DEC-156), which a
// remount opens again, with the form's last error line. `unsent` says the draft holds a change no save has taken: the
// host asks before the page is left while the editor is off the screen. `lost` says a send of this draft got no answer
// (useRevisionSave), shared with the editor so an answer that lands while it is off the screen still counts.
export interface DateDraftHold {
  opened: EditBase;
  draft: Fields;
  reason: string;
  unsent: boolean;
  kept: KeptDateCount[] | null;
  notice: string | null;
  lost: LostSend;
}

interface DateEditorProps {
  // Which date: the save's refusal reads it again through this.
  opener: DateOpener;
  // A manager read of the date (lib/editBase), which a new draft opens from; a held draft keeps the read it opened from.
  base: EditBase;
  // When present, the editor starts from it and writes back to it after every render, so the host may unmount the
  // editor and mount it again with the same draft.
  hold?: MutableRefObject<DateDraftHold | null>;
  // The update was stored: the host closes the editor and reads its page again, as after any save.
  onSaved: () => void;
  // A save is on its way (true) or ended (false), also while the editor is off the screen.
  onSaving?: (saving: boolean) => void;
  // A save of this draft from before the host remounted the editor is still on its way: nothing can change until it
  // ends, since its answer may close the editor.
  pending?: boolean;
  // The save was refused and not sent again: the host closes the editor and shows the date as it is now (DEC-164).
  onConflict: (why: RevisionConflict) => void;
  onClose: () => void;
  // The date's room may have changed (its own action, not part of Save): a pick landed, or someone else's change
  // refused it (`refused`). The host reads again quietly.
  onRoomChanged?: (refused?: boolean) => void;
  // Opened by "Pick a room": the cursor goes to the Room control.
  focusRoom?: boolean;
  // The kept-dates panel's read was refused: this person may no longer manage the event's sign-ups.
  onAuthorityLost?: () => void;
}

// The press ended without sending anything more: the person said no, or the question no longer stood.
const DECLINED = Symbol("declined");
const WITHDRAWN = Symbol("withdrawn");

// DateEditor changes one occurrence only (time, duration, title, location, caps, visibility). Later series changes
// leave an edited date alone. Its save is the one conflict protocol (useRevisionSave, DEC-164): it names the read the
// draft opened with, and a refusal reads the date once and either is done, sends again or closes the editor.
export function DateEditor({ opener, base, hold, onSaved, onSaving, pending = false, onConflict, onClose, onRoomChanged, focusRoom, onAuthorityLost }: DateEditorProps) {
  // The read this draft opened from: its values and its revision, for as long as the draft lives.
  const [pinned] = useEventReadPin();
  const [opened] = useState(() => hold?.current?.opened ?? base);
  const [draft, setDraft] = useState(() => hold?.current?.draft ?? fieldsOf(opened));
  const [reason, setReason] = useState(() => hold?.current?.reason ?? "");
  const [lost] = useState(() => hold?.current?.lost ?? { current: false });
  const edit = <K extends keyof Fields>(k: K) => (value: Fields[K]) => setDraft((d) => ({ ...d, [k]: value }));
  const [remoteError, setRemoteError] = useState<string | null>(null);
  const remotePrefix = `date-${opened.id}`;
  // The form keeps keyboard focus when a question closes over a Save this render disables (a roster write on the
  // date began).
  const home = useFocusHome<HTMLFormElement>();
  const root = home.ref;
  useKeepFocusHome(home);
  const [saving, setSaving] = useState(false);
  const busy = saving || pending;
  const sending = useRef(false);
  // A stored save or a conflict ends the draft: nothing is written back to the hold after it.
  const ended = useRef(false);
  const [error, setErrorState] = useState(() => hold?.current?.notice ?? null);

  // An answer may land while the host has this editor off the screen: the hold hears of it at once.
  const setError = (message: string | null) => {
    setErrorState(message);

    if (hold?.current?.opened === opened) hold.current.notice = message;
  };

  const mounted = useRef(true);
  useLayoutEffect(() => {
    mounted.current = true;

    return () => { mounted.current = false; };
  }, []);

  // Roster writes on this date, from any roster or kept-dates panel in the session: while one is in flight nothing
  // can be saved, and one that begins or ends closes an open question as no.
  const roster = useRosterActivity(opened.eventId, opened.date);
  // The writer's question this editor holds, from the answer until it closes: the roster sequence and the sign-in of
  // the press that asked it, which must still hold for it to stand.
  const asking = useRef<{ at: number; credential: Credential | null } | null>(null);
  const withdrawn = asking.current !== null && (roster.changes() !== asking.current.at || !sameSignIn(asking.current.credential));
  // An administrator can erase this person from their own browser, which leaves this tab's marker cookie as it
  // was, so the credential's own focus check sees nothing. Coming back to the window while the question is open
  // asks the server again who this tab signs in as; an answer for anyone else replaces the credential and the
  // session under it. A failed check changes nothing: the writer still refuses a stale actor.
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

  const submit = useRef<HTMLButtonElement>(null);

  // The host may unmount this editor in the commit that changes these, so the hold is written at once too.
  const kept = useKeptRefusal({
    initial: hold?.current?.kept ?? null,
    submit,
    onChange: (dates) => { if (hold?.current) hold.current.kept = dates; },
    onRefused: setError,
    onAuthorityLost,
  });

  const openedFields = dateFields(opened);

  // The editor's own save, or its room control, can no longer go ahead: the draft ends and the host closes the editor.
  const handOver = (why: RevisionConflict) => {
    ended.current = true;

    if (hold?.current?.opened === opened) hold.current = null;
    onConflict(why);
  };

  const send = useRevisionSave<DateFields | null, DateFields>({
    opened: openedFields,
    revision: opened.revision,
    read: () => readDateFields(opener, pinned),
    fields: (value) => value,
    answers: answerNames("occurrence", opened.id),
    lost,
    onConflict: handOver,
    changedCode: "date_changed",
  });

  const changesNow = JSON.stringify(changesOf(draft, opened));
  const unsent = !busy && !ended.current && changesNow !== "{}";

  useLayoutEffect(() => {
    if (hold && !ended.current) hold.current = { opened, draft, reason, unsent, kept: kept.refusal?.dates ?? null, notice: error, lost };
  });

  async function save(e: React.FormEvent) {
    e.preventDefault();
    const remoteMsg = remoteProblem({ available: opened.remoteAvailable, capacity: draft.remoteCapacity }, true);

    if (remoteMsg) {
      showFieldErrors(root.current, () => setRemoteError(remoteMsg));

      return;
    }

    setRemoteError(null);
    const changes = changesOf(draft, opened);

    if (Object.keys(changes).length === 0) {
      onClose();

      return;
    }

    if (sending.current || pending || roster.writing) return;
    sending.current = true;
    const action = beginSave();
    // Every send of this press (the first, the consent answer's and any the protocol sends again) goes only while
    // nothing it was judged under moved: this editor is on screen, no roster write on the date began or ended, and
    // this tab signs in as it did.
    const at = roster.changes();
    const credential = pinned ?? currentCredential();
    const live = () => mounted.current && roster.changes() === at && sameSignIn(credential);
    const reasonSent = changes.visibility !== undefined ? reason.trim() || undefined : undefined;

    // The question must be about this save: the date's row, the visibility the opened read showed, and the value
    // asked for ("same as the series" resolved from that read). A save that changes no visibility is never asked.
    const expected = changes.visibility === undefined ? null : {
      eventId: opened.eventId, occurrenceId: opened.id, current: opened.visibility,
      target: changes.visibility === "series" || changes.visibility === null ? opened.seriesVisibility : changes.visibility,
    };

    let consent: string | undefined;
    let asked = 0;

    const write = async (revision: string | undefined) => {
      for (;;) {
        if (!live()) throw WITHDRAWN;

        try {
          return await Events.editDate(opened.eventId, opened.date, { revision: revision ?? "", changes, reason: reasonSent, consent });
        } catch (err) {
          const decision = expected && dateDecision(err, expected);

          if (!decision) throw err;

          if (!live()) throw WITHDRAWN;
          // Every question comes from the writer's own answer; a second one in the same save says it checked again,
          // and needs its own yes.
          const again = asked++ > 0 ? "We checked again before saving. " : "";
          const question = `${again}${formatDayFull(opened.date)}. ${changeSentence("date", decision.current, decision.target)} This cancels and notifies ${plural(decision.count, "registered person", "registered people")} who can no longer see this date.`;
          asking.current = { at, credential };
          let yes = false;

          try {
            yes = await action.ask({ title: "Save this date?", body: question, confirmLabel: "Save and notify", tone: "normal", native: `${question} Save this date?`, interruption: "before-leaving" });
          } finally {
            asking.current = null;
          }

          // A no the person chose says nothing; a question that closed because what it asked under moved says why.
          if (!yes) throw live() && action.current() ? DECLINED : WITHDRAWN;
          consent = decision.token;
        }
      }
    };

    setSaving(true);
    onSaving?.(true);
    setError(null);
    let saved = false;

    try {
      const sent = await send(write, intendedDateFields(openedFields, changes), reasonSent ?? "");

      if (sent === "saved") {
        // The answer may land while the host has this editor off the screen: the draft it holds is this one.
        saved = mounted.current || hold?.current?.opened === opened;
        ended.current = true;

        if (hold?.current?.opened === opened) hold.current = null;
        kept.close();
      } else if (sent === "refused") {
        setError("We couldn't update this date. Please try again.");
      }
    } catch (err) {
      if (err === WITHDRAWN) {
        setError(STALE_QUESTION);
      } else if (err !== DECLINED) {
        const keptDates = keptDatesOf(err);
        // A refusal for a kept date is told by the kept-dates panel alone (DEC-156); this editor edits one date, so an
        // unreadable list still names it.
        const msg = keptDates ? null : reviewOutdated(err) ?? errorText(err, "We couldn't update this date.");

        if (keptDates) kept.open(keptDates.length > 0 ? keptDates : [{ date: opened.date, keptCount: 0 }]);

        if (msg && isRemoteError(err)) {
          // The fields take focus again before the refused one is focused.
          showFieldErrors(root.current, () => {
            setSaving(false);
            onSaving?.(false);
            setError(msg);
            setRemoteError(msg);
          });
        } else {
          setError(msg);
        }
      }
    } finally {
      sending.current = false;
      onSaving?.(false);

      if (mounted.current) setSaving(false);
      // A press that ended unsaved while the host had this editor off the screen leaves its change unsent: leaving the
      // page asks first (DEC-98).
      else if (!ended.current && hold?.current?.opened === opened) hold.current.unsent = true;
    }

    if (saved) onSaved();
  }

  // Leaving the page with a change typed and not sent asks first (DEC-98), as the series editor does.
  useUnsavedChanges(unsent);
  useKeptDraft(() => asking.current !== null, true);

  return (<>
    <Form ref={root} tabIndex={-1} noValidate onSubmit={save} className="mt-3 space-y-3 border-t border-border pt-3">
      <p className="text-sm text-text-muted">
        This date only. Later series changes leave it alone. The series date stays {formatDayFull(opened.date)}. {zoneNote(opened.tzid)}
      </p>
      {error && <FormError>{error}</FormError>}
      {/* The draft a save sends cannot change while it is being saved. */}
      <fieldset disabled={busy} className="min-w-0 space-y-3">
      <div className="grid gap-3">
        <Field id={`cev-date-${opened.id}`} label="Date">
          <Input type="date" value={draft.date} onChange={(e) => edit("date")(e.target.value)} required />
        </Field>
        <Field id={`cev-time-${opened.id}`} label="Start time">
          <Input type="time" value={draft.time} onChange={(e) => edit("time")(e.target.value)} required />
        </Field>
        <Field id={`cev-dur-${opened.id}`} label="Minutes" hint="Enter a whole number from 1 to 10,080.">
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
        <Field id={`cev-title-${opened.id}`} label="Title" hint="Up to 200 characters.">
          <Input value={draft.title} onChange={(e) => edit("title")(e.target.value)} />
        </Field>
        <Field id={`cev-loc-${opened.id}`} label="Location" optional hint="Up to 300 characters.">
          <Input value={draft.location} onChange={(e) => edit("location")(e.target.value)} />
        </Field>
      </div>
      <RoomField eventId={opened.eventId} date={opened.date} idPrefix={`cev-${opened.id}`} onChanged={onRoomChanged} onGone={() => handOver("gone")} focusOnLoad={focusRoom} />
      <Field id={`cev-cap-${opened.id}`} label="Capacity for this date" optional hint="Enter a whole number from 1 to 100,000. Leave it blank to use the event’s capacity.">
        <Input
          type="number"
          min={1}
          placeholder="Same as the event"
          value={draft.capacity}
          onChange={(e) => edit("capacity")(e.target.value)}
        />
      </Field>
      {opened.remoteAvailable && (
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
      <VisibilityField id={`date-visibility-${opened.id}`} value={draft.visibility === "series" ? opened.seriesVisibility : draft.visibility} onChange={edit("visibility")}
        inheritSelected={draft.visibility === "series"} onInherit={() => edit("visibility")("series")}
        seriesVisibility={opened.seriesVisibility}
        churchWide={opened.churchWide} groupName={opened.groupName} groupVisibility={opened.groupVisibility}
        reason={reason} onReason={setReason} />
      </fieldset>
      <FormActions>
        <Button ref={submit} variant="primary" layout="w-full sm:w-auto" type="submit" disabled={busy || roster.writing}>
          {busy ? "Saving…" : "Save this date"}
        </Button>
        <Button variant="secondary" layout="w-full sm:w-auto" disabled={busy} onClick={onClose}>
          Close
        </Button>
      </FormActions>
    </Form>
    {/* After the form, never inside it: the panel's roster has forms of its own. */}
    {kept.refusal && <KeptDates key={kept.refusal.seq} eventId={opened.eventId} keptDates={kept.refusal.dates} onRefused={kept.refused} onClose={kept.close} />}
  </>);
}

// Whether this tab still signs in with the credential a press began under.
function sameSignIn(credential: Credential | null): boolean {
  return credential === null ? currentCredential() === null : credentialCurrent(credential);
}
