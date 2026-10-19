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

func scanJob(row pgx.Row) (*domain.Job, error) {
	var j domain.Job
	err := row.Scan(&j.ID, &j.TenantID, &j.CreatedBy, &j.Name, &j.Description, &j.Handler, &j.Payload,
		&j.ScheduleType, &j.RunAt, &j.CronExpr, &j.Timezone, &j.MissedRunPolicy, &j.OverlapPolicy,
		&j.MaxConcurrency, &j.Priority, &j.MaxAttempts, &j.BackoffBaseSeconds, &j.BackoffMaxSeconds,
		&j.TimeoutSeconds, &j.Status, &j.NextRunAt, &j.LastRunAt, &j.LastRunStatus, &j.PausedAt,
		&j.CancelledAt, &j.IdempotencyKey, &j.RequestHash, &j.Version, &j.CreatedAt, &j.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &j, nil
}

// InsertJob inserts a job. If the (tenant, idempotency_key) pair already
// exists it returns inserted=false and the caller must load the existing job.
func InsertJob(ctx context.Context, q db.DBTX, j *domain.Job) (inserted bool, err error) {
	tag, err := q.Exec(ctx, `
		INSERT INTO jobs (id, tenant_id, created_by, name, description, handler, payload, schedule_type,
			run_at, cron_expr, timezone, missed_run_policy, overlap_policy, max_concurrency, priority,
			max_attempts, backoff_base_seconds, backoff_max_seconds, timeout_seconds, status, next_run_at,
			idempotency_key, request_hash, version, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,1,$24,$24)
		ON CONFLICT (tenant_id, idempotency_key) DO NOTHING`,
		j.ID, j.TenantID, j.CreatedBy, j.Name, j.Description, j.Handler, j.Payload, j.ScheduleType,
		j.RunAt, j.CronExpr, j.Timezone, j.MissedRunPolicy, j.OverlapPolicy, j.MaxConcurrency, j.Priority,
		j.MaxAttempts, j.BackoffBaseSeconds, j.BackoffMaxSeconds, j.TimeoutSeconds, j.Status, j.NextRunAt,
		j.IdempotencyKey, j.RequestHash, j.CreatedAt)
	if err != nil {
		return false, fmt.Errorf("insert job: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// GetJob loads a job scoped to a tenant.
func GetJob(ctx context.Context, q db.DBTX, tenantID, id uuid.UUID) (*domain.Job, error) {
	j, err := scanJob(q.QueryRow(ctx, `SELECT `+jobColumns+` FROM jobs j WHERE j.id = $1 AND j.tenant_id = $2`, id, tenantID))
	if db.IsNoRows(err) {
		return nil, domain.ErrNotFound
	}
	return j, err
}

// GetJobByIdempotencyKey loads the job created with a given idempotency key.
func GetJobByIdempotencyKey(ctx context.Context, q db.DBTX, tenantID uuid.UUID, key string) (*domain.Job, error) {
	j, err := scanJob(q.QueryRow(ctx, `SELECT `+jobColumns+` FROM jobs j WHERE j.tenant_id = $1 AND j.idempotency_key = $2`, tenantID, key))
	if db.IsNoRows(err) {
		return nil, domain.ErrNotFound
	}
	return j, err
}

// LockJob loads a job with a row lock. Lock order across TaskFlow is always
// job row first, then run rows, which prevents deadlocks between the
// scheduler, workers and API actions. FOR NO KEY UPDATE does not conflict with
// the KEY SHARE locks taken by foreign-key checks when runs are inserted.
func LockJob(ctx context.Context, q db.DBTX, id uuid.UUID) (*domain.Job, error) {
	j, err := scanJob(q.QueryRow(ctx, `SELECT `+jobColumns+` FROM jobs j WHERE j.id = $1 FOR NO KEY UPDATE`, id))
	if db.IsNoRows(err) {
		return nil, domain.ErrNotFound
	}
	return j, err
}

// LockTenantJob is LockJob with tenant scoping for API-initiated actions.
func LockTenantJob(ctx context.Context, q db.DBTX, tenantID, id uuid.UUID) (*domain.Job, error) {
	j, err := scanJob(q.QueryRow(ctx, `SELECT `+jobColumns+` FROM jobs j WHERE j.id = $1 AND j.tenant_id = $2 FOR NO KEY UPDATE`, id, tenantID))
	if db.IsNoRows(err) {
		return nil, domain.ErrNotFound
	}
	return j, err
}

// LockDueJobs locks up to limit active jobs whose next occurrence is due.
// SKIP LOCKED lets several scheduler instances (or a scheduler racing an API
// edit) proceed without blocking on the same rows.
func LockDueJobs(ctx context.Context, q db.DBTX, now time.Time, limit int) ([]*domain.Job, error) {
	rows, err := q.Query(ctx, `
		SELECT `+jobColumns+` FROM jobs j
		WHERE j.next_run_at IS NOT NULL AND j.next_run_at <= $1
		  AND j.paused_at IS NULL AND j.cancelled_at IS NULL
		ORDER BY j.next_run_at
		LIMIT $2
		FOR NO KEY UPDATE SKIP LOCKED`, now, limit)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanJob)
}

// UpdateJobDefinition persists user-editable fields with optimistic locking.
func UpdateJobDefinition(ctx context.Context, q db.DBTX, j *domain.Job, expectedVersion int, now time.Time) error {
	tag, err := q.Exec(ctx, `
		UPDATE jobs SET name=$1, description=$2, handler=$3, payload=$4, schedule_type=$5, run_at=$6,
			cron_expr=$7, timezone=$8, missed_run_policy=$9, overlap_policy=$10, max_concurrency=$11,
			priority=$12, max_attempts=$13, backoff_base_seconds=$14, backoff_max_seconds=$15,
			timeout_seconds=$16, next_run_at=$17, version = version + 1, updated_at=$18
		WHERE id=$19 AND version=$20`,
		j.Name, j.Description, j.Handler, j.Payload, j.ScheduleType, j.RunAt, j.CronExpr, j.Timezone,
		j.MissedRunPolicy, j.OverlapPolicy, j.MaxConcurrency, j.Priority, j.MaxAttempts,
		j.BackoffBaseSeconds, j.BackoffMaxSeconds, j.TimeoutSeconds, j.NextRunAt, now, j.ID, expectedVersion)
	if err != nil {
		return fmt.Errorf("update job: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.Conflict("version_conflict", "the job was modified by someone else; reload and try again")
	}
	return nil
}

// SetJobNextRun updates the scheduling cursor.
func SetJobNextRun(ctx context.Context, q db.DBTX, id uuid.UUID, next *time.Time, now time.Time) error {
	_, err := q.Exec(ctx, `UPDATE jobs SET next_run_at=$1, updated_at=$2 WHERE id=$3`, next, now, id)
	return err
}

// SetJobPaused toggles the paused flag.
func SetJobPaused(ctx context.Context, q db.DBTX, id uuid.UUID, pausedAt *time.Time, now time.Time) error {
	_, err := q.Exec(ctx, `UPDATE jobs SET paused_at=$1, version=version+1, updated_at=$2 WHERE id=$3`, pausedAt, now, id)
	return err
}

// SetJobCancelled marks a job cancelled and clears its schedule.
func SetJobCancelled(ctx context.Context, q db.DBTX, id uuid.UUID, now time.Time) error {
	_, err := q.Exec(ctx, `UPDATE jobs SET cancelled_at=$1, next_run_at=NULL, version=version+1, updated_at=$1 WHERE id=$2`, now, id)
	return err
}

// SetJobLastRun records the most recent terminal run outcome.
func SetJobLastRun(ctx context.Context, q db.DBTX, id uuid.UUID, at time.Time, status domain.RunStatus) error {
	_, err := q.Exec(ctx, `UPDATE jobs SET last_run_at=$1, last_run_status=$2 WHERE id=$3`, at, status, id)
	return err
}

// SetJobStatus writes the derived aggregate status. Only the engine's
// SyncJobStatus calls this.
func SetJobStatus(ctx context.Context, q db.DBTX, id uuid.UUID, status domain.JobStatus, now time.Time) error {
	_, err := q.Exec(ctx, `UPDATE jobs SET status=$1, updated_at=$2 WHERE id=$3 AND status <> $1`, status, now, id)
	return err
}

// JobFilter drives the jobs explorer.
type JobFilter struct {
	TenantID     uuid.UUID
	Query        string
	Statuses     []string
	Handlers     []string
	ScheduleType string
	Priority     int
	Sort         string // name | created_at | next_run_at | priority | status | updated_at
	Desc         bool
	Page         Page
}

var jobSortColumns = map[string]string{
	"name":        "j.name",
	"created_at":  "j.created_at",
	"updated_at":  "j.updated_at",
	"next_run_at": "j.next_run_at",
	"last_run_at": "j.last_run_at",
	"priority":    "j.priority",
	"status":      "j.status",
}

// JobListItem adds run statistics to a job for the explorer.
type JobListItem struct {
	domain.Job
	SucceededRuns int `json:"succeeded_runs"`
	FailedRuns    int `json:"failed_runs"`
	ActiveRuns    int `json:"active_runs"`
}

// ListJobs returns a filtered, sorted page of jobs plus the total count.
func ListJobs(ctx context.Context, q db.DBTX, f JobFilter) ([]JobListItem, int, error) {
	f.Page = f.Page.Normalize()
	where := []string{"j.tenant_id = $1"}
	args := []any{f.TenantID}
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, strings.ReplaceAll(cond, "?", fmt.Sprintf("$%d", len(args))))
	}
	if f.Query != "" {
		args = append(args, "%"+escapeLike(f.Query)+"%", strings.ToLower(f.Query))
		n := len(args)
		where = append(where, fmt.Sprintf(
			`(j.name ILIKE $%d ESCAPE '\' OR j.description ILIKE $%d ESCAPE '\' OR j.id::text = $%d)`, n-1, n-1, n))
	}
	if len(f.Statuses) > 0 {
		add("j.status = ANY(?)", f.Statuses)
	}
	if len(f.Handlers) > 0 {
		add("j.handler = ANY(?)", f.Handlers)
	}
	if f.ScheduleType != "" {
		add("j.schedule_type = ?", f.ScheduleType)
	}
	if f.Priority > 0 {
		add("j.priority = ?", f.Priority)
	}
	whereSQL := strings.Join(where, " AND ")

	var total int
	if err := q.QueryRow(ctx, "SELECT count(*) FROM jobs j WHERE "+whereSQL, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count jobs: %w", err)
	}

	col, ok := jobSortColumns[f.Sort]
	if !ok {
		col = "j.created_at"
		f.Desc = true
	}
	dir := "ASC"
	if f.Desc {
		dir = "DESC"
	}
	args = append(args, f.Page.PageSize, f.Page.Offset())
	sql := fmt.Sprintf(`
		SELECT %s,
			COALESCE(s.succeeded, 0), COALESCE(s.failed, 0), COALESCE(s.active, 0)
		FROM jobs j
		LEFT JOIN LATERAL (
			SELECT count(*) FILTER (WHERE r.status = 'SUCCEEDED') AS succeeded,
			       count(*) FILTER (WHERE r.status = 'FAILED') AS failed,
			       count(*) FILTER (WHERE r.status IN ('QUEUED','RUNNING','RETRY_WAIT')) AS active
			FROM job_runs r WHERE r.job_id = j.id
		) s ON true
		WHERE %s
		ORDER BY %s %s NULLS LAST, j.id
		LIMIT $%d OFFSET $%d`, jobColumns, whereSQL, col, dir, len(args)-1, len(args))
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list jobs: %w", err)
	}
	items, err := collect(rows, func(row pgx.Row) (JobListItem, error) {
		var it JobListItem
		j := &it.Job
		err := row.Scan(&j.ID, &j.TenantID, &j.CreatedBy, &j.Name, &j.Description, &j.Handler, &j.Payload,
			&j.ScheduleType, &j.RunAt, &j.CronExpr, &j.Timezone, &j.MissedRunPolicy, &j.OverlapPolicy,
			&j.MaxConcurrency, &j.Priority, &j.MaxAttempts, &j.BackoffBaseSeconds, &j.BackoffMaxSeconds,
			&j.TimeoutSeconds, &j.Status, &j.NextRunAt, &j.LastRunAt, &j.LastRunStatus, &j.PausedAt,
			&j.CancelledAt, &j.IdempotencyKey, &j.RequestHash, &j.Version, &j.CreatedAt, &j.UpdatedAt,
			&it.SucceededRuns, &it.FailedRuns, &it.ActiveRuns)
		return it, err
	})
	return items, total, err
}

// ListSchedules returns the tenant's recurring jobs ordered by next run.
func ListSchedules(ctx context.Context, q db.DBTX, tenantID uuid.UUID) ([]*domain.Job, error) {
	rows, err := q.Query(ctx, `SELECT `+jobColumns+` FROM jobs j
		WHERE j.tenant_id = $1 AND j.schedule_type = 'cron'
		ORDER BY (j.cancelled_at IS NOT NULL), j.next_run_at NULLS LAST, j.name LIMIT 500`, tenantID)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanJob)
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}
