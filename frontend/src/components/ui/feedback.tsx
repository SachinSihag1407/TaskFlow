import type { ReactNode } from "react";
import { AlertTriangle, RefreshCw } from "lucide-react";
import { cn } from "@/lib/utils";
import { ApiError, errorMessage } from "@/lib/api";
import { Button } from "./button";

export function Skeleton({ className }: { className?: string }) {
  return <div className={cn("animate-pulse rounded bg-surface-3/70", className)} aria-hidden />;
}

/** Placeholder rows shaped like the table that will replace them. */
export function TableSkeleton({ rows = 8, cols = 5 }: { rows?: number; cols?: number }) {
  return (
    <div className="divide-y" role="status" aria-label="Loading">
      {Array.from({ length: rows }).map((_, r) => (
        <div key={r} className="flex h-11 items-center gap-4 px-4">
          {Array.from({ length: cols }).map((_, c) => (
            <Skeleton key={c} className={cn("h-3", c === 0 ? "w-1/4" : "w-1/6")} />
          ))}
        </div>
      ))}
    </div>
  );
}

interface EmptyStateProps {
  icon?: ReactNode;
  title: string;
  description?: ReactNode;
  action?: ReactNode;
  className?: string;
}

export function EmptyState({ icon, title, description, action, className }: EmptyStateProps) {
  return (
    <div className={cn("flex flex-col items-center justify-center px-6 py-14 text-center", className)}>
      {icon && (
        <div className="mb-3 flex h-10 w-10 items-center justify-center rounded-lg bg-surface-2 text-fg-subtle ring-1 ring-line [&>svg]:h-5 [&>svg]:w-5">
          {icon}
        </div>
      )}
      <h3 className="text-sm font-medium text-fg">{title}</h3>
      {description && <p className="mt-1 max-w-sm text-sm text-fg-subtle">{description}</p>}
      {action && <div className="mt-4 flex flex-wrap justify-center gap-2">{action}</div>}
    </div>
  );
}

/** Error panel with the server's message, request id and a retry action. */
export function ErrorState({
  error,
  onRetry,
  className,
}: {
  error: unknown;
  onRetry?: () => void;
  className?: string;
}) {
  const requestId = error instanceof ApiError ? error.requestId : undefined;
  const network = error instanceof ApiError && error.isNetwork;
  return (
    <div
      className={cn("flex flex-col items-center justify-center px-6 py-12 text-center", className)}
      role="alert"
    >
      <div className="mb-3 flex h-10 w-10 items-center justify-center rounded-lg bg-danger-soft text-danger">
        <AlertTriangle className="h-5 w-5" />
      </div>
      <h3 className="text-sm font-medium">{network ? "Can't reach TaskFlow" : "Couldn't load this data"}</h3>
      <p className="mt-1 max-w-md text-sm text-fg-subtle">{errorMessage(error)}</p>
      {requestId && <p className="mt-1 font-mono text-2xs text-fg-faint">Request ID {requestId}</p>}
      {onRetry && (
        <Button className="mt-4" size="sm" icon={<RefreshCw className="h-3.5 w-3.5" />} onClick={onRetry}>
          Try again
        </Button>
      )}
    </div>
  );
}

export function Spinner({ className }: { className?: string }) {
  return (
    <span
      className={cn(
        "inline-block h-4 w-4 animate-spin rounded-full border-2 border-line-strong border-t-accent",
        className,
      )}
      role="status"
      aria-label="Loading"
    />
  );
}
