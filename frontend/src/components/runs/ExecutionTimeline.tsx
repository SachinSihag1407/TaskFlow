import {
  AlertTriangle,
  CheckCircle2,
  CircleDashed,
  Clock,
  Loader2,
  Server,
  Slash,
  XCircle,
} from "lucide-react";
import type { Attempt } from "@/lib/types";
import { formatDuration, shortId } from "@/lib/format";
import { cn } from "@/lib/utils";
import { StatusBadge } from "@/components/ui/badge";
import { RelTime } from "@/components/common/Time";

const icons: Record<string, { icon: typeof CheckCircle2; cls: string }> = {
  SUCCEEDED: { icon: CheckCircle2, cls: "text-success" },
  FAILED: { icon: XCircle, cls: "text-danger" },
  TIMED_OUT: { icon: Clock, cls: "text-danger" },
  CANCELLED: { icon: Slash, cls: "text-fg-subtle" },
  LOST: { icon: AlertTriangle, cls: "text-warning" },
  RUNNING: { icon: Loader2, cls: "text-info animate-spin" },
};

const kindCopy: Record<string, string> = {
  handler_error: "Handler error",
  permanent_error: "Permanent error (not retried)",
  timeout: "Timed out",
  lease_expired: "Worker lost",
  interrupted: "Interrupted by worker shutdown",
  panic: "Handler panic",
  cancelled: "Cancelled",
};

/**
 * Vertical timeline of execution attempts, oldest first. Each node shows the
 * outcome, worker, timing and — for failures — the error and what happened
 * next (retry time or dead-letter).
 */
export function ExecutionTimeline({
  attempts,
  onSelect,
  selectedId,
}: {
  attempts: Attempt[];
  onSelect?: (a: Attempt) => void;
  selectedId?: string;
}) {
  if (attempts.length === 0) {
    return (
      <div className="flex items-center gap-2 px-1 py-4 text-sm text-fg-subtle">
        <CircleDashed className="h-4 w-4" /> No attempts yet. The first attempt starts when a worker claims
        the run.
      </div>
    );
  }
  const sorted = [...attempts].sort((a, b) => a.started_at.localeCompare(b.started_at));
  return (
    <ol className="relative">
      {sorted.map((a, i) => {
        const meta = icons[a.status] ?? icons.RUNNING;
        const Icon = meta.icon;
        const last = i === sorted.length - 1;
        return (
          <li key={a.id} className="relative flex gap-3 pb-4 last:pb-0">
            {!last && (
              <span className="absolute left-[9px] top-6 h-[calc(100%-16px)] w-px bg-line" aria-hidden />
            )}
            <Icon
              className={cn("relative mt-0.5 h-[19px] w-[19px] shrink-0 bg-surface", meta.cls)}
              aria-hidden
            />
            <button
              type="button"
              disabled={!onSelect}
              onClick={() => onSelect?.(a)}
              className={cn(
                "min-w-0 flex-1 rounded-md border px-3 py-2 text-left transition-colors",
                onSelect && "hover:bg-surface-2/60",
                selectedId === a.id && "border-accent/50 bg-accent-soft/30",
              )}
            >
              <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
                <span className="text-sm font-medium">
                  Attempt {a.attempt_number}
                  {a.max_attempts ? (
                    <span className="font-normal text-fg-subtle"> of {a.max_attempts}</span>
                  ) : null}
                </span>
                <StatusBadge status={a.status} />
                <span className="tabular ml-auto text-xs text-fg-subtle">
                  {formatDuration(a.duration_ms)}
                </span>
              </div>
              <div className="mt-1 flex flex-wrap items-center gap-x-3 gap-y-0.5 text-xs text-fg-subtle">
                <RelTime iso={a.started_at} />
                <span className="inline-flex items-center gap-1">
                  <Server className="h-3 w-3" />
                  {a.worker_hostname || shortId(a.worker_id)}
                </span>
                {a.queued_at && (
                  <span className="tabular">
                    waited{" "}
                    {formatDuration(new Date(a.started_at).getTime() - new Date(a.queued_at).getTime())}
                  </span>
                )}
              </div>
              {a.error_message && (
                <div className="mt-2 rounded bg-danger-soft/60 px-2 py-1.5 text-xs">
                  {a.error_kind && (
                    <div className="font-medium text-danger">{kindCopy[a.error_kind] ?? a.error_kind}</div>
                  )}
                  <div className="line-clamp-3 break-words font-mono text-fg">{a.error_message}</div>
                </div>
              )}
              {a.status !== "SUCCEEDED" && a.status !== "RUNNING" && a.status !== "CANCELLED" && (
                <div className="mt-1.5 text-xs text-fg-subtle">
                  {a.next_retry_at ? (
                    new Date(a.next_retry_at).getTime() > Date.now() ? (
                      <>
                        Retry scheduled <RelTime iso={a.next_retry_at} />
                      </>
                    ) : last ? (
                      "Retry pending — waiting for a worker"
                    ) : (
                      "Retried after backoff"
                    )
                  ) : a.retryable === false ? (
                    "No further retries — moved to the dead-letter queue"
                  ) : null}
                </div>
              )}
            </button>
          </li>
        );
      })}
    </ol>
  );
}
