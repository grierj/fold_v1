import { stubService } from "../test/service";
import { StrictMode } from "react";
import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, expect, it, vi } from "vitest";
import { AdminTeenConsents } from "../api/adminUsers";
import { ApiError } from "../api/core";
import type { TeenConsent, User } from "../types";
import TeenConsentPanel from "./TeenConsentPanel";

stubService(AdminTeenConsents, () => ({ list: vi.fn(), change: vi.fn() }));

const teen: User = {
  id: 7, name: "Casey Teen", displayName: "Casey", email: "casey@example.com", phone: "",
  avatarUrl: "", avatarFocalX: 50, avatarFocalY: 50, role: "member", status: "pending_consent",
  hasPassword: true, accountType: "teen", hideContactFromLeaders: true, birthDate: "2011-01-01", timezone: "",
};

const consent: TeenConsent = {
  id: 11, teenUserId: teen.id, scope: "account", status: "granted", grantedBy: 1, granterRole: "admin",
  evidenceText: "Signed parent form", at: "2026-10-22T12:00:00Z",
};

beforeEach(() => {
  vi.mocked(AdminTeenConsents.list).mockReset().mockResolvedValue({ consents: [], administrativelyDisabled: false });
  vi.mocked(AdminTeenConsents.change).mockReset().mockResolvedValue({});
});

it("requires and focuses evidence before an admin grant", async () => {
  render(<TeenConsentPanel user={teen} onUpdated={vi.fn()} onClose={vi.fn()} />);
  await userEvent.click(await screen.findByRole("button", { name: "Grant consent" }));
  expect(screen.getByLabelText("Consent evidence")).toHaveFocus();
  expect(screen.getByText("Write how you received consent first.")).toBeInTheDocument();
  expect(AdminTeenConsents.change).not.toHaveBeenCalled();
});

it("saves the chosen scope and reloads the recorded history", async () => {
  const updated = vi.fn();
  render(<TeenConsentPanel user={teen} onUpdated={updated} onClose={vi.fn()} />);
  await userEvent.click(await screen.findByRole("radio", { name: "SMS" }));
  await userEvent.type(screen.getByLabelText("Consent evidence"), "Parent signed SMS form");
  vi.mocked(AdminTeenConsents.list).mockResolvedValue({ consents: [{ ...consent, scope: "sms", evidenceText: "Parent signed SMS form" }], administrativelyDisabled: false });
  await userEvent.click(screen.getByRole("button", { name: "Grant consent" }));
  expect(AdminTeenConsents.change).toHaveBeenCalledWith(7, { scope: "sms", status: "granted", evidenceText: "Parent signed SMS form" });
  expect(await screen.findByRole("status")).toHaveTextContent("SMS consent granted.");
  expect(updated).toHaveBeenCalledOnce();
  await userEvent.click(screen.getByText("Consent history (1)"));
  expect(screen.getByText("Parent signed SMS form")).toBeVisible();
});

it("confirms account revocation and preserves evidence after an API error", async () => {
  const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
  const updated = vi.fn();
  render(<TeenConsentPanel user={teen} onUpdated={updated} onClose={vi.fn()} />);
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
  vi.mocked(AdminTeenConsents.list).mockResolvedValue({ consents: [consent], administrativelyDisabled: true });
  const view = render(<TeenConsentPanel user={teen} onUpdated={vi.fn()} onClose={vi.fn()} />);
  expect(await screen.findByText(/Granting consent keeps it disabled/)).toBeInTheDocument();
  view.rerender(<TeenConsentPanel user={{ ...teen, accountType: "adult", hideContactFromLeaders: false }} onUpdated={vi.fn()} onClose={vi.fn()} />);
  await waitFor(() => expect(screen.queryByRole("button", { name: "Grant consent" })).not.toBeInTheDocument());
  expect(screen.getByText(/Their teen consent history stays on file/)).toBeVisible();
  await userEvent.click(screen.getByText("Consent history (1)"));
  expect(screen.getByText("Signed parent form")).toBeVisible();
});

it("says a grant was saved when only the refresh after it fails, and offers no consent action until a refresh lands (#430)", async () => {
  const updated = vi.fn();
  render(<TeenConsentPanel user={teen} onUpdated={updated} onClose={vi.fn()} />);
  await userEvent.type(await screen.findByLabelText("Consent evidence"), "Signed parent form");
  vi.mocked(AdminTeenConsents.list).mockRejectedValueOnce(new Error("offline")).mockRejectedValueOnce(new Error("offline"));
  await userEvent.click(screen.getByRole("button", { name: "Grant consent" }));
  expect(AdminTeenConsents.change).toHaveBeenCalledOnce();
  expect(await screen.findByRole("status")).toHaveTextContent("Account access consent granted.");
  expect(screen.getByRole("alert")).toHaveTextContent("Your change is saved, but we couldn't load Casey's latest consent. Refresh to see it before you change anything else.");
  expect(screen.queryByText(/couldn't save/)).not.toBeInTheDocument();

  const stale = () => {
    expect(screen.queryByRole("button", { name: /^(Grant|Revoke) consent$/ })).not.toBeInTheDocument();
    expect(screen.queryAllByText(/Not granted|Granted/)).toHaveLength(0);
    expect(screen.queryByLabelText("Consent evidence")).not.toBeInTheDocument();
  };

  stale();
  expect(updated).toHaveBeenCalledOnce();
  const refresh = screen.getByRole("button", { name: "Refresh consent" });
  await userEvent.click(refresh);
  await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("couldn't reach"));
  stale();
  expect(refresh).toHaveFocus();
  vi.mocked(AdminTeenConsents.list).mockResolvedValueOnce({ consents: [consent], administrativelyDisabled: false });
  await userEvent.click(refresh);
  expect(await screen.findByText("Granted")).toBeInTheDocument();
  expect(screen.getByLabelText("Consent evidence")).toHaveValue("");
  expect(screen.getByRole("button", { name: "Grant consent" })).toHaveFocus();
  expect(AdminTeenConsents.change).toHaveBeenCalledOnce();
});

function held<T>() {
  let resolve!: (value: T) => void;
  let reject!: (cause: unknown) => void;
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });

  return { promise, resolve, reject };
}

it("an older read that lands after a newer one never replaces its answer", async () => {
  const first = held<{ consents: TeenConsent[]; administrativelyDisabled: boolean }>();
  const second = held<{ consents: TeenConsent[]; administrativelyDisabled: boolean }>();
  vi.mocked(AdminTeenConsents.list).mockReturnValueOnce(first.promise).mockReturnValueOnce(second.promise);
  // StrictMode mounts twice, so two reads go out; the first one asked answers last.
  render(<StrictMode><TeenConsentPanel user={teen} onUpdated={vi.fn()} onClose={vi.fn()} /></StrictMode>);
  await waitFor(() => expect(AdminTeenConsents.list).toHaveBeenCalledTimes(2));
  await act(async () => { second.resolve({ consents: [consent], administrativelyDisabled: false }); await second.promise; });
  expect(await screen.findByText("Granted")).toBeInTheDocument();
  await act(async () => { first.resolve({ consents: [], administrativelyDisabled: false }); await first.promise; });
  expect(screen.getByText("Granted")).toBeInTheDocument();
  expect(screen.queryByText("Consent history (0)")).not.toBeInTheDocument();
});

it("moves focus from the pressed button to Refresh consent when the refresh after a change fails", async () => {
  render(<TeenConsentPanel user={teen} onUpdated={vi.fn()} onClose={vi.fn()} />);
  await userEvent.type(await screen.findByLabelText("Consent evidence"), "Signed parent form");
  vi.mocked(AdminTeenConsents.list).mockRejectedValueOnce(new Error("offline"));
  screen.getByRole("button", { name: "Grant consent" }).focus();
  await userEvent.keyboard("{Enter}");
  const refresh = await screen.findByRole("button", { name: "Refresh consent" });
  await waitFor(() => expect(refresh).toHaveFocus());
});

it("leaves focus where the person moved it while the change was on its way", async () => {
  const change = held<Record<string, never>>();
  vi.mocked(AdminTeenConsents.change).mockReturnValueOnce(change.promise);
  render(<><button>Elsewhere</button><TeenConsentPanel user={teen} onUpdated={vi.fn()} onClose={vi.fn()} /></>);
  await userEvent.type(await screen.findByLabelText("Consent evidence"), "Signed parent form");
  vi.mocked(AdminTeenConsents.list).mockRejectedValueOnce(new Error("offline"));
  await userEvent.click(screen.getByRole("button", { name: "Grant consent" }));
  const elsewhere = screen.getByRole("button", { name: "Elsewhere" });
  elsewhere.focus();
  await act(async () => { change.resolve({}); await change.promise; });
  expect(await screen.findByRole("button", { name: "Refresh consent" })).toBeInTheDocument();
  expect(document.activeElement).toBe(elsewhere);
});

it("locks the scope and evidence while a change is on its way, so nothing typed then is cleared by its success", async () => {
  const change = held<Record<string, never>>();
  vi.mocked(AdminTeenConsents.change).mockReturnValueOnce(change.promise);
  render(<TeenConsentPanel user={teen} onUpdated={vi.fn()} onClose={vi.fn()} />);
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
