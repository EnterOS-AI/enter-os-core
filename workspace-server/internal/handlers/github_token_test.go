package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Molecule-AI/molecule-monorepo/platform/pkg/provisionhook"
	"github.com/gin-gonic/gin"
)

// ─── mock helpers ────────────────────────────────────────────────────────────

// mockMutatorOnly implements EnvMutator but NOT TokenProvider.
type mockMutatorOnly struct{ name string }

func (m *mockMutatorOnly) Name() string { return m.name }
func (m *mockMutatorOnly) MutateEnv(_ context.Context, _ string, _ map[string]string) error {
	return nil
}

// mockTokenMutator implements both EnvMutator and TokenProvider.
// Set err to simulate a provider failure; otherwise returns token + expiresAt.
type mockTokenMutator struct {
	name      string
	token     string
	expiresAt time.Time
	err       error
}

func (m *mockTokenMutator) Name() string { return m.name }
func (m *mockTokenMutator) MutateEnv(_ context.Context, _ string, _ map[string]string) error {
	return nil
}
func (m *mockTokenMutator) Token(_ context.Context) (string, time.Time, error) {
	return m.token, m.expiresAt, m.err
}

// ─── request helper ──────────────────────────────────────────────────────────

func newGitHubTokenRequest() (*httptest.ResponseRecorder, *gin.Context) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/admin/github-installation-token", nil)
	return w, c
}

// ─── tests ───────────────────────────────────────────────────────────────────

// TestGitHubToken_NilRegistry — no GitHub App plugin loaded at all.
// Expect 404 so operators can distinguish "not configured" from "forbidden".
func TestGitHubToken_NilRegistry(t *testing.T) {
	h := NewGitHubTokenHandler(nil)
	w, c := newGitHubTokenRequest()

	h.GetInstallationToken(c)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for nil registry, got %d: %s", w.Code, w.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if body["error"] == "" {
		t.Error("expected non-empty error field in response")
	}
}

// TestGitHubToken_NoTokenProvider — plugin registered but doesn't
// implement TokenProvider (e.g. a non-GitHub mutator in the chain).
//
// Post-#960/#1101 the handler now falls back to direct env-based App
// token generation (GITHUB_APP_ID / INSTALLATION_ID / PRIVATE_KEY_FILE)
// when no registered provider matches. In the test environment those
// env vars are unset, so the fallback fails with 500 "token refresh
// failed" — a clean retryable signal for the workspace credential
// helper. Previously this path returned 404; the new 500 matches the
// ProviderError shape so callers don't have to branch on "missing
// provider" vs "provider failed".
func TestGitHubToken_NoTokenProvider(t *testing.T) {
	reg := provisionhook.NewRegistry()
	reg.Register(&mockMutatorOnly{name: "other-plugin"})
	h := NewGitHubTokenHandler(reg)
	w, c := newGitHubTokenRequest()

	h.GetInstallationToken(c)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 (env-based fallback fails with unset GITHUB_APP_* vars), got %d: %s",
			w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "token refresh failed") {
		t.Errorf("expected body to contain 'token refresh failed', got: %s", w.Body.String())
	}
}

// TestGitHubToken_ProviderError — provider returns an error (e.g. GitHub API
// unreachable). Expect 500 so the workspace credential helper retries.
func TestGitHubToken_ProviderError(t *testing.T) {
	reg := provisionhook.NewRegistry()
	reg.Register(&mockTokenMutator{
		name: "github-app-auth",
		err:  errors.New("github: 503 service unavailable"),
	})
	h := NewGitHubTokenHandler(reg)
	w, c := newGitHubTokenRequest()

	h.GetInstallationToken(c)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on provider error, got %d: %s", w.Code, w.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if body["error"] == "" {
		t.Error("expected non-empty error field in 500 response")
	}
}

// TestGitHubToken_EmptyToken — provider returns no error but an empty token.
// This should never happen in normal operation but is a programming error in
// the plugin; treat it as a refresh failure.
func TestGitHubToken_EmptyToken(t *testing.T) {
	exp := time.Now().Add(55 * time.Minute)
	reg := provisionhook.NewRegistry()
	reg.Register(&mockTokenMutator{
		name:      "github-app-auth",
		token:     "", // empty — plugin bug
		expiresAt: exp,
	})
	h := NewGitHubTokenHandler(reg)
	w, c := newGitHubTokenRequest()

	h.GetInstallationToken(c)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for empty token, got %d: %s", w.Code, w.Body.String())
	}
}

// TestGitHubToken_HappyPath — provider returns a valid token.
// Assert: 200, token present, expires_at is a valid RFC3339 timestamp
// with a positive TTL (i.e. the token is not already expired).
func TestGitHubToken_HappyPath(t *testing.T) {
	exp := time.Now().UTC().Add(55 * time.Minute).Truncate(time.Second)
	reg := provisionhook.NewRegistry()
	reg.Register(&mockTokenMutator{
		name:      "github-app-auth",
		token:     "ghs_TestTokenABC123",
		expiresAt: exp,
	})
	h := NewGitHubTokenHandler(reg)
	w, c := newGitHubTokenRequest()

	h.GetInstallationToken(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var body struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}

	if body.Token != "ghs_TestTokenABC123" {
		t.Errorf("expected token 'ghs_TestTokenABC123', got %q", body.Token)
	}

	parsed, err := time.Parse(time.RFC3339, body.ExpiresAt)
	if err != nil {
		t.Fatalf("expires_at is not valid RFC3339: %q — %v", body.ExpiresAt, err)
	}
	if !parsed.After(time.Now()) {
		t.Errorf("expires_at %s is in the past — handler served an expired token", body.ExpiresAt)
	}
}

// TestGitHubToken_FirstProviderWins — two mutators registered; only the first
// implements TokenProvider. Confirm the first one is used (registration order).
func TestGitHubToken_FirstProviderWins(t *testing.T) {
	exp := time.Now().UTC().Add(55 * time.Minute)
	reg := provisionhook.NewRegistry()
	reg.Register(&mockTokenMutator{
		name:      "first-provider",
		token:     "ghs_First",
		expiresAt: exp,
	})
	reg.Register(&mockTokenMutator{
		name:      "second-provider",
		token:     "ghs_Second",
		expiresAt: exp,
	})
	h := NewGitHubTokenHandler(reg)
	w, c := newGitHubTokenRequest()

	h.GetInstallationToken(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var body map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["token"] != "ghs_First" {
		t.Errorf("expected first provider's token 'ghs_First', got %q", body["token"])
	}
}

// TestGitHubToken_NonProviderBeforeProvider — a plain EnvMutator is registered
// first, then a TokenProvider. Confirm the provider is still found (skip over
// non-providers).
func TestGitHubToken_NonProviderBeforeProvider(t *testing.T) {
	exp := time.Now().UTC().Add(55 * time.Minute)
	reg := provisionhook.NewRegistry()
	reg.Register(&mockMutatorOnly{name: "env-injector"})
	reg.Register(&mockTokenMutator{
		name:      "github-app-auth",
		token:     "ghs_FoundBehindOther",
		expiresAt: exp,
	})
	h := NewGitHubTokenHandler(reg)
	w, c := newGitHubTokenRequest()

	h.GetInstallationToken(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var body map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["token"] != "ghs_FoundBehindOther" {
		t.Errorf("expected 'ghs_FoundBehindOther', got %q", body["token"])
	}
}
