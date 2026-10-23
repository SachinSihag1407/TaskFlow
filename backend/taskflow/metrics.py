"""Prometheus instruments. Each process (api, scheduler, worker) registers the
same set; series a process never touches simply stay at zero."""

from __future__ import annotations

from prometheus_client import (
    CollectorRegistry,
    Counter,
    Gauge,
    Histogram,
    PlatformCollector,
    ProcessCollector,
    generate_latest,
)


def _exp(start: float, factor: float, count: int) -> list[float]:
    return [start * factor**i for i in range(count)]


REGISTRY = CollectorRegistry(auto_describe=True)
ProcessCollector(registry=REGISTRY)
PlatformCollector(registry=REGISTRY)

HTTP_REQUESTS = Counter("taskflow_http_requests", "HTTP requests by route pattern, method and status class.",
                        ["route", "method", "code"], registry=REGISTRY)
HTTP_DURATION = Histogram("taskflow_http_request_duration_seconds", "HTTP request latency by route pattern.",
                          ["route", "method"], buckets=[.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5], registry=REGISTRY)
RUNS_ENQUEUED = Counter("taskflow_runs_enqueued", "Runs created, by trigger.", ["trigger"], registry=REGISTRY)
OCCURRENCES_SKIPPED = Counter("taskflow_occurrences_skipped",
                              "Cron occurrences not run, by reason (missed_run_policy, overlap).", ["reason"],
                              registry=REGISTRY)
RUNS_FINISHED = Counter("taskflow_runs_finished", "Runs reaching a terminal state, by handler and status.",
                        ["handler", "status"], registry=REGISTRY)
ATTEMPTS_FINISHED = Counter("taskflow_attempts_finished", "Execution attempts finished, by handler and attempt status.",
                            ["handler", "status"], registry=REGISTRY)
ATTEMPT_DURATION = Histogram("taskflow_attempt_duration_seconds", "Execution attempt duration by handler.",
                             ["handler"], buckets=_exp(0.01, 2.5, 12), registry=REGISTRY)
QUEUE_WAIT = Histogram("taskflow_queue_wait_seconds", "Time from a run becoming QUEUED to an attempt starting.",
                       buckets=_exp(0.005, 2.5, 12), registry=REGISTRY)
CLAIM_RESULTS = Counter("taskflow_claims",
                        "Claim attempts by outcome (claimed, stale, deferred, error) and source (redis, db).",
                        ["outcome", "source"], registry=REGISTRY)
LEASES_RECOVERED = Counter("taskflow_leases_recovered",
                           "RUNNING runs recovered after their worker stopped renewing the lease.", registry=REGISTRY)
LEASE_LOST = Counter("taskflow_worker_lease_lost",
                     "Attempts whose results were discarded because the worker no longer owned the run.",
                     registry=REGISTRY)
REPUBLISHED = Counter("taskflow_reconciler_republished", "QUEUED runs re-published to Redis by the reconciler.",
                      registry=REGISTRY)
REDIS_ERRORS = Counter("taskflow_redis_errors", "Redis operation failures by operation.", ["op"], registry=REGISTRY)
WORKER_ACTIVE_RUNS = Gauge("taskflow_worker_active_runs", "Runs currently executing in this worker process.",
                           registry=REGISTRY)
WORKER_CAPACITY = Gauge("taskflow_worker_capacity", "Configured concurrency of this worker process.", registry=REGISTRY)
QUEUE_DEPTH = Gauge("taskflow_queue_depth",
                    "Queue depth observed by the scheduler, by source "
                    "(postgres_queued, postgres_retry_wait, redis_ready, redis_delayed).", ["source"], registry=REGISTRY)
SCHEDULER_TICK = Histogram("taskflow_scheduler_tick_seconds", "Duration of scheduler leader ticks.",
                           buckets=_exp(0.001, 2.5, 10), registry=REGISTRY)
SCHEDULER_LEADER = Gauge("taskflow_scheduler_is_leader", "1 if this scheduler instance currently holds leadership.",
                         registry=REGISTRY)
SSE_CLIENTS = Gauge("taskflow_sse_clients", "Connected Server-Sent Events clients.", registry=REGISTRY)


def exposition() -> bytes:
    return generate_latest(REGISTRY)
