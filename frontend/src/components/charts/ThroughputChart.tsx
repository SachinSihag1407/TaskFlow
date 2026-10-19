import { useEffect, useMemo, useState } from "react";
import { Bar, BarChart, CartesianGrid, ResponsiveContainer, Tooltip, XAxis, YAxis } from "recharts";
import { Table2, BarChart3 } from "lucide-react";
import type { ThroughputPoint } from "@/lib/types";
import { formatNumber } from "@/lib/format";
import { useTheme } from "@/app/theme";
import { IconButton } from "@/components/ui/button";

/** Reads chart colours from CSS tokens so they follow the active theme. */
function useChartColors() {
  const { resolved } = useTheme();
  const [c, setC] = useState({
    success: "#56ba8a",
    danger: "#c62a2f",
    grid: "#e4e4e8",
    text: "#71717a",
    surface: "#fff",
  });
  useEffect(() => {
    const s = getComputedStyle(document.documentElement);
    const v = (n: string) => `rgb(${s.getPropertyValue(n).trim().split(/\s+/).join(",")})`;
    setC({
      success: v("--chart-success"),
      danger: v("--chart-danger"),
      grid: v("--chart-grid"),
      text: v("--fg-subtle"),
      surface: v("--surface"),
    });
  }, [resolved]);
  return c;
}

const compact = new Intl.NumberFormat(undefined, { notation: "compact", maximumFractionDigits: 1 });

function bucketLabel(iso: string, range: string) {
  const d = new Date(iso);
  if (range === "7d" || range === "30d") {
    return new Intl.DateTimeFormat(
      undefined,
      range === "30d" ? { month: "short", day: "numeric" } : { weekday: "short", hour: "2-digit" },
    ).format(d);
  }
  return new Intl.DateTimeFormat(undefined, { hour: "2-digit", minute: "2-digit" }).format(d);
}

interface TooltipEntry {
  dataKey?: unknown;
  color?: string;
  payload: ThroughputPoint;
}

function ChartTooltip({
  active,
  payload,
  range,
}: {
  active?: boolean;
  payload?: readonly TooltipEntry[];
  range: string;
}) {
  if (!active || !payload?.length) return null;
  const p = payload[0].payload as ThroughputPoint;
  const total = p.succeeded + p.failed;
  return (
    <div className="rounded-md bg-surface px-3 py-2 text-xs shadow-pop">
      <div className="mb-1.5 font-medium text-fg">
        {new Intl.DateTimeFormat(undefined, {
          dateStyle: range === "30d" ? "medium" : undefined,
          timeStyle: "short",
          month: "short",
          day: "numeric",
        } as Intl.DateTimeFormatOptions).format(new Date(p.bucket))}
      </div>
      <div className="space-y-1 tabular">
        <Row
          color={payload.find((x) => x.dataKey === "succeeded")?.color}
          label="Succeeded"
          value={p.succeeded}
        />
        <Row color={payload.find((x) => x.dataKey === "failed")?.color} label="Failed" value={p.failed} />
        <div className="flex justify-between gap-6 border-t pt-1 text-fg-muted">
          <span>Total</span>
          <span>{formatNumber(total)}</span>
        </div>
      </div>
    </div>
  );
}

function Row({ color, label, value }: { color?: string; label: string; value: number }) {
  return (
    <div className="flex items-center justify-between gap-6">
      <span className="flex items-center gap-1.5 text-fg-muted">
        <span className="h-2 w-2 rounded-sm" style={{ background: color }} />
        {label}
      </span>
      <span className="text-fg">{formatNumber(value)}</span>
    </div>
  );
}

/**
 * Finished attempts per time bucket, stacked by outcome. Identity is never
 * colour-only: the legend names each series with its total, the tooltip
 * labels values, and a table view is one click away.
 */
export function ThroughputChart({ data, range }: { data: ThroughputPoint[]; range: string }) {
  const colors = useChartColors();
  const [asTable, setAsTable] = useState(false);
  const totals = useMemo(
    () => data.reduce((a, p) => ({ s: a.s + p.succeeded, f: a.f + p.failed }), { s: 0, f: 0 }),
    [data],
  );
  return (
    <div>
      <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
        <div className="flex items-center gap-4 text-xs text-fg-muted" aria-label="Legend">
          <span className="flex items-center gap-1.5">
            <span className="h-2.5 w-2.5 rounded-sm" style={{ background: colors.success }} />
            Succeeded <span className="tabular font-medium text-fg">{formatNumber(totals.s)}</span>
          </span>
          <span className="flex items-center gap-1.5">
            <span className="h-2.5 w-2.5 rounded-sm" style={{ background: colors.danger }} />
            Failed <span className="tabular font-medium text-fg">{formatNumber(totals.f)}</span>
          </span>
        </div>
        <IconButton
          label={asTable ? "Show chart" : "Show as table"}
          size="icon-sm"
          onClick={() => setAsTable((t) => !t)}
        >
          {asTable ? <BarChart3 className="h-3.5 w-3.5" /> : <Table2 className="h-3.5 w-3.5" />}
        </IconButton>
      </div>
      {asTable ? (
        <div className="scrollbar-thin max-h-[240px] overflow-auto rounded-md border">
          <table className="w-full text-xs">
            <thead className="sticky top-0 bg-surface-2">
              <tr className="text-left text-fg-subtle">
                <th className="px-3 py-1.5 font-medium">Bucket</th>
                <th className="px-3 py-1.5 text-right font-medium">Succeeded</th>
                <th className="px-3 py-1.5 text-right font-medium">Failed</th>
              </tr>
            </thead>
            <tbody className="tabular">
              {data
                .filter((p) => p.succeeded + p.failed > 0)
                .map((p) => (
                  <tr key={p.bucket} className="border-t">
                    <td className="px-3 py-1.5">{new Date(p.bucket).toLocaleString()}</td>
                    <td className="px-3 py-1.5 text-right">{formatNumber(p.succeeded)}</td>
                    <td className="px-3 py-1.5 text-right">{formatNumber(p.failed)}</td>
                  </tr>
                ))}
            </tbody>
          </table>
        </div>
      ) : (
        <div
          className="h-[240px]"
          role="img"
          aria-label={`Throughput: ${totals.s} succeeded and ${totals.f} failed attempts in this range`}
        >
          <ResponsiveContainer width="100%" height="100%">
            <BarChart data={data} margin={{ top: 4, right: 4, bottom: 0, left: 0 }} barCategoryGap="18%">
              <CartesianGrid vertical={false} stroke={colors.grid} />
              <XAxis
                dataKey="bucket"
                tickFormatter={(v: string) => bucketLabel(v, range)}
                tick={{ fill: colors.text, fontSize: 11 }}
                tickLine={false}
                axisLine={{ stroke: colors.grid }}
                minTickGap={24}
              />
              <YAxis
                allowDecimals={false}
                tickFormatter={(v: number) => compact.format(v)}
                tick={{ fill: colors.text, fontSize: 11 }}
                tickLine={false}
                axisLine={false}
                width={36}
              />
              <Tooltip
                content={(p) => (
                  <ChartTooltip
                    active={p.active}
                    payload={p.payload as unknown as TooltipEntry[]}
                    range={range}
                  />
                )}
                cursor={{ fill: colors.grid, opacity: 0.5 }}
              />
              {/* A thin surface-coloured stroke separates stacked segments. */}
              <Bar
                dataKey="succeeded"
                stackId="a"
                fill={colors.success}
                stroke={colors.surface}
                strokeWidth={1}
                isAnimationActive={false}
              />
              <Bar
                dataKey="failed"
                stackId="a"
                fill={colors.danger}
                stroke={colors.surface}
                strokeWidth={1}
                radius={[3, 3, 0, 0]}
                isAnimationActive={false}
              />
            </BarChart>
          </ResponsiveContainer>
        </div>
      )}
    </div>
  );
}
