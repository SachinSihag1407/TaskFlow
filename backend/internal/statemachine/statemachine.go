// Package statemachine is the single source of truth for TaskFlow's lifecycle
// rules: which run transitions are legal, how a job's aggregate status is
// derived from its runs, and which user actions are currently permitted.
//
// The engine and repositories call into this package; no other code decides
// whether a status change is valid.
package statemachine

import (
	"fmt"

	"github.com/taskflow/taskflow/internal/domain"
)

// runTransitions enumerates every legal run status change.
//
//	QUEUED     -> RUNNING (claimed) | CANCELLED
//	RUNNING    -> SUCCEEDED | RETRY_WAIT (retryable failure, lease lost) |
//	              FAILED (attempts exhausted / permanent error) | CANCELLED
//	RETRY_WAIT -> QUEUED (backoff elapsed) | CANCELLED
//
// SUCCEEDED, FAILED and CANCELLED are terminal. A dead-lettered (FAILED) run is
// never revived; replay creates a new run linked through replay_of.
var runTransitions = map[domain.RunStatus][]domain.RunStatus{
	domain.RunQueued:    {domain.RunRunning, domain.RunCancelled},
	domain.RunRunning:   {domain.RunSucceeded, domain.RunRetryWait, domain.RunFailed, domain.RunCancelled},
	domain.RunRetryWait: {domain.RunQueued, domain.RunCancelled},
}

// CanTransition reports whether a run may move from one status to another.
func CanTransition(from, to domain.RunStatus) bool {
	for _, s := range runTransitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// ErrInvalidTransition is returned when a transition is not in the table.
type ErrInvalidTransition struct {
	From, To domain.RunStatus
}

func (e *ErrInvalidTransition) Error() string {
	return fmt.Sprintf("invalid run transition %s -> %s", e.From, e.To)
}

// Validate returns an error if the transition is illegal.
func Validate(from, to domain.RunStatus) error {
	if !CanTransition(from, to) {
		return &ErrInvalidTransition{From: from, To: to}
	}
	return nil
}

// SourcesFor returns every status from which `to` can be reached. Repositories
// use it to build conditional UPDATE ... WHERE status = ANY($sources) clauses,
// which makes each transition atomic under concurrency.
func SourcesFor(to domain.RunStatus) []domain.RunStatus {
	var out []domain.RunStatus
	for from, tos := range runTransitions {
		for _, t := range tos {
			if t == to {
				out = append(out, from)
			}
		}
	}
	return out
}

// RunSummary is the information about a job's runs needed to derive the
// job's aggregate status.
type RunSummary struct {
	Running   int
	Queued    int
	RetryWait int
	// LastTerminal is the status of the most recently finished run, if any.
	LastTerminal *domain.RunStatus
	TotalRuns    int
}

// Active reports whether any run is still in flight.
func (s RunSummary) Active() int { return s.Running + s.Queued + s.RetryWait }

// JobState is the subset of job fields that influence its aggregate status.
type JobState struct {
	Recurring  bool
	Paused     bool
	Cancelled  bool
	HasNextRun bool
}

// DeriveJobStatus computes a job's aggregate status deterministically:
//
//  1. CANCELLED and PAUSED are operator decisions and take precedence.
//  2. Otherwise the most advanced in-flight run wins: RUNNING > QUEUED > RETRY_WAIT.
//  3. Otherwise, if another occurrence is scheduled, SCHEDULED.
//  4. Otherwise the outcome of the last finished run (one-time jobs end here).
//  5. A job with no runs and no schedule is CREATED.
func DeriveJobStatus(j JobState, runs RunSummary) domain.JobStatus {
	switch {
	case j.Cancelled:
		return domain.JobCancelled
	case j.Paused:
		return domain.JobPaused
	case runs.Running > 0:
		return domain.JobRunning
	case runs.Queued > 0:
		return domain.JobQueued
	case runs.RetryWait > 0:
		return domain.JobRetryWait
	case j.HasNextRun:
		return domain.JobScheduled
	case runs.LastTerminal != nil:
		switch *runs.LastTerminal {
		case domain.RunSucceeded:
			return domain.JobSucceeded
		case domain.RunFailed:
			return domain.JobFailed
		case domain.RunCancelled:
			return domain.JobCancelled
		}
	}
	return domain.JobCreated
}

// Action names exposed to API clients.
const (
	ActionEdit   = "edit"
	ActionPause  = "pause"
	ActionResume = "resume"
	ActionCancel = "cancel"
	ActionRetry  = "retry"
	ActionRunNow = "run_now"
)

// ActionState tells the UI whether an action is available and, if not, why.
type ActionState struct {
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason,omitempty"`
}

func allow() ActionState             { return ActionState{Allowed: true} }
func deny(reason string) ActionState { return ActionState{Reason: reason} }

// JobActions computes which job-level actions are currently valid. The same
// rules are enforced server-side when an action is invoked; this function only
// lets clients explain unavailable actions without guessing.
func JobActions(status domain.JobStatus, recurring bool, runs RunSummary, lastRunReplayable bool) map[string]ActionState {
	a := map[string]ActionState{}
	finishedOnce := !recurring && (status == domain.JobSucceeded || status == domain.JobFailed)

	// Edit
	switch {
	case status == domain.JobCancelled:
		a[ActionEdit] = deny("Cancelled jobs cannot be edited.")
	case finishedOnce:
		a[ActionEdit] = deny("This one-time job has already finished. Create a new job instead.")
	case !recurring && runs.Active() > 0:
		a[ActionEdit] = deny("A run is in progress and uses the configuration captured when it was queued.")
	default:
		a[ActionEdit] = allow()
	}

	// Pause / resume
	switch {
	case status == domain.JobPaused:
		a[ActionPause] = deny("The job is already paused.")
		a[ActionResume] = allow()
	case status == domain.JobCancelled:
		a[ActionPause] = deny("Cancelled jobs cannot be paused.")
		a[ActionResume] = deny("Cancelled jobs cannot be resumed.")
	case finishedOnce:
		a[ActionPause] = deny("This one-time job has already finished.")
		a[ActionResume] = deny("The job is not paused.")
	case !recurring && status == domain.JobRunning:
		a[ActionPause] = deny("The job is executing. Cancel it instead, or wait for it to finish.")
		a[ActionResume] = deny("The job is not paused.")
	default:
		a[ActionPause] = allow()
		a[ActionResume] = deny("The job is not paused.")
	}

	// Cancel
	switch {
	case status == domain.JobCancelled:
		a[ActionCancel] = deny("The job is already cancelled.")
	case finishedOnce:
		a[ActionCancel] = deny("This one-time job has already finished.")
	default:
		a[ActionCancel] = allow()
	}

	// Retry (one-time jobs only; recurring runs are replayed individually)
	switch {
	case recurring:
		a[ActionRetry] = deny("Recurring jobs retry automatically. Replay individual failed runs from the dead-letter queue.")
	case status != domain.JobFailed:
		a[ActionRetry] = deny("Only failed jobs can be retried.")
	case !lastRunReplayable:
		a[ActionRetry] = deny("The failed run has already been retried.")
	default:
		a[ActionRetry] = allow()
	}

	// Run now (recurring only)
	switch {
	case !recurring:
		a[ActionRunNow] = deny("Run now is available for recurring jobs.")
	case status == domain.JobCancelled:
		a[ActionRunNow] = deny("Cancelled jobs cannot be run.")
	case status == domain.JobPaused:
		a[ActionRunNow] = deny("Resume the job before running it.")
	default:
		a[ActionRunNow] = allow()
	}
	return a
}
