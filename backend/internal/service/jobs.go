// Package service contains TaskFlow's business logic between the HTTP layer
// and the repositories: validation, authorization-aware orchestration and
// composition of engine operations.
package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/taskflow/taskflow/internal/db"
	"github.com/taskflow/taskflow/internal/domain"
	"github.com/taskflow/taskflow/internal/engine"
	"github.com/taskflow/taskflow/internal/handlers"
	"github.com/taskflow/taskflow/internal/queue"
	"github.com/taskflow/taskflow/internal/schedule"
	"github.com/taskflow/taskflow/internal/statemachine"
	"github.com/taskflow/taskflow/internal/store"
)

// MaxPayloadBytes bounds a job's JSON payload.
const MaxPayloadBytes = 64 << 10

// Jobs implements job management.
type Jobs struct {
	pool *pgxpool.Pool
	eng  *engine.Engine
	reg  *handlers.Registry
	log  *slog.Logger
	now  func() time.Time
}

func NewJobs(pool *pgxpool.Pool, eng *engine.Engine, reg *handlers.Registry, log *slog.Logger) *Jobs {
	return &Jobs{pool: pool, eng: eng, reg: reg, log: log, now: func() time.Time { return time.Now().UTC() }}
}

// SetClock overrides the time source (tests).
func (s *Jobs) SetClock(now func() time.Time) { s.now = now }

// JobInput is the create/update request body. Optional numeric fields fall
// back to the workspace defaults.
type JobInput struct {
	Name               string          `json:"name"`
	Description        string          `json:"description"`
	Handler            string          `json:"handler"`
	Payload            json.RawMessage `json:"payload"`
	ScheduleType       string          `json:"schedule_type"`
	RunAt              *time.Time      `json:"run_at,omitempty"`
	DelaySeconds       *int            `json:"delay_seconds,omitempty"`
	CronExpr           *string         `json:"cron_expr,omitempty"`
	Timezone           string          `json:"timezone,omitempty"`
	MissedRunPolicy    string          `json:"missed_run_policy,omitempty"`
	OverlapPolicy      string          `json:"overlap_policy,omitempty"`
	MaxConcurrency     *int            `json:"max_concurrency,omitempty"`
	Priority           *int            `json:"priority,omitempty"`
	MaxAttempts        *int            `json:"max_attempts,omitempty"`
	BackoffBaseSeconds *int            `json:"backoff_base_seconds,omitempty"`
	BackoffMaxSeconds  *int            `json:"backoff_max_seconds,omitempty"`
	TimeoutSeconds     *int            `json:"timeout_seconds,omitempty"`
}

func intOr(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}

// build validates input and produces a job definition (without identity,
// status or timestamps). It is shared by create and update.
func (s *Jobs) build(in JobInput, settings *domain.Settings, now time.Time) (*domain.Job, error) {
	v := &domain.Validator{}
	name := strings.TrimSpace(in.Name)
	v.Check(name != "", "name", "Name is required.")
	v.Check(utf8.RuneCountInString(name) <= 200, "name", "Name must be at most 200 characters.")
	v.Check(utf8.RuneCountInString(in.Description) <= 2000, "description", "Description must be at most 2000 characters.")

	payload := in.Payload
	if len(bytes.TrimSpace(payload)) == 0 || string(bytes.TrimSpace(payload)) == "null" {
		payload = json.RawMessage(`{}`)
	}
	h, ok := s.reg.Get(in.Handler)
	switch {
	case in.Handler == "":
		v.Add("handler", "Choose a handler.")
	case !ok:
		v.Add("handler", fmt.Sprintf("Unknown handler %q.", in.Handler))
	}
	if len(payload) > MaxPayloadBytes {
		v.Add("payload", "Payload must be at most 64 KB.")
	} else if trimmed := bytes.TrimSpace(payload); len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		v.Add("payload", "Payload must be a JSON object.")
	} else if ok {
		if err := h.Validate(payload); err != nil {
			v.Add("payload", capitalize(err.Error())+".")
		}
		var compact bytes.Buffer
		if json.Compact(&compact, payload) == nil {
			payload = compact.Bytes()
		}
	}

	tz := in.Timezone
	if tz == "" {
		tz = settings.DefaultTimezone
	}
	if _, err := schedule.LoadTimezone(tz); err != nil {
		v.Add("timezone", "Unknown timezone.")
	}

	j := &domain.Job{
		Name: name, Description: in.Description, Handler: in.Handler, Payload: payload,
		ScheduleType: domain.ScheduleType(in.ScheduleType), Timezone: tz,
		MissedRunPolicy:    domain.MissedRunPolicy(in.MissedRunPolicy),
		OverlapPolicy:      domain.OverlapPolicy(in.OverlapPolicy),
		MaxConcurrency:     intOr(in.MaxConcurrency, 1),
		Priority:           intOr(in.Priority, settings.DefaultPriority),
		MaxAttempts:        intOr(in.MaxAttempts, settings.DefaultMaxAttempts),
		BackoffBaseSeconds: intOr(in.BackoffBaseSeconds, settings.DefaultBackoffBaseSeconds),
		BackoffMaxSeconds:  intOr(in.BackoffMaxSeconds, settings.DefaultBackoffMaxSeconds),
		TimeoutSeconds:     intOr(in.TimeoutSeconds, settings.DefaultTimeoutSeconds),
	}
	if j.MissedRunPolicy == "" {
		j.MissedRunPolicy = domain.MissedRunOnce
	}
	if j.OverlapPolicy == "" {
		j.OverlapPolicy = domain.OverlapSkip
	}

	switch j.ScheduleType {
	case domain.ScheduleOnce:
		if in.RunAt == nil {
			t := now
			j.RunAt = &t
		} else {
			t := in.RunAt.UTC()
			v.Check(!t.Before(now.Add(-time.Minute)), "run_at", "Run time must be in the future.")
			v.Check(t.Before(now.AddDate(5, 0, 0)), "run_at", "Run time must be within 5 years.")
			j.RunAt = &t
		}
	case domain.ScheduleDelayed:
		switch {
		case in.DelaySeconds == nil:
			v.Add("delay_seconds", "Delay is required.")
		case *in.DelaySeconds < 1 || *in.DelaySeconds > 365*24*3600:
			v.Add("delay_seconds", "Delay must be between 1 second and 365 days.")
		default:
			t := now.Add(time.Duration(*in.DelaySeconds) * time.Second)
			j.RunAt = &t
		}
	case domain.ScheduleCron:
		expr := ""
		if in.CronExpr != nil {
			expr = strings.TrimSpace(*in.CronExpr)
		}
		if _, err := schedule.ParseCron(expr, tz); err != nil {
			v.Add("cron_expr", capitalize(err.Error())+".")
		}
		j.CronExpr = &expr
	default:
		v.Add("schedule_type", "Schedule type must be once, delayed or cron.")
	}

	switch j.MissedRunPolicy {
	case domain.MissedSkip, domain.MissedRunOnce, domain.MissedRunAll:
	default:
		v.Add("missed_run_policy", "Missed-run policy must be skip, run_once or run_all.")
	}
	switch j.OverlapPolicy {
	case domain.OverlapSkip, domain.OverlapQueue, domain.OverlapAllow:
	default:
		v.Add("overlap_policy", "Overlap policy must be skip, queue or allow.")
	}
	v.Check(j.MaxConcurrency >= 1 && j.MaxConcurrency <= 100, "max_concurrency", "Concurrency must be between 1 and 100.")
	v.Check(j.Priority >= 1 && j.Priority <= 10, "priority", "Priority must be between 1 and 10.")
	v.Check(j.MaxAttempts >= 1 && j.MaxAttempts <= 25, "max_attempts", "Max attempts must be between 1 and 25.")
	v.Check(j.BackoffBaseSeconds >= 1 && j.BackoffBaseSeconds <= 3600, "backoff_base_seconds", "Base delay must be between 1 second and 1 hour.")
	v.Check(j.BackoffMaxSeconds >= 1 && j.BackoffMaxSeconds <= 86400, "backoff_max_seconds", "Max delay must be between 1 second and 24 hours.")
	v.Check(j.BackoffMaxSeconds >= j.BackoffBaseSeconds, "backoff_max_seconds", "Max delay must be at least the base delay.")
	v.Check(j.TimeoutSeconds >= 1 && j.TimeoutSeconds <= 86400, "timeout_seconds", "Timeout must be between 1 second and 24 hours.")
	if err := v.Err(); err != nil {
		return nil, err
	}
	return j, nil
}

// firstRun computes the initial next_run_at for a validated definition.
func firstRun(j *domain.Job, now time.Time) *time.Time {
	switch j.ScheduleType {
	case domain.ScheduleCron:
		spec, err := schedule.ParseCron(*j.CronExpr, j.Timezone)
		if err != nil {
			return nil
		}
		n := spec.Next(now)
		return &n
	default:
		t := *j.RunAt
		return &t
	}
}

func requestHash(in JobInput) string {
	b, _ := json.Marshal(in)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Create validates and stores a new job. If idemKey was already used by this
// workspace, the original job is returned with replayed=true (same request)
// or a conflict is reported (different request). The unique index on
// (tenant_id, idempotency_key) makes this safe under concurrent retries.
func (s *Jobs) Create(ctx context.Context, p *domain.Principal, in JobInput, idemKey string) (*domain.Job, bool, error) {
	if idemKey != "" && (len(idemKey) > 128 || strings.TrimSpace(idemKey) != idemKey) {
		return nil, false, &domain.ValidationError{Fields: map[string]string{"idempotency_key": "Idempotency-Key must be 1-128 characters without surrounding whitespace."}}
	}
	settings, err := store.GetSettings(ctx, s.pool, p.TenantID)
	if err != nil {
		return nil, false, err
	}
	now := s.now()
	j, err := s.build(in, settings, now)
	if err != nil {
		return nil, false, err
	}
	j.ID = uuid.New()
	j.TenantID = p.TenantID
	j.CreatedBy = &p.UserID
	j.Status = domain.JobCreated
	j.NextRunAt = firstRun(j, now)
	j.CreatedAt, j.UpdatedAt = now, now
	j.Version = 1
	hash := requestHash(in)
	j.RequestHash = &hash
	if idemKey != "" {
		j.IdempotencyKey = &idemKey
	}

	var replayed bool
	var events []queue.Event
	err = db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		inserted, err := store.InsertJob(ctx, tx, j)
		if err != nil {
			return err
		}
		if !inserted {
			existing, err := store.GetJobByIdempotencyKey(ctx, tx, p.TenantID, idemKey)
			if err != nil {
				return err
			}
			if existing.RequestHash == nil || *existing.RequestHash != hash {
				return domain.Conflict("idempotency_key_reused", "this Idempotency-Key was already used with a different request")
			}
			j, replayed = existing, true
			return nil
		}
		s.eng.RecordEvent(ctx, tx, domain.Event{TenantID: &j.TenantID, JobID: &j.ID, Type: "job.created",
			Severity: domain.SeverityInfo, Message: fmt.Sprintf("Job created by %s", p.Email)})
		status, evs, err := s.eng.SyncJobStatusTx(ctx, tx, j)
		if err != nil {
			return err
		}
		events = evs
		if status != domain.JobCreated {
			s.eng.RecordEvent(ctx, tx, domain.Event{TenantID: &j.TenantID, JobID: &j.ID, Type: "job.status",
				Severity: domain.SeverityInfo, Message: fmt.Sprintf("CREATED → %s", status)})
		}
		return s.eng.Audit(ctx, tx, p, "job.create", "job", j.ID.String(), map[string]any{"name": j.Name, "handler": j.Handler})
	})
	if err != nil {
		return nil, false, err
	}
	s.eng.PublishEvents(ctx, events)
	return j, replayed, nil
}

// Update replaces a job's definition. expectedVersion implements optimistic
// concurrency: concurrent edits fail with a conflict instead of silently
// overwriting each other. Edits only affect future runs; runs already queued
// keep the configuration snapshot taken when they were created.
func (s *Jobs) Update(ctx context.Context, p *domain.Principal, id uuid.UUID, in JobInput, expectedVersion int) (*domain.Job, error) {
	settings, err := store.GetSettings(ctx, s.pool, p.TenantID)
	if err != nil {
		return nil, err
	}
	var out *domain.Job
	var events []queue.Event
	err = db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		now := s.now()
		cur, err := store.LockTenantJob(ctx, tx, p.TenantID, id)
		if err != nil {
			return err
		}
		if cur.Version != expectedVersion {
			return domain.Conflict("version_conflict", "the job was modified by someone else; reload and try again")
		}
		summary, err := store.ActiveRunSummary(ctx, tx, id)
		if err != nil {
			return err
		}
		if st := statemachine.JobActions(cur.Status, cur.Recurring(), summary, false)[statemachine.ActionEdit]; !st.Allowed {
			return domain.Conflict("action_not_allowed", "%s", st.Reason)
		}
		// One-time jobs keep their run time unless it was changed explicitly.
		if in.ScheduleType == string(domain.ScheduleOnce) && in.RunAt == nil && cur.RunAt != nil {
			t := *cur.RunAt
			in.RunAt = &t
			if t.Before(now.Add(-time.Minute)) {
				in.RunAt = nil // stale time on an unscheduled job: run now
			}
		}
		next, err := s.build(in, settings, now)
		if err != nil {
			return err
		}
		if cur.Recurring() != (next.ScheduleType == domain.ScheduleCron) {
			return &domain.ValidationError{Fields: map[string]string{"schedule_type": "A job cannot be converted between one-time and recurring. Create a new job instead."}}
		}
		next.ID, next.TenantID, next.CreatedBy = cur.ID, cur.TenantID, cur.CreatedBy
		next.Status, next.PausedAt, next.CancelledAt = cur.Status, cur.PausedAt, cur.CancelledAt
		next.LastRunAt, next.LastRunStatus = cur.LastRunAt, cur.LastRunStatus
		next.IdempotencyKey, next.CreatedAt = cur.IdempotencyKey, cur.CreatedAt
		next.NextRunAt = cur.NextRunAt
		scheduleChanged := next.ScheduleType != cur.ScheduleType || derefStr(next.CronExpr) != derefStr(cur.CronExpr) ||
			next.Timezone != cur.Timezone || !timeEq(next.RunAt, cur.RunAt)
		if scheduleChanged && (cur.NextRunAt != nil || !cur.Recurring()) {
			next.NextRunAt = firstRun(next, now)
		}
		if err := store.UpdateJobDefinition(ctx, tx, next, expectedVersion, now); err != nil {
			return err
		}
		next.Version = expectedVersion + 1
		next.UpdatedAt = now
		changed := diffFields(cur, next)
		s.eng.RecordEvent(ctx, tx, domain.Event{TenantID: &next.TenantID, JobID: &next.ID, Type: "job.updated",
			Severity: domain.SeverityInfo, Message: fmt.Sprintf("Job updated by %s: %s", p.Email, strings.Join(changed, ", "))})
		if _, events, err = s.eng.SyncJobStatusTx(ctx, tx, next); err != nil {
			return err
		}
		if err := s.eng.Audit(ctx, tx, p, "job.update", "job", id.String(), map[string]any{"changed": changed}); err != nil {
			return err
		}
		out = next
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.eng.PublishEvents(ctx, events)
	return out, nil
}

func diffFields(a, b *domain.Job) []string {
	var out []string
	check := func(name string, changed bool) {
		if changed {
			out = append(out, name)
		}
	}
	check("name", a.Name != b.Name)
	check("description", a.Description != b.Description)
	check("handler", a.Handler != b.Handler)
	check("payload", !bytes.Equal(a.Payload, b.Payload))
	check("schedule", a.ScheduleType != b.ScheduleType || derefStr(a.CronExpr) != derefStr(b.CronExpr) || !timeEq(a.RunAt, b.RunAt))
	check("timezone", a.Timezone != b.Timezone)
	check("missed_run_policy", a.MissedRunPolicy != b.MissedRunPolicy)
	check("overlap_policy", a.OverlapPolicy != b.OverlapPolicy)
	check("max_concurrency", a.MaxConcurrency != b.MaxConcurrency)
	check("priority", a.Priority != b.Priority)
	check("retry_policy", a.MaxAttempts != b.MaxAttempts || a.BackoffBaseSeconds != b.BackoffBaseSeconds || a.BackoffMaxSeconds != b.BackoffMaxSeconds)
	check("timeout_seconds", a.TimeoutSeconds != b.TimeoutSeconds)
	if len(out) == 0 {
		out = []string{"no changes"}
	}
	return out
}

// JobDetail is the job details view model.
type JobDetail struct {
	Job          *domain.Job                         `json:"job"`
	Actions      map[string]statemachine.ActionState `json:"actions"`
	ActiveRuns   int                                 `json:"active_runs"`
	UpcomingRuns []time.Time                         `json:"upcoming_runs"`
	Handler      *handlers.Info                      `json:"handler"`
	Runs         []*domain.Run                       `json:"runs"`
	Attempts     []*domain.Attempt                   `json:"attempts"`
	Events       []*domain.Event                     `json:"events"`
}

// Get returns a job with its runs, attempts, timeline and permitted actions.
func (s *Jobs) Get(ctx context.Context, p *domain.Principal, id uuid.UUID) (*JobDetail, error) {
	j, err := store.GetJob(ctx, s.pool, p.TenantID, id)
	if err != nil {
		return nil, err
	}
	summary, err := store.ActiveRunSummary(ctx, s.pool, id)
	if err != nil {
		return nil, err
	}
	replayable := false
	if last, err := store.LastFailedRun(ctx, s.pool, id); err == nil {
		replayable = last.ReplayedAt == nil
	}
	runs, _, err := store.ListRuns(ctx, s.pool, store.RunFilter{TenantID: p.TenantID, JobID: &id, Page: store.Page{PageSize: 25}})
	if err != nil {
		return nil, err
	}
	attempts, err := store.AttemptsForJob(ctx, s.pool, p.TenantID, id, 100)
	if err != nil {
		return nil, err
	}
	events, err := store.ListEvents(ctx, s.pool, store.EventFilter{TenantID: p.TenantID, JobID: &id, Limit: 100})
	if err != nil {
		return nil, err
	}
	d := &JobDetail{
		Job: j, Actions: statemachine.JobActions(j.Status, j.Recurring(), summary, replayable),
		ActiveRuns: summary.Active(), Runs: nonNil(runs), Attempts: nonNil(attempts), Events: nonNil(events),
		UpcomingRuns: []time.Time{},
	}
	if h, ok := s.reg.Get(j.Handler); ok {
		info := h.Info()
		d.Handler = &info
	}
	if j.Recurring() && j.NextRunAt != nil && j.PausedAt == nil {
		if spec, err := schedule.ParseCron(derefStr(j.CronExpr), j.Timezone); err == nil {
			d.UpcomingRuns = append([]time.Time{*j.NextRunAt}, spec.NextN(*j.NextRunAt, 4)...)
		}
	} else if j.NextRunAt != nil {
		d.UpcomingRuns = []time.Time{*j.NextRunAt}
	}
	return d, nil
}

// List returns a filtered page of jobs.
func (s *Jobs) List(ctx context.Context, p *domain.Principal, f store.JobFilter) ([]store.JobListItem, int, error) {
	f.TenantID = p.TenantID
	items, total, err := store.ListJobs(ctx, s.pool, f)
	return nonNil(items), total, err
}

// ScheduleView is one row of the schedules page.
type ScheduleView struct {
	Job          *domain.Job   `json:"job"`
	UpcomingRuns []time.Time   `json:"upcoming_runs"`
	RecentRuns   []*domain.Run `json:"recent_runs"`
}

// Schedules lists recurring jobs with upcoming and recent runs.
func (s *Jobs) Schedules(ctx context.Context, p *domain.Principal) ([]ScheduleView, error) {
	jobs, err := store.ListSchedules(ctx, s.pool, p.TenantID)
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, len(jobs))
	for i, j := range jobs {
		ids[i] = j.ID
	}
	recent, err := store.RecentRunsForJobs(ctx, s.pool, ids, 8)
	if err != nil {
		return nil, err
	}
	out := make([]ScheduleView, 0, len(jobs))
	for _, j := range jobs {
		v := ScheduleView{Job: j, UpcomingRuns: []time.Time{}, RecentRuns: nonNil(recent[j.ID])}
		if j.NextRunAt != nil && j.PausedAt == nil && j.CancelledAt == nil {
			if spec, err := schedule.ParseCron(derefStr(j.CronExpr), j.Timezone); err == nil {
				v.UpcomingRuns = append([]time.Time{*j.NextRunAt}, spec.NextN(*j.NextRunAt, 4)...)
			}
		}
		out = append(out, v)
	}
	return out, nil
}

// PreviewSchedule validates a cron expression and returns upcoming times.
func (s *Jobs) PreviewSchedule(expr, tz string, count int) ([]time.Time, error) {
	if count < 1 || count > 20 {
		count = 5
	}
	spec, err := schedule.ParseCron(expr, tz)
	if err != nil {
		field := "cron_expr"
		if _, tzErr := schedule.LoadTimezone(tz); tzErr != nil {
			field = "timezone"
		}
		return nil, &domain.ValidationError{Fields: map[string]string{field: capitalize(err.Error()) + "."}}
	}
	return spec.NextN(s.now(), count), nil
}

// BulkResult reports the outcome of one item in a bulk action.
type BulkResult struct {
	ID      uuid.UUID `json:"id"`
	OK      bool      `json:"ok"`
	Code    string    `json:"code,omitempty"`
	Message string    `json:"message,omitempty"`
}

// Bulk applies an action to several jobs independently. Each job is its own
// transaction, so one failure does not roll back the others; per-item results
// tell the client exactly what happened.
func (s *Jobs) Bulk(ctx context.Context, p *domain.Principal, action string, ids []uuid.UUID) ([]BulkResult, error) {
	if len(ids) == 0 || len(ids) > 100 {
		return nil, &domain.ValidationError{Fields: map[string]string{"ids": "Select between 1 and 100 jobs."}}
	}
	var op func(uuid.UUID) error
	switch action {
	case "pause":
		op = func(id uuid.UUID) error { _, err := s.eng.PauseJob(ctx, p, id); return err }
	case "resume":
		op = func(id uuid.UUID) error { _, err := s.eng.ResumeJob(ctx, p, id); return err }
	case "cancel":
		op = func(id uuid.UUID) error { _, err := s.eng.CancelJob(ctx, p, id); return err }
	case "retry":
		op = func(id uuid.UUID) error { _, err := s.eng.RetryJob(ctx, p, id); return err }
	default:
		return nil, &domain.ValidationError{Fields: map[string]string{"action": "Action must be pause, resume, cancel or retry."}}
	}
	seen := map[uuid.UUID]bool{}
	out := make([]BulkResult, 0, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		r := BulkResult{ID: id, OK: true}
		if err := op(id); err != nil {
			r.OK = false
			var ce *domain.ConflictError
			switch {
			case errors.As(err, &ce):
				r.Code, r.Message = ce.Code, ce.Message
			case errors.Is(err, domain.ErrNotFound):
				r.Code, r.Message = "not_found", "Job not found."
			default:
				s.log.Error("bulk action failed", "action", action, "job_id", id, "err", err)
				r.Code, r.Message = "internal_error", "Unexpected error."
			}
		}
		out = append(out, r)
	}
	return out, nil
}

// HandlerSupported reports whether a handler is registered (used before replays).
func (s *Jobs) HandlerSupported(name string) bool {
	_, ok := s.reg.Get(name)
	return ok
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func timeEq(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
