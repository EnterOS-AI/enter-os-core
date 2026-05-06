package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/memory/contract"
)

// roundTripperFunc lets tests inject a fully synthetic transport.
// Avoids spinning up an httptest.Server for unit tests focused on
// breaker / decode behavior.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResp(status int, body interface{}) *http.Response {
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(string(b))),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}
}

func emptyResp(status int) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader("")),
	}
}

// --- New / config ---

func TestNew_DefaultsApply(t *testing.T) {
	t.Setenv(envBaseURL, "")
	t.Setenv(envTimeout, "")
	c := New(Config{})
	if c.baseURL != defaultBase {
		t.Errorf("baseURL = %q, want %q", c.baseURL, defaultBase)
	}
}

func TestNew_BaseURLFromEnv(t *testing.T) {
	t.Setenv(envBaseURL, "http://example.com:9100/")
	c := New(Config{})
	if c.baseURL != "http://example.com:9100" {
		t.Errorf("baseURL = %q, want trimmed env value", c.baseURL)
	}
}

func TestNew_BaseURLFromConfigOverridesEnv(t *testing.T) {
	t.Setenv(envBaseURL, "http://from-env:9100")
	c := New(Config{BaseURL: "http://from-cfg:9100"})
	if c.baseURL != "http://from-cfg:9100" {
		t.Errorf("baseURL = %q, want config value", c.baseURL)
	}
}

func TestNew_TimeoutFromEnv(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want time.Duration
	}{
		{"5s", "5s", 5 * time.Second},
		{"empty falls through", "", defaultTimeout},
		{"invalid falls through", "bogus", defaultTimeout},
		{"zero falls through", "0s", defaultTimeout},
		{"negative falls through", "-1s", defaultTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envTimeout, tc.env)
			t.Setenv(envBaseURL, "http://x")
			// We can't read timeout from Client (it's on the http.Client
			// inside), so we exercise it indirectly: parseDurationEnv
			// returns the same value New uses.
			got, ok := parseDurationEnv(tc.env)
			if !ok {
				got = defaultTimeout
			}
			if got != tc.want {
				t.Errorf("parseDurationEnv(%q) = %v, want %v", tc.env, got, tc.want)
			}
		})
	}
}

func TestBaseURL(t *testing.T) {
	c := New(Config{BaseURL: "http://x"})
	if c.BaseURL() != "http://x" {
		t.Errorf("BaseURL() = %q, want http://x", c.BaseURL())
	}
}

// --- Boot / Refresh / Capabilities ---

func TestBoot_HappyPath(t *testing.T) {
	rt := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v1/health" || r.Method != http.MethodGet {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		return jsonResp(200, contract.HealthResponse{
			Status:       "ok",
			Version:      "1.0.0",
			Capabilities: []string{contract.CapabilityFTS, contract.CapabilityEmbedding},
		}), nil
	})
	c := New(Config{BaseURL: "http://x", HTTP: rt})

	hr, err := c.Boot(context.Background())
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	if hr.Status != "ok" {
		t.Errorf("status = %q", hr.Status)
	}
	if !c.SupportsCapability(contract.CapabilityFTS) {
		t.Error("FTS capability not registered")
	}
	if !c.SupportsCapability(contract.CapabilityEmbedding) {
		t.Error("embedding capability not registered")
	}
	if c.SupportsCapability(contract.CapabilityTTL) {
		t.Error("TTL capability falsely registered")
	}
	if c.Capabilities() == nil {
		t.Error("Capabilities() nil after Boot")
	}
}

func TestBoot_PluginUnreachable(t *testing.T) {
	rt := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	})
	c := New(Config{BaseURL: "http://x", HTTP: rt})
	_, err := c.Boot(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if c.Capabilities() != nil {
		t.Error("Capabilities should be nil on Boot failure")
	}
	if c.SupportsCapability(contract.CapabilityFTS) {
		t.Error("SupportsCapability should be false when plugin unreachable")
	}
}

func TestRefresh_UpdatesCapabilities(t *testing.T) {
	first := true
	rt := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		caps := []string{contract.CapabilityFTS}
		if !first {
			caps = []string{contract.CapabilityFTS, contract.CapabilityEmbedding}
		}
		first = false
		return jsonResp(200, contract.HealthResponse{Status: "ok", Version: "1.0.0", Capabilities: caps}), nil
	})
	c := New(Config{BaseURL: "http://x", HTTP: rt})

	if _, err := c.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	if c.SupportsCapability(contract.CapabilityEmbedding) {
		t.Error("embedding should not be present yet")
	}
	if _, err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if !c.SupportsCapability(contract.CapabilityEmbedding) {
		t.Error("embedding should be present after Refresh")
	}
}

// --- Namespace endpoints ---

func TestUpsertNamespace_HappyPath(t *testing.T) {
	rt := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPut {
			t.Errorf("method = %q", r.Method)
		}
		// URL path must be escaped
		if !strings.Contains(r.URL.Path, "/v1/namespaces/workspace:") {
			t.Errorf("path = %q", r.URL.Path)
		}
		return jsonResp(200, contract.Namespace{
			Name:      "workspace:abc",
			Kind:      contract.NamespaceKindWorkspace,
			CreatedAt: time.Now().UTC(),
		}), nil
	})
	c := New(Config{BaseURL: "http://x", HTTP: rt})
	got, err := c.UpsertNamespace(context.Background(), "workspace:abc", contract.NamespaceUpsert{Kind: contract.NamespaceKindWorkspace})
	if err != nil {
		t.Fatalf("UpsertNamespace: %v", err)
	}
	if got.Name != "workspace:abc" || got.Kind != contract.NamespaceKindWorkspace {
		t.Errorf("got %+v", got)
	}
}

func TestUpsertNamespace_RejectsInvalidName(t *testing.T) {
	c := New(Config{BaseURL: "http://x", HTTP: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		t.Error("HTTP should not be called for invalid name")
		return nil, errors.New("not called")
	})})
	_, err := c.UpsertNamespace(context.Background(), "BAD-NS", contract.NamespaceUpsert{Kind: contract.NamespaceKindWorkspace})
	if err == nil {
		t.Error("expected validation error")
	}
}

func TestUpsertNamespace_RejectsInvalidBody(t *testing.T) {
	c := New(Config{BaseURL: "http://x", HTTP: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		t.Error("HTTP should not be called for invalid body")
		return nil, errors.New("not called")
	})})
	_, err := c.UpsertNamespace(context.Background(), "workspace:abc", contract.NamespaceUpsert{Kind: ""})
	if err == nil {
		t.Error("expected validation error for empty Kind")
	}
}

func TestPatchNamespace_HappyPath(t *testing.T) {
	exp := time.Now().Add(time.Hour).UTC()
	rt := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPatch {
			t.Errorf("method = %q", r.Method)
		}
		return jsonResp(200, contract.Namespace{
			Name:      "team:abc",
			Kind:      contract.NamespaceKindTeam,
			ExpiresAt: &exp,
			CreatedAt: time.Now().UTC(),
		}), nil
	})
	c := New(Config{BaseURL: "http://x", HTTP: rt})
	got, err := c.PatchNamespace(context.Background(), "team:abc", contract.NamespacePatch{ExpiresAt: &exp})
	if err != nil {
		t.Fatalf("PatchNamespace: %v", err)
	}
	if got.ExpiresAt == nil {
		t.Error("ExpiresAt nil")
	}
}

func TestPatchNamespace_RejectsEmptyBody(t *testing.T) {
	c := New(Config{BaseURL: "http://x", HTTP: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		t.Error("HTTP should not be called")
		return nil, errors.New("nope")
	})})
	_, err := c.PatchNamespace(context.Background(), "workspace:abc", contract.NamespacePatch{})
	if err == nil {
		t.Error("expected validation error")
	}
}

func TestPatchNamespace_RejectsInvalidName(t *testing.T) {
	c := New(Config{BaseURL: "http://x", HTTP: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		t.Error("HTTP should not be called for invalid name")
		return nil, errors.New("nope")
	})})
	exp := time.Now().Add(time.Hour).UTC()
	_, err := c.PatchNamespace(context.Background(), "BAD-NS", contract.NamespacePatch{ExpiresAt: &exp})
	if err == nil {
		t.Error("expected validation error")
	}
}

func TestDeleteNamespace_NoContent(t *testing.T) {
	rt := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %q", r.Method)
		}
		return emptyResp(204), nil
	})
	c := New(Config{BaseURL: "http://x", HTTP: rt})
	if err := c.DeleteNamespace(context.Background(), "workspace:abc"); err != nil {
		t.Fatalf("DeleteNamespace: %v", err)
	}
}

func TestDeleteNamespace_RejectsInvalidName(t *testing.T) {
	c := New(Config{BaseURL: "http://x", HTTP: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		t.Error("HTTP should not be called")
		return nil, errors.New("nope")
	})})
	if err := c.DeleteNamespace(context.Background(), "BAD"); err == nil {
		t.Error("expected validation error")
	}
}

// --- Memory endpoints ---

func TestCommitMemory_HappyPath(t *testing.T) {
	rt := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("missing content-type")
		}
		return jsonResp(201, contract.MemoryWriteResponse{ID: "mem-1", Namespace: "workspace:abc"}), nil
	})
	c := New(Config{BaseURL: "http://x", HTTP: rt})
	got, err := c.CommitMemory(context.Background(), "workspace:abc", contract.MemoryWrite{
		Content: "fact x", Kind: contract.MemoryKindFact, Source: contract.MemorySourceAgent,
	})
	if err != nil {
		t.Fatalf("CommitMemory: %v", err)
	}
	if got.ID != "mem-1" {
		t.Errorf("id = %q", got.ID)
	}
}

func TestCommitMemory_RejectsInvalidNamespace(t *testing.T) {
	c := New(Config{BaseURL: "http://x", HTTP: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		t.Error("HTTP should not be called")
		return nil, errors.New("nope")
	})})
	_, err := c.CommitMemory(context.Background(), "BAD", contract.MemoryWrite{
		Content: "x", Kind: contract.MemoryKindFact, Source: contract.MemorySourceAgent,
	})
	if err == nil {
		t.Error("expected validation error")
	}
}

func TestCommitMemory_RejectsInvalidBody(t *testing.T) {
	c := New(Config{BaseURL: "http://x", HTTP: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		t.Error("HTTP should not be called")
		return nil, errors.New("nope")
	})})
	_, err := c.CommitMemory(context.Background(), "workspace:abc", contract.MemoryWrite{Content: ""})
	if err == nil {
		t.Error("expected validation error for empty content")
	}
}

func TestSearch_HappyPath(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	rt := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v1/search" {
			t.Errorf("path = %q", r.URL.Path)
		}
		return jsonResp(200, contract.SearchResponse{
			Memories: []contract.Memory{
				{ID: "id-1", Namespace: "workspace:abc", Content: "x", Kind: contract.MemoryKindFact, Source: contract.MemorySourceAgent, CreatedAt: now},
			},
		}), nil
	})
	c := New(Config{BaseURL: "http://x", HTTP: rt})
	got, err := c.Search(context.Background(), contract.SearchRequest{Namespaces: []string{"workspace:abc"}, Query: "x"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got.Memories) != 1 || got.Memories[0].ID != "id-1" {
		t.Errorf("got %+v", got)
	}
}

func TestSearch_RejectsInvalidBody(t *testing.T) {
	c := New(Config{BaseURL: "http://x", HTTP: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		t.Error("HTTP should not be called")
		return nil, errors.New("nope")
	})})
	_, err := c.Search(context.Background(), contract.SearchRequest{}) // empty namespaces
	if err == nil {
		t.Error("expected validation error")
	}
}

func TestForgetMemory_HappyPath(t *testing.T) {
	rt := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %q", r.Method)
		}
		return emptyResp(204), nil
	})
	c := New(Config{BaseURL: "http://x", HTTP: rt})
	err := c.ForgetMemory(context.Background(), "id-1", contract.ForgetRequest{RequestedByNamespace: "workspace:abc"})
	if err != nil {
		t.Fatalf("ForgetMemory: %v", err)
	}
}

func TestForgetMemory_RejectsEmptyID(t *testing.T) {
	c := New(Config{BaseURL: "http://x", HTTP: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		t.Error("HTTP should not be called")
		return nil, errors.New("nope")
	})})
	err := c.ForgetMemory(context.Background(), "", contract.ForgetRequest{RequestedByNamespace: "workspace:abc"})
	if err == nil {
		t.Error("expected validation error")
	}
}

func TestForgetMemory_RejectsInvalidBody(t *testing.T) {
	c := New(Config{BaseURL: "http://x", HTTP: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		t.Error("HTTP should not be called")
		return nil, errors.New("nope")
	})})
	err := c.ForgetMemory(context.Background(), "id-1", contract.ForgetRequest{}) // empty namespace
	if err == nil {
		t.Error("expected validation error")
	}
}

// --- Error decoding ---

func TestErrorDecoding_StandardEnvelope(t *testing.T) {
	rt := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		return jsonResp(404, contract.Error{Code: contract.ErrorCodeNotFound, Message: "ns gone"}), nil
	})
	c := New(Config{BaseURL: "http://x", HTTP: rt})
	_, err := c.UpsertNamespace(context.Background(), "workspace:abc", contract.NamespaceUpsert{Kind: contract.NamespaceKindWorkspace})
	if err == nil {
		t.Fatal("expected error")
	}
	var ce *contract.Error
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want *contract.Error", err)
	}
	if ce.Code != contract.ErrorCodeNotFound {
		t.Errorf("code = %q", ce.Code)
	}
}

func TestErrorDecoding_NonStandardBody(t *testing.T) {
	rt := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 502,
			Body:       io.NopCloser(strings.NewReader("upstream timeout")),
		}, nil
	})
	c := New(Config{BaseURL: "http://x", HTTP: rt})
	_, err := c.Search(context.Background(), contract.SearchRequest{Namespaces: []string{"workspace:abc"}})
	if err == nil {
		t.Fatal("expected error")
	}
	var ce *contract.Error
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want *contract.Error", err)
	}
	if ce.Code != contract.ErrorCodeInternal {
		t.Errorf("code = %q, want internal (5xx)", ce.Code)
	}
	if !strings.Contains(ce.Message, "upstream timeout") {
		t.Errorf("message lost the body: %q", ce.Message)
	}
}

func TestErrorDecoding_EmptyBody(t *testing.T) {
	rt := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		return emptyResp(403), nil
	})
	c := New(Config{BaseURL: "http://x", HTTP: rt})
	_, err := c.UpsertNamespace(context.Background(), "workspace:abc", contract.NamespaceUpsert{Kind: contract.NamespaceKindWorkspace})
	if err == nil {
		t.Fatal("expected error")
	}
	var ce *contract.Error
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v", err)
	}
	if ce.Code != contract.ErrorCodeForbidden {
		t.Errorf("code = %q", ce.Code)
	}
}

func TestHttpStatusToCode(t *testing.T) {
	cases := []struct {
		status int
		want   contract.ErrorCode
	}{
		{404, contract.ErrorCodeNotFound},
		{403, contract.ErrorCodeForbidden},
		{500, contract.ErrorCodeInternal},
		{502, contract.ErrorCodeInternal},
		{400, contract.ErrorCodeBadRequest},
		{422, contract.ErrorCodeBadRequest},
	}
	for _, tc := range cases {
		if got := httpStatusToCode(tc.status); got != tc.want {
			t.Errorf("httpStatusToCode(%d) = %q, want %q", tc.status, got, tc.want)
		}
	}
}

// Truncate moved to internal/textutil — coverage lives in
// internal/textutil/truncate_test.go (TestTruncateBytes_RuneBoundary).
// memory/client just calls it as a wire-shape helper for error
// messages; no client-specific behavior to pin here.

// --- Circuit breaker ---

func TestBreaker_OpensAfterConsecutiveFailures(t *testing.T) {
	rt := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("network down")
	})
	c := New(Config{BaseURL: "http://x", HTTP: rt})

	for i := 0; i < ConfigConsecutiveFailuresToOpen; i++ {
		_, err := c.Boot(context.Background())
		if err == nil {
			t.Fatalf("[%d] expected error", i)
		}
	}
	if !c.BreakerOpen() {
		t.Errorf("breaker not open after %d failures", ConfigConsecutiveFailuresToOpen)
	}

	// Next call must short-circuit with ErrBreakerOpen, not call HTTP.
	rt2 := roundTripperFunc(func(*http.Request) (*http.Response, error) {
		t.Error("HTTP must not be called when breaker is open")
		return nil, errors.New("not called")
	})
	c.http = rt2
	_, err := c.Boot(context.Background())
	if !errors.Is(err, ErrBreakerOpen) {
		t.Errorf("err = %v, want ErrBreakerOpen", err)
	}
}

func TestBreaker_4xxDoesNotOpen(t *testing.T) {
	rt := roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return jsonResp(404, contract.Error{Code: contract.ErrorCodeNotFound, Message: "x"}), nil
	})
	c := New(Config{BaseURL: "http://x", HTTP: rt})

	for i := 0; i < 10; i++ {
		// All 404s. Should never open the breaker.
		_, _ = c.UpsertNamespace(context.Background(), "workspace:abc", contract.NamespaceUpsert{Kind: contract.NamespaceKindWorkspace})
	}
	if c.BreakerOpen() {
		t.Error("breaker opened on 4xx; should only open on 5xx + transport errors")
	}
	if c.Failures() != 0 {
		t.Errorf("failures = %d, want 0 (4xx resets count because plugin is alive)", c.Failures())
	}
}

func TestBreaker_5xxOpens(t *testing.T) {
	rt := roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return jsonResp(503, contract.Error{Code: contract.ErrorCodeUnavailable, Message: "x"}), nil
	})
	c := New(Config{BaseURL: "http://x", HTTP: rt})
	for i := 0; i < ConfigConsecutiveFailuresToOpen; i++ {
		_, _ = c.UpsertNamespace(context.Background(), "workspace:abc", contract.NamespaceUpsert{Kind: contract.NamespaceKindWorkspace})
	}
	if !c.BreakerOpen() {
		t.Error("breaker should open after 3 consecutive 5xx")
	}
}

func TestBreaker_ClosesOnSuccessAfterCooldown(t *testing.T) {
	now := time.Now()
	calls := 0
	rt := roundTripperFunc(func(*http.Request) (*http.Response, error) {
		calls++
		if calls <= ConfigConsecutiveFailuresToOpen {
			return nil, errors.New("dead")
		}
		return jsonResp(200, contract.HealthResponse{Status: "ok", Version: "1.0.0"}), nil
	})
	c := New(Config{
		BaseURL: "http://x",
		HTTP:    rt,
		Now:     func() time.Time { return now },
	})

	// Trip the breaker.
	for i := 0; i < ConfigConsecutiveFailuresToOpen; i++ {
		_, _ = c.Boot(context.Background())
	}
	if !c.BreakerOpen() {
		t.Fatal("breaker must be open")
	}

	// Within cooldown — still open.
	now = now.Add(ConfigBreakerCooldown / 2)
	if !c.BreakerOpen() {
		t.Error("breaker must remain open within cooldown")
	}

	// After cooldown — closed, next call goes through.
	now = now.Add(ConfigBreakerCooldown)
	if c.BreakerOpen() {
		t.Error("breaker must close after cooldown elapses")
	}

	// Successful call resets failure count cleanly.
	if _, err := c.Boot(context.Background()); err != nil {
		t.Errorf("Boot: %v", err)
	}
	if c.Failures() != 0 {
		t.Errorf("failures = %d, want 0 after success", c.Failures())
	}
}

func TestBreaker_SuccessResetsFailureCount(t *testing.T) {
	calls := 0
	rt := roundTripperFunc(func(*http.Request) (*http.Response, error) {
		calls++
		if calls <= 2 {
			return nil, errors.New("flaky")
		}
		return jsonResp(200, contract.HealthResponse{Status: "ok", Version: "1.0.0"}), nil
	})
	c := New(Config{BaseURL: "http://x", HTTP: rt})

	// Two failures (just below threshold), then a success.
	_, _ = c.Boot(context.Background())
	_, _ = c.Boot(context.Background())
	if c.Failures() != 2 {
		t.Errorf("failures = %d, want 2", c.Failures())
	}
	if _, err := c.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	if c.Failures() != 0 {
		t.Errorf("failures = %d, want 0 after success", c.Failures())
	}

	// Now another two failures should NOT trip the breaker (counter was reset).
	rt2 := roundTripperFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("fail") })
	c.http = rt2
	_, _ = c.Boot(context.Background())
	_, _ = c.Boot(context.Background())
	if c.BreakerOpen() {
		t.Error("breaker tripped at 2 failures after intervening success — should not")
	}
}

func TestBreaker_OpenStateBlocksAllEndpoints(t *testing.T) {
	rt := roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dead")
	})
	c := New(Config{BaseURL: "http://x", HTTP: rt})

	// Trip the breaker.
	for i := 0; i < ConfigConsecutiveFailuresToOpen; i++ {
		_, _ = c.Boot(context.Background())
	}

	// Verify every public endpoint short-circuits.
	if _, err := c.UpsertNamespace(context.Background(), "workspace:abc", contract.NamespaceUpsert{Kind: contract.NamespaceKindWorkspace}); !errors.Is(err, ErrBreakerOpen) {
		t.Errorf("UpsertNamespace: %v", err)
	}
	if _, err := c.PatchNamespace(context.Background(), "workspace:abc", contract.NamespacePatch{Metadata: map[string]interface{}{"k": "v"}}); !errors.Is(err, ErrBreakerOpen) {
		t.Errorf("PatchNamespace: %v", err)
	}
	if err := c.DeleteNamespace(context.Background(), "workspace:abc"); !errors.Is(err, ErrBreakerOpen) {
		t.Errorf("DeleteNamespace: %v", err)
	}
	if _, err := c.CommitMemory(context.Background(), "workspace:abc", contract.MemoryWrite{Content: "x", Kind: contract.MemoryKindFact, Source: contract.MemorySourceAgent}); !errors.Is(err, ErrBreakerOpen) {
		t.Errorf("CommitMemory: %v", err)
	}
	if _, err := c.Search(context.Background(), contract.SearchRequest{Namespaces: []string{"workspace:abc"}}); !errors.Is(err, ErrBreakerOpen) {
		t.Errorf("Search: %v", err)
	}
	if err := c.ForgetMemory(context.Background(), "id-1", contract.ForgetRequest{RequestedByNamespace: "workspace:abc"}); !errors.Is(err, ErrBreakerOpen) {
		t.Errorf("ForgetMemory: %v", err)
	}
}

// --- Real round-trip via httptest (ensures the HTTP layer wiring is right) ---

func TestRealHTTP_RoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/health":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(contract.HealthResponse{Status: "ok", Version: "1.0.0", Capabilities: []string{"fts"}})
		case strings.HasPrefix(r.URL.Path, "/v1/namespaces/") && r.Method == http.MethodPut:
			w.WriteHeader(200)
			_ = json.NewEncoder(w).Encode(contract.Namespace{Name: "workspace:abc", Kind: contract.NamespaceKindWorkspace, CreatedAt: time.Now().UTC()})
		default:
			http.Error(w, "no", 500)
		}
	}))
	t.Cleanup(srv.Close)

	c := New(Config{BaseURL: srv.URL})
	if _, err := c.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	if !c.SupportsCapability(contract.CapabilityFTS) {
		t.Error("FTS capability missing")
	}
	if _, err := c.UpsertNamespace(context.Background(), "workspace:abc", contract.NamespaceUpsert{Kind: contract.NamespaceKindWorkspace}); err != nil {
		t.Errorf("UpsertNamespace: %v", err)
	}
}

// --- Bad JSON response handling ---

func TestDecode_GarbageResponseBody(t *testing.T) {
	rt := roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader("not-json")),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
		}, nil
	})
	c := New(Config{BaseURL: "http://x", HTTP: rt})
	_, err := c.Boot(context.Background())
	if err == nil || !strings.Contains(err.Error(), "decode") {
		t.Errorf("err = %v, want decode error", err)
	}
}

// --- Coverage corner cases ---

// Pins the env-var success branch in New (line ~107). The parameterised
// TestNew_TimeoutFromEnv only exercises parseDurationEnv directly; we
// also need to confirm New itself wires it through.
func TestNew_TimeoutFromEnvActuallyApplied(t *testing.T) {
	t.Setenv(envTimeout, "7s")
	t.Setenv(envBaseURL, "http://x")
	c := New(Config{})
	// Inspecting the inner *http.Client.Timeout requires a type
	// assertion against the unexported field — instead, verify via
	// behavior: an http.Client with 7s timeout is constructed (not the
	// 2s default). We probe by checking the http field is the default
	// *http.Client (not nil), then assert its Timeout.
	hc, ok := c.http.(*http.Client)
	if !ok {
		t.Fatalf("c.http is %T, expected *http.Client", c.http)
	}
	if hc.Timeout != 7*time.Second {
		t.Errorf("Timeout = %v, want 7s", hc.Timeout)
	}
}

// Pins the json.Marshal error branch in doJSON (line ~279). Triggered
// by passing a value with a non-marshalable field — channels can't be
// JSON-encoded. Propagation is map[string]interface{} so it accepts
// arbitrary values that pass Validate() but fail Marshal.
func TestDoJSON_MarshalError(t *testing.T) {
	c := New(Config{BaseURL: "http://x", HTTP: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		t.Error("HTTP must not be reached when marshal fails")
		return nil, errors.New("nope")
	})})
	_, err := c.CommitMemory(context.Background(), "workspace:abc", contract.MemoryWrite{
		Content:     "x",
		Kind:        contract.MemoryKindFact,
		Source:      contract.MemorySourceAgent,
		Propagation: map[string]interface{}{"bad": make(chan int)},
	})
	if err == nil || !strings.Contains(err.Error(), "marshal") {
		t.Errorf("err = %v, want wrapped marshal error", err)
	}
}

// Pins the http.NewRequestWithContext error branch in doJSON (line
// ~286). Triggered by an unparseable base URL — unbalanced bracket in
// the host part fails url.Parse.
func TestDoJSON_NewRequestError(t *testing.T) {
	c := New(Config{BaseURL: "http://[::1", HTTP: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		t.Error("HTTP must not be reached when request construction fails")
		return nil, errors.New("nope")
	})})
	_, err := c.UpsertNamespace(context.Background(), "workspace:abc", contract.NamespaceUpsert{Kind: contract.NamespaceKindWorkspace})
	if err == nil || !strings.Contains(err.Error(), "new request") {
		t.Errorf("err = %v, want wrapped new-request error", err)
	}
}

// Pins the "204 with respBody passed" path in doJSON (line ~320).
// Defensive: plugin returns NoContent on an endpoint that normally
// has a body (Search). doJSON must not try to decode an empty body
// into the typed response.
func TestDoJSON_204OnEndpointExpectingBody(t *testing.T) {
	rt := roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return emptyResp(204), nil
	})
	c := New(Config{BaseURL: "http://x", HTTP: rt})
	got, err := c.Search(context.Background(), contract.SearchRequest{Namespaces: []string{"workspace:abc"}})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got == nil {
		t.Error("got nil SearchResponse, want zero value")
	}
	if len(got.Memories) != 0 {
		t.Errorf("memories = %v, want empty", got.Memories)
	}
}

// Pins the empty-body error envelope path. decodeError
// wraps an empty error body in a stub *contract.Error rather than
// returning an unmarshal error.
func TestDecodeError_EmptyBodyWithUnknownStatus(t *testing.T) {
	rt := roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 418, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	c := New(Config{BaseURL: "http://x", HTTP: rt})
	_, err := c.UpsertNamespace(context.Background(), "workspace:abc", contract.NamespaceUpsert{Kind: contract.NamespaceKindWorkspace})
	if err == nil {
		t.Fatal("expected error")
	}
	var ce *contract.Error
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(ce.Message, "empty body") {
		t.Errorf("message = %q, want 'empty body' marker", ce.Message)
	}
}

// --- ContextCancel ---

func TestContextCancel_PropagatesToTransport(t *testing.T) {
	rt := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	c := New(Config{BaseURL: "http://x", HTTP: rt})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Boot(ctx)
	if err == nil {
		t.Error("expected error from cancelled context")
	}
}
