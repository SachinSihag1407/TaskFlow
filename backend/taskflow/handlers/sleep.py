"""test.sleep: a long-running job for exercising timeouts, cancellation,
retries, crash recovery and crashes ("panics"). Failure decisions are
deterministic per (run, attempt), so a given scenario reproduces exactly."""

from __future__ import annotations

import asyncio
import time

from . import Execution, Handler, HandlerError, Payload, Permanent, decode, field, fnv1a64


class SleepPayload(Payload):
    duration_ms: int = 0
    fail_attempts: int = 0
    fail_probability: float = 0.0
    permanent: bool = False
    panic: bool = False


class Sleep(Handler):
    name = "test.sleep"

    def info(self) -> dict:
        return {
            "name": self.name, "title": "Long-running test job",
            "description": "Sleeps for a configurable duration and can simulate failures, permanent errors and panics.",
            "idempotency": "No side effects.",
            "example_payload": {"duration_ms": 5000, "fail_attempts": 1},
            "fields": [
                field("duration_ms", "integer", "How long to run (0-3,600,000 ms)", True),
                field("fail_attempts", "integer", "Fail the first N attempts (0-25)"),
                field("fail_probability", "number", "Deterministic per-attempt failure probability (0-1)"),
                field("permanent", "boolean", "Make simulated failures permanent (no retry)"),
                field("panic", "boolean", "Crash with an unexpected exception instead of a handled error"),
            ],
        }

    def _parse(self, payload) -> SleepPayload:
        p = decode(SleepPayload, payload)
        if not 0 <= p.duration_ms <= 3_600_000:
            raise ValueError("duration_ms must be between 0 and 3,600,000")
        if not 0 <= p.fail_attempts <= 25:
            raise ValueError("fail_attempts must be between 0 and 25")
        if not 0 <= p.fail_probability <= 1:
            raise ValueError("fail_probability must be between 0 and 1")
        return p

    def validate(self, payload) -> None:
        self._parse(payload)

    async def execute(self, x: Execution):
        try:
            p = self._parse(x.payload)
        except ValueError as e:
            raise Permanent(str(e)) from None
        total = p.duration_ms / 1000
        x.log.info(f"sleeping for {total:g}s (attempt {x.attempt}/{x.max_attempts})")
        deadline = time.monotonic() + total
        try:
            while (remaining := deadline - time.monotonic()) > 0:
                await asyncio.sleep(min(remaining, 1.0))
                if total >= 3:
                    done = total - max(0.0, deadline - time.monotonic())
                    x.log.info(f"progress {int(100 * done / total)}%")
        except asyncio.CancelledError:
            x.log.warning("interrupted")
            raise

        fail = x.attempt <= p.fail_attempts
        if not fail and p.fail_probability > 0:
            fail = (fnv1a64(f"{x.run_id}:{x.attempt}".encode()) % 10_000) / 10_000 < p.fail_probability
        if fail:
            if p.panic:
                raise RuntimeError(f"simulated panic on attempt {x.attempt}")
            msg = f"simulated failure on attempt {x.attempt}"
            x.log.error(msg)
            raise Permanent(msg) if p.permanent else HandlerError(msg)
        x.log.info("finished")
        return {"slept_ms": p.duration_ms, "attempt": x.attempt}
