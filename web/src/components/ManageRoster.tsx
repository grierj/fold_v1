import { rosterWriteFailure, useRosterWrites, type RosterGate } from "../lib/rosterWrites";
import { Form } from "./ui/Form";
import { AddRequestForm } from "./AddRequestForm";
import { PeopleList } from "./PeopleList";
import { Menu } from "./ui/Menu";
import { PeoplePicker } from "./ui/PeoplePicker";
import { useEffect, useState } from "react";
import { useAuth } from "../auth";
import { RequestContactButton } from "./RequestContactButton";
import { Events } from "../api/events";
import { attendeeSubject } from "../lib/attendee";
import type { PersonSearchResult, Attendee, EventOccurrence } from "../types";
import { errorCode, errorText } from "../lib/errors";
import { AttendeeDetails } from "./AttendeeDetails";
import { Button } from "./ui/Button";
import { Field, FormError, Input } from "./ui/Field";
import { EmptyState } from "./ui/EmptyState";
import { registrationStatusLabel } from "../lib/status";
import { poolFull } from "../lib/registration";
import { canAskForContact, isActiveAdult } from "../lib/can";
import { useConfirm } from "./ui/ConfirmDialog";

const poolName = (a: Attendee) => a.attendanceMode === "remote" ? "remote" : "in-person";

// Why a host that fails closed is asked to read the roster again: the window came back, or a care note's
// window ended.
/** @public the kept-dates panel's read tells the two apart (it ignores "focus"). */
export type RosterRecheck = "focus" | "care";

// How the roster's host refreshes it, one shape or the other so a host can't mix them.
type RosterRefresh =
  // A host that reads quietly (EventDetail's date rows): today's behavior.
  | { onChanged: () => void; gate?: undefined; onRecheck?: undefined }
  // A host that fails closed (ManageDate, the kept-dates panel): writes are reported to the gate, which starts
  // the read after each (lib/rosterWrites), and rechecks go to onRecheck.
  | { gate: RosterGate; onRecheck: (why: RosterRecheck) => void; onChanged?: undefined };

type ManageRosterProps = RosterRefresh & { instance: EventOccurrence; onClose: () => void; headingLevel?: 3 | 4 };

export function ManageRoster({ instance, onChanged, gate, onRecheck, onClose, headingLevel = 3 }: ManageRosterProps) {
  const Heading = headingLevel === 3 ? "h3" : "h4";
  const Subheading = headingLevel === 3 ? "h4" : "h5";
  const { user } = useAuth();
  const [busy, setBusy] = useState<number | "add" | null>(null);
  const [email, setEmail] = useState("");
  const [adding, setAdding] = useState((instance.attendees?.length ?? 0) <= 6);
  const [selection, setSelection] = useState<PersonSearchResult | null>(null);
  const [approvalRequired, setApprovalRequired] = useState<{ overCapacity: boolean } | null>(null);
  const [askPerson, setAskPerson] = useState<{ person: PersonSearchResult; overCapacity: boolean } | null>(null);
  const [error, setError] = useState<string | null>(null);
  // Roster writes on this date in flight for the session, this roster's or another's: none starts while one is.
  const own = useRosterWrites(instance.eventId, instance.date);
  const writes = gate ?? own;
  const blocked = busy !== null || writes.writing;
  // Roster questions are about the page as it was when the action started (EventDetail's usePageFacts).
  const beginAction = useConfirm();
  // An add is also for the form it was typed in: hiding the form, even if it is opened again, drops its retry.
  const beginAdd = useConfirm(adding);
  // Refresh at the next care boundary and whenever a background tab returns.
  useEffect(() => {
    const care = () => (onRecheck ? onRecheck("care") : onChanged?.());
    const focus = () => (onRecheck ? onRecheck("focus") : onChanged?.());
    const boundaries = (instance.attendees ?? []).flatMap((a) => a.householdPerson?.careNotesVisibleUntil ? [new Date(a.householdPerson.careNotesVisibleUntil).getTime()] : []).filter((t) => t >= Date.now());
    const timer = boundaries.length ? window.setTimeout(care, Math.max(0, Math.min(...boundaries) - Date.now() + 1)) : undefined;
    window.addEventListener("focus", focus);

    return () => {
      window.removeEventListener("focus", focus);

      if (timer !== undefined) window.clearTimeout(timer);
    };
  }, [instance.attendees, onChanged, onRecheck]);

  async function action(a: Attendee, kind: "remove" | "promote" | "restore") {
    if (blocked) return;
    const pending = beginAction();

    // DEC-156: a kept row's Remove ends what they kept after leaving, and never promises their place back.
    const body = a.keptAfterLeaving === true
      ? "They left the group and kept this date. Removing them ends that and tells them. They won't be able to re-register on their own."
      : "They won't be able to re-register on their own.";

    if (kind === "remove" && !(await pending.ask({
      title: `Remove ${a.name}'s registration for this date?`,
      body,
      confirmLabel: "Remove",
      tone: "danger",
      native: `Remove ${a.name}'s registration for this date? ${body}`,
    }))) return;
    const full = poolFull(instance, a.attendanceMode);

    if (kind === "restore" && full && !(await pending.ask({
      title: `Add ${a.name} over capacity?`,
      body: `This date's ${poolName(a)} seats are full.`,
      confirmLabel: "Add over capacity",
      tone: "normal",
      native: `This date's ${poolName(a)} seats are full. Add ${a.name} over capacity?`,
    }))) return;

    if (!pending.current()) return;
    const subject = kind === "restore" ? attendeeSubject(a) : null;
    const target = subject?.kind === "household" ? { personId: subject.person.personId, registeredBy: subject.person.registeredBy.userId } : subject?.kind === "user" ? { userId: subject.userId } : null;

    if (kind === "restore" && !target) return;
    // One write from its first request to its answer, the over-capacity retry included.
    const end = writes.begin();
    setBusy(a.registrationId); setError(null);

    try {
      if (kind === "remove") await Events.removeRegistration(instance.eventId, instance.date, a.registrationId);
      else if (kind === "promote") await Events.promoteRegistration(instance.eventId, instance.date, a.registrationId);
      else if (target) {
        try { await Events.restoreRegistration(instance.eventId, instance.date, target, a.attendanceMode, full); }
        catch (e) {
          // A closed roster or a refresh that changed these facts drops the retry: the refusal stands.
          if (errorCode(e) !== "event_full" || full || !(await pending.ask({
            title: `Add ${a.name} over capacity?`,
            body: `This date's ${poolName(a)} seats filled.`,
            confirmLabel: "Add over capacity",
            tone: "normal",
            native: `This date's ${poolName(a)} seats filled. Add ${a.name} over capacity?`,
          }))) throw e;
          await Events.restoreRegistration(instance.eventId, instance.date, target, a.attendanceMode, true);
        }
      }

      end("stored");
      // With a gate, the gate starts the read after the write.
      onChanged?.();
    } catch (e) {
      end(rosterWriteFailure(e));
      setError(errorText(e, "We couldn't update that registration."));
    }
    finally { setBusy(null); }
  }

  async function add(e: React.FormEvent) {
    e.preventDefault();

    if (blocked) return;

    if (selection && (selection.teen || approvalRequired)) {
      setAskPerson({ person: selection, overCapacity: approvalRequired?.overCapacity ?? false });

      return;
    }

    if (!selection && !email.trim()) return;
    const pending = beginAdd();
    const target = selection ? { selectionId: selection.selectionId } : email.trim();
    const full = poolFull(instance, "in_person");
    let overCapacity = full;

    if (full && !(await pending.ask({ title: "Add them over capacity?", body: "This date is full.", confirmLabel: "Add over capacity", tone: "normal", native: "This date is full. Add them over capacity?" }))) return;

    if (!pending.current()) return;
    const end = writes.begin();
    setBusy("add"); setError(null);

    try {
      try { await Events.addRegistration(instance.eventId, instance.date, target, full || undefined); }
      catch (e) {
        if (errorCode(e) !== "event_full" || full || !(await pending.ask({ title: "Add them over capacity?", body: "This date filled up.", confirmLabel: "Add over capacity", tone: "normal", native: "This date filled up. Add them over capacity?" }))) throw e;
        overCapacity = true;
        await Events.addRegistration(instance.eventId, instance.date, target, true);
      }

      end("stored");
      setEmail(""); setSelection(null); setApprovalRequired(null); onChanged?.();
    }
    catch (e) {
      end(rosterWriteFailure(e));

      if (["date_add_approval_required", "teen_add_approval_required"].includes(errorCode(e) ?? "")) {
        if (selection) { setApprovalRequired({ overCapacity }); setError("They need to approve this date. Choose Ask to add to send an ask using your selected person and options."); }
        else setError("Select this person by name, then choose Ask to add. An email address cannot be used to send an ask.");
      } else setError(errorText(e, "We couldn't add that registration."));
    }
    finally { setBusy(null); }
  }

  const roster = (instance.attendees ?? []).filter((a) => ["going", "maybe", "waitlisted"].includes(a.status));

  return <section aria-label="Manage sign-ups" className="mt-3 border-t border-border pt-3">
    <div className="flex items-center justify-between gap-3"><Heading className="font-semibold">Manage sign-ups</Heading><Button variant="link" onClick={onClose}>Close</Button></div>
    {error && <FormError>{error}</FormError>}
    <div className="my-3"><Button variant="secondary" aria-expanded={adding} onClick={() => setAdding(!adding)}>{adding ? "Close add person" : "Add person"}</Button></div>
    <div hidden={!adding} className="mb-4">
      <Form noValidate onSubmit={add} className="space-y-3">
        {isActiveAdult(user) && <Field label="Find a person by name"><PeoplePicker value={selection} disabled={blocked} onChange={(person) => { setSelection(person); setApprovalRequired(null); setAskPerson(null); setEmail(""); setError(null); }} /></Field>}
        <Field label="Add someone" hint="Or use their email address"><Input type="email" value={email} disabled={blocked} onChange={(e) => { setEmail(e.target.value); setSelection(null); setApprovalRequired(null); setAskPerson(null); }} /></Field><Button type="submit" variant="secondary" disabled={blocked || (!selection && !email.trim())}>{busy === "add" ? "Adding…" : selection && (selection.teen || approvalRequired) ? "Ask to add" : "Add"}</Button>
      </Form>
      {askPerson && isActiveAdult(user) && <AddRequestForm key={askPerson.person.selectionId} person={askPerson.person} scope={{ contextType: "event", contextId: instance.eventId, occurrenceId: instance.id, overCapacity: askPerson.overCapacity }} full={poolFull(instance, "in_person")} onClose={() => setAskPerson(null)} />}
    </div>
    {roster.length === 0 && <EmptyState title="No sign-ups yet" body="People who sign up for this date will appear here. You can add someone by name or email above." />}
    {roster.length > 0 && <PeopleList rows={roster} noun="sign-ups" sort={false} filters={[
      { value: "going", label: registrationStatusLabel("going"), matches: (a) => a.status === "going" },
      { value: "maybe", label: registrationStatusLabel("maybe"), matches: (a) => a.status === "maybe" },
      { value: "waitlisted", label: "Waitlist", matches: (a) => a.status === "waitlisted" },
    ]}>{(a) => <li key={a.registrationId} className="flex items-start justify-between gap-3 py-3 border-b border-border">
      <div className="min-w-0 flex-1"><AttendeeDetails attendee={a} childcare={instance.childcareAvailable} status={statusLabel(a, roster)} showAccountType />
        {a.addedByLeader === true && <p className="text-sm text-text-muted">Added by a leader</p>}
        {a.keptAfterLeaving === true && <p className="text-sm text-text-muted">Kept after leaving</p>}
        {a.userId !== undefined && a.userId > 0 && !a.householdPerson && canAskForContact(user, { id: a.userId, email: a.email, phone: a.phone }) && <RequestContactButton subjectUserId={a.userId} subjectName={a.name} contextType="event" contextId={instance.eventId} />}
      </div>
      <div className="flex flex-wrap gap-2">
        {a.status === "waitlisted" && <Button variant="secondary" disabled={blocked} onClick={() => void action(a, "promote")}>Promote</Button>}
        <Menu tone="quiet" label={`More for ${a.name}`} items={[{ label: `Remove ${a.name}`, danger: true, disabled: blocked, onSelect: () => void action(a, "remove") }]} />
      </div>
    </li>}</PeopleList>}
    {(instance.removedRegistrations?.length ?? 0) > 0 && <section aria-label="Removed or left out" className="my-4">
      <Subheading className="font-semibold">Removed or left out</Subheading>
      <PeopleList rows={instance.removedRegistrations ?? []} noun="removed sign-ups">{(a) => <li key={a.registrationId} className="flex flex-col sm:flex-row sm:items-start sm:justify-between gap-3">
        <div className="min-w-0 w-full sm:flex-1"><AttendeeDetails attendee={a} childcare={false} showAccountType /></div><Button variant="secondary" aria-label={`Add back ${a.name}`} disabled={blocked} onClick={() => void action(a, "restore")}>Add back</Button>
      </li>}</PeopleList>
    </section>}
  </section>;
}

// Roster status in the RSVP form's words (Illuminator, PR 110 N2).
// The API lists waiters in waitlist order; each attendance pool has its own line.
function statusLabel(a: Attendee, roster: Attendee[]): string {
  switch (a.status) {
    case "going": return a.attendanceMode === "remote" ? "Going remotely" : "Going";
    case "maybe": return registrationStatusLabel("maybe");
    case "waitlisted": {
      const line = roster.filter((r) => r.status === "waitlisted" && r.attendanceMode === a.attendanceMode);

      return `Waitlist position ${line.indexOf(a) + 1}`;
    }

    case "declined": return registrationStatusLabel("declined");
    default: return "Removed";
  }
}
