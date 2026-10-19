import { ChevronLeft, ChevronRight } from "lucide-react";
import type { PageMeta } from "@/lib/types";
import { formatNumber } from "@/lib/format";
import { Button } from "./button";

export function Pagination({
  meta,
  onPage,
  onPageSize,
}: {
  meta?: PageMeta;
  onPage: (p: number) => void;
  onPageSize?: (n: number) => void;
}) {
  if (!meta || meta.total === 0) return null;
  const from = (meta.page - 1) * meta.page_size + 1;
  const to = Math.min(meta.total, meta.page * meta.page_size);
  return (
    <div className="flex flex-wrap items-center justify-between gap-3 border-t px-4 py-2.5 text-xs text-fg-subtle">
      <span className="tabular">
        {formatNumber(from)}–{formatNumber(to)} of {formatNumber(meta.total)}
      </span>
      <div className="flex items-center gap-3">
        {onPageSize && (
          <label className="hidden items-center gap-1.5 sm:flex">
            Rows
            <select
              value={meta.page_size}
              onChange={(e) => onPageSize(Number(e.target.value))}
              className="h-7 rounded-md bg-surface px-1.5 text-xs ring-1 ring-inset ring-line-strong"
            >
              {[25, 50, 100].map((n) => (
                <option key={n} value={n}>
                  {n}
                </option>
              ))}
            </select>
          </label>
        )}
        <div className="flex items-center gap-1">
          <Button
            size="icon-sm"
            variant="outline"
            aria-label="Previous page"
            disabled={meta.page <= 1}
            onClick={() => onPage(meta.page - 1)}
          >
            <ChevronLeft className="h-3.5 w-3.5" />
          </Button>
          <span className="tabular px-2">
            {meta.page} / {Math.max(1, meta.total_pages)}
          </span>
          <Button
            size="icon-sm"
            variant="outline"
            aria-label="Next page"
            disabled={meta.page >= meta.total_pages}
            onClick={() => onPage(meta.page + 1)}
          >
            <ChevronRight className="h-3.5 w-3.5" />
          </Button>
        </div>
      </div>
    </div>
  );
}
