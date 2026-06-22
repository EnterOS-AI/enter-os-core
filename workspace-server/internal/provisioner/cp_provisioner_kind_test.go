package provisioner

// cp_provisioner_kind_test.go — pins the kind passthrough on the CP provision
// wire (core#2495 SSOT): a kind='platform' workspace (the org concierge) is
// provisioned through this SAME path as every ordinary workspace, differing
// only in the image the CP selects — which requires the CP to KNOW the kind.
// Before this field, the CP picked the plain runtime image, the platform MCP
// binary was absent, and the concierge hard-failed its MCP readiness gate.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// startCaptureCP spins a fake CP that captures the provision request body and
// returns a minimal 201. Returns the provisioner wired at it + the body ptr.
func startCaptureCP(t *testing.T) (*CPProvisioner, *[]byte) {
	t.Helper()
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = b
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"instance_id":"i-test","private_ip":"10.0.0.9","state":"pending"}`))
	}))
	t.Cleanup(srv.Close)
	return &CPProvisioner{
		baseURL:      srv.URL,
		orgID:        "org-1",
		sharedSecret: "s3cret",
		adminToken:   "tok-xyz",
		httpClient:   srv.Client(),
	}, &body
}

// The concierge: kind='platform' must reach the CP verbatim.
func TestStart_ForwardsPlatformKind(t *testing.T) {
	p, body := startCaptureCP(t)
	_, err := p.Start(context.Background(), WorkspaceConfig{
		WorkspaceID: "ws-concierge",
		Runtime:     "claude-code",
		Kind:        WorkspaceKindPlatform,
		PlatformURL: "https://acme.example.com",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	var req map[string]any
	if err := json.Unmarshal(*body, &req); err != nil {
		t.Fatalf("unmarshal captured body: %v", err)
	}
	if got := req["kind"]; got != "platform" {
		t.Errorf("kind on the CP wire = %v, want \"platform\" — without it the CP picks the plain runtime image and the concierge loses its platform MCP (core#2495)", got)
	}
}

// Ordinary workspaces: the wire shape must be UNCHANGED (omitempty) so older
// CPs see byte-identical requests.
func TestStart_OmitsKindForOrdinaryWorkspace(t *testing.T) {
	p, body := startCaptureCP(t)
	// Ordinary workspaces have kind="workspace" from the DB COALESCE;
	// the CP provisioner must suppress it so omitempty keeps the wire
	// shape unchanged (core#2498 truth-up).
	_, err := p.Start(context.Background(), WorkspaceConfig{
		WorkspaceID: "ws-ordinary",
		Runtime:     "claude-code",
		Kind:        "workspace",
		PlatformURL: "https://acme.example.com",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if strings.Contains(string(*body), `"kind"`) {
		t.Errorf("ordinary workspace provision body must omit the kind field (omitempty contract), got: %s", string(*body))
	}
}
