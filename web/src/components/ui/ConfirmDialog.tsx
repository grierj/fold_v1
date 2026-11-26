import { createContext, useCallback, useContext, useLayoutEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { useLocation } from "react-router-dom";
import { Button } from "./Button";
import { Dialog } from "./Dialog";
import { FormActions } from "./FormActions";

export interface ConfirmRequest {
  // The question, as the dialog's heading.
  title: string;
  // What saying yes does, under the heading.
  body?: string;
  confirmLabel: string;
  cancelLabel?: string;
  // DEC-63: "danger" when the destructive act is the whole point (delete,
  // remove, cancel a date, end an invitation); focus starts on the answer that
  // changes nothing. "normal" for an override the person may legitimately take
  // (over capacity, decline dates, leave without saving); focus starts on it.
  tone: "danger" | "normal";
  // The whole question as the browser's own prompt words it, for routes that do not supply the dialog.
  native: string;
  interruption?: "leave" | "before-leaving";
}

// One action that may ask a question before it writes, captured when it starts
// (before its first await).
/** @public tests hold a started action and check when it goes stale. */
export interface PendingAction {
  // Asks; answers no without asking once the action is stale, and no if it went stale while open.
  ask: (request: ConfirmRequest) => Promise<boolean>;
  // May the action still write: the asking component has stayed mounted and on
  // screen, and neither the address, the page's facts nor the action's own facts
  // have changed since it started. A change that is later undone still counts.
  current: () => boolean;
  // Is the page it started on still the one showing: for what follows a write
  // that succeeded (the write itself may change the facts).
  here: () => boolean;
  // Calls `stale` once, as soon as the action is no longer current (at once if it already is not): for
  // something the action keeps open that is not one of these questions, such as the series scope picker.
  // Returns a function that stops watching.
  whenStale: (stale: () => void) => () => void;
}

// A fact an action was started under: a question asked under one value never submits under another.
type Authority = string | number | boolean | null | undefined;

interface Asked {
  id: number;
  request: ConfirmRequest;
  returnFocus: HTMLElement | null;
  resolve: (confirmed: boolean) => void;
  stale: () => boolean;
}

interface Scope {
  ask: (request: ConfirmRequest, returnFocus: HTMLElement | null, stale: () => boolean) => Promise<boolean>;
  // Closes the open question as no if the action it is for has gone stale, then tells every watcher to check.
  recheck: () => void;
  watch: (check: () => void) => () => void;
  // Counters that only go up: every committed move to another address, and every committed change of the page's facts.
  route: () => number;
  facts: () => number;
  noteFacts: (value: string) => void;
}

const ConfirmScope = createContext<Scope | null>(null);

// Whether the controls an action starts from are on screen. A list that keeps a hidden item mounted
// (CompactDates) says so, and hiding it, even if it is shown again, makes the item's actions stale.
const Shown = createContext(true);

export function ConfirmShown({ shown, children }: { shown: boolean; children: ReactNode }) {
  return <Shown.Provider value={shown}>{children}</Shown.Provider>;
}

// Whether the list keeping this component mounted shows it now (true outside such a list).
export function useShownInList(): boolean {
  return useContext(Shown);
}

// One yes-or-no question. The first answer (a button, Escape or a tap outside)
// is the only one: a second activation does nothing.
function ConfirmDialog({ request, returnFocus, onAnswer }: { request: ConfirmRequest; returnFocus: HTMLElement | null; onAnswer: (confirmed: boolean) => void }) {
  const answered = useRef(false);
  const [done, setDone] = useState(false);
  const confirm = useRef<HTMLButtonElement>(null);
  const cancel = useRef<HTMLButtonElement>(null);
  const danger = request.tone === "danger";

  function answer(confirmed: boolean) {
    if (answered.current) return;
    answered.current = true;
    setDone(true);
    onAnswer(confirmed);
  }

  return <Dialog title={request.title} description={request.body} initialFocus={danger ? cancel : confirm} returnFocus={returnFocus} onDismiss={() => answer(false)}>
    <FormActions>
      <Button ref={confirm} variant={danger ? "danger" : "primary"} layout="w-full sm:w-auto" disabled={done} onClick={() => answer(true)}>{request.confirmLabel}</Button>
      <Button ref={cancel} variant="secondary" layout="w-full sm:w-auto" disabled={done} onClick={() => answer(false)}>{request.cancelLabel ?? "Cancel"}</Button>
    </FormActions>
  </Dialog>;
}

// The routes that ask in the app's own dialog wrap their page in this. One
// question is open at a time. A leave question can take the place of one marked for interruption.
// An open question closes as no as soon as its action goes stale.
export function ConfirmProvider({ children }: { children: ReactNode }) {
  const [open, setOpen] = useState<Asked | null>(null);
  const current = useRef<Asked | null>(null);
  const nextId = useRef(0);
  const location = useLocation();
  // Every generation here and in useConfirm is published at commit, in a layout effect: a render React has
  // not committed (a transition that suspends or yields) leaves the tree the person sees, and the actions
  // its handlers start, under the generation they were shown. Unmounting counts at commit too.
  const route = useRef({ key: location.key, generation: 0 });
  const facts = useRef<{ value: string | null; generation: number }>({ value: null, generation: 0 });
  const watchers = useRef(new Set<() => void>());

  const finish = useCallback((asked: Asked, confirmed: boolean) => {
    if (current.current !== asked) return;
    current.current = null;
    setOpen(null);
    asked.resolve(confirmed);
  }, []);

  const scope = useMemo<Scope>(() => ({
    ask: (request, returnFocus, stale) => {
      const previous = current.current;

      if (previous) {
        if (request.interruption !== "leave" || previous.request.interruption !== "before-leaving") return Promise.resolve(false);
        returnFocus = previous.returnFocus;
        finish(previous, false);
      }

      return new Promise<boolean>((resolve) => {
        const asked = { id: ++nextId.current, request, returnFocus, resolve, stale };
        current.current = asked;
        setOpen(asked);
      });
    },
    recheck: () => {
      const asked = current.current;

      if (asked?.stale()) finish(asked, false);

      for (const check of watchers.current) check();
    },
    watch: (check) => {
      watchers.current.add(check);

      return () => { watchers.current.delete(check); };
    },
    route: () => route.current.generation,
    facts: () => facts.current.generation,
    noteFacts: (value) => {
      if (facts.current.value !== value) facts.current = { value, generation: facts.current.generation + 1 };
    },
  }), [finish]);

  useLayoutEffect(() => {
    if (route.current.key !== location.key) route.current = { key: location.key, generation: route.current.generation + 1 };
    scope.recheck();
  }, [location.key, scope]);

  useLayoutEffect(() => () => {
    const asked = current.current;
    current.current = null;
    asked?.resolve(false);
  }, []);

  return <ConfirmScope.Provider value={scope}>
    {children}
    {open && <ConfirmDialog key={open.id} request={open.request} returnFocus={open.returnFocus} onAnswer={(confirmed) => finish(open, confirmed)} />}
  </ConfirmScope.Provider>;
}

// The page under a ConfirmProvider names the data it shows (one call per
// provider): any change to it makes every action started before it stale, so
// a refresh can never leave a question about what the page used to show.
export function usePageFacts(value: string) {
  const scope = useContext(ConfirmScope);

  useLayoutEffect(() => {
    scope?.noteFacts(value);
    scope?.recheck();
  }, [value, scope]);
}

const unguarded: PendingAction = {
  ask: (request) => Promise.resolve(window.confirm(request.native)),
  current: () => true,
  here: () => true,
  whenStale: () => () => {},
};

// Starts an action that may ask a yes-or-no question before it writes. Inside
// a ConfirmProvider the question is the app's dialog, and the action goes
// stale when the asking component unmounts, the page moves to another
// address, the page's facts (usePageFacts) change, unless `followsPage` is
// false, `authority` (facts only this component knows) changes, or a list
// hides the component (ConfirmShown), even if it shows it again; an open
// question then closes as no. An action started while its list hides it is
// never current. Elsewhere it is the browser's own prompt, asked
// at once with `native`, and the action never goes stale, exactly as before.
export function useConfirm(authority?: Authority, { followsPage = true }: { followsPage?: boolean } = {}): () => PendingAction {
  const scope = useContext(ConfirmScope);
  const shown = useContext(Shown);
  const own = useRef({ authority, shown, generation: 0 });
  const mounted = useRef(false);
  const life = useRef(0);

  useLayoutEffect(() => {
    if (!Object.is(own.current.authority, authority) || own.current.shown !== shown) own.current = { authority, shown, generation: own.current.generation + 1 };
    scope?.recheck();
  }, [authority, shown, scope]);

  useLayoutEffect(() => {
    mounted.current = true;

    return () => {
      mounted.current = false;
      life.current++;
      scope?.recheck();
    };
  }, [scope]);

  return useCallback((): PendingAction => {
    if (!scope) return unguarded;
    const at = { route: scope.route(), facts: scope.facts(), own: own.current.generation, life: life.current };
    // The control that started the action, while it still has focus: it may be disabled or gone by the time a question opens.
    const opener = document.activeElement instanceof HTMLElement && document.activeElement !== document.body ? document.activeElement : null;
    const here = () => mounted.current && life.current === at.life && scope.route() === at.route;
    const current = () => here() && own.current.shown && own.current.generation === at.own && (!followsPage || scope.facts() === at.facts);

    return {
      current,
      here,
      ask: (request: ConfirmRequest) => current()
        ? scope.ask(request, opener, () => !current()).then((confirmed) => confirmed && current())
        : Promise.resolve(false),
      whenStale: (stale) => {
        let watching = true;
        let unwatch = () => {};

        const check = () => {
          if (!watching || current()) return;
          watching = false;
          unwatch();
          stale();
        };

        unwatch = scope.watch(check);
        check();

        return () => {
          watching = false;
          unwatch();
        };
      },
    };
  }, [scope, followsPage]);
}
