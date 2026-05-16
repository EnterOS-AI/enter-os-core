package handlers

// Regression coverage for the POLL-mode arm of the canvas user-message
// data-loss bug (internal#470 sibling — tracked on internal#471).
//
// Bug (reported 2026-05-16 by CTO Hongming): "in canvas i sometimes lose
// my own message when i exit chat". The push-mode arm was fixed by
// #1347 (persistUserMessageAtIngest — a SYNCHRONOUS, before-dispatch,
// context.WithoutCancel INSERT). #1347's framing asserted "poll-mode
// workspaces were never affected — logA2AReceiveQueued already persists
// at ingest". That assertion is OVERSTATED.
//
// Hongming's tenant (slug `hongming`, org 2c940477-...) has 4 workspaces,
// ALL runtime=external with empty URL → ALL delivery_mode=poll (proven
// empirically: a benign A2A probe returns the synthetic
// {"delivery_mode":"poll","status":"queued"} envelope for every one).
// So his reported loss is the POLL path, NOT the push path #1347 fixes.
//
// Root cause (poll arm): the poll-mode short-circuit (a2a_proxy.go ~402)
// calls logA2AReceiveQueued and then IMMEDIATELY returns the synthetic
// 200 {status:"queued"} to the canvas. But logA2AReceiveQueued's durable
// INSERT runs inside h.goAsync(...) — a DETACHED goroutine with NO
// happens-before barrier against the HTTP response. The canvas sees 200
// ("message accepted") while the activity_logs row may not yet be — and,
// on a workspace-server restart / deploy / OOM / EC2 hibernation between
// the 200 and the goroutine's commit, NEVER will be — durable. There is
// also no fallback (unlike push-mode's legacy-INSERT fallback): a
// swallowed LogActivity error loses the message with only a log line.
// Chat-history reads activity_logs (postgres_store.go:165-187); a missing
// row = message gone on reopen. That is exactly Hongming's symptom.
//
// Fix (parity with push-mode): the poll-mode ingest persist of the
// canvas user message must be SYNCHRONOUS — committed before the queued
// 200 is returned — on a context.WithoutCancel derived context, so a
// client disconnect on chat-exit and a post-response restart cannot lose
// it. Behavior is never worse than today (best-effort; a persist error
// still returns queued).

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

// TestProxyA2A_PollMode_PersistsUserMessageSynchronouslyBeforeQueuedResponse
// is the defining contract: for a poll-mode workspace, the canvas user
// message MUST be durably INSERTed into activity_logs BEFORE the synthetic
// queued 200 is returned to the client — with NO reliance on a detached
// async goroutine completing later.
//
// The test proves the ordering by making the INSERT block briefly and
// asserting the handler does NOT return until the INSERT has completed.
// Pre-fix (INSERT in h.goAsync, response returned immediately) the
// handler returns ~instantly while the INSERT is still pending in the
// goroutine → the elapsed time is far below the injected INSERT delay and
// ExpectationsWereMet() is racy/unmet at return. Post-fix (synchronous
// persist before the queued response) the handler return is gated on the
// INSERT, so elapsed >= the injected delay and the expectation is met
// deterministically at return WITHOUT any waitAsyncForTest()/sleep.
func TestProxyA2A_PollMode_PersistsUserMessageSynchronouslyBeforeQueuedResponse(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	const wsID = "ws-poll-sync-persist"
	const insertDelay = 150 * time.Millisecond

	expectBudgetCheck(mock, wsID)

	// lookupDeliveryMode → poll, triggering the short-circuit.
	mock.ExpectQuery("SELECT delivery_mode FROM workspaces WHERE id").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"delivery_mode"}).AddRow("poll"))

	// workspace-name lookup inside logA2AReceiveQueued.
	mock.ExpectQuery(`SELECT name FROM workspaces WHERE id`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("Poll WS"))

	// The durable user-message write. We delay it so a synchronous
	// persist visibly gates the handler return; a detached-goroutine
	// persist (pre-fix) does not. The fix must keep using
	// context.WithoutCancel so this write survives a chat-exit cancel.
	mock.ExpectExec("INSERT INTO activity_logs").
		WillDelayFor(insertDelay).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}

	// callerID == "" (no X-Workspace-ID) → this is a canvas_user message,
	// exactly Hongming's case.
	body := `{"jsonrpc":"2.0","id":"poll-canvas-1","method":"message/send","params":{"message":{"role":"user","parts":[{"text":"my own message"}]}}}`
	c.Request = httptest.NewRequest("POST", "/workspaces/"+wsID+"/a2a", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	start := time.Now()
	handler.ProxyA2A(c)
	elapsed := time.Since(start)

	// Defining assertion #1: the handler must not have returned the
	// queued response before the durable INSERT committed. Pre-fix this
	// fails (elapsed ≈ 0, INSERT still racing in goAsync).
	if elapsed < insertDelay {
		t.Fatalf("poll-mode queued response returned in %v, before the %v user-message INSERT — "+
			"the message is not durable when the client/process goes away (DATA LOSS). "+
			"Persist must be synchronous before the queued 200.", elapsed, insertDelay)
	}

	// Defining assertion #2: the durable write actually happened by the
	// time the handler returned — checked WITHOUT waitAsyncForTest()/sleep.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("user-message INSERT was not durable at handler return (unmet sqlmock expectations): %v", err)
	}

	// Sanity: still the correct poll-mode envelope + status.
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (queued), got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if resp["status"] != "queued" || resp["delivery_mode"] != "poll" {
		t.Errorf("poll envelope changed: got status=%v delivery_mode=%v, want queued/poll",
			resp["status"], resp["delivery_mode"])
	}
}
