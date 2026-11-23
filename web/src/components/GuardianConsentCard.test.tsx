import { stubService } from "../test/service";
import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, expect, test, vi } from "vitest";
import { MemoryRouter } from "react-router-dom";
import { ApiError } from "../api/core";
import { Contacts } from "../api/contacts";
import type { ContactGrant } from "../types";
import { GuardianConsentCard } from "./GuardianConsentCard";
import { Guardians } from "../api/guardians";

stubService(Guardians, () => ({ teens: vi.fn(), claim: vi.fn(), change: vi.fn(), email: vi.fn() }));

stubService(Contacts, () => ({ grants: vi.fn(), reveal: vi.fn(), revoke: vi.fn() }));

const teen = { linkId: 1, teenUserId: 2, teenName: "Sam Teen", accountConsent: true, smsConsent: false, photosConsent: false };
const grant: ContactGrant = { id: 1, requestId: 1, requesterId: 10, requesterName: "Lee", subjectUserId: 2, subjectName: "Sam Teen", contextType: "group", fields: "email", needsAdmin: true, grantedAt: "2026-10-02T12:00:00Z", endReason: "", canReveal: false, canRevoke: true };

beforeEach(() => {
  vi.resetAllMocks();
  vi.spyOn(window, "confirm").mockReturnValue(true);
});

test("a failed guardian list can be retried without reloading profile", async () => {
  vi.mocked(Guardians.teens).mockRejectedValueOnce(new Error("offline")).mockResolvedValueOnce({ teens: [{ linkId: 1, teenUserId: 2, teenName: "Sam Teen", accountConsent: false, smsConsent: false, photosConsent: false }] });
  render(<GuardianConsentCard />);
  await userEvent.click(await screen.findByRole("button", { name: "Try again" }));
  expect(await screen.findByRole("heading", { name: "Sam Teen" })).toBeVisible();
  expect(screen.getByRole("button", { name: "Grant consent" })).toBeEnabled();
  expect(screen.queryByRole("button", { name: "Try again" })).not.toBeInTheDocument();
});

test("successful account consent revocation closes active grants before the parent reloads; reopening fetches current access", async () => {
  let finishReload!: (value: { teens: typeof teen[] }) => void;
  const reload = new Promise<{ teens: typeof teen[] }>((resolve) => { finishReload = resolve; });
  vi.mocked(Guardians.teens).mockResolvedValueOnce({ teens: [teen] }).mockReturnValueOnce(reload);
  vi.mocked(Guardians.change).mockResolvedValue({});
  vi.mocked(Contacts.grants).mockResolvedValueOnce({ grants: [grant] }).mockResolvedValueOnce({ grants: [] });
  render(<MemoryRouter><GuardianConsentCard /></MemoryRouter>);
  await userEvent.click(await screen.findByRole("button", { name: "View active contact access" }));
  expect(await screen.findByText("Email · Active grant")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Revoke access" })).toBeInTheDocument();
  await userEvent.click(screen.getByRole("button", { name: "Revoke consent" }));
  expect(window.confirm).toHaveBeenCalledWith(expect.stringContaining("They will be signed out"));
  expect(Guardians.change).toHaveBeenCalledWith(1, "account", "revoked");
  expect(screen.queryByRole("region", { name: "Active teen contact grants" })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Revoke access" })).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: "View active contact access" })).toHaveAttribute("aria-expanded", "false");
  await act(async () => { finishReload({ teens: [{ ...teen, accountConsent: false }] }); });
  expect(await screen.findByText("Account consent revoked.")).toBeInTheDocument();
  expect(screen.getByText("Consent not granted")).toBeInTheDocument();
  await userEvent.click(screen.getByRole("button", { name: "View active contact access" }));
  expect(await screen.findByText("No active contact grants")).toBeInTheDocument();
  expect(Contacts.grants).toHaveBeenCalledTimes(2);
  expect(screen.queryByText("Email · Active grant")).not.toBeInTheDocument();
  expect(Contacts.reveal).not.toHaveBeenCalled();
});

test("a grant load pending at consent revocation cannot restore ended access after reopening", async () => {
  let finishOld!: (value: { grants: ContactGrant[] }) => void;
  const old = new Promise<{ grants: ContactGrant[] }>((resolve) => { finishOld = resolve; });
  vi.mocked(Guardians.teens).mockResolvedValueOnce({ teens: [teen] }).mockResolvedValue({ teens: [{ ...teen, accountConsent: false }] });
  vi.mocked(Guardians.change).mockResolvedValue({});
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

test("a consent change that saved but whose refresh failed offers no consent action until a refresh lands (#430)", async () => {
  vi.mocked(Guardians.teens).mockResolvedValueOnce({ teens: [teen] }).mockRejectedValueOnce(new Error("offline")).mockRejectedValueOnce(new Error("offline"))
    .mockResolvedValueOnce({ teens: [{ ...teen, accountConsent: false }] });
  vi.mocked(Guardians.change).mockResolvedValue({});
  render(<MemoryRouter><GuardianConsentCard /></MemoryRouter>);
  await userEvent.click(await screen.findByRole("button", { name: "Revoke consent" }));
  expect(await screen.findByRole("status")).toHaveTextContent("Account consent revoked.");
  expect(screen.getByRole("alert")).toHaveTextContent("Your change is saved, but we couldn't load Sam Teen's latest consent. Refresh to see it before you change anything else.");

  const stale = () => {
    expect(screen.queryByText("Consent granted")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^(Grant|Revoke) consent$/ })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Email me a consent link" })).not.toBeInTheDocument();
    expect(screen.queryByRole("combobox", { name: /^Consent/ })).not.toBeInTheDocument();
  };

  stale();
  const refresh = screen.getByRole("button", { name: "Refresh consent" });
  await userEvent.click(refresh);
  await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("couldn't reach"));
  stale();
  expect(screen.getByRole("status")).toHaveTextContent("Account consent revoked.");
  expect(refresh).toHaveFocus();
  await userEvent.click(refresh);
  expect(await screen.findByText("Consent not granted")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Refresh consent" })).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Grant consent" })).toHaveFocus();
  expect(Guardians.change).toHaveBeenCalledOnce();
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
