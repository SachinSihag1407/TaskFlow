import { keepPreviousData, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { api, errorMessage, qs, request } from "@/lib/api";
import { usePollInterval } from "@/app/live";
import type {
  Attempt,
  AttemptDetail,
  AuditEvent,
  BulkResult,
  Health,
  HandlerInfo,
  Job,
  JobDetail,
  JobInput,
  JobListItem,
  LogLine,
  Notifications,
  Overview,
  Run,
  RunDetail,
  ScheduleView,
  Settings,
  SystemEvent,
  User,
  Worker,
  WorkerDetail,
} from "@/lib/types";

// ---- Queries ---------------------------------------------------------------

export interface JobQuery {
  q?: string;
  status?: string[];
  handler?: string[];
  schedule_type?: string;
  priority?: number;
  sort?: string;
  order?: "asc" | "desc";
  page?: number;
  page_size?: number;
}

export function useJobs(params: JobQuery) {
  const poll = usePollInterval();
  return useQuery({
    queryKey: ["jobs", params],
    queryFn: ({ signal }) => api.page<JobListItem>(`/jobs${qs({ ...params })}`, signal),
    placeholderData: keepPreviousData,
    refetchInterval: poll,
  });
}

export function useJob(id: string | undefined) {
  const poll = usePollInterval();
  return useQuery({
    queryKey: ["job", id],
    queryFn: ({ signal }) => api.get<JobDetail>(`/jobs/${id}`, signal),
    enabled: !!id,
    refetchInterval: poll,
  });
}

export function useHandlers() {
  return useQuery({
    queryKey: ["handlers"],
    queryFn: ({ signal }) => api.get<HandlerInfo[]>("/handlers", signal),
    staleTime: 5 * 60_000,
  });
}

export function useSchedules() {
  const poll = usePollInterval();
  return useQuery({
    queryKey: ["schedules"],
    queryFn: ({ signal }) => api.get<ScheduleView[]>("/schedules", signal),
    refetchInterval: poll,
  });
}

export function useRun(id: string | undefined) {
  const poll = usePollInterval();
  return useQuery({
    queryKey: ["run", id],
    queryFn: ({ signal }) => api.get<RunDetail>(`/runs/${id}`, signal),
    enabled: !!id,
    refetchInterval: poll,
  });
}

export function useDeadLetter(params: { q?: string; page?: number; page_size?: number }) {
  const poll = usePollInterval();
  return useQuery({
    queryKey: ["dead-letter", params],
    queryFn: ({ signal }) => api.page<Run>(`/dead-letter${qs({ ...params })}`, signal),
    placeholderData: keepPreviousData,
    refetchInterval: poll,
  });
}

export interface ExecutionQuery {
  q?: string;
  status?: string[];
  handler?: string[];
  job_id?: string;
  worker_id?: string;
  since?: string;
  sort?: string;
  order?: "asc" | "desc";
  page?: number;
  page_size?: number;
}

export function useExecutions(params: ExecutionQuery) {
  const poll = usePollInterval();
  return useQuery({
    queryKey: ["executions", params],
    queryFn: ({ signal }) => api.page<Attempt>(`/executions${qs({ ...params })}`, signal),
    placeholderData: keepPreviousData,
    refetchInterval: poll,
  });
}

export function useExecution(id: string | undefined) {
  return useQuery({
    queryKey: ["execution", id],
    queryFn: ({ signal }) => api.get<AttemptDetail>(`/executions/${id}`, signal),
    enabled: !!id,
    // Follow running attempts: refresh logs every 2s until the attempt finishes.
    refetchInterval: (q) => (q.state.data?.attempt.status === "RUNNING" ? 2000 : false),
  });
}

export function useWorkers() {
  return useQuery({
    queryKey: ["workers"],
    queryFn: ({ signal }) => api.get<Worker[]>("/workers", signal),
    // Heartbeats are not pushed over SSE; poll so freshness stays honest.
    refetchInterval: 5000,
  });
}

export function useWorker(id: string | undefined) {
  return useQuery({
    queryKey: ["worker", id],
    queryFn: ({ signal }) => api.get<WorkerDetail>(`/workers/${id}`, signal),
    enabled: !!id,
    refetchInterval: 5000,
  });
}

export function useOverview(range: string) {
  const poll = usePollInterval(30_000);
  return useQuery({
    queryKey: ["overview", range],
    queryFn: ({ signal }) => api.get<Overview>(`/analytics/overview${qs({ range })}`, signal),
    placeholderData: keepPreviousData,
    refetchInterval: poll,
  });
}

export function useHealth() {
  return useQuery({
    queryKey: ["health"],
    queryFn: ({ signal }) => api.get<Health>("/system/health", signal),
    refetchInterval: 10_000,
  });
}

export function useEvents(params: { severity?: string[]; limit?: number } = {}) {
  const poll = usePollInterval();
  return useQuery({
    queryKey: ["events", params],
    queryFn: ({ signal }) => api.get<SystemEvent[]>(`/events${qs({ ...params })}`, signal),
    refetchInterval: poll,
  });
}

export function useNotifications() {
  return useQuery({
    queryKey: ["notifications"],
    queryFn: ({ signal }) => api.get<Notifications>("/notifications", signal),
    refetchInterval: 30_000,
  });
}

export function useSettings() {
  return useQuery({
    queryKey: ["settings"],
    queryFn: ({ signal }) => api.get<Settings>("/settings", signal),
  });
}

export function useUsers(enabled: boolean) {
  return useQuery({
    queryKey: ["users"],
    queryFn: ({ signal }) => api.get<User[]>("/users", signal),
    enabled,
  });
}

export function useAudit(params: { action?: string; page?: number }, enabled: boolean) {
  return useQuery({
    queryKey: ["audit", params],
    queryFn: ({ signal }) => api.page<AuditEvent>(`/audit${qs({ ...params, page_size: 50 })}`, signal),
    enabled,
    placeholderData: keepPreviousData,
  });
}

export async function fetchLogs(attemptId: string, after: number) {
  return api.get<LogLine[]>(`/executions/${attemptId}/logs${qs({ after })}`);
}

// ---- Mutations -------------------------------------------------------------

/** Invalidates everything that depends on job/run state. */
export function useInvalidateJobs() {
  const qc = useQueryClient();
  return () =>
    Promise.all(
      [
        "jobs",
        "job",
        "run",
        "schedules",
        "dead-letter",
        "executions",
        "overview",
        "events",
        "notifications",
      ].map((k) => qc.invalidateQueries({ queryKey: [k] })),
    );
}

type JobActionName = "pause" | "resume" | "cancel" | "retry" | "run";

const actionCopy: Record<JobActionName, string> = {
  pause: "Job paused",
  resume: "Job resumed",
  cancel: "Job cancelled",
  retry: "Retry queued",
  run: "Run queued",
};

export function useJobAction() {
  const invalidate = useInvalidateJobs();
  return useMutation({
    mutationFn: ({ id, action }: { id: string; action: JobActionName }) =>
      api.post<Job | Run>(`/jobs/${id}/${action}`),
    onSuccess: (_d, v) => {
      toast.success(actionCopy[v.action]);
      return invalidate();
    },
    onError: (e) => toast.error(errorMessage(e)),
  });
}

export function useBulkJobs() {
  const invalidate = useInvalidateJobs();
  return useMutation({
    mutationFn: (body: { action: string; ids: string[] }) => api.post<BulkResult[]>("/jobs/bulk", body),
    onSuccess: (results, v) => {
      const ok = results.filter((r) => r.ok).length;
      const failed = results.length - ok;
      if (failed === 0)
        toast.success(`${v.action[0].toUpperCase() + v.action.slice(1)}: ${ok} job(s) updated`);
      else
        toast.warning(`${ok} updated, ${failed} skipped`, {
          description: results.find((r) => !r.ok)?.message,
        });
      return invalidate();
    },
    onError: (e) => toast.error(errorMessage(e)),
  });
}

export function useCreateJob() {
  const invalidate = useInvalidateJobs();
  return useMutation({
    mutationFn: ({ input, key }: { input: JobInput; key: string }) =>
      request<Job>("/jobs", { method: "POST", body: input, headers: { "Idempotency-Key": key } }).then(
        (r) => r.data,
      ),
    onSuccess: () => invalidate(),
  });
}

export function useUpdateJob() {
  const invalidate = useInvalidateJobs();
  return useMutation({
    mutationFn: ({ id, input, version }: { id: string; input: JobInput; version: number }) =>
      api.put<Job>(`/jobs/${id}`, input, { "If-Match": String(version) }),
    onSuccess: () => invalidate(),
  });
}

export function useRunAction() {
  const invalidate = useInvalidateJobs();
  return useMutation({
    mutationFn: ({ id, action }: { id: string; action: "cancel" | "replay" }) =>
      api.post<Run>(`/runs/${id}/${action}`),
    onSuccess: (_d, v) => {
      toast.success(v.action === "cancel" ? "Cancellation requested" : "Run replayed");
      return invalidate();
    },
    onError: (e) => toast.error(errorMessage(e)),
  });
}

export function useBulkReplay() {
  const invalidate = useInvalidateJobs();
  return useMutation({
    mutationFn: (ids: string[]) => api.post<BulkResult[]>("/dead-letter/replay", { ids }),
    onSuccess: (results) => {
      const ok = results.filter((r) => r.ok).length;
      const failed = results.length - ok;
      if (failed === 0) toast.success(`Replayed ${ok} run(s)`);
      else
        toast.warning(`${ok} replayed, ${failed} skipped`, {
          description: results.find((r) => !r.ok)?.message,
        });
      return invalidate();
    },
    onError: (e) => toast.error(errorMessage(e)),
  });
}

export function useDrainWorker() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => api.post<void>(`/workers/${id}/drain`),
    onSuccess: () => {
      toast.success("Drain requested. The worker will finish in-flight runs and stop.");
      return qc.invalidateQueries({ queryKey: ["workers"] });
    },
    onError: (e) => toast.error(errorMessage(e)),
  });
}
