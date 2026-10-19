// Package handlers defines the job-handler plugin interface and TaskFlow's
// built-in handlers. Adding a job type means implementing Handler and
// registering it; the worker runtime needs no changes.
//
// Handlers only ever run predefined code with validated JSON payloads.
// TaskFlow never executes shell commands or user-supplied code.
package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
)

// Logger records attempt logs. Messages are persisted and shown in the UI, so
// handlers must not log secrets or raw sensitive payload fields.
type Logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

// Execution is everything a handler receives for one attempt.
type Execution struct {
	RunID       uuid.UUID
	JobID       uuid.UUID
	TenantID    uuid.UUID
	AttemptID   uuid.UUID
	Attempt     int // 1-based
	MaxAttempts int
	Payload     json.RawMessage
	// Checkpoint is progress saved by a previous attempt of this run.
	Checkpoint json.RawMessage
	// IdempotencyKey is stable across all attempts of the same run. Handlers
	// with external side effects must pass it downstream (or use it as a
	// dedupe key) so a retried attempt does not repeat the effect.
	IdempotencyKey string
	Log            Logger
	// SaveCheckpoint persists progress for this run. It fails with an error
	// if the worker no longer owns the run.
	SaveCheckpoint func(ctx context.Context, cp any) error
}

// Handler is a job type.
type Handler interface {
	// Name is the stable identifier stored on jobs, e.g. "report.generate".
	Name() string
	Info() Info
	// Validate checks a payload at job creation and before execution.
	Validate(payload json.RawMessage) error
	// Execute performs one attempt. It must honour ctx cancellation (timeout,
	// operator cancel, worker shutdown) and return promptly when it fires.
	// The returned JSON is stored as the run's result.
	Execute(ctx context.Context, x *Execution) (json.RawMessage, error)
}

// Info describes a handler to API clients (create-job form).
type Info struct {
	Name           string          `json:"name"`
	Title          string          `json:"title"`
	Description    string          `json:"description"`
	Idempotency    string          `json:"idempotency"`
	ExamplePayload json.RawMessage `json:"example_payload"`
	Fields         []Field         `json:"fields"`
}

// Field documents one payload field.
type Field struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Required    bool   `json:"required"`
	Description string `json:"description"`
}

// PermanentError marks a failure that retrying cannot fix (bad input, 4xx
// response). The run goes straight to the dead-letter queue.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent wraps err as non-retryable.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &PermanentError{Err: err}
}

// Permanentf formats a non-retryable error.
func Permanentf(format string, args ...any) error {
	return &PermanentError{Err: fmt.Errorf(format, args...)}
}

// IsPermanent reports whether err is non-retryable.
func IsPermanent(err error) bool {
	var p *PermanentError
	return errors.As(err, &p)
}

// Registry holds the handlers available in a process.
type Registry struct {
	handlers map[string]Handler
}

func NewRegistry(hs ...Handler) *Registry {
	r := &Registry{handlers: map[string]Handler{}}
	for _, h := range hs {
		r.handlers[h.Name()] = h
	}
	return r
}

func (r *Registry) Get(name string) (Handler, bool) {
	h, ok := r.handlers[name]
	return h, ok
}

// Names returns registered handler names in sorted order.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.handlers))
	for n := range r.handlers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Infos returns handler descriptions in sorted order.
func (r *Registry) Infos() []Info {
	var out []Info
	for _, n := range r.Names() {
		out = append(out, r.handlers[n].Info())
	}
	return out
}

// Subset returns a registry with only the named handlers (WORKER_HANDLERS).
func (r *Registry) Subset(names []string) (*Registry, error) {
	if len(names) == 0 {
		return r, nil
	}
	sub := &Registry{handlers: map[string]Handler{}}
	for _, n := range names {
		h, ok := r.handlers[n]
		if !ok {
			return nil, fmt.Errorf("unknown handler %q", n)
		}
		sub.handlers[n] = h
	}
	return sub, nil
}

// decodeStrict unmarshals JSON rejecting unknown fields, so typos in payloads
// are reported instead of silently ignored.
func decodeStrict(payload json.RawMessage, v any) error {
	if len(payload) == 0 {
		payload = json.RawMessage("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid payload: %v", err)
	}
	if dec.More() {
		return errors.New("invalid payload: trailing data")
	}
	return nil
}

// sleepCtx waits for d or until ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
