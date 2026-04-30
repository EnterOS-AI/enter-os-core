package middleware

// Coverage tests for the CanvasOrBearer middleware and IsSameOriginCanvas
// per issue #1818. The existing wsauth_middleware_test.go covered the
// fail-open and Origin paths but missed the bearer-validate, admin-secret,
// same-origin-canvas, and IsSameOriginCanvas wrapper branches — leaving
// CanvasOrBearer at 50% and IsSameOriginCanvas at 0% line coverage.
//
// These tests target the gaps without re-asserting behavior already
// pinned by the existing suite.

import (
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// ── CanvasOrBearer: bearer-token branches (#1818) ───────────────────────────

// TestCanvasOrBearer_ValidBearer_Passes exercises path 1 (the success
// branch of the bearer-token validation block). Without this test the
// only "OK" path covered was the Origin allowlist match — bearer
// success was never asserted.
func TestCanvasOrBearer_ValidBearer_Passes(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer mockDB.Close()

	bearer := "valid-bearer-for-canvas-or-bearer"
	hash := sha256.Sum256([]byte(bearer))

	mock.ExpectQuery(hasAnyLiveTokenGlobalQuery).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(validateAnyTokenSelectQuery).
		WithArgs(hash[:]).
		WillReturnRows(sqlmock.NewRows([]string{"id", "workspace_id"}).AddRow("tok-1", "ws-x"))
	mock.ExpectExec(validateTokenUpdateQuery).
		WithArgs("tok-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	r := gin.New()
	r.PUT("/canvas/viewport", CanvasOrBearer(mockDB), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPut, "/canvas/viewport", nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("valid bearer: got %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock: %v", err)
	}
}

// TestCanvasOrBearer_InvalidBearer_Returns401 covers the rejection
// branch when a bearer is supplied but ValidateAnyToken fails. This
// is the auth-escape case the issue called out: an attacker with a
// revoked/expired token + matching Origin previously could bypass
// auth, so the code MUST reject on bad bearer instead of falling
// through to Origin.
func TestCanvasOrBearer_InvalidBearer_Returns401(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer mockDB.Close()

	bad := "expired-or-revoked-token"
	hash := sha256.Sum256([]byte(bad))

	mock.ExpectQuery(hasAnyLiveTokenGlobalQuery).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(validateAnyTokenSelectQuery).
		WithArgs(hash[:]).
		WillReturnRows(sqlmock.NewRows([]string{"id"})) // empty → ErrNoRows

	// Origin would otherwise grant access — assert it doesn't here.
	t.Setenv("CORS_ORIGINS", "https://acme.moleculesai.app")

	handlerCalled := false
	r := gin.New()
	r.PUT("/canvas/viewport", CanvasOrBearer(mockDB), func(c *gin.Context) {
		handlerCalled = true
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPut, "/canvas/viewport", nil)
	req.Header.Set("Authorization", "Bearer "+bad)
	req.Header.Set("Origin", "https://acme.moleculesai.app")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("invalid bearer with matching origin: got %d, want 401", w.Code)
	}
	if handlerCalled {
		t.Error("invalid bearer leaked to handler — Origin fallback bypassed bearer validation")
	}
}

// TestCanvasOrBearer_AdminTokenEnv_Passes exercises the ADMIN_TOKEN
// constant-time-compare branch. The env-secret short-circuit is what
// keeps the canvas dashboard usable when no DB-backed token rows are
// provisioned yet (Hyperion bootstrap path).
func TestCanvasOrBearer_AdminTokenEnv_Passes(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer mockDB.Close()

	mock.ExpectQuery(hasAnyLiveTokenGlobalQuery).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	// No ValidateAnyToken expectation — env match short-circuits before that.

	t.Setenv("ADMIN_TOKEN", "platform-admin-secret")

	r := gin.New()
	r.PUT("/canvas/viewport", CanvasOrBearer(mockDB), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPut, "/canvas/viewport", nil)
	req.Header.Set("Authorization", "Bearer platform-admin-secret")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("admin env match: got %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock: %v", err)
	}
}

// TestCanvasOrBearer_DBError_FailOpen pins the documented behavior on a
// HasAnyLiveTokenGlobal failure. The middleware logs and falls open so a
// flaky DB doesn't lock canvas users out of cosmetic routes. Hardcoded in
// the comment block; this is a reminder if anyone changes that semantic.
func TestCanvasOrBearer_DBError_FailOpen(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer mockDB.Close()
	mock.ExpectQuery(hasAnyLiveTokenGlobalQuery).
		WillReturnError(http.ErrAbortHandler) // any non-nil error suffices

	r := gin.New()
	r.PUT("/canvas/viewport", CanvasOrBearer(mockDB), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPut, "/canvas/viewport", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("DB error fail-open: got %d, want 200 (%s)", w.Code, w.Body.String())
	}
}

// TestCanvasOrBearer_SameOriginCanvas_Passes exercises path 3: when no
// CORS Origin matches but the Referer/Host pair indicates the canvas is
// served from the same combined-tenant image. CANVAS_PROXY_URL gates
// this branch — without flipping canvasProxyActive the path is dead.
func TestCanvasOrBearer_SameOriginCanvas_Passes(t *testing.T) {
	prev := canvasProxyActive
	canvasProxyActive = true
	defer func() { canvasProxyActive = prev }()

	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer mockDB.Close()
	mock.ExpectQuery(hasAnyLiveTokenGlobalQuery).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	r := gin.New()
	r.PUT("/canvas/viewport", CanvasOrBearer(mockDB), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPut, "/canvas/viewport", nil)
	req.Host = "tenant.moleculesai.app"
	req.Header.Set("Referer", "https://tenant.moleculesai.app/dashboard")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("same-origin canvas: got %d, want 200 (%s)", w.Code, w.Body.String())
	}
}

// ── IsSameOriginCanvas: exported wrapper + branch coverage (#1818) ──────────

// TestIsSameOriginCanvas_ExportedWrapper_DelegatesToInternal — IsSame...
// is at 0% per the audit because nothing in the test suite calls the
// exported variant. It's a one-line delegation but the auth-boundary
// surface must stay testable from outside the package.
func TestIsSameOriginCanvas_ExportedWrapper_DelegatesToInternal(t *testing.T) {
	prev := canvasProxyActive
	canvasProxyActive = true
	defer func() { canvasProxyActive = prev }()

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/x", nil)
	c.Request.Host = "tenant.moleculesai.app"
	c.Request.Header.Set("Referer", "https://tenant.moleculesai.app/x")

	if !IsSameOriginCanvas(c) {
		t.Error("exported IsSameOriginCanvas should accept matching Referer")
	}
}

// TestIsSameOriginCanvas_DisabledByEnv — when CANVAS_PROXY_URL was unset
// at boot, canvasProxyActive is false and the wrapper must return false
// even on a perfect Referer match. Self-hosted / dev installs rely on
// this short-circuit.
func TestIsSameOriginCanvas_DisabledByEnv(t *testing.T) {
	prev := canvasProxyActive
	canvasProxyActive = false
	defer func() { canvasProxyActive = prev }()

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/x", nil)
	c.Request.Host = "tenant.moleculesai.app"
	c.Request.Header.Set("Referer", "https://tenant.moleculesai.app/x")

	if IsSameOriginCanvas(c) {
		t.Error("CANVAS_PROXY_URL unset → IsSameOriginCanvas must return false")
	}
}

// TestIsSameOriginCanvas_BranchCoverage exercises every Referer/Origin
// combination isSameOriginCanvas accepts or rejects. Table-driven so
// adding a new edge case is one row.
func TestIsSameOriginCanvas_BranchCoverage(t *testing.T) {
	prev := canvasProxyActive
	canvasProxyActive = true
	defer func() { canvasProxyActive = prev }()

	cases := []struct {
		name    string
		host    string
		referer string
		origin  string
		want    bool
	}{
		// Referer accepts:
		{"https referer matches host with path", "h.example.com", "https://h.example.com/dash", "", true},
		{"http referer matches host with path", "h.example.com", "http://h.example.com/dash", "", true},
		{"https referer matches host root no path", "h.example.com", "https://h.example.com", "", true},
		{"http referer matches host root no path", "h.example.com", "http://h.example.com", "", true},

		// Origin fallback (no Referer, used by WS upgrade):
		{"https origin only (no referer)", "h.example.com", "", "https://h.example.com", true},
		{"http origin only (no referer)", "h.example.com", "", "http://h.example.com", true},

		// Reject paths — the security-critical ones:
		{"empty host short-circuits", "", "https://h.example.com/", "", false},
		{"referer host suffix attack — h.example.com.evil.com", "h.example.com", "https://h.example.com.evil.com/", "", false},
		{"referer different host", "h.example.com", "https://other.example.com/", "", false},
		{"origin different host", "h.example.com", "", "https://other.example.com", false},
		{"no referer no origin", "h.example.com", "", "", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodGet, "/x", nil)
			ctx.Request.Host = c.host
			if c.referer != "" {
				ctx.Request.Header.Set("Referer", c.referer)
			}
			if c.origin != "" {
				ctx.Request.Header.Set("Origin", c.origin)
			}
			if got := isSameOriginCanvas(ctx); got != c.want {
				t.Errorf("isSameOriginCanvas(host=%q, referer=%q, origin=%q) = %v, want %v",
					c.host, c.referer, c.origin, got, c.want)
			}
		})
	}
}
