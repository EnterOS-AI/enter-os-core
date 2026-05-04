// memory-backfill is a one-shot CLI that copies rows from the legacy
// agent_memories table into the v2 plugin via its HTTP API.
//
// Idempotent on re-run: the backfill passes each source row's UUID
// to the plugin's MemoryWrite.ID field, and the plugin upserts on
// conflict. Re-running the backfill (whole or partial) updates rows
// in place rather than duplicating.
//
// Usage:
//   memory-backfill -dry-run                    # count + diff
//   memory-backfill -apply                      # actually copy
//   memory-backfill -apply -limit=10000         # cap rows per run
//   memory-backfill -apply -workspace=<uuid>    # one workspace only
//
// Required env:
//   DATABASE_URL                — workspace-server DB (read agent_memories)
//   MEMORY_PLUGIN_URL           — target plugin (write memory_records)
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	_ "github.com/lib/pq"

	mclient "github.com/Molecule-AI/molecule-monorepo/platform/internal/memory/client"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/memory/contract"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/memory/namespace"
)

const defaultLimit = 1000000 // effectively unlimited; cap keeps SQL pageable

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		log.Fatalf("memory-backfill: %v", err)
	}
}

// run is extracted so tests can drive it with synthesized argv +
// captured stdout/stderr. Returns nil on success.
func run(argv []string, stdout, stderr *os.File) error {
	fs := flag.NewFlagSet("memory-backfill", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dryRun := fs.Bool("dry-run", false, "count + diff only, no writes")
	apply := fs.Bool("apply", false, "actually copy rows to the plugin")
	verify := fs.Bool("verify", false, "post-apply parity check: random-sample N workspaces, diff agent_memories vs plugin search")
	verifySample := fs.Int("verify-sample", 50, "number of workspaces to sample in -verify mode")
	workspace := fs.String("workspace", "", "limit to a single workspace UUID (empty = all)")
	limit := fs.Int("limit", defaultLimit, "max rows to process this run")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	modesPicked := 0
	if *dryRun {
		modesPicked++
	}
	if *apply {
		modesPicked++
	}
	if *verify {
		modesPicked++
	}
	if modesPicked != 1 {
		return errors.New("specify exactly one of -dry-run, -apply, or -verify")
	}

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return errors.New("DATABASE_URL is required")
	}
	pluginURL := os.Getenv("MEMORY_PLUGIN_URL")
	if pluginURL == "" {
		return errors.New("MEMORY_PLUGIN_URL is required")
	}

	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping db: %w", err)
	}

	plugin := mclient.New(mclient.Config{BaseURL: pluginURL})
	resolver := namespace.New(db)

	if *verify {
		vcfg := verifyConfig{
			DB:          db,
			Plugin:      plugin,
			Resolver:    namespaceResolverAdapter{resolver},
			SampleSize:  *verifySample,
			WorkspaceID: *workspace,
		}
		report, err := verifyParity(context.Background(), vcfg, stdout)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "\nVerify complete: workspaces_sampled=%d matches=%d mismatches=%d errors=%d\n",
			report.WorkspacesSampled, report.Matches, report.Mismatches, report.Errors)
		if report.Mismatches > 0 || report.Errors > 0 {
			return fmt.Errorf("verify found %d mismatches and %d errors", report.Mismatches, report.Errors)
		}
		return nil
	}

	cfg := backfillConfig{
		DB:          db,
		Plugin:      plugin,
		Resolver:    resolver,
		WorkspaceID: *workspace,
		Limit:       *limit,
		DryRun:      *dryRun,
	}
	stats, err := backfill(context.Background(), cfg, stdout)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "\nBackfill complete: scanned=%d copied=%d skipped=%d errors=%d\n",
		stats.Scanned, stats.Copied, stats.Skipped, stats.Errors)
	return nil
}

// backfillStats accumulates the counters the CLI reports.
type backfillStats struct {
	Scanned int
	Copied  int
	Skipped int
	Errors  int
}

// backfillConfig is the typed dependency bundle. Tests inject stubs
// for Plugin and Resolver; production wires real client + resolver.
type backfillConfig struct {
	DB          *sql.DB
	Plugin      backfillPlugin
	Resolver    backfillResolver
	WorkspaceID string
	Limit       int
	DryRun      bool
}

// backfillPlugin is the slice of memory-plugin client we call.
type backfillPlugin interface {
	UpsertNamespace(ctx context.Context, name string, body contract.NamespaceUpsert) (*contract.Namespace, error)
	CommitMemory(ctx context.Context, namespace string, body contract.MemoryWrite) (*contract.MemoryWriteResponse, error)
}

// backfillResolver lets the backfill compute namespace strings the
// same way the live MCP layer does.
type backfillResolver interface {
	WritableNamespaces(ctx context.Context, workspaceID string) ([]namespace.Namespace, error)
}

// backfill is the workhorse. Iterates agent_memories, maps each row's
// scope to a v2 namespace via the resolver, and POSTs to the plugin.
// Returns final stats. Stops after Limit rows.
func backfill(ctx context.Context, cfg backfillConfig, stdout *os.File) (*backfillStats, error) {
	stats := &backfillStats{}

	query := `
		SELECT id, workspace_id, content, scope, created_at
		FROM agent_memories
	`
	args := []interface{}{}
	if cfg.WorkspaceID != "" {
		query += ` WHERE workspace_id = $1`
		args = append(args, cfg.WorkspaceID)
	}
	query += ` ORDER BY created_at ASC LIMIT $` + fmt.Sprintf("%d", len(args)+1)
	args = append(args, cfg.Limit)

	rows, err := cfg.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return stats, fmt.Errorf("query agent_memories: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		stats.Scanned++
		var (
			id, workspaceID, content, scope string
			createdAt                       time.Time
		)
		if err := rows.Scan(&id, &workspaceID, &content, &scope, &createdAt); err != nil {
			fmt.Fprintf(stdout, "scan: %v\n", err)
			stats.Errors++
			continue
		}

		ns, err := mapScopeToNamespace(ctx, cfg.Resolver, workspaceID, scope)
		if err != nil {
			fmt.Fprintf(stdout, "[skip] id=%s workspace=%s: %v\n", id, workspaceID, err)
			stats.Skipped++
			continue
		}

		if cfg.DryRun {
			fmt.Fprintf(stdout, "[dry] id=%s scope=%s → ns=%s\n", id, scope, ns)
			stats.Copied++ // would-have-copied
			continue
		}

		// Ensure the namespace exists before posting memories. Plugin's
		// UpsertNamespace is idempotent so calling per-row is wasteful
		// but safe; for v1 we accept the chattiness.
		if _, err := cfg.Plugin.UpsertNamespace(ctx, ns, contract.NamespaceUpsert{
			Kind: namespaceKindFromString(scope),
		}); err != nil {
			fmt.Fprintf(stdout, "[err-ns] id=%s ns=%s: %v\n", id, ns, err)
			stats.Errors++
			continue
		}

		// Pass the source row's UUID as the idempotency key so re-runs
		// upsert in place. Without this, retries would duplicate every
		// memory.
		if _, err := cfg.Plugin.CommitMemory(ctx, ns, contract.MemoryWrite{
			ID:      id,
			Content: content,
			Kind:    contract.MemoryKindFact,
			Source:  contract.MemorySourceAgent,
		}); err != nil {
			fmt.Fprintf(stdout, "[err-mem] id=%s ns=%s: %v\n", id, ns, err)
			stats.Errors++
			continue
		}
		stats.Copied++
	}
	if err := rows.Err(); err != nil {
		return stats, fmt.Errorf("iterate rows: %w", err)
	}
	return stats, nil
}

// mapScopeToNamespace mirrors the legacy-shim translation. The
// backfill needs the SAME mapping the runtime uses so reads work
// after cutover.
func mapScopeToNamespace(ctx context.Context, r backfillResolver, workspaceID, scope string) (string, error) {
	writable, err := r.WritableNamespaces(ctx, workspaceID)
	if err != nil {
		return "", fmt.Errorf("resolve writable: %w", err)
	}
	wantKind := contract.NamespaceKindWorkspace
	switch scope {
	case "LOCAL":
		wantKind = contract.NamespaceKindWorkspace
	case "TEAM":
		wantKind = contract.NamespaceKindTeam
	case "GLOBAL":
		wantKind = contract.NamespaceKindOrg
	default:
		return "", fmt.Errorf("unknown scope %q", scope)
	}
	for _, ns := range writable {
		if ns.Kind == wantKind {
			return ns.Name, nil
		}
	}
	return "", fmt.Errorf("no writable namespace of kind %s for workspace %s", wantKind, workspaceID)
}

// namespaceKindFromString returns the contract.NamespaceKind for a
// legacy scope value. Unknown scopes default to "workspace" so the
// backfill never aborts on an unexpected row.
func namespaceKindFromString(scope string) contract.NamespaceKind {
	switch strings.ToUpper(scope) {
	case "TEAM":
		return contract.NamespaceKindTeam
	case "GLOBAL":
		return contract.NamespaceKindOrg
	default:
		return contract.NamespaceKindWorkspace
	}
}

// namespaceResolverAdapter bridges *namespace.Resolver (which returns
// []namespace.Namespace) to verify.go's verifyResolver interface
// (which wants []ResolvedNamespace). Keeps verify.go independent of
// the namespace-package dependency so its tests can stub easily.
type namespaceResolverAdapter struct {
	r *namespace.Resolver
}

func (a namespaceResolverAdapter) ReadableNamespaces(ctx context.Context, workspaceID string) ([]ResolvedNamespace, error) {
	src, err := a.r.ReadableNamespaces(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	out := make([]ResolvedNamespace, len(src))
	for i, ns := range src {
		out[i] = ResolvedNamespace{Name: ns.Name}
	}
	return out, nil
}
