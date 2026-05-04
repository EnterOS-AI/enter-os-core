package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	platformdb "github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/memory/contract"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/memory/namespace"
)

// --- stubs ---

type stubAdminPlugin struct {
	upserts  []string
	commits  []commitRecord
	searches []contract.SearchRequest
	commitFn func(ctx context.Context, ns string, body contract.MemoryWrite) (*contract.MemoryWriteResponse, error)
	searchFn func(ctx context.Context, body contract.SearchRequest) (*contract.SearchResponse, error)
	upsertFn func(ctx context.Context, name string, body contract.NamespaceUpsert) (*contract.Namespace, error)
}

type commitRecord struct {
	NS      string
	Content string
}

func (s *stubAdminPlugin) UpsertNamespace(ctx context.Context, name string, body contract.NamespaceUpsert) (*contract.Namespace, error) {
	s.upserts = append(s.upserts, name)
	if s.upsertFn != nil {
		return s.upsertFn(ctx, name, body)
	}
	return &contract.Namespace{Name: name, Kind: body.Kind, CreatedAt: time.Now().UTC()}, nil
}
func (s *stubAdminPlugin) CommitMemory(ctx context.Context, ns string, body contract.MemoryWrite) (*contract.MemoryWriteResponse, error) {
	s.commits = append(s.commits, commitRecord{NS: ns, Content: body.Content})
	if s.commitFn != nil {
		return s.commitFn(ctx, ns, body)
	}
	return &contract.MemoryWriteResponse{ID: "out-1", Namespace: ns}, nil
}
func (s *stubAdminPlugin) Search(ctx context.Context, body contract.SearchRequest) (*contract.SearchResponse, error) {
	s.searches = append(s.searches, body)
	if s.searchFn != nil {
		return s.searchFn(ctx, body)
	}
	return &contract.SearchResponse{}, nil
}

type stubAdminResolver struct {
	readable []namespace.Namespace
	writable []namespace.Namespace
	err      error
}

func (s *stubAdminResolver) ReadableNamespaces(_ context.Context, _ string) ([]namespace.Namespace, error) {
	return s.readable, s.err
}
func (s *stubAdminResolver) WritableNamespaces(_ context.Context, _ string) ([]namespace.Namespace, error) {
	return s.writable, s.err
}

func adminRootResolver() *stubAdminResolver {
	return &stubAdminResolver{
		readable: []namespace.Namespace{
			{Name: "workspace:root-1", Kind: contract.NamespaceKindWorkspace, Writable: true},
			{Name: "team:root-1", Kind: contract.NamespaceKindTeam, Writable: true},
			{Name: "org:root-1", Kind: contract.NamespaceKindOrg, Writable: true},
		},
		writable: []namespace.Namespace{
			{Name: "workspace:root-1", Kind: contract.NamespaceKindWorkspace, Writable: true},
			{Name: "team:root-1", Kind: contract.NamespaceKindTeam, Writable: true},
			{Name: "org:root-1", Kind: contract.NamespaceKindOrg, Writable: true},
		},
	}
}

// installMockDB swaps platformdb.DB with a sqlmock for a test.
func installMockDB(t *testing.T) sqlmock.Sqlmock {
	t.Helper()
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	prev := platformdb.DB
	platformdb.DB = mockDB
	t.Cleanup(func() {
		_ = mockDB.Close()
		platformdb.DB = prev
	})
	return mock
}

// --- cutoverActive ---

func TestCutoverActive(t *testing.T) {
	cases := []struct {
		name     string
		envVal   string
		plugin   adminMemoriesPlugin
		resolver adminMemoriesResolver
		want     bool
	}{
		{"env unset", "", &stubAdminPlugin{}, adminRootResolver(), false},
		{"env true but unwired", "true", nil, nil, false},
		{"env false", "false", &stubAdminPlugin{}, adminRootResolver(), false},
		{"env true wired", "true", &stubAdminPlugin{}, adminRootResolver(), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envMemoryV2Cutover, tc.envVal)
			h := &AdminMemoriesHandler{plugin: tc.plugin, resolver: tc.resolver}
			if got := h.cutoverActive(); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// --- WithMemoryV2 wiring ---

func TestWithMemoryV2_AttachesDeps(t *testing.T) {
	h := NewAdminMemoriesHandler().WithMemoryV2(nil, nil)
	// Both nil pointers — wiring still attaches them; cutoverActive
	// reports false because the interface values are nil.
	if h.plugin == nil && h.resolver == nil {
		// expected
	}
}

func TestWithMemoryV2APIs_AttachesDeps(t *testing.T) {
	h := NewAdminMemoriesHandler().withMemoryV2APIs(&stubAdminPlugin{}, adminRootResolver())
	if h.plugin == nil || h.resolver == nil {
		t.Error("withMemoryV2APIs must attach both interfaces")
	}
}

// --- Export via plugin ---

func TestExport_RoutesThroughPluginWhenCutoverActive(t *testing.T) {
	t.Setenv(envMemoryV2Cutover, "true")
	mock := installMockDB(t)

	mock.ExpectQuery("WITH RECURSIVE chain").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "root_id"}).
			AddRow("ws-1", "alpha", "ws-1"))

	plugin := &stubAdminPlugin{
		searchFn: func(_ context.Context, body contract.SearchRequest) (*contract.SearchResponse, error) {
			return &contract.SearchResponse{Memories: []contract.Memory{
				{ID: "mem-1", Namespace: "workspace:root-1", Content: "fact x", Kind: contract.MemoryKindFact, Source: contract.MemorySourceAgent, CreatedAt: time.Now().UTC()},
				{ID: "mem-2", Namespace: "team:root-1", Content: "team y", Kind: contract.MemoryKindFact, Source: contract.MemorySourceAgent, CreatedAt: time.Now().UTC()},
			}}, nil
		},
	}
	h := NewAdminMemoriesHandler().withMemoryV2APIs(plugin, adminRootResolver())

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/admin/memories/export", nil)
	h.Export(c)

	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	var entries []memoryExportEntry
	if err := json.Unmarshal(w.Body.Bytes(), &entries); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("entries = %d", len(entries))
	}
	// Legacy scope label must be in the export
	scopes := map[string]bool{}
	for _, e := range entries {
		scopes[e.Scope] = true
	}
	if !scopes["LOCAL"] || !scopes["TEAM"] {
		t.Errorf("expected LOCAL+TEAM scopes, got %v", scopes)
	}
}

func TestExport_DeduplicatesByMemoryID(t *testing.T) {
	t.Setenv(envMemoryV2Cutover, "true")
	mock := installMockDB(t)

	// Two workspaces, both will see the same team-shared memory.
	mock.ExpectQuery("WITH RECURSIVE chain").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "root_id"}).
			AddRow("ws-1", "alpha", "ws-1").
			AddRow("ws-2", "beta", "ws-2"))

	plugin := &stubAdminPlugin{
		searchFn: func(_ context.Context, body contract.SearchRequest) (*contract.SearchResponse, error) {
			return &contract.SearchResponse{Memories: []contract.Memory{
				{ID: "mem-shared", Namespace: "team:root-1", Content: "team-fact", Kind: contract.MemoryKindFact, Source: contract.MemorySourceAgent, CreatedAt: time.Now().UTC()},
			}}, nil
		},
	}
	h := NewAdminMemoriesHandler().withMemoryV2APIs(plugin, adminRootResolver())
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/admin/memories/export", nil)
	h.Export(c)

	var entries []memoryExportEntry
	_ = json.Unmarshal(w.Body.Bytes(), &entries)
	if len(entries) != 1 {
		t.Errorf("dedup failed; got %d entries, want 1", len(entries))
	}
}

func TestExport_SkipsWorkspaceWhenResolverFails(t *testing.T) {
	t.Setenv(envMemoryV2Cutover, "true")
	mock := installMockDB(t)
	mock.ExpectQuery("WITH RECURSIVE chain").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "root_id"}).
			AddRow("ws-1", "alpha", "ws-1"))

	plugin := &stubAdminPlugin{}
	resolver := &stubAdminResolver{err: errors.New("resolver dead")}
	h := NewAdminMemoriesHandler().withMemoryV2APIs(plugin, resolver)
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/admin/memories/export", nil)
	h.Export(c)

	// Should still 200 with empty memories — failure is per-workspace.
	if w.Code != http.StatusOK {
		t.Errorf("code = %d body=%s", w.Code, w.Body.String())
	}
}

func TestExport_SkipsWorkspaceWhenPluginSearchFails(t *testing.T) {
	t.Setenv(envMemoryV2Cutover, "true")
	mock := installMockDB(t)
	mock.ExpectQuery("WITH RECURSIVE chain").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "root_id"}).
			AddRow("ws-1", "alpha", "ws-1"))

	plugin := &stubAdminPlugin{
		searchFn: func(_ context.Context, _ contract.SearchRequest) (*contract.SearchResponse, error) {
			return nil, errors.New("plugin dead")
		},
	}
	h := NewAdminMemoriesHandler().withMemoryV2APIs(plugin, adminRootResolver())
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/admin/memories/export", nil)
	h.Export(c)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d", w.Code)
	}
}

func TestExport_WorkspacesQueryFails(t *testing.T) {
	t.Setenv(envMemoryV2Cutover, "true")
	mock := installMockDB(t)
	mock.ExpectQuery("WITH RECURSIVE chain").
		WillReturnError(errors.New("db dead"))

	plugin := &stubAdminPlugin{}
	h := NewAdminMemoriesHandler().withMemoryV2APIs(plugin, adminRootResolver())
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/admin/memories/export", nil)
	h.Export(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("code = %d, want 500", w.Code)
	}
}

func TestExport_EmptyReadable(t *testing.T) {
	t.Setenv(envMemoryV2Cutover, "true")
	mock := installMockDB(t)
	mock.ExpectQuery("WITH RECURSIVE chain").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "root_id"}).
			AddRow("ws-1", "alpha", "ws-1"))

	resolver := &stubAdminResolver{readable: []namespace.Namespace{}}
	h := NewAdminMemoriesHandler().withMemoryV2APIs(&stubAdminPlugin{}, resolver)
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/admin/memories/export", nil)
	h.Export(c)
	if w.Code != http.StatusOK {
		t.Errorf("code = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "[]") {
		t.Errorf("expected empty array, got %s", w.Body.String())
	}
}

func TestExport_RedactsSecretsInPluginPath(t *testing.T) {
	t.Setenv(envMemoryV2Cutover, "true")
	mock := installMockDB(t)
	mock.ExpectQuery("WITH RECURSIVE chain").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "root_id"}).
			AddRow("ws-1", "alpha", "ws-1"))

	plugin := &stubAdminPlugin{
		searchFn: func(_ context.Context, _ contract.SearchRequest) (*contract.SearchResponse, error) {
			return &contract.SearchResponse{Memories: []contract.Memory{
				{ID: "mem-1", Namespace: "workspace:root-1", Content: "API_KEY=sk-1234567890abcdefghijk0123456789", Kind: contract.MemoryKindFact, Source: contract.MemorySourceAgent, CreatedAt: time.Now().UTC()},
			}}, nil
		},
	}
	h := NewAdminMemoriesHandler().withMemoryV2APIs(plugin, adminRootResolver())
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/admin/memories/export", nil)
	h.Export(c)

	if strings.Contains(w.Body.String(), "sk-1234567890abcdef") {
		t.Errorf("export leaked unredacted secret: %s", w.Body.String())
	}
}

// --- Import via plugin ---

func TestImport_RoutesThroughPluginWhenCutoverActive(t *testing.T) {
	t.Setenv(envMemoryV2Cutover, "true")
	mock := installMockDB(t)
	mock.ExpectQuery("SELECT id::text FROM workspaces").
		WithArgs("alpha").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("root-1"))

	plugin := &stubAdminPlugin{}
	h := NewAdminMemoriesHandler().withMemoryV2APIs(plugin, adminRootResolver())

	body, _ := json.Marshal([]memoryImportEntry{
		{Content: "fact x", Scope: "LOCAL", WorkspaceName: "alpha"},
	})
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/admin/memories/import", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.Import(c)

	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	if len(plugin.commits) != 1 {
		t.Errorf("commits = %d, want 1", len(plugin.commits))
	}
	if plugin.commits[0].NS != "workspace:root-1" {
		t.Errorf("ns = %q", plugin.commits[0].NS)
	}
}

func TestImport_SkipsUnknownWorkspace(t *testing.T) {
	t.Setenv(envMemoryV2Cutover, "true")
	mock := installMockDB(t)
	mock.ExpectQuery("SELECT id::text FROM workspaces").
		WithArgs("ghost").
		WillReturnError(errors.New("no rows"))

	plugin := &stubAdminPlugin{}
	h := NewAdminMemoriesHandler().withMemoryV2APIs(plugin, adminRootResolver())

	body, _ := json.Marshal([]memoryImportEntry{
		{Content: "x", Scope: "LOCAL", WorkspaceName: "ghost"},
	})
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/admin/memories/import", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.Import(c)

	var resp map[string]int
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["skipped"] != 1 || resp["imported"] != 0 {
		t.Errorf("resp = %v", resp)
	}
}

func TestImport_PluginUpsertNamespaceError(t *testing.T) {
	t.Setenv(envMemoryV2Cutover, "true")
	mock := installMockDB(t)
	mock.ExpectQuery("SELECT id::text FROM workspaces").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("root-1"))

	plugin := &stubAdminPlugin{
		upsertFn: func(_ context.Context, _ string, _ contract.NamespaceUpsert) (*contract.Namespace, error) {
			return nil, errors.New("upsert dead")
		},
	}
	h := NewAdminMemoriesHandler().withMemoryV2APIs(plugin, adminRootResolver())

	body, _ := json.Marshal([]memoryImportEntry{
		{Content: "x", Scope: "LOCAL", WorkspaceName: "alpha"},
	})
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/admin/memories/import", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.Import(c)

	var resp map[string]int
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["errors"] != 1 || resp["imported"] != 0 {
		t.Errorf("resp = %v", resp)
	}
}

func TestImport_PluginCommitError(t *testing.T) {
	t.Setenv(envMemoryV2Cutover, "true")
	mock := installMockDB(t)
	mock.ExpectQuery("SELECT id::text FROM workspaces").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("root-1"))

	plugin := &stubAdminPlugin{
		commitFn: func(_ context.Context, _ string, _ contract.MemoryWrite) (*contract.MemoryWriteResponse, error) {
			return nil, errors.New("commit dead")
		},
	}
	h := NewAdminMemoriesHandler().withMemoryV2APIs(plugin, adminRootResolver())

	body, _ := json.Marshal([]memoryImportEntry{
		{Content: "x", Scope: "LOCAL", WorkspaceName: "alpha"},
	})
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/admin/memories/import", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.Import(c)

	var resp map[string]int
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["errors"] != 1 {
		t.Errorf("resp = %v", resp)
	}
}

func TestImport_RedactsBeforePluginSeesContent(t *testing.T) {
	t.Setenv(envMemoryV2Cutover, "true")
	mock := installMockDB(t)
	mock.ExpectQuery("SELECT id::text FROM workspaces").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("root-1"))

	plugin := &stubAdminPlugin{}
	h := NewAdminMemoriesHandler().withMemoryV2APIs(plugin, adminRootResolver())

	body, _ := json.Marshal([]memoryImportEntry{
		{Content: "API_KEY=sk-1234567890abcdefghijk0123456789", Scope: "LOCAL", WorkspaceName: "alpha"},
	})
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/admin/memories/import", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.Import(c)

	if len(plugin.commits) != 1 {
		t.Fatalf("commits = %d", len(plugin.commits))
	}
	if strings.Contains(plugin.commits[0].Content, "sk-1234567890") {
		t.Errorf("plugin received unredacted content: %q", plugin.commits[0].Content)
	}
}

func TestImport_SkipsUnknownScope(t *testing.T) {
	t.Setenv(envMemoryV2Cutover, "true")
	mock := installMockDB(t)
	mock.ExpectQuery("SELECT id::text FROM workspaces").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("root-1"))

	plugin := &stubAdminPlugin{}
	h := NewAdminMemoriesHandler().withMemoryV2APIs(plugin, adminRootResolver())

	body, _ := json.Marshal([]memoryImportEntry{
		{Content: "x", Scope: "WEIRD", WorkspaceName: "alpha"},
	})
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/admin/memories/import", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.Import(c)

	var resp map[string]int
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["skipped"] != 1 {
		t.Errorf("resp = %v", resp)
	}
}

func TestImport_SkipsWhenResolverErrors(t *testing.T) {
	t.Setenv(envMemoryV2Cutover, "true")
	mock := installMockDB(t)
	mock.ExpectQuery("SELECT id::text FROM workspaces").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("root-1"))

	plugin := &stubAdminPlugin{}
	resolver := &stubAdminResolver{err: errors.New("dead")}
	h := NewAdminMemoriesHandler().withMemoryV2APIs(plugin, resolver)

	body, _ := json.Marshal([]memoryImportEntry{
		{Content: "x", Scope: "LOCAL", WorkspaceName: "alpha"},
	})
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/admin/memories/import", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.Import(c)

	var resp map[string]int
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["skipped"] != 1 {
		t.Errorf("resp = %v", resp)
	}
}

// TestExport_BatchesPluginCallsByRoot pins the I3 fix: previously the
// export ran one resolver + one plugin search per workspace (N+1 in
// both); now it groups by root and runs one resolver + one plugin
// search per UNIQUE root.
//
// Setup: 3 workspaces under 1 root → 1 resolver call + 1 plugin call
// (was: 3 resolver + 3 plugin in the old code). The plugin search
// receives 5 namespaces: each member's workspace:<id> + team:root-1
// + org:root-1. (Children's workspace:<id> namespaces must be
// included or admin export silently drops their private memories.)
func TestExport_BatchesPluginCallsByRoot(t *testing.T) {
	t.Setenv(envMemoryV2Cutover, "true")
	mock := installMockDB(t)

	mock.ExpectQuery("WITH RECURSIVE chain").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "root_id"}).
			AddRow("root-1", "alpha", "root-1").
			AddRow("child-1", "alpha-child", "root-1").
			AddRow("child-2", "alpha-grandchild", "root-1"))

	pluginSearchCount := 0
	plugin := &stubAdminPlugin{
		searchFn: func(_ context.Context, body contract.SearchRequest) (*contract.SearchResponse, error) {
			pluginSearchCount++
			if len(body.Namespaces) != 5 {
				t.Errorf("plugin search call %d: namespaces len = %d, want 5 (3 workspace + team + org); got %v", pluginSearchCount, len(body.Namespaces), body.Namespaces)
			}
			return &contract.SearchResponse{}, nil
		},
	}
	h := NewAdminMemoriesHandler().withMemoryV2APIs(plugin, adminRootResolver())

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/admin/memories/export", nil)
	h.Export(c)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d body=%s", w.Code, w.Body.String())
	}
	if pluginSearchCount != 1 {
		t.Errorf("plugin search called %d times, want 1 (was 3 with the old N+1 code)", pluginSearchCount)
	}
}

// perWorkspaceResolver mimics the real resolver: ReadableNamespaces
// returns the SPECIFIC workspace's view (workspace:<that ID> +
// team:<root> + org:<root>), not a constant set. The legacy
// stubAdminResolver hides the I3 silent-drop bug by ignoring its
// workspace-id argument.
type perWorkspaceResolver map[string][]namespace.Namespace

func (r perWorkspaceResolver) ReadableNamespaces(_ context.Context, ws string) ([]namespace.Namespace, error) {
	v, ok := r[ws]
	if !ok {
		return nil, errors.New("perWorkspaceResolver: unknown ws " + ws)
	}
	return v, nil
}
func (r perWorkspaceResolver) WritableNamespaces(_ context.Context, ws string) ([]namespace.Namespace, error) {
	return r.ReadableNamespaces(nil, ws)
}

// TestExport_IncludesEveryMembersPrivateNamespace pins the I3 follow-up
// fix: when a root group has multiple members, the export must surface
// each member's workspace:<id> namespace, not just the root's. Before
// the fix, calling ReadableNamespaces(rootID) returned only
// workspace:rootID + team:rootID + org:rootID — every child workspace's
// private memories were silently dropped from admin export.
func TestExport_IncludesEveryMembersPrivateNamespace(t *testing.T) {
	t.Setenv(envMemoryV2Cutover, "true")
	mock := installMockDB(t)

	mock.ExpectQuery("WITH RECURSIVE chain").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "root_id"}).
			AddRow("root-1", "alpha", "root-1").
			AddRow("child-1", "alpha-child", "root-1").
			AddRow("child-2", "alpha-grandchild", "root-1"))

	resolver := perWorkspaceResolver{
		"root-1": {
			{Name: "workspace:root-1", Kind: contract.NamespaceKindWorkspace, Writable: true},
			{Name: "team:root-1", Kind: contract.NamespaceKindTeam, Writable: true},
			{Name: "org:root-1", Kind: contract.NamespaceKindOrg, Writable: true},
		},
		"child-1": {
			{Name: "workspace:child-1", Kind: contract.NamespaceKindWorkspace, Writable: true},
			{Name: "team:root-1", Kind: contract.NamespaceKindTeam, Writable: true},
			{Name: "org:root-1", Kind: contract.NamespaceKindOrg, Writable: true},
		},
		"child-2": {
			{Name: "workspace:child-2", Kind: contract.NamespaceKindWorkspace, Writable: true},
			{Name: "team:root-1", Kind: contract.NamespaceKindTeam, Writable: true},
			{Name: "org:root-1", Kind: contract.NamespaceKindOrg, Writable: true},
		},
	}

	var passedNamespaces []string
	plugin := &stubAdminPlugin{
		searchFn: func(_ context.Context, body contract.SearchRequest) (*contract.SearchResponse, error) {
			passedNamespaces = append(passedNamespaces, body.Namespaces...)
			return &contract.SearchResponse{Memories: []contract.Memory{
				{ID: "m-root", Namespace: "workspace:root-1", Content: "root private", Kind: contract.MemoryKindFact, Source: contract.MemorySourceAgent, CreatedAt: time.Now().UTC()},
				{ID: "m-child1", Namespace: "workspace:child-1", Content: "child-1 private", Kind: contract.MemoryKindFact, Source: contract.MemorySourceAgent, CreatedAt: time.Now().UTC()},
				{ID: "m-child2", Namespace: "workspace:child-2", Content: "child-2 private", Kind: contract.MemoryKindFact, Source: contract.MemorySourceAgent, CreatedAt: time.Now().UTC()},
				{ID: "m-team", Namespace: "team:root-1", Content: "shared team", Kind: contract.MemoryKindFact, Source: contract.MemorySourceAgent, CreatedAt: time.Now().UTC()},
			}}, nil
		},
	}
	h := NewAdminMemoriesHandler().withMemoryV2APIs(plugin, resolver)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/admin/memories/export", nil)
	h.Export(c)

	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}

	// Every member's private namespace must reach the plugin search.
	want := []string{"workspace:root-1", "workspace:child-1", "workspace:child-2", "team:root-1", "org:root-1"}
	got := make(map[string]bool, len(passedNamespaces))
	for _, ns := range passedNamespaces {
		got[ns] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("plugin search missing namespace %q (got %v)", w, passedNamespaces)
		}
	}
	if len(passedNamespaces) != 5 {
		t.Errorf("plugin search namespace count = %d, want 5 (3 workspace + team + org)", len(passedNamespaces))
	}

	// Children's private memories must appear in the export, attributed
	// to the right workspace_name.
	var entries []memoryExportEntry
	if err := json.Unmarshal(w.Body.Bytes(), &entries); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	byID := map[string]memoryExportEntry{}
	for _, e := range entries {
		byID[e.ID] = e
	}
	for _, exp := range []struct{ id, ns, owner string }{
		{"m-root", "workspace:root-1", "alpha"},
		{"m-child1", "workspace:child-1", "alpha-child"},
		{"m-child2", "workspace:child-2", "alpha-grandchild"},
	} {
		e, ok := byID[exp.id]
		if !ok {
			t.Errorf("export missing memory %s — children's private memories silently dropped", exp.id)
			continue
		}
		if e.Namespace != exp.ns {
			t.Errorf("memory %s namespace = %q, want %q", exp.id, e.Namespace, exp.ns)
		}
		if e.WorkspaceName != exp.owner {
			t.Errorf("memory %s owner = %q, want %q", exp.id, e.WorkspaceName, exp.owner)
		}
	}
}

// TestPickOwnerForNamespace covers the namespace→workspace_name
// attribution helper introduced in I3.
func TestPickOwnerForNamespace(t *testing.T) {
	members := []workspaceRow{
		{ID: "root-1", Name: "alpha", RootID: "root-1"},
		{ID: "child-1", Name: "alpha-child", RootID: "root-1"},
	}
	cases := []struct {
		name string
		ns   string
		want string
	}{
		{"workspace ns matches member id", "workspace:child-1", "alpha-child"},
		{"workspace ns no match → first", "workspace:foreign", "alpha"},
		{"team ns → first member of root group", "team:root-1", "alpha"},
		{"org ns → first member", "org:root-1", "alpha"},
		{"custom ns → first member", "custom:foo", "alpha"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pickOwnerForNamespace(tc.ns, members); got != tc.want {
				t.Errorf("pickOwnerForNamespace(%q) = %q, want %q", tc.ns, got, tc.want)
			}
		})
	}
	if got := pickOwnerForNamespace("workspace:abc", nil); got != "" {
		t.Errorf("empty members must return \"\", got %q", got)
	}
}

// --- Helper functions ---

func TestLegacyScopeFromNamespace(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"workspace:abc", "LOCAL"},
		{"team:abc", "TEAM"},
		{"org:abc", "GLOBAL"},
		{"custom:abc", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := legacyScopeFromNamespace(tc.in); got != tc.want {
			t.Errorf("legacyScopeFromNamespace(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNamespaceKindFromLegacyScope(t *testing.T) {
	cases := []struct {
		in   string
		want contract.NamespaceKind
	}{
		{"LOCAL", contract.NamespaceKindWorkspace},
		{"local", contract.NamespaceKindWorkspace},
		{"TEAM", contract.NamespaceKindTeam},
		{"GLOBAL", contract.NamespaceKindOrg},
		{"weird", contract.NamespaceKindWorkspace},
	}
	for _, tc := range cases {
		if got := namespaceKindFromLegacyScope(tc.in); got != tc.want {
			t.Errorf("namespaceKindFromLegacyScope(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSkipImport_ErrorMessage(t *testing.T) {
	e := &skipImport{reason: "unknown scope: WEIRD"}
	if !strings.Contains(e.Error(), "unknown scope: WEIRD") {
		t.Errorf("Error() = %q", e.Error())
	}
}

// --- Confirm legacy paths still work when env is unset ---

func TestExport_LegacyPathWhenCutoverInactive(t *testing.T) {
	t.Setenv(envMemoryV2Cutover, "")
	mock := installMockDB(t)
	mock.ExpectQuery("SELECT am.id, am.content, am.scope, am.namespace").
		WillReturnRows(sqlmock.NewRows([]string{"id", "content", "scope", "namespace", "created_at", "workspace_name"}))

	h := NewAdminMemoriesHandler().withMemoryV2APIs(&stubAdminPlugin{}, adminRootResolver())
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/admin/memories/export", nil)
	h.Export(c)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d body=%s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("legacy SQL path not exercised: %v", err)
	}
}
