import { defineConfig, devices } from "@playwright/test";

// End-to-end tests run against a real stack (API, scheduler, workers,
// PostgreSQL, Redis): `docker compose up` or the dev setup in docs/deployment.md.
// E2E_BASE_URL selects the frontend; PW_CHANNEL=chrome uses an installed Chrome.
export default defineConfig({
  testDir: "./e2e",
  testMatch: "**/*.spec.ts",
  timeout: 60_000,
  expect: { timeout: 10_000 },
  fullyParallel: true,
  workers: process.env.CI ? 2 : 3,
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? [["github"], ["html", { open: "never" }]] : [["list"]],
  use: {
    baseURL: process.env.E2E_BASE_URL ?? "http://localhost:5173",
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
    channel: process.env.PW_CHANNEL || undefined,
  },
  // Responsive tests set their own viewport sizes (375 to 1366px).
  projects: [{ name: "chrome", use: { ...devices["Desktop Chrome"], viewport: { width: 1440, height: 900 } } }],
});
