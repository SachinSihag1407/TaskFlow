import {
  Activity,
  CalendarClock,
  Cpu,
  HeartPulse,
  Inbox,
  LayoutDashboard,
  ListChecks,
  Settings,
  type LucideIcon,
} from "lucide-react";

export interface NavItem {
  to: string;
  label: string;
  icon: LucideIcon;
  /** Keyboard shortcut shown in the command palette (g + key). */
  key: string;
}

export const NAV: NavItem[] = [
  { to: "/", label: "Overview", icon: LayoutDashboard, key: "o" },
  { to: "/jobs", label: "Jobs", icon: ListChecks, key: "j" },
  { to: "/schedules", label: "Schedules", icon: CalendarClock, key: "s" },
  { to: "/executions", label: "Executions", icon: Activity, key: "e" },
  { to: "/workers", label: "Workers", icon: Cpu, key: "w" },
  { to: "/dead-letter", label: "Dead-letter queue", icon: Inbox, key: "d" },
  { to: "/health", label: "System health", icon: HeartPulse, key: "h" },
  { to: "/settings", label: "Settings", icon: Settings, key: "," },
];
