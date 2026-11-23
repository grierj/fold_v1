import { useEffect } from "react";
import { useConfirm } from "../components/ui/ConfirmDialog";
import { LEAVE, askToLeaveWith } from "./unsavedChanges";

// While mounted, leaving with unsaved edits asks in the page's ConfirmProvider dialog instead of the browser.
// A refresh of the page's data does not close it: the question is about the draft, not the data.
export function useLeaveQuestion() {
  const begin = useConfirm(undefined, { followsPage: false });

  useEffect(() => askToLeaveWith(() => begin().ask(LEAVE)), [begin]);
}
