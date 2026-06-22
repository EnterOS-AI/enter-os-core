package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func TestSecurityHeaders(t *testing.T) {
	r := gin.New()
	r.Use(SecurityHeaders())
	r.GET("/test", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/test", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	tests := []struct {
		header string
		want   string
	}{
		{"X-Content-Type-Options", "nosniff"},
		{"X-Frame-Options", "DENY"},
		{"Strict-Transport-Security", "max-age=31536000; includeSubDomains"},
		// #282: regression guards for the two headers that were
		// documented in CLAUDE.md but missing from the implementation.
		{"Referrer-Policy", "strict-origin-when-cross-origin"},
		{"Permissions-Policy", "camera=(), microphone=(), geolocation=()"},
	}

	for _, tt := range tests {
		got := w.Header().Get(tt.header)
		if got != tt.want {
			t.Errorf("header %s = %q, want %q", tt.header, got, tt.want)
		}
	}

	// /test is not a registered API prefix → canvas-style permissive CSP.
	// Fragment-match rather than exact — CSP subsource lists may be tuned
	// without changing the security posture.
	csp := w.Header().Get("Content-Security-Policy")
	for _, fragment := range []string{
		"default-src 'self'",
		"script-src 'self' 'unsafe-inline'",
		"style-src 'self' 'unsafe-inline'",
		// img-src must allow the generated-image R2 host (default wildcard)
		// in addition to self/data:/blob: — see generatedImageImgSrc().
		"img-src 'self' data: blob: https://*.r2.cloudflarestorage.com",
		"frame-src 'self' blob:",
		"connect-src 'self' ws: wss:",
		"font-src 'self' data:",
	} {
		if !strings.Contains(csp, fragment) {
			t.Errorf("CSP missing expected fragment %q (full CSP: %q)", fragment, csp)
		}
	}
}

func TestSecurityHeadersPresenceOnMultipleRoutes(t *testing.T) {
	r := gin.New()
	r.Use(SecurityHeaders())
	r.GET("/a", func(c *gin.Context) { c.String(http.StatusOK, "a") })
	r.POST("/b", func(c *gin.Context) { c.String(http.StatusCreated, "b") })

	// GET /a
	w1 := httptest.NewRecorder()
	req1, _ := http.NewRequest(http.MethodGet, "/a", nil)
	r.ServeHTTP(w1, req1)

	if v := w1.Header().Get("X-Frame-Options"); v != "DENY" {
		t.Errorf("GET /a: X-Frame-Options = %q, want DENY", v)
	}

	// POST /b
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest(http.MethodPost, "/b", nil)
	r.ServeHTTP(w2, req2)

	if v := w2.Header().Get("X-Content-Type-Options"); v != "nosniff" {
		t.Errorf("POST /b: X-Content-Type-Options = %q, want nosniff", v)
	}
	if v := w2.Header().Get("Strict-Transport-Security"); v != "max-age=31536000; includeSubDomains" {
		t.Errorf("POST /b: Strict-Transport-Security = %q, want max-age=31536000; includeSubDomains", v)
	}
	// /a and /b are not API prefixes → canvas-style permissive CSP.
	csp := w2.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'self'") {
		t.Errorf("POST /b: CSP missing default-src 'self' (full: %q)", csp)
	}
}

func TestSecurityHeadersDoNotOverrideExisting(t *testing.T) {
	r := gin.New()
	r.Use(SecurityHeaders())
	r.GET("/custom", func(c *gin.Context) {
		// Handler sets its own X-Frame-Options — SecurityHeaders runs before
		// the handler, so the handler's value will take precedence.
		c.Header("X-Frame-Options", "SAMEORIGIN")
		c.String(http.StatusOK, "custom")
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/custom", nil)
	r.ServeHTTP(w, req)

	// The handler's value should be present (may override middleware's)
	got := w.Header().Get("X-Frame-Options")
	if got != "SAMEORIGIN" {
		t.Errorf("expected handler override SAMEORIGIN, got %q", got)
	}
}

// TestCSPAPIRoutesGetStrictPolicy verifies that all registered Go platform
// API prefixes receive a strict "default-src 'self'" CSP with no unsafe
// directives. This is the core fix for issue #450.
func TestCSPAPIRoutesGetStrictPolicy(t *testing.T) {
	r := gin.New()
	r.Use(SecurityHeaders())
	// Register representative routes for each API prefix.
	for _, prefix := range apiPrefixes {
		prefix := prefix // capture
		r.GET(prefix, func(c *gin.Context) { c.JSON(http.StatusOK, nil) })
		r.GET(prefix+"/sub", func(c *gin.Context) { c.JSON(http.StatusOK, nil) })
	}

	strictCSP := "default-src 'self'"

	paths := make([]string, 0, len(apiPrefixes)*2)
	for _, p := range apiPrefixes {
		paths = append(paths, p, p+"/sub")
	}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			w := httptest.NewRecorder()
			req, _ := http.NewRequest(http.MethodGet, path, nil)
			r.ServeHTTP(w, req)

			csp := w.Header().Get("Content-Security-Policy")
			if csp != strictCSP {
				t.Errorf("API path %q: want strict CSP %q, got %q", path, strictCSP, csp)
			}
			// Belt-and-suspenders: confirm no unsafe directives leak through.
			for _, bad := range []string{"unsafe-inline", "unsafe-eval"} {
				if strings.Contains(csp, bad) {
					t.Errorf("API path %q: CSP must not contain %q, got %q", path, bad, csp)
				}
			}
		})
	}
}

// TestCSPCanvasRoutesGetPermissivePolicy verifies that paths not in the API
// prefix list receive the permissive CSP needed for Next.js hydration.
func TestCSPCanvasRoutesGetPermissivePolicy(t *testing.T) {
	r := gin.New()
	r.Use(SecurityHeaders())
	// Simulate canvas/NoRoute paths — register them explicitly so Gin
	// doesn't 404 before reaching our middleware.
	r.GET("/", func(c *gin.Context) { c.String(http.StatusOK, "<html/>") })
	r.GET("/canvas", func(c *gin.Context) { c.String(http.StatusOK, "<html/>") })
	r.GET("/canvas/some-page", func(c *gin.Context) { c.String(http.StatusOK, "<html/>") })
	r.GET("/some-unknown-path", func(c *gin.Context) { c.String(http.StatusOK, "<html/>") })

	canvasPaths := []string{
		"/",
		"/canvas",
		"/canvas/some-page",
		"/some-unknown-path",
	}

	for _, path := range canvasPaths {
		t.Run(path, func(t *testing.T) {
			w := httptest.NewRecorder()
			req, _ := http.NewRequest(http.MethodGet, path, nil)
			r.ServeHTTP(w, req)

			csp := w.Header().Get("Content-Security-Policy")
			// Canvas CSP must contain unsafe-inline for Next.js hydration.
			if !strings.Contains(csp, "'unsafe-inline'") {
				t.Errorf("canvas path %q: CSP should contain 'unsafe-inline' for Next.js, got %q", path, csp)
			}
			// 'unsafe-eval' must NOT be present — it was removed after
			// confirming production canvas renders without it.
			if strings.Contains(csp, "'unsafe-eval'") {
				t.Errorf("canvas path %q: CSP must not contain 'unsafe-eval', got %q", path, csp)
			}
			if !strings.Contains(csp, "frame-src 'self' blob:") {
				t.Errorf("canvas path %q: CSP should allow blob: frames for PDF previews, got %q", path, csp)
			}
		})
	}
}

// TestCSPGeneratedImageHost verifies the canvas CSP allows the generated-image
// R2 host in img-src (so image-gen results render) WITHOUT widening connect-src
// (no exfiltration channel). Covers both the default wildcard and a pinned host
// via MOLECULE_IMAGE_GEN_R2_HOST.
func TestCSPGeneratedImageHost(t *testing.T) {
	t.Run("default wildcard", func(t *testing.T) {
		t.Setenv("MOLECULE_IMAGE_GEN_R2_HOST", "")
		r := gin.New()
		r.Use(SecurityHeaders())
		r.GET("/", func(c *gin.Context) { c.String(http.StatusOK, "<html/>") })

		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, "/", nil)
		r.ServeHTTP(w, req)

		csp := w.Header().Get("Content-Security-Policy")
		imgSrc := cspDirective(csp, "img-src")
		if !strings.Contains(imgSrc, "https://*.r2.cloudflarestorage.com") {
			t.Errorf("img-src should allow R2 wildcard for generated images, got %q", imgSrc)
		}
		// img-src must still keep self/data:/blob: for canvas's own assets.
		for _, want := range []string{"'self'", "data:", "blob:"} {
			if !strings.Contains(imgSrc, want) {
				t.Errorf("img-src missing %q, got %q", want, imgSrc)
			}
		}
		// connect-src must NOT be widened to R2 — fetch()/XHR to R2 stays blocked.
		connectSrc := cspDirective(csp, "connect-src")
		if strings.Contains(connectSrc, "r2.cloudflarestorage.com") {
			t.Errorf("connect-src must NOT include R2 (no exfil channel), got %q", connectSrc)
		}
	})

	t.Run("pinned host", func(t *testing.T) {
		const pinned = "https://molecule-workspace-data.deadbeef.r2.cloudflarestorage.com"
		t.Setenv("MOLECULE_IMAGE_GEN_R2_HOST", pinned)
		r := gin.New()
		r.Use(SecurityHeaders())
		r.GET("/", func(c *gin.Context) { c.String(http.StatusOK, "<html/>") })

		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, "/", nil)
		r.ServeHTTP(w, req)

		imgSrc := cspDirective(w.Header().Get("Content-Security-Policy"), "img-src")
		if !strings.Contains(imgSrc, pinned) {
			t.Errorf("img-src should use the pinned R2 host, got %q", imgSrc)
		}
		if strings.Contains(imgSrc, "*.r2.cloudflarestorage.com") {
			t.Errorf("img-src should NOT fall back to wildcard when pinned, got %q", imgSrc)
		}
	})

	t.Run("api routes stay strict", func(t *testing.T) {
		t.Setenv("MOLECULE_IMAGE_GEN_R2_HOST", "")
		r := gin.New()
		r.Use(SecurityHeaders())
		r.GET("/workspaces/abc", func(c *gin.Context) { c.JSON(http.StatusOK, nil) })

		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, "/workspaces/abc", nil)
		r.ServeHTTP(w, req)

		csp := w.Header().Get("Content-Security-Policy")
		if csp != "default-src 'self'" {
			t.Errorf("API route must keep strict CSP (no R2 host), got %q", csp)
		}
	})
}

// cspDirective extracts the single directive (e.g. "img-src ...") from a CSP
// header value, or "" if absent.
func cspDirective(csp, name string) string {
	for _, d := range strings.Split(csp, ";") {
		d = strings.TrimSpace(d)
		if strings.HasPrefix(d, name+" ") || d == name {
			return d
		}
	}
	return ""
}

// TestSecurityHeaders_614_NosniffOnSSEAndAPIEndpoints is the acceptance test for
// issue #614 — verifies X-Content-Type-Options: nosniff and X-Frame-Options: DENY
// are present on API and SSE paths. SecurityHeaders() was already wired globally
// in router.go (issue #151), so this test pins that contract against regression.
func TestSecurityHeaders_614_NosniffOnSSEAndAPIEndpoints(t *testing.T) {
	r := gin.New()
	r.Use(SecurityHeaders())

	// Register a sample of high-value endpoints that #614 flagged.
	r.GET("/workspaces/ws-1/events/stream", func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		c.String(http.StatusOK, "data: ping\n\n")
	})
	r.GET("/settings/secrets", func(c *gin.Context) {
		c.JSON(http.StatusOK, nil)
	})
	r.GET("/events/ws-1", func(c *gin.Context) {
		c.JSON(http.StatusOK, nil)
	})
	r.GET("/orgs/org-1/plugins/allowlist", func(c *gin.Context) {
		c.JSON(http.StatusOK, nil)
	})

	paths := []string{
		"/workspaces/ws-1/events/stream",
		"/settings/secrets",
		"/events/ws-1",
		"/orgs/org-1/plugins/allowlist",
	}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			w := httptest.NewRecorder()
			req, _ := http.NewRequest(http.MethodGet, path, nil)
			r.ServeHTTP(w, req)

			if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("#614 %s: X-Content-Type-Options = %q, want nosniff", path, got)
			}
			if got := w.Header().Get("X-Frame-Options"); got != "DENY" {
				t.Errorf("#614 %s: X-Frame-Options = %q, want DENY", path, got)
			}
		})
	}
}

// TestIsAPIPath unit-tests the path classifier directly.
func TestIsAPIPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		// Exact prefix matches
		{"/workspaces", true},
		{"/health", true},
		{"/admin", true},
		{"/metrics", true},
		{"/registry", true},
		{"/settings", true},
		{"/bundles", true},
		{"/org", true},
		{"/templates", true},
		{"/plugins", true},
		{"/webhooks", true},
		{"/channels", true},
		{"/ws", true},
		{"/events", true},
		{"/approvals", true},
		{"/orgs", true}, // #610 allowlist routes
		{"/orgs/org-1/plugins/allowlist", true},
		// Sub-paths
		{"/workspaces/abc-123", true},
		{"/workspaces/abc-123/state", true},
		{"/registry/discover/xyz", true},
		{"/admin/liveness", true},
		// Canvas / non-API paths
		{"/", false},
		{"/canvas", false},
		{"/canvas/viewport", false}, // returned by Next.js canvas page, not the Go API
		{"/some-page", false},
		{"/_next/static/chunks/main.js", false},
		// Ensure prefix is not a substring match (e.g. "/workspaces" should
		// not match "/workspacesXXX").
		{"/workspacesX", false},
		{"/healthcheck", false},
	}

	for _, tc := range cases {
		got := isAPIPath(tc.path)
		if got != tc.want {
			t.Errorf("isAPIPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}
