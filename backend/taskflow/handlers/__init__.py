"""The job-handler plugin interface and its registry.

Adding a job type means subclassing Handler and registering it; the worker
runtime needs no changes. Handlers only ever run predefined code with
validated JSON payloads. TaskFlow never executes shell commands or
user-supplied code.

Error contract for ``execute``:
  - raise HandlerError for a retryable failure,
  - raise Permanent for a failure retrying cannot fix (bad input, 4xx),
  - any other exception is treated like a crash ("panic"): logged with its
    traceback and retried,
  - cancellation (timeout, operator cancel, worker shutdown) arrives as
    asyncio.CancelledError and must not be swallowed.
"""

from __future__ import annotations

from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from typing import Any, Protocol
from uuid import UUID

from pydantic import BaseModel, ConfigDict, ValidationError


class HandlerError(Exception):
    """A retryable failure."""


class Permanent(HandlerError):
    """A failure that retrying cannot fix; the run goes straight to the DLQ."""


class AttemptLog(Protocol):
    """Records attempt logs. Messages are persisted and shown in the UI, so
    handlers must not log secrets or raw sensitive payload fields."""

    def info(self, msg: str) -> None: ...
    def warning(self, msg: str) -> None: ...
    def error(self, msg: str) -> None: ...


@dataclass
class Execution:
    """Everything a handler receives for one attempt."""

    run_id: UUID
    job_id: UUID
    tenant_id: UUID
    attempt_id: UUID
    attempt: int  # 1-based
    max_attempts: int
    payload: Any
    # Progress saved by a previous attempt of this run.
    checkpoint: Any
    # Stable across all attempts of the same run. Handlers with external side
    # effects must pass it downstream (or dedupe on it) so a retried attempt
    # does not repeat the effect.
    idempotency_key: str
    log: AttemptLog
    # Persists progress; raises if the worker no longer owns the run.
    save_checkpoint: Callable[[Any], Awaitable[None]]


class Payload(BaseModel):
    """Base for payload schemas: unknown fields are rejected so typos are
    reported instead of silently ignored, and types are not coerced."""

    model_config = ConfigDict(extra="forbid", strict=True)


def decode(model: type[Payload], payload: Any):
    if payload is None:
        payload = {}
    if not isinstance(payload, dict):
        raise ValueError("invalid payload: payload must be a JSON object")
    try:
        return model.model_validate(payload)
    except ValidationError as e:
        err = e.errors()[0]
        loc = ".".join(str(p) for p in err["loc"])
        msg = "unknown field" if err["type"] == "extra_forbidden" else err["msg"].lower()
        raise ValueError(f"invalid payload: {loc}: {msg}" if loc else f"invalid payload: {msg}") from None


class Handler:
    """A job type."""

    #: Stable identifier stored on jobs, e.g. "report.generate".
    name: str = ""

    def info(self) -> dict:
        """Describes the handler to API clients (the create-job form):
        name, title, description, idempotency, example_payload, fields."""
        raise NotImplementedError

    def validate(self, payload: Any) -> None:
        """Checks a payload at job creation and before execution; raises ValueError."""
        raise NotImplementedError

    async def execute(self, x: Execution) -> Any:
        """Performs one attempt; the returned JSON value is the run's result."""
        raise NotImplementedError


def field(name: str, typ: str, description: str, required: bool = False) -> dict:
    return {"name": name, "type": typ, "required": required, "description": description}


class Registry:
    """The handlers available in a process."""

    def __init__(self, handlers: list[Handler]):
        self._handlers = {h.name: h for h in handlers}

    def get(self, name: str) -> Handler | None:
        return self._handlers.get(name)

    def names(self) -> list[str]:
        return sorted(self._handlers)

    def infos(self) -> list[dict]:
        return [self._handlers[n].info() for n in self.names()]

    def subset(self, names: list[str]) -> Registry:
        """Only the named handlers (ENABLED_HANDLERS / WORKER_HANDLERS)."""
        if not names:
            return self
        unknown = [n for n in names if n not in self._handlers]
        if unknown:
            raise ValueError(f"unknown handler {unknown[0]!r}")
        return Registry([self._handlers[n] for n in names])


def builtin(pool, allow_private_webhooks: bool = False, webhook_max_body: int = 64 << 10) -> Registry:
    """A registry with every built-in handler."""
    from .batch import Batch
    from .notify import Notify
    from .report import Report
    from .sleep import Sleep
    from .webhook import Webhook

    return Registry([Report(), Batch(), Notify(pool), Webhook(allow_private_webhooks, webhook_max_body), Sleep()])


def fnv1a64(data: bytes) -> int:
    h = 0xCBF29CE484222325
    for b in data:
        h = ((h ^ b) * 0x100000001B3) & 0xFFFFFFFFFFFFFFFF
    return h
