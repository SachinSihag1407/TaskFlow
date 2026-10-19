import type { ReactNode } from "react";
import { cn } from "@/lib/utils";

/** Page title row: title, optional description and contextual actions. */
export function PageHeader({
  title,
  description,
  actions,
  meta,
}: {
  title: ReactNode;
  description?: ReactNode;
  actions?: ReactNode;
  meta?: ReactNode;
}) {
  return (
    <div className="mb-5 flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
      <div className="min-w-0">
        <h1 className="truncate text-xl font-semibold tracking-tight">{title}</h1>
        {description && <p className="mt-0.5 text-sm text-fg-subtle">{description}</p>}
        {meta && <div className="mt-2 flex flex-wrap items-center gap-2">{meta}</div>}
      </div>
      {actions && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
    </div>
  );
}

/** Bordered surface with an optional header row. */
export function Panel({
  title,
  description,
  actions,
  children,
  className,
  bodyClassName,
}: {
  title?: ReactNode;
  description?: ReactNode;
  actions?: ReactNode;
  children: ReactNode;
  className?: string;
  bodyClassName?: string;
}) {
  return (
    <section className={cn("min-w-0 rounded-lg border bg-surface shadow-xs", className)}>
      {(title || actions) && (
        <header className="flex min-h-[44px] flex-wrap items-center justify-between gap-2 border-b px-4 py-2">
          <div className="min-w-0">
            {title && <h2 className="text-sm font-medium">{title}</h2>}
            {description && <p className="text-xs text-fg-subtle">{description}</p>}
          </div>
          {actions && <div className="flex items-center gap-2">{actions}</div>}
        </header>
      )}
      <div className={bodyClassName}>{children}</div>
    </section>
  );
}

/** Label/value pairs for configuration and metadata. */
export function DescriptionList({
  items,
  columns = 2,
}: {
  items: { label: string; value: ReactNode; full?: boolean }[];
  columns?: 1 | 2 | 3;
}) {
  return (
    <dl
      className={cn(
        "grid gap-x-6 gap-y-4",
        columns === 1 ? "grid-cols-1" : columns === 2 ? "sm:grid-cols-2" : "sm:grid-cols-2 lg:grid-cols-3",
      )}
    >
      {items.map((it) => (
        <div key={it.label} className={cn("min-w-0", it.full && "sm:col-span-full")}>
          <dt className="text-xs text-fg-subtle">{it.label}</dt>
          <dd className="mt-0.5 break-words text-sm text-fg">{it.value}</dd>
        </div>
      ))}
    </dl>
  );
}

export function Kbd({ children }: { children: ReactNode }) {
  return (
    <kbd className="inline-flex h-5 min-w-[20px] items-center justify-center rounded border border-line-strong bg-surface-2 px-1 font-mono text-2xs text-fg-subtle">
      {children}
    </kbd>
  );
}

/** Segmented control for small mutually-exclusive choices (e.g. time range). */
export function Segmented<T extends string>({
  value,
  onChange,
  options,
  label,
}: {
  value: T;
  onChange: (v: T) => void;
  options: { value: T; label: string }[];
  label: string;
}) {
  return (
    <div
      role="radiogroup"
      aria-label={label}
      className="inline-flex rounded-md bg-surface-2 p-0.5 ring-1 ring-inset ring-line"
    >
      {options.map((o) => (
        <button
          key={o.value}
          type="button"
          role="radio"
          aria-checked={value === o.value}
          onClick={() => onChange(o.value)}
          className={cn(
            "h-6 rounded px-2.5 text-xs font-medium transition-colors",
            value === o.value
              ? "bg-surface text-fg shadow-xs ring-1 ring-line"
              : "text-fg-subtle hover:text-fg",
          )}
        >
          {o.label}
        </button>
      ))}
    </div>
  );
}
