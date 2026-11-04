// The dashboard's browser tests: a real `athena server` (TLS off on loopback, fakeclaude as the runtime) started by
// e2e/global-setup.ts, driven in Chromium at the three screenshot widths. One worker: the specs share the server.
// Run with `make e2e`; it is not part of `make check`.
import { defineConfig } from "@playwright/test";

export default defineConfig({
  testDir: "e2e",
  globalSetup: "./e2e/global-setup.ts",
  fullyParallel: false,
  workers: 1,
  forbidOnly: !!process.env.CI,
  retries: 0,
  timeout: 60_000,
  reporter: [["list"]],
  outputDir: "test-results",
  snapshotPathTemplate: "{testDir}/__screenshots__/{testFileName}/{arg}-{projectName}-{platform}{ext}",
  expect: {
    timeout: 10_000,
    toHaveScreenshot: { maxDiffPixelRatio: 0.01, animations: "disabled" },
  },
  use: {
    browserName: "chromium",
    trace: "retain-on-failure",
  },
  projects: [
    { name: "phone", use: { viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true, deviceScaleFactor: 2 } },
    { name: "tablet", use: { viewport: { width: 820, height: 1180 } } },
    { name: "desktop", use: { viewport: { width: 1440, height: 900 } } },
  ],
});
