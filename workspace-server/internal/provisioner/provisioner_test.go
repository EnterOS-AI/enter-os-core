package provisioner

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
)

// TestValidateConfigSource covers issue #17: a workspace restart with no
// template and no in-memory configFiles must be caught before Docker
// starts a container destined to crash-loop on FileNotFoundError.
func TestValidateConfigSource_ConfigFilesPresent(t *testing.T) {
	files := map[string][]byte{"config.yaml": []byte("name: test\n")}
	if err := ValidateConfigSource("", files); err != nil {
		t.Fatalf("expected nil error when configFiles has config.yaml, got %v", err)
	}
}

func TestValidateConfigSource_ConfigFilesEmptyValue(t *testing.T) {
	files := map[string][]byte{"config.yaml": {}}
	if err := ValidateConfigSource("", files); !errors.Is(err, ErrNoConfigSource) {
		t.Fatalf("expected ErrNoConfigSource for empty config.yaml bytes, got %v", err)
	}
}

func TestValidateConfigSource_TemplatePathWithConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("name: x\n"), 0644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := ValidateConfigSource(dir, nil); err != nil {
		t.Fatalf("expected nil when template dir has config.yaml, got %v", err)
	}
}

func TestValidateConfigSource_TemplatePathMissingConfig(t *testing.T) {
	dir := t.TempDir() // empty dir
	if err := ValidateConfigSource(dir, nil); !errors.Is(err, ErrNoConfigSource) {
		t.Fatalf("expected ErrNoConfigSource for template dir without config.yaml, got %v", err)
	}
}

func TestValidateConfigSource_BothEmpty(t *testing.T) {
	if err := ValidateConfigSource("", nil); !errors.Is(err, ErrNoConfigSource) {
		t.Fatalf("expected ErrNoConfigSource when both sources empty, got %v", err)
	}
}

func TestValidateConfigSource_TemplateIsDirName(t *testing.T) {
	// If `config.yaml` at the template path is itself a directory (weird
	// but possible), the validator should reject it.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "config.yaml"), 0755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := ValidateConfigSource(dir, nil); !errors.Is(err, ErrNoConfigSource) {
		t.Fatalf("expected ErrNoConfigSource when config.yaml is a dir, got %v", err)
	}
}

func TestStartSeedsConfigsBeforeContainerStart(t *testing.T) {
	src, err := os.ReadFile("provisioner.go")
	if err != nil {
		t.Fatalf("read provisioner.go: %v", err)
	}
	text := string(src)
	copyTemplate := strings.Index(text, "p.CopyTemplateToContainer(ctx, resp.ID, cfg.TemplatePath)")
	writeFiles := strings.Index(text, "p.WriteFilesToContainer(ctx, resp.ID, cfg.ConfigFiles)")
	start := strings.Index(text, "p.cli.ContainerStart(ctx, resp.ID, container.StartOptions{})")

	if copyTemplate < 0 || writeFiles < 0 || start < 0 {
		t.Fatalf("expected Start to copy template, write config files, and start container")
	}
	if copyTemplate >= start || writeFiles >= start {
		t.Fatalf("config seeding must happen before ContainerStart: copyTemplate=%d writeFiles=%d start=%d", copyTemplate, writeFiles, start)
	}
}

func TestBuildTemplateTar_SkipsSymlinks(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("name: safe\n"), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("do-not-copy\n"), 0644); err != nil {
		t.Fatalf("write outside target: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "linked-secret.txt")); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	buf, err := buildTemplateTar(dir)
	if err != nil {
		t.Fatalf("buildTemplateTar: %v", err)
	}

	names := map[string]string{}
	tr := tar.NewReader(buf)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read tar: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read body for %s: %v", hdr.Name, err)
		}
		names[hdr.Name] = string(body)
	}

	if got := names["config.yaml"]; got != "name: safe\n" {
		t.Fatalf("config.yaml body = %q, want safe config", got)
	}
	if _, ok := names["linked-secret.txt"]; ok {
		t.Fatalf("symlink entry was copied into template tar: %#v", names)
	}
	for name, body := range names {
		if strings.Contains(body, "do-not-copy") {
			t.Fatalf("symlink target leaked through %s: %q", name, body)
		}
	}
}

// baseHostConfig returns a fresh HostConfig with typical pre-tier binds,
// mimicking what Start() builds before calling ApplyTierConfig.
func baseHostConfig(pluginsPath string) *container.HostConfig {
	binds := []string{
		"ws-abc123-configs:/configs",
		"ws-abc123-workspace:/workspace",
	}
	if pluginsPath != "" {
		binds = append(binds, pluginsPath+":/plugins:ro")
	}
	return &container.HostConfig{
		Binds: binds,
	}
}

func TestApplyTierConfig_Tier1_Sandboxed(t *testing.T) {
	configMount := "ws-abc123-configs:/configs"
	hc := baseHostConfig("")
	cfg := WorkspaceConfig{
		WorkspaceID: "abc123",
		Tier:        1,
	}

	ApplyTierConfig(hc, cfg, configMount, "ws-abc123")

	// T1 should strip /workspace mount — only config bind remains
	if len(hc.Binds) != 1 {
		t.Fatalf("T1: expected 1 bind (config only), got %d: %v", len(hc.Binds), hc.Binds)
	}
	if hc.Binds[0] != configMount {
		t.Errorf("T1: expected bind %q, got %q", configMount, hc.Binds[0])
	}

	// ReadonlyRootfs must be set
	if !hc.ReadonlyRootfs {
		t.Error("T1: expected ReadonlyRootfs=true")
	}

	// Tmpfs at /tmp must be set
	if _, ok := hc.Tmpfs["/tmp"]; !ok {
		t.Error("T1: expected tmpfs mount at /tmp")
	}

	// Must NOT be privileged
	if hc.Privileged {
		t.Error("T1: must not be privileged")
	}

	// Must NOT have host network
	if hc.NetworkMode == "host" {
		t.Error("T1: must not have host network")
	}
}

func TestApplyTierConfig_Tier1_NoGlobalPlugins(t *testing.T) {
	configMount := "ws-abc123-configs:/configs"
	hc := baseHostConfig("")
	cfg := WorkspaceConfig{
		WorkspaceID: "abc123",
		Tier:        1,
	}

	ApplyTierConfig(hc, cfg, configMount, "ws-abc123")

	// T1 should have only 1 bind: config (plugins are per-workspace in /configs/plugins/)
	if len(hc.Binds) != 1 {
		t.Fatalf("T1: expected 1 bind, got %d: %v", len(hc.Binds), hc.Binds)
	}
	if hc.Binds[0] != configMount {
		t.Errorf("T1: expected bind %q, got %q", configMount, hc.Binds[0])
	}
}

func TestApplyTierConfig_Tier2_Standard(t *testing.T) {
	configMount := "ws-abc123-configs:/configs"
	hc := baseHostConfig("")
	originalBinds := make([]string, len(hc.Binds))
	copy(originalBinds, hc.Binds)

	cfg := WorkspaceConfig{
		WorkspaceID: "abc123",
		Tier:        2,
	}

	ApplyTierConfig(hc, cfg, configMount, "ws-abc123")

	// T2 should NOT modify binds — /workspace mount stays
	if len(hc.Binds) != len(originalBinds) {
		t.Fatalf("T2: binds should be unchanged, got %v", hc.Binds)
	}

	// Memory limit: 512 MiB
	expectedMemory := int64(512 * 1024 * 1024)
	if hc.Memory != expectedMemory {
		t.Errorf("T2: expected Memory=%d (512m), got %d", expectedMemory, hc.Memory)
	}

	// CPU limit: 1.0 CPU (1e9 NanoCPUs)
	expectedCPU := int64(1_000_000_000)
	if hc.NanoCPUs != expectedCPU {
		t.Errorf("T2: expected NanoCPUs=%d (1.0 CPU), got %d", expectedCPU, hc.NanoCPUs)
	}

	// Must NOT be privileged
	if hc.Privileged {
		t.Error("T2: must not be privileged")
	}

	// Must NOT have host network
	if hc.NetworkMode == "host" {
		t.Error("T2: must not have host network")
	}

	// Must NOT have readonly rootfs
	if hc.ReadonlyRootfs {
		t.Error("T2: must not have ReadonlyRootfs")
	}
}

func TestApplyTierConfig_Tier3_Privileged(t *testing.T) {
	configMount := "ws-abc123-configs:/configs"
	hc := baseHostConfig("")
	originalBinds := make([]string, len(hc.Binds))
	copy(originalBinds, hc.Binds)

	cfg := WorkspaceConfig{
		WorkspaceID: "abc123",
		Tier:        3,
	}

	ApplyTierConfig(hc, cfg, configMount, "ws-abc123")

	// T3 must be privileged
	if !hc.Privileged {
		t.Error("T3: expected Privileged=true")
	}

	// T3 must have host PID
	if hc.PidMode != "host" {
		t.Errorf("T3: expected PidMode=host, got %q", hc.PidMode)
	}

	// T3 must NOT have host network (to avoid port collisions)
	if hc.NetworkMode == "host" {
		t.Error("T3: must not have host network (use Docker network for inter-container discovery)")
	}

	// Binds should be unchanged (keeps /workspace)
	if len(hc.Binds) != len(originalBinds) {
		t.Fatalf("T3: binds should be unchanged, got %v", hc.Binds)
	}
}

func TestApplyTierConfig_Tier4_FullHost(t *testing.T) {
	configMount := "ws-abc123-configs:/configs"
	hc := baseHostConfig("")
	originalBindCount := len(hc.Binds)

	cfg := WorkspaceConfig{
		WorkspaceID: "abc123",
		Tier:        4,
	}

	ApplyTierConfig(hc, cfg, configMount, "ws-abc123")

	// T4 must be privileged (inherits from T3)
	if !hc.Privileged {
		t.Error("T4: expected Privileged=true")
	}

	// T4 must have host PID (inherits from T3)
	if hc.PidMode != "host" {
		t.Errorf("T4: expected PidMode=host, got %q", hc.PidMode)
	}

	// T4 must have host network
	if hc.NetworkMode != "host" {
		t.Errorf("T4: expected NetworkMode=host, got %q", hc.NetworkMode)
	}

	// T4 should add Docker socket mount to existing binds
	expectedBindCount := originalBindCount + 1
	if len(hc.Binds) != expectedBindCount {
		t.Fatalf("T4: expected %d binds (original + docker socket), got %d: %v",
			expectedBindCount, len(hc.Binds), hc.Binds)
	}

	// Last bind should be the Docker socket
	dockerSocket := "/var/run/docker.sock:/var/run/docker.sock"
	lastBind := hc.Binds[len(hc.Binds)-1]
	if lastBind != dockerSocket {
		t.Errorf("T4: expected docker socket bind %q, got %q", dockerSocket, lastBind)
	}
}

func TestApplyTierConfig_UnknownTier_DefaultsToT2(t *testing.T) {
	configMount := "ws-abc123-configs:/configs"
	hc := baseHostConfig("")

	cfg := WorkspaceConfig{
		WorkspaceID: "abc123",
		Tier:        99, // Unknown tier
	}

	ApplyTierConfig(hc, cfg, configMount, "ws-abc123")

	// Unknown tiers should get T2 resource limits as a safe default
	expectedMemory := int64(512 * 1024 * 1024)
	if hc.Memory != expectedMemory {
		t.Errorf("Unknown tier: expected Memory=%d (512m), got %d", expectedMemory, hc.Memory)
	}

	expectedCPU := int64(1_000_000_000)
	if hc.NanoCPUs != expectedCPU {
		t.Errorf("Unknown tier: expected NanoCPUs=%d (1.0 CPU), got %d", expectedCPU, hc.NanoCPUs)
	}

	// Must NOT be privileged
	if hc.Privileged {
		t.Error("Unknown tier: must not be privileged")
	}
}

func TestApplyTierConfig_ZeroTier_DefaultsToT2(t *testing.T) {
	configMount := "ws-abc123-configs:/configs"
	hc := baseHostConfig("")

	cfg := WorkspaceConfig{
		WorkspaceID: "abc123",
		Tier:        0, // Unset / zero-value
	}

	ApplyTierConfig(hc, cfg, configMount, "ws-abc123")

	// Zero tier (default int value) should also get T2 resource limits
	expectedMemory := int64(512 * 1024 * 1024)
	if hc.Memory != expectedMemory {
		t.Errorf("Tier 0: expected Memory=%d, got %d", expectedMemory, hc.Memory)
	}
	if hc.Privileged {
		t.Error("Tier 0: must not be privileged")
	}
}

// TestTierEscalation verifies that lower tiers don't accidentally
// get higher-tier privileges.
func TestTierEscalation(t *testing.T) {
	tests := []struct {
		tier              int
		expectPrivileged  bool
		expectHostNetwork bool
		expectHostPID     bool
		expectReadonly    bool
	}{
		{1, false, false, false, true},
		{2, false, false, false, false},
		{3, true, false, true, false},
		{4, true, true, true, false},
	}

	for _, tt := range tests {
		t.Run("tier_"+string(rune('0'+tt.tier)), func(t *testing.T) {
			configMount := "ws-test-configs:/configs"
			hc := baseHostConfig("")
			cfg := WorkspaceConfig{
				WorkspaceID: "test",
				Tier:        tt.tier,
			}

			ApplyTierConfig(hc, cfg, configMount, "ws-test")

			if hc.Privileged != tt.expectPrivileged {
				t.Errorf("Tier %d: Privileged=%v, want %v", tt.tier, hc.Privileged, tt.expectPrivileged)
			}
			if (hc.NetworkMode == "host") != tt.expectHostNetwork {
				t.Errorf("Tier %d: NetworkMode=%q, wantHost=%v", tt.tier, hc.NetworkMode, tt.expectHostNetwork)
			}
			if (hc.PidMode == "host") != tt.expectHostPID {
				t.Errorf("Tier %d: PidMode=%q, wantHost=%v", tt.tier, hc.PidMode, tt.expectHostPID)
			}
			if hc.ReadonlyRootfs != tt.expectReadonly {
				t.Errorf("Tier %d: ReadonlyRootfs=%v, want %v", tt.tier, hc.ReadonlyRootfs, tt.expectReadonly)
			}
		})
	}
}

// TestContainerName verifies the naming convention.
func TestContainerName(t *testing.T) {
	tests := []struct {
		id   string
		want string
	}{
		{"short", "ws-short"},
		{"exactly12ch", "ws-exactly12ch"},
		{"longer-than-twelve-characters", "ws-longer-than-twelve-characters"},
		{"abc", "ws-abc"},
	}

	for _, tt := range tests {
		got := ContainerName(tt.id)
		if got != tt.want {
			t.Errorf("ContainerName(%q) = %q, want %q", tt.id, got, tt.want)
		}
	}
}

// TestContainerName_DistinctSamePrefix12 is a regression guard for KI-013:
// two UUIDs sharing the same first 12 characters must produce distinct
// container names (the old 12-char truncation caused collisions).
func TestContainerName_DistinctSamePrefix12(t *testing.T) {
	id1 := "123456789abc-4def-1234-567890abcdef"
	id2 := "123456789abc-4def-1234-567890abcdf0"
	if ContainerName(id1) == ContainerName(id2) {
		t.Fatalf("ContainerName must differ for same-first-12 UUIDs: both = %q", ContainerName(id1))
	}
}

// TestConfigVolumeName verifies config volume naming.
func TestConfigVolumeName(t *testing.T) {
	tests := []struct {
		id   string
		want string
	}{
		{"short", "ws-short-configs"},
		{"exactly12ch", "ws-exactly12ch-configs"},
		{"longer-than-twelve-characters", "ws-longer-than-twelve-characters-configs"},
		{"abc", "ws-abc-configs"},
	}

	for _, tt := range tests {
		got := ConfigVolumeName(tt.id)
		if got != tt.want {
			t.Errorf("ConfigVolumeName(%q) = %q, want %q", tt.id, got, tt.want)
		}
	}
}

// TestConfigVolumeName_DistinctSamePrefix12 is a regression guard for KI-013.
func TestConfigVolumeName_DistinctSamePrefix12(t *testing.T) {
	id1 := "123456789abc-4def-1234-567890abcdef"
	id2 := "123456789abc-4def-1234-567890abcdf0"
	if ConfigVolumeName(id1) == ConfigVolumeName(id2) {
		t.Fatalf("ConfigVolumeName must differ for same-first-12 UUIDs: both = %q", ConfigVolumeName(id1))
	}
}

// TestWorkspaceVolumeName verifies workspace volume naming.
func TestWorkspaceVolumeName(t *testing.T) {
	tests := []struct {
		id   string
		want string
	}{
		{"short", "ws-short-workspace"},
		{"exactly12ch", "ws-exactly12ch-workspace"},
		{"longer-than-twelve-characters", "ws-longer-than-twelve-characters-workspace"},
		{"abc", "ws-abc-workspace"},
	}
	for _, tt := range tests {
		got := WorkspaceVolumeName(tt.id)
		if got != tt.want {
			t.Errorf("WorkspaceVolumeName(%q) = %q, want %q", tt.id, got, tt.want)
		}
	}
}

// TestWorkspaceVolumeName_DistinctSamePrefix12 is a regression guard for KI-013.
func TestWorkspaceVolumeName_DistinctSamePrefix12(t *testing.T) {
	id1 := "123456789abc-4def-1234-567890abcdef"
	id2 := "123456789abc-4def-1234-567890abcdf0"
	if WorkspaceVolumeName(id1) == WorkspaceVolumeName(id2) {
		t.Fatalf("WorkspaceVolumeName must differ for same-first-12 UUIDs: both = %q", WorkspaceVolumeName(id1))
	}
}

// ---------- #12 — claude-sessions volume naming ----------

// TestClaudeSessionVolumeName_Deterministic: same ID → same volume name, and
// the name follows the ws-<id>-claude-sessions shape used everywhere
// else in the provisioner.
func TestClaudeSessionVolumeName_Deterministic(t *testing.T) {
	tests := []struct {
		id   string
		want string
	}{
		{"short", "ws-short-claude-sessions"},
		{"exactly12ch", "ws-exactly12ch-claude-sessions"},
		{"longer-than-twelve-characters", "ws-longer-than-twelve-characters-claude-sessions"},
		{"abc", "ws-abc-claude-sessions"},
	}
	for _, tt := range tests {
		got := ClaudeSessionVolumeName(tt.id)
		if got != tt.want {
			t.Errorf("ClaudeSessionVolumeName(%q) = %q, want %q", tt.id, got, tt.want)
		}
		// Deterministic: calling twice returns the same value.
		if again := ClaudeSessionVolumeName(tt.id); again != got {
			t.Errorf("ClaudeSessionVolumeName not deterministic: %q vs %q", got, again)
		}
	}
}

// TestClaudeSessionVolumeName_DistinctSamePrefix12 is a regression guard for KI-013.
func TestClaudeSessionVolumeName_DistinctSamePrefix12(t *testing.T) {
	id1 := "123456789abc-4def-1234-567890abcdef"
	id2 := "123456789abc-4def-1234-567890abcdf0"
	if ClaudeSessionVolumeName(id1) == ClaudeSessionVolumeName(id2) {
		t.Fatalf("ClaudeSessionVolumeName must differ for same-first-12 UUIDs: both = %q", ClaudeSessionVolumeName(id1))
	}
}

// TestClaudeSessionVolumeName_DistinctFromConfig ensures we never alias the
// claude-sessions volume onto the config volume (deleting one must not wipe
// the other in RemoveVolume's cleanup path).
func TestClaudeSessionVolumeName_DistinctFromConfig(t *testing.T) {
	id := "abc123def456"
	if ClaudeSessionVolumeName(id) == ConfigVolumeName(id) {
		t.Fatalf("claude-sessions and config volume names must differ (both = %q)", ConfigVolumeName(id))
	}
}

// TestWorkspaceConfig_ResetClaudeSessionFieldPresent is a compile-time check
// that the ResetClaudeSession knob exists on WorkspaceConfig so handlers can
// plumb ?reset=true through to the provisioner without a struct tag dance.
func TestWorkspaceConfig_ResetClaudeSessionFieldPresent(t *testing.T) {
	cfg := WorkspaceConfig{WorkspaceID: "x", Runtime: "claude-code", ResetClaudeSession: true}
	if !cfg.ResetClaudeSession {
		t.Fatal("ResetClaudeSession should round-trip through struct literal")
	}
}

// ---------- selectImage (#2272 layer 1) ----------

// TestSelectImage_PrefersExplicitImage: when CP (the SSOT for runtime image
// pins under RFC internal#617 / task #335) supplied a digest pin via
// cfg.Image, selectImage must honor it and ignore the cfg.Runtime → :latest
// fallback. This is the load-bearing invariant for digest pinning — if it
// ever silently reverts to :latest, we lose the "one bad publish doesn't
// break every workspace" guarantee.
func TestSelectImage_PrefersExplicitImage(t *testing.T) {
	pinned := "ghcr.io/molecule-ai/workspace-template-claude-code@sha256:3d6761a97ed07d7d33cfc19a8fbab81175d9d9179618d493dbc00c5f7ef076a3"
	got, err := selectImage(WorkspaceConfig{Runtime: "claude-code", Image: pinned})
	if err != nil {
		t.Fatalf("selectImage with cfg.Image=pinned: unexpected error %v", err)
	}
	if got != pinned {
		t.Errorf("selectImage with cfg.Image=pinned: got %q, want %q", got, pinned)
	}
}

// TestSelectImage_FallsBackToRuntimeMap: handler returned "" (no pin or
// pin lookup deliberately bypassed via WORKSPACE_IMAGE_LOCAL_OVERRIDE).
// selectImage must use the legacy runtime→:latest map.
func TestSelectImage_FallsBackToRuntimeMap(t *testing.T) {
	got, err := selectImage(WorkspaceConfig{Runtime: "claude-code", Image: ""})
	if err != nil {
		t.Fatalf("selectImage with empty Image: unexpected error %v", err)
	}
	want := RuntimeImages["claude-code"]
	if got != want {
		t.Errorf("selectImage with empty Image: got %q, want %q", got, want)
	}
}

// TestSelectImage_NamedUnresolvableRuntimeRejects pins the fail-closed
// contract (RFC internal#483 / security review 4269 /
// feedback_platform_must_hardgate_base_contract): a NAMED runtime with no
// resolvable image must reject with ErrUnresolvableRuntime, NOT silently
// substitute DefaultImage. Pre-fix this returned claude-code — a user asking
// for a removed runtime silently got a claude-code container. The named
// legacy runtime below is the concrete regression from the
// security finding.
func TestSelectImage_NamedUnresolvableRuntimeRejects(t *testing.T) {
	for _, rt := range []string{"no-such-runtime", "legacy-runtime-a", "legacy-runtime-b"} {
		got, err := selectImage(WorkspaceConfig{Runtime: rt})
		if !errors.Is(err, ErrUnresolvableRuntime) {
			t.Errorf("selectImage(%q): got err %v, want ErrUnresolvableRuntime", rt, err)
		}
		if got != "" {
			t.Errorf("selectImage(%q): got image %q, want \"\" on reject", rt, got)
		}
		if err != nil && !strings.Contains(err.Error(), rt) {
			t.Errorf("selectImage(%q): error must name the offending runtime, got %v", rt, err)
		}
	}
}

// TestSelectImage_EmptyRuntimeFallsBackToDefault: same invariant for the
// no-runtime-supplied path (legacy callers / older handler code).
func TestSelectImage_EmptyRuntimeFallsBackToDefault(t *testing.T) {
	got, err := selectImage(WorkspaceConfig{})
	if err != nil {
		t.Fatalf("selectImage with zero cfg: unexpected error %v (empty runtime is a legitimate DefaultImage path)", err)
	}
	if got != DefaultImage {
		t.Errorf("selectImage with zero cfg: got %q, want DefaultImage %q", got, DefaultImage)
	}
}

// TestWorkspaceConfig_ImageFieldPresent compile-time-pins the Image field
// so the handler→provisioner contract for digest-pinned image refs can't
// be silently removed by a future struct refactor (#2272).
func TestWorkspaceConfig_ImageFieldPresent(t *testing.T) {
	cfg := WorkspaceConfig{Image: "ghcr.io/example@sha256:abc"}
	if cfg.Image == "" {
		t.Fatal("Image should round-trip through struct literal")
	}
}

// ---------- buildContainerEnv — #67 MOLECULE_URL injection ----------

func TestBuildContainerEnv_InjectsBothPlatformURLAndMoleculeAIURL(t *testing.T) {
	cfg := WorkspaceConfig{
		WorkspaceID: "ws-abc123",
		PlatformURL: "http://host.docker.internal:8080",
		Tier:        2,
	}
	env := buildContainerEnv(cfg)

	wantPairs := map[string]string{
		"WORKSPACE_ID":          "ws-abc123",
		"WORKSPACE_CONFIG_PATH": "/configs",
		"PLATFORM_URL":          "http://host.docker.internal:8080",
		"MOLECULE_URL":          "http://host.docker.internal:8080",
		"TIER":                  "2",
		"PLUGINS_DIR":           "/plugins",
	}
	for k, wantV := range wantPairs {
		want := k + "=" + wantV
		found := false
		for _, e := range env {
			if e == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected env to contain %q, got %v", want, env)
		}
	}
}

func TestBuildContainerEnv_InjectsPYTHONPATH(t *testing.T) {
	// Standalone workspace-template repos COPY adapter.py to /app and rely on
	// `import adapter` resolving via PYTHONPATH. molecule-runtime is a pip
	// console_script entry, so cwd isn't on sys.path automatically. The
	// provisioner injects PYTHONPATH=/app so every adapter image works
	// without per-template Dockerfile patching. See workspace-runtime#1
	// for the runtime-side bug this works around.
	cfg := WorkspaceConfig{WorkspaceID: "ws-x", PlatformURL: "http://x", Tier: 1}
	env := buildContainerEnv(cfg)
	want := "PYTHONPATH=/app"
	for _, e := range env {
		if e == want {
			return
		}
	}
	t.Errorf("expected env to contain %q, got %v", want, env)
}

func TestBuildContainerEnv_WorkspaceEnvVarsCanOverridePYTHONPATH(t *testing.T) {
	// Operator escape hatch: a per-workspace EnvVars["PYTHONPATH"] = "/custom"
	// MUST appear AFTER the default in the env slice so Docker uses the
	// later one. Without this, an operator who needs a custom path can't
	// override the provisioner default.
	cfg := WorkspaceConfig{
		WorkspaceID: "ws-x",
		PlatformURL: "http://x",
		Tier:        1,
		EnvVars:     map[string]string{"PYTHONPATH": "/custom:/app"},
	}
	env := buildContainerEnv(cfg)
	defaultIdx, customIdx := -1, -1
	for i, e := range env {
		if e == "PYTHONPATH=/app" {
			defaultIdx = i
		}
		if e == "PYTHONPATH=/custom:/app" {
			customIdx = i
		}
	}
	if defaultIdx < 0 || customIdx < 0 {
		t.Fatalf("expected both default and custom PYTHONPATH entries, got %v", env)
	}
	if customIdx < defaultIdx {
		t.Errorf("custom PYTHONPATH (idx=%d) must come AFTER default (idx=%d) so Docker takes the operator override", customIdx, defaultIdx)
	}
}

func TestBuildContainerEnv_MoleculeAIURLAlwaysMatchesPlatformURL(t *testing.T) {
	// Regression guard: MOLECULE_URL must never drift from PLATFORM_URL —
	// if someone changes one they must change the other. This test pins
	// the invariant. See #67.
	for _, url := range []string{
		"http://localhost:8080",
		"http://host.docker.internal:8080",
		"http://platform:8080",
		"https://molecule.example.com",
	} {
		cfg := WorkspaceConfig{WorkspaceID: "ws-x", PlatformURL: url, Tier: 1}
		env := buildContainerEnv(cfg)
		var pURL, sURL string
		for _, e := range env {
			if strings.HasPrefix(e, "PLATFORM_URL=") {
				pURL = strings.TrimPrefix(e, "PLATFORM_URL=")
			}
			if strings.HasPrefix(e, "MOLECULE_URL=") {
				sURL = strings.TrimPrefix(e, "MOLECULE_URL=")
			}
		}
		if pURL != sURL {
			t.Errorf("PLATFORM_URL (%q) must match MOLECULE_URL (%q)", pURL, sURL)
		}
		if pURL != url {
			t.Errorf("expected PLATFORM_URL=%q, got %q", url, pURL)
		}
	}
}

func TestBuildContainerEnv_CustomEnvVarsAppended(t *testing.T) {
	// NOTE: this test previously asserted GITHUB_TOKEN passed through
	// verbatim. That assertion encoded the forensic #145 latent leak as
	// expected behavior. Post-guard, ordinary custom env still flows but
	// SCM-write credentials are stripped — see
	// TestBuildContainerEnv_StripsSCMWriteTokens for the negative assertion.
	cfg := WorkspaceConfig{
		WorkspaceID: "ws-x",
		PlatformURL: "http://localhost:8080",
		EnvVars:     map[string]string{"CUSTOM": "value", "ANTHROPIC_API_KEY": "sk-not-an-scm-token"},
	}
	env := buildContainerEnv(cfg)
	seen := map[string]string{}
	for _, e := range env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			seen[parts[0]] = parts[1]
		}
	}
	if seen["CUSTOM"] != "value" {
		t.Errorf("CUSTOM env missing, got env=%v", env)
	}
	if seen["ANTHROPIC_API_KEY"] != "sk-not-an-scm-token" {
		t.Errorf("non-SCM custom env must still pass through, got env=%v", env)
	}
	// Built-in defaults still present
	if seen["MOLECULE_URL"] == "" {
		t.Errorf("MOLECULE_URL must still be set alongside custom envs")
	}
}

// ---------- forensic #145: SCM-write-token denylist guard ----------

// TestBuildContainerEnv_StripsSCMWriteTokens is the core negative
// assertion: a tenant workspace env constructed via buildContainerEnv MUST
// NOT contain any Git SCM *write* credential, regardless of how it got into
// cfg.EnvVars. This proves the two-eyes review gate stays structurally
// self-bypass-proof — an agent in-container has no merge/approve token to
// act on a forged approval. See forensic #145.
//
// This test FAILS on the pre-guard code (where buildContainerEnv passed
// cfg.EnvVars through verbatim) and PASSES once the denylist filter is in
// place — i.e. the guard is proven by construction, not by environment
// accident.
func TestBuildContainerEnv_StripsSCMWriteTokens(t *testing.T) {
	// GH_TOKEN and GITHUB_TOKEN are preserved when explicitly set (#1687)
	// because they win over the GH_PAT alias. The unconditional strip list
	// therefore excludes them; see TestBuildContainerEnv_GHPATAliasPrecedence
	// for the positive assertion.
	scmTokens := []string{
		"GITEA_TOKEN", "GITLAB_TOKEN", "GL_TOKEN", "BITBUCKET_TOKEN",
	}

	t.Run("normal path — SCM tokens explicitly set in EnvVars", func(t *testing.T) {
		envVars := map[string]string{"CUSTOM": "ok", "ANTHROPIC_API_KEY": "sk-keep"}
		for _, k := range scmTokens {
			envVars[k] = "leaked-write-credential-" + k
		}
		// Explicit GH_TOKEN / GITHUB_TOKEN are now preserved (#1687).
		envVars["GH_TOKEN"] = "explicit-gh-token"
		envVars["GITHUB_TOKEN"] = "explicit-github-token"
		cfg := WorkspaceConfig{
			WorkspaceID: "ws-tenant",
			PlatformURL: "http://localhost:8080",
			Tier:        2,
			EnvVars:     envVars,
		}
		assertNoSCMWriteToken(t, buildContainerEnv(cfg), scmTokens)

		// Sanity: non-SCM custom env is NOT collateral-damaged by the filter.
		if !envContains(buildContainerEnv(cfg), "CUSTOM=ok") {
			t.Errorf("filter must not strip non-SCM custom env")
		}
		if !envContains(buildContainerEnv(cfg), "ANTHROPIC_API_KEY=sk-keep") {
			t.Errorf("filter must not strip non-SCM API keys")
		}
		// Explicit GH tokens must be preserved (not stripped).
		if !envContains(buildContainerEnv(cfg), "GH_TOKEN=explicit-gh-token") {
			t.Errorf("explicit GH_TOKEN must be preserved")
		}
		if !envContains(buildContainerEnv(cfg), "GITHUB_TOKEN=explicit-github-token") {
			t.Errorf("explicit GITHUB_TOKEN must be preserved")
		}
	})

	t.Run("persona-file path — simulates loadPersonaEnvFile merge", func(t *testing.T) {
		// The latent path: handlers.loadPersonaEnvFile() merges a per-role
		// persona env file (carrying GITEA_USER, GITEA_TOKEN, …) into the
		// workspace env map when MOLECULE_PERSONA_ROOT is set on a tenant
		// host. We can't invoke that cross-package helper here, but its
		// observable effect is exactly "a GITEA_TOKEN appears in
		// cfg.EnvVars". Constructing that condition directly proves the
		// guard holds even if the latent path becomes reachable.
		cfg := WorkspaceConfig{
			WorkspaceID: "ws-tenant",
			PlatformURL: "http://localhost:8080",
			Tier:        2,
			EnvVars: map[string]string{
				// Persona identity fields that are SAFE to keep (read-only
				// identity, not a write credential):
				"GITEA_USER":       "backend-engineer",
				"GITEA_USER_EMAIL": "backend-engineer@agents.moleculesai.app",
				// The credential that must be stripped:
				"GITEA_TOKEN":        "persona-merged-write-pat",
				"GITEA_TOKEN_SCOPES": "write:repository",
			},
		}
		got := buildContainerEnv(cfg)
		assertNoSCMWriteToken(t, got, scmTokens)
		// Non-credential persona identity may still flow through — only the
		// write token is the denied surface.
		if !envContains(got, "GITEA_USER=backend-engineer") {
			t.Errorf("non-credential persona identity (GITEA_USER) should not be stripped")
		}
	})
}

// TestCPProvisionerEnv_StripsSCMWriteTokens covers the tenant-EC2 path:
// CPProvisioner.Start builds the env map the control plane forwards to the
// EC2 workspace container. The same forensic #145 denylist must hold there.
func TestCPProvisionerEnv_StripsSCMWriteTokens(t *testing.T) {
	// isSCMWriteTokenKey is the single source of truth shared by both
	// buildContainerEnv (local Docker) and CPProvisioner.Start (tenant EC2).
	// Assert it classifies every known SCM-write var as denied and leaves
	// ordinary / read-only-identity vars alone.
	for _, k := range []string{
		"GITEA_TOKEN", "GITHUB_TOKEN", "GH_TOKEN",
		"GITLAB_TOKEN", "GL_TOKEN", "BITBUCKET_TOKEN",
	} {
		if !isSCMWriteTokenKey(k) {
			t.Errorf("isSCMWriteTokenKey(%q) = false, want true (SCM-write credential must be denied)", k)
		}
	}
	for _, k := range []string{
		"GITEA_USER", "GITEA_USER_EMAIL", "ANTHROPIC_API_KEY",
		"CUSTOM", "PLATFORM_URL", "ADMIN_TOKEN", "",
	} {
		if isSCMWriteTokenKey(k) {
			t.Errorf("isSCMWriteTokenKey(%q) = true, want false (must not over-strip non-SCM env)", k)
		}
	}
}

// TestBuildCPTenantEnv_ForensicGuardProvenance pins the forensic #145
// provenance-aware guard on the tenant-EC2 path (CPProvisioner.Start →
// buildCPTenantEnv). The guard strips SCM-write tokens UNLESS they are
// positively workspace-authored (present in cfg.WorkspaceSecretKeys). Each
// security invariant from the fix spec gets a row:
//
//  1. SCM token ONLY in global_secrets (in EnvVars, NOT WorkspaceSecretKeys) → STRIPPED.
//  2. SCM token persona/mutator-injected (in EnvVars, NOT WorkspaceSecretKeys) → STRIPPED.
//  3. SCM token authored via workspace_secrets (in EnvVars AND WorkspaceSecretKeys) → PRESERVED.
//  4. WorkspaceSecretKeys == nil → ALL SCM-write tokens STRIPPED (fail-safe).
//  5. Non-SCM keys pass through unchanged regardless of the set.
func TestBuildCPTenantEnv_ForensicGuardProvenance(t *testing.T) {
	const tok = "gitea-write-pat-value"

	tests := []struct {
		name             string
		envVars          map[string]string
		workspaceKeys    map[string]struct{}
		wantPreserved    map[string]string // key→expected value that MUST survive
		wantStrippedKeys []string          // keys that MUST be absent from the result
	}{
		{
			name:             "invariant 1 — global_secrets-only SCM token is stripped",
			envVars:          map[string]string{"GITEA_TOKEN": tok},
			workspaceKeys:    map[string]struct{}{}, // not workspace-authored
			wantStrippedKeys: []string{"GITEA_TOKEN"},
		},
		{
			name:    "invariant 2 — persona/mutator-injected SCM token is stripped",
			envVars: map[string]string{"GITEA_TOKEN": "persona-merged-write-pat"},
			// Persona/mutator merges into EnvVars but NEVER into the
			// workspace_secrets provenance set — this is the exact bleed the
			// guard exists for and MUST stay stripped.
			workspaceKeys:    map[string]struct{}{"ANTHROPIC_API_KEY": {}},
			wantStrippedKeys: []string{"GITEA_TOKEN"},
		},
		{
			name:          "invariant 3 — workspace_secrets-authored SCM token is preserved",
			envVars:       map[string]string{"GITEA_TOKEN": tok},
			workspaceKeys: map[string]struct{}{"GITEA_TOKEN": {}},
			wantPreserved: map[string]string{"GITEA_TOKEN": tok},
		},
		{
			name: "invariant 4 — nil provenance map strips ALL SCM-write tokens (fail-safe)",
			envVars: map[string]string{
				"GITEA_TOKEN":     tok,
				"GITHUB_TOKEN":    "gh",
				"GH_TOKEN":        "gh2",
				"GITLAB_TOKEN":    "gl",
				"GL_TOKEN":        "gl2",
				"BITBUCKET_TOKEN": "bb",
			},
			workspaceKeys: nil, // missing provenance map must never leak
			wantStrippedKeys: []string{
				"GITEA_TOKEN", "GITHUB_TOKEN", "GH_TOKEN",
				"GITLAB_TOKEN", "GL_TOKEN", "BITBUCKET_TOKEN",
			},
		},
		{
			name: "invariant 5 — non-SCM keys pass through regardless of the set",
			envVars: map[string]string{
				"ANTHROPIC_API_KEY": "sk-keep",
				"CUSTOM":            "ok",
				"GITEA_USER":        "reviewer-agent", // read-only identity, not a write token
				"GITEA_TOKEN":       tok,              // SCM-write, NOT workspace-authored → stripped
			},
			workspaceKeys: map[string]struct{}{}, // empty → GITEA_TOKEN not exempt
			wantPreserved: map[string]string{
				"ANTHROPIC_API_KEY": "sk-keep",
				"CUSTOM":            "ok",
				"GITEA_USER":        "reviewer-agent",
			},
			wantStrippedKeys: []string{"GITEA_TOKEN"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := WorkspaceConfig{
				WorkspaceID:         "ws-tenant",
				PlatformURL:         "http://localhost:8080",
				Tier:                2,
				EnvVars:             tt.envVars,
				WorkspaceSecretKeys: tt.workspaceKeys,
			}
			// adminToken empty so the guard's behaviour is isolated; ADMIN_TOKEN
			// injection is covered separately below.
			got := buildCPTenantEnv(cfg, "")

			for _, k := range tt.wantStrippedKeys {
				if v, ok := got[k]; ok {
					t.Errorf("SCM-write credential %q leaked into tenant env (forensic #145 invariant violated): value=%q", k, v)
				}
			}
			for k, want := range tt.wantPreserved {
				if got[k] != want {
					t.Errorf("key %q = %q; want preserved value %q", k, got[k], want)
				}
			}
		})
	}
}

// TestBuildCPTenantEnv_AdminTokenInjected asserts ADMIN_TOKEN is injected when
// the provisioner carries one, and is never subject to the SCM-write strip.
func TestBuildCPTenantEnv_AdminTokenInjected(t *testing.T) {
	cfg := WorkspaceConfig{
		WorkspaceID: "ws-tenant",
		EnvVars:     map[string]string{"GITEA_TOKEN": "stripme"},
	}
	got := buildCPTenantEnv(cfg, "admin-secret")
	if got["ADMIN_TOKEN"] != "admin-secret" {
		t.Errorf("ADMIN_TOKEN = %q; want admin-secret", got["ADMIN_TOKEN"])
	}
	if _, ok := got["GITEA_TOKEN"]; ok {
		t.Errorf("GITEA_TOKEN must still be stripped alongside ADMIN_TOKEN injection")
	}
}

// TestBuildContainerEnv_GHPATAliasPrecedence asserts that explicit GH_TOKEN /
// GITHUB_TOKEN in workspace secrets win over the GH_PAT alias (#1687 CR2
// review_id=5646). The alias must only inject a key when it was NOT explicitly
// set.
func TestBuildContainerEnv_GHPATAliasPrecedence(t *testing.T) {
	pat := "ghp_pat_from_secrets"
	explicitGH := "gh_explicit_token"
	explicitGitHub := "github_explicit_token"

	t.Run("GH_PAT alone → alias both", func(t *testing.T) {
		cfg := WorkspaceConfig{
			WorkspaceID: "ws-x",
			PlatformURL: "http://localhost:8080",
			EnvVars:     map[string]string{"GH_PAT": pat},
		}
		env := buildContainerEnv(cfg)
		if !envContains(env, "GH_TOKEN="+pat) {
			t.Errorf("GH_PAT alias must set GH_TOKEN, got %v", env)
		}
		if !envContains(env, "GITHUB_TOKEN="+pat) {
			t.Errorf("GH_PAT alias must set GITHUB_TOKEN, got %v", env)
		}
	})

	t.Run("explicit GH_TOKEN wins over GH_PAT alias", func(t *testing.T) {
		cfg := WorkspaceConfig{
			WorkspaceID: "ws-x",
			PlatformURL: "http://localhost:8080",
			EnvVars: map[string]string{
				"GH_PAT":   pat,
				"GH_TOKEN": explicitGH,
			},
		}
		env := buildContainerEnv(cfg)
		if envContains(env, "GH_TOKEN="+pat) {
			t.Errorf("explicit GH_TOKEN must win over GH_PAT alias, got GH_TOKEN=%q", pat)
		}
		if !envContains(env, "GH_TOKEN="+explicitGH) {
			t.Errorf("explicit GH_TOKEN must be preserved, got %v", env)
		}
	})

	t.Run("explicit GITHUB_TOKEN wins over GH_PAT alias", func(t *testing.T) {
		cfg := WorkspaceConfig{
			WorkspaceID: "ws-x",
			PlatformURL: "http://localhost:8080",
			EnvVars: map[string]string{
				"GH_PAT":       pat,
				"GITHUB_TOKEN": explicitGitHub,
			},
		}
		env := buildContainerEnv(cfg)
		if envContains(env, "GITHUB_TOKEN="+pat) {
			t.Errorf("explicit GITHUB_TOKEN must win over GH_PAT alias, got GITHUB_TOKEN=%q", pat)
		}
		if !envContains(env, "GITHUB_TOKEN="+explicitGitHub) {
			t.Errorf("explicit GITHUB_TOKEN must be preserved, got %v", env)
		}
	})

	t.Run("explicit both → both preserved, no alias", func(t *testing.T) {
		cfg := WorkspaceConfig{
			WorkspaceID: "ws-x",
			PlatformURL: "http://localhost:8080",
			EnvVars: map[string]string{
				"GH_PAT":       pat,
				"GH_TOKEN":     explicitGH,
				"GITHUB_TOKEN": explicitGitHub,
			},
		}
		env := buildContainerEnv(cfg)
		if envContains(env, "GH_TOKEN="+pat) {
			t.Errorf("explicit GH_TOKEN must win, got alias value %q", pat)
		}
		if envContains(env, "GITHUB_TOKEN="+pat) {
			t.Errorf("explicit GITHUB_TOKEN must win, got alias value %q", pat)
		}
		if !envContains(env, "GH_TOKEN="+explicitGH) {
			t.Errorf("explicit GH_TOKEN must be preserved, got %v", env)
		}
		if !envContains(env, "GITHUB_TOKEN="+explicitGitHub) {
			t.Errorf("explicit GITHUB_TOKEN must be preserved, got %v", env)
		}
	})

	t.Run("no GH_PAT → no alias injected", func(t *testing.T) {
		cfg := WorkspaceConfig{
			WorkspaceID: "ws-x",
			PlatformURL: "http://localhost:8080",
			EnvVars:     map[string]string{"OTHER": "ok"},
		}
		env := buildContainerEnv(cfg)
		for _, e := range env {
			if strings.HasPrefix(e, "GH_TOKEN=") || strings.HasPrefix(e, "GITHUB_TOKEN=") {
				t.Errorf("no GH_PAT present → no alias should be injected, got %q", e)
			}
		}
	})
}

func assertNoSCMWriteToken(t *testing.T, env []string, scmTokens []string) {
	t.Helper()
	for _, e := range env {
		key := e
		if i := strings.IndexByte(e, '='); i >= 0 {
			key = e[:i]
		}
		for _, banned := range scmTokens {
			if key == banned {
				t.Errorf("SCM-write credential %q leaked into workspace env (forensic #145 invariant violated): %q", banned, e)
			}
		}
	}
}

func envContains(env []string, want string) bool {
	for _, e := range env {
		if e == want {
			return true
		}
	}
	return false
}

// ---------- buildWorkspaceMount — #65 workspace_access ----------

func TestBuildWorkspaceMount_SelectionMatrix(t *testing.T) {
	cases := []struct {
		name       string
		path       string
		access     string
		wantSuffix string // suffix of the mount string for partial match
		wantBind   bool   // true if bind-mount (starts with path), false if named volume
	}{
		{"empty path + none → named volume", "", "none", ":/workspace", false},
		{"empty path + empty access → named volume", "", "", ":/workspace", false},
		{"host path + read_only → :ro bind", "/Users/x/repo", "read_only", "/Users/x/repo:/workspace:ro", true},
		{"host path + read_write → rw bind", "/Users/x/repo", "read_write", "/Users/x/repo:/workspace", true},
		{"host path + none → named volume (opts out of mount)", "/Users/x/repo", "none", ":/workspace", false},
		{"host path + empty access → default rw bind", "/Users/x/repo", "", "/Users/x/repo:/workspace", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := WorkspaceConfig{
				WorkspaceID:     "abc123",
				WorkspacePath:   tc.path,
				WorkspaceAccess: tc.access,
			}
			got := buildWorkspaceMount(cfg)
			if tc.wantBind {
				if got != tc.wantSuffix {
					t.Errorf("want exact %q, got %q", tc.wantSuffix, got)
				}
			} else {
				// Named volume: should NOT start with tc.path, should end in :/workspace
				if strings.HasPrefix(got, tc.path+":") && tc.path != "" {
					t.Errorf("expected named volume (not bind), got %q", got)
				}
				if !strings.HasSuffix(got, tc.wantSuffix) {
					t.Errorf("want suffix %q, got %q", tc.wantSuffix, got)
				}
			}
		})
	}
}

func TestValidateWorkspaceAccess(t *testing.T) {
	cases := []struct {
		name    string
		access  string
		path    string
		wantErr bool
	}{
		{"none + empty path", "none", "", false},
		{"empty access + empty path", "", "", false},
		{"read_only + host path", "read_only", "/Users/x/repo", false},
		{"read_write + host path", "read_write", "/Users/x/repo", false},
		{"read_only + empty path (error)", "read_only", "", true},
		{"read_write + empty path (error)", "read_write", "", true},
		{"unknown value (error)", "wildcard", "/Users/x/repo", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateWorkspaceAccess(tc.access, tc.path)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateWorkspaceAccess(%q, %q) = %v, wantErr %v",
					tc.access, tc.path, err, tc.wantErr)
			}
		})
	}
}

// ---------- isImageNotFoundErr (issue #117) ----------

func TestIsImageNotFoundErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"moby no such image", fmtErr(`Error response from daemon: No such image: workspace-template:openclaw`), true},
		{"no such image lowercase", fmtErr(`error: no such image: foo:bar`), true},
		{"image not found", fmtErr(`Error: image "workspace-template:hermes" not found`), true},
		{"generic not found without image", fmtErr(`container not found`), false},
		{"unrelated error", fmtErr(`connection refused`), false},
		{"permission denied", fmtErr(`permission denied`), false},
	}
	for _, tc := range cases {
		got := isImageNotFoundErr(tc.err)
		if got != tc.want {
			t.Errorf("%s: isImageNotFoundErr(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}

// fmtErr builds a plain error for table-driven tests without pulling in fmt.
type testErr string

func (e testErr) Error() string { return string(e) }

func fmtErr(s string) error { return testErr(s) }

// ---------- runtimeTagFromImage (issue #117) ----------

func TestRuntimeTagFromImage(t *testing.T) {
	cases := map[string]string{
		// Legacy local-build form (still supported for `docker build -t
		// workspace-template:<runtime>` dev loops).
		"workspace-template:openclaw":    "openclaw",
		"workspace-template:claude-code": "claude-code",
		"workspace-template:base":        "base",
		// Current GHCR form produced by molecule-ci's publish-template-image
		// workflow and consumed by RuntimeImages.
		"ghcr.io/molecule-ai/workspace-template-hermes:latest":           "hermes",
		"ghcr.io/molecule-ai/workspace-template-claude-code:latest":      "claude-code",
		"ghcr.io/molecule-ai/workspace-template-claude-code:sha-abc1234": "claude-code",
		// Fallbacks for non-standard shapes
		"myregistry.io/foo:v1.2": "v1.2",
		"no-colon-at-all":        "no-colon-at-all",
		// Edge: trailing colon — use whole string (tag is empty)
		"foo:": "foo:",
	}
	for in, want := range cases {
		got := runtimeTagFromImage(in)
		if got != want {
			t.Errorf("runtimeTagFromImage(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---------- imageTagIsMoving (task #215) ----------

// TestImageTagIsMoving pins the moving-tag classifier. The classifier
// gates whether Start() forces a re-pull on a local-cache hit — get
// the classification wrong on the "moving" side and we waste bandwidth
// on every provision; get it wrong on the "pinned" side and the fleet
// silently sticks on a stale `:latest` snapshot (the bug class this
// task closes).
func TestImageTagIsMoving(t *testing.T) {
	cases := []struct {
		name  string
		image string
		want  bool
	}{
		// Bare references default to :latest at the registry level.
		{"bare repo no tag", "ghcr.io/molecule-ai/workspace-template-hermes", true},
		{"bare local image no tag", "workspace-template", true},

		// Explicit moving tags.
		{"explicit latest", "ghcr.io/molecule-ai/workspace-template-hermes:latest", true},
		{"explicit staging", "ghcr.io/molecule-ai/workspace-template-hermes:staging", true},
		{"explicit main", "ghcr.io/molecule-ai/workspace-template-hermes:main", true},
		{"explicit dev", "ghcr.io/molecule-ai/workspace-template-hermes:dev", true},
		{"explicit edge", "ghcr.io/molecule-ai/workspace-template-hermes:edge", true},
		{"explicit nightly", "ghcr.io/molecule-ai/workspace-template-hermes:nightly", true},
		{"explicit rolling", "ghcr.io/molecule-ai/workspace-template-hermes:rolling", true},

		// Pinned tags — must NOT be classified as moving.
		{"semver tag", "ghcr.io/molecule-ai/workspace-template-hermes:0.8.2", false},
		{"semver with v prefix", "ghcr.io/molecule-ai/workspace-template-hermes:v1.2.3", false},
		{"sha-prefixed commit tag", "ghcr.io/molecule-ai/workspace-template-claude-code:sha-abc1234", false},
		{"date-stamped tag", "ghcr.io/molecule-ai/workspace-template-hermes:2026-04-30", false},
		{"build-id tag", "ghcr.io/molecule-ai/workspace-template-hermes:build-12345", false},

		// Digest pinning — strongest immutability signal, never moving
		// even if a moving-looking tag is also present.
		{"digest only", "ghcr.io/molecule-ai/workspace-template-hermes@sha256:abc123def456", false},
		{"tag plus digest", "ghcr.io/molecule-ai/workspace-template-hermes:latest@sha256:abc123def456", false},

		// Registry hostname with port — the `:` in `:5000` must NOT be
		// mistaken for a tag separator. Without this guard, a private
		// registry like `localhost:5000/foo` would always re-pull.
		{"registry with port no tag", "localhost:5000/workspace-template-hermes", true}, // bare → moving
		{"registry with port pinned tag", "localhost:5000/workspace-template-hermes:0.8.2", false},
		{"registry with port latest tag", "localhost:5000/workspace-template-hermes:latest", true},

		// Legacy local-build tags from `docker build -t workspace-template:<runtime>`.
		// These are arbitrary strings, treated as pinned (they don't
		// move from the registry's perspective — there is no registry).
		{"legacy local hermes tag", "workspace-template:hermes", false},
		{"legacy local claude-code tag", "workspace-template:claude-code", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := imageTagIsMoving(tc.image)
			if got != tc.want {
				t.Errorf("imageTagIsMoving(%q) = %v, want %v", tc.image, got, tc.want)
			}
		})
	}
}

// ---------- End-to-end error-message shape ----------
//
// Verifies the wrapped error that Start() surfaces when ContainerCreate
// hits "no such image" after the pull-on-miss attempt. Callers rely on
// both the human hint and the original underlying error being preserved
// (via %w) for errors.Is chains.

func TestImageNotFoundErrorIncludesPullHint(t *testing.T) {
	underlying := testErr(`Error response from daemon: No such image: ghcr.io/molecule-ai/workspace-template-openclaw:latest`)
	if !isImageNotFoundErr(underlying) {
		t.Fatalf("precondition failed: classifier didn't recognise moby's message")
	}

	image := "ghcr.io/molecule-ai/workspace-template-openclaw:latest"
	tag := runtimeTagFromImage(image)
	wrapped := testErr(
		`docker image "` + image + `" not found after pull attempt — verify GHCR visibility for ` + tag +
			` and that the tenant has internet access (underlying error: ` + underlying.Error() + `)`,
	)
	s := wrapped.Error()

	for _, want := range []string{
		`"ghcr.io/molecule-ai/workspace-template-openclaw:latest"`,
		`verify GHCR visibility for openclaw`,
		`No such image`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("wrapped error missing %q, got: %s", want, s)
		}
	}
}

// ---- issue #14: configurable per-tier memory/CPU limits ----

// TestGetTierMemoryMB_DefaultsMatchLegacy asserts that with no env overrides,
// getTierMemoryMB returns the agreed (issue #14) defaults.
func TestGetTierMemoryMB_DefaultsMatchLegacy(t *testing.T) {
	for _, k := range []string{"TIER2_MEMORY_MB", "TIER3_MEMORY_MB", "TIER4_MEMORY_MB"} {
		os.Unsetenv(k)
	}
	cases := map[int]int64{
		1: 0, // no cap
		2: 512,
		3: 2048,
		4: 4096,
		9: 0, // unknown
	}
	for tier, want := range cases {
		if got := getTierMemoryMB(tier); got != want {
			t.Errorf("getTierMemoryMB(%d): got %d, want %d", tier, got, want)
		}
	}
}

// TestGetTierMemoryMB_EnvOverride asserts TIERn_MEMORY_MB takes precedence,
// and that malformed / non-positive values fall back to the default.
func TestGetTierMemoryMB_EnvOverride(t *testing.T) {
	t.Setenv("TIER3_MEMORY_MB", "512")
	if got := getTierMemoryMB(3); got != 512 {
		t.Errorf("with TIER3_MEMORY_MB=512, got %d, want 512", got)
	}
	t.Setenv("TIER3_MEMORY_MB", "not-a-number")
	if got := getTierMemoryMB(3); got != defaultTier3MemoryMB {
		t.Errorf("malformed TIER3_MEMORY_MB: got %d, want default %d", got, defaultTier3MemoryMB)
	}
	t.Setenv("TIER3_MEMORY_MB", "0")
	if got := getTierMemoryMB(3); got != defaultTier3MemoryMB {
		t.Errorf("zero TIER3_MEMORY_MB: got %d, want default %d", got, defaultTier3MemoryMB)
	}
}

// TestGetTierCPUShares_EnvOverride asserts TIERn_CPU_SHARES takes precedence.
func TestGetTierCPUShares_EnvOverride(t *testing.T) {
	t.Setenv("TIER3_CPU_SHARES", "4096")
	if got := getTierCPUShares(3); got != 4096 {
		t.Errorf("with TIER3_CPU_SHARES=4096, got %d, want 4096", got)
	}
	os.Unsetenv("TIER3_CPU_SHARES")
	if got := getTierCPUShares(3); got != defaultTier3CPUShares {
		t.Errorf("unset TIER3_CPU_SHARES: got %d, want default %d", got, defaultTier3CPUShares)
	}
}

// TestApplyTierConfig_T3_UsesEnvOverride is the wiring test: env vars must
// flow through ApplyTierConfig into hostCfg.Resources.
func TestApplyTierConfig_T3_UsesEnvOverride(t *testing.T) {
	t.Setenv("TIER3_MEMORY_MB", "8192")
	t.Setenv("TIER3_CPU_SHARES", "4096") // 4 CPU == 4e9 NanoCPUs

	hc := baseHostConfig("")
	cfg := WorkspaceConfig{WorkspaceID: "abc123", Tier: 3}
	ApplyTierConfig(hc, cfg, "ws-abc123-configs:/configs", "ws-abc123")

	wantMem := int64(8192) * 1024 * 1024
	if hc.Memory != wantMem {
		t.Errorf("T3 memory override: got %d, want %d", hc.Memory, wantMem)
	}
	wantCPU := int64(4_000_000_000)
	if hc.NanoCPUs != wantCPU {
		t.Errorf("T3 CPU override: got %d NanoCPUs, want %d", hc.NanoCPUs, wantCPU)
	}
	if !hc.Privileged || hc.PidMode != "host" {
		t.Errorf("T3 override should preserve privileged/pid-host flags, got Privileged=%v PidMode=%q",
			hc.Privileged, hc.PidMode)
	}
}

// TestApplyTierConfig_T3_DefaultCap asserts T3 now gets a memory/CPU cap by
// default (previously uncapped — behaviour change per issue #14).
func TestApplyTierConfig_T3_DefaultCap(t *testing.T) {
	for _, k := range []string{"TIER3_MEMORY_MB", "TIER3_CPU_SHARES"} {
		os.Unsetenv(k)
	}
	hc := baseHostConfig("")
	cfg := WorkspaceConfig{WorkspaceID: "abc123", Tier: 3}
	ApplyTierConfig(hc, cfg, "ws-abc123-configs:/configs", "ws-abc123")

	wantMem := int64(defaultTier3MemoryMB) * 1024 * 1024
	if hc.Memory != wantMem {
		t.Errorf("T3 default memory: got %d, want %d", hc.Memory, wantMem)
	}
	wantCPU := int64(defaultTier3CPUShares) * 1_000_000_000 / 1024
	if hc.NanoCPUs != wantCPU {
		t.Errorf("T3 default NanoCPUs: got %d, want %d", hc.NanoCPUs, wantCPU)
	}
}

// TestMigrateVolumeIfNeeded_ExistingTruncatedVolume verifies the KI-013 deploy
// safety path: when a legacy truncated-name volume already exists, data is
// copied to the new full-ID name and the legacy volume is removed.  Existing
// workspace state is preserved without operator intervention.
func TestMigrateVolumeIfNeeded_ExistingTruncatedVolume(t *testing.T) {
	ctx := context.Background()

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Skip("docker client unavailable:", err)
	}
	if _, pingErr := cli.Ping(ctx); pingErr != nil {
		t.Skip("docker daemon unreachable:", pingErr)
	}

	p := &Provisioner{cli: cli, alpineImage: "alpine"}
	workspaceID := "test-migrate-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	legacyName := legacyConfigVolumeName(workspaceID)
	newName := ConfigVolumeName(workspaceID)

	// Cleanup before and after (defensive — avoid pollution on retries).
	_ = cli.VolumeRemove(ctx, legacyName, true)
	_ = cli.VolumeRemove(ctx, newName, true)
	defer func() {
		_ = cli.VolumeRemove(ctx, legacyName, true)
		_ = cli.VolumeRemove(ctx, newName, true)
	}()

	// 1. Create legacy volume and seed it with a sentinel file.
	if _, err := cli.VolumeCreate(ctx, volume.CreateOptions{Name: legacyName}); err != nil {
		t.Fatalf("create legacy volume: %v", err)
	}
	seedResp, err := cli.ContainerCreate(ctx, &container.Config{
		Image: "alpine",
		Cmd:   []string{"sh", "-c", "echo sentinel-data > /vol/sentinel.txt"},
	}, &container.HostConfig{
		Binds: []string{legacyName + ":/vol"},
	}, nil, nil, "")
	if err != nil {
		t.Fatalf("create seed container: %v", err)
	}
	defer cli.ContainerRemove(ctx, seedResp.ID, container.RemoveOptions{Force: true})
	if err := cli.ContainerStart(ctx, seedResp.ID, container.StartOptions{}); err != nil {
		t.Fatalf("start seed container: %v", err)
	}
	waitCh, errCh := cli.ContainerWait(ctx, seedResp.ID, container.WaitConditionNotRunning)
	select {
	case <-waitCh:
	case err := <-errCh:
		if err != nil {
			t.Fatalf("seed container failed: %v", err)
		}
	}
	// Remove the seed container before migration so the legacy volume is
	// no longer referenced by any container. The deferred remove above is a
	// safety net for panic/early-return paths.
	if err := cli.ContainerRemove(ctx, seedResp.ID, container.RemoveOptions{Force: true}); err != nil {
		t.Fatalf("remove seed container: %v", err)
	}

	// 2. Run migration.
	if err := p.migrateVolumeIfNeeded(ctx, newName, legacyName); err != nil {
		t.Fatalf("migrateVolumeIfNeeded failed: %v", err)
	}

	// 3. Legacy volume must be gone.
	if _, inspectErr := cli.VolumeInspect(ctx, legacyName); inspectErr == nil {
		t.Fatalf("legacy volume %s still exists after migration", legacyName)
	}

	// 4. New volume must exist and contain the sentinel file.
	if _, inspectErr := cli.VolumeInspect(ctx, newName); inspectErr != nil {
		t.Fatalf("new volume %s does not exist after migration: %v", newName, inspectErr)
	}

	readResp, err := cli.ContainerCreate(ctx, &container.Config{
		Image: "alpine",
		Cmd:   []string{"cat", "/vol/sentinel.txt"},
	}, &container.HostConfig{
		Binds: []string{newName + ":/vol"},
	}, nil, nil, "")
	if err != nil {
		t.Fatalf("create read container: %v", err)
	}
	defer cli.ContainerRemove(ctx, readResp.ID, container.RemoveOptions{Force: true})
	if err := cli.ContainerStart(ctx, readResp.ID, container.StartOptions{}); err != nil {
		t.Fatalf("start read container: %v", err)
	}
	waitCh, errCh = cli.ContainerWait(ctx, readResp.ID, container.WaitConditionNotRunning)
	select {
	case <-waitCh:
	case err := <-errCh:
		if err != nil {
			t.Fatalf("read container failed: %v", err)
		}
	}

	logs, err := cli.ContainerLogs(ctx, readResp.ID, container.LogsOptions{ShowStdout: true})
	if err != nil {
		t.Fatalf("read container logs: %v", err)
	}
	defer logs.Close()
	data, err := io.ReadAll(logs)
	if err != nil {
		t.Fatalf("read logs: %v", err)
	}
	if !strings.Contains(string(data), "sentinel-data") {
		t.Fatalf("new volume missing sentinel data; logs: %q", data)
	}

	// 5. Idempotency: second migration must be a no-op.
	if err := p.migrateVolumeIfNeeded(ctx, newName, legacyName); err != nil {
		t.Fatalf("second migration (idempotency) failed: %v", err)
	}
}

// TestInternalURL verifies the container-internal URL shape.
func TestInternalURL(t *testing.T) {
	tests := []struct {
		id   string
		want string
	}{
		{"abc123", "http://ws-abc123:8000"},
		{"longer-than-twelve-characters", "http://ws-longer-than-twelve-characters:8000"},
		{"", "http://ws-:8000"},
	}
	for _, tt := range tests {
		got := InternalURL(tt.id)
		if got != tt.want {
			t.Errorf("InternalURL(%q) = %q, want %q", tt.id, got, tt.want)
		}
	}
}

// TestApplyTierResources verifies the direct memory/CPU resource application.
// Note: the T2-fallback for unknown tiers is handled by ApplyTierConfig, not
// this low-level helper.
func TestApplyTierResources(t *testing.T) {
	for _, k := range []string{"TIER2_MEMORY_MB", "TIER2_CPU_SHARES", "TIER3_MEMORY_MB", "TIER3_CPU_SHARES", "TIER4_MEMORY_MB", "TIER4_CPU_SHARES"} {
		os.Unsetenv(k)
	}

	tests := []struct {
		name         string
		tier         int
		wantMemory   int64
		wantNanoCPUs int64
		wantShares   int64
	}{
		{"T1 no cap", 1, 0, 0, 0},
		{"T2 512MiB 1CPU", 2, 512 * 1024 * 1024, 1_000_000_000, 1024},
		{"T3 2048MiB 2CPU", 3, 2048 * 1024 * 1024, 2_000_000_000, 2048},
		{"T4 4096MiB 4CPU", 4, 4096 * 1024 * 1024, 4_000_000_000, 4096},
		{"unknown tier no cap", 99, 0, 0, 0},
		{"zero tier no cap", 0, 0, 0, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hc := &container.HostConfig{}
			memMB, cpuShares := applyTierResources(hc, tt.tier)

			if memMB != tt.wantMemory/(1024*1024) {
				t.Errorf("memMB = %d, want %d", memMB, tt.wantMemory/(1024*1024))
			}
			if hc.Memory != tt.wantMemory {
				t.Errorf("Memory = %d, want %d", hc.Memory, tt.wantMemory)
			}
			if hc.NanoCPUs != tt.wantNanoCPUs {
				t.Errorf("NanoCPUs = %d, want %d", hc.NanoCPUs, tt.wantNanoCPUs)
			}
			if cpuShares != tt.wantShares {
				t.Errorf("cpuShares = %d, want %d", cpuShares, tt.wantShares)
			}
		})
	}
}

// ---------- #2851 host-port advertisement ----------

func TestAllocateHostPort(t *testing.T) {
	port, err := allocateHostPort()
	if err != nil {
		t.Fatalf("allocateHostPort failed: %v", err)
	}
	if port == "" {
		t.Fatal("allocateHostPort returned empty port")
	}
	if port == "0" {
		t.Fatalf("allocateHostPort returned port 0")
	}

	// Verify the port is actually numeric and in the ephemeral range.
	n, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("allocateHostPort returned non-numeric port %q: %v", port, err)
	}
	if n < 1024 || n > 65535 {
		t.Fatalf("allocateHostPort returned out-of-range port %d", n)
	}

	// Verify the port is genuinely free for Docker to bind. (We don't assert
	// uniqueness across two calls — the OS may immediately reuse a just-closed
	// ephemeral port under heavy load.)
	l, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		t.Fatalf("allocateHostPort returned port %s that is not bindable: %v", port, err)
	}
	l.Close()
}

func TestWorkspaceAdvertiseURL(t *testing.T) {
	t.Run("default localhost", func(t *testing.T) {
		got := workspaceAdvertiseURL("12345")
		want := "http://localhost:12345"
		if got != want {
			t.Errorf("workspaceAdvertiseURL = %q, want %q", got, want)
		}
	})

	t.Run("MOLECULE_WORKSPACE_ADVERTISE_HOST override", func(t *testing.T) {
		t.Setenv("MOLECULE_WORKSPACE_ADVERTISE_HOST", "192.168.1.100")
		got := workspaceAdvertiseURL("12345")
		want := "http://192.168.1.100:12345"
		if got != want {
			t.Errorf("workspaceAdvertiseURL = %q, want %q", got, want)
		}
	})

	t.Run("empty env defaults to localhost", func(t *testing.T) {
		t.Setenv("MOLECULE_WORKSPACE_ADVERTISE_HOST", "")
		got := workspaceAdvertiseURL("12345")
		want := "http://localhost:12345"
		if got != want {
			t.Errorf("workspaceAdvertiseURL = %q, want %q", got, want)
		}
	})
}

// TestResolveStartWorkspaceHostURL covers the #2851 registration-path fix:
// the hostURL StartWorkspace persists in the DB must be the host-reachable
// advertise URL (not 127.0.0.1) so ProxyA2A's resolveAgentURL doesn't
// rewrite it to the internal Docker hostname. Each subtest pins the env
// var then asserts the hostURL for both the initial and inspect-fallback
// paths.
func TestResolveStartWorkspaceHostURL(t *testing.T) {
	t.Run("default localhost (no env override)", func(t *testing.T) {
		t.Setenv("MOLECULE_WORKSPACE_ADVERTISE_HOST", "")
		got := resolveStartWorkspaceHostURL("12345", "")
		want := "http://localhost:12345"
		if got != want {
			t.Errorf("resolveStartWorkspaceHostURL = %q, want %q", got, want)
		}
	})

	t.Run("env override, no bound-port swap", func(t *testing.T) {
		t.Setenv("MOLECULE_WORKSPACE_ADVERTISE_HOST", "172.18.0.1")
		got := resolveStartWorkspaceHostURL("33605", "")
		want := "http://172.18.0.1:33605"
		if got != want {
			t.Errorf("resolveStartWorkspaceHostURL = %q, want %q", got, want)
		}
	})

	t.Run("env override, bound port differs (inspect-fallback path keeps advertise host)", func(t *testing.T) {
		t.Setenv("MOLECULE_WORKSPACE_ADVERTISE_HOST", "172.18.0.1")
		// pre-allocated hostPort was 33605 but Docker bound 33606.
		// Final URL must use the advertise host + the bound port.
		got := resolveStartWorkspaceHostURL("33605", "33606")
		want := "http://172.18.0.1:33606"
		if got != want {
			t.Errorf("resolveStartWorkspaceHostURL = %q, want %q", got, want)
		}
	})

	t.Run("env override, bound port matches (no-op)", func(t *testing.T) {
		t.Setenv("MOLECULE_WORKSPACE_ADVERTISE_HOST", "192.168.65.2")
		got := resolveStartWorkspaceHostURL("8080", "8080")
		want := "http://192.168.65.2:8080"
		if got != want {
			t.Errorf("resolveStartWorkspaceHostURL = %q, want %q", got, want)
		}
	})

	t.Run("no env override, bound port swap (legacy 127.0.0.1 case)", func(t *testing.T) {
		t.Setenv("MOLECULE_WORKSPACE_ADVERTISE_HOST", "")
		// default "localhost" but bound port differs
		got := resolveStartWorkspaceHostURL("8080", "9090")
		want := "http://localhost:9090"
		if got != want {
			t.Errorf("resolveStartWorkspaceHostURL = %q, want %q", got, want)
		}
	})
}


// TestBuildStartWorkspaceEnv covers the #2851 production-path env injection
// gap that bit 3 rounds running (Researcher #11798 / #11787 close-out).
// The provisioner's Start() must inject MOLECULE_WORKSPACE_URL=<host-port>
// into the container env so the runtime's resolve_workspace_url
// (highest precedence for the env var) returns the same host-port URL
// the provisioner persisted. When env propagation is broken
// (the real-image lifecycle E2E gap), the runtime falls back to
// http://HOSTNAME:8000 — the Register handler's upsert must then
// preserve the provisioner's URL (the Register handler fix is in
// registry.go, this test covers the provisioner side).
func TestBuildStartWorkspaceEnv(t *testing.T) {
	cfg := WorkspaceConfig{
		WorkspaceID: "ws-test-1",
		Tier:        1,
		PlatformURL: "http://platform:8080",
	}

	find := func(env []string, key string) string {
		for _, e := range env {
			if len(e) >= len(key)+1 && e[:len(key)+1] == key+"=" {
				return e[len(key)+1:]
			}
		}
		return ""
	}

	t.Run("default localhost (no env override)", func(t *testing.T) {
		t.Setenv("MOLECULE_WORKSPACE_ADVERTISE_HOST", "")
		env := buildStartWorkspaceEnv(cfg, "41751")
		got := find(env, "MOLECULE_WORKSPACE_URL")
		want := "http://localhost:41751"
		if got != want {
			t.Errorf("MOLECULE_WORKSPACE_URL = %q, want %q", got, want)
		}
	})

	t.Run("env override (containerized-platform path)", func(t *testing.T) {
		t.Setenv("MOLECULE_WORKSPACE_ADVERTISE_HOST", "172.18.0.1")
		env := buildStartWorkspaceEnv(cfg, "33605")
		got := find(env, "MOLECULE_WORKSPACE_URL")
		want := "http://172.18.0.1:33605"
		if got != want {
			t.Errorf("MOLECULE_WORKSPACE_URL = %q, want %q", got, want)
		}
	})

	t.Run("env injection comes AFTER buildContainerEnv (last-wins for docker -e)", func(t *testing.T) {
		t.Setenv("MOLECULE_WORKSPACE_ADVERTISE_HOST", "")
		env := buildStartWorkspaceEnv(cfg, "12345")
		var workspaceURLIdx, workspaceIDIdx int
		workspaceURLIdx, workspaceIDIdx = -1, -1
		for i, e := range env {
			if len(e) > 22 && e[:23] == "MOLECULE_WORKSPACE_URL=" {
				workspaceURLIdx = i
			}
			if len(e) > 12 && e[:13] == "WORKSPACE_ID=" {
				workspaceIDIdx = i
			}
		}
		if workspaceURLIdx == -1 {
			t.Fatal("MOLECULE_WORKSPACE_URL not in env")
		}
		if workspaceURLIdx <= workspaceIDIdx {
			t.Errorf("MOLECULE_WORKSPACE_URL (idx %d) must come AFTER buildContainerEnv entries (WORKSPACE_ID idx %d) for docker -e last-wins", workspaceURLIdx, workspaceIDIdx)
		}
	})
}
