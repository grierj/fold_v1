import { stubService } from "../test/service";
import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, expect, test, vi } from "vitest";
import { MemoryRouter } from "react-router-dom";
import { ApiError } from "../api/core";
import { Contacts } from "../api/contacts";
import type { ContactGrant, GuardianConsentChangedResponse, GuardianTeen } from "../types";
import { GuardianConsentCard } from "./GuardianConsentCard";
import { Guardians } from "../api/guardians";

stubService(Guardians, () => ({ teens: vi.fn(), claim: vi.fn(), change: vi.fn(), email: vi.fn() }));

stubService(Contacts, () => ({ grants: vi.fn(), reveal: vi.fn(), revoke: vi.fn() }));

const teen: GuardianTeen = { linkId: 1, teenUserId: 2, teenName: "Sam Teen", accountConsent: true, smsConsent: false, photosConsent: false, revision: "r1", changes: 1, lastChangeYours: false };

// What a landed change answers: the card row as it left it, named with the token that follows it.
const stored = (row: GuardianTeen, revision = "r2"): GuardianConsentChangedResponse => ({
  teen: { id: row.teenUserId, revision }, guardianTeen: { ...row, revision },
});

const changed = () => new ApiError(409, "private", { code: "consent_changed" });
const grant: ContactGrant = { id: 1, requestId: 1, requesterId: 10, requesterName: "Lee", subjectUserId: 2, subjectName: "Sam Teen", contextType: "group", fields: "email", needsAdmin: true, grantedAt: "2026-10-02T12:00:00Z", endReason: "", canReveal: false, canRevoke: true };

beforeEach(() => {
  vi.resetAllMocks();
  vi.spyOn(window, "confirm").mockReturnValue(true);
});

test("a failed guardian list can be retried without reloading profile", async () => {
  vi.mocked(Guardians.teens).mockRejectedValueOnce(new Error("offline")).mockResolvedValueOnce({ teens: [{ ...teen, accountConsent: false }] });
  render(<GuardianConsentCard />);
  await userEvent.click(await screen.findByRole("button", { name: "Try again" }));
  expect(await screen.findByRole("heading", { name: "Sam Teen" })).toBeVisible();
  expect(screen.getByRole("button", { name: "Grant consent" })).toBeEnabled();
  expect(screen.queryByRole("button", { name: "Try again" })).not.toBeInTheDocument();
});

test("successful account consent revocation closes active grants and shows what its answer stored; reopening fetches current access", async () => {
  let finish!: (value: GuardianConsentChangedResponse) => void;
  const answer = new Promise<GuardianConsentChangedResponse>((resolve) => { finish = resolve; });
  vi.mocked(Guardians.teens).mockResolvedValueOnce({ teens: [teen] });
  vi.mocked(Guardians.change).mockReturnValueOnce(answer);
  vi.mocked(Contacts.grants).mockResolvedValueOnce({ grants: [grant] }).mockResolvedValueOnce({ grants: [] });
  render(<MemoryRouter><GuardianConsentCard /></MemoryRouter>);
  await userEvent.click(await screen.findByRole("button", { name: "View active contact access" }));
  expect(await screen.findByText("Email · Active grant")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Revoke access" })).toBeInTheDocument();
  await userEvent.click(screen.getByRole("button", { name: "Revoke consent" }));
  expect(window.confirm).toHaveBeenCalledWith(expect.stringContaining("They will be signed out"));
  expect(Guardians.change).toHaveBeenCalledWith(1, "account", "revoked", "r1");
  await act(async () => { finish(stored({ ...teen, accountConsent: false, changes: 2, lastChangeYours: true })); });
  expect(await screen.findByText("Account consent revoked.")).toBeInTheDocument();
  expect(screen.queryByRole("region", { name: "Active teen contact grants" })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Revoke access" })).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: "View active contact access" })).toHaveAttribute("aria-expanded", "false");
  expect(screen.getByText("Consent not granted")).toBeInTheDocument();
  expect(Guardians.teens).toHaveBeenCalledOnce();
  await userEvent.click(screen.getByRole("button", { name: "View active contact access" }));
  expect(await screen.findByText("No active contact grants")).toBeInTheDocument();
  expect(Contacts.grants).toHaveBeenCalledTimes(2);
  expect(screen.queryByText("Email · Active grant")).not.toBeInTheDocument();
  expect(Contacts.reveal).not.toHaveBeenCalled();
});

test("a grant load pending at consent revocation cannot restore ended access after reopening", async () => {
  let finishOld!: (value: { grants: ContactGrant[] }) => void;
  const old = new Promise<{ grants: ContactGrant[] }>((resolve) => { finishOld = resolve; });
  vi.mocked(Guardians.teens).mockResolvedValueOnce({ teens: [teen] });
  vi.mocked(Guardians.change).mockResolvedValue(stored({ ...teen, accountConsent: false, changes: 2, lastChangeYours: true }));
  vi.mocked(Contacts.grants).mockReturnValueOnce(old).mockResolvedValueOnce({ grants: [] });
  render(<MemoryRouter><GuardianConsentCard /></MemoryRouter>);
  await userEvent.click(await screen.findByRole("button", { name: "View active contact access" }));
  expect(await screen.findByText("Loading active grants…")).toBeInTheDocument();
  await userEvent.click(screen.getByRole("button", { name: "Revoke consent" }));
  expect(await screen.findByText("Account consent revoked.")).toBeInTheDocument();
  expect(screen.queryByRole("region", { name: "Active teen contact grants" })).not.toBeInTheDocument();
  await userEvent.click(screen.getByRole("button", { name: "View active contact access" }));
  expect(await screen.findByText("No active contact grants")).toBeInTheDocument();
  await act(async () => { finishOld({ grants: [grant] }); });
  expect(screen.getByText("No active contact grants")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Revoke access" })).not.toBeInTheDocument();
});

test.each(["cancelled", "failed"])("%s consent revocation preserves the expanded current grant", async (outcome) => {
  vi.mocked(Guardians.teens).mockResolvedValue({ teens: [teen] });
  vi.mocked(Contacts.grants).mockResolvedValue({ grants: [grant] });

  if (outcome === "cancelled") vi.mocked(window.confirm).mockReturnValue(false);
  else vi.mocked(Guardians.change).mockRejectedValue(new ApiError(403, "private", { code: "contact_unavailable" }));
  render(<MemoryRouter><GuardianConsentCard /></MemoryRouter>);
  await userEvent.click(await screen.findByRole("button", { name: "View active contact access" }));
  expect(await screen.findByRole("button", { name: "Revoke access" })).toBeInTheDocument();
  await userEvent.click(screen.getByRole("button", { name: "Revoke consent" }));

  if (outcome === "cancelled") expect(Guardians.change).not.toHaveBeenCalled();
  else expect(await screen.findByRole("alert")).toHaveTextContent("no longer available to you");
  expect(screen.getByRole("button", { name: "Close contact access" })).toHaveAttribute("aria-expanded", "true");
  expect(screen.getByRole("button", { name: "Revoke access" })).toBeInTheDocument();
  expect(Guardians.teens).toHaveBeenCalledTimes(1);
});

test("changing the consent type while a change is on its way keeps that change's answer", async () => {
  vi.mocked(Guardians.teens).mockResolvedValue({ teens: [teen] });
  let refuse!: (cause: unknown) => void;
  vi.mocked(Guardians.change).mockReturnValueOnce(new Promise((_, reject) => { refuse = reject; }));
  render(<MemoryRouter><GuardianConsentCard /></MemoryRouter>);
  await userEvent.click(await screen.findByRole("button", { name: "Revoke consent" }));
  await userEvent.selectOptions(screen.getByRole("combobox", { name: /^Consent/ }), "sms");
  await act(async () => { refuse(new ApiError(403, "private", { code: "contact_unavailable" })); });
  expect(await screen.findByRole("alert")).toHaveTextContent("no longer available to you");
});

test("a new guardian link request clears the last one's success line (#430)", async () => {
  vi.mocked(Guardians.teens).mockResolvedValue({ teens: [] });
  let refuse!: (cause: unknown) => void;
  vi.mocked(Guardians.claim).mockResolvedValueOnce({ message: "Request sent." }).mockReturnValueOnce(new Promise((_, reject) => { refuse = reject; }));
  render(<MemoryRouter><GuardianConsentCard /></MemoryRouter>);
  const email = await screen.findByLabelText(/Teen's email/);
  await userEvent.type(email, "sam@example.com");
  await userEvent.click(screen.getByRole("button", { name: "Request guardian link" }));
  expect(await screen.findByText("Request sent.")).toBeInTheDocument();
  await userEvent.type(email, "other@example.com");
  await userEvent.click(screen.getByRole("button", { name: "Request guardian link" }));
  expect(screen.queryByText("Request sent.")).not.toBeInTheDocument();
  await act(async () => { refuse(new ApiError(400, "private", { code: "invalid_guardian_email" })); });
  expect(await screen.findByRole("alert")).toHaveTextContent("Enter a parent or guardian's email address");
  expect(screen.queryByText("Request sent.")).not.toBeInTheDocument();
});

test("a grant refused because someone else changed the consent shows the consent as it is now and sends nothing more", async () => {
  const opened = { ...teen, accountConsent: false };
  vi.mocked(Guardians.teens).mockResolvedValueOnce({ teens: [opened] })
    .mockResolvedValueOnce({ teens: [{ ...opened, accountConsent: true, changes: 2, revision: "r2" }] });
  vi.mocked(Guardians.change).mockRejectedValue(changed());
  render(<MemoryRouter><GuardianConsentCard /></MemoryRouter>);
  await userEvent.click(await screen.findByRole("button", { name: "Grant consent" }));
  expect(await screen.findByText("Sam Teen's consent changed while you were choosing, so your choice could not be saved. This is the consent as it is now.")).toHaveFocus();
  expect(screen.getByText("Consent granted")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Revoke consent" })).toBeInTheDocument();
  expect(Guardians.change).toHaveBeenCalledOnce();
  expect(screen.queryByRole("alert")).not.toBeInTheDocument();
});

test("a refused grant whose read shows an account revoke closes the ended contact access; opening it again reads it", async () => {
  // Account consent is granted, so contact access is listed; SMS is not.
  vi.mocked(Guardians.teens).mockResolvedValueOnce({ teens: [teen] })
    .mockResolvedValueOnce({ teens: [{ ...teen, accountConsent: false, changes: 2, revision: "r2" }] });
  vi.mocked(Guardians.change).mockRejectedValue(changed());
  vi.mocked(Contacts.grants).mockResolvedValueOnce({ grants: [grant] }).mockResolvedValueOnce({ grants: [] });
  render(<MemoryRouter><GuardianConsentCard /></MemoryRouter>);
  await userEvent.click(await screen.findByRole("button", { name: "View active contact access" }));
  expect(await screen.findByText("Email · Active grant")).toBeInTheDocument();
  await userEvent.selectOptions(screen.getByRole("combobox", { name: /^Consent/ }), "sms");
  await userEvent.click(screen.getByRole("button", { name: "Grant consent" }));
  expect(await screen.findByText(/could not be saved/)).toHaveFocus();
  expect(screen.queryByText("Email · Active grant")).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Revoke access" })).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: "View active contact access" })).toHaveAttribute("aria-expanded", "false");
  await userEvent.click(screen.getByRole("button", { name: "View active contact access" }));
  expect(await screen.findByText("No active contact grants")).toBeInTheDocument();
  expect(Contacts.grants).toHaveBeenCalledTimes(2);
});

test("a grant this guardian already made in another tab is told as the state it left, and sent once", async () => {
  const opened = { ...teen, accountConsent: false };
  // The other tab's grant: one new change, this guardian's, account granted.
  vi.mocked(Guardians.teens).mockResolvedValueOnce({ teens: [opened] })
    .mockResolvedValueOnce({ teens: [{ ...opened, accountConsent: true, changes: 2, lastChangeYours: true, revision: "r2" }] });
  vi.mocked(Guardians.change).mockRejectedValue(changed());
  render(<MemoryRouter><GuardianConsentCard /></MemoryRouter>);
  await userEvent.click(await screen.findByRole("button", { name: "Grant consent" }));
  expect(await screen.findByRole("status")).toHaveTextContent("Account consent granted.");
  expect(screen.queryByText(/could not be saved/)).not.toBeInTheDocument();
  expect(screen.getByText("Consent granted")).toBeInTheDocument();
  expect(Guardians.change).toHaveBeenCalledOnce();
});

test("a grant and a revoke by someone else since the read are a conflict, never a quiet resend of the grant", async () => {
  const opened = { ...teen, accountConsent: false };
  // The account looks as it did (not granted), but two changes landed since the card read it.
  vi.mocked(Guardians.teens).mockResolvedValueOnce({ teens: [opened] })
    .mockResolvedValueOnce({ teens: [{ ...opened, changes: 3, revision: "r3" }] });
  vi.mocked(Guardians.change).mockRejectedValue(changed());
  render(<MemoryRouter><GuardianConsentCard /></MemoryRouter>);
  await userEvent.click(await screen.findByRole("button", { name: "Grant consent" }));
  expect(await screen.findByText(/could not be saved/)).toBeInTheDocument();
  expect(Guardians.change).toHaveBeenCalledOnce();
  expect(screen.getByText("Consent not granted")).toBeInTheDocument();
});

test("a grant refused only because the token moved (a restart) goes again under the new one and lands", async () => {
  const opened = { ...teen, accountConsent: false };
  vi.mocked(Guardians.teens).mockResolvedValueOnce({ teens: [opened] }).mockResolvedValueOnce({ teens: [{ ...opened, revision: "r9" }] });
  vi.mocked(Guardians.change).mockRejectedValueOnce(changed()).mockResolvedValueOnce(stored({ ...opened, accountConsent: true, changes: 2, lastChangeYours: true }, "r10"));
  render(<MemoryRouter><GuardianConsentCard /></MemoryRouter>);
  await userEvent.click(await screen.findByRole("button", { name: "Grant consent" }));
  expect(await screen.findByText("Account consent granted.")).toBeInTheDocument();
  expect(vi.mocked(Guardians.change).mock.calls.map((call) => call[3])).toEqual(["r1", "r9"]);
  expect(screen.queryByText(/could not be saved/)).not.toBeInTheDocument();
});

test("a grant whose answer was lost and is pressed again is saved, not refused", async () => {
  const opened = { ...teen, accountConsent: false };
  vi.mocked(Guardians.teens).mockResolvedValueOnce({ teens: [opened] })
    .mockResolvedValueOnce({ teens: [{ ...opened, accountConsent: true, changes: 2, lastChangeYours: true, revision: "r2" }] });
  vi.mocked(Guardians.change).mockRejectedValueOnce(new ApiError(502, "bad gateway", {})).mockRejectedValueOnce(changed());
  render(<MemoryRouter><GuardianConsentCard /></MemoryRouter>);
  await userEvent.click(await screen.findByRole("button", { name: "Grant consent" }));
  expect(await screen.findByRole("alert")).toBeInTheDocument();
  await userEvent.click(screen.getByRole("button", { name: "Grant consent" }));
  expect(await screen.findByText("Account consent granted.")).toBeInTheDocument();
  expect(screen.queryByText(/could not be saved/)).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Revoke consent" })).toBeInTheDocument();
  expect(vi.mocked(Guardians.change).mock.calls.map((call) => call[3])).toEqual(["r1", "r1"]);
});

test("a lost grant whose read shows someone else's grant is a conflict, not this guardian's save", async () => {
  const opened = { ...teen, accountConsent: false };
  vi.mocked(Guardians.teens).mockResolvedValueOnce({ teens: [opened] })
    .mockResolvedValueOnce({ teens: [{ ...opened, accountConsent: true, changes: 2, lastChangeYours: false, revision: "r2" }] });
  vi.mocked(Guardians.change).mockRejectedValueOnce(new ApiError(502, "bad gateway", {})).mockRejectedValueOnce(changed());
  render(<MemoryRouter><GuardianConsentCard /></MemoryRouter>);
  await userEvent.click(await screen.findByRole("button", { name: "Grant consent" }));
  await screen.findByRole("alert");
  await userEvent.click(screen.getByRole("button", { name: "Grant consent" }));
  expect(await screen.findByText(/could not be saved/)).toBeInTheDocument();
});

test("a revoke and then a grant each name what the last change left, and focus follows the button that replaced the pressed one", async () => {
  vi.mocked(Guardians.teens).mockResolvedValueOnce({ teens: [teen] });
  vi.mocked(Guardians.change)
    .mockResolvedValueOnce(stored({ ...teen, accountConsent: false, changes: 2, lastChangeYours: true }, "r2"))
    .mockResolvedValueOnce(stored({ ...teen, accountConsent: true, changes: 3, lastChangeYours: true }, "r3"));
  render(<MemoryRouter><GuardianConsentCard /></MemoryRouter>);
  (await screen.findByRole("button", { name: "Revoke consent" })).focus();
  await userEvent.keyboard("{Enter}");
  expect(await screen.findByText("Account consent revoked.")).toBeInTheDocument();
  await waitFor(() => expect(screen.getByRole("button", { name: "Grant consent" })).toHaveFocus());
  await userEvent.keyboard("{Enter}");
  expect(await screen.findByText("Account consent granted.")).toBeInTheDocument();
  await waitFor(() => expect(screen.getByRole("button", { name: "Revoke consent" })).toHaveFocus());
  expect(vi.mocked(Guardians.change).mock.calls.map((call) => [call[2], call[3]])).toEqual([["revoked", "r1"], ["granted", "r2"]]);
  expect(Guardians.teens).toHaveBeenCalledOnce();
});

test("a grant whose read no longer finds the link reads the links again and says the change could not be saved", async () => {
  const opened = { ...teen, accountConsent: false };
  vi.mocked(Guardians.teens).mockResolvedValueOnce({ teens: [opened] }).mockResolvedValue({ teens: [] });
  vi.mocked(Guardians.change).mockRejectedValue(changed());
  render(<MemoryRouter><GuardianConsentCard /></MemoryRouter>);
  await userEvent.click(await screen.findByRole("button", { name: "Grant consent" }));
  expect(await screen.findByText("Your changes could not be saved.")).toHaveFocus();
  expect(screen.queryByRole("heading", { name: "Sam Teen" })).not.toBeInTheDocument();
  expect(Guardians.change).toHaveBeenCalledOnce();
});
