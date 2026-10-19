import { createContext, useContext, useEffect, type ReactNode } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError, onUnauthorized, request, setCsrfToken } from "@/lib/api";
import type { Me, Role } from "@/lib/types";

interface SessionValue {
  me: Me | null;
  loading: boolean;
  error: unknown;
  refetch: () => void;
  setSession: (me: Me | null) => void;
  can: (action: "write" | "admin") => boolean;
}

const SessionContext = createContext<SessionValue | null>(null);

export function useSession(): SessionValue {
  const v = useContext(SessionContext);
  if (!v) throw new Error("useSession outside SessionProvider");
  return v;
}

/** Convenience: the signed-in user's role permissions. */
export function useCan() {
  return useSession().can;
}

async function fetchMe(): Promise<Me | null> {
  try {
    const r = await request<Me>("/auth/me", { quiet401: true });
    setCsrfToken(r.data.csrf_token);
    return r.data;
  } catch (e) {
    if (e instanceof ApiError && e.status === 401) {
      setCsrfToken(null);
      return null;
    }
    throw e;
  }
}

export function SessionProvider({ children }: { children: ReactNode }) {
  const qc = useQueryClient();
  const q = useQuery({ queryKey: ["me"], queryFn: fetchMe, staleTime: 5 * 60_000, retry: 1 });

  useEffect(
    () =>
      // Any 401 from another request means the session expired or was revoked.
      onUnauthorized(() => {
        setCsrfToken(null);
        qc.setQueryData(["me"], null);
      }),
    [qc],
  );

  const me = q.data ?? null;
  const role: Role | undefined = me?.user.role;
  const value: SessionValue = {
    me,
    loading: q.isLoading,
    error: q.error,
    refetch: () => void q.refetch(),
    setSession: (m) => {
      setCsrfToken(m?.csrf_token ?? null);
      if (!m) qc.clear();
      qc.setQueryData(["me"], m);
    },
    can: (action) => (action === "admin" ? role === "admin" : role === "admin" || role === "operator"),
  };
  return <SessionContext.Provider value={value}>{children}</SessionContext.Provider>;
}

export const roleCopy: Record<Role, string> = {
  admin: "Full access, including settings, members and the audit log.",
  operator: "Can create and manage jobs, runs and the dead-letter queue.",
  viewer: "Read-only access.",
};

/** Explanation shown on disabled write actions for viewers. */
export const VIEWER_REASON = "Your role is read-only. Ask an admin for operator access.";
export const ADMIN_REASON = "Only workspace admins can do this.";
