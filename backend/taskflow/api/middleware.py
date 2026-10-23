"""The outer ASGI layer applied to every request: request id, security
headers, CORS for configured origins, the Origin check on unsafe methods,
body-size limits, panic recovery, access logs and HTTP metrics.

Written as plain ASGI (not BaseHTTPMiddleware) so streaming responses (SSE)
pass through untouched."""

from __future__ import annotations

import ipaddress
import time
import traceback
import uuid
from urllib.parse import urlsplit

from .. import metrics
from ..config import Config
from ..logs import Log
from .respond import encode, error_body

SAFE_METHODS = {"GET", "HEAD", "OPTIONS"}
_API_CSP = "default-src 'none'; frame-ancestors 'none'"
# Swagger UI: scripts and styles only from the pinned CDN.
DOCS_CSP = ("default-src 'none'; script-src https://cdn.jsdelivr.net 'unsafe-inline'; "
            "style-src https://cdn.jsdelivr.net 'unsafe-inline'; img-src 'self' data: https://cdn.jsdelivr.net; "
            "connect-src 'self'; frame-ancestors 'none'")
_QUIET_ROUTES = {"GET /healthz", "GET /readyz", "GET /metrics"}


def is_api_path(path: str) -> bool:
    return path.startswith("/api/") or path in ("/api", "/metrics", "/healthz", "/readyz")


def _header(scope, name: bytes) -> str:
    for k, v in scope.get("headers", ()):
        if k == name:
            return v.decode("latin-1")
    return ""


def client_ip(scope, trust_proxy_headers: bool) -> str:
    """The caller's address. Forwarded headers are honoured only behind a
    trusted proxy (TRUST_PROXY_HEADERS=true); otherwise clients could spoof
    them to evade rate limits and audit logs."""
    if trust_proxy_headers:
        candidates = [_header(scope, b"x-real-ip").strip()]
        # Platforms such as Render append the client address to
        # X-Forwarded-For. Use the rightmost entry: earlier entries are
        # supplied by the client and can be forged.
        if xff := _header(scope, b"x-forwarded-for"):
            candidates.append(xff.split(",")[-1].strip())
        for c in candidates:
            try:
                return str(ipaddress.ip_address(c))
            except ValueError:
                continue
    client = scope.get("client")
    return client[0] if client else ""


class BaseMiddleware:
    def __init__(self, app, cfg: Config, log: Log):
        self.app = app
        self.cfg = cfg
        self.log = log

    async def _send_json(self, send, status: int, body: dict, extra: list | None = None) -> None:
        payload = encode(body)
        headers = [(b"content-type", b"application/json; charset=utf-8"),
                   (b"content-length", str(len(payload)).encode())] + (extra or [])
        await send({"type": "http.response.start", "status": status, "headers": headers})
        await send({"type": "http.response.body", "body": payload})

    def _origin_allowed(self, scope, origin: str) -> bool:
        """Same host, or an explicitly allowed origin (CSRF defence in depth on
        top of SameSite cookies and the per-session CSRF token)."""
        if origin in self.cfg.allowed_origins:
            return True
        try:
            host = urlsplit(origin).netloc
        except ValueError:
            return False
        return host != "" and host.lower() == _header(scope, b"host").lower()

    async def __call__(self, scope, receive, send):
        if scope["type"] != "http":
            await self.app(scope, receive, send)
            return
        start = time.perf_counter()
        method, path = scope["method"], scope["path"]
        rid = _header(scope, b"x-request-id")
        if not rid or len(rid) > 64 or any(c in rid for c in "\r\n"):
            rid = str(uuid.uuid4())
        state = scope.setdefault("state", {})
        state["request_id"] = rid
        state["client_ip"] = client_ip(scope, self.cfg.trust_proxy_headers)
        info = {"status": 0, "bytes": 0}

        sec = [(b"x-request-id", rid.encode()), (b"x-content-type-options", b"nosniff"),
               (b"x-frame-options", b"DENY"), (b"referrer-policy", b"strict-origin-when-cross-origin"),
               (b"cross-origin-opener-policy", b"same-origin")]
        if is_api_path(path):
            sec.append((b"content-security-policy", (DOCS_CSP if path.startswith("/api/docs") else _API_CSP).encode()))
        if self.cfg.cookie_secure:
            sec.append((b"strict-transport-security", b"max-age=31536000; includeSubDomains"))
        if not path.endswith("/events/stream"):
            sec.append((b"cache-control", b"no-store"))
        origin = _header(scope, b"origin")
        if origin and origin in self.cfg.allowed_origins:
            sec += [(b"access-control-allow-origin", origin.encode()), (b"access-control-allow-credentials", b"true"),
                    (b"vary", b"Origin")]

        async def send_wrapper(message):
            if message["type"] == "http.response.start":
                info["status"] = message["status"]
                present = {k.lower() for k, _ in message.get("headers", [])}
                message = {**message, "headers": list(message.get("headers", []))
                           + [(k, v) for k, v in sec if k not in present]}
            elif message["type"] == "http.response.body":
                info["bytes"] += len(message.get("body", b""))
            await send(message)

        try:
            if origin and origin in self.cfg.allowed_origins and method == "OPTIONS":
                await send_wrapper({"type": "http.response.start", "status": 204, "headers": [
                    (b"access-control-allow-methods", b"GET, POST, PUT, PATCH, DELETE, OPTIONS"),
                    (b"access-control-allow-headers", b"Content-Type, X-CSRF-Token, Idempotency-Key, X-Request-ID"),
                    (b"access-control-max-age", b"600")]})
                await send_wrapper({"type": "http.response.body", "body": b""})
                return
            if method not in SAFE_METHODS and path.startswith("/api/"):
                o = origin
                if not o and (ref := _header(scope, b"referer")):
                    try:
                        u = urlsplit(ref)
                        o = f"{u.scheme}://{u.netloc}"
                    except ValueError:
                        o = "invalid"
                if o and not self._origin_allowed(scope, o):
                    await self._send_wrapped_error(send_wrapper, 403, "origin_rejected", "Cross-origin request rejected.", rid)
                    return
            length = _header(scope, b"content-length")
            if length.isdigit() and int(length) > self.cfg.max_request_bytes:
                await self._send_wrapped_error(send_wrapper, 413, "body_too_large", "Request body is too large.", rid)
                return
            await self.app(scope, receive, send_wrapper)
        except Exception as e:  # noqa: BLE001 - last-resort handler: log once, answer generically
            self.log.error("panic serving request", request_id=rid, err=repr(e), stack=traceback.format_exc()[-3000:])
            if info["status"] == 0:
                await self._send_wrapped_error(send_wrapper, 500, "internal_error", "Something went wrong.", rid)
        finally:
            route = state.get("route") or "unmatched"
            status = info["status"] or 200
            elapsed = time.perf_counter() - start
            metrics.HTTP_REQUESTS.labels(route, method, f"{status // 100}xx").inc()
            metrics.HTTP_DURATION.labels(route, method).observe(elapsed)
            if route not in _QUIET_ROUTES:
                fields = {"request_id": rid, "method": method, "route": route, "status": status,
                          "duration_ms": int(elapsed * 1000), "bytes": info["bytes"], "ip": state.get("client_ip", "")}
                if state.get("user_id"):
                    fields["user_id"] = state["user_id"]
                (self.log.error if status >= 500 else self.log.info)("request", **fields)

    async def _send_wrapped_error(self, send, status: int, code: str, message: str, rid: str) -> None:
        await self._send_json(send, status, error_body(code, message, rid))
