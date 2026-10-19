# Deployment and operations

## Docker Compose (local / single host)

```bash
cp .env.example .env                    # set POSTGRES_PASSWORD and REDIS_PASSWORD
docker compose up -d --build --wait     # waits until every service is healthy
docker compose run --rm seed            # optional demo data
```

| Service | Image | Notes |
|---|---|---|
| `postgres` | `postgres:16-alpine` | volume `pgdata`; port bound to 127.0.0.1 |
| `redis` | `redis:7.4-alpine` | AOF persistence, password required; volume `redisdata` |
| `migrate` | backend image | one-shot; app services start only after it succeeds |
| `api` | backend image | `/readyz` healthcheck; trusts `X-Real-IP` from nginx only |
| `scheduler` | backend image | admin port 9090 (`/healthz`, `/readyz`, `/metrics`) |
| `worker` × 2 | backend image | `stop_grace_period: 45s` > `SHUTDOWN_GRACE` (30 s) |
| `web` | nginx + built SPA | serves the UI and proxies `/api` (SSE unbuffered) on port 8080 |
| `seed` | backend image | profile `tools`; run on demand |

Backend containers run as a non-root user on a distroless base, with a read-only root filesystem, all capabilities dropped and `no-new-privileges`. The IANA timezone database is compiled into the binary.

### Day-2 commands

```bash
docker compose ps                                  # health of every service
docker compose logs -f api scheduler worker        # structured JSON logs
docker compose up -d --scale worker=4              # more workers (any number)
docker compose restart scheduler                   # safe at any time
docker compose stop worker                         # graceful drain (SIGTERM)
docker compose run --rm migrate                    # apply new migrations
docker compose down                                # stop, keep data
docker compose down -v                             # stop and delete data
```

Prometheus scrape targets inside the Compose network: `api:8080/metrics` (set `METRICS_TOKEN` to require a bearer token), `scheduler:9090/metrics`, `worker:9090/metrics`. The API's metrics are also published on the host at `127.0.0.1:8081/metrics`; nginx does not expose them publicly.

## Configuration reference

All configuration comes from environment variables. Durations use Go syntax (`30s`, `5m`).

| Variable | Default | Used by | Purpose |
|---|---|---|---|
| `DATABASE_URL` | — (required) | all | PostgreSQL DSN |
| `REDIS_URL` | `redis://localhost:6379/0` | all | Redis URL (`redis://:password@host:6379/0`) |
| `TASKFLOW_ENV` | `development` | all | `production` enables Secure cookies by default, forbids private webhooks and demo seeding |
| `LOG_LEVEL` | `info` | all | `debug`, `info`, `warn`, `error` |
| `DB_MAX_CONNS` | `20` | all | pool size per process |
| `HTTP_ADDR` | `:8080` | api | listen address |
| `CORS_ALLOWED_ORIGINS` | `http://localhost:5173,http://localhost:3000` | api | extra origins allowed for credentialed requests (same-origin always works) |
| `COOKIE_SECURE` | `true` in production | api | `Secure` flag on the session cookie |
| `SESSION_TTL` | `168h` | api | sliding session lifetime |
| `ALLOW_REGISTRATION` | `true` | api | self-service workspace creation |
| `RATE_LIMIT_REGISTER_PER_HOUR` | `5` | api | registrations per IP per hour |
| `TRUST_PROXY_HEADERS` | `false` | api | honour `X-Real-IP` (only behind a trusted proxy) |
| `MAX_REQUEST_BYTES` | `1048576` | api | request body limit |
| `METRICS_TOKEN` | empty | api | bearer token for `/metrics` |
| `PLATFORM_ADMIN_EMAILS` | empty | api | admins allowed to operate shared infrastructure (drain workers) |
| `ADMIN_ADDR` | `:9090` | scheduler, worker | health and metrics listener |
| `WORKER_CONCURRENCY` | `4` | worker | parallel attempts per worker |
| `WORKER_HEARTBEAT_INTERVAL` | `5s` | worker | heartbeat period |
| `LEASE_DURATION` | `30s` | worker | run lease; renewed every lease/3; must be ≥ 3 × heartbeat |
| `SHUTDOWN_GRACE` | `30s` | worker | time for in-flight attempts on SIGTERM |
| `WORKER_POLL_INTERVAL` | `2s` | worker | PostgreSQL polling interval when Redis is down |
| `WORKER_HANDLERS` | all | worker | comma-separated subset of handlers this worker runs |
| `WEBHOOK_ALLOW_PRIVATE_NETWORKS` | `false` | api, worker | **testing only**; refused in production |
| `WEBHOOK_MAX_RESPONSE_BYTES` | `65536` | worker | webhook response read limit |
| `SCHEDULER_TICK` | `1s` | scheduler | loop period |
| `SCHEDULER_BATCH_SIZE` | `200` | scheduler | due jobs per transaction |
| `RECONCILE_INTERVAL` | `10s` | scheduler | Redis reconciliation period |
| `WORKER_STALE_AFTER` | `20s` | api | heartbeat age shown as "stale" |
| `WORKER_OFFLINE_AFTER` | `2m` | scheduler, api | heartbeat age at which a worker is marked offline |
| `SEED_PASSWORD` | generated | seed | password for demo users |

## Production checklist

- Serve over HTTPS (terminate TLS at a load balancer or in nginx), set `TASKFLOW_ENV=production` and `COOKIE_SECURE=true`.
- Use managed PostgreSQL with backups and point-in-time recovery; Redis needs no backups (it is rebuilt from PostgreSQL), but AOF shortens recovery.
- Supply secrets through your platform's secret store; never bake them into images.
- Set `ALLOW_REGISTRATION=false` for private installations and create users as an admin.
- Protect `/metrics` (`METRICS_TOKEN` or network policy) and keep the admin ports internal.
- Keep hosts NTP-synchronised (leases compare timestamps produced on different machines).
- Run ≥ 2 schedulers for fast failover and ≥ 2 workers; scale the API horizontally behind the proxy.
- Alert on: `taskflow_queue_depth{source="postgres_queued"}` growing, `taskflow_leases_recovered_total` rate, `taskflow_redis_errors_total`, `taskflow_scheduler_is_leader` summing to 0, `/readyz` failures, dead-letter growth.

## Development without Docker

You need Go 1.26+, Node 22+, PostgreSQL 16+ and Redis 7+ (or just `make test-deps` for the two databases in containers).

```bash
# Databases (throwaway containers)
make test-deps
createdb -h localhost -U taskflow taskflow   # or: docker exec tf-test-pg createdb -U taskflow taskflow

# Backend: four terminals (or a process manager)
cd backend
export DATABASE_URL="postgres://taskflow:taskflow@localhost:5432/taskflow?sslmode=disable"
export REDIS_URL="redis://localhost:6379/0"
go run ./cmd/taskflow migrate
SEED_PASSWORD='Demo-Password-2026' go run ./cmd/taskflow seed
go run ./cmd/taskflow api
ADMIN_ADDR=:9091 go run ./cmd/taskflow scheduler
ADMIN_ADDR=:9092 go run ./cmd/taskflow worker

# Frontend (Vite proxies /api to localhost:8080, so cookies behave like production)
cd frontend && npm ci && npm run dev    # http://localhost:5173
```

## Tests

```bash
make test-deps          # PostgreSQL + Redis containers on 5432/6379
make test-integration   # all Go tests with -race (each test gets its own database and Redis prefix)
make test-frontend      # Vitest
make up seed            # full stack for E2E
make e2e                # Playwright
make drills PASSWORD=…  # failure drills (needs curl, jq, docker)
make lint
```

Go integration tests skip themselves when `TASKFLOW_TEST_DATABASE_URL` / `TASKFLOW_TEST_REDIS_URL` are not set, so `go test ./...` works anywhere.

## Environment notes from development

The project was developed and verified on Windows 11 with Go and Docker Engine running inside a WSL2 Ubuntu distribution (Smart App Control on the host blocks freshly built unsigned binaries, so Go tests and the backend ran in WSL). Everything — unit and integration tests with the race detector, the Compose stack, Playwright E2E against both the Vite dev server and the Compose/nginx production build, failure drills and load tests — was executed in that environment. The GitHub Actions workflow mirrors those commands but has not yet run on GitHub.
