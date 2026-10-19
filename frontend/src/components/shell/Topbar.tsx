import { Link, useNavigate } from "react-router-dom";
import { useQueryClient } from "@tanstack/react-query";
import { Bell, ChevronRight, Menu, Monitor, Moon, Search, Sun } from "lucide-react";
import { cn } from "@/lib/utils";
import { api } from "@/lib/api";
import { relativeTime } from "@/lib/format";
import { useCrumbs } from "@/app/page-meta";
import { useLiveStatus } from "@/app/live";
import { useTheme, type ThemePref } from "@/app/theme";
import { useNotifications } from "@/api/hooks";
import { Tooltip } from "@/components/ui/tooltip";
import { Kbd } from "@/components/ui/layout";
import {
  Dropdown,
  DropdownContent,
  DropdownItem,
  DropdownLabel,
  DropdownTrigger,
} from "@/components/ui/dropdown";
import * as Popover from "@radix-ui/react-popover";

function Breadcrumbs() {
  const crumbs = useCrumbs();
  return (
    <nav aria-label="Breadcrumb" className="min-w-0">
      <ol className="flex min-w-0 items-center gap-1 text-sm">
        {crumbs.map((c, i) => {
          const last = i === crumbs.length - 1;
          return (
            <li
              key={`${c.label}-${i}`}
              className={cn("flex min-w-0 items-center gap-1", !last && "hidden sm:flex")}
            >
              {i > 0 && <ChevronRight className="h-3.5 w-3.5 shrink-0 text-fg-faint" aria-hidden />}
              {c.to && !last ? (
                <Link to={c.to} className="truncate text-fg-subtle hover:text-fg">
                  {c.label}
                </Link>
              ) : (
                <span
                  className={cn("truncate", last ? "font-medium text-fg" : "text-fg-subtle")}
                  aria-current={last ? "page" : undefined}
                >
                  {c.label}
                </span>
              )}
            </li>
          );
        })}
      </ol>
    </nav>
  );
}

const liveCopy = {
  connecting: { label: "Connecting", cls: "bg-fg-faint", tip: "Connecting to live updates…" },
  live: { label: "Live", cls: "bg-success-solid", tip: "Receiving real-time updates from the server." },
  degraded: {
    label: "Polling",
    cls: "bg-warning-solid",
    tip: "Live updates are degraded (Redis unavailable). Refreshing every 5 seconds.",
  },
  offline: {
    label: "Offline",
    cls: "bg-danger-solid",
    tip: "Disconnected from live updates. Reconnecting automatically; data refreshes every 5 seconds.",
  },
};

export function LiveIndicator() {
  const status = useLiveStatus();
  const c = liveCopy[status];
  return (
    <Tooltip content={c.tip}>
      <span
        className="inline-flex h-7 items-center gap-1.5 rounded-md px-2 text-xs text-fg-subtle ring-1 ring-inset ring-line"
        role="status"
        aria-label={`Live updates: ${c.label}`}
      >
        <span className={cn("h-1.5 w-1.5 rounded-full", c.cls, status === "live" && "animate-pulse-dot")} />
        <span className="hidden md:inline">{c.label}</span>
      </span>
    </Tooltip>
  );
}

function Notifications() {
  const q = useNotifications();
  const qc = useQueryClient();
  const navigate = useNavigate();
  const unread = q.data?.unread ?? 0;
  const markSeen = async () => {
    await api.post("/notifications/seen");
    await qc.invalidateQueries({ queryKey: ["notifications"] });
  };
  return (
    <Popover.Root onOpenChange={(open) => !open && unread > 0 && void markSeen()}>
      <Popover.Trigger asChild>
        <button
          className="relative inline-flex h-8 w-8 items-center justify-center rounded-md text-fg-muted hover:bg-surface-2 hover:text-fg"
          aria-label={`Notifications${unread ? ` (${unread} unread)` : ""}`}
        >
          <Bell className="h-4 w-4" />
          {unread > 0 && (
            <span className="tabular absolute -right-0.5 -top-0.5 flex h-4 min-w-[16px] items-center justify-center rounded-full bg-danger-solid px-1 text-[10px] font-semibold text-white">
              {unread > 99 ? "99+" : unread}
            </span>
          )}
        </button>
      </Popover.Trigger>
      <Popover.Portal>
        <Popover.Content
          align="end"
          sideOffset={6}
          collisionPadding={8}
          className="z-50 w-[min(380px,calc(100vw-1rem))] animate-fade-in rounded-lg bg-surface shadow-pop"
        >
          <div className="flex items-center justify-between border-b px-3 py-2">
            <span className="text-sm font-medium">Notifications</span>
            <span className="text-2xs text-fg-subtle">Warnings and failures</span>
          </div>
          <div className="scrollbar-thin max-h-[420px] overflow-y-auto">
            {(q.data?.items ?? []).length === 0 ? (
              <p className="px-3 py-8 text-center text-sm text-fg-subtle">
                No warnings or failures. All quiet.
              </p>
            ) : (
              q.data!.items.map((e) => (
                <Popover.Close asChild key={e.id}>
                  <button
                    onClick={() => e.job_id && navigate(`/jobs/${e.job_id}`)}
                    className="flex w-full gap-2.5 border-b px-3 py-2.5 text-left last:border-0 hover:bg-surface-2"
                  >
                    <span
                      className={cn(
                        "mt-1.5 h-1.5 w-1.5 shrink-0 rounded-full",
                        e.severity === "error" ? "bg-danger-solid" : "bg-warning-solid",
                      )}
                    />
                    <span className="min-w-0">
                      <span className="line-clamp-2 block text-sm">{e.message}</span>
                      <span className="text-2xs text-fg-subtle">{relativeTime(e.created_at)}</span>
                    </span>
                  </button>
                </Popover.Close>
              ))
            )}
          </div>
        </Popover.Content>
      </Popover.Portal>
    </Popover.Root>
  );
}

const themeIcons: Record<ThemePref, typeof Sun> = { light: Sun, dark: Moon, system: Monitor };

export function ThemeMenu() {
  const { pref, setPref } = useTheme();
  const Icon = themeIcons[pref];
  return (
    <Dropdown>
      <Tooltip content="Theme">
        <DropdownTrigger asChild>
          <button
            className="inline-flex h-8 w-8 items-center justify-center rounded-md text-fg-muted hover:bg-surface-2 hover:text-fg"
            aria-label="Change theme"
          >
            <Icon className="h-4 w-4" />
          </button>
        </DropdownTrigger>
      </Tooltip>
      <DropdownContent className="w-40">
        <DropdownLabel>Theme</DropdownLabel>
        {(["light", "dark", "system"] as ThemePref[]).map((p) => {
          const I = themeIcons[p];
          return (
            <DropdownItem key={p} icon={<I />} onSelect={() => setPref(p)}>
              <span className="capitalize">{p}</span>
              {pref === p && <span className="sr-only"> (selected)</span>}
            </DropdownItem>
          );
        })}
      </DropdownContent>
    </Dropdown>
  );
}

export function Topbar({ onMenu, onSearch }: { onMenu: () => void; onSearch: () => void }) {
  return (
    <header className="sticky top-0 z-30 flex h-12 items-center gap-2 border-b bg-surface/85 px-3 backdrop-blur sm:px-4">
      <button
        onClick={onMenu}
        className="-ml-1 rounded-md p-1.5 text-fg-muted hover:bg-surface-2 lg:hidden"
        aria-label="Open navigation"
      >
        <Menu className="h-5 w-5" />
      </button>
      <div className="min-w-0 flex-1">
        <Breadcrumbs />
      </div>
      <button
        onClick={onSearch}
        className="hidden h-8 w-56 items-center gap-2 rounded-md bg-surface-2 px-2.5 text-sm text-fg-subtle ring-1 ring-inset ring-line hover:text-fg md:flex"
      >
        <Search className="h-3.5 w-3.5" />
        <span className="flex-1 text-left">Search or jump to…</span>
        <Kbd>⌘K</Kbd>
      </button>
      <button
        onClick={onSearch}
        className="rounded-md p-1.5 text-fg-muted hover:bg-surface-2 md:hidden"
        aria-label="Search"
      >
        <Search className="h-4 w-4" />
      </button>
      <LiveIndicator />
      <Notifications />
      <ThemeMenu />
    </header>
  );
}
