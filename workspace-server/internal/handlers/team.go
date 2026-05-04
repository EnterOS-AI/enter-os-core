package handlers

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/events"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/models"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/provisioner"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

// TeamHandler delegates child-workspace provisioning to wh so child
// workspaces go through the same prepare/mint/preflight pipeline that
// every other provision path uses. Pre-fix (issue #2367): Expand
// directly invoked h.provisioner.Start which skipped mintWorkspaceSecrets,
// leaving every team-expanded child with NULL platform_inbound_secret +
// NULL auth_token — same drift class as the SaaS bug fixed in #2366.
type TeamHandler struct {
	broadcaster *events.Broadcaster
	// provisioner is interface-typed (#2369) for the same reason as
	// WorkspaceHandler.provisioner — Stop is the only call site here
	// and it's on the LocalProvisionerAPI surface, so widening is free
	// and symmetric with WorkspaceHandler.
	provisioner provisioner.LocalProvisionerAPI
	wh          *WorkspaceHandler
	platformURL string
	configsDir  string
}

func NewTeamHandler(b *events.Broadcaster, p *provisioner.Provisioner, wh *WorkspaceHandler, platformURL, configsDir string) *TeamHandler {
	h := &TeamHandler{
		broadcaster: b,
		wh:          wh,
		platformURL: platformURL,
		configsDir:  configsDir,
	}
	// Avoid the typed-nil interface trap (see NewWorkspaceHandler note).
	if p != nil {
		h.provisioner = p
	}
	return h
}

// Expand handles POST /workspaces/:id/expand
// Reads sub_workspaces from the workspace's config and provisions child workspaces.
func (h *TeamHandler) Expand(c *gin.Context) {
	parentID := c.Param("id")
	ctx := c.Request.Context()

	// Verify workspace exists and is online
	var name string
	var tier int
	var status string
	err := db.DB.QueryRowContext(ctx,
		`SELECT name, tier, status FROM workspaces WHERE id = $1`, parentID,
	).Scan(&name, &tier, &status)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "lookup failed"})
		return
	}

	// Find the workspace's config to get sub_workspaces
	templateDir := findTemplateDirByName(h.configsDir, name)
	if templateDir == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no config found for workspace"})
		return
	}

	configData, err := os.ReadFile(filepath.Join(templateDir, "config.yaml"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read config"})
		return
	}

	var config struct {
		SubWorkspaces []struct {
			Config string `yaml:"config"`
			Name   string `yaml:"name"`
			Role   string `yaml:"role"`
		} `yaml:"sub_workspaces"`
	}
	if err := yaml.Unmarshal(configData, &config); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to parse config"})
		return
	}

	if len(config.SubWorkspaces) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "workspace has no sub_workspaces defined in config"})
		return
	}

	// Create child workspaces
	children := make([]map[string]interface{}, 0)
	for _, sub := range config.SubWorkspaces {
		childID := uuid.New().String()
		childName := sub.Name
		if childName == "" {
			childName = sub.Config
		}

		_, err := db.DB.ExecContext(ctx, `
			INSERT INTO workspaces (id, name, role, tier, status, parent_id)
			VALUES ($1, $2, $3, $4, 'provisioning', $5)
		`, childID, childName, nilStr(sub.Role), tier, parentID)
		if err != nil {
			log.Printf("Expand: failed to create child %s: %v", childName, err)
			continue
		}

		// Insert canvas layout (offset from parent)
		if _, err := db.DB.ExecContext(ctx, `
			INSERT INTO canvas_layouts (workspace_id, x, y) VALUES ($1, $2, $3)
		`, childID, 0, 0); err != nil {
			log.Printf("Team expand: failed to insert layout for child %s: %v", childID, err)
		}

		h.broadcaster.RecordAndBroadcast(ctx, "WORKSPACE_PROVISIONING", childID, map[string]interface{}{
			"name":      childName,
			"tier":      tier,
			"parent_id": parentID,
		})

		// Delegate child-workspace provisioning to the shared
		// provision pipeline. Issue #2367: previously Expand called
		// h.provisioner.Start directly, bypassing mintWorkspaceSecrets
		// and every other preflight (secrets, env mutators, identity
		// injection, missing-env). That left every child with NULL
		// platform_inbound_secret and never-issued auth_token. Now
		// children go through the same provisionWorkspaceAuto path as
		// Create/Restart, so adding a future provision-time step
		// automatically covers Expand too.
		//
		// 2026-05-04 follow-up: switched from provisionWorkspace
		// (hardcoded Docker) to provisionWorkspaceAuto (picks CP for
		// SaaS, Docker for self-hosted). Pre-fix, deploying a team on
		// a SaaS tenant created child rows but never an EC2 instance —
		// the 600s sweeper logged the misleading "container started
		// but never called /registry/register". Templates only own
		// shape (config/prompts/files/plugins/runtime); the platform
		// owns where it runs.
		if h.wh != nil && sub.Config != "" {
			templatePath := filepath.Join(h.configsDir, sub.Config)
			if _, err := os.Stat(templatePath); err == nil {
				parent := parentID // copy for closure
				h.wh.provisionWorkspaceAuto(childID, templatePath, nil, models.CreateWorkspacePayload{
					Name:     childName,
					Role:     sub.Role,
					Tier:     tier,
					ParentID: &parent,
				})
			}
		}

		children = append(children, map[string]interface{}{
			"id":   childID,
			"name": childName,
			"role": sub.Role,
		})
	}

	// Mark parent as expanded
	h.broadcaster.RecordAndBroadcast(ctx, "WORKSPACE_EXPANDED", parentID, map[string]interface{}{
		"children": children,
	})

	c.JSON(http.StatusOK, gin.H{
		"status":   "expanded",
		"children": children,
	})
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

		// Stop container if provisioner available
		if h.provisioner != nil {
			h.provisioner.Stop(ctx, childID)
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

		h.broadcaster.RecordAndBroadcast(ctx, "WORKSPACE_REMOVED", childID, map[string]interface{}{})

		removed = append(removed, childName)
	}

	h.broadcaster.RecordAndBroadcast(ctx, "WORKSPACE_COLLAPSED", parentID, map[string]interface{}{
		"removed_children": removed,
	})

	c.JSON(http.StatusOK, gin.H{
		"status":  "collapsed",
		"removed": removed,
	})
}

func nilStr(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

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
		// Try yaml unmarshal too
		if yaml.Unmarshal(data, &cfg) == nil && cfg.Name == name {
			return filepath.Join(configsDir, e.Name())
		}
	}
	return ""
}
