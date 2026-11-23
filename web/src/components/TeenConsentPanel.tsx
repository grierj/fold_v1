import { publicName } from "../lib/can";
import { useEffect, useLayoutEffect, useRef, useState } from "react";
import { AdminTeenConsents } from "../api/adminUsers";
import type { TeenConsent, User } from "../types";
import { errorText } from "../lib/errors";
import { formatWhen } from "../lib/when";
import { useAction } from "../lib/useAction";
import { useNewestRead } from "../lib/newestRead";
import { useFocusHome, useLockedFocus } from "../lib/focusHome";
import { Button } from "./ui/Button";
import { Choices, Field, FormError, Radio, showFieldErrors, Textarea } from "./ui/Field";

const scopes = [
  { value: "account", label: "Account access" },
  { value: "sms", label: "SMS" },
  { value: "photos", label: "Photos" },
] satisfies { value: TeenConsent["scope"]; label: string }[];

export default function TeenConsentPanel({ user, onUpdated, onClose }: { user: User; onUpdated: () => void; onClose: () => void }) {
  const [history, setHistory] = useState<TeenConsent[]>([]);
  const [suspended, setSuspended] = useState(false);
  const [loaded, setLoaded] = useState(false);
  const [scope, setScope] = useState<TeenConsent["scope"]>("account");
  const [evidence, setEvidence] = useState("");
  const [evidenceError, setEvidenceError] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const action = useAction();
  const busy = action.running;
  // A change disables what was pressed and, when its refresh fails, removes every action: focus waits on the
  // panel and goes to Refresh consent (or back to what was pressed), never the page.
  const home = useFocusHome<HTMLDivElement>();
  const ref = home.ref;
  // A change that saved but whose refresh failed: what is shown may be out of date, so no consent action is
  // offered until a refresh lands, and only that clears it.
  const [stale, setStale] = useState(false);
  const refresh = useAction();
  const grantButton = useRef<HTMLButtonElement>(null);
  const closeButton = useRef<HTMLButtonElement>(null);
  const refreshButton = useRef<HTMLButtonElement>(null);
  // A landed refresh removes its button: focus that was on it goes to Grant consent (Close consent for an adult).
  const refocus = useRef(false);

  const load = useNewestRead(() => AdminTeenConsents.list(user.id), (result) => {
    setHistory(result.consents);
    setSuspended(result.administrativelyDisabled);
    setLoaded(true);
  });

  useEffect(() => { load().catch((e) => setError(errorText(e, "We couldn't load consent history."))); }, [load, user.id]);

  useLockedFocus(home, busy);

  useLayoutEffect(() => {
    if (stale || !refocus.current) return;
    refocus.current = false;
    (grantButton.current ?? closeButton.current)?.focus({ preventScroll: true });
  }, [stale]);

  useLayoutEffect(() => {
    if (busy || !stale || document.activeElement !== home.ref.current) return;
    refreshButton.current?.focus({ preventScroll: true });
  }, [busy, stale, home]);

  function reload() {
    void refresh.run(async () => {
      await load();
      refocus.current = document.activeElement === refreshButton.current;
      setStale(false);
    });
  }

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
      await AdminTeenConsents.change(user.id, { scope, status, evidenceText: evidence.trim() });
      setEvidence("");
      onUpdated();

      return `${scopes.find((item) => item.value === scope)?.label} consent ${status}.`;
    }, {
      fallback: "We couldn't save consent.",
      followUp: async () => {
        try {
          await load();
        } catch (cause) {
          refresh.reset();
          setStale(true);
          throw cause;
        }
      },
    });
  }

  const { state } = action;

  return (
    <div ref={ref} tabIndex={-1} className="mt-4 border-t border-border pt-4 space-y-4 whitespace-normal outline-none" aria-label={`Consent for ${publicName(user)}`}>
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h3 className="font-semibold">Teen consent</h3>
        <Button ref={closeButton} variant="ghost" disabled={busy} onClick={onClose}>Close consent</Button>
      </div>
      {!loaded && !error && <p role="status">Loading consent…</p>}
      {loaded && !stale && <>
        <ul className="space-y-1">
          {scopes.map((item) => <li key={item.value}>
            {item.label}: <strong>{history.find((row) => row.scope === item.value)?.status === "granted" ? "Granted" : "Not granted"}</strong>
          </li>)}
        </ul>
        {user.accountType === "teen" ? <>
          {suspended && <p className="text-text-muted">An admin has disabled this account. Granting consent keeps it disabled until an admin enables it.</p>}
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
            <Button ref={grantButton} variant="primary" onClick={() => void change("granted")} disabled={busy}>{busy ? "Saving…" : "Grant consent"}</Button>
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
      </>}
      {error && <FormError>{error}</FormError>}
      {state.status === "failed" && <FormError>{state.text}</FormError>}
      <p role="status" className="text-success empty:hidden">{state.status === "done" ? state.text : ""}</p>
      {stale && <>
        <FormError>{refresh.state.status === "failed" ? refresh.state.text : `Your change is saved, but we couldn't load ${publicName(user)}'s latest consent. Refresh to see it before you change anything else.`}</FormError>
        <Button ref={refreshButton} variant="secondary" onClick={reload}>{refresh.running ? "Refreshing…" : "Refresh consent"}</Button>
      </>}
    </div>
  );
}
