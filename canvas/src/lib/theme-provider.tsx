"use client";

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
} from "react";
import {
  THEME_COOKIE,
  type ResolvedTheme,
  type ThemePreference,
} from "@/lib/theme-cookie";

// Re-export so callers can keep `import { THEME_COOKIE, type ThemePreference } from "@/lib/theme-provider"`
// working — but for server-component imports, prefer the underlying module
// directly to dodge the "use client" serialization wrapper.
export { THEME_COOKIE, themeBootScript } from "@/lib/theme-cookie";
export type { ThemePreference, ResolvedTheme } from "@/lib/theme-cookie";

/**
 * Theme system: System / Light / Dark.
 *
 * `theme`         — what the user picked. Persisted in the `mol_theme`
 *                   cookie so it survives reloads and (when set to a
 *                   parent domain) follows the user across moleculesai.app
 *                   surfaces (app, market, docs, landing, canvas).
 * `resolvedTheme` — the mode actually rendered. Equal to `theme` when the
 *                   user picked light or dark; equal to the OS preference
 *                   when they picked system.
 *
 * No-flash on first paint is handled by the inline `<script>` in
 * app/layout.tsx, which runs before hydration and stamps data-theme on
 * <html> based on cookie + matchMedia. This provider then takes over on
 * mount and keeps the attribute in sync with state changes.
 */

type ThemeContextValue = {
  theme: ThemePreference;
  resolvedTheme: ResolvedTheme;
  setTheme: (next: ThemePreference) => void;
};

const ThemeContext = createContext<ThemeContextValue | null>(null);

/**
 * Cookie attributes:
 *  - `Domain=.moleculesai.app` so the preference follows the user across
 *    canvas.moleculesai.app, app.moleculesai.app, market.moleculesai.app,
 *    docs.moleculesai.app, AND tenant subdomains (acme.moleculesai.app,
 *    acme.staging.moleculesai.app, ...). All match `endsWith(".moleculesai.app")`.
 *    Skipped on localhost (browser would reject Domain= for a
 *    non-public-suffix host).
 *  - `Max-Age=1y` — long-lived; users rarely change theme.
 *  - `SameSite=Lax` — fine for a UI preference; not security-sensitive.
 *  - `Secure` only in production HTTPS contexts.
 */
function writeThemeCookie(value: ThemePreference): void {
  if (typeof document === "undefined") return;
  const isProdHost =
    typeof window !== "undefined" &&
    window.location.hostname.endsWith(".moleculesai.app");
  const parts = [
    `${THEME_COOKIE}=${value}`,
    "Path=/",
    "Max-Age=31536000",
    "SameSite=Lax",
  ];
  if (isProdHost) {
    parts.push("Domain=.moleculesai.app");
    parts.push("Secure");
  }
  document.cookie = parts.join("; ");
}

function applyResolvedTheme(resolved: ResolvedTheme): void {
  if (typeof document === "undefined") return;
  document.documentElement.dataset.theme = resolved;
}

export function ThemeProvider({
  initialTheme,
  children,
}: {
  initialTheme: ThemePreference;
  children: React.ReactNode;
}) {
  const [theme, setThemeState] = useState<ThemePreference>(initialTheme);
  const [systemPref, setSystemPref] = useState<ResolvedTheme>("light");

  // Track OS preference when the user is on "system". Only registers a
  // listener while theme === "system" so we don't pay listener cost in
  // explicit modes.
  useEffect(() => {
    if (typeof window === "undefined") return;
    const mql = window.matchMedia("(prefers-color-scheme: dark)");
    setSystemPref(mql.matches ? "dark" : "light");
    if (theme !== "system") return;
    const onChange = (e: MediaQueryListEvent) =>
      setSystemPref(e.matches ? "dark" : "light");
    mql.addEventListener("change", onChange);
    return () => mql.removeEventListener("change", onChange);
  }, [theme]);

  const resolvedTheme: ResolvedTheme =
    theme === "system" ? systemPref : theme;

  // Reflect resolvedTheme onto <html data-theme>. The inline boot script
  // already did this once before hydration; this keeps it in sync after.
  useEffect(() => {
    applyResolvedTheme(resolvedTheme);
  }, [resolvedTheme]);

  const setTheme = useCallback((next: ThemePreference) => {
    setThemeState(next);
    writeThemeCookie(next);
  }, []);

  const value = useMemo<ThemeContextValue>(
    () => ({ theme, resolvedTheme, setTheme }),
    [theme, resolvedTheme, setTheme],
  );

  return (
    <ThemeContext.Provider value={value}>{children}</ThemeContext.Provider>
  );
}

// Defaults returned when no <ThemeProvider> is in the tree. Real app
// always wraps via app/layout.tsx; this fallback exists so unit tests
// rendering components in isolation don't have to know about theme.
// setTheme is a no-op — there's no state to mutate without a provider —
// and the noopTheme reference is stable so consumers using it in deps
// arrays don't churn.
const noopTheme: ThemeContextValue = {
  theme: "system",
  resolvedTheme: "light",
  setTheme: () => {},
};

export function useTheme(): ThemeContextValue {
  return useContext(ThemeContext) ?? noopTheme;
}
