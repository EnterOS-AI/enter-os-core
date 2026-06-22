package provisioner

// template_assets_test.go — generic template-asset channel tests
// (RFC #2843 #24). Verifies:
//   1. The fetcher wires into collectCPConfigFiles when both
//      cfg.TemplateAssetFetcher and cfg.TemplateIdentity are set.
//   2. Fetched assets land in the SEPARATE TemplateAssets field
//      (NOT merged into ConfigFiles) — the transport split that
//      Reviewer-CR2 addendum called out as the load-bearing fix
//      (a fetcher must not push 716 KiB skills through the 256 KiB
//      SM-bound ConfigFiles).
//   3. Every key in the fetcher's output is gated by
//      IsCPTemplateAssetPath. Paths outside the template-asset
//      allowlist (config.yaml / prompts/* only — agent-skills are
//      plugins now, RFC#2843 #32) ABORT the provision — Reviewer-CR2
//      RC #11690's load-bearing blast-radius guard.
//   4. A transport error on the fetcher ABORTS the provision
//      (fail-closed; never regresses to stub /configs).
//   5. Nil fetcher = no-op (self-host default; the existing
//      TemplatePath local-dir path still works).
//   6. Empty TemplateIdentity with a non-nil fetcher = no-op
//      (the fetcher is only called when there's an identity
//      to resolve).

import (
	"context"
	"encoding/base64"
	"errors"
	"runtime"
	"strings"
	"testing"
)

// fakeTemplateAssetFetcher is a capture-only stub satisfying
// TemplateAssetFetcher. Returns the configured bundle+err;
// records the template identities the handler asked for.
type fakeTemplateAssetFetcher struct {
	bundle map[string][]byte
	err    error
	calls  []string
}

func (f *fakeTemplateAssetFetcher) Load(_ context.Context, templateIdentity string) (map[string][]byte, error) {
	f.calls = append(f.calls, templateIdentity)
	return f.bundle, f.err
}

// TestCollectCPConfigFiles_MergesFetcherAssets is the
// happy path: fetcher returns assets, the assets land in
// TemplateAssets (the SEPARATE wire field — the transport
// split per Reviewer-CR2 addendum). Fails if a future
// refactor stops calling cfg.TemplateAssetFetcher.Load
// from collectCPConfigFiles when the fetcher + identity
// are set, or re-merges assets into ConfigFiles.
func TestCollectCPConfigFiles_MergesFetcherAssets(t *testing.T) {
	prov := &fakeTemplateAssetFetcher{
		bundle: map[string][]byte{
			"config.yaml":       []byte("# from template repo"),
			"prompts/system.md": []byte("# template system prompt"),
		},
	}
	cfg := WorkspaceConfig{
		TemplateIdentity:     "seo-agent-v1.2.3",
		TemplateAssetFetcher: prov,
	}

	files, assets, err := collectCPConfigFiles(cfg)
	if err != nil {
		t.Fatalf("collectCPConfigFiles: %v", err)
	}
	// The fetched assets land in TemplateAssets (the
	// non-secret transport), NOT in ConfigFiles (the SM-bound
	// bundle). The transport split is the load-bearing fix.
	// NB (RFC#2843 #32): agent-skills/* are NOT carried on this
	// channel anymore — skills are plugins. Only config.yaml +
	// prompts/* are asset-eligible.
	wantKeys := []string{
		"config.yaml",
		"prompts/system.md",
	}
	for _, wk := range wantKeys {
		if _, ok := assets[wk]; !ok {
			t.Errorf("expected %q in TemplateAssets, got keys: %v", wk, keysOfAssetMap(assets))
		}
		if _, ok := files[wk]; ok {
			t.Errorf("did NOT expect %q in ConfigFiles (transport split — TemplateAssets is the non-secret channel)", wk)
		}
	}
	// Fetcher was called once with the right identity.
	if len(prov.calls) != 1 || prov.calls[0] != "seo-agent-v1.2.3" {
		t.Errorf("expected one Load call with identity=seo-agent-v1.2.3, got calls=%v", prov.calls)
	}
}

// TestCollectCPConfigFiles_RejectsAgentSkillsAsset is the RFC#2843 #32
// regression: a fetcher that returns an agent-skills/* path must ABORT the
// provision (skills are plugins now, NOT asset-channel eligible). Before #32
// the allowlist admitted agent-skills/* and the ~716 KiB seo-all tree got
// pulled into the provision payload, which fail-closed BEFORE the CP was ever
// called. Guards against re-adding agent-skills/* to IsCPTemplateAssetPath.
func TestCollectCPConfigFiles_RejectsAgentSkillsAsset(t *testing.T) {
	prov := &fakeTemplateAssetFetcher{
		bundle: map[string][]byte{
			"config.yaml":                     []byte("# from template repo"),
			"agent-skills/seo-audit/SKILL.md": []byte("# 716 KiB skill tree does not belong here"),
		},
	}
	cfg := WorkspaceConfig{
		TemplateIdentity:     "seo-agent-v1.2.3",
		TemplateAssetFetcher: prov,
	}

	_, _, err := collectCPConfigFiles(cfg)
	if err == nil {
		t.Fatal("expected collectCPConfigFiles to abort when fetcher returns an agent-skills/* path, got nil")
	}
	if !stringsContains(err.Error(), "allowlist") {
		t.Errorf("expected the reject error to mention the allowlist, got: %v", err)
	}
}

// TestCollectCPConfigFiles_CallerWinsOnConfigFiles asserts
// the caller's cfg.ConfigFiles entry lands in the ConfigFiles
// field (the SM-bound bundle) even when a fetcher is wired.
// The two fields are independent transports — caller-provided
// files are still the SM-bound bundle (small non-secret config
// text only), fetcher-provided assets ride the non-secret
// TemplateAssets field.
func TestCollectCPConfigFiles_CallerWinsOnConfigFiles(t *testing.T) {
	prov := &fakeTemplateAssetFetcher{
		bundle: map[string][]byte{
			"config.yaml": []byte("# from template repo"),
		},
	}
	cfg := WorkspaceConfig{
		TemplateIdentity:     "seo-agent-v1.2.3",
		TemplateAssetFetcher: prov,
		ConfigFiles: map[string][]byte{
			"config.yaml": []byte("# caller override"),
		},
	}

	files, assets, err := collectCPConfigFiles(cfg)
	if err != nil {
		t.Fatalf("collectCPConfigFiles: %v", err)
	}
	// Caller's ConfigFiles["config.yaml"] is base64-encoded into
	// the SM-bound ConfigFiles field. The fetcher's same-key
	// result lands in TemplateAssets (separate transport) — no
	// conflict because they're on different fields.
	decoded, decErr := base64.StdEncoding.DecodeString(files["config.yaml"])
	if decErr != nil {
		t.Fatalf("decode config.yaml from ConfigFiles: %v", decErr)
	}
	if string(decoded) != "# caller override" {
		t.Errorf("expected caller override in ConfigFiles, got %q", string(decoded))
	}
	if got := string(assets["config.yaml"]); got != "# from template repo" {
		t.Errorf("expected fetcher asset in TemplateAssets, got %q", got)
	}
}

// TestCollectCPConfigFiles_FetcherErrorAborts is the
// fail-closed assertion: a transport error from the fetcher
// must abort the provision rather than regressing to stub
// /configs (the same fail-closed contract as the
// persisted-bundle provider in #2831 PIECE 1). If a future
// refactor swallows the fetch error, the bundle would
// silently miss the config.yaml + prompts files and the
// workspace would boot with a stub config — the exact
// regression this test guards against.
func TestCollectCPConfigFiles_FetcherErrorAborts(t *testing.T) {
	prov := &fakeTemplateAssetFetcher{err: errors.New("gitea 503")}
	cfg := WorkspaceConfig{
		TemplateIdentity:     "seo-agent-v1.2.3",
		TemplateAssetFetcher: prov,
	}

	_, _, err := collectCPConfigFiles(cfg)
	if err == nil {
		t.Fatal("expected collectCPConfigFiles to abort on fetcher error, got nil")
	}
	if !stringsContains(err.Error(), "gitea 503") {
		t.Errorf("expected error to surface the underlying gitea 503, got: %v", err)
	}
}

// TestCollectCPConfigFiles_NilFetcherIsNoop asserts the
// self-host default: a WorkspaceConfig without a fetcher
// does NOT call anything (the existing TemplatePath +
// ConfigFiles path is unchanged). The RFC's opt-in
// contract: nil fetcher = no asset channel, no behavior
// change for self-host callers.
func TestCollectCPConfigFiles_NilFetcherIsNoop(t *testing.T) {
	cfg := WorkspaceConfig{
		TemplateIdentity: "seo-agent-v1.2.3", // set but no fetcher wired
	}
	_, _, err := collectCPConfigFiles(cfg)
	if err != nil {
		t.Fatalf("collectCPConfigFiles with nil fetcher: %v", err)
	}
}

// TestCollectCPConfigFiles_EmptyIdentityNoop asserts that
// even a wired fetcher is NOT called when TemplateIdentity is
// empty (the Gitea fetcher needs an identity to resolve; an
// empty identity would be a programming error and should
// be a no-op rather than a fetch-with-empty-identity call).
func TestCollectCPConfigFiles_EmptyIdentityNoop(t *testing.T) {
	prov := &fakeTemplateAssetFetcher{
		bundle: map[string][]byte{"config.yaml": []byte("# unexpected")},
	}
	cfg := WorkspaceConfig{
		TemplateIdentity:     "",   // empty
		TemplateAssetFetcher: prov, // wired but no identity
	}
	_, _, err := collectCPConfigFiles(cfg)
	if err != nil {
		t.Fatalf("collectCPConfigFiles with empty identity: %v", err)
	}
	if len(prov.calls) != 0 {
		t.Errorf("expected fetcher NOT to be called with empty identity, got calls=%v", prov.calls)
	}
}

// --- Blast-radius allowlist tests (Reviewer-CR2 RC #11690) ---

// TestIsCPTemplateAssetPath_AllowsConfigYaml pins the happy
// path: a fetcher returning "config.yaml" passes the
// allowlist.
func TestIsCPTemplateAssetPath_AllowsConfigYaml(t *testing.T) {
	if !IsCPTemplateAssetPath("config.yaml") {
		t.Error("expected config.yaml to be allowed")
	}
}

// TestIsCPTemplateAssetPath_AllowsPromptsPrefix pins the
// prompts/* namespace. Note: "prompts/" (with trailing slash)
// is normalized to "prompts" by filepath.Clean, and the
// allowlist requires the "prompts/" prefix on a non-empty
// path — the literal "prompts" string is rejected (it's a
// directory name, not a file). The test pins file-shaped
// paths under prompts/.
func TestIsCPTemplateAssetPath_AllowsPromptsPrefix(t *testing.T) {
	for _, ok := range []string{"prompts/system.md", "prompts/sub/foo.md"} {
		if !IsCPTemplateAssetPath(ok) {
			t.Errorf("expected %q to be allowed", ok)
		}
	}
}

// TestIsCPTemplateAssetPath_RejectsAgentSkillsPrefix pins the RFC#2843 #32
// contract: agent-skills/* is NO LONGER asset-channel eligible. Skills are
// PLUGINS now — they install dynamically post-online via the plugin pipeline
// (the gitea:// resolver reads agent-skills/<skill> from the template repo at
// install time), NOT through the provisioning-time asset channel. Keeping
// agent-skills/* in the allowlist re-created the #32 fresh-seo-agent provision
// failure (the ~716 KiB seo-all tree pulled into the provision payload).
func TestIsCPTemplateAssetPath_RejectsAgentSkillsPrefix(t *testing.T) {
	for _, bad := range []string{
		"agent-skills/seo-audit/SKILL.md",
		"agent-skills/seo-audit/manifest.yaml",
		"agent-skills/index.json",
		"agent-skills/seo-all/SKILL.md",
	} {
		if IsCPTemplateAssetPath(bad) {
			t.Errorf("expected %q to be REJECTED (agent-skills are plugins now, RFC#2843 #32)", bad)
		}
	}
}

// TestIsCPTemplateAssetPath_RejectsMemoryMd pins the
// curated-memory exclusion. MEMORY.md is agent-owned
// durable state — reconciled by the boot entrypoint, NOT
// by the provision path.
func TestIsCPTemplateAssetPath_RejectsMemoryMd(t *testing.T) {
	if IsCPTemplateAssetPath("MEMORY.md") {
		t.Error("MEMORY.md must NOT be allowed (agent-owned curated memory, reconciled by boot entrypoint)")
	}
}

// TestIsCPTemplateAssetPath_RejectsUserMd pins the
// curated-memory exclusion for USER.md.
func TestIsCPTemplateAssetPath_RejectsUserMd(t *testing.T) {
	if IsCPTemplateAssetPath("USER.md") {
		t.Error("USER.md must NOT be allowed (agent-owned curated memory, reconciled by boot entrypoint)")
	}
}

// TestIsCPTemplateAssetPath_RejectsClaudeMd pins the
// runtime-memory exclusion. CLAUDE.md is the runtime's
// memory file (Claude Code reads it at session start).
func TestIsCPTemplateAssetPath_RejectsClaudeMd(t *testing.T) {
	if IsCPTemplateAssetPath("CLAUDE.md") {
		t.Error("CLAUDE.md must NOT be allowed (runtime memory file, agent-owned state)")
	}
}

// TestIsCPTemplateAssetPath_RejectsClaudeSessionsPath
// pins the Claude Code session-dir exclusion. Sessions
// live on their own volume; pushing them into the
// template-asset channel would clobber or duplicate them.
func TestIsCPTemplateAssetPath_RejectsClaudeSessionsPath(t *testing.T) {
	for _, bad := range []string{
		".claude/sessions/abc.json",
		".claude/sessions",
		".claude/settings.json",
	} {
		if IsCPTemplateAssetPath(bad) {
			t.Errorf("%q must NOT be allowed (Claude Code agent-owned state)", bad)
		}
	}
}

// TestIsCPTemplateAssetPath_RejectsAbsoluteAndTraversal
// pins the path-shape guards. The function applies
// filepath.Clean + filepath.ToSlash before matching, so
// ".." and absolute paths normalize to non-matching
// shapes. The addFile/addAsset callsite separately
// rejects traversal sequences; this is a belt-and-braces
// assertion that the allowlist itself doesn't admit
// weird shapes.
func TestIsCPTemplateAssetPath_RejectsAbsoluteAndTraversal(t *testing.T) {
	for _, bad := range []string{
		"../etc/passwd",
		"prompts/../secrets",
		"/etc/passwd",
		"..",
		".",
		"",
	} {
		if IsCPTemplateAssetPath(bad) {
			t.Errorf("%q must NOT be allowed", bad)
		}
	}
}

// TestIsCPTemplateAssetPath_NormalizesSlashes pins the
// Windows-separator normalization for the file path
// pre-processing. On Windows, filepath.ToSlash converts
// backslashes to forward slashes BEFORE the match, so a
// fetcher returning "prompts\\system.md" is treated as
// "prompts/system.md" (allowed). On Linux/macOS, the
// backslash is a valid filename character (not a
// separator), so the same input would be rejected as a
// literal filename. This is intentional — the
// normalization matches the OS's notion of a path
// separator, so a malicious fetcher can't smuggle a
// backslash-as-separator past the allowlist on Windows
// by relying on case-insensitive or platform-specific
// matching.
//
// Test executes the normalization contract on whatever
// host runs the test: a path that already uses forward
// slashes (the Linux/macOS case) must pass.
func TestIsCPTemplateAssetPath_NormalizesSlashes(t *testing.T) {
	if !IsCPTemplateAssetPath("prompts/system.md") {
		t.Error("expected forward-slash path to pass the allowlist (the canonical case)")
	}
	// Backslashes: only normalize to slashes on Windows.
	// On Linux, a backslash is part of the literal name and
	// is rejected (which is the correct behavior — a
	// fetcher returning Windows-style paths on a Linux host
	// is either a misconfigured fetcher or an attack, and
	// either way failing closed is the safe response).
	if runtime.GOOS == "windows" {
		if !IsCPTemplateAssetPath(`prompts\system.md`) {
			t.Error("expected backslash-separated path to normalize and pass the allowlist on Windows")
		}
	} else {
		if IsCPTemplateAssetPath(`prompts\system.md`) {
			t.Error("backslash is a literal character on non-Windows hosts and must NOT be treated as a path separator by the allowlist")
		}
	}
}

// TestCollectCPConfigFiles_RejectsFetcherAssetOutsideAllowlist
// is the load-bearing test for RC #11690. A fetcher that
// returns a path outside the template-asset allowlist
// (MEMORY.md / USER.md / CLAUDE.md / .claude/sessions/*)
// MUST abort the provision. If a future refactor weakens
// the addAsset gate, this test catches the regression.
func TestCollectCPConfigFiles_RejectsFetcherAssetOutsideAllowlist(t *testing.T) {
	cases := []struct {
		name        string
		badKey      string
		expectedSub string
	}{
		{"MEMORY.md", "MEMORY.md", "MEMORY.md"},
		{"USER.md", "USER.md", "USER.md"},
		{"CLAUDE.md", "CLAUDE.md", "CLAUDE.md"},
		{"claude-sessions", ".claude/sessions/abc.json", ".claude/sessions/abc.json"},
		{"absolute-path", "/etc/passwd", "/etc/passwd"},
		{"adapter.py", "adapter.py", "adapter.py"},
		{"Dockerfile", "Dockerfile", "Dockerfile"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			prov := &fakeTemplateAssetFetcher{
				bundle: map[string][]byte{c.badKey: []byte("hostile content")},
			}
			cfg := WorkspaceConfig{
				TemplateIdentity:     "seo-agent-v1.2.3",
				TemplateAssetFetcher: prov,
			}
			_, _, err := collectCPConfigFiles(cfg)
			if err == nil {
				t.Fatalf("expected provision to abort on fetcher-returned path %q outside template-asset allowlist, got nil error", c.badKey)
			}
			if !stringsContains(err.Error(), c.expectedSub) {
				t.Errorf("expected error to mention %q, got: %v", c.expectedSub, err)
			}
		})
	}
}

// TestCollectCPConfigFiles_FetcherAssetsBase64EncodedOnWire
// pins the wire-format invariant for the new TemplateAssets
// field: assets travel base64-encoded over JSON (same as
// ConfigFiles), to avoid JSON escaping issues with binary
// content (manifests, SKILL.md files, etc.). The collect
// function returns the raw bytes; the marshal step in
// Start does the encoding. This test documents the
// invariant at the collect boundary.
func TestCollectCPConfigFiles_FetcherAssetsRawBytes(t *testing.T) {
	prov := &fakeTemplateAssetFetcher{
		bundle: map[string][]byte{
			"config.yaml":          []byte("# raw bytes, will be base64 by marshaler"),
			"prompts/seo-agent.md": []byte("raw-prompt"),
		},
	}
	cfg := WorkspaceConfig{
		TemplateIdentity:     "seo-agent-v1.2.3",
		TemplateAssetFetcher: prov,
	}
	_, assets, err := collectCPConfigFiles(cfg)
	if err != nil {
		t.Fatalf("collectCPConfigFiles: %v", err)
	}
	if got := string(assets["config.yaml"]); got != "# raw bytes, will be base64 by marshaler" {
		t.Errorf("expected raw bytes in TemplateAssets, got %q (encoding happens at marshal time, not at collect time)", got)
	}
	if got := string(assets["prompts/seo-agent.md"]); got != "raw-prompt" {
		t.Errorf("expected raw prompt bytes in TemplateAssets, got %q", got)
	}
}

// TestCollectCPConfigFiles_NoAssetsWhenNoFetcher pins the
// non-fetcher case: when no fetcher is wired (self-host
// default), the TemplateAssets field is nil/empty. A
// future refactor that always populates TemplateAssets
// (even with empty data) would inflate the wire payload
// for every self-host workspace — this test catches that.
func TestCollectCPConfigFiles_NoAssetsWhenNoFetcher(t *testing.T) {
	cfg := WorkspaceConfig{
		TemplateIdentity: "seo-agent-v1.2.3",
		// TemplateAssetFetcher is nil
	}
	_, assets, err := collectCPConfigFiles(cfg)
	if err != nil {
		t.Fatalf("collectCPConfigFiles: %v", err)
	}
	if len(assets) != 0 {
		t.Errorf("expected nil/empty TemplateAssets when no fetcher wired, got %d entries: %v", len(assets), keysOfAssetMap(assets))
	}
}

// TestCollectCPConfigFiles_PreservesCallerConfigFiles pins
// the existing TemplatePath + ConfigFiles path: when a
// fetcher is NOT wired, the SM-bound ConfigFiles field
// behaves exactly as before (TemplatePath walk +
// cfg.ConfigFiles entries). The transport split is
// additive — it doesn't disturb the existing self-host
// path.
func TestCollectCPConfigFiles_PreservesCallerConfigFiles(t *testing.T) {
	cfg := WorkspaceConfig{
		ConfigFiles: map[string][]byte{
			"config.yaml":      []byte("# caller"),
			"generated.secret": []byte("not really a secret"),
		},
	}
	files, assets, err := collectCPConfigFiles(cfg)
	if err != nil {
		t.Fatalf("collectCPConfigFiles: %v", err)
	}
	if _, ok := files["config.yaml"]; !ok {
		t.Error("expected config.yaml in ConfigFiles (caller-provided)")
	}
	if _, ok := files["generated.secret"]; !ok {
		t.Error("expected generated.secret in ConfigFiles (caller-provided)")
	}
	if len(assets) != 0 {
		t.Errorf("expected empty TemplateAssets when no fetcher wired, got %d entries", len(assets))
	}
}

// keysOfAssetMap returns the sorted keys of a map for stable
// test output. Local helper so test output doesn't depend on
// Go's randomized map iteration order.
func keysOfAssetMap(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// stringsContains is a tiny shim so the test doesn't need
// the strings import elsewhere (it does already, but this
// keeps the dependency local). Existence is asserted via
// string comparison.
func stringsContains(s, substr string) bool {
	return strings.Contains(s, substr)
}

// TestCollectCPConfigFiles_AssetsNotBoundBy256KCap is the regression for
// Reviewer-CR2's size-cap RC on head 7bcc3b5f: template ASSETS ride a
// NON-secret channel and must NOT be bound by the 256 KiB SM/config-bundle cap
// (cpConfigFilesMaxBytes). A >256 KiB asset payload must SUCCEED on
// TemplateAssets (bounded only by the far larger cpTemplateAssetsMaxBytes DoS
// guard) while ConfigFiles stays capped.
//
// NB (RFC#2843 #32): agent-skills are NO LONGER asset-eligible (skills are
// plugins now), so this uses a large prompts/* asset — the largest legitimate
// asset payload after the skill tree was removed from the channel. In practice
// the seo-agent asset set is tiny (config.yaml ~8 KiB + prompts ~8 KiB); this
// test only pins that the asset cap is the larger one, not the SM cap.
func TestCollectCPConfigFiles_AssetsNotBoundBy256KCap(t *testing.T) {
	// 716 KiB prompt blob — over the old 256 KiB cap, well under the asset bound.
	big := make([]byte, 716<<10)
	for i := range big {
		big[i] = 'x'
	}
	prov := &fakeTemplateAssetFetcher{
		bundle: map[string][]byte{
			"config.yaml":           []byte("# from template repo"),
			"prompts/big-prompt.md": big,
		},
	}
	cfg := WorkspaceConfig{
		TemplateIdentity:     "seo-agent-v1.2.3",
		TemplateAssetFetcher: prov,
	}

	_, assets, err := collectCPConfigFiles(cfg)
	if err != nil {
		t.Fatalf("a %d-byte prompts asset must succeed on the non-secret asset channel (no SM cap), got error: %v", len(big), err)
	}
	if got := len(assets["prompts/big-prompt.md"]); got != len(big) {
		t.Errorf("expected the full %d-byte prompt in TemplateAssets, got %d", len(big), got)
	}
}

// TestCollectCPConfigFiles_ConfigFilesStillCappedAt256K pins that lifting the
// asset cap did NOT relax the SM/user-data transport limit: the SM-bound
// ConfigFiles bundle keeps its 256 KiB cap (cpConfigFilesMaxBytes).
func TestCollectCPConfigFiles_ConfigFilesStillCappedAt256K(t *testing.T) {
	big := make([]byte, (256<<10)+1)
	for i := range big {
		big[i] = 'y'
	}
	cfg := WorkspaceConfig{
		ConfigFiles: map[string][]byte{"system-prompt.md": big},
	}
	if _, _, err := collectCPConfigFiles(cfg); err == nil {
		t.Fatal("ConfigFiles over 256 KiB must still be rejected (SM/user-data transport cap unchanged)")
	}
}

// TestSelectTemplateAssetFetcher_SaaS_GiteaFetcher covers the SaaS
// selection: the Gitea fetcher is wired (authenticated when the
// token is set, unauthenticated/public when empty — the public-fetch
// activation default for the molecule-ai/* PUBLIC templates).
func TestSelectTemplateAssetFetcher_SaaS_GiteaFetcher(t *testing.T) {
	// With token
	sel := SelectTemplateAssetFetcher(func() bool { return true }, "http://gitea", "the-token")
	if sel.Fetcher == nil {
		t.Fatal("SaaS + token: expected non-nil fetcher")
	}
	if !sel.Authenticated {
		t.Error("SaaS + token: expected Authenticated=true")
	}
	if sel.Mode == "self-host-noop" {
		t.Errorf("SaaS + token: expected non-noop mode, got %q", sel.Mode)
	}
	// Without token (PR-B public-fetch default)
	sel2 := SelectTemplateAssetFetcher(func() bool { return true }, "http://gitea", "")
	if sel2.Fetcher == nil {
		t.Fatal("SaaS + no token: expected non-nil fetcher (public-fetch)")
	}
	if sel2.Authenticated {
		t.Error("SaaS + no token: expected Authenticated=false (public-fetch)")
	}
	if sel2.Mode == "self-host-noop" {
		t.Errorf("SaaS + no token: expected non-noop mode, got %q", sel2.Mode)
	}
}

// TestSelectTemplateAssetFetcher_SelfHost_NoopFetcher covers the
// self-host selection: the no-op fetcher is wired regardless of
// token state (self-host doesn't need an external asset channel).
func TestSelectTemplateAssetFetcher_SelfHost_NoopFetcher(t *testing.T) {
	// With token (token is IGNORED on self-host)
	sel := SelectTemplateAssetFetcher(func() bool { return false }, "http://gitea", "the-token")
	if sel.Fetcher == nil {
		t.Fatal("self-host: expected non-nil fetcher (no-op default)")
	}
	if sel.Authenticated {
		t.Error("self-host: expected Authenticated=false (no-op never sends auth headers)")
	}
	if sel.Mode != "self-host-noop" {
		t.Errorf("self-host: expected Mode=self-host-noop, got %q", sel.Mode)
	}
	// The fetcher's Load must return (nil, nil) — "no assets" signal.
	assets, err := sel.Fetcher.Load(t.Context(), "molecule-ai/workspace-template-seo@main")
	if err != nil {
		t.Errorf("self-host no-op Load: expected nil error, got %v", err)
	}
	if len(assets) != 0 {
		t.Errorf("self-host no-op Load: expected empty map, got %d entries: %v", len(assets), keysOf(assets))
	}
}

// TestSelectTemplateAssetFetcher_NilSaaSCheck_FallsBackToNoop covers
// the safe default: a nil isSaaSTenant closure is treated as
// "not SaaS" (no-op fetcher), so a misconfigured selection never
// accidentally routes a self-host deployment to the real Gitea
// fetcher.
func TestSelectTemplateAssetFetcher_NilSaaSCheck_FallsBackToNoop(t *testing.T) {
	sel := SelectTemplateAssetFetcher(nil, "http://gitea", "the-token")
	if sel.Fetcher == nil {
		t.Fatal("nil isSaaSTenant closure: expected non-nil fetcher (no-op default)")
	}
	if sel.Mode != "self-host-noop" {
		t.Errorf("nil isSaaSTenant closure: expected Mode=self-host-noop, got %q", sel.Mode)
	}
}
