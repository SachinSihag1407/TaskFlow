import { useNavigate } from "react-router-dom";
import { Cpu } from "lucide-react";
import { usePageMeta } from "@/app/page-meta";
import { useWorkers } from "@/api/hooks";
import { formatBytes, formatNumber, shortId } from "@/lib/format";
import { cn } from "@/lib/utils";
import { PageHeader, Panel } from "@/components/ui/layout";
import { StatusBadge } from "@/components/ui/badge";
import { Table, TD, TH, THead, TR } from "@/components/ui/table";
import { EmptyState, ErrorState, TableSkeleton } from "@/components/ui/feedback";
import { Metric, MetricStrip } from "@/components/ui/metric";
import { RelTime } from "@/components/common/Time";
import { Utilization } from "@/components/workers/Utilization";

export default function WorkersPage() {
  usePageMeta([{ label: "Workers" }]);
  const q = useWorkers();
  const navigate = useNavigate();
  const ws = q.data ?? [];
  const count = (s: string) => ws.filter((w) => w.status === s).length;
  const online = ws.filter((w) => w.status === "ONLINE");
  const capacity = online.reduce((n, w) => n + w.concurrency, 0);
  const busy = online.reduce((n, w) => n + w.active_runs, 0);

  return (
    <>
      <PageHeader
        title="Workers"
        description="Worker processes, their heartbeats and what they are running. Refreshes every 5 seconds."
      />
      <MetricStrip className="mb-5 lg:grid-cols-5">
        <Metric label="Online" value={count("ONLINE")} tone="success" loading={q.isLoading} />
        <Metric
          label="Stale"
          value={count("STALE")}
          tone={count("STALE") ? "warning" : "default"}
          hint="Heartbeat overdue"
          loading={q.isLoading}
        />
        <Metric label="Draining" value={count("DRAINING")} loading={q.isLoading} />
        <Metric label="Offline (24h)" value={count("OFFLINE")} loading={q.isLoading} />
        <Metric
          label="Slots in use"
          value={`${busy} / ${capacity}`}
          hint="Across online workers"
          loading={q.isLoading}
        />
      </MetricStrip>
      <Panel>
        {q.isLoading ? (
          <TableSkeleton rows={4} cols={7} />
        ) : q.error ? (
          <ErrorState error={q.error} onRetry={() => q.refetch()} />
        ) : ws.length === 0 ? (
          <EmptyState
            icon={<Cpu />}
            title="No workers have reported"
            description={
              <>
                Start a worker with <code className="text-xs">taskflow worker</code> (or{" "}
                <code className="text-xs">docker compose up --scale worker=3</code>). Queued runs wait until a
                worker is online.
              </>
            }
          />
        ) : (
          <Table minWidth={960}>
            <THead>
              <tr>
                <TH>Worker</TH>
                <TH>Status</TH>
                <TH>Last heartbeat</TH>
                <TH>Active</TH>
                <TH align="right">Completed</TH>
                <TH align="right">Failed</TH>
                <TH align="right">Memory</TH>
                <TH align="right">Started</TH>
              </tr>
            </THead>
            <tbody>
              {ws.map((w) => (
                <TR key={w.id} onClick={() => navigate(`/workers/${w.id}`)}>
                  <TD className="max-w-[240px]">
                    <div className="truncate font-medium">{w.hostname}</div>
                    <div className="truncate font-mono text-2xs text-fg-subtle">
                      {shortId(w.id)} · pid {w.pid} · {w.version}
                    </div>
                  </TD>
                  <TD>
                    <StatusBadge status={w.status} />
                  </TD>
                  <TD className={cn("text-xs", w.status === "STALE" ? "text-warning" : "text-fg-muted")}>
                    <RelTime iso={w.last_heartbeat_at} />
                  </TD>
                  <TD>
                    <Utilization w={w} />
                  </TD>
                  <TD align="right" className="tabular">
                    {formatNumber(w.completed_count)}
                  </TD>
                  <TD
                    align="right"
                    className={cn("tabular", w.failed_count ? "text-danger" : "text-fg-subtle")}
                  >
                    {formatNumber(w.failed_count)}
                  </TD>
                  <TD align="right" className="tabular text-xs text-fg-muted">
                    {w.status === "OFFLINE" ? "—" : formatBytes(w.heap_alloc_bytes)}
                  </TD>
                  <TD align="right" className="text-xs text-fg-muted">
                    <RelTime iso={w.started_at} />
                  </TD>
                </TR>
              ))}
            </tbody>
          </Table>
        )}
      </Panel>
      <p className="mt-3 text-xs text-fg-subtle">
        A worker is <span className="font-medium">stale</span> when its heartbeat is overdue and{" "}
        <span className="font-medium">offline</span> after prolonged silence. Runs held by silent workers are
        recovered when their lease expires.
      </p>
    </>
  );
}
