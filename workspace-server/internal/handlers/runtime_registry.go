package handlers

// runtime_registry.go — single source of truth for "which runtime
// strings is the provisioner willing to honor".
//
// Before this file, knownRuntimes was a hardcoded Go map in
// workspace_provision.go, kept in sync MANUALLY with both
// workspace/build-all.sh and manifest.json's workspace_templates.
// That drift produced two visible bugs:
//
//   - a template existed in manifest.json but not the Go map, so
//     the UI/workspace-create rejected it and fell back to claude-code.
//   - "claude-code-default" in manifest vs "claude-code" in Go —
//     operators typing the manifest name got silently coerced.
//
// The fix: read manifest.json at boot. manifest.json lives in the
// monorepo root and is already the declarative registry — adding a
// runtime now means one line in that file + cutting the image.
// The Go allowlist is built from it + the hardcoded "external"
// meta-runtime (which has no template repo — it's a first-class
// "bring your own compute" option).
//
// Fallback: if manifest.json isn't readable (dev container without
// the file, tests without the workspace tree mounted) we fall back
// to the pre-refactor hardcoded list so nothing regresses.

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// manifestPath defaults to the repo root next to the binary. In
// production the workspace-server Dockerfile COPY's manifest.json
// into /app/manifest.json. Override with WORKSPACE_MANIFEST_PATH
// when running from an unusual location.
func manifestPath() string {
	if v := os.Getenv("WORKSPACE_MANIFEST_PATH"); v != "" {
		return v
	}
	// Standard container layout.
	if _, err := os.Stat("/app/manifest.json"); err == nil {
		return "/app/manifest.json"
	}
	// Dev: cwd + ../../manifest.json (run from workspace-server/cmd/server).
	for _, p := range []string{"manifest.json", "../manifest.json", "../../manifest.json", "../../../manifest.json"} {
		if abs, err := filepath.Abs(p); err == nil {
			if _, err := os.Stat(abs); err == nil {
				return abs
			}
		}
	}
	return ""
}

// manifestEntry mirrors the shape of a workspace_templates item.
// Only the fields we read are declared; extras are ignored.
type manifestEntry struct {
	Name    string `json:"name"`
	Repo    string `json:"repo"`
	Ref     string `json:"ref"`
	Runtime string `json:"runtime"` // optional base runtime identifier for template variants (e.g. "seo-agent" → "claude-code")
}

type manifestFile struct {
	WorkspaceTemplates []manifestEntry `json:"workspace_templates"`
}

// joinExternalLikeRuntimesForMessage returns the runtime list as it
// appears in user-facing error messages, e.g. `"external", "kimi", or
// "kimi-cli"`. Oxford-comma style for 3+ items, plain "or" for 2.
// Each item is Go-quoted (with surrounding double quotes) so the
// message reads naturally for an operator typing it.
//
// Used by workspace.go:400 (the "external workspaces must use
// runtime ..." error). Derived from the externalLikeRuntimes SSOT
// so adding a new BYO-compute meta-runtime only requires updating
// the SSOT in one place.
func joinExternalLikeRuntimesForMessage() string {
	quoted := make([]string, len(externalLikeRuntimes))
	for i, r := range externalLikeRuntimes {
		quoted[i] = fmt.Sprintf("%q", r)
	}
	switch len(quoted) {
	case 0:
		return ""
	case 1:
		return quoted[0]
	case 2:
		return quoted[0] + " or " + quoted[1]
	default:
		return strings.Join(quoted[:len(quoted)-1], ", ") + ", or " + quoted[len(quoted)-1]
	}
}

// externalLikeRuntimes is the SINGLE source of truth for the set of
// "BYO-compute meta-runtimes" (operator-managed, no platform-owned
// container or EC2). These runtimes share behavior around
// delivery_mode defaulting, plugin install, restart, and discovery,
// and are always available regardless of what manifest.json says
// (they have no template repo).
//
// Before this constant the same set was hardcoded in 3 separate
// places in this file (fallbackRuntimes, loadRuntimesFromManifest
// injection, isExternalLikeRuntime switch) + 1 string-literal in
// workspace.go:400. Adding a new BYO-compute meta-runtime required
// updating all 4 sites in lockstep; missing one was a silent drift
// surface. The TestExternalLikeRuntimesConsistent pin test in
// runtime_registry_test.go locks the shape across all 4 sites.
//
// "mock" is intentionally NOT in this set — it's a virtual
// workspace with hardcoded canned A2A replies (no container, no
// EC2, no template repo) but it's never user-selected (only the
// funding-demo org uses it), so it doesn't share the BYO-compute
// predicate behavior with external/kimi/kimi-cli.
var externalLikeRuntimes = []string{"external", "kimi", "kimi-cli"}

// fallbackRuntimes is used when manifest.json can't be loaded. Keeps
// tests + dev containers working even if the file isn't mounted.
// Kept slightly broader than the original hardcoded map so a stale
// manifest doesn't silently drop a runtime that was previously
// supported in the wild. "external" is always a valid runtime —
// manifest or not — because it has no template repo.
//
// The 3 externalLikeRuntimes + mock are derived from the SSOT
// (externalLikeRuntimes + the separate "mock" entry) so adding a
// new BYO-compute meta-runtime only requires updating
// externalLikeRuntimes above.
var fallbackRuntimes = func() map[string]struct{} {
	out := map[string]struct{}{
		"claude-code": {},
		"hermes":      {},
		"openclaw":    {},
		"codex":       {},
		// mock — virtual workspace with hardcoded canned A2A replies.
		// No container, no EC2, no template repo. See mock_runtime.go
		// for the full rationale (200-workspace funding-demo org).
		"mock": {},
	}
	for _, r := range externalLikeRuntimes {
		out[r] = struct{}{}
	}
	return out
}()

// loadRuntimesFromManifest builds the runtime allowlist from
// manifest.json. Each workspace_templates[].name is normalized to its
// base runtime identifier (strips the `-default` suffix templates
// use for the "vanilla" variant of their runtime) and added to the
// set. "external" is always injected — it's not a template-backed
// runtime, it's the BYO-compute meta-runtime.
//
// Caller logs + falls back to fallbackRuntimes on any error. Not
// returning the fallback here ourselves so the caller can decide
// how loud to be about the miss (prod = WARN, tests = silent).
func loadRuntimesFromManifest(path string) (map[string]struct{}, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m manifestFile
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	// The 3 externalLikeRuntimes + mock are ALWAYS available
	// regardless of what the manifest contains (they have no
	// template repo, so the manifest doesn't know about them).
	// Injected here from the SSOT (externalLikeRuntimes + the
	// separate "mock" entry) so adding a new BYO-compute
	// meta-runtime only requires updating externalLikeRuntimes
	// above. See TestExternalLikeRuntimesConsistent for the
	// pin test that locks this shape.
	out := map[string]struct{}{
		// mock is ALWAYS available for the same reason as external:
		// virtual workspace, no template repo, never spawns a
		// container. See mock_runtime.go.
		"mock": {},
	}
	for _, r := range externalLikeRuntimes {
		out[r] = struct{}{}
	}
	for _, e := range m.WorkspaceTemplates {
		name := strings.TrimSpace(e.Name)
		if name == "" {
			continue
		}
		// Normalize template-name → runtime-identifier.
		// Convention: "<runtime>-default" is the vanilla variant of
		// <runtime>. Strip the suffix so both `claude-code` and
		// `claude-code-default` resolve to the same runtime.
		// If the manifest entry declares an explicit `runtime`, use it
		// as the base runtime identifier (e.g. the "seo-agent" template
		// is a claude-code variant, not a runtime of its own).
		name = strings.TrimSuffix(name, "-default")
		if strings.TrimSpace(e.Runtime) != "" {
			name = strings.TrimSpace(e.Runtime)
		}
		out[name] = struct{}{}
	}
	return out, nil
}

// isExternalLikeRuntime returns true for runtimes that are BYO-compute
// (operator-managed, no platform-owned container or EC2). These runtimes
// share behavior around delivery_mode defaulting, plugin install, restart,
// and discovery.
//
// The set is derived from the externalLikeRuntimes SSOT (above) so
// adding a new BYO-compute meta-runtime only requires updating
// externalLikeRuntimes in one place — see
// TestExternalLikeRuntimesConsistent for the pin test.
func isExternalLikeRuntime(runtime string) bool {
	for _, r := range externalLikeRuntimes {
		if r == runtime {
			return true
		}
	}
	return false
}

// normalizeExternalRuntime returns the given runtime label if non-empty,
// otherwise falls back to "external". Used when persisting BYO-compute
// workspaces so we don't store an empty runtime string.
func normalizeExternalRuntime(runtime string) string {
	if runtime == "" {
		return "external"
	}
	return runtime
}

// initKnownRuntimes is called from the package init chain (see
// workspace_provision.go var initialization) to replace the
// fallback map with the manifest-derived one. Idempotent —
// safe to call multiple times.
func initKnownRuntimes() {
	path := manifestPath()
	if path == "" {
		log.Printf("runtime registry: manifest.json not found, using fallback allowlist (%d entries)", len(fallbackRuntimes))
		return
	}
	loaded, err := loadRuntimesFromManifest(path)
	if err != nil {
		log.Printf("runtime registry: manifest.json load failed (%v) — using fallback allowlist", err)
		return
	}
	knownRuntimes = loaded
	names := make([]string, 0, len(loaded))
	for k := range loaded {
		names = append(names, k)
	}
	log.Printf("runtime registry: loaded %d runtimes from %s: %v", len(loaded), path, names)
}

// isKnownRuntime reports whether runtime is a recognized workspace runtime
// (first-party template-backed, external-like meta-runtime, or mock).
// Safe to call before initKnownRuntimes — falls back to the compile-time
// fallbackRuntimes map.
func isKnownRuntime(runtime string) bool {
	if _, ok := knownRuntimes[runtime]; ok {
		return true
	}
	// externalLikeRuntimes and mock are always valid, even when the manifest
	// is missing and knownRuntimes is still the fallback set (which already
	// includes them, but re-checking is cheap and explicit).
	if isExternalLikeRuntime(runtime) {
		return true
	}
	if runtime == "mock" {
		return true
	}
	return false
}

// templateRepoRef is the parsed manifest entry needed to
// derive a TemplateIdentity for the Gitea fetcher. The
// identity is "<repo>@<ref>" (the giteaTemplateAssetFetcher
// parses this as "<owner>/<repo>@<ref>").
type templateRepoRef struct {
	Repo string // e.g. "molecule-ai/molecule-ai-workspace-template-claude-code"
	Ref  string // e.g. "main" or a pinned tag/sha
}

// templateRepoByName holds the runtime → (repo, ref) mapping
// parsed from manifest.json at boot. Empty for runtimes that
// have no template repo (external, kimi, kimi-cli, mock —
// caller leaves cfg.TemplateIdentity empty for those, which
// the SCAFFOLD gate in collectCPConfigFiles treats as
// "skip the fetcher", pre-scaffold behavior preserved).
var templateRepoByName = make(map[string]templateRepoRef)

// initTemplateRepoByName is called from the package init chain
// (alongside initKnownRuntimes) to populate the repo map. The
// fallback set returns an empty map — external/kimi/kimi-cli/mock
// have no manifest entry by design.
//
// Reconcile-on-every-boot: the map is RESET at the start of every
// call, then re-populated from the current manifest. Stale entries
// (runtimes removed from the manifest) are dropped; the consumer
// (collectCPConfigFiles + the SCAFFOLD gate) can rely on the map
// being exactly the current manifest's runtimes. The reset is
// critical for the every-boot reconcile semantic — without it,
// dropping a template from the manifest would leave its identity
// resolvable, and the fetcher would attempt a no-longer-existing
// repo. Idempotent for the same manifest input.
func initTemplateRepoByName() {
	path := manifestPath()
	if path == "" {
		log.Printf("template repo registry: manifest.json not found, no template repos available")
		templateRepoByName = make(map[string]templateRepoRef)
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf("template repo registry: manifest.json load failed (%v) — no template repos", err)
		templateRepoByName = make(map[string]templateRepoRef)
		return
	}
	var m manifestFile
	if err := json.Unmarshal(data, &m); err != nil {
		log.Printf("template repo registry: manifest.json parse failed (%v) — no template repos", err)
		templateRepoByName = make(map[string]templateRepoRef)
		return
	}
	// Reconcile: reset, then re-populate. Stale entries from a
	// previous manifest are dropped — see func comment.
	templateRepoByName = make(map[string]templateRepoRef, len(m.WorkspaceTemplates))
	for _, e := range m.WorkspaceTemplates {
		// Same normalization as loadRuntimesFromManifest: strip
		// the "-default" suffix so the runtime identifier is
		// the map key, not the template-variant name.
		name := strings.TrimSuffix(e.Name, "-default")
		templateRepoByName[name] = templateRepoRef{Repo: e.Repo, Ref: e.Ref}
	}
}

// templateIdentityForRuntime returns the Gitea template identity
// for the given runtime name, or "" + false if the runtime has
// no template repo (external / kimi / kimi-cli / mock / unknown).
// The format is "<repo>@<ref>" — the giteaTemplateAssetFetcher
// parses this further as "<owner>/<repo>@<ref>".
func templateIdentityForRuntime(runtime string) (string, bool) {
	rr, ok := templateRepoByName[runtime]
	if !ok {
		return "", false
	}
	return rr.Repo + "@" + rr.Ref, true
}

// isKnownTemplate reports whether name is a registered workspace template in
// manifest.json. The empty string is intentionally NOT known — it is the
// "no installed template" sentinel and falls back to runtime resolution.
func isKnownTemplate(name string) bool {
	if name == "" {
		return false
	}
	_, ok := templateRepoByName[name]
	return ok
}

// resolveTemplateIdentity returns the Gitea template identity (repo@ref) for a
// workspace. If a template is explicitly installed, it wins; otherwise the
// runtime's default template is used. This is the SSOT for the RFC #2843 asset
// fetcher and for the control-plane provision wire.
//
// Fail-closed: an explicitly set but unknown template returns ("", false) so
// callers can surface a 422 instead of silently degrading to the runtime
// fallback (matches the create-boundary posture for unknown runtimes).
func resolveTemplateIdentity(template, runtime string) (string, bool) {
	if template != "" {
		if rr, ok := templateRepoByName[template]; ok {
			return rr.Repo + "@" + rr.Ref, true
		}
		return "", false
	}
	return templateIdentityForRuntime(runtime)
}
