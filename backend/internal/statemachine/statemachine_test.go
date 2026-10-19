package statemachine

import (
	"testing"

	"github.com/taskflow/taskflow/internal/domain"
)

func TestRunTransitions(t *testing.T) {
	all := []domain.RunStatus{domain.RunQueued, domain.RunRunning, domain.RunRetryWait,
		domain.RunSucceeded, domain.RunFailed, domain.RunCancelled}
	legal := map[[2]domain.RunStatus]bool{
		{domain.RunQueued, domain.RunRunning}:      true,
		{domain.RunQueued, domain.RunCancelled}:    true,
		{domain.RunRunning, domain.RunSucceeded}:   true,
		{domain.RunRunning, domain.RunRetryWait}:   true,
		{domain.RunRunning, domain.RunFailed}:      true,
		{domain.RunRunning, domain.RunCancelled}:   true,
		{domain.RunRetryWait, domain.RunQueued}:    true,
		{domain.RunRetryWait, domain.RunCancelled}: true,
	}
	// Exhaustively check every pair so an accidental table edit is caught.
	for _, from := range all {
		for _, to := range all {
			want := legal[[2]domain.RunStatus{from, to}]
			if got := CanTransition(from, to); got != want {
				t.Errorf("CanTransition(%s, %s) = %v, want %v", from, to, got, want)
			}
			if err := Validate(from, to); (err == nil) != want {
				t.Errorf("Validate(%s, %s) err = %v, want legal=%v", from, to, err, want)
			}
		}
	}
}

func TestTerminalStatesHaveNoExits(t *testing.T) {
	for _, s := range []domain.RunStatus{domain.RunSucceeded, domain.RunFailed, domain.RunCancelled} {
		if !s.Terminal() {
			t.Errorf("%s should be terminal", s)
		}
		if len(runTransitions[s]) != 0 {
			t.Errorf("terminal status %s has outgoing transitions", s)
		}
	}
}

func TestSourcesFor(t *testing.T) {
	got := map[domain.RunStatus]bool{}
	for _, s := range SourcesFor(domain.RunCancelled) {
		got[s] = true
	}
	for _, s := range []domain.RunStatus{domain.RunQueued, domain.RunRunning, domain.RunRetryWait} {
		if !got[s] {
			t.Errorf("CANCELLED should be reachable from %s", s)
		}
	}
	if len(got) != 3 {
		t.Errorf("unexpected sources for CANCELLED: %v", got)
	}
	if src := SourcesFor(domain.RunRunning); len(src) != 1 || src[0] != domain.RunQueued {
		t.Errorf("RUNNING must only be reachable from QUEUED, got %v", src)
	}
}

func ptr[T any](v T) *T { return &v }

func TestDeriveJobStatus(t *testing.T) {
	tests := []struct {
		name string
		job  JobState
		runs RunSummary
		want domain.JobStatus
	}{
		{"new job without schedule", JobState{}, RunSummary{}, domain.JobCreated},
		{"scheduled one-time", JobState{HasNextRun: true}, RunSummary{}, domain.JobScheduled},
		{"queued", JobState{}, RunSummary{Queued: 1}, domain.JobQueued},
		{"running beats queued", JobState{}, RunSummary{Running: 1, Queued: 2}, domain.JobRunning},
		{"queued beats retry wait", JobState{}, RunSummary{Queued: 1, RetryWait: 1}, domain.JobQueued},
		{"retry wait", JobState{}, RunSummary{RetryWait: 1}, domain.JobRetryWait},
		{"one-time succeeded", JobState{}, RunSummary{LastTerminal: ptr(domain.RunSucceeded)}, domain.JobSucceeded},
		{"one-time failed", JobState{}, RunSummary{LastTerminal: ptr(domain.RunFailed)}, domain.JobFailed},
		{"recurring returns to scheduled after success",
			JobState{Recurring: true, HasNextRun: true}, RunSummary{LastTerminal: ptr(domain.RunSucceeded)}, domain.JobScheduled},
		{"recurring returns to scheduled after dead-letter",
			JobState{Recurring: true, HasNextRun: true}, RunSummary{LastTerminal: ptr(domain.RunFailed)}, domain.JobScheduled},
		{"paused overrides running", JobState{Paused: true}, RunSummary{Running: 1}, domain.JobPaused},
		{"cancelled overrides everything", JobState{Cancelled: true, Paused: true, HasNextRun: true}, RunSummary{Running: 1}, domain.JobCancelled},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := DeriveJobStatus(tc.job, tc.runs); got != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestJobActions(t *testing.T) {
	t.Run("failed one-time job can be retried once", func(t *testing.T) {
		a := JobActions(domain.JobFailed, false, RunSummary{}, true)
		if !a[ActionRetry].Allowed {
			t.Fatal("retry should be allowed")
		}
		a = JobActions(domain.JobFailed, false, RunSummary{}, false)
		if a[ActionRetry].Allowed || a[ActionRetry].Reason == "" {
			t.Fatal("retry should be denied with a reason once replayed")
		}
	})
	t.Run("running one-time job cannot be paused or edited", func(t *testing.T) {
		a := JobActions(domain.JobRunning, false, RunSummary{Running: 1}, false)
		if a[ActionPause].Allowed || a[ActionEdit].Allowed {
			t.Fatal("pause/edit should be denied")
		}
		if !a[ActionCancel].Allowed {
			t.Fatal("cancel should be allowed")
		}
	})
	t.Run("recurring job can always be edited unless cancelled", func(t *testing.T) {
		if !JobActions(domain.JobRunning, true, RunSummary{Running: 1}, false)[ActionEdit].Allowed {
			t.Fatal("edit should be allowed")
		}
		if JobActions(domain.JobCancelled, true, RunSummary{}, false)[ActionEdit].Allowed {
			t.Fatal("edit should be denied when cancelled")
		}
	})
	t.Run("every denied action has a reason", func(t *testing.T) {
		statuses := []domain.JobStatus{domain.JobCreated, domain.JobScheduled, domain.JobQueued, domain.JobRunning,
			domain.JobRetryWait, domain.JobPaused, domain.JobSucceeded, domain.JobFailed, domain.JobCancelled}
		for _, s := range statuses {
			for _, rec := range []bool{true, false} {
				for name, st := range JobActions(s, rec, RunSummary{}, false) {
					if !st.Allowed && st.Reason == "" {
						t.Errorf("status=%s recurring=%v action=%s denied without reason", s, rec, name)
					}
				}
			}
		}
	})
}
