// Package schedule parses cron expressions, evaluates them in a job's IANA
// timezone, and plans which occurrences to enqueue when a job becomes due,
// applying the job's missed-run policy.
//
// All returned times are UTC. Cron fields are interpreted as wall-clock time
// in the configured timezone, so "0 9 * * *" in Europe/Berlin fires at 09:00
// Berlin time year-round (07:00 or 08:00 UTC depending on DST).
package schedule

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// MisfireGrace is how late an occurrence may be and still count as "on time".
// Occurrences older than this are "missed" and handled by the missed-run
// policy. It comfortably exceeds normal scheduler tick lag.
const MisfireGrace = time.Minute

// MaxCatchUp caps how many missed occurrences run_all will enqueue at once so a
// long outage on a minutely job cannot flood the queue.
const MaxCatchUp = 50

// maxScan bounds the occurrence iteration for very long outages.
const maxScan = 10_000

var parser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// Spec is a parsed cron expression bound to a timezone.
//
// Daylight-saving semantics follow Vixie cron / cronie:
//
//   - Expressions with a fixed hour (e.g. "30 2 * * *") are evaluated in
//     wall-clock time. A wall time that does not exist because clocks jumped
//     forward runs at the equivalent instant after the jump (02:30 -> 03:30).
//     A wall time that occurs twice because clocks fell back runs only once,
//     on its first occurrence. A daily job therefore runs exactly once per day.
//   - Expressions that fire every hour ("0 * * * *", "*/15 * * * *",
//     "@hourly") follow real elapsed time: they run during both passes of a
//     repeated hour, and there is nothing to run in an hour that is skipped.
type Spec struct {
	expr      string
	loc       *time.Location
	sched     cron.Schedule
	everyHour bool
}

// LoadTimezone validates and loads an IANA timezone name.
func LoadTimezone(tz string) (*time.Location, error) {
	if tz == "" {
		return nil, errors.New("timezone is required")
	}
	// time.LoadLocation accepts "Local", which would make behaviour depend on
	// the host; only explicit zones are allowed.
	if strings.EqualFold(tz, "local") {
		return nil, errors.New("timezone must be an IANA name such as UTC or Europe/Berlin")
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("unknown timezone %q", tz)
	}
	return loc, nil
}

// ParseCron parses a standard 5-field cron expression (or a descriptor such
// as @hourly / @daily) evaluated in the given timezone. Seconds fields and
// CRON_TZ prefixes are rejected so the timezone is always the explicit one.
func ParseCron(expr, tz string) (*Spec, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, errors.New("cron expression is required")
	}
	if len(expr) > 100 {
		return nil, errors.New("cron expression is too long")
	}
	upper := strings.ToUpper(expr)
	if strings.HasPrefix(upper, "CRON_TZ=") || strings.HasPrefix(upper, "TZ=") {
		return nil, errors.New("set the timezone field instead of a TZ prefix")
	}
	if strings.HasPrefix(expr, "@every") {
		return nil, errors.New("@every is not supported; use a cron expression")
	}
	loc, err := LoadTimezone(tz)
	if err != nil {
		return nil, err
	}
	sched, err := parser.Parse(expr)
	if err != nil {
		return nil, fmt.Errorf("invalid cron expression: %v", err)
	}
	s := &Spec{expr: expr, loc: loc, sched: sched}
	if spec, ok := sched.(*cron.SpecSchedule); ok {
		const allHours = 1<<24 - 1
		s.everyHour = spec.Hour&allHours == allHours
	}
	// Expressions such as "0 0 30 2 *" (Feb 30) never fire; robfig returns
	// the zero time after a five-year search.
	if s.Next(time.Now()).IsZero() {
		return nil, errors.New("cron expression never fires")
	}
	return s, nil
}

// Expr returns the original expression.
func (s *Spec) Expr() string { return s.expr }

// Next returns the first occurrence strictly after t, in UTC. It returns the
// zero time if the expression never fires.
func (s *Spec) Next(t time.Time) time.Time {
	if s.everyHour {
		// Real elapsed time: robfig evaluates directly in the zone.
		n := s.sched.Next(t.In(s.loc))
		if n.IsZero() {
			return n
		}
		return n.UTC()
	}
	// Wall-clock time: evaluate the expression against a DST-free "fake UTC"
	// copy of the local wall clock, then map each candidate back to a real
	// instant. Candidates that map to an instant not after t (the second pass
	// of a repeated hour) are skipped, so each wall time fires at most once.
	w := wallClock(t, s.loc)
	for i := 0; i < 1000; i++ {
		w = s.sched.Next(w)
		if w.IsZero() {
			return w
		}
		if inst := wallToInstant(w, s.loc); inst.After(t) {
			return inst.UTC()
		}
	}
	return time.Time{}
}

// wallClock returns t's local wall-clock reading in loc, expressed as a UTC
// time with the same fields.
func wallClock(t time.Time, loc *time.Location) time.Time {
	l := t.In(loc)
	return time.Date(l.Year(), l.Month(), l.Day(), l.Hour(), l.Minute(), l.Second(), l.Nanosecond(), time.UTC)
}

// wallToInstant maps a wall-clock reading (fields of w, which is in fake UTC)
// to a real instant in loc:
//   - normal time: the unique instant;
//   - repeated (fall back): the earlier instant;
//   - non-existent (spring forward): shifted forward by the size of the gap.
func wallToInstant(w time.Time, loc *time.Location) time.Time {
	// The real instant is within ±14h of w. Offsets 26h either side bracket
	// any single DST transition affecting it.
	_, offBefore := w.Add(-26 * time.Hour).In(loc).Zone()
	_, offAfter := w.Add(26 * time.Hour).In(loc).Zone()
	c1 := w.Add(-time.Duration(offBefore) * time.Second)
	c2 := w.Add(-time.Duration(offAfter) * time.Second)
	v1 := wallClock(c1, loc).Equal(w)
	v2 := wallClock(c2, loc).Equal(w)
	switch {
	case v1 && v2:
		if c2.Before(c1) {
			return c2
		}
		return c1
	case v1:
		return c1
	case v2:
		return c2
	default:
		// Gap: interpreting the wall time with the pre-transition offset
		// lands the same distance past the transition as it was into the gap.
		return c1
	}
}

// NextN returns up to n occurrences strictly after t.
func (s *Spec) NextN(t time.Time, n int) []time.Time {
	out := make([]time.Time, 0, n)
	for i := 0; i < n; i++ {
		t = s.Next(t)
		if t.IsZero() {
			break
		}
		out = append(out, t)
	}
	return out
}

// Plan is the scheduler's decision for one due job.
type Plan struct {
	// Enqueue lists the occurrence times to create runs for, ascending.
	Enqueue []time.Time
	// Skipped counts due occurrences that will not run (missed-run policy).
	Skipped int
	// Next is the next future occurrence, or nil when there is none.
	Next *time.Time
}

// PolicyName is the subset of missed-run policies the planner understands.
type PolicyName string

const (
	PolicySkip    PolicyName = "skip"
	PolicyRunOnce PolicyName = "run_once"
	PolicyRunAll  PolicyName = "run_all"
)

// PlanCron decides which occurrences of a recurring job to enqueue given that
// its stored next_run_at (`due`) is at or before `now`.
//
//   - skip:     run occurrences that are within MisfireGrace; drop older ones.
//   - run_once: coalesce every due occurrence into a single run (the latest).
//   - run_all:  run every due occurrence, capped at MaxCatchUp (latest kept).
func PlanCron(s *Spec, due, now time.Time, policy PolicyName) Plan {
	var occ []time.Time
	t := due
	scanned := 0
	for !t.IsZero() && !t.After(now) && scanned < maxScan {
		occ = append(occ, t)
		t = s.Next(t)
		scanned++
	}
	var next time.Time
	if scanned >= maxScan {
		// Pathological outage: stop scanning and jump straight to the future.
		next = s.Next(now)
	} else {
		next = t
	}

	p := Plan{}
	if !next.IsZero() {
		n := next
		p.Next = &n
	}
	if len(occ) == 0 {
		return p
	}

	switch policy {
	case PolicySkip:
		for _, o := range occ {
			if now.Sub(o) <= MisfireGrace {
				p.Enqueue = append(p.Enqueue, o)
			} else {
				p.Skipped++
			}
		}
	case PolicyRunAll:
		if len(occ) > MaxCatchUp {
			p.Skipped = len(occ) - MaxCatchUp
			occ = occ[len(occ)-MaxCatchUp:]
		}
		p.Enqueue = occ
	default: // run_once
		p.Enqueue = []time.Time{occ[len(occ)-1]}
		p.Skipped = len(occ) - 1
	}
	return p
}
