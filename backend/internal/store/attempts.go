package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/taskflow/taskflow/internal/db"
	"github.com/taskflow/taskflow/internal/domain"
)

func scanAttempt(row pgx.Row) (*domain.Attempt, error) {
	var a domain.Attempt
	err := row.Scan(&a.ID, &a.RunID, &a.JobID, &a.TenantID, &a.AttemptNumber, &a.WorkerID, &a.Handler,
		&a.Status, &a.QueuedAt, &a.StartedAt, &a.FinishedAt, &a.DurationMS, &a.ErrorKind,
		&a.ErrorMessage, &a.Retryable, &a.NextRetryAt)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func scanAttemptJoined(row pgx.Row) (*domain.Attempt, error) {
	var a domain.Attempt
	var host *string
	err := row.Scan(&a.ID, &a.RunID, &a.JobID, &a.TenantID, &a.AttemptNumber, &a.WorkerID, &a.Handler,
		&a.Status, &a.QueuedAt, &a.StartedAt, &a.FinishedAt, &a.DurationMS, &a.ErrorKind,
		&a.ErrorMessage, &a.Retryable, &a.NextRetryAt, &a.JobName, &host, &a.MaxAttempts)
	if err != nil {
		return nil, err
	}
	if host != nil {
		a.WorkerHostname = *host
	}
	return &a, nil
}

const attemptJoin = ` FROM job_attempts a JOIN jobs j ON j.id = a.job_id JOIN job_runs r ON r.id = a.run_id
	LEFT JOIN workers w ON w.id = a.worker_id`

// InsertAttempt records the start of an attempt.
func InsertAttempt(ctx context.Context, q db.DBTX, a *domain.Attempt) error {
	_, err := q.Exec(ctx, `
		INSERT INTO job_attempts (id, run_id, job_id, tenant_id, attempt_number, worker_id, handler, status, queued_at, started_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,'RUNNING',$8,$9)`,
		a.ID, a.RunID, a.JobID, a.TenantID, a.AttemptNumber, a.WorkerID, a.Handler, a.QueuedAt, a.StartedAt)
	if err != nil {
		return fmt.Errorf("insert attempt: %w", err)
	}
	return nil
}

// AttemptOutcome is the final state of an attempt.
type AttemptOutcome struct {
	Status       domain.AttemptStatus
	FinishedAt   time.Time
	ErrorKind    *string
	ErrorMessage *string
	Retryable    *bool
	NextRetryAt  *time.Time
}

// FinishAttempt closes a RUNNING attempt. The status condition makes it a
// no-op if the reaper already marked the attempt LOST.
func FinishAttempt(ctx context.Context, q db.DBTX, id uuid.UUID, o AttemptOutcome) error {
	_, err := q.Exec(ctx, `
		UPDATE job_attempts SET status = $2, finished_at = $3,
			duration_ms = GREATEST(0, (EXTRACT(EPOCH FROM ($3 - started_at)) * 1000)::bigint),
			error_kind = $4, error_message = $5, retryable = $6, next_retry_at = $7
		WHERE id = $1 AND status = 'RUNNING'`,
		id, o.Status, o.FinishedAt, o.ErrorKind, o.ErrorMessage, o.Retryable, o.NextRetryAt)
	return err
}

// GetAttempt loads one attempt scoped to a tenant.
func GetAttempt(ctx context.Context, q db.DBTX, tenantID, id uuid.UUID) (*domain.Attempt, error) {
	a, err := scanAttemptJoined(q.QueryRow(ctx, `SELECT `+attemptColumns+`, j.name, w.hostname, r.max_attempts`+attemptJoin+
		` WHERE a.id = $1 AND a.tenant_id = $2`, id, tenantID))
	if db.IsNoRows(err) {
		return nil, domain.ErrNotFound
	}
	return a, err
}

// AttemptFilter drives the execution history page.
type AttemptFilter struct {
	TenantID uuid.UUID
	JobID    *uuid.UUID
	RunID    *uuid.UUID
	WorkerID *uuid.UUID
	Statuses []string
	Handlers []string
	Query    string
	Since    *time.Time
	Until    *time.Time
	Sort     string // started_at | duration_ms
	Desc     bool
	Page     Page
}

// ListAttempts returns a page of attempts.
func ListAttempts(ctx context.Context, q db.DBTX, f AttemptFilter) ([]*domain.Attempt, int, error) {
	f.Page = f.Page.Normalize()
	where := []string{"a.tenant_id = $1"}
	args := []any{f.TenantID}
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, strings.ReplaceAll(cond, "?", fmt.Sprintf("$%d", len(args))))
	}
	if f.JobID != nil {
		add("a.job_id = ?", *f.JobID)
	}
	if f.RunID != nil {
		add("a.run_id = ?", *f.RunID)
	}
	if f.WorkerID != nil {
		add("a.worker_id = ?", *f.WorkerID)
	}
	if len(f.Statuses) > 0 {
		add("a.status = ANY(?)", f.Statuses)
	}
	if len(f.Handlers) > 0 {
		add("a.handler = ANY(?)", f.Handlers)
	}
	if f.Since != nil {
		add("a.started_at >= ?", *f.Since)
	}
	if f.Until != nil {
		add("a.started_at < ?", *f.Until)
	}
	if f.Query != "" {
		add(`(j.name ILIKE ? ESCAPE '\' OR a.error_message ILIKE ? ESCAPE '\')`, "%"+escapeLike(f.Query)+"%")
	}
	whereSQL := strings.Join(where, " AND ")
	var total int
	if err := q.QueryRow(ctx, "SELECT count(*)"+attemptJoin+" WHERE "+whereSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	order := "a.started_at"
	if f.Sort == "duration_ms" {
		order = "a.duration_ms"
	}
	dir := "DESC"
	if !f.Desc && f.Sort != "" {
		dir = "ASC"
	}
	args = append(args, f.Page.PageSize, f.Page.Offset())
	rows, err := q.Query(ctx, fmt.Sprintf(`SELECT %s, j.name, w.hostname, r.max_attempts%s WHERE %s
		ORDER BY %s %s NULLS LAST, a.id LIMIT $%d OFFSET $%d`,
		attemptColumns, attemptJoin, whereSQL, order, dir, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	items, err := collect(rows, scanAttemptJoined)
	return items, total, err
}

// AttemptsForJob returns a job's most recent attempts, newest first.
func AttemptsForJob(ctx context.Context, q db.DBTX, tenantID, jobID uuid.UUID, limit int) ([]*domain.Attempt, error) {
	rows, err := q.Query(ctx, `SELECT `+attemptColumns+`, j.name, w.hostname, r.max_attempts`+attemptJoin+`
		WHERE a.tenant_id = $1 AND a.job_id = $2 ORDER BY a.started_at DESC LIMIT $3`, tenantID, jobID, limit)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanAttemptJoined)
}

// GetAttemptUnscoped loads an attempt by id for internal components.
func GetAttemptUnscoped(ctx context.Context, q db.DBTX, id uuid.UUID) (*domain.Attempt, error) {
	a, err := scanAttempt(q.QueryRow(ctx, `SELECT `+attemptColumns+` FROM job_attempts a WHERE a.id = $1`, id))
	if db.IsNoRows(err) {
		return nil, domain.ErrNotFound
	}
	return a, err
}

// MaxLogLinesPerAttempt bounds log storage per attempt.
const MaxLogLinesPerAttempt = 1000

// InsertLogs appends log lines for an attempt using a single batched insert.
func InsertLogs(ctx context.Context, q db.DBTX, attemptID uuid.UUID, lines []domain.LogLine) error {
	if len(lines) == 0 {
		return nil
	}
	ts := make([]time.Time, len(lines))
	levels := make([]string, len(lines))
	msgs := make([]string, len(lines))
	for i, l := range lines {
		ts[i], levels[i], msgs[i] = l.TS, l.Level, l.Message
	}
	_, err := q.Exec(ctx, `INSERT INTO attempt_logs (attempt_id, ts, level, message)
		SELECT $1, t, l, m FROM unnest($2::timestamptz[], $3::text[], $4::text[]) AS x(t, l, m)`,
		attemptID, ts, levels, msgs)
	return err
}

// ListLogs returns log lines for an attempt after a cursor (for incremental
// fetching while an attempt is still running).
func ListLogs(ctx context.Context, q db.DBTX, tenantID, attemptID uuid.UUID, afterID int64, limit int) ([]domain.LogLine, error) {
	if limit <= 0 || limit > MaxLogLinesPerAttempt {
		limit = MaxLogLinesPerAttempt
	}
	rows, err := q.Query(ctx, `SELECT l.id, l.ts, l.level, l.message FROM attempt_logs l
		JOIN job_attempts a ON a.id = l.attempt_id
		WHERE l.attempt_id = $1 AND a.tenant_id = $2 AND l.id > $3 ORDER BY l.id LIMIT $4`,
		attemptID, tenantID, afterID, limit)
	if err != nil {
		return nil, err
	}
	return collect(rows, func(row pgx.Row) (domain.LogLine, error) {
		var l domain.LogLine
		return l, row.Scan(&l.ID, &l.TS, &l.Level, &l.Message)
	})
}
