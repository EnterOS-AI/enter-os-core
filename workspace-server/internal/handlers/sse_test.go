package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// expectWorkspaceExists queues the EXISTS query that StreamEvents fires first.
func expectWorkspaceExists(mock sqlmock.Sqlmock, workspaceID string, exists bool) {
	rows := sqlmock.NewRows([]string{"exists"}).AddRow(exists)
	mock.ExpectQuery(`SELECT EXISTS`).
		WithArgs(workspaceID).
		WillReturnRows(rows)
}

// runSSEHandler starts StreamEvents in a background goroutine using a
// cancellable context, waits waitAfterStart for the handler to subscribe,
// then returns a drain function (cancel + wait for goroutine exit).
func runSSEHandler(t *testing.T, h *SSEHandler, workspaceID string) (
	w *httptest.ResponseRecorder,
	inject func(), // call to cancel immediately
	done <-chan struct{},
) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	w = httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: workspaceID}}
	c.Request = httptest.NewRequest("GET", "/workspaces/"+workspaceID+"/events/stream", nil).WithContext(ctx)

	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		h.StreamEvents(c)
	}()

	return w, cancel, doneCh
}

// TestSSE_ContentType verifies the handler sets text/event-stream on the response.
func TestSSE_ContentType(t *testing.T) {
	mock := setupTestDB(t)
	expectWorkspaceExists(mock, "ws-1", true)

	b := newTestBroadcaster()
	h := NewSSEHandler(b)

	w, cancel, done := runSSEHandler(t, h, "ws-1")

	// Allow the handler to subscribe, then tear it down.
	time.Sleep(30 * time.Millisecond)
	cancel()
	<-done

	ct := w.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("expected Content-Type text/event-stream, got %q", ct)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet DB expectations: %v", err)
	}
}

// TestSSE_InitialPing verifies the handler emits the ": ping" SSE comment on connect.
func TestSSE_InitialPing(t *testing.T) {
	mock := setupTestDB(t)
	expectWorkspaceExists(mock, "ws-1", true)

	b := newTestBroadcaster()
	h := NewSSEHandler(b)

	w, cancel, done := runSSEHandler(t, h, "ws-1")
	time.Sleep(30 * time.Millisecond)
	cancel()
	<-done

	body := w.Body.String()
	if !strings.Contains(body, ": ping") {
		t.Errorf("expected SSE ping comment, body was:\n%s", body)
	}
}

// TestSSE_AGUIFormat verifies that a broadcast event is wrapped in the AG-UI envelope.
func TestSSE_AGUIFormat(t *testing.T) {
	mock := setupTestDB(t)
	expectWorkspaceExists(mock, "ws-1", true)

	b := newTestBroadcaster()
	h := NewSSEHandler(b)

	w, cancel, done := runSSEHandler(t, h, "ws-1")

	// Wait for the handler goroutine to reach its select loop.
	time.Sleep(30 * time.Millisecond)
	b.BroadcastOnly("ws-1", "TASK_UPDATED", map[string]string{"status": "running"})
	time.Sleep(30 * time.Millisecond)
	cancel()
	<-done

	body := w.Body.String()
	// Find the first "data: ..." line.
	var dataLine string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "data: ") {
			dataLine = strings.TrimPrefix(line, "data: ")
			break
		}
	}
	if dataLine == "" {
		t.Fatalf("no data: line found in SSE response:\n%s", body)
	}

	var env struct {
		Type      string          `json:"type"`
		Timestamp int64           `json:"timestamp"`
		Data      json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(dataLine), &env); err != nil {
		t.Fatalf("invalid AG-UI envelope JSON %q: %v", dataLine, err)
	}
	if env.Type != "TASK_UPDATED" {
		t.Errorf("expected type TASK_UPDATED, got %q", env.Type)
	}
	if env.Timestamp <= 0 {
		t.Errorf("expected positive timestamp, got %d", env.Timestamp)
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		t.Errorf("expected non-null data field, got %q", string(env.Data))
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet DB expectations: %v", err)
	}
}

// TestSSE_WorkspaceFilter verifies that events for a different workspace are NOT delivered.
func TestSSE_WorkspaceFilter(t *testing.T) {
	mock := setupTestDB(t)
	expectWorkspaceExists(mock, "ws-1", true)

	b := newTestBroadcaster()
	h := NewSSEHandler(b)

	w, cancel, done := runSSEHandler(t, h, "ws-1")

	time.Sleep(30 * time.Millisecond)
	// Broadcast to a completely different workspace.
	b.BroadcastOnly("ws-99", "AGENT_MESSAGE", map[string]string{"text": "secret"})
	time.Sleep(30 * time.Millisecond)
	cancel()
	<-done

	body := w.Body.String()
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "data: ") {
			t.Errorf("expected no data: events for different workspace, got: %s", line)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet DB expectations: %v", err)
	}
}

// TestSSE_WorkspaceNotFound verifies a 404 is returned when the workspace does not exist.
func TestSSE_WorkspaceNotFound(t *testing.T) {
	mock := setupTestDB(t)
	expectWorkspaceExists(mock, "missing-ws", false)

	b := newTestBroadcaster()
	h := NewSSEHandler(b)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "missing-ws"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/missing-ws/events/stream", nil)

	h.StreamEvents(c)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for missing workspace, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet DB expectations: %v", err)
	}
}

// TestSSE_MultipleEventsDelivered verifies multiple sequential broadcasts all arrive.
func TestSSE_MultipleEventsDelivered(t *testing.T) {
	mock := setupTestDB(t)
	expectWorkspaceExists(mock, "ws-1", true)

	b := newTestBroadcaster()
	h := NewSSEHandler(b)

	w, cancel, done := runSSEHandler(t, h, "ws-1")

	time.Sleep(30 * time.Millisecond)
	b.BroadcastOnly("ws-1", "AGENT_MESSAGE", map[string]string{"msg": "one"})
	b.BroadcastOnly("ws-1", "TASK_UPDATED", map[string]string{"status": "done"})
	b.BroadcastOnly("ws-1", "A2A_RESPONSE", map[string]string{"result": "ok"})
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	body := w.Body.String()
	var dataLines []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "data: ") {
			dataLines = append(dataLines, line)
		}
	}
	if len(dataLines) != 3 {
		t.Errorf("expected 3 data: lines, got %d:\n%s", len(dataLines), body)
	}

	// Verify event types appear in order.
	expectedTypes := []string{"AGENT_MESSAGE", "TASK_UPDATED", "A2A_RESPONSE"}
	for i, dl := range dataLines {
		var env struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(dl, "data: ")), &env); err != nil {
			t.Fatalf("line %d: invalid JSON: %v", i, err)
		}
		if env.Type != expectedTypes[i] {
			t.Errorf("line %d: expected type %s, got %s", i, expectedTypes[i], env.Type)
		}
	}
}
