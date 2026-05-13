package pgplugin

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/memory/contract"
)

// --- marshalMetadata corner cases ---

func TestMarshalMetadata_Nil(t *testing.T) {
	got, err := marshalMetadata(nil)
	if err != nil {
		t.Errorf("err = %v", err)
	}
	if got != nil {
		t.Errorf("got = %v, want nil", got)
	}
}

func TestMarshalMetadata_HappyPath(t *testing.T) {
	got, err := marshalMetadata(map[string]interface{}{"k": "v"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(string(got), `"k":"v"`) {
		t.Errorf("got = %s", got)
	}
}

func TestMarshalMetadata_Unmarshalable(t *testing.T) {
	// Channels cannot be JSON-encoded — exercises the error branch.
	_, err := marshalMetadata(map[string]interface{}{"chan": make(chan int)})
	if err == nil || !strings.Contains(err.Error(), "marshal metadata") {
		t.Errorf("err = %v, want wrapped marshal error", err)
	}
}

// --- nullTime ---

func TestNullTime_Nil(t *testing.T) {
	got := nullTime(nil)
	if got.Valid {
		t.Errorf("nil pointer should give invalid NullTime")
	}
}

func TestNullTime_NonNil(t *testing.T) {
	now := time.Now().UTC()
	got := nullTime(&now)
	if !got.Valid || !got.Time.Equal(now) {
		t.Errorf("got = %v, want valid + equal", got)
	}
}

// --- vectorString ---

func TestVectorString_Empty(t *testing.T) {
	if got := vectorString(nil); got != "" {
		t.Errorf("got = %q, want empty", got)
	}
}

func TestVectorString_Format(t *testing.T) {
	got := vectorString([]float32{0.1, 0.2, 0.3})
	if got != "[0.1,0.2,0.3]" {
		t.Errorf("got = %q", got)
	}
}

func TestNullVectorString_EmptyReturnsNil(t *testing.T) {
	if got := nullVectorString(nil); got != nil {
		t.Errorf("got = %v, want nil", got)
	}
}

func TestNullVectorString_NonEmptyReturnsString(t *testing.T) {
	got := nullVectorString([]float32{1.0})
	if got != "[1]" {
		t.Errorf("got = %v, want [1]", got)
	}
}

// --- Store error paths via direct calls ---

func TestStore_UpsertNamespace_MarshalError(t *testing.T) {
	db, _ := setupMockDB(t)
	store := NewStore(db)
	_, err := store.UpsertNamespace(context.Background(), "workspace:abc", contract.NamespaceUpsert{
		Kind:     contract.NamespaceKindWorkspace,
		Metadata: map[string]interface{}{"chan": make(chan int)},
	})
	if err == nil || !strings.Contains(err.Error(), "marshal") {
		t.Errorf("err = %v, want marshal error", err)
	}
}

func TestStore_UpsertNamespace_ScanError(t *testing.T) {
	db, mock := setupMockDB(t)
	store := NewStore(db)
	mock.ExpectQuery("INSERT INTO memory_namespaces").
		WillReturnRows(sqlmock.NewRows([]string{"name"}). // wrong shape
								AddRow("x"))
	_, err := store.UpsertNamespace(context.Background(), "workspace:abc", contract.NamespaceUpsert{Kind: contract.NamespaceKindWorkspace})
	if err == nil || !strings.Contains(err.Error(), "scan") {
		t.Errorf("err = %v, want scan error", err)
	}
}

func TestStore_PatchNamespace_MarshalError(t *testing.T) {
	db, _ := setupMockDB(t)
	store := NewStore(db)
	_, err := store.PatchNamespace(context.Background(), "workspace:abc", contract.NamespacePatch{
		Metadata: map[string]interface{}{"chan": make(chan int)},
	})
	if err == nil || !strings.Contains(err.Error(), "marshal") {
		t.Errorf("err = %v, want marshal error", err)
	}
}

func TestStore_DeleteNamespace_RowsAffectedError(t *testing.T) {
	db, mock := setupMockDB(t)
	store := NewStore(db)
	mock.ExpectExec("DELETE FROM memory_namespaces").
		WillReturnResult(sqlmock.NewErrorResult(errors.New("rows error")))
	err := store.DeleteNamespace(context.Background(), "workspace:abc")
	if err == nil || !strings.Contains(err.Error(), "rows") {
		t.Errorf("err = %v, want rows error", err)
	}
}

func TestStore_CommitMemory_MarshalError(t *testing.T) {
	db, _ := setupMockDB(t)
	store := NewStore(db)
	_, err := store.CommitMemory(context.Background(), "workspace:abc", contract.MemoryWrite{
		Content:     "x",
		Kind:        contract.MemoryKindFact,
		Source:      contract.MemorySourceAgent,
		Propagation: map[string]interface{}{"chan": make(chan int)},
	})
	if err == nil || !strings.Contains(err.Error(), "marshal") {
		t.Errorf("err = %v, want marshal error", err)
	}
}

func TestStore_ForgetMemory_RowsAffectedError(t *testing.T) {
	db, mock := setupMockDB(t)
	store := NewStore(db)
	mock.ExpectExec("DELETE FROM memory_records").
		WillReturnResult(sqlmock.NewErrorResult(errors.New("rows error")))
	err := store.ForgetMemory(context.Background(), "mem-1", "workspace:abc")
	if err == nil || !strings.Contains(err.Error(), "rows") {
		t.Errorf("err = %v, want rows error", err)
	}
}

func TestStore_Search_ScanError(t *testing.T) {
	db, mock := setupMockDB(t)
	store := NewStore(db)
	mock.ExpectQuery("SELECT id, namespace, content").
		WillReturnRows(sqlmock.NewRows([]string{"id"}). // wrong shape
								AddRow("x"))
	_, err := store.Search(context.Background(), contract.SearchRequest{Namespaces: []string{"workspace:abc"}})
	if err == nil || !strings.Contains(err.Error(), "scan") {
		t.Errorf("err = %v, want scan error", err)
	}
}

func TestStore_Search_RowsErr(t *testing.T) {
	db, mock := setupMockDB(t)
	store := NewStore(db)
	mock.ExpectQuery("SELECT id, namespace, content").
		WillReturnRows(sqlmock.NewRows([]string{"id", "namespace", "content", "kind", "source", "expires_at", "propagation", "pin", "created_at", "score"}).
			AddRow("id-1", "workspace:abc", "x", "fact", "agent", nil, nil, false, time.Now(), nil).
			RowError(0, errors.New("rows broken")))
	_, err := store.Search(context.Background(), contract.SearchRequest{Namespaces: []string{"workspace:abc"}})
	if err == nil || !strings.Contains(err.Error(), "rows broken") {
		t.Errorf("err = %v, want rows error", err)
	}
}

func TestStore_Search_PropagatesQueryError(t *testing.T) {
	db, mock := setupMockDB(t)
	store := NewStore(db)
	mock.ExpectQuery("SELECT id, namespace, content").
		WillReturnError(errors.New("dead"))
	_, err := store.Search(context.Background(), contract.SearchRequest{Namespaces: []string{"workspace:abc"}})
	if err == nil || !strings.Contains(err.Error(), "search") {
		t.Errorf("err = %v, want wrapped error", err)
	}
}

func TestScanNamespace_MetadataDecodeError(t *testing.T) {
	db, mock := setupMockDB(t)
	store := NewStore(db)
	// Return invalid JSON in metadata column to exercise the unmarshal error.
	mock.ExpectQuery("INSERT INTO memory_namespaces").
		WillReturnRows(sqlmock.NewRows([]string{"name", "kind", "expires_at", "metadata", "created_at"}).
			AddRow("workspace:abc", "workspace", nil, []byte(`{not valid`), time.Now()))
	_, err := store.UpsertNamespace(context.Background(), "workspace:abc", contract.NamespaceUpsert{Kind: contract.NamespaceKindWorkspace})
	if err == nil || !strings.Contains(err.Error(), "unmarshal") {
		t.Errorf("err = %v, want unmarshal error", err)
	}
}

func TestScanMemory_PropagationDecodeError(t *testing.T) {
	db, mock := setupMockDB(t)
	store := NewStore(db)
	mock.ExpectQuery("SELECT id, namespace, content").
		WillReturnRows(sqlmock.NewRows([]string{"id", "namespace", "content", "kind", "source", "expires_at", "propagation", "pin", "created_at", "score"}).
			AddRow("id-1", "workspace:abc", "x", "fact", "agent", nil, []byte(`{not valid`), false, time.Now(), nil))
	_, err := store.Search(context.Background(), contract.SearchRequest{Namespaces: []string{"workspace:abc"}})
	if err == nil || !strings.Contains(err.Error(), "unmarshal") {
		t.Errorf("err = %v, want unmarshal error", err)
	}
}

func TestScanMemory_WithExpiresAndPropagation(t *testing.T) {
	db, mock := setupMockDB(t)
	store := NewStore(db)
	exp := time.Now().Add(time.Hour).UTC()
	mock.ExpectQuery("SELECT id, namespace, content").
		WillReturnRows(sqlmock.NewRows([]string{"id", "namespace", "content", "kind", "source", "expires_at", "propagation", "pin", "created_at", "score"}).
			AddRow("id-1", "workspace:abc", "x", "fact", "agent", exp, []byte(`{"hop":1}`), true, time.Now(), 0.9))
	resp, err := store.Search(context.Background(), contract.SearchRequest{Namespaces: []string{"workspace:abc"}})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(resp.Memories) != 1 {
		t.Fatalf("memories len = %d", len(resp.Memories))
	}
	m := resp.Memories[0]
	if m.ExpiresAt == nil || !m.ExpiresAt.Equal(exp) {
		t.Errorf("expires = %v", m.ExpiresAt)
	}
	if v, ok := m.Propagation["hop"].(float64); !ok || v != 1 {
		t.Errorf("propagation = %v", m.Propagation)
	}
	if !m.Pin {
		t.Errorf("pin should be true")
	}
}

func TestScanNamespace_WithExpiresAndMetadata(t *testing.T) {
	db, mock := setupMockDB(t)
	store := NewStore(db)
	exp := time.Now().Add(time.Hour).UTC()
	mock.ExpectQuery("INSERT INTO memory_namespaces").
		WillReturnRows(sqlmock.NewRows([]string{"name", "kind", "expires_at", "metadata", "created_at"}).
			AddRow("workspace:abc", "workspace", exp, []byte(`{"k":"v"}`), time.Now()))
	ns, err := store.UpsertNamespace(context.Background(), "workspace:abc", contract.NamespaceUpsert{Kind: contract.NamespaceKindWorkspace})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ns.ExpiresAt == nil || !ns.ExpiresAt.Equal(exp) {
		t.Errorf("expires = %v", ns.ExpiresAt)
	}
	if v, ok := ns.Metadata["k"].(string); !ok || v != "v" {
		t.Errorf("metadata = %v", ns.Metadata)
	}
}

// --- DeleteNamespace + ForgetMemory exec-error paths ---

func TestStore_DeleteNamespace_ExecError(t *testing.T) {
	db, mock := setupMockDB(t)
	store := NewStore(db)
	mock.ExpectExec("DELETE FROM memory_namespaces").
		WillReturnError(errors.New("dead"))
	err := store.DeleteNamespace(context.Background(), "workspace:abc")
	if err == nil || !strings.Contains(err.Error(), "delete namespace") {
		t.Errorf("err = %v, want wrapped delete error", err)
	}
}

func TestStore_ForgetMemory_ExecError(t *testing.T) {
	db, mock := setupMockDB(t)
	store := NewStore(db)
	mock.ExpectExec("DELETE FROM memory_records").
		WillReturnError(errors.New("dead"))
	err := store.ForgetMemory(context.Background(), "mem-1", "workspace:abc")
	if err == nil || !strings.Contains(err.Error(), "forget memory") {
		t.Errorf("err = %v, want wrapped forget error", err)
	}
}

func TestStore_PatchNamespace_NotFound_SqlNoRows(t *testing.T) {
	db, mock := setupMockDB(t)
	store := NewStore(db)
	exp := time.Now().Add(time.Hour).UTC()
	mock.ExpectQuery("UPDATE memory_namespaces").
		WillReturnError(sql.ErrNoRows)
	_, err := store.PatchNamespace(context.Background(), "workspace:abc", contract.NamespacePatch{ExpiresAt: &exp})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// TestStore_PatchNamespace_DualFields verifies that when both ExpiresAt and
// Metadata are set, the positional indexes are correct ($2 for expires_at,
// $3 for metadata).  Prior to ad7acd30 this was broken: the idx++ after the
// metadata branch was removed as a golangci-lint false-positive, causing
// metadata to be written as $2 (same slot as expires_at) and expires_at to
// be omitted from args entirely.
func TestStore_PatchNamespace_DualFields(t *testing.T) {
	db, mock := setupMockDB(t)
	store := NewStore(db)
	exp := time.Now().Add(time.Hour).UTC()
	// sqlmock matches by query string; we verify the query uses $2 and $3.
	mock.ExpectQuery("UPDATE memory_namespaces SET expires_at = \\$2, metadata = \\$3 WHERE name = \\$1").
		WithArgs("workspace:abc", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"name", "kind", "expires_at", "metadata", "created_at"}).
			AddRow("workspace:abc", "workspace", exp, []byte(`{}`), time.Now()))
	got, err := store.PatchNamespace(context.Background(), "workspace:abc", contract.NamespacePatch{
		ExpiresAt: &exp,
		Metadata:  map[string]interface{}{"key": "value"},
	})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got.Name != "workspace:abc" {
		t.Errorf("got.Name = %q, want workspace:abc", got.Name)
	}
}
