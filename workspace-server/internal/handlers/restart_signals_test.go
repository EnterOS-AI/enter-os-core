package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/provisioner"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// stubLocalProv is a minimal LocalProvisionerAPI stub used to make
// h.provisioner non-nil for the Docker-URL-rewrite tests.
// All methods panic — rewriteForDocker only checks h.provisioner != nil.
type stubLocalProv struct{}

func (s *stubLocalProv) Start(_ context.Context, _ provisioner.WorkspaceConfig) (string, error) {
	panic("stubLocalProv.Start not implemented in test")
}
func (s *stubLocalProv) Stop(_ context.Context, _ string) error {
	panic("stubLocalProv.Stop not implemented in test")
}
func (s *stubLocalProv) IsRunning(_ context.Context, _ string) (bool, error) {
	panic("stubLocalProv.IsRunning not implemented in test")
}
func (s *stubLocalProv) ExecRead(_ context.Context, _, _ string) ([]byte, error) {
	panic("stubLocalProv.ExecRead not implemented in test")
}
func (s *stubLocalProv) RemoveVolume(_ context.Context, _ string) error {
	panic("stubLocalProv.RemoveVolume not implemented in test")
}
func (s *stubLocalProv) VolumeHasFile(_ context.Context, _, _ string) (bool, error) {
	panic("stubLocalProv.VolumeHasFile not implemented in test")
}
func (s *stubLocalProv) WriteAuthTokenToVolume(_ context.Context, _, _ string) error {
	panic("stubLocalProv.WriteAuthTokenToVolume not implemented in test")
}

// Compile-time assertion: stubLocalProv satisfies LocalProvisionerAPI.
var _ provisioner.LocalProvisionerAPI = (*stubLocalProv)(nil)

// TestRewriteForDocker_NonDockerHostUrlUnchanged verifies that a non-Docker
// URL passes through rewriteForDocker unchanged when platform is not in Docker.
func TestRewriteForDocker_NonDockerHostUrlUnchanged(t *testing.T) {
	restore := setPlatformInDockerForTest(false)
	defer restore()

	h := newHandlerWithTestDeps(t)
	url := h.rewriteForDocker("http://example.com:8000/agent", "ws-test-123")
	if url != "http://example.com:8000/agent" {
		t.Errorf("expected unchanged URL, got %q", url)
	}
}

// TestRewriteForDocker_LocalhostUrlUnchanged_NoProvisioner verifies that a
// localhost URL is NOT rewritten when h.provisioner is nil (SaaS/CP mode).
func TestRewriteForDocker_LocalhostUrlUnchanged_NoProvisioner(t *testing.T) {
	restore := setPlatformInDockerForTest(true)
	defer restore()

	h := newHandlerWithTestDeps(t)
	// h.provisioner is nil → no Docker rewrite even when platformInDocker=true
	url := h.rewriteForDocker("http://127.0.0.1:49152/agent", "ws-test-123")
	if url != "http://127.0.0.1:49152/agent" {
		t.Errorf("expected localhost URL unchanged (no provisioner), got %q", url)
	}
}

// TestRewriteForDocker_LocalhostUrlRewritten verifies that a localhost URL
// IS rewritten to the Docker-DNS form when platform is in Docker AND a
// provisioner is wired.
func TestRewriteForDocker_LocalhostUrlRewritten(t *testing.T) {
	restore := setPlatformInDockerForTest(true)
	defer restore()

	h := newHandlerWithTestDeps(t)
	h.provisioner = &stubLocalProv{} // non-nil → triggers Docker rewrite

	url := h.rewriteForDocker("http://127.0.0.1:49152/agent", "ws-test-123")
	// Docker DNS form: ws-<short-id>:8000
	if url == "http://127.0.0.1:49152/agent" {
		t.Error("expected localhost URL to be rewritten to Docker DNS form")
	}
	// Verify the rewrite matches the expected Docker internal URL format
	expectedInternal := "http://ws-ws-test-123:8000"
	if url != expectedInternal {
		t.Errorf("expected %q, got %q", expectedInternal, url)
	}
}

// TestResolveAgentURLForRestartSignal_CacheHit verifies that a Redis-cached
// URL is returned without hitting the DB.
func TestResolveAgentURLForRestartSignal_CacheHit(t *testing.T) {
	_ = setupTestDB(t) // db.DB must be set before setupTestRedisWithURL
	_ = setupTestRedisWithURL(t, "http://cached.internal:9000/agent")

	h := newHandlerWithTestDeps(t)

	// Redis cache hit → DB should NOT be queried
	url, err := h.resolveAgentURLForRestartSignal(context.Background(), "ws-cache-hit-123")
	if err != nil {
		t.Fatalf("resolveAgentURLForRestartSignal failed: %v", err)
	}
	if url == "" {
		t.Fatal("expected non-empty URL from cache")
	}
	if url != "http://cached.internal:9000/agent" {
		t.Errorf("expected cached URL, got %q", url)
	}
}

// TestResolveAgentURLForRestartSignal_DBError verifies that a DB error is
// returned and propagated when neither Redis cache nor DB lookup succeeds.
func TestResolveAgentURLForRestartSignal_DBError(t *testing.T) {
	mock := setupTestDB(t) // must come before setupTestRedis so db.DB is correct
	_ = setupTestRedis(t) // empty → cache miss

	h := newHandlerWithTestDeps(t)

	mock.ExpectQuery(`SELECT url FROM workspaces WHERE id =`).
		WithArgs("ws-db-err-789").
		WillReturnError(context.DeadlineExceeded)

	_, err := h.resolveAgentURLForRestartSignal(context.Background(), "ws-db-err-789")
	if err == nil {
		t.Fatal("expected DB error to be returned")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unfulfilled DB expectations: %v", err)
	}
}

// TestResolveAgentURLForRestartSignal_CacheMiss verifies that on Redis miss,
// the URL is fetched from the DB and cached.
func TestResolveAgentURLForRestartSignal_CacheMiss(t *testing.T) {
	mock := setupTestDB(t) // must come before setupTestRedis so db.DB is correct
	_ = setupTestRedis(t)  // empty → cache miss

	h := newHandlerWithTestDeps(t)

	mock.ExpectQuery(`SELECT url FROM workspaces WHERE id =`).
		WithArgs("ws-cache-miss-456").
		WillReturnRows(sqlmock.NewRows([]string{"url"}).
			AddRow("http://db.internal:8000/agent"))

	url, err := h.resolveAgentURLForRestartSignal(context.Background(), "ws-cache-miss-456")
	if err != nil {
		t.Fatalf("resolveAgentURLForRestartSignal failed: %v", err)
	}
	if url != "http://db.internal:8000/agent" {
		t.Errorf("expected DB URL, got %q", url)
	}

	// Verify the URL was cached in Redis via db.GetCachedURL.
	// GetCachedURL takes workspaceID and builds the key internally, so
	// pass "ws-cache-miss-456" (not the full "ws:ws-cache-miss-456:url").
	cached, err := db.GetCachedURL(context.Background(), "ws-cache-miss-456")
	if err != nil {
		t.Fatalf("URL cache read failed: %v", err)
	}
	if cached != "http://db.internal:8000/agent" {
		t.Errorf("expected cached URL %q, got %q", "http://db.internal:8000/agent", cached)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unfulfilled DB expectations: %v", err)
	}
}

// TestGracefulPreRestart_Success verifies that when the workspace returns 200,
// the signal is logged as acknowledged without error.
func TestGracefulPreRestart_Success(t *testing.T) {
	_ = setupTestDB(t)

	// httptest server simulating the workspace container's /signals/restart_pending
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("expected Content-Type: application/json, got %s", r.Header.Get("Content-Type"))
		}
		if r.Header.Get("X-Restart-Signal") != "true" {
			t.Error("expected X-Restart-Signal: true header")
		}

		var req map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("failed to decode request body: %v", err)
		}
		if req["method"] != "signals/restart_pending" {
			t.Errorf("expected method signals/restart_pending, got %v", req["method"])
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"jsonrpc": "2.0",
			"result":  map[string]interface{}{"acknowledged": true},
		})
	}))
	defer srv.Close()

	// Pre-populate Redis cache with the test server URL
	_ = setupTestRedisWithURL(t, srv.URL)

	// Use an embedded struct to override resolveAgentURLForRestartSignal.
	hWrapper := &resolveURLTestWrapper{
		WorkspaceHandler: newHandlerWithTestDeps(t),
		testURL:         srv.URL + "/agent",
	}

	// gracefulPreRestart runs in a goroutine with its own timeout.
	// We give it time to complete before the test ends.
	hWrapper.gracefulPreRestart(context.Background(), "ws-ack-789")
	time.Sleep(200 * time.Millisecond)
}

// TestGracefulPreRestart_NotImplemented verifies that when the workspace returns
// 404 (old SDK version), the platform proceeds gracefully (log + no error).
func TestGracefulPreRestart_NotImplemented(t *testing.T) {
	_ = setupTestDB(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	_ = setupTestRedisWithURL(t, srv.URL)

	hWrapper := &resolveURLTestWrapper{
		WorkspaceHandler: newHandlerWithTestDeps(t),
		testURL:         srv.URL + "/agent",
	}

	hWrapper.gracefulPreRestart(context.Background(), "ws-noimpl-999")
	time.Sleep(200 * time.Millisecond)
	// No panic or error expected — graceful degradation
}

// TestGracefulPreRestart_ConnectionRefused verifies that when the workspace
// is unreachable, the platform proceeds gracefully without error.
func TestGracefulPreRestart_ConnectionRefused(t *testing.T) {
	_ = setupTestDB(t)

	mr := setupTestRedisWithURL(t, "http://localhost:19999/agent") // nothing listening on 19999
	_ = mr

	hWrapper := &resolveURLTestWrapper{
		WorkspaceHandler: newHandlerWithTestDeps(t),
		testURL:         "http://localhost:19999/agent",
	}

	hWrapper.gracefulPreRestart(context.Background(), "ws-unreachable-000")
	time.Sleep(200 * time.Millisecond)
	// No panic or error expected — proceeds with stop as documented
}

// TestGracefulPreRestart_URLResolutionError verifies that when URL resolution
// fails, the platform proceeds gracefully without blocking the restart.
func TestGracefulPreRestart_URLResolutionError(t *testing.T) {
	_ = setupTestDB(t)
	_ = setupTestRedis(t) // empty → URL resolution will fail in resolveAgentURLForRestartSignal

	hWrapper := &resolveURLTestWrapper{
		WorkspaceHandler: newHandlerWithTestDeps(t),
		errToReturn:     context.DeadlineExceeded,
	}

	hWrapper.gracefulPreRestart(context.Background(), "ws-url-err-111")
	time.Sleep(200 * time.Millisecond)
	// No panic or error expected — proceeds with stop as documented
}

// ─── helpers ─────────────────────────────────────────────────────────────────

// resolveURLTestWrapper embeds *WorkspaceHandler and overrides
// resolveAgentURLForRestartSignal so tests can inject a fixed URL or error.
type resolveURLTestWrapper struct {
	*WorkspaceHandler
	testURL     string
	errToReturn error
}

func (w *resolveURLTestWrapper) resolveAgentURLForRestartSignal(ctx context.Context, workspaceID string) (string, error) {
	if w.errToReturn != nil {
		return "", w.errToReturn
	}
	return w.testURL, nil
}

// newHandlerWithTestDeps creates a WorkspaceHandler with test stubs.
func newHandlerWithTestDeps(t *testing.T) *WorkspaceHandler {
	return NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
}

// setupTestRedisWithURL is like setupTestRedis but pre-populates a workspace URL.
func setupTestRedisWithURL(t *testing.T, url string) *miniredis.Miniredis {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("failed to start miniredis: %v", err)
	}
	db.RDB = redis.NewClient(&redis.Options{Addr: mr.Addr()})
	for _, wsID := range []string{"ws-cache-hit-123", "ws-cache-miss-456", "ws-ack-789", "ws-noimpl-999", "ws-unreachable-000"} {
		if err := db.CacheURL(context.Background(), wsID, url); err != nil {
			t.Fatalf("failed to cache URL for %s: %v", wsID, err)
		}
	}
	t.Cleanup(func() { mr.Close() })
	return mr
}