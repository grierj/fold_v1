import { testUser } from "../test/fixtures";
import { renderWithAuth } from "../test/auth";
import { stubService } from "../test/service";
import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { beforeEach, expect, it, vi } from "vitest";
import { Auth } from "../api/auth";
import { ApiError } from "../api/core";
import { ERROR_COPY } from "../lib/errors";
import { ResetPassword } from "./ResetPassword";

const signedIn = vi.fn();

const render = renderWithAuth(() => ({ signedIn }));

stubService(Auth, () => ({ confirmPasswordReset: vi.fn() }));

function renderAt(path: string) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <Routes>
        <Route path="/reset-password" element={<ResetPassword />} />
        <Route path="/" element={<p>Home page</p>} />
      </Routes>
    </MemoryRouter>,
  );
}

async function fillAndSubmit(password: string, confirm = password) {
  await userEvent.type(screen.getByLabelText("New password"), password);
  await userEvent.type(screen.getByLabelText("Confirm new password"), confirm);
  await userEvent.click(screen.getByRole("button", { name: "Set new password" }));
}

beforeEach(() => {
  vi.resetAllMocks();
  signedIn.mockResolvedValue("/");
});

it("offers a new reset link when the link has no token", () => {
  renderAt("/reset-password");
  expect(screen.getByText("This link is missing its token.")).toBeInTheDocument();
  expect(screen.getByRole("link", { name: "Request a new reset link" })).toHaveAttribute("href", "/forgot-password");
  expect(screen.queryByLabelText("New password")).not.toBeInTheDocument();
});

it("sends the token with the new password, signs the person in and goes where signedIn says", async () => {
  vi.mocked(Auth.confirmPasswordReset).mockResolvedValue({ user: testUser() });
  renderAt("/reset-password?token=abc123");
  await fillAndSubmit("a long enough password");
  expect(await screen.findByText("Home page")).toBeInTheDocument();
  expect(Auth.confirmPasswordReset).toHaveBeenCalledWith("abc123", "a long enough password");
  expect(signedIn).toHaveBeenCalledOnce();
});

it("does not send a password that is too short or does not match its confirmation", async () => {
  renderAt("/reset-password?token=abc123");
  await fillAndSubmit("short");
  expect(await screen.findByText("Password must be at least 10 characters.")).toBeInTheDocument();
  await userEvent.clear(screen.getByLabelText("New password"));
  await userEvent.clear(screen.getByLabelText("Confirm new password"));
  await fillAndSubmit("a long enough password", "a different password");
  expect(await screen.findByText("The passwords don't match.")).toBeInTheDocument();
  expect(Auth.confirmPasswordReset).not.toHaveBeenCalled();
});

it("replaces the password fields with a new reset request when the link has expired", async () => {
  vi.mocked(Auth.confirmPasswordReset).mockRejectedValue(new ApiError(410, "gone", { code: "link_expired" }));
  renderAt("/reset-password?token=old");
  await fillAndSubmit("a long enough password");
  expect(await screen.findByRole("link", { name: "Request a new reset link" })).toHaveAttribute("href", "/forgot-password");
  expect(screen.getByText(ERROR_COPY.link_expired)).toHaveFocus();
  expect(screen.queryByLabelText("New password")).not.toBeInTheDocument();
  expect(signedIn).not.toHaveBeenCalled();
});
