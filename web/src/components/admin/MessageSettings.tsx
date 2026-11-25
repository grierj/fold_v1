import { Admin } from "../../api/admin";
import { Field, Input } from "../ui/Field";
import { useLoad } from "../../lib/useLoad";
import { RevisedSetting } from "./RevisedSetting";
import { SettingSwitch } from "./SettingSwitch";
import { ErrorNotice } from "../ErrorNotice";

// The two church settings announcements read (M3.5b): guardian copies of
// messages to teens (DEC-19) and where replies go (DEC-130). Each is
// audited on the server.

// GuardianCopiesSetting is one on/off switch (SettingSwitch, DEC-98).
export function GuardianCopiesSetting() {
  const setting = useLoad(() => Admin.guardianCopies(), []);

  if (setting.error) {
    return <div className="mb-6 max-w-xl"><ErrorNotice message="We couldn't load the guardian copies setting." onRetry={setting.retry} /></div>;
  }

  return (
    <SettingSwitch
      label="Send guardians a copy of messages to their teens"
      description="When an announcement, a reminder or an optional notice email goes to a teen, their confirmed parents or guardians also get an email copy, marked as a copy. Account and safety emails aren't copied. Takes effect for emails sent from now on. Off by default."
      value={setting.data ? setting.data.on : null}
      save={async (next) => {
        const r = await Admin.setGuardianCopies(next);
        setting.setData({ on: r.on });
      }}
    />
  );
}

export function ReplyToSetting() {
  return (
    <RevisedSetting
      noun="reply address"
      read={async () => {
        const answer = await Admin.replyTo();

        return { value: answer?.address, revision: answer?.revision };
      }}
      write={async (value, revision) => {
        const answer = await Admin.setReplyTo(value.trim(), revision);

        return { value: answer?.address, revision: answer?.revision };
      }}
      saveLabel="Save reply address"
      loadError="We couldn't load where replies go."
      saveError="We couldn't save where replies go. Try again."
      fieldErrorCode="reply_to_invalid"
    >
      {({ value, change, disabled, error }) => (
        <Field id="reply-to" label="Where replies go" optional error={error}
          hint="Replies to announcements go here, usually the church office. Leave empty for no reply address.">
          <Input type="email" value={value} maxLength={254} autoComplete="email" disabled={disabled}
            onChange={(e) => change(e.target.value)} />
        </Field>
      )}
    </RevisedSetting>
  );
}
