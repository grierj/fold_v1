import { useCallback, useEffect, useRef, useState } from "react";
import { errorText } from "./errors";

// One value, so a form is never saving and failed at once (docs/agents/lifecycles.md, Web client).
type ActionState =
  | { status: "idle" }
  | { status: "running" }
  | { status: "failed"; text: string }
  // `text` is what the work said it did ("" for nothing). `followUpFailed`: the work stands, and a step after it
  // (a reload) did not finish, so what is on screen may be out of date.
  | { status: "done"; text: string; followUpFailed: boolean };

interface RunOptions {
  // The errorText fallback for a thrown error.
  fallback?: string;
  // Runs after the work succeeded, inside the same run, so the controls stay disabled until it ends. Its error is
  // never the work's: copy for it says to reload, never to repeat the work.
  followUp?: () => Promise<void>;
}

export interface Action {
  state: ActionState;
  running: boolean;
  // Starts `work` unless a run is on its way (refused at once, even twice in one tick). The work resolves to what
  // to say it did, or to false when nothing was done and it has told the person itself (back to idle). A throw is
  // a failure, worded by errorText.
  run: (work: () => Promise<string | false | void>, options?: RunOptions) => Promise<void>;
  // Clears a done or failed state. A run on its way stays running (its controls disabled, a second run refused)
  // until it settles, and then lands as idle: its answer belongs to a draft that is gone, never to what comes next.
  reset: () => void;
}

// useAction is the one submit lifecycle for a form or a button that changes something.
export function useAction(): Action {
  const [state, setState] = useState<ActionState>({ status: "idle" });
  const busy = useRef(false);
  const alive = useRef(false);
  // Bumped by reset; a run started under an older generation settles as idle.
  const generation = useRef(0);

  // Set in the body, not only at init: StrictMode's rehearsal unmount must not leave it false.
  useEffect(() => {
    alive.current = true;

    return () => { alive.current = false; };
  }, []);

  const run = useCallback(async (work: () => Promise<string | false | void>, options: RunOptions = {}) => {
    if (busy.current) return;

    busy.current = true;
    const mine = generation.current;

    const settle = (next: ActionState) => {
      busy.current = false;

      if (alive.current) setState(mine === generation.current ? next : { status: "idle" });
    };

    if (alive.current) setState({ status: "running" });
    let result: string | false | void;

    try {
      result = await work();
    } catch (cause) {
      settle({ status: "failed", text: errorText(cause, options.fallback) });

      return;
    }

    if (result === false) {
      settle({ status: "idle" });

      return;
    }

    const text = result ?? "";

    try {
      await options.followUp?.();
    } catch {
      settle({ status: "done", text, followUpFailed: true });

      return;
    }

    settle({ status: "done", text, followUpFailed: false });
  }, []);

  const reset = useCallback(() => {
    if (busy.current) {
      generation.current++;

      return;
    }

    if (alive.current) setState({ status: "idle" });
  }, []);

  return { state, running: state.status === "running", run, reset };
}
