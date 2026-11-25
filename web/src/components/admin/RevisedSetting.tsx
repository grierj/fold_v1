import { useState, type ReactNode } from "react";
import { ApiError } from "../../api/core";
import { errorCode, errorText } from "../../lib/errors";
import { RefusedSaveLine, useRefusedSave } from "../../lib/refusedSave";
import { useAction } from "../../lib/useAction";
import { useLoad, type Load } from "../../lib/useLoad";
import { answerSetting, refusedAgain, useManagedSave } from "../../lib/useRevisionSave";
import { useUnsavedChanges } from "../../lib/unsavedChanges";
import { ErrorNotice } from "../ErrorNotice";
import { Skeleton } from "../ui/Skeleton";
import { Button } from "../ui/Button";
import { FormError } from "../ui/Field";
import { Form } from "../ui/Form";
import { FormActions } from "../ui/FormActions";

interface SettingRead {
  value: string;
  revision: string;
}

interface SettingProps {
  noun: string;
  read: () => Promise<SettingRead>;
  write: (value: string, revision: string | undefined) => Promise<SettingRead>;
  saveLabel: string;
  loadError: string;
  saveError: string;
  normalize?: (value: string) => string;
  validate?: (value: string) => string | undefined;
  fieldErrorCode?: string;
  onSaved?: (value: string) => void;
  children: (field: { value: string; saved: string; change: (value: string) => void; disabled: boolean; error?: string }) => ReactNode;
}

export function RevisedSetting(props: SettingProps) {
  const read = async () => {
    const answer = await props.read();

    if (!answerSetting(answer)) throw new ApiError(502, "unreadable setting", {}, true);

    return answer;
  };

  const load = useLoad(read, []);
  const refusal = useRefusedSave(load);

  return (
    <>
      <RefusedSaveLine save={refusal} noun={props.noun} canManage={load.data ? true : undefined} />
      {load.loading ? <Skeleton kind="lines" count={1} /> : load.data
        ? <SettingForm {...props} read={read} initial={load.data} load={load} refusal={refusal} />
        : <ErrorNotice message={props.loadError} onRetry={load.retry} />}
    </>
  );
}

function SettingForm({ read, write, saveLabel, saveError, normalize = (s) => s.trim(), validate, fieldErrorCode, onSaved, children, initial, load, refusal }: SettingProps & {
  initial: SettingRead;
  load: Load<SettingRead>;
  refusal: ReturnType<typeof useRefusedSave>;
}) {
  const [edited, setEdited] = useState<string | null>(null);
  const action = useAction();
  const [fieldError, setFieldError] = useState<string>();
  const value = edited ?? initial.value;
  const dirty = normalize(value) !== initial.value;
  const { guard } = useUnsavedChanges(dirty);

  const managedSave = useManagedSave({
    opened: initial,
    revision: initial.revision,
    read: async () => {
      const row = await read();

      return { allowed: true, revision: row.revision, row };
    },
    value: (row) => row.value,
    stored: (answer: SettingRead) => answer,
    fields: (stored) => stored,
    answers: answerSetting,
    changedCode: "setting_changed",
    stays: true,
    onConflict: (why) => refusal.refused(why),
  });

  function save(e: React.FormEvent) {
    e.preventDefault();

    if (!dirty) return;

    void action.run(async () => {
      const validation = validate?.(value);
      setFieldError(validation);

      if (validation) return false;
      const epoch = load.epoch();

      try {
        const result = await managedSave((revision) => write(value, revision), normalize(value));

        if (result.sent === "refused") throw refusedAgain("setting_changed");

        if (result.sent !== "saved" || !result.row || load.epoch() !== epoch) return false;

        load.setData(result.row);
        setEdited(null);
        onSaved?.(result.row.value);

        return "Saved";
      } catch (err) {
        if (!fieldErrorCode || errorCode(err) !== fieldErrorCode) throw err;

        setFieldError(errorText(err, saveError));

        return false;
      }
    }, { fallback: saveError });
  }

  return (
    <Form onSubmit={save} className="mb-6 max-w-xl space-y-4">
      {guard}
      {children({ value, saved: initial.value, disabled: action.running, error: fieldError, change: (next) => {
        setEdited(next);
        action.reset();
        setFieldError(undefined);
        refusal.clear();
      } })}
      <FormActions>
        <Button variant="secondary" layout="w-full sm:w-auto" type="submit" disabled={action.running || !dirty}>
          {action.running ? "Saving…" : saveLabel}
        </Button>
        {action.state.status === "done" && <span role="status" className="text-sm text-success">{action.state.text}</span>}
      </FormActions>
      {action.state.status === "failed" && <FormError>{action.state.text}</FormError>}
    </Form>
  );
}
