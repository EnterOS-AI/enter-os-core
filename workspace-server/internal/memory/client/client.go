// Package client is the HTTP client for the memory plugin contract
// defined at docs/api-protocol/memory-plugin-v1.yaml.
//
// This is the only piece of workspace-server that talks to the plugin
// over HTTP. MCP handlers (PR-5) call into Client; the wire is JSON
// using the typed objects in the contract package.
//
// Two operational concerns this package handles:
//
//  1. Capability negotiation. On Boot/Refresh, calls /v1/health,
//     captures the plugin's capability list. MCP handlers consult
//     SupportsCapability before exposing capability-gated features
//     (e.g., semantic search only when "embedding" is reported).
//
//  2. Circuit breaker. After ConfigConsecutiveFailuresToOpen
//     consecutive failures the breaker opens for ConfigBreakerCooldown.
//     While open, calls fail fast with ErrBreakerOpen rather than
//     blocking the request thread on a 2s timeout. Memory is
//     non-critical to a workspace-server response — failing closed
//     would degrade chat latency for everyone.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/memory/contract"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/textutil"
)

const (
	envBaseURL  = "MEMORY_PLUGIN_URL"
	envTimeout  = "MEMORY_PLUGIN_TIMEOUT"
	defaultBase = "http://localhost:9100"

	defaultTimeout = 2 * time.Second

	// ConfigConsecutiveFailuresToOpen — three timeouts in a row is
	// long enough to be confident the plugin is misbehaving rather
	// than a transient blip. Two would chatter on transient blips;
	// five is too forgiving.
	ConfigConsecutiveFailuresToOpen = 3

	// ConfigBreakerCooldown — how long the breaker stays open before
	// allowing one probe through. Picked at 60s as a balance: long
	// enough that a flapping plugin doesn't get hammered, short
	// enough that recovery is felt within a single user session.
	ConfigBreakerCooldown = 60 * time.Second
)

// ErrBreakerOpen is returned when a request is rejected because the
// circuit breaker is open. Callers SHOULD treat this as "memory
// unavailable, return empty" rather than surfacing the error to the
// agent.
var ErrBreakerOpen = errors.New("memory-plugin: circuit breaker open")

// Doer is the minimal HTTP interface the client needs. *http.Client
// satisfies it; tests inject a mock.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Config tunes Client behavior. Zero value uses sensible defaults.
type Config struct {
	BaseURL string
	Timeout time.Duration
	HTTP    Doer

	// Now lets tests inject a deterministic clock for breaker tests.
	// Production callers leave this nil; we fall back to time.Now.
	Now func() time.Time
}

// Client talks to a memory plugin. Safe for concurrent use.
type Client struct {
	baseURL  string
	http     Doer
	now      func() time.Time

	mu             sync.RWMutex
	caps           *contract.HealthResponse
	failures       int
	breakerOpenedAt time.Time
}

// New constructs a Client. Uses MEMORY_PLUGIN_URL +
// MEMORY_PLUGIN_TIMEOUT env vars when cfg fields are unset.
func New(cfg Config) *Client {
	base := cfg.BaseURL
	if base == "" {
		base = strings.TrimRight(os.Getenv(envBaseURL), "/")
	}
	if base == "" {
		base = defaultBase
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		if t, ok := parseDurationEnv(os.Getenv(envTimeout)); ok {
			timeout = t
		} else {
			timeout = defaultTimeout
		}
	}
	httpClient := cfg.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: timeout}
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Client{
		baseURL: base,
		http:    httpClient,
		now:     now,
	}
}

func parseDurationEnv(s string) (time.Duration, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}

// BaseURL is exposed for diagnostic logging only.
func (c *Client) BaseURL() string { return c.baseURL }

// Capabilities returns the most recent /v1/health response. nil before
// the first successful Boot/Refresh.
func (c *Client) Capabilities() *contract.HealthResponse {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.caps
}

// SupportsCapability is a convenience wrapper around
// Capabilities().HasCapability(c). False before first Boot or if the
// plugin doesn't advertise it.
func (c *Client) SupportsCapability(cap string) bool {
	return c.Capabilities().HasCapability(cap)
}

// Boot performs the initial health check + capability snapshot. Called
// once at workspace-server startup. Returns the parsed health
// response. On failure, returns the error and leaves Capabilities()
// nil so MCP handlers can treat the plugin as effectively unavailable
// (every capability check will return false).
func (c *Client) Boot(ctx context.Context) (*contract.HealthResponse, error) {
	return c.refresh(ctx)
}

// Refresh re-runs the health check. MCP handlers MAY call this on a
// cadence; not required. Currently a thin alias of Boot.
func (c *Client) Refresh(ctx context.Context) (*contract.HealthResponse, error) {
	return c.refresh(ctx)
}

func (c *Client) refresh(ctx context.Context) (*contract.HealthResponse, error) {
	var resp contract.HealthResponse
	if err := c.doJSON(ctx, http.MethodGet, "/v1/health", nil, &resp); err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.caps = &resp
	c.mu.Unlock()
	return &resp, nil
}

// --- Namespace endpoints ---

// UpsertNamespace calls PUT /v1/namespaces/{name}.
func (c *Client) UpsertNamespace(ctx context.Context, name string, body contract.NamespaceUpsert) (*contract.Namespace, error) {
	if err := contract.ValidateNamespaceName(name); err != nil {
		return nil, err
	}
	if err := body.Validate(); err != nil {
		return nil, err
	}
	var resp contract.Namespace
	path := "/v1/namespaces/" + url.PathEscape(name)
	if err := c.doJSON(ctx, http.MethodPut, path, body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// PatchNamespace calls PATCH /v1/namespaces/{name}.
func (c *Client) PatchNamespace(ctx context.Context, name string, body contract.NamespacePatch) (*contract.Namespace, error) {
	if err := contract.ValidateNamespaceName(name); err != nil {
		return nil, err
	}
	if err := body.Validate(); err != nil {
		return nil, err
	}
	var resp contract.Namespace
	path := "/v1/namespaces/" + url.PathEscape(name)
	if err := c.doJSON(ctx, http.MethodPatch, path, body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// DeleteNamespace calls DELETE /v1/namespaces/{name}.
func (c *Client) DeleteNamespace(ctx context.Context, name string) error {
	if err := contract.ValidateNamespaceName(name); err != nil {
		return err
	}
	path := "/v1/namespaces/" + url.PathEscape(name)
	return c.doJSON(ctx, http.MethodDelete, path, nil, nil)
}

// --- Memory endpoints ---

// CommitMemory calls POST /v1/namespaces/{name}/memories.
func (c *Client) CommitMemory(ctx context.Context, namespace string, body contract.MemoryWrite) (*contract.MemoryWriteResponse, error) {
	if err := contract.ValidateNamespaceName(namespace); err != nil {
		return nil, err
	}
	if err := body.Validate(); err != nil {
		return nil, err
	}
	var resp contract.MemoryWriteResponse
	path := "/v1/namespaces/" + url.PathEscape(namespace) + "/memories"
	if err := c.doJSON(ctx, http.MethodPost, path, body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Search calls POST /v1/search.
func (c *Client) Search(ctx context.Context, body contract.SearchRequest) (*contract.SearchResponse, error) {
	if err := body.Validate(); err != nil {
		return nil, err
	}
	var resp contract.SearchResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v1/search", body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ForgetMemory calls DELETE /v1/memories/{id}.
func (c *Client) ForgetMemory(ctx context.Context, id string, body contract.ForgetRequest) error {
	if id == "" {
		return errors.New("memory id is empty")
	}
	if err := body.Validate(); err != nil {
		return err
	}
	path := "/v1/memories/" + url.PathEscape(id)
	return c.doJSON(ctx, http.MethodDelete, path, body, nil)
}

// --- HTTP plumbing ---

func (c *Client) doJSON(ctx context.Context, method, path string, reqBody interface{}, respBody interface{}) error {
	if c.breakerIsOpen() {
		return ErrBreakerOpen
	}

	var body io.Reader
	if reqBody != nil {
		buf, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("marshal: %w", err)
		}
		body = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		c.recordFailure()
		return fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 {
		// 5xx counts toward breaker; 4xx does not (those are client
		// bugs, not plugin health issues).
		c.recordFailure()
		return decodeError(resp)
	}
	if resp.StatusCode >= 400 {
		// Don't open the breaker on 4xx, but do reset failure count
		// because the request reached the plugin and got a coherent
		// response — plugin is alive.
		c.recordSuccess()
		return decodeError(resp)
	}

	c.recordSuccess()

	if respBody == nil {
		return nil
	}
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(respBody); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return nil
}

func decodeError(resp *http.Response) error {
	var e contract.Error
	body, _ := io.ReadAll(resp.Body)
	if len(body) == 0 {
		return &contract.Error{
			Code:    httpStatusToCode(resp.StatusCode),
			Message: fmt.Sprintf("status %d (empty body)", resp.StatusCode),
		}
	}
	if err := json.Unmarshal(body, &e); err != nil || e.Code == "" {
		// Plugin returned a non-standard error body; surface what we
		// have rather than dropping it.
		return &contract.Error{
			Code:    httpStatusToCode(resp.StatusCode),
			Message: fmt.Sprintf("status %d: %s", resp.StatusCode, textutil.TruncateBytes(string(body), 256)),
		}
	}
	return &e
}

func httpStatusToCode(status int) contract.ErrorCode {
	switch {
	case status == http.StatusNotFound:
		return contract.ErrorCodeNotFound
	case status == http.StatusForbidden:
		return contract.ErrorCodeForbidden
	case status >= 500:
		return contract.ErrorCodeInternal
	default:
		return contract.ErrorCodeBadRequest
	}
}

// truncation moved to internal/textutil.TruncateBytes (#2962 SSOT).

// --- Circuit breaker ---

func (c *Client) breakerIsOpen() bool {
	c.mu.RLock()
	openedAt := c.breakerOpenedAt
	c.mu.RUnlock()
	if openedAt.IsZero() {
		return false
	}
	if c.now().Sub(openedAt) >= ConfigBreakerCooldown {
		// Cooldown elapsed — let the next request through. Reset
		// counters so a single successful call closes the breaker.
		c.mu.Lock()
		c.breakerOpenedAt = time.Time{}
		c.failures = 0
		c.mu.Unlock()
		return false
	}
	return true
}

func (c *Client) recordFailure() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failures++
	if c.failures >= ConfigConsecutiveFailuresToOpen && c.breakerOpenedAt.IsZero() {
		c.breakerOpenedAt = c.now()
	}
}

func (c *Client) recordSuccess() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failures = 0
	c.breakerOpenedAt = time.Time{}
}

// --- Diagnostic accessors for tests ---

// Failures returns the current consecutive-failure count.
func (c *Client) Failures() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.failures
}

// BreakerOpen reports whether the breaker is currently open.
func (c *Client) BreakerOpen() bool { return c.breakerIsOpen() }
