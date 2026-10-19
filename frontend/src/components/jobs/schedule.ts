import cronstrue from "cronstrue";
import type { Job } from "@/lib/types";
import { formatDateTime } from "@/lib/format";

/** Human description of a cron expression; falls back to the raw text. */
export function describeCron(expr: string | null | undefined): string {
  if (!expr) return "";
  try {
    return cronstrue.toString(expr, { use24HourTimeFormat: true, verbose: false });
  } catch {
    return expr;
  }
}

/** One-line schedule summary for tables. */
export function scheduleSummary(j: Pick<Job, "schedule_type" | "cron_expr" | "timezone" | "run_at">): string {
  switch (j.schedule_type) {
    case "cron":
      return `${describeCron(j.cron_expr)}${j.timezone !== "UTC" ? ` (${j.timezone})` : ""}`;
    case "delayed":
      return j.run_at ? `Delayed · ${formatDateTime(j.run_at)}` : "Delayed";
    default:
      return j.run_at ? `Once · ${formatDateTime(j.run_at)}` : "Once";
  }
}
