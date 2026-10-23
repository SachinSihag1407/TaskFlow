"""Dashboard and health aggregates, computed from stored executions."""

from __future__ import annotations

from datetime import datetime, timedelta
from uuid import UUID

from ..db import Conn

# Percentiles are reported only with at least this many samples.
MIN_LATENCY_SAMPLES = 5


async def job_status_counts(conn: Conn, tenant_id: UUID) -> dict[str, int]:
    rows = await (await conn.execute("SELECT status, count(*) AS n FROM jobs WHERE tenant_id = %s GROUP BY status",
                                     (tenant_id,))).fetchall()
    return {r["status"]: r["n"] for r in rows}


_ACTIVE_COUNTS = """SELECT count(*) FILTER (WHERE status = 'QUEUED') AS queued,
    count(*) FILTER (WHERE status = 'RUNNING') AS running,
    count(*) FILTER (WHERE status = 'RETRY_WAIT') AS retry_wait
    FROM job_runs WHERE status IN ('QUEUED','RUNNING','RETRY_WAIT')"""


async def run_status_counts(conn: Conn, tenant_id: UUID) -> dict[str, int]:
    return await (await conn.execute(_ACTIVE_COUNTS + " AND tenant_id = %s", (tenant_id,))).fetchone()


async def global_queue_counts(conn: Conn) -> dict[str, int]:
    return await (await conn.execute(_ACTIVE_COUNTS)).fetchone()


async def outcomes(conn: Conn, tenant_id: UUID, since: datetime) -> dict:
    runs = await (await conn.execute("""SELECT
        count(*) FILTER (WHERE status = 'SUCCEEDED') AS runs_succeeded,
        count(*) FILTER (WHERE status = 'FAILED') AS runs_failed,
        count(*) FILTER (WHERE status = 'CANCELLED') AS runs_cancelled
        FROM job_runs WHERE tenant_id = %s AND finished_at >= %s""", (tenant_id, since))).fetchone()
    att = await (await conn.execute("""SELECT
        count(*) AS attempts_total,
        count(*) FILTER (WHERE status IN ('FAILED','TIMED_OUT','LOST')) AS attempts_failed,
        count(*) FILTER (WHERE attempt_number > 1) AS retries,
        count(*) FILTER (WHERE status = 'SUCCEEDED' AND duration_ms IS NOT NULL) AS latency_samples,
        avg(duration_ms) FILTER (WHERE status = 'SUCCEEDED') AS latency_avg_ms,
        percentile_cont(0.5) WITHIN GROUP (ORDER BY duration_ms) FILTER (WHERE status = 'SUCCEEDED') AS latency_p50_ms,
        percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms) FILTER (WHERE status = 'SUCCEEDED') AS latency_p95_ms,
        percentile_cont(0.99) WITHIN GROUP (ORDER BY duration_ms) FILTER (WHERE status = 'SUCCEEDED') AS latency_p99_ms,
        avg(EXTRACT(EPOCH FROM (started_at - queued_at)) * 1000) FILTER (WHERE queued_at IS NOT NULL) AS queue_wait_avg_ms,
        percentile_cont(0.95) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (started_at - queued_at)) * 1000)
            FILTER (WHERE queued_at IS NOT NULL) AS queue_wait_p95_ms
        FROM job_attempts WHERE tenant_id = %s AND finished_at >= %s""", (tenant_id, since))).fetchone()
    out = {**runs, **{k: (float(v) if v is not None and k.endswith("_ms") else v) for k, v in att.items()}}
    if out["latency_samples"] < MIN_LATENCY_SAMPLES:
        for k in ("latency_avg_ms", "latency_p50_ms", "latency_p95_ms", "latency_p99_ms"):
            out[k] = None
    return out


async def throughput(conn: Conn, tenant_id: UUID, since: datetime, until: datetime, bucket: timedelta) -> list[dict]:
    """Finished attempts per bucket, with empty buckets filled in."""
    secs = int(bucket.total_seconds())
    origin = datetime.fromtimestamp(since.timestamp() // secs * secs, since.tzinfo)
    rows = await (await conn.execute(
        """SELECT date_bin(%(b)s::interval, finished_at, %(o)s) AS bucket,
            count(*) FILTER (WHERE status = 'SUCCEEDED') AS succeeded,
            count(*) FILTER (WHERE status <> 'SUCCEEDED') AS failed
        FROM job_attempts WHERE tenant_id = %(t)s AND finished_at >= %(o)s AND finished_at < %(u)s
        GROUP BY bucket ORDER BY bucket""",
        {"b": f"{secs} seconds", "o": origin, "t": tenant_id, "u": until})).fetchall()
    found = {int(r["bucket"].timestamp()): r for r in rows}
    out = []
    b = origin
    while b < until:
        r = found.get(int(b.timestamp()))
        out.append({"bucket": b, "succeeded": r["succeeded"] if r else 0, "failed": r["failed"] if r else 0})
        b += bucket
    return out


async def outcomes_by_handler(conn: Conn, tenant_id: UUID, since: datetime) -> list[dict]:
    return await (await conn.execute("""SELECT handler,
        count(*) FILTER (WHERE status = 'SUCCEEDED') AS succeeded, count(*) FILTER (WHERE status <> 'SUCCEEDED') AS failed
        FROM job_attempts WHERE tenant_id = %s AND finished_at >= %s GROUP BY handler ORDER BY count(*) DESC""",
                                      (tenant_id, since))).fetchall()


async def global_error_rates(conn: Conn, since: datetime) -> dict[str, int]:
    return await (await conn.execute("""SELECT count(*) AS attempts,
        count(*) FILTER (WHERE status IN ('FAILED','TIMED_OUT','LOST')) AS failed,
        count(*) FILTER (WHERE attempt_number > 1) AS retries
        FROM job_attempts WHERE finished_at >= %s""", (since,))).fetchone()


async def dead_letter_count(conn: Conn, tenant_id: UUID) -> int:
    row = await (await conn.execute("""SELECT count(*) AS n FROM job_runs
        WHERE tenant_id = %s AND dead_lettered_at IS NOT NULL AND replayed_at IS NULL""", (tenant_id,))).fetchone()
    return row["n"]
