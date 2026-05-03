/**
 * Theme cookie constants + boot script.
 *
 * No "use client" pragma — these are imported by both server components
 * (app/layout.tsx, which calls cookies() during SSR) and client
 * components (lib/theme-provider.tsx). Constants exported from a
 * "use client" file get rewritten by Next.js as client-reference
 * placeholders, so a server importer sees a Function instead of the
 * underlying value. Keeping shared primitives here avoids that trap.
 *
 * Aligned with molecule-app's matching module — same cookie name, same
 * three-value enum — so the preference follows the user across surfaces
 * (app, market, landing, canvas) when the cookie is set with
 * Domain=.moleculesai.app.
 */

export type ThemePreference = "system" | "light" | "dark";
export type ResolvedTheme = "light" | "dark";

export const THEME_COOKIE = "mol_theme";

export function readThemeCookie(value: string | undefined): ThemePreference {
  if (value === "light" || value === "dark" || value === "system") {
    return value;
  }
  return "system";
}

/**
 * Inline boot script. Stringified verbatim by app/layout.tsx so it runs
 * synchronously before the body paints — preventing a flash of the wrong
 * theme. Reads cookie via document.cookie regex (no parser available
 * yet), falls back to matchMedia, and stamps data-theme on <html>.
 *
 * Must remain tiny and dependency-free — runs before hydration. The
 * canvas's middleware sets a strict CSP with nonce-based script-src in
 * production; the layout passes the nonce on the <script> tag so this
 * passes the inline-script gate.
 */
export const themeBootScript = `(()=>{try{var m=document.cookie.match(/(?:^|;\\s*)${THEME_COOKIE}=(system|light|dark)/);var p=m?m[1]:"system";var r=p==="system"?(window.matchMedia("(prefers-color-scheme: dark)").matches?"dark":"light"):p;document.documentElement.dataset.theme=r;}catch(e){}})();`;
