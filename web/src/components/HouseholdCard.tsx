import { isNumber } from "../lib/wireValues";
import { Form } from "./ui/Form";
import { useEffect, useRef, useState } from "react";
import { Households, type HouseholdPersonInput } from "../api/households";
import type { Household, HouseholdAccount, HouseholdPerson, HouseholdResponse } from "../types";
import { errorCode, errorText } from "../lib/errors";
import { RefusedSaveLine, useRefusedSave } from "../lib/refusedSave";
import { useLoad } from "../lib/useLoad";
import { answerNames, lostAuthority, refusedAgain, useManagedSave, useRevisionSave, type RevisionConflict } from "../lib/useRevisionSave";
import { formatBirthDate, formatWhen } from "../lib/when";
import { Button } from "./ui/Button";
import { Checkbox, Choices, Field, FormError, Input, Radio, showFieldErrors, Textarea } from "./ui/Field";
import { EmptyState } from "./ui/EmptyState";
import { ErrorNotice } from "./ErrorNotice";
import { HouseholdSkeleton } from "./HouseholdSkeleton";

// "My household" on the Profile page (M1.9 slice 1). GET never creates the
// household: it is null until the first person is added (Warden, PR 106).
// Children and teens wait for an admin to confirm the link (DEC-32).

const KIND_LABEL: Record<HouseholdPerson["kind"], string> = { child: "Child", teen: "Teen", adult: "Adult" };

const LINK_LABEL: Partial<Record<HouseholdPerson["linkStatus"], string>> = {
  pending: "Waiting for an admin to check",
  confirmed: "Confirmed",
  rejected: "Not confirmed",
  revoked: "Link ended",
};

const STONE = "inline-block rounded-full bg-sunken px-2 py-0.5 text-xs font-semibold text-text-muted";
const VIOLET = "inline-block rounded-full bg-primary-subtle px-2 py-0.5 text-xs font-semibold text-primary-on-subtle";

type Draft = { firstName: string; lastName: string; kind: HouseholdPerson["kind"]; birthDate: string; careNotes: string; photoConsent: boolean };

const emptyDraft: Draft = { firstName: "", lastName: "", kind: "child", birthDate: "", careNotes: "", photoConsent: false };

// personProblems checks a draft before it is sent. today is YYYY-MM-DD.
interface PersonProblems {
  firstName?: string;
  birthDate?: string;
}

/** @public tested directly: draft validation rules. */
export function personProblems(d: Draft, today: string): PersonProblems {
  const out: PersonProblems = {};

  if (!d.firstName.trim()) out.firstName = "Add a first name.";

  if (d.kind !== "adult" && !d.birthDate) out.birthDate = "Add a birth date for a child or teen.";
  else if (d.birthDate && d.birthDate > today) out.birthDate = "A birth date can't be in the future.";

  return out;
}

function PersonForm({ initial, onSave, onCancel, heading, level, careVisible = true, identityLocked = false }: {
  careVisible?: boolean;
  identityLocked?: boolean;
  initial: Draft;
  onSave: (input: HouseholdPersonInput) => Promise<void>;
  onCancel: () => void;
  heading: string;
  level: "h2" | "h3";
}) {
  const Heading = level;
  const [d, setD] = useState(initial);
  const [careTouched, setCareTouched] = useState(false);
  const [shown, setShown] = useState<ReturnType<typeof personProblems>>({});
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const formRef = useRef<HTMLFormElement>(null);
  const set = (patch: Partial<Draft>) => setD((prev) => ({ ...prev, ...patch }));

  useEffect(() => {
    formRef.current?.querySelector<HTMLInputElement>("input")?.focus();
  }, []);

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    const problems = personProblems(d, new Date().toISOString().slice(0, 10));

    if (problems.firstName || problems.birthDate) {
      showFieldErrors(formRef.current, () => setShown(problems));

      return;
    }

    setShown({});
    setBusy(true);
    setError(null);

    try {
      await onSave({
        firstName: d.firstName.trim(), lastName: d.lastName.trim(), kind: d.kind,
        birthDate: d.birthDate, careNotes: careVisible || careTouched ? d.careNotes.trim() : undefined, photoConsent: d.photoConsent,
      });
    } catch (err) {
      setError(errorText(err, "We couldn't save that person."));
      setBusy(false);
    }
  }

  return (
    <Form noValidate ref={formRef} onSubmit={submit} aria-label={heading} className="space-y-4 mt-4 rounded-lg border border-border p-4">
      <Heading className="font-semibold">{heading}</Heading>
      {identityLocked && <p className="text-sm text-text-muted">The account is waiting for consent and guardian confirmation. You can update care notes and photo settings here. Ask your church office for help with identity changes.</p>}
      <Field label="First name" error={shown.firstName}>
        <Input readOnly={identityLocked} value={d.firstName} onChange={(e) => set({ firstName: e.target.value })} autoComplete="off" />
      </Field>
      <Field label="Last name" optional hint="Optional. Leave empty if they go by one name.">
        <Input readOnly={identityLocked} value={d.lastName} onChange={(e) => set({ lastName: e.target.value })} autoComplete="off" />
      </Field>
      <Choices legend="Who are they?" hint="Children and teens are checked by an admin before the link is confirmed.">
        {(["child", "teen", "adult"] as const).map((k) => (
          <Radio disabled={identityLocked} key={k} name="household-kind" label={KIND_LABEL[k]} checked={d.kind === k} onChange={() => set({ kind: k })} />
        ))}
      </Choices>
      <Field label="Birth date" optional={d.kind === "adult"} hint={d.kind === "adult" ? "A birth date is needed before you can sign this adult up for an event." : undefined} error={shown.birthDate}>
        <Input readOnly={identityLocked} type="date" value={d.birthDate} onChange={(e) => set({ birthDate: e.target.value })} layout="w-48" />
      </Field>
      <Field label="Care notes" optional hint="Admins and confirmed guardians can read these notes. Their event’s leaders and hosts can read them from 24 hours before it starts until 24 hours after it ends, while they are going.">
        <Textarea value={d.careNotes} onChange={(e) => { setCareTouched(true); set({ careNotes: e.target.value }); }} rows={3} maxLength={5000} />
      </Field>
      <Checkbox label="OK to appear in church photos" checked={d.photoConsent} onChange={(e) => set({ photoConsent: e.target.checked })} />
      {error && <FormError>{error}</FormError>}
      <div className="flex gap-3">
        <Button variant="primary" type="submit" disabled={busy}>{busy ? "Saving…" : "Save"}</Button>
        <Button variant="secondary" onClick={onCancel} disabled={busy}>Cancel</Button>
      </div>
    </Form>
  );
}

function toDraft(p: HouseholdPerson): Draft {
  return { firstName: p.firstName, lastName: p.lastName, kind: p.kind, birthDate: p.birthDate ?? "", careNotes: p.careNotes ?? "", photoConsent: p.photoConsent };
}

// What an edit of a person writes, as PUT /me/household/people/:id stores it: the names and care notes trimmed, the
// birth date or none, the kind and the photo consent. Care notes this person can't read are no field of theirs, unless
// the save writes them (`writesHiddenCare`): then no read can show that the save is still the same decision, so a
// refusal is a conflict, never a send again over notes someone else wrote.
function personFields(p: HouseholdPerson | undefined, writesHiddenCare: boolean): PersonFields | null {
  if (!p || (writesHiddenCare && p.careNotes === undefined)) return null;

  return { firstName: p.firstName.trim(), lastName: p.lastName.trim(), kind: p.kind, birthDate: p.birthDate ?? "", careNotes: (p.careNotes ?? "").trim(), photoConsent: p.photoConsent };
}

type PersonFields = Pick<HouseholdPerson, "firstName" | "lastName" | "kind" | "photoConsent"> & { birthDate: string; careNotes: string };

// What a save of the household's details writes, as PUT /me/household stores it.
const householdFields = (h: Household | undefined) => (h ? { name: h.name.trim(), address: h.address.trim() } : null);

type HouseholdFields = NonNullable<ReturnType<typeof householdFields>>;

type HouseholdSaveRefusal = RevisionConflict | "unconfirmed";

async function confirmHouseholdWrite<T>(write: () => Promise<T>, onUnconfirmed: () => void): Promise<T | undefined> {
  try {
    return await write();
  } catch (error) {
    if (errorCode(error) !== "household_write_unconfirmed") throw error;
    onUnconfirmed();

    return undefined;
  }
}

// The household as one read shows it, for the form whose save was refused.
async function readHousehold(id: number) {
  const { household } = await Households.mine();
  const row = household?.id === id ? household : undefined;

  return { allowed: row !== undefined, revision: row?.revision, row };
}

async function readPerson(id: number) {
  const { household } = await Households.mine();
  const person = household?.people.find((p) => p.id === id);

  return { allowed: person !== undefined, revision: person?.revision, value: person };
}

// PersonEditor is one opening of a person's edit form. It mounts once per opening, so its save names the read the form
// opened with (DEC-174), and a refusal is the shared protocol's (useRevisionSave). On a conflict the form closes and
// the card reads the household from nothing (`onConflict`).
function PersonEditor({ person, level, onSaved, onConflict, onCancel }: {
  person: HouseholdPerson;
  level: "h2" | "h3";
  onSaved: (firstName: string) => Promise<void>;
  onConflict: (why: HouseholdSaveRefusal) => void;
  onCancel: () => void;
}) {
  const [opened] = useState(person);
  const writesHiddenCare = useRef(false);

  const save = useRevisionSave<HouseholdPerson | undefined, PersonFields>({
    opened,
    revision: opened.revision,
    read: () => readPerson(opened.id),
    fields: (p) => personFields(p, writesHiddenCare.current),
    answers: answerNames("person", opened.id),
    changedCode: "household_person_changed",
    // A person who became their own account is no longer in the card to edit.
    gone: (err) => lostAuthority(err) || errorCode(err) === "household_person_converted",
    onConflict,
  });

  async function submit(input: HouseholdPersonInput) {
    writesHiddenCare.current = opened.careNotes === undefined && input.careNotes !== undefined;

    const intended: HouseholdPerson = {
      ...opened,
      firstName: input.firstName,
      lastName: input.lastName ?? "",
      kind: input.kind,
      birthDate: input.birthDate || undefined,
      careNotes: input.careNotes ?? opened.careNotes,
      photoConsent: input.photoConsent ?? opened.photoConsent,
    };

    const sent = await confirmHouseholdWrite(
      () => save((revision) => Households.updatePerson(opened.id, input, revision), intended),
      () => onConflict("unconfirmed"),
    );

    if (sent === "refused") throw refusedAgain("household_person_changed");

    if (sent === "saved") await onSaved(input.firstName);
  }

  return (
    <PersonForm
      initial={toDraft(opened)}
      careVisible={opened.careNotes !== undefined}
      identityLocked={opened.conversion?.state === "awaiting_consent"}
      heading={`Edit ${opened.name}`}
      level={level}
      onCancel={onCancel}
      onSave={submit}
    />
  );
}

// HouseholdEditor is one opening of the household's name and address form, like PersonEditor. The card shows the stored
// household after a save, so it saves through useManagedSave, which hands back the row from the answer or the read.
function HouseholdEditor({ household, onSaved, onConflict, onCancel }: {
  household: Household;
  onSaved: (household: Household) => void;
  onConflict: (why: HouseholdSaveRefusal) => void;
  onCancel: () => void;
}) {
  const [opened] = useState(household);

  const save = useManagedSave<Household | undefined, Household | undefined, HouseholdFields, HouseholdResponse>({
    opened,
    revision: opened.revision,
    read: () => readHousehold(opened.id),
    value: (h) => h,
    stored: (answer) => answer.household,
    fields: householdFields,
    answers: answerNames("household", opened.id),
    changedCode: "household_changed",
    onConflict,
  });

  async function submit(name: string, address: string) {
    const intended = { ...opened, name, address };

    const result = await confirmHouseholdWrite(
      () => save((revision) => Households.update({ name, address }, revision), intended),
      () => onConflict("unconfirmed"),
    );

    if (result?.sent === "refused") throw refusedAgain("household_changed");

    if (result?.sent === "saved") onSaved(result.row ?? intended);
  }

  return <HouseholdForm household={opened} onSave={submit} onCancel={onCancel} />;
}

// The confirmation says what removing someone does (apiDeleteHouseholdPerson): their sign-ups for dates still to come
// are cancelled, and an open account invitation with them.
function removeQuestion(p: HouseholdPerson) {
  const invitation = p.conversion && p.conversion.state !== "awaiting_approval" ? " This also cancels their account invitation." : "";

  return `Remove ${p.name} from your household? Any upcoming sign-ups for them will be cancelled.${invitation}`;
}

// showHeading=false under a page that already has the h1 "Household" (the
// Profile section); the region keeps its "My household" name.
export function HouseholdCard({ showHeading = true }: { showHeading?: boolean }) {
  const load = useLoad(() => Households.mine(), []);
  const household = load.data?.household ?? null;
  const loadError = load.error ? errorText(load.error, "We couldn't load your household.") : null;
  const [error, setError] = useState<string | null>(null);
  const [editing, setEditing] = useState<number | "new" | null>(null);
  const [status, setStatus] = useState("");
  const [metadata, setMetadata] = useState(false);
  const [converting, setConverting] = useState<number | null>(null);
  const metadataInvoker = useRef<HTMLButtonElement | null>(null);
  // After a save or removal, where focus goes once the list re-renders (M1).
  const focusNext = useRef<number | "add" | "heading" | null>(null);
  const cardRef = useRef<HTMLElement>(null);
  const statusRef = useRef<HTMLParagraphElement>(null);
  const refusal = useRefusedSave(load);
  const [refusedPerson, setRefusedPerson] = useState<{ id: number; name: string } | null>(null);
  // The read the open details form was made from: a save's answer replaces it only if nothing read the household since.
  const openedEpoch = useRef(0);
  // A removal or an invitation action on its way.
  const [acting, setActing] = useState(false);
  // While a form is open or an action is on its way, nothing that opens a form or reads the household again is offered:
  // a save names the read its form opened with, and an answer must reach what it was made for.
  const locked = editing !== null || metadata || converting !== null || acting;
  const showStatus = status !== "" && loadError !== null && load.data === undefined;

  useEffect(() => {
    const target = focusNext.current;

    if (target === null || locked || load.loading) return;

    if (showStatus) {
      statusRef.current?.focus();

      return;
    }

    focusNext.current = null;
    const root = cardRef.current;

    const el =
      isNumber(target)
        ? root?.querySelector<HTMLElement>(`[data-person="${target}"] button`)
        : target === "add"
          ? root?.querySelector<HTMLElement>("[data-add]")
          : root?.querySelector<HTMLElement>("#household-heading");

    (el ?? root?.querySelector<HTMLElement>("#household-heading") ?? root)?.focus();
  }, [household, locked, load.loading, showStatus]);

  // The refused line says "this is the household as it is now" about the list below it: whatever changes that list
  // withdraws it.
  function opening() {
    refusal.clear();
    setRefusedPerson(null);
  }

  function refused(why: HouseholdSaveRefusal, person?: HouseholdPerson, what: "save" | "action" = "save") {
    focusNext.current = null;
    setEditing(null);
    setMetadata(false);
    setConverting(null);
    setStatus("");
    setRefusedPerson(person ? { id: person.id, name: person.name } : null);

    if (why === "unconfirmed") refusal.refused("gone", "unconfirmed");
    else refusal.refused(why, what);
  }

  async function remove(p: HouseholdPerson) {
    if (!confirm(removeQuestion(p))) return;
    opening();
    setError(null);
    setActing(true);

    try {
      await Households.removePerson(p.id);
      setStatus(`Removed ${p.name}.`);
      focusNext.current = "heading";
      await load.reload();
    } catch (e) {
      const code = errorCode(e);

      if (code === "household_person_converted" || code === "household_conversion_unavailable") refused("gone", p, "action");
      else if (code === "household_conversion_consent") refused("changed", p, "action");
      else setError(errorText(e, "We couldn't remove that person."));
    } finally {
      setActing(false);
    }
  }

  async function refreshInvitation(id: number, message: string) {
    setStatus(message);
    focusNext.current = id;

    // Remove's confirmation needs the invitation state from a read made after the write.
    if (await load.reload() === "failed") load.setData(undefined);
  }

  function retryRead() {
    opening();
    focusNext.current ??= "heading";
    void load.retry();
  }

  function openDetails(invoker: HTMLButtonElement) {
    opening();
    metadataInvoker.current = invoker;
    openedEpoch.current = load.epoch();
    setMetadata(true);
  }

  const people = household?.people ?? [];

  const startAdding = () => { opening(); setEditing("new"); };

  const addButton = (
    <span data-add>
      <Button variant={people.length ? "secondary" : "primary"} layout="mt-3" disabled={locked} onClick={startAdding}>Add a household member</Button>
    </span>
  );

  // Under the page's own h1 the household name is the first sub-heading.
  const Sub = showHeading ? "h3" : "h2";

  // The line names the person when the card lists several, or no longer lists them.
  const refusedName = refusedPerson && (people.length > 1 || !people.some((p) => p.id === refusedPerson.id))
    ? people.find((p) => p.id === refusedPerson.id)?.name ?? refusedPerson.name
    : undefined;

  if (load.loading) return <HouseholdSkeleton showHeading={showHeading} />;

  return (
    <section ref={cardRef} className={`${showHeading ? "mt-6 " : ""}focus:outline-none`} tabIndex={showHeading ? undefined : -1} aria-labelledby={showHeading ? "household-heading" : undefined} aria-label={showHeading ? undefined : "My household"}>
      {showHeading && <h2 id="household-heading" tabIndex={-1} className="text-xl font-bold mb-1 focus:outline-none">My household</h2>}
      <p className="text-sm mb-2"><a className="inline-flex min-h-11 items-center text-primary underline" href="/help/members">Accounts and household help</a></p>
      <p ref={statusRef} role="status" aria-live="polite" tabIndex={showStatus ? -1 : undefined} className={showStatus ? "mb-3 text-sm" : "sr-only"}>{status}</p>
      <RefusedSaveLine save={refusal} noun="household" canManage={load.data ? load.data.household !== null : undefined} about={refusedName} />
      {loadError && <ErrorNotice message={loadError} onRetry={locked ? undefined : retryRead} />}
      {!loadError && people.length === 0 && (household?.accounts.length ?? 0) <= 1 && editing !== "new" && !metadata && (
        <EmptyState
          level={2}
          title="No one in your household yet"
          body="Add your children, teens or other family members so you can sign them up for events together."
          action={<span data-add><Button variant="primary" disabled={locked} onClick={startAdding}>Add a household member</Button></span>}
        />
      )}
      {household && (
        <div className="my-3">
          <div className="flex flex-wrap items-center gap-3">
            <Sub className="font-semibold break-words min-w-0">{household.name}</Sub>
            <Button variant="secondary" disabled={locked} onClick={(e) => openDetails(e.currentTarget)}>Rename</Button>
          </div>
          <p className="text-sm text-text-muted whitespace-pre-wrap break-words mt-2">{household.address || "No household address yet."}</p>
          <Button variant="secondary" layout="mt-2" disabled={locked} onClick={(e) => openDetails(e.currentTarget)}>Edit address</Button>
          {metadata && <HouseholdEditor
            household={household}
            onCancel={() => { setMetadata(false); requestAnimationFrame(() => metadataInvoker.current?.focus()); }}
            onConflict={(why) => refused(why)}
            onSaved={(saved) => {
              if (load.epoch() === openedEpoch.current) load.setData({ household: saved });
              else void load.reload();
              setMetadata(false);
              setStatus("Household updated.");
              requestAnimationFrame(() => metadataInvoker.current?.focus());
            }}
          />}
        </div>
      )}
      {household && (
        <>
          {household.accounts.length > 1 && (
            <p className="text-sm text-text-muted mb-3">
              Accounts: {household.accounts.map((a: HouseholdAccount) => a.name).join(", ")}
            </p>
          )}
          <ul className="space-y-3">
            {people.map((p) => (
              <li key={p.id} data-person={p.id} className="rounded-lg border border-border p-4">
                {editing === p.id ? (
                  <PersonEditor
                    person={p}
                    level={Sub}
                    onCancel={() => { focusNext.current = p.id; setEditing(null); }}
                    onConflict={(why) => refused(why, p)}
                    onSaved={async (firstName) => {
                      setStatus(`${firstName} updated.`);
                      focusNext.current = p.id;
                      await load.reload();
                      setEditing(null);
                    }}
                  />
                ) : (
                  <div className="flex items-start gap-3 flex-wrap">
                    <div className="grow min-w-0">
                      <div className="font-semibold">{p.name}</div>
                      <div className="mt-1 flex flex-wrap gap-2">
                        <span className={STONE}>{KIND_LABEL[p.kind]}</span>
                        {LINK_LABEL[p.linkStatus] && (
                          <span className={p.linkStatus === "confirmed" ? VIOLET : STONE}>{LINK_LABEL[p.linkStatus]}</span>
                        )}
                      </div>
                      {p.birthDate && <div className="text-sm text-text-muted mt-1">Born {formatBirthDate(p.birthDate)}</div>}
                      {p.careNotes && <div className="text-sm text-text-muted whitespace-pre-wrap wrap-anywhere min-w-0">Care notes: {p.careNotes}</div>}
                    </div>
                    <div className="flex flex-wrap gap-2">
                      <Button variant="secondary" aria-label={`Edit ${p.name}`} disabled={locked} onClick={() => { opening(); setEditing(p.id); }}>Edit</Button>
                      <Button disabled={locked || p.conversion?.state === "awaiting_consent"} variant="danger-outline" aria-label={`Remove ${p.name}`} onClick={() => remove(p)}>Remove</Button>
                    </div>
                  </div>
                )}
                {p.conversion?.state === "awaiting_approval" && <div className="mt-3 text-sm text-text-muted"><p>Account waiting for an admin</p><p>Your household keeps managing their sign-ups until then.</p></div>}
                {p.conversion?.state === "awaiting_consent" && <div className="mt-3 text-sm text-text-muted"><p>Account waiting for consent and guardian confirmation</p><p>Your household keeps managing their sign-ups. Their name and birth date stay fixed while the church office finishes these checks. Care notes and photo settings can still be updated.</p></div>}
                {p.conversion && (p.conversion.state === "pending" || p.conversion.state === "expired") && <div className="mt-3 space-y-2">
                  <p className="text-sm text-text-muted break-words">{p.conversion.state === "expired" ? "Invitation expired" : "Invitation pending"}: {p.conversion.email}. Expires {formatWhen(p.conversion.expiresAt)}.</p>
                  <p className="text-sm text-text-muted">Changing their name, type or birth date cancels this invitation. Your household keeps managing their sign-ups.</p>
                  <ConversionActions person={p} disabled={locked} onBusy={(busy) => { if (busy) opening(); setActing(busy); }} onChanged={(message) => refreshInvitation(p.id, message)} />
                </div>}
                {!p.conversion && p.canConvert && converting !== p.id && <Button variant="secondary" layout="mt-3" disabled={locked} onClick={() => { opening(); setConverting(p.id); }}>Create their own account</Button>}
                {!p.conversion && converting === p.id && <ConversionForm person={p} onCancel={() => { focusNext.current = p.id; setConverting(null); }} onSent={async () => { await refreshInvitation(p.id, `Invitation sent for ${p.name}. Their sign-ups will move after approval and any required consent and guardian checks.`); setConverting(null); }} />}
              </li>
            ))}
          </ul>
        </>
      )}
      {error && <FormError layout="mt-2">{error}</FormError>}
      {!loadError && editing === "new" && (
        <PersonForm
          initial={emptyDraft}
          heading="Add a household member"
          level={Sub}
          onCancel={() => { focusNext.current = "add"; setEditing(null); }}
          onSave={async (input) => {
            const answer = await confirmHouseholdWrite(() => Households.addPerson(input), () => refused("unconfirmed"));

            if (!answer) return;

            setStatus(`Added ${input.firstName}.`);
            focusNext.current = answer.person.id;
            await load.reload();
            setEditing(null);
          }}
        />
      )}
      {!loadError && (people.length > 0 || (household?.accounts.length ?? 0) > 1) && editing !== "new" && addButton}
    </section>
  );
}

function ConversionForm({ person, onCancel, onSent }: { person: HouseholdPerson; onCancel: () => void; onSent: () => Promise<void> }) {
  const [email, setEmail] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const ref = useRef<HTMLFormElement>(null);
  useEffect(() => { ref.current?.querySelector("input")?.focus(); }, []);

  async function submit(e: React.FormEvent) {
    e.preventDefault(); setBusy(true); setError(null);

    try { await Households.convert(person.id, email.trim()); await onSent(); }
    catch (e) { showFieldErrors(ref.current, () => setError(errorText(e, "We couldn't send this invitation. Try again."))); }
    finally { setBusy(false); }
  }

  return <Form noValidate ref={ref} onSubmit={submit} className="mt-3 space-y-3" aria-label={`Create an account for ${person.name}`}>
    <p className="text-sm">Use their own email address. All past and upcoming sign-ups move after {person.kind === "teen" ? "an admin approves the account, consent is granted, and an admin confirms the new guardian link" : "an admin approves"}. Group memberships do not move.</p>
    <p className="text-sm">{person.kind === "teen" ? "Your household keeps managing their sign-ups until all checks are complete. The guardian link for the new account needs a fresh admin check." : "A church admin will approve their account before they can sign in."}</p>
    <Field label="Their email address" hint="Required. Use an email that has no account or open invitation." error={error ?? undefined}><Input type="email" value={email} onChange={(e) => setEmail(e.target.value)} required autoComplete="off" /></Field>
    <div className="flex flex-wrap gap-3"><Button variant="primary" layout="w-full sm:w-auto" type="submit" disabled={busy}>{busy ? "Sending…" : "Send account invitation"}</Button><Button variant="secondary" layout="w-full sm:w-auto" onClick={onCancel} disabled={busy}>Cancel</Button></div>
  </Form>;
}

function ConversionActions({ person, disabled, onBusy, onChanged }: { person: HouseholdPerson; disabled: boolean; onBusy: (busy: boolean) => void; onChanged: (message: string) => Promise<void> }) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function act(cancel: boolean) {
    onBusy(true);
    setBusy(true); setError(null);

    try {
      if (cancel) await Households.cancelConversion(person.id); else await Households.resendConversion(person.id);
      await onChanged(cancel ? "Invitation cancelled. Their sign-ups are unchanged." : "Invitation resent. The previous link can no longer be used.");
    } catch (e) { setError(errorText(e, "We couldn't update the invitation. Try again.")); }
    finally { setBusy(false); onBusy(false); }
  }

  return <div>
    <div className="flex flex-wrap gap-3"><Button variant="secondary" disabled={disabled || busy || !person.canConvert} onClick={() => void act(false)}>Resend invitation</Button><Button variant="danger-outline" disabled={disabled || busy} onClick={() => void act(true)}>Cancel invitation</Button></div>
    {error && <ErrorNotice message={error} />}
  </div>;
}

function HouseholdForm({ household, onSave, onCancel }: { household: Household; onSave: (name: string, address: string) => Promise<void>; onCancel: () => void }) {
  const [name, setName] = useState(household.name);
  const [address, setAddress] = useState(household.address);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [nameError, setNameError] = useState<string | undefined>();
  const root = useRef<HTMLFormElement>(null);
  useEffect(() => { root.current?.querySelector("input")?.focus(); }, []);

  async function save(e: React.FormEvent) {
    e.preventDefault();

    if (!name.trim() || [...name.trim()].length > 100) {
      showFieldErrors(root.current, () => setNameError("Enter a household name of 1 to 100 characters."));

      return;
    }

    setBusy(true); setError(null); setNameError(undefined);

    try { await onSave(name.trim(), address.trim()); }
    catch (e) { setError(errorText(e, "We couldn't save your household.")); setBusy(false); }
  }

  return <Form noValidate ref={root} aria-label="Edit household" onSubmit={save} className="mt-3 space-y-3 rounded-lg border border-border p-4">
    <Field label="Household name" hint="Required. Up to 100 characters." error={nameError}><Input value={name} onChange={(e) => setName(e.target.value)} required maxLength={100} /></Field>
    <Field label="Address" optional hint="Optional. Up to 500 characters."><Textarea value={address} onChange={(e) => setAddress(e.target.value)} rows={3} maxLength={500} /></Field>
    {error && <FormError>{error}</FormError>}
    <div className="flex flex-wrap gap-3"><Button variant="primary" type="submit" disabled={busy}>{busy ? "Saving…" : "Save household"}</Button><Button variant="secondary" onClick={onCancel} disabled={busy}>Cancel</Button></div>
  </Form>;
}
