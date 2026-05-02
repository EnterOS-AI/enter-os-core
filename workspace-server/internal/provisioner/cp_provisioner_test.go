package provisioner

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// primeInstanceIDLookup swaps resolveInstanceID for a stub that returns
// the mapped instance_id for the given workspace_id, or "" for anything
// not in the map. Cheaper than standing up a sqlmock since Stop/IsRunning
// tests mostly don't care about the SQL path — they're testing the CP
// HTTP interaction downstream of the lookup.
func primeInstanceIDLookup(t *testing.T, pairs map[string]string) {
	t.Helper()
	prev := resolveInstanceID
	resolveInstanceID = func(_ context.Context, wsID string) (string, error) {
		if id, ok := pairs[wsID]; ok {
			return id, nil
		}
		return "", nil
	}
	t.Cleanup(func() { resolveInstanceID = prev })
}

// TestNewCPProvisioner_RequiresOrgID — self-hosted deployments don't
// have a MOLECULE_ORG_ID, and the provisioner must refuse to construct
// rather than silently phone home to the prod CP with an empty tenant.
func TestNewCPProvisioner_RequiresOrgID(t *testing.T) {
	t.Setenv("MOLECULE_ORG_ID", "")
	if _, err := NewCPProvisioner(); err == nil {
		t.Error("want error when MOLECULE_ORG_ID is unset, got nil")
	}
}

// TestNewCPProvisioner_FallsBackToProvisionSharedSecret — operators
// may set PROVISION_SHARED_SECRET on both sides of the wire with a
// single value; the tenant accepts that name as a fallback for
// MOLECULE_CP_SHARED_SECRET. The fallback is documented in
// NewCPProvisioner; this test is the regression gate.
func TestNewCPProvisioner_FallsBackToProvisionSharedSecret(t *testing.T) {
	t.Setenv("MOLECULE_ORG_ID", "org-abc")
	t.Setenv("MOLECULE_CP_SHARED_SECRET", "")
	t.Setenv("PROVISION_SHARED_SECRET", "from-fallback")

	p, err := NewCPProvisioner()
	if err != nil {
		t.Fatalf("NewCPProvisioner: %v", err)
	}
	if p.sharedSecret != "from-fallback" {
		t.Errorf("sharedSecret = %q, want %q", p.sharedSecret, "from-fallback")
	}
}

// TestProvisionAuthHeaders_NoopWhenBothEmpty — the self-hosted path
// that doesn't gate /cp/workspaces/* must not add stray auth headers
// (bearer-like content would surprise non-bearer intermediaries).
func TestProvisionAuthHeaders_NoopWhenBothEmpty(t *testing.T) {
	p := &CPProvisioner{sharedSecret: "", adminToken: ""}
	req := httptest.NewRequest("GET", "http://x/", nil)
	p.provisionAuthHeaders(req)
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization set to %q with empty secret; want unset", got)
	}
	if got := req.Header.Get("X-Molecule-Admin-Token"); got != "" {
		t.Errorf("X-Molecule-Admin-Token set to %q with empty token; want unset", got)
	}
}

// TestProvisionAuthHeaders_SetsBothWhenBothProvided — happy path for
// SaaS tenants. Both the platform-wide shared secret and the per-tenant
// admin_token land on every outbound call.
func TestProvisionAuthHeaders_SetsBothWhenBothProvided(t *testing.T) {
	p := &CPProvisioner{sharedSecret: "the-secret", adminToken: "tok-abc"}
	req := httptest.NewRequest("GET", "http://x/", nil)
	p.provisionAuthHeaders(req)
	if got := req.Header.Get("Authorization"); got != "Bearer the-secret" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer the-secret")
	}
	if got := req.Header.Get("X-Molecule-Admin-Token"); got != "tok-abc" {
		t.Errorf("X-Molecule-Admin-Token = %q, want tok-abc", got)
	}
}

// TestProvisionAuthHeaders_OnlyAdminTokenWhenSecretEmpty — in the
// transition window where the tenant has admin_token but
// PROVISION_SHARED_SECRET isn't set, still send the admin token. CP
// middleware decides whether the shared secret is required.
func TestProvisionAuthHeaders_OnlyAdminTokenWhenSecretEmpty(t *testing.T) {
	p := &CPProvisioner{sharedSecret: "", adminToken: "tok-abc"}
	req := httptest.NewRequest("GET", "http://x/", nil)
	p.provisionAuthHeaders(req)
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q, want unset", got)
	}
	if got := req.Header.Get("X-Molecule-Admin-Token"); got != "tok-abc" {
		t.Errorf("X-Molecule-Admin-Token = %q, want tok-abc", got)
	}
}

// TestAdminAuthHeaders_UsesCPAdminAPIKeyNotSharedSecret — /cp/admin/*
// routes are gated by CP_ADMIN_API_TOKEN on the CP side (distinct from
// PROVISION_SHARED_SECRET). The tenant must send the admin key as the
// bearer on these routes or CP returns 401.
func TestAdminAuthHeaders_UsesCPAdminAPIKeyNotSharedSecret(t *testing.T) {
	p := &CPProvisioner{
		sharedSecret:  "provision-secret",
		adminToken:    "tok-abc",
		cpAdminAPIKey: "admin-api-key",
	}
	req := httptest.NewRequest("GET", "http://x/", nil)
	p.adminAuthHeaders(req)
	if got := req.Header.Get("Authorization"); got != "Bearer admin-api-key" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer admin-api-key")
	}
	if got := req.Header.Get("X-Molecule-Admin-Token"); got != "tok-abc" {
		t.Errorf("X-Molecule-Admin-Token = %q, want tok-abc", got)
	}
}

// TestAdminAuthHeaders_FallsBackToSharedSecretWhenAdminKeyUnset —
// self-hosted and dev deployments set PROVISION_SHARED_SECRET but not
// CP_ADMIN_API_TOKEN. Fall back so single-secret setups keep working
// (CP in those deployments either accepts both bearers or doesn't gate
// /cp/admin/*).
func TestAdminAuthHeaders_FallsBackToSharedSecretWhenAdminKeyUnset(t *testing.T) {
	p := &CPProvisioner{
		sharedSecret:  "provision-secret",
		adminToken:    "tok-abc",
		cpAdminAPIKey: "provision-secret", // NewCPProvisioner sets this when env is unset
	}
	req := httptest.NewRequest("GET", "http://x/", nil)
	p.adminAuthHeaders(req)
	if got := req.Header.Get("Authorization"); got != "Bearer provision-secret" {
		t.Errorf("Authorization = %q, want fallback %q", got, "Bearer provision-secret")
	}
}

// TestNewCPProvisioner_ReadsCPAdminAPIToken — env-to-field wiring.
// When CP_ADMIN_API_TOKEN is set, cpAdminAPIKey picks it up.
func TestNewCPProvisioner_ReadsCPAdminAPIToken(t *testing.T) {
	t.Setenv("MOLECULE_ORG_ID", "org-abc")
	t.Setenv("MOLECULE_CP_SHARED_SECRET", "shared")
	t.Setenv("CP_ADMIN_API_TOKEN", "admin-key")
	p, err := NewCPProvisioner()
	if err != nil {
		t.Fatalf("NewCPProvisioner: %v", err)
	}
	if p.cpAdminAPIKey != "admin-key" {
		t.Errorf("cpAdminAPIKey = %q, want %q", p.cpAdminAPIKey, "admin-key")
	}
}

// TestNewCPProvisioner_CPAdminAPITokenFallsBackToSharedSecret —
// operators that don't split the two secrets (dev / self-hosted) still
// get a working admin bearer via the fallback.
func TestNewCPProvisioner_CPAdminAPITokenFallsBackToSharedSecret(t *testing.T) {
	t.Setenv("MOLECULE_ORG_ID", "org-abc")
	t.Setenv("MOLECULE_CP_SHARED_SECRET", "shared")
	t.Setenv("CP_ADMIN_API_TOKEN", "")
	p, err := NewCPProvisioner()
	if err != nil {
		t.Fatalf("NewCPProvisioner: %v", err)
	}
	if p.cpAdminAPIKey != "shared" {
		t.Errorf("cpAdminAPIKey fallback = %q, want %q", p.cpAdminAPIKey, "shared")
	}
}

// TestStart_HappyPath — Start posts to the stubbed CP, passes the
// bearer, and parses the returned instance_id.
func TestStart_HappyPath(t *testing.T) {
	var sawBearer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawBearer = r.Header.Get("Authorization")
		if r.URL.Path != "/cp/workspaces/provision" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		// Verify the request body round-trips our fields
		var body cpProvisionRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.WorkspaceID != "ws-1" || body.Runtime != "python" {
			t.Errorf("body mismatch: %+v", body)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"instance_id":"i-abc123","state":"pending"}`)
	}))
	defer srv.Close()

	p := &CPProvisioner{
		baseURL:      srv.URL,
		orgID:        "org-1",
		sharedSecret: "s3cret",
		httpClient:   srv.Client(),
	}

	id, err := p.Start(context.Background(), WorkspaceConfig{
		WorkspaceID: "ws-1", Runtime: "python", Tier: 1, PlatformURL: "http://tenant",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if id != "i-abc123" {
		t.Errorf("instance id = %q, want i-abc123", id)
	}
	if sawBearer != "Bearer s3cret" {
		t.Errorf("server saw Authorization = %q, want Bearer s3cret", sawBearer)
	}
}

// TestStart_Non201ReturnsStructuredError — when CP returns 401 with a
// structured {"error":"..."} body, Start surfaces that error message.
// Verifies the defense against log-leaking raw upstream bodies.
func TestStart_Non201ReturnsStructuredError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"invalid credentials"}`)
	}))
	defer srv.Close()

	p := &CPProvisioner{baseURL: srv.URL, orgID: "org-1", httpClient: srv.Client()}

	_, err := p.Start(context.Background(), WorkspaceConfig{WorkspaceID: "ws-1", Runtime: "py"})
	if err == nil {
		t.Fatal("expected error on 401, got nil")
	}
	if !strings.Contains(err.Error(), "invalid credentials") {
		t.Errorf("error message %q should include upstream error field", err.Error())
	}
}

// TestStart_NoStructuredErrorFallsBackToSize — the anti-leak path:
// when upstream returns non-JSON, we refuse to log the body and
// report only the byte count, preventing Authorization header echoes
// from landing in our logs.
func TestStart_NoStructuredErrorFallsBackToSize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "raw proxy error page, could contain echoed headers")
	}))
	defer srv.Close()

	p := &CPProvisioner{baseURL: srv.URL, orgID: "org-1", httpClient: srv.Client()}

	_, err := p.Start(context.Background(), WorkspaceConfig{WorkspaceID: "ws-1", Runtime: "py"})
	if err == nil {
		t.Fatal("expected error on 500, got nil")
	}
	if strings.Contains(err.Error(), "raw proxy error") {
		t.Errorf("error leaked raw body: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "<unstructured body") {
		t.Errorf("expected byte-count fallback, got %q", err.Error())
	}
}

// TestStart_TransportFailureSurfaces — the CP isn't reachable at all
// (DNS fails, TCP refused, TLS handshake error). Start must return an
// error tagged with enough context to find the failed call in logs
// without leaking credentials.
func TestStart_TransportFailureSurfaces(t *testing.T) {
	// Port 1 is reserved by IANA; connect attempts fail immediately.
	p := &CPProvisioner{
		baseURL:    "http://127.0.0.1:1",
		orgID:      "org-1",
		httpClient: &http.Client{Timeout: 500 * time.Millisecond},
	}
	_, err := p.Start(context.Background(), WorkspaceConfig{WorkspaceID: "ws-1", Runtime: "py"})
	if err == nil {
		t.Fatal("expected transport error, got nil")
	}
	if !strings.Contains(err.Error(), "cp provisioner: send") {
		t.Errorf("error should be tagged cp provisioner: send, got %q", err.Error())
	}
}

// TestStop_SendsBothAuthHeaders — verify #118/#130 compliance on the
// teardown path. Any call to /cp/workspaces/:id must carry both the
// platform-wide shared secret AND the per-tenant admin token, or the
// CP will 401.
func TestStop_SendsBothAuthHeaders(t *testing.T) {
	// resolveInstanceID looks up the real EC2 id from the workspaces
	// table; previously this test passed when the tenant buggily
	// reused workspaceID AS instance_id. Now we assert the correct
	// EC2 id round-trips.
	primeInstanceIDLookup(t, map[string]string{"ws-1": "i-abc123"})

	var sawBearer, sawAdminToken, sawMethod, sawPath string
	var sawInstance string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawBearer = r.Header.Get("Authorization")
		sawAdminToken = r.Header.Get("X-Molecule-Admin-Token")
		sawMethod = r.Method
		sawPath = r.URL.Path
		sawInstance = r.URL.Query().Get("instance_id")
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
	if err := p.Stop(context.Background(), "ws-1"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if sawMethod != "DELETE" {
		t.Errorf("method = %q, want DELETE", sawMethod)
	}
	if sawPath != "/cp/workspaces/ws-1" {
		t.Errorf("path = %q, want /cp/workspaces/ws-1", sawPath)
	}
	if sawInstance != "i-abc123" {
		t.Errorf("instance_id query = %q, want i-abc123 (from DB lookup, NOT the workspace UUID)", sawInstance)
	}
	if sawBearer != "Bearer s3cret" {
		t.Errorf("bearer = %q, want Bearer s3cret", sawBearer)
	}
	if sawAdminToken != "tok-xyz" {
		t.Errorf("admin token = %q, want tok-xyz", sawAdminToken)
	}
}

// TestStop_TransportErrorSurfaces — same treatment as Start. If the
// teardown call hits a dead CP, the error must surface so the caller
// knows the workspace might still be running and needs retry.
func TestStop_TransportErrorSurfaces(t *testing.T) {
	primeInstanceIDLookup(t, map[string]string{"ws-1": "i-abc123"})
	p := &CPProvisioner{
		baseURL:    "http://127.0.0.1:1",
		orgID:      "org-1",
		httpClient: &http.Client{Timeout: 500 * time.Millisecond},
	}
	err := p.Stop(context.Background(), "ws-1")
	if err == nil {
		t.Fatal("expected transport error, got nil")
	}
	if !strings.Contains(err.Error(), "cp provisioner: stop") {
		t.Errorf("error should be tagged, got %q", err.Error())
	}
}

// TestStop_5xxResponseSurfacesError — pre-fix bug: the CP returning 500
// (AWS hiccup, missing IAM, transient outage) was silently treated as
// success because http.Client.Do only errors on transport failures.
// Result was a leaked workspace EC2 the orphan-report couldn't see.
// Workspace-server's Delete handler depends on this error to populate
// stopErrs and surface a 500 to its client (the loud-fail-instead-of-
// silent-leak choice documented in workspace_crud.go #1843).
func TestStop_5xxResponseSurfacesError(t *testing.T) {
	primeInstanceIDLookup(t, map[string]string{"ws-1": "i-leaksrc"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "ec2 terminate hit IAM rate limit", http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := &CPProvisioner{
		baseURL: srv.URL, orgID: "org-1",
		sharedSecret: "s", adminToken: "t",
		httpClient: srv.Client(),
	}
	err := p.Stop(context.Background(), "ws-1")
	if err == nil {
		t.Fatal("Stop swallowed CP 500 — leak source still open")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should contain status code, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "ws-1") {
		t.Errorf("error should name the workspace for triage, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "IAM rate limit") {
		t.Errorf("error should include CP body excerpt for triage, got %q", err.Error())
	}
}

// TestStop_4xxResponseSurfacesError — same shape as 5xx for the 4xx
// case (e.g. CP returns 404 because the instance_id was already gone,
// or 401 because PROVISION_SHARED_SECRET drifted). 404 in particular
// is a real signal — caller may want to short-circuit, but it must
// NOT be swallowed as success because it leaves the workspace row
// flipped to status='removed' on a different mismatch path.
func TestStop_4xxResponseSurfacesError(t *testing.T) {
	primeInstanceIDLookup(t, map[string]string{"ws-1": "i-gone"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"instance not found"}`))
	}))
	defer srv.Close()

	p := &CPProvisioner{
		baseURL: srv.URL, orgID: "org-1",
		sharedSecret: "s", adminToken: "t",
		httpClient: srv.Client(),
	}
	err := p.Stop(context.Background(), "ws-1")
	if err == nil {
		t.Fatal("Stop swallowed CP 404")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error should contain status, got %q", err.Error())
	}
}

// TestStop_2xxVariantsAllSucceed — ensures the new check accepts the
// full 2xx range. CP currently returns 200, but we have historically
// flipped between 200 and 204 No Content for DELETE; lock both in.
func TestStop_2xxVariantsAllSucceed(t *testing.T) {
	primeInstanceIDLookup(t, map[string]string{"ws-1": "i-ok"})
	for _, code := range []int{
		http.StatusOK,           // 200
		http.StatusAccepted,     // 202
		http.StatusNoContent,    // 204
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
		}))
		p := &CPProvisioner{
			baseURL: srv.URL, orgID: "org-1",
			sharedSecret: "s", adminToken: "t",
			httpClient: srv.Client(),
		}
		err := p.Stop(context.Background(), "ws-1")
		srv.Close()
		if err != nil {
			t.Errorf("Stop with CP HTTP %d returned err = %v, want nil", code, err)
		}
	}
}

// TestStop_LongBodyTruncatedInError — defensive: a pathological CP
// returning a multi-MB body shouldn't blow up the error message. The
// fix uses io.LimitReader(512) so logs stay sane during an outage.
func TestStop_LongBodyTruncatedInError(t *testing.T) {
	primeInstanceIDLookup(t, map[string]string{"ws-1": "i-bigbody"})
	bigBody := strings.Repeat("x", 10000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, bigBody)
	}))
	defer srv.Close()

	p := &CPProvisioner{
		baseURL: srv.URL, orgID: "org-1",
		sharedSecret: "s", adminToken: "t",
		httpClient: srv.Client(),
	}
	err := p.Stop(context.Background(), "ws-1")
	if err == nil {
		t.Fatal("Stop swallowed 502")
	}
	// Error should contain SOME body bytes (proves we read something) but
	// not all of it (proves the LimitReader actually capped).
	if !strings.Contains(err.Error(), "x") {
		t.Errorf("error should include body excerpt, got %q", err.Error())
	}
	if len(err.Error()) > 1024 {
		t.Errorf("error length = %d, want capped well below body length 10000", len(err.Error()))
	}
}

// TestIsRunning_ParsesStateField — CP returns the EC2 state, we expose
// a bool ("running"/"pending"/"terminated" → true only for "running").
func TestIsRunning_ParsesStateField(t *testing.T) {
	primeInstanceIDLookup(t, map[string]string{"ws-1": "i-parsesstatefield"})
	cases := map[string]bool{
		"running":    true,
		"pending":    false,
		"stopping":   false,
		"terminated": false,
	}
	for state, want := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/cp/workspaces/ws-1/status" {
				t.Errorf("path = %q", r.URL.Path)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"state":"`+state+`"}`)
		}))
		p := &CPProvisioner{
			baseURL:    srv.URL,
			orgID:      "org-1",
			sharedSecret: "s3cret",
			adminToken:   "tok-xyz",
			httpClient: srv.Client(),
		}
		got, err := p.IsRunning(context.Background(), "ws-1")
		srv.Close()
		if err != nil {
			t.Errorf("state=%s: IsRunning error %v", state, err)
			continue
		}
		if got != want {
			t.Errorf("state=%s: got %v, want %v", state, got, want)
		}
	}
}

// TestIsRunning_SendsBothAuthHeaders — parity with Stop. Status reads
// require the same per-tenant auth because they leak public_ip +
// private_ip to the caller.
func TestIsRunning_SendsBothAuthHeaders(t *testing.T) {
	primeInstanceIDLookup(t, map[string]string{"ws-1": "i-auth-headers"})
	var sawBearer, sawAdminToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawBearer = r.Header.Get("Authorization")
		sawAdminToken = r.Header.Get("X-Molecule-Admin-Token")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"state":"running"}`)
	}))
	defer srv.Close()

	p := &CPProvisioner{
		baseURL:      srv.URL,
		orgID:        "org-1",
		sharedSecret: "s3cret",
		adminToken:   "tok-xyz",
		httpClient:   srv.Client(),
	}
	_, _ = p.IsRunning(context.Background(), "ws-1")
	if sawBearer != "Bearer s3cret" {
		t.Errorf("bearer = %q, want Bearer s3cret", sawBearer)
	}
	if sawAdminToken != "tok-xyz" {
		t.Errorf("admin token = %q, want tok-xyz", sawAdminToken)
	}
}

// TestIsRunning_TransportErrorReturnsTrue — when the CP is
// unreachable, IsRunning must return (true, err) — matching the
// Docker provisioner contract so a2a_proxy stays on the alive path
// during a transient CP outage. Returning false here would trigger
// restart cascades on every brief CP blip.
//
// The sweeper (healthsweep.go) inspects err independently and skips
// on any error, so (true, err) is equally safe for that caller.
func TestIsRunning_TransportErrorReturnsTrue(t *testing.T) {
	primeInstanceIDLookup(t, map[string]string{"ws-1": "i-transport"})
	p := &CPProvisioner{
		baseURL:    "http://127.0.0.1:1",
		orgID:      "org-1",
		httpClient: &http.Client{Timeout: 500 * time.Millisecond},
	}
	got, err := p.IsRunning(context.Background(), "ws-1")
	if err == nil {
		t.Errorf("expected transport error, got nil (got=%v)", got)
	}
	if !got {
		t.Errorf("transport failure must report running=true so a2a_proxy stays on the alive path (matches Docker provisioner contract); got false")
	}
}

// TestIsRunning_Non2xxSurfacesError — a CP 500/502/etc. must NOT
// be silently treated as "workspace stopped". Previously the handler
// would decode an empty body → State="" → return (false, nil) and
// the sweeper would see the workspace as not-running. Now every
// non-2xx is an error the caller can log + retry.
func TestIsRunning_Non2xxSurfacesError(t *testing.T) {
	primeInstanceIDLookup(t, map[string]string{"ws-1": "i-non2xx"})
	cases := []struct {
		name   string
		status int
	}{
		{"500 internal", 500},
		{"502 bad gateway", 502},
		{"503 unavailable", 503},
		{"401 unauthorized", 401},
		{"404 not found", 404},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"state":"running"}`) // liar body — must not be trusted
			}))
			defer srv.Close()
			p := &CPProvisioner{baseURL: srv.URL, orgID: "org-1", httpClient: srv.Client()}

			got, err := p.IsRunning(context.Background(), "ws-1")
			if err == nil {
				t.Errorf("status %d: expected error, got nil", tc.status)
			}
			if !got {
				t.Errorf("status %d: must report running=true on non-2xx so a2a_proxy stays on alive path; got false", tc.status)
			}
			// Error must NOT echo the upstream body — CP 5xx bodies
			// can contain echoed headers and we don't want logs to
			// leak bearer tokens.
			if err != nil && strings.Contains(err.Error(), "running") {
				t.Errorf("status %d: error leaked upstream body: %q", tc.status, err.Error())
			}
		})
	}
}

// TestIsRunning_MalformedJSONBodyReturnsError — 200 but invalid JSON
// must surface an error rather than silently returning false. Prevents
// a middleware glitch (HTML error page with 200) from looking like
// "workspace stopped".
func TestIsRunning_MalformedJSONBodyReturnsError(t *testing.T) {
	primeInstanceIDLookup(t, map[string]string{"ws-1": "i-malformed"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "<html>maintenance mode</html>")
	}))
	defer srv.Close()
	p := &CPProvisioner{baseURL: srv.URL, orgID: "org-1", httpClient: srv.Client()}

	got, err := p.IsRunning(context.Background(), "ws-1")
	if err == nil {
		t.Errorf("malformed body: expected error, got nil (got=%v)", got)
	}
	if !got {
		t.Errorf("malformed body must report running=true so a2a_proxy stays on alive path; got false")
	}
}

// TestIsRunning_ContractCompat_A2AProxy — codifies the critical
// invariant that a2a_proxy.go line ~534 depends on: during CP
// transient errors, the handler must inspect `running`, see true,
// and skip the restart cascade. If this contract drifts (e.g., a
// future refactor returns false on error), every brief CP outage
// cascades into a workspace restart storm.
//
// This is a regression guard, not a functional test — it asserts
// the documented contract values rather than simulating the whole
// a2a_proxy flow.
func TestIsRunning_ContractCompat_A2AProxy(t *testing.T) {
	primeInstanceIDLookup(t, map[string]string{"ws-1": "i-contract"})
	// Simulate every error path and assert running==true for each.
	t.Run("transport error", func(t *testing.T) {
		p := &CPProvisioner{
			baseURL: "http://127.0.0.1:1", orgID: "org-1",
			httpClient: &http.Client{Timeout: 500 * time.Millisecond},
		}
		running, err := p.IsRunning(context.Background(), "ws-1")
		if err == nil || !running {
			t.Errorf("want (true, err); got (%v, %v)", running, err)
		}
	})
	t.Run("CP 500 response", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(500)
		}))
		defer srv.Close()
		p := &CPProvisioner{baseURL: srv.URL, orgID: "org-1", httpClient: srv.Client()}
		running, err := p.IsRunning(context.Background(), "ws-1")
		if err == nil || !running {
			t.Errorf("want (true, err); got (%v, %v)", running, err)
		}
	})
	t.Run("malformed 200 body", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(200)
			_, _ = io.WriteString(w, "garbage")
		}))
		defer srv.Close()
		p := &CPProvisioner{baseURL: srv.URL, orgID: "org-1", httpClient: srv.Client()}
		running, err := p.IsRunning(context.Background(), "ws-1")
		if err == nil || !running {
			t.Errorf("want (true, err); got (%v, %v)", running, err)
		}
	})
	// And the non-error paths must still report the truth.
	t.Run("2xx stopped → false nil", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(200)
			_, _ = io.WriteString(w, `{"state":"stopped"}`)
		}))
		defer srv.Close()
		p := &CPProvisioner{baseURL: srv.URL, orgID: "org-1", httpClient: srv.Client()}
		running, err := p.IsRunning(context.Background(), "ws-1")
		if err != nil || running {
			t.Errorf("want (false, nil); got (%v, %v)", running, err)
		}
	})
	t.Run("2xx running → true nil", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(200)
			_, _ = io.WriteString(w, `{"state":"running"}`)
		}))
		defer srv.Close()
		p := &CPProvisioner{baseURL: srv.URL, orgID: "org-1", httpClient: srv.Client()}
		running, err := p.IsRunning(context.Background(), "ws-1")
		if err != nil || !running {
			t.Errorf("want (true, nil); got (%v, %v)", running, err)
		}
	})
}

// TestIsRunning_BoundedBodyRead — IsRunning caps the decoded body at
// 64 KiB so a misconfigured/compromised CP streaming a huge body can't
// exhaust memory in this hot path. We serve a body larger than the cap
// and assert the decode either succeeds (json terminated within cap) or
// fails with a decode error (json truncated) — what MUST NOT happen is
// the whole body getting buffered.
//
// Implementation note: the test server writes a valid JSON object
// whose trailing whitespace pads the body past 64 KiB. The decoder
// only needs the prefix to produce a value, so the decode succeeds —
// and the LimitReader enforces the cap regardless.
func TestIsRunning_BoundedBodyRead(t *testing.T) {
	primeInstanceIDLookup(t, map[string]string{"ws-1": "i-bounded"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		// Valid JSON prefix, then pad well past the 64 KiB cap.
		_, _ = io.WriteString(w, `{"state":"running"}`)
		_, _ = io.WriteString(w, strings.Repeat(" ", 128<<10))
	}))
	defer srv.Close()

	p := &CPProvisioner{baseURL: srv.URL, orgID: "org-1", httpClient: srv.Client()}
	got, err := p.IsRunning(context.Background(), "ws-1")
	if err != nil {
		t.Fatalf("decode of prefix should succeed, got err %v", err)
	}
	if !got {
		t.Errorf("state=running in prefix should decode to true, got false")
	}
}

// TestClose_Noop — explicit contract: Close has no side effects and
// no error. Exists for the Provisioner interface; compliance guard.
func TestClose_Noop(t *testing.T) {
	p := &CPProvisioner{}
	if err := p.Close(); err != nil {
		t.Errorf("Close should return nil, got %v", err)
	}
}

// TestGetConsoleOutput_UsesAdminBearer — regression guard for the
// split-bearer fix. /cp/admin/workspaces/:id/console must send
// Authorization: Bearer <cpAdminAPIKey>, NOT <sharedSecret>.
// Previously the tenant sent sharedSecret → CP 401 → tenant 502 on
// the "View Logs" UI. Symptom log: "cp provisioner: console: unexpected 401"
// on hongmingwang prod tenant, 2026-04-22.
func TestGetConsoleOutput_UsesAdminBearer(t *testing.T) {
	var sawBearer, sawMethod, sawPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawBearer = r.Header.Get("Authorization")
		sawMethod = r.Method
		sawPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"output":"boot log"}`)
	}))
	defer srv.Close()

	p := &CPProvisioner{
		baseURL:       srv.URL,
		orgID:         "org-1",
		sharedSecret:  "provision-secret-do-not-use-here",
		adminToken:    "tok-xyz",
		cpAdminAPIKey: "admin-api-key",
		httpClient:    srv.Client(),
	}
	out, err := p.GetConsoleOutput(context.Background(), "ws-1")
	if err != nil {
		t.Fatalf("GetConsoleOutput: %v", err)
	}
	if out != "boot log" {
		t.Errorf("output = %q, want %q", out, "boot log")
	}
	if sawMethod != "GET" {
		t.Errorf("method = %q, want GET", sawMethod)
	}
	if sawPath != "/cp/admin/workspaces/ws-1/console" {
		t.Errorf("path = %q, want /cp/admin/workspaces/ws-1/console", sawPath)
	}
	if sawBearer != "Bearer admin-api-key" {
		t.Errorf("bearer = %q, want Bearer admin-api-key (NOT the provision secret)", sawBearer)
	}
}

// TestStop_EmptyInstanceIDIsNoop — when workspaces.instance_id is NULL
// (e.g. a row that was never provisioned, or deprovisioned and cleared),
// Stop should be a no-op instead of sending a malformed CP request.
// Regression guard: previously Stop sent workspaceID as instance_id
// even when no EC2 had been booked, causing CP → EC2 to 400.
func TestStop_EmptyInstanceIDIsNoop(t *testing.T) {
	// Empty map → lookup returns ("", nil) for any workspace.
	primeInstanceIDLookup(t, map[string]string{})

	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := &CPProvisioner{baseURL: srv.URL, orgID: "org-1", httpClient: srv.Client()}
	if err := p.Stop(context.Background(), "ws-ghost"); err != nil {
		t.Fatalf("Stop with empty instance_id should no-op, got err %v", err)
	}
	if hit {
		t.Errorf("Stop contacted CP even though instance_id was empty")
	}
}

// TestIsRunning_UsesDBInstanceID — IsRunning must also look up
// instance_id from the workspaces table, same as Stop. Mirror of
// TestStop_SendsBothAuthHeaders.
func TestIsRunning_UsesDBInstanceID(t *testing.T) {
	primeInstanceIDLookup(t, map[string]string{"ws-1": "i-xyz789"})

	var sawInstance string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawInstance = r.URL.Query().Get("instance_id")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"state":"running"}`)
	}))
	defer srv.Close()

	p := &CPProvisioner{baseURL: srv.URL, orgID: "org-1", httpClient: srv.Client()}
	if _, err := p.IsRunning(context.Background(), "ws-1"); err != nil {
		t.Fatalf("IsRunning: %v", err)
	}
	if sawInstance != "i-xyz789" {
		t.Errorf("instance_id query = %q, want i-xyz789 (from DB lookup)", sawInstance)
	}
}

// TestIsRunning_EmptyInstanceIDReturnsFalse — IsRunning on a
// workspace with no recorded EC2 instance must report (false, nil) so
// restart cascades re-provision fresh instead of looping on a stale
// row with no backing instance.
func TestIsRunning_EmptyInstanceIDReturnsFalse(t *testing.T) {
	primeInstanceIDLookup(t, map[string]string{})
	p := &CPProvisioner{baseURL: "http://unused", orgID: "org-1"}
	running, err := p.IsRunning(context.Background(), "ws-ghost")
	if err != nil {
		t.Errorf("IsRunning with empty instance_id should return (false, nil), got err %v", err)
	}
	if running {
		t.Errorf("IsRunning with empty instance_id should return running=false, got true")
	}
}
