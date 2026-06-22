package handlers

// org_scope.go — cross-tenant isolation helpers (#1953).
//
// The `workspaces` table has no `org_id` column; an "org" is the subtree of
// workspaces reachable through the `parent_id` chain from a single org root
// (a row with parent_id IS NULL). Several code paths historically computed an
// org-root sibling set as `WHERE parent_id IS NULL`, which matches EVERY
// tenant's org root and therefore leaks peer metadata / routing across tenants.
//
// This file centralises the org-scoping primitive so peer discovery, the MCP
// list_peers tool, and a2a routing all derive "the caller's org" the SAME way
// the OFFSEC-015 broadcast fix (commit 5a05302c, workspace_broadcast.go) does:
// a recursive CTE that walks the parent_id chain up to the org root. Keeping
// the CTE in one place means there is a single, testable source of truth for
// tenant isolation rather than four hand-copied queries that can drift.
//
// NOTE: this is the parent_id-chain scoping that the broadcast fix already
// ships. It is deliberately NOT an `org_id` column — adding that column is a
// separate architecture decision pending CTO sign-off. See #1953.

import (
	"context"
	"database/sql"
	"errors"
)

// errNoOrgRoot is returned by orgRootID when the workspace id has no row (and
// therefore no resolvable org root). Callers translate this into a 404/not-found
// at their own layer; it is distinct from a transient DB error so a missing
// workspace never gets treated as "belongs to every org".
var errNoOrgRoot = errors.New("org root not found for workspace")

// orgRootSubtreeCTE is the recursive CTE — identical in shape to the OFFSEC-015
// broadcast fix — that walks UP the parent_id chain from a single workspace to
// its org root. The org root is the row on the chain whose parent_id IS NULL.
//
//	$1 = workspace id to resolve
//
// The recursive member walks UP the parent_id chain: each step joins to the row
// whose id is the current row's parent_id. The topmost ancestor is the single
// chain row with parent_id IS NULL — and THAT row's own `id` is the org root.
//
// We select that parentless row's `id` (aliased root_id). We must NOT carry a
// fixed `id AS root_id` from the recursive seed: that value is just the input
// workspace id, so a non-root caller (e.g. a child delegating to a sibling)
// would resolve to ITSELF instead of its org root, and sameOrg() would wrongly
// report two genuinely same-org workspaces as different orgs and 403 a
// legitimate a2a route. A workspace that already IS an org root has a one-row
// chain whose id == itself, so it correctly resolves to itself.
const orgRootSubtreeCTE = `
	WITH RECURSIVE org_chain AS (
		SELECT id, parent_id
		FROM workspaces
		WHERE id = $1
		UNION ALL
		SELECT w.id, w.parent_id
		FROM workspaces w
		JOIN org_chain c ON w.id = c.parent_id
	)
	SELECT id AS root_id FROM org_chain WHERE parent_id IS NULL LIMIT 1
`

// orgRootID resolves the org root of `workspaceID` by walking the parent_id
// chain via orgRootSubtreeCTE. Returns errNoOrgRoot when the workspace (or its
// chain) yields no org root row, and the underlying error on any DB failure.
//
// This is the SAME lookup the broadcast handler performs inline; the three
// leak paths in #1953 call this instead of re-deriving "the org" from
// `parent_id IS NULL` (which spans all tenants).
func orgRootID(ctx context.Context, database *sql.DB, workspaceID string) (string, error) {
	var root string
	err := database.QueryRowContext(ctx, orgRootSubtreeCTE, workspaceID).Scan(&root)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errNoOrgRoot
	}
	if err != nil {
		return "", err
	}
	if root == "" {
		return "", errNoOrgRoot
	}
	return root, nil
}

// sameOrg reports whether workspaces `a` and `b` share an org root, i.e. they
// belong to the same tenant. Used by a2a routing to reject resolving/dispatching
// to a workspace id outside the caller's org. Fail-CLOSED: any lookup error or
// missing org root yields (false, err) so a DB hiccup denies cross-tenant
// routing rather than allowing it.
func sameOrg(ctx context.Context, database *sql.DB, a, b string) (bool, error) {
	if a == b {
		return true, nil
	}
	rootA, err := orgRootID(ctx, database, a)
	if err != nil {
		return false, err
	}
	rootB, err := orgRootID(ctx, database, b)
	if err != nil {
		return false, err
	}
	return rootA == rootB, nil
}
