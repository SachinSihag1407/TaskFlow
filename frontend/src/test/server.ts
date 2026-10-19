import { setupServer } from "msw/node";
import { http, HttpResponse } from "msw";
import type { HandlerInfo, Settings } from "@/lib/types";

export const settings: Settings = {
  default_max_attempts: 3,
  default_timeout_seconds: 300,
  default_backoff_base_seconds: 10,
  default_backoff_max_seconds: 600,
  default_priority: 5,
  default_timezone: "UTC",
  max_concurrent_runs: 0,
  event_retention_days: 30,
};

export const handlers: HandlerInfo[] = [
  {
    name: "test.sleep",
    title: "Long-running test job",
    description: "Sleeps.",
    idempotency: "No side effects.",
    example_payload: { duration_ms: 1000 },
    fields: [{ name: "duration_ms", type: "integer", required: true, description: "How long" }],
  },
];

export const server = setupServer(
  http.get("/api/v1/settings", () => HttpResponse.json({ data: settings })),
  http.get("/api/v1/handlers", () => HttpResponse.json({ data: handlers })),
  http.get("/api/v1/auth/config", () => HttpResponse.json({ data: { registration_enabled: true } })),
);
