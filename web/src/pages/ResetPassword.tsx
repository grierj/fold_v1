import { useState } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { Auth as AuthApi } from "../api/auth";
import { useAuth } from "../auth";
import { ERROR_COPY, errorCode, errorText } from "../lib/errors";
import { NewPasswordForm } from "../components/NewPasswordForm";
import { ButtonLink } from "../components/ui/Button";
import { EmptyState } from "../components/ui/EmptyState";

export function ResetPassword() {
  const [params] = useSearchParams();
  const token = params.get("token") ?? "";

  return <ResetPasswordForm key={token} token={token} />;
}

function focusExplanation(element: HTMLElement | null) {
  element?.focus();
}

function ResetPasswordForm({ token }: { token: string }) {
  const navigate = useNavigate();
  const { signedIn } = useAuth();
  const [expired, setExpired] = useState(false);

  if (!token || expired) {
    return (
      <div className="fold-auth wrap-anywhere">
        <h1 className="fold-page-header text-3xl font-bold mb-6">Reset your password</h1>
        <EmptyState
          title={expired ? ERROR_COPY.link_expired : "This link is missing its token."}
          titleRef={expired ? focusExplanation : undefined}
          action={<ButtonLink to="/forgot-password" variant="primary">Request a new reset link</ButtonLink>}
        />
      </div>
    );
  }

  return (
    <div className="fold-auth wrap-anywhere">
      <h1 className="fold-page-header text-3xl font-bold mb-6">Choose a new password</h1>
      <NewPasswordForm
        idPrefix="rp"
        passwordLabel="New password"
        confirmLabel="Confirm new password"
        submitLabel="Set new password"
        submit={async (password) => {
          try {
            const result = await AuthApi.confirmPasswordReset(token, password);
            navigate(await signedIn(result), { replace: true });
          } catch (err) {
            if (errorCode(err) !== "link_expired") throw err;
            setExpired(true);
          }
        }}
        describe={(e) => errorText(e, "We couldn't reset your password.")}
      />
    </div>
  );
}
