import { Fragment } from "react";

export function ManagerSaveStatus({ saving, updated, updateKey }: { saving: boolean; updated: boolean; updateKey?: number | null }) {
  return (
    <p aria-live="polite" aria-atomic="true" className={updated && !saving ? "mb-3 text-sm text-success" : "sr-only"}>
      {saving ? "Saving…" : updated ? <Fragment key={updateKey}>Updated</Fragment> : ""}
    </p>
  );
}
