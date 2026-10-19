import * as RT from "@radix-ui/react-tooltip";
import type { ReactNode } from "react";
import { cn } from "@/lib/utils";

export const TooltipProvider = RT.Provider;

interface TooltipProps {
  content: ReactNode;
  children: ReactNode;
  side?: "top" | "bottom" | "left" | "right";
  className?: string;
  /** Render nothing extra when there is no content. */
  disabled?: boolean;
}

export function Tooltip({ content, children, side = "top", className, disabled }: TooltipProps) {
  if (disabled || !content) return <>{children}</>;
  return (
    <RT.Root delayDuration={250}>
      <RT.Trigger asChild>{children}</RT.Trigger>
      <RT.Portal>
        <RT.Content
          side={side}
          sideOffset={6}
          collisionPadding={8}
          className={cn(
            "z-50 max-w-xs animate-fade-in rounded-md bg-fg px-2.5 py-1.5 text-xs text-canvas shadow-pop",
            className,
          )}
        >
          {content}
        </RT.Content>
      </RT.Portal>
    </RT.Root>
  );
}
