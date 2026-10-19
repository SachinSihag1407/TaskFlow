import type { ReactNode } from "react";
import { Logo } from "@/components/shell/Sidebar";

export function AuthLayout({
  title,
  subtitle,
  children,
  footer,
}: {
  title: string;
  subtitle: string;
  children: ReactNode;
  footer: ReactNode;
}) {
  return (
    <div className="flex min-h-dvh flex-col bg-canvas">
      <div className="flex flex-1 items-center justify-center px-4 py-10">
        <div className="w-full max-w-[380px]">
          <div className="mb-8 flex justify-center">
            <Logo />
          </div>
          <div className="rounded-lg border bg-surface p-6 shadow-sm">
            <h1 className="text-lg font-semibold tracking-tight">{title}</h1>
            <p className="mt-1 text-sm text-fg-subtle">{subtitle}</p>
            <div className="mt-6">{children}</div>
          </div>
          <div className="mt-4 text-center text-sm text-fg-subtle">{footer}</div>
        </div>
      </div>
      <p className="pb-6 text-center text-2xs text-fg-faint">TaskFlow · Distributed job scheduling</p>
    </div>
  );
}
