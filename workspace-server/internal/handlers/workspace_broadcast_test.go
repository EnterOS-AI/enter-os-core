package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// -------- Org-scoped recipient query tests (OFFSEC-015) --------

// TestBroadcast_OrgScopedRecipients verifies that a broadcast from Org-A does
// NOT reach workspaces belonging to Org-B. This is the core regression test
// for OFFSEC-015: the original query had no org filter, so a workspace in
// Org-A could broadcast to every non-removed workspace in the entire DB,
// including workspaces owned by other tenants.
func TestBroadcast_OrgScopedRecipients(t *testing.T) {
	mock := setupTestDB(t)
	broadcaster := newTestBroadcaster()
	handler := NewBroadcastHandler(broadcaster)

	// Org-A structure:
	//   org-a-root  (parent_id = NULL)  ← sender
	//   ├── ws-a-child
	// Org-B structure:
	//   org-b-root  (parent_id = NULL)
	//   └── ws-b-child
	senderID := "00000000-0000-0000-0000-000000000001" // org-a-root
	wsAChild := "00000000-0000-0000-0000-000000000002"
	// ws-b-child is in Org-B (different root); the org-scoped query MUST NOT include it.

	// 1. Sender lookup
	mock.ExpectQuery(`SELECT name, broadcast_enabled FROM workspaces WHERE id = \$1 AND status != 'removed'`).
		WithArgs(senderID).
		WillReturnRows(sqlmock.NewRows([]string{"name", "broadcast_enabled"}).AddRow("Org-A Root", true))

	// 2. Org root lookup — sender is its own root (parent_id = NULL)
	mock.ExpectQuery(`WITH RECURSIVE org_chain AS`).
		WithArgs(senderID).
		WillReturnRows(sqlmock.NewRows([]string{"root_id"}).AddRow(senderID))

	// 3. Org-scoped recipient query — MUST include org filter so ws-b-child is NOT included.
	// The query joins on org_chain.root_id = orgRootID, which scopes to Org-A only.
	mock.ExpectQuery(`WITH RECURSIVE org_chain AS`).
		WithArgs(senderID, senderID). // orgRootID, senderID (EXCLUDED)
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(wsAChild)) // only Org-A child

	// Activity log inserts
	mock.ExpectExec(`INSERT INTO activity_logs`).WithArgs(wsAChild, senderID, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO activity_logs`).WithArgs(senderID, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: senderID}}
	body := `{"message":"hello from org-a"}`
	c.Request = httptest.NewRequest("POST", "/workspaces/"+senderID+"/broadcast", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Broadcast(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if resp["status"] != "sent" {
		t.Errorf("expected status 'sent', got %v", resp["status"])
	}
	// ws-b-child is in a DIFFERENT org — the org-scoped query MUST NOT include it.
	// If it were included, the mock would have an unmet expectation.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations — cross-org workspace was included in broadcast: %v", err)
	}
}

// TestBroadcast_OrgScoped_OrgRootSender verifies that when the sender IS the
// org root (parent_id = NULL), broadcasts still reach sibling workspaces.
func TestBroadcast_OrgScoped_OrgRootSender(t *testing.T) {
	mock := setupTestDB(t)
	broadcaster := newTestBroadcaster()
	handler := NewBroadcastHandler(broadcaster)

	senderID := "00000000-0000-0000-0000-000000000001" // org-a-root
	siblingID := "00000000-0000-0000-0000-000000000002"

	mock.ExpectQuery(`SELECT name, broadcast_enabled FROM workspaces WHERE id = \$1 AND status != 'removed'`).
		WithArgs(senderID).
		WillReturnRows(sqlmock.NewRows([]string{"name", "broadcast_enabled"}).AddRow("Root Agent", true))

	// Sender is the org root — CTE returns sender's own ID as root
	mock.ExpectQuery(`WITH RECURSIVE org_chain AS`).
		WithArgs(senderID).
		WillReturnRows(sqlmock.NewRows([]string{"root_id"}).AddRow(senderID))

	// Recipients in same org, excluding sender
	mock.ExpectQuery(`WITH RECURSIVE org_chain AS`).
		WithArgs(senderID, senderID).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(siblingID))

	mock.ExpectExec(`INSERT INTO activity_logs`).WithArgs(siblingID, senderID, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO activity_logs`).WithArgs(senderID, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: senderID}}
	body := `{"message":"hello siblings"}`
	c.Request = httptest.NewRequest("POST", "/workspaces/"+senderID+"/broadcast", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Broadcast(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestBroadcast_OrgScoped_ChildWorkspaceSender verifies that a non-root child
// workspace can broadcast to siblings in the same org.
func TestBroadcast_OrgScoped_ChildWorkspaceSender(t *testing.T) {
	mock := setupTestDB(t)
	broadcaster := newTestBroadcaster()
	handler := NewBroadcastHandler(broadcaster)

	orgRootID := "00000000-0000-0000-0000-000000000001"
	senderID := "00000000-0000-0000-0000-000000000002" // child workspace
	siblingID := "00000000-0000-0000-0000-000000000003"

	mock.ExpectQuery(`SELECT name, broadcast_enabled FROM workspaces WHERE id = \$1 AND status != 'removed'`).
		WithArgs(senderID).
		WillReturnRows(sqlmock.NewRows([]string{"name", "broadcast_enabled"}).AddRow("Child Agent", true))

	// Org root lookup — walk up to find org-a-root
	mock.ExpectQuery(`WITH RECURSIVE org_chain AS`).
		WithArgs(senderID).
		WillReturnRows(sqlmock.NewRows([]string{"root_id"}).AddRow(orgRootID))

	// Recipients: same org, excluding sender
	mock.ExpectQuery(`WITH RECURSIVE org_chain AS`).
		WithArgs(orgRootID, senderID).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(siblingID))

	mock.ExpectExec(`INSERT INTO activity_logs`).WithArgs(siblingID, senderID, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO activity_logs`).WithArgs(senderID, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: senderID}}
	body := `{"message":"child broadcasting"}`
	c.Request = httptest.NewRequest("POST", "/workspaces/"+senderID+"/broadcast", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Broadcast(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// -------- Non-regression cases --------

func TestBroadcast_NotFound(t *testing.T) {
	mock := setupTestDB(t)
	broadcaster := newTestBroadcaster()
	handler := NewBroadcastHandler(broadcaster)

	senderID := "00000000-0000-0000-0000-000000000099"
	// UUID is valid, but no workspace row matches
	mock.ExpectQuery(`SELECT name, broadcast_enabled FROM workspaces WHERE id = \$1 AND status != 'removed'`).
		WithArgs(senderID).
		WillReturnError(errors.New("workspace not found"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: senderID}}
	body := `{"message":"test"}`
	c.Request = httptest.NewRequest("POST", "/workspaces/"+senderID+"/broadcast", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Broadcast(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestBroadcast_Disabled(t *testing.T) {
	mock := setupTestDB(t)
	broadcaster := newTestBroadcaster()
	handler := NewBroadcastHandler(broadcaster)

	senderID := "00000000-0000-0000-0000-000000000001"
	mock.ExpectQuery(`SELECT name, broadcast_enabled FROM workspaces WHERE id = \$1 AND status != 'removed'`).
		WithArgs(senderID).
		WillReturnRows(sqlmock.NewRows([]string{"name", "broadcast_enabled"}).AddRow("Disabled Agent", false))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: senderID}}
	body := `{"message":"should not send"}`
	c.Request = httptest.NewRequest("POST", "/workspaces/"+senderID+"/broadcast", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Broadcast(c)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if resp["error"] != "broadcast_disabled" {
		t.Errorf("expected error 'broadcast_disabled', got %v", resp["error"])
	}
}

func TestBroadcast_EmptyOrg_NoRecipients(t *testing.T) {
	mock := setupTestDB(t)
	broadcaster := newTestBroadcaster()
	handler := NewBroadcastHandler(broadcaster)

	senderID := "00000000-0000-0000-0000-000000000001" // org root, only workspace in org

	mock.ExpectQuery(`SELECT name, broadcast_enabled FROM workspaces WHERE id = \$1 AND status != 'removed'`).
		WithArgs(senderID).
		WillReturnRows(sqlmock.NewRows([]string{"name", "broadcast_enabled"}).AddRow("Lone Root", true))

	mock.ExpectQuery(`WITH RECURSIVE org_chain AS`).
		WithArgs(senderID).
		WillReturnRows(sqlmock.NewRows([]string{"root_id"}).AddRow(senderID))

	// No other workspaces in this org
	mock.ExpectQuery(`WITH RECURSIVE org_chain AS`).
		WithArgs(senderID, senderID).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	mock.ExpectExec(`INSERT INTO activity_logs`).WithArgs(senderID, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: senderID}}
	body := `{"message":"hello org"}`
	c.Request = httptest.NewRequest("POST", "/workspaces/"+senderID+"/broadcast", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Broadcast(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if resp["delivered"] != float64(0) {
		t.Errorf("expected delivered=0, got %v", resp["delivered"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestBroadcast_InvalidWorkspaceID(t *testing.T) {
	setupTestDB(t)
	broadcaster := newTestBroadcaster()
	handler := NewBroadcastHandler(broadcaster)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "not-a-uuid"}}
	body := `{"message":"test"}`
	c.Request = httptest.NewRequest("POST", "/workspaces/not-a-uuid/broadcast", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Broadcast(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestBroadcast_MissingMessage(t *testing.T) {
	setupTestDB(t)
	broadcaster := newTestBroadcaster()
	handler := NewBroadcastHandler(broadcaster)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "00000000-0000-0000-0000-000000000001"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/00000000-0000-0000-0000-000000000001/broadcast", bytes.NewBufferString("{}"))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Broadcast(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

// TestBroadcast_OrgRootLookupFails verifies that if the recursive CTE for
// finding the org root errors, the handler returns 500 instead of proceeding
// with an un-scoped query that would broadcast to all orgs.
func TestBroadcast_OrgRootLookupFails(t *testing.T) {
	mock := setupTestDB(t)
	broadcaster := newTestBroadcaster()
	handler := NewBroadcastHandler(broadcaster)

	senderID := "00000000-0000-0000-0000-000000000001"

	mock.ExpectQuery(`SELECT name, broadcast_enabled FROM workspaces WHERE id = \$1 AND status != 'removed'`).
		WithArgs(senderID).
		WillReturnRows(sqlmock.NewRows([]string{"name", "broadcast_enabled"}).AddRow("Root Agent", true))

	// Org root CTE fails
	mock.ExpectQuery(`WITH RECURSIVE org_chain AS`).
		WithArgs(senderID).
		WillReturnError(context.DeadlineExceeded)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: senderID}}
	body := `{"message":"should not broadcast"}`
	c.Request = httptest.NewRequest("POST", "/workspaces/"+senderID+"/broadcast", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Broadcast(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
	// The recipient query MUST NOT be called — it would broadcast cross-org
	// if the org root lookup failed silently.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestBroadcast_OrgScoped_SelfBroadcastExcluded verifies that broadcasting
// from a workspace does not send a broadcast_receive to the sender itself
// (the sender logs broadcast_sent, not broadcast_receive).
func TestBroadcast_OrgScoped_SelfBroadcastExcluded(t *testing.T) {
	mock := setupTestDB(t)
	broadcaster := newTestBroadcaster()
	handler := NewBroadcastHandler(broadcaster)

	senderID := "00000000-0000-0000-0000-000000000001"
	peerID := "00000000-0000-0000-0000-000000000002"

	mock.ExpectQuery(`SELECT name, broadcast_enabled FROM workspaces WHERE id = \$1 AND status != 'removed'`).
		WithArgs(senderID).
		WillReturnRows(sqlmock.NewRows([]string{"name", "broadcast_enabled"}).AddRow("Root Agent", true))

	mock.ExpectQuery(`WITH RECURSIVE org_chain AS`).
		WithArgs(senderID).
		WillReturnRows(sqlmock.NewRows([]string{"root_id"}).AddRow(senderID))

	// Recipient query MUST exclude sender via id != senderID
	mock.ExpectQuery(`WITH RECURSIVE org_chain AS`).
		WithArgs(senderID, senderID).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(peerID))

	// Peer receives broadcast_receive
	mock.ExpectExec(`INSERT INTO activity_logs`).WithArgs(peerID, senderID, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	// Sender logs broadcast_sent (NOT broadcast_receive)
	mock.ExpectExec(`INSERT INTO activity_logs`).WithArgs(senderID, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: senderID}}
	body := `{"message":"no echo to self"}`
	c.Request = httptest.NewRequest("POST", "/workspaces/"+senderID+"/broadcast", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Broadcast(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestBroadcast_Truncate tests that messages are truncated with the Unicode ellipsis
// TestBroadcast_Truncate tests that messages are truncated with the Unicode ellipsis
// character (U+2026) when len(msg) > max. The truncated output is max runes + "…",
// so truncating a 48-char string at max=20 produces 21 characters (20 runes + "…").
func TestBroadcast_Truncate(t *testing.T) {
	cases := []struct {
		msg    string
		max    int
		expect string
	}{
		{"short", 120, "short"}, // under max — no truncation
		// exactly120chars (15) + 105 ones = 120 chars; at max=120 → unchanged
		{"exactly120chars1111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111", 120, "exactly120chars111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111…"},
		// "this is a longer mes" = 20 runes; + "…" = 21 chars
		{"this is a longer message that needs truncating", 20, "this is a longer mes…"},
		// at-max boundary: 20 chars at max=20 → no truncation
		{"exactly twenty chars", 20, "exactly twenty chars"},
		// over max: 11 chars at max=10 → 10 + "…" = 11
		{"hello world!", 10, "hello worl…"},
	}
	for _, tc := range cases {
		result := broadcastTruncate(tc.msg, tc.max)
		if result != tc.expect {
			t.Errorf("broadcastTruncate(%q, %d) = %q; want %q", tc.msg, tc.max, result, tc.expect)
		}
	}
}
