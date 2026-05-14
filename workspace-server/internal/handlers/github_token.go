// Package handlers — GitHub App installation-token refresh endpoint.
//
// GET /admin/github-installation-token returns a fresh GitHub App
// installation token on demand. Long-running workspace containers use
// this as a git credential helper and for explicit `gh auth` re-runs
// so they never operate with an expired GH_TOKEN.
//
// # Why this endpoint?
//
// The github-app-auth plugin (PR #506) injects GH_TOKEN + GITHUB_TOKEN
// into a workspace container's env at provision time. Those tokens are
// GitHub App installation tokens with a fixed ~60 min TTL. The plugin
// keeps a server-side in-process cache and proactively refreshes it
// 5 min before expiry, but the workspace env is set once at container
// start and never updated — so any workspace alive >60 min ends up with
// an expired token (issue #547).
//
// The fix is:
//
//  1. Platform side (this file): expose GET /admin/github-installation-token.
//     The handler delegates to the registered TokenProvider (typically the
//     github-app-auth plugin), whose cache is always fresh. Gated behind
//     AdminAuth — any valid workspace bearer token can call it.
//
//  2. Workspace side: a shell credential helper
//     (workspace/scripts/molecule-git-token-helper.sh) configured
//     as the git credential helper. git calls it on every push/fetch;
//     it hits this endpoint and emits the fresh token to stdout. A 30-min
//     cron also runs `gh auth login --with-token` using the same helper.
//
// # Approach chosen
//
// Option B (pre-flight/on-demand): workspaces poll for a token when
// they need one (credential helper callback). This is preferable over a
// background goroutine pusher (Option A) because:
//
//   - The plugin already maintains its own refresh cache — there is no
//     token to refresh on the platform side.
//   - Pushing a new token into running containers requires docker exec /
//     env mutation, which the architecture explicitly rejects (see issue
//     #547 "Alternatives considered").
//   - On-demand is pull-based, stateless, and trivially testable.
package handlers

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/Molecule-AI/molecule-monorepo/platform/pkg/provisionhook"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

// GitHubTokenHandler serves GET /admin/github-installation-token.
type GitHubTokenHandler struct {
	registry *provisionhook.Registry
}

// NewGitHubTokenHandler constructs the handler. registry may be nil when
// no GitHub App plugin is registered (dev / self-hosted deployments).
func NewGitHubTokenHandler(reg *provisionhook.Registry) *GitHubTokenHandler {
	return &GitHubTokenHandler{registry: reg}
}

// GetInstallationToken handles GET /admin/github-installation-token.
//
// Returns:
//
//	200 {"token": "ghs_...", "expires_at": "2026-04-17T22:50:00Z"}
//	404 {"error": "no GitHub App configured"}  — GITHUB_APP_ID not set
//	404 {"error": "no token provider registered"} — plugin loaded but
//	     doesn't implement TokenProvider
//	500 {"error": "token refresh failed"}  — provider returned error
//
// The 404 vs 403 distinction is intentional: a 404 means the feature is
// simply not configured, not that the caller is forbidden. This matches
// the pattern used by GET /admin/workspaces/:id/test-token.
//
// Callers must retry with exponential back-off on 500 — a transient
// upstream GitHub API error should not permanently block git operations.
func (h *GitHubTokenHandler) GetInstallationToken(c *gin.Context) {
	if h.registry == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "no GitHub App configured"})
		return
	}

	provider := h.registry.FirstTokenProvider()
	if provider == nil {
		// #960/#1101: Plugin's TokenProvider interface fails due to Go module
		// boundary. Fall back to direct App token generation using env vars.
		// TODO: refactor into a platform-level CredentialRefreshHook (#1101)
		log.Printf("[github] no TokenProvider in registry — using env-based fallback")
		token, expiresAt, err := generateAppInstallationToken()
		if err != nil {
			log.Printf("[github] fallback token generation failed: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "token refresh failed"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"token": token, "expires_at": expiresAt})
		return
	}

	token, expiresAt, err := provider.Token(c.Request.Context())
	if err != nil {
		log.Printf("[github] token refresh failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "token refresh failed"})
		return
	}

	if token == "" {
		log.Printf("[github] token provider returned empty token")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "token refresh failed: empty token"})
		return
	}

	// Never log the token itself.
	log.Printf("[github] served fresh installation token (expires %s, TTL %.0fs)",
		expiresAt.Format(time.RFC3339),
		time.Until(expiresAt).Seconds())

	c.JSON(http.StatusOK, gin.H{
		"token":      token,
		"expires_at": expiresAt.UTC().Format(time.RFC3339),
	})
}

// generateAppInstallationToken generates a GitHub App installation token
// directly from env vars. Temporary fallback for #960 (Go module boundary
// prevents plugin TokenProvider from matching). Tracked for refactor in #1101.
func generateAppInstallationToken() (string, time.Time, error) {
	appID, _ := strconv.ParseInt(os.Getenv("GITHUB_APP_ID"), 10, 64)
	installID, _ := strconv.ParseInt(os.Getenv("GITHUB_APP_INSTALLATION_ID"), 10, 64)
	keyFile := os.Getenv("GITHUB_APP_PRIVATE_KEY_FILE")
	if appID == 0 || installID == 0 || keyFile == "" {
		return "", time.Time{}, fmt.Errorf("GITHUB_APP_ID/INSTALLATION_ID/PRIVATE_KEY_FILE required")
	}
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("read key: %w", err)
	}
	rsaKey, err := jwt.ParseRSAPrivateKeyFromPEM(keyPEM)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("parse key: %w", err)
	}
	now := time.Now()
	signed, err := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(10 * time.Minute).Unix(),
		"iss": appID,
	}).SignedString(rsaKey)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign JWT: %w", err)
	}
	req, _ := http.NewRequest("POST", fmt.Sprintf("https://api.github.com/app/installations/%d/access_tokens", installID), nil)
	req.Header.Set("Authorization", "Bearer "+signed)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", time.Time{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	var result struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", time.Time{}, err
	}
	if result.Token == "" {
		return "", time.Time{}, fmt.Errorf("empty token (status %d)", resp.StatusCode)
	}
	return result.Token, result.ExpiresAt, nil
}
