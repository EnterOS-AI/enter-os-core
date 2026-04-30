package middleware

import (
	"os"
	"strings"

	"github.com/gin-gonic/gin"
)

// flyReplaySrcHeader is the header Fly injects on requests it replays via
// the `fly-replay: ...;state=...` mechanism. Format is a semicolon-
// separated list of k=v pairs, e.g.
//   instance=91854...;region=ord;t=1700000000000;state=<uuid>
// Control plane puts the bare UUID in state (no prefix) because Fly's
// proxy returns 502 "replay malformed" on any second `=` in the value.
// We read the whole state= segment as the org id.
const flyReplaySrcHeader = "Fly-Replay-Src"

// Tenant-mode guard — public repo's only SaaS hook.
//
// The SaaS control plane (private `molecule-controlplane` repo) provisions one
// platform instance per customer org on Fly Machines and sets:
//   - MOLECULE_ORG_ID=<uuid>                       (env on the machine)
//   - forwards requests with X-Molecule-Org-Id=<uuid> (control-plane router)
//
// TenantGuard wraps every non-allowlisted route so a mis-routed request from
// another org bounces with 404 (not 403 — don't leak existence).
//
// When MOLECULE_ORG_ID is unset (self-hosted / dev / CI), the guard is a
// passthrough — self-hosters see no behavior change.
//
// The guard intentionally knows nothing about orgs, signup, billing, or
// provisioning. Those live in the private control-plane repo. All this code
// does is: "am I the tenant for this request? if not, 404."

// tenantOrgIDHeader is the HTTP header the control-plane router sets when it
// uses fly-replay to route a request to a tenant machine. Case-insensitive at
// the HTTP layer (Gin normalizes).
const tenantOrgIDHeader = "X-Molecule-Org-Id"

// tenantGuardAllowlist is the set of paths that MUST remain accessible even in
// tenant mode without the org header (health checks, Prometheus scrapes,
// workspace → platform boot signals).
// Exact-match — no prefix semantics — to avoid accidentally exposing admin
// routes via e.g. "/health/debug/admin".
//
// /registry/register and /registry/heartbeat are workspace-initiated boot
// signals. Workspace EC2s are provisioned by the control plane with
// PLATFORM_URL but no MOLECULE_ORG_ID env var, so the runtime's httpx
// calls can't attach X-Molecule-Org-Id. Tenant SG already scopes these
// ports to the VPC CIDR; the registry handlers themselves enforce
// workspace-scoped bearer auth via wsauth.HasAnyLiveToken. Allowlisting
// here only bypasses the cross-org routing check, not auth.
var tenantGuardAllowlist = map[string]struct{}{
	"/health":             {},
	"/buildinfo":          {},
	"/metrics":            {},
	"/registry/register":  {},
	"/registry/heartbeat": {},
}

// TenantGuard returns a Gin middleware configured from the MOLECULE_ORG_ID env
// var. Reads env once at construction — changing the env at runtime requires
// a restart (matches every other platform env var). Pass the orgID directly to
// TenantGuardWithOrgID if you need to test a specific configuration without
// mutating the process environment.
func TenantGuard() gin.HandlerFunc {
	return TenantGuardWithOrgID(strings.TrimSpace(os.Getenv("MOLECULE_ORG_ID")))
}

// TenantGuardWithOrgID is the constructor used by tests; ordinary callers use
// TenantGuard. When configuredOrgID is empty the guard is a no-op.
func TenantGuardWithOrgID(configuredOrgID string) gin.HandlerFunc {
	if configuredOrgID == "" {
		return func(c *gin.Context) { c.Next() }
	}
	return func(c *gin.Context) {
		if _, ok := tenantGuardAllowlist[c.Request.URL.Path]; ok {
			c.Next()
			return
		}
		// /cp/* is reverse-proxied to the control plane. The CP has its
		// own auth (WorkOS session cookie + admin bearer) so the tenant
		// doesn't need to attach org identity here. Bypassing the guard
		// avoids blocking the proxy with a 404 that would then look
		// like the CP is down.
		//
		// SECURITY NOTE: this pass-through is only safe because:
		//   (a) cp_proxy enforces its own explicit path allowlist
		//       (see router/cp_proxy.go cpProxyAllowedPrefixes) so
		//       traversal to admin-surface endpoints is blocked.
		//   (b) tenant SG has no :8080 inbound; only the Cloudflare
		//       tunnel reaches the platform. A future SG change that
		//       opens :8080 to the VPC would also open this path to
		//       unauthenticated /cp/* probing — tighten cp_proxy's
		//       allowlist OR remove this bypass if that happens.
		if strings.HasPrefix(c.Request.URL.Path, "/cp/") {
			c.Next()
			return
		}
		// Primary: explicit X-Molecule-Org-Id header (direct access path,
		// e.g. from molecli or internal tooling that sets it directly).
		if c.GetHeader(tenantOrgIDHeader) == configuredOrgID {
			c.Next()
			return
		}
		// Secondary: org id encoded in Fly-Replay-Src state by the control
		// plane. This is the path every production request takes, because
		// response headers set by the cp don't travel to the replayed
		// tenant — only the state= param does.
		if orgIDFromReplaySrc(c.GetHeader(flyReplaySrcHeader)) == configuredOrgID {
			c.Next()
			return
		}
		// Tertiary: same-origin Canvas requests on tenant EC2 instances where
		// Caddy serves Canvas (:3000) and API (:8080) under the same domain.
		// CANVAS_PROXY_URL is set → Referer/Origin matches Host → trusted.
		if isSameOriginCanvas(c) {
			c.Next()
			return
		}
		// 404 not 403 — existence of this tenant must not be inferable by
		// probing other orgs' machines.
		c.AbortWithStatus(404)
	}
}

// orgIDFromReplaySrc extracts the org id the control plane put in the
// fly-replay state= segment. Value is the bare UUID — the control plane
// deliberately doesn't prefix it because Fly 502s on any `=` in the state
// value. Returns "" if the header is missing or has no state segment.
// Separated from TenantGuardWithOrgID so tests can round-trip header →
// id without spinning a full Gin context.
func orgIDFromReplaySrc(header string) string {
	if header == "" {
		return ""
	}
	for _, seg := range strings.Split(header, ";") {
		seg = strings.TrimSpace(seg)
		const statePrefix = "state="
		if strings.HasPrefix(seg, statePrefix) {
			return seg[len(statePrefix):]
		}
	}
	return ""
}
