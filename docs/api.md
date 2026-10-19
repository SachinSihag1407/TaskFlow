# API

The authoritative reference is the OpenAPI 3 document at `backend/internal/api/openapi.yaml`, served by the API at `/api/openapi.yaml` with Swagger UI at `/api/docs`. `TestOpenAPICoversEveryRoute` fails the build if a registered route is missing from the spec (or the spec documents a route that does not exist). This page covers conventions and common flows.

## Conventions

| Topic | Rule |
|---|---|
| Base path | `/api/v1` |
| Format | JSON in and out; request bodies must be `application/json`, unknown fields are rejected, max 1 MB |
| Success | `{"data": …}`; lists add `"meta": {"page", "page_size", "total", "total_pages"}` |
| Errors | `{"error": {"code", "message", "fields"?, "request_id"}}` |
| Pagination | `?page=1&page_size=25` (max 200) |
| Times | RFC 3339, UTC |
| Request ids | `X-Request-ID` echoed on every response (and accepted from callers) |
| Idempotent creation | `Idempotency-Key` header on `POST /jobs` |
| Optimistic locking | `If-Match: <version>` on `PUT /jobs/{id}` |

### Status codes

| Code | Meaning |
|---|---|
| 200 / 201 / 202 / 204 | success (201 created, 202 accepted for async actions like worker drain) |
| 400 | malformed JSON / unknown field |
| 401 | not signed in (`unauthenticated`) or bad credentials (`invalid_credentials`) |
| 403 | role not allowed (`forbidden`), missing/invalid CSRF token (`csrf_failed`), foreign Origin (`origin_rejected`) |
| 404 | not found **or belongs to another workspace** (existence is not revealed) |
| 409 | not allowed in the current state; `code` says why (`action_not_allowed`, `version_conflict`, `already_replayed`, `run_finished`, `last_admin`, …) and `message` is human-readable |
| 413 | body too large |
| 415 | wrong content type |
| 422 | validation failed; `fields` maps JSON field names to messages; also `idempotency_key_reused` |
| 428 | `If-Match` missing on job update |
| 429 | rate limited; see `Retry-After` |

## Authentication

```bash
# Sign in: sets the HttpOnly tf_session cookie and returns a CSRF token.
curl -c jar -H 'Content-Type: application/json' \
  -d '{"email":"admin@example.com","password":"…"}' \
  http://localhost:8080/api/v1/auth/login
# → {"data":{"user":{…},"workspace":{…},"csrf_token":"…"}}

# Reads need only the cookie.
curl -b jar http://localhost:8080/api/v1/jobs

# Writes also need the CSRF token.
curl -b jar -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' -X POST \
  http://localhost:8080/api/v1/jobs/$ID/pause
```

`GET /auth/me` returns the current user, workspace and CSRF token (used by the SPA after a reload).

## Creating jobs

```bash
curl -b jar -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: nightly-report-v1' \
  -d '{
        "name": "Nightly sales report",
        "handler": "report.generate",
        "payload": {"report_type": "sales_summary", "period_days": 1},
        "schedule_type": "cron",
        "cron_expr": "0 2 * * *",
        "timezone": "Europe/Berlin",
        "missed_run_policy": "run_once",
        "overlap_policy": "skip",
        "max_attempts": 4,
        "backoff_base_seconds": 30,
        "backoff_max_seconds": 900,
        "timeout_seconds": 600,
        "priority": 7
      }' \
  http://localhost:8080/api/v1/jobs
```

- `schedule_type`: `once` (optional `run_at`, default now), `delayed` (`delay_seconds`), or `cron` (`cron_expr` + `timezone`).
- Omitted numeric fields use the workspace defaults (`GET /settings`).
- Repeating the request with the same `Idempotency-Key` returns the original job with `200` and `Idempotent-Replayed: true`; reusing the key with a different body returns `422 idempotency_key_reused`.
- Validation errors name each field:

```json
{"error":{"code":"validation_failed","message":"Some fields are invalid.",
  "fields":{"cron_expr":"Invalid cron expression: …","priority":"Priority must be between 1 and 10."},
  "request_id":"7f0c…"}}
```

Use `POST /schedules/preview` to validate a cron expression and get its next runs in a timezone before saving.

## Job actions and what they mean

`GET /jobs/{id}` returns the job, its recent runs and attempts, timeline events, upcoming occurrences, and an `actions` map:

```json
"actions": {
  "pause":  {"allowed": false, "reason": "The job is executing. Cancel it instead, or wait for it to finish."},
  "cancel": {"allowed": true},
  …
}
```

The same rules are enforced when the action is invoked (a disallowed call returns `409 action_not_allowed` with that reason).

| Endpoint | Effect |
|---|---|
| `POST /jobs/{id}/pause` | stop scheduling new occurrences and hold queued runs |
| `POST /jobs/{id}/resume` | resume; missed occurrences follow the missed-run policy |
| `POST /jobs/{id}/cancel` | stop the schedule, cancel waiting runs, ask running runs to stop |
| `POST /jobs/{id}/retry` | re-run a failed one-time job (replays its last failed run) |
| `POST /jobs/{id}/run` | immediate manual run of a recurring job |
| `POST /jobs/bulk` | `{"action": "pause"|"resume"|"cancel"|"retry", "ids": [...]}` → per-item results |
| `POST /runs/{id}/cancel` | cancel one run (cooperative if running) |
| `POST /runs/{id}/replay` | replay a dead-lettered run once |
| `POST /dead-letter/replay` | replay up to 100 dead-lettered runs → per-item results |

## Endpoint list

| Area | Endpoints |
|---|---|
| Auth | `GET /auth/config`, `POST /auth/register`, `POST /auth/login`, `POST /auth/logout`, `GET /auth/me`, `POST /auth/password` |
| Jobs | `GET /handlers`, `GET/POST /jobs`, `POST /jobs/bulk`, `GET/PUT /jobs/{id}`, `GET /jobs/{id}/runs`, `POST /jobs/{id}/{pause,resume,cancel,retry,run}` |
| Schedules | `GET /schedules`, `POST /schedules/preview` |
| Runs & DLQ | `GET /runs/{id}`, `POST /runs/{id}/cancel`, `POST /runs/{id}/replay`, `GET /dead-letter`, `POST /dead-letter/replay` |
| Executions | `GET /executions`, `GET /executions/{id}`, `GET /executions/{id}/logs?after=` |
| Workers | `GET /workers`, `GET /workers/{id}`, `POST /workers/{id}/drain` (platform admins) |
| Observability | `GET /analytics/overview?range=1h|6h|24h|7d|30d`, `GET /system/health`, `GET /events`, `GET /events/stream` (SSE), `GET /notifications`, `POST /notifications/seen` |
| Workspace | `GET /workspace`, `GET/PUT /settings` (PUT admin), `GET /audit` (admin), `GET/POST /users`, `PATCH /users/{id}` (admin) |
| Probes | `GET /healthz`, `GET /readyz`, `GET /metrics`, `GET /api/openapi.yaml`, `GET /api/docs` |

## Worker registration and heartbeats

Workers are trusted internal processes. They register and heartbeat by writing to PostgreSQL through the same repository code the API uses (`workers` table) rather than over HTTP, so a worker keeps working even if the API is down and there is no worker credential to manage. The API exposes worker state read-only (`GET /workers`, `GET /workers/{id}`) plus the `drain` action, which the worker observes on its next heartbeat. Because workers serve every workspace, draining is restricted to platform administrators (workspace admins listed in `PLATFORM_ADMIN_EMAILS`).

## Server-Sent Events

`GET /api/v1/events/stream` (authenticated) emits:

```
retry: 5000

event: status
data: {"live":true}

event: update
data: {"type":"run.updated","tenant_id":"…","job_id":"…","run_id":"…","status":"SUCCEEDED","at":1791718000000}
```

Events carry identifiers only. Clients should re-fetch the affected resources through the REST API. When `live` is `false` (Redis down), poll instead. A `: keepalive` comment is sent every 15 s.

## Rate limits

| Scope | Limit |
|---|---|
| Login | 10/min per IP, 20 per 15 min per account |
| Registration | 5/hour per IP (configurable via `RATE_LIMIT_REGISTER_PER_HOUR`) |
| Password change | 5/min per IP |
| Job creation | 3000/min per user |
| Bulk job actions | 60/min per user |
| DLQ bulk replay | 30/min per user |
| Schedule preview | 120/min per user |
