import { stubService } from "../test/service";
import { renderWithAuth } from "../test/auth";
import { StrictMode } from "react";
import { act, render as rtlRender, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, expect, it, vi } from "vitest";
import { AdminTeenConsents } from "../api/adminUsers";
import { ApiError } from "../api/core";
import type { TeenConsent, TeenConsentChangedResponse, TeenConsentsResponse, User } from "../types";
import TeenConsentPanel from "./TeenConsentPanel";

stubService(AdminTeenConsents, () => ({ list: vi.fn(), change: vi.fn() }));

const render = renderWithAuth(() => ({ user: { id: 1, role: "admin" } }));

const teen: User = {
  id: 7, name: "Casey Teen", displayName: "Casey", email: "casey@example.com", phone: "",
  avatarUrl: "", avatarFocalX: 50, avatarFocalY: 50, role: "member", status: "pending_consent",
  hasPassword: true, accountType: "teen", hideContactFromLeaders: true, birthDate: "2011-01-01", timezone: "",
};

const consent: TeenConsent = {
  id: 11, teenUserId: teen.id, scope: "account", status: "granted", grantedBy: 1, granterRole: "admin",
  evidenceText: "Signed parent form", at: "2026-10-22T12:00:00Z",
};

const log = (consents: TeenConsent[], revision = "r1"): TeenConsentsResponse => ({ consents, administrativelyDisabled: false, revision });

const answer = (consents: TeenConsent[], revision = "r2"): TeenConsentChangedResponse => ({
  teen: { id: teen.id, revision }, consents, administrativelyDisabled: false,
});

const changed = () => new ApiError(409, "private detail", { code: "consent_changed" });

beforeEach(() => {
  vi.mocked(AdminTeenConsents.list).mockReset().mockResolvedValue(log([]));
  vi.mocked(AdminTeenConsents.change).mockReset().mockImplementation(async (_, data) => answer([{ ...consent, id: 20, ...data }]));
});

const panel = (props: Partial<Parameters<typeof TeenConsentPanel>[0]> = {}) =>
  <TeenConsentPanel user={teen} onUpdated={vi.fn()} onClose={vi.fn()} onRefused={vi.fn()} {...props} />;

it("requires and focuses evidence before an admin grant", async () => {
  render(panel());
  await userEvent.click(await screen.findByRole("button", { name: "Grant consent" }));
  expect(screen.getByLabelText("Consent evidence")).toHaveFocus();
  expect(screen.getByText("Write how you received consent first.")).toBeInTheDocument();
  expect(AdminTeenConsents.change).not.toHaveBeenCalled();
});

it("names the read it was made from and shows the history its answer stored, with no second read", async () => {
  const updated = vi.fn();
  render(panel({ onUpdated: updated }));
  await userEvent.click(await screen.findByRole("radio", { name: "SMS" }));
  await userEvent.type(screen.getByLabelText("Consent evidence"), "Parent signed SMS form");
  await userEvent.click(screen.getByRole("button", { name: "Grant consent" }));
  expect(AdminTeenConsents.change).toHaveBeenCalledWith(7, { scope: "sms", status: "granted", evidenceText: "Parent signed SMS form", revision: "r1" });
  expect(await screen.findByRole("status")).toHaveTextContent("SMS consent granted.");
  expect(updated).toHaveBeenCalledOnce();
  expect(AdminTeenConsents.list).toHaveBeenCalledOnce();
  await userEvent.click(screen.getByText("Consent history (1)"));
  expect(screen.getByText("Parent signed SMS form")).toBeVisible();
});

it("a revoke and then a grant on the open panel each name what the last change left", async () => {
  vi.spyOn(window, "confirm").mockReturnValue(true);
  vi.mocked(AdminTeenConsents.list).mockResolvedValue(log([consent]));
  vi.mocked(AdminTeenConsents.change)
    .mockResolvedValueOnce(answer([{ ...consent, id: 12, status: "revoked", evidenceText: "" }, consent], "r2"))
    .mockResolvedValueOnce(answer([{ ...consent, id: 13 }, { ...consent, id: 12, status: "revoked", evidenceText: "" }, consent], "r3"));
  render(panel());
  await userEvent.click(await screen.findByRole("button", { name: "Revoke consent" }));
  expect(await screen.findByRole("status")).toHaveTextContent("Account access consent revoked.");
  expect(screen.queryByText("Granted", { selector: "strong", exact: true })).not.toBeInTheDocument();
  await userEvent.type(screen.getByLabelText("Consent evidence"), "Signed parent form");
  await userEvent.click(screen.getByRole("button", { name: "Grant consent" }));
  expect(await screen.findByRole("status")).toHaveTextContent("Account access consent granted.");
  expect(vi.mocked(AdminTeenConsents.change).mock.calls.map(([, data]) => data.revision)).toEqual(["r1", "r2"]);
  expect(screen.getByText("Consent history (3)")).toBeInTheDocument();
});

it("a grant refused because someone else changed the consent writes nothing more and hands the conflict to its host", async () => {
  const refused = vi.fn();
  const updated = vi.fn();
  vi.mocked(AdminTeenConsents.change).mockRejectedValue(changed());
  render(panel({ onRefused: refused, onUpdated: updated }));
  await userEvent.type(await screen.findByLabelText("Consent evidence"), "Signed parent form");
  vi.mocked(AdminTeenConsents.list).mockResolvedValue(log([{ ...consent, grantedBy: 9, granterRole: "guardian", status: "revoked", evidenceText: "" }], "r2"));
  await userEvent.click(screen.getByRole("button", { name: "Grant consent" }));
  await waitFor(() => expect(refused).toHaveBeenCalledWith("changed"));
  expect(AdminTeenConsents.change).toHaveBeenCalledOnce();
  expect(updated).not.toHaveBeenCalled();
  expect(screen.queryByRole("alert")).not.toBeInTheDocument();
});

it("a grant and a revoke by someone else since the read are a conflict, never a quiet resend of the grant", async () => {
  const refused = vi.fn();
  vi.mocked(AdminTeenConsents.change).mockRejectedValue(changed());
  render(panel({ onRefused: refused }));
  await userEvent.type(await screen.findByLabelText("Consent evidence"), "Signed parent form");
  // The account looks as it did (not granted), but two changes landed since the panel read it.
  vi.mocked(AdminTeenConsents.list).mockResolvedValue(log([
    { ...consent, id: 31, status: "revoked", grantedBy: 9, granterRole: "guardian", evidenceText: "" },
    { ...consent, id: 30, grantedBy: 9, granterRole: "guardian", evidenceText: "" },
  ], "r3"));
  await userEvent.click(screen.getByRole("button", { name: "Grant consent" }));
  await waitFor(() => expect(refused).toHaveBeenCalledWith("changed"));
  expect(AdminTeenConsents.change).toHaveBeenCalledOnce();
});

it("a grant whose answer was lost and is pressed again is saved, not refused", async () => {
  const refused = vi.fn();
  vi.mocked(AdminTeenConsents.change).mockRejectedValueOnce(new ApiError(502, "bad gateway", {})).mockRejectedValueOnce(changed());
  render(panel({ onRefused: refused }));
  await userEvent.type(await screen.findByLabelText("Consent evidence"), "Signed parent form");
  await userEvent.click(screen.getByRole("button", { name: "Grant consent" }));
  expect(await screen.findByRole("alert")).toBeInTheDocument();
  expect(screen.getByLabelText("Consent evidence")).toHaveValue("Signed parent form");
  vi.mocked(AdminTeenConsents.list).mockResolvedValue(log([{ ...consent, id: 40 }], "r2"));
  await userEvent.click(screen.getByRole("button", { name: "Grant consent" }));
  expect(await screen.findByRole("status")).toHaveTextContent("Account access consent granted.");
  expect(refused).not.toHaveBeenCalled();
  expect(vi.mocked(AdminTeenConsents.change).mock.calls.map(([, data]) => data.revision)).toEqual(["r1", "r1"]);
  expect(screen.getByText("Granted", { selector: "strong", exact: true })).toBeInTheDocument();
  expect(screen.getByLabelText("Consent evidence")).toHaveValue("");
});

it("a lost grant whose read shows another admin's grant is a conflict, not this admin's save", async () => {
  const refused = vi.fn();
  vi.mocked(AdminTeenConsents.change).mockRejectedValueOnce(new ApiError(502, "bad gateway", {})).mockRejectedValueOnce(changed());
  render(panel({ onRefused: refused }));
  await userEvent.type(await screen.findByLabelText("Consent evidence"), "Signed parent form");
  await userEvent.click(screen.getByRole("button", { name: "Grant consent" }));
  await screen.findByRole("alert");
  vi.mocked(AdminTeenConsents.list).mockResolvedValue(log([{ ...consent, id: 40, grantedBy: 2, evidenceText: "Phone call" }], "r2"));
  await userEvent.click(screen.getByRole("button", { name: "Grant consent" }));
  await waitFor(() => expect(refused).toHaveBeenCalledWith("changed"));
});

it("confirms account revocation and preserves evidence after an API error", async () => {
  const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
  const updated = vi.fn();
  render(panel({ onUpdated: updated }));
  await userEvent.click(await screen.findByRole("button", { name: "Revoke consent" }));
  expect(confirm).toHaveBeenCalledWith(expect.stringContaining("They will be signed out"));
  expect(AdminTeenConsents.change).not.toHaveBeenCalled();
  confirm.mockReturnValue(true);
  vi.mocked(AdminTeenConsents.change).mockRejectedValue(new ApiError(409, "private detail", { code: "consent_teen_only" }));
  await userEvent.type(screen.getByLabelText("Consent evidence"), "Parent withdrew consent");
  await userEvent.click(screen.getByRole("button", { name: "Revoke consent" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("This account is no longer a teen account. Refresh the user list.");
  expect(screen.getByLabelText("Consent evidence")).toHaveValue("Parent withdrew consent");
  expect(updated).not.toHaveBeenCalled();
  confirm.mockRestore();
});

it("explains independent suspension and keeps adult history read-only", async () => {
  vi.mocked(AdminTeenConsents.list).mockResolvedValue({ ...log([consent]), administrativelyDisabled: true });
  const view = render(panel());
  expect(await screen.findByText(/Granting consent keeps it disabled/)).toBeInTheDocument();
  view.rerender(panel({ user: { ...teen, accountType: "adult", hideContactFromLeaders: false } }));
  await waitFor(() => expect(screen.queryByRole("button", { name: "Grant consent" })).not.toBeInTheDocument());
  expect(screen.getByText(/Their teen consent history stays on file/)).toBeVisible();
  await userEvent.click(screen.getByText("Consent history (1)"));
  expect(screen.getByText("Signed parent form")).toBeVisible();
});

function held<T>() {
  let resolve!: (value: T) => void;
  let reject!: (cause: unknown) => void;
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });

  return { promise, resolve, reject };
}

it("an older read that lands after a newer one never replaces its answer", async () => {
  const first = held<TeenConsentsResponse>();
  const second = held<TeenConsentsResponse>();
  vi.mocked(AdminTeenConsents.list).mockReturnValueOnce(first.promise).mockReturnValueOnce(second.promise);
  // StrictMode mounts twice, so two reads go out; the first one asked answers last. The read needs no signed-in admin.
  rtlRender(<StrictMode>{panel()}</StrictMode>);
  await waitFor(() => expect(AdminTeenConsents.list).toHaveBeenCalledTimes(2));
  await act(async () => { second.resolve(log([consent])); await second.promise; });
  expect(await screen.findByText("Granted")).toBeInTheDocument();
  await act(async () => { first.resolve(log([])); await first.promise; });
  expect(screen.getByText("Granted")).toBeInTheDocument();
  expect(screen.queryByText("Consent history (0)")).not.toBeInTheDocument();
});

it("gives focus back to the pressed button once a change lands", async () => {
  render(panel());
  await userEvent.type(await screen.findByLabelText("Consent evidence"), "Signed parent form");
  const grant = screen.getByRole("button", { name: "Grant consent" });
  grant.focus();
  await userEvent.keyboard("{Enter}");
  expect(await screen.findByRole("status")).toHaveTextContent("Account access consent granted.");
  await waitFor(() => expect(screen.getByRole("button", { name: "Grant consent" })).toHaveFocus());
});

it("leaves focus where the person moved it while the change was on its way", async () => {
  const change = held<TeenConsentChangedResponse>();
  vi.mocked(AdminTeenConsents.change).mockReturnValueOnce(change.promise);
  render(<><button>Elsewhere</button>{panel()}</>);
  await userEvent.type(await screen.findByLabelText("Consent evidence"), "Signed parent form");
  await userEvent.click(screen.getByRole("button", { name: "Grant consent" }));
  const elsewhere = screen.getByRole("button", { name: "Elsewhere" });
  elsewhere.focus();
  await act(async () => { change.resolve(answer([consent])); await change.promise; });
  expect(await screen.findByRole("status")).toHaveTextContent("Account access consent granted.");
  expect(document.activeElement).toBe(elsewhere);
});

it("locks the scope and evidence while a change is on its way, so nothing typed then is cleared by its success", async () => {
  const change = held<TeenConsentChangedResponse>();
  vi.mocked(AdminTeenConsents.change).mockReturnValueOnce(change.promise);
  render(panel());
  const evidence = await screen.findByLabelText("Consent evidence");
  await userEvent.type(evidence, "Signed parent form");
  await userEvent.click(screen.getByRole("button", { name: "Grant consent" }));
  expect(evidence).toBeDisabled();
  expect(screen.getByRole("radio", { name: "SMS" })).toBeDisabled();
  await userEvent.type(evidence, " and a call");
  expect(evidence).toHaveValue("Signed parent form");
  await act(async () => { change.reject(new Error("offline")); await change.promise.catch(() => {}); });
  expect(evidence).toBeEnabled();
  expect(evidence).toHaveValue("Signed parent form");
});
