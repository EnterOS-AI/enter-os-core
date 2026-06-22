package provisioner

// Regression tests for PR #1738 (merged 2026-04-23) — CPProvisioner.Stop +
// IsRunning must look up the real EC2 instance_id (i-*) from the DB
// before calling the control plane, NOT pass the workspace UUID verbatim.
//
// Original bug:
//   url := fmt.Sprintf("%s/cp/workspaces/%s?instance_id=%s",
//                       baseURL, workspaceID, workspaceID)
//                                             ^^^^^^^^^^^^^^
//                                             sends UUID as instance_id
//
// AWS then rejects with InvalidInstanceID.Malformed, the next provision
// hits InvalidGroup.Duplicate on the leftover SG, and Save & Restart
// cascades into a full failure. Production incident 2026-04-22 on
// hongmingwang workspace a8af9d79 + recurrent on every SaaS workspace
// secret update that triggers a restart.
//
// These tests pin two invariants of the fix:
//   1. Stop + IsRunning query resolveInstanceID(ctx, workspaceID) BEFORE
//      hitting CP, and use the returned i-* ID (not the workspace UUID)
//      in the instance_id query param.
//   2. Empty instance_id → no CP call (idempotent no-op).

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestStop_UsesRealInstanceIDNotWorkspaceUUID is the load-bearing
// regression guard for #1738. If someone reverts the resolveInstanceID
// lookup and ships the `workspaceID, workspaceID` version back, this
// test fails immediately.
func TestStop_UsesRealInstanceIDNotWorkspaceUUID(t *testing.T) {
	primeInstanceIDLookup(t, map[string]string{
		"ws-cd5c9906-bfd7-4e2a-8c0b-9f1e2d3a4b5c": "i-0a1b2c3d4e5f67890",
	})

	var sawInstance string
	var sawPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawInstance = r.URL.Query().Get("instance_id")
		sawPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := &CPProvisioner{
		baseURL:      srv.URL,
		orgID:        "org-1",
		sharedSecret: "s3cret",
		adminToken:   "tok-xyz",
		httpClient:   srv.Client(),
	}
	if err := p.Stop(context.Background(), "ws-cd5c9906-bfd7-4e2a-8c0b-9f1e2d3a4b5c"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Load-bearing assertion: the AWS-facing instance_id must be the
	// i-* ID from the DB, NEVER the workspace UUID.
	if sawInstance != "i-0a1b2c3d4e5f67890" {
		t.Errorf("#1738 REGRESSION: instance_id query = %q, want i-0a1b2c3d4e5f67890. "+
			"CP would forward this to AWS TerminateInstances — a UUID triggers "+
			"InvalidInstanceID.Malformed and orphans the EC2. See PR #1738.", sawInstance)
	}

	// Sanity: path still carries the workspace UUID (that's how CP looks
	// up the row). Only the instance_id query param changed.
	if sawPath != "/cp/workspaces/ws-cd5c9906-bfd7-4e2a-8c0b-9f1e2d3a4b5c" {
		t.Errorf("path = %q, want /cp/workspaces/ws-cd5c9906-bfd7-4e2a-8c0b-9f1e2d3a4b5c", sawPath)
	}
}

// TestStop_NoInstanceIDSkipsCPCall — when the workspace has no EC2 on
// file (never provisioned, already deprovisioned, or external runtime),
// Stop must be a no-op. Calling CP with empty instance_id triggers the
// exact AWS error the fix was meant to prevent.
func TestStop_NoInstanceIDSkipsCPCall(t *testing.T) {
	primeInstanceIDLookup(t, map[string]string{}) // empty map → "" for everything

	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := &CPProvisioner{baseURL: srv.URL, orgID: "org-1", httpClient: srv.Client()}
	if err := p.Stop(context.Background(), "ws-never-provisioned"); err != nil {
		t.Errorf("Stop with no instance_id should be no-op, got err: %v", err)
	}
	if called {
		t.Error("#1738 REGRESSION: Stop hit CP with empty instance_id — would trigger " +
			"InvalidInstanceID.Malformed downstream. Fix must short-circuit on empty lookup.")
	}
}

// TestStop_SendsProviderQueryParam — #2386 regression guard. When the
// workspace row carries a non-empty provider (e.g. "hetzner", "gcp"), the
// deprovision DELETE must include ?provider= so CP routes to the correct
// backend. Without it, non-AWS workspaces fall through to the AWS terminate
// path and leak.
func TestStop_SendsProviderQueryParam(t *testing.T) {
	primeInstanceIDLookup(t, map[string]string{
		"ws-cd5c9906-bfd7-4e2a-8c0b-9f1e2d3a4b5c": "i-0a1b2c3d4e5f67890",
	})
	primeProviderLookup(t, map[string]string{
		"ws-cd5c9906-bfd7-4e2a-8c0b-9f1e2d3a4b5c": "hetzner",
	})

	var sawProvider string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawProvider = r.URL.Query().Get("provider")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := &CPProvisioner{
		baseURL:      srv.URL,
		orgID:        "org-1",
		sharedSecret: "s3cret",
		adminToken:   "tok-xyz",
		httpClient:   srv.Client(),
	}
	if err := p.Stop(context.Background(), "ws-cd5c9906-bfd7-4e2a-8c0b-9f1e2d3a4b5c"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if sawProvider != "hetzner" {
		t.Errorf("#2386 REGRESSION: provider query = %q, want hetzner. "+
			"CP would route to AWS backend and leak the non-AWS box.", sawProvider)
	}
}

// TestStop_EmptyProviderOmitsQueryParam — when provider is absent (default
// AWS path), the URL must not include ?provider= so the CP uses its default
// AWS terminate route.
func TestStop_EmptyProviderOmitsQueryParam(t *testing.T) {
	primeInstanceIDLookup(t, map[string]string{
		"ws-cd5c9906-bfd7-4e2a-8c0b-9f1e2d3a4b5c": "i-0a1b2c3d4e5f67890",
	})
	primeProviderLookup(t, map[string]string{}) // empty → "" for everything

	var sawProvider string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawProvider = r.URL.Query().Get("provider")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := &CPProvisioner{
		baseURL:    srv.URL,
		orgID:      "org-1",
		httpClient: srv.Client(),
	}
	if err := p.Stop(context.Background(), "ws-cd5c9906-bfd7-4e2a-8c0b-9f1e2d3a4b5c"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if sawProvider != "" {
		t.Errorf("provider query = %q, want omitted. Empty provider must default to AWS.", sawProvider)
	}
}

// TestStop_ProviderLookupErrorFailsClosed — #2386 CR2. If the DB/provider
// lookup fails after instance_id resolves, a non-AWS workspace must NOT
// silently omit provider= and fall back to the AWS terminate path. The fix
// must return the error (fail closed) so the caller retries instead of
// leaking the box.
func TestStop_ProviderLookupErrorFailsClosed(t *testing.T) {
	primeInstanceIDLookup(t, map[string]string{
		"ws-cd5c9906-bfd7-4e2a-8c0b-9f1e2d3a4b5c": "i-0a1b2c3d4e5f67890",
	})
	prev := resolveProvider
	resolveProvider = func(_ context.Context, _ string) (string, error) {
		return "", fmt.Errorf("db connection reset")
	}
	defer func() { resolveProvider = prev }()

	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := &CPProvisioner{baseURL: srv.URL, orgID: "org-1", httpClient: srv.Client()}
	err := p.Stop(context.Background(), "ws-cd5c9906-bfd7-4e2a-8c0b-9f1e2d3a4b5c")
	if err == nil {
		t.Fatal("want error when provider lookup fails, got nil — would leak to AWS path")
	}
	if called {
		t.Error("CR2 REGRESSION: Stop hit CP after provider lookup error — should fail closed before any CP call")
	}
}

// TestStop_ProviderQueryParamIsEncoded — #2386 CR2. Provider slugs that
// contain query-special characters must be URL-encoded so they don't
// corrupt the DELETE URL or inject unintended query parameters.
func TestStop_ProviderQueryParamIsEncoded(t *testing.T) {
	primeInstanceIDLookup(t, map[string]string{
		"ws-cd5c9906-bfd7-4e2a-8c0b-9f1e2d3a4b5c": "i-0a1b2c3d4e5f67890",
	})
	primeProviderLookup(t, map[string]string{
		// Intentionally hostile slug: contains '=', '&', and '%'.
		"ws-cd5c9906-bfd7-4e2a-8c0b-9f1e2d3a4b5c": "prov=a&b=2%c",
	})

	var rawQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := &CPProvisioner{baseURL: srv.URL, orgID: "org-1", httpClient: srv.Client()}
	if err := p.Stop(context.Background(), "ws-cd5c9906-bfd7-4e2a-8c0b-9f1e2d3a4b5c"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// The raw query must NOT contain the literal hostile string — it must
	// be percent-encoded. If it appears literally, url.Values was not used.
	if strings.Contains(rawQuery, "prov=a&b=2%c") {
		t.Errorf("CR2 REGRESSION: provider query param is raw/unchecked — contains literal hostile string in %q", rawQuery)
	}
	// Sanity: after decoding the provider value must round-trip correctly.
	parsed, _ := url.ParseQuery(rawQuery)
	if parsed.Get("provider") != "prov=a&b=2%c" {
		t.Errorf("provider round-trip failed: got %q, want prov=a&b=2%%c", parsed.Get("provider"))
	}
}

// TestIsRunning_UsesRealInstanceIDNotWorkspaceUUID mirrors the Stop test
// for IsRunning's GET /cp/workspaces/:id/status?instance_id=... path.
// Same class of bug, same acceptance criterion.
func TestIsRunning_UsesRealInstanceIDNotWorkspaceUUID(t *testing.T) {
	primeInstanceIDLookup(t, map[string]string{
		"ws-abc": "i-deadbeef",
	})

	var sawInstance string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawInstance = r.URL.Query().Get("instance_id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"state":"running"}`))
	}))
	defer srv.Close()

	p := &CPProvisioner{baseURL: srv.URL, orgID: "org-1", httpClient: srv.Client()}
	running, err := p.IsRunning(context.Background(), "ws-abc")
	if err != nil {
		t.Fatalf("IsRunning: %v", err)
	}
	if !running {
		t.Errorf("expected running=true")
	}
	if sawInstance != "i-deadbeef" {
		t.Errorf("#1738 REGRESSION: IsRunning sent instance_id=%q, want i-deadbeef", sawInstance)
	}
}
