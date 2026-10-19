import { expect, test } from "@playwright/test";
import { apiFor, signIn, signUp, uniqueEmail } from "./helpers";

test.describe("core workflows", () => {
  test("register, create a job from the form, and watch it execute", async ({ page }) => {
    await signUp(page);
    // Empty workspace shows onboarding, not zeros.
    await expect(page.getByText("Run your first job")).toBeVisible();

    await page.goto("/jobs/new");
    await page.getByLabel(/^Name/).fill("E2E report");
    await page.getByRole("radio", { name: /Generate report/ }).click();
    await expect(page.getByLabel(/Payload/)).toHaveValue(/report_type/);
    await page.getByRole("button", { name: "Review" }).click();
    await expect(page.getByText("Check the configuration before saving.")).toBeVisible();
    await page.getByRole("button", { name: "Create job" }).click();

    // Job details page; a real worker picks it up and the UI updates.
    await expect(page.getByRole("heading", { name: "E2E report" })).toBeVisible();
    await expect(page.getByTestId("job-state")).toContainText("Succeeded", { timeout: 30_000 });
    await expect(page.getByText(/Attempt 1/).first()).toBeVisible();

    // Inspect the attempt's logs.
    await page.getByRole("button", { name: /Attempt 1/ }).click();
    const sheet = page.getByRole("dialog");
    await expect(sheet.getByText("Logs")).toBeVisible();
    await expect(sheet.getByRole("log")).toContainText("aggregated");
    await sheet.getByRole("button", { name: "Close" }).click();

    // It appears in the executions history and links back to the job.
    await page.goto("/executions");
    await page.getByRole("link", { name: "E2E report" }).first().click();
    await expect(page).toHaveURL(/\/jobs\//);
  });

  test("recurring job: cron preview, pause, resume and cancel with confirmation", async ({ page }) => {
    await signUp(page);
    await page.goto("/jobs/new");
    await page.getByLabel(/^Name/).fill("E2E recurring");
    await page.getByRole("radio", { name: /Long-running test job/ }).click();
    await page.getByRole("radio", { name: "Recurring" }).click();
    await page.getByRole("button", { name: "Weekdays at 09:00" }).click();
    await expect(page.getByText("At 09:00, Monday through Friday")).toBeVisible();
    await expect(page.getByText(/Next runs/)).toBeVisible();
    await page.getByRole("button", { name: "Review" }).click();
    await page.getByRole("button", { name: "Create job" }).click();
    await expect(page.getByRole("heading", { name: "E2E recurring" })).toBeVisible();
    await expect(page.getByText("Upcoming")).toBeVisible();

    await page.getByRole("button", { name: "More actions" }).click();
    await page.getByRole("menuitem", { name: "Pause", exact: true }).click();
    await expect(page.getByTestId("job-state")).toContainText("Paused");
    await page.getByRole("button", { name: "Resume" }).click();
    await expect(page.getByTestId("job-state")).toContainText("Scheduled");

    await page.getByRole("button", { name: "More actions" }).click();
    await page.getByRole("menuitem", { name: /Cancel job/ }).click();
    const dialog = page.getByRole("dialog");
    await expect(dialog).toContainText("This cannot be undone");
    await dialog.getByRole("button", { name: "Cancel job" }).click();
    await expect(page.getByTestId("job-state")).toContainText("Cancelled");
    // Unavailable actions explain themselves.
    await page.getByRole("button", { name: "More actions" }).click();
    await expect(page.getByRole("menuitem", { name: /Cancel job/ })).toContainText("already cancelled");
  });

  test("failed job lands in the dead-letter queue and can be replayed once, with an audit trail", async ({
    page,
  }) => {
    await signUp(page);
    const api = await apiFor(page);
    await api.post("/jobs", {
      name: "E2E always fails",
      handler: "test.sleep",
      schedule_type: "once",
      payload: { duration_ms: 10, fail_attempts: 25, permanent: true },
      max_attempts: 1,
    });
    await page.goto("/dead-letter");
    const row = page.getByRole("row", { name: /E2E always fails/ });
    await expect(row).toBeVisible({ timeout: 30_000 });
    await expect(row).toContainText("simulated failure on attempt 1");
    await row.getByRole("button", { name: "Replay" }).click();
    const dialog = page.getByRole("dialog");
    await expect(dialog).toContainText("can be replayed once");
    await dialog.getByRole("button", { name: "Replay" }).click();
    await expect(row).toContainText("Replayed");
    await expect(row.getByRole("button", { name: "Replay" })).toBeDisabled();

    await page.goto("/settings?tab=audit");
    await expect(page.getByRole("cell", { name: "run.replay" }).first()).toBeVisible();
  });

  test("viewers get read-only access with explanations", async ({ page, browser }) => {
    await signUp(page, "Admin");
    const api = await apiFor(page);
    const viewer = uniqueEmail("viewer");
    await api.post("/users", {
      name: "Read Only",
      email: viewer,
      password: "viewer-Password-1",
      role: "viewer",
    });

    const ctx = await browser.newContext();
    const vp = await ctx.newPage();
    await signIn(vp, viewer, "viewer-Password-1");
    await vp.goto("/jobs");
    const newJob = vp.getByRole("button", { name: "New job" });
    await expect(newJob).toBeDisabled();
    await vp.goto("/settings?tab=members");
    await expect(vp.getByText("Admins only")).toBeVisible();
    await ctx.close();
  });

  test("live updates indicator reflects the deployment", async ({ page }) => {
    await signUp(page);
    // Single-process demo deployments run without Redis: the UI must say it
    // is polling instead of pretending to be live.
    const ready = await (await page.request.get("/readyz")).json();
    const expected = ready.redis === "disabled" ? /Live updates: Polling/ : /Live updates: Live/;
    await expect(page.getByRole("status", { name: expected })).toBeVisible({ timeout: 15_000 });
  });

  test("command palette navigates and searches jobs", async ({ page }) => {
    await signUp(page);
    const api = await apiFor(page);
    await api.post("/jobs", {
      name: "Palette target",
      handler: "test.sleep",
      schedule_type: "delayed",
      delay_seconds: 3600,
      payload: { duration_ms: 1 },
    });
    await page.keyboard.press("Control+K");
    await page.getByPlaceholder("Search jobs, pages and actions…").fill("Palette");
    await page.getByRole("option", { name: /Palette target/ }).click();
    await expect(page.getByRole("heading", { name: "Palette target" })).toBeVisible();
  });
});
