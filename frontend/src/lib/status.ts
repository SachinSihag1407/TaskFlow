import type { AnyStatus } from "./types";

export type Tone = "neutral" | "accent" | "success" | "danger" | "warning" | "info" | "muted";

interface StatusMeta {
  label: string;
  tone: Tone;
  /** Animated dot for in-progress states. */
  live?: boolean;
  description: string;
}

// One mapping for every status in the system so colour always means the same
// thing: green = done, red = failed, amber = waiting to retry / uncertain,
// blue = executing, grey = waiting or inactive.
const META: Record<string, StatusMeta> = {
  CREATED: { label: "Created", tone: "neutral", description: "Created, not yet scheduled" },
  SCHEDULED: { label: "Scheduled", tone: "neutral", description: "Waiting for its next run time" },
  QUEUED: { label: "Queued", tone: "accent", description: "Ready and waiting for a worker" },
  RUNNING: { label: "Running", tone: "info", live: true, description: "Executing on a worker" },
  RETRY_WAIT: {
    label: "Retry wait",
    tone: "warning",
    description: "Failed; waiting for backoff before retrying",
  },
  PAUSED: { label: "Paused", tone: "muted", description: "Paused by an operator" },
  SUCCEEDED: { label: "Succeeded", tone: "success", description: "Finished successfully" },
  FAILED: { label: "Failed", tone: "danger", description: "Failed after exhausting its retry policy" },
  CANCELLED: { label: "Cancelled", tone: "muted", description: "Cancelled by an operator" },
  TIMED_OUT: { label: "Timed out", tone: "danger", description: "Exceeded its timeout" },
  LOST: { label: "Lost", tone: "warning", description: "Worker stopped responding; outcome unknown" },
  ONLINE: { label: "Online", tone: "success", description: "Heartbeating normally" },
  DRAINING: { label: "Draining", tone: "warning", description: "Finishing in-flight work before shutdown" },
  STALE: { label: "Stale", tone: "warning", description: "Heartbeats overdue" },
  OFFLINE: { label: "Offline", tone: "muted", description: "Stopped or unreachable" },
};

export function statusMeta(status: AnyStatus | string): StatusMeta {
  return META[status] ?? { label: status, tone: "neutral", description: status };
}

export const toneClasses: Record<Tone, { badge: string; dot: string; text: string }> = {
  neutral: { badge: "bg-surface-2 text-fg-muted ring-line", dot: "bg-fg-subtle", text: "text-fg-muted" },
  muted: { badge: "bg-surface-2 text-fg-subtle ring-line", dot: "bg-fg-faint", text: "text-fg-subtle" },
  accent: {
    badge: "bg-accent-soft text-accent-soft-fg ring-accent/20",
    dot: "bg-accent",
    text: "text-accent-soft-fg",
  },
  success: {
    badge: "bg-success-soft text-success ring-success/20",
    dot: "bg-success-solid",
    text: "text-success",
  },
  danger: { badge: "bg-danger-soft text-danger ring-danger/20", dot: "bg-danger-solid", text: "text-danger" },
  warning: {
    badge: "bg-warning-soft text-warning ring-warning/25",
    dot: "bg-warning-solid",
    text: "text-warning",
  },
  info: { badge: "bg-info-soft text-info ring-info/20", dot: "bg-info-solid", text: "text-info" },
};

export const JOB_STATUSES = [
  "SCHEDULED",
  "QUEUED",
  "RUNNING",
  "RETRY_WAIT",
  "PAUSED",
  "SUCCEEDED",
  "FAILED",
  "CANCELLED",
] as const;

export const ATTEMPT_STATUSES = ["RUNNING", "SUCCEEDED", "FAILED", "TIMED_OUT", "CANCELLED", "LOST"] as const;

export const priorityLabel = (p: number) =>
  p >= 9 ? "Critical" : p >= 7 ? "High" : p >= 4 ? "Normal" : "Low";
