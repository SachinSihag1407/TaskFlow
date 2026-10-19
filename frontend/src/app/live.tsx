import { createContext, useContext, useEffect, useRef, useState, type ReactNode } from "react";
import { useQueryClient } from "@tanstack/react-query";

/**
 * Live updates over Server-Sent Events.
 *
 * - "live":      stream connected and the API is receiving events from Redis.
 * - "degraded":  stream connected but the API's Redis subscription is down.
 * - "offline":   stream disconnected (EventSource retries automatically).
 *
 * Events carry identifiers only. On each event the affected queries are
 * invalidated (debounced) and re-fetched through the normal API. Whenever the
 * stream is not live, queries fall back to polling (see usePollInterval).
 */
export type LiveStatus = "connecting" | "live" | "degraded" | "offline";

const LiveContext = createContext<LiveStatus>("connecting");

export function useLiveStatus() {
  return useContext(LiveContext);
}

/** Polling interval for queries: slow when live, fast otherwise. */
export function usePollInterval(liveInterval: number | false = false, fallback = 5000): number | false {
  const status = useContext(LiveContext);
  return status === "live" ? liveInterval : fallback;
}

const INVALIDATE: Record<string, string[]> = {
  "run.updated": [
    "jobs",
    "job",
    "run",
    "executions",
    "overview",
    "dead-letter",
    "schedules",
    "events",
    "worker",
  ],
  "job.updated": ["jobs", "job", "overview", "schedules"],
};

export function LiveProvider({ children, enabled }: { children: ReactNode; enabled: boolean }) {
  const qc = useQueryClient();
  const [status, setStatus] = useState<LiveStatus>("connecting");
  const pending = useRef(new Set<string>());
  const timer = useRef<ReturnType<typeof setTimeout>>();

  useEffect(() => {
    if (!enabled || typeof EventSource === "undefined") {
      setStatus("offline");
      return;
    }
    const es = new EventSource("/api/v1/events/stream", { withCredentials: true });
    const flush = () => {
      for (const k of pending.current) qc.invalidateQueries({ queryKey: [k] });
      pending.current.clear();
    };
    es.addEventListener("status", (e) => {
      try {
        const { live } = JSON.parse((e as MessageEvent).data) as { live: boolean };
        setStatus(live ? "live" : "degraded");
      } catch {
        setStatus("degraded");
      }
    });
    es.addEventListener("update", (e) => {
      try {
        const ev = JSON.parse((e as MessageEvent).data) as { type: string };
        for (const k of INVALIDATE[ev.type] ?? []) pending.current.add(k);
      } catch {
        return;
      }
      // Coalesce bursts (e.g. 50 runs finishing together) into one refetch.
      clearTimeout(timer.current);
      timer.current = setTimeout(flush, 400);
    });
    es.onerror = () => setStatus("offline");
    return () => {
      clearTimeout(timer.current);
      es.close();
    };
  }, [enabled, qc]);

  return <LiveContext.Provider value={status}>{children}</LiveContext.Provider>;
}
