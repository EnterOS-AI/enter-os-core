// Package namespace derives the set of memory namespaces a workspace
// can read from / write to, based on the live workspace tree.
//
// Today the workspace tree is depth-1 (root + children). The recursive
// CTE below tolerates deeper trees if we ever introduce them, with a
// hop limit to prevent infinite loops on malformed data.
//
// This package owns the namespace-derivation policy and is the only
// caller that should be talking to the workspaces table for ACL
// purposes. Memory plugin clients receive the result as opaque
// namespace strings — the plugin never knows about parent_id.
package namespace

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/memory/contract"
)

// Max parent_id chain depth we will walk before bailing out. Today's
// production tree is depth 1; this is a guard against malformed data
// (e.g., a self-cycle that slipped past application checks).
const maxChainDepth = 50

// Namespace is a typed namespace entry returned to the agent through
// the list_writable_namespaces / list_readable_namespaces MCP tools.
// The Name field is the wire string sent to the plugin.
type Namespace struct {
	Name        string                 `json:"name"`
	Kind        contract.NamespaceKind `json:"kind"`
	Description string                 `json:"description"`
	Writable    bool                   `json:"writable"`
}

// ErrWorkspaceNotFound is returned when the input workspace ID does
// not exist in the workspaces table.
var ErrWorkspaceNotFound = errors.New("workspace not found")

// Resolver computes the namespace lists from the workspaces table.
// Stateless; safe to share. Per-request caching (gin context) lives
// in the MCP handler layer (PR-5), not here.
type Resolver struct {
	db *sql.DB
}

// New constructs a Resolver bound to the given DB handle.
func New(db *sql.DB) *Resolver {
	return &Resolver{db: db}
}

// chainNode is one row from the recursive CTE.
type chainNode struct {
	id       string
	parentID *string
	depth    int
}

// walkChain returns the workspace plus all its ancestors, ordered
// from self (depth 0) to root (depth N). Returns ErrWorkspaceNotFound
// if the input id has no row.
func (r *Resolver) walkChain(ctx context.Context, workspaceID string) ([]chainNode, error) {
	const query = `
		WITH RECURSIVE chain AS (
			SELECT id, parent_id, 0 AS depth
			FROM workspaces
			WHERE id = $1
			UNION ALL
			SELECT w.id, w.parent_id, c.depth + 1
			FROM workspaces w
			JOIN chain c ON w.id = c.parent_id
			WHERE c.depth < $2
		)
		SELECT id::text, parent_id::text, depth FROM chain ORDER BY depth ASC
	`
	rows, err := r.db.QueryContext(ctx, query, workspaceID, maxChainDepth)
	if err != nil {
		return nil, fmt.Errorf("walk chain: %w", err)
	}
	defer rows.Close()

	var out []chainNode
	for rows.Next() {
		var n chainNode
		var parentStr sql.NullString
		if err := rows.Scan(&n.id, &parentStr, &n.depth); err != nil {
			return nil, fmt.Errorf("scan chain: %w", err)
		}
		if parentStr.Valid && parentStr.String != "" {
			p := parentStr.String
			n.parentID = &p
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iter chain: %w", err)
	}
	if len(out) == 0 {
		return nil, ErrWorkspaceNotFound
	}
	return out, nil
}

// derive computes the three canonical namespaces (workspace, team,
// org) from a chain. Today this is mostly degenerate because the tree
// is depth-1, but the function shape generalises:
//
//   - workspace: always self
//   - team: parent if child, self if root
//   - org: root of the chain (highest ancestor)
func derive(chain []chainNode) (workspace, team, org string) {
	self := chain[0]
	workspace = self.id
	if self.parentID != nil {
		team = *self.parentID
	} else {
		team = self.id
	}
	org = chain[len(chain)-1].id
	return
}

// ReadableNamespaces returns the namespaces the workspace can read
// from. Order is deterministic (workspace, team, org) so callers can
// reason about precedence.
func (r *Resolver) ReadableNamespaces(ctx context.Context, workspaceID string) ([]Namespace, error) {
	chain, err := r.walkChain(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	wsID, teamID, orgID := derive(chain)
	isRoot := chain[0].parentID == nil

	out := []Namespace{
		{
			Name:        "workspace:" + wsID,
			Kind:        contract.NamespaceKindWorkspace,
			Description: "This workspace's private memories",
			Writable:    true,
		},
		{
			Name:        "team:" + teamID,
			Kind:        contract.NamespaceKindTeam,
			Description: "Memories shared across team members (parent + siblings)",
			Writable:    true,
		},
	}
	// Org namespace is readable by every workspace in the tree, but
	// only writable by the root (preserves today's GLOBAL constraint
	// at memories.go:167-174).
	out = append(out, Namespace{
		Name:        "org:" + orgID,
		Kind:        contract.NamespaceKindOrg,
		Description: "Org-wide memories visible to every workspace under this root",
		Writable:    isRoot,
	})
	return out, nil
}

// WritableNamespaces returns the subset of ReadableNamespaces the
// workspace can write to. Filters by the Writable flag.
//
// Server-side enforcement: the MCP handler MUST re-derive this list
// at write time and validate the requested namespace is in it. Don't
// trust client-side discovery — workspaces can be re-parented between
// the discovery call and the write call.
func (r *Resolver) WritableNamespaces(ctx context.Context, workspaceID string) ([]Namespace, error) {
	all, err := r.ReadableNamespaces(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	out := make([]Namespace, 0, len(all))
	for _, ns := range all {
		if ns.Writable {
			out = append(out, ns)
		}
	}
	return out, nil
}

// CanWrite is a fast-path check for "is this namespace string in the
// caller's writable set?" Used by MCP handlers before calling the
// plugin to enforce server-side ACL.
func (r *Resolver) CanWrite(ctx context.Context, workspaceID, namespace string) (bool, error) {
	writable, err := r.WritableNamespaces(ctx, workspaceID)
	if err != nil {
		return false, err
	}
	for _, ns := range writable {
		if ns.Name == namespace {
			return true, nil
		}
	}
	return false, nil
}

// IntersectReadable returns the subset of `requested` that are in the
// caller's readable set. Used by MCP handlers before calling
// search_memory to prevent leakage from no-longer-permitted scopes.
//
// If `requested` is empty, returns the entire readable set (default
// behavior: search everything visible).
func (r *Resolver) IntersectReadable(ctx context.Context, workspaceID string, requested []string) ([]string, error) {
	readable, err := r.ReadableNamespaces(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	if len(requested) == 0 {
		out := make([]string, len(readable))
		for i, ns := range readable {
			out[i] = ns.Name
		}
		return out, nil
	}
	allowed := make(map[string]struct{}, len(readable))
	for _, ns := range readable {
		allowed[ns.Name] = struct{}{}
	}
	out := make([]string, 0, len(requested))
	for _, want := range requested {
		if _, ok := allowed[want]; ok {
			out = append(out, want)
		}
	}
	return out, nil
}
