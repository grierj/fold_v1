import { Form } from "./ui/Form";
import { lazy, Suspense, useEffect, useLayoutEffect, useRef, useState } from "react";
import { Guardians, type GuardianConsentScope } from "../api/guardians";
import type { GuardianConsentChangedResponse, GuardianTeen } from "../types";
import { errorText } from "../lib/errors";
import { useAction } from "../lib/useAction";
import { useNewestRead } from "../lib/newestRead";
import { RefusedSaveLine, useRefusedSave } from "../lib/refusedSave";
import { answerNames, refusedAgain, useManagedSave } from "../lib/useRevisionSave";
import { useFocusHome, useKeepFocusHome, useLockedFocus } from "../lib/focusHome";
import { Button } from "./ui/Button";
import { Field, FormError, Input, Select } from "./ui/Field";
import { ErrorNotice } from "./ErrorNotice";

const GuardianContactGrants = lazy(() => import("./GuardianContactGrants").then((m) => ({ default: m.GuardianContactGrants })));

// What a change can move, as the save compares it (DEC-176): each scope's status, how many changes the teen has, and
// whether this guardian made the newest. A count tells a grant and a revoke since the read from no change, which the
// statuses alone cannot; the last flag tells this guardian's own grant, whose answer was lost, from anyone else's.
const consentFields = (row: GuardianTeen) => ({
  account: row.accountConsent, sms: row.smsConsent, photos: row.photosConsent, changes: row.changes, yours: row.lastChangeYours,
});

const scopeKey = { account: "accountConsent", sms: "smsConsent", photos: "photosConsent" } as const;

// A teen's controls stay on screen after a change (`stays`): each change names what the last one left, and a change's
// answer (or the read a refusal made) is the row the host shows. A revoke is never refused as stale (DEC-176), but goes
// through the same save so the next grant names what it left.
function TeenConsentControls({ teen, onStored, onGone }: { teen: GuardianTeen; onStored: (row: GuardianTeen) => void; onGone: () => void }) {
  const [showGrants, setShowGrants] = useState(false);
  const [scope, setScope] = useState<GuardianConsentScope>("account");
  const action = useAction();
  const { state } = action;
  const refusal = useRefusedSave();
  const consentButton = useRef<HTMLButtonElement>(null);
  const granted = teen[scopeKey[scope]];
  // A change disables the button pressed and then replaces it (Revoke becomes Grant): focus waits on the teen's
  // section and goes to what replaced it, never the page.
  const home = useFocusHome<HTMLDivElement>();
  useLockedFocus(home, action.running);
  useKeepFocusHome(home);

  useLayoutEffect(() => {
    if (action.running || refusal.why || document.activeElement !== home.ref.current) return;
    consentButton.current?.focus({ preventScroll: true });
  }, [action.running, refusal.why, home]);

  const save = useManagedSave({
    opened: teen,
    revision: teen.revision,
    read: async () => {
      const row = (await Guardians.teens()).teens.find((item) => item.linkId === teen.linkId);

      return { allowed: !!row, revision: row?.revision, row: row ?? teen };
    },
    value: (row: GuardianTeen) => row,
    stored: (answer: GuardianConsentChangedResponse) => answer.guardianTeen,
    fields: consentFields,
    answers: answerNames("teen", teen.teenUserId),
    changedCode: "consent_changed",
    stays: true,
    // A refused change keeps today's words for why (a link no longer available, a sign-in that changed); only a read
    // that no longer finds the link hands the card back to its host.
    gone: () => false,
    onConflict: (why, row) => {
      if (why === "gone") {
        onGone();

        return;
      }

      // What the read found may have ended contact access (an account revoke), so the list closes as after a change
      // of this card's own, and opening it again reads the grants as they are now.
      if (row) onStored(row);
      setShowGrants(false);
      refusal.refused(why);
    },
  });

  async function change(status: "granted" | "revoked") {
    if (status === "revoked" && !confirm(scope === "account" ? `Revoke account consent for ${teen.teenName}? They will be signed out and unable to use Fold until consent is granted again.` : `Revoke ${scope} consent for ${teen.teenName}?`)) return;
    refusal.clear();
    await action.run(async () => {
      const { sent, row } = await save(
        (revision) => Guardians.change(teen.linkId, scope, status, revision),
        { ...teen, [scopeKey[scope]]: status === "granted", changes: teen.changes + 1, lastChangeYours: true },
      );

      if (sent === "refused") throw refusedAgain("consent_changed");

      if (sent !== "saved") return false;

      if (row) onStored(row);
      setShowGrants(false);

      return `${scope === "sms" ? "SMS" : scope === "photos" ? "Photo" : "Account"} consent ${status}.`;
    });
  }

  function email() {
    refusal.clear();
    void action.run(async () => (await Guardians.email(teen.linkId)).message);
  }

  return <div ref={home.ref} tabIndex={-1} className="border-t border-border pt-4 mt-4 outline-none">
    <h3 className="font-semibold">{teen.teenName}</h3>
    <Button variant="link" aria-expanded={showGrants} onClick={() => setShowGrants(!showGrants)}>{showGrants ? "Close contact access" : "View active contact access"}</Button>
    {showGrants && <Suspense fallback={<p role="status">Loading contact access…</p>}><GuardianContactGrants teenUserId={teen.teenUserId} /></Suspense>}
    <Field id={`guardian-scope-${teen.linkId}`} label="Consent" hint="Account consent lets your teen use Fold. SMS consent also needs their own opt-in before any texts can be sent.">
      <Select value={scope} onChange={(e) => {
        const value = e.target.value;

        if (value === "account" || value === "sms" || value === "photos") {
          setScope(value);

          if (!action.running) {
            action.reset();
            refusal.clear();
          }
        }
      }}>
        <option value="account">Account</option><option value="sms">SMS</option><option value="photos">Photos</option>
      </Select>
    </Field>
    <p className="text-sm text-text-muted mt-2">{granted ? "Consent granted" : "Consent not granted"}</p>
    <div className="mt-2 empty:hidden"><RefusedSaveLine save={refusal} noun="consent" choosing={`${teen.teenName}'s consent`} /></div>
    {state.status === "failed" && <FormError layout="mt-2">{state.text}</FormError>}
    {state.status === "done" && state.text && <p role="status" className="text-sm mt-2">{state.text}</p>}
    <div className="flex flex-wrap gap-3 mt-3">
      {granted ? <Button ref={consentButton} key="revoke" variant="danger-outline" layout="w-full sm:w-auto" disabled={action.running} onClick={() => change("revoked")}>Revoke consent</Button> : <Button ref={consentButton} key="grant" variant="primary" layout="w-full sm:w-auto" disabled={action.running} onClick={() => change("granted")}>Grant consent</Button>}
      {scope === "account" && <Button variant="secondary" layout="w-full sm:w-auto" disabled={action.running} onClick={email}>Email me a consent link</Button>}
    </div>
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

  // A teen's change this guardian may no longer make: the card reads the links again and says so.
  const gone = useRefusedSave();

  function stored(row: GuardianTeen) {
    setTeens((all) => all.map((item) => (item.linkId === row.linkId ? row : item)));
  }

  function lost() {
    void load().catch((cause: unknown) => setLoadError(errorText(cause))).finally(() => gone.refused("gone"));
  }

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
    <div className="mt-3 empty:hidden"><RefusedSaveLine save={gone} noun="consent" /></div>
    {teens.map((teen) => <TeenConsentControls key={teen.linkId} teen={teen} onStored={stored} onGone={lost} />)}
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
