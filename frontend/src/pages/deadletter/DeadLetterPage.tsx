import { useState } from "react";
import { Link } from "react-router-dom";
import { Inbox, RotateCcw, ShieldCheck } from "lucide-react";
import { usePageMeta } from "@/app/page-meta";
import { useCan, VIEWER_REASON } from "@/app/session";
import { useBulkReplay, useDeadLetter, useRun, useRunAction } from "@/api/hooks";
import type { Run } from "@/lib/types";
import { formatDateTime, plural, shortId } from "@/lib/format";
import { useUrlFilters } from "@/lib/url-state";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/input";
import { PageHeader, Panel } from "@/components/ui/layout";
import { Table, TD, TH, THead, TR } from "@/components/ui/table";
import { Pagination } from "@/components/ui/pagination";
import { EmptyState, ErrorState, Skeleton, TableSkeleton } from "@/components/ui/feedback";
import { ConfirmDialog, Sheet } from "@/components/ui/dialog";
import { JsonBlock } from "@/components/ui/code";
import { FilterBar, SearchInput } from "@/components/common/Filters";
import { RelTime } from "@/components/common/Time";
import { ExecutionTimeline } from "@/components/runs/ExecutionTimeline";

const DEFAULTS = { q: "", page: "1", page_size: "25" };

function RunSheet({
  runId,
  onClose,
  onReplay,
}: {
  runId: string | null;
  onClose: () => void;
  onReplay: (r: Run) => void;
}) {
  const q = useRun(runId ?? undefined);
  const can = useCan();
  const r = q.data?.run;
  return (
    <Sheet
      open={!!runId}
      onOpenChange={(o) => !o && onClose()}
      title={r ? `${r.job_name} · run ${shortId(r.id)}` : "Dead-lettered run"}
    >
      {q.isLoading ? (
        <div className="space-y-3 p-5">
          <Skeleton className="h-24 w-full" />
          <Skeleton className="h-48 w-full" />
        </div>
      ) : q.error ? (
        <ErrorState error={q.error} onRetry={() => q.refetch()} />
      ) : r ? (
        <div className="space-y-6 p-5">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <Link to={`/jobs/${r.job_id}`} onClick={onClose} className="text-sm text-accent hover:underline">
              Open job
            </Link>
            <Button
              variant="primary"
              size="sm"
              icon={<RotateCcw className="h-3.5 w-3.5" />}
              disabledReason={
                !can("write")
                  ? VIEWER_REASON
                  : r.replayed_at
                    ? `Already replayed ${formatDateTime(r.replayed_at)}.`
                    : undefined
              }
              onClick={() => onReplay(r)}
            >
              Replay
            </Button>
          </div>
          {r.last_error && (
            <section>
              <h3 className="mb-2 text-sm font-medium">Failure reason</h3>
              <pre className="whitespace-pre-wrap break-words rounded-md border border-danger/20 bg-danger-soft/50 p-3 font-mono text-xs">
                {r.last_error}
              </pre>
            </section>
          )}
          <section>
            <h3 className="mb-2 text-sm font-medium">Retry history</h3>
            <ExecutionTimeline
              attempts={q.data!.attempts.map((a) => ({ ...a, max_attempts: r.max_attempts }))}
            />
          </section>
          <section>
            <h3 className="mb-2 text-sm font-medium">Replay audit trail</h3>
            {q.data!.audit.length === 0 ? (
              <p className="flex items-center gap-2 text-sm text-fg-subtle">
                <ShieldCheck className="h-4 w-4" />
                Not replayed yet.
              </p>
            ) : (
              <ul className="space-y-2">
                {q.data!.audit.map((a) => (
                  <li key={a.id} className="rounded-md border px-3 py-2 text-sm">
                    <span className="font-medium">{a.actor_email ?? "system"}</span>{" "}
                    <span className="text-fg-subtle">{a.action}</span>
                    <div className="text-2xs text-fg-subtle">
                      {formatDateTime(a.created_at)}
                      {a.ip ? ` · ${a.ip}` : ""}
                    </div>
                  </li>
                ))}
              </ul>
            )}
          </section>
          <section>
            <h3 className="mb-2 text-sm font-medium">Payload snapshot</h3>
            <JsonBlock value={r.payload} maxHeight={220} />
          </section>
        </div>
      ) : null}
    </Sheet>
  );
}

export default function DeadLetterPage() {
  usePageMeta([{ label: "Dead-letter queue" }]);
  const can = useCan();
  const [f, setF] = useUrlFilters(DEFAULTS);
  const q = useDeadLetter({ q: f.q || undefined, page: Number(f.page), page_size: Number(f.page_size) });
  const replayOne = useRunAction();
  const replayMany = useBulkReplay();
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [open, setOpen] = useState<string | null>(null);
  const [confirm, setConfirm] = useState<{ ids: string[]; label: string } | null>(null);
  const rows = q.data?.data ?? [];
  const eligible = rows.filter((r) => !r.replayed_at);
  const allSelected = eligible.length > 0 && eligible.every((r) => selected.has(r.id));
  const toggle = (id: string) =>
    setSelected((s) => {
      const n = new Set(s);
      if (n.has(id)) n.delete(id);
      else n.add(id);
      return n;
    });

  return (
    <>
      <PageHeader
        title="Dead-letter queue"
        description="Runs that exhausted their retry policy or failed permanently. Replaying queues a new run with the same payload."
        actions={
          selected.size > 0 && (
            <Button
              variant="primary"
              icon={<RotateCcw className="h-3.5 w-3.5" />}
              disabledReason={can("write") ? undefined : VIEWER_REASON}
              onClick={() => setConfirm({ ids: [...selected], label: `${selected.size} runs` })}
            >
              Replay {selected.size} selected
            </Button>
          )
        }
      />
      <Panel>
        <FilterBar
          right={
            <span className="tabular text-xs text-fg-subtle">
              {q.data?.meta ? plural(q.data.meta.total, "run") : ""}
            </span>
          }
        >
          <SearchInput value={f.q} onChange={(v) => setF({ q: v })} placeholder="Search job name or error" />
        </FilterBar>
        {q.isLoading ? (
          <TableSkeleton rows={6} cols={6} />
        ) : q.error ? (
          <ErrorState error={q.error} onRetry={() => q.refetch()} />
        ) : rows.length === 0 ? (
          <EmptyState
            icon={<Inbox />}
            title={f.q ? "No dead-lettered runs match" : "The dead-letter queue is empty"}
            description={
              f.q ? undefined : "Runs land here after their final failed attempt. Nothing needs attention."
            }
          />
        ) : (
          <>
            <Table minWidth={900}>
              <THead>
                <tr>
                  <TH className="w-10">
                    <Checkbox
                      checked={allSelected}
                      disabled={!eligible.length}
                      onChange={() =>
                        setSelected(allSelected ? new Set() : new Set(eligible.map((r) => r.id)))
                      }
                      aria-label="Select all replayable runs"
                    />
                  </TH>
                  <TH>Job</TH>
                  <TH>Failure reason</TH>
                  <TH>Attempts</TH>
                  <TH>Dead-lettered</TH>
                  <TH>Replay</TH>
                  <TH align="right">
                    <span className="sr-only">Actions</span>
                  </TH>
                </tr>
              </THead>
              <tbody>
                {rows.map((r) => (
                  <TR key={r.id} onClick={() => setOpen(r.id)} selected={selected.has(r.id)}>
                    <TD onClick={(e) => e.stopPropagation()}>
                      <Checkbox
                        checked={selected.has(r.id)}
                        disabled={!!r.replayed_at}
                        onChange={() => toggle(r.id)}
                        aria-label={`Select run ${shortId(r.id)}`}
                      />
                    </TD>
                    <TD className="max-w-[220px]">
                      <span className="block truncate font-medium">{r.job_name}</span>
                      <span className="block truncate font-mono text-2xs text-fg-subtle">
                        {r.handler} · {shortId(r.id)}
                      </span>
                    </TD>
                    <TD className="max-w-[320px]">
                      <span
                        className="block truncate font-mono text-xs text-danger"
                        title={r.last_error ?? ""}
                      >
                        {r.last_error ?? "—"}
                      </span>
                    </TD>
                    <TD className="tabular text-fg-muted">
                      {r.attempt} / {r.max_attempts}
                    </TD>
                    <TD className="text-xs text-fg-muted">
                      <RelTime iso={r.dead_lettered_at} />
                    </TD>
                    <TD className="text-xs">
                      {r.replayed_at ? (
                        <span className="text-fg-subtle">
                          Replayed <RelTime iso={r.replayed_at} />
                        </span>
                      ) : (
                        <span className="text-warning">Pending</span>
                      )}
                    </TD>
                    <TD align="right" onClick={(e) => e.stopPropagation()}>
                      <Button
                        size="sm"
                        variant="ghost"
                        icon={<RotateCcw className="h-3.5 w-3.5" />}
                        disabledReason={
                          !can("write") ? VIEWER_REASON : r.replayed_at ? "Already replayed." : undefined
                        }
                        onClick={() =>
                          setConfirm({ ids: [r.id], label: `run ${shortId(r.id)} of “${r.job_name}”` })
                        }
                      >
                        Replay
                      </Button>
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
      <RunSheet
        runId={open}
        onClose={() => setOpen(null)}
        onReplay={(r) => setConfirm({ ids: [r.id], label: `run ${shortId(r.id)} of “${r.job_name}”` })}
      />
      <ConfirmDialog
        open={!!confirm}
        onOpenChange={(o) => !o && setConfirm(null)}
        title={`Replay ${confirm?.label}?`}
        confirmLabel="Replay"
        description="Each replay queues a new run with the original payload and a fresh attempt budget. Every failed run can be replayed once, and the action is recorded in the audit log. If the handler has external side effects, make sure replaying is safe."
        onConfirm={async () => {
          if (!confirm) return;
          if (confirm.ids.length === 1) await replayOne.mutateAsync({ id: confirm.ids[0], action: "replay" });
          else await replayMany.mutateAsync(confirm.ids);
          setSelected(new Set());
        }}
      />
    </>
  );
}
