import { useEffect, useMemo, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { Ban, CalendarClock, History, Info, RotateCcw, SearchX, Settings2 } from "lucide-react";
import { usePageMeta } from "@/app/page-meta";
import { useCan, VIEWER_REASON } from "@/app/session";
import { useJob, useRunAction } from "@/api/hooks";
import type { Run } from "@/lib/types";
import { formatDateTime, formatDuration, formatSeconds, shortId } from "@/lib/format";
import { priorityLabel, statusMeta } from "@/lib/status";
import { cn } from "@/lib/utils";
import { ApiError } from "@/lib/api";
import { Button } from "@/components/ui/button";
import { StatusBadge, StatusDot } from "@/components/ui/badge";
import { DescriptionList, PageHeader, Panel } from "@/components/ui/layout";
import { EmptyState, ErrorState, Skeleton } from "@/components/ui/feedback";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { JsonBlock, Mono } from "@/components/ui/code";
import { ConfirmDialog } from "@/components/ui/dialog";
import { RelTime } from "@/components/common/Time";
import { JobActions } from "@/components/jobs/JobActions";
import { describeCron, scheduleSummary } from "@/components/jobs/schedule";
import { ExecutionTimeline } from "@/components/runs/ExecutionTimeline";
import { ExecutionSheet } from "@/components/runs/ExecutionSheet";

const triggerCopy: Record<Run["trigger"], string> = {
  schedule: "Scheduled",
  manual: "Manual",
  retry: "Retry",
  replay: "Replay",
};

function runDuration(r: Run) {
  if (!r.started_at) return null;
  const end = r.finished_at ? new Date(r.finished_at).getTime() : Date.now();
  return end - new Date(r.started_at).getTime();
}

function RunPanel({
  run,
  attempts,
  onAttempt,
  selectedAttempt,
}: {
  run: Run;
  attempts: Parameters<typeof ExecutionTimeline>[0]["attempts"];
  onAttempt: (id: string) => void;
  selectedAttempt: string | null;
}) {
  const can = useCan();
  const act = useRunAction();
  const [confirm, setConfirm] = useState<"cancel" | "replay" | null>(null);
  const cancellable =
    run.status === "QUEUED" ||
    run.status === "RETRY_WAIT" ||
    (run.status === "RUNNING" && !run.cancel_requested_at);
  const replayable = run.status === "FAILED" && !run.replayed_at;
  const cancelReason = !can("write")
    ? VIEWER_REASON
    : run.cancel_requested_at && run.status === "RUNNING"
      ? "Cancellation already requested; waiting for the worker to stop."
      : !cancellable
        ? `This run has finished (${statusMeta(run.status).label.toLowerCase()}).`
        : undefined;
  const replayReason = !can("write")
    ? VIEWER_REASON
    : run.replayed_at
      ? "Already replayed."
      : run.status !== "FAILED"
        ? "Only failed runs can be replayed."
        : undefined;

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <div className="flex flex-wrap items-center gap-2">
            <span className="text-sm font-medium">Run {shortId(run.id)}</span>
            <StatusBadge status={run.status} />
            <span className="rounded bg-surface-2 px-1.5 text-2xs text-fg-subtle ring-1 ring-inset ring-line">
              {triggerCopy[run.trigger]}
            </span>
            {run.dead_lettered_at && (
              <span className="rounded bg-danger-soft px-1.5 text-2xs text-danger">Dead-lettered</span>
            )}
          </div>
          <div className="mt-1 flex flex-wrap gap-x-3 text-xs text-fg-subtle">
            <span>Scheduled for {formatDateTime(run.scheduled_for)}</span>
            {run.replay_of && (
              <span>
                Replay of <Mono>{shortId(run.replay_of)}</Mono>
              </span>
            )}
            {run.cancel_requested_at && run.status === "RUNNING" && (
              <span className="text-warning">Cancellation requested</span>
            )}
            {run.next_attempt_at && (
              <span>
                Next attempt <RelTime iso={run.next_attempt_at} />
              </span>
            )}
          </div>
        </div>
        <div className="flex gap-2">
          {replayable || run.status === "FAILED" ? (
            <Button
              size="sm"
              icon={<RotateCcw className="h-3.5 w-3.5" />}
              disabledReason={replayReason}
              onClick={() => setConfirm("replay")}
            >
              Replay
            </Button>
          ) : null}
          <Button
            size="sm"
            icon={<Ban className="h-3.5 w-3.5" />}
            disabledReason={cancelReason}
            onClick={() => setConfirm("cancel")}
          >
            Cancel run
          </Button>
        </div>
      </div>
      {run.last_error && run.status !== "SUCCEEDED" && (
        <div className="rounded-md border border-danger/20 bg-danger-soft/50 px-3 py-2">
          <div className="text-2xs font-medium uppercase tracking-wide text-danger">Last error</div>
          <p className="mt-0.5 break-words font-mono text-xs">{run.last_error}</p>
        </div>
      )}
      <div>
        <div className="mb-2 text-xs font-medium text-fg-muted">Attempts</div>
        <ExecutionTimeline
          attempts={attempts}
          onSelect={(a) => onAttempt(a.id)}
          selectedId={selectedAttempt ?? undefined}
        />
      </div>
      {run.status === "SUCCEEDED" && run.result != null && (
        <div>
          <div className="mb-2 text-xs font-medium text-fg-muted">Result</div>
          <JsonBlock value={run.result} maxHeight={200} />
        </div>
      )}
      <ConfirmDialog
        open={confirm === "cancel"}
        onOpenChange={(o) => !o && setConfirm(null)}
        tone="danger"
        title="Cancel this run?"
        confirmLabel="Cancel run"
        description={
          run.status === "RUNNING"
            ? "The worker will be asked to stop. If the handler finishes before it notices, the run is recorded as succeeded."
            : "The run will not execute."
        }
        onConfirm={() => act.mutateAsync({ id: run.id, action: "cancel" })}
      />
      <ConfirmDialog
        open={confirm === "replay"}
        onOpenChange={(o) => !o && setConfirm(null)}
        title="Replay this run?"
        confirmLabel="Replay"
        description="A new run is queued with the same payload and retry policy. The failed run stays in history and can only be replayed once."
        onConfirm={() => act.mutateAsync({ id: run.id, action: "replay" })}
      />
    </div>
  );
}

export default function JobDetailPage() {
  const { id } = useParams();
  const q = useJob(id);
  const d = q.data;
  usePageMeta([{ label: "Jobs", to: "/jobs" }, { label: d?.job.name ?? "Job" }]);
  const [runId, setRunId] = useState<string | null>(null);
  const [attemptId, setAttemptId] = useState<string | null>(null);

  useEffect(() => {
    if (d && (!runId || !d.runs.some((r) => r.id === runId))) setRunId(d.runs[0]?.id ?? null);
  }, [d, runId]);

  const selectedRun = d?.runs.find((r) => r.id === runId);
  const runAttempts = useMemo(() => {
    if (!d || !selectedRun) return [];
    return d.attempts
      .filter((a) => a.run_id === selectedRun.id)
      .map((a) => ({ ...a, max_attempts: selectedRun.max_attempts }));
  }, [d, selectedRun]);

  if (q.error) {
    if (q.error instanceof ApiError && q.error.status === 404) {
      return (
        <EmptyState
          icon={<SearchX />}
          title="Job not found"
          description="It may belong to another workspace or the link is wrong."
          action={
            <Link to="/jobs" className="text-sm text-accent hover:underline">
              Back to jobs
            </Link>
          }
        />
      );
    }
    return <ErrorState error={q.error} onRetry={() => q.refetch()} />;
  }
  if (!d) {
    return (
      <div className="space-y-4">
        <Skeleton className="h-8 w-72" />
        <Skeleton className="h-4 w-96" />
        <div className="grid grid-cols-1 gap-5 lg:grid-cols-3">
          <Skeleton className="h-96 lg:col-span-2" />
          <Skeleton className="h-96" />
        </div>
      </div>
    );
  }
  const j = d.job;

  return (
    <>
      <PageHeader
        title={<span className="break-words">{j.name}</span>}
        description={j.description || undefined}
        meta={
          <>
            <span className="flex items-center gap-1.5 text-xs text-fg-subtle" data-testid="job-state">
              Current state <StatusBadge status={j.status} />
            </span>
            {j.last_run_status && (
              <span className="flex items-center gap-1.5 text-xs text-fg-subtle">
                · Last run <StatusDot status={j.last_run_status} /> <RelTime iso={j.last_run_at} />
              </span>
            )}
            <span className="text-xs text-fg-subtle">
              · <code>{j.handler}</code>
            </span>
            <span className="text-xs text-fg-subtle">
              · <Mono copy={j.id}>{shortId(j.id)}</Mono>
            </span>
          </>
        }
        actions={
          <JobActions
            jobId={j.id}
            jobName={j.name}
            status={j.status}
            actions={d.actions}
            activeRuns={d.active_runs}
          />
        }
      />

      <div className="grid grid-cols-1 gap-5 lg:grid-cols-3">
        <div className="min-w-0 lg:col-span-2">
          <Tabs defaultValue="runs">
            <TabsList className="mb-4">
              <TabsTrigger value="runs" count={d.runs.length}>
                <History className="h-3.5 w-3.5" />
                Runs
              </TabsTrigger>
              <TabsTrigger value="activity" count={d.events.length}>
                Activity
              </TabsTrigger>
              <TabsTrigger value="config">
                <Settings2 className="h-3.5 w-3.5" />
                Configuration
              </TabsTrigger>
            </TabsList>

            <TabsContent value="runs" className="focus:outline-none">
              {d.runs.length === 0 ? (
                <Panel>
                  <EmptyState
                    icon={<CalendarClock />}
                    title="No runs yet"
                    description={
                      j.next_run_at ? (
                        <>
                          The first run is scheduled <RelTime iso={j.next_run_at} />. Runs appear here as soon
                          as the scheduler enqueues them.
                        </>
                      ) : (
                        "This job has no upcoming runs."
                      )
                    }
                  />
                </Panel>
              ) : (
                <div className="grid grid-cols-1 gap-4 xl:grid-cols-[240px_1fr]">
                  <Panel bodyClassName="scrollbar-thin max-h-[560px] overflow-y-auto p-1">
                    <ul role="listbox" aria-label="Runs">
                      {d.runs.map((r) => (
                        <li key={r.id}>
                          <button
                            role="option"
                            aria-selected={r.id === runId}
                            onClick={() => setRunId(r.id)}
                            className={cn(
                              "flex w-full items-center justify-between gap-2 rounded-md px-2.5 py-2 text-left",
                              r.id === runId ? "bg-surface-3" : "hover:bg-surface-2",
                            )}
                          >
                            <span className="min-w-0">
                              <span className="block truncate text-xs font-medium">
                                {formatDateTime(r.scheduled_for)}
                              </span>
                              <span className="tabular block text-2xs text-fg-subtle">
                                {triggerCopy[r.trigger]} · {r.attempt} attempt{r.attempt === 1 ? "" : "s"} ·{" "}
                                {formatDuration(runDuration(r))}
                              </span>
                            </span>
                            <StatusDot status={r.status} label="" />
                          </button>
                        </li>
                      ))}
                    </ul>
                  </Panel>
                  <Panel bodyClassName="p-4">
                    {selectedRun ? (
                      <RunPanel
                        run={selectedRun}
                        attempts={runAttempts}
                        onAttempt={setAttemptId}
                        selectedAttempt={attemptId}
                      />
                    ) : null}
                  </Panel>
                </div>
              )}
            </TabsContent>

            <TabsContent value="activity" className="focus:outline-none">
              <Panel bodyClassName="divide-y">
                {d.events.length === 0 ? (
                  <EmptyState title="No activity yet" />
                ) : (
                  d.events.map((e) => (
                    <div key={e.id} className="flex gap-3 px-4 py-2.5">
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
                        <p className="text-sm">{e.message}</p>
                        <p className="mt-0.5 flex flex-wrap gap-x-2 text-2xs text-fg-subtle">
                          <span className="font-mono">{e.type}</span>
                          <span>{formatDateTime(e.created_at)}</span>
                          {e.run_id && (
                            <button
                              className="text-accent hover:underline"
                              onClick={() => setRunId(e.run_id)}
                            >
                              run {shortId(e.run_id)}
                            </button>
                          )}
                        </p>
                      </div>
                    </div>
                  ))
                )}
              </Panel>
            </TabsContent>

            <TabsContent value="config" className="focus:outline-none">
              <Panel bodyClassName="p-5 space-y-6">
                <DescriptionList
                  items={[
                    {
                      label: "Handler",
                      value: (
                        <>
                          <code className="text-xs">{j.handler}</code>
                          {d.handler && <span className="ml-2 text-fg-subtle">{d.handler.title}</span>}
                        </>
                      ),
                    },
                    { label: "Schedule", value: scheduleSummary(j) },
                    ...(j.schedule_type === "cron"
                      ? [
                          { label: "Cron expression", value: <code className="text-xs">{j.cron_expr}</code> },
                          { label: "Timezone", value: j.timezone },
                          {
                            label: "Missed runs",
                            value: {
                              run_once: "Run once to catch up",
                              run_all: "Run each missed occurrence",
                              skip: "Skip",
                            }[j.missed_run_policy],
                          },
                          {
                            label: "Overlap",
                            value: {
                              skip: "Skip new occurrence",
                              queue: "Queue and run after",
                              allow: `Concurrent (max ${j.max_concurrency})`,
                            }[j.overlap_policy],
                          },
                        ]
                      : []),
                    { label: "Priority", value: `${j.priority} · ${priorityLabel(j.priority)}` },
                    { label: "Timeout", value: formatSeconds(j.timeout_seconds) },
                    { label: "Max attempts", value: j.max_attempts },
                    {
                      label: "Retry backoff",
                      value: `${formatSeconds(j.backoff_base_seconds)} → ${formatSeconds(j.backoff_max_seconds)}, exponential with jitter`,
                    },
                    { label: "Version", value: j.version },
                    {
                      label: "Idempotency key",
                      value: j.idempotency_key ? (
                        <Mono copy={j.idempotency_key}>{j.idempotency_key}</Mono>
                      ) : (
                        "—"
                      ),
                    },
                  ]}
                />
                <div>
                  <div className="mb-2 text-xs text-fg-subtle">Payload</div>
                  <JsonBlock value={j.payload} />
                </div>
              </Panel>
            </TabsContent>
          </Tabs>
        </div>

        <div className="space-y-5">
          <Panel title="Schedule" bodyClassName="p-4">
            {j.schedule_type === "cron" && (
              <p className="mb-3 text-sm">
                {describeCron(j.cron_expr)} <span className="text-fg-subtle">({j.timezone})</span>
              </p>
            )}
            {j.status === "PAUSED" ? (
              <p className="flex items-start gap-2 text-sm text-fg-muted">
                <Info className="mt-0.5 h-4 w-4 shrink-0" />
                Paused — no runs will be scheduled until the job is resumed.
              </p>
            ) : j.status === "CANCELLED" ? (
              <p className="text-sm text-fg-muted">Cancelled — nothing further is scheduled.</p>
            ) : d.upcoming_runs.length ? (
              <>
                <div className="mb-1.5 text-2xs uppercase tracking-wide text-fg-subtle">Upcoming</div>
                <ol className="space-y-1.5">
                  {d.upcoming_runs.map((t, i) => (
                    <li
                      key={t}
                      className={cn(
                        "flex items-center justify-between gap-2 text-sm",
                        i === 0 ? "font-medium" : "text-fg-muted",
                      )}
                    >
                      <span className="tabular">{formatDateTime(t, j.timezone)}</span>
                      {i === 0 && (
                        <span className="text-xs text-fg-subtle">
                          <RelTime iso={t} />
                        </span>
                      )}
                    </li>
                  ))}
                </ol>
              </>
            ) : (
              <p className="text-sm text-fg-muted">No upcoming runs.</p>
            )}
          </Panel>
          <Panel title="Summary" bodyClassName="p-4">
            <DescriptionList
              columns={1}
              items={[
                { label: "Active runs", value: <span className="tabular">{d.active_runs}</span> },
                {
                  label: "Last run",
                  value: j.last_run_at ? (
                    <span className="flex items-center gap-2">
                      {j.last_run_status && <StatusBadge status={j.last_run_status} />}
                      <RelTime iso={j.last_run_at} />
                    </span>
                  ) : (
                    "Never"
                  ),
                },
                { label: "Created", value: formatDateTime(j.created_at) },
                { label: "Updated", value: formatDateTime(j.updated_at) },
              ]}
            />
          </Panel>
          {d.handler && (
            <Panel title="About this handler" bodyClassName="p-4 space-y-2 text-sm">
              <p className="text-fg-muted">{d.handler.description}</p>
              <p className="text-xs text-fg-subtle">
                <span className="font-medium text-fg-muted">Idempotency: </span>
                {d.handler.idempotency}
              </p>
            </Panel>
          )}
        </div>
      </div>
      <ExecutionSheet attemptId={attemptId} onClose={() => setAttemptId(null)} />
    </>
  );
}
