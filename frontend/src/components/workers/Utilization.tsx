import type { Worker } from "@/lib/types";
import { cn } from "@/lib/utils";

/** Busy slots / capacity bar. Non-live workers show no usage. */
export function Utilization({ w }: { w: Worker }) {
  const live = w.status === "ONLINE" || w.status === "DRAINING";
  const pct = w.concurrency ? (w.active_runs / w.concurrency) * 100 : 0;
  return (
    <div className="flex items-center gap-2">
      <div className="h-1.5 w-16 overflow-hidden rounded-full bg-surface-3">
        <div
          className={cn("h-full rounded-full", live ? "bg-accent" : "bg-fg-faint")}
          style={{ width: `${live ? pct : 0}%` }}
        />
      </div>
      <span className="tabular text-xs text-fg-muted">
        {live ? w.active_runs : "—"} / {w.concurrency}
      </span>
    </div>
  );
}
