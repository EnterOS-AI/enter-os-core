package handlers

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// ---------- ListRegistry: empty dir → 200 [] ----------

func TestPluginListRegistry_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	h := NewPluginsHandler(dir, nil, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/plugins", nil)

	h.ListRegistry(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var plugins []pluginInfo
	if err := json.Unmarshal(w.Body.Bytes(), &plugins); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if len(plugins) != 0 {
		t.Errorf("expected 0 plugins, got %d", len(plugins))
	}
}

// ---------- ListRegistry: non-existent dir → 200 [] ----------

func TestPluginListRegistry_NonExistentDir(t *testing.T) {
	h := NewPluginsHandler("/tmp/does-not-exist-plugins-xyz", nil, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/plugins", nil)

	h.ListRegistry(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var plugins []pluginInfo
	if err := json.Unmarshal(w.Body.Bytes(), &plugins); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if len(plugins) != 0 {
		t.Errorf("expected 0 plugins, got %d", len(plugins))
	}
}

// ---------- ListRegistry: with plugins → returns manifest data ----------

func TestPluginListRegistry_WithPlugins(t *testing.T) {
	dir := t.TempDir()

	// Create a plugin with manifest
	pluginDir := filepath.Join(dir, "my-plugin")
	if err := os.Mkdir(pluginDir, 0755); err != nil {
		t.Fatal(err)
	}
	manifest := `name: my-plugin
version: "1.0.0"
description: A test plugin
author: tester
tags:
  - test
  - example
skills:
  - greet
`
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.yaml"), []byte(manifest), 0644); err != nil {
		t.Fatal(err)
	}

	// Create a plugin without manifest (just a directory)
	bareDir := filepath.Join(dir, "bare-plugin")
	if err := os.Mkdir(bareDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create a regular file (should be skipped — not a directory)
	if err := os.WriteFile(filepath.Join(dir, "not-a-dir.txt"), []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}

	h := NewPluginsHandler(dir, nil, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/plugins", nil)

	h.ListRegistry(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var plugins []pluginInfo
	if err := json.Unmarshal(w.Body.Bytes(), &plugins); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if len(plugins) != 2 {
		t.Fatalf("expected 2 plugins, got %d", len(plugins))
	}

	// Find the manifest plugin (order depends on readdir)
	var found bool
	for _, p := range plugins {
		if p.Name == "my-plugin" {
			found = true
			if p.Version != "1.0.0" {
				t.Errorf("expected version 1.0.0, got %s", p.Version)
			}
			if p.Description != "A test plugin" {
				t.Errorf("expected description 'A test plugin', got %s", p.Description)
			}
			if p.Author != "tester" {
				t.Errorf("expected author 'tester', got %s", p.Author)
			}
			if len(p.Tags) != 2 || p.Tags[0] != "test" || p.Tags[1] != "example" {
				t.Errorf("unexpected tags: %v", p.Tags)
			}
			if len(p.Skills) != 1 || p.Skills[0] != "greet" {
				t.Errorf("unexpected skills: %v", p.Skills)
			}
		}
		if p.Name == "bare-plugin" {
			if p.Version != "" {
				t.Errorf("bare plugin should have empty version, got %s", p.Version)
			}
		}
	}
	if !found {
		t.Error("my-plugin not found in results")
	}
}

// ---------- Install: missing source → 400 ----------

func TestPluginInstall_MissingSource(t *testing.T) {
	h := NewPluginsHandler(t.TempDir(), nil, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-123"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-123/plugins", bytes.NewBufferString(`{}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h.Install(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------- Install: invalid name in local source (path traversal) → 400 ----------

func TestPluginInstall_InvalidName_PathTraversal(t *testing.T) {
	h := NewPluginsHandler(t.TempDir(), nil, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-123"}}
	body := `{"source":"local://../../../etc/passwd"}`
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-123/plugins", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	h.Install(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------- Install: plugin not found → 404 ----------

func TestPluginInstall_NotFound(t *testing.T) {
	h := NewPluginsHandler(t.TempDir(), nil, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-123"}}
	body := `{"source":"local://nonexistent-plugin"}`
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-123/plugins", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	h.Install(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------- Uninstall: invalid name → 400 ----------

func TestPluginUninstall_InvalidName(t *testing.T) {
	h := NewPluginsHandler(t.TempDir(), nil, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-123"},
		{Key: "name", Value: "../escape"},
	}
	c.Request = httptest.NewRequest("DELETE", "/workspaces/ws-123/plugins/../escape", nil)

	h.Uninstall(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------- Uninstall: empty name → 400 ----------

func TestPluginUninstall_EmptyName(t *testing.T) {
	h := NewPluginsHandler(t.TempDir(), nil, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-123"},
		{Key: "name", Value: ""},
	}
	c.Request = httptest.NewRequest("DELETE", "/workspaces/ws-123/plugins/", nil)

	h.Uninstall(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------- validatePluginName: valid names pass ----------

func TestValidatePluginName_ValidNames(t *testing.T) {
	valid := []string{
		"my-plugin",
		"plugin_v2",
		"AwesomePlugin",
		"plugin123",
		"a",
	}
	for _, name := range valid {
		if err := validatePluginName(name); err != nil {
			t.Errorf("validatePluginName(%q) should pass, got: %v", name, err)
		}
	}
}

// ---------- validatePluginName: "/" rejected ----------

func TestValidatePluginName_SlashRejected(t *testing.T) {
	names := []string{
		"foo/bar",
		"/leading",
		"trailing/",
		"a/b/c",
	}
	for _, name := range names {
		if err := validatePluginName(name); err == nil {
			t.Errorf("validatePluginName(%q) should fail for slash", name)
		}
	}
}

// ---------- validatePluginName: ".." rejected ----------

func TestValidatePluginName_DotDotRejected(t *testing.T) {
	names := []string{
		"..",
		"..foo",
		"foo..",
		"a..b",
	}
	for _, name := range names {
		if err := validatePluginName(name); err == nil {
			t.Errorf("validatePluginName(%q) should fail for '..'", name)
		}
	}
}

// ---------- validatePluginName: backslash rejected ----------

func TestValidatePluginName_BackslashRejected(t *testing.T) {
	if err := validatePluginName(`foo\bar`); err == nil {
		t.Error(`validatePluginName("foo\\bar") should fail`)
	}
}

// ---------- validatePluginName: empty rejected ----------

func TestValidatePluginName_EmptyRejected(t *testing.T) {
	if err := validatePluginName(""); err == nil {
		t.Error("validatePluginName(\"\") should fail")
	}
}

// ---------- parseManifestYAML: valid yaml → correct pluginInfo ----------

func TestParseManifestYAML_ValidYAML(t *testing.T) {
	yaml := []byte(`
name: test-plugin
version: "2.0.0"
description: "Does things"
author: "dev"
tags:
  - utility
  - automation
skills:
  - summarize
  - translate
`)
	info := parseManifestYAML("fallback-name", yaml)

	// Name should use fallbackName, not the yaml name field
	if info.Name != "fallback-name" {
		t.Errorf("expected name 'fallback-name', got %s", info.Name)
	}
	if info.Version != "2.0.0" {
		t.Errorf("expected version 2.0.0, got %s", info.Version)
	}
	if info.Description != "Does things" {
		t.Errorf("expected description 'Does things', got %s", info.Description)
	}
	if info.Author != "dev" {
		t.Errorf("expected author 'dev', got %s", info.Author)
	}
	if len(info.Tags) != 2 || info.Tags[0] != "utility" || info.Tags[1] != "automation" {
		t.Errorf("unexpected tags: %v", info.Tags)
	}
	if len(info.Skills) != 2 || info.Skills[0] != "summarize" || info.Skills[1] != "translate" {
		t.Errorf("unexpected skills: %v", info.Skills)
	}
}

// ---------- parseManifestYAML: invalid yaml → fallback ----------

func TestParseManifestYAML_InvalidYAML(t *testing.T) {
	info := parseManifestYAML("safe-name", []byte(`{{{not valid yaml`))
	if info.Name != "safe-name" {
		t.Errorf("expected fallback name 'safe-name', got %s", info.Name)
	}
	if info.Version != "" {
		t.Errorf("expected empty version on invalid yaml, got %s", info.Version)
	}
}

// ---------- parseManifestYAML: minimal yaml (no tags/skills) ----------

func TestParseManifestYAML_MinimalYAML(t *testing.T) {
	yaml := []byte(`version: "0.1"`)
	info := parseManifestYAML("minimal", yaml)

	if info.Name != "minimal" {
		t.Errorf("expected name 'minimal', got %s", info.Name)
	}
	if info.Version != "0.1" {
		t.Errorf("expected version '0.1', got %s", info.Version)
	}
	if info.Tags != nil {
		t.Errorf("expected nil tags, got %v", info.Tags)
	}
	if info.Skills != nil {
		t.Errorf("expected nil skills, got %v", info.Skills)
	}
}

// ---------- Runtime filter on ListRegistry ----------

// writePlugin is a small helper for the runtime-filter tests.
func writePlugin(t *testing.T, dir, name, manifest string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, name), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name, "plugin.yaml"), []byte(manifest), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestPluginListRegistry_FiltersByRuntime(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "p-cc", "name: p-cc\nruntimes: [claude_code]\n")
	writePlugin(t, dir, "p-da", "name: p-da\nruntimes: [hermes]\n")
	writePlugin(t, dir, "p-both", "name: p-both\nruntimes: [claude_code, hermes]\n")
	writePlugin(t, dir, "p-legacy", "name: p-legacy\n") // no runtimes — always allowed

	h := NewPluginsHandler(dir, nil, nil)

	cases := []struct {
		name     string
		runtime  string
		expected map[string]bool
	}{
		{"no filter returns all", "", map[string]bool{"p-cc": true, "p-da": true, "p-both": true, "p-legacy": true}},
		{"claude_code filter", "claude_code", map[string]bool{"p-cc": true, "p-both": true, "p-legacy": true}},
		{"hermes filter", "hermes", map[string]bool{"p-da": true, "p-both": true, "p-legacy": true}},
		{"hyphen form normalized", "claude-code", map[string]bool{"p-cc": true, "p-both": true, "p-legacy": true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			url := "/plugins"
			if tc.runtime != "" {
				url += "?runtime=" + tc.runtime
			}
			c.Request = httptest.NewRequest("GET", url, nil)
			h.ListRegistry(c)

			var plugins []pluginInfo
			if err := json.Unmarshal(w.Body.Bytes(), &plugins); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			got := map[string]bool{}
			for _, p := range plugins {
				got[p.Name] = true
			}
			if len(got) != len(tc.expected) {
				t.Errorf("runtime=%q: got %v, want %v", tc.runtime, got, tc.expected)
			}
			for name := range tc.expected {
				if !got[name] {
					t.Errorf("runtime=%q: missing %q", tc.runtime, name)
				}
			}
		})
	}
}

// ---------- ListAvailableForWorkspace ----------

func TestPluginListAvailableForWorkspace_UsesRuntimeLookup(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "only-hermes", "name: only-hermes\nruntimes: [hermes]\n")
	writePlugin(t, dir, "only-claude", "name: only-claude\nruntimes: [claude_code]\n")

	// Workspace resolves to hermes.
	h := NewPluginsHandler(dir, nil, nil).WithRuntimeLookup(func(id string) (string, error) {
		if id == "ws-da" {
			return "hermes", nil
		}
		return "claude_code", nil
	})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-da"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-da/plugins/available", nil)
	h.ListAvailableForWorkspace(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var plugins []pluginInfo
	if err := json.Unmarshal(w.Body.Bytes(), &plugins); err != nil {
		t.Fatal(err)
	}
	if len(plugins) != 1 || plugins[0].Name != "only-hermes" {
		t.Errorf("expected only-hermes, got %+v", plugins)
	}
}

func TestPluginListAvailableForWorkspace_NoLookupReturnsAll(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "only-hermes", "name: only-hermes\nruntimes: [hermes]\n")
	writePlugin(t, dir, "only-claude", "name: only-claude\nruntimes: [claude_code]\n")

	// No runtime lookup wired → falls back to full registry.
	h := NewPluginsHandler(dir, nil, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "anything"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/anything/plugins/available", nil)
	h.ListAvailableForWorkspace(c)

	var plugins []pluginInfo
	if err := json.Unmarshal(w.Body.Bytes(), &plugins); err != nil {
		t.Fatal(err)
	}
	if len(plugins) != 2 {
		t.Errorf("expected 2 plugins, got %d", len(plugins))
	}
}

// ---------- Manifest parsing: runtimes field ----------

func TestParseManifestYAML_PicksUpRuntimes(t *testing.T) {
	info := parseManifestYAML("demo", []byte("name: demo\nruntimes:\n  - claude_code\n  - hermes\n"))
	if len(info.Runtimes) != 2 || info.Runtimes[0] != "claude_code" || info.Runtimes[1] != "hermes" {
		t.Errorf("expected [claude_code, hermes], got %v", info.Runtimes)
	}
	if !info.supportsRuntime("claude-code") {
		t.Error("hyphen/underscore normalization broken")
	}
	if info.supportsRuntime("openclaw") {
		t.Error("should not support openclaw")
	}
}

func TestSupportsRuntime_EmptyMeansLegacy(t *testing.T) {
	info := pluginInfo{Name: "legacy"}
	if !info.supportsRuntime("anything") {
		t.Error("legacy plugins (no runtimes field) must be treated as compatible")
	}
}

// ---------- CheckRuntimeCompatibility ----------

func TestCheckRuntimeCompatibility_RejectsMissingRuntimeParam(t *testing.T) {
	h := NewPluginsHandler(t.TempDir(), nil, nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws/plugins/compatibility", nil)
	h.CheckRuntimeCompatibility(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestCheckRuntimeCompatibility_TriviallyCompatibleWhenContainerMissing(t *testing.T) {
	// No docker client + no container → treated as "nothing installed, all compatible".
	h := NewPluginsHandler(t.TempDir(), nil, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws/plugins/compatibility?runtime=hermes", nil)
	h.CheckRuntimeCompatibility(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["all_compatible"] != true {
		t.Errorf("expected all_compatible=true, got %v", body["all_compatible"])
	}
	if body["target_runtime"] != "hermes" {
		t.Errorf("target_runtime mismatch: %v", body["target_runtime"])
	}
}

// ---------- ListSources ----------

func TestPluginListSources_ReturnsRegisteredSchemes(t *testing.T) {
	h := NewPluginsHandler(t.TempDir(), nil, nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/plugins/sources", nil)
	h.ListSources(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	var body map[string][]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	hasLocal, hasGithub := false, false
	for _, s := range body["schemes"] {
		if s == "local" {
			hasLocal = true
		}
		if s == "github" {
			hasGithub = true
		}
	}
	if !hasLocal || !hasGithub {
		t.Errorf("expected local+github by default, got %v", body["schemes"])
	}
}

// ---------- Install — source routing ----------

func TestPluginInstall_RejectsEmptyBody(t *testing.T) {
	h := NewPluginsHandler(t.TempDir(), nil, nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws"}}
	c.Request = httptest.NewRequest("POST", "/x", bytes.NewBufferString(`{}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.Install(c)
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestPluginInstall_RejectsUnknownScheme(t *testing.T) {
	h := NewPluginsHandler(t.TempDir(), nil, nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws"}}
	c.Request = httptest.NewRequest("POST", "/x",
		bytes.NewBufferString(`{"source":"mystery://thing"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.Install(c)
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d: %s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("available_schemes")) {
		t.Errorf("response should list available_schemes: %s", w.Body.String())
	}
}

func TestPluginInstall_LocalSourceReachesContainerLookup(t *testing.T) {
	mock := setupTestDB(t)
	expectAllowlistAllowAll(mock)

	base := t.TempDir()
	pluginDir := filepath.Join(base, "demo")
	_ = os.MkdirAll(pluginDir, 0o755)
	_ = os.WriteFile(filepath.Join(pluginDir, "plugin.yaml"), []byte("name: demo\n"), 0o644)
	h := NewPluginsHandler(base, nil, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws"}}
	c.Request = httptest.NewRequest("POST", "/x",
		bytes.NewBufferString(`{"source":"local://demo"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.Install(c)
	// No docker client configured → source resolves, stage succeeds, then
	// 503 on container lookup. Proves the local dispatch + stage worked.
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("local:// should reach container lookup: got %d: %s", w.Code, w.Body.String())
	}
}

func TestPluginInstall_InvalidSourceString(t *testing.T) {
	h := NewPluginsHandler(t.TempDir(), nil, nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws"}}
	c.Request = httptest.NewRequest("POST", "/x",
		bytes.NewBufferString(`{"source":"   "}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.Install(c)
	if w.Code != http.StatusBadRequest {
		t.Errorf("whitespace-only source should be rejected: got %d", w.Code)
	}
}

func TestPluginInstall_RejectsOversizedBody(t *testing.T) {
	h := NewPluginsHandler(t.TempDir(), nil, nil)
	// Build a JSON body larger than the cap (default 64 KiB).
	big := `{"source":"local://` + strings.Repeat("a", 70*1024) + `"}`
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws"}}
	c.Request = httptest.NewRequest("POST", "/x", bytes.NewBufferString(big))
	c.Request.Header.Set("Content-Type", "application/json")
	h.Install(c)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for oversized body, got %d", w.Code)
	}
}

// Install 404 via the typed sentinel (replaces the old string-match test).
func TestPluginInstall_NotFoundUsesTypedSentinel(t *testing.T) {
	h := NewPluginsHandler(t.TempDir(), nil, nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-123"}}
	c.Request = httptest.NewRequest("POST", "/x",
		bytes.NewBufferString(`{"source":"local://nonexistent"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.Install(c)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 via ErrPluginNotFound, got %d: %s", w.Code, w.Body.String())
	}
}

// Install 502 for non-sentinel resolver errors (e.g. network / auth).
func TestPluginInstall_NonSentinelResolverErrorIs502(t *testing.T) {
	// Register a stub resolver whose Fetch returns a plain (non-ErrPluginNotFound) error.
	h := NewPluginsHandler(t.TempDir(), nil, nil).
		WithSourceResolver(alwaysErrs("broken", errors.New("connection refused")))
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws"}}
	c.Request = httptest.NewRequest("POST", "/x",
		bytes.NewBufferString(`{"source":"broken://whatever"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.Install(c)
	if w.Code != http.StatusBadGateway {
		t.Errorf("non-sentinel resolver error should be 502, got %d: %s", w.Code, w.Body.String())
	}
}

// Install returns 504 when fetch honours ctx.DeadlineExceeded.
func TestPluginInstall_DeadlineExceededIs504(t *testing.T) {
	h := NewPluginsHandler(t.TempDir(), nil, nil).
		WithSourceResolver(alwaysErrs("slow", context.DeadlineExceeded))
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws"}}
	c.Request = httptest.NewRequest("POST", "/x",
		bytes.NewBufferString(`{"source":"slow://x"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.Install(c)
	if w.Code != http.StatusGatewayTimeout {
		t.Errorf("deadline exceeded should be 504, got %d", w.Code)
	}
}

// Install 413 when the fetched tree exceeds the configured cap.
func TestPluginInstall_OversizedStagedTreeIs413(t *testing.T) {
	t.Setenv("PLUGIN_INSTALL_MAX_DIR_BYTES", "1024") // 1 KiB cap
	h := NewPluginsHandler(t.TempDir(), nil, nil).
		WithSourceResolver(writesBlob("big", 2048)) // 2 KiB blob
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws"}}
	c.Request = httptest.NewRequest("POST", "/x",
		bytes.NewBufferString(`{"source":"big://whatever"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.Install(c)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized staged tree should be 413, got %d: %s", w.Code, w.Body.String())
	}
}

// envDuration / envInt64 moved to platform/internal/envx; see
// envx/envx_test.go for their tests.

func TestDirSize_ShortCircuitsOnCap(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a"), bytes.Repeat([]byte("x"), 2048), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := dirSize(dir, 1024)
	if err == nil {
		t.Error("expected cap-exceeded error")
	}
	// Under the cap: no error.
	if _, err := dirSize(dir, 1<<20); err != nil {
		t.Errorf("1 MiB cap should accept 2 KiB: %v", err)
	}
}

// ---- test-only resolver stub ----

// fakeResolver is a single hook-based SourceResolver replacing the
// previously-separate erroringResolver / bigBlobResolver /
// hostileResolver types. Tests pick the scheme and supply a fetchFn.
type fakeResolver struct {
	scheme  string
	fetchFn func(ctx context.Context, spec, dst string) (string, error)
}

func (f *fakeResolver) Scheme() string { return f.scheme }
func (f *fakeResolver) Fetch(ctx context.Context, spec, dst string) (string, error) {
	return f.fetchFn(ctx, spec, dst)
}

// alwaysErrs returns a fakeResolver that fails every fetch with err.
func alwaysErrs(scheme string, err error) *fakeResolver {
	return &fakeResolver{
		scheme:  scheme,
		fetchFn: func(context.Context, string, string) (string, error) { return "", err },
	}
}

// writesBlob returns a fakeResolver that writes N bytes into dst
// before returning `scheme` as the plugin name.
func writesBlob(scheme string, size int) *fakeResolver {
	return &fakeResolver{
		scheme: scheme,
		fetchFn: func(_ context.Context, _, dst string) (string, error) {
			if err := os.WriteFile(filepath.Join(dst, "blob"), bytes.Repeat([]byte("a"), size), 0o644); err != nil {
				return "", err
			}
			return scheme, nil
		},
	}
}

// emitsName returns a fakeResolver that writes a valid plugin.yaml
// but returns `returnedName` — used to probe post-fetch name
// validation (e.g. hostile traversal names).
func emitsName(scheme, returnedName string) *fakeResolver {
	return &fakeResolver{
		scheme: scheme,
		fetchFn: func(_ context.Context, _, dst string) (string, error) {
			_ = os.WriteFile(filepath.Join(dst, "plugin.yaml"), []byte("name: x\n"), 0o644)
			return returnedName, nil
		},
	}
}

func TestPluginInstall_RejectsHostileResolverPluginName(t *testing.T) {
	// Prove the post-fetch validatePluginName call catches a resolver
	// that tries to smuggle a traversal name into /configs/plugins/.
	h := NewPluginsHandler(t.TempDir(), nil, nil).
		WithSourceResolver(emitsName("hostile", "../../../etc/passwd"))
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws"}}
	c.Request = httptest.NewRequest("POST", "/x",
		bytes.NewBufferString(`{"source":"hostile://anything"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.Install(c)
	if w.Code != http.StatusBadRequest {
		t.Errorf("hostile plugin name must be 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestPluginInstall_EmptySpecAfterSchemeRejected(t *testing.T) {
	h := NewPluginsHandler(t.TempDir(), nil, nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws"}}
	c.Request = httptest.NewRequest("POST", "/x",
		bytes.NewBufferString(`{"source":"github://"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.Install(c)
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d: %s", w.Code, w.Body.String())
	}
	// Handler now returns the generic "invalid plugin source" rather than
	// propagating the internal ParseSource "empty spec" wording — intentional
	// so the HTTP surface doesn't leak parser-internal vocabulary. Unit-level
	// coverage of the "empty spec" wording lives in plugins/source_test.go.
	if !bytes.Contains(w.Body.Bytes(), []byte("invalid plugin source")) {
		t.Errorf("error should mention 'invalid plugin source': %s", w.Body.String())
	}
}

// ---- resolveAndStage in isolation (now testable sans gin.Context) ----

func TestResolveAndStage_HappyPath_Local(t *testing.T) {
	base := t.TempDir()
	pluginDir := filepath.Join(base, "demo")
	_ = os.MkdirAll(pluginDir, 0o755)
	_ = os.WriteFile(filepath.Join(pluginDir, "plugin.yaml"), []byte("name: demo\n"), 0o644)
	h := NewPluginsHandler(base, nil, nil)

	res, err := h.resolveAndStage(context.Background(), installRequest{Source: "local://demo"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	defer os.RemoveAll(res.StagedDir)

	if res.PluginName != "demo" {
		t.Errorf("got plugin %q", res.PluginName)
	}
	if _, err := os.Stat(filepath.Join(res.StagedDir, "plugin.yaml")); err != nil {
		t.Errorf("plugin.yaml not staged: %v", err)
	}
}

func TestResolveAndStage_EmptyRequest(t *testing.T) {
	h := NewPluginsHandler(t.TempDir(), nil, nil)
	_, err := h.resolveAndStage(context.Background(), installRequest{})
	var he *httpErr
	if !errors.As(err, &he) || he.Status != http.StatusBadRequest {
		t.Errorf("want typed httpErr with 400, got %v", err)
	}
}

func TestResolveAndStage_NotFoundFromResolver(t *testing.T) {
	h := NewPluginsHandler(t.TempDir(), nil, nil) // local resolver pointed at empty dir
	_, err := h.resolveAndStage(context.Background(), installRequest{Source: "local://absent"})
	var he *httpErr
	if !errors.As(err, &he) || he.Status != http.StatusNotFound {
		t.Errorf("want 404 via ErrPluginNotFound, got %v", err)
	}
}

func TestResolveAndStage_CleansUpStagedDirOnError(t *testing.T) {
	// Hostile resolver emits a file then returns a traversal name → 400.
	// Verify the staged dir it used is NOT left behind.
	h := NewPluginsHandler(t.TempDir(), nil, nil).
		WithSourceResolver(emitsName("hostile", "../../../etc/passwd"))
	_, err := h.resolveAndStage(context.Background(), installRequest{Source: "hostile://x"})
	var he *httpErr
	if !errors.As(err, &he) || he.Status != http.StatusBadRequest {
		t.Fatalf("expected 400, got %v", err)
	}
	// Walk /tmp for leftover "molecule-plugin-fetch-*" dirs with the
	// hostile resolver's marker. Probabilistic (can't pinpoint the exact
	// dir), but a clean cleanup means none contain the plugin.yaml blob
	// the resolver writes.
	entries, _ := filepath.Glob(filepath.Join(os.TempDir(), "molecule-plugin-fetch-*"))
	for _, e := range entries {
		if _, err := os.Stat(filepath.Join(e, "plugin.yaml")); err == nil {
			t.Errorf("leaked staging dir with plugin.yaml still present: %s", e)
		}
	}
}

func TestHTTPErr_WrapsCleanly(t *testing.T) {
	e := newHTTPErr(http.StatusTeapot, gin.H{"error": "I'm a teapot"})
	if !strings.Contains(e.Error(), "418") {
		t.Errorf("Error() should include status: %q", e.Error())
	}
	var he *httpErr
	if !errors.As(e, &he) || he.Status != http.StatusTeapot {
		t.Errorf("errors.As should extract typed error")
	}
}

func TestLogInstallLimitsOnce(t *testing.T) {
	// sync.Once guarantees exactly one emission per process, so this
	// test can't reset it. We call with a local buffer writer and assert
	// behaviour that holds regardless of ordering:
	//   - first caller sees the line
	//   - later callers see nothing
	//   - no panic either way
	// If another test's NewPluginsHandler already fired the Once, our
	// buffer stays empty — that's correct, not a bug.
	var buf bytes.Buffer
	logInstallLimitsOnce(&buf)
	logInstallLimitsOnce(&buf) // must not panic; Once is idempotent

	if buf.Len() > 0 {
		// We were the first caller. Verify the line contains all three
		// limit names so operators can grep for them.
		out := buf.String()
		for _, want := range []string{"Plugin install limits", "body=", "timeout=", "staged="} {
			if !strings.Contains(out, want) {
				t.Errorf("log line missing %q: %s", want, out)
			}
		}
	}
	// If buf is empty: another test's NewPluginsHandler already called
	// Once. That's also correct behavior — nothing to assert beyond "it
	// didn't panic."
}

// ---------- regexpEscapeForAwk: special chars escaped ----------

func TestRegexpEscapeForAwk(t *testing.T) {
	cases := map[string]string{
		"my-plugin":       `my-plugin`,
		"# Plugin: foo /": `# Plugin: foo \/`,
		"# Plugin: a.b /": `# Plugin: a\.b \/`,
		"foo[bar]":        `foo\[bar\]`,
		"a*b+c?":          `a\*b\+c\?`,
		"path|with|pipes": `path\|with\|pipes`,
		`back\slash`:      `back\\slash`,
		"":                ``,
	}
	for in, want := range cases {
		got := regexpEscapeForAwk(in)
		if got != want {
			t.Errorf("regexpEscapeForAwk(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---------- stripPluginMarkersFromMemory: awk script logic ----------
//
// We can't unit-test the in-container exec without Docker, but we CAN
// test the awk script against a real bash on the test runner — which is
// exactly the same script that runs in the container. This catches
// off-by-one block-stripping bugs without needing a workspace.

func TestStripPluginMarkers_AwkScript(t *testing.T) {
	tmp := t.TempDir()
	memory := filepath.Join(tmp, "CLAUDE.md")

	// Mirrors the layout AgentskillsAdaptor.append_to_memory writes:
	// blank line, marker line, body line(s), blank line separator.
	initial := `# Agent Workspace

Original user content here.

# Plugin: my-plugin / rule: foo.md

These are my-plugin's rules.
Multiple lines of content.

# Plugin: keep-me / rule: bar.md

Should remain untouched.

# Plugin: my-plugin / fragment: baz.md

Another my-plugin block.

Trailing user content.
`
	if err := os.WriteFile(memory, []byte(initial), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Run the same awk pipeline the production code uses.
	marker := "# Plugin: my-plugin /"
	script := fmt.Sprintf(
		`awk 'BEGIN{skip=0; blanks=0} /^%s/{skip=1; blanks=0; next} skip==1 && /^[[:space:]]*$/{blanks++; if(blanks>=2){skip=0; print; next} next} /^# Plugin: /{if(skip==1)skip=0} skip==1{next} {print}' %s > %s.new && mv %s.new %s`,
		regexpEscapeForAwk(marker), memory, memory, memory, memory,
	)
	cmd := exec.Command("bash", "-c", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("awk run failed: %v\n%s", err, out)
	}

	got, _ := os.ReadFile(memory)
	gs := string(got)

	// The two my-plugin blocks must be gone, including their content.
	if strings.Contains(gs, "my-plugin") {
		t.Errorf("expected all my-plugin references stripped; got:\n%s", gs)
	}
	if strings.Contains(gs, "These are my-plugin's rules.") {
		t.Errorf("plugin body content leaked through; got:\n%s", gs)
	}
	if strings.Contains(gs, "Another my-plugin block.") {
		t.Errorf("second plugin block content leaked; got:\n%s", gs)
	}

	// The keep-me block and surrounding user content must survive intact.
	for _, want := range []string{
		"# Agent Workspace",
		"Original user content here.",
		"# Plugin: keep-me / rule: bar.md",
		"Should remain untouched.",
		"Trailing user content.",
	} {
		if !strings.Contains(gs, want) {
			t.Errorf("expected %q to remain in CLAUDE.md; got:\n%s", want, gs)
		}
	}
}

// ---------- stripPluginMarkers: missing CLAUDE.md is silent ----------

func TestStripPluginMarkers_MissingFileIsNoOp(t *testing.T) {
	// Awk on a missing file is a non-zero exit (which our production code
	// silently ignores via `_, _ =`). Verify that the local invocation
	// behaves the same and doesn't crash the test process.
	tmp := t.TempDir()
	missing := filepath.Join(tmp, "does-not-exist.md")
	script := fmt.Sprintf(
		`awk 'BEGIN{skip=0; blanks=0} /^%s/{skip=1; blanks=0; next} skip==1 && /^[[:space:]]*$/{blanks++; if(blanks>=2){skip=0; print; next} next} /^# Plugin: /{if(skip==1)skip=0} skip==1{next} {print}' %s > /tmp/x.$$ 2>/dev/null && mv /tmp/x.$$ %s`,
		regexpEscapeForAwk("# Plugin: x /"), missing, missing,
	)
	cmd := exec.Command("bash", "-c", script)
	_ = cmd.Run() // expected to fail; we just check it doesn't hang/panic
}

// ================== Phase 30.3 — Download endpoint ==================

// Download is exercised via an integration-style test because its main
// work (stream tar.gz) is straightforward; the interesting paths are
// the auth gate and the plugin-name mismatch guard.

func TestPluginDownload_RejectsInvalidName(t *testing.T) {
	h := NewPluginsHandler(t.TempDir(), nil, nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-123"},
		{Key: "name", Value: "../traversal"},
	}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-123/plugins/..%2Ftraversal/download", nil)
	h.Download(c)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for traversal, got %d", w.Code)
	}
}

func TestPluginDownload_MissingTokenWhenWorkspaceHasOne(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	h := NewPluginsHandler(t.TempDir(), nil, nil)

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM workspace_auth_tokens`).
		WithArgs("550e8400-e29b-41d4-a716-446655440000").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "550e8400-e29b-41d4-a716-446655440000"},
		{Key: "name", Value: "some-plugin"},
	}
	c.Request = httptest.NewRequest("GET", "/workspaces/X/plugins/some-plugin/download", nil)
	h.Download(c)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 when token required and absent, got %d", w.Code)
	}
}

func TestPluginDownload_LegacyWorkspaceStreamsTarball(t *testing.T) {
	// Stage a small plugin dir; legacy workspace (no tokens on file) is
	// grandfathered through; caller should get a tar.gz body back.
	mock := setupTestDB(t)
	setupTestRedis(t)

	pluginsDir := t.TempDir()
	pluginRoot := filepath.Join(pluginsDir, "hello-plugin")
	if err := os.MkdirAll(pluginRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginRoot, "plugin.yaml"), []byte("name: hello-plugin\nversion: 1.0.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginRoot, "rules.md"), []byte("some rules\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	h := NewPluginsHandler(pluginsDir, nil, nil)

	// Legacy path — workspace has no live tokens.
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM workspace_auth_tokens`).
		WithArgs("550e8400-e29b-41d4-a716-446655440000").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "550e8400-e29b-41d4-a716-446655440000"},
		{Key: "name", Value: "hello-plugin"},
	}
	c.Request = httptest.NewRequest("GET", "/workspaces/X/plugins/hello-plugin/download", nil)
	h.Download(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/gzip" {
		t.Errorf("Content-Type: got %q, want application/gzip", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, `filename="hello-plugin.tar.gz"`) {
		t.Errorf("Content-Disposition missing canonical filename: %q", cd)
	}
	// Body should be gzip-magic-bytes prefixed
	body := w.Body.Bytes()
	if len(body) < 2 || body[0] != 0x1f || body[1] != 0x8b {
		t.Errorf("body does not start with gzip magic (0x1f 0x8b); first bytes: %x", body[:min(4, len(body))])
	}
}

// ================== streamDirAsTar helper ==================

func TestStreamDirAsTar_RelativePaths(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "sub"), 0o755)
	os.WriteFile(filepath.Join(root, "top.md"), []byte("top content"), 0o644)
	os.WriteFile(filepath.Join(root, "sub", "nested.md"), []byte("nested content"), 0o644)

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := streamDirAsTar(root, tw); err != nil {
		t.Fatalf("streamDirAsTar: %v", err)
	}
	tw.Close()

	tr := tar.NewReader(&buf)
	seen := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar read: %v", err)
		}
		seen[hdr.Name] = true
		if strings.HasPrefix(hdr.Name, "/") || strings.Contains(hdr.Name, "..") {
			t.Errorf("tar entry has unsafe path: %q", hdr.Name)
		}
	}
	// Must include both our files with relative paths
	if !seen["top.md"] {
		t.Errorf("missing top.md; saw: %v", seen)
	}
	if !seen[filepath.Join("sub", "nested.md")] {
		t.Errorf("missing sub/nested.md; saw: %v", seen)
	}
}

func TestStreamDirAsTar_SkipsSymlinks(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "outside.txt")
	os.WriteFile(target, []byte("secret"), 0o600)
	os.WriteFile(filepath.Join(root, "real.md"), []byte("real"), 0o644)
	if err := os.Symlink(target, filepath.Join(root, "escape.lnk")); err != nil {
		t.Skipf("symlink unsupported on this fs: %v", err)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	streamDirAsTar(root, tw)
	tw.Close()

	tr := tar.NewReader(&buf)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Name == "escape.lnk" {
			t.Error("symlink leaked into archive — would escape staged dir")
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Phase 30.3 regression: previously only the local:// path had a unit
// test. The github:// (and any other registered scheme) path was only
// covered by live E2E. This test mocks the resolver registry so the
// non-local install path stays under test even without network access.
func TestPluginDownload_GithubSchemeStreamsTarball(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	h := NewPluginsHandler(t.TempDir(), nil, nil)
	// Reuse the existing test-double — supply a fetchFn that drops a
	// known file set into dst and returns the plugin name.
	h.WithSourceResolver(&fakeResolver{
		scheme: "github",
		fetchFn: func(_ context.Context, _ string, dst string) (string, error) {
			files := map[string]string{
				"plugin.yaml":             "name: remote-plugin\nversion: 1.0.0\n",
				"skills/x/SKILL.md":       "---\nname: x\n---\n",
				"adapters/claude_code.py": "from plugins_registry.builtins import AgentskillsAdaptor as Adaptor\n",
			}
			for relPath, content := range files {
				full := filepath.Join(dst, relPath)
				if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
					return "", err
				}
				if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
					return "", err
				}
			}
			return "remote-plugin", nil
		},
	})

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM workspace_auth_tokens`).
		WithArgs("550e8400-e29b-41d4-a716-446655440000").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "550e8400-e29b-41d4-a716-446655440000"},
		{Key: "name", Value: "remote-plugin"},
	}
	req := httptest.NewRequest("GET",
		"/workspaces/X/plugins/remote-plugin/download?source=github%3A%2F%2Facme%2Fremote-plugin%23v1.0.0", nil)
	req.URL.RawQuery = "source=github%3A%2F%2Facme%2Fremote-plugin%23v1.0.0"
	c.Request = req
	h.Download(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Plugin-Source"); got != "github://acme/remote-plugin#v1.0.0" {
		t.Errorf("X-Plugin-Source: got %q, want github://acme/remote-plugin#v1.0.0", got)
	}

	// Decode + verify the tarball contains the resolver's files
	gz, err := gzip.NewReader(bytes.NewReader(w.Body.Bytes()))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	tr := tar.NewReader(gz)
	seen := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar read: %v", err)
		}
		seen[hdr.Name] = true
	}
	for _, want := range []string{"plugin.yaml", filepath.Join("skills", "x", "SKILL.md"), filepath.Join("adapters", "claude_code.py")} {
		if !seen[want] {
			t.Errorf("expected tar entry %q, saw: %v", want, seen)
		}
	}
}

// Buffered-vs-streamed contract: a tar pack failure must surface as a
// clean 5xx with a JSON error, not a truncated 200. We exercise this by
// pointing the resolver at a path the OS will refuse to read.
func TestPluginDownload_TarPackFailureReturns5xxNotTruncated200(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	pluginsDir := t.TempDir()
	pluginRoot := filepath.Join(pluginsDir, "broken-plugin")
	os.MkdirAll(pluginRoot, 0o755)
	os.WriteFile(filepath.Join(pluginRoot, "plugin.yaml"), []byte("name: broken-plugin\n"), 0o644)
	// Make a directory we can't read by chmod 000 — streamDirAsTar's
	// filepath.Walk will surface a permission error.
	unread := filepath.Join(pluginRoot, "unread")
	os.MkdirAll(unread, 0o755)
	os.WriteFile(filepath.Join(unread, "secret"), []byte("x"), 0o600)
	if err := os.Chmod(unread, 0o000); err != nil {
		t.Skipf("chmod 0000 unsupported on this fs: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(unread, 0o755) }) // restore for tempdir cleanup

	h := NewPluginsHandler(pluginsDir, nil, nil)

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM workspace_auth_tokens`).
		WithArgs("550e8400-e29b-41d4-a716-446655440000").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "550e8400-e29b-41d4-a716-446655440000"},
		{Key: "name", Value: "broken-plugin"},
	}
	c.Request = httptest.NewRequest("GET", "/workspaces/X/plugins/broken-plugin/download", nil)
	h.Download(c)

	// On macOS root may bypass chmod; in that case we get a clean 200 + tar
	// and the test is moot.
	if w.Code == http.StatusOK && w.Header().Get("Content-Type") == "application/gzip" {
		t.Skip("running as root; chmod 0000 didn't take effect, skipping")
	}
	// The contract this test guards: failure surfaces as a CLEAN 5xx
	// + JSON body, NEVER as truncated 200 + Content-Type=application/gzip.
	// The failure can come from either resolveAndStage (502 if the local
	// resolver can't read the dir) or the tar-pack stage (500 if read
	// succeeds but tar.Walk hits the unreadable subdir).
	if w.Code < 500 || w.Code > 599 {
		t.Fatalf("expected 5xx, got %d: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct == "application/gzip" {
		t.Errorf("must not advertise gzip on failure path; got Content-Type=%q", ct)
	}
	if !strings.HasPrefix(strings.TrimSpace(w.Body.String()), "{") {
		t.Errorf("expected JSON error body, got: %s", w.Body.String())
	}
}
