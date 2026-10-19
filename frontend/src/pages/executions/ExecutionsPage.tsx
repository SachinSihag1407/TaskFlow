import { useMemo } from "react";
import { Link } from "react-router-dom";
import { Activity, X } from "lucide-react";
import { usePageMeta } from "@/app/page-meta";
import { useExecutions, useHandlers } from "@/api/hooks";
import { ATTEMPT_STATUSES } from "@/lib/status";
import { formatDuration, plural, shortId } from "@/lib/format";
import { useUrlFilters } from "@/lib/url-state";
import { PageHeader, Panel } from "@/components/ui/layout";
import { Select } from "@/components/ui/input";
import { Button } from "@/components/ui/button";
import { StatusBadge, StatusDot } from "@/components/ui/badge";
import { Table, TD, TH, THead, TR } from "@/components/ui/table";
import { Pagination } from "@/components/ui/pagination";
import { EmptyState, ErrorState, TableSkeleton } from "@/components/ui/feedback";
import { FilterBar, MultiFilter, SearchInput } from "@/components/common/Filters";
import { RelTime } from "@/components/common/Time";
import { ExecutionSheet } from "@/components/runs/ExecutionSheet";

const DEFAULTS = {
  q: "",
  status: "",
  handler: "",
  window: "",
  sort: "started_at",
  order: "desc",
  page: "1",
  page_size: "25",
  attempt: "",
};

const WINDOWS: Record<string, number> = { "1h": 3600e3, "24h": 86400e3, "7d": 7 * 86400e3 };

export default function ExecutionsPage() {
  usePageMeta([{ label: "Executions" }]);
  const [f, setF] = useUrlFilters(DEFAULTS);
  const handlers = useHandlers();
  const statuses = f.status ? f.status.split(",") : [];
  const hs = f.handler ? f.handler.split(",") : [];
  // Freeze the window start when the selection changes so pagination is stable.
  const since = useMemo(
    () => (f.window ? new Date(Date.now() - WINDOWS[f.window]).toISOString() : undefined),
    [f.window],
  );
  const q = useExecutions({
    q: f.q || undefined,
    status: statuses,
    handler: hs,
    since,
    sort: f.sort,
    order: f.order as "asc" | "desc",
    page: Number(f.page),
    page_size: Number(f.page_size),
  });
  const rows = q.data?.data ?? [];
  const filtered = !!(f.q || statuses.length || hs.length || f.window);
  const sort = { key: f.sort, desc: f.order !== "asc" };
  const onSort = (key: string) =>
    setF({ sort: key, order: f.sort === key && f.order !== "asc" ? "asc" : "desc" });
  const clear = () => setF({ q: "", status: "", handler: "", window: "" });

  return (
    <>
      <PageHeader
        title="Executions"
        description="Every execution attempt, including retries, with errors and logs."
      />
      <Panel>
        <FilterBar
          right={
            <span className="tabular text-xs text-fg-subtle">
              {q.data?.meta ? plural(q.data.meta.total, "attempt") : ""}
            </span>
          }
        >
          <SearchInput value={f.q} onChange={(v) => setF({ q: v })} placeholder="Search job name or error" />
          <MultiFilter
            label="Status"
            options={ATTEMPT_STATUSES}
            selected={statuses}
            onChange={(v) => setF({ status: v.join(",") })}
            render={(s) => <StatusDot status={s} />}
          />
          <MultiFilter
            label="Handler"
            options={(handlers.data ?? []).map((h) => h.name)}
            selected={hs}
            onChange={(v) => setF({ handler: v.join(",") })}
            render={(h) => <code className="text-xs">{h}</code>}
          />
          <Select
            value={f.window}
            onChange={(e) => setF({ window: e.target.value })}
            aria-label="Time window"
            className="w-36"
          >
            <option value="">All time</option>
            <option value="1h">Last hour</option>
            <option value="24h">Last 24 hours</option>
            <option value="7d">Last 7 days</option>
          </Select>
          {filtered && (
            <Button variant="ghost" size="sm" icon={<X className="h-3.5 w-3.5" />} onClick={clear}>
              Clear
            </Button>
          )}
        </FilterBar>
        {q.isLoading ? (
          <TableSkeleton rows={10} cols={7} />
        ) : q.error ? (
          <ErrorState error={q.error} onRetry={() => q.refetch()} />
        ) : rows.length === 0 ? (
          <EmptyState
            icon={<Activity />}
            title={filtered ? "No executions match these filters" : "No executions yet"}
            description={
              filtered
                ? "Try widening the time window or clearing filters."
                : "Attempts appear here as soon as workers start running jobs."
            }
            action={filtered && <Button onClick={clear}>Clear filters</Button>}
          />
        ) : (
          <>
            <Table minWidth={960}>
              <THead>
                <tr>
                  <TH>Job</TH>
                  <TH>Status</TH>
                  <TH>Attempt</TH>
                  <TH>Error</TH>
                  <TH>Worker</TH>
                  <TH align="right" sortKey="duration_ms" sort={sort} onSort={onSort}>
                    Duration
                  </TH>
                  <TH align="right" sortKey="started_at" sort={sort} onSort={onSort}>
                    Started
                  </TH>
                </tr>
              </THead>
              <tbody>
                {rows.map((a) => (
                  <TR key={a.id} onClick={() => setF({ attempt: a.id }, false)} selected={f.attempt === a.id}>
                    <TD className="max-w-[240px]">
                      <Link
                        to={`/jobs/${a.job_id}`}
                        onClick={(e) => e.stopPropagation()}
                        className="block truncate font-medium hover:underline"
                      >
                        {a.job_name}
                      </Link>
                      <span className="block truncate font-mono text-2xs text-fg-subtle">{a.handler}</span>
                    </TD>
                    <TD>
                      <StatusBadge status={a.status} />
                    </TD>
                    <TD className="tabular text-fg-muted">
                      {a.attempt_number}
                      {a.max_attempts ? ` / ${a.max_attempts}` : ""}
                    </TD>
                    <TD className="max-w-[280px]">
                      {a.error_message ? (
                        <span
                          className="block truncate font-mono text-xs text-danger"
                          title={a.error_message}
                        >
                          {a.error_message}
                        </span>
                      ) : (
                        <span className="text-fg-faint">—</span>
                      )}
                    </TD>
                    <TD className="max-w-[140px] truncate text-xs text-fg-muted">
                      {a.worker_id ? (
                        <Link
                          to={`/workers/${a.worker_id}`}
                          onClick={(e) => e.stopPropagation()}
                          className="hover:underline"
                        >
                          {a.worker_hostname || shortId(a.worker_id)}
                        </Link>
                      ) : (
                        "—"
                      )}
                    </TD>
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
            <Pagination
              meta={q.data?.meta}
              onPage={(p) => setF({ page: String(p) }, false)}
              onPageSize={(n) => setF({ page_size: String(n) })}
            />
          </>
        )}
      </Panel>
      <ExecutionSheet attemptId={f.attempt || null} onClose={() => setF({ attempt: "" }, false)} />
    </>
  );
}
