import { useEffect, useState } from "react";
import { formatDateTime, relativeTime } from "@/lib/format";
import { Tooltip } from "@/components/ui/tooltip";

/** Re-renders every 15s so relative times stay accurate. */
export function useNow(intervalMs = 15_000) {
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), intervalMs);
    return () => clearInterval(t);
  }, [intervalMs]);
  return now;
}

/** Relative time with the absolute timestamp (and zone) on hover. */
export function RelTime({
  iso,
  timeZone,
  className,
}: {
  iso: string | null | undefined;
  timeZone?: string;
  className?: string;
}) {
  const now = useNow();
  if (!iso) return <span className="text-fg-faint">—</span>;
  return (
    <Tooltip content={`${formatDateTime(iso, timeZone)}${timeZone ? ` (${timeZone})` : ""}`}>
      <time dateTime={iso} className={className ?? "tabular whitespace-nowrap"}>
        {relativeTime(iso, now)}
      </time>
    </Tooltip>
  );
}
