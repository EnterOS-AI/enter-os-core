import { getTenantSlug } from "./tenant";

// When NEXT_PUBLIC_PLATFORM_URL is set to "" (empty string), the canvas
// uses relative paths — correct for the combined tenant image where Go
// platform + canvas run on the same port via reverse proxy. The `??`
// operator preserves "" as a valid value; `||` would fall through to
// the localhost default.
export const PLATFORM_URL =
  process.env.NEXT_PUBLIC_PLATFORM_URL ?? "http://localhost:8080";

// 15s is long enough for slow CP queries but short enough that a
// hung backend doesn't leave the UI spinning forever. The abort
// propagates through AbortController so React components can observe
// the error and render a retry affordance. Callers that know the
// endpoint is intentionally slow (org import walks a tree of
// workspaces with server-side pacing) can pass `timeoutMs` to
// override.
const DEFAULT_TIMEOUT_MS = 15_000;

export interface RequestOptions {
  timeoutMs?: number;
}

/**
 * Build the platform auth header set used by every authenticated fetch
 * from the canvas. Returns a fresh object so callers can mutate (e.g.
 * append `Content-Type` for JSON requests, omit it for FormData).
 *
 * SaaS cross-origin shape:
 *  - `X-Molecule-Org-Slug` — derived from `window.location.hostname`
 *    by `getTenantSlug()`. Control plane uses it for fly-replay
 *    routing. Empty on localhost / non-tenant hosts — safe to omit.
 *  - `Authorization: Bearer <token>` — `NEXT_PUBLIC_ADMIN_TOKEN` baked
 *    into the canvas build (see canvas/Dockerfile L8/L11). Required by
 *    the workspace-server when `ADMIN_TOKEN` is set on the server side
 *    (Tier-2b AdminAuth gate, wsauth_middleware.go ~L245). Empty when
 *    no admin token was provisioned — the Tier-1 session-cookie path
 *    handles that case via `credentials:"include"`.
 *
 * Why a shared helper: the two-line "read env, attach bearer; read
 * slug, attach header" pattern was duplicated across `request()` and
 * 7 raw-fetch callsites (chat uploads/download + 5 Attachment*
 * components) before this consolidation. A new poller or raw fetch
 * that forgets one of the two headers silently 401s against
 * workspace-server when ADMIN_TOKEN is set — the exact bug shape
 * called out in #178 / closes the post-#176 self-review gap.
 *
 * Callers that want JSON Content-Type should spread this and add it
 * themselves; FormData callers should NOT add Content-Type (the
 * browser sets the multipart boundary). Centralizing the auth pair
 * but leaving Content-Type up to the caller is the minimum viable
 * shared shape.
 */
export function platformAuthHeaders(): Record<string, string> {
  const headers: Record<string, string> = {};
  const slug = getTenantSlug();
  if (slug) headers["X-Molecule-Org-Slug"] = slug;
  const adminToken = process.env.NEXT_PUBLIC_ADMIN_TOKEN;
  if (adminToken) headers["Authorization"] = `Bearer ${adminToken}`;
  return headers;
}

async function request<T>(
  method: string,
  path: string,
  body?: unknown,
  retryCount = 0,
  options?: RequestOptions,
): Promise<T> {
  // JSON-bodied request — Content-Type is JSON. Auth pair comes from
  // the shared helper; see its doc comment for the SaaS-shape rationale.
  const headers: Record<string, string> = {
    "Content-Type": "application/json",
    ...platformAuthHeaders(),
  };
  // Re-read slug locally for the 401 handler below — `headers` already
  // has it, but the 401 branch needs the bare value to gate the
  // session-probe + redirect logic on tenant context.
  const slug = getTenantSlug();

  const res = await fetch(`${PLATFORM_URL}${path}`, {
    method,
    headers,
    body: body ? JSON.stringify(body) : undefined,
    credentials: "include",
    signal: AbortSignal.timeout(options?.timeoutMs ?? DEFAULT_TIMEOUT_MS),
  });
  // Transient rate-limit recovery. A single IP bucket can momentarily
  // spike on page load (several panels hydrate simultaneously). Instead
  // of bubbling up a 429 that blanks the Canvas, wait the
  // Retry-After window and try once — any further 429 surfaces normally.
  // GET / idempotent methods only; never auto-retry mutations.
  if (res.status === 429 && retryCount === 0 && method === "GET") {
    const retryAfterHeader = res.headers.get("Retry-After");
    const retryAfter = retryAfterHeader ? parseInt(retryAfterHeader, 10) : NaN;
    const delayMs = Number.isFinite(retryAfter) ? Math.min(retryAfter, 20) * 1000 : 2000;
    await new Promise((resolve) => setTimeout(resolve, delayMs));
    return request<T>(method, path, body, retryCount + 1, options);
  }
  if (res.status === 401) {
    // Distinguish "session is dead" from "this endpoint refused this
    // token." Old behaviour blanket-redirected on every 401, so a
    // single transient 401 from a workspace-scoped endpoint
    // (/workspaces/:id/peers, /plugins, etc. that need a workspace
    // token rather than the tenant admin bearer) yanked the user
    // back to AuthKit even when their session was perfectly fine.
    // That broke the staging-tabs E2E for the entire 2026-04-25
    // night; #2073/#2074 worked around the symptom in the test by
    // mocking 401→200 for every fetch, but the user-facing bug
    // stayed.
    //
    // The canonical "session is dead" signal is /cp/auth/me
    // returning 401. For any 401 on a non-auth path, probe
    // /cp/auth/me before deciding to redirect:
    //   - probe 401 → session is actually dead → redirect
    //   - probe 200 → session is fine, the endpoint just refused
    //                 our specific token → throw a real error,
    //                 caller renders an error state
    //   - probe network error → assume session-fine (conservative;
    //                 better to throw than to redirect on a
    //                 transient probe failure)
    //
    // Self-hosted / localhost / reserved subdomains still throw
    // without redirecting (slug is empty in those cases) — same
    // policy as before.
    const isAuthPath = path.startsWith("/cp/auth/");
    let sessionDead = isAuthPath;
    if (!isAuthPath && slug) {
      try {
        const probe = await fetch(`${PLATFORM_URL}/cp/auth/me`, {
          credentials: "include",
          signal: AbortSignal.timeout(5000),
        });
        sessionDead = probe.status === 401;
      } catch {
        // Probe failed (network/timeout) — fall through to throw.
      }
    }
    if (sessionDead && slug) {
      const { redirectToLogin } = await import("./auth");
      redirectToLogin("sign-in");
      throw new Error("Session expired — redirecting to login");
    }
    throw new Error(`API ${method} ${path}: 401 ${await res.text()}`);
  }
  if (!res.ok) {
    const text = await res.text();
    // Recognise the platform's structured "datastore unreachable"
    // shape (returned by wsauth_middleware.abortAuthLookupError when
    // Postgres/Redis is down). Surface as a typed error so callers
    // can render a dedicated diagnostic instead of a generic toast.
    if (res.status === 503 && text) {
      try {
        const parsed = JSON.parse(text) as { code?: string; error?: string };
        if (parsed.code === "platform_unavailable") {
          throw new PlatformUnavailableError(parsed.error || "platform datastore unavailable");
        }
      } catch (err) {
        // Re-throw the typed error if that's what we just constructed.
        // JSON.parse failures fall through to the generic Error below.
        if (err instanceof PlatformUnavailableError) throw err;
      }
    }
    throw new Error(`API ${method} ${path}: ${res.status} ${text}`);
  }
  return res.json();
}

/** Thrown when the platform reports its datastore (Postgres/Redis) is
 *  unreachable. Surface with a dedicated diagnostic UI rather than a
 *  generic API-error toast — the user's next action is to check local
 *  services, not to retry the API call. */
export class PlatformUnavailableError extends Error {
  readonly code = "platform_unavailable" as const;
  constructor(message: string) {
    super(message);
    this.name = "PlatformUnavailableError";
  }
}

export const api = {
  get: <T>(path: string, options?: RequestOptions) => request<T>("GET", path, undefined, 0, options),
  post: <T>(path: string, body?: unknown, options?: RequestOptions) => request<T>("POST", path, body, 0, options),
  patch: <T>(path: string, body?: unknown, options?: RequestOptions) => request<T>("PATCH", path, body, 0, options),
  put: <T>(path: string, body?: unknown, options?: RequestOptions) => request<T>("PUT", path, body, 0, options),
  del: <T>(path: string, options?: RequestOptions) => request<T>("DELETE", path, undefined, 0, options),
};
