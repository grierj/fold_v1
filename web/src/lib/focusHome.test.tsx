import { act, render, screen } from "@testing-library/react";
import { createRef, useState, type RefObject } from "react";
import { afterEach, expect, it, vi } from "vitest";
import { Dialog } from "../components/ui/Dialog";
import { useFocusHome, useKeepFocusHome, useLockedFocus, type FocusHome } from "./focusHome";

afterEach(() => { vi.useRealTimers(); });

// A home whose own render removes the focused button when `show` turns false, keeping focus when `keep` is set.
function Region({ show, keep, outer }: { show: boolean; keep: boolean; outer?: RefObject<FocusHome<HTMLElement> | null> }) {
  const home = useFocusHome<HTMLElement>();
  useKeepFocusHome(keep ? home : null);

  if (outer) outer.current = home;

  return <section ref={home.ref} tabIndex={-1} aria-label="Focus home">{show && <button type="button">Inside</button>}</section>;
}

it("moves focus to the home without scrolling", () => {
  const outer = createRef<FocusHome<HTMLElement>>();
  render(<Region show keep outer={outer} />);
  const section = screen.getByRole("region", { name: "Focus home" });
  const focus = vi.spyOn(section, "focus");
  act(() => outer.current?.moveHere());
  expect(document.activeElement).toBe(section);
  expect(focus).toHaveBeenCalledWith({ preventScroll: true });
});

it("keeps focus in the same commit when the calling component removes the focused control", () => {
  vi.useFakeTimers();
  const view = render(<Region show keep />);
  act(() => screen.getByRole("button", { name: "Inside" }).focus());
  act(() => view.rerender(<Region show={false} keep />));
  expect(document.activeElement).toBe(screen.getByRole("region", { name: "Focus home" }));
});

// A child that removes the focused control on its own, without the layout-effect path.
function Child({ show }: { show: boolean }) {
  return show ? <button type="button">Child control</button> : null;
}

function Outer({ show }: { show: boolean }) {
  const home = useFocusHome<HTMLElement>();

  return <section ref={home.ref} tabIndex={-1} aria-label="Focus home"><Child show={show} /></section>;
}

it("brings focus back after the observer's batch and a frame when another component removed the control", async () => {
  const view = render(<Outer show />);
  act(() => screen.getByRole("button", { name: "Child control" }).focus());
  view.rerender(<Outer show={false} />);
  await act(async () => { await new Promise((r) => requestAnimationFrame(() => r(null))); });
  expect(document.activeElement).toBe(screen.getByRole("region", { name: "Focus home" }));
});

it("leaves focus alone when it had moved outside first", async () => {
  const view = render(<><button type="button">Elsewhere</button><Outer show /></>);
  act(() => screen.getByRole("button", { name: "Child control" }).focus());
  act(() => screen.getByRole("button", { name: "Elsewhere" }).focus());
  view.rerender(<><button type="button">Elsewhere</button><Outer show={false} /></>);
  await act(async () => { await new Promise((r) => requestAnimationFrame(() => r(null))); });
  expect(document.activeElement).toBe(screen.getByRole("button", { name: "Elsewhere" }));
});

function WithDialog() {
  const home = useFocusHome<HTMLElement>();
  const [opener, setOpener] = useState(true);
  const [open, setOpen] = useState(false);

  return <>
    <section ref={home.ref} tabIndex={-1} aria-label="Focus home">
      {opener && <button type="button" onClick={() => setOpen(true)}>Ask</button>}
    </section>
    {open && <Dialog title="A question" onDismiss={() => setOpen(false)}><button type="button" onClick={() => setOpener(false)}>Drop the opener</button><button type="button" onClick={() => setOpen(false)}>Close it</button></Dialog>}
  </>;
}

it("returns focus to the home when a dialog opened from inside closes after its opener is gone", async () => {
  render(<WithDialog />);
  const ask = screen.getByRole("button", { name: "Ask" });
  act(() => ask.focus());
  act(() => ask.click());
  act(() => screen.getByRole("button", { name: "Drop the opener" }).click());
  expect(screen.queryByRole("button", { name: "Ask" })).not.toBeInTheDocument();
  act(() => screen.getByRole("button", { name: "Close it" }).click());
  await act(async () => { await new Promise((r) => requestAnimationFrame(() => r(null))); });
  expect(document.activeElement).toBe(screen.getByRole("region", { name: "Focus home" }));
});

it("does not move focus on a window blur and focus", async () => {
  render(<Outer show />);
  const control = screen.getByRole("button", { name: "Child control" });
  act(() => control.focus());
  act(() => { window.dispatchEvent(new Event("blur")); window.dispatchEvent(new Event("focus")); });
  await act(async () => { await new Promise((r) => requestAnimationFrame(() => r(null))); });
  expect(document.activeElement).toBe(control);
});

it("never focuses a home that is gone", () => {
  const outer = createRef<FocusHome<HTMLElement>>();
  const view = render(<Region show keep outer={outer} />);
  act(() => screen.getByRole("button", { name: "Inside" }).focus());
  const home = outer.current;
  view.unmount();
  expect(document.activeElement).toBe(document.body);
  act(() => { home?.rescue(); home?.moveHere(); });
  expect(document.activeElement).toBe(document.body);
});

function Locked({ locked }: { locked: boolean }) {
  const home = useFocusHome<HTMLFormElement>();
  useLockedFocus(home, locked);

  return <form ref={home.ref} tabIndex={-1} aria-label="Locked form">
    <fieldset disabled={locked}><input aria-label="Name" /></fieldset>
    <button type="button">Elsewhere</button>
  </form>;
}

it("a lock hands focus from the control it disables to the home, and back when it ends", () => {
  const view = render(<Locked locked={false} />);
  const name = screen.getByRole("textbox", { name: "Name" });
  name.focus();
  view.rerender(<Locked locked />);
  expect(document.activeElement).toBe(screen.getByRole("form", { name: "Locked form" }));
  view.rerender(<Locked locked={false} />);
  expect(document.activeElement).toBe(name);
});

it("a lock finds the control a browser blurred as soon as its fieldset was disabled", () => {
  const view = render(<Locked locked={false} />);
  const name = screen.getByRole("textbox", { name: "Name" });
  name.focus();
  // Chromium drops a text field's focus to the body when its fieldset is disabled, before any effect runs.
  name.blur();
  view.rerender(<Locked locked />);
  expect(document.activeElement).toBe(screen.getByRole("form", { name: "Locked form" }));
  view.rerender(<Locked locked={false} />);
  expect(document.activeElement).toBe(name);
});

it("a lock leaves focus alone when it is on a control the lock does not disable, or elsewhere", () => {
  const view = render(<><Locked locked={false} /><button type="button">Outside</button></>);
  screen.getByRole("textbox", { name: "Name" }).focus();
  const outside = screen.getByRole("button", { name: "Outside" });
  outside.focus();
  view.rerender(<><Locked locked /><button type="button">Outside</button></>);
  expect(document.activeElement).toBe(outside);
  const elsewhere = screen.getByRole("button", { name: "Elsewhere" });
  elsewhere.focus();
  view.rerender(<><Locked locked={false} /><button type="button">Outside</button></>);
  view.rerender(<><Locked locked /><button type="button">Outside</button></>);
  expect(document.activeElement).toBe(elsewhere);
});
