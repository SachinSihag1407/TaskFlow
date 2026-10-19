import { describe, expect, it } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { server } from "@/test/server";
import { renderPage } from "@/test/render";
import type { ActionState, JobAction, Me } from "@/lib/types";
import { StatusBadge } from "@/components/ui/badge";
import { JobActions } from "@/components/jobs/JobActions";
import { ExecutionTimeline } from "@/components/runs/ExecutionTimeline";

function me(role: Me["user"]["role"]): Me {
  return {
    user: {
      id: "u1",
      tenant_id: "t1",
      email: "a@example.com",
      name: "A",
      role,
      notifications_seen_at: null,
      last_login_at: null,
      disabled_at: null,
      created_at: "",
    },
    workspace: { id: "t1", name: "WS", created_at: "" },
    csrf_token: "csrf",
    platform_admin: false,
  };
}

const allowAll = Object.fromEntries(
  ["edit", "pause", "resume", "cancel", "retry", "run_now"].map((a) => [a, { allowed: true }]),
) as Record<JobAction, ActionState>;

describe("StatusBadge", () => {
  it("renders the human label for a status", () => {
    renderPage(<StatusBadge status="RETRY_WAIT" />);
    expect(screen.getByText("Retry wait")).toBeInTheDocument();
  });
});

describe("JobActions", () => {
  it("disables write actions for viewers and explains why", async () => {
    server.use(http.get("/api/v1/auth/me", () => HttpResponse.json({ data: me("viewer") })));
    renderPage(
      <JobActions jobId="j1" jobName="Report" status="SCHEDULED" actions={allowAll} activeRuns={0} />,
    );
    const edit = await screen.findByRole("button", { name: "Edit" });
    expect(edit).toBeDisabled();
    expect(screen.getAllByLabelText(/read-only/i).length).toBeGreaterThan(0);
  });

  it("shows the server's reason for unavailable actions in the menu", async () => {
    server.use(http.get("/api/v1/auth/me", () => HttpResponse.json({ data: me("operator") })));
    const actions = {
      ...allowAll,
      pause: { allowed: false, reason: "The job is executing. Cancel it instead." },
    };
    renderPage(<JobActions jobId="j1" jobName="Report" status="RUNNING" actions={actions} activeRuns={1} />);
    await screen.findByRole("button", { name: "Edit" });
    await userEvent.click(screen.getByRole("button", { name: "More actions" }));
    const menu = await screen.findByRole("menu");
    expect(within(menu).getByText("The job is executing. Cancel it instead.")).toBeInTheDocument();
  });

  it("confirms before cancelling and calls the API", async () => {
    let called = false;
    server.use(
      http.get("/api/v1/auth/me", () => HttpResponse.json({ data: me("admin") })),
      http.post("/api/v1/jobs/j1/cancel", () => {
        called = true;
        return HttpResponse.json({ data: {} });
      }),
    );
    renderPage(
      <JobActions jobId="j1" jobName="Report" status="SCHEDULED" actions={allowAll} activeRuns={2} />,
    );
    await screen.findByRole("button", { name: "Edit" });
    await userEvent.click(screen.getByRole("button", { name: "More actions" }));
    await userEvent.click(await screen.findByRole("menuitem", { name: /Cancel job/ }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText(/2 active run/)).toBeInTheDocument();
    expect(called).toBe(false);
    await userEvent.click(within(dialog).getByRole("button", { name: "Cancel job" }));
    await waitFor(() => expect(called).toBe(true));
  });
});

describe("ExecutionTimeline", () => {
  it("explains failures and what happens next", () => {
    renderPage(
      <ExecutionTimeline
        attempts={[
          {
            id: "a1",
            run_id: "r",
            job_id: "j",
            attempt_number: 1,
            worker_id: "w",
            handler: "h",
            status: "LOST",
            queued_at: null,
            started_at: "2026-01-01T00:00:00Z",
            finished_at: "2026-01-01T00:00:30Z",
            duration_ms: 30000,
            error_kind: "lease_expired",
            error_message: "worker stopped renewing",
            retryable: true,
            next_retry_at: "2026-01-01T00:01:00Z",
            max_attempts: 2,
          },
          {
            id: "a2",
            run_id: "r",
            job_id: "j",
            attempt_number: 2,
            worker_id: "w",
            handler: "h",
            status: "FAILED",
            queued_at: null,
            started_at: "2026-01-01T00:01:00Z",
            finished_at: "2026-01-01T00:01:01Z",
            duration_ms: 1000,
            error_kind: "permanent_error",
            error_message: "bad input",
            retryable: false,
            next_retry_at: null,
            max_attempts: 2,
          },
        ]}
      />,
    );
    expect(screen.getByText("Worker lost")).toBeInTheDocument();
    expect(screen.getByText("Retried after backoff")).toBeInTheDocument();
    expect(screen.getByText(/moved to the dead-letter queue/)).toBeInTheDocument();
  });
});
