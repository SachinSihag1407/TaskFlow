import { useEffect, useMemo, useRef, useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { AlertCircle, ArrowLeft, Braces, CalendarClock, Check, Clock, Timer, Wand2 } from "lucide-react";
import { toast } from "sonner";
import { usePageMeta } from "@/app/page-meta";
import { useCan, VIEWER_REASON } from "@/app/session";
import { useCreateJob, useHandlers, useJob, useSettings, useUpdateJob } from "@/api/hooks";
import { ApiError, api, errorMessage } from "@/lib/api";
import type {
  HandlerInfo,
  Job,
  JobInput,
  MissedRunPolicy,
  OverlapPolicy,
  ScheduleType,
  Settings,
} from "@/lib/types";
import { formatDateTime, formatSeconds, localTimeZone, timeZones } from "@/lib/format";
import { cn } from "@/lib/utils";
import { useDebouncedValue } from "@/lib/url-state";
import { Button } from "@/components/ui/button";
import { Field, Input, Select, Textarea } from "@/components/ui/input";
import { DescriptionList, PageHeader, Panel, Segmented } from "@/components/ui/layout";
import { ErrorState, Skeleton } from "@/components/ui/feedback";
import { JsonBlock } from "@/components/ui/code";
import { describeCron } from "@/components/jobs/schedule";

const CRON_PRESETS = [
  { label: "Every 5 minutes", expr: "*/5 * * * *" },
  { label: "Hourly", expr: "0 * * * *" },
  { label: "Daily at 09:00", expr: "0 9 * * *" },
  { label: "Weekdays at 09:00", expr: "0 9 * * 1-5" },
  { label: "Weekly (Mon 06:00)", expr: "0 6 * * 1" },
  { label: "Monthly (1st, 00:00)", expr: "0 0 1 * *" },
];

const DELAY_UNITS = { seconds: 1, minutes: 60, hours: 3600, days: 86400 } as const;
type DelayUnit = keyof typeof DELAY_UNITS;

interface FormState {
  name: string;
  description: string;
  handler: string;
  payloadText: string;
  scheduleType: ScheduleType;
  runImmediately: boolean;
  runAtLocal: string;
  delayAmount: string;
  delayUnit: DelayUnit;
  cronExpr: string;
  timezone: string;
  missedRunPolicy: MissedRunPolicy;
  overlapPolicy: OverlapPolicy;
  maxConcurrency: string;
  priority: string;
  maxAttempts: string;
  backoffBase: string;
  backoffMax: string;
  timeout: string;
}

function toLocalInput(iso: string): string {
  const d = new Date(iso);
  const p = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`;
}

function initialState(settings?: Settings, job?: Job): FormState {
  if (job) {
    return {
      name: job.name,
      description: job.description,
      handler: job.handler,
      payloadText: JSON.stringify(job.payload, null, 2),
      scheduleType: job.schedule_type === "delayed" ? "once" : job.schedule_type,
      runImmediately: !job.run_at || new Date(job.run_at).getTime() <= Date.now(),
      runAtLocal: job.run_at && new Date(job.run_at).getTime() > Date.now() ? toLocalInput(job.run_at) : "",
      delayAmount: "5",
      delayUnit: "minutes",
      cronExpr: job.cron_expr ?? "0 9 * * *",
      timezone: job.timezone,
      missedRunPolicy: job.missed_run_policy,
      overlapPolicy: job.overlap_policy,
      maxConcurrency: String(job.max_concurrency),
      priority: String(job.priority),
      maxAttempts: String(job.max_attempts),
      backoffBase: String(job.backoff_base_seconds),
      backoffMax: String(job.backoff_max_seconds),
      timeout: String(job.timeout_seconds),
    };
  }
  return {
    name: "",
    description: "",
    handler: "",
    payloadText: "{}",
    scheduleType: "once",
    runImmediately: true,
    runAtLocal: "",
    delayAmount: "5",
    delayUnit: "minutes",
    cronExpr: "0 9 * * *",
    timezone: settings?.default_timezone ?? localTimeZone(),
    missedRunPolicy: "run_once",
    overlapPolicy: "skip",
    maxConcurrency: "1",
    priority: String(settings?.default_priority ?? 5),
    maxAttempts: String(settings?.default_max_attempts ?? 3),
    backoffBase: String(settings?.default_backoff_base_seconds ?? 10),
    backoffMax: String(settings?.default_backoff_max_seconds ?? 600),
    timeout: String(settings?.default_timeout_seconds ?? 300),
  };
}

const intIn = (v: string, lo: number, hi: number) => /^\d+$/.test(v) && Number(v) >= lo && Number(v) <= hi;

/** Client-side validation mirroring the server's rules for instant feedback. */
function validate(s: FormState, handler?: HandlerInfo): Record<string, string> {
  const e: Record<string, string> = {};
  if (!s.name.trim()) e.name = "Name is required.";
  else if (s.name.trim().length > 200) e.name = "Name must be at most 200 characters.";
  if (s.description.length > 2000) e.description = "Description must be at most 2000 characters.";
  if (!s.handler) e.handler = "Choose a handler.";
  try {
    const p = JSON.parse(s.payloadText || "{}");
    if (typeof p !== "object" || p === null || Array.isArray(p)) e.payload = "Payload must be a JSON object.";
    else if (handler) {
      const missing = handler.fields.filter((f) => f.required && !(f.name in p)).map((f) => f.name);
      if (missing.length)
        e.payload = `Missing required field${missing.length > 1 ? "s" : ""}: ${missing.join(", ")}.`;
    }
  } catch (err) {
    e.payload = `Invalid JSON: ${(err as Error).message}`;
  }
  if (s.scheduleType === "once" && !s.runImmediately) {
    if (!s.runAtLocal) e.run_at = "Pick a date and time.";
    else if (new Date(s.runAtLocal).getTime() < Date.now() - 60_000)
      e.run_at = "Run time must be in the future.";
  }
  if (
    s.scheduleType === "delayed" &&
    (!intIn(s.delayAmount, 1, 1_000_000) || Number(s.delayAmount) * DELAY_UNITS[s.delayUnit] > 365 * 86400)
  )
    e.delay_seconds = "Delay must be between 1 second and 365 days.";
  if (
    s.scheduleType === "cron" &&
    s.cronExpr.trim().split(/\s+/).length !== 5 &&
    !s.cronExpr.trim().startsWith("@")
  )
    e.cron_expr = "Use a 5-field cron expression.";
  if (!intIn(s.priority, 1, 10)) e.priority = "Priority must be between 1 and 10.";
  if (!intIn(s.maxAttempts, 1, 25)) e.max_attempts = "Between 1 and 25.";
  if (!intIn(s.backoffBase, 1, 3600)) e.backoff_base_seconds = "Between 1 and 3600 seconds.";
  if (!intIn(s.backoffMax, 1, 86400)) e.backoff_max_seconds = "Between 1 and 86400 seconds.";
  else if (Number(s.backoffMax) < Number(s.backoffBase))
    e.backoff_max_seconds = "Must be at least the base delay.";
  if (!intIn(s.timeout, 1, 86400)) e.timeout_seconds = "Between 1 and 86400 seconds.";
  if (s.overlapPolicy === "allow" && !intIn(s.maxConcurrency, 1, 100))
    e.max_concurrency = "Between 1 and 100.";
  return e;
}

function toInput(s: FormState): JobInput {
  const input: JobInput = {
    name: s.name.trim(),
    description: s.description,
    handler: s.handler,
    payload: JSON.parse(s.payloadText || "{}"),
    schedule_type: s.scheduleType,
    timezone: s.timezone,
    missed_run_policy: s.missedRunPolicy,
    overlap_policy: s.overlapPolicy,
    max_concurrency: s.overlapPolicy === "allow" ? Number(s.maxConcurrency) : 1,
    priority: Number(s.priority),
    max_attempts: Number(s.maxAttempts),
    backoff_base_seconds: Number(s.backoffBase),
    backoff_max_seconds: Number(s.backoffMax),
    timeout_seconds: Number(s.timeout),
  };
  if (s.scheduleType === "once" && !s.runImmediately) input.run_at = new Date(s.runAtLocal).toISOString();
  if (s.scheduleType === "delayed") input.delay_seconds = Number(s.delayAmount) * DELAY_UNITS[s.delayUnit];
  if (s.scheduleType === "cron") input.cron_expr = s.cronExpr.trim();
  return input;
}

function SectionTitle({
  icon,
  title,
  description,
}: {
  icon: React.ReactNode;
  title: string;
  description: string;
}) {
  return (
    <div className="mb-4 flex items-start gap-2.5">
      <span className="mt-0.5 text-fg-subtle [&>svg]:h-4 [&>svg]:w-4">{icon}</span>
      <div>
        <h2 className="text-sm font-semibold">{title}</h2>
        <p className="text-xs text-fg-subtle">{description}</p>
      </div>
    </div>
  );
}

function CronPreview({ expr, timezone }: { expr: string; timezone: string }) {
  const debounced = useDebouncedValue(expr.trim(), 350);
  const q = useQuery({
    queryKey: ["cron-preview", debounced, timezone],
    queryFn: () =>
      api.post<{ next_runs: string[] }>("/schedules/preview", { cron_expr: debounced, timezone, count: 5 }),
    enabled: debounced.length > 0,
    retry: false,
  });
  if (!debounced) return null;
  return (
    <div className="rounded-md border bg-surface-2/40 p-3">
      <div className="text-sm font-medium">{describeCron(debounced)}</div>
      {q.isLoading ? (
        <Skeleton className="mt-2 h-16 w-full" />
      ) : q.error ? (
        <p className="mt-1.5 flex items-center gap-1.5 text-xs text-danger">
          <AlertCircle className="h-3.5 w-3.5" />
          {errorMessage(q.error)}
        </p>
      ) : (
        <>
          <div className="mt-2 text-2xs uppercase tracking-wide text-fg-subtle">Next runs ({timezone})</div>
          <ol className="mt-1 space-y-0.5 font-mono text-xs tabular">
            {q.data?.next_runs.map((t) => (
              <li key={t}>{formatDateTime(t, timezone)}</li>
            ))}
          </ol>
        </>
      )}
    </div>
  );
}

function BackoffPreview({ attempts, base, max }: { attempts: number; base: number; max: number }) {
  if (!attempts || !base || !max || attempts < 2)
    return (
      <p className="text-xs text-fg-subtle">No retries: a failure goes straight to the dead-letter queue.</p>
    );
  const rows = Array.from({ length: Math.min(attempts - 1, 6) }, (_, i) => {
    const ceil = Math.min(Math.max(max, base), base * 2 ** i);
    return { n: i + 1, lo: Math.round(ceil / 2), hi: Math.round(ceil) };
  });
  return (
    <div className="text-xs text-fg-subtle">
      <p className="mb-1">Retry delays (exponential backoff with jitter):</p>
      <ul className="tabular grid grid-cols-2 gap-x-4 gap-y-0.5 sm:grid-cols-3">
        {rows.map((r) => (
          <li key={r.n}>
            after attempt {r.n}: {formatSeconds(r.lo)}–{formatSeconds(r.hi)}
          </li>
        ))}
        {attempts - 1 > 6 && <li>…</li>}
      </ul>
    </div>
  );
}

export default function JobFormPage() {
  const { id } = useParams();
  const editing = !!id;
  const navigate = useNavigate();
  const can = useCan();
  const settings = useSettings();
  const handlers = useHandlers();
  const existing = useJob(id);
  const create = useCreateJob();
  const update = useUpdateJob();
  usePageMeta(
    editing
      ? [
          { label: "Jobs", to: "/jobs" },
          { label: existing.data?.job.name ?? "Job", to: `/jobs/${id}` },
          { label: "Edit" },
        ]
      : [{ label: "Jobs", to: "/jobs" }, { label: "New job" }],
  );

  const [s, setS] = useState<FormState | null>(null);
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [step, setStep] = useState<"edit" | "review">("edit");
  const [serverError, setServerError] = useState<string | null>(null);
  const payloadTouched = useRef(editing);
  // One key per form session: a double-click or a retried request after a
  // network blip returns the same job instead of creating a duplicate.
  const idemKey = useRef(globalThis.crypto?.randomUUID?.() ?? `${Date.now()}-${Math.random()}`);
  const zones = useMemo(timeZones, []);

  useEffect(() => {
    if (s) return;
    if (editing && existing.data) setS(initialState(settings.data, existing.data.job));
    else if (!editing && settings.data) setS(initialState(settings.data));
    else if (!editing && settings.error) setS(initialState());
  }, [s, editing, existing.data, settings.data, settings.error]);

  const handler = handlers.data?.find((h) => h.name === s?.handler);
  const set = <K extends keyof FormState>(k: K, v: FormState[K]) => {
    setS((prev) => (prev ? { ...prev, [k]: v } : prev));
    setErrors((e) => {
      if (!e[k as string]) return e;
      const n = { ...e };
      delete n[k as string];
      return n;
    });
  };

  if (editing && existing.error)
    return <ErrorState error={existing.error} onRetry={() => existing.refetch()} />;
  if (!s || handlers.isLoading) {
    return (
      <div className="space-y-4">
        <Skeleton className="h-8 w-48" />
        <Skeleton className="h-64 w-full" />
        <Skeleton className="h-64 w-full" />
      </div>
    );
  }

  const editBlocked =
    editing && existing.data && !existing.data.actions.edit.allowed
      ? existing.data.actions.edit.reason
      : undefined;
  const writeBlocked = !can("write") ? VIEWER_REASON : editBlocked;

  const chooseHandler = (h: HandlerInfo) => {
    set("handler", h.name);
    if (!payloadTouched.current || s.payloadText.trim() === "{}" || s.payloadText.trim() === "") {
      set("payloadText", JSON.stringify(h.example_payload, null, 2));
    }
  };

  const review = () => {
    const e = validate(s, handler);
    setErrors(e);
    setServerError(null);
    if (Object.keys(e).length) {
      toast.error("Fix the highlighted fields.");
      document.querySelector("[aria-invalid=true]")?.scrollIntoView({ behavior: "smooth", block: "center" });
      return;
    }
    setStep("review");
    window.scrollTo({ top: 0, behavior: "smooth" });
  };

  const submit = async () => {
    setServerError(null);
    try {
      const input = toInput(s);
      const job = editing
        ? await update.mutateAsync({ id: id!, input, version: existing.data!.job.version })
        : await create.mutateAsync({ input, key: idemKey.current });
      toast.success(editing ? "Job updated" : "Job created", {
        description: editing
          ? "Changes apply to future runs."
          : job.next_run_at
            ? `First run ${formatDateTime(job.next_run_at)}`
            : undefined,
      });
      navigate(`/jobs/${job.id}`);
    } catch (err) {
      if (err instanceof ApiError && Object.keys(err.fields).length) {
        setErrors(err.fields);
        setStep("edit");
        toast.error("Some fields need attention.");
      } else if (err instanceof ApiError && err.code === "version_conflict") {
        setServerError(
          "Someone else changed this job while you were editing. Reload to get the latest version, then re-apply your changes.",
        );
      } else {
        setServerError(errorMessage(err));
      }
    }
  };

  const payloadPreview = (() => {
    try {
      return JSON.parse(s.payloadText);
    } catch {
      return s.payloadText;
    }
  })();

  return (
    <div className="mx-auto max-w-4xl">
      <PageHeader
        title={editing ? `Edit ${existing.data?.job.name}` : "New job"}
        description={
          editing
            ? "Changes apply to future runs. Runs already queued keep the configuration they were created with."
            : "Configure what runs, when it runs and how failures are handled."
        }
        actions={
          <Button
            variant="ghost"
            icon={<ArrowLeft className="h-3.5 w-3.5" />}
            onClick={() =>
              step === "review" ? setStep("edit") : navigate(editing ? `/jobs/${id}` : "/jobs")
            }
          >
            {step === "review" ? "Back to editing" : "Cancel"}
          </Button>
        }
      />

      {editBlocked && (
        <div
          className="mb-4 flex items-start gap-2 rounded-md bg-warning-soft px-3 py-2 text-sm text-warning"
          role="status"
        >
          <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" /> {editBlocked}
        </div>
      )}
      {serverError && (
        <div
          className="mb-4 flex items-start justify-between gap-2 rounded-md bg-danger-soft px-3 py-2 text-sm text-danger"
          role="alert"
        >
          <span className="flex gap-2">
            <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" />
            {serverError}
          </span>
          {editing && (
            <Button size="sm" onClick={() => window.location.reload()}>
              Reload
            </Button>
          )}
        </div>
      )}

      <ol className="mb-5 flex items-center gap-2 text-xs" aria-label="Steps">
        {(["edit", "review"] as const).map((st, i) => (
          <li
            key={st}
            className={cn("flex items-center gap-1.5", step === st ? "text-fg" : "text-fg-subtle")}
          >
            <span
              className={cn(
                "flex h-5 w-5 items-center justify-center rounded-full text-2xs ring-1",
                step === st
                  ? "bg-accent text-accent-fg ring-accent"
                  : i === 0
                    ? "bg-success-soft text-success ring-success/30"
                    : "ring-line-strong",
              )}
            >
              {step === "review" && i === 0 ? <Check className="h-3 w-3" /> : i + 1}
            </span>
            {st === "edit" ? "Configure" : "Review"}
            {i === 0 && <span className="mx-1 h-px w-6 bg-line-strong" />}
          </li>
        ))}
      </ol>

      {step === "edit" ? (
        <div className="space-y-5">
          <Panel bodyClassName="p-5">
            <SectionTitle icon={<Braces />} title="Basics" description="How this job appears in TaskFlow." />
            <div className="grid gap-4">
              <Field label="Name" required error={errors.name}>
                {(p) => (
                  <Input
                    {...p}
                    value={s.name}
                    onChange={(e) => set("name", e.target.value)}
                    placeholder="Nightly sales report"
                    maxLength={200}
                  />
                )}
              </Field>
              <Field label="Description" error={errors.description} hint="Optional context for teammates.">
                {(p) => (
                  <Textarea
                    {...p}
                    value={s.description}
                    onChange={(e) => set("description", e.target.value)}
                    rows={2}
                    maxLength={2000}
                  />
                )}
              </Field>
            </div>
          </Panel>

          <Panel bodyClassName="p-5">
            <SectionTitle
              icon={<Wand2 />}
              title="Handler & payload"
              description="Predefined job types only — TaskFlow never runs arbitrary code."
            />
            <div role="radiogroup" aria-label="Handler" className="grid gap-2 sm:grid-cols-2">
              {(handlers.data ?? []).map((h) => (
                <button
                  key={h.name}
                  type="button"
                  role="radio"
                  aria-checked={s.handler === h.name}
                  onClick={() => chooseHandler(h)}
                  className={cn(
                    "rounded-md border p-3 text-left transition-colors",
                    s.handler === h.name
                      ? "border-accent bg-accent-soft/40 ring-1 ring-accent"
                      : "hover:bg-surface-2/60",
                  )}
                >
                  <div className="flex items-center justify-between gap-2">
                    <span className="text-sm font-medium">{h.title}</span>
                    <code className="text-2xs text-fg-subtle">{h.name}</code>
                  </div>
                  <p className="mt-1 text-xs text-fg-subtle">{h.description}</p>
                </button>
              ))}
            </div>
            {errors.handler && (
              <p role="alert" className="mt-2 text-xs text-danger">
                {errors.handler}
              </p>
            )}

            {handler && (
              <div className="mt-5 grid gap-4 lg:grid-cols-[1fr_260px]">
                <Field label="Payload (JSON)" required error={errors.payload}>
                  {(p) => (
                    <div>
                      <Textarea
                        {...p}
                        value={s.payloadText}
                        onChange={(e) => {
                          payloadTouched.current = true;
                          set("payloadText", e.target.value);
                        }}
                        spellCheck={false}
                        rows={Math.min(18, Math.max(6, s.payloadText.split("\n").length + 1))}
                        className="font-mono text-xs"
                      />
                      <div className="mt-1.5 flex gap-2">
                        <Button
                          size="sm"
                          variant="outline"
                          onClick={() => {
                            try {
                              set("payloadText", JSON.stringify(JSON.parse(s.payloadText), null, 2));
                            } catch (err) {
                              setErrors((e) => ({
                                ...e,
                                payload: `Invalid JSON: ${(err as Error).message}`,
                              }));
                            }
                          }}
                        >
                          Format
                        </Button>
                        <Button
                          size="sm"
                          variant="outline"
                          onClick={() => set("payloadText", JSON.stringify(handler.example_payload, null, 2))}
                        >
                          Use example
                        </Button>
                      </div>
                    </div>
                  )}
                </Field>
                <div className="rounded-md border bg-surface-2/40 p-3 text-xs">
                  <div className="mb-2 font-medium text-fg">Fields</div>
                  <ul className="space-y-2">
                    {handler.fields.map((f) => (
                      <li key={f.name}>
                        <code className="text-fg">{f.name}</code>
                        <span className="text-fg-subtle">
                          {" "}
                          · {f.type}
                          {f.required && <span className="text-danger"> · required</span>}
                        </span>
                        <p className="text-fg-subtle">{f.description}</p>
                      </li>
                    ))}
                  </ul>
                  <p className="mt-3 border-t pt-2 text-fg-subtle">
                    <span className="font-medium text-fg-muted">Idempotency: </span>
                    {handler.idempotency}
                  </p>
                </div>
              </div>
            )}
          </Panel>

          <Panel bodyClassName="p-5">
            <SectionTitle
              icon={<CalendarClock />}
              title="Schedule"
              description="Times are stored in UTC; cron expressions run in the timezone you choose."
            />
            {!editing || existing.data?.job.schedule_type !== "cron" ? (
              <Segmented
                label="Schedule type"
                value={s.scheduleType}
                onChange={(v) => set("scheduleType", v)}
                options={
                  editing
                    ? [{ value: "once", label: "One-time" }]
                    : [
                        { value: "once", label: "One-time" },
                        { value: "delayed", label: "Delayed" },
                        { value: "cron", label: "Recurring" },
                      ]
                }
              />
            ) : (
              <p className="text-xs text-fg-subtle">
                Recurring job. (A job cannot be converted between one-time and recurring.)
              </p>
            )}

            <div className="mt-4 grid gap-4">
              {s.scheduleType === "once" && (
                <>
                  <label className="flex items-center gap-2 text-sm">
                    <input
                      type="checkbox"
                      checked={s.runImmediately}
                      onChange={(e) => set("runImmediately", e.target.checked)}
                      className="h-4 w-4 accent-[rgb(var(--accent))]"
                    />
                    Run as soon as possible
                  </label>
                  {!s.runImmediately && (
                    <Field
                      label="Run at"
                      required
                      error={errors.run_at}
                      hint={`In your local time (${localTimeZone()}).`}
                    >
                      {(p) => (
                        <Input
                          {...p}
                          type="datetime-local"
                          value={s.runAtLocal}
                          onChange={(e) => set("runAtLocal", e.target.value)}
                          className="max-w-xs"
                        />
                      )}
                    </Field>
                  )}
                </>
              )}
              {s.scheduleType === "delayed" && (
                <Field
                  label="Run after"
                  required
                  error={errors.delay_seconds}
                  hint="Counted from when you create the job."
                >
                  {(p) => (
                    <div className="flex max-w-xs gap-2">
                      <Input
                        {...p}
                        inputMode="numeric"
                        value={s.delayAmount}
                        onChange={(e) => set("delayAmount", e.target.value)}
                        className="w-24"
                      />
                      <Select
                        value={s.delayUnit}
                        onChange={(e) => set("delayUnit", e.target.value as DelayUnit)}
                        aria-label="Delay unit"
                        className="flex-1"
                      >
                        {Object.keys(DELAY_UNITS).map((u) => (
                          <option key={u} value={u}>
                            {u}
                          </option>
                        ))}
                      </Select>
                    </div>
                  )}
                </Field>
              )}
              {s.scheduleType === "cron" && (
                <>
                  <div className="grid gap-4 sm:grid-cols-2">
                    <Field
                      label="Cron expression"
                      required
                      error={errors.cron_expr}
                      hint="minute hour day-of-month month day-of-week"
                    >
                      {(p) => (
                        <Input
                          {...p}
                          value={s.cronExpr}
                          onChange={(e) => set("cronExpr", e.target.value)}
                          className="font-mono"
                          spellCheck={false}
                        />
                      )}
                    </Field>
                    <Field
                      label="Timezone"
                      error={errors.timezone}
                      hint="Daylight-saving changes follow this zone."
                    >
                      {(p) => (
                        <Select {...p} value={s.timezone} onChange={(e) => set("timezone", e.target.value)}>
                          {zones.map((z) => (
                            <option key={z} value={z}>
                              {z}
                            </option>
                          ))}
                        </Select>
                      )}
                    </Field>
                  </div>
                  <div className="flex flex-wrap gap-1.5" aria-label="Cron presets">
                    {CRON_PRESETS.map((p) => (
                      <button
                        key={p.expr}
                        type="button"
                        onClick={() => set("cronExpr", p.expr)}
                        className={cn(
                          "rounded-md px-2 py-1 text-xs ring-1 ring-inset",
                          s.cronExpr === p.expr
                            ? "bg-accent-soft text-accent-soft-fg ring-accent/30"
                            : "text-fg-muted ring-line hover:bg-surface-2",
                        )}
                      >
                        {p.label}
                      </button>
                    ))}
                  </div>
                  <CronPreview expr={s.cronExpr} timezone={s.timezone} />
                  <div className="grid gap-4 sm:grid-cols-2">
                    <Field
                      label="Missed runs"
                      hint="What to do with occurrences missed during downtime or while paused."
                    >
                      {(p) => (
                        <Select
                          {...p}
                          value={s.missedRunPolicy}
                          onChange={(e) => set("missedRunPolicy", e.target.value as MissedRunPolicy)}
                        >
                          <option value="run_once">Run once to catch up</option>
                          <option value="run_all">Run every missed occurrence (max 50)</option>
                          <option value="skip">Skip missed occurrences</option>
                        </Select>
                      )}
                    </Field>
                    <Field
                      label="If the previous run is still active"
                      hint="Prevents or allows overlapping runs."
                    >
                      {(p) => (
                        <Select
                          {...p}
                          value={s.overlapPolicy}
                          onChange={(e) => set("overlapPolicy", e.target.value as OverlapPolicy)}
                        >
                          <option value="skip">Skip the new occurrence</option>
                          <option value="queue">Queue it and run after</option>
                          <option value="allow">Run concurrently</option>
                        </Select>
                      )}
                    </Field>
                  </div>
                  {s.overlapPolicy === "allow" && (
                    <Field
                      label="Max concurrent runs"
                      error={errors.max_concurrency}
                      hint="Upper bound on simultaneous runs of this job."
                    >
                      {(p) => (
                        <Input
                          {...p}
                          inputMode="numeric"
                          value={s.maxConcurrency}
                          onChange={(e) => set("maxConcurrency", e.target.value)}
                          className="max-w-[120px]"
                        />
                      )}
                    </Field>
                  )}
                </>
              )}
            </div>
          </Panel>

          <Panel bodyClassName="p-5">
            <SectionTitle
              icon={<Timer />}
              title="Execution & retries"
              description="Defaults come from workspace settings."
            />
            <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
              <Field label="Priority" error={errors.priority} hint="10 runs first.">
                {(p) => (
                  <Select {...p} value={s.priority} onChange={(e) => set("priority", e.target.value)}>
                    {Array.from({ length: 10 }, (_, i) => 10 - i).map((n) => (
                      <option key={n} value={n}>
                        {n}
                        {n === 10 ? " (highest)" : n === 1 ? " (lowest)" : ""}
                      </option>
                    ))}
                  </Select>
                )}
              </Field>
              <Field
                label="Timeout (seconds)"
                error={errors.timeout_seconds}
                hint={intIn(s.timeout, 1, 86400) ? formatSeconds(Number(s.timeout)) : undefined}
              >
                {(p) => (
                  <Input
                    {...p}
                    inputMode="numeric"
                    value={s.timeout}
                    onChange={(e) => set("timeout", e.target.value)}
                  />
                )}
              </Field>
              <Field label="Max attempts" error={errors.max_attempts} hint="Including the first.">
                {(p) => (
                  <Input
                    {...p}
                    inputMode="numeric"
                    value={s.maxAttempts}
                    onChange={(e) => set("maxAttempts", e.target.value)}
                  />
                )}
              </Field>
              <div />
              <Field label="Base retry delay (s)" error={errors.backoff_base_seconds}>
                {(p) => (
                  <Input
                    {...p}
                    inputMode="numeric"
                    value={s.backoffBase}
                    onChange={(e) => set("backoffBase", e.target.value)}
                  />
                )}
              </Field>
              <Field label="Max retry delay (s)" error={errors.backoff_max_seconds}>
                {(p) => (
                  <Input
                    {...p}
                    inputMode="numeric"
                    value={s.backoffMax}
                    onChange={(e) => set("backoffMax", e.target.value)}
                  />
                )}
              </Field>
            </div>
            <div className="mt-4">
              <BackoffPreview
                attempts={Number(s.maxAttempts)}
                base={Number(s.backoffBase)}
                max={Number(s.backoffMax)}
              />
            </div>
          </Panel>

          <div className="flex justify-end gap-2 pb-6">
            <Button variant="secondary" onClick={() => navigate(editing ? `/jobs/${id}` : "/jobs")}>
              Cancel
            </Button>
            <Button variant="primary" onClick={review} disabledReason={writeBlocked}>
              Review
            </Button>
          </div>
        </div>
      ) : (
        <div className="space-y-5">
          <Panel
            title="Review"
            description="Check the configuration before saving."
            bodyClassName="p-5 space-y-6"
          >
            <DescriptionList
              items={[
                { label: "Name", value: s.name },
                { label: "Handler", value: <code className="text-xs">{s.handler}</code> },
                {
                  label: "Schedule",
                  value:
                    s.scheduleType === "cron" ? (
                      <span>
                        <code className="text-xs">{s.cronExpr}</code> — {describeCron(s.cronExpr)} (
                        {s.timezone})
                      </span>
                    ) : s.scheduleType === "delayed" ? (
                      `In ${s.delayAmount} ${s.delayUnit}`
                    ) : s.runImmediately ? (
                      "As soon as possible"
                    ) : (
                      formatDateTime(new Date(s.runAtLocal).toISOString())
                    ),
                },
                ...(s.scheduleType === "cron"
                  ? [
                      {
                        label: "Missed runs",
                        value: {
                          run_once: "Run once to catch up",
                          run_all: "Run every missed occurrence",
                          skip: "Skip",
                        }[s.missedRunPolicy],
                      },
                      {
                        label: "Overlap",
                        value: {
                          skip: "Skip new occurrence",
                          queue: "Queue and run after",
                          allow: `Run concurrently (max ${s.maxConcurrency})`,
                        }[s.overlapPolicy],
                      },
                    ]
                  : []),
                { label: "Priority", value: s.priority },
                { label: "Timeout", value: formatSeconds(Number(s.timeout)) },
                {
                  label: "Retries",
                  value: `${Number(s.maxAttempts) - 1} retr${Number(s.maxAttempts) - 1 === 1 ? "y" : "ies"}, backoff ${formatSeconds(Number(s.backoffBase))} → ${formatSeconds(Number(s.backoffMax))}`,
                },
                ...(s.description ? [{ label: "Description", value: s.description, full: true }] : []),
              ]}
            />
            <div>
              <div className="mb-2 text-xs text-fg-subtle">Payload</div>
              <JsonBlock value={payloadPreview} maxHeight={260} />
            </div>
            {editing && (
              <p className="flex items-start gap-2 rounded-md bg-surface-2 px-3 py-2 text-xs text-fg-muted">
                <Clock className="mt-0.5 h-3.5 w-3.5 shrink-0" />
                Saving applies to future runs. Runs already queued or executing keep their original
                configuration.
              </p>
            )}
          </Panel>
          <div className="flex justify-end gap-2 pb-6">
            <Button variant="secondary" onClick={() => setStep("edit")}>
              Back
            </Button>
            <Button
              variant="primary"
              onClick={submit}
              loading={create.isPending || update.isPending}
              disabledReason={writeBlocked}
            >
              {editing ? "Save changes" : "Create job"}
            </Button>
          </div>
        </div>
      )}
    </div>
  );
}
