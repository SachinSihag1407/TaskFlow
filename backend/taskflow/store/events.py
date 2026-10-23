"""Operational event stream and the audit log."""

from __future__ import annotations

from dataclasses import dataclass, field
from datetime import datetime, timedelta
from uuid import UUID

from psycopg.types.json import Jsonb

from ..db import Conn
from ..domain import AuditEvent, Event
from . import Page


async def insert_event(conn: Conn, e: Event) -> None:
    row = await (await conn.execute(
        """INSERT INTO system_events (tenant_id, job_id, run_id, worker_id, type, severity, message, data, created_at)
        VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s) RETURNING id""",
        (e.tenant_id, e.job_id, e.run_id, e.worker_id, e.type, str(e.severity), e.message,
         Jsonb(e.data) if e.data is not None else None, e.created_at))).fetchone()
    e.id = row["id"]


@dataclass
class EventFilter:
    tenant_id: UUID
    job_id: UUID | None = None
    run_id: UUID | None = None
    worker_id: UUID | None = None
    severities: list[str] = field(default_factory=list)
    include_platform: bool = False  # tenant-less events (worker lifecycle)
    limit: int = 50


async def list_events(conn: Conn, f: EventFilter) -> list[Event]:
    limit = f.limit if 0 < f.limit <= 500 else 50
    where = ["(e.tenant_id = %(t)s OR e.tenant_id IS NULL)" if f.include_platform else "e.tenant_id = %(t)s"]
    p: dict = {"t": f.tenant_id, "limit": limit}
    for col, key, val in (("e.job_id", "job", f.job_id), ("e.run_id", "run", f.run_id), ("e.worker_id", "worker", f.worker_id)):
        if val:
            where.append(f"{col} = %({key})s")
            p[key] = val
    if f.severities:
        where.append("e.severity = ANY(%(sev)s)")
        p["sev"] = f.severities
    rows = await (await conn.execute(
        f"""SELECT e.id, e.tenant_id, e.job_id, e.run_id, e.worker_id, e.type, e.severity, e.message, e.data,
            e.created_at FROM system_events e WHERE {" AND ".join(where)}
        ORDER BY e.created_at DESC, e.id DESC LIMIT %(limit)s""", p)).fetchall()
    return [Event(**r) for r in rows]


async def count_since(conn: Conn, tenant_id: UUID, severities: list[str], since: datetime) -> int:
    row = await (await conn.execute("""SELECT count(*) AS n FROM system_events
        WHERE tenant_id = %s AND severity = ANY(%s) AND created_at > %s""", (tenant_id, severities, since))).fetchone()
    return row["n"]


async def delete_old(conn: Conn, now: datetime) -> int:
    """Per-tenant retention of the operational stream. Audit events are never
    deleted by this job."""
    cur = await conn.execute("""DELETE FROM system_events e USING tenant_settings s
        WHERE e.tenant_id = s.tenant_id AND e.created_at < %s::timestamptz - make_interval(days => s.event_retention_days)""",
                             (now,))
    n = cur.rowcount
    cur = await conn.execute("DELETE FROM system_events WHERE tenant_id IS NULL AND created_at < %s",
                             (now - timedelta(days=30),))
    return n + cur.rowcount


async def insert_audit(conn: Conn, a: AuditEvent | dict) -> None:
    d = a if isinstance(a, dict) else a.model_dump()
    await conn.execute(
        """INSERT INTO audit_events (tenant_id, actor_id, actor_email, action, target_type, target_id, ip,
            user_agent, metadata, created_at)
        VALUES (%(tenant_id)s, %(actor_id)s, %(actor_email)s, %(action)s, %(target_type)s, %(target_id)s, %(ip)s,
            %(user_agent)s, %(metadata)s, %(created_at)s)""",
        {**d, "metadata": Jsonb(d.get("metadata")) if d.get("metadata") is not None else None})


@dataclass
class AuditFilter:
    tenant_id: UUID
    action: str = ""
    target_type: str = ""
    target_id: str = ""
    page: Page = field(default_factory=Page)


async def list_audit(conn: Conn, f: AuditFilter) -> tuple[list[AuditEvent], int]:
    page = f.page.normalize()
    where = ["tenant_id = %(t)s"]
    p: dict = {"t": f.tenant_id}
    if f.action:
        where.append("action LIKE %(action)s")
        p["action"] = f.action + "%"
    if f.target_type:
        where.append("target_type = %(tt)s")
        p["tt"] = f.target_type
    if f.target_id:
        where.append("target_id = %(tid)s")
        p["tid"] = f.target_id
    where_sql = " AND ".join(where)
    total = (await (await conn.execute(f"SELECT count(*) AS n FROM audit_events WHERE {where_sql}", p)).fetchone())["n"]
    p.update(limit=page.page_size, offset=page.offset)
    rows = await (await conn.execute(
        f"""SELECT id, tenant_id, actor_id, actor_email, action, target_type, target_id, ip, user_agent, metadata,
            created_at FROM audit_events WHERE {where_sql} ORDER BY created_at DESC, id DESC
        LIMIT %(limit)s OFFSET %(offset)s""", p)).fetchall()
    return [AuditEvent(**r) for r in rows], total
