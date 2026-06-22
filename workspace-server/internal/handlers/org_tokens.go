package handlers

import (
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/approvals"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/orgtoken"
	"github.com/gin-gonic/gin"
)

// OrgTokenHandler exposes CRUD for organization-scoped API tokens.
//
// Routes (all AdminAuth-gated, mounted at root):
//
//	GET    /org/tokens         list live tokens
//	POST   /org/tokens         mint a new token; plaintext returned once
//	DELETE /org/tokens/:id     revoke
//
// Sibling of TokenHandler (workspace-scoped); deliberately kept
// separate because the admin surface is wider — an org token can
// mint/revoke other org tokens, escalate workspace perms, etc. —
// and conflating them with workspace tokens makes revoke UX
// confusing.
type OrgTokenHandler struct{}

func NewOrgTokenHandler() *OrgTokenHandler {
	return &OrgTokenHandler{}
}

// List returns live (non-revoked) tokens, newest-first. Prefix only —
// never plaintext or hash.
func (h *OrgTokenHandler) List(c *gin.Context) {
	tokens, err := orgtoken.List(c.Request.Context(), db.DB)
	if err != nil {
		log.Printf("orgtoken list: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list tokens"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"tokens": tokens, "count": len(tokens)})
}

type createOrgTokenRequest struct {
	Name      string     `json:"name"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

type createOrgTokenResponse struct {
	ID        string     `json:"id"`
	Prefix    string     `json:"prefix"`
	Name      string     `json:"name,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Token     string     `json:"auth_token"` // plaintext — shown ONCE
	Warning   string     `json:"warning"`    // UX hint: copy now
}

// Create mints a new org token. The plaintext is returned exactly
// once in the response body. Mirrors wsauth's Issue semantics so UI
// flow (copy-once, dismiss, no retrieval) is consistent across
// token types.
//
// created_by is captured from the org_token_id or admin-token
// provenance of the current request — so an audit trail points back
// to who minted what. For the bootstrap ADMIN_TOKEN path, created_by
// is "admin-token" (no session identity available).
//
// orgID is the caller's org workspace ID, captured at mint time.
// requireCallerOwnsOrg (org_plugin_allowlist.go:116) uses this to
// enforce org isolation (#1200 / F1094).
func (h *OrgTokenHandler) Create(c *gin.Context) {
	var req createOrgTokenRequest
	// Optional body — an empty POST should still work (unnamed token).
	if err := c.ShouldBindJSON(&req); err != nil && err != io.EOF {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body"})
		return
	}
	if len(req.Name) > 100 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name too long (max 100 chars)"})
		return
	}

	// (core#2574) Phase-4 approval gate — org_token_mint is in the gated
	// action set (approvals.ActionOrgTokenMint, approvals.IsGated). Without
	// this gate call, an admin-token-bearing agent (the concierge uses
	// MOLECULE_API_KEY = $ADMIN_TOKEN) could mint a full-tenant-admin org
	// API token with ZERO pending approvals — a real privilege-escalation
	// bypass that was exploited in the live incident (two live org tokens
	// minted without human review, then operator-revoked). Now the agent
	// gets HTTP 202 with an approval_id and has to retry after a human
	// approves via /approvals/decide.
	//
	// (core#2579) Approvals are keyed by workspace_id (UUID, NOT NULL).
	// The previous design passed callerOrg(c) as the anchor — but for
	// admin-token callers callerOrg returns "" (no org_token_id in
	// context), and "" into the UUID-backed approval query errors
	// "invalid input syntax for type uuid" → handler 500. Fix: derive
	// a VALID approval anchor per caller class. Org-token callers use
	// their token's org_id (callerOrg); admin-token callers use the
	// concierge/platform root workspace (callerPlatformOrg); session
	// callers fall through. If no anchor can be derived, return a
	// controlled 4xx — never pass "" into the UUID query.
	// (core#2593) Verified-session (human) callers SKIP the approval
	// gate by design: the gate exists to put a human between an AGENT
	// and a privileged mint — when the minter IS a human at the
	// dashboard (CP-verified WorkOS session; AdminAuth only sets
	// cp_session_actor after VerifiedCPSession confirms the cookie
	// upstream), routing them through a pending-approval that the same
	// human would approve is a pure no-op round-trip. Live regression
	// this fixes: Settings → Org API Keys → "+ New Key" returned the
	// raw anchor 400 because approvalAnchorForGate had no session
	// branch (the #2579 assumption "the UI mints via the concierge"
	// was wrong — the canvas mints directly with the browser session).
	//
	// SECURITY: keyed on cp_session_actor (set ONLY post-verification),
	// NEVER on the raw Cookie header — an admin-token agent attaching a
	// junk Cookie header must NOT bypass the gate (pinned by
	// TestOrgTokenHandler_Create_ActorFromSession).
	if actor := callerVerifiedSessionActor(c); actor != "" {
		log.Printf("orgtoken: mint by verified human session %s — approval gate bypassed by design (core#2593)", actor)
	} else {
		ws := approvalAnchorForGate(c)
		if ws == "" {
			log.Printf("OrgTokenHandler.Create: no approval anchor for caller class=%s (admin-token callers need MOLECULE_PLATFORM_WORKSPACE_ID; org-token callers need a token with a valid org_id)", orgTokenActorClass(c))
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "no approval anchor for this caller class — set MOLECULE_PLATFORM_WORKSPACE_ID for admin-token callers, or call via an org-token with a valid org",
			})
			return
		}
		if !gateDestructive(c, nil, ws, approvals.ActionOrgTokenMint,
			"mint org token "+req.Name,
			map[string]interface{}{"actor": orgTokenActorClass(c), "name": req.Name, "workspace_id": ws}) {
			return
		}
	}

	createdBy, orgID := orgTokenActor(c)

		plaintext, id, err := orgtoken.IssueWithExpiry(c.Request.Context(), db.DB, req.Name, createdBy, orgID, req.ExpiresAt, orgtoken.AuditLogRequestContextFromGin(c))
	if err != nil {
		if errors.Is(err, orgtoken.ErrMintCeilingExceeded) {
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "org api token mint ceiling exceeded"})
			return
		}
		log.Printf("orgtoken issue: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to mint token"})
		return
	}
	log.Printf("orgtoken: minted id=%s by=%s org=%s name=%q", id, createdBy, orgID, req.Name)

	c.JSON(http.StatusOK, createOrgTokenResponse{
		ID:        id,
		Prefix:    plaintext[:8],
		Name:      req.Name,
		ExpiresAt: req.ExpiresAt,
		Token:     plaintext,
		Warning:   "copy this token now; it will not be shown again",
	})
}

// orgTokenActorClass returns the credential class label for the current
// request, used in the approval gate's context (so an approval for "mint
// from admin-token" cannot be replayed as "mint from org-token:abc" or
// vice versa — the request_hash differs by credential class).
func orgTokenActorClass(c *gin.Context) string {
	if v, ok := c.Get("caller_credential_class"); ok {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	if v, ok := c.Get("org_token_prefix"); ok {
		if s, ok := v.(string); ok && s != "" {
			return actorOrgTokenPrefix + s
		}
	}
	// Verified session only — the raw Cookie header is forgeable by
	// bearer-authed agents and must not influence classification.
	if callerVerifiedSessionActor(c) != "" {
		return actorSession
	}
	return actorAdminToken
}

// Revoke flips revoked_at. 404 when the id doesn't exist OR was
// already revoked — idempotent from the caller's perspective.
func (h *OrgTokenHandler) Revoke(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id required"})
		return
	}
	actor, _ := orgTokenActor(c)
	ok, err := orgtoken.Revoke(c.Request.Context(), db.DB, id, orgtoken.AuditLogRequestContextFromGin(c), actor)
	if err != nil {
		log.Printf("orgtoken revoke: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to revoke"})
		return
	}
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "token not found or already revoked"})
		return
	}
	log.Printf("orgtoken: revoked id=%s by=%s", id, actor)
	c.JSON(http.StatusOK, gin.H{"revoked": id})
}

// Provenance labels used in the org_api_tokens.created_by column
// and in mint/revoke audit logs. Kept as constants so the labels
// are greppable across services (log pipelines, audit queries).
const (
	actorOrgTokenPrefix = "org-token:"  // appended: 8-char plaintext prefix from the UI
	actorSession        = "session"     // WorkOS-session-verified call
	actorAdminToken     = "admin-token" // bootstrap ADMIN_TOKEN env
)

// callerContext returns the caller's org workspace ID for use in
// org-token creation (#1200 / F1094). It reads org_token_id from the
// gin context (set by AdminAuth when an org token authed the request)
// and looks up the token's org_id.
//
// For session/ADMIN_TOKEN callers (no org_token_id in context), returns
// ("", "") so the token is minted as "unanchored" (org_id=NULL).
// Unanchored tokens cannot access org-scoped routes — safer than
// permitting cross-org access until the operator explicitly sets org_id.
func callerOrg(c *gin.Context) string {
	tokenID, ok := c.Get("org_token_id")
	if !ok {
		return ""
	}
	tokID, ok := tokenID.(string)
	if !ok || tokID == "" {
		return ""
	}
	orgID, err := orgtoken.OrgIDByTokenID(c.Request.Context(), db.DB, tokID)
	if err != nil || orgID == "" {
		return ""
	}
	return orgID
}

// approvalAnchorForGate (core#2579) returns a NON-EMPTY workspace_id
// suitable for the approval gate (requireApproval queries
// approval_requests.workspace_id as a UUID, NOT NULL). Unlike
// callerOrg — which intentionally returns "" for admin-token callers
// so org_api_tokens.org_id is NULL (unanchored tokens, deny by
// default) — the approval gate needs a stable anchor for EVERY caller
// class:
//
//   - Org-token callers: callerOrg(c) (the token's org_id). Same
//     anchor the minted token will be tied to, so the approval and
//     the token are audit-co-located.
//   - Admin-token callers: MOLECULE_PLATFORM_WORKSPACE_ID env var
//     (operator-set UUID of the concierge/platform root workspace).
//     This is the platform-org anchor — every admin-token approval
//     for org_token_mint lives against the platform root, so a
//     human approver sees ONE pending-approval inbox entry for
//     "platform agent minted N org tokens this hour" instead of
//     N entries scattered by unanchored orgs. Without this env var
//     set, admin-token callers get "" (caller returns a controlled
//     4xx, never passes "" into the UUID query).
//   - Session callers: same as org-token callers (callerOrg). Today
//     session callers have no org_token_id → callerOrg returns "" —
//     these callers are blocked at the 4xx with a hint to set the
//     platform workspace ID or use an org-token credential. Session
//     paths through CP/UI are NOT expected to mint org tokens
//     directly (the UI mints via the concierge, which is admin-token
//     authenticated → covers via the admin-token branch above).
//
// CRITICAL (RCA on #2579): the previous code passed callerOrg(c)
// directly to gateDestructive, which crashed with "invalid input
// syntax for type uuid" when callerOrg returned "" (admin-token
// callers). That crashed path returned 500 from the handler, which
// silently bypassed the approval gate in a different way (caller
// sees 500 → retries → eventually unblocks via unmonitored paths).
// This helper is the fix: every caller class either gets a valid
// UUID or a controlled 4xx.
func approvalAnchorForGate(c *gin.Context) string {
	if orgID := callerOrg(c); orgID != "" {
		return orgID
	}
	if callerIsAdminToken(c) {
		if v := os.Getenv("MOLECULE_PLATFORM_WORKSPACE_ID"); v != "" {
			return v
		}
	}
	return ""
}

// callerVerifiedSessionActor returns the cp_session_actor identity
// ("session:<hash>") when — and ONLY when — the request authenticated
// via a CP-VERIFIED WorkOS session cookie (AdminAuth sets the key
// strictly after VerifiedCPSession confirms the cookie upstream
// against /cp/auth/me). Returns "" for every bearer-authed caller,
// including ones that happen to carry an unverified/junk Cookie
// header. This is the human-vs-agent discriminator for the approval
// gate (core#2593) — do NOT replace it with a raw Cookie check, which
// any agent can forge.
func callerVerifiedSessionActor(c *gin.Context) string {
	v, ok := c.Get("cp_session_actor")
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}

// orgTokenActor returns (createdBy, orgID) for the current request.
//
//   - If authed via another org token (org_token_id in context),
//     createdBy = "org-token:<prefix>" and orgID = token's org_id.
//   - If authed via session cookie (AdminAuth's session tier),
//     createdBy = "session:<workos_user_id>" when cp_session_user_id is
//     available, otherwise "session", and orgID = "" (session → org mapping
//     not available in the handler; must be filled by the CP or left null).
//     Capturing the WorkOS user_id closes KI-004 item 5.
//   - If ADMIN_TOKEN / bootstrap, createdBy = "admin-token",
//     orgID = "".
func orgTokenActor(c *gin.Context) (createdBy, orgID string) {
	if v, ok := c.Get("org_token_prefix"); ok {
		if s, ok := v.(string); ok && s != "" {
			return actorOrgTokenPrefix + s, callerOrg(c)
		}
	}
	// Verified-session callers carry the cp_session_actor identity
	// ("session:<hash>") — record the cp_session_user_id as created_by
	// so the audit trail names the actual WorkOS user, not just a
	// session hash. Fall back to "session" when the user_id is absent.
	if actor := callerVerifiedSessionActor(c); actor != "" {
		if uid, ok := c.Get("cp_session_user_id"); ok {
			if s, ok := uid.(string); ok && s != "" {
				return actorSession + ":" + s, ""
			}
		}
		return actor, ""
	}
	return actorAdminToken, ""
}
