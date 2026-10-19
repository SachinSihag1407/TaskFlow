import type { ReactElement } from "react";
import { render } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { TooltipProvider } from "@/components/ui/tooltip";
import { ThemeProvider } from "@/app/theme";
import { SessionProvider } from "@/app/session";
import { LiveProvider } from "@/app/live";
import { PageMetaProvider } from "@/app/page-meta";

/** Renders a page with the app's providers and an in-memory router. */
export function renderPage(
  ui: ReactElement,
  { path = "/", route = "/" }: { path?: string; route?: string } = {},
) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(
    <ThemeProvider>
      <QueryClientProvider client={qc}>
        <TooltipProvider>
          <SessionProvider>
            <LiveProvider enabled={false}>
              <PageMetaProvider>
                <MemoryRouter initialEntries={[route]}>
                  <Routes>
                    <Route path={path} element={ui} />
                    <Route path="*" element={<div>navigated</div>} />
                  </Routes>
                </MemoryRouter>
              </PageMetaProvider>
            </LiveProvider>
          </SessionProvider>
        </TooltipProvider>
      </QueryClientProvider>
    </ThemeProvider>,
  );
}
