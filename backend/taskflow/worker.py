"""The execution runtime: registers the process, sends heartbeats, claims runs
(from Redis, or directly from PostgreSQL when Redis is unavailable), executes
handlers with bounded concurrency, timeouts and cooperative cancellation,
renews leases, and shuts down gracefully."""

from __future__ import annotations

import asyncio
import os
import random
import socket
import time
import traceback
import uuid
from dataclasses import dataclass
from datetime import UTC, datetime, timedelta
from typing import Any

from psycopg_pool import AsyncConnectionPool

from . import __version__, metrics
from .domain import Event, LogLine, RunStatus, Severity, WorkerState
from .domain import Worker as WorkerRow
from .engine import Claimed, Deferred, Engine, LeaseLost, Outcome, OutcomeKind, Paused, Stale, Unsupported
from .handlers import Execution, Permanent, Registry
from .logs import Log, redact
from .queue import Item, Queue
from .store import attempts as attempt_store
from .store import runs as run_store
from .store import workers as worker_store

# Why an attempt was stopped. The worker inspects the cause to classify it.
CANCEL_REQUESTED = "cancelled by operator"
SHUTDOWN = "worker shutting down"
LEASE_LOST = "lease lost: run reclaimed after this worker was presumed dead"
TIMEOUT = "attempt timed out"

MAX_LOG_MESSAGE = 2000


@dataclass
class WorkerConfig:
    concurrency: int = 4
    lease: float = 30           # seconds
    heartbeat: float = 5
    shutdown_grace: float = 30
    poll_interval: float = 2
    version: str = __version__


def _utcnow() -> datetime:
    return datetime.now(UTC)


def _rss_bytes() -> int:
    """Resident memory of this process (Linux), falling back to peak RSS."""
    try:
        with open("/proc/self/statm") as f:
            return int(f.read().split()[1]) * os.sysconf("SC_PAGE_SIZE")
    except (OSError, ValueError, IndexError):
        try:
            import resource
            return resource.getrusage(resource.RUSAGE_SELF).ru_maxrss * 1024
        except (ImportError, OSError):
            return 0


class AttemptLogger:
    """Buffers a handler's log lines and flushes them to PostgreSQL
    periodically, so logs of long-running attempts are visible while they
    run. Lines are redacted and size-capped before storage."""

    def __init__(self, pool: AsyncConnectionPool, attempt_id: uuid.UUID, sys_log: Log):
        self.pool = pool
        self.attempt_id = attempt_id
        self.sys = sys_log
        self.pending: list[LogLine] = []
        self.total = 0
        self.dropped = 0

    def _add(self, level: str, msg: str) -> None:
        msg = redact(str(msg))
        if len(msg) > MAX_LOG_MESSAGE:
            msg = msg[:MAX_LOG_MESSAGE] + "…"
        if self.total >= attempt_store.MAX_LOG_LINES - 1:
            self.dropped += 1
            return
        self.total += 1
        self.pending.append(LogLine(ts=_utcnow(), level=level, message=msg))

    def info(self, msg: str) -> None:
        self._add("info", msg)

    def warning(self, msg: str) -> None:
        self._add("warn", msg)

    def error(self, msg: str) -> None:
        self._add("error", msg)

    async def flush(self) -> None:
        """Writes buffered lines; on failure they are kept for the next try."""
        batch, self.pending = self.pending, []
        if not batch:
            return
        try:
            async with self.pool.connection() as conn:
                await attempt_store.insert_logs(conn, self.attempt_id, batch)
        except Exception as e:  # noqa: BLE001
            self.sys.warning("flush attempt logs failed", attempt_id=str(self.attempt_id), err=str(e))
            self.pending = batch + self.pending

    async def close(self) -> None:
        """Records the truncation notice (if any) and flushes."""
        if self.dropped:
            self.pending.append(LogLine(ts=_utcnow(), level="warn", message=(
                f"{self.dropped} further log line(s) dropped (limit {attempt_store.MAX_LOG_LINES} per attempt)")))
        await self.flush()


class _Running:
    """Book-keeping for one in-flight attempt."""

    def __init__(self) -> None:
        self.task: asyncio.Task | None = None
        self.cause: str | None = None
        self.stopped_at: float | None = None  # monotonic time the handler was asked to stop

    def stop(self, cause: str) -> None:
        if self.cause is None:
            self.cause = cause
            self.stopped_at = time.monotonic()
        if self.task is not None and not self.task.done():
            self.task.cancel()


@dataclass
class _Result:
    value: Any = None
    error: BaseException | None = None
    panicked: bool = False


class Worker:
    def __init__(self, cfg: WorkerConfig, eng: Engine, pool: AsyncConnectionPool, q: Queue | None,
                 reg: Registry, log: Log):
        self.id = uuid.uuid4()
        self.cfg = cfg
        self.eng = eng
        self.pool = pool
        self.q = q  # None disables Redis entirely
        self.reg = reg
        self.hostname = socket.gethostname()
        self.log = log.with_(worker_id=str(self.id))
        self._active: dict[uuid.UUID, _Running] = {}
        self._tasks: set[asyncio.Task] = set()
        self._draining = asyncio.Event()
        self._redis_healthy = q is not None
        self._redis_retry_at = 0.0

    @property
    def active_runs(self) -> int:
        return len(self._tasks)

    def drain(self) -> None:
        """Stops claiming new work; run() returns once in-flight work finishes."""
        self._draining.set()

    async def run(self, stop: asyncio.Event) -> None:
        """Executes until stop is set (SIGTERM) or a drain is requested, then
        shuts down gracefully:

          1. stop claiming and report DRAINING;
          2. let in-flight attempts finish for up to shutdown_grace;
          3. interrupt whatever is still running; those attempts are recorded
             as interrupted and retried promptly by another worker;
          4. report OFFLINE.

        If the process is killed instead (SIGKILL, OOM, power loss) none of
        this runs; the scheduler's lease reaper recovers the abandoned runs."""
        now = _utcnow()
        async with self.pool.connection() as conn:
            await worker_store.register(conn, WorkerRow(
                id=self.id, hostname=self.hostname, pid=os.getpid(), version=self.cfg.version,
                concurrency=self.cfg.concurrency, handlers=self.reg.names(), status=WorkerState.ONLINE,
                started_at=now, last_heartbeat_at=now))
        await self._event("worker.online", f"Worker {str(self.id)[:8]} on {self.hostname} started "
                                           f"(concurrency {self.cfg.concurrency})")
        metrics.WORKER_CAPACITY.set(self.cfg.concurrency)
        self.log.info("worker started", hostname=self.hostname, concurrency=self.cfg.concurrency,
                      handlers=self.reg.names())

        # Background loops outlive the claim loop so heartbeats and
        # cancellation keep working while in-flight attempts drain.
        background = [asyncio.create_task(self._heartbeat_loop())]
        if self.q is not None:
            background.append(asyncio.create_task(self._cancel_listener()))
        stop_watch = asyncio.create_task(self._propagate_stop(stop))
        try:
            await self._claim_loop()
            self.drain()
            await self._beat()  # report DRAINING immediately
            self.log.info("draining", active=self.active_runs, grace=self.cfg.shutdown_grace)
            if self._tasks:
                _, pending = await asyncio.wait(set(self._tasks), timeout=self.cfg.shutdown_grace)
                if pending:
                    self.log.warning("grace period elapsed; interrupting remaining attempts", active=len(pending))
                    for r in list(self._active.values()):
                        r.stop(SHUTDOWN)
                    _, pending = await asyncio.wait(pending, timeout=15)
                    if pending:
                        self.log.error("attempts did not stop after interruption; "
                                       "their leases will expire and be recovered")
        finally:
            stop_watch.cancel()
            for t in background:
                t.cancel()
            await asyncio.gather(*background, stop_watch, return_exceptions=True)
        try:
            async with self.pool.connection() as conn:
                await worker_store.mark_stopped(conn, self.id, _utcnow())
        except Exception as e:  # noqa: BLE001
            self.log.warning("mark worker stopped failed", err=str(e))
        await self._event("worker.stopped", f"Worker {str(self.id)[:8]} on {self.hostname} shut down gracefully")
        self.log.info("worker stopped")

    async def _propagate_stop(self, stop: asyncio.Event) -> None:
        await stop.wait()
        self.drain()

    async def _event(self, typ: str, msg: str) -> None:
        try:
            async with self.pool.connection() as conn:
                await self.eng.record_event(conn, Event(worker_id=self.id, type=typ, severity=Severity.INFO, message=msg))
        except Exception as e:  # noqa: BLE001 - diagnostic only
            self.log.warning("record event failed", type=typ, err=str(e))

    async def _pause(self, seconds: float) -> None:
        """Sleeps, returning early when a drain or shutdown starts."""
        try:
            await asyncio.wait_for(self._draining.wait(), seconds)
        except TimeoutError:
            pass

    # ---- claiming ------------------------------------------------------------

    async def _claim_loop(self) -> None:
        """One loop per execution slot. Each slot claims and runs one attempt
        at a time, so at most `concurrency` attempts are in flight, while
        claims of different slots overlap instead of queueing behind each
        other. Returns as soon as a drain starts; attempts still running are
        tracked in self._tasks and handled by run()."""
        await asyncio.gather(*(self._slot() for _ in range(self.cfg.concurrency)))

    async def _slot(self) -> None:
        while not self._draining.is_set():
            try:
                c = await self._next()
            except Exception as e:  # noqa: BLE001
                self.log.warning("claim failed", err=str(e))
                await self._pause(self.cfg.poll_interval)
                continue
            if c is None:
                continue
            # A run claimed just as a drain began is still executed: it is ours
            # now, and the grace period or interruption decides how it ends.
            metrics.WORKER_ACTIVE_RUNS.inc()
            task = asyncio.create_task(self._execute(c))
            self._tasks.add(task)

            def _done(t: asyncio.Task) -> None:
                self._tasks.discard(t)
                metrics.WORKER_ACTIVE_RUNS.dec()

            task.add_done_callback(_done)
            drain = asyncio.create_task(self._draining.wait())
            await asyncio.wait({task, drain}, return_when=asyncio.FIRST_COMPLETED)
            drain.cancel()

    async def _next(self) -> Claimed | None:
        """A claimed run, or None if none was available."""
        if self.q is not None and await self._use_redis():
            try:
                run_id = await self.q.pop(1.0)
            except Exception as e:  # noqa: BLE001 - fall through to the database path
                metrics.REDIS_ERRORS.labels("pop").inc()
                self._mark_redis_down(e)
            else:
                return None if run_id is None else await self._claim_from_redis(run_id)
        lease = timedelta(seconds=self.cfg.lease)
        try:
            c = await self.eng.claim_next(self.id, self.reg.names(), lease)
        except Exception:
            metrics.CLAIM_RESULTS.labels("error", "db").inc()
            raise
        if c is None:
            metrics.CLAIM_RESULTS.labels("empty", "db").inc()
            await self._pause(self.cfg.poll_interval)
            return None
        metrics.CLAIM_RESULTS.labels("claimed", "db").inc()
        return c

    async def _claim_from_redis(self, run_id: uuid.UUID) -> Claimed | None:
        try:
            c = await self.eng.claim(run_id, self.id, timedelta(seconds=self.cfg.lease),
                                     lambda h: self.reg.get(h) is not None)
        except (Stale, Paused):
            # Duplicate/outdated entry or paused job: drop it. Resume re-publishes.
            metrics.CLAIM_RESULTS.labels("stale", "redis").inc()
            return None
        except (Deferred, Unsupported):
            metrics.CLAIM_RESULTS.labels("deferred", "redis").inc()
            await self._defer(run_id)
            return None
        except Exception:
            metrics.CLAIM_RESULTS.labels("error", "redis").inc()
            # Put it back so it is not lost while the database hiccups; the
            # reconciler would also restore it, just more slowly.
            await self._defer(run_id)
            raise
        metrics.CLAIM_RESULTS.labels("claimed", "redis").inc()
        return c

    async def _defer(self, run_id: uuid.UUID) -> None:
        try:
            async with self.pool.connection() as conn:
                r = await run_store.get_unscoped(conn, run_id)
            if r.status != RunStatus.QUEUED:
                return
            await self.q.defer(Item(run_id, r.priority, r.queued_at or r.created_at), 1 + random.random())
        except Exception:  # noqa: BLE001
            metrics.REDIS_ERRORS.labels("defer").inc()

    async def _use_redis(self) -> bool:
        if self._redis_healthy:
            return True
        if time.monotonic() < self._redis_retry_at:
            return False
        try:
            await asyncio.wait_for(self.q.ping(), 1)
        except Exception:  # noqa: BLE001
            self._redis_retry_at = time.monotonic() + 5
            return False
        self._redis_healthy = True
        self.log.info("redis reachable again; resuming queue-based dispatch")
        return True

    def _mark_redis_down(self, err: Exception) -> None:
        if self._redis_healthy:
            self._redis_healthy = False
            self.log.warning("redis unavailable; falling back to polling PostgreSQL", err=str(err))
        self._redis_retry_at = time.monotonic() + 5

    # ---- execution -----------------------------------------------------------

    async def _execute(self, c: Claimed) -> None:
        """Runs one attempt end to end."""
        log = self.log.with_(run_id=str(c.run.id), attempt=c.attempt.attempt_number, handler=c.run.handler)
        h = self.reg.get(c.run.handler)
        if h is None:  # cannot happen: claims are filtered by supported handlers
            await self._complete(log, c, Outcome(OutcomeKind.FAILURE, f"handler {c.run.handler!r} is not registered"))
            return
        alog = AttemptLogger(self.pool, c.attempt.id, self.log)
        state = _Running()
        self._active[c.run.id] = state

        async def save_checkpoint(cp: Any) -> None:
            await self.eng.save_checkpoint(c, cp)

        x = Execution(run_id=c.run.id, job_id=c.run.job_id, tenant_id=c.run.tenant_id, attempt_id=c.attempt.id,
                      attempt=c.attempt.attempt_number, max_attempts=c.run.max_attempts, payload=c.run.payload,
                      checkpoint=c.run.checkpoint, idempotency_key=str(c.run.id), log=alog,
                      save_checkpoint=save_checkpoint)
        if c.attempt.attempt_number > 1:
            alog.info(f"retry attempt {c.attempt.attempt_number} of {c.run.max_attempts}")
        try:
            state.task = asyncio.create_task(self._safe_execute(h, x, alog))
            aux = asyncio.create_task(self._lease_and_flush(c, state, alog, log))
            done, _ = await asyncio.wait({state.task}, timeout=c.run.timeout_seconds)
            if not done:
                state.stop(TIMEOUT)
            res: _Result = await asyncio.shield(state.task)
            aux.cancel()
            await asyncio.gather(aux, return_exceptions=True)
        finally:
            self._active.pop(c.run.id, None)

        out = self._classify(state.cause, res, c.run.timeout_seconds)
        if state.cause == LEASE_LOST and res.error is not None:
            # Another worker may already be running this run. Do not report.
            alog.warning("stopped: this worker lost ownership of the run")
            await alog.close()
            metrics.LEASE_LOST.inc()
            log.warning("abandoning attempt after lease loss")
            return
        if out.kind == OutcomeKind.SUCCESS:
            alog.info("attempt succeeded")
        elif out.kind == OutcomeKind.CANCELLED:
            alog.warning("attempt cancelled by operator")
        else:
            alog.error(f"attempt failed ({out.kind}): {out.error}")
        await alog.close()
        await self._complete(log, c, out)

    @staticmethod
    async def _safe_execute(h, x: Execution, alog: AttemptLogger) -> _Result:
        """Runs the handler, converting an unexpected exception into a result
        so one bad handler cannot crash the worker and abandon every other
        in-flight attempt."""
        try:
            return _Result(value=await h.execute(x))
        except asyncio.CancelledError as e:
            return _Result(error=e)
        except Exception as e:  # noqa: BLE001
            from .handlers import HandlerError
            if isinstance(e, HandlerError):
                return _Result(error=e)
            stack = traceback.format_exc()[-1500:]
            alog.error(f"panic: {e!r}\n{stack}")
            return _Result(error=RuntimeError(f"handler panicked: {e}"), panicked=True)

    @staticmethod
    def _classify(cause: str | None, res: _Result, timeout_seconds: int) -> Outcome:
        """Maps a handler's result and its stop cause to an engine outcome."""
        if res.error is None and not res.panicked:
            return Outcome(OutcomeKind.SUCCESS, result={} if res.value is None else res.value)
        if cause == CANCEL_REQUESTED:
            return Outcome(OutcomeKind.CANCELLED, CANCEL_REQUESTED)
        if cause == TIMEOUT:
            return Outcome(OutcomeKind.TIMEOUT, f"attempt exceeded its {timeout_seconds}s timeout", retryable=True)
        if cause == SHUTDOWN:
            return Outcome(OutcomeKind.INTERRUPTED, SHUTDOWN, retryable=True)
        if res.panicked:
            return Outcome(OutcomeKind.PANIC, str(res.error), retryable=True)
        msg = str(res.error) or type(res.error).__name__
        if isinstance(res.error, asyncio.CancelledError):
            msg = "attempt was cancelled"
        return Outcome(OutcomeKind.FAILURE, msg, retryable=not isinstance(res.error, Permanent))

    async def _lease_and_flush(self, c: Claimed, state: _Running, alog: AttemptLogger, log: Log) -> None:
        """Renews the lease every lease/3, propagates cancellation requests,
        and flushes attempt logs every second."""
        lease = self.cfg.lease
        next_renew = time.monotonic() + lease / 3
        warned = False
        while True:
            await asyncio.sleep(min(1.0, max(0.05, next_renew - time.monotonic())))
            await alog.flush()
            if time.monotonic() < next_renew:
                continue
            next_renew = time.monotonic() + lease / 3
            if state.stopped_at is not None and time.monotonic() - state.stopped_at > lease:
                # The handler ignores cancellation. Stop renewing so the lease
                # expires and the reaper takes the run back; this slot stays
                # busy until the handler returns, but the run is not stuck.
                if not warned:
                    log.error("handler did not stop after cancellation; letting its lease expire")
                    warned = True
                continue
            try:
                cancel_requested = await asyncio.wait_for(self.eng.renew_lease(c, timedelta(seconds=lease)), 5)
            except LeaseLost:
                log.warning("lease lost; stopping attempt")
                state.stop(LEASE_LOST)
            except Exception as e:  # noqa: BLE001
                # Transient database error: keep running and retry. If it
                # persists past the lease, the reaper takes over and the
                # fenced completion write will be rejected.
                log.warning("lease renewal failed", err=str(e))
            else:
                if cancel_requested:
                    state.stop(CANCEL_REQUESTED)

    async def _complete(self, log: Log, c: Claimed, out: Outcome) -> None:
        """Reports the outcome, retrying transient database failures for up to
        one lease. If it still cannot report, the lease expires and the reaper
        re-runs the attempt (at-least-once)."""
        deadline = time.monotonic() + self.cfg.lease
        backoff = 0.2
        while True:
            try:
                await asyncio.wait_for(self.eng.complete(c, out), 10)
                log.info("attempt finished", outcome=out.kind)
                return
            except LeaseLost:
                log.warning("result discarded: run was reclaimed by the lease reaper", outcome=out.kind)
                return
            except Exception as e:  # noqa: BLE001
                if time.monotonic() > deadline:
                    log.error("could not report attempt outcome; lease will expire and the run will be retried",
                              err=str(e))
                    return
                log.warning("report outcome failed; retrying", err=str(e))
                await asyncio.sleep(backoff)
                backoff = min(backoff * 2, 5)

    # ---- liveness ------------------------------------------------------------

    async def _heartbeat_loop(self) -> None:
        while True:
            await asyncio.sleep(self.cfg.heartbeat)
            await self._beat()

    async def _beat(self) -> None:
        status = WorkerState.DRAINING if self._draining.is_set() else WorkerState.ONLINE
        try:
            async with self.pool.connection() as conn:
                drain = await asyncio.wait_for(worker_store.heartbeat(conn, worker_store.Heartbeat(
                    worker_id=self.id, status=status, active_runs=self.active_runs, memory_bytes=_rss_bytes(),
                    tasks=len(asyncio.all_tasks()), now=_utcnow())), 5)
        except Exception as e:  # noqa: BLE001
            self.log.warning("heartbeat failed", err=str(e))
            return
        if drain and not self._draining.is_set():
            self.log.info("drain requested by operator")
            self.drain()

    async def _cancel_listener(self) -> None:
        """Receives cancellation requests over Redis pub/sub for immediate
        effect. An optimisation: lease renewal also observes
        cancel_requested_at in PostgreSQL within lease/3."""
        while True:
            ps = self.q.pubsub()
            try:
                await ps.subscribe(self.q.channel_cancel)
                async for msg in ps.listen():
                    if msg.get("type") != "message":
                        continue
                    try:
                        run_id = uuid.UUID(str(msg["data"]))
                    except ValueError:
                        continue
                    r = self._active.get(run_id)
                    if r is not None:
                        self.log.info("cancellation signal received", run_id=str(run_id))
                        r.stop(CANCEL_REQUESTED)
            except asyncio.CancelledError:
                raise
            except Exception:  # noqa: BLE001 - reconnect after a pause
                pass
            finally:
                try:
                    await ps.aclose()
                except Exception:  # noqa: BLE001
                    pass
            await asyncio.sleep(2)
