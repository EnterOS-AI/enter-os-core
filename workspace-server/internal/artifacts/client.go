// Package artifacts provides a minimal Go client for the Cloudflare Artifacts
// REST API (private beta Apr 2026, public beta May 2026).
//
// API reference: https://developers.cloudflare.com/artifacts/api/rest-api/
// Blog post:     https://blog.cloudflare.com/artifacts-git-for-agents-beta/
//
// Base URL: https://artifacts.cloudflare.net/v1/api/namespaces/{namespace}
// Auth:     Authorization: Bearer <CLOUDFLARE_API_TOKEN>
//
// This client covers the subset of operations needed for the Molecule AI
// workspace-snapshot demo:
//   - CreateRepo  — provision a bare Git repo for a workspace
//   - GetRepo     — fetch repo metadata (remote URL, created_at, …)
//   - ForkRepo    — create an isolated copy (e.g. workspace branching)
//   - ImportRepo  — bootstrap from an external GitHub/GitLab URL
//   - DeleteRepo  — clean-up
//   - CreateToken — mint a short-lived Git credential for clone/push
//   - RevokeToken — invalidate an issued token
package artifacts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const (
	defaultBaseURL = "https://artifacts.cloudflare.net/v1/api"
	defaultTimeout = 30 * time.Second
)

// Client is a thin HTTP wrapper around the Cloudflare Artifacts REST API.
// Instantiate with New(); override BaseURL in tests via NewWithBaseURL().
type Client struct {
	baseURL    string // e.g. https://artifacts.cloudflare.net/v1/api/namespaces/my-ns
	apiToken   string // Cloudflare API token — never logged
	httpClient *http.Client
}

// New returns a Client scoped to the given namespace.
// apiToken is a Cloudflare API token with Artifacts write permissions.
// namespace identifies the CF Artifacts namespace (maps to CLOUDFLARE_ARTIFACTS_NAMESPACE).
func New(apiToken, namespace string) *Client {
	return NewWithBaseURL(apiToken, namespace, defaultBaseURL)
}

// NewWithBaseURL is the same as New but lets callers override the base URL —
// primarily used in unit tests to point at an httptest.Server.
func NewWithBaseURL(apiToken, namespace, baseURL string) *Client {
	ns := url.PathEscape(namespace)
	return &Client{
		baseURL:  fmt.Sprintf("%s/namespaces/%s", baseURL, ns),
		apiToken: apiToken,
		httpClient: &http.Client{
			Timeout: defaultTimeout,
		},
	}
}

// ---- Domain types --------------------------------------------------------

// Repo represents a single Cloudflare Artifacts repository.
type Repo struct {
	// Name is the user-supplied identifier within the namespace.
	Name string `json:"name"`
	// ID is the opaque CF-assigned identifier.
	ID string `json:"id,omitempty"`
	// RemoteURL is the authenticated Git remote in the form
	// https://x:<TOKEN>@<hash>.artifacts.cloudflare.net/git/repo-<id>.git
	RemoteURL string `json:"remote_url,omitempty"`
	// ReadOnly marks repos that accept only fetch/clone operations.
	ReadOnly bool `json:"read_only,omitempty"`
	// Description is an optional human-readable label.
	Description string `json:"description,omitempty"`
	CreatedAt   time.Time `json:"created_at,omitempty"`
	UpdatedAt   time.Time `json:"updated_at,omitempty"`
}

// ForkResult is the response body from POST /repos/:name/fork.
type ForkResult struct {
	Repo        Repo `json:"repo"`
	ObjectCount int  `json:"object_count,omitempty"`
}

// RepoToken is a short-lived credential for Git operations against a single repo.
// The plaintext Token value is returned only once — callers must store it.
type RepoToken struct {
	ID        string    `json:"id"`
	Token     string    `json:"token"`
	Scope     string    `json:"scope"`  // "read" | "write"
	ExpiresAt time.Time `json:"expires_at"`
}

// ---- Request payloads ----------------------------------------------------

// CreateRepoRequest is the body for POST /repos.
type CreateRepoRequest struct {
	Name          string `json:"name"`
	Description   string `json:"description,omitempty"`
	DefaultBranch string `json:"default_branch,omitempty"`
	ReadOnly      bool   `json:"read_only,omitempty"`
}

// ForkRepoRequest is the body for POST /repos/:name/fork.
type ForkRepoRequest struct {
	Name              string `json:"name"`
	Description       string `json:"description,omitempty"`
	ReadOnly          bool   `json:"read_only,omitempty"`
	DefaultBranchOnly bool   `json:"default_branch_only,omitempty"`
}

// ImportRepoRequest is the body for POST /repos/:name/import.
type ImportRepoRequest struct {
	// URL is the HTTPS URL of the source Git repository.
	URL      string `json:"url"`
	Branch   string `json:"branch,omitempty"`
	Depth    int    `json:"depth,omitempty"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

// CreateTokenRequest is the body for POST /tokens.
type CreateTokenRequest struct {
	// Repo is the name of the repository to scope the token to.
	Repo  string `json:"repo"`
	Scope string `json:"scope,omitempty"` // "read" | "write"; default "write"
	// TTL is the lifetime in seconds. Default 86400 (24h).
	TTL int `json:"ttl,omitempty"`
}

// ---- API error -----------------------------------------------------------

// APIError represents a non-2xx response from the Cloudflare v4 envelope.
type APIError struct {
	StatusCode int
	Code       int    `json:"code"`
	Message    string `json:"message"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("cloudflare artifacts: HTTP %d — code %d: %s", e.StatusCode, e.Code, e.Message)
}

// ---- HTTP helpers --------------------------------------------------------

// do executes an HTTP request, checks the Cloudflare v4 envelope, and
// JSON-decodes the "result" field into out (pass nil to discard).
func (c *Client) do(ctx context.Context, method, path string, body, out interface{}) error {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("artifacts: marshal request: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return fmt.Errorf("artifacts: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("artifacts: request %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Decode the Cloudflare v4 envelope. Cap at 1 MiB to prevent a
	// malicious or runaway upstream response from exhausting memory.
	var envelope struct {
		Result  json.RawMessage `json:"result"`
		Success bool            `json:"success"`
		Errors  []struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&envelope); err != nil {
		// Non-JSON body (network error page, etc.)
		return &APIError{StatusCode: resp.StatusCode, Message: fmt.Sprintf("non-JSON body (status %d)", resp.StatusCode)}
	}

	if !envelope.Success || resp.StatusCode >= 300 {
		apiErr := &APIError{StatusCode: resp.StatusCode}
		if len(envelope.Errors) > 0 {
			apiErr.Code = envelope.Errors[0].Code
			apiErr.Message = envelope.Errors[0].Message
		} else {
			apiErr.Message = "unknown error"
		}
		return apiErr
	}

	if out != nil && len(envelope.Result) > 0 {
		if err := json.Unmarshal(envelope.Result, out); err != nil {
			return fmt.Errorf("artifacts: decode result: %w", err)
		}
	}
	return nil
}

// ---- Repo operations -----------------------------------------------------

// CreateRepo provisions a new bare Git repo in the namespace.
// Corresponds to POST /repos.
func (c *Client) CreateRepo(ctx context.Context, req CreateRepoRequest) (*Repo, error) {
	var repo Repo
	if err := c.do(ctx, http.MethodPost, "/repos", req, &repo); err != nil {
		return nil, err
	}
	return &repo, nil
}

// GetRepo fetches metadata for an existing repo.
// Corresponds to GET /repos/:name.
func (c *Client) GetRepo(ctx context.Context, name string) (*Repo, error) {
	var repo Repo
	path := "/repos/" + url.PathEscape(name)
	if err := c.do(ctx, http.MethodGet, path, nil, &repo); err != nil {
		return nil, err
	}
	return &repo, nil
}

// ForkRepo creates an isolated copy of an existing repo.
// Corresponds to POST /repos/:name/fork.
func (c *Client) ForkRepo(ctx context.Context, sourceName string, req ForkRepoRequest) (*ForkResult, error) {
	var result ForkResult
	path := "/repos/" + url.PathEscape(sourceName) + "/fork"
	if err := c.do(ctx, http.MethodPost, path, req, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// ImportRepo bootstraps a repo from an external HTTPS Git URL.
// Corresponds to POST /repos/:name/import.
func (c *Client) ImportRepo(ctx context.Context, name string, req ImportRepoRequest) (*Repo, error) {
	var repo Repo
	path := "/repos/" + url.PathEscape(name) + "/import"
	if err := c.do(ctx, http.MethodPost, path, req, &repo); err != nil {
		return nil, err
	}
	return &repo, nil
}

// DeleteRepo deletes a repo (returns 202 Accepted).
// Corresponds to DELETE /repos/:name.
func (c *Client) DeleteRepo(ctx context.Context, name string) error {
	path := "/repos/" + url.PathEscape(name)
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

// ---- Token operations ----------------------------------------------------

// CreateToken mints a short-lived Git credential scoped to a single repo.
// The plaintext token is in the returned RepoToken.Token field — it will not
// be available again after this call returns.
// Corresponds to POST /tokens.
func (c *Client) CreateToken(ctx context.Context, req CreateTokenRequest) (*RepoToken, error) {
	var token RepoToken
	if err := c.do(ctx, http.MethodPost, "/tokens", req, &token); err != nil {
		return nil, err
	}
	return &token, nil
}

// RevokeToken invalidates an issued token by its ID.
// Corresponds to DELETE /tokens/:id.
func (c *Client) RevokeToken(ctx context.Context, tokenID string) error {
	path := "/tokens/" + url.PathEscape(tokenID)
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}
