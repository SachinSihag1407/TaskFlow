import {
  forwardRef,
  useId,
  type InputHTMLAttributes,
  type ReactNode,
  type SelectHTMLAttributes,
  type TextareaHTMLAttributes,
} from "react";
import { ChevronDown } from "lucide-react";
import { cn } from "@/lib/utils";

const control =
  "w-full rounded-md bg-surface text-sm text-fg ring-1 ring-inset ring-line-strong placeholder:text-fg-faint " +
  "transition-shadow focus:outline-none focus:ring-2 focus:ring-accent/60 disabled:cursor-not-allowed disabled:opacity-60 " +
  "aria-[invalid=true]:ring-danger-solid/70";

export const Input = forwardRef<
  HTMLInputElement,
  InputHTMLAttributes<HTMLInputElement> & { leading?: ReactNode }
>(function Input({ className, leading, ...props }, ref) {
  if (leading) {
    return (
      <div className={cn("relative", className)}>
        <span className="pointer-events-none absolute inset-y-0 left-2.5 flex items-center text-fg-subtle">
          {leading}
        </span>
        <input ref={ref} className={cn(control, "h-8 pl-8 pr-2.5")} {...props} />
      </div>
    );
  }
  return <input ref={ref} className={cn(control, "h-8 px-2.5", className)} {...props} />;
});

export const Textarea = forwardRef<HTMLTextAreaElement, TextareaHTMLAttributes<HTMLTextAreaElement>>(
  function Textarea({ className, ...props }, ref) {
    return (
      <textarea
        ref={ref}
        className={cn(control, "min-h-[80px] px-2.5 py-2 leading-5", className)}
        {...props}
      />
    );
  },
);

/** Native select styled to match inputs: accessible and mobile-friendly by default. */
export const Select = forwardRef<HTMLSelectElement, SelectHTMLAttributes<HTMLSelectElement>>(function Select(
  { className, children, ...props },
  ref,
) {
  return (
    <div className={cn("relative", className)}>
      <select ref={ref} className={cn(control, "h-8 appearance-none pl-2.5 pr-8")} {...props}>
        {children}
      </select>
      <ChevronDown
        className="pointer-events-none absolute right-2.5 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-fg-subtle"
        aria-hidden
      />
    </div>
  );
});

export function Checkbox({ className, ...props }: InputHTMLAttributes<HTMLInputElement>) {
  return (
    <input
      type="checkbox"
      className={cn(
        "h-4 w-4 cursor-pointer rounded border-line-strong bg-surface accent-[rgb(var(--accent))] align-middle",
        className,
      )}
      {...props}
    />
  );
}

interface FieldProps {
  label: string;
  hint?: ReactNode;
  error?: string;
  required?: boolean;
  className?: string;
  children: (props: { id: string; "aria-invalid"?: boolean; "aria-describedby"?: string }) => ReactNode;
}

/** Label + control + hint/error, wired for screen readers. */
export function Field({ label, hint, error, required, className, children }: FieldProps) {
  const id = useId();
  const descId = `${id}-desc`;
  return (
    <div className={cn("space-y-1.5", className)}>
      <label htmlFor={id} className="block text-sm font-medium text-fg">
        {label}
        {required && (
          <span className="ml-0.5 text-danger" aria-hidden>
            *
          </span>
        )}
      </label>
      {children({
        id,
        "aria-invalid": error ? true : undefined,
        "aria-describedby": error || hint ? descId : undefined,
      })}
      {error ? (
        <p id={descId} role="alert" className="text-xs text-danger">
          {error}
        </p>
      ) : hint ? (
        <p id={descId} className="text-xs text-fg-subtle">
          {hint}
        </p>
      ) : null}
    </div>
  );
}
