import { describe, expect, it, vi } from "vitest";
import { http, HttpResponse } from "msw";
import { server } from "@/test/server";
import { ApiError, onUnauthorized, qs, request, setCsrfToken } from "./api";
import { formatDuration, formatSeconds, plural, relativeTime } from "./format";
import { passwordProblem } from "./validation";
import { statusMeta } from "./status";

describe("format", () => {
  it("formats durations across magnitudes", () => {
    expect(formatDuration(null)).toBe("—");
    expect(formatDuration(450)).toBe("450 ms");
    expect(formatDuration(2500)).toBe("2.50 s");
    expect(formatDuration(125_000)).toBe("2m 5s");
    expect(formatDuration(3_900_000)).toBe("1h 5m");
  });
  it("formats seconds compactly", () => {
    expect(formatSeconds(30)).toBe("30s");
    expect(formatSeconds(600)).toBe("10m");
    expect(formatSeconds(7200)).toBe("2h");
  });
  it("pluralises counts", () => {
    expect(plural(1, "run")).toBe("1 run");
    expect(plural(3, "run")).toBe("3 runs");
  });
  it("renders relative times", () => {
    const now = Date.parse("2026-01-01T12:00:00Z");
    expect(relativeTime("2026-01-01T11:59:58Z", now)).toBe("just now");
    expect(relativeTime(null)).toBe("—");
    expect(relativeTime("2026-01-01T11:00:00Z", now)).toMatch(/1 hr|hour/);
  });
});

describe("validation", () => {
  it("mirrors the server password policy", () => {
    expect(passwordProblem("short1")).not.toBeNull();
    expect(passwordProblem("onlyletters")).not.toBeNull();
    expect(passwordProblem("correct-horse")).toBeNull();
  });
});

describe("status", () => {
  it("maps every status to a consistent tone", () => {
    expect(statusMeta("SUCCEEDED").tone).toBe("success");
    expect(statusMeta("FAILED").tone).toBe("danger");
    expect(statusMeta("RUNNING").live).toBe(true);
    expect(statusMeta("UNKNOWN").label).toBe("UNKNOWN");
  });
});

describe("api client", () => {
  it("builds query strings without empty values", () => {
    expect(qs({ a: "1", b: "", c: undefined, d: ["x", "y"], e: [] })).toBe("?a=1&d=x%2Cy");
    expect(qs({})).toBe("");
  });

  it("sends the CSRF token on writes but not on reads", async () => {
    const seen: Record<string, string | null> = {};
    server.use(
      http.get("/api/v1/thing", ({ request }) => {
        seen.get = request.headers.get("X-CSRF-Token");
        return HttpResponse.json({ data: 1 });
      }),
      http.post("/api/v1/thing", ({ request }) => {
        seen.post = request.headers.get("X-CSRF-Token");
        return HttpResponse.json({ data: 2 });
      }),
    );
    setCsrfToken("tok-123");
    await request("/thing");
    await request("/thing", { method: "POST", body: {} });
    expect(seen.get).toBeNull();
    expect(seen.post).toBe("tok-123");
    setCsrfToken(null);
  });

  it("surfaces server error envelopes as ApiError", async () => {
    server.use(
      http.post("/api/v1/jobs", () =>
        HttpResponse.json(
          {
            error: {
              code: "validation_failed",
              message: "Some fields are invalid.",
              fields: { name: "Name is required." },
              request_id: "rid-1",
            },
          },
          { status: 422 },
        ),
      ),
    );
    const err = await request("/jobs", { method: "POST", body: {} }).catch((e) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect(err.status).toBe(422);
    expect(err.fields.name).toBe("Name is required.");
    expect(err.requestId).toBe("rid-1");
  });

  it("notifies listeners on 401 unless quiet", async () => {
    server.use(
      http.get("/api/v1/private", () =>
        HttpResponse.json({ error: { code: "unauthenticated", message: "x" } }, { status: 401 }),
      ),
    );
    const fn = vi.fn();
    const off = onUnauthorized(fn);
    await request("/private").catch(() => {});
    await request("/private", { quiet401: true }).catch(() => {});
    off();
    expect(fn).toHaveBeenCalledTimes(1);
  });
});
