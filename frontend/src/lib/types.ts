// Types mirror the Go API's JSON (see backend/internal/api/openapi.yaml).

export type JobStatus =
  | "CREATED"
  | "SCHEDULED"
  | "QUEUED"
  | "RUNNING"
  | "RETRY_WAIT"
  | "PAUSED"
  | "SUCCEEDED"
  | "FAILED"
  | "CANCELLED";

export type RunStatus = "QUEUED" | "RUNNING" | "RETRY_WAIT" | "SUCCEEDED" | "FAILED" | "CANCELLED";
export type AttemptStatus = "RUNNING" | "SUCCEEDED" | "FAILED" | "TIMED_OUT" | "CANCELLED" | "LOST";
export type WorkerStatus = "ONLINE" | "DRAINING" | "STALE" | "OFFLINE";
export type AnyStatus = JobStatus | RunStatus | AttemptStatus | WorkerStatus;

export type ScheduleType = "once" | "delayed" | "cron";
export type MissedRunPolicy = "skip" | "run_once" | "run_all";
export type OverlapPolicy = "skip" | "queue" | "allow";
export type Role = "admin" | "operator" | "viewer";
export type Severity = "info" | "warning" | "error";

export interface PageMeta {
  page: number;
  page_size: number;
  total: number;
  total_pages: number;
}

export interface User {
  id: string;
  tenant_id: string;
  email: string;
  name: string;
  role: Role;
  notifications_seen_at: string | null;
  last_login_at: string | null;
  disabled_at: string | null;
  created_at: string;
}

export interface Workspace {
  id: string;
  name: string;
  created_at: string;
}

export interface Me {
  user: User;
  workspace: Workspace;
  csrf_token: string;
  /** May operate shared infrastructure, e.g. drain workers. */
  platform_admin: boolean;
}

export interface Job {
  id: string;
  created_by: string | null;
  name: string;
  description: string;
  handler: string;
  payload: Record<string, unknown>;
  schedule_type: ScheduleType;
  run_at: string | null;
  cron_expr: string | null;
  timezone: string;
  missed_run_policy: MissedRunPolicy;
  overlap_policy: OverlapPolicy;
  max_concurrency: number;
  priority: number;
  max_attempts: number;
  backoff_base_seconds: number;
  backoff_max_seconds: number;
  timeout_seconds: number;
  status: JobStatus;
  next_run_at: string | null;
  last_run_at: string | null;
  last_run_status: RunStatus | null;
  paused_at: string | null;
  cancelled_at: string | null;
  idempotency_key: string | null;
  version: number;
  created_at: string;
  updated_at: string;
}

export interface JobListItem extends Job {
  succeeded_runs: number;
  failed_runs: number;
  active_runs: number;
}

export interface ActionState {
  allowed: boolean;
  reason?: string;
}

export type JobAction = "edit" | "pause" | "resume" | "cancel" | "retry" | "run_now";

export interface Run {
  id: string;
  job_id: string;
  job_name?: string;
  scheduled_for: string;
  trigger: "schedule" | "manual" | "retry" | "replay";
  status: RunStatus;
  priority: number;
  handler: string;
  payload: Record<string, unknown>;
  timeout_seconds: number;
  max_attempts: number;
  backoff_base_seconds: number;
  backoff_max_seconds: number;
  attempt: number;
  current_attempt_id: string | null;
  worker_id: string | null;
  lease_expires_at: string | null;
  queued_at: string | null;
  started_at: string | null;
  finished_at: string | null;
  next_attempt_at: string | null;
  cancel_requested_at: string | null;
  last_error: string | null;
  result: unknown;
  checkpoint: unknown;
  dead_lettered_at: string | null;
  replayed_at: string | null;
  replay_of: string | null;
  created_at: string;
  updated_at: string;
}

export interface Attempt {
  id: string;
  run_id: string;
  job_id: string;
  attempt_number: number;
  worker_id: string | null;
  handler: string;
  status: AttemptStatus;
  queued_at: string | null;
  started_at: string;
  finished_at: string | null;
  duration_ms: number | null;
  error_kind: string | null;
  error_message: string | null;
  retryable: boolean | null;
  next_retry_at: string | null;
  job_name?: string;
  worker_hostname?: string;
  max_attempts?: number;
}

export interface LogLine {
  id: number;
  ts: string;
  level: "debug" | "info" | "warn" | "error";
  message: string;
}

export interface SystemEvent {
  id: number;
  job_id: string | null;
  run_id: string | null;
  worker_id: string | null;
  type: string;
  severity: Severity;
  message: string;
  data?: Record<string, unknown>;
  created_at: string;
}

export interface AuditEvent {
  id: number;
  actor_id: string | null;
  actor_email: string | null;
  action: string;
  target_type: string | null;
  target_id: string | null;
  ip: string | null;
  user_agent: string | null;
  metadata?: Record<string, unknown>;
  created_at: string;
}

export interface HandlerField {
  name: string;
  type: string;
  required: boolean;
  description: string;
}

export interface HandlerInfo {
  name: string;
  title: string;
  description: string;
  idempotency: string;
  example_payload: Record<string, unknown>;
  fields: HandlerField[];
}

export interface JobDetail {
  job: Job;
  actions: Record<JobAction, ActionState>;
  active_runs: number;
  upcoming_runs: string[];
  handler: HandlerInfo | null;
  runs: Run[];
  attempts: Attempt[];
  events: SystemEvent[];
}

export interface RunDetail {
  run: Run;
  attempts: Attempt[];
  events: SystemEvent[];
  audit: AuditEvent[];
}

export interface AttemptDetail {
  attempt: Attempt;
  run: Run;
  logs: LogLine[];
}

export interface ScheduleView {
  job: Job;
  upcoming_runs: string[];
  recent_runs: Run[];
}

export interface Worker {
  id: string;
  hostname: string;
  pid: number;
  version: string;
  concurrency: number;
  handlers: string[];
  status: WorkerStatus;
  active_runs: number;
  completed_count: number;
  failed_count: number;
  heap_alloc_bytes: number | null;
  goroutines: number | null;
  drain_requested_at: string | null;
  started_at: string;
  last_heartbeat_at: string;
  stopped_at: string | null;
  seconds_since_heartbeat: number;
}

export interface WorkerDetail {
  worker: Worker;
  running_runs: Run[];
  recent_attempts: Attempt[];
  events: SystemEvent[];
}

export interface ThroughputPoint {
  bucket: string;
  succeeded: number;
  failed: number;
}

export interface Outcomes {
  runs_succeeded: number;
  runs_failed: number;
  runs_cancelled: number;
  attempts_total: number;
  attempts_failed: number;
  retries: number;
  latency_samples: number;
  latency_avg_ms: number | null;
  latency_p50_ms: number | null;
  latency_p95_ms: number | null;
  latency_p99_ms: number | null;
  queue_wait_avg_ms: number | null;
  queue_wait_p95_ms: number | null;
}

export interface WorkerSummary {
  online: number;
  draining: number;
  stale: number;
  offline: number;
  capacity: number;
  busy: number;
}

export interface Overview {
  range: string;
  generated_at: string;
  job_counts: Partial<Record<JobStatus, number>>;
  total_jobs: number;
  queued: number;
  running: number;
  retry_wait: number;
  dead_letter: number;
  outcomes: Outcomes;
  success_rate: number | null;
  throughput: ThroughputPoint[];
  by_handler: { handler: string; succeeded: number; failed: number }[];
  workers: WorkerSummary;
  queue: { redis_available: boolean; redis_ready: number | null; redis_delayed: number | null };
  recent_attempts: Attempt[];
  events: SystemEvent[];
}

export interface ComponentHealth {
  name: string;
  status: "healthy" | "degraded" | "down" | "disabled";
  latency_ms: number;
  detail: string;
}

export interface SchedulerInstance {
  id: string;
  hostname: string;
  is_leader: boolean;
  started_at: string;
  last_heartbeat_at: string;
  last_tick_at: string | null;
  last_tick_ms: number | null;
  last_tick_enqueued: number | null;
  stopped_at: string | null;
}

export interface Health {
  status: "healthy" | "degraded" | "down";
  checked_at: string;
  components: ComponentHealth[];
  schedulers: SchedulerInstance[];
  workers: WorkerSummary;
  queue: {
    postgres_queued: number;
    postgres_running: number;
    postgres_retry_wait: number;
    redis_ready: number | null;
    redis_delayed: number | null;
  };
  rates: {
    window_minutes: number;
    attempts: number;
    failed: number;
    retries: number;
    error_rate: number | null;
    retry_rate: number | null;
  };
  live_updates: boolean;
  sse_clients: number;
}

export interface Settings {
  default_max_attempts: number;
  default_timeout_seconds: number;
  default_backoff_base_seconds: number;
  default_backoff_max_seconds: number;
  default_priority: number;
  default_timezone: string;
  max_concurrent_runs: number;
  event_retention_days: number;
  updated_at?: string;
}

export interface BulkResult {
  id: string;
  ok: boolean;
  code?: string;
  message?: string;
}

export interface Notifications {
  unread: number;
  items: SystemEvent[];
}

export interface JobInput {
  name: string;
  description: string;
  handler: string;
  payload: Record<string, unknown>;
  schedule_type: ScheduleType;
  run_at?: string;
  delay_seconds?: number;
  cron_expr?: string;
  timezone: string;
  missed_run_policy: MissedRunPolicy;
  overlap_policy: OverlapPolicy;
  max_concurrency: number;
  priority: number;
  max_attempts: number;
  backoff_base_seconds: number;
  backoff_max_seconds: number;
  timeout_seconds: number;
}
