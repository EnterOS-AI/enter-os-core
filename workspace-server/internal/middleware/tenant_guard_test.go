package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// helper: build a router with TenantGuard configured to `orgID` and a
// representative API route plus the public allowlisted ones (/health,
// /buildinfo, /metrics).
func newGuardedRouter(orgID string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(TenantGuardWithOrgID(orgID))
	r.GET("/health", func(c *gin.Context) { c.String(200, "ok") })
	r.GET("/buildinfo", func(c *gin.Context) { c.String(200, "buildinfo") })
	r.GET("/metrics", func(c *gin.Context) { c.String(200, "metrics") })
	r.GET("/workspaces", func(c *gin.Context) { c.String(200, "workspaces") })
	return r
}

func doRequest(r *gin.Engine, path, orgIDHeader string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	if orgIDHeader != "" {
		req.Header.Set("X-Molecule-Org-Id", orgIDHeader)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// MOLECULE_ORG_ID unset → passthrough. Existing self-hosted behavior preserved.
func TestTenantGuard_UnsetIsPassthrough(t *testing.T) {
	r := newGuardedRouter("")
	for _, path := range []string{"/health", "/metrics", "/workspaces"} {
		if w := doRequest(r, path, ""); w.Code != 200 {
			t.Errorf("%s: expected 200 with guard disabled, got %d", path, w.Code)
		}
	}
}

// Set + matching header → 200.
func TestTenantGuard_MatchingHeader(t *testing.T) {
	r := newGuardedRouter("org-abc")
	if w := doRequest(r, "/workspaces", "org-abc"); w.Code != 200 {
		t.Errorf("matching header: expected 200, got %d", w.Code)
	}
}

// Set + mismatching header → 404 (not 403 — don't leak tenant existence).
func TestTenantGuard_MismatchedHeaderIs404(t *testing.T) {
	r := newGuardedRouter("org-abc")
	w := doRequest(r, "/workspaces", "org-xyz")
	if w.Code != 404 {
		t.Errorf("mismatched header: expected 404, got %d", w.Code)
	}
	if w.Body.String() != "" {
		// Bouncing via AbortWithStatus leaves an empty body, which is what we
		// want — no response body means no tenant fingerprint.
		t.Errorf("expected empty body on 404, got %q", w.Body.String())
	}
}

// Set + missing header → 404.
func TestTenantGuard_MissingHeaderIs404(t *testing.T) {
	r := newGuardedRouter("org-abc")
	if w := doRequest(r, "/workspaces", ""); w.Code != 404 {
		t.Errorf("missing header: expected 404, got %d", w.Code)
	}
}

// Allowlisted paths bypass the guard even in tenant mode — required for health
// probes (Fly Machines checks), Prometheus scrape, and the redeploy-fleet
// /buildinfo verification step. /buildinfo without an org header used to
// 404-via-NoRoute → canvas (HTML), which made the redeploy verifier think
// every tenant was stale even when the binary was current. Pin this so a
// future allowlist edit can't silently regress that check.
func TestTenantGuard_AllowlistBypassesCheck(t *testing.T) {
	r := newGuardedRouter("org-abc")
	for _, path := range []string{"/health", "/buildinfo", "/metrics"} {
		w := doRequest(r, path, "") // no header
		if w.Code != 200 {
			t.Errorf("%s: allowlisted path should return 200 without header, got %d", path, w.Code)
		}
	}
}

// Workspace EC2s POST to these two paths during startup and do NOT have
// MOLECULE_ORG_ID to attach as a header — CP's user-data only exports
// WORKSPACE_ID + PLATFORM_URL + RUNTIME + PORT. Without this allowlist
// entry every workspace silently fails to register and sits in
// 'provisioning' until the 10-min sweeper — the same failure class
// that caused the 2026-04-21 prod incident.
func TestTenantGuard_AllowsWorkspaceRegistryPaths(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(TenantGuardWithOrgID("org-abc"))
	// Register stub handlers so the test distinguishes "guard rejected"
	// (404 from middleware) vs "route not matched" (404 from gin). The
	// actual registry handlers live elsewhere; we only care that the
	// guard doesn't abort before dispatch.
	r.POST("/registry/register", func(c *gin.Context) { c.String(200, "register-reached") })
	r.POST("/registry/heartbeat", func(c *gin.Context) { c.String(200, "heartbeat-reached") })

	for _, path := range []string{"/registry/register", "/registry/heartbeat"} {
		req := httptest.NewRequest("POST", path, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Errorf("%s: workspace boot path must bypass TenantGuard; got %d (body=%q)", path, w.Code, w.Body.String())
		}
	}
}

// Fly-Replay-Src state path: the production path. Control plane puts the
// bare UUID in state= (no prefix — Fly 502s on `=` in the state value).
// Fly injects the whole Fly-Replay-Src header on the replayed request.
func TestTenantGuard_AcceptsFlyReplaySrcState(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(TenantGuardWithOrgID("org-abc"))
	r.GET("/workspaces", func(c *gin.Context) { c.String(200, "ok") })

	req := httptest.NewRequest("GET", "/workspaces", nil)
	req.Header.Set("Fly-Replay-Src", "instance=src-123;region=ord;t=1700000000000;state=org-abc")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("Fly-Replay-Src state match: expected 200, got %d", w.Code)
	}
}

func TestTenantGuard_RejectsFlyReplaySrcMismatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(TenantGuardWithOrgID("org-abc"))
	r.GET("/workspaces", func(c *gin.Context) { c.String(200, "ok") })

	req := httptest.NewRequest("GET", "/workspaces", nil)
	req.Header.Set("Fly-Replay-Src", "state=org-xyz")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 404 {
		t.Errorf("mismatched Fly-Replay-Src state: expected 404, got %d", w.Code)
	}
}

func TestOrgIDFromReplaySrc(t *testing.T) {
	cases := map[string]string{
		"instance=x;region=ord;state=abc-123": "abc-123",
		"state=abc-123;instance=x":            "abc-123",
		"   state=abc-123  ":                  "abc-123",
		"instance=x;region=ord":               "", // no state
		"":                                    "", // empty header
		"garbage":                             "", // unparseable
	}
	for in, want := range cases {
		if got := orgIDFromReplaySrc(in); got != want {
			t.Errorf("orgIDFromReplaySrc(%q) = %q, want %q", in, got, want)
		}
	}
}

// Same-origin Canvas bypass: when CANVAS_PROXY_URL is set and Referer matches
// Host, the request is from the co-served Canvas and should pass through.
func TestTenantGuard_SameOriginCanvasBypass(t *testing.T) {
	origActive := canvasProxyActive
	canvasProxyActive = true
	defer func() { canvasProxyActive = origActive }()

	r := newGuardedRouter("org-abc")

	req := httptest.NewRequest("GET", "/workspaces", nil)
	req.Host = "molecule1.moleculesai.app"
	req.Header.Set("Referer", "https://molecule1.moleculesai.app/")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("same-origin canvas: expected 200, got %d", w.Code)
	}
}

// Same-origin Canvas bypass via Origin header (WebSocket upgrade path).
func TestTenantGuard_SameOriginCanvasViaOrigin(t *testing.T) {
	origActive := canvasProxyActive
	canvasProxyActive = true
	defer func() { canvasProxyActive = origActive }()

	r := newGuardedRouter("org-abc")

	req := httptest.NewRequest("GET", "/workspaces", nil)
	req.Host = "molecule1.moleculesai.app"
	req.Header.Set("Origin", "https://molecule1.moleculesai.app")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("same-origin canvas via Origin: expected 200, got %d", w.Code)
	}
}

// Same-origin Canvas bypass must NOT work when CANVAS_PROXY_URL is unset.
func TestTenantGuard_SameOriginCanvasInactiveWithoutEnv(t *testing.T) {
	origActive := canvasProxyActive
	canvasProxyActive = false
	defer func() { canvasProxyActive = origActive }()

	r := newGuardedRouter("org-abc")

	req := httptest.NewRequest("GET", "/workspaces", nil)
	req.Host = "molecule1.moleculesai.app"
	req.Header.Set("Referer", "https://molecule1.moleculesai.app/")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 404 {
		t.Errorf("same-origin canvas without CANVAS_PROXY_URL: expected 404, got %d", w.Code)
	}
}

// The allowlist is exact-match, not prefix. "/health/debug" must NOT bypass.
func TestTenantGuard_AllowlistIsExactMatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(TenantGuardWithOrgID("org-abc"))
	r.GET("/health/debug", func(c *gin.Context) { c.String(200, "debug") })

	req := httptest.NewRequest("GET", "/health/debug", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected /health/debug to be guarded (404), got %d", w.Code)
	}
}
