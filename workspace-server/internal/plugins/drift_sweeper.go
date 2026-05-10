package plugins

// drift_sweeper.go — periodic drift detection for the plugin version-subscription
// model (core#113 / #123).
//
// How it works
// ─────────────
// Every DriftSweepInterval the sweeper:
//   1. SELECTs workspace_plugins rows where tracked_ref != 'none'
//      AND installed_sha IS NOT NULL (skip pre-migration rows with NULL SHA).
//   2. For each row, resolves the tracked ref to its current upstream SHA
//      using the appropriate SourceResolver.
//   3. If the resolved SHA differs from installed_sha → drift detected.
//   4. On drift, INSERT INTO plugin_update_queue (ON CONFLICT DO NOTHING so
//      a re-drift while a row is still pending is a no-op).
//
// Thread-safety
// ─────────────
// The sweeper holds no mutable state between ticks. Each tick runs a fresh
// goroutine spawned by the ticker; the parent goroutine is cancelled when
// the passed context is cancelled. This matches the pattern used by
// pendinguploads/sweeper.go and registry/orphan_sweeper.go.
//
// Gitea compatibility
// ───────────────────
// Gitea's REST API is a GitHub-API-compatible surface, so the GithubResolver
// with BaseURL pointing at a Gitea instance works for Gitea-hosted plugin
// sources too. The source_raw in workspace_plugins stores the full spec
// (e.g. "github://owner/repo#tag:v1.0.0") which the resolver parses.
// For "local://" sources the resolver has no SHA concept, so those rows
// are skipped (local plugins have no upstream to drift against).
//
// Resource cost
// ─────────────
// Each tick runs O(N) resolves where N is the count of tracked plugins.
// Each resolve does a --depth=1 git fetch, bounded by the network round-trip
// to GitHub/Gitea. With 1000 tracked plugins and 1h interval, worst case is
// ~1,000 network calls per hour. The per-row timeout (ResolveRefDeadline)
// prevents a slow/hanging fetch from blocking the entire sweep cycle.

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
)

// DriftSweepInterval is the cadence between drift-sweep cycles.
// 1 hour is a reasonable balance: fast enough to surface new tag releases
// within a reasonable window, sparse enough to not hammer GitHub's API with
// 1000s of concurrent requests across a large deployment.
const DriftSweepInterval = 1 * time.Hour

// ResolveRefDeadline bounds the git fetch for a single plugin. A
// --depth=1 clone of any reasonable plugin repo should complete well
// within 30s on a healthy connection; 60s is the conservative ceiling
// that handles Gitea instances on high-latency links.
const ResolveRefDeadline = 60 * time.Second

// SourceResolver resolves plugin sources to installable directories.
// Satisfied by *Registry (which wraps GithubResolver + LocalResolver).
type SourceResolver interface {
	Resolve(source Source) (SourceResolver, error)
	Schemes() []string
}

// StartPluginDriftSweeper runs the drift-detection loop until ctx is cancelled.
// Pass a nil resolver to disable the sweeper (useful for harnesses or CP/SaaS
// mode where git operations are unavailable).
//
// Registers itself via atexits in cmd/server/main.go so the process
// shuts down cleanly on SIGTERM.
func StartPluginDriftSweeper(ctx context.Context, resolver SourceResolver) {
	if resolver == nil {
		log.Println("Plugin drift sweeper: resolver is nil — sweeper disabled")
		return
	}
	log.Printf("Plugin drift sweeper started — interval %s", DriftSweepInterval)
	ticker := time.NewTicker(DriftSweepInterval)
	defer ticker.Stop()

	// Run once on startup so we detect drift immediately rather than waiting
	// for the first tick.
	sweepDriftOnce(ctx, resolver)
	for {
		select {
		case <-ctx.Done():
			log.Println("Plugin drift sweeper: shutdown")
			return
		case <-ticker.C:
			// ctx.Err() guard: the ticker may fire just as ctx is cancelled
			// (MPMC channel race). Skip the sweep so we don't start a
			// ResolveRef cycle after shutdown that would pollute the next
			// test's baseline.
			if ctx.Err() != nil {
				continue
			}
			sweepDriftOnce(ctx, resolver)
		}
	}
}

// sweepDriftOnce runs one full drift-detection cycle.
// Errors are non-fatal — each row is handled independently so a single
// slow row doesn't block the rest of the sweep.
func sweepDriftOnce(parent context.Context, resolver SourceResolver) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Minute)
	defer cancel()

	rows, err := db.DB.QueryContext(ctx, `
		SELECT wp.id, wp.workspace_id, wp.plugin_name, wp.source_raw,
		       wp.tracked_ref, wp.installed_sha
		  FROM workspace_plugins wp
		 WHERE wp.tracked_ref != 'none'
		   AND wp.installed_sha IS NOT NULL
	`)
	if err != nil {
		log.Printf("Plugin drift sweeper: SELECT failed: %v", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var row struct {
			id          string
			workspaceID string
			pluginName  string
			sourceRaw   string
			trackedRef  string
			installedSHA string
		}
		if scanErr := rows.Scan(&row.id, &row.workspaceID, &row.pluginName,
			&row.sourceRaw, &row.trackedRef, &row.installedSHA); scanErr != nil {
			log.Printf("Plugin drift sweeper: row scan failed: %v", scanErr)
			continue
		}

		latestSHA, resolveErr := resolveLatestSHA(ctx, resolver, row.sourceRaw, row.trackedRef)
		if resolveErr != nil {
			// Log and skip — don't queue drift if we couldn't resolve.
			// Transient network errors self-heal on the next cycle.
			log.Printf("Plugin drift sweeper: resolve %s@%s failed: %v — skipping",
				row.pluginName, row.trackedRef, resolveErr)
			continue
		}

		if latestSHA == row.installedSHA {
			continue // no drift
		}

		log.Printf("Plugin drift sweeper: drift detected for %s (workspace=%s): "+
			"installed=%s upstream=%s", row.pluginName, row.workspaceID,
			row.installedSHA[:8], latestSHA[:8])

		if queueErr := queueDriftEntry(ctx, row.workspaceID, row.pluginName,
			row.trackedRef, row.installedSHA, latestSHA); queueErr != nil {
			log.Printf("Plugin drift sweeper: queue drift for %s failed: %v",
				row.pluginName, queueErr)
		}
	}
	if iterErr := rows.Err(); iterErr != nil {
		log.Printf("Plugin drift sweeper: rows iteration failed: %v", iterErr)
	}
}

// resolveLatestSHA resolves the tracked ref to its current upstream SHA.
// Handles both github:// and local:// sources; local sources are skipped
// (no meaningful upstream to drift against).
func resolveLatestSHA(ctx context.Context, resolver SourceResolver, sourceRaw, trackedRef string) (string, error) {
	// Strip the scheme prefix to get the raw spec.
	// sourceRaw is stored as the full string, e.g. "github://owner/repo#tag:v1.0.0"
	spec := sourceRaw
	for _, scheme := range resolver.Schemes() {
		if strings.HasPrefix(spec, scheme+"://") {
			spec = strings.TrimPrefix(spec, scheme+"://")
			break
		}
	}

	// Parse the ref from the tracked_ref field (e.g. "tag:v1.0.0").
	// Prepend it as a # suffix so the resolver can fetch the right ref.
	var refSuffix string
	switch {
	case strings.HasPrefix(trackedRef, "tag:"):
		refSuffix = "#" + trackedRef
	case strings.HasPrefix(trackedRef, "sha:"):
		refSuffix = "#" + trackedRef
	default:
		// Bare ref (shouldn't happen per validateTrackedRef, but be safe).
		refSuffix = "#" + trackedRef
	}

	// If spec already has a # fragment, replace it with the tracked ref.
	// (In practice source_raw always has one, but handle both cases.)
	if strings.Contains(spec, "#") {
		spec = strings.SplitN(spec, "#", 2)[0] + refSuffix
	} else {
		spec = spec + refSuffix
	}

	// Use the github resolver directly — it handles the fetch + rev-parse.
	gh := NewGithubResolver()
	resolvedSHA, err := gh.ResolveRef(ctx, spec)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", spec, err)
	}
	return resolvedSHA, nil
}

// queueDriftEntry inserts a pending drift entry into plugin_update_queue.
// ON CONFLICT (workspace_id, plugin_name) WHERE status = 'pending' DO NOTHING
// makes this idempotent — re-drift while a row is already pending is a no-op.
// Uses the partial unique index plugin_update_queue_pending_unique as the
// inference target; the WHERE clause ensures we only dedup pending rows.
func queueDriftEntry(ctx context.Context, workspaceID, pluginName, trackedRef, currentSHA, latestSHA string) error {
	_, err := db.DB.ExecContext(ctx, `
		INSERT INTO plugin_update_queue
		  (workspace_id, plugin_name, tracked_ref, current_sha, latest_sha)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (workspace_id, plugin_name) DO NOTHING
	`, workspaceID, pluginName, trackedRef, currentSHA, latestSHA)
	return err
}

// ─────────────────────────────────────────────────────────────────────────────
// Test helpers
// ─────────────────────────────────────────────────────────────────────────────

// SweepDriftOnceForTest exposes sweepDriftOnce for package-level testing.
func SweepDriftOnceForTest(parent context.Context, resolver SourceResolver) {
	sweepDriftOnce(parent, resolver)
}

// QueueDriftEntryForTest exposes queueDriftEntry for package-level testing.
func QueueDriftEntryForTest(ctx context.Context, workspaceID, pluginName, trackedRef, currentSHA, latestSHA string) error {
	return queueDriftEntry(ctx, workspaceID, pluginName, trackedRef, currentSHA, latestSHA)
}

// PluginUpdateQueueRow is the Go struct mirroring a plugin_update_queue row.
// Exported for tests and for the admin handler to consume.
type PluginUpdateQueueRow struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	PluginName  string    `json:"plugin_name"`
	TrackedRef  string    `json:"tracked_ref"`
	CurrentSHA  string    `json:"current_sha"`
	LatestSHA   string    `json:"latest_sha"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
}

// ListPendingUpdates returns all pending drift entries, newest first.
func ListPendingUpdates(ctx context.Context) ([]PluginUpdateQueueRow, error) {
	rows, err := db.DB.QueryContext(ctx, `
		SELECT id, workspace_id, plugin_name, tracked_ref,
		       current_sha, latest_sha, status, created_at
		  FROM plugin_update_queue
		 WHERE status = 'pending'
		 ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("list pending updates: %w", err)
	}
	defer rows.Close()

	var result []PluginUpdateQueueRow
	for rows.Next() {
		var r PluginUpdateQueueRow
		if scanErr := rows.Scan(&r.ID, &r.WorkspaceID, &r.PluginName,
			&r.TrackedRef, &r.CurrentSHA, &r.LatestSHA, &r.Status, &r.CreatedAt); scanErr != nil {
			return nil, fmt.Errorf("scan row: %w", scanErr)
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// ApplyDriftUpdate marks a queue entry as applied (or already-applied idempotently)
// and returns the workspace_id and plugin_name so the caller can trigger a restart.
func ApplyDriftUpdate(ctx context.Context, queueID string) (workspaceID, pluginName string, err error) {
	var row struct {
		WorkspaceID string
		PluginName  string
		Status      sql.NullString
	}
	err = db.DB.QueryRowContext(ctx, `
		SELECT workspace_id, plugin_name, status
		  FROM plugin_update_queue
		 WHERE id = $1
	`, queueID).Scan(&row.WorkspaceID, &row.PluginName, &row.Status)
	if err == sql.ErrNoRows {
		return "", "", fmt.Errorf("queue entry %s not found", queueID)
	}
	if err != nil {
		return "", "", fmt.Errorf("query queue entry: %w", err)
	}

	if row.Status.Valid && row.Status.String == "applied" {
		// Idempotent — already applied.
		return row.WorkspaceID, row.PluginName, nil
	}

	_, execErr := db.DB.ExecContext(ctx, `
		UPDATE plugin_update_queue
		   SET status = 'applied'
		 WHERE id = $1
		   AND status = 'pending'
	`, queueID)
	if execErr != nil {
		return "", "", fmt.Errorf("update status: %w", execErr)
	}
	return row.WorkspaceID, row.PluginName, nil
}
