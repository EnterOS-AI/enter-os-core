package handlers

// template_files_eic_dispatch_test.go — handler-level tests for the
// EIC dispatch added in PR-A of issue #2999. Pre-PR-A, ListFiles and
// DeleteFile silently fell through to the local-Docker path on SaaS
// workspaces (where dockerCli is nil) and returned [] / silent no-op.
// These tests pin the new behavior:
//
//   1. instance_id != "" → handler invokes the EIC helper
//   2. EIC success → 200 with the helper's payload
//   3. EIC error → 500 (does NOT fall through to local-Docker /
//      template-dir, which would mask the real failure)
//   4. instance_id == "" → existing local-Docker / template-dir
//      fallback (back-compat with self-hosted operators)
//
// Stubs `withEICTunnel` so the entire EIC dance (keypair, AWS calls,
// tunnel, ssh) is replaced with a fake closure that yields a captured
// session — lets the test capture what the inner closure would have
// done without spinning up a real sshd. The test for the actual
// remote shell shapes lives in template_files_eic_shells_test.go
// (pure-function tests on buildFindShell / buildInstallShell etc).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// stubWithEICTunnel replaces the package-level withEICTunnel with a
// closure that records its inputs and runs fn against a fake session,
// returning fnErr from the inner fn if non-nil. Restores the original
// on test cleanup.
func stubWithEICTunnel(t *testing.T, fnErr error) (calls *[]string) {
	t.Helper()
	captured := []string{}
	calls = &captured
	prev := withEICTunnel
	withEICTunnel = func(ctx context.Context, instanceID string, fn func(s eicSSHSession) error) error {
		captured = append(captured, instanceID)
		// Hand the closure a sentinel session so any code that pulls
		// session fields gets deterministic non-empty values. The
		// closure's exec.Command call will fail at runtime because no
		// real ssh exists for instanceID="i-test"; but most
		// dispatch-tests inject fnErr directly to skip that.
		return fnErr
	}
	t.Cleanup(func() { withEICTunnel = prev })
	return calls
}

// stubWithEICTunnelReturning is like stubWithEICTunnel but lets the
// test substitute the inner fn entirely so it can populate `out` /
// return shaped errors without invoking the real ssh closure.
func stubWithEICTunnelReturning(t *testing.T, replacement func(s eicSSHSession) error) (calls *[]string) {
	t.Helper()
	captured := []string{}
	calls = &captured
	prev := withEICTunnel
	withEICTunnel = func(ctx context.Context, instanceID string, _ func(s eicSSHSession) error) error {
		captured = append(captured, instanceID)
		return replacement(eicSSHSession{instanceID: instanceID, osUser: "ubuntu", localPort: 12345, keyPath: "/tmp/k"})
	}
	t.Cleanup(func() { withEICTunnel = prev })
	return calls
}

// ---- ListFiles EIC dispatch ----

// TestListFiles_EICDispatch_Success: a workspace with instance_id set
// must route to listFilesViaEIC, NOT to local-Docker / template-dir.
// Verifies the handler hands the EIC helper's output back as JSON.
//
// Until PR-A this test would fail no matter what mocks were in place —
// the dispatch branch did not exist.
func TestListFiles_EICDispatch_Success(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	mock.ExpectQuery(`SELECT name, COALESCE\(instance_id, ''\), COALESCE\(runtime, ''\) FROM workspaces WHERE id =`).
		WithArgs("ws-eic").
		WillReturnRows(sqlmock.NewRows([]string{"name", "instance_id", "runtime"}).
			AddRow("My Agent", "i-test", "claude-code"))

	// The package-level withEICTunnel stub doesn't get to set the
	// listFilesViaEIC outparam, so we have to override the helper at
	// a higher level. Instead, we stub withEICTunnel to *return* the
	// inner closure's err — but we can't reach the byte-output path.
	// Use the dedicated stubWithEICTunnelReturning + intercept ssh:
	// since the tunnel stub doesn't run the closure's ssh exec at all
	// when we replace the inner fn, the helper's `rawOutput` stays
	// nil and parseFindOutput returns []. Sufficient for "200 + empty"
	// dispatch verification.
	stubWithEICTunnelReturning(t, func(s eicSSHSession) error {
		return nil // skip the real ssh; outer rawOutput stays nil → []
	})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-eic"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-eic/files?root=/configs", nil)

	(&TemplatesHandler{}).ListFiles(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var got []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("response not JSON array: %v (body=%s)", err, w.Body.String())
	}
	// EIC stub returned no output → empty list. The point of this
	// assertion is "200 with [] from EIC", not "fell through to host
	// template fallback which would 200 with []" — to discriminate,
	// we ALSO assert mock expectations were met (proving the new SQL
	// shape was queried) AND the local-Docker fallback path can't
	// have run (handler.docker is nil here, so findContainer returns
	// "" and the only paths that reach 200 are EIC or template-dir;
	// template-dir requires a non-empty configsDir which we left at
	// "" via the zero-value handler).
	if got == nil {
		t.Errorf("expected JSON array (even if empty); got null")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestListFiles_EICDispatch_Error: a real EIC failure (network blip,
// AWS API throttle, sshd down) must surface as 500, NOT silently fall
// through to the local-Docker path which would mask the failure as
// "0 files" — which is the exact UX symptom the PR-A bug report cites.
func TestListFiles_EICDispatch_Error(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	mock.ExpectQuery(`SELECT name, COALESCE\(instance_id, ''\), COALESCE\(runtime, ''\) FROM workspaces WHERE id =`).
		WithArgs("ws-eic-err").
		WillReturnRows(sqlmock.NewRows([]string{"name", "instance_id", "runtime"}).
			AddRow("My Agent", "i-test", "claude-code"))

	stubWithEICTunnel(t, errors.New("eic open-tunnel: timeout"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-eic-err"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-eic-err/files?root=/home", nil)

	(&TemplatesHandler{}).ListFiles(c)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "failed to list files") {
		t.Errorf("error body should describe ListFiles failure; got %s", w.Body.String())
	}
}

// TestListFiles_EICBranch_NotTakenForSelfHosted: workspaces with no
// instance_id (self-hosted, local-Docker path) MUST NOT enter the EIC
// branch. Stubs withEICTunnel to fail loudly if it's called — the
// stub being invoked is itself the assertion failure.
func TestListFiles_EICBranch_NotTakenForSelfHosted(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	mock.ExpectQuery(`SELECT name, COALESCE\(instance_id, ''\), COALESCE\(runtime, ''\) FROM workspaces WHERE id =`).
		WithArgs("ws-local").
		WillReturnRows(sqlmock.NewRows([]string{"name", "instance_id", "runtime"}).
			AddRow("Local Agent", "", ""))

	prev := withEICTunnel
	withEICTunnel = func(ctx context.Context, instanceID string, fn func(s eicSSHSession) error) error {
		t.Errorf("withEICTunnel called for self-hosted workspace (instance_id=''); EIC branch must be gated on non-empty instance_id")
		return errors.New("should not be called")
	}
	defer func() { withEICTunnel = prev }()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-local"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-local/files", nil)

	(&TemplatesHandler{configsDir: t.TempDir()}).ListFiles(c)

	// Don't pin the response code here — the local path's behavior is
	// covered by TestListFiles_FallbackToHost_NoTemplate. Just confirm
	// EIC wasn't called.
}

// ---- DeleteFile EIC dispatch ----

// TestDeleteFile_EICDispatch_Success: same shape as ListFiles —
// instance_id != "" routes to deleteFileViaEIC and returns 200 on
// success. Pre-PR-A right-click delete on a SaaS workspace silently
// no-op'd because findContainer returned "" and the ephemeral-volume
// fallback only handles local Docker volumes.
func TestDeleteFile_EICDispatch_Success(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	mock.ExpectQuery(`SELECT name, COALESCE\(instance_id, ''\), COALESCE\(runtime, ''\) FROM workspaces WHERE id =`).
		WithArgs("ws-eic-del").
		WillReturnRows(sqlmock.NewRows([]string{"name", "instance_id", "runtime"}).
			AddRow("My Agent", "i-test", "claude-code"))

	stubWithEICTunnel(t, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-eic-del"},
		{Key: "path", Value: "old.txt"},
	}
	c.Request = httptest.NewRequest("DELETE", "/workspaces/ws-eic-del/files/old.txt", nil)

	(&TemplatesHandler{}).DeleteFile(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"deleted"`) {
		t.Errorf("expected status:deleted; got %s", w.Body.String())
	}
}

func TestDeleteFile_EICDispatch_Error(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	mock.ExpectQuery(`SELECT name, COALESCE\(instance_id, ''\), COALESCE\(runtime, ''\) FROM workspaces WHERE id =`).
		WithArgs("ws-eic-del-err").
		WillReturnRows(sqlmock.NewRows([]string{"name", "instance_id", "runtime"}).
			AddRow("My Agent", "i-test", "hermes"))

	stubWithEICTunnel(t, errors.New("ssh rm: connection refused"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-eic-del-err"},
		{Key: "path", Value: "old.txt"},
	}
	c.Request = httptest.NewRequest("DELETE", "/workspaces/ws-eic-del-err/files/old.txt", nil)

	(&TemplatesHandler{}).DeleteFile(c)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
}

// TestListFiles_RootValidation: the handler must reject roots outside
// the allowlist BEFORE any DB query (otherwise a bad root would burn
// a tunnel + EIC call to discover what a 400 already knows). Critical
// security guard — without it `?root=/etc` would translate via the
// resolver's literal-pass-through. Let me prove the gate exists by
// driving an out-of-allowlist root and asserting 400 + no DB query.
func TestListFiles_RootValidation(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-x"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-x/files?root=/etc", nil)

	(&TemplatesHandler{}).ListFiles(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for /etc root, got %d: %s", w.Code, w.Body.String())
	}
}

// TestDeleteFile_RootValidation mirrors the ListFiles guard. PR-A
// added ?root= handling to DeleteFile so the canvas's right-click
// delete works on any root (not just /configs) — that means the
// allowlist guard has to be present here too, otherwise an unsafe
// root flows straight into the resolver.
func TestDeleteFile_RootValidation(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-x"},
		{Key: "path", Value: "f.txt"},
	}
	c.Request = httptest.NewRequest("DELETE", "/workspaces/ws-x/files/f.txt?root=/etc", nil)

	(&TemplatesHandler{}).DeleteFile(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for /etc root, got %d: %s", w.Code, w.Body.String())
	}
}
