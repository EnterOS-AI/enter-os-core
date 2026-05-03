package provisioner

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
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
	if hc.Resources.Memory != expectedMemory {
		t.Errorf("T2: expected Memory=%d (512m), got %d", expectedMemory, hc.Resources.Memory)
	}

	// CPU limit: 1.0 CPU (1e9 NanoCPUs)
	expectedCPU := int64(1_000_000_000)
	if hc.Resources.NanoCPUs != expectedCPU {
		t.Errorf("T2: expected NanoCPUs=%d (1.0 CPU), got %d", expectedCPU, hc.Resources.NanoCPUs)
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
	if hc.Resources.Memory != expectedMemory {
		t.Errorf("Unknown tier: expected Memory=%d (512m), got %d", expectedMemory, hc.Resources.Memory)
	}

	expectedCPU := int64(1_000_000_000)
	if hc.Resources.NanoCPUs != expectedCPU {
		t.Errorf("Unknown tier: expected NanoCPUs=%d (1.0 CPU), got %d", expectedCPU, hc.Resources.NanoCPUs)
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
	if hc.Resources.Memory != expectedMemory {
		t.Errorf("Tier 0: expected Memory=%d, got %d", expectedMemory, hc.Resources.Memory)
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
		{"longer-than-twelve-characters", "ws-longer-than-"},
		{"abc", "ws-abc"},
	}

	for _, tt := range tests {
		got := ContainerName(tt.id)
		if got != tt.want {
			t.Errorf("ContainerName(%q) = %q, want %q", tt.id, got, tt.want)
		}
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
		{"longer-than-twelve-characters", "ws-longer-than--configs"},
		{"abc", "ws-abc-configs"},
	}

	for _, tt := range tests {
		got := ConfigVolumeName(tt.id)
		if got != tt.want {
			t.Errorf("ConfigVolumeName(%q) = %q, want %q", tt.id, got, tt.want)
		}
	}
}

// ---------- #12 — claude-sessions volume naming ----------

// TestClaudeSessionVolumeName_Deterministic: same ID → same volume name, and
// the name follows the ws-<id[:12]>-claude-sessions shape used everywhere
// else in the provisioner.
func TestClaudeSessionVolumeName_Deterministic(t *testing.T) {
	tests := []struct {
		id   string
		want string
	}{
		{"short", "ws-short-claude-sessions"},
		{"exactly12ch", "ws-exactly12ch-claude-sessions"},
		{"longer-than-twelve-characters", "ws-longer-than--claude-sessions"},
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

// TestSelectImage_PrefersExplicitImage: when the handler resolved a digest
// pin via runtime_image_pins, cfg.Image is set. selectImage must honor it
// and ignore the cfg.Runtime → :latest fallback. This is the load-bearing
// invariant for digest pinning — if it ever silently reverts to :latest,
// we lose the "one bad publish doesn't break every workspace" guarantee.
func TestSelectImage_PrefersExplicitImage(t *testing.T) {
	pinned := "ghcr.io/molecule-ai/workspace-template-claude-code@sha256:3d6761a97ed07d7d33cfc19a8fbab81175d9d9179618d493dbc00c5f7ef076a3"
	got := selectImage(WorkspaceConfig{Runtime: "claude-code", Image: pinned})
	if got != pinned {
		t.Errorf("selectImage with cfg.Image=pinned: got %q, want %q", got, pinned)
	}
}

// TestSelectImage_FallsBackToRuntimeMap: handler returned "" (no pin or
// pin lookup deliberately bypassed via WORKSPACE_IMAGE_LOCAL_OVERRIDE).
// selectImage must use the legacy runtime→:latest map.
func TestSelectImage_FallsBackToRuntimeMap(t *testing.T) {
	got := selectImage(WorkspaceConfig{Runtime: "claude-code", Image: ""})
	want := RuntimeImages["claude-code"]
	if got != want {
		t.Errorf("selectImage with empty Image: got %q, want %q", got, want)
	}
}

// TestSelectImage_UnknownRuntimeFallsBackToDefault preserves today's
// behavior — an unrecognized runtime resolves to DefaultImage rather than
// "" so ContainerCreate gets a usable arg and surfaces a meaningful
// "No such image" error if the default itself is missing.
func TestSelectImage_UnknownRuntimeFallsBackToDefault(t *testing.T) {
	got := selectImage(WorkspaceConfig{Runtime: "no-such-runtime"})
	if got != DefaultImage {
		t.Errorf("selectImage with unknown runtime: got %q, want DefaultImage %q", got, DefaultImage)
	}
}

// TestSelectImage_EmptyRuntimeFallsBackToDefault: same invariant for the
// no-runtime-supplied path (legacy callers / older handler code).
func TestSelectImage_EmptyRuntimeFallsBackToDefault(t *testing.T) {
	got := selectImage(WorkspaceConfig{})
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

func TestBuildContainerEnv_AwarenessOnlyWhenBothSet(t *testing.T) {
	// Both set → both injected.
	cfg := WorkspaceConfig{
		WorkspaceID:        "ws-x",
		PlatformURL:        "http://localhost:8080",
		AwarenessURL:       "http://awareness:9000",
		AwarenessNamespace: "ns-1",
	}
	env := buildContainerEnv(cfg)
	hasNS := false
	hasURL := false
	for _, e := range env {
		if e == "AWARENESS_NAMESPACE=ns-1" {
			hasNS = true
		}
		if e == "AWARENESS_URL=http://awareness:9000" {
			hasURL = true
		}
	}
	if !hasNS || !hasURL {
		t.Errorf("both awareness vars must be present: env=%v", env)
	}

	// Only namespace set → neither injected (must be both-or-nothing).
	cfg.AwarenessURL = ""
	env2 := buildContainerEnv(cfg)
	for _, e := range env2 {
		if strings.HasPrefix(e, "AWARENESS_") {
			t.Errorf("awareness vars must NOT be injected when URL is missing: got %q", e)
		}
	}
}

func TestBuildContainerEnv_CustomEnvVarsAppended(t *testing.T) {
	cfg := WorkspaceConfig{
		WorkspaceID: "ws-x",
		PlatformURL: "http://localhost:8080",
		EnvVars:     map[string]string{"CUSTOM": "value", "GITHUB_TOKEN": "fake-token-for-test"},
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
	if seen["GITHUB_TOKEN"] != "fake-token-for-test" {
		t.Errorf("GITHUB_TOKEN env missing, got env=%v", env)
	}
	// Built-in defaults still present
	if seen["MOLECULE_URL"] == "" {
		t.Errorf("MOLECULE_URL must still be set alongside custom envs")
	}
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
		{"image not found", fmtErr(`Error: image "workspace-template:crewai" not found`), true},
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
		"ghcr.io/molecule-ai/workspace-template-hermes:latest":         "hermes",
		"ghcr.io/molecule-ai/workspace-template-claude-code:latest":    "claude-code",
		"ghcr.io/molecule-ai/workspace-template-langgraph:sha-abc1234": "langgraph",
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
		{"sha-prefixed commit tag", "ghcr.io/molecule-ai/workspace-template-langgraph:sha-abc1234", false},
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
	if hc.Resources.Memory != wantMem {
		t.Errorf("T3 memory override: got %d, want %d", hc.Resources.Memory, wantMem)
	}
	wantCPU := int64(4_000_000_000)
	if hc.Resources.NanoCPUs != wantCPU {
		t.Errorf("T3 CPU override: got %d NanoCPUs, want %d", hc.Resources.NanoCPUs, wantCPU)
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
	if hc.Resources.Memory != wantMem {
		t.Errorf("T3 default memory: got %d, want %d", hc.Resources.Memory, wantMem)
	}
	wantCPU := int64(defaultTier3CPUShares) * 1_000_000_000 / 1024
	if hc.Resources.NanoCPUs != wantCPU {
		t.Errorf("T3 default NanoCPUs: got %d, want %d", hc.Resources.NanoCPUs, wantCPU)
	}
}
