import { render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { StickyRsvpBar } from "./StickyRsvpBar";

afterEach(() => { document.querySelector('nav[aria-label="Tabs"]')?.remove(); vi.restoreAllMocks(); });

function tabs(height: number) {
  const nav = document.createElement("nav");
  nav.setAttribute("aria-label", "Tabs");
  vi.spyOn(nav, "getBoundingClientRect").mockReturnValue({ height, width: 390, top: 844 - height, bottom: 844, left: 0, right: 390, x: 0, y: 844 - height, toJSON: () => ({}) });
  document.body.append(nav);
}

it("sits on the phone Tabs it measures and reserves its own height and theirs", () => {
  tabs(57);
  vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockReturnValue({ height: 61, width: 390, top: 0, bottom: 61, left: 0, right: 390, x: 0, y: 0, toJSON: () => ({}) });
  const view = render(<StickyRsvpBar label="Answer this date"><button type="button">Going</button></StickyRsvpBar>);
  const bar = screen.getByRole("region", { name: "Answer this date" });
  expect(bar.style.bottom).toBe("57px");
  expect(bar).not.toHaveClass("date-bar-inset");
  expect(view.container.firstElementChild).toHaveStyle({ height: "61px" });
  expect(document.documentElement).toHaveClass("fold-date-bar");
  expect(document.documentElement.style.getPropertyValue("--fold-date-bar")).toBe("118px");
  view.unmount();
  // The root's scroll padding is the page's alone again.
  expect(document.documentElement).not.toHaveClass("fold-date-bar");
  expect(document.documentElement.style.getPropertyValue("--fold-date-bar")).toBe("");
});

it.each([["no Tabs", false], ["Tabs hidden at a wider width", true]])("keeps the bottom safe inset itself with %s", (_label, hidden) => {
  if (hidden) {
    tabs(57);
    document.querySelector<HTMLElement>('nav[aria-label="Tabs"]')!.style.display = "none";
  }

  render(<StickyRsvpBar label="Answer this date"><button type="button">Register as a guest</button></StickyRsvpBar>);
  const bar = screen.getByRole("region", { name: "Answer this date" });
  expect(bar.style.bottom).toBe("0px");
  expect(bar).toHaveClass("date-bar-inset");
});
