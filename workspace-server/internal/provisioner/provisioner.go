// Package provisioner manages Docker container lifecycle for workspace agents.
package provisioner

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
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

// ErrUnresolvableRuntime is returned by selectImage when a workspace
// names a runtime that has no resolvable image (not in RuntimeImages and
// no operator-pinned cfg.Image). RFC internal#483 + security review 4269:
// previously such a request silently fell through to DefaultImage — a user
// asking for a removed runtime would get a different container
// with no signal. The CTO standing directive
// (feedback_platform_must_hardgate_base_contract) is fail-closed: a
// named-but-unresolvable runtime must reject with a structured,
// runtime-naming error so the existing provision-failed notify/log path
// surfaces it, NOT silently degrade. The genuinely-unspecified (empty)
// runtime is still a distinct, legitimate path that keeps DefaultImage.
var ErrUnresolvableRuntime = errors.New("provisioner: requested runtime has no resolvable image")

// RuntimeImages maps runtime names to their Docker image refs.
// Each standalone template repo publishes its image via the reusable
// publish-template-image workflow in molecule-ci on every main merge.
// The provisioner pulls these on demand (see ensureImageLocal) — no
// pre-build step on the tenant host.
//
// The registry prefix is determined by RegistryPrefix() in registry.go;
// defaults to ghcr.io/molecule-ai (upstream OSS) and is overridden via the
// MOLECULE_IMAGE_REGISTRY env var in production tenants that mirror to
// AWS ECR or another registry. The map is computed at package init and
// captures whatever prefix was active then.
//
// Legacy local-build path (`docker build -t workspace-template:<runtime>`
// via scripts/build-images.sh) is still supported for development:
// when a bare `workspace-template:<runtime>` image is present locally,
// Docker's image resolver matches it before any pull is attempted. Set
// the env var WORKSPACE_IMAGE_LOCAL_OVERRIDE=1 (enforced by callers) to
// short-circuit pulls entirely if needed.
var RuntimeImages = computeRuntimeImages()

// DefaultImage is the fallback workspace Docker image. Computed via RegistryPrefix() so the prefix
// override applies to the fallback path too.
//
// NOTE: Every runtime MUST have an entry in knownRuntimes (registry.go).
// If a runtime is missing, it falls back to DefaultImage which may have
// wrong deps. Add new runtimes to knownRuntimes AND create the standalone
// template repo.
var DefaultImage = RuntimeImage(defaultRuntime)

const (
	// DefaultNetwork is the Docker network workspaces join.
	DefaultNetwork = "molecule-core-net"

	// DefaultPort is the port the A2A server listens on inside the container.
	DefaultPort = "8000"

	// ProvisionTimeout is how long to wait for first heartbeat before marking as failed.
	ProvisionTimeout = 3 * time.Minute

	// WorkspaceKindPlatform mirrors models.KindPlatform — the org-level
	// concierge / platform agent. Duplicated here (rather than importing the
	// models package from the lower-level provisioner) so the image-resolution
	// branch can compare WorkspaceConfig.Kind without pulling a higher-level
	// dependency into the provisioner. Kept in sync with models.KindPlatform
	// (a provisioner test asserts the two agree).
	WorkspaceKindPlatform = "platform"

	// alpineImage is the digest-pinned alpine image used for throwaway
	// containers that read/write volumes or migrate data.  Pinning prevents
	// supply-chain drift / compromised-tag attacks (core#2545).
	// Matches workspace-server/Dockerfile FROM alpine:3.20@sha256:...
	alpineImageDefault = "alpine:3.20@sha256:c64c687cbea9300178b30c95835354e34c4e4febc4badfe27102879de0483b5e"
)

// WorkspaceConfig holds the parameters needed to provision a workspace container.
type WorkspaceConfig struct {
	WorkspaceID      string
	TemplatePath     string            // Host path to template dir to copy from (e.g. claude-code-default/)
        Template         string            // RFC #2948 Phase 1: installed template name, distinct from engine runtime.
	TemplateIdentity string            // RFC #2843 #24: opaque token the TemplateAssetFetcher resolves to the template repo+ref (e.g. "claudius-v1.2.3" or a sha). Used by SaaS; ignored by the local-dir TemplatePath path.
	ConfigFiles      map[string][]byte // Generated config files to write into /configs volume
	PluginsPath      string            // Host path to plugins directory (mounted at /plugins)
	WorkspacePath    string            // Host path to bind-mount as /workspace (if empty, uses Docker named volume)
	Tier             int
	Runtime          string // "claude-code" (default), "codex", "hermes", "openclaw", etc.
	InstanceType     string // Optional CP EC2 instance type override (SaaS only)
	DiskGB           int32  // Optional CP root volume size override in GiB (SaaS only)
	DataPersistence  string // internal#734: "persist"|"ephemeral"|"" — durable-data choice forwarded to CP (SaaS only)
	Provider         string // multi-provider RFC: ""/"aws"|"hetzner"|"gcp" compute backend for the workspace box (per-workspace; distinct from LLM/model provider). Forwarded to CP.
	Display          WorkspaceDisplayConfig
	EnvVars          map[string]string // Additional env vars (API keys, etc.)
	PlatformURL      string

	// WorkspaceSecretKeys are env keys authored via the workspace_secrets table
	// (user/org-admin set, per-workspace). The Forensic #145 SCM-write-token
	// guard EXEMPTS these from stripping: a workspace-scoped GITEA_TOKEN is the
	// intended, legitimate delivery channel for that workspace's agent. Operator/
	// persona-merged (global) SCM tokens are NOT in this set and stay stripped.
	WorkspaceSecretKeys map[string]struct{}
	WorkspaceAccess     string // #65: "none" (default), "read_only", or "read_write"
	ResetClaudeSession  bool   // #12: if true, discard the claude-sessions volume before start (fresh session dir)

	// TemplateAssetFetcher (RFC #2843 #24) is the generic
	// non-secret asset channel for template assets
	// (config.yaml + prompts/ — agent-skills are plugins now,
	// RFC#2843 #32, and are not carried here). The fetcher
	// resolves cfg.TemplateIdentity to a shallow clone of the
	// template repo (Gitea per RFC §4.2 transport option (a))
	// and returns the asset file map. nil = no provider wired
	// (self-host default; falls through to the local TemplatePath
	// path for the config bundle). For SaaS workspaces, main.go
	// wires a real impl (Gitea shallow clone). When the fetcher
	// is set, it MERGES with cfg.ConfigFiles (caller wins on
	// conflict, like the prior PersistedBundleProvider pattern
	// in #2831 PIECE 1) so the existing TemplatePath+ConfigFiles
	// path keeps working for callers that don't opt in.
	TemplateAssetFetcher TemplateAssetFetcher

	// Kind is the workspace kind: "" / "workspace" (ordinary) or "platform"
	// (the org-level concierge / platform agent). When "platform", the local
	// Docker provisioner prefers the platform-agent image variant (which bakes
	// /opt/molecule-mcp-server so the org-admin platform MCP can load) over the
	// plain runtime image. See models.KindPlatform + RFC
	// docs/design/rfc-platform-agent.md.
	Kind string

	// Image, when non-empty, overrides the runtime→image lookup. CP
	// (molecule-controlplane) is the single SSOT for runtime image digest
	// pins via its migrations/027_runtime_image_pins table — the pin is
	// applied at CP's provisioner layer before the workspace-server even
	// runs, so under the current architecture this field is always empty
	// on the workspace-server side. Empty = fall back to RuntimeImages
	// [Runtime] which resolves to the moving `:latest` tag.
	//
	// Historical note: molecule-core's own runtime_image_pins table
	// (workspace-server/migrations 047) was the original aspirational
	// design (#2272 layer 1) but never received a writer; RFC internal#617 /
	// task #335 retired the dead reader + table in favor of CP-as-SSOT.
	Image string
}

type WorkspaceDisplayConfig struct {
	Mode     string `json:"mode,omitempty"`
	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`
	Protocol string `json:"protocol,omitempty"`
}

// selectImage resolves the final Docker image ref for a workspace. The handler
// layer is the source of truth — if it set cfg.Image (the digest-pinned form
// supplied by CP, the SSOT for runtime image pins; molecule-core's own
// runtime_image_pins reader retired by RFC internal#617 / task #335), honor
// that. Otherwise fall back to the runtime→tag lookup in RuntimeImages
// (legacy `:latest` behavior).
//
// Fail-closed contract (RFC internal#483 / security review 4269 /
// feedback_platform_must_hardgate_base_contract): if the workspace NAMES a
// runtime that resolves to no image (not in RuntimeImages, no pinned
// cfg.Image), reject with ErrUnresolvableRuntime instead of silently
// substituting DefaultImage. Pre-fix, removing a runtime from the catalog left
// those create requests silently provisioning a fallback container with no
// signal. The error propagates through Start → markProvisionFailed, which
// already broadcasts WorkspaceProvisionFailed and records the message.
//
// The genuinely-unspecified runtime (empty cfg.Runtime, e.g. an org template
// that doesn't pin one) is an intended distinct path and still resolves to
// DefaultImage — only a NAMED-but-unresolvable runtime is rejected.
func selectImage(cfg WorkspaceConfig) (string, error) {
	if cfg.Image != "" {
		return cfg.Image, nil
	}
	if cfg.Runtime != "" {
		if img, ok := RuntimeImages[cfg.Runtime]; ok {
			return img, nil
		}
		return "", fmt.Errorf("%w: runtime %q (known runtimes: %v)",
			ErrUnresolvableRuntime, cfg.Runtime, knownRuntimes)
	}
	return DefaultImage, nil
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
	return fmt.Sprintf("ws-%s-configs", workspaceID)
}

// legacyConfigVolumeName returns the pre-KI-013 truncated config volume name.
func legacyConfigVolumeName(workspaceID string) string {
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
	return fmt.Sprintf("ws-%s-claude-sessions", workspaceID)
}

// WorkspaceVolumeName returns the Docker named volume for a workspace's
// /workspace mount.
func WorkspaceVolumeName(workspaceID string) string {
	return fmt.Sprintf("ws-%s-workspace", workspaceID)
}

// legacyClaudeSessionVolumeName returns the pre-KI-013 truncated session volume name.
func legacyClaudeSessionVolumeName(workspaceID string) string {
	id := workspaceID
	if len(id) > 12 {
		id = id[:12]
	}
	return fmt.Sprintf("ws-%s-claude-sessions", id)
}

// dockerClient is the subset of client.Client methods used by Provisioner.
// Declared as an interface so tests can inject fakes without a real daemon.
type dockerClient interface {
	Close() error
	ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkingConfig *network.NetworkingConfig, platform *ocispec.Platform, containerName string) (container.CreateResponse, error)
	ContainerExecAttach(ctx context.Context, execID string, config container.ExecAttachOptions) (types.HijackedResponse, error)
	ContainerExecCreate(ctx context.Context, container string, config container.ExecOptions) (container.ExecCreateResponse, error)
	ContainerInspect(ctx context.Context, container string) (container.InspectResponse, error)
	ContainerList(ctx context.Context, options container.ListOptions) ([]container.Summary, error)
	ContainerLogs(ctx context.Context, container string, options container.LogsOptions) (io.ReadCloser, error)
	ContainerRemove(ctx context.Context, container string, options container.RemoveOptions) error
	ContainerRename(ctx context.Context, container, newContainerName string) error
	ContainerStart(ctx context.Context, container string, options container.StartOptions) error
	ContainerWait(ctx context.Context, container string, condition container.WaitCondition) (<-chan container.WaitResponse, <-chan error)
	CopyToContainer(ctx context.Context, container, path string, content io.Reader, options container.CopyToContainerOptions) error
	ImageInspect(ctx context.Context, image string, opts ...client.ImageInspectOption) (dockerimage.InspectResponse, error)
	ImagePull(ctx context.Context, ref string, opts dockerimage.PullOptions) (io.ReadCloser, error)
	VolumeCreate(ctx context.Context, options volume.CreateOptions) (volume.Volume, error)
	VolumeInspect(ctx context.Context, volumeID string) (volume.Volume, error)
	VolumeRemove(ctx context.Context, volumeID string, force bool) error
}

// Provisioner manages Docker containers for workspace agents.
type Provisioner struct {
	cli         dockerClient
	alpineImage string // overridable in tests; production uses the digest-pinned default
}

// New creates a new Provisioner connected to the local Docker daemon.
func New() (*Provisioner, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("failed to connect to Docker: %w", err)
	}
	return &Provisioner{cli: cli, alpineImage: alpineImageDefault}, nil
}

// ContainerName returns the Docker container name for a workspace.
func ContainerName(workspaceID string) string {
	return fmt.Sprintf("ws-%s", workspaceID)
}

// legacyContainerName returns the pre-KI-013 truncated container name.
// Used only for backward-compatible lookups during the deploy transition.
func legacyContainerName(workspaceID string) string {
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

// LabelInstance namespaces a managed container/volume to the SPECIFIC
// platform process (more precisely: the specific Postgres database) that
// provisioned it. LabelManaged alone is NOT enough to claim ownership on
// a shared Docker daemon: two co-resident platform processes (two dev
// stacks, two concurrent CI runs, a blue/green pair) each run their own
// provisioner and each stamp LabelManaged=true. The orphan sweeper's
// wiped-DB pass ("labeled container with no row in MY DB → reap") would
// then cross-reap a sibling platform's perfectly-alive containers (the
// sibling has the DB row; we don't). Observed in production: a second
// platform reaped 25 live containers across 8 workspaces.
//
// Scoping the reap to LabelInstance == PlatformInstanceID() fixes this:
// the wiped-DB pass only ever considers containers THIS database
// provisioned, so a sibling's containers (different DB → different
// instance id, or — for legacy containers — no instance label at all)
// are invisible to it. The per-row orphan logic is unchanged: a
// this-instance container with no row is still the real wiped-DB orphan.
const LabelInstance = "molecule.platform.instance"

// AgentUID / AgentGID are the uid/gid of the unprivileged `agent` user that
// every workspace template creates and drops to via `gosu agent` before
// exec'ing the runtime (the a2a_mcp_server runs under this uid). The value is
// fixed at 1000:1000 across all templates — see:
//   - workspace-configs-templates/claude-code-default/Dockerfile (`useradd -u 1000 ... agent`)
//   - workspace-configs-templates/hermes/Dockerfile               (`useradd -u 1000 ... agent`)
//   - workspace/entrypoint.sh                                     (`exec gosu agent` — "uid 1000")
//
// Files the platform injects into /configs AFTER the entrypoint's
// `chown -R agent:agent /configs` (the post-start #418 re-injection and the
// pre-start #1877 volume write) must be owned by this uid/gid, otherwise the
// agent-uid MCP server hits EACCES reading /configs/.auth_token, sends an
// empty bearer, and the platform 401s on /registry/{id}/peers (list_peers).
const (
	AgentUID = 1000
	AgentGID = 1000
)

// managedLabels is the canonical label map applied to every workspace
// container + volume. Both the cross-platform "this is a Molecule
// platform container" marker (LabelManaged) and the per-instance
// ownership marker (LabelInstance) are stamped here so the orphan
// sweeper can distinguish "ours" from "a co-resident sibling's".
func managedLabels() map[string]string {
	return map[string]string{
		LabelManaged:  "true",
		LabelInstance: PlatformInstanceID(),
	}
}

// platformInstanceID is the resolved-once, process-lifetime stable
// identifier for THIS platform instance. Computed lazily on first use
// and memoised so every container/volume + every sweeper filter in a
// single process sees an identical value.
var (
	platformInstanceOnce sync.Once
	platformInstanceID   string
)

// PlatformInstanceID returns a stable identifier for this platform
// instance, used to namespace managed-container labels so co-resident
// platforms sharing one Docker daemon can't cross-reap each other's
// workspaces.
//
// The identity is derived from DATABASE_URL — the Postgres connection
// string. This is the correct anchor because the orphan sweeper's
// wiped-DB pass is defined RELATIVE TO A SPECIFIC DATABASE ("a labeled
// container with no row in this DB"). Two co-resident platforms point at
// different databases (different host/port/dbname), so their derived ids
// differ; the SAME platform restarting reconnects to the SAME DSN, so
// its id is stable across restarts (the property the per-row orphan
// logic relies on). We hash the DSN with SHA-256 and keep the first 16
// hex chars: it never leaks credentials into a Docker label, is a valid
// label value, and 64 bits is far more than enough to avoid collisions
// between the handful of platforms that could ever share a daemon.
//
// If DATABASE_URL is unset (no-DB test harness / misconfiguration) the
// id falls back to the literal "default". That is intentional: it keeps
// behaviour identical to the pre-namespacing world for a lone platform,
// while two unconfigured co-resident platforms — which would have no DB
// to define a wiped-DB orphan against — are an unsupported deployment we
// don't try to disambiguate.
func PlatformInstanceID() string {
	platformInstanceOnce.Do(func() {
		dsn := os.Getenv("DATABASE_URL")
		if dsn == "" {
			platformInstanceID = "default"
			return
		}
		sum := sha256.Sum256([]byte(dsn))
		platformInstanceID = hex.EncodeToString(sum[:])[:16]
	})
	return platformInstanceID
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
// container provisioned by THIS platform instance — carrying both the
// LabelManaged stamp AND a LabelInstance value matching
// PlatformInstanceID(). Distinct from ListWorkspaceContainerIDPrefixes
// (name-filtered, may include sibling platforms' containers on a shared
// Docker daemon): this method is the "things THIS database provisioned"
// set.
//
// The orphan sweeper uses this for its second pass — reaping containers
// whose workspace row no longer exists at all (the wiped-DB case after
// `docker compose down -v`). The instance-scoped filter is load-bearing
// for multi-platform-on-shared-daemon safety: without it, a co-resident
// sibling platform's live containers (which carry LabelManaged=true from
// their OWN provisioner) would show up here, the sweeper would find no
// row for them in THIS DB, and reap a sibling's healthy workspaces. The
// label filter pins the candidate set to this instance, so a sibling's
// containers (different DB → different instance id) and legacy containers
// (no instance label) are never returned — the wiped-DB pass can't see
// them, let alone reap them.
//
// Returns an empty slice on any Docker error (sweeper treats that as
// "skip this round" — same contract as ListWorkspaceContainerIDPrefixes).
func (p *Provisioner) ListManagedContainerIDPrefixes(ctx context.Context) ([]string, error) {
	if p == nil || p.cli == nil {
		return nil, nil
	}
	containers, err := p.cli.ContainerList(ctx, container.ListOptions{
		All: true,
		// Both labels are required (Docker ANDs multiple label filters):
		// LabelManaged proves it's a Molecule container, LabelInstance
		// proves it's OURS. A sibling platform stamps LabelManaged=true
		// but a different LabelInstance, so it's excluded here.
		Filters: filters.NewArgs(
			filters.Arg("label", LabelManaged+"=true"),
			filters.Arg("label", LabelInstance+"="+PlatformInstanceID()),
		),
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

// allocateHostPort binds a temporary TCP listener on 127.0.0.1:0 and returns
// the allocated ephemeral port. The listener is closed before returning, so
// the port is free for Docker to bind. This lets the provisioner advertise a
// stable, host-reachable workspace URL (http://localhost:<port>) before the
// container exists, eliminating the race where the runtime registers its
// unresolvable Docker container-id hostname.
func allocateHostPort() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("failed to allocate ephemeral host port: %w", err)
	}
	defer l.Close()
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		return "", fmt.Errorf("unexpected listener address type %T", l.Addr())
	}
	return strconv.Itoa(addr.Port), nil
}

// workspaceAdvertiseURL returns the URL the runtime should advertise at
// register time. localhost is accepted by registry validateAgentURL; the
// handler layer then stores the equivalent http://127.0.0.1:<port> URL and
// the A2A proxy rewrites it to ws-<id>:8000 when the platform itself runs
// inside Docker.
//
// #2851 follow-up: when the platform also runs inside a container, the
// workspace must advertise the Docker host/gateway IP (PLATFORM_HOST_IP) so
// the platform container can reach the host-mapped workspace port. The
// operator sets MOLECULE_WORKSPACE_ADVERTISE_HOST to override the default
// "localhost".
func workspaceAdvertiseURL(hostPort string) string {
	host := os.Getenv("MOLECULE_WORKSPACE_ADVERTISE_HOST")
	if host == "" {
		host = "localhost"
	}
	return fmt.Sprintf("http://%s:%s", host, hostPort)
}

// buildStartWorkspaceEnv assembles the full env for the workspace container,
// including the platform-injected MOLECULE_WORKSPACE_URL. Extracted from
// Start() so the production-path env injection is unit-testable without a
// Docker daemon (Researcher #11798 / #11787 close-out — the gap that bit
// 3 rounds running: in real-image lifecycle E2E, the runtime's resolve_workspace_url
// fell back to http://localhost:8000 because the provisioner's
// MOLECULE_WORKSPACE_URL injection was not exercised in tests, and the
// Register handler's URL-preservation CASE only matched the legacy
// 127.0.0.1 prefix — so the upsert overwrote the host-port URL with the
// runtime's 8000 fallback. Both gaps closed: this helper ensures the
// injection happens, and the Register handler's CASE now also matches
// the localhost prefix the provisioner uses after round-3).
func buildStartWorkspaceEnv(cfg WorkspaceConfig, hostPort string) []string {
	return append(buildContainerEnv(cfg),
		fmt.Sprintf("MOLECULE_WORKSPACE_URL=%s", workspaceAdvertiseURL(hostPort)))
}

// resolveStartWorkspaceHostURL computes the hostURL that StartWorkspace
// should return to the platform. The host comes from workspaceAdvertiseURL
// (env override → localhost); the port starts at hostPort and is swapped
// to boundPort if Docker bound a different one.
//
// #2851 (registration-path fix): the persisted hostURL must be the
// host-reachable advertise URL (not 127.0.0.1), so ProxyA2A's
// resolveAgentURL doesn't rewrite it to the internal Docker hostname
// (ws-<id>:8000) and isSafeURL then reject it. The env override lets
// the operator point the runtime at the Docker host/gateway IP in
// containerized-platform mode; the default "localhost" preserves the
// pre-#2851 behavior when the platform runs on the host directly.
func resolveStartWorkspaceHostURL(hostPort, boundPort string) string {
	hostURL := workspaceAdvertiseURL(hostPort)
	if boundPort == "" || boundPort == hostPort {
		return hostURL
	}
	u, err := url.Parse(hostURL)
	if err != nil || u.Hostname() == "" {
		return hostURL
	}
	return fmt.Sprintf("http://%s:%s", u.Hostname(), boundPort)
}

// Start provisions and starts a workspace container.
func (p *Provisioner) Start(ctx context.Context, cfg WorkspaceConfig) (string, error) {
	if p == nil || p.cli == nil {
		return "", ErrNoBackend
	}
	name := ContainerName(cfg.WorkspaceID)
	// KI-013 deploy safety: prefer legacy truncated config volume if it
	// already exists, so pre-deploy workspace data is not orphaned.
	configVolume := p.resolveConfigVolumeName(ctx, cfg.WorkspaceID)

	// #2851: allocate a stable host port BEFORE building container env so the
	// runtime can advertise a host-reachable URL. The alternative — letting
	// Docker pick an ephemeral port and inspecting after start — leaves the
	// runtime guessing its own address and registering an unresolvable
	// container-id hostname.
	hostPort, err := allocateHostPort()
	if err != nil {
		return "", err
	}

	// Create named volume for configs (idempotent — no-op if already exists)
	_, err = p.cli.VolumeCreate(ctx, volume.CreateOptions{
		Name:   configVolume,
		Labels: managedLabels(),
	})
	if err != nil {
		return "", fmt.Errorf("failed to create config volume %s: %w", configVolume, err)
	}
	log.Printf("Provisioner: config volume %s ready", configVolume)

	// #2851 (round-4 production-injection fix): tell the runtime exactly which
	// URL to advertise. The helper injects MOLECULE_WORKSPACE_URL=<host-port>
	// into the container env (host-port URL, not the runtime's listen-port
	// 8000 fallback). The runtime's resolve_workspace_url honors
	// MOLECULE_WORKSPACE_URL at highest precedence, so the registered URL
	// matches the host-port the provisioner allocated — preventing the
	// "registered as localhost:8000 but the host-port is 41751" gap
	// that bit 3 rounds running in production (the real-image lifecycle
	// E2E ProxyA2A path).
	env := buildStartWorkspaceEnv(cfg, hostPort)
	advertiseURL := workspaceAdvertiseURL(hostPort)

	image, imgErr := selectImage(cfg)
	if imgErr != nil {
		// Fail-closed: a named-but-unresolvable runtime must not silently
		// become DefaultImage (RFC internal#483 / review 4269). The caller's
		// error path (markProvisionFailed) broadcasts the failure + records
		// the message so the canvas surfaces it.
		log.Printf("Provisioner: refusing to start %s: %v", cfg.WorkspaceID, imgErr)
		return "", imgErr
	}

	// Local-build mode (issue #63 / Task #194): when MOLECULE_IMAGE_REGISTRY
	// is unset, the OSS contributor path skips the registry pull entirely
	// and instead clones the workspace-template-<runtime> repo from Gitea
	// + `docker build`s it locally. Replace the placeholder image ref with
	// the SHA-pinned tag of the freshly-built image before ContainerCreate.
	//
	// Pinned overrides (cfg.Image set, e.g. via CP's runtime_image_pins for
	// production thin-AMI launches) bypass this path — they pin a digest
	// the operator chose explicitly.
	if cfg.Image == "" && cfg.Runtime != "" {
		if src := Resolve(); src.Mode == RegistryModeLocal {
			builtTag, buildErr := ensureLocalImageHook(ctx, cfg.Runtime)
			if buildErr != nil {
				return "", fmt.Errorf("local-build mode: ensure image for runtime %q: %w", cfg.Runtime, buildErr)
			}
			image = builtTag
			log.Printf("Provisioner: local-build mode → using locally-built image %s for runtime %s", image, cfg.Runtime)

			// kind='platform' (the org concierge): prefer the platform-agent
			// image variant, which bakes /opt/molecule-mcp-server so the
			// org-admin platform MCP can load. Gated on the image being present
			// — if an operator hasn't built it (Dockerfile.platform-agent), we
			// fall back to the plain runtime image + log, so an ordinary local
			// stack keeps working (the concierge just runs without org-admin
			// tools, exactly as before this change). Never breaks normal
			// workspaces (kind != platform) or the SaaS/CP path (handled in the
			// control-plane provisioner — separate repo).
			if cfg.Kind == WorkspaceKindPlatform {
				if paTag, ok := resolvePlatformAgentImage(ctx, cfg.Runtime, image, nil); ok {
					image = paTag
				}
			}
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
		// KI-013 deploy safety: prefer legacy truncated session volume if it
		// already exists, so pre-deploy session data is not orphaned.
		claudeSessionsVolume := p.resolveClaudeSessionVolumeName(ctx, cfg.WorkspaceID)
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
				{HostIP: "127.0.0.1", HostPort: hostPort}, // Pre-allocated stable host port (#2851)
			},
		},
	}

	// Apply tier-based container configuration
	ApplyTierConfig(hostCfg, cfg, configMount, name)

	// Network config — join molecule-core-net with container name as alias
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
	// GHCR so tenant hosts don't need a pre-build step anymore. Two cases
	// trigger a pull:
	//   1. Image not present locally — historical behavior (pull-on-miss).
	//   2. Image present locally AND tag is moving (`:latest`, no tag,
	//      `:staging`, etc.) — without this, a tenant that pulled `:latest`
	//      once is stuck on that snapshot forever even after publish-runtime
	//      pushes a newer image with the same tag. See task #215; sibling
	//      task #232 fixed the same class on the platform-tenant redeploy
	//      path. Pinned tags (semver, sha256) skip the pull because their
	//      contents are by definition immutable.
	// The pull is best-effort: if it fails (network, auth, rate limit) the
	// subsequent ContainerCreate still surfaces the actionable error below.
	imgInspect, imgErr := p.cli.ImageInspect(ctx, image)
	moving := imageTagIsMoving(image)
	switch {
	case imgErr != nil:
		if imgPlatformStr != "" {
			log.Printf("Provisioner: image %s not present locally (%v) — attempting pull (platform=%s)", image, imgErr, imgPlatformStr)
		} else {
			log.Printf("Provisioner: image %s not present locally (%v) — attempting pull", image, imgErr)
		}
	case moving:
		log.Printf("Provisioner: image %s present locally (ID: %s, created: %s) but tag is moving — re-pulling to refresh",
			image, imgInspect.ID[:19], imgInspect.Created[:19])
	default:
		log.Printf("Provisioner: creating %s from image %s (ID: %s, created: %s)",
			name, image, imgInspect.ID[:19], imgInspect.Created[:19])
	}
	if imgErr != nil || moving {
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

	// Seed /configs before the entrypoint starts. molecule-runtime reads
	// /configs/config.yaml immediately; post-start copy races fast runtimes
	// into a FileNotFoundError crash loop.
	if cfg.TemplatePath != "" {
		if err := p.CopyTemplateToContainer(ctx, resp.ID, cfg.TemplatePath); err != nil {
			_ = p.cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
			return "", fmt.Errorf("failed to copy template to container %s before start: %w", name, err)
		}
	}
	if len(cfg.ConfigFiles) > 0 {
		if err := p.WriteFilesToContainer(ctx, resp.ID, cfg.ConfigFiles); err != nil {
			_ = p.cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
			return "", fmt.Errorf("failed to write config files to container %s before start: %w", name, err)
		}
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

	// #2851 (registration-path fix): use the host-reachable advertise URL
	// (not just 127.0.0.1) as the hostURL persisted in the DB. When the
	// platform itself runs inside an act_runner container, 127.0.0.1 in
	// the workspace URL would force ProxyA2A's resolveAgentURL to rewrite
	// to the internal Docker hostname (ws-<id>:8000), which isSafeURL then
	// rejects as "workspace URL is not publicly routable". By persisting
	// the advertise URL (172.18.0.1:<port> in containerized dev, or the
	// operator's host IP in prod), the platform-facing URL is host-
	// reachable, the resolveAgentURL rewrite doesn't kick in, and the
	// dev-mode SSRF relaxation allows the 172.18/16 private range.
	hostURL := resolveStartWorkspaceHostURL(hostPort, "")
	for attempt := 0; attempt < 3; attempt++ {
		info, inspectErr := p.cli.ContainerInspect(ctx, resp.ID)
		if inspectErr != nil {
			break
		}
		portBindings := info.NetworkSettings.Ports[nat.Port(DefaultPort+"/tcp")]
		if len(portBindings) > 0 && portBindings[0].HostPort == hostPort {
			break
		}
		if attempt < 2 {
			time.Sleep(500 * time.Millisecond) // wait for Docker to bind the port
		} else {
			log.Printf("Provisioner: container %s did not bind expected host port %s; falling back to bound port (keeping advertise host)", name, hostPort)
			if len(portBindings) > 0 {
				hostURL = resolveStartWorkspaceHostURL(hostPort, portBindings[0].HostPort)
			}
		}
	}

	log.Printf("Provisioner: started container %s for workspace %s at %s (advertise: %s, internal: %s)", name, cfg.WorkspaceID, hostURL, advertiseURL, InternalURL(cfg.WorkspaceID))
	return hostURL, nil
}

// buildWorkspaceMount returns the Docker volume spec for /workspace (#65).
//
// Selection matrix:
//
//	cfg.WorkspacePath | cfg.WorkspaceAccess     | mount
//	------------------+-------------------------+--------------------------------
//	""                | "" / "none"             | <named-volume>:/workspace  (isolated, current default)
//	"<host-dir>"      | "" / "read_write"       | <host-dir>:/workspace      (current PM behaviour)
//	"<host-dir>"      | "read_only"             | <host-dir>:/workspace:ro   (research agents get read access without write risk)
//	""                | "read_only"/"read_write"| <named-volume>:/workspace  (degraded — access requires a mount; validated at handler layer)
//
// Kept pure + side-effect-free so it's unit-testable.
func buildWorkspaceMount(cfg WorkspaceConfig) string {
	// Named volume when no host path is configured.
	if cfg.WorkspacePath == "" {
		return fmt.Sprintf("%s:/workspace", WorkspaceVolumeName(cfg.WorkspaceID))
	}
	// Host bind mount. Append :ro for read-only mode; otherwise default
	// (implicit read-write). "none" explicitly opts out of the mount
	// even when a path is set.
	if cfg.WorkspaceAccess == WorkspaceAccessNone {
		return fmt.Sprintf("%s:/workspace", WorkspaceVolumeName(cfg.WorkspaceID))
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

// scmWriteTokenKeys is the explicit denylist of environment variable names
// that carry a Git SCM *write* credential (push / merge / approve). These
// must never reach a tenant workspace container — see the forensic #145
// rationale in buildContainerEnv. Kept as an exact-match set rather than a
// substring/prefix heuristic so the guard is auditable and can't silently
// over-strip a legitimately-named var.
var scmWriteTokenKeys = map[string]struct{}{
	"GITEA_TOKEN":     {},
	"GITHUB_TOKEN":    {},
	"GH_TOKEN":        {}, // gh CLI honours GH_TOKEN as a GITHUB_TOKEN alias
	"GITLAB_TOKEN":    {},
	"GL_TOKEN":        {}, // glab CLI alias
	"BITBUCKET_TOKEN": {},
}

// isSCMWriteTokenKey reports whether an env var name is a known Git SCM
// write credential that must be stripped from tenant workspace env.
func isSCMWriteTokenKey(key string) bool {
	_, ok := scmWriteTokenKeys[key]
	return ok
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
		// (claude-code, codex, hermes, openclaw, …) without needing to PR each
		// standalone template repo. Per-template ENV in the Dockerfile can
		// still override (Dockerfile ENV is overridden by docker -e at runtime).
		"PYTHONPATH=/app",
	}
	// #1687: track explicit GH_TOKEN / GITHUB_TOKEN so they win over GH_PAT
	// alias. These are normally stripped by the SCM-write guard below, but
	// when a user explicitly sets them we preserve the value.
	var explicitGHToken, explicitGitHubToken string
	for k, v := range cfg.EnvVars {
		if k == "GH_TOKEN" {
			explicitGHToken = v
			continue
		}
		if k == "GITHUB_TOKEN" {
			explicitGitHubToken = v
			continue
		}
		// Forensic #145 hardening: tenant workspace containers run
		// agent-controlled code and must NEVER receive a Git SCM *write*
		// credential. Without merge/approve creds in-container the
		// two-eyes review gate is structurally self-bypass-proof — an
		// agent that forges an approval has no token to act on it. A
		// latent path exists (loadPersonaEnvFile merges a per-role
		// persona `GITEA_TOKEN` into cfg.EnvVars when MOLECULE_PERSONA_ROOT
		// is set on a tenant host); it is inert today (persona dirs are
		// operator-host-only) but unguarded. Strip SCM-write tokens here
		// by construction so the invariant holds regardless of whether
		// that path ever becomes reachable.
		if isSCMWriteTokenKey(k) {
			log.Printf("buildContainerEnv: dropped SCM-write credential %q from workspace env (forensic #145 guard)", k)
			continue
		}
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}
	// #1687: alias GH_PAT → GH_TOKEN / GITHUB_TOKEN on the READ side
	// (container env assembly). Explicit values win: only alias when the
	// key was not set in workspace secrets.
	if explicitGHToken != "" {
		env = append(env, fmt.Sprintf("GH_TOKEN=%s", explicitGHToken))
	} else if pat, hasPAT := cfg.EnvVars["GH_PAT"]; hasPAT && pat != "" {
		env = append(env, fmt.Sprintf("GH_TOKEN=%s", pat))
	}
	if explicitGitHubToken != "" {
		env = append(env, fmt.Sprintf("GITHUB_TOKEN=%s", explicitGitHubToken))
	} else if pat, hasPAT := cfg.EnvVars["GH_PAT"]; hasPAT && pat != "" {
		env = append(env, fmt.Sprintf("GITHUB_TOKEN=%s", pat))
	}
	// Inject ADMIN_TOKEN from the platform server's environment so workspace
	// containers can call /admin/liveness and other admin-gated endpoints
	// (core#831). cp_provisioner.go handles this separately for SaaS tenants.
	if adminToken := os.Getenv("ADMIN_TOKEN"); adminToken != "" {
		env = append(env, fmt.Sprintf("ADMIN_TOKEN=%s", adminToken))
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
		hostCfg.Memory = memMB * 1024 * 1024
	}
	if cpuShares > 0 {
		// shares -> NanoCPUs: 1024 shares == 1 CPU == 1e9 NanoCPUs
		hostCfg.NanoCPUs = (cpuShares * 1_000_000_000) / 1024
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
	buf, err := buildTemplateTar(templatePath)
	if err != nil {
		return err
	}

	return p.cli.CopyToContainer(ctx, containerID, "/configs", buf, container.CopyToContainerOptions{})
}

func buildTemplateTar(templatePath string) (*bytes.Buffer, error) {
	// Resolve symlinks at the root before walking. filepath.Walk does
	// NOT follow a symlink that IS the root — it Lstats the path, sees
	// a symlink (non-directory), and emits exactly one entry without
	// descending. With cross-repo composition (parent template's
	// dev-lead → ../sibling-repo/dev-lead/, see internal#77), the
	// caller routinely passes a symlink as templatePath. Without this
	// resolution the workspace's /configs/ mount lands empty.
	//
	// Security: templatePath has already passed resolveInsideRoot's
	// path-string check at the call site — the trust boundary is the
	// operator-side /org-templates/ filesystem layout, not this
	// resolution step.
	if resolved, err := filepath.EvalSymlinks(templatePath); err == nil {
		templatePath = resolved
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	err := filepath.Walk(templatePath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		// OFFSEC-010: skip symlinks to prevent path traversal via malicious
		// template symlinks (e.g. template/.ssh → /root/.ssh). filepath.Walk
		// follows symlinks by default, so without this guard a crafted symlink
		// inside the template directory could escape to include arbitrary host
		// files in the tar archive. We intentionally skip rather than error so
		// a broken symlink in an org template is a silent no-op.
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
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
		return nil, fmt.Errorf("failed to create tar from %s: %w", templatePath, err)
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("failed to close tar writer: %w", err)
	}

	return &buf, nil
}

// buildConfigFilesTar builds the tar stream that WriteFilesToContainer streams
// into /configs via CopyToContainer. Every entry is stamped Uid/Gid = agent
// (AgentUID/AgentGID) so the files land agent-owned after extraction. This is
// the issue #418 post-start re-injection path: it runs AFTER the template
// entrypoint's `chown -R agent:agent /configs`, so without explicit ownership
// in the tar header the files extract as root:root (tar Uid/Gid default 0) and
// the agent-uid MCP server can no longer read /configs/.auth_token (and
// /configs/.platform_inbound_secret) → empty bearer → list_peers 401.
//
// Pulled out as a pure function so the ownership contract is unit-testable
// without a live Docker daemon (mirrors buildTemplateTar).
func buildConfigFilesTar(files map[string][]byte) (*bytes.Buffer, error) {
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
				Uid:      AgentUID,
				Gid:      AgentGID,
			}); err != nil {
				return nil, fmt.Errorf("failed to write tar dir header for %s: %w", dir, err)
			}
			createdDirs[dir] = true
		}

		header := &tar.Header{
			Name: name,
			Mode: 0644,
			Size: int64(len(data)),
			Uid:  AgentUID,
			Gid:  AgentGID,
		}
		if err := tw.WriteHeader(header); err != nil {
			return nil, fmt.Errorf("failed to write tar header for %s: %w", name, err)
		}
		if _, err := tw.Write(data); err != nil {
			return nil, fmt.Errorf("failed to write tar data for %s: %w", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("failed to close tar writer: %w", err)
	}
	return &buf, nil
}

// WriteFilesToContainer writes in-memory files into /configs in the container,
// agent-owned (see buildConfigFilesTar).
func (p *Provisioner) WriteFilesToContainer(ctx context.Context, containerID string, files map[string][]byte) error {
	buf, err := buildConfigFilesTar(files)
	if err != nil {
		return err
	}
	return p.cli.CopyToContainer(ctx, containerID, "/configs", buf, container.CopyToContainerOptions{})
}

// CopyToContainer exposes CopyToContainer from the Docker client for use by other packages.
func (p *Provisioner) CopyToContainer(ctx context.Context, containerID, dstPath string, content io.Reader) error {
	return p.cli.CopyToContainer(ctx, containerID, dstPath, content, container.CopyToContainerOptions{})
}

// ExecRead runs "cat <filePath>" in an existing container and returns the output.
// Used to read config files from a running container before stopping it.
func (p *Provisioner) ExecRead(ctx context.Context, containerName, filePath string) ([]byte, error) {
	if p == nil || p.cli == nil {
		return nil, ErrNoBackend
	}
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
		Image: p.alpineImage,
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

// writeAuthTokenVolumeCmd is the shell command the throwaway alpine container
// runs to seed /vol/.auth_token. alpine runs it as root, so without the
// explicit `chown 1000:1000` the file stays root:root after the template
// entrypoint's `chown -R agent:agent /configs` has already run — the agent-uid
// (AgentUID) MCP server then gets EACCES reading it → empty bearer →
// list_peers 401. Pulled out as a pure function so the ownership contract is
// unit-testable without a live Docker daemon. Issue #1877.
func writeAuthTokenVolumeCmd() string {
	return fmt.Sprintf(
		"mkdir -p /vol && printf '%%s' $TOKEN > /vol/.auth_token && chmod 0600 /vol/.auth_token && chown %d:%d /vol/.auth_token",
		AgentUID, AgentGID,
	)
}

// WriteAuthTokenToVolume writes the workspace auth token into the config volume
// BEFORE the container starts, eliminating the token-injection race window where
// a restarted container could read a stale token from /configs/.auth_token before
// WriteFilesToContainer writes the new one. Issue #1877.
//
// Uses a throwaway alpine container to write directly to the named volume,
// bypassing the container lifecycle entirely. The written file is chowned to
// the agent uid/gid (see writeAuthTokenVolumeCmd).
func (p *Provisioner) WriteAuthTokenToVolume(ctx context.Context, workspaceID, token string) error {
	if p == nil || p.cli == nil {
		return ErrNoBackend
	}
	volName := p.resolveConfigVolumeName(ctx, workspaceID)
	resp, err := p.cli.ContainerCreate(ctx, &container.Config{
		Image: p.alpineImage,
		Cmd:   []string{"sh", "-c", writeAuthTokenVolumeCmd()},
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

// resolveConfigVolumeName returns the effective config volume name for a
// workspace.  KI-013 deploy safety: if a legacy truncated-name volume exists,
// it is migrated in-place to the new full-ID name so existing workspace data
// is preserved AND all workspaces eventually use collision-safe names.
func (p *Provisioner) resolveConfigVolumeName(ctx context.Context, workspaceID string) string {
	if p == nil || p.cli == nil {
		return ConfigVolumeName(workspaceID)
	}
	newName := ConfigVolumeName(workspaceID)
	legacy := legacyConfigVolumeName(workspaceID)
	if err := p.migrateVolumeIfNeeded(ctx, newName, legacy); err != nil {
		log.Printf("Provisioner: volume migration warning for %s: %v", workspaceID, err)
	}
	return newName
}

// resolveClaudeSessionVolumeName returns the effective claude-sessions volume
// name.  KI-013 deploy safety: legacy truncated-name volumes are migrated
// in-place to the new full-ID name.
func (p *Provisioner) resolveClaudeSessionVolumeName(ctx context.Context, workspaceID string) string {
	if p == nil || p.cli == nil {
		return ClaudeSessionVolumeName(workspaceID)
	}
	newName := ClaudeSessionVolumeName(workspaceID)
	legacy := legacyClaudeSessionVolumeName(workspaceID)
	if err := p.migrateVolumeIfNeeded(ctx, newName, legacy); err != nil {
		log.Printf("Provisioner: session volume migration warning for %s: %v", workspaceID, err)
	}
	return newName
}

// migrateVolumeIfNeeded renames a legacy truncated-name Docker volume to its
// new full-ID name by copying data via a temporary alpine container.  If the
// legacy volume does not exist, or the new volume already exists, this is a
// no-op.  The operation is idempotent — calling it multiple times is safe.
func (p *Provisioner) migrateVolumeIfNeeded(ctx context.Context, newName, legacyName string) error {
	if p == nil || p.cli == nil {
		return nil
	}

	// Legacy volume missing — nothing to migrate.
	if _, err := p.cli.VolumeInspect(ctx, legacyName); err != nil {
		return nil
	}

	// New volume already exists — migration already done (or new workspace).
	if _, err := p.cli.VolumeInspect(ctx, newName); err == nil {
		return nil
	}

	// Create the new volume.
	if _, err := p.cli.VolumeCreate(ctx, volume.CreateOptions{
		Name:   newName,
		Labels: managedLabels(),
	}); err != nil {
		return fmt.Errorf("create new volume %s: %w", newName, err)
	}

	// Copy data from legacy to new via a short-lived alpine container.
	// The trailing test guards against silent empty copies (e.g. legacy
	// volume unexpectedly bare) which would leave the workspace without
	// its config on restart.  Core#2545.
	resp, err := p.cli.ContainerCreate(ctx, &container.Config{
		Image: p.alpineImage,
		Cmd:   []string{"sh", "-c", "cp -a /legacy/. /new/ && test -n \"$(ls -A /new/)\""},
	}, &container.HostConfig{
		Binds: []string{
			legacyName + ":/legacy",
			newName + ":/new",
		},
	}, nil, nil, "")
	if err != nil {
		return fmt.Errorf("create migration container: %w", err)
	}
	defer p.cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})

	if err := p.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("start migration container: %w", err)
	}

	waitCh, errCh := p.cli.ContainerWait(ctx, resp.ID, container.WaitConditionNotRunning)
	select {
	case waitResp := <-waitCh:
		if waitResp.StatusCode != 0 {
			return fmt.Errorf("migration copy failed (exit %d) — preserving legacy volume %s for retry", waitResp.StatusCode, legacyName)
		}
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("migration container exited with error: %w", err)
		}
	}

	// Explicitly remove the migration container before removing the legacy
	// volume so the volume is no longer referenced. The deferred remove above
	// is a safety-net for early-return paths.
	_ = p.cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})

	// Best-effort cleanup of the legacy volume.  If removal fails (e.g. still
	// referenced by a running container), the new volume is already populated
	// and the next restart will retry.
	if err := p.cli.VolumeRemove(ctx, legacyName, true); err != nil {
		log.Printf("Provisioner: warning: failed to remove legacy volume %s after migration: %v", legacyName, err)
	} else {
		log.Printf("Provisioner: migrated legacy volume %s → %s", legacyName, newName)
	}
	return nil
}

// RemoveVolume removes the config volume for a workspace.
// Also removes the claude-sessions volume (best-effort, may not exist
// for non claude-code runtimes). Issue #12.
// Also removes the claude-sessions volume (best-effort, may not exist
// for non claude-code runtimes). Issue #12.
func (p *Provisioner) RemoveVolume(ctx context.Context, workspaceID string) error {
	if p == nil || p.cli == nil {
		return ErrNoBackend
	}
	// KI-013 deploy safety: remove both new full-ID name and legacy
	// truncated name if present, so pre-deploy volumes are not orphaned.
	removed := false
	for _, volName := range []string{ConfigVolumeName(workspaceID), legacyConfigVolumeName(workspaceID)} {
		if err := p.cli.VolumeRemove(ctx, volName, true); err == nil {
			log.Printf("Provisioner: removed config volume %s", volName)
			removed = true
		}
	}
	if !removed {
		return fmt.Errorf("failed to remove config volume for %s", workspaceID)
	}
	for _, csName := range []string{ClaudeSessionVolumeName(workspaceID), legacyClaudeSessionVolumeName(workspaceID)} {
		if rmErr := p.cli.VolumeRemove(ctx, csName, true); rmErr == nil {
			log.Printf("Provisioner: removed claude-sessions volume %s", csName)
		}
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
	// KI-013 deploy safety: try new full-ID name first, then fall back to
	// the old truncated name so pre-deploy containers are still stoppable.
	names := []string{ContainerName(workspaceID), legacyContainerName(workspaceID)}
	for _, name := range names {
		// Force-remove kills and removes in one atomic operation, bypassing
		// the restart policy entirely.
		err := p.cli.ContainerRemove(ctx, name, container.RemoveOptions{Force: true})
		if err == nil {
			log.Printf("Provisioner: stopped and removed container %s", name)
			return nil
		}
		if isContainerNotFound(err) {
			// Try the next name (legacy fallback). If both miss, the
			// container is genuinely gone — post-condition satisfied.
			continue
		}
		if isRemovalInProgress(err) {
			// Another concurrent caller is already removing this container.
			log.Printf("Provisioner: container %s removal already in progress (no-op)", name)
			return nil
		}
		// Real failure: daemon timeout, socket EOF, ctx cancellation, etc.
		log.Printf("Provisioner: force-remove failed for %s: %v", name, err)
		return fmt.Errorf("force-remove %s: %w", name, err)
	}
	// Both names missed — container was already gone.
	log.Printf("Provisioner: container %s already gone (no-op)", ContainerName(workspaceID))
	return nil
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
	name, err := RunningContainerName(ctx, p.cli, workspaceID)
	if err != nil {
		// Transient daemon error: caller treats !running as dead + restarts.
		// Returning true + the underlying error preserves the error for
		// metrics/logging without triggering the destructive path.
		return true, err
	}
	return name != "", nil
}

// RunningContainerName returns the container name for workspaceID iff the
// container exists AND is in the Running state. Single source of truth for
// "what live container should I exec into for this workspace?" — used by
// both Provisioner.IsRunning (healthsweep) and the plugins handler.
//
// Distinguishes three outcomes so callers can pick their own policy:
//
//   - ("ws-<id>", nil): container is running. Caller can exec into it.
//   - ("",        nil): container does not exist OR exists but is stopped
//     (NotFound, Exited, Created, Restarting…). Caller
//     should treat as a definitive "not running."
//   - ("",        err): transient daemon error (timeout, socket EOF, ctx
//     cancel). Caller should NOT infer "not running" —
//     this could be a flaky daemon under load. Decide
//     per-callsite whether to fail soft or hard.
//
// Background — molecule-core#10: the plugins handler used to carry its own
// copy of this inspect logic (`findRunningContainer`) which collapsed
// transient errors into the same "" return as a genuinely-stopped container.
// That hid daemon flakes as misleading 503 "container not running" responses
// AND let the two impls drift on edge-case behavior. This is the SSOT.
// isNilDockerClient reports whether cli is nil or a typed nil pointer
// (e.g. (*client.Client)(nil) passed as a dockerClient interface value).
// Required because a non-nil interface holding a nil pointer does not == nil.
func isNilDockerClient(cli dockerClient) bool {
	if cli == nil {
		return true
	}
	switch c := cli.(type) {
	case *client.Client:
		return c == nil
	default:
		return false
	}
}

func RunningContainerName(ctx context.Context, cli dockerClient, workspaceID string) (string, error) {
	if isNilDockerClient(cli) {
		return "", ErrNoBackend
	}

	newName := ContainerName(workspaceID)
	legacyName := legacyContainerName(workspaceID)

	// If a legacy container is still running, rename it in-place to the
	// new full-ID name so all callers converge on collision-safe names.
	legacyInfo, legacyErr := cli.ContainerInspect(ctx, legacyName)
	if legacyErr == nil && legacyInfo.State.Running {
		if _, newErr := cli.ContainerInspect(ctx, newName); isContainerNotFound(newErr) {
			if renameErr := cli.ContainerRename(ctx, legacyName, newName); renameErr == nil {
				log.Printf("Provisioner: renamed legacy container %s → %s", legacyName, newName)
				return newName, nil
			} else {
				log.Printf("Provisioner: warning: failed to rename legacy container %s → %s: %v", legacyName, newName, renameErr)
			}
		}
		// Rename not possible (or new name already occupied) — return legacy
		// name so the caller can still exec into the live container.
		return legacyName, nil
	}

	// Standard path: look for a running container with the new name.
	info, err := cli.ContainerInspect(ctx, newName)
	if err != nil {
		if isContainerNotFound(err) {
			return "", nil
		}
		return "", err
	}
	if info.State.Running {
		return newName, nil
	}
	return "", nil
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

// isRemovalInProgress detects the race where Docker is already removing
// the container in response to a concurrent call. Symptom observed
// during cascade-delete of a 7-workspace org: two of the seven returned
//
//	Error response from daemon: removal of container ws-xxx is already in progress
//
// because the platform's deletion fanout fired Stop() on every workspace
// in parallel and the orphan sweeper happened to also reap two of them
// at the same instant. The post-condition is identical to a successful
// removal — the container WILL be gone — so callers should treat this
// as a no-op rather than a real failure.
//
// String-match for the same reason as isContainerNotFound: docker/docker
// surfaces this as a plain error string, no typed predicate. The CLI
// itself relies on the message text.
func isRemovalInProgress(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "removal of container") &&
		strings.Contains(err.Error(), "already in progress")
}

// DockerClient returns the underlying Docker client for sharing with other handlers.
// If the provisioner is backed by a fake (e.g. in unit tests), this returns nil.
func (p *Provisioner) DockerClient() *client.Client {
	if p == nil || p.cli == nil {
		return nil
	}
	if c, ok := p.cli.(*client.Client); ok {
		return c
	}
	return nil
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
	if p == nil || p.cli == nil {
		return false, ErrNoBackend
	}
	volName := ConfigVolumeName(workspaceID)
	// Confirm the volume exists first — Docker auto-creates on bind otherwise.
	if _, err := p.cli.VolumeInspect(ctx, volName); err != nil {
		return false, nil
	}
	resp, err := p.cli.ContainerCreate(ctx, &container.Config{
		Image: p.alpineImage,
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

// imageTagIsMoving reports whether the tag portion of an image reference
// is one whose contents change over time at the registry — meaning a
// local-cache hit is not safe to trust because the cached snapshot may
// be stale relative to what the registry currently serves under the
// same tag.
//
// Returns true for:
//   - References with no tag at all (Docker defaults the missing tag
//     to `:latest`, which is the canonical moving tag).
//   - Explicit `:latest`, `:staging`, `:main`, `:dev`, `:edge`, `:nightly`,
//     `:rolling` — the conventional set of "moves on every publish"
//     tags across the org's pipelines.
//
// Returns false for:
//   - Digest-pinned references (`@sha256:...`) — by definition immutable.
//   - Semver / SHA / build-ID tags (`:0.8.2`, `:abc1234`, `:2026-04-30`) —
//     these are conventionally pinned, and even if a publisher mis-uses
//     them, the wrong behavior is "stale" not "broken-fleet" because
//     the tenant who chose a pinned tag is asking for that snapshot.
//
// The classification is deliberately conservative on the "moving" side
// (only the well-known moving tags) because mis-classifying a pinned
// tag as moving means we re-pull on every provision — wasted bandwidth,
// no correctness loss. Mis-classifying moving as pinned silently bricks
// the fleet on stale snapshots — exactly the bug class that motivated
// task #215. So the bias is: when in doubt, treat as pinned.
//
// Sibling task #232 (Platform-tenant :latest re-pull on redeploy)
// applied the same principle on the controlplane redeploy path. Keep
// the moving-tag list aligned across both implementations if updated.
func imageTagIsMoving(image string) bool {
	// Digest-pinned references are immutable by construction.
	if strings.Contains(image, "@sha256:") {
		return false
	}
	// Strip everything before the LAST colon to isolate the tag, but
	// stop at a `/` to avoid mistaking a port number in a registry
	// hostname (e.g. `localhost:5000/foo`) for a tag.
	tag := ""
	if i := strings.LastIndex(image, ":"); i >= 0 && !strings.Contains(image[i+1:], "/") {
		tag = image[i+1:]
	}
	switch tag {
	case "", "latest", "staging", "main", "dev", "edge", "nightly", "rolling":
		return true
	}
	return false
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
