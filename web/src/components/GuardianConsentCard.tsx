import { Form } from "./ui/Form";
import { lazy, Suspense, useEffect, useLayoutEffect, useRef, useState } from "react";
import { Guardians, type GuardianConsentScope } from "../api/guardians";
import type { GuardianTeen } from "../types";
import { errorText } from "../lib/errors";
import { useAction } from "../lib/useAction";
import { useNewestRead } from "../lib/newestRead";
import { useFocusHome, useKeepFocusHome, useLockedFocus } from "../lib/focusHome";
import { Button } from "./ui/Button";
import { Field, FormError, Input, Select } from "./ui/Field";
import { ErrorNotice } from "./ErrorNotice";

const GuardianContactGrants = lazy(() => import("./GuardianContactGrants").then((m) => ({ default: m.GuardianContactGrants })));

// A change that saved but whose refresh failed leaves `stale` set: the status shown may be out of date, so no
// consent action is offered until a refresh lands, and only that clears it.
function TeenConsentControls({ teen, onChanged }: { teen: GuardianTeen; onChanged: () => Promise<void> }) {
  const [showGrants, setShowGrants] = useState(false);
  const [scope, setScope] = useState<GuardianConsentScope>("account");
  const action = useAction();
  const { state } = action;
  const [stale, setStale] = useState(false);
  const refresh = useAction();
  const consentButton = useRef<HTMLButtonElement>(null);
  const refreshButton = useRef<HTMLButtonElement>(null);
  // A landed refresh removes its button: focus that was on it goes to the consent action it brings back.
  const refocus = useRef(false);
  const granted = scope === "account" ? teen.accountConsent : scope === "sms" ? teen.smsConsent : teen.photosConsent;
  // A change disables the button pressed and then replaces it (Revoke becomes Grant) or, when its refresh fails,
  // removes every action: focus waits on the teen's section and goes to what replaced it, never the page.
  const home = useFocusHome<HTMLDivElement>();
  useLockedFocus(home, action.running);
  useKeepFocusHome(home);

  useLayoutEffect(() => {
    if (stale || !refocus.current) return;
    refocus.current = false;
    consentButton.current?.focus({ preventScroll: true });
  }, [stale]);

  useLayoutEffect(() => {
    if (action.running || document.activeElement !== home.ref.current) return;
    (stale ? refreshButton : consentButton).current?.focus({ preventScroll: true });
  }, [action.running, stale, home]);

  async function change(status: "granted" | "revoked") {
    if (status === "revoked" && !confirm(scope === "account" ? `Revoke account consent for ${teen.teenName}? They will be signed out and unable to use Fold until consent is granted again.` : `Revoke ${scope} consent for ${teen.teenName}?`)) return;
    await action.run(async () => {
      await Guardians.change(teen.linkId, scope, status);
      setShowGrants(false);

      return `${scope === "sms" ? "SMS" : scope === "photos" ? "Photo" : "Account"} consent ${status}.`;
    }, {
      followUp: async () => {
        try {
          await onChanged();
        } catch (cause) {
          refresh.reset();
          setStale(true);
          throw cause;
        }
      },
    });
  }

  function reload() {
    void refresh.run(async () => {
      await onChanged();
      refocus.current = document.activeElement === refreshButton.current;
      setStale(false);
    });
  }

  function email() {
    void action.run(async () => (await Guardians.email(teen.linkId)).message);
  }

  return <div ref={home.ref} tabIndex={-1} className="border-t border-border pt-4 mt-4 outline-none">
    <h3 className="font-semibold">{teen.teenName}</h3>
    <Button variant="link" aria-expanded={showGrants} onClick={() => setShowGrants(!showGrants)}>{showGrants ? "Close contact access" : "View active contact access"}</Button>
    {showGrants && <Suspense fallback={<p role="status">Loading contact access…</p>}><GuardianContactGrants teenUserId={teen.teenUserId} /></Suspense>}
    {!stale && <>
      <Field id={`guardian-scope-${teen.linkId}`} label="Consent" hint="Account consent lets your teen use Fold. SMS consent also needs their own opt-in before any texts can be sent.">
        <Select value={scope} onChange={(e) => {
          const value = e.target.value;

          if (value === "account" || value === "sms" || value === "photos") {
            setScope(value);

            if (!action.running) action.reset();
          }
        }}>
          <option value="account">Account</option><option value="sms">SMS</option><option value="photos">Photos</option>
        </Select>
      </Field>
      <p className="text-sm text-text-muted mt-2">{granted ? "Consent granted" : "Consent not granted"}</p>
    </>}
    {state.status === "failed" && <FormError layout="mt-2">{state.text}</FormError>}
    {state.status === "done" && state.text && <p role="status" className="text-sm mt-2">{state.text}</p>}
    {stale ? <>
      <FormError layout="mt-2">{refresh.state.status === "failed" ? refresh.state.text : `Your change is saved, but we couldn't load ${teen.teenName}'s latest consent. Refresh to see it before you change anything else.`}</FormError>
      <div className="flex flex-wrap gap-3 mt-3">
        <Button ref={refreshButton} variant="secondary" layout="w-full sm:w-auto" onClick={reload}>{refresh.running ? "Refreshing…" : "Refresh consent"}</Button>
      </div>
    </> : <div className="flex flex-wrap gap-3 mt-3">
      {granted ? <Button ref={consentButton} key="revoke" variant="danger-outline" layout="w-full sm:w-auto" disabled={action.running} onClick={() => change("revoked")}>Revoke consent</Button> : <Button ref={consentButton} key="grant" variant="primary" layout="w-full sm:w-auto" disabled={action.running} onClick={() => change("granted")}>Grant consent</Button>}
      {scope === "account" && <Button variant="secondary" layout="w-full sm:w-auto" disabled={action.running} onClick={email}>Email me a consent link</Button>}
    </div>}
  </div>;
}

export function GuardianConsentCard() {
  const [teens, setTeens] = useState<GuardianTeen[]>([]);
  const [loaded, setLoaded] = useState(false);
  const [email, setEmail] = useState("");
  const [loadError, setLoadError] = useState("");
  const claimAction = useAction();
  const claimed = claimAction.state;

  const load = useNewestRead(Guardians.teens, (answer) => {
    setTeens(answer.teens);
    setLoaded(true);
  });

  useEffect(() => { load().catch((cause: unknown) => setLoadError(errorText(cause))); }, [load]);

  function retry() { setLoadError(""); void load().catch((cause: unknown) => setLoadError(errorText(cause))); }

  function claim(e: React.FormEvent) {
    e.preventDefault();
    void claimAction.run(async () => {
      const { message } = await Guardians.claim(email);
      setEmail("");

      return message;
    });
  }

  return <section className="bg-surface rounded-lg border border-border p-6 mb-6" aria-label="Teen consent">
    <h2 className="text-xl font-bold">Teen consent</h2>
    <p className="text-sm text-text-muted mt-2">You can manage consent after an admin confirms your guardian link.</p>
    {!loaded && loadError && <ErrorNotice message={loadError} onRetry={retry} />}
    {claimed.status === "failed" && <FormError layout="mt-3">{claimed.text}</FormError>}
    {!loaded && !loadError && <p className="text-sm mt-3">Loading your guardian links…</p>}
    {teens.map((teen) => <TeenConsentControls key={teen.linkId} teen={teen} onChanged={load} />)}
    <Form noValidate onSubmit={claim} className="border-t border-border pt-4 mt-4 space-y-3">
      <h3 className="font-semibold">Request a guardian link</h3>
      <Field id="guardian-claim-email" label="Teen's email" hint="Use the email on their Fold account. An admin checks every request before you can give consent.">
        <Input type="email" value={email} onChange={(e) => setEmail(e.target.value)} maxLength={254} required />
      </Field>
      <Button variant="secondary" layout="w-full sm:w-auto" type="submit" disabled={claimAction.running}>{claimAction.running ? "Sending…" : "Request guardian link"}</Button>
      {claimed.status === "done" && claimed.text && <p role="status" className="text-sm">{claimed.text}</p>}
    </Form>
  </section>;
}
