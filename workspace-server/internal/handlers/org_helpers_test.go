package handlers

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadWorkspaceEnv_RejectsTraversal asserts that loadWorkspaceEnv refuses
// to read workspace-specific .env files when filesDir contains CWE-22 traversal
// patterns (../../../etc, absolute paths, etc.). This is the primary security
// control for the ws.FilesDir attack surface in POST /org/import.

func TestLoadWorkspaceEnv_RejectsTraversal(t *testing.T) {
	tmp := t.TempDir()
	orgRoot := filepath.Join(tmp, "my-org")
	if err := os.Mkdir(orgRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		filesDir string
	}{
		{"traversal_parent", "../../../etc"},
		{"traversal_deep", "../../../../../../../../../etc"},
		{"traversal_sibling", "../sibling"},
		{"traversal_mixed", "foo/../../bar"},
		{"absolute_path", "/etc/passwd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Write an org-level .env to confirm it loads even when the
			// workspace .env is rejected.
			orgEnv := filepath.Join(orgRoot, ".env")
			if err := os.WriteFile(orgEnv, []byte("ORG_KEY=org-value\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			got := loadWorkspaceEnv(orgRoot, tc.filesDir)

			// Org-level .env must be loaded regardless of workspace rejection.
			if got["ORG_KEY"] != "org-value" {
				t.Errorf("org-level .env not loaded: got %v", got)
			}
			// Traversal path must NOT have been read.
			if val, ok := got["TRAVERSAL_KEY"]; ok {
				t.Errorf("traversal escaped: got TRAVERSAL_KEY=%q", val)
			}
		})
	}
}

// TestLoadWorkspaceEnv_HappyPath verifies that legitimate filesDir values
// resolve correctly and workspace .env overrides org-level values.

func TestLoadWorkspaceEnv_HappyPath(t *testing.T) {
	tmp := t.TempDir()
	orgRoot := filepath.Join(tmp, "my-org")
	wsDir := filepath.Join(orgRoot, "workspaces", "dev-workspace")
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	orgEnv := filepath.Join(orgRoot, ".env")
	wsEnv := filepath.Join(wsDir, ".env")
	if err := os.WriteFile(orgEnv, []byte("ORG_KEY=org-val\nSHARED=org-wins\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wsEnv, []byte("WS_KEY=ws-val\nSHARED=ws-wins\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := loadWorkspaceEnv(orgRoot, filepath.Join("workspaces", "dev-workspace"))

	if got["ORG_KEY"] != "org-val" {
		t.Errorf("org-level key missing: %v", got)
	}
	if got["WS_KEY"] != "ws-val" {
		t.Errorf("workspace key missing: %v", got)
	}
	if got["SHARED"] != "ws-wins" {
		t.Errorf("workspace should override org-level: got %v", got)
	}
}

// TestLoadWorkspaceEnv_EmptyFilesDirOnlyLoadsOrgLevel verifies that an empty
// filesDir only loads the org-level .env (no workspace override).

func TestLoadWorkspaceEnv_EmptyFilesDir(t *testing.T) {
	tmp := t.TempDir()
	orgRoot := filepath.Join(tmp, "my-org")
	if err := os.Mkdir(orgRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orgRoot, ".env"), []byte("KEY=only-org\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := loadWorkspaceEnv(orgRoot, "")
	if got["KEY"] != "only-org" {
		t.Errorf("expected only-org, got %v", got)
	}
}
