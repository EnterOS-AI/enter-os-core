# Same-Origin Canvas Fetches — the /cp/* Reverse Proxy

> How EnterOS's SaaS Canvas makes browser API calls to two backends
> through one origin — and why the `/cp/*` proxy makes multi-tenant
> deployment simpler and safer.

**PRs:** #1095 (`feat/tenant-cp-proxy-same-origin`) | **Status:** ✅ Merged

---

## The problem: two backends, one browser origin

Canvas (EnterOS's browser UI) makes API calls to two distinct services:

| Service | What it does | Example endpoints |
|---|---|---|
| **Tenant platform** | Your EnterOS workspace management | `/workspaces`, `/approvals/pending` |
| **Control Plane (CP)** | Org-level operations, billing, auth verification | `/cp/auth/me`, `/cp/orgs`, `/cp/billing/checkout` |

Before this change, Canvas had to call both services directly from the browser. That meant:

- Two separate base URLs in the browser bundle (`NEXT_PUBLIC_PLATFORM_URL` for tenant, another for CP)
- CORS preflight complexity — cross-origin calls need explicit `Access-Control-Allow-*` headers on the CP
- Cookie domain issues — WorkOS session cookies scoped to `.moleculesai.app` aren't sent to a custom tenant domain

The result was a fragile configuration that complicated tenant provisioning.

## The fix: server-side split, same-origin fetches

The tenant platform now runs a `/cp/*` reverse proxy. Canvas makes **all** calls to its single `NEXT_PUBLIC_PLATFORM_URL` (the tenant). The tenant splits the traffic:

```
Browser → tenant.moleculesai.app
  ├── /workspaces, /approvals/pending, /channels/*  → handled locally
  └── /cp/*                                     → reverse-proxied upstream to CP
```

The browser never knows there are two backends. No CORS, no cookie domain mismatches, no extra env vars for Canvas to configure.

---

## Architecture at a glance

```
Browser (Canvas)
    │
    │  GET /cp/auth/me   (or any /cp/* path)
    ▼
Tenant Platform (:8080)
    │
    │  Reverse proxy: forward Cookie + Authorization headers
    ▼
Control Plane (api.moleculesai.app)
    │
    │  WorkOS session cookie → verify membership
    ▼
Response flows back through tenant → browser
```

The proxy:
- **Does NOT strip** `Cookie` or `Authorization` headers — they carry the WorkOS session cookie needed by the CP
- **Does rewrite** the `Host` header so CP middleware (CORS checks, cookie-domain logic) sees the CP origin, not the tenant
- **Does NOT strip** `X-Forwarded-For` — upstream uses it for audit and rate limiting

---

## Security: fail-closed allowlist

The proxy does **not** forward arbitrary `/cp/*` paths. An explicit allowlist gates every upstream route **before** cookies leave the tenant:

| Allowed prefix | What Canvas uses it for |
|---|---|
| `/cp/auth/` | Session verification: `GET /cp/auth/me`, `GET /cp/auth/tenant-member` |
| `/cp/orgs` | Org listing, provision status, export |
| `/cp/billing/` | Checkout and billing portal |
| `/cp/templates` | Template registry reads |
| `/cp/legal/` | Terms of service document (served from CP) |

**Every other `/cp/*` path returns 404**, not 403. The 404 prevents leaking which CP routes exist to an attacker probing the proxy.

### Why an allowlist instead of a denylist

`/cp/admin/*` endpoints accept WorkOS session cookies as a valid auth tier. A tenant-authed browser user could craft a request to `/cp/admin/tenants/other-slug/diagnostics` — without the allowlist, the tenant would happily forward their cookie upstream. The CP would see a legitimate admin session and honor the request, turning any tenant into a lateral-movement hop. The allowlist is the structural fix.

---

## Configuration

**For SaaS tenants:** No configuration needed. The control plane provisioner sets `CP_UPSTREAM_URL` automatically at tenant launch.

```bash
# What the provisioner sets:
CP_UPSTREAM_URL=https://api.moleculesai.app
```

**For self-hosted / local dev:** `CP_UPSTREAM_URL` is unset. The `/cp/*` proxy is never mounted. Canvas connects directly to the local platform — behaviour is unchanged.

**For operators investigating:** If Canvas admin pages (billing, org switcher) return 502, check that `CP_UPSTREAM_URL` is reachable from the tenant platform's network.

---

## What changed in the browser bundle

Canvas's Next.js build sets one base URL:

```typescript
// NEXT_PUBLIC_PLATFORM_URL = https://<tenant-slug>.moleculesai.app
const res = await fetch(`${process.env.NEXT_PUBLIC_PLATFORM_URL}/cp/auth/me`, {
  credentials: 'include',   // send WorkOS session cookie
});
```

Previously Canvas needed two separate env vars and conditional logic to choose the right base URL for each call. That conditional logic is gone — one URL, server-side routing.

---

## AdminAuth + WorkOS session verification

The `/cp/*` proxy enables a related improvement: **browser-based admin authentication**.

Canvas runs in the browser and authenticates users via a WorkOS session cookie (scoped to `.moleculesai.app`). It has no bearer token — the `ADMIN_TOKEN` scheme is for CLI and server-to-server callers, not browser users.

AdminAuth now accepts a session-verification tier that runs **before** the bearer check:

1. If a `Cookie` header is present **and** `CP_UPSTREAM_URL` is configured → the tenant platform calls `GET /cp/auth/tenant-member?slug=<tenant-slug>` upstream with the same cookie. 200 + `member: true` → grant admin access.
2. If the upstream says no, or no cookie is present → fall through to the existing bearer-token path.

Positive verifications are cached **30 seconds** (keyed by `sha256(slug + cookie)`), so a burst of Canvas admin-page renders doesn't hammer the CP. Negative results (invalid session) are cached **5 seconds** to absorb retry bursts without fan-out. Logout and role changes propagate within that window.

For **self-hosted** and **local dev** deployments, `CP_UPSTREAM_URL` is unset → this feature is disabled, behaviour is unchanged.

---

## Code references

| File | What it does |
|---|---|
| `workspace-server/internal/router/cp_proxy.go` | `/cp/*` reverse proxy + allowlist |
| `workspace-server/internal/middleware/session_auth.go` | WorkOS session verification + 30s cache |
| `workspace-server/internal/router/router.go` | Mounts proxy when `CP_UPSTREAM_URL` set |
| `canvas/src/middleware.ts` | Simplified Canvas fetch base — one URL |

---

## What this means for you

- **SaaS tenants**: Canvas Just Works after provisioning. No extra env vars for browser API calls.
- **Self-hosted operators**: No change — your Canvas talks to your local platform as before.
- **Platform contributors**: If a new Canvas UI fetch needs a `/cp/*` path, add it to `cpProxyAllowedPrefixes` in `cp_proxy.go`. The allowlist means you must opt in — no accidental exposure.
