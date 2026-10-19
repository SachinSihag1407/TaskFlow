// Formatting helpers. All timestamps from the API are UTC ISO strings; they
// are rendered in the viewer's local timezone unless a zone is given.

const rtf = new Intl.RelativeTimeFormat(undefined, { numeric: "auto", style: "short" });

export function relativeTime(iso: string | null | undefined, now = Date.now()): string {
  if (!iso) return "—";
  const diff = new Date(iso).getTime() - now;
  const abs = Math.abs(diff);
  if (abs < 5_000) return diff > 0 ? "in a moment" : "just now";
  if (abs < 60_000) return rtf.format(Math.round(diff / 1000), "second");
  if (abs < 3_600_000) return rtf.format(Math.round(diff / 60_000), "minute");
  if (abs < 86_400_000) return rtf.format(Math.round(diff / 3_600_000), "hour");
  if (abs < 30 * 86_400_000) return rtf.format(Math.round(diff / 86_400_000), "day");
  return formatDate(iso);
}

export function formatDateTime(iso: string | null | undefined, timeZone?: string): string {
  if (!iso) return "—";
  return new Intl.DateTimeFormat(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    timeZone,
  }).format(new Date(iso));
}

export function formatDate(iso: string | null | undefined): string {
  if (!iso) return "—";
  return new Intl.DateTimeFormat(undefined, { year: "numeric", month: "short", day: "numeric" }).format(
    new Date(iso),
  );
}

export function formatTime(iso: string, withSeconds = false, timeZone?: string): string {
  return new Intl.DateTimeFormat(undefined, {
    hour: "2-digit",
    minute: "2-digit",
    second: withSeconds ? "2-digit" : undefined,
    timeZone,
  }).format(new Date(iso));
}

/** Precise timestamp for logs: HH:MM:SS.mmm in local time. */
export function formatLogTime(iso: string): string {
  const d = new Date(iso);
  const p = (n: number, w = 2) => String(n).padStart(w, "0");
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}.${p(d.getMilliseconds(), 3)}`;
}

export function formatDuration(ms: number | null | undefined): string {
  if (ms === null || ms === undefined || Number.isNaN(ms)) return "—";
  if (ms < 1) return "<1 ms";
  if (ms < 1000) return `${Math.round(ms)} ms`;
  const s = ms / 1000;
  if (s < 60) return `${s < 10 ? s.toFixed(2) : s.toFixed(1)} s`;
  const m = Math.floor(s / 60);
  const rem = Math.round(s % 60);
  if (m < 60) return `${m}m ${rem}s`;
  const h = Math.floor(m / 60);
  return `${h}h ${m % 60}m`;
}

export function formatSeconds(sec: number): string {
  if (sec < 60) return `${sec}s`;
  if (sec < 3600) return sec % 60 === 0 ? `${sec / 60}m` : `${Math.floor(sec / 60)}m ${sec % 60}s`;
  return sec % 3600 === 0 ? `${sec / 3600}h` : `${Math.floor(sec / 3600)}h ${Math.round((sec % 3600) / 60)}m`;
}

const nf = new Intl.NumberFormat();
export function formatNumber(n: number | null | undefined): string {
  if (n === null || n === undefined) return "—";
  return nf.format(n);
}

export function formatPercent(ratio: number | null | undefined, digits = 1): string {
  if (ratio === null || ratio === undefined) return "—";
  return `${(ratio * 100).toFixed(digits)}%`;
}

export function formatBytes(b: number | null | undefined): string {
  if (b === null || b === undefined) return "—";
  const units = ["B", "KB", "MB", "GB"];
  let i = 0;
  let v = b;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(v < 10 && i > 0 ? 1 : 0)} ${units[i]}`;
}

export function shortId(id: string | null | undefined): string {
  return id ? id.slice(0, 8) : "—";
}

export function titleCase(s: string): string {
  return s
    .toLowerCase()
    .split(/[_\s]+/)
    .map((w) => w.charAt(0).toUpperCase() + w.slice(1))
    .join(" ");
}

/** The browser's IANA timezone, used as a sensible default. */
export function localTimeZone(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
  } catch {
    return "UTC";
  }
}

/** Full list of IANA zones supported by the browser, with UTC first. */
export function timeZones(): string[] {
  const intl = Intl as unknown as { supportedValuesOf?: (k: string) => string[] };
  const zones = intl.supportedValuesOf
    ? intl.supportedValuesOf("timeZone")
    : ["UTC", "Europe/London", "America/New_York"];
  return ["UTC", ...zones.filter((z) => z !== "UTC")];
}

/** "1 run", "3 runs" — count with a correctly pluralised noun. */
export function plural(n: number, noun: string, pluralNoun = `${noun}s`): string {
  return `${formatNumber(n)} ${n === 1 ? noun : pluralNoun}`;
}
