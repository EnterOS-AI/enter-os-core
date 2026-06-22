package handlers

import (
	"log"
	"net/http"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/wsauth"
	"github.com/gin-gonic/gin"
)

// RevokeAuthTokens revokes every live workspace_auth_tokens row for a
// workspace, so the NEXT /registry/register call for that workspace is
// bootstrap-allowed (no live token on file → requireWorkspaceToken lets the
// first registration through and issues a fresh token).
//
// Why this exists — cross-cloud migration (CP#672 + migrate-provider):
// when the CP migrates a workspace to another cloud it provisions a FRESH
// container. CP#672 persists only /workspace + /home/agent/.claude — NOT
// /configs — so the migrated container boots with an empty
// /configs/.auth_token and cannot present the bearer the SOURCE box minted.
// The source's token is still live in workspace_auth_tokens, so the migrated
// container's /registry/register 401s (C18 ownership guard) and the workspace
// is wedged: it serves its agent-card but never re-registers, so its
// advertised URL never flips to the new box.
//
// The single-tenant Docker deployment self-heals this via
// sweepStaleTokensWithoutContainer (orphan_sweeper.go) — but that sweeper
// only runs in single-tenant Docker mode (no Docker daemon in CP/SaaS), so a
// per-tenant SaaS platform never revokes the stale token and the migration
// 401-wedges forever. The platform's own restart pipeline already does the
// right thing (workspace_restart.go → issueAndInjectToken →
// wsauth.RevokeAllForWorkspace); this endpoint exposes the SAME revoke so the
// CP migrator — which provisions the target out-of-band, bypassing the restart
// pipeline — can trigger it as part of the cutover.
//
// AdminAuth-gated (wired in router.go's wsAdmin group): only the CP (holding
// the tenant admin token) may revoke a workspace's tokens. Idempotent —
// revoking an already-revoked / never-registered workspace is a no-op 200, so
// the migrator can call it unconditionally.
func (h *WorkspaceHandler) RevokeAuthTokens(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "workspace id required"})
		return
	}
	if err := wsauth.RevokeAllForWorkspace(c.Request.Context(), db.DB, id); err != nil {
		log.Printf("RevokeAuthTokens: revoke %s: %v", id, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "revoke failed"})
		return
	}
	log.Printf("RevokeAuthTokens: revoked live auth tokens for workspace %s (migration cutover / admin)", id)
	c.JSON(http.StatusOK, gin.H{"status": "revoked", "workspace_id": id})
}
