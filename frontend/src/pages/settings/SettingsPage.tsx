import { useEffect, useMemo, useState, type FormEvent } from "react";
import { useSearchParams } from "react-router-dom";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Monitor, Moon, Sun, UserPlus } from "lucide-react";
import { toast } from "sonner";
import { usePageMeta } from "@/app/page-meta";
import { ADMIN_REASON, roleCopy, useSession } from "@/app/session";
import { useTheme, type ThemePref } from "@/app/theme";
import { useAudit, useSettings, useUsers } from "@/api/hooks";
import { ApiError, api, errorMessage } from "@/lib/api";
import type { Role, Settings, User } from "@/lib/types";
import { formatDateTime, formatSeconds, timeZones } from "@/lib/format";
import { cn } from "@/lib/utils";
import { Button } from "@/components/ui/button";
import { Field, Input, Select } from "@/components/ui/input";
import { PageHeader, Panel } from "@/components/ui/layout";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Dialog } from "@/components/ui/dialog";
import { Badge } from "@/components/ui/badge";
import { Table, TD, TH, THead, TR } from "@/components/ui/table";
import { Pagination } from "@/components/ui/pagination";
import { EmptyState, ErrorState, Skeleton, TableSkeleton } from "@/components/ui/feedback";
import { RelTime } from "@/components/common/Time";
import { passwordProblem } from "@/lib/validation";

type NumKey = Exclude<keyof Settings, "default_timezone" | "updated_at">;

function DefaultsTab() {
  const { can } = useSession();
  const q = useSettings();
  const qc = useQueryClient();
  const zones = useMemo(timeZones, []);
  const [form, setForm] = useState<Record<string, string> | null>(null);
  const [errors, setErrors] = useState<Record<string, string>>({});
  useEffect(() => {
    if (q.data && !form) setForm(Object.fromEntries(Object.entries(q.data).map(([k, v]) => [k, String(v)])));
  }, [q.data, form]);
  const save = useMutation({
    mutationFn: (s: Settings) => api.put<Settings>("/settings", s),
    onSuccess: (s) => {
      qc.setQueryData(["settings"], s);
      toast.success("Settings saved", { description: "New jobs will use these defaults." });
      setErrors({});
    },
    onError: (e) => {
      if (e instanceof ApiError && Object.keys(e.fields).length) setErrors(e.fields);
      toast.error(errorMessage(e));
    },
  });
  if (q.error) return <ErrorState error={q.error} onRetry={() => q.refetch()} />;
  if (!form) return <Skeleton className="h-80 w-full" />;
  const admin = can("admin");
  const num = (k: NumKey, label: string, hint?: string) => (
    <Field label={label} error={errors[k]} hint={hint}>
      {(p) => (
        <Input
          {...p}
          inputMode="numeric"
          disabled={!admin}
          value={form[k]}
          onChange={(e) => setForm({ ...form, [k]: e.target.value })}
        />
      )}
    </Field>
  );
  const submit = (e: FormEvent) => {
    e.preventDefault();
    const out = { ...form } as Record<string, string | number>;
    for (const k of Object.keys(out))
      if (k !== "default_timezone" && k !== "updated_at") out[k] = Number(out[k]);
    delete out.updated_at;
    save.mutate(out as unknown as Settings);
  };
  return (
    <form onSubmit={submit} className="space-y-5">
      {!admin && (
        <p className="rounded-md bg-surface-2 px-3 py-2 text-sm text-fg-muted">
          {ADMIN_REASON} You can view the current values.
        </p>
      )}
      <Panel
        title="Job defaults"
        description="Applied when a new job does not specify a value."
        bodyClassName="p-5 grid gap-4 sm:grid-cols-2 lg:grid-cols-3"
      >
        {num("default_max_attempts", "Max attempts", "1–25, including the first attempt")}
        {num(
          "default_timeout_seconds",
          "Timeout (seconds)",
          form.default_timeout_seconds ? formatSeconds(Number(form.default_timeout_seconds) || 0) : undefined,
        )}
        {num("default_priority", "Priority", "1 (lowest) – 10 (highest)")}
        {num("default_backoff_base_seconds", "Base retry delay (seconds)")}
        {num("default_backoff_max_seconds", "Max retry delay (seconds)")}
        <Field label="Default timezone" error={errors.default_timezone} hint="Used for new cron schedules.">
          {(p) => (
            <Select
              {...p}
              disabled={!admin}
              value={form.default_timezone}
              onChange={(e) => setForm({ ...form, default_timezone: e.target.value })}
            >
              {zones.map((z) => (
                <option key={z} value={z}>
                  {z}
                </option>
              ))}
            </Select>
          )}
        </Field>
      </Panel>
      <Panel title="Limits & retention" bodyClassName="p-5 grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
        {num(
          "max_concurrent_runs",
          "Workspace concurrency limit",
          "Max runs executing at once across the workspace. 0 = unlimited.",
        )}
        {num(
          "event_retention_days",
          "Event retention (days)",
          "Operational events older than this are deleted. Audit logs are kept.",
        )}
      </Panel>
      <div className="flex items-center justify-between gap-3">
        <span className="text-xs text-fg-subtle">
          {q.data?.updated_at && (
            <>
              Last updated <RelTime iso={q.data.updated_at} />
            </>
          )}
        </span>
        <Button
          type="submit"
          variant="primary"
          loading={save.isPending}
          disabledReason={admin ? undefined : ADMIN_REASON}
        >
          Save settings
        </Button>
      </div>
    </form>
  );
}

function AddMemberDialog({ open, onOpenChange }: { open: boolean; onOpenChange: (o: boolean) => void }) {
  const qc = useQueryClient();
  const [f, setF] = useState({ name: "", email: "", password: "", role: "operator" as Role });
  const [errors, setErrors] = useState<Record<string, string>>({});
  const create = useMutation({
    mutationFn: () => api.post<User>("/users", f),
    onSuccess: (u) => {
      toast.success(`${u.name} added`, { description: "Share the initial password with them securely." });
      qc.invalidateQueries({ queryKey: ["users"] });
      setF({ name: "", email: "", password: "", role: "operator" });
      onOpenChange(false);
    },
    onError: (e) => {
      if (e instanceof ApiError && Object.keys(e.fields).length) setErrors(e.fields);
      else toast.error(errorMessage(e));
    },
  });
  const submit = () => {
    const local: Record<string, string> = {};
    if (!f.name.trim()) local.name = "Enter a name.";
    if (!/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(f.email)) local.email = "Enter a valid email address.";
    const pw = passwordProblem(f.password);
    if (pw) local.password = pw;
    setErrors(local);
    if (!Object.keys(local).length) create.mutate();
  };
  return (
    <Dialog
      open={open}
      onOpenChange={onOpenChange}
      title="Add member"
      description="Members sign in with this email and initial password."
      footer={
        <>
          <Button onClick={() => onOpenChange(false)}>Cancel</Button>
          <Button variant="primary" loading={create.isPending} onClick={submit}>
            Add member
          </Button>
        </>
      }
    >
      <div className="space-y-4">
        <Field label="Name" error={errors.name} required>
          {(p) => <Input {...p} value={f.name} onChange={(e) => setF({ ...f, name: e.target.value })} />}
        </Field>
        <Field label="Email" error={errors.email} required>
          {(p) => (
            <Input
              {...p}
              type="email"
              value={f.email}
              onChange={(e) => setF({ ...f, email: e.target.value })}
            />
          )}
        </Field>
        <Field
          label="Initial password"
          error={errors.password}
          hint="At least 10 characters, with a letter and a number or symbol."
          required
        >
          {(p) => (
            <Input
              {...p}
              type="password"
              autoComplete="new-password"
              value={f.password}
              onChange={(e) => setF({ ...f, password: e.target.value })}
            />
          )}
        </Field>
        <Field label="Role" error={errors.role} hint={roleCopy[f.role]}>
          {(p) => (
            <Select {...p} value={f.role} onChange={(e) => setF({ ...f, role: e.target.value as Role })}>
              <option value="viewer">Viewer</option>
              <option value="operator">Operator</option>
              <option value="admin">Admin</option>
            </Select>
          )}
        </Field>
      </div>
    </Dialog>
  );
}

function MembersTab() {
  const { me, can } = useSession();
  const q = useUsers(can("admin"));
  const qc = useQueryClient();
  const [adding, setAdding] = useState(false);
  const update = useMutation({
    mutationFn: ({ id, body }: { id: string; body: { role?: Role; disabled?: boolean } }) =>
      api.patch<User>(`/users/${id}`, body),
    onSuccess: () => {
      toast.success("Member updated");
      qc.invalidateQueries({ queryKey: ["users"] });
    },
    onError: (e) => toast.error(errorMessage(e)),
  });
  if (!can("admin")) return <EmptyState title="Admins only" description={ADMIN_REASON} />;
  return (
    <Panel
      title="Members"
      description="Roles: admin (everything), operator (manage jobs), viewer (read-only)."
      actions={
        <Button
          size="sm"
          variant="primary"
          icon={<UserPlus className="h-3.5 w-3.5" />}
          onClick={() => setAdding(true)}
        >
          Add member
        </Button>
      }
    >
      {q.isLoading ? (
        <TableSkeleton rows={3} cols={4} />
      ) : q.error ? (
        <ErrorState error={q.error} onRetry={() => q.refetch()} />
      ) : (
        <Table minWidth={640}>
          <THead>
            <tr>
              <TH>Member</TH>
              <TH>Role</TH>
              <TH>Last sign-in</TH>
              <TH align="right">Access</TH>
            </tr>
          </THead>
          <tbody>
            {(q.data ?? []).map((u) => {
              const self = u.id === me?.user.id;
              return (
                <TR key={u.id}>
                  <TD>
                    <div className="font-medium">
                      {u.name}
                      {self && <span className="ml-1.5 text-xs font-normal text-fg-subtle">(you)</span>}
                    </div>
                    <div className="text-xs text-fg-subtle">{u.email}</div>
                  </TD>
                  <TD>
                    <Select
                      aria-label={`Role for ${u.name}`}
                      value={u.role}
                      disabled={self || update.isPending}
                      title={self ? "You cannot change your own role." : undefined}
                      onChange={(e) => update.mutate({ id: u.id, body: { role: e.target.value as Role } })}
                      className="w-32"
                    >
                      <option value="viewer">Viewer</option>
                      <option value="operator">Operator</option>
                      <option value="admin">Admin</option>
                    </Select>
                  </TD>
                  <TD className="text-xs text-fg-muted">
                    {u.last_login_at ? <RelTime iso={u.last_login_at} /> : "Never"}
                  </TD>
                  <TD align="right">
                    {u.disabled_at ? (
                      <span className="inline-flex items-center gap-2">
                        <Badge tone="muted">Disabled</Badge>
                        <Button
                          size="sm"
                          onClick={() => update.mutate({ id: u.id, body: { disabled: false } })}
                        >
                          Enable
                        </Button>
                      </span>
                    ) : (
                      <Button
                        size="sm"
                        variant="ghost"
                        disabledReason={self ? "You cannot disable your own account." : undefined}
                        onClick={() => update.mutate({ id: u.id, body: { disabled: true } })}
                      >
                        Disable
                      </Button>
                    )}
                  </TD>
                </TR>
              );
            })}
          </tbody>
        </Table>
      )}
      <AddMemberDialog open={adding} onOpenChange={setAdding} />
    </Panel>
  );
}

function AuditTab() {
  const { can } = useSession();
  const [action, setAction] = useState("");
  const [page, setPage] = useState(1);
  const q = useAudit({ action: action || undefined, page }, can("admin"));
  if (!can("admin")) return <EmptyState title="Admins only" description={ADMIN_REASON} />;
  return (
    <Panel
      title="Audit log"
      description="Security-relevant actions: sign-ins, job changes, cancellations, replays, settings and membership."
      actions={
        <Select
          aria-label="Filter by action"
          value={action}
          onChange={(e) => {
            setAction(e.target.value);
            setPage(1);
          }}
          className="w-44"
        >
          <option value="">All actions</option>
          <option value="auth.">Authentication</option>
          <option value="job.">Jobs</option>
          <option value="run.">Runs & replays</option>
          <option value="settings.">Settings</option>
          <option value="user.">Members</option>
          <option value="worker.">Workers</option>
        </Select>
      }
    >
      {q.isLoading ? (
        <TableSkeleton rows={8} cols={4} />
      ) : q.error ? (
        <ErrorState error={q.error} onRetry={() => q.refetch()} />
      ) : (q.data?.data ?? []).length === 0 ? (
        <EmptyState title="No audit events" />
      ) : (
        <>
          <Table minWidth={720}>
            <THead>
              <tr>
                <TH>When</TH>
                <TH>Actor</TH>
                <TH>Action</TH>
                <TH>Target</TH>
                <TH>IP</TH>
              </tr>
            </THead>
            <tbody>
              {q.data!.data.map((a) => (
                <TR key={a.id}>
                  <TD className="tabular whitespace-nowrap text-xs text-fg-muted">
                    {formatDateTime(a.created_at)}
                  </TD>
                  <TD className="text-sm">
                    {a.actor_email ?? <span className="text-fg-subtle">anonymous</span>}
                  </TD>
                  <TD>
                    <code className={cn("text-xs", a.action.includes("failed") && "text-danger")}>
                      {a.action}
                    </code>
                  </TD>
                  <TD
                    className="max-w-[220px] truncate font-mono text-xs text-fg-subtle"
                    title={a.target_id ?? ""}
                  >
                    {a.target_type}
                    {a.target_id ? `:${a.target_id.slice(0, 13)}` : ""}
                  </TD>
                  <TD className="font-mono text-xs text-fg-subtle">{a.ip ?? "—"}</TD>
                </TR>
              ))}
            </tbody>
          </Table>
          <Pagination meta={q.data?.meta} onPage={setPage} />
        </>
      )}
    </Panel>
  );
}

function AccountTab() {
  const { me } = useSession();
  const [f, setF] = useState({ current_password: "", new_password: "" });
  const [errors, setErrors] = useState<Record<string, string>>({});
  const change = useMutation({
    mutationFn: () => api.post("/auth/password", f),
    onSuccess: () => {
      toast.success("Password changed", { description: "Other sessions were signed out." });
      setF({ current_password: "", new_password: "" });
      setErrors({});
    },
    onError: (e) => {
      if (e instanceof ApiError && Object.keys(e.fields).length) setErrors(e.fields);
      else toast.error(errorMessage(e));
    },
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    const pw = passwordProblem(f.new_password);
    if (pw) return setErrors({ new_password: pw });
    change.mutate();
  };
  return (
    <div className="grid grid-cols-1 gap-5 lg:grid-cols-2">
      <Panel title="Profile" bodyClassName="p-5 space-y-3 text-sm">
        <div>
          <span className="text-fg-subtle">Name: </span>
          {me?.user.name}
        </div>
        <div>
          <span className="text-fg-subtle">Email: </span>
          {me?.user.email}
        </div>
        <div>
          <span className="text-fg-subtle">Role: </span>
          <span className="capitalize">{me?.user.role}</span>{" "}
          <span className="text-xs text-fg-subtle">— {me && roleCopy[me.user.role]}</span>
        </div>
        <div>
          <span className="text-fg-subtle">Workspace: </span>
          {me?.workspace.name}
        </div>
      </Panel>
      <Panel title="Change password" bodyClassName="p-5">
        <form onSubmit={submit} className="space-y-4">
          <Field label="Current password" error={errors.current_password}>
            {(p) => (
              <Input
                {...p}
                type="password"
                autoComplete="current-password"
                value={f.current_password}
                onChange={(e) => setF({ ...f, current_password: e.target.value })}
              />
            )}
          </Field>
          <Field
            label="New password"
            error={errors.new_password}
            hint="At least 10 characters, with a letter and a number or symbol."
          >
            {(p) => (
              <Input
                {...p}
                type="password"
                autoComplete="new-password"
                value={f.new_password}
                onChange={(e) => setF({ ...f, new_password: e.target.value })}
              />
            )}
          </Field>
          <Button
            type="submit"
            variant="primary"
            loading={change.isPending}
            disabled={!f.current_password || !f.new_password}
          >
            Update password
          </Button>
        </form>
      </Panel>
    </div>
  );
}

function AppearanceTab() {
  const { pref, setPref } = useTheme();
  const opts: { v: ThemePref; label: string; icon: typeof Sun }[] = [
    { v: "light", label: "Light", icon: Sun },
    { v: "dark", label: "Dark", icon: Moon },
    { v: "system", label: "System", icon: Monitor },
  ];
  return (
    <Panel title="Theme" description="Stored in this browser." bodyClassName="p-5">
      <div role="radiogroup" aria-label="Theme" className="grid max-w-md grid-cols-3 gap-2">
        {opts.map((o) => (
          <button
            key={o.v}
            role="radio"
            aria-checked={pref === o.v}
            onClick={() => setPref(o.v)}
            className={cn(
              "flex flex-col items-center gap-2 rounded-md border p-3 text-sm",
              pref === o.v ? "border-accent bg-accent-soft/40 ring-1 ring-accent" : "hover:bg-surface-2",
            )}
          >
            <o.icon className="h-4 w-4" />
            {o.label}
          </button>
        ))}
      </div>
    </Panel>
  );
}

const TABS = ["defaults", "members", "audit", "account", "appearance"] as const;

export default function SettingsPage() {
  usePageMeta([{ label: "Settings" }]);
  const [params, setParams] = useSearchParams();
  const tab = (TABS as readonly string[]).includes(params.get("tab") ?? "") ? params.get("tab")! : "defaults";
  return (
    <>
      <PageHeader
        title="Settings"
        description="Workspace defaults, members, audit trail and personal preferences."
      />
      <Tabs
        value={tab}
        onValueChange={(v) => setParams(v === "defaults" ? {} : { tab: v }, { replace: true })}
      >
        <TabsList className="mb-5">
          <TabsTrigger value="defaults">Execution defaults</TabsTrigger>
          <TabsTrigger value="members">Members</TabsTrigger>
          <TabsTrigger value="audit">Audit log</TabsTrigger>
          <TabsTrigger value="account">Account</TabsTrigger>
          <TabsTrigger value="appearance">Appearance</TabsTrigger>
        </TabsList>
        <TabsContent value="defaults">
          <DefaultsTab />
        </TabsContent>
        <TabsContent value="members">
          <MembersTab />
        </TabsContent>
        <TabsContent value="audit">
          <AuditTab />
        </TabsContent>
        <TabsContent value="account">
          <AccountTab />
        </TabsContent>
        <TabsContent value="appearance">
          <AppearanceTab />
        </TabsContent>
      </Tabs>
    </>
  );
}
