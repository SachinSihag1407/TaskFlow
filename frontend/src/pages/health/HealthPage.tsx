import { AlertTriangle, CheckCircle2, CircleSlash, ExternalLink, XCircle } from "lucide-react";
import { usePageMeta } from "@/app/page-meta";
import { useLiveStatus } from "@/app/live";
import { useHealth } from "@/api/hooks";
import type { ComponentHealth } from "@/lib/types";
import { formatNumber, formatPercent, shortId } from "@/lib/format";
import { cn } from "@/lib/utils";
import { PageHeader, Panel } from "@/components/ui/layout";
import { Metric, MetricStrip } from "@/components/ui/metric";
import { ErrorState, Skeleton } from "@/components/ui/feedback";
import { Table, TD, TH, THead, TR } from "@/components/ui/table";
import { RelTime } from "@/components/common/Time";

const statusUi = {
  healthy: { icon: CheckCircle2, cls: "text-success", label: "Healthy" },
  degraded: { icon: AlertTriangle, cls: "text-warning", label: "Degraded" },
  down: { icon: XCircle, cls: "text-danger", label: "Down" },
  disabled: { icon: CircleSlash, cls: "text-fg-subtle", label: "Disabled" },
};

const componentCopy: Record<string, string> = {
  api: "API server",
  postgres: "PostgreSQL",
  redis: "Redis",
  scheduler: "Scheduler",
  workers: "Workers",
};

function ComponentRow({ c }: { c: ComponentHealth }) {
  const ui = statusUi[c.status];
  return (
    <div className="flex items-start gap-3 px-4 py-3">
      <ui.icon className={cn("mt-0.5 h-4 w-4 shrink-0", ui.cls)} aria-hidden />
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <span className="text-sm font-medium">{componentCopy[c.name] ?? c.name}</span>
          <span className={cn("text-xs font-medium", ui.cls)}>{ui.label}</span>
        </div>
        <p className="text-xs text-fg-subtle">{c.detail}</p>
      </div>
      {c.latency_ms > 0 && (
        <span className="tabular shrink-0 text-xs text-fg-subtle">
          {c.latency_ms.toFixed(c.latency_ms < 10 ? 1 : 0)} ms
        </span>
      )}
    </div>
  );
}

export default function HealthPage() {
  usePageMeta([{ label: "System health" }]);
  const q = useHealth();
  const live = useLiveStatus();
  const h = q.data;
  const redisDisabled = h?.components.some((c) => c.name === "redis" && c.status === "disabled") ?? false;

  if (q.error && !h) return <ErrorState error={q.error} onRetry={() => q.refetch()} />;
  const overall = h ? statusUi[h.status] : null;

  return (
    <>
      <PageHeader
        title="System health"
        description="Dependencies, scheduler leadership, worker availability and queue state. Refreshes every 10 seconds."
        meta={
          h && (
            <span className="text-xs text-fg-subtle">
              Checked <RelTime iso={h.checked_at} />
            </span>
          )
        }
      />
      {h && h.status !== "healthy" && (
        <div
          role="alert"
          className={cn(
            "mb-5 flex items-start gap-2 rounded-lg border px-4 py-3 text-sm",
            h.status === "down"
              ? "border-danger/30 bg-danger-soft text-danger"
              : "border-warning/30 bg-warning-soft text-warning",
          )}
        >
          <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0" />
          <div>
            <div className="font-medium">
              {h.status === "down" ? "TaskFlow is down" : "Running in a degraded state"}
            </div>
            <ul className="mt-0.5 list-disc pl-4 text-xs">
              {h.components
                .filter((c) => c.status !== "healthy" && c.status !== "disabled")
                .map((c) => (
                  <li key={c.name}>
                    {componentCopy[c.name] ?? c.name}: {c.detail}
                  </li>
                ))}
            </ul>
          </div>
        </div>
      )}

      <MetricStrip className="mb-5">
        <Metric
          label="Overall"
          loading={!h}
          value={overall ? <span className={overall.cls}>{overall.label}</span> : ""}
        />
        <Metric
          label="Online workers"
          loading={!h}
          value={h?.workers.online}
          hint={h ? `${h.workers.busy}/${h.workers.capacity} slots busy` : undefined}
          tone={h && h.workers.online === 0 ? "danger" : "default"}
        />
        <Metric
          label="Queued (PostgreSQL)"
          loading={!h}
          value={formatNumber(h?.queue.postgres_queued)}
          hint={h ? `${formatNumber(h.queue.postgres_retry_wait)} waiting to retry` : undefined}
        />
        <Metric
          label="Ready (Redis)"
          loading={!h}
          value={h?.queue.redis_ready == null ? "—" : formatNumber(h.queue.redis_ready)}
          hint={
            h?.queue.redis_delayed != null ? `${formatNumber(h.queue.redis_delayed)} deferred` : "Unavailable"
          }
        />
        <Metric
          label={`Error rate · ${h?.rates.window_minutes ?? 60}m`}
          loading={!h}
          value={formatPercent(h?.rates.error_rate)}
          hint={
            h ? `${formatNumber(h.rates.failed)} of ${formatNumber(h.rates.attempts)} attempts` : undefined
          }
          tone={h?.rates.error_rate && h.rates.error_rate > 0.1 ? "danger" : "default"}
        />
        <Metric
          label={`Retry rate · ${h?.rates.window_minutes ?? 60}m`}
          loading={!h}
          value={formatPercent(h?.rates.retry_rate)}
          hint={h ? `${formatNumber(h.rates.retries)} retries` : undefined}
        />
      </MetricStrip>

      <div className="grid grid-cols-1 gap-5 lg:grid-cols-2">
        <Panel title="Components" bodyClassName="divide-y">
          {!h ? (
            <div className="space-y-3 p-4">
              {Array.from({ length: 5 }).map((_, i) => (
                <Skeleton key={i} className="h-10 w-full" />
              ))}
            </div>
          ) : (
            h.components.map((c) => <ComponentRow key={c.name} c={c} />)
          )}
          {h && (
            <ComponentRow
              c={{
                name: "Live updates",
                status: redisDisabled
                  ? "disabled"
                  : live === "live"
                    ? "healthy"
                    : live === "connecting"
                      ? "degraded"
                      : h.live_updates
                        ? "degraded"
                        : "down",
                latency_ms: 0,
                detail: redisDisabled
                  ? "Not used in this deployment; pages refresh every 5 seconds."
                  : live === "live"
                    ? `Server-Sent Events connected (${h.sse_clients} client${h.sse_clients === 1 ? "" : "s"} on this API instance).`
                    : "Not receiving real-time events; pages fall back to polling.",
              }}
            />
          )}
        </Panel>

        <div className="space-y-5">
          <Panel
            title="Scheduler instances"
            description="One instance holds leadership via a PostgreSQL advisory lock."
          >
            {!h ? (
              <Skeleton className="m-4 h-16" />
            ) : h.schedulers.length === 0 ? (
              <p className="px-4 py-6 text-center text-sm text-danger">
                No scheduler has reported in the last 5 minutes. Due jobs are not being enqueued.
              </p>
            ) : (
              <Table minWidth={460}>
                <THead>
                  <tr>
                    <TH>Instance</TH>
                    <TH>Role</TH>
                    <TH>Heartbeat</TH>
                    <TH align="right">Last tick</TH>
                  </tr>
                </THead>
                <tbody>
                  {h.schedulers.map((s) => (
                    <TR key={s.id}>
                      <TD>
                        <div className="font-medium">{s.hostname}</div>
                        <div className="font-mono text-2xs text-fg-subtle">{shortId(s.id)}</div>
                      </TD>
                      <TD className="text-xs">
                        {s.stopped_at ? (
                          <span className="text-fg-subtle">Stopped</span>
                        ) : s.is_leader ? (
                          <span className="font-medium text-success">Leader</span>
                        ) : (
                          <span className="text-fg-muted">Standby</span>
                        )}
                      </TD>
                      <TD className="text-xs text-fg-muted">
                        <RelTime iso={s.last_heartbeat_at} />
                      </TD>
                      <TD align="right" className="tabular text-xs text-fg-muted">
                        {s.last_tick_ms != null
                          ? `${s.last_tick_ms} ms · ${s.last_tick_enqueued ?? 0} enqueued`
                          : "—"}
                      </TD>
                    </TR>
                  ))}
                </tbody>
              </Table>
            )}
          </Panel>

          <Panel title="Prometheus metrics" bodyClassName="p-4 space-y-3 text-sm">
            <p className="text-fg-muted">
              Every process exposes Prometheus metrics. Scrape them for dashboards and alerts:
            </p>
            <ul className="space-y-1.5 font-mono text-xs">
              <li className="flex flex-wrap items-center gap-2">
                <span className="w-20 font-sans text-fg-subtle">API</span>
                <a
                  href="/metrics"
                  target="_blank"
                  rel="noreferrer"
                  className="inline-flex items-center gap-1 text-accent hover:underline"
                >
                  /metrics <ExternalLink className="h-3 w-3" />
                </a>
              </li>
              <li className="flex flex-wrap items-center gap-2">
                <span className="w-20 font-sans text-fg-subtle">Scheduler</span>scheduler:9090/metrics
              </li>
              <li className="flex flex-wrap items-center gap-2">
                <span className="w-20 font-sans text-fg-subtle">Workers</span>worker:9090/metrics
              </li>
            </ul>
            <p className="text-xs text-fg-subtle [overflow-wrap:anywhere]">
              Key series: <code>taskflow_queue_depth</code>, <code>taskflow_attempt_duration_seconds</code>,{" "}
              <code>taskflow_queue_wait_seconds</code>, <code>taskflow_leases_recovered_total</code>,{" "}
              <code>taskflow_redis_errors_total</code>.
            </p>
          </Panel>
        </div>
      </div>
    </>
  );
}
