package middleware

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// no_fail_open_test.go is the regression gate for the CTO directive
// "nothing should be fail-open" (branch harden/no-fail-open-auth).
//
// It asserts that AdminAuth and WorkspaceAuth fail CLOSED (401) under the
// EXACT conditions that used to trigger the removed dev-mode fail-open hatch:
//   - ADMIN_TOKEN unset, AND
//   - MOLECULE_ENV is a dev value ("development" / "dev"), AND
//   - any HasAnyLiveTokenGlobal state (0 = fresh install, 1 = post-workspace).
//
// To prove this is RED against the old behaviour: temporarily restore the
// `if isDevModeFailOpen() { c.Next(); return }` short-circuit in
// wsauth_middleware.go (and the Tier-1 `if adminSecret == "" { c.Next() }`
// branch) — every sub-case below flips from 401 to 200 and fails. After the
// hardening, all sub-cases are 401.

// failOpenConditions enumerates the (MOLECULE_ENV, hasLiveTokens) combinations
// that the removed hatch keyed on. ADMIN_TOKEN is always unset here — that was
// a precondition of the old fail-open.
var failOpenConditions = []struct {
	name      string
	molEnv    string
	liveCount int
}{
	{"dev_alias_fresh_install", "dev", 0},
	{"dev_alias_post_workspace", "dev", 1},
	{"development_fresh_install", "development", 0},
	{"development_post_workspace", "development", 1},
}

func TestAdminAuth_NoFailOpen_UnderOldHatchConditions(t *testing.T) {
	for _, tc := range failOpenConditions {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ADMIN_TOKEN", "")
			t.Setenv("MOLECULE_ENV", tc.molEnv)
			// Ensure no CP-session path can accidentally pass.
			t.Setenv("CP_UPSTREAM_URL", "")

			mockDB, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock.New: %v", err)
			}
			defer mockDB.Close()

			// AdminAuth always probes HasAnyLiveTokenGlobal (for the 503-on-
			// outage semantics), so it must be expected for both counts.
			mock.ExpectQuery(hasAnyLiveTokenGlobalQuery).
				WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(tc.liveCount))

			r := gin.New()
			r.GET("/admin/secrets", AdminAuth(mockDB), func(c *gin.Context) {
				c.JSON(http.StatusOK, gin.H{"ok": true})
			})

			w := httptest.NewRecorder()
			req, _ := http.NewRequest(http.MethodGet, "/admin/secrets", nil)
			r.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Errorf("AdminAuth must fail CLOSED under old hatch conditions "+
					"(MOLECULE_ENV=%q, ADMIN_TOKEN unset, liveTokens=%d): expected 401, got %d: %s",
					tc.molEnv, tc.liveCount, w.Code, w.Body.String())
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet sqlmock expectations: %v", err)
			}
		})
	}
}

func TestWorkspaceAuth_NoFailOpen_UnderOldHatchConditions(t *testing.T) {
	for _, tc := range failOpenConditions {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ADMIN_TOKEN", "")
			t.Setenv("MOLECULE_ENV", tc.molEnv)
			t.Setenv("CP_UPSTREAM_URL", "")

			// WorkspaceAuth 401s before any DB lookup when there is no
			// bearer / cookie, so no queries are expected regardless of
			// the nominal live-token count.
			mockDB, _, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock.New: %v", err)
			}
			defer mockDB.Close()

			r := gin.New()
			r.GET("/workspaces/:id/activity", WorkspaceAuth(mockDB), func(c *gin.Context) {
				c.JSON(http.StatusOK, gin.H{"ok": true})
			})

			w := httptest.NewRecorder()
			req, _ := http.NewRequest(http.MethodGet,
				"/workspaces/00000000-0000-0000-0000-000000000000/activity", nil)
			r.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Errorf("WorkspaceAuth must fail CLOSED under old hatch conditions "+
					"(MOLECULE_ENV=%q, ADMIN_TOKEN unset): expected 401, got %d: %s",
					tc.molEnv, w.Code, w.Body.String())
			}
		})
	}
}

// TestCanvasOrBearer_NoFailOpen_UnderOldHatchConditions is the regression gate
// for the two fail-open branches removed from CanvasOrBearer
// (harden/no-fail-open-auth, "nothing fail-open" pass 2):
//
//	(a) lazy-bootstrap pass: `if !hasLive { c.Next(); return }` — a zero-token
//	    install used to pass EVERYTHING through. Now a bearer-less request on a
//	    fresh install (HasAnyLiveTokenGlobal → 0) fails CLOSED with 401.
//	(b) fail-open-on-DB-error: `if err != nil { log; c.Next(); return }` — a
//	    HasAnyLiveTokenGlobal error used to ALLOW. Now it fails CLOSED with 503.
//
// Watch-it-fail: restore either short-circuit in CanvasOrBearer and the
// matching sub-case flips (401→200 / 503→200) and fails.
func TestCanvasOrBearer_NoFailOpen_UnderOldHatchConditions(t *testing.T) {
	// (a) Fresh install (0 live tokens), no bearer, no ADMIN_TOKEN → 401.
	t.Run("zero_token_install_no_bearer_fails_closed_401", func(t *testing.T) {
		t.Setenv("ADMIN_TOKEN", "")
		t.Setenv("CORS_ORIGINS", "")

		mockDB, mock, err := sqlmock.New()
		if err != nil {
			t.Fatalf("sqlmock.New: %v", err)
		}
		defer mockDB.Close()

		mock.ExpectQuery(hasAnyLiveTokenGlobalQuery).
			WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

		handlerCalled := false
		r := gin.New()
		r.PUT("/canvas/viewport", CanvasOrBearer(mockDB), func(c *gin.Context) {
			handlerCalled = true
			c.JSON(http.StatusOK, gin.H{"ok": true})
		})

		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPut, "/canvas/viewport", nil)
		r.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Errorf("CanvasOrBearer lazy-bootstrap fail-open removed: zero-token install must 401, got %d: %s",
				w.Code, w.Body.String())
		}
		if handlerCalled {
			t.Error("handler reached on a fresh-install bearer-less request — lazy-bootstrap fail-open not removed")
		}
	})

	// (b) Auth datastore error → 503 (NOT allow).
	t.Run("db_error_fails_closed_503", func(t *testing.T) {
		mockDB, mock, err := sqlmock.New()
		if err != nil {
			t.Fatalf("sqlmock.New: %v", err)
		}
		defer mockDB.Close()

		mock.ExpectQuery(hasAnyLiveTokenGlobalQuery).
			WillReturnError(http.ErrAbortHandler) // any non-nil error suffices

		handlerCalled := false
		r := gin.New()
		r.PUT("/canvas/viewport", CanvasOrBearer(mockDB), func(c *gin.Context) {
			handlerCalled = true
			c.JSON(http.StatusOK, gin.H{"ok": true})
		})

		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPut, "/canvas/viewport", nil)
		r.ServeHTTP(w, req)

		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("CanvasOrBearer DB-error fail-open removed: must 503, got %d: %s", w.Code, w.Body.String())
		}
		if handlerCalled {
			t.Error("handler reached on a datastore-error request — DB-error fail-open not removed")
		}
	})
}

// TestNoFailOpenAuthHelperReexists is a source-guard: it asserts that no
// fail-open auth helper (the removed isDevModeFailOpen / IsDevModeFailOpen)
// has crept back into the middleware package as real code. The replacement
// predicate is the NON-security isLocalDevEnv (bind / rate-limit only);
// re-introducing the old fail-open identifier as a declaration or call is a
// regression of the CTO directive.
//
// It matches the *invocation/declaration* form `isDevModeFailOpen(` (which
// only appears in live code) and deliberately ignores prose mentions in
// `//` comments, so the historical references kept in doc comments don't
// trip the guard.
func TestNoFailOpenAuthHelperReexists(t *testing.T) {
	forbidden := []string{"isDevModeFailOpen(", "IsDevModeFailOpen("}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		// Skip this guard file itself (it names the forbidden tokens on
		// purpose, including inside a comment).
		if name == "no_fail_open_test.go" {
			continue
		}
		data, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatalf("ReadFile %s: %v", name, err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			// Ignore single-line comments — historical mentions live there.
			code := line
			if idx := strings.Index(code, "//"); idx >= 0 {
				code = code[:idx]
			}
			for _, f := range forbidden {
				if strings.Contains(code, f) {
					t.Errorf("%s:%d uses forbidden fail-open auth helper %q — "+
						"the dev-mode fail-open hatch must stay removed (harden/no-fail-open-auth). "+
						"Use isLocalDevEnv (NON-security) for dev-only knobs instead.",
						name, i+1, strings.TrimSuffix(f, "("))
				}
			}
		}
	}
}
