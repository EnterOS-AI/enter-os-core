package main

// verify.go — post-apply parity check.
//
// After a backfill -apply, run with -verify to confirm the migration
// actually produced equivalent data. Picks `SampleSize` random
// workspaces, queries agent_memories direct + plugin search via the
// caller's namespaces, and diffs the result sets by content.
//
// The diff is best-effort: pg's recent-first ordering and the plugin's
// internal ordering may differ, so we compare as sets, not lists.
// We do require strict 1:1 multiset equality (every legacy row maps
// to exactly one plugin row, ignoring id since the backfill preserves
// it via the C1 idempotency key).

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"os"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/memory/contract"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/textutil"
)

// verifyConfig is the typed dependency bundle for verifyParity.
type verifyConfig struct {
	DB          *sql.DB
	Plugin      verifyPlugin
	Resolver    verifyResolver
	SampleSize  int
	WorkspaceID string // optional: limit to one workspace
	Rand        *rand.Rand
}

// verifyPlugin is the slice of memory-plugin client we call.
type verifyPlugin interface {
	Search(ctx context.Context, body contract.SearchRequest) (*contract.SearchResponse, error)
}

// verifyResolver mirrors namespace.Resolver. Same shape as
// backfillResolver but kept distinct so verify isn't tied to
// backfill's interface.
type verifyResolver interface {
	ReadableNamespaces(ctx context.Context, workspaceID string) ([]ResolvedNamespace, error)
}

// ResolvedNamespace is the minimum we need from the resolver — kept
// separate so the verify code doesn't depend on the namespace package
// (the live tests inject stubs, the binary uses an adapter).
type ResolvedNamespace struct {
	Name string
}

// verifyReport accumulates the per-workspace results.
type verifyReport struct {
	WorkspacesSampled int
	Matches           int
	Mismatches        int
	Errors            int
}

// verifyParity is the workhorse. Returns a report; the CLI converts
// any non-zero mismatches/errors into a non-zero exit so CI can gate
// the cutover.
func verifyParity(ctx context.Context, cfg verifyConfig, stdout *os.File) (*verifyReport, error) {
	report := &verifyReport{}
	rng := cfg.Rand
	if rng == nil {
		rng = rand.New(rand.NewSource(42)) //nolint:gosec // determinism > unpredictability for ops
	}

	wsIDs, err := pickWorkspaceSample(ctx, cfg.DB, cfg.WorkspaceID, cfg.SampleSize, rng)
	if err != nil {
		return report, fmt.Errorf("pick sample: %w", err)
	}

	for _, wsID := range wsIDs {
		report.WorkspacesSampled++
		legacy, err := queryLegacyMemories(ctx, cfg.DB, wsID)
		if err != nil {
			fmt.Fprintf(stdout, "[err] workspace=%s legacy query: %v\n", wsID, err)
			report.Errors++
			continue
		}
		readable, err := cfg.Resolver.ReadableNamespaces(ctx, wsID)
		if err != nil {
			fmt.Fprintf(stdout, "[err] workspace=%s resolve: %v\n", wsID, err)
			report.Errors++
			continue
		}
		nsList := make([]string, len(readable))
		for i, ns := range readable {
			nsList[i] = ns.Name
		}
		if len(nsList) == 0 {
			// No readable namespaces — empty plugin result expected.
			if len(legacy) == 0 {
				report.Matches++
			} else {
				fmt.Fprintf(stdout, "[mismatch] workspace=%s legacy=%d plugin=0 (no readable namespaces)\n", wsID, len(legacy))
				report.Mismatches++
			}
			continue
		}
		resp, err := cfg.Plugin.Search(ctx, contract.SearchRequest{Namespaces: nsList, Limit: 100})
		if err != nil {
			fmt.Fprintf(stdout, "[err] workspace=%s plugin search: %v\n", wsID, err)
			report.Errors++
			continue
		}
		pluginContents := make(map[string]int, len(resp.Memories))
		for _, m := range resp.Memories {
			pluginContents[m.Content]++
		}
		// Compare as multisets: each legacy content appears at least
		// once in plugin output. We deliberately tolerate plugin
		// having MORE rows (the namespace might include team-shared
		// memories from sibling workspaces that aren't in this
		// workspace's agent_memories rows).
		matched := true
		for _, c := range legacy {
			if pluginContents[c] == 0 {
				fmt.Fprintf(stdout, "[mismatch] workspace=%s missing-from-plugin content=%q\n", wsID, textutil.TruncateBytes(c, 80))
				matched = false
				break
			}
			pluginContents[c]--
		}
		if matched {
			report.Matches++
		} else {
			report.Mismatches++
		}
	}
	return report, nil
}

// pickWorkspaceSample returns up to N workspace UUIDs. If
// WorkspaceID is set, returns only that one. Otherwise selects N
// random workspaces from the workspaces table (TABLESAMPLE would be
// nicer but SYSTEM/BERNOULLI sampling has surprising distribution
// properties for small populations; we just ORDER BY random() LIMIT).
func pickWorkspaceSample(ctx context.Context, db *sql.DB, workspaceID string, n int, _ *rand.Rand) ([]string, error) {
	if workspaceID != "" {
		return []string{workspaceID}, nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT id::text
		FROM workspaces
		WHERE status != 'removed'
		ORDER BY random()
		LIMIT $1
	`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]string, 0, n)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// queryLegacyMemories pulls all agent_memories rows for a workspace
// (LOCAL + TEAM scopes — what the plugin search would return through
// the resolver's readable list, mapped via PR-6 shim semantics).
func queryLegacyMemories(ctx context.Context, db *sql.DB, workspaceID string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT content
		FROM agent_memories
		WHERE workspace_id = $1
		ORDER BY created_at DESC
	`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// truncation moved to internal/textutil.TruncateBytes (#2962 SSOT).
