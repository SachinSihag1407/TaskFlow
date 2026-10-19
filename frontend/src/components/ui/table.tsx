import type { HTMLAttributes, ReactNode, TdHTMLAttributes, ThHTMLAttributes } from "react";
import { ArrowDown, ArrowUp, ArrowUpDown } from "lucide-react";
import { cn } from "@/lib/utils";

/** Horizontally scrollable table container; rows keep a fixed height. */
export function Table({
  children,
  className,
  minWidth = 720,
}: {
  children: ReactNode;
  className?: string;
  minWidth?: number;
}) {
  return (
    <div className={cn("scrollbar-thin overflow-x-auto", className)}>
      <table className="w-full border-collapse text-sm" style={{ minWidth }}>
        {children}
      </table>
    </div>
  );
}

export function THead({ children }: { children: ReactNode }) {
  return <thead className="sticky top-0 z-[1] bg-surface-2/60 backdrop-blur">{children}</thead>;
}

interface THProps extends ThHTMLAttributes<HTMLTableCellElement> {
  sortKey?: string;
  sort?: { key: string; desc: boolean };
  onSort?: (key: string) => void;
  align?: "left" | "right";
}

export function TH({ children, className, sortKey, sort, onSort, align = "left", ...props }: THProps) {
  const active = sortKey && sort?.key === sortKey;
  const content = sortKey ? (
    <button
      type="button"
      onClick={() => onSort?.(sortKey)}
      className={cn("inline-flex items-center gap-1 hover:text-fg", align === "right" && "flex-row-reverse")}
    >
      {children}
      {active ? (
        sort!.desc ? (
          <ArrowDown className="h-3 w-3" />
        ) : (
          <ArrowUp className="h-3 w-3" />
        )
      ) : (
        <ArrowUpDown className="h-3 w-3 opacity-40" />
      )}
    </button>
  ) : (
    children
  );
  return (
    <th
      scope="col"
      aria-sort={active ? (sort!.desc ? "descending" : "ascending") : undefined}
      className={cn(
        "h-8 whitespace-nowrap border-b px-3 text-left text-2xs font-medium uppercase tracking-wide text-fg-subtle first:pl-4 last:pr-4",
        align === "right" && "text-right",
        className,
      )}
      {...props}
    >
      {content}
    </th>
  );
}

export function TR({
  children,
  className,
  onClick,
  selected,
  ...props
}: HTMLAttributes<HTMLTableRowElement> & { selected?: boolean }) {
  return (
    <tr
      onClick={onClick}
      className={cn(
        "group h-11 border-b last:border-b-0 transition-colors",
        onClick && "cursor-pointer hover:bg-surface-2/60",
        selected && "bg-accent-soft/40",
        className,
      )}
      {...props}
    >
      {children}
    </tr>
  );
}

export function TD({
  children,
  className,
  align = "left",
  ...props
}: TdHTMLAttributes<HTMLTableCellElement> & { align?: "left" | "right" }) {
  return (
    <td
      className={cn("px-3 align-middle first:pl-4 last:pr-4", align === "right" && "text-right", className)}
      {...props}
    >
      {children}
    </td>
  );
}
