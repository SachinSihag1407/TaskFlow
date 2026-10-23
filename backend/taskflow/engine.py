"""Every lifecycle operation: scheduling occurrences, claiming, completing,
retrying, recovering and cancelling runs.

Invariants enforced here:

  - PostgreSQL is the source of truth. Each operation is one transaction that
    validates transitions with the state machine and applies them with
    conditional UPDATEs, so concurrent actors cannot both succeed.
  - Lock order is always job row, then run rows. This prevents deadlocks
    between the scheduler, workers and API requests.
  - Side effects outside PostgreSQL (Redis publication, SSE events,
    cancellation signals) happen only after commit and are best effort. The
    scheduler's reconciler repairs anything lost between commit and publish.
  - Writes from a worker are fenced by the run's current_attempt_id, so a
    worker whose lease was reclaimed can never overwrite a newer attempt.
"""

from __future__ import annotations

import random
import uuid
from collections.abc import Callable
from dataclasses import dataclass, field
from datetime import UTC, datetime, timedelta
from typing import Any
from uuid import UUID

from psycopg import errors as pgerrors
from psycopg_pool import AsyncConnectionPool

from . import cron, metrics
from . import statemachine as sm
from .db import Conn, is_unique_violation, tx
from .domain import (
    Attempt,
    AttemptStatus,
    Conflict,
    Event,
    Job,
    JobStatus,
    NotFound,
    OverlapPolicy,
    Principal,
    Run,
    RunStatus,
    ScheduleType,
    Severity,
    Trigger,
)
from .logs import Log, redact
from .queue import Item, Queue, QueueEvent
from .retry import Policy
from .store import attempts as attempt_store
from .store import events as event_store
from .store import jobs as job_store
from .store import runs as run_store
from .store import users as user_store
from .store import workers as worker_store


class Stale(Exception):
    """The run is no longer QUEUED (claimed elsewhere, cancelled, or a duplicate
    queue entry). The queue entry should simply be dropped."""


class Deferred(Exception):
    """A concurrency limit is saturated; try again shortly."""


class Paused(Exception):
    """The job is paused; resume re-publishes its queued runs."""


class LeaseLost(Exception):
    """The worker no longer owns the run."""


class Unsupported(Exception):
    """This worker does not run the run's handler."""


# Bounds stored error messages.
MAX_ERROR_LEN = 4000


def utcnow() -> datetime:
    return datetime.now(UTC)


def fmt_duration(seconds: float) -> str:
    """Renders a rounded duration like Go's time.Duration: 45s, 2m30s, 1h0m5s."""
    s = int(round(seconds))
    h, rem = divmod(s, 3600)
    m, sec = divmod(rem, 60)
    if h:
        return f"{h}h{m}m{sec}s"
    if m:
        return f"{m}m{sec}s"
    return f"{sec}s"


@dataclass
class Effects:
    """Post-commit side effects of one transaction."""

    publish: list[Item] = field(default_factory=list)
    events: list[QueueEvent] = field(default_factory=list)
    cancels: list[UUID] = field(default_factory=list)
    removes: list[UUID] = field(default_factory=list)

    def queue_run(self, r: Run) -> None:
        self.publish.append(Item(r.id, r.priority, r.queued_at or r.created_at))

    def queue_refs(self, refs: list[run_store.QueueRef]) -> None:
        self.publish.extend(Item(x.run_id, x.priority, x.queued_at) for x in refs)

    def run_event(self, r: Run, status: RunStatus | str) -> None:
        self.events.append(QueueEvent(type="run.updated", tenant_id=str(r.tenant_id), job_id=str(r.job_id),
                                      run_id=str(r.id), status=str(status)))


@dataclass
class Claimed:
    """A run a worker has won the right to execute."""

    run: Run
    attempt: Attempt
    job_name: str


class OutcomeKind:
    SUCCESS = "success"
    FAILURE = "failure"
    TIMEOUT = "timeout"
    CANCELLED = "cancelled"      # cooperative cancellation completed
    INTERRUPTED = "interrupted"  # worker shutting down
    PANIC = "panic"              # the handler raised an unexpected exception


@dataclass
class Outcome:
    """A worker's report for one attempt."""

    kind: str
    error: str | None = None
    retryable: bool = False
    result: Any = None


def effective_concurrency(j: Job) -> int:
    """Runs of a job allowed to execute at once. Only overlap_policy=allow
    permits more than one."""
    if j.overlap_policy == OverlapPolicy.ALLOW and j.max_concurrency > 1:
        return j.max_concurrency
    return 1


def _job_event(j: Job, typ: str, sev: Severity, msg: str) -> Event:
    return Event(tenant_id=j.tenant_id, job_id=j.id, type=typ, severity=sev, message=msg)


def _run_event(r: Run, typ: str, sev: Severity, msg: str, data: dict | None = None) -> Event:
    return Event(tenant_id=r.tenant_id, job_id=r.job_id, run_id=r.id, worker_id=r.worker_id, type=typ,
                 severity=sev, message=msg, data=data)


class Engine:
    def __init__(self, pool: AsyncConnectionPool, pub: Queue | None, log: Log,
                 now: Callable[[], datetime] = utcnow, rand: Callable[[], float] = random.random):
        self.pool = pool
        self.pub = pub  # None in Redis-free mode and in tests
        self.log = log
        # Injectable for deterministic tests.
        self.now = now
        self.rand = rand

    # ---- shared helpers ------------------------------------------------------

    async def flush(self, f: Effects | None) -> None:
        """Applies side effects after commit. Failures are logged, counted and
        otherwise tolerated: PostgreSQL already holds the authoritative state."""
        if self.pub is None or f is None:
            return
        if f.publish:
            try:
                await self.pub.publish(*f.publish)
            except Exception as e:  # noqa: BLE001
                metrics.REDIS_ERRORS.labels("publish").inc()
                self.log.warning("publish to redis failed; reconciler will retry", runs=len(f.publish), err=str(e))
            else:
                try:
                    async with self.pool.connection() as conn:
                        await run_store.mark_published(conn, [i.run_id for i in f.publish], self.now())
                except Exception as e:  # noqa: BLE001
                    self.log.warning("mark published failed", err=str(e))
        for run_id in f.removes:
            try:
                await self.pub.remove(run_id)
            except Exception:  # noqa: BLE001
                metrics.REDIS_ERRORS.labels("remove").inc()
        for run_id in f.cancels:
            try:
                await self.pub.publish_cancel(run_id)
            except Exception as e:  # noqa: BLE001
                metrics.REDIS_ERRORS.labels("publish_cancel").inc()
                self.log.warning("cancel signal failed; worker will observe it on lease renewal",
                                 run_id=str(run_id), err=str(e))
        for ev in f.events:
            try:
                await self.pub.publish_event(ev)
            except Exception:  # noqa: BLE001
                metrics.REDIS_ERRORS.labels("publish_event").inc()
                break  # pub/sub is down; skip the rest rather than log per event

    async def sync_job_status(self, conn: Conn, j: Job, f: Effects | None) -> JobStatus:
        """Recomputes a job's aggregate status from its in-flight runs and
        persists it if it changed. The caller must hold the job's row lock and
        must have applied pending changes to j (next_run_at, paused_at, ...)."""
        summary = await run_store.active_summary(conn, j.id)
        summary.last_terminal = j.last_run_status
        status = sm.derive_job_status(sm.JobState(
            recurring=j.recurring, paused=j.paused_at is not None, cancelled=j.cancelled_at is not None,
            has_next_run=j.next_run_at is not None), summary)
        if status != j.status:
            await job_store.set_status(conn, j.id, status, self.now())
            j.status = status
            if f is not None:
                f.events.append(QueueEvent(type="job.updated", tenant_id=str(j.tenant_id), job_id=str(j.id),
                                           status=str(status)))
        return status

    async def record_event(self, conn: Conn, ev: Event) -> None:
        """Appends an operational event. Events are diagnostic: a failure is
        logged, and the savepoint keeps it from aborting the business transaction."""
        if ev.created_at is None:
            ev.created_at = self.now()
        try:
            async with conn.transaction():
                await event_store.insert_event(conn, ev)
        except pgerrors.Error as e:
            self.log.warning("record event failed", type=ev.type, err=str(e))

    async def audit(self, conn: Conn, actor: Principal | None, action: str, target_type: str, target_id: str,
                    meta: dict | None = None) -> None:
        """Writes an audit record. Failures abort the operation: a sensitive
        action must not succeed without its audit trail."""
        await event_store.insert_audit(conn, {
            "tenant_id": actor.tenant_id if actor else None,
            "actor_id": actor.user_id if actor else None,
            "actor_email": actor.email if actor else None,
            "action": action, "target_type": target_type, "target_id": target_id,
            "ip": (actor.ip or None) if actor else None,
            "user_agent": (actor.user_agent[:256] or None) if actor else None,
            "metadata": meta, "created_at": self.now(),
        })

    def _failure_target(self, r: Run, retryable: bool) -> tuple[RunStatus, datetime | None]:
        """Where a failed RUNNING run goes next."""
        if not retryable or r.attempt >= r.max_attempts:
            return RunStatus.FAILED, None
        delay = Policy(r.backoff_base_seconds, r.backoff_max_seconds).delay(r.attempt, self.rand)
        return RunStatus.RETRY_WAIT, self.now() + timedelta(seconds=delay)

    # ---- scheduling ----------------------------------------------------------

    async def schedule_due(self, limit: int) -> int:
        """Turns due job occurrences into QUEUED runs. Safe to run from several
        scheduler instances: due jobs are locked with SKIP LOCKED, and the
        unique (job_id, scheduled_for) index rejects any occurrence that was
        already enqueued."""
        now = self.now()
        f = Effects()
        created = 0
        async with tx(self.pool) as conn:
            for j in await job_store.lock_due(conn, now, limit):
                created += await self._schedule_job_isolated(conn, j, now, f)
        await self.flush(f)
        return created

    async def _schedule_job_isolated(self, conn: Conn, j: Job, now: datetime, f: Effects) -> int:
        # Each job runs in a savepoint so one job that cannot be scheduled
        # (corrupt row, constraint violation) does not stall the batch.
        mark, ev_mark = len(f.publish), len(f.events)
        try:
            async with conn.transaction():
                return await self._schedule_job(conn, j, now, f)
        except Exception as e:  # noqa: BLE001
            del f.publish[mark:]
            del f.events[ev_mark:]
            self.log.error("could not schedule job; skipping it this tick", job_id=str(j.id), err=str(e))
            return 0

    async def _schedule_job(self, conn: Conn, j: Job, now: datetime, f: Effects) -> int:
        occurrences: list[datetime] = []
        nxt: datetime | None = None
        if j.schedule_type in (ScheduleType.ONCE, ScheduleType.DELAYED):
            occurrences = [j.next_run_at]
        else:
            try:
                spec = cron.parse(j.cron_expr or "", j.timezone)
            except cron.CronError as e:
                # Validated on write, so the tz database changed under us. Stop
                # scheduling rather than spinning on the job every tick.
                await self.record_event(conn, _job_event(j, "job.schedule_invalid", Severity.ERROR,
                                                         f"Schedule could not be evaluated and was suspended: {e}"))
                j.next_run_at = None
                await job_store.set_next_run(conn, j.id, None, now)
                await self.sync_job_status(conn, j, f)
                return 0
            plan = cron.plan_cron(spec, j.next_run_at, now, str(j.missed_run_policy))
            occurrences, nxt = plan.enqueue, plan.next
            if plan.skipped:
                metrics.OCCURRENCES_SKIPPED.labels("missed_run_policy").inc(plan.skipped)
                await self.record_event(conn, _job_event(
                    j, "job.occurrences_missed", Severity.WARNING,
                    f"{plan.skipped} missed occurrence(s) not run (missed-run policy: {j.missed_run_policy})"))
            # With overlap "skip", at most one run of a recurring job may be in
            # flight, so extra occurrences are dropped.
            if j.overlap_policy == OverlapPolicy.SKIP and occurrences:
                summary = await run_store.active_summary(conn, j.id)
                if summary.active > 0:
                    metrics.OCCURRENCES_SKIPPED.labels("overlap").inc(len(occurrences))
                    await self.record_event(conn, _job_event(
                        j, "job.occurrence_skipped", Severity.WARNING,
                        f"Occurrence at {occurrences[-1].isoformat().replace('+00:00', 'Z')} skipped: "
                        "the previous run is still active (overlap policy: skip)"))
                    occurrences = []
                elif len(occurrences) > 1:
                    metrics.OCCURRENCES_SKIPPED.labels("overlap").inc(len(occurrences) - 1)
                    occurrences = occurrences[-1:]

        created = 0
        for occ in occurrences:
            run = run_store.from_job(j, occ, Trigger.SCHEDULE, now)
            if not await run_store.insert(conn, run):
                # Duplicate occurrence: already enqueued by an earlier tick.
                self.log.info("duplicate occurrence ignored", job_id=str(j.id), scheduled_for=occ.isoformat())
                continue
            created += 1
            metrics.RUNS_ENQUEUED.labels(Trigger.SCHEDULE.value).inc()
            f.queue_run(run)
            f.run_event(run, RunStatus.QUEUED)

        j.next_run_at = nxt
        await job_store.set_next_run(conn, j.id, nxt, now)
        await self.sync_job_status(conn, j, f)
        return created

    async def promote_retries(self, limit: int) -> int:
        """Moves RETRY_WAIT runs whose backoff elapsed back to QUEUED."""
        now = self.now()
        async with self.pool.connection() as conn:
            refs = await run_store.due_retry_candidates(conn, now, limit)
        by_job: dict[UUID, list[UUID]] = {}
        for run_id, job_id in refs:
            by_job.setdefault(job_id, []).append(run_id)
        promoted = 0
        for job_id, run_ids in by_job.items():
            f = Effects()
            async with tx(self.pool) as conn:
                j = await job_store.lock(conn, job_id)
                for run_id in run_ids:
                    try:
                        r = await run_store.lock_if_status(conn, run_id, RunStatus.RETRY_WAIT)
                    except NotFound:
                        continue  # cancelled or promoted concurrently
                    if r.next_attempt_at is None or r.next_attempt_at > now:
                        continue
                    await run_store.transition(conn, r.id, RunStatus.RETRY_WAIT, run_store.Transition(RunStatus.QUEUED, now))
                    r.queued_at = now
                    f.queue_run(r)
                    f.run_event(r, RunStatus.QUEUED)
                    promoted += 1
                await self.sync_job_status(conn, j, f)
            await self.flush(f)
        return promoted

    async def recover_expired_leases(self, limit: int) -> int:
        """Handles RUNNING runs whose worker stopped renewing its lease (crash,
        OOM kill, partition, long pause). The attempt is marked LOST — its
        outcome is unknown — and the run is retried if attempts remain,
        otherwise dead-lettered. This is where at-least-once delivery comes
        from, so handlers must be idempotent."""
        now = self.now()
        async with self.pool.connection() as conn:
            candidates = await run_store.expired_lease_candidates(conn, now, limit)
        recovered = 0
        for c in candidates:
            f = Effects()
            ok = False
            async with tx(self.pool) as conn:
                j = await job_store.lock(conn, c.job_id)
                r = await run_store.lock(conn, c.id)
                # Re-check under lock: the worker may have renewed or finished.
                if (r.status == RunStatus.RUNNING and r.lease_expires_at is not None and r.lease_expires_at < now
                        and r.current_attempt_id is not None and r.current_attempt_id == c.current_attempt_id):
                    await self._recover_locked(conn, j, r, now, f)
                    ok = True
            if ok:
                recovered += 1
                metrics.LEASES_RECOVERED.inc()
                metrics.ATTEMPTS_FINISHED.labels(c.handler, AttemptStatus.LOST.value).inc()
            await self.flush(f)
        return recovered

    async def _recover_locked(self, conn: Conn, j: Job, r: Run, now: datetime, f: Effects) -> None:
        attempt_id = r.current_attempt_id
        who = f"Worker {str(r.worker_id)[:8]}" if r.worker_id else "Worker"
        msg = (f"{who} stopped renewing its lease (crash, restart, network partition or overload). "
               "The attempt's outcome is unknown.")
        to, nxt = self._failure_target(r, True)
        if r.cancel_requested_at is not None:
            to, nxt = RunStatus.CANCELLED, None
        await attempt_store.finish(conn, attempt_id, attempt_store.Outcome(
            AttemptStatus.LOST, now, error_kind="lease_expired", error_message=msg,
            retryable=to == RunStatus.RETRY_WAIT, next_retry_at=nxt))
        await run_store.transition(conn, r.id, RunStatus.RUNNING, run_store.Transition(
            to, now, attempt_id=attempt_id, next_attempt_at=nxt, last_error=msg, dead_letter=to == RunStatus.FAILED))
        if r.worker_id:
            await worker_store.increment_counters(conn, r.worker_id, 0, 1)
        if to.terminal:
            await job_store.set_last_run(conn, j.id, now, to)
            j.last_run_status, j.last_run_at = to, now
        sev = Severity.WARNING
        text = f"Recovered run after lease expiry (attempt {r.attempt}/{r.max_attempts}); retry scheduled"
        if to == RunStatus.FAILED:
            sev = Severity.ERROR
            text = f"Run lost on attempt {r.attempt}/{r.max_attempts} and moved to the dead-letter queue"
        elif to == RunStatus.CANCELLED:
            text = "Run lost while cancellation was pending; marked cancelled"
        await self.record_event(conn, _run_event(r, "run.recovered", sev, text, {"attempt": r.attempt}))
        f.run_event(r, to)
        await self.sync_job_status(conn, j, f)

    async def reconcile(self, older_than: timedelta, limit: int) -> int:
        """Re-publishes QUEUED runs that may be missing from Redis: failed
        publications, entries lost in a Redis restart, and entries popped by a
        worker that crashed before claiming. ZADD NX makes this idempotent and
        the PostgreSQL claim makes duplicates harmless."""
        if self.pub is None:
            return 0
        async with self.pool.connection() as conn:
            refs = await run_store.unpublished_queued(conn, self.now() - older_than, limit)
        if not refs:
            return 0
        f = Effects()
        f.queue_refs(refs)
        try:
            await self.pub.publish(*f.publish)
        except Exception:
            metrics.REDIS_ERRORS.labels("reconcile").inc()
            raise
        async with self.pool.connection() as conn:
            await run_store.mark_published(conn, [x.run_id for x in refs], self.now())
        metrics.REPUBLISHED.inc(len(refs))
        return len(refs)

    # ---- execution -----------------------------------------------------------

    async def claim(self, run_id: UUID, worker_id: UUID, lease: timedelta,
                    supports: Callable[[str], bool] | None = None) -> Claimed:
        """Atomically moves a QUEUED run to RUNNING for worker_id and opens a
        new attempt. Exactly one concurrent caller can succeed for a run.

        Concurrency limits are checked under the job's row lock (per-job limit)
        and a transaction-scoped advisory lock per tenant (workspace limit), so
        two workers cannot both observe a free slot and exceed the limit."""
        async with self.pool.connection() as conn:
            try:
                peek = await run_store.get_unscoped(conn, run_id)
            except NotFound:
                raise Stale() from None
        if peek.status != RunStatus.QUEUED:
            raise Stale()
        if supports is not None and not supports(peek.handler):
            raise Unsupported()

        f = Effects()
        claimed: Claimed | None = None
        async with tx(self.pool) as conn:
            now = self.now()
            j = await job_store.lock(conn, peek.job_id)
            # Blocking lock (not SKIP LOCKED): the job row lock already
            # serialises claims of this job, and a run row can be locked
            # briefly by unrelated writers such as mark_published. Skipping it
            # would wrongly treat a claimable run as stale.
            try:
                r = await run_store.lock(conn, run_id)
            except NotFound:
                raise Stale() from None
            if r.status != RunStatus.QUEUED:
                raise Stale()
            if j.cancelled_at is not None:
                # Should already be cancelled; close the gap (committed below).
                await run_store.transition(conn, r.id, RunStatus.QUEUED, run_store.Transition(RunStatus.CANCELLED, now))
                await self.sync_job_status(conn, j, f)
            else:
                if j.paused_at is not None:
                    raise Paused()
                if await run_store.count_running(conn, j.id) >= effective_concurrency(j):
                    raise Deferred()
                limit = await user_store.tenant_concurrency_limit(conn, j.tenant_id)
                if limit > 0:
                    await conn.execute("SELECT pg_advisory_xact_lock(hashtextextended(%s, 0))", (f"tenant:{j.tenant_id}",))
                    if await run_store.count_tenant_running(conn, j.tenant_id) >= limit:
                        raise Deferred()
                a = Attempt(id=uuid.uuid4(), run_id=r.id, job_id=r.job_id, tenant_id=r.tenant_id,
                            attempt_number=r.attempt + 1, worker_id=worker_id, handler=r.handler,
                            status=AttemptStatus.RUNNING, queued_at=r.queued_at, started_at=now)
                await attempt_store.insert(conn, a)
                lease_until = now + lease
                try:
                    await run_store.mark_claimed(conn, r.id, a.id, worker_id, a.attempt_number, lease_until, now)
                except NotFound:
                    raise Stale() from None
                r.status, r.attempt, r.current_attempt_id = RunStatus.RUNNING, a.attempt_number, a.id
                r.worker_id, r.lease_expires_at = worker_id, lease_until
                r.started_at = r.started_at or now
                await self.sync_job_status(conn, j, f)
                f.run_event(r, RunStatus.RUNNING)
                claimed = Claimed(run=r, attempt=a, job_name=j.name)
        await self.flush(f)
        if claimed is None:
            raise Stale()
        if claimed.run.queued_at is not None:
            metrics.QUEUE_WAIT.observe((claimed.attempt.started_at - claimed.run.queued_at).total_seconds())
        return claimed

    async def claim_next(self, worker_id: UUID, handlers: list[str], lease: timedelta) -> Claimed | None:
        """The Redis-independent dispatch path: scans PostgreSQL for claimable
        runs in priority order and claims the first one it can. A Redis outage
        (or Redis-free mode) degrades latency but never stops execution."""
        async with self.pool.connection() as conn:
            ids = await run_store.claim_candidates(conn, handlers, 20)
        for run_id in ids:
            try:
                return await self.claim(run_id, worker_id, lease)  # candidates are pre-filtered by handler
            except (Stale, Deferred, Paused):
                continue
        return None

    async def renew_lease(self, c: Claimed, lease: timedelta) -> bool:
        """Extends the lease and reports whether cancellation was requested.
        Raises LeaseLost if the reaper reclaimed the run."""
        try:
            async with self.pool.connection() as conn:
                return await run_store.renew_lease(conn, c.run.id, c.attempt.id, self.now() + lease)
        except NotFound:
            raise LeaseLost() from None

    async def save_checkpoint(self, c: Claimed, cp: Any) -> None:
        """Persists handler progress so a retry can resume."""
        try:
            async with self.pool.connection() as conn:
                await run_store.save_checkpoint(conn, c.run.id, c.attempt.id, cp)
        except NotFound:
            raise LeaseLost() from None

    async def complete(self, c: Claimed, out: Outcome) -> None:
        """Records an attempt's outcome and moves the run on:

          - success -> SUCCEEDED
          - cancelled, or any failure while cancellation is pending -> CANCELLED
          - retryable failure with attempts left -> RETRY_WAIT (backoff + jitter)
          - otherwise -> FAILED and dead-lettered

        Raises LeaseLost (writing nothing) if the worker no longer owns the run."""
        f = Effects()
        try:
            async with tx(self.pool) as conn:
                final, attempt_status = await self._complete_locked(conn, c, out, f)
        except LeaseLost:
            metrics.LEASE_LOST.inc()
            raise
        metrics.ATTEMPTS_FINISHED.labels(c.run.handler, attempt_status.value).inc()
        metrics.ATTEMPT_DURATION.labels(c.run.handler).observe((self.now() - c.attempt.started_at).total_seconds())
        if final.terminal:
            metrics.RUNS_FINISHED.labels(c.run.handler, final.value).inc()
        await self.flush(f)

    async def _complete_locked(self, conn: Conn, c: Claimed, out: Outcome,
                               f: Effects) -> tuple[RunStatus, AttemptStatus]:
        now = self.now()
        j = await job_store.lock(conn, c.run.job_id)
        r = await run_store.lock(conn, c.run.id)
        if r.status != RunStatus.RUNNING or r.current_attempt_id != c.attempt.id:
            raise LeaseLost()

        err_msg = None
        if out.error is not None:
            err_msg = redact(out.error)
            if len(err_msg) > MAX_ERROR_LEN:
                err_msg = err_msg[:MAX_ERROR_LEN] + "…"
        nxt: datetime | None = None
        err_kind: str | None = None
        if out.kind == OutcomeKind.SUCCESS:
            final, attempt_status = RunStatus.SUCCEEDED, AttemptStatus.SUCCEEDED
        elif out.kind == OutcomeKind.CANCELLED or r.cancel_requested_at is not None:
            final, attempt_status, err_kind = RunStatus.CANCELLED, AttemptStatus.CANCELLED, "cancelled"
        else:
            attempt_status = AttemptStatus.TIMED_OUT if out.kind == OutcomeKind.TIMEOUT else AttemptStatus.FAILED
            err_kind = out.kind
            if out.kind == OutcomeKind.FAILURE:
                err_kind = "handler_error" if out.retryable else "permanent_error"
            if out.kind == OutcomeKind.INTERRUPTED and r.attempt < r.max_attempts:
                # Worker shutdown is not the job's fault: retry promptly.
                final, nxt = RunStatus.RETRY_WAIT, now + timedelta(seconds=1)
            else:
                final, nxt = self._failure_target(r, out.retryable)

        retryable = None
        if attempt_status not in (AttemptStatus.SUCCEEDED, AttemptStatus.CANCELLED):
            retryable = final == RunStatus.RETRY_WAIT
        await attempt_store.finish(conn, c.attempt.id, attempt_store.Outcome(
            attempt_status, now, error_kind=err_kind, error_message=err_msg, retryable=retryable, next_retry_at=nxt))
        t = run_store.Transition(final, now, attempt_id=c.attempt.id, next_attempt_at=nxt,
                                 dead_letter=final == RunStatus.FAILED)
        if final == RunStatus.SUCCEEDED:
            t.result = out.result
        elif err_msg is not None:
            t.last_error = err_msg
        try:
            await run_store.transition(conn, r.id, RunStatus.RUNNING, t)
        except NotFound:
            raise LeaseLost() from None
        if r.worker_id:
            ok = 1 if attempt_status == AttemptStatus.SUCCEEDED else 0
            failed = 1 if attempt_status not in (AttemptStatus.SUCCEEDED, AttemptStatus.CANCELLED) else 0
            await worker_store.increment_counters(conn, r.worker_id, ok, failed)
        if final.terminal:
            await job_store.set_last_run(conn, j.id, now, final)
            j.last_run_status, j.last_run_at = final, now

        if final == RunStatus.RETRY_WAIT:
            await self.record_event(conn, _run_event(
                r, "run.retry_scheduled", Severity.WARNING,
                f"Attempt {r.attempt}/{r.max_attempts} failed; retrying in {fmt_duration((nxt - now).total_seconds())}",
                {"attempt": r.attempt, "error": err_msg or ""}))
        elif final == RunStatus.FAILED:
            await self.record_event(conn, _run_event(
                r, "run.dead_lettered", Severity.ERROR,
                f"Run failed after {r.attempt} attempt(s) and was moved to the dead-letter queue",
                {"attempt": r.attempt, "error": err_msg or ""}))
        elif final == RunStatus.CANCELLED:
            await self.record_event(conn, _run_event(r, "run.cancelled", Severity.INFO, "Run cancelled during execution"))
        f.run_event(r, final)

        await self.sync_job_status(conn, j, f)
        # A concurrency slot just freed up: nudge deferred runs of this job so
        # they do not wait for the delayed-set timer.
        f.queue_refs(await run_store.queued_refs_for_job(conn, j.id, effective_concurrency(j)))
        return final, attempt_status

    # ---- operator actions ----------------------------------------------------

    async def _require_action(self, conn: Conn, j: Job, action: str) -> sm.RunSummary:
        """Fails with a conflict unless the action is currently permitted, using
        the same rules the API exposes."""
        summary = await run_store.active_summary(conn, j.id)
        replayable = False
        if action == sm.RETRY:
            try:
                replayable = (await run_store.last_failed(conn, j.id)).replayed_at is None
            except NotFound:
                pass
        st = sm.job_actions(j.status, j.recurring, summary, replayable)[action]
        if not st["allowed"]:
            raise Conflict("action_not_allowed", st["reason"])
        return summary

    async def cancel_job(self, actor: Principal, job_id: UUID) -> Job:
        """Stops the schedule, cancels waiting runs and asks running runs to stop."""
        f = Effects()
        async with tx(self.pool) as conn:
            now = self.now()
            j = await job_store.lock_tenant_job(conn, actor.tenant_id, job_id)
            await self._require_action(conn, j, sm.CANCEL)
            await job_store.set_cancelled(conn, j.id, now)
            j.cancelled_at, j.next_run_at = now, None
            j.version += 1
            stopping = 0
            for r in await run_store.lock_active(conn, j.id):
                await self._cancel_run_locked(conn, j, r, f)
                if r.status == RunStatus.RUNNING:
                    stopping += 1
            msg = f"Job cancelled; {stopping} running run(s) asked to stop" if stopping else "Job cancelled"
            await self.record_event(conn, _job_event(j, "job.cancelled", Severity.INFO, msg))
            await self.audit(conn, actor, "job.cancel", "job", str(j.id), {"name": j.name})
            await self.sync_job_status(conn, j, f)
        await self.flush(f)
        return j

    async def _cancel_run_locked(self, conn: Conn, j: Job, r: Run, f: Effects) -> None:
        """Waiting runs move straight to CANCELLED; a RUNNING run is flagged and
        its worker cancels the handler. The run reaches CANCELLED when the
        worker reports back — or stays SUCCEEDED if the handler finished first."""
        now = self.now()
        if r.status in (RunStatus.QUEUED, RunStatus.RETRY_WAIT):
            await run_store.transition(conn, r.id, RunStatus(r.status), run_store.Transition(RunStatus.CANCELLED, now))
            f.removes.append(r.id)
            f.run_event(r, RunStatus.CANCELLED)
            # A cancelled run is a terminal outcome: record it so the job
            # status derives correctly (a one-time job whose run was cancelled).
            await job_store.set_last_run(conn, j.id, now, RunStatus.CANCELLED)
            j.last_run_status, j.last_run_at = RunStatus.CANCELLED, now
            metrics.RUNS_FINISHED.labels(r.handler, RunStatus.CANCELLED.value).inc()
        elif r.status == RunStatus.RUNNING:
            await run_store.set_cancel_requested(conn, r.id, now)
            f.cancels.append(r.id)
            f.run_event(r, RunStatus.RUNNING)
        else:
            raise Conflict("run_finished", f"run already finished with status {r.status}")

    async def cancel_run(self, actor: Principal, run_id: UUID) -> Run:
        async with self.pool.connection() as conn:
            peek = await run_store.get(conn, actor.tenant_id, run_id)
        f = Effects()
        async with tx(self.pool) as conn:
            j = await job_store.lock_tenant_job(conn, actor.tenant_id, peek.job_id)
            r = await run_store.lock(conn, run_id)
            if r.status == RunStatus.RUNNING and r.cancel_requested_at is not None:
                raise Conflict("cancel_pending", "cancellation is already pending for this run")
            await self._cancel_run_locked(conn, j, r, f)
            if r.status != RunStatus.RUNNING:
                await self.record_event(conn, _run_event(r, "run.cancelled", Severity.INFO, "Run cancelled before execution"))
            else:
                await self.record_event(conn, _run_event(r, "run.cancel_requested", Severity.INFO,
                                                         "Cancellation requested for running run"))
            await self.audit(conn, actor, "run.cancel", "run", str(r.id), {"job_id": str(j.id), "status": r.status})
            await self.sync_job_status(conn, j, f)
            out = await run_store.get(conn, actor.tenant_id, run_id)
        await self.flush(f)
        return out

    async def pause_job(self, actor: Principal, job_id: UUID) -> Job:
        """Stops new occurrences and holds QUEUED runs; executing runs continue."""
        return await self._toggle_pause(actor, job_id, True)

    async def resume_job(self, actor: Principal, job_id: UUID) -> Job:
        """Occurrences missed while paused follow the job's missed-run policy."""
        return await self._toggle_pause(actor, job_id, False)

    async def _toggle_pause(self, actor: Principal, job_id: UUID, pause: bool) -> Job:
        f = Effects()
        if pause:
            action, audit_action, ev_type, msg = sm.PAUSE, "job.pause", "job.paused", "Job paused"
        else:
            action, audit_action, ev_type, msg = sm.RESUME, "job.resume", "job.resumed", "Job resumed"
        async with tx(self.pool) as conn:
            now = self.now()
            j = await job_store.lock_tenant_job(conn, actor.tenant_id, job_id)
            await self._require_action(conn, j, action)
            at = now if pause else None
            await job_store.set_paused(conn, j.id, at, now)
            j.paused_at = at
            j.version += 1
            if not pause:
                f.queue_refs(await run_store.queued_refs_for_job(conn, j.id, 500))
            await self.record_event(conn, _job_event(j, ev_type, Severity.INFO, msg))
            await self.audit(conn, actor, audit_action, "job", str(j.id), {"name": j.name})
            await self.sync_job_status(conn, j, f)
        await self.flush(f)
        return j

    async def retry_job(self, actor: Principal, job_id: UUID) -> Run:
        """Re-runs a failed one-time job by replaying its last failed run."""
        f = Effects()
        async with tx(self.pool) as conn:
            j = await job_store.lock_tenant_job(conn, actor.tenant_id, job_id)
            await self._require_action(conn, j, sm.RETRY)
            last = await run_store.last_failed(conn, j.id)
            run = await self._replay_locked(conn, actor, j, last.id, Trigger.RETRY, f)
        await self.flush(f)
        return run

    async def replay_run(self, actor: Principal, run_id: UUID) -> Run:
        """Re-enqueues a dead-lettered run as a new run linked through
        replay_of. Each failed run can be replayed at most once (unique index),
        so concurrent replay requests cannot double-enqueue."""
        async with self.pool.connection() as conn:
            peek = await run_store.get(conn, actor.tenant_id, run_id)
        f = Effects()
        async with tx(self.pool) as conn:
            j = await job_store.lock_tenant_job(conn, actor.tenant_id, peek.job_id)
            if j.cancelled_at is not None:
                raise Conflict("job_cancelled", "the parent job is cancelled; runs of cancelled jobs cannot be replayed")
            run = await self._replay_locked(conn, actor, j, run_id, Trigger.REPLAY, f)
        await self.flush(f)
        return run

    async def _replay_locked(self, conn: Conn, actor: Principal, j: Job, run_id: UUID, trigger: Trigger,
                             f: Effects) -> Run:
        now = self.now()
        old = await run_store.lock(conn, run_id)
        if old.status != RunStatus.FAILED:
            raise Conflict("not_replayable", f"only failed runs can be replayed (status is {old.status})")
        if old.replayed_at is not None:
            raise Conflict("already_replayed", "this run has already been replayed")
        await run_store.mark_replayed(conn, old.id, now)
        # Replays reproduce the original execution: same payload and policy
        # snapshot as the failed run, fresh attempt budget.
        nr = old.model_copy(update={
            "id": uuid.uuid4(), "scheduled_for": now, "trigger": trigger.value, "status": RunStatus.QUEUED.value,
            "queued_at": now, "replay_of": old.id, "created_at": now, "updated_at": now, "attempt": 0,
            "current_attempt_id": None, "worker_id": None, "lease_expires_at": None, "started_at": None,
            "finished_at": None, "next_attempt_at": None, "cancel_requested_at": None, "last_error": None,
            "result": None, "checkpoint": None, "dead_lettered_at": None, "replayed_at": None})
        try:
            async with conn.transaction():
                await run_store.insert(conn, nr)
        except pgerrors.UniqueViolation as e:
            if is_unique_violation(e, "job_runs_replay_key"):
                raise Conflict("already_replayed", "this run has already been replayed") from None
            raise
        metrics.RUNS_ENQUEUED.labels(trigger.value).inc()
        verb = "Manually retried failed run" if trigger == Trigger.RETRY else "Replayed dead-lettered run"
        await self.record_event(conn, _run_event(nr, f"run.{trigger.value}", Severity.INFO,
                                                 f"{verb} {str(old.id)[:8]}", {"replay_of": str(old.id)}))
        await self.audit(conn, actor, f"run.{trigger.value}", "run", str(old.id),
                         {"job_id": str(j.id), "new_run_id": str(nr.id), "job_name": j.name})
        await self.sync_job_status(conn, j, f)
        f.queue_run(nr)
        f.run_event(nr, RunStatus.QUEUED)
        nr.job_name = j.name
        return nr

    async def run_now(self, actor: Principal, job_id: UUID) -> Run:
        """Enqueues an immediate, manually triggered run of a recurring job."""
        f = Effects()
        async with tx(self.pool) as conn:
            now = self.now()
            j = await job_store.lock_tenant_job(conn, actor.tenant_id, job_id)
            summary = await self._require_action(conn, j, sm.RUN_NOW)
            if j.overlap_policy == OverlapPolicy.SKIP and summary.active > 0:
                raise Conflict("run_active", "a run is already active and the overlap policy is skip")
            run = run_store.from_job(j, now, Trigger.MANUAL, now)
            await run_store.insert(conn, run)
            metrics.RUNS_ENQUEUED.labels(Trigger.MANUAL.value).inc()
            await self.record_event(conn, _run_event(run, "run.manual", Severity.INFO, "Run triggered manually"))
            await self.audit(conn, actor, "job.run_now", "job", str(j.id), {"run_id": str(run.id)})
            await self.sync_job_status(conn, j, f)
            f.queue_run(run)
            f.run_event(run, RunStatus.QUEUED)
            run.job_name = j.name
        await self.flush(f)
        return run
