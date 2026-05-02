package handlers

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// ==================== validateRelPath ====================

func TestValidateRelPath_Valid(t *testing.T) {
	cases := []string{
		"config.yaml",
		"skills/my-skill/SKILL.md",
		"system-prompt.md",
		"a/b/c.txt",
	}
	for _, tc := range cases {
		if err := validateRelPath(tc); err != nil {
			t.Errorf("expected valid path %q, got error: %v", tc, err)
		}
	}
}

func TestValidateRelPath_Invalid(t *testing.T) {
	cases := []string{
		"../etc/passwd",
		"../../secrets",
		"/absolute/path",
	}
	for _, tc := range cases {
		if err := validateRelPath(tc); err == nil {
			t.Errorf("expected error for path %q, got nil", tc)
		}
	}
}

// ==================== GET /templates ====================

func TestTemplatesList_EmptyDir(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	tmpDir := t.TempDir()
	handler := NewTemplatesHandler(tmpDir, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/templates", nil)

	handler.List(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp []templateSummary
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if len(resp) != 0 {
		t.Errorf("expected empty list, got %d items", len(resp))
	}
}

func TestTemplatesList_WithTemplates(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	tmpDir := t.TempDir()

	// Create a template directory with config.yaml
	tmplDir := filepath.Join(tmpDir, "test-agent")
	os.MkdirAll(tmplDir, 0755)
	configYaml := `name: Test Agent
description: A test agent
tier: 2
model: anthropic:claude-sonnet-4-20250514
skills:
  - web-search
  - code-review
`
	os.WriteFile(filepath.Join(tmplDir, "config.yaml"), []byte(configYaml), 0644)

	// Create a non-directory file (should be skipped)
	os.WriteFile(filepath.Join(tmpDir, "README.md"), []byte("# readme"), 0644)

	// Create a directory without config.yaml (should be skipped)
	os.MkdirAll(filepath.Join(tmpDir, "no-config"), 0755)

	handler := NewTemplatesHandler(tmpDir, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/templates", nil)

	handler.List(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp []templateSummary
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if len(resp) != 1 {
		t.Fatalf("expected 1 template, got %d", len(resp))
	}
	if resp[0].ID != "test-agent" {
		t.Errorf("expected ID 'test-agent', got %q", resp[0].ID)
	}
	if resp[0].Name != "Test Agent" {
		t.Errorf("expected Name 'Test Agent', got %q", resp[0].Name)
	}
	if resp[0].Tier != 2 {
		t.Errorf("expected Tier 2, got %d", resp[0].Tier)
	}
	if resp[0].SkillCount != 2 {
		t.Errorf("expected SkillCount 2, got %d", resp[0].SkillCount)
	}
}

func TestTemplatesList_RuntimeAndModelsRegistry(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	tmpDir := t.TempDir()
	tmplDir := filepath.Join(tmpDir, "hermes")
	if err := os.MkdirAll(tmplDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	configYaml := `name: Hermes Agent
description: test
tier: 2
runtime: hermes
runtime_config:
  model: nous-hermes-3-70b
  models:
    - id: nous-hermes-3-70b
      name: Nous Hermes 3 70B
      required_env: [HERMES_API_KEY]
    - id: minimax/minimax-m2.7
      name: MiniMax M2.7 (via OpenRouter)
      required_env: [OPENROUTER_API_KEY]
skills: []
`
	if err := os.WriteFile(filepath.Join(tmplDir, "config.yaml"), []byte(configYaml), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	handler := NewTemplatesHandler(tmpDir, nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/templates", nil)
	handler.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp []templateSummary
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(resp) != 1 {
		t.Fatalf("expected 1 template, got %d", len(resp))
	}
	got := resp[0]
	if got.Runtime != "hermes" {
		t.Errorf("Runtime: want hermes, got %q", got.Runtime)
	}
	if got.Model != "nous-hermes-3-70b" {
		t.Errorf("Model: want nous-hermes-3-70b (from runtime_config.model), got %q", got.Model)
	}
	if len(got.Models) != 2 {
		t.Fatalf("Models: want 2, got %d", len(got.Models))
	}
	if got.Models[0].ID != "nous-hermes-3-70b" || got.Models[0].Name != "Nous Hermes 3 70B" {
		t.Errorf("Models[0] id/name mismatch: %+v", got.Models[0])
	}
	if len(got.Models[0].RequiredEnv) != 1 || got.Models[0].RequiredEnv[0] != "HERMES_API_KEY" {
		t.Errorf("Models[0] required_env: want [HERMES_API_KEY], got %+v", got.Models[0].RequiredEnv)
	}
	if got.Models[1].ID != "minimax/minimax-m2.7" {
		t.Errorf("Models[1].ID: got %q", got.Models[1].ID)
	}
	if len(got.Models[1].RequiredEnv) != 1 || got.Models[1].RequiredEnv[0] != "OPENROUTER_API_KEY" {
		t.Errorf("Models[1] required_env: want [OPENROUTER_API_KEY], got %+v", got.Models[1].RequiredEnv)
	}
}

// TestTemplatesList_SurfacesProviders pins the Option B PR-5 wiring:
// /templates must echo runtime_config.providers from the template's
// config.yaml into the JSON response. Canvas reads this list to
// populate the Provider override dropdown WITHOUT hardcoding any
// provider taxonomy on the frontend — that's the "data-driven from
// adapter" invariant.
//
// If a future yaml-tag rename or struct edit drops the field, every
// runtime would silently fall back to model-prefix derivation. For
// hermes specifically (default model has no clean prefix), that
// degrades the dropdown to empty and reintroduces the "No LLM
// provider configured" UX gap from 2026-05-01.
func TestTemplatesList_SurfacesProviders(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	tmpDir := t.TempDir()
	tmplDir := filepath.Join(tmpDir, "hermes-prov")
	if err := os.MkdirAll(tmplDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	configYaml := `name: Hermes
description: test
tier: 2
runtime: hermes
runtime_config:
  model: nousresearch/hermes-4-70b
  providers:
    - nous
    - openrouter
    - anthropic
skills: []
`
	if err := os.WriteFile(filepath.Join(tmplDir, "config.yaml"), []byte(configYaml), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	handler := NewTemplatesHandler(tmpDir, nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/templates", nil)
	handler.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp []templateSummary
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(resp) != 1 {
		t.Fatalf("expected 1 template, got %d", len(resp))
	}
	got := resp[0]
	want := []string{"nous", "openrouter", "anthropic"}
	if len(got.Providers) != len(want) {
		t.Fatalf("Providers: want %v, got %v", want, got.Providers)
	}
	for i, p := range want {
		if got.Providers[i] != p {
			t.Errorf("Providers[%d]: want %q, got %q", i, p, got.Providers[i])
		}
	}

	// Cross-check the JSON wire shape directly — canvas reads the field
	// as `providers` (lowercase) and a struct-tag rename here would
	// break consumers without surfacing in the typed assertions above.
	if !strings.Contains(w.Body.String(), `"providers":["nous","openrouter","anthropic"]`) {
		t.Errorf("response missing providers JSON field: %s", w.Body.String())
	}
}

// TestTemplatesList_SurfacesProviderRegistry pins the structured
// provider-registry shape that claude-code-default ships in its
// TOP-LEVEL `providers:` block. Canvas reads this to build a
// provider→model cascade (provider first, then a model dropdown
// filtered to that provider's prefixes/aliases) and a read-only
// required-env display sourced from auth_env + per-model required_env.
//
// If a future yaml-tag rename or struct edit drops the field, the
// canvas dropdown silently degrades to "20 unfiltered models in a
// datalist that hides everything not matching the typed substring" —
// which is the exact UX bug a user reported on hongming.moleculesai.app
// 2026-05-02 ("missing opus for claude code"). The fix landed here
// pivots away from datalist filtering toward explicit selects, but
// the wire shape carrying the registry is the load-bearing
// dependency. Pinning it stops a silent rename from re-introducing
// the bug under a different surface.
func TestTemplatesList_SurfacesProviderRegistry(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	tmpDir := t.TempDir()
	tmplDir := filepath.Join(tmpDir, "claude-code")
	if err := os.MkdirAll(tmplDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Mirrors the production claude-code-default schema: structured
	// providers at top level + flat models[] under runtime_config.
	configYaml := `name: Claude Code Agent
runtime: claude-code
providers:
  - name: anthropic-oauth
    auth_mode: oauth
    model_prefixes: []
    model_aliases: [sonnet, opus, haiku]
    base_url: null
    auth_env: [CLAUDE_CODE_OAUTH_TOKEN]
  - name: anthropic-api
    auth_mode: anthropic_api
    model_prefixes: [claude-]
    model_aliases: []
    auth_env: [ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN]
  - name: xiaomi-mimo
    auth_mode: third_party_anthropic_compat
    model_prefixes: [mimo-]
    base_url: https://api.xiaomimimo.com/anthropic
    auth_env: [ANTHROPIC_AUTH_TOKEN, ANTHROPIC_API_KEY]
runtime_config:
  model: sonnet
  models:
    - id: sonnet
      name: Claude Sonnet (OAuth / Claude Code subscription)
      required_env: [CLAUDE_CODE_OAUTH_TOKEN]
    - id: opus
      required_env: [CLAUDE_CODE_OAUTH_TOKEN]
skills: []
`
	if err := os.WriteFile(filepath.Join(tmplDir, "config.yaml"), []byte(configYaml), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	handler := NewTemplatesHandler(tmpDir, nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/templates", nil)
	handler.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp []templateSummary
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(resp) != 1 {
		t.Fatalf("expected 1 template, got %d", len(resp))
	}
	got := resp[0]

	if len(got.ProviderRegistry) != 3 {
		t.Fatalf("ProviderRegistry: want 3 entries, got %d (%+v)", len(got.ProviderRegistry), got.ProviderRegistry)
	}

	// First entry: OAuth path with model aliases (sonnet/opus/haiku).
	oauth := got.ProviderRegistry[0]
	if oauth.Name != "anthropic-oauth" {
		t.Errorf("Provider[0].Name: got %q", oauth.Name)
	}
	if oauth.AuthMode != "oauth" {
		t.Errorf("Provider[0].AuthMode: got %q", oauth.AuthMode)
	}
	if len(oauth.ModelAliases) != 3 || oauth.ModelAliases[1] != "opus" {
		t.Errorf("Provider[0].ModelAliases: got %+v", oauth.ModelAliases)
	}
	if len(oauth.AuthEnv) != 1 || oauth.AuthEnv[0] != "CLAUDE_CODE_OAUTH_TOKEN" {
		t.Errorf("Provider[0].AuthEnv: got %+v", oauth.AuthEnv)
	}
	if oauth.BaseURL != "" {
		t.Errorf("Provider[0].BaseURL: want empty (yaml null), got %q", oauth.BaseURL)
	}

	// Second entry: API key with model prefixes.
	api := got.ProviderRegistry[1]
	if api.Name != "anthropic-api" {
		t.Errorf("Provider[1].Name: got %q", api.Name)
	}
	if len(api.ModelPrefixes) != 1 || api.ModelPrefixes[0] != "claude-" {
		t.Errorf("Provider[1].ModelPrefixes: got %+v", api.ModelPrefixes)
	}

	// Third entry: third-party with non-null base_url.
	mimo := got.ProviderRegistry[2]
	if mimo.BaseURL != "https://api.xiaomimimo.com/anthropic" {
		t.Errorf("Provider[2].BaseURL: got %q", mimo.BaseURL)
	}

	// Wire-shape check — canvas reads `provider_registry` (snake case).
	if !strings.Contains(w.Body.String(), `"provider_registry":[`) {
		t.Errorf("response missing provider_registry JSON field: %s", w.Body.String())
	}
}

// TestTemplatesList_RealClaudeCodeDefaultSchema is the live-shape
// integration test: loads the actual workspace-configs-templates/
// claude-code-default/config.yaml from the repo root, runs it
// through the List handler, and asserts the response shape matches
// what the canvas expects to consume. If the production config.yaml
// drifts away from the shape canvas reads (e.g. providers becomes
// `provider_set:` or model_aliases becomes `aliases:`), this test
// fails the build at PR time instead of after deploy.
//
// This is the "live-test the actual code path before shipping a
// hypothesis-driven fix" pattern from feedback memory 2026-05-02:
// don't rely on synthetic fixtures alone when the production schema
// is right there in the repo.
func TestTemplatesList_RealClaudeCodeDefaultSchema(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	// Walk up from the test file's package dir to the repo root, then
	// down into workspace-configs-templates/. Skip the test if we
	// can't find it — the workspace-configs-templates dir is part of
	// the monorepo, not the workspace-server module, so a partial
	// checkout could omit it (CI builds the whole monorepo so it'll
	// always be present there).
	repoRoot := ""
	cwd, err := os.Getwd()
	if err != nil {
		t.Skipf("getwd: %v", err)
	}
	for dir := cwd; dir != "/" && dir != "."; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "workspace-configs-templates")); err == nil {
			repoRoot = dir
			break
		}
	}
	if repoRoot == "" {
		t.Skip("workspace-configs-templates/ not found — partial checkout?")
	}

	configsDir := filepath.Join(repoRoot, "workspace-configs-templates")
	handler := NewTemplatesHandler(configsDir, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/templates", nil)
	handler.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp []templateSummary
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}

	// Locate claude-code-default in the response.
	var ccd *templateSummary
	for i := range resp {
		if resp[i].ID == "claude-code-default" {
			ccd = &resp[i]
			break
		}
	}
	if ccd == nil {
		t.Fatalf("claude-code-default not found in /templates response (%d templates returned)", len(resp))
	}

	// The shape-contract assertions canvas depends on. Each one pins
	// a specific UX point the user reported on 2026-05-02.

	// (4) opus must be discoverable from the response. The model id
	// is the literal "opus" alias, and a model.required_env declares
	// what env var the workspace needs.
	hasOpus := false
	for _, m := range ccd.Models {
		if m.ID == "opus" {
			hasOpus = true
			if len(m.RequiredEnv) == 0 {
				t.Errorf("opus has no required_env in template")
			}
		}
	}
	if !hasOpus {
		t.Errorf("opus alias missing from claude-code-default models[]: got %d models", len(ccd.Models))
	}

	// (2) Provider registry must surface so the canvas can render
	// a Provider→Model cascade. The first entry must be the OAuth
	// path with `sonnet`/`opus`/`haiku` aliases.
	if len(ccd.ProviderRegistry) == 0 {
		t.Fatalf("provider_registry empty for claude-code-default — canvas cascade UI won't render")
	}
	oauth := ccd.ProviderRegistry[0]
	if oauth.Name != "anthropic-oauth" {
		t.Errorf("provider_registry[0].name = %q, want anthropic-oauth", oauth.Name)
	}
	hasOpusAlias := false
	for _, a := range oauth.ModelAliases {
		if a == "opus" {
			hasOpusAlias = true
		}
	}
	if !hasOpusAlias {
		t.Errorf("anthropic-oauth.model_aliases doesn't include opus: %+v", oauth.ModelAliases)
	}

	// (3) auth_env must be populated so the canvas can render the
	// read-only required-env display. Without this the cascade
	// degrades back to the editable TagList.
	if len(oauth.AuthEnv) == 0 {
		t.Errorf("anthropic-oauth.auth_env empty — required-env display will be empty")
	}

	// Cross-check: the JSON wire shape canvas actually parses.
	body := w.Body.String()
	if !strings.Contains(body, `"provider_registry"`) {
		t.Errorf("response missing provider_registry JSON field")
	}
	if !strings.Contains(body, `"auth_env"`) {
		t.Errorf("response missing auth_env in registry — canvas can't compute required env")
	}
	if !strings.Contains(body, `"model_aliases"`) {
		t.Errorf("response missing model_aliases in registry — canvas can't filter models by provider")
	}
}

// TestTemplatesList_OmitsProviderRegistryWhenAbsent — a template that
// hasn't migrated to the structured `providers:` block must NOT emit
// `provider_registry: null` (canvas's array-typed parser would treat
// that as a load failure). omitempty on the struct slice elides the
// field entirely.
func TestTemplatesList_OmitsProviderRegistryWhenAbsent(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	tmpDir := t.TempDir()
	tmplDir := filepath.Join(tmpDir, "no-registry")
	if err := os.MkdirAll(tmplDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	configYaml := `name: Legacy
runtime: hermes
runtime_config:
  model: anthropic:claude-opus-4-7
  providers: [nous, openrouter]
skills: []
`
	if err := os.WriteFile(filepath.Join(tmplDir, "config.yaml"), []byte(configYaml), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	handler := NewTemplatesHandler(tmpDir, nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/templates", nil)
	handler.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), `"provider_registry":`) {
		t.Errorf("response should omit provider_registry when template has none, got: %s", w.Body.String())
	}
	// Sanity: the legacy flat-string Providers field is still surfaced
	// for hermes-style templates. Both shapes coexist intentionally.
	if !strings.Contains(w.Body.String(), `"providers":["nous","openrouter"]`) {
		t.Errorf("response should still surface flat providers list: %s", w.Body.String())
	}
}

// TestTemplatesList_BothProviderShapesCoexist — claude-code-default
// uses the structured top-level providers; hermes-shape templates may
// keep the flat runtime_config.providers list. The /templates payload
// must surface both fields independently so canvas can pick the
// richer one when present and fall back to the flat list otherwise.
func TestTemplatesList_BothProviderShapesCoexist(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	tmpDir := t.TempDir()
	tmplDir := filepath.Join(tmpDir, "both")
	if err := os.MkdirAll(tmplDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	configYaml := `name: Mixed
runtime: experimental
providers:
  - name: vendor-a
    auth_env: [VENDOR_A_KEY]
runtime_config:
  model: x
  providers: [legacy-flat]
skills: []
`
	if err := os.WriteFile(filepath.Join(tmplDir, "config.yaml"), []byte(configYaml), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	handler := NewTemplatesHandler(tmpDir, nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/templates", nil)
	handler.List(c)

	var resp []templateSummary
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(resp) != 1 {
		t.Fatalf("expected 1 template, got %d", len(resp))
	}
	got := resp[0]
	if len(got.Providers) != 1 || got.Providers[0] != "legacy-flat" {
		t.Errorf("flat Providers preserved: got %+v", got.Providers)
	}
	if len(got.ProviderRegistry) != 1 || got.ProviderRegistry[0].Name != "vendor-a" {
		t.Errorf("structured ProviderRegistry preserved: got %+v", got.ProviderRegistry)
	}
}

// TestTemplatesList_DropsRegistryEntriesWithoutName — a malformed
// providers entry missing `name:` would otherwise render as a blank
// dropdown option on canvas. Drop those defensively at parse time so
// the wire shape never carries them.
func TestTemplatesList_DropsRegistryEntriesWithoutName(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	tmpDir := t.TempDir()
	tmplDir := filepath.Join(tmpDir, "malformed")
	if err := os.MkdirAll(tmplDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	configYaml := `name: Test
runtime: claude-code
providers:
  - name: real-provider
    auth_env: [TOKEN]
  - auth_env: [SHOULD_BE_DROPPED]
  - name: ""
    auth_env: [ALSO_DROPPED]
runtime_config:
  model: x
skills: []
`
	if err := os.WriteFile(filepath.Join(tmplDir, "config.yaml"), []byte(configYaml), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	handler := NewTemplatesHandler(tmpDir, nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/templates", nil)
	handler.List(c)

	var resp []templateSummary
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(resp) != 1 {
		t.Fatalf("expected 1 template, got %d", len(resp))
	}
	got := resp[0]
	if len(got.ProviderRegistry) != 1 {
		t.Fatalf("expected 1 valid entry (others dropped), got %d: %+v", len(got.ProviderRegistry), got.ProviderRegistry)
	}
	if got.ProviderRegistry[0].Name != "real-provider" {
		t.Errorf("wrong entry kept: %+v", got.ProviderRegistry[0])
	}
}

// TestTemplatesList_OmitsProvidersWhenAbsent pins the omitempty
// behavior — older templates that haven't migrated to
// runtime_config.providers yet must NOT emit `providers: null` (which
// would break canvas's array-typed parser). A template that simply
// omits the field stays absent in the response and canvas falls back
// to deriving suggestions from model-slug prefixes.
func TestTemplatesList_OmitsProvidersWhenAbsent(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	tmpDir := t.TempDir()
	tmplDir := filepath.Join(tmpDir, "no-prov")
	if err := os.MkdirAll(tmplDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	configYaml := `name: Legacy
runtime: langgraph
runtime_config:
  model: anthropic:claude-opus-4-7
skills: []
`
	if err := os.WriteFile(filepath.Join(tmplDir, "config.yaml"), []byte(configYaml), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	handler := NewTemplatesHandler(tmpDir, nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/templates", nil)
	handler.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), `"providers":`) {
		t.Errorf("response should omit providers when template has none, got: %s", w.Body.String())
	}
}

func TestTemplatesList_LegacyTopLevelModel(t *testing.T) {
	// Older templates (pre-runtime_config) declared `model:` at the top level.
	// The /templates endpoint should keep surfacing those for backward compat.
	setupTestDB(t)
	setupTestRedis(t)

	tmpDir := t.TempDir()
	tmplDir := filepath.Join(tmpDir, "legacy")
	if err := os.MkdirAll(tmplDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	configYaml := `name: Legacy Agent
tier: 1
model: anthropic:claude-sonnet-4-6
skills: []
`
	if err := os.WriteFile(filepath.Join(tmplDir, "config.yaml"), []byte(configYaml), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	handler := NewTemplatesHandler(tmpDir, nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/templates", nil)
	handler.List(c)

	var resp []templateSummary
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(resp) != 1 || resp[0].Model != "anthropic:claude-sonnet-4-6" {
		t.Errorf("legacy top-level model not surfaced: %+v", resp)
	}
	if resp[0].Runtime != "" {
		t.Errorf("Runtime should be empty for legacy template, got %q", resp[0].Runtime)
	}
	if len(resp[0].Models) != 0 {
		t.Errorf("Models should be empty for legacy template, got %+v", resp[0].Models)
	}
}

func TestTemplatesList_NonexistentDir(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	handler := NewTemplatesHandler("/nonexistent/path/to/templates", nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/templates", nil)

	handler.List(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp []templateSummary
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp) != 0 {
		t.Errorf("expected empty list, got %d items", len(resp))
	}
}

// ==================== GET /workspaces/:id/files ====================

func TestListFiles_InvalidRoot(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	handler := NewTemplatesHandler(t.TempDir(), nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-1/files?root=/etc", nil)
	// Need to set query params
	c.Request.URL.RawQuery = "root=/etc"

	handler.ListFiles(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}

	// Verify no DB call was made (early return before DB query)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestListFiles_WorkspaceNotFound(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	handler := NewTemplatesHandler(t.TempDir(), nil)

	mock.ExpectQuery("SELECT name FROM workspaces WHERE id =").
		WithArgs("ws-nonexist").
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-nonexist"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-nonexist/files", nil)

	handler.ListFiles(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestListFiles_FallbackToHost_NoTemplate(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	tmpDir := t.TempDir()
	handler := NewTemplatesHandler(tmpDir, nil) // nil docker = no container

	mock.ExpectQuery("SELECT name FROM workspaces WHERE id =").
		WithArgs("ws-fallback").
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("Unknown Agent"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-fallback"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-fallback/files", nil)

	handler.ListFiles(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Should return empty list
	var resp []interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp) != 0 {
		t.Errorf("expected empty file list, got %d items", len(resp))
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestListFiles_FallbackToHost_WithTemplate(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	tmpDir := t.TempDir()
	// Create a template matching the workspace name
	tmplDir := filepath.Join(tmpDir, "test-agent")
	os.MkdirAll(tmplDir, 0755)
	os.WriteFile(filepath.Join(tmplDir, "config.yaml"), []byte("name: Test Agent\n"), 0644)
	os.WriteFile(filepath.Join(tmplDir, "system-prompt.md"), []byte("# prompt"), 0644)

	handler := NewTemplatesHandler(tmpDir, nil)

	mock.ExpectQuery("SELECT name FROM workspaces WHERE id =").
		WithArgs("ws-tmpl").
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("Test Agent"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-tmpl"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-tmpl/files", nil)

	handler.ListFiles(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp []map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp) < 2 {
		t.Errorf("expected at least 2 files, got %d", len(resp))
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== GET /workspaces/:id/files/*path ====================

func TestReadFile_PathTraversal(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	handler := NewTemplatesHandler(t.TempDir(), nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-1"},
		{Key: "path", Value: "/../../../etc/passwd"},
	}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-1/files/../../../etc/passwd", nil)

	handler.ReadFile(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestReadFile_InvalidRoot(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	handler := NewTemplatesHandler(t.TempDir(), nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-1"},
		{Key: "path", Value: "/config.yaml"},
	}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-1/files/config.yaml?root=/tmp", nil)
	c.Request.URL.RawQuery = "root=/tmp"

	handler.ReadFile(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestReadFile_WorkspaceNotFound(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	handler := NewTemplatesHandler(t.TempDir(), nil)

	mock.ExpectQuery("SELECT name FROM workspaces WHERE id =").
		WithArgs("ws-nf").
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-nf"},
		{Key: "path", Value: "/config.yaml"},
	}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-nf/files/config.yaml", nil)

	handler.ReadFile(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestReadFile_FallbackToHost_Success(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	tmpDir := t.TempDir()
	tmplDir := filepath.Join(tmpDir, "reader-agent")
	os.MkdirAll(tmplDir, 0755)
	os.WriteFile(filepath.Join(tmplDir, "config.yaml"), []byte("name: Reader Agent\ntier: 1\n"), 0644)

	handler := NewTemplatesHandler(tmpDir, nil)

	mock.ExpectQuery("SELECT name FROM workspaces WHERE id =").
		WithArgs("ws-read").
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("Reader Agent"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-read"},
		{Key: "path", Value: "/config.yaml"},
	}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-read/files/config.yaml", nil)

	handler.ReadFile(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["path"] != "config.yaml" {
		t.Errorf("expected path 'config.yaml', got %v", resp["path"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestReadFile_FallbackToHost_NotFound(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	tmpDir := t.TempDir()
	handler := NewTemplatesHandler(tmpDir, nil)

	mock.ExpectQuery("SELECT name FROM workspaces WHERE id =").
		WithArgs("ws-nofile").
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("No File Agent"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-nofile"},
		{Key: "path", Value: "/nonexistent.txt"},
	}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-nofile/files/nonexistent.txt", nil)

	handler.ReadFile(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== PUT /workspaces/:id/files/*path ====================

func TestWriteFile_PathTraversal(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	handler := NewTemplatesHandler(t.TempDir(), nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-1"},
		{Key: "path", Value: "/../../../etc/shadow"},
	}
	body := `{"content": "malicious"}`
	c.Request = httptest.NewRequest("PUT", "/workspaces/ws-1/files/../../../etc/shadow",
		strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.WriteFile(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestWriteFile_InvalidBody(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	handler := NewTemplatesHandler(t.TempDir(), nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-1"},
		{Key: "path", Value: "/config.yaml"},
	}
	c.Request = httptest.NewRequest("PUT", "/workspaces/ws-1/files/config.yaml",
		strings.NewReader("not json"))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.WriteFile(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestWriteFile_WorkspaceNotFound(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	handler := NewTemplatesHandler(t.TempDir(), nil)

	mock.ExpectQuery(`SELECT name, COALESCE\(instance_id, ''\), COALESCE\(runtime, ''\) FROM workspaces WHERE id =`).
		WithArgs("ws-wf-nf").
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-wf-nf"},
		{Key: "path", Value: "/config.yaml"},
	}
	body := `{"content": "name: test"}`
	c.Request = httptest.NewRequest("PUT", "/workspaces/ws-wf-nf/files/config.yaml",
		strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.WriteFile(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== DELETE /workspaces/:id/files/*path ====================

func TestDeleteFile_PathTraversal(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	handler := NewTemplatesHandler(t.TempDir(), nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-1"},
		{Key: "path", Value: "/../../../etc/passwd"},
	}
	c.Request = httptest.NewRequest("DELETE", "/workspaces/ws-1/files/../../../etc/passwd", nil)

	handler.DeleteFile(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDeleteFile_WorkspaceNotFound(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	handler := NewTemplatesHandler(t.TempDir(), nil)

	mock.ExpectQuery("SELECT name FROM workspaces WHERE id =").
		WithArgs("ws-del-nf").
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-del-nf"},
		{Key: "path", Value: "old-file.txt"},
	}
	c.Request = httptest.NewRequest("DELETE", "/workspaces/ws-del-nf/files/old-file.txt", nil)

	handler.DeleteFile(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== GET /workspaces/:id/shared-context ====================

func TestSharedContext_WorkspaceNotFound(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	handler := NewTemplatesHandler(t.TempDir(), nil)

	mock.ExpectQuery("SELECT name FROM workspaces WHERE id =").
		WithArgs("ws-sc-nf").
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-sc-nf"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-sc-nf/shared-context", nil)

	handler.SharedContext(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestSharedContext_NoTemplate(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	tmpDir := t.TempDir()
	handler := NewTemplatesHandler(tmpDir, nil) // no docker

	mock.ExpectQuery("SELECT name FROM workspaces WHERE id =").
		WithArgs("ws-sc-nt").
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("Unknown Agent"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-sc-nt"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-sc-nt/shared-context", nil)

	handler.SharedContext(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Should return empty array
	var resp []interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp) != 0 {
		t.Errorf("expected empty list, got %d items", len(resp))
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestSharedContext_WithFiles(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	tmpDir := t.TempDir()
	tmplDir := filepath.Join(tmpDir, "ctx-agent")
	os.MkdirAll(tmplDir, 0755)
	os.WriteFile(filepath.Join(tmplDir, "config.yaml"), []byte("name: Ctx Agent\nshared_context:\n  - rules.md\n  - style.md\n"), 0644)
	os.WriteFile(filepath.Join(tmplDir, "rules.md"), []byte("# Rules\nBe nice"), 0644)
	os.WriteFile(filepath.Join(tmplDir, "style.md"), []byte("# Style\nBe clear"), 0644)

	handler := NewTemplatesHandler(tmpDir, nil)

	mock.ExpectQuery("SELECT name FROM workspaces WHERE id =").
		WithArgs("ws-sc-ok").
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("Ctx Agent"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-sc-ok"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-sc-ok/shared-context", nil)

	handler.SharedContext(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp []map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp) != 2 {
		t.Fatalf("expected 2 context files, got %d", len(resp))
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== resolveTemplateDir ====================

func TestResolveTemplateDir_ByNormalizedName(t *testing.T) {
	tmpDir := t.TempDir()
	tmplDir := filepath.Join(tmpDir, "my-agent")
	os.MkdirAll(tmplDir, 0755)

	handler := NewTemplatesHandler(tmpDir, nil)
	result := handler.resolveTemplateDir("My Agent")

	if result != tmplDir {
		t.Errorf("expected %q, got %q", tmplDir, result)
	}
}

func TestResolveTemplateDir_NotFound(t *testing.T) {
	tmpDir := t.TempDir()
	handler := NewTemplatesHandler(tmpDir, nil)
	result := handler.resolveTemplateDir("Nonexistent Agent")

	if result != "" {
		t.Errorf("expected empty string, got %q", result)
	}
}

// ==================== CWE-78 hardening regression (issue #2011) ====================
// These tests lock in the defence-in-depth guards for DeleteFile and SharedContext.
// The primary guard is validateRelPath (fires before any exec/file-read path);
// the exec-form path construction (filepath.Join / separate args) is defence-in-depth.

// TestCWE78_DeleteFile_TraversalVariants asserts that a range of traversal patterns
// are all rejected with 400 before any Docker exec or ephemeral container operation.
// This covers the validateRelPath guard that sits at the entry of DeleteFile.
func TestCWE78_DeleteFile_TraversalVariants(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{"double dotdot", "/../../../etc/passwd"},
		{"leading dotdot", "/../secret"},
		{"mid-path traversal", "/valid/../../../etc/shadow"},
		{"absolute path", "/etc/passwd"},
		{"encoded dotdot raw", "..%2F..%2Fetc%2Fpasswd"},
		{"triple dotdot", "/../../.."},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setupTestDB(t)
			setupTestRedis(t)

			handler := NewTemplatesHandler(t.TempDir(), nil)

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Params = gin.Params{
				{Key: "id", Value: "ws-cwe78"},
				{Key: "path", Value: tc.path},
			}
			c.Request = httptest.NewRequest("DELETE", "/workspaces/ws-cwe78/files"+tc.path, nil)

			handler.DeleteFile(c)

			if w.Code != http.StatusBadRequest {
				t.Errorf("path %q: expected 400 (traversal blocked), got %d: %s",
					tc.path, w.Code, w.Body.String())
			}
		})
	}
}

// TestCWE78_SharedContext_SkipsTraversalPaths asserts that when a workspace's
// config.yaml lists traversal paths in shared_context, SharedContext skips them
// via validateRelPath rather than passing them to exec or os.ReadFile.
// Uses the filesystem fallback path (no docker client) so no container mock needed.
func TestCWE78_SharedContext_SkipsTraversalPaths(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	tmpDir := t.TempDir()
	// Create a template directory that SharedContext will resolve for "Cwe Agent".
	tmplDir := filepath.Join(tmpDir, "cwe-agent")
	os.MkdirAll(tmplDir, 0755)
	// config.yaml with a mix of safe and traversal-attack paths.
	configYAML := "name: Cwe Agent\nshared_context:\n  - safe-file.md\n  - ../../etc/passwd\n  - ../shadow\n  - another-safe.md\n"
	os.WriteFile(filepath.Join(tmplDir, "config.yaml"), []byte(configYAML), 0644)
	// Only write the safe files — traversal paths must not be reachable.
	os.WriteFile(filepath.Join(tmplDir, "safe-file.md"), []byte("# safe"), 0644)
	os.WriteFile(filepath.Join(tmplDir, "another-safe.md"), []byte("# also safe"), 0644)

	mock.ExpectQuery("SELECT name FROM workspaces WHERE id =").
		WithArgs("ws-cwe78-sc").
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("Cwe Agent"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-cwe78-sc"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-cwe78-sc/shared-context", nil)

	handler := NewTemplatesHandler(tmpDir, nil) // nil docker → filesystem fallback
	handler.SharedContext(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var files []struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &files); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Only the two safe files must appear; traversal paths must be absent.
	if len(files) != 2 {
		t.Errorf("expected 2 safe files, got %d: %v", len(files), files)
	}
	for _, f := range files {
		if strings.Contains(f.Path, "..") || strings.Contains(f.Path, "etc") || strings.Contains(f.Path, "shadow") {
			t.Errorf("traversal path %q must not appear in shared-context response", f.Path)
		}
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}
