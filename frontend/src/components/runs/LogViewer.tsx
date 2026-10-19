import { useEffect, useMemo, useRef, useState } from "react";
import { WrapText } from "lucide-react";
import type { LogLine } from "@/lib/types";
import { formatLogTime } from "@/lib/format";
import { cn } from "@/lib/utils";
import { CopyButton } from "@/components/ui/code";
import { IconButton } from "@/components/ui/button";

const levelCls: Record<string, string> = {
  debug: "text-fg-faint",
  info: "text-info",
  warn: "text-warning",
  error: "text-danger",
};

/**
 * Attempt logs as persisted by the worker: timestamps, severity, wrap or
 * horizontal scroll, copy. Follows the tail while the attempt is running.
 */
export function LogViewer({
  lines,
  running,
  emptyText = "No log output was recorded for this attempt.",
}: {
  lines: LogLine[];
  running?: boolean;
  emptyText?: string;
}) {
  const [wrap, setWrap] = useState(true);
  const [filter, setFilter] = useState<"all" | "warn" | "error">("all");
  const ref = useRef<HTMLDivElement>(null);
  const shown = useMemo(
    () =>
      lines.filter(
        (l) =>
          filter === "all" ||
          (filter === "warn" ? l.level === "warn" || l.level === "error" : l.level === "error"),
      ),
    [lines, filter],
  );
  const text = useMemo(
    () => lines.map((l) => `${l.ts} ${l.level.toUpperCase().padEnd(5)} ${l.message}`).join("\n"),
    [lines],
  );

  useEffect(() => {
    if (running && ref.current) ref.current.scrollTop = ref.current.scrollHeight;
  }, [running, lines.length]);

  return (
    <div className="overflow-hidden rounded-md border bg-surface-2/40">
      <div className="flex flex-wrap items-center justify-between gap-2 border-b px-2 py-1.5">
        <div className="flex items-center gap-1 text-2xs" role="radiogroup" aria-label="Filter log level">
          {(["all", "warn", "error"] as const).map((f) => (
            <button
              key={f}
              role="radio"
              aria-checked={filter === f}
              onClick={() => setFilter(f)}
              className={cn(
                "rounded px-2 py-0.5 capitalize",
                filter === f ? "bg-surface-3 text-fg" : "text-fg-subtle hover:text-fg",
              )}
            >
              {f === "warn" ? "Warnings+" : f === "error" ? "Errors" : "All"}
            </button>
          ))}
          <span className="tabular ml-2 text-fg-faint">{shown.length} lines</span>
          {running && (
            <span className="ml-2 inline-flex items-center gap-1 text-info">
              <span className="h-1.5 w-1.5 animate-pulse-dot rounded-full bg-info-solid" />
              following
            </span>
          )}
        </div>
        <div className="flex items-center">
          <IconButton
            label={wrap ? "Disable line wrap" : "Wrap lines"}
            size="icon-sm"
            onClick={() => setWrap((w) => !w)}
          >
            <WrapText className={cn("h-3.5 w-3.5", wrap && "text-accent")} />
          </IconButton>
          <CopyButton text={text} label="Copy logs" />
        </div>
      </div>
      <div
        ref={ref}
        className="scrollbar-thin max-h-[420px] overflow-auto py-1.5 font-mono text-xs leading-5"
        role="log"
        aria-live={running ? "polite" : "off"}
      >
        {shown.length === 0 ? (
          <p className="px-3 py-6 text-center font-sans text-sm text-fg-subtle">
            {lines.length ? "No lines match this filter." : emptyText}
          </p>
        ) : (
          shown.map((l) => (
            <div
              key={l.id}
              className={cn("flex gap-3 px-3 hover:bg-surface-3/40", !wrap && "whitespace-pre")}
            >
              <span className="tabular shrink-0 select-none text-fg-faint">{formatLogTime(l.ts)}</span>
              <span className={cn("w-10 shrink-0 select-none uppercase", levelCls[l.level])}>{l.level}</span>
              <span className={cn("min-w-0 text-fg", wrap && "whitespace-pre-wrap break-words")}>
                {l.message}
              </span>
            </div>
          ))
        )}
      </div>
    </div>
  );
}
