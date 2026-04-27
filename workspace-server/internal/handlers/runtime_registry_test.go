package handlers

// Unit tests for runtime_registry.go. Verify:
//   1. Happy path — manifest.json maps correctly to runtime names
//      (including the -default suffix strip).
//   2. "external" is always injected, even on manifests without it.
//   3. Missing file / malformed JSON returns error, caller uses
//      fallback (tested at the initKnownRuntimes level via integration).

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadRuntimesFromManifest_StripsDefaultSuffix(t *testing.T) {
	// This mirrors the real manifest.json: claude-code-default is the
	// "vanilla" variant of claude-code. After load, both names
	// collapse to "claude-code".
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")
	err := os.WriteFile(path, []byte(`{
		"workspace_templates": [
			{"name": "claude-code-default", "repo": "org/t-cc"},
			{"name": "langgraph", "repo": "org/t-lg"},
			{"name": "hermes", "repo": "org/t-hermes"}
		]
	}`), 0600)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := loadRuntimesFromManifest(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want := []string{"claude-code", "langgraph", "hermes", "external"}
	for _, w := range want {
		if _, ok := got[w]; !ok {
			t.Errorf("want runtime %q in set, missing. got=%v", w, keys(got))
		}
	}
	// "claude-code-default" must NOT survive as-is — it should have
	// been normalized to "claude-code" above. If both are present
	// something's wrong with the TrimSuffix.
	if _, ok := got["claude-code-default"]; ok {
		t.Errorf("expected '-default' suffix stripped, still present: %v", keys(got))
	}
}

func TestLoadRuntimesFromManifest_ExternalAlwaysInjected(t *testing.T) {
	// Even a manifest without external (which matches reality —
	// external has no template repo) must still produce "external"
	// in the set, because it's the BYO-compute meta-runtime.
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")
	_ = os.WriteFile(path, []byte(`{"workspace_templates":[{"name":"langgraph","repo":"org/t"}]}`), 0600)

	got, err := loadRuntimesFromManifest(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, ok := got["external"]; !ok {
		t.Errorf("external must be injected even when absent from manifest: %v", keys(got))
	}
}

func TestLoadRuntimesFromManifest_MissingFileErrors(t *testing.T) {
	_, err := loadRuntimesFromManifest("/does/not/exist.json")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoadRuntimesFromManifest_MalformedJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	_ = os.WriteFile(path, []byte("not json"), 0600)
	_, err := loadRuntimesFromManifest(path)
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

// TestRealManifestParses — sanity check against the actual
// monorepo manifest.json so a future schema change to that file
// (e.g. workspace_templates → workspace_runtime_templates) surfaces
// here rather than at prod startup.
func TestRealManifestParses(t *testing.T) {
	path := manifestPath()
	if path == "" {
		t.Skip("manifest.json not discoverable from this test cwd")
	}
	got, err := loadRuntimesFromManifest(path)
	if err != nil {
		t.Fatalf("real manifest load: %v", err)
	}
	// Core runtimes we always expect to ship.
	for _, must := range []string{"langgraph", "hermes", "claude-code", "external"} {
		if _, ok := got[must]; !ok {
			t.Errorf("real manifest missing runtime %q — got=%v", must, keys(got))
		}
	}
}

func keys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
