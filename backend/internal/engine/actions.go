package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/taskflow/taskflow/internal/db"
	"github.com/taskflow/taskflow/internal/domain"
	"github.com/taskflow/taskflow/internal/metrics"
	"github.com/taskflow/taskflow/internal/statemachine"
	"github.com/taskflow/taskflow/internal/store"
)

// requireAction loads the job's run summary and fails with a conflict if the
// action is not currently permitted, using the same rules the API exposes.
func (e *Engine) requireAction(ctx context.Context, tx pgx.Tx, j *domain.Job, action string) (statemachine.RunSummary, error) {
	summary, err := store.ActiveRunSummary(ctx, tx, j.ID)
	if err != nil {
		return summary, err
	}
	replayable := false
	if action == statemachine.ActionRetry {
		if r, err := store.LastFailedRun(ctx, tx, j.ID); err == nil {
			replayable = r.ReplayedAt == nil
		} else if !errors.Is(err, domain.ErrNotFound) {
			return summary, err
		}
	}
	st := statemachine.JobActions(j.Status, j.Recurring(), summary, replayable)[action]
	if !st.Allowed {
		return summary, domain.Conflict("action_not_allowed", "%s", st.Reason)
	}
	return summary, nil
}

// CancelJob cancels a job: its schedule stops, waiting runs are cancelled and
// running runs are asked to stop cooperatively.
func (e *Engine) CancelJob(ctx context.Context, actor *domain.Principal, jobID uuid.UUID) (*domain.Job, error) {
	f := &effects{}
	var job *domain.Job
	err := db.WithTx(ctx, e.pool, func(tx pgx.Tx) error {
		now := e.Now()
		j, err := store.LockTenantJob(ctx, tx, actor.TenantID, jobID)
		if err != nil {
			return err
		}
		if _, err := e.requireAction(ctx, tx, j, statemachine.ActionCancel); err != nil {
			return err
		}
		if err := store.SetJobCancelled(ctx, tx, j.ID, now); err != nil {
			return err
		}
		j.CancelledAt, j.NextRunAt = &now, nil
		j.Version++
		runs, err := store.LockActiveRuns(ctx, tx, j.ID)
		if err != nil {
			return err
		}
		stopping := 0
		for _, r := range runs {
			if err := e.cancelRunLocked(ctx, tx, j, r, f); err != nil {
				return err
			}
			if r.Status == domain.RunRunning {
				stopping++
			}
		}
		msg := "Job cancelled"
		if stopping > 0 {
			msg = fmt.Sprintf("Job cancelled; %d running run(s) asked to stop", stopping)
		}
		e.RecordEvent(ctx, tx, jobEvent(j, "job.cancelled", domain.SeverityInfo, msg))
		if err := e.Audit(ctx, tx, actor, "job.cancel", "job", j.ID.String(), map[string]any{"name": j.Name}); err != nil {
			return err
		}
		if _, err := e.SyncJobStatus(ctx, tx, j, f); err != nil {
			return err
		}
		job = j
		return nil
	})
	if err != nil {
		return nil, err
	}
	e.flush(ctx, f)
	return job, nil
}

// cancelRunLocked cancels a locked active run. Waiting runs move straight to
// CANCELLED; a RUNNING run is flagged and its worker cancels the handler's
// context. The run reaches CANCELLED when the worker reports back — or, if
// the handler finished first, it stays SUCCEEDED: the work really happened.
func (e *Engine) cancelRunLocked(ctx context.Context, tx pgx.Tx, j *domain.Job, r *domain.Run, f *effects) error {
	now := e.Now()
	switch r.Status {
	case domain.RunQueued, domain.RunRetryWait:
		if err := store.TransitionRun(ctx, tx, r.ID, r.Status, store.RunTransition{To: domain.RunCancelled, Now: now}); err != nil {
			return err
		}
		f.removes = append(f.removes, r.ID)
		f.runEvent(r, domain.RunCancelled)
		// A cancelled run is a terminal outcome: record it so the job status
		// derives correctly (e.g. a one-time job whose only run was cancelled).
		cancelled := domain.RunCancelled
		if err := store.SetJobLastRun(ctx, tx, j.ID, now, cancelled); err != nil {
			return err
		}
		j.LastRunStatus, j.LastRunAt = &cancelled, &now
		metrics.RunsFinished.WithLabelValues(r.Handler, string(domain.RunCancelled)).Inc()
	case domain.RunRunning:
		if err := store.SetCancelRequested(ctx, tx, r.ID, now); err != nil {
			return err
		}
		f.cancels = append(f.cancels, r.ID)
		f.runEvent(r, domain.RunRunning)
	default:
		return domain.Conflict("run_finished", "run already finished with status %s", r.Status)
	}
	return nil
}

// CancelRun cancels a single run of a job.
func (e *Engine) CancelRun(ctx context.Context, actor *domain.Principal, runID uuid.UUID) (*domain.Run, error) {
	peek, err := store.GetRun(ctx, e.pool, actor.TenantID, runID)
	if err != nil {
		return nil, err
	}
	f := &effects{}
	var out *domain.Run
	err = db.WithTx(ctx, e.pool, func(tx pgx.Tx) error {
		j, err := store.LockTenantJob(ctx, tx, actor.TenantID, peek.JobID)
		if err != nil {
			return err
		}
		r, err := store.LockRun(ctx, tx, runID)
		if err != nil {
			return err
		}
		if r.Status == domain.RunRunning && r.CancelRequestedAt != nil {
			return domain.Conflict("cancel_pending", "cancellation is already pending for this run")
		}
		if err := e.cancelRunLocked(ctx, tx, j, r, f); err != nil {
			return err
		}
		if r.Status != domain.RunRunning {
			e.RecordEvent(ctx, tx, runEventRecord(r, "run.cancelled", domain.SeverityInfo, "Run cancelled before execution", nil))
		} else {
			e.RecordEvent(ctx, tx, runEventRecord(r, "run.cancel_requested", domain.SeverityInfo, "Cancellation requested for running run", nil))
		}
		if err := e.Audit(ctx, tx, actor, "run.cancel", "run", r.ID.String(), map[string]any{"job_id": j.ID.String(), "status": r.Status}); err != nil {
			return err
		}
		if _, err := e.SyncJobStatus(ctx, tx, j, f); err != nil {
			return err
		}
		out, err = store.GetRun(ctx, tx, actor.TenantID, runID)
		return err
	})
	if err != nil {
		return nil, err
	}
	e.flush(ctx, f)
	return out, nil
}

// PauseJob stops new occurrences from being scheduled and holds QUEUED runs.
// Runs already executing are not interrupted.
func (e *Engine) PauseJob(ctx context.Context, actor *domain.Principal, jobID uuid.UUID) (*domain.Job, error) {
	return e.togglePause(ctx, actor, jobID, true)
}

// ResumeJob reverses PauseJob. Occurrences missed while paused are handled by
// the job's missed-run policy on the next scheduler tick.
func (e *Engine) ResumeJob(ctx context.Context, actor *domain.Principal, jobID uuid.UUID) (*domain.Job, error) {
	return e.togglePause(ctx, actor, jobID, false)
}

func (e *Engine) togglePause(ctx context.Context, actor *domain.Principal, jobID uuid.UUID, pause bool) (*domain.Job, error) {
	f := &effects{}
	var job *domain.Job
	err := db.WithTx(ctx, e.pool, func(tx pgx.Tx) error {
		now := e.Now()
		j, err := store.LockTenantJob(ctx, tx, actor.TenantID, jobID)
		if err != nil {
			return err
		}
		action, auditAction, evType, msg := statemachine.ActionPause, "job.pause", "job.paused", "Job paused"
		if !pause {
			action, auditAction, evType, msg = statemachine.ActionResume, "job.resume", "job.resumed", "Job resumed"
		}
		if _, err := e.requireAction(ctx, tx, j, action); err != nil {
			return err
		}
		var at *time.Time
		if pause {
			at = &now
		}
		if err := store.SetJobPaused(ctx, tx, j.ID, at, now); err != nil {
			return err
		}
		j.PausedAt = at
		j.Version++
		if !pause {
			refs, err := store.QueuedRefsForJob(ctx, tx, j.ID, 500)
			if err != nil {
				return err
			}
			f.queueRefs(refs)
		}
		e.RecordEvent(ctx, tx, jobEvent(j, evType, domain.SeverityInfo, msg))
		if err := e.Audit(ctx, tx, actor, auditAction, "job", j.ID.String(), map[string]any{"name": j.Name}); err != nil {
			return err
		}
		if _, err := e.SyncJobStatus(ctx, tx, j, f); err != nil {
			return err
		}
		job = j
		return nil
	})
	if err != nil {
		return nil, err
	}
	e.flush(ctx, f)
	return job, nil
}

// RetryJob re-runs a failed one-time job by replaying its last failed run.
func (e *Engine) RetryJob(ctx context.Context, actor *domain.Principal, jobID uuid.UUID) (*domain.Run, error) {
	var newRun *domain.Run
	f := &effects{}
	err := db.WithTx(ctx, e.pool, func(tx pgx.Tx) error {
		j, err := store.LockTenantJob(ctx, tx, actor.TenantID, jobID)
		if err != nil {
			return err
		}
		if _, err := e.requireAction(ctx, tx, j, statemachine.ActionRetry); err != nil {
			return err
		}
		last, err := store.LastFailedRun(ctx, tx, j.ID)
		if err != nil {
			return err
		}
		newRun, err = e.replayLocked(ctx, tx, actor, j, last.ID, domain.TriggerRetry, f)
		return err
	})
	if err != nil {
		return nil, err
	}
	e.flush(ctx, f)
	return newRun, nil
}

// ReplayRun re-enqueues a dead-lettered run as a new run linked through
// replay_of. Each failed run can be replayed at most once (unique index), so
// concurrent replay requests cannot double-enqueue.
func (e *Engine) ReplayRun(ctx context.Context, actor *domain.Principal, runID uuid.UUID) (*domain.Run, error) {
	peek, err := store.GetRun(ctx, e.pool, actor.TenantID, runID)
	if err != nil {
		return nil, err
	}
	var newRun *domain.Run
	f := &effects{}
	err = db.WithTx(ctx, e.pool, func(tx pgx.Tx) error {
		j, err := store.LockTenantJob(ctx, tx, actor.TenantID, peek.JobID)
		if err != nil {
			return err
		}
		if j.CancelledAt != nil {
			return domain.Conflict("job_cancelled", "the parent job is cancelled; runs of cancelled jobs cannot be replayed")
		}
		newRun, err = e.replayLocked(ctx, tx, actor, j, runID, domain.TriggerReplay, f)
		return err
	})
	if err != nil {
		return nil, err
	}
	e.flush(ctx, f)
	return newRun, nil
}

func (e *Engine) replayLocked(ctx context.Context, tx pgx.Tx, actor *domain.Principal, j *domain.Job, runID uuid.UUID,
	trigger domain.Trigger, f *effects) (*domain.Run, error) {
	now := e.Now()
	old, err := store.LockRun(ctx, tx, runID)
	if err != nil {
		return nil, err
	}
	if old.Status != domain.RunFailed {
		return nil, domain.Conflict("not_replayable", "only failed runs can be replayed (status is %s)", old.Status)
	}
	if old.ReplayedAt != nil {
		return nil, domain.Conflict("already_replayed", "this run has already been replayed")
	}
	if err := store.MarkReplayed(ctx, tx, old.ID, now); err != nil {
		return nil, err
	}
	// Replays reproduce the original execution: same payload and policy
	// snapshot as the failed run, fresh attempt budget.
	nr := *old
	nr.ID = uuid.New()
	nr.ScheduledFor = now
	nr.Trigger = trigger
	nr.Status = domain.RunQueued
	nr.QueuedAt = &now
	nr.ReplayOf = &old.ID
	nr.CreatedAt, nr.UpdatedAt = now, now
	if _, err := store.InsertRun(ctx, tx, &nr); err != nil {
		if db.IsUniqueViolation(err, "job_runs_replay_key") {
			return nil, domain.Conflict("already_replayed", "this run has already been replayed")
		}
		return nil, err
	}
	metrics.RunsEnqueued.WithLabelValues(string(trigger)).Inc()
	verb := "Replayed dead-lettered run"
	if trigger == domain.TriggerRetry {
		verb = "Manually retried failed run"
	}
	e.RecordEvent(ctx, tx, runEventRecord(&nr, "run."+string(trigger), domain.SeverityInfo,
		fmt.Sprintf("%s %s", verb, old.ID.String()[:8]), map[string]any{"replay_of": old.ID.String()}))
	if err := e.Audit(ctx, tx, actor, "run."+string(trigger), "run", old.ID.String(),
		map[string]any{"job_id": j.ID.String(), "new_run_id": nr.ID.String(), "job_name": j.Name}); err != nil {
		return nil, err
	}
	if _, err := e.SyncJobStatus(ctx, tx, j, f); err != nil {
		return nil, err
	}
	f.queueRun(&nr)
	f.runEvent(&nr, domain.RunQueued)
	nr.JobName = j.Name
	return &nr, nil
}

// RunNow enqueues an immediate, manually triggered run of a recurring job.
func (e *Engine) RunNow(ctx context.Context, actor *domain.Principal, jobID uuid.UUID) (*domain.Run, error) {
	var run *domain.Run
	f := &effects{}
	err := db.WithTx(ctx, e.pool, func(tx pgx.Tx) error {
		now := e.Now()
		j, err := store.LockTenantJob(ctx, tx, actor.TenantID, jobID)
		if err != nil {
			return err
		}
		summary, err := e.requireAction(ctx, tx, j, statemachine.ActionRunNow)
		if err != nil {
			return err
		}
		if j.OverlapPolicy == domain.OverlapSkip && summary.Active() > 0 {
			return domain.Conflict("run_active", "a run is already active and the overlap policy is skip")
		}
		run = store.NewRunFromJob(j, now, domain.TriggerManual, now)
		if _, err := store.InsertRun(ctx, tx, run); err != nil {
			return err
		}
		metrics.RunsEnqueued.WithLabelValues(string(domain.TriggerManual)).Inc()
		e.RecordEvent(ctx, tx, runEventRecord(run, "run.manual", domain.SeverityInfo, "Run triggered manually", nil))
		if err := e.Audit(ctx, tx, actor, "job.run_now", "job", j.ID.String(), map[string]any{"run_id": run.ID.String()}); err != nil {
			return err
		}
		if _, err := e.SyncJobStatus(ctx, tx, j, f); err != nil {
			return err
		}
		f.queueRun(run)
		f.runEvent(run, domain.RunQueued)
		run.JobName = j.Name
		return nil
	})
	if err != nil {
		return nil, err
	}
	e.flush(ctx, f)
	return run, nil
}
