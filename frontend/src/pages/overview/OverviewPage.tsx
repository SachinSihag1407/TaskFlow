import { useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { useMutation } from "@tanstack/react-query";
import { ArrowRight, Plus, Rocket, Sparkles } from "lucide-react";
import { toast } from "sonner";
import { usePageMeta } from "@/app/page-meta";
import { useCan, VIEWER_REASON } from "@/app/session";
import { useInvalidateJobs, useOverview } from "@/api/hooks";
import { api, errorMessage } from "@/lib/api";
import type { Job, Overview } from "@/lib/types";
import { formatDuration, formatNumber, formatPercent, shortId } from "@/lib/format";
import { storage, cn } from "@/lib/utils";
import { Button, LinkButton } from "@/components/ui/button";
import { PageHeader, Panel, Segmented } from "@/components/ui/layout";
import { Metric, MetricStrip } from "@/components/ui/metric";
import { EmptyState, ErrorState, Skeleton, TableSkeleton } from "@/components/ui/feedback";
import { StatusBadge } from "@/components/ui/badge";
import { Table, TD, TH, THead, TR } from "@/components/ui/table";
import { ThroughputChart } from "@/components/charts/ThroughputChart";
import { RelTime } from "@/components/common/Time";
import { ExecutionSheet } from "@/components/runs/ExecutionSheet";

const RANGES = [
  { value: "1h", label: "1h" },
  { value: "6h", label: "6h" },
  { value: "24h", label: "24h" },
  { value: "7d", label: "7d" },
  { value: "30d", label: "30d" },
] as const;
type Range = (typeof RANGES)[number]["value"];

const SAMPLE_JOB = {
  name: "Sample: daily sales report",
  description: "Created from the onboarding screen. Generates a report from deterministic sample data.",
  handler: "report.generate",
  payload: { report_type: "sales_summary", period_days: 30, seed: 42 },
  schedule_type: "once",
};

function Onboarding() {
  const navigate = useNavigate();
  const can = useCan();
  const invalidate = useInvalidateJobs();
  const sample = useMutation({
    mutationFn: () =>
      api.post<Job>("/jobs", SAMPLE_JOB, { "Idempotency-Key": `onboarding-sample-${Date.now()}` }),
    onSuccess: async (job) => {
      toast.success("Sample job created", { description: "It will run on the next scheduler tick." });
      await invalidate();
      navigate(`/jobs/${job.id}`);
    },
    onError: (e) => toast.error(errorMessage(e)),
  });
  return (
    <Panel>
      <EmptyState
        icon={<Rocket />}
        title="Run your first job"
        description="TaskFlow schedules jobs, dispatches them to workers and tracks every attempt. Create a job from a built-in handler, or start with a sample to see the whole lifecycle."
        action={
          <>
            <Button
              variant="primary"
              icon={<Sparkles className="h-3.5 w-3.5" />}
              loading={sample.isPending}
              disabledReason={can("write") ? undefined : VIEWER_REASON}
              onClick={() => sample.mutate()}
            >
              Create a sample job
            </Button>
            {can("write") && (
              <LinkButton to="/jobs/new" icon={<Plus className="h-3.5 w-3.5" />}>
                Create a job
              </LinkButton>
            )}
          </>
        }
      />
    </Panel>
  );
}

function LatencyRow({ label, value }: { label: string; value: number | null }) {
  return (
    <div className="flex items-center justify-between py-1.5 text-sm">
      <span className="text-fg-subtle">{label}</span>
      <span className="tabular font-medium">{formatDuration(value)}</span>
    </div>
  );
}

function QueuePanel({ ov }: { ov: Overview }) {
  const w = ov.workers;
  const util = w.capacity > 0 ? w.busy / w.capacity : 0;
  return (
    <Panel
      title="Queue & workers"
      actions={
        <Link to="/health" className="text-xs text-accent hover:underline">
          System health
        </Link>
      }
      bodyClassName="divide-y"
    >
      <div className="grid grid-cols-3 divide-x">
        {[
          { label: "Queued", value: ov.queued },
          { label: "Retry wait", value: ov.retry_wait },
          { label: "Dead-letter", value: ov.dead_letter, to: "/dead-letter", danger: ov.dead_letter > 0 },
        ].map((m) => {
          const body = (
            <>
              <div className="text-2xs text-fg-subtle">{m.label}</div>
              <div className={cn("tabular text-lg font-semibold", m.danger && "text-danger")}>
                {formatNumber(m.value)}
              </div>
            </>
          );
          return m.to ? (
            <Link key={m.label} to={m.to} className="px-4 py-3 hover:bg-surface-2/60">
              {body}
            </Link>
          ) : (
            <div key={m.label} className="px-4 py-3">
              {body}
            </div>
          );
        })}
      </div>
      <div className="px-4 py-3">
        <div className="flex items-center justify-between text-sm">
          <span className="text-fg-subtle">Worker slots in use</span>
          <span className="tabular font-medium">
            {w.busy} / {w.capacity}
          </span>
        </div>
        <div
          className="mt-2 h-1.5 overflow-hidden rounded-full bg-surface-3"
          role="progressbar"
          aria-valuemin={0}
          aria-valuemax={100}
          aria-valuenow={Math.round(util * 100)}
          aria-label="Worker utilisation"
        >
          <div
            className="h-full rounded-full bg-accent transition-[width]"
            style={{ width: `${Math.min(100, util * 100)}%` }}
          />
        </div>
        <div className="mt-3 flex flex-wrap gap-x-4 gap-y-1 text-xs">
          <span className="flex items-center gap-1.5">
            <span className="h-1.5 w-1.5 rounded-full bg-success-solid" />
            {w.online} online
          </span>
          {w.draining > 0 && (
            <span className="flex items-center gap-1.5">
              <span className="h-1.5 w-1.5 rounded-full bg-warning-solid" />
              {w.draining} draining
            </span>
          )}
          <span className="flex items-center gap-1.5">
            <span className={cn("h-1.5 w-1.5 rounded-full", w.stale ? "bg-warning-solid" : "bg-fg-faint")} />
            {w.stale} stale
          </span>
          <span className="flex items-center gap-1.5 text-fg-subtle">
            <span className="h-1.5 w-1.5 rounded-full bg-fg-faint" />
            {w.offline} offline
          </span>
        </div>
        {!ov.queue.redis_available && (
          <p className="mt-3 rounded bg-warning-soft px-2 py-1.5 text-xs text-warning">
            Redis is unavailable: workers are polling PostgreSQL directly.
          </p>
        )}
      </div>
      <div className="px-4 py-2">
        <div className="pb-1 pt-1 text-xs font-medium text-fg-muted">Execution latency</div>
        {ov.outcomes.latency_samples >= 5 ? (
          <>
            <LatencyRow label="p50" value={ov.outcomes.latency_p50_ms} />
            <LatencyRow label="p95" value={ov.outcomes.latency_p95_ms} />
            <LatencyRow label="p99" value={ov.outcomes.latency_p99_ms} />
            <LatencyRow label="Queue wait (avg)" value={ov.outcomes.queue_wait_avg_ms} />
          </>
        ) : (
          <p className="py-2 text-xs text-fg-subtle">
            Not enough data yet ({ov.outcomes.latency_samples} successful attempt
            {ov.outcomes.latency_samples === 1 ? "" : "s"} in range; percentiles need 5).
          </p>
        )}
      </div>
    </Panel>
  );
}

export default function OverviewPage() {
  usePageMeta([{ label: "Overview" }]);
  const [range, setRangeState] = useState<Range>(() => (storage.get("tf-range") as Range) || "24h");
  const setRange = (r: Range) => {
    storage.set("tf-range", r);
    setRangeState(r);
  };
  const q = useOverview(range);
  const can = useCan();
  const [attempt, setAttempt] = useState<string | null>(null);
  const ov = q.data;

  return (
    <>
      <PageHeader
        title="Overview"
        description="Activity across your workspace, computed from recorded executions."
        actions={
          <>
            <Segmented
              label="Time range"
              value={range}
              onChange={setRange}
              options={RANGES.map((r) => ({ ...r }))}
            />
            {can("write") && (
              <LinkButton to="/jobs/new" variant="primary" icon={<Plus className="h-3.5 w-3.5" />}>
                New job
              </LinkButton>
            )}
          </>
        }
      />

      {q.error && !ov ? (
        <Panel>
          <ErrorState error={q.error} onRetry={() => q.refetch()} />
        </Panel>
      ) : ov && ov.total_jobs === 0 ? (
        <Onboarding />
      ) : (
        <div className="space-y-5">
          <MetricStrip>
            <Metric
              label={`Succeeded · ${range}`}
              loading={!ov}
              tone="success"
              value={formatNumber(ov?.outcomes.runs_succeeded)}
              hint={`${formatNumber(ov?.outcomes.attempts_total)} attempts`}
              to="/executions?status=SUCCEEDED"
            />
            <Metric
              label={`Failed · ${range}`}
              loading={!ov}
              tone={ov && ov.outcomes.runs_failed > 0 ? "danger" : "default"}
              value={formatNumber(ov?.outcomes.runs_failed)}
              hint={`${formatNumber(ov?.outcomes.retries)} retries`}
              to="/dead-letter"
            />
            <Metric
              label="Success rate"
              loading={!ov}
              value={formatPercent(ov?.success_rate)}
              hint={ov?.success_rate == null ? "No finished runs in range" : "Of finished runs"}
            />
            <Metric
              label="Queued now"
              loading={!ov}
              value={formatNumber(ov?.queued)}
              hint={
                ov?.queue.redis_ready != null
                  ? `${formatNumber(ov.queue.redis_ready)} in Redis`
                  : "Redis unavailable"
              }
              to="/jobs?status=QUEUED"
            />
            <Metric
              label="Running now"
              loading={!ov}
              tone={ov && ov.running > 0 ? "info" : "default"}
              value={formatNumber(ov?.running)}
              hint={`${formatNumber(ov?.retry_wait)} awaiting retry`}
              to="/jobs?status=RUNNING"
            />
            <Metric
              label="Active workers"
              loading={!ov}
              tone={ov && ov.workers.online === 0 ? "danger" : "default"}
              value={formatNumber(ov?.workers.online)}
              hint={ov ? `${ov.workers.capacity} slots` : undefined}
              to="/workers"
            />
          </MetricStrip>

          <div className="grid grid-cols-1 gap-5 xl:grid-cols-3">
            <Panel
              title="Throughput"
              description="Finished attempts per interval"
              className="xl:col-span-2"
              bodyClassName="p-4"
            >
              {ov ? (
                <ThroughputChart data={ov.throughput} range={range} />
              ) : (
                <Skeleton className="h-[270px] w-full" />
              )}
            </Panel>
            {ov ? <QueuePanel ov={ov} /> : <Skeleton className="h-[360px] w-full rounded-lg" />}
          </div>

          <div className="grid grid-cols-1 items-start gap-5 xl:grid-cols-3">
            <Panel
              title="Recent executions"
              className="xl:col-span-2"
              actions={
                <Link
                  to="/executions"
                  className="inline-flex items-center gap-1 text-xs text-accent hover:underline"
                >
                  View all <ArrowRight className="h-3 w-3" />
                </Link>
              }
            >
              {!ov ? (
                <TableSkeleton rows={6} />
              ) : ov.recent_attempts.length === 0 ? (
                <EmptyState
                  title="No executions yet"
                  description="Executions appear here as soon as a worker picks up a run."
                />
              ) : (
                <Table minWidth={620}>
                  <THead>
                    <tr>
                      <TH>Job</TH>
                      <TH>Status</TH>
                      <TH>Attempt</TH>
                      <TH>Worker</TH>
                      <TH align="right">Duration</TH>
                      <TH align="right">Started</TH>
                    </tr>
                  </THead>
                  <tbody>
                    {ov.recent_attempts.map((a) => (
                      <TR key={a.id} onClick={() => setAttempt(a.id)}>
                        <TD className="max-w-[260px]">
                          <div className="truncate font-medium">{a.job_name}</div>
                          <div className="truncate font-mono text-2xs text-fg-subtle">{a.handler}</div>
                        </TD>
                        <TD>
                          <StatusBadge status={a.status} />
                        </TD>
                        <TD className="tabular text-fg-muted">
                          {a.attempt_number}
                          {a.max_attempts ? ` / ${a.max_attempts}` : ""}
                        </TD>
                        <TD className="max-w-[140px] truncate text-fg-muted">
                          {a.worker_hostname || shortId(a.worker_id)}
                        </TD>
                        <TD align="right" className="tabular text-fg-muted">
                          {formatDuration(a.duration_ms)}
                        </TD>
                        <TD align="right" className="text-fg-muted">
                          <RelTime iso={a.started_at} />
                        </TD>
                      </TR>
                    ))}
                  </tbody>
                </Table>
              )}
            </Panel>

            <Panel title="Recent events" bodyClassName="divide-y">
              {!ov ? (
                <div className="space-y-3 p-4">
                  {Array.from({ length: 6 }).map((_, i) => (
                    <Skeleton key={i} className="h-8 w-full" />
                  ))}
                </div>
              ) : ov.events.length === 0 ? (
                <EmptyState title="No events yet" />
              ) : (
                ov.events.map((e) => {
                  const inner = (
                    <>
                      <span
                        className={cn(
                          "mt-1.5 h-1.5 w-1.5 shrink-0 rounded-full",
                          e.severity === "error"
                            ? "bg-danger-solid"
                            : e.severity === "warning"
                              ? "bg-warning-solid"
                              : "bg-fg-faint",
                        )}
                        aria-label={e.severity}
                      />
                      <div className="min-w-0 flex-1">
                        <p className="line-clamp-2 text-sm">{e.message}</p>
                        <p className="mt-0.5 flex gap-2 text-2xs text-fg-subtle">
                          <span className="font-mono">{e.type}</span>
                          <RelTime iso={e.created_at} />
                        </p>
                      </div>
                    </>
                  );
                  const to = e.job_id ? `/jobs/${e.job_id}` : e.worker_id ? `/workers/${e.worker_id}` : null;
                  return to ? (
                    <Link key={e.id} to={to} className="flex gap-2.5 px-4 py-2.5 hover:bg-surface-2/60">
                      {inner}
                    </Link>
                  ) : (
                    <div key={e.id} className="flex gap-2.5 px-4 py-2.5">
                      {inner}
                    </div>
                  );
                })
              )}
            </Panel>
          </div>

          {ov && ov.by_handler.length > 0 && (
            <Panel title="Outcomes by handler" description={`Finished attempts · ${range}`}>
              <Table minWidth={520}>
                <THead>
                  <tr>
                    <TH>Handler</TH>
                    <TH align="right">Succeeded</TH>
                    <TH align="right">Failed</TH>
                    <TH className="w-[40%]">Failure rate</TH>
                  </tr>
                </THead>
                <tbody>
                  {ov.by_handler.map((h) => {
                    const total = h.succeeded + h.failed;
                    const rate = total ? h.failed / total : 0;
                    return (
                      <TR key={h.handler}>
                        <TD>
                          <code className="text-xs">{h.handler}</code>
                        </TD>
                        <TD align="right" className="tabular">
                          {formatNumber(h.succeeded)}
                        </TD>
                        <TD align="right" className="tabular">
                          {formatNumber(h.failed)}
                        </TD>
                        <TD>
                          <div className="flex items-center gap-2">
                            <div className="h-1.5 flex-1 overflow-hidden rounded-full bg-surface-3">
                              <div
                                className="h-full rounded-full bg-danger-solid"
                                style={{ width: `${rate * 100}%` }}
                              />
                            </div>
                            <span className="tabular w-12 text-right text-xs text-fg-muted">
                              {formatPercent(rate)}
                            </span>
                          </div>
                        </TD>
                      </TR>
                    );
                  })}
                </tbody>
              </Table>
            </Panel>
          )}
        </div>
      )}
      <ExecutionSheet attemptId={attempt} onClose={() => setAttempt(null)} />
    </>
  );
}
