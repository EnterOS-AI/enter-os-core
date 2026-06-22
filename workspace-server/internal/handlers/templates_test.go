package handlers

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/providers"
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
	handler := NewTemplatesHandler(tmpDir, nil, nil)

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
runtime: claude-code
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

	handler := NewTemplatesHandler(tmpDir, nil, nil)

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

func TestTemplatesList_CacheOverridesBakedTemplate(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	bakedDir := t.TempDir()
	cacheDir := t.TempDir()

	mustWriteTemplate := func(root, id, body string) {
		t.Helper()
		dir := filepath.Join(root, id)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0644); err != nil {
			t.Fatalf("write config: %v", err)
		}
	}

	mustWriteTemplate(bakedDir, "seo-agent", `name: SEO Agent
description: stale
tier: 4
runtime: claude-code
model: old
runtime_config:
  recommended_env: [TELEGRAM_BOT_TOKEN]
skills: []
`)
	mustWriteTemplate(cacheDir, "seo-agent", `name: SEO Agent
description: fresh
tier: 4
runtime: claude-code
model: moonshot/kimi-k2.6
runtime_config:
  required_env: [TENANT_NAME]
  recommended_env: [GOOGLE_GSC_SITE]
skills: []
`)

	handler := NewTemplatesHandler(bakedDir, nil, nil).WithCacheDir(cacheDir)
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
	if resp[0].Description != "fresh" {
		t.Fatalf("cache template should override baked copy, got description %q", resp[0].Description)
	}
	if !reflect.DeepEqual(resp[0].RequiredEnv, []string{"TENANT_NAME"}) {
		t.Fatalf("RequiredEnv = %+v", resp[0].RequiredEnv)
	}
	if reflect.DeepEqual(resp[0].RecommendedEnv, []string{"TELEGRAM_BOT_TOKEN"}) {
		t.Fatalf("stale baked recommended_env leaked through: %+v", resp[0].RecommendedEnv)
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
  recommended_env: [GOOGLE_GSC_SITE, GOOGLE_GA4_PROPERTY_ID]
  models:
    - id: nous-hermes-3-70b
      name: Nous Hermes 3 70B
      required_env: [HERMES_API_KEY]
    - id: minimax/minimax-m2.7
      name: MiniMax M2.7 (via OpenRouter)
      provider: platform
      required_env: [OPENROUTER_API_KEY]
skills: []
`
	if err := os.WriteFile(filepath.Join(tmplDir, "config.yaml"), []byte(configYaml), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	handler := NewTemplatesHandler(tmpDir, nil, nil)
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
	if got.Models[1].Provider != "platform" {
		t.Errorf("Models[1].Provider: got %q", got.Models[1].Provider)
	}
	if len(got.Models[1].RequiredEnv) != 1 || got.Models[1].RequiredEnv[0] != "OPENROUTER_API_KEY" {
		t.Errorf("Models[1] required_env: want [OPENROUTER_API_KEY], got %+v", got.Models[1].RequiredEnv)
	}
	if len(got.RecommendedEnv) != 2 ||
		got.RecommendedEnv[0] != "GOOGLE_GSC_SITE" ||
		got.RecommendedEnv[1] != "GOOGLE_GA4_PROPERTY_ID" {
		t.Errorf("RecommendedEnv: want [GOOGLE_GSC_SITE GOOGLE_GA4_PROPERTY_ID], got %+v", got.RecommendedEnv)
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

	handler := NewTemplatesHandler(tmpDir, nil, nil)
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

// TestTemplatesList_SurfacesProviderRegistry pins the #235 enrichment:
// /templates must echo the template's TOP-LEVEL `providers:` block as a
// structured array of providerRegistryEntry, separate from the
// runtime_config.providers slug list above. Each entry carries auth_env
// + model_prefixes + base_url so the canvas can stop inferring vendor
// taxonomy from per-model required_env tuples.
//
// Use a claude-code-shaped fixture (the only template in production
// that ships the registry today, modulo the per-vendor work in PR #33).
// Order MUST be preserved — the canvas surfaces the dropdown in
// declaration order so operators can put their preferred provider first.
func TestTemplatesList_SurfacesProviderRegistry(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	tmpDir := t.TempDir()
	tmplDir := filepath.Join(tmpDir, "claude-code")
	if err := os.MkdirAll(tmplDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	configYaml := `name: Claude Code
runtime: claude-code
providers:
  - name: anthropic-oauth
    auth_mode: oauth
    model_prefixes: []
    model_aliases: [sonnet, opus, haiku]
    base_url: null
    auth_env: [CLAUDE_CODE_OAUTH_TOKEN]
  - name: minimax
    auth_mode: third_party_anthropic_compat
    model_prefixes: [minimax-]
    model_aliases: []
    base_url: https://api.minimax.io/anthropic
    auth_env: [MINIMAX_API_KEY, ANTHROPIC_AUTH_TOKEN]
runtime_config:
  model: claude-sonnet-4-6
skills: []
`
	if err := os.WriteFile(filepath.Join(tmplDir, "config.yaml"), []byte(configYaml), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	handler := NewTemplatesHandler(tmpDir, nil, nil)
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
	got := resp[0].ProviderRegistry
	if len(got) != 2 {
		t.Fatalf("ProviderRegistry: want 2 entries, got %d (%+v)", len(got), got)
	}
	// Order preservation
	if got[0].Name != "anthropic-oauth" {
		t.Errorf("ProviderRegistry[0].Name: want %q, got %q", "anthropic-oauth", got[0].Name)
	}
	if got[1].Name != "minimax" {
		t.Errorf("ProviderRegistry[1].Name: want %q, got %q", "minimax", got[1].Name)
	}
	// Field plumbing on the first (oauth) entry
	if got[0].AuthMode != "oauth" {
		t.Errorf("ProviderRegistry[0].AuthMode: want %q, got %q", "oauth", got[0].AuthMode)
	}
	if !reflect.DeepEqual(got[0].ModelAliases, []string{"sonnet", "opus", "haiku"}) {
		t.Errorf("ProviderRegistry[0].ModelAliases: want sonnet/opus/haiku, got %v", got[0].ModelAliases)
	}
	if !reflect.DeepEqual(got[0].AuthEnv, []string{"CLAUDE_CODE_OAUTH_TOKEN"}) {
		t.Errorf("ProviderRegistry[0].AuthEnv: want [CLAUDE_CODE_OAUTH_TOKEN], got %v", got[0].AuthEnv)
	}
	// `base_url: null` in YAML → empty string for a plain `string` field
	// (yaml.v3 default). Pinning this so a future change to `*string`
	// (which would decode to nil instead and surface differently in JSON)
	// is caught loudly. The canvas treats "" the same as "no base_url"
	// (uses provider defaults); a `*string` change would emit a JSON
	// `null` and break that branch.
	if got[0].BaseURL != "" {
		t.Errorf("ProviderRegistry[0].BaseURL: want empty string for `null` YAML, got %q", got[0].BaseURL)
	}
	// Field plumbing on the second (third-party) entry — base_url is the
	// distinguishing signal for compat providers; canvas uses it to render
	// the "via Anthropic-compat endpoint" badge.
	if got[1].BaseURL != "https://api.minimax.io/anthropic" {
		t.Errorf("ProviderRegistry[1].BaseURL: want minimax url, got %q", got[1].BaseURL)
	}
	if !reflect.DeepEqual(got[1].ModelPrefixes, []string{"minimax-"}) {
		t.Errorf("ProviderRegistry[1].ModelPrefixes: want [minimax-], got %v", got[1].ModelPrefixes)
	}
	if !reflect.DeepEqual(got[1].AuthEnv, []string{"MINIMAX_API_KEY", "ANTHROPIC_AUTH_TOKEN"}) {
		t.Errorf("ProviderRegistry[1].AuthEnv: want [MINIMAX_API_KEY, ANTHROPIC_AUTH_TOKEN], got %v", got[1].AuthEnv)
	}

	// Wire-shape gate — canvas reads this as `provider_registry` (snake_case).
	// A struct-tag rename would silently drop it from consumers; the typed
	// assertions above can't catch a tag-only change because they decode via
	// the same struct.
	if !strings.Contains(w.Body.String(), `"provider_registry":[{"name":"anthropic-oauth"`) {
		t.Errorf("response missing provider_registry JSON field with expected first entry: %s", w.Body.String())
	}
}

// TestTemplatesList_OmitsProviderRegistryWhenAbsent pins the omitempty
// behavior for the new field — templates without a top-level
// `providers:` block (hermes today, claude-code, etc.) must NOT emit
// `provider_registry: null`, which would break canvas's array-typed
// parser (Array.isArray check returns false for null).
// TestTemplatesList_BothProviderShapesCoexist pins the real production
// shape: claude-code-default ships BOTH a top-level `providers:` block
// (structured registry) AND a `runtime_config.providers:` slug list
// (canvas Config tab dropdown). Both must surface independently —
// `provider_registry` on one field, `providers` on the other — with no
// cross-talk or struct-tag collision.
//
// PR #2543 introduced the structured field; reviewer noted the two
// fields' coexistence was only tested in isolation. This locks it in
// against the production layout so a future struct refactor that
// accidentally aliases the two YAML keys (or, e.g., moves the registry
// under `runtime_config:`) would fail loudly.
func TestTemplatesList_BothProviderShapesCoexist(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	tmpDir := t.TempDir()
	tmplDir := filepath.Join(tmpDir, "claude-code-default")
	if err := os.MkdirAll(tmplDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Mirrors workspace-configs-templates/claude-code-default/config.yaml:
	// top-level structured `providers:` (auth_mode + auth_env) + nested
	// `runtime_config.providers:` slug list.
	configYaml := `name: Claude Code
runtime: claude-code
providers:
  - name: anthropic-oauth
    auth_mode: oauth
    auth_env: [CLAUDE_CODE_OAUTH_TOKEN]
  - name: minimax
    auth_mode: third_party_anthropic_compat
    base_url: https://api.minimax.io/anthropic
    auth_env: [MINIMAX_API_KEY]
runtime_config:
  model: claude-sonnet-4-6
  providers:
    - anthropic-oauth
    - anthropic-api
    - minimax
skills: []
`
	if err := os.WriteFile(filepath.Join(tmplDir, "config.yaml"), []byte(configYaml), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	handler := NewTemplatesHandler(tmpDir, nil, nil)
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

	// Slug list (runtime_config.providers) — independent of structured
	// registry. Order preserved.
	wantSlugs := []string{"anthropic-oauth", "anthropic-api", "minimax"}
	if !reflect.DeepEqual(got.Providers, wantSlugs) {
		t.Errorf("Providers (slug list): want %v, got %v", wantSlugs, got.Providers)
	}

	// Structured registry (top-level providers) — fully populated, also
	// in declaration order. Crucially, the slug list above does NOT
	// bleed into here even though one slug (`anthropic-api`) is NOT in
	// the structured registry — they really are two distinct YAML paths.
	if len(got.ProviderRegistry) != 2 {
		t.Fatalf("ProviderRegistry: want 2 entries (top-level only), got %d: %+v", len(got.ProviderRegistry), got.ProviderRegistry)
	}
	if got.ProviderRegistry[0].Name != "anthropic-oauth" || got.ProviderRegistry[0].AuthMode != "oauth" {
		t.Errorf("ProviderRegistry[0]: want anthropic-oauth/oauth, got %+v", got.ProviderRegistry[0])
	}
	if got.ProviderRegistry[1].Name != "minimax" || got.ProviderRegistry[1].BaseURL != "https://api.minimax.io/anthropic" {
		t.Errorf("ProviderRegistry[1]: want minimax with base_url, got %+v", got.ProviderRegistry[1])
	}

	// Cross-shape negative: `anthropic-api` appears in slugs but not in
	// the structured registry — make sure our parsing didn't synthesize
	// a stub entry for it.
	for _, e := range got.ProviderRegistry {
		if e.Name == "anthropic-api" {
			t.Errorf("ProviderRegistry must not synthesize entries from the slug list — found stray %q", e.Name)
		}
	}

	// JSON wire shape: both fields present in the same response.
	body := w.Body.String()
	if !strings.Contains(body, `"providers":["anthropic-oauth","anthropic-api","minimax"]`) {
		t.Errorf("response missing slug-list providers field: %s", body)
	}
	if !strings.Contains(body, `"provider_registry":[{"name":"anthropic-oauth"`) {
		t.Errorf("response missing structured provider_registry field: %s", body)
	}
}

func TestTemplatesList_OmitsProviderRegistryWhenAbsent(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	tmpDir := t.TempDir()
	tmplDir := filepath.Join(tmpDir, "hermes-no-reg")
	if err := os.MkdirAll(tmplDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	configYaml := `name: Hermes
runtime: hermes
runtime_config:
  model: nousresearch/hermes-4-70b
  providers: [nous, openrouter]
skills: []
`
	if err := os.WriteFile(filepath.Join(tmplDir, "config.yaml"), []byte(configYaml), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	handler := NewTemplatesHandler(tmpDir, nil, nil)
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
	// But the slug list must still surface — both shapes coexist.
	if !strings.Contains(w.Body.String(), `"providers":["nous","openrouter"]`) {
		t.Errorf("expected slug-list providers field still present: %s", w.Body.String())
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
runtime: hermes
runtime_config:
  model: anthropic:claude-opus-4-7
skills: []
`
	if err := os.WriteFile(filepath.Join(tmplDir, "config.yaml"), []byte(configYaml), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	handler := NewTemplatesHandler(tmpDir, nil, nil)
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
runtime: claude-code
model: anthropic:claude-sonnet-4-6
skills: []
`
	if err := os.WriteFile(filepath.Join(tmplDir, "config.yaml"), []byte(configYaml), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	handler := NewTemplatesHandler(tmpDir, nil, nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/templates", nil)
	handler.List(c)

	var resp []templateSummary
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(resp) != 1 || resp[0].Model != "anthropic:claude-sonnet-4-6" {
		t.Fatalf("legacy top-level model not surfaced: %+v", resp)
	}
	if resp[0].Runtime != "claude-code" {
		t.Errorf("Runtime should be claude-code for legacy template, got %q", resp[0].Runtime)
	}
	if len(resp[0].Models) != 0 {
		t.Errorf("Models should be empty for legacy template, got %+v", resp[0].Models)
	}
}

// TestTemplatesList_MalformedYAMLLogsAndSkips pins the diagnostic-on-skip
// behavior. Before, a malformed config.yaml made the affected template
// vanish from /templates with NO trace — operator can't tell it was
// excluded vs never existed. Now the handler logs `templates list:
// skip <id>: yaml.Unmarshal: <err>` and continues with the rest.
//
// Asserts:
//   - bad template is skipped (not present in response)
//   - good sibling template still surfaces (one bad apple shouldn't
//     poison the whole list)
//   - log line names the offending template id (operator can grep)
func TestTemplatesList_MalformedYAMLLogsAndSkips(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	tmpDir := t.TempDir()

	// Bad: YAML scalar where a struct is expected. tier expects int;
	// supplying a list crashes yaml.Unmarshal cleanly.
	badDir := filepath.Join(tmpDir, "bad-template")
	if err := os.MkdirAll(badDir, 0755); err != nil {
		t.Fatalf("mkdir bad: %v", err)
	}
	badYaml := `name: Broken
tier: [not, an, int]
runtime: claude-code
`
	if err := os.WriteFile(filepath.Join(badDir, "config.yaml"), []byte(badYaml), 0644); err != nil {
		t.Fatalf("write bad: %v", err)
	}

	// Good sibling — must survive the bad neighbor.
	goodDir := filepath.Join(tmpDir, "good-template")
	if err := os.MkdirAll(goodDir, 0755); err != nil {
		t.Fatalf("mkdir good: %v", err)
	}
	goodYaml := `name: Good
tier: 1
runtime: hermes
skills: []
`
	if err := os.WriteFile(filepath.Join(goodDir, "config.yaml"), []byte(goodYaml), 0644); err != nil {
		t.Fatalf("write good: %v", err)
	}

	// Capture log output so we can assert on the skip line.
	var logBuf bytes.Buffer
	prevOutput := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(prevOutput)

	handler := NewTemplatesHandler(tmpDir, nil, nil)
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
	// Bad template MUST NOT appear; good template MUST appear.
	if len(resp) != 1 {
		t.Fatalf("expected 1 template (good only, bad skipped), got %d: %+v", len(resp), resp)
	}
	if resp[0].ID != "good-template" {
		t.Errorf("surviving template should be good-template, got %q", resp[0].ID)
	}

	// Log line MUST contain the bad template id and the parse error
	// signal — without these, an operator looking at logs can't
	// correlate "missing from /templates" with "yaml.Unmarshal failed".
	logged := logBuf.String()
	if !strings.Contains(logged, "bad-template") {
		t.Errorf("expected log line to name bad-template, got: %s", logged)
	}
	if !strings.Contains(logged, "yaml.Unmarshal") {
		t.Errorf("expected log line to mention yaml.Unmarshal, got: %s", logged)
	}
}

func TestTemplatesList_NonexistentDir(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	handler := NewTemplatesHandler("/nonexistent/path/to/templates", nil, nil)

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

	handler := NewTemplatesHandler(t.TempDir(), nil, nil)

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

	handler := NewTemplatesHandler(t.TempDir(), nil, nil)

	// SQL shape: SELECT name, COALESCE(instance_id, ''), COALESCE(runtime, '') FROM workspaces WHERE id = $1
	// (matches the L/R/W/D unified shape so dispatchers can branch on
	// instance_id; sqlmock matches via QueryMatcherRegexp so the parens
	// need escaping.)
	mock.ExpectQuery(`SELECT name, COALESCE\(instance_id, ''\), COALESCE\(runtime, ''\) FROM workspaces WHERE id =`).
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
	handler := NewTemplatesHandler(tmpDir, nil, nil) // nil docker = no container

	mock.ExpectQuery(`SELECT name, COALESCE\(instance_id, ''\), COALESCE\(runtime, ''\) FROM workspaces WHERE id =`).
		WithArgs("ws-fallback").
		WillReturnRows(sqlmock.NewRows([]string{"name", "instance_id", "runtime"}).AddRow("Unknown Agent", "", ""))

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

	handler := NewTemplatesHandler(tmpDir, nil, nil)

	mock.ExpectQuery(`SELECT name, COALESCE\(instance_id, ''\), COALESCE\(runtime, ''\) FROM workspaces WHERE id =`).
		WithArgs("ws-tmpl").
		WillReturnRows(sqlmock.NewRows([]string{"name", "instance_id", "runtime"}).AddRow("Test Agent", "", ""))

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

func TestListFiles_FallbackToHost_SkipsSymlinks(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	tmpDir := t.TempDir()
	tmplDir := filepath.Join(tmpDir, "test-agent")
	if err := os.MkdirAll(tmplDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmplDir, "config.yaml"), []byte("name: Test Agent\n"), 0644); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("do-not-list"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(tmplDir, "leaked-secret")); err != nil {
		t.Fatal(err)
	}

	handler := NewTemplatesHandler(tmpDir, nil, nil)

	mock.ExpectQuery(`SELECT name, COALESCE\(instance_id, ''\), COALESCE\(runtime, ''\) FROM workspaces WHERE id =`).
		WithArgs("ws-tmpl").
		WillReturnRows(sqlmock.NewRows([]string{"name", "instance_id", "runtime"}).AddRow("Test Agent", "", ""))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-tmpl"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-tmpl/files", nil)

	handler.ListFiles(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	for _, file := range resp {
		if file["path"] == "leaked-secret" {
			t.Fatalf("symlink should not be listed: %#v", resp)
		}
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== GET /workspaces/:id/files/*path ====================

func TestReadFile_PathTraversal(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	handler := NewTemplatesHandler(t.TempDir(), nil, nil)

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

	handler := NewTemplatesHandler(t.TempDir(), nil, nil)

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

	handler := NewTemplatesHandler(t.TempDir(), nil, nil)

	mock.ExpectQuery(`SELECT name, COALESCE\(instance_id, ''\), COALESCE\(runtime, ''\) FROM workspaces WHERE id =`).
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

	handler := NewTemplatesHandler(tmpDir, nil, nil)

	// instance_id="" → SaaS branch skipped → falls through to local
	// Docker / template-dir host fallback (the only path the test
	// exercises). When instance_id is set, ReadFile would dispatch
	// through readFileViaEIC, which is covered by integration tests.
	mock.ExpectQuery(`SELECT name, COALESCE\(instance_id, ''\), COALESCE\(runtime, ''\) FROM workspaces WHERE id =`).
		WithArgs("ws-read").
		WillReturnRows(sqlmock.NewRows([]string{"name", "instance_id", "runtime"}).
			AddRow("Reader Agent", "", ""))

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
	handler := NewTemplatesHandler(tmpDir, nil, nil)

	mock.ExpectQuery(`SELECT name, COALESCE\(instance_id, ''\), COALESCE\(runtime, ''\) FROM workspaces WHERE id =`).
		WithArgs("ws-nofile").
		WillReturnRows(sqlmock.NewRows([]string{"name", "instance_id", "runtime"}).
			AddRow("No File Agent", "", ""))

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

	handler := NewTemplatesHandler(t.TempDir(), nil, nil)

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

	handler := NewTemplatesHandler(t.TempDir(), nil, nil)

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

	handler := NewTemplatesHandler(t.TempDir(), nil, nil)

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

	handler := NewTemplatesHandler(t.TempDir(), nil, nil)

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

	handler := NewTemplatesHandler(t.TempDir(), nil, nil)

	mock.ExpectQuery(`SELECT name, COALESCE\(instance_id, ''\), COALESCE\(runtime, ''\) FROM workspaces WHERE id =`).
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

// ==================== resolveTemplateDir ====================

func TestResolveTemplateDir_ByNormalizedName(t *testing.T) {
	tmpDir := t.TempDir()
	tmplDir := filepath.Join(tmpDir, "my-agent")
	os.MkdirAll(tmplDir, 0755)

	handler := NewTemplatesHandler(tmpDir, nil, nil)
	result := handler.resolveTemplateDir("My Agent")

	if result != tmplDir {
		t.Errorf("expected %q, got %q", tmplDir, result)
	}
}

func TestResolveTemplateDir_NotFound(t *testing.T) {
	tmpDir := t.TempDir()
	handler := NewTemplatesHandler(tmpDir, nil, nil)
	result := handler.resolveTemplateDir("Nonexistent Agent")

	if result != "" {
		t.Errorf("expected empty string, got %q", result)
	}
}

// ==================== CWE-78 hardening regression (issue #2011) ====================
// These tests lock in the defence-in-depth guards for DeleteFile.
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

			handler := NewTemplatesHandler(t.TempDir(), nil, nil)

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

// ============================================================================
// internal#718 P3 — GET /templates serves the selectable provider/model list
// FROM the provider registry (workspace-server/internal/providers), not from
// each template's hand-authored config.yaml. Additive: the registry-served
// fields (registry_backed / registry_providers / registry_models) ride
// ALONGSIDE the existing template-served fields so non-registry runtimes and
// older canvases keep working. The canvas (PR-B) prefers the registry block;
// "only registered selectable" follows because the registry block is the
// authoritative list for a registry-known runtime.
// ============================================================================

// TestTemplatesList_RegistryServesSelectableModels pins the core P3 contract:
// for a runtime the provider registry knows (claude-code), /templates serves
// the registry's NATIVE model ids — regardless of what the template's
// config.yaml runtime_config.models happens to list. A template author can no
// longer surface an unregistered model into the canvas dropdown.
func TestTemplatesList_RegistryServesSelectableModels(t *testing.T) {
	// SaaS path: a Molecule proxy is configured, so the platform-managed
	// provider + its models ARE selectable (this test's assertions cover them).
	t.Setenv("MOLECULE_LLM_BASE_URL", "https://llm.example.test")
	t.Setenv("MOLECULE_LLM_USAGE_TOKEN", "proxy-tok")

	tmpDir := t.TempDir()
	tmplDir := filepath.Join(tmpDir, "claude-code-default")
	if err := os.MkdirAll(tmplDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Deliberately list a BOGUS model the registry does not know. The
	// registry-served list must NOT contain it.
	configYaml := `name: Claude Code
runtime: claude-code
runtime_config:
  model: claude-sonnet-4-6
  models:
    - id: totally-made-up-model
      name: Not In Registry
skills: []
`
	if err := os.WriteFile(filepath.Join(tmplDir, "config.yaml"), []byte(configYaml), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	handler := NewTemplatesHandler(tmpDir, nil, nil)
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

	if !got.RegistryBacked {
		t.Fatalf("claude-code is a registry-known runtime; RegistryBacked must be true")
	}

	// The registry-served model set must be the claude-code native set
	// (anthropic-oauth: sonnet/opus/haiku, anthropic-api: claude-*-4-*,
	// kimi-coding: kimi-*, minimax: MiniMax-*, platform: vendor/model ids).
	// It must NOT contain the template's bogus id.
	regModelIDs := map[string]bool{}
	for _, m := range got.RegistryModels {
		regModelIDs[m.ID] = true
	}
	if regModelIDs["totally-made-up-model"] {
		t.Errorf("RegistryModels leaked the template's unregistered model id")
	}
	for _, want := range []string{"sonnet", "opus", "claude-opus-4-7", "anthropic/claude-opus-4-7"} {
		if !regModelIDs[want] {
			t.Errorf("RegistryModels missing native model %q; got %v", want, regModelIDs)
		}
	}
}

// TestTemplatesList_RegistryAnnotatesDerivedProviderAndBilling pins that each
// registry-served model carries its DERIVED provider name + a billing_mode
// reflecting whether that derived provider is the closed platform set
// (platform_managed) or BYOK (byok). This is what the canvas Config tab reads
// to show the billing-mode of the DERIVED provider (folds in #1931 intent),
// instead of its hardcoded billingModeForProvider rule.
func TestTemplatesList_RegistryAnnotatesDerivedProviderAndBilling(t *testing.T) {
	// SaaS path: a Molecule proxy is configured, so the platform-managed
	// provider + its models ARE present (this test pins their annotations).
	t.Setenv("MOLECULE_LLM_BASE_URL", "https://llm.example.test")
	t.Setenv("MOLECULE_LLM_USAGE_TOKEN", "proxy-tok")

	tmpDir := t.TempDir()
	tmplDir := filepath.Join(tmpDir, "claude-code-default")
	if err := os.MkdirAll(tmplDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	configYaml := `name: Claude Code
runtime: claude-code
runtime_config:
  model: claude-sonnet-4-6
skills: []
`
	if err := os.WriteFile(filepath.Join(tmplDir, "config.yaml"), []byte(configYaml), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	handler := NewTemplatesHandler(tmpDir, nil, nil)
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
	got := resp[0]

	billByModel := map[string]string{}
	provByModel := map[string]string{}
	for _, m := range got.RegistryModels {
		billByModel[m.ID] = m.BillingMode
		provByModel[m.ID] = m.Provider
	}

	// A BYOK API model derives to anthropic-api → byok.
	if provByModel["claude-opus-4-7"] != "anthropic-api" {
		t.Errorf("claude-opus-4-7 derived provider: want anthropic-api, got %q", provByModel["claude-opus-4-7"])
	}
	if billByModel["claude-opus-4-7"] != "byok" {
		t.Errorf("claude-opus-4-7 billing_mode: want byok, got %q", billByModel["claude-opus-4-7"])
	}
	// A platform-namespaced model derives to the closed platform provider →
	// platform_managed.
	if provByModel["anthropic/claude-opus-4-7"] != "platform" {
		t.Errorf("anthropic/claude-opus-4-7 derived provider: want platform, got %q", provByModel["anthropic/claude-opus-4-7"])
	}
	if billByModel["anthropic/claude-opus-4-7"] != "platform_managed" {
		t.Errorf("anthropic/claude-opus-4-7 billing_mode: want platform_managed, got %q", billByModel["anthropic/claude-opus-4-7"])
	}

	// registry_providers carries the provider display_name + auth_env +
	// billing_mode for the dropdown labels — sourced from the registry, not
	// the canvas VENDOR_LABELS map.
	byName := map[string]registryProviderView{}
	for _, p := range got.RegistryProviders {
		byName[p.Name] = p
	}
	oauth, ok := byName["anthropic-oauth"]
	if !ok {
		t.Fatalf("registry_providers missing anthropic-oauth; got %v", byName)
	}
	if oauth.DisplayName != "Claude Code subscription" {
		t.Errorf("anthropic-oauth display_name: want %q, got %q", "Claude Code subscription", oauth.DisplayName)
	}
	if oauth.BillingMode != "byok" {
		t.Errorf("anthropic-oauth billing_mode: want byok, got %q", oauth.BillingMode)
	}
	if len(oauth.AuthEnv) != 1 || oauth.AuthEnv[0] != "CLAUDE_CODE_OAUTH_TOKEN" {
		t.Errorf("anthropic-oauth auth_env: want [CLAUDE_CODE_OAUTH_TOKEN], got %v", oauth.AuthEnv)
	}
	plat, ok := byName["platform"]
	if !ok || plat.BillingMode != "platform_managed" {
		t.Errorf("platform provider billing_mode: want platform_managed, got %+v", plat)
	}
}

// TestTemplatesList_NonRegistryRuntimeFallsOpenToTemplate pins federation-
// readiness: for a runtime the registry does NOT know (a hypothetical
// third-party / external-like runtime), /templates does NOT set
// RegistryBacked and does NOT synthesize a registry block — the template's
// own config.yaml fields remain the source, unchanged. No behavior change for
// non-registry runtimes.
func TestTemplatesList_NonRegistryRuntimeFallsOpenToTemplate(t *testing.T) {
	tmpDir := t.TempDir()
	tmplDir := filepath.Join(tmpDir, "byo-runtime")
	if err := os.MkdirAll(tmplDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// "mock" is a known runtime to the manifest allowlist (so List doesn't
	// skip it) but is NOT in the provider registry's runtimes: block.
	configYaml := `name: Mock Runtime
runtime: mock
runtime_config:
  model: canned-reply
  providers: [some-byo-provider]
  models:
    - id: canned-reply
      name: Canned Reply
skills: []
`
	if err := os.WriteFile(filepath.Join(tmplDir, "config.yaml"), []byte(configYaml), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	handler := NewTemplatesHandler(tmpDir, nil, nil)
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

	if got.RegistryBacked {
		t.Errorf("mock is NOT a registry runtime; RegistryBacked must be false")
	}
	if len(got.RegistryModels) != 0 || len(got.RegistryProviders) != 0 {
		t.Errorf("non-registry runtime must not synthesize a registry block; got models=%v providers=%v",
			got.RegistryModels, got.RegistryProviders)
	}
	// Template-served fields untouched.
	if len(got.Models) != 1 || got.Models[0].ID != "canned-reply" {
		t.Errorf("template Models unchanged: got %+v", got.Models)
	}
	if !reflect.DeepEqual(got.Providers, []string{"some-byo-provider"}) {
		t.Errorf("template Providers unchanged: got %v", got.Providers)
	}
}

// TestTemplatesList_DisplayableFlag verifies the SSOT-driven runtime-picker
// opt-out: a template's config.yaml `displayable: false` surfaces as a
// non-nil false on the /templates row (canvas hides it), while an absent
// flag stays nil (canvas shows it) and an explicit true surfaces as true.
// This is the backend half of removing the hardcoded frontend allowlist —
// the picker trusts this list, so hiding a runtime must be declarative here.
func TestTemplatesList_DisplayableFlag(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	tmpDir := t.TempDir()

	mk := func(dir, yaml string) {
		d := filepath.Join(tmpDir, dir)
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "config.yaml"), []byte(yaml), 0644); err != nil {
			t.Fatal(err)
		}
	}
	// absent → nil
	mk("adk-shown", "name: ADK Shown\nruntime: claude-code\n")
	// explicit false → hidden marker
	mk("adk-hidden", "name: ADK Hidden\nruntime: claude-code\ndisplayable: false\n")
	// explicit true → shown marker
	mk("adk-explicit", "name: ADK Explicit\nruntime: claude-code\ndisplayable: true\n")

	handler := NewTemplatesHandler(tmpDir, nil, nil)
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
	byID := map[string]templateSummary{}
	for _, s := range resp {
		byID[s.ID] = s
	}

	if s, ok := byID["adk-shown"]; !ok {
		t.Fatal("adk-shown missing")
	} else if s.Displayable != nil {
		t.Errorf("adk-shown: expected nil Displayable (absent), got %v", *s.Displayable)
	}

	if s, ok := byID["adk-hidden"]; !ok {
		t.Fatal("adk-hidden missing")
	} else if s.Displayable == nil || *s.Displayable != false {
		t.Errorf("adk-hidden: expected non-nil false Displayable, got %v", s.Displayable)
	}

	if s, ok := byID["adk-explicit"]; !ok {
		t.Fatal("adk-explicit missing")
	} else if s.Displayable == nil || *s.Displayable != true {
		t.Errorf("adk-explicit: expected non-nil true Displayable, got %v", s.Displayable)
	}

	// JSON contract: omitempty drops the field entirely when nil so existing
	// templates' payloads are byte-unchanged; present when set.
	var rawRows []map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &rawRows); err != nil {
		t.Fatalf("raw parse: %v", err)
	}
	for _, row := range rawRows {
		id := ""
		_ = json.Unmarshal(row["id"], &id)
		_, present := row["displayable"]
		if id == "adk-shown" && present {
			t.Error("adk-shown: displayable key should be omitted when nil")
		}
		if (id == "adk-hidden" || id == "adk-explicit") && !present {
			t.Errorf("%s: displayable key should be present when set", id)
		}
	}
}

// TestTemplatesList_PlatformManagedFilteredWhenNoProxy pins the SSOT filter:
// the closed `platform` provider (and every model that derives to it) is
// dropped from the /templates payload AT THE SOURCE when no Molecule LLM
// proxy is configured (self-host) — so every consumer (ConfigTab,
// CreateWorkspaceDialog, MissingKeysModal) inherits it — and is present,
// unchanged, when the proxy IS configured (SaaS).
func TestTemplatesList_PlatformManagedFilteredWhenNoProxy(t *testing.T) {
	tmpDir := t.TempDir()
	tmplDir := filepath.Join(tmpDir, "claude-code-default")
	if err := os.MkdirAll(tmplDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	configYaml := `name: Claude Code
runtime: claude-code
runtime_config:
  model: claude-sonnet-4-6
skills: []
`
	if err := os.WriteFile(filepath.Join(tmplDir, "config.yaml"), []byte(configYaml), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	fetch := func(t *testing.T) templateSummary {
		t.Helper()
		handler := NewTemplatesHandler(tmpDir, nil, nil)
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
		return resp[0]
	}

	hasPlatformProvider := func(s templateSummary) bool {
		for _, p := range s.RegistryProviders {
			if p.Name == providers.PlatformProviderName {
				return true
			}
		}
		return false
	}
	hasPlatformModel := func(s templateSummary) bool {
		for _, m := range s.RegistryModels {
			if m.Provider == providers.PlatformProviderName {
				return true
			}
		}
		return false
	}

	// Self-host: NO proxy → platform provider + its models are filtered out.
	t.Run("no_proxy_filters_platform", func(t *testing.T) {
		t.Setenv("MOLECULE_LLM_BASE_URL", "")
		t.Setenv("MOLECULE_LLM_USAGE_TOKEN", "")
		t.Setenv("OPENAI_BASE_URL", "")
		t.Setenv("OPENAI_API_KEY", "")
		if PlatformManagedProxyConfigured() {
			t.Fatalf("precondition: proxy must read as unconfigured")
		}

		got := fetch(t)
		if !got.RegistryBacked {
			t.Fatalf("claude-code is registry-backed; RegistryBacked must be true")
		}
		if hasPlatformProvider(got) {
			t.Errorf("self-host: platform provider must be filtered from RegistryProviders; got %+v", got.RegistryProviders)
		}
		if hasPlatformModel(got) {
			t.Errorf("self-host: platform-derived models must be filtered from RegistryModels; got %+v", got.RegistryModels)
		}
		// A BYOK provider/model must STILL be present — only platform is dropped.
		byokPresent := false
		for _, p := range got.RegistryProviders {
			if p.Name == "anthropic-api" {
				byokPresent = true
			}
		}
		if !byokPresent {
			t.Errorf("self-host: BYOK provider anthropic-api must remain; got %+v", got.RegistryProviders)
		}
	})

	// SaaS: proxy configured → platform provider + its models ARE present.
	t.Run("proxy_keeps_platform", func(t *testing.T) {
		t.Setenv("MOLECULE_LLM_BASE_URL", "https://llm.example.test")
		t.Setenv("MOLECULE_LLM_USAGE_TOKEN", "proxy-tok")
		if !PlatformManagedProxyConfigured() {
			t.Fatalf("precondition: proxy must read as configured")
		}

		got := fetch(t)
		if !hasPlatformProvider(got) {
			t.Errorf("SaaS: platform provider must be present in RegistryProviders; got %+v", got.RegistryProviders)
		}
		if !hasPlatformModel(got) {
			t.Errorf("SaaS: platform-derived models must be present in RegistryModels; got %+v", got.RegistryModels)
		}
	})
}
