// Package staginge2e holds live, against-real-staging-infra end-to-end tests
// for molecule-core's workspace-server that are NOT part of the normal
// `go test ./...` run and NOT part of any unit/httptest suite.
//
// Every test here is guarded by the `staging_e2e` build tag AND skips itself
// at runtime unless the required staging credentials are present in the
// environment (see requireStagingEnv). So:
//
//	go test ./...                      # compiles nothing here (tag absent)
//	go test -tags=staging_e2e ./...    # compiles; skips LOUD if creds absent
//	STAGING_E2E=1 CP_BASE_URL=... CP_ADMIN_API_TOKEN=... \
//	  go test -tags=staging_e2e -run TestWorkspaceLifecycle_Staging \
//	  -timeout 40m ./internal/staginge2e/
//
// These tests provision a REAL throwaway tenant (real EC2-backed workspace on
// staging) via the CP admin API, drive the workspace lifecycle endpoints
// against the live tenant ws-server, and assert OBSERVABLE container-state
// transitions (status + serve reachability) — not just HTTP 200. Teardown is
// t.Cleanup-driven (admin DELETE /cp/admin/tenants).
//
// Run them from the operator host (or CI on dispatch/schedule) where the
// staging CP admin surface + tenant DNS are reachable.
//
// This suite is advisory-by-infra: it needs a live staging tenant, so it is
// NOT a merge-blocking required check. Promotion to required is a separate CTO
// decision (mirrors the cp internal/staginge2e suite, cp#386).
package staginge2e
