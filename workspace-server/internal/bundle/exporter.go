package bundle

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/provisioner"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"gopkg.in/yaml.v3"
)

// Export serializes a running workspace into a Bundle.
// dockerCli is optional — when provided, config is read from the running container's /configs volume.
func Export(ctx context.Context, workspaceID, configsDir string, dockerCli *client.Client) (*Bundle, error) {
	// Fetch workspace record
	var name, role, status string
	var tier int
	var agentCard []byte
	var parentID *string

	err := db.DB.QueryRowContext(ctx, `
		SELECT name, COALESCE(role, ''), tier, status,
		       COALESCE(agent_card, 'null'::jsonb), parent_id
		FROM workspaces WHERE id = $1
	`, workspaceID).Scan(&name, &role, &tier, &status, &agentCard, &parentID)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("workspace %s not found", workspaceID)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to fetch workspace: %w", err)
	}

	// Parse agent card
	var card interface{}
	if err := json.Unmarshal(agentCard, &card); err != nil {
		card = nil
	}

	b := &Bundle{
		Schema:      "1.0",
		ID:          workspaceID,
		Name:        name,
		Description: role,
		Tier:        tier,
		AgentCard:   card,
		Version:     "1.0.0",
	}

	// Initialize slices/maps before loadFromConfigDir uses them
	b.SubWorkspaces = []Bundle{}
	b.Skills = []BundleSkill{}
	b.Tools = []BundleTool{}
	b.Prompts = map[string]string{}

	// Try to read config from running container first, then fall back to host templates
	loaded := false
	if dockerCli != nil {
		containerName := provisioner.ContainerName(workspaceID)
		if err := b.loadFromContainer(ctx, dockerCli, containerName); err == nil {
			loaded = true
		}
	}
	if !loaded {
		// Fallback: read from host-side template directory
		configPath := findConfigDir(configsDir, name)
		if configPath != "" {
			b.loadFromConfigDir(configPath)
		}
	}

	// Recursively export sub-workspaces
	rows, err := db.DB.QueryContext(ctx,
		`SELECT id FROM workspaces WHERE parent_id = $1 AND status != 'removed'`, workspaceID)
	if err == nil {
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var childID string
			if rows.Scan(&childID) == nil {
				childBundle, err := Export(ctx, childID, configsDir, dockerCli)
				if err == nil {
					b.SubWorkspaces = append(b.SubWorkspaces, *childBundle)
				}
			}
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("export sub-workspaces: %w", err)
		}
	}

	return b, nil
}

// execInContainer runs a command in a container and returns stdout.
func execInContainer(ctx context.Context, dockerCli *client.Client, containerName string, cmd []string) (string, error) {
	execCfg := container.ExecOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
	}
	execID, err := dockerCli.ContainerExecCreate(ctx, containerName, execCfg)
	if err != nil {
		return "", err
	}
	resp, err := dockerCli.ContainerExecAttach(ctx, execID.ID, container.ExecAttachOptions{})
	if err != nil {
		return "", err
	}
	defer resp.Close()
	var stdout bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, io.Discard, io.LimitReader(resp.Reader, 5*1024*1024)); err != nil {
		return "", fmt.Errorf("failed to read exec output: %w", err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// loadFromContainer reads config files from a running container's /configs directory.
func (b *Bundle) loadFromContainer(ctx context.Context, dockerCli *client.Client, containerName string) error {
	// Check container is running
	info, err := dockerCli.ContainerInspect(ctx, containerName)
	if err != nil || !info.State.Running {
		return fmt.Errorf("container not running")
	}

	// Read system-prompt.md
	if content, err := execInContainer(ctx, dockerCli, containerName, []string{"cat", "/configs/system-prompt.md"}); err == nil {
		b.SystemPrompt = content
	}

	// Read config.yaml
	if content, err := execInContainer(ctx, dockerCli, containerName, []string{"cat", "/configs/config.yaml"}); err == nil {
		b.Prompts["config.yaml"] = content
	}

	// Read skills
	output, err := execInContainer(ctx, dockerCli, containerName, []string{"sh", "-c", "ls -1 /configs/skills/ 2>/dev/null || true"})
	if err != nil || output == "" {
		return nil
	}

	for _, skillName := range strings.Split(output, "\n") {
		skillName = strings.TrimSpace(skillName)
		if skillName == "" {
			continue
		}
		skill := BundleSkill{
			ID:    skillName,
			Name:  skillName,
			Files: map[string]string{},
		}

		// List files in skill directory
		skillFiles, err := execInContainer(ctx, dockerCli, containerName, []string{
			"find", "/configs/skills/" + skillName, "-type", "f",
		})
		if err != nil {
			continue
		}
		for _, filePath := range strings.Split(skillFiles, "\n") {
			filePath = strings.TrimSpace(filePath)
			if filePath == "" {
				continue
			}
			relPath := strings.TrimPrefix(filePath, "/configs/skills/"+skillName+"/")
			if content, err := execInContainer(ctx, dockerCli, containerName, []string{"cat", filePath}); err == nil {
				skill.Files[relPath] = content
			}
		}

		if content, ok := skill.Files["SKILL.md"]; ok {
			skill.Description = extractDescription(content)
		}
		b.Skills = append(b.Skills, skill)
	}

	return nil
}

// loadFromConfigDir reads config files and skills from a workspace config directory.
func (b *Bundle) loadFromConfigDir(dir string) {
	// Read system-prompt.md
	if data, err := os.ReadFile(filepath.Join(dir, "system-prompt.md")); err == nil {
		b.SystemPrompt = string(data)
	}

	// Read config.yaml for model/tools
	if data, err := os.ReadFile(filepath.Join(dir, "config.yaml")); err == nil {
		b.Prompts["config.yaml"] = string(data)
	}

	// Read skills
	skillsDir := filepath.Join(dir, "skills")
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		skill := BundleSkill{
			ID:    entry.Name(),
			Name:  entry.Name(),
			Files: map[string]string{},
		}

		// Walk all files in the skill directory
		skillPath := filepath.Join(skillsDir, entry.Name())
		_ = filepath.Walk(skillPath, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			relPath, _ := filepath.Rel(skillPath, path)
			data, err := os.ReadFile(path)
			if err == nil {
				skill.Files[relPath] = string(data)
			}
			return nil
		})

		// Extract description from SKILL.md if present
		if content, ok := skill.Files["SKILL.md"]; ok {
			skill.Description = extractDescription(content)
		}

		b.Skills = append(b.Skills, skill)
	}
}

// findConfigDir tries to match a workspace name to a config directory.
// It checks for a directory whose config.yaml "name" field matches the workspace name,
// falling back to the first directory with a config.yaml if no name match is found.
func findConfigDir(configsDir, name string) string {
	entries, err := os.ReadDir(configsDir)
	if err != nil {
		return ""
	}

	var fallback string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		configPath := filepath.Join(configsDir, e.Name(), "config.yaml")
		data, err := os.ReadFile(configPath)
		if err != nil {
			continue
		}
		// Check if the config name matches the workspace name
		var cfg struct {
			Name string `yaml:"name"`
		}
		if yaml.Unmarshal(data, &cfg) == nil && cfg.Name == name {
			return filepath.Join(configsDir, e.Name())
		}
		if fallback == "" {
			fallback = filepath.Join(configsDir, e.Name())
		}
	}
	return fallback
}

// extractDescription pulls the first non-empty line after YAML frontmatter.
func extractDescription(content string) string {
	inFrontmatter := false
	for _, line := range splitLines(content) {
		if line == "---" {
			inFrontmatter = !inFrontmatter
			continue
		}
		if !inFrontmatter && len(line) > 0 && line[0] != '#' {
			return line
		}
	}
	return ""
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}
