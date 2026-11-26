import { useState } from "react";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, it } from "vitest";
import { Tabs } from "./Tabs";

function Example({ variant }: { variant?: "chips" | "underline" }) {
  const [value, setValue] = useState("events");

  return <Tabs label="Group sections" value={value} onChange={setValue} variant={variant} options={[
    { id: "events", label: "Events" }, { id: "about", label: "About" }, { id: "members", label: "Members" },
  ]}>{value} content</Tabs>;
}

it.each(["chips", "underline"] as const)("links %s tabs and panels with one tab stop and wrapping arrow navigation", async (variant) => {
  render(<Example variant={variant} />);
  const user = userEvent.setup();
  await user.tab();
  const events = screen.getByRole("tab", { name: "Events" });
  const about = screen.getByRole("tab", { name: "About" });
  const members = screen.getByRole("tab", { name: "Members" });
  expect(events).toHaveFocus();
  expect(events).toHaveAttribute("aria-selected", "true");
  expect(about).toHaveAttribute("tabindex", "-1");
  await user.keyboard("{ArrowRight}");
  expect(about).toHaveFocus();
  expect(about).toHaveAttribute("aria-selected", "true");
  const panel = screen.getByRole("tabpanel", { name: "About" });
  expect(about).toHaveAttribute("aria-controls", panel.id);
  expect(panel).toHaveTextContent("about content");
  await user.tab();
  expect(panel).toHaveFocus();
  about.focus();
  await user.keyboard("{End}");
  expect(members).toHaveFocus();
  await user.keyboard("{ArrowRight}");
  expect(events).toHaveFocus();
  await user.keyboard("{ArrowLeft}");
  expect(members).toHaveFocus();
  await user.keyboard("{Home}");
  expect(events).toHaveFocus();
});

it.each(["tab", "panel", "blurred"] as const)("handles focus when a %s is removed", (where) => {
  const options = [{ id: "about", label: "About" }, { id: "events", label: "Events" }];
  const page = (value: string, items: typeof options) => <Tabs label="Sections" value={value} onChange={() => {}} options={items}>Content</Tabs>;
  const { rerender } = render(page("events", options));
  const focused = where === "panel" ? screen.getByRole("tabpanel", { name: "Events" }) : screen.getByRole("tab", { name: "Events" });
  focused.focus();

  if (where === "blurred") focused.blur();

  rerender(page("about", [options[0]]));
  expect(where === "blurred" ? document.body : screen.getByRole("tab", { name: "About" })).toHaveFocus();
});
