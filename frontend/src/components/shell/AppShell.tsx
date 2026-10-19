import { useEffect, useState } from "react";
import { Outlet, useLocation } from "react-router-dom";
import * as RD from "@radix-ui/react-dialog";
import { X } from "lucide-react";
import { storage } from "@/lib/utils";
import { LiveProvider } from "@/app/live";
import { PageMetaProvider } from "@/app/page-meta";
import { Logo, SidebarNav, Sidebar, UserMenu, WorkspaceMenu } from "./Sidebar";
import { Topbar } from "./Topbar";
import { CommandPalette } from "./CommandPalette";

/** Mobile navigation: a proper drawer instead of a squeezed sidebar. */
function MobileDrawer({ open, onOpenChange }: { open: boolean; onOpenChange: (o: boolean) => void }) {
  return (
    <RD.Root open={open} onOpenChange={onOpenChange}>
      <RD.Portal>
        <RD.Overlay className="fixed inset-0 z-50 animate-fade-in bg-black/40 lg:hidden" />
        <RD.Content className="fixed inset-y-0 left-0 z-50 flex w-[280px] max-w-[85vw] animate-slide-in-left flex-col bg-surface shadow-dialog focus:outline-none lg:hidden">
          <RD.Title className="sr-only">Navigation</RD.Title>
          <RD.Description className="sr-only">Main navigation</RD.Description>
          <div className="flex h-12 items-center justify-between border-b px-3">
            <Logo />
            <RD.Close
              className="rounded p-1.5 text-fg-subtle hover:bg-surface-2"
              aria-label="Close navigation"
            >
              <X className="h-4 w-4" />
            </RD.Close>
          </div>
          <div className="p-2">
            <WorkspaceMenu />
          </div>
          <div className="flex-1 overflow-y-auto px-2">
            <SidebarNav onNavigate={() => onOpenChange(false)} />
          </div>
          <div className="border-t p-2">
            <UserMenu />
          </div>
        </RD.Content>
      </RD.Portal>
    </RD.Root>
  );
}

export function AppShell() {
  const [collapsed, setCollapsed] = useState(() => storage.get("tf-sidebar") === "collapsed");
  const [drawer, setDrawer] = useState(false);
  const [palette, setPalette] = useState(false);
  const location = useLocation();

  useEffect(() => setDrawer(false), [location.pathname]);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        setPalette((p) => !p);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  const toggle = () => {
    setCollapsed((c) => {
      storage.set("tf-sidebar", c ? "expanded" : "collapsed");
      return !c;
    });
  };

  return (
    <LiveProvider enabled>
      <PageMetaProvider>
        <a
          href="#main"
          className="sr-only focus:not-sr-only focus:fixed focus:left-2 focus:top-2 focus:z-[60] focus:rounded focus:bg-surface focus:px-3 focus:py-2 focus:shadow-pop"
        >
          Skip to content
        </a>
        <div className="flex min-h-dvh">
          <Sidebar collapsed={collapsed} onToggle={toggle} />
          <div className="flex min-w-0 flex-1 flex-col">
            <Topbar onMenu={() => setDrawer(true)} onSearch={() => setPalette(true)} />
            <main id="main" className="mx-auto w-full max-w-[1440px] flex-1 px-4 py-5 sm:px-6 lg:px-8">
              <Outlet />
            </main>
          </div>
        </div>
        <MobileDrawer open={drawer} onOpenChange={setDrawer} />
        <CommandPalette open={palette} onOpenChange={setPalette} />
      </PageMetaProvider>
    </LiveProvider>
  );
}
