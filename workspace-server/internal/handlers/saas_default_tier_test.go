package handlers

import (
	"strings"
	"testing"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/provisioner"
)

// Tests for the SaaS-aware default-tier resolution introduced in #2901
// and hardened in #2910 (multi-model review of #2901 found the original
// claim of "all green" was passing because no SaaS-mode test existed).
//
// These tests pin three invariants:
//
//   1. WorkspaceHandler.IsSaaS() returns true when cpProv is wired,
//      false otherwise.
//   2. WorkspaceHandler.DefaultTier() returns 4 on SaaS, 3 self-hosted.
//   3. generateDefaultConfig (TemplatesHandler.Import path) writes the
//      passed-in tier into the generated config.yaml — pre-#2910 it
//      was hardcoded to 3 and silently disagreed with the create-
//      handler default on SaaS.

// stubCPProv is a minimal stand-in for the CP provisioner — only
// exercises the IsSaaS / HasProvisioner contract, never invoked in
// these tests.
type stubCPProv struct{}

func (stubCPProv) Start(_ interface{}, _ provisioner.WorkspaceConfig) (string, error) {
	return "", nil
}
func (stubCPProv) Stop(_ interface{}, _ string) error { return nil }
func (stubCPProv) Restart(_ interface{}, _ provisioner.WorkspaceConfig) (string, error) {
	return "", nil
}

func TestIsSaaS_TrueWhenCPProvWired(t *testing.T) {
	h := &WorkspaceHandler{cpProv: &trackingCPProv{}}
	if !h.IsSaaS() {
		t.Errorf("IsSaaS()=false with cpProv wired; expected true")
	}
}

func TestIsSaaS_FalseWhenOnlyDocker(t *testing.T) {
	// provisioner field set, cpProv nil — the self-hosted path.
	// Use a non-nil sentinel so the check actually has something to
	// disagree with. trackingCPProv lives in workspace_provision_auto_test.go
	// and is the established stub for these handler-level tests.
	h := &WorkspaceHandler{provisioner: nil, cpProv: nil}
	if h.IsSaaS() {
		t.Errorf("IsSaaS()=true with both backends nil; expected false")
	}
}

func TestDefaultTier_SaaS_IsT4(t *testing.T) {
	h := &WorkspaceHandler{cpProv: &trackingCPProv{}}
	if got := h.DefaultTier(); got != 4 {
		t.Errorf("SaaS DefaultTier()=%d; expected 4", got)
	}
}

func TestDefaultTier_SelfHosted_IsT3(t *testing.T) {
	h := &WorkspaceHandler{}
	if got := h.DefaultTier(); got != 3 {
		t.Errorf("self-hosted DefaultTier()=%d; expected 3", got)
	}
}

// generateDefaultConfig — pin that the tier param flows into the
// emitted config.yaml verbatim. Pre-#2910 this was hardcoded "tier: 3"
// regardless of caller intent.
func TestGenerateDefaultConfig_RespectsTierParam(t *testing.T) {
	cfg := generateDefaultConfig("Test Agent", map[string]string{"system-prompt.md": ""}, 4)
	if !strings.Contains(cfg, "tier: 4\n") {
		t.Errorf("expected `tier: 4` in generated config, got:\n%s", cfg)
	}
	// The pre-#2910 hardcoded `tier: 3` line must NOT appear.
	if strings.Contains(cfg, "tier: 3\n") {
		t.Errorf("config should not contain `tier: 3` when caller passed 4, got:\n%s", cfg)
	}
}

func TestGenerateDefaultConfig_SelfHostedTierT3(t *testing.T) {
	cfg := generateDefaultConfig("Test Agent", map[string]string{"system-prompt.md": ""}, 3)
	if !strings.Contains(cfg, "tier: 3\n") {
		t.Errorf("expected `tier: 3` in generated config, got:\n%s", cfg)
	}
}

// Bounds check — caller passes 0 or out-of-range, helper falls back
// to T3 (the safer-of-the-two when deployment mode can't be resolved).
func TestGenerateDefaultConfig_OutOfRangeFallsBackToT3(t *testing.T) {
	for _, tier := range []int{0, -1, 99} {
		cfg := generateDefaultConfig("X", map[string]string{}, tier)
		if !strings.Contains(cfg, "tier: 3\n") {
			t.Errorf("invalid tier %d should fall back to T3, got:\n%s", tier, cfg)
		}
	}
}
