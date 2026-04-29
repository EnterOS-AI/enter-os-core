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

// expectURL stubs the SELECT that resolves the workspace's url.
func expectURL(mock sqlmock.Sqlmock, workspaceID, url string) {
	mock.ExpectQuery(`SELECT COALESCE\(url, ''\) FROM workspaces WHERE id = \$1`).
		WithArgs(workspaceID).
		WillReturnRows(sqlmock.NewRows([]string{"url"}).AddRow(url))
}

// expectURLMissing stubs the SELECT to return sql.ErrNoRows.
func expectURLMissing(mock sqlmock.Sqlmock, workspaceID string) {
	mock.ExpectQuery(`SELECT COALESCE\(url, ''\) FROM workspaces WHERE id = \$1`).
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

func TestChatUpload_NoInboundSecret(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	// Legacy row: URL set but platform_inbound_secret is NULL.
	wsID := "00000000-0000-0000-0000-000000000041"
	expectURL(mock, wsID, "http://127.0.0.1:1")
	expectInboundSecret(mock, wsID, nil) // NULL

	h := NewChatFilesHandler(NewTemplatesHandler(t.TempDir(), nil))
	body, ct := uploadFixture(t)
	c, w := makeUploadRequest(t, wsID, body, ct)
	h.Upload(c)

	// 503 with detail steering ops to reprovision. NOT 200, NOT a
	// silent no-bearer forward (which would land as a 401 that the
	// user can't action).
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when platform_inbound_secret missing, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "RFC #2312") {
		t.Errorf("expected detail to reference RFC #2312, got: %s", w.Body.String())
	}
}

func TestChatUpload_NoURL(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	// Workspace registered but URL hasn't been reported yet (mid-boot).
	wsID := "00000000-0000-0000-0000-000000000042"
	expectURL(mock, wsID, "")

	h := NewChatFilesHandler(NewTemplatesHandler(t.TempDir(), nil))
	body, ct := uploadFixture(t)
	c, w := makeUploadRequest(t, wsID, body, ct)
	h.Upload(c)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when workspace url empty, got %d: %s", w.Code, w.Body.String())
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

func TestChatDownload_DockerUnavailable(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	tmplh := NewTemplatesHandler(t.TempDir(), nil) // docker=nil
	h := NewChatFilesHandler(tmplh)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "00000000-0000-0000-0000-000000000001"}}
	req := httptest.NewRequest("GET", "/workspaces/xxx/chat/download?path=/workspace/report.pdf", nil)
	c.Request = req

	h.Download(c)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when docker is nil, got %d: %s", w.Code, w.Body.String())
	}
}
