// Package retry computes retry delays.
package retry

import (
	"math"
	"math/rand/v2"
	"time"
)

// Policy is an exponential backoff policy.
type Policy struct {
	Base time.Duration
	Max  time.Duration
}

// RandFunc returns a float in [0, 1). Injected for deterministic tests.
type RandFunc func() float64

// Delay returns the wait before the next attempt, given the number of the
// attempt that just failed (1-based).
//
// The un-jittered delay is min(Max, Base * 2^(attempt-1)). "Equal jitter" is
// then applied: half of the delay is fixed and the other half random. This
// keeps a guaranteed minimum backoff (unlike full jitter, which can retry
// almost immediately) while still spreading out retries from many runs that
// failed at the same moment, e.g. during a downstream outage.
func (p Policy) Delay(failedAttempt int, rnd RandFunc) time.Duration {
	if rnd == nil {
		rnd = rand.Float64
	}
	return p.ceiling(failedAttempt)/2 + time.Duration(rnd()*float64(p.ceiling(failedAttempt)/2))
}

// ceiling returns the un-jittered delay.
func (p Policy) ceiling(failedAttempt int) time.Duration {
	if failedAttempt < 1 {
		failedAttempt = 1
	}
	if p.Base <= 0 {
		return 0
	}
	max := p.Max
	if max < p.Base {
		max = p.Base
	}
	exp := math.Pow(2, float64(failedAttempt-1))
	d := float64(p.Base) * exp
	if d > float64(max) || math.IsInf(d, 0) {
		return max
	}
	return time.Duration(d)
}

// Bounds returns the [min, max] range Delay can produce for an attempt.
func (p Policy) Bounds(failedAttempt int) (time.Duration, time.Duration) {
	c := p.ceiling(failedAttempt)
	return c / 2, c
}
