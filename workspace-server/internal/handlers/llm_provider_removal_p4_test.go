package handlers

// internal#718 P4 closure — LLM_PROVIDER removal + PUT /provider retirement.
//
// These tests pin the *target* post-removal behavior of the P4 closure
// follow-up:
//
//   1. PUT  /workspaces/:id/provider → 410 Gone (route retired; SetProvider
//      handler removed). Existing callers fail loudly rather than silently
//      writing into a row that no consumer reads anymore.
//   2. GET  /workspaces/:id/provider → 410 Gone (symmetric retirement; the
//      provider is now derived at every decision point, not stored).
//   3. WorkspaceHandler.Create no longer writes LLM_PROVIDER to
//      workspace_secrets. The model selection (`payload.Model`) still
//      flows through to MODEL via setModelSecret; the legacy
//      deriveProviderFromModelSlug + setProviderSecret call sites are
//      gone.
//   4. Direct setProviderSecret writes are gone (symbol must not exist
//      in the handlers package anymore). Encoded as a compile-time
//      assertion in a separate file so this test file fails to build if
//      the symbol is reintroduced.
//
// These are red-before-the-source-edit tests. Each failure here points
// at exactly the code path the closure removes.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// TestPutProvider_410Gone asserts that PUT /workspaces/:id/provider
// is registered to a Gone handler after P4 closure. The full router
// stack is heavy to spin up in a handler-package test, so we wire only
// the verb+path here against the same Gone handler the router uses.
func TestPutProvider_410Gone(t *testing.T) {
	router := gin.New()
	router.PUT("/workspaces/:id/provider", ProviderEndpointGone)
	router.GET("/workspaces/:id/provider", ProviderEndpointGone)

	body, _ := json.Marshal(map[string]string{"provider": "anthropic-api"})
	req := httptest.NewRequest("PUT", "/workspaces/00000000-0000-0000-0000-000000000003/provider", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusGone {
		t.Fatalf("PUT /provider: want 410 Gone, got %d (body=%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "LLM_PROVIDER") || !strings.Contains(w.Body.String(), "internal#718") {
		t.Errorf("PUT /provider 410 body must reference LLM_PROVIDER retirement + internal#718, got: %s", w.Body.String())
	}
}

func TestGetProvider_410Gone(t *testing.T) {
	router := gin.New()
	router.GET("/workspaces/:id/provider", ProviderEndpointGone)

	req := httptest.NewRequest("GET", "/workspaces/00000000-0000-0000-0000-000000000003/provider", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusGone {
		t.Fatalf("GET /provider: want 410 Gone, got %d", w.Code)
	}
}

// TestProviderEndpointGone_BodyShape asserts the Gone handler returns a
// stable JSON shape so callers can recognize the retirement (instead of
// treating it as a generic 410 + retry).
func TestProviderEndpointGone_BodyShape(t *testing.T) {
	router := gin.New()
	router.PUT("/workspaces/:id/provider", ProviderEndpointGone)

	body, _ := json.Marshal(map[string]string{"provider": "anthropic-api"})
	req := httptest.NewRequest("PUT", "/workspaces/00000000-0000-0000-0000-000000000003/provider", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	raw, _ := io.ReadAll(w.Body)
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Gone body not JSON: %v\n%s", err, raw)
	}
	for _, key := range []string{"code", "error", "issue"} {
		if _, ok := got[key]; !ok {
			t.Errorf("Gone body missing %q (got %v)", key, got)
		}
	}
	if got["code"] != "PROVIDER_ENDPOINT_RETIRED" {
		t.Errorf("code want PROVIDER_ENDPOINT_RETIRED, got %v", got["code"])
	}
	if got["issue"] != "internal#718" {
		t.Errorf("issue want internal#718, got %v", got["issue"])
	}
}
