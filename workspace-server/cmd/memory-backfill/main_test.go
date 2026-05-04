package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/memory/contract"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/memory/namespace"
)

// stubBackfillPlugin records calls for assertions.
type stubBackfillPlugin struct {
	upsertedNamespaces  []string
	committedNamespaces []string
	committedIDs        []string // captures MemoryWrite.ID per call
	upsertErr           error
	commitErr           error
}

func (s *stubBackfillPlugin) UpsertNamespace(_ context.Context, name string, _ contract.NamespaceUpsert) (*contract.Namespace, error) {
	s.upsertedNamespaces = append(s.upsertedNamespaces, name)
	if s.upsertErr != nil {
		return nil, s.upsertErr
	}
	return &contract.Namespace{Name: name, Kind: contract.NamespaceKindWorkspace}, nil
}
func (s *stubBackfillPlugin) CommitMemory(_ context.Context, ns string, body contract.MemoryWrite) (*contract.MemoryWriteResponse, error) {
	s.committedNamespaces = append(s.committedNamespaces, ns)
	s.committedIDs = append(s.committedIDs, body.ID)
	if s.commitErr != nil {
		return nil, s.commitErr
	}
	id := body.ID
	if id == "" {
		id = "out-1"
	}
	return &contract.MemoryWriteResponse{ID: id, Namespace: ns}, nil
}

type stubBackfillResolver struct {
	writable []namespace.Namespace
	err      error
}

func (s *stubBackfillResolver) WritableNamespaces(_ context.Context, _ string) ([]namespace.Namespace, error) {
	return s.writable, s.err
}

func rootBackfillResolver() *stubBackfillResolver {
	return &stubBackfillResolver{
		writable: []namespace.Namespace{
			{Name: "workspace:root-1", Kind: contract.NamespaceKindWorkspace, Writable: true},
			{Name: "team:root-1", Kind: contract.NamespaceKindTeam, Writable: true},
			{Name: "org:root-1", Kind: contract.NamespaceKindOrg, Writable: true},
		},
	}
}

// --- mapScopeToNamespace ---

func TestMapScopeToNamespace(t *testing.T) {
	cases := []struct {
		scope   string
		want    string
		wantErr string
	}{
		{"LOCAL", "workspace:root-1", ""},
		{"TEAM", "team:root-1", ""},
		{"GLOBAL", "org:root-1", ""},
		{"WEIRD", "", "unknown scope"},
	}
	for _, tc := range cases {
		t.Run(tc.scope, func(t *testing.T) {
			got, err := mapScopeToNamespace(context.Background(), rootBackfillResolver(), "root-1", tc.scope)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMapScopeToNamespace_ResolverError(t *testing.T) {
	r := &stubBackfillResolver{err: errors.New("dead")}
	_, err := mapScopeToNamespace(context.Background(), r, "root-1", "LOCAL")
	if err == nil {
		t.Error("expected error")
	}
}

func TestMapScopeToNamespace_NoMatchingKind(t *testing.T) {
	r := &stubBackfillResolver{writable: []namespace.Namespace{
		{Name: "workspace:x", Kind: contract.NamespaceKindWorkspace, Writable: true},
	}}
	_, err := mapScopeToNamespace(context.Background(), r, "root-1", "TEAM")
	if err == nil || !strings.Contains(err.Error(), "no writable namespace") {
		t.Errorf("err = %v", err)
	}
}

// --- namespaceKindFromString ---

func TestNamespaceKindFromString(t *testing.T) {
	cases := []struct {
		in   string
		want contract.NamespaceKind
	}{
		{"LOCAL", contract.NamespaceKindWorkspace},
		{"local", contract.NamespaceKindWorkspace},
		{"TEAM", contract.NamespaceKindTeam},
		{"team", contract.NamespaceKindTeam},
		{"GLOBAL", contract.NamespaceKindOrg},
		{"global", contract.NamespaceKindOrg},
		{"weird", contract.NamespaceKindWorkspace}, // safe default
		{"", contract.NamespaceKindWorkspace},
	}
	for _, tc := range cases {
		if got := namespaceKindFromString(tc.in); got != tc.want {
			t.Errorf("namespaceKindFromString(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// --- backfill (the workhorse) ---

// TestBackfill_PassesSourceUUIDAsIdempotencyKey pins the Critical-1
// fix: backfill must forward agent_memories.id to MemoryWrite.ID so
// re-runs upsert in place.
func TestBackfill_PassesSourceUUIDAsIdempotencyKey(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	now := time.Now().UTC()
	mock.ExpectQuery("SELECT id, workspace_id, content, scope, created_at").
		WillReturnRows(sqlmock.NewRows([]string{"id", "workspace_id", "content", "scope", "created_at"}).
			AddRow("source-uuid-A", "root-1", "fact 1", "LOCAL", now).
			AddRow("source-uuid-B", "root-1", "fact 2", "LOCAL", now))

	plugin := &stubBackfillPlugin{}
	cfg := backfillConfig{DB: db, Plugin: plugin, Resolver: rootBackfillResolver(), Limit: 100}
	devnull, _ := os.Open(os.DevNull)
	defer devnull.Close()
	if _, err := backfill(context.Background(), cfg, devnull); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if len(plugin.committedIDs) != 2 {
		t.Fatalf("commits = %d", len(plugin.committedIDs))
	}
	if plugin.committedIDs[0] != "source-uuid-A" || plugin.committedIDs[1] != "source-uuid-B" {
		t.Errorf("committedIDs = %v; idempotency key not forwarded", plugin.committedIDs)
	}
}

// TestBackfill_RerunIsIdempotent: same agent_memories rows backfilled
// twice. Plugin sees the same UUIDs both times; without the fix the
// plugin would generate fresh UUIDs and duplicate.
func TestBackfill_RerunIsIdempotent(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	now := time.Now().UTC()
	rows1 := sqlmock.NewRows([]string{"id", "workspace_id", "content", "scope", "created_at"}).
		AddRow("uuid-1", "root-1", "fact", "LOCAL", now)
	rows2 := sqlmock.NewRows([]string{"id", "workspace_id", "content", "scope", "created_at"}).
		AddRow("uuid-1", "root-1", "fact", "LOCAL", now)
	mock.ExpectQuery("SELECT id, workspace_id, content, scope, created_at").WillReturnRows(rows1)
	mock.ExpectQuery("SELECT id, workspace_id, content, scope, created_at").WillReturnRows(rows2)

	plugin := &stubBackfillPlugin{}
	cfg := backfillConfig{DB: db, Plugin: plugin, Resolver: rootBackfillResolver(), Limit: 100}
	devnull, _ := os.Open(os.DevNull)
	defer devnull.Close()

	if _, err := backfill(context.Background(), cfg, devnull); err != nil {
		t.Fatal(err)
	}
	if _, err := backfill(context.Background(), cfg, devnull); err != nil {
		t.Fatal(err)
	}
	if len(plugin.committedIDs) != 2 {
		t.Errorf("commits = %d, want 2", len(plugin.committedIDs))
	}
	if plugin.committedIDs[0] != "uuid-1" || plugin.committedIDs[1] != "uuid-1" {
		t.Errorf("ids = %v; both runs must pass uuid-1 (relies on plugin upsert for actual de-dup)", plugin.committedIDs)
	}
}

func TestBackfill_HappyPath_Apply(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	now := time.Now().UTC()
	mock.ExpectQuery("SELECT id, workspace_id, content, scope, created_at").
		WillReturnRows(sqlmock.NewRows([]string{"id", "workspace_id", "content", "scope", "created_at"}).
			AddRow("mem-1", "root-1", "fact x", "LOCAL", now).
			AddRow("mem-2", "root-1", "team y", "TEAM", now).
			AddRow("mem-3", "root-1", "org z", "GLOBAL", now))

	plugin := &stubBackfillPlugin{}
	cfg := backfillConfig{
		DB:       db,
		Plugin:   plugin,
		Resolver: rootBackfillResolver(),
		Limit:    100,
		DryRun:   false,
	}
	devnull, _ := os.Open(os.DevNull)
	defer devnull.Close()
	stats, err := backfill(context.Background(), cfg, devnull)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if stats.Scanned != 3 || stats.Copied != 3 || stats.Errors != 0 {
		t.Errorf("stats = %+v", stats)
	}
	if len(plugin.committedNamespaces) != 3 {
		t.Errorf("commits = %v", plugin.committedNamespaces)
	}
}

func TestBackfill_DryRun_DoesNotCallPlugin(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	now := time.Now().UTC()
	mock.ExpectQuery("SELECT id, workspace_id, content, scope, created_at").
		WillReturnRows(sqlmock.NewRows([]string{"id", "workspace_id", "content", "scope", "created_at"}).
			AddRow("mem-1", "root-1", "fact x", "LOCAL", now))

	plugin := &stubBackfillPlugin{}
	cfg := backfillConfig{DB: db, Plugin: plugin, Resolver: rootBackfillResolver(), Limit: 100, DryRun: true}
	devnull, _ := os.Open(os.DevNull)
	defer devnull.Close()
	stats, err := backfill(context.Background(), cfg, devnull)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if stats.Copied != 1 {
		t.Errorf("copied = %d", stats.Copied)
	}
	if len(plugin.committedNamespaces) != 0 {
		t.Errorf("plugin must not be called in dry-run mode")
	}
}

func TestBackfill_WorkspaceFilter(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	mock.ExpectQuery("SELECT id, workspace_id, content, scope, created_at").
		WithArgs("specific-ws", 100).
		WillReturnRows(sqlmock.NewRows([]string{"id", "workspace_id", "content", "scope", "created_at"}))
	cfg := backfillConfig{DB: db, Plugin: &stubBackfillPlugin{}, Resolver: rootBackfillResolver(), Limit: 100, WorkspaceID: "specific-ws"}
	devnull, _ := os.Open(os.DevNull)
	defer devnull.Close()
	if _, err := backfill(context.Background(), cfg, devnull); err != nil {
		t.Fatalf("err: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("workspace filter not applied: %v", err)
	}
}

func TestBackfill_QueryError(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	mock.ExpectQuery("SELECT id, workspace_id, content, scope, created_at").
		WillReturnError(errors.New("dead"))
	cfg := backfillConfig{DB: db, Plugin: &stubBackfillPlugin{}, Resolver: rootBackfillResolver(), Limit: 100}
	devnull, _ := os.Open(os.DevNull)
	defer devnull.Close()
	_, err := backfill(context.Background(), cfg, devnull)
	if err == nil {
		t.Error("expected error")
	}
}

func TestBackfill_ScanError(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	mock.ExpectQuery("SELECT id, workspace_id, content, scope, created_at").
		WillReturnRows(sqlmock.NewRows([]string{"id"}). // wrong shape
								AddRow("mem-1"))
	cfg := backfillConfig{DB: db, Plugin: &stubBackfillPlugin{}, Resolver: rootBackfillResolver(), Limit: 100}
	devnull, _ := os.Open(os.DevNull)
	defer devnull.Close()
	stats, err := backfill(context.Background(), cfg, devnull)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if stats.Errors != 1 {
		t.Errorf("errors = %d, want 1", stats.Errors)
	}
}

func TestBackfill_RowsErr(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	mock.ExpectQuery("SELECT id, workspace_id, content, scope, created_at").
		WillReturnRows(sqlmock.NewRows([]string{"id", "workspace_id", "content", "scope", "created_at"}).
			AddRow("mem-1", "root-1", "x", "LOCAL", time.Now().UTC()).
			RowError(0, errors.New("mid-iter")))
	cfg := backfillConfig{DB: db, Plugin: &stubBackfillPlugin{}, Resolver: rootBackfillResolver(), Limit: 100}
	devnull, _ := os.Open(os.DevNull)
	defer devnull.Close()
	_, err := backfill(context.Background(), cfg, devnull)
	if err == nil || !strings.Contains(err.Error(), "iterate") {
		t.Errorf("err = %v", err)
	}
}

func TestBackfill_SkipsUnmappableRow(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	mock.ExpectQuery("SELECT id, workspace_id, content, scope, created_at").
		WillReturnRows(sqlmock.NewRows([]string{"id", "workspace_id", "content", "scope", "created_at"}).
			AddRow("mem-1", "root-1", "x", "WEIRD", time.Now().UTC()))
	cfg := backfillConfig{DB: db, Plugin: &stubBackfillPlugin{}, Resolver: rootBackfillResolver(), Limit: 100}
	devnull, _ := os.Open(os.DevNull)
	defer devnull.Close()
	stats, err := backfill(context.Background(), cfg, devnull)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if stats.Skipped != 1 || stats.Copied != 0 {
		t.Errorf("stats = %+v", stats)
	}
}

func TestBackfill_PluginUpsertNamespaceError(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	mock.ExpectQuery("SELECT id, workspace_id, content, scope, created_at").
		WillReturnRows(sqlmock.NewRows([]string{"id", "workspace_id", "content", "scope", "created_at"}).
			AddRow("mem-1", "root-1", "x", "LOCAL", time.Now().UTC()))
	cfg := backfillConfig{DB: db, Plugin: &stubBackfillPlugin{upsertErr: errors.New("ns dead")}, Resolver: rootBackfillResolver(), Limit: 100}
	devnull, _ := os.Open(os.DevNull)
	defer devnull.Close()
	stats, err := backfill(context.Background(), cfg, devnull)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if stats.Errors != 1 || stats.Copied != 0 {
		t.Errorf("stats = %+v", stats)
	}
}

func TestBackfill_PluginCommitMemoryError(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	mock.ExpectQuery("SELECT id, workspace_id, content, scope, created_at").
		WillReturnRows(sqlmock.NewRows([]string{"id", "workspace_id", "content", "scope", "created_at"}).
			AddRow("mem-1", "root-1", "x", "LOCAL", time.Now().UTC()))
	cfg := backfillConfig{DB: db, Plugin: &stubBackfillPlugin{commitErr: errors.New("mem dead")}, Resolver: rootBackfillResolver(), Limit: 100}
	devnull, _ := os.Open(os.DevNull)
	defer devnull.Close()
	stats, err := backfill(context.Background(), cfg, devnull)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if stats.Errors != 1 || stats.Copied != 0 {
		t.Errorf("stats = %+v", stats)
	}
}

// --- run (CLI driver) ---

func TestRun_RejectsBothModes(t *testing.T) {
	stderr, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	defer stderr.Close()
	stdout, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	defer stdout.Close()
	err := run([]string{"-dry-run", "-apply"}, stdout, stderr)
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Errorf("err = %v", err)
	}
}

func TestRun_RejectsNeitherMode(t *testing.T) {
	stderr, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	defer stderr.Close()
	stdout, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	defer stdout.Close()
	err := run([]string{}, stdout, stderr)
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Errorf("err = %v", err)
	}
}

func TestRun_RejectsMissingDatabaseURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("MEMORY_PLUGIN_URL", "http://x")
	stderr, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	defer stderr.Close()
	stdout, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	defer stdout.Close()
	err := run([]string{"-dry-run"}, stdout, stderr)
	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Errorf("err = %v", err)
	}
}

func TestRun_RejectsMissingPluginURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://invalid")
	t.Setenv("MEMORY_PLUGIN_URL", "")
	stderr, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	defer stderr.Close()
	stdout, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	defer stdout.Close()
	err := run([]string{"-dry-run"}, stdout, stderr)
	if err == nil || !strings.Contains(err.Error(), "MEMORY_PLUGIN_URL") {
		t.Errorf("err = %v", err)
	}
}

func TestRun_BadFlags(t *testing.T) {
	stderr, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	defer stderr.Close()
	stdout, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	defer stdout.Close()
	err := run([]string{"-not-a-flag"}, stdout, stderr)
	if err == nil {
		t.Error("expected flag parse error")
	}
}
