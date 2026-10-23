# Failure scenarios

What happens when each component fails, which guarantee holds, and how to reproduce it. "Tested" names the automated test; "Drill" refers to `scripts/failure-drills.sh`.

## Summary

| Failure | Effect on correctness | Effect on latency | Recovery |
|---|---|---|---|
| Worker killed mid-attempt | Run is re-executed (at-least-once) | Up to lease (30 s) + reaper interval (2 s) | Automatic |
| Worker partitioned but alive | Late result rejected by fencing | Same as above | Automatic |
| Worker SIGTERM (deploy) | None | In-flight work finishes or is retried after ~1 s | Automatic |
| Redis down | None | Workers poll PostgreSQL every 2 s; SSE paused (UI polls) | Automatic on return |
| Redis data lost | None | Lost entries re-published within 10 s | Reconciler |
| Scheduler crash | None | Another replica takes over on its next tick; a lone scheduler resumes on restart | Automatic |
| Scheduler restart during scheduling | No duplicate occurrences | Missed occurrences follow the job's policy | Automatic |
| PostgreSQL down | No progress (by design) | Everything waits | Processes retry; workers retry completions for one lease duration |
| API down | Execution continues | UI unavailable | Restart |

## Worker crashes (SIGKILL, OOM, power loss)

**Sequence.** Worker claims run *R* (attempt *A1*, lease until *t+30s*), starts executing, dies. No more lease renewals. After the lease expires, the scheduler's reaper locks job → run, re-checks that *A1* still owns *R* and the lease is still expired, marks *A1* `LOST` ("outcome unknown") and moves *R* to `RETRY_WAIT` with backoff (or `FAILED` if attempts are exhausted, or `CANCELLED` if cancellation was pending). Another worker claims *R* as attempt *A2*.

**Duplicate window.** If the work had actually completed (e.g. a webhook was delivered) before the crash, *A2* repeats it. That is why handlers are idempotent and why the webhook handler sends the same `Idempotency-Key` on every attempt.

- Tested: `test_expired_lease_is_recovered_and_retried`, `test_lease_recovery_dead_letters_when_attempts_are_exhausted` (engine), `test_crashed_workers_run_is_recovered_by_another_worker` (worker).
- Drill: `failure-drills.sh crash` kills the container running a 20 s job with `docker kill -s KILL` and asserts the run ends `SUCCEEDED` with attempt history `LOST → SUCCEEDED` on another worker.

## Slow or partitioned worker (the "zombie")

A worker pauses (GC, CPU starvation, network partition) longer than its lease. The reaper recovers the run and another worker starts a new attempt. When the first worker wakes up:

- its lease renewal returns "not found" → its handler task is cancelled with cause *lease lost*;
- if it still tries to report an outcome, `Complete` checks `current_attempt_id` under lock and rejects the write (`LeaseLost`), counted in `taskflow_worker_lease_lost_total`;
- checkpoints are fenced the same way.

The zombie can still have performed external side effects before noticing — again covered by idempotency. Tested: the second half of `test_expired_lease_is_recovered_and_retried`.

## Graceful shutdown

On SIGTERM a worker stops claiming, reports `DRAINING`, waits up to `SHUTDOWN_GRACE` (30 s) for in-flight attempts, then cancels the remainder with cause *shutdown*. Those attempts are recorded `FAILED (interrupted)` and their runs return to the queue after 1 s (not after a full backoff — a deploy is not the job's fault). Finally the worker reports `OFFLINE`. Compose sets `stop_grace_period: 45s` so Docker does not SIGKILL first. Operators can trigger the same path from the UI ("Drain worker").

Tested: `test_shutdown_interrupts_and_retries_promptly`, `test_operator_drain_stops_the_worker`.

## Redis unavailable

- **Dispatch:** a worker's `BZPOPMIN` fails → it logs once, switches to `ClaimNext` every `WORKER_POLL_INTERVAL` (reads claimable runs from PostgreSQL in priority order; each candidate is verified by the normal claim transaction), and probes Redis every 5 s.
- **Publication:** the engine's post-commit publish fails → logged and counted; the run stays `QUEUED` with `published_at` NULL; workers find it through PostgreSQL; the reconciler publishes it when Redis returns.
- **Live updates:** the API's SSE hub notices via a periodic ping, tells clients `live: false`, and the UI shows "Polling" and refreshes every 5 s.
- **Rate limiting:** falls back to in-process counters (per API instance).
- **Health:** Redis shows `degraded`.

Tested: `test_redis_outage_falls_back_to_postgres`. Drill: `failure-drills.sh redis` stops the Redis container, runs a job to completion, restarts Redis and verifies queue dispatch resumes.

## Redis restarted without its data

Entries in the ready/delayed sets are gone, but each run is still `QUEUED` in PostgreSQL. The reconciler re-publishes QUEUED runs that were never published or have been queued longer than the reconcile interval, using `ZADD NX` (idempotent). Tested: `test_redis_publication_and_reconcile`.

A related window: a worker pops a run id and crashes before claiming. The entry is gone from Redis and the run is still `QUEUED` — the same reconciler path recovers it.

## Duplicate or stale queue entries

Redis may contain a run id twice (reconciler + original publish) or an id whose run was cancelled or already claimed. The PostgreSQL claim (`status = 'QUEUED'` under lock) fails for all but one claimer; the others drop the entry. Tested: `test_exactly_one_concurrent_claim_wins` (10 concurrent claimers, exactly one succeeds).

## Scheduler crash or restart

- Scheduling transactions are atomic: either the runs were inserted *and* `next_run_at` advanced, or neither.
- If two scheduler instances (or a restarted one) process the same occurrence, the unique index `(job_id, scheduled_for)` rejects the second insert (`ON CONFLICT DO NOTHING`).
- Leadership is a session advisory lock; if the leader's process or connection dies, PostgreSQL releases it and a standby acquires it on its next tick.
- Occurrences that became due while no scheduler ran are handled by the job's missed-run policy.

Tested: `test_concurrent_schedulers_do_not_duplicate_occurrences` (5 concurrent schedulers over 4 due occurrences → exactly 4 runs), `test_plan_cron_policies` and `test_long_outage_terminates` (missed-run policies after downtime). Drill: `failure-drills.sh restart`.

## Partial failure between commit and side effects

| Window | Outcome |
|---|---|
| Run committed, Redis publish not done | Reconciler / PostgreSQL fallback |
| Cancellation committed, cancel signal not sent | Worker sees `cancel_requested_at` on its next lease renewal (≤ 10 s) |
| Attempt finished, worker can't reach PostgreSQL to report | Worker retries `Complete` for one lease duration; afterwards the lease expires and the run is retried (at-least-once) |
| Job created, HTTP response lost | Client retries with the same `Idempotency-Key` and gets the same job |

## Concurrent operator actions

| Race | Result |
|---|---|
| Cancel vs. completion of a running run | Whichever commits first wins; if the handler finished successfully before observing the cancellation, the run is `SUCCEEDED` (the work happened). Never both. Tested: `test_completion_beats_late_cancellation`, `test_cancel_running_run_is_cooperative`. |
| Two operators replay the same dead-lettered run | One succeeds; the other gets `409 already_replayed` (conditional update + unique `replay_of`). Tested: `test_dead_letter_replay_happens_once` (5 concurrent). |
| Two edits of the same job | Second gets `409 version_conflict` (optimistic locking via `If-Match`). |
| Two admins demote each other (last admin) | Serialised per workspace; the workspace always keeps one active admin. |
| Pause while runs are queued | Queued runs are held (claim returns "paused"); resume re-publishes them. Tested: `test_cancel_pause_resume`. |

## PostgreSQL unavailable

TaskFlow deliberately stops making progress: without the source of truth, no claim or transition can be made safely. Processes keep retrying connections; workers that finish an attempt retry reporting for one lease duration. Running handlers keep running; when lease renewals fail persistently, the reaper (once PostgreSQL returns) recovers their runs. `/readyz` reports unavailable so orchestrators stop routing traffic.

## Clock skew

Lease expiry times are computed by workers and compared by the scheduler, so a scheduler clock running ahead of a worker's could recover a healthy run early (the worker would then be fenced out and the run re-executed). The 30 s lease and 10 s renewal interval tolerate ordinary NTP-synchronised skew. Moving lease arithmetic into SQL (`now()`) is a planned improvement.

## Found by testing

Real defects uncovered while building and verifying TaskFlow — kept here because how they were found matters as much as the fix:

1. **Claims skipped momentarily locked runs** (found by the load test). After publishing a run to Redis, the engine records `published_at` with an UPDATE that briefly row-locks the run. A fast worker could pop the id in that window; the claim used `FOR UPDATE SKIP LOCKED`, treated the locked row as stale and dropped the queue entry. Correctness held (the reconciler re-published it ~10 s later), but tail latency suffered. Fix: block on the run lock (the job lock already serialises claims). With identical load-test parameters, throughput rose from 37.8 to 57.2 jobs/s. Regression test: `test_claim_waits_for_briefly_locked_run_instead_of_dropping_it`. (Numbers are from the original Go implementation, where this was found; the fix carried over to the Python port.)
2. **Retention query failed** (found by running the stack). `$1 - make_interval(...)` made PostgreSQL infer `$1` as an interval. Fix: explicit `::timestamptz`. Test: `test_retention_deletes_old_events_but_keeps_audit`.
3. **Login rejected behind nginx on non-default ports** (found by E2E against Docker Compose). nginx's `$host` drops the port, so the API's Origin check saw a host mismatch. Fix: forward `$http_host`.
4. **Fonts blocked by CSP in production** (found by the visual audit against the production bundle). Vite inlined small font files as `data:` URIs, which `font-src 'self'` correctly blocks. Fix: disable asset inlining rather than weaken the CSP.
5. **Dialogs off-centre after their animation** (found by the responsive E2E test). The centring transform lived only in the animation keyframes.
6. **502s after the API container was recreated** (found by re-running E2E after a rebuild). nginx resolved `api` once at startup and kept proxying to the old container IP. Fix: Docker DNS resolver with `server api:8080 resolve` so the upstream is re-resolved.
7. **Independent code review findings** (fixed): any self-registered workspace admin could drain shared workers (now platform-admin only, test `test_worker_drain_requires_platform_admin`); a handler ignoring cancellation kept its lease alive forever (renewal now stops one lease after cancellation so the reaper reclaims the run); SSE streams had no per-user cap and outlived logout (10 per user, session re-checked every 15 s); `mark_published` could deadlock with job cancellation (now locks in id order with `SKIP LOCKED`); cancelling the only queued run of a one-time job left it looking `CREATED` (cancellation now records the last outcome); the per-account login limit could be bypassed by changing email case; one unschedulable job could stall a whole scheduling batch (each job now runs in a savepoint).
8. **Worker claim loop was the throughput ceiling** (found by load-testing the Python port). Each worker claimed runs through one sequential loop, so with claims taking ~30 ms under load a 4-slot worker could not keep its slots busy: one worker reached 27.5 jobs/s and two only 31.9. Fix: one claim loop per execution slot, so claims overlap while the in-flight bound stays exactly `WORKER_CONCURRENCY`. Two-worker throughput rose to 50.9 jobs/s with identical parameters. Test: `test_worker_never_exceeds_its_concurrency`.
9. **Healthchecks cost a CPU core every 10 s** (found while profiling the same load test). The container healthcheck ran `python -m taskflow healthcheck`, which imported the whole web stack each time. Fix: the healthcheck path imports only the standard library.
