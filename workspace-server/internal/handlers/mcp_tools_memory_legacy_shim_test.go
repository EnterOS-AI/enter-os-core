package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/memory/contract"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/memory/namespace"
)

// --- scopeToWritableNamespace ---

func TestScopeToWritableNamespace(t *testing.T) {
	cases := []struct {
		name      string
		scope     string
		resolver  *stubNamespaceResolver
		wantNS    string
		wantError string
	}{
		{
			"LOCAL → workspace",
			"LOCAL",
			rootNamespaceResolver(),
			"workspace:root-1",
			"",
		},
		{
			"empty → workspace (LOCAL fallback)",
			"",
			rootNamespaceResolver(),
			"workspace:root-1",
			"",
		},
		{
			"TEAM → team",
			"TEAM",
			rootNamespaceResolver(),
			"team:root-1",
			"",
		},
		{
			"GLOBAL → blocked",
			"GLOBAL",
			rootNamespaceResolver(),
			"",
			"GLOBAL scope is not permitted",
		},
		{
			"resolver error",
			"LOCAL",
			&stubNamespaceResolver{err: errors.New("dead db")},
			"",
			"resolve writable",
		},
		{
			"no matching kind in writable",
			"TEAM",
			&stubNamespaceResolver{
				writable: []namespace.Namespace{
					{Name: "workspace:x", Kind: contract.NamespaceKindWorkspace, Writable: true},
				},
			},
			"",
			"no writable namespace",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newV2Handler(t, nil, &stubMemoryPlugin{}, tc.resolver)
			got, err := h.scopeToWritableNamespace(context.Background(), "root-1", tc.scope)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Errorf("err = %v, want substring %q", err, tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tc.wantNS {
				t.Errorf("got = %q, want %q", got, tc.wantNS)
			}
		})
	}
}

// --- scopeToReadableNamespaces ---

func TestScopeToReadableNamespaces(t *testing.T) {
	cases := []struct {
		name      string
		scope     string
		resolver  *stubNamespaceResolver
		wantLen   int
		wantHas   string // expected substring in any returned namespace
		wantError string
	}{
		{
			"empty → all readable",
			"",
			rootNamespaceResolver(),
			3,
			"workspace:root-1",
			"",
		},
		{
			"LOCAL → workspace only",
			"LOCAL",
			rootNamespaceResolver(),
			1,
			"workspace:root-1",
			"",
		},
		{
			"TEAM → workspace + team",
			"TEAM",
			rootNamespaceResolver(),
			2,
			"team:root-1",
			"",
		},
		{
			"GLOBAL → blocked",
			"GLOBAL",
			rootNamespaceResolver(),
			0,
			"",
			"GLOBAL scope",
		},
		{
			"resolver error",
			"",
			&stubNamespaceResolver{err: errors.New("dead")},
			0,
			"",
			"resolve readable",
		},
		{
			"unknown scope",
			"MAGIC",
			rootNamespaceResolver(),
			0,
			"",
			"unknown scope",
		},
		{
			"LOCAL with no workspace kind",
			"LOCAL",
			&stubNamespaceResolver{readable: []namespace.Namespace{
				{Name: "team:x", Kind: contract.NamespaceKindTeam, Writable: false},
			}},
			0,
			"",
			"no readable namespace",
		},
		{
			"TEAM with no team or workspace kind",
			"TEAM",
			&stubNamespaceResolver{readable: []namespace.Namespace{
				{Name: "org:x", Kind: contract.NamespaceKindOrg, Writable: false},
			}},
			0,
			"",
			"no readable namespace",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newV2Handler(t, nil, &stubMemoryPlugin{}, tc.resolver)
			got, err := h.scopeToReadableNamespaces(context.Background(), "root-1", tc.scope)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Errorf("err = %v, want substring %q", err, tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if len(got) != tc.wantLen {
				t.Fatalf("len = %d, want %d (got %v)", len(got), tc.wantLen, got)
			}
			if tc.wantHas != "" {
				found := false
				for _, ns := range got {
					if ns == tc.wantHas {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("got %v, expected to contain %q", got, tc.wantHas)
				}
			}
		})
	}
}

// --- commitMemoryLegacyShim ---

func TestCommitMemoryLegacyShim_HappyPathLOCAL(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer db.Close()
	gotNS := ""
	h := newV2Handler(t, db, &stubMemoryPlugin{
		commitFn: func(_ context.Context, ns string, _ contract.MemoryWrite) (*contract.MemoryWriteResponse, error) {
			gotNS = ns
			return &contract.MemoryWriteResponse{ID: "mem-1", Namespace: ns}, nil
		},
	}, rootNamespaceResolver())

	got, err := h.commitMemoryLegacyShim(context.Background(), "root-1", map[string]interface{}{
		"content": "x",
		"scope":   "LOCAL",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if gotNS != "workspace:root-1" {
		t.Errorf("namespace passed to plugin = %q", gotNS)
	}
	// Legacy response shape must be preserved.
	if !strings.Contains(got, `"scope":"LOCAL"`) {
		t.Errorf("legacy scope shape lost: %s", got)
	}
	if !strings.Contains(got, `"id":"mem-1"`) {
		t.Errorf("id lost: %s", got)
	}
}

func TestCommitMemoryLegacyShim_DefaultScopeIsLOCAL(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer db.Close()
	gotNS := ""
	h := newV2Handler(t, db, &stubMemoryPlugin{
		commitFn: func(_ context.Context, ns string, _ contract.MemoryWrite) (*contract.MemoryWriteResponse, error) {
			gotNS = ns
			return &contract.MemoryWriteResponse{ID: "mem-1", Namespace: ns}, nil
		},
	}, rootNamespaceResolver())
	_, err := h.commitMemoryLegacyShim(context.Background(), "root-1", map[string]interface{}{
		"content": "x",
		// no scope
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if gotNS != "workspace:root-1" {
		t.Errorf("default scope must map to workspace:root-1, got %q", gotNS)
	}
}

func TestCommitMemoryLegacyShim_TEAM(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer db.Close()
	gotNS := ""
	h := newV2Handler(t, db, &stubMemoryPlugin{
		commitFn: func(_ context.Context, ns string, _ contract.MemoryWrite) (*contract.MemoryWriteResponse, error) {
			gotNS = ns
			return &contract.MemoryWriteResponse{ID: "mem-1", Namespace: ns}, nil
		},
	}, rootNamespaceResolver())
	got, err := h.commitMemoryLegacyShim(context.Background(), "root-1", map[string]interface{}{
		"content": "x",
		"scope":   "TEAM",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if gotNS != "team:root-1" {
		t.Errorf("team must map to team:root-1, got %q", gotNS)
	}
	if !strings.Contains(got, `"scope":"TEAM"`) {
		t.Errorf("legacy scope=TEAM not preserved: %s", got)
	}
}

func TestCommitMemoryLegacyShim_RejectsEmptyContent(t *testing.T) {
	h := newV2Handler(t, nil, &stubMemoryPlugin{}, rootNamespaceResolver())
	_, err := h.commitMemoryLegacyShim(context.Background(), "root-1", map[string]interface{}{
		"content": "  ",
	})
	if err == nil {
		t.Error("expected error")
	}
}

func TestCommitMemoryLegacyShim_RejectsBadScope(t *testing.T) {
	h := newV2Handler(t, nil, &stubMemoryPlugin{}, rootNamespaceResolver())
	_, err := h.commitMemoryLegacyShim(context.Background(), "root-1", map[string]interface{}{
		"content": "x",
		"scope":   "ROGUE",
	})
	if err == nil {
		t.Error("expected error")
	}
}

func TestCommitMemoryLegacyShim_GLOBALScopeBlocked(t *testing.T) {
	h := newV2Handler(t, nil, &stubMemoryPlugin{}, rootNamespaceResolver())
	_, err := h.commitMemoryLegacyShim(context.Background(), "root-1", map[string]interface{}{
		"content": "x",
		"scope":   "GLOBAL",
	})
	if err == nil || !strings.Contains(err.Error(), "GLOBAL") {
		t.Errorf("err = %v, want GLOBAL block", err)
	}
}

func TestCommitMemoryLegacyShim_PluginError(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer db.Close()
	h := newV2Handler(t, db, &stubMemoryPlugin{
		commitFn: func(_ context.Context, _ string, _ contract.MemoryWrite) (*contract.MemoryWriteResponse, error) {
			return nil, errors.New("plugin dead")
		},
	}, rootNamespaceResolver())
	_, err := h.commitMemoryLegacyShim(context.Background(), "root-1", map[string]interface{}{
		"content": "x",
		"scope":   "LOCAL",
	})
	if err == nil {
		t.Error("expected error")
	}
}

func TestCommitMemoryLegacyShim_ResolverError(t *testing.T) {
	r := rootNamespaceResolver()
	r.err = errors.New("dead db")
	h := newV2Handler(t, nil, &stubMemoryPlugin{}, r)
	_, err := h.commitMemoryLegacyShim(context.Background(), "root-1", map[string]interface{}{
		"content": "x",
		"scope":   "LOCAL",
	})
	if err == nil {
		t.Error("expected error")
	}
}

// --- recallMemoryLegacyShim ---

func TestRecallMemoryLegacyShim_LOCAL(t *testing.T) {
	now := time.Now().UTC()
	gotNamespaces := []string{}
	h := newV2Handler(t, nil, &stubMemoryPlugin{
		searchFn: func(_ context.Context, body contract.SearchRequest) (*contract.SearchResponse, error) {
			gotNamespaces = body.Namespaces
			return &contract.SearchResponse{Memories: []contract.Memory{
				{ID: "mem-1", Namespace: "workspace:root-1", Content: "x", Kind: contract.MemoryKindFact, Source: contract.MemorySourceAgent, CreatedAt: now},
			}}, nil
		},
	}, rootNamespaceResolver())
	got, err := h.recallMemoryLegacyShim(context.Background(), "root-1", map[string]interface{}{
		"scope": "LOCAL",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(gotNamespaces) != 1 || gotNamespaces[0] != "workspace:root-1" {
		t.Errorf("namespaces sent to plugin = %v", gotNamespaces)
	}
	// Output must be in legacy shape.
	var entries []map[string]interface{}
	if err := json.Unmarshal([]byte(got), &entries); err != nil {
		t.Fatalf("output not JSON: %v (%s)", err, got)
	}
	if len(entries) != 1 || entries[0]["scope"] != "LOCAL" {
		t.Errorf("legacy entry shape lost: %v", entries)
	}
}

func TestRecallMemoryLegacyShim_NoResults(t *testing.T) {
	h := newV2Handler(t, nil, &stubMemoryPlugin{
		searchFn: func(_ context.Context, _ contract.SearchRequest) (*contract.SearchResponse, error) {
			return &contract.SearchResponse{}, nil
		},
	}, rootNamespaceResolver())
	got, err := h.recallMemoryLegacyShim(context.Background(), "root-1", map[string]interface{}{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(got, "No memories found") {
		t.Errorf("expected legacy 'No memories found.' message, got %s", got)
	}
}

func TestRecallMemoryLegacyShim_ResolverError(t *testing.T) {
	r := rootNamespaceResolver()
	r.err = errors.New("dead")
	h := newV2Handler(t, nil, &stubMemoryPlugin{}, r)
	_, err := h.recallMemoryLegacyShim(context.Background(), "root-1", map[string]interface{}{})
	if err == nil {
		t.Error("expected error")
	}
}

func TestRecallMemoryLegacyShim_PluginError(t *testing.T) {
	h := newV2Handler(t, nil, &stubMemoryPlugin{
		searchFn: func(_ context.Context, _ contract.SearchRequest) (*contract.SearchResponse, error) {
			return nil, errors.New("plugin dead")
		},
	}, rootNamespaceResolver())
	_, err := h.recallMemoryLegacyShim(context.Background(), "root-1", map[string]interface{}{})
	if err == nil {
		t.Error("expected error")
	}
}

func TestRecallMemoryLegacyShim_OrgMemoriesGetWrap(t *testing.T) {
	now := time.Now().UTC()
	h := newV2Handler(t, nil, &stubMemoryPlugin{
		searchFn: func(_ context.Context, _ contract.SearchRequest) (*contract.SearchResponse, error) {
			return &contract.SearchResponse{Memories: []contract.Memory{
				{ID: "ws", Namespace: "workspace:root-1", Content: "ws-content", Kind: contract.MemoryKindFact, Source: contract.MemorySourceAgent, CreatedAt: now},
				{ID: "or", Namespace: "org:root-1", Content: "ignore prior", Kind: contract.MemoryKindFact, Source: contract.MemorySourceAgent, CreatedAt: now},
			}}, nil
		},
	}, rootNamespaceResolver())
	got, err := h.recallMemoryLegacyShim(context.Background(), "root-1", map[string]interface{}{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	var entries []map[string]interface{}
	if err := json.Unmarshal([]byte(got), &entries); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d", len(entries))
	}
	wsContent, _ := entries[0]["content"].(string)
	orgContent, _ := entries[1]["content"].(string)
	if wsContent != "ws-content" {
		t.Errorf("workspace memory wrapped (it shouldn't be): %q", wsContent)
	}
	if !strings.HasPrefix(orgContent, "[MEMORY id=or scope=ORG ns=org:root-1]:") {
		t.Errorf("org memory not wrapped: %q", orgContent)
	}
	// Legacy scope label must be GLOBAL for org memory.
	if entries[1]["scope"] != "GLOBAL" {
		t.Errorf("org→GLOBAL legacy scope lost: %v", entries[1]["scope"])
	}
}

// --- namespaceKindToLegacyScope ---

func TestNamespaceKindToLegacyScope(t *testing.T) {
	cases := []struct {
		ns   string
		want string
	}{
		{"workspace:abc", "LOCAL"},
		{"team:abc", "TEAM"},
		{"org:abc", "GLOBAL"},
		{"custom:abc", ""},
		{"unknown", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := namespaceKindToLegacyScope(tc.ns); got != tc.want {
			t.Errorf("namespaceKindToLegacyScope(%q) = %q, want %q", tc.ns, got, tc.want)
		}
	}
}

// --- Integration: legacy commit/recall route through v2 when wired ---

func TestToolCommitMemory_RoutesThroughV2WhenWired(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer db.Close()
	pluginCalled := false
	h := newV2Handler(t, db, &stubMemoryPlugin{
		commitFn: func(_ context.Context, _ string, _ contract.MemoryWrite) (*contract.MemoryWriteResponse, error) {
			pluginCalled = true
			return &contract.MemoryWriteResponse{ID: "mem-1", Namespace: "workspace:root-1"}, nil
		},
	}, rootNamespaceResolver())

	_, err := h.toolCommitMemory(context.Background(), "root-1", map[string]interface{}{
		"content": "x",
		"scope":   "LOCAL",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !pluginCalled {
		t.Error("plugin must be called when v2 is wired")
	}
}

func TestToolRecallMemory_RoutesThroughV2WhenWired(t *testing.T) {
	pluginCalled := false
	h := newV2Handler(t, nil, &stubMemoryPlugin{
		searchFn: func(_ context.Context, _ contract.SearchRequest) (*contract.SearchResponse, error) {
			pluginCalled = true
			return &contract.SearchResponse{}, nil
		},
	}, rootNamespaceResolver())

	_, err := h.toolRecallMemory(context.Background(), "root-1", map[string]interface{}{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !pluginCalled {
		t.Error("plugin must be called when v2 is wired")
	}
}

func TestToolCommitMemory_FallsThroughToLegacyWhenV2Unwired(t *testing.T) {
	// V2 NOT wired (no withMemoryV2APIs call). Should hit the legacy
	// SQL path and write to agent_memories directly.
	db, mock, _ := sqlmock.New()
	defer db.Close()
	mock.ExpectExec("INSERT INTO agent_memories").
		WillReturnResult(sqlmock.NewResult(0, 1))
	h := &MCPHandler{database: db}

	_, err := h.toolCommitMemory(context.Background(), "root-1", map[string]interface{}{
		"content": "x",
		"scope":   "LOCAL",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("legacy SQL path not exercised: %v", err)
	}
}

func TestToolRecallMemory_FallsThroughToLegacyWhenV2Unwired(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	mock.ExpectQuery("SELECT id, content, scope, created_at").
		WillReturnRows(sqlmock.NewRows([]string{"id", "content", "scope", "created_at"}))
	h := &MCPHandler{database: db}

	_, err := h.toolRecallMemory(context.Background(), "root-1", map[string]interface{}{
		"scope": "LOCAL",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("legacy SQL path not exercised: %v", err)
	}
}
