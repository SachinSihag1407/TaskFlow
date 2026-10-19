import * as RD from "@radix-ui/react-dialog";
import { X } from "lucide-react";
import { useState, type ReactNode } from "react";
import { cn } from "@/lib/utils";
import { Button } from "./button";

interface DialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: string;
  description?: ReactNode;
  children?: ReactNode;
  footer?: ReactNode;
  className?: string;
}

/** Modal dialog: focus-trapped, Esc to close, fits small viewports. */
export function Dialog({ open, onOpenChange, title, description, children, footer, className }: DialogProps) {
  return (
    <RD.Root open={open} onOpenChange={onOpenChange}>
      <RD.Portal>
        <RD.Overlay className="fixed inset-0 z-50 animate-fade-in bg-black/40 backdrop-blur-[1px]" />
        <RD.Content
          className={cn(
            "fixed left-1/2 top-1/2 z-50 flex max-h-[calc(100dvh-2rem)] w-[calc(100vw-2rem)] max-w-lg -translate-x-1/2 -translate-y-1/2 flex-col",
            "animate-zoom-in rounded-lg bg-surface shadow-dialog focus:outline-none",
            className,
          )}
        >
          <div className="flex items-start justify-between gap-4 border-b px-5 py-4">
            <div className="min-w-0">
              <RD.Title className="text-base font-semibold text-fg">{title}</RD.Title>
              {description && (
                <RD.Description className="mt-1 text-sm text-fg-muted">{description}</RD.Description>
              )}
            </div>
            <RD.Close
              className="-mr-1 rounded p-1 text-fg-subtle hover:bg-surface-2 hover:text-fg"
              aria-label="Close"
            >
              <X className="h-4 w-4" />
            </RD.Close>
          </div>
          {children && <div className="min-h-0 flex-1 overflow-y-auto px-5 py-4">{children}</div>}
          {footer && (
            <div className="flex flex-wrap justify-end gap-2 border-t bg-surface-2/40 px-5 py-3">
              {footer}
            </div>
          )}
        </RD.Content>
      </RD.Portal>
    </RD.Root>
  );
}

interface ConfirmProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: string;
  description?: ReactNode;
  confirmLabel: string;
  tone?: "danger" | "primary";
  onConfirm: () => Promise<unknown> | void;
  children?: ReactNode;
}

/** Confirmation for destructive or consequential actions. */
export function ConfirmDialog({
  open,
  onOpenChange,
  title,
  description,
  confirmLabel,
  tone = "primary",
  onConfirm,
  children,
}: ConfirmProps) {
  const [busy, setBusy] = useState(false);
  const run = async () => {
    setBusy(true);
    try {
      await onConfirm();
      onOpenChange(false);
    } catch {
      // The caller surfaces the error (toast); keep the dialog open.
    } finally {
      setBusy(false);
    }
  };
  return (
    <Dialog
      open={open}
      onOpenChange={(o) => !busy && onOpenChange(o)}
      title={title}
      description={description}
      footer={
        <>
          <Button variant="secondary" onClick={() => onOpenChange(false)} disabled={busy}>
            Cancel
          </Button>
          <Button variant={tone === "danger" ? "danger" : "primary"} onClick={run} loading={busy}>
            {confirmLabel}
          </Button>
        </>
      }
    >
      {children}
    </Dialog>
  );
}

/** Slide-over panel from the right (used for inspecting records). */
export function Sheet({
  open,
  onOpenChange,
  title,
  children,
  className,
}: Omit<DialogProps, "footer" | "description">) {
  return (
    <RD.Root open={open} onOpenChange={onOpenChange}>
      <RD.Portal>
        <RD.Overlay className="fixed inset-0 z-50 animate-fade-in bg-black/30" />
        <RD.Content
          className={cn(
            "fixed inset-y-0 right-0 z-50 flex w-full max-w-2xl animate-slide-in-right flex-col bg-surface shadow-dialog focus:outline-none",
            className,
          )}
        >
          <div className="flex h-12 items-center justify-between gap-4 border-b px-4">
            <RD.Title className="truncate text-sm font-semibold">{title}</RD.Title>
            <RD.Close
              className="rounded p-1 text-fg-subtle hover:bg-surface-2 hover:text-fg"
              aria-label="Close"
            >
              <X className="h-4 w-4" />
            </RD.Close>
          </div>
          <RD.Description className="sr-only">{title}</RD.Description>
          <div className="min-h-0 flex-1 overflow-y-auto">{children}</div>
        </RD.Content>
      </RD.Portal>
    </RD.Root>
  );
}
