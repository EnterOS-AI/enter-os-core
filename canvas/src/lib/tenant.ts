/**
 * Tenant slug derivation for SaaS-mode canvas.
 *
 * When canvas is served at <slug>.moleculesai.app the org slug comes from
 * the browser's hostname. When served anywhere else (localhost, Vercel
 * preview URL, direct vercel.app) we fall back to a configured slug
 * (NEXT_PUBLIC_DEFAULT_ORG_SLUG) or an empty string — API calls without
 * a slug hit the control plane's non-tenant routes.
 */

// SaaSHostSuffix is the domain this canvas is the tenant UI for. Parent
// domain with a leading dot; the hostname must end with this to be
// recognized as a tenant subdomain. Defaults to `.moleculesai.app` but
// is overridable via NEXT_PUBLIC_SAAS_HOST_SUFFIX for multi-brand or
// staging environments.
export const SaaSHostSuffix =
  process.env.NEXT_PUBLIC_SAAS_HOST_SUFFIX ?? ".moleculesai.app";

// reservedSubdomains mirrors the control plane's list so we don't
// accidentally treat canvas-itself subdomains as tenant slugs when the
// user lands on e.g. app.moleculesai.app directly.
const reservedSubdomains = new Set([
  "app",
  "www",
  "api",
  "admin",
  "cp",
  "dashboard",
  "billing",
  "status",
  "docs",
]);

/**
 * getTenantSlug returns the tenant slug for the current request.
 *
 * Client-side: reads window.location.hostname.
 * Server-side (SSR / build): reads NEXT_PUBLIC_DEFAULT_ORG_SLUG, which is
 *   unset in production SaaS (we never SSR tenant pages without a host)
 *   but useful for local dev when the app is served at localhost:3000.
 *
 * Returns "" if no slug can be derived — callers must handle that case
 * (usually by redirecting to app.moleculesai.app for signup/org picker).
 */
export function getTenantSlug(): string {
  if (typeof window === "undefined") {
    return process.env.NEXT_PUBLIC_DEFAULT_ORG_SLUG ?? "";
  }
  const host = window.location.hostname.toLowerCase();
  if (!host.endsWith(SaaSHostSuffix)) {
    return process.env.NEXT_PUBLIC_DEFAULT_ORG_SLUG ?? "";
  }
  const slug = host.slice(0, host.length - SaaSHostSuffix.length);
  if (reservedSubdomains.has(slug)) return "";
  return slug;
}

/**
 * isSaaSTenant reports whether the canvas is running as the UI for a
 * SaaS tenant (served at <slug>.moleculesai.app). Use for client-side
 * UX branches that should behave differently on SaaS vs self-hosted —
 * e.g. the workspace tier picker hides T1/T2/T3 sandbox tiers because
 * every SaaS workspace gets its own EC2 VM (inherently T4 Full Access).
 *
 * SSR-safe: returns false on the server to avoid hydration drift; call
 * sites should tolerate a flip from false→true on first client render.
 */
export function isSaaSTenant(): boolean {
  if (typeof window === "undefined") return false;
  return getTenantSlug() !== "";
}
