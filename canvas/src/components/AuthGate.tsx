"use client";

/**
 * AuthGate wraps the canvas root so every page is gated on a valid session.
 * Anonymous users get bounced to app.moleculesai.app/cp/auth/login?return_to=<here>.
 *
 * In non-SaaS mode (no tenant slug — local dev, apex, vercel preview URL),
 * the gate is a pass-through: canvas works without auth for local dev.
 * This mirrors the control plane's "disabled provider" fallback.
 */
import { useEffect, useState, type ReactNode } from "react";
import { fetchSession, redirectToLogin, type Session } from "@/lib/auth";
import { getTenantSlug } from "@/lib/tenant";

export type AuthGateState =
  | { kind: "loading" }
  | { kind: "anonymous"; skipRedirect: boolean }
  | { kind: "authenticated"; session: Session };

export function AuthGate({ children }: { children: ReactNode }) {
  const [state, setState] = useState<AuthGateState>({ kind: "loading" });

  useEffect(() => {
    // In non-SaaS mode (no tenant slug) we skip the gate entirely —
    // local dev, vercel preview URLs, and the app.moleculesai.app apex
    // should not force login for API-only interactions.
    const slug = getTenantSlug();
    if (!slug) {
      setState({ kind: "anonymous", skipRedirect: true });
      return;
    }
    // Never gate /cp/auth/* paths — these ARE the login pages.
    if (typeof window !== "undefined" && window.location.pathname.startsWith("/cp/auth/")) {
      setState({ kind: "anonymous", skipRedirect: true });
      return;
    }
    let cancelled = false;
    fetchSession()
      .then((s) => {
        if (cancelled) return;
        if (s) {
          setState({ kind: "authenticated", session: s });
        } else {
          setState({ kind: "anonymous", skipRedirect: false });
        }
      })
      .catch(() => {
        // Network error — fail closed (show signin) so a transient
        // outage doesn't leak the canvas UI to an unauth'd user.
        if (!cancelled) setState({ kind: "anonymous", skipRedirect: false });
      });
    return () => {
      cancelled = true;
    };
  }, []);

  useEffect(() => {
    if (state.kind === "anonymous" && !state.skipRedirect) {
      redirectToLogin("sign-in");
    }
  }, [state]);

  if (state.kind === "loading") {
    // Zinc-950 backdrop matches the canvas background so the browser
    // never paints a white flash while the session round-trip resolves.
    return <div className="fixed inset-0 bg-surface" aria-hidden="true" />;
  }
  if (state.kind === "anonymous" && !state.skipRedirect) {
    // Redirect already firing from the effect above; render nothing in
    // the interim to avoid a flash of unauthenticated content.
    return null;
  }
  return <>{children}</>;
}
