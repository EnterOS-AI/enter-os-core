package handlers

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/events"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/models"
	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"
)

// TeamHandler now hosts only Collapse — the visual "expand" action is
// canvas-side and creating children goes through the regular
// WorkspaceHandler.Create path with parent_id set, like any other
// workspace. Every workspace can have children; "team" is just the
// state of having children. The old Expand handler bulk-created
// children by reading sub_workspaces from a parent's config and was
// non-idempotent — calling it N times leaked N×children EC2s, which
// is how tenant-hongming accumulated 72 stale workspaces.
type TeamHandler struct {
	wh *WorkspaceHandler
	b  *events.Broadcaster
}

// NewTeamHandler constructs a TeamHandler. wh is used by Collapse to
// route StopWorkspaceAuto through the backend dispatcher.
func NewTeamHandler(b *events.Broadcaster, wh *WorkspaceHandler, platformURL, configsDir string) *TeamHandler {
	return &TeamHandler{wh: wh, b: b}
}

// Collapse handles POST /workspaces/:id/collapse
// Stops and removes all child workspaces.
func (h *TeamHandler) Collapse(c *gin.Context) {
	parentID := c.Param("id")
	ctx := c.Request.Context()

	// Find children
	rows, err := db.DB.QueryContext(ctx,
		`SELECT id, name FROM workspaces WHERE parent_id = $1 AND status != 'removed'`, parentID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query children"})
		return
	}
	defer rows.Close()

	removed := make([]string, 0)
	for rows.Next() {
		var childID, childName string
		if rows.Scan(&childID, &childName) != nil {
			continue
		}

		// Stop the workload via the backend dispatcher (CP for SaaS,
		// Docker for self-hosted). Pre-2026-05-05 this was
		// `if h.provisioner != nil { h.provisioner.Stop(...) }`, which
		// silently skipped on every SaaS tenant — child EC2s kept running
		// after team-collapse until the orphan sweeper caught them
		// (issue #2813).
		if err := h.wh.StopWorkspaceAuto(ctx, childID); err != nil {
			log.Printf("Team collapse: stop %s failed: %v — orphan sweeper will reconcile", childID, err)
		}

		// Mark as removed
		if _, err := db.DB.ExecContext(ctx,
			`UPDATE workspaces SET status = $1, updated_at = now() WHERE id = $2`, models.StatusRemoved, childID); err != nil {
			log.Printf("Team collapse: failed to remove workspace %s: %v", childID, err)
		}
		if _, err := db.DB.ExecContext(ctx,
			`DELETE FROM canvas_layouts WHERE workspace_id = $1`, childID); err != nil {
			log.Printf("Team collapse: failed to delete layout for %s: %v", childID, err)
		}

		h.b.RecordAndBroadcast(ctx, "WORKSPACE_REMOVED", childID, map[string]interface{}{})

		removed = append(removed, childName)
	}

	h.b.RecordAndBroadcast(ctx, "WORKSPACE_COLLAPSED", parentID, map[string]interface{}{
		"removed_children": removed,
	})

	c.JSON(http.StatusOK, gin.H{
		"status":  "collapsed",
		"removed": removed,
	})
}

// findTemplateDirByName resolves a workspace name to its template
// directory. Kept here because callers outside this package may use
// it, even though the in-package consumer (Expand) is gone.
//
// TODO: relocate alongside the templates handler if no other callers
// surface, or delete entirely after a deprecation cycle.
func findTemplateDirByName(configsDir, name string) string {
	normalized := normalizeName(name)

	candidate := filepath.Join(configsDir, normalized)
	if _, err := os.Stat(filepath.Join(candidate, "config.yaml")); err == nil {
		return candidate
	}

	// Fall back to scanning all dirs
	entries, err := os.ReadDir(configsDir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		cfgPath := filepath.Join(configsDir, e.Name(), "config.yaml")
		data, err := os.ReadFile(cfgPath)
		if err != nil {
			continue
		}
		var cfg struct {
			Name string `yaml:"name"`
		}
		if json.Unmarshal(data, &cfg) == nil && cfg.Name == name {
			return filepath.Join(configsDir, e.Name())
		}
		if yaml.Unmarshal(data, &cfg) == nil && cfg.Name == name {
			return filepath.Join(configsDir, e.Name())
		}
	}
	return ""
}
