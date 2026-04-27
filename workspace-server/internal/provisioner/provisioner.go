// Package provisioner manages Docker container lifecycle for workspace agents.
package provisioner

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	dockerimage "github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// ErrNoBackend is returned by lifecycle methods (Stop, IsRunning) when
// the receiver is zero-valued — i.e. no Docker daemon connection has
// been wired up. The orphan sweeper and the contract-test scaffolding
// both speculatively call these methods on a possibly-nil receiver;
// returning a typed error rather than panicking lets callers reason
// about the case explicitly. See docs/architecture/backends.md
// drift-risk #6.
var ErrNoBackend = errors.New("provisioner: no backend configured (zero-valued receiver)")

// RuntimeImages maps runtime names to their Docker image refs on GHCR.
// Each standalone template repo publishes its image via the reusable
// publish-template-image workflow in molecule-ci on every main merge.
// The provisioner pulls these on demand (see ensureImageLocal) — no
// pre-build step on the tenant host.
//
// Legacy local-build path (`docker build -t workspace-template:<runtime>`
// via scripts/build-images.sh) is still supported for development:
// when a bare `workspace-template:<runtime>` image is present locally,
// Docker's image resolver matches it before any pull is attempted. Set
// the env var WORKSPACE_IMAGE_LOCAL_OVERRIDE=1 (enforced by callers) to
// short-circuit pulls entirely if needed.
var RuntimeImages = map[string]string{
	"langgraph":   "ghcr.io/molecule-ai/workspace-template-langgraph:latest",
	"claude-code": "ghcr.io/molecule-ai/workspace-template-claude-code:latest",
	"openclaw":    "ghcr.io/molecule-ai/workspace-template-openclaw:latest",
	"deepagents":  "ghcr.io/molecule-ai/workspace-template-deepagents:latest",
	"crewai":      "ghcr.io/molecule-ai/workspace-template-crewai:latest",
	"autogen":     "ghcr.io/molecule-ai/workspace-template-autogen:latest",
	"hermes":      "ghcr.io/molecule-ai/workspace-template-hermes:latest",     // Hermes (Nous Research) — real hermes-agent behind A2A bridge
	"gemini-cli":  "ghcr.io/molecule-ai/workspace-template-gemini-cli:latest", // Google Gemini CLI
}

const (
	// DefaultImage is the fallback workspace Docker image (langgraph is the most common runtime).
	DefaultImage = "ghcr.io/molecule-ai/workspace-template-langgraph:latest"
	// NOTE: Every runtime MUST have an entry in RuntimeImages above. If a runtime is missing,
	// it falls back to DefaultImage which may have wrong deps. Add new runtimes to both
	// RuntimeImages AND create the standalone template repo.

	// DefaultNetwork is the Docker network workspaces join.
	DefaultNetwork = "molecule-monorepo-net"

	// DefaultPort is the port the A2A server listens on inside the container.
	DefaultPort = "8000"

	// ProvisionTimeout is how long to wait for first heartbeat before marking as failed.
	ProvisionTimeout = 3 * time.Minute
)

// WorkspaceConfig holds the parameters needed to provision a workspace container.
type WorkspaceConfig struct {
	WorkspaceID        string
	TemplatePath       string            // Host path to template dir to copy from (e.g. claude-code-default/)
	ConfigFiles        map[string][]byte // Generated config files to write into /configs volume
	PluginsPath        string            // Host path to plugins directory (mounted at /plugins)
	WorkspacePath      string            // Host path to bind-mount as /workspace (if empty, uses Docker named volume)
	Tier               int
	Runtime            string            // "langgraph" (default) or "claude-code", "codex", "ollama", "custom"
	EnvVars            map[string]string // Additional env vars (API keys, etc.)
	PlatformURL        string
	AwarenessURL       string
	AwarenessNamespace string
	WorkspaceAccess    string // #65: "none" (default), "read_only", or "read_write"
	ResetClaudeSession bool   // #12: if true, discard the claude-sessions volume before start (fresh session dir)
}

// Workspace-access constants for #65. Matches the CHECK constraint on
// the workspaces.workspace_access column (migration 019).
const (
	WorkspaceAccessNone      = "none"
	WorkspaceAccessReadOnly  = "read_only"
	WorkspaceAccessReadWrite = "read_write"
)

// ConfigVolumeName returns the Docker named volume for a workspace's configs.
func ConfigVolumeName(workspaceID string) string {
	id := workspaceID
	if len(id) > 12 {
		id = id[:12]
	}
	return fmt.Sprintf("ws-%s-configs", id)
}

// ClaudeSessionVolumeName returns the Docker named volume for a workspace's
// Claude Code session directory (/root/.claude/sessions). Separate from the
// config volume so it can be discarded independently (via WORKSPACE_RESET_SESSION
// or ?reset=true) without wiping the user's config. Issue #12.
func ClaudeSessionVolumeName(workspaceID string) string {
	id := workspaceID
	if len(id) > 12 {
		id = id[:12]
	}
	return fmt.Sprintf("ws-%s-claude-sessions", id)
}

// Provisioner manages Docker containers for workspace agents.
type Provisioner struct {
	cli *client.Client
}

// New creates a new Provisioner connected to the local Docker daemon.
func New() (*Provisioner, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("failed to connect to Docker: %w", err)
	}
	return &Provisioner{cli: cli}, nil
}

// ContainerName returns the Docker container name for a workspace.
func ContainerName(workspaceID string) string {
	id := workspaceID
	if len(id) > 12 {
		id = id[:12]
	}
	return fmt.Sprintf("ws-%s", id)
}

// containerNamePrefix is the shared prefix every workspace container
// name carries (`ws-`). Used by ListWorkspaceContainerIDPrefixes for
// the Docker name-filter, and by the orphan sweeper to recognise our
// own containers vs. anything else on the host.
const containerNamePrefix = "ws-"

// LabelManaged is stamped on every workspace container + volume the
// provisioner creates. It's the orphan sweeper's signal for "I (or a
// previous platform process on this deployment) provisioned this" —
// without it, the sweeper has to assume any ws-* container might
// belong to a different platform sharing the same Docker daemon and
// only reaps things whose workspace row explicitly says
// status='removed'. With it, the sweeper can confidently reap a
// labeled container whose workspace row no longer exists at all
// (the wiped-DB case after `docker compose down -v`).
const LabelManaged = "molecule.platform.managed"

// managedLabels is the canonical label map applied to every workspace
// container + volume. Pulled out so a future addition (e.g. instance
// UUID for multi-platform-shared-daemon disambiguation) is one edit.
func managedLabels() map[string]string {
	return map[string]string{LabelManaged: "true"}
}

// ListWorkspaceContainerIDPrefixes returns the 12-char workspace ID
// prefixes of every running ws-* container the Docker daemon knows
// about. The 12-char form matches ContainerName's truncation, so the
// orphan sweeper can intersect this set against `SELECT
// substring(id::text, 1, 12) FROM workspaces WHERE status = 'removed'`
// without an extra round-trip per row.
//
// Returns an empty slice on any Docker error (sweeper treats that as
// "skip this round" — better than a partial scan that misses leaks).
func (p *Provisioner) ListWorkspaceContainerIDPrefixes(ctx context.Context) ([]string, error) {
	if p == nil || p.cli == nil {
		return nil, nil
	}
	containers, err := p.cli.ContainerList(ctx, container.ListOptions{
		// All=true catches stopped-but-not-removed containers too —
		// those still hold their volume references and would block
		// RemoveVolume just like a running container would.
		All:     true,
		Filters: filters.NewArgs(filters.Arg("name", containerNamePrefix)),
	})
	if err != nil {
		return nil, err
	}
	prefixes := make([]string, 0, len(containers))
	for _, c := range containers {
		// Container names from the API include a leading slash:
		// "/ws-abc123def456". Strip both the slash and our prefix
		// to recover the 12-char workspace ID.
		//
		// The Docker name filter is a SUBSTRING match (not a prefix
		// match), so something like "my-ws-thing" would also be
		// returned. The HasPrefix check below is load-bearing:
		// without it those false positives would flow into the
		// orphan sweeper's DB query as bogus LIKE patterns.
		for _, name := range c.Names {
			n := strings.TrimPrefix(name, "/")
			if !strings.HasPrefix(n, containerNamePrefix) {
				continue
			}
			id := strings.TrimPrefix(n, containerNamePrefix)
			if id == "" {
				continue
			}
			prefixes = append(prefixes, id)
			break // one name is enough; multiple aliases would dup
		}
	}
	return prefixes, nil
}

// ListManagedContainerIDPrefixes returns the workspace ID prefix of every
// container carrying the LabelManaged stamp. Distinct from
// ListWorkspaceContainerIDPrefixes (name-filtered, may include sibling
// platforms' containers on a shared Docker daemon): this method is the
// "things definitely provisioned by a Molecule platform process" set.
//
// The orphan sweeper uses this for its second pass — reaping containers
// whose workspace row no longer exists at all (the wiped-DB case after
// `docker compose down -v`). Without the label gate the sweeper would
// have to be conservative and only reap rows it could prove were ours,
// leaving wiped-DB orphans to leak forever.
//
// Returns an empty slice on any Docker error (sweeper treats that as
// "skip this round" — same contract as ListWorkspaceContainerIDPrefixes).
func (p *Provisioner) ListManagedContainerIDPrefixes(ctx context.Context) ([]string, error) {
	if p == nil || p.cli == nil {
		return nil, nil
	}
	containers, err := p.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", LabelManaged+"=true")),
	})
	if err != nil {
		return nil, err
	}
	prefixes := make([]string, 0, len(containers))
	for _, c := range containers {
		// Same name-strip dance as ListWorkspaceContainerIDPrefixes —
		// label filter is exact (not substring), so any false-positive
		// must be a non-ws-* container we accidentally labeled. Defence
		// against a future bug that stamps the label on something else.
		for _, name := range c.Names {
			n := strings.TrimPrefix(name, "/")
			if !strings.HasPrefix(n, containerNamePrefix) {
				continue
			}
			id := strings.TrimPrefix(n, containerNamePrefix)
			if id == "" {
				continue
			}
			prefixes = append(prefixes, id)
			break
		}
	}
	return prefixes, nil
}

// InternalURL returns the Docker-internal URL for a workspace container.
func InternalURL(workspaceID string) string {
	return fmt.Sprintf("http://%s:%s", ContainerName(workspaceID), DefaultPort)
}

// Start provisions and starts a workspace container.
func (p *Provisioner) Start(ctx context.Context, cfg WorkspaceConfig) (string, error) {
	name := ContainerName(cfg.WorkspaceID)
	configVolume := ConfigVolumeName(cfg.WorkspaceID)

	// Create named volume for configs (idempotent — no-op if already exists)
	_, err := p.cli.VolumeCreate(ctx, volume.CreateOptions{
		Name:   configVolume,
		Labels: managedLabels(),
	})
	if err != nil {
		return "", fmt.Errorf("failed to create config volume %s: %w", configVolume, err)
	}
	log.Printf("Provisioner: config volume %s ready", configVolume)

	env := buildContainerEnv(cfg)

	// Select image based on runtime (each adapter has its own pre-built image)
	image := DefaultImage
	if cfg.Runtime != "" {
		if img, ok := RuntimeImages[cfg.Runtime]; ok {
			image = img
		}
	}

	containerCfg := &container.Config{
		Image:  image,
		Env:    env,
		Labels: managedLabels(),
		ExposedPorts: nat.PortSet{
			nat.Port(DefaultPort + "/tcp"): {},
		},
	}

	// Host config with volume mounts. #65: workspace_access controls whether
	// a bind-mount is read-only (:ro) or read-write. Default "none" implies
	// isolated volume; "read_only"/"read_write" require WorkspacePath set
	// (validated at the handler layer before we get here).
	workspaceMount := buildWorkspaceMount(cfg)
	log.Printf("Provisioner: workspace mount = %q (access=%q)", workspaceMount, cfg.WorkspaceAccess)

	// Mount configs as read-write named volume (agent and Files API need to write)
	// Plugins are installed per-workspace into /configs/plugins/ via the platform API.
	// No global /plugins mount — each workspace owns its plugin set.
	configMount := fmt.Sprintf("%s:/configs", configVolume)
	binds := []string{
		configMount,
		workspaceMount,
	}

	// #12: Preserve Claude Code session directory across restarts.
	// The claude-code SDK stores conversations in /root/.claude/sessions/
	// and Postgres keeps current_session_id. Without a persistent volume,
	// restarts drop the session file and the SDK dies with
	// "No conversation found with session ID: <uuid>".
	//
	// Only mount for runtime=claude-code (other runtimes don't use the path).
	// Opt-out: ResetClaudeSession or env WORKSPACE_RESET_SESSION=1 → we
	// remove the existing volume before recreating it, so the agent
	// boots with a clean session dir.
	if cfg.Runtime == "claude-code" {
		claudeSessionsVolume := ClaudeSessionVolumeName(cfg.WorkspaceID)
		resetEnv, _ := strconv.ParseBool(cfg.EnvVars["WORKSPACE_RESET_SESSION"])
		if cfg.ResetClaudeSession || resetEnv {
			if rmErr := p.cli.VolumeRemove(ctx, claudeSessionsVolume, true); rmErr != nil {
				log.Printf("Provisioner: claude-sessions volume reset warning for %s: %v", claudeSessionsVolume, rmErr)
			} else {
				log.Printf("Provisioner: claude-sessions volume %s reset (fresh session)", claudeSessionsVolume)
			}
		}
		if _, cvErr := p.cli.VolumeCreate(ctx, volume.CreateOptions{Name: claudeSessionsVolume, Labels: managedLabels()}); cvErr != nil {
			return "", fmt.Errorf("failed to create claude-sessions volume %s: %w", claudeSessionsVolume, cvErr)
		}
		binds = append(binds, fmt.Sprintf("%s:/root/.claude/sessions", claudeSessionsVolume))
		log.Printf("Provisioner: claude-sessions volume %s mounted at /root/.claude/sessions", claudeSessionsVolume)
	}

	hostCfg := &container.HostConfig{
		Binds:         binds,
		RestartPolicy: container.RestartPolicy{Name: "unless-stopped"},
		PortBindings: nat.PortMap{
			nat.Port(DefaultPort + "/tcp"): []nat.PortBinding{
				{HostIP: "127.0.0.1", HostPort: ""}, // Ephemeral host port
			},
		},
	}

	// Apply tier-based container configuration
	ApplyTierConfig(hostCfg, cfg, configMount, name)

	// Network config — join molecule-monorepo-net with container name as alias
	networkCfg := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			DefaultNetwork: {
				Aliases: []string{name},
			},
		},
	}

	// Ensure no stale container exists with the same name (race with restart policy)
	_ = p.cli.ContainerRemove(ctx, name, container.RemoveOptions{Force: true})

	// Resolve the target image platform once so the pull and the
	// container-create use the same value. On an Apple Silicon dev
	// laptop the GHCR workspace-template-* images only ship a
	// linux/amd64 manifest today; without an explicit platform the
	// daemon asks for linux/arm64/v8 and ImagePull returns
	// "no matching manifest for linux/arm64/v8 in the manifest list
	// entries". Forcing linux/amd64 lets Docker Desktop run them
	// under QEMU emulation (slow but functional — unblocks local
	// dev + Canvas smoke-testing on M-series Macs). See issue #1875.
	imgPlatformStr := defaultImagePlatform()
	imgPlatform := parseOCIPlatform(imgPlatformStr)

	// Log image resolution for debugging stale-image issues, and pull from
	// GHCR on miss so tenant hosts don't need a pre-build step anymore.
	// The pull is best-effort: if it fails (network, auth, rate limit) the
	// subsequent ContainerCreate still surfaces the actionable error below.
	imgInspect, _, imgErr := p.cli.ImageInspectWithRaw(ctx, image)
	if imgErr == nil {
		log.Printf("Provisioner: creating %s from image %s (ID: %s, created: %s)",
			name, image, imgInspect.ID[:19], imgInspect.Created[:19])
	} else {
		if imgPlatformStr != "" {
			log.Printf("Provisioner: image %s not present locally (%v) — attempting pull (platform=%s)", image, imgErr, imgPlatformStr)
		} else {
			log.Printf("Provisioner: image %s not present locally (%v) — attempting pull", image, imgErr)
		}
		if perr := pullImageAndDrain(ctx, p.cli, image, imgPlatformStr); perr != nil {
			log.Printf("Provisioner: image pull for %s failed: %v (falling through to create)", image, perr)
		} else {
			log.Printf("Provisioner: pulled %s", image)
		}
	}

	// Create and start container. If the image still isn't available,
	// Docker returns a generic "No such image" error that's opaque to
	// operators — wrap it with the resolved tag and the exact pull
	// command so last_sample_error surfaces something actionable. Issue #117.
	resp, err := p.cli.ContainerCreate(ctx, containerCfg, hostCfg, networkCfg, imgPlatform, name)
	if err != nil {
		if isImageNotFoundErr(err) {
			return "", fmt.Errorf(
				"docker image %q not found after pull attempt — verify GHCR visibility for %s and that the tenant has internet access (underlying error: %w)",
				image, runtimeTagFromImage(image), err,
			)
		}
		return "", fmt.Errorf("failed to create container: %w", err)
	}

	if err := p.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		// Clean up created container on start failure
		_ = p.cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		return "", fmt.Errorf("failed to start container: %w", err)
	}

	// Verify the started container uses the expected image
	if startedInfo, siErr := p.cli.ContainerInspect(ctx, resp.ID); siErr == nil {
		log.Printf("Provisioner: started container %s (image: %s)", name, startedInfo.Image[:19])
	}

	// Volume ownership is fixed by the entrypoint (starts as root, chowns
	// /configs and /workspace, then drops to agent via gosu). No per-start
	// chown needed here.

	// Copy template files into /configs if TemplatePath is set
	if cfg.TemplatePath != "" {
		if err := p.CopyTemplateToContainer(ctx, resp.ID, cfg.TemplatePath); err != nil {
			log.Printf("Provisioner: warning — failed to copy template to container %s: %v", name, err)
		}
	}

	// Write generated config files into /configs if ConfigFiles is set
	if len(cfg.ConfigFiles) > 0 {
		if err := p.WriteFilesToContainer(ctx, resp.ID, cfg.ConfigFiles); err != nil {
			log.Printf("Provisioner: warning — failed to write config files to container %s: %v", name, err)
		}
	}

	// Resolve the host-mapped port. Retry inspect up to 3 times if Docker hasn't
	// bound the ephemeral port yet (rare race under heavy load).
	hostURL := InternalURL(cfg.WorkspaceID) // fallback to Docker-internal
	for attempt := 0; attempt < 3; attempt++ {
		info, inspectErr := p.cli.ContainerInspect(ctx, resp.ID)
		if inspectErr != nil {
			break
		}
		portBindings := info.NetworkSettings.Ports[nat.Port(DefaultPort+"/tcp")]
		if len(portBindings) > 0 {
			hostPort := portBindings[0].HostPort
			hostIP := portBindings[0].HostIP
			if hostIP == "" {
				hostIP = "127.0.0.1"
			}
			hostURL = fmt.Sprintf("http://%s:%s", hostIP, hostPort)
			break
		}
		if attempt < 2 {
			time.Sleep(500 * time.Millisecond) // wait for Docker to bind the port
		}
	}

	log.Printf("Provisioner: started container %s for workspace %s at %s (internal: %s)", name, cfg.WorkspaceID, hostURL, InternalURL(cfg.WorkspaceID))
	return hostURL, nil
}

// buildWorkspaceMount returns the Docker volume spec for /workspace (#65).
//
// Selection matrix:
//
//   cfg.WorkspacePath | cfg.WorkspaceAccess     | mount
//   ------------------+-------------------------+--------------------------------
//   ""                | "" / "none"             | <named-volume>:/workspace  (isolated, current default)
//   "<host-dir>"      | "" / "read_write"       | <host-dir>:/workspace      (current PM behaviour)
//   "<host-dir>"      | "read_only"             | <host-dir>:/workspace:ro   (research agents get read access without write risk)
//   ""                | "read_only"/"read_write"| <named-volume>:/workspace  (degraded — access requires a mount; validated at handler layer)
//
// Kept pure + side-effect-free so it's unit-testable.
func buildWorkspaceMount(cfg WorkspaceConfig) string {
	// Named volume when no host path is configured.
	if cfg.WorkspacePath == "" {
		volumeName := fmt.Sprintf("ws-%s-workspace", cfg.WorkspaceID)
		return fmt.Sprintf("%s:/workspace", volumeName)
	}
	// Host bind mount. Append :ro for read-only mode; otherwise default
	// (implicit read-write). "none" explicitly opts out of the mount
	// even when a path is set.
	if cfg.WorkspaceAccess == WorkspaceAccessNone {
		volumeName := fmt.Sprintf("ws-%s-workspace", cfg.WorkspaceID)
		return fmt.Sprintf("%s:/workspace", volumeName)
	}
	if cfg.WorkspaceAccess == WorkspaceAccessReadOnly {
		return fmt.Sprintf("%s:/workspace:ro", cfg.WorkspacePath)
	}
	return fmt.Sprintf("%s:/workspace", cfg.WorkspacePath)
}

// ValidateWorkspaceAccess checks that a (access, path) pair is consistent.
// Returns a clear error on mismatch so the handler layer can reject bad
// payloads with a 400 before provisioning.
//
//   - read_only / read_write with empty path → error (needs a host dir)
//   - unknown access value                   → error
//   - none / ""                              → always valid
func ValidateWorkspaceAccess(access, workspacePath string) error {
	switch access {
	case "", WorkspaceAccessNone:
		return nil
	case WorkspaceAccessReadOnly, WorkspaceAccessReadWrite:
		if workspacePath == "" {
			return fmt.Errorf("workspace_access=%q requires workspace_dir to be set", access)
		}
		return nil
	default:
		return fmt.Errorf("workspace_access=%q — must be 'none', 'read_only', or 'read_write'", access)
	}
}

// buildContainerEnv assembles the initial environment variables injected
// into every workspace container.
//
//   - PLATFORM_URL: canonical env var the workspace runtime reads for
//     heartbeat / register / A2A proxy.
//   - MOLECULE_URL: canonical env var the Molecule AI MCP client reads
//     (mcp-server/src/index.ts). Injecting it at provision time so
//     mcp__molecule__* tools called FROM inside the agent container
//     reach the host platform instead of localhost:8080 (which is the
//     container itself). Fixes #67.
//
// Extracted from Start() so it's unit-testable without standing up a
// Docker daemon.
func buildContainerEnv(cfg WorkspaceConfig) []string {
	env := []string{
		fmt.Sprintf("WORKSPACE_ID=%s", cfg.WorkspaceID),
		"WORKSPACE_CONFIG_PATH=/configs",
		fmt.Sprintf("PLATFORM_URL=%s", cfg.PlatformURL),
		fmt.Sprintf("MOLECULE_URL=%s", cfg.PlatformURL),
		fmt.Sprintf("TIER=%d", cfg.Tier),
		"PLUGINS_DIR=/plugins",
		// PYTHONPATH=/app makes ADAPTER_MODULE imports resolve regardless of
		// runtime cwd. Standalone workspace-template repos COPY adapter.py to
		// /app and set ENV ADAPTER_MODULE=adapter, but molecule-runtime is a
		// pip console_script entry point so cwd isn't on sys.path automatically.
		// Setting PYTHONPATH from the provisioner fixes every adapter image
		// (claude-code, hermes, langgraph, …) without needing to PR each
		// standalone template repo. Per-template ENV in the Dockerfile can
		// still override (Dockerfile ENV is overridden by docker -e at runtime).
		"PYTHONPATH=/app",
	}
	if cfg.AwarenessNamespace != "" && cfg.AwarenessURL != "" {
		env = append(env, fmt.Sprintf("AWARENESS_NAMESPACE=%s", cfg.AwarenessNamespace))
		env = append(env, fmt.Sprintf("AWARENESS_URL=%s", cfg.AwarenessURL))
	}
	for k, v := range cfg.EnvVars {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}
	return env
}

// Per-tier resource defaults. Configurable via TIERn_MEMORY_MB and
// TIERn_CPU_SHARES env vars (n in {2,3,4}). CPU shares follow the convention
// 1024 shares == 1 CPU; internally translated to NanoCPUs for a hard cap.
//
// Defaults reflect the tier sizing agreed in issue #14:
//   - T2: 512 MiB,  1024 shares (1 CPU)  — unchanged historical default
//   - T3: 2048 MiB, 2048 shares (2 CPU)  — new cap (previously uncapped)
//   - T4: 4096 MiB, 4096 shares (4 CPU)  — new cap (previously uncapped)
const (
	defaultTier2MemoryMB  = 512
	defaultTier2CPUShares = 1024
	defaultTier3MemoryMB  = 2048
	defaultTier3CPUShares = 2048
	defaultTier4MemoryMB  = 4096
	defaultTier4CPUShares = 4096
)

// getTierMemoryMB returns the memory cap (MiB) for the given tier, reading
// TIERn_MEMORY_MB env var with fallback to the hardcoded default. Returns 0
// for tiers with no cap (e.g. tier 1).
func getTierMemoryMB(tier int) int64 {
	var def int64
	switch tier {
	case 2:
		def = defaultTier2MemoryMB
	case 3:
		def = defaultTier3MemoryMB
	case 4:
		def = defaultTier4MemoryMB
	default:
		return 0
	}
	if v := os.Getenv(fmt.Sprintf("TIER%d_MEMORY_MB", tier)); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// getTierCPUShares returns the CPU allocation (shares, where 1024 == 1 CPU)
// for the given tier, reading TIERn_CPU_SHARES env var with fallback to the
// hardcoded default. Returns 0 for tiers with no cap.
func getTierCPUShares(tier int) int64 {
	var def int64
	switch tier {
	case 2:
		def = defaultTier2CPUShares
	case 3:
		def = defaultTier3CPUShares
	case 4:
		def = defaultTier4CPUShares
	default:
		return 0
	}
	if v := os.Getenv(fmt.Sprintf("TIER%d_CPU_SHARES", tier)); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// applyTierResources writes Memory + NanoCPUs to hostCfg from the tier's
// configured limits (env override or default). Returns the resolved values
// for logging.
func applyTierResources(hostCfg *container.HostConfig, tier int) (memMB, cpuShares int64) {
	memMB = getTierMemoryMB(tier)
	cpuShares = getTierCPUShares(tier)
	if memMB > 0 {
		hostCfg.Resources.Memory = memMB * 1024 * 1024
	}
	if cpuShares > 0 {
		// shares -> NanoCPUs: 1024 shares == 1 CPU == 1e9 NanoCPUs
		hostCfg.Resources.NanoCPUs = (cpuShares * 1_000_000_000) / 1024
	}
	return memMB, cpuShares
}

// ApplyTierConfig configures a HostConfig based on the workspace tier.
// Extracted from Start() so it can be tested independently.
//
//   - Tier 1 (Sandboxed):  readonly rootfs, tmpfs /tmp, strip /workspace mount
//   - Tier 2 (Standard):   resource limits (default 512 MiB, 1 CPU)
//   - Tier 3 (Privileged): privileged + host PID, Docker network, capped resources
//   - Tier 4 (Full access): privileged, host PID, host network, Docker socket, capped resources
//
// Per-tier memory/CPU caps are overridable via TIERn_MEMORY_MB /
// TIERn_CPU_SHARES env vars (n in {2,3,4}).
//
// Unknown/zero tiers default to Tier 2 behavior (safe resource-limited container).
func ApplyTierConfig(hostCfg *container.HostConfig, cfg WorkspaceConfig, configMount, name string) {
	switch cfg.Tier {
	case 1:
		// Sandboxed: strip /workspace mount, keep only config (plugins are in /configs/plugins/)
		tier1Binds := []string{configMount}
		hostCfg.Binds = tier1Binds
		// Readonly root filesystem with tmpfs for /tmp (agent needs scratch space)
		hostCfg.ReadonlyRootfs = true
		hostCfg.Tmpfs = map[string]string{
			"/tmp": "rw,noexec,nosuid,size=64m",
		}
		log.Printf("Provisioner: T1 sandboxed mode for %s (readonly, no /workspace)", name)

	case 3:
		// Privileged access: privileged mode + host PID.
		// Keep the Docker network (not host network) so containers can still reach
		// each other by name. Host networking conflicts with Docker networks and
		// causes port collisions when multiple T3 containers run simultaneously.
		hostCfg.Privileged = true
		hostCfg.PidMode = "host"
		memMB, shares := applyTierResources(hostCfg, 3)
		log.Printf("Provisioner: T3 privileged mode for %s (privileged, host PID, %dm memory, %d CPU shares)", name, memMB, shares)

	case 4:
		// Full host access: everything from T3 + host network + Docker socket + all capabilities.
		// Use for workspaces that need to manage other containers or access host services directly.
		hostCfg.Privileged = true
		hostCfg.PidMode = "host"
		hostCfg.NetworkMode = "host"
		// Mount Docker socket so workspace can manage containers
		hostCfg.Binds = append(hostCfg.Binds, "/var/run/docker.sock:/var/run/docker.sock")
		memMB, shares := applyTierResources(hostCfg, 4)
		log.Printf("Provisioner: T4 full-host mode for %s (privileged, host PID, host network, docker socket, %dm memory, %d CPU shares)", name, memMB, shares)

	default:
		// Tier 2 (Standard) and unknown tiers: normal container with resource limits.
		// This is the safe default — no privileged access, reasonable resource caps.
		memMB, shares := applyTierResources(hostCfg, 2)
		log.Printf("Provisioner: T2 standard mode for %s (%dm memory, %d CPU shares)", name, memMB, shares)
	}
}

// CopyTemplateToContainer copies files from a host directory into /configs in the container.
func (p *Provisioner) CopyTemplateToContainer(ctx context.Context, containerID, templatePath string) error {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	err := filepath.Walk(templatePath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(templatePath, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}

		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = rel

		if err := tw.WriteHeader(header); err != nil {
			return err
		}

		if !info.IsDir() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			// Strip CRLF from shell scripts and Python files. Windows
			// git checkout introduces \r\n even with .gitattributes eol=lf;
			// Linux containers choke on \r in shebangs and Python path args.
			// This is the single fix point — every file that enters a
			// container passes through CopyTemplateToContainer.
			ext := filepath.Ext(path)
			if ext == ".sh" || ext == ".py" || ext == ".md" {
				data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
			}
			header.Size = int64(len(data))
			if _, err := tw.Write(data); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to create tar from %s: %w", templatePath, err)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("failed to close tar writer: %w", err)
	}

	return p.cli.CopyToContainer(ctx, containerID, "/configs", &buf, container.CopyToContainerOptions{})
}

// WriteFilesToContainer writes in-memory files into /configs in the container.
func (p *Provisioner) WriteFilesToContainer(ctx context.Context, containerID string, files map[string][]byte) error {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	createdDirs := map[string]bool{}
	for name, data := range files {
		// Create parent directories in tar (deduplicated)
		dir := filepath.Dir(name)
		if dir != "." && !createdDirs[dir] {
			if err := tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeDir,
				Name:     dir + "/",
				Mode:     0755,
			}); err != nil {
				return fmt.Errorf("failed to write tar dir header for %s: %w", dir, err)
			}
			createdDirs[dir] = true
		}

		header := &tar.Header{
			Name: name,
			Mode: 0644,
			Size: int64(len(data)),
		}
		if err := tw.WriteHeader(header); err != nil {
			return fmt.Errorf("failed to write tar header for %s: %w", name, err)
		}
		if _, err := tw.Write(data); err != nil {
			return fmt.Errorf("failed to write tar data for %s: %w", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("failed to close tar writer: %w", err)
	}

	return p.cli.CopyToContainer(ctx, containerID, "/configs", &buf, container.CopyToContainerOptions{})
}

// CopyToContainer exposes CopyToContainer from the Docker client for use by other packages.
func (p *Provisioner) CopyToContainer(ctx context.Context, containerID, dstPath string, content io.Reader) error {
	return p.cli.CopyToContainer(ctx, containerID, dstPath, content, container.CopyToContainerOptions{})
}

// ExecRead runs "cat <filePath>" in an existing container and returns the output.
// Used to read config files from a running container before stopping it.
func (p *Provisioner) ExecRead(ctx context.Context, containerName, filePath string) ([]byte, error) {
	exec, err := p.cli.ContainerExecCreate(ctx, containerName, container.ExecOptions{
		Cmd:          []string{"cat", filePath},
		AttachStdout: true,
	})
	if err != nil {
		return nil, err
	}
	attach, err := p.cli.ContainerExecAttach(ctx, exec.ID, container.ExecAttachOptions{})
	if err != nil {
		return nil, err
	}
	defer attach.Close()
	data, err := io.ReadAll(attach.Reader)
	if err != nil {
		return nil, err
	}
	// Docker multiplexed stream: strip 8-byte headers
	var clean []byte
	for len(data) >= 8 {
		size := int(data[4])<<24 | int(data[5])<<16 | int(data[6])<<8 | int(data[7])
		if 8+size > len(data) {
			break
		}
		clean = append(clean, data[8:8+size]...)
		data = data[8+size:]
	}
	return clean, nil
}

// ReadFromVolume reads a file from a Docker named volume using a throwaway container.
// Used as a fallback when ExecRead fails (container already stopped).
func (p *Provisioner) ReadFromVolume(ctx context.Context, volumeName, filePath string) ([]byte, error) {
	resp, err := p.cli.ContainerCreate(ctx, &container.Config{
		Image: "alpine",
		Cmd:   []string{"cat", "/vol/" + filePath},
	}, &container.HostConfig{
		Binds: []string{volumeName + ":/vol:ro"},
	}, nil, nil, "")
	if err != nil {
		return nil, err
	}
	defer p.cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})

	if err := p.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return nil, err
	}
	waitCh, errCh := p.cli.ContainerWait(ctx, resp.ID, container.WaitConditionNotRunning)
	select {
	case <-waitCh:
	case err := <-errCh:
		if err != nil {
			return nil, err
		}
	}
	reader, err := p.cli.ContainerLogs(ctx, resp.ID, container.LogsOptions{ShowStdout: true})
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	// Strip Docker multiplexed stream headers
	var clean []byte
	for len(data) >= 8 {
		size := int(data[4])<<24 | int(data[5])<<16 | int(data[6])<<8 | int(data[7])
		if 8+size > len(data) {
			break
		}
		clean = append(clean, data[8:8+size]...)
		data = data[8+size:]
	}
	return clean, nil
}

// WriteAuthTokenToVolume writes the workspace auth token into the config volume
// BEFORE the container starts, eliminating the token-injection race window where
// a restarted container could read a stale token from /configs/.auth_token before
// WriteFilesToContainer writes the new one. Issue #1877.
//
// Uses a throwaway alpine container to write directly to the named volume,
// bypassing the container lifecycle entirely.
func (p *Provisioner) WriteAuthTokenToVolume(ctx context.Context, workspaceID, token string) error {
	volName := ConfigVolumeName(workspaceID)
	resp, err := p.cli.ContainerCreate(ctx, &container.Config{
		Image: "alpine",
		Cmd:   []string{"sh", "-c", "mkdir -p /vol && printf '%s' $TOKEN > /vol/.auth_token && chmod 0600 /vol/.auth_token"},
		Env:   []string{"TOKEN=" + token},
	}, &container.HostConfig{
		Binds: []string{volName + ":/vol"},
	}, nil, nil, "")
	if err != nil {
		return fmt.Errorf("failed to create token-write container: %w", err)
	}
	defer p.cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
	if err := p.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("failed to start token-write container: %w", err)
	}
	waitCh, errCh := p.cli.ContainerWait(ctx, resp.ID, container.WaitConditionNotRunning)
	select {
	case <-waitCh:
	case writeErr := <-errCh:
		if writeErr != nil {
			return fmt.Errorf("token-write container exited with error: %w", writeErr)
		}
	}
	log.Printf("Provisioner: wrote auth token to volume %s/.auth_token", volName)
	return nil
}

// execInContainer runs a command inside a running container as root.
// Best-effort: logs errors but does not fail the caller.
func (p *Provisioner) execInContainer(ctx context.Context, containerID string, cmd []string) {
	execCfg := container.ExecOptions{Cmd: cmd, User: "root"}
	execID, err := p.cli.ContainerExecCreate(ctx, containerID, execCfg)
	if err != nil {
		log.Printf("Provisioner: exec create failed: %v", err)
		return
	}
	if err := p.cli.ContainerExecStart(ctx, execID.ID, container.ExecStartOptions{}); err != nil {
		log.Printf("Provisioner: exec start failed: %v", err)
	}
}

// RemoveVolume removes the config volume for a workspace.
// Also removes the claude-sessions volume (best-effort, may not exist
// for non claude-code runtimes). Issue #12.
func (p *Provisioner) RemoveVolume(ctx context.Context, workspaceID string) error {
	volName := ConfigVolumeName(workspaceID)
	if err := p.cli.VolumeRemove(ctx, volName, true); err != nil {
		return fmt.Errorf("failed to remove volume %s: %w", volName, err)
	}
	log.Printf("Provisioner: removed config volume %s", volName)
	csName := ClaudeSessionVolumeName(workspaceID)
	if rmErr := p.cli.VolumeRemove(ctx, csName, true); rmErr != nil {
		log.Printf("Provisioner: claude-sessions volume cleanup warning for %s: %v", csName, rmErr)
	} else {
		log.Printf("Provisioner: removed claude-sessions volume %s", csName)
	}
	return nil
}

// Stop stops and removes a workspace container.
//
// Uses force-remove FIRST to avoid a race with Docker's `unless-stopped`
// restart policy: if we ContainerStop first, the restart policy can
// respawn the container before ContainerRemove runs, leaving a zombie
// that re-registers via heartbeat after deletion.
//
// Returns nil on success AND on "container does not exist" (the cleanup
// goal is achieved either way). Returns the underlying Docker error
// only when the daemon actually failed to remove a live container —
// callers that follow Stop with RemoveVolume MUST check the return
// and skip volume removal on a real error, otherwise the volume
// removal will fail with "volume in use" because the container is
// still alive.
func (p *Provisioner) Stop(ctx context.Context, workspaceID string) error {
	if p == nil || p.cli == nil {
		return ErrNoBackend
	}
	name := ContainerName(workspaceID)

	// Force-remove kills and removes in one atomic operation, bypassing
	// the restart policy entirely.
	err := p.cli.ContainerRemove(ctx, name, container.RemoveOptions{Force: true})
	if err == nil {
		log.Printf("Provisioner: stopped and removed container %s", name)
		return nil
	}
	if isContainerNotFound(err) {
		// Container was already gone — the post-condition we want is
		// satisfied. Don't surface as an error.
		log.Printf("Provisioner: container %s already gone (no-op)", name)
		return nil
	}
	// Real failure: daemon timeout, socket EOF, ctx cancellation, etc.
	// Caller (workspace_crud.stopAndRemove, orphan_sweeper.sweepOnce)
	// must propagate this so they can skip the follow-up RemoveVolume.
	log.Printf("Provisioner: force-remove failed for %s: %v", name, err)
	return fmt.Errorf("force-remove %s: %w", name, err)
}

// IsRunning checks if a workspace container is currently running.
//
// Conservative on transient Docker errors: returns (true, err) for any
// inspect failure OTHER than NotFound. Rationale: the only caller that
// acts destructively on `running=false` is a2a_proxy.maybeMarkContainerDead,
// which tears down + re-provisions the workspace. A Docker daemon hiccup
// (timeout, EOF on the daemon socket, context deadline) is NOT evidence
// that the container died — it's evidence the daemon is momentarily busy.
// The old behaviour collapsed all errors into "container doesn't exist",
// which triggered a restart cascade on 2026-04-16 when 6 containers
// received simultaneous A2A forward failures during a batch delegation;
// the followup reactive IsRunning calls all hit the daemon under load,
// timed out, and flipped every container to "dead" in parallel.
//
// NotFound (container legitimately deleted) is the only case where
// running=false is safe to act on — every other error path stays alive
// so a real crash still surfaces via exec heartbeat or TTL, both of which
// have narrower false-positive windows than daemon-inspect RPC.
func (p *Provisioner) IsRunning(ctx context.Context, workspaceID string) (bool, error) {
	if p == nil || p.cli == nil {
		return false, ErrNoBackend
	}
	name := ContainerName(workspaceID)
	info, err := p.cli.ContainerInspect(ctx, name)
	if err != nil {
		if isContainerNotFound(err) {
			return false, nil
		}
		// Transient daemon error: caller treats !running as dead + restarts.
		// Returning true + the underlying error preserves the error for
		// metrics/logging without triggering the destructive path.
		return true, err
	}
	return info.State.Running, nil
}

// isContainerNotFound returns true when the Docker client indicates the
// named container genuinely does not exist, versus a transient daemon
// error (timeout, socket EOF, context cancellation).
//
// docker/docker v28 uses multiple distinct NotFound shapes depending on
// transport:
//   - the typed errdefs.ErrNotFound
//   - a wrapped error whose message contains "No such container"
//
// Rather than import errdefs (which would add a transitive dep), we
// match on the error string. String-matching is the exact approach the
// Docker CLI itself uses internally — see the "No such container" check
// in docker/cli — and is stable across daemon versions.
func isContainerNotFound(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "No such container") ||
		strings.Contains(s, "not found")
}

// DockerClient returns the underlying Docker client for sharing with other handlers.
func (p *Provisioner) DockerClient() *client.Client {
	return p.cli
}

// Close cleans up the Docker client.
func (p *Provisioner) Close() error {
	return p.cli.Close()
}

// ValidateConfigSource is a pure check that ensures at least one static
// source of /configs/config.yaml is available before a container starts.
//
// Inputs mirror the fields on WorkspaceConfig:
//   - templatePath: host dir expected to contain config.yaml (copied into /configs)
//   - configFiles:  in-memory files written into /configs at start time
//
// Returns nil if either source will place config.yaml into /configs.
// When both sources are empty, returns ErrNoConfigSource so callers can
// fall through to a volume probe (VolumeHasFile) before giving up.
//
// Used by the platform's provision flow to catch the rogue-restart-loop
// case (#17): a workspace whose config volume was wiped and whose
// auto-restart path passes empty template+configFiles would otherwise
// boot into a FileNotFoundError crash loop under Docker's
// `unless-stopped` restart policy.
func ValidateConfigSource(templatePath string, configFiles map[string][]byte) error {
	if templatePath != "" {
		// Stat the template's config.yaml; an empty/stale template dir
		// without config.yaml is as broken as no template at all.
		info, err := os.Stat(filepath.Join(templatePath, "config.yaml"))
		if err == nil && !info.IsDir() {
			return nil
		}
	}
	if configFiles != nil {
		if data, ok := configFiles["config.yaml"]; ok && len(data) > 0 {
			return nil
		}
	}
	return ErrNoConfigSource
}

// ErrNoConfigSource is returned by ValidateConfigSource when neither the
// template path nor the in-memory config files supply a config.yaml.
var ErrNoConfigSource = fmt.Errorf("no config.yaml source: template path missing config.yaml and configFiles empty")

// VolumeHasFile returns true if the named config volume for a workspace
// already contains the given file path (relative to /configs). Used by
// the auto-restart path to confirm a previously-provisioned volume is
// still populated before reusing it — if the volume was wiped, we must
// regenerate config or fail cleanly rather than loop on FileNotFoundError.
//
// Implementation: run a throwaway alpine `test -f` container bound to the
// volume read-only. Returns (false, nil) if the file is absent and
// (false, err) only on Docker-level failures.
func (p *Provisioner) VolumeHasFile(ctx context.Context, workspaceID, relPath string) (bool, error) {
	volName := ConfigVolumeName(workspaceID)
	// Confirm the volume exists first — Docker auto-creates on bind otherwise.
	if _, err := p.cli.VolumeInspect(ctx, volName); err != nil {
		return false, nil
	}
	resp, err := p.cli.ContainerCreate(ctx, &container.Config{
		Image: "alpine",
		Cmd:   []string{"test", "-f", "/vol/" + relPath},
	}, &container.HostConfig{
		Binds: []string{volName + ":/vol:ro"},
	}, nil, nil, "")
	if err != nil {
		return false, err
	}
	defer p.cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})

	if err := p.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return false, err
	}
	waitCh, errCh := p.cli.ContainerWait(ctx, resp.ID, container.WaitConditionNotRunning)
	select {
	case w := <-waitCh:
		return w.StatusCode == 0, nil
	case err := <-errCh:
		return false, err
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// isImageNotFoundErr classifies a Docker client error as "image not
// available locally." The daemon wraps this message in a generic
// SystemError type without exposing a typed sentinel, so we fall back
// to substring match on the known messages emitted by moby. Used by
// Start() to rewrite opaque ContainerCreate failures into actionable
// "run build-all.sh" hints. Issue #117.
func isImageNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "no such image") ||
		strings.Contains(m, "not found") && strings.Contains(m, "image")
}

// runtimeTagFromImage extracts the runtime name from a workspace-template
// image reference for use in user-facing error hints. Handles both the
// legacy local tag (`workspace-template:<runtime>`) and the current GHCR
// form (`ghcr.io/molecule-ai/workspace-template-<runtime>:<tag>`). Falls
// back to the full image string if the shape is unrecognised.
func runtimeTagFromImage(image string) string {
	const legacyPrefix = "workspace-template:"
	if strings.HasPrefix(image, legacyPrefix) {
		return image[len(legacyPrefix):]
	}
	// GHCR form: strip everything before and including "workspace-template-",
	// then drop the :<tag> suffix.
	const ghcrInfix = "workspace-template-"
	if i := strings.Index(image, ghcrInfix); i >= 0 {
		rest := image[i+len(ghcrInfix):]
		if j := strings.Index(rest, ":"); j >= 0 {
			rest = rest[:j]
		}
		return rest
	}
	if i := strings.LastIndex(image, ":"); i >= 0 && i < len(image)-1 {
		return image[i+1:]
	}
	return image
}

// dockerImageClient is the subset of the Docker client API used by
// pullImageAndDrain. Declared as an interface so tests can inject a
// fake without spinning up a daemon.
type dockerImageClient interface {
	ImagePull(ctx context.Context, ref string, opts dockerimage.PullOptions) (io.ReadCloser, error)
}

// pullImageAndDrain pulls the given image from its registry and drains
// the progress stream to completion. The Docker engine pull API is
// asynchronous — the returned ReadCloser MUST be fully consumed for the
// pull to finish; returning early leaves the daemon mid-pull. We
// discard the progress payload because operators read container logs
// for boot diagnostics, not pull chatter.
//
// `platform` is "os/arch" (e.g. "linux/amd64") when the host needs to
// pull a non-native manifest, or "" to let the daemon pick the default
// for its arch. See defaultImagePlatform for when that matters.
func pullImageAndDrain(ctx context.Context, cli dockerImageClient, ref, platform string) error {
	rc, err := cli.ImagePull(ctx, ref, dockerimage.PullOptions{Platform: platform})
	if err != nil {
		return fmt.Errorf("ImagePull: %w", err)
	}
	defer rc.Close()
	if _, err := io.Copy(io.Discard, rc); err != nil {
		return fmt.Errorf("drain pull stream: %w", err)
	}
	return nil
}

// defaultImagePlatform picks the Docker image platform string used for
// `ImagePull` + `ContainerCreate` on the workspace-template-* images.
//
// Empty result means "use the daemon default" — the common case on
// linux/amd64 hosts (CI, SaaS EC2, Linux dev machines). On Apple Silicon
// the GHCR workspace-template-* images ship a single linux/amd64
// manifest today, so the daemon's native linux/arm64/v8 request misses
// with "no matching manifest". Forcing linux/amd64 pulls the amd64
// manifest and lets Docker Desktop run it under QEMU emulation. Slow
// (2–5× native) but functional — unblocks local dev on M-series Macs.
//
// Override via MOLECULE_IMAGE_PLATFORM — set to the empty string to
// disable the auto-force, or to a specific value ("linux/amd64",
// "linux/arm64") to pin. SaaS production should leave this unset.
//
// Tracked in issue #1875; remove this fallback once the template repos
// publish multi-arch manifests.
// DefaultImagePlatform is the exported alias used by the admin
// workspace-images handler so its ImagePull picks the same platform as
// the provisioner's. Avoids duplicating the Apple-Silicon-needs-amd64
// logic and keeps both call sites in sync if Docker manifest support
// changes (e.g., when the templates start shipping multi-arch).
func DefaultImagePlatform() string { return defaultImagePlatform() }

func defaultImagePlatform() string {
	if v, ok := os.LookupEnv("MOLECULE_IMAGE_PLATFORM"); ok {
		return v
	}
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		return "linux/amd64"
	}
	return ""
}

// parseOCIPlatform turns "linux/amd64" into the *ocispec.Platform shape
// `ContainerCreate`'s platform argument expects. "" returns nil, which
// is exactly how the Docker SDK signals "no preference".
func parseOCIPlatform(s string) *ocispec.Platform {
	if s == "" {
		return nil
	}
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil
	}
	return &ocispec.Platform{OS: parts[0], Architecture: parts[1]}
}
