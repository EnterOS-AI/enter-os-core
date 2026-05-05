package handlers

// chat_files_poll_test.go — Upload poll-mode branch tests.
//
// Pinned in their own file so the existing chat_files_test.go stays
// focused on the push-mode forward proxy. Same setupTestDB / sqlmock
// scaffolding as the rest of the package, plus an in-memory
// pendinguploads.Storage so we don't have to mock six SQL statements
// per assertion.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/pendinguploads"
)

// inMemStorage is a process-local pendinguploads.Storage for branch
// tests. Records every Put for assertion. Failure modes (Put error,
// MarkFetched / Ack tested elsewhere) are injected via fields.
type inMemStorage struct {
	mu     sync.Mutex
	rows   map[uuid.UUID]pendinguploads.Record
	puts   []putCall
	putErr error
}

type putCall struct {
	WorkspaceID uuid.UUID
	Filename    string
	Mimetype    string
	Size        int
}

func newInMemStorage() *inMemStorage {
	return &inMemStorage{rows: map[uuid.UUID]pendinguploads.Record{}}
}

func (s *inMemStorage) Put(_ context.Context, ws uuid.UUID, content []byte, filename, mimetype string) (uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.putErr != nil {
		return uuid.Nil, s.putErr
	}
	id := uuid.New()
	s.rows[id] = pendinguploads.Record{
		FileID: id, WorkspaceID: ws, Content: content,
		Filename: filename, Mimetype: mimetype,
		SizeBytes: int64(len(content)), CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(24 * time.Hour),
	}
	s.puts = append(s.puts, putCall{
		WorkspaceID: ws, Filename: filename, Mimetype: mimetype, Size: len(content),
	})
	return id, nil
}

func (s *inMemStorage) Get(context.Context, uuid.UUID) (pendinguploads.Record, error) {
	return pendinguploads.Record{}, pendinguploads.ErrNotFound
}
func (s *inMemStorage) MarkFetched(context.Context, uuid.UUID) error { return nil }
func (s *inMemStorage) Ack(context.Context, uuid.UUID) error         { return nil }

// Sweep is required by the Storage interface (Phase 3 GC). Not
// exercised by upload-branch tests — the dedicated sweeper_test.go +
// storage_sweep_test.go cover it.
func (s *inMemStorage) Sweep(context.Context, time.Duration) (pendinguploads.SweepResult, error) {
	return pendinguploads.SweepResult{}, nil
}

// expectPollDeliveryMode stubs the SELECT delivery_mode lookup that
// uploadPollMode does (separate from the one resolveWorkspaceForwardCreds
// does — this is the new helper introduced for the poll branch).
func expectPollDeliveryMode(mock sqlmock.Sqlmock, workspaceID, mode string) {
	rows := sqlmock.NewRows([]string{"delivery_mode"}).AddRow(mode)
	mock.ExpectQuery(`SELECT delivery_mode FROM workspaces WHERE id = \$1`).
		WithArgs(workspaceID).
		WillReturnRows(rows)
}

func expectPollDeliveryModeMissing(mock sqlmock.Sqlmock, workspaceID string) {
	mock.ExpectQuery(`SELECT delivery_mode FROM workspaces WHERE id = \$1`).
		WithArgs(workspaceID).
		WillReturnError(sql.ErrNoRows)
}

// expectActivityInsert stubs the LogActivity INSERT so the poll branch's
// per-file activity row write doesn't fail the sqlmock expectations.
func expectActivityInsert(mock sqlmock.Sqlmock) {
	mock.ExpectExec(`INSERT INTO activity_logs`).
		WillReturnResult(sqlmock.NewResult(1, 1))
}

// expectActivityInsertWithTypeAndMethod is a strict variant that pins
// the activity_type and method positional args. Used in the discriminator
// regression test below — the workspace inbox poller filters
// `?type=a2a_receive`, so writing any other activity_type silently breaks
// poll-mode delivery without a build/test error. Pin the two discriminator
// fields so a refactor that flips activity_type back to a custom value is
// caught here instead of at runtime by a confused poller.
//
// Positional args (LogActivity uses ExecContext with 12 positional params):
//   $1 workspace_id, $2 activity_type, $3 source_id, $4 target_id,
//   $5 method, $6 summary, $7 request_body, $8 response_body,
//   $9 tool_trace, $10 duration_ms, $11 status, $12 error_detail.
func expectActivityInsertWithTypeAndMethod(mock sqlmock.Sqlmock, workspaceID, activityType, method string) {
	mock.ExpectExec(`INSERT INTO activity_logs`).
		WithArgs(
			workspaceID,             // $1 workspace_id
			activityType,            // $2 activity_type ← pinned
			sqlmock.AnyArg(),        // $3 source_id
			sqlmock.AnyArg(),        // $4 target_id (workspaceID, but already covered)
			method,                  // $5 method ← pinned
			sqlmock.AnyArg(),        // $6 summary
			sqlmock.AnyArg(),        // $7 request_body
			sqlmock.AnyArg(),        // $8 response_body
			sqlmock.AnyArg(),        // $9 tool_trace
			sqlmock.AnyArg(),        // $10 duration_ms
			sqlmock.AnyArg(),        // $11 status
			sqlmock.AnyArg(),        // $12 error_detail
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
}

// pollUploadFixture builds a multipart body with N named files.
func pollUploadFixture(t *testing.T, files map[string][]byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for name, data := range files {
		fw, err := mw.CreateFormFile("files", name)
		if err != nil {
			t.Fatalf("CreateFormFile: %v", err)
		}
		_, _ = fw.Write(data)
	}
	mw.Close()
	return &buf, mw.FormDataContentType()
}

// ---- happy path ----

func TestPollUpload_HappyPath_OneFile_StagesAndLogs(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	wsID := "11111111-2222-3333-4444-555555555555"
	expectPollDeliveryMode(mock, wsID, "poll")
	expectActivityInsert(mock)

	store := newInMemStorage()
	h := NewChatFilesHandler(NewTemplatesHandler(t.TempDir(), nil)).
		WithPendingUploads(store, nil)

	body, ct := pollUploadFixture(t, map[string][]byte{"report.pdf": []byte("PDF-bytes")})
	c, w := makeUploadRequest(t, wsID, body, ct)
	h.Upload(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if len(store.puts) != 1 {
		t.Fatalf("expected 1 storage Put, got %d", len(store.puts))
	}
	put := store.puts[0]
	if put.Filename != "report.pdf" || put.Size != 9 {
		t.Errorf("unexpected put: %+v", put)
	}

	// Response shape must match the workspace-side
	// /internal/chat/uploads/ingest schema so canvas can't tell which
	// path handled the upload.
	var resp struct {
		Files []struct {
			URI      string `json:"uri"`
			Name     string `json:"name"`
			Mimetype string `json:"mimeType"`
			Size     int    `json:"size"`
		} `json:"files"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v body=%s", err, w.Body.String())
	}
	if len(resp.Files) != 1 {
		t.Fatalf("response files count = %d, want 1", len(resp.Files))
	}
	got := resp.Files[0]
	if got.Name != "report.pdf" || got.Size != 9 {
		t.Errorf("response file mismatch: %+v", got)
	}
	if !strings.HasPrefix(got.URI, "platform-pending:"+wsID+"/") {
		t.Errorf("URI %q does not start with platform-pending:%s/", got.URI, wsID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestPollUpload_MultipleFiles_AllStagedAndLogged(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	wsID := "11111111-aaaa-bbbb-cccc-555555555555"
	expectPollDeliveryMode(mock, wsID, "poll")
	expectActivityInsert(mock)
	expectActivityInsert(mock)
	expectActivityInsert(mock)

	store := newInMemStorage()
	h := NewChatFilesHandler(NewTemplatesHandler(t.TempDir(), nil)).
		WithPendingUploads(store, nil)

	body, ct := pollUploadFixture(t, map[string][]byte{
		"a.txt": []byte("aaaa"),
		"b.txt": []byte("bbbbb"),
		"c.txt": []byte("cccccc"),
	})
	c, w := makeUploadRequest(t, wsID, body, ct)
	h.Upload(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if len(store.puts) != 3 {
		t.Fatalf("expected 3 storage Puts, got %d", len(store.puts))
	}
}

// ---- regression: push-mode unchanged ----

func TestPollUpload_PushModeFallsThroughToForward(t *testing.T) {
	// With pendingUploads wired but the workspace's mode is push,
	// the poll branch must NOT activate — flow falls through to the
	// existing resolveWorkspaceForwardCreds path. Pinned via the
	// "delivery_mode lookup happened, then the URL+mode SELECT
	// happened, then we 503 because no inbound secret" sequence.
	mock := setupTestDB(t)
	setupTestRedis(t)

	wsID := "22222222-2222-3333-4444-555555555555"
	expectPollDeliveryMode(mock, wsID, "push")
	// After the poll branch is bypassed, we hit
	// resolveWorkspaceForwardCreds which selects url+delivery_mode.
	expectURL(mock, wsID, "")
	// URL empty + mode=push → 503 (no inbound secret check needed).

	store := newInMemStorage()
	h := NewChatFilesHandler(NewTemplatesHandler(t.TempDir(), nil)).
		WithPendingUploads(store, nil)

	body, ct := pollUploadFixture(t, map[string][]byte{"x": []byte("data")})
	c, w := makeUploadRequest(t, wsID, body, ct)
	h.Upload(c)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s — expected push-mode 503 fall-through", w.Code, w.Body.String())
	}
	if len(store.puts) != 0 {
		t.Errorf("push-mode should NOT have hit storage, got %d puts", len(store.puts))
	}
}

func TestPollUpload_NotConfigured_FallsThrough(t *testing.T) {
	// Backwards compat: a binary running without WithPendingUploads
	// behaves exactly as before — the poll branch is dead code.
	mock := setupTestDB(t)
	setupTestRedis(t)

	wsID := "33333333-2222-3333-4444-555555555555"
	expectURLAndMode(mock, wsID, "", "poll") // resolveWorkspaceForwardCreds emits 422

	h := NewChatFilesHandler(NewTemplatesHandler(t.TempDir(), nil))
	// No WithPendingUploads — pendingUploads is nil.

	body, ct := pollUploadFixture(t, map[string][]byte{"x": []byte("data")})
	c, w := makeUploadRequest(t, wsID, body, ct)
	h.Upload(c)

	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("status=%d, want 422 (legacy poll-mode rejection)", w.Code)
	}
}

// ---- error paths ----

func TestPollUpload_WorkspaceMissing_404(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	wsID := "44444444-2222-3333-4444-555555555555"
	expectPollDeliveryModeMissing(mock, wsID)

	h := NewChatFilesHandler(NewTemplatesHandler(t.TempDir(), nil)).
		WithPendingUploads(newInMemStorage(), nil)

	body, ct := pollUploadFixture(t, map[string][]byte{"x": []byte("d")})
	c, w := makeUploadRequest(t, wsID, body, ct)
	h.Upload(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("status=%d, want 404", w.Code)
	}
}

func TestPollUpload_DeliveryModeLookupDBError_500(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	wsID := "55555555-2222-3333-4444-555555555555"
	mock.ExpectQuery(`SELECT delivery_mode FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).WillReturnError(errors.New("connection lost"))

	h := NewChatFilesHandler(NewTemplatesHandler(t.TempDir(), nil)).
		WithPendingUploads(newInMemStorage(), nil)

	body, ct := pollUploadFixture(t, map[string][]byte{"x": []byte("d")})
	c, w := makeUploadRequest(t, wsID, body, ct)
	h.Upload(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status=%d, want 500", w.Code)
	}
}

func TestPollUpload_NoFilesField_400(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	wsID := "66666666-2222-3333-4444-555555555555"
	expectPollDeliveryMode(mock, wsID, "poll")

	store := newInMemStorage()
	h := NewChatFilesHandler(NewTemplatesHandler(t.TempDir(), nil)).
		WithPendingUploads(store, nil)

	// Multipart with a non-files field — no actual files.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("not_files", "hi")
	mw.Close()

	c, w := makeUploadRequest(t, wsID, &buf, mw.FormDataContentType())
	h.Upload(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400 on no files field", w.Code)
	}
}

func TestPollUpload_MalformedMultipart_400(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	wsID := "77777777-2222-3333-4444-555555555555"
	expectPollDeliveryMode(mock, wsID, "poll")

	store := newInMemStorage()
	h := NewChatFilesHandler(NewTemplatesHandler(t.TempDir(), nil)).
		WithPendingUploads(store, nil)

	// Body that doesn't match the boundary in Content-Type.
	c, w := makeUploadRequest(t, wsID, bytes.NewBufferString("garbage"), "multipart/form-data; boundary=fake")
	h.Upload(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400 on malformed multipart", w.Code)
	}
}

func TestPollUpload_StorageError_500(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	wsID := "88888888-2222-3333-4444-555555555555"
	expectPollDeliveryMode(mock, wsID, "poll")

	store := newInMemStorage()
	store.putErr = errors.New("disk full")
	h := NewChatFilesHandler(NewTemplatesHandler(t.TempDir(), nil)).
		WithPendingUploads(store, nil)

	body, ct := pollUploadFixture(t, map[string][]byte{"x.bin": []byte("data")})
	c, w := makeUploadRequest(t, wsID, body, ct)
	h.Upload(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status=%d, want 500", w.Code)
	}
}

func TestPollUpload_StorageTooLarge_413(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	wsID := "99999999-2222-3333-4444-555555555555"
	expectPollDeliveryMode(mock, wsID, "poll")

	store := newInMemStorage()
	store.putErr = pendinguploads.ErrTooLarge
	h := NewChatFilesHandler(NewTemplatesHandler(t.TempDir(), nil)).
		WithPendingUploads(store, nil)

	body, ct := pollUploadFixture(t, map[string][]byte{"x.bin": []byte("data")})
	c, w := makeUploadRequest(t, wsID, body, ct)
	h.Upload(c)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status=%d, want 413", w.Code)
	}
}

func TestPollUpload_TooManyFiles_400(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	wsID := "aaaaaaaa-2222-3333-4444-555555555555"
	expectPollDeliveryMode(mock, wsID, "poll")

	store := newInMemStorage()
	h := NewChatFilesHandler(NewTemplatesHandler(t.TempDir(), nil)).
		WithPendingUploads(store, nil)

	// 65 files — over the per-batch cap.
	files := map[string][]byte{}
	for i := 0; i < 65; i++ {
		files[uuid.New().String()] = []byte("x")
	}
	body, ct := pollUploadFixture(t, files)
	c, w := makeUploadRequest(t, wsID, body, ct)
	h.Upload(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400 on too many files", w.Code)
	}
}

func TestPollUpload_NullDeliveryMode_TreatedAsPush(t *testing.T) {
	// Production-observed 2026-05-04: external runtime workspaces
	// (molecule-sdk-python on user infra) sometimes register with
	// delivery_mode = NULL — the schema default for legacy rows from
	// before #2339. The poll branch must NOT activate on NULL — only
	// the explicit "poll" string. This is the same defensive posture
	// resolveWorkspaceForwardCreds takes for legacy rows.
	mock := setupTestDB(t)
	setupTestRedis(t)

	wsID := "cccccccc-2222-3333-4444-555555555555"
	mock.ExpectQuery(`SELECT delivery_mode FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"delivery_mode"}).AddRow(nil))
	// Falls through to resolveWorkspaceForwardCreds:
	expectURLAndMode(mock, wsID, "", "")

	store := newInMemStorage()
	h := NewChatFilesHandler(NewTemplatesHandler(t.TempDir(), nil)).
		WithPendingUploads(store, nil)

	body, ct := pollUploadFixture(t, map[string][]byte{"x.bin": []byte("data")})
	c, w := makeUploadRequest(t, wsID, body, ct)
	h.Upload(c)

	// resolveWorkspaceForwardCreds with empty url + NULL mode = 422
	// (the legacy "no callback URL" rejection — exactly what we're
	// fixing for ACTUAL poll-mode rows but want to preserve for
	// NULL ones until the row gets a real mode value via the next
	// /registry/register).
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("status=%d, want 422 for NULL delivery_mode (legacy fallthrough)", w.Code)
	}
	if len(store.puts) != 0 {
		t.Errorf("NULL mode should NOT have hit storage, got %d puts", len(store.puts))
	}
}

func TestPollUpload_PerFileCapPreStorage_413(t *testing.T) {
	// Pin the early-reject branch (fh.Size > MaxFileBytes) BEFORE we
	// read the part into memory. Without this, an oversize file
	// would hit the storage layer's belt-and-suspenders check, which
	// works but burns ~25 MB of memory + DB round-trip first. Send
	// 25 MB + 1 byte → 413 with the file size in the response.
	mock := setupTestDB(t)
	setupTestRedis(t)

	wsID := "dddddddd-2222-3333-4444-555555555555"
	expectPollDeliveryMode(mock, wsID, "poll")

	store := newInMemStorage()
	h := NewChatFilesHandler(NewTemplatesHandler(t.TempDir(), nil)).
		WithPendingUploads(store, nil)

	// 25 MB + 1 byte. Single file, large enough to trip the early
	// size check.
	oversize := make([]byte, pendinguploads.MaxFileBytes+1)
	body, ct := pollUploadFixture(t, map[string][]byte{"big.bin": oversize})
	c, w := makeUploadRequest(t, wsID, body, ct)
	h.Upload(c)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d, want 413 on per-file size cap", w.Code)
	}
	if len(store.puts) != 0 {
		t.Errorf("per-file cap reject should NOT have called storage.Put, got %d puts", len(store.puts))
	}
	// Sanity: response carries the size we tried to upload + the cap.
	var body_ map[string]any
	json.Unmarshal(w.Body.Bytes(), &body_)
	if got := body_["max"]; got == nil {
		t.Errorf("expected max field in response, got %v", body_)
	}
}

// SanitizeFilename is exercised in the upload chain — pin one
// end-to-end case that exercises the URI path through the response.
func TestPollUpload_SanitizesFilenameInResponse(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	wsID := "bbbbbbbb-2222-3333-4444-555555555555"
	expectPollDeliveryMode(mock, wsID, "poll")
	expectActivityInsert(mock)

	store := newInMemStorage()
	h := NewChatFilesHandler(NewTemplatesHandler(t.TempDir(), nil)).
		WithPendingUploads(store, nil)

	body, ct := pollUploadFixture(t, map[string][]byte{"hello world!.pdf": []byte("data")})
	c, w := makeUploadRequest(t, wsID, body, ct)
	h.Upload(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Files []struct {
			Name string `json:"name"`
			URI  string `json:"uri"`
		}
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Files) == 0 || resp.Files[0].Name != "hello_world_.pdf" {
		t.Errorf("expected sanitized name 'hello_world_.pdf', got: %+v", resp.Files)
	}
	if len(store.puts) == 0 || store.puts[0].Filename != "hello_world_.pdf" {
		t.Errorf("storage Put didn't receive sanitized filename: %+v", store.puts)
	}
}

// TestPollUpload_ActivityRowDiscriminator pins the
// activity_type / method shape that the workspace inbox poller depends
// on. The poller filters `GET /workspaces/:id/activity?type=a2a_receive`
// so the handler MUST write activity_type=a2a_receive (NOT a custom
// type), and use method=chat_upload_receive as the
// upload-vs-message-vs-task discriminator.
//
// Why pinned: a previous iteration of this handler used
// activity_type="chat_upload_receive" — silently invisible to the
// existing poller. The branch passed every push-mode test, every
// storage test, and every per-file content test; the bug only
// surfaced at runtime when the workspace polled and got nothing.
// Encode the contract in a unit test so the next refactor can't
// re-break it without a red CI.
func TestPollUpload_ActivityRowDiscriminator(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	wsID := "abc12345-6789-4abc-8def-000000000999"
	expectPollDeliveryMode(mock, wsID, "poll")
	expectActivityInsertWithTypeAndMethod(mock, wsID, "a2a_receive", "chat_upload_receive")

	store := newInMemStorage()
	h := NewChatFilesHandler(NewTemplatesHandler(t.TempDir(), nil)).
		WithPendingUploads(store, nil)

	body, ct := pollUploadFixture(t, map[string][]byte{"x.pdf": []byte("xx")})
	c, w := makeUploadRequest(t, wsID, body, ct)
	h.Upload(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}
