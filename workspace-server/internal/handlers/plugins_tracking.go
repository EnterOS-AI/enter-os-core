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
	"strings"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
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

// deleteWorkspacePluginRow removes the workspace_plugins row for a workspace/plugin
// pair. Called by the uninstall path so the row doesn't persist with a stale
// installed_sha after the plugin has been removed from the container.
func deleteWorkspacePluginRow(ctx context.Context, workspaceID, pluginName string) error {
	_, err := db.DB.ExecContext(ctx, `
		DELETE FROM workspace_plugins WHERE workspace_id = $1 AND plugin_name = $2
	`, workspaceID, pluginName)
	return err
}
