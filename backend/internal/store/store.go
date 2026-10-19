// Package store is TaskFlow's PostgreSQL repository layer. Functions take a
// db.DBTX so callers decide transaction boundaries; the engine and services
// compose them into atomic operations. All queries are parameterised.
package store

import (
	"github.com/jackc/pgx/v5"
)

const jobColumns = `j.id, j.tenant_id, j.created_by, j.name, j.description, j.handler, j.payload,
	j.schedule_type, j.run_at, j.cron_expr, j.timezone, j.missed_run_policy, j.overlap_policy,
	j.max_concurrency, j.priority, j.max_attempts, j.backoff_base_seconds, j.backoff_max_seconds,
	j.timeout_seconds, j.status, j.next_run_at, j.last_run_at, j.last_run_status, j.paused_at,
	j.cancelled_at, j.idempotency_key, j.request_hash, j.version, j.created_at, j.updated_at`

const runColumns = `r.id, r.job_id, r.tenant_id, r.scheduled_for, r.trigger, r.status, r.priority,
	r.handler, r.payload, r.timeout_seconds, r.max_attempts, r.backoff_base_seconds,
	r.backoff_max_seconds, r.attempt, r.current_attempt_id, r.worker_id, r.lease_expires_at,
	r.queued_at, r.started_at, r.finished_at, r.next_attempt_at, r.cancel_requested_at,
	r.last_error, r.result, r.checkpoint, r.dead_lettered_at, r.replayed_at, r.replay_of,
	r.created_at, r.updated_at`

const attemptColumns = `a.id, a.run_id, a.job_id, a.tenant_id, a.attempt_number, a.worker_id, a.handler,
	a.status, a.queued_at, a.started_at, a.finished_at, a.duration_ms, a.error_kind,
	a.error_message, a.retryable, a.next_retry_at`

const workerColumns = `w.id, w.hostname, w.pid, w.version, w.concurrency, w.handlers, w.status,
	w.active_runs, w.completed_count, w.failed_count, w.heap_alloc_bytes, w.goroutines,
	w.drain_requested_at, w.started_at, w.last_heartbeat_at, w.stopped_at`

// collect scans every row with fn and returns the results.
func collect[T any](rows pgx.Rows, fn func(pgx.Row) (T, error)) ([]T, error) {
	defer rows.Close()
	var out []T
	for rows.Next() {
		v, err := fn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Page is a generic offset-pagination request.
type Page struct {
	Page     int
	PageSize int
}

func (p Page) Normalize() Page {
	if p.Page < 1 {
		p.Page = 1
	}
	if p.PageSize < 1 {
		p.PageSize = 25
	}
	if p.PageSize > 200 {
		p.PageSize = 200
	}
	return p
}

func (p Page) Offset() int { return (p.Page - 1) * p.PageSize }
