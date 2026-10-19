package worker_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/taskflow/taskflow/internal/domain"
	"github.com/taskflow/taskflow/internal/engine"
	"github.com/taskflow/taskflow/internal/queue"
	"github.com/taskflow/taskflow/internal/scheduler"
	"github.com/taskflow/taskflow/internal/service"
	"github.com/taskflow/taskflow/internal/store"
	. "github.com/taskflow/taskflow/internal/testutil"
	"github.com/taskflow/taskflow/internal/worker"
)

var ctx = context.Background()

type cluster struct {
	env     *Env
	cancels []context.CancelFunc
	wg      sync.WaitGroup
	workers []*worker.Worker
}

func workerCfg() worker.Config {
	return worker.Config{Concurrency: 4, Lease: 3 * time.Second, Heartbeat: 500 * time.Millisecond,
		ShutdownGrace: 2 * time.Second, PollInterval: 200 * time.Millisecond, Version: "test"}
}

// start launches a scheduler and n workers sharing env's database. q is the
// queue the processes use (may be a dead queue to simulate an outage).
func start(t *testing.T, env *Env, q *queue.Queue, n int) *cluster {
	t.Helper()
	c := &cluster{env: env}
	var pub engine.Publisher
	if q != nil {
		pub = q
	}
	eng := engine.New(env.Pool, pub, env.Log)
	sched := scheduler.New(scheduler.Config{Tick: 100 * time.Millisecond, BatchSize: 100,
		ReconcileInterval: time.Second, WorkerOfflineAfter: 10 * time.Second}, eng, env.Pool, q, env.Log)
	c.launch(func(ctx context.Context) { _ = sched.Run(ctx) })
	for i := 0; i < n; i++ {
		c.addWorker(t, eng, q, workerCfg())
	}
	t.Cleanup(c.stop)
	return c
}

func (c *cluster) launch(fn func(context.Context)) context.CancelFunc {
	cctx, cancel := context.WithCancel(ctx)
	c.cancels = append(c.cancels, cancel)
	c.wg.Add(1)
	go func() { defer c.wg.Done(); fn(cctx) }()
	return cancel
}

func (c *cluster) addWorker(t *testing.T, eng *engine.Engine, q *queue.Queue, cfg worker.Config) (*worker.Worker, context.CancelFunc) {
	w := worker.New(cfg, eng, c.env.Pool, q, c.env.Reg, c.env.Log)
	c.workers = append(c.workers, w)
	cancel := c.launch(func(ctx context.Context) {
		if err := w.Run(ctx); err != nil {
			t.Errorf("worker: %v", err)
		}
	})
	return w, cancel
}

func (c *cluster) stop() {
	for _, cancel := range c.cancels {
		cancel()
	}
	c.wg.Wait()
}

func waitRunStatus(t *testing.T, env *Env, jobID uuid.UUID, timeout time.Duration, want domain.JobStatus) *domain.Job {
	t.Helper()
	var j *domain.Job
	Eventually(t, timeout, "job reaches "+string(want), func() bool {
		j = env.Job(t, jobID)
		return j.Status == want
	})
	return j
}

func TestJobsExecuteAcrossMultipleWorkers(t *testing.T) {
	env := NewEnv(t, Queue(t))
	start(t, env, env.Queue, 3)
	var ids []uuid.UUID
	for i := 0; i < 30; i++ {
		ids = append(ids, env.MustCreate(t, SleepJob("bulk", `{"duration_ms":50}`)).ID)
	}
	for _, id := range ids {
		waitRunStatus(t, env, id, 20*time.Second, domain.JobSucceeded)
	}
	// Work was actually spread across workers and every run ran exactly once.
	workers := map[uuid.UUID]int{}
	for _, id := range ids {
		runs := env.Runs(t, id)
		if len(runs) != 1 || runs[0].Attempt != 1 {
			t.Fatalf("job %s: %d runs, attempt %d", id, len(runs), runs[0].Attempt)
		}
		workers[*runs[0].WorkerID]++
	}
	if len(workers) < 2 {
		t.Fatalf("all 30 runs executed on %d worker(s); expected distribution across workers", len(workers))
	}
}

func TestRetriesThenSucceeds(t *testing.T) {
	env := NewEnv(t, Queue(t))
	start(t, env, env.Queue, 1)
	in := SleepJob("flaky", `{"duration_ms":10,"fail_attempts":2}`)
	in.MaxAttempts, in.BackoffBaseSeconds, in.BackoffMaxSeconds = Ptr(3), Ptr(1), Ptr(1)
	j := env.MustCreate(t, in)
	waitRunStatus(t, env, j.ID, 20*time.Second, domain.JobSucceeded)
	run := env.Runs(t, j.ID)[0]
	as := env.Attempts(t, run.ID)
	if len(as) != 3 || as[0].Status != domain.AttemptFailed || as[1].Status != domain.AttemptFailed || as[2].Status != domain.AttemptSucceeded {
		t.Fatalf("unexpected attempt history: %d attempts", len(as))
	}
	logs, err := store.ListLogs(ctx, env.Pool, env.Admin.TenantID, as[0].ID, 0, 100)
	if err != nil || len(logs) == 0 {
		t.Fatalf("attempt logs not persisted: %v", err)
	}
}

func TestCrashedWorkerRunIsRecoveredByAnotherWorker(t *testing.T) {
	env := NewEnv(t, Queue(t))
	in := SleepJob("survives crash", `{"duration_ms":10}`)
	in.MaxAttempts = Ptr(3)
	j := env.MustCreate(t, in)

	// Simulate a worker that claims the run and then dies (SIGKILL): it never
	// renews its lease and never reports an outcome.
	eng := engine.New(env.Pool, env.Queue, env.Log)
	if _, err := eng.ScheduleDue(ctx, 10); err != nil {
		t.Fatal(err)
	}
	run := env.Runs(t, j.ID)[0]
	dead := env.NewWorker(t)
	if _, err := eng.Claim(ctx, run.ID, dead, 2*time.Second, nil); err != nil {
		t.Fatal(err)
	}

	start(t, env, env.Queue, 1)
	waitRunStatus(t, env, j.ID, 30*time.Second, domain.JobSucceeded)
	as := env.Attempts(t, run.ID)
	if len(as) != 2 || as[0].Status != domain.AttemptLost || *as[0].WorkerID != dead || as[1].Status != domain.AttemptSucceeded {
		t.Fatalf("expected LOST attempt on the dead worker followed by success; got %d attempts", len(as))
	}
}

func TestRedisOutageFallsBackToPostgres(t *testing.T) {
	env := NewEnv(t, nil)
	start(t, env, DeadQueue(t), 2)
	j := env.MustCreate(t, SleepJob("no redis", `{"duration_ms":10}`))
	waitRunStatus(t, env, j.ID, 20*time.Second, domain.JobSucceeded)
}

func TestReconcilerRepublishesAfterRedisDataLoss(t *testing.T) {
	q := Queue(t)
	env := NewEnv(t, q)
	j := env.MustCreate(t, SleepJob("lost entry", `{"duration_ms":10}`))
	if _, err := env.Eng.ScheduleDue(ctx, 10); err != nil {
		t.Fatal(err)
	}
	// Simulate Redis restarting without persistence: the queue entry vanishes.
	run := env.Runs(t, j.ID)[0]
	if err := q.Remove(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if ready, _, _ := q.Depth(ctx); ready != 0 {
		t.Fatalf("expected empty ready set, got %d", ready)
	}
	n, err := env.Eng.Reconcile(ctx, 0, 100)
	if err != nil || n != 1 {
		t.Fatalf("reconcile = %d, %v; want 1", n, err)
	}
	if ready, _, _ := q.Depth(ctx); ready != 1 {
		t.Fatalf("run not re-published, ready=%d", ready)
	}
	start(t, env, q, 1)
	waitRunStatus(t, env, j.ID, 20*time.Second, domain.JobSucceeded)
}

func TestCancelDuringExecutionStopsHandler(t *testing.T) {
	env := NewEnv(t, Queue(t))
	start(t, env, env.Queue, 1)
	j := env.MustCreate(t, SleepJob("long", `{"duration_ms":60000}`))
	waitRunStatus(t, env, j.ID, 15*time.Second, domain.JobRunning)
	run := env.Runs(t, j.ID)[0]
	started := time.Now()
	if _, err := env.Eng.CancelRun(ctx, env.Admin, run.ID); err != nil {
		t.Fatal(err)
	}
	Eventually(t, 10*time.Second, "run cancelled", func() bool { return env.Run(t, run.ID).Status == domain.RunCancelled })
	if time.Since(started) > 5*time.Second {
		t.Fatalf("cancellation took %s", time.Since(started))
	}
	if as := env.Attempts(t, run.ID); as[0].Status != domain.AttemptCancelled {
		t.Fatalf("attempt status %s", as[0].Status)
	}
}

func TestTimeoutIsRecordedAndRetried(t *testing.T) {
	env := NewEnv(t, Queue(t))
	start(t, env, env.Queue, 1)
	in := SleepJob("slow", `{"duration_ms":10000}`)
	in.TimeoutSeconds, in.MaxAttempts, in.BackoffBaseSeconds, in.BackoffMaxSeconds = Ptr(1), Ptr(2), Ptr(1), Ptr(1)
	j := env.MustCreate(t, in)
	waitRunStatus(t, env, j.ID, 30*time.Second, domain.JobFailed)
	as := env.Attempts(t, env.Runs(t, j.ID)[0].ID)
	if len(as) != 2 || as[0].Status != domain.AttemptTimedOut || as[1].Status != domain.AttemptTimedOut {
		t.Fatalf("expected 2 TIMED_OUT attempts, got %d", len(as))
	}
	if *as[0].DurationMS > 3000 {
		t.Fatalf("timed-out attempt ran %dms, timeout is 1s", *as[0].DurationMS)
	}
}

func TestHandlerPanicDoesNotCrashWorker(t *testing.T) {
	env := NewEnv(t, Queue(t))
	start(t, env, env.Queue, 1)
	in := SleepJob("panics", `{"duration_ms":0,"fail_attempts":1,"panic":true}`)
	in.MaxAttempts, in.BackoffBaseSeconds, in.BackoffMaxSeconds = Ptr(2), Ptr(1), Ptr(1)
	j := env.MustCreate(t, in)
	waitRunStatus(t, env, j.ID, 20*time.Second, domain.JobSucceeded)
	as := env.Attempts(t, env.Runs(t, j.ID)[0].ID)
	if as[0].ErrorKind == nil || *as[0].ErrorKind != "panic" || !strings.Contains(*as[0].ErrorMessage, "panicked") {
		t.Fatalf("panic not recorded: %+v", as[0])
	}
	// The same worker kept running and executed the retry.
	if *as[0].WorkerID != *as[1].WorkerID {
		t.Fatal("expected the retry on the same (surviving) worker")
	}
}

func TestGracefulShutdownHandsOffInterruptedWork(t *testing.T) {
	env := NewEnv(t, Queue(t))
	c := start(t, env, env.Queue, 0)
	eng := engine.New(env.Pool, env.Queue, env.Log)
	cfg := workerCfg()
	cfg.ShutdownGrace = 500 * time.Millisecond
	_, stopFirst := c.addWorker(t, eng, env.Queue, cfg)

	in := SleepJob("long", `{"duration_ms":4000}`)
	in.MaxAttempts = Ptr(3)
	j := env.MustCreate(t, in)
	waitRunStatus(t, env, j.ID, 15*time.Second, domain.JobRunning)
	first := c.workers[0]
	stopFirst() // SIGTERM: grace (0.5s) is shorter than the job (4s)

	Eventually(t, 10*time.Second, "first worker offline", func() bool {
		w, err := store.GetWorker(ctx, env.Pool, first.ID)
		return err == nil && w.Status == domain.WorkerOffline && w.StoppedAt != nil
	})
	c.addWorker(t, eng, env.Queue, workerCfg())
	waitRunStatus(t, env, j.ID, 30*time.Second, domain.JobSucceeded)
	as := env.Attempts(t, env.Runs(t, j.ID)[0].ID)
	if len(as) != 2 || as[0].ErrorKind == nil || *as[0].ErrorKind != "interrupted" {
		t.Fatalf("expected interrupted attempt followed by success, got %d attempts", len(as))
	}
}

func TestDrainRequestStopsWorker(t *testing.T) {
	env := NewEnv(t, Queue(t))
	c := start(t, env, env.Queue, 1)
	w := c.workers[0]
	Eventually(t, 5*time.Second, "worker registered", func() bool {
		_, err := store.GetWorker(ctx, env.Pool, w.ID)
		return err == nil
	})
	ops := service.NewOps(env.Pool, env.Eng, env.Queue, 20*time.Second, time.Minute)
	if err := ops.DrainWorker(ctx, env.Admin, w.ID); err != nil {
		t.Fatal(err)
	}
	Eventually(t, 10*time.Second, "drained worker offline", func() bool {
		ww, _ := store.GetWorker(ctx, env.Pool, w.ID)
		return ww.Status == domain.WorkerOffline
	})
}

func TestNotificationIsIdempotentAcrossAttempts(t *testing.T) {
	env := NewEnv(t, Queue(t))
	in := service.JobInput{Name: "notify", Handler: "notification.send", ScheduleType: "once",
		Payload: []byte(`{"channel":"email","recipients":["a@example.com","b@example.com"],"subject":"hi"}`)}
	j := env.MustCreate(t, in)
	eng := engine.New(env.Pool, env.Queue, env.Log)
	if _, err := eng.ScheduleDue(ctx, 10); err != nil {
		t.Fatal(err)
	}
	run := env.Runs(t, j.ID)[0]
	h, _ := env.Reg.Get("notification.send")
	// Simulate a first attempt that delivered to everyone but crashed before
	// reporting, then a retry: the retry must not notify anyone twice.
	for attempt := 1; attempt <= 2; attempt++ {
		x := testExecution(run, attempt)
		out, err := h.Execute(ctx, x)
		if err != nil {
			t.Fatal(err)
		}
		var res map[string]int
		_ = json.Unmarshal(out, &res)
		wantDelivered := map[int]int{1: 2, 2: 0}[attempt]
		if res["delivered"] != wantDelivered {
			t.Fatalf("attempt %d delivered %d, want %d", attempt, res["delivered"], wantDelivered)
		}
	}
	var n int
	_ = env.Pool.QueryRow(ctx, `SELECT count(*) FROM notification_deliveries WHERE run_id = $1`, run.ID).Scan(&n)
	if n != 2 {
		t.Fatalf("%d deliveries recorded, want 2", n)
	}
}

func TestBatchResumesFromCheckpoint(t *testing.T) {
	env := NewEnv(t, Queue(t))
	start(t, env, env.Queue, 1)
	in := service.JobInput{Name: "batch", Handler: "batch.process", ScheduleType: "once",
		Payload: []byte(`{"total_records":1000,"chunk_size":100,"fail_at_record":550}`)}
	in.MaxAttempts, in.BackoffBaseSeconds, in.BackoffMaxSeconds = Ptr(2), Ptr(1), Ptr(1)
	j := env.MustCreate(t, in)
	waitRunStatus(t, env, j.ID, 20*time.Second, domain.JobSucceeded)
	run := env.Runs(t, j.ID)[0]
	var res struct {
		Processed   int    `json:"processed"`
		ResumedFrom int    `json:"resumed_from"`
		Accumulator string `json:"accumulator"`
	}
	if err := json.Unmarshal(run.Result, &res); err != nil {
		t.Fatal(err)
	}
	if res.Processed != 1000 || res.ResumedFrom != 500 {
		t.Fatalf("processed=%d resumed_from=%d; want 1000 and 500", res.Processed, res.ResumedFrom)
	}
	// The result equals an uninterrupted run's result.
	in2 := in
	in2.Payload = []byte(`{"total_records":1000,"chunk_size":100}`)
	j2 := env.MustCreate(t, in2)
	waitRunStatus(t, env, j2.ID, 20*time.Second, domain.JobSucceeded)
	var res2 struct {
		Accumulator string `json:"accumulator"`
	}
	_ = json.Unmarshal(env.Runs(t, j2.ID)[0].Result, &res2)
	if res.Accumulator != res2.Accumulator {
		t.Fatalf("resumed result %s differs from clean result %s", res.Accumulator, res2.Accumulator)
	}
}
