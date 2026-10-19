import { useState, type FormEvent } from "react";
import { useLocation, useNavigate } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { AlertCircle } from "lucide-react";
import { ApiError, request } from "@/lib/api";
import type { Me } from "@/lib/types";
import { useSession } from "@/app/session";
import { Button, LinkButton } from "@/components/ui/button";
import { Field, Input } from "@/components/ui/input";
import { AuthLayout } from "./AuthLayout";

export default function LoginPage() {
  const { setSession } = useSession();
  const navigate = useNavigate();
  const location = useLocation();
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const cfg = useQuery({
    queryKey: ["auth-config"],
    queryFn: () => request<{ registration_enabled: boolean }>("/auth/config").then((r) => r.data),
  });

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setError(null);
    if (!email || !password) {
      setError("Enter your email and password.");
      return;
    }
    setBusy(true);
    try {
      const r = await request<Me>("/auth/login", {
        method: "POST",
        body: { email, password },
        quiet401: true,
      });
      setSession(r.data);
      const from = (location.state as { from?: string } | null)?.from;
      navigate(from && from !== "/login" ? from : "/", { replace: true });
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Sign-in failed. Try again.");
    } finally {
      setBusy(false);
    }
  };

  return (
    <AuthLayout
      title="Sign in"
      subtitle="Welcome back to TaskFlow."
      footer={cfg.data?.registration_enabled === false ? "Ask a workspace admin for an account." : null}
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
        <Field label="Email">
          {(p) => (
            <Input
              {...p}
              type="email"
              autoComplete="email"
              autoFocus
              value={email}
              onChange={(e) => setEmail(e.target.value)}
            />
          )}
        </Field>
        <Field label="Password">
          {(p) => (
            <Input
              {...p}
              type="password"
              autoComplete="current-password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
            />
          )}
        </Field>
        <Button type="submit" variant="primary" className="w-full justify-center" loading={busy}>
          Sign in
        </Button>
        {cfg.data?.registration_enabled !== false && (
          <>
            <div className="flex items-center gap-3 text-2xs uppercase tracking-wide text-fg-faint">
              <span className="h-px flex-1 bg-line" />
              New here?
              <span className="h-px flex-1 bg-line" />
            </div>
            <LinkButton to="/register" className="w-full justify-center">
              Create a free account
            </LinkButton>
            <p className="text-center text-xs text-fg-subtle">
              You get your own private workspace to create and run jobs.
            </p>
          </>
        )}
      </form>
    </AuthLayout>
  );
}
