package handlers

import (
	"context"
	"net/http"
	"testing"
)

// TestHandleA2ADispatchError_NativeSession_SkipsEnqueue validates capability
// primitive #5: when the target workspace has declared
// provides_native_session=True, a busy-shaped dispatch error MUST short-
// circuit straight to 503 + Retry-After. The platform's a2a_queue is
// skipped because the SDK owns its own queue/session state — double-
// buffering would cause spurious dispatches when the SDK is still busy.
//
// Pin via sqlmock: we deliberately do NOT expect any INSERT INTO a2a_queue.
// If a future refactor re-introduces enqueueing under native_session,
// sqlmock fails the test on the unexpected query.
func TestHandleA2ADispatchError_NativeSession_SkipsEnqueue(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	// Pre-populate the cache: ws-native owns its session natively.
	runtimeOverrides.SetCapabilities("ws-native", map[string]bool{"session": true})
	defer runtimeOverrides.Reset()

	// DeadlineExceeded triggers isUpstreamBusyError. Without the native
	// gate, this would fire EnqueueA2A → INSERT INTO a2a_queue. With
	// the gate, it short-circuits to 503. We expect ZERO queue queries;
	// sqlmock's ExpectationsWereMet implicitly enforces that on teardown.
	_, _, perr := handler.handleA2ADispatchError(
		context.Background(), "ws-native", "", []byte("{}"), "message/send",
		context.DeadlineExceeded, 1, false,
	)
	if perr == nil {
		t.Fatal("expected proxy error, got nil")
	}
	if perr.Status != http.StatusServiceUnavailable {
		t.Errorf("got status %d, want 503 (native_session bypasses queue but still 503s)", perr.Status)
	}
	if perr.Headers["Retry-After"] == "" {
		t.Error("expected Retry-After header on native-session 503")
	}
	// Pin the marker so callers' adapters can distinguish this from a
	// queue-failure 503: the body has native_session=true.
	if got, _ := perr.Response["native_session"].(bool); !got {
		t.Errorf("expected native_session=true in response body; got %+v", perr.Response)
	}
	// And busy=true stays so existing busy-handling code paths still trigger.
	if got, _ := perr.Response["busy"].(bool); !got {
		t.Errorf("expected busy=true in response body; got %+v", perr.Response)
	}
}

// TestHandleA2ADispatchError_NoNativeSession_StillEnqueues is the negative
// pin: a workspace WITHOUT the capability flag falls through to the
// existing EnqueueA2A path (and 503 if that fails). Same shape as
// TestHandleA2ADispatchError_ContextDeadline; we duplicate it here so
// the native_session gate change is bracketed by both positive and
// negative tests in the same file.
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
	// Queue insert failed → falls through to legacy 503 (without
	// native_session marker).
	if perr.Status != http.StatusServiceUnavailable {
		t.Errorf("got status %d, want 503", perr.Status)
	}
	if got, _ := perr.Response["native_session"].(bool); got {
		t.Errorf("non-native workspace should NOT carry native_session=true in response; got %+v", perr.Response)
	}
}

// errTestQueueUnavailable is reused in this file's tests to simulate a
// transient queue-insert failure without dragging in fmt.Errorf at every
// call site.
var errTestQueueUnavailable = &queueUnavailableErr{}

type queueUnavailableErr struct{}

func (e *queueUnavailableErr) Error() string { return "test: queue unavailable" }
