import { useEffect, useState, type ReactNode } from "react";
import * as Popover from "@radix-ui/react-popover";
import { Check, ChevronDown, Search, X } from "lucide-react";
import { cn } from "@/lib/utils";
import { useDebouncedValue } from "@/lib/url-state";
import { Input } from "@/components/ui/input";

/** Debounced search box bound to a URL filter. */
export function SearchInput({
  value,
  onChange,
  placeholder,
}: {
  value: string;
  onChange: (v: string) => void;
  placeholder: string;
}) {
  const [text, setText] = useState(value);
  const debounced = useDebouncedValue(text, 250);
  useEffect(() => setText(value), [value]);
  useEffect(() => {
    if (debounced !== value) onChange(debounced);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [debounced]);
  return (
    <div className="relative w-full sm:w-64">
      <Input
        leading={<Search className="h-3.5 w-3.5" />}
        value={text}
        onChange={(e) => setText(e.target.value)}
        placeholder={placeholder}
        aria-label={placeholder}
        type="search"
      />
      {text && (
        <button
          onClick={() => setText("")}
          className="absolute right-2 top-1/2 -translate-y-1/2 rounded p-0.5 text-fg-subtle hover:text-fg"
          aria-label="Clear search"
        >
          <X className="h-3.5 w-3.5" />
        </button>
      )}
    </div>
  );
}

/** Multi-select filter rendered as a compact button with a checklist. */
export function MultiFilter({
  label,
  options,
  selected,
  onChange,
  render,
}: {
  label: string;
  options: readonly string[];
  selected: string[];
  onChange: (v: string[]) => void;
  render?: (v: string) => ReactNode;
}) {
  const toggle = (v: string) =>
    onChange(selected.includes(v) ? selected.filter((s) => s !== v) : [...selected, v]);
  return (
    <Popover.Root>
      <Popover.Trigger asChild>
        <button
          className={cn(
            "inline-flex h-8 items-center gap-1.5 rounded-md px-2.5 text-sm ring-1 ring-inset hover:bg-surface-2",
            selected.length
              ? "bg-accent-soft/50 text-fg ring-accent/30"
              : "bg-surface text-fg-muted ring-line-strong",
          )}
        >
          {label}
          {selected.length > 0 && (
            <span className="tabular rounded bg-accent px-1 text-2xs font-medium text-accent-fg">
              {selected.length}
            </span>
          )}
          <ChevronDown className="h-3.5 w-3.5 text-fg-subtle" />
        </button>
      </Popover.Trigger>
      <Popover.Portal>
        <Popover.Content
          align="start"
          sideOffset={4}
          collisionPadding={8}
          className="z-50 w-56 animate-fade-in rounded-lg bg-surface p-1 shadow-pop"
        >
          <div
            role="listbox"
            aria-multiselectable
            aria-label={label}
            className="scrollbar-thin max-h-72 overflow-y-auto"
          >
            {options.map((o) => {
              const on = selected.includes(o);
              return (
                <button
                  key={o}
                  role="option"
                  aria-selected={on}
                  onClick={() => toggle(o)}
                  className="flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-left text-sm hover:bg-surface-2"
                >
                  <span
                    className={cn(
                      "flex h-4 w-4 items-center justify-center rounded border",
                      on ? "border-accent bg-accent text-accent-fg" : "border-line-strong",
                    )}
                  >
                    {on && <Check className="h-3 w-3" />}
                  </span>
                  {render ? render(o) : o}
                </button>
              );
            })}
          </div>
          {selected.length > 0 && (
            <button
              onClick={() => onChange([])}
              className="mt-1 w-full rounded-md border-t px-2 py-1.5 text-left text-xs text-fg-subtle hover:bg-surface-2"
            >
              Clear selection
            </button>
          )}
        </Popover.Content>
      </Popover.Portal>
    </Popover.Root>
  );
}

export function FilterBar({ children, right }: { children: ReactNode; right?: ReactNode }) {
  return (
    <div className="flex flex-col gap-2 border-b px-4 py-3 sm:flex-row sm:items-center sm:justify-between">
      <div className="flex flex-wrap items-center gap-2">{children}</div>
      {right && <div className="flex items-center gap-2">{right}</div>}
    </div>
  );
}
