"""SSRF guard, handler payload validation and the route contract."""

from __future__ import annotations

import uuid

import pytest

from taskflow import netguard
from taskflow.handlers import Execution, Permanent, builtin
from taskflow.handlers.webhook import Webhook, safe_url


@pytest.mark.parametrize("ip", ["127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254", "100.64.0.1",
                                "0.0.0.0", "::1", "fe80::1", "fc00::1", "::ffff:127.0.0.1", "64:ff9b::a00:1",
                                "2002:7f00:1::", "224.0.0.1", "not-an-ip"])
def test_non_public_addresses_are_blocked(ip):
    assert not netguard.is_public(ip)


@pytest.mark.parametrize("ip", ["93.184.215.14", "1.1.1.1", "2606:4700:4700::1111"])
def test_public_addresses_are_allowed(ip):
    assert netguard.is_public(ip)


@pytest.mark.parametrize("url,msg", [
    ("ftp://example.com/", "scheme"), ("https://user:pw@example.com/", "credentials"), ("https:///x", "host"),
    ("https://example.com:22/", "port"), ("http://localhost:8080/", "not allowed"),
    ("http://metadata.google.internal/", "not allowed"), ("http://169.254.169.254/latest/", "not allowed"),
    ("http://[::1]:8080/", "not allowed"), ("https://example.com/" + "a" * 2100, "too long"),
])
def test_validate_url_rejects(url, msg):
    with pytest.raises((ValueError, netguard.Blocked), match=msg):
        netguard.validate_url(url)


def test_validate_url_accepts_public_https():
    netguard.validate_url("https://example.com:8443/hooks?x=1")


class _Log:
    def __init__(self):
        self.lines = []

    def info(self, m): self.lines.append(m)
    def warning(self, m): self.lines.append(m)
    def error(self, m): self.lines.append(m)


async def _noop(_):
    pass


async def test_webhook_blocks_hostnames_resolving_to_private_ips(monkeypatch):
    """A public-looking hostname that resolves to 127.0.0.1 (DNS rebinding):
    the dial-time check must catch it even though the URL passed the static checks."""

    async def rebound(host, port):
        return ["127.0.0.1"]

    monkeypatch.setattr(netguard, "resolve", rebound)
    h = Webhook(allow_private=False)
    h.validate({"url": "http://localtest.me:8080/"})
    x = Execution(run_id=uuid.uuid4(), job_id=uuid.uuid4(), tenant_id=uuid.uuid4(), attempt_id=uuid.uuid4(), attempt=1,
                  max_attempts=1, payload={"url": "http://localtest.me:8080/", "timeout_seconds": 5}, checkpoint=None,
                  idempotency_key="k", log=_Log(), save_checkpoint=_noop)
    with pytest.raises(Permanent, match="blocked"):
        await h.execute(x)


def test_safe_url_hides_credentials_and_query():
    assert safe_url("https://u:p@example.com/a?token=secret") == "https://example.com/a?…"


@pytest.mark.parametrize("handler,payload,msg", [
    ("test.sleep", {"duration_ms": -1}, "duration_ms"),
    ("test.sleep", {"duration_ms": "10"}, "invalid payload"),
    ("report.generate", {"report_type": "x", "period_days": 1}, "report_type"),
    ("batch.process", {"total_records": 10, "fail_at_record": 11}, "fail_at_record"),
    ("notification.send", {"channel": "sms", "recipients": ["12345"], "subject": "s"}, "recipient 1"),
    ("notification.send", {"channel": "email", "recipients": ["a@example.com", "a@example.com"], "subject": "s"}, "duplicate"),
    ("http.webhook", {"url": "https://example.com/", "headers": {"Host": "evil"}}, "cannot be set"),
    ("http.webhook", {"url": "https://example.com/", "headers": {"X-A": "a\r\nb"}}, "invalid value"),
])
def test_payload_validation(handler, payload, msg):
    with pytest.raises(ValueError, match=msg):
        builtin(None).get(handler).validate(payload)


async def test_report_is_deterministic():
    h = builtin(None).get("report.generate")

    def x():
        return Execution(run_id=uuid.uuid4(), job_id=uuid.uuid4(), tenant_id=uuid.uuid4(), attempt_id=uuid.uuid4(),
                         attempt=1, max_attempts=1, payload={"report_type": "inventory", "period_days": 12, "seed": 7},
                         checkpoint=None, idempotency_key="k", log=_Log(), save_checkpoint=_noop)

    a, b = await h.execute(x()), await h.execute(x())
    assert a == b and a["rows"] == 60 and len(a["checksum"]) == 16


# Every route the frontend and docs rely on (the original Go API surface).
ROUTES = {
    "GET /healthz", "GET /readyz", "GET /metrics", "GET /api/docs", "GET /api/v1/auth/config",
    "POST /api/v1/auth/register", "POST /api/v1/auth/login", "POST /api/v1/auth/logout", "GET /api/v1/auth/me",
    "POST /api/v1/auth/password", "GET /api/v1/handlers", "GET /api/v1/jobs", "POST /api/v1/jobs",
    "POST /api/v1/jobs/bulk", "GET /api/v1/jobs/{job_id}", "PUT /api/v1/jobs/{job_id}", "GET /api/v1/jobs/{job_id}/runs",
    "POST /api/v1/jobs/{job_id}/pause", "POST /api/v1/jobs/{job_id}/resume", "POST /api/v1/jobs/{job_id}/cancel",
    "POST /api/v1/jobs/{job_id}/retry", "POST /api/v1/jobs/{job_id}/run", "GET /api/v1/schedules",
    "POST /api/v1/schedules/preview", "GET /api/v1/runs/{run_id}", "POST /api/v1/runs/{run_id}/cancel",
    "POST /api/v1/runs/{run_id}/replay", "GET /api/v1/dead-letter", "POST /api/v1/dead-letter/replay",
    "GET /api/v1/executions", "GET /api/v1/executions/{attempt_id}", "GET /api/v1/executions/{attempt_id}/logs",
    "GET /api/v1/workers", "GET /api/v1/workers/{worker_id}", "POST /api/v1/workers/{worker_id}/drain",
    "GET /api/v1/analytics/overview", "GET /api/v1/system/health", "GET /api/v1/events", "GET /api/v1/events/stream",
    "GET /api/v1/notifications", "POST /api/v1/notifications/seen", "GET /api/v1/workspace", "GET /api/v1/settings",
    "PUT /api/v1/settings", "GET /api/v1/audit", "GET /api/v1/users", "POST /api/v1/users",
    "PATCH /api/v1/users/{user_id}",
}


def test_route_contract(app):
    have = {f"{m} {r.path}" for r in app.routes for m in getattr(r, "methods", ()) if m != "HEAD"}
    assert ROUTES - have == set()
