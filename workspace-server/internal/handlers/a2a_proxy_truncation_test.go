package handlers

// a2a_proxy_truncation_test.go — regression coverage for core#2677.
//
// The A2A proxy caps request/response bodies to bounded sizes. The bug was
// that oversize bodies were silently truncated by io.LimitReader; these tests
// lock in the fix: bodies within the limit pass through intact, bodies over
// the limit fail LOUD with a clear truncated flag and no silent cutting.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// TestReadBodyWithLimit_UnderLimit proves a body smaller than the limit is
// returned unchanged and without error.
func TestReadBodyWithLimit_UnderLimit(t *testing.T) {
	body := []byte("small payload")
	got, err := readBodyWithLimit(bytes.NewReader(body), 1024, "request")
	if err != nil {
		t.Fatalf("readBodyWithLimit returned unexpected error: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("body changed: got %q, want %q", got, body)
	}
}

// TestReadBodyWithLimit_AtLimit proves a body exactly at the limit is accepted
// (the limit is an inclusive maximum).
func TestReadBodyWithLimit_AtLimit(t *testing.T) {
	body := []byte("exactly-five")
	got, err := readBodyWithLimit(bytes.NewReader(body), len(body), "request")
	if err != nil {
		t.Fatalf("readBodyWithLimit returned unexpected error: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("body changed: got %q, want %q", got, body)
	}
}

// TestReadBodyWithLimit_OverLimit proves an oversize body returns the first
// limit bytes AND an errA2ABodyTooLarge-wrapped error so callers can fail loud.
func TestReadBodyWithLimit_OverLimit(t *testing.T) {
	body := []byte("hello world")
	limit := 5
	got, err := readBodyWithLimit(bytes.NewReader(body), limit, "request")
	if err == nil {
		t.Fatal("expected error for oversize body, got nil")
	}
	if !errors.Is(err, errA2ABodyTooLarge) {
		t.Fatalf("expected errA2ABodyTooLarge, got %v", err)
	}
	want := body[:limit]
	if !bytes.Equal(got, want) {
		t.Fatalf("truncated body mismatch: got %q, want %q", got, want)
	}
}

// TestProxyA2A_RequestBodyTooLarge proves the public proxy endpoint returns
// 413 Payload Too Large with a truncated flag instead of silently cutting a
// >maxProxyRequestBody payload.
func TestProxyA2A_RequestBodyTooLarge(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-oversize-req"}}

	// maxProxyRequestBody+1 bytes guarantees truncation detection.
	oversize := strings.Repeat("A", maxProxyRequestBody+1)
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-oversize-req/a2a", strings.NewReader(oversize))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.ProxyA2A(c)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected status 413, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["truncated"] != true {
		t.Errorf("expected truncated=true, got %v", resp["truncated"])
	}
	if _, ok := resp["max_bytes"]; !ok {
		t.Errorf("expected max_bytes in response, got %v", resp)
	}
	if !strings.Contains(fmt.Sprint(resp["error"]), "exceeds") {
		t.Errorf("expected error to mention limit exceeded, got %v", resp["error"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestProxyA2A_LargeRequestWithinLimit proves a body at the (raised) request
// limit is accepted and forwarded intact, closing the original 1MB silent-cut
// gap for spec-length delegations.
func TestProxyA2A_LargeRequestWithinLimit(t *testing.T) {
	mock := setupTestDB(t)
	mr := setupTestRedis(t)
	allowLoopbackForTest(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	var receivedLen int
	agentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedBody, _ := io.ReadAll(r.Body)
		receivedLen = len(receivedBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":"1","result":{"status":"ok"}}`)
	}))
	defer agentServer.Close()

	mr.Set(fmt.Sprintf("ws:%s:url", "ws-large-ok"), agentServer.URL)
	expectBudgetCheck(mock, "ws-large-ok")
	mock.ExpectExec("INSERT INTO activity_logs").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-large-ok"}}

	// Build a valid JSON-RPC body just under the new 16MB cap.
	// Include messageId and use the canonical v0.3 "kind" discriminator so
	// normalizeA2APayload does not add fields during forwarding (which would
	// change the body length and break the exact-length assertion).
	prefix := `{"jsonrpc":"2.0","id":"1","method":"message/send","params":{"message":{"role":"user","messageId":"msg-1","parts":[{"kind":"text","text":"`
	suffix := `"}]}}}`
	paddingLen := maxProxyRequestBody - len(prefix) - len(suffix)
	if paddingLen < 0 {
		t.Fatalf("test setup error: prefix+suffix already exceeds maxProxyRequestBody")
	}
	largeBody := prefix + strings.Repeat("X", paddingLen) + suffix

	c.Request = httptest.NewRequest("POST", "/workspaces/ws-large-ok/a2a", strings.NewReader(largeBody))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.ProxyA2A(c)

	time.Sleep(50 * time.Millisecond)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	if receivedLen != len(largeBody) {
		t.Errorf("forwarded body length mismatch: got %d, want %d", receivedLen, len(largeBody))
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}
