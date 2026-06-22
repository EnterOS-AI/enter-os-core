package provisioner

import (
	"context"
	"errors"
	"testing"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/models"
)

// TestWorkspaceKindPlatform_MatchesModels guards the duplicated constant: the
// provisioner-local WorkspaceKindPlatform MUST equal models.KindPlatform, else a
// kind='platform' concierge would silently miss the platform-agent image.
func TestWorkspaceKindPlatform_MatchesModels(t *testing.T) {
	if WorkspaceKindPlatform != models.KindPlatform {
		t.Fatalf("WorkspaceKindPlatform=%q != models.KindPlatform=%q — keep them in sync", WorkspaceKindPlatform, models.KindPlatform)
	}
}

// TestLocalPlatformAgentLatestTag asserts the platform-agent image variant tag
// shape: molecule-local/workspace-template-<runtime>-platform-agent:latest.
func TestLocalPlatformAgentLatestTag(t *testing.T) {
	got := LocalPlatformAgentLatestTag("claude-code")
	want := "molecule-local/workspace-template-claude-code-platform-agent:latest"
	if got != want {
		t.Fatalf("LocalPlatformAgentLatestTag = %q, want %q", got, want)
	}
}

// TestResolvePlatformAgentImage_Present: when the platform-agent image variant
// exists in the local store, resolvePlatformAgentImage returns it + true so the
// concierge runs on the image that bakes /opt/molecule-mcp-server.
func TestResolvePlatformAgentImage_Present(t *testing.T) {
	wantTag := LocalPlatformAgentLatestTag("claude-code")
	var probed string
	hasTag := func(_ context.Context, tag string) (bool, error) {
		probed = tag
		return tag == wantTag, nil
	}
	got, ok := resolvePlatformAgentImage(context.Background(), "claude-code", "molecule-local/workspace-template-claude-code:abc", hasTag)
	if !ok {
		t.Fatalf("expected ok=true when platform-agent image present")
	}
	if got != wantTag {
		t.Fatalf("got image %q, want %q", got, wantTag)
	}
	if probed != wantTag {
		t.Fatalf("probed %q, want %q", probed, wantTag)
	}
}

// TestResolvePlatformAgentImage_Absent: when the variant is NOT present, the
// resolver returns ("", false) so the caller safely falls back to the plain
// runtime image — an ordinary local stack keeps working (concierge just runs
// without the org-admin MCP). This is the gate the task requires.
func TestResolvePlatformAgentImage_Absent(t *testing.T) {
	hasTag := func(_ context.Context, _ string) (bool, error) { return false, nil }
	got, ok := resolvePlatformAgentImage(context.Background(), "claude-code", "molecule-local/workspace-template-claude-code:abc", hasTag)
	if ok {
		t.Fatalf("expected ok=false when platform-agent image absent")
	}
	if got != "" {
		t.Fatalf("expected empty image on absent, got %q", got)
	}
}

// TestResolvePlatformAgentImage_ProbeError: a docker-inspect error is treated as
// "absent" (fall back to the plain image) — never fails the provision.
func TestResolvePlatformAgentImage_ProbeError(t *testing.T) {
	hasTag := func(_ context.Context, _ string) (bool, error) {
		return false, errors.New("docker daemon unreachable")
	}
	got, ok := resolvePlatformAgentImage(context.Background(), "claude-code", "fallback:img", hasTag)
	if ok || got != "" {
		t.Fatalf("expected fall-back on probe error, got (%q, %v)", got, ok)
	}
}
