package handlers

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
)

// ListRegistry handles GET /plugins — lists all available plugins from the registry.
// Supports optional ?runtime=<name> query param to filter to plugins that
// declare support for the given runtime. Plugins with no declared
// `runtimes` field are treated as "unspecified, try it" and included.
func (h *PluginsHandler) ListRegistry(c *gin.Context) {
	runtime := c.Query("runtime")
	c.JSON(http.StatusOK, h.listRegistryFiltered(runtime))
}

// listRegistryFiltered is the shared read-plus-filter path used by both
// /plugins and /workspaces/:id/plugins/available.
func (h *PluginsHandler) listRegistryFiltered(runtime string) []pluginInfo {
	plugins := []pluginInfo{}
	entries, err := os.ReadDir(h.pluginsDir)
	if err != nil {
		return plugins
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info := h.readPluginManifest(filepath.Join(h.pluginsDir, e.Name()), e.Name())
		if runtime != "" && !info.supportsRuntime(runtime) {
			continue
		}
		plugins = append(plugins, info)
	}
	return plugins
}

// ListAvailableForWorkspace handles GET /workspaces/:id/plugins/available —
// returns plugins from the registry filtered to those supported by the
// workspace's runtime. If no runtime lookup is wired, falls back to the
// full registry.
func (h *PluginsHandler) ListAvailableForWorkspace(c *gin.Context) {
	workspaceID := c.Param("id")
	runtime := ""
	if h.runtimeLookup != nil {
		if r, err := h.runtimeLookup(workspaceID); err == nil {
			runtime = r
		}
	}
	c.JSON(http.StatusOK, h.listRegistryFiltered(runtime))
}

// ListInstalled handles GET /workspaces/:id/plugins — lists plugins installed in the workspace.
func (h *PluginsHandler) ListInstalled(c *gin.Context) {
	workspaceID := c.Param("id")
	ctx := c.Request.Context()
	plugins := []pluginInfo{}

	// Dispatch order mirrors Install/Uninstall (plugins_install.go): a local
	// Docker container wins; otherwise fall back to the SaaS EIC path. Without
	// the SaaS branch ListInstalled returned [] for EVERY SaaS tenant (no LOCAL
	// container), so an installed plugin read back as not-installed even though
	// it was on the box — the "[] readback after a successful install" bug.
	if containerName := h.findRunningContainer(ctx, workspaceID); containerName != "" {
		// List directories in /configs/plugins/
		output, err := h.execInContainer(ctx, containerName, []string{
			"sh", "-c", "ls -1 /configs/plugins/ 2>/dev/null || true",
		})
		if err != nil {
			c.JSON(http.StatusOK, plugins)
			return
		}
		for _, name := range strings.Split(output, "\n") {
			name = strings.TrimSpace(name)
			if name == "" || validatePluginName(name) != nil {
				continue
			}
			// Try to read manifest from container (safe: name is validated)
			manifestOutput, err := h.execInContainer(ctx, containerName, []string{
				"cat", fmt.Sprintf("/configs/plugins/%s/plugin.yaml", name),
			})
			if err != nil || manifestOutput == "" {
				plugins = append(plugins, pluginInfo{Name: name})
				continue
			}
			info := parseManifestYAML(name, []byte(manifestOutput))
			plugins = append(plugins, info)
		}
		h.annotateRuntimeSupport(workspaceID, plugins)
		c.JSON(http.StatusOK, plugins)
		return
	}

	// SaaS path: list + read manifests over the EIC SSH tunnel.
	if instanceID, runtime := h.lookupSaaSDispatch(workspaceID); instanceID != "" {
		names, err := listPluginsViaEIC(ctx, instanceID, runtime)
		if err != nil {
			// Couldn't reach the box — return [] (not a 5xx) to match the
			// local path's fail-soft posture; the canvas treats an empty list
			// as "none installed / try again", never a hard error.
			log.Printf("ListInstalled: EIC list failed for %s: %v", workspaceID, err)
			c.JSON(http.StatusOK, plugins)
			return
		}
		for _, name := range names {
			if validatePluginName(name) != nil {
				continue
			}
			manifest, mErr := readPluginManifestViaEIC(ctx, instanceID, runtime, name)
			if mErr != nil || len(manifest) == 0 {
				plugins = append(plugins, pluginInfo{Name: name})
				continue
			}
			plugins = append(plugins, parseManifestYAML(name, manifest))
		}
		h.annotateRuntimeSupport(workspaceID, plugins)
		c.JSON(http.StatusOK, plugins)
		return
	}

	// Neither backend reachable — empty list (fail-soft, same as before).
	c.JSON(http.StatusOK, plugins)
}

// annotateRuntimeSupport stamps each plugin with whether it still supports the
// workspace's current runtime. Lets the canvas grey out plugins that went inert
// after a runtime change. Shared by the Docker and SaaS ListInstalled branches.
func (h *PluginsHandler) annotateRuntimeSupport(workspaceID string, plugins []pluginInfo) {
	if h.runtimeLookup == nil {
		return
	}
	runtime, err := h.runtimeLookup(workspaceID)
	if err != nil || runtime == "" {
		return
	}
	for i := range plugins {
		ok := plugins[i].supportsRuntime(runtime)
		plugins[i].SupportedOnRuntime = &ok
	}
}

// CheckRuntimeCompatibility handles GET /workspaces/:id/plugins/compatibility?runtime=<name>
// — preflight for runtime changes. Reports which installed plugins would
// become inert if the workspace switched to <runtime>. Canvas uses this
// to show a confirm dialog before applying the change.
func (h *PluginsHandler) CheckRuntimeCompatibility(c *gin.Context) {
	workspaceID := c.Param("id")
	targetRuntime := c.Query("runtime")
	ctx := c.Request.Context()

	if targetRuntime == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "runtime query parameter is required"})
		return
	}

	containerName := h.findRunningContainer(ctx, workspaceID)
	if containerName == "" {
		// Workspace not running — nothing installed yet, trivially compatible.
		c.JSON(http.StatusOK, gin.H{
			"target_runtime": targetRuntime,
			"compatible":     []pluginInfo{},
			"incompatible":   []pluginInfo{},
			"all_compatible": true,
		})
		return
	}

	output, err := h.execInContainer(ctx, containerName, []string{
		"sh", "-c", "ls -1 /configs/plugins/ 2>/dev/null || true",
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list installed plugins"})
		return
	}

	compatible := []pluginInfo{}
	incompatible := []pluginInfo{}
	for _, name := range strings.Split(output, "\n") {
		name = strings.TrimSpace(name)
		if name == "" || validatePluginName(name) != nil {
			continue
		}
		manifestOutput, err := h.execInContainer(ctx, containerName, []string{
			"cat", fmt.Sprintf("/configs/plugins/%s/plugin.yaml", name),
		})
		var info pluginInfo
		if err != nil || manifestOutput == "" {
			info = pluginInfo{Name: name}
		} else {
			info = parseManifestYAML(name, []byte(manifestOutput))
		}
		if info.supportsRuntime(targetRuntime) {
			compatible = append(compatible, info)
		} else {
			incompatible = append(incompatible, info)
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"target_runtime": targetRuntime,
		"compatible":     compatible,
		"incompatible":   incompatible,
		"all_compatible": len(incompatible) == 0,
	})
}
