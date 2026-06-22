package router

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestIsCPProxyAllowedPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
		why  string
	}{
		// Allowed — canvas UI needs these
		{"/cp/auth/me", true, "auth check"},
		{"/cp/auth/tenant-member", true, "membership check"},
		{"/cp/auth/login", true, "return-flow login"},
		{"/cp/orgs", true, "list orgs"},
		{"/cp/orgs/acme", true, "get one org"},
		{"/cp/orgs/acme/provision-status", true, "provision poll"},
		{"/cp/billing/checkout", true, "Stripe checkout"},
		{"/cp/templates", true, "template registry"},
		{"/cp/templates/starter", true, "template detail"},
		{"/cp/legal/terms", true, "ToS document"},

		// Blocked — admin surface must not traverse the tenant proxy
		{"/cp/admin/orgs", false, "cross-tenant admin list (lateral movement)"},
		{"/cp/admin/tenants/other/diagnostics", false, "admin tenant probe"},
		{"/cp/admin/beta-allowlist", false, "beta admin"},
		{"/cp/workspaces/provision", false, "CP provisioning (shared-secret gate)"},
		{"/cp/internal/usage", false, "internal usage ingest"},
		{"/cp/tenants/config", false, "tenant-bootstrap config (admin_token gated)"},
		{"/cp/tenants/backup-report", false, "tenant-bootstrap backup (admin_token gated)"},

		// Edge cases
		{"/cp/", false, "empty suffix"},
		{"/cp", false, "no trailing slash"},
		{"/something-else", false, "not under /cp/"},
		{"/cp/auth", false, "prefix trailing-slash entries require subpath"},
		{"/cp/authsomething", false, "substring match defense"},
		{"/cp/orgsabc", false, "prefix match needs / or exact"},
	}
	for _, tc := range cases {
		got := isCPProxyAllowedPath(tc.path)
		if got != tc.want {
			t.Errorf("path %q: want %v (%s); got %v", tc.path, tc.want, tc.why, got)
		}
	}
}

func TestCPProxy_Allowlist_Blocks404(t *testing.T) {
	// Allowlist should return 404 before any upstream call.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("upstream must NOT be called for blocked paths")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	handler := newCPProxy(upstream.URL)
	r := gin.New()
	r.Any("/cp/*path", handler)

	w := newTestRecorder()
	req := httptest.NewRequest("GET", "/cp/admin/orgs", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("blocked path should 404; got %d", w.Code)
	}
}

func TestCPProxy_AllowedPathsForward(t *testing.T) {
	var receivedPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	handler := newCPProxy(upstream.URL)
	r := gin.New()
	r.Any("/cp/*path", handler)

	w := newTestRecorder()
	req := httptest.NewRequest("GET", "/cp/auth/me", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("allowed path should forward; got %d", w.Code)
	}
	if receivedPath != "/cp/auth/me" {
		t.Errorf("path not forwarded cleanly; got %q", receivedPath)
	}
}

func TestCPProxy_ForwardsCookiesAndAuth(t *testing.T) {
	// Cookie + Authorization must reach the CP — that's how
	// session verification + bearer auth work upstream.
	var gotCookie, gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie = r.Header.Get("Cookie")
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	handler := newCPProxy(upstream.URL)
	r := gin.New()
	r.Any("/cp/*path", handler)

	w := newTestRecorder()
	req := httptest.NewRequest("GET", "/cp/auth/me", nil)
	req.Header.Set("Cookie", "session=abc123")
	req.Header.Set("Authorization", "Bearer xyz")
	r.ServeHTTP(w, req)

	if gotCookie != "session=abc123" {
		t.Errorf("Cookie not forwarded: got %q", gotCookie)
	}
	if gotAuth != "Bearer xyz" {
		t.Errorf("Authorization not forwarded: got %q", gotAuth)
	}
}

func TestCPProxy_HostRewrittenToUpstream(t *testing.T) {
	var gotHost string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	handler := newCPProxy(upstream.URL)
	r := gin.New()
	r.Any("/cp/*path", handler)

	w := newTestRecorder()
	req := httptest.NewRequest("GET", "/cp/auth/me", nil)
	req.Host = "acme.moleculesai.app" // the tenant hostname the browser used
	r.ServeHTTP(w, req)

	// Host should be rewritten to the upstream's host so CP's
	// CORS + cookie-domain logic sees itself.
	if gotHost == "acme.moleculesai.app" {
		t.Errorf("Host was not rewritten; upstream still saw tenant Host: %q", gotHost)
	}
}
