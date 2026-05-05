package bundle

import (
	"context"
	"fmt"
	"strings"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/events"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/models"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/provisioner"
	"github.com/google/uuid"
)

// ImportResult tracks the outcome of importing a bundle tree.
type ImportResult struct {
	WorkspaceID string         `json:"workspace_id"`
	Name        string         `json:"name"`
	Status      string         `json:"status"` // "provisioning" or "failed"
	Error       string         `json:"error,omitempty"`
	Children    []ImportResult `json:"children,omitempty"`
}

// Import provisions a workspace tree from a Bundle.
// It creates workspace records, writes config files to a temp dir, and triggers the provisioner.
func Import(
	ctx context.Context,
	b *Bundle,
	parentID *string,
	broadcaster *events.Broadcaster,
	prov *provisioner.Provisioner,
	platformURL string,
) ImportResult {
	// Generate fresh workspace ID
	wsID := uuid.New().String()

	result := ImportResult{
		WorkspaceID: wsID,
		Name:        b.Name,
		Status:      "provisioning",
	}

	// Create workspace record
	_, err := db.DB.ExecContext(ctx, `
		INSERT INTO workspaces (id, name, role, tier, status, parent_id, source_bundle_id)
		VALUES ($1, $2, $3, $4, 'provisioning', $5, $6)
	`, wsID, b.Name, nilIfEmpty(b.Description), b.Tier, parentID, b.ID)
	if err != nil {
		result.Status = "failed"
		result.Error = fmt.Sprintf("failed to create workspace record: %v", err)
		return result
	}

	_ = broadcaster.RecordAndBroadcast(ctx, "WORKSPACE_PROVISIONING", wsID, map[string]interface{}{
		"name":             b.Name,
		"tier":             b.Tier,
		"source_bundle_id": b.ID,
	})

	// Build config files in memory for the provisioner
	configFiles := buildBundleConfigFiles(b)

	// Extract runtime from config.yaml in the bundle
	bundleRuntime := "langgraph"
	if configYaml, ok := b.Prompts["config.yaml"]; ok {
		for _, line := range strings.Split(configYaml, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "runtime:") {
				bundleRuntime = strings.TrimSpace(strings.TrimPrefix(line, "runtime:"))
				break
			}
		}
	}
	// Store runtime in DB
	_, _ = db.DB.ExecContext(ctx, `UPDATE workspaces SET runtime = $1 WHERE id = $2`, bundleRuntime, wsID)

	// Provision the container if provisioner is available
	if prov != nil {
		cfg := provisioner.WorkspaceConfig{
			WorkspaceID: wsID,
			ConfigFiles: configFiles,
			Tier:        b.Tier,
			Runtime:     bundleRuntime,
			EnvVars:     map[string]string{},
			PlatformURL: platformURL,
			// PluginsPath set by caller if available
		}
		go func() {
			provCtx, cancel := context.WithTimeout(context.Background(), provisioner.ProvisionTimeout)
			defer cancel()
			url, err := prov.Start(provCtx, cfg)
			if err != nil {
				markFailed(provCtx, wsID, broadcaster, err)
			} else if url != "" {
				db.DB.ExecContext(provCtx, `UPDATE workspaces SET url = $1 WHERE id = $2`, url, wsID)
			}
		}()
	}

	// Recursively import sub-workspaces
	for _, sub := range b.SubWorkspaces {
		childResult := Import(ctx, &sub, &wsID, broadcaster, prov, platformURL)
		result.Children = append(result.Children, childResult)
	}

	return result
}

// buildBundleConfigFiles builds a map of config files from a bundle for writing into a container volume.
func buildBundleConfigFiles(b *Bundle) map[string][]byte {
	files := make(map[string][]byte)

	// Write system-prompt.md
	if b.SystemPrompt != "" {
		files["system-prompt.md"] = []byte(b.SystemPrompt)
	}

	// Write config.yaml from prompts if present
	if configYaml, ok := b.Prompts["config.yaml"]; ok {
		files["config.yaml"] = []byte(configYaml)
	}

	// Write skills
	for _, skill := range b.Skills {
		for relPath, content := range skill.Files {
			files[fmt.Sprintf("skills/%s/%s", skill.ID, relPath)] = []byte(content)
		}
	}

	return files
}

func markFailed(ctx context.Context, wsID string, broadcaster *events.Broadcaster, err error) {
	// Set last_sample_error along with status so operators (and the
	// Canvas E2E + GET /workspaces/:id callers) get a non-null reason
	// in the row. Pre-2026-05-05 this UPDATE only set status, leaving
	// last_sample_error NULL — Canvas E2E #2632 surfaced the gap with
	// `Workspace failed: (no last_sample_error)`. Same UPDATE shape as
	// markProvisionFailed in workspace-server/internal/handlers/
	// workspace_provision_shared.go.
	msg := err.Error()
	db.DB.ExecContext(ctx,
		`UPDATE workspaces SET status = $1, last_sample_error = $2, updated_at = now() WHERE id = $3`,
		models.StatusFailed, msg, wsID)
	broadcaster.RecordAndBroadcast(ctx, "WORKSPACE_PROVISION_FAILED", wsID, map[string]interface{}{
		"error": msg,
	})
}

func nilIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}
