import type { ReactNode } from "react";
import { cn } from "@/lib/utils";
import { statusMeta, toneClasses, type Tone } from "@/lib/status";
import { Tooltip } from "./tooltip";

export function Badge({
  tone = "neutral",
  children,
  className,
}: {
  tone?: Tone;
  children: ReactNode;
  className?: string;
}) {
  return (
    <span
      className={cn(
        "inline-flex h-5 items-center gap-1 whitespace-nowrap rounded px-1.5 text-2xs font-medium ring-1 ring-inset",
        toneClasses[tone].badge,
        className,
      )}
    >
      {children}
    </span>
  );
}

/** Status pill with a coloured dot; the description appears on hover. */
export function StatusBadge({
  status,
  className,
  withTooltip = true,
}: {
  status: string;
  className?: string;
  withTooltip?: boolean;
}) {
  const meta = statusMeta(status);
  const t = toneClasses[meta.tone];
  const pill = (
    <span
      className={cn(
        "inline-flex h-5 items-center gap-1.5 whitespace-nowrap rounded px-1.5 text-2xs font-medium ring-1 ring-inset",
        t.badge,
        className,
      )}
    >
      <span className={cn("h-1.5 w-1.5 rounded-full", t.dot, meta.live && "animate-pulse-dot")} aria-hidden />
      {meta.label}
    </span>
  );
  return withTooltip ? <Tooltip content={meta.description}>{pill}</Tooltip> : pill;
}

/** Inline dot + label, for dense contexts. */
export function StatusDot({ status, label }: { status: string; label?: string }) {
  const meta = statusMeta(status);
  return (
    <span className="inline-flex items-center gap-1.5 text-sm">
      <span
        className={cn("h-2 w-2 rounded-full", toneClasses[meta.tone].dot, meta.live && "animate-pulse-dot")}
        aria-hidden
      />
      <span className={toneClasses[meta.tone].text}>{label ?? meta.label}</span>
    </span>
  );
}
