package handlers

// plugins_tracking.go — workspace_plugins DB tracking for the
// version-subscription model (core#113).
//
// Schema lives in migration 20260508160000_workspace_plugins_tracking.up.sql.
// This file is the Go-side write surface used at install time to record
// each plugin's install record. Drift detection / queue / apply are
// follow-up scope (filed as a separate issue once this lands).

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/models"
)

// trackedRefValues is the closed set of bare-string values the
// workspace_plugins.tracked_ref column accepts. Prefixed values
// ("tag:..." / "sha:...") are validated structurally below.
var trackedRefValues = map[string]bool{
	"none": true,
}

// validateTrackedRef returns the canonical form of a track string, or
// an error if the input is malformed. Empty input → "none" (default).
//
// Accepted shapes:
//
//	""                — defaults to "none"
//	"none"            — no tracking
//	"tag:vX.Y.Z"      — track a specific tag
//	"tag:latest"      — track latest tag, drift on every new tag
//	"sha:<full-sha>"  — pinned to commit SHA
func validateTrackedRef(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "none", nil
	}
	if trackedRefValues[s] {
		return s, nil
	}
	if strings.HasPrefix(s, "tag:") && len(s) > 4 {
		return s, nil
	}
	if strings.HasPrefix(s, "sha:") && len(s) > 4 {
		return s, nil
	}
	return "", fmt.Errorf("invalid track value %q: expected 'none' | 'tag:vX.Y.Z' | 'tag:latest' | 'sha:<full>'", s)
}

// recordWorkspacePluginInstall upserts the workspace_plugins row for a
// plugin install. ON CONFLICT (workspace_id, plugin_name) DO UPDATE so
// reinstalling the same plugin name (with a possibly-different source or
// track value) updates the existing row rather than failing.
//
// installedSHA records the commit SHA that was installed; used by the drift
// sweeper to detect when the upstream ref has moved. May be empty (e.g. for
// local:// sources or pre-migration installs) — the sweeper skips NULL SHAs.
func recordWorkspacePluginInstall(
	ctx context.Context, workspaceID, pluginName, sourceRaw, track, installedSHA string,
) error {
	if workspaceID == "" || pluginName == "" || sourceRaw == "" {
		return errors.New("recordWorkspacePluginInstall: missing required field")
	}

	// Entitlement gate (defense-in-depth) for the PRIVILEGED org-management MCP
	// plugin. The install path (workspace_plugins) unions into the boot-install set
	// without re-validation, so a non-platform workspace that somehow staged the
	// plugin files could get the management MCP installed. Refuse the privileged
	// name here, mirroring the gate in recordDeclaredPlugin. Fail-closed on a kind
	// read error.
	if pluginName == conciergePlatformMCPName {
		var kind string
		if err := db.DB.QueryRowContext(ctx,
			`SELECT COALESCE(kind, 'workspace') FROM workspaces WHERE id = $1`, workspaceID).Scan(&kind); err != nil {
			return fmt.Errorf("recordWorkspacePluginInstall: kind precheck for privileged plugin %q on %s: %w", pluginName, workspaceID, err)
		}
		if kind != models.KindPlatform {
			return fmt.Errorf("recordWorkspacePluginInstall: refusing to install privileged plugin %q on non-platform workspace %s (kind=%s)", pluginName, workspaceID, kind)
		}
	}

	canonicalTrack, err := validateTrackedRef(track)
	if err != nil {
		return err
	}
	_, err = db.DB.ExecContext(ctx, `
		INSERT INTO workspace_plugins (workspace_id, plugin_name, source_raw, tracked_ref, installed_sha)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (workspace_id, plugin_name)
		DO UPDATE SET
			source_raw    = EXCLUDED.source_raw,
			tracked_ref   = EXCLUDED.tracked_ref,
			installed_sha = EXCLUDED.installed_sha,
			updated_at    = NOW()
	`, workspaceID, pluginName, sourceRaw, canonicalTrack, installedSHA)
	return err
}

// DeclaredPlugin is a (plugin_name, source_raw) pair from a workspace's
// DECLARED plugin set (workspace_declared_plugins). source_raw is the full
// source-contract string the template put in `plugins:` and is passed verbatim
// to the install pipeline by the post-online reconcile (RFC#2843).
type DeclaredPlugin struct {
	PluginName string
	SourceRaw  string
}

// recordDeclaredPlugin upserts a row in workspace_declared_plugins. Called at
// org/import time (org_import.go) for each merged plugin entry so the
// post-online reconcile knows the DESIRED plugin set. Idempotent:
// ON CONFLICT refreshes source_raw (a re-import with a moved ref updates it).
func recordDeclaredPlugin(ctx context.Context, workspaceID, pluginName, sourceRaw string) error {
	if workspaceID == "" || pluginName == "" || sourceRaw == "" {
		return errors.New("recordDeclaredPlugin: missing required field")
	}
	if db.DB == nil {
		return nil // nil in unit tests; declaration is test-only there
	}
	// Entitlement gate (defense-in-depth) for the PRIVILEGED org-management MCP
	// plugin. It carries the org-admin tool surface (create_workspace, …), so it
	// may be declared ONLY on the org-root kind='platform' concierge. Core
	// declares it exactly once, from the kind-gated applyConciergeProvisionConfig;
	// this is the single chokepoint EVERY declaration path flows through (template
	// seed, org_import, a user-authored workspace.yaml), so refusing it here for a
	// non-platform workspace closes the privilege-escalation vector regardless of
	// declaration source. Fail-closed on a kind read error.
	if pluginName == conciergePlatformMCPName {
		var kind string
		if err := db.DB.QueryRowContext(ctx,
			`SELECT COALESCE(kind, 'workspace') FROM workspaces WHERE id = $1`, workspaceID).Scan(&kind); err != nil {
			return fmt.Errorf("recordDeclaredPlugin: kind precheck for privileged plugin %q on %s: %w", pluginName, workspaceID, err)
		}
		if kind != models.KindPlatform {
			return fmt.Errorf("recordDeclaredPlugin: refusing to declare privileged plugin %q on non-platform workspace %s (kind=%s)", pluginName, workspaceID, kind)
		}
	}
	_, err := db.DB.ExecContext(ctx, `
		INSERT INTO workspace_declared_plugins (workspace_id, plugin_name, source_raw)
		VALUES ($1, $2, $3)
		ON CONFLICT (workspace_id, plugin_name)
		DO UPDATE SET source_raw = EXCLUDED.source_raw, updated_at = NOW()
	`, workspaceID, pluginName, sourceRaw)
	return err
}

// listDeclaredPlugins returns the declared plugin set for a workspace. The
// reconcile diffs this against the installed set (workspace_plugins) to find
// what to install. Returns an empty slice (not an error) when the workspace
// has no declarations — the common case for workspaces with no plugins.
func listDeclaredPlugins(ctx context.Context, workspaceID string) ([]DeclaredPlugin, error) {
	if db.DB == nil {
		return nil, nil
	}
	rows, err := db.DB.QueryContext(ctx, `
		SELECT plugin_name, source_raw
		  FROM workspace_declared_plugins
		 WHERE workspace_id = $1
		 ORDER BY plugin_name
	`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("listDeclaredPlugins: query: %w", err)
	}
	defer rows.Close()
	var out []DeclaredPlugin
	for rows.Next() {
		var d DeclaredPlugin
		if scanErr := rows.Scan(&d.PluginName, &d.SourceRaw); scanErr != nil {
			return nil, fmt.Errorf("listDeclaredPlugins: scan: %w", scanErr)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// listInstalledPluginNames returns the set of plugin names already recorded as
// installed for a workspace (workspace_plugins). The reconcile treats a name
// present here as "installed" and skips it — the DB record is the backend-
// agnostic install SSOT (written by both the Docker and EIC install paths),
// so the reconcile doesn't need to exec into the container to diff.
func listInstalledPluginNames(ctx context.Context, workspaceID string) (map[string]bool, error) {
	out := map[string]bool{}
	if db.DB == nil {
		return out, nil
	}
	rows, err := db.DB.QueryContext(ctx, `
		SELECT plugin_name FROM workspace_plugins WHERE workspace_id = $1
	`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("listInstalledPluginNames: query: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if scanErr := rows.Scan(&name); scanErr != nil {
			return nil, fmt.Errorf("listInstalledPluginNames: scan: %w", scanErr)
		}
		out[name] = true
	}
	return out, rows.Err()
}

// listInstalledPlugins returns the installed plugin set for a workspace as
// (plugin_name, source_raw) pairs from workspace_plugins. Unlike
// listInstalledPluginNames (which the reconcile uses for a present/absent diff),
// this carries the source so the boot-install desired-set can re-fetch a
// user-installed plugin that the template never declared (RFC#2843 #42). A row
// with an empty source_raw is skipped — it can't be re-fetched on boot.
func listInstalledPlugins(ctx context.Context, workspaceID string) ([]DeclaredPlugin, error) {
	if db.DB == nil {
		return nil, nil
	}
	rows, err := db.DB.QueryContext(ctx, `
		SELECT plugin_name, source_raw
		  FROM workspace_plugins
		 WHERE workspace_id = $1
		 ORDER BY plugin_name
	`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("listInstalledPlugins: query: %w", err)
	}
	defer rows.Close()
	var out []DeclaredPlugin
	for rows.Next() {
		var d DeclaredPlugin
		if scanErr := rows.Scan(&d.PluginName, &d.SourceRaw); scanErr != nil {
			return nil, fmt.Errorf("listInstalledPlugins: scan: %w", scanErr)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// desiredPluginSources computes the boot-install desired-set for a workspace as
// the UNION of its DECLARED plugins (template intent, workspace_declared_plugins)
// and its INSTALLED plugins (the live runtime set, workspace_plugins), keyed by
// plugin_name. This is the set the runtime-image entrypoint re-establishes in
// /configs/plugins on EVERY (re)provision.
//
// Why the union (RFC#2843 #42): a restart is a fresh ephemeral instance, so the
// box is rebuilt purely from this set. Stamping DECLARED-only wiped any plugin a
// user installed at runtime via install_plugin (recorded in workspace_plugins,
// never in workspace_declared_plugins) — it silently vanished on the next
// restart. Stamping INSTALLED-only would drop a declared plugin whose first
// install hasn't been recorded yet (first boot, before the post-online reconcile
// runs). The union covers both: declared seeds first boot; installed preserves
// user additions across restarts.
//
// On a name collision (declared AND installed — the steady state for a template
// plugin after its first reconcile) the INSTALLED source wins: it reflects what
// is actually running, including any ref the user re-pinned via install_plugin.
// Returns sources sorted by plugin_name for a stable env value. Rows with an
// empty source_raw are skipped (nothing to fetch).
func desiredPluginSources(ctx context.Context, workspaceID string) ([]string, error) {
	declared, err := listDeclaredPlugins(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("desiredPluginSources: declared: %w", err)
	}
	installed, err := listInstalledPlugins(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("desiredPluginSources: installed: %w", err)
	}
	// byName: declared first (seed), then installed overrides on collision.
	byName := make(map[string]string, len(declared)+len(installed))
	names := make([]string, 0, len(declared)+len(installed))
	add := func(p DeclaredPlugin) {
		src := strings.TrimSpace(p.SourceRaw)
		if p.PluginName == "" || src == "" {
			return
		}
		if _, seen := byName[p.PluginName]; !seen {
			names = append(names, p.PluginName)
		}
		byName[p.PluginName] = src
	}
	for _, d := range declared {
		add(d)
	}
	for _, i := range installed {
		add(i) // installed source wins on collision
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, byName[n])
	}
	return out, nil
}

// deleteWorkspacePluginRow removes the workspace_plugins row for a workspace/plugin
// pair. Called by the uninstall path so the row doesn't persist with a stale
// installed_sha after the plugin has been removed from the container.
func deleteWorkspacePluginRow(ctx context.Context, workspaceID, pluginName string) error {
	if db.DB == nil {
		return nil // nil in unit tests; no-op since the row is test-only
	}
	_, err := db.DB.ExecContext(ctx, `
		DELETE FROM workspace_plugins WHERE workspace_id = $1 AND plugin_name = $2
	`, workspaceID, pluginName)
	return err
}
