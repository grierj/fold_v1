import { act, render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { RefusedSaveLine, useRefusedSave } from "./refusedSave";
import { useLoad } from "./useLoad";

type Shown = "loaded" | "missing";

// A page reduced to what the piece needs: a read it can retry, and the screen that read leads to.
function setup(screenShown: Shown = "loaded", canManage = true, about?: string) {
  let answer!: () => void;
  let api!: ReturnType<typeof useRefusedSave>;
  let retry!: () => void;

  function Page() {
    const load = useLoad(() => new Promise<string>((done) => { answer = () => done("thing"); }), []);
    api = useRefusedSave(load);
    retry = () => void load.retry();

    if (load.loading) return <p>Loading</p>;

    if (screenShown === "missing") return <div><RefusedSaveLine save={api} noun="widget" /></div>;

    return <div><RefusedSaveLine save={api} noun="widget" canManage={canManage} about={about} /></div>;
  }

  render(<Page />);

  return {
    answer: () => act(async () => answer()),
    refused: (why: "changed" | "gone") => act(() => api.refused(why)),
    clear: () => act(() => api.clear()),
    retry: () => act(() => retry()),
  };
}

describe("useRefusedSave and RefusedSaveLine", () => {
  it("names which one first, on a page that lists several", async () => {
    const page = setup("loaded", true, "Tuesday, December 1, 2026");
    await page.answer();
    page.refused("changed");
    await page.answer();
    expect(screen.getByRole("status")).toHaveTextContent("Tuesday, December 1, 2026. This widget changed while you were editing, so your changes weren't saved. This is the widget as it is now.");
    expect(screen.getByRole("status")).toHaveFocus();
  });

  it("shows nothing on an ordinary visit", async () => {
    const page = setup();
    await page.answer();
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
  });

  it("reads from nothing and says so, with focus, only once the read answers", async () => {
    const page = setup();
    await page.answer();
    page.refused("changed");
    expect(screen.getByText("Loading")).toBeInTheDocument();
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
    await page.answer();
    const line = screen.getByRole("status");
    expect(line).toHaveTextContent("This widget changed while you were editing, so your changes weren't saved. This is the widget as it is now.");
    expect(line).toHaveFocus();
  });

  it.each([
    ["changed", false, "This widget changed while you were editing, so your changes weren't saved. This is the widget as it is now."],
    ["gone", true, "Your changes weren't saved. This is the widget as it is now."],
    ["gone", false, "You can no longer manage this widget, so your changes weren't saved."],
  ] as const)("on the loaded screen, says the %s refusal for a viewer who may manage it: %s", async (why, canManage, text) => {
    const page = setup("loaded", canManage);
    await page.answer();
    page.refused(why);
    await page.answer();
    expect(screen.getByRole("status")).toHaveTextContent(text);
  });

  it.each(["changed", "gone"] as const)("on a screen that shows no widget, says only that the changes weren't saved (%s)", async (why) => {
    const page = setup("missing");
    await page.answer();
    page.refused(why);
    await page.answer();
    const line = screen.getByRole("status");
    expect(line).toHaveTextContent(/^Your changes weren't saved\.$/);
    expect(line).toHaveFocus();
  });

  it("clears when told, and a clear before the read answers keeps it from showing", async () => {
    const page = setup();
    await page.answer();
    page.refused("changed");
    await page.answer();
    page.clear();
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
    page.refused("gone");
    page.clear();
    await page.answer();
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
  });

  it("does not take focus again when a later read shows the line again", async () => {
    const page = setup();
    await page.answer();
    page.refused("changed");
    await page.answer();
    page.retry();
    await page.answer();
    expect(screen.getByRole("status")).not.toHaveFocus();
  });
});
