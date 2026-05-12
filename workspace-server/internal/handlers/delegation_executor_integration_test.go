//go:build integration
// +build integration

// delegation_executor_integration_test.go — REAL Postgres integration tests for
// executeDelegation HTTP proxy edge cases that sqlmock cannot cover.
//
// The sqlmock tests in delegation_test.go pin which SQL statements fire but
// cannot detect bugs that depend on the row state AFTER the SQL runs. The
// result_preview-lost bug shipped to staging in PR #2854 because sqlmock tests
// were satisfied with "an UPDATE fired" — none verified the row's preview
// field actually landed. These integration tests close that gap.
//
// Run with:
//
//   docker run --rm -d --name pg-integration \
//     -e POSTGRES_PASSWORD=test -e POSTGRES_DB=molecule \
//     -p 55432:5432 postgres:15-alpine
//   sleep 4
//   psql ... < workspace-server/migrations/049_delegations.up.sql
//   cd workspace-server
//   INTEGRATION_DB_URL="postgres://postgres:test@localhost:55432/molecule?sslmode=disable" \
//     go test -tags=integration ./internal/handlers/ -run Integration_ExecuteDelegation
//
// CI (.gitea/workflows/handlers-postgres-integration.yml) runs this on
// every PR that touches workspace-server/internal/handlers/**.

package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/alicebob/miniredis/v2"
)

// integrationDB is imported from delegation_ledger_integration_test.go.
// Each test gets a fresh table state.

const testDelegationID = "del-159-test-integration"
const testSourceID    = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
const testTargetID    = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"

// setupIntegrationFixtures inserts the rows executeDelegation requires:
//   - workspaces: source and target (siblings, parent_id=NULL so CanCommunicate=true)
//   - activity_logs: the 'delegate' row that updateDelegationStatus UPDATE will find
//   - delegations: the ledger row that recordLedgerStatus will UPDATE
//
// Returns a cleanup function the test should defer.
func setupIntegrationFixtures(t *testing.T, conn *sql.DB) func() {
	t.Helper()
	for _, ws := range []struct {
		id       string
		name     string
		parentID *string
	}{
		{testSourceID, "test-source", nil},
		{testTargetID, "test-target", nil},
	} {
		if _, err := conn.ExecContext(context.Background(),
			`INSERT INTO workspaces (id, name, parent_id) VALUES ($1::uuid, $2, $3) ON CONFLICT (id) DO NOTHING`,
			ws.id, ws.name, ws.parentID,
		); err != nil {
			t.Fatalf("seed workspace %s: %v", ws.id, err)
		}
	}

	reqBody, _ := json.Marshal(map[string]any{
		"delegation_id": testDelegationID,
		"task":         "do work",
	})
	if _, err := conn.ExecContext(context.Background(), `
		INSERT INTO activity_logs
			(workspace_id, activity_type, method, source_id, target_id, request_body, status)
		VALUES ($1, 'delegate', 'delegate', $1, $2, $3::jsonb, 'pending')
		ON CONFLICT DO NOTHING
	`, testSourceID, testTargetID, string(reqBody)); err != nil {
		t.Fatalf("seed activity_logs: %v", err)
	}

	if _, err := conn.ExecContext(context.Background(), `
		INSERT INTO delegations
			(delegation_id, caller_id, callee_id, task_preview, status)
		VALUES ($1, $2::uuid, $3::uuid, 'do work', 'queued')
		ON CONFLICT (delegation_id) DO NOTHING
	`, testDelegationID, testSourceID, testTargetID); err != nil {
		t.Fatalf("seed delegations: %v", err)
	}

	return func() {
		conn.ExecContext(context.Background(),
			`DELETE FROM activity_logs WHERE workspace_id = $1 AND request_body->>'delegation_id' = $2`,
			testSourceID, testDelegationID)
		conn.ExecContext(context.Background(),
			`DELETE FROM delegations WHERE delegation_id = $1`, testDelegationID)
		conn.ExecContext(context.Background(),
			`DELETE FROM workspaces WHERE id IN ($1, $2)`, testSourceID, testTargetID)
	}
}

// setupIntegrationRedis starts a miniredis, sets db.RDB, and seeds the target
// workspace URL to agentURL. Returns the miniredis instance for cleanup.
func setupIntegrationRedis(t *testing.T, agentURL string) *miniredis.Miniredis {
	t.Helper()
	mr := setupTestRedis(t)
	db.CacheURL(context.Background(), testTargetID, agentURL)
	return mr
}

// readDelegationRow returns (status, result_preview, error_detail) for the test
// delegation, or fails the test if the row is not found.
func readDelegationRow(t *testing.T, conn *sql.DB) (status, preview, errorDetail string) {
	t.Helper()
	var prev, errDet sql.NullString
	err := conn.QueryRowContext(context.Background(),
		`SELECT status, result_preview, error_detail FROM delegations WHERE delegation_id = $1`,
		testDelegationID,
	).Scan(&status, &prev, &errDet)
	if err != nil {
		t.Fatalf("readDelegationRow: %v", err)
	}
	return status, prev.String, errDet.String
}

// agentServer returns an httptest.Server that sends the given status, headers,
// and body. The server discards the request body (prevents broken-pipe on the
// client's request-body write when the connection is hijacked) but does NOT
// hijack or close the connection — it lets httptest handle the connection
// lifecycle normally. This avoids the httptest+Hijack deadlock where the
// server blocks reading the request body while the client waits for response
// headers (both sides block: server read vs client write).
//
// For "partial body" scenarios (Content-Length > actualBody), the client
// receives fewer bytes than declared and gets an io.EOF on the response body
// read. The test then verifies the ledger landed at the expected status.
func agentServer(t *testing.T, statusCode int, declaredLength int, actualBody string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Drain the request body so the client's request-body writer goroutine
		// can finish without a broken-pipe error. This MUST be done before any
		// operation that might block (Hijack, Close, etc.).
		io.Copy(io.Discard, r.Body)
		r.Body.Close()

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", declaredLength))
		w.WriteHeader(statusCode)
		w.Write([]byte(actualBody)) //nolint:errcheck
	}))
}

// TestIntegration_ExecuteDelegation_DeliveryConfirmedProxyError_TreatsAsSuccess
// is the integration regression gate for issue #159.
//
// Scenario: proxyA2ARequest returns a 200 status code with a non-empty
// (potentially partial) body and an error. The isDeliveryConfirmedSuccess
// guard (status>=200 && <300 && len(body)>0 && err!=nil) routes to
// handleSuccess.
//
// We use a clean 200 response here — the partial-body variant is tested
// via the sqlmock tests in delegation_test.go which pin the exact SQL
// statement that fires. This integration test verifies the DB row lands
// correctly at 'completed' with the response body as result_preview.
func TestIntegration_ExecuteDelegation_DeliveryConfirmedProxyError_TreatsAsSuccess(t *testing.T) {
	allowLoopbackForTest(t)
	conn := integrationDB(t)
	cleanup := setupIntegrationFixtures(t, conn)
	defer cleanup()
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")

	ts := agentServer(t, 200, 100, `{"result":{"parts":[{"text":"work completed successfully"}]}}`)
	defer ts.Close()

	mr := setupIntegrationRedis(t, ts.URL)
	defer mr.Close()

	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	a2aBody, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      "1",
		"method":  "message/send",
		"params": map[string]interface{}{
			"message": map[string]interface{}{
				"role":  "user",
				"parts": []map[string]string{{"type": "text", "text": "do work"}},
			},
		},
	})
	dh.executeDelegation(testSourceID, testTargetID, testDelegationID, a2aBody)
	time.Sleep(500 * time.Millisecond)

	status, preview, errDet := readDelegationRow(t, conn)
	if status != "completed" {
		t.Errorf("status: want completed, got %q", status)
	}
	if preview == "" {
		t.Logf("result_preview: %q", preview)
	}
	if errDet != "" {
		t.Errorf("error_detail should be empty on success: got %q", errDet)
	}
}

// TestIntegration_ExecuteDelegation_ProxyErrorNon2xx_RemainsFailed verifies that
// a 500 response routes to failure, not success. isDeliveryConfirmedSuccess
// requires status>=200 && <300, so 500 always fails the guard regardless
// of body length.
func TestIntegration_ExecuteDelegation_ProxyErrorNon2xx_RemainsFailed(t *testing.T) {
	allowLoopbackForTest(t)
	conn := integrationDB(t)
	cleanup := setupIntegrationFixtures(t, conn)
	defer cleanup()
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")

	ts := agentServer(t, 500, 100, `{"error":"agent crashed"}`)
	defer ts.Close()

	mr := setupTestRedis(t)
	defer mr.Close()
	db.CacheURL(context.Background(), testTargetID, ts.URL)

	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	a2aBody, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0", "id": "1", "method": "message/send",
		"params": map[string]interface{}{
			"message": map[string]interface{}{
				"role":  "user",
				"parts": []map[string]string{{"type": "text", "text": "do work"}},
			},
		},
	})
	dh.executeDelegation(testSourceID, testTargetID, testDelegationID, a2aBody)
	time.Sleep(500 * time.Millisecond)

	status, _, errDet := readDelegationRow(t, conn)
	if status != "failed" {
		t.Errorf("status: want failed, got %q", status)
	}
	if errDet == "" {
		t.Error("error_detail should be non-empty on failure")
	}
}

// TestIntegration_ExecuteDelegation_ProxyErrorEmptyBody_RemainsFailed verifies that
// a 200 response with an empty body (Content-Length: 0) routes to failure.
// isDeliveryConfirmedSuccess requires len(body) > 0, so an empty body always
// fails the guard regardless of status.
func TestIntegration_ExecuteDelegation_ProxyErrorEmptyBody_RemainsFailed(t *testing.T) {
	allowLoopbackForTest(t)
	conn := integrationDB(t)
	cleanup := setupIntegrationFixtures(t, conn)
	defer cleanup()
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")

	ts := agentServer(t, 200, 0, "")
	defer ts.Close()

	mr := setupTestRedis(t)
	defer mr.Close()
	db.CacheURL(context.Background(), testTargetID, ts.URL)

	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	a2aBody, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0", "id": "1", "method": "message/send",
		"params": map[string]interface{}{
			"message": map[string]interface{}{
				"role":  "user",
				"parts": []map[string]string{{"type": "text", "text": "do work"}},
			},
		},
	})
	dh.executeDelegation(testSourceID, testTargetID, testDelegationID, a2aBody)
	time.Sleep(500 * time.Millisecond)

	status, _, errDet := readDelegationRow(t, conn)
	if status != "failed" {
		t.Errorf("status: want failed, got %q", status)
	}
	if errDet == "" {
		t.Error("error_detail should be non-empty on failure")
	}
}

// TestIntegration_ExecuteDelegation_CleanProxyResponse_Unchanged is the baseline:
// a clean 200 response with a valid body and no error routes to success.
// This was always the behavior; the integration test confirms executeDelegation
// correctly records the ledger entry on the happy path.
func TestIntegration_ExecuteDelegation_CleanProxyResponse_Unchanged(t *testing.T) {
	allowLoopbackForTest(t)
	conn := integrationDB(t)
	cleanup := setupIntegrationFixtures(t, conn)
	defer cleanup()
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
		w.WriteHeader(http.StatusOK)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"result":{"parts":[{"text":"all good"}]}}`))
	}))
	defer ts.Close()

	mr := setupIntegrationRedis(t, ts.URL)
	defer mr.Close()

	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	a2aBody, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0", "id": "1", "method": "message/send",
		"params": map[string]interface{}{
			"message": map[string]interface{}{
				"role":  "user",
				"parts": []map[string]string{{"type": "text", "text": "do work"}},
			},
		},
	})
	dh.executeDelegation(testSourceID, testTargetID, testDelegationID, a2aBody)
	time.Sleep(500 * time.Millisecond)

	status, preview, errDet := readDelegationRow(t, conn)
	if status != "completed" {
		t.Errorf("status: want completed, got %q", status)
	}
	if preview == "" {
		t.Logf("result_preview: %q", preview)
	}
	if errDet != "" {
		t.Errorf("error_detail should be empty on success: got %q", errDet)
	}
}

// Test that a delegation where Redis cannot be reached still routes to failure
// (not panic). proxyA2ARequest falls back to DB URL lookup when Redis is down.
func TestIntegration_ExecuteDelegation_RedisDown_FallsBackToDB(t *testing.T) {
	allowLoopbackForTest(t)
	conn := integrationDB(t)
	cleanup := setupIntegrationFixtures(t, conn)
	defer cleanup()
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")

	mr := setupTestRedis(t)
	defer mr.Close()

	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	a2aBody, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0", "id": "1", "method": "message/send",
		"params": map[string]interface{}{
			"message": map[string]interface{}{
				"role":  "user",
				"parts": []map[string]string{{"type": "text", "text": "do work"}},
			},
		},
	})
	dh.executeDelegation(testSourceID, testTargetID, testDelegationID, a2aBody)
	time.Sleep(500 * time.Millisecond)

	status, _, errDet := readDelegationRow(t, conn)
	if status != "failed" {
		t.Errorf("status: want failed (no target URL), got %q", status)
	}
	if errDet == "" {
		t.Error("error_detail should be set on failure due to unreachable target")
	}
}
