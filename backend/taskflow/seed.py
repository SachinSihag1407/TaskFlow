"""Demo workspace, users and jobs (development and opt-in demos only).

Idempotent: re-running does not duplicate users or jobs. The password comes
from SEED_PASSWORD; if unset, a random one is generated and printed once.
Nothing in the repository contains a usable credential.
"""

from __future__ import annotations

from .config import Config
from .domain import NotFound, Principal, Role
from .engine import Engine
from .handlers import Registry
from .logs import Log
from .services.auth import Auth
from .services.jobs import JobInput, Jobs
from .store import users as user_store

DEMO_JOBS: list[tuple[str, dict]] = [
    ("nightly-sales", dict(name="Nightly sales report", description="Aggregates the previous day's sales by region.",
                           handler="report.generate",
                           payload={"report_type": "sales_summary", "period_days": 1, "seed": 2026},
                           schedule_type="cron", cron_expr="0 2 * * *", timezone="Europe/Berlin",
                           missed_run_policy="run_once", priority=6)),
    ("inventory-snapshot", dict(name="Hourly inventory snapshot", description="Inventory levels across all regions.",
                                handler="report.generate", payload={"report_type": "inventory", "period_days": 7},
                                schedule_type="cron", cron_expr="0 * * * *", timezone="UTC", missed_run_policy="skip")),
    ("heartbeat", dict(name="Pipeline heartbeat", description="Lightweight job every minute to keep the dashboard live.",
                       handler="test.sleep", payload={"duration_ms": 800}, schedule_type="cron", cron_expr="* * * * *",
                       timezone="UTC", missed_run_policy="skip", priority=3)),
    ("partner-sync", dict(name="Flaky partner sync",
                          description="Fails its first two attempts to demonstrate retries with backoff.",
                          handler="test.sleep", payload={"duration_ms": 1500, "fail_attempts": 2}, schedule_type="cron",
                          cron_expr="*/10 * * * *", timezone="UTC", max_attempts=4, backoff_base_seconds=5,
                          backoff_max_seconds=60)),
    ("customer-import", dict(name="Process customer import", description="20k records in checkpointed chunks.",
                             handler="batch.process",
                             payload={"total_records": 20000, "chunk_size": 1000, "delay_ms_per_chunk": 300,
                                      "fail_at_record": 12500},
                             schedule_type="once", max_attempts=3, backoff_base_seconds=5, backoff_max_seconds=30,
                             priority=7)),
    ("oncall-notify", dict(name="Notify on-call about release", handler="notification.send",
                           payload={"channel": "email", "recipients": ["oncall@example.com", "release@example.com"],
                                    "subject": "Release 2.4 deployed", "body": "All services healthy."},
                           schedule_type="delayed", delay_seconds=30)),
    ("broken-export", dict(name="Legacy export (broken)",
                           description="Always fails permanently; lands in the dead-letter queue.",
                           handler="test.sleep", payload={"duration_ms": 500, "fail_attempts": 25, "permanent": True},
                           schedule_type="once", max_attempts=3)),
    ("reindex", dict(name="Search reindex", description="Long-running job with progress logs.", handler="test.sleep",
                     payload={"duration_ms": 45000}, schedule_type="delayed", delay_seconds=10, timeout_seconds=120)),
    ("status-webhook", dict(name="Status page ping", description="Calls a public endpoint every 15 minutes.",
                            handler="http.webhook",
                            payload={"url": "https://example.com/", "method": "GET", "timeout_seconds": 10},
                            schedule_type="cron", cron_expr="*/15 * * * *", timezone="UTC", max_attempts=3)),
]


async def seed_demo(cfg: Config, pool, log: Log, registry: Registry, password: str, with_jobs: bool) -> bool:
    """Creates the demo workspace, its three users and the demo jobs; returns
    whether the users were newly created."""
    eng = Engine(pool, None, log)  # the scheduler publishes demo runs when they become due
    auth = Auth(pool, eng, cfg.session_ttl)
    created = False
    try:
        async with pool.connection() as conn:
            admin = await user_store.get_by_email(conn, "admin@example.com")
    except NotFound:
        admin = await auth.seed_user("Acme Engineering", "Ada Admin", "admin@example.com", password, Role.ADMIN)
        await auth.seed_user("", "Omar Operator", "operator@example.com", password, Role.OPERATOR, admin.tenant_id)
        await auth.seed_user("", "Vera Viewer", "viewer@example.com", password, Role.VIEWER, admin.tenant_id)
        created = True
    if not with_jobs:
        return created
    p = Principal(user_id=admin.id, tenant_id=admin.tenant_id, email=admin.email, role=Role.ADMIN)
    jobs = Jobs(pool, eng, registry, log)
    n = 0
    for key, spec in DEMO_JOBS:
        if not jobs.handler_supported(spec["handler"]):
            continue  # handler disabled in this deployment (ENABLED_HANDLERS)
        _, replayed = await jobs.create(p, JobInput(**spec), f"seed:{key}")
        n += 0 if replayed else 1
    log.info("demo data ready", users_created=created, jobs_created=n)
    return created
