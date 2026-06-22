"use client";

// React context for accent overrides + the React-side `usePalette` hook.
// Keeps the pure data (MOL_LIGHT/MOL_DARK) in palette.ts and the
// pure-function `getPalette` available for tests; this file is the
// React-only entry point so mobile components don't have to plumb
// accent through props.

import { createContext, useContext, type ReactNode } from "react";

import { MOL_DARK, MOL_LIGHT, type MobilePalette } from "./palette";

const MobileAccentContext = createContext<string | null>(null);

export function MobileAccentProvider({
  accent,
  children,
}: {
  accent: string | null;
  children: ReactNode;
}) {
  return <MobileAccentContext.Provider value={accent}>{children}</MobileAccentContext.Provider>;
}

/**
 * Hook variant of palette resolution. Reads the user's accent override
 * from context and returns a fresh palette object with the override
 * applied. Critically, it never mutates the static MOL_LIGHT/MOL_DARK
 * singletons — that was the foot-gun the prior version had.
 *
 * Outside of a `<MobileAccentProvider>`, the context default of `null`
 * means we just return the static palette unchanged. That's the right
 * behaviour for tests + for any non-mobile caller that imports a token.
 */
export function usePalette(dark: boolean): MobilePalette {
  const accent = useContext(MobileAccentContext);
  const base = dark ? MOL_DARK : MOL_LIGHT;
  if (!accent || accent === base.accent) return base;
  return { ...base, accent, online: accent };
}
