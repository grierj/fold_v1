import { useContext, useEffect, useRef } from "react";
import { UNSAFE_DataRouterContext, useBlocker } from "react-router-dom";
import type { ConfirmRequest } from "../components/ui/ConfirmDialog";

// Leaving a page with unsaved edits asks first (DEC-98). Closing or reloading
// the tab gets the browser's own prompt; moving to another page in the app
// gets a confirm from the one app-wide <UnsavedChangesBlocker> (React Router
// consults only one blocker, so forms register here instead of each adding
// their own). Call `markSaved()` right before navigating away after a
// successful save, so the edits the save just stored don't block it; a page
// that stays open after that navigation calls `resume()` right after it, so
// later edits are guarded again. A page that asks in the app's own dialog
// registers it with `useLeaveQuestion` (lib/leaveQuestion.ts, kept out of the
// eager bundle with the dialog); elsewhere the browser's confirm asks.
const openForms = new Set<() => boolean>();
let leavingOnPurpose = false;
let askToLeave: (() => Promise<boolean>) | null = null;

export const LEAVE = {
  title: "Leave without saving?",
  body: "Your changes will be lost.",
  confirmLabel: "Leave without saving",
  cancelLabel: "Keep editing",
  tone: "normal",
  native: "Leave without saving? Your changes will be lost.",
} satisfies ConfirmRequest;

const anyUnsaved = () => !leavingOnPurpose && [...openForms].some((unsaved) => unsaved());

// Log out leaves the page on purpose; don't ask about unsaved edits then.
export function leaveWithoutAsking() {
  leavingOnPurpose = true;
}

export function useUnsavedChanges(dirty: boolean) {
  const dirtyRef = useRef(dirty);
  const saved = useRef(false);
  dirtyRef.current = dirty;
  useEffect(() => {
    const unsaved = () => dirtyRef.current && !saved.current;
    openForms.add(unsaved);

    return () => { openForms.delete(unsaved); };
  }, []);
  useEffect(() => {
    if (!dirty) {
      saved.current = false;

      return;
    }

    const onBeforeUnload = (e: BeforeUnloadEvent) => {
      if (anyUnsaved()) e.preventDefault();
    };

    window.addEventListener("beforeunload", onBeforeUnload);

    return () => window.removeEventListener("beforeunload", onBeforeUnload);
  }, [dirty]);

  // `guard` is kept for callers that render it; the app-wide blocker does the work.
  return { guard: null, markSaved: () => { saved.current = true; }, resume: () => { saved.current = false; } };
}

// A draft a host keeps for an editor it has taken off the screen for a moment (the date editor's, while its date is
// read again): `unsent` is read when the person leaves, since the host does not render when the draft changes.
export function useKeptDraft(unsent: () => boolean, on: boolean) {
  const latest = useRef(unsent);
  latest.current = unsent;
  useEffect(() => {
    if (!on) return;

    const unsaved = () => latest.current();

    const onBeforeUnload = (e: BeforeUnloadEvent) => {
      if (anyUnsaved()) e.preventDefault();
    };

    openForms.add(unsaved);
    window.addEventListener("beforeunload", onBeforeUnload);

    return () => {
      openForms.delete(unsaved);
      window.removeEventListener("beforeunload", onBeforeUnload);
    };
  }, [on]);
}

// The page dialog that asks before leaving, while its page is mounted; returns the unregister.
export function askToLeaveWith(asker: () => Promise<boolean>): () => void {
  askToLeave = asker;

  return () => {
    if (askToLeave === asker) askToLeave = null;
  };
}

// Rendered once inside the data router (App). useBlocker needs a data router;
// tests that render in a MemoryRouter get the tab-close prompt only.
export function UnsavedChangesBlocker() {
  const dataRouter = useContext(UNSAFE_DataRouterContext);

  return dataRouter ? <NavigationGuard /> : null;
}

function NavigationGuard() {
  const blocker = useBlocker(({ currentLocation, nextLocation }) =>
    anyUnsaved() && (currentLocation.pathname !== nextLocation.pathname || currentLocation.search !== nextLocation.search));

  const latest = useRef(blocker);
  // One question at a time. A navigation blocked while it is open (Back
  // pressed again) asks nothing more: the answer applies to whichever
  // navigation is waiting when it comes, so Leave goes to the latest one.
  const asking = useRef(false);
  latest.current = blocker;

  useEffect(() => {
    if (blocker.state !== "blocked") return;
    const ask = askToLeave;

    if (!ask) {
      if (window.confirm(LEAVE.native)) blocker.proceed();
      else blocker.reset();

      return;
    }

    if (asking.current) return;
    asking.current = true;
    void ask().then((leave) => {
      asking.current = false;
      // The newest blocker's own proceed and reset: an earlier one's no longer moves history.
      const now = latest.current;

      if (now.state !== "blocked") return;

      if (leave) now.proceed();
      else now.reset();
    });
  }, [blocker]);

  return null;
}
