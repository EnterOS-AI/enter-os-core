package middleware

import (
	"crypto/subtle"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/orgtoken"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/wsauth"
	"github.com/gin-gonic/gin"
)

// abortAuthLookupError is the single response shape for "the auth
// middleware tried to validate a token but the underlying datastore
// lookup failed." Returns 503 (not 500) because the right semantic
// is "platform infrastructure unavailable, retry shortly" — not
// "internal server error in our application logic". The structured
// `code` lets the canvas distinguish this from generic 5xx and
// surface a dedicated diagnostic ("Postgres/Redis unreachable —
// check local services") instead of a confusing
// `auth check failed` toast.
//
// `where` is included in the log line so the operator can grep
// which call site fired (WorkspaceAuth vs AdminAuth, the
// HasAnyLiveTokenGlobal probe vs orgtoken.Validate). The
// user-visible body deliberately does NOT include the underlying
// error string — that could leak DB hostnames, connection-string
// fragments, or internal code paths.
func abortAuthLookupError(c *gin.Context, where string, err error) {
	log.Printf("wsauth: %s: datastore lookup failed (returning 503): %v", where, err)
	c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
		"error": "platform datastore unavailable — retry shortly",
		"code":  "platform_unavailable",
	})
}

// WorkspaceAuth returns a Gin middleware that enforces per-workspace bearer-token
// authentication on /workspaces/:id/* sub-routes.
//
// Strict: every request MUST carry Authorization: Bearer <token> matching a
// live (non-revoked) token for the workspace. No grace period, no fail-open.
//
// History: originally this middleware had a lazy-bootstrap grace period for
// pre-Phase-30.1 workspaces without a live token, so rolling upgrades didn't
// brick in-flight agents. #318 tightened the fake-UUID leak (non-existent
// workspace IDs were falling through). #351 then showed the remaining hole:
// test-artifact workspaces from prior DAST runs still exist in the DB with
// empty configs and no tokens, so they pass WorkspaceExists + fall through
// the grace period — leaking global-secret key names to any unauth caller on
// the Docker network. Phase 30.1 shipped months ago; every live workspace has
// since gone through multiple boot cycles and acquired a token. The grace
// period no longer serves legitimate traffic. Removing it entirely closes
// #351 without affecting registration (which is on /registry/register,
// outside this middleware's scope).
//
// Intended for route groups that cover all /workspaces/:id/* paths.
// The /workspaces/:id/a2a route must be registered on the root router (outside
// this group) because it already authenticates callers via CanCommunicate.
func WorkspaceAuth(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		workspaceID := c.Param("id")
		if workspaceID == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing workspace ID"})
			return
		}
		ctx := c.Request.Context()

		tok := wsauth.BearerTokenFromHeader(c.GetHeader("Authorization"))
		if tok != "" {
			// Admin token fallback — lets the canvas dashboard read workspace
			// activity, traces, delegations with a single admin credential.
			adminSecret := os.Getenv("ADMIN_TOKEN")
			if adminSecret != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(adminSecret)) == 1 {
				c.Next()
				return
			}
			// Org-scoped API token — user-minted from canvas UI. Grants
			// access to EVERY workspace in the org (that's the explicit
			// product spec: one org key can touch each workspace). Same
			// power surface as ADMIN_TOKEN but named, revocable, audited.
			// Check before per-workspace token so an org-key presenter
			// doesn't hit the narrower ValidateToken failure path.
			if id, prefix, orgID, err := orgtoken.Validate(ctx, database, tok); err == nil {
				c.Set("org_token_id", id)
				c.Set("org_token_prefix", prefix)
				// org_id may be "" for pre-migration tokens (NULL column).
				// Don't set the context key in that case so downstream callers
				// can distinguish "unanchored token" (exists==false) from
				// "anchored to this org" (exists==true, value non-empty).
				if orgID != "" {
					c.Set("org_id", orgID)
				}
				c.Next()
				return
			} else if !errors.Is(err, orgtoken.ErrInvalidToken) {
				abortAuthLookupError(c, "WorkspaceAuth: orgtoken.Validate", err)
				return
			}
			// Per-workspace token — narrowest scope, bound to this :id.
			if err := wsauth.ValidateToken(ctx, database, workspaceID, tok); err != nil {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid workspace auth token"})
				return
			}
			c.Next()
			return
		}
		// Same-origin canvas on tenant image — Referer matches Host.
		if isSameOriginCanvas(c) {
			c.Next()
			return
		}
		// Local-dev escape hatch — see devmode.go. Unreachable on SaaS
		// (hosted tenants always have ADMIN_TOKEN + MOLECULE_ENV=production).
		if isDevModeFailOpen() {
			c.Next()
			return
		}
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing workspace auth token"})
	}
}

// AdminAuth returns a Gin middleware for global/admin routes (e.g.
// /settings/secrets, /admin/secrets) that have no per-workspace scope.
//
// # Credential tier (evaluated in order)
//
//  1. Lazy-bootstrap fail-open: if no live workspace token exists anywhere on
//     the platform (fresh install / pre-Phase-30 upgrade), every request passes
//     through so existing deployments keep working.
//
//  2. ADMIN_TOKEN env var (recommended, closes #684): when set, the bearer
//     MUST equal this value exactly (constant-time comparison). Workspace
//     bearer tokens are intentionally rejected even if valid — a compromised
//     workspace agent must not be able to read global secrets, steal GitHub App
//     installation tokens, or enumerate pending approvals across the platform.
//     Set ADMIN_TOKEN to a strong random secret (e.g. openssl rand -base64 32).
//
//  3. Fallback — workspace token (deprecated, backward-compat): when
//     ADMIN_TOKEN is not set and workspace tokens do exist globally, any valid
//     workspace bearer token is still accepted. This preserves existing
//     behaviour for deployments that have not yet configured ADMIN_TOKEN, but
//     it leaves the blast-radius isolation gap described in #684 open. Set
//     ADMIN_TOKEN to eliminate this fallback.
//
// NOTE: canvasOriginAllowed / isSameOriginCanvas are intentionally NOT called
// here.  The Origin header is trivially forgeable by any container on the
// Docker network; using it as an auth bypass would let an attacker reach
// /settings/secrets, /bundles/import, /events, etc. without a bearer token.
// Those short-circuits belong ONLY in CanvasOrBearer (cosmetic routes). (#623)
func AdminAuth(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		adminSecret := os.Getenv("ADMIN_TOKEN")

		hasLive, err := wsauth.HasAnyLiveTokenGlobal(ctx, database)
		if err != nil {
			abortAuthLookupError(c, "AdminAuth: HasAnyLiveTokenGlobal", err)
			return
		}
		if !hasLive {
			// Tier 1: fail-open is ONLY safe when ADMIN_TOKEN is unset
			// (self-hosted dev, pre-Phase-30 upgrade). Hosted SaaS always
			// sets ADMIN_TOKEN at provision time, and C4 (SaaS-launch
			// blocker) showed that without this guard an attacker can
			// pre-empt the first user by POSTing /org/import before any
			// token gets minted. When ADMIN_TOKEN is set we fall through
			// into the same bearer-check path Tier-2 uses below.
			if adminSecret == "" {
				c.Next()
				return
			}
		}

		// Tier 1b: Local-dev escape hatch — see devmode.go. Lets the
		// Canvas dashboard keep working after the first workspace token
		// lands in the DB on `go run ./cmd/server`. Unreachable on SaaS
		// (hosted tenants always have ADMIN_TOKEN + MOLECULE_ENV=production).
		if isDevModeFailOpen() {
			c.Next()
			return
		}

		// SaaS-canvas path: when the request carries a WorkOS session
		// cookie AND the CP confirms it's valid, accept without a
		// bearer. This is how the tenant's Next.js canvas UI
		// authenticates — the browser has a session cookie scoped
		// to .moleculesai.app, and we verify it upstream against
		// /cp/auth/me (short-cached; see verifiedCPSession).
		//
		// Only runs when CP_UPSTREAM_URL is set (prod SaaS); self-
		// hosted / dev deploys without a CP fall through to the
		// bearer-only path unchanged.
		if cookieHeader := c.GetHeader("Cookie"); cookieHeader != "" {
			if ok, _ := VerifiedCPSession(cookieHeader); ok {
				c.Next()
				return
			}
			// Cookie presented but invalid: fall through to the
			// bearer-check path, which will 401. We do NOT abort
			// here so molecli / CLI users with both a cookie and
			// a stale cookie + valid bearer still pass.
		}

		// Bearer token is the ONLY accepted credential for admin routes.
		tok := wsauth.BearerTokenFromHeader(c.GetHeader("Authorization"))
		if tok == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "admin auth required"})
			return
		}

		// Tier 2a: org-scoped API tokens (user-minted via canvas UI).
		// Precedes the ADMIN_TOKEN check because these are the
		// tokens users actually manage — named, revocable, audited.
		// ADMIN_TOKEN is the bootstrap/break-glass credential that
		// still works but is NOT visible through the UI. Both grant
		// the same access surface (full org admin); the tier split
		// is about provenance + rotation, not privilege.
		//
		// Validate() runs ONE indexed lookup (token_hash partial
		// index with revoked_at IS NULL) + an async last_used_at
		// bump. Cost per request: one SELECT + one UPDATE, both
		// hitting the same narrow partial index.
		if id, prefix, orgID, err := orgtoken.Validate(ctx, database, tok); err == nil {
			c.Set("org_token_id", id)
			c.Set("org_token_prefix", prefix)
			// Conditional set — see WorkspaceAuth branch above for rationale.
			if orgID != "" {
				c.Set("org_id", orgID)
			}
			c.Next()
			return
		} else if !errors.Is(err, orgtoken.ErrInvalidToken) {
			// DB error — fail closed and log. Don't expose DB text.
			abortAuthLookupError(c, "AdminAuth: orgtoken.Validate", err)
			return
		}

		// Tier 2b (#684 fix): dedicated ADMIN_TOKEN — workspace bearer tokens
		// must not grant access to admin routes.
		if adminSecret != "" {
			if subtle.ConstantTimeCompare([]byte(tok), []byte(adminSecret)) != 1 {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid admin auth token"})
				return
			}
			c.Next()
			return
		}

		// Tier 3 (deprecated): ADMIN_TOKEN not configured — fall back to any
		// valid workspace token. Operators should set ADMIN_TOKEN to close #684.
		if err := wsauth.ValidateAnyToken(ctx, database, tok); err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid admin auth token"})
			return
		}
		c.Next()
	}
}

// CanvasOrBearer is a softer admin-auth variant used ONLY for cosmetic
// canvas routes where forging the request has zero security impact (PUT
// /canvas/viewport: worst case an attacker resets the shared viewport
// position, user refreshes the page, problem solved).
//
// Accepts either:
//
//  1. A valid bearer token (same contract as AdminAuth) — covers molecli,
//     agent-to-platform calls, and anyone using the API directly.
//  2. A browser Origin header that matches CORS_ORIGINS (canvas itself).
//     This is NOT a strict auth boundary — curl can forge Origin — but for
//     cosmetic-only routes the trade-off is acceptable. Non-cosmetic routes
//     MUST NOT use this middleware (see #194 review on why it would re-open
//     #164 CRITICAL if applied to /bundles/import).
//
// Lazy-bootstrap fail-open preserved: zero-token installs pass everything
// through so fresh self-hosted / dev sessions aren't bricked.
func CanvasOrBearer(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()

		hasLive, err := wsauth.HasAnyLiveTokenGlobal(ctx, database)
		if err != nil {
			log.Printf("wsauth: CanvasOrBearer HasAnyLiveTokenGlobal failed: %v — allowing request", err)
			c.Next()
			return
		}
		if !hasLive {
			c.Next()
			return
		}

		// Path 1: bearer present → bearer MUST validate. Do not fall through
		// to Origin on an invalid bearer — an attacker with a revoked /
		// expired token + a matching Origin would otherwise bypass auth.
		// Empty bearer → skip to Origin path (canvas never sends one).
		if tok := wsauth.BearerTokenFromHeader(c.GetHeader("Authorization")); tok != "" {
			// Admin token accepted for canvas dashboard
			adminSecret := os.Getenv("ADMIN_TOKEN")
			if adminSecret != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(adminSecret)) == 1 {
				c.Next()
				return
			}
			if err := wsauth.ValidateAnyToken(ctx, database, tok); err != nil {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid admin auth token"})
				return
			}
			c.Next()
			return
		}

		// Path 2: canvas origin match (cross-origin canvas).
		if canvasOriginAllowed(c.GetHeader("Origin")) {
			c.Next()
			return
		}

		// Path 3: same-origin canvas (tenant image).
		if isSameOriginCanvas(c) {
			c.Next()
			return
		}

		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "admin auth required"})
	}
}

// canvasOriginAllowed returns true if origin matches any entry in the
// CORS_ORIGINS env var (comma-separated) or the localhost defaults.
// Exact-match only; no prefix or wildcard logic — that's handled by the
// real CORS middleware upstream. The intent here is "did this request come
// from the canvas page the user is already logged into?" — a binary check.
func canvasOriginAllowed(origin string) bool {
	if origin == "" {
		return false
	}
	allowed := []string{"http://localhost:3000", "http://localhost:3001"}
	if v := os.Getenv("CORS_ORIGINS"); v != "" {
		for _, o := range strings.Split(v, ",") {
			if o = strings.TrimSpace(o); o != "" {
				allowed = append(allowed, o)
			}
		}
	}
	for _, a := range allowed {
		if a == origin {
			return true
		}
	}
	return false
}

// isSameOriginCanvas returns true when the request appears to come from the
// canvas UI served by the same Go process (tenant image). In this topology,
// the browser sends same-origin requests with an empty Origin header but a
// Referer matching the request Host. We accept these requests because the
// canvas is the trusted frontend — same as if Origin matched CORS_ORIGINS.
//
// This only fires when CANVAS_PROXY_URL is set (i.e. the combined tenant
// image is active), so self-hosted / dev setups with separate canvas and
// platform origins are unaffected.
// canvasProxyActive is true when the platform runs as a combined tenant
// image (CANVAS_PROXY_URL set at boot). Cached once to avoid os.Getenv
// on every request.
var canvasProxyActive = os.Getenv("CANVAS_PROXY_URL") != ""

// IsSameOriginCanvas is the exported version for use outside the middleware
// package (e.g. workspace.go field-level auth). Same logic as the internal
// callers in AdminAuth/WorkspaceAuth/CanvasOrBearer.
func IsSameOriginCanvas(c *gin.Context) bool {
	return isSameOriginCanvas(c)
}

func isSameOriginCanvas(c *gin.Context) bool {
	if !canvasProxyActive {
		return false
	}
	host := c.Request.Host
	if host == "" {
		return false
	}
	// Check Referer first (standard browser requests).
	referer := c.GetHeader("Referer")
	if referer != "" {
		// Referer must start with https://<host>/ or http://<host>/ (trailing
		// slash required to prevent hongming-wang.moleculesai.app.evil.com from
		// matching hongming-wang.moleculesai.app).
		if strings.HasPrefix(referer, "https://"+host+"/") ||
			strings.HasPrefix(referer, "http://"+host+"/") ||
			referer == "https://"+host ||
			referer == "http://"+host {
			return true
		}
	}
	// Fallback: check Origin header (WebSocket upgrade requests may not have
	// Referer but always send Origin).
	origin := c.GetHeader("Origin")
	return origin == "https://"+host || origin == "http://"+host
}
