package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/memory/contract"
)

// stubVerifyPlugin records search calls and returns canned results.
type stubVerifyPlugin struct {
	searchFn func(ctx context.Context, body contract.SearchRequest) (*contract.SearchResponse, error)
}

func (s *stubVerifyPlugin) Search(ctx context.Context, body contract.SearchRequest) (*contract.SearchResponse, error) {
	if s.searchFn != nil {
		return s.searchFn(ctx, body)
	}
	return &contract.SearchResponse{}, nil
}

// stubVerifyResolver returns a canned readable namespace list.
type stubVerifyResolver struct {
	namespaces []ResolvedNamespace
	err        error
}

func (s *stubVerifyResolver) ReadableNamespaces(_ context.Context, _ string) ([]ResolvedNamespace, error) {
	return s.namespaces, s.err
}

// --- pickWorkspaceSample ---

func TestPickWorkspaceSample_SingleWorkspaceShortCircuit(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer db.Close()
	got, err := pickWorkspaceSample(context.Background(), db, "specific-ws", 50, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 1 || got[0] != "specific-ws" {
		t.Errorf("got %v, want [specific-ws]", got)
	}
}

func TestPickWorkspaceSample_RandomSample(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	mock.ExpectQuery("SELECT id::text FROM workspaces").
		WithArgs(50).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).
			AddRow("ws-1").
			AddRow("ws-2").
			AddRow("ws-3"))
	got, err := pickWorkspaceSample(context.Background(), db, "", 50, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("got len %d, want 3", len(got))
	}
}

func TestPickWorkspaceSample_QueryError(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	mock.ExpectQuery("SELECT id::text FROM workspaces").
		WillReturnError(errors.New("dead"))
	_, err := pickWorkspaceSample(context.Background(), db, "", 50, nil)
	if err == nil {
		t.Error("expected error")
	}
}

func TestPickWorkspaceSample_ScanError(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	mock.ExpectQuery("SELECT id::text FROM workspaces").
		WillReturnRows(sqlmock.NewRows([]string{"id", "extra"}). // wrong shape
								AddRow("ws-1", "extra"))
	_, err := pickWorkspaceSample(context.Background(), db, "", 50, nil)
	if err == nil {
		t.Error("expected scan error")
	}
}

// --- queryLegacyMemories ---

func TestQueryLegacyMemories_HappyPath(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	mock.ExpectQuery("SELECT content FROM agent_memories").
		WithArgs("ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"content"}).
			AddRow("fact 1").
			AddRow("fact 2"))
	got, err := queryLegacyMemories(context.Background(), db, "ws-1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 2 || got[0] != "fact 1" {
		t.Errorf("got %v", got)
	}
}

func TestQueryLegacyMemories_QueryError(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	mock.ExpectQuery("SELECT content FROM agent_memories").
		WillReturnError(errors.New("dead"))
	_, err := queryLegacyMemories(context.Background(), db, "ws-1")
	if err == nil {
		t.Error("expected error")
	}
}

// --- verifyParity (the workhorse) ---

func TestVerifyParity_AllMatch(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	mock.ExpectQuery("SELECT id::text FROM workspaces").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ws-1"))
	mock.ExpectQuery("SELECT content FROM agent_memories").
		WithArgs("ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"content"}).
			AddRow("fact A").
			AddRow("fact B"))

	plugin := &stubVerifyPlugin{
		searchFn: func(_ context.Context, _ contract.SearchRequest) (*contract.SearchResponse, error) {
			return &contract.SearchResponse{Memories: []contract.Memory{
				{ID: "id-A", Content: "fact A"},
				{ID: "id-B", Content: "fact B"},
			}}, nil
		},
	}
	resolver := &stubVerifyResolver{
		namespaces: []ResolvedNamespace{{Name: "workspace:ws-1"}},
	}
	cfg := verifyConfig{DB: db, Plugin: plugin, Resolver: resolver, SampleSize: 50}
	devnull, _ := os.Open(os.DevNull)
	defer devnull.Close()
	report, err := verifyParity(context.Background(), cfg, devnull)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if report.Matches != 1 || report.Mismatches != 0 || report.Errors != 0 {
		t.Errorf("report = %+v, want 1 match", report)
	}
}

func TestVerifyParity_MismatchDetectsMissingFromPlugin(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	mock.ExpectQuery("SELECT id::text FROM workspaces").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ws-1"))
	mock.ExpectQuery("SELECT content FROM agent_memories").
		WillReturnRows(sqlmock.NewRows([]string{"content"}).
			AddRow("fact A").
			AddRow("fact-missing-from-plugin"))

	plugin := &stubVerifyPlugin{
		searchFn: func(_ context.Context, _ contract.SearchRequest) (*contract.SearchResponse, error) {
			return &contract.SearchResponse{Memories: []contract.Memory{
				{ID: "id-A", Content: "fact A"},
			}}, nil
		},
	}
	resolver := &stubVerifyResolver{
		namespaces: []ResolvedNamespace{{Name: "workspace:ws-1"}},
	}
	cfg := verifyConfig{DB: db, Plugin: plugin, Resolver: resolver, SampleSize: 50}
	devnull, _ := os.Open(os.DevNull)
	defer devnull.Close()
	report, err := verifyParity(context.Background(), cfg, devnull)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if report.Mismatches != 1 {
		t.Errorf("report = %+v, want 1 mismatch", report)
	}
}

func TestVerifyParity_PluginExtraRowsTolerated(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	mock.ExpectQuery("SELECT id::text FROM workspaces").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ws-1"))
	mock.ExpectQuery("SELECT content FROM agent_memories").
		WillReturnRows(sqlmock.NewRows([]string{"content"}).
			AddRow("fact A"))

	// Plugin returns more rows (e.g., team-shared from a sibling).
	// Verify treats this as a match — legacy is a subset of plugin.
	plugin := &stubVerifyPlugin{
		searchFn: func(_ context.Context, _ contract.SearchRequest) (*contract.SearchResponse, error) {
			return &contract.SearchResponse{Memories: []contract.Memory{
				{ID: "id-A", Content: "fact A"},
				{ID: "id-team-1", Content: "team-shared content from sibling"},
			}}, nil
		},
	}
	resolver := &stubVerifyResolver{
		namespaces: []ResolvedNamespace{{Name: "workspace:ws-1"}, {Name: "team:root"}},
	}
	cfg := verifyConfig{DB: db, Plugin: plugin, Resolver: resolver, SampleSize: 50}
	devnull, _ := os.Open(os.DevNull)
	defer devnull.Close()
	report, err := verifyParity(context.Background(), cfg, devnull)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if report.Matches != 1 || report.Mismatches != 0 {
		t.Errorf("report = %+v, want 1 match (plugin-extra is OK)", report)
	}
}

func TestVerifyParity_LegacyQueryError(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	mock.ExpectQuery("SELECT id::text FROM workspaces").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ws-1"))
	mock.ExpectQuery("SELECT content FROM agent_memories").
		WillReturnError(errors.New("dead"))

	cfg := verifyConfig{
		DB:       db,
		Plugin:   &stubVerifyPlugin{},
		Resolver: &stubVerifyResolver{namespaces: []ResolvedNamespace{{Name: "workspace:ws-1"}}},
	}
	devnull, _ := os.Open(os.DevNull)
	defer devnull.Close()
	report, err := verifyParity(context.Background(), cfg, devnull)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if report.Errors != 1 {
		t.Errorf("report = %+v, want 1 error", report)
	}
}

func TestVerifyParity_ResolverError(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	mock.ExpectQuery("SELECT id::text FROM workspaces").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ws-1"))
	mock.ExpectQuery("SELECT content FROM agent_memories").
		WillReturnRows(sqlmock.NewRows([]string{"content"}).AddRow("x"))

	cfg := verifyConfig{
		DB:       db,
		Plugin:   &stubVerifyPlugin{},
		Resolver: &stubVerifyResolver{err: errors.New("dead")},
	}
	devnull, _ := os.Open(os.DevNull)
	defer devnull.Close()
	report, _ := verifyParity(context.Background(), cfg, devnull)
	if report.Errors != 1 {
		t.Errorf("report = %+v, want 1 error", report)
	}
}

func TestVerifyParity_PluginSearchError(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	mock.ExpectQuery("SELECT id::text FROM workspaces").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ws-1"))
	mock.ExpectQuery("SELECT content FROM agent_memories").
		WillReturnRows(sqlmock.NewRows([]string{"content"}).AddRow("x"))

	cfg := verifyConfig{
		DB: db,
		Plugin: &stubVerifyPlugin{
			searchFn: func(_ context.Context, _ contract.SearchRequest) (*contract.SearchResponse, error) {
				return nil, errors.New("plugin dead")
			},
		},
		Resolver: &stubVerifyResolver{namespaces: []ResolvedNamespace{{Name: "workspace:ws-1"}}},
	}
	devnull, _ := os.Open(os.DevNull)
	defer devnull.Close()
	report, _ := verifyParity(context.Background(), cfg, devnull)
	if report.Errors != 1 {
		t.Errorf("report = %+v, want 1 error", report)
	}
}

func TestVerifyParity_NoReadableNamespacesEmptyLegacy(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	mock.ExpectQuery("SELECT id::text FROM workspaces").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ws-1"))
	mock.ExpectQuery("SELECT content FROM agent_memories").
		WillReturnRows(sqlmock.NewRows([]string{"content"})) // empty

	cfg := verifyConfig{
		DB:       db,
		Plugin:   &stubVerifyPlugin{},
		Resolver: &stubVerifyResolver{namespaces: []ResolvedNamespace{}}, // empty
	}
	devnull, _ := os.Open(os.DevNull)
	defer devnull.Close()
	report, _ := verifyParity(context.Background(), cfg, devnull)
	// Empty legacy + empty namespaces → match.
	if report.Matches != 1 {
		t.Errorf("report = %+v, want 1 match (both empty)", report)
	}
}

func TestVerifyParity_NoReadableNamespacesNonEmptyLegacy(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	mock.ExpectQuery("SELECT id::text FROM workspaces").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ws-1"))
	mock.ExpectQuery("SELECT content FROM agent_memories").
		WillReturnRows(sqlmock.NewRows([]string{"content"}).AddRow("orphan-fact"))

	cfg := verifyConfig{
		DB:       db,
		Plugin:   &stubVerifyPlugin{},
		Resolver: &stubVerifyResolver{namespaces: []ResolvedNamespace{}},
	}
	devnull, _ := os.Open(os.DevNull)
	defer devnull.Close()
	report, _ := verifyParity(context.Background(), cfg, devnull)
	// Legacy has rows but plugin can't see any → mismatch.
	if report.Mismatches != 1 {
		t.Errorf("report = %+v, want 1 mismatch", report)
	}
}

func TestVerifyParity_PickSampleError(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	mock.ExpectQuery("SELECT id::text FROM workspaces").
		WillReturnError(errors.New("dead"))
	cfg := verifyConfig{DB: db, Plugin: &stubVerifyPlugin{}, Resolver: &stubVerifyResolver{}}
	devnull, _ := os.Open(os.DevNull)
	defer devnull.Close()
	_, err := verifyParity(context.Background(), cfg, devnull)
	if err == nil || !strings.Contains(err.Error(), "pick sample") {
		t.Errorf("err = %v", err)
	}
}

// Truncate moved to internal/textutil — coverage in
// internal/textutil/truncate_test.go (TestTruncateBytes_RuneBoundary).

// --- CLI: -verify mode ---

func TestRun_VerifyVsApplyMutuallyExclusive(t *testing.T) {
	stderr, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	defer stderr.Close()
	stdout, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	defer stdout.Close()
	err := run([]string{"-verify", "-apply"}, stdout, stderr)
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Errorf("err = %v", err)
	}
}

func TestRun_VerifyAloneIsValid(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("MEMORY_PLUGIN_URL", "http://x")
	stderr, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	defer stderr.Close()
	stdout, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	defer stdout.Close()
	err := run([]string{"-verify"}, stdout, stderr)
	// Will fail later on missing DATABASE_URL, NOT on the
	// mutually-exclusive-modes check. Asserts that -verify is
	// recognized as a valid mode.
	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Errorf("err = %v, want DATABASE_URL error (-verify alone is a valid mode)", err)
	}
}
