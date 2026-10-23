"""Worker runtime integration tests: a real worker executes real handlers
while a background loop plays the scheduler's role."""

from __future__ import annotations

import asyncio
import contextlib
import time
import uuid

from taskflow.domain import RunStatus
from taskflow.engine import Engine
from taskflow.handlers import Execution
from taskflow.logs import null_logger
from taskflow.services.jobs import JobInput, Jobs
from taskflow.worker import Worker, WorkerConfig

FAST = WorkerConfig(concurrency=4, lease=3, heartbeat=0.5, shutdown_grace=1, poll_interval=0.1)


async def wait_for(pred, within: float = 15, every: float = 0.1):
    deadline = time.monotonic() + within
    while time.monotonic() < deadline:
        if v := await pred():
            return v
        await asyncio.sleep(every)
    raise AssertionError("condition not met in time")


@contextlib.asynccontextmanager
async def running(pool, eng: Engine, registry, cfg: WorkerConfig = FAST, q=None):
    """A worker plus a scheduler-like loop; yields (worker, stop_event)."""
    stop = asyncio.Event()
    w = Worker(cfg, eng, pool, q, registry, null_logger())

    async def control():
        while True:
            await eng.schedule_due(100)
            await eng.promote_retries(100)
            await eng.recover_expired_leases(100)
            if q is not None:
                with contextlib.suppress(Exception):  # Redis may be down; the real scheduler tolerates it too
                    await q.promote_delayed(eng.now(), 100)
            await asyncio.sleep(0.1)

    ctl = asyncio.create_task(control())
    task = asyncio.create_task(w.run(stop))
    try:
        yield w, stop
    finally:
        stop.set()
        await asyncio.wait_for(task, 30)
        ctl.cancel()
        await asyncio.gather(ctl, return_exceptions=True)


async def submit(pool, eng, registry, p, payload, handler="test.sleep", **kw):
    spec = {"name": "w", "handler": handler, "payload": payload, "schedule_type": "once", "backoff_base_seconds": 1,
            "backoff_max_seconds": 1, **kw}
    job, _ = await Jobs(pool, eng, registry, null_logger()).create(p, JobInput(**spec), "")
    return job


async def run_of(pool, job_id):
    async with pool.connection() as conn:
        return await (await conn.execute(
            "SELECT id, status, attempt, result, last_error FROM job_runs WHERE job_id = %s", (job_id,))).fetchone()


async def attempts_of(pool, job_id):
    async with pool.connection() as conn:
        return await (await conn.execute(
            "SELECT attempt_number, status, error_kind, error_message FROM job_attempts WHERE job_id = %s "
            "ORDER BY attempt_number", (job_id,))).fetchall()


def finished(pool, job_id, status=None):
    async def pred():
        r = await run_of(pool, job_id)
        if r and r["status"] in ("SUCCEEDED", "FAILED", "CANCELLED") and (status is None or r["status"] == status):
            return r
        return None
    return pred


async def test_success_with_logs(pool, engine, registry, admin):
    async with running(pool, engine, registry):
        job = await submit(pool, engine, registry, admin, {"duration_ms": 50})
        r = await wait_for(finished(pool, job.id))
    assert r["status"] == RunStatus.SUCCEEDED and r["result"] == {"slept_ms": 50, "attempt": 1}
    async with pool.connection() as conn:
        logs = await (await conn.execute("SELECT message FROM attempt_logs l JOIN job_attempts a ON a.id = l.attempt_id "
                                         "WHERE a.job_id = %s ORDER BY l.id", (job.id,))).fetchall()
    assert [x["message"] for x in logs][-1] == "attempt succeeded"


async def test_retry_then_success(pool, engine, registry, admin):
    async with running(pool, engine, registry):
        job = await submit(pool, engine, registry, admin, {"duration_ms": 10, "fail_attempts": 1}, max_attempts=3)
        r = await wait_for(finished(pool, job.id))
    assert r["status"] == RunStatus.SUCCEEDED and r["attempt"] == 2
    a = await attempts_of(pool, job.id)
    assert [(x["status"], x["error_kind"]) for x in a] == [("FAILED", "handler_error"), ("SUCCEEDED", None)]


async def test_timeout(pool, engine, registry, admin):
    async with running(pool, engine, registry):
        job = await submit(pool, engine, registry, admin, {"duration_ms": 10000}, max_attempts=1, timeout_seconds=1)
        r = await wait_for(finished(pool, job.id))
    assert r["status"] == RunStatus.FAILED
    assert (await attempts_of(pool, job.id))[0]["status"] == "TIMED_OUT"


async def test_operator_cancel_stops_running_handler(pool, engine, registry, admin):
    async with running(pool, engine, registry):
        job = await submit(pool, engine, registry, admin, {"duration_ms": 20000})
        r = await wait_for(lambda: _status(pool, job.id, "RUNNING"))
        start = time.monotonic()
        await engine.cancel_run(admin, r["id"])
        r = await wait_for(finished(pool, job.id))
        assert time.monotonic() - start < 5  # observed on the next lease renewal (lease/3)
    assert r["status"] == RunStatus.CANCELLED


async def _status(pool, job_id, status):
    r = await run_of(pool, job_id)
    return r if r and r["status"] == status else None


async def test_handler_crash_is_contained_and_retried(pool, engine, registry, admin):
    async with running(pool, engine, registry):
        job = await submit(pool, engine, registry, admin, {"duration_ms": 10, "fail_attempts": 1, "panic": True},
                           max_attempts=2)
        r = await wait_for(finished(pool, job.id))
    assert r["status"] == RunStatus.SUCCEEDED
    first = (await attempts_of(pool, job.id))[0]
    assert first["error_kind"] == "panic" and "handler panicked" in first["error_message"]


async def test_shutdown_interrupts_and_retries_promptly(pool, engine, registry, admin):
    cfg = WorkerConfig(concurrency=2, lease=3, heartbeat=0.5, shutdown_grace=0.5, poll_interval=0.1)
    async with running(pool, engine, registry, cfg) as (_, stop):
        job = await submit(pool, engine, registry, admin, {"duration_ms": 20000}, max_attempts=3)
        await wait_for(lambda: _status(pool, job.id, "RUNNING"))
        stop.set()
        await wait_for(lambda: _status(pool, job.id, "RETRY_WAIT"))
    a = await attempts_of(pool, job.id)
    assert a[0]["error_kind"] == "interrupted"


async def test_batch_resumes_from_checkpoint(pool, engine, registry, admin):
    async with running(pool, engine, registry):
        job = await submit(pool, engine, registry, admin,
                           {"total_records": 3000, "chunk_size": 500, "fail_at_record": 1700}, handler="batch.process",
                           max_attempts=2)
        r = await wait_for(finished(pool, job.id))
    assert r["status"] == RunStatus.SUCCEEDED
    assert r["result"]["resumed_from"] == 1500 and r["result"]["processed"] == 3000


async def test_notification_retries_do_not_resend(pool, engine, registry, admin):
    job = await submit(pool, engine, registry, admin, {"channel": "email", "recipients": ["a@example.com", "b@example.com"],
                                                       "subject": "Hi"}, handler="notification.send")
    await engine.schedule_due(100)
    run = await run_of(pool, job.id)
    h = registry.get("notification.send")

    class Log:
        def info(self, m): pass
        def warning(self, m): pass
        def error(self, m): pass

    async def save(_): pass

    def x(attempt):
        return Execution(run_id=run["id"], job_id=job.id, tenant_id=admin.tenant_id, attempt_id=uuid.uuid4(),
                         attempt=attempt, max_attempts=3, payload=job.payload, checkpoint=None,
                         idempotency_key=str(run["id"]), log=Log(), save_checkpoint=save)

    assert (await h.execute(x(1)))["delivered"] == 2
    second = await h.execute(x(2))
    assert second["delivered"] == 0 and second["deduplicated"] == 2


async def test_redis_dispatch(pool, queue, registry, admin):
    eng = Engine(pool, queue, null_logger())
    async with running(pool, eng, registry, q=queue):
        job = await submit(pool, eng, registry, admin, {"duration_ms": 10})
        r = await wait_for(finished(pool, job.id))
    assert r["status"] == RunStatus.SUCCEEDED


async def test_worker_never_exceeds_its_concurrency(pool, engine, registry, admin):
    cfg = WorkerConfig(concurrency=2, lease=3, heartbeat=0.5, shutdown_grace=1, poll_interval=0.05)
    jobs = [await submit(pool, engine, registry, admin, {"duration_ms": 200}) for _ in range(8)]
    peak = 0
    async with running(pool, engine, registry, cfg) as (w, _):
        async def all_done():
            nonlocal peak
            async with pool.connection() as conn:
                row = await (await conn.execute(
                    "SELECT count(*) FILTER (WHERE status = 'RUNNING') AS running, "
                    "count(*) FILTER (WHERE status = 'SUCCEEDED') AS ok FROM job_runs")).fetchone()
            peak = max(peak, row["running"], w.active_runs)
            return row["ok"] == len(jobs)
        await wait_for(all_done, every=0.02)
    assert peak == 2


async def test_crashed_workers_run_is_recovered_by_another_worker(pool, engine, registry, admin):
    from datetime import timedelta

    from .conftest import make_worker
    job = await submit(pool, engine, registry, admin, {"duration_ms": 10}, max_attempts=3)
    await engine.schedule_due(100)
    r = await run_of(pool, job.id)
    # A worker claims the run with a 1 s lease and then "crashes" (never reports).
    await engine.claim(r["id"], await make_worker(pool), timedelta(seconds=1))
    async with running(pool, engine, registry):
        done = await wait_for(finished(pool, job.id), within=20)
    assert done["status"] == RunStatus.SUCCEEDED and done["attempt"] == 2
    assert (await attempts_of(pool, job.id))[0]["status"] == "LOST"


async def test_operator_drain_stops_the_worker(pool, engine, registry, admin):
    from datetime import UTC, datetime

    from taskflow.store import workers as worker_store
    stop = asyncio.Event()
    w = Worker(FAST, engine, pool, None, registry, null_logger())
    task = asyncio.create_task(w.run(stop))
    await asyncio.sleep(0.5)
    async with pool.connection() as conn:
        await worker_store.request_drain(conn, w.id, datetime.now(UTC))
    await asyncio.wait_for(task, 10)  # run() returns without the stop event
    async with pool.connection() as conn:
        row = await (await conn.execute("SELECT status FROM workers WHERE id = %s", (w.id,))).fetchone()
    assert row["status"] == "OFFLINE" and not stop.is_set()


async def test_redis_outage_falls_back_to_postgres(pool, registry, admin):
    from taskflow.queue import Queue
    dead = Queue("redis://127.0.0.1:1/0", prefix="tf-dead")  # nothing listens on port 1
    eng = Engine(pool, dead, null_logger())  # publishing fails; the run stays QUEUED in PostgreSQL
    try:
        async with running(pool, eng, registry, q=dead):
            job = await submit(pool, eng, registry, admin, {"duration_ms": 10})
            r = await wait_for(finished(pool, job.id))
        assert r["status"] == RunStatus.SUCCEEDED
    finally:
        await dead.close()
