import { publicName } from "../lib/can";
import { useEffect, useState } from "react";
import { AdminTeenConsents } from "../api/adminUsers";
import { useAuth } from "../auth";
import type { TeenConsent, TeenConsentChangedResponse, TeenConsentsResponse, User } from "../types";
import { errorText } from "../lib/errors";
import { formatWhen } from "../lib/when";
import { useAction, type Action } from "../lib/useAction";
import { useNewestRead } from "../lib/newestRead";
import { useFocusHome, useLockedFocus } from "../lib/focusHome";
import { answerNames, refusedAgain, useManagedSave, type RevisionConflict } from "../lib/useRevisionSave";
import { Button } from "./ui/Button";
import { Choices, Field, FormError, Radio, showFieldErrors, Textarea } from "./ui/Field";

const scopes = [
  { value: "account", label: "Account access" },
  { value: "sms", label: "SMS" },
  { value: "photos", label: "Photos" },
] satisfies { value: TeenConsent["scope"]; label: string }[];

// What a change can move, as the save compares it (DEC-176): how many changes the log holds, and who made the newest,
// how and with what evidence. A count tells a grant and a revoke since the read from no change, which a status alone
// cannot; the newest row tells this admin's own grant, whose answer was lost, from anyone else's.
function consentFields(log: TeenConsentsResponse) {
  const newest: TeenConsent | undefined = log.consents[0];

  return {
    changes: log.consents.length,
    newest: newest ? { scope: newest.scope, status: newest.status, grantedBy: newest.grantedBy, granterRole: newest.granterRole, evidenceText: newest.evidenceText } : null,
  };
}

const storedLog = (answer: TeenConsentChangedResponse): TeenConsentsResponse => ({
  consents: answer.consents, administrativelyDisabled: answer.administrativelyDisabled, revision: answer.teen.revision,
});

export default function TeenConsentPanel({ user, onUpdated, onClose, onRefused, action }: {
  user: User;
  onUpdated: () => void;
  onClose: () => void;
  // A grant someone else's change refused, or one this admin may no longer make: the host closes the panel and says so.
  onRefused: (why: RevisionConflict) => void;
  action?: Action;
}) {
  const [opened, setOpened] = useState<TeenConsentsResponse | null>(null);
  const [error, setError] = useState<string | null>(null);

  const load = useNewestRead(() => AdminTeenConsents.list(user.id), setOpened);

  useEffect(() => { load().catch((e) => setError(errorText(e, "We couldn't load consent history."))); }, [load, user.id]);

  if (!opened) return (
    <div className="mt-4 border-t border-border pt-4 space-y-4 whitespace-normal" aria-label={`Consent for ${publicName(user)}`}>
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h3 className="font-semibold">Teen consent</h3>
        <Button variant="ghost" onClick={onClose}>Close consent</Button>
      </div>
      {error ? <FormError>{error}</FormError> : <p role="status">Loading consent…</p>}
    </div>
  );

  return <ConsentEditor user={user} opened={opened} onUpdated={onUpdated} onClose={onClose} onRefused={onRefused} action={action} />;
}

// The panel once its first read landed: a change names the log it shows (DEC-176) and shows the log the change's answer
// says it left.
function ConsentEditor({ user, opened, onUpdated, onClose, onRefused, action: shared }: {
  user: User;
  opened: TeenConsentsResponse;
  onUpdated: () => void;
  onClose: () => void;
  onRefused: (why: RevisionConflict) => void;
  action?: Action;
}) {
  const { user: me } = useAuth();
  const [log, setLog] = useState(opened);
  const [scope, setScope] = useState<TeenConsent["scope"]>("account");
  const [evidence, setEvidence] = useState("");
  const [evidenceError, setEvidenceError] = useState<string | null>(null);
  const own = useAction();
  const action = shared ?? own;
  const busy = action.running;
  // A change disables what was pressed: focus waits on the panel, never the page.
  const home = useFocusHome<HTMLDivElement>();
  const ref = home.ref;

  useLockedFocus(home, busy);

  // The panel stays open after a change (`stays`), so each change names what the last one left. A revoke is never
  // refused as stale (DEC-176), but goes through the same save so the next grant names what it left.
  const save = useManagedSave({
    opened,
    revision: opened.revision,
    read: async () => {
      const row = await AdminTeenConsents.list(user.id);

      return { allowed: true, revision: row.revision, row };
    },
    value: (row: TeenConsentsResponse) => row,
    stored: storedLog,
    fields: consentFields,
    answers: answerNames("teen", user.id),
    changedCode: "consent_changed",
    stays: true,
    onConflict: (why) => onRefused(why),
  });

  async function change(status: TeenConsent["status"]) {
    if (status === "granted" && !evidence.trim()) {
      showFieldErrors(ref.current, () => setEvidenceError("Write how you received consent first."));

      return;
    }

    if (status === "revoked" && !confirm(scope === "account"
      ? `Revoke account consent for ${publicName(user)}? They will be signed out and can't sign in.`
      : `Revoke ${scope === "sms" ? "SMS" : "photo"} consent for ${publicName(user)}?`)) return;
    setEvidenceError(null);
    await action.run(async () => {
      const evidenceText = evidence.trim();
      const mine: TeenConsent = { id: 0, teenUserId: user.id, scope, status, grantedBy: me?.id ?? 0, granterRole: "admin", evidenceText, at: "" };

      const { sent, row } = await save(
        (revision) => AdminTeenConsents.change(user.id, { scope, status, evidenceText, revision }),
        { ...log, consents: [mine, ...log.consents] },
      );

      if (sent === "refused") throw refusedAgain("consent_changed");

      if (sent !== "saved") return false;

      if (row) setLog(row);
      setEvidence("");
      onUpdated();

      return `${scopes.find((item) => item.value === scope)?.label} consent ${status}.`;
    }, { fallback: "We couldn't save consent." });
  }

  const { state } = action;
  const history = log.consents;

  return (
    <div ref={ref} tabIndex={-1} className="mt-4 border-t border-border pt-4 space-y-4 whitespace-normal outline-none" aria-label={`Consent for ${publicName(user)}`}>
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h3 className="font-semibold">Teen consent</h3>
        <Button variant="ghost" disabled={busy} onClick={onClose}>Close consent</Button>
      </div>
      <ul className="space-y-1">
        {scopes.map((item) => <li key={item.value}>
          {item.label}: <strong>{history.find((row) => row.scope === item.value)?.status === "granted" ? "Granted" : "Not granted"}</strong>
        </li>)}
      </ul>
      {user.accountType === "teen" ? <>
        {log.administrativelyDisabled && <p className="text-text-muted">An admin has disabled this account. Granting consent keeps it disabled until an admin enables it.</p>}
        {!user.hasPassword && <p className="text-text-muted">They still need to set a password before they can sign in.</p>}
        {/* Locked while a change is on its way: a saved grant clears the evidence, so a later edit would be lost. */}
        <fieldset disabled={busy} className="min-w-0 space-y-4">
          <Choices legend="Consent scope">
            {scopes.map((item) => <Radio key={item.value} name={`consent-scope-${user.id}`} label={item.label} checked={scope === item.value} onChange={() => setScope(item.value)} />)}
          </Choices>
          <Field label="Consent evidence" hint="Required to grant. For example: paper form signed by a parent on October 22. Maximum 1,000 characters. You can add a reason when revoking." error={evidenceError ?? undefined}>
            <Textarea value={evidence} onChange={(event) => { setEvidence(event.target.value); setEvidenceError(null); }} maxLength={1000} rows={3} />
          </Field>
        </fieldset>
        <p className="text-sm text-text-muted">SMS consent also needs their own SMS opt-in before any texts can be sent. Photo consent applies to galleries.</p>
        <div className="flex flex-wrap gap-2">
          <Button variant="primary" onClick={() => void change("granted")} disabled={busy}>{busy ? "Saving…" : "Grant consent"}</Button>
          <Button variant="danger-outline" onClick={() => void change("revoked")} disabled={busy}>Revoke consent</Button>
        </div>
      </> : <p className="text-text-muted">This person is now an adult. Their teen consent history stays on file.</p>}
      <details>
        <summary className="cursor-pointer py-3">Consent history ({history.length})</summary>
        {history.length ? <ol className="space-y-3 mt-2">
          {history.map((row) => <li key={row.id} className="border-l-2 border-border pl-3 break-words">
            <div>{scopes.find((item) => item.value === row.scope)?.label}: {row.status}</div>
            <div className="text-xs text-text-muted">{formatWhen(row.at)} · {row.granterRole}</div>
            {row.evidenceText && <p className="mt-1 whitespace-pre-wrap">{row.evidenceText}</p>}
          </li>)}
        </ol> : <p>No consent has been recorded. Choose a scope and record evidence to grant consent.</p>}
      </details>
      {state.status === "failed" && <FormError>{state.text}</FormError>}
      <p role="status" className="text-success empty:hidden">{state.status === "done" ? state.text : ""}</p>
    </div>
  );
}
