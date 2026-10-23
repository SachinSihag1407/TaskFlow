"""Execution attempts and their logs."""

from __future__ import annotations

from dataclasses import dataclass, field
from datetime import datetime
from uuid import UUID

from ..db import Conn
from ..domain import Attempt, AttemptStatus, LogLine, NotFound
from . import Page, escape_like

COLUMNS = """a.id, a.run_id, a.job_id, a.tenant_id, a.attempt_number, a.worker_id, a.handler,
    a.status, a.queued_at, a.started_at, a.finished_at, a.duration_ms, a.error_kind,
    a.error_message, a.retryable, a.next_retry_at"""
JOINED = COLUMNS + ", j.name AS job_name, w.hostname AS worker_hostname, r.max_attempts"
JOIN = """ FROM job_attempts a JOIN jobs j ON j.id = a.job_id JOIN job_runs r ON r.id = a.run_id
    LEFT JOIN workers w ON w.id = a.worker_id"""

# Bounds log storage per attempt.
MAX_LOG_LINES = 1000


async def insert(conn: Conn, a: Attempt) -> None:
    await conn.execute(
        """INSERT INTO job_attempts (id, run_id, job_id, tenant_id, attempt_number, worker_id, handler, status,
            queued_at, started_at)
        VALUES (%(id)s, %(run_id)s, %(job_id)s, %(tenant_id)s, %(attempt_number)s, %(worker_id)s, %(handler)s,
            'RUNNING', %(queued_at)s, %(started_at)s)""",
        {**a.model_dump(), "tenant_id": a.tenant_id})


@dataclass
class Outcome:
    status: AttemptStatus
    finished_at: datetime
    error_kind: str | None = None
    error_message: str | None = None
    retryable: bool | None = None
    next_retry_at: datetime | None = None


async def finish(conn: Conn, attempt_id: UUID, o: Outcome) -> None:
    """Closes a RUNNING attempt. A no-op if the reaper already marked it LOST."""
    await conn.execute(
        """UPDATE job_attempts SET status = %(s)s, finished_at = %(f)s,
            duration_ms = GREATEST(0, (EXTRACT(EPOCH FROM (%(f)s - started_at)) * 1000)::bigint),
            error_kind = %(kind)s, error_message = %(msg)s, retryable = %(retry)s, next_retry_at = %(next)s
        WHERE id = %(id)s AND status = 'RUNNING'""",
        {"s": str(o.status), "f": o.finished_at, "kind": o.error_kind, "msg": o.error_message,
         "retry": o.retryable, "next": o.next_retry_at, "id": attempt_id})


async def get(conn: Conn, tenant_id: UUID, attempt_id: UUID) -> Attempt:
    row = await (await conn.execute(f"SELECT {JOINED}{JOIN} WHERE a.id = %s AND a.tenant_id = %s",
                                    (attempt_id, tenant_id))).fetchone()
    if row is None:
        raise NotFound()
    return Attempt(**row)


@dataclass
class AttemptFilter:
    tenant_id: UUID
    job_id: UUID | None = None
    run_id: UUID | None = None
    worker_id: UUID | None = None
    statuses: list[str] = field(default_factory=list)
    handlers: list[str] = field(default_factory=list)
    query: str = ""
    since: datetime | None = None
    until: datetime | None = None
    sort: str = ""  # started_at | duration_ms
    desc: bool = True
    page: Page = field(default_factory=Page)


async def list_attempts(conn: Conn, f: AttemptFilter) -> tuple[list[Attempt], int]:
    page = f.page.normalize()
    where = ["a.tenant_id = %(t)s"]
    p: dict = {"t": f.tenant_id}
    for col, key, val in (("a.job_id", "job", f.job_id), ("a.run_id", "run", f.run_id), ("a.worker_id", "worker", f.worker_id)):
        if val:
            where.append(f"{col} = %({key})s")
            p[key] = val
    if f.statuses:
        where.append("a.status = ANY(%(st)s)")
        p["st"] = f.statuses
    if f.handlers:
        where.append("a.handler = ANY(%(h)s)")
        p["h"] = f.handlers
    if f.since:
        where.append("a.started_at >= %(since)s")
        p["since"] = f.since
    if f.until:
        where.append("a.started_at < %(until)s")
        p["until"] = f.until
    if f.query:
        where.append("(j.name ILIKE %(q)s ESCAPE '\\' OR a.error_message ILIKE %(q)s ESCAPE '\\')")
        p["q"] = f"%{escape_like(f.query)}%"
    where_sql = " AND ".join(where)
    total = (await (await conn.execute(f"SELECT count(*) AS n{JOIN} WHERE {where_sql}", p)).fetchone())["n"]
    order = "a.duration_ms" if f.sort == "duration_ms" else "a.started_at"
    direction = "ASC" if (f.sort and not f.desc) else "DESC"
    p.update(limit=page.page_size, offset=page.offset)
    rows = await (await conn.execute(
        f"SELECT {JOINED}{JOIN} WHERE {where_sql} ORDER BY {order} {direction} NULLS LAST, a.id "
        "LIMIT %(limit)s OFFSET %(offset)s", p)).fetchall()
    return [Attempt(**r) for r in rows], total


async def for_job(conn: Conn, tenant_id: UUID, job_id: UUID, limit: int) -> list[Attempt]:
    rows = await (await conn.execute(
        f"SELECT {JOINED}{JOIN} WHERE a.tenant_id = %s AND a.job_id = %s ORDER BY a.started_at DESC LIMIT %s",
        (tenant_id, job_id, limit))).fetchall()
    return [Attempt(**r) for r in rows]


async def insert_logs(conn: Conn, attempt_id: UUID, lines: list[LogLine]) -> None:
    """Appends log lines with one batched insert."""
    if not lines:
        return
    await conn.execute(
        """INSERT INTO attempt_logs (attempt_id, ts, level, message)
        SELECT %s, t, l, m FROM unnest(%s::timestamptz[], %s::text[], %s::text[]) AS x(t, l, m)""",
        (attempt_id, [x.ts for x in lines], [x.level for x in lines], [x.message for x in lines]))


async def list_logs(conn: Conn, tenant_id: UUID, attempt_id: UUID, after_id: int = 0,
                    limit: int = MAX_LOG_LINES) -> list[LogLine]:
    limit = limit if 0 < limit <= MAX_LOG_LINES else MAX_LOG_LINES
    rows = await (await conn.execute(
        """SELECT l.id, l.ts, l.level, l.message FROM attempt_logs l JOIN job_attempts a ON a.id = l.attempt_id
        WHERE l.attempt_id = %s AND a.tenant_id = %s AND l.id > %s ORDER BY l.id LIMIT %s""",
        (attempt_id, tenant_id, after_id, limit))).fetchall()
    return [LogLine(**r) for r in rows]
