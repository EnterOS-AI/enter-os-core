import type { Metadata } from "next";
import { cookies, headers } from "next/headers";
import "./globals.css";
import { AuthGate } from "@/components/AuthGate";
import { CookieConsent } from "@/components/CookieConsent";
import { ThemeProvider } from "@/lib/theme-provider";
import {
  THEME_COOKIE,
  readThemeCookie,
  themeBootScript,
} from "@/lib/theme-cookie";

export const metadata: Metadata = {
  title: "Molecule AI",
  description: "AI Org Chart Canvas",
};

export default async function RootLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  // Read the per-request CSP nonce that middleware.ts sets via the
  // `x-nonce` request header. This call is load-bearing for THREE
  // independent reasons:
  //
  //   1. It opts the root layout into dynamic rendering. Without a
  //      `headers()` / `cookies()` / `noStore()` call, Next.js treats
  //      the layout as statically pre-rendered and serves the SAME
  //      HTML for every request — which means the Next.js bootstrap
  //      <script> tags bake into the HTML without any nonce. The
  //      browser then rejects every one with a CSP violation because
  //      the header demands nonce-only script execution.
  //
  //   2. Next.js 15 propagates the nonce to its own generated inline
  //      scripts (the __next_f chunk push frames) ONLY when the header
  //      is actually read via `headers()`. The header's existence on
  //      the request isn't enough — Next.js watches for the read.
  //
  //   3. We need the nonce to attach to the inline theme boot script
  //      below, otherwise CSP rejects it in production where
  //      script-src is `'self' 'nonce-{nonce}' 'strict-dynamic'`.
  //      'strict-dynamic' propagates trust from a nonce'd script to
  //      scripts it inserts, but does NOT forgive an un-nonce'd
  //      sibling — the boot script must carry its own nonce.
  const hdrs = await headers();
  const nonce = hdrs.get("x-nonce") ?? undefined;

  // SSR: read the user's saved preference. For light/dark we can stamp
  // data-theme on <html> here so the very first paint matches; for
  // "system" we leave the attribute off and let the inline boot script
  // resolve from matchMedia before paint.
  const cookieStore = await cookies();
  const theme = readThemeCookie(cookieStore.get(THEME_COOKIE)?.value);
  const initialDataTheme = theme === "system" ? undefined : theme;

  return (
    // suppressHydrationWarning on <html>: the inline boot script below
    // mutates `data-theme` before React hydrates (system mode reads
    // matchMedia + writes the attribute). That's the entire point of the
    // script — eliminate the flash — and it's the documented escape hatch
    // for "the server-rendered HTML is intentionally not what React would
    // produce client-side at this exact attribute."
    <html lang="en" data-theme={initialDataTheme} suppressHydrationWarning>
      <head>
        {/*
         * Boot script: runs synchronously before the body paints, sets
         * data-theme on <html> for "system" preference based on the OS
         * media query. For explicit light/dark, SSR already set the
         * attribute above and the script's write is a no-op.
         *
         * `nonce` comes from middleware's per-request CSP nonce — see
         * the comment block above for why CSP requires this even though
         * the page also has 'strict-dynamic'.
         */}
        <script
          nonce={nonce}
          dangerouslySetInnerHTML={{ __html: themeBootScript }}
        />
      </head>
      <body className="bg-surface text-ink">
        <ThemeProvider initialTheme={theme}>
          {/* AuthGate is a client component; it checks the session on mount
              and bounces anonymous users to the control plane's login page
              when running on a tenant subdomain. Non-SaaS hosts (localhost,
              vercel preview URL, apex) pass through unchanged. */}
          <AuthGate>{children}</AuthGate>
          <CookieConsent />
        </ThemeProvider>
      </body>
    </html>
  );
}
