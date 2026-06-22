package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// ProviderEndpointGone is wired for both GET and PUT
// /workspaces/:id/provider. The handler has no side effects and no
// dependencies, so a single test pins the retirement contract.
func TestProviderEndpointGone(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-1/provider", nil)

	ProviderEndpointGone(c)

	if w.Code != http.StatusGone {
		t.Fatalf("expected 410, got %d: %s", w.Code, w.Body.String())
	}

	var body struct {
		Code  string `json:"code"`
		Error string `json:"error"`
		Issue string `json:"issue"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body parse: %v", err)
	}
	if body.Code != "PROVIDER_ENDPOINT_RETIRED" {
		t.Errorf("code: expected PROVIDER_ENDPOINT_RETIRED, got %q", body.Code)
	}
	if body.Issue != "internal#718" {
		t.Errorf("issue: expected internal#718, got %q", body.Issue)
	}
	if body.Error == "" {
		t.Errorf("error message should be present")
	}
}
