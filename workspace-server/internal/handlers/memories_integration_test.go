//go:build integration
// +build integration

// memories_integration_test.go — REAL Postgres integration tests for the
// #2517 memory-write FK outage (fleet-wide 2026-06-10).
//
// Run with:
//
//   docker run --rm -d --name pg-mem-integ \
//     -e POSTGRES_PASSWORD=test -e POSTGRES_DB=molecule \
//     -p 55432:5432 pgvector/pgvector:pg15
//   sleep 4
//   psql ... < workspace-server/cmd/memory-plugin-postgres/migrations/001_memory_v2.up.sql
//   cd workspace-server
//   INTEGRATION_DB_URL="postgres://postgres:test@localhost:55432/molecule?sslmode=disable" \
//     go test -tags=integration ./internal/handlers/ -run "^TestIntegration_Memories"
//
// CI: Handlers Postgres Integration workflow (handlers-postgres-integration.yml)
//     already starts postgres and applies migrations. The test applies the
//     memory plugin schema inline if the tables are missing.

package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/memory/contract"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/memory/namespace"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/memory/pgplugin"
	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
)

// pgpluginAdapter wraps *pgplugin.Store to satisfy memoryPluginAPI.
// The store's ForgetMemory takes (ctx, id, namespace) while the interface
// takes (ctx, id, contract.ForgetRequest); this adapter bridges the gap.
type pgpluginAdapter struct {
	store *pgplugin.Store
}

func (a *pgpluginAdapter) UpsertNamespace(ctx context.Context, name string, body contract.NamespaceUpsert) (*contract.Namespace, error) {
	return a.store.UpsertNamespace(ctx, name, body)
}

func (a *pgpluginAdapter) CommitMemory(ctx context.Context, namespace string, body contract.MemoryWrite) (*contract.MemoryWriteResponse, error) {
	return a.store.CommitMemory(ctx, namespace, body)
}

func (a *pgpluginAdapter) Search(ctx context.Context, body contract.SearchRequest) (*contract.SearchResponse, error) {
	return a.store.Search(ctx, body)
}

func (a *pgpluginAdapter) ForgetMemory(ctx context.Context, id string, _ contract.ForgetRequest) error {
	// The integration test only exercises Commit, so the exact namespace
	// extraction from ForgetRequest is not load-bearing here.
	return a.store.ForgetMemory(ctx, id, "")
}

// memoryIntegrationDB returns a real postgres connection for memory tests.
// It applies the memory plugin schema if missing and cleans up tables on
// t.Cleanup so tests are hermetic.
func memoryIntegrationDB(t *testing.T) *sql.DB {
	t.Helper()
	url := requireIntegrationDBURL(t)
	conn, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := conn.PingContext(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}

	// Apply memory plugin schema if tables are missing (the CI workflow
	// only applies workspace-server/migrations/*.sql, not the plugin's
	// own migrations under cmd/memory-plugin-postgres/migrations/).
	//
	// We create the pgvector extension first so the vector(1536) column
	// type resolves. The CI workflow uses pgvector/pgvector:pg15 and
	// creates the extension before migrations; if it is still missing here,
	// the test environment is mis-configured and must fail loud.
	if _, err := conn.ExecContext(ctx, `CREATE EXTENSION IF NOT EXISTS vector;`); err != nil {
		t.Fatalf("pgvector extension unavailable — memory integration tests require pgvector: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS memory_namespaces (
		    name        TEXT PRIMARY KEY,
		    kind        TEXT NOT NULL CHECK (kind IN ('workspace','team','org','custom')),
		    expires_at  TIMESTAMPTZ,
		    metadata    JSONB,
		    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
		);
		CREATE TABLE IF NOT EXISTS memory_records (
		    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		    namespace    TEXT NOT NULL REFERENCES memory_namespaces(name) ON DELETE CASCADE,
		    content      TEXT NOT NULL,
		    kind         TEXT NOT NULL CHECK (kind IN ('fact','summary','checkpoint')),
		    source       TEXT NOT NULL CHECK (source IN ('agent','runtime','user')),
		    expires_at   TIMESTAMPTZ,
		    propagation  JSONB,
		    pin          BOOLEAN NOT NULL DEFAULT false,
		    embedding    vector(1536),
		    content_tsv  tsvector GENERATED ALWAYS AS (to_tsvector('english', content)) STORED,
		    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
		);
	`); err != nil {
		t.Fatalf("memory schema apply failed: %v", err)
	}

	// Clean slate: delete all memory rows so tests are hermetic.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	if _, err := conn.ExecContext(ctx2, `DELETE FROM memory_records`); err != nil {
		if !strings.Contains(err.Error(), "does not exist") {
			t.Fatalf("cleanup memory_records: %v", err)
		}
	}
	if _, err := conn.ExecContext(ctx2, `DELETE FROM memory_namespaces`); err != nil {
		if !strings.Contains(err.Error(), "does not exist") {
			t.Fatalf("cleanup memory_namespaces: %v", err)
		}
	}

	t.Cleanup(func() { conn.Close() })
	return conn
}

// TestIntegration_MemoriesCommit_NoNamespace_UpsertsAndWrites pins the
// #2517 fleet-wide regression: the HTTP Commit path skipped namespace
// upsert, so any workspace whose memory_namespaces row was never seeded
// failed every write with memory_records_namespace_fkey.
//
// This test uses a REAL postgres (no stubs) and asserts the observable
// row state: after Commit returns 201, both the namespace row and the
// memory record exist in the DB.
func TestIntegration_MemoriesCommit_NoNamespace_UpsertsAndWrites(t *testing.T) {
	conn := memoryIntegrationDB(t)
	gin.SetMode(gin.TestMode)

	wsID := seedWorkspace(t, conn, "fk-integ-ws")
	adapter := &pgpluginAdapter{store: pgplugin.NewStore(conn)}
	resolver := namespace.New(conn)
	handler := NewMemoriesHandler().withMemoryV2APIs(adapter, resolver)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest("POST", "/", bytes.NewBufferString(`{"content":"integration test memory","scope":"LOCAL"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Commit(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	// Verify the namespace row was auto-created.
	var nsCount int
	err := conn.QueryRowContext(context.Background(),
		`SELECT count(*) FROM memory_namespaces WHERE name = $1`, "workspace:"+wsID).Scan(&nsCount)
	if err != nil {
		t.Fatalf("select namespace: %v", err)
	}
	if nsCount != 1 {
		t.Errorf("namespace row missing — upsert did not run before commit (count=%d)", nsCount)
	}

	// Verify the memory record landed.
	var memCount int
	err = conn.QueryRowContext(context.Background(),
		`SELECT count(*) FROM memory_records WHERE namespace = $1 AND content = $2`,
		"workspace:"+wsID, "integration test memory").Scan(&memCount)
	if err != nil {
		t.Fatalf("select memory record: %v", err)
	}
	if memCount != 1 {
		t.Errorf("memory record missing — write did not land (count=%d)", memCount)
	}
}

// TestIntegration_MemoriesCommit_NamespaceAlreadyExists_Idempotent pins
// that the upsert is harmless when the namespace already exists (warm
// path — no duplicate rows, no error).
func TestIntegration_MemoriesCommit_NamespaceAlreadyExists_Idempotent(t *testing.T) {
	conn := memoryIntegrationDB(t)
	gin.SetMode(gin.TestMode)

	wsID := seedWorkspace(t, conn, "warm-ws")
	store := pgplugin.NewStore(conn)
	adapter := &pgpluginAdapter{store: store}
	resolver := namespace.New(conn)
	handler := NewMemoriesHandler().withMemoryV2APIs(adapter, resolver)

	nsName := "workspace:" + wsID
	// Pre-seed the namespace.
	if _, err := store.UpsertNamespace(context.Background(), nsName, contract.NamespaceUpsert{Kind: contract.NamespaceKindWorkspace}); err != nil {
		t.Fatalf("pre-seed namespace: %v", err)
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest("POST", "/", bytes.NewBufferString(`{"content":"warm path memory","scope":"LOCAL"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Commit(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	// Must still be exactly one namespace row.
	var nsCount int
	err := conn.QueryRowContext(context.Background(),
		`SELECT count(*) FROM memory_namespaces WHERE name = $1`, nsName).Scan(&nsCount)
	if err != nil {
		t.Fatalf("select namespace: %v", err)
	}
	if nsCount != 1 {
		t.Errorf("duplicate namespace rows created (count=%d)", nsCount)
	}
}
