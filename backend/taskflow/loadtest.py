"""Submits jobs through the public API and measures what actually happened,
using the execution records TaskFlow stores:

  - submission throughput (API job creations per second);
  - end-to-end throughput (completed runs per second, first submit to last finish);
  - queue wait (attempt start minus run queued time) percentiles;
  - execution latency (attempt duration) percentiles;
  - failure and retry counts;
  - worker utilisation (busy worker-seconds / available worker-seconds).

No numbers are estimated: everything is computed from API responses.
"""

from __future__ import annotations

import argparse
import asyncio
import json
import math
import os
import sys
import time
from datetime import UTC, datetime, timedelta

import httpx


def _ts(s: str | None) -> datetime | None:
    return datetime.fromisoformat(s.replace("Z", "+00:00")) if s else None


def percentile(sorted_values: list[float], p: float) -> float:
    if not sorted_values:
        return math.nan
    return sorted_values[max(0, math.ceil(p * len(sorted_values)) - 1)]


class Client:
    def __init__(self, base: str):
        self.http = httpx.AsyncClient(base_url=base.rstrip("/"), timeout=30, trust_env=False)
        self.csrf = ""

    async def call(self, method: str, path: str, body=None, idem: str = "", params=None) -> dict:
        headers = {"Content-Type": "application/json"}
        if self.csrf:
            headers["X-CSRF-Token"] = self.csrf
        if idem:
            headers["Idempotency-Key"] = idem
        r = await self.http.request(method, path, json=body, headers=headers, params=params)
        if r.status_code >= 300:
            raise RuntimeError(f"{method} {path}: HTTP {r.status_code}: {r.text.strip()[:500]}")
        return r.json() if r.content else {}

    async def login(self, email: str, password: str) -> None:
        res = await self.call("POST", "/api/v1/auth/login", {"email": email, "password": password})
        self.csrf = res["data"]["csrf_token"]

    async def capacity(self) -> int:
        res = await self.call("GET", "/api/v1/workers")
        return sum(w["concurrency"] for w in res["data"] if w["status"] == "ONLINE")

    async def count_jobs(self, tag: str, statuses: str) -> int:
        res = await self.call("GET", "/api/v1/jobs", params={"q": tag, "status": statuses, "page_size": 1})
        return res["meta"]["total"]

    async def attempts(self, tag: str, since: datetime) -> list[dict]:
        out, page = [], 1
        while True:
            res = await self.call("GET", "/api/v1/executions", params={
                "q": tag, "since": since.strftime("%Y-%m-%dT%H:%M:%SZ"), "page_size": 200, "page": page})
            out += res["data"]
            if page >= res["meta"]["total_pages"]:
                return out
            page += 1


async def run(args: argparse.Namespace) -> dict:
    c = Client(args.url)
    await c.login(args.email, args.password)
    capacity = await c.capacity()
    if capacity == 0:
        raise RuntimeError("no online workers; start workers before running a load test")
    tag = f"loadtest-{int(time.time())}"
    payload = {"duration_ms": args.duration_ms, "fail_probability": args.fail_rate}
    print(f"submitting {args.jobs} jobs ({tag}) with {args.concurrency} submitters; worker capacity {capacity}",
          file=sys.stderr)

    queue: asyncio.Queue[int] = asyncio.Queue()
    for i in range(args.jobs):
        queue.put_nowait(i)
    submitted = errors = 0

    async def submitter() -> None:
        nonlocal submitted, errors
        while True:
            try:
                n = queue.get_nowait()
            except asyncio.QueueEmpty:
                return
            body = {"name": f"{tag}-{n:05d}", "handler": "test.sleep", "payload": payload, "schedule_type": "once",
                    "max_attempts": args.max_attempts, "backoff_base_seconds": 1, "backoff_max_seconds": 5}
            try:
                await c.call("POST", "/api/v1/jobs", body, idem=f"{tag}-{n}")
                submitted += 1
            except Exception as e:  # noqa: BLE001 - counted and reported
                errors += 1
                print(f"submit error: {e}", file=sys.stderr)

    start_wall = datetime.now(UTC)
    start = time.monotonic()
    await asyncio.gather(*(submitter() for _ in range(args.concurrency)))
    submit_secs = time.monotonic() - start
    print(f"submitted {submitted} jobs in {submit_secs:.2f}s ({errors} errors); waiting for completion…",
          file=sys.stderr)

    deadline = time.monotonic() + args.timeout
    while (pending := await c.count_jobs(tag, "SCHEDULED,QUEUED,RUNNING,RETRY_WAIT,CREATED")) > 0:
        if time.monotonic() > deadline:
            raise RuntimeError(f"timed out with {pending} jobs still pending")
        print(f"  {pending} pending", file=sys.stderr)
        await asyncio.sleep(2)

    attempts = await c.attempts(tag, start_wall - timedelta(minutes=1))
    failed_jobs = await c.count_jobs(tag, "FAILED")
    await c.http.aclose()

    waits, execs, busy, last, retries = [], [], 0.0, start_wall, 0
    for a in attempts:
        retries += a["attempt_number"] > 1
        started, queued, finished = _ts(a["started_at"]), _ts(a.get("queued_at")), _ts(a.get("finished_at"))
        if queued:
            waits.append((started - queued).total_seconds() * 1000)
        if a.get("duration_ms") is not None:
            execs.append(float(a["duration_ms"]))
            busy += a["duration_ms"] / 1000
        if finished and finished > last:
            last = finished
    waits.sort()
    execs.sort()
    wall = (last - start_wall).total_seconds()
    return {
        "tag": tag, "jobs_submitted": submitted, "submit_errors": errors, "submit_seconds": submit_secs,
        "submit_per_second": submitted / submit_secs if submit_secs else 0, "attempts": len(attempts),
        "retries": retries, "failed_jobs": failed_jobs, "job_failure_rate": failed_jobs / submitted if submitted else 0,
        "wall_seconds": wall, "completed_per_second": submitted / wall if wall > 0 else 0,
        "queue_wait_p50_ms": percentile(waits, .5), "queue_wait_p95_ms": percentile(waits, .95),
        "queue_wait_p99_ms": percentile(waits, .99), "exec_p50_ms": percentile(execs, .5),
        "exec_p95_ms": percentile(execs, .95), "exec_p99_ms": percentile(execs, .99),
        "worker_capacity": capacity, "worker_utilization": busy / (wall * capacity) if wall > 0 else 0,
    }


def main(argv: list[str]) -> int:
    ap = argparse.ArgumentParser(prog="taskflow loadtest")
    ap.add_argument("--url", default="http://localhost:8080")
    ap.add_argument("--email", default=os.environ.get("LOADTEST_EMAIL", ""))
    ap.add_argument("--password", default=os.environ.get("LOADTEST_PASSWORD", ""))
    ap.add_argument("--jobs", type=int, default=500)
    ap.add_argument("--concurrency", type=int, default=20)
    ap.add_argument("--duration-ms", type=int, default=100)
    ap.add_argument("--fail-rate", type=float, default=0.0)
    ap.add_argument("--max-attempts", type=int, default=3)
    ap.add_argument("--timeout", type=float, default=600, help="seconds to wait for completion")
    ap.add_argument("--json", action="store_true")
    args = ap.parse_args(argv)
    if not args.email or not args.password:
        print("provide --email and --password (or LOADTEST_EMAIL / LOADTEST_PASSWORD)", file=sys.stderr)
        return 2
    if args.jobs < 1 or args.concurrency < 1:
        print("--jobs and --concurrency must be positive", file=sys.stderr)
        return 2
    r = asyncio.run(run(args))
    if args.json:
        print(json.dumps(r, indent=2))
    else:
        print(f"""
TaskFlow load test  {r['tag']}
------------------------------------------------------------
Jobs submitted         {r['jobs_submitted']}  ({r['submit_errors']} submit errors)
Submission rate        {r['submit_per_second']:.1f} jobs/s over {r['submit_seconds']:.1f}s
Attempts / retries     {r['attempts']} / {r['retries']}
Failed jobs            {r['failed_jobs']}  ({r['job_failure_rate'] * 100:.2f}%)
End-to-end throughput  {r['completed_per_second']:.1f} jobs/s  (wall {r['wall_seconds']:.1f}s)
Queue wait  p50/95/99  {r['queue_wait_p50_ms']:.0f} / {r['queue_wait_p95_ms']:.0f} / {r['queue_wait_p99_ms']:.0f} ms
Execution   p50/95/99  {r['exec_p50_ms']:.0f} / {r['exec_p95_ms']:.0f} / {r['exec_p99_ms']:.0f} ms
Worker utilisation     {r['worker_utilization'] * 100:.1f}% of {r['worker_capacity']} slots""")
    return 0
