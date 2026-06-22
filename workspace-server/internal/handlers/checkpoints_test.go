package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/gin-gonic/gin"
)

// newCheckpointsHandler is a test helper that constructs a CheckpointsHandler
// backed by the sqlmock DB set up by setupTestDB.
func newCheckpointsHandler(t *testing.T, mock sqlmock.Sqlmock) *CheckpointsHandler {
	t.Helper()
	_ = mock // surfaced for callers that need to set expectations
	return NewCheckpointsHandler(db.DB)
}

// ---------- Upsert ----------

// TestCheckpointsUpsert_CreatesNew verifies that a valid POST inserts a new
// checkpoint row and returns 201 with the generated id.
func TestCheckpointsUpsert_CreatesNew(t *testing.T) {
	mock := setupTestDB(t)
	h := newCheckpointsHandler(t, mock)

	mock.ExpectQuery("INSERT INTO workflow_checkpoints").
		WithArgs("ws-1", "wf-abc", "step-init", 0, "null").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ckpt-001"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	body := `{"workflow_id":"wf-abc","step_name":"step-init","step_index":0}`
	c.Request = httptest.NewRequest("POST", "/", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	h.Upsert(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["id"] != "ckpt-001" {
		t.Errorf("expected id 'ckpt-001', got %v", resp["id"])
	}
	if resp["workflow_id"] != "wf-abc" {
		t.Errorf("expected workflow_id 'wf-abc', got %v", resp["workflow_id"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestCheckpointsUpsert_UpdatesExisting verifies that re-POSTing the same
// (workspace_id, workflow_id, step_name) triple updates the existing row via
// ON CONFLICT DO UPDATE and still returns 201.
func TestCheckpointsUpsert_UpdatesExisting(t *testing.T) {
	mock := setupTestDB(t)
	h := newCheckpointsHandler(t, mock)

	// ON CONFLICT DO UPDATE — same SQL, returns existing id.
	mock.ExpectQuery("INSERT INTO workflow_checkpoints").
		WithArgs("ws-1", "wf-abc", "step-init", 2, "null").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ckpt-001"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	body := `{"workflow_id":"wf-abc","step_name":"step-init","step_index":2}`
	c.Request = httptest.NewRequest("POST", "/", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	h.Upsert(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 on update, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["id"] != "ckpt-001" {
		t.Errorf("expected existing id 'ckpt-001', got %v", resp["id"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestCheckpointsUpsert_WithPayload verifies that a non-empty payload is
// forwarded to the DB as-is (stringified JSONB).
func TestCheckpointsUpsert_WithPayload(t *testing.T) {
	mock := setupTestDB(t)
	h := newCheckpointsHandler(t, mock)

	mock.ExpectQuery("INSERT INTO workflow_checkpoints").
		WithArgs("ws-2", "wf-xyz", "step-process", 1, `{"result":"ok"}`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ckpt-002"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-2"}}
	body := `{"workflow_id":"wf-xyz","step_name":"step-process","step_index":1,"payload":{"result":"ok"}}`
	c.Request = httptest.NewRequest("POST", "/", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	h.Upsert(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ---------- List ----------

// TestCheckpointsList_OrderedByStepIndex verifies that List returns rows
// ordered by step_index DESC (highest step first, as the DB provides).
func TestCheckpointsList_OrderedByStepIndex(t *testing.T) {
	mock := setupTestDB(t)
	h := newCheckpointsHandler(t, mock)

	cols := []string{"id", "workspace_id", "workflow_id", "step_name", "step_index", "completed_at", "payload"}
	mock.ExpectQuery("SELECT id, workspace_id, workflow_id, step_name, step_index").
		WithArgs("ws-1", "wf-abc").
		WillReturnRows(sqlmock.NewRows(cols).
			AddRow("ckpt-b", "ws-1", "wf-abc", "step-two", 2, "2026-04-17T10:01:00Z", nil).
			AddRow("ckpt-a", "ws-1", "wf-abc", "step-one", 1, "2026-04-17T10:00:00Z", nil))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "wfid", Value: "wf-abc"}}
	c.Request = httptest.NewRequest("GET", "/", nil)

	h.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var result []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 checkpoints, got %d", len(result))
	}
	// DB returns pre-ordered (step_index DESC); first entry must be step 2.
	if result[0]["step_name"] != "step-two" {
		t.Errorf("expected step-two first (step_index=2), got %v", result[0]["step_name"])
	}
	if result[1]["step_name"] != "step-one" {
		t.Errorf("expected step-one second (step_index=1), got %v", result[1]["step_name"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestCheckpointsList_NotFound verifies that List returns 404 when no
// checkpoints exist for the given workflow.
func TestCheckpointsList_NotFound(t *testing.T) {
	mock := setupTestDB(t)
	h := newCheckpointsHandler(t, mock)

	cols := []string{"id", "workspace_id", "workflow_id", "step_name", "step_index", "completed_at", "payload"}
	mock.ExpectQuery("SELECT id, workspace_id, workflow_id, step_name, step_index").
		WithArgs("ws-1", "wf-missing").
		WillReturnRows(sqlmock.NewRows(cols)) // empty

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "wfid", Value: "wf-missing"}}
	c.Request = httptest.NewRequest("GET", "/", nil)

	h.List(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for unknown workflow, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestCheckpointsList_RowsErr_Returns500 verifies that a rows.Err() set on
// the very first rows.Next() call causes the handler to return 500 rather
// than an empty 404.
//
// RowError(0, ...) fires on the first advance — rows.Next() returns false
// immediately with the injected error, rows.Err() is non-nil, and the
// handler must detect it and return 500. This exercises the rows.Err()
// guard that lives after the scan loop.
func TestCheckpointsList_RowsErr_Returns500(t *testing.T) {
	mock := setupTestDB(t)
	h := newCheckpointsHandler(t, mock)

	cols := []string{"id", "workspace_id", "workflow_id", "step_name", "step_index", "completed_at", "payload"}
	// RowError(0, err) requires a real row at index 0 to be reachable —
	// sqlmock only invokes nextErr[N] when r.pos-1 == N and the row exists.
	// The driver copies row data into dest and THEN returns the error, so
	// database/sql's rows.Next() receives a non-EOF error, sets lasterr, and
	// returns false without ever calling Scan. rows.Err() then exposes lasterr.
	mock.ExpectQuery("SELECT id, workspace_id, workflow_id, step_name, step_index").
		WithArgs("ws-1", "wf-err").
		WillReturnRows(sqlmock.NewRows(cols).
			AddRow("ckpt-ok", "ws-1", "wf-err", "step-a", 0, "2026-04-17T10:00:00Z", nil).
			RowError(0, errors.New("storage engine fault")))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "wfid", Value: "wf-err"}}
	c.Request = httptest.NewRequest("GET", "/", nil)

	h.List(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("rows.Err() must yield 500, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ---------- Delete ----------

// TestCheckpointsDelete_Success verifies that DELETE returns 200 and the
// count of removed rows when checkpoints exist.
func TestCheckpointsDelete_Success(t *testing.T) {
	mock := setupTestDB(t)
	h := newCheckpointsHandler(t, mock)

	mock.ExpectExec("DELETE FROM workflow_checkpoints").
		WithArgs("ws-1", "wf-abc").
		WillReturnResult(sqlmock.NewResult(0, 3))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "wfid", Value: "wf-abc"}}
	c.Request = httptest.NewRequest("DELETE", "/", nil)

	h.Delete(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["workflow_id"] != "wf-abc" {
		t.Errorf("expected workflow_id 'wf-abc' in response, got %v", resp["workflow_id"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestCheckpointsDelete_NotFound verifies that DELETE returns 404 when no
// checkpoints exist for the workflow (clean-up of already-clean workflow).
func TestCheckpointsDelete_NotFound(t *testing.T) {
	mock := setupTestDB(t)
	h := newCheckpointsHandler(t, mock)

	mock.ExpectExec("DELETE FROM workflow_checkpoints").
		WithArgs("ws-1", "wf-gone").
		WillReturnResult(sqlmock.NewResult(0, 0))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "wfid", Value: "wf-gone"}}
	c.Request = httptest.NewRequest("DELETE", "/", nil)

	h.Delete(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for missing workflow, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ---------- Access control (caller_workspace_id mismatch → 403) ----------

// TestCheckpointsUpsert_CallerMismatch_Returns403 verifies that Upsert
// returns 403 when the Gin context carries a caller_workspace_id that does
// not match the URL :id param. This simulates the defence-in-depth check
// that future middleware (or tests) can activate by setting the context key.
func TestCheckpointsUpsert_CallerMismatch_Returns403(t *testing.T) {
	mock := setupTestDB(t)
	h := newCheckpointsHandler(t, mock)
	// No DB expectations — handler must abort before touching the DB.

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-target"}}
	c.Set("caller_workspace_id", "ws-attacker")
	body := `{"workflow_id":"wf-x","step_name":"step-x","step_index":0}`
	c.Request = httptest.NewRequest("POST", "/", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	h.Upsert(c)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 on workspace mismatch, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected DB calls after caller mismatch: %v", err)
	}
}

// TestCheckpointsList_CallerMismatch_Returns403 mirrors the Upsert test for
// the List endpoint.
func TestCheckpointsList_CallerMismatch_Returns403(t *testing.T) {
	mock := setupTestDB(t)
	h := newCheckpointsHandler(t, mock)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-target"}, {Key: "wfid", Value: "wf-x"}}
	c.Set("caller_workspace_id", "ws-attacker")
	c.Request = httptest.NewRequest("GET", "/", nil)

	h.List(c)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 on workspace mismatch, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected DB calls after caller mismatch: %v", err)
	}
}

// TestCheckpointsDelete_CallerMismatch_Returns403 mirrors the Upsert test for
// the Delete endpoint.
func TestCheckpointsDelete_CallerMismatch_Returns403(t *testing.T) {
	mock := setupTestDB(t)
	h := newCheckpointsHandler(t, mock)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-target"}, {Key: "wfid", Value: "wf-x"}}
	c.Set("caller_workspace_id", "ws-attacker")
	c.Request = httptest.NewRequest("DELETE", "/", nil)

	h.Delete(c)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 on workspace mismatch, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected DB calls after caller mismatch: %v", err)
	}
}

// ---------- Latest ----------

// TestCheckpointsLatest_ReturnsNewest verifies that Latest returns the most
// recently completed checkpoint (highest completed_at) for the workspace.
func TestCheckpointsLatest_ReturnsNewest(t *testing.T) {
	mock := setupTestDB(t)
	h := newCheckpointsHandler(t, mock)

	mock.ExpectQuery("SELECT id, workspace_id, workflow_id, step_name, step_index, completed_at, payload").
		WithArgs("ws-latest").
		WillReturnRows(
			sqlmock.NewRows([]string{
				"id", "workspace_id", "workflow_id",
				"step_name", "step_index", "completed_at", "payload",
			}).AddRow(
				"ckpt-abc", "ws-latest", "wf-123",
				"llm_call", 1, "2026-04-18T02:00:00Z", []byte(`{"success":true}`),
			),
		)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-latest"}}
	c.Request = httptest.NewRequest("GET", "/", nil)

	h.Latest(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}
	if resp["id"] != "ckpt-abc" {
		t.Errorf("expected id=ckpt-abc, got %v", resp["id"])
	}
	if resp["step_name"] != "llm_call" {
		t.Errorf("expected step_name=llm_call, got %v", resp["step_name"])
	}
	if resp["workflow_id"] != "wf-123" {
		t.Errorf("expected workflow_id=wf-123, got %v", resp["workflow_id"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestCheckpointsLatest_DBError_Returns500 verifies that Latest returns 500
// when the DB query itself fails (e.g., connection error, not a missing row).
func TestCheckpointsLatest_DBError_Returns500(t *testing.T) {
	mock := setupTestDB(t)
	h := newCheckpointsHandler(t, mock)

	mock.ExpectQuery("SELECT id, workspace_id, workflow_id, step_name, step_index, completed_at, payload").
		WithArgs("ws-err").
		WillReturnError(errors.New("db: connection refused"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-err"}}
	c.Request = httptest.NewRequest("GET", "/", nil)

	h.Latest(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 on DB error, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestCheckpointsLatest_ErrNoRows_Returns404 uses sql.ErrNoRows directly to
// verify the 404 branch is exercised.
func TestCheckpointsLatest_ErrNoRows_Returns404(t *testing.T) {
	mock := setupTestDB(t)
	h := newCheckpointsHandler(t, mock)

	mock.ExpectQuery("SELECT id, workspace_id, workflow_id, step_name, step_index, completed_at, payload").
		WithArgs("ws-none").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "workspace_id", "workflow_id",
			"step_name", "step_index", "completed_at", "payload",
		})) // empty result set → sql.ErrNoRows on Scan

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-none"}}
	c.Request = httptest.NewRequest("GET", "/", nil)

	h.Latest(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for empty result, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestCheckpointsLatest_CallerMismatch_Returns403 mirrors the Upsert test
// for the Latest endpoint.
func TestCheckpointsLatest_CallerMismatch_Returns403(t *testing.T) {
	mock := setupTestDB(t)
	h := newCheckpointsHandler(t, mock)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-target"}}
	c.Set("caller_workspace_id", "ws-attacker")
	c.Request = httptest.NewRequest("GET", "/", nil)

	h.Latest(c)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 on workspace mismatch, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected DB calls after caller mismatch: %v", err)
	}
}
