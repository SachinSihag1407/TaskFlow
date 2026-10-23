"""Job management: create, update, read models, schedules and bulk actions."""

from __future__ import annotations

import hashlib
import json
import uuid
from dataclasses import dataclass
from datetime import UTC, datetime, timedelta
from typing import Any
from uuid import UUID

from .. import cron
from .. import statemachine as sm
from ..db import tx
from ..domain import (
    Conflict,
    Event,
    Job,
    JobStatus,
    MissedRunPolicy,
    NotFound,
    OverlapPolicy,
    Principal,
    ScheduleType,
    Settings,
    Severity,
    ValidationFailed,
    Validator,
)
from ..engine import Effects, Engine
from ..handlers import Registry
from ..logs import Log
from ..store import attempts as attempt_store
from ..store import events as event_store
from ..store import jobs as job_store
from ..store import runs as run_store
from ..store import users as user_store
from ..store.runs import RunFilter
from . import Input, capitalize

# Bounds a job's JSON payload.
MAX_PAYLOAD_BYTES = 64 << 10


class JobInput(Input):
    """The create/update request body. Optional numeric fields fall back to
    the workspace defaults."""

    name: str = ""
    description: str = ""
    handler: str = ""
    payload: Any = None
    schedule_type: str = ""
    run_at: datetime | None = None
    delay_seconds: int | None = None
    cron_expr: str | None = None
    timezone: str = ""
    missed_run_policy: str = ""
    overlap_policy: str = ""
    max_concurrency: int | None = None
    priority: int | None = None
    max_attempts: int | None = None
    backoff_base_seconds: int | None = None
    backoff_max_seconds: int | None = None
    timeout_seconds: int | None = None


class BulkInput(Input):
    action: str = ""
    ids: list[UUID] = []


class PreviewInput(Input):
    cron_expr: str = ""
    timezone: str = ""
    count: int = 0


def _or(v: int | None, default: int) -> int:
    return default if v is None else v


def request_hash(body: JobInput) -> str:
    canonical = json.dumps(body.model_dump(mode="json"), sort_keys=True, separators=(",", ":"))
    return hashlib.sha256(canonical.encode()).hexdigest()


def first_run(j: Job, now: datetime) -> datetime | None:
    """The initial next_run_at for a validated definition."""
    if j.schedule_type == ScheduleType.CRON:
        try:
            return cron.parse(j.cron_expr or "", j.timezone).next(now)
        except cron.CronError:
            return None
    return j.run_at


def _upcoming(j: Job) -> list[datetime]:
    try:
        spec = cron.parse(j.cron_expr or "", j.timezone)
    except cron.CronError:
        return []
    return [j.next_run_at, *spec.next_n(j.next_run_at, 4)]


def diff_fields(a: Job, b: Job) -> list[str]:
    checks = [
        ("name", a.name != b.name),
        ("description", a.description != b.description),
        ("handler", a.handler != b.handler),
        ("payload", a.payload != b.payload),
        ("schedule", a.schedule_type != b.schedule_type or (a.cron_expr or "") != (b.cron_expr or "") or a.run_at != b.run_at),
        ("timezone", a.timezone != b.timezone),
        ("missed_run_policy", a.missed_run_policy != b.missed_run_policy),
        ("overlap_policy", a.overlap_policy != b.overlap_policy),
        ("max_concurrency", a.max_concurrency != b.max_concurrency),
        ("priority", a.priority != b.priority),
        ("retry_policy", (a.max_attempts, a.backoff_base_seconds, a.backoff_max_seconds)
         != (b.max_attempts, b.backoff_base_seconds, b.backoff_max_seconds)),
        ("timeout_seconds", a.timeout_seconds != b.timeout_seconds),
    ]
    return [name for name, changed in checks if changed] or ["no changes"]


@dataclass
class BulkResult:
    id: UUID
    ok: bool
    code: str = ""
    message: str = ""

    def to_json(self) -> dict:
        out: dict = {"id": str(self.id), "ok": self.ok}
        if self.code:
            out["code"], out["message"] = self.code, self.message
        return out


class Jobs:
    def __init__(self, pool, eng: Engine, reg: Registry, log: Log):
        self.pool = pool
        self.eng = eng
        self.reg = reg
        self.log = log
        self.now = lambda: datetime.now(UTC)

    def _build(self, body: JobInput, settings: Settings, now: datetime) -> Job:
        """Validates input and produces a job definition (without identity,
        status or timestamps). Shared by create and update."""
        v = Validator()
        name = body.name.strip()
        v.check(name != "", "name", "Name is required.")
        v.check(len(name) <= 200, "name", "Name must be at most 200 characters.")
        v.check(len(body.description) <= 2000, "description", "Description must be at most 2000 characters.")

        payload = {} if body.payload is None else body.payload
        h = self.reg.get(body.handler)
        if body.handler == "":
            v.add("handler", "Choose a handler.")
        elif h is None:
            v.add("handler", f'Unknown handler "{body.handler}".')
        if len(json.dumps(payload, separators=(",", ":"))) > MAX_PAYLOAD_BYTES:
            v.add("payload", "Payload must be at most 64 KB.")
        elif not isinstance(payload, dict):
            v.add("payload", "Payload must be a JSON object.")
        elif h is not None:
            try:
                h.validate(payload)
            except ValueError as e:
                v.add("payload", capitalize(str(e)) + ".")

        tz = body.timezone or settings.default_timezone
        try:
            cron.load_timezone(tz)
        except cron.CronError:
            v.add("timezone", "Unknown timezone.")

        run_at: datetime | None = None
        cron_expr: str | None = None
        stype = body.schedule_type
        if stype == ScheduleType.ONCE:
            if body.run_at is None:
                run_at = now
            else:
                t = body.run_at if body.run_at.tzinfo else body.run_at.replace(tzinfo=UTC)
                t = t.astimezone(UTC)
                v.check(t >= now - timedelta(minutes=1), "run_at", "Run time must be in the future.")
                v.check(t < now + timedelta(days=5 * 365 + 1), "run_at", "Run time must be within 5 years.")
                run_at = t
        elif stype == ScheduleType.DELAYED:
            if body.delay_seconds is None:
                v.add("delay_seconds", "Delay is required.")
            elif not 1 <= body.delay_seconds <= 365 * 24 * 3600:
                v.add("delay_seconds", "Delay must be between 1 second and 365 days.")
            else:
                run_at = now + timedelta(seconds=body.delay_seconds)
        elif stype == ScheduleType.CRON:
            cron_expr = (body.cron_expr or "").strip()
            try:
                cron.parse(cron_expr, tz)
            except cron.CronError as e:
                v.add("cron_expr", capitalize(str(e)) + ".")
        else:
            v.add("schedule_type", "Schedule type must be once, delayed or cron.")

        missed = body.missed_run_policy or MissedRunPolicy.RUN_ONCE.value
        overlap = body.overlap_policy or OverlapPolicy.SKIP.value
        if missed not in {p.value for p in MissedRunPolicy}:
            v.add("missed_run_policy", "Missed-run policy must be skip, run_once or run_all.")
        if overlap not in {p.value for p in OverlapPolicy}:
            v.add("overlap_policy", "Overlap policy must be skip, queue or allow.")
        conc = _or(body.max_concurrency, 1)
        prio = _or(body.priority, settings.default_priority)
        attempts = _or(body.max_attempts, settings.default_max_attempts)
        base = _or(body.backoff_base_seconds, settings.default_backoff_base_seconds)
        bmax = _or(body.backoff_max_seconds, settings.default_backoff_max_seconds)
        timeout = _or(body.timeout_seconds, settings.default_timeout_seconds)
        v.check(1 <= conc <= 100, "max_concurrency", "Concurrency must be between 1 and 100.")
        v.check(1 <= prio <= 10, "priority", "Priority must be between 1 and 10.")
        v.check(1 <= attempts <= 25, "max_attempts", "Max attempts must be between 1 and 25.")
        v.check(1 <= base <= 3600, "backoff_base_seconds", "Base delay must be between 1 second and 1 hour.")
        v.check(1 <= bmax <= 86400, "backoff_max_seconds", "Max delay must be between 1 second and 24 hours.")
        v.check(bmax >= base, "backoff_max_seconds", "Max delay must be at least the base delay.")
        v.check(1 <= timeout <= 86400, "timeout_seconds", "Timeout must be between 1 second and 24 hours.")
        v.raise_if_any()
        return Job.model_construct(
            name=name, description=body.description, handler=body.handler, payload=payload,
            schedule_type=stype, run_at=run_at, cron_expr=cron_expr, timezone=tz, missed_run_policy=missed,
            overlap_policy=overlap, max_concurrency=conc, priority=prio, max_attempts=attempts,
            backoff_base_seconds=base, backoff_max_seconds=bmax, timeout_seconds=timeout)

    async def create(self, p: Principal, body: JobInput, idem_key: str) -> tuple[Job, bool]:
        """Validates and stores a new job. If idem_key was already used by this
        workspace, the original job is returned with replayed=True (same
        request) or a conflict is raised (different request). The unique index
        on (tenant_id, idempotency_key) makes this safe under concurrent retries."""
        if idem_key and (len(idem_key) > 128 or idem_key.strip() != idem_key):
            raise ValidationFailed({"idempotency_key":
                                    "Idempotency-Key must be 1-128 characters without surrounding whitespace."})
        async with self.pool.connection() as conn:
            settings = await user_store.get_settings(conn, p.tenant_id)
        now = self.now()
        d = self._build(body, settings, now)
        j = Job(**{**d.__dict__, "id": uuid.uuid4(), "tenant_id": p.tenant_id, "created_by": p.user_id,
                   "status": JobStatus.CREATED, "created_at": now, "updated_at": now, "version": 1,
                   "request_hash": request_hash(body), "idempotency_key": idem_key or None})
        j.next_run_at = first_run(j, now)

        f = Effects()
        replayed = False
        async with tx(self.pool) as conn:
            if not await job_store.insert(conn, j):
                existing = await job_store.get_by_idempotency_key(conn, p.tenant_id, idem_key)
                if existing.request_hash != j.request_hash:
                    raise Conflict("idempotency_key_reused",
                                   "this Idempotency-Key was already used with a different request")
                j, replayed = existing, True
            else:
                await self.eng.record_event(conn, Event(tenant_id=j.tenant_id, job_id=j.id, type="job.created",
                                                        severity=Severity.INFO, message=f"Job created by {p.email}"))
                status = await self.eng.sync_job_status(conn, j, f)
                if status != JobStatus.CREATED:
                    await self.eng.record_event(conn, Event(tenant_id=j.tenant_id, job_id=j.id, type="job.status",
                                                            severity=Severity.INFO, message=f"CREATED → {status}"))
                await self.eng.audit(conn, p, "job.create", "job", str(j.id), {"name": j.name, "handler": j.handler})
        await self.eng.flush(f)
        return j, replayed

    async def update(self, p: Principal, job_id: UUID, body: JobInput, expected_version: int) -> Job:
        """Replaces a job's definition. expected_version implements optimistic
        concurrency: concurrent edits fail with a conflict instead of silently
        overwriting each other. Edits only affect future runs; queued runs keep
        the configuration snapshot taken when they were created."""
        async with self.pool.connection() as conn:
            settings = await user_store.get_settings(conn, p.tenant_id)
        f = Effects()
        async with tx(self.pool) as conn:
            now = self.now()
            cur = await job_store.lock_tenant_job(conn, p.tenant_id, job_id)
            if cur.version != expected_version:
                raise Conflict("version_conflict", "the job was modified by someone else; reload and try again")
            summary = await run_store.active_summary(conn, job_id)
            st = sm.job_actions(cur.status, cur.recurring, summary, False)[sm.EDIT]
            if not st["allowed"]:
                raise Conflict("action_not_allowed", st["reason"])
            # One-time jobs keep their run time unless it was changed explicitly.
            if body.schedule_type == ScheduleType.ONCE and body.run_at is None and cur.run_at is not None:
                keep = cur.run_at >= now - timedelta(minutes=1)  # a stale time on an unscheduled job means "run now"
                body = body.model_copy(update={"run_at": cur.run_at if keep else None})
            d = self._build(body, settings, now)
            if cur.recurring != (d.schedule_type == ScheduleType.CRON):
                raise ValidationFailed({"schedule_type": "A job cannot be converted between one-time and recurring. "
                                                         "Create a new job instead."})
            nxt = Job(**{**d.__dict__, "id": cur.id, "tenant_id": cur.tenant_id, "created_by": cur.created_by,
                         "status": cur.status, "paused_at": cur.paused_at, "cancelled_at": cur.cancelled_at,
                         "last_run_at": cur.last_run_at, "last_run_status": cur.last_run_status,
                         "idempotency_key": cur.idempotency_key, "created_at": cur.created_at,
                         "next_run_at": cur.next_run_at, "updated_at": now, "version": expected_version})
            schedule_changed = (nxt.schedule_type != cur.schedule_type or (nxt.cron_expr or "") != (cur.cron_expr or "")
                                or nxt.timezone != cur.timezone or nxt.run_at != cur.run_at)
            if schedule_changed and (cur.next_run_at is not None or not cur.recurring):
                nxt.next_run_at = first_run(nxt, now)
            await job_store.update_definition(conn, nxt, expected_version, now)
            nxt.version = expected_version + 1
            changed = diff_fields(cur, nxt)
            await self.eng.record_event(conn, Event(tenant_id=nxt.tenant_id, job_id=nxt.id, type="job.updated",
                                                    severity=Severity.INFO,
                                                    message=f"Job updated by {p.email}: {', '.join(changed)}"))
            await self.eng.sync_job_status(conn, nxt, f)
            await self.eng.audit(conn, p, "job.update", "job", str(job_id), {"changed": changed})
        await self.eng.flush(f)
        return nxt

    async def get(self, p: Principal, job_id: UUID) -> dict:
        """A job with its runs, attempts, timeline and permitted actions."""
        async with self.pool.connection() as conn:
            j = await job_store.get(conn, p.tenant_id, job_id)
            summary = await run_store.active_summary(conn, job_id)
            replayable = False
            try:
                replayable = (await run_store.last_failed(conn, job_id)).replayed_at is None
            except NotFound:
                pass
            runs, _ = await run_store.list_runs(conn, RunFilter(tenant_id=p.tenant_id, job_id=job_id,
                                                                page=run_store.Page(1, 25)))
            attempts = await attempt_store.for_job(conn, p.tenant_id, job_id, 100)
            events = await event_store.list_events(conn, event_store.EventFilter(tenant_id=p.tenant_id, job_id=job_id,
                                                                                 limit=100))
        upcoming: list[datetime] = []
        if j.recurring and j.next_run_at is not None and j.paused_at is None:
            upcoming = _upcoming(j)
        elif j.next_run_at is not None:
            upcoming = [j.next_run_at]
        h = self.reg.get(j.handler)
        return {"job": j, "actions": sm.job_actions(j.status, j.recurring, summary, replayable),
                "active_runs": summary.active, "upcoming_runs": upcoming, "handler": h.info() if h else None,
                "runs": runs, "attempts": attempts, "events": events}

    async def list(self, p: Principal, f: job_store.JobFilter) -> tuple[list[dict], int]:
        f.tenant_id = p.tenant_id
        async with self.pool.connection() as conn:
            return await job_store.list_jobs(conn, f)

    async def schedules(self, p: Principal) -> list[dict]:
        """Recurring jobs with upcoming and recent runs."""
        async with self.pool.connection() as conn:
            jobs = await job_store.list_schedules(conn, p.tenant_id)
            recent = await run_store.recent_for_jobs(conn, [j.id for j in jobs], 8)
        out = []
        for j in jobs:
            upcoming = _upcoming(j) if j.next_run_at and j.paused_at is None and j.cancelled_at is None else []
            out.append({"job": j, "upcoming_runs": upcoming, "recent_runs": recent.get(j.id, [])})
        return out

    def preview_schedule(self, expr: str, tz: str, count: int) -> list[datetime]:
        """Validates a cron expression and returns upcoming times."""
        if not 1 <= count <= 20:
            count = 5
        try:
            spec = cron.parse(expr, tz)
        except cron.CronError as e:
            field = "cron_expr"
            try:
                cron.load_timezone(tz)
            except cron.CronError:
                field = "timezone"
            raise ValidationFailed({field: capitalize(str(e)) + "."}) from None
        return spec.next_n(self.now(), count)

    async def bulk(self, p: Principal, action: str, ids: list[UUID]) -> list[BulkResult]:
        """Applies an action to several jobs independently. Each job is its own
        transaction, so one failure does not roll back the others."""
        if not 1 <= len(ids) <= 100:
            raise ValidationFailed({"ids": "Select between 1 and 100 jobs."})
        ops = {"pause": self.eng.pause_job, "resume": self.eng.resume_job, "cancel": self.eng.cancel_job,
               "retry": self.eng.retry_job}
        op = ops.get(action)
        if op is None:
            raise ValidationFailed({"action": "Action must be pause, resume, cancel or retry."})
        out: list[BulkResult] = []
        for job_id in dict.fromkeys(ids):
            r = BulkResult(job_id, True)
            try:
                await op(p, job_id)
            except Conflict as e:
                r.ok, r.code, r.message = False, e.code, e.message
            except NotFound:
                r.ok, r.code, r.message = False, "not_found", "Job not found."
            except Exception as e:  # noqa: BLE001 - reported per item
                self.log.error("bulk action failed", action=action, job_id=str(job_id), err=str(e))
                r.ok, r.code, r.message = False, "internal_error", "Unexpected error."
            out.append(r)
        return out

    def handler_supported(self, name: str) -> bool:
        return self.reg.get(name) is not None
