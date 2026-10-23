"""Worker registry, heartbeats and scheduler instances."""

from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime
from uuid import UUID

from ..db import Conn
from ..domain import Conflict, NotFound, Run, SchedulerInstance, Worker, WorkerState
from .runs import COLUMNS as RUN_COLUMNS

COLUMNS = """w.id, w.hostname, w.pid, w.version, w.concurrency, w.handlers, w.status,
    w.active_runs, w.completed_count, w.failed_count, w.heap_alloc_bytes, w.goroutines,
    w.drain_requested_at, w.started_at, w.last_heartbeat_at, w.stopped_at"""


async def register(conn: Conn, w: Worker) -> None:
    await conn.execute(
        """INSERT INTO workers (id, hostname, pid, version, concurrency, handlers, status, started_at, last_heartbeat_at)
        VALUES (%(id)s, %(hostname)s, %(pid)s, %(version)s, %(concurrency)s, %(handlers)s, 'ONLINE',
            %(started_at)s, %(started_at)s)""", w.model_dump())


@dataclass
class Heartbeat:
    worker_id: UUID
    status: WorkerState
    active_runs: int
    memory_bytes: int
    tasks: int
    now: datetime


async def heartbeat(conn: Conn, hb: Heartbeat) -> bool:
    """Records liveness and returns whether an operator requested a drain. A
    worker previously declared OFFLINE (long pause, partition) comes back
    ONLINE on its next beat."""
    row = await (await conn.execute(
        """UPDATE workers SET last_heartbeat_at = %s, status = %s, active_runs = %s,
            heap_alloc_bytes = %s, goroutines = %s, stopped_at = NULL
        WHERE id = %s RETURNING drain_requested_at IS NOT NULL AS drain""",
        (hb.now, str(hb.status), hb.active_runs, hb.memory_bytes, hb.tasks, hb.worker_id))).fetchone()
    if row is None:
        raise NotFound()
    return row["drain"]


async def mark_stopped(conn: Conn, worker_id: UUID, now: datetime) -> None:
    await conn.execute("""UPDATE workers SET status = 'OFFLINE', active_runs = 0, stopped_at = %(now)s,
        last_heartbeat_at = %(now)s WHERE id = %(id)s""", {"now": now, "id": worker_id})


async def increment_counters(conn: Conn, worker_id: UUID, completed: int, failed: int) -> None:
    await conn.execute("UPDATE workers SET completed_count = completed_count + %s, failed_count = failed_count + %s "
                       "WHERE id = %s", (completed, failed, worker_id))


async def request_drain(conn: Conn, worker_id: UUID, now: datetime) -> None:
    cur = await conn.execute("""UPDATE workers SET drain_requested_at = COALESCE(drain_requested_at, %s)
        WHERE id = %s AND status <> 'OFFLINE'""", (now, worker_id))
    if cur.rowcount == 0:
        raise Conflict("worker_offline", "worker is offline or does not exist")


async def mark_silent_offline(conn: Conn, cutoff: datetime) -> list[Worker]:
    rows = await (await conn.execute(f"""UPDATE workers w SET status = 'OFFLINE', active_runs = 0
        WHERE w.status <> 'OFFLINE' AND w.last_heartbeat_at < %s RETURNING {COLUMNS}""", (cutoff,))).fetchall()
    return [Worker(**r) for r in rows]


async def list_workers(conn: Conn, seen_since: datetime) -> list[Worker]:
    rows = await (await conn.execute(f"""SELECT {COLUMNS} FROM workers w
        WHERE w.status <> 'OFFLINE' OR w.last_heartbeat_at >= %s
        ORDER BY (w.status = 'OFFLINE'), w.started_at DESC LIMIT 200""", (seen_since,))).fetchall()
    return [Worker(**r) for r in rows]


async def get(conn: Conn, worker_id: UUID) -> Worker:
    row = await (await conn.execute(f"SELECT {COLUMNS} FROM workers w WHERE w.id = %s", (worker_id,))).fetchone()
    if row is None:
        raise NotFound()
    return Worker(**row)


async def delete_old(conn: Conn, before: datetime) -> int:
    cur = await conn.execute("DELETE FROM workers WHERE status = 'OFFLINE' AND last_heartbeat_at < %s", (before,))
    return cur.rowcount


async def running_runs(conn: Conn, tenant_id: UUID, worker_id: UUID) -> list[Run]:
    rows = await (await conn.execute(f"""SELECT {RUN_COLUMNS}, j.name AS job_name FROM job_runs r
        JOIN jobs j ON j.id = r.job_id
        WHERE r.worker_id = %s AND r.tenant_id = %s AND r.status = 'RUNNING' ORDER BY r.started_at""",
                                     (worker_id, tenant_id))).fetchall()
    return [Run(**r) for r in rows]


async def upsert_scheduler(conn: Conn, s: SchedulerInstance) -> None:
    await conn.execute(
        """INSERT INTO scheduler_instances (id, hostname, is_leader, started_at, last_heartbeat_at, last_tick_at,
            last_tick_ms, last_tick_enqueued)
        VALUES (%(id)s, %(hostname)s, %(is_leader)s, %(started_at)s, %(last_heartbeat_at)s, %(last_tick_at)s,
            %(last_tick_ms)s, %(last_tick_enqueued)s)
        ON CONFLICT (id) DO UPDATE SET is_leader = EXCLUDED.is_leader, last_heartbeat_at = EXCLUDED.last_heartbeat_at,
            last_tick_at = COALESCE(EXCLUDED.last_tick_at, scheduler_instances.last_tick_at),
            last_tick_ms = COALESCE(EXCLUDED.last_tick_ms, scheduler_instances.last_tick_ms),
            last_tick_enqueued = COALESCE(EXCLUDED.last_tick_enqueued, scheduler_instances.last_tick_enqueued),
            stopped_at = NULL""", s.model_dump())


async def stop_scheduler(conn: Conn, scheduler_id: UUID, now: datetime) -> None:
    await conn.execute("UPDATE scheduler_instances SET is_leader = false, stopped_at = %s WHERE id = %s",
                       (now, scheduler_id))


async def list_schedulers(conn: Conn, since: datetime) -> list[SchedulerInstance]:
    rows = await (await conn.execute("""SELECT id, hostname, is_leader, started_at, last_heartbeat_at, last_tick_at,
        last_tick_ms, last_tick_enqueued, stopped_at FROM scheduler_instances
        WHERE last_heartbeat_at >= %s ORDER BY is_leader DESC, last_heartbeat_at DESC""", (since,))).fetchall()
    return [SchedulerInstance(**r) for r in rows]
