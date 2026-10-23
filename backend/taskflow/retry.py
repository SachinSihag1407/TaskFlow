"""Retry delays: exponential backoff with equal jitter."""

from __future__ import annotations

import random
from collections.abc import Callable
from dataclasses import dataclass


@dataclass(frozen=True)
class Policy:
    base: float  # seconds
    max: float   # seconds

    def ceiling(self, failed_attempt: int) -> float:
        """Un-jittered delay: min(max, base * 2^(attempt-1))."""
        failed_attempt = max(1, failed_attempt)
        if self.base <= 0:
            return 0.0
        cap = max(self.max, self.base)
        try:
            return min(cap, self.base * 2 ** (failed_attempt - 1))
        except OverflowError:
            return cap

    def delay(self, failed_attempt: int, rnd: Callable[[], float] | None = None) -> float:
        """Half of the delay is fixed and half random ("equal jitter"). This
        keeps a guaranteed minimum backoff while spreading out retries of runs
        that failed together, e.g. during a downstream outage."""
        c = self.ceiling(failed_attempt)
        return c / 2 + (rnd or random.random)() * c / 2

    def bounds(self, failed_attempt: int) -> tuple[float, float]:
        c = self.ceiling(failed_attempt)
        return c / 2, c
