package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// usageColumns matches the SELECT in GetMetrics.
var usageColumns = []string{
	"sum_input_tokens", "sum_output_tokens", "sum_call_count", "sum_cost",
}

// expectWorkspaceExistsMetrics queues the EXISTS check in GetMetrics.
func expectWorkspaceExistsMetrics(mock sqlmock.Sqlmock, workspaceID string, exists bool) {
	mock.ExpectQuery(`SELECT EXISTS`).
		WithArgs(workspaceID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(exists))
}

// TestGetMetrics_HappyPath verifies the handler returns correct aggregated data.
func TestGetMetrics_HappyPath(t *testing.T) {
	mock := setupTestDB(t)

	expectWorkspaceExistsMetrics(mock, "ws-1", true)

	// Simulate one row with usage data.
	mock.ExpectQuery(`SELECT\s+COALESCE\(SUM\(input_tokens\)`).
		WithArgs("ws-1", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows(usageColumns).
			AddRow(int64(1500), int64(300), int64(5), float64(0.009)))

	h := NewMetricsHandler()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-1/metrics", nil)

	h.GetMetrics(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		InputTokens      int64  `json:"input_tokens"`
		OutputTokens     int64  `json:"output_tokens"`
		TotalCalls       int64  `json:"total_calls"`
		EstimatedCost    string `json:"estimated_cost_usd"`
		PeriodStart      string `json:"period_start"`
		PeriodEnd        string `json:"period_end"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, w.Body.String())
	}

	if resp.InputTokens != 1500 {
		t.Errorf("expected input_tokens=1500, got %d", resp.InputTokens)
	}
	if resp.OutputTokens != 300 {
		t.Errorf("expected output_tokens=300, got %d", resp.OutputTokens)
	}
	if resp.TotalCalls != 5 {
		t.Errorf("expected total_calls=5, got %d", resp.TotalCalls)
	}
	if resp.EstimatedCost == "" {
		t.Error("expected non-empty estimated_cost_usd")
	}
	if resp.PeriodStart == "" {
		t.Error("expected non-empty period_start")
	}
	if resp.PeriodEnd == "" {
		t.Error("expected non-empty period_end")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet DB expectations: %v", err)
	}
}

// TestGetMetrics_WorkspaceNotFound verifies a 404 when workspace is absent.
func TestGetMetrics_WorkspaceNotFound(t *testing.T) {
	mock := setupTestDB(t)
	expectWorkspaceExistsMetrics(mock, "ghost", false)

	h := NewMetricsHandler()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ghost"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ghost/metrics", nil)

	h.GetMetrics(c)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet DB expectations: %v", err)
	}
}

// TestGetMetrics_EmptyPeriod verifies the handler returns zeros when no usage exists yet.
func TestGetMetrics_EmptyPeriod(t *testing.T) {
	mock := setupTestDB(t)
	expectWorkspaceExistsMetrics(mock, "ws-new", true)

	// COALESCE returns 0 for each column when no rows match.
	mock.ExpectQuery(`SELECT\s+COALESCE\(SUM\(input_tokens\)`).
		WithArgs("ws-new", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows(usageColumns).
			AddRow(int64(0), int64(0), int64(0), float64(0)))

	h := NewMetricsHandler()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-new"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-new/metrics", nil)

	h.GetMetrics(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	// Verify period_start and period_end are present and distinct.
	ps, _ := resp["period_start"].(string)
	pe, _ := resp["period_end"].(string)
	if ps == "" || pe == "" {
		t.Errorf("expected non-empty period_start/period_end, got %q / %q", ps, pe)
	}
	if ps == pe {
		t.Errorf("period_start and period_end must differ, both are %q", ps)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet DB expectations: %v", err)
	}
}

// TestGetMetrics_CostFormat verifies estimated_cost_usd is formatted to 6 decimal places.
func TestGetMetrics_CostFormat(t *testing.T) {
	mock := setupTestDB(t)
	expectWorkspaceExistsMetrics(mock, "ws-1", true)

	mock.ExpectQuery(`SELECT\s+COALESCE\(SUM\(input_tokens\)`).
		WithArgs("ws-1", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows(usageColumns).
			AddRow(int64(1000000), int64(0), int64(1), float64(3.0)))

	h := NewMetricsHandler()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-1/metrics", nil)

	h.GetMetrics(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	cost, _ := resp["estimated_cost_usd"].(string)
	if len(cost) < 8 {
		// "3.000000" is 8 chars minimum
		t.Errorf("expected at least 8-char cost string, got %q", cost)
	}
}

// ---- upsertTokenUsage cap tests (#615) ----

// TestUpsertTokenUsage_615_CapsInt64Max verifies that an adversarial
// INT64_MAX token count is clamped to maxTokensPerCall before the upsert,
// preventing NUMERIC(12,6) overflow in Postgres.
func TestUpsertTokenUsage_615_CapsInt64Max(t *testing.T) {
	mock := setupTestDB(t)

	// We expect the INSERT to be called with maxTokensPerCall, not math.MaxInt64.
	mock.ExpectExec(`INSERT INTO workspace_token_usage`).
		WithArgs("ws-1", sqlmock.AnyArg(),
			maxTokensPerCall,  // input clamped
			maxTokensPerCall,  // output clamped
			sqlmock.AnyArg()). // cost
		WillReturnResult(sqlmock.NewResult(0, 1))

	// INT64_MAX overflows — must be clamped.
	const int64Max = int64(^uint64(0) >> 1)
	upsertTokenUsage(t.Context(), "ws-1", int64Max, int64Max)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expected clamped values in upsert: %v", err)
	}
}

// TestUpsertTokenUsage_615_CapsNegative verifies negative token counts are
// clamped to 0 before upsert (no negative accumulation in cost rows).
func TestUpsertTokenUsage_615_CapsNegative(t *testing.T) {
	// Negative input + negative output → both become 0 → early return, no DB call.
	setupTestDB(t) // no expectations

	upsertTokenUsage(t.Context(), "ws-1", -100, -200)
	// If any DB call were made the mock would error — passing here is the assertion.
}

// TestUpsertTokenUsage_615_NormalValuesUnchanged verifies that token counts
// within the valid range pass through to the DB unchanged.
func TestUpsertTokenUsage_615_NormalValuesUnchanged(t *testing.T) {
	mock := setupTestDB(t)

	mock.ExpectExec(`INSERT INTO workspace_token_usage`).
		WithArgs("ws-1", sqlmock.AnyArg(),
			int64(1500),      // input unchanged
			int64(300),       // output unchanged
			sqlmock.AnyArg()). // cost
		WillReturnResult(sqlmock.NewResult(0, 1))

	upsertTokenUsage(t.Context(), "ws-1", 1500, 300)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("normal values altered unexpectedly: %v", err)
	}
}

// TestUpsertTokenUsage_615_ExactlyAtCap verifies that a count exactly equal
// to maxTokensPerCall is accepted without clamping.
func TestUpsertTokenUsage_615_ExactlyAtCap(t *testing.T) {
	mock := setupTestDB(t)

	mock.ExpectExec(`INSERT INTO workspace_token_usage`).
		WithArgs("ws-1", sqlmock.AnyArg(),
			maxTokensPerCall,
			maxTokensPerCall,
			sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	upsertTokenUsage(t.Context(), "ws-1", maxTokensPerCall, maxTokensPerCall)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("at-cap values should not be altered: %v", err)
	}
}

// ---- parseUsageFromA2AResponse tests ----

func TestParseUsage_JSONRPCResultEnvelope(t *testing.T) {
	body := []byte(`{
		"jsonrpc": "2.0",
		"id": "abc",
		"result": {
			"usage": {
				"input_tokens": 100,
				"output_tokens": 50
			}
		}
	}`)
	in, out := parseUsageFromA2AResponse(body)
	if in != 100 {
		t.Errorf("expected input_tokens=100, got %d", in)
	}
	if out != 50 {
		t.Errorf("expected output_tokens=50, got %d", out)
	}
}

func TestParseUsage_TopLevelUsage(t *testing.T) {
	body := []byte(`{
		"usage": {
			"input_tokens": 200,
			"output_tokens": 75
		}
	}`)
	in, out := parseUsageFromA2AResponse(body)
	if in != 200 {
		t.Errorf("expected input_tokens=200, got %d", in)
	}
	if out != 75 {
		t.Errorf("expected output_tokens=75, got %d", out)
	}
}

func TestParseUsage_NoUsageField(t *testing.T) {
	body := []byte(`{"jsonrpc":"2.0","id":"x","result":{"message":"hello"}}`)
	in, out := parseUsageFromA2AResponse(body)
	if in != 0 || out != 0 {
		t.Errorf("expected (0, 0) with no usage field, got (%d, %d)", in, out)
	}
}

func TestParseUsage_ZeroTokensIgnored(t *testing.T) {
	body := []byte(`{"result":{"usage":{"input_tokens":0,"output_tokens":0}}}`)
	in, out := parseUsageFromA2AResponse(body)
	if in != 0 || out != 0 {
		t.Errorf("expected (0, 0) for zero tokens, got (%d, %d)", in, out)
	}
}

func TestParseUsage_EmptyBody(t *testing.T) {
	in, out := parseUsageFromA2AResponse([]byte{})
	if in != 0 || out != 0 {
		t.Errorf("expected (0, 0) for empty body, got (%d, %d)", in, out)
	}
}

func TestParseUsage_InvalidJSON(t *testing.T) {
	in, out := parseUsageFromA2AResponse([]byte("not json"))
	if in != 0 || out != 0 {
		t.Errorf("expected (0, 0) for invalid JSON, got (%d, %d)", in, out)
	}
}

func TestParseUsage_NestedResultPreferredOverTopLevel(t *testing.T) {
	// result.usage should be preferred over top-level usage.
	body := []byte(`{
		"usage": {"input_tokens": 999, "output_tokens": 999},
		"result": {
			"usage": {"input_tokens": 42, "output_tokens": 21}
		}
	}`)
	in, out := parseUsageFromA2AResponse(body)
	if in != 42 {
		t.Errorf("expected result.usage.input_tokens=42, got %d", in)
	}
	if out != 21 {
		t.Errorf("expected result.usage.output_tokens=21, got %d", out)
	}
}
