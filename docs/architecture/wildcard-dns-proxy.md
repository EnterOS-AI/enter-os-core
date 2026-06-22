# Wildcard DNS + Cloudflare Worker Proxy

> **Status:** Planned — replaces per-tenant DNS record creation.
>
> **Problem:** When a user creates an org, we create an EC2 instance and a
> Cloudflare A record pointing `<slug>.moleculesai.app` to the instance IP.
> This causes 3-5 min of DNS propagation + NXDOMAIN caching by ISPs, meaning
> users see "site can't be reached" for minutes after creating their org.
>
> **Solution:** Every SaaS (Vercel, Railway, Fly.io, WordPress, n8n) uses the
> same pattern: wildcard DNS + a reverse proxy that routes by hostname.

---

## Architecture

```
Browser → https://acme.moleculesai.app
          ↓
   *.moleculesai.app DNS → Cloudflare (proxied, orange cloud)
          ↓
   Cloudflare Worker (edge, ~50ms)
     1. Extract slug from hostname
     2. Lookup backend IP from CP API (cached 60s)
     3. If no backend → return "provisioning" splash page
     4. Proxy request to EC2 instance
          ↓
   EC2 tenant (platform :8080, canvas :3000)
```

## Why this fixes the DNS problem

| Before (per-tenant DNS) | After (wildcard + proxy) |
|--------------------------|--------------------------|
| Create A record per org | Wildcard `*.moleculesai.app` exists once, forever |
| 3-5 min DNS propagation | Zero — wildcard already resolves |
| NXDOMAIN cached by ISP for hours | Never happens — domain always resolves |
| Let's Encrypt cert per EC2 (~30s) | Cloudflare handles TLS (wildcard or per-host, free) |
| Caddy on each EC2 for HTTPS | Caddy only needed for local reverse proxy (HTTP, no TLS) |
| DNS cleanup on org delete | No DNS records to clean up |

## Components

### 1. Cloudflare DNS (one-time setup)

Add a single wildcard record in the Cloudflare dashboard:

```
Type: A
Name: *
Content: 0.0.0.0 (placeholder — Worker intercepts before it reaches this)
Proxy: ON (orange cloud — routes through Cloudflare)
TTL: Auto
```

The `0.0.0.0` content doesn't matter because the Worker intercepts every
request before Cloudflare would try to connect to the origin. The orange
cloud (proxy ON) is required for Workers to fire on the route.

Also keep the explicit records for non-tenant subdomains:
- `api.moleculesai.app` → Railway (control plane)
- `app.moleculesai.app` → Vercel (customer dashboard)
- `moleculesai.app` → Vercel (landing page)

These explicit records take priority over the wildcard.

### 2. Cloudflare Worker (~50 lines)

The Worker runs on every request to `*.moleculesai.app` that isn't matched
by an explicit DNS record. It:

1. **Extracts the slug** from the `Host` header
2. **Looks up the backend IP** using a 3-tier cache strategy:
   - **L1: in-memory cache** (60s TTL) — fastest, per-isolate
   - **L2: Workers KV** (5 min TTL, stale-while-revalidate) — survives isolate
     restarts, shared across all edge locations
   - **L3: CP API** — `GET https://api.moleculesai.app/cp/orgs/<slug>/instance`
   - **Fallback:** if CP is unreachable, serve stale KV entry (any age) rather
     than erroring. A 10-minute CP outage is invisible to tenants.
   - If the org doesn't exist (404 from CP, no KV entry) → 404 page
   - If the org is provisioning (no IP yet) → return a static "provisioning" HTML page
3. **Proxies the request** to `http://<ec2-ip>:8080` (platform) or `:3000` (canvas)
   - Route: `/health`, `/workspaces*`, `/registry*`, etc. → `:8080`
   - Route: everything else → `:3000`
   - Route: `/ws` → `:8080` with WebSocket upgrade (see WebSocket section below)
   - Injects `X-Molecule-Org-Id` header (same as Caddy does today)
   - Injects `Origin` header for AdminAuth bypass
   - Injects `X-Forwarded-For` with client IP from `CF-Connecting-IP`
   - Injects `X-Forwarded-Proto: https`
4. **Returns the response** to the browser with Cloudflare's TLS

#### WebSocket proxying

Cloudflare Workers support WebSocket proxying via the `upgradeHeader` check.
The Worker detects `Upgrade: websocket` on incoming requests and passes them
through to the EC2 backend on `:8080/ws`. The Worker acts as a transparent
tunnel — it does not inspect or buffer WebSocket frames.

```js
// Simplified WebSocket handling in the Worker
if (request.headers.get('Upgrade') === 'websocket') {
  return fetch(`http://${backendIp}:8080${url.pathname}`, request);
}
```

If Workers WebSocket proxying proves unreliable in production (frame drops,
idle timeout issues), Phase 33.3 keeps Caddy as a thin WSocket-only reverse
proxy on EC2 instead of removing it entirely.

#### Trusted proxy configuration

The platform's Gin server uses `SetTrustedProxies(nil)` (trust all) by
default. When requests come through the Worker instead of directly, the
platform should trust `CF-Connecting-IP` for the real client IP. In
production, set `TRUSTED_PROXIES` to Cloudflare's published IP ranges
(auto-updated from `https://api.cloudflare.com/client/v4/ips`).

### 3. CP API endpoint: `GET /cp/orgs/:slug/instance`

New public endpoint (no auth — needed by the Worker which has no session):

```json
// GET /cp/orgs/acme/instance
// 200 when running:
{
  "slug": "acme",
  "status": "running",
  "ip": "<EC2_IP>",
  "region": "us-east-2"
}

// 200 when provisioning:
{
  "slug": "acme",
  "status": "provisioning",
  "ip": null
}

// 404 when org doesn't exist
```

**Security note:** This endpoint exposes the EC2 IP for a given slug. This is
equivalent to what DNS already exposes (A record → IP). No secrets are leaked.
The endpoint should be rate-limited to prevent enumeration.

### 4. EC2 tenant changes

With Cloudflare handling TLS, the EC2 instance no longer needs Caddy for HTTPS:

**Before:**
```
Caddy (:443, auto Let's Encrypt) → platform (:8080) / canvas (:3000)
```

**After:**
```
Worker → EC2 :8080 (platform, direct HTTP)
Worker → EC2 :3000 (canvas, direct HTTP)
```

Caddy can be removed from the EC2 user-data script for HTTP routing. If
WebSocket proxying through Workers proves reliable, Caddy is fully removed.
If not, Caddy stays as a thin WebSocket-only reverse proxy (no TLS, no
HTTP routing — just `/ws` → `:8080`).

The EC2 security group should allow inbound HTTP from Cloudflare IPs only
(not public). **Automate the IP list** — Cloudflare publishes their ranges
at `https://api.cloudflare.com/client/v4/ips`. Use a Lambda or cron to
update the SG weekly. Do not hardcode the IP ranges.

**Headers injected by Worker** (replaces Caddy's `header_up`):
- `X-Molecule-Org-Id: <org-id>` — for TenantGuard
- `Origin: https://<slug>.moleculesai.app` — for AdminAuth
- `X-Forwarded-For: <client-ip>` — for rate limiting
- `X-Forwarded-Proto: https` — so the platform knows the original scheme

### 5. Provisioning splash page

When the Worker detects `status: "provisioning"`, it returns a static HTML
page with:
- The EnterOS logo
- "Setting up your workspace..."
- A progress animation
- Auto-refresh every 5s (meta refresh or JS fetch)

This replaces the molecule-app provisioning page for direct subdomain visits.
The molecule-app provisioning page at `app.moleculesai.app/orgs/:slug/provisioning`
continues to work as the primary flow (redirect after org creation).

## Migration plan

1. **Phase 1: Deploy Worker + wildcard DNS** (no tenant changes)
   - Worker proxies to existing EC2 instances (Caddy still running)
   - Both paths work: direct DNS (old A records) + Worker proxy (new)
   - Verify Worker routing works for existing tenants

2. **Phase 2: Stop creating per-tenant DNS records**
   - Update CP provisioner to skip Cloudflare A record creation
   - Remove Cloudflare DNS cleanup from deprovision
   - Existing A records coexist with wildcard (explicit wins)

3. **Phase 3: Remove Caddy from EC2 user-data**
   - Worker handles TLS + routing
   - EC2 runs platform on :8080 and canvas on :3000 (plain HTTP)
   - Simpler boot script, ~30s faster cold start

4. **Phase 4: Clean up old A records**
   - Delete per-tenant A records (wildcard handles everything)
   - Remove Cloudflare client from CP provisioner

## Cost

- Cloudflare Worker: free tier = 100k requests/day. Paid = $5/mo for 10M.
- Wildcard DNS: free (Cloudflare).
- Savings: no more per-instance Let's Encrypt, no Caddy install time.

## Files to change

| File | Change |
|------|--------|
| `the private control-plane repo/internal/provisioner/ec2.go` | Remove Cloudflare DNS creation, remove Caddy from user-data |
| `the private control-plane repo/internal/cloudflareapi/dns.go` | Eventually removable (Worker replaces it) |
| `the private control-plane repo/internal/handlers/orgs.go` | Add `GET /cp/orgs/:slug/instance` endpoint |
| New: `Molecule-AI/molecule-tenant-proxy (separate repo)` | Worker source + wrangler.toml |
| `docs/runbooks/saas-secrets.md` | Add Worker secrets (CF account ID, API token) |
| `.github/workflows/deploy-worker.yml` | CI/CD for Worker deploys |

## References

- [Cloudflare Workers docs](https://developers.cloudflare.com/workers/)
- [Vercel's routing architecture](https://vercel.com/docs/edge-network/overview) — same pattern
- [Railway custom domains](https://docs.railway.app/guides/public-networking#custom-domains) — same pattern
