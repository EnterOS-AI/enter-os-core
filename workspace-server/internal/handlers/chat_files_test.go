package handlers

// Unit tests for chat_files.go.
//
// Upload (HTTP-forward, RFC #2312 PR-C): exercised against an httptest
// mock workspace + sqlmock-backed db.DB. The platform-side handler is
// now a streaming proxy; assertions focus on:
//   * input validation (400 on bad workspace id)
//   * resolution failures (404 missing row, 503 missing secret/url)
//   * forward shape (Authorization, Content-Type, body)
//   * pass-through of the workspace's status + body
//
// Path-safety + sanitization that lived on the platform pre-#2312 is
// now the workspace-side handler's concern; covered in the Python
// suite (workspace/tests/test_internal_chat_uploads.py).

import (
	"bytes"
	"database/sql"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// makeUploadRequest builds a gin context for POST /workspaces/:id/chat/uploads
// with the given multipart body. The recorder is returned so callers can
// assert status + body after invoking h.Upload(c).
func makeUploadRequest(t *testing.T, workspaceID string, body *bytes.Buffer, contentType string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: workspaceID}}
	req := httptest.NewRequest("POST", "/workspaces/"+workspaceID+"/chat/uploads", body)
	req.Header.Set("Content-Type", contentType)
	c.Request = req
	return c, w
}

// uploadFixture builds a minimal multipart/form-data body with a single
// `files` part. The exact bytes don't matter for proxy tests — only that
// the workspace receives the same boundary + headers we sent.
func uploadFixture(t *testing.T) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("files", "fixture.txt")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	_, _ = fw.Write([]byte("fixture-payload"))
	mw.Close()
	return &buf, mw.FormDataContentType()
}

// expectURL stubs the SELECT that resolves the workspace's url +
// delivery_mode. Defaults delivery_mode to "push" — most tests don't
// care about the mode and just want a URL to forward to. Use
// expectURLAndMode when the test needs a specific mode (e.g. the
// poll-mode 422 path).
func expectURL(mock sqlmock.Sqlmock, workspaceID, url string) {
	expectURLAndMode(mock, workspaceID, url, "push")
}

// expectURLAndMode is the explicit form for tests that need to
// exercise the delivery_mode branch (e.g. poll-mode workspaces get
// a 422 instead of a 503 when URL is empty — the platform can't
// dispatch to a non-push workspace at all).
func expectURLAndMode(mock sqlmock.Sqlmock, workspaceID, url, mode string) {
	mock.ExpectQuery(`SELECT COALESCE\(url, ''\), delivery_mode FROM workspaces WHERE id = \$1`).
		WithArgs(workspaceID).
		WillReturnRows(sqlmock.NewRows([]string{"url", "delivery_mode"}).AddRow(url, mode))
}

// expectURLNullMode is the production-observed shape: external runtime
// workspaces (molecule-sdk-python on user infra) register with
// delivery_mode = NULL, not "poll". Caught 2026-05-04 — the narrow
// "poll" check missed three of three real workspaces in user reports.
func expectURLNullMode(mock sqlmock.Sqlmock, workspaceID, url string) {
	mock.ExpectQuery(`SELECT COALESCE\(url, ''\), delivery_mode FROM workspaces WHERE id = \$1`).
		WithArgs(workspaceID).
		WillReturnRows(sqlmock.NewRows([]string{"url", "delivery_mode"}).AddRow(url, nil))
}

// expectURLMissing stubs the SELECT to return sql.ErrNoRows.
func expectURLMissing(mock sqlmock.Sqlmock, workspaceID string) {
	mock.ExpectQuery(`SELECT COALESCE\(url, ''\), delivery_mode FROM workspaces WHERE id = \$1`).
		WithArgs(workspaceID).
		WillReturnError(sql.ErrNoRows)
}

// expectInboundSecret stubs the SELECT performed by ReadPlatformInboundSecret.
func expectInboundSecret(mock sqlmock.Sqlmock, workspaceID string, secret interface{}) {
	mock.ExpectQuery(`SELECT platform_inbound_secret FROM workspaces WHERE id = \$1`).
		WithArgs(workspaceID).
		WillReturnRows(sqlmock.NewRows([]string{"platform_inbound_secret"}).AddRow(secret))
}

func TestChatUpload_InvalidWorkspaceID(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	h := NewChatFilesHandler(NewTemplatesHandler(t.TempDir(), nil))

	c, w := makeUploadRequest(t, "not-a-uuid", &bytes.Buffer{}, "")
	h.Upload(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 on invalid workspace id, got %d: %s", w.Code, w.Body.String())
	}
}

func TestChatUpload_WorkspaceNotInDB(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	wsID := "00000000-0000-0000-0000-000000000099"
	expectURLMissing(mock, wsID)

	h := NewChatFilesHandler(NewTemplatesHandler(t.TempDir(), nil))
	body, ct := uploadFixture(t)
	c, w := makeUploadRequest(t, wsID, body, ct)
	h.Upload(c)

	// QueryRow returning sql.ErrNoRows surfaces as 404. The validate-id
	// step already passed; this is the next layer.
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 when workspace row missing, got %d: %s", w.Code, w.Body.String())
	}
}

// TestChatUpload_NoInboundSecret_LazyHeal pins the lazy-heal flow
// added 2026-04-30 alongside the SaaS shared-prepare refactor:
//
//   1. Reading the workspace's platform_inbound_secret returns NULL
//      (legacy row from before RFC #2312).
//   2. Handler MUST call wsauth.IssuePlatformInboundSecret (an UPDATE
//      on the workspaces row) to backfill the secret, so the next
//      upload after the workspace's heartbeat picks it up succeeds
//      without operator action.
//   3. Response is 503 with retry_after_seconds=30 — the workspace's
//      local /configs/.platform_inbound_secret is also empty, so the
//      forward this request would do still fails. The user retries
//      after the next register response delivers the new secret.
//
// Pre-fix (before the lazy-heal): handlers returned 503 with
// "Reprovision the workspace" — accurate, but every legacy workspace
// would 503 forever until ops manually triggered a reprovision.
func TestChatUpload_NoInboundSecret_LazyHeal(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	// Legacy row: URL set but platform_inbound_secret is NULL.
	wsID := "00000000-0000-0000-0000-000000000041"
	expectURL(mock, wsID, "http://127.0.0.1:1")
	expectInboundSecret(mock, wsID, nil) // NULL — triggers lazy-heal
	// Lazy-heal mint MUST land. If this expectation isn't matched,
	// the upload handler skipped the backfill and ops would have to
	// manually reprovision every legacy workspace.
	mock.ExpectExec(`UPDATE workspaces SET platform_inbound_secret = \$1 WHERE id = \$2`).
		WithArgs(sqlmock.AnyArg(), wsID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	h := NewChatFilesHandler(NewTemplatesHandler(t.TempDir(), nil))
	body, ct := uploadFixture(t)
	c, w := makeUploadRequest(t, wsID, body, ct)
	h.Upload(c)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when platform_inbound_secret missing, got %d: %s", w.Code, w.Body.String())
	}
	// Lazy-heal-success body steers the user to retry; the failure
	// body steers them to reprovision. Distinguishing them pins which
	// branch ran.
	if !strings.Contains(w.Body.String(), "retry") {
		t.Errorf("expected lazy-heal success response (retry hint), got: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "30") {
		t.Errorf("expected retry_after_seconds=30 in body, got: %s", w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met — lazy-heal mint did NOT run, regression of #2312 backfill: %v", err)
	}
}

// TestChatUpload_NoInboundSecret_LazyHealFailure pins the alternate
// branch: the platform_inbound_secret is NULL AND the lazy-heal mint
// itself fails (e.g. DB unreachable). Handler must surface the
// reprovision-steering error rather than silently swallowing.
func TestChatUpload_NoInboundSecret_LazyHealFailure(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	wsID := "00000000-0000-0000-0000-000000000042"
	expectURL(mock, wsID, "http://127.0.0.1:1")
	expectInboundSecret(mock, wsID, nil) // NULL — triggers lazy-heal
	mock.ExpectExec(`UPDATE workspaces SET platform_inbound_secret = \$1 WHERE id = \$2`).
		WithArgs(sqlmock.AnyArg(), wsID).
		WillReturnError(sql.ErrConnDone) // mint fails

	h := NewChatFilesHandler(NewTemplatesHandler(t.TempDir(), nil))
	body, ct := uploadFixture(t)
	c, w := makeUploadRequest(t, wsID, body, ct)
	h.Upload(c)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when lazy-heal fails, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "RFC #2312") {
		t.Errorf("expected detail to reference RFC #2312 on lazy-heal failure, got: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Reprovision") {
		t.Errorf("expected reprovision hint on mint failure, got: %s", w.Body.String())
	}
}

func TestChatUpload_NoURL(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	// Workspace registered (push-mode) but URL hasn't been reported
	// yet (mid-boot). 503 + "not registered yet" is the right surface — the
	// canvas client can retry after the next heartbeat picks up the URL.
	// Push mode is the only branch that produces 503; everything else
	// (poll, NULL, empty) gets 422 because no amount of waiting helps.
	wsID := "00000000-0000-0000-0000-000000000042"
	expectURLAndMode(mock, wsID, "", "push")

	h := NewChatFilesHandler(NewTemplatesHandler(t.TempDir(), nil))
	body, ct := uploadFixture(t)
	c, w := makeUploadRequest(t, wsID, body, ct)
	h.Upload(c)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when workspace url empty (push mode), got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "not registered yet") {
		t.Errorf("expected transient-state error message, got: %s", w.Body.String())
	}
}

// TestChatUpload_PollModeEmptyURL pins the 422 distinguisher: a
// poll-mode workspace has no URL by design, so chat upload (which is
// HTTP-forward to the workspace) cannot succeed by retrying. Returning
// 503 here would loop the canvas client forever; 422 + an actionable
// message tells the user what to do.
func TestChatUpload_PollModeEmptyURL(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	wsID := "00000000-0000-0000-0000-000000000099"
	expectURLAndMode(mock, wsID, "", "poll")

	h := NewChatFilesHandler(NewTemplatesHandler(t.TempDir(), nil))
	body, ct := uploadFixture(t)
	c, w := makeUploadRequest(t, wsID, body, ct)
	h.Upload(c)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for poll-mode upload, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "push") {
		t.Errorf("expected error to suggest push mode, got: %s", w.Body.String())
	}
}

// TestChatUpload_NullModeEmptyURL — production-observed 2026-05-04:
// external-runtime workspaces (molecule-sdk-python on user infra)
// register with delivery_mode = NULL, not "poll". The earlier narrow
// poll-only check fell through to the misleading 503. The fix is the
// inverse-of-push test: anything not exactly "push" with empty URL
// can't dispatch and gets the actionable 422.
//
// Three of three external workspaces in the user's tenant had this
// shape (home hermes / runner mac mini / mac laptop, all
// runtime=external + url='' + delivery_mode=NULL).
func TestChatUpload_NullModeEmptyURL(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	wsID := "30ba7f0b-b303-4a20-aefe-3a4a675b8aa4" // user's "mac laptop"
	expectURLNullMode(mock, wsID, "")

	h := NewChatFilesHandler(NewTemplatesHandler(t.TempDir(), nil))
	body, ct := uploadFixture(t)
	c, w := makeUploadRequest(t, wsID, body, ct)
	h.Upload(c)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for null-delivery-mode upload, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "callback URL") {
		t.Errorf("expected error to mention callback URL, got: %s", w.Body.String())
	}
}

// captured snapshots everything the forwarder sent to the workspace so
// we can assert auth + body + content-type forwarded correctly.
type captured struct {
	authorization string
	contentType   string
	method        string
	path          string
	body          []byte
}

func newCapturingWorkspace(t *testing.T, status int, response string) (*httptest.Server, *captured) {
	t.Helper()
	cap := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.authorization = r.Header.Get("Authorization")
		cap.contentType = r.Header.Get("Content-Type")
		cap.method = r.Method
		cap.path = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		cap.body = body

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(srv.Close)
	return srv, cap
}

func TestChatUpload_ForwardsToWorkspace_HappyPath(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	srv, captured := newCapturingWorkspace(t, http.StatusOK, `{"files":[{"uri":"workspace:/workspace/.molecule/chat-uploads/abc-fixture.txt","name":"fixture.txt","size":15}]}`)

	wsID := "00000000-0000-0000-0000-000000000043"
	expectURL(mock, wsID, srv.URL)
	expectInboundSecret(mock, wsID, "super-secret-123")

	h := NewChatFilesHandler(NewTemplatesHandler(t.TempDir(), nil))
	body, ct := uploadFixture(t)
	c, w := makeUploadRequest(t, wsID, body, ct)
	h.Upload(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 from happy forward, got %d: %s", w.Code, w.Body.String())
	}
	if captured.method != "POST" {
		t.Errorf("expected POST, got %s", captured.method)
	}
	if captured.path != "/internal/chat/uploads/ingest" {
		t.Errorf("expected /internal/chat/uploads/ingest, got %s", captured.path)
	}
	if captured.authorization != "Bearer super-secret-123" {
		t.Errorf("expected secret in Authorization header, got %q", captured.authorization)
	}
	if !strings.HasPrefix(captured.contentType, "multipart/form-data") {
		t.Errorf("expected multipart Content-Type forwarded, got %q", captured.contentType)
	}
	// Body shape: must contain the multipart-encoded fixture content.
	if !bytes.Contains(captured.body, []byte("fixture-payload")) {
		t.Errorf("expected body to contain fixture payload, got %d bytes", len(captured.body))
	}
	// Response body streamed back unchanged.
	if !strings.Contains(w.Body.String(), "fixture.txt") {
		t.Errorf("expected workspace response forwarded back, got: %s", w.Body.String())
	}
}

func TestChatUpload_ForwardsErrorStatusUnchanged(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	// Workspace returns 413 with its standard "exceeds per-file limit"
	// shape. Platform must propagate, NOT remap to 500.
	srv, _ := newCapturingWorkspace(t, http.StatusRequestEntityTooLarge, `{"error":"big.bin exceeds per-file limit (25 MB)"}`)

	wsID := "00000000-0000-0000-0000-000000000044"
	expectURL(mock, wsID, srv.URL)
	expectInboundSecret(mock, wsID, "tok")

	h := NewChatFilesHandler(NewTemplatesHandler(t.TempDir(), nil))
	body, ct := uploadFixture(t)
	c, w := makeUploadRequest(t, wsID, body, ct)
	h.Upload(c)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413 propagated unchanged, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "exceeds per-file limit") {
		t.Errorf("expected workspace's 413 body verbatim, got: %s", w.Body.String())
	}
}

func TestChatUpload_WorkspaceUnreachable(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	wsID := "00000000-0000-0000-0000-000000000045"
	// 127.0.0.1:1 — port 1 has no listener → connect refused.
	expectURL(mock, wsID, "http://127.0.0.1:1")
	expectInboundSecret(mock, wsID, "tok")

	h := NewChatFilesHandler(NewTemplatesHandler(t.TempDir(), nil))
	body, ct := uploadFixture(t)
	c, w := makeUploadRequest(t, wsID, body, ct)
	h.Upload(c)

	// Connect-refused → BadGateway. NOT 500 — the platform itself is
	// fine; the upstream is broken.
	if w.Code != http.StatusBadGateway {
		t.Errorf("expected 502 on workspace unreachable, got %d: %s", w.Code, w.Body.String())
	}
}

func TestChatDownload_InvalidPath(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	h := NewChatFilesHandler(NewTemplatesHandler(t.TempDir(), nil))

	cases := []struct {
		name, path, wantSubstr string
	}{
		{"empty", "", "path query required"},
		{"relative", "workspace/foo.txt", "must be absolute"},
		{"wrong root", "/etc/passwd", "must be under"},
		{"traversal", "/workspace/../etc/passwd", "invalid path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Params = gin.Params{{Key: "id", Value: "00000000-0000-0000-0000-000000000001"}}
			req := httptest.NewRequest("GET", "/workspaces/xxx/chat/download?path="+tc.path, nil)
			c.Request = req

			h.Download(c)

			if w.Code != http.StatusBadRequest {
				t.Errorf("expected 400 for %s, got %d: %s", tc.name, w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.wantSubstr) {
				t.Errorf("expected error to contain %q, got: %s", tc.wantSubstr, w.Body.String())
			}
		})
	}
}

func TestContentDispositionAttachment_Escapes(t *testing.T) {
	cases := []struct {
		name, input, wantSubstr string
	}{
		{
			name:       "plain ASCII passes through",
			input:      "report.pdf",
			wantSubstr: `filename="report.pdf"`,
		},
		{
			name:       "double-quote is backslash-escaped",
			input:      `weird".pdf`,
			wantSubstr: `filename="weird\".pdf"`,
		},
		{
			name:       "CR and LF dropped to prevent header injection",
			input:      "bad\r\nX-Leak: 1\r\n.txt",
			wantSubstr: `filename="badX-Leak: 1.txt"`,
		},
		{
			name:       "non-ASCII emits filename* percent-encoded",
			input:      "résumé.pdf",
			wantSubstr: "filename*=UTF-8''r%C3%A9sum%C3%A9.pdf",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := contentDispositionAttachment(tc.input)
			if !strings.Contains(got, tc.wantSubstr) {
				t.Errorf("contentDispositionAttachment(%q) = %q, missing substring %q", tc.input, got, tc.wantSubstr)
			}
			// Must never contain a bare CR or LF — either would end the header.
			if strings.ContainsAny(got, "\r\n") {
				t.Errorf("header contains CR/LF: %q", got)
			}
		})
	}
}

// makeDownloadRequest builds a gin context for GET /workspaces/:id/chat/download
// with the given path query param.
func makeDownloadRequest(t *testing.T, workspaceID, path string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: workspaceID}}
	c.Request = httptest.NewRequest("GET", "/workspaces/"+workspaceID+"/chat/download?path="+path, nil)
	return c, w
}

func TestChatDownload_WorkspaceNotInDB(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	wsID := "00000000-0000-0000-0000-000000000099"
	mock.ExpectQuery(`SELECT COALESCE\(url, ''\) FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnError(sql.ErrNoRows)

	h := NewChatFilesHandler(NewTemplatesHandler(t.TempDir(), nil))
	c, w := makeDownloadRequest(t, wsID, "/workspace/foo.txt")
	h.Download(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 when workspace row missing, got %d", w.Code)
	}
}

// TestChatDownload_NoInboundSecret_LazyHeal — same lazy-heal flow
// as TestChatUpload_NoInboundSecret_LazyHeal but on the Download
// handler. Pinned separately because Upload + Download have
// independent code paths into ReadPlatformInboundSecret; a partial
// regression that healed Upload but skipped Download is the kind of
// drift we want to fail the test, not ship.
func TestChatDownload_NoInboundSecret_LazyHeal(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	wsID := "00000000-0000-0000-0000-000000000051"
	expectURL(mock, wsID, "http://127.0.0.1:1")
	expectInboundSecret(mock, wsID, nil)
	mock.ExpectExec(`UPDATE workspaces SET platform_inbound_secret = \$1 WHERE id = \$2`).
		WithArgs(sqlmock.AnyArg(), wsID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	h := NewChatFilesHandler(NewTemplatesHandler(t.TempDir(), nil))
	c, w := makeDownloadRequest(t, wsID, "/workspace/foo.txt")
	h.Download(c)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when platform_inbound_secret missing, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "retry") {
		t.Errorf("expected lazy-heal success response (retry hint), got: %s", w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met — Download lazy-heal mint did NOT run: %v", err)
	}
}

func TestChatDownload_NoInboundSecret_LazyHealFailure(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	wsID := "00000000-0000-0000-0000-000000000052"
	expectURL(mock, wsID, "http://127.0.0.1:1")
	expectInboundSecret(mock, wsID, nil)
	mock.ExpectExec(`UPDATE workspaces SET platform_inbound_secret = \$1 WHERE id = \$2`).
		WithArgs(sqlmock.AnyArg(), wsID).
		WillReturnError(sql.ErrConnDone)

	h := NewChatFilesHandler(NewTemplatesHandler(t.TempDir(), nil))
	c, w := makeDownloadRequest(t, wsID, "/workspace/foo.txt")
	h.Download(c)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when lazy-heal fails, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "RFC #2312") {
		t.Errorf("expected detail to reference RFC #2312 on lazy-heal failure, got: %s", w.Body.String())
	}
}

func TestChatDownload_ForwardsToWorkspace_HappyPath(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	body := []byte("file-contents-here\nmultiline\n")
	cap := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.authorization = r.Header.Get("Authorization")
		cap.method = r.Method
		cap.path = r.URL.Path
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Disposition", `attachment; filename="report.txt"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	wsID := "00000000-0000-0000-0000-000000000052"
	expectURL(mock, wsID, srv.URL)
	expectInboundSecret(mock, wsID, "the-secret")

	h := NewChatFilesHandler(NewTemplatesHandler(t.TempDir(), nil))
	c, w := makeDownloadRequest(t, wsID, "/workspace/report.txt")
	h.Download(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if cap.authorization != "Bearer the-secret" {
		t.Errorf("expected secret in Authorization header, got %q", cap.authorization)
	}
	if cap.method != "GET" {
		t.Errorf("expected GET, got %s", cap.method)
	}
	if cap.path != "/internal/file/read" {
		t.Errorf("expected /internal/file/read, got %s", cap.path)
	}
	if got := w.Header().Get("Content-Type"); got != "text/plain" {
		t.Errorf("Content-Type not forwarded: %q", got)
	}
	if got := w.Header().Get("Content-Disposition"); got != `attachment; filename="report.txt"` {
		t.Errorf("Content-Disposition not forwarded: %q", got)
	}
	if got := w.Body.Bytes(); !bytes.Equal(got, body) {
		t.Errorf("body mismatch: got %q, want %q", got, body)
	}
}

func TestChatDownload_404FromWorkspacePropagated(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"file not found"}`))
	}))
	t.Cleanup(srv.Close)

	wsID := "00000000-0000-0000-0000-000000000053"
	expectURL(mock, wsID, srv.URL)
	expectInboundSecret(mock, wsID, "tok")

	h := NewChatFilesHandler(NewTemplatesHandler(t.TempDir(), nil))
	c, w := makeDownloadRequest(t, wsID, "/workspace/missing.txt")
	h.Download(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 propagated, got %d", w.Code)
	}
}
