# Interview guide

Short, defensible answers to the questions this project invites, with pointers into the code. Each answer ends with the trade-off an interviewer is likely to probe.

---

### 1. Why PostgreSQL *and* Redis? Why not just one?

**PostgreSQL** holds every fact that must survive a crash: job definitions, runs, attempts, leases, logs, audit. I need transactions spanning several rows (create run + advance schedule + update job status), row locks, `SKIP LOCKED`, and unique/partial indexes — that is what makes claims atomic and scheduling idempotent.

**Redis** makes dispatch fast and cheap: workers block on a priority ZSET (`BZPOPMIN`) instead of N workers polling PostgreSQL; pub/sub pushes lifecycle events to the API for SSE and delivers cancellation signals instantly; it stores shared rate-limit counters.

The key decision is that **Redis is never trusted**. A Redis entry is only a hint that a run *might* be claimable; the claim itself is a PostgreSQL transaction. So duplicate entries fail to claim, stale entries are dropped, and lost entries are re-published by the reconciler (`Engine.reconcile`). With Redis completely down, workers claim directly from PostgreSQL (`Engine.claim_next`) — tested in `test_redis_outage_falls_back_to_postgres` and the `redis` failure drill.

*Trade-off:* PostgreSQL is the throughput ceiling — every claim and completion is a ~10–15 statement transaction. A PostgreSQL-only design (just `SKIP LOCKED` polling) would also work at this scale; Redis buys latency and lower DB load, at the cost of a reconciliation path.

### 2. How do multiple workers coordinate without running the same job twice at the same time?

They never coordinate with each other — they coordinate through PostgreSQL:

1. A worker pops a run id from Redis.
2. `engine.Claim` opens a transaction, locks the **job** row, then the **run** row, checks `status = 'QUEUED'`, checks per-job and per-workspace concurrency limits, inserts an attempt and sets `status = 'RUNNING'`, `current_attempt_id`, `worker_id` and `lease_expires_at`.
3. A second worker trying the same run blocks on the lock, then sees `RUNNING` and drops the entry.

`test_exactly_one_concurrent_claim_wins` fires 10 concurrent claims at one run and asserts exactly one succeeds.

*Follow-up — why lock the job row?* Concurrency limits ("at most 2 runs of this job at once") are a check-then-act across rows; holding the job lock makes the count and the insert atomic. A fixed lock order (job → run) everywhere prevents deadlocks.

### 3. "So you have exactly-once execution?"

No — and the design says so explicitly. Consider: the worker finishes sending a webhook, then dies before committing "succeeded". From the system's point of view the outcome is unknown, so it must either re-run (risk a duplicate) or give up (risk losing work). TaskFlow chooses **at-least-once**.

What it *does* guarantee:
- at most one *healthy* worker owns a run at a time (claims + leases);
- a worker that lost its lease cannot record a result (fencing by `current_attempt_id`, see below);
- each scheduled occurrence is created once; each failed run is replayed at most once;
- job creation is idempotent via `Idempotency-Key`.

And it makes duplicates harmless: every attempt of a run receives the same idempotency key (the run id). The webhook handler sends it as `Idempotency-Key`; the notification handler records deliveries in a table with `UNIQUE(run_id, channel, recipient)` so retries skip already-notified recipients; the batch handler checkpoints; reports are deterministic.

### 4. Why do you need idempotency if you use locks?

Because locks protect *TaskFlow's* state, not the outside world. A lock (or lease) can expire while its holder is still running — GC pause, network partition, overloaded host — and the holder doesn't know. Martin Kleppmann's classic example: a client pauses, its lock expires, another client takes over, the first wakes up and writes anyway. TaskFlow defends its own database with **fencing** (writes conditioned on the current attempt id), but an HTTP call the zombie already made cannot be fenced. Only the receiver can deduplicate, using the idempotency key.

### 5. What happens when a worker crashes mid-job?

The worker held a 30 s lease and renewed it every 10 s. After the crash:

1. Renewals stop; the lease expires.
2. The scheduler's reaper (every 2 s) finds `RUNNING` runs with expired leases, locks job → run, **re-checks** under the lock that the same attempt still owns it and the lease is still expired (it might have just been renewed), then marks the attempt `LOST` ("outcome unknown").
3. The run goes to `RETRY_WAIT` with backoff (or `FAILED`/dead-letter if attempts are exhausted, or `CANCELLED` if someone had requested cancellation).
4. Another worker claims it as a new attempt.

If the "crashed" worker was actually alive (partition), its next lease renewal fails, its handler context is cancelled with cause "lease lost", and any result it tries to report is rejected. Reproduce with `scripts/failure-drills.sh crash`, which `docker kill -s KILL`s the container running a 20 s job.

*Trade-off:* recovery takes up to lease + reaper interval (~32 s). Shorter leases recover faster but renew more often and are more sensitive to pauses and clock skew.

### 6. How are recurring jobs scheduled — and not double-scheduled?

Each cron job stores `next_run_at`. Every second, the leader scheduler:

1. selects due jobs `FOR NO KEY UPDATE SKIP LOCKED` (so concurrent schedulers don't block each other);
2. computes which occurrences to create (missed-run policy: `skip` / `run_once` / `run_all` ≤ 50) and applies the overlap policy (`skip` / `queue` / `allow`);
3. inserts a run per occurrence with `ON CONFLICT (job_id, scheduled_for) DO NOTHING` — the unique index is the real guard;
4. advances `next_run_at` and derives the job status, all in one transaction;
5. after commit, publishes the new runs to Redis.

If the scheduler crashes after commit but before publishing, the reconciler publishes later. If it crashes before commit, nothing happened and the next tick redoes it. If two schedulers race, the unique index makes the second insert a no-op. `test_concurrent_schedulers_do_not_duplicate_occurrences` runs 5 schedulers concurrently over 4 due occurrences and expects exactly 4 runs.

Leadership (a PostgreSQL session advisory lock) only avoids wasted work; correctness doesn't depend on it.

### 7. Timezones and daylight saving?

Timestamps are UTC everywhere; cron fields are evaluated in the job's IANA zone. For DST I chose cronie's semantics, implemented in `backend/taskflow/cron.py` (a small, readable evaluator written for this project):

- fixed-hour schedules use wall-clock time: `30 2 * * *` in New York runs at 03:30 on the spring-forward day (02:30 doesn't exist) and only once on the fall-back day (01:30 happens twice);
- schedules that fire every hour follow real elapsed time.

The cron library's default would have *skipped* the spring-forward run and *doubled* the fall-back run — I found that by probing it before writing the tests (`test_dst_spring_forward_fixed_time_shifts_forward`, `test_dst_fall_back_fixed_time_runs_once`).

### 8. How do retries work?

Delay after failed attempt *n* is `min(max, base · 2ⁿ⁻¹)` with equal jitter (half fixed, half random). Jitter spreads out retries from runs that failed together (e.g. a downstream outage), avoiding a thundering herd; the fixed half guarantees a minimum backoff. Permanent errors (bad payload, HTTP 4xx) skip retries. After the last attempt the run is dead-lettered with its full attempt history and can be replayed once, with confirmation and an audit record. Worker shutdown interruptions retry after 1 s — a deploy shouldn't cost a full backoff.

### 9. How do cancellation, completion and retries interact when they race?

Every transition is a conditional update under the job → run locks, so exactly one wins:
- cancelling a queued run → `CANCELLED` immediately, and its Redis entry becomes stale;
- cancelling a running run sets `cancel_requested_at`; the worker learns via pub/sub (instant) or lease renewal (≤ 10 s) and cancels the handler's context;
- if the handler finished successfully before noticing, the run is `SUCCEEDED` — the work really happened, so recording "cancelled" would be a lie;
- replaying the same dead-lettered run concurrently: one succeeds, the rest get `409` (conditional update + unique `replay_of`).

### 10. How does the UI stay live without leaking data?

Workers publish `{type, tenant_id, job_id, run_id, status}` to Redis. Each API process has one subscription and forwards events over SSE only to browsers in that workspace. Events carry ids, not data; the browser invalidates React Query caches and re-fetches through authorised endpoints, so a missed or extra event can't show wrong data. If Redis is down the API tells clients and they poll every 5 s; the top bar shows Live / Polling / Offline.

### 11. How do you protect the webhook feature from SSRF?

URL checks at save time (scheme, no credentials, port policy, no `localhost`/`*.internal`/literal private IPs), plus a custom dialer that validates the **actual IP being connected to**, after DNS resolution, on every connection and redirect. That defeats DNS rebinding (a hostname that resolves to a public IP at validation time and `169.254.169.254` at request time). Proxy env vars are ignored so a proxy can't make the request on our behalf. Responses are size-limited and secrets in headers are never logged.

### 12. How did you verify it, beyond "it compiles"?

- Integration tests against real PostgreSQL and Redis for the concurrency claims (concurrent claims, schedulers, cancel/complete races, replay races).
- Worker tests that run a real asyncio worker through crashes, Redis outages, timeouts, handler crashes, shutdown and drain.
- Playwright E2E against both the dev server and the Docker Compose production build, including responsive checks at 375/768/1366 px.
- Failure drills against the container stack (SIGKILL a busy worker, stop Redis, restart the scheduler).
- A load test that measures from stored execution records.

Several real bugs came out of this (listed in [failure-scenarios.md](failure-scenarios.md#found-by-testing)) — the best story is the load test: utilisation was only 33% while the queue was full. Sampling the pipeline showed a few runs stuck in `QUEUED` until the reconciler rescued them. Cause: the claim used `SKIP LOCKED` on the run row, and a post-publish bookkeeping UPDATE briefly locked it, so the claim misread a claimable run as stale. Switching to a blocking lock (safe because the job lock already serialises claims) raised throughput from 37.8 to 57.2 jobs/s with identical parameters. Porting the backend to Python produced a second story of the same kind: one worker reached 27.5 jobs/s and two only 31.9, because each worker claimed runs through a single sequential loop and a claim takes ~30 ms under load. One claim loop per execution slot (same concurrency bound, now pinned by a test) took two workers to 50.9 jobs/s.

### 13. What would you change for 100× scale?

- **PostgreSQL first:** fewer statements per claim/complete (batch claims, combine status derivation), partition `job_runs`/`job_attempts` by time, archive old history, read replicas for dashboards, PgBouncer.
- **Dispatch:** per-handler and per-tenant queues with weighted fairness; batch pops.
- **Leases on the database clock** to remove skew sensitivity.
- **Scheduling:** shard due-job scanning by hash when one leader's tick can't keep up.
- **Observability:** OpenTelemetry traces from API → queue → worker.

### 14. What trade-offs remain that you'd call out yourself?

- At-least-once means external effects can repeat without receiver cooperation.
- A single PostgreSQL primary is both a SPOF and the throughput bound.
- Crash recovery latency is bounded by the lease (~30 s).
- Application-clock leases assume NTP.
- One global ready queue causes churn when workers support different handler subsets.
- Payload secrets are not encrypted at rest by TaskFlow.

### Where to look in the code

| Topic | File |
|---|---|
| Transition table, status derivation, action rules | `backend/taskflow/statemachine.py` |
| Claim, complete, fencing; scheduling, retries, lease recovery, reconcile; cancel / pause / replay / run-now | `backend/taskflow/engine.py` |
| Cron, timezones, DST, missed runs | `backend/taskflow/cron.py` |
| Worker runtime (leases, cancellation, shutdown) | `backend/taskflow/worker.py` |
| Scheduler leadership and loops | `backend/taskflow/scheduler.py` |
| Redis queue and Lua promotion | `backend/taskflow/queue.py` |
| SSRF guard | `backend/taskflow/netguard.py` |
| Schema | `backend/taskflow/migrations/0001_init.sql` |
| Concurrency tests | `backend/tests/test_engine.py`, `backend/tests/test_worker.py` |
