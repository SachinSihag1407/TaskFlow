"""Read models and admin operations for the dashboard, workers, executions,
dead-letter queue, health, settings and audit pages."""

from __future__ import annotations

import asyncio
import time
from datetime import UTC, datetime, timedelta
from uuid import UUID

from .. import cron
from ..db import tx
from ..domain import Event, Principal, Settings, Severity, Tenant, Validator, Worker, WorkerState
from ..engine import Engine
from ..queue import Queue
from ..store import Page, analytics
from ..store import attempts as attempt_store
from ..store import events as event_store
from ..store import runs as run_store
from ..store import users as user_store
from ..store import workers as worker_store
from . import Input


class SettingsInput(Input):
    """Missing numbers default to 0 so they fail validation with a field
    message rather than a parse error."""

    default_max_attempts: int = 0
    default_timeout_seconds: int = 0
    default_backoff_base_seconds: int = 0
    default_backoff_max_seconds: int = 0
    default_priority: int = 0
    default_timezone: str = ""
    max_concurrent_runs: int = 0
    event_retention_days: int = 0
    updated_at: datetime | None = None  # echoed back by clients; ignored


# Dashboard time range -> (window, bucket width).
RANGES = {
    "1h": (timedelta(hours=1), timedelta(minutes=2)),
    "6h": (timedelta(hours=6), timedelta(minutes=10)),
    "24h": (timedelta(hours=24), timedelta(hours=1)),
    "7d": (timedelta(days=7), timedelta(hours=6)),
    "30d": (timedelta(days=30), timedelta(days=1)),
}


def _empty_worker_summary() -> dict:
    return {"online": 0, "draining": 0, "stale": 0, "offline": 0, "capacity": 0, "busy": 0}


class Ops:
    def __init__(self, pool, eng: Engine, q: Queue | None, stale_after: float, offline_after: float):
        self.pool = pool
        self.eng = eng
        self.q = q
        self.stale_after = timedelta(seconds=stale_after)
        self.offline_after = timedelta(seconds=offline_after)
        self.now = lambda: datetime.now(UTC)

    # ---- settings ------------------------------------------------------------

    async def settings(self, p: Principal) -> Settings:
        async with self.pool.connection() as conn:
            return await user_store.get_settings(conn, p.tenant_id)

    async def update_settings(self, p: Principal, body: SettingsInput) -> Settings:
        v = Validator()
        v.check(1 <= body.default_max_attempts <= 25, "default_max_attempts", "Must be between 1 and 25.")
        v.check(1 <= body.default_timeout_seconds <= 86400, "default_timeout_seconds", "Must be between 1 and 86400 seconds.")
        v.check(1 <= body.default_backoff_base_seconds <= 3600, "default_backoff_base_seconds",
                "Must be between 1 and 3600 seconds.")
        v.check(1 <= body.default_backoff_max_seconds <= 86400, "default_backoff_max_seconds",
                "Must be between 1 and 86400 seconds.")
        v.check(body.default_backoff_max_seconds >= body.default_backoff_base_seconds, "default_backoff_max_seconds",
                "Must be at least the base delay.")
        v.check(1 <= body.default_priority <= 10, "default_priority", "Must be between 1 and 10.")
        v.check(0 <= body.max_concurrent_runs <= 10000, "max_concurrent_runs", "Must be between 0 (unlimited) and 10000.")
        v.check(1 <= body.event_retention_days <= 365, "event_retention_days", "Must be between 1 and 365 days.")
        try:
            cron.load_timezone(body.default_timezone)
        except cron.CronError:
            v.add("default_timezone", "Unknown timezone.")
        v.raise_if_any()
        new = Settings(**body.model_dump(exclude={"updated_at"}), tenant_id=p.tenant_id)
        async with tx(self.pool) as conn:
            before = await user_store.get_settings(conn, p.tenant_id)
            await user_store.update_settings(conn, p.tenant_id, new, p.user_id, self.now())
            await self.eng.audit(conn, p, "settings.update", "tenant", str(p.tenant_id),
                                 {"before": before.model_dump(mode="json"), "after": new.model_dump(mode="json")})
        return await self.settings(p)

    # ---- workers -------------------------------------------------------------

    def _effective_status(self, w: Worker, now: datetime) -> str:
        """STALE for workers whose heartbeat is overdue but that the scheduler
        has not yet declared OFFLINE."""
        if w.status != WorkerState.OFFLINE and now - w.last_heartbeat_at > self.stale_after:
            return WorkerState.STALE.value
        return w.status

    def _view(self, w: Worker, now: datetime) -> dict:
        w.status = self._effective_status(w, now)
        out = w.model_dump(mode="json")
        if w.status in (WorkerState.OFFLINE, WorkerState.STALE):
            out["active_runs"] = 0  # the last reported value is not trustworthy
        out["seconds_since_heartbeat"] = (now - w.last_heartbeat_at).total_seconds()
        return out

    async def workers(self) -> list[dict]:
        now = self.now()
        async with self.pool.connection() as conn:
            ws = await worker_store.list_workers(conn, now - timedelta(hours=24))
        return [self._view(w, now) for w in ws]

    async def worker(self, p: Principal, worker_id: UUID) -> dict:
        """Run and attempt data are scoped to the caller's workspace: workers
        are shared infrastructure, job data is not."""
        async with self.pool.connection() as conn:
            w = await worker_store.get(conn, worker_id)
            running = await worker_store.running_runs(conn, p.tenant_id, worker_id)
            recent, _ = await attempt_store.list_attempts(conn, attempt_store.AttemptFilter(
                tenant_id=p.tenant_id, worker_id=worker_id, page=Page(1, 25)))
            events = await event_store.list_events(conn, event_store.EventFilter(
                tenant_id=p.tenant_id, worker_id=worker_id, include_platform=True, limit=25))
        view = self._view(w, self.now())
        view["active_runs"] = w.active_runs if w.status not in (WorkerState.OFFLINE, WorkerState.STALE) else 0
        return {"worker": view, "running_runs": running, "recent_attempts": recent, "events": events}

    async def drain_worker(self, p: Principal, worker_id: UUID) -> None:
        """Asks a worker to stop taking new runs and shut down gracefully."""
        async with tx(self.pool) as conn:
            await worker_store.request_drain(conn, worker_id, self.now())
            await self.eng.record_event(conn, Event(worker_id=worker_id, type="worker.drain_requested",
                                                    severity=Severity.INFO,
                                                    message="Drain requested by a platform administrator"))
            await self.eng.audit(conn, p, "worker.drain", "worker", str(worker_id))

    @staticmethod
    def _summarize(workers: list[dict], count_draining_busy: bool) -> dict:
        s = _empty_worker_summary()
        for w in workers:
            if w["status"] == WorkerState.ONLINE:
                s["online"] += 1
                s["capacity"] += w["concurrency"]
                s["busy"] += w["active_runs"]
            elif w["status"] == WorkerState.DRAINING:
                s["draining"] += 1
                if count_draining_busy:
                    s["busy"] += w["active_runs"]
            elif w["status"] == WorkerState.STALE:
                s["stale"] += 1
            else:
                s["offline"] += 1
        return s

    # ---- analytics -----------------------------------------------------------

    async def overview(self, p: Principal, rng: str) -> dict:
        """The dashboard model. Every number comes from the database."""
        if rng not in RANGES:
            rng = "24h"
        window, bucket = RANGES[rng]
        now = self.now()
        since = now - window
        async with self.pool.connection() as conn:
            job_counts = await analytics.job_status_counts(conn, p.tenant_id)
            counts = await analytics.run_status_counts(conn, p.tenant_id)
            dead = await analytics.dead_letter_count(conn, p.tenant_id)
            outcomes = await analytics.outcomes(conn, p.tenant_id, since)
            throughput = await analytics.throughput(conn, p.tenant_id, since, now, bucket)
            by_handler = await analytics.outcomes_by_handler(conn, p.tenant_id, since)
            recent, _ = await attempt_store.list_attempts(conn, attempt_store.AttemptFilter(
                tenant_id=p.tenant_id, page=Page(1, 8)))
            events = await event_store.list_events(conn, event_store.EventFilter(
                tenant_id=p.tenant_id, include_platform=True, limit=12))
        finished = outcomes["runs_succeeded"] + outcomes["runs_failed"]
        return {
            "range": rng, "generated_at": now, "job_counts": job_counts, "total_jobs": sum(job_counts.values()),
            "queued": counts["queued"], "running": counts["running"], "retry_wait": counts["retry_wait"],
            "dead_letter": dead, "outcomes": outcomes,
            "success_rate": outcomes["runs_succeeded"] / finished if finished else None,
            "throughput": throughput, "by_handler": by_handler,
            "workers": self._summarize(await self.workers(), True), "queue": await self._queue_summary(),
            "recent_attempts": recent, "events": events,
        }

    async def _queue_summary(self) -> dict:
        qs = {"redis_available": False, "redis_ready": None, "redis_delayed": None}
        if self.q is None:
            return qs
        try:
            ready, delayed = await asyncio.wait_for(self.q.depth(), 1)
        except Exception:  # noqa: BLE001
            return qs
        return {"redis_available": True, "redis_ready": ready, "redis_delayed": delayed}

    # ---- health --------------------------------------------------------------

    async def health(self) -> dict:
        """Checks every dependency. Never fails as a whole: each component
        reports its own status so the page can show exactly what is degraded."""
        h: dict = {"checked_at": self.now(), "components": [], "schedulers": [],
                   "workers": _empty_worker_summary(),
                   "queue": {"postgres_queued": 0, "postgres_running": 0, "postgres_retry_wait": 0,
                             "redis_ready": None, "redis_delayed": None},
                   "rates": {"window_minutes": 0, "attempts": 0, "failed": 0, "retries": 0,
                             "error_rate": None, "retry_rate": None}}
        pg = {"name": "postgres", "status": "healthy", "latency_ms": 0.0, "detail": "Primary datastore"}
        start = time.perf_counter()
        try:
            async with self.pool.connection(timeout=2) as conn:
                await asyncio.wait_for(conn.execute("SELECT 1"), 2)
        except Exception:  # noqa: BLE001 - reported generically; details stay in server logs
            pg["status"], pg["detail"] = "down", "Database unreachable"
        pg["latency_ms"] = round((time.perf_counter() - start) * 1000, 3)
        h["components"] += [{"name": "api", "status": "healthy", "latency_ms": 0.0, "detail": "Serving requests"}, pg]

        redis = {"name": "redis", "status": "healthy", "latency_ms": 0.0, "detail": "Queue and pub/sub"}
        if self.q is None:
            redis["status"] = "disabled"
            redis["detail"] = "Not used in this deployment — workers poll PostgreSQL and the UI polls for updates"
        else:
            start = time.perf_counter()
            try:
                await asyncio.wait_for(self.q.ping(), 1)
                ok = True
            except Exception:  # noqa: BLE001
                ok = False
            redis["latency_ms"] = round((time.perf_counter() - start) * 1000, 3)
            if not ok:
                redis["status"] = "degraded"
                redis["detail"] = "Unreachable — workers fall back to PostgreSQL polling and live updates pause"
            else:
                try:
                    h["queue"]["redis_ready"], h["queue"]["redis_delayed"] = await self.q.depth()
                except Exception:  # noqa: BLE001
                    pass
        h["components"].append(redis)

        if pg["status"] == "healthy":
            await self._health_details(h)

        status = "healthy"
        for c in h["components"]:
            if c["status"] == "down" and c["name"] in ("postgres", "api"):
                status = "down"
                break
            if c["status"] not in ("healthy", "disabled"):
                status = "degraded"
        h["status"] = status
        return h

    async def _health_details(self, h: dict) -> None:
        now = self.now()
        async with self.pool.connection() as conn:
            scheds = await worker_store.list_schedulers(conn, now - timedelta(minutes=5))
            q = await analytics.global_queue_counts(conn)
            rates = await analytics.global_error_rates(conn, now - timedelta(hours=1))
        h["schedulers"] = scheds
        sched = {"name": "scheduler", "status": "down", "latency_ms": 0.0,
                 "detail": "No scheduler instance has reported in the last 5 minutes"}
        for s in scheds:
            alive = s.stopped_at is None and now - s.last_heartbeat_at < timedelta(seconds=15)
            if s.is_leader and alive:
                sched.update(status="healthy", detail=f"Leader {s.hostname}", latency_ms=float(s.last_tick_ms or 0))
                break
            if alive:
                sched.update(status="degraded", detail="Instances are alive but none holds leadership")
        h["components"].append(sched)

        h["workers"] = self._summarize(await self.workers(), False)
        wc = {"name": "workers", "status": "healthy", "latency_ms": 0.0, "detail": "All reporting workers are healthy"}
        if h["workers"]["online"] == 0:
            wc.update(status="down", detail="No online workers: queued runs will not execute")
        elif h["workers"]["stale"] > 0:
            wc.update(status="degraded", detail="Some workers have missed heartbeats")
        h["components"].append(wc)

        h["queue"].update(postgres_queued=q["queued"], postgres_running=q["running"], postgres_retry_wait=q["retry_wait"])
        total = rates["attempts"]
        h["rates"] = {"window_minutes": 60, "attempts": total, "failed": rates["failed"], "retries": rates["retries"],
                      "error_rate": rates["failed"] / total if total else None,
                      "retry_rate": rates["retries"] / total if total else None}

    # ---- executions, runs, events, audit -------------------------------------

    async def attempts(self, p: Principal, f: attempt_store.AttemptFilter):
        f.tenant_id = p.tenant_id
        async with self.pool.connection() as conn:
            return await attempt_store.list_attempts(conn, f)

    async def attempt(self, p: Principal, attempt_id: UUID) -> dict:
        async with self.pool.connection() as conn:
            a = await attempt_store.get(conn, p.tenant_id, attempt_id)
            r = await run_store.get(conn, p.tenant_id, a.run_id)
            logs = await attempt_store.list_logs(conn, p.tenant_id, attempt_id)
        return {"attempt": a, "run": r, "logs": logs}

    async def logs(self, p: Principal, attempt_id: UUID, after: int):
        async with self.pool.connection() as conn:
            await attempt_store.get(conn, p.tenant_id, attempt_id)
            return await attempt_store.list_logs(conn, p.tenant_id, attempt_id, after)

    async def runs(self, p: Principal, f: run_store.RunFilter):
        f.tenant_id = p.tenant_id
        async with self.pool.connection() as conn:
            return await run_store.list_runs(conn, f)

    async def run(self, p: Principal, run_id: UUID) -> dict:
        """One run with all its attempts, events and audit trail."""
        async with self.pool.connection() as conn:
            r = await run_store.get(conn, p.tenant_id, run_id)
            attempts, _ = await attempt_store.list_attempts(conn, attempt_store.AttemptFilter(
                tenant_id=p.tenant_id, run_id=run_id, sort="started_at", page=Page(1, 50)))
            events = await event_store.list_events(conn, event_store.EventFilter(
                tenant_id=p.tenant_id, run_id=run_id, limit=50))
            audit, _ = await event_store.list_audit(conn, event_store.AuditFilter(
                tenant_id=p.tenant_id, target_type="run", target_id=str(run_id), page=Page(1, 20)))
        return {"run": r, "attempts": attempts, "events": events, "audit": audit}

    async def events(self, p: Principal, f: event_store.EventFilter):
        f.tenant_id, f.include_platform = p.tenant_id, True
        async with self.pool.connection() as conn:
            return await event_store.list_events(conn, f)

    async def audit(self, p: Principal, f: event_store.AuditFilter):
        f.tenant_id = p.tenant_id
        async with self.pool.connection() as conn:
            return await event_store.list_audit(conn, f)

    async def notifications(self, p: Principal, seen_at: datetime | None) -> dict:
        """Recent warning/error events and the unread count."""
        sev = ["warning", "error"]
        since = self.now() - timedelta(days=7)
        if seen_at is not None and seen_at > since:
            since = seen_at
        async with self.pool.connection() as conn:
            items = await event_store.list_events(conn, event_store.EventFilter(tenant_id=p.tenant_id, severities=sev,
                                                                                limit=20))
            unread = await event_store.count_since(conn, p.tenant_id, sev, since)
        return {"unread": unread, "items": items}

    async def tenant(self, tenant_id: UUID) -> Tenant:
        async with self.pool.connection() as conn:
            return await user_store.get_tenant(conn, tenant_id)
