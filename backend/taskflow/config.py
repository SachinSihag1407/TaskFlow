"""Process configuration, read from environment variables.

Secrets only ever come from the environment; nothing sensitive has a default.
"""

from __future__ import annotations

import os
import re
from dataclasses import dataclass, field


class ConfigError(Exception):
    """Raised when the environment contains invalid configuration."""


_DURATION = re.compile(r"^(\d+(?:\.\d+)?)(ms|s|m|h)$")


def parse_duration(value: str) -> float:
    """Parses Go-style durations such as 500ms, 30s, 5m, 168h into seconds."""
    m = _DURATION.match(value.strip())
    if not m:
        raise ValueError(value)
    n, unit = float(m.group(1)), m.group(2)
    return n * {"ms": 0.001, "s": 1, "m": 60, "h": 3600}[unit]


def _split(value: str | None) -> list[str]:
    return [p.strip() for p in (value or "").split(",") if p.strip()]


@dataclass
class Config:
    env: str = "development"
    log_level: str = "info"
    database_url: str = ""
    db_max_conns: int = 20
    redis_url: str = "redis://localhost:6379/0"
    # REDIS_URL=disabled: workers poll PostgreSQL and the UI polls for updates.
    redis_disabled: bool = False

    # API
    http_host: str = "0.0.0.0"
    http_port: int = 8080
    allowed_origins: list[str] = field(default_factory=list)
    cookie_secure: bool = False
    session_ttl: float = 7 * 24 * 3600
    metrics_token: str = ""
    max_request_bytes: int = 1 << 20
    allow_registration: bool = True
    trust_proxy_headers: bool = False
    register_per_hour: int = 5
    # Admins allowed to operate shared infrastructure (drain workers).
    platform_admins: list[str] = field(default_factory=list)
    # Serve the built frontend from this directory (single-origin deployments).
    static_dir: str = ""

    # Worker
    worker_concurrency: int = 4
    worker_heartbeat: float = 5
    lease_duration: float = 30
    shutdown_grace: float = 30
    admin_port: int = 9090
    worker_poll_interval: float = 2
    allow_private_webhooks: bool = False
    webhook_max_resp_bytes: int = 64 << 10
    worker_handlers: list[str] = field(default_factory=list)
    # Handlers offered by the whole deployment; empty = all built-ins.
    enabled_handlers: list[str] = field(default_factory=list)

    # Scheduler
    scheduler_tick: float = 1
    reconcile_interval: float = 10
    worker_stale_after: float = 20
    worker_offline_after: float = 120
    scheduler_batch_size: int = 200

    # Demo seeding (all-in-one)
    seed_demo: bool = False
    seed_password: str = ""

    @property
    def production(self) -> bool:
        return self.env == "production"

    @classmethod
    def load(cls, env: dict[str, str] | None = None) -> Config:
        e = dict(os.environ if env is None else env)
        errors: list[str] = []

        def get(key: str, default: str = "") -> str:
            v = e.get(key, "")
            return v if v != "" else default

        def get_int(key: str, default: int) -> int:
            v = e.get(key, "")
            if v == "":
                return default
            try:
                return int(v)
            except ValueError:
                errors.append(f"{key}: invalid integer {v!r}")
                return default

        def get_bool(key: str, default: bool) -> bool:
            v = e.get(key, "").strip().lower()
            if v == "":
                return default
            if v in ("1", "true", "yes", "on"):
                return True
            if v in ("0", "false", "no", "off"):
                return False
            errors.append(f"{key}: invalid boolean {v!r}")
            return default

        def get_dur(key: str, default: float) -> float:
            v = e.get(key, "")
            if v == "":
                return default
            try:
                d = parse_duration(v)
                if d <= 0:
                    raise ValueError
                return d
            except ValueError:
                errors.append(f"{key}: invalid duration {v!r}")
                return default

        c = cls()
        c.env = get("TASKFLOW_ENV", "development")
        c.log_level = get("LOG_LEVEL", "info")
        c.database_url = get("DATABASE_URL")
        c.db_max_conns = get_int("DB_MAX_CONNS", 20)
        c.redis_url = get("REDIS_URL", "redis://localhost:6379/0")
        if c.redis_url.strip().lower() in ("disabled", "off", "none"):
            c.redis_disabled, c.redis_url = True, ""

        c.http_port = get_int("PORT", 0) or get_int("HTTP_PORT", 8080)
        c.allowed_origins = _split(get("CORS_ALLOWED_ORIGINS", "http://localhost:5173,http://localhost:3000"))
        c.cookie_secure = get_bool("COOKIE_SECURE", c.env == "production")
        c.session_ttl = get_dur("SESSION_TTL", 7 * 24 * 3600)
        c.metrics_token = get("METRICS_TOKEN")
        c.max_request_bytes = get_int("MAX_REQUEST_BYTES", 1 << 20)
        c.allow_registration = get_bool("ALLOW_REGISTRATION", True)
        c.trust_proxy_headers = get_bool("TRUST_PROXY_HEADERS", False)
        c.register_per_hour = get_int("RATE_LIMIT_REGISTER_PER_HOUR", 5)
        c.platform_admins = [x.lower() for x in _split(e.get("PLATFORM_ADMIN_EMAILS"))]
        c.static_dir = get("STATIC_DIR")

        c.worker_concurrency = get_int("WORKER_CONCURRENCY", 4)
        c.worker_heartbeat = get_dur("WORKER_HEARTBEAT_INTERVAL", 5)
        c.lease_duration = get_dur("LEASE_DURATION", 30)
        c.shutdown_grace = get_dur("SHUTDOWN_GRACE", 30)
        c.admin_port = get_int("ADMIN_PORT", 9090)
        c.worker_poll_interval = get_dur("WORKER_POLL_INTERVAL", 2)
        c.allow_private_webhooks = get_bool("WEBHOOK_ALLOW_PRIVATE_NETWORKS", False)
        c.webhook_max_resp_bytes = get_int("WEBHOOK_MAX_RESPONSE_BYTES", 64 << 10)
        c.worker_handlers = _split(e.get("WORKER_HANDLERS"))
        c.enabled_handlers = _split(e.get("ENABLED_HANDLERS"))

        c.scheduler_tick = get_dur("SCHEDULER_TICK", 1)
        c.reconcile_interval = get_dur("RECONCILE_INTERVAL", 10)
        c.worker_stale_after = get_dur("WORKER_STALE_AFTER", 20)
        c.worker_offline_after = get_dur("WORKER_OFFLINE_AFTER", 120)
        c.scheduler_batch_size = get_int("SCHEDULER_BATCH_SIZE", 200)

        c.seed_demo = get_bool("SEED_DEMO", False)
        c.seed_password = get("SEED_PASSWORD")

        if not c.database_url:
            errors.append("DATABASE_URL is required")
        if not 1 <= c.worker_concurrency <= 256:
            errors.append("WORKER_CONCURRENCY must be between 1 and 256")
        if c.lease_duration < 3 * c.worker_heartbeat:
            errors.append("LEASE_DURATION must be at least 3x WORKER_HEARTBEAT_INTERVAL")
        if c.worker_offline_after <= c.worker_stale_after:
            errors.append("WORKER_OFFLINE_AFTER must exceed WORKER_STALE_AFTER")
        if c.production and c.allow_private_webhooks:
            errors.append("WEBHOOK_ALLOW_PRIVATE_NETWORKS must not be enabled in production")
        if c.seed_demo and len(c.seed_password) < 10:
            errors.append("SEED_DEMO requires SEED_PASSWORD of at least 10 characters")
        if errors:
            raise ConfigError("; ".join(errors))
        return c
