// Package worker is the execution runtime: it registers the process, sends
// heartbeats, claims runs (from Redis, or directly from PostgreSQL when Redis
// is unavailable), executes handlers with bounded concurrency, timeouts and
// cooperative cancellation, renews leases, and shuts down gracefully.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/taskflow/taskflow/internal/domain"
	"github.com/taskflow/taskflow/internal/engine"
	"github.com/taskflow/taskflow/internal/handlers"
	"github.com/taskflow/taskflow/internal/metrics"
	"github.com/taskflow/taskflow/internal/queue"
	"github.com/taskflow/taskflow/internal/store"
)

// Causes attached to an attempt's context when it is cancelled. The worker
// inspects them to classify the outcome.
var (
	errCancelRequested = errors.New("cancelled by operator")
	errShutdown        = errors.New("worker shutting down")
	errLeaseLost       = errors.New("lease lost: run reclaimed after this worker was presumed dead")
	errTimeout         = errors.New("attempt timed out")
)

// Config controls the runtime.
type Config struct {
	Concurrency   int
	Lease         time.Duration
	Heartbeat     time.Duration
	ShutdownGrace time.Duration
	PollInterval  time.Duration
	Version       string
}

// Worker is one worker process.
type Worker struct {
	ID       uuid.UUID
	cfg      Config
	eng      *engine.Engine
	pool     *pgxpool.Pool
	q        *queue.Queue // nil disables Redis entirely
	reg      *handlers.Registry
	log      *slog.Logger
	hostname string

	active       sync.Map // run id -> context.CancelCauseFunc
	activeCount  atomic.Int32
	draining     atomic.Bool
	redisHealthy atomic.Bool
	redisRetryAt atomic.Int64
	drainOnce    sync.Once
	drainCh      chan struct{}
}

func New(cfg Config, eng *engine.Engine, pool *pgxpool.Pool, q *queue.Queue, reg *handlers.Registry, log *slog.Logger) *Worker {
	host, _ := os.Hostname()
	w := &Worker{
		ID: uuid.New(), cfg: cfg, eng: eng, pool: pool, q: q, reg: reg, hostname: host,
		log: log, drainCh: make(chan struct{}),
	}
	w.log = log.With("worker_id", w.ID.String())
	w.redisHealthy.Store(q != nil)
	return w
}

// Drain stops claiming new work; Run returns once in-flight work finishes.
func (w *Worker) Drain() {
	w.drainOnce.Do(func() {
		w.draining.Store(true)
		close(w.drainCh)
	})
}

// ActiveRuns returns the number of attempts currently executing.
func (w *Worker) ActiveRuns() int { return int(w.activeCount.Load()) }

// Run executes until ctx is cancelled (SIGTERM) or a drain is requested, then
// shuts down gracefully:
//
//  1. stop claiming and report DRAINING;
//  2. let in-flight attempts finish for up to ShutdownGrace;
//  3. cancel whatever is still running; those attempts are recorded as
//     interrupted and retried promptly by another worker;
//  4. report OFFLINE.
//
// If the process is killed instead (SIGKILL, OOM, power loss) none of this
// runs; the scheduler's lease reaper recovers the abandoned runs.
func (w *Worker) Run(ctx context.Context) error {
	now := time.Now().UTC()
	if err := store.RegisterWorker(ctx, w.pool, &domain.Worker{
		ID: w.ID, Hostname: w.hostname, PID: os.Getpid(), Version: w.cfg.Version,
		Concurrency: w.cfg.Concurrency, Handlers: w.reg.Names(), StartedAt: now,
	}); err != nil {
		return fmt.Errorf("register worker: %w", err)
	}
	w.eng.RecordEvent(ctx, w.pool, domain.Event{WorkerID: &w.ID, Type: "worker.online", Severity: domain.SeverityInfo,
		Message: fmt.Sprintf("Worker %s on %s started (concurrency %d)", w.ID.String()[:8], w.hostname, w.cfg.Concurrency)})
	metrics.WorkerCapacity.Set(float64(w.cfg.Concurrency))
	w.log.Info("worker started", "hostname", w.hostname, "concurrency", w.cfg.Concurrency, "handlers", w.reg.Names())

	// Background loops outlive the claim loop so heartbeats and cancellation
	// keep working while in-flight attempts drain.
	bgCtx, stopBG := context.WithCancel(context.WithoutCancel(ctx))
	defer stopBG()
	go w.heartbeatLoop(bgCtx)
	if w.q != nil {
		go w.cancelListener(bgCtx)
	}

	claimCtx, stopClaiming := context.WithCancel(ctx)
	go func() {
		select {
		case <-w.drainCh:
		case <-ctx.Done():
		}
		stopClaiming()
	}()

	// Attempts get their own root so SIGTERM does not abort them instantly.
	execRoot, cancelExec := context.WithCancelCause(context.WithoutCancel(ctx))
	defer cancelExec(nil)
	var wg sync.WaitGroup
	w.claimLoop(claimCtx, execRoot, &wg)

	w.Drain()
	w.beat(bgCtx) // report DRAINING immediately
	w.log.Info("draining", "active", w.ActiveRuns(), "grace", w.cfg.ShutdownGrace)

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(w.cfg.ShutdownGrace):
		w.log.Warn("grace period elapsed; interrupting remaining attempts", "active", w.ActiveRuns())
		cancelExec(errShutdown)
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			w.log.Error("attempts did not stop after interruption; their leases will expire and be recovered")
		}
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := store.MarkWorkerStopped(stopCtx, w.pool, w.ID, time.Now().UTC()); err != nil {
		w.log.Warn("mark worker stopped failed", "err", err)
	}
	w.eng.RecordEvent(stopCtx, w.pool, domain.Event{WorkerID: &w.ID, Type: "worker.stopped", Severity: domain.SeverityInfo,
		Message: fmt.Sprintf("Worker %s on %s shut down gracefully", w.ID.String()[:8], w.hostname)})
	w.log.Info("worker stopped")
	return nil
}

func (w *Worker) claimLoop(ctx, execRoot context.Context, wg *sync.WaitGroup) {
	slots := make(chan struct{}, w.cfg.Concurrency)
	for {
		select {
		case <-ctx.Done():
			return
		case slots <- struct{}{}:
		}
		c, err := w.next(ctx)
		if err != nil || c == nil {
			<-slots
			if err != nil && ctx.Err() == nil {
				w.log.Warn("claim failed", "err", err)
				sleep(ctx, w.cfg.PollInterval)
			}
			continue
		}
		wg.Add(1)
		w.activeCount.Add(1)
		metrics.WorkerActiveRuns.Inc()
		go func() {
			defer func() {
				w.activeCount.Add(-1)
				metrics.WorkerActiveRuns.Dec()
				<-slots
				wg.Done()
			}()
			w.execute(execRoot, c)
		}()
	}
}

// next returns a claimed run, or nil if none was available.
func (w *Worker) next(ctx context.Context) (*engine.Claimed, error) {
	if w.q != nil && w.useRedis(ctx) {
		id, err := w.q.Pop(ctx, time.Second)
		switch {
		case errors.Is(err, queue.ErrEmpty):
			return nil, nil
		case err != nil:
			if ctx.Err() != nil {
				return nil, nil
			}
			metrics.RedisErrors.WithLabelValues("pop").Inc()
			w.markRedisDown(err)
			// fall through to the database path below
		default:
			return w.claimFromRedis(ctx, id)
		}
	}
	c, err := w.eng.ClaimNext(ctx, w.ID, w.reg.Names(), w.cfg.Lease)
	if errors.Is(err, engine.ErrNoWork) {
		metrics.ClaimResults.WithLabelValues("empty", "db").Inc()
		sleep(ctx, w.cfg.PollInterval)
		return nil, nil
	}
	if err != nil {
		metrics.ClaimResults.WithLabelValues("error", "db").Inc()
		return nil, err
	}
	metrics.ClaimResults.WithLabelValues("claimed", "db").Inc()
	return c, nil
}

func (w *Worker) claimFromRedis(ctx context.Context, id uuid.UUID) (*engine.Claimed, error) {
	supports := func(h string) bool { _, ok := w.reg.Get(h); return ok }
	c, err := w.eng.Claim(ctx, id, w.ID, w.cfg.Lease, supports)
	switch {
	case err == nil:
		metrics.ClaimResults.WithLabelValues("claimed", "redis").Inc()
		return c, nil
	case errors.Is(err, engine.ErrStale), errors.Is(err, engine.ErrPaused):
		// Duplicate/outdated entry or paused job: drop it. Resume re-publishes.
		metrics.ClaimResults.WithLabelValues("stale", "redis").Inc()
		return nil, nil
	case errors.Is(err, engine.ErrDeferred), errors.Is(err, engine.ErrUnsupported):
		metrics.ClaimResults.WithLabelValues("deferred", "redis").Inc()
		w.deferRun(ctx, id)
		return nil, nil
	default:
		metrics.ClaimResults.WithLabelValues("error", "redis").Inc()
		// Put it back so it is not lost while the database hiccups; the
		// reconciler would also restore it, just more slowly.
		w.deferRun(ctx, id)
		return nil, err
	}
}

func (w *Worker) deferRun(ctx context.Context, id uuid.UUID) {
	r, err := store.GetRunUnscoped(ctx, w.pool, id)
	if err != nil || r.Status != domain.RunQueued {
		return
	}
	at := r.CreatedAt
	if r.QueuedAt != nil {
		at = *r.QueuedAt
	}
	delay := time.Second + time.Duration(rand.IntN(1000))*time.Millisecond
	if err := w.q.Defer(ctx, queue.Item{RunID: id, Priority: r.Priority, QueuedAt: at}, delay); err != nil {
		metrics.RedisErrors.WithLabelValues("defer").Inc()
	}
}

func (w *Worker) useRedis(ctx context.Context) bool {
	if w.redisHealthy.Load() {
		return true
	}
	if time.Now().UnixMilli() < w.redisRetryAt.Load() {
		return false
	}
	pingCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := w.q.Ping(pingCtx); err != nil {
		w.redisRetryAt.Store(time.Now().Add(5 * time.Second).UnixMilli())
		return false
	}
	w.redisHealthy.Store(true)
	w.log.Info("redis reachable again; resuming queue-based dispatch")
	return true
}

func (w *Worker) markRedisDown(err error) {
	if w.redisHealthy.CompareAndSwap(true, false) {
		w.log.Warn("redis unavailable; falling back to polling PostgreSQL", "err", err)
	}
	w.redisRetryAt.Store(time.Now().Add(5 * time.Second).UnixMilli())
}

// execute runs one attempt end to end.
func (w *Worker) execute(root context.Context, c *engine.Claimed) {
	log := w.log.With("run_id", c.Run.ID.String(), "attempt", c.Attempt.AttemptNumber, "handler", c.Run.Handler)
	h, ok := w.reg.Get(c.Run.Handler)
	if !ok { // cannot happen: claims are filtered by supported handlers
		w.complete(log, c, engine.Outcome{Kind: engine.OutcomeFailure, Err: fmt.Errorf("handler %q is not registered", c.Run.Handler)})
		return
	}

	attemptCtx, cancel := context.WithCancelCause(root)
	defer cancel(nil)
	timeout := time.Duration(c.Run.TimeoutSeconds) * time.Second
	execCtx, cancelTimeout := context.WithTimeoutCause(attemptCtx, timeout, errTimeout)
	defer cancelTimeout()

	w.active.Store(c.Run.ID, cancel)
	defer w.active.Delete(c.Run.ID)

	alog := newAttemptLogger(w.pool, c.Attempt.ID, w.log)
	stopAux := make(chan struct{})
	auxDone := make(chan struct{})
	go func() {
		defer close(auxDone)
		w.leaseAndFlushLoop(execCtx, c, cancel, alog, stopAux, log)
	}()

	x := &handlers.Execution{
		RunID: c.Run.ID, JobID: c.Run.JobID, TenantID: c.Run.TenantID, AttemptID: c.Attempt.ID,
		Attempt: c.Attempt.AttemptNumber, MaxAttempts: c.Run.MaxAttempts, Payload: c.Run.Payload,
		Checkpoint: c.Run.Checkpoint, IdempotencyKey: c.Run.ID.String(), Log: alog,
		SaveCheckpoint: func(ctx context.Context, cp any) error {
			b, err := json.Marshal(cp)
			if err != nil {
				return err
			}
			return w.eng.SaveCheckpoint(ctx, c, b)
		},
	}
	if c.Attempt.AttemptNumber > 1 {
		alog.Info(fmt.Sprintf("retry attempt %d of %d", c.Attempt.AttemptNumber, c.Run.MaxAttempts))
	}
	result, err, panicked := safeExecute(execCtx, h, x)

	close(stopAux)
	<-auxDone

	out := classify(execCtx, result, err, panicked, timeout)
	if errors.Is(context.Cause(execCtx), errLeaseLost) && err != nil {
		// Another worker may already be running this run. Do not report.
		alog.Warn("stopped: this worker lost ownership of the run")
		alog.close(context.WithoutCancel(root))
		metrics.LeaseLost.Inc()
		log.Warn("abandoning attempt after lease loss")
		return
	}
	switch out.Kind {
	case engine.OutcomeSuccess:
		alog.Info("attempt succeeded")
	case engine.OutcomeCancelled:
		alog.Warn("attempt cancelled by operator")
	default:
		alog.Error(fmt.Sprintf("attempt failed (%s): %v", out.Kind, out.Err))
	}
	alog.close(context.WithoutCancel(root))
	w.complete(log, c, out)
}

// leaseAndFlushLoop renews the lease every Lease/3, propagates cancellation
// requests, and flushes attempt logs every second.
func (w *Worker) leaseAndFlushLoop(ctx context.Context, c *engine.Claimed, cancel context.CancelCauseFunc,
	alog *attemptLogger, stop <-chan struct{}, log *slog.Logger) {
	renew := time.NewTicker(w.cfg.Lease / 3)
	defer renew.Stop()
	flush := time.NewTicker(time.Second)
	defer flush.Stop()
	bg := context.WithoutCancel(ctx)
	done := ctx.Done()
	var doneAt time.Time
	for {
		select {
		case <-stop:
			return
		case <-done:
			// Timeout, cancel or shutdown fired. Well-behaved handlers return
			// promptly; done is nil-ed so this case fires once.
			doneAt, done = time.Now(), nil
		case <-flush.C:
			alog.flush(bg)
		case <-renew.C:
			if !doneAt.IsZero() && time.Since(doneAt) > w.cfg.Lease {
				// The handler ignores cancellation. Stop renewing so the lease
				// expires and the reaper takes the run back; this slot stays
				// busy until the handler returns, but the run is not stuck.
				if time.Since(doneAt) < w.cfg.Lease+w.cfg.Lease/3+time.Second {
					log.Error("handler did not stop after cancellation; letting its lease expire")
				}
				continue
			}
			rctx, rcancel := context.WithTimeout(bg, 5*time.Second)
			cancelRequested, err := w.eng.RenewLease(rctx, c, w.cfg.Lease)
			rcancel()
			switch {
			case errors.Is(err, engine.ErrLeaseLost):
				log.Warn("lease lost; stopping attempt")
				cancel(errLeaseLost)
			case err != nil:
				// Transient database error: keep running and retry. If it
				// persists past the lease, the reaper takes over and the fenced
				// completion write will be rejected.
				log.Warn("lease renewal failed", "err", err)
			case cancelRequested:
				cancel(errCancelRequested)
			}
		}
	}
}

// safeExecute runs the handler, converting a panic into an error so one bad
// handler cannot crash the worker and abandon every other in-flight attempt.
func safeExecute(ctx context.Context, h handlers.Handler, x *handlers.Execution) (res json.RawMessage, err error, panicked bool) {
	defer func() {
		if p := recover(); p != nil {
			stack := string(debug.Stack())
			if len(stack) > 1500 {
				stack = stack[:1500]
			}
			err = fmt.Errorf("handler panicked: %v", p)
			x.Log.Error(fmt.Sprintf("panic: %v\n%s", p, stack))
			panicked = true
		}
	}()
	res, err = h.Execute(ctx, x)
	return res, err, false
}

// classify maps a handler's return value and its context's cancellation
// cause to an engine outcome.
func classify(ctx context.Context, result json.RawMessage, err error, panicked bool, timeout time.Duration) engine.Outcome {
	if err == nil && !panicked {
		if len(result) == 0 {
			result = json.RawMessage(`{}`)
		}
		return engine.Outcome{Kind: engine.OutcomeSuccess, Result: result}
	}
	cause := context.Cause(ctx)
	switch {
	case errors.Is(cause, errCancelRequested):
		return engine.Outcome{Kind: engine.OutcomeCancelled, Err: errCancelRequested}
	case errors.Is(cause, errTimeout):
		return engine.Outcome{Kind: engine.OutcomeTimeout, Retryable: true,
			Err: fmt.Errorf("attempt exceeded its %s timeout", timeout)}
	case errors.Is(cause, errShutdown):
		return engine.Outcome{Kind: engine.OutcomeInterrupted, Retryable: true, Err: errShutdown}
	case panicked:
		return engine.Outcome{Kind: engine.OutcomePanic, Retryable: true, Err: err}
	case handlers.IsPermanent(err):
		return engine.Outcome{Kind: engine.OutcomeFailure, Retryable: false, Err: err}
	default:
		return engine.Outcome{Kind: engine.OutcomeFailure, Retryable: true, Err: err}
	}
}

// complete reports the outcome, retrying transient database failures for up
// to one lease duration. If it still cannot report, the lease expires and the
// reaper re-runs the attempt (at-least-once).
func (w *Worker) complete(log *slog.Logger, c *engine.Claimed, out engine.Outcome) {
	deadline := time.Now().Add(w.cfg.Lease)
	backoff := 200 * time.Millisecond
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := w.eng.Complete(ctx, c, out)
		cancel()
		switch {
		case err == nil:
			log.Info("attempt finished", "outcome", out.Kind)
			return
		case errors.Is(err, engine.ErrLeaseLost):
			log.Warn("result discarded: run was reclaimed by the lease reaper", "outcome", out.Kind)
			return
		case time.Now().After(deadline):
			log.Error("could not report attempt outcome; lease will expire and the run will be retried", "err", err)
			return
		default:
			log.Warn("report outcome failed; retrying", "err", err)
			time.Sleep(backoff)
			backoff = min(backoff*2, 5*time.Second)
		}
	}
}

func (w *Worker) heartbeatLoop(ctx context.Context) {
	t := time.NewTicker(w.cfg.Heartbeat)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.beat(ctx)
		}
	}
}

func (w *Worker) beat(ctx context.Context) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	status := domain.WorkerOnline
	if w.draining.Load() {
		status = domain.WorkerDraining
	}
	hctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	drain, err := store.WorkerHeartbeat(hctx, w.pool, store.Heartbeat{
		WorkerID: w.ID, Status: status, ActiveRuns: w.ActiveRuns(),
		HeapAllocBytes: int64(ms.HeapAlloc), Goroutines: runtime.NumGoroutine(), Now: time.Now().UTC(),
	})
	if err != nil {
		w.log.Warn("heartbeat failed", "err", err)
		return
	}
	if drain && !w.draining.Load() {
		w.log.Info("drain requested by operator")
		w.Drain()
	}
}

// cancelListener receives cancellation requests over Redis pub/sub for
// immediate effect. It is an optimisation: lease renewal also observes
// cancel_requested_at in PostgreSQL within Lease/3.
func (w *Worker) cancelListener(ctx context.Context) {
	for ctx.Err() == nil {
		ps := w.q.SubscribeCancel(ctx)
		ch := ps.Channel()
	recv:
		for {
			select {
			case <-ctx.Done():
				_ = ps.Close()
				return
			case msg, ok := <-ch:
				if !ok {
					break recv
				}
				id, err := uuid.Parse(msg.Payload)
				if err != nil {
					continue
				}
				if v, ok := w.active.Load(id); ok {
					w.log.Info("cancellation signal received", "run_id", id)
					v.(context.CancelCauseFunc)(errCancelRequested)
				}
			}
		}
		_ = ps.Close()
		sleep(ctx, 2*time.Second)
	}
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
