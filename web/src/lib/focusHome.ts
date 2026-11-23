import { useEffect, useLayoutEffect, useMemo, useRef, type RefObject } from "react";

// A stable, named element (tabIndex -1) inside a region that stays on screen, which keeps keyboard focus when a
// render removes the control that had it: focus goes there, never to the page body.
export interface FocusHome<T extends HTMLElement = HTMLElement> {
  ref: RefObject<T | null>;
  // Focus the home now, without scrolling: just before a change known to remove the focused control.
  /** @public the kept-dates panel's date switch calls it. */
  moveHere: () => void;
  // Focus the home if focus was inside it and has been lost.
  rescue: () => void;
}

// Where focus lands when it is lost: nowhere, or the page's main region, which a closing dialog falls back to
// when the control that opened it is gone (Dialog.tsx).
const lostFocus = (active: Element | null, fromDialog: boolean) => active === null || active === document.body || (fromDialog && active.id === "main");

// `yields`, when given, says a home inside this one keeps focus now (ManagedDateEditor's while its editor's own
// form is mounted), so this one rescues only when that inner home is gone.
export function useFocusHome<T extends HTMLElement>(yields?: () => boolean): FocusHome<T> {
  const ref = useRef<T | null>(null);
  const yielding = useRef(yields);
  yielding.current = yields;
  // Whether focus was last inside the home, or in a dialog opened from inside it (Dialog portals out).
  const inside = useRef(false);
  const fromDialog = useRef(false);

  const home = useMemo<FocusHome<T>>(() => ({
    ref,
    moveHere: () => ref.current?.focus({ preventScroll: true }),
    rescue: () => {
      const el = ref.current;

      if (yielding.current?.()) return;

      if (inside.current && el?.isConnected && lostFocus(document.activeElement, fromDialog.current)) {
        fromDialog.current = false;
        el.focus({ preventScroll: true });
      }
    },
  }), []);

  useEffect(() => {
    const track = (target: Element | null, focus: boolean) => {
      const el = ref.current;

      if (el && target && el.contains(target)) {
        inside.current = true;
        fromDialog.current = false;
      } else if (inside.current && target?.closest("[role=dialog]")) {
        fromDialog.current = true;
      } else if (!(focus && inside.current && fromDialog.current && target?.id === "main")) {
        inside.current = false;
        fromDialog.current = false;
      }
    };

    const onFocusIn = (e: FocusEvent) => track(e.target instanceof Element ? e.target : null, true);
    // A press on something that takes no focus (the page's background) leaves the home too.
    const onPointerDown = (e: PointerEvent) => track(e.target instanceof Element ? e.target : null, false);
    document.addEventListener("focusin", onFocusIn);
    document.addEventListener("pointerdown", onPointerDown);

    return () => {
      document.removeEventListener("focusin", onFocusIn);
      document.removeEventListener("pointerdown", onPointerDown);
    };
  }, []);

  // Afterwards: focus lost in a passive effect is brought back after the DOM change and one frame. The home's
  // own subtree covers a removed control; the body's children cover a dialog closing after its opener is gone.
  useEffect(() => {
    const el = ref.current;

    if (!el || typeof MutationObserver === "undefined") return;
    let frame = 0;

    const observer = new MutationObserver(() => {
      home.rescue();
      cancelAnimationFrame(frame);
      frame = requestAnimationFrame(() => home.rescue());
    });

    observer.observe(el, { childList: true, subtree: true });
    observer.observe(document.body, { childList: true });

    return () => {
      observer.disconnect();
      cancelAnimationFrame(frame);
    };
  }, [home]);

  return home;
}

// Called by the component whose own render may remove the focused control: the rescue runs after every commit
// of that component, so focus is kept in the same commit.
export function useKeepFocusHome(home: FocusHome | null): void {
  useLayoutEffect(() => {
    home?.rescue();
  });
}
