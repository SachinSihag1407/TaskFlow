"""Job definitions."""

from __future__ import annotations

from dataclasses import dataclass, field
from datetime import datetime
from uuid import UUID

from psycopg.types.json import Jsonb

from ..db import Conn
from ..domain import Conflict, Job, JobStatus, NotFound, RunStatus
from . import Page, escape_like

COLUMNS = """j.id, j.tenant_id, j.created_by, j.name, j.description, j.handler, j.payload,
    j.schedule_type, j.run_at, j.cron_expr, j.timezone, j.missed_run_policy, j.overlap_policy,
    j.max_concurrency, j.priority, j.max_attempts, j.backoff_base_seconds, j.backoff_max_seconds,
    j.timeout_seconds, j.status, j.next_run_at, j.last_run_at, j.last_run_status, j.paused_at,
    j.cancelled_at, j.idempotency_key, j.request_hash, j.version, j.created_at, j.updated_at"""


async def insert(conn: Conn, j: Job) -> bool:
    """Inserts a job. Returns False if (tenant, idempotency_key) already exists."""
    cur = await conn.execute(
        """INSERT INTO jobs (id, tenant_id, created_by, name, description, handler, payload, schedule_type,
            run_at, cron_expr, timezone, missed_run_policy, overlap_policy, max_concurrency, priority,
            max_attempts, backoff_base_seconds, backoff_max_seconds, timeout_seconds, status, next_run_at,
            idempotency_key, request_hash, version, created_at, updated_at)
        VALUES (%(id)s, %(tenant_id)s, %(created_by)s, %(name)s, %(description)s, %(handler)s, %(payload)s,
            %(schedule_type)s, %(run_at)s, %(cron_expr)s, %(timezone)s, %(missed_run_policy)s, %(overlap_policy)s,
            %(max_concurrency)s, %(priority)s, %(max_attempts)s, %(backoff_base_seconds)s, %(backoff_max_seconds)s,
            %(timeout_seconds)s, %(status)s, %(next_run_at)s, %(idempotency_key)s, %(request_hash)s, 1,
            %(created_at)s, %(created_at)s)
        ON CONFLICT (tenant_id, idempotency_key) DO NOTHING""",
        {**j.model_dump(exclude={"payload"}), "tenant_id": j.tenant_id, "request_hash": j.request_hash,
         "payload": Jsonb(j.payload)},
    )
    return cur.rowcount == 1


async def _one(conn: Conn, sql: str, params: dict) -> Job:
    row = await (await conn.execute(sql, params)).fetchone()
    if row is None:
        raise NotFound()
    return Job(**row)


async def get(conn: Conn, tenant_id: UUID, job_id: UUID) -> Job:
    return await _one(conn, f"SELECT {COLUMNS} FROM jobs j WHERE j.id = %(id)s AND j.tenant_id = %(t)s",
                      {"id": job_id, "t": tenant_id})


async def get_by_idempotency_key(conn: Conn, tenant_id: UUID, key: str) -> Job:
    return await _one(conn, f"SELECT {COLUMNS} FROM jobs j WHERE j.tenant_id = %(t)s AND j.idempotency_key = %(k)s",
                      {"t": tenant_id, "k": key})


async def lock(conn: Conn, job_id: UUID) -> Job:
    """Locks a job row. Lock order everywhere is job row first, then run rows,
    which prevents deadlocks between scheduler, workers and API actions.
    FOR NO KEY UPDATE does not block the KEY SHARE locks of FK checks."""
    return await _one(conn, f"SELECT {COLUMNS} FROM jobs j WHERE j.id = %(id)s FOR NO KEY UPDATE", {"id": job_id})


async def lock_tenant_job(conn: Conn, tenant_id: UUID, job_id: UUID) -> Job:
    return await _one(conn, f"SELECT {COLUMNS} FROM jobs j WHERE j.id = %(id)s AND j.tenant_id = %(t)s FOR NO KEY UPDATE",
                      {"id": job_id, "t": tenant_id})


async def lock_due(conn: Conn, now: datetime, limit: int) -> list[Job]:
    """Locks up to `limit` active jobs whose next occurrence is due. SKIP LOCKED
    lets several schedulers proceed without blocking on the same rows."""
    rows = await (await conn.execute(
        f"""SELECT {COLUMNS} FROM jobs j
        WHERE j.next_run_at IS NOT NULL AND j.next_run_at <= %(now)s
          AND j.paused_at IS NULL AND j.cancelled_at IS NULL
        ORDER BY j.next_run_at LIMIT %(limit)s
        FOR NO KEY UPDATE SKIP LOCKED""", {"now": now, "limit": limit})).fetchall()
    return [Job(**r) for r in rows]


async def update_definition(conn: Conn, j: Job, expected_version: int, now: datetime) -> None:
    """Persists user-editable fields with optimistic locking."""
    cur = await conn.execute(
        """UPDATE jobs SET name=%(name)s, description=%(description)s, handler=%(handler)s, payload=%(payload)s,
            schedule_type=%(schedule_type)s, run_at=%(run_at)s, cron_expr=%(cron_expr)s, timezone=%(timezone)s,
            missed_run_policy=%(missed_run_policy)s, overlap_policy=%(overlap_policy)s,
            max_concurrency=%(max_concurrency)s, priority=%(priority)s, max_attempts=%(max_attempts)s,
            backoff_base_seconds=%(backoff_base_seconds)s, backoff_max_seconds=%(backoff_max_seconds)s,
            timeout_seconds=%(timeout_seconds)s, next_run_at=%(next_run_at)s, version = version + 1,
            updated_at=%(now)s
        WHERE id=%(id)s AND version=%(expected)s""",
        {**j.model_dump(exclude={"payload"}), "payload": Jsonb(j.payload), "now": now, "expected": expected_version},
    )
    if cur.rowcount == 0:
        raise Conflict("version_conflict", "the job was modified by someone else; reload and try again")


async def set_next_run(conn: Conn, job_id: UUID, nxt: datetime | None, now: datetime) -> None:
    await conn.execute("UPDATE jobs SET next_run_at=%s, updated_at=%s WHERE id=%s", (nxt, now, job_id))


async def set_paused(conn: Conn, job_id: UUID, paused_at: datetime | None, now: datetime) -> None:
    await conn.execute("UPDATE jobs SET paused_at=%s, version=version+1, updated_at=%s WHERE id=%s",
                       (paused_at, now, job_id))


async def set_cancelled(conn: Conn, job_id: UUID, now: datetime) -> None:
    await conn.execute("UPDATE jobs SET cancelled_at=%(now)s, next_run_at=NULL, version=version+1, updated_at=%(now)s "
                       "WHERE id=%(id)s", {"now": now, "id": job_id})


async def set_last_run(conn: Conn, job_id: UUID, at: datetime, status: RunStatus) -> None:
    await conn.execute("UPDATE jobs SET last_run_at=%s, last_run_status=%s WHERE id=%s", (at, str(status), job_id))


async def set_status(conn: Conn, job_id: UUID, status: JobStatus, now: datetime) -> None:
    """Writes the derived aggregate status. Only engine.sync_job_status calls this."""
    await conn.execute("UPDATE jobs SET status=%(s)s, updated_at=%(now)s WHERE id=%(id)s AND status <> %(s)s",
                       {"s": str(status), "now": now, "id": job_id})


@dataclass
class JobFilter:
    tenant_id: UUID
    query: str = ""
    statuses: list[str] = field(default_factory=list)
    handlers: list[str] = field(default_factory=list)
    schedule_type: str = ""
    priority: int = 0
    sort: str = "created_at"
    desc: bool = True
    page: Page = field(default_factory=Page)


SORT_COLUMNS = {
    "name": "j.name", "created_at": "j.created_at", "updated_at": "j.updated_at", "next_run_at": "j.next_run_at",
    "last_run_at": "j.last_run_at", "priority": "j.priority", "status": "j.status",
}


async def list_jobs(conn: Conn, f: JobFilter) -> tuple[list[dict], int]:
    """A filtered, sorted page of jobs (with run counts) and the total count."""
    page = f.page.normalize()
    where = ["j.tenant_id = %(t)s"]
    p: dict = {"t": f.tenant_id}
    if f.query:
        where.append("(j.name ILIKE %(q)s ESCAPE '\\' OR j.description ILIKE %(q)s ESCAPE '\\' OR j.id::text = %(qid)s)")
        p["q"], p["qid"] = f"%{escape_like(f.query)}%", f.query.lower()
    if f.statuses:
        where.append("j.status = ANY(%(statuses)s)")
        p["statuses"] = f.statuses
    if f.handlers:
        where.append("j.handler = ANY(%(handlers)s)")
        p["handlers"] = f.handlers
    if f.schedule_type:
        where.append("j.schedule_type = %(st)s")
        p["st"] = f.schedule_type
    if f.priority:
        where.append("j.priority = %(prio)s")
        p["prio"] = f.priority
    where_sql = " AND ".join(where)
    total = (await (await conn.execute(f"SELECT count(*) AS n FROM jobs j WHERE {where_sql}", p)).fetchone())["n"]
    col = SORT_COLUMNS.get(f.sort)
    desc = f.desc
    if col is None:
        col, desc = "j.created_at", True
    p.update(limit=page.page_size, offset=page.offset)
    rows = await (await conn.execute(
        f"""SELECT {COLUMNS},
            COALESCE(s.succeeded, 0) AS succeeded_runs, COALESCE(s.failed, 0) AS failed_runs,
            COALESCE(s.active, 0) AS active_runs
        FROM jobs j
        LEFT JOIN LATERAL (
            SELECT count(*) FILTER (WHERE r.status = 'SUCCEEDED') AS succeeded,
                   count(*) FILTER (WHERE r.status = 'FAILED') AS failed,
                   count(*) FILTER (WHERE r.status IN ('QUEUED','RUNNING','RETRY_WAIT')) AS active
            FROM job_runs r WHERE r.job_id = j.id) s ON true
        WHERE {where_sql}
        ORDER BY {col} {"DESC" if desc else "ASC"} NULLS LAST, j.id
        LIMIT %(limit)s OFFSET %(offset)s""", p)).fetchall()
    items = []
    for r in rows:
        counts = {k: r.pop(k) for k in ("succeeded_runs", "failed_runs", "active_runs")}
        items.append({**Job(**r).model_dump(mode="json"), **counts})
    return items, total


async def list_schedules(conn: Conn, tenant_id: UUID) -> list[Job]:
    rows = await (await conn.execute(
        f"""SELECT {COLUMNS} FROM jobs j WHERE j.tenant_id = %(t)s AND j.schedule_type = 'cron'
        ORDER BY (j.cancelled_at IS NOT NULL), j.next_run_at NULLS LAST, j.name LIMIT 500""", {"t": tenant_id})).fetchall()
    return [Job(**r) for r in rows]
