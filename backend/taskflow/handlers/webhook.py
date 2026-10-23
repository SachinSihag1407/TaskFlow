"""http.webhook: calls a user-configured endpoint through the SSRF guard.

Every attempt of a run sends the same Idempotency-Key header (the run id) so
receivers can deduplicate: a webhook receiver is an external system TaskFlow
cannot roll back, so idempotency has to be cooperative.
"""

from __future__ import annotations

import asyncio
import json
import re
import time
from typing import Any
from urllib.parse import urljoin, urlsplit, urlunsplit

import httpcore

from .. import netguard
from ..logs import redact
from . import Execution, Handler, HandlerError, Payload, Permanent, decode, field

HEADER_NAME = re.compile(r"^[A-Za-z0-9!#$%&'*+.^_|~-]{1,64}$")
FORBIDDEN_HEADERS = {
    "host", "content-length", "transfer-encoding", "connection", "upgrade", "te", "trailer", "keep-alive",
    "proxy-authorization", "proxy-connection", "idempotency-key", "x-taskflow-run-id", "x-taskflow-attempt",
}
METHODS = ("GET", "POST", "PUT", "PATCH", "DELETE")
MAX_REDIRECTS = 3


class WebhookPayload(Payload):
    url: str = ""
    method: str = ""
    headers: dict[str, str] = {}
    body: Any = None
    timeout_seconds: int = 0
    success_status: list[int] = []


def safe_url(raw: str) -> str:
    """Renders a URL for logs without query values or credentials."""
    try:
        u = urlsplit(raw)
        netloc = u.hostname or ""
        if u.port:
            netloc += f":{u.port}"
    except ValueError:
        return "[invalid url]"
    return urlunsplit((u.scheme, netloc, u.path, "…" if u.query else "", ""))


def is_success(code: int, allowed: list[int]) -> bool:
    return code in allowed if allowed else 200 <= code < 300


class Webhook(Handler):
    name = "http.webhook"

    def __init__(self, allow_private: bool = False, max_body_bytes: int = 64 << 10):
        self.allow_private = allow_private
        self.max_body_bytes = max_body_bytes if max_body_bytes > 0 else 64 << 10
        self._pool: httpcore.AsyncConnectionPool | None = None

    def info(self) -> dict:
        return {
            "name": self.name, "title": "HTTP webhook",
            "description": "Sends an HTTP request to a public endpoint. Private, loopback and cloud-metadata "
                           "addresses are blocked.",
            "idempotency": "Sends Idempotency-Key: <run id> on every attempt so the receiver can deduplicate retries.",
            "example_payload": {"url": "https://example.com/hooks/taskflow", "method": "POST",
                                "headers": {"Content-Type": "application/json"}, "body": {"event": "report.ready"},
                                "timeout_seconds": 10},
            "fields": [
                field("url", "string", "Public http(s) URL", True),
                field("method", "string", "GET, POST (default), PUT, PATCH or DELETE"),
                field("headers", "object", "Request headers (max 20); values of credential headers are never logged"),
                field("body", "any", "JSON value (strings are sent verbatim)"),
                field("timeout_seconds", "integer", "Request timeout, 1-120 (default 30)"),
                field("success_status", "integer[]", "Status codes treated as success (default 200-299)"),
            ],
        }

    def _parse(self, payload) -> WebhookPayload:
        p = decode(WebhookPayload, payload)
        try:
            netguard.validate_url(p.url, self.allow_private)
        except (ValueError, netguard.Blocked) as e:
            raise ValueError(f"url: {e}") from None
        p.method = (p.method or "POST").upper()
        if p.method not in METHODS:
            raise ValueError("method must be GET, POST, PUT, PATCH or DELETE")
        if len(p.headers) > 20:
            raise ValueError("at most 20 headers are allowed")
        for k, v in p.headers.items():
            if not HEADER_NAME.match(k):
                raise ValueError(f'header name "{k}" is not valid')
            if k.lower() in FORBIDDEN_HEADERS:
                raise ValueError(f'header "{k}" cannot be set')
            if len(v) > 2048 or "\r" in v or "\n" in v:
                raise ValueError(f'header "{k}" has an invalid value')
        p.timeout_seconds = p.timeout_seconds or 30
        if not 1 <= p.timeout_seconds <= 120:
            raise ValueError("timeout_seconds must be between 1 and 120")
        if p.body is not None and len(json.dumps(p.body)) > 32 << 10:
            raise ValueError("body must be at most 32 KB")
        if any(not 100 <= s <= 599 for s in p.success_status):
            raise ValueError("success_status entries must be valid HTTP status codes")
        return p

    def validate(self, payload) -> None:
        self._parse(payload)

    def _client(self) -> httpcore.AsyncConnectionPool:
        if self._pool is None:
            self._pool = netguard.new_pool(self.allow_private)
        return self._pool

    async def execute(self, x: Execution):
        try:
            p = self._parse(x.payload)
        except ValueError as e:
            raise Permanent(str(e)) from None
        content = b""
        headers: dict[str, str] = {}
        if p.body is not None:
            if isinstance(p.body, str):
                content, headers["Content-Type"] = p.body.encode(), "text/plain; charset=utf-8"  # sent verbatim
            else:
                content, headers["Content-Type"] = json.dumps(p.body).encode(), "application/json"
        headers.update(p.headers)
        headers.update({"User-Agent": "TaskFlow-Webhook/1.0", "Idempotency-Key": x.idempotency_key,
                        "X-TaskFlow-Run-Id": str(x.run_id), "X-TaskFlow-Attempt": str(x.attempt)})

        x.log.info(f"{p.method} {safe_url(p.url)} (timeout {p.timeout_seconds}s)")
        start = time.monotonic()
        try:
            status, body = await asyncio.wait_for(self._send(p.method, p.url, headers, content), p.timeout_seconds)
        except netguard.Blocked as e:
            raise Permanent(f"request blocked: {redact(str(e))}") from None
        except TimeoutError:
            raise HandlerError(f"request failed: timed out after {p.timeout_seconds}s") from None
        except (httpcore.HTTPError, OSError) as e:
            raise HandlerError(f"request failed: {redact(str(e) or type(e).__name__)}") from None
        elapsed_ms = int((time.monotonic() - start) * 1000)
        x.log.info(f"response {status} in {elapsed_ms}ms ({len(body)} bytes read)")

        snippet = body.decode("utf-8", errors="replace")
        if len(snippet) > 2048:
            snippet = snippet[:2048] + "…"
        if is_success(status, p.success_status):
            return {"status_code": status, "duration_ms": elapsed_ms, "response_snippet": snippet}
        msg = f"endpoint returned HTTP {status}"
        if status in (408, 425, 429) or status >= 500:
            raise HandlerError(msg)  # transient
        raise Permanent(msg)  # other 4xx: retrying will not help

    async def _send(self, method: str, url: str, headers: dict[str, str], content: bytes) -> tuple[int, bytes]:
        """Sends the request, following up to MAX_REDIRECTS redirects, each
        re-validated. Reads at most max_body_bytes of the final response."""
        for _ in range(MAX_REDIRECTS + 1):
            async with self._client().stream(method, url, headers=list(headers.items()), content=content) as resp:
                location = next((v.decode("latin-1") for k, v in resp.headers if k.lower() == b"location"), None)
                if resp.status in (301, 302, 303, 307, 308) and location:
                    url = urljoin(url, location)
                    netguard.validate_url(url, self.allow_private)
                    if resp.status == 303 or (resp.status in (301, 302) and method == "POST"):
                        method, content = "GET", b""
                        headers.pop("Content-Type", None)
                    continue
                body = bytearray()
                async for chunk in resp.aiter_stream():
                    body += chunk[: self.max_body_bytes - len(body)]
                    if len(body) >= self.max_body_bytes:
                        break
                return resp.status, bytes(body)
        raise HandlerError(f"stopped after {MAX_REDIRECTS} redirects")
