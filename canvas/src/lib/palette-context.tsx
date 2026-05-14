"use client";

/**
 * palette-context.tsx
 *
 * Mobile canvas accent palette system.
 *
 * - MOL_LIGHT / MOL_DARK  — immutable base singletons
 * - getPalette(accent, isDark) — returns base palette or accent-overridden copy
 * - normalizeStatus(status, isDark) — maps workspace status → online dot color
 * - tierCode(tier) — maps tier number → display label
 * - MobileAccentProvider — React context that propagates accent override
 * - usePalette(allowAccentOverride) — hook; returns the effective palette
 */

import { createContext, useContext } from "react";

// ─── Types ─────────────────────────────────────────────────────────────────────

export interface Palette {
  /** Accent colour (CSS colour string). */
  accent: string;
  /** Online indicator colour (CSS class string, e.g. "bg-emerald-400"). */
  online: string;
  /** Surface background colour class. */
  surface: string;
  /** Primary text colour class. */
  ink: string;
  /** Border/divider colour class. */
  line: string;
  /** Background colour class. */
  bg: string;
  /** Tier display code, e.g. "T1". */
  tier: string;
}

// ─── Singleton base palettes ────────────────────────────────────────────────────

/** Light-mode base palette — must never be mutated. */
export const MOL_LIGHT: Readonly<Palette> = Object.freeze({
  accent: "bg-blue-500",
  online: "bg-emerald-400",
  surface: "bg-zinc-900",
  ink: "text-zinc-100",
  line: "border-zinc-700",
  bg: "bg-zinc-950",
  tier: "T1",
});

/** Dark-mode base palette — must never be mutated. */
export const MOL_DARK: Readonly<Palette> = Object.freeze({
  accent: "bg-sky-400",
  online: "bg-emerald-400",
  surface: "bg-zinc-800",
  ink: "text-zinc-100",
  line: "border-zinc-700",
  bg: "bg-zinc-950",
  tier: "T1",
});

// ─── Pure helpers ─────────────────────────────────────────────────────────────

/**
 * Maps workspace status string → online dot colour class.
 * Returns the appropriate green for light/dark mode.
 */
export function normalizeStatus(
  status: string,
  _isDark: boolean,
): string {
  if (status === "online" || status === "degraded") {
    return "bg-emerald-400";
  }
  if (status === "failed") {
    return "bg-red-400";
  }
  if (status === "paused" || status === "not_configured") {
    return "bg-amber-400";
  }
  return "bg-zinc-400";
}

/**
 * Maps tier number → display code.
 */
export function tierCode(tier: number): string {
  return `T${tier}`;
}

/**
 * Returns the effective palette.
 *
 * - `accent = null` → base palette (light or dark) unchanged
 * - `accent = basePalette.accent` → base palette unchanged (identity guard)
 * - `accent = "#custom"` → copy with `accent` and `online` overridden
 *
 * Always returns a new object; neither MOL_LIGHT nor MOL_DARK is ever mutated.
 */
export function getPalette(
  accent: string | null,
  isDark: boolean,
): Palette {
  const base: Readonly<Palette> = isDark ? MOL_DARK : MOL_LIGHT;

  // null accent → use base unchanged
  if (accent === null) return { ...base };

  // identity guard — accent same as base accent → no override needed
  if (accent === base.accent) return { ...base };

  // Custom accent: override accent + online to keep them in sync
  return { ...base, accent, online: normalizeStatus("online", isDark) };
}

// ─── Context ──────────────────────────────────────────────────────────────────

type MobileAccentContextValue = {
  /** Override accent colour (null = no override, use default). */
  accent: string | null;
};

const MobileAccentContext = createContext<MobileAccentContextValue>({
  accent: null,
});

export { MobileAccentContext };

/**
 * Renders children inside the accent override context.
 */
export function MobileAccentProvider({
  accent,
  children,
}: {
  accent: string | null;
  children: React.ReactNode;
}) {
  return (
    <MobileAccentContext.Provider value={{ accent }}>
      {children}
    </MobileAccentContext.Provider>
  );
}

// ─── Hook ─────────────────────────────────────────────────────────────────────

/**
 * Returns the effective `Palette` for the current context.
 *
 * @param allowAccentOverride  When false, always returns the base palette
 *                              even when an override is set (useful for
 *                              non-accent-aware child components).
 */
export function usePalette(allowAccentOverride: boolean): Palette {
  const { accent } = useContext(MobileAccentContext);

  // Resolved from the OS-level theme preference. In a real app this would
  // be derived from useTheme().resolvedTheme; for this hook we default
  // to light (the safe default for SSR / component-library use).
  // We read data-theme from <html> to stay in sync with the theme system.
  const isDark =
    typeof document !== "undefined" &&
    document.documentElement.dataset.theme === "dark";

  const effectiveAccent = allowAccentOverride ? accent : null;
  return getPalette(effectiveAccent, isDark);
}
