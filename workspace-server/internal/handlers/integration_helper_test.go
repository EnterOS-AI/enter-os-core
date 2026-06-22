//go:build integration
// +build integration

// integration_helper_test.go — shared preflight for handler Postgres
// integration tests. Extracted so the fail-open/skip logic is in ONE place
// and can be tightened without editing every integration test file.
//
// See delegation_ledger_integration_test.go for the docker-postgres setup
// incantation used by local devs.

package handlers

import (
	"os"
	"testing"
)

// requireIntegrationDBURL returns $INTEGRATION_DB_URL.
//
// In CI (CI, GITHUB_ACTIONS, or GITEA_ACTIONS env var is non-empty), an
// empty URL is a fatal error — it means the workflow failed to export the
// variable (postgres container did not start, bridge IP resolution failed,
// or a regression in the workflow YAML). t.Fatalf keeps the test red so the
// failure is visible; t.Skip would silently pass and mask the defect.
//
// Locally (none of the three CI markers set), an empty URL skips the test
// so devs can run `go test ./...` without booting a Postgres container.
func requireIntegrationDBURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("INTEGRATION_DB_URL")
	if url == "" {
		if os.Getenv("CI") != "" ||
			os.Getenv("GITHUB_ACTIONS") != "" ||
			os.Getenv("GITEA_ACTIONS") != "" {
			t.Fatalf("INTEGRATION_DB_URL required in CI handler integration tests — check workflow env export")
		}
		t.Skip("INTEGRATION_DB_URL not set; skipping (local devs: see file header)")
	}
	return url
}
