"""PostgreSQL connection pool, transactions and schema migrations."""

from __future__ import annotations

import asyncio
from collections.abc import AsyncIterator
from contextlib import asynccontextmanager
from importlib import resources

from psycopg import AsyncConnection, errors
from psycopg.rows import dict_row
from psycopg_pool import AsyncConnectionPool

from .logs import Log

Conn = AsyncConnection  # connections use dict rows and autocommit

# Serialises concurrent migrators (e.g. several containers starting at once).
_MIGRATION_LOCK = 7_201_551_001


async def _configure(conn: AsyncConnection) -> None:
    # UTC everywhere: now() and timestamp rendering never depend on the server.
    await conn.execute("SET TIME ZONE 'UTC'")


async def connect(url: str, max_conns: int = 20, *, retries: int = 1, log: Log | None = None) -> AsyncConnectionPool:
    """Opens a pool, retrying while the database starts up."""
    last: Exception | None = None
    for attempt in range(1, retries + 1):
        pool = AsyncConnectionPool(
            url,
            min_size=1,
            max_size=max_conns,
            kwargs={"row_factory": dict_row, "autocommit": True},
            configure=_configure,
            open=False,
            max_idle=300,
        )
        try:
            await pool.open(wait=True, timeout=10)
            return pool
        except Exception as e:  # noqa: BLE001 - surfaced after retries
            last = e
            await pool.close()
            if log:
                log.warning("database not reachable yet; retrying", attempt=attempt, err=str(e))
            if attempt < retries:
                await asyncio.sleep(2)
    raise RuntimeError(f"database unreachable: {last}")


@asynccontextmanager
async def tx(pool: AsyncConnectionPool) -> AsyncIterator[AsyncConnection]:
    """A connection inside a transaction: commits on success, rolls back on error."""
    async with pool.connection() as conn, conn.transaction():
        yield conn


def is_unique_violation(err: BaseException, constraint: str = "") -> bool:
    if isinstance(err, errors.UniqueViolation):
        return not constraint or getattr(err.diag, "constraint_name", "") == constraint
    return False


async def migrate(pool: AsyncConnectionPool, log: Log) -> None:
    """Applies embedded migrations that have not been applied yet, each in its
    own transaction together with its bookkeeping row."""
    files = sorted(
        (f for f in resources.files("taskflow.migrations").iterdir() if f.name.endswith(".sql")),
        key=lambda f: f.name,
    )
    async with pool.connection() as conn:
        await conn.execute("SELECT pg_advisory_lock(%s)", (_MIGRATION_LOCK,))
        try:
            await conn.execute(
                "CREATE TABLE IF NOT EXISTS schema_migrations (version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())"
            )
            rows = await (await conn.execute("SELECT version FROM schema_migrations")).fetchall()
            applied = {r["version"] for r in rows}
            for f in files:
                version = f.name.removesuffix(".sql")
                if version in applied:
                    continue
                async with conn.transaction():
                    await conn.execute(f.read_text(encoding="utf-8"))
                    await conn.execute("INSERT INTO schema_migrations (version) VALUES (%s)", (version,))
                log.info("applied migration", version=version)
        finally:
            await conn.execute("SELECT pg_advisory_unlock(%s)", (_MIGRATION_LOCK,))
