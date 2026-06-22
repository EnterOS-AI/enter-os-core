package handlers

// Tests for GET /workspaces/:id/rescue (RFC internal#742 Part 3).
//
// These exercise the handler against a FAKE store (no DB) so every path
// is deterministic without external infra:
//   - returns the latest bundle in the documented shape
//   - 404 when no bundle exists for the workspace
//   - org-scoping: the handler passes the tenant's MOLECULE_ORG_ID to
//     the store, so a fake that returns nil for a mismatched org proves a
//     sibling org cannot read another org's bundle
//   - 503 on a store/datastore error (not a 404 masquerade)
//   - redaction/shape preserved: stored sections are returned verbatim,
//     no re-derivation
//
// WorkspaceAuth gating itself is covered by the middleware tests; here we
// invoke the handler directly (the route is registered on the wsAuth
// group in router.go).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/rescue"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/rescuestore"
	"github.com/gin-gonic/gin"
)

func init() { gin.SetMode(gin.TestMode) }

// fakeRescueStore records the args it was called with and returns a
// scripted result. Implements rescuestore.Store.
type fakeRescueStore struct {
	// gotWorkspaceID/gotOrgID capture what the handler passed.
	gotWorkspaceID string
	gotOrgID       string
	// ret/err are the scripted GetLatest result.
	ret *rescuestore.StoredBundle
	err error
}

func (f *fakeRescueStore) Persist(_ context.Context, _ rescue.Bundle) error { return nil }

func (f *fakeRescueStore) GetLatest(_ context.Context, workspaceID, orgID string) (*rescuestore.StoredBundle, error) {
	f.gotWorkspaceID = workspaceID
	f.gotOrgID = orgID
	return f.ret, f.err
}

// doRescueGet runs the handler for ws against the given fake and returns
// the recorder. orgEnv sets MOLECULE_ORG_ID for the duration.
func doRescueGet(t *testing.T, ws, orgEnv string, fake *fakeRescueStore) *httptest.ResponseRecorder {
	t.Helper()
	t.Setenv("MOLECULE_ORG_ID", orgEnv)

	h := (&RescueReadHandler{}).WithStore(fake)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: ws}}
	c.Request = httptest.NewRequest("GET", "/workspaces/"+ws+"/rescue", nil)
	h.GetRescue(c)
	return w
}

// sampleStored builds a representative stored bundle with a redacted +
// a failure-marker section.
func sampleStored() *rescuestore.StoredBundle {
	return &rescuestore.StoredBundle{
		CapturedAt: time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC),
		Bundle: rescue.Bundle{
			WorkspaceID: "ws-1",
			OrgID:       "org-9",
			InstanceID:  "i-abc123",
			Reason:      "provision_timeout_sweep",
			Sections: []rescue.Section{
				{Name: "config.yaml", Content: "model: gpt-4\nANTHROPIC_API_KEY=[REDACTED]", Redacted: true},
				{Name: "docker-ps", Content: "(rescue: section collection failed: ssh blip)", Redacted: false},
			},
		},
	}
}

// TestGetRescue_ReturnsLatestBundle — happy path: 200 with the full
// documented shape, sections in order, redaction-preserved.
func TestGetRescue_ReturnsLatestBundle(t *testing.T) {
	fake := &fakeRescueStore{ret: sampleStored()}
	w := doRescueGet(t, "ws-1", "org-9", fake)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		WorkspaceID string    `json:"workspace_id"`
		CapturedAt  time.Time `json:"captured_at"`
		Reason      string    `json:"reason"`
		InstanceID  string    `json:"instance_id"`
		Sections    []struct {
			Name     string `json:"name"`
			Content  string `json:"content"`
			Redacted bool   `json:"redacted"`
		} `json:"sections"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, w.Body.String())
	}
	if resp.WorkspaceID != "ws-1" {
		t.Errorf("workspace_id = %q, want ws-1", resp.WorkspaceID)
	}
	if resp.Reason != "provision_timeout_sweep" {
		t.Errorf("reason = %q", resp.Reason)
	}
	if resp.InstanceID != "i-abc123" {
		t.Errorf("instance_id = %q", resp.InstanceID)
	}
	if !resp.CapturedAt.Equal(time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("captured_at = %v", resp.CapturedAt)
	}
	if len(resp.Sections) != 2 {
		t.Fatalf("sections = %d, want 2", len(resp.Sections))
	}
	// Order preserved: config first, docker-ps second.
	if resp.Sections[0].Name != "config.yaml" || resp.Sections[1].Name != "docker-ps" {
		t.Errorf("section order wrong: %q, %q", resp.Sections[0].Name, resp.Sections[1].Name)
	}
	// Redaction-preserved: the redacted flag rides through untouched, and
	// the failure marker stays a non-redacted marker.
	if !resp.Sections[0].Redacted {
		t.Error("config.yaml section should be redacted=true")
	}
	if resp.Sections[1].Redacted {
		t.Error("failure-marker section should be redacted=false")
	}
	// Handler does NOT re-derive secrets; stored [REDACTED] verbatim.
	if want := "ANTHROPIC_API_KEY=[REDACTED]"; !strings.Contains(resp.Sections[0].Content, want) {
		t.Errorf("section content = %q, want it to contain %q", resp.Sections[0].Content, want)
	}
}

// TestGetRescue_404WhenNone — no bundle on file → 404, not 500/200.
func TestGetRescue_404WhenNone(t *testing.T) {
	fake := &fakeRescueStore{ret: nil} // store returns (nil, nil)
	w := doRescueGet(t, "ws-none", "org-9", fake)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

// TestGetRescue_OrgScopingPassedToStore — the handler must hand the
// tenant's MOLECULE_ORG_ID to the store, and a store that returns nil for
// a mismatched org yields 404. This is the sibling-org isolation: a
// caller in org B (a different tenant process, MOLECULE_ORG_ID=org-B)
// reading ws-1 (which belongs to org-9) gets the org filter applied → no
// row → 404.
func TestGetRescue_OrgScopingPassedToStore(t *testing.T) {
	// Tenant configured as a DIFFERENT org than the bundle's owner.
	// Fake mimics the Postgres org filter: returns nil because org-B
	// doesn't match the row's org-9.
	fake := &fakeRescueStore{ret: nil}
	w := doRescueGet(t, "ws-1", "org-B", fake)

	if fake.gotOrgID != "org-B" {
		t.Errorf("store got org_id = %q, want the tenant's org-B", fake.gotOrgID)
	}
	if fake.gotWorkspaceID != "ws-1" {
		t.Errorf("store got workspace_id = %q, want ws-1", fake.gotWorkspaceID)
	}
	if w.Code != http.StatusNotFound {
		t.Fatalf("sibling-org read: status = %d, want 404", w.Code)
	}
}

// TestGetRescue_EmptyOrgEnvRejected — empty MOLECULE_ORG_ID is a
// fail-closed security violation (#2020). The handler must 503 before
// calling the store, so the org filter cannot be bypassed.
func TestGetRescue_EmptyOrgEnvRejected(t *testing.T) {
	fake := &fakeRescueStore{ret: sampleStored()}
	w := doRescueGet(t, "ws-1", "", fake)
	if fake.gotOrgID != "" {
		t.Errorf("store was called with org_id = %q; want no call when env empty", fake.gotOrgID)
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "platform_misconfigured") {
		t.Fatalf("body = %s, want platform_misconfigured code", w.Body.String())
	}
}

// TestGetRescue_StoreErrorIs503 — an actual datastore error must surface
// as 503, never a 404 (which would hide an outage as "no bundle").
func TestGetRescue_StoreErrorIs503(t *testing.T) {
	fake := &fakeRescueStore{err: errors.New("connection refused")}
	w := doRescueGet(t, "ws-1", "org-9", fake)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", w.Code, w.Body.String())
	}
}

// TestGetRescue_NilStoreIs503 — defensive: a handler with no store wired
// (db.DB nil in a degraded boot) returns 503, never panics.
func TestGetRescue_NilStoreIs503(t *testing.T) {
	t.Setenv("MOLECULE_ORG_ID", "org-9")
	h := &RescueReadHandler{} // store == nil
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-1/rescue", nil)
	h.GetRescue(c)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

// TestBuildRescueResponse_BoundsSections — a stored bundle with more than
// maxResponseSections sections is capped + flagged truncated.
func TestBuildRescueResponse_BoundsSections(t *testing.T) {
	many := make([]rescue.Section, maxResponseSections+5)
	for i := range many {
		many[i] = rescue.Section{Name: "s", Content: "c", Redacted: true}
	}
	stored := &rescuestore.StoredBundle{
		CapturedAt: time.Now(),
		Bundle:     rescue.Bundle{WorkspaceID: "ws-1", Sections: many},
	}
	resp := buildRescueResponse("ws-1", stored)
	if len(resp.Sections) != maxResponseSections {
		t.Errorf("sections = %d, want capped at %d", len(resp.Sections), maxResponseSections)
	}
	if !resp.Truncated {
		t.Error("truncated flag should be set when sections were capped")
	}
}
