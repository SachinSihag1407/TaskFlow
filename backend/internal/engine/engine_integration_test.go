package engine_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/taskflow/taskflow/internal/domain"
	"github.com/taskflow/taskflow/internal/engine"
	"github.com/taskflow/taskflow/internal/service"
	"github.com/taskflow/taskflow/internal/statemachine"
	"github.com/taskflow/taskflow/internal/store"
	. "github.com/taskflow/taskflow/internal/testutil"
)

const lease = 30 * time.Second

var ctx = context.Background()

func TestOneTimeJobLifecycle(t *testing.T) {
	env := NewEnv(t, nil)
	j := env.MustCreate(t, SleepJob("lifecycle", `{"duration_ms":0}`))
	if j.Status != domain.JobScheduled || j.NextRunAt == nil {
		t.Fatalf("new job: status=%s next=%v, want SCHEDULED with next_run_at", j.Status, j.NextRunAt)
	}

	n, err := env.Eng.ScheduleDue(ctx, 100)
	if err != nil || n != 1 {
		t.Fatalf("ScheduleDue = %d, %v; want 1 run", n, err)
	}
	if got := env.Job(t, j.ID); got.Status != domain.JobQueued || got.NextRunAt != nil {
		t.Fatalf("after scheduling: status=%s next=%v", got.Status, got.NextRunAt)
	}
	// A second tick must not enqueue the same occurrence again.
	if n, _ := env.Eng.ScheduleDue(ctx, 100); n != 0 {
		t.Fatalf("second ScheduleDue enqueued %d runs", n)
	}

	runs := env.Runs(t, j.ID)
	c, err := env.Eng.Claim(ctx, runs[0].ID, env.NewWorker(t), lease, nil)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if got := env.Job(t, j.ID); got.Status != domain.JobRunning {
		t.Fatalf("status while running = %s", got.Status)
	}
	if err := env.Eng.Complete(ctx, c, engine.Outcome{Kind: engine.OutcomeSuccess, Result: []byte(`{"ok":true}`)}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	got := env.Job(t, j.ID)
	if got.Status != domain.JobSucceeded || got.LastRunStatus == nil || *got.LastRunStatus != domain.RunSucceeded {
		t.Fatalf("final job status=%s last=%v", got.Status, got.LastRunStatus)
	}
	r := env.Run(t, runs[0].ID)
	if r.Status != domain.RunSucceeded || string(r.Result) != `{"ok": true}` || r.FinishedAt == nil {
		t.Fatalf("run: status=%s result=%s finished=%v", r.Status, r.Result, r.FinishedAt)
	}
	as := env.Attempts(t, r.ID)
	if len(as) != 1 || as[0].Status != domain.AttemptSucceeded || as[0].DurationMS == nil {
		t.Fatalf("attempts: %+v", as)
	}
}

func TestConcurrentSchedulersDoNotDuplicateOccurrences(t *testing.T) {
	env := NewEnv(t, nil)
	clock := NewClock(time.Date(2026, 1, 1, 0, 0, 30, 0, time.UTC))
	env.UseClock(clock)
	in := service.JobInput{Name: "every minute", Handler: "test.sleep", Payload: []byte(`{"duration_ms":0}`),
		ScheduleType: "cron", CronExpr: Ptr("* * * * *"), Timezone: "UTC",
		MissedRunPolicy: "run_all", OverlapPolicy: "allow", MaxConcurrency: Ptr(100)}
	var jobs []*domain.Job
	for i := 0; i < 5; i++ {
		jobs = append(jobs, env.MustCreate(t, in))
	}
	// 10 minutes pass with no scheduler: 10 occurrences due per job.
	clock.Advance(10 * time.Minute)

	var wg sync.WaitGroup
	var total atomic.Int64
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := env.Eng.ScheduleDue(ctx, 100)
			if err != nil {
				t.Errorf("ScheduleDue: %v", err)
			}
			total.Add(int64(n))
		}()
	}
	wg.Wait()
	if total.Load() != 50 {
		t.Fatalf("enqueued %d runs across concurrent schedulers, want exactly 50", total.Load())
	}
	for _, j := range jobs {
		runs := env.Runs(t, j.ID)
		seen := map[time.Time]bool{}
		for _, r := range runs {
			if seen[r.ScheduledFor] {
				t.Fatalf("duplicate occurrence %s for job %s", r.ScheduledFor, j.ID)
			}
			seen[r.ScheduledFor] = true
		}
		if len(runs) != 10 {
			t.Fatalf("job %s has %d runs, want 10", j.ID, len(runs))
		}
	}
}

func TestOccurrenceUniqueIndexRejectsDuplicates(t *testing.T) {
	env := NewEnv(t, nil)
	j := env.MustCreate(t, SleepJob("dup", `{"duration_ms":0}`))
	when := time.Now().UTC().Truncate(time.Second)
	r1 := store.NewRunFromJob(j, when, domain.TriggerSchedule, when)
	r2 := store.NewRunFromJob(j, when, domain.TriggerSchedule, when)
	if ok, err := store.InsertRun(ctx, env.Pool, r1); !ok || err != nil {
		t.Fatalf("first insert: %v %v", ok, err)
	}
	if ok, err := store.InsertRun(ctx, env.Pool, r2); ok || err != nil {
		t.Fatalf("duplicate occurrence was inserted (ok=%v err=%v)", ok, err)
	}
}

func queuedRun(t *testing.T, env *Env, in service.JobInput) (*domain.Job, *domain.Run) {
	t.Helper()
	j := env.MustCreate(t, in)
	if _, err := env.Eng.ScheduleDue(ctx, 100); err != nil {
		t.Fatal(err)
	}
	runs := env.Runs(t, j.ID)
	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}
	return j, runs[0]
}

func TestConcurrentClaimsSucceedExactlyOnce(t *testing.T) {
	env := NewEnv(t, nil)
	_, run := queuedRun(t, env, SleepJob("claim race", `{"duration_ms":0}`))
	var wg sync.WaitGroup
	var wins atomic.Int32
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := env.Eng.Claim(ctx, run.ID, env.NewWorker(t), lease, nil)
			switch {
			case err == nil:
				wins.Add(1)
			case errors.Is(err, engine.ErrStale), errors.Is(err, engine.ErrDeferred):
			default:
				t.Errorf("unexpected claim error: %v", err)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d workers claimed the same run, want exactly 1", wins.Load())
	}
	if as := env.Attempts(t, run.ID); len(as) != 1 {
		t.Fatalf("%d attempts recorded, want 1", len(as))
	}
}

func TestPerJobConcurrencyLimit(t *testing.T) {
	env := NewEnv(t, nil)
	in := service.JobInput{Name: "parallel", Handler: "test.sleep", Payload: []byte(`{"duration_ms":0}`),
		ScheduleType: "cron", CronExpr: Ptr("0 0 1 1 *"), Timezone: "UTC", OverlapPolicy: "allow", MaxConcurrency: Ptr(2)}
	j := env.MustCreate(t, in)
	var runs []*domain.Run
	for i := 0; i < 4; i++ {
		r, err := env.Eng.RunNow(ctx, env.Admin, j.ID)
		if err != nil {
			t.Fatal(err)
		}
		runs = append(runs, r)
	}
	claimed := 0
	for _, r := range runs {
		_, err := env.Eng.Claim(ctx, r.ID, env.NewWorker(t), lease, nil)
		if err == nil {
			claimed++
		} else if !errors.Is(err, engine.ErrDeferred) {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if claimed != 2 {
		t.Fatalf("claimed %d runs with max_concurrency=2", claimed)
	}
}

func TestOverlapQueueSerialisesRuns(t *testing.T) {
	env := NewEnv(t, nil)
	in := service.JobInput{Name: "serial", Handler: "test.sleep", Payload: []byte(`{"duration_ms":0}`),
		ScheduleType: "cron", CronExpr: Ptr("0 0 1 1 *"), Timezone: "UTC", OverlapPolicy: "queue", MaxConcurrency: Ptr(5)}
	j := env.MustCreate(t, in)
	r1, _ := env.Eng.RunNow(ctx, env.Admin, j.ID)
	r2, _ := env.Eng.RunNow(ctx, env.Admin, j.ID)
	if _, err := env.Eng.Claim(ctx, r1.ID, env.NewWorker(t), lease, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Eng.Claim(ctx, r2.ID, env.NewWorker(t), lease, nil); !errors.Is(err, engine.ErrDeferred) {
		t.Fatalf("overlap=queue must ignore max_concurrency and run serially; got %v", err)
	}
}

func TestWorkspaceConcurrencyLimit(t *testing.T) {
	env := NewEnv(t, nil)
	if _, err := env.Pool.Exec(ctx, `UPDATE tenant_settings SET max_concurrent_runs = 1 WHERE tenant_id = $1`, env.Admin.TenantID); err != nil {
		t.Fatal(err)
	}
	_, a := queuedRun(t, env, SleepJob("a", `{"duration_ms":0}`))
	_, b := queuedRun(t, env, SleepJob("b", `{"duration_ms":0}`))
	if _, err := env.Eng.Claim(ctx, a.ID, env.NewWorker(t), lease, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Eng.Claim(ctx, b.ID, env.NewWorker(t), lease, nil); !errors.Is(err, engine.ErrDeferred) {
		t.Fatalf("second claim should be deferred by the workspace limit, got %v", err)
	}
}

func TestRetryBackoffThenDeadLetter(t *testing.T) {
	env := NewEnv(t, nil)
	clock := NewClock(time.Now())
	env.UseClock(clock)
	in := SleepJob("flaky", `{"duration_ms":0}`)
	in.MaxAttempts, in.BackoffBaseSeconds, in.BackoffMaxSeconds = Ptr(3), Ptr(10), Ptr(600)
	j, run := queuedRun(t, env, in)

	for attempt := 1; attempt <= 3; attempt++ {
		c, err := env.Eng.Claim(ctx, run.ID, env.NewWorker(t), lease, nil)
		if err != nil {
			t.Fatalf("attempt %d claim: %v", attempt, err)
		}
		if c.Attempt.AttemptNumber != attempt {
			t.Fatalf("attempt number %d, want %d", c.Attempt.AttemptNumber, attempt)
		}
		if err := env.Eng.Complete(ctx, c, engine.Outcome{Kind: engine.OutcomeFailure, Retryable: true, Err: errors.New("boom")}); err != nil {
			t.Fatal(err)
		}
		r := env.Run(t, run.ID)
		if attempt < 3 {
			if r.Status != domain.RunRetryWait || r.NextAttemptAt == nil {
				t.Fatalf("attempt %d: status %s, want RETRY_WAIT", attempt, r.Status)
			}
			// Rand is fixed at 0.5: delay = 0.75 * base * 2^(attempt-1).
			want := time.Duration(float64(10*time.Second) * float64(int(1)<<(attempt-1)) * 0.75)
			if d := r.NextAttemptAt.Sub(clock.Now()); d < want-time.Second || d > want+time.Second {
				t.Fatalf("attempt %d: backoff %s, want ~%s", attempt, d, want)
			}
			if got := env.Job(t, j.ID); got.Status != domain.JobRetryWait {
				t.Fatalf("job status %s, want RETRY_WAIT", got.Status)
			}
			// Not yet due: promotion must not touch it.
			if n, _ := env.Eng.PromoteRetries(ctx, 100); n != 0 {
				t.Fatal("promoted a retry before its backoff elapsed")
			}
			clock.Advance(r.NextAttemptAt.Sub(clock.Now()) + time.Millisecond)
			if n, err := env.Eng.PromoteRetries(ctx, 100); n != 1 || err != nil {
				t.Fatalf("promote: %d %v", n, err)
			}
		}
	}
	r := env.Run(t, run.ID)
	if r.Status != domain.RunFailed || r.DeadLetteredAt == nil || r.LastError == nil || *r.LastError != "boom" {
		t.Fatalf("final run: status=%s dlq=%v err=%v", r.Status, r.DeadLetteredAt, r.LastError)
	}
	if got := env.Job(t, j.ID); got.Status != domain.JobFailed {
		t.Fatalf("job status %s, want FAILED", got.Status)
	}
	as := env.Attempts(t, run.ID)
	if len(as) != 3 {
		t.Fatalf("%d attempts, want 3", len(as))
	}
	if *as[0].Retryable != true || *as[2].Retryable != false {
		t.Fatal("retryable flags wrong")
	}
}

func TestPermanentFailureSkipsRetries(t *testing.T) {
	env := NewEnv(t, nil)
	in := SleepJob("bad input", `{"duration_ms":0}`)
	in.MaxAttempts = Ptr(5)
	_, run := queuedRun(t, env, in)
	c, _ := env.Eng.Claim(ctx, run.ID, env.NewWorker(t), lease, nil)
	if err := env.Eng.Complete(ctx, c, engine.Outcome{Kind: engine.OutcomeFailure, Retryable: false, Err: errors.New("invalid")}); err != nil {
		t.Fatal(err)
	}
	if r := env.Run(t, run.ID); r.Status != domain.RunFailed || r.Attempt != 1 {
		t.Fatalf("status=%s attempt=%d; permanent errors must not retry", r.Status, r.Attempt)
	}
}

func TestCrashedWorkerLeaseRecoveryAndFencing(t *testing.T) {
	env := NewEnv(t, nil)
	clock := NewClock(time.Now())
	env.UseClock(clock)
	_, run := queuedRun(t, env, SleepJob("crash", `{"duration_ms":0}`))

	workerA, workerB := env.NewWorker(t), env.NewWorker(t)
	ca, err := env.Eng.Claim(ctx, run.ID, workerA, lease, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Worker A "crashes": no renewals. Before expiry the reaper must not act.
	clock.Advance(lease - time.Second)
	if n, _ := env.Eng.RecoverExpiredLeases(ctx, 10); n != 0 {
		t.Fatal("recovered a run whose lease had not expired")
	}
	clock.Advance(2 * time.Second)
	if n, err := env.Eng.RecoverExpiredLeases(ctx, 10); n != 1 || err != nil {
		t.Fatalf("recover: %d %v", n, err)
	}
	as := env.Attempts(t, run.ID)
	if as[0].Status != domain.AttemptLost || as[0].ErrorKind == nil || *as[0].ErrorKind != "lease_expired" {
		t.Fatalf("lost attempt not recorded: %+v", as[0])
	}
	r := env.Run(t, run.ID)
	if r.Status != domain.RunRetryWait {
		t.Fatalf("status %s, want RETRY_WAIT", r.Status)
	}
	clock.Advance(time.Hour)
	_, _ = env.Eng.PromoteRetries(ctx, 10)
	cb, err := env.Eng.Claim(ctx, run.ID, workerB, lease, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Worker A was only partitioned, not dead. Its late writes are fenced off.
	if _, err := env.Eng.RenewLease(ctx, ca, lease); !errors.Is(err, engine.ErrLeaseLost) {
		t.Fatalf("stale renew: %v, want ErrLeaseLost", err)
	}
	if err := env.Eng.Complete(ctx, ca, engine.Outcome{Kind: engine.OutcomeSuccess}); !errors.Is(err, engine.ErrLeaseLost) {
		t.Fatalf("stale complete: %v, want ErrLeaseLost", err)
	}
	if r := env.Run(t, run.ID); r.Status != domain.RunRunning || *r.WorkerID != workerB {
		t.Fatalf("stale worker changed the run: status=%s worker=%v", r.Status, r.WorkerID)
	}
	if err := env.Eng.Complete(ctx, cb, engine.Outcome{Kind: engine.OutcomeSuccess}); err != nil {
		t.Fatal(err)
	}
	if r := env.Run(t, run.ID); r.Status != domain.RunSucceeded || r.Attempt != 2 {
		t.Fatalf("final: %s attempt %d", r.Status, r.Attempt)
	}
}

func TestLeaseRecoveryExhaustedAttemptsDeadLetters(t *testing.T) {
	env := NewEnv(t, nil)
	clock := NewClock(time.Now())
	env.UseClock(clock)
	in := SleepJob("crash once", `{"duration_ms":0}`)
	in.MaxAttempts = Ptr(1)
	j, run := queuedRun(t, env, in)
	if _, err := env.Eng.Claim(ctx, run.ID, env.NewWorker(t), lease, nil); err != nil {
		t.Fatal(err)
	}
	clock.Advance(lease + time.Second)
	if n, _ := env.Eng.RecoverExpiredLeases(ctx, 10); n != 1 {
		t.Fatal("not recovered")
	}
	if r := env.Run(t, run.ID); r.Status != domain.RunFailed || r.DeadLetteredAt == nil {
		t.Fatalf("status %s, want dead-lettered FAILED", r.Status)
	}
	if got := env.Job(t, j.ID); got.Status != domain.JobFailed {
		t.Fatalf("job %s", got.Status)
	}
}

func TestCancelQueuedRun(t *testing.T) {
	env := NewEnv(t, nil)
	j, run := queuedRun(t, env, SleepJob("cancel me", `{"duration_ms":0}`))
	if _, err := env.Eng.CancelJob(ctx, env.Admin, j.ID); err != nil {
		t.Fatal(err)
	}
	if r := env.Run(t, run.ID); r.Status != domain.RunCancelled {
		t.Fatalf("run %s, want CANCELLED", r.Status)
	}
	if _, err := env.Eng.Claim(ctx, run.ID, env.NewWorker(t), lease, nil); !errors.Is(err, engine.ErrStale) {
		t.Fatalf("claimed a cancelled run: %v", err)
	}
	if got := env.Job(t, j.ID); got.Status != domain.JobCancelled {
		t.Fatalf("job %s", got.Status)
	}
	// Cancelling again is rejected with an explanation.
	var ce *domain.ConflictError
	if _, err := env.Eng.CancelJob(ctx, env.Admin, j.ID); !errors.As(err, &ce) {
		t.Fatalf("second cancel: %v", err)
	}
}

func TestCancelRunningRunIsCooperative(t *testing.T) {
	env := NewEnv(t, nil)
	_, run := queuedRun(t, env, SleepJob("long", `{"duration_ms":0}`))
	c, _ := env.Eng.Claim(ctx, run.ID, env.NewWorker(t), lease, nil)
	if _, err := env.Eng.CancelRun(ctx, env.Admin, run.ID); err != nil {
		t.Fatal(err)
	}
	if r := env.Run(t, run.ID); r.Status != domain.RunRunning || r.CancelRequestedAt == nil {
		t.Fatalf("running run should stay RUNNING with cancel flagged; got %s", r.Status)
	}
	cancel, err := env.Eng.RenewLease(ctx, c, lease)
	if err != nil || !cancel {
		t.Fatalf("lease renewal must surface the cancel request: %v %v", cancel, err)
	}
	// The handler reacts and fails due to context cancellation.
	if err := env.Eng.Complete(ctx, c, engine.Outcome{Kind: engine.OutcomeCancelled}); err != nil {
		t.Fatal(err)
	}
	if r := env.Run(t, run.ID); r.Status != domain.RunCancelled {
		t.Fatalf("run %s, want CANCELLED", r.Status)
	}
}

func TestCompletionBeatsLateCancellation(t *testing.T) {
	env := NewEnv(t, nil)
	_, run := queuedRun(t, env, SleepJob("race", `{"duration_ms":0}`))
	c, _ := env.Eng.Claim(ctx, run.ID, env.NewWorker(t), lease, nil)
	if _, err := env.Eng.CancelRun(ctx, env.Admin, run.ID); err != nil {
		t.Fatal(err)
	}
	// The handler finished successfully before it noticed the cancellation:
	// the work happened, so the run is recorded as SUCCEEDED, not CANCELLED.
	if err := env.Eng.Complete(ctx, c, engine.Outcome{Kind: engine.OutcomeSuccess}); err != nil {
		t.Fatal(err)
	}
	if r := env.Run(t, run.ID); r.Status != domain.RunSucceeded {
		t.Fatalf("run %s, want SUCCEEDED", r.Status)
	}
}

func TestConcurrentCancelAndCompleteStayConsistent(t *testing.T) {
	for i := 0; i < 10; i++ {
		env := NewEnv(t, nil)
		j, run := queuedRun(t, env, SleepJob("race", `{"duration_ms":0}`))
		c, _ := env.Eng.Claim(ctx, run.ID, env.NewWorker(t), lease, nil)
		var wg sync.WaitGroup
		wg.Add(3)
		go func() { defer wg.Done(); _ = env.Eng.Complete(ctx, c, engine.Outcome{Kind: engine.OutcomeSuccess}) }()
		go func() { defer wg.Done(); _, _ = env.Eng.CancelJob(ctx, env.Admin, j.ID) }()
		go func() { defer wg.Done(); _, _ = env.Eng.CancelRun(ctx, env.Admin, run.ID) }()
		wg.Wait()
		r := env.Run(t, run.ID)
		// Complete always wins over a cooperative cancel here because nothing
		// observes the flag; what matters is a single, terminal, consistent state.
		if !r.Status.Terminal() {
			t.Fatalf("iteration %d: run left in %s", i, r.Status)
		}
		as := env.Attempts(t, run.ID)
		if len(as) != 1 || as[0].Status == domain.AttemptRunning {
			t.Fatalf("iteration %d: attempts inconsistent: %+v", i, as)
		}
		got := env.Job(t, j.ID)
		if got.CancelledAt != nil && got.Status != domain.JobCancelled {
			t.Fatalf("iteration %d: job cancelled but status %s", i, got.Status)
		}
	}
}

func TestReplayDeadLetterExactlyOnce(t *testing.T) {
	env := NewEnv(t, nil)
	in := SleepJob("dlq", `{"duration_ms":0}`)
	in.MaxAttempts = Ptr(1)
	j, run := queuedRun(t, env, in)
	c, _ := env.Eng.Claim(ctx, run.ID, env.NewWorker(t), lease, nil)
	_ = env.Eng.Complete(ctx, c, engine.Outcome{Kind: engine.OutcomeFailure, Retryable: true, Err: errors.New("x")})

	var wg sync.WaitGroup
	var ok atomic.Int32
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := env.Eng.ReplayRun(ctx, env.Admin, run.ID); err == nil {
				ok.Add(1)
			} else {
				var ce *domain.ConflictError
				if !errors.As(err, &ce) {
					t.Errorf("unexpected error: %v", err)
				}
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 1 {
		t.Fatalf("%d concurrent replays succeeded, want 1", ok.Load())
	}
	runs := env.Runs(t, j.ID)
	if len(runs) != 2 || runs[0].ReplayOf == nil || *runs[0].ReplayOf != run.ID || runs[0].Status != domain.RunQueued {
		t.Fatalf("replay run not created correctly: %+v", runs[0])
	}
	if got := env.Job(t, j.ID); got.Status != domain.JobQueued {
		t.Fatalf("job %s", got.Status)
	}
	audit, _, _ := store.ListAudit(ctx, env.Pool, store.AuditFilter{TenantID: env.Admin.TenantID, Action: "run.replay"})
	if len(audit) != 1 {
		t.Fatalf("%d replay audit records, want 1", len(audit))
	}
}

func TestRetryActionOnlyForFailedJobs(t *testing.T) {
	env := NewEnv(t, nil)
	in := SleepJob("retry", `{"duration_ms":0}`)
	in.MaxAttempts = Ptr(1)
	j, run := queuedRun(t, env, in)
	var ce *domain.ConflictError
	if _, err := env.Eng.RetryJob(ctx, env.Admin, j.ID); !errors.As(err, &ce) {
		t.Fatalf("retry of queued job: %v", err)
	}
	c, _ := env.Eng.Claim(ctx, run.ID, env.NewWorker(t), lease, nil)
	_ = env.Eng.Complete(ctx, c, engine.Outcome{Kind: engine.OutcomeFailure, Retryable: true, Err: errors.New("x")})
	if _, err := env.Eng.RetryJob(ctx, env.Admin, j.ID); err != nil {
		t.Fatalf("retry failed job: %v", err)
	}
	if got := env.Job(t, j.ID); got.Status != domain.JobQueued {
		t.Fatalf("job %s after retry", got.Status)
	}
}

func TestPauseHoldsQueuedRunsAndResumeReleases(t *testing.T) {
	env := NewEnv(t, nil)
	j, run := queuedRun(t, env, SleepJob("pause", `{"duration_ms":0}`))
	if _, err := env.Eng.PauseJob(ctx, env.Admin, j.ID); err != nil {
		t.Fatal(err)
	}
	if got := env.Job(t, j.ID); got.Status != domain.JobPaused {
		t.Fatalf("job %s", got.Status)
	}
	if _, err := env.Eng.Claim(ctx, run.ID, env.NewWorker(t), lease, nil); !errors.Is(err, engine.ErrPaused) {
		t.Fatalf("claim on paused job: %v", err)
	}
	if _, err := env.Eng.ResumeJob(ctx, env.Admin, j.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Eng.Claim(ctx, run.ID, env.NewWorker(t), lease, nil); err != nil {
		t.Fatalf("claim after resume: %v", err)
	}
}

func TestOverlapSkipDropsOccurrenceWhileRunning(t *testing.T) {
	env := NewEnv(t, nil)
	clock := NewClock(time.Date(2026, 5, 1, 10, 0, 30, 0, time.UTC))
	env.UseClock(clock)
	in := service.JobInput{Name: "no overlap", Handler: "test.sleep", Payload: []byte(`{"duration_ms":0}`),
		ScheduleType: "cron", CronExpr: Ptr("* * * * *"), Timezone: "UTC", OverlapPolicy: "skip"}
	j := env.MustCreate(t, in)
	clock.Advance(time.Minute)
	if n, _ := env.Eng.ScheduleDue(ctx, 10); n != 1 {
		t.Fatal("first occurrence not scheduled")
	}
	run := env.Runs(t, j.ID)[0]
	if _, err := env.Eng.Claim(ctx, run.ID, env.NewWorker(t), lease, nil); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute)
	if n, _ := env.Eng.ScheduleDue(ctx, 10); n != 0 {
		t.Fatal("overlap=skip enqueued a run while the previous one is still running")
	}
	events, _ := store.ListEvents(ctx, env.Pool, store.EventFilter{TenantID: env.Admin.TenantID, JobID: &j.ID, Limit: 50})
	found := false
	for _, e := range events {
		if e.Type == "job.occurrence_skipped" {
			found = true
		}
	}
	if !found {
		t.Fatal("skipped occurrence was not recorded as an event")
	}
	if got := env.Job(t, j.ID); got.NextRunAt == nil || !got.NextRunAt.After(clock.Now()) {
		t.Fatalf("schedule did not advance: %v", got.NextRunAt)
	}
}

func TestMissedRunPolicyAfterDowntime(t *testing.T) {
	// */5 cron created at 10:00:30; scheduler down until 10:33. Occurrences
	// 10:05..10:30 (6) are all older than the misfire grace period.
	cases := map[string]int{"skip": 0, "run_once": 1, "run_all": 6}
	for policy, want := range cases {
		t.Run(policy, func(t *testing.T) {
			env := NewEnv(t, nil)
			clock := NewClock(time.Date(2026, 5, 1, 10, 0, 30, 0, time.UTC))
			env.UseClock(clock)
			in := service.JobInput{Name: "missed", Handler: "test.sleep", Payload: []byte(`{"duration_ms":0}`),
				ScheduleType: "cron", CronExpr: Ptr("*/5 * * * *"), Timezone: "UTC",
				MissedRunPolicy: policy, OverlapPolicy: "allow", MaxConcurrency: Ptr(100)}
			j := env.MustCreate(t, in)
			clock.Advance(32*time.Minute + 30*time.Second)
			n, err := env.Eng.ScheduleDue(ctx, 10)
			if err != nil || n != want {
				t.Fatalf("policy %s enqueued %d (err %v), want %d", policy, n, err, want)
			}
			if got := env.Job(t, j.ID); got.NextRunAt == nil || !got.NextRunAt.After(clock.Now()) {
				t.Fatalf("next run not in the future: %v", got.NextRunAt)
			}
		})
	}
}

func TestInvalidTransitionIsRejected(t *testing.T) {
	env := NewEnv(t, nil)
	_, run := queuedRun(t, env, SleepJob("sm", `{"duration_ms":0}`))
	err := store.TransitionRun(ctx, env.Pool, run.ID, domain.RunQueued, store.RunTransition{To: domain.RunSucceeded, Now: time.Now()})
	var it *statemachine.ErrInvalidTransition
	if !errors.As(err, &it) {
		t.Fatalf("QUEUED -> SUCCEEDED should be rejected by the state machine, got %v", err)
	}
	// A transition whose source state is stale affects no rows.
	err = store.TransitionRun(ctx, env.Pool, run.ID, domain.RunRetryWait, store.RunTransition{To: domain.RunQueued, Now: time.Now()})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("stale source state should not match, got %v", err)
	}
}

func TestTenantIsolation(t *testing.T) {
	env := NewEnv(t, nil)
	j, run := queuedRun(t, env, SleepJob("private", `{"duration_ms":0}`))
	other := env.NewTenant(t, "Other Co", domain.RoleAdmin)
	if _, err := env.Jobs.Get(ctx, other, j.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("other tenant read job: %v", err)
	}
	if _, err := env.Eng.CancelJob(ctx, other, j.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("other tenant cancelled job: %v", err)
	}
	if _, err := env.Eng.CancelRun(ctx, other, run.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("other tenant cancelled run: %v", err)
	}
	if _, err := env.Eng.ReplayRun(ctx, other, run.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("other tenant replayed run: %v", err)
	}
	items, total, _ := env.Jobs.List(ctx, other, store.JobFilter{})
	if total != 0 || len(items) != 0 {
		t.Fatal("other tenant can list jobs")
	}
}

func TestClaimWaitsForBrieflyLockedRunInsteadOfDroppingIt(t *testing.T) {
	env := NewEnv(t, nil)
	_, run := queuedRun(t, env, SleepJob("locked", `{"duration_ms":0}`))
	// Simulate MarkPublished holding the run row right after publication.
	tx, err := env.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM job_runs WHERE id = $1 FOR UPDATE`, run.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := env.Eng.Claim(ctx, run.ID, env.NewWorker(t), lease, nil)
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	_ = tx.Rollback(ctx)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("claim of a briefly locked run failed: %v (must wait, not report stale)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("claim did not complete after the lock was released")
	}
}
