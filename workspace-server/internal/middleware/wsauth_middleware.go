package middleware

import (
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/orgtoken"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/wsauth"
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
			if id, prefix, orgID, err := orgtoken.Validate(ctx, database, tok, orgtoken.AuditLogRequestContextFromGin(c), "", false); err == nil {
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
		// SaaS-canvas path: a browser cookie is acceptable only after the
		// control plane confirms membership in this tenant. Referer/Origin
		// are forgeable and must never authenticate workspace data routes.
		if cookieHeader := c.GetHeader("Cookie"); cookieHeader != "" {
			if ok, _, userID := VerifiedCPSession(cookieHeader); ok {
				c.Set("cp_session_actor", cpSessionActor(cookieHeader))
				c.Set("cp_session_user_id", userID)
				c.Next()
				return
			}
		}
		// No bearer, no verified CP session: fail CLOSED in EVERY
		// environment (harden/no-fail-open-auth). The old local-dev
		// escape hatch that let bearer-less requests through when
		// ADMIN_TOKEN was unset + MOLECULE_ENV=dev has been removed —
		// local dev now authenticates with a provisioned ADMIN_TOKEN
		// (see scripts/dev-start.sh).
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing workspace auth token"})
	}
}

// AdminAuth returns a Gin middleware for global/admin routes (e.g.
// /settings/secrets, /admin/secrets) that have no per-workspace scope.
//
// FAIL-CLOSED in every environment (harden/no-fail-open-auth): there is no
// bearer-less path through this middleware. A request reaches the handler
// ONLY by presenting a valid credential (verified CP session cookie, org
// token, ADMIN_TOKEN, or — deprecated — a live workspace token). The former
// "Tier-1 lazy-bootstrap fail-open" (no live tokens + no ADMIN_TOKEN ⇒ pass)
// has been removed: it let an attacker pre-empt the first user by POSTing
// /org/import before any token was minted (C4 SaaS-launch finding). A fresh
// install must set ADMIN_TOKEN to reach admin routes.
//
// # Credential tier (evaluated in order)
//
//  1. Verified CP session cookie (SaaS canvas) — upstream-confirmed.
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

		// (harden/no-fail-open-auth) Both former fail-open branches have
		// been REMOVED here:
		//   - Tier-1 lazy-bootstrap (no live tokens + no ADMIN_TOKEN ⇒ pass)
		//   - Tier-1b local-dev escape hatch (isDevModeFailOpen ⇒ pass)
		// Admin auth is now fail-CLOSED in every environment. We still probe
		// HasAnyLiveTokenGlobal so a datastore outage returns a structured
		// 503 (not a silent pass), but its result no longer opens any path.
		if _, err := wsauth.HasAnyLiveTokenGlobal(ctx, database); err != nil {
			abortAuthLookupError(c, "AdminAuth: HasAnyLiveTokenGlobal", err)
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
			if ok, _, userID := VerifiedCPSession(cookieHeader); ok {
				c.Set("cp_session_actor", cpSessionActor(cookieHeader))
				c.Set("cp_session_user_id", userID)
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
		if id, prefix, orgID, err := orgtoken.Validate(ctx, database, tok, orgtoken.AuditLogRequestContextFromGin(c), "", false); err == nil {
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
			// (core#2574) Mark the request as admin-token-authed so the
			// approval gate can apply. Without this flag the gate's
			// callerHoldsOrgToken helper only fires for org-token callers,
			// and the concierge's admin-token path bypassed every gated
			// verb (org_token_mint, secret_write) — see core#2574 for the
			// live privilege-escalation evidence that motivated this fix.
			c.Set("caller_is_admin_token", true)
			c.Set("caller_credential_class", "admin-token")
			c.Next()
			return
		}

		// Tier 3 (deprecated): ADMIN_TOKEN not configured — fall back to any
		// valid workspace token. Operators should set ADMIN_TOKEN to close #684.
		if err := wsauth.ValidateAnyToken(ctx, database, tok); err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid admin auth token"})
			return
		}
		// (core#2574) Tier-3 fallback is also an "agent that can do org-admin
		// things" — flag it for the gate too, so the gate doesn't accidentally
		// bypass on a misconfigured deploy (no ADMIN_TOKEN, any workspace
		// token accepted as admin). Without this, the bypass is silent.
		c.Set("caller_is_admin_token", true)
		c.Set("caller_credential_class", "admin-token-tier3-fallback")
		c.Next()
	}
}

func cpSessionActor(cookieHeader string) string {
	sum := sha256.Sum256([]byte(tenantSlug() + "\x00" + cookieHeader))
	return "session:" + hex.EncodeToString(sum[:])[:16]
}

// CanvasOrBearer is a softer admin-auth variant used ONLY for cosmetic
// canvas routes where forging the request has zero security impact (PUT
// /canvas/viewport: worst case an attacker resets the shared viewport
// position, user refreshes the page, problem solved).
//
// Accepts either:
//
//  1. A valid bearer token (same contract as AdminAuth) — covers molecli,
//     agent-to-platform calls, the browser canvas (which now sends
//     Authorization: Bearer $NEXT_PUBLIC_ADMIN_TOKEN on every platform
//     call — see canvas/src/lib/api.ts platformAuthHeaders), and anyone
//     using the API directly.
//  2. A same-origin canvas request (Referer/Host match), but ONLY when the
//     combined-tenant canvas proxy is active (CANVAS_PROXY_URL set). This is
//     a real same-origin check the browser cannot forge cross-origin (see
//     isSameOriginCanvas / IsVerifiedCanvasSession, #623/#194) — NOT the
//     trivially-forgeable cross-origin Origin header. The forgeable
//     CORS_ORIGINS Origin-match path was REMOVED under the CTO
//     "nothing fail-open" directive (a no-bearer request passing purely on a
//     spoofable Origin is effectively open even for a cosmetic route, and is
//     no longer needed now that the canvas always sends a bearer).
//
// Non-cosmetic routes MUST NOT use this middleware (see #194 review on why it
// would re-open #164 CRITICAL if applied to /bundles/import).
//
// (harden/no-fail-open-auth) Two former fail-open branches are REMOVED:
//   - DB-error on HasAnyLiveTokenGlobal used to `c.Next()` (allow); it now
//     fails CLOSED with 503 (availability tradeoff that grants NO access).
//   - The lazy-bootstrap pass (`!hasLive ⇒ c.Next()`) used to let a
//     zero-token install through EVERYTHING; it is gone. Bootstrap is now via
//     ADMIN_TOKEN (provisioned by scripts/dev-start.sh for local dev,
//     operator/SaaS-set in production) — local mimics production.
func CanvasOrBearer(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()

		// Probe global token state for the (no-bearer) same-origin path
		// below. Fail CLOSED on a datastore error — an availability tradeoff
		// that does NOT grant access (was: log + c.Next() fail-open).
		if _, err := wsauth.HasAnyLiveTokenGlobal(ctx, database); err != nil {
			abortAuthLookupError(c, "CanvasOrBearer: HasAnyLiveTokenGlobal", err)
			return
		}

		// Path 1: bearer present → bearer MUST validate. Do not fall through
		// to the same-origin path on an invalid bearer — an attacker with a
		// revoked / expired token would otherwise bypass auth.
		// Empty bearer → fall to the same-origin canvas path.
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

		// Path 2: same-origin canvas (combined-tenant image). Gated behind
		// canvasProxyActive (CANVAS_PROXY_URL) and a non-forgeable
		// Referer/Host same-origin check — NOT the spoofable cross-origin
		// Origin header (that path was removed, see doc comment above).
		if isSameOriginCanvas(c) {
			c.Next()
			return
		}

		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "admin auth required"})
	}
}

// (harden/no-fail-open-auth) canvasOriginAllowed was REMOVED. It matched a
// request's (trivially forgeable, cross-origin) Origin header against
// CORS_ORIGINS and was the basis of CanvasOrBearer's no-bearer Origin-match
// pass — effectively open to any curl that sets a matching Origin. Under the
// CTO "nothing fail-open" directive that path is gone; the canvas now always
// sends a bearer (NEXT_PUBLIC_ADMIN_TOKEN), so nothing legitimate relied on it.
// The CORS *response-header* allowlist is handled by the real CORS middleware
// upstream, unaffected by this removal.

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

// cpSessionConfigured reports whether this platform is wired for upstream
// session-cookie verification — i.e. it runs as a SaaS tenant image with
// both CP_UPSTREAM_URL and MOLECULE_ORG_SLUG set. When false (self-hosted /
// dev), VerifiedCPSession can never succeed, so callers that want a
// non-forgeable canvas signal in SaaS while still working in dev can use
// this to decide whether the forgeable same-origin fallback is acceptable.
func cpSessionConfigured() bool {
	return os.Getenv("CP_UPSTREAM_URL") != "" && tenantSlug() != ""
}

// CPSessionConfigured is the exported form of cpSessionConfigured for callers
// outside this package (e.g. the A2A proxy's canvas-user classification).
func CPSessionConfigured() bool {
	return cpSessionConfigured()
}

// IsVerifiedCanvasSession returns true ONLY when the request carries a WorkOS
// session cookie that the control plane confirms belongs to a member of THIS
// tenant's org (via /cp/auth/tenant-member). Unlike IsSameOriginCanvas — whose
// Host/Referer/Origin inputs are trivially forgeable by any container on the
// Docker network and which is therefore documented as cosmetic-only (see
// AdminAuth / CanvasOrBearer comments above, #623/#194) — this is a real,
// upstream-verified authentication boundary. It is the correct gate for
// non-cosmetic actions such as A2A dispatch on behalf of a canvas user.
//
// Returns false (no network call) in self-hosted / dev deployments where
// CP_UPSTREAM_URL / MOLECULE_ORG_SLUG are unset; callers should treat that as
// "no verified canvas session available" and fall back accordingly.
func IsVerifiedCanvasSession(c *gin.Context) bool {
	cookie := c.GetHeader("Cookie")
	if cookie == "" {
		return false
	}
	valid, _, _ := VerifiedCPSession(cookie)
	return valid
}
