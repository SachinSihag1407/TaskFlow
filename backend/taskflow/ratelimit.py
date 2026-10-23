"""Fixed-window rate limiting backed by Redis, with an in-process fallback so
that a Redis outage does not disable protection of sensitive endpoints (it
only makes limits per-instance)."""

from __future__ import annotations

import asyncio
import time

_INCR = """
local n = redis.call('INCR', KEYS[1])
if n == 1 then redis.call('PEXPIRE', KEYS[1], ARGV[1]) end
return {n, redis.call('PTTL', KEYS[1])}"""


class Limiter:
    def __init__(self, rdb=None, prefix: str = "taskflow"):
        self.rdb = rdb
        self.prefix = prefix
        self._script = rdb.register_script(_INCR) if rdb is not None else None
        self._local: dict[str, list[float]] = {}  # key -> [count, reset_monotonic]
        self._last_sweep = time.monotonic()

    async def allow(self, key: str, limit: int, window_seconds: float) -> tuple[bool, float]:
        """Records one hit for key; returns (within limit, seconds until reset)."""
        if self._script is not None:
            try:
                n, ttl_ms = await asyncio.wait_for(
                    self._script(keys=[f"{self.prefix}:rl:{key}"], args=[int(window_seconds * 1000)]), 0.3)
                ttl = ttl_ms / 1000 if ttl_ms >= 0 else window_seconds
                return int(n) <= limit, ttl
            except Exception:  # noqa: BLE001 - degrade to the local limiter on any Redis failure
                pass
        return self._allow_local(key, limit, window_seconds)

    def _allow_local(self, key: str, limit: int, window_seconds: float) -> tuple[bool, float]:
        now = time.monotonic()
        if now - self._last_sweep > 60:  # periodic cleanup of expired windows
            self._local = {k: w for k, w in self._local.items() if w[1] > now}
            self._last_sweep = now
        w = self._local.get(key)
        if w is None or now >= w[1]:
            w = [0, now + window_seconds]
            self._local[key] = w
        w[0] += 1
        return w[0] <= limit, w[1] - now
