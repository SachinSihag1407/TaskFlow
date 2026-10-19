import { expect, test } from "@playwright/test";
import { expectNoHorizontalOverflow, signUp } from "./helpers";

const PAGES = [
  "/",
  "/jobs",
  "/jobs/new",
  "/schedules",
  "/executions",
  "/workers",
  "/dead-letter",
  "/health",
  "/settings",
];

for (const width of [375, 768, 1366]) {
  test(`no horizontal overflow at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 });
    await signUp(page);
    for (const path of PAGES) {
      await page.goto(path);
      await expect(page.locator("main h1")).toBeVisible();
      await expectNoHorizontalOverflow(page);
    }
  });
}

test("mobile uses a navigation drawer", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await signUp(page);
  // The desktop sidebar is hidden on phones.
  await expect(page.getByRole("button", { name: "Collapse sidebar" })).toBeHidden();
  await page.getByRole("button", { name: "Open navigation" }).click();
  const drawer = page.getByRole("dialog", { name: "Navigation" });
  await drawer.getByRole("link", { name: "Workers" }).click();
  await expect(page.getByRole("heading", { name: "Workers" })).toBeVisible();
  await expect(drawer).toBeHidden();
});

test("dialogs fit inside a small viewport", async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 667 });
  await signUp(page);
  await page.goto("/settings?tab=members");
  await page.getByRole("button", { name: "Add member" }).click();
  const box = await page.getByRole("dialog").boundingBox();
  expect(box).not.toBeNull();
  expect(box!.x).toBeGreaterThanOrEqual(0);
  expect(box!.x + box!.width).toBeLessThanOrEqual(375);
});
