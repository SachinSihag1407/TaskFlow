"""The HTTP API: routing, authentication, authorization, rate limits and
request/response translation. Business logic lives in taskflow.services."""

from __future__ import annotations

import asyncio
import contextlib
import hashlib
import hmac
from dataclasses import dataclass
from datetime import datetime
from typing import Any
from uuid import UUID

from fastapi import Depends, FastAPI, Request
from fastapi.exceptions import RequestValidationError
from fastapi.openapi.docs import get_swagger_ui_html
from fastapi.responses import Response, StreamingResponse
from starlette.exceptions import HTTPException as StarletteHTTPException

from .. import __version__, metrics
from ..config import Config
from ..domain import Conflict, Forbidden, NotFound, Principal, Role, Unauthorized, User, ValidationFailed
from ..engine import Engine
from ..handlers import Registry
from ..logs import Log
from ..queue import Queue
from ..ratelimit import Limiter
from ..services import Input, capitalize
from ..services.auth import (
    Auth,
    InvalidCredentials,
    IssuedSession,
    LoginInput,
    PasswordInput,
    RegisterInput,
    UpdateUserInput,
    UserInput,
)
from ..services.jobs import BulkInput, BulkResult, JobInput, Jobs, PreviewInput
from ..services.ops import Ops, SettingsInput
from ..statemachine import InvalidTransition
from ..store import Page
from ..store.attempts import AttemptFilter
from ..store.events import AuditFilter, EventFilter
from ..store.jobs import JobFilter
from ..store.runs import RunFilter
from . import respond
from .middleware import SAFE_METHODS, BaseMiddleware, is_api_path
from .respond import ApiError, created, decode, error_body, json_response, no_content, ok, paged, parse_uuid
from .sse import Hub, stream
from .static import StaticSite

SESSION_COOKIE = "tf_session"
CSRF_HEADER = "x-csrf-token"
WRITERS = {Role.ADMIN, Role.OPERATOR}
ADMINS = {Role.ADMIN}


@dataclass
class Deps:
    cfg: Config
    log: Log
    pool: Any
    q: Queue | None
    eng: Engine
    jobs: Jobs
    auth: Auth
    ops: Ops
    registry: Registry


# ---- query helpers -----------------------------------------------------------


def q_int(request: Request, key: str, default: int) -> int:
    try:
        return int(request.query_params.get(key, ""))
    except ValueError:
        return default


def q_str(request: Request, key: str, max_len: int) -> str:
    return request.query_params.get(key, "").strip()[:max_len]


def q_list(request: Request, key: str) -> list[str]:
    out = [p.strip() for v in request.query_params.getlist(key) for p in v.split(",")]
    return [p for p in out if p and len(p) <= 64][:20]


def q_uuid(request: Request, key: str) -> UUID | None:
    try:
        return UUID(request.query_params[key])
    except (KeyError, ValueError):
        return None


def q_time(request: Request, key: str) -> datetime | None:
    v = request.query_params.get(key)
    if not v:
        return None
    try:
        t = datetime.fromisoformat(v.replace("Z", "+00:00"))
    except ValueError:
        return None
    return t if t.tzinfo else None


def q_page(request: Request) -> Page:
    return Page(q_int(request, "page", 1), q_int(request, "page_size", 25)).normalize()


def create_app(d: Deps) -> FastAPI:
    cfg, log = d.cfg, d.log
    limiter = Limiter(d.q.rdb if d.q else None, d.q.prefix if d.q else "taskflow")
    hub = Hub(d.q, log)
    static = StaticSite(cfg.static_dir) if cfg.static_dir else None

    @contextlib.asynccontextmanager
    async def lifespan(_app: FastAPI):
        task = asyncio.create_task(hub.run())
        try:
            yield
        finally:
            task.cancel()
            await asyncio.gather(task, return_exceptions=True)

    app = FastAPI(title="TaskFlow API", version=__version__, lifespan=lifespan, openapi_url="/api/openapi.json",
                  docs_url=None, redoc_url=None,
                  description="Distributed job scheduling and execution. Authenticate with the session cookie "
                              "from POST /api/v1/auth/login and send X-CSRF-Token on unsafe methods.")
    app.state.hub = hub
    app.state.limiter = limiter

    # ---- errors --------------------------------------------------------------

    def fail(request: Request, status: int, code: str, message: str, fields=None, headers=None) -> Response:
        return json_response(status, error_body(code, message, respond.request_id(request), fields), headers)

    @app.exception_handler(ApiError)
    async def _api_error(request: Request, e: ApiError):
        return fail(request, e.status, e.code, e.message, e.fields, e.headers)

    @app.exception_handler(ValidationFailed)
    async def _validation(request: Request, e: ValidationFailed):
        return fail(request, 422, "validation_failed", "Some fields are invalid.", e.fields)

    @app.exception_handler(Conflict)
    async def _conflict(request: Request, e: Conflict):
        return fail(request, 409, e.code, capitalize(e.message) + ".")

    @app.exception_handler(InvalidTransition)
    async def _transition(request: Request, e: InvalidTransition):
        return fail(request, 409, "invalid_transition", str(e))

    @app.exception_handler(NotFound)
    async def _not_found(request: Request, _e: NotFound):
        return fail(request, 404, "not_found", "Resource not found.")

    @app.exception_handler(Forbidden)
    async def _forbidden(request: Request, _e: Forbidden):
        return fail(request, 403, "forbidden", "You do not have permission to perform this action.")

    @app.exception_handler(Unauthorized)
    async def _unauthorized(request: Request, _e: Unauthorized):
        return fail(request, 401, "unauthenticated", "Sign in to continue.")

    @app.exception_handler(StarletteHTTPException)
    async def _http(request: Request, e: StarletteHTTPException):
        if e.status_code == 405:
            return fail(request, 405, "method_not_allowed", "Method not allowed.")
        return fail(request, e.status_code, "not_found" if e.status_code == 404 else "http_error",
                    "No such endpoint." if e.status_code == 404 else str(e.detail))

    @app.exception_handler(RequestValidationError)
    async def _request_validation(request: Request, _e: RequestValidationError):
        return fail(request, 400, "invalid_request", "The request is malformed.")

    # ---- auth dependencies ---------------------------------------------------

    def ip(request: Request) -> str:
        return request.scope.get("state", {}).get("client_ip", "")

    async def resolve(request: Request) -> tuple[Principal, User] | None:
        """The session behind the cookie, if any (cached per request)."""
        state = request.scope.setdefault("state", {})
        if "auth" not in state:
            state["auth"] = None
            token = request.cookies.get(SESSION_COOKIE, "")
            if token:
                try:
                    state["auth"] = await d.auth.authenticate(token, ip(request), request.headers.get("user-agent", ""))
                    state["user_id"] = str(state["auth"][0].user_id)
                except Unauthorized:
                    pass
        return state["auth"]

    def require(roles: set[Role] | None = None):
        async def dep(request: Request) -> Principal:
            res = await resolve(request)
            if res is None:
                raise ApiError(401, "unauthenticated", "Sign in to continue.")
            p = res[0]
            if request.method not in SAFE_METHODS:
                # Synchronizer token bound to the session.
                tok = request.headers.get(CSRF_HEADER, "")
                if not tok or not hmac.compare_digest(tok.encode(), p.csrf_token.encode()):
                    raise ApiError(403, "csrf_failed", "Missing or invalid CSRF token. Reload the page and try again.")
            if roles is not None and p.role not in roles:
                raise ApiError(403, "forbidden", "Your role does not allow this action.")
            return p
        return dep

    authed, writer, admin = require(), require(WRITERS), require(ADMINS)

    async def enforce(key: str, n: int, window: float, message: str = "Too many requests. Try again shortly.") -> None:
        allowed, retry = await limiter.allow(key, n, window)
        if not allowed:
            raise ApiError(429, "rate_limited", message, headers={"Retry-After": str(int(retry) + 1)})

    def limit(name: str, n: int, window: float, by: str):
        async def dep(request: Request) -> None:
            if by == "ip":
                key = ip(request)
            else:
                res = request.scope.get("state", {}).get("auth")
                key = str(res[0].user_id) if res else "anon"
            await enforce(f"{name}:{key}", n, window)
        return dep

    def platform_admin(p: Principal | None) -> bool:
        return p is not None and p.role == Role.ADMIN and p.email.lower() in cfg.platform_admins

    async def body(request: Request, model):
        return await decode(request, model, cfg.max_request_bytes)

    # ---- probes, metrics, docs -----------------------------------------------

    @app.get("/healthz", include_in_schema=False)
    async def healthz():
        return json_response(200, {"status": "ok"})

    @app.get("/readyz", include_in_schema=False)
    async def readyz():
        """Ready only when PostgreSQL is reachable. Redis is optional: the API
        keeps serving (and workers keep executing) without it."""
        try:
            async with d.pool.connection(timeout=2) as conn:
                await asyncio.wait_for(conn.execute("SELECT 1"), 2)
        except Exception:  # noqa: BLE001
            return json_response(503, {"status": "unavailable", "postgres": "down"})
        redis = "disabled"
        if d.q is not None:
            try:
                await asyncio.wait_for(d.q.ping(), 2)
                redis = "up"
            except Exception:  # noqa: BLE001
                redis = "down"
        return json_response(200, {"status": "ready", "postgres": "up", "redis": redis})

    @app.get("/metrics", include_in_schema=False)
    async def metrics_endpoint(request: Request):
        if cfg.metrics_token:
            got = request.headers.get("authorization", "").removeprefix("Bearer ")
            if not hmac.compare_digest(got.encode(), cfg.metrics_token.encode()):
                return fail(request, 401, "unauthenticated", "Metrics require a bearer token.")
        return Response(metrics.exposition(), media_type="text/plain; version=0.0.4; charset=utf-8")

    @app.get("/api/docs", include_in_schema=False)
    async def docs():
        return get_swagger_ui_html(
            openapi_url="/api/openapi.json", title="TaskFlow API",
            swagger_js_url="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5.17.14/swagger-ui-bundle.js",
            swagger_css_url="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5.17.14/swagger-ui.css",
            swagger_favicon_url="data:,", swagger_ui_parameters={"withCredentials": True})

    # ---- auth ----------------------------------------------------------------

    async def session_response(request: Request, sess: IssuedSession, status: int) -> Response:
        ws = await d.ops.tenant(sess.user.tenant_id)
        p = Principal(user_id=sess.user.id, tenant_id=sess.user.tenant_id, email=sess.user.email, role=sess.user.role)
        resp = ok({"user": sess.user, "workspace": ws, "csrf_token": sess.csrf_token,
                   "platform_admin": platform_admin(p)}, status)
        resp.set_cookie(SESSION_COOKIE, sess.token, path="/", expires=sess.expires_at, httponly=True,
                        secure=cfg.cookie_secure, samesite="lax")
        return resp

    @app.get("/api/v1/auth/config", tags=["auth"])
    async def auth_config():
        return ok({"registration_enabled": cfg.allow_registration})

    @app.post("/api/v1/auth/register", tags=["auth"],
              dependencies=[Depends(limit("register", max(1, cfg.register_per_hour), 3600, "ip"))])
    async def register(request: Request):
        if not cfg.allow_registration:
            raise ApiError(403, "registration_disabled",
                           "Self-service registration is disabled. Ask an admin for an account.")
        b = await body(request, RegisterInput)
        sess = await d.auth.register(b, ip(request), request.headers.get("user-agent", ""))
        return await session_response(request, sess, 201)

    @app.post("/api/v1/auth/login", tags=["auth"], dependencies=[Depends(limit("login", 10, 60, "ip"))])
    async def login(request: Request):
        b = await body(request, LoginInput)
        # A second, per-account limit slows down distributed guessing against
        # one account from many IPs. The key is hashed so Redis holds no emails.
        account = hashlib.sha256(b.email.strip().lower().encode()).hexdigest()[:32]
        await enforce(f"login_account:{account}", 20, 900, "Too many sign-in attempts for this account. Try again later.")
        try:
            sess = await d.auth.login(b.email, b.password, ip(request), request.headers.get("user-agent", ""))
        except InvalidCredentials:
            raise ApiError(401, "invalid_credentials", "Incorrect email or password.") from None
        return await session_response(request, sess, 200)

    @app.post("/api/v1/auth/logout", tags=["auth"])
    async def logout(p: Principal = Depends(authed)):
        await d.auth.logout(p)
        resp = no_content()
        resp.delete_cookie(SESSION_COOKIE, path="/", httponly=True, secure=cfg.cookie_secure, samesite="lax")
        return resp

    @app.get("/api/v1/auth/me", tags=["auth"])
    async def me(request: Request, p: Principal = Depends(authed)):
        user = request.scope["state"]["auth"][1]
        ws = await d.ops.tenant(p.tenant_id)
        return ok({"user": user, "workspace": ws, "csrf_token": p.csrf_token, "platform_admin": platform_admin(p)})

    @app.post("/api/v1/auth/password", tags=["auth"],
              dependencies=[Depends(authed), Depends(limit("password", 5, 60, "ip"))])
    async def change_password(request: Request):
        p = request.scope["state"]["auth"][0]
        b = await body(request, PasswordInput)
        await d.auth.change_password(p, b.current_password, b.new_password)
        return no_content()

    # ---- jobs ----------------------------------------------------------------

    @app.get("/api/v1/handlers", tags=["jobs"], dependencies=[Depends(authed)])
    async def list_handlers():
        return ok(d.registry.infos())

    @app.get("/api/v1/jobs", tags=["jobs"])
    async def list_jobs(request: Request, p: Principal = Depends(authed)):
        f = JobFilter(tenant_id=p.tenant_id, query=q_str(request, "q", 200), statuses=q_list(request, "status"),
                      handlers=q_list(request, "handler"), schedule_type=q_str(request, "schedule_type", 16),
                      priority=q_int(request, "priority", 0), sort=q_str(request, "sort", 32),
                      desc=request.query_params.get("order") != "asc", page=q_page(request))
        items, total = await d.jobs.list(p, f)
        return paged(items, f.page, total)

    @app.post("/api/v1/jobs", tags=["jobs"], status_code=201,
              dependencies=[Depends(writer), Depends(limit("job_create", 3000, 60, "user"))])
    async def create_job(request: Request):
        p = request.scope["state"]["auth"][0]
        b = await body(request, JobInput)
        try:
            job, replayed = await d.jobs.create(p, b, request.headers.get("idempotency-key", ""))
        except Conflict as e:
            if e.code == "idempotency_key_reused":
                raise ApiError(422, e.code, capitalize(e.message) + ".") from None
            raise
        if replayed:
            return ok(job, headers={"Idempotent-Replayed": "true"})
        return created(job, headers={"Location": f"/api/v1/jobs/{job.id}"})

    @app.post("/api/v1/jobs/bulk", tags=["jobs"],
              dependencies=[Depends(writer), Depends(limit("job_bulk", 60, 60, "user"))])
    async def bulk_jobs(request: Request):
        p = request.scope["state"]["auth"][0]
        b = await body(request, BulkInput)
        return ok(await d.jobs.bulk(p, b.action, b.ids))

    @app.get("/api/v1/jobs/{job_id}", tags=["jobs"])
    async def get_job(job_id: str, p: Principal = Depends(authed)):
        return ok(await d.jobs.get(p, parse_uuid(job_id)))

    @app.put("/api/v1/jobs/{job_id}", tags=["jobs"])
    async def update_job(job_id: str, request: Request, p: Principal = Depends(writer)):
        """Requires the caller's last-seen version in If-Match (or ?version=)."""
        jid = parse_uuid(job_id)
        raw = request.headers.get("if-match", "").strip().strip('"W/') or request.query_params.get("version", "")
        try:
            version = int(raw)
        except ValueError:
            version = 0
        if version < 1:
            raise ApiError(428, "version_required", "Send the job's current version in an If-Match header.")
        b = await body(request, JobInput)
        return ok(await d.jobs.update(p, jid, b, version))

    @app.get("/api/v1/jobs/{job_id}/runs", tags=["jobs"])
    async def list_job_runs(job_id: str, request: Request, p: Principal = Depends(authed)):
        jid = parse_uuid(job_id)
        await d.jobs.get(p, jid)  # 404 for jobs outside the workspace
        f = RunFilter(tenant_id=p.tenant_id, job_id=jid, statuses=q_list(request, "status"), page=q_page(request))
        runs, total = await d.ops.runs(p, f)
        return paged(runs, f.page, total)

    for action in ("pause", "resume", "cancel"):
        def make(action: str):
            async def job_action(job_id: str, p: Principal = Depends(writer)):
                op = {"pause": d.eng.pause_job, "resume": d.eng.resume_job, "cancel": d.eng.cancel_job}[action]
                return ok(await op(p, parse_uuid(job_id)))
            job_action.__name__ = f"{action}_job"
            return job_action
        app.post(f"/api/v1/jobs/{{job_id}}/{action}", tags=["jobs"])(make(action))

    @app.post("/api/v1/jobs/{job_id}/retry", tags=["jobs"], status_code=201)
    async def retry_job(job_id: str, p: Principal = Depends(writer)):
        return created(await d.eng.retry_job(p, parse_uuid(job_id)))

    @app.post("/api/v1/jobs/{job_id}/run", tags=["jobs"], status_code=201)
    async def run_now(job_id: str, p: Principal = Depends(writer)):
        return created(await d.eng.run_now(p, parse_uuid(job_id)))

    # ---- schedules -----------------------------------------------------------

    @app.get("/api/v1/schedules", tags=["schedules"])
    async def list_schedules(p: Principal = Depends(authed)):
        return ok(await d.jobs.schedules(p))

    @app.post("/api/v1/schedules/preview", tags=["schedules"],
              dependencies=[Depends(authed), Depends(limit("preview", 120, 60, "user"))])
    async def preview_schedule(request: Request):
        b = await body(request, PreviewInput)
        return ok({"next_runs": d.jobs.preview_schedule(b.cron_expr, b.timezone, b.count)})

    # ---- runs, dead-letter queue, executions ---------------------------------

    @app.get("/api/v1/runs/{run_id}", tags=["runs"])
    async def get_run(run_id: str, p: Principal = Depends(authed)):
        return ok(await d.ops.run(p, parse_uuid(run_id)))

    @app.post("/api/v1/runs/{run_id}/cancel", tags=["runs"])
    async def cancel_run(run_id: str, p: Principal = Depends(writer)):
        return ok(await d.eng.cancel_run(p, parse_uuid(run_id)))

    async def check_replayable(p: Principal, run_id: UUID) -> None:
        """Refuses replays of runs whose handler no longer exists."""
        detail = await d.ops.run(p, run_id)
        if not d.jobs.handler_supported(detail["run"].handler):
            raise Conflict("handler_unavailable", f'handler "{detail["run"].handler}" is no longer available')

    @app.post("/api/v1/runs/{run_id}/replay", tags=["runs"], status_code=201)
    async def replay_run(run_id: str, p: Principal = Depends(writer)):
        rid = parse_uuid(run_id)
        await check_replayable(p, rid)
        return created(await d.eng.replay_run(p, rid))

    @app.get("/api/v1/dead-letter", tags=["runs"])
    async def list_dead_letter(request: Request, p: Principal = Depends(authed)):
        f = RunFilter(tenant_id=p.tenant_id, dead_lettered=True, query=q_str(request, "q", 200),
                      job_id=q_uuid(request, "job_id"), page=q_page(request))
        runs, total = await d.ops.runs(p, f)
        return paged(runs, f.page, total)

    @app.post("/api/v1/dead-letter/replay", tags=["runs"],
              dependencies=[Depends(writer), Depends(limit("dlq_replay", 30, 60, "user"))])
    async def bulk_replay(request: Request):
        p = request.scope["state"]["auth"][0]
        b = await body(request, _IdsInput)
        if not 1 <= len(b.ids) <= 100:
            raise ApiError(422, "validation_failed", "Select between 1 and 100 runs.",
                           {"ids": "Select between 1 and 100 runs."})
        results: list[BulkResult] = []
        for rid in b.ids:
            r = BulkResult(rid, True)
            try:
                await check_replayable(p, rid)
                await d.eng.replay_run(p, rid)
            except Conflict as e:
                r.ok, r.code, r.message = False, e.code, capitalize(e.message) + "."
            except NotFound:
                r.ok, r.code, r.message = False, "not_found", "Run not found."
            except Exception as e:  # noqa: BLE001 - reported per item
                log.error("bulk replay failed", run_id=str(rid), err=str(e))
                r.ok, r.code, r.message = False, "internal_error", "Unexpected error."
            results.append(r)
        return ok(results)

    @app.get("/api/v1/executions", tags=["executions"])
    async def list_executions(request: Request, p: Principal = Depends(authed)):
        f = AttemptFilter(tenant_id=p.tenant_id, job_id=q_uuid(request, "job_id"), run_id=q_uuid(request, "run_id"),
                          worker_id=q_uuid(request, "worker_id"), statuses=q_list(request, "status"),
                          handlers=q_list(request, "handler"), query=q_str(request, "q", 200),
                          since=q_time(request, "since"), until=q_time(request, "until"),
                          sort=q_str(request, "sort", 32), desc=request.query_params.get("order") != "asc",
                          page=q_page(request))
        items, total = await d.ops.attempts(p, f)
        return paged(items, f.page, total)

    @app.get("/api/v1/executions/{attempt_id}", tags=["executions"])
    async def get_execution(attempt_id: str, p: Principal = Depends(authed)):
        return ok(await d.ops.attempt(p, parse_uuid(attempt_id)))

    @app.get("/api/v1/executions/{attempt_id}/logs", tags=["executions"])
    async def execution_logs(attempt_id: str, request: Request, p: Principal = Depends(authed)):
        return ok(await d.ops.logs(p, parse_uuid(attempt_id), max(0, q_int(request, "after", 0))))

    # ---- workers -------------------------------------------------------------

    @app.get("/api/v1/workers", tags=["workers"], dependencies=[Depends(authed)])
    async def list_workers():
        return ok(await d.ops.workers())

    @app.get("/api/v1/workers/{worker_id}", tags=["workers"])
    async def get_worker(worker_id: str, p: Principal = Depends(authed)):
        return ok(await d.ops.worker(p, parse_uuid(worker_id)))

    @app.post("/api/v1/workers/{worker_id}/drain", tags=["workers"], status_code=202)
    async def drain_worker(worker_id: str, p: Principal = Depends(admin)):
        wid = parse_uuid(worker_id)
        if not platform_admin(p):
            raise ApiError(403, "forbidden",
                           "Workers are shared by every workspace; only platform administrators can drain them.")
        await d.ops.drain_worker(p, wid)
        return Response(status_code=202)

    # ---- observability -------------------------------------------------------

    @app.get("/api/v1/analytics/overview", tags=["observability"])
    async def overview(request: Request, p: Principal = Depends(authed)):
        return ok(await d.ops.overview(p, request.query_params.get("range", "")))

    @app.get("/api/v1/system/health", tags=["observability"], dependencies=[Depends(authed)])
    async def system_health():
        h = await d.ops.health()
        return ok({**h, "live_updates": hub.live, "sse_clients": len(hub.clients)})

    @app.get("/api/v1/events", tags=["observability"])
    async def list_events(request: Request, p: Principal = Depends(authed)):
        f = EventFilter(tenant_id=p.tenant_id, job_id=q_uuid(request, "job_id"), run_id=q_uuid(request, "run_id"),
                        worker_id=q_uuid(request, "worker_id"), severities=q_list(request, "severity"),
                        limit=q_int(request, "limit", 50))
        return ok(await d.ops.events(p, f))

    @app.get("/api/v1/events/stream", tags=["observability"])
    async def event_stream(request: Request, p: Principal = Depends(authed)):
        c = hub.add(str(p.user_id), str(p.tenant_id))
        if c is None:
            raise ApiError(503, "too_many_streams", "Too many live connections; falling back to polling.")
        token = request.cookies.get(SESSION_COOKIE, "")
        ua = request.headers.get("user-agent", "")

        async def still_valid() -> bool:
            try:
                await asyncio.wait_for(d.auth.authenticate(token, ip(request), ua), 3)
                return True
            except Exception:  # noqa: BLE001 - revoked, expired, disabled or DB unreachable: end the stream
                return False

        return StreamingResponse(stream(hub, c, still_valid), media_type="text/event-stream",
                                 headers={"Cache-Control": "no-cache", "Connection": "keep-alive",
                                          "X-Accel-Buffering": "no"})

    @app.get("/api/v1/notifications", tags=["observability"])
    async def notifications(request: Request, p: Principal = Depends(authed)):
        user = request.scope["state"]["auth"][1]
        return ok(await d.ops.notifications(p, user.notifications_seen_at))

    @app.post("/api/v1/notifications/seen", tags=["observability"])
    async def notifications_seen(p: Principal = Depends(authed)):
        await d.auth.mark_notifications_seen(p)
        return no_content()

    # ---- workspace administration --------------------------------------------

    @app.get("/api/v1/workspace", tags=["workspace"])
    async def workspace(p: Principal = Depends(authed)):
        return ok(await d.ops.tenant(p.tenant_id))

    @app.get("/api/v1/settings", tags=["workspace"])
    async def get_settings(p: Principal = Depends(authed)):
        return ok(await d.ops.settings(p))

    @app.put("/api/v1/settings", tags=["workspace"])
    async def update_settings(request: Request, p: Principal = Depends(admin)):
        b = await body(request, SettingsInput)
        return ok(await d.ops.update_settings(p, b))

    @app.get("/api/v1/audit", tags=["workspace"])
    async def list_audit(request: Request, p: Principal = Depends(admin)):
        f = AuditFilter(tenant_id=p.tenant_id, action=q_str(request, "action", 64),
                        target_type=q_str(request, "target_type", 32), target_id=q_str(request, "target_id", 64),
                        page=q_page(request))
        items, total = await d.ops.audit(p, f)
        return paged(items, f.page, total)

    @app.get("/api/v1/users", tags=["workspace"])
    async def list_users(p: Principal = Depends(admin)):
        return ok(await d.auth.list_users(p))

    @app.post("/api/v1/users", tags=["workspace"], status_code=201,
              dependencies=[Depends(admin), Depends(limit("user_create", 20, 60, "user"))])
    async def create_user(request: Request):
        p = request.scope["state"]["auth"][0]
        b = await body(request, UserInput)
        return created(await d.auth.create_user(p, b))

    @app.patch("/api/v1/users/{user_id}", tags=["workspace"])
    async def update_user(user_id: str, request: Request, p: Principal = Depends(admin)):
        uid = parse_uuid(user_id)
        b = await body(request, UpdateUserInput)
        return ok(await d.auth.update_user(p, uid, b))

    # ---- SPA and unknown paths -----------------------------------------------

    @app.api_route("/{path:path}", methods=["GET", "HEAD", "POST", "PUT", "PATCH", "DELETE"], include_in_schema=False)
    async def fallback(path: str, request: Request):
        if static is None or is_api_path(request.url.path):
            raise ApiError(404, "not_found", "No such endpoint.")
        if request.method not in ("GET", "HEAD"):
            raise ApiError(405, "method_not_allowed", "Method not allowed.")
        return static.serve(path)

    class _Route:
        """Records the matched route pattern for logs and metrics."""

        def __init__(self, inner, path: str):
            self.inner = inner
            self.path = path

        async def __call__(self, scope, receive, send):
            if scope["type"] == "http":
                scope.setdefault("state", {})["route"] = f"{scope['method']} {self.path}"
            await self.inner(scope, receive, send)

    for r in app.router.routes:
        if hasattr(r, "app") and hasattr(r, "path"):
            r.app = _Route(r.app, "/*" if r.path == "/{path:path}" else r.path)

    app.add_middleware(BaseMiddleware, cfg=cfg, log=log)
    return app


class _IdsInput(Input):
    """{"ids": [...]} for the dead-letter bulk replay."""

    ids: list[UUID] = []
