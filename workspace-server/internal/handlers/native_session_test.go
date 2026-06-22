package handlers

import (
	"context"
	"net/http"
	"testing"
)

// TestHandleA2ADispatchError_NativeSession_NowEnqueues validates the #1684
// fix: native_session adapters used to short-circuit to 503-no-queue here,
// on the assumption that the SDK owned an inbound queue. In practice the
// common native_session SDKs (claude-agent-sdk, codex app-server, hermes)
// don't — new turns arrive only via the same HTTP POST that returns busy.
// So cron fires bounced 503 every tick until the SDK voluntarily yielded;
// Reno Stars #1684 observed 12 consecutive `*/30` cron fires lost over 6h.
//
// Post-fix: native_session and non-native both enqueue. Drain timing is
// gated by registry.go:Heartbeat (`payload.ActiveTasks < maxConcurrent`)
// so the queued item only dispatches when the SDK reports spare capacity
// — i.e. the next heartbeat after the in-flight turn returns.
//
// This test pins the new behavior: native_session capability DOES NOT
// bypass EnqueueA2A. We expect the INSERT INTO a2a_queue query to fire,
// here arranged to fail so we can observe the legacy 503 fallback (and
// thereby confirm the INSERT was attempted; sqlmock fails the test if
// the expected query never runs).
func TestHandleA2ADispatchError_NativeSession_NowEnqueues(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	// Pre-populate the cache: ws-native owns its session natively.
	runtimeOverrides.SetCapabilities("ws-native", map[string]bool{"session": true})
	defer runtimeOverrides.Reset()

	// We now EXPECT the INSERT to fire even with native_session=true. Make
	// it fail so the handler falls through to the legacy 503 path — that
	// lets us assert (1) enqueue was attempted, (2) the response on
	// queue-failure does NOT carry native_session=true marker (that field
	// was removed alongside the gate).
	mock.ExpectQuery(`INSERT INTO a2a_queue`).
		WithArgs("ws-native", nil, PriorityTask, "{}", "message/send", nil).
		WillReturnError(errTestQueueUnavailable)

	_, _, perr := handler.handleA2ADispatchError(
		context.Background(), "ws-native", "", []byte("{}"), "message/send",
		context.DeadlineExceeded, 1, false,
	)
	if perr == nil {
		t.Fatal("expected proxy error, got nil")
	}
	if perr.Status != http.StatusServiceUnavailable {
		t.Errorf("got status %d, want 503 (enqueue failed → legacy 503 fallback)", perr.Status)
	}
	if perr.Headers["Retry-After"] == "" {
		t.Error("expected Retry-After header on busy-503")
	}
	// The native_session marker was removed from the response body — the
	// platform queues both kinds now, callers no longer distinguish. Pin
	// its absence so a future revert is caught.
	if got, ok := perr.Response["native_session"].(bool); ok && got {
		t.Errorf("native_session marker should be gone after #1684 fix; got %+v", perr.Response)
	}
	if got, _ := perr.Response["busy"].(bool); !got {
		t.Errorf("expected busy=true; got %+v", perr.Response)
	}
}

// TestHandleA2ADispatchError_NoNativeSession_StillEnqueues — non-native
// behavior is unchanged: enqueue is attempted, fail-fallback to 503. This
// negative pin guards against accidentally reverting the unification by
// re-introducing a `if HasCapability(...)` gate that would short-circuit
// the enqueue path.
func TestHandleA2ADispatchError_NoNativeSession_StillEnqueues(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	// Cache is empty for this workspace → falls through to EnqueueA2A.
	runtimeOverrides.Reset()
	defer runtimeOverrides.Reset()

	mock.ExpectQuery(`INSERT INTO a2a_queue`).
		WithArgs("ws-platform-queue", nil, PriorityTask, "{}", "message/send", nil).
		WillReturnError(errTestQueueUnavailable)

	_, _, perr := handler.handleA2ADispatchError(
		context.Background(), "ws-platform-queue", "", []byte("{}"), "message/send",
		context.DeadlineExceeded, 1, false,
	)
	if perr == nil {
		t.Fatal("expected proxy error, got nil")
	}
	if perr.Status != http.StatusServiceUnavailable {
		t.Errorf("got status %d, want 503", perr.Status)
	}
	if got, _ := perr.Response["native_session"].(bool); got {
		t.Errorf("non-native workspace should NOT carry native_session=true; got %+v", perr.Response)
	}
}

// errTestQueueUnavailable is reused in this file's tests to simulate a
// transient queue-insert failure without dragging in fmt.Errorf at every
// call site.
var errTestQueueUnavailable = &queueUnavailableErr{}

type queueUnavailableErr struct{}

func (e *queueUnavailableErr) Error() string { return "test: queue unavailable" }
