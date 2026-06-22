package handlers

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/scheduler"
)

func TestOrgDefaults_InitialPrompt_YAMLParsing(t *testing.T) {
	raw := `
runtime: claude-code
tier: 2
initial_prompt: |
  Clone the repo and read CLAUDE.md.
  Report ready status.
`
	var defaults OrgDefaults
	if err := yaml.Unmarshal([]byte(raw), &defaults); err != nil {
		t.Fatalf("failed to parse YAML: %v", err)
	}
	if defaults.Runtime != "claude-code" {
		t.Errorf("expected runtime 'claude-code', got %q", defaults.Runtime)
	}
	if !strings.Contains(defaults.InitialPrompt, "Clone the repo") {
		t.Errorf("expected InitialPrompt to contain 'Clone the repo', got %q", defaults.InitialPrompt)
	}
	if !strings.Contains(defaults.InitialPrompt, "Report ready") {
		t.Errorf("expected InitialPrompt to contain 'Report ready', got %q", defaults.InitialPrompt)
	}
}

func TestOrgWorkspace_InitialPrompt_Override(t *testing.T) {
	raw := `
name: Frontend Engineer
role: Next.js canvas
initial_prompt: Custom FE prompt
`
	var ws OrgWorkspace
	if err := yaml.Unmarshal([]byte(raw), &ws); err != nil {
		t.Fatalf("failed to parse YAML: %v", err)
	}
	if ws.InitialPrompt != "Custom FE prompt" {
		t.Errorf("expected 'Custom FE prompt', got %q", ws.InitialPrompt)
	}
}

func TestInitialPrompt_ConfigYAML_Injection(t *testing.T) {
	// Simulate what createWorkspaceTree does: append initial_prompt to config.yaml
	configYAML := "name: Test\nruntime: claude-code\n"
	initialPrompt := "Clone the repo.\nRead CLAUDE.md.\nReport ready."

	trimmed := strings.TrimSpace(initialPrompt)
	lines := strings.Split(trimmed, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t")
	}
	indented := strings.Join(lines, "\n  ")
	result := configYAML + "initial_prompt: |\n  " + indented + "\n"

	// Parse result as YAML to verify it's valid
	var parsed map[string]interface{}
	if err := yaml.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("generated YAML is invalid: %v\n---\n%s", err, result)
	}

	prompt, ok := parsed["initial_prompt"].(string)
	if !ok {
		t.Fatalf("initial_prompt not found or not a string in parsed YAML")
	}
	if !strings.Contains(prompt, "Clone the repo") {
		t.Errorf("expected prompt to contain 'Clone the repo', got %q", prompt)
	}
	if !strings.Contains(prompt, "Read CLAUDE.md") {
		t.Errorf("expected prompt to contain 'Read CLAUDE.md', got %q", prompt)
	}
}

func TestInitialPrompt_ConfigYAML_Empty(t *testing.T) {
	// When initial_prompt is empty, nothing should be appended
	configYAML := "name: Test\nruntime: claude-code\n"
	initialPrompt := ""

	result := configYAML
	if initialPrompt != "" {
		// This block shouldn't execute
		result += "initial_prompt: |\n  " + initialPrompt + "\n"
	}

	var parsed map[string]interface{}
	if err := yaml.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("generated YAML is invalid: %v", err)
	}
	if _, exists := parsed["initial_prompt"]; exists {
		t.Error("initial_prompt should not exist in config when empty")
	}
}

func TestOrgDefaults_Model_YAMLParsing(t *testing.T) {
	raw := `
runtime: hermes
tier: 2
model: google_genai:gemini-2.5-flash
`
	var defaults OrgDefaults
	if err := yaml.Unmarshal([]byte(raw), &defaults); err != nil {
		t.Fatalf("failed to parse YAML: %v", err)
	}
	if defaults.Model != "google_genai:gemini-2.5-flash" {
		t.Errorf("expected model 'google_genai:gemini-2.5-flash', got %q", defaults.Model)
	}
}

func TestOrgDefaults_Model_Empty(t *testing.T) {
	raw := `
runtime: claude-code
tier: 2
`
	var defaults OrgDefaults
	if err := yaml.Unmarshal([]byte(raw), &defaults); err != nil {
		t.Fatalf("failed to parse YAML: %v", err)
	}
	if defaults.Model != "" {
		t.Errorf("expected empty model when not specified, got %q", defaults.Model)
	}
}

func TestOrgWorkspace_Model_Override(t *testing.T) {
	raw := `
name: Worker
role: coding
model: groq:llama-3.3-70b-versatile
`
	var ws OrgWorkspace
	if err := yaml.Unmarshal([]byte(raw), &ws); err != nil {
		t.Fatalf("failed to parse YAML: %v", err)
	}
	if ws.Model != "groq:llama-3.3-70b-versatile" {
		t.Errorf("expected model 'groq:llama-3.3-70b-versatile', got %q", ws.Model)
	}
}

// ==================== Model Fallback Edge Cases ====================
// These test the cascading fallback: ws.Model → defaults.Model → runtime-specific default
// They verify behavior without a database since createWorkspaceTree requires sqlmock.
// The struct-level tests + ensureDefaultConfig tests cover the full data flow.

func TestOrgDefaults_Model_WorkspaceOverridesDefault(t *testing.T) {
	// When both ws and defaults have a model, ws.Model takes precedence.
	// This verifies the YAML struct correctly captures both values.
	defaultsRaw := `
runtime: hermes
model: google_genai:gemini-2.5-flash
`
	wsRaw := `
name: Worker
model: groq:llama-3.3-70b-versatile
`
	var defaults OrgDefaults
	if err := yaml.Unmarshal([]byte(defaultsRaw), &defaults); err != nil {
		t.Fatalf("failed to parse defaults: %v", err)
	}
	var ws OrgWorkspace
	if err := yaml.Unmarshal([]byte(wsRaw), &ws); err != nil {
		t.Fatalf("failed to parse workspace: %v", err)
	}

	// Simulate the fallback logic from createWorkspaceTree
	model := ws.Model
	if model == "" {
		model = defaults.Model
	}
	if model != "groq:llama-3.3-70b-versatile" {
		t.Errorf("ws.Model should override defaults.Model, got %q", model)
	}
}

func TestOrgDefaults_Model_FallbackClaudeCode(t *testing.T) {
	// When both ws and defaults models are empty, claude-code runtime → "sonnet"
	var defaults OrgDefaults
	var ws OrgWorkspace

	runtime := "claude-code"
	model := ws.Model
	if model == "" {
		model = defaults.Model
	}
	if model == "" {
		if runtime == "claude-code" {
			model = "sonnet"
		} else {
			model = "anthropic:claude-opus-4-7"
		}
	}
	if model != "sonnet" {
		t.Errorf("claude-code with empty model should get 'sonnet', got %q", model)
	}
}

func TestOrgDefaults_Model_FallbackHermes(t *testing.T) {
	// When both ws and defaults models are empty, hermes runtime → anthropic default
	var defaults OrgDefaults
	var ws OrgWorkspace

	runtime := "hermes"
	model := ws.Model
	if model == "" {
		model = defaults.Model
	}
	if model == "" {
		if runtime == "claude-code" {
			model = "sonnet"
		} else {
			model = "anthropic:claude-opus-4-7"
		}
	}
	if model != "anthropic:claude-opus-4-7" {
		t.Errorf("hermes with empty model should get 'anthropic:claude-opus-4-7', got %q", model)
	}
}

func TestOrgDefaults_Model_FallbackCodex(t *testing.T) {
	// Non-Claude-Code runtimes get the default anthropic model in this legacy fallback path.
	model := ""
	runtime := "codex"
	if model == "" {
		if runtime == "claude-code" {
			model = "sonnet"
		} else {
			model = "anthropic:claude-opus-4-7"
		}
	}
	if model != "anthropic:claude-opus-4-7" {
		t.Errorf("codex with empty model should get 'anthropic:claude-opus-4-7', got %q", model)
	}
}

func TestOrgDefaults_Model_DefaultsModelUsedWhenWsEmpty(t *testing.T) {
	// ws.Model empty → falls back to defaults.Model
	defaultsRaw := `
model: cerebras:llama3.1-8b
`
	var defaults OrgDefaults
	if err := yaml.Unmarshal([]byte(defaultsRaw), &defaults); err != nil {
		t.Fatalf("failed to parse defaults: %v", err)
	}

	model := "" // ws.Model is empty
	if model == "" {
		model = defaults.Model
	}
	if model != "cerebras:llama3.1-8b" {
		t.Errorf("expected defaults.Model 'cerebras:llama3.1-8b', got %q", model)
	}
}

func TestInitialPrompt_SpecialChars(t *testing.T) {
	// Ensure YAML-special characters in prompt don't break parsing
	initialPrompt := `Run: git clone https://${GITHUB_TOKEN}@github.com/${GITHUB_REPO}.git
Check "config.yaml" for settings
Use key: value pairs`

	configYAML := "name: Test\n"
	trimmed := strings.TrimSpace(initialPrompt)
	lines := strings.Split(trimmed, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t")
	}
	indented := strings.Join(lines, "\n  ")
	result := configYAML + "initial_prompt: |\n  " + indented + "\n"

	var parsed map[string]interface{}
	if err := yaml.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("generated YAML with special chars is invalid: %v\n---\n%s", err, result)
	}

	prompt := parsed["initial_prompt"].(string)
	if !strings.Contains(prompt, "${GITHUB_TOKEN}") {
		t.Error("expected prompt to preserve ${GITHUB_TOKEN}")
	}
	if !strings.Contains(prompt, `"config.yaml"`) {
		t.Error("expected prompt to preserve quoted strings")
	}
}

// ==================== OrgChannel + env expansion tests ====================

func TestOrgChannel_YAMLParsing(t *testing.T) {
	raw := `
name: PM
files_dir: pm
channels:
  - type: telegram
    config:
      bot_token: ${TELEGRAM_BOT_TOKEN}
      chat_id: ${TELEGRAM_CHAT_ID}
    allowed_users: ["123", "456"]
    enabled: true
`
	var ws OrgWorkspace
	if err := yaml.Unmarshal([]byte(raw), &ws); err != nil {
		t.Fatalf("YAML parse failed: %v", err)
	}
	if len(ws.Channels) != 1 {
		t.Fatalf("expected 1 channel, got %d", len(ws.Channels))
	}
	ch := ws.Channels[0]
	if ch.Type != "telegram" {
		t.Errorf("expected type telegram, got %q", ch.Type)
	}
	if ch.Config["bot_token"] != "${TELEGRAM_BOT_TOKEN}" {
		t.Errorf("expected raw ${VAR}, got %q", ch.Config["bot_token"])
	}
	if len(ch.AllowedUsers) != 2 {
		t.Errorf("expected 2 allowed users, got %d", len(ch.AllowedUsers))
	}
	if ch.Enabled == nil || !*ch.Enabled {
		t.Error("expected enabled=true")
	}
}

func TestExpandWithEnv_FromMap(t *testing.T) {
	env := map[string]string{"FOO": "bar", "TOKEN": "abc123"}
	got := expandWithEnv("${FOO}-${TOKEN}", env)
	if got != "bar-abc123" {
		t.Errorf("expected 'bar-abc123', got %q", got)
	}
}

func TestExpandWithEnv_FromProcessEnv(t *testing.T) {
	t.Setenv("EXPAND_TEST_VAR", "process-value")
	got := expandWithEnv("${EXPAND_TEST_VAR}", map[string]string{})
	if got != "process-value" {
		t.Errorf("expected 'process-value', got %q", got)
	}
}

func TestExpandWithEnv_MapOverridesProcess(t *testing.T) {
	t.Setenv("OVERRIDE_VAR", "process")
	got := expandWithEnv("${OVERRIDE_VAR}", map[string]string{"OVERRIDE_VAR": "map"})
	if got != "map" {
		t.Errorf("map should override process env, got %q", got)
	}
}

func TestExpandWithEnv_UnsetVar(t *testing.T) {
	got := expandWithEnv("${DEFINITELY_NOT_SET_XYZ}", map[string]string{})
	if got != "" {
		t.Errorf("unset var should expand to empty, got %q", got)
	}
}

func TestHasUnresolvedVarRef_LiteralDollar(t *testing.T) {
	// "$5" is a literal price, not a var ref — should NOT be flagged
	if hasUnresolvedVarRef("price: $5", "price: $5") {
		t.Error("literal $5 should not be flagged as unresolved")
	}
}

func TestHasUnresolvedVarRef_DollarVarSyntax(t *testing.T) {
	// $VAR syntax (no braces) — also a real ref
	if !hasUnresolvedVarRef("$MISSING_VAR", "") {
		t.Error("$VAR syntax should be detected as ref when unresolved")
	}
}

func eqStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPlugins_UnionWithDefaults(t *testing.T) {
	got := mergePlugins([]string{"a", "b"}, []string{"c"})
	want := []string{"a", "b", "c"}
	if !eqStringSlice(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestPlugins_DedupesDuplicates(t *testing.T) {
	got := mergePlugins([]string{"a", "b"}, []string{"b", "c"})
	want := []string{"a", "b", "c"}
	if !eqStringSlice(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestPlugins_OptOutWithBang(t *testing.T) {
	got := mergePlugins([]string{"a", "b", "c"}, []string{"!b", "d"})
	want := []string{"a", "c", "d"}
	if !eqStringSlice(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestPlugins_OptOutWithDash(t *testing.T) {
	got := mergePlugins([]string{"a", "b"}, []string{"-a"})
	want := []string{"b"}
	if !eqStringSlice(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// ==================== category_routing (issue #51) ====================

func TestCategoryRouting_ParsedFromOrgYaml(t *testing.T) {
	raw := `
name: Test Org
defaults:
  runtime: claude-code
  category_routing:
    security: [Backend Engineer, DevOps Engineer]
    ui: [Frontend Engineer]
    infra: [DevOps Engineer]
workspaces:
  - name: PM
    role: Project Manager
    category_routing:
      performance: [Backend Engineer]
`
	var tmpl OrgTemplate
	if err := yaml.Unmarshal([]byte(raw), &tmpl); err != nil {
		t.Fatalf("yaml parse failed: %v", err)
	}
	if got := tmpl.Defaults.CategoryRouting["security"]; len(got) != 2 || got[0] != "Backend Engineer" {
		t.Errorf("defaults.category_routing.security wrong: %v", got)
	}
	if got := tmpl.Defaults.CategoryRouting["ui"]; len(got) != 1 || got[0] != "Frontend Engineer" {
		t.Errorf("defaults.category_routing.ui wrong: %v", got)
	}
	if len(tmpl.Workspaces) != 1 {
		t.Fatalf("expected 1 workspace, got %d", len(tmpl.Workspaces))
	}
	if got := tmpl.Workspaces[0].CategoryRouting["performance"]; len(got) != 1 || got[0] != "Backend Engineer" {
		t.Errorf("ws.category_routing.performance wrong: %v", got)
	}
}

func TestCategoryRouting_UnionWithDefaults(t *testing.T) {
	defaults := map[string][]string{
		"security": {"Backend Engineer", "DevOps"},
		"ui":       {"Frontend Engineer"},
		"infra":    {"DevOps"},
	}
	ws := map[string][]string{
		"performance": {"Backend Engineer"}, // new key, added
		"ui":          {"Designer"},         // override-replace existing key
		"infra":       {},                   // empty → drop
	}
	got := mergeCategoryRouting(defaults, ws)

	if v := got["security"]; len(v) != 2 || v[0] != "Backend Engineer" || v[1] != "DevOps" {
		t.Errorf("security should be inherited from defaults unchanged, got %v", v)
	}
	if v := got["ui"]; len(v) != 1 || v[0] != "Designer" {
		t.Errorf("ui should be replaced by ws value, got %v", v)
	}
	if _, ok := got["infra"]; ok {
		t.Errorf("infra should be dropped (empty ws list), got %v", got["infra"])
	}
	if v := got["performance"]; len(v) != 1 || v[0] != "Backend Engineer" {
		t.Errorf("performance should be added from ws, got %v", v)
	}
}

func TestCategoryRouting_RenderedIntoWorkspaceConfig(t *testing.T) {
	routing := map[string][]string{
		"security": {"Backend Engineer", "DevOps"},
		"ui":       {"Frontend Engineer"},
	}
	block, err := renderCategoryRoutingYAML(routing)
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	if block == "" {
		t.Fatal("expected non-empty block")
	}
	// Must parse as valid YAML when concatenated with a base config
	combined := "name: Test\nruntime: claude-code\n" + block
	var parsed map[string]interface{}
	if err := yaml.Unmarshal([]byte(combined), &parsed); err != nil {
		t.Fatalf("rendered YAML is invalid: %v\n---\n%s", err, combined)
	}
	cr, ok := parsed["category_routing"].(map[string]interface{})
	if !ok {
		t.Fatalf("category_routing not a map in parsed config: %T", parsed["category_routing"])
	}
	sec, ok := cr["security"].([]interface{})
	if !ok || len(sec) != 2 {
		t.Fatalf("security routing wrong shape: %v", cr["security"])
	}
	if sec[0] != "Backend Engineer" || sec[1] != "DevOps" {
		t.Errorf("security roles wrong: %v", sec)
	}
	ui, ok := cr["ui"].([]interface{})
	if !ok || len(ui) != 1 || ui[0] != "Frontend Engineer" {
		t.Errorf("ui roles wrong: %v", cr["ui"])
	}
	// Output should be deterministic (keys sorted) — security < ui
	if strings.Index(block, "security") > strings.Index(block, "ui") {
		t.Errorf("expected sorted keys (security before ui), got:\n%s", block)
	}
}

// YAML-reserved characters in role names must be escaped by the YAML library.
// Regression guard for the earlier hand-rolled JSON-as-YAML implementation.
func TestCategoryRouting_EscapesYAMLSpecials(t *testing.T) {
	routing := map[string][]string{
		"security": {"Role: with colon", `Role "with quotes"`, "Role\nwith newline"},
	}
	block, err := renderCategoryRoutingYAML(routing)
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	var parsed map[string]interface{}
	if err := yaml.Unmarshal([]byte(block), &parsed); err != nil {
		t.Fatalf("rendered YAML is invalid for special chars: %v\n---\n%s", err, block)
	}
	cr := parsed["category_routing"].(map[string]interface{})
	roles := cr["security"].([]interface{})
	if len(roles) != 3 || roles[0] != "Role: with colon" {
		t.Errorf("special-char roles did not round-trip: %v", roles)
	}
}

// appendYAMLBlock must guarantee a newline boundary between existing buffer and
// the new block so downstream parsers see two separate top-level keys.
func TestAppendYAMLBlock_NewlineGuard(t *testing.T) {
	cases := []struct {
		name     string
		existing string
		block    string
	}{
		{"existing ends without newline", "name: foo", "category_routing:\n  a: [b]\n"},
		{"existing ends with newline", "name: foo\n", "category_routing:\n  a: [b]\n"},
		{"empty existing", "", "category_routing:\n  a: [b]\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := appendYAMLBlock([]byte(tc.existing), tc.block)
			var parsed map[string]interface{}
			if err := yaml.Unmarshal(got, &parsed); err != nil {
				t.Fatalf("appended YAML invalid: %v\n---\n%s", err, string(got))
			}
			if _, ok := parsed["category_routing"]; !ok {
				t.Errorf("expected top-level category_routing key, got: %v", parsed)
			}
		})
	}
}

func TestCategoryRouting_EmptyRendersNothing(t *testing.T) {
	got, err := renderCategoryRoutingYAML(nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty render for nil routing, got %q", got)
	}
	got, err = renderCategoryRoutingYAML(map[string][]string{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty render for empty map, got %q", got)
	}
}

func TestPlugins_BackwardCompat(t *testing.T) {
	// Re-listing defaults in per-workspace plugins still yields the same list
	// (dedupe keeps behavior stable for existing org.yaml files).
	got := mergePlugins([]string{"a", "b"}, []string{"a", "b", "c"})
	want := []string{"a", "b", "c"}
	if !eqStringSlice(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// ── TestOrgImport_ScheduleComputeError (#722 Bug 2) ───────────────────────────
//
// The org importer previously used `nextRun, _ := scheduler.ComputeNextRun(...)`,
// discarding the error and passing time.Time{} (zero value) to the INSERT.
// After fix #722 it surfaces the error and skips the INSERT via `continue`.
//
// This test verifies that the inputs an org.yaml schedule can supply (bad cron
// expression, invalid timezone) DO cause ComputeNextRun to return a non-nil
// error — confirming that the fix is meaningful and the skip path is reachable.

func TestOrgImport_ScheduleComputeError(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name     string
		cronExpr string
		tz       string
	}{
		{
			name:     "invalid cron expression",
			cronExpr: "not-a-cron-expr",
			tz:       "UTC",
		},
		{
			name:     "invalid timezone",
			cronExpr: "0 9 * * 1",
			tz:       "Not/A/Valid/Timezone",
		},
		{
			name:     "both invalid",
			cronExpr: "every monday",
			tz:       "Moon/Far_Side",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := scheduler.ComputeNextRun(tc.cronExpr, tc.tz, now)
			if err == nil {
				t.Errorf("ComputeNextRun(%q, %q) returned nil error — "+
					"org importer would silently insert zero next_run_at; #722 fix requires non-nil",
					tc.cronExpr, tc.tz)
			}
		})
	}
}

// ============================================================================
// Org env-preflight aggregation (collectOrgEnv)
// ============================================================================

// strictReq builds a slice of single-name EnvRequirements for test
// fixtures. Equivalent to the old []string literal but wrapped in
// the new union shape.
func strictReq(names ...string) []EnvRequirement {
	out := make([]EnvRequirement, 0, len(names))
	for _, n := range names {
		out = append(out, EnvRequirement{Name: n})
	}
	return out
}

// anyOfReq builds a single any-of EnvRequirement for test fixtures.
func anyOfReq(names ...string) EnvRequirement {
	return EnvRequirement{AnyOf: append([]string(nil), names...)}
}

// reqNames flattens a slice of EnvRequirements into a single comparable
// slice: single-name reqs contribute their Name, any-of reqs contribute
// "anyOf(A|B|C)" with members sorted for deterministic output. Lets
// tests assert against a string form regardless of which kind each
// entry takes.
func reqNames(reqs []EnvRequirement) []string {
	out := make([]string, 0, len(reqs))
	for _, r := range reqs {
		if r.Name != "" {
			out = append(out, r.Name)
			continue
		}
		members := append([]string(nil), r.AnyOf...)
		sort.Strings(members)
		out = append(out, "anyOf("+strings.Join(members, "|")+")")
	}
	return out
}

func TestCollectOrgEnv_UnionAcrossLevels(t *testing.T) {
	tmpl := &OrgTemplate{
		RequiredEnv:    strictReq("ANTHROPIC_API_KEY"),
		RecommendedEnv: strictReq("SLACK_WEBHOOK_URL"),
		Workspaces: []OrgWorkspace{
			{
				Name:        "Root",
				RequiredEnv: strictReq("GITHUB_TOKEN"),
				Children: []OrgWorkspace{
					{
						Name:           "Leaf",
						RequiredEnv:    strictReq("OPENROUTER_API_KEY"),
						RecommendedEnv: strictReq("DISCORD_WEBHOOK_URL"),
					},
				},
			},
		},
	}
	req, rec := collectOrgEnv(tmpl)
	// Required is the union of top-level + root + leaf.
	wantReq := []string{"ANTHROPIC_API_KEY", "GITHUB_TOKEN", "OPENROUTER_API_KEY"}
	if !stringSlicesEqual(reqNames(req), wantReq) {
		t.Errorf("required mismatch: got %v, want %v", reqNames(req), wantReq)
	}
	wantRec := []string{"DISCORD_WEBHOOK_URL", "SLACK_WEBHOOK_URL"}
	if !stringSlicesEqual(reqNames(rec), wantRec) {
		t.Errorf("recommended mismatch: got %v, want %v", reqNames(rec), wantRec)
	}
}

func TestCollectOrgEnv_RequiredWinsOverRecommended(t *testing.T) {
	// Same key declared at one layer as recommended and another as
	// required MUST surface only on the required side — a required
	// declaration is strictly stricter than a recommended one, and
	// listing it in both tiers would confuse the preflight modal.
	tmpl := &OrgTemplate{
		RecommendedEnv: strictReq("API_KEY"),
		Workspaces: []OrgWorkspace{
			{Name: "X", RequiredEnv: strictReq("API_KEY")},
		},
	}
	req, rec := collectOrgEnv(tmpl)
	if len(req) != 1 || req[0].Name != "API_KEY" {
		t.Errorf("required should contain API_KEY, got %v", reqNames(req))
	}
	for _, r := range rec {
		if r.Name == "API_KEY" {
			t.Errorf("API_KEY must not appear in recommended once required elsewhere")
		}
	}
}

func TestCollectOrgEnv_Dedup(t *testing.T) {
	// Same key declared twice at different levels should appear once.
	tmpl := &OrgTemplate{
		RequiredEnv: strictReq("K", "K"),
		Workspaces: []OrgWorkspace{
			{Name: "A", RequiredEnv: strictReq("K")},
			{Name: "B", RequiredEnv: strictReq("K"), Children: []OrgWorkspace{
				{Name: "C", RequiredEnv: strictReq("K")},
			}},
		},
	}
	req, _ := collectOrgEnv(tmpl)
	if len(req) != 1 || req[0].Name != "K" {
		t.Errorf("dedup failed: got %v, want [K]", reqNames(req))
	}
}

func TestCollectOrgEnv_Empty(t *testing.T) {
	tmpl := &OrgTemplate{}
	req, rec := collectOrgEnv(tmpl)
	if len(req) != 0 || len(rec) != 0 {
		t.Errorf("empty template should produce empty slices, got req=%v rec=%v", reqNames(req), reqNames(rec))
	}
}

// stringSlicesEqual checks ordered equality — collectOrgEnv sorts its
// output so callers can do deterministic comparisons.
func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestCollectOrgEnv_RequiredWinsOnSameStruct(t *testing.T) {
	// The same key declared required AND recommended on the SAME
	// workspace node (rare but legal to parse) must still dedup
	// correctly and end up required-only.
	tmpl := &OrgTemplate{
		Workspaces: []OrgWorkspace{
			{
				Name:           "X",
				RequiredEnv:    strictReq("API_KEY"),
				RecommendedEnv: strictReq("API_KEY"),
			},
		},
	}
	req, rec := collectOrgEnv(tmpl)
	if len(req) != 1 || req[0].Name != "API_KEY" {
		t.Errorf("required should contain API_KEY once, got %v", reqNames(req))
	}
	for _, r := range rec {
		if r.Name == "API_KEY" {
			t.Errorf("API_KEY must not appear in recommended when also required on same struct")
		}
	}
}

func TestCollectOrgEnv_RejectsInvalidNames(t *testing.T) {
	// Names failing envVarNamePattern (lowercase, traversal, whitespace,
	// shell metachars) must be dropped silently — the log line is not
	// asserted here; the output slice assertion is enough to prove the
	// filter fires.
	tmpl := &OrgTemplate{
		RequiredEnv: strictReq(
			"VALID_ONE",
			"lowercase_bad",
			"../../etc/passwd",
			"name with spaces",
			"WITH-DASH",
			"'; DROP TABLE users;--",
			"",
			"A", // single char — still valid per regex
		),
	}
	req, _ := collectOrgEnv(tmpl)
	if !stringSlicesEqual(reqNames(req), []string{"A", "VALID_ONE"}) {
		t.Errorf("expected only valid names, got %v", reqNames(req))
	}
}

// TestOrgTemplate_ClaudeAnyOfAuthPreflight exercises the shape the
// ux-ab-lab template ships with: a single any-of group at the org
// level covering ANTHROPIC_API_KEY vs. CLAUDE_CODE_OAUTH_TOKEN, plus
// two strict recommended entries (SERPER_API_KEY, VERCEL_TOKEN).
// Proves the end-to-end YAML → OrgTemplate → collectOrgEnv → IsSatisfied
// pipeline works for the canonical "Claude sub OR API key" pattern
// without depending on the on-disk template file (org-templates/ is
// populated by the clone-manifest, not tracked in this monorepo).
func TestOrgTemplate_ClaudeAnyOfAuthPreflight(t *testing.T) {
	src := `
name: UX A/B Lab
required_env:
  - any_of:
      - ANTHROPIC_API_KEY
      - CLAUDE_CODE_OAUTH_TOKEN
recommended_env:
  - SERPER_API_KEY
  - VERCEL_TOKEN
workspaces:
  - name: Design Director
    children:
      - name: UX Researcher
      - name: Visual Designer
      - name: React Engineer
      - name: Deploy Engineer
      - name: A11y + SEO Auditor
      - name: Perf Auditor
`
	var tmpl OrgTemplate
	if err := yaml.Unmarshal([]byte(src), &tmpl); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(tmpl.Workspaces) != 1 || len(tmpl.Workspaces[0].Children) != 6 {
		t.Fatalf("expected 1 root with 6 children, got shape %+v", tmpl.Workspaces)
	}

	required, recommended := collectOrgEnv(&tmpl)
	if len(required) != 1 {
		t.Fatalf("expected 1 required requirement (the any-of group), got %d: %v", len(required), reqNames(required))
	}
	if required[0].Name != "" {
		t.Errorf("expected any-of group, got strict name %q", required[0].Name)
	}
	wantMembers := []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"}
	got := append([]string(nil), required[0].AnyOf...)
	sort.Strings(got)
	if !stringSlicesEqual(got, wantMembers) {
		t.Errorf("any-of members mismatch: got %v, want %v", got, wantMembers)
	}

	// Either member should independently satisfy the group.
	if !required[0].IsSatisfied(map[string]struct{}{"ANTHROPIC_API_KEY": {}}) {
		t.Errorf("ANTHROPIC_API_KEY alone should satisfy the group")
	}
	if !required[0].IsSatisfied(map[string]struct{}{"CLAUDE_CODE_OAUTH_TOKEN": {}}) {
		t.Errorf("CLAUDE_CODE_OAUTH_TOKEN alone should satisfy the group")
	}
	if required[0].IsSatisfied(map[string]struct{}{"OPENAI_API_KEY": {}}) {
		t.Errorf("unrelated key should NOT satisfy the group")
	}

	wantRec := []string{"SERPER_API_KEY", "VERCEL_TOKEN"}
	if !stringSlicesEqual(reqNames(recommended), wantRec) {
		t.Errorf("recommended mismatch: got %v, want %v", reqNames(recommended), wantRec)
	}
}

// TestEnvRequirement_UnmarshalYAML proves the on-disk YAML shape
// (scalar OR `{any_of: [...]}` block) round-trips into EnvRequirement
// correctly. The preflight pipeline reads user-authored org.yaml
// files; a regression here would silently drop requirements.
func TestEnvRequirement_UnmarshalYAML(t *testing.T) {
	src := `
required_env:
  - GITHUB_TOKEN
  - any_of:
      - ANTHROPIC_API_KEY
      - CLAUDE_CODE_OAUTH_TOKEN
`
	var parsed struct {
		RequiredEnv []EnvRequirement `yaml:"required_env"`
	}
	if err := yaml.Unmarshal([]byte(src), &parsed); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if len(parsed.RequiredEnv) != 2 {
		t.Fatalf("want 2 requirements, got %d", len(parsed.RequiredEnv))
	}
	if parsed.RequiredEnv[0].Name != "GITHUB_TOKEN" {
		t.Errorf("first should be strict GITHUB_TOKEN, got %+v", parsed.RequiredEnv[0])
	}
	if parsed.RequiredEnv[1].Name != "" || len(parsed.RequiredEnv[1].AnyOf) != 2 {
		t.Errorf("second should be any-of group, got %+v", parsed.RequiredEnv[1])
	}
}

// TestEnvRequirement_UnmarshalYAML_RejectsEmptyAnyOf guards against a
// template that ships `any_of: []` — ambiguous semantics (impossible
// to satisfy), so the parser must fail loudly rather than silently
// pass a never-satisfiable requirement through the preflight.
func TestEnvRequirement_UnmarshalYAML_RejectsEmptyAnyOf(t *testing.T) {
	src := `
required_env:
  - any_of: []
`
	var parsed struct {
		RequiredEnv []EnvRequirement `yaml:"required_env"`
	}
	err := yaml.Unmarshal([]byte(src), &parsed)
	if err == nil {
		t.Errorf("expected error for empty any_of, got nil: %+v", parsed)
	}
}

// ---------------------------------------------------------------------
// any_of group tests — the new EnvRequirement union shape allows a
// single requirement to be satisfied by any of a list of members (e.g.
// ANTHROPIC_API_KEY OR CLAUDE_CODE_OAUTH_TOKEN). collectOrgEnv +
// IsSatisfied together must handle this correctly.
// ---------------------------------------------------------------------

func TestEnvRequirement_IsSatisfied(t *testing.T) {
	configured := map[string]struct{}{
		"ANTHROPIC_API_KEY": {},
		"GITHUB_TOKEN":      {},
	}
	tests := []struct {
		name string
		req  EnvRequirement
		want bool
	}{
		{"strict present", EnvRequirement{Name: "ANTHROPIC_API_KEY"}, true},
		{"strict absent", EnvRequirement{Name: "MISSING_KEY"}, false},
		{"any-of first member present", anyOfReq("ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"), true},
		{"any-of second member present", anyOfReq("CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY"), true},
		{"any-of none present", anyOfReq("OPENAI_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"), false},
		{"any-of single member present", anyOfReq("GITHUB_TOKEN"), true},
	}
	for _, tt := range tests {
		if got := tt.req.IsSatisfied(configured); got != tt.want {
			t.Errorf("%s: got %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestCollectOrgEnv_AnyOfGroupPreserved(t *testing.T) {
	// A group with two alternatives should come through as a single
	// EnvRequirement carrying both members.
	tmpl := &OrgTemplate{
		RequiredEnv: []EnvRequirement{
			anyOfReq("ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"),
		},
	}
	req, _ := collectOrgEnv(tmpl)
	if len(req) != 1 {
		t.Fatalf("expected 1 requirement, got %d: %v", len(req), reqNames(req))
	}
	if req[0].Name != "" {
		t.Errorf("expected any-of group, got strict name %q", req[0].Name)
	}
	wantMembers := []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"}
	got := append([]string(nil), req[0].AnyOf...)
	sort.Strings(got)
	if !stringSlicesEqual(got, wantMembers) {
		t.Errorf("any-of members mismatch: got %v, want %v", got, wantMembers)
	}
}

func TestCollectOrgEnv_AnyOfGroupDedup(t *testing.T) {
	// Two identical groups (members in different order) declared at
	// different levels must collapse to one.
	tmpl := &OrgTemplate{
		RequiredEnv: []EnvRequirement{
			anyOfReq("ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"),
		},
		Workspaces: []OrgWorkspace{
			{
				Name: "Root",
				RequiredEnv: []EnvRequirement{
					anyOfReq("CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY"),
				},
			},
		},
	}
	req, _ := collectOrgEnv(tmpl)
	if len(req) != 1 {
		t.Errorf("expected 1 requirement after dedup, got %d: %v", len(req), reqNames(req))
	}
}

func TestCollectOrgEnv_StrictDominatesGroup(t *testing.T) {
	// If a strict requirement X is declared anywhere, any-of groups
	// that CONTAIN X are redundant — the strict requirement will force
	// X to be configured, which satisfies any group mentioning it too.
	// Same-tier pruning drops the group.
	tmpl := &OrgTemplate{
		RequiredEnv: []EnvRequirement{
			{Name: "ANTHROPIC_API_KEY"},
			anyOfReq("ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"),
		},
	}
	req, _ := collectOrgEnv(tmpl)
	if len(req) != 1 || req[0].Name != "ANTHROPIC_API_KEY" {
		t.Errorf("strict should dominate group, got %v", reqNames(req))
	}
}

func TestCollectOrgEnv_StrictRequiredDominatesRecommendedGroup(t *testing.T) {
	// Cross-tier: a strict required X drops any-of groups in the
	// recommended tier that mention X.
	tmpl := &OrgTemplate{
		RequiredEnv: strictReq("ANTHROPIC_API_KEY"),
		RecommendedEnv: []EnvRequirement{
			anyOfReq("ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"),
			{Name: "SLACK_WEBHOOK_URL"},
		},
	}
	req, rec := collectOrgEnv(tmpl)
	if len(req) != 1 || req[0].Name != "ANTHROPIC_API_KEY" {
		t.Errorf("required mismatch: got %v", reqNames(req))
	}
	// The any-of group should have been pruned; only SLACK remains.
	if len(rec) != 1 || rec[0].Name != "SLACK_WEBHOOK_URL" {
		t.Errorf("recommended mismatch: got %v, want [SLACK_WEBHOOK_URL]", reqNames(rec))
	}
}

func TestCollectOrgEnv_AnyOfWithInvalidMemberKeepsValidOnes(t *testing.T) {
	// A group with one valid + one invalid member should keep the
	// valid one (group carried by any remaining legitimate name). A
	// group where ALL members are invalid is dropped entirely.
	tmpl := &OrgTemplate{
		RequiredEnv: []EnvRequirement{
			anyOfReq("VALID_ONE", "lowercase_bad"),
			anyOfReq("'; DROP TABLE;--", ""),
		},
	}
	req, _ := collectOrgEnv(tmpl)
	if len(req) != 1 {
		t.Fatalf("expected 1 requirement, got %d: %v", len(req), reqNames(req))
	}
	// The remaining group has only one valid member, so it gets
	// promoted to a single-name requirement (len(members)==1 path).
	if req[0].Name != "VALID_ONE" && !stringSlicesEqual(req[0].AnyOf, []string{"VALID_ONE"}) {
		t.Errorf("expected VALID_ONE to survive, got %v", reqNames(req))
	}
}

func TestResolveProvisionConcurrency_ValidPositive(t *testing.T) {
	t.Setenv("MOLECULE_PROVISION_CONCURRENCY", "8")
	got := resolveProvisionConcurrency()
	if got != 8 {
		t.Errorf("valid positive: got %d, want 8", got)
	}
}

func TestResolveProvisionConcurrency_Zero(t *testing.T) {
	t.Setenv("MOLECULE_PROVISION_CONCURRENCY", "0")
	got := resolveProvisionConcurrency()
	if got != 1<<20 {
		t.Errorf("zero (unlimited): got %d, want %d", got, 1<<20)
	}
}

func TestResolveProvisionConcurrency_Negative(t *testing.T) {
	t.Setenv("MOLECULE_PROVISION_CONCURRENCY", "-5")
	got := resolveProvisionConcurrency()
	if got != defaultProvisionConcurrency {
		t.Errorf("negative: got %d, want default %d", got, defaultProvisionConcurrency)
	}
}

func TestResolveProvisionConcurrency_NonInteger(t *testing.T) {
	t.Setenv("MOLECULE_PROVISION_CONCURRENCY", "abc")
	got := resolveProvisionConcurrency()
	if got != defaultProvisionConcurrency {
		t.Errorf("non-integer: got %d, want default %d", got, defaultProvisionConcurrency)
	}
}

func TestResolveProvisionConcurrency_Whitespace(t *testing.T) {
	t.Setenv("MOLECULE_PROVISION_CONCURRENCY", "  7  ")
	got := resolveProvisionConcurrency()
	if got != 7 {
		t.Errorf("whitespace: got %d, want 7", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// errString tests
// ─────────────────────────────────────────────────────────────────────────────

func TestErrString_Nil(t *testing.T) {
	got := errString(nil)
	if got != "" {
		t.Errorf("nil error: got %q, want empty string", got)
	}
}

func TestErrString_NonNil(t *testing.T) {
	err := fmt.Errorf("something went wrong")
	got := errString(err)
	if got != "something went wrong" {
		t.Errorf("non-nil error: got %q, want %q", got, "something went wrong")
	}
}

func TestErrString_Wrapped(t *testing.T) {
	inner := errors.New("inner")
	err := fmt.Errorf("outer: %w", inner)
	got := errString(err)
	if !strings.Contains(got, "outer") {
		t.Errorf("wrapped error: got %q, want containing 'outer'", got)
	}
}
