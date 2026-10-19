import type { ReactNode } from "react";
import { Link } from "react-router-dom";
import { cn } from "@/lib/utils";
import { Skeleton } from "./feedback";

interface MetricProps {
  label: string;
  value: ReactNode;
  hint?: ReactNode;
  tone?: "default" | "success" | "danger" | "warning" | "info";
  to?: string;
  loading?: boolean;
}

const toneText = {
  default: "text-fg",
  success: "text-success",
  danger: "text-danger",
  warning: "text-warning",
  info: "text-info",
};

/**
 * A compact metric: label, value, context line. Metrics sit in a shared
 * bordered strip rather than separate colourful cards.
 */
export function Metric({ label, value, hint, tone = "default", to, loading }: MetricProps) {
  const body = (
    <>
      <div className="text-xs text-fg-subtle">{label}</div>
      {loading ? (
        <Skeleton className="mt-2 h-6 w-16" />
      ) : (
        <div className={cn("tabular mt-1 text-2xl font-semibold tracking-tight", toneText[tone])}>
          {value}
        </div>
      )}
      {hint && <div className="mt-0.5 truncate text-xs text-fg-subtle">{hint}</div>}
    </>
  );
  const cls = "block min-w-0 px-4 py-3";
  return to ? (
    <Link to={to} className={cn(cls, "transition-colors hover:bg-surface-2/60")}>
      {body}
    </Link>
  ) : (
    <div className={cls}>{body}</div>
  );
}

export function MetricStrip({ children, className }: { children: ReactNode; className?: string }) {
  return (
    <div
      className={cn(
        "grid grid-cols-2 divide-line overflow-hidden rounded-lg border bg-surface shadow-xs sm:grid-cols-3 lg:grid-cols-6",
        "[&>*]:border-b [&>*]:border-r [&>*]:border-line",
        className,
      )}
    >
      {children}
    </div>
  );
}
