import { Check, Copy } from "lucide-react";
import { useState } from "react";
import { toast } from "sonner";
import { cn, copyText } from "@/lib/utils";
import { IconButton } from "./button";

export function CopyButton({ text, label = "Copy" }: { text: string; label?: string }) {
  const [done, setDone] = useState(false);
  return (
    <IconButton
      label={done ? "Copied" : label}
      size="icon-sm"
      onClick={async () => {
        if (await copyText(text)) {
          setDone(true);
          setTimeout(() => setDone(false), 1500);
        } else {
          toast.error("Clipboard is not available in this browser context.");
        }
      }}
    >
      {done ? <Check className="h-3.5 w-3.5 text-success" /> : <Copy className="h-3.5 w-3.5" />}
    </IconButton>
  );
}

/** Pretty-printed, copyable JSON. */
export function JsonBlock({
  value,
  className,
  maxHeight = 320,
}: {
  value: unknown;
  className?: string;
  maxHeight?: number;
}) {
  const text = value === undefined || value === null ? "null" : JSON.stringify(value, null, 2);
  return (
    <div className={cn("group relative rounded-md border bg-surface-2/50", className)}>
      <div className="absolute right-1.5 top-1.5 opacity-70 group-hover:opacity-100">
        <CopyButton text={text} label="Copy JSON" />
      </div>
      <pre
        className="scrollbar-thin overflow-auto p-3 pr-10 font-mono text-xs leading-5 text-fg"
        style={{ maxHeight }}
      >
        {text}
      </pre>
    </div>
  );
}

/** Inline monospace identifier with copy-on-click. */
export function Mono({ children, copy, className }: { children: string; copy?: string; className?: string }) {
  return (
    <span className={cn("inline-flex items-center gap-1 font-mono text-xs text-fg-muted", className)}>
      <span className="truncate">{children}</span>
      {copy && <CopyButton text={copy} label="Copy ID" />}
    </span>
  );
}
