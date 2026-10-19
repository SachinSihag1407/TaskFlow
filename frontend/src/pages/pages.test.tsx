import { describe, expect, it } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { server } from "@/test/server";
import { renderPage } from "@/test/render";
import LoginPage from "./auth/LoginPage";
import JobFormPage from "./jobs/JobFormPage";

const meAdmin = {
  user: {
    id: "u1",
    tenant_id: "t1",
    email: "a@example.com",
    name: "A",
    role: "admin",
    notifications_seen_at: null,
    last_login_at: null,
    disabled_at: null,
    created_at: "",
  },
  workspace: { id: "t1", name: "WS", created_at: "" },
  csrf_token: "csrf",
};

describe("LoginPage", () => {
  it("shows the server's error for bad credentials", async () => {
    server.use(
      http.get("/api/v1/auth/me", () =>
        HttpResponse.json({ error: { code: "unauthenticated", message: "x" } }, { status: 401 }),
      ),
      http.post("/api/v1/auth/login", () =>
        HttpResponse.json(
          { error: { code: "invalid_credentials", message: "Incorrect email or password." } },
          { status: 401 },
        ),
      ),
    );
    renderPage(<LoginPage />, { path: "/login", route: "/login" });
    await userEvent.type(screen.getByLabelText("Email"), "a@example.com");
    await userEvent.type(screen.getByLabelText("Password"), "wrong-password");
    await userEvent.click(screen.getByRole("button", { name: "Sign in" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("Incorrect email or password.");
  });

  it("requires both fields before calling the API", async () => {
    server.use(
      http.get("/api/v1/auth/me", () =>
        HttpResponse.json({ error: { code: "unauthenticated", message: "x" } }, { status: 401 }),
      ),
    );
    renderPage(<LoginPage />, { path: "/login", route: "/login" });
    await userEvent.click(screen.getByRole("button", { name: "Sign in" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("Enter your email and password.");
  });
});

describe("JobFormPage", () => {
  it("validates fields beside the inputs, then reviews and creates with an idempotency key", async () => {
    let body: Record<string, unknown> | null = null;
    let idem: string | null = null;
    server.use(
      http.get("/api/v1/auth/me", () => HttpResponse.json({ data: meAdmin })),
      http.post("/api/v1/jobs", async ({ request }) => {
        body = (await request.json()) as Record<string, unknown>;
        idem = request.headers.get("Idempotency-Key");
        return HttpResponse.json({ data: { id: "new-job", next_run_at: null } }, { status: 201 });
      }),
    );
    renderPage(<JobFormPage />, { path: "/jobs/new", route: "/jobs/new" });
    const review = await screen.findByRole("button", { name: "Review" });
    await userEvent.click(review);
    expect(await screen.findByText("Name is required.")).toBeInTheDocument();
    expect(screen.getByText("Choose a handler.")).toBeInTheDocument();

    await userEvent.type(screen.getByLabelText(/^Name/), "My job");
    await userEvent.click(screen.getByRole("radio", { name: /Long-running test job/ }));
    // Selecting a handler fills its example payload.
    expect((screen.getByLabelText(/Payload/) as HTMLTextAreaElement).value).toContain("duration_ms");
    await userEvent.click(screen.getByRole("button", { name: "Review" }));
    expect(await screen.findByText("Check the configuration before saving.")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Create job" }));

    await waitFor(() => expect(body).not.toBeNull());
    expect(body).toMatchObject({
      name: "My job",
      handler: "test.sleep",
      schedule_type: "once",
      max_attempts: 3,
      payload: { duration_ms: 1000 },
    });
    expect(idem).toBeTruthy();
  });

  it("rejects invalid JSON payloads", async () => {
    server.use(http.get("/api/v1/auth/me", () => HttpResponse.json({ data: meAdmin })));
    renderPage(<JobFormPage />, { path: "/jobs/new", route: "/jobs/new" });
    await userEvent.type(await screen.findByLabelText(/^Name/), "x");
    await userEvent.click(screen.getByRole("radio", { name: /Long-running test job/ }));
    const payload = screen.getByLabelText(/Payload/);
    await userEvent.clear(payload);
    await userEvent.type(payload, "[[1");
    await userEvent.click(screen.getByRole("button", { name: "Review" }));
    expect(await screen.findByText(/Invalid JSON/)).toBeInTheDocument();
  });
});
