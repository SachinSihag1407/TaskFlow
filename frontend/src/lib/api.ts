import type { PageMeta } from "./types";

/** Error returned by the API, carrying the server's code, field errors and request id. */
export class ApiError extends Error {
  constructor(
    public status: number,
    public code: string,
    message: string,
    public fields: Record<string, string> = {},
    public requestId?: string,
  ) {
    super(message);
    this.name = "ApiError";
  }

  get isNetwork() {
    return this.status === 0;
  }
}

let csrfToken: string | null = null;

/** Set after login/register/me; sent on every state-changing request. */
export function setCsrfToken(token: string | null) {
  csrfToken = token;
}

type Listener = () => void;
const unauthorizedListeners = new Set<Listener>();
/** Subscribe to 401 responses (session expired / revoked). */
export function onUnauthorized(fn: Listener) {
  unauthorizedListeners.add(fn);
  return () => {
    unauthorizedListeners.delete(fn);
  };
}

export interface RequestOptions {
  method?: "GET" | "POST" | "PUT" | "PATCH" | "DELETE";
  body?: unknown;
  headers?: Record<string, string>;
  signal?: AbortSignal;
  /** Skip the global 401 handler (used by the session probe and login). */
  quiet401?: boolean;
}

export interface Envelope<T> {
  data: T;
  meta?: PageMeta;
}

const BASE = "/api/v1";

/** Performs a request against the TaskFlow API and unwraps the response envelope. */
export async function request<T>(path: string, opts: RequestOptions = {}): Promise<Envelope<T>> {
  const method = opts.method ?? "GET";
  const headers: Record<string, string> = { Accept: "application/json", ...opts.headers };
  if (opts.body !== undefined) headers["Content-Type"] = "application/json";
  if (method !== "GET" && csrfToken) headers["X-CSRF-Token"] = csrfToken;

  let res: Response;
  try {
    res = await fetch(BASE + path, {
      method,
      headers,
      body: opts.body !== undefined ? JSON.stringify(opts.body) : undefined,
      credentials: "same-origin",
      signal: opts.signal,
    });
  } catch (e) {
    if ((e as Error).name === "AbortError") throw e;
    throw new ApiError(0, "network_error", "Cannot reach the TaskFlow API. Check your connection.");
  }

  if (res.status === 204) return { data: undefined as T };
  let payload: unknown = null;
  const text = await res.text();
  if (text) {
    try {
      payload = JSON.parse(text);
    } catch {
      payload = null;
    }
  }
  if (!res.ok) {
    const err = (
      payload as {
        error?: { code?: string; message?: string; fields?: Record<string, string>; request_id?: string };
      }
    )?.error;
    if (res.status === 401 && !opts.quiet401) unauthorizedListeners.forEach((fn) => fn());
    throw new ApiError(
      res.status,
      err?.code ?? "http_error",
      err?.message ?? `Request failed with status ${res.status}.`,
      err?.fields ?? {},
      err?.request_id ?? res.headers.get("X-Request-ID") ?? undefined,
    );
  }
  return payload as Envelope<T>;
}

export const api = {
  get: <T>(path: string, signal?: AbortSignal) => request<T>(path, { signal }).then((r) => r.data),
  page: <T>(path: string, signal?: AbortSignal) => request<T[]>(path, { signal }) as Promise<Envelope<T[]>>,
  post: <T>(path: string, body?: unknown, headers?: Record<string, string>) =>
    request<T>(path, { method: "POST", body, headers }).then((r) => r.data),
  put: <T>(path: string, body: unknown, headers?: Record<string, string>) =>
    request<T>(path, { method: "PUT", body, headers }).then((r) => r.data),
  patch: <T>(path: string, body: unknown) => request<T>(path, { method: "PATCH", body }).then((r) => r.data),
};

/** Builds a query string, skipping empty values. */
export function qs(params: Record<string, string | number | boolean | string[] | undefined | null>): string {
  const sp = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v === undefined || v === null || v === "" || (Array.isArray(v) && v.length === 0)) continue;
    sp.set(k, Array.isArray(v) ? v.join(",") : String(v));
  }
  const s = sp.toString();
  return s ? `?${s}` : "";
}

/** Human-readable message for any thrown value. */
export function errorMessage(e: unknown): string {
  if (e instanceof ApiError) return e.message;
  if (e instanceof Error) return e.message;
  return "Something went wrong.";
}
