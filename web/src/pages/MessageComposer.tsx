import { publicName } from "../lib/can";
import { lazy, Suspense, useEffect, useId, useRef, useState, type FormEvent } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router-dom";
import { ChevronLeftIcon } from "../components/icons";
import { useAuth } from "../auth";
import { ApiError } from "../api/core";
import { Messages, previewHTMLPath } from "../api/messages";
import type { AudienceChange, AudienceSummary, ComposeOptions, Message, MessageAudience, MessagePreview, MessageReview } from "../types";
import { ERROR_COPY, errorCode, errorText, messageDailyLimitText, type ErrorCode } from "../lib/errors";
import { audienceKey, audienceLabel, changeLines, confirmLine, fromAudience, guardianLine, historyLine, historyTeenLine, isOneEvent, listLimitLine, notMovedLine, prefillChoice, pushHistoryLine, pushLine, retryQuestion, reviewRefusal, saveListLine, statusLabel, stillScheduledLine, toAudience, type AudienceChoice, type ReviewRefusal } from "../lib/messages";
import { reminderAudience, reminderDateText, reminderName } from "../lib/reminders";
import { RefusedSaveLine, useRefusedSave } from "../lib/refusedSave";
import { outcomeOf } from "../lib/rosterWrites";
import { useAction } from "../lib/useAction";
import { answerNames, refusedAgain, useRevisionSave, type RevisionConflict } from "../lib/useRevisionSave";
import { useChurchZoneLoad } from "../lib/useSite";
import { isNumber, isObject, isString } from "../lib/wireValues";
import { useUnsavedChanges } from "../lib/unsavedChanges";
import { browserTimeZone, wallClock, wallToInstant, zoneNote } from "../lib/when";
import { AudiencePicker, RecipientList } from "../components/messages/AudiencePicker";
import { SendProblem, useMessageHistory, WhoGotIt } from "../components/messages/MessageHistory";
import { ErrorNotice } from "../components/ErrorNotice";
import { Button, ButtonLink } from "../components/ui/Button";
import { EmptyState } from "../components/ui/EmptyState";
import { Checkbox, Choices, Field, FormError, Input, Radio, showFieldErrors } from "../components/ui/Field";
import { Form } from "../components/ui/Form";
import { FormActions } from "../components/ui/FormActions";
import { Skeleton } from "../components/ui/Skeleton";

const RichTextEditor = lazy(() => import("../components/messages/RichTextEditor").then((m) => ({ default: m.RichTextEditor })));
const RichTextView = lazy(() => import("../components/messages/RichTextEditor").then((m) => ({ default: m.RichTextView })));

// The composer (M3.4c, PLAN 7.3): who gets it, a subject and a rich-text
// body, a preview and a test to yourself, then Send now or a date and time
// in the church's zone, confirmed with the count. One primary action: Send
// now (or Schedule). Save draft sits beside it. A scheduled message shows
// when it goes out, with Edit and Cancel message, until sending starts;
// after that, and once cancelled, it is read only. Every error is the
// API's code in words (lib/errors.ts).

type Kind = "draft" | "preview" | "test" | "send" | "schedule" | "cancel" | "retry";

// A press that saves the form first. On a scheduled message whose list the save changes, the sender confirms that list
// (DEC-169) and the press then goes on.
type Press = "draft" | "preview" | "test" | "send";

type GoesOn = Exclude<Press, "draft">;

// A send whose answer was lost: the next press settles it, and never sends twice.
const UNSURE_SEND = "We couldn't tell whether it went out, and pressing it again won't send it twice.";

type Errors = { audience?: string; subject?: string; body?: string; when?: string; form?: string; test?: string };

// Which field a server error belongs under.
const FIELD_OF: Partial<Record<ErrorCode, keyof Errors>> = {
  message_subject: "subject",
  message_body: "body",
  message_merge_unknown: "body",
  message_merge_event: "body",
  message_image: "body",
  message_audience: "audience",
  message_audience_gone: "audience",
  message_not_yours: "audience",
  message_nobody: "audience",
  message_when: "when",
};

const YEAR_MS = 366 * 24 * 60 * 60 * 1000;

// What a save writes, as the server stores it, and the status and scheduled time it leaves as they are: a save made
// beside a status or a time that moved is a save on a message that changed.
type MessageValue = Pick<Message, "subject" | "bodyHtml" | "audience" | "pushAlert" | "channel" | "status" | "scheduledAt">;

const valueOf = (m: Message): MessageValue => ({ subject: m.subject, bodyHtml: m.bodyHtml, audience: m.audience, pushAlert: m.pushAlert, channel: m.channel, status: m.status, scheduledAt: m.scheduledAt });

const sorted = (xs: readonly string[] | undefined, fallback: readonly string[]) => [...(xs?.length ? xs : fallback)].sort();

// An audience in the server's canonical form: only the fields its type uses, the server's defaults filled in, sets in
// one order.
function canonicalAudience(a: MessageAudience) {
  switch (a.type) {
    case "group": return { type: a.type, groupId: a.groupId, roles: sorted(a.roles, ["leader", "member"]) };
    case "occurrence": return { type: a.type, occurrenceId: a.occurrenceId, statuses: sorted(a.statuses, ["going", "waitlisted"]) };
    case "event_series": return { type: a.type, eventId: a.eventId, statuses: sorted(a.statuses, ["going", "waitlisted"]) };
    case "events": return { type: a.type, eventIds: [...new Set(a.eventIds ?? [])].sort((x, y) => x - y), statuses: sorted(a.statuses, ["going", "waitlisted"]) };
    case "users": return { type: a.type, userIds: [...new Set(a.userIds ?? [])].sort((x, y) => x - y) };
    default: return { type: a.type };
  }
}

// textFields is everything PUT /messages/:id writes, as the server stores it: the subject trimmed, the body (the
// server sanitises it, so an editor's HTML it rewrites never reads as already stored), the audience in canonical form,
// the channel and "Push notification too".
const textFields = (v: MessageValue) => ({ subject: v.subject.trim(), body: v.bodyHtml, audience: canonicalAudience(v.audience), channel: v.channel, pushAlert: v.pushAlert });

// messageFields is what a save compares after a refusal: what it writes, and the status and scheduled time beside it.
const messageFields = (v: MessageValue) => ({ ...textFields(v), status: v.status, at: v.scheduledAt === null ? null : Date.parse(v.scheduledAt) });

type SavedAnswer = ReturnType<typeof answerNames> extends (answer: unknown) => answer is infer S ? S : never;

// A save the server answered with a message, by id, with a revision.
const namesAMessage = (answer: unknown): answer is { message: Message } =>
  isObject(answer) && "message" in answer && isObject(answer.message) && "id" in answer.message && isNumber(answer.message.id) && answerNames("message", answer.message.id)(answer);

// The value a new message's form opens with, before its first save.
const BLANK: MessageValue = { subject: "", bodyHtml: "", audience: { type: "group" }, pushAlert: true, channel: "email", status: "draft", scheduledAt: null };

// What a press of the confirmation sends: a send (the schedule) of the message it was pressed on (`value`, with the
// status and scheduled time it had), or a save of a scheduled message's new list (DEC-169) that then goes on with the
// press that asked it (`goesOn`); when it should go and the review it showed.
interface Asked {
  kind: "send" | "save";
  goesOn: GoesOn;
  revision: string | undefined;
  value: MessageValue;
  at: string | null;
  review: MessageReview;
  // What changed since the review before this one (message_list_changed); null for a first review.
  change: AudienceChange | null;
  // The server asked for a fresh review without a change to show (message_review_needed, or a restart).
  again: boolean;
  // The question follows a confirmed save of the scheduled message's new list: it takes focus on its line, never on its
  // button, so a key still held from "Save the change" sends nothing.
  afterSave?: boolean;
  snap: string;
}

// Whether m shows that the send `asked` stored: going out, with its text, at its time. A send changes no text, so the
// text alone proves nothing.
function wentOut(m: Message, asked: Asked): boolean {
  if (!["scheduled", "sending", "sent"].includes(m.status)) return false;

  if (JSON.stringify(textFields(valueOf(m))) !== JSON.stringify(textFields(asked.value))) return false;

  if (asked.at) return !!m.scheduledAt && Date.parse(m.scheduledAt) === Date.parse(asked.at);

  return m.status !== "scheduled" || (!!m.scheduledAt && Date.parse(m.scheduledAt) <= Date.now() + 60_000);
}

export function MessageComposerPage() {
  const { id } = useParams<{ id: string }>();
  const [params] = useSearchParams();
  // The schedule is in the church's zone: wait for it, so a saved time is
  // never read on the browser's clock first.
  const church = useChurchZoneLoad();
  const zone = church.zone ?? browserTimeZone();
  // The message one opening of the composer starts from (null for a new one), the opening's number, and whether it opens
  // in the editor: each read of another message, and each Edit press, mounts a new Composer, whose saves name the read
  // it opened with.
  const [opening, setOpening] = useState<{ message: Message | null; options: ComposeOptions; n: number; edit: boolean } | null>(null);
  // The message the composer shows, a new draft once its first save lands included, so that address is not read again.
  const shown = useRef<number | null | undefined>(undefined);
  const pageRef = useRef<HTMLDivElement>(null);
  const editRequested = useRef<number | null>(null);
  const [loadError, setLoadError] = useState<{ text: string; missing: boolean } | null>(null);
  const [reload, setReload] = useState(0);
  const refusal = useRefusedSave();

  useEffect(() => {
    if (id && Number(id) === shown.current) return;
    shown.current = undefined;

    if (editRequested.current !== Number(id)) editRequested.current = null;
    setLoadError(null);
    let alive = true;

    async function open() {
      const m = id ? (await Messages.get(Number(id))).message : null;

      if (!alive) return;
      const edit = !!m?.editable && (m.status === "draft" || m.id === editRequested.current);
      const options = edit ? await Messages.editOptions(m.id) : await Messages.options();

      if (!alive) return;
      shown.current = m?.id ?? null;
      refusal.clear();
      setOpening((o) => ({ message: m, options, edit, n: (o?.n ?? 0) + 1 }));
    }

    open().catch((e) => {
      if (alive) setLoadError({ text: errorText(e, id ? "We couldn't open this message." : "We couldn't open the composer."), missing: !!id && errorCode(e) === "not_found" });
    });

    return () => { alive = false; };
  }, [id, reload]);

  useEffect(() => {
    if (editRequested.current !== Number(id)) return;
    const result = pageRef.current?.querySelector("h1");

    if (!result) return;
    result.tabIndex = -1;
    result.focus();

    if (!loadError) editRequested.current = null;
  }, [id, opening, loadError, church.done]);

  const back = <ButtonLink variant="link" to="/messages"><ChevronLeftIcon className="h-4 w-4 shrink-0" /><span aria-hidden="true">Messages</span><span className="sr-only">Back to messages</span></ButtonLink>;
  const page = (children: React.ReactNode) => <div ref={pageRef} className="fold-page fold-page-narrow space-y-6 wrap-anywhere">{children}</div>;

  if (loadError) {
    return page(<>
      {back}
      <RefusedSaveLine save={refusal} noun="message" />
      {loadError.missing
        ? <EmptyState level={1} title="This message isn't here" body="It may have been written by someone else. Your own messages are on the Messages page." action={<ButtonLink variant="primary" to="/messages">Go to messages</ButtonLink>} />
        : <><h1 className="text-3xl font-bold">Message</h1><ErrorNotice message={loadError.text} onRetry={() => { setLoadError(null); setReload((n) => n + 1); }} /></>}
    </>);
  }

  // The editor on screen is the address's: while another message loads, the one the person left is not shown, so nothing
  // on it can act on a message they moved away from. A new draft's first save names its address before moving to it.
  const current = opening && shown.current === (id ? Number(id) : null);

  if (!church.done || !opening || !current) return page(<Skeleton kind="page" label="Loading the composer…" />);

  const { options } = opening;

  if (!options.canCompose && !opening.message) {
    return page(<>
      {back}
      <EmptyState level={1} title="Messages are for leaders" body="Group leaders, event hosts and admins write to their groups and events here. Ask a leader if you'd like something sent." action={<ButtonLink variant="secondary" to="/my-groups">Go to my groups</ButtonLink>} />
    </>);
  }

  return page(
    <Composer key={opening.n} options={options} zone={zone} opened={opening.message} edit={opening.edit} params={params} refusal={refusal} back={back}
      onShown={(m) => { shown.current = m; }}
      onEdit={(m) => {
        editRequested.current = m.id;
        shown.current = undefined;
        setReload((n) => n + 1);
      }}
      onGone={() => setLoadError({ text: "", missing: true })} />,
  );
}

// Composer is one opening of a message, from the read the page made (or a new one). It stays open after its saves and
// sends, so each save names what the last outcome left (useRevisionSave's `stays`).
function Composer({ options, zone, opened, edit, params, refusal, back, onShown, onEdit, onGone }: {
  options: ComposeOptions;
  zone: string;
  opened: Message | null;
  edit: boolean;
  params: URLSearchParams;
  refusal: ReturnType<typeof useRefusedSave>;
  back: React.ReactNode;
  // The message the composer shows now (a new draft's first save).
  onShown: (id: number) => void;
  // Edit, from the message on screen: a new opening, so the saves name the read it shows.
  onEdit: (m: Message) => void;
  // The message is not there, or not this person's, any more.
  onGone: () => void;
}) {
  const navigate = useNavigate();
  const { user, isAdmin } = useAuth();
  const ids = { audience: useId(), subject: useId(), body: useId(), day: useId(), when: useId(), change: useId() };
  const form = useRef<HTMLFormElement>(null);
  const heading = useRef<HTMLHeadingElement>(null);
  // The question replaces the button that asked it, so focus moves to it: to its button when the sender pressed Send now
  // or Schedule, and otherwise (a refusal put it there) to the line that says what happened, so a key still held from
  // the press that was refused chooses nothing.
  const confirmButton = useRef<HTMLButtonElement>(null);
  const confirmLead = useRef<HTMLParagraphElement>(null);
  const previewSection = useRef<HTMLElement>(null);
  const alive = useRef(true);
  useEffect(() => {
    alive.current = true;

    return () => { alive.current = false; };
  }, []);

  const start = opened ? formTime(opened, zone) : null;
  const firstChoice = opened ? fromAudience(opened.audience, options) : prefillChoice(params, options);
  const [message, setMessage] = useState<Message | null>(opened);
  const [editing, setEditing] = useState(!opened || edit);

  const [choice, setChoice] = useState<AudienceChoice>(firstChoice);
  const [subject, setSubject] = useState(opened?.subject ?? "");
  const [body, setBody] = useState(opened?.bodyHtml ?? "");
  const [when, setWhen] = useState<"now" | "later">(start?.when ?? "now");
  const [day, setDay] = useState(start?.day ?? "");
  const [time, setTime] = useState(start?.time ?? "09:00");
  // "Push notification too" (DEC-123, DEC-127), on by default. Hidden when
  // the church turned notifications off; the saved value stays as it was.
  const [pushAlert, setPushAlert] = useState(opened?.pushAlert ?? true);

  // What was last saved, to tell edits from it; body is null until the
  // editor says how it writes the saved HTML.
  const [saved, setSaved] = useState<{ subject: string; body: string | null; key: string; pushAlert: boolean }>(
    { subject: opened?.subject ?? "", body: null, key: audienceKey(toAudience(firstChoice)), pushAlert: opened?.pushAlert ?? true });

  const [errors, setErrors] = useState<Errors>({});
  const action = useAction();
  // Which press the action is running, for its button's words; set when its work starts.
  const kind = useRef<Kind | null>(null);
  const doing = (k: Kind) => action.running && kind.current === k;
  const [preview, setPreview] = useState<(MessagePreview & { id: number; n: number }) | null>(null);
  // The question before sending, for exactly what was on screen (snap).
  const [confirm, setConfirm] = useState<Asked | null>(null);
  // The last review taken of the form's audience: while it is in hand the picker shows its names and counts, the ones the
  // sender confirms, until the audience is changed. Bumped by "Review the whole list": the picker opens that list.
  const [reviewed, setReviewed] = useState<{ key: string; summary: AudienceSummary } | null>(null);
  const [reviewing, setReviewing] = useState(0);
  // Bumped when the server refuses the people chosen: the picker drops the list it showed and counts again.
  const [recount, setRecount] = useState(0);
  // Once it goes out: who got it, checked every few seconds while sending.
  const { history, failed: historyFailed, stale, announce, retryLoad } = useMessageHistory(editing ? null : message, setMessage);
  const [retryError, setRetryError] = useState("");

  // The message the saves are for: null until a new draft's first save lands.
  const messageId = useRef<number | null>(opened?.id ?? null);
  // The message as the last answer or read showed it, with its revision: what the next send names.
  const known = useRef<Message | null>(opened);
  const lastRead = useRef<Message | null>(null);
  // A send whose answer was lost: the next answer from the server settles it.
  const lostSend = useRef<Asked | null>(null);
  // The token an edit's answer gives for the scheduled list the sender just confirmed, for the schedule press after it.
  const savedReview = useRef<string | undefined>(undefined);

  const save = useRevisionSave<MessageValue, ReturnType<typeof messageFields>>({
    opened: opened ? valueOf(opened) : BLANK,
    revision: opened?.revision,
    read: async () => {
      const { message: m } = await Messages.get(messageId.current ?? 0);
      lastRead.current = m;
      known.current = m;

      return { allowed: m.editable, revision: m.revision, value: valueOf(m) };
    },
    fields: messageFields,
    answers: (answer): answer is SavedAnswer => namesAMessage(answer) && (messageId.current === null || answer.message.id === messageId.current),
    stays: () => (known.current ? valueOf(known.current) : BLANK),
    // A message that isn't there, or isn't this person's. A 403 or another 404 is about the people chosen.
    gone: (err) => err.status === 404 && err.code === "not_found",
    changedCode: "message_changed",
    onConflict: refused,
  });

  const audience = toAudience(choice);
  const key = audienceKey(audience);
  const dirty = editing && (saved.subject !== subject.trim() || saved.key !== key || saved.pushAlert !== pushAlert || (saved.body !== null && saved.body !== body));
  const pushShown = !!options.pushEnabled;
  const { markSaved, resume } = useUnsavedChanges(dirty);
  // Any change after "Send to N people?" withdraws the question: it would
  // otherwise send something other than what it asked about.
  const snap = JSON.stringify({ subject: subject.trim(), body, key, pushAlert, when, day, time });
  const asking = confirm && confirm.snap === snap ? confirm : null;
  useEffect(() => { setConfirm((c) => (c && c.snap !== snap ? null : c)); }, [snap]);
  useEffect(() => {
    if (!asking) return;
    const target = asking.kind === "send" && !asking.change && !asking.again && !asking.afterSave ? confirmButton.current : confirmLead.current;
    target?.focus({ preventScroll: true });
    target?.scrollIntoView?.({ block: "center" });
  }, [asking]);
  const previewN = preview?.n ?? 0;
  useEffect(() => {
    if (previewN) previewSection.current?.scrollIntoView?.({ block: "start" });
  }, [previewN]);

  // Leaves the editor for the message as the page shows it; Edit opens a new Composer from it (onEdit).
  function closeEditor() {
    setEditing(false);
    setErrors({});
    setConfirm(null);
  }

  // The save was refused and not sent again (DEC-163), or a send after a save that landed was: the editor closes on its
  // own and the page shows the message as it is now, or says it is not there.
  function refused(why: RevisionConflict, what: "save" | "send" = "save") {
    if (!alive.current) return;
    const m = lastRead.current;
    markSaved();

    if (!m) {
      refusal.refused("gone", what);
      onGone();

      return;
    }

    setMessage(m);
    closeEditor();
    refusal.refused(why, what);
  }

  // A review of the form's audience is in hand: the picker shows it.
  function took(r: MessageReview) {
    setReviewed({ key, summary: r.summary });
  }

  // The editor writes saved HTML its own way; that is the baseline.
  function onEditorReady(html: string) {
    setBody(html);
    setSaved((s) => (s.body === null ? { ...s, body: html } : s));
  }

  function show(next: Errors) {
    showFieldErrors(form.current, () => setErrors(next));
  }

  function problems(k: Kind): Errors {
    const e: Errors = {};

    if (!audience) e.audience = choice.target ? "Choose at least one kind of person." : ERROR_COPY.message_audience;

    if (k === "test" || k === "send") {
      if (!subject.trim()) e.subject = "Add a subject.";

      if (!body) e.body = "Write your message.";
    }

    if (k === "send" && when === "later") {
      const at = wallToInstant(day, time, zone);
      const ms = at ? new Date(at).getTime() : NaN;

      if (!day || !time) e.when = "Choose a date and a time.";
      else if (!at || ms < Date.now() || ms > Date.now() + YEAR_MS) e.when = ERROR_COPY.message_when;
    }

    return e;
  }

  // The message went out as a press asked, its answer seen now or lost before: the page shows it.
  function sent(m: Message) {
    lostSend.current = null;
    known.current = m;
    setMessage(m);
    setEditing(false);
    setConfirm(null);
    setPreview(null);
    refusal.clear();
    requestAnimationFrame(() => heading.current?.focus());
  }

  // place shows a server error under its field, or with the buttons. A
  // message that started sending meanwhile (message_locked) is read again;
  // when that is a send of this page's whose answer was lost, it went.
  async function place(cause: unknown, fallback: string, where: keyof Errors = "form") {
    const code = errorCode(cause);
    const field = code ? FIELD_OF[code] : undefined;
    const text = errorText(cause, fallback);

    if (code === "message_locked" && message) {
      try {
        const { message: m } = await Messages.get(message.id);

        if (!alive.current) return;
        const lost = lostSend.current;

        if (lost && wentOut(m, lost)) {
          sent(m);

          return;
        }

        markSaved();
        known.current = m;
        setMessage(m);
        closeEditor();
        setErrors({ form: text });
      } catch {
        if (alive.current) show({ form: text });
      }

      return;
    }

    // The people chosen were refused: the list on screen is no longer who gets it, and a question about it is withdrawn.
    if (field === "audience") {
      setConfirm(null);
      setReviewed(null);
      setRecount((n) => n + 1);
    }

    show({ [field ?? where]: text });
  }

  // Saves the form through the one conflict protocol: a new message's first save creates it, and every later one names
  // the read it was made from. null: the save was refused and the editor has closed (or the page went).
  // review is the token of a scheduled message's new list the sender confirmed (DEC-169).
  async function saveForm(review?: string): Promise<Message | null> {
    if (!audience) throw new Error("save needs an audience");

    refusal.clear();
    lastRead.current = null;
    savedReview.current = undefined;
    const draft = { subject: subject.trim(), bodyHtml: body, audience, pushAlert };
    const creating = messageId.current === null;

    const outcome = await save(async (revision) => {
      const res = messageId.current === null ? await Messages.create(draft) : await Messages.update(messageId.current, draft, revision, review);

      if (namesAMessage(res) && (messageId.current === null || res.message.id === messageId.current)) {
        messageId.current = res.message.id;
        known.current = res.message;
        savedReview.current = "review" in res && isString(res.review) ? res.review : undefined;
      }

      return res;
    }, { ...draft, channel: "email", status: known.current?.status ?? "draft", scheduledAt: known.current?.scheduledAt ?? null });

    if (outcome === "refused") throw refusedAgain("message_changed");

    if (outcome !== "saved" || !alive.current || !known.current) return null;
    const m = known.current;
    setMessage(m);
    setSaved({ subject: draft.subject, body, key, pushAlert });

    if (creating) {
      // The new address is the same draft, now saved: no "Leave without
      // saving?" for this move, and the guard is back right after it.
      onShown(m.id);
      markSaved();
      navigate(`/messages/${m.id}`, { replace: true });
      resume();
    }

    return m;
  }

  // A picture is added to a saved message, so a new one is saved as a draft
  // first (it needs its audience, like any draft). The editor shows the words.
  async function saveForPicture(): Promise<{ id: number } | { error: string }> {
    if (messageId.current !== null) return { id: messageId.current };

    if (!audience) return { error: "Choose who this message is for first. A picture is added to a saved draft." };

    if (action.running) return { error: "The draft is being saved. Add the picture again in a moment." };
    let out: { id: number } | { error: string } = { error: "We couldn't save the draft first." };
    await action.run(async () => {
      kind.current = "draft";

      try {
        const m = await saveForm();

        if (m) out = { id: m.id };
      } catch (e) {
        out = { error: errorText(e, "We couldn't save the draft first.") };
      }

      return false;
    });

    return out;
  }

  function run(k: Press) {
    const found = problems(k);

    if (Object.keys(found).length) {
      show(found);

      return;
    }

    void action.run(async () => {
      kind.current = k;
      setErrors({});
      setConfirm(null);

      return press(k);
    });
  }

  // Saves the form, then does what the press is for. list is the confirmation of a scheduled message's new list the
  // press is answering: the save names its review, and the press goes on once it lands.
  async function press(k: Press, list?: Asked): Promise<false | string> {
    let m: Message | null;

    try {
      m = await saveForm(list?.review.token);
    } catch (e) {
      const r = reviewRefusal(e);

      if (r && k !== "draft") {
        askSave(k, r, !!list && r.change === null);

        return false;
      }

      await place(e, "We couldn't save the message.");
      const stored = known.current;

      // A scheduled message's new list is saved at the time it has, so that is the day whose limit refused it, whatever
      // day the form shows: the question is withdrawn and the form says which day, and what to do first. Only for a
      // confirmed list: the server checks the day's limit on every scheduled edit, a words-only one included.
      if (list && errorCode(e) === "message_daily_limit" && stored?.status === "scheduled") {
        setConfirm(null);

        return listLimitLine(stored.scheduledAt, zone);
      }

      return false;
    }

    if (!m) return false;

    if (list) setConfirm(null);

    try {
      if (k === "draft") return "Draft saved.";

      // A Schedule press that changed a scheduled message's list: the list is saved, at the time it already had. A new
      // time is its own question, the ordinary send's, so no list is ever stored beside a time nobody was asked about.
      // The save's answer seals the list just confirmed for the text it left, so that question needs no new review.
      if (k === "send" && list) {
        if (keepsTime(m)) {
          sent(m);

          return false;
        }

        const token = savedReview.current;
        await ask(m, null, false, token ? { ...list.review, token } : undefined, true);

        return false;
      }

      if (k === "preview") {
        const { preview: p } = await Messages.preview(m.id);
        setPreview((old) => ({ ...p, id: m.id, n: (old?.n ?? 0) + 1 }));
      }

      if (k === "test") {
        await Messages.test(m.id);

        return `Sent to you at ${user?.email ?? "your email address"}.`;
      }

      if (k === "send") await ask(m, null, false);
    } catch (e) {
      const fallback = { draft: "", preview: "We couldn't show the preview.", test: "We couldn't send the test.", send: "We couldn't count who gets it." }[k];
      await place(e, fallback, k === "test" ? "test" : "form");
    }

    return false;
  }

  // The form's time, as a send asks for it: null for now.
  const formAt = () => (when === "later" ? wallToInstant(day, time, zone) : null);

  // The form asks for the time m is already scheduled for, to the minute the form shows.
  function keepsTime(m: Message): boolean {
    if (when !== "later" || m.status !== "scheduled" || !m.scheduledAt) return false;
    const has = wallClock(m.scheduledAt, zone);

    return has.day === day && has.time === time;
  }

  // Takes a review of who m reaches and asks before sending (DEC-169).
  async function ask(m: Message, change: AudienceChange | null, again: boolean, review?: MessageReview, afterSave = false) {
    const r = review ?? (await Messages.review(m.id)).review;

    if (!alive.current) return;

    took(r);

    if (r.summary.eligible === 0) {
      show({ audience: ERROR_COPY.message_nobody });

      return;
    }

    setConfirm({ kind: "send", goesOn: "send", revision: m.revision, value: valueOf(m), at: formAt(), review: r, change, again, afterSave, snap });
  }

  // A scheduled message's save changed who it goes to: the server wrote nothing and answered a review of the new list,
  // which the sender confirms before the press goes on (DEC-169). Going back leaves it scheduled for its old list.
  function askSave(then: GoesOn, r: ReviewRefusal, again: boolean) {
    if (!alive.current) return;
    const m = known.current;
    took(r.review);
    setConfirm({ kind: "save", goesOn: then, revision: m?.revision, value: m ? valueOf(m) : BLANK, at: formAt(), review: r.review, change: r.change, again, snap });
  }

  // The confirmation's press: a save of a scheduled message's new list goes on with the press that asked it; a send
  // names the text it was pressed on and the review it showed.
  function confirmed() {
    const asked = asking;

    if (!asked) return;

    if (asked.kind === "save") {
      void action.run(async () => {
        kind.current = asked.goesOn;

        return press(asked.goesOn, asked);
      });

      return;
    }

    void action.run(async () => {
      kind.current = "schedule";

      return send(asked);
    }, { fallback: UNSURE_SEND });
  }

  // The schedule: it names the text it was pressed on and the review it showed. A changed list shows the change and a
  // fresh review; a lost answer is settled by the next press, which the server refuses if the first one went.
  async function send(asked: Asked): Promise<false | string> {
    const id = messageId.current;

    if (id === null) return false;

    try {
      const res = await Messages.schedule(id, { revision: asked.revision, review: asked.review.token, scheduledAt: asked.at ?? undefined });

      if (!alive.current) return false;

      if (!namesAMessage(res) || res.message.id !== id || res.message.status === "draft") {
        lostSend.current = asked;

        throw new ApiError(502, "unreadable answer", {}, true);
      }

      sent(res.message);

      return false;
    } catch (e) {
      if (!alive.current) return false;
      const refusedReview = reviewRefusal(e);

      if (refusedReview) {
        const m = known.current;

        if (m) await ask(m, refusedReview.change, refusedReview.change === null, refusedReview.review);

        return false;
      }

      if (outcomeOf(e) === "unsure") {
        lostSend.current = asked;

        throw e;
      }

      const changed = errorCode(e) === "message_changed";
      const lost = lostSend.current;

      // A refusal after an answer was lost may be about a message that already went (its time has passed, say): a read
      // settles that before anything else is said.
      if (changed || lost) {
        const { message: m } = await Messages.get(id);

        if (!alive.current) return false;

        // It went as asked, or it is past editing (sent at a time the server set, say): the page shows it as it is.
        if (lost && (wentOut(m, lost) || !m.editable)) {
          sent(m);

          return false;
        }

        if (changed) {
          known.current = m;

          // Nothing this send names changed (a restart, or the same words saved again): ask again on the message as it
          // is. A status or a time moved since the question was asked is someone else's change, never one to send over.
          if (m.editable && JSON.stringify(messageFields(valueOf(m))) === JSON.stringify(messageFields(asked.value))) {
            await ask(m, null, true);

            return false;
          }

          lastRead.current = m;
          refused(m.editable ? "changed" : "gone", "send");

          return false;
        }
      }

      setConfirm(null);
      const code = errorCode(e);

      if (code === "message_daily_limit") show({ form: messageDailyLimitText(asked.at, zone) });
      else await place(e, "We couldn't schedule the message.");

      // A new time for a message already scheduled was refused: it says where that leaves it. Not for a refusal the
      // server gives before it reads the message (the time asked for), or one that reads it again (it started sending).
      return asked.value.status === "scheduled" && code !== "message_locked" && code !== "message_when" ? notMovedLine(asked.value.scheduledAt, zone) : false;
    }
  }

  // "Review the whole list": back to the form, with the picker showing the fresh review's list, open.
  function reviewList() {
    setConfirm(null);
    setReviewing((n) => n + 1);
  }

  function cancelMessage() {
    if (!message) return;

    if (!window.confirm("Cancel this message? It won't be sent, and you can't undo this.")) return;

    void action.run(async () => {
      kind.current = "cancel";

      try {
        const { message: m } = await Messages.cancel(message.id);
        known.current = m;
        setMessage(m);
        setEditing(false);
      } catch (e) {
        await place(e, "We couldn't cancel the message.");
      }

      return false;
    });
  }

  // Try again (DEC-131): only the people it failed for, after asking.
  function retry() {
    if (!message) return;

    if (!window.confirm(retryQuestion(history?.counts.failed ?? message.failedCount, history?.counts.guardianCopiesFailed ?? 0))) return;
    setRetryError("");

    void action.run(async () => {
      kind.current = "retry";

      try {
        const { message: m } = await Messages.retry(message.id);
        setMessage(m);
      } catch (e) {
        setRetryError(errorText(e, "We couldn't send it again. Try again in a few minutes."));
      }

      return false;
    });
  }

  // Discarding a draft cancels it for good; it stays on the list as Discarded.
  function discardDraft() {
    if (!message) return;

    if (!window.confirm("Discard this draft? You can't undo this.")) return;

    void action.run(async () => {
      kind.current = "cancel";

      try {
        await Messages.cancel(message.id);

        if (!alive.current) return false;
        markSaved();
        navigate("/messages");
      } catch (e) {
        await place(e, "We couldn't discard the draft.");
      }

      return false;
    });
  }

  // Done editing a scheduled message: what Preview or the test saved stays;
  // only unsaved changes are asked about.
  function doneEditing() {
    if (!message) return;

    if (dirty && !window.confirm("Stop editing? Your unsaved changes will be lost.")) return;
    closeEditor();
  }

  function submit(e: FormEvent) {
    e.preventDefault();
    run("send");
  }

  const oneEvent = isOneEvent(audience);
  const scheduled = message?.status === "scheduled";
  const title = editing ? (message ? "Edit message" : "New message") : message?.displaySubject || message?.subject || "Untitled message";
  const note = action.state.status === "done" ? action.state.text : "";
  const formError = errors.form ?? (action.state.status === "failed" ? action.state.text : undefined);

  if (!editing && message) {
    const reminder = message.category === "reminder";
    // A leader opens a reminder from its date page (it isn't on their list), so that is where back goes.
    const datePath = message.reminder ? `/events/${message.reminder.eventId}/dates/${message.reminder.date}` : null;
    // A push reminder (M3.13d3) has push rows only (statusLabel words its
    // state), so its email counts are always empty.
    const pushReminder = message.channel === "push";
    const label = statusLabel(message, zone);

    return <>
      {reminder && !isAdmin && message.reminder && datePath
        ? <ButtonLink variant="link" to={datePath}>‹ Back to {message.reminder.eventTitle}</ButtonLink>
        : reminder && isAdmin
          ? <ButtonLink variant="link" to="/messages?show=reminders">‹ Back to automatic reminders</ButtonLink>
          : back}
      <h1 ref={heading} tabIndex={-1} className="fold-page-header text-3xl font-bold focus:outline-none">{title}</h1>
      <RefusedSaveLine save={refusal} noun="message" canManage />
      <SendProblem message={message} history={history} busy={doing("retry")} error={retryError} onRetry={retry} />
      <section aria-label="This message" className="space-y-6">
        <div className="space-y-1">
          <p className="text-lg font-semibold">{label}</p>
          {history && !pushReminder && <p className="text-sm">{historyLine(history, message)}</p>}
          {history && pushHistoryLine(history.counts.push) && <p className="text-sm">{pushHistoryLine(history.counts.push)}</p>}
          {history && message.status !== "sending" && history.counts.teens > 0 && <p className="text-sm text-text-muted">{historyTeenLine(history.counts.teens)}</p>}
          {history && message.status !== "sending" && guardianLine(history.counts) && <p className="text-sm text-text-muted">{guardianLine(history.counts)}</p>}
          {historyFailed && !history && (
            <p className="text-sm text-text-muted">
              We couldn't load who got it. <Button variant="link" onClick={retryLoad}>Load again</Button>
            </p>
          )}
          {stale && (
            <p className="text-sm text-text-muted">
              Nothing has changed for a while. <Button variant="link" onClick={retryLoad}>Refresh to check again</Button>
            </p>
          )}
          {/* Said once when a send settles, not on every check. */}
          <p role="status" className="sr-only">{announce}</p>
        </div>
        {scheduled && message.editable && <p className="text-sm text-text-muted">You can change or cancel it until it starts sending.</p>}
        {/* An automatic reminder (M3.6b) has no author: what it was for, in place of the author line. */}
        {reminder && (
          <p className="text-sm">
            {reminderName(message.reminder)}
            {message.reminder && datePath && <> for <Link to={datePath} className="font-medium text-primary underline">{reminderDateText(message.reminder)}</Link></>}
          </p>
        )}
        <dl className="grid gap-1 text-sm sm:grid-cols-[8rem_1fr]">
          <dt className="font-medium text-text-muted">To</dt>
          <dd>{reminder ? reminderAudience(message.reminder) : audienceLabel(message.audience, options)}</dd>
          {!reminder && message.authorName && message.authorName !== (user ? publicName(user) : undefined) && <>
            <dt className="font-medium text-text-muted">Written by</dt>
            <dd>{message.authorName}</dd>
          </>}
        </dl>
        {history && <WhoGotIt history={history} withCopies={isAdmin} />}
        <div className="fold-section">
          <Suspense fallback={<Skeleton kind="lines" label="Loading the message…" />}>
            <RichTextView html={message.bodyHtml} />
          </Suspense>
        </div>
      </section>
      {formError && <FormError>{formError}</FormError>}
      {message.editable && (
        <FormActions>
          <Button variant="primary" layout="w-full sm:w-auto" disabled={action.running} onClick={() => onEdit(message)}>Edit</Button>
          <Button variant="danger-outline" layout="w-full sm:w-auto" disabled={action.running} onClick={cancelMessage}>
            {doing("cancel") ? "Cancelling…" : "Cancel message"}
          </Button>
        </FormActions>
      )}
    </>;
  }

  // A confirmation of a scheduled message's new list for Preview or the test saves it and sends nothing; one for Schedule
  // or Send now asks the send's own question.
  const savingList = asking?.kind === "save";
  const busyWord = savingList ? "Saving…" : asking?.at ? "Scheduling…" : "Sending…";
  const pushOn = pushShown && pushAlert;

  return <>
    {back}
    <h1 ref={heading} tabIndex={-1} className="fold-page-header text-3xl font-bold focus:outline-none">{title}</h1>
    <Form ref={form} aria-label={title} onSubmit={submit} className="space-y-8">
      <section aria-labelledby={`${ids.audience}-h`} className="space-y-4">
        <h2 id={`${ids.audience}-h`} className="text-xl font-semibold">Who gets it</h2>
        <AudiencePicker id={ids.audience} messageId={message?.id} options={options} choice={choice} error={errors.audience} push={pushOn}
          reviewed={reviewed?.key === key ? reviewed.summary : null} reviewing={reviewing} recount={recount}
          onChange={(c) => { setChoice(c); setReviewed(null); setErrors((e) => ({ ...e, audience: undefined })); }} />
        {/* A companion to the email, never instead of it (DEC-123). */}
        {pushShown && (
          <Checkbox label="Push notification too"
            description="People who turned on notifications also get a short note: the subject and the start of the message."
            checked={pushAlert} onChange={(e) => setPushAlert(e.target.checked)} />
        )}
      </section>

      <section aria-labelledby={`${ids.body}-h`} className="fold-section space-y-5">
        <h2 id={`${ids.body}-h`} className="text-xl font-semibold">What it says</h2>
        <Field id={ids.subject} label="Subject" hint="One line, up to 200 characters." error={errors.subject}>
          <Input value={subject} maxLength={200} onChange={(e) => setSubject(e.target.value)} />
        </Field>
        <Field id={ids.body} label="Message" hint={oneEvent
          ? "Insert field adds each person's first name, or the event's title, time and links."
          : "Insert field adds each person's first name."} error={errors.body}>
          <Suspense fallback={<Skeleton kind="lines" label="Loading the editor…" />}>
            <RichTextEditor value={body} onChange={setBody} onReady={onEditorReady} oneEvent={oneEvent} pictures={{ messageId: message?.id ?? null, save: saveForPicture }} />
          </Suspense>
        </Field>
        <div className="flex flex-wrap gap-2">
          <Button variant="secondary" layout="w-full sm:w-auto" disabled={action.running} onClick={() => run("preview")}>{doing("preview") ? "Saving…" : "Preview"}</Button>
          <Button variant="secondary" layout="w-full sm:w-auto" disabled={action.running} onClick={() => run("test")}>{doing("test") ? "Sending…" : "Send a test to me"}</Button>
        </div>
        {scheduled && <p className="text-sm text-text-muted">Your changes save when you preview or send a test. The message stays scheduled.</p>}
        {errors.test && <FormError>{errors.test}</FormError>}
      </section>

      {preview && message && preview.id === message.id && (
        <section ref={previewSection} aria-labelledby={`${ids.body}-p`} className="scroll-mt-[calc(var(--fold-header-height,4rem)+1rem)] space-y-3">
          <h2 id={`${ids.body}-p`} className="text-xl font-semibold">Preview</h2>
          <p className="text-sm text-text-muted">{dirty ? "This is the last saved version. Preview again to see your changes." : "How it looks to you, with your own name in the fields."}</p>
          <p><span className="font-semibold">Subject: </span>{preview.subject || "(no subject yet)"}</p>
          {/* iframe-ok: the email preview, our own page served sandboxed with no scripts (DEC-16 is about video). */}
          <iframe key={preview.n} src={previewHTMLPath(message.id)} sandbox="allow-popups allow-popups-to-escape-sandbox" title="Email preview"
            className="h-[32rem] w-full rounded-md border border-border bg-surface" />
          <details>
            <summary className="inline-flex min-h-11 cursor-pointer items-center text-sm font-semibold text-primary">Plain-text version</summary>
            <pre className="mt-2 whitespace-pre-wrap rounded-md border border-border bg-surface p-3 font-sans text-sm">{preview.text}</pre>
          </details>
          <Button variant="ghost" onClick={() => setPreview(null)}>Close preview</Button>
        </section>
      )}

      <section aria-labelledby={`${ids.when}-h`} className="fold-section space-y-4">
        <h2 id={`${ids.when}-h`} className="text-xl font-semibold">When</h2>
        <Choices legend="When to send">
          <Radio name={ids.when} label="Send now" checked={when === "now"} onChange={() => setWhen("now")} />
          <Radio name={ids.when} label="Pick a date and time" checked={when === "later"} onChange={() => setWhen("later")} />
        </Choices>
        {when === "later" && <>
          <p className="field-hint">{zoneNote(zone)} You can pick up to a year ahead.</p>
          <div className="grid gap-3 sm:grid-cols-2">
            <Field id={ids.day} label="Date" error={errors.when}>
              <Input type="date" value={day} onChange={(e) => setDay(e.target.value)} />
            </Field>
            <Field label="Time">
              <Input type="time" value={time} onChange={(e) => setTime(e.target.value)} />
            </Field>
          </div>
        </>}
        {when === "now" && errors.when && <FormError>{errors.when}</FormError>}
      </section>

      {formError && <FormError>{formError}</FormError>}
      <p role="status" className="text-sm text-text-muted empty:hidden">{note}</p>
      {asking?.change ? (
        <div role="group" aria-labelledby={`${ids.change}-h`} aria-describedby={`${ids.change}-d`} className="space-y-3 rounded-lg border border-primary-border bg-primary-subtle p-4">
          <p ref={confirmLead} id={`${ids.change}-h`} role="status" tabIndex={-1} className="font-semibold text-primary-on-subtle outline-none">The list of people who get this has changed since you looked.</p>
          <div id={`${ids.change}-d`} className="space-y-1 text-sm text-primary-on-subtle">
            {changeLines(asking.change).map((line) => <p key={line}>{line}</p>)}
            <p>{savingList ? saveListLine(asking.review.reached) : confirmLine(asking.review.reached, asking.at, zone)}</p>
            {savingList && <p>{stillScheduledLine(asking.value.scheduledAt, zone)}</p>}
          </div>
          <RecipientList summary={asking.review.summary} push={pushOn} />
          <FormActions>
            <Button ref={confirmButton} variant="primary" layout="w-full sm:w-auto" disabled={action.running} onClick={confirmed}>
              {action.running ? busyWord : savingList ? "Continue and save" : asking.at ? "Continue and schedule" : "Send to the list as it is now"}
            </Button>
            <Button variant="secondary" layout="w-full sm:w-auto" disabled={action.running} onClick={reviewList}>Review the whole list</Button>
            <Button variant="ghost" layout="w-full sm:w-auto" disabled={action.running} onClick={() => setConfirm(null)}>Go back</Button>
          </FormActions>
        </div>
      ) : asking ? (
        <div role="group" aria-labelledby={`${ids.when}-c`} className="space-y-3 rounded-lg border border-primary-border bg-primary-subtle p-4">
          <p ref={asking.again ? undefined : confirmLead} id={`${ids.when}-c`} tabIndex={-1} className="font-semibold text-primary-on-subtle outline-none">
            {savingList ? saveListLine(asking.review.reached) : confirmLine(asking.review.reached, asking.at, zone)}
          </p>
          {asking.again && <p ref={confirmLead} role="status" tabIndex={-1} className="text-sm text-primary-on-subtle outline-none">We checked who gets it again, so this is the list as it is now.</p>}
          {savingList
            ? <p className="text-sm text-primary-on-subtle">To {audience ? audienceLabel(audience, options) : "the people you chose"}. {stillScheduledLine(asking.value.scheduledAt, zone)}</p>
            : <p className="text-sm text-primary-on-subtle">To {audience ? audienceLabel(audience, options) : "the people you chose"}. You can change or cancel it until it starts sending.</p>}
          {pushOn && asking.review.summary.push > 0 && <p className="text-sm text-primary-on-subtle">{pushLine(asking.review.summary)}</p>}
          <RecipientList summary={asking.review.summary} push={pushOn} />
          <FormActions>
            <Button ref={confirmButton} variant="primary" layout="w-full sm:w-auto" disabled={action.running} onClick={confirmed}>
              {action.running ? busyWord : savingList ? "Save the change" : asking.at ? "Schedule" : "Send now"}
            </Button>
            <Button variant="secondary" layout="w-full sm:w-auto" disabled={action.running} onClick={() => setConfirm(null)}>Go back</Button>
          </FormActions>
        </div>
      ) : (
        <FormActions>
          <Button type="submit" variant="primary" layout="w-full sm:w-auto" disabled={action.running}>{doing("send") ? "Saving…" : when === "now" ? "Send now" : "Schedule"}</Button>
          {scheduled
            ? <Button variant="secondary" layout="w-full sm:w-auto" disabled={action.running} onClick={doneEditing}>Done editing</Button>
            : <Button variant="secondary" layout="w-full sm:w-auto" disabled={action.running} onClick={() => run("draft")}>{doing("draft") ? "Saving…" : "Save draft"}</Button>}
          {message?.status === "draft" && (
            <Button variant="danger-outline" layout="w-full sm:w-auto" disabled={action.running} onClick={discardDraft}>
              {doing("cancel") ? "Discarding…" : "Discard draft"}
            </Button>
          )}
        </FormActions>
      )}
    </Form>
  </>;
}

// When a message's form says it goes: a scheduled time still ahead, as that day and time on the church's clock.
interface FormTime {
  when: "now" | "later";
  day: string;
  time: string;
}

function formTime(m: Message, zone: string): FormTime {
  const later = m.status === "scheduled" && m.scheduledAt && new Date(m.scheduledAt).getTime() > Date.now() + 60_000;

  if (!later || !m.scheduledAt) return { when: "now", day: "", time: "09:00" };
  const w = wallClock(m.scheduledAt, zone);

  return { when: "later", day: w.day, time: w.time };
}
