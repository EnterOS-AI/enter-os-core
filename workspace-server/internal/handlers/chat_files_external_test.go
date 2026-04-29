package handlers

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// TestChatFilesUpload_ExternalRuntime_Returns422 pins the contract that
// upload to a runtime="external" workspace returns a clear 422 with the
// "external workspaces don't support file upload" message instead of the
// misleading 503 "container not running" the v0.1 surface returned (#2308).
//
// Without this branch, an operator pasting a screenshot into the canvas chat
// for an external CEO workspace got a `503 {"error":"workspace container
// not running"}` — accurate from the upload handler's POV (no container
// exists for external workspaces) but misleading because it implies the
// container has crashed. The 422 with structured detail tells the operator
// what's actually happening + points at the v0.2 follow-up issue.
func TestChatFilesUpload_ExternalRuntime_Returns422(t *testing.T) {
	mock := setupTestDB(t)

	const wsID = "00000000-0000-0000-0000-000000000001"

	// Runtime lookup returns "external" — should trigger the early return.
	mock.ExpectQuery("SELECT COALESCE\\(runtime, ''\\) FROM workspaces").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"runtime"}).AddRow("external"))

	// Construct a multipart upload body — handler must reject BEFORE
	// touching docker, so a nil templates field is intentional. If the
	// runtime check is removed in a future change, this test crashes on
	// nil deref of h.templates instead of silently passing.
	body, contentType := buildMultipartUpload(t, "screenshot.png", []byte("fake-png-bytes"))

	// Use a nil-templates handler — proves runtime check happens BEFORE
	// any docker plumbing. The handler is constructed in production via
	// NewChatFilesHandler(templates) but the runtime branch should never
	// reach into templates for external workspaces.
	h := &ChatFilesHandler{templates: nil}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest("POST", "/workspaces/"+wsID+"/chat/files", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", contentType)

	h.Upload(c)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if !strings.Contains(resp["error"], "external workspaces") {
		t.Errorf("expected error to mention external workspaces, got: %q", resp["error"])
	}
	if resp["runtime"] != "external" {
		t.Errorf("expected runtime=external in response, got %q", resp["runtime"])
	}
	// Spot-check that the error points at issue #2308 so operators reading
	// it know where to track v0.2 file-ingest.
	if !strings.Contains(resp["detail"], "2308") {
		t.Errorf("expected detail to reference issue #2308, got: %q", resp["detail"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// buildMultipartUpload returns a body + content-type pair suitable for
// posting a single file through the chat upload handler.
func buildMultipartUpload(t *testing.T, filename string, contents []byte) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("files", filename)
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := part.Write(contents); err != nil {
		t.Fatalf("part.Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("multipart Close: %v", err)
	}
	return buf.Bytes(), w.FormDataContentType()
}
