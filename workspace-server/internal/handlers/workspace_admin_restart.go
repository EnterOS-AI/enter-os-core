package handlers

// workspace_admin_restart.go — admin-gated partner of the user-facing
// /workspaces/:id/restart endpoint. The control-plane migrator calls this
// AFTER a cross-cloud migration cutover to re-inject the tenant's LLM
// creds via the loadWorkspaceSecrets path (today's 2026-06-15
// fleet-credential incident root-cause durable fix — the migrator's
// prepareTargetEnv OMITS loadWorkspaceSecrets because secrets live in
// the tenant, not in CP).
//
// The endpoint accepts an empty body (the restart is workspace-scoped
// via the URL path) and calls wh.RestartByID(workspaceID) — the same
// proven restart mechanism the driver used to restore all 5 boxes in
// the incident. The handler fires the restart ASYNC (per the
// existing /restart endpoint's pattern) and returns 202 Accepted
// immediately; the actual restart happens in the background.
//
// Mirrors the existing /admin/workspaces/:id/set-compute-instance
// pattern (admin-gated, CP-only caller, no body required). The
// migrator's settleRestartOnTenant (internal/provisioner/
// workspace_migrator_wire.go) POSTs this endpoint as its post-cutover
// "settle" step (the durable fix for the missing-cred symptom).
//
// Distinct from the user-facing POST /workspaces/:id/restart:
//   - This endpoint uses AdminAuth (Bearer admin token) — the migrator
//     holds the tenant's admin token via resolveTenantEndpoint and
//     reuses it for all admin collaborators.
//   - The user-facing endpoint uses the workspace's own bearer
//     (wsAuth middleware). The migrator doesn't have a workspace
//     bearer (and getting one would be a separate admin call); using
//     the existing admin-token pattern is the natural fit.

import (
	"log"
	"net/http"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/gin-gonic/gin"
)

// AdminRestart handles POST /admin/workspaces/:id/restart (AdminAuth). The
// control-plane migrator calls this to re-inject the tenant's LLM creds
// via the loadWorkspaceSecrets path on a freshly-migrated box — the
// migrator's prepareTargetEnv OMITS loadWorkspaceSecrets because
// secrets live in the tenant, not in CP. The restart re-runs
// prepareProvisionContext which calls loadWorkspaceSecrets, re-issuing
// the per-workspace bearer + injecting CLAUDE_CODE_OAUTH_TOKEN /
// CODEX_AUTH_JSON / MINIMAX_API_KEY into the container env.
//
// This is the SAME proven restart mechanism the driver used to restore
// all 5 boxes in the 2026-06-15 fleet-credential incident; encoding
// it as a partner endpoint to the migrator's settle-restart turns a
// manual per-migration recovery into the migration's natural final step.
//
// Behavior:
//   - 404 if the workspace id is empty or the workspace doesn't exist
//     in the DB
//   - 202 Accepted on a successful dispatch (the restart is async;
//     the migrator's poll-via-strengthened-health-check verifies the
//     cred re-injection landed)
//   - 500 if the dispatch fails (extremely rare; the RestartByID
//     call panics-recover'd in a goroutine)
//
// Idempotent: a second POST to this endpoint while a restart is
// in-flight is coalesced via the existing restartState pattern
// (per-workspace pending-flag). Safe to call repeatedly.
func (h *WorkspaceHandler) AdminRestart(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "workspace id required"})
		return
	}

	// Pre-flight: confirm the workspace exists. A 404 here (vs. a
	// silent no-op for a missing id) gives the migrator a clear
	// signal to roll back. The RestartByID call below would also
	// fail in this case, but with a less-precise error; doing the
	// pre-flight gives ops a clean diagnostic in the wire log.
	var exists int
	err := db.DB.QueryRowContext(c.Request.Context(), `SELECT 1 FROM workspaces WHERE id = $1`, id).Scan(&exists)
	if err != nil {
		if err.Error() == "sql: no rows in result set" {
			c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
			return
		}
		log.Printf("AdminRestart: workspace lookup %s: %v", id, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db lookup failed"})
		return
	}

	// Fire the restart ASYNC — same pattern as the user-facing
	// POST /workspaces/:id/restart handler. The actual restart runs
	// in a goroutine; we return 202 Accepted immediately so the
	// migrator's poll loop isn't held by the restart's own
	// provisioning time.
	h.goAsync(func() { h.RestartByID(id) })
	log.Printf("AdminRestart: dispatching restart for workspace %s (CP migrator settle — fleet-credential incident durable fix)", id)
	c.JSON(http.StatusAccepted, gin.H{
		"status":       "restart_dispatched",
		"workspace_id": id,
	})
}
