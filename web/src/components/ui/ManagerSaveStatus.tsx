export function ManagerSaveStatus({ saving, updated }: { saving: boolean; updated: boolean }) {
  return (
    <p aria-live="polite" aria-atomic="true" className={updated && !saving ? "mb-3 text-sm text-success" : "sr-only"}>
      {saving ? "Saving…" : updated ? "Updated" : ""}
    </p>
  );
}
