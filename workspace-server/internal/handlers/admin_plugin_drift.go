package handlers

// admin_plugin_drift.go — admin endpoints for plugin version-subscription drift queue
// (core#123).
//
// Routes:
//   GET  /admin/plugin-updates-pending  — list all pending drift entries
//   POST /admin/plugin-updates/:id/apply — apply a queued drift update

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/plugins"
	"github.com/gin-gonic/gin"
)

// AdminPluginDriftHandler handles admin endpoints for the plugin drift queue.
type AdminPluginDriftHandler struct {
	pluginsHandler *PluginsHandler // used to re-trigger plugin install on apply
}

// NewAdminPluginDriftHandler constructs a handler wired to the plugins handler.
func NewAdminPluginDriftHandler(ph *PluginsHandler) *AdminPluginDriftHandler {
	return &AdminPluginDriftHandler{pluginsHandler: ph}
}

// ListPending handles GET /admin/plugin-updates-pending.
//
// Returns a JSON array of pending drift entries, newest-first. Empty array
// means no plugins have drifted since the last sweep cycle.
func (h *AdminPluginDriftHandler) ListPending(c *gin.Context) {
	rows, err := plugins.ListPendingUpdates(c.Request.Context())
	if err != nil {
		log.Printf("AdminPluginDrift: ListPendingUpdates failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list pending updates"})
		return
	}
	c.JSON(http.StatusOK, rows)
}

// Apply handles POST /admin/plugin-updates/:id/apply.
//
// 1. Reads the queue entry and verifies it's still pending.
// 2. Reads the workspace_plugins row to get the plugin's source.
// 3. Re-installs the plugin from source_raw (re-fetch from upstream at the
//    same tracked ref — the drift was caused by upstream moving).
// 4. Marks the queue entry as applied.
// 5. Triggers workspace restart.
//
// Idempotent: if the entry is already 'applied', returns 200 with the
// workspace_id and plugin_name so callers can still poll for confirmation.
func (h *AdminPluginDriftHandler) Apply(c *gin.Context) {
	queueID := c.Param("id")
	if queueID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "queue id is required"})
		return
	}

	ctx := c.Request.Context()

	// Step 1: read and lock the queue entry.
	var entry struct {
		WorkspaceID string `json:"workspace_id"`
		PluginName  string `json:"plugin_name"`
		TrackedRef string `json:"tracked_ref"`
		Status     string `json:"status"`
	}
	err := db.DB.QueryRowContext(ctx, `
		SELECT workspace_id, plugin_name, tracked_ref, status
		  FROM plugin_update_queue
		 WHERE id = $1
	`, queueID).Scan(&entry.WorkspaceID, &entry.PluginName, &entry.TrackedRef, &entry.Status)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("queue entry %s not found", queueID)})
		return
	}
	if err != nil {
		log.Printf("AdminPluginDrift: apply: query queue entry: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read queue entry"})
		return
	}

	if entry.Status == "applied" {
		// Idempotent — already applied.
		c.JSON(http.StatusOK, gin.H{
			"status":        "already_applied",
			"workspace_id":   entry.WorkspaceID,
			"plugin_name":   entry.PluginName,
			"message":       "drift update was already applied",
		})
		return
	}

	if entry.Status == "dismissed" {
		c.JSON(http.StatusConflict, gin.H{
			"error":       "queue entry was dismissed",
			"workspace_id": entry.WorkspaceID,
			"plugin_name": entry.PluginName,
		})
		return
	}

	// Step 2: read the workspace_plugins row to get source_raw.
	var sourceRaw string
	err = db.DB.QueryRowContext(ctx, `
		SELECT source_raw FROM workspace_plugins
		 WHERE workspace_id = $1 AND plugin_name = $2
	`, entry.WorkspaceID, entry.PluginName).Scan(&sourceRaw)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{
			"error":       "workspace_plugins row not found — plugin may have been uninstalled",
			"workspace_id": entry.WorkspaceID,
			"plugin_name":  entry.PluginName,
		})
		return
	}
	if err != nil {
		log.Printf("AdminPluginDrift: apply: query workspace_plugins: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read plugin record"})
		return
	}

	// Step 3: re-install the plugin.
	//
	// We call h.pluginsHandler.Install indirectly via a subrequest to reuse
	// the full install pipeline (resolve → stage → deliver → record).
	// Construct the apply installRequest: same source as the existing row,
	// same tracked_ref. The source_raw already encodes the pinned ref
	// (e.g. "github://owner/repo#tag:v1.0.0"), so the resolver fetches
	// the latest commit at that ref — the drift.
	installReq := installRequest{
		Source: sourceRaw,
		Track:  entry.TrackedRef,
	}
	result, instErr := h.pluginsHandler.ResolveAndStageForApply(ctx, installReq)
	if instErr != nil {
		var he *httpErr
		if errors.As(instErr, &he) {
			c.JSON(he.Status, gin.H{
				"error": fmt.Sprintf("plugin install failed: %v", he.Body["error"]),
				"queue_id": queueID,
			})
			return
		}
		log.Printf("AdminPluginDrift: apply: install failed for %s/%s: %v",
			entry.WorkspaceID, entry.PluginName, instErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "plugin install failed", "queue_id": queueID})
		return
	}
	defer func() { _ = os.RemoveAll(result.StagedDir) }()

	// Deliver to the workspace container.
	if err := h.pluginsHandler.DeliverForApply(ctx, entry.WorkspaceID, result); err != nil {
		var he *httpErr
		if errors.As(err, &he) {
			c.JSON(he.Status, gin.H{"error": fmt.Sprintf("plugin deliver failed: %v", he.Body["error"]), "queue_id": queueID})
			return
		}
		log.Printf("AdminPluginDrift: apply: deliver failed for %s/%s: %v",
			entry.WorkspaceID, entry.PluginName, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "plugin deliver failed", "queue_id": queueID})
		return
	}

	// Record the install with the new SHA. This updates installed_sha on the
	// workspace_plugins row so the next drift sweep finds no drift.
	if err := recordWorkspacePluginInstall(ctx, entry.WorkspaceID, result.PluginName,
		result.Source.Raw(), entry.TrackedRef, result.InstalledSHA); err != nil {
		log.Printf("AdminPluginDrift: apply: recordWorkspacePluginInstall failed: %v (install succeeded)", err)
		// Non-fatal: the plugin IS installed; just log and continue.
	}

	// Step 4: mark queue entry as applied.
	if _, err := db.DB.ExecContext(ctx, `
		UPDATE plugin_update_queue SET status = 'applied' WHERE id = $1
	`, queueID); err != nil {
		log.Printf("AdminPluginDrift: apply: failed to mark queue entry %s as applied: %v", queueID, err)
		// Non-fatal: install succeeded; operator can retry or mark manually.
	}

	// Step 5: trigger workspace restart.
	// The pluginsHandler carries a restartFunc (Provisioner.RestartByID) set
	// at construction. Trigger it asynchronously so the HTTP response returns
	// immediately after the install; the restart is best-effort.
	if h.pluginsHandler != nil {
		go func() {
			// We can't use result.PluginName as a restart key since the
			// restartFunc takes a workspaceID. Pass the workspaceID.
			if restart := h.pluginsHandler.GetRestartFunc(); restart != nil {
				restart(entry.WorkspaceID)
			}
		}()
	}

	log.Printf("AdminPluginDrift: applied drift update for %s/%s (queue_id=%s)",
		entry.WorkspaceID, entry.PluginName, queueID)
	c.JSON(http.StatusOK, gin.H{
		"status":       "applied",
		"workspace_id":  entry.WorkspaceID,
		"plugin_name":   entry.PluginName,
		"installed_sha": result.InstalledSHA,
		"restarting":    true,
	})
}
