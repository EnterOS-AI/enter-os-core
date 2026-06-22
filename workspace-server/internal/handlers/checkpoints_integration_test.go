package handlers

// checkpoints_integration_test.go
//
// Integration-level tests for the Temporal checkpoint crash-resume system
// (issue #790). These scenarios test multi-step lifecycle flows, access
// control at the router level, and idempotent upsert semantics — distinct
// from checkpoints_test.go which focuses on single-handler correctness.
//
// All tests use sqlmock + httptest to stay in-process. Cascade-delete
// semantics are verified by simulating the post-cascade state (empty rows)
// because ON DELETE CASCADE is enforced by the DB schema, not app code.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/middleware"
	"github.com/gin-gonic/gin"
)

// checkpointCols is the column list returned by List queries.
var checkpointCols = []string{
	"id", "workspace_id", "workflow_id", "step_name", "step_index",
	"completed_at", "payload",
}

// upsertSQL is the pattern matched by sqlmock for the checkpoint upsert.
const upsertSQL = "INSERT INTO workflow_checkpoints"

// selectSQL is the pattern matched by sqlmock for the checkpoint list query.
const selectSQL = "SELECT id, workspace_id, workflow_id, step_name, step_index"

// ---------------------------------------------------------------------------
// Test 1 — Checkpoint persistence: all three Temporal stages stored & listed
// ---------------------------------------------------------------------------

// TestCheckpointsIntegration_ThreeStepPersistence verifies the full three-stage
// workflow lifecycle: POST task_receive (step 0) → POST llm_call (step 1) →
// POST task_complete (step 2) → GET returns all three in step_index DESC order.
//
// This mirrors what TemporalWorkflowWrapper calls in temporal_workflow.py
// after each of the three activity stages.
func TestCheckpointsIntegration_ThreeStepPersistence(t *testing.T) {
	mock := setupTestDB(t)
	h := newCheckpointsHandler(t, mock)

	stages := []struct {
		stepName  string
		stepIndex int
		id        string
		payload   string
	}{
		{"task_receive", 0, "ckpt-tr", `{"task_id":"t-1"}`},
		{"llm_call", 1, "ckpt-lc", `{"model":"claude-sonnet-4-5"}`},
		{"task_complete", 2, "ckpt-tc", `{"success":true}`},
	}

	// POST all three stages in order.
	for _, s := range stages {
		mock.ExpectQuery(upsertSQL).
			WithArgs("ws-1", "wf-temporal-001", s.stepName, s.stepIndex, s.payload).
			WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(s.id))

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
		body, _ := json.Marshal(map[string]interface{}{
			"workflow_id": "wf-temporal-001",
			"step_name":   s.stepName,
			"step_index":  s.stepIndex,
			"payload":     json.RawMessage(s.payload),
		})
		c.Request = httptest.NewRequest("POST", "/", bytes.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")

		h.Upsert(c)

		if w.Code != http.StatusCreated {
			t.Fatalf("stage %q: expected 201, got %d: %s", s.stepName, w.Code, w.Body.String())
		}
	}

	// GET — DB returns them in step_index DESC (task_complete first).
	mock.ExpectQuery(selectSQL).
		WithArgs("ws-1", "wf-temporal-001").
		WillReturnRows(sqlmock.NewRows(checkpointCols).
			AddRow("ckpt-tc", "ws-1", "wf-temporal-001", "task_complete", 2, "2026-04-17T10:02:00Z", []byte(`{"success":true}`)).
			AddRow("ckpt-lc", "ws-1", "wf-temporal-001", "llm_call", 1, "2026-04-17T10:01:00Z", []byte(`{"model":"claude-sonnet-4-5"}`)).
			AddRow("ckpt-tr", "ws-1", "wf-temporal-001", "task_receive", 0, "2026-04-17T10:00:00Z", []byte(`{"task_id":"t-1"}`)))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-1"},
		{Key: "wfid", Value: "wf-temporal-001"},
	}
	c.Request = httptest.NewRequest("GET", "/", nil)
	h.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("List: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var result []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("List: invalid JSON response: %v", err)
	}
	if len(result) != 3 {
		t.Fatalf("expected 3 checkpoints, got %d", len(result))
	}
	// Verify step_index DESC ordering (highest first).
	expectedOrder := []string{"task_complete", "llm_call", "task_receive"}
	for i, want := range expectedOrder {
		if got := result[i]["step_name"]; got != want {
			t.Errorf("result[%d].step_name: want %q, got %v", i, want, got)
		}
	}
	// Verify step_index values.
	for i, wantIdx := range []float64{2, 1, 0} {
		if got := result[i]["step_index"]; got != wantIdx {
			t.Errorf("result[%d].step_index: want %.0f, got %v", i, wantIdx, got)
		}
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Test 2 — Crash-and-resume: highest persisted step_index is the resume point
// ---------------------------------------------------------------------------

// TestCheckpointsIntegration_CrashResume_HighestStepIsResumptionPoint simulates
// a process crash after llm_call completes (step 1 persisted) but before
// task_complete runs (step 2 never persisted).
//
// On restart, the workflow queries its checkpoints: the highest step_index
// present is 1 (llm_call). The workflow can therefore skip task_receive
// and llm_call and resume from task_complete, avoiding duplicate LLM calls.
func TestCheckpointsIntegration_CrashResume_HighestStepIsResumptionPoint(t *testing.T) {
	mock := setupTestDB(t)
	h := newCheckpointsHandler(t, mock)

	// Two stages persisted before crash.
	for _, stage := range []struct {
		name  string
		idx   int
		id    string
	}{
		{"task_receive", 0, "ckpt-tr"},
		{"llm_call", 1, "ckpt-lc"},
	} {
		mock.ExpectQuery(upsertSQL).
			WithArgs("ws-crash", "wf-crash-001", stage.name, stage.idx, "null").
			WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(stage.id))

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Params = gin.Params{{Key: "id", Value: "ws-crash"}}
		body, _ := json.Marshal(map[string]interface{}{
			"workflow_id": "wf-crash-001",
			"step_name":   stage.name,
			"step_index":  stage.idx,
		})
		c.Request = httptest.NewRequest("POST", "/", bytes.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		h.Upsert(c)
		if w.Code != http.StatusCreated {
			t.Fatalf("stage %q: expected 201, got %d", stage.name, w.Code)
		}
	}

	// On restart: query checkpoints — DB returns step_index DESC.
	mock.ExpectQuery(selectSQL).
		WithArgs("ws-crash", "wf-crash-001").
		WillReturnRows(sqlmock.NewRows(checkpointCols).
			AddRow("ckpt-lc", "ws-crash", "wf-crash-001", "llm_call", 1, "2026-04-17T10:01:00Z", nil).
			AddRow("ckpt-tr", "ws-crash", "wf-crash-001", "task_receive", 0, "2026-04-17T10:00:00Z", nil))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-crash"},
		{Key: "wfid", Value: "wf-crash-001"},
	}
	c.Request = httptest.NewRequest("GET", "/", nil)
	h.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("List after crash: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var result []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 checkpoints (crash before step 2), got %d", len(result))
	}

	// The first element (highest step_index) is the resumption point.
	resumeStep := result[0]
	if resumeStep["step_name"] != "llm_call" {
		t.Errorf("resume point: want step_name 'llm_call', got %v", resumeStep["step_name"])
	}
	if resumeStep["step_index"] != float64(1) {
		t.Errorf("resume point: want step_index 1, got %v", resumeStep["step_index"])
	}

	// task_complete (step 2) must be absent.
	for _, cp := range result {
		if cp["step_name"] == "task_complete" {
			t.Error("task_complete should not be present — crash happened before that step")
		}
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Test 3 — Upsert idempotency: latest payload wins on repeated POST
// ---------------------------------------------------------------------------

// TestCheckpointsIntegration_UpsertIdempotency_LatestPayloadWins verifies
// that POSTing the same (workspace_id, workflow_id, step_name) triple a second
// time with a different payload replaces the stored payload (ON CONFLICT DO UPDATE).
//
// Concrete scenario: llm_call checkpoint is first saved with {"partial":true}
// then overwritten with {"partial":false,"tokens":512} when the activity
// retries with the full result.
func TestCheckpointsIntegration_UpsertIdempotency_LatestPayloadWins(t *testing.T) {
	mock := setupTestDB(t)
	h := newCheckpointsHandler(t, mock)

	const wsID = "ws-idem"
	const wfID = "wf-idem-001"
	const ckptID = "ckpt-idem"

	// First POST — partial result.
	firstPayload := `{"partial":true}`
	mock.ExpectQuery(upsertSQL).
		WithArgs(wsID, wfID, "llm_call", 1, firstPayload).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(ckptID))

	postCheckpoint(t, h, wsID, wfID, "llm_call", 1, firstPayload)

	// Second POST — full result overwrites via ON CONFLICT DO UPDATE.
	secondPayload := `{"partial":false,"tokens":512}`
	mock.ExpectQuery(upsertSQL).
		WithArgs(wsID, wfID, "llm_call", 1, secondPayload).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(ckptID)) // same ID after update

	postCheckpoint(t, h, wsID, wfID, "llm_call", 1, secondPayload)

	// GET — DB returns a single row with the updated payload.
	mock.ExpectQuery(selectSQL).
		WithArgs(wsID, wfID).
		WillReturnRows(sqlmock.NewRows(checkpointCols).
			AddRow(ckptID, wsID, wfID, "llm_call", 1, "2026-04-17T10:01:30Z",
				[]byte(secondPayload)))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}, {Key: "wfid", Value: wfID}}
	c.Request = httptest.NewRequest("GET", "/", nil)
	h.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("List: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var result []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 row (idempotent upsert), got %d", len(result))
	}

	// The stored payload must reflect the second POST.
	payloadRaw, _ := json.Marshal(result[0]["payload"])
	var payloadMap map[string]interface{}
	json.Unmarshal(payloadRaw, &payloadMap)
	if payloadMap["partial"] != false {
		t.Errorf("payload.partial: want false (updated), got %v", payloadMap["partial"])
	}
	if payloadMap["tokens"] != float64(512) {
		t.Errorf("payload.tokens: want 512, got %v", payloadMap["tokens"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Test 4 — Cascade delete: workspace deletion cascades to checkpoints
// ---------------------------------------------------------------------------

// TestCheckpointsIntegration_PostCascadeDelete_Returns404 verifies the
// application's behaviour after ON DELETE CASCADE removes all checkpoint rows
// when their parent workspace is deleted.
//
// The cascade is enforced by the DB schema:
//   workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE
//
// This test simulates the post-cascade state: the checkpoints query that runs
// after workspace deletion sees an empty result set and returns 404, exactly
// as it would if the workspace had never had checkpoints.
func TestCheckpointsIntegration_PostCascadeDelete_Returns404(t *testing.T) {
	mock := setupTestDB(t)
	h := newCheckpointsHandler(t, mock)

	const wsID = "ws-cascade"
	const wfID = "wf-cascade-001"

	// Pre-crash: two checkpoints were persisted.
	for _, stage := range []struct{ name string; idx int; id string }{
		{"task_receive", 0, "ckpt-tr"},
		{"llm_call", 1, "ckpt-lc"},
	} {
		mock.ExpectQuery(upsertSQL).
			WithArgs(wsID, wfID, stage.name, stage.idx, "null").
			WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(stage.id))
		postCheckpointNoPayload(t, h, wsID, wfID, stage.name, stage.idx)
	}

	// Workspace is deleted (ON DELETE CASCADE fires, checkpoints are gone).
	// Simulate post-cascade state: List returns empty rows → handler returns 404.
	mock.ExpectQuery(selectSQL).
		WithArgs(wsID, wfID).
		WillReturnRows(sqlmock.NewRows(checkpointCols)) // empty — cascade deleted them

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}, {Key: "wfid", Value: wfID}}
	c.Request = httptest.NewRequest("GET", "/", nil)
	h.List(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("post-cascade List: want 404 (no rows), got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Test 5 — Auth gate: WorkspaceAuth middleware rejects requests without a token
// ---------------------------------------------------------------------------

// TestCheckpointsIntegration_AuthGate_NoToken_Returns401 tests the checkpoint
// endpoints through a full Gin router with the WorkspaceAuth middleware applied.
// Every request lacking a valid Authorization: Bearer token must receive 401.
//
// This pins the security contract established by #351 / Phase 30.1:
// no grace period, no fail-open, no existence check before token validation.
func TestCheckpointsIntegration_AuthGate_NoToken_Returns401(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer mockDB.Close()

	// No DB expectations — strict WorkspaceAuth path short-circuits before
	// any handler (and therefore before any DB call) when the bearer is absent.

	r := gin.New()
	wsGroup := r.Group("/workspaces/:id")
	wsGroup.Use(middleware.WorkspaceAuth(mockDB))
	{
		// Handler uses mockDB too; WorkspaceAuth 401s before the handler runs,
		// so the DB is never queried — any valid *sql.DB pointer works here.
		cpth := NewCheckpointsHandler(mockDB)
		wsGroup.POST("/checkpoints", cpth.Upsert)
		wsGroup.GET("/checkpoints/:wfid", cpth.List)
		wsGroup.DELETE("/checkpoints/:wfid", cpth.Delete)
	}

	cases := []struct {
		method string
		path   string
		body   string
	}{
		{
			"POST",
			"/workspaces/ws-secure/checkpoints",
			`{"workflow_id":"wf-1","step_name":"task_receive","step_index":0}`,
		},
		{
			"GET",
			"/workspaces/ws-secure/checkpoints/wf-1",
			"",
		},
		{
			"DELETE",
			"/workspaces/ws-secure/checkpoints/wf-1",
			"",
		},
	}

	for _, tc := range cases {
		t.Run(tc.method, func(t *testing.T) {
			var bodyReader *bytes.Reader
			if tc.body != "" {
				bodyReader = bytes.NewReader([]byte(tc.body))
			} else {
				bodyReader = bytes.NewReader(nil)
			}

			req, _ := http.NewRequest(tc.method, tc.path, bodyReader)
			if tc.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			// Deliberately no Authorization header.

			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Errorf("%s %s without token: want 401, got %d: %s",
					tc.method, tc.path, w.Code, w.Body.String())
			}
		})
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected DB calls during no-token requests: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// postCheckpoint is a test helper that POSTs a checkpoint with a raw JSON
// payload string and asserts a 201 response.
func postCheckpoint(t *testing.T, h *CheckpointsHandler, wsID, wfID, stepName string, stepIndex int, rawPayload string) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	body, _ := json.Marshal(map[string]interface{}{
		"workflow_id": wfID,
		"step_name":   stepName,
		"step_index":  stepIndex,
		"payload":     json.RawMessage(rawPayload),
	})
	c.Request = httptest.NewRequest("POST", "/", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.Upsert(c)
	if w.Code != http.StatusCreated {
		t.Fatalf("postCheckpoint %q: expected 201, got %d: %s", stepName, w.Code, w.Body.String())
	}
}

// postCheckpointNoPayload is a test helper that POSTs a checkpoint without
// a payload field (stored as JSON null in the DB).
func postCheckpointNoPayload(t *testing.T, h *CheckpointsHandler, wsID, wfID, stepName string, stepIndex int) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	body, _ := json.Marshal(map[string]interface{}{
		"workflow_id": wfID,
		"step_name":   stepName,
		"step_index":  stepIndex,
	})
	c.Request = httptest.NewRequest("POST", "/", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.Upsert(c)
	if w.Code != http.StatusCreated {
		t.Fatalf("postCheckpointNoPayload %q: expected 201, got %d: %s", stepName, w.Code, w.Body.String())
	}
}
