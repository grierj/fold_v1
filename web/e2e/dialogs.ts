import AxeBuilder from "@axe-core/playwright";
import { expect, type Locator, type Page } from "@playwright/test";
import { mkdirSync } from "node:fs";

const proof = process.env.SCREENS_DIR;
const BLOCKING = new Set(["moderate", "serious", "critical"]);
const AXE_WIDTHS = [390, 1280];

// An open app dialog is the only live thing on the page: everything else in
// <body> is inert (an older dialog under it too), Tab and Shift+Tab wrap
// inside it, a click on its heading keeps the keyboard in it, the open portal
// has no moderate or worse axe issue at 390 and 1280px, nothing in it scrolls
// sideways at 390px, its buttons are 44px targets there, and on a short screen
// every button can still be scrolled to. Focus ends where it started, so the
// caller can go on to answer it.
export async function expectModalDialog(page: Page, dialog: Locator, name: string) {
  await expect(dialog).toBeVisible();
  const viewport = page.viewportSize();
  const started = await dialog.evaluate((panel) => panel.contains(document.activeElement) ? document.activeElement?.textContent ?? "" : null);
  expect(started, `${name}: focus starts inside the dialog`).not.toBeNull();
  const startedAt = await page.evaluateHandle(() => document.activeElement);

  const live = await dialog.evaluate((panel) => {
    const overlay = Array.from(document.body.children).find((el) => el.contains(panel));

    return Array.from(document.body.children).filter((el) => el !== overlay && !el.hasAttribute("inert")).map((el) => el.outerHTML.slice(0, 80));
  });

  expect(live, `${name}: the page behind is inert`).toEqual([]);
  expect(await page.locator("#main").evaluate((main) => main.closest("[inert]") !== null), `${name}: the page's main region is inert`).toBe(true);

  const controls = await dialog.locator("button:not([disabled]), a[href], input:not([disabled]), select:not([disabled]), textarea:not([disabled])").count();
  expect(controls, `${name}: the dialog has controls`).toBeGreaterThan(0);

  for (const key of ["Tab", "Shift+Tab"]) {
    for (let i = 0; i < controls; i++) {
      await page.keyboard.press(key);
      expect(await dialog.evaluate((panel) => panel.contains(document.activeElement)), `${name}: ${key} stays in the dialog`).toBe(true);
    }
  }

  expect(await dialog.evaluate(() => document.activeElement?.textContent ?? ""), `${name}: a full Tab cycle comes back to the start`).toBe(started);

  // A click on the heading leaves focus on the panel, not on the page or nowhere; Tab comes back to a control.
  await dialog.getByRole("heading").first().click();
  expect(await dialog.evaluate((panel) => panel.contains(document.activeElement)), `${name}: a click on the heading keeps focus in the dialog`).toBe(true);
  await page.keyboard.press("Tab");
  expect(await dialog.evaluate((panel) => panel.contains(document.activeElement) && document.activeElement !== panel), `${name}: Tab after a heading click reaches a control`).toBe(true);
  await startedAt.asElement()?.focus();
  expect(await dialog.evaluate(() => document.activeElement?.textContent ?? ""), `${name}: focus is back where it started`).toBe(started);

  // Only this dialog: an older one under it is inert, as is the page.
  const labelledBy = await dialog.getAttribute("aria-labelledby");
  expect(labelledBy, `${name}: the dialog is labelled by its heading`).toBeTruthy();

  for (const width of AXE_WIDTHS) {
    await page.setViewportSize({ width, height: 844 });

    const { violations } = await new AxeBuilder({ page })
      .include(`[role="dialog"][aria-labelledby="${labelledBy}"]`)
      .withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa", "wcag22aa"])
      .analyze();

    const bad = violations.filter((v) => BLOCKING.has(v.impact ?? "")).map((v) => `${v.impact} ${v.id}: ${v.nodes.map((n) => n.target.join(" ")).join(", ")}`);
    expect(bad, `${name}@${width}: axe on the open dialog`).toEqual([]);
  }

  await page.setViewportSize({ width: 390, height: 844 });

  const phone = await dialog.evaluate((panel) => ({
    sideways: panel.scrollWidth - panel.clientWidth,
    outside: Math.round(panel.getBoundingClientRect().right - window.innerWidth),
    small: Array.from(panel.querySelectorAll("button")).filter((b) => b.getClientRects().length > 0)
      .map((b) => ({ name: b.textContent?.trim() ?? "", width: Math.round(b.getBoundingClientRect().width), height: Math.round(b.getBoundingClientRect().height) }))
      .filter((b) => b.width < 44 || b.height < 44),
  }));

  expect(phone.sideways, `${name}@390: nothing scrolls sideways`).toBeLessThanOrEqual(0);
  expect(phone.outside, `${name}@390: the panel stays on screen`).toBeLessThanOrEqual(0);
  expect(phone.small, `${name}@390: 44px targets`).toEqual([]);

  if (proof) {
    mkdirSync(proof, { recursive: true });
    await page.screenshot({ path: `${proof}/${name}-390.png`, animations: "disabled", caret: "hide" });
  }

  await page.setViewportSize({ width: 390, height: 420 });

  for (const button of await dialog.getByRole("button").all()) {
    await button.scrollIntoViewIfNeeded();
    await expect(button, `${name}@390x420: every button can be reached`).toBeInViewport();
  }

  if (proof) await page.screenshot({ path: `${proof}/${name}-390x420.png`, animations: "disabled", caret: "hide" });

  if (viewport) await page.setViewportSize(viewport);
}
