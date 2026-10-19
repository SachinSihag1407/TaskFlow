package worker_test

import (
	"context"

	"github.com/google/uuid"

	"github.com/taskflow/taskflow/internal/domain"
	"github.com/taskflow/taskflow/internal/handlers"
)

type nopLogger struct{}

func (nopLogger) Info(string, ...any)  {}
func (nopLogger) Warn(string, ...any)  {}
func (nopLogger) Error(string, ...any) {}

func testExecution(r *domain.Run, attempt int) *handlers.Execution {
	return &handlers.Execution{
		RunID: r.ID, JobID: r.JobID, TenantID: r.TenantID, AttemptID: uuid.New(), Attempt: attempt,
		MaxAttempts: r.MaxAttempts, Payload: r.Payload, IdempotencyKey: r.ID.String(), Log: nopLogger{},
		SaveCheckpoint: func(context.Context, any) error { return nil },
	}
}
