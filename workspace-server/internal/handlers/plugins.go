package handlers

import (
	"bytes"
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/plugins"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/provisioner"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"gopkg.in/yaml.v3"
)

// RuntimeLookup resolves a workspace's runtime identifier by ID. The
// handler uses this to filter the plugin registry to compatible plugins
// without needing a direct DB dependency. A nil lookup disables
// workspace-scoped filtering (handler falls back to unfiltered list).
type RuntimeLookup func(workspaceID string) (string, error)

// InstanceIDLookup resolves a workspace's EC2 instance_id by ID. Empty
// string means the workspace is not on the SaaS (EC2-per-workspace)
// backend — i.e. either local-Docker or pre-provision. The handler uses
// this to dispatch plugin install/uninstall to the EIC SSH path
// (template_files_eic.go primitive) when a workspace runs on its own EC2
// and there's no local Docker container to exec into. A nil lookup keeps
// the handler on the local-Docker code path only — same shape as the
// pre-fix behaviour.
type InstanceIDLookup func(workspaceID string) (string, error)

// pluginSources is the contract PluginsHandler uses to talk to the
// plugin source registry. Extracted as an interface (#1814) so tests can
// substitute a stub without standing up the real *plugins.Registry +
// every concrete resolver. Production wires *plugins.Registry directly,
// which satisfies this interface — see the compile-time assertion below.
//
// Method set is intentionally narrow — only what handler code calls.
// Register is included because WithSourceResolver and NewPluginsHandler
// both invoke it; a stub that doesn't need to record registrations can
// implement it as a no-op.
type pluginSources interface {
	Register(resolver plugins.SourceResolver)
	Resolve(source plugins.Source) (plugins.SourceResolver, error)
	Schemes() []string
}

// Compile-time assertion: *plugins.Registry satisfies pluginSources.
// Catches a future method-signature drift at build time instead of when
// router wiring runs in main().
var _ pluginSources = (*plugins.Registry)(nil)

// PluginsHandler manages the plugin registry and per-workspace plugin installation.
type PluginsHandler struct {
	pluginsDir       string           // host path to plugins/ registry
	docker           *client.Client   // Docker client for container operations
	restartFunc      func(string)     // auto-restart workspace after install/uninstall
	runtimeLookup    RuntimeLookup    // workspace_id → runtime (optional)
	instanceIDLookup InstanceIDLookup // workspace_id → EC2 instance_id (optional)
	// sources narrowed from `*plugins.Registry` to the pluginSources
	// interface (#1814) so tests can substitute a stub. Production
	// callers still pass *plugins.Registry, which satisfies the
	// interface — see the compile-time assertion above.
	sources pluginSources
}

// NewPluginsHandler constructs a PluginsHandler with the default source
// registry (local + github resolvers). Deployments can add more schemes
// via WithSourceResolver before routes are wired — e.g. a private
// enterprise registry or ClawHub. Logs the effective install limits
// exactly once per process on first construction.
func NewPluginsHandler(pluginsDir string, docker *client.Client, restartFunc func(string)) *PluginsHandler {
	sources := plugins.NewRegistry()
	sources.Register(plugins.NewLocalResolver(pluginsDir))
	sources.Register(plugins.NewGithubResolver())
	logInstallLimitsOnce(os.Stderr)
	return &PluginsHandler{
		pluginsDir:  pluginsDir,
		docker:      docker,
		restartFunc: restartFunc,
		sources:     sources,
	}
}

// WithSourceResolver registers a custom source resolver (e.g. a ClawHub
// client) alongside the defaults. Call during router wiring, before the
// first request. Chainable.
func (h *PluginsHandler) WithSourceResolver(resolver plugins.SourceResolver) *PluginsHandler {
	h.sources.Register(resolver)
	return h
}

// WithRuntimeLookup installs a workspace-runtime resolver. Used by the
// router during wiring so tests don't need a real DB.
func (h *PluginsHandler) WithRuntimeLookup(lookup RuntimeLookup) *PluginsHandler {
	h.runtimeLookup = lookup
	return h
}

// WithInstanceIDLookup installs a workspace → EC2 instance_id resolver.
// Wired by the router so production hits a real DB; tests stub it. The
// install/uninstall pipeline uses this to dispatch to the EIC SSH path
// for SaaS workspaces (no local Docker container to exec into).
func (h *PluginsHandler) WithInstanceIDLookup(lookup InstanceIDLookup) *PluginsHandler {
	h.instanceIDLookup = lookup
	return h
}

// Sources returns the underlying plugin source registry. Used by main.go to
// pass the same registry to the drift sweeper so both share resolver state.
func (h *PluginsHandler) Sources() plugins.SourceResolver {
	return h.sources
}

// pluginInfo is the API response for a plugin.
type pluginInfo struct {
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Description string   `json:"description"`
	Author      string   `json:"author"`
	Tags        []string `json:"tags"`
	Skills      []string `json:"skills"`
	// Runtimes declares which workspace runtimes this plugin ships an adaptor
	// for. Empty means "unspecified" — the canvas still allows install (the
	// raw-drop fallback surfaces a warning at install time). Runtime names
	// use underscore form (e.g. "claude_code").
	Runtimes []string `json:"runtimes"`
	// SupportedOnRuntime is populated by ListInstalled/compatibility only.
	// When a workspace changes runtime, plugins whose manifest doesn't
	// declare the new runtime become inert (files present, tools unwired).
	// The canvas reads this to grey out rows.
	// Pointer so the field is omitted on endpoints that don't compute it.
	SupportedOnRuntime *bool `json:"supported_on_runtime,omitempty"`
}

// supportsRuntime returns true if the plugin declares support for the given
// runtime OR if it declares no runtimes at all (treat as "unspecified, try it").
// Comparison is normalized — "claude-code" and "claude_code" are equal.
func (p pluginInfo) supportsRuntime(runtime string) bool {
	if len(p.Runtimes) == 0 {
		return true
	}
	want := strings.ReplaceAll(runtime, "-", "_")
	for _, r := range p.Runtimes {
		if strings.ReplaceAll(r, "-", "_") == want {
			return true
		}
	}
	return false
}

func (h *PluginsHandler) readPluginManifest(pluginPath, fallbackName string) pluginInfo {
	data, err := os.ReadFile(filepath.Join(pluginPath, "plugin.yaml"))
	if err != nil {
		return pluginInfo{Name: fallbackName}
	}
	return parseManifestYAML(fallbackName, data)
}

// parseManifestYAML parses plugin.yaml bytes into pluginInfo.
func parseManifestYAML(fallbackName string, data []byte) pluginInfo {
	info := pluginInfo{Name: fallbackName}
	var raw map[string]interface{}
	if yaml.Unmarshal(data, &raw) != nil {
		return info
	}
	info.Version = strDefault(raw, "version", "")
	info.Description = strDefault(raw, "description", "")
	info.Author = strDefault(raw, "author", "")
	if tags, ok := raw["tags"].([]interface{}); ok {
		for _, t := range tags {
			if s, ok := t.(string); ok {
				info.Tags = append(info.Tags, s)
			}
		}
	}
	if skills, ok := raw["skills"].([]interface{}); ok {
		for _, s := range skills {
			if str, ok := s.(string); ok {
				info.Skills = append(info.Skills, str)
			}
		}
	}
	if runtimes, ok := raw["runtimes"].([]interface{}); ok {
		for _, r := range runtimes {
			if str, ok := r.(string); ok {
				info.Runtimes = append(info.Runtimes, str)
			}
		}
	}
	return info
}

func strDefault(m map[string]interface{}, key, fallback string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return fallback
}

// findRunningContainer returns the live container name for workspaceID, or ""
// when the container is genuinely not running OR the daemon errored
// transiently. Routed through provisioner.RunningContainerName as the SSOT
// (molecule-core#10) so this handler agrees with healthsweep on the same
// inputs. Transient daemon errors are logged distinctly so triage doesn't
// confuse a flaky daemon with a stopped container.
func (h *PluginsHandler) findRunningContainer(ctx context.Context, workspaceID string) string {
	name, err := provisioner.RunningContainerName(ctx, h.docker, workspaceID)
	if err != nil {
		log.Printf("plugins: docker inspect transient error for %s: %v (treating as not-running for this request)", workspaceID, err)
		return ""
	}
	return name
}

// isExternalRuntime reports whether the workspace's runtime is the
// `external` (remote-pull) shape introduced in Phase 30. External
// workspaces have no local container — `POST /plugins` (push-install via
// docker exec) doesn't apply to them; they pull via the download endpoint
// instead. Returns false (allow-install) if the lookup is unwired or
// errors — failing open here is safe because the downstream
// findRunningContainer step still gates on a real container being there.
//
// Background — molecule-core#10: without this check, external workspaces
// fall through to findRunningContainer's NotFound path and return a
// misleading 503 "container not running" instead of a clear "use the
// pull endpoint" message.
func (h *PluginsHandler) isExternalRuntime(workspaceID string) bool {
	if h.runtimeLookup == nil {
		return false
	}
	runtime, err := h.runtimeLookup(workspaceID)
	if err != nil {
		return false
	}
	return runtime == "external"
}

func (h *PluginsHandler) execAsRoot(ctx context.Context, containerName string, cmd []string) (string, error) {
	return h.execInContainerAs(ctx, containerName, "root", cmd)
}

func (h *PluginsHandler) execInContainer(ctx context.Context, containerName string, cmd []string) (string, error) {
	return h.execInContainerAs(ctx, containerName, "", cmd)
}

func (h *PluginsHandler) execInContainerAs(ctx context.Context, containerName, user string, cmd []string) (string, error) {
	execCfg := container.ExecOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
		User:         user,
	}
	execID, err := h.docker.ContainerExecCreate(ctx, containerName, execCfg)
	if err != nil {
		return "", err
	}
	resp, err := h.docker.ContainerExecAttach(ctx, execID.ID, container.ExecAttachOptions{})
	if err != nil {
		return "", err
	}
	defer resp.Close()
	var stdout bytes.Buffer
	stdcopy.StdCopy(&stdout, io.Discard, resp.Reader)
	return strings.TrimSpace(stdout.String()), nil
}
