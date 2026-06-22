package handlers

import (
	"bytes"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// ─────────────────────────────────────────────────────────────────────────────
// BundleHandler Import — JSON binding error cases
// ─────────────────────────────────────────────────────────────────────────────

func TestBundleImport_InvalidJSON(t *testing.T) {
	h := NewBundleHandler(nil, nil, "http://localhost:8080", t.TempDir(), nil)

	tests := []struct {
		name string
		body string
	}{
		{"not JSON", `not json at all`},
		{"truncated JSON", `{"name": "test",`},
		{"null", `null`},
		{"array", `[]`},
		{"number", `42`},
		{"boolean", `true`},
		{"string", `"just a string"`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest("POST", "/bundles/import", bytes.NewBufferString(tc.body))
			c.Request.Header.Set("Content-Type", "application/json")

			h.Import(c)

			if w.Code != http.StatusBadRequest {
				t.Errorf("invalid JSON %q: expected status %d, got %d", tc.body, http.StatusBadRequest, w.Code)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// BundleHandler Import — valid JSON routes to bundle.Import and returns 201
// ─────────────────────────────────────────────────────────────────────────────

func TestBundleImport_ValidJSON(t *testing.T) {
	mock := setupTestDB(t)
	_ = setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	h := NewBundleHandler(broadcaster, nil, "http://localhost:8080", t.TempDir(), nil)

	// bundle.Import does: INSERT workspaces, broadcast provisioning, then UPDATE runtime.
	// bundle.Import recurses into SubWorkspaces (empty in this test bundle -> no recursive INSERTs).
	mock.ExpectExec("INSERT INTO workspaces").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE workspaces SET runtime").
		WillReturnResult(sqlmock.NewResult(0, 1))

	body := `{"name": "test-workspace", "schema": "1.0", "tier": 3}`
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/bundles/import", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	h.Import(c)

	if w.Code != http.StatusCreated {
		t.Errorf("valid JSON: expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// BundleHandler Export — workspace not found (ErrNoRows → 404)
// ─────────────────────────────────────────────────────────────────────────────

func TestBundleExport_NotFound(t *testing.T) {
	mock := setupTestDB(t)
	_ = setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	h := NewBundleHandler(broadcaster, nil, "http://localhost:8080", t.TempDir(), nil)

	// bundle.Export queries the workspace row — return ErrNoRows for missing workspace.
	mock.ExpectQuery(`SELECT name, COALESCE\(role`).
		WithArgs("ws-nonexistent").
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-nonexistent"}}
	c.Request = httptest.NewRequest("GET", "/bundles/export/ws-nonexistent", nil)

	h.Export(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected status %d, got %d: %s", http.StatusNotFound, w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// BundleHandler Export — query error (DB error → 404, per bundle.Export semantics)
// ─────────────────────────────────────────────────────────────────────────────

func TestBundleExport_QueryError(t *testing.T) {
	mock := setupTestDB(t)
	_ = setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	h := NewBundleHandler(broadcaster, nil, "http://localhost:8080", t.TempDir(), nil)

	// Simulate a non-ErrNoRows DB error.
	mock.ExpectQuery(`SELECT name, COALESCE\(role`).
		WithArgs("ws-error").
		WillReturnError(sql.ErrConnDone)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-error"}}
	c.Request = httptest.NewRequest("GET", "/bundles/export/ws-error", nil)

	h.Export(c)

	// bundle.Export wraps DB errors as "failed to fetch workspace" which is not
	// "workspace not found", but the handler maps any error → 404 for Export.
	if w.Code != http.StatusNotFound {
		t.Errorf("expected status %d for DB error, got %d: %s", http.StatusNotFound, w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}
