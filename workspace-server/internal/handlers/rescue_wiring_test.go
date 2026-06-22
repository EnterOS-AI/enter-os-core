package handlers

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/models"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/rescue"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// rescueTestHarness makes the otherwise-async rescue capture
// deterministic + observable for handler tests:
//   - rescueDispatch runs synchronously (no goroutine race).
//   - rescueResolveInstanceID returns a fixed instance id.
//   - rescue.RunRemote / rescue.Redact are stubbed so no real EIC/SSH
//     fires; runCalls counts how many remote-command collections ran,
//     which is the proxy for "did the capture fire".
//
// All originals are restored on cleanup.
func rescueTestHarness(t *testing.T, instanceID string) (runCalls *int) {
	t.Helper()
	n := 0
	runCalls = &n

	prevDispatch := rescueDispatch
	rescueDispatch = func(fn func()) { fn() } // synchronous
	prevResolve := rescueResolveInstanceID
	rescueResolveInstanceID = func(_ context.Context, _ string) (string, error) { return instanceID, nil }
	prevRun, prevRedact := rescue.RunRemote, rescue.Redact
	rescue.RunRemote = func(_ context.Context, _ string, _ string) (string, error) { n++; return "out", nil }
	rescue.Redact = func(_ws, c string) string { return c }

	t.Cleanup(func() {
		rescueDispatch = prevDispatch
		rescueResolveInstanceID = prevResolve
		rescue.RunRemote = prevRun
		rescue.Redact = prevRedact
	})
	return runCalls
}

// TestBootstrapFailed_FiresRescueOnFlip — the RFC internal#742 handler
// hook: when BootstrapFailed actually flips a workspace to `failed`
// (affected==1), the rescue capture fires against the resolved instance.
func TestBootstrapFailed_FiresRescueOnFlip(t *testing.T) {
	h, mock := setupBootstrapHandler(t)
	runCalls := rescueTestHarness(t, "i-failed01")

	mock.ExpectExec(`UPDATE workspaces`).
		WithArgs("ws-crashed", sqlmock.AnyArg(), models.StatusFailed).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO structure_events`).
		WithArgs("WORKSPACE_PROVISION_FAILED", "ws-crashed", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-crashed"}}
	c.Request = httptest.NewRequest("POST", "/admin/workspaces/ws-crashed/bootstrap-failed",
		bytes.NewBufferString(`{"error":"codex provider derivation failed","log_tail":"panic"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h.BootstrapFailed(c)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if *runCalls != len(rescueBundleSectionCount()) {
		t.Errorf("rescue capture ran %d remote commands, want %d (one per bundle section)", *runCalls, len(rescueBundleSectionCount()))
	}
}

// TestBootstrapFailed_NoRescueOnNoChange — an already-transitioned
// workspace (affected==0: raced to online, or double-report) is NOT a
// boot-failure verdict here, so the rescue capture must NOT fire.
func TestBootstrapFailed_NoRescueOnNoChange(t *testing.T) {
	h, mock := setupBootstrapHandler(t)
	runCalls := rescueTestHarness(t, "i-online01")

	mock.ExpectExec(`UPDATE workspaces`).
		WithArgs("ws-online", sqlmock.AnyArg(), models.StatusFailed).
		WillReturnResult(sqlmock.NewResult(0, 0)) // already transitioned

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-online"}}
	c.Request = httptest.NewRequest("POST", "/admin/workspaces/ws-online/bootstrap-failed",
		bytes.NewBufferString(`{"error":"late report","log_tail":""}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h.BootstrapFailed(c)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if *runCalls != 0 {
		t.Errorf("rescue capture fired (%d cmds) on a no-change report; it must only fire on a real flip", *runCalls)
	}
}

// rescueBundleSectionCount returns the production rescue bundle section
// list length by running a capture against a counting runner once. It's
// a small indirection so the handler test stays decoupled from the exact
// section set in internal/rescue (which has its own tests).
func rescueBundleSectionCount() []struct{} {
	count := 0
	prevRun, prevRedact := rescue.RunRemote, rescue.Redact
	rescue.RunRemote = func(_ context.Context, _ string, _ string) (string, error) { count++; return "", nil }
	rescue.Redact = func(_ws, c string) string { return c }
	rescue.Capture(context.Background(), rescue.Input{InstanceID: "i-probe", WorkspaceID: "w", OrgID: "o"})
	rescue.RunRemote = prevRun
	rescue.Redact = prevRedact
	return make([]struct{}, count)
}
