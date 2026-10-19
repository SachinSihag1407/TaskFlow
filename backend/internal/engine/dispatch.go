package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/taskflow/taskflow/internal/db"
	"github.com/taskflow/taskflow/internal/domain"
	"github.com/taskflow/taskflow/internal/metrics"
	"github.com/taskflow/taskflow/internal/schedule"
	"github.com/taskflow/taskflow/internal/store"
)

// ScheduleDue turns due job occurrences into QUEUED runs. It is safe to call
// from several scheduler instances concurrently: due jobs are locked with
// SKIP LOCKED, and the unique (job_id, scheduled_for) index rejects any
// occurrence that was already enqueued (e.g. by an instance that crashed
// after committing but before it could advance its own view).
func (e *Engine) ScheduleDue(ctx context.Context, limit int) (int, error) {
	now := e.Now()
	f := &effects{}
	created := 0
	err := db.WithTx(ctx, e.pool, func(tx pgx.Tx) error {
		jobs, err := store.LockDueJobs(ctx, tx, now, limit)
		if err != nil {
			return fmt.Errorf("lock due jobs: %w", err)
		}
		for _, j := range jobs {
			// Each job runs in a savepoint so one job that cannot be scheduled
			// (corrupt row, constraint violation) does not stall the batch.
			n, err := e.scheduleJobIsolated(ctx, tx, j, now, f)
			if err != nil {
				return fmt.Errorf("schedule job %s: %w", j.ID, err)
			}
			created += n
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	e.flush(ctx, f)
	return created, nil
}

func (e *Engine) scheduleJobIsolated(ctx context.Context, tx pgx.Tx, j *domain.Job, now time.Time, f *effects) (int, error) {
	sp, err := tx.Begin(ctx) // nested Begin = SAVEPOINT
	if err != nil {
		return 0, err
	}
	mark := len(f.publish)
	evMark := len(f.events)
	n, err := e.scheduleJob(ctx, sp, j, now, f)
	if err != nil {
		_ = sp.Rollback(ctx)
		f.publish, f.events = f.publish[:mark], f.events[:evMark]
		e.log.Error("could not schedule job; skipping it this tick", "job_id", j.ID, "err", err)
		return 0, nil
	}
	return n, sp.Commit(ctx)
}

func (e *Engine) scheduleJob(ctx context.Context, tx pgx.Tx, j *domain.Job, now time.Time, f *effects) (int, error) {
	var occurrences []time.Time
	var next *time.Time
	skipped := 0

	switch j.ScheduleType {
	case domain.ScheduleOnce, domain.ScheduleDelayed:
		occurrences = []time.Time{*j.NextRunAt}
	case domain.ScheduleCron:
		spec, err := schedule.ParseCron(derefStr(j.CronExpr), j.Timezone)
		if err != nil {
			// Validated on write, so this means the tz database changed under
			// us. Stop scheduling rather than spinning on the job every tick.
			e.RecordEvent(ctx, tx, jobEvent(j, "job.schedule_invalid", domain.SeverityError,
				"Schedule could not be evaluated and was suspended: "+err.Error()))
			j.NextRunAt = nil
			if err := store.SetJobNextRun(ctx, tx, j.ID, nil, now); err != nil {
				return 0, err
			}
			_, err = e.SyncJobStatus(ctx, tx, j, f)
			return 0, err
		}
		plan := schedule.PlanCron(spec, *j.NextRunAt, now, schedule.PolicyName(j.MissedRunPolicy))
		occurrences, skipped, next = plan.Enqueue, plan.Skipped, plan.Next
		if skipped > 0 {
			metrics.OccurrencesSkipped.WithLabelValues("missed_run_policy").Add(float64(skipped))
			e.RecordEvent(ctx, tx, jobEvent(j, "job.occurrences_missed", domain.SeverityWarning,
				fmt.Sprintf("%d missed occurrence(s) not run (missed-run policy: %s)", skipped, j.MissedRunPolicy)))
		}

		// Overlap policy. With "skip", at most one run of a recurring job may
		// be in flight, so extra occurrences are dropped.
		if j.OverlapPolicy == domain.OverlapSkip && len(occurrences) > 0 {
			summary, err := store.ActiveRunSummary(ctx, tx, j.ID)
			if err != nil {
				return 0, err
			}
			if summary.Active() > 0 {
				metrics.OccurrencesSkipped.WithLabelValues("overlap").Add(float64(len(occurrences)))
				e.RecordEvent(ctx, tx, jobEvent(j, "job.occurrence_skipped", domain.SeverityWarning,
					fmt.Sprintf("Occurrence at %s skipped: the previous run is still active (overlap policy: skip)",
						occurrences[len(occurrences)-1].Format(time.RFC3339))))
				occurrences = nil
			} else if len(occurrences) > 1 {
				metrics.OccurrencesSkipped.WithLabelValues("overlap").Add(float64(len(occurrences) - 1))
				occurrences = occurrences[len(occurrences)-1:]
			}
		}
	}

	created := 0
	for _, occ := range occurrences {
		run := store.NewRunFromJob(j, occ, domain.TriggerSchedule, now)
		inserted, err := store.InsertRun(ctx, tx, run)
		if err != nil {
			return 0, err
		}
		if !inserted {
			// Duplicate occurrence: already enqueued by an earlier tick.
			e.log.Info("duplicate occurrence ignored", "job_id", j.ID, "scheduled_for", occ)
			continue
		}
		created++
		metrics.RunsEnqueued.WithLabelValues(string(domain.TriggerSchedule)).Inc()
		f.queueRun(run)
		f.runEvent(run, domain.RunQueued)
	}

	j.NextRunAt = next
	if err := store.SetJobNextRun(ctx, tx, j.ID, next, now); err != nil {
		return 0, err
	}
	if _, err := e.SyncJobStatus(ctx, tx, j, f); err != nil {
		return 0, err
	}
	return created, nil
}

// PromoteRetries moves RETRY_WAIT runs whose backoff has elapsed back to
// QUEUED and publishes them.
func (e *Engine) PromoteRetries(ctx context.Context, limit int) (int, error) {
	now := e.Now()
	refs, err := store.DueRetryCandidates(ctx, e.pool, now, limit)
	if err != nil {
		return 0, err
	}
	byJob := map[uuid.UUID][]store.RunRef{}
	var order []uuid.UUID
	for _, r := range refs {
		if _, ok := byJob[r.JobID]; !ok {
			order = append(order, r.JobID)
		}
		byJob[r.JobID] = append(byJob[r.JobID], r)
	}
	promoted := 0
	for _, jobID := range order {
		f := &effects{}
		err := db.WithTx(ctx, e.pool, func(tx pgx.Tx) error {
			j, err := store.LockJob(ctx, tx, jobID)
			if err != nil {
				return err
			}
			for _, ref := range byJob[jobID] {
				r, err := store.LockRunIfStatus(ctx, tx, ref.RunID, domain.RunRetryWait)
				if err == domain.ErrNotFound {
					continue // cancelled or promoted concurrently
				}
				if err != nil {
					return err
				}
				if r.NextAttemptAt == nil || r.NextAttemptAt.After(now) {
					continue
				}
				if err := store.TransitionRun(ctx, tx, r.ID, domain.RunRetryWait, store.RunTransition{To: domain.RunQueued, Now: now}); err != nil {
					return err
				}
				r.QueuedAt = &now
				f.queueRun(r)
				f.runEvent(r, domain.RunQueued)
				promoted++
			}
			_, err = e.SyncJobStatus(ctx, tx, j, f)
			return err
		})
		if err != nil {
			return promoted, err
		}
		e.flush(ctx, f)
	}
	return promoted, nil
}

// RecoverExpiredLeases handles RUNNING runs whose worker stopped renewing its
// lease (crash, OOM kill, partition, stop-the-world pause). The attempt is
// marked LOST — its outcome is unknown, so the work may or may not have taken
// effect — and the run is retried if attempts remain, otherwise dead-lettered.
// This is where at-least-once delivery comes from: the work is re-run rather
// than silently dropped, and handlers must therefore be idempotent.
func (e *Engine) RecoverExpiredLeases(ctx context.Context, limit int) (int, error) {
	now := e.Now()
	candidates, err := store.ExpiredLeaseCandidates(ctx, e.pool, now, limit)
	if err != nil {
		return 0, err
	}
	recovered := 0
	for _, c := range candidates {
		f := &effects{}
		ok := false
		err := db.WithTx(ctx, e.pool, func(tx pgx.Tx) error {
			j, err := store.LockJob(ctx, tx, c.JobID)
			if err != nil {
				return err
			}
			r, err := store.LockRun(ctx, tx, c.ID)
			if err != nil {
				return err
			}
			// Re-check under lock: the worker may have renewed or finished.
			if r.Status != domain.RunRunning || r.LeaseExpiresAt == nil || !r.LeaseExpiresAt.Before(now) ||
				r.CurrentAttemptID == nil || c.CurrentAttemptID == nil || *r.CurrentAttemptID != *c.CurrentAttemptID {
				return nil
			}
			attemptID := *r.CurrentAttemptID
			msg := "Worker stopped renewing its lease (crash, restart, network partition or overload). " +
				"The attempt's outcome is unknown."
			if r.WorkerID != nil {
				msg = fmt.Sprintf("Worker %s stopped renewing its lease (crash, restart, network partition or overload). "+
					"The attempt's outcome is unknown.", r.WorkerID.String()[:8])
			}
			to, next := e.failureTarget(r, true)
			if r.CancelRequestedAt != nil {
				to, next = domain.RunCancelled, nil
			}
			if err := store.FinishAttempt(ctx, tx, attemptID, store.AttemptOutcome{
				Status: domain.AttemptLost, FinishedAt: now, ErrorKind: ptr("lease_expired"),
				ErrorMessage: &msg, Retryable: ptr(to == domain.RunRetryWait), NextRetryAt: next,
			}); err != nil {
				return err
			}
			if err := store.TransitionRun(ctx, tx, r.ID, domain.RunRunning, store.RunTransition{
				To: to, Now: now, AttemptID: &attemptID, NextAttemptAt: next, LastError: &msg,
				DeadLetter: to == domain.RunFailed,
			}); err != nil {
				return err
			}
			if r.WorkerID != nil {
				if err := store.IncrementWorkerCounters(ctx, tx, *r.WorkerID, 0, 1); err != nil {
					return err
				}
			}
			if to.Terminal() {
				if err := store.SetJobLastRun(ctx, tx, j.ID, now, to); err != nil {
					return err
				}
				j.LastRunStatus, j.LastRunAt = &to, &now
			}
			sev := domain.SeverityWarning
			text := fmt.Sprintf("Recovered run after lease expiry (attempt %d/%d); retry scheduled", r.Attempt, r.MaxAttempts)
			switch to {
			case domain.RunFailed:
				sev = domain.SeverityError
				text = fmt.Sprintf("Run lost on attempt %d/%d and moved to the dead-letter queue", r.Attempt, r.MaxAttempts)
			case domain.RunCancelled:
				text = "Run lost while cancellation was pending; marked cancelled"
			}
			e.RecordEvent(ctx, tx, runEventRecord(r, "run.recovered", sev, text, map[string]any{"attempt": r.Attempt}))
			f.runEvent(r, to)
			if _, err := e.SyncJobStatus(ctx, tx, j, f); err != nil {
				return err
			}
			ok = true
			return nil
		})
		if err != nil {
			return recovered, err
		}
		if ok {
			recovered++
			metrics.LeasesRecovered.Inc()
			metrics.AttemptsFinished.WithLabelValues(c.Handler, string(domain.AttemptLost)).Inc()
		}
		e.flush(ctx, f)
	}
	return recovered, nil
}

// failureTarget decides where a failed RUNNING run goes next.
func (e *Engine) failureTarget(r *domain.Run, retryable bool) (domain.RunStatus, *time.Time) {
	if !retryable || r.Attempt >= r.MaxAttempts {
		return domain.RunFailed, nil
	}
	next := e.Now().Add(backoffFor(r).Delay(r.Attempt, e.Rand))
	return domain.RunRetryWait, &next
}

// Reconcile re-publishes QUEUED runs that may be missing from Redis: runs
// whose publication failed after commit, entries lost in a Redis restart or
// flush, and entries popped by a worker that crashed before claiming. ZADD NX
// makes this idempotent, and the PostgreSQL claim makes duplicates harmless.
func (e *Engine) Reconcile(ctx context.Context, olderThan time.Duration, limit int) (int, error) {
	if e.pub == nil {
		return 0, nil
	}
	refs, err := store.UnpublishedQueued(ctx, e.pool, e.Now().Add(-olderThan), limit)
	if err != nil {
		return 0, err
	}
	if len(refs) == 0 {
		return 0, nil
	}
	f := &effects{}
	f.queueRefs(refs)
	if err := e.pub.Publish(ctx, f.publish...); err != nil {
		metrics.RedisErrors.WithLabelValues("reconcile").Inc()
		return 0, err
	}
	runIDs := make([]uuid.UUID, len(refs))
	for i, r := range refs {
		runIDs[i] = r.RunID
	}
	if err := store.MarkPublished(ctx, e.pool, runIDs, e.Now()); err != nil {
		return len(refs), err
	}
	metrics.Republished.Add(float64(len(refs)))
	return len(refs), nil
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
