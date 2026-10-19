import type { Config } from "tailwindcss";

// Every colour maps to a CSS variable defined in src/index.css, so light and
// dark themes share one set of semantic class names.
const v = (name: string) => `rgb(var(--${name}) / <alpha-value>)`;

export default {
  content: ["./index.html", "./src/**/*.{ts,tsx}"],
  darkMode: "class",
  theme: {
    extend: {
      colors: {
        canvas: v("canvas"),
        surface: { DEFAULT: v("surface"), 2: v("surface-2"), 3: v("surface-3") },
        line: { DEFAULT: v("line"), strong: v("line-strong") },
        fg: { DEFAULT: v("fg"), muted: v("fg-muted"), subtle: v("fg-subtle"), faint: v("fg-faint") },
        accent: { DEFAULT: v("accent"), hover: v("accent-hover"), fg: v("accent-fg"), soft: v("accent-soft"), "soft-fg": v("accent-soft-fg") },
        success: { DEFAULT: v("success"), soft: v("success-soft"), solid: v("success-solid") },
        danger: { DEFAULT: v("danger"), soft: v("danger-soft"), solid: v("danger-solid") },
        warning: { DEFAULT: v("warning"), soft: v("warning-soft"), solid: v("warning-solid") },
        info: { DEFAULT: v("info"), soft: v("info-soft"), solid: v("info-solid") },
      },
      fontFamily: {
        sans: ['"Inter Variable"', "ui-sans-serif", "system-ui", "-apple-system", "Segoe UI", "sans-serif"],
        mono: ['"JetBrains Mono Variable"', "ui-monospace", "SFMono-Regular", "Menlo", "Consolas", "monospace"],
      },
      fontSize: {
        "2xs": ["11px", { lineHeight: "16px" }],
        xs: ["12px", { lineHeight: "16px" }],
        sm: ["13px", { lineHeight: "20px" }],
        base: ["14px", { lineHeight: "22px" }],
        lg: ["16px", { lineHeight: "24px" }],
        xl: ["18px", { lineHeight: "26px" }],
        "2xl": ["22px", { lineHeight: "30px" }],
      },
      borderRadius: { DEFAULT: "6px", md: "6px", lg: "8px", xl: "10px" },
      boxShadow: {
        xs: "0 1px 2px 0 rgb(0 0 0 / 0.04)",
        sm: "0 1px 3px 0 rgb(0 0 0 / 0.06), 0 1px 2px -1px rgb(0 0 0 / 0.04)",
        pop: "0 8px 24px -6px rgb(0 0 0 / 0.16), 0 0 0 1px rgb(var(--line) / 1)",
        dialog: "0 24px 48px -12px rgb(0 0 0 / 0.28), 0 0 0 1px rgb(var(--line) / 1)",
      },
      keyframes: {
        "fade-in": { from: { opacity: "0" }, to: { opacity: "1" } },
        "zoom-in": { from: { opacity: "0", transform: "translate(-50%, -48%) scale(0.98)" }, to: { opacity: "1", transform: "translate(-50%, -50%) scale(1)" } },
        "slide-in-left": { from: { transform: "translateX(-100%)" }, to: { transform: "translateX(0)" } },
        "slide-in-right": { from: { transform: "translateX(100%)" }, to: { transform: "translateX(0)" } },
        pulse: { "0%, 100%": { opacity: "1" }, "50%": { opacity: "0.35" } },
        shimmer: { "100%": { transform: "translateX(100%)" } },
      },
      animation: {
        "fade-in": "fade-in 120ms ease-out",
        "zoom-in": "zoom-in 140ms ease-out",
        "slide-in-left": "slide-in-left 180ms ease-out",
        "slide-in-right": "slide-in-right 180ms ease-out",
        "pulse-dot": "pulse 1.6s ease-in-out infinite",
      },
    },
  },
  plugins: [],
} satisfies Config;
