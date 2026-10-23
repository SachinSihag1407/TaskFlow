"""Registration, login, sessions and member management."""

from __future__ import annotations

import re
import uuid
from dataclasses import dataclass
from datetime import UTC, datetime, timedelta
from uuid import UUID

from .. import auth
from ..db import tx
from ..domain import (
    Conflict,
    NotFound,
    Principal,
    Role,
    Tenant,
    Unauthorized,
    User,
    ValidationFailed,
    Validator,
)
from ..engine import Engine
from ..store import users as user_store
from . import Input


class InvalidCredentials(Exception):
    """Deliberately generic: does not reveal whether the email exists or the
    account is disabled."""


# A single plain address: no display name, comments or quoted local parts.
_EMAIL = re.compile(r"^[A-Za-z0-9.!#$%&'*+/=?^_`{|}~-]+@[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?"
                    r"(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$")
_ROLES = {r.value for r in Role}


class RegisterInput(Input):
    name: str = ""
    email: str = ""
    password: str = ""
    workspace: str = ""


class LoginInput(Input):
    email: str = ""
    password: str = ""


class PasswordInput(Input):
    current_password: str = ""
    new_password: str = ""


class UserInput(Input):
    name: str = ""
    email: str = ""
    password: str = ""
    role: str = ""


class UpdateUserInput(Input):
    role: str | None = None
    disabled: bool | None = None


@dataclass
class IssuedSession:
    token: str
    csrf_token: str
    expires_at: datetime
    user: User


def normalize_email(s: str) -> str:
    return s.strip().lower()


def valid_email(email: str) -> bool:
    return 3 <= len(email) <= 254 and _EMAIL.match(email) is not None


def principal_for(u: User, ip: str = "", ua: str = "") -> Principal:
    return Principal(user_id=u.id, tenant_id=u.tenant_id, email=u.email, role=u.role, ip=ip, user_agent=ua)


class Auth:
    def __init__(self, pool, eng: Engine, session_ttl: float):
        self.pool = pool
        self.eng = eng
        self.session_ttl = timedelta(seconds=session_ttl)
        self.now = lambda: datetime.now(UTC)

    async def register(self, body: RegisterInput, ip: str, ua: str) -> IssuedSession:
        """Creates a workspace with the caller as admin and signs them in."""
        email, name, workspace = normalize_email(body.email), body.name.strip(), body.workspace.strip()
        v = Validator()
        v.check(name != "" and len(name) <= 100, "name", "Enter your name (max 100 characters).")
        v.check(valid_email(email), "email", "Enter a valid email address.")
        if msg := auth.password_problem(body.password):
            v.add("password", msg)
        workspace = workspace or f"{name}'s workspace"
        v.check(len(workspace) <= 100, "workspace", "Workspace name must be at most 100 characters.")
        v.raise_if_any()
        pw_hash = await auth.hash_password(body.password)
        now = self.now()
        tenant = Tenant(id=uuid.uuid4(), name=workspace, created_at=now)
        user = User(id=uuid.uuid4(), tenant_id=tenant.id, email=email, name=name, password_hash=pw_hash,
                    role=Role.ADMIN, created_at=now)
        async with tx(self.pool) as conn:
            await user_store.insert_tenant(conn, tenant)
            try:
                await user_store.insert_user(conn, user)
            except Conflict:
                raise ValidationFailed({"email": "An account with this email already exists."}) from None
            await self.eng.audit(conn, principal_for(user, ip, ua), "auth.register", "user", str(user.id),
                                 {"workspace": tenant.name})
            return await self._issue(conn, user, ip, ua)

    async def login(self, email: str, password: str, ip: str, ua: str) -> IssuedSession:
        email = normalize_email(email)
        if not email or not password or len(password) > 256:
            await auth.verify_dummy(password[:256])
            raise InvalidCredentials()
        async with self.pool.connection() as conn:
            try:
                u = await user_store.get_by_email(conn, email)
            except NotFound:
                await auth.verify_dummy(password)
                await self.eng.audit(conn, None, "auth.login_failed", "email", email, {"ip": ip})
                raise InvalidCredentials() from None
            if not await auth.verify_password(password, u.password_hash) or u.disabled_at is not None:
                await self.eng.audit(conn, principal_for(u, ip, ua), "auth.login_failed", "user", str(u.id))
                raise InvalidCredentials()
        async with tx(self.pool) as conn:
            await user_store.touch_login(conn, u.id, self.now())
            await self.eng.audit(conn, principal_for(u, ip, ua), "auth.login", "user", str(u.id))
            return await self._issue(conn, u, ip, ua)

    async def _issue(self, conn, u: User, ip: str, ua: str) -> IssuedSession:
        token, csrf = auth.new_token(), auth.new_token()
        now = self.now()
        s = user_store.Session(id=uuid.uuid4(), user_id=u.id, csrf_token=csrf, expires_at=now + self.session_ttl)
        await user_store.insert_session(conn, s, auth.hash_token(token), ip, ua[:512], now)
        return IssuedSession(token, csrf, s.expires_at, u)

    async def authenticate(self, token: str, ip: str, ua: str) -> tuple[Principal, User]:
        """Resolves a session token, sliding its expiry at most once a minute
        to avoid a write per request."""
        if not token or len(token) > 100:
            raise Unauthorized()
        now = self.now()
        async with self.pool.connection() as conn:
            s, u = await user_store.session_user(conn, auth.hash_token(token), now)
            if u.disabled_at is not None:
                raise Unauthorized()
            if s.last_seen_at is None or now - s.last_seen_at > timedelta(minutes=1):
                try:
                    await user_store.touch_session(conn, s.id, now, now + self.session_ttl)
                except Exception:  # noqa: BLE001 - best effort
                    pass
        p = principal_for(u, ip, ua)
        p.session_id, p.csrf_token = s.id, s.csrf_token
        return p, u

    async def logout(self, p: Principal) -> None:
        async with self.pool.connection() as conn:
            await user_store.delete_session(conn, p.session_id)
            await self.eng.audit(conn, p, "auth.logout", "user", str(p.user_id))

    async def change_password(self, p: Principal, current: str, new: str) -> None:
        """Verifies the current password, sets a new one and revokes all other
        sessions of the user."""
        async with self.pool.connection() as conn:
            u = await user_store.get(conn, p.tenant_id, p.user_id)
        if not await auth.verify_password(current, u.password_hash):
            raise ValidationFailed({"current_password": "Current password is incorrect."})
        if msg := auth.password_problem(new):
            raise ValidationFailed({"new_password": msg})
        pw_hash = await auth.hash_password(new)
        async with tx(self.pool) as conn:
            await user_store.update_password(conn, u.id, pw_hash, self.now())
            await user_store.delete_user_sessions(conn, u.id, keep=p.session_id)
            await self.eng.audit(conn, p, "auth.password_changed", "user", str(u.id))

    async def create_user(self, p: Principal, body: UserInput) -> User:
        """Adds a member to the caller's workspace."""
        email, name = normalize_email(body.email), body.name.strip()
        v = Validator()
        v.check(name != "" and len(name) <= 100, "name", "Enter a name (max 100 characters).")
        v.check(valid_email(email), "email", "Enter a valid email address.")
        v.check(body.role in _ROLES, "role", "Role must be admin, operator or viewer.")
        if msg := auth.password_problem(body.password):
            v.add("password", msg)
        v.raise_if_any()
        u = User(id=uuid.uuid4(), tenant_id=p.tenant_id, email=email, name=name,
                 password_hash=await auth.hash_password(body.password), role=body.role, created_at=self.now())
        async with tx(self.pool) as conn:
            try:
                await user_store.insert_user(conn, u)
            except Conflict:
                raise ValidationFailed({"email": "An account with this email already exists."}) from None
            await self.eng.audit(conn, p, "user.create", "user", str(u.id), {"email": u.email, "role": u.role})
        return u

    async def update_user(self, p: Principal, user_id: UUID, body: UpdateUserInput) -> User:
        """Changes a member's role or disables them. A workspace always keeps
        at least one enabled admin, and admins cannot lock themselves out."""
        async with tx(self.pool) as conn:
            # Serialise membership changes per workspace so two admins cannot
            # demote each other concurrently and leave the workspace adminless.
            await conn.execute("SELECT pg_advisory_xact_lock(hashtextextended(%s, 0))", (f"members:{p.tenant_id}",))
            u = await user_store.get(conn, p.tenant_id, user_id)
            role, disabled_at = u.role, u.disabled_at
            if body.role is not None:
                if body.role not in _ROLES:
                    raise ValidationFailed({"role": "Role must be admin, operator or viewer."})
                role = body.role
            if body.disabled is True and disabled_at is None:
                disabled_at = self.now()
            elif body.disabled is False:
                disabled_at = None
            if user_id == p.user_id and (role != Role.ADMIN or disabled_at is not None):
                raise Conflict("self_lockout", "you cannot remove your own admin access")
            if u.role == Role.ADMIN and u.disabled_at is None and (role != Role.ADMIN or disabled_at is not None):
                if await user_store.count_active_admins(conn, p.tenant_id) <= 1:
                    raise Conflict("last_admin", "the workspace must keep at least one active admin")
            await user_store.update_access(conn, p.tenant_id, user_id, Role(role), disabled_at, self.now())
            if disabled_at is not None:
                await user_store.delete_user_sessions(conn, user_id)
            u.role, u.disabled_at = role, disabled_at
            await self.eng.audit(conn, p, "user.update", "user", str(user_id),
                                 {"email": u.email, "role": role, "disabled": disabled_at is not None})
        return u

    async def list_users(self, p: Principal) -> list[User]:
        async with self.pool.connection() as conn:
            return await user_store.list_users(conn, p.tenant_id)

    async def mark_notifications_seen(self, p: Principal) -> None:
        async with self.pool.connection() as conn:
            await user_store.set_notifications_seen(conn, p.user_id, self.now())

    async def seed_user(self, tenant_name: str, name: str, email: str, password: str, role: Role,
                        tenant_id: UUID | None = None) -> User:
        """Creates a workspace (unless tenant_id is given) and a user directly;
        used by the seed command."""
        now = self.now()
        u = User(id=uuid.uuid4(), tenant_id=tenant_id or uuid.uuid4(), email=normalize_email(email), name=name,
                 password_hash=await auth.hash_password(password), role=role, created_at=now)
        async with tx(self.pool) as conn:
            if tenant_id is None:
                await user_store.insert_tenant(conn, Tenant(id=u.tenant_id, name=tenant_name, created_at=now))
            await user_store.insert_user(conn, u)
        return u
