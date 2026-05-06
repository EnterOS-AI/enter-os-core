package provisioner

import (
	"strings"
	"testing"
)

// TestRegistryPrefix_DefaultsToGHCR pins the OSS-default behavior. If a future
// refactor accidentally drops the default, OSS users self-hosting Molecule
// would silently lose image pulls — this test should fail loudly instead.
func TestRegistryPrefix_DefaultsToGHCR(t *testing.T) {
	t.Setenv("MOLECULE_IMAGE_REGISTRY", "")
	got := RegistryPrefix()
	want := "ghcr.io/molecule-ai"
	if got != want {
		t.Fatalf("RegistryPrefix() = %q, want %q (default must remain GHCR for OSS users)", got, want)
	}
}

// TestRegistryPrefix_RespectsEnv verifies the override path used in
// production tenants where MOLECULE_IMAGE_REGISTRY points at a private
// mirror (AWS ECR, self-hosted Harbor, etc.).
func TestRegistryPrefix_RespectsEnv(t *testing.T) {
	t.Setenv("MOLECULE_IMAGE_REGISTRY", "123456789012.dkr.ecr.us-east-2.amazonaws.com/molecule-ai")
	got := RegistryPrefix()
	want := "123456789012.dkr.ecr.us-east-2.amazonaws.com/molecule-ai"
	if got != want {
		t.Fatalf("RegistryPrefix() = %q, want %q (env override path is the production cutover mechanism)", got, want)
	}
}

// TestRegistryPrefix_EmptyEnvFallsBackToDefault — guard against an operator
// setting MOLECULE_IMAGE_REGISTRY="" by mistake (e.g. unset deploy variable
// becomes empty string, not literally absent). We treat "" as "use default"
// so a misconfigured env doesn't mean an empty registry prefix.
func TestRegistryPrefix_EmptyEnvFallsBackToDefault(t *testing.T) {
	t.Setenv("MOLECULE_IMAGE_REGISTRY", "")
	if RegistryPrefix() != defaultRegistryPrefix {
		t.Fatalf("empty MOLECULE_IMAGE_REGISTRY should fall back to %q, got %q", defaultRegistryPrefix, RegistryPrefix())
	}
}

// TestRuntimeImage_AllKnownRuntimes — every runtime in the canonical list
// must produce a properly-formatted image ref. If a new runtime is added to
// knownRuntimes but the format changes, this catches it.
func TestRuntimeImage_AllKnownRuntimes(t *testing.T) {
	t.Setenv("MOLECULE_IMAGE_REGISTRY", "")
	for _, r := range knownRuntimes {
		got := RuntimeImage(r)
		want := "ghcr.io/molecule-ai/workspace-template-" + r + ":latest"
		if got != want {
			t.Errorf("RuntimeImage(%q) = %q, want %q", r, got, want)
		}
	}
	// Pin the count so adding a runtime requires explicit test acknowledgement.
	if len(knownRuntimes) != 9 {
		t.Errorf("knownRuntimes length = %d, want 9 (autogen, claude-code, codex, crewai, deepagents, gemini-cli, hermes, langgraph, openclaw)", len(knownRuntimes))
	}
}

// TestRuntimeImage_UnknownRuntime — defensive: callers must fall back to
// DefaultImage when a runtime is unknown, never silently use the wrong
// prefix. Returning "" enforces an explicit fallback at every call site.
func TestRuntimeImage_UnknownRuntime(t *testing.T) {
	for _, name := range []string{"", "nonexistent", "WORKSPACE-TEMPLATE-FAKE", "../../../etc/passwd"} {
		if got := RuntimeImage(name); got != "" {
			t.Errorf("RuntimeImage(%q) = %q, want empty string for unknown runtime", name, got)
		}
	}
}

// TestRuntimeImage_RegistryOverrideAppliesToAllRuntimes — the override
// flips ALL runtimes consistently. If a refactor accidentally hardcoded
// the prefix in some runtimes but not others (the failure mode that
// triggered this whole rollout), this test catches it.
func TestRuntimeImage_RegistryOverrideAppliesToAllRuntimes(t *testing.T) {
	const ecr = "999999999999.dkr.ecr.us-east-2.amazonaws.com/molecule-ai"
	t.Setenv("MOLECULE_IMAGE_REGISTRY", ecr)

	for _, r := range knownRuntimes {
		got := RuntimeImage(r)
		if !strings.HasPrefix(got, ecr+"/workspace-template-") {
			t.Errorf("RuntimeImage(%q) = %q, must start with override prefix %q", r, got, ecr)
		}
		if !strings.HasSuffix(got, ":latest") {
			t.Errorf("RuntimeImage(%q) = %q, must keep :latest tag suffix", r, got)
		}
	}
}

// TestComputeRuntimeImages_AllRuntimesPresent — the map must contain every
// known runtime. Drift between knownRuntimes and computeRuntimeImages would
// silently break the runtime → image lookup that provisioner.Start uses.
func TestComputeRuntimeImages_AllRuntimesPresent(t *testing.T) {
	t.Setenv("MOLECULE_IMAGE_REGISTRY", "")
	m := computeRuntimeImages()
	if len(m) != len(knownRuntimes) {
		t.Fatalf("computeRuntimeImages() has %d entries, want %d (one per knownRuntime)", len(m), len(knownRuntimes))
	}
	for _, r := range knownRuntimes {
		img, ok := m[r]
		if !ok {
			t.Errorf("computeRuntimeImages() missing runtime %q", r)
			continue
		}
		if img == "" {
			t.Errorf("computeRuntimeImages()[%q] is empty", r)
		}
	}
}

// TestComputeRuntimeImages_ReflectsCurrentEnv — calling computeRuntimeImages
// after env change rebuilds the map with new prefix. Tests + ops procedures
// that flip the env in-process rely on this.
func TestComputeRuntimeImages_ReflectsCurrentEnv(t *testing.T) {
	t.Setenv("MOLECULE_IMAGE_REGISTRY", "")
	defaultMap := computeRuntimeImages()
	if !strings.HasPrefix(defaultMap["claude-code"], "ghcr.io/molecule-ai/") {
		t.Fatalf("default map should be GHCR-prefixed, got %q", defaultMap["claude-code"])
	}

	const mirror = "registry.example.com/molecule-ai"
	t.Setenv("MOLECULE_IMAGE_REGISTRY", mirror)
	mirrorMap := computeRuntimeImages()
	if !strings.HasPrefix(mirrorMap["claude-code"], mirror+"/") {
		t.Fatalf("mirror-prefixed map should start with %q, got %q", mirror, mirrorMap["claude-code"])
	}
}

// TestKnownRuntimes_AlphabeticalOrder — pin the order so test snapshots
// (and human readers diffing the file) see deterministic output. Adding a
// new runtime out of alphabetical order will fail this test, which is the
// nudge to keep the file readable.
func TestKnownRuntimes_AlphabeticalOrder(t *testing.T) {
	for i := 1; i < len(knownRuntimes); i++ {
		if knownRuntimes[i-1] >= knownRuntimes[i] {
			t.Errorf("knownRuntimes not alphabetical: %q comes before %q", knownRuntimes[i-1], knownRuntimes[i])
		}
	}
}
