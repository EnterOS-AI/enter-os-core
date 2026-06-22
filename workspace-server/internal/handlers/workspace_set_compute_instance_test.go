package handlers

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// SetComputeInstance repoints the tenant workspace record (instance_id +
// compute.provider) at the new cloud after a cross-cloud migration so the
// CP-instance reconciler stops self-healing on the dead AWS instance (#806).
func TestSetComputeInstance_HappyPath(t *testing.T) {
	h, mock := setupBootstrapHandler(t)

	mock.ExpectExec(`UPDATE workspaces\s+SET instance_id = \$2`).
		WithArgs("ws-migrated", "140729808", "hetzner").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-migrated"}}
	c.Request = httptest.NewRequest("POST", "/admin/workspaces/ws-migrated/set-compute-instance",
		bytes.NewBufferString(`{"instance_id":"140729808","provider":"hetzner"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h.SetComputeInstance(c)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// A missing instance_id is a 400 before any DB work — a repoint with no target
// box would be meaningless.
func TestSetComputeInstance_MissingInstanceIs400(t *testing.T) {
	h, _ := setupBootstrapHandler(t)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("POST", "/admin/workspaces/ws-1/set-compute-instance",
		bytes.NewBufferString(`{"provider":"hetzner"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h.SetComputeInstance(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
}

// An unknown provider is rejected (400) — only aws|hetzner|gcp are routable.
func TestSetComputeInstance_BadProviderIs400(t *testing.T) {
	h, _ := setupBootstrapHandler(t)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("POST", "/admin/workspaces/ws-1/set-compute-instance",
		bytes.NewBufferString(`{"instance_id":"i-1","provider":"azure"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h.SetComputeInstance(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
}

// A workspace id that matches no row is a 404 (0 rows affected) — the migrator
// can tell a stale id from a real repoint.
func TestSetComputeInstance_NoRowIs404(t *testing.T) {
	h, mock := setupBootstrapHandler(t)

	mock.ExpectExec(`UPDATE workspaces`).
		WithArgs("ws-gone", "i-1", "aws").
		WillReturnResult(sqlmock.NewResult(0, 0))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-gone"}}
	c.Request = httptest.NewRequest("POST", "/admin/workspaces/ws-gone/set-compute-instance",
		bytes.NewBufferString(`{"instance_id":"i-1","provider":"aws"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h.SetComputeInstance(c)

	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// A DB failure surfaces as 500 so the migrator can fail loudly rather than
// leave the tenant record stale (which would re-trigger the AWS self-heal).
func TestSetComputeInstance_DBErrorIs500(t *testing.T) {
	h, mock := setupBootstrapHandler(t)

	mock.ExpectExec(`UPDATE workspaces`).
		WithArgs("ws-1", "i-1", "hetzner").
		WillReturnError(errors.New("connection reset"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("POST", "/admin/workspaces/ws-1/set-compute-instance",
		bytes.NewBufferString(`{"instance_id":"i-1","provider":"hetzner"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h.SetComputeInstance(c)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d: %s", w.Code, w.Body.String())
	}
}
