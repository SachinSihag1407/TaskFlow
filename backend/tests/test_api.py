"""HTTP API tests: authentication, CSRF, RBAC, tenant isolation,
idempotency, optimistic concurrency, strict decoding, rate limits, probes
and static serving — exercised through the real ASGI app."""

from __future__ import annotations

import asyncio
import uuid

import httpx

from taskflow.api.sse import Hub, stream
from taskflow.logs import null_logger

PASSWORD = "correct-horse-42"
JOB = {"name": "Nightly", "handler": "test.sleep", "payload": {"duration_ms": 10}, "schedule_type": "cron",
       "cron_expr": "0 2 * * *", "timezone": "UTC"}


def client(app) -> httpx.AsyncClient:
    return httpx.AsyncClient(transport=httpx.ASGITransport(app=app), base_url="http://testserver")


async def register(c: httpx.AsyncClient, email: str | None = None) -> str:
    """Registers a fresh workspace admin; returns the CSRF token."""
    r = await c.post("/api/v1/auth/register", json={"name": "Ada", "email": email or f"{uuid.uuid4().hex[:8]}@example.com",
                                                    "password": PASSWORD})
    assert r.status_code == 201, r.text
    assert "tf_session" in r.cookies
    return r.json()["data"]["csrf_token"]


async def test_register_login_me_logout(app):
    async with client(app) as c:
        csrf = await register(c, "ada@example.com")
        me = (await c.get("/api/v1/auth/me")).json()["data"]
        assert me["user"]["email"] == "ada@example.com" and me["user"]["role"] == "admin"
        assert "password_hash" not in me["user"]
        assert me["platform_admin"] is False
        r = await c.post("/api/v1/auth/register", json={"name": "Ada", "email": "ADA@example.com", "password": PASSWORD})
        assert r.status_code == 422 and "email" in r.json()["error"]["fields"]
        assert (await c.post("/api/v1/auth/logout", headers={"X-CSRF-Token": csrf})).status_code == 204
        assert (await c.get("/api/v1/auth/me")).status_code == 401
    async with client(app) as c:
        bad = await c.post("/api/v1/auth/login", json={"email": "ada@example.com", "password": "wrong-password-1"})
        assert bad.status_code == 401 and bad.json()["error"]["code"] == "invalid_credentials"
        unknown = await c.post("/api/v1/auth/login", json={"email": "nobody@example.com", "password": PASSWORD})
        assert unknown.json()["error"] == {**bad.json()["error"], "request_id": unknown.json()["error"]["request_id"]}
        assert (await c.post("/api/v1/auth/login", json={"email": " Ada@Example.com ", "password": PASSWORD})).status_code == 200


async def test_weak_password_rejected(app):
    async with client(app) as c:
        r = await c.post("/api/v1/auth/register", json={"name": "A", "email": "weak@example.com", "password": "short"})
        assert r.status_code == 422 and r.json()["error"]["fields"]["password"].startswith("Password must be at least")


async def test_csrf_and_origin_are_enforced(app):
    async with client(app) as c:
        csrf = await register(c)
        assert (await c.post("/api/v1/jobs", json=JOB)).json()["error"]["code"] == "csrf_failed"
        assert (await c.post("/api/v1/jobs", json=JOB, headers={"X-CSRF-Token": "nope"})).status_code == 403
        r = await c.post("/api/v1/jobs", json=JOB, headers={"X-CSRF-Token": csrf, "Origin": "https://evil.example"})
        assert r.status_code == 403 and r.json()["error"]["code"] == "origin_rejected"
        ok = await c.post("/api/v1/jobs", json=JOB, headers={"X-CSRF-Token": csrf, "Origin": "http://testserver"})
        assert ok.status_code == 201


async def test_job_lifecycle_contract(app):
    async with client(app) as c:
        csrf = await register(c)
        h = {"X-CSRF-Token": csrf}
        r = await c.post("/api/v1/jobs", json=JOB, headers={**h, "Idempotency-Key": "k1"})
        assert r.status_code == 201
        job = r.json()["data"]
        assert r.headers["location"] == f"/api/v1/jobs/{job['id']}" and job["status"] == "SCHEDULED"
        assert job["next_run_at"].endswith("Z") and "tenant_id" not in job and "request_hash" not in job

        again = await c.post("/api/v1/jobs", json=JOB, headers={**h, "Idempotency-Key": "k1"})
        assert again.status_code == 200 and again.headers["idempotent-replayed"] == "true"
        assert again.json()["data"]["id"] == job["id"]
        reused = await c.post("/api/v1/jobs", json={**JOB, "name": "Other"}, headers={**h, "Idempotency-Key": "k1"})
        assert reused.status_code == 422 and reused.json()["error"]["code"] == "idempotency_key_reused"

        listing = (await c.get("/api/v1/jobs", params={"page_size": 10})).json()
        assert listing["meta"] == {"page": 1, "page_size": 10, "total": 1, "total_pages": 1}
        assert listing["data"][0]["active_runs"] == 0

        detail = (await c.get(f"/api/v1/jobs/{job['id']}")).json()["data"]
        assert detail["actions"]["pause"] == {"allowed": True} and len(detail["upcoming_runs"]) == 5

        url = f"/api/v1/jobs/{job['id']}"
        assert (await c.put(url, json=JOB, headers=h)).status_code == 428
        stale = await c.put(url, json=JOB, headers={**h, "If-Match": '"7"'})
        assert stale.status_code == 409 and stale.json()["error"]["code"] == "version_conflict"
        updated = await c.put(url, json={**JOB, "priority": 9}, headers={**h, "If-Match": '"1"'})
        assert updated.status_code == 200 and updated.json()["data"]["version"] == 2

        paused = await c.post(f"{url}/pause", headers=h)
        assert paused.json()["data"]["status"] == "PAUSED"
        again = await c.post(f"{url}/pause", headers=h)
        assert again.status_code == 409 and again.json()["error"]["code"] == "action_not_allowed"
        run = await c.post(f"{url}/run", headers=h)
        assert run.status_code == 409  # paused jobs cannot be run now


async def test_strict_request_decoding(app):
    async with client(app) as c:
        h = {"X-CSRF-Token": await register(c)}
        bad_cron = await c.post("/api/v1/jobs", json={**JOB, "cron_expr": "61 * * * *"}, headers=h)
        assert bad_cron.status_code == 422 and "cron_expr" in bad_cron.json()["error"]["fields"]
        bad_payload = await c.post("/api/v1/jobs", json={**JOB, "payload": {"duration_ms": 10, "typo": 1}}, headers=h)
        assert bad_payload.json()["error"]["fields"]["payload"].startswith("Invalid payload")
        unknown = await c.post("/api/v1/jobs", json={**JOB, "colour": "red"}, headers=h)
        assert unknown.status_code == 400 and 'unknown field "colour"' in unknown.json()["error"]["message"]
        wrong_type = await c.post("/api/v1/jobs", json={**JOB, "priority": "high"}, headers=h)
        assert wrong_type.status_code == 400 and '"priority" must be an integer' in wrong_type.json()["error"]["message"]
        empty = await c.post("/api/v1/jobs", content=b"", headers={**h, "Content-Type": "application/json"})
        assert empty.json()["error"]["message"] == "Request body is empty."
        text = await c.post("/api/v1/jobs", content=b"x", headers={**h, "Content-Type": "text/plain"})
        assert text.status_code == 415
        huge = await c.post("/api/v1/jobs", content=b"{" + b" " * (2 << 20) + b"}",
                            headers={**h, "Content-Type": "application/json"})
        assert huge.status_code == 413
        assert (await c.get("/api/v1/jobs/not-a-uuid")).status_code == 404


async def test_rbac_and_tenant_isolation(app):
    async with client(app) as admin, client(app) as viewer, client(app) as outsider:
        h = {"X-CSRF-Token": await register(admin)}
        job = (await admin.post("/api/v1/jobs", json=JOB, headers=h)).json()["data"]
        r = await admin.post("/api/v1/users", json={"name": "Vee", "email": "vee@example.com", "password": PASSWORD,
                                                    "role": "viewer"}, headers=h)
        assert r.status_code == 201
        login = await viewer.post("/api/v1/auth/login", json={"email": "vee@example.com", "password": PASSWORD})
        vh = {"X-CSRF-Token": login.json()["data"]["csrf_token"]}
        assert (await viewer.get(f"/api/v1/jobs/{job['id']}")).status_code == 200
        denied = await viewer.post(f"/api/v1/jobs/{job['id']}/pause", headers=vh)
        assert denied.status_code == 403 and denied.json()["error"]["code"] == "forbidden"
        assert (await viewer.get("/api/v1/audit")).status_code == 403

        await register(outsider)
        assert (await outsider.get(f"/api/v1/jobs/{job['id']}")).status_code == 404
        assert (await outsider.get("/api/v1/jobs")).json()["meta"]["total"] == 0

        me = (await admin.get("/api/v1/auth/me")).json()["data"]["user"]
        lockout = await admin.patch(f"/api/v1/users/{me['id']}", json={"role": "viewer"}, headers=h)
        assert lockout.status_code == 409 and lockout.json()["error"]["code"] == "self_lockout"


async def test_worker_drain_requires_platform_admin(app):
    async with client(app) as c:
        h = {"X-CSRF-Token": await register(c)}
        r = await c.post(f"/api/v1/workers/{uuid.uuid4()}/drain", headers=h)
        assert r.status_code == 403
    async with client(app) as root:
        h = {"X-CSRF-Token": await register(root, "root@example.com")}
        assert (await root.get("/api/v1/auth/me")).json()["data"]["platform_admin"] is True
        r = await root.post(f"/api/v1/workers/{uuid.uuid4()}/drain", headers=h)
        assert r.status_code == 409 and r.json()["error"]["code"] == "worker_offline"


async def test_login_is_rate_limited(app):
    async with client(app) as c:
        codes = [(await c.post("/api/v1/auth/login", json={"email": "x@example.com", "password": "wrong-pass-1"})).status_code
                 for _ in range(11)]
        assert codes[:10] == [401] * 10 and codes[10] == 429
        r = await c.post("/api/v1/auth/login", json={"email": "x@example.com", "password": "wrong-pass-1"})
        assert int(r.headers["retry-after"]) >= 1


async def test_probes_health_and_docs(app):
    async with client(app) as c:
        assert (await c.get("/healthz")).json() == {"status": "ok"}
        assert (await c.get("/readyz")).json() == {"status": "ready", "postgres": "up", "redis": "disabled"}
        assert (await c.get("/metrics")).status_code == 200
        assert (await c.get("/api/openapi.json")).json()["info"]["title"] == "TaskFlow API"
        docs = await c.get("/api/docs")
        assert "swagger-ui-dist@5.17.14" in docs.text and "cdn.jsdelivr.net" in docs.headers["content-security-policy"]
        await register(c)
        h = (await c.get("/api/v1/system/health")).json()["data"]
        redis = next(x for x in h["components"] if x["name"] == "redis")
        assert redis["status"] == "disabled" and h["live_updates"] is False
        assert (await c.get("/api/v1/analytics/overview?range=1h")).json()["data"]["range"] == "1h"
        r = await c.get("/api/v1/nope")
        assert r.status_code == 404 and r.json()["error"]["code"] == "not_found"
        assert r.headers["content-security-policy"] == "default-src 'none'; frame-ancestors 'none'"
        assert r.headers["x-request-id"]


async def test_static_spa_serving(app):
    async with client(app) as c:
        index = await c.get("/jobs/123")
        assert index.status_code == 200 and "<title>TaskFlow</title>" in index.text
        assert "script-src 'self'" in index.headers["content-security-policy"]
        asset = await c.get("/assets/app-123.js")
        assert asset.headers["cache-control"] == "public, max-age=31536000, immutable"
        for probe in ("/../secret.txt", "/%2e%2e/secret.txt", "/assets/../../secret.txt"):
            r = await c.get(probe)
            assert "outside" not in r.text
        assert (await c.post("/somewhere")).status_code == 405


async def test_sse_stream_frames():
    hub = Hub(None, null_logger())
    c = hub.add("u", "t")
    gen = stream(hub, c, lambda: asyncio.sleep(0, True))
    assert await gen.__anext__() == b"retry: 5000\n\n"
    assert await gen.__anext__() == b'event: status\ndata: {"live": false}\n\n'
    hub._dispatch('{"type":"run.updated","tenant_id":"other","at":1}')
    hub._dispatch('{"type":"run.updated","tenant_id":"t","run_id":"r","at":1}')
    frame = await asyncio.wait_for(gen.__anext__(), 2)
    assert b'"run_id":"r"' in frame and frame.startswith(b"event: update\n")
    await gen.aclose()
    assert not hub.clients
    for _ in range(10):
        assert hub.add("same-user", "t") is not None
    assert hub.add("same-user", "t") is None  # per-user cap
