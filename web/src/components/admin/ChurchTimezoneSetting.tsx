import { Admin } from "../../api/admin";
import { invalidateSite } from "../../lib/useSite";
import { Field } from "../ui/Field";
import { ZonePicker } from "../ui/ZonePicker";
import { RevisedSetting } from "./RevisedSetting";

export function ChurchTimezoneSetting() {
  return (
    <RevisedSetting
      noun="church time zone"
      read={async () => {
        const answer = await Admin.timezone();

        return { value: answer?.timezone, revision: answer?.revision };
      }}
      write={async (value, revision) => {
        const answer = await Admin.setTimezone(value.trim(), revision);

        return { value: answer?.timezone, revision: answer?.revision };
      }}
      onSaved={(timezone) => invalidateSite({ timezone })}
      saveLabel="Save time zone"
      loadError="We couldn't load the church time zone."
      saveError="We couldn't save the time zone."
    >
      {({ value, change, disabled }) => (
        <Field id="church-tz" label="Church time zone" hint="New events start in this time zone; leaders can pick another when they schedule.">
          <ZonePicker forChurchSetting value={value} onChange={change} disabled={disabled} />
        </Field>
      )}
    </RevisedSetting>
  );
}
