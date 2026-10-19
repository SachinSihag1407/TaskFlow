package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/taskflow/taskflow/internal/db"
	"github.com/taskflow/taskflow/internal/domain"
)

func scanWorker(row pgx.Row) (*domain.Worker, error) {
	var w domain.Worker
	err := row.Scan(&w.ID, &w.Hostname, &w.PID, &w.Version, &w.Concurrency, &w.Handlers, &w.Status,
		&w.ActiveRuns, &w.CompletedCount, &w.FailedCount, &w.HeapAllocBytes, &w.Goroutines,
		&w.DrainRequestedAt, &w.StartedAt, &w.LastHeartbeatAt, &w.StoppedAt)
	if err != nil {
		return nil, err
	}
	return &w, nil
}

// RegisterWorker inserts a worker row at startup.
func RegisterWorker(ctx context.Context, q db.DBTX, w *domain.Worker) error {
	_, err := q.Exec(ctx, `
		INSERT INTO workers (id, hostname, pid, version, concurrency, handlers, status, started_at, last_heartbeat_at)
		VALUES ($1,$2,$3,$4,$5,$6,'ONLINE',$7,$7)`,
		w.ID, w.Hostname, w.PID, w.Version, w.Concurrency, w.Handlers, w.StartedAt)
	return err
}

// Heartbeat is a worker's periodic status report.
type Heartbeat struct {
	WorkerID       uuid.UUID
	Status         domain.WorkerState
	ActiveRuns     int
	HeapAllocBytes int64
	Goroutines     int
	Now            time.Time
}

// WorkerHeartbeat records liveness and returns whether an operator requested a
// drain. A worker previously declared OFFLINE by the scheduler (e.g. after a
// long GC pause or network partition) comes back ONLINE on its next beat.
func WorkerHeartbeat(ctx context.Context, q db.DBTX, hb Heartbeat) (drainRequested bool, err error) {
	err = q.QueryRow(ctx, `
		UPDATE workers SET last_heartbeat_at = $2, status = $3, active_runs = $4,
			heap_alloc_bytes = $5, goroutines = $6, stopped_at = NULL
		WHERE id = $1
		RETURNING drain_requested_at IS NOT NULL`,
		hb.WorkerID, hb.Now, hb.Status, hb.ActiveRuns, hb.HeapAllocBytes, hb.Goroutines).Scan(&drainRequested)
	if db.IsNoRows(err) {
		return false, domain.ErrNotFound
	}
	return drainRequested, err
}

// MarkWorkerStopped records a graceful shutdown.
func MarkWorkerStopped(ctx context.Context, q db.DBTX, id uuid.UUID, now time.Time) error {
	_, err := q.Exec(ctx, `UPDATE workers SET status = 'OFFLINE', active_runs = 0, stopped_at = $2, last_heartbeat_at = $2 WHERE id = $1`, id, now)
	return err
}

// IncrementWorkerCounters updates completion statistics.
func IncrementWorkerCounters(ctx context.Context, q db.DBTX, id uuid.UUID, completed, failed int) error {
	_, err := q.Exec(ctx, `UPDATE workers SET completed_count = completed_count + $2, failed_count = failed_count + $3 WHERE id = $1`,
		id, completed, failed)
	return err
}

// RequestDrain asks a worker to stop taking new work.
func RequestDrain(ctx context.Context, q db.DBTX, id uuid.UUID, now time.Time) error {
	tag, err := q.Exec(ctx, `UPDATE workers SET drain_requested_at = COALESCE(drain_requested_at, $2) WHERE id = $1 AND status <> 'OFFLINE'`, id, now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.Conflict("worker_offline", "worker is offline or does not exist")
	}
	return nil
}

// MarkSilentWorkersOffline declares workers offline after prolonged silence
// and returns the affected rows so events can be emitted.
func MarkSilentWorkersOffline(ctx context.Context, q db.DBTX, cutoff time.Time) ([]*domain.Worker, error) {
	rows, err := q.Query(ctx, `
		UPDATE workers w SET status = 'OFFLINE', active_runs = 0
		WHERE w.status <> 'OFFLINE' AND w.last_heartbeat_at < $1
		RETURNING `+workerColumns, cutoff)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanWorker)
}

// ListWorkers returns workers seen recently plus all currently live ones.
func ListWorkers(ctx context.Context, q db.DBTX, seenSince time.Time) ([]*domain.Worker, error) {
	rows, err := q.Query(ctx, `SELECT `+workerColumns+` FROM workers w
		WHERE w.status <> 'OFFLINE' OR w.last_heartbeat_at >= $1
		ORDER BY (w.status = 'OFFLINE'), w.started_at DESC LIMIT 200`, seenSince)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanWorker)
}

// GetWorker loads one worker.
func GetWorker(ctx context.Context, q db.DBTX, id uuid.UUID) (*domain.Worker, error) {
	w, err := scanWorker(q.QueryRow(ctx, `SELECT `+workerColumns+` FROM workers w WHERE w.id = $1`, id))
	if db.IsNoRows(err) {
		return nil, domain.ErrNotFound
	}
	return w, err
}

// DeleteOldWorkers removes long-offline worker rows.
func DeleteOldWorkers(ctx context.Context, q db.DBTX, before time.Time) (int64, error) {
	tag, err := q.Exec(ctx, `DELETE FROM workers WHERE status = 'OFFLINE' AND last_heartbeat_at < $1`, before)
	return tag.RowsAffected(), err
}

// RunningRunsForWorker lists the runs a worker currently owns (tenant scoped).
func RunningRunsForWorker(ctx context.Context, q db.DBTX, tenantID, workerID uuid.UUID) ([]*domain.Run, error) {
	rows, err := q.Query(ctx, `SELECT `+runColumns+`, j.name FROM job_runs r JOIN jobs j ON j.id = r.job_id
		WHERE r.worker_id = $1 AND r.tenant_id = $2 AND r.status = 'RUNNING' ORDER BY r.started_at`, workerID, tenantID)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanRunWithJobName)
}

// UpsertSchedulerInstance records scheduler liveness and tick statistics.
func UpsertSchedulerInstance(ctx context.Context, q db.DBTX, s *domain.SchedulerInstance) error {
	_, err := q.Exec(ctx, `
		INSERT INTO scheduler_instances (id, hostname, is_leader, started_at, last_heartbeat_at, last_tick_at, last_tick_ms, last_tick_enqueued)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (id) DO UPDATE SET is_leader = EXCLUDED.is_leader, last_heartbeat_at = EXCLUDED.last_heartbeat_at,
			last_tick_at = EXCLUDED.last_tick_at, last_tick_ms = EXCLUDED.last_tick_ms,
			last_tick_enqueued = EXCLUDED.last_tick_enqueued, stopped_at = NULL`,
		s.ID, s.Hostname, s.IsLeader, s.StartedAt, s.LastHeartbeatAt, s.LastTickAt, s.LastTickMS, s.LastTickEnqueued)
	return err
}

// StopSchedulerInstance marks a scheduler as stopped.
func StopSchedulerInstance(ctx context.Context, q db.DBTX, id uuid.UUID, now time.Time) error {
	_, err := q.Exec(ctx, `UPDATE scheduler_instances SET is_leader = false, stopped_at = $2 WHERE id = $1`, id, now)
	return err
}

// ListSchedulerInstances returns recently active schedulers.
func ListSchedulerInstances(ctx context.Context, q db.DBTX, since time.Time) ([]*domain.SchedulerInstance, error) {
	rows, err := q.Query(ctx, `SELECT id, hostname, is_leader, started_at, last_heartbeat_at, last_tick_at,
		last_tick_ms, last_tick_enqueued, stopped_at FROM scheduler_instances
		WHERE last_heartbeat_at >= $1 ORDER BY is_leader DESC, last_heartbeat_at DESC`, since)
	if err != nil {
		return nil, err
	}
	return collect(rows, func(row pgx.Row) (*domain.SchedulerInstance, error) {
		var s domain.SchedulerInstance
		err := row.Scan(&s.ID, &s.Hostname, &s.IsLeader, &s.StartedAt, &s.LastHeartbeatAt, &s.LastTickAt,
			&s.LastTickMS, &s.LastTickEnqueued, &s.StoppedAt)
		return &s, err
	})
}
