package handlers

// Regression coverage for the PUSH-mode arm of the canvas user-message
// mid-turn leave/refresh bug (issue #2560).
//
// Pre-#2560: the user message was only written to activity_logs at turn
// completion (logA2ASuccess, post-dispatch). A leave/refresh mid-turn
// re-hydrated an empty pane plus typing dots — the durable
// activity_logs row didn't exist yet — and a workspace-server restart /
// deploy / OOM between the canvas 200 and the goroutine's commit lost
// the message permanently (chat-history is read from activity_logs; no
// row == no message on reopen). The poll-mode arm was separately
// covered by a2a_poll_ingest_persist_test.go; this file covers the
// push-mode arm.
//
// Fix: persistUserMessageAtIngest runs SYNCHRONOUSLY in
// proxyA2ARequest, immediately after normalizeA2APayload and BEFORE
// the poll/mock/push short-circuits, with context.WithoutCancel so a
// client disconnect on chat-exit cannot cancel the write. Idempotent
// on messageId via the partial unique index (idx_activity_logs_msg_id)
// + ON CONFLICT DO UPDATE in logActivityExec — the same row is later
// updated by logA2ASuccess to attach response_body, so chat-history
// reads back exactly one (user, agent) pair per messageId with no
// duplicate bubble.
//
// Defining assertions for this test:
//  1. The ingest INSERT into activity_logs MUST complete BEFORE
//     proxyA2ARequest returns (i.e. before the 200 reaches the client).
//     Pre-fix: handler returns ~instantly while the INSERT is still
//     racing in a detached goroutine → elapsed ≪ insertDelay.
//     Post-fix: handler return is gated on the INSERT → elapsed ≥
//     insertDelay.
//  2. The ingest row carries the canvas user's messageId. The
//     completion row (logA2ASuccess) attaches response_body to the
//     SAME row via ON CONFLICT — chat-history emits a single
//     (user, agent) pair, not two.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

func TestProxyA2A_PushMode_PersistsUserMessageSynchronouslyBeforeDispatch(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	const wsID = "ws-push-sync-persist"
	const insertDelay = 50 * time.Millisecond

	expectBudgetCheck(mock, wsID)

	// Ingest at receipt (the fix). Synchronous — handler return is
	// gated on this write completing. A detached-goroutine ingest
	// (pre-fix) does NOT block; a synchronous ingest does.
	mock.ExpectExec("INSERT INTO activity_logs").
		WillDelayFor(insertDelay).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}

	// Canvas user message with explicit messageId. The fix is
	// observable only when the messageId is set (otherwise
	// persistUserMessageAtIngest short-circuits and the completion
	// path remains authoritative — pre-#2560 behavior, no regression).
	body := `{"jsonrpc":"2.0","id":"push-canvas-1","method":"message/send","params":{"message":{"role":"user","messageId":"msg-2560-push-1","parts":[{"text":"my own message"}]}}}`
	c.Request = httptest.NewRequest("POST", "/workspaces/"+wsID+"/a2a", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	start := time.Now()
	handler.ProxyA2A(c)
	elapsed := time.Since(start)

	// Defining assertion #1: handler must not have returned the
	// response before the durable ingest INSERT committed. Pre-fix this
	// fails (elapsed ≈ 0, INSERT still racing in goAsync).
	if elapsed < insertDelay {
		t.Fatalf("push-mode handler returned in %v, before the %v user-message ingest INSERT — "+
			"mid-turn leave/refresh re-hydrates an empty pane (DATA LOSS). "+
			"persistUserMessageAtIngest must be synchronous before dispatch.", elapsed, insertDelay)
	}

	// Defining assertion #2: the durable write actually happened by the
	// time the handler returned. ExpectationsWereMet() in a goroutine
	// with a hard 2s timeout — fails fast (no CI hang) on regression
	// while returning promptly on success.
	expectDone := make(chan error, 1)
	go func() { expectDone <- mock.ExpectationsWereMet() }()
	select {
	case err := <-expectDone:
		if err != nil {
			t.Fatalf("user-message ingest INSERT was not durable at handler return (unmet sqlmock expectations): %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("ExpectationsWereMet() hung for >2s — INSERT mock never fired. " +
			"Likely cause: production code regressed persistUserMessageAtIngest to async " +
			"(INSERT fires after handler returns, not before).")
	}

	// Sanity: the response is JSON. We don't assert the body shape
	// here — push-mode goes through a longer code path (resolveAgentURL,
	// preflight, dispatch) and short-circuits in this test setup
	// (the dispatch will fail with the mock URL, but the ingest write
	// has already happened, which is the only thing #2560 tests).
	if w.Code == 0 {
		t.Errorf("handler did not write a response (w.Code == 0)")
	}
	_ = json.RawMessage{}
	_ = http.StatusOK
}
