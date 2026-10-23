"""Server-Sent Events.

The hub fans lifecycle events from one Redis subscription out to connected
clients. Events carry identifiers only; each client re-fetches whatever it
displays through the normal (authorized) API, so the stream never carries
data a client could not otherwise read. Clients only receive events for their
own workspace (plus tenant-less platform events).
"""

from __future__ import annotations

import asyncio
import json
from collections.abc import AsyncIterator, Awaitable, Callable

from .. import metrics
from ..logs import Log
from ..queue import Queue, QueueEvent

MAX_CLIENTS = 1000
MAX_PER_USER = 10
HEARTBEAT_SECONDS = 15
CLIENT_BUFFER = 64


class Client:
    def __init__(self, user: str, tenant: str):
        self.user = user
        self.tenant = tenant
        self.events: asyncio.Queue[str] = asyncio.Queue(CLIENT_BUFFER)
        self.status: asyncio.Queue[bool] = asyncio.Queue(4)


class Hub:
    def __init__(self, q: Queue | None, log: Log):
        self.q = q
        self.log = log
        self.clients: set[Client] = set()
        self.live = False

    async def run(self) -> None:
        """Subscribes to Redis and dispatches events until cancelled,
        re-subscribing with backoff while Redis is unavailable."""
        if self.q is None:
            return
        backoff = 1.0
        while True:
            ps = self.q.pubsub()
            try:
                await ps.subscribe(self.q.channel_events)
                self._set_live(True)
                backoff = 1.0
                self.log.info("sse hub subscribed to redis events")
                while True:
                    # get_message times out every 5s so an outage becomes
                    # visible (clients are told to fall back to polling).
                    msg = await ps.get_message(ignore_subscribe_messages=True, timeout=5)
                    if msg is None:
                        await asyncio.wait_for(self.q.ping(), 2)
                    elif msg.get("type") == "message":
                        self._dispatch(msg["data"])
            except asyncio.CancelledError:
                raise
            except Exception as e:  # noqa: BLE001
                if self.live:
                    self.log.warning("redis unreachable; live updates degraded", err=str(e))
            finally:
                self._set_live(False)
                try:
                    await ps.aclose()
                except Exception:  # noqa: BLE001
                    pass
            await asyncio.sleep(backoff)
            backoff = min(backoff * 2, 30)

    def _set_live(self, live: bool) -> None:
        if self.live == live:
            return
        self.live = live
        for c in self.clients:
            try:
                c.status.put_nowait(live)
            except asyncio.QueueFull:
                pass

    def _dispatch(self, payload: str) -> None:
        try:
            ev = QueueEvent.parse(payload)
        except (ValueError, TypeError):
            return
        for c in self.clients:
            if ev.tenant_id and ev.tenant_id != c.tenant:
                continue
            try:
                c.events.put_nowait(payload)
            except asyncio.QueueFull:
                # Slow client: drop rather than block everyone; its periodic
                # refetch keeps its view eventually correct.
                pass

    def add(self, user: str, tenant: str) -> Client | None:
        if len(self.clients) >= MAX_CLIENTS or sum(1 for c in self.clients if c.user == user) >= MAX_PER_USER:
            return None
        c = Client(user, tenant)
        self.clients.add(c)
        metrics.SSE_CLIENTS.set(len(self.clients))
        return c

    def remove(self, c: Client) -> None:
        self.clients.discard(c)
        metrics.SSE_CLIENTS.set(len(self.clients))


async def stream(hub: Hub, c: Client, still_valid: Callable[[], Awaitable[bool]]) -> AsyncIterator[bytes]:
    """The event stream body. Ends when the client disconnects (the server
    cancels the generator) or when the session is no longer valid."""
    try:
        # retry: how long EventSource waits before reconnecting.
        yield b"retry: 5000\n\n"
        yield _frame("status", json.dumps({"live": hub.live}))
        loop = asyncio.get_running_loop()
        next_beat = loop.time() + HEARTBEAT_SECONDS
        while True:
            ev_task = asyncio.ensure_future(c.events.get())
            st_task = asyncio.ensure_future(c.status.get())
            done, _ = await asyncio.wait({ev_task, st_task}, timeout=max(0.0, next_beat - loop.time()),
                                         return_when=asyncio.FIRST_COMPLETED)
            for t in (ev_task, st_task):
                if t not in done:
                    t.cancel()
            if ev_task in done:
                yield _frame("update", ev_task.result())
            if st_task in done:
                yield _frame("status", json.dumps({"live": st_task.result()}))
            if loop.time() >= next_beat:
                # Streams outlive the request that authenticated them; end the
                # stream once the session is revoked, expired or the user disabled.
                if not await still_valid():
                    return
                yield b": keepalive\n\n"
                next_beat = loop.time() + HEARTBEAT_SECONDS
    finally:
        hub.remove(c)


def _frame(event: str, data: str) -> bytes:
    return f"event: {event}\ndata: {data}\n\n".encode()
