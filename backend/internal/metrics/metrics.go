// Package metrics defines TaskFlow's Prometheus instruments. Each process
// (api, scheduler, worker) registers the same set; series a process never
// touches simply stay at zero.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

var (
	HTTPRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "taskflow_http_requests_total",
		Help: "HTTP requests by route pattern, method and status class.",
	}, []string{"route", "method", "code"})

	HTTPDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "taskflow_http_request_duration_seconds",
		Help:    "HTTP request latency by route pattern.",
		Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
	}, []string{"route", "method"})

	RunsEnqueued = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "taskflow_runs_enqueued_total",
		Help: "Runs created, by trigger.",
	}, []string{"trigger"})

	OccurrencesSkipped = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "taskflow_occurrences_skipped_total",
		Help: "Cron occurrences not run, by reason (missed_run_policy, overlap).",
	}, []string{"reason"})

	RunsFinished = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "taskflow_runs_finished_total",
		Help: "Runs reaching a terminal state, by handler and status.",
	}, []string{"handler", "status"})

	AttemptsFinished = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "taskflow_attempts_finished_total",
		Help: "Execution attempts finished, by handler and attempt status.",
	}, []string{"handler", "status"})

	AttemptDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "taskflow_attempt_duration_seconds",
		Help:    "Execution attempt duration by handler.",
		Buckets: prometheus.ExponentialBuckets(0.01, 2.5, 12),
	}, []string{"handler"})

	QueueWait = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "taskflow_queue_wait_seconds",
		Help:    "Time from a run becoming QUEUED to an attempt starting.",
		Buckets: prometheus.ExponentialBuckets(0.005, 2.5, 12),
	})

	ClaimResults = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "taskflow_claims_total",
		Help: "Claim attempts by outcome (claimed, stale, deferred, error) and source (redis, db).",
	}, []string{"outcome", "source"})

	LeasesRecovered = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "taskflow_leases_recovered_total",
		Help: "RUNNING runs recovered after their worker stopped renewing the lease.",
	})

	LeaseLost = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "taskflow_worker_lease_lost_total",
		Help: "Attempts whose results were discarded because the worker no longer owned the run.",
	})

	Republished = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "taskflow_reconciler_republished_total",
		Help: "QUEUED runs re-published to Redis by the reconciler.",
	})

	RedisErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "taskflow_redis_errors_total",
		Help: "Redis operation failures by operation.",
	}, []string{"op"})

	WorkerActiveRuns = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "taskflow_worker_active_runs",
		Help: "Runs currently executing in this worker process.",
	})

	WorkerCapacity = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "taskflow_worker_capacity",
		Help: "Configured concurrency of this worker process.",
	})

	QueueDepth = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "taskflow_queue_depth",
		Help: "Queue depth observed by the scheduler, by source (postgres_queued, postgres_retry_wait, redis_ready, redis_delayed).",
	}, []string{"source"})

	SchedulerTick = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "taskflow_scheduler_tick_seconds",
		Help:    "Duration of scheduler leader ticks.",
		Buckets: prometheus.ExponentialBuckets(0.001, 2.5, 10),
	})

	SchedulerLeader = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "taskflow_scheduler_is_leader",
		Help: "1 if this scheduler instance currently holds leadership.",
	})

	SSEClients = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "taskflow_sse_clients",
		Help: "Connected Server-Sent Events clients.",
	})
)

// Registry returns a registry with TaskFlow and Go runtime collectors.
func Registry() *prometheus.Registry {
	r := prometheus.NewRegistry()
	r.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		HTTPRequests, HTTPDuration, RunsEnqueued, OccurrencesSkipped, RunsFinished, AttemptsFinished,
		AttemptDuration, QueueWait, ClaimResults, LeasesRecovered, LeaseLost, Republished, RedisErrors,
		WorkerActiveRuns, WorkerCapacity, QueueDepth, SchedulerTick, SchedulerLeader, SSEClients,
	)
	return r
}
