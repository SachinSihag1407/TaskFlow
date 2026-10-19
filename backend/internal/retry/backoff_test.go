package retry

import (
	"testing"
	"time"
)

func TestDelayGrowsExponentiallyAndCaps(t *testing.T) {
	p := Policy{Base: 10 * time.Second, Max: 5 * time.Minute}
	maxRand := func() float64 { return 0.999999 }
	cases := []struct {
		attempt int
		ceiling time.Duration
	}{
		{1, 10 * time.Second},
		{2, 20 * time.Second},
		{3, 40 * time.Second},
		{5, 160 * time.Second},
		{6, 5 * time.Minute}, // 320s capped to 300s
		{60, 5 * time.Minute},
	}
	for _, c := range cases {
		lo, hi := p.Bounds(c.attempt)
		if hi != c.ceiling || lo != c.ceiling/2 {
			t.Errorf("attempt %d: bounds [%s,%s], want [%s,%s]", c.attempt, lo, hi, c.ceiling/2, c.ceiling)
		}
		if d := p.Delay(c.attempt, func() float64 { return 0 }); d != c.ceiling/2 {
			t.Errorf("attempt %d: min delay %s, want %s", c.attempt, d, c.ceiling/2)
		}
		if d := p.Delay(c.attempt, maxRand); d > c.ceiling || d < c.ceiling-time.Millisecond {
			t.Errorf("attempt %d: max delay %s, want ~%s", c.attempt, d, c.ceiling)
		}
	}
}

func TestJitterSpreadsRetries(t *testing.T) {
	p := Policy{Base: 10 * time.Second, Max: time.Hour}
	seen := map[time.Duration]bool{}
	for i := 0; i < 200; i++ {
		d := p.Delay(3, nil)
		lo, hi := p.Bounds(3)
		if d < lo || d > hi {
			t.Fatalf("delay %s outside [%s, %s]", d, lo, hi)
		}
		seen[d] = true
	}
	if len(seen) < 150 {
		t.Fatalf("expected jittered delays to vary, got %d distinct values", len(seen))
	}
}

func TestDegenerateInputs(t *testing.T) {
	if d := (Policy{}).Delay(1, nil); d != 0 {
		t.Fatalf("zero policy should give zero delay, got %s", d)
	}
	p := Policy{Base: time.Minute, Max: time.Second} // max < base
	if _, hi := p.Bounds(4); hi != time.Minute {
		t.Fatalf("max below base should clamp to base, got %s", hi)
	}
	if _, hi := p.Bounds(0); hi != time.Minute {
		t.Fatalf("attempt 0 treated as 1, got %s", hi)
	}
}
