import { forwardRef, type ButtonHTMLAttributes, type ReactNode } from "react";
import { Link, type LinkProps } from "react-router-dom";
import { Loader2 } from "lucide-react";
import { cn } from "@/lib/utils";
import { Tooltip } from "./tooltip";

type Variant = "primary" | "secondary" | "ghost" | "danger" | "outline";
type Size = "sm" | "md" | "icon" | "icon-sm";

const variants: Record<Variant, string> = {
  primary: "bg-accent text-accent-fg shadow-xs hover:bg-accent-hover",
  secondary: "bg-surface text-fg ring-1 ring-inset ring-line-strong shadow-xs hover:bg-surface-2",
  outline: "bg-transparent text-fg ring-1 ring-inset ring-line hover:bg-surface-2",
  ghost: "bg-transparent text-fg-muted hover:bg-surface-2 hover:text-fg",
  danger: "bg-danger-solid text-white shadow-xs hover:bg-danger-solid/90",
};

const sizes: Record<Size, string> = {
  sm: "h-7 gap-1.5 px-2.5 text-xs",
  md: "h-8 gap-2 px-3 text-sm",
  icon: "h-8 w-8 justify-center",
  "icon-sm": "h-7 w-7 justify-center",
};

const base =
  "inline-flex shrink-0 select-none items-center whitespace-nowrap rounded-md font-medium transition-colors";

/** Class names for button styling, shared by <Button> and <LinkButton>. */
export function buttonClass(variant: Variant = "secondary", size: Size = "md", className?: string) {
  return cn(base, variants[variant], sizes[size], className);
}

/** A router link styled as a button (avoids nesting <button> inside <a>). */
export function LinkButton({
  variant,
  size,
  icon,
  className,
  children,
  ...props
}: LinkProps & { variant?: Variant; size?: Size; icon?: ReactNode }) {
  return (
    <Link className={buttonClass(variant, size, className)} {...props}>
      {icon}
      {children}
    </Link>
  );
}

export interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: Variant;
  size?: Size;
  loading?: boolean;
  icon?: ReactNode;
  /**
   * When set, the button is disabled and the reason is shown in a tooltip,
   * so users always learn why an action is unavailable.
   */
  disabledReason?: string;
}

export const Button = forwardRef<HTMLButtonElement, ButtonProps>(function Button(
  {
    variant = "secondary",
    size = "md",
    loading,
    icon,
    disabledReason,
    className,
    children,
    disabled,
    type = "button",
    ...props
  },
  ref,
) {
  const isDisabled = disabled || loading || !!disabledReason;
  const btn = (
    <button
      ref={ref}
      type={type}
      disabled={isDisabled}
      aria-disabled={isDisabled || undefined}
      className={cn(buttonClass(variant, size), "disabled:cursor-not-allowed disabled:opacity-50", className)}
      {...props}
    >
      {loading ? <Loader2 className="h-3.5 w-3.5 animate-spin" aria-hidden /> : icon}
      {children}
    </button>
  );
  if (disabledReason) {
    // Disabled buttons do not emit pointer events; wrap so the tooltip works.
    return (
      <Tooltip content={disabledReason}>
        <span tabIndex={0} className="inline-flex rounded-md" aria-label={disabledReason}>
          {btn}
        </span>
      </Tooltip>
    );
  }
  return btn;
});

interface IconButtonProps extends Omit<ButtonProps, "size" | "children"> {
  label: string;
  size?: "icon" | "icon-sm";
  children: ReactNode;
}

/** A square icon-only button with an accessible label and tooltip. */
export const IconButton = forwardRef<HTMLButtonElement, IconButtonProps>(function IconButton(
  { label, size = "icon", variant = "ghost", children, ...props },
  ref,
) {
  return (
    <Tooltip content={label} disabled={!!props.disabledReason}>
      <Button ref={ref} aria-label={label} size={size} variant={variant} {...props}>
        {children}
      </Button>
    </Tooltip>
  );
});
