import { useEffect, useId, useRef, type ReactNode, type RefObject } from "react";
import { createPortal } from "react-dom";

const FOCUSABLE = "button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), a[href], [tabindex]:not([tabindex='-1'])";
const RETURN_WAIT_MS = 10_000;

interface OpenDialog {
  overlay: HTMLElement;
  returnTo: HTMLElement | null;
}

// Every open dialog, oldest first, whoever rendered it (a page's ConfirmProvider,
// the series scope picker). Only the newest one is live: the page and every
// dialog under it are inert, so two overlapping dialogs never share the keyboard.
const openDialogs: OpenDialog[] = [];
const madeInert = new Set<Element>();
const topDialog = (): OpenDialog | undefined => openDialogs[openDialogs.length - 1];

function syncInert() {
  const top = topDialog()?.overlay;
  const want = new Set<Element>();

  if (top) {
    for (const el of Array.from(document.body.children)) {
      if (el !== top && (madeInert.has(el) || !el.hasAttribute("inert"))) want.add(el);
    }
  }

  for (const el of madeInert) {
    if (!want.has(el)) {
      el.removeAttribute("inert");
      madeInert.delete(el);
    }
  }

  for (const el of want) {
    if (!madeInert.has(el)) {
      el.setAttribute("inert", "");
      madeInert.add(el);
    }
  }
}

const firstControl = (root: HTMLElement | null) => root?.querySelector<HTMLElement>(FOCUSABLE) ?? null;

// A control in a hidden part of the page (a closed form) cannot take focus, however long it is waited for.
const gone = (target: HTMLElement) => !target.isConnected || target.closest("[hidden]") !== null;

const nobodyHasFocus = (target: HTMLElement) => !document.activeElement || document.activeElement === document.body || document.activeElement === target;

// The page's main region (Layout's <main tabIndex={-1}>), for when the opener is gone.
function focusFallback() {
  document.getElementById("main")?.focus({ preventScroll: true });
}

// An opener that is busy (disabled while its action runs) cannot take focus
// yet: wait until it is enabled again, unless focus has moved on meanwhile.
// If it leaves the page while waiting, the page's main region takes focus.
function focusWhenEnabled(target: HTMLElement) {
  const observer = new MutationObserver(check);
  const timer = window.setTimeout(stop, RETURN_WAIT_MS);

  function stop() {
    observer.disconnect();
    window.clearTimeout(timer);
  }

  function check() {
    if (openDialogs.length > 0 || !nobodyHasFocus(target)) {
      stop();

      return;
    }

    if (gone(target)) {
      stop();
      focusFallback();
    } else if (!target.matches(":disabled")) {
      stop();
      target.focus();
    }
  }

  observer.observe(document.body, { subtree: true, childList: true, attributes: true, attributeFilter: ["disabled"] });
}

function giveFocusBack(target: HTMLElement | null) {
  const top = topDialog();

  if (top) {
    (target && top.overlay.contains(target) ? target : firstControl(top.overlay))?.focus();

    return;
  }

  if (!target) return;

  if (gone(target)) focusFallback();
  else if (target.matches(":disabled")) focusWhenEnabled(target);
  else target.focus();
}

// The one modal shell. The overlay sits directly in <body> and everything else
// is inert while it is open, so taps, focus, screen readers and target-size
// checks never reach the page behind it. Focus starts on `initialFocus` (else
// the first control), Tab stays inside, Escape or a tap outside dismisses.
// The keys are heard on the document, so they still work when a click on the
// dialog's text has left focus on the panel or nowhere, and only the newest
// open dialog answers them.
// Closing gives focus back to `returnFocus` when it is still on the page (the
// control that started the action, captured before any await), else to
// whatever had focus when the dialog opened.
export function Dialog({ title, description, onDismiss, initialFocus, returnFocus, children }: {
  title: ReactNode;
  description?: ReactNode;
  onDismiss: () => void;
  initialFocus?: RefObject<HTMLElement | null>;
  returnFocus?: HTMLElement | null;
  children: ReactNode;
}) {
  const titleId = useId();
  const descriptionId = useId();
  const panel = useRef<HTMLDivElement>(null);
  const overlay = useRef<HTMLDivElement>(null);
  const dismiss = useRef(onDismiss);
  dismiss.current = onDismiss;

  useEffect(() => {
    if (!overlay.current) return;
    const active = document.activeElement instanceof HTMLElement && document.activeElement !== document.body ? document.activeElement : null;
    const entry: OpenDialog = { overlay: overlay.current, returnTo: returnFocus?.isConnected ? returnFocus : active };
    openDialogs.push(entry);
    syncInert();
    (initialFocus?.current ?? firstControl(panel.current) ?? panel.current)?.focus();

    function keyDown(event: KeyboardEvent) {
      const root = panel.current;

      if (topDialog() !== entry || !root) return;

      if (event.key === "Escape") {
        event.preventDefault();
        dismiss.current();

        return;
      }

      if (event.key !== "Tab") return;
      const controls = Array.from(root.querySelectorAll<HTMLElement>(FOCUSABLE));
      const active = document.activeElement;
      // Focus on the panel itself or outside it (a click on its text): Tab starts again at an end.
      const lost = active === root || !root.contains(active);

      if (!controls.length) {
        event.preventDefault();
        root.focus();

        return;
      }

      const first = controls[0];
      const last = controls[controls.length - 1];

      if (event.shiftKey && (lost || active === first)) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && (lost || active === last)) {
        event.preventDefault();
        first.focus();
      }
    }

    document.addEventListener("keydown", keyDown);

    return () => {
      document.removeEventListener("keydown", keyDown);
      const at = openDialogs.indexOf(entry);
      openDialogs.splice(at, 1);
      const above = openDialogs[at];

      // A dialog closing under a newer one hands it its own return target when the newer one's was inside it.
      if (above && (!above.returnTo || entry.overlay.contains(above.returnTo))) above.returnTo = entry.returnTo;
      syncInert();

      if (!above) giveFocusBack(entry.returnTo);
    };
    // Opening is the only moment that decides focus and inertness.
  }, []);

  return createPortal(<div ref={overlay} className="fixed inset-0 z-50 flex items-end sm:items-center justify-center bg-black/40 p-0 sm:p-4" onMouseDown={(event) => {
    if (event.target === event.currentTarget) onDismiss();
  }}>
    <div ref={panel} role="dialog" aria-modal="true" aria-labelledby={titleId} aria-describedby={description ? descriptionId : undefined} tabIndex={-1}
      className="outline-none w-full sm:max-w-md max-h-full overflow-y-auto wrap-anywhere rounded-t-2xl sm:rounded-2xl bg-surface border border-border p-5 shadow-xl space-y-3">
      <h2 id={titleId} className="text-xl font-bold">{title}</h2>
      {description && <p id={descriptionId} className="text-sm text-text-muted">{description}</p>}
      {children}
    </div>
  </div>, document.body);
}
