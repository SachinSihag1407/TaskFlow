-- TaskFlow initial schema.
-- All timestamps are timestamptz and are written in UTC by the application.

-- Workspaces (tenants). Every user-created resource is scoped to one.
CREATE TABLE tenants (
    id          uuid PRIMARY KEY,
    name        text NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE users (
    id                     uuid PRIMARY KEY,
    tenant_id              uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    email                  text NOT NULL CHECK (length(email) BETWEEN 3 AND 254),
    name                   text NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
    password_hash          text NOT NULL,
    role                   text NOT NULL CHECK (role IN ('admin', 'operator', 'viewer')),
    notifications_seen_at  timestamptz,
    last_login_at          timestamptz,
    disabled_at            timestamptz,
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX users_email_key ON users (lower(email));
CREATE INDEX users_tenant_idx ON users (tenant_id);

-- Opaque session tokens. Only the SHA-256 of the token is stored.
CREATE TABLE sessions (
    id            uuid PRIMARY KEY,
    user_id       uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash    bytea NOT NULL UNIQUE,
    csrf_token    text NOT NULL,
    ip            text,
    user_agent    text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_seen_at  timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL
);
CREATE INDEX sessions_user_idx ON sessions (user_id);
CREATE INDEX sessions_expiry_idx ON sessions (expires_at);

-- Per-workspace defaults and limits, editable by admins.
CREATE TABLE tenant_settings (
    tenant_id                    uuid PRIMARY KEY REFERENCES tenants(id) ON DELETE CASCADE,
    default_max_attempts         int  NOT NULL DEFAULT 3   CHECK (default_max_attempts BETWEEN 1 AND 25),
    default_timeout_seconds      int  NOT NULL DEFAULT 300 CHECK (default_timeout_seconds BETWEEN 1 AND 86400),
    default_backoff_base_seconds int  NOT NULL DEFAULT 10  CHECK (default_backoff_base_seconds BETWEEN 1 AND 3600),
    default_backoff_max_seconds  int  NOT NULL DEFAULT 600 CHECK (default_backoff_max_seconds BETWEEN 1 AND 86400),
    default_priority             int  NOT NULL DEFAULT 5   CHECK (default_priority BETWEEN 1 AND 10),
    default_timezone             text NOT NULL DEFAULT 'UTC',
    max_concurrent_runs          int  NOT NULL DEFAULT 0   CHECK (max_concurrent_runs BETWEEN 0 AND 10000),
    event_retention_days         int  NOT NULL DEFAULT 30  CHECK (event_retention_days BETWEEN 1 AND 365),
    updated_at                   timestamptz NOT NULL DEFAULT now(),
    updated_by                   uuid REFERENCES users(id) ON DELETE SET NULL
);

-- Job definitions. status is an aggregate derived from the job's runs by the
-- engine (see internal/statemachine); it is never written ad hoc.
CREATE TABLE jobs (
    id                    uuid PRIMARY KEY,
    tenant_id             uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    created_by            uuid REFERENCES users(id) ON DELETE SET NULL,
    name                  text NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    description           text NOT NULL DEFAULT '' CHECK (length(description) <= 2000),
    handler               text NOT NULL,
    payload               jsonb NOT NULL DEFAULT '{}'::jsonb,
    schedule_type         text NOT NULL CHECK (schedule_type IN ('once', 'delayed', 'cron')),
    run_at                timestamptz,
    cron_expr             text,
    timezone              text NOT NULL DEFAULT 'UTC',
    missed_run_policy     text NOT NULL DEFAULT 'run_once' CHECK (missed_run_policy IN ('skip', 'run_once', 'run_all')),
    overlap_policy        text NOT NULL DEFAULT 'skip' CHECK (overlap_policy IN ('skip', 'queue', 'allow')),
    max_concurrency       int  NOT NULL DEFAULT 1 CHECK (max_concurrency BETWEEN 1 AND 100),
    priority              int  NOT NULL DEFAULT 5 CHECK (priority BETWEEN 1 AND 10),
    max_attempts          int  NOT NULL DEFAULT 3 CHECK (max_attempts BETWEEN 1 AND 25),
    backoff_base_seconds  int  NOT NULL DEFAULT 10 CHECK (backoff_base_seconds BETWEEN 1 AND 3600),
    backoff_max_seconds   int  NOT NULL DEFAULT 600 CHECK (backoff_max_seconds BETWEEN 1 AND 86400),
    timeout_seconds       int  NOT NULL DEFAULT 300 CHECK (timeout_seconds BETWEEN 1 AND 86400),
    status                text NOT NULL CHECK (status IN ('CREATED', 'SCHEDULED', 'QUEUED', 'RUNNING', 'RETRY_WAIT',
                                                          'PAUSED', 'SUCCEEDED', 'FAILED', 'CANCELLED')),
    next_run_at           timestamptz,
    last_run_at           timestamptz,
    last_run_status       text,
    paused_at             timestamptz,
    cancelled_at          timestamptz,
    idempotency_key       text CHECK (length(idempotency_key) BETWEEN 1 AND 128),
    request_hash          text,
    version               int  NOT NULL DEFAULT 1,
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),
    CHECK (schedule_type <> 'cron' OR cron_expr IS NOT NULL),
    CHECK (backoff_max_seconds >= backoff_base_seconds),
    UNIQUE (tenant_id, idempotency_key)
);
-- The scheduler's hot query: due, active jobs.
CREATE INDEX jobs_due_idx ON jobs (next_run_at)
    WHERE next_run_at IS NOT NULL AND paused_at IS NULL AND cancelled_at IS NULL;
CREATE INDEX jobs_tenant_status_idx ON jobs (tenant_id, status);
CREATE INDEX jobs_tenant_created_idx ON jobs (tenant_id, created_at DESC);
CREATE INDEX jobs_tenant_next_run_idx ON jobs (tenant_id, next_run_at);

-- One row per occurrence of a job. The run is the unit that moves through the
-- execution state machine. Configuration is snapshotted at creation so edits
-- to the job never change the semantics of an in-flight run.
CREATE TABLE job_runs (
    id                    uuid PRIMARY KEY,
    job_id                uuid NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    tenant_id             uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    scheduled_for         timestamptz NOT NULL,
    trigger               text NOT NULL CHECK (trigger IN ('schedule', 'manual', 'retry', 'replay')),
    status                text NOT NULL CHECK (status IN ('QUEUED', 'RUNNING', 'RETRY_WAIT', 'SUCCEEDED', 'FAILED', 'CANCELLED')),
    priority              int  NOT NULL,
    handler               text NOT NULL,
    payload               jsonb NOT NULL,
    timeout_seconds       int  NOT NULL,
    max_attempts          int  NOT NULL,
    backoff_base_seconds  int  NOT NULL,
    backoff_max_seconds   int  NOT NULL,
    attempt               int  NOT NULL DEFAULT 0,
    current_attempt_id    uuid,
    worker_id             uuid,
    lease_expires_at      timestamptz,
    queued_at             timestamptz,
    started_at            timestamptz,
    finished_at           timestamptz,
    next_attempt_at       timestamptz,
    published_at          timestamptz,
    cancel_requested_at   timestamptz,
    last_error            text,
    result                jsonb,
    checkpoint            jsonb,
    dead_lettered_at      timestamptz,
    replayed_at           timestamptz,
    replay_of             uuid REFERENCES job_runs(id) ON DELETE SET NULL,
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),
    CHECK (status <> 'RUNNING' OR (current_attempt_id IS NOT NULL AND lease_expires_at IS NOT NULL)),
    CHECK (status <> 'RETRY_WAIT' OR next_attempt_at IS NOT NULL)
);
-- Exactly one run per scheduled occurrence: the core duplicate-scheduling guard.
CREATE UNIQUE INDEX job_runs_occurrence_key ON job_runs (job_id, scheduled_for) WHERE trigger = 'schedule';
-- A failed run can be replayed at most once.
CREATE UNIQUE INDEX job_runs_replay_key ON job_runs (replay_of) WHERE replay_of IS NOT NULL;
CREATE INDEX job_runs_queued_idx ON job_runs (priority DESC, queued_at) WHERE status = 'QUEUED';
CREATE INDEX job_runs_lease_idx ON job_runs (lease_expires_at) WHERE status = 'RUNNING';
CREATE INDEX job_runs_retry_idx ON job_runs (next_attempt_at) WHERE status = 'RETRY_WAIT';
CREATE INDEX job_runs_job_idx ON job_runs (job_id, created_at DESC);
CREATE INDEX job_runs_job_active_idx ON job_runs (job_id) WHERE status IN ('QUEUED', 'RUNNING', 'RETRY_WAIT');
CREATE INDEX job_runs_tenant_running_idx ON job_runs (tenant_id) WHERE status = 'RUNNING';
CREATE INDEX job_runs_dead_letter_idx ON job_runs (tenant_id, dead_lettered_at DESC) WHERE dead_lettered_at IS NOT NULL;
CREATE INDEX job_runs_tenant_created_idx ON job_runs (tenant_id, created_at DESC);

CREATE TABLE workers (
    id                  uuid PRIMARY KEY,
    hostname            text NOT NULL,
    pid                 int  NOT NULL,
    version             text NOT NULL,
    concurrency         int  NOT NULL,
    handlers            text[] NOT NULL DEFAULT '{}',
    status              text NOT NULL CHECK (status IN ('ONLINE', 'DRAINING', 'OFFLINE')),
    active_runs         int  NOT NULL DEFAULT 0,
    completed_count     bigint NOT NULL DEFAULT 0,
    failed_count        bigint NOT NULL DEFAULT 0,
    heap_alloc_bytes    bigint,
    goroutines          int,
    drain_requested_at  timestamptz,
    started_at          timestamptz NOT NULL DEFAULT now(),
    last_heartbeat_at   timestamptz NOT NULL DEFAULT now(),
    stopped_at          timestamptz
);
CREATE INDEX workers_heartbeat_idx ON workers (last_heartbeat_at DESC);

-- One row per execution attempt of a run.
CREATE TABLE job_attempts (
    id              uuid PRIMARY KEY,
    run_id          uuid NOT NULL REFERENCES job_runs(id) ON DELETE CASCADE,
    job_id          uuid NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    tenant_id       uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    attempt_number  int  NOT NULL,
    worker_id       uuid REFERENCES workers(id) ON DELETE SET NULL,
    handler         text NOT NULL,
    status          text NOT NULL CHECK (status IN ('RUNNING', 'SUCCEEDED', 'FAILED', 'TIMED_OUT', 'CANCELLED', 'LOST')),
    queued_at       timestamptz,
    started_at      timestamptz NOT NULL,
    finished_at     timestamptz,
    duration_ms     bigint,
    error_kind      text,
    error_message   text,
    retryable       boolean,
    next_retry_at   timestamptz,
    UNIQUE (run_id, attempt_number)
);
CREATE INDEX job_attempts_tenant_started_idx ON job_attempts (tenant_id, started_at DESC);
CREATE INDEX job_attempts_job_idx ON job_attempts (job_id, started_at DESC);
CREATE INDEX job_attempts_worker_idx ON job_attempts (worker_id, started_at DESC);
CREATE INDEX job_attempts_finished_idx ON job_attempts (tenant_id, finished_at DESC) WHERE finished_at IS NOT NULL;

CREATE TABLE attempt_logs (
    id          bigserial PRIMARY KEY,
    attempt_id  uuid NOT NULL REFERENCES job_attempts(id) ON DELETE CASCADE,
    ts          timestamptz NOT NULL,
    level       text NOT NULL CHECK (level IN ('debug', 'info', 'warn', 'error')),
    message     text NOT NULL
);
CREATE INDEX attempt_logs_attempt_idx ON attempt_logs (attempt_id, id);

CREATE TABLE scheduler_instances (
    id                  uuid PRIMARY KEY,
    hostname            text NOT NULL,
    is_leader           boolean NOT NULL DEFAULT false,
    started_at          timestamptz NOT NULL DEFAULT now(),
    last_heartbeat_at   timestamptz NOT NULL DEFAULT now(),
    last_tick_at        timestamptz,
    last_tick_ms        int,
    last_tick_enqueued  int,
    stopped_at          timestamptz
);

-- Operational event stream: lifecycle transitions, recoveries, worker changes.
CREATE TABLE system_events (
    id          bigserial PRIMARY KEY,
    tenant_id   uuid REFERENCES tenants(id) ON DELETE CASCADE,
    job_id      uuid REFERENCES jobs(id) ON DELETE CASCADE,
    run_id      uuid REFERENCES job_runs(id) ON DELETE CASCADE,
    worker_id   uuid,
    type        text NOT NULL,
    severity    text NOT NULL CHECK (severity IN ('info', 'warning', 'error')),
    message     text NOT NULL,
    data        jsonb,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX system_events_tenant_idx ON system_events (tenant_id, created_at DESC);
CREATE INDEX system_events_job_idx ON system_events (job_id, created_at DESC);
CREATE INDEX system_events_created_idx ON system_events (created_at);

-- Security-relevant actions by humans. Never deleted by retention jobs.
CREATE TABLE audit_events (
    id           bigserial PRIMARY KEY,
    tenant_id    uuid REFERENCES tenants(id) ON DELETE CASCADE,
    actor_id     uuid REFERENCES users(id) ON DELETE SET NULL,
    actor_email  text,
    action       text NOT NULL,
    target_type  text,
    target_id    text,
    ip           text,
    user_agent   text,
    metadata     jsonb,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_events_tenant_idx ON audit_events (tenant_id, created_at DESC);
CREATE INDEX audit_events_target_idx ON audit_events (target_type, target_id);

-- Side-effect ledger for the simulated notification handler. The unique key
-- makes delivery idempotent across retried attempts of the same run.
CREATE TABLE notification_deliveries (
    id          uuid PRIMARY KEY,
    run_id      uuid NOT NULL REFERENCES job_runs(id) ON DELETE CASCADE,
    tenant_id   uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    channel     text NOT NULL,
    recipient   text NOT NULL,
    subject     text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (run_id, channel, recipient)
);
