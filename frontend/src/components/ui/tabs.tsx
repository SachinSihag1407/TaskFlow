import * as RT from "@radix-ui/react-tabs";
import type { ReactNode } from "react";
import { cn } from "@/lib/utils";

export const Tabs = RT.Root;
export const TabsContent = RT.Content;

export function TabsList({ children, className }: { children: ReactNode; className?: string }) {
  return (
    <RT.List className={cn("scrollbar-thin flex gap-1 overflow-x-auto border-b", className)}>
      {children}
    </RT.List>
  );
}

export function TabsTrigger({
  value,
  children,
  count,
}: {
  value: string;
  children: ReactNode;
  count?: number;
}) {
  return (
    <RT.Trigger
      value={value}
      className={cn(
        "relative -mb-px inline-flex h-9 shrink-0 items-center gap-1.5 border-b-2 border-transparent px-2.5 text-sm text-fg-subtle",
        "hover:text-fg data-[state=active]:border-accent data-[state=active]:text-fg",
      )}
    >
      {children}
      {count !== undefined && (
        <span className="tabular rounded bg-surface-2 px-1.5 text-2xs text-fg-subtle ring-1 ring-inset ring-line">
          {count}
        </span>
      )}
    </RT.Trigger>
  );
}
