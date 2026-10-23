"""Core types: statuses, entities and domain errors.

Entities are Pydantic models so they serialise to exactly the JSON the
frontend expects. Fields marked ``exclude=True`` (tenant ids, password
hashes) never leave the server.
"""

from __future__ import annotations

from datetime import datetime
from enum import StrEnum
from typing import Any
from uuid import UUID

from pydantic import BaseModel, ConfigDict, Field


class JobStatus(StrEnum):
    """Aggregate job status, derived from runs by statemachine.derive_job_status."""

    CREATED = "CREATED"
    SCHEDULED = "SCHEDULED"
    QUEUED = "QUEUED"
    RUNNING = "RUNNING"
    RETRY_WAIT = "RETRY_WAIT"
    PAUSED = "PAUSED"
    SUCCEEDED = "SUCCEEDED"
    FAILED = "FAILED"
    CANCELLED = "CANCELLED"


class RunStatus(StrEnum):
    """Status of one occurrence of a job; follows the strict transition table."""

    QUEUED = "QUEUED"
    RUNNING = "RUNNING"
    RETRY_WAIT = "RETRY_WAIT"
    SUCCEEDED = "SUCCEEDED"
    FAILED = "FAILED"
    CANCELLED = "CANCELLED"

    @property
    def terminal(self) -> bool:
        return self in (RunStatus.SUCCEEDED, RunStatus.FAILED, RunStatus.CANCELLED)


class AttemptStatus(StrEnum):
    RUNNING = "RUNNING"
    SUCCEEDED = "SUCCEEDED"
    FAILED = "FAILED"
    TIMED_OUT = "TIMED_OUT"
    CANCELLED = "CANCELLED"
    # The worker stopped renewing its lease; the outcome of the work is unknown.
    LOST = "LOST"


class ScheduleType(StrEnum):
    ONCE = "once"
    DELAYED = "delayed"
    CRON = "cron"


class MissedRunPolicy(StrEnum):
    SKIP = "skip"
    RUN_ONCE = "run_once"
    RUN_ALL = "run_all"


class OverlapPolicy(StrEnum):
    SKIP = "skip"
    QUEUE = "queue"
    ALLOW = "allow"


class Trigger(StrEnum):
    SCHEDULE = "schedule"
    MANUAL = "manual"
    RETRY = "retry"
    REPLAY = "replay"


class Role(StrEnum):
    ADMIN = "admin"
    OPERATOR = "operator"
    VIEWER = "viewer"

    @property
    def can_write(self) -> bool:
        return self in (Role.ADMIN, Role.OPERATOR)


class WorkerState(StrEnum):
    ONLINE = "ONLINE"
    DRAINING = "DRAINING"
    OFFLINE = "OFFLINE"
    # Never stored: computed for workers whose heartbeat is overdue.
    STALE = "STALE"


class Severity(StrEnum):
    INFO = "info"
    WARNING = "warning"
    ERROR = "error"


class Model(BaseModel):
    model_config = ConfigDict(use_enum_values=True)


class Tenant(Model):
    id: UUID
    name: str
    created_at: datetime


class User(Model):
    id: UUID
    tenant_id: UUID
    email: str
    name: str
    password_hash: str = Field(default="", exclude=True)
    role: Role
    notifications_seen_at: datetime | None = None
    last_login_at: datetime | None = None
    disabled_at: datetime | None = None
    created_at: datetime


class Principal(Model):
    """The authenticated caller attached to a request."""

    user_id: UUID
    tenant_id: UUID
    email: str
    role: Role
    session_id: UUID | None = None
    csrf_token: str = ""
    ip: str = ""
    user_agent: str = ""


class Settings(Model):
    tenant_id: UUID | None = Field(default=None, exclude=True)
    default_max_attempts: int
    default_timeout_seconds: int
    default_backoff_base_seconds: int
    default_backoff_max_seconds: int
    default_priority: int
    default_timezone: str
    max_concurrent_runs: int
    event_retention_days: int
    updated_at: datetime | None = None


class Job(Model):
    id: UUID
    tenant_id: UUID = Field(exclude=True)
    created_by: UUID | None = None
    name: str
    description: str = ""
    handler: str
    payload: Any = Field(default_factory=dict)
    schedule_type: ScheduleType
    run_at: datetime | None = None
    cron_expr: str | None = None
    timezone: str = "UTC"
    missed_run_policy: MissedRunPolicy = MissedRunPolicy.RUN_ONCE
    overlap_policy: OverlapPolicy = OverlapPolicy.SKIP
    max_concurrency: int = 1
    priority: int = 5
    max_attempts: int = 3
    backoff_base_seconds: int = 10
    backoff_max_seconds: int = 600
    timeout_seconds: int = 300
    status: JobStatus = JobStatus.CREATED
    next_run_at: datetime | None = None
    last_run_at: datetime | None = None
    last_run_status: RunStatus | None = None
    paused_at: datetime | None = None
    cancelled_at: datetime | None = None
    idempotency_key: str | None = None
    request_hash: str | None = Field(default=None, exclude=True)
    version: int = 1
    created_at: datetime
    updated_at: datetime

    @property
    def recurring(self) -> bool:
        return self.schedule_type == ScheduleType.CRON


class Run(Model):
    id: UUID
    job_id: UUID
    tenant_id: UUID = Field(exclude=True)
    scheduled_for: datetime
    trigger: Trigger
    status: RunStatus
    priority: int
    handler: str
    payload: Any = Field(default_factory=dict)
    timeout_seconds: int
    max_attempts: int
    backoff_base_seconds: int
    backoff_max_seconds: int
    attempt: int = 0
    current_attempt_id: UUID | None = None
    worker_id: UUID | None = None
    lease_expires_at: datetime | None = None
    queued_at: datetime | None = None
    started_at: datetime | None = None
    finished_at: datetime | None = None
    next_attempt_at: datetime | None = None
    cancel_requested_at: datetime | None = None
    last_error: str | None = None
    result: Any = None
    checkpoint: Any = None
    dead_lettered_at: datetime | None = None
    replayed_at: datetime | None = None
    replay_of: UUID | None = None
    created_at: datetime
    updated_at: datetime
    job_name: str | None = None


class Attempt(Model):
    id: UUID
    run_id: UUID
    job_id: UUID
    tenant_id: UUID = Field(exclude=True)
    attempt_number: int
    worker_id: UUID | None = None
    handler: str
    status: AttemptStatus
    queued_at: datetime | None = None
    started_at: datetime
    finished_at: datetime | None = None
    duration_ms: int | None = None
    error_kind: str | None = None
    error_message: str | None = None
    retryable: bool | None = None
    next_retry_at: datetime | None = None
    job_name: str | None = None
    worker_hostname: str | None = None
    max_attempts: int | None = None


class LogLine(Model):
    id: int = 0
    ts: datetime
    level: str
    message: str


class Worker(Model):
    id: UUID
    hostname: str
    pid: int
    version: str
    concurrency: int
    handlers: list[str] = Field(default_factory=list)
    status: WorkerState
    active_runs: int = 0
    completed_count: int = 0
    failed_count: int = 0
    heap_alloc_bytes: int | None = None
    goroutines: int | None = None
    drain_requested_at: datetime | None = None
    started_at: datetime
    last_heartbeat_at: datetime
    stopped_at: datetime | None = None


class SchedulerInstance(Model):
    id: UUID
    hostname: str
    is_leader: bool
    started_at: datetime
    last_heartbeat_at: datetime
    last_tick_at: datetime | None = None
    last_tick_ms: int | None = None
    last_tick_enqueued: int | None = None
    stopped_at: datetime | None = None


class Event(Model):
    id: int = 0
    tenant_id: UUID | None = Field(default=None, exclude=True)
    job_id: UUID | None = None
    run_id: UUID | None = None
    worker_id: UUID | None = None
    type: str
    severity: Severity
    message: str
    data: Any = None
    created_at: datetime | None = None


class AuditEvent(Model):
    id: int
    tenant_id: UUID | None = Field(default=None, exclude=True)
    actor_id: UUID | None = None
    actor_email: str | None = None
    action: str
    target_type: str | None = None
    target_id: str | None = None
    ip: str | None = None
    user_agent: str | None = None
    metadata: Any = None
    created_at: datetime


# ---- Errors -----------------------------------------------------------------


class NotFound(Exception):
    """Resource missing or outside the caller's workspace."""


class Forbidden(Exception):
    pass


class Unauthorized(Exception):
    pass


class Conflict(Exception):
    """The request is valid but cannot be applied to the current state."""

    def __init__(self, code: str, message: str):
        super().__init__(message)
        self.code = code
        self.message = message


class ValidationFailed(Exception):
    """Per-field validation messages (HTTP 422)."""

    def __init__(self, fields: dict[str, str]):
        super().__init__("validation failed: " + "; ".join(f"{k}: {v}" for k, v in sorted(fields.items())))
        self.fields = fields


class Validator:
    """Accumulates field errors; the first message per field wins."""

    def __init__(self) -> None:
        self.fields: dict[str, str] = {}

    def add(self, field: str, msg: str) -> None:
        self.fields.setdefault(field, msg)

    def check(self, ok: bool, field: str, msg: str) -> None:
        if not ok:
            self.add(field, msg)

    def raise_if_any(self) -> None:
        if self.fields:
            raise ValidationFailed(self.fields)
