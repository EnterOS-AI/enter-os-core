package handlers

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/envx"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/wsauth"
	"github.com/gin-gonic/gin"
)

// Install handles POST /workspaces/:id/plugins — installs a plugin.
//
// Body: {"source": "<scheme>://<spec>"}
//
//   - {"source": "local://my-plugin"}               → install from platform registry
//   - {"source": "github://owner/repo"}             → install from GitHub
//   - {"source": "github://owner/repo#v1.2.0"}      → pinned ref
//   - {"source": "clawhub://sonoscli@1.2.0"}        → when a ClawHub resolver is registered
//
// The shape of the plugin (agentskills.io format, MCP server, DeepAgents
// sub-agent, …) is orthogonal and handled by the per-runtime adapter
// inside the workspace at startup.
func (h *PluginsHandler) Install(c *gin.Context) {
	workspaceID := c.Param("id")
	// External-runtime guard (molecule-core#10): push-install via docker
	// exec is meaningless for `runtime='external'` workspaces — they have
	// no local container. Reject early with a hint pointing at the
	// pull-mode endpoint, instead of falling through to a misleading
	// "container not running" 503 from findRunningContainer.
	if h.isExternalRuntime(workspaceID) {
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error": "plugin install via push is not supported for external runtimes",
			"hint":  "external workspaces pull plugins via GET /workspaces/:id/plugins/:name/download",
		})
		return
	}
	// Cap the JSON body so a pathological POST can't exhaust parser memory.
	bodyMax := envx.Int64("PLUGIN_INSTALL_BODY_MAX_BYTES", defaultInstallBodyMaxBytes)
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, bodyMax)

	// Bound the whole install (fetch + copy) so a slow/malicious source
	// can't tie up an HTTP handler goroutine indefinitely. Overridable
	// via PLUGIN_INSTALL_FETCH_TIMEOUT (duration string, e.g. "10m").
	timeout := envx.Duration("PLUGIN_INSTALL_FETCH_TIMEOUT", defaultInstallFetchTimeout)
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
	defer cancel()

	var req installRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	result, err := h.resolveAndStage(ctx, req)
	if err != nil {
		var he *httpErr
		if errors.As(err, &he) {
			c.JSON(he.Status, he.Body)
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "plugin install failed"})
		return
	}
	defer os.RemoveAll(result.StagedDir)

	// Org plugin allowlist gate (#591).
	// If the workspace's org has a non-empty allowlist, the plugin must be
	// on it. An empty allowlist means allow-all (backward compat).
	if blocked, reason := checkOrgPluginAllowlist(ctx, workspaceID, result.PluginName); blocked {
		c.JSON(http.StatusForbidden, gin.H{"error": reason})
		return
	}

	if err := h.deliverToContainer(ctx, workspaceID, result); err != nil {
		var he *httpErr
		if errors.As(err, &he) {
			c.JSON(he.Status, he.Body)
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "plugin deliver failed"})
		return
	}

	// Record the install in workspace_plugins (core#113 — version-subscription
	// foundation). Best-effort: DB write failure is logged but doesn't fail
	// the install — the plugin IS in the container; surfacing a 500 here
	// would mislead the caller about the install state.
	if err := recordWorkspacePluginInstall(ctx, workspaceID, result.PluginName, result.Source.Raw(), req.Track); err != nil {
		log.Printf("Plugin install: failed to record %s for %s in workspace_plugins: %v (install succeeded; tracking row missing)", result.PluginName, workspaceID, err)
	}

	log.Printf("Plugin install: %s via %s → workspace %s (restarting)", result.PluginName, result.Source.Scheme, workspaceID)
	c.JSON(http.StatusOK, gin.H{
		"status": "installed",
		"plugin": result.PluginName,
		"source": result.Source.Raw(),
	})
}

// Uninstall handles DELETE /workspaces/:id/plugins/:name — removes a plugin.
//
// Dispatch order mirrors Install's deliverToContainer:
//
//  1. Local Docker container up → exec rm -rf via existing helpers.
//  2. SaaS workspace (instance_id set) → ssh sudo rm -rf via EIC.
//  3. external runtime → 422 (caller manages its own plugin dir).
//  4. Neither → 503.
func (h *PluginsHandler) Uninstall(c *gin.Context) {
	workspaceID := c.Param("id")
	pluginName := c.Param("name")
	ctx := c.Request.Context()

	// Mirror Install's external-runtime guard (molecule-core#10) so the
	// two endpoints reject the same shape with the same message.
	if h.isExternalRuntime(workspaceID) {
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error": "plugin uninstall via docker exec is not supported for external runtimes",
			"hint":  "external workspaces manage their own plugin directory; remove it locally",
		})
		return
	}

	if err := validatePluginName(pluginName); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid plugin name"})
		return
	}

	if containerName := h.findRunningContainer(ctx, workspaceID); containerName != "" {
		h.uninstallViaDocker(ctx, c, workspaceID, pluginName, containerName)
		return
	}

	if instanceID, runtime := h.lookupSaaSDispatch(workspaceID); instanceID != "" {
		h.uninstallViaEIC(ctx, c, workspaceID, pluginName, instanceID, runtime)
		return
	}

	c.JSON(http.StatusServiceUnavailable, gin.H{"error": "workspace container not running"})
}

// uninstallViaDocker holds the historical Docker-exec uninstall flow.
// Extracted out of Uninstall so the new SaaS dispatch reads cleanly and
// the two backend bodies are visibly symmetric (same steps, different
// transport).
func (h *PluginsHandler) uninstallViaDocker(ctx context.Context, c *gin.Context, workspaceID, pluginName, containerName string) {
	// Read the plugin's manifest BEFORE deletion to learn which skill dirs
	// it owns, so we can clean them out of /configs/skills/ and avoid the
	// auto-restart re-mounting them. Issue #106.
	skillNames := h.readPluginSkillsFromContainer(ctx, containerName, pluginName)

	// 1. Strip plugin's rule/fragment markers from CLAUDE.md (mirrors
	//    AgentskillsAdaptor.uninstall lines 184-188). Best-effort: if
	//    the user edited CLAUDE.md, our marker stays untouched.
	h.stripPluginMarkersFromMemory(ctx, containerName, pluginName)

	// 2. Remove copied skill dirs declared in the plugin's plugin.yaml.
	for _, skill := range skillNames {
		if err := validatePluginName(skill); err != nil {
			// Defensive: a malformed skill name in plugin.yaml shouldn't
			// turn into a path-traversal exec. Just skip it.
			log.Printf("Plugin uninstall: skipping invalid skill name %q in %s: %v", skill, pluginName, err)
			continue
		}
		_, _ = h.execAsRoot(ctx, containerName, []string{
			"rm", "-rf", "/configs/skills/" + skill,
		})
	}

	// 3. Delete the plugin directory itself (as root to handle file ownership).
	_, err := h.execAsRoot(ctx, containerName, []string{
		"rm", "-rf", "/configs/plugins/" + pluginName,
	})
	if err != nil {
		log.Printf("Plugin uninstall: failed to remove %s from %s: %v", pluginName, workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to remove plugin"})
		return
	}

	// Verify deletion before restart
	h.execInContainer(ctx, containerName, []string{"sync"})

	// Auto-restart (small delay to ensure fs writes are flushed)
	if h.restartFunc != nil {
		go func() {
			time.Sleep(2 * time.Second)
			h.restartFunc(workspaceID)
		}()
	}

	log.Printf("Plugin uninstall: %s from workspace %s (restarting)", pluginName, workspaceID)
	c.JSON(http.StatusOK, gin.H{
		"status": "uninstalled",
		"plugin": pluginName,
	})
}

// uninstallViaEIC removes a plugin from a SaaS workspace EC2 over SSH.
// Symmetric with uninstallViaDocker:
//
//   - Read manifest (best-effort, missing plugin.yaml = no skills to clean).
//   - Skip CLAUDE.md awk-strip for now: that file lives at
//     <runtime-config-prefix>/CLAUDE.md on the host and the same awk script
//     would work over ssh, but the file is rewritten on workspace restart
//     by the runtime adapter anyway, so the marker either stays harmless
//     or gets dropped on the next install/restart cycle. Tracked as
//     follow-up; not a regression vs the docker path's semantics here.
//   - rm -rf the plugin dir.
//   - Trigger restart.
//
// We intentionally don't try to remove /configs/skills/<skill> entries
// over ssh because the same /configs is bind-mounted into the runtime
// container; the agent's own start-up adapter rewrites that tree from
// the live plugin set, so a stale skill dir for an uninstalled plugin
// is cleaned up at restart. The docker path removes them eagerly only
// because docker-exec is cheap. We can mirror that later if a real bug
// surfaces, but adding two extra ssh round-trips per uninstall today
// would be churn for no behavioural win.
func (h *PluginsHandler) uninstallViaEIC(ctx context.Context, c *gin.Context, workspaceID, pluginName, instanceID, runtime string) {
	// Read manifest first (best-effort) — we don't currently use the
	// skills list on the SaaS path (see comment above), but reading it
	// keeps the parsing path warm and lets log lines distinguish "we
	// deleted a real plugin" from "user asked us to delete something
	// that wasn't there." Errors here are swallowed: missing manifest
	// must not block uninstall.
	if data, err := readPluginManifestViaEIC(ctx, instanceID, runtime, pluginName); err == nil && len(data) > 0 {
		info := parseManifestYAML(pluginName, data)
		if len(info.Skills) > 0 {
			log.Printf("Plugin uninstall: %s declared skills=%v (left to runtime restart to clean)", pluginName, info.Skills)
		}
	}

	if err := uninstallPluginViaEIC(ctx, instanceID, runtime, pluginName); err != nil {
		log.Printf("Plugin uninstall: EIC rm failed for %s on %s: %v", pluginName, workspaceID, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to remove plugin from workspace EC2"})
		return
	}

	if h.restartFunc != nil {
		go func() {
			time.Sleep(2 * time.Second)
			h.restartFunc(workspaceID)
		}()
	}

	log.Printf("Plugin uninstall: %s from workspace %s (restarting via SaaS path)", pluginName, workspaceID)
	c.JSON(http.StatusOK, gin.H{
		"status": "uninstalled",
		"plugin": pluginName,
	})
}

// Download handles GET /workspaces/:id/plugins/:name/download?source=<scheme://spec>
//
// Phase 30.3 — stream the named plugin as a gzipped tarball so remote
// agents can pull and unpack locally. Replaces the Docker-exec install
// path for `runtime='external'` workspaces.
//
// The `source` query parameter is optional. When omitted we default to
// `local://<name>` (the platform's curated registry). When set, any
// registered scheme works — `github://owner/repo`, future `clawhub://…`,
// etc. — which lets a workspace install plugins from upstream repos
// without the platform pre-staging them.
//
// Auth: requires the workspace's bearer token (same shape as 30.2). A
// plugin tarball often ships rule text + skill files that reference
// internal APIs, so we prefer fail-closed on DB errors to prevent a
// hiccup from turning this into an unauth'd download endpoint.
func (h *PluginsHandler) Download(c *gin.Context) {
	workspaceID := c.Param("id")
	pluginName := c.Param("name")
	ctx := c.Request.Context()

	if err := validatePluginName(pluginName); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid plugin name"})
		return
	}

	// Auth gate — workspace token required (fail-closed on DB errors).
	hasLive, hlErr := wsauth.HasAnyLiveToken(ctx, db.DB, workspaceID)
	if hlErr != nil {
		log.Printf("wsauth: plugin.Download HasAnyLiveToken(%s) failed: %v", workspaceID, hlErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "auth check failed"})
		return
	}
	if hasLive {
		tok := wsauth.BearerTokenFromHeader(c.GetHeader("Authorization"))
		if tok == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "missing workspace auth token"})
			return
		}
		if err := wsauth.ValidateToken(ctx, db.DB, workspaceID, tok); err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid workspace auth token"})
			return
		}
	}

	// Resolve source — default to local://<name> when caller doesn't
	// specify. This is the common case: pulling a platform-curated
	// plugin by its canonical name.
	source := c.Query("source")
	if source == "" {
		source = "local://" + pluginName
	}

	// Reuse the existing install-layer bounds so download shares
	// fetch-timeout, body limits, and staged-dir size caps with Install.
	timeout := envx.Duration("PLUGIN_INSTALL_FETCH_TIMEOUT", defaultInstallFetchTimeout)
	fetchCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	result, err := h.resolveAndStage(fetchCtx, installRequest{Source: source})
	if err != nil {
		var he *httpErr
		if errors.As(err, &he) {
			c.JSON(he.Status, he.Body)
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "plugin download failed"})
		return
	}
	defer os.RemoveAll(result.StagedDir)

	// Sanity: resolved plugin name must match the URL path param.
	// Resolvers can return a plugin.yaml-derived name that differs
	// from the URL segment; reject the mismatch rather than ship a
	// tarball labeled "foo" that actually contains plugin "bar".
	if result.PluginName != pluginName {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":          fmt.Sprintf("source resolved to plugin %q but URL requested %q", result.PluginName, pluginName),
			"resolved_name":  result.PluginName,
			"requested_name": pluginName,
		})
		return
	}

	// Buffer the full tar.gz before writing any response bytes. This lets
	// us emit a clean 5xx if tar packing fails — previously, a partial
	// stream surfaced as HTTP 200 + truncated body, which made remote
	// agents fail at unpack time with cryptic gzip errors instead of
	// distinguishing "platform borked" from "network glitch".
	//
	// Plugin sizes are bounded by PLUGIN_INSTALL_MAX_DIR_BYTES (default
	// 100 MiB) which `resolveAndStage` already validated — buffering at
	// that scale is acceptable. If we ever raise the cap above ~500 MiB,
	// switch to a temp file backed io.ReadSeeker and use http.ServeContent.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := streamDirAsTar(result.StagedDir, tw); err != nil {
		log.Printf("plugin.Download: tar pack failed for %s: %v", pluginName, err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":  "failed to pack plugin",
			"plugin": pluginName,
		})
		return
	}
	if err := tw.Close(); err != nil {
		log.Printf("plugin.Download: tar close failed for %s: %v", pluginName, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to finalize tar"})
		return
	}
	if err := gz.Close(); err != nil {
		log.Printf("plugin.Download: gzip close failed for %s: %v", pluginName, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to finalize gzip"})
		return
	}

	c.Header("Content-Type", "application/gzip")
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.tar.gz"`, pluginName))
	c.Header("Content-Length", fmt.Sprintf("%d", buf.Len()))
	c.Header("X-Plugin-Name", pluginName)
	c.Header("X-Plugin-Source", result.Source.Raw())
	if _, err := c.Writer.Write(buf.Bytes()); err != nil {
		log.Printf("plugin.Download: response write failed for %s: %v", pluginName, err)
	}
}
