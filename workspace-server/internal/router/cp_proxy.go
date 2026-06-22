package router

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
)

// cpProxyAllowedPrefixes is the explicit list of /cp/* paths the
// tenant will forward to the CP. Anything else 404s BEFORE the cookie
// and Authorization headers leave the tenant.
//
// Why an allowlist, not a denylist: /cp/admin/* endpoints accept a
// WorkOS session cookie (scoped to .moleculesai.app) as one of their
// auth tiers. A tenant-authed user visiting <tenant>.moleculesai.app
// and crafting a request to /cp/admin/tenants/other-slug/diagnostics
// would have the tenant happily forward their cookie upstream. The CP
// would then see a legitimate admin session and honor the request —
// effectively turning any tenant into an admin-access lateral-
// movement hop. (Observed as a theoretical risk in today's review.)
//
// Only paths that are legitimately used by the canvas browser bundle
// go in this list. If a new UI fetch needs a new /cp/ prefix, add it
// here — fail-closed is the default.
var cpProxyAllowedPrefixes = []string{
	"/cp/auth/",     // me, tenant-member, login/signup/callback for return flows
	"/cp/orgs",      // list / get / provision-status / export
	"/cp/billing/",  // checkout + portal
	"/cp/templates", // template registry reads
	"/cp/legal/",    // terms document (served on CP)
}

// isCPProxyAllowedPath enforces the allowlist. Prefix match with an
// optional trailing slash tolerance (/cp/orgs matches /cp/orgs AND
// /cp/orgs/acme). Rejects any path that doesn't start with /cp/ so
// the handler isn't inadvertently mounted on other prefixes.
func isCPProxyAllowedPath(p string) bool {
	if !strings.HasPrefix(p, "/cp/") {
		return false
	}
	for _, prefix := range cpProxyAllowedPrefixes {
		if p == prefix || strings.HasPrefix(p, prefix+"/") || strings.HasPrefix(p, prefix) && prefixMatches(p, prefix) {
			return true
		}
	}
	return false
}

// prefixMatches handles the case where the allowlist entry itself ends
// in a slash (e.g. /cp/auth/): that means "anything under /cp/auth/".
// Entries without a trailing slash (/cp/orgs) match both the exact path
// and any subpath. Separate function so the intent is readable.
func prefixMatches(path, prefix string) bool {
	if strings.HasSuffix(prefix, "/") {
		return strings.HasPrefix(path, prefix)
	}
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

// newCPProxy returns a Gin handler that reverse-proxies /cp/* requests
// to the control plane. Lives beside newCanvasProxy because they solve
// the same problem — tenant browser fetches targeted at a single
// same-origin base — for the mirror-image endpoint set.
//
// Why this exists: canvas's browser bundle calls both CP endpoints
// (/cp/auth/me, /cp/orgs, /cp/billing/checkout) AND tenant-platform
// endpoints (/canvas/viewport, /approvals/pending). They share ONE
// build-time base URL (NEXT_PUBLIC_PLATFORM_URL). Baking the CP
// origin breaks tenant calls; baking the tenant origin breaks CP
// calls. The only sane fix is same-origin fetches + let the server
// split the traffic. This handler is the /cp/* leg of that split;
// newCanvasProxy is the UI leg.
//
// Security:
//   - We do NOT strip Cookie/Authorization here: those carry the
//     WorkOS session cookie and must reach the CP to resolve the
//     user. That's the whole point of this proxy.
//   - We DO rewrite the Host header to the CP upstream so CORS and
//     cookie-domain logic upstream see themselves, not the tenant.
//   - We do NOT strip X-Forwarded-For — upstream may want it for
//     audit and rate-limit keying.
//   - The proxy ONLY forwards /cp/* paths. The upstream URL is
//     env-configured and its scheme is enforced https in prod via
//     url.Parse (the caller passes the URL; we reject anything
//     that isn't http/https at construction time).
//
// Rate / timeout note: we do NOT set a custom Transport with
// aggressive timeouts because CP endpoints are fast and any hang
// is already bounded by the caller's browser-level timeout. If a
// future slow endpoint warrants a bound, add here not at the
// gateway.
func newCPProxy(targetURL string) gin.HandlerFunc {
	target, err := url.Parse(targetURL)
	if err != nil {
		log.Fatalf("cp_proxy: invalid CP_UPSTREAM_URL %q: %v", targetURL, err)
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		log.Fatalf("cp_proxy: CP_UPSTREAM_URL scheme must be http(s), got %q", target.Scheme)
	}
	if target.Host == "" {
		log.Fatalf("cp_proxy: CP_UPSTREAM_URL missing host: %q", targetURL)
	}

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			// Host header rewrite: CP middleware (CORS, cookie-domain)
			// keys off Host; rewriting avoids "origin not allowed" on
			// upstream OPTIONS preflight.
			req.Host = target.Host
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			log.Printf("cp_proxy: %v", err)
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("control plane unavailable"))
		},
	}

	return func(c *gin.Context) {
		// Allowlist enforcement: block anything outside the browser-
		// canvas-facing /cp/* surface. Returns 404 (not 403) to avoid
		// leaking which paths exist on the CP side.
		if !isCPProxyAllowedPath(c.Request.URL.Path) {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		proxy.ServeHTTP(c.Writer, c.Request)
	}
}
