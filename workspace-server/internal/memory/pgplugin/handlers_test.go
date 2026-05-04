package pgplugin

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/memory/contract"
)

func setupMockDB(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, mock
}

func newTestHandler(t *testing.T, db *sql.DB, pingErr error) *Handler {
	t.Helper()
	store := NewStore(db)
	return NewHandler(store, func() error { return pingErr })
}

func doRequest(h *Handler, method, path string, body interface{}) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	var r *http.Request
	if body != nil {
		buf, _ := json.Marshal(body)
		r = httptest.NewRequest(method, path, bytes.NewReader(buf))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	h.ServeHTTP(w, r)
	return w
}

// --- Health ---

func TestHealth_OK(t *testing.T) {
	db, _ := setupMockDB(t)
	h := newTestHandler(t, db, nil)
	w := doRequest(h, "GET", "/v1/health", nil)
	if w.Code != 200 {
		t.Errorf("code = %d, want 200", w.Code)
	}
	var hr contract.HealthResponse
	if err := json.Unmarshal(w.Body.Bytes(), &hr); err != nil {
		t.Fatal(err)
	}
	if hr.Status != "ok" {
		t.Errorf("status = %q", hr.Status)
	}
	if !hr.HasCapability(contract.CapabilityFTS) || !hr.HasCapability(contract.CapabilityEmbedding) {
		t.Errorf("missing capabilities: %v", hr.Capabilities)
	}
}

func TestHealth_Degraded(t *testing.T) {
	db, _ := setupMockDB(t)
	h := newTestHandler(t, db, errors.New("db dead"))
	w := doRequest(h, "GET", "/v1/health", nil)
	if w.Code != 503 {
		t.Errorf("code = %d, want 503", w.Code)
	}
	var hr contract.HealthResponse
	_ = json.Unmarshal(w.Body.Bytes(), &hr)
	if hr.Status != "degraded" {
		t.Errorf("status = %q, want degraded", hr.Status)
	}
}

func TestHealth_NoPing(t *testing.T) {
	db, _ := setupMockDB(t)
	store := NewStore(db)
	h := NewHandler(store, nil) // no ping fn
	w := doRequest(h, "GET", "/v1/health", nil)
	if w.Code != 200 {
		t.Errorf("code = %d, want 200 when no ping", w.Code)
	}
}

// --- UpsertNamespace ---

func TestUpsertNamespace_HappyPath(t *testing.T) {
	db, mock := setupMockDB(t)
	h := newTestHandler(t, db, nil)
	mock.ExpectQuery("INSERT INTO memory_namespaces").
		WithArgs("workspace:abc", "workspace", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"name", "kind", "expires_at", "metadata", "created_at"}).
			AddRow("workspace:abc", "workspace", nil, nil, time.Now()))
	w := doRequest(h, "PUT", "/v1/namespaces/workspace:abc", contract.NamespaceUpsert{Kind: contract.NamespaceKindWorkspace})
	if w.Code != 200 {
		t.Errorf("code = %d body=%s", w.Code, w.Body.String())
	}
}

func TestUpsertNamespace_RejectsBadName(t *testing.T) {
	db, _ := setupMockDB(t)
	h := newTestHandler(t, db, nil)
	w := doRequest(h, "PUT", "/v1/namespaces/BAD-NAME", contract.NamespaceUpsert{Kind: contract.NamespaceKindWorkspace})
	if w.Code != 400 {
		t.Errorf("code = %d, want 400", w.Code)
	}
}

func TestUpsertNamespace_RejectsBadJSON(t *testing.T) {
	db, _ := setupMockDB(t)
	h := newTestHandler(t, db, nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("PUT", "/v1/namespaces/workspace:abc", strings.NewReader("not-json"))
	h.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Errorf("code = %d, want 400", w.Code)
	}
}

func TestUpsertNamespace_RejectsBadBody(t *testing.T) {
	db, _ := setupMockDB(t)
	h := newTestHandler(t, db, nil)
	w := doRequest(h, "PUT", "/v1/namespaces/workspace:abc", contract.NamespaceUpsert{Kind: ""})
	if w.Code != 400 {
		t.Errorf("code = %d, want 400 for empty kind", w.Code)
	}
}

func TestUpsertNamespace_StoreError(t *testing.T) {
	db, mock := setupMockDB(t)
	h := newTestHandler(t, db, nil)
	mock.ExpectQuery("INSERT INTO memory_namespaces").
		WillReturnError(errors.New("db down"))
	w := doRequest(h, "PUT", "/v1/namespaces/workspace:abc", contract.NamespaceUpsert{Kind: contract.NamespaceKindWorkspace})
	if w.Code != 500 {
		t.Errorf("code = %d, want 500", w.Code)
	}
}

// --- PatchNamespace ---

func TestPatchNamespace_HappyPath_ExpiresOnly(t *testing.T) {
	db, mock := setupMockDB(t)
	h := newTestHandler(t, db, nil)
	exp := time.Now().Add(time.Hour).UTC()
	mock.ExpectQuery("UPDATE memory_namespaces").
		WithArgs("workspace:abc", exp).
		WillReturnRows(sqlmock.NewRows([]string{"name", "kind", "expires_at", "metadata", "created_at"}).
			AddRow("workspace:abc", "workspace", exp, nil, time.Now()))
	w := doRequest(h, "PATCH", "/v1/namespaces/workspace:abc", contract.NamespacePatch{ExpiresAt: &exp})
	if w.Code != 200 {
		t.Errorf("code = %d body=%s", w.Code, w.Body.String())
	}
}

func TestPatchNamespace_HappyPath_BothFields(t *testing.T) {
	db, mock := setupMockDB(t)
	h := newTestHandler(t, db, nil)
	exp := time.Now().Add(time.Hour).UTC()
	mock.ExpectQuery("UPDATE memory_namespaces").
		WithArgs("workspace:abc", exp, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"name", "kind", "expires_at", "metadata", "created_at"}).
			AddRow("workspace:abc", "workspace", exp, []byte(`{"k":"v"}`), time.Now()))
	w := doRequest(h, "PATCH", "/v1/namespaces/workspace:abc", contract.NamespacePatch{
		ExpiresAt: &exp,
		Metadata:  map[string]interface{}{"k": "v"},
	})
	if w.Code != 200 {
		t.Errorf("code = %d body=%s", w.Code, w.Body.String())
	}
}

func TestPatchNamespace_NotFound(t *testing.T) {
	db, mock := setupMockDB(t)
	h := newTestHandler(t, db, nil)
	exp := time.Now().Add(time.Hour).UTC()
	mock.ExpectQuery("UPDATE memory_namespaces").
		WithArgs("workspace:gone", exp).
		WillReturnError(sql.ErrNoRows)
	w := doRequest(h, "PATCH", "/v1/namespaces/workspace:gone", contract.NamespacePatch{ExpiresAt: &exp})
	if w.Code != 404 {
		t.Errorf("code = %d, want 404", w.Code)
	}
}

func TestPatchNamespace_StoreError(t *testing.T) {
	db, mock := setupMockDB(t)
	h := newTestHandler(t, db, nil)
	exp := time.Now().Add(time.Hour).UTC()
	mock.ExpectQuery("UPDATE memory_namespaces").
		WillReturnError(errors.New("db dead"))
	w := doRequest(h, "PATCH", "/v1/namespaces/workspace:abc", contract.NamespacePatch{ExpiresAt: &exp})
	if w.Code != 500 {
		t.Errorf("code = %d, want 500", w.Code)
	}
}

func TestPatchNamespace_RejectsEmptyBody(t *testing.T) {
	db, _ := setupMockDB(t)
	h := newTestHandler(t, db, nil)
	w := doRequest(h, "PATCH", "/v1/namespaces/workspace:abc", contract.NamespacePatch{})
	if w.Code != 400 {
		t.Errorf("code = %d, want 400", w.Code)
	}
}

func TestPatchNamespace_RejectsBadName(t *testing.T) {
	db, _ := setupMockDB(t)
	h := newTestHandler(t, db, nil)
	exp := time.Now()
	w := doRequest(h, "PATCH", "/v1/namespaces/BAD", contract.NamespacePatch{ExpiresAt: &exp})
	if w.Code != 400 {
		t.Errorf("code = %d, want 400", w.Code)
	}
}

func TestPatchNamespace_RejectsBadJSON(t *testing.T) {
	db, _ := setupMockDB(t)
	h := newTestHandler(t, db, nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("PATCH", "/v1/namespaces/workspace:abc", strings.NewReader("not-json"))
	h.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Errorf("code = %d, want 400", w.Code)
	}
}

// --- DeleteNamespace ---

func TestDeleteNamespace_HappyPath(t *testing.T) {
	db, mock := setupMockDB(t)
	h := newTestHandler(t, db, nil)
	mock.ExpectExec("DELETE FROM memory_namespaces").
		WithArgs("workspace:abc").
		WillReturnResult(sqlmock.NewResult(0, 1))
	w := doRequest(h, "DELETE", "/v1/namespaces/workspace:abc", nil)
	if w.Code != 204 {
		t.Errorf("code = %d body=%s", w.Code, w.Body.String())
	}
}

func TestDeleteNamespace_NotFound(t *testing.T) {
	db, mock := setupMockDB(t)
	h := newTestHandler(t, db, nil)
	mock.ExpectExec("DELETE FROM memory_namespaces").
		WithArgs("workspace:gone").
		WillReturnResult(sqlmock.NewResult(0, 0))
	w := doRequest(h, "DELETE", "/v1/namespaces/workspace:gone", nil)
	if w.Code != 404 {
		t.Errorf("code = %d, want 404", w.Code)
	}
}

func TestDeleteNamespace_StoreError(t *testing.T) {
	db, mock := setupMockDB(t)
	h := newTestHandler(t, db, nil)
	mock.ExpectExec("DELETE FROM memory_namespaces").
		WillReturnError(errors.New("db dead"))
	w := doRequest(h, "DELETE", "/v1/namespaces/workspace:abc", nil)
	if w.Code != 500 {
		t.Errorf("code = %d, want 500", w.Code)
	}
}

func TestDeleteNamespace_RejectsBadName(t *testing.T) {
	db, _ := setupMockDB(t)
	h := newTestHandler(t, db, nil)
	w := doRequest(h, "DELETE", "/v1/namespaces/BAD", nil)
	if w.Code != 400 {
		t.Errorf("code = %d, want 400", w.Code)
	}
}

// --- CommitMemory ---

func TestCommitMemory_HappyPath(t *testing.T) {
	db, mock := setupMockDB(t)
	h := newTestHandler(t, db, nil)
	mock.ExpectQuery("INSERT INTO memory_records").
		WithArgs("workspace:abc", "fact x", "fact", "agent", sqlmock.AnyArg(), sqlmock.AnyArg(), false, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "namespace"}).
			AddRow("mem-id-1", "workspace:abc"))
	w := doRequest(h, "POST", "/v1/namespaces/workspace:abc/memories", contract.MemoryWrite{
		Content: "fact x", Kind: contract.MemoryKindFact, Source: contract.MemorySourceAgent,
	})
	if w.Code != 201 {
		t.Errorf("code = %d body=%s", w.Code, w.Body.String())
	}
}

func TestCommitMemory_RejectsBadName(t *testing.T) {
	db, _ := setupMockDB(t)
	h := newTestHandler(t, db, nil)
	w := doRequest(h, "POST", "/v1/namespaces/BAD/memories", contract.MemoryWrite{
		Content: "x", Kind: contract.MemoryKindFact, Source: contract.MemorySourceAgent,
	})
	if w.Code != 400 {
		t.Errorf("code = %d, want 400", w.Code)
	}
}

func TestCommitMemory_RejectsBadJSON(t *testing.T) {
	db, _ := setupMockDB(t)
	h := newTestHandler(t, db, nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/namespaces/workspace:abc/memories", strings.NewReader("not-json"))
	h.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Errorf("code = %d, want 400", w.Code)
	}
}

func TestCommitMemory_RejectsBadBody(t *testing.T) {
	db, _ := setupMockDB(t)
	h := newTestHandler(t, db, nil)
	w := doRequest(h, "POST", "/v1/namespaces/workspace:abc/memories", contract.MemoryWrite{Content: ""})
	if w.Code != 400 {
		t.Errorf("code = %d, want 400 for empty content", w.Code)
	}
}

func TestCommitMemory_StoreError(t *testing.T) {
	db, mock := setupMockDB(t)
	h := newTestHandler(t, db, nil)
	mock.ExpectQuery("INSERT INTO memory_records").
		WillReturnError(errors.New("db dead"))
	w := doRequest(h, "POST", "/v1/namespaces/workspace:abc/memories", contract.MemoryWrite{
		Content: "x", Kind: contract.MemoryKindFact, Source: contract.MemorySourceAgent,
	})
	if w.Code != 500 {
		t.Errorf("code = %d, want 500", w.Code)
	}
}

func TestCommitMemory_WithIDUpserts(t *testing.T) {
	// Idempotency-key path. When body.id is set, the store must use
	// the upsert SQL (INSERT ... ON CONFLICT DO UPDATE) so a re-run
	// updates in place instead of inserting a new row.
	db, mock := setupMockDB(t)
	h := newTestHandler(t, db, nil)
	mock.ExpectQuery("INSERT INTO memory_records.*ON CONFLICT").
		WithArgs("fixed-id-1", "workspace:abc", "fact x", "fact", "agent",
			sqlmock.AnyArg(), sqlmock.AnyArg(), false, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "namespace"}).
			AddRow("fixed-id-1", "workspace:abc"))
	w := doRequest(h, "POST", "/v1/namespaces/workspace:abc/memories", contract.MemoryWrite{
		ID:      "fixed-id-1",
		Content: "fact x",
		Kind:    contract.MemoryKindFact,
		Source:  contract.MemorySourceAgent,
	})
	if w.Code != 201 {
		t.Errorf("code = %d body=%s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("upsert SQL not used: %v", err)
	}
}

func TestCommitMemory_UpsertScanError(t *testing.T) {
	db, mock := setupMockDB(t)
	h := newTestHandler(t, db, nil)
	mock.ExpectQuery("INSERT INTO memory_records.*ON CONFLICT").
		WillReturnRows(sqlmock.NewRows([]string{"id"}). // wrong shape
								AddRow("x"))
	w := doRequest(h, "POST", "/v1/namespaces/workspace:abc/memories", contract.MemoryWrite{
		ID:      "fixed-id-1",
		Content: "x", Kind: contract.MemoryKindFact, Source: contract.MemorySourceAgent,
	})
	if w.Code != 500 {
		t.Errorf("code = %d body=%s", w.Code, w.Body.String())
	}
}

func TestCommitMemory_WithEmbedding(t *testing.T) {
	db, mock := setupMockDB(t)
	h := newTestHandler(t, db, nil)
	mock.ExpectQuery("INSERT INTO memory_records").
		WithArgs("workspace:abc", "x", "fact", "agent",
			sqlmock.AnyArg(), sqlmock.AnyArg(), false, "[0.1,0.2,0.3]").
		WillReturnRows(sqlmock.NewRows([]string{"id", "namespace"}).
			AddRow("mem-id-1", "workspace:abc"))
	w := doRequest(h, "POST", "/v1/namespaces/workspace:abc/memories", contract.MemoryWrite{
		Content: "x", Kind: contract.MemoryKindFact, Source: contract.MemorySourceAgent,
		Embedding: []float32{0.1, 0.2, 0.3},
	})
	if w.Code != 201 {
		t.Errorf("code = %d body=%s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// --- Search ---

func TestSearch_FTS(t *testing.T) {
	db, mock := setupMockDB(t)
	h := newTestHandler(t, db, nil)
	mock.ExpectQuery("SELECT id, namespace, content").
		WillReturnRows(sqlmock.NewRows([]string{"id", "namespace", "content", "kind", "source", "expires_at", "propagation", "pin", "created_at", "score"}).
			AddRow("id-1", "workspace:abc", "remembered fact", "fact", "agent", nil, nil, false, time.Now(), 0.85))
	w := doRequest(h, "POST", "/v1/search", contract.SearchRequest{
		Namespaces: []string{"workspace:abc"},
		Query:      "fact",
	})
	if w.Code != 200 {
		t.Errorf("code = %d body=%s", w.Code, w.Body.String())
	}
	var resp contract.SearchResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Memories) != 1 {
		t.Errorf("memories len = %d, want 1", len(resp.Memories))
	}
	if resp.Memories[0].Score == nil || *resp.Memories[0].Score != 0.85 {
		t.Errorf("score = %v", resp.Memories[0].Score)
	}
}

func TestSearch_Semantic(t *testing.T) {
	db, mock := setupMockDB(t)
	h := newTestHandler(t, db, nil)
	mock.ExpectQuery("SELECT id, namespace, content").
		WillReturnRows(sqlmock.NewRows([]string{"id", "namespace", "content", "kind", "source", "expires_at", "propagation", "pin", "created_at", "score"}).
			AddRow("id-1", "workspace:abc", "x", "fact", "agent", nil, nil, false, time.Now(), 0.92))
	w := doRequest(h, "POST", "/v1/search", contract.SearchRequest{
		Namespaces: []string{"workspace:abc"},
		Embedding:  []float32{1.0, 2.0, 3.0},
	})
	if w.Code != 200 {
		t.Errorf("code = %d body=%s", w.Code, w.Body.String())
	}
}

func TestSearch_ShortQueryUsesILIKE(t *testing.T) {
	db, mock := setupMockDB(t)
	h := newTestHandler(t, db, nil)
	mock.ExpectQuery("SELECT id, namespace, content").
		WillReturnRows(sqlmock.NewRows([]string{"id", "namespace", "content", "kind", "source", "expires_at", "propagation", "pin", "created_at", "score"}).
			AddRow("id-1", "workspace:abc", "x", "fact", "agent", nil, nil, false, time.Now(), nil))
	// Single-char query falls through to ILIKE
	w := doRequest(h, "POST", "/v1/search", contract.SearchRequest{
		Namespaces: []string{"workspace:abc"},
		Query:      "x",
	})
	if w.Code != 200 {
		t.Errorf("code = %d body=%s", w.Code, w.Body.String())
	}
}

func TestSearch_NoQueryListsRecent(t *testing.T) {
	db, mock := setupMockDB(t)
	h := newTestHandler(t, db, nil)
	mock.ExpectQuery("SELECT id, namespace, content").
		WillReturnRows(sqlmock.NewRows([]string{"id", "namespace", "content", "kind", "source", "expires_at", "propagation", "pin", "created_at", "score"}))
	w := doRequest(h, "POST", "/v1/search", contract.SearchRequest{
		Namespaces: []string{"workspace:abc"},
	})
	if w.Code != 200 {
		t.Errorf("code = %d body=%s", w.Code, w.Body.String())
	}
}

func TestSearch_KindsFilter(t *testing.T) {
	db, mock := setupMockDB(t)
	h := newTestHandler(t, db, nil)
	mock.ExpectQuery("SELECT id, namespace, content").
		WillReturnRows(sqlmock.NewRows([]string{"id", "namespace", "content", "kind", "source", "expires_at", "propagation", "pin", "created_at", "score"}))
	w := doRequest(h, "POST", "/v1/search", contract.SearchRequest{
		Namespaces: []string{"workspace:abc"},
		Kinds:      []contract.MemoryKind{contract.MemoryKindFact, contract.MemoryKindSummary},
	})
	if w.Code != 200 {
		t.Errorf("code = %d body=%s", w.Code, w.Body.String())
	}
}

func TestSearch_RejectsEmpty(t *testing.T) {
	db, _ := setupMockDB(t)
	h := newTestHandler(t, db, nil)
	w := doRequest(h, "POST", "/v1/search", contract.SearchRequest{})
	if w.Code != 400 {
		t.Errorf("code = %d, want 400", w.Code)
	}
}

func TestSearch_RejectsBadJSON(t *testing.T) {
	db, _ := setupMockDB(t)
	h := newTestHandler(t, db, nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/search", strings.NewReader("not-json"))
	h.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Errorf("code = %d, want 400", w.Code)
	}
}

func TestSearch_StoreError(t *testing.T) {
	db, mock := setupMockDB(t)
	h := newTestHandler(t, db, nil)
	mock.ExpectQuery("SELECT id, namespace, content").
		WillReturnError(errors.New("db dead"))
	w := doRequest(h, "POST", "/v1/search", contract.SearchRequest{
		Namespaces: []string{"workspace:abc"},
	})
	if w.Code != 500 {
		t.Errorf("code = %d, want 500", w.Code)
	}
}

// --- ForgetMemory ---

func TestForgetMemory_HappyPath(t *testing.T) {
	db, mock := setupMockDB(t)
	h := newTestHandler(t, db, nil)
	mock.ExpectExec("DELETE FROM memory_records").
		WithArgs("mem-1", "workspace:abc").
		WillReturnResult(sqlmock.NewResult(0, 1))
	w := doRequest(h, "DELETE", "/v1/memories/mem-1", contract.ForgetRequest{RequestedByNamespace: "workspace:abc"})
	if w.Code != 204 {
		t.Errorf("code = %d body=%s", w.Code, w.Body.String())
	}
}

func TestForgetMemory_NotFoundOrWrongNamespace(t *testing.T) {
	db, mock := setupMockDB(t)
	h := newTestHandler(t, db, nil)
	mock.ExpectExec("DELETE FROM memory_records").
		WillReturnResult(sqlmock.NewResult(0, 0))
	w := doRequest(h, "DELETE", "/v1/memories/mem-1", contract.ForgetRequest{RequestedByNamespace: "workspace:abc"})
	if w.Code != 404 {
		t.Errorf("code = %d, want 404", w.Code)
	}
}

func TestForgetMemory_RejectsEmptyID(t *testing.T) {
	db, _ := setupMockDB(t)
	h := newTestHandler(t, db, nil)
	// Empty trailing id "/v1/memories/" matches the prefix; handler
	// extracts an empty id and rejects with 400.
	w := doRequest(h, "DELETE", "/v1/memories/", contract.ForgetRequest{RequestedByNamespace: "workspace:abc"})
	if w.Code != 400 {
		t.Errorf("code = %d body=%s want 400", w.Code, w.Body.String())
	}
}

func TestForgetMemory_RejectsBadJSON(t *testing.T) {
	db, _ := setupMockDB(t)
	h := newTestHandler(t, db, nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("DELETE", "/v1/memories/mem-1", strings.NewReader("not-json"))
	h.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Errorf("code = %d, want 400", w.Code)
	}
}

func TestForgetMemory_RejectsBadBody(t *testing.T) {
	db, _ := setupMockDB(t)
	h := newTestHandler(t, db, nil)
	w := doRequest(h, "DELETE", "/v1/memories/mem-1", contract.ForgetRequest{RequestedByNamespace: "BAD-NS"})
	if w.Code != 400 {
		t.Errorf("code = %d, want 400", w.Code)
	}
}

func TestForgetMemory_StoreError(t *testing.T) {
	db, mock := setupMockDB(t)
	h := newTestHandler(t, db, nil)
	mock.ExpectExec("DELETE FROM memory_records").
		WillReturnError(errors.New("db dead"))
	w := doRequest(h, "DELETE", "/v1/memories/mem-1", contract.ForgetRequest{RequestedByNamespace: "workspace:abc"})
	if w.Code != 500 {
		t.Errorf("code = %d, want 500", w.Code)
	}
}

// --- Routing edge cases ---

func TestRouting_Unknown(t *testing.T) {
	db, _ := setupMockDB(t)
	h := newTestHandler(t, db, nil)
	w := doRequest(h, "GET", "/no/such/route", nil)
	if w.Code != 404 {
		t.Errorf("code = %d, want 404", w.Code)
	}
}

func TestRouting_NamespacesEmpty(t *testing.T) {
	db, _ := setupMockDB(t)
	h := newTestHandler(t, db, nil)
	w := doRequest(h, "PUT", "/v1/namespaces/", contract.NamespaceUpsert{Kind: contract.NamespaceKindWorkspace})
	if w.Code != 400 {
		t.Errorf("code = %d, want 400 for missing name", w.Code)
	}
}

func TestRouting_NamespaceUnknownSub(t *testing.T) {
	db, _ := setupMockDB(t)
	h := newTestHandler(t, db, nil)
	w := doRequest(h, "GET", "/v1/namespaces/workspace:abc/whatever", nil)
	if w.Code != 404 {
		t.Errorf("code = %d, want 404", w.Code)
	}
}

func TestRouting_NamespaceMethodNotAllowed(t *testing.T) {
	db, _ := setupMockDB(t)
	h := newTestHandler(t, db, nil)
	w := doRequest(h, "POST", "/v1/namespaces/workspace:abc", nil)
	if w.Code != 405 {
		t.Errorf("code = %d, want 405", w.Code)
	}
}

func TestRouting_HealthWrongMethod(t *testing.T) {
	db, _ := setupMockDB(t)
	h := newTestHandler(t, db, nil)
	w := doRequest(h, "POST", "/v1/health", nil)
	if w.Code != 404 {
		t.Errorf("code = %d, want 404", w.Code)
	}
}

func TestRouting_SearchWrongMethod(t *testing.T) {
	db, _ := setupMockDB(t)
	h := newTestHandler(t, db, nil)
	w := doRequest(h, "GET", "/v1/search", nil)
	if w.Code != 404 {
		t.Errorf("code = %d, want 404", w.Code)
	}
}

// --- writeJSON / writeError direct ---

func TestWriteError_IncludesDetails(t *testing.T) {
	w := httptest.NewRecorder()
	writeError(w, 422, contract.ErrorCodeBadRequest, "bad", map[string]interface{}{"field": "kind"})
	if w.Code != 422 {
		t.Errorf("code = %d", w.Code)
	}
	body, _ := io.ReadAll(w.Body)
	if !strings.Contains(string(body), `"field"`) {
		t.Errorf("details lost: %s", body)
	}
}

func TestWriteJSON_SetsContentType(t *testing.T) {
	w := httptest.NewRecorder()
	writeJSON(w, 200, map[string]string{"k": "v"})
	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("content-type = %q", got)
	}
}
