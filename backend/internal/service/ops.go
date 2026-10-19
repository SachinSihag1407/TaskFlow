package service

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/taskflow/taskflow/internal/db"
	"github.com/taskflow/taskflow/internal/domain"
	"github.com/taskflow/taskflow/internal/engine"
	"github.com/taskflow/taskflow/internal/queue"
	"github.com/taskflow/taskflow/internal/schedule"
	"github.com/taskflow/taskflow/internal/store"
)

// Ops serves read models and admin operations for the dashboard, workers,
// executions, dead-letter queue, health, settings and audit pages.
type Ops struct {
	pool         *pgxpool.Pool
	eng          *engine.Engine
	q            *queue.Queue
	staleAfter   time.Duration
	offlineAfter time.Duration
	now          func() time.Time
}

func NewOps(pool *pgxpool.Pool, eng *engine.Engine, q *queue.Queue, staleAfter, offlineAfter time.Duration) *Ops {
	return &Ops{pool: pool, eng: eng, q: q, staleAfter: staleAfter, offlineAfter: offlineAfter,
		now: func() time.Time { return time.Now().UTC() }}
}

// ---- Settings -------------------------------------------------------------

func (o *Ops) Settings(ctx context.Context, p *domain.Principal) (*domain.Settings, error) {
	return store.GetSettings(ctx, o.pool, p.TenantID)
}

func (o *Ops) UpdateSettings(ctx context.Context, p *domain.Principal, in domain.Settings) (*domain.Settings, error) {
	v := &domain.Validator{}
	v.Check(in.DefaultMaxAttempts >= 1 && in.DefaultMaxAttempts <= 25, "default_max_attempts", "Must be between 1 and 25.")
	v.Check(in.DefaultTimeoutSeconds >= 1 && in.DefaultTimeoutSeconds <= 86400, "default_timeout_seconds", "Must be between 1 and 86400 seconds.")
	v.Check(in.DefaultBackoffBaseSeconds >= 1 && in.DefaultBackoffBaseSeconds <= 3600, "default_backoff_base_seconds", "Must be between 1 and 3600 seconds.")
	v.Check(in.DefaultBackoffMaxSeconds >= 1 && in.DefaultBackoffMaxSeconds <= 86400, "default_backoff_max_seconds", "Must be between 1 and 86400 seconds.")
	v.Check(in.DefaultBackoffMaxSeconds >= in.DefaultBackoffBaseSeconds, "default_backoff_max_seconds", "Must be at least the base delay.")
	v.Check(in.DefaultPriority >= 1 && in.DefaultPriority <= 10, "default_priority", "Must be between 1 and 10.")
	v.Check(in.MaxConcurrentRuns >= 0 && in.MaxConcurrentRuns <= 10000, "max_concurrent_runs", "Must be between 0 (unlimited) and 10000.")
	v.Check(in.EventRetentionDays >= 1 && in.EventRetentionDays <= 365, "event_retention_days", "Must be between 1 and 365 days.")
	if _, err := schedule.LoadTimezone(in.DefaultTimezone); err != nil {
		v.Add("default_timezone", "Unknown timezone.")
	}
	if err := v.Err(); err != nil {
		return nil, err
	}
	in.TenantID = p.TenantID
	err := db.WithTx(ctx, o.pool, func(tx pgx.Tx) error {
		before, err := store.GetSettings(ctx, tx, p.TenantID)
		if err != nil {
			return err
		}
		if err := store.UpdateSettings(ctx, tx, &in, p.UserID, o.now()); err != nil {
			return err
		}
		return o.eng.Audit(ctx, tx, p, "settings.update", "tenant", p.TenantID.String(), map[string]any{
			"before": before, "after": in})
	})
	if err != nil {
		return nil, err
	}
	return store.GetSettings(ctx, o.pool, p.TenantID)
}

// ---- Workers --------------------------------------------------------------

// effectiveStatus derives STALE for workers whose heartbeat is overdue but
// that the scheduler has not yet declared OFFLINE.
func (o *Ops) effectiveStatus(w *domain.Worker, now time.Time) domain.WorkerState {
	if w.Status != domain.WorkerOffline && now.Sub(w.LastHeartbeatAt) > o.staleAfter {
		return domain.WorkerStale
	}
	return w.Status
}

// WorkerView is a worker with its derived status.
type WorkerView struct {
	*domain.Worker
	SecondsSinceHeartbeat float64 `json:"seconds_since_heartbeat"`
}

func (o *Ops) Workers(ctx context.Context) ([]WorkerView, error) {
	now := o.now()
	ws, err := store.ListWorkers(ctx, o.pool, now.Add(-24*time.Hour))
	if err != nil {
		return nil, err
	}
	out := make([]WorkerView, 0, len(ws))
	for _, w := range ws {
		w.Status = o.effectiveStatus(w, now)
		if w.Status == domain.WorkerOffline || w.Status == domain.WorkerStale {
			w.ActiveRuns = 0 // last reported value is not trustworthy
		}
		out = append(out, WorkerView{Worker: w, SecondsSinceHeartbeat: now.Sub(w.LastHeartbeatAt).Seconds()})
	}
	return out, nil
}

// WorkerDetail is the worker page model. Run and attempt data are scoped to
// the caller's workspace: workers are shared infrastructure, job data is not.
type WorkerDetail struct {
	Worker      WorkerView        `json:"worker"`
	RunningRuns []*domain.Run     `json:"running_runs"`
	Recent      []*domain.Attempt `json:"recent_attempts"`
	Events      []*domain.Event   `json:"events"`
}

func (o *Ops) Worker(ctx context.Context, p *domain.Principal, id uuid.UUID) (*WorkerDetail, error) {
	w, err := store.GetWorker(ctx, o.pool, id)
	if err != nil {
		return nil, err
	}
	now := o.now()
	w.Status = o.effectiveStatus(w, now)
	running, err := store.RunningRunsForWorker(ctx, o.pool, p.TenantID, id)
	if err != nil {
		return nil, err
	}
	recent, _, err := store.ListAttempts(ctx, o.pool, store.AttemptFilter{TenantID: p.TenantID, WorkerID: &id, Page: store.Page{PageSize: 25}})
	if err != nil {
		return nil, err
	}
	events, err := store.ListEvents(ctx, o.pool, store.EventFilter{TenantID: p.TenantID, WorkerID: &id, IncludePlatform: true, Limit: 25})
	if err != nil {
		return nil, err
	}
	return &WorkerDetail{
		Worker:      WorkerView{Worker: w, SecondsSinceHeartbeat: now.Sub(w.LastHeartbeatAt).Seconds()},
		RunningRuns: nonNil(running), Recent: nonNil(recent), Events: nonNil(events),
	}, nil
}

// DrainWorker asks a worker to stop taking new runs and shut down gracefully.
func (o *Ops) DrainWorker(ctx context.Context, p *domain.Principal, id uuid.UUID) error {
	return db.WithTx(ctx, o.pool, func(tx pgx.Tx) error {
		if err := store.RequestDrain(ctx, tx, id, o.now()); err != nil {
			return err
		}
		o.eng.RecordEvent(ctx, tx, domain.Event{WorkerID: &id, Type: "worker.drain_requested", Severity: domain.SeverityInfo,
			Message: "Drain requested by a platform administrator"})
		return o.eng.Audit(ctx, tx, p, "worker.drain", "worker", id.String(), nil)
	})
}

// ---- Analytics ------------------------------------------------------------

// RangeSpec maps a dashboard time range to a window and bucket width.
type RangeSpec struct {
	Window time.Duration
	Bucket time.Duration
}

var Ranges = map[string]RangeSpec{
	"1h":  {time.Hour, 2 * time.Minute},
	"6h":  {6 * time.Hour, 10 * time.Minute},
	"24h": {24 * time.Hour, time.Hour},
	"7d":  {7 * 24 * time.Hour, 6 * time.Hour},
	"30d": {30 * 24 * time.Hour, 24 * time.Hour},
}

// Overview is the dashboard model. Every number comes from the database.
type Overview struct {
	Range       string                   `json:"range"`
	GeneratedAt time.Time                `json:"generated_at"`
	JobCounts   map[string]int           `json:"job_counts"`
	TotalJobs   int                      `json:"total_jobs"`
	Queued      int                      `json:"queued"`
	Running     int                      `json:"running"`
	RetryWait   int                      `json:"retry_wait"`
	DeadLetter  int                      `json:"dead_letter"`
	Outcomes    *store.OutcomeStats      `json:"outcomes"`
	SuccessRate *float64                 `json:"success_rate"`
	Throughput  []store.ThroughputPoint  `json:"throughput"`
	ByHandler   []store.HandlerBreakdown `json:"by_handler"`
	Workers     WorkerSummary            `json:"workers"`
	Queue       QueueSummary             `json:"queue"`
	Recent      []*domain.Attempt        `json:"recent_attempts"`
	Events      []*domain.Event          `json:"events"`
}

type WorkerSummary struct {
	Online   int `json:"online"`
	Draining int `json:"draining"`
	Stale    int `json:"stale"`
	Offline  int `json:"offline"`
	Capacity int `json:"capacity"`
	Busy     int `json:"busy"`
}

type QueueSummary struct {
	RedisAvailable bool   `json:"redis_available"`
	RedisReady     *int64 `json:"redis_ready"`
	RedisDelayed   *int64 `json:"redis_delayed"`
}

func (o *Ops) Overview(ctx context.Context, p *domain.Principal, rng string) (*Overview, error) {
	spec, ok := Ranges[rng]
	if !ok {
		rng, spec = "24h", Ranges["24h"]
	}
	now := o.now()
	since := now.Add(-spec.Window)
	ov := &Overview{Range: rng, GeneratedAt: now}
	var err error
	if ov.JobCounts, err = store.JobStatusCounts(ctx, o.pool, p.TenantID); err != nil {
		return nil, err
	}
	for _, n := range ov.JobCounts {
		ov.TotalJobs += n
	}
	if ov.Queued, ov.Running, ov.RetryWait, err = store.RunStatusCounts(ctx, o.pool, p.TenantID); err != nil {
		return nil, err
	}
	if err := o.pool.QueryRow(ctx, `SELECT count(*) FROM job_runs WHERE tenant_id = $1 AND dead_lettered_at IS NOT NULL AND replayed_at IS NULL`,
		p.TenantID).Scan(&ov.DeadLetter); err != nil {
		return nil, err
	}
	if ov.Outcomes, err = store.Outcomes(ctx, o.pool, p.TenantID, since); err != nil {
		return nil, err
	}
	if total := ov.Outcomes.RunsSucceeded + ov.Outcomes.RunsFailed; total > 0 {
		r := float64(ov.Outcomes.RunsSucceeded) / float64(total)
		ov.SuccessRate = &r
	}
	if ov.Throughput, err = store.Throughput(ctx, o.pool, p.TenantID, since, now, spec.Bucket); err != nil {
		return nil, err
	}
	if ov.ByHandler, err = store.OutcomesByHandler(ctx, o.pool, p.TenantID, since); err != nil {
		return nil, err
	}
	ov.ByHandler = nonNil(ov.ByHandler)
	workers, err := o.Workers(ctx)
	if err != nil {
		return nil, err
	}
	for _, w := range workers {
		switch w.Status {
		case domain.WorkerOnline:
			ov.Workers.Online++
			ov.Workers.Capacity += w.Concurrency
			ov.Workers.Busy += w.ActiveRuns
		case domain.WorkerDraining:
			ov.Workers.Draining++
			ov.Workers.Busy += w.ActiveRuns
		case domain.WorkerStale:
			ov.Workers.Stale++
		default:
			ov.Workers.Offline++
		}
	}
	ov.Queue = o.queueSummary(ctx)
	recent, _, err := store.ListAttempts(ctx, o.pool, store.AttemptFilter{TenantID: p.TenantID, Page: store.Page{PageSize: 8}})
	if err != nil {
		return nil, err
	}
	ov.Recent = nonNil(recent)
	events, err := store.ListEvents(ctx, o.pool, store.EventFilter{TenantID: p.TenantID, IncludePlatform: true, Limit: 12})
	if err != nil {
		return nil, err
	}
	ov.Events = nonNil(events)
	return ov, nil
}

func (o *Ops) queueSummary(ctx context.Context) QueueSummary {
	qs := QueueSummary{}
	if o.q == nil {
		return qs
	}
	c, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	ready, delayed, err := o.q.Depth(c)
	if err != nil {
		return qs
	}
	qs.RedisAvailable, qs.RedisReady, qs.RedisDelayed = true, &ready, &delayed
	return qs
}

// ---- Health ---------------------------------------------------------------

// ComponentHealth is the health of one dependency.
type ComponentHealth struct {
	Name      string  `json:"name"`
	Status    string  `json:"status"` // healthy | degraded | down | disabled
	LatencyMS float64 `json:"latency_ms"`
	Detail    string  `json:"detail"`
}

// Health is the system health page model.
type Health struct {
	Status     string                      `json:"status"`
	CheckedAt  time.Time                   `json:"checked_at"`
	Components []ComponentHealth           `json:"components"`
	Schedulers []*domain.SchedulerInstance `json:"schedulers"`
	Workers    WorkerSummary               `json:"workers"`
	Queue      struct {
		PostgresQueued    int    `json:"postgres_queued"`
		PostgresRunning   int    `json:"postgres_running"`
		PostgresRetryWait int    `json:"postgres_retry_wait"`
		RedisReady        *int64 `json:"redis_ready"`
		RedisDelayed      *int64 `json:"redis_delayed"`
	} `json:"queue"`
	Rates struct {
		WindowMinutes int      `json:"window_minutes"`
		Attempts      int      `json:"attempts"`
		Failed        int      `json:"failed"`
		Retries       int      `json:"retries"`
		ErrorRate     *float64 `json:"error_rate"`
		RetryRate     *float64 `json:"retry_rate"`
	} `json:"rates"`
}

// Health checks every dependency. It never fails as a whole: each component
// reports its own status so the page can show exactly what is degraded.
func (o *Ops) Health(ctx context.Context) *Health {
	h := &Health{CheckedAt: o.now(), Schedulers: []*domain.SchedulerInstance{}}
	pgStatus := ComponentHealth{Name: "postgres", Status: "healthy", Detail: "Primary datastore"}
	start := time.Now()
	c, cancel := context.WithTimeout(ctx, 2*time.Second)
	err := o.pool.Ping(c)
	cancel()
	pgStatus.LatencyMS = float64(time.Since(start).Microseconds()) / 1000
	if err != nil {
		pgStatus.Status, pgStatus.Detail = "down", "Database unreachable"
	}
	h.Components = append(h.Components, ComponentHealth{Name: "api", Status: "healthy", Detail: "Serving requests"}, pgStatus)

	redis := ComponentHealth{Name: "redis", Status: "healthy", Detail: "Queue and pub/sub"}
	if o.q == nil {
		redis.Status, redis.Detail = "disabled", "Not used in this deployment — workers poll PostgreSQL and the UI polls for updates"
	} else {
		start = time.Now()
		c, cancel := context.WithTimeout(ctx, time.Second)
		err := o.q.Ping(c)
		cancel()
		redis.LatencyMS = float64(time.Since(start).Microseconds()) / 1000
		if err != nil {
			redis.Status, redis.Detail = "degraded", "Unreachable — workers fall back to PostgreSQL polling and live updates pause"
		} else if ready, delayed, err := o.q.Depth(ctx); err == nil {
			h.Queue.RedisReady, h.Queue.RedisDelayed = &ready, &delayed
		}
	}
	h.Components = append(h.Components, redis)

	if pgStatus.Status == "healthy" {
		now := o.now()
		scheds, err := store.ListSchedulerInstances(ctx, o.pool, now.Add(-5*time.Minute))
		if err == nil {
			h.Schedulers = nonNil(scheds)
		}
		sched := ComponentHealth{Name: "scheduler", Status: "down", Detail: "No scheduler instance has reported in the last 5 minutes"}
		for _, s := range h.Schedulers {
			if s.IsLeader && s.StoppedAt == nil && now.Sub(s.LastHeartbeatAt) < 15*time.Second {
				sched.Status = "healthy"
				sched.Detail = "Leader " + s.Hostname
				if s.LastTickMS != nil {
					sched.LatencyMS = float64(*s.LastTickMS)
				}
				break
			}
			if s.StoppedAt == nil && now.Sub(s.LastHeartbeatAt) < 15*time.Second {
				sched.Status, sched.Detail = "degraded", "Instances are alive but none holds leadership"
			}
		}
		h.Components = append(h.Components, sched)

		if workers, err := o.Workers(ctx); err == nil {
			for _, w := range workers {
				switch w.Status {
				case domain.WorkerOnline:
					h.Workers.Online++
					h.Workers.Capacity += w.Concurrency
					h.Workers.Busy += w.ActiveRuns
				case domain.WorkerDraining:
					h.Workers.Draining++
				case domain.WorkerStale:
					h.Workers.Stale++
				default:
					h.Workers.Offline++
				}
			}
		}
		wc := ComponentHealth{Name: "workers", Status: "healthy"}
		switch {
		case h.Workers.Online == 0:
			wc.Status, wc.Detail = "down", "No online workers: queued runs will not execute"
		case h.Workers.Stale > 0:
			wc.Status, wc.Detail = "degraded", "Some workers have missed heartbeats"
		default:
			wc.Detail = "All reporting workers are healthy"
		}
		h.Components = append(h.Components, wc)

		h.Queue.PostgresQueued, h.Queue.PostgresRunning, h.Queue.PostgresRetryWait, _ = store.GlobalQueueCounts(ctx, o.pool)
		h.Rates.WindowMinutes = 60
		total, failed, retries, err := store.GlobalErrorRates(ctx, o.pool, now.Add(-time.Hour))
		if err == nil {
			h.Rates.Attempts, h.Rates.Failed, h.Rates.Retries = total, failed, retries
			if total > 0 {
				er := float64(failed) / float64(total)
				rr := float64(retries) / float64(total)
				h.Rates.ErrorRate, h.Rates.RetryRate = &er, &rr
			}
		}
	}

	h.Status = "healthy"
	for _, c := range h.Components {
		if c.Status == "down" && (c.Name == "postgres" || c.Name == "api") {
			h.Status = "down"
			break
		}
		if c.Status != "healthy" && c.Status != "disabled" {
			h.Status = "degraded"
		}
	}
	return h
}

// ---- Executions, runs, events, audit --------------------------------------

func (o *Ops) Attempts(ctx context.Context, p *domain.Principal, f store.AttemptFilter) ([]*domain.Attempt, int, error) {
	f.TenantID = p.TenantID
	items, total, err := store.ListAttempts(ctx, o.pool, f)
	return nonNil(items), total, err
}

// AttemptDetail is one execution attempt with its run and logs.
type AttemptDetail struct {
	Attempt *domain.Attempt  `json:"attempt"`
	Run     *domain.Run      `json:"run"`
	Logs    []domain.LogLine `json:"logs"`
}

func (o *Ops) Attempt(ctx context.Context, p *domain.Principal, id uuid.UUID) (*AttemptDetail, error) {
	a, err := store.GetAttempt(ctx, o.pool, p.TenantID, id)
	if err != nil {
		return nil, err
	}
	r, err := store.GetRun(ctx, o.pool, p.TenantID, a.RunID)
	if err != nil {
		return nil, err
	}
	logs, err := store.ListLogs(ctx, o.pool, p.TenantID, id, 0, store.MaxLogLinesPerAttempt)
	if err != nil {
		return nil, err
	}
	return &AttemptDetail{Attempt: a, Run: r, Logs: nonNil(logs)}, nil
}

func (o *Ops) Logs(ctx context.Context, p *domain.Principal, attemptID uuid.UUID, after int64) ([]domain.LogLine, error) {
	if _, err := store.GetAttempt(ctx, o.pool, p.TenantID, attemptID); err != nil {
		return nil, err
	}
	logs, err := store.ListLogs(ctx, o.pool, p.TenantID, attemptID, after, store.MaxLogLinesPerAttempt)
	return nonNil(logs), err
}

func (o *Ops) Runs(ctx context.Context, p *domain.Principal, f store.RunFilter) ([]*domain.Run, int, error) {
	f.TenantID = p.TenantID
	items, total, err := store.ListRuns(ctx, o.pool, f)
	return nonNil(items), total, err
}

// RunDetail is one run with all its attempts.
type RunDetail struct {
	Run      *domain.Run          `json:"run"`
	Attempts []*domain.Attempt    `json:"attempts"`
	Events   []*domain.Event      `json:"events"`
	Audit    []*domain.AuditEvent `json:"audit"`
}

func (o *Ops) Run(ctx context.Context, p *domain.Principal, id uuid.UUID) (*RunDetail, error) {
	r, err := store.GetRun(ctx, o.pool, p.TenantID, id)
	if err != nil {
		return nil, err
	}
	attempts, _, err := store.ListAttempts(ctx, o.pool, store.AttemptFilter{TenantID: p.TenantID, RunID: &id, Sort: "started_at", Page: store.Page{PageSize: 50}})
	if err != nil {
		return nil, err
	}
	events, err := store.ListEvents(ctx, o.pool, store.EventFilter{TenantID: p.TenantID, RunID: &id, Limit: 50})
	if err != nil {
		return nil, err
	}
	audit, _, err := store.ListAudit(ctx, o.pool, store.AuditFilter{TenantID: p.TenantID, TargetType: "run", TargetID: id.String(), Page: store.Page{PageSize: 20}})
	if err != nil {
		return nil, err
	}
	return &RunDetail{Run: r, Attempts: nonNil(attempts), Events: nonNil(events), Audit: nonNil(audit)}, nil
}

func (o *Ops) Events(ctx context.Context, p *domain.Principal, f store.EventFilter) ([]*domain.Event, error) {
	f.TenantID = p.TenantID
	f.IncludePlatform = true
	items, err := store.ListEvents(ctx, o.pool, f)
	return nonNil(items), err
}

func (o *Ops) Audit(ctx context.Context, p *domain.Principal, f store.AuditFilter) ([]*domain.AuditEvent, int, error) {
	f.TenantID = p.TenantID
	items, total, err := store.ListAudit(ctx, o.pool, f)
	return nonNil(items), total, err
}

// Notifications returns recent warning/error events and the unread count.
type Notifications struct {
	Unread int             `json:"unread"`
	Items  []*domain.Event `json:"items"`
}

func (o *Ops) Notifications(ctx context.Context, p *domain.Principal, seenAt *time.Time) (*Notifications, error) {
	items, err := store.ListEvents(ctx, o.pool, store.EventFilter{TenantID: p.TenantID,
		Severities: []string{"warning", "error"}, Limit: 20})
	if err != nil {
		return nil, err
	}
	since := o.now().Add(-7 * 24 * time.Hour)
	if seenAt != nil && seenAt.After(since) {
		since = *seenAt
	}
	n, err := store.CountEventsSince(ctx, o.pool, p.TenantID, []string{"warning", "error"}, since)
	if err != nil {
		return nil, err
	}
	return &Notifications{Unread: n, Items: nonNil(items)}, nil
}

// Tenant returns the caller's workspace.
func (o *Ops) Tenant(ctx context.Context, p *domain.Principal) (*domain.Tenant, error) {
	return store.GetTenant(ctx, o.pool, p.TenantID)
}
