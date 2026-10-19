import { Command } from "cmdk";
import * as RD from "@radix-ui/react-dialog";
import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { LogOut, Monitor, Moon, Plus, Search, Sun } from "lucide-react";
import { api, qs } from "@/lib/api";
import type { JobListItem } from "@/lib/types";
import { useTheme } from "@/app/theme";
import { useCan } from "@/app/session";
import { StatusBadge } from "@/components/ui/badge";
import { NAV } from "./nav";
import { useSignOut } from "./Sidebar";

function useDebounced<T>(value: T, ms: number) {
  const [v, setV] = useState(value);
  useEffect(() => {
    const t = setTimeout(() => setV(value), ms);
    return () => clearTimeout(t);
  }, [value, ms]);
  return v;
}

const item =
  "flex h-9 cursor-pointer items-center gap-2.5 rounded-md px-2 text-sm text-fg aria-selected:bg-surface-2 [&>svg]:h-4 [&>svg]:w-4 [&>svg]:text-fg-subtle";

export function CommandPalette({
  open,
  onOpenChange,
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
}) {
  const navigate = useNavigate();
  const { setPref } = useTheme();
  const can = useCan();
  const signOut = useSignOut();
  const [search, setSearch] = useState("");
  const q = useDebounced(search.trim(), 200);

  const jobs = useQuery({
    queryKey: ["palette-jobs", q],
    queryFn: ({ signal }) =>
      api.page<JobListItem>(`/jobs${qs({ q, page_size: 8, sort: "updated_at" })}`, signal),
    enabled: open && q.length > 0,
  });

  useEffect(() => {
    if (!open) setSearch("");
  }, [open]);

  const go = (fn: () => void) => {
    onOpenChange(false);
    fn();
  };

  return (
    <RD.Root open={open} onOpenChange={onOpenChange}>
      <RD.Portal>
        <RD.Overlay className="fixed inset-0 z-50 animate-fade-in bg-black/40" />
        <RD.Content className="fixed left-1/2 top-[12vh] z-50 w-[calc(100vw-1.5rem)] max-w-xl -translate-x-1/2 overflow-hidden rounded-lg bg-surface shadow-dialog">
          <RD.Title className="sr-only">Command palette</RD.Title>
          <RD.Description className="sr-only">Search jobs, navigate, and run actions</RD.Description>
          <Command shouldFilter={true} label="Command palette" loop>
            <div className="flex items-center gap-2 border-b px-3">
              <Search className="h-4 w-4 text-fg-subtle" />
              <Command.Input
                value={search}
                onValueChange={setSearch}
                placeholder="Search jobs, pages and actions…"
                className="h-11 flex-1 bg-transparent text-sm outline-none placeholder:text-fg-faint"
              />
            </div>
            <Command.List className="scrollbar-thin max-h-[min(420px,60vh)] overflow-y-auto p-1.5">
              <Command.Empty className="px-3 py-8 text-center text-sm text-fg-subtle">
                {jobs.isFetching ? "Searching…" : "No matches."}
              </Command.Empty>
              {(jobs.data?.data ?? []).length > 0 && (
                <Command.Group
                  heading="Jobs"
                  className="[&_[cmdk-group-heading]]:px-2 [&_[cmdk-group-heading]]:py-1.5 [&_[cmdk-group-heading]]:text-2xs [&_[cmdk-group-heading]]:uppercase [&_[cmdk-group-heading]]:text-fg-subtle"
                >
                  {jobs.data!.data.map((j) => (
                    <Command.Item
                      key={j.id}
                      value={`job ${j.name} ${j.id} ${search}`}
                      onSelect={() => go(() => navigate(`/jobs/${j.id}`))}
                      className={item}
                    >
                      <span className="min-w-0 flex-1 truncate">{j.name}</span>
                      <StatusBadge status={j.status} withTooltip={false} />
                    </Command.Item>
                  ))}
                </Command.Group>
              )}
              <Command.Group
                heading="Navigate"
                className="[&_[cmdk-group-heading]]:px-2 [&_[cmdk-group-heading]]:py-1.5 [&_[cmdk-group-heading]]:text-2xs [&_[cmdk-group-heading]]:uppercase [&_[cmdk-group-heading]]:text-fg-subtle"
              >
                {NAV.map((n) => (
                  <Command.Item
                    key={n.to}
                    value={`go ${n.label}`}
                    onSelect={() => go(() => navigate(n.to))}
                    className={item}
                  >
                    <n.icon />
                    {n.label}
                  </Command.Item>
                ))}
              </Command.Group>
              <Command.Group
                heading="Actions"
                className="[&_[cmdk-group-heading]]:px-2 [&_[cmdk-group-heading]]:py-1.5 [&_[cmdk-group-heading]]:text-2xs [&_[cmdk-group-heading]]:uppercase [&_[cmdk-group-heading]]:text-fg-subtle"
              >
                {can("write") && (
                  <Command.Item
                    value="create new job"
                    onSelect={() => go(() => navigate("/jobs/new"))}
                    className={item}
                  >
                    <Plus />
                    Create job
                  </Command.Item>
                )}
                <Command.Item
                  value="theme light"
                  onSelect={() => go(() => setPref("light"))}
                  className={item}
                >
                  <Sun />
                  Switch to light theme
                </Command.Item>
                <Command.Item value="theme dark" onSelect={() => go(() => setPref("dark"))} className={item}>
                  <Moon />
                  Switch to dark theme
                </Command.Item>
                <Command.Item
                  value="theme system"
                  onSelect={() => go(() => setPref("system"))}
                  className={item}
                >
                  <Monitor />
                  Use system theme
                </Command.Item>
                <Command.Item value="sign out logout" onSelect={() => go(signOut)} className={item}>
                  <LogOut />
                  Sign out
                </Command.Item>
              </Command.Group>
            </Command.List>
          </Command>
        </RD.Content>
      </RD.Portal>
    </RD.Root>
  );
}
