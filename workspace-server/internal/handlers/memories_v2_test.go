package handlers

// memories_v2_test.go — comprehensive coverage for the Memory v2
// canvas-facing HTTP surface. Pinned shape:
//
//   - 503 path when plugin unwired (every route)
//   - GET /v2/namespaces success + readable/writable propagation
//   - GET /v2/namespaces error path (resolver failure on either call)
//   - GET /v2/memories: empty intersection, namespace passthrough,
//     query+kind+limit propagation, plugin error mapping
//   - DELETE /v2/memories/:id: success, plugin not_found→404, other
//     plugin errors→502, missing memoryId→400
//   - View shaping: namespaceLabel for all four kinds + truncation,
//     memoryToView with/without propagation source, parseLimit edge
//     cases (default, negative, zero, over-cap, non-numeric)
//
// Tests use the same `memoryPluginAPI` / `namespaceResolverAPI` fakes
// the MCP v2 tests use so we don't spin up a real plugin server.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/memory/contract"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/memory/namespace"
	"github.com/gin-gonic/gin"
)

// ─────────────────────────────────────────────────────────────────────────────
// Fakes
// ─────────────────────────────────────────────────────────────────────────────

type fakePlugin struct {
	searchResp *contract.SearchResponse
	searchErr  error
	searchReq  contract.SearchRequest // captured for assertion
	forgetErr  error
	forgetID   string
	forgetReq  contract.ForgetRequest
}

func (f *fakePlugin) CommitMemory(ctx context.Context, ns string, body contract.MemoryWrite) (*contract.MemoryWriteResponse, error) {
	return nil, errors.New("not implemented in fake")
}
func (f *fakePlugin) Search(ctx context.Context, body contract.SearchRequest) (*contract.SearchResponse, error) {
	f.searchReq = body
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	return f.searchResp, nil
}
func (f *fakePlugin) ForgetMemory(ctx context.Context, id string, body contract.ForgetRequest) error {
	f.forgetID = id
	f.forgetReq = body
	return f.forgetErr
}

type fakeNSResolver struct {
	readable     []namespace.Namespace
	readableErr  error
	writable     []namespace.Namespace
	writableErr  error
	intersect    []string
	intersectErr error
	intersectIn  []string // captured
}

func (f *fakeNSResolver) ReadableNamespaces(ctx context.Context, ws string) ([]namespace.Namespace, error) {
	return f.readable, f.readableErr
}
func (f *fakeNSResolver) WritableNamespaces(ctx context.Context, ws string) ([]namespace.Namespace, error) {
	return f.writable, f.writableErr
}
func (f *fakeNSResolver) CanWrite(ctx context.Context, ws, ns string) (bool, error) {
	return true, nil
}
func (f *fakeNSResolver) IntersectReadable(ctx context.Context, ws string, requested []string) ([]string, error) {
	f.intersectIn = requested
	return f.intersect, f.intersectErr
}

// ─────────────────────────────────────────────────────────────────────────────
// Test helpers
// ─────────────────────────────────────────────────────────────────────────────

func init() {
	gin.SetMode(gin.TestMode)
}

// newWiredHandler returns a handler with both the fake plugin + fake
// resolver attached. Tests that need the unwired (503) path use
// NewMemoriesV2Handler() directly.
func newWiredHandler(p *fakePlugin, r *fakeNSResolver) *MemoriesV2Handler {
	return NewMemoriesV2Handler().withMemoryV2APIs(p, r)
}

func doRequest(t *testing.T, h *MemoriesV2Handler, method, path string, params gin.Params) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = params
	req := httptest.NewRequest(method, path, nil)
	c.Request = req
	switch {
	case method == http.MethodGet && strings.HasSuffix(path, "/v2/namespaces"):
		h.Namespaces(c)
	case method == http.MethodGet && strings.Contains(path, "/v2/memories"):
		h.Search(c)
	case method == http.MethodDelete:
		h.Forget(c)
	default:
		t.Fatalf("doRequest: don't know how to dispatch %s %s", method, path)
	}
	return rec
}

func mustJSON(t *testing.T, body []byte, out interface{}) {
	t.Helper()
	if err := json.Unmarshal(body, out); err != nil {
		t.Fatalf("json decode: %v\nbody=%s", err, string(body))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 503 — plugin unwired
// ─────────────────────────────────────────────────────────────────────────────

func TestMemoriesV2_PluginUnwired_All503(t *testing.T) {
	h := NewMemoriesV2Handler() // no WithMemoryV2 / withMemoryV2APIs

	cases := []struct {
		name   string
		method string
		path   string
		params gin.Params
	}{
		{"namespaces", http.MethodGet, "/workspaces/ws-a/v2/namespaces", gin.Params{{Key: "id", Value: "ws-a"}}},
		{"search", http.MethodGet, "/workspaces/ws-a/v2/memories", gin.Params{{Key: "id", Value: "ws-a"}}},
		{"forget", http.MethodDelete, "/workspaces/ws-a/v2/memories/m-1", gin.Params{{Key: "id", Value: "ws-a"}, {Key: "memoryId", Value: "m-1"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(t, h, tc.method, tc.path, tc.params)
			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("expected 503, got %d", rec.Code)
			}
			var body map[string]string
			mustJSON(t, rec.Body.Bytes(), &body)
			if !strings.Contains(body["error"], "MEMORY_PLUGIN_URL") {
				t.Errorf("503 body missing operator hint, got: %q", body["error"])
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// GET /v2/namespaces
// ─────────────────────────────────────────────────────────────────────────────

func TestMemoriesV2_Namespaces_Success(t *testing.T) {
	resolver := &fakeNSResolver{
		readable: []namespace.Namespace{
			{Name: "workspace:abc-1234-5678", Kind: contract.NamespaceKindWorkspace},
			{Name: "team:t-99", Kind: contract.NamespaceKindTeam},
			{Name: "org:acme", Kind: contract.NamespaceKindOrg},
			{Name: "custom:special", Kind: contract.NamespaceKindCustom},
		},
		writable: []namespace.Namespace{
			{Name: "workspace:abc-1234-5678", Kind: contract.NamespaceKindWorkspace},
		},
	}
	h := newWiredHandler(&fakePlugin{}, resolver)

	rec := doRequest(t, h, http.MethodGet, "/workspaces/ws-a/v2/namespaces",
		gin.Params{{Key: "id", Value: "ws-a"}})
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body NamespacesResponse
	mustJSON(t, rec.Body.Bytes(), &body)

	if len(body.Readable) != 4 {
		t.Errorf("expected 4 readable, got %d", len(body.Readable))
	}
	if len(body.Writable) != 1 {
		t.Errorf("expected 1 writable, got %d", len(body.Writable))
	}

	// Label shaping pinned exactly — drift would silently break the
	// dropdown rendering.
	wantLabels := map[string]string{
		"workspace:abc-1234-5678": "Workspace (abc-1234)",
		"team:t-99":               "Team (t-99)",
		"org:acme":                "Org (acme)",
		"custom:special":          "special",
	}
	for _, v := range body.Readable {
		want, ok := wantLabels[v.Name]
		if !ok {
			t.Errorf("unexpected namespace name %q", v.Name)
			continue
		}
		if v.Label != want {
			t.Errorf("namespace %q: want label %q, got %q", v.Name, want, v.Label)
		}
	}
}

func TestMemoriesV2_Namespaces_ReadableError(t *testing.T) {
	resolver := &fakeNSResolver{readableErr: errors.New("boom")}
	h := newWiredHandler(&fakePlugin{}, resolver)

	rec := doRequest(t, h, http.MethodGet, "/workspaces/ws-a/v2/namespaces",
		gin.Params{{Key: "id", Value: "ws-a"}})
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", rec.Code)
	}
}

func TestMemoriesV2_Namespaces_WritableError(t *testing.T) {
	resolver := &fakeNSResolver{
		readable:    []namespace.Namespace{},
		writableErr: errors.New("boom"),
	}
	h := newWiredHandler(&fakePlugin{}, resolver)

	rec := doRequest(t, h, http.MethodGet, "/workspaces/ws-a/v2/namespaces",
		gin.Params{{Key: "id", Value: "ws-a"}})
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", rec.Code)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// GET /v2/memories — search path
// ─────────────────────────────────────────────────────────────────────────────

func TestMemoriesV2_Search_NoReadableNamespaces_EmptyResult(t *testing.T) {
	// Empty intersection (e.g. workspace just provisioned, plugin
	// hasn't created namespaces yet, OR caller asked for ns they
	// can't read). Expected: 200 with empty memories array, NOT 404.
	resolver := &fakeNSResolver{intersect: []string{}}
	plugin := &fakePlugin{searchResp: &contract.SearchResponse{Memories: []contract.Memory{}}}
	h := newWiredHandler(plugin, resolver)

	rec := doRequest(t, h, http.MethodGet, "/workspaces/ws-a/v2/memories",
		gin.Params{{Key: "id", Value: "ws-a"}})
	if rec.Code != 200 {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	var body MemoriesResponse
	mustJSON(t, rec.Body.Bytes(), &body)
	if body.Memories == nil {
		t.Error("Memories should be empty array, not nil — JSON would render null")
	}
	if len(body.Memories) != 0 {
		t.Errorf("expected empty memories, got %d", len(body.Memories))
	}
	// Plugin must NOT be called when intersection is empty.
	if plugin.searchReq.Namespaces != nil {
		t.Error("plugin Search should not be called when intersection is empty")
	}
}

func TestMemoriesV2_Search_FullPath_NamespaceQueryKindLimit(t *testing.T) {
	expiresAt := time.Now().Add(24 * time.Hour)
	resolver := &fakeNSResolver{intersect: []string{"workspace:ws-a"}}
	score := 0.87
	plugin := &fakePlugin{
		searchResp: &contract.SearchResponse{
			Memories: []contract.Memory{
				{
					ID:        "m-1",
					Namespace: "workspace:ws-a",
					Content:   "fact one",
					Kind:      contract.MemoryKindFact,
					Source:    contract.MemorySourceAgent,
					Pin:       true,
					ExpiresAt: &expiresAt,
					CreatedAt: time.Now(),
					Score:     &score,
					Propagation: map[string]interface{}{
						"source_workspace_id": "ws-peer-42",
					},
				},
			},
		},
	}
	h := newWiredHandler(plugin, resolver)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = gin.Params{{Key: "id", Value: "ws-a"}}
	c.Request = httptest.NewRequest(http.MethodGet,
		"/workspaces/ws-a/v2/memories?namespace=workspace:ws-a&q=hello&kind=fact&limit=10", nil)
	h.Search(c)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	// Resolver received the requested namespace as a single-element list
	if len(resolver.intersectIn) != 1 || resolver.intersectIn[0] != "workspace:ws-a" {
		t.Errorf("resolver.IntersectReadable received %v, want [workspace:ws-a]", resolver.intersectIn)
	}
	// Plugin received query + kind + limit propagated through
	if plugin.searchReq.Query != "hello" {
		t.Errorf("plugin.Query=%q, want hello", plugin.searchReq.Query)
	}
	if len(plugin.searchReq.Kinds) != 1 || plugin.searchReq.Kinds[0] != contract.MemoryKindFact {
		t.Errorf("plugin.Kinds=%v, want [fact]", plugin.searchReq.Kinds)
	}
	if plugin.searchReq.Limit != 10 {
		t.Errorf("plugin.Limit=%d, want 10", plugin.searchReq.Limit)
	}
	// Response shape — pin/expires_at/score/source_workspace_id all
	// surfaced into MemoryView so the canvas doesn't have to dig
	// through propagation map.
	var body MemoriesResponse
	mustJSON(t, rec.Body.Bytes(), &body)
	if len(body.Memories) != 1 {
		t.Fatalf("expected 1 memory, got %d", len(body.Memories))
	}
	m := body.Memories[0]
	if !m.Pin {
		t.Error("Pin not propagated")
	}
	if m.ExpiresAt == nil {
		t.Error("ExpiresAt not propagated")
	}
	if m.Score == nil || *m.Score != 0.87 {
		t.Errorf("Score=%v, want 0.87", m.Score)
	}
	if m.SourceWorkspaceID != "ws-peer-42" {
		t.Errorf("SourceWorkspaceID=%q, want ws-peer-42", m.SourceWorkspaceID)
	}
}

func TestMemoriesV2_Search_NoNamespaceQuery_AllReadable(t *testing.T) {
	// No ?namespace= → resolver.IntersectReadable receives nil (empty
	// requested) and returns ALL readable. Plugin gets full set.
	resolver := &fakeNSResolver{intersect: []string{"workspace:ws-a", "team:t-1"}}
	plugin := &fakePlugin{searchResp: &contract.SearchResponse{}}
	h := newWiredHandler(plugin, resolver)

	rec := doRequest(t, h, http.MethodGet, "/workspaces/ws-a/v2/memories",
		gin.Params{{Key: "id", Value: "ws-a"}})
	if rec.Code != 200 {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if resolver.intersectIn != nil {
		t.Errorf("requested should be nil for unscoped query, got %v", resolver.intersectIn)
	}
	if len(plugin.searchReq.Namespaces) != 2 {
		t.Errorf("plugin.Namespaces=%v, want both readable", plugin.searchReq.Namespaces)
	}
}

func TestMemoriesV2_Search_IntersectError(t *testing.T) {
	resolver := &fakeNSResolver{intersectErr: errors.New("db down")}
	h := newWiredHandler(&fakePlugin{}, resolver)

	rec := doRequest(t, h, http.MethodGet, "/workspaces/ws-a/v2/memories",
		gin.Params{{Key: "id", Value: "ws-a"}})
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", rec.Code)
	}
}

func TestMemoriesV2_Search_PluginError(t *testing.T) {
	resolver := &fakeNSResolver{intersect: []string{"workspace:ws-a"}}
	plugin := &fakePlugin{searchErr: errors.New("plugin down")}
	h := newWiredHandler(plugin, resolver)

	rec := doRequest(t, h, http.MethodGet, "/workspaces/ws-a/v2/memories",
		gin.Params{{Key: "id", Value: "ws-a"}})
	if rec.Code != http.StatusBadGateway {
		t.Errorf("expected 502 (plugin error), got %d", rec.Code)
	}
}

func TestMemoriesV2_Search_PropagationMissing_NoSourceWorkspaceID(t *testing.T) {
	resolver := &fakeNSResolver{intersect: []string{"workspace:ws-a"}}
	plugin := &fakePlugin{
		searchResp: &contract.SearchResponse{
			Memories: []contract.Memory{
				{ID: "m-1", Namespace: "workspace:ws-a", Content: "no propagation"},
			},
		},
	}
	h := newWiredHandler(plugin, resolver)

	rec := doRequest(t, h, http.MethodGet, "/workspaces/ws-a/v2/memories",
		gin.Params{{Key: "id", Value: "ws-a"}})
	var body MemoriesResponse
	mustJSON(t, rec.Body.Bytes(), &body)
	if len(body.Memories) != 1 || body.Memories[0].SourceWorkspaceID != "" {
		t.Errorf("SourceWorkspaceID should be empty when propagation is nil, got %q", body.Memories[0].SourceWorkspaceID)
	}
}

func TestMemoriesV2_Search_PropagationWrongType_DoesNotPanic(t *testing.T) {
	resolver := &fakeNSResolver{intersect: []string{"workspace:ws-a"}}
	plugin := &fakePlugin{
		searchResp: &contract.SearchResponse{
			Memories: []contract.Memory{
				{
					ID:      "m-1",
					Content: "wrong-type propagation",
					Propagation: map[string]interface{}{
						"source_workspace_id": 12345, // int, not string
					},
				},
			},
		},
	}
	h := newWiredHandler(plugin, resolver)

	rec := doRequest(t, h, http.MethodGet, "/workspaces/ws-a/v2/memories",
		gin.Params{{Key: "id", Value: "ws-a"}})
	if rec.Code != 200 {
		t.Fatalf("expected 200 (graceful), got %d", rec.Code)
	}
	var body MemoriesResponse
	mustJSON(t, rec.Body.Bytes(), &body)
	// Wrong-typed prop entry → empty SourceWorkspaceID, no panic.
	if body.Memories[0].SourceWorkspaceID != "" {
		t.Errorf("expected empty SourceWorkspaceID for non-string propagation, got %q", body.Memories[0].SourceWorkspaceID)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// DELETE /v2/memories/:memoryId
// ─────────────────────────────────────────────────────────────────────────────

func TestMemoriesV2_Forget_Success(t *testing.T) {
	plugin := &fakePlugin{} // forgetErr nil
	h := newWiredHandler(plugin, &fakeNSResolver{})

	rec := doRequest(t, h, http.MethodDelete, "/workspaces/ws-a/v2/memories/mem-42",
		gin.Params{{Key: "id", Value: "ws-a"}, {Key: "memoryId", Value: "mem-42"}})
	if rec.Code != 200 {
		t.Errorf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if plugin.forgetID != "mem-42" {
		t.Errorf("plugin received memoryID=%q, want mem-42", plugin.forgetID)
	}
	if plugin.forgetReq.RequestedByNamespace != "workspace:ws-a" {
		t.Errorf("requested_by_namespace=%q, want workspace:ws-a", plugin.forgetReq.RequestedByNamespace)
	}
}

func TestMemoriesV2_Forget_PluginNotFound_Maps404(t *testing.T) {
	plugin := &fakePlugin{
		forgetErr: &contract.Error{Code: contract.ErrorCodeNotFound, Message: "no such memory"},
	}
	h := newWiredHandler(plugin, &fakeNSResolver{})

	rec := doRequest(t, h, http.MethodDelete, "/workspaces/ws-a/v2/memories/m-1",
		gin.Params{{Key: "id", Value: "ws-a"}, {Key: "memoryId", Value: "m-1"}})
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

func TestMemoriesV2_Forget_PluginOtherError_Maps502(t *testing.T) {
	plugin := &fakePlugin{
		forgetErr: &contract.Error{Code: contract.ErrorCodeInternal, Message: "db dead"},
	}
	h := newWiredHandler(plugin, &fakeNSResolver{})

	rec := doRequest(t, h, http.MethodDelete, "/workspaces/ws-a/v2/memories/m-1",
		gin.Params{{Key: "id", Value: "ws-a"}, {Key: "memoryId", Value: "m-1"}})
	if rec.Code != http.StatusBadGateway {
		t.Errorf("expected 502, got %d", rec.Code)
	}
}

func TestMemoriesV2_Forget_NonContractError_Maps502(t *testing.T) {
	// A raw error (e.g. transport failure) — not a contract.Error —
	// also bubbles up as 502.
	plugin := &fakePlugin{forgetErr: errors.New("connection reset")}
	h := newWiredHandler(plugin, &fakeNSResolver{})

	rec := doRequest(t, h, http.MethodDelete, "/workspaces/ws-a/v2/memories/m-1",
		gin.Params{{Key: "id", Value: "ws-a"}, {Key: "memoryId", Value: "m-1"}})
	if rec.Code != http.StatusBadGateway {
		t.Errorf("expected 502, got %d", rec.Code)
	}
}

func TestMemoriesV2_Forget_MissingMemoryID_400(t *testing.T) {
	h := newWiredHandler(&fakePlugin{}, &fakeNSResolver{})
	rec := doRequest(t, h, http.MethodDelete, "/workspaces/ws-a/v2/memories/",
		gin.Params{{Key: "id", Value: "ws-a"}, {Key: "memoryId", Value: ""}})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// View-shaping unit tests — pin individual helpers
// ─────────────────────────────────────────────────────────────────────────────

// namespaceLabelWithName tests — the new code path that prefers
// DisplayName over UUID-prefix fallback (issue #2988).
func TestNamespaceLabelWithName_PrefersDisplayNameWhenSet(t *testing.T) {
	cases := []struct {
		name         string
		raw          string
		kind         contract.NamespaceKind
		display      string
		want         string
	}{
		{"workspace with name", "workspace:abc-1234", contract.NamespaceKindWorkspace, "mac laptop", "Workspace (mac laptop)"},
		{"team with name", "team:abc-1234", contract.NamespaceKindTeam, "Engineering", "Team (Engineering)"},
		{"org with name", "org:acme", contract.NamespaceKindOrg, "Hongming's Org", "Org (Hongming's Org)"},
		// Custom ignores displayName by design — operator chose the suffix.
		{"custom ignores displayName", "custom:ops-shared", contract.NamespaceKindCustom, "FancyName", "ops-shared"},
		{"unknown kind falls through", "weird:x", contract.NamespaceKind("future"), "WhoCares", "weird:x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := namespaceLabelWithName(tc.raw, tc.kind, tc.display)
			if got != tc.want {
				t.Errorf("namespaceLabelWithName(%q, %q, %q) = %q, want %q",
					tc.raw, tc.kind, tc.display, got, tc.want)
			}
		})
	}
}

func TestNamespaceLabelWithName_FallsBackToUUIDPrefixWhenEmpty(t *testing.T) {
	// When displayName is empty (NULL in DB, lookup miss, etc.), the
	// label shape MUST match the legacy UUID-prefix shape exactly so
	// existing canvas behaviour is unchanged for callers that don't
	// plumb a name.
	cases := []struct {
		raw  string
		kind contract.NamespaceKind
		want string
	}{
		{"workspace:abcdefghij", contract.NamespaceKindWorkspace, "Workspace (abcdefgh)"},
		{"team:t-99", contract.NamespaceKindTeam, "Team (t-99)"},
		{"org:acme", contract.NamespaceKindOrg, "Org (acme)"},
	}
	for _, tc := range cases {
		got := namespaceLabelWithName(tc.raw, tc.kind, "")
		if got != tc.want {
			t.Errorf("displayName=\"\" path: got %q, want %q", got, tc.want)
		}
	}
}

func TestNamespacesToViews_PassesDisplayNameThrough(t *testing.T) {
	in := []namespace.Namespace{
		{Name: "workspace:root-1", Kind: contract.NamespaceKindWorkspace, DisplayName: "mac laptop"},
		{Name: "team:root-1", Kind: contract.NamespaceKindTeam, DisplayName: "mac laptop"}, // root → team aliases self
		{Name: "org:root-1", Kind: contract.NamespaceKindOrg, DisplayName: "mac laptop"},
	}
	out := namespacesToViews(in)
	if len(out) != 3 {
		t.Fatalf("len = %d, want 3", len(out))
	}
	wantLabels := []string{
		"Workspace (mac laptop)",
		"Team (mac laptop)",
		"Org (mac laptop)",
	}
	for i, v := range out {
		if v.Label != wantLabels[i] {
			t.Errorf("[%d] label = %q, want %q", i, v.Label, wantLabels[i])
		}
	}
}

func TestNamespacesToViews_FallsBackToUUIDLabelWhenDisplayNameEmpty(t *testing.T) {
	// Exercises the back-compat path — DisplayName="" plumbs through
	// to namespaceLabelWithName which returns the legacy UUID-prefix
	// label. This is what callers see when the workspaces table
	// has a NULL name (defensive — workspaces.name is NOT NULL today).
	in := []namespace.Namespace{
		{Name: "workspace:root-1", Kind: contract.NamespaceKindWorkspace}, // no DisplayName
	}
	out := namespacesToViews(in)
	if out[0].Label != "Workspace (root-1)" {
		t.Errorf("fallback label = %q, want %q", out[0].Label, "Workspace (root-1)")
	}
}

func TestNamespaceLabel_AllKinds(t *testing.T) {
	cases := []struct {
		name string
		kind contract.NamespaceKind
		want string
	}{
		{"workspace:abcdefghij", contract.NamespaceKindWorkspace, "Workspace (abcdefgh)"}, // truncated to 8
		{"workspace:abc", contract.NamespaceKindWorkspace, "Workspace (abc)"},             // shorter than 8, kept as-is
		{"team:t-99", contract.NamespaceKindTeam, "Team (t-99)"},
		{"org:acme", contract.NamespaceKindOrg, "Org (acme)"},
		{"custom:my-ns", contract.NamespaceKindCustom, "my-ns"},
		{"custom:", contract.NamespaceKindCustom, "custom:"}, // empty suffix → fallback to raw name
		{"weird-no-colon", contract.NamespaceKindWorkspace, "Workspace ()"},
		{"unknown:x", contract.NamespaceKind("future"), "unknown:x"}, // unknown kind → fallback to raw name
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := namespaceLabel(tc.name, tc.kind)
			if got != tc.want {
				t.Errorf("namespaceLabel(%q, %q) = %q, want %q", tc.name, tc.kind, got, tc.want)
			}
		})
	}
}

func TestParseLimit(t *testing.T) {
	cases := []struct {
		raw  string
		want int
	}{
		{"", memoriesV2DefaultLimit},
		{"10", 10},
		{"0", memoriesV2DefaultLimit},        // ≤0 → default, not error
		{"-5", memoriesV2DefaultLimit},       // negative → default
		{"abc", memoriesV2DefaultLimit},      // non-numeric → default
		{"99999", memoriesV2MaxLimit},        // over cap → clamped
		{"100", memoriesV2MaxLimit},          // exactly cap → kept
		{"99", 99},                           // just under cap → kept
	}
	for _, tc := range cases {
		t.Run("raw="+tc.raw, func(t *testing.T) {
			if got := parseLimit(tc.raw); got != tc.want {
				t.Errorf("parseLimit(%q) = %d, want %d", tc.raw, got, tc.want)
			}
		})
	}
}

func TestMemoryToView_AllFieldsPropagated(t *testing.T) {
	now := time.Now()
	exp := now.Add(time.Hour)
	score := 0.95
	m := contract.Memory{
		ID:        "m-1",
		Namespace: "team:t-1",
		Content:   "hello",
		Kind:      contract.MemoryKindSummary,
		Source:    contract.MemorySourceUser,
		Pin:       true,
		ExpiresAt: &exp,
		CreatedAt: now,
		Score:     &score,
		Propagation: map[string]interface{}{
			"source_workspace_id": "ws-other",
		},
	}
	v := memoryToView(m)
	if v.ID != m.ID || v.Namespace != m.Namespace || v.Content != m.Content {
		t.Errorf("basic fields: %+v", v)
	}
	if v.Kind != contract.MemoryKindSummary || v.Source != contract.MemorySourceUser {
		t.Errorf("kind/source: %+v", v)
	}
	if !v.Pin || v.ExpiresAt == nil || v.Score == nil || *v.Score != 0.95 {
		t.Errorf("pin/expires/score: %+v", v)
	}
	if v.SourceWorkspaceID != "ws-other" {
		t.Errorf("SourceWorkspaceID=%q, want ws-other", v.SourceWorkspaceID)
	}
}

func TestNamespacesToViews_PreservesOrder(t *testing.T) {
	in := []namespace.Namespace{
		{Name: "team:t1", Kind: contract.NamespaceKindTeam},
		{Name: "workspace:w1", Kind: contract.NamespaceKindWorkspace},
	}
	out := namespacesToViews(in)
	if len(out) != 2 {
		t.Fatalf("len=%d", len(out))
	}
	// Resolver determines order; we just preserve it. (Sorting can be
	// added at the resolver layer if the canvas needs it.)
	if out[0].Name != "team:t1" || out[1].Name != "workspace:w1" {
		t.Errorf("order not preserved: %+v", out)
	}
}

func TestNamespacesToViews_EmptyInput_EmptySlice(t *testing.T) {
	out := namespacesToViews(nil)
	if out == nil {
		t.Error("expected empty slice, not nil — JSON-marshals as null otherwise")
	}
	if len(out) != 0 {
		t.Errorf("expected len 0, got %d", len(out))
	}
}

func TestIndexOfColon(t *testing.T) {
	cases := []struct {
		s    string
		want int
	}{
		{"abc:def", 3},
		{":foo", 0},
		{"nocolon", -1},
		{"", -1},
		{"a:b:c", 1}, // first colon only
	}
	for _, tc := range cases {
		if got := indexOfColon(tc.s); got != tc.want {
			t.Errorf("indexOfColon(%q) = %d, want %d", tc.s, got, tc.want)
		}
	}
}

func TestWithMemoryV2_FluentReturnsReceiver(t *testing.T) {
	// WithMemoryV2 is the production wiring path (takes *client.Client +
	// *namespace.Resolver). withMemoryV2APIs is the test path. The
	// production call is structural — assigns the two fields and
	// returns the receiver — but we still want a 100% coverage gate
	// to catch a future refactor that accidentally drops the fluent
	// return (breaking the boot-time chain in router.go).
	//
	// We can't pass nil for the typed pointers and call available()
	// here because Go interface-with-nil-pointer is non-nil at the
	// interface level — `available()` would not detect that as
	// "unwired". The unwired-plugin behaviour is exhaustively
	// covered by TestMemoriesV2_PluginUnwired_All503; this test just
	// pins the fluent contract.
	h := NewMemoriesV2Handler()
	got := h.WithMemoryV2(nil, nil)
	if got != h {
		t.Error("WithMemoryV2 must return receiver for fluent chaining")
	}
}

func TestShortID(t *testing.T) {
	cases := map[string]string{
		"":                    "",
		"short":               "short",
		"exactly8":            "exactly8",
		"longer-than-eight":   "longer-t",
		"abc-1234-5678-90ab":  "abc-1234",
	}
	for in, want := range cases {
		if got := shortID(in); got != want {
			t.Errorf("shortID(%q) = %q, want %q", in, got, want)
		}
	}
}
