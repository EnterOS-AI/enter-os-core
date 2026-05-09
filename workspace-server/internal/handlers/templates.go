package handlers

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/provisioner"
	"github.com/docker/docker/client"
	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"
)

// allowedRoots are the container paths that the Files API can browse.
var allowedRoots = map[string]bool{
	"/configs":   true,
	"/workspace": true,
	"/home":      true,
	"/plugins":   true,
}

// maxUploadFiles limits the number of files in a single import/replace.
const maxUploadFiles = 200

type TemplatesHandler struct {
	configsDir string
	docker     *client.Client
	// wh is used by Import and ReplaceFiles to call DefaultTier() so a
	// generated config.yaml's tier matches the SaaS-vs-self-hosted
	// boundary (#2910 PR-B). nil-tolerant — the field is unused when
	// the caller doesn't import templates that need a fresh config
	// generated.
	wh *WorkspaceHandler
}

// NewTemplatesHandler constructs a TemplatesHandler. wh may be nil for
// callers that only use the read-only template surfaces (List,
// ReadFile, ListFiles). Import + ReplaceFiles need wh non-nil so the
// generated config.yaml picks the SaaS-aware default tier.
func NewTemplatesHandler(configsDir string, dockerCli *client.Client, wh *WorkspaceHandler) *TemplatesHandler {
	return &TemplatesHandler{configsDir: configsDir, docker: dockerCli, wh: wh}
}

// modelSpec describes a single supported model on a template: its id (sent
// to the runtime), a human-readable label, and the env vars that must be
// present for that model to work (e.g. API keys).
type modelSpec struct {
	ID          string   `json:"id" yaml:"id"`
	Name        string   `json:"name,omitempty" yaml:"name"`
	RequiredEnv []string `json:"required_env,omitempty" yaml:"required_env"`
}

// providerRegistryEntry mirrors a row from a template's top-level
// `providers:` registry block (claude-code, hermes, etc.). Each entry
// fully describes one provider: its name, auth flow, the model id
// prefixes/aliases that route to it, an optional base_url override, and
// the env vars required to authenticate.
//
// This is the structured taxonomy the canvas's ProviderModelSelector
// comment anticipates ("Templates that ship explicit vendor metadata
// (future) should override the heuristic.") — surfacing it here lets
// the canvas drop its prefix-inference fallback for templates that ship
// an explicit registry. Templates without the block omit the field
// (omitempty); the canvas falls back to its current per-model
// required_env derivation.
type providerRegistryEntry struct {
	Name          string   `json:"name" yaml:"name"`
	AuthMode      string   `json:"auth_mode,omitempty" yaml:"auth_mode"`
	ModelPrefixes []string `json:"model_prefixes,omitempty" yaml:"model_prefixes"`
	ModelAliases  []string `json:"model_aliases,omitempty" yaml:"model_aliases"`
	BaseURL       string   `json:"base_url,omitempty" yaml:"base_url"`
	AuthEnv       []string `json:"auth_env,omitempty" yaml:"auth_env"`
}

type templateSummary struct {
	ID          string      `json:"id"`
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Tier        int         `json:"tier"`
	Runtime     string      `json:"runtime"`
	Model       string      `json:"model"`
	Models      []modelSpec `json:"models,omitempty"`
	// RequiredEnv mirrors runtime_config.required_env from the template's
	// config.yaml — the AND-required env vars the template declares at the
	// runtime level (separate from per-model required_env). The canvas
	// preflight uses this as the fallback provider when `models` is empty
	// so provider picker stays data-driven instead of hardcoded in the UI.
	RequiredEnv []string `json:"required_env,omitempty"`
	// Providers is the runtime's own list of supported provider slugs,
	// sourced from runtime_config.providers in the template's config.yaml.
	// The canvas Config tab surfaces this as the Provider override
	// dropdown (Option B PR-5). Data-driven so each runtime owns its own
	// taxonomy — hermes-agent supports 20+ providers; claude-code only
	// "anthropic"; gemini-cli only "gemini" — and a future runtime with
	// a different vendor list doesn't need a canvas edit. Empty list →
	// canvas falls back to deriving suggestions from `models[].id` slug
	// prefixes (still adapter-driven, just inferred).
	Providers []string `json:"providers,omitempty"`
	// ProviderRegistry is the structured provider taxonomy from the
	// template's TOP-LEVEL `providers:` block (separate from the
	// runtime_config.providers slug list above). Each entry carries
	// auth_env / model_prefixes / model_aliases / base_url so the canvas
	// can render an authoritative Provider→Model cascade without
	// re-deriving vendor metadata from per-model required_env tuples.
	//
	// Closes #235 (server-side enrichment): the `Providers []string`
	// field shipped a name list but never the structured payload the
	// canvas's ProviderModelSelector comment block anticipates as the
	// override for its prefix-inference heuristic. Pre-existing
	// templates without the top-level block omit the field
	// (omitempty); the canvas's existing per-model fallback continues
	// to work for them.
	ProviderRegistry []providerRegistryEntry `json:"provider_registry,omitempty"`
	Skills           []string                `json:"skills"`
	SkillCount       int                     `json:"skill_count"`
	// ProvisionTimeoutSeconds lets a slow runtime declare its expected
	// cold-boot duration in its template manifest. Canvas's
	// ProvisioningTimeout banner respects this per-workspace via the
	// `provision_timeout_ms` field in the workspace API response (#2054).
	// 0 = template hasn't declared one, falls through to canvas's
	// runtime-profile default.
	ProvisionTimeoutSeconds int `json:"provision_timeout_seconds,omitempty"`
}

// resolveTemplateDir finds the template directory for a workspace on the host.
// Only resolves to actual templates (not ws-* dirs since those are now Docker volumes).
// Returns empty string if no matching template is found.
func (h *TemplatesHandler) resolveTemplateDir(wsName string) string {
	nameDir := filepath.Join(h.configsDir, normalizeName(wsName))
	if _, err := os.Stat(nameDir); err == nil {
		return nameDir
	}
	// Search templates by config.yaml name field (e.g., org-pm has name: "PM")
	if tmpl := findTemplateByName(h.configsDir, wsName); tmpl != "" {
		return filepath.Join(h.configsDir, tmpl)
	}
	return ""
}

// List handles GET /templates
func (h *TemplatesHandler) List(c *gin.Context) {
	templates := make([]templateSummary, 0)
	walkTemplateConfigs(h.configsDir, func(id string, data []byte) {
		var raw struct {
			Name        string   `yaml:"name"`
			Description string   `yaml:"description"`
			Tier        int      `yaml:"tier"`
			Runtime     string   `yaml:"runtime"`
			Model       string   `yaml:"model"`
			Skills      []string `yaml:"skills"`
			// Top-level `providers:` block — structured registry. Distinct
			// from runtime_config.providers (slug list) below. Both shapes
			// coexist in production: claude-code ships the structured
			// registry, hermes still uses the slug list. /templates surfaces
			// both verbatim so each runtime owns its taxonomy.
			Providers     []providerRegistryEntry `yaml:"providers"`
			RuntimeConfig struct {
				Model                   string      `yaml:"model"`
				Models                  []modelSpec `yaml:"models"`
				RequiredEnv             []string    `yaml:"required_env"`
				Providers               []string    `yaml:"providers"`
				ProvisionTimeoutSeconds int         `yaml:"provision_timeout_seconds"`
			} `yaml:"runtime_config"`
		}
		if err := yaml.Unmarshal(data, &raw); err != nil {
			// Without this log a malformed config.yaml causes the
			// template to silently disappear from /templates with no
			// trace — the operator can't tell "excluded due to parse
			// error" from "never existed." That matters more now that
			// templates ship richer YAML shapes (top-level providers
			// registry, models[] with required_env, etc.) where a
			// type-shape mismatch on one field drops the whole entry.
			log.Printf("templates list: skip %s: yaml.Unmarshal: %v", id, err)
			return
		}

		// Model comes from either top-level (legacy) or runtime_config.model (current).
		model := raw.Model
		if model == "" {
			model = raw.RuntimeConfig.Model
		}

		templates = append(templates, templateSummary{
			ID:                      id,
			Name:                    raw.Name,
			Description:             raw.Description,
			Tier:                    raw.Tier,
			Runtime:                 raw.Runtime,
			Model:                   model,
			Models:                  raw.RuntimeConfig.Models,
			RequiredEnv:             raw.RuntimeConfig.RequiredEnv,
			Providers:               raw.RuntimeConfig.Providers,
			ProviderRegistry:        raw.Providers,
			Skills:                  raw.Skills,
			SkillCount:              len(raw.Skills),
			ProvisionTimeoutSeconds: raw.RuntimeConfig.ProvisionTimeoutSeconds,
		})
	})

	c.JSON(http.StatusOK, templates)
}

// ListFiles handles GET /workspaces/:id/files
// Lists files inside the running container's /configs directory (or /workspace, etc.).
// Falls back to host-side config templates directory when container isn't running.
func (h *TemplatesHandler) ListFiles(c *gin.Context) {
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	// Query params:
	//   ?root=  — base path in container (default: /configs)
	//   ?path=  — subdirectory to list (relative to root, default: "")
	//   ?depth= — max depth to recurse (default: 1, max: 5)
	rootPath := c.DefaultQuery("root", "/configs")
	if !allowedRoots[rootPath] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "root must be one of: /configs, /workspace, /home, /plugins"})
		return
	}
	subPath := c.DefaultQuery("path", "")
	if subPath != "" {
		if err := validateRelPath(subPath); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid path"})
			return
		}
	}
	depth := 1
	if d := c.Query("depth"); d != "" {
		n, err := strconv.Atoi(d)
		if err != nil || n < 1 || n > 5 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "depth must be 1-5"})
			return
		}
		depth = n
	}
	listPath := rootPath
	if subPath != "" {
		listPath = rootPath + "/" + subPath
	}

	var wsName, instanceID, runtime string
	if err := db.DB.QueryRowContext(ctx,
		`SELECT name, COALESCE(instance_id, ''), COALESCE(runtime, '') FROM workspaces WHERE id = $1`,
		workspaceID,
	).Scan(&wsName, &instanceID, &runtime); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return
	}

	type fileEntry struct {
		Path string `json:"path"`
		Size int64  `json:"size"`
		Dir  bool   `json:"dir"`
	}

	// SaaS workspace (EC2-per-workspace) — no Docker on this tenant. List
	// via SSH through the EIC endpoint, mirroring ReadFile/WriteFile's
	// dispatch. Pre-fix this branch was missing and SaaS workspaces
	// always fell through to local-Docker check (finds nothing on a SaaS
	// tenant) + template-dir fallback (returns the seed template, not
	// the persisted state, and almost never matches on user-named
	// workspaces). Net effect: the canvas Files tab always rendered "0
	// files / No config files yet" for SaaS workspaces, regardless of
	// what was actually on disk. See issue #2999.
	if instanceID != "" {
		entries, err := listFilesViaEIC(ctx, instanceID, runtime, rootPath, subPath, depth)
		if err != nil {
			log.Printf("ListFiles EIC for %s root=%s sub=%s: %v", workspaceID, rootPath, subPath, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to list files: %v", err)})
			return
		}
		// Translate to the handler's wire shape (the field names match
		// 1:1, but Go can't implicit-convert named struct types).
		out := make([]fileEntry, 0, len(entries))
		for _, e := range entries {
			out = append(out, fileEntry{Path: e.Path, Size: e.Size, Dir: e.Dir})
		}
		c.JSON(http.StatusOK, out)
		return
	}

	// Try container filesystem first
	if containerName := h.findContainer(ctx, workspaceID); containerName != "" {
		// Portable file listing: works on both GNU and BusyBox/Alpine.
		// Uses find + sh -c stat to output TYPE|SIZE|PATH per line.
		output, err := h.execInContainer(ctx, containerName, []string{
			"sh", "-c",
			fmt.Sprintf(`find '%s' -maxdepth %d -not -path '*/.git/*' -not -path '*/__pycache__/*' -not -path '*/node_modules/*' -not -name .DS_Store | while IFS= read -r f; do
				rel="${f#'%s'/}"; [ "$rel" = '%s' ] && continue; [ -z "$rel" ] && continue
				if [ -d "$f" ]; then echo "d|0|$rel"; else s=$(stat -c %%s "$f" 2>/dev/null || stat -f %%z "$f" 2>/dev/null || echo 0); echo "f|$s|$rel"; fi
			done`, listPath, depth, listPath, listPath),
		})
		if err != nil {
			log.Printf("Container file list failed, falling back to host: %v", err)
		} else {
			var files []fileEntry
			for _, line := range strings.Split(output, "\n") {
				parts := strings.SplitN(line, "|", 3)
				if len(parts) != 3 || parts[2] == "" {
					continue
				}
				size, _ := strconv.ParseInt(parts[1], 10, 64)
				files = append(files, fileEntry{
					Path: parts[2],
					Size: size,
					Dir:  parts[0] == "d",
				})
			}
			if files == nil {
				files = []fileEntry{}
			}
			c.JSON(http.StatusOK, files)
			return
		}
	}

	// Fallback: host-side template dir (only for templates, not ws-* workspace volumes)
	configDir := h.resolveTemplateDir(wsName)
	if configDir == "" {
		c.JSON(http.StatusOK, []fileEntry{})
		return
	}

	walkRoot := configDir
	if subPath != "" {
		walkRoot = filepath.Join(configDir, subPath)
	}
	if _, err := os.Stat(walkRoot); os.IsNotExist(err) {
		c.JSON(http.StatusOK, []fileEntry{})
		return
	}

	var files []fileEntry
	filepath.Walk(walkRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil || path == walkRoot {
			return nil
		}
		rel, _ := filepath.Rel(walkRoot, path)
		// Enforce depth limit
		if strings.Count(rel, string(filepath.Separator))+1 > depth {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		base := filepath.Base(rel)
		if base == ".git" || base == ".DS_Store" || base == "__pycache__" || base == "node_modules" {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		files = append(files, fileEntry{
			Path: rel,
			Size: info.Size(),
			Dir:  info.IsDir(),
		})
		return nil
	})

	if files == nil {
		files = []fileEntry{}
	}
	c.JSON(http.StatusOK, files)
}

// ReadFile handles GET /workspaces/:id/files/*path
func (h *TemplatesHandler) ReadFile(c *gin.Context) {
	workspaceID := c.Param("id")
	filePath := c.Param("path")
	if strings.HasPrefix(filePath, "/") {
		filePath = filePath[1:]
	}

	if err := validateRelPath(filePath); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid path"})
		return
	}

	ctx := c.Request.Context()
	rootPath := c.DefaultQuery("root", "/configs")
	if !allowedRoots[rootPath] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "root must be one of: /configs, /workspace, /home, /plugins"})
		return
	}

	var wsName, instanceID, runtime string
	if err := db.DB.QueryRowContext(ctx,
		`SELECT name, COALESCE(instance_id, ''), COALESCE(runtime, '') FROM workspaces WHERE id = $1`,
		workspaceID,
	).Scan(&wsName, &instanceID, &runtime); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return
	}

	// SaaS workspace (EC2-per-workspace) — no Docker on this tenant. Read
	// via SSH through the EIC endpoint, mirroring WriteFile's dispatch
	// in this same file. Pre-fix this branch was missing and SaaS
	// workspaces always fell through to the local-Docker container check
	// (finds nothing on a SaaS tenant) + template-dir fallback (returns
	// the seed template, not the persisted state). Net effect: the
	// canvas Config tab always 404'd for SaaS workspaces — visible to
	// users after #2781 added the "no config.yaml" error UX.
	//
	// `?root=` flows through resolveWorkspaceFilePath: "/configs" stays
	// the per-runtime managed-config indirection (claude-code → /configs,
	// hermes → /home/ubuntu/.hermes); other allow-listed roots
	// (`/home`, `/workspace`, `/plugins`) pass through literally so
	// list/read/write/delete agree on what file a tree row points to.
	if instanceID != "" {
		content, err := readFileViaEIC(ctx, instanceID, runtime, rootPath, filePath)
		if err == nil {
			c.JSON(http.StatusOK, gin.H{
				"path":    filePath,
				"content": string(content),
				"size":    len(content),
			})
			return
		}
		if errors.Is(err, os.ErrNotExist) {
			c.JSON(http.StatusNotFound, gin.H{"error": "file not found on workspace"})
			return
		}
		log.Printf("ReadFile EIC for %s path=%s: %v", workspaceID, filePath, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to read file: %v", err)})
		return
	}

	// Local Docker path: try the workspace container first. `cat` wants a
	// single path argument — passing rootPath and filePath as two args
	// would make `cat` try to read the rootPath directory (error) and
	// then resolve filePath relative to the container's cwd, which
	// isn't guaranteed to equal rootPath.
	if containerName := h.findContainer(ctx, workspaceID); containerName != "" {
		fullPath := strings.TrimRight(rootPath, "/") + "/" + filePath
		content, err := h.execInContainer(ctx, containerName, []string{"cat", fullPath})
		if err == nil {
			c.JSON(http.StatusOK, gin.H{
				"path":    filePath,
				"content": content,
				"size":    len(content),
			})
			return
		}
	}

	// Fallback: host-side template dir
	templateDir := h.resolveTemplateDir(wsName)
	if templateDir == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "file not found (container offline, no template)"})
		return
	}
	// validateRelPath is already called above (line 260) for the container path,
	// but the fallback below uses filePath directly in filepath.Join without
	// any sanitization. Re-validate before the host-side read to close the gap.
	if err := validateRelPath(filePath); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid path"})
		return
	}
	fullPath := filepath.Join(templateDir, filePath)
	data, err := os.ReadFile(fullPath)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "file not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"path":    filePath,
		"content": string(data),
		"size":    len(data),
	})
}

// WriteFile handles PUT /workspaces/:id/files/*path
func (h *TemplatesHandler) WriteFile(c *gin.Context) {
	workspaceID := c.Param("id")
	filePath := c.Param("path")
	if strings.HasPrefix(filePath, "/") {
		filePath = filePath[1:]
	}

	if err := validateRelPath(filePath); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid path"})
		return
	}

	var body struct {
		Content string `json:"content"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	ctx := c.Request.Context()
	rootPath := c.DefaultQuery("root", "/configs")
	if !allowedRoots[rootPath] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "root must be one of: /configs, /workspace, /home, /plugins"})
		return
	}
	var wsName, instanceID, runtime string
	if err := db.DB.QueryRowContext(ctx,
		`SELECT name, COALESCE(instance_id, ''), COALESCE(runtime, '') FROM workspaces WHERE id = $1`,
		workspaceID,
	).Scan(&wsName, &instanceID, &runtime); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return
	}

	// SaaS workspace (EC2-per-workspace) — no Docker on this tenant. Write
	// via SSH through the EIC endpoint to the runtime-specific path.
	// `?root=` flows through the same per-runtime / literal indirection
	// as ReadFile so list/read/write/delete agree on what file a tree
	// row points to.
	if instanceID != "" {
		if err := writeFileViaEIC(ctx, instanceID, runtime, rootPath, filePath, []byte(body.Content)); err != nil {
			log.Printf("WriteFile EIC for %s path=%s: %v", workspaceID, filePath, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to write file: %v", err)})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "saved", "path": filePath})
		if h.wh != nil {
			go h.wh.RestartByID(workspaceID)
		}
		return
	}

	// Local Docker path — write via CopyToContainer when container is running
	if containerName := h.findContainer(ctx, workspaceID); containerName != "" {
		singleFile := map[string]string{filePath: body.Content}
		if err := h.copyFilesToContainer(ctx, containerName, "/configs", singleFile); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to write file: %v", err)})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "saved", "path": filePath})
		if h.wh != nil {
			go h.wh.RestartByID(workspaceID)
		}
		return
	}

	// Container offline — write via ephemeral container mounting the config volume
	volName := provisioner.ConfigVolumeName(workspaceID)
	singleFile := map[string]string{filePath: body.Content}
	if err := h.writeViaEphemeral(ctx, volName, singleFile); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to write file: %v", err)})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "saved", "path": filePath})
	if h.wh != nil {
		go h.wh.RestartByID(workspaceID)
	}
}

// DeleteFile handles DELETE /workspaces/:id/files/*path
func (h *TemplatesHandler) DeleteFile(c *gin.Context) {
	workspaceID := c.Param("id")
	filePath := c.Param("path")
	// Reject absolute paths before stripping the leading slash — this check
	// must come before the strip so that "/etc/passwd" is not silently accepted.
	if filepath.IsAbs(filePath) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "absolute paths not permitted"})
		return
	}
	filePath = strings.TrimPrefix(filePath, "/")

	if err := validateRelPath(filePath); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid path"})
		return
	}

	ctx := c.Request.Context()
	rootPath := c.DefaultQuery("root", "/configs")
	if !allowedRoots[rootPath] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "root must be one of: /configs, /workspace, /home, /plugins"})
		return
	}
	var wsName, instanceID, runtime string
	if err := db.DB.QueryRowContext(ctx,
		`SELECT name, COALESCE(instance_id, ''), COALESCE(runtime, '') FROM workspaces WHERE id = $1`,
		workspaceID,
	).Scan(&wsName, &instanceID, &runtime); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return
	}

	// SaaS workspace (EC2-per-workspace) — no Docker on this tenant. Delete
	// via SSH through the EIC endpoint, mirroring ReadFile/WriteFile's
	// dispatch. Pre-fix this branch was missing — DeleteFile fell through
	// to local-Docker (no container) + ephemeral-volume (no Docker) and
	// silently 500'd. See issue #2999.
	if instanceID != "" {
		if err := deleteFileViaEIC(ctx, instanceID, runtime, rootPath, filePath); err != nil {
			log.Printf("DeleteFile EIC for %s root=%s path=%s: %v", workspaceID, rootPath, filePath, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to delete file: %v", err)})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "deleted", "path": filePath})
		if h.wh != nil {
			go h.wh.RestartByID(workspaceID)
		}
		return
	}

	// Delete via docker exec when container is running
	if containerName := h.findContainer(ctx, workspaceID); containerName != "" {
		// CWE-78: use filepath.Join instead of string concat to prevent path
		// injection into the exec argument. validateRelPath above is the primary
		// guard; filepath.Join is defence-in-depth. Use -f (not -rf) to avoid
		// recursive deletion of an entire directory via traversal.
		_, err := h.execInContainer(ctx, containerName, []string{"rm", "-f", filepath.Join("/configs", filePath)})
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to delete: %v", err)})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "deleted", "path": filePath})
		if h.wh != nil {
			go h.wh.RestartByID(workspaceID)
		}
		return
	}

	// Container offline — delete via ephemeral container
	volName := provisioner.ConfigVolumeName(workspaceID)
	if err := h.deleteViaEphemeral(ctx, volName, filePath); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to delete: %v", err)})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "deleted", "path": filePath})
	if h.wh != nil {
		go h.wh.RestartByID(workspaceID)
	}
}

