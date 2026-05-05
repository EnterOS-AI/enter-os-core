package handlers

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/provisioner"
	"github.com/gin-gonic/gin"
)

// normalizeName converts a display name to a directory-safe lowercase-hyphen string.
// Only allows alphanumeric, hyphens, and underscores. Strips everything else.
func normalizeName(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r == ' ' || r == '-' {
			b.WriteRune('-')
		} else if r >= 'A' && r <= 'Z' {
			b.WriteRune(r + 32)
		} else if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		}
		// Skip all other characters (dots, slashes, etc.)
	}
	result := b.String()
	// Prevent path traversal
	result = strings.ReplaceAll(result, "..", "")
	if result == "" {
		result = "unnamed"
	}
	return result
}

// generateDefaultConfig creates a config.yaml from detected prompt files
// and skills. tier is the deployment-aware default (caller passes
// h.wh.DefaultTier() — T4 on SaaS, T3 on self-hosted) so the generated
// file matches what POST /workspaces would default to. Pre-#2910 this
// was hardcoded to 3, which split-brained with the create-handler
// default on SaaS (T4) and pinned newly-imported templates at T3 even
// when downstream Create paths picked T4.
func generateDefaultConfig(name string, files map[string]string, tier int) string {
	promptFiles := []string{}
	skillSet := map[string]bool{}

	for path := range files {
		// Root .md files are prompt files
		if filepath.Dir(path) == "." && filepath.Ext(path) == ".md" {
			promptFiles = append(promptFiles, path)
		}
		// Detect skills from skills/*/SKILL.md
		if filepath.Base(path) == "SKILL.md" {
			dir := filepath.Dir(path)
			if filepath.Dir(dir) == "skills" {
				skillSet[filepath.Base(dir)] = true
			}
		}
	}

	// yamlEscape escapes a string for use as a YAML double-quoted scalar.
	// All five dangerous characters are neutralised: \\ / \" / \n / \r / \t.
	// This mirrors the escaping applied to the name field (#221) and is
	// extended here to skill names and prompt file names (#460): those values
	// also derive from attacker-controlled body.Files keys, and an unquoted
	// list item like "  - foo\nruntime: pwned\n" would inject a top-level key.
	yamlEscape := strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		"\n", `\n`,
		"\r", `\r`,
		"\t", `\t`,
	).Replace

	escaped := yamlEscape(name)

	var cfg strings.Builder
	cfg.WriteString(`name: "` + escaped + `"` + "\n")
	cfg.WriteString("description: Imported agent\n")
	// Tier is SaaS-aware via the caller's DefaultTier (#2910 PR-B).
	// Bounds-checked: invalid input falls back to T3 (the historical
	// default + the safer-of-the-two when the deployment mode can't
	// be resolved).
	if tier < 1 || tier > 4 {
		tier = 3
	}
	cfg.WriteString("version: 1.0.0\n")
	cfg.WriteString(fmt.Sprintf("tier: %d\n", tier))
	cfg.WriteString("model: anthropic:claude-haiku-4-5-20251001\n")
	cfg.WriteString("\nprompt_files:\n")
	if len(promptFiles) > 0 {
		for _, f := range promptFiles {
			cfg.WriteString(`  - "` + yamlEscape(f) + `"` + "\n")
		}
	} else {
		cfg.WriteString("  - system-prompt.md\n")
	}
	cfg.WriteString("\nskills:\n")
	if len(skillSet) > 0 {
		for s := range skillSet {
			cfg.WriteString(`  - "` + yamlEscape(s) + `"` + "\n")
		}
	} else {
		cfg.WriteString("  []\n")
	}
	cfg.WriteString("\ntools: []\n")
	cfg.WriteString("\na2a:\n  port: 8000\n  streaming: true\n  push_notifications: true\n")
	cfg.WriteString("\nenv:\n  required:\n    - ANTHROPIC_API_KEY\n  optional: []\n")
	return cfg.String()
}

// writeFiles writes a map of relative paths → content into destDir, validating each path.
func writeFiles(destDir string, files map[string]string) error {
	for relPath, content := range files {
		if err := validateRelPath(relPath); err != nil {
			return err
		}
		fullPath := filepath.Join(destDir, relPath)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
			return fmt.Errorf("failed to create directory for %s: %w", relPath, err)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0600); err != nil {
			return fmt.Errorf("failed to write %s: %w", relPath, err)
		}
	}
	return nil
}

// Import handles POST /templates/import
func (h *TemplatesHandler) Import(c *gin.Context) {
	var body struct {
		Name  string            `json:"name" binding:"required"`
		Files map[string]string `json:"files" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	if len(body.Files) > maxUploadFiles {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("too many files (%d), max %d", len(body.Files), maxUploadFiles)})
		return
	}

	dirName := normalizeName(body.Name)
	destDir := filepath.Join(h.configsDir, dirName)

	if _, err := os.Stat(destDir); err == nil {
		c.JSON(http.StatusConflict, gin.H{"error": "template already exists", "id": dirName})
		return
	}

	if err := writeFiles(destDir, body.Files); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	// Auto-generate config.yaml if not provided
	if _, exists := body.Files["config.yaml"]; !exists {
		tier := 3
		if h.wh != nil {
			tier = h.wh.DefaultTier()
		}
		cfg := generateDefaultConfig(body.Name, body.Files, tier)
		if err := os.WriteFile(filepath.Join(destDir, "config.yaml"), []byte(cfg), 0600); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to write config.yaml"})
			return
		}
	}

	c.JSON(http.StatusCreated, gin.H{"status": "imported", "id": dirName, "name": body.Name})
}

// ReplaceFiles handles PUT /workspaces/:id/files
func (h *TemplatesHandler) ReplaceFiles(c *gin.Context) {
	workspaceID := c.Param("id")

	var body struct {
		Files map[string]string `json:"files" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	if len(body.Files) > maxUploadFiles {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("too many files (%d), max %d", len(body.Files), maxUploadFiles)})
		return
	}

	ctx := c.Request.Context()
	var wsName, instanceID, runtime string
	if err := db.DB.QueryRowContext(ctx,
		`SELECT name, COALESCE(instance_id, ''), COALESCE(runtime, '') FROM workspaces WHERE id = $1`,
		workspaceID,
	).Scan(&wsName, &instanceID, &runtime); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return
	}

	// Validate all paths first
	for relPath := range body.Files {
		if err := validateRelPath(relPath); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}
	}

	// SaaS workspace (EC2-per-workspace) — route bulk write through the
	// EIC endpoint, one SSH session per file. Per-file cost is ~3s
	// (key push + tunnel + install), so up to 10 files is fine; above
	// that we should reuse the tunnel across multiple writes — tracked
	// as a follow-up.
	if instanceID != "" {
		for relPath, content := range body.Files {
			if err := writeFileViaEIC(ctx, instanceID, runtime, relPath, []byte(content)); err != nil {
				log.Printf("ReplaceFiles EIC for %s path=%s: %v", workspaceID, relPath, err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to write file %s: %v", relPath, err)})
				return
			}
		}
		c.JSON(http.StatusOK, gin.H{
			"status":    "replaced",
			"workspace": workspaceID,
			"files":     len(body.Files),
			"source":    "ec2-ssh",
		})
		return
	}

	// Write via Docker CopyToContainer when container is running
	if containerName := h.findContainer(ctx, workspaceID); containerName != "" {
		if err := h.copyFilesToContainer(ctx, containerName, "/configs", body.Files); err != nil {
			log.Printf("ReplaceFiles: copyFilesToContainer failed for %s: %v", workspaceID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to write files to workspace"})
			return
		}

		// Auto-generate config.yaml if not provided
		if _, exists := body.Files["config.yaml"]; !exists {
			// Check if config.yaml exists in container
			if _, err := h.execInContainer(ctx, containerName, []string{"test", "-f", "/configs/config.yaml"}); err != nil {
				tier := 3
				if h.wh != nil {
					tier = h.wh.DefaultTier()
				}
				cfg := generateDefaultConfig(wsName, body.Files, tier)
				singleFile := map[string]string{"config.yaml": cfg}
				h.copyFilesToContainer(ctx, containerName, "/configs", singleFile)
			}
		}

		c.JSON(http.StatusOK, gin.H{
			"status":    "replaced",
			"workspace": workspaceID,
			"files":     len(body.Files),
			"source":    "container",
		})
		return
	}

	// Container offline — try ephemeral container to write to volume
	volName := provisioner.ConfigVolumeName(workspaceID)
	if err := h.writeViaEphemeral(ctx, volName, body.Files); err != nil {
		// Last resort: write to host-side template dir
		destDir := h.resolveTemplateDir(wsName)
		if destDir == "" {
			log.Printf("ReplaceFiles: writeViaEphemeral failed and no template dir for %s: %v", workspaceID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to write files to workspace"})
			return
		}
		os.MkdirAll(destDir, 0o755)
		if err := writeFiles(destDir, body.Files); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "replaced", "workspace": workspaceID, "files": len(body.Files), "source": "template"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "replaced", "workspace": workspaceID, "files": len(body.Files), "source": "volume"})
}
