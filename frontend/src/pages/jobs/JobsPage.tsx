import { useMemo, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import {
  Ban,
  CalendarClock,
  ListChecks,
  MoreHorizontal,
  Pause,
  Play,
  Plus,
  RotateCcw,
  X,
} from "lucide-react";
import { usePageMeta } from "@/app/page-meta";
import { useCan, VIEWER_REASON } from "@/app/session";
import { useBulkJobs, useHandlers, useJobAction, useJobs } from "@/api/hooks";
import type { JobListItem } from "@/lib/types";
import { JOB_STATUSES, priorityLabel, statusMeta } from "@/lib/status";
import { plural } from "@/lib/format";
import { useUrlFilters } from "@/lib/url-state";
import { cn } from "@/lib/utils";
import { Button, LinkButton } from "@/components/ui/button";
import { PageHeader, Panel } from "@/components/ui/layout";
import { Checkbox, Select } from "@/components/ui/input";
import { StatusBadge, StatusDot } from "@/components/ui/badge";
import { Table, TD, TH, THead, TR } from "@/components/ui/table";
import { Pagination } from "@/components/ui/pagination";
import { EmptyState, ErrorState, TableSkeleton } from "@/components/ui/feedback";
import { ConfirmDialog } from "@/components/ui/dialog";
import {
  Dropdown,
  DropdownContent,
  DropdownItem,
  DropdownSeparator,
  DropdownTrigger,
} from "@/components/ui/dropdown";
import { FilterBar, MultiFilter, SearchInput } from "@/components/common/Filters";
import { RelTime } from "@/components/common/Time";
import { scheduleSummary } from "@/components/jobs/schedule";

const DEFAULTS = {
  q: "",
  status: "",
  handler: "",
  schedule_type: "",
  sort: "created_at",
  order: "desc",
  page: "1",
  page_size: "25",
};

const ACTIVE = new Set(["QUEUED", "RUNNING", "RETRY_WAIT"]);

/** Client-side mirror of the server rules, used only to grey out row menu items. */
function rowActions(j: JobListItem) {
  const recurring = j.schedule_type === "cron";
  const finished = !recurring && (j.status === "SUCCEEDED" || j.status === "FAILED");
  return {
    pause:
      j.status !== "PAUSED" &&
      j.status !== "CANCELLED" &&
      !finished &&
      !(!recurring && j.status === "RUNNING"),
    resume: j.status === "PAUSED",
    cancel: j.status !== "CANCELLED" && !finished,
    retry: !recurring && j.status === "FAILED",
    run: recurring && j.status !== "CANCELLED" && j.status !== "PAUSED",
  };
}

function RowMenu({ job, onCancel }: { job: JobListItem; onCancel: (j: JobListItem) => void }) {
  const can = useCan();
  const mut = useJobAction();
  const a = rowActions(job);
  const w = can("write");
  const hint = (ok: boolean, why: string) => (!w ? VIEWER_REASON : ok ? undefined : why);
  return (
    <Dropdown>
      <DropdownTrigger asChild>
        <button
          className="rounded-md p-1 text-fg-subtle opacity-70 hover:bg-surface-3 hover:text-fg group-hover:opacity-100"
          aria-label={`Actions for ${job.name}`}
          onClick={(e) => e.stopPropagation()}
        >
          <MoreHorizontal className="h-4 w-4" />
        </button>
      </DropdownTrigger>
      <DropdownContent className="w-56">
        <DropdownItem
          icon={<Play />}
          disabled={!w || !a.run}
          hint={hint(a.run, "Run now is for active recurring jobs.")}
          onSelect={() => mut.mutate({ id: job.id, action: "run" })}
        >
          Run now
        </DropdownItem>
        <DropdownItem
          icon={<Pause />}
          disabled={!w || !a.pause}
          hint={hint(a.pause, `Not available while ${statusMeta(job.status).label.toLowerCase()}.`)}
          onSelect={() => mut.mutate({ id: job.id, action: "pause" })}
        >
          Pause
        </DropdownItem>
        <DropdownItem
          icon={<Play />}
          disabled={!w || !a.resume}
          hint={hint(a.resume, "The job is not paused.")}
          onSelect={() => mut.mutate({ id: job.id, action: "resume" })}
        >
          Resume
        </DropdownItem>
        <DropdownItem
          icon={<RotateCcw />}
          disabled={!w || !a.retry}
          hint={hint(a.retry, "Only failed one-time jobs can be retried.")}
          onSelect={() => mut.mutate({ id: job.id, action: "retry" })}
        >
          Retry
        </DropdownItem>
        <DropdownSeparator />
        <DropdownItem
          icon={<Ban />}
          danger
          disabled={!w || !a.cancel}
          hint={hint(a.cancel, "The job has already finished or was cancelled.")}
          onSelect={() => onCancel(job)}
        >
          Cancel…
        </DropdownItem>
      </DropdownContent>
    </Dropdown>
  );
}

function NextOrLast({ job }: { job: JobListItem }) {
  if (job.next_run_at && job.status !== "PAUSED" && job.status !== "CANCELLED") {
    return (
      <span className="text-fg-muted">
        Next <RelTime iso={job.next_run_at} timeZone={job.timezone} />
      </span>
    );
  }
  if (job.last_run_at && job.last_run_status) {
    return (
      <span className="inline-flex items-center gap-1.5 text-fg-muted">
        <StatusDot status={job.last_run_status} label="" />
        <RelTime iso={job.last_run_at} />
      </span>
    );
  }
  return <span className="text-fg-faint">—</span>;
}

export default function JobsPage() {
  usePageMeta([{ label: "Jobs" }]);
  const navigate = useNavigate();
  const can = useCan();
  const [f, setF] = useUrlFilters(DEFAULTS);
  const statuses = f.status ? f.status.split(",") : [];
  const handlersSel = f.handler ? f.handler.split(",") : [];
  const params = {
    q: f.q || undefined,
    status: statuses,
    handler: handlersSel,
    schedule_type: f.schedule_type || undefined,
    sort: f.sort,
    order: f.order as "asc" | "desc",
    page: Number(f.page),
    page_size: Number(f.page_size),
  };
  const q = useJobs(params);
  const handlers = useHandlers();
  const bulk = useBulkJobs();
  const cancelOne = useJobAction();
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [confirm, setConfirm] = useState<{ kind: "bulk" } | { kind: "one"; job: JobListItem } | null>(null);

  const rows = useMemo(() => q.data?.data ?? [], [q.data]);
  const allSelected = rows.length > 0 && rows.every((r) => selected.has(r.id));
  const filtered = !!(f.q || statuses.length || handlersSel.length || f.schedule_type);
  const sort = { key: f.sort, desc: f.order !== "asc" };
  const onSort = (key: string) =>
    setF({ sort: key, order: f.sort === key && f.order !== "asc" ? "asc" : "desc" });
  const toggle = (id: string) =>
    setSelected((s) => {
      const n = new Set(s);
      if (n.has(id)) n.delete(id);
      else n.add(id);
      return n;
    });
  const runBulk = async (action: string) => {
    await bulk.mutateAsync({ action, ids: [...selected] });
    setSelected(new Set());
  };

  return (
    <>
      <PageHeader
        title="Jobs"
        description="Every job definition in this workspace and its current state."
        actions={
          can("write") ? (
            <LinkButton to="/jobs/new" variant="primary" icon={<Plus className="h-3.5 w-3.5" />}>
              New job
            </LinkButton>
          ) : (
            <Button variant="primary" icon={<Plus className="h-3.5 w-3.5" />} disabledReason={VIEWER_REASON}>
              New job
            </Button>
          )
        }
      />
      <Panel>
        <FilterBar
          right={
            <span className="tabular text-xs text-fg-subtle">
              {q.data?.meta ? plural(q.data.meta.total, "job") : ""}
            </span>
          }
        >
          <SearchInput
            value={f.q}
            onChange={(v) => setF({ q: v })}
            placeholder="Search name, description or ID"
          />
          <MultiFilter
            label="Status"
            options={JOB_STATUSES}
            selected={statuses}
            onChange={(v) => setF({ status: v.join(",") })}
            render={(s) => <StatusDot status={s} />}
          />
          <MultiFilter
            label="Handler"
            options={(handlers.data ?? []).map((h) => h.name)}
            selected={handlersSel}
            onChange={(v) => setF({ handler: v.join(",") })}
            render={(h) => <code className="text-xs">{h}</code>}
          />
          <Select
            value={f.schedule_type}
            onChange={(e) => setF({ schedule_type: e.target.value })}
            aria-label="Schedule type"
            className="w-36"
          >
            <option value="">Any schedule</option>
            <option value="once">One-time</option>
            <option value="delayed">Delayed</option>
            <option value="cron">Recurring</option>
          </Select>
          {filtered && (
            <Button
              variant="ghost"
              size="sm"
              icon={<X className="h-3.5 w-3.5" />}
              onClick={() => setF({ q: "", status: "", handler: "", schedule_type: "" })}
            >
              Clear
            </Button>
          )}
        </FilterBar>

        {selected.size > 0 && (
          <div
            className="flex flex-wrap items-center gap-2 border-b bg-accent-soft/40 px-4 py-2"
            role="region"
            aria-label="Bulk actions"
          >
            <span className="tabular text-sm font-medium">{selected.size} selected</span>
            <div className="flex flex-wrap gap-1.5">
              <Button
                size="sm"
                icon={<Pause className="h-3.5 w-3.5" />}
                loading={bulk.isPending && bulk.variables?.action === "pause"}
                onClick={() => runBulk("pause")}
                disabledReason={can("write") ? undefined : VIEWER_REASON}
              >
                Pause
              </Button>
              <Button
                size="sm"
                icon={<Play className="h-3.5 w-3.5" />}
                loading={bulk.isPending && bulk.variables?.action === "resume"}
                onClick={() => runBulk("resume")}
                disabledReason={can("write") ? undefined : VIEWER_REASON}
              >
                Resume
              </Button>
              <Button
                size="sm"
                icon={<RotateCcw className="h-3.5 w-3.5" />}
                loading={bulk.isPending && bulk.variables?.action === "retry"}
                onClick={() => runBulk("retry")}
                disabledReason={can("write") ? undefined : VIEWER_REASON}
              >
                Retry
              </Button>
              <Button
                size="sm"
                variant="danger"
                icon={<Ban className="h-3.5 w-3.5" />}
                onClick={() => setConfirm({ kind: "bulk" })}
                disabledReason={can("write") ? undefined : VIEWER_REASON}
              >
                Cancel
              </Button>
            </div>
            <Button size="sm" variant="ghost" className="ml-auto" onClick={() => setSelected(new Set())}>
              Clear selection
            </Button>
          </div>
        )}

        {q.isLoading ? (
          <TableSkeleton rows={10} cols={6} />
        ) : q.error ? (
          <ErrorState error={q.error} onRetry={() => q.refetch()} />
        ) : rows.length === 0 ? (
          filtered ? (
            <EmptyState
              icon={<ListChecks />}
              title="No jobs match these filters"
              description="Try a different search or clear the filters."
              action={
                <Button onClick={() => setF({ q: "", status: "", handler: "", schedule_type: "" })}>
                  Clear filters
                </Button>
              }
            />
          ) : (
            <EmptyState
              icon={<ListChecks />}
              title="No jobs yet"
              description="Jobs run built-in handlers on a schedule: once, after a delay, or on a cron expression."
              action={
                can("write") && (
                  <LinkButton to="/jobs/new" variant="primary" icon={<Plus className="h-3.5 w-3.5" />}>
                    Create your first job
                  </LinkButton>
                )
              }
            />
          )
        ) : (
          <>
            {/* Phones: compact cards instead of a wide table. */}
            <ul className="divide-y sm:hidden">
              {rows.map((j) => (
                <li key={j.id} className="flex items-start gap-3 px-4 py-3">
                  <Checkbox
                    checked={selected.has(j.id)}
                    onChange={() => toggle(j.id)}
                    aria-label={`Select ${j.name}`}
                    className="mt-1"
                  />
                  <Link to={`/jobs/${j.id}`} className="min-w-0 flex-1">
                    <div className="flex items-start justify-between gap-2">
                      <span className="truncate font-medium">{j.name}</span>
                      <StatusBadge status={j.status} withTooltip={false} />
                    </div>
                    <div className="mt-0.5 truncate text-xs text-fg-subtle">{scheduleSummary(j)}</div>
                    <div className="mt-1 flex flex-wrap gap-x-3 text-xs">
                      <NextOrLast job={j} />
                    </div>
                  </Link>
                  <RowMenu job={j} onCancel={(job) => setConfirm({ kind: "one", job })} />
                </li>
              ))}
            </ul>
            <div className="hidden sm:block">
              <Table minWidth={900}>
                <THead>
                  <tr>
                    <TH className="w-10">
                      <Checkbox
                        checked={allSelected}
                        onChange={() => setSelected(allSelected ? new Set() : new Set(rows.map((r) => r.id)))}
                        aria-label="Select all on this page"
                      />
                    </TH>
                    <TH sortKey="name" sort={sort} onSort={onSort}>
                      Job
                    </TH>
                    <TH sortKey="status" sort={sort} onSort={onSort}>
                      Status
                    </TH>
                    <TH sortKey="priority" sort={sort} onSort={onSort}>
                      Priority
                    </TH>
                    <TH>Schedule</TH>
                    <TH sortKey="next_run_at" sort={sort} onSort={onSort}>
                      Next / last run
                    </TH>
                    <TH align="right">Runs ok / failed</TH>
                    <TH className="w-10">
                      <span className="sr-only">Actions</span>
                    </TH>
                  </tr>
                </THead>
                <tbody>
                  {rows.map((j) => (
                    <TR key={j.id} onClick={() => navigate(`/jobs/${j.id}`)} selected={selected.has(j.id)}>
                      <TD onClick={(e) => e.stopPropagation()}>
                        <Checkbox
                          checked={selected.has(j.id)}
                          onChange={() => toggle(j.id)}
                          aria-label={`Select ${j.name}`}
                        />
                      </TD>
                      <TD className="max-w-[320px]">
                        <Link
                          to={`/jobs/${j.id}`}
                          onClick={(e) => e.stopPropagation()}
                          className="block truncate font-medium hover:underline"
                          title={j.name}
                        >
                          {j.name}
                        </Link>
                        <span className="block truncate font-mono text-2xs text-fg-subtle">{j.handler}</span>
                      </TD>
                      <TD>
                        <StatusBadge status={j.status} />
                      </TD>
                      <TD>
                        <span
                          className={cn(
                            "tabular text-xs",
                            j.priority >= 7 ? "font-medium text-fg" : "text-fg-muted",
                          )}
                        >
                          {j.priority} · {priorityLabel(j.priority)}
                        </span>
                      </TD>
                      <TD className="max-w-[240px]">
                        <span
                          className="flex items-center gap-1.5 truncate text-xs text-fg-muted"
                          title={scheduleSummary(j)}
                        >
                          {j.schedule_type === "cron" && <CalendarClock className="h-3.5 w-3.5 shrink-0" />}
                          <span className="truncate">{scheduleSummary(j)}</span>
                        </span>
                      </TD>
                      <TD className="text-xs">
                        <NextOrLast job={j} />
                      </TD>
                      <TD align="right" className="tabular whitespace-nowrap text-xs">
                        <span className="text-success">{j.succeeded_runs}</span>
                        <span className="text-fg-faint"> / </span>
                        <span className={j.failed_runs ? "text-danger" : "text-fg-subtle"}>
                          {j.failed_runs}
                        </span>
                        {ACTIVE.has(j.status) && j.active_runs > 1 && (
                          <span className="ml-1 text-info">+{j.active_runs} active</span>
                        )}
                      </TD>
                      <TD onClick={(e) => e.stopPropagation()}>
                        <RowMenu job={j} onCancel={(job) => setConfirm({ kind: "one", job })} />
                      </TD>
                    </TR>
                  ))}
                </tbody>
              </Table>
            </div>
            <Pagination
              meta={q.data?.meta}
              onPage={(p) => setF({ page: String(p) }, false)}
              onPageSize={(n) => setF({ page_size: String(n) })}
            />
          </>
        )}
      </Panel>

      <ConfirmDialog
        open={!!confirm}
        onOpenChange={(o) => !o && setConfirm(null)}
        tone="danger"
        title={confirm?.kind === "one" ? `Cancel “${confirm.job.name}”?` : `Cancel ${selected.size} job(s)?`}
        confirmLabel="Cancel jobs"
        description="No further runs will be scheduled. Waiting runs are cancelled; executing runs are asked to stop. This cannot be undone."
        onConfirm={async () => {
          if (confirm?.kind === "one") await cancelOne.mutateAsync({ id: confirm.job.id, action: "cancel" });
          else await runBulk("cancel");
        }}
      />
    </>
  );
}
