package handlers

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func TestGHCRAuthHeader_NoEnvReturnsEmpty(t *testing.T) {
	t.Setenv("GHCR_USER", "")
	t.Setenv("GHCR_TOKEN", "")
	t.Setenv("MOLECULE_IMAGE_REGISTRY", "")
	if got := ghcrAuthHeader(); got != "" {
		t.Errorf("expected empty (no auth → public-only), got %q", got)
	}
}

func TestGHCRAuthHeader_PartialEnvReturnsEmpty(t *testing.T) {
	// Both must be set — defensive against half-configured env.
	t.Setenv("GHCR_USER", "alice")
	t.Setenv("GHCR_TOKEN", "")
	if got := ghcrAuthHeader(); got != "" {
		t.Errorf("user-only env should disable auth, got %q", got)
	}
	t.Setenv("GHCR_USER", "")
	t.Setenv("GHCR_TOKEN", "fake-tok-xxx")
	if got := ghcrAuthHeader(); got != "" {
		t.Errorf("token-only env should disable auth, got %q", got)
	}
}

func TestGHCRAuthHeader_EncodesDockerEnginePayload(t *testing.T) {
	// Default registry env (unset → ghcr.io/molecule-ai) means the
	// serveraddress field should resolve to ghcr.io. Pin both env vars so the
	// test is hermetic regardless of the host's MOLECULE_IMAGE_REGISTRY.
	t.Setenv("MOLECULE_IMAGE_REGISTRY", "")
	t.Setenv("GHCR_USER", "alice")
	t.Setenv("GHCR_TOKEN", "fake-tok-value")
	got := ghcrAuthHeader()
	if got == "" {
		t.Fatal("expected non-empty auth header")
	}
	raw, err := base64.URLEncoding.DecodeString(got)
	if err != nil {
		t.Fatalf("auth header is not valid base64-url: %v", err)
	}
	var payload map[string]string
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decoded auth is not valid JSON: %v (raw=%s)", err, raw)
	}
	if payload["username"] != "alice" {
		t.Errorf("username: got %q, want alice", payload["username"])
	}
	if payload["password"] != "fake-tok-value" {
		t.Errorf("password: got %q, want fake-tok-value", payload["password"])
	}
	if payload["serveraddress"] != "ghcr.io" {
		t.Errorf("serveraddress: got %q, want ghcr.io", payload["serveraddress"])
	}
}

// TestGHCRAuthHeader_RespectsRegistryEnv pins the RFC #229 fix: when
// MOLECULE_IMAGE_REGISTRY points at a private mirror (e.g. AWS ECR), the
// Docker engine auth payload's serveraddress must reflect that mirror's
// host so credential matching lands on the right entry. Pre-fix this was
// hardcoded to "ghcr.io" and silently dropped the override.
func TestGHCRAuthHeader_RespectsRegistryEnv(t *testing.T) {
	t.Setenv("GHCR_USER", "alice")
	t.Setenv("GHCR_TOKEN", "fake-tok-value")
	t.Setenv("MOLECULE_IMAGE_REGISTRY", "004947743811.dkr.ecr.us-east-2.amazonaws.com/molecule-ai")

	got := ghcrAuthHeader()
	if got == "" {
		t.Fatal("expected non-empty auth header")
	}
	raw, err := base64.URLEncoding.DecodeString(got)
	if err != nil {
		t.Fatalf("auth header is not valid base64-url: %v", err)
	}
	var payload map[string]string
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decoded auth is not valid JSON: %v (raw=%s)", err, raw)
	}
	want := "004947743811.dkr.ecr.us-east-2.amazonaws.com"
	if payload["serveraddress"] != want {
		t.Errorf("serveraddress: got %q, want %q (must follow MOLECULE_IMAGE_REGISTRY host)",
			payload["serveraddress"], want)
	}
	// Sanity: the org-path portion must NOT leak into serveraddress.
	if payload["serveraddress"] == "004947743811.dkr.ecr.us-east-2.amazonaws.com/molecule-ai" {
		t.Error("serveraddress must be host-only, not host+org-path")
	}
}

func TestGHCRAuthHeader_TrimsWhitespace(t *testing.T) {
	t.Setenv("MOLECULE_IMAGE_REGISTRY", "")
	// .env lines often have trailing newlines or accidental spaces. Without
	// trimming, a stray space would produce an auth payload the engine
	// rejects with a confusing 401.
	t.Setenv("GHCR_USER", "  alice  ")
	t.Setenv("GHCR_TOKEN", "\tfake-tok-value\n")
	got := ghcrAuthHeader()
	raw, _ := base64.URLEncoding.DecodeString(got)
	var payload map[string]string
	_ = json.Unmarshal(raw, &payload)
	if payload["username"] != "alice" {
		t.Errorf("username not trimmed: got %q", payload["username"])
	}
	if payload["password"] != "fake-tok-value" {
		t.Errorf("password not trimmed: got %q", payload["password"])
	}
}
