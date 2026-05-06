package handlers

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/events"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/wsauth"
	"github.com/gin-gonic/gin"
)

// external_rotate.go — operator-facing endpoints for credential lifecycle
// on runtime=external workspaces.
//
//   POST /workspaces/:id/external/rotate
//     Mints a fresh workspace_auth_token, revokes any prior live tokens
//     for the same workspace, and returns the same payload shape Create
//     returns. Old credentials stop working immediately — the next
//     heartbeat from the previously-paired agent will fail auth.
//
//   GET /workspaces/:id/external/connection
//     Returns the connection payload WITHOUT minting (auth_token = "").
//     For the operator who lost their copy of the snippet but still has
//     the token elsewhere — they want the rest of the connect block
//     (PLATFORM_URL, WORKSPACE_ID, registry endpoints, all 7 snippets)
//     without invalidating the live agent.
//
// Both endpoints reject runtime ≠ external with 400 — the "external
// connection" payload only makes sense for awaiting-agent / online-
// external workspaces. A user clicking Rotate on a hermes / claude-code
// workspace would silently break ssh-EIC tunnel auth, which is worse
// than refusing the action.

// RotateExternalCredentials handles POST /workspaces/:id/external/rotate.
//
// Why this endpoint exists: today the auth_token is only revealed once
// (on Create), via the Modal that closes after the operator dismisses
// it. There's no recovery path — lost the token, lost the workspace.
// Rotation gives operators a way to (a) recover from lost credentials
// and (b) respond to a suspected leak without recreating the workspace
// from scratch (which would also invalidate any cross-workspace
// delegation links + memory namespace).
func (h *WorkspaceHandler) RotateExternalCredentials(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id required"})
		return
	}
	ctx := c.Request.Context()

	runtime, err := lookupWorkspaceRuntime(ctx, db.DB, id)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return
	}
	if err != nil {
		log.Printf("RotateExternalCredentials(%s): runtime lookup failed: %v", id, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "lookup failed"})
		return
	}
	if runtime != "external" {
		// Rotating a hermes/claude-code workspace's bearer would not
		// just break the ssh-EIC tunnel auth on the platform side — it
		// would also leave the workspace's in-container heartbeat with
		// a stale token until the next reboot. The right action for a
		// non-external workspace's compromised credential is restart,
		// which mints a fresh token AND injects it into the container
		// (workspace_provision.go:issueAndInjectToken). Refuse cleanly
		// here so the canvas can show "rotate is for external workspaces;
		// click Restart instead" rather than silently corrupting state.
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "rotate is only valid for runtime=external workspaces",
			"runtime": runtime,
			"hint":    "use POST /workspaces/:id/restart for non-external runtimes",
		})
		return
	}

	// Revoke first, then mint. Order matters: if mint fails, the
	// workspace is left without any live token (operator can retry) —
	// that's better than the inverse where mint succeeds + revoke fails
	// and TWO live tokens end up valid (the previous one + the new one),
	// silently leaving the leaked credential alive.
	if err := wsauth.RevokeAllForWorkspace(ctx, db.DB, id); err != nil {
		log.Printf("RotateExternalCredentials(%s): revoke failed: %v", id, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "revoke failed"})
		return
	}
	tok, err := wsauth.IssueToken(ctx, db.DB, id)
	if err != nil {
		log.Printf("RotateExternalCredentials(%s): mint failed: %v", id, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "mint failed"})
		return
	}

	// Audit broadcast — operators reviewing the activity feed should
	// see when credentials were rotated. No PII; the token plaintext
	// is NOT logged.
	if h.broadcaster != nil {
		h.broadcaster.RecordAndBroadcast(ctx, string(events.EventExternalCredentialsRotated), id, map[string]interface{}{
			"workspace_id": id,
		})
	}

	platformURL := externalPlatformURL(c)
	c.JSON(http.StatusOK, gin.H{
		"connection": BuildExternalConnectionPayload(platformURL, id, tok),
	})
}

// GetExternalConnection handles GET /workspaces/:id/external/connection.
//
// Returns the connect-block WITHOUT minting (auth_token = ""). For the
// operator who needs to re-find PLATFORM_URL / WORKSPACE_ID / one of
// the snippets (their note app got wiped, they switched machines, etc.)
// but doesn't want to invalidate the live external agent.
//
// The canvas modal masks the auth_token field in this mode and labels
// it "(rotate to reveal a new token — current token is unrecoverable)".
func (h *WorkspaceHandler) GetExternalConnection(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id required"})
		return
	}
	ctx := c.Request.Context()

	runtime, err := lookupWorkspaceRuntime(ctx, db.DB, id)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return
	}
	if err != nil {
		log.Printf("GetExternalConnection(%s): runtime lookup failed: %v", id, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "lookup failed"})
		return
	}
	if runtime != "external" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "connection payload is only valid for runtime=external workspaces",
			"runtime": runtime,
		})
		return
	}

	platformURL := externalPlatformURL(c)
	c.JSON(http.StatusOK, gin.H{
		"connection": BuildExternalConnectionPayload(platformURL, id, ""),
	})
}

// lookupWorkspaceRuntime returns the workspace's runtime field. Wrapped
// for readability + so tests can mock the single SELECT.
func lookupWorkspaceRuntime(ctx context.Context, handle *sql.DB, id string) (string, error) {
	var runtime string
	err := handle.QueryRowContext(ctx, `
		SELECT COALESCE(runtime, '') FROM workspaces WHERE id = $1
	`, id).Scan(&runtime)
	return runtime, err
}
