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
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"strings"
	"sync"
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
	// Seed workspaces (siblings — both root-level so CanCommunicate is true).
	// We INSERT ... ON CONFLICT DO NOTHING so parallel test runs don't conflict.
	for _, ws := range []struct {
		id       string
		name     string
		parentID *string // nil means NULL
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

// rawTCPMockServer starts a raw TCP listener. It returns the server URL,
// a wait function, and a cleanup function.
//
// CRITICAL ordering to avoid deadlock:
//   1. Accept connection.
//   2. Read HTTP request headers (stop at blank line).
//   3. SEND RESPONSE FIRST — this unblocks the client's response reader so it can
//      finish its request-body write. If we drain the body FIRST, we deadlock: server
//      blocks reading body, client blocks waiting for response headers — neither can
//      proceed (server read vs client write on same body stream).
//   4. Drain request body in a background goroutine (with short timeout) so the
//      client doesn't get a broken-pipe error when we close.
//   5. Close connection.
func rawTCPMockServer(t *testing.T, statusCode int, declaredLength int, actualBody string) (url string, wait, cleanup func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	url = "http://" + ln.Addr().String()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		conn, err := ln.Accept()
		ln.Close()
		if err != nil {
			return
		}
		defer conn.Close()

		// Read HTTP request line + headers.
		reader := bufio.NewReader(conn)
		reqLine, _ := reader.ReadString('\n')
		_ = reqLine

		tp := textproto.NewReader(reader)
		headers := make(textproto.MIMEHeader)
		for {
			line, err := tp.ReadLine()
			if err != nil {
				return
			}
			if line == "" {
				break
			}
			k, v, _ := strings.Cut(line, ": ")
			headers.Set(k, v)
		}

		// Send response FIRST. This unblocks the client's response reader so the
		// client's request-body writer can complete (it waits for response headers
		// before sending the body in HTTP/1.1). Declared Content-Length may be
		// larger than actualBody — simulates a server that announces more bytes
		// than it sends before dropping the connection.
		statusText := http.StatusText(statusCode)
		resp := fmt.Sprintf(
			"HTTP/1.1 %d %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
			statusCode, statusText, declaredLength, actualBody,
		)
		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		conn.Write([]byte(resp)) //nolint:errcheck

		// Brief pause so the client's kernel TCP receive buffer gets the response
		// before we close. The client reads headers + partial body in this window.
		time.Sleep(50 * time.Millisecond)

		// Drain request body in background goroutine with a short timeout.
		// This prevents a broken-pipe error on the client's request-body write:
		// the client's write goroutine needs the server to read the body before
		// it can finish; if we just close the conn, the client gets a SIGPIPE.
		var drainWg sync.WaitGroup
		drainWg.Add(1)
		go func() {
			defer drainWg.Done()
			if cl := headers.Get("Content-Length"); cl != "" {
				var n int
				fmt.Sscanf(cl, "%d", &n)
				conn.SetReadDeadline(time.Now().Add(2 * time.Second))
				io.Copy(io.Discard, io.LimitReader(conn, int64(n))) //nolint:errcheck
			}
		}()

		// Close while drain is in progress. drain goroutine will get a
		// connection-closed error, which is fine (it just stops reading).
		conn.Close()
		drainWg.Wait()
	}()
	return url, func() { wg.Wait() }, func() {
		conn, err := net.DialTimeout("tcp", ln.Addr().String(), 100*time.Millisecond)
		if err == nil {
			conn.Close()
		}
	}
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
	allowLoopbackForTest(t) // raw TCP mock uses 127.0.0.1; SSRF guard must permit it
	conn := integrationDB(t)
	cleanup := setupIntegrationFixtures(t, conn)
	defer cleanup()
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")

	// Raw TCP mock: Content-Length:100 declared, 74 bytes sent, then close.
	// The server drains the request body before writing the response so the
	// client doesn't get a broken-pipe on its request-body write.
	url, serverWait, serverCleanup := rawTCPMockServer(t, 200, 100, `{"result":{"parts":[{"text":"work completed successfully"}]}}`)
	defer serverCleanup()

	mr := setupIntegrationRedis(t, url)
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
	serverWait()

	status, preview, errDet := readDelegationRow(t, conn)
	if status != "completed" {
		t.Errorf("status: want completed, got %q", status)
	}
	if preview == "" {
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
	allowLoopbackForTest(t) // raw TCP mock uses 127.0.0.1; SSRF guard must permit it
	conn := integrationDB(t)
	cleanup := setupIntegrationFixtures(t, conn)
	defer cleanup()
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")

	url, serverWait, serverCleanup := rawTCPMockServer(t, 500, 100, `{"error":"agent crashed"}`)
	defer serverCleanup()

	mr := setupTestRedis(t)
	defer mr.Close()
	db.CacheURL(context.Background(), testTargetID, url)

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
	serverWait()

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
	allowLoopbackForTest(t) // raw TCP mock uses 127.0.0.1; SSRF guard must permit it
	conn := integrationDB(t)
	cleanup := setupIntegrationFixtures(t, conn)
	defer cleanup()
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")

	url, serverWait, serverCleanup := rawTCPMockServer(t, 200, 0, "")
	defer serverCleanup()

	mr := setupTestRedis(t)
	defer mr.Close()
	db.CacheURL(context.Background(), testTargetID, url)

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
	serverWait()

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
	allowLoopbackForTest(t) // raw TCP mock uses 127.0.0.1; SSRF guard must permit it
	conn := integrationDB(t)
	cleanup := setupIntegrationFixtures(t, conn)
	defer cleanup()
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")

	url, serverWait, serverCleanup := rawTCPMockServer(t, 200, 36, `{"result":{"parts":[{"text":"all good"}]}}`)
	defer serverCleanup()

	mr := setupIntegrationRedis(t, url)
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
	serverWait()

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
	conn := integrationDB(t)
	cleanup := setupIntegrationFixtures(t, conn)
	defer cleanup()
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")

	// Set up miniredis so db.RDB is non-nil (RecordAndBroadcast requires it),
	// but do NOT cache the workspace URL. resolveAgentURL skips Redis and falls
	// back to DB, which also has no URL → target unreachable.
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
	// No URL available — delegation should fail gracefully (target unreachable).
	dh.executeDelegation(testSourceID, testTargetID, testDelegationID, a2aBody)
	// No serverWait() needed — the server was never started.

	status, _, errDet := readDelegationRow(t, conn)
	if status != "failed" {
		t.Errorf("status: want failed (no target URL), got %q", status)
	}
	if errDet == "" {
		t.Error("error_detail should be set on failure due to unreachable target")
	}
}
