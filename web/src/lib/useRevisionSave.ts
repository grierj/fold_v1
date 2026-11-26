import { useEffect, useRef } from "react";
import { ApiError, credentialCurrent, currentCredential } from "../api/core";
import { useEventReadPin } from "./eventReadPin";
import type { RevisionRef } from "../types";
import { errorCode } from "./errors";
import { outcomeOf } from "./rosterWrites";
import { isNumber, isObject, isString } from "./wireValues";

// changed: what is stored differs from what the form opened with and from what the save would leave. gone: this
// person may no longer make this save.
export type RevisionConflict = "changed" | "gone";

// saved: the write landed, or a refused one found what it would leave already stored. refused: refused every time,
// with nothing the form can write changed (the form says its ordinary error). left: the form went away mid-way;
// nothing more was sent. conflict: onConflict was told.
export type RevisionSent = "saved" | "refused" | "left" | "conflict";

export interface RevisionRead<T> {
  // Whether the read still lets this person make the save.
  allowed: boolean;
  revision?: string;
  value: T;
}

// A refused save is sent again at most this many times in all, while the stored value still matches the form.
const MAX_SENDS = 3;

// The server says the person may no longer manage or see what they were editing.
export const lostAuthority = (err: ApiError) => err.status === 403 || err.status === 404;

// A send that may have been stored with its answer lost (outcomeOf's "unsure"). A form's own reason to stop, which is
// no Error (a question answered no), sent nothing.
const unanswered = (cause: unknown) => cause instanceof Error && outcomeOf(cause) === "unsure";

// What the transport throws for a body it cannot read (api/core.ts), so the form says what it always says then.
const unreadableAnswer = () => new ApiError(502, "unreadable answer", {}, true);


// A send of this draft got no answer (`current`), and what it carried that no read shows (`unseen`).
export interface LostSend {
  current: boolean;
  unseen?: string;
}

// The refusal each kind of save answers when the thing changed since the read it names.
type ChangedCode = "event_changed" | "date_changed" | "group_changed" | "date_room_changed" | "message_changed" | "room_changed" | "setting_changed" | "admin_user_changed" | "household_changed" | "household_person_changed";

// What a save throws when every send was refused while nothing it writes changed, so the form says its ordinary
// refusal for that code.
export const refusedAgain = (code: ChangedCode) => new ApiError(409, code.replace(/_/g, " "), { code });

// What a save names in its answer: an event, a single date, a group, a date's room (named by its date's id), a
// message, a room, an account, a household or a household person.
type AnswerKey = "event" | "occurrence" | "group" | "dateRoom" | "message" | "room" | "user" | "household" | "person";

// What a save that landed answers: the thing it saved, with its new revision.
interface SettingAnswer {
  value: string;
  revision: string;
}

type SavedAnswer = { [K in AnswerKey]: Record<K, RevisionRef> }[AnswerKey] | SettingAnswer;

export function answerSetting(answer: unknown): answer is SettingAnswer {
  return isObject(answer) && "value" in answer && isString(answer.value)
    && "revision" in answer && isString(answer.revision) && answer.revision !== "";
}

// A save's answer names the thing saved: `{ [key]: { id, revision } }` with this id and a revision. Anything else (a
// page from something in between, which reads as {}, or another thing's answer) is no answer.
export const answerNames = (key: AnswerKey, id: number) => (answer: unknown): answer is SavedAnswer => {
  if (!isObject(answer)) return false;

  const thing = key === "event" ? "event" in answer && answer.event
    : key === "occurrence" ? "occurrence" in answer && answer.occurrence
      : key === "group" ? "group" in answer && answer.group
        : key === "message" ? "message" in answer && answer.message
          : key === "room" ? "room" in answer && answer.room
            : key === "user" ? "user" in answer && answer.user
              : key === "household" ? "household" in answer && answer.household
                : key === "person" ? "person" in answer && answer.person
                  : "dateRoom" in answer && answer.dateRoom;

  return isObject(thing) && "id" in thing && "revision" in thing && isNumber(thing.id) && thing.id === id && isString(thing.revision) && thing.revision !== "";
};

// The revision a landed save's answer names.
function answeredRevision(answer: SavedAnswer): string {
  if ("revision" in answer) return answer.revision;

  if ("user" in answer) return answer.user.revision;

  if ("event" in answer) return answer.event.revision;

  if ("occurrence" in answer) return answer.occurrence.revision;

  if ("group" in answer) return answer.group.revision;

  if ("message" in answer) return answer.message.revision;

  if ("room" in answer) return answer.room.revision;

  if ("household" in answer) return answer.household.revision;

  if ("person" in answer) return answer.person.revision;

  return answer.dateRoom.revision;
}

// useRevisionSave is the one conflict protocol for a manager's save (DEC-162 to DEC-164, DEC-166, DEC-169, DEC-171, DEC-174). The save
// names the read the form opened with (`revision`, kept from the first render, like `opened`). On a refusal
// (`changedCode`: an event's `event_changed`, a single date's `date_changed`, a group's `group_changed`, a message's
// `message_changed`, a room's `room_changed`, a household's `household_changed`, a household person's
// `household_person_changed`) the value is read once and compared, in this order: what is stored is what this save would leave
// (`intended`: an earlier press stored it and its answer was lost), so the save is done and nothing more is sent;
// nothing the save can write has changed since the form opened (the person's own room booking, a restart), so the same
// save goes again under the new revision; anything else goes to `onConflict`, and the page shows what is stored.
// A save counts only when its answer names the thing saved (`answers`, usually `answerNames`); any other answer, like a
// dropped connection or a 5xx, leaves the outcome unknown and is thrown for the form's ordinary error. Once a send from
// this form got no answer, until a save lands, a refusal ends only as already stored or as a conflict: the form never
// sends again under a newer revision, which could re-apply a change someone deliberately undid after the lost send
// stored it (issue 455). `lost` keeps that for a form that may be unmounted and mounted again with the same draft.
// A save may carry what no read shows (`unseen`: the reason a visibility change is recorded and announced with). What
// is stored then counts as this save only when the send that got no answer carried the same: a reason typed since
// was never stored, so that is a conflict, not a save.
// Repeated refusals alone are never a conflict. Only the form and the sign-in that pressed Save send again: an answer
// that lands after the form went away, or after this tab took another account, sends nothing more. A 403 or 404 to
// any send or to the read says the person may no longer manage or see what they were editing: that is `onConflict`
// too ("gone"), so the page stops showing it.
//
// `fields` is everything the save can write, as the form reads it (an absent value read as the form reads it), and
// the one normaliser for the value opened, the value intended and the value read. A field left out would be
// overwritten without a word. It answers null when a read cannot show that the save is still the same decision, or
// already stored, which is a conflict. `intended` is the value as the server stores it: where the form cannot tell
// what that is, it must differ from any read, so a stored save is never claimed by mistake.
//
// Every other answer, on the first send or a later one, is thrown for the form to handle as it always has.
//
// A form that stays open after an outcome and is used again (a room control) passes `stays`, which reads the value a
// landed save's answer says was stored. Each outcome then moves the base the next save names: a save that landed to
// that value and the answer's revision; one already stored, a conflict, or refusals with nothing changed to what the
// read found and its revision. A send with no answer moves nothing, so the lost-answer rule above holds across sends.
// Without it, the base is the read the form opened with, for as long as the form lives.
//
// `gone` says which refusals of a send or of the read mean the person may no longer make it (by default every 403 and
// 404). A control that stays (`stays`) and is no longer on screen when such a refusal arrives hands nothing over: it
// returns "left", so a late answer never reaches whatever its host shows now.
//
// A save that writes more than `fields` can show (`writesMore`: the series room pick books every date, and its read
// shows the series room) counts a read that shows what it would leave as this save only when a send of it got no
// answer. Otherwise someone else stored that value, and the same save goes again under the new revision, so its own
// answer says what it did.
export function useRevisionSave<T, F>({
  opened,
  revision,
  read,
  fields,
  answers,
  lost: kept,
  onConflict,
  changedCode = "event_changed",
  stays,
  gone = lostAuthority,
  writesMore = false,
}: {
  opened: T;
  revision: string | undefined;
  read: () => Promise<RevisionRead<T>>;
  fields: (value: T) => F | null;
  answers: (answer: unknown) => answer is SavedAnswer;
  lost?: LostSend;
  onConflict?: (why: RevisionConflict) => void;
  changedCode?: ChangedCode;
  stays?: (answer: SavedAnswer) => T;
  gone?: (err: ApiError) => boolean;
  writesMore?: boolean;
}) {
  const base = useRef({ opened, revision });
  const own = useRef<LostSend>({ current: false });
  const lost = kept ?? own.current;
  const mounted = useRef(true);
  const [pinned] = useEventReadPin();

  useEffect(() => {
    mounted.current = true;

    return () => { mounted.current = false; };
  }, []);

  return async function send<R>(write: (revision: string | undefined) => Promise<R>, intended: T, unseen = ""): Promise<RevisionSent> {
    const conflict = (why: RevisionConflict): RevisionSent => {
      onConflict?.(why);

      return "conflict";
    };

    const same = (a: F | null, b: F | null) => a !== null && b !== null && JSON.stringify(a) === JSON.stringify(b);
    const was = fields(base.current.opened);
    const goal = fields(intended);
    const credential = pinned ?? currentCredential();
    const sameSignIn = () => (credential === null ? currentCredential() === null : credentialCurrent(credential));
    let current = base.current.revision;

    const rebase = (value: T, revision: string) => {
      if (stays) base.current = { opened: value, revision };
    };

    const noAnswer = () => {
      lost.current = true;
      lost.unseen = unseen;
    };

    const landed = () => {
      lost.current = false;
      lost.unseen = undefined;
    };

    for (let sends = 1; ; sends++) {
      if (pinned && !credentialCurrent(pinned)) return "left";
      let answer: R | undefined;
      let refused = false;

      try {
        answer = await write(current);

        if (pinned && !credentialCurrent(pinned)) {
          noAnswer();

          return "left";
        }
      } catch (err) {
        if (unanswered(err)) noAnswer();

        if (pinned && !credentialCurrent(pinned)) return "left";

        if (err instanceof ApiError && gone(err)) return stays && !mounted.current ? "left" : conflict("gone");

        if (errorCode(err) !== changedCode) throw err;

        refused = true;
      }

      if (!refused) {
        if (!answers(answer)) {
          noAnswer();

          throw unreadableAnswer();
        }

        landed();

        if (stays) rebase(stays(answer), answeredRevision(answer));

        return "saved";
      }

      if (!mounted.current) return "left";

      if (!sameSignIn()) return conflict("gone");

      let stored: RevisionRead<T>;

      try {
        stored = await read();
      } catch (err) {
        if (pinned && (!credentialCurrent(pinned) || !mounted.current)) return "left";

        if (pinned && errorCode(err) === "session_expired") throw err;

        if (err instanceof ApiError && gone(err)) return stays && !mounted.current ? "left" : conflict("gone");

        throw err;
      }

      if (!mounted.current || (pinned && !credentialCurrent(pinned))) return "left";

      if (!sameSignIn() || !stored.allowed || !stored.revision) return conflict("gone");

      const now = fields(stored.value);
      const storedGoal = same(now, goal);

      if (storedGoal && (lost.current || !writesMore)) {
        rebase(stored.value, stored.revision);

        if (unseen !== (lost.unseen ?? "")) return conflict("changed");

        landed();

        return "saved";
      }

      if (!storedGoal && (!same(now, was) || lost.current)) {
        rebase(stored.value, stored.revision);

        return conflict("changed");
      }

      if (sends === MAX_SENDS) {
        rebase(stored.value, stored.revision);

        return "refused";
      }

      current = stored.revision;
    }
  }
}

// What a managed save's send has seen: its write's answer, and the protocol's read.
interface Seen<Row, A> {
  answer?: A;
  row?: Row;
}

// useManagedSave is useRevisionSave for a form or control that shows a row and needs it back after each outcome, so no
// caller keeps the last answer and the last read beside the hook. `read` reads the whole row the form shows; `value` is
// the part the save compares and intends; `stored` is the row a landed save's answer says was stored. `onConflict` gets
// the row the read found (none when it found nothing). A control that stays on screen passes `stays`: each outcome then
// moves the base its next save names, as useRevisionSave says. A save resolves how it ended (`sent`), the write's
// `answer` when it landed by it, and the stored `row`: from that answer, or from the read the protocol made.
export function useManagedSave<Row, T, F, A>({ opened, revision, read, value, stored, fields, answers, onConflict, changedCode, stays = false, gone, writesMore }: {
  opened: Row;
  revision: string | undefined;
  read: () => Promise<{ allowed: boolean; revision?: string; row: Row }>;
  value: (row: Row) => T;
  stored: (answer: A) => Row;
  fields: (value: T) => F | null;
  answers: (answer: unknown) => answer is SavedAnswer;
  onConflict?: (why: RevisionConflict, row?: Row) => void;
  changedCode?: ChangedCode;
  stays?: boolean;
  gone?: (err: ApiError) => boolean;
  writesMore?: boolean;
}) {
  // What the send on its way has seen.
  const seen = useRef<Seen<Row, A>>({});

  const send = useRevisionSave<T, F>({
    opened: value(opened),
    revision,
    read: async () => {
      const now = await read();
      seen.current.row = now.row;

      return { allowed: now.allowed, revision: now.revision, value: value(now.row) };
    },
    fields,
    answers,
    onConflict: onConflict && ((why) => onConflict(why, seen.current.row)),
    changedCode,
    // SAFETY: the hook calls `stays` only for a send that landed, after this save's write recorded its answer in `seen`.
    stays: stays ? () => value(stored(seen.current.answer as A)) : undefined,
    gone,
    writesMore,
  });

  async function save(write: (revision: string | undefined) => Promise<A>, intended: T, unseen?: string) {
    const mine: Seen<Row, A> = {};
    seen.current = mine;

    const sent = await send(async (rev) => {
      const answer = await write(rev);
      mine.answer = answer;

      return answer;
    }, intended, unseen);

    const answer = sent === "saved" ? mine.answer : undefined;

    return { sent, answer, row: answer !== undefined ? stored(answer) : mine.row };
  }

  return save;
}
