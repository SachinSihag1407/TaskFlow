import { expect, type APIRequestContext, type Page } from "@playwright/test";

export const PASSWORD = "e2e-Password-123";

export function uniqueEmail(prefix = "e2e") {
  return `${prefix}-${Date.now()}-${Math.random().toString(36).slice(2, 8)}@example.com`;
}

/** Registers a fresh workspace through the UI and lands on the overview. */
export async function signUp(page: Page, name = "E2E User") {
  const email = uniqueEmail();
  await page.goto("/register");
  await page.getByLabel("Your name").fill(name);
  await page.getByLabel("Work email").fill(email);
  await page.getByLabel("Password").fill(PASSWORD);
  await page.getByLabel("Workspace name").fill(`E2E ${Date.now()}`);
  await page.getByRole("button", { name: "Create workspace" }).click();
  await expect(page).toHaveURL("/");
  return email;
}

export async function signIn(page: Page, email: string, password = PASSWORD) {
  await page.goto("/login");
  await page.getByLabel("Email").fill(email);
  await page.getByLabel("Password").fill(password);
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page).toHaveURL("/");
}

/** Authenticated API calls reusing the page's session cookie and CSRF token. */
export async function apiFor(page: Page) {
  const req: APIRequestContext = page.request;
  const me = await (await req.get("/api/v1/auth/me")).json();
  const csrf: string = me.data.csrf_token;
  return {
    async post<T = unknown>(path: string, data?: unknown, headers: Record<string, string> = {}): Promise<T> {
      const r = await req.post(`/api/v1${path}`, { data, headers: { "X-CSRF-Token": csrf, ...headers } });
      expect(r.ok(), `${path}: ${r.status()} ${await r.text()}`).toBeTruthy();
      return (await r.json()).data as T;
    },
    async get<T = unknown>(path: string): Promise<T> {
      const r = await req.get(`/api/v1${path}`);
      expect(r.ok()).toBeTruthy();
      return (await r.json()).data as T;
    },
  };
}

/** Fails the test if the page is wider than the viewport. */
export async function expectNoHorizontalOverflow(page: Page) {
  const overflow = await page.evaluate(
    () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
  );
  expect(overflow, "page should not scroll horizontally").toBeLessThanOrEqual(1);
}
