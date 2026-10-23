"""Fixtures for integration tests against real PostgreSQL and Redis.

TASKFLOW_TEST_DATABASE_URL must point at a disposable database: its public
schema is dropped and recreated once per session and every table is
truncated before each test. Tests that need it are skipped when unset.
TASKFLOW_TEST_REDIS_URL is optional; each test gets its own key prefix.
"""

from __future__ import annotations

import asyncio
import os
import uuid
from datetime import UTC, datetime

import psycopg
import pytest

from taskflow import db
from taskflow.domain import Principal, Role, Tenant, User
from taskflow.engine import Engine
from taskflow.handlers import builtin
from taskflow.logs import null_logger
from taskflow.queue import Queue
from taskflow.store import users as user_store

DB_URL = os.environ.get("TASKFLOW_TEST_DATABASE_URL", "")
REDIS_URL = os.environ.get("TASKFLOW_TEST_REDIS_URL", "")

TABLES = ("attempt_logs", "notification_deliveries", "job_attempts", "job_runs", "jobs", "system_events",
          "audit_events", "sessions", "users", "tenant_settings", "tenants", "workers", "scheduler_instances")


@pytest.fixture(scope="session")
def migrated() -> str:
    if not DB_URL:
        pytest.skip("TASKFLOW_TEST_DATABASE_URL not set")
    with psycopg.connect(DB_URL, autocommit=True) as conn:
        conn.execute("DROP SCHEMA public CASCADE")
        conn.execute("CREATE SCHEMA public")

    async def go():
        pool = await db.connect(DB_URL, 4)
        try:
            await db.migrate(pool, null_logger())
        finally:
            await pool.close()

    asyncio.run(go())
    return DB_URL


@pytest.fixture
async def pool(migrated):
    p = await db.connect(migrated, 20)
    async with p.connection() as conn:
        await conn.execute(f"TRUNCATE {', '.join(TABLES)} CASCADE")
    yield p
    await p.close()


@pytest.fixture
async def queue():
    if not REDIS_URL:
        pytest.skip("TASKFLOW_TEST_REDIS_URL not set")
    q = Queue(REDIS_URL, prefix=f"tftest-{uuid.uuid4().hex[:8]}")
    yield q
    keys = [k async for k in q.rdb.scan_iter(f"{q.prefix}:*")]
    if keys:
        await q.rdb.delete(*keys)
    await q.close()


class Clock:
    """A controllable clock for the engine."""

    def __init__(self) -> None:
        self.t: datetime | None = None

    def __call__(self) -> datetime:
        return self.t or datetime.now(UTC)

    def advance(self, seconds: float) -> None:
        from datetime import timedelta
        self.t = self() + timedelta(seconds=seconds)


@pytest.fixture
def clock() -> Clock:
    return Clock()


@pytest.fixture
def engine(pool, clock) -> Engine:
    return Engine(pool, None, null_logger(), now=clock, rand=lambda: 0.0)


@pytest.fixture
def registry(pool):
    return builtin(pool, allow_private_webhooks=False)


async def make_tenant(pool, role: Role = Role.ADMIN, email: str | None = None, max_concurrent: int = 0) -> Principal:
    now = datetime.now(UTC)
    t = Tenant(id=uuid.uuid4(), name="Test workspace", created_at=now)
    u = User(id=uuid.uuid4(), tenant_id=t.id, email=email or f"{uuid.uuid4().hex[:8]}@example.com", name="Test",
             password_hash="x", role=role, created_at=now)
    async with pool.connection() as conn:
        await user_store.insert_tenant(conn, t)
        await user_store.insert_user(conn, u)
        if max_concurrent:
            await conn.execute("UPDATE tenant_settings SET max_concurrent_runs = %s WHERE tenant_id = %s",
                               (max_concurrent, t.id))
    return Principal(user_id=u.id, tenant_id=t.id, email=u.email, role=role)


@pytest.fixture
async def admin(pool) -> Principal:
    return await make_tenant(pool)


async def make_worker(pool) -> uuid.UUID:
    """Registers a worker row (attempts reference their worker)."""
    from taskflow.domain import Worker, WorkerState
    from taskflow.store import workers as worker_store
    now = datetime.now(UTC)
    w = Worker(id=uuid.uuid4(), hostname="test", pid=1, version="test", concurrency=4, handlers=["test.sleep"],
               status=WorkerState.ONLINE, started_at=now, last_heartbeat_at=now)
    async with pool.connection() as conn:
        await worker_store.register(conn, w)
    return w.id


@pytest.fixture
def static_dir(tmp_path):
    (tmp_path / "index.html").write_text("<!doctype html><title>TaskFlow</title>")
    (tmp_path / "assets").mkdir()
    (tmp_path / "assets" / "app-123.js").write_text("console.log(1)")
    (tmp_path.parent / "secret.txt").write_text("outside")
    return tmp_path


@pytest.fixture
def app(pool, registry, migrated, static_dir):
    from taskflow.api.app import Deps, create_app
    from taskflow.config import Config
    from taskflow.services.auth import Auth
    from taskflow.services.jobs import Jobs
    from taskflow.services.ops import Ops

    cfg = Config.load({"DATABASE_URL": migrated, "REDIS_URL": "disabled", "RATE_LIMIT_REGISTER_PER_HOUR": "1000",
                       "PLATFORM_ADMIN_EMAILS": "root@example.com", "STATIC_DIR": str(static_dir),
                       "CORS_ALLOWED_ORIGINS": "http://localhost:5173"})
    log = null_logger()
    eng = Engine(pool, None, log)
    return create_app(Deps(cfg=cfg, log=log, pool=pool, q=None, eng=eng, jobs=Jobs(pool, eng, registry, log),
                           auth=Auth(pool, eng, cfg.session_ttl), ops=Ops(pool, eng, None, 20, 120),
                           registry=registry))
