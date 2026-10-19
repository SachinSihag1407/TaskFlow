// Package scheduler runs TaskFlow's periodic control loops:
//
//   - enqueue due occurrences (one-time, delayed and cron jobs);
//   - promote runs whose retry backoff elapsed;
//   - move Redis-deferred runs back to the ready set;
//   - recover runs whose worker lease expired (crashed workers);
//   - mark silent workers offline;
//   - reconcile PostgreSQL with Redis (re-publish lost queue entries);
//   - apply retention to events and sessions.
//
// Several scheduler replicas can run; one is leader at a time. Leadership is a
// PostgreSQL session advisory lock held on a dedicated connection: if the
// leader crashes or loses its database connection, the lock is released by
// PostgreSQL itself and another replica takes over on its next tick. Note that
// correctness does not depend on there being a single leader — every loop is
// safe to run concurrently (SKIP LOCKED, conditional updates and the unique
// occurrence index) — leadership only avoids wasted work.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/taskflow/taskflow/internal/domain"
	"github.com/taskflow/taskflow/internal/engine"
	"github.com/taskflow/taskflow/internal/metrics"
	"github.com/taskflow/taskflow/internal/queue"
	"github.com/taskflow/taskflow/internal/store"
)

const leaderLockKey = 7_201_551_002

// Config controls loop intervals.
type Config struct {
	Tick               time.Duration
	BatchSize          int
	ReconcileInterval  time.Duration
	WorkerOfflineAfter time.Duration
}

// Scheduler is one scheduler process.
type Scheduler struct {
	ID       uuid.UUID
	cfg      Config
	eng      *engine.Engine
	pool     *pgxpool.Pool
	q        *queue.Queue
	log      *slog.Logger
	hostname string
	started  time.Time

	leaderConn *pgxpool.Conn
	lastRun    map[string]time.Time
}

func New(cfg Config, eng *engine.Engine, pool *pgxpool.Pool, q *queue.Queue, log *slog.Logger) *Scheduler {
	host, _ := os.Hostname()
	s := &Scheduler{ID: uuid.New(), cfg: cfg, eng: eng, pool: pool, q: q, hostname: host,
		started: time.Now().UTC(), lastRun: map[string]time.Time{}}
	s.log = log.With("scheduler_id", s.ID.String())
	return s
}

// IsLeader reports whether this instance currently holds leadership.
func (s *Scheduler) IsLeader() bool { return s.leaderConn != nil }

// Run loops until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) error {
	s.log.Info("scheduler started", "tick", s.cfg.Tick)
	t := time.NewTicker(s.cfg.Tick)
	defer t.Stop()
	defer s.shutdown()
	for {
		s.iterate(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func (s *Scheduler) iterate(ctx context.Context) {
	s.ensureLeadership(ctx)
	inst := &domain.SchedulerInstance{ID: s.ID, Hostname: s.hostname, IsLeader: s.IsLeader(),
		StartedAt: s.started, LastHeartbeatAt: time.Now().UTC()}
	if s.IsLeader() {
		start := time.Now()
		enqueued := s.Tick(ctx)
		ms := int(time.Since(start).Milliseconds())
		now := time.Now().UTC()
		inst.LastTickAt, inst.LastTickMS, inst.LastTickEnqueued = &now, &ms, &enqueued
		metrics.SchedulerTick.Observe(time.Since(start).Seconds())
	}
	if s.due("heartbeat", 2*time.Second) || inst.LastTickAt != nil {
		if err := store.UpsertSchedulerInstance(ctx, s.pool, inst); err != nil && ctx.Err() == nil {
			s.log.Warn("scheduler heartbeat failed", "err", err)
		}
	}
}

// ensureLeadership acquires or verifies the advisory lock.
func (s *Scheduler) ensureLeadership(ctx context.Context) {
	if s.leaderConn != nil {
		pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := s.leaderConn.Ping(pingCtx)
		cancel()
		if err == nil {
			return
		}
		s.log.Warn("lost leader connection; stepping down", "err", err)
		s.dropLeaderConn()
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return
	}
	var ok bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", leaderLockKey).Scan(&ok); err != nil || !ok {
		conn.Release()
		if s.IsLeader() {
			metrics.SchedulerLeader.Set(0)
		}
		return
	}
	s.leaderConn = conn
	metrics.SchedulerLeader.Set(1)
	s.log.Info("acquired scheduler leadership")
}

func (s *Scheduler) dropLeaderConn() {
	if s.leaderConn == nil {
		return
	}
	// Closing the session releases the advisory lock even if unlock fails.
	_ = s.leaderConn.Conn().Close(context.Background())
	s.leaderConn.Release()
	s.leaderConn = nil
	metrics.SchedulerLeader.Set(0)
}

func (s *Scheduler) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if s.leaderConn != nil {
		_, _ = s.leaderConn.Exec(ctx, "SELECT pg_advisory_unlock($1)", leaderLockKey)
		s.dropLeaderConn()
	}
	_ = store.StopSchedulerInstance(ctx, s.pool, s.ID, time.Now().UTC())
	s.log.Info("scheduler stopped")
}

func (s *Scheduler) due(name string, every time.Duration) bool {
	now := time.Now()
	if now.Sub(s.lastRun[name]) < every {
		return false
	}
	s.lastRun[name] = now
	return true
}

// Tick runs one leader iteration and returns the number of runs enqueued.
// Exported so tests can drive the scheduler deterministically.
func (s *Scheduler) Tick(ctx context.Context) int {
	enqueued := 0
	// Drain a backlog in batches, bounded so one tick cannot run forever.
	for i := 0; i < 20; i++ {
		n, err := s.eng.ScheduleDue(ctx, s.cfg.BatchSize)
		if err != nil {
			s.logErr(ctx, "schedule due jobs", err)
			break
		}
		enqueued += n
		if n < s.cfg.BatchSize {
			break
		}
	}
	if n, err := s.eng.PromoteRetries(ctx, s.cfg.BatchSize); err != nil {
		s.logErr(ctx, "promote retries", err)
	} else if n > 0 {
		s.log.Debug("promoted retries", "count", n)
	}
	if s.q != nil {
		if _, err := s.q.PromoteDelayed(ctx, time.Now(), 1000); err != nil && ctx.Err() == nil {
			metrics.RedisErrors.WithLabelValues("promote_delayed").Inc()
		}
	}
	if s.due("reaper", 2*time.Second) {
		if n, err := s.eng.RecoverExpiredLeases(ctx, 100); err != nil {
			s.logErr(ctx, "recover expired leases", err)
		} else if n > 0 {
			s.log.Warn("recovered runs with expired leases", "count", n)
		}
	}
	if s.due("workers", 5*time.Second) {
		s.markOfflineWorkers(ctx)
	}
	if s.due("reconcile", s.cfg.ReconcileInterval) {
		if n, err := s.eng.Reconcile(ctx, s.cfg.ReconcileInterval, 1000); err != nil {
			if ctx.Err() == nil {
				s.log.Warn("reconcile with redis failed", "err", err)
			}
		} else if n > 0 {
			s.log.Info("re-published queued runs to redis", "count", n)
		}
	}
	if s.due("depth", 5*time.Second) {
		s.observeDepth(ctx)
	}
	if s.due("retention", time.Hour) {
		s.retention(ctx)
	}
	return enqueued
}

func (s *Scheduler) markOfflineWorkers(ctx context.Context) {
	cutoff := time.Now().UTC().Add(-s.cfg.WorkerOfflineAfter)
	ws, err := store.MarkSilentWorkersOffline(ctx, s.pool, cutoff)
	if err != nil {
		s.logErr(ctx, "mark offline workers", err)
		return
	}
	for _, w := range ws {
		id := w.ID
		s.eng.RecordEvent(ctx, s.pool, domain.Event{WorkerID: &id, Type: "worker.offline", Severity: domain.SeverityWarning,
			Message: fmt.Sprintf("Worker %s on %s stopped heartbeating at %s and was marked offline",
				id.String()[:8], w.Hostname, w.LastHeartbeatAt.Format(time.RFC3339))})
		s.log.Warn("worker marked offline", "worker_id", id, "last_heartbeat", w.LastHeartbeatAt)
	}
}

func (s *Scheduler) observeDepth(ctx context.Context) {
	q, _, rw, err := store.GlobalQueueCounts(ctx, s.pool)
	if err == nil {
		metrics.QueueDepth.WithLabelValues("postgres_queued").Set(float64(q))
		metrics.QueueDepth.WithLabelValues("postgres_retry_wait").Set(float64(rw))
	}
	if s.q != nil {
		if ready, delayed, err := s.q.Depth(ctx); err == nil {
			metrics.QueueDepth.WithLabelValues("redis_ready").Set(float64(ready))
			metrics.QueueDepth.WithLabelValues("redis_delayed").Set(float64(delayed))
		}
	}
}

func (s *Scheduler) retention(ctx context.Context) {
	now := time.Now().UTC()
	if n, err := store.DeleteOldEvents(ctx, s.pool, now); err != nil {
		s.logErr(ctx, "event retention", err)
	} else if n > 0 {
		s.log.Info("deleted old events", "count", n)
	}
	if _, err := store.DeleteExpiredSessions(ctx, s.pool, now); err != nil {
		s.logErr(ctx, "session retention", err)
	}
	if _, err := store.DeleteOldWorkers(ctx, s.pool, now.AddDate(0, 0, -7)); err != nil {
		s.logErr(ctx, "worker retention", err)
	}
}

func (s *Scheduler) logErr(ctx context.Context, op string, err error) {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return
	}
	s.log.Error(op+" failed", "err", err)
}
