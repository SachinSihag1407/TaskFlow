import { Link, useNavigate } from "react-router-dom";
import { CalendarClock, Plus } from "lucide-react";
import { usePageMeta } from "@/app/page-meta";
import { useCan } from "@/app/session";
import { useJobAction, useSchedules } from "@/api/hooks";
import { formatDateTime } from "@/lib/format";
import { statusMeta, toneClasses } from "@/lib/status";
import { cn } from "@/lib/utils";
import { Button, LinkButton } from "@/components/ui/button";
import { PageHeader, Panel } from "@/components/ui/layout";
import { StatusBadge } from "@/components/ui/badge";
import { Table, TD, TH, THead, TR } from "@/components/ui/table";
import { EmptyState, ErrorState, TableSkeleton } from "@/components/ui/feedback";
import { Tooltip } from "@/components/ui/tooltip";
import { RelTime } from "@/components/common/Time";
import { describeCron } from "@/components/jobs/schedule";

const missedCopy = { run_once: "Catch up once", run_all: "Catch up all", skip: "Skip missed" };
const overlapCopy = { skip: "No overlap", queue: "Queue overlap", allow: "Concurrent" };

export default function SchedulesPage() {
  usePageMeta([{ label: "Schedules" }]);
  const q = useSchedules();
  const can = useCan();
  const navigate = useNavigate();
  const act = useJobAction();
  const rows = q.data ?? [];

  return (
    <>
      <PageHeader
        title="Schedules"
        description="Recurring jobs, their cron semantics and recent history."
        actions={
          can("write") && (
            <LinkButton to="/jobs/new" variant="primary" icon={<Plus className="h-3.5 w-3.5" />}>
              New recurring job
            </LinkButton>
          )
        }
      />
      <Panel>
        {q.isLoading ? (
          <TableSkeleton rows={6} cols={6} />
        ) : q.error ? (
          <ErrorState error={q.error} onRetry={() => q.refetch()} />
        ) : rows.length === 0 ? (
          <EmptyState
            icon={<CalendarClock />}
            title="No recurring jobs"
            description="Create a job with a cron schedule to see it here."
            action={
              can("write") && (
                <LinkButton to="/jobs/new" variant="primary">
                  Create recurring job
                </LinkButton>
              )
            }
          />
        ) : (
          <Table minWidth={980}>
            <THead>
              <tr>
                <TH>Job</TH>
                <TH>Schedule</TH>
                <TH>Policies</TH>
                <TH>Next run</TH>
                <TH>Recent runs</TH>
                <TH>Status</TH>
                <TH align="right">
                  <span className="sr-only">Actions</span>
                </TH>
              </tr>
            </THead>
            <tbody>
              {rows.map(({ job: j, upcoming_runs, recent_runs }) => (
                <TR key={j.id} onClick={() => navigate(`/jobs/${j.id}`)}>
                  <TD className="max-w-[240px]">
                    <Link
                      to={`/jobs/${j.id}`}
                      onClick={(e) => e.stopPropagation()}
                      className="block truncate font-medium hover:underline"
                    >
                      {j.name}
                    </Link>
                    <span className="block truncate font-mono text-2xs text-fg-subtle">{j.handler}</span>
                  </TD>
                  <TD className="max-w-[260px]">
                    <code className="text-xs">{j.cron_expr}</code>
                    <span className="block truncate text-xs text-fg-subtle" title={describeCron(j.cron_expr)}>
                      {describeCron(j.cron_expr)} · {j.timezone}
                    </span>
                  </TD>
                  <TD className="text-xs text-fg-muted">
                    <div>{missedCopy[j.missed_run_policy]}</div>
                    <div>
                      {overlapCopy[j.overlap_policy]}
                      {j.overlap_policy === "allow" ? ` (${j.max_concurrency})` : ""}
                    </div>
                  </TD>
                  <TD className="text-xs">
                    {upcoming_runs[0] ? (
                      <Tooltip
                        content={
                          <div className="space-y-0.5">
                            {upcoming_runs.map((t) => (
                              <div key={t}>{formatDateTime(t, j.timezone)}</div>
                            ))}
                          </div>
                        }
                      >
                        <span className="tabular">
                          <RelTime iso={upcoming_runs[0]} />
                          <span className="block text-fg-subtle">
                            {formatDateTime(upcoming_runs[0], j.timezone)}
                          </span>
                        </span>
                      </Tooltip>
                    ) : (
                      <span className="text-fg-faint">{j.status === "PAUSED" ? "Paused" : "—"}</span>
                    )}
                  </TD>
                  <TD>
                    {recent_runs.length === 0 ? (
                      <span className="text-xs text-fg-faint">No runs yet</span>
                    ) : (
                      <div className="flex items-center gap-1" aria-label="Recent run outcomes, newest last">
                        {[...recent_runs].reverse().map((r) => {
                          const m = statusMeta(r.status);
                          return (
                            <Tooltip key={r.id} content={`${m.label} · ${formatDateTime(r.scheduled_for)}`}>
                              <span className={cn("h-4 w-1.5 rounded-sm", toneClasses[m.tone].dot)} />
                            </Tooltip>
                          );
                        })}
                      </div>
                    )}
                  </TD>
                  <TD>
                    <StatusBadge status={j.status} />
                  </TD>
                  <TD align="right" onClick={(e) => e.stopPropagation()}>
                    {can("write") &&
                      j.status !== "CANCELLED" &&
                      (j.status === "PAUSED" ? (
                        <Button size="sm" onClick={() => act.mutate({ id: j.id, action: "resume" })}>
                          Resume
                        </Button>
                      ) : (
                        <Button
                          size="sm"
                          variant="ghost"
                          onClick={() => act.mutate({ id: j.id, action: "pause" })}
                        >
                          Pause
                        </Button>
                      ))}
                  </TD>
                </TR>
              ))}
            </tbody>
          </Table>
        )}
      </Panel>
      <p className="mt-3 text-xs text-fg-subtle">
        Daylight saving: fixed-time schedules run once per wall-clock time (times skipped by a spring-forward
        change run right after it); hourly schedules follow real elapsed time.
      </p>
    </>
  );
}
