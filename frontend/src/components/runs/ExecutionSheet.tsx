import { Link } from "react-router-dom";
import { ArrowUpRight } from "lucide-react";
import { useExecution } from "@/api/hooks";
import { formatDateTime, formatDuration, shortId } from "@/lib/format";
import { Sheet } from "@/components/ui/dialog";
import { StatusBadge } from "@/components/ui/badge";
import { DescriptionList } from "@/components/ui/layout";
import { ErrorState, Skeleton } from "@/components/ui/feedback";
import { JsonBlock, Mono } from "@/components/ui/code";
import { LogViewer } from "./LogViewer";

/** Slide-over inspecting one execution attempt: metadata, error, result, logs. */
export function ExecutionSheet({ attemptId, onClose }: { attemptId: string | null; onClose: () => void }) {
  const q = useExecution(attemptId ?? undefined);
  const a = q.data?.attempt;
  const run = q.data?.run;
  return (
    <Sheet
      open={!!attemptId}
      onOpenChange={(o) => !o && onClose()}
      title={a ? `${a.job_name} · attempt ${a.attempt_number}` : "Execution"}
    >
      {q.isLoading ? (
        <div className="space-y-3 p-5">
          <Skeleton className="h-5 w-1/2" />
          <Skeleton className="h-24 w-full" />
          <Skeleton className="h-64 w-full" />
        </div>
      ) : q.error ? (
        <ErrorState error={q.error} onRetry={() => q.refetch()} />
      ) : a && run ? (
        <div className="space-y-6 p-5">
          <div className="flex flex-wrap items-center gap-2">
            <StatusBadge status={a.status} />
            <Link
              to={`/jobs/${a.job_id}`}
              onClick={onClose}
              className="inline-flex items-center gap-1 text-sm text-accent hover:underline"
            >
              Open job <ArrowUpRight className="h-3.5 w-3.5" />
            </Link>
          </div>
          <DescriptionList
            items={[
              { label: "Attempt", value: `${a.attempt_number} of ${run.max_attempts}` },
              { label: "Duration", value: <span className="tabular">{formatDuration(a.duration_ms)}</span> },
              { label: "Started", value: formatDateTime(a.started_at) },
              { label: "Finished", value: formatDateTime(a.finished_at) },
              {
                label: "Worker",
                value: a.worker_hostname
                  ? `${a.worker_hostname} (${shortId(a.worker_id)})`
                  : shortId(a.worker_id),
              },
              { label: "Handler", value: <code className="text-xs">{a.handler}</code> },
              { label: "Run", value: <Mono copy={run.id}>{shortId(run.id)}</Mono> },
              { label: "Attempt ID", value: <Mono copy={a.id}>{shortId(a.id)}</Mono> },
              {
                label: "Queue wait",
                value: a.queued_at ? (
                  <span className="tabular">
                    {formatDuration(new Date(a.started_at).getTime() - new Date(a.queued_at).getTime())}
                  </span>
                ) : (
                  "—"
                ),
              },
              { label: "Run status", value: <StatusBadge status={run.status} /> },
            ]}
          />
          {a.error_message && (
            <section>
              <h3 className="mb-2 text-sm font-medium">
                Error
                {a.error_kind ? (
                  <span className="ml-2 font-normal text-fg-subtle">{a.error_kind}</span>
                ) : null}
              </h3>
              <pre className="scrollbar-thin overflow-auto whitespace-pre-wrap break-words rounded-md border border-danger/20 bg-danger-soft/50 p-3 font-mono text-xs text-fg">
                {a.error_message}
              </pre>
            </section>
          )}
          {a.status === "SUCCEEDED" && run.result != null && (
            <section>
              <h3 className="mb-2 text-sm font-medium">Result</h3>
              <JsonBlock value={run.result} maxHeight={220} />
            </section>
          )}
          <section>
            <h3 className="mb-2 text-sm font-medium">Logs</h3>
            <LogViewer lines={q.data!.logs} running={a.status === "RUNNING"} />
          </section>
        </div>
      ) : null}
    </Sheet>
  );
}
