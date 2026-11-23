import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useRef } from "react";
import { expect, it, vi } from "vitest";
import { Button } from "./Button";
import { Dialog } from "./Dialog";

function Shell({ onDismiss, focusSecond = false }: { onDismiss: () => void; focusSecond?: boolean }) {
  const second = useRef<HTMLButtonElement>(null);

  return <Dialog title="Delete this?" description="It cannot be undone." onDismiss={onDismiss} initialFocus={focusSecond ? second : undefined}>
    <Button variant="danger">Delete</Button>
    <Button ref={second} variant="secondary">Keep</Button>
  </Dialog>;
}

it("is a labelled, described modal that starts on its first control and keeps Tab inside", async () => {
  render(<Shell onDismiss={vi.fn()} />);
  const dialog = screen.getByRole("dialog", { name: "Delete this?" });
  expect(dialog).toHaveAttribute("aria-modal", "true");
  expect(dialog).toHaveAccessibleDescription("It cannot be undone.");
  expect(screen.getByRole("button", { name: "Delete" })).toHaveFocus();
  await userEvent.tab();
  expect(screen.getByRole("button", { name: "Keep" })).toHaveFocus();
  await userEvent.tab();
  expect(screen.getByRole("button", { name: "Delete" })).toHaveFocus();
  await userEvent.tab({ shift: true });
  expect(screen.getByRole("button", { name: "Keep" })).toHaveFocus();
});

it("starts on the control it is given", () => {
  render(<Shell onDismiss={vi.fn()} focusSecond />);
  expect(screen.getByRole("button", { name: "Keep" })).toHaveFocus();
});

it("dismisses with Escape or a tap outside, not a tap inside", async () => {
  const dismiss = vi.fn();
  render(<Shell onDismiss={dismiss} />);
  fireEvent.mouseDown(screen.getByRole("dialog"));
  expect(dismiss).not.toHaveBeenCalled();
  await userEvent.keyboard("{Escape}");
  expect(dismiss).toHaveBeenCalledOnce();
  const overlay = screen.getByRole("dialog").parentElement;

  if (!overlay) throw new Error("The dialog has no overlay.");
  fireEvent.mouseDown(overlay);
  expect(dismiss).toHaveBeenCalledTimes(2);
});

it("makes the page behind it inert while open and gives focus back to its opener", () => {
  const page = document.createElement("main");
  const opener = document.createElement("button");
  page.append(opener);
  document.body.append(page);
  opener.focus();
  const view = render(<Shell onDismiss={vi.fn()} />);
  expect(page).toHaveAttribute("inert");
  expect(screen.getByRole("dialog").closest("[inert]")).toBeNull();
  view.unmount();
  expect(page).not.toHaveAttribute("inert");
  expect(opener).toHaveFocus();
  page.remove();
});

it("leaves focus alone when its opener is gone", () => {
  const opener = document.createElement("button");
  document.body.append(opener);
  opener.focus();
  const view = render(<Shell onDismiss={vi.fn()} />);
  opener.remove();
  view.unmount();
  expect(document.activeElement).not.toBe(opener);
});

function Named({ name, onDismiss = vi.fn(), returnFocus }: { name: string; onDismiss?: () => void; returnFocus?: HTMLElement | null }) {
  return <Dialog title={name} onDismiss={onDismiss} returnFocus={returnFocus}>
    <Button variant="primary">{`${name} yes`}</Button>
    <Button variant="secondary">{`${name} no`}</Button>
  </Dialog>;
}

function overlayOf(name: string) {
  const overlay = screen.getByRole("dialog", { name }).parentElement;

  if (!overlay) throw new Error(`${name} has no overlay.`);

  return overlay;
}

it("keeps only the newest of two open dialogs live, and the page inert until both close", () => {
  const page = document.createElement("main");
  const opener = document.createElement("button");
  page.append(opener);
  document.body.append(page);
  opener.focus();
  const first = render(<Named name="Scope" />);
  const second = render(<Named name="Retry" />);
  expect(page).toHaveAttribute("inert");
  expect(overlayOf("Scope")).toHaveAttribute("inert");
  expect(overlayOf("Retry")).not.toHaveAttribute("inert");
  expect(screen.getByRole("button", { name: "Retry yes" })).toHaveFocus();

  // The older dialog goes away first (a refresh): the newer one stays live and the page stays inert.
  first.unmount();
  expect(page).toHaveAttribute("inert");
  expect(overlayOf("Retry")).not.toHaveAttribute("inert");
  expect(overlayOf("Retry").closest("[inert]")).toBeNull();

  // Its return target was the opener the older dialog would have gone back to.
  second.unmount();
  expect(page).not.toHaveAttribute("inert");
  expect(opener).toHaveFocus();
  page.remove();
});

it("gives two dialogs that open in one commit one live dialog, the last", () => {
  const page = document.createElement("main");
  document.body.append(page);
  const view = render(<><Named name="Scope" /><Named name="Retry" /></>);
  expect(overlayOf("Scope")).toHaveAttribute("inert");
  expect(overlayOf("Retry")).not.toHaveAttribute("inert");
  expect(screen.getByRole("button", { name: "Retry yes" })).toHaveFocus();
  view.unmount();
  expect(page).not.toHaveAttribute("inert");
  page.remove();
});

it("hands focus to the dialog underneath when the newer one closes, and wakes it", () => {
  const first = render(<Named name="Scope" />);
  const second = render(<Named name="Retry" />);
  second.unmount();
  expect(overlayOf("Scope")).not.toHaveAttribute("inert");
  expect(screen.getByRole("dialog", { name: "Scope" }).contains(document.activeElement)).toBe(true);
  first.unmount();
});

it("only the dialog a key is pressed in answers it", async () => {
  const scope = vi.fn();
  const retry = vi.fn();
  const first = render(<Named name="Scope" onDismiss={scope} />);
  const second = render(<Named name="Retry" onDismiss={retry} />);
  await userEvent.keyboard("{Escape}");
  expect(retry).toHaveBeenCalledOnce();
  expect(scope).not.toHaveBeenCalled();
  second.unmount();
  first.unmount();
});

it("gives focus back to a busy opener once it is enabled again", async () => {
  const opener = document.createElement("button");
  document.body.append(opener);
  opener.focus();
  opener.disabled = true;
  const view = render(<Named name="Retry" returnFocus={opener} />);
  view.unmount();
  expect(opener).not.toHaveFocus();
  opener.disabled = false;
  await waitFor(() => expect(opener).toHaveFocus());
  opener.remove();
});

it("does not take focus back for a busy opener once something else has it", async () => {
  const opener = document.createElement("button");
  const elsewhere = document.createElement("button");
  document.body.append(opener, elsewhere);
  opener.disabled = true;
  const view = render(<Named name="Retry" returnFocus={opener} />);
  view.unmount();
  elsewhere.focus();
  opener.disabled = false;
  await waitFor(() => expect(elsewhere).toHaveFocus());
  await new Promise((resolve) => setTimeout(resolve, 0));
  expect(elsewhere).toHaveFocus();
  opener.remove();
  elsewhere.remove();
});

it("moves focus to the page's main region when the opener has left the page", async () => {
  const main = document.createElement("main");
  main.id = "main";
  main.tabIndex = -1;
  const opener = document.createElement("button");
  main.append(opener);
  document.body.append(main);
  opener.disabled = true;
  const view = render(<Named name="Retry" returnFocus={opener} />);
  view.unmount();
  opener.remove();
  await waitFor(() => expect(main).toHaveFocus());
  main.remove();
});

const dropFocus = () => {
  if (document.activeElement instanceof HTMLElement) document.activeElement.blur();
};

it("keeps Escape and Tab working after a click on its heading or text takes focus off its buttons", async () => {
  const dismiss = vi.fn();
  render(<Shell onDismiss={dismiss} />);
  const dialog = screen.getByRole("dialog", { name: "Delete this?" });
  await userEvent.click(screen.getByRole("heading", { name: "Delete this?" }));
  expect(screen.getByRole("button", { name: "Delete" })).not.toHaveFocus();
  expect(dialog).toHaveFocus();
  await userEvent.tab({ shift: true });
  expect(screen.getByRole("button", { name: "Keep" })).toHaveFocus();
  await userEvent.click(screen.getByText("It cannot be undone."));
  await userEvent.keyboard("{Escape}");
  expect(dismiss).toHaveBeenCalledOnce();

  // Focus nowhere at all (the body): Tab comes back in at the start, and Escape still dismisses.
  dropFocus();
  expect(document.body).toHaveFocus();
  await userEvent.tab();
  expect(screen.getByRole("button", { name: "Delete" })).toHaveFocus();
  dropFocus();
  await userEvent.keyboard("{Escape}");
  expect(dismiss).toHaveBeenCalledTimes(2);
});

it("answers a key with focus nowhere only in the newest dialog", async () => {
  const scope = vi.fn();
  const retry = vi.fn();
  const first = render(<Named name="Scope" onDismiss={scope} />);
  const second = render(<Named name="Retry" onDismiss={retry} />);
  await userEvent.click(screen.getByRole("heading", { name: "Retry" }));
  dropFocus();
  await userEvent.keyboard("{Escape}");
  expect(retry).toHaveBeenCalledOnce();
  expect(scope).not.toHaveBeenCalled();
  await userEvent.tab();
  expect(screen.getByRole("button", { name: "Retry yes" })).toHaveFocus();
  second.unmount();
  await userEvent.keyboard("{Escape}");
  expect(scope).toHaveBeenCalledOnce();
  first.unmount();
});

it("puts focus on the panel when it has no control to start on", () => {
  render(<Dialog title="Working…" onDismiss={vi.fn()}><p>Please wait.</p></Dialog>);
  expect(screen.getByRole("dialog", { name: "Working…" })).toHaveFocus();
});

it("moves focus to the page's main region when the opener is in a part of the page that was hidden", async () => {
  const main = document.createElement("main");
  main.id = "main";
  main.tabIndex = -1;
  const form = document.createElement("div");
  const opener = document.createElement("button");
  form.append(opener);
  main.append(form);
  document.body.append(main);
  opener.disabled = true;
  const view = render(<Named name="Retry" returnFocus={opener} />);
  form.hidden = true;
  view.unmount();
  opener.disabled = false;
  await waitFor(() => expect(main).toHaveFocus());
  main.remove();
});
