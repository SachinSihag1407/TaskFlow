"""Engine integration tests: claim races, concurrency limits, fencing, lease
recovery, scheduling idempotency and operator actions."""

from __future__ import annotations

import asyncio
from datetime import UTC, datetime, timedelta

import pytest

from taskflow.domain import Conflict, JobStatus, RunStatus
from taskflow.engine import Deferred, LeaseLost, Outcome, OutcomeKind, Paused, Stale
from taskflow.logs import null_logger
from taskflow.services.jobs import JobInput, Jobs
from taskflow.store import runs as run_store

from .conftest import make_tenant, make_worker

LEASE = timedelta(seconds=30)


async def create_job(pool, engine, registry, p, **kw):
    spec = {"name": "job", "handler": "test.sleep", "payload": {"duration_ms": 0}, "schedule_type": "once", **kw}
    job, _ = await Jobs(pool, engine, registry, null_logger()).create(p, JobInput(**spec), "")
    return job


async def queued_runs(pool, job_id):
    async with pool.connection() as conn:
        rows = await (await conn.execute(
            "SELECT id FROM job_runs WHERE job_id = %s AND status = 'QUEUED' ORDER BY created_at", (job_id,))).fetchall()
    return [r["id"] for r in rows]


async def run_row(pool, run_id):
    async with pool.connection() as conn:
        return await run_store.get_unscoped(conn, run_id)


async def schedule(engine):
    return await engine.schedule_due(100)


async def test_exactly_one_concurrent_claim_wins(pool, engine, registry, admin):
    job = await create_job(pool, engine, registry, admin)
    await schedule(engine)
    [run_id] = await queued_runs(pool, job.id)
    workers = [await make_worker(pool) for _ in range(10)]
    results = await asyncio.gather(*(engine.claim(run_id, w, LEASE) for w in workers),
                                   return_exceptions=True)
    wins = [r for r in results if not isinstance(r, Exception)]
    assert len(wins) == 1
    assert all(isinstance(r, Stale) for r in results if isinstance(r, Exception))
    r = await run_row(pool, run_id)
    assert r.status == RunStatus.RUNNING and r.attempt == 1


async def test_job_concurrency_defers_a_second_claim(pool, engine, registry, admin):
    job = await create_job(pool, engine, registry, admin, schedule_type="cron", cron_expr="* * * * *",
                           overlap_policy="queue")
    a = await engine.run_now(admin, job.id)
    b = await engine.run_now(admin, job.id)
    await engine.claim(a.id, await make_worker(pool), LEASE)
    with pytest.raises(Deferred):
        await engine.claim(b.id, await make_worker(pool), LEASE)


async def test_workspace_limit_holds_under_concurrent_claims(pool, engine, registry):
    p = await make_tenant(pool, max_concurrent=2)
    run_ids = []
    for i in range(6):
        await create_job(pool, engine, registry, p, name=f"j{i}")
    await schedule(engine)
    async with pool.connection() as conn:
        rows = await (await conn.execute("SELECT id FROM job_runs WHERE tenant_id = %s", (p.tenant_id,))).fetchall()
    run_ids = [r["id"] for r in rows]
    w = await make_worker(pool)
    results = await asyncio.gather(*(engine.claim(rid, w, LEASE) for rid in run_ids),
                                   return_exceptions=True)
    assert sum(not isinstance(r, Exception) for r in results) == 2
    assert all(isinstance(r, Deferred) for r in results if isinstance(r, Exception))


async def test_retry_then_dead_letter_with_fencing(pool, engine, registry, admin, clock):
    job = await create_job(pool, engine, registry, admin, max_attempts=2, backoff_base_seconds=10,
                           backoff_max_seconds=60)
    await schedule(engine)
    clock.t = datetime.now(UTC)  # freeze time so the backoff is exact
    [run_id] = await queued_runs(pool, job.id)
    c1 = await engine.claim(run_id, await make_worker(pool), LEASE)
    await engine.complete(c1, Outcome(OutcomeKind.FAILURE, "boom", retryable=True))
    r = await run_row(pool, run_id)
    assert r.status == RunStatus.RETRY_WAIT
    # rand=0 gives the minimum of the equal-jitter window: half of base.
    assert r.next_attempt_at == clock() + timedelta(seconds=5)

    # A stale report from the first attempt is rejected.
    with pytest.raises(LeaseLost):
        await engine.complete(c1, Outcome(OutcomeKind.SUCCESS))

    assert await engine.promote_retries(100) == 0  # backoff not elapsed
    clock.advance(6)
    assert await engine.promote_retries(100) == 1
    c2 = await engine.claim(run_id, await make_worker(pool), LEASE)
    assert c2.attempt.attempt_number == 2
    await engine.complete(c2, Outcome(OutcomeKind.FAILURE, "boom again", retryable=True))
    r = await run_row(pool, run_id)
    assert r.status == RunStatus.FAILED and r.dead_lettered_at is not None and r.last_error == "boom again"


async def test_permanent_failure_skips_retries(pool, engine, registry, admin):
    job = await create_job(pool, engine, registry, admin, max_attempts=5)
    await schedule(engine)
    [run_id] = await queued_runs(pool, job.id)
    c = await engine.claim(run_id, await make_worker(pool), LEASE)
    await engine.complete(c, Outcome(OutcomeKind.FAILURE, "bad input", retryable=False))
    assert (await run_row(pool, run_id)).status == RunStatus.FAILED


async def test_expired_lease_is_recovered_and_retried(pool, engine, registry, admin, clock):
    job = await create_job(pool, engine, registry, admin, max_attempts=2)
    await schedule(engine)
    [run_id] = await queued_runs(pool, job.id)
    c = await engine.claim(run_id, await make_worker(pool), LEASE)
    assert await engine.recover_expired_leases(100) == 0
    clock.advance(31)
    assert await engine.recover_expired_leases(100) == 1
    r = await run_row(pool, run_id)
    assert r.status == RunStatus.RETRY_WAIT
    async with pool.connection() as conn:
        a = await (await conn.execute("SELECT status, error_kind FROM job_attempts WHERE id = %s",
                                      (c.attempt.id,))).fetchone()
    assert a == {"status": "LOST", "error_kind": "lease_expired"}
    # The presumed-dead worker comes back: its writes are fenced off.
    with pytest.raises(LeaseLost):
        await engine.renew_lease(c, LEASE)
    with pytest.raises(LeaseLost):
        await engine.complete(c, Outcome(OutcomeKind.SUCCESS))


async def test_concurrent_schedulers_do_not_duplicate_occurrences(pool, engine, registry, admin, clock):
    job = await create_job(pool, engine, registry, admin, schedule_type="cron", cron_expr="* * * * *",
                           overlap_policy="allow", max_concurrency=5, missed_run_policy="run_all")
    clock.t = job.next_run_at + timedelta(minutes=3, seconds=1)
    results = await asyncio.gather(*(engine.schedule_due(100) for _ in range(5)))
    assert sum(results) == 4  # occurrences at +0, +1, +2, +3 minutes
    async with pool.connection() as conn:
        n = (await (await conn.execute("SELECT count(*) AS n FROM job_runs WHERE job_id = %s", (job.id,))).fetchone())["n"]
    assert n == 4


async def test_cancel_pause_resume(pool, engine, registry, admin):
    job = await create_job(pool, engine, registry, admin)
    await schedule(engine)
    [run_id] = await queued_runs(pool, job.id)
    j = await engine.pause_job(admin, job.id)
    assert j.status == JobStatus.PAUSED
    with pytest.raises(Paused):
        await engine.claim(run_id, await make_worker(pool), LEASE)
    with pytest.raises(Conflict):
        await engine.pause_job(admin, job.id)
    j = await engine.resume_job(admin, job.id)
    assert j.status == JobStatus.QUEUED
    j = await engine.cancel_job(admin, job.id)
    assert j.status == JobStatus.CANCELLED
    assert (await run_row(pool, run_id)).status == RunStatus.CANCELLED


async def test_cancel_running_run_is_cooperative(pool, engine, registry, admin):
    job = await create_job(pool, engine, registry, admin)
    await schedule(engine)
    [run_id] = await queued_runs(pool, job.id)
    c = await engine.claim(run_id, await make_worker(pool), LEASE)
    await engine.cancel_run(admin, run_id)
    assert await engine.renew_lease(c, LEASE) is True  # the worker learns about it
    await engine.complete(c, Outcome(OutcomeKind.CANCELLED, "cancelled by operator"))
    assert (await run_row(pool, run_id)).status == RunStatus.CANCELLED


async def test_dead_letter_replay_happens_once(pool, engine, registry, admin):
    job = await create_job(pool, engine, registry, admin, max_attempts=1)
    await schedule(engine)
    [run_id] = await queued_runs(pool, job.id)
    c = await engine.claim(run_id, await make_worker(pool), LEASE)
    await engine.complete(c, Outcome(OutcomeKind.FAILURE, "boom", retryable=True))
    results = await asyncio.gather(*(engine.replay_run(admin, run_id) for _ in range(5)), return_exceptions=True)
    assert sum(not isinstance(r, Exception) for r in results) == 1
    assert all(isinstance(r, Conflict) for r in results if isinstance(r, Exception))


async def test_other_workspaces_cannot_act_on_jobs(pool, engine, registry, admin):
    from taskflow.domain import NotFound
    job = await create_job(pool, engine, registry, admin)
    other = await make_tenant(pool)
    with pytest.raises(NotFound):
        await engine.cancel_job(other, job.id)


async def test_redis_publication_and_reconcile(pool, queue, registry, admin, clock):
    from taskflow.engine import Engine
    eng = Engine(pool, queue, null_logger(), now=clock)
    job = await create_job(pool, eng, registry, admin)
    await eng.schedule_due(100)
    [run_id] = await queued_runs(pool, job.id)
    assert await queue.pop(0.5) == run_id
    # The popped entry is lost (worker crashed before claiming): reconcile restores it.
    assert await eng.reconcile(timedelta(seconds=0), 100) == 1
    assert await queue.pop(0.5) == run_id
    assert (await run_row(pool, run_id)).status == RunStatus.QUEUED


async def test_one_time_job_created_in_the_past_runs_now(pool, engine, registry, admin):
    run_at = datetime.now(UTC) - timedelta(seconds=30)  # within the one-minute grace
    job = await create_job(pool, engine, registry, admin, run_at=run_at)
    assert await schedule(engine) == 1
    assert job.next_run_at is not None


async def test_lease_recovery_dead_letters_when_attempts_are_exhausted(pool, engine, registry, admin, clock):
    job = await create_job(pool, engine, registry, admin, max_attempts=1)
    await schedule(engine)
    [run_id] = await queued_runs(pool, job.id)
    await engine.claim(run_id, await make_worker(pool), LEASE)
    clock.advance(31)
    assert await engine.recover_expired_leases(100) == 1
    r = await run_row(pool, run_id)
    assert r.status == RunStatus.FAILED and r.dead_lettered_at is not None


async def test_completion_beats_late_cancellation(pool, engine, registry, admin):
    job = await create_job(pool, engine, registry, admin)
    await schedule(engine)
    [run_id] = await queued_runs(pool, job.id)
    c = await engine.claim(run_id, await make_worker(pool), LEASE)
    await engine.complete(c, Outcome(OutcomeKind.SUCCESS, result={"ok": True}))
    with pytest.raises(Conflict) as e:
        await engine.cancel_run(admin, run_id)
    assert e.value.code == "run_finished"
    assert (await run_row(pool, run_id)).status == RunStatus.SUCCEEDED


async def test_claim_waits_for_briefly_locked_run_instead_of_dropping_it(pool, engine, registry, admin):
    """Regression: a short-lived row lock (e.g. post-publish bookkeeping) must
    not make a claimable run look stale."""
    job = await create_job(pool, engine, registry, admin)
    await schedule(engine)
    [run_id] = await queued_runs(pool, job.id)
    w = await make_worker(pool)
    async with pool.connection() as conn, conn.transaction():
        await conn.execute("SELECT 1 FROM job_runs WHERE id = %s FOR UPDATE", (run_id,))
        claim = asyncio.create_task(engine.claim(run_id, w, LEASE))
        await asyncio.sleep(0.3)
        assert not claim.done()  # waiting, not dropped
    c = await claim
    assert c.run.id == run_id


async def test_retention_deletes_old_events_but_keeps_audit(pool, engine, registry, admin, clock):
    from taskflow.store import events as event_store
    await create_job(pool, engine, registry, admin)  # one event + one audit record
    async with pool.connection() as conn:
        assert await event_store.delete_old(conn, datetime.now(UTC) + timedelta(days=31)) >= 1
        audit = (await (await conn.execute("SELECT count(*) AS n FROM audit_events")).fetchone())["n"]
    assert audit >= 1
