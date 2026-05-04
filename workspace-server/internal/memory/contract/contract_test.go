package contract

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- HealthResponse ---

func TestHealthResponse_HasCapability(t *testing.T) {
	cases := []struct {
		name string
		h    *HealthResponse
		cap  string
		want bool
	}{
		{"nil receiver", nil, CapabilityEmbedding, false},
		{"empty caps", &HealthResponse{Capabilities: nil}, CapabilityEmbedding, false},
		{"present", &HealthResponse{Capabilities: []string{CapabilityFTS, CapabilityEmbedding}}, CapabilityEmbedding, true},
		{"absent", &HealthResponse{Capabilities: []string{CapabilityFTS}}, CapabilityEmbedding, false},
		{"unknown cap string", &HealthResponse{Capabilities: []string{"future-cap"}}, "future-cap", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.h.HasCapability(tc.cap); got != tc.want {
				t.Errorf("HasCapability(%q) = %v, want %v", tc.cap, got, tc.want)
			}
		})
	}
}

// --- ValidateNamespaceName ---

func TestValidateNamespaceName(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"empty", "", true},
		{"workspace uuid", "workspace:550e8400-e29b-41d4-a716-446655440000", false},
		{"team uuid", "team:550e8400-e29b-41d4-a716-446655440000", false},
		{"org slug", "org:acme-corp", false},
		{"custom slug", "custom:engineering-shared", false},
		{"no colon", "workspace_self", true},
		{"empty prefix", ":foo", true},
		{"empty body", "workspace:", true},
		{"uppercase prefix", "WORKSPACE:abc", true},
		{"prefix with digit", "ws1:abc", true},
		{"body with space", "workspace:abc def", true},
		{"body with slash", "workspace:abc/def", true},
		{"valid with dots", "workspace:abc.def.ghi", false},
		{"valid with underscores", "workspace:abc_def", false},
		{"valid with double colon in body", "team:abc:def", false},
		{"too long", "workspace:" + strings.Repeat("a", 257), true},
		{"exactly max", "workspace:" + strings.Repeat("a", maxNamespaceLen-len("workspace:")), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateNamespaceName(tc.in)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateNamespaceName(%q) err=%v, wantErr=%v", tc.in, err, tc.wantErr)
			}
		})
	}
}

// --- NamespaceUpsert.Validate ---

func TestNamespaceUpsert_Validate(t *testing.T) {
	cases := []struct {
		name    string
		in      *NamespaceUpsert
		wantErr bool
	}{
		{"nil", nil, true},
		{"workspace kind", &NamespaceUpsert{Kind: NamespaceKindWorkspace}, false},
		{"team kind", &NamespaceUpsert{Kind: NamespaceKindTeam}, false},
		{"org kind", &NamespaceUpsert{Kind: NamespaceKindOrg}, false},
		{"custom kind", &NamespaceUpsert{Kind: NamespaceKindCustom}, false},
		{"empty kind", &NamespaceUpsert{Kind: ""}, true},
		{"unknown kind", &NamespaceUpsert{Kind: "futurekind"}, true},
		{"with TTL", &NamespaceUpsert{Kind: NamespaceKindTeam, ExpiresAt: timePtr(time.Now().Add(time.Hour))}, false},
		{"with metadata", &NamespaceUpsert{Kind: NamespaceKindOrg, Metadata: map[string]interface{}{"tier": "pro"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.in.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() err=%v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

// --- NamespacePatch.Validate ---

func TestNamespacePatch_Validate(t *testing.T) {
	cases := []struct {
		name    string
		in      *NamespacePatch
		wantErr bool
	}{
		{"nil", nil, true},
		{"empty patch", &NamespacePatch{}, true},
		{"only TTL", &NamespacePatch{ExpiresAt: timePtr(time.Now())}, false},
		{"only metadata", &NamespacePatch{Metadata: map[string]interface{}{"k": "v"}}, false},
		{"both fields", &NamespacePatch{ExpiresAt: timePtr(time.Now()), Metadata: map[string]interface{}{"k": "v"}}, false},
		// Note: empty (non-nil) metadata map IS considered a mutation —
		// it lets operators clear metadata by sending {}.
		{"empty metadata map mutates", &NamespacePatch{Metadata: map[string]interface{}{}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.in.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() err=%v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

// --- MemoryWrite.Validate ---

func TestMemoryWrite_Validate(t *testing.T) {
	valid := func(mut func(*MemoryWrite)) *MemoryWrite {
		w := &MemoryWrite{
			Content: "user prefers tabs",
			Kind:    MemoryKindFact,
			Source:  MemorySourceAgent,
		}
		if mut != nil {
			mut(w)
		}
		return w
	}
	cases := []struct {
		name    string
		in      *MemoryWrite
		wantErr bool
	}{
		{"nil", nil, true},
		{"happy path", valid(nil), false},
		{"empty content", valid(func(w *MemoryWrite) { w.Content = "" }), true},
		{"whitespace-only content", valid(func(w *MemoryWrite) { w.Content = "   \t\n " }), true},
		{"summary kind", valid(func(w *MemoryWrite) { w.Kind = MemoryKindSummary }), false},
		{"checkpoint kind", valid(func(w *MemoryWrite) { w.Kind = MemoryKindCheckpoint }), false},
		{"empty kind", valid(func(w *MemoryWrite) { w.Kind = "" }), true},
		{"unknown kind", valid(func(w *MemoryWrite) { w.Kind = "rumor" }), true},
		{"runtime source", valid(func(w *MemoryWrite) { w.Source = MemorySourceRuntime }), false},
		{"user source", valid(func(w *MemoryWrite) { w.Source = MemorySourceUser }), false},
		{"empty source", valid(func(w *MemoryWrite) { w.Source = "" }), true},
		{"unknown source", valid(func(w *MemoryWrite) { w.Source = "spy" }), true},
		{"with embedding", valid(func(w *MemoryWrite) { w.Embedding = []float32{0.1, 0.2, 0.3} }), false},
		{"with TTL", valid(func(w *MemoryWrite) { w.ExpiresAt = timePtr(time.Now().Add(time.Hour)) }), false},
		{"with propagation", valid(func(w *MemoryWrite) { w.Propagation = map[string]interface{}{"hop": 1} }), false},
		{"pin true", valid(func(w *MemoryWrite) { w.Pin = true }), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.in.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() err=%v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

// --- SearchRequest.Validate ---

func TestSearchRequest_Validate(t *testing.T) {
	cases := []struct {
		name    string
		in      *SearchRequest
		wantErr bool
	}{
		{"nil", nil, true},
		{"empty namespaces", &SearchRequest{}, true},
		{"single ns", &SearchRequest{Namespaces: []string{"workspace:abc"}}, false},
		{"multi ns", &SearchRequest{Namespaces: []string{"workspace:abc", "team:def", "org:ghi"}}, false},
		{"invalid ns in list", &SearchRequest{Namespaces: []string{"workspace:abc", "BAD"}}, true},
		{"limit zero", &SearchRequest{Namespaces: []string{"workspace:abc"}, Limit: 0}, false},
		{"limit max", &SearchRequest{Namespaces: []string{"workspace:abc"}, Limit: 100}, false},
		{"limit too high", &SearchRequest{Namespaces: []string{"workspace:abc"}, Limit: 101}, true},
		{"limit negative", &SearchRequest{Namespaces: []string{"workspace:abc"}, Limit: -1}, true},
		{"valid kinds", &SearchRequest{Namespaces: []string{"workspace:abc"}, Kinds: []MemoryKind{MemoryKindFact, MemoryKindSummary}}, false},
		{"invalid kind in list", &SearchRequest{Namespaces: []string{"workspace:abc"}, Kinds: []MemoryKind{"bogus"}}, true},
		{"with query and embedding", &SearchRequest{Namespaces: []string{"workspace:abc"}, Query: "prefs", Embedding: []float32{1, 2, 3}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.in.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() err=%v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

// --- ForgetRequest.Validate ---

func TestForgetRequest_Validate(t *testing.T) {
	cases := []struct {
		name    string
		in      *ForgetRequest
		wantErr bool
	}{
		{"nil", nil, true},
		{"empty ns", &ForgetRequest{}, true},
		{"valid ns", &ForgetRequest{RequestedByNamespace: "workspace:abc"}, false},
		{"invalid ns", &ForgetRequest{RequestedByNamespace: "no-colon"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.in.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() err=%v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

// --- Error type ---

func TestError_Error(t *testing.T) {
	cases := []struct {
		name string
		in   *Error
		want string
	}{
		{"nil", nil, "<nil contract.Error>"},
		{"basic", &Error{Code: ErrorCodeNotFound, Message: "ns gone"}, "memory-plugin: not_found: ns gone"},
		{"with details", &Error{Code: ErrorCodeInternal, Message: "boom", Details: map[string]interface{}{"trace": "x"}}, "memory-plugin: internal: boom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.Error(); got != tc.want {
				t.Errorf("Error() = %q, want %q", got, tc.want)
			}
		})
	}

	// Verifies Error implements the standard error interface so callers
	// can use errors.As/errors.Is. This was missed pre-PR; an incident
	// in PR #2509 was caused by a type that looked like an error but
	// wasn't assertable, so we pin the contract explicitly.
	var e error = &Error{Code: ErrorCodeBadRequest, Message: "x"}
	var target *Error
	if !errors.As(e, &target) {
		t.Errorf("Error must satisfy errors.As to *Error")
	}
}

// --- Round-trip JSON tests for every type ---

func TestRoundTrip_HealthResponse(t *testing.T) {
	original := HealthResponse{
		Status:       "ok",
		Version:      SchemaVersion,
		Capabilities: []string{CapabilityFTS, CapabilityEmbedding, CapabilityTTL},
	}
	roundTripJSON(t, original, &HealthResponse{}, func(got, want interface{}) {
		g := got.(*HealthResponse)
		w := want.(HealthResponse)
		if g.Status != w.Status || g.Version != w.Version {
			t.Errorf("status/version mismatch")
		}
		if len(g.Capabilities) != len(w.Capabilities) {
			t.Errorf("capabilities len mismatch: got %d want %d", len(g.Capabilities), len(w.Capabilities))
		}
	})
}

func TestRoundTrip_Namespace(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	exp := now.Add(24 * time.Hour)
	original := Namespace{
		Name:      "workspace:550e8400-e29b-41d4-a716-446655440000",
		Kind:      NamespaceKindWorkspace,
		ExpiresAt: &exp,
		Metadata:  map[string]interface{}{"owner": "agent-x"},
		CreatedAt: now,
	}
	roundTripJSON(t, original, &Namespace{}, nil)
}

func TestRoundTrip_NamespaceUpsert(t *testing.T) {
	exp := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	original := NamespaceUpsert{
		Kind:      NamespaceKindTeam,
		ExpiresAt: &exp,
		Metadata:  map[string]interface{}{"tier": "pro"},
	}
	roundTripJSON(t, original, &NamespaceUpsert{}, nil)
}

func TestRoundTrip_NamespacePatch(t *testing.T) {
	exp := time.Now().UTC().Truncate(time.Second)
	original := NamespacePatch{
		ExpiresAt: &exp,
		Metadata:  map[string]interface{}{"k": "v"},
	}
	roundTripJSON(t, original, &NamespacePatch{}, nil)
}

func TestRoundTrip_MemoryWrite(t *testing.T) {
	exp := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	original := MemoryWrite{
		Content:     "remembered fact",
		Kind:        MemoryKindFact,
		Source:      MemorySourceAgent,
		ExpiresAt:   &exp,
		Propagation: map[string]interface{}{"hop": float64(1)},
		Pin:         true,
		Embedding:   []float32{0.1, 0.2, 0.3},
	}
	roundTripJSON(t, original, &MemoryWrite{}, func(got, want interface{}) {
		g := got.(*MemoryWrite)
		w := want.(MemoryWrite)
		if g.Content != w.Content || g.Kind != w.Kind || g.Source != w.Source {
			t.Errorf("content/kind/source mismatch")
		}
		if g.Pin != w.Pin {
			t.Errorf("pin mismatch")
		}
		if len(g.Embedding) != len(w.Embedding) {
			t.Errorf("embedding len mismatch")
		}
	})
}

func TestRoundTrip_MemoryWriteResponse(t *testing.T) {
	original := MemoryWriteResponse{
		ID:        "550e8400-e29b-41d4-a716-446655440000",
		Namespace: "workspace:abc",
	}
	roundTripJSON(t, original, &MemoryWriteResponse{}, nil)
}

func TestRoundTrip_Memory(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	score := 0.87
	original := Memory{
		ID:        "550e8400-e29b-41d4-a716-446655440000",
		Namespace: "team:abc",
		Content:   "team agreed on tabs",
		Kind:      MemoryKindFact,
		Source:    MemorySourceAgent,
		CreatedAt: now,
		Score:     &score,
	}
	roundTripJSON(t, original, &Memory{}, func(got, want interface{}) {
		g := got.(*Memory)
		w := want.(Memory)
		if g.ID != w.ID || g.Namespace != w.Namespace {
			t.Errorf("id/ns mismatch")
		}
		if g.Score == nil || *g.Score != *w.Score {
			t.Errorf("score mismatch")
		}
	})
}

func TestRoundTrip_SearchRequest(t *testing.T) {
	original := SearchRequest{
		Namespaces: []string{"workspace:abc", "team:def"},
		Query:      "prefs",
		Kinds:      []MemoryKind{MemoryKindFact, MemoryKindSummary},
		Limit:      20,
		Embedding:  []float32{1, 2, 3},
	}
	roundTripJSON(t, original, &SearchRequest{}, nil)
}

func TestRoundTrip_SearchResponse(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	original := SearchResponse{
		Memories: []Memory{
			{ID: "id-1", Namespace: "workspace:abc", Content: "x", Kind: MemoryKindFact, Source: MemorySourceAgent, CreatedAt: now},
			{ID: "id-2", Namespace: "team:def", Content: "y", Kind: MemoryKindSummary, Source: MemorySourceRuntime, CreatedAt: now},
		},
	}
	roundTripJSON(t, original, &SearchResponse{}, nil)
}

func TestRoundTrip_ForgetRequest(t *testing.T) {
	original := ForgetRequest{RequestedByNamespace: "workspace:abc"}
	roundTripJSON(t, original, &ForgetRequest{}, nil)
}

func TestRoundTrip_Error(t *testing.T) {
	original := Error{
		Code:    ErrorCodeBadRequest,
		Message: "invalid input",
		Details: map[string]interface{}{"field": "kind"},
	}
	roundTripJSON(t, original, &Error{}, nil)
}

// --- Golden vector tests ---
//
// These pin the exact wire shape against committed JSON files. If a
// future refactor accidentally changes a JSON tag or omits a field, the
// golden test fails. Update goldens via `go test -update` (env var
// based; see updateGoldens()).

func TestGolden_HealthResponse_OK(t *testing.T) {
	checkGolden(t, "health_ok.json", HealthResponse{
		Status:       "ok",
		Version:      "1.0.0",
		Capabilities: []string{"fts", "embedding"},
	})
}

func TestGolden_NamespaceUpsert_Workspace(t *testing.T) {
	checkGolden(t, "namespace_upsert_workspace.json", NamespaceUpsert{
		Kind: NamespaceKindWorkspace,
	})
}

func TestGolden_MemoryWrite_Minimal(t *testing.T) {
	checkGolden(t, "memory_write_minimal.json", MemoryWrite{
		Content: "user prefers tabs over spaces",
		Kind:    MemoryKindFact,
		Source:  MemorySourceAgent,
	})
}

func TestGolden_SearchRequest_MultiNamespace(t *testing.T) {
	checkGolden(t, "search_request_multi_namespace.json", SearchRequest{
		Namespaces: []string{
			"workspace:550e8400-e29b-41d4-a716-446655440000",
			"team:660e8400-e29b-41d4-a716-446655440001",
			"org:acme-corp",
		},
		Query: "indentation preferences",
		Limit: 20,
	})
}

func TestGolden_Error_NotFound(t *testing.T) {
	checkGolden(t, "error_not_found.json", Error{
		Code:    ErrorCodeNotFound,
		Message: "namespace not found",
	})
}

// --- Helpers ---

func timePtr(t time.Time) *time.Time { return &t }

// roundTripJSON marshals `original` to JSON, unmarshals into `got`,
// then validates the round-trip integrity. If `extra` is non-nil it
// runs additional type-specific assertions.
func roundTripJSON(t *testing.T, original interface{}, got interface{}, extra func(got, want interface{})) {
	t.Helper()
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := json.Unmarshal(data, got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Re-marshal the unmarshaled value and compare to the original
	// JSON. Catches asymmetric tag bugs (e.g., `omitempty` differences).
	roundData, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if err := jsonEqual(data, roundData); err != nil {
		t.Errorf("round-trip diverged:\n  before: %s\n  after:  %s\n  diff: %v", data, roundData, err)
	}
	if extra != nil {
		extra(got, original)
	}
}

// jsonEqual compares two JSON byte slices semantically (key order
// independent, type-preserving).
func jsonEqual(a, b []byte) error {
	var ax, bx interface{}
	if err := json.Unmarshal(a, &ax); err != nil {
		return fmt.Errorf("a unmarshal: %w", err)
	}
	if err := json.Unmarshal(b, &bx); err != nil {
		return fmt.Errorf("b unmarshal: %w", err)
	}
	an, _ := json.Marshal(ax)
	bn, _ := json.Marshal(bx)
	if string(an) != string(bn) {
		return fmt.Errorf("differ: %s vs %s", an, bn)
	}
	return nil
}

func checkGolden(t *testing.T, filename string, value interface{}) {
	t.Helper()
	path := filepath.Join("testdata", filename)
	got, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got = append(got, '\n')

	if updateGoldens() {
		if err := os.WriteFile(path, got, 0644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run with UPDATE_GOLDENS=1 to create)", path, err)
	}
	if string(got) != string(want) {
		t.Errorf("golden %s mismatch:\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}

func updateGoldens() bool { return os.Getenv("UPDATE_GOLDENS") == "1" }
