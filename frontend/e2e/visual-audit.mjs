// Visual audit: renders every page at several viewports and both themes,
// saves screenshots and reports horizontal overflow and console errors.
// Usage: node e2e/visual-audit.mjs <outDir> [baseURL]
import { chromium } from "@playwright/test";
import fs from "node:fs";
import path from "node:path";

const out = process.argv[2] ?? "screenshots";
const base = process.argv[3] ?? "http://localhost:5173";
const email = process.env.AUDIT_EMAIL ?? "admin@example.com";
const password = process.env.AUDIT_PASSWORD ?? "";
fs.mkdirSync(out, { recursive: true });

const viewports = [
  { name: "mobile-375", width: 375, height: 812 },
  { name: "mobile-390", width: 390, height: 844 },
  { name: "tablet-768", width: 768, height: 1024 },
  { name: "laptop-1366", width: 1366, height: 768 },
  { name: "desktop-1440", width: 1440, height: 900 },
];

const browser = await chromium.launch({ channel: process.env.PW_CHANNEL ?? "chrome" });
const problems = [];

async function login(page) {
  await page.goto(`${base}/login`);
  await page.getByLabel("Email").fill(email);
  await page.getByLabel("Password").fill(password);
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(`${base}/`);
}

const ctx0 = await browser.newContext({ viewport: { width: 1440, height: 900 } });
const p0 = await ctx0.newPage();
await p0.goto(`${base}/login`);
await p0.screenshot({ path: path.join(out, "login-desktop.png") });
await login(p0);
// Discover a job, worker and a dead-letter run to visit detail pages.
const jobs = await p0.evaluate(() =>
  fetch("/api/v1/jobs?page_size=50&sort=name&order=asc").then((r) => r.json()),
);
const workers = await p0.evaluate(() => fetch("/api/v1/workers").then((r) => r.json()));
const storage = await ctx0.storageState();
await ctx0.close();

const job = jobs.data.find((j) => j.schedule_type === "cron") ?? jobs.data[0];
const pages = [
  ["overview", "/"],
  ["jobs", "/jobs"],
  ["job-new", "/jobs/new"],
  ["job-detail", job ? `/jobs/${job.id}` : "/jobs"],
  ["schedules", "/schedules"],
  ["executions", "/executions"],
  ["workers", "/workers"],
  ["worker-detail", workers.data[0] ? `/workers/${workers.data[0].id}` : "/workers"],
  ["dead-letter", "/dead-letter"],
  ["health", "/health"],
  ["settings", "/settings"],
  ["settings-audit", "/settings?tab=audit"],
];

for (const theme of ["light", "dark"]) {
  for (const vp of viewports) {
    const ctx = await browser.newContext({
      viewport: { width: vp.width, height: vp.height },
      storageState: storage,
      colorScheme: theme,
    });
    await ctx.addInitScript((t) => localStorage.setItem("tf-theme", t), theme);
    const page = await ctx.newPage();
    page.on(
      "console",
      (m) => m.type() === "error" && problems.push(`[console ${vp.name} ${theme}] ${m.text()}`),
    );
    page.on("pageerror", (e) => problems.push(`[pageerror ${vp.name} ${theme}] ${e.message}`));
    for (const [name, url] of pages) {
      // Only capture every page at desktop/mobile in light; dark at key sizes.
      if (theme === "dark" && !["mobile-390", "desktop-1440"].includes(vp.name)) continue;
      await page.goto(base + url);
      // The SSE stream keeps a connection open, so "networkidle" never fires.
      // Wait for skeletons to disappear instead.
      await page
        .waitForFunction(() => !document.querySelector(".animate-pulse"), null, { timeout: 8000 })
        .catch(() => {});
      await page.waitForTimeout(300);
      const overflow = await page.evaluate(
        () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
      );
      if (overflow > 1)
        problems.push(
          `[overflow ${vp.name} ${theme}] ${name}: page is ${overflow}px wider than the viewport`,
        );
      await page.screenshot({
        path: path.join(out, `${name}-${vp.name}-${theme}.png`),
        fullPage: vp.width >= 768,
      });
    }
    await ctx.close();
  }
}
await browser.close();
fs.writeFileSync(path.join(out, "problems.txt"), problems.join("\n") || "none");
console.log(problems.length ? problems.join("\n") : "No overflow or console errors detected.");
