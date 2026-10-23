"""Runs: one row per occurrence of a job, moving through the state machine."""

from __future__ import annotations

import uuid
from dataclasses import dataclass, field
from datetime import datetime
from typing import Any
from uuid import UUID

from psycopg.types.json import Jsonb

from .. import statemachine as sm
from ..db import Conn
from ..domain import Conflict, Job, NotFound, Run, RunStatus, Trigger
from . import Page, escape_like

COLUMNS = """r.id, r.job_id, r.tenant_id, r.scheduled_for, r.trigger, r.status, r.priority,
    r.handler, r.payload, r.timeout_seconds, r.max_attempts, r.backoff_base_seconds,
    r.backoff_max_seconds, r.attempt, r.current_attempt_id, r.worker_id, r.lease_expires_at,
    r.queued_at, r.started_at, r.finished_at, r.next_attempt_at, r.cancel_requested_at,
    r.last_error, r.result, r.checkpoint, r.dead_lettered_at, r.replayed_at, r.replay_of,
    r.created_at, r.updated_at"""


def from_job(j: Job, scheduled_for: datetime, trigger: Trigger, now: datetime) -> Run:
    """Snapshots a job's execution configuration into a new QUEUED run."""
    return Run(
        id=uuid.uuid4(), job_id=j.id, tenant_id=j.tenant_id, scheduled_for=scheduled_for, trigger=trigger,
        status=RunStatus.QUEUED, priority=j.priority, handler=j.handler, payload=j.payload,
        timeout_seconds=j.timeout_seconds, max_attempts=j.max_attempts,
        backoff_base_seconds=j.backoff_base_seconds, backoff_max_seconds=j.backoff_max_seconds,
        queued_at=now, created_at=now, updated_at=now,
    )


async def insert(conn: Conn, r: Run) -> bool:
    """Inserts a run. For scheduled occurrences the partial unique index on
    (job_id, scheduled_for) makes this idempotent: a duplicate occurrence
    returns False instead of creating a second run."""
    cur = await conn.execute(
        """INSERT INTO job_runs (id, job_id, tenant_id, scheduled_for, trigger, status, priority, handler,
            payload, timeout_seconds, max_attempts, backoff_base_seconds, backoff_max_seconds, attempt,
            queued_at, replay_of, created_at, updated_at)
        VALUES (%(id)s, %(job_id)s, %(tenant_id)s, %(scheduled_for)s, %(trigger)s, %(status)s, %(priority)s,
            %(handler)s, %(payload)s, %(timeout_seconds)s, %(max_attempts)s, %(backoff_base_seconds)s,
            %(backoff_max_seconds)s, 0, %(queued_at)s, %(replay_of)s, %(created_at)s, %(created_at)s)
        ON CONFLICT (job_id, scheduled_for) WHERE trigger = 'schedule' DO NOTHING""",
        {**r.model_dump(exclude={"payload", "result", "checkpoint"}), "tenant_id": r.tenant_id,
         "payload": Jsonb(r.payload)},
    )
    return cur.rowcount == 1


async def _one(conn: Conn, sql: str, params: Any) -> Run:
    row = await (await conn.execute(sql, params)).fetchone()
    if row is None:
        raise NotFound()
    return Run(**row)


async def get(conn: Conn, tenant_id: UUID, run_id: UUID) -> Run:
    return await _one(conn, f"""SELECT {COLUMNS}, j.name AS job_name FROM job_runs r JOIN jobs j ON j.id = r.job_id
        WHERE r.id = %s AND r.tenant_id = %s""", (run_id, tenant_id))


async def get_unscoped(conn: Conn, run_id: UUID) -> Run:
    """For internal components (worker, scheduler)."""
    return await _one(conn, f"SELECT {COLUMNS} FROM job_runs r WHERE r.id = %s", (run_id,))


async def lock(conn: Conn, run_id: UUID) -> Run:
    """Locks a run row. Callers must already hold the parent job's lock."""
    return await _one(conn, f"SELECT {COLUMNS} FROM job_runs r WHERE r.id = %s FOR UPDATE", (run_id,))


async def lock_if_status(conn: Conn, run_id: UUID, status: RunStatus) -> Run:
    """Locks a run only if it has the given status, skipping it if busy."""
    return await _one(conn, f"SELECT {COLUMNS} FROM job_runs r WHERE r.id = %s AND r.status = %s FOR UPDATE SKIP LOCKED",
                      (run_id, str(status)))


async def lock_active(conn: Conn, job_id: UUID) -> list[Run]:
    rows = await (await conn.execute(
        f"""SELECT {COLUMNS} FROM job_runs r WHERE r.job_id = %s AND r.status IN ('QUEUED','RUNNING','RETRY_WAIT')
        ORDER BY r.created_at FOR UPDATE""", (job_id,))).fetchall()
    return [Run(**r) for r in rows]


@dataclass
class Transition:
    to: RunStatus
    now: datetime
    attempt_id: UUID | None = None  # fencing: required current_attempt_id
    next_attempt_at: datetime | None = None
    last_error: str | None = None
    result: Any = None
    dead_letter: bool = False


async def transition(conn: Conn, run_id: UUID, src: RunStatus, t: Transition) -> None:
    """Applies a validated status change conditionally on the run's current
    status (and, for RUNNING runs, its current attempt). Raises NotFound if the
    condition no longer holds — the caller lost a race and must not overwrite."""
    sm.validate(src, t.to)
    sets = ["status = %(to)s", "updated_at = %(now)s"]
    p: dict = {"id": run_id, "src": str(src), "to": str(t.to), "now": t.now}
    if t.to == RunStatus.QUEUED:
        sets += ["queued_at = %(now)s", "next_attempt_at = NULL", "published_at = NULL"]
    elif t.to == RunStatus.RETRY_WAIT:
        sets += ["next_attempt_at = %(next)s", "worker_id = NULL", "lease_expires_at = NULL", "current_attempt_id = NULL"]
        p["next"] = t.next_attempt_at
    elif t.to.terminal:
        sets += ["finished_at = %(now)s", "lease_expires_at = NULL", "next_attempt_at = NULL"]
    if t.last_error is not None:
        sets.append("last_error = %(err)s")
        p["err"] = t.last_error
    if t.result is not None:
        sets.append("result = %(result)s")
        p["result"] = Jsonb(t.result)
    if t.dead_letter:
        sets.append("dead_lettered_at = %(now)s")
    where = "id = %(id)s AND status = %(src)s"
    if t.attempt_id is not None:
        where += " AND current_attempt_id = %(attempt)s"
        p["attempt"] = t.attempt_id
    cur = await conn.execute(f"UPDATE job_runs SET {', '.join(sets)} WHERE {where}", p)
    if cur.rowcount == 0:
        raise NotFound()


async def mark_claimed(conn: Conn, run_id: UUID, attempt_id: UUID, worker_id: UUID, attempt_no: int,
                       lease_until: datetime, now: datetime) -> None:
    sm.validate(RunStatus.QUEUED, RunStatus.RUNNING)
    cur = await conn.execute(
        """UPDATE job_runs SET status = 'RUNNING', attempt = %(n)s, current_attempt_id = %(a)s, worker_id = %(w)s,
            lease_expires_at = %(lease)s, started_at = COALESCE(started_at, %(now)s), updated_at = %(now)s
        WHERE id = %(id)s AND status = 'QUEUED'""",
        {"n": attempt_no, "a": attempt_id, "w": worker_id, "lease": lease_until, "now": now, "id": run_id})
    if cur.rowcount == 0:
        raise NotFound()


async def renew_lease(conn: Conn, run_id: UUID, attempt_id: UUID, until: datetime) -> bool:
    """Extends a running attempt's lease and reports whether cancellation was
    requested. Raises NotFound if the attempt no longer owns the run."""
    row = await (await conn.execute(
        """UPDATE job_runs SET lease_expires_at = %s
        WHERE id = %s AND current_attempt_id = %s AND status = 'RUNNING'
        RETURNING cancel_requested_at IS NOT NULL AS cancel""", (until, run_id, attempt_id))).fetchone()
    if row is None:
        raise NotFound()
    return row["cancel"]


async def set_cancel_requested(conn: Conn, run_id: UUID, now: datetime) -> None:
    await conn.execute("""UPDATE job_runs SET cancel_requested_at = COALESCE(cancel_requested_at, %(now)s),
        updated_at = %(now)s WHERE id = %(id)s AND status = 'RUNNING'""", {"now": now, "id": run_id})


async def save_checkpoint(conn: Conn, run_id: UUID, attempt_id: UUID, cp: Any) -> None:
    cur = await conn.execute(
        "UPDATE job_runs SET checkpoint = %s WHERE id = %s AND current_attempt_id = %s AND status = 'RUNNING'",
        (Jsonb(cp), run_id, attempt_id))
    if cur.rowcount == 0:
        raise NotFound()


async def mark_replayed(conn: Conn, run_id: UUID, now: datetime) -> None:
    cur = await conn.execute("""UPDATE job_runs SET replayed_at = %(now)s, updated_at = %(now)s
        WHERE id = %(id)s AND status = 'FAILED' AND replayed_at IS NULL""", {"now": now, "id": run_id})
    if cur.rowcount == 0:
        raise Conflict("not_replayable", "run is not a failed run awaiting replay")


async def mark_published(conn: Conn, ids: list[UUID], now: datetime) -> None:
    """Best-effort bookkeeping: locks rows in a fixed order and skips contended
    ones, so it never deadlocks with transactions holding job locks."""
    if ids:
        await conn.execute("""UPDATE job_runs SET published_at = %s WHERE id IN (
            SELECT id FROM job_runs WHERE id = ANY(%s) AND status = 'QUEUED' ORDER BY id FOR UPDATE SKIP LOCKED)""",
                           (now, ids))


async def active_summary(conn: Conn, job_id: UUID) -> sm.RunSummary:
    """In-flight run counts; reads only the partial index of active runs."""
    row = await (await conn.execute(
        """SELECT count(*) FILTER (WHERE status = 'RUNNING') AS running,
                  count(*) FILTER (WHERE status = 'QUEUED') AS queued,
                  count(*) FILTER (WHERE status = 'RETRY_WAIT') AS retry_wait
        FROM job_runs WHERE job_id = %s AND status IN ('QUEUED','RUNNING','RETRY_WAIT')""", (job_id,))).fetchone()
    return sm.RunSummary(running=row["running"], queued=row["queued"], retry_wait=row["retry_wait"])


async def count_running(conn: Conn, job_id: UUID) -> int:
    row = await (await conn.execute("SELECT count(*) AS n FROM job_runs WHERE job_id = %s AND status = 'RUNNING'",
                                    (job_id,))).fetchone()
    return row["n"]


async def count_tenant_running(conn: Conn, tenant_id: UUID) -> int:
    row = await (await conn.execute("SELECT count(*) AS n FROM job_runs WHERE tenant_id = %s AND status = 'RUNNING'",
                                    (tenant_id,))).fetchone()
    return row["n"]


@dataclass
class QueueRef:
    run_id: UUID
    priority: int
    queued_at: datetime


async def due_retry_candidates(conn: Conn, now: datetime, limit: int) -> list[tuple[UUID, UUID]]:
    """(run_id, job_id) of RETRY_WAIT runs whose backoff elapsed. Not locked;
    the engine re-checks each one under the job-then-run lock order."""
    rows = await (await conn.execute("""SELECT id, job_id FROM job_runs
        WHERE status = 'RETRY_WAIT' AND next_attempt_at <= %s ORDER BY next_attempt_at LIMIT %s""",
                                     (now, limit))).fetchall()
    return [(r["id"], r["job_id"]) for r in rows]


async def expired_lease_candidates(conn: Conn, now: datetime, limit: int) -> list[Run]:
    rows = await (await conn.execute(f"""SELECT {COLUMNS} FROM job_runs r
        WHERE r.status = 'RUNNING' AND r.lease_expires_at < %s ORDER BY r.lease_expires_at LIMIT %s""",
                                     (now, limit))).fetchall()
    return [Run(**r) for r in rows]


async def unpublished_queued(conn: Conn, older_than: datetime, limit: int) -> list[QueueRef]:
    """QUEUED runs of active jobs that may be missing from Redis."""
    rows = await (await conn.execute("""SELECT r.id, r.priority, r.queued_at FROM job_runs r JOIN jobs j ON j.id = r.job_id
        WHERE r.status = 'QUEUED' AND j.paused_at IS NULL AND (r.published_at IS NULL OR r.queued_at < %s)
        ORDER BY r.priority DESC, r.queued_at LIMIT %s""", (older_than, limit))).fetchall()
    return [QueueRef(r["id"], r["priority"], r["queued_at"]) for r in rows]


async def queued_refs_for_job(conn: Conn, job_id: UUID, limit: int) -> list[QueueRef]:
    rows = await (await conn.execute("""SELECT id, priority, queued_at FROM job_runs
        WHERE job_id = %s AND status = 'QUEUED' ORDER BY queued_at LIMIT %s""", (job_id, limit))).fetchall()
    return [QueueRef(r["id"], r["priority"], r["queued_at"]) for r in rows]


async def claim_candidates(conn: Conn, handlers: list[str], limit: int) -> list[UUID]:
    """QUEUED run ids in dispatch order for the Redis-free path. Not locked;
    claim re-checks under lock."""
    rows = await (await conn.execute("""SELECT r.id FROM job_runs r JOIN jobs j ON j.id = r.job_id
        WHERE r.status = 'QUEUED' AND j.paused_at IS NULL AND r.handler = ANY(%s)
        ORDER BY r.priority DESC, r.queued_at LIMIT %s""", (handlers, limit))).fetchall()
    return [r["id"] for r in rows]


@dataclass
class RunFilter:
    tenant_id: UUID
    job_id: UUID | None = None
    statuses: list[str] = field(default_factory=list)
    dead_lettered: bool = False
    query: str = ""
    page: Page = field(default_factory=Page)


async def list_runs(conn: Conn, f: RunFilter) -> tuple[list[Run], int]:
    page = f.page.normalize()
    where = ["r.tenant_id = %(t)s"]
    p: dict = {"t": f.tenant_id}
    if f.job_id:
        where.append("r.job_id = %(job)s")
        p["job"] = f.job_id
    if f.statuses:
        where.append("r.status = ANY(%(st)s)")
        p["st"] = f.statuses
    order = "r.created_at DESC"
    if f.dead_lettered:
        where.append("r.dead_lettered_at IS NOT NULL")
        order = "r.dead_lettered_at DESC"
    if f.query:
        where.append("(j.name ILIKE %(q)s ESCAPE '\\' OR r.last_error ILIKE %(q)s ESCAPE '\\')")
        p["q"] = f"%{escape_like(f.query)}%"
    where_sql = " AND ".join(where)
    total = (await (await conn.execute(
        f"SELECT count(*) AS n FROM job_runs r JOIN jobs j ON j.id = r.job_id WHERE {where_sql}", p)).fetchone())["n"]
    p.update(limit=page.page_size, offset=page.offset)
    rows = await (await conn.execute(
        f"""SELECT {COLUMNS}, j.name AS job_name FROM job_runs r JOIN jobs j ON j.id = r.job_id
        WHERE {where_sql} ORDER BY {order}, r.id LIMIT %(limit)s OFFSET %(offset)s""", p)).fetchall()
    return [Run(**r) for r in rows], total


async def last_failed(conn: Conn, job_id: UUID) -> Run:
    return await _one(conn, f"""SELECT {COLUMNS} FROM job_runs r WHERE r.job_id = %s AND r.status = 'FAILED'
        ORDER BY r.finished_at DESC NULLS LAST LIMIT 1""", (job_id,))


async def recent_for_jobs(conn: Conn, job_ids: list[UUID], per_job: int) -> dict[UUID, list[Run]]:
    out: dict[UUID, list[Run]] = {}
    if not job_ids:
        return out
    rows = await (await conn.execute(
        f"""SELECT {COLUMNS} FROM unnest(%s::uuid[]) AS ids(job_id)
        CROSS JOIN LATERAL (SELECT * FROM job_runs x WHERE x.job_id = ids.job_id ORDER BY x.created_at DESC LIMIT %s) r""",
        (job_ids, per_job))).fetchall()
    for r in rows:
        run = Run(**r)
        out.setdefault(run.job_id, []).append(run)
    return out
