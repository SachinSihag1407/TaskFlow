package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/taskflow/taskflow/internal/db"
	"github.com/taskflow/taskflow/internal/domain"
	"github.com/taskflow/taskflow/internal/logging"
	"github.com/taskflow/taskflow/internal/metrics"
	"github.com/taskflow/taskflow/internal/store"
)

// Claimed is a run a worker has won the right to execute.
type Claimed struct {
	Run     *domain.Run
	Attempt *domain.Attempt
	JobName string
}

// Claim atomically moves a QUEUED run to RUNNING for workerID and opens a new
// attempt. Exactly one concurrent caller can succeed for a given run: the run
// row is locked with SKIP LOCKED and updated conditionally on status QUEUED.
//
// Concurrency limits are checked under the job's row lock (per-job limit) and
// a transaction-scoped advisory lock per tenant (workspace limit), so two
// workers cannot both observe a free slot and exceed the limit.
//
// supports, if non-nil, restricts which handlers this worker may claim; runs
// of other handlers return ErrUnsupported without being claimed.
func (e *Engine) Claim(ctx context.Context, runID, workerID uuid.UUID, lease time.Duration, supports func(handler string) bool) (*Claimed, error) {
	peek, err := store.GetRunUnscoped(ctx, e.pool, runID)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, ErrStale
	}
	if err != nil {
		return nil, err
	}
	if peek.Status != domain.RunQueued {
		return nil, ErrStale
	}
	if supports != nil && !supports(peek.Handler) {
		return nil, ErrUnsupported
	}

	var claimed *Claimed
	f := &effects{}
	err = db.WithTx(ctx, e.pool, func(tx pgx.Tx) error {
		now := e.Now()
		j, err := store.LockJob(ctx, tx, peek.JobID)
		if err != nil {
			return err
		}
		// Blocking lock (not SKIP LOCKED): the job row lock above already
		// serialises claims of this job, and a run row can be locked briefly
		// by unrelated writers such as MarkPublished right after a publish.
		// Skipping it would wrongly treat a claimable run as stale and drop
		// its queue entry until the reconciler re-published it.
		r, err := store.LockRun(ctx, tx, runID)
		if errors.Is(err, domain.ErrNotFound) {
			return ErrStale
		}
		if err != nil {
			return err
		}
		if r.Status != domain.RunQueued {
			return ErrStale
		}
		if j.CancelledAt != nil {
			// Should already be cancelled; close the gap defensively.
			if err := store.TransitionRun(ctx, tx, r.ID, domain.RunQueued, store.RunTransition{To: domain.RunCancelled, Now: now}); err != nil {
				return err
			}
			_, err := e.SyncJobStatus(ctx, tx, j, f)
			if err != nil {
				return err
			}
			return ErrStale
		}
		if j.PausedAt != nil {
			return ErrPaused
		}
		running, err := store.CountRunning(ctx, tx, j.ID)
		if err != nil {
			return err
		}
		if running >= effectiveConcurrency(j) {
			return ErrDeferred
		}
		limit, err := store.TenantConcurrencyLimit(ctx, tx, j.TenantID)
		if err != nil {
			return err
		}
		if limit > 0 {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "tenant:"+j.TenantID.String()); err != nil {
				return err
			}
			n, err := store.CountTenantRunning(ctx, tx, j.TenantID)
			if err != nil {
				return err
			}
			if n >= limit {
				return ErrDeferred
			}
		}

		a := &domain.Attempt{
			ID: uuid.New(), RunID: r.ID, JobID: r.JobID, TenantID: r.TenantID,
			AttemptNumber: r.Attempt + 1, WorkerID: &workerID, Handler: r.Handler,
			Status: domain.AttemptRunning, QueuedAt: r.QueuedAt, StartedAt: now,
		}
		if err := store.InsertAttempt(ctx, tx, a); err != nil {
			return err
		}
		leaseUntil := now.Add(lease)
		if err := store.MarkRunClaimed(ctx, tx, r.ID, a.ID, workerID, a.AttemptNumber, leaseUntil, now); err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				return ErrStale
			}
			return err
		}
		r.Status, r.Attempt, r.CurrentAttemptID, r.WorkerID, r.LeaseExpiresAt = domain.RunRunning, a.AttemptNumber, &a.ID, &workerID, &leaseUntil
		if r.StartedAt == nil {
			r.StartedAt = &now
		}
		if _, err := e.SyncJobStatus(ctx, tx, j, f); err != nil {
			return err
		}
		f.runEvent(r, domain.RunRunning)
		claimed = &Claimed{Run: r, Attempt: a, JobName: j.Name}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if claimed.Run.QueuedAt != nil {
		metrics.QueueWait.Observe(claimed.Attempt.StartedAt.Sub(*claimed.Run.QueuedAt).Seconds())
	}
	e.flush(ctx, f)
	return claimed, nil
}

// ClaimNext is the Redis-independent dispatch path: it scans PostgreSQL for
// claimable runs in priority order and claims the first one it can. Workers
// use it when Redis is unavailable, so a Redis outage degrades latency but
// never stops execution.
func (e *Engine) ClaimNext(ctx context.Context, workerID uuid.UUID, handlers []string, lease time.Duration) (*Claimed, error) {
	ids, err := store.ClaimCandidates(ctx, e.pool, handlers, 20)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		c, err := e.Claim(ctx, id, workerID, lease, nil) // candidates are pre-filtered by handler
		switch {
		case err == nil:
			return c, nil
		case errors.Is(err, ErrStale), errors.Is(err, ErrDeferred), errors.Is(err, ErrPaused):
			continue
		default:
			return nil, err
		}
	}
	return nil, ErrNoWork
}

// RenewLease extends the lease of a running attempt and reports whether the
// run has been asked to cancel. ErrLeaseLost means the reaper reclaimed the
// run (this worker was considered dead) and the attempt must stop.
func (e *Engine) RenewLease(ctx context.Context, c *Claimed, lease time.Duration) (cancelRequested bool, err error) {
	cancel, err := store.RenewLease(ctx, e.pool, c.Run.ID, c.Attempt.ID, e.Now().Add(lease))
	if errors.Is(err, domain.ErrNotFound) {
		return false, ErrLeaseLost
	}
	return cancel, err
}

// SaveCheckpoint persists handler progress so a retry can resume.
func (e *Engine) SaveCheckpoint(ctx context.Context, c *Claimed, cp json.RawMessage) error {
	err := store.SaveCheckpoint(ctx, e.pool, c.Run.ID, c.Attempt.ID, cp)
	if errors.Is(err, domain.ErrNotFound) {
		return ErrLeaseLost
	}
	return err
}

// OutcomeKind classifies how an attempt ended.
type OutcomeKind string

const (
	OutcomeSuccess     OutcomeKind = "success"
	OutcomeFailure     OutcomeKind = "failure"
	OutcomeTimeout     OutcomeKind = "timeout"
	OutcomeCancelled   OutcomeKind = "cancelled"   // cooperative cancellation completed
	OutcomeInterrupted OutcomeKind = "interrupted" // worker shutting down
	OutcomePanic       OutcomeKind = "panic"
)

// Outcome is a worker's report for one attempt.
type Outcome struct {
	Kind      OutcomeKind
	Err       error
	Retryable bool
	Result    json.RawMessage
}

// maxErrorLen bounds stored error messages.
const maxErrorLen = 4000

// Complete records an attempt's outcome and moves the run to its next state:
//
//   - success                      -> SUCCEEDED
//   - cancelled, or any failure while cancellation is pending -> CANCELLED
//   - retryable failure, attempts left -> RETRY_WAIT (exponential backoff + jitter)
//   - otherwise                    -> FAILED and dead-lettered
//
// If the worker no longer owns the run (its lease expired and the reaper
// already recovered it), ErrLeaseLost is returned and nothing is written.
func (e *Engine) Complete(ctx context.Context, c *Claimed, out Outcome) error {
	f := &effects{}
	var final domain.RunStatus
	var attemptStatus domain.AttemptStatus
	err := db.WithTx(ctx, e.pool, func(tx pgx.Tx) error {
		now := e.Now()
		j, err := store.LockJob(ctx, tx, c.Run.JobID)
		if err != nil {
			return err
		}
		r, err := store.LockRun(ctx, tx, c.Run.ID)
		if err != nil {
			return err
		}
		if r.Status != domain.RunRunning || r.CurrentAttemptID == nil || *r.CurrentAttemptID != c.Attempt.ID {
			return ErrLeaseLost
		}

		var errMsg *string
		if out.Err != nil {
			m := logging.Redact(out.Err.Error())
			if len(m) > maxErrorLen {
				m = m[:maxErrorLen] + "…"
			}
			errMsg = &m
		}
		var next *time.Time
		var errKind *string
		switch {
		case out.Kind == OutcomeSuccess:
			final, attemptStatus = domain.RunSucceeded, domain.AttemptSucceeded
		case out.Kind == OutcomeCancelled || r.CancelRequestedAt != nil:
			final, attemptStatus = domain.RunCancelled, domain.AttemptCancelled
			errKind = ptr("cancelled")
		default:
			attemptStatus = domain.AttemptFailed
			kind := string(out.Kind)
			switch out.Kind {
			case OutcomeTimeout:
				attemptStatus = domain.AttemptTimedOut
			case OutcomeFailure:
				kind = "handler_error"
				if !out.Retryable {
					kind = "permanent_error"
				}
			}
			errKind = &kind
			if out.Kind == OutcomeInterrupted && r.Attempt < r.MaxAttempts {
				// Worker shutdown is not the job's fault: retry promptly.
				t := now.Add(time.Second)
				final, next = domain.RunRetryWait, &t
			} else {
				final, next = e.failureTarget(r, out.Retryable)
			}
		}

		var retryable *bool
		if attemptStatus != domain.AttemptSucceeded && attemptStatus != domain.AttemptCancelled {
			retryable = ptr(final == domain.RunRetryWait)
		}
		if err := store.FinishAttempt(ctx, tx, c.Attempt.ID, store.AttemptOutcome{
			Status: attemptStatus, FinishedAt: now, ErrorKind: errKind, ErrorMessage: errMsg,
			Retryable: retryable, NextRetryAt: next,
		}); err != nil {
			return err
		}
		t := store.RunTransition{To: final, Now: now, AttemptID: &c.Attempt.ID, NextAttemptAt: next,
			DeadLetter: final == domain.RunFailed}
		if final == domain.RunSucceeded {
			t.Result = out.Result
		} else if errMsg != nil {
			t.LastError = errMsg
		}
		if err := store.TransitionRun(ctx, tx, r.ID, domain.RunRunning, t); err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				return ErrLeaseLost
			}
			return err
		}
		if r.WorkerID != nil {
			ok, failed := 0, 0
			if attemptStatus == domain.AttemptSucceeded {
				ok = 1
			} else if attemptStatus != domain.AttemptCancelled {
				failed = 1
			}
			if err := store.IncrementWorkerCounters(ctx, tx, *r.WorkerID, ok, failed); err != nil {
				return err
			}
		}
		if final.Terminal() {
			if err := store.SetJobLastRun(ctx, tx, j.ID, now, final); err != nil {
				return err
			}
			j.LastRunStatus, j.LastRunAt = &final, &now
		}

		switch final {
		case domain.RunRetryWait:
			e.RecordEvent(ctx, tx, runEventRecord(r, "run.retry_scheduled", domain.SeverityWarning,
				fmt.Sprintf("Attempt %d/%d failed; retrying in %s", r.Attempt, r.MaxAttempts, next.Sub(now).Round(time.Second)),
				map[string]any{"attempt": r.Attempt, "error": derefStr(errMsg)}))
		case domain.RunFailed:
			e.RecordEvent(ctx, tx, runEventRecord(r, "run.dead_lettered", domain.SeverityError,
				fmt.Sprintf("Run failed after %d attempt(s) and was moved to the dead-letter queue", r.Attempt),
				map[string]any{"attempt": r.Attempt, "error": derefStr(errMsg)}))
		case domain.RunCancelled:
			e.RecordEvent(ctx, tx, runEventRecord(r, "run.cancelled", domain.SeverityInfo,
				"Run cancelled during execution", nil))
		}
		f.runEvent(r, final)

		if _, err := e.SyncJobStatus(ctx, tx, j, f); err != nil {
			return err
		}
		// A concurrency slot just freed up: nudge any runs of this job that
		// were deferred so they do not wait for the delayed-set timer.
		refs, err := store.QueuedRefsForJob(ctx, tx, j.ID, effectiveConcurrency(j))
		if err != nil {
			return err
		}
		f.queueRefs(refs)
		return nil
	})
	if errors.Is(err, ErrLeaseLost) {
		metrics.LeaseLost.Inc()
		return err
	}
	if err != nil {
		return err
	}
	metrics.AttemptsFinished.WithLabelValues(c.Run.Handler, string(attemptStatus)).Inc()
	metrics.AttemptDuration.WithLabelValues(c.Run.Handler).Observe(e.Now().Sub(c.Attempt.StartedAt).Seconds())
	if final.Terminal() {
		metrics.RunsFinished.WithLabelValues(c.Run.Handler, string(final)).Inc()
	}
	e.flush(ctx, f)
	return nil
}
