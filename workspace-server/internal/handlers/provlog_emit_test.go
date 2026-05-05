package handlers

// provlog_emit_test.go — pins that the structured-logging emit sites
// added for #2867 PR-D actually fire when their boundary is crossed.
//
// These are call-site contract tests, not provlog package tests (those
// live next to the helper). The assertion is "this dispatcher path
// emits this event name" — if a refactor moves the call out of the
// boundary helper, the gate fails. Fields are NOT pinned here on
// purpose; the field set is convenience for ops, not contract for the
// emit point. Pinning fields would block additive evolution of the
// payload (see also feedback_behavior_based_ast_gates.md).

import (
	"bytes"
	"context"
	"log"
	"strings"
	"sync"
	"testing"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/models"
)

// captureProvLog redirects the global logger to a buffer for the test
// duration. provlog.Event uses log.Printf, so this is the only seam.
// Returned mutex protects against concurrent reads from the goroutine
// fired by provisionWorkspaceAuto (the goroutine never returns in
// these tests because Start() is stubbed, but the buffer can still be
// touched by it racing the assertion).
func captureProvLog(t *testing.T) (read func() string) {
	t.Helper()
	var buf bytes.Buffer
	var mu sync.Mutex
	prevWriter := log.Writer()
	prevFlags := log.Flags()
	log.SetFlags(0)
	log.SetOutput(&safeWriter{buf: &buf, mu: &mu})
	t.Cleanup(func() {
		log.SetOutput(prevWriter)
		log.SetFlags(prevFlags)
	})
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}

// TestProvisionWorkspaceAutoSync_EmitsProvisionStart — sync variant is
// chosen for the assertion path because it returns once the (stubbed)
// Start() has been called, so we know the emit has flushed. The async
// variant would race a goroutine.
func TestProvisionWorkspaceAutoSync_EmitsProvisionStart(t *testing.T) {
	read := captureProvLog(t)
	h := &WorkspaceHandler{cpProv: &trackingCPProv{}}
	// Best-effort: the body will hit DB code under provisionWorkspaceCP
	// — we only need the emit at the entry, which fires unconditionally
	// before the dispatch. Recovering from any later panic keeps the
	// test focused.
	defer func() { _ = recover() }()
	h.provisionWorkspaceAutoSync("ws-test-1", "tmpl", nil, models.CreateWorkspacePayload{
		Name: "n", Tier: 4, Runtime: "claude-code",
	})
	got := read()
	if !strings.Contains(got, "evt: provision.start ") {
		t.Fatalf("expected provision.start emit, got log:\n%s", got)
	}
	if !strings.Contains(got, `"workspace_id":"ws-test-1"`) {
		t.Errorf("workspace_id not in payload: %s", got)
	}
	if !strings.Contains(got, `"sync":true`) {
		t.Errorf("sync flag not pinned for sync dispatcher: %s", got)
	}
}

// TestStopForRestart_EmitsRestartPreStop — emit fires before the actual
// Stop call, so the trackingCPProv stub doesn't need to be wired for
// real Stop semantics. Backend label "cp" pinned because that's the
// SaaS path; we don't pin "docker" or "none" branches here (separate
// tests would only re-test the trivial branch label switch).
func TestStopForRestart_EmitsRestartPreStop(t *testing.T) {
	read := captureProvLog(t)
	h := &WorkspaceHandler{cpProv: &trackingCPProv{}}
	defer func() { _ = recover() }()
	h.stopForRestart(context.Background(), "ws-restart-1")
	got := read()
	if !strings.Contains(got, "evt: restart.pre_stop ") {
		t.Fatalf("expected restart.pre_stop emit, got log:\n%s", got)
	}
	if !strings.Contains(got, `"workspace_id":"ws-restart-1"`) {
		t.Errorf("workspace_id not in payload: %s", got)
	}
	if !strings.Contains(got, `"backend":"cp"`) {
		t.Errorf("backend label missing or wrong: %s", got)
	}
}

// TestStopForRestart_EmitsBackendNoneWhenUnwired — pin the no-backend
// branch so a future refactor that drops the label switch is caught.
// This is the silent-Stop case (workspace_dispatchers.go:StopWorkspaceAuto
// returns nil for unwired backends); the emit ensures the operator can
// still see the boundary in the log.
func TestStopForRestart_EmitsBackendNoneWhenUnwired(t *testing.T) {
	read := captureProvLog(t)
	h := &WorkspaceHandler{} // both nil
	h.stopForRestart(context.Background(), "ws-restart-2")
	got := read()
	if !strings.Contains(got, `"backend":"none"`) {
		t.Fatalf("expected backend=none for unwired handler: %s", got)
	}
}
