import { NavLink, useNavigate } from "react-router-dom";
import { useQueryClient } from "@tanstack/react-query";
import {
  ChevronsUpDown,
  LogOut,
  PanelLeftClose,
  PanelLeftOpen,
  Settings,
  UserRound,
  Users,
} from "lucide-react";
import { toast } from "sonner";
import { cn } from "@/lib/utils";
import { api, errorMessage } from "@/lib/api";
import { useSession } from "@/app/session";
import { useDeadLetter } from "@/api/hooks";
import { Tooltip } from "@/components/ui/tooltip";
import {
  Dropdown,
  DropdownContent,
  DropdownItem,
  DropdownLabel,
  DropdownSeparator,
  DropdownTrigger,
} from "@/components/ui/dropdown";
import { NAV } from "./nav";

export function Logo({ collapsed }: { collapsed?: boolean }) {
  return (
    <div className="flex items-center gap-2">
      <svg viewBox="0 0 32 32" className="h-6 w-6 shrink-0" aria-hidden>
        <rect width="32" height="32" rx="7" className="fill-accent" />
        <path d="M9 11h14M9 16h9M9 21h5" stroke="white" strokeWidth="2.6" strokeLinecap="round" />
        <circle cx="22" cy="21" r="2.6" fill="white" />
      </svg>
      {!collapsed && <span className="text-[15px] font-semibold tracking-tight">TaskFlow</span>}
    </div>
  );
}

function initials(name: string) {
  return name
    .split(/\s+/)
    .filter(Boolean)
    .slice(0, 2)
    .map((p) => p[0]!.toUpperCase())
    .join("");
}

export function useSignOut() {
  const { setSession } = useSession();
  const navigate = useNavigate();
  const qc = useQueryClient();
  return async () => {
    try {
      await api.post("/auth/logout");
    } catch (e) {
      toast.error(errorMessage(e));
    }
    setSession(null);
    qc.clear();
    navigate("/login", { replace: true });
  };
}

export function SidebarNav({ collapsed, onNavigate }: { collapsed?: boolean; onNavigate?: () => void }) {
  const dlq = useDeadLetter({ page_size: 1 });
  const dlqCount = dlq.data?.meta?.total ?? 0;
  return (
    <nav aria-label="Main" className="flex flex-col gap-0.5">
      {NAV.map((item) => {
        const link = (
          <NavLink
            key={item.to}
            to={item.to}
            end={item.to === "/"}
            onClick={onNavigate}
            className={({ isActive }) =>
              cn(
                "group flex h-8 items-center gap-2.5 rounded-md px-2 text-sm transition-colors",
                isActive
                  ? "bg-surface-3 font-medium text-fg"
                  : "text-fg-muted hover:bg-surface-2 hover:text-fg",
                collapsed && "justify-center px-0",
              )
            }
          >
            <item.icon className="h-4 w-4 shrink-0" aria-hidden />
            {!collapsed && <span className="flex-1 truncate">{item.label}</span>}
            {!collapsed && item.to === "/dead-letter" && dlqCount > 0 && (
              <span className="tabular rounded bg-danger-soft px-1.5 text-2xs font-medium text-danger">
                {dlqCount}
              </span>
            )}
          </NavLink>
        );
        return collapsed ? (
          <Tooltip key={item.to} content={item.label} side="right">
            {link}
          </Tooltip>
        ) : (
          link
        );
      })}
    </nav>
  );
}

function WorkspaceMenu({ collapsed }: { collapsed?: boolean }) {
  const { me } = useSession();
  const navigate = useNavigate();
  if (!me) return null;
  return (
    <Dropdown>
      <DropdownTrigger asChild>
        <button
          className={cn(
            "flex w-full items-center gap-2 rounded-md p-1.5 text-left hover:bg-surface-2",
            collapsed && "justify-center",
          )}
          aria-label="Workspace menu"
        >
          <span className="flex h-6 w-6 shrink-0 items-center justify-center rounded bg-surface-3 text-2xs font-semibold text-fg-muted ring-1 ring-line">
            {initials(me.workspace.name) || "W"}
          </span>
          {!collapsed && (
            <>
              <span className="min-w-0 flex-1">
                <span className="block truncate text-sm font-medium">{me.workspace.name}</span>
                <span className="block text-2xs capitalize text-fg-subtle">{me.user.role}</span>
              </span>
              <ChevronsUpDown className="h-3.5 w-3.5 text-fg-subtle" />
            </>
          )}
        </button>
      </DropdownTrigger>
      <DropdownContent align="start" className="w-60">
        <DropdownLabel>Workspace</DropdownLabel>
        <DropdownItem icon={<Settings />} onSelect={() => navigate("/settings")}>
          Workspace settings
        </DropdownItem>
        {me.user.role === "admin" && (
          <DropdownItem icon={<Users />} onSelect={() => navigate("/settings?tab=members")}>
            Members
          </DropdownItem>
        )}
      </DropdownContent>
    </Dropdown>
  );
}

function UserMenu({ collapsed }: { collapsed?: boolean }) {
  const { me } = useSession();
  const navigate = useNavigate();
  const signOut = useSignOut();
  if (!me) return null;
  return (
    <Dropdown>
      <DropdownTrigger asChild>
        <button
          className={cn(
            "flex w-full items-center gap-2 rounded-md p-1.5 text-left hover:bg-surface-2",
            collapsed && "justify-center",
          )}
          aria-label="Account menu"
        >
          <span className="flex h-6 w-6 shrink-0 items-center justify-center rounded-full bg-accent-soft text-2xs font-semibold text-accent-soft-fg">
            {initials(me.user.name)}
          </span>
          {!collapsed && (
            <span className="min-w-0 flex-1">
              <span className="block truncate text-sm">{me.user.name}</span>
              <span className="block truncate text-2xs text-fg-subtle">{me.user.email}</span>
            </span>
          )}
        </button>
      </DropdownTrigger>
      <DropdownContent align="start" className="w-56">
        <DropdownLabel>{me.user.email}</DropdownLabel>
        <DropdownItem icon={<UserRound />} onSelect={() => navigate("/settings?tab=account")}>
          Account
        </DropdownItem>
        <DropdownSeparator />
        <DropdownItem icon={<LogOut />} onSelect={signOut}>
          Sign out
        </DropdownItem>
      </DropdownContent>
    </Dropdown>
  );
}

export function Sidebar({ collapsed, onToggle }: { collapsed: boolean; onToggle: () => void }) {
  return (
    <aside
      className={cn(
        "sticky top-0 hidden h-dvh shrink-0 flex-col border-r bg-surface transition-[width] duration-150 lg:flex",
        collapsed ? "w-[60px]" : "w-[232px]",
      )}
    >
      <div
        className={cn(
          "flex h-12 items-center border-b px-3",
          collapsed ? "justify-center" : "justify-between",
        )}
      >
        <Logo collapsed={collapsed} />
        {!collapsed && (
          <button
            onClick={onToggle}
            className="rounded p-1 text-fg-subtle hover:bg-surface-2 hover:text-fg"
            aria-label="Collapse sidebar"
          >
            <PanelLeftClose className="h-4 w-4" />
          </button>
        )}
      </div>
      <div className="p-2">
        <WorkspaceMenu collapsed={collapsed} />
      </div>
      <div className="scrollbar-thin flex-1 overflow-y-auto px-2 pb-2">
        <SidebarNav collapsed={collapsed} />
      </div>
      <div className="space-y-1 border-t p-2">
        {collapsed && (
          <Tooltip content="Expand sidebar" side="right">
            <button
              onClick={onToggle}
              className="flex h-8 w-full items-center justify-center rounded-md text-fg-subtle hover:bg-surface-2"
              aria-label="Expand sidebar"
            >
              <PanelLeftOpen className="h-4 w-4" />
            </button>
          </Tooltip>
        )}
        <UserMenu collapsed={collapsed} />
      </div>
    </aside>
  );
}

export { WorkspaceMenu, UserMenu };
