import * as DM from "@radix-ui/react-dropdown-menu";
import type { ReactNode } from "react";
import { cn } from "@/lib/utils";

export const Dropdown = DM.Root;
export const DropdownTrigger = DM.Trigger;

export function DropdownContent({
  children,
  align = "end",
  className,
}: {
  children: ReactNode;
  align?: "start" | "end";
  className?: string;
}) {
  return (
    <DM.Portal>
      <DM.Content
        align={align}
        sideOffset={4}
        collisionPadding={8}
        className={cn("z-50 min-w-[180px] animate-fade-in rounded-lg bg-surface p-1 shadow-pop", className)}
      >
        {children}
      </DM.Content>
    </DM.Portal>
  );
}

interface ItemProps {
  children: ReactNode;
  icon?: ReactNode;
  onSelect?: () => void;
  disabled?: boolean;
  /** Shown under the label when the item is disabled. */
  hint?: string;
  danger?: boolean;
  shortcut?: string;
}

export function DropdownItem({ children, icon, onSelect, disabled, hint, danger, shortcut }: ItemProps) {
  return (
    <DM.Item
      disabled={disabled}
      onSelect={onSelect}
      className={cn(
        "flex cursor-pointer select-none items-start gap-2 rounded-md px-2 py-1.5 text-sm outline-none",
        "data-[highlighted]:bg-surface-2 data-[disabled]:cursor-not-allowed data-[disabled]:opacity-50",
        danger ? "text-danger" : "text-fg",
      )}
    >
      {icon && <span className="mt-0.5 text-fg-subtle [&>svg]:h-3.5 [&>svg]:w-3.5">{icon}</span>}
      <span className="min-w-0 flex-1">
        <span className="block">{children}</span>
        {disabled && hint && <span className="block text-2xs text-fg-subtle">{hint}</span>}
      </span>
      {shortcut && <span className="text-2xs text-fg-faint">{shortcut}</span>}
    </DM.Item>
  );
}

export function DropdownLabel({ children }: { children: ReactNode }) {
  return (
    <DM.Label className="px-2 py-1.5 text-2xs font-medium uppercase tracking-wide text-fg-subtle">
      {children}
    </DM.Label>
  );
}

export function DropdownSeparator() {
  return <DM.Separator className="my-1 h-px bg-line" />;
}
