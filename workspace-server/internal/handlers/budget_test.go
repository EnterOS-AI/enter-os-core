package handlers

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// Multi-period budget (#49): GET/PATCH now read workspaces.budget_limits (jsonb)
// and compute per-period spend from the workspace_spend_events ledger
// (spendByPeriod — matched here by the "FROM workspace_spend_events" fragment).
// The legacy budget_limit/monthly_spend response fields are still emitted
// (monthly period) for rollout back-compat, and the legacy {"budget_limit":N}
// PATCH shape still works.

// spendRows builds the 4-column row spendByPeriod scans (hourly,daily,weekly,monthly).
func spendRows(h, d, w, m int64) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"h", "d", "w", "mo"}).AddRow(h, d, w, m)
}

// ==================== GET /workspaces/:id/budget ====================

func TestBudgetGet_NotFound(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	mock.ExpectQuery(`SELECT COALESCE\(budget_limits`).
		WithArgs("ws-not-there").
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-not-there"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-not-there/budget", nil)

	NewBudgetHandler().GetBudget(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

func TestBudgetGet_DBError(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	mock.ExpectQuery(`SELECT COALESCE\(budget_limits`).
		WithArgs("ws-db-err").
		WillReturnError(sql.ErrConnDone)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-db-err"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-db-err/budget", nil)

	NewBudgetHandler().GetBudget(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

func TestBudgetGet_NoLimit(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	mock.ExpectQuery(`SELECT COALESCE\(budget_limits`).
		WithArgs("ws-free").
		WillReturnRows(sqlmock.NewRows([]string{"budget_limits"}).AddRow([]byte(`{}`)))
	mock.ExpectQuery(`FROM workspace_spend_events`).
		WithArgs("ws-free").
		WillReturnRows(spendRows(0, 0, 0, 42))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-free"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-free/budget", nil)

	NewBudgetHandler().GetBudget(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if resp["budget_limit"] != nil {
		t.Errorf("expected budget_limit=null, got %v", resp["budget_limit"])
	}
	if resp["budget_remaining"] != nil {
		t.Errorf("expected budget_remaining=null, got %v", resp["budget_remaining"])
	}
	if resp["monthly_spend"] != float64(42) {
		t.Errorf("expected monthly_spend=42, got %v", resp["monthly_spend"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

func TestBudgetGet_WithLimit(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	mock.ExpectQuery(`SELECT COALESCE\(budget_limits`).
		WithArgs("ws-capped").
		WillReturnRows(sqlmock.NewRows([]string{"budget_limits"}).AddRow([]byte(`{"monthly":500}`)))
	mock.ExpectQuery(`FROM workspace_spend_events`).
		WithArgs("ws-capped").
		WillReturnRows(spendRows(0, 0, 0, 123))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-capped"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-capped/budget", nil)

	NewBudgetHandler().GetBudget(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if resp["budget_limit"] != float64(500) {
		t.Errorf("expected budget_limit=500, got %v", resp["budget_limit"])
	}
	if resp["monthly_spend"] != float64(123) {
		t.Errorf("expected monthly_spend=123, got %v", resp["monthly_spend"])
	}
	if resp["budget_remaining"] != float64(377) {
		t.Errorf("expected budget_remaining=377, got %v", resp["budget_remaining"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

func TestBudgetGet_OverBudget(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	mock.ExpectQuery(`SELECT COALESCE\(budget_limits`).
		WithArgs("ws-over").
		WillReturnRows(sqlmock.NewRows([]string{"budget_limits"}).AddRow([]byte(`{"monthly":100}`)))
	mock.ExpectQuery(`FROM workspace_spend_events`).
		WithArgs("ws-over").
		WillReturnRows(spendRows(0, 0, 0, 150))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-over"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-over/budget", nil)

	NewBudgetHandler().GetBudget(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if resp["budget_remaining"] != float64(-50) {
		t.Errorf("expected budget_remaining=-50, got %v", resp["budget_remaining"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

// TestBudgetGet_MultiPeriod pins the new per-period shape: each period reports
// its own limit/spend/remaining, and an over-budget sub-period is visible.
func TestBudgetGet_MultiPeriod(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	mock.ExpectQuery(`SELECT COALESCE\(budget_limits`).
		WithArgs("ws-mp").
		WillReturnRows(sqlmock.NewRows([]string{"budget_limits"}).
			AddRow([]byte(`{"hourly":100,"daily":1000}`)))
	mock.ExpectQuery(`FROM workspace_spend_events`).
		WithArgs("ws-mp").
		WillReturnRows(spendRows(120, 300, 300, 300)) // hourly over (120>=100)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-mp"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-mp/budget", nil)

	NewBudgetHandler().GetBudget(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Periods map[string]struct {
			Limit     *int64 `json:"limit"`
			Spend     int64  `json:"spend"`
			Remaining *int64 `json:"remaining"`
		} `json:"periods"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if resp.Periods["hourly"].Limit == nil || *resp.Periods["hourly"].Limit != 100 {
		t.Errorf("hourly.limit: want 100, got %v", resp.Periods["hourly"].Limit)
	}
	if resp.Periods["hourly"].Spend != 120 {
		t.Errorf("hourly.spend: want 120, got %d", resp.Periods["hourly"].Spend)
	}
	if r := resp.Periods["hourly"].Remaining; r == nil || *r != -20 {
		t.Errorf("hourly.remaining: want -20, got %v", r)
	}
	if resp.Periods["weekly"].Limit != nil {
		t.Errorf("weekly.limit: want null (unset), got %v", resp.Periods["weekly"].Limit)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

// ==================== PATCH /workspaces/:id/budget ====================

func TestBudgetPatch_MissingField(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-patch-missing"}}
	c.Request = httptest.NewRequest("PATCH", "/workspaces/ws-patch-missing/budget",
		bytes.NewBufferString(`{"other_field":123}`))
	c.Request.Header.Set("Content-Type", "application/json")

	NewBudgetHandler().PatchBudget(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestBudgetPatch_InvalidBody(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-patch-bad"}}
	c.Request = httptest.NewRequest("PATCH", "/workspaces/ws-patch-bad/budget",
		bytes.NewBufferString(`not json`))
	c.Request.Header.Set("Content-Type", "application/json")

	NewBudgetHandler().PatchBudget(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestBudgetPatch_NegativeValue(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-negative"}}
	c.Request = httptest.NewRequest("PATCH", "/workspaces/ws-negative/budget",
		bytes.NewBufferString(`{"budget_limit":-1}`))
	c.Request.Header.Set("Content-Type", "application/json")

	NewBudgetHandler().PatchBudget(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for negative budget_limit, got %d: %s", w.Code, w.Body.String())
	}
}

func TestBudgetPatch_InvalidType(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-badtype"}}
	c.Request = httptest.NewRequest("PATCH", "/workspaces/ws-badtype/budget",
		bytes.NewBufferString(`{"budget_limit":"not-a-number"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	NewBudgetHandler().PatchBudget(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for string budget_limit, got %d: %s", w.Code, w.Body.String())
	}
}

// TestBudgetPatch_UnknownPeriod rejects an unsupported period key.
func TestBudgetPatch_UnknownPeriod(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-badperiod"}}
	c.Request = httptest.NewRequest("PATCH", "/workspaces/ws-badperiod/budget",
		bytes.NewBufferString(`{"budget_limits":{"yearly":100}}`))
	c.Request.Header.Set("Content-Type", "application/json")

	NewBudgetHandler().PatchBudget(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for unknown period, got %d: %s", w.Code, w.Body.String())
	}
}

func TestBudgetPatch_WorkspaceNotFound(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	mock.ExpectQuery(`SELECT EXISTS.*status != 'removed'`).
		WithArgs("ws-no-exist").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-no-exist"}}
	c.Request = httptest.NewRequest("PATCH", "/workspaces/ws-no-exist/budget",
		bytes.NewBufferString(`{"budget_limit":500}`))
	c.Request.Header.Set("Content-Type", "application/json")

	NewBudgetHandler().PatchBudget(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

// TestBudgetPatch_SetLimit (legacy monthly shape) updates + returns new state.
func TestBudgetPatch_SetLimit(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	mock.ExpectQuery(`SELECT EXISTS.*status != 'removed'`).
		WithArgs("ws-set-limit").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectExec(`UPDATE workspaces SET budget_limits`).
		WithArgs("ws-set-limit", sqlmock.AnyArg(), int64(500)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`FROM workspace_spend_events`).
		WithArgs("ws-set-limit").
		WillReturnRows(spendRows(0, 0, 0, 200))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-set-limit"}}
	c.Request = httptest.NewRequest("PATCH", "/workspaces/ws-set-limit/budget",
		bytes.NewBufferString(`{"budget_limit":500}`))
	c.Request.Header.Set("Content-Type", "application/json")

	NewBudgetHandler().PatchBudget(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if resp["budget_limit"] != float64(500) {
		t.Errorf("expected budget_limit=500, got %v", resp["budget_limit"])
	}
	if resp["monthly_spend"] != float64(200) {
		t.Errorf("expected monthly_spend=200, got %v", resp["monthly_spend"])
	}
	if resp["budget_remaining"] != float64(300) {
		t.Errorf("expected budget_remaining=300, got %v", resp["budget_remaining"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

// TestBudgetPatch_SetMultiPeriod sets several periods at once and verifies the
// per-period response.
func TestBudgetPatch_SetMultiPeriod(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	mock.ExpectQuery(`SELECT EXISTS.*status != 'removed'`).
		WithArgs("ws-mp-set").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	// no monthly in payload → legacy budget_limit column set to NULL
	mock.ExpectExec(`UPDATE workspaces SET budget_limits`).
		WithArgs("ws-mp-set", sqlmock.AnyArg(), nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`FROM workspace_spend_events`).
		WithArgs("ws-mp-set").
		WillReturnRows(spendRows(10, 20, 30, 40))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-mp-set"}}
	c.Request = httptest.NewRequest("PATCH", "/workspaces/ws-mp-set/budget",
		bytes.NewBufferString(`{"budget_limits":{"hourly":100,"daily":200,"monthly":null}}`))
	c.Request.Header.Set("Content-Type", "application/json")

	NewBudgetHandler().PatchBudget(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Periods map[string]struct {
			Limit *int64 `json:"limit"`
			Spend int64  `json:"spend"`
		} `json:"periods"`
		BudgetLimit *int64 `json:"budget_limit"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if resp.Periods["hourly"].Limit == nil || *resp.Periods["hourly"].Limit != 100 {
		t.Errorf("hourly.limit want 100, got %v", resp.Periods["hourly"].Limit)
	}
	if resp.Periods["daily"].Limit == nil || *resp.Periods["daily"].Limit != 200 {
		t.Errorf("daily.limit want 200, got %v", resp.Periods["daily"].Limit)
	}
	if resp.BudgetLimit != nil {
		t.Errorf("monthly cleared → budget_limit should be null, got %v", *resp.BudgetLimit)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

func TestBudgetPatch_ClearLimit(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	mock.ExpectQuery(`SELECT EXISTS.*status != 'removed'`).
		WithArgs("ws-clear-limit").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectExec(`UPDATE workspaces SET budget_limits`).
		WithArgs("ws-clear-limit", sqlmock.AnyArg(), nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`FROM workspace_spend_events`).
		WithArgs("ws-clear-limit").
		WillReturnRows(spendRows(0, 0, 0, 50))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-clear-limit"}}
	c.Request = httptest.NewRequest("PATCH", "/workspaces/ws-clear-limit/budget",
		bytes.NewBufferString(`{"budget_limit":null}`))
	c.Request.Header.Set("Content-Type", "application/json")

	NewBudgetHandler().PatchBudget(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if resp["budget_limit"] != nil {
		t.Errorf("expected budget_limit=null after clear, got %v", resp["budget_limit"])
	}
	if resp["budget_remaining"] != nil {
		t.Errorf("expected budget_remaining=null after clear, got %v", resp["budget_remaining"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

func TestBudgetPatch_UpdateDBError(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	mock.ExpectQuery(`SELECT EXISTS.*status != 'removed'`).
		WithArgs("ws-patch-dberr").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectExec(`UPDATE workspaces SET budget_limits`).
		WithArgs("ws-patch-dberr", sqlmock.AnyArg(), int64(500)).
		WillReturnError(sql.ErrConnDone)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-patch-dberr"}}
	c.Request = httptest.NewRequest("PATCH", "/workspaces/ws-patch-dberr/budget",
		bytes.NewBufferString(`{"budget_limit":500}`))
	c.Request.Header.Set("Content-Type", "application/json")

	NewBudgetHandler().PatchBudget(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 on UPDATE error, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

// TestBudgetPatch_ZeroLimit verifies budget_limit=0 is accepted + stored (0 =
// block-all: every period call is blocked — pauses the workspace's spend).
func TestBudgetPatch_ZeroLimit(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	mock.ExpectQuery(`SELECT EXISTS.*status != 'removed'`).
		WithArgs("ws-zero-limit").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectExec(`UPDATE workspaces SET budget_limits`).
		WithArgs("ws-zero-limit", sqlmock.AnyArg(), int64(0)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`FROM workspace_spend_events`).
		WithArgs("ws-zero-limit").
		WillReturnRows(spendRows(0, 0, 0, 0))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-zero-limit"}}
	c.Request = httptest.NewRequest("PATCH", "/workspaces/ws-zero-limit/budget",
		bytes.NewBufferString(`{"budget_limit":0}`))
	c.Request.Header.Set("Content-Type", "application/json")

	NewBudgetHandler().PatchBudget(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for zero budget_limit, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if resp["budget_limit"] != float64(0) {
		t.Errorf("expected budget_limit=0 (block-all), got %v", resp["budget_limit"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}
