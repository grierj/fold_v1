import "@testing-library/jest-dom/vitest";
import { cleanup } from "@testing-library/react";
import { afterEach, beforeEach } from "vitest";
import { forgetSite } from "../lib/useSite";

// Node's BroadcastChannel reaches every thread of the process, and vmThreads runs test files side by side in
// worker threads, so another file's "fold-credential" hint would make a tab here bootstrap again. Each file
// gets its own namespace; channels within one file still hear each other, as two tabs do.
const fileScope = crypto.randomUUID();

globalThis.BroadcastChannel = class FileBroadcastChannel extends BroadcastChannel {
  constructor(name: string) {
    super(`${fileScope}/${name}`);
  }
};

// /site is shared for the page's life; each test starts with nothing read.
beforeEach(forgetSite);

afterEach(() => cleanup());
