/**
 * Canvas-side session detection. Calls /cp/auth/me on the control plane
 * (via same-origin → PLATFORM_URL) and returns the session or null.
 *
 * 401 is the "anonymous" signal and does NOT throw — the caller decides
 * whether to redirect. Network errors do throw so React error boundaries
 * can surface them.
 */
import { PLATFORM_URL } from "./api";
import { SaaSHostSuffix } from "./tenant";

export interface Session {
  user_id: string;
  org_id: string;
  email: string;
}

// Base path prefix for auth endpoints on the control plane.
const AUTH_BASE = "/cp/auth";

// Auth UI lives on the "app" subdomain (app.moleculesai.app), NOT on
// tenant subdomains (hongmingwang.moleculesai.app). Tenant subdomains
// proxy to EC2 platform which has no auth routes.
function getAuthOrigin(): string {
  if (typeof window === "undefined") return PLATFORM_URL;
  const host = window.location.hostname;
  if (host.endsWith(SaaSHostSuffix)) {
    return `${window.location.protocol}//app${SaaSHostSuffix}`;
  }
  return PLATFORM_URL;
}

/**
 * fetchSession probes /cp/auth/me with the session cookie (credentials:
 * include mandatory cross-origin). Returns the Session on 200, null on
 * 401 (anonymous), throws on anything else so callers don't silently
 * treat a 5xx as "not logged in".
 */
export async function fetchSession(): Promise<Session | null> {
  const res = await fetch(`${PLATFORM_URL}${AUTH_BASE}/me`, {
    credentials: "include",
  });
  if (res.status === 401) return null;
  if (!res.ok) {
    throw new Error(`/cp/auth/me: ${res.status} ${res.statusText}`);
  }
  return res.json();
}

/**
 * redirectToLogin bounces the browser to the control plane's login page
 * with a `return_to` param so the user lands back on the current URL
 * after signup/login completes. Same-origin safety is enforced on the
 * CP side (isSafeReturnTo rejects cross-domain / http / protocol-
 * relative URLs). Uses window.location.href so the full URL including
 * query + hash survives the round trip.
 */
export function redirectToLogin(screenHint: "sign-up" | "sign-in" = "sign-in"): void {
  if (typeof window === "undefined") return;
  // Guard against infinite redirect loop: if we're already on the login
  // page, don't redirect again (each redirect double-encodes return_to
  // until the URL exceeds header limits → 431).
  if (window.location.pathname.startsWith("/cp/auth/")) return;
  const returnTo = window.location.href;
  const path = screenHint === "sign-up" ? "signup" : "login";
  const authOrigin = getAuthOrigin();
  const dest = `${authOrigin}${AUTH_BASE}/${path}?return_to=${encodeURIComponent(returnTo)}`;
  window.location.href = dest;
}

/**
 * signOut posts to /cp/auth/signout to clear the WorkOS session cookie
 * + revoke at the provider, then bounces to the auth-origin login page.
 *
 * Best-effort by design: a 5xx, network failure, or stale cookie still
 * results in the browser navigation away from the authenticated app —
 * leaving the user on a logged-in-looking page after they clicked
 * "Sign out" is the worst possible UX. The cookie is cleared client-
 * visibly via the redirect target's response (Set-Cookie with maxAge=-1
 * runs even on a non-200 path). If the user is already anonymous, the
 * POST 401s harmlessly + we still redirect.
 *
 * Throws nothing — callers can disable the button optimistically or
 * await this and trust it returns. On a redirect-blocked test
 * environment (jsdom under vitest) we still exit cleanly so unit tests
 * can spy on the fetch call.
 */
export async function signOut(): Promise<void> {
  // Fire-and-tolerate the POST. credentials:include is mandatory cross-
  // origin so the SaaS canvas (acme.moleculesai.app) can hit
  // app.moleculesai.app/cp/auth/signout with the session cookie.
  try {
    await fetch(`${getAuthOrigin()}${AUTH_BASE}/signout`, {
      method: "POST",
      credentials: "include",
    });
  } catch {
    // Ignore — we still redirect below.
  }
  if (typeof window === "undefined") return;
  // Land on the login screen rather than the current URL: returning to
  // a tenant URL after signout would just re-redirect through
  // /cp/auth/login due to AuthGate. Send the user straight there with
  // no return_to so they don't loop back into the org they just left.
  const authOrigin = getAuthOrigin();
  window.location.href = `${authOrigin}${AUTH_BASE}/login`;
}
