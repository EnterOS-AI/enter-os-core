package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/approvals"
	"github.com/gin-gonic/gin"
)

// TestGateDestructive_NonGatedPassesThrough verifies a non-gated action skips
// the gate entirely (no DB access, no 202) so handlers whose action isn't in the
// policy map behave exactly as before.
func TestGateDestructive_NonGatedPassesThrough(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/x", nil)

	proceed := gateDestructive(c, newTestBroadcaster(), "ws-1",
		approvals.Action("not_a_gated_action"), "noop", nil)

	if !proceed {
		t.Fatalf("non-gated action must proceed, got proceed=false (status %d)", w.Code)
	}
	if w.Code != http.StatusOK { // CreateTestContext default; nothing written
		t.Errorf("non-gated action wrote a response (status %d), want none", w.Code)
	}
}

// TestApprovalRequestHash_StableAndContextSensitive pins the two properties the
// gate relies on: the same operation hashes identically across calls, and a
// different context yields a different hash (so an approval can't be replayed
// onto a different target).
func TestApprovalRequestHash_StableAndContextSensitive(t *testing.T) {
	a := approvalRequestHash("ws", "delete_workspace", map[string]interface{}{"target": "A", "n": 1})
	aAgain := approvalRequestHash("ws", "delete_workspace", map[string]interface{}{"n": 1, "target": "A"})
	b := approvalRequestHash("ws", "delete_workspace", map[string]interface{}{"target": "B", "n": 1})
	if a != aAgain {
		t.Errorf("hash not stable across equal contexts: %s vs %s", a, aAgain)
	}
	if a == b {
		t.Errorf("hash not context-sensitive: target A and B collided (%s)", a)
	}
}
