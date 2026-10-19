// Package domain holds TaskFlow's core types. It has no dependencies on
// storage, transport or infrastructure packages.
package domain

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// JobStatus is the aggregate status of a job definition, derived from its
// runs by statemachine.DeriveJobStatus.
type JobStatus string

const (
	JobCreated   JobStatus = "CREATED"
	JobScheduled JobStatus = "SCHEDULED"
	JobQueued    JobStatus = "QUEUED"
	JobRunning   JobStatus = "RUNNING"
	JobRetryWait JobStatus = "RETRY_WAIT"
	JobPaused    JobStatus = "PAUSED"
	JobSucceeded JobStatus = "SUCCEEDED"
	JobFailed    JobStatus = "FAILED"
	JobCancelled JobStatus = "CANCELLED"
)

// RunStatus is the status of one occurrence of a job. Runs follow the strict
// transition table in package statemachine.
type RunStatus string

const (
	RunQueued    RunStatus = "QUEUED"
	RunRunning   RunStatus = "RUNNING"
	RunRetryWait RunStatus = "RETRY_WAIT"
	RunSucceeded RunStatus = "SUCCEEDED"
	RunFailed    RunStatus = "FAILED"
	RunCancelled RunStatus = "CANCELLED"
)

func (s RunStatus) Terminal() bool {
	return s == RunSucceeded || s == RunFailed || s == RunCancelled
}

func (s RunStatus) Active() bool { return !s.Terminal() }

type AttemptStatus string

const (
	AttemptRunning   AttemptStatus = "RUNNING"
	AttemptSucceeded AttemptStatus = "SUCCEEDED"
	AttemptFailed    AttemptStatus = "FAILED"
	AttemptTimedOut  AttemptStatus = "TIMED_OUT"
	AttemptCancelled AttemptStatus = "CANCELLED"
	// AttemptLost marks an attempt whose worker stopped renewing its lease
	// (crash, partition, OOM kill). The outcome of the work is unknown.
	AttemptLost AttemptStatus = "LOST"
)

type ScheduleType string

const (
	ScheduleOnce    ScheduleType = "once"    // run at a specific time (or immediately)
	ScheduleDelayed ScheduleType = "delayed" // run after a delay from creation
	ScheduleCron    ScheduleType = "cron"    // recurring
)

// MissedRunPolicy decides what happens to cron occurrences that were missed
// because the scheduler was down (or the job was paused).
type MissedRunPolicy string

const (
	MissedSkip    MissedRunPolicy = "skip"     // drop missed occurrences
	MissedRunOnce MissedRunPolicy = "run_once" // coalesce into one run
	MissedRunAll  MissedRunPolicy = "run_all"  // run every missed occurrence (capped)
)

// OverlapPolicy decides what happens when a recurring occurrence becomes due
// while a previous run of the same job is still active.
type OverlapPolicy string

const (
	OverlapSkip  OverlapPolicy = "skip"  // record the occurrence as skipped
	OverlapQueue OverlapPolicy = "queue" // enqueue; it waits for a concurrency slot
	OverlapAllow OverlapPolicy = "allow" // run concurrently up to max_concurrency
)

type Trigger string

const (
	TriggerSchedule Trigger = "schedule"
	TriggerManual   Trigger = "manual" // "run now"
	TriggerRetry    Trigger = "retry"  // manual retry of a failed job
	TriggerReplay   Trigger = "replay" // dead-letter replay
)

type Role string

const (
	RoleAdmin    Role = "admin"
	RoleOperator Role = "operator"
	RoleViewer   Role = "viewer"
)

func (r Role) Valid() bool { return r == RoleAdmin || r == RoleOperator || r == RoleViewer }

// CanWrite reports whether the role may mutate jobs.
func (r Role) CanWrite() bool { return r == RoleAdmin || r == RoleOperator }

type Tenant struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

type User struct {
	ID                  uuid.UUID  `json:"id"`
	TenantID            uuid.UUID  `json:"tenant_id"`
	Email               string     `json:"email"`
	Name                string     `json:"name"`
	PasswordHash        string     `json:"-"`
	Role                Role       `json:"role"`
	NotificationsSeenAt *time.Time `json:"notifications_seen_at"`
	LastLoginAt         *time.Time `json:"last_login_at"`
	DisabledAt          *time.Time `json:"disabled_at"`
	CreatedAt           time.Time  `json:"created_at"`
}

// Principal is the authenticated caller attached to a request context.
type Principal struct {
	UserID    uuid.UUID
	TenantID  uuid.UUID
	Email     string
	Role      Role
	SessionID uuid.UUID
	CSRFToken string
	IP        string
	UserAgent string
}

type Settings struct {
	TenantID                  uuid.UUID `json:"-"`
	DefaultMaxAttempts        int       `json:"default_max_attempts"`
	DefaultTimeoutSeconds     int       `json:"default_timeout_seconds"`
	DefaultBackoffBaseSeconds int       `json:"default_backoff_base_seconds"`
	DefaultBackoffMaxSeconds  int       `json:"default_backoff_max_seconds"`
	DefaultPriority           int       `json:"default_priority"`
	DefaultTimezone           string    `json:"default_timezone"`
	MaxConcurrentRuns         int       `json:"max_concurrent_runs"`
	EventRetentionDays        int       `json:"event_retention_days"`
	UpdatedAt                 time.Time `json:"updated_at"`
}

type Job struct {
	ID                 uuid.UUID       `json:"id"`
	TenantID           uuid.UUID       `json:"-"`
	CreatedBy          *uuid.UUID      `json:"created_by"`
	Name               string          `json:"name"`
	Description        string          `json:"description"`
	Handler            string          `json:"handler"`
	Payload            json.RawMessage `json:"payload"`
	ScheduleType       ScheduleType    `json:"schedule_type"`
	RunAt              *time.Time      `json:"run_at"`
	CronExpr           *string         `json:"cron_expr"`
	Timezone           string          `json:"timezone"`
	MissedRunPolicy    MissedRunPolicy `json:"missed_run_policy"`
	OverlapPolicy      OverlapPolicy   `json:"overlap_policy"`
	MaxConcurrency     int             `json:"max_concurrency"`
	Priority           int             `json:"priority"`
	MaxAttempts        int             `json:"max_attempts"`
	BackoffBaseSeconds int             `json:"backoff_base_seconds"`
	BackoffMaxSeconds  int             `json:"backoff_max_seconds"`
	TimeoutSeconds     int             `json:"timeout_seconds"`
	Status             JobStatus       `json:"status"`
	NextRunAt          *time.Time      `json:"next_run_at"`
	LastRunAt          *time.Time      `json:"last_run_at"`
	LastRunStatus      *RunStatus      `json:"last_run_status"`
	PausedAt           *time.Time      `json:"paused_at"`
	CancelledAt        *time.Time      `json:"cancelled_at"`
	IdempotencyKey     *string         `json:"idempotency_key"`
	RequestHash        *string         `json:"-"`
	Version            int             `json:"version"`
	CreatedAt          time.Time       `json:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at"`
}

// Recurring reports whether the job has a cron schedule.
func (j *Job) Recurring() bool { return j.ScheduleType == ScheduleCron }

type Run struct {
	ID                 uuid.UUID       `json:"id"`
	JobID              uuid.UUID       `json:"job_id"`
	TenantID           uuid.UUID       `json:"-"`
	ScheduledFor       time.Time       `json:"scheduled_for"`
	Trigger            Trigger         `json:"trigger"`
	Status             RunStatus       `json:"status"`
	Priority           int             `json:"priority"`
	Handler            string          `json:"handler"`
	Payload            json.RawMessage `json:"payload"`
	TimeoutSeconds     int             `json:"timeout_seconds"`
	MaxAttempts        int             `json:"max_attempts"`
	BackoffBaseSeconds int             `json:"backoff_base_seconds"`
	BackoffMaxSeconds  int             `json:"backoff_max_seconds"`
	Attempt            int             `json:"attempt"`
	CurrentAttemptID   *uuid.UUID      `json:"current_attempt_id"`
	WorkerID           *uuid.UUID      `json:"worker_id"`
	LeaseExpiresAt     *time.Time      `json:"lease_expires_at"`
	QueuedAt           *time.Time      `json:"queued_at"`
	StartedAt          *time.Time      `json:"started_at"`
	FinishedAt         *time.Time      `json:"finished_at"`
	NextAttemptAt      *time.Time      `json:"next_attempt_at"`
	CancelRequestedAt  *time.Time      `json:"cancel_requested_at"`
	LastError          *string         `json:"last_error"`
	Result             json.RawMessage `json:"result"`
	Checkpoint         json.RawMessage `json:"checkpoint"`
	DeadLetteredAt     *time.Time      `json:"dead_lettered_at"`
	ReplayedAt         *time.Time      `json:"replayed_at"`
	ReplayOf           *uuid.UUID      `json:"replay_of"`
	CreatedAt          time.Time       `json:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at"`

	// Populated by list queries that join the parent job.
	JobName string `json:"job_name,omitempty"`
}

type Attempt struct {
	ID            uuid.UUID     `json:"id"`
	RunID         uuid.UUID     `json:"run_id"`
	JobID         uuid.UUID     `json:"job_id"`
	TenantID      uuid.UUID     `json:"-"`
	AttemptNumber int           `json:"attempt_number"`
	WorkerID      *uuid.UUID    `json:"worker_id"`
	Handler       string        `json:"handler"`
	Status        AttemptStatus `json:"status"`
	QueuedAt      *time.Time    `json:"queued_at"`
	StartedAt     time.Time     `json:"started_at"`
	FinishedAt    *time.Time    `json:"finished_at"`
	DurationMS    *int64        `json:"duration_ms"`
	ErrorKind     *string       `json:"error_kind"`
	ErrorMessage  *string       `json:"error_message"`
	Retryable     *bool         `json:"retryable"`
	NextRetryAt   *time.Time    `json:"next_retry_at"`

	// Joined fields for list views.
	JobName        string `json:"job_name,omitempty"`
	WorkerHostname string `json:"worker_hostname,omitempty"`
	MaxAttempts    int    `json:"max_attempts,omitempty"`
}

type LogLine struct {
	ID      int64     `json:"id"`
	TS      time.Time `json:"ts"`
	Level   string    `json:"level"`
	Message string    `json:"message"`
}

type WorkerState string

const (
	WorkerOnline   WorkerState = "ONLINE"
	WorkerDraining WorkerState = "DRAINING"
	WorkerOffline  WorkerState = "OFFLINE"
	// WorkerStale is never stored; it is computed at read time for workers
	// whose heartbeat is overdue but which have not yet been declared offline.
	WorkerStale WorkerState = "STALE"
)

type Worker struct {
	ID               uuid.UUID   `json:"id"`
	Hostname         string      `json:"hostname"`
	PID              int         `json:"pid"`
	Version          string      `json:"version"`
	Concurrency      int         `json:"concurrency"`
	Handlers         []string    `json:"handlers"`
	Status           WorkerState `json:"status"`
	ActiveRuns       int         `json:"active_runs"`
	CompletedCount   int64       `json:"completed_count"`
	FailedCount      int64       `json:"failed_count"`
	HeapAllocBytes   *int64      `json:"heap_alloc_bytes"`
	Goroutines       *int        `json:"goroutines"`
	DrainRequestedAt *time.Time  `json:"drain_requested_at"`
	StartedAt        time.Time   `json:"started_at"`
	LastHeartbeatAt  time.Time   `json:"last_heartbeat_at"`
	StoppedAt        *time.Time  `json:"stopped_at"`
}

type SchedulerInstance struct {
	ID               uuid.UUID  `json:"id"`
	Hostname         string     `json:"hostname"`
	IsLeader         bool       `json:"is_leader"`
	StartedAt        time.Time  `json:"started_at"`
	LastHeartbeatAt  time.Time  `json:"last_heartbeat_at"`
	LastTickAt       *time.Time `json:"last_tick_at"`
	LastTickMS       *int       `json:"last_tick_ms"`
	LastTickEnqueued *int       `json:"last_tick_enqueued"`
	StoppedAt        *time.Time `json:"stopped_at"`
}

type Severity string

const (
	SeverityInfo    Severity = "info"
	SeverityWarning Severity = "warning"
	SeverityError   Severity = "error"
)

type Event struct {
	ID        int64           `json:"id"`
	TenantID  *uuid.UUID      `json:"-"`
	JobID     *uuid.UUID      `json:"job_id"`
	RunID     *uuid.UUID      `json:"run_id"`
	WorkerID  *uuid.UUID      `json:"worker_id"`
	Type      string          `json:"type"`
	Severity  Severity        `json:"severity"`
	Message   string          `json:"message"`
	Data      json.RawMessage `json:"data,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

type AuditEvent struct {
	ID         int64           `json:"id"`
	TenantID   *uuid.UUID      `json:"-"`
	ActorID    *uuid.UUID      `json:"actor_id"`
	ActorEmail *string         `json:"actor_email"`
	Action     string          `json:"action"`
	TargetType *string         `json:"target_type"`
	TargetID   *string         `json:"target_id"`
	IP         *string         `json:"ip"`
	UserAgent  *string         `json:"user_agent"`
	Metadata   json.RawMessage `json:"metadata,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
}
