import { lazy, Suspense, type ReactNode } from "react";
import { createBrowserRouter, Navigate, RouterProvider, useLocation, useRouteError } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { Toaster } from "sonner";
import { ApiError } from "@/lib/api";
import { SessionProvider, useSession } from "@/app/session";
import { ThemeProvider, useTheme } from "@/app/theme";
import { TooltipProvider } from "@/components/ui/tooltip";
import { AppShell } from "@/components/shell/AppShell";
import { ErrorState, Spinner } from "@/components/ui/feedback";
import { Button } from "@/components/ui/button";

const LoginPage = lazy(() => import("@/pages/auth/LoginPage"));
const RegisterPage = lazy(() => import("@/pages/auth/RegisterPage"));
const OverviewPage = lazy(() => import("@/pages/overview/OverviewPage"));
const JobsPage = lazy(() => import("@/pages/jobs/JobsPage"));
const JobFormPage = lazy(() => import("@/pages/jobs/JobFormPage"));
const JobDetailPage = lazy(() => import("@/pages/jobs/JobDetailPage"));
const SchedulesPage = lazy(() => import("@/pages/schedules/SchedulesPage"));
const ExecutionsPage = lazy(() => import("@/pages/executions/ExecutionsPage"));
const WorkersPage = lazy(() => import("@/pages/workers/WorkersPage"));
const WorkerDetailPage = lazy(() => import("@/pages/workers/WorkerDetailPage"));
const DeadLetterPage = lazy(() => import("@/pages/deadletter/DeadLetterPage"));
const HealthPage = lazy(() => import("@/pages/health/HealthPage"));
const SettingsPage = lazy(() => import("@/pages/settings/SettingsPage"));
const NotFoundPage = lazy(() => import("@/pages/NotFoundPage"));

export function createQueryClient() {
  return new QueryClient({
    defaultOptions: {
      queries: {
        staleTime: 10_000,
        refetchOnWindowFocus: true,
        retry: (count, err) =>
          !(err instanceof ApiError && err.status >= 400 && err.status < 500) && count < 2,
      },
      mutations: { retry: false },
    },
  });
}

function FullPageSpinner() {
  return (
    <div className="flex min-h-[50vh] items-center justify-center">
      <Spinner />
    </div>
  );
}

function RequireAuth({ children }: { children: ReactNode }) {
  const { me, loading, error, refetch } = useSession();
  const location = useLocation();
  if (loading) return <FullPageSpinner />;
  if (error && !me) return <ErrorState error={error} onRetry={refetch} className="min-h-dvh" />;
  if (!me) return <Navigate to="/login" replace state={{ from: location.pathname + location.search }} />;
  return <>{children}</>;
}

function RedirectIfAuthed({ children }: { children: ReactNode }) {
  const { me, loading } = useSession();
  if (loading) return <FullPageSpinner />;
  if (me) return <Navigate to="/" replace />;
  return <>{children}</>;
}

function RouteError() {
  const error = useRouteError();
  return (
    <div className="flex min-h-dvh flex-col items-center justify-center gap-2">
      <ErrorState error={error instanceof Error ? error : new Error("This page failed to load.")} />
      <Button onClick={() => window.location.assign("/")}>Back to overview</Button>
    </div>
  );
}

const page = (el: ReactNode) => <Suspense fallback={<FullPageSpinner />}>{el}</Suspense>;

const router = createBrowserRouter([
  {
    path: "/login",
    element: page(
      <RedirectIfAuthed>
        <LoginPage />
      </RedirectIfAuthed>,
    ),
    errorElement: <RouteError />,
  },
  {
    path: "/register",
    element: page(
      <RedirectIfAuthed>
        <RegisterPage />
      </RedirectIfAuthed>,
    ),
    errorElement: <RouteError />,
  },
  {
    path: "/",
    element: (
      <RequireAuth>
        <AppShell />
      </RequireAuth>
    ),
    errorElement: <RouteError />,
    children: [
      { index: true, element: page(<OverviewPage />) },
      { path: "jobs", element: page(<JobsPage />) },
      { path: "jobs/new", element: page(<JobFormPage />) },
      { path: "jobs/:id", element: page(<JobDetailPage />) },
      { path: "jobs/:id/edit", element: page(<JobFormPage />) },
      { path: "schedules", element: page(<SchedulesPage />) },
      { path: "executions", element: page(<ExecutionsPage />) },
      { path: "workers", element: page(<WorkersPage />) },
      { path: "workers/:id", element: page(<WorkerDetailPage />) },
      { path: "dead-letter", element: page(<DeadLetterPage />) },
      { path: "health", element: page(<HealthPage />) },
      { path: "settings", element: page(<SettingsPage />) },
      { path: "*", element: page(<NotFoundPage />) },
    ],
  },
]);

function ThemedToaster() {
  const { resolved } = useTheme();
  return (
    <Toaster
      theme={resolved}
      position="bottom-right"
      closeButton
      toastOptions={{
        classNames: {
          toast: "!rounded-lg !border-line !bg-surface !text-fg !shadow-pop",
          description: "!text-fg-subtle",
        },
      }}
    />
  );
}

const queryClient = createQueryClient();

export default function App() {
  return (
    <ThemeProvider>
      <QueryClientProvider client={queryClient}>
        <TooltipProvider>
          <SessionProvider>
            <RouterProvider router={router} />
            <ThemedToaster />
          </SessionProvider>
        </TooltipProvider>
      </QueryClientProvider>
    </ThemeProvider>
  );
}
