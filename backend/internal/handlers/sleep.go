package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"strconv"
	"time"
)

// Sleep is a long-running test job for exercising timeouts, cancellation,
// retries, crash recovery and panics. Failure decisions are deterministic per
// (run, attempt), so a given scenario reproduces exactly.
type Sleep struct{}

func (Sleep) Name() string { return "test.sleep" }

type sleepPayload struct {
	DurationMS      int     `json:"duration_ms"`
	FailAttempts    int     `json:"fail_attempts,omitempty"`
	FailProbability float64 `json:"fail_probability,omitempty"`
	Permanent       bool    `json:"permanent,omitempty"`
	Panic           bool    `json:"panic,omitempty"`
}

func (Sleep) Info() Info {
	return Info{
		Name:        "test.sleep",
		Title:       "Long-running test job",
		Description: "Sleeps for a configurable duration and can simulate failures, permanent errors and panics.",
		Idempotency: "No side effects.",
		ExamplePayload: mustJSON(map[string]any{
			"duration_ms": 5000, "fail_attempts": 1,
		}),
		Fields: []Field{
			{Name: "duration_ms", Type: "integer", Required: true, Description: "How long to run (0-3,600,000 ms)"},
			{Name: "fail_attempts", Type: "integer", Description: "Fail the first N attempts (0-25)"},
			{Name: "fail_probability", Type: "number", Description: "Deterministic per-attempt failure probability (0-1)"},
			{Name: "permanent", Type: "boolean", Description: "Make simulated failures permanent (no retry)"},
			{Name: "panic", Type: "boolean", Description: "Panic instead of returning an error (tests panic recovery)"},
		},
	}
}

func (Sleep) parse(payload json.RawMessage) (*sleepPayload, error) {
	var p sleepPayload
	if err := decodeStrict(payload, &p); err != nil {
		return nil, err
	}
	switch {
	case p.DurationMS < 0 || p.DurationMS > 3_600_000:
		return nil, errors.New("duration_ms must be between 0 and 3,600,000")
	case p.FailAttempts < 0 || p.FailAttempts > 25:
		return nil, errors.New("fail_attempts must be between 0 and 25")
	case p.FailProbability < 0 || p.FailProbability > 1:
		return nil, errors.New("fail_probability must be between 0 and 1")
	}
	return &p, nil
}

func (s Sleep) Validate(payload json.RawMessage) error {
	_, err := s.parse(payload)
	return err
}

func (s Sleep) Execute(ctx context.Context, x *Execution) (json.RawMessage, error) {
	p, err := s.parse(x.Payload)
	if err != nil {
		return nil, Permanent(err)
	}
	total := time.Duration(p.DurationMS) * time.Millisecond
	x.Log.Info(fmt.Sprintf("sleeping for %s (attempt %d/%d)", total, x.Attempt, x.MaxAttempts))
	deadline := time.Now().Add(total)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		step := min(remaining, time.Second)
		if err := sleepCtx(ctx, step); err != nil {
			x.Log.Warn("interrupted: " + err.Error())
			return nil, err
		}
		if total >= 3*time.Second {
			done := total - time.Until(deadline)
			x.Log.Info(fmt.Sprintf("progress %d%%", int(100*done/total)))
		}
	}

	fail := x.Attempt <= p.FailAttempts
	if !fail && p.FailProbability > 0 {
		h := fnv.New64a()
		_, _ = h.Write([]byte(x.RunID.String() + ":" + strconv.Itoa(x.Attempt)))
		fail = float64(h.Sum64()%10_000)/10_000 < p.FailProbability
	}
	if fail {
		if p.Panic {
			panic(fmt.Sprintf("simulated panic on attempt %d", x.Attempt))
		}
		err := fmt.Errorf("simulated failure on attempt %d", x.Attempt)
		x.Log.Error(err.Error())
		if p.Permanent {
			return nil, Permanent(err)
		}
		return nil, err
	}
	x.Log.Info("finished")
	return mustJSON(map[string]any{"slept_ms": p.DurationMS, "attempt": x.Attempt}), nil
}
