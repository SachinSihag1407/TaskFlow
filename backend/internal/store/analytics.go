package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/taskflow/taskflow/internal/db"
)

// JobStatusCounts returns the number of jobs in each aggregate status.
func JobStatusCounts(ctx context.Context, q db.DBTX, tenantID uuid.UUID) (map[string]int, error) {
	rows, err := q.Query(ctx, `SELECT status, count(*) FROM jobs WHERE tenant_id = $1 GROUP BY status`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			return nil, err
		}
		out[s] = n
	}
	return out, rows.Err()
}

// RunStatusCounts returns current counts of in-flight runs.
func RunStatusCounts(ctx context.Context, q db.DBTX, tenantID uuid.UUID) (queued, running, retryWait int, err error) {
	err = q.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE status = 'QUEUED'),
		count(*) FILTER (WHERE status = 'RUNNING'),
		count(*) FILTER (WHERE status = 'RETRY_WAIT')
		FROM job_runs WHERE tenant_id = $1 AND status IN ('QUEUED','RUNNING','RETRY_WAIT')`, tenantID).
		Scan(&queued, &running, &retryWait)
	return
}

// OutcomeStats summarises activity in a time window.
type OutcomeStats struct {
	RunsSucceeded  int      `json:"runs_succeeded"`
	RunsFailed     int      `json:"runs_failed"`
	RunsCancelled  int      `json:"runs_cancelled"`
	AttemptsTotal  int      `json:"attempts_total"`
	AttemptsFailed int      `json:"attempts_failed"`
	Retries        int      `json:"retries"`
	LatencySamples int      `json:"latency_samples"`
	LatencyAvgMS   *float64 `json:"latency_avg_ms"`
	LatencyP50MS   *float64 `json:"latency_p50_ms"`
	LatencyP95MS   *float64 `json:"latency_p95_ms"`
	LatencyP99MS   *float64 `json:"latency_p99_ms"`
	QueueWaitAvgMS *float64 `json:"queue_wait_avg_ms"`
	QueueWaitP95MS *float64 `json:"queue_wait_p95_ms"`
}

// MinLatencySamples is the minimum number of samples before percentiles are
// reported; below it the API returns null rather than a misleading figure.
const MinLatencySamples = 5

// Outcomes computes run and attempt statistics for [since, now).
func Outcomes(ctx context.Context, q db.DBTX, tenantID uuid.UUID, since time.Time) (*OutcomeStats, error) {
	s := &OutcomeStats{}
	err := q.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE status = 'SUCCEEDED'),
		count(*) FILTER (WHERE status = 'FAILED'),
		count(*) FILTER (WHERE status = 'CANCELLED')
		FROM job_runs WHERE tenant_id = $1 AND finished_at >= $2`, tenantID, since).
		Scan(&s.RunsSucceeded, &s.RunsFailed, &s.RunsCancelled)
	if err != nil {
		return nil, err
	}
	err = q.QueryRow(ctx, `SELECT
		count(*),
		count(*) FILTER (WHERE status IN ('FAILED','TIMED_OUT','LOST')),
		count(*) FILTER (WHERE attempt_number > 1),
		count(*) FILTER (WHERE status = 'SUCCEEDED' AND duration_ms IS NOT NULL),
		avg(duration_ms) FILTER (WHERE status = 'SUCCEEDED'),
		percentile_cont(0.5) WITHIN GROUP (ORDER BY duration_ms) FILTER (WHERE status = 'SUCCEEDED'),
		percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms) FILTER (WHERE status = 'SUCCEEDED'),
		percentile_cont(0.99) WITHIN GROUP (ORDER BY duration_ms) FILTER (WHERE status = 'SUCCEEDED'),
		avg(EXTRACT(EPOCH FROM (started_at - queued_at)) * 1000) FILTER (WHERE queued_at IS NOT NULL),
		percentile_cont(0.95) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (started_at - queued_at)) * 1000)
			FILTER (WHERE queued_at IS NOT NULL)
		FROM job_attempts WHERE tenant_id = $1 AND finished_at >= $2`, tenantID, since).
		Scan(&s.AttemptsTotal, &s.AttemptsFailed, &s.Retries, &s.LatencySamples, &s.LatencyAvgMS,
			&s.LatencyP50MS, &s.LatencyP95MS, &s.LatencyP99MS, &s.QueueWaitAvgMS, &s.QueueWaitP95MS)
	if err != nil {
		return nil, err
	}
	if s.LatencySamples < MinLatencySamples {
		s.LatencyAvgMS, s.LatencyP50MS, s.LatencyP95MS, s.LatencyP99MS = nil, nil, nil, nil
	}
	return s, nil
}

// ThroughputPoint is one bucket of the throughput series.
type ThroughputPoint struct {
	Bucket    time.Time `json:"bucket"`
	Succeeded int       `json:"succeeded"`
	Failed    int       `json:"failed"`
}

// Throughput returns finished-attempt counts per bucket for [since, until),
// with empty buckets filled in so charts have a continuous x axis.
func Throughput(ctx context.Context, q db.DBTX, tenantID uuid.UUID, since, until time.Time, bucket time.Duration) ([]ThroughputPoint, error) {
	origin := since.Truncate(bucket)
	rows, err := q.Query(ctx, `
		SELECT date_bin($4::interval, finished_at, $5) AS b,
		       count(*) FILTER (WHERE status = 'SUCCEEDED'),
		       count(*) FILTER (WHERE status <> 'SUCCEEDED')
		FROM job_attempts
		WHERE tenant_id = $1 AND finished_at >= $2 AND finished_at < $3
		GROUP BY b ORDER BY b`, tenantID, origin, until, fmt.Sprintf("%d seconds", int64(bucket.Seconds())), origin)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	found := map[int64]ThroughputPoint{}
	for rows.Next() {
		var p ThroughputPoint
		if err := rows.Scan(&p.Bucket, &p.Succeeded, &p.Failed); err != nil {
			return nil, err
		}
		found[p.Bucket.Unix()] = p
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []ThroughputPoint
	for b := origin; b.Before(until); b = b.Add(bucket) {
		if p, ok := found[b.Unix()]; ok {
			p.Bucket = p.Bucket.UTC()
			out = append(out, p)
		} else {
			out = append(out, ThroughputPoint{Bucket: b.UTC()})
		}
	}
	return out, nil
}

// HandlerBreakdown counts finished attempts by handler and outcome.
type HandlerBreakdown struct {
	Handler   string `json:"handler"`
	Succeeded int    `json:"succeeded"`
	Failed    int    `json:"failed"`
}

func OutcomesByHandler(ctx context.Context, q db.DBTX, tenantID uuid.UUID, since time.Time) ([]HandlerBreakdown, error) {
	rows, err := q.Query(ctx, `SELECT handler,
		count(*) FILTER (WHERE status = 'SUCCEEDED'), count(*) FILTER (WHERE status <> 'SUCCEEDED')
		FROM job_attempts WHERE tenant_id = $1 AND finished_at >= $2 GROUP BY handler ORDER BY count(*) DESC`, tenantID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HandlerBreakdown
	for rows.Next() {
		var h HandlerBreakdown
		if err := rows.Scan(&h.Handler, &h.Succeeded, &h.Failed); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// GlobalQueueCounts returns platform-wide queued/running counts (for health).
func GlobalQueueCounts(ctx context.Context, q db.DBTX) (queued, running, retryWait int, err error) {
	err = q.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE status = 'QUEUED'),
		count(*) FILTER (WHERE status = 'RUNNING'),
		count(*) FILTER (WHERE status = 'RETRY_WAIT')
		FROM job_runs WHERE status IN ('QUEUED','RUNNING','RETRY_WAIT')`).Scan(&queued, &running, &retryWait)
	return
}

// GlobalErrorRates returns platform-wide attempt counts in a window.
func GlobalErrorRates(ctx context.Context, q db.DBTX, since time.Time) (total, failed, retries int, err error) {
	err = q.QueryRow(ctx, `SELECT count(*),
		count(*) FILTER (WHERE status IN ('FAILED','TIMED_OUT','LOST')),
		count(*) FILTER (WHERE attempt_number > 1)
		FROM job_attempts WHERE finished_at >= $1`, since).Scan(&total, &failed, &retries)
	return
}
