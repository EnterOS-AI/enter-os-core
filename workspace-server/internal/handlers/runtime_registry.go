package handlers

// runtime_registry.go — single source of truth for "which runtime
// strings is the provisioner willing to honor".
//
// Before this file, knownRuntimes was a hardcoded Go map in
// workspace_provision.go, kept in sync MANUALLY with both
// workspace/build-all.sh and manifest.json's workspace_templates.
// That drift produced two visible bugs:
//
//   - "gemini-cli" existed in manifest.json but not the Go map, so
//     the UI/workspace-create rejected it and fell back to langgraph.
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
	for _, p := range []string{"manifest.json", "../manifest.json", "../../manifest.json"} {
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
	Name string `json:"name"`
	Repo string `json:"repo"`
}

type manifestFile struct {
	WorkspaceTemplates []manifestEntry `json:"workspace_templates"`
}

// fallbackRuntimes is used when manifest.json can't be loaded. Keeps
// tests + dev containers working even if the file isn't mounted.
// Kept slightly broader than the original hardcoded map so a stale
// manifest doesn't silently drop a runtime that was previously
// supported in the wild. "external" is always a valid runtime —
// manifest or not — because it has no template repo.
var fallbackRuntimes = map[string]struct{}{
	"langgraph":   {},
	"claude-code": {},
	"openclaw":    {},
	"crewai":      {},
	"autogen":     {},
	"deepagents":  {},
	"hermes":      {},
	"codex":       {},
	"gemini-cli":  {},
	"external":    {},
}

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
	out := map[string]struct{}{
		// external is ALWAYS available — it has no template repo, so
		// the manifest doesn't know about it. Injected here so we
		// don't need a special-case in every caller.
		"external": {},
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
		name = strings.TrimSuffix(name, "-default")
		out[name] = struct{}{}
	}
	return out, nil
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
