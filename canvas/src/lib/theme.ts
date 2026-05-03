/**
 * Theme tokens — semantic, light + dark.
 *
 * Source of truth for colours lives in app/globals.css as CSS custom
 * properties (`--color-surface`, `--color-ink`, etc.). Tailwind v4
 * generates utilities (`bg-surface`, `text-ink`, ...) from those tokens
 * automatically; that's the preferred consumption path.
 *
 * This module exports `cssVar()` for the rare case where an inline
 * `style={{}}` prop or SVG fill needs a token value — the returned
 * `var(--color-foo)` string follows the live theme without re-renders.
 */

export type ColorToken =
  // Warm-paper surface (light-flippable)
  | "surface"
  | "surface-elevated"
  | "surface-sunken"
  | "surface-card"
  | "line"
  | "line-soft"
  | "ink"
  | "ink-mid"
  | "ink-soft"
  | "accent"
  | "accent-strong"
  | "warm"
  | "good"
  | "bad"
  // Always-dark (terminal / console / log surfaces)
  | "bg"
  | "bg-elev"
  | "bg-card"
  | "line-strong"
  | "ink-mute"
  | "ink-dim"
  | "accent-dim"
  | "plasma"
  | "warn";

export function cssVar(token: ColorToken): string {
  return `var(--color-${token})`;
}
