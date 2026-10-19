package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/taskflow/taskflow/internal/db"
	"github.com/taskflow/taskflow/internal/domain"
	"github.com/taskflow/taskflow/internal/statemachine"
)

func scanRun(row pgx.Row) (*domain.Run, error) {
	var r domain.Run
	err := row.Scan(&r.ID, &r.JobID, &r.TenantID, &r.ScheduledFor, &r.Trigger, &r.Status, &r.Priority,
		&r.Handler, &r.Payload, &r.TimeoutSeconds, &r.MaxAttempts, &r.BackoffBaseSeconds,
		&r.BackoffMaxSeconds, &r.Attempt, &r.CurrentAttemptID, &r.WorkerID, &r.LeaseExpiresAt,
		&r.QueuedAt, &r.StartedAt, &r.FinishedAt, &r.NextAttemptAt, &r.CancelRequestedAt,
		&r.LastError, &r.Result, &r.Checkpoint, &r.DeadLetteredAt, &r.ReplayedAt, &r.ReplayOf,
		&r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func scanRunWithJobName(row pgx.Row) (*domain.Run, error) {
	var r domain.Run
	err := row.Scan(&r.ID, &r.JobID, &r.TenantID, &r.ScheduledFor, &r.Trigger, &r.Status, &r.Priority,
		&r.Handler, &r.Payload, &r.TimeoutSeconds, &r.MaxAttempts, &r.BackoffBaseSeconds,
		&r.BackoffMaxSeconds, &r.Attempt, &r.CurrentAttemptID, &r.WorkerID, &r.LeaseExpiresAt,
		&r.QueuedAt, &r.StartedAt, &r.FinishedAt, &r.NextAttemptAt, &r.CancelRequestedAt,
		&r.LastError, &r.Result, &r.Checkpoint, &r.DeadLetteredAt, &r.ReplayedAt, &r.ReplayOf,
		&r.CreatedAt, &r.UpdatedAt, &r.JobName)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// NewRunFromJob snapshots a job's execution configuration into a QUEUED run.
func NewRunFromJob(j *domain.Job, scheduledFor time.Time, trigger domain.Trigger, now time.Time) *domain.Run {
	return &domain.Run{
		ID:                 uuid.New(),
		JobID:              j.ID,
		TenantID:           j.TenantID,
		ScheduledFor:       scheduledFor,
		Trigger:            trigger,
		Status:             domain.RunQueued,
		Priority:           j.Priority,
		Handler:            j.Handler,
		Payload:            j.Payload,
		TimeoutSeconds:     j.TimeoutSeconds,
		MaxAttempts:        j.MaxAttempts,
		BackoffBaseSeconds: j.BackoffBaseSeconds,
		BackoffMaxSeconds:  j.BackoffMaxSeconds,
		QueuedAt:           &now,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
}

// InsertRun inserts a run. For scheduled occurrences the partial unique index
// on (job_id, scheduled_for) makes this idempotent: a duplicate occurrence
// returns inserted=false instead of creating a second run.
func InsertRun(ctx context.Context, q db.DBTX, r *domain.Run) (inserted bool, err error) {
	tag, err := q.Exec(ctx, `
		INSERT INTO job_runs (id, job_id, tenant_id, scheduled_for, trigger, status, priority, handler,
			payload, timeout_seconds, max_attempts, backoff_base_seconds, backoff_max_seconds, attempt,
			queued_at, replay_of, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,0,$14,$15,$16,$16)
		ON CONFLICT (job_id, scheduled_for) WHERE trigger = 'schedule' DO NOTHING`,
		r.ID, r.JobID, r.TenantID, r.ScheduledFor, r.Trigger, r.Status, r.Priority, r.Handler,
		r.Payload, r.TimeoutSeconds, r.MaxAttempts, r.BackoffBaseSeconds, r.BackoffMaxSeconds,
		r.QueuedAt, r.ReplayOf, r.CreatedAt)
	if err != nil {
		return false, fmt.Errorf("insert run: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// GetRun loads a run scoped to a tenant.
func GetRun(ctx context.Context, q db.DBTX, tenantID, id uuid.UUID) (*domain.Run, error) {
	r, err := scanRunWithJobName(q.QueryRow(ctx, `SELECT `+runColumns+`, j.name FROM job_runs r
		JOIN jobs j ON j.id = r.job_id WHERE r.id = $1 AND r.tenant_id = $2`, id, tenantID))
	if db.IsNoRows(err) {
		return nil, domain.ErrNotFound
	}
	return r, err
}

// GetRunUnscoped loads a run by id for internal components (worker, scheduler).
func GetRunUnscoped(ctx context.Context, q db.DBTX, id uuid.UUID) (*domain.Run, error) {
	r, err := scanRun(q.QueryRow(ctx, `SELECT `+runColumns+` FROM job_runs r WHERE r.id = $1`, id))
	if db.IsNoRows(err) {
		return nil, domain.ErrNotFound
	}
	return r, err
}

// LockRun locks a run row. Callers must already hold the parent job's lock.
func LockRun(ctx context.Context, q db.DBTX, id uuid.UUID) (*domain.Run, error) {
	r, err := scanRun(q.QueryRow(ctx, `SELECT `+runColumns+` FROM job_runs r WHERE r.id = $1 FOR UPDATE`, id))
	if db.IsNoRows(err) {
		return nil, domain.ErrNotFound
	}
	return r, err
}

// LockRunIfStatus locks a run only if it is in the given status, skipping it
// if another transaction holds the lock.
func LockRunIfStatus(ctx context.Context, q db.DBTX, id uuid.UUID, status domain.RunStatus) (*domain.Run, error) {
	r, err := scanRun(q.QueryRow(ctx, `SELECT `+runColumns+` FROM job_runs r
		WHERE r.id = $1 AND r.status = $2 FOR UPDATE SKIP LOCKED`, id, status))
	if db.IsNoRows(err) {
		return nil, domain.ErrNotFound
	}
	return r, err
}

// LockActiveRuns locks all in-flight runs of a job.
func LockActiveRuns(ctx context.Context, q db.DBTX, jobID uuid.UUID) ([]*domain.Run, error) {
	rows, err := q.Query(ctx, `SELECT `+runColumns+` FROM job_runs r
		WHERE r.job_id = $1 AND r.status IN ('QUEUED','RUNNING','RETRY_WAIT') ORDER BY r.created_at FOR UPDATE`, jobID)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanRun)
}

// RunTransition describes a status change applied by TransitionRun.
type RunTransition struct {
	To            domain.RunStatus
	Now           time.Time
	AttemptID     *uuid.UUID // fencing: required current_attempt_id (RUNNING runs)
	NextAttemptAt *time.Time
	LastError     *string
	Result        json.RawMessage
	DeadLetter    bool
}

// TransitionRun applies a validated status change conditionally on the run's
// current status (and, for RUNNING runs, its current attempt). It returns
// ErrNotFound if the condition no longer holds, which callers treat as "lost
// the race" rather than overwriting a newer state.
func TransitionRun(ctx context.Context, q db.DBTX, runID uuid.UUID, from domain.RunStatus, t RunTransition) error {
	if err := statemachine.Validate(from, t.To); err != nil {
		return err
	}
	sets := []string{"status = $3", "updated_at = $4"}
	args := []any{runID, from, t.To, t.Now}
	add := func(expr string, v any) {
		args = append(args, v)
		sets = append(sets, strings.ReplaceAll(expr, "?", fmt.Sprintf("$%d", len(args))))
	}
	switch t.To {
	case domain.RunQueued:
		sets = append(sets, "queued_at = $4", "next_attempt_at = NULL", "published_at = NULL")
	case domain.RunRetryWait:
		add("next_attempt_at = ?", t.NextAttemptAt)
		sets = append(sets, "worker_id = NULL", "lease_expires_at = NULL", "current_attempt_id = NULL")
	case domain.RunSucceeded, domain.RunFailed, domain.RunCancelled:
		sets = append(sets, "finished_at = $4", "lease_expires_at = NULL", "next_attempt_at = NULL")
	}
	if t.LastError != nil {
		add("last_error = ?", *t.LastError)
	}
	if t.Result != nil {
		add("result = ?", t.Result)
	}
	if t.DeadLetter {
		sets = append(sets, "dead_lettered_at = $4")
	}
	where := "id = $1 AND status = $2"
	if t.AttemptID != nil {
		args = append(args, *t.AttemptID)
		where += fmt.Sprintf(" AND current_attempt_id = $%d", len(args))
	}
	tag, err := q.Exec(ctx, "UPDATE job_runs SET "+strings.Join(sets, ", ")+" WHERE "+where, args...)
	if err != nil {
		return fmt.Errorf("transition run %s -> %s: %w", from, t.To, err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// MarkRunClaimed moves a QUEUED run to RUNNING for a specific attempt.
func MarkRunClaimed(ctx context.Context, q db.DBTX, runID, attemptID, workerID uuid.UUID, attemptNo int, leaseUntil, now time.Time) error {
	if err := statemachine.Validate(domain.RunQueued, domain.RunRunning); err != nil {
		return err
	}
	tag, err := q.Exec(ctx, `
		UPDATE job_runs SET status = 'RUNNING', attempt = $2, current_attempt_id = $3, worker_id = $4,
			lease_expires_at = $5, started_at = COALESCE(started_at, $6), updated_at = $6
		WHERE id = $1 AND status = 'QUEUED'`, runID, attemptNo, attemptID, workerID, leaseUntil, now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// RenewLease extends a running attempt's lease. It returns ErrNotFound if the
// attempt no longer owns the run (reclaimed by the reaper or finished), and
// reports whether cancellation has been requested.
func RenewLease(ctx context.Context, q db.DBTX, runID, attemptID uuid.UUID, until time.Time) (cancelRequested bool, err error) {
	err = q.QueryRow(ctx, `
		UPDATE job_runs SET lease_expires_at = $3
		WHERE id = $1 AND current_attempt_id = $2 AND status = 'RUNNING'
		RETURNING cancel_requested_at IS NOT NULL`, runID, attemptID, until).Scan(&cancelRequested)
	if db.IsNoRows(err) {
		return false, domain.ErrNotFound
	}
	return cancelRequested, err
}

// SetCancelRequested flags a RUNNING run for cooperative cancellation.
func SetCancelRequested(ctx context.Context, q db.DBTX, runID uuid.UUID, now time.Time) error {
	_, err := q.Exec(ctx, `UPDATE job_runs SET cancel_requested_at = COALESCE(cancel_requested_at, $2), updated_at = $2
		WHERE id = $1 AND status = 'RUNNING'`, runID, now)
	return err
}

// SaveCheckpoint stores handler progress for the attempt that owns the run.
func SaveCheckpoint(ctx context.Context, q db.DBTX, runID, attemptID uuid.UUID, cp json.RawMessage) error {
	tag, err := q.Exec(ctx, `UPDATE job_runs SET checkpoint = $3 WHERE id = $1 AND current_attempt_id = $2 AND status = 'RUNNING'`,
		runID, attemptID, cp)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// MarkReplayed flags a failed run as replayed, at most once.
func MarkReplayed(ctx context.Context, q db.DBTX, runID uuid.UUID, now time.Time) error {
	tag, err := q.Exec(ctx, `UPDATE job_runs SET replayed_at = $2, updated_at = $2
		WHERE id = $1 AND status = 'FAILED' AND replayed_at IS NULL`, runID, now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.Conflict("not_replayable", "run is not a failed run awaiting replay")
	}
	return nil
}

// MarkPublished records that runs were handed to the Redis queue.
func MarkPublished(ctx context.Context, q db.DBTX, ids []uuid.UUID, now time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	// Best-effort bookkeeping: lock rows in a fixed order and skip contended
	// ones, so this never deadlocks with transactions that lock run rows
	// under their job lock.
	_, err := q.Exec(ctx, `UPDATE job_runs SET published_at = $2 WHERE id IN (
		SELECT id FROM job_runs WHERE id = ANY($1) AND status = 'QUEUED' ORDER BY id FOR UPDATE SKIP LOCKED)`, ids, now)
	return err
}

// ActiveRunSummary returns in-flight run counts for a job. It reads only the
// partial index of active runs, so it stays cheap for long-lived cron jobs.
func ActiveRunSummary(ctx context.Context, q db.DBTX, jobID uuid.UUID) (statemachine.RunSummary, error) {
	var s statemachine.RunSummary
	err := q.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE status = 'RUNNING'),
		       count(*) FILTER (WHERE status = 'QUEUED'),
		       count(*) FILTER (WHERE status = 'RETRY_WAIT')
		FROM job_runs WHERE job_id = $1 AND status IN ('QUEUED','RUNNING','RETRY_WAIT')`, jobID).
		Scan(&s.Running, &s.Queued, &s.RetryWait)
	return s, err
}

// CountRunning returns the number of RUNNING runs of a job.
func CountRunning(ctx context.Context, q db.DBTX, jobID uuid.UUID) (int, error) {
	var n int
	err := q.QueryRow(ctx, `SELECT count(*) FROM job_runs WHERE job_id = $1 AND status = 'RUNNING'`, jobID).Scan(&n)
	return n, err
}

// CountTenantRunning returns the number of RUNNING runs in a tenant.
func CountTenantRunning(ctx context.Context, q db.DBTX, tenantID uuid.UUID) (int, error) {
	var n int
	err := q.QueryRow(ctx, `SELECT count(*) FROM job_runs WHERE tenant_id = $1 AND status = 'RUNNING'`, tenantID).Scan(&n)
	return n, err
}

// QueueRef is the minimal information needed to publish a run to Redis.
type QueueRef struct {
	RunID    uuid.UUID
	Priority int
	QueuedAt time.Time
}

// RunRef identifies a run and its parent job.
type RunRef struct {
	RunID uuid.UUID
	JobID uuid.UUID
}

// DueRetryCandidates returns RETRY_WAIT runs whose backoff has elapsed. Rows
// are not locked; the engine re-checks each one under the job-then-run lock
// order before promoting it.
func DueRetryCandidates(ctx context.Context, q db.DBTX, now time.Time, limit int) ([]RunRef, error) {
	rows, err := q.Query(ctx, `SELECT id, job_id FROM job_runs
		WHERE status = 'RETRY_WAIT' AND next_attempt_at <= $1 ORDER BY next_attempt_at LIMIT $2`, now, limit)
	if err != nil {
		return nil, err
	}
	return collect(rows, func(row pgx.Row) (RunRef, error) {
		var r RunRef
		return r, row.Scan(&r.RunID, &r.JobID)
	})
}

// ExpiredLeaseCandidates returns RUNNING runs whose lease has expired.
func ExpiredLeaseCandidates(ctx context.Context, q db.DBTX, now time.Time, limit int) ([]*domain.Run, error) {
	rows, err := q.Query(ctx, `SELECT `+runColumns+` FROM job_runs r
		WHERE r.status = 'RUNNING' AND r.lease_expires_at < $1 ORDER BY r.lease_expires_at LIMIT $2`, now, limit)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanRun)
}

// UnpublishedQueued returns QUEUED runs of active jobs that may be missing
// from Redis: never published, or queued long enough ago that a worker should
// have claimed them by now.
func UnpublishedQueued(ctx context.Context, q db.DBTX, olderThan time.Time, limit int) ([]QueueRef, error) {
	rows, err := q.Query(ctx, `
		SELECT r.id, r.priority, r.queued_at FROM job_runs r JOIN jobs j ON j.id = r.job_id
		WHERE r.status = 'QUEUED' AND j.paused_at IS NULL
		  AND (r.published_at IS NULL OR r.queued_at < $1)
		ORDER BY r.priority DESC, r.queued_at LIMIT $2`, olderThan, limit)
	if err != nil {
		return nil, err
	}
	return collect(rows, func(row pgx.Row) (QueueRef, error) {
		var ref QueueRef
		return ref, row.Scan(&ref.RunID, &ref.Priority, &ref.QueuedAt)
	})
}

// QueuedRefsForJob returns the job's QUEUED runs (used to re-publish after a
// resume or when a concurrency slot frees up).
func QueuedRefsForJob(ctx context.Context, q db.DBTX, jobID uuid.UUID, limit int) ([]QueueRef, error) {
	rows, err := q.Query(ctx, `SELECT id, priority, queued_at FROM job_runs
		WHERE job_id = $1 AND status = 'QUEUED' ORDER BY queued_at LIMIT $2`, jobID, limit)
	if err != nil {
		return nil, err
	}
	return collect(rows, func(row pgx.Row) (QueueRef, error) {
		var ref QueueRef
		return ref, row.Scan(&ref.RunID, &ref.Priority, &ref.QueuedAt)
	})
}

// ClaimCandidates returns QUEUED run ids in dispatch order for the Redis-less
// fallback path. Rows are not locked here; Claim re-checks under lock.
func ClaimCandidates(ctx context.Context, q db.DBTX, handlers []string, limit int) ([]uuid.UUID, error) {
	rows, err := q.Query(ctx, `
		SELECT r.id FROM job_runs r JOIN jobs j ON j.id = r.job_id
		WHERE r.status = 'QUEUED' AND j.paused_at IS NULL AND r.handler = ANY($1)
		ORDER BY r.priority DESC, r.queued_at LIMIT $2`, handlers, limit)
	if err != nil {
		return nil, err
	}
	return collect(rows, func(row pgx.Row) (uuid.UUID, error) {
		var id uuid.UUID
		return id, row.Scan(&id)
	})
}

// RunFilter drives run listings (dead-letter queue, job details).
type RunFilter struct {
	TenantID     uuid.UUID
	JobID        *uuid.UUID
	Statuses     []string
	DeadLettered bool
	Query        string
	Page         Page
}

// ListRuns returns a page of runs, newest first.
func ListRuns(ctx context.Context, q db.DBTX, f RunFilter) ([]*domain.Run, int, error) {
	f.Page = f.Page.Normalize()
	where := []string{"r.tenant_id = $1"}
	args := []any{f.TenantID}
	if f.JobID != nil {
		args = append(args, *f.JobID)
		where = append(where, fmt.Sprintf("r.job_id = $%d", len(args)))
	}
	if len(f.Statuses) > 0 {
		args = append(args, f.Statuses)
		where = append(where, fmt.Sprintf("r.status = ANY($%d)", len(args)))
	}
	order := "r.created_at DESC"
	if f.DeadLettered {
		where = append(where, "r.dead_lettered_at IS NOT NULL")
		order = "r.dead_lettered_at DESC"
	}
	if f.Query != "" {
		args = append(args, "%"+escapeLike(f.Query)+"%")
		where = append(where, fmt.Sprintf(`(j.name ILIKE $%d ESCAPE '\' OR r.last_error ILIKE $%d ESCAPE '\')`, len(args), len(args)))
	}
	whereSQL := strings.Join(where, " AND ")
	var total int
	if err := q.QueryRow(ctx, "SELECT count(*) FROM job_runs r JOIN jobs j ON j.id = r.job_id WHERE "+whereSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, f.Page.PageSize, f.Page.Offset())
	rows, err := q.Query(ctx, fmt.Sprintf(`SELECT %s, j.name FROM job_runs r JOIN jobs j ON j.id = r.job_id
		WHERE %s ORDER BY %s, r.id LIMIT $%d OFFSET $%d`, runColumns, whereSQL, order, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	runs, err := collect(rows, scanRunWithJobName)
	return runs, total, err
}

// LastFailedRun returns the job's most recent FAILED run, if any.
func LastFailedRun(ctx context.Context, q db.DBTX, jobID uuid.UUID) (*domain.Run, error) {
	r, err := scanRun(q.QueryRow(ctx, `SELECT `+runColumns+` FROM job_runs r
		WHERE r.job_id = $1 AND r.status = 'FAILED' ORDER BY r.finished_at DESC NULLS LAST LIMIT 1`, jobID))
	if db.IsNoRows(err) {
		return nil, domain.ErrNotFound
	}
	return r, err
}

// RecentRunsForJobs returns the most recent runs of each job (schedules page).
func RecentRunsForJobs(ctx context.Context, q db.DBTX, jobIDs []uuid.UUID, perJob int) (map[uuid.UUID][]*domain.Run, error) {
	out := map[uuid.UUID][]*domain.Run{}
	if len(jobIDs) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `
		SELECT `+runColumns+` FROM unnest($1::uuid[]) AS ids(job_id)
		CROSS JOIN LATERAL (
			SELECT * FROM job_runs x WHERE x.job_id = ids.job_id ORDER BY x.created_at DESC LIMIT $2
		) r`, jobIDs, perJob)
	if err != nil {
		return nil, err
	}
	runs, err := collect(rows, scanRun)
	if err != nil {
		return nil, err
	}
	for _, r := range runs {
		out[r.JobID] = append(out[r.JobID], r)
	}
	return out, nil
}
