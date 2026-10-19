import { useState } from "react";
import { Link, useParams } from "react-router-dom";
import { Power, SearchX } from "lucide-react";
import { usePageMeta } from "@/app/page-meta";
import { useSession } from "@/app/session";
import { useDrainWorker, useWorker } from "@/api/hooks";
import { ApiError } from "@/lib/api";
import { formatBytes, formatDateTime, formatDuration, formatNumber, shortId } from "@/lib/format";
import { cn } from "@/lib/utils";
import { Button } from "@/components/ui/button";
import { StatusBadge } from "@/components/ui/badge";
import { DescriptionList, PageHeader, Panel } from "@/components/ui/layout";
import { EmptyState, ErrorState, Skeleton } from "@/components/ui/feedback";
import { Table, TD, TH, THead, TR } from "@/components/ui/table";
import { ConfirmDialog } from "@/components/ui/dialog";
import { Mono } from "@/components/ui/code";
import { RelTime } from "@/components/common/Time";
import { ExecutionSheet } from "@/components/runs/ExecutionSheet";
import { Utilization } from "@/components/workers/Utilization";

export default function WorkerDetailPage() {
  const { id } = useParams();
  const q = useWorker(id);
  const { me } = useSession();
  const drain = useDrainWorker();
  const [confirm, setConfirm] = useState(false);
  const [attempt, setAttempt] = useState<string | null>(null);
  const d = q.data;
  usePageMeta([{ label: "Workers", to: "/workers" }, { label: d?.worker.hostname ?? "Worker" }]);

  if (q.error) {
    if (q.error instanceof ApiError && q.error.status === 404)
      return (
        <EmptyState
          icon={<SearchX />}
          title="Worker not found"
          description="Offline workers are removed after 7 days."
          action={
            <Link to="/workers" className="text-sm text-accent hover:underline">
              Back to workers
            </Link>
          }
        />
      );
    return <ErrorState error={q.error} onRetry={() => q.refetch()} />;
  }
  if (!d) return <Skeleton className="h-96 w-full" />;
  const w = d.worker;
  const live = w.status === "ONLINE" || w.status === "STALE";
  const drainReason = !me?.platform_admin
    ? "Workers are shared by every workspace; only platform administrators can drain them."
    : w.drain_requested_at
      ? "Drain already requested."
      : !live
        ? "The worker is not running."
        : undefined;

  return (
    <>
      <PageHeader
        title={w.hostname}
        meta={
          <>
            <StatusBadge status={w.status} />
            <span className="text-xs text-fg-subtle">
              <Mono copy={w.id}>{shortId(w.id)}</Mono>
            </span>
            <span className="text-xs text-fg-subtle">
              · heartbeat <RelTime iso={w.last_heartbeat_at} />
            </span>
          </>
        }
        actions={
          <Button
            icon={<Power className="h-3.5 w-3.5" />}
            disabledReason={drainReason}
            onClick={() => setConfirm(true)}
          >
            Drain worker
          </Button>
        }
      />
      <div className="grid grid-cols-1 gap-5 lg:grid-cols-3">
        <div className="min-w-0 space-y-5 lg:col-span-2">
          <Panel title="Running now" description="Runs from your workspace currently held by this worker">
            {d.running_runs.length === 0 ? (
              <p className="px-4 py-6 text-center text-sm text-fg-subtle">
                Nothing from this workspace is running here.
              </p>
            ) : (
              <Table minWidth={560}>
                <THead>
                  <tr>
                    <TH>Job</TH>
                    <TH>Attempt</TH>
                    <TH>Started</TH>
                    <TH>Lease expires</TH>
                  </tr>
                </THead>
                <tbody>
                  {d.running_runs.map((r) => (
                    <TR key={r.id}>
                      <TD>
                        <Link to={`/jobs/${r.job_id}`} className="font-medium hover:underline">
                          {r.job_name}
                        </Link>
                      </TD>
                      <TD className="tabular text-fg-muted">
                        {r.attempt} / {r.max_attempts}
                      </TD>
                      <TD className="text-xs text-fg-muted">
                        <RelTime iso={r.started_at} />
                      </TD>
                      <TD className="text-xs text-fg-muted">
                        <RelTime iso={r.lease_expires_at} />
                      </TD>
                    </TR>
                  ))}
                </tbody>
              </Table>
            )}
          </Panel>
          <Panel title="Recent activity" description="Latest attempts from your workspace on this worker">
            {d.recent_attempts.length === 0 ? (
              <p className="px-4 py-6 text-center text-sm text-fg-subtle">No attempts recorded yet.</p>
            ) : (
              <Table minWidth={600}>
                <THead>
                  <tr>
                    <TH>Job</TH>
                    <TH>Status</TH>
                    <TH>Attempt</TH>
                    <TH align="right">Duration</TH>
                    <TH align="right">Started</TH>
                  </tr>
                </THead>
                <tbody>
                  {d.recent_attempts.map((a) => (
                    <TR key={a.id} onClick={() => setAttempt(a.id)}>
                      <TD className="max-w-[220px] truncate font-medium">{a.job_name}</TD>
                      <TD>
                        <StatusBadge status={a.status} />
                      </TD>
                      <TD className="tabular text-fg-muted">{a.attempt_number}</TD>
                      <TD align="right" className="tabular text-fg-muted">
                        {formatDuration(a.duration_ms)}
                      </TD>
                      <TD align="right" className="text-xs text-fg-muted">
                        <RelTime iso={a.started_at} />
                      </TD>
                    </TR>
                  ))}
                </tbody>
              </Table>
            )}
          </Panel>
        </div>
        <div className="space-y-5">
          <Panel title="Details" bodyClassName="p-4">
            <DescriptionList
              columns={1}
              items={[
                { label: "Utilisation", value: <Utilization w={w} /> },
                {
                  label: "Completed / failed attempts",
                  value: (
                    <span className="tabular">
                      {formatNumber(w.completed_count)} /{" "}
                      <span className={cn(w.failed_count && "text-danger")}>
                        {formatNumber(w.failed_count)}
                      </span>
                    </span>
                  ),
                },
                { label: "Heap memory", value: live ? formatBytes(w.heap_alloc_bytes) : "—" },
                {
                  label: "Goroutines",
                  value: live && w.goroutines != null ? formatNumber(w.goroutines) : "—",
                },
                { label: "Process", value: `pid ${w.pid} · ${w.version}` },
                { label: "Started", value: formatDateTime(w.started_at) },
                ...(w.stopped_at ? [{ label: "Stopped", value: formatDateTime(w.stopped_at) }] : []),
                ...(w.drain_requested_at
                  ? [{ label: "Drain requested", value: formatDateTime(w.drain_requested_at) }]
                  : []),
                {
                  label: "Handlers",
                  value: (
                    <div className="flex flex-wrap gap-1">
                      {w.handlers.map((h) => (
                        <code key={h} className="rounded bg-surface-2 px-1 text-2xs">
                          {h}
                        </code>
                      ))}
                    </div>
                  ),
                },
              ]}
            />
          </Panel>
          <Panel title="Lifecycle events" bodyClassName="divide-y">
            {d.events.length === 0 ? (
              <p className="px-4 py-6 text-center text-sm text-fg-subtle">No events.</p>
            ) : (
              d.events.map((e) => (
                <div key={e.id} className="px-4 py-2.5">
                  <p className="text-sm">{e.message}</p>
                  <p className="text-2xs text-fg-subtle">{formatDateTime(e.created_at)}</p>
                </div>
              ))
            )}
          </Panel>
        </div>
      </div>
      <ConfirmDialog
        open={confirm}
        onOpenChange={setConfirm}
        title={`Drain ${w.hostname}?`}
        confirmLabel="Drain"
        tone="danger"
        description="The worker stops claiming new runs, finishes what it is executing (within its shutdown grace period) and exits. Restart the process to bring it back."
        onConfirm={() => drain.mutateAsync(w.id)}
      />
      <ExecutionSheet attemptId={attempt} onClose={() => setAttempt(null)} />
    </>
  );
}
