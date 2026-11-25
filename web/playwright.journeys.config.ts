import { defineConfig } from "@playwright/test";
import e2e from "./playwright.config";

export default defineConfig({
  ...e2e,
  timeout: 300_000,
  outputDir: process.env.E2E_OUTPUT_DIR ?? "test-results/journeys",
  projects: [
    { name: "seed", testMatch: /seed\.setup\.ts/ },
    { name: "journeys", use: { trace: "off" }, testMatch: /performance\.journey\.ts/, dependencies: ["seed"] },
  ],
});
