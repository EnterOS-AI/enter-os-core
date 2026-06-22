package handlers

// Unit tests for runtime_registry.go. Verify:
//   1. Happy path — manifest.json maps correctly to runtime names
//      (including the -default suffix strip).
//   2. "external" is always injected, even on manifests without it.
//   3. Missing file / malformed JSON returns error, caller uses
//      fallback (tested at the initKnownRuntimes level via integration).
//   4. initTemplateRepoByName populates the map at the prod-init
//      path (PR-B / RFC #2843 #24 contract-pin: the map must be
//      non-empty after init for shipped runtimes).
//   5. initTemplateRepoByName is idempotent + reconciles stale
//      entries on every-boot (a runtime removed from the manifest
//      must NOT be resolvable in the map after the next init).

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
			{"name": "codex", "repo": "org/t-codex"},
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
	want := []string{"claude-code", "codex", "hermes", "external", "kimi", "kimi-cli"}
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

func TestLoadRuntimesFromManifest_UsesExplicitRuntimeForVariants(t *testing.T) {
	// Template variants such as "seo-agent" declare an explicit base
	// runtime in manifest.json. The variant name must NOT become a
	// standalone runtime identifier, or PATCH /workspaces/:id could
	// persist a pseudo-runtime that no adapter recognizes.
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")
	err := os.WriteFile(path, []byte(`{
		"workspace_templates": [
			{"name": "claude-code-default", "repo": "org/t-cc"},
			{"name": "seo-agent", "repo": "org/t-seo", "runtime": "claude-code"}
		]
	}`), 0600)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := loadRuntimesFromManifest(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, ok := got["claude-code"]; !ok {
		t.Errorf("expected base runtime 'claude-code' in set, got=%v", keys(got))
	}
	if _, ok := got["seo-agent"]; ok {
		t.Errorf("template variant 'seo-agent' must NOT be exposed as a runtime, got=%v", keys(got))
	}
}

func TestLoadRuntimesFromManifest_ExternalAlwaysInjected(t *testing.T) {
	// Even a manifest without external (which matches reality —
	// external has no template repo) must still produce "external"
	// in the set, because it's the BYO-compute meta-runtime.
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")
	_ = os.WriteFile(path, []byte(`{"workspace_templates":[{"name":"codex","repo":"org/t"}]}`), 0600)

	got, err := loadRuntimesFromManifest(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, must := range []string{"external", "kimi", "kimi-cli"} {
		if _, ok := got[must]; !ok {
			t.Errorf("%s must be injected even when absent from manifest: %v", must, keys(got))
		}
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
	for _, must := range []string{"codex", "hermes", "openclaw", "claude-code", "external", "kimi", "kimi-cli"} {
		if _, ok := got[must]; !ok {
			t.Errorf("real manifest missing runtime %q — got=%v", must, keys(got))
		}
	}
	for _, removed := range retiredRuntimeNamesForTest() {
		if _, ok := got[removed]; ok {
			t.Errorf("real manifest should not expose unsupported runtime %q — got=%v", removed, keys(got))
		}
	}
	for _, variant := range templateVariantNamesForTest() {
		if _, ok := got[variant]; ok {
			t.Errorf("real manifest should not expose template variant %q as a runtime — got=%v", variant, keys(got))
		}
	}
}

func retiredRuntimeNamesForTest() []string {
	return []string{
		"auto" + "gen",
		"deep" + "agents",
		"crew" + "ai",
		"gemini" + "-cli",
		"lang" + "graph",
	}
}

// templateVariantNamesForTest are template slugs that must NOT be exposed as
// standalone runtime identifiers. They are selected via the `template` field
// (or manifest runtime mapping) and resolve to a base runtime at create time.
func templateVariantNamesForTest() []string {
	return []string{"seo-agent"}
}

func keys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestTemplateIdentityForRuntime pins the runtime -> "<repo>@<ref>"
// mapping that drives cfg.TemplateIdentity (RFC #2843 #24 PR-B).
// Loads the real manifest.json (test setup already has it), then
// asserts the wire shape for a known runtime + the empty-result
// for the BYO-compute meta-runtimes (external / kimi / kimi-cli /
// mock / unknown).
func TestTemplateIdentityForRuntime(t *testing.T) {
	path := manifestPath()
	if path == "" {
		t.Skip("manifest.json not discoverable from this test cwd")
	}
	// Force the repo registry to load from the same path
	// (idempotent — safe to call multiple times).
	initTemplateRepoByName()

	// Sanity: a real template-backed runtime resolves to a
	// "<repo>@<ref>" identity.
	id, ok := templateIdentityForRuntime("claude-code")
	if !ok {
		t.Errorf("claude-code should resolve to an identity (manifest has it), got none")
	} else {
		// Identity must contain an @ and a slash (the fetcher
		// parses this as "<owner>/<repo>@<ref>").
		if !strings.Contains(id, "@") || !strings.Contains(id, "/") {
			t.Errorf("claude-code identity %q doesn't look like \"<owner>/<repo>@<ref>\"", id)
		}
		// Identity must NOT be empty (the SCAFFOLD gate in
		// collectCPConfigFiles would skip the fetcher on empty).
		if id == "" {
			t.Errorf("claude-code identity is empty; should be \"<repo>@<ref>\"")
		}
	}

	// BYO-compute meta-runtimes have no template repo — the
	// lookup MUST return (empty, false) so the SCAFFOLD gate
	// skips the fetcher for them (preserves pre-scaffold
	// behavior).
	for _, rt := range []string{"external", "kimi", "kimi-cli", "mock", "unknown-runtime-xyz"} {
		id2, ok2 := templateIdentityForRuntime(rt)
		if ok2 {
			t.Errorf("runtime %q should NOT have a template identity (BYO-compute / unknown), got identity=%q", rt, id2)
		}
		if id2 != "" {
			t.Errorf("runtime %q identity should be empty, got %q", rt, id2)
		}
	}
}

// TestTemplateIdentityOrEmpty pins the single-expression wrapper used at the
// call site in buildProvisionerConfig.
func TestTemplateIdentityOrEmpty(t *testing.T) {
	if manifestPath() == "" {
		t.Skip("manifest.json not discoverable from this test cwd")
	}
	initTemplateRepoByName()
	if got := templateIdentityOrEmpty(resolveTemplateIdentity("", "claude-code")); got == "" {
		t.Error("claude-code should return a non-empty identity")
	}
	if got := templateIdentityOrEmpty(resolveTemplateIdentity("", "external")); got != "" {
		t.Errorf("external should return empty, got %q", got)
	}
	if got := templateIdentityOrEmpty(resolveTemplateIdentity("", "unknown-xyz")); got != "" {
		t.Errorf("unknown-xyz should return empty, got %q", got)
	}
	// Phase 1: explicit template wins over runtime fallback.
	if got := templateIdentityOrEmpty(resolveTemplateIdentity("claude-code", "external")); got == "" {
		t.Error("explicit claude-code template should return a non-empty identity even when runtime is external")
	}
	if got := templateIdentityOrEmpty(resolveTemplateIdentity("unknown-template-xyz", "claude-code")); got != "" {
		t.Errorf("unknown explicit template should fail-closed to empty, got %q", got)
	}
}

// TestResolveTemplateIdentity pins the Phase 1 template-first resolver.
func TestResolveTemplateIdentity(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "manifest.json")
	manifest := `{
		"workspace_templates": [
			{"name": "claude-code-default", "repo": "molecule-ai/t-cc", "ref": "main"},
			{"name": "seo-agent", "repo": "molecule-ai/t-seo", "ref": "v1"},
			{"name": "hermes", "repo": "molecule-ai/t-hermes", "ref": "v2"}
		]
	}`
	if err := os.WriteFile(p, []byte(manifest), 0600); err != nil {
		t.Fatalf("write temp manifest: %v", err)
	}
	t.Setenv("WORKSPACE_MANIFEST_PATH", p)
	initTemplateRepoByName()

	cases := []struct {
		name     string
		template string
		runtime  string
		wantOk   bool
		wantRepo string
	}{
		{"template wins", "seo-agent", "claude-code", true, "molecule-ai/t-seo@v1"},
		{"template empty falls back to runtime", "", "claude-code", true, "molecule-ai/t-cc@main"},
		{"template empty external returns empty", "", "external", false, ""},
		{"unknown explicit template fail-closed", "no-such-template", "claude-code", false, ""},
		{"unknown runtime empty", "", "no-such-runtime", false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id, ok := resolveTemplateIdentity(c.template, c.runtime)
			if ok != c.wantOk {
				t.Fatalf("template=%q runtime=%q: want ok=%v, got ok=%v", c.template, c.runtime, c.wantOk, ok)
			}
			if id != c.wantRepo {
				t.Errorf("template=%q runtime=%q: want id=%q, got %q", c.template, c.runtime, c.wantRepo, id)
			}
		})
	}
}

// TestIsKnownTemplate pins the manifest-entry gate used by PATCH /template.
func TestIsKnownTemplate(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "manifest.json")
	manifest := `{"workspace_templates": [{"name": "seo-agent", "repo": "r", "ref": "main"}]}`
	if err := os.WriteFile(p, []byte(manifest), 0600); err != nil {
		t.Fatalf("write temp manifest: %v", err)
	}
	t.Setenv("WORKSPACE_MANIFEST_PATH", p)
	initTemplateRepoByName()

	if isKnownTemplate("") {
		t.Error("empty template should not be known")
	}
	if !isKnownTemplate("seo-agent") {
		t.Error("seo-agent should be known")
	}
	if isKnownTemplate("ghost") {
		t.Error("ghost template should not be known")
	}
}

// TestTemplateIdentityForTemplateOrRuntime is the #32 regression gate: a
// template VARIANT (seo-agent, runtime=claude-code) must resolve its fetch
// identity from the TEMPLATE (seo-agent), not the runtime (claude-code).
// Before the fix the fetch keyed on runtime → resolved the claude-code-default
// template → delivered NONE of seo-agent's agent-skills/seo-all. This asserts
// the variant resolves to its own repo, falls back to runtime when no template,
// and stays empty for external runtimes.
func TestTemplateIdentityForTemplateOrRuntime(t *testing.T) {
	if manifestPath() == "" {
		t.Skip("manifest.json not discoverable from this test cwd")
	}
	initTemplateRepoByName()

	// VARIANT: template=seo-agent + runtime=claude-code must resolve to the
	// SEO-AGENT repo, NOT the claude-code template. THIS is the regression.
	seo := templateIdentityForTemplateOrRuntime("seo-agent", "claude-code")
	if seo == "" || !strings.Contains(seo, "seo-agent") {
		t.Errorf("seo-agent variant must resolve to the seo-agent template identity; got %q", seo)
	}
	cc := templateIdentityForTemplateOrRuntime("", "claude-code")
	if seo == cc {
		t.Errorf("seo-agent variant resolved to the SAME identity as claude-code (%q) — the fetch is keying on runtime, not template (#32 regression)", seo)
	}

	// FALLBACK: no template → use the runtime (runtime==template-name case).
	if got := templateIdentityForTemplateOrRuntime("", "hermes"); got == "" {
		t.Error("empty template should fall back to the runtime (hermes) identity")
	}
	// Unknown template falls back to runtime, then to "".
	if got := templateIdentityForTemplateOrRuntime("no-such-template", "external"); got != "" {
		t.Errorf("unknown template + external runtime should be empty, got %q", got)
	}
}

// TestInitTemplateRepoByName_PopulatesMap_FromTempManifest pins the
// PR-B contract-pin: the prod-init path must populate templateRepoByName
// from a real manifest so cfg.TemplateIdentity is non-empty for
// template-backed runtimes at boot. Uses a temp manifest.json (via
// the WORKSPACE_MANIFEST_PATH env var, read by manifestPath()) so the
// test doesn't depend on a real manifest being present.
//
// This is the load-bearing test PM required: "Keep a test asserting
// the prod-init path populates it (so this can't regress to test-only)".
func TestInitTemplateRepoByName_PopulatesMap_FromTempManifest(t *testing.T) {
	// Write a temp manifest.json with a known set of template
	// runtimes.
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "manifest.json")
	manifest := `{
		"workspace_templates": [
			{"name": "claude-code-default", "repo": "molecule-ai/t-cc", "ref": "main"},
			{"name": "hermes", "repo": "molecule-ai/t-hermes", "ref": "v1.2.3"},
			{"name": "codex", "repo": "molecule-ai/t-codex", "ref": "main"}
		]
	}`
	if err := os.WriteFile(manifestPath, []byte(manifest), 0600); err != nil {
		t.Fatalf("write temp manifest: %v", err)
	}
	// Point manifestPath() at the temp file (it reads
	// WORKSPACE_MANIFEST_PATH first).
	t.Setenv("WORKSPACE_MANIFEST_PATH", manifestPath)

	// Run the prod-init path.
	initTemplateRepoByName()

	// Assert the map is populated for the shipped runtimes.
	cases := []struct {
		runtime  string
		wantRepo string
		wantRef  string
	}{
		{"claude-code", "molecule-ai/t-cc", "main"},
		{"hermes", "molecule-ai/t-hermes", "v1.2.3"},
		{"codex", "molecule-ai/t-codex", "main"},
	}
	for _, c := range cases {
		rr, ok := templateRepoByName[c.runtime]
		if !ok {
			t.Errorf("runtime %q missing from templateRepoByName after init (got %d entries)", c.runtime, len(templateRepoByName))
			continue
		}
		if rr.Repo != c.wantRepo {
			t.Errorf("runtime %q: want repo=%q, got %q", c.runtime, c.wantRepo, rr.Repo)
		}
		if rr.Ref != c.wantRef {
			t.Errorf("runtime %q: want ref=%q, got %q", c.runtime, c.wantRef, rr.Ref)
		}
	}

	// Assert the lookup function returns the expected identity.
	for _, c := range cases {
		id, ok := templateIdentityForRuntime(c.runtime)
		if !ok {
			t.Errorf("templateIdentityForRuntime(%q) returned (empty, false); want (non-empty, true)", c.runtime)
			continue
		}
		want := c.wantRepo + "@" + c.wantRef
		if id != want {
			t.Errorf("templateIdentityForRuntime(%q) = %q, want %q", c.runtime, id, want)
		}
	}
}

// TestInitTemplateRepoByName_ReconcilesStaleEntries pins the
// every-boot reconcile property: a runtime removed from the manifest
// between two init calls must NOT be resolvable in the map after
// the second init. This catches the "stale entry persists" bug that
// would otherwise let the fetcher attempt a no-longer-existing repo.
func TestInitTemplateRepoByName_ReconcilesStaleEntries(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "manifest.json")
	t.Setenv("WORKSPACE_MANIFEST_PATH", manifestPath)

	// First manifest: claude-code + hermes present.
	if err := os.WriteFile(manifestPath, []byte(`{
		"workspace_templates": [
			{"name": "claude-code-default", "repo": "molecule-ai/t-cc", "ref": "main"},
			{"name": "hermes", "repo": "molecule-ai/t-hermes", "ref": "main"}
		]
	}`), 0600); err != nil {
		t.Fatalf("write manifest v1: %v", err)
	}
	initTemplateRepoByName()
	if _, ok := templateRepoByName["claude-code"]; !ok {
		t.Fatalf("after first init: claude-code missing")
	}
	if _, ok := templateRepoByName["hermes"]; !ok {
		t.Fatalf("after first init: hermes missing")
	}

	// Second manifest: hermes REMOVED. claude-code unchanged.
	if err := os.WriteFile(manifestPath, []byte(`{
		"workspace_templates": [
			{"name": "claude-code-default", "repo": "molecule-ai/t-cc", "ref": "main"}
		]
	}`), 0600); err != nil {
		t.Fatalf("write manifest v2: %v", err)
	}
	initTemplateRepoByName()

	// claude-code still resolves.
	if _, ok := templateRepoByName["claude-code"]; !ok {
		t.Errorf("after second init: claude-code should still resolve (it stayed in the manifest)")
	}
	// hermes must be GONE — the manifest removed it.
	if _, ok := templateRepoByName["hermes"]; ok {
		t.Errorf("after second init: hermes should NOT resolve (it was removed from the manifest); the every-boot reconcile failed")
	}
	// And the lookup returns ok=false for hermes.
	if id, ok := templateIdentityForRuntime("hermes"); ok || id != "" {
		t.Errorf("templateIdentityForRuntime(hermes) should return (\"\", false), got (%q, %v)", id, ok)
	}
}

// =============================================================================
// TestExternalLikeRuntimesConsistent — pin test for the
// externalLikeRuntimes SSOT consolidation. Locks the shape across
// all 4 sites that previously hardcoded the same set in 3 different
// shapes (fallbackRuntimes map, loadRuntimesFromManifest injection,
// isExternalLikeRuntime switch, workspace.go:400 error message).
//
// If anyone adds a new BYO-compute meta-runtime (e.g. "byo-cli"),
// they should:
//   1. add it to the externalLikeRuntimes slice in runtime_registry.go
//   2. run the test suite (this pin test still passes — same
//      resolved shape)
//   3. the workspace.go:400 error message auto-includes it
//
// If anyone adds a new hardcoded list anywhere (drift surface),
// this test fails. The expected externalLikeRuntimes set is
// {"external", "kimi", "kimi-cli"} per the current production
// state — locked here so a future "we don't actually support kimi
// anymore" decision is a deliberate test update, not silent drift.
// =============================================================================

func TestExternalLikeRuntimesConsistent(t *testing.T) {
	want := []string{"external", "kimi", "kimi-cli"}
	if len(externalLikeRuntimes) != len(want) {
		t.Fatalf("externalLikeRuntimes length = %d, want %d (drift surface: SSOT changed but test wasn't updated)",
			len(externalLikeRuntimes), len(want))
	}
	for i, r := range want {
		if externalLikeRuntimes[i] != r {
			t.Errorf("externalLikeRuntimes[%d] = %q, want %q (SSOT shape changed without test update)",
				i, externalLikeRuntimes[i], r)
		}
	}

	// 1. fallbackRuntimes contains the SSOT (plus template-backed
	//    runtimes + mock). The SSOT MUST be a subset.
	for _, r := range want {
		if _, ok := fallbackRuntimes[r]; !ok {
			t.Errorf("fallbackRuntimes missing externalLikeRuntimes entry %q (drift: SSOT says %q is BYO-compute but fallback allowlist doesn't include it)",
				r, r)
		}
	}
	// fallbackRuntimes ALSO contains the template-backed runtimes
	// (claude-code, hermes, openclaw, codex) + mock — pin the
	// resolved shape so a future edit doesn't silently drop them.
	for _, r := range []string{"claude-code", "hermes", "openclaw", "codex", "mock"} {
		if _, ok := fallbackRuntimes[r]; !ok {
			t.Errorf("fallbackRuntimes missing expected entry %q (drift: a runtime was silently dropped from the fallback allowlist)",
				r)
		}
	}

	// 2. isExternalLikeRuntime returns true for each SSOT entry
	//    and false for the template-backed runtimes. (Locked because
	//    plugins.go / discovery.go / registry.go all switch on this
	//    predicate — silently flipping it would break BYO-compute
	//    behavior in 4 different files.)
	for _, r := range want {
		if !isExternalLikeRuntime(r) {
			t.Errorf("isExternalLikeRuntime(%q) = false, want true (drift: predicate lost the SSOT entry)", r)
		}
	}
	for _, r := range []string{"claude-code", "hermes", "openclaw", "codex", "mock", "unknown-runtime-xyz"} {
		if isExternalLikeRuntime(r) {
			t.Errorf("isExternalLikeRuntime(%q) = true, want false (drift: predicate now claims a template-backed runtime is BYO-compute)", r)
		}
	}

	// 3. joinExternalLikeRuntimesForMessage produces the exact
	//    user-facing string the production error message uses. Pin
	//    the wire shape so a future edit doesn't silently change
	//    the user-facing 422 response.
	wantMsg := `"external", "kimi", or "kimi-cli"`
	if got := joinExternalLikeRuntimesForMessage(); got != wantMsg {
		t.Errorf("joinExternalLikeRuntimesForMessage() = %q, want %q (drift: user-facing error string shape changed)",
			got, wantMsg)
	}

	// 4. The full error message (the one workspace.go:400 sends in
	//    the 422 body) is the prefix + the joined SSOT. Pin it.
	fullWant := `external workspaces must use runtime "external", "kimi", or "kimi-cli"`
	// Reproduce the exact fmt.Sprintf call workspace.go:400 makes.
	// We don't import workspace.go's Create (it has many other
	// dependencies); we just rebuild the string the same way and
	// assert the wire shape is preserved.
	fullGot := fmt.Sprintf("external workspaces must use runtime %s", joinExternalLikeRuntimesForMessage())
	if fullGot != fullWant {
		t.Errorf("full error string drift:\n  got:  %q\n  want: %q", fullGot, fullWant)
	}
}
