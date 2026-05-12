// Mobile design system tokens — verbatim from the Claude Design handoff
// (molecules-ai-mobile-app/project/shared.jsx). Kept as an inline-style
// palette object so screens can mirror the design 1:1; theming routes
// through `usePalette(dark)` exactly like the prototype.

export interface MobilePalette {
  bg: string;
  surface: string;
  surface2: string;
  border: string;
  divider: string;
  text: string;
  text2: string;
  text3: string;

  green: string;
  greenSoft: string;
  greenInk: string;

  t1Bg: string; t1Ink: string; t1Br: string;
  t2Bg: string; t2Ink: string; t2Br: string;
  t3Bg: string; t3Ink: string; t3Br: string;
  t4Bg: string; t4Ink: string; t4Br: string;

  t4SoftCard: string;

  online: string;
  starting: string;
  degraded: string;
  failed: string;
  paused: string;
  offline: string;

  remote: string;
  remoteBg: string;
  accent: string;
}

export const MOL_LIGHT: MobilePalette = {
  bg: "#f6f4ef",
  surface: "#ffffff",
  surface2: "#fbf9f4",
  border: "rgba(40,30,20,0.08)",
  divider: "rgba(40,30,20,0.06)",
  text: "#29261b",
  text2: "rgba(41,38,27,0.62)",
  text3: "rgba(41,38,27,0.42)",

  green: "#2f9e6a",
  greenSoft: "#d9ebe0",
  greenInk: "#1f6a47",

  t1Bg: "#dde6f1", t1Ink: "#3a6aa3", t1Br: "#b9c8de",
  t2Bg: "#dbe5f4", t2Ink: "#2f5fb4", t2Br: "#b1c2e0",
  t3Bg: "#e3dcef", t3Ink: "#6a4ba1", t3Br: "#c8b9e1",
  t4Bg: "#f5dcc7", t4Ink: "#a8501d", t4Br: "#e8c6a4",

  t4SoftCard: "#f9ece0",

  online: "#2f9e6a",
  starting: "#e9b53b",
  degraded: "#d28a2a",
  failed: "#c8472a",
  paused: "#7a8696",
  offline: "#9aa0a6",

  remote: "#7a4dd1",
  remoteBg: "#ede2ff",
  accent: "#2f9e6a",
};

export const MOL_DARK: MobilePalette = {
  bg: "#15140f",
  surface: "#1d1c17",
  surface2: "#22211c",
  border: "rgba(255,250,240,0.08)",
  divider: "rgba(255,250,240,0.06)",
  text: "#f1eee5",
  text2: "rgba(241,238,229,0.6)",
  text3: "rgba(241,238,229,0.38)",

  green: "#3eb37c",
  greenSoft: "#1f3a2c",
  greenInk: "#7fd3a8",

  t1Bg: "#1a2230", t1Ink: "#7ea4d4", t1Br: "#2a3a52",
  t2Bg: "#1b2434", t2Ink: "#86a6e2", t2Br: "#2c3c58",
  t3Bg: "#251f33", t3Ink: "#b39be0", t3Br: "#3e3450",
  t4Bg: "#332316", t4Ink: "#e5a878", t4Br: "#553622",

  t4SoftCard: "#2a1f17",

  online: "#3eb37c",
  starting: "#e9b53b",
  degraded: "#d28a2a",
  failed: "#d65a3e",
  paused: "#8a96a6",
  offline: "#6a6a6a",

  remote: "#a38aff",
  remoteBg: "#2a1f44",
  accent: "#3eb37c",
};

/**
 * Pure-function variant of palette resolution. No React, no context,
 * no mutation — for tests and other non-component code.
 *
 * Components should import `usePalette` from `./palette-context` so the
 * user's accent override (held in context, not in module state) flows
 * through automatically. Re-exported below so the existing
 * `import { usePalette } from "./palette"` call sites keep working.
 */
export const getPalette = (dark: boolean): MobilePalette => (dark ? MOL_DARK : MOL_LIGHT);

// Back-compat re-export. Once we're confident nothing imports
// `usePalette` from this file we can drop this line.
export { usePalette } from "./palette-context";

// References the CSS variables that next/font/google emits in
// app/layout.tsx. Falls through to system fonts if the variable is
// undefined (e.g. in unit tests with no <body> font class).
export const MOBILE_FONT_SANS = "var(--font-inter), 'Inter', ui-sans-serif, system-ui, sans-serif";
export const MOBILE_FONT_MONO = "var(--font-jetbrains), 'JetBrains Mono', ui-monospace, monospace";

// Status keys we surface in the mobile UI. Anything else from the
// platform falls back to "offline" tinting — the desktop has more
// statuses ("provisioning", etc.) than the design's 6-key palette.
export type MobileStatus =
  | "online" | "starting" | "degraded" | "failed" | "paused" | "offline";

export function normalizeStatus(s: string | undefined | null): MobileStatus {
  if (s === "online" || s === "degraded" || s === "failed" || s === "paused" || s === "offline") {
    return s;
  }
  if (s === "provisioning" || s === "starting") return "starting";
  return "offline";
}

// Platform tier (number 1-4) → design tier code "T1".."T4"
export function tierCode(tier: number | undefined | null): "T1" | "T2" | "T3" | "T4" {
  const n = typeof tier === "number" ? tier : 2;
  if (n <= 1) return "T1";
  if (n === 2) return "T2";
  if (n === 3) return "T3";
  return "T4";
}
