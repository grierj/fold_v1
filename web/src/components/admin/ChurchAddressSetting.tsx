import { Admin } from "../../api/admin";
import { Field, Textarea } from "../ui/Field";
import { RevisedSetting } from "./RevisedSetting";

const MAX_LINES = 6;
const lines = (address: string) => address.replace(/\r/g, "").split("\n").map((line) => line.trim()).filter(Boolean);

export function ChurchAddressSetting() {
  return (
    <RevisedSetting
      noun="church mailing address"
      read={async () => {
        const answer = await Admin.churchAddress();

        return { value: answer?.address, revision: answer?.revision };
      }}
      write={async (value, revision) => {
        const answer = await Admin.setChurchAddress(value, revision);

        return { value: answer?.address, revision: answer?.revision };
      }}
      saveLabel="Save address"
      loadError="We couldn't load the church's mailing address."
      saveError="We couldn't save the address. Try again."
      normalize={(address) => lines(address).join("\n")}
      validate={(address) => lines(address).length > MAX_LINES ? `Keep the address to ${MAX_LINES} lines.` : undefined}
    >
      {({ value, saved, change, disabled, error }) => <>
        {saved === "" && <p className="text-sm text-text-muted">Add your church's mailing address so optional emails can include it.</p>}
        <Field id="church-address" label="Church mailing address" optional error={error}
          hint="Shown at the bottom of optional emails, as the law asks. Up to 6 lines.">
          <Textarea rows={3} value={value} maxLength={300} autoComplete="street-address" disabled={disabled}
            onChange={(e) => change(e.target.value)} />
        </Field>
      </>}
    </RevisedSetting>
  );
}
