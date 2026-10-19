package schedule

import (
	"testing"
	"time"
)

func mustParse(t *testing.T, expr, tz string) *Spec {
	t.Helper()
	s, err := ParseCron(expr, tz)
	if err != nil {
		t.Fatalf("ParseCron(%q, %q): %v", expr, tz, err)
	}
	return s
}

func ts(t *testing.T, v string) time.Time {
	t.Helper()
	p, err := time.Parse(time.RFC3339, v)
	if err != nil {
		t.Fatal(err)
	}
	return p.UTC()
}

func fmtAll(ts []time.Time) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.UTC().Format(time.RFC3339)
	}
	return out
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestParseCronValidation(t *testing.T) {
	bad := []struct{ expr, tz string }{
		{"", "UTC"},
		{"* * * *", "UTC"},               // too few fields
		{"0 0 * * * *", "UTC"},           // seconds field not supported
		{"61 * * * *", "UTC"},            // out of range
		{"0 0 30 2 *", "UTC"},            // never fires
		{"CRON_TZ=UTC 0 * * * *", "UTC"}, // tz prefix
		{"@every 5m", "UTC"},
		{"0 * * * *", ""},
		{"0 * * * *", "Local"},
		{"0 * * * *", "Mars/Olympus"},
	}
	for _, c := range bad {
		if _, err := ParseCron(c.expr, c.tz); err == nil {
			t.Errorf("ParseCron(%q, %q) succeeded, want error", c.expr, c.tz)
		}
	}
	good := []string{"*/5 * * * *", "0 9 * * 1-5", "@daily", "@hourly", "15 3 1 * *", "0 0 * * SUN"}
	for _, e := range good {
		mustParse(t, e, "Europe/Berlin")
	}
}

func TestNextIsTimezoneAware(t *testing.T) {
	s := mustParse(t, "0 9 * * *", "Asia/Kolkata") // UTC+5:30, no DST
	got := s.Next(ts(t, "2026-06-01T00:00:00Z"))
	if want := ts(t, "2026-06-01T03:30:00Z"); !got.Equal(want) {
		t.Fatalf("got %s want %s", got, want)
	}
	if got.Location() != time.UTC {
		t.Fatalf("Next must return UTC, got %s", got.Location())
	}
}

func TestDSTSpringForwardFixedTimeShiftsForward(t *testing.T) {
	// 2026-03-08 02:00 EST -> 03:00 EDT in New York; 02:30 does not exist.
	s := mustParse(t, "30 2 * * *", "America/New_York")
	got := fmtAll(s.NextN(ts(t, "2026-03-07T12:00:00Z"), 3))
	want := []string{
		"2026-03-08T07:30:00Z", // 03:30 EDT: shifted forward, still runs that day
		"2026-03-09T06:30:00Z", // 02:30 EDT
		"2026-03-10T06:30:00Z",
	}
	if !equalSlices(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestDSTFallBackFixedTimeRunsOnce(t *testing.T) {
	// 2026-11-01 02:00 EDT -> 01:00 EST; 01:30 happens twice.
	s := mustParse(t, "30 1 * * *", "America/New_York")
	got := fmtAll(s.NextN(ts(t, "2026-10-31T12:00:00Z"), 3))
	want := []string{
		"2026-11-01T05:30:00Z", // 01:30 EDT (first pass only)
		"2026-11-02T06:30:00Z", // 01:30 EST next day
		"2026-11-03T06:30:00Z",
	}
	if !equalSlices(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	// Even when asked from inside the repeated hour, it must not fire again.
	if n := s.Next(ts(t, "2026-11-01T06:10:00Z")); !n.Equal(ts(t, "2026-11-02T06:30:00Z")) {
		t.Fatalf("second pass fired: %s", n)
	}
}

func TestDSTHourlyFollowsRealTime(t *testing.T) {
	s := mustParse(t, "0 * * * *", "America/New_York")
	fall := fmtAll(s.NextN(ts(t, "2026-11-01T04:30:00Z"), 3))
	if want := []string{"2026-11-01T05:00:00Z", "2026-11-01T06:00:00Z", "2026-11-01T07:00:00Z"}; !equalSlices(fall, want) {
		t.Fatalf("fall back: got %v want %v (01:00 should fire in both passes)", fall, want)
	}
	spring := fmtAll(s.NextN(ts(t, "2026-03-08T05:30:00Z"), 2))
	if want := []string{"2026-03-08T06:00:00Z", "2026-03-08T07:00:00Z"}; !equalSlices(spring, want) {
		t.Fatalf("spring forward: got %v want %v", spring, want)
	}
}

func TestPlanCronPolicies(t *testing.T) {
	s := mustParse(t, "*/10 * * * *", "UTC")
	due := ts(t, "2026-01-01T00:00:00Z")
	now := ts(t, "2026-01-01T01:00:30Z") // 7 occurrences due: 00:00 .. 01:00

	t.Run("run_once coalesces", func(t *testing.T) {
		p := PlanCron(s, due, now, PolicyRunOnce)
		if len(p.Enqueue) != 1 || !p.Enqueue[0].Equal(ts(t, "2026-01-01T01:00:00Z")) || p.Skipped != 6 {
			t.Fatalf("unexpected plan %+v", p)
		}
		if !p.Next.Equal(ts(t, "2026-01-01T01:10:00Z")) {
			t.Fatalf("next = %s", p.Next)
		}
	})
	t.Run("skip keeps only on-time occurrences", func(t *testing.T) {
		p := PlanCron(s, due, now, PolicySkip)
		if len(p.Enqueue) != 1 || !p.Enqueue[0].Equal(ts(t, "2026-01-01T01:00:00Z")) || p.Skipped != 6 {
			t.Fatalf("unexpected plan %+v", p)
		}
		late := PlanCron(s, due, ts(t, "2026-01-01T01:05:00Z"), PolicySkip)
		if len(late.Enqueue) != 0 || late.Skipped != 7 {
			t.Fatalf("all occurrences are older than grace; got %+v", late)
		}
	})
	t.Run("run_all enqueues each occurrence", func(t *testing.T) {
		p := PlanCron(s, due, now, PolicyRunAll)
		if len(p.Enqueue) != 7 || p.Skipped != 0 {
			t.Fatalf("unexpected plan %+v", p)
		}
		for i := 1; i < len(p.Enqueue); i++ {
			if !p.Enqueue[i].After(p.Enqueue[i-1]) {
				t.Fatal("occurrences must be ascending")
			}
		}
	})
	t.Run("run_all is capped", func(t *testing.T) {
		p := PlanCron(s, due, due.Add(24*time.Hour), PolicyRunAll)
		if len(p.Enqueue) != MaxCatchUp || p.Skipped != 145-MaxCatchUp {
			t.Fatalf("enqueue=%d skipped=%d", len(p.Enqueue), p.Skipped)
		}
	})
	t.Run("not yet due", func(t *testing.T) {
		p := PlanCron(s, now.Add(time.Hour), now, PolicyRunAll)
		if len(p.Enqueue) != 0 || p.Next == nil {
			t.Fatalf("unexpected plan %+v", p)
		}
	})
	t.Run("very long outage does not hang", func(t *testing.T) {
		every := mustParse(t, "* * * * *", "UTC")
		p := PlanCron(every, due, due.Add(365*24*time.Hour), PolicyRunOnce)
		if len(p.Enqueue) != 1 || p.Next == nil || !p.Next.After(due.Add(365*24*time.Hour)) {
			t.Fatalf("unexpected plan: enqueue=%d next=%v", len(p.Enqueue), p.Next)
		}
	})
}
