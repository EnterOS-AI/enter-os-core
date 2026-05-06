package handlers

import (
	"log"
	"net/http"
	"strings"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/events"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/models"
	"github.com/gin-gonic/gin"
)

// BootstrapFailedRequest is the body shape the control plane POSTs when a
// workspace EC2 crashes during user-data execution — before the agent runtime
// ever calls /registry/register. Without this signal the workspace sits in
// `provisioning` until the 10-minute sweeper flips it. Fast-path fail keeps
// the canvas honest about state.
type BootstrapFailedRequest struct {
	// Error is the short, single-line message surfaced in the UI banner
	// and the WORKSPACE_PROVISION_FAILED payload.
	Error string `json:"error"`
	// LogTail is the last ~2KB of /var/log/molecule-runtime.log or the
	// cloud-init serial console. Stored in `last_sample_error` so the
	// canvas's Details tab can render the real stack trace next to the
	// failed status, with no extra fetch needed.
	LogTail string `json:"log_tail"`
}

// BootstrapFailed marks a workspace as failed from an out-of-band signal —
// specifically the control plane's bootstrap watcher when it detects
// "RUNTIME CRASHED" in the EC2 console output of a workspace that never
// self-registered. Idempotent: a workspace already flipped to online
// (raced with a late self-registration) or to failed (double-report) is
// left alone.
func (h *WorkspaceHandler) BootstrapFailed(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "workspace id required"})
		return
	}
	var req BootstrapFailedRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	// Cap log_tail so a runaway heredoc from user-data doesn't bloat the
	// workspaces row. 8KB is plenty for a Python traceback.
	tail := req.LogTail
	if len(tail) > 8192 {
		tail = "...(truncated)...\n" + tail[len(tail)-8192:]
	}
	errMsg := strings.TrimSpace(req.Error)
	if errMsg == "" {
		errMsg = "bootstrap failed — see log_tail"
	}

	// Store the tail as last_sample_error so the UI can render the real
	// error without a second fetch. Guard against overwriting a workspace
	// that already reached online/failed — only act on `provisioning`.
	res, err := db.DB.ExecContext(c.Request.Context(), `
		UPDATE workspaces
		   SET status = $3,
		       last_sample_error = $2,
		       updated_at = now()
		 WHERE id = $1
		   AND status = 'provisioning'
	`, id, truncateString(errMsg+"\n\n"+tail, 8192), models.StatusFailed)
	if err != nil {
		log.Printf("BootstrapFailed: db update %s: %v", id, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db update failed"})
		return
	}
	affected, err := res.RowsAffected()
	if err != nil {
		log.Printf("BootstrapFailed: RowsAffected error for %s: %v", id, err)
		// Workspace likely already transitioned — treat as no-op like affected==0.
		c.JSON(http.StatusOK, gin.H{"ok": true, "no_change": true})
		return
	}
	if affected == 0 {
		// Already transitioned out of provisioning — don't re-emit the
		// event (would lie to the canvas). Return 200 so CP doesn't retry.
		c.JSON(http.StatusOK, gin.H{"ok": true, "no_change": true})
		return
	}

	h.broadcaster.RecordAndBroadcast(c.Request.Context(), string(events.EventWorkspaceProvisionFailed), id, map[string]interface{}{
		"error":    errMsg,
		"log_tail": tail,
		"source":   "bootstrap_watcher",
	})
	log.Printf("BootstrapFailed: marked %s failed (tail=%d bytes, err=%q)", id, len(tail), errMsg)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// Console proxies EC2 console output for a workspace from the control plane.
// Only CP has `ec2:GetConsoleOutput` permission — the tenant platform can't
// call AWS directly (no AWS creds on the tenant EC2 by design). The canvas
// hits this endpoint; the platform proxies via the CP admin bearer it was
// provisioned with. Admin-gated because raw console output can leak
// user-data snippets that we treat as semi-sensitive.
//
// Endpoint shape:  GET /workspaces/:id/console
// Response shape:  {"output": "<serial console text>"}
func (h *WorkspaceHandler) Console(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "workspace id required"})
		return
	}
	if h.cpProv == nil {
		// Self-hosted / docker-compose deploys don't use CP — there's no
		// serial console to fetch (logs live in `docker logs` instead).
		c.JSON(http.StatusNotImplemented, gin.H{"error": "console output unavailable on this deployment (no control plane)"})
		return
	}
	output, err := h.cpProv.GetConsoleOutput(c.Request.Context(), id)
	if err != nil {
		log.Printf("Console: cp get for %s: %v", id, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "control plane returned an error fetching console output"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"output": output})
}

// truncateString returns s limited to n bytes, trimming partial UTF-8
// sequences at the boundary.
func truncateString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	end := n
	for end > 0 && (s[end]&0xC0) == 0x80 {
		end--
	}
	return s[:end]
}
