package handlers_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/handlers"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/pendinguploads"
)

// fakeStorage is an in-memory pendinguploads.Storage. Lets handler
// tests pin behaviour without going through Postgres + sqlmock — the
// storage layer's own tests (internal/pendinguploads/storage_test.go)
// cover the SQL drift surface; here we only care about the handler's
// 4xx/5xx mapping and side-effect ordering.
type fakeStorage struct {
	rows         map[uuid.UUID]pendinguploads.Record
	getErr       error // forced error from Get (overrides rows lookup)
	ackErr       error // forced error from Ack
	markErr      error // forced error from MarkFetched
	markFetched  []uuid.UUID
	ackCalls     []uuid.UUID
}

func newFakeStorage() *fakeStorage {
	return &fakeStorage{rows: map[uuid.UUID]pendinguploads.Record{}}
}

func (f *fakeStorage) Put(ctx context.Context, ws uuid.UUID, content []byte, filename, mimetype string) (uuid.UUID, error) {
	id := uuid.New()
	f.rows[id] = pendinguploads.Record{
		FileID: id, WorkspaceID: ws, Content: content,
		Filename: filename, Mimetype: mimetype,
		SizeBytes: int64(len(content)), CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(24 * time.Hour),
	}
	return id, nil
}

func (f *fakeStorage) Get(_ context.Context, fileID uuid.UUID) (pendinguploads.Record, error) {
	if f.getErr != nil {
		return pendinguploads.Record{}, f.getErr
	}
	rec, ok := f.rows[fileID]
	if !ok {
		return pendinguploads.Record{}, pendinguploads.ErrNotFound
	}
	return rec, nil
}

func (f *fakeStorage) MarkFetched(_ context.Context, fileID uuid.UUID) error {
	f.markFetched = append(f.markFetched, fileID)
	return f.markErr
}

func (f *fakeStorage) Ack(_ context.Context, fileID uuid.UUID) error {
	f.ackCalls = append(f.ackCalls, fileID)
	if f.ackErr != nil {
		return f.ackErr
	}
	delete(f.rows, fileID)
	return nil
}

// Sweep is required by the Storage interface (Phase 3 GC). Not exercised
// by these handler tests — the dedicated sweeper_test.go covers it.
func (f *fakeStorage) Sweep(_ context.Context, _ time.Duration) (pendinguploads.SweepResult, error) {
	return pendinguploads.SweepResult{}, nil
}

// PutBatch is required by the Storage interface; the upload handler
// tests live in chat_files_poll_test.go and use a separate fake
// (inMemStorage). Stubbed here because the Get/Ack tests don't drive
// PutBatch, but the interface must be satisfied.
func (f *fakeStorage) PutBatch(_ context.Context, _ uuid.UUID, _ []pendinguploads.PutItem) ([]uuid.UUID, error) {
	return nil, nil
}
func (f *fakeStorage) PutBatchTx(_ context.Context, _ *sql.Tx, _ uuid.UUID, _ []pendinguploads.PutItem) ([]uuid.UUID, error) {
	return nil, nil
}

func newRouter(handler *handlers.PendingUploadsHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/workspaces/:id/pending-uploads/:file_id/content", handler.GetContent)
	r.POST("/workspaces/:id/pending-uploads/:file_id/ack", handler.Ack)
	return r
}

// ---- GetContent ----

func TestGetContent_HappyPath_StreamsBytesAndStampsFetched(t *testing.T) {
	fs := newFakeStorage()
	wsID := uuid.New()
	fileID, err := fs.Put(context.Background(), wsID, []byte("hello world"), "report.pdf", "application/pdf")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	h := handlers.NewPendingUploadsHandler(fs)
	r := newRouter(h)

	req := httptest.NewRequest(http.MethodGet,
		"/workspaces/"+wsID.String()+"/pending-uploads/"+fileID.String()+"/content", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != "hello world" {
		t.Errorf("body = %q, want %q", got, "hello world")
	}
	if got := w.Header().Get("Content-Type"); got != "application/pdf" {
		t.Errorf("Content-Type = %q, want application/pdf", got)
	}
	if got := w.Header().Get("Content-Disposition"); !strings.Contains(got, "report.pdf") {
		t.Errorf("Content-Disposition = %q, expected to mention report.pdf", got)
	}
	if got := w.Header().Get("Content-Length"); got != "11" {
		t.Errorf("Content-Length = %q, want 11", got)
	}
	if len(fs.markFetched) != 1 || fs.markFetched[0] != fileID {
		t.Errorf("expected MarkFetched(%s), got %v", fileID, fs.markFetched)
	}
}

func TestGetContent_DefaultsMimetypeWhenEmpty(t *testing.T) {
	fs := newFakeStorage()
	wsID := uuid.New()
	fileID, _ := fs.Put(context.Background(), wsID, []byte("data"), "x.bin", "")
	h := handlers.NewPendingUploadsHandler(fs)
	r := newRouter(h)

	req := httptest.NewRequest(http.MethodGet,
		"/workspaces/"+wsID.String()+"/pending-uploads/"+fileID.String()+"/content", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if got := w.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("Content-Type fallback = %q, want application/octet-stream", got)
	}
}

func TestGetContent_InvalidWorkspaceID_400(t *testing.T) {
	fs := newFakeStorage()
	r := newRouter(handlers.NewPendingUploadsHandler(fs))
	req := httptest.NewRequest(http.MethodGet, "/workspaces/not-a-uuid/pending-uploads/00000000-0000-0000-0000-000000000000/content", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", w.Code)
	}
}

func TestGetContent_InvalidFileID_400(t *testing.T) {
	fs := newFakeStorage()
	r := newRouter(handlers.NewPendingUploadsHandler(fs))
	wsID := uuid.New()
	req := httptest.NewRequest(http.MethodGet,
		"/workspaces/"+wsID.String()+"/pending-uploads/not-a-uuid/content", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", w.Code)
	}
}

func TestGetContent_NotFound_404(t *testing.T) {
	fs := newFakeStorage()
	r := newRouter(handlers.NewPendingUploadsHandler(fs))
	wsID := uuid.New()
	missing := uuid.New()
	req := httptest.NewRequest(http.MethodGet,
		"/workspaces/"+wsID.String()+"/pending-uploads/"+missing.String()+"/content", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status=%d, want 404", w.Code)
	}
}

func TestGetContent_StorageError_500(t *testing.T) {
	fs := newFakeStorage()
	fs.getErr = errors.New("connection refused")
	r := newRouter(handlers.NewPendingUploadsHandler(fs))
	wsID := uuid.New()
	req := httptest.NewRequest(http.MethodGet,
		"/workspaces/"+wsID.String()+"/pending-uploads/"+uuid.New().String()+"/content", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status=%d, want 500", w.Code)
	}
}

func TestGetContent_CrossWorkspaceBleed_404(t *testing.T) {
	// Token leak: workspace A's wsAuth-validated request tries to
	// pull workspace B's file_id. Handler must 404 even though the
	// row exists.
	fs := newFakeStorage()
	wsB := uuid.New()
	fileID, _ := fs.Put(context.Background(), wsB, []byte("secret"), "leak.txt", "text/plain")

	wsA := uuid.New()
	r := newRouter(handlers.NewPendingUploadsHandler(fs))
	req := httptest.NewRequest(http.MethodGet,
		"/workspaces/"+wsA.String()+"/pending-uploads/"+fileID.String()+"/content", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 for cross-workspace bleed", w.Code)
	}
	// Critical: must not have leaked the bytes.
	if strings.Contains(w.Body.String(), "secret") {
		t.Errorf("response body leaked content from another workspace: %q", w.Body.String())
	}
}

func TestGetContent_MarkFetchedFailureLoggedNotPropagated(t *testing.T) {
	fs := newFakeStorage()
	wsID := uuid.New()
	fileID, _ := fs.Put(context.Background(), wsID, []byte("ok"), "x.txt", "text/plain")
	fs.markErr = errors.New("update failed (sweep raced)")
	h := handlers.NewPendingUploadsHandler(fs)
	r := newRouter(h)

	req := httptest.NewRequest(http.MethodGet,
		"/workspaces/"+wsID.String()+"/pending-uploads/"+fileID.String()+"/content", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// Body already returned 200 OK + bytes BEFORE the MarkFetched
	// failure — workspace fetch must NOT fail because of an
	// observability hook.
	if w.Code != http.StatusOK {
		t.Errorf("status=%d, want 200 even on MarkFetched failure", w.Code)
	}
	if w.Body.String() != "ok" {
		t.Errorf("body = %q, want %q", w.Body.String(), "ok")
	}
}

// ---- Ack ----

func TestAck_HappyPath_RemovesRow(t *testing.T) {
	fs := newFakeStorage()
	wsID := uuid.New()
	fileID, _ := fs.Put(context.Background(), wsID, []byte("data"), "x.bin", "")
	r := newRouter(handlers.NewPendingUploadsHandler(fs))

	req := httptest.NewRequest(http.MethodPost,
		"/workspaces/"+wsID.String()+"/pending-uploads/"+fileID.String()+"/ack", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["acked"] != true {
		t.Errorf("body.acked = %v, want true", body["acked"])
	}
	if _, exists := fs.rows[fileID]; exists {
		t.Errorf("row should have been removed after ack")
	}
}

func TestAck_NonExistent_404(t *testing.T) {
	fs := newFakeStorage()
	r := newRouter(handlers.NewPendingUploadsHandler(fs))
	wsID := uuid.New()
	req := httptest.NewRequest(http.MethodPost,
		"/workspaces/"+wsID.String()+"/pending-uploads/"+uuid.New().String()+"/ack", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status=%d, want 404", w.Code)
	}
}

func TestAck_CrossWorkspaceBleed_404(t *testing.T) {
	fs := newFakeStorage()
	wsB := uuid.New()
	fileID, _ := fs.Put(context.Background(), wsB, []byte("data"), "x.bin", "")
	wsA := uuid.New()
	r := newRouter(handlers.NewPendingUploadsHandler(fs))

	req := httptest.NewRequest(http.MethodPost,
		"/workspaces/"+wsA.String()+"/pending-uploads/"+fileID.String()+"/ack", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status=%d, want 404 for cross-workspace ack", w.Code)
	}
	// Row must remain — workspace A's bogus ack must NOT delete
	// workspace B's file.
	if _, exists := fs.rows[fileID]; !exists {
		t.Errorf("row should NOT have been removed by cross-workspace ack")
	}
}

func TestAck_InvalidWorkspaceID_400(t *testing.T) {
	fs := newFakeStorage()
	r := newRouter(handlers.NewPendingUploadsHandler(fs))
	req := httptest.NewRequest(http.MethodPost, "/workspaces/not-a-uuid/pending-uploads/"+uuid.New().String()+"/ack", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", w.Code)
	}
}

func TestAck_InvalidFileID_400(t *testing.T) {
	fs := newFakeStorage()
	r := newRouter(handlers.NewPendingUploadsHandler(fs))
	req := httptest.NewRequest(http.MethodPost,
		"/workspaces/"+uuid.New().String()+"/pending-uploads/not-a-uuid/ack", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", w.Code)
	}
}

func TestAck_GetStorageError_500(t *testing.T) {
	fs := newFakeStorage()
	fs.getErr = errors.New("conn lost")
	r := newRouter(handlers.NewPendingUploadsHandler(fs))
	req := httptest.NewRequest(http.MethodPost,
		"/workspaces/"+uuid.New().String()+"/pending-uploads/"+uuid.New().String()+"/ack", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status=%d, want 500", w.Code)
	}
}

func TestAck_RaceWithSweep_ReturnsRacedTrue(t *testing.T) {
	// Sweep deletes the row between the handler's Get and Ack calls.
	// Storage.Ack returns ErrNotFound; handler treats that as success
	// (intent honored, row gone) and reports raced:true.
	fs := newFakeStorage()
	wsID := uuid.New()
	fileID, _ := fs.Put(context.Background(), wsID, []byte("data"), "x.bin", "")
	fs.ackErr = pendinguploads.ErrNotFound
	r := newRouter(handlers.NewPendingUploadsHandler(fs))

	req := httptest.NewRequest(http.MethodPost,
		"/workspaces/"+wsID.String()+"/pending-uploads/"+fileID.String()+"/ack", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 on race", w.Code)
	}
	var body map[string]any
	json.Unmarshal(w.Body.Bytes(), &body)
	if body["acked"] != true || body["raced"] != true {
		t.Errorf("expected acked=true raced=true, got %v", body)
	}
}

func TestAck_StorageError_500(t *testing.T) {
	fs := newFakeStorage()
	wsID := uuid.New()
	fileID, _ := fs.Put(context.Background(), wsID, []byte("data"), "x.bin", "")
	fs.ackErr = errors.New("conn refused")
	r := newRouter(handlers.NewPendingUploadsHandler(fs))

	req := httptest.NewRequest(http.MethodPost,
		"/workspaces/"+wsID.String()+"/pending-uploads/"+fileID.String()+"/ack", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status=%d, want 500", w.Code)
	}
}
