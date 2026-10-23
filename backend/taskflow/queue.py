"""Redis-based dispatch layer.

Redis is an accelerator, not a source of truth. PostgreSQL holds every run's
authoritative status; Redis holds *hints* that a run is ready:

  - ready   (ZSET): run ids ordered by priority, then FIFO. Workers pop from it.
  - delayed (ZSET): run ids deferred because a concurrency limit was hit.
  - pub/sub channels for lifecycle events (SSE fan-out) and cancellation.

Because a worker must still win an atomic claim in PostgreSQL, duplicate or
stale entries are harmless, and entries lost in a Redis restart are
re-published by the scheduler's reconciler.
"""

from __future__ import annotations

import json
import time
from dataclasses import asdict, dataclass
from datetime import datetime
from uuid import UUID

import redis.asyncio as aioredis

# Separates priority bands in the ready score. Unix milliseconds (~1.8e12) fit
# below it, and 11 bands * 1e13 stays under 2^53 so float scores are exact.
PRIORITY_STRIDE = 10**13

_PROMOTE = """
local due = redis.call('ZRANGEBYSCORE', KEYS[1], '-inf', ARGV[1], 'LIMIT', 0, tonumber(ARGV[2]))
for _, m in ipairs(due) do
  local sep = string.find(m, '|', 1, true)
  if sep then
    redis.call('ZADD', KEYS[2], 'NX', string.sub(m, sep + 1), string.sub(m, 1, sep - 1))
  end
  redis.call('ZREM', KEYS[1], m)
end
return #due"""


def score(priority: int, queued_at: datetime) -> int:
    """Encodes priority (1..10, higher first) and enqueue time (FIFO within a
    priority) into one ZSET score; lower scores pop first."""
    priority = min(10, max(1, priority))
    return (10 - priority) * PRIORITY_STRIDE + int(queued_at.timestamp() * 1000)


@dataclass
class Item:
    run_id: UUID
    priority: int
    queued_at: datetime


@dataclass
class QueueEvent:
    """A lifecycle notification fanned out to SSE clients. It carries
    identifiers only; clients re-fetch authoritative state from the API."""

    type: str  # run.updated | job.updated | worker.updated
    tenant_id: str = ""
    job_id: str = ""
    run_id: str = ""
    worker_id: str = ""
    status: str = ""
    at: int = 0

    def to_json(self) -> str:
        return json.dumps({k: v for k, v in asdict(self).items() if v or k == "at"})

    @classmethod
    def parse(cls, payload: str | bytes) -> QueueEvent:
        d = json.loads(payload)
        if not isinstance(d, dict) or not isinstance(d.get("type"), str):
            raise ValueError("malformed event")
        return cls(**{k: d[k] for k in cls.__dataclass_fields__ if k in d})


class Queue:
    """Wraps a Redis client. All keys and channels share a prefix so several
    deployments (or parallel test runs) can share one Redis."""

    def __init__(self, url: str, prefix: str = "taskflow"):
        self.rdb = aioredis.from_url(url, socket_connect_timeout=2, socket_timeout=3, decode_responses=True,
                                     health_check_interval=30)
        self.prefix = prefix
        self.key_ready = f"{prefix}:queue:ready"
        self.key_delayed = f"{prefix}:queue:delayed"
        self.channel_events = f"{prefix}:events"
        self.channel_cancel = f"{prefix}:cancel"
        self._promote = self.rdb.register_script(_PROMOTE)

    async def close(self) -> None:
        await self.rdb.aclose()

    async def ping(self) -> None:
        await self.rdb.ping()

    async def publish(self, *items: Item) -> None:
        """Adds runs to the ready set. ZADD NX keeps the original score, so
        re-publishing is idempotent."""
        if items:
            await self.rdb.zadd(self.key_ready, {str(i.run_id): score(i.priority, i.queued_at) for i in items}, nx=True)

    async def pop(self, wait_seconds: float) -> UUID | None:
        """Blocks up to wait_seconds for the highest-priority ready run."""
        res = await self.rdb.bzpopmin(self.key_ready, timeout=wait_seconds)
        if res is None:
            return None
        _, member, _ = res
        return UUID(member)

    async def defer(self, it: Item, delay_seconds: float) -> None:
        """Parks a run in the delayed set. The member encodes the ready score so
        promotion preserves priority ordering."""
        member = f"{it.run_id}|{score(it.priority, it.queued_at)}"
        await self.rdb.zadd(self.key_delayed, {member: int((time.time() + delay_seconds) * 1000)})

    async def promote_delayed(self, now: datetime, limit: int) -> int:
        return int(await self._promote(keys=[self.key_delayed, self.key_ready],
                                       args=[int(now.timestamp() * 1000), limit]))

    async def depth(self) -> tuple[int, int]:
        async with self.rdb.pipeline(transaction=False) as p:
            p.zcard(self.key_ready)
            p.zcard(self.key_delayed)
            ready, delayed = await p.execute()
        return ready, delayed

    async def remove(self, run_id: UUID) -> None:
        await self.rdb.zrem(self.key_ready, str(run_id))

    async def publish_event(self, e: QueueEvent) -> None:
        if not e.at:
            e.at = int(time.time() * 1000)
        await self.rdb.publish(self.channel_events, e.to_json())

    def pubsub(self):
        return self.rdb.pubsub(ignore_subscribe_messages=True)

    async def publish_cancel(self, run_id: UUID) -> None:
        """Asks whichever worker runs run_id to stop. Best effort: the lease
        loop also observes cancel_requested_at in PostgreSQL."""
        await self.rdb.publish(self.channel_cancel, str(run_id))
