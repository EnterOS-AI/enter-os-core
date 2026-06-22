package handlers

// template_plugins.go — read a workspace template's `plugins:` block and
// record the DECLARED plugin set (workspace_declared_plugins) for a workspace
// created directly via WorkspaceHandler.Create. Mirrors the org/import flow
// (org_import.go), so a singly-provisioned workspace lands with the same
// declared rows the org/import path would have produced.
//
// Why this exists (RFC#2843 #32): the post-online reconcile
// (ReconcileWorkspacePlugins) only installs plugins that have
// workspace_declared_plugins rows. recordDeclaredPlugin previously ran ONLY in
// org_import.go, NOT in the single create_workspace path. So a seo-agent
// created via Create (template="seo-agent") got NO declared rows → the
// reconcile no-op'd → the seo-all skill (now a plugin, per #32) never
// installed. This module closes that gap by parsing the template config.yaml's
// `plugins:` block at create time and recording each declared plugin.
//
// CONTRACT (matches org_import.go):
//   - Each `plugins:` entry is a source-contract string (e.g.
//     "gitea://owner/repo/subpath#ref" or a bare local name). The install
//     name is derived from the source via plugins.PluginNameFromSource so the
//     reconcile can diff declared-vs-installed without fetching.
//   - Entries are de-duplicated by raw source (mergePlugins semantics: a
//     single template config.yaml's `plugins:` list is already the "merged"
//     set, so there are no defaults/per-ws halves to union here — but we run
//     it through mergePlugins(nil, list) to apply the same dedup + "!"/"-"
//     opt-out handling the org path uses, keeping the two paths byte-aligned).
//   - recordDeclaredPlugin upserts ON CONFLICT (idempotent across re-creates),
//     so this is safe to call on every Create.
//
// Hostile-template defenses are shared with template_schedules.go: the
// config.yaml is read through the same maxTemplateConfigYAMLBytes LimitReader
// so a YAML anchor-bomb cannot pre-explode memory before unmarshal returns.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/plugins"
)

// maxTemplatePlugins bounds how many plugin entries a single template may
// declare. Generous relative to legitimate use (production templates declare
// 1–3 plugins); the cap exists only so a hostile/buggy template can't enqueue
// an unbounded declared set.
const maxTemplatePlugins = 100

// templateConfigPlugins is the minimal shape parsed from a workspace template's
// config.yaml. Only the top-level `plugins:` block is modelled; the rest of the
// file is opaque to this loader (parsed elsewhere for schedules, runtime_config,
// etc.).
type templateConfigPlugins struct {
	Plugins []string `yaml:"plugins"`
}

// parseTemplatePlugins reads `<templatePath>/config.yaml` and returns its
// `plugins:` block (nil + nil error when the file is absent or the block is
// empty). The file is read through the shared maxTemplateConfigYAMLBytes
// LimitReader. Returns an error only when a present config.yaml fails to read
// or parse — callers should treat that as a template-author bug and continue
// (a broken plugins block must never block workspace provisioning).
func parseTemplatePlugins(templatePath string) ([]string, error) {
	if templatePath == "" {
		return nil, nil
	}
	f, err := os.Open(filepath.Join(templatePath, "config.yaml"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("open template config.yaml: %w", err)
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, maxTemplateConfigYAMLBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read template config.yaml: %w", err)
	}
	if int64(len(data)) > maxTemplateConfigYAMLBytes {
		return nil, fmt.Errorf("template config.yaml exceeds %d-byte cap", maxTemplateConfigYAMLBytes)
	}
	var cfg templateConfigPlugins
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse template config.yaml plugins: %w", err)
	}
	// A single template's `plugins:` list IS the merged set — there are no
	// defaults/per-ws halves to union (that's the org-import-only case). Run it
	// through mergePlugins(nil, list) anyway so dedup + "!"/"-" opt-out handling
	// stays byte-identical to the org path.
	merged := mergePlugins(nil, cfg.Plugins)
	if len(merged) > maxTemplatePlugins {
		return nil, fmt.Errorf("template declares %d plugins; cap is %d", len(merged), maxTemplatePlugins)
	}
	return merged, nil
}

// seedTemplatePlugins records each declared plugin source (workspace_declared_plugins)
// for a Create-provisioned workspace. Returns (recorded, skipped) counts so the
// caller can observe partial-record states. Mirrors the org_import.go loop:
//   - derive the install name via plugins.PluginNameFromSource,
//   - warn on a name collision (two sources collapsing to the same install
//     name — the latter wins via the ON CONFLICT upsert),
//   - recordDeclaredPlugin upserts the (workspace_id, plugin_name, source) row.
//
// Per-entry failures are logged and skipped so one bad entry never blocks the
// rest of the declared set.
func seedTemplatePlugins(ctx context.Context, workspaceID string, sources []string) (recorded, skipped int) {
	seenPluginNames := map[string]string{} // name → first source that claimed it
	for _, pluginSource := range sources {
		pluginName, nameErr := plugins.PluginNameFromSource(pluginSource)
		if nameErr != nil {
			log.Printf("Create %s: skipping plugin %q — cannot derive install name: %v", workspaceID, pluginSource, nameErr)
			skipped++
			continue
		}
		if prevSource, dup := seenPluginNames[pluginName]; dup && prevSource != pluginSource {
			log.Printf("Create %s: WARNING plugin name collision — %q and %q both derive name %q; the latter overwrites the former",
				workspaceID, prevSource, pluginSource, pluginName)
		}
		seenPluginNames[pluginName] = pluginSource
		if recErr := recordDeclaredPlugin(ctx, workspaceID, pluginName, pluginSource); recErr != nil {
			log.Printf("Create %s: failed to record declared plugin %s (%s): %v", workspaceID, pluginName, pluginSource, recErr)
			skipped++
			continue
		}
		recorded++
	}
	return recorded, skipped
}


// parseTemplatePluginsFromBytes parses the  list from raw config.yaml
// bytes — the SaaS sibling of parseTemplatePlugins. On a fresh SaaS tenant the
// template config arrives via the Gitea asset channel (not a local dir), so the
// declared-plugin SSOT is those fetched bytes, not a local templatePath (which
// may be empty or fall back to <runtime>-default and miss the real template's
// plugins:). RFC#2843 #32.
func parseTemplatePluginsFromBytes(data []byte) ([]string, error) {
	if len(data) == 0 {
		return nil, nil
	}
	if int64(len(data)) > maxTemplateConfigYAMLBytes {
		return nil, fmt.Errorf("template config.yaml exceeds %d-byte cap", maxTemplateConfigYAMLBytes)
	}
	var cfg templateConfigPlugins
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse template config.yaml plugins: %w", err)
	}
	merged := mergePlugins(nil, cfg.Plugins)
	if len(merged) > maxTemplatePlugins {
		return nil, fmt.Errorf("template declares %d plugins; cap is %d", len(merged), maxTemplatePlugins)
	}
	return merged, nil
}
