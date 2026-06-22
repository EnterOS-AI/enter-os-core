package handlers

// Phase 4b — unit coverage for the gate's activation SCOPE: the default-off
// rollout flag + org-token-only targeting + (core#2574) admin-token
// ALWAYS-gated. These exercise the short-circuit paths that return
// "proceed" BEFORE requireApproval, so they need no DB. The full
// flag-on + org-token + gated → 202 path is covered by the real-Postgres
// approval_gate_integration_test.go.

import (
	"database/sql"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/approvals"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

func TestDestructiveGateEnabled_DefaultOff(t *testing.T) {
	os.Unsetenv("MOLECULE_PLATFORM_APPROVAL_GATE")
	if destructiveGateEnabled() {
		t.Fatal("gate must be OFF by default (no env)")
	}
	for _, v := range []string{"1", "true"} {
		t.Setenv("MOLECULE_PLATFORM_APPROVAL_GATE", v)
		if !destructiveGateEnabled() {
			t.Errorf("%q must enable the gate", v)
		}
	}
	t.Setenv("MOLECULE_PLATFORM_APPROVAL_GATE", "0")
	if destructiveGateEnabled() {
		t.Error(`"0" must keep the gate off`)
	}
}

func TestCallerHoldsOrgToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	if callerHoldsOrgToken(c) {
		t.Error("no org_token_id in context → must be false (workspace/CP caller)")
	}
	c.Set("org_token_id", "tok-abc")
	if !callerHoldsOrgToken(c) {
		t.Error("org_token_id set → must be true (platform-agent / org-admin caller)")
	}
}

// (core#2574) admin-token callers are detected via the caller_is_admin_token
// context key, set by wsauth_middleware.AdminAuth on the Tier 2b ADMIN_TOKEN
// path + Tier 3 workspace-token-fallback path. Without this, the concierge's
// admin-token auth bypassed every gated verb.
func TestCallerIsAdminToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	if callerIsAdminToken(c) {
		t.Error("no caller_is_admin_token in context → must be false")
	}
	c.Set("caller_is_admin_token", true)
	if !callerIsAdminToken(c) {
		t.Error("caller_is_admin_token=true → must be true (admin-token path)")
	}
	// Defensive: wrong type must NOT panic and must return false.
	c2, _ := gin.CreateTestContext(httptest.NewRecorder())
	c2.Set("caller_is_admin_token", "not-a-bool")
	if callerIsAdminToken(c2) {
		t.Error(`caller_is_admin_token="not-a-bool" → must be false (type assertion guard)`)
	}
}

// gateDestructive scope short-circuits. Updated for core#2574: admin-token
// callers are ALWAYS gated (regardless of the rollout flag); org-token callers
// still follow the rollout flag; everyone else bypasses.
func TestGateDestructive_ScopeShortCircuits(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mock := setupTestDB(t)

	// requireApproval sequence for an admin-token caller with a gated action
	// and no pre-existing approval: UPDATE consumes nothing, INSERT returns
	// the new approval id, SELECT parent_id returns nil. (b is nil in the
	// admin-token gate call, so no RecordAndBroadcast query.)
	mock.ExpectQuery(`UPDATE approval_requests SET consumed_at`).
		WillReturnError(sqlmock.ErrCancelled) // not sql.ErrNoRows on purpose — see note below
	_ = mock // referenced for the deferred expectations set inside the call

	newCtx := func(cred string) *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("DELETE", "/x", nil)
		switch cred {
		case "org-token":
			c.Set("org_token_id", "tok")
		case "admin-token":
			c.Set("caller_is_admin_token", true)
		}
		return c
	}

	// flag OFF (default) + org-token + gated action → proceed (rollout dormant).
	os.Unsetenv("MOLECULE_PLATFORM_APPROVAL_GATE")
	if !gateDestructive(newCtx("org-token"), nil, "ws", approvals.ActionSecretWrite, "r", nil) {
		t.Error("flag off + org-token must proceed (gate dormant)")
	}

	// flag ON + NO agent credential (workspace/CP caller) → proceed.
	t.Setenv("MOLECULE_PLATFORM_APPROVAL_GATE", "1")
	if !gateDestructive(newCtx("none"), nil, "ws", approvals.ActionSecretWrite, "r", nil) {
		t.Error("non-agent caller must proceed (normal operation unchanged)")
	}

	// flag ON + org token + NON-gated action → proceed (IsGated short-circuit).
	if !gateDestructive(newCtx("org-token"), nil, "ws", approvals.Action("not_a_gated_action"), "r", nil) {
		t.Error("non-gated action must proceed")
	}
}

// (core#2574) admin-token callers are ALWAYS gated — the rollout flag is
// irrelevant on the admin-token path. The old code would have bypassed; the
// new code MUST fire the gate. This is the regression test for the
// privilege-escalation hole.
func TestGateDestructive_AdminTokenAlwaysGated(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mock := setupTestDB(t)

	// requireApproval sequence for an admin-token caller (gated action,
	// no pre-existing approval). Reusable across BOTH sub-cases below
	// (flag off + flag on) — requireApproval makes the same three calls
	// regardless of the flag.
	expectGateSequence := func() {
		mock.ExpectQuery(`UPDATE approval_requests SET consumed_at`).
			WillReturnError(sql.ErrNoRows)
		mock.ExpectQuery(`WITH existing AS`).
			WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("appr-core2574-1"))
		mock.ExpectQuery(`SELECT parent_id FROM workspaces WHERE id`).
			WillReturnRows(sqlmock.NewRows([]string{"parent_id"}).AddRow(nil))
	}

	newCtx := func() *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/org/tokens", nil)
		c.Set("caller_is_admin_token", true)
		return c
	}

	// Sub-case A: flag OFF (default) + admin-token + gated action → MUST gate.
	// This is the regression for core#2574. Old code bypassed here; new
	// code MUST NOT.
	expectGateSequence()
	os.Unsetenv("MOLECULE_PLATFORM_APPROVAL_GATE")
	if gateDestructive(newCtx(), nil, "ws", approvals.ActionOrgTokenMint, "r", nil) {
		t.Errorf("admin-token + gated action + flag OFF MUST gate (core#2574 privilege-escalation fix). want proceed=false, got proceed=true")
	}

	// Sub-case B: flag ON + admin-token + gated action → still must gate.
	// The flag is irrelevant to admin-token callers.
	expectGateSequence()
	t.Setenv("MOLECULE_PLATFORM_APPROVAL_GATE", "1")
	if gateDestructive(newCtx(), nil, "ws", approvals.ActionOrgTokenMint, "r", nil) {
		t.Errorf("admin-token + gated action + flag ON must gate")
	}
}
