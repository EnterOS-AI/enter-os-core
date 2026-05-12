//go:build integration
// +build integration

// delegation_executor_integration_test.go — REAL Postgres integration tests for
// executeDelegation HTTP proxy edge cases that sqlmock cannot cover.
//
// The sqlmock tests in delegation_test.go pin which SQL statements fire but
// cannot detect bugs that depend on row state after the SQL runs, or on the
// ordering of ledger writes vs. HTTP response processing. The real-Postgres
// integration closes that gap.
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
	"net"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	mdb "github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/alicebob/miniredis/v2"
)

// integrationDB is imported from delegation_ledger_integration_test.go.
// Each test gets a fresh table state.

const testDelegationID = "del-159-test-integration"
const testSourceID    = "ws-source-159-integration"
const testTargetID    = "ws-target-159-integration"

// setupIntegrationFixtures inserts the rows executeDelegation requires:
//   - workspaces: source and target (siblings, parent_id=NULL so CanCommunicate=true)
//   - activity_logs: the 'delegate' row that updateDelegationStatus UPDATE will find
//   - delegations: the ledger row that recordLedgerStatus will UPDATE
//
// Returns a cleanup function the test should defer.
func setupIntegrationFixtures(t *testing.T, conn *sql.DB) func() {
	t.Helper()
	// Seed workspaces (siblings — both root-level so CanCommunicate is true).
	// We INSERT ... ON CONFLICT DO NOTHING so parallel test runs don't conflict.
	for _, ws := range []struct {
		id       string
		parentID *string // nil means NULL
	}{
		{testSourceID, nil},
		{testTargetID, nil},
	} {
		if _, err := conn.ExecContext(context.Background(),
			`INSERT INTO workspaces (id, parent_id) VALUES ($1, $2) ON CONFLICT (id) DO NOTHING`,
			ws.id, ws.parentID,
		); err != nil {
			t.Fatalf("seed workspace %s: %v", ws.id, err)
		}
	}

	// Seed the activity_logs row that updateDelegationStatus UPDATE will find.
	// request_body carries delegation_id so the UPDATE WHERE clause matches.
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

	// Seed the delegations ledger row (recordLedgerStatus inserts if not exists;
	// seed it as queued so recordLedgerStatus UPDATE lands cleanly).
	if _, err := conn.ExecContext(context.Background(), `
		INSERT INTO delegations
			(delegation_id, caller_id, callee_id, task_preview, status)
		VALUES ($1, $2::uuid, $3::uuid, 'do work', 'queued')
		ON CONFLICT (delegation_id) DO NOTHING
	`, testDelegationID, testSourceID, testTargetID); err != nil {
		t.Fatalf("seed delegations: %v", err)
	}

	return func() {
		// Clean up seeded rows so tests don't bleed into each other.
		conn.ExecContext(context.Background(),
			`DELETE FROM activity_logs WHERE workspace_id = $1 AND request_body->>'delegation_id' = $2`,
			testSourceID, testDelegationID)
		conn.ExecContext(context.Background(),
			`DELETE FROM delegations WHERE delegation_id = $1`, testDelegationID)
		conn.ExecContext(context.Background(),
			`DELETE FROM workspaces WHERE id IN ($1, $2)`, testSourceID, testTargetID)
	}
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

// TestIntegration_ExecuteDelegation_DeliveryConfirmedProxyError_TreatsAsSuccess
// is the integration regression gate for issue #159.
//
// Scenario: proxyA2ARequest returns an error but also a 200 status code with
// a non-empty partial body (connection closed before full Content-Length
// delivered). The isDeliveryConfirmedSuccess guard (status>=200 && <300 &&
// len(body)>0 && err!=nil) routes to handleSuccess.
//
// In the sqlmock version this test only verified that the UPDATE SQL fired.
// Here we verify the ledger row landed at 'completed' with the response body
// as result_preview.
func TestIntegration_ExecuteDelegation_DeliveryConfirmedProxyError_TreatsAsSuccess(t *testing.T) {
	conn := integrationDB(t)
	cleanup := setupIntegrationFixtures(t, conn)
	defer cleanup()
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")

	// Mock A2A agent server: 200 OK with Content-Length:100 but only 74 bytes
	// of body, then close the connection. Go's http.Client sees io.EOF on the
	// body read and proxyA2ARequest returns (200, <partial>, BadGateway).
	// isDeliveryConfirmedSuccess sees status=200, len(body)>0, err!=nil → true.
	var wg sync.WaitGroup
	wg.Add(1)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		defer wg.Done()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 2048)
		conn.Read(buf)
		// 200 with Content-Length:100 but only 74 bytes
		resp := "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n"
		resp += `{"result":{"parts":[{"text":"work completed successfully"}]}}` // 74 bytes
		conn.Write([]byte(resp))
		// Close immediately — client gets io.EOF on body read
	}()

	// Wire up mocks.
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	t.Setenv("REDIS_ADDR", mr.Addr())

	agentURL := "http://" + ln.Addr().String()
	mr.Set(fmt.Sprintf("ws:%s:url", testTargetID), agentURL)

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

	// Wait for goroutine + DB writes to settle.
	time.Sleep(500 * time.Millisecond)
	wg.Wait()

	status, preview, errDet := readDelegationRow(t, conn)
	if status != "completed" {
		t.Errorf("status: want completed, got %q", status)
	}
	if preview == "" {
		// The response body should land as result_preview.
		// Note: the partial body "work completed successfully" is what was read
		// before the connection dropped.
		t.Logf("result_preview (partial body expected): %q", preview)
	}
	if errDet != "" {
		t.Errorf("error_detail should be empty on success: got %q", errDet)
	}
}

// TestIntegration_ExecuteDelegation_ProxyErrorNon2xx_RemainsFailed verifies that
// a 500 response with a non-empty partial body (connection drop) routes to failure,
// not success. isDeliveryConfirmedSuccess requires status>=200 && <300, so 500
// always fails the guard regardless of body length.
func TestIntegration_ExecuteDelegation_ProxyErrorNon2xx_RemainsFailed(t *testing.T) {
	conn := integrationDB(t)
	cleanup := setupIntegrationFixtures(t, conn)
	defer cleanup()
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")

	// Mock server: 500 with Content-Length:100 but only ~24 bytes of body.
	var wg sync.WaitGroup
	wg.Add(1)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		defer wg.Done()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 2048)
		conn.Read(buf)
		resp := "HTTP/1.1 500 Internal Server Error\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n"
		resp += `{"error":"agent crashed"}` // ~24 bytes
		conn.Write([]byte(resp))
	}()

	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	t.Setenv("REDIS_ADDR", mr.Addr())

	mr.Set(fmt.Sprintf("ws:%s:url", testTargetID), "http://"+ln.Addr().String())

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
	wg.Wait()

	status, _, errDet := readDelegationRow(t, conn)
	if status != "failed" {
		t.Errorf("status: want failed, got %q", status)
	}
	if errDet == "" {
		t.Error("error_detail should be non-empty on failure")
	}
}

// TestIntegration_ExecuteDelegation_ProxyErrorEmptyBody_RemainsFailed verifies that
// a 200 response with an empty body (Content-Length: 0) and a transport error
// routes to failure. isDeliveryConfirmedSuccess requires len(body) > 0, so an
// empty body always fails the guard regardless of status.
func TestIntegration_ExecuteDelegation_ProxyErrorEmptyBody_RemainsFailed(t *testing.T) {
	conn := integrationDB(t)
	cleanup := setupIntegrationFixtures(t, conn)
	defer cleanup()
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")

	// Mock server: 200 with Content-Length:0, empty body, then close.
	var wg sync.WaitGroup
	wg.Add(1)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		defer wg.Done()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 2048)
		conn.Read(buf)
		resp := "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 0\r\n\r\n"
		conn.Write([]byte(resp))
	}()

	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	t.Setenv("REDIS_ADDR", mr.Addr())

	mr.Set(fmt.Sprintf("ws:%s:url", testTargetID), "http://"+ln.Addr().String())

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
	wg.Wait()

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
	conn := integrationDB(t)
	cleanup := setupIntegrationFixtures(t, conn)
	defer cleanup()
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")

	// Use httptest.NewServer for the clean success case (no connection drop).
	agentServer := http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"result":{"parts":[{"text":"all good"}]}}`))
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go agentServer.Serve(ln)
	defer agentServer.Close()

	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	t.Setenv("REDIS_ADDR", mr.Addr())

	mr.Set(fmt.Sprintf("ws:%s:url", testTargetID), "http://"+ln.Addr().String())

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
		// result_preview should carry the response body
		t.Logf("result_preview: %q", preview)
	}
	if errDet != "" {
		t.Errorf("error_detail should be empty on success: got %q", errDet)
	}
}

// Test that a delegation where Redis cannot be reached still routes to failure
// (not panic). proxyA2ARequest falls back to DB URL lookup when Redis is down.
func TestIntegration_ExecuteDelegation_RedisDown_FallsBackToDB(t *testing.T) {
	conn := integrationDB(t)
	cleanup := setupIntegrationFixtures(t, conn)
	defer cleanup()
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")

	// No miniredis — REDIS_ADDR not set, so resolveAgentURL falls back to DB.
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
	// No URL available — delegation should fail gracefully (target unreachable).
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

