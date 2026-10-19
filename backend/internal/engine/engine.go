// Package engine implements every lifecycle operation in TaskFlow: scheduling
// occurrences, claiming, completing, retrying, recovering and cancelling runs.
//
// Invariants enforced here:
//
//   - PostgreSQL is the source of truth. Each operation is one transaction
//     that validates transitions with package statemachine and applies them
//     with conditional UPDATEs, so concurrent actors cannot both succeed.
//   - Lock order is always job row, then run rows. This prevents deadlocks
//     between the scheduler, workers and API requests.
//   - Side effects outside PostgreSQL (Redis publication, SSE events,
//     cancellation signals) happen only after commit and are best effort.
//     The scheduler's reconciler repairs anything lost between commit and
//     publication.
//   - Writes from a worker are fenced by the run's current_attempt_id, so a
//     worker whose lease was reclaimed can never overwrite a newer attempt.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/taskflow/taskflow/internal/db"
	"github.com/taskflow/taskflow/internal/domain"
	"github.com/taskflow/taskflow/internal/metrics"
	"github.com/taskflow/taskflow/internal/queue"
	"github.com/taskflow/taskflow/internal/retry"
	"github.com/taskflow/taskflow/internal/statemachine"
	"github.com/taskflow/taskflow/internal/store"
)

var (
	// ErrStale: the run is no longer QUEUED (claimed elsewhere, cancelled, or
	// a duplicate queue entry). The queue entry should simply be dropped.
	ErrStale = errors.New("run is not claimable")
	// ErrDeferred: a concurrency limit is saturated; try again shortly.
	ErrDeferred = errors.New("concurrency limit reached")
	// ErrPaused: the job is paused; resume re-publishes its queued runs.
	ErrPaused = errors.New("job is paused")
	// ErrLeaseLost: the worker no longer owns the run.
	ErrLeaseLost = errors.New("lease lost")
	// ErrUnsupported: this worker does not run the run's handler.
	ErrUnsupported = errors.New("handler not supported by this worker")
	// ErrNoWork: the database fallback found nothing claimable.
	ErrNoWork = errors.New("no claimable runs")
)

// Publisher is the subset of the Redis queue the engine needs. A nil
// Publisher is allowed (tests, degraded mode).
type Publisher interface {
	Publish(ctx context.Context, items ...queue.Item) error
	PublishEvent(ctx context.Context, e queue.Event) error
	PublishCancel(ctx context.Context, runID uuid.UUID) error
	Remove(ctx context.Context, runID uuid.UUID) error
}

// Engine coordinates lifecycle operations.
type Engine struct {
	pool *pgxpool.Pool
	pub  Publisher
	log  *slog.Logger
	// Now and Rand are injectable for deterministic tests.
	Now  func() time.Time
	Rand retry.RandFunc
}

func New(pool *pgxpool.Pool, pub Publisher, log *slog.Logger) *Engine {
	return &Engine{pool: pool, pub: pub, log: log, Now: func() time.Time { return time.Now().UTC() }}
}

// Pool exposes the database pool to collaborators in the same process.
func (e *Engine) Pool() *pgxpool.Pool { return e.pool }

// effects collects post-commit side effects of a transaction.
type effects struct {
	publish []queue.Item
	events  []queue.Event
	cancels []uuid.UUID
	removes []uuid.UUID
}

func (f *effects) queueRun(r *domain.Run) {
	at := r.CreatedAt
	if r.QueuedAt != nil {
		at = *r.QueuedAt
	}
	f.publish = append(f.publish, queue.Item{RunID: r.ID, Priority: r.Priority, QueuedAt: at})
}

func (f *effects) queueRefs(refs []store.QueueRef) {
	for _, ref := range refs {
		f.publish = append(f.publish, queue.Item{RunID: ref.RunID, Priority: ref.Priority, QueuedAt: ref.QueuedAt})
	}
}

func (f *effects) runEvent(r *domain.Run, status domain.RunStatus) {
	f.events = append(f.events, queue.Event{Type: "run.updated", TenantID: r.TenantID.String(),
		JobID: r.JobID.String(), RunID: r.ID.String(), Status: string(status)})
}

// flush applies side effects after commit. Failures are logged, counted and
// otherwise tolerated: PostgreSQL already holds the authoritative state.
func (e *Engine) flush(ctx context.Context, f *effects) {
	if e.pub == nil || f == nil {
		return
	}
	ctx = context.WithoutCancel(ctx)
	if len(f.publish) > 0 {
		if err := e.pub.Publish(ctx, f.publish...); err != nil {
			metrics.RedisErrors.WithLabelValues("publish").Inc()
			e.log.Warn("publish to redis failed; reconciler will retry", "runs", len(f.publish), "err", err)
		} else {
			ids := make([]uuid.UUID, len(f.publish))
			for i, it := range f.publish {
				ids[i] = it.RunID
			}
			if err := store.MarkPublished(ctx, e.pool, ids, e.Now()); err != nil {
				e.log.Warn("mark published failed", "err", err)
			}
		}
	}
	for _, id := range f.removes {
		if err := e.pub.Remove(ctx, id); err != nil {
			metrics.RedisErrors.WithLabelValues("remove").Inc()
		}
	}
	for _, id := range f.cancels {
		if err := e.pub.PublishCancel(ctx, id); err != nil {
			metrics.RedisErrors.WithLabelValues("publish_cancel").Inc()
			e.log.Warn("cancel signal failed; worker will observe it on lease renewal", "run_id", id, "err", err)
		}
	}
	for _, ev := range f.events {
		if err := e.pub.PublishEvent(ctx, ev); err != nil {
			metrics.RedisErrors.WithLabelValues("publish_event").Inc()
			break // pub/sub is down; skip the rest rather than log per event
		}
	}
}

// SyncJobStatus recomputes a job's aggregate status from its in-flight runs
// and persists it if it changed. The caller must hold the job's row lock and
// must have applied any pending changes to j (next_run_at, paused_at, ...).
func (e *Engine) SyncJobStatus(ctx context.Context, tx pgx.Tx, j *domain.Job, f *effects) (domain.JobStatus, error) {
	summary, err := store.ActiveRunSummary(ctx, tx, j.ID)
	if err != nil {
		return "", err
	}
	summary.LastTerminal = j.LastRunStatus
	status := statemachine.DeriveJobStatus(statemachine.JobState{
		Recurring:  j.Recurring(),
		Paused:     j.PausedAt != nil,
		Cancelled:  j.CancelledAt != nil,
		HasNextRun: j.NextRunAt != nil,
	}, summary)
	if status != j.Status {
		if err := store.SetJobStatus(ctx, tx, j.ID, status, e.Now()); err != nil {
			return "", err
		}
		j.Status = status
		if f != nil {
			f.events = append(f.events, queue.Event{Type: "job.updated", TenantID: j.TenantID.String(),
				JobID: j.ID.String(), Status: string(status)})
		}
	}
	return status, nil
}

// SyncJobStatusTx is SyncJobStatus for callers outside the package that run
// their own transaction (job creation and edits in the service layer).
func (e *Engine) SyncJobStatusTx(ctx context.Context, tx pgx.Tx, j *domain.Job) (domain.JobStatus, []queue.Event, error) {
	f := &effects{}
	s, err := e.SyncJobStatus(ctx, tx, j, f)
	return s, f.events, err
}

// PublishEvents forwards events collected by SyncJobStatusTx after commit.
func (e *Engine) PublishEvents(ctx context.Context, events []queue.Event) {
	e.flush(ctx, &effects{events: events})
}

// RecordEvent appends an operational event inside tx.
func (e *Engine) RecordEvent(ctx context.Context, tx db.DBTX, ev domain.Event) {
	if ev.CreatedAt.IsZero() {
		ev.CreatedAt = e.Now()
	}
	if err := store.InsertEvent(ctx, tx, &ev); err != nil {
		// Events are diagnostic; never fail the business operation for them.
		e.log.Warn("record event failed", "type", ev.Type, "err", err)
	}
}

func jobEvent(j *domain.Job, typ string, sev domain.Severity, msg string) domain.Event {
	return domain.Event{TenantID: &j.TenantID, JobID: &j.ID, Type: typ, Severity: sev, Message: msg}
}

func runEventRecord(r *domain.Run, typ string, sev domain.Severity, msg string, data map[string]any) domain.Event {
	ev := domain.Event{TenantID: &r.TenantID, JobID: &r.JobID, RunID: &r.ID, Type: typ, Severity: sev, Message: msg,
		WorkerID: r.WorkerID}
	if data != nil {
		if b, err := json.Marshal(data); err == nil {
			ev.Data = b
		}
	}
	return ev
}

// Audit writes an audit record inside tx. Audit failures abort the operation:
// a sensitive action must not succeed without its audit trail.
func (e *Engine) Audit(ctx context.Context, tx db.DBTX, actor *domain.Principal, action, targetType, targetID string, meta map[string]any) error {
	a := &domain.AuditEvent{Action: action, TargetType: &targetType, TargetID: &targetID, CreatedAt: e.Now()}
	if actor != nil {
		a.TenantID = &actor.TenantID
		a.ActorID = &actor.UserID
		a.ActorEmail = &actor.Email
		if actor.IP != "" {
			a.IP = &actor.IP
		}
		if actor.UserAgent != "" {
			ua := actor.UserAgent
			if len(ua) > 256 {
				ua = ua[:256]
			}
			a.UserAgent = &ua
		}
	}
	if meta != nil {
		b, err := json.Marshal(meta)
		if err != nil {
			return err
		}
		a.Metadata = b
	}
	return store.InsertAudit(ctx, tx, a)
}

// effectiveConcurrency is the number of runs of a job allowed to execute at
// once. Only overlap_policy=allow permits more than one.
func effectiveConcurrency(j *domain.Job) int {
	if j.OverlapPolicy == domain.OverlapAllow && j.MaxConcurrency > 1 {
		return j.MaxConcurrency
	}
	return 1
}

func backoffFor(r *domain.Run) retry.Policy {
	return retry.Policy{
		Base: time.Duration(r.BackoffBaseSeconds) * time.Second,
		Max:  time.Duration(r.BackoffMaxSeconds) * time.Second,
	}
}

func ptr[T any](v T) *T { return &v }
