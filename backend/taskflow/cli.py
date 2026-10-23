"""Process runtimes behind `python -m taskflow` (imported only by commands that need them)."""


from __future__ import annotations

import asyncio
import contextlib
import secrets
import signal

import uvicorn
from fastapi import FastAPI
from fastapi.responses import JSONResponse, Response

from . import __version__, db, metrics
from .config import Config
from .engine import Engine
from .handlers import builtin
from .logs import Log, new_logger
from .queue import Queue
from .scheduler import Scheduler
from .worker import Worker, WorkerConfig


class _Server(uvicorn.Server):
    """uvicorn without its own signal handling: shutdown is coordinated here
    so the worker can drain before the process exits."""

    @contextlib.contextmanager
    def capture_signals(self):
        yield

    def install_signal_handlers(self) -> None:  # older uvicorn releases
        pass


def _stop_event() -> asyncio.Event:
    """Set on SIGINT/SIGTERM."""
    stop = asyncio.Event()
    loop = asyncio.get_running_loop()
    for sig in (signal.SIGINT, signal.SIGTERM):
        with contextlib.suppress(NotImplementedError, RuntimeError):  # not supported on Windows event loops
            loop.add_signal_handler(sig, stop.set)
    return stop


class Runtime:
    """Shared dependencies of a process."""

    def __init__(self, cfg: Config, log: Log, pool, q: Queue | None):
        self.cfg, self.log, self.pool, self.q = cfg, log, pool, q
        self.eng = Engine(pool, q, log)
        self.registry = builtin(pool, cfg.allow_private_webhooks, cfg.webhook_max_resp_bytes).subset(cfg.enabled_handlers)

    @classmethod
    async def start(cls, component: str) -> Runtime:
        cfg = Config.load()
        log = new_logger(cfg.log_level, component).with_(version=__version__)
        builtin(None).subset(cfg.enabled_handlers)  # fail fast on unknown ENABLED_HANDLERS
        pool = await db.connect(cfg.database_url, cfg.db_max_conns, retries=30, log=log)
        q = None
        if cfg.redis_disabled:
            log.info("redis disabled; workers poll PostgreSQL and the UI polls for updates")
        else:
            q = Queue(cfg.redis_url)
            try:
                await asyncio.wait_for(q.ping(), 3)
            except Exception as e:  # noqa: BLE001 - every component degrades gracefully without Redis
                log.warning("redis unavailable at startup; continuing in degraded mode", err=str(e))
        return cls(cfg, log, pool, q)

    async def close(self) -> None:
        if self.q is not None:
            await self.q.close()
        await self.pool.close()

    def api_app(self) -> FastAPI:
        from .api.app import Deps, create_app
        from .services.auth import Auth
        from .services.jobs import Jobs
        from .services.ops import Ops

        return create_app(Deps(
            cfg=self.cfg, log=self.log, pool=self.pool, q=self.q, eng=self.eng,
            jobs=Jobs(self.pool, self.eng, self.registry, self.log),
            auth=Auth(self.pool, self.eng, self.cfg.session_ttl),
            ops=Ops(self.pool, self.eng, self.q, self.cfg.worker_stale_after, self.cfg.worker_offline_after),
            registry=self.registry))

    def worker(self, log: Log | None = None) -> Worker:
        return Worker(WorkerConfig(concurrency=self.cfg.worker_concurrency, lease=self.cfg.lease_duration,
                                   heartbeat=self.cfg.worker_heartbeat, shutdown_grace=self.cfg.shutdown_grace,
                                   poll_interval=self.cfg.worker_poll_interval),
                      self.eng, self.pool, self.q, self.registry.subset(self.cfg.worker_handlers), log or self.log)

    def scheduler(self, log: Log | None = None) -> Scheduler:
        return Scheduler(self.eng, self.pool, self.q, log or self.log, database_url=self.cfg.database_url,
                         tick=self.cfg.scheduler_tick, batch_size=self.cfg.scheduler_batch_size,
                         reconcile_interval=self.cfg.reconcile_interval,
                         worker_offline_after=self.cfg.worker_offline_after)

    def http_server(self, app, port: int, host: str | None = None) -> _Server:
        return _Server(uvicorn.Config(app, host=host or self.cfg.http_host, port=port, log_config=None,
                                      access_log=False, server_header=False, proxy_headers=False,
                                      timeout_keep_alive=120, timeout_graceful_shutdown=20, h11_max_incomplete_event_size=64 << 10))

    def admin_app(self) -> FastAPI:
        """Liveness, readiness and metrics for worker/scheduler processes on an
        internal-only port."""
        app = FastAPI(openapi_url=None, docs_url=None, redoc_url=None)

        @app.get("/healthz")
        async def healthz():
            return {"status": "ok"}

        @app.get("/readyz")
        async def readyz():
            try:
                async with self.pool.connection(timeout=2) as conn:
                    await asyncio.wait_for(conn.execute("SELECT 1"), 2)
            except Exception:  # noqa: BLE001
                return JSONResponse({"status": "unavailable"}, status_code=503)
            return {"status": "ready"}

        @app.get("/metrics")
        async def metrics_endpoint():
            return Response(metrics.exposition(), media_type="text/plain; version=0.0.4; charset=utf-8")

        return app


async def _serve(server: _Server) -> None:
    """Runs an HTTP server. uvicorn reports startup failures (e.g. the port is
    in use) with SystemExit, which would escape the task and leave the other
    components running headless; turn it into an ordinary error instead."""
    try:
        await server.serve()
    except SystemExit as e:
        raise RuntimeError(f"HTTP server failed to start on port {server.config.port} (exit {e.code})") from None
    if not server.started:
        raise RuntimeError(f"HTTP server failed to start on port {server.config.port}")


async def _serve_until(server: _Server, stop: asyncio.Event) -> None:
    task = asyncio.create_task(_serve(server))
    await asyncio.wait({task, asyncio.create_task(stop.wait())}, return_when=asyncio.FIRST_COMPLETED)
    server.should_exit = True
    await task


async def run_api() -> None:
    rt = await Runtime.start("api")
    stop = _stop_event()
    try:
        rt.log.info("starting api", port=rt.cfg.http_port)
        await _serve_until(rt.http_server(rt.api_app(), rt.cfg.http_port), stop)
    finally:
        await rt.close()


async def _run_with_admin(rt: Runtime, component, stop: asyncio.Event) -> None:
    admin = rt.http_server(rt.admin_app(), rt.cfg.admin_port)
    admin_task = asyncio.create_task(_serve(admin))
    try:
        await component.run(stop)
    finally:
        admin.should_exit = True
        await asyncio.gather(admin_task, return_exceptions=True)
        await rt.close()


async def run_worker() -> None:
    rt = await Runtime.start("worker")
    await _run_with_admin(rt, rt.worker(), _stop_event())


async def run_scheduler() -> None:
    rt = await Runtime.start("scheduler")
    await _run_with_admin(rt, rt.scheduler(), _stop_event())


async def run_all_in_one() -> None:
    """Migrations, the API, the scheduler and one worker in a single process,
    for hosting tiers that allow only one web service (e.g. Render's free
    plan). The components are the same code as the separate processes and
    coordinate through PostgreSQL exactly as they would across machines."""
    rt = await Runtime.start("all-in-one")
    stop = _stop_event()
    try:
        await db.migrate(rt.pool, rt.log)
        if rt.cfg.seed_demo:
            from .seed import seed_demo
            await seed_demo(rt.cfg, rt.pool, rt.log, rt.registry, rt.cfg.seed_password, True)
        worker_stop, sched_stop = asyncio.Event(), asyncio.Event()
        worker_task = asyncio.create_task(rt.worker(rt.log.with_(role="worker")).run(worker_stop))
        sched_task = asyncio.create_task(rt.scheduler(rt.log.with_(role="scheduler")).run(sched_stop))
        server = rt.http_server(rt.api_app(), rt.cfg.http_port)
        rt.log.info("starting api", port=rt.cfg.http_port)
        server_task = asyncio.create_task(_serve(server))
        await asyncio.wait({server_task, worker_task, asyncio.create_task(stop.wait())},
                           return_when=asyncio.FIRST_COMPLETED)
        # Stop accepting requests, then let the worker drain in-flight attempts.
        server.should_exit = True
        worker_stop.set()
        sched_stop.set()
        failed = None
        for name, t in (("api", server_task), ("worker", worker_task), ("scheduler", sched_task)):
            try:
                await t
            except Exception as e:  # noqa: BLE001 - every component still gets to shut down
                rt.log.error(f"{name} stopped with an error", err=str(e))
                failed = failed or e
        if failed is not None and not stop.is_set():
            # A component died on its own: exit non-zero so the platform restarts us.
            raise failed
    finally:
        await rt.close()


async def run_migrate() -> None:
    cfg = Config.load()
    log = new_logger(cfg.log_level, "migrate")
    pool = await db.connect(cfg.database_url, cfg.db_max_conns, retries=30, log=log)
    try:
        await db.migrate(pool, log)
        log.info("migrations up to date")
    finally:
        await pool.close()


async def run_seed(args: list[str]) -> None:
    from .seed import seed_demo

    cfg = Config.load()
    if cfg.production:
        raise SystemExit("refusing to seed demo data when TASKFLOW_ENV=production")
    log = new_logger(cfg.log_level, "seed")
    pool = await db.connect(cfg.database_url, cfg.db_max_conns, retries=30, log=log)
    try:
        password, generated = cfg.seed_password, False
        if not password:
            password, generated = "Tf-" + secrets.token_urlsafe(12), True
        registry = builtin(pool, cfg.allow_private_webhooks, cfg.webhook_max_resp_bytes).subset(cfg.enabled_handlers)
        created = await seed_demo(cfg, pool, log, registry, password, "--no-jobs" not in args)
        if created:
            print("Created demo users: admin@example.com, operator@example.com, viewer@example.com")
            print(f"Generated password (shown once): {password}" if generated else "Password: value of SEED_PASSWORD")
        else:
            print("Demo users already exist; skipping user creation.")
    finally:
        await pool.close()
