//go:build integration
// +build integration

// integration_test_helpers_test.go — shared helpers for the
// `//go:build integration` test files.
//
// The handlers package uses github.com/google/uuid in production code
// (workspaces.id, workspace_schedules.workspace_id, activity_logs.workspace_id,
// and workspace_auth_tokens.workspace_id are all UUID columns — see
// migrations 001_workspaces.sql, 015_workspace_schedules.sql,
// 009_activity_logs.sql, 020_workspace_auth_tokens.up.sql). Real
// Postgres rejects non-UUID-shaped strings on insert.
//
// The integration tests in this package want human-readable fixture
// names so failures print obviously ("integ-sch-ws-a", not a random
// UUID). integUUID is a tiny helper that maps any string to a
// stable UUID via SHA-1 in the URL namespace — same input → same
// UUID, different inputs → different UUIDs. The test can keep its
// readable names but every place that needs a UUID-shaped value
// passes through this helper.
//
// Cleanup is driven off `workspaces.name` (a TEXT column we set to
// the test marker) rather than `workspaces.id` (a UUID column) so
// we don't have to keep a running list of generated UUIDs in sync
// between the test body and the cleanup helper.

package handlers

import "github.com/google/uuid"

// integUUID returns a deterministic UUID derived from s. The URL
// namespace keeps the input space disjoint from production UUIDs
// (which use the random v4 generator) and from the OID namespace
// (which uuid.NewSHA1 would default to).
func integUUID(s string) string {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(s)).String()
}
