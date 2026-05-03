"use client";

import { useTheme, type ThemePreference } from "@/lib/theme-provider";

const OPTIONS: { value: ThemePreference; label: string; icon: string }[] = [
  // Sun: explicit light
  {
    value: "light",
    label: "Light",
    icon: "M12 3v1.5M12 19.5V21M4.22 4.22l1.06 1.06M18.72 18.72l1.06 1.06M3 12h1.5M19.5 12H21M4.22 19.78l1.06-1.06M18.72 5.28l1.06-1.06M16 12a4 4 0 11-8 0 4 4 0 018 0z",
  },
  // Monitor: follow OS
  {
    value: "system",
    label: "System",
    icon: "M3 5h18v11H3zM8 21h8M9 21l1-5h4l1 5",
  },
  // Moon: explicit dark
  {
    value: "dark",
    label: "Dark",
    icon: "M21 12.79A9 9 0 1111.21 3 7 7 0 0021 12.79z",
  },
];

/**
 * Three-way preference picker: System / Light / Dark.
 *
 * Highlights the user's *picked* preference, not the resolved render
 * mode. So "System" stays highlighted while the screen renders dark
 * (because the OS is dark) — that's the user's mental model: "I told
 * the app to follow my OS."
 *
 * Aligned with molecule-app/components/theme-toggle.tsx so the picker
 * behaves identically across surfaces.
 */
export function ThemeToggle({ className = "" }: { className?: string }) {
  const { theme, setTheme } = useTheme();

  return (
    <div
      role="radiogroup"
      aria-label="Theme preference"
      className={`inline-flex items-center gap-0.5 rounded-md border border-line bg-surface-sunken p-0.5 ${className}`}
    >
      {OPTIONS.map((opt) => {
        const active = theme === opt.value;
        return (
          <button
            key={opt.value}
            type="button"
            role="radio"
            aria-checked={active}
            aria-label={opt.label}
            onClick={() => setTheme(opt.value)}
            className={
              "flex h-6 w-6 items-center justify-center rounded transition-colors " +
              (active
                ? "bg-surface-elevated text-ink shadow-sm"
                : "text-ink-soft hover:text-ink-mid")
            }
          >
            <svg
              width={13}
              height={13}
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              strokeWidth="1.6"
              strokeLinecap="round"
              strokeLinejoin="round"
              aria-hidden="true"
            >
              <path d={opt.icon} />
            </svg>
          </button>
        );
      })}
    </div>
  );
}
