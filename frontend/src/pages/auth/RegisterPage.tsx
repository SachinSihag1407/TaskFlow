import { useState, type FormEvent } from "react";
import { Link, useNavigate } from "react-router-dom";
import { AlertCircle } from "lucide-react";
import { ApiError, request } from "@/lib/api";
import type { Me } from "@/lib/types";
import { useSession } from "@/app/session";
import { Button } from "@/components/ui/button";
import { Field, Input } from "@/components/ui/input";
import { passwordProblem } from "@/lib/validation";
import { AuthLayout } from "./AuthLayout";

export default function RegisterPage() {
  const { setSession } = useSession();
  const navigate = useNavigate();
  const [form, setForm] = useState({ name: "", email: "", password: "", workspace: "" });
  const [fields, setFields] = useState<Record<string, string>>({});
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const set = (k: keyof typeof form) => (e: React.ChangeEvent<HTMLInputElement>) =>
    setForm((f) => ({ ...f, [k]: e.target.value }));

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setError(null);
    const local: Record<string, string> = {};
    if (!form.name.trim()) local.name = "Enter your name.";
    if (!/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(form.email)) local.email = "Enter a valid email address.";
    const pw = passwordProblem(form.password);
    if (pw) local.password = pw;
    setFields(local);
    if (Object.keys(local).length) return;
    setBusy(true);
    try {
      const r = await request<Me>("/auth/register", { method: "POST", body: form, quiet401: true });
      setSession(r.data);
      navigate("/", { replace: true });
    } catch (err) {
      if (err instanceof ApiError && Object.keys(err.fields).length) setFields(err.fields);
      else setError(err instanceof ApiError ? err.message : "Registration failed. Try again.");
    } finally {
      setBusy(false);
    }
  };

  return (
    <AuthLayout
      title="Create your workspace"
      subtitle="You'll be the workspace admin and can invite teammates later."
      footer={
        <>
          Already have an account?{" "}
          <Link to="/login" className="font-medium text-accent hover:underline">
            Sign in
          </Link>
        </>
      }
    >
      <form onSubmit={submit} className="space-y-4" noValidate>
        {error && (
          <div
            role="alert"
            className="flex items-start gap-2 rounded-md bg-danger-soft px-3 py-2 text-sm text-danger"
          >
            <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" />
            {error}
          </div>
        )}
        <Field label="Your name" error={fields.name} required>
          {(p) => <Input {...p} autoComplete="name" autoFocus value={form.name} onChange={set("name")} />}
        </Field>
        <Field label="Work email" error={fields.email} required>
          {(p) => (
            <Input {...p} type="email" autoComplete="email" value={form.email} onChange={set("email")} />
          )}
        </Field>
        <Field
          label="Password"
          error={fields.password}
          hint="At least 10 characters, with a letter and a number or symbol."
          required
        >
          {(p) => (
            <Input
              {...p}
              type="password"
              autoComplete="new-password"
              value={form.password}
              onChange={set("password")}
            />
          )}
        </Field>
        <Field label="Workspace name" error={fields.workspace} hint="Optional. You can rename it later.">
          {(p) => (
            <Input {...p} placeholder="Acme Engineering" value={form.workspace} onChange={set("workspace")} />
          )}
        </Field>
        <Button type="submit" variant="primary" className="w-full justify-center" loading={busy}>
          Create workspace
        </Button>
      </form>
    </AuthLayout>
  );
}
