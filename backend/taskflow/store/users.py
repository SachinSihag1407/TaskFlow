"""Workspaces, users, sessions and workspace settings."""

from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime
from uuid import UUID

from psycopg import errors

from ..db import Conn, is_unique_violation
from ..domain import Conflict, NotFound, Role, Settings, Tenant, Unauthorized, User

USER_COLUMNS = """u.id, u.tenant_id, u.email, u.name, u.password_hash, u.role, u.notifications_seen_at,
    u.last_login_at, u.disabled_at, u.created_at"""


async def insert_tenant(conn: Conn, t: Tenant) -> None:
    await conn.execute("INSERT INTO tenants (id, name, created_at) VALUES (%s, %s, %s)", (t.id, t.name, t.created_at))
    await conn.execute("INSERT INTO tenant_settings (tenant_id) VALUES (%s)", (t.id,))


async def get_tenant(conn: Conn, tenant_id: UUID) -> Tenant:
    row = await (await conn.execute("SELECT id, name, created_at FROM tenants WHERE id = %s", (tenant_id,))).fetchone()
    if row is None:
        raise NotFound()
    return Tenant(**row)


async def insert_user(conn: Conn, u: User) -> None:
    try:
        async with conn.transaction():  # savepoint, so a duplicate doesn't abort the caller's transaction
            await conn.execute(
                """INSERT INTO users (id, tenant_id, email, name, password_hash, role, created_at, updated_at)
                VALUES (%s, %s, %s, %s, %s, %s, %s, %s)""",
                (u.id, u.tenant_id, u.email, u.name, u.password_hash, str(u.role), u.created_at, u.created_at))
    except errors.UniqueViolation as e:
        if is_unique_violation(e, "users_email_key"):
            raise Conflict("email_taken", "an account with this email already exists") from None
        raise


async def _one(conn: Conn, sql: str, params: tuple) -> User:
    row = await (await conn.execute(sql, params)).fetchone()
    if row is None:
        raise NotFound()
    return User(**row)


async def get_by_email(conn: Conn, email: str) -> User:
    return await _one(conn, f"SELECT {USER_COLUMNS} FROM users u WHERE lower(u.email) = lower(%s)", (email,))


async def get(conn: Conn, tenant_id: UUID, user_id: UUID) -> User:
    return await _one(conn, f"SELECT {USER_COLUMNS} FROM users u WHERE u.id = %s AND u.tenant_id = %s", (user_id, tenant_id))


async def list_users(conn: Conn, tenant_id: UUID) -> list[User]:
    rows = await (await conn.execute(f"SELECT {USER_COLUMNS} FROM users u WHERE u.tenant_id = %s ORDER BY u.created_at",
                                     (tenant_id,))).fetchall()
    return [User(**r) for r in rows]


async def update_access(conn: Conn, tenant_id: UUID, user_id: UUID, role: Role, disabled_at: datetime | None,
                        now: datetime) -> None:
    cur = await conn.execute("UPDATE users SET role = %s, disabled_at = %s, updated_at = %s WHERE id = %s AND tenant_id = %s",
                             (str(role), disabled_at, now, user_id, tenant_id))
    if cur.rowcount == 0:
        raise NotFound()


async def count_active_admins(conn: Conn, tenant_id: UUID) -> int:
    row = await (await conn.execute("""SELECT count(*) AS n FROM users
        WHERE tenant_id = %s AND role = 'admin' AND disabled_at IS NULL""", (tenant_id,))).fetchone()
    return row["n"]


async def update_password(conn: Conn, user_id: UUID, password_hash: str, now: datetime) -> None:
    await conn.execute("UPDATE users SET password_hash = %s, updated_at = %s WHERE id = %s", (password_hash, now, user_id))


async def touch_login(conn: Conn, user_id: UUID, now: datetime) -> None:
    await conn.execute("UPDATE users SET last_login_at = %s WHERE id = %s", (now, user_id))


async def set_notifications_seen(conn: Conn, user_id: UUID, now: datetime) -> None:
    await conn.execute("UPDATE users SET notifications_seen_at = %s WHERE id = %s", (now, user_id))


@dataclass
class Session:
    id: UUID
    user_id: UUID
    csrf_token: str
    expires_at: datetime
    last_seen_at: datetime | None = None


async def insert_session(conn: Conn, s: Session, token_hash: bytes, ip: str, ua: str, now: datetime) -> None:
    await conn.execute(
        """INSERT INTO sessions (id, user_id, token_hash, csrf_token, ip, user_agent, created_at, last_seen_at, expires_at)
        VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s)""",
        (s.id, s.user_id, token_hash, s.csrf_token, ip, ua, now, now, s.expires_at))


async def session_user(conn: Conn, token_hash: bytes, now: datetime) -> tuple[Session, User]:
    row = await (await conn.execute(
        f"""SELECT s.id AS s_id, s.user_id AS s_user_id, s.csrf_token AS s_csrf, s.expires_at AS s_expires,
            s.last_seen_at AS s_seen, {USER_COLUMNS}
        FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.token_hash = %s AND s.expires_at > %s""",
        (token_hash, now))).fetchone()
    if row is None:
        raise Unauthorized()
    sess = Session(row.pop("s_id"), row.pop("s_user_id"), row.pop("s_csrf"), row.pop("s_expires"), row.pop("s_seen"))
    return sess, User(**row)


async def touch_session(conn: Conn, session_id: UUID, now: datetime, expires: datetime) -> None:
    await conn.execute("UPDATE sessions SET last_seen_at = %s, expires_at = %s WHERE id = %s", (now, expires, session_id))


async def delete_session(conn: Conn, session_id: UUID) -> None:
    await conn.execute("DELETE FROM sessions WHERE id = %s", (session_id,))


async def delete_user_sessions(conn: Conn, user_id: UUID, keep: UUID | None = None) -> None:
    await conn.execute("DELETE FROM sessions WHERE user_id = %s AND (%s::uuid IS NULL OR id <> %s)", (user_id, keep, keep))


async def delete_expired_sessions(conn: Conn, now: datetime) -> int:
    return (await conn.execute("DELETE FROM sessions WHERE expires_at < %s", (now,))).rowcount


SETTINGS_COLUMNS = """tenant_id, default_max_attempts, default_timeout_seconds, default_backoff_base_seconds,
    default_backoff_max_seconds, default_priority, default_timezone, max_concurrent_runs, event_retention_days, updated_at"""


async def get_settings(conn: Conn, tenant_id: UUID) -> Settings:
    row = await (await conn.execute(f"SELECT {SETTINGS_COLUMNS} FROM tenant_settings WHERE tenant_id = %s",
                                    (tenant_id,))).fetchone()
    if row is None:
        raise NotFound()
    return Settings(**row)


async def update_settings(conn: Conn, tenant_id: UUID, s: Settings, by: UUID, now: datetime) -> None:
    await conn.execute(
        """UPDATE tenant_settings SET default_max_attempts=%(default_max_attempts)s,
            default_timeout_seconds=%(default_timeout_seconds)s,
            default_backoff_base_seconds=%(default_backoff_base_seconds)s,
            default_backoff_max_seconds=%(default_backoff_max_seconds)s, default_priority=%(default_priority)s,
            default_timezone=%(default_timezone)s, max_concurrent_runs=%(max_concurrent_runs)s,
            event_retention_days=%(event_retention_days)s, updated_at=%(now)s, updated_by=%(by)s
        WHERE tenant_id=%(t)s""", {**s.model_dump(), "now": now, "by": by, "t": tenant_id})


async def tenant_concurrency_limit(conn: Conn, tenant_id: UUID) -> int:
    row = await (await conn.execute("SELECT max_concurrent_runs FROM tenant_settings WHERE tenant_id = %s",
                                    (tenant_id,))).fetchone()
    return row["max_concurrent_runs"] if row else 0
