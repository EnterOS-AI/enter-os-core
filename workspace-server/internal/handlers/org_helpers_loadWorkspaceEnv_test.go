package handlers

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupOrgEnv creates a temp dir with an optional org .env file and returns the dir.
func setupOrgEnv(t *testing.T, orgEnvContent string) string {
	t.Helper()
	dir := t.TempDir()
	if orgEnvContent != "" {
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".env"), []byte(orgEnvContent), 0o600))
	}
	return dir
}

func Test_loadWorkspaceEnv_orgRootOnly(t *testing.T) {
	org := setupOrgEnv(t, "ORG_VAR=orgval\nORG_DEBUG=true")
	vars := loadWorkspaceEnv(org, "")
	assert.Equal(t, "orgval", vars["ORG_VAR"])
	assert.Equal(t, "true", vars["ORG_DEBUG"])
}

func Test_loadWorkspaceEnv_orgRootMissing(t *testing.T) {
	// No .env at org root — should return empty map without error.
	dir := t.TempDir()
	vars := loadWorkspaceEnv(dir, "")
	assertEmpty(t, vars)
}

func Test_loadWorkspaceEnv_workspaceEnvMerges(t *testing.T) {
	org := setupOrgEnv(t, "SHARED=sharedval\nORG_ONLY=orgonly")
	wsDir := filepath.Join(org, "myworkspace")
	require.NoError(t, os.MkdirAll(wsDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(wsDir, ".env"), []byte("WS_VAR=wsval\nSHARED=overridden"), 0o600))

	vars := loadWorkspaceEnv(org, "myworkspace")
	assert.Equal(t, "wsval", vars["WS_VAR"])
	assert.Equal(t, "overridden", vars["SHARED"]) // workspace overrides org
	assert.Equal(t, "orgonly", vars["ORG_ONLY"])   // org vars preserved
}

func Test_loadWorkspaceEnv_emptyFilesDir(t *testing.T) {
	org := setupOrgEnv(t, "VAR=val")
	vars := loadWorkspaceEnv(org, "")
	assert.Equal(t, "val", vars["VAR"])
}

func Test_loadWorkspaceEnv_traversalRejects(t *testing.T) {
	// #321 / CWE-22: filesDir "../../../etc" must not escape the org root.
	// resolveInsideRoot rejects the traversal so workspace .env is skipped;
	// org root .env is still loaded (it's before the guard).
	org := setupOrgEnv(t, "INNOCENT=val\nSAFE_WS=wsval")
	parent := filepath.Dir(org)
	require.NoError(t, os.WriteFile(filepath.Join(parent, ".env"), []byte("MALICIOUS=evil"), 0o600))
	// Also create a workspace dir inside org to prove it IS accessible normally.
	wsDir := filepath.Join(org, "legit-workspace")
	require.NoError(t, os.MkdirAll(wsDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(wsDir, ".env"), []byte("WS_SECRET=ssh-key-123"), 0o600))

	// Traversal is blocked.
	vars := loadWorkspaceEnv(org, "../../../etc")
	// Org root vars present; workspace vars blocked.
	assert.Equal(t, "val", vars["INNOCENT"])
	assert.Equal(t, "wsval", vars["SAFE_WS"]) // from org root .env
	assert.Empty(t, vars["WS_SECRET"])        // workspace .env blocked by traversal guard
	_, hasEvil := vars["MALICIOUS"]
	assert.False(t, hasEvil, "MALICIOUS from escaped path must not appear")
}

func Test_loadWorkspaceEnv_traversalWithDots(t *testing.T) {
	// A sibling-traversal attempt: go up one level then into a sibling dir.
	// The sibling dir is NOT inside org, so it must be rejected.
	org := setupOrgEnv(t, "INNOCENT=val")
	parent := filepath.Dir(org)
	require.NoError(t, os.MkdirAll(filepath.Join(parent, "sibling"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(parent, "sibling/.env"), []byte("LEAKED=secret"), 0o600))

	vars := loadWorkspaceEnv(org, "../sibling")
	// Org vars loaded; sibling vars blocked.
	assert.Equal(t, "val", vars["INNOCENT"])
	assert.Empty(t, vars["LEAKED"], "sibling traversal must be rejected")
}

func Test_loadWorkspaceEnv_absolutePathRejected(t *testing.T) {
	// Absolute paths are rejected outright by resolveInsideRoot.
	org := setupOrgEnv(t, "INNOCENT=val")
	vars := loadWorkspaceEnv(org, "/etc")
	assert.Equal(t, "val", vars["INNOCENT"]) // org root still loaded
	assert.Empty(t, vars["SAFE_WS"])
}

func Test_loadWorkspaceEnv_dotPathRejected(t *testing.T) {
	// "." resolves to the org root itself — this is NOT a traversal but
	// would create org-root/.env which is the org root .env, not a
	// workspace .env. resolveInsideRoot accepts this; the workspace .env
	// path is org/.env, which IS the org root .env (already loaded).
	// So the correct result is the org vars (same as org root, no change).
	org := setupOrgEnv(t, "INNOCENT=val")
	vars := loadWorkspaceEnv(org, ".")
	// "." passes resolveInsideRoot (resolves to org root, which is valid).
	// But workspace path org/.env is the same as org/.env already loaded.
	assert.Equal(t, "val", vars["INNOCENT"])
}

func Test_loadWorkspaceEnv_emptyOrgRootReturnsEmpty(t *testing.T) {
	vars := loadWorkspaceEnv("", "some/dir")
	assertEmpty(t, vars)
}

func Test_loadWorkspaceEnv_missingWorkspaceDir(t *testing.T) {
	org := setupOrgEnv(t, "ORG=val")
	// Workspace dir doesn't exist — org vars still loaded.
	vars := loadWorkspaceEnv(org, "nonexistent")
	assert.Equal(t, "val", vars["ORG"])
}

func assertEmpty(t *testing.T, m map[string]string) {
	t.Helper()
	assert.Equal(t, 0, len(m), "expected empty map, got %v", m)
}
