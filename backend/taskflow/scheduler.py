"""Periodic control loops:

  - enqueue due occurrences (one-time, delayed and cron jobs);
  - promote runs whose retry backoff elapsed;
  - move Redis-deferred runs back to the ready set;
  - recover runs whose worker lease expired (crashed workers);
  - mark silent workers offline;
  - reconcile PostgreSQL with Redis (re-publish lost queue entries);
  - apply retention to events, sessions and old workers.

Several scheduler replicas can run; one is leader at a time. Leadership is a
PostgreSQL session advisory lock held on a dedicated connection: if the
leader crashes or loses its connection, PostgreSQL releases the lock and
another replica takes over on its next tick. Correctness does not depend on
a single leader — every loop is safe to run concurrently (SKIP LOCKED,
conditional updates, the unique occurrence index) — leadership only avoids
wasted work.
"""

from __future__ import annotations

import asyncio
import socket
import time
import uuid
from datetime import UTC, datetime, timedelta

import psycopg
from psycopg_pool import AsyncConnectionPool

from . import metrics
from .domain import Event, SchedulerInstance, Severity
from .engine import Engine
from .logs import Log
from .queue import Queue
from .store import analytics
from .store import events as event_store
from .store import users as user_store
from .store import workers as worker_store

LEADER_LOCK_KEY = 7_201_551_002


def _utcnow() -> datetime:
    return datetime.now(UTC)


class Scheduler:
    def __init__(self, eng: Engine, pool: AsyncConnectionPool, q: Queue | None, log: Log, *, database_url: str,
                 tick: float = 1, batch_size: int = 200, reconcile_interval: float = 10,
                 worker_offline_after: float = 120):
        self.id = uuid.uuid4()
        self.eng = eng
        self.pool = pool
        self.q = q
        self.database_url = database_url
        self.tick_interval = tick
        self.batch_size = batch_size
        self.reconcile_interval = reconcile_interval
        self.worker_offline_after = worker_offline_after
        self.hostname = socket.gethostname()
        self.started = _utcnow()
        self.log = log.with_(scheduler_id=str(self.id))
        self._leader_conn: psycopg.AsyncConnection | None = None
        self._last_run: dict[str, float] = {}

    @property
    def is_leader(self) -> bool:
        return self._leader_conn is not None

    async def run(self, stop: asyncio.Event) -> None:
        """Loops until stop is set."""
        self.log.info("scheduler started", tick=self.tick_interval)
        try:
            while not stop.is_set():
                await self._iterate()
                try:
                    await asyncio.wait_for(stop.wait(), self.tick_interval)
                except TimeoutError:
                    pass
        finally:
            await self._shutdown()

    async def _iterate(self) -> None:
        await self._ensure_leadership()
        inst = SchedulerInstance(id=self.id, hostname=self.hostname, is_leader=self.is_leader,
                                 started_at=self.started, last_heartbeat_at=_utcnow())
        if self.is_leader:
            start = time.monotonic()
            enqueued = await self.tick()
            elapsed = time.monotonic() - start
            inst.last_tick_at, inst.last_tick_ms, inst.last_tick_enqueued = _utcnow(), int(elapsed * 1000), enqueued
            metrics.SCHEDULER_TICK.observe(elapsed)
        if self._due("heartbeat", 2) or inst.last_tick_at is not None:
            try:
                async with self.pool.connection() as conn:
                    await worker_store.upsert_scheduler(conn, inst)
            except Exception as e:  # noqa: BLE001
                self.log.warning("scheduler heartbeat failed", err=str(e))

    async def _ensure_leadership(self) -> None:
        """Acquires or verifies the advisory lock."""
        if self._leader_conn is not None:
            try:
                await asyncio.wait_for(self._leader_conn.execute("SELECT 1"), 2)
                return
            except Exception as e:  # noqa: BLE001
                self.log.warning("lost leader connection; stepping down", err=str(e))
                await self._drop_leader_conn()
        try:
            conn = await psycopg.AsyncConnection.connect(self.database_url, autocommit=True, connect_timeout=5)
        except Exception:  # noqa: BLE001 - try again next tick
            return
        try:
            row = await (await conn.execute("SELECT pg_try_advisory_lock(%s)", (LEADER_LOCK_KEY,))).fetchone()
        except Exception:  # noqa: BLE001
            row = None
        if not row or not row[0]:
            await conn.close()
            return
        self._leader_conn = conn
        metrics.SCHEDULER_LEADER.set(1)
        self.log.info("acquired scheduler leadership")

    async def _drop_leader_conn(self) -> None:
        if self._leader_conn is None:
            return
        # Closing the session releases the advisory lock even if unlock fails.
        try:
            await self._leader_conn.close()
        except Exception:  # noqa: BLE001
            pass
        self._leader_conn = None
        metrics.SCHEDULER_LEADER.set(0)

    async def _shutdown(self) -> None:
        if self._leader_conn is not None:
            try:
                await asyncio.wait_for(self._leader_conn.execute("SELECT pg_advisory_unlock(%s)", (LEADER_LOCK_KEY,)), 5)
            except Exception:  # noqa: BLE001
                pass
            await self._drop_leader_conn()
        try:
            async with self.pool.connection() as conn:
                await worker_store.stop_scheduler(conn, self.id, _utcnow())
        except Exception:  # noqa: BLE001
            pass
        self.log.info("scheduler stopped")

    def _due(self, name: str, every: float) -> bool:
        now = time.monotonic()
        if now - self._last_run.get(name, float("-inf")) < every:
            return False
        self._last_run[name] = now
        return True

    async def _step(self, op: str, coro):
        """Runs one loop step; failures are logged and never stop the tick."""
        try:
            return await coro
        except asyncio.CancelledError:
            raise
        except Exception as e:  # noqa: BLE001
            self.log.error(f"{op} failed", err=str(e))
            return None

    async def tick(self) -> int:
        """One leader iteration; returns the number of runs enqueued. Public
        so tests can drive the scheduler deterministically."""
        enqueued = 0
        # Drain a backlog in batches, bounded so one tick cannot run forever.
        for _ in range(20):
            n = await self._step("schedule due jobs", self.eng.schedule_due(self.batch_size))
            if n is None:
                break
            enqueued += n
            if n < self.batch_size:
                break
        n = await self._step("promote retries", self.eng.promote_retries(self.batch_size))
        if n:
            self.log.debug("promoted retries", count=n)
        if self.q is not None:
            try:
                await self.q.promote_delayed(_utcnow(), 1000)
            except Exception:  # noqa: BLE001
                metrics.REDIS_ERRORS.labels("promote_delayed").inc()
        if self._due("reaper", 2):
            n = await self._step("recover expired leases", self.eng.recover_expired_leases(100))
            if n:
                self.log.warning("recovered runs with expired leases", count=n)
        if self._due("workers", 5):
            await self._step("mark offline workers", self._mark_offline_workers())
        if self._due("reconcile", self.reconcile_interval):
            try:
                n = await self.eng.reconcile(timedelta(seconds=self.reconcile_interval), 1000)
                if n:
                    self.log.info("re-published queued runs to redis", count=n)
            except Exception as e:  # noqa: BLE001
                self.log.warning("reconcile with redis failed", err=str(e))
        if self._due("depth", 5):
            await self._observe_depth()
        if self._due("retention", 3600):
            await self._retention()
        return enqueued

    async def _mark_offline_workers(self) -> None:
        cutoff = _utcnow() - timedelta(seconds=self.worker_offline_after)
        async with self.pool.connection() as conn:
            for w in await worker_store.mark_silent_offline(conn, cutoff):
                await self.eng.record_event(conn, Event(
                    worker_id=w.id, type="worker.offline", severity=Severity.WARNING,
                    message=f"Worker {str(w.id)[:8]} on {w.hostname} stopped heartbeating at "
                            f"{w.last_heartbeat_at.isoformat().replace('+00:00', 'Z')} and was marked offline"))
                self.log.warning("worker marked offline", worker_id=str(w.id), last_heartbeat=str(w.last_heartbeat_at))

    async def _observe_depth(self) -> None:
        try:
            async with self.pool.connection() as conn:
                c = await analytics.global_queue_counts(conn)
            metrics.QUEUE_DEPTH.labels("postgres_queued").set(c["queued"])
            metrics.QUEUE_DEPTH.labels("postgres_retry_wait").set(c["retry_wait"])
        except Exception:  # noqa: BLE001
            pass
        if self.q is not None:
            try:
                ready, delayed = await self.q.depth()
                metrics.QUEUE_DEPTH.labels("redis_ready").set(ready)
                metrics.QUEUE_DEPTH.labels("redis_delayed").set(delayed)
            except Exception:  # noqa: BLE001
                pass

    async def _retention(self) -> None:
        now = _utcnow()
        async with self.pool.connection() as conn:
            n = await self._step("event retention", event_store.delete_old(conn, now))
            if n:
                self.log.info("deleted old events", count=n)
            await self._step("session retention", user_store.delete_expired_sessions(conn, now))
            await self._step("worker retention", worker_store.delete_old(conn, now - timedelta(days=7)))
