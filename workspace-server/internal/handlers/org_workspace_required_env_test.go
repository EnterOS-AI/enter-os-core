package handlers

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCollectPerWorkspaceUnsatisfied_BothFiles covers the case where a key
// is present in both the org root .env and the workspace-specific .env. Both
// should satisfy the requirement (no entry in output).
func TestCollectPerWorkspaceUnsatisfied_BothFiles(t *testing.T) {
	tmp := t.TempDir()
	writeEnvFile(t, tmp, ".env", "PER_WS_KEY=globalvalue")
	writeEnvFile(t, tmp, "ws-a/.env", "PER_WS_KEY=wsvalue")

	workspaces := []OrgWorkspace{
		{Name: "ws-a", FilesDir: "ws-a", RequiredEnv: []EnvRequirement{{Name: "PER_WS_KEY"}}},
	}

	// Global secret covers it.
	globals := map[string]struct{}{"PER_WS_KEY": {}}
	missing := collectPerWorkspaceUnsatisfied(workspaces, tmp, globals)

	if len(missing) != 0 {
		t.Errorf("PER_WS_KEY present in global + .env: should be satisfied, got %d missing", len(missing))
	}
}

// TestCollectPerWorkspaceUnsatisfied_WorkspaceEnvOnly covers a key present
// only in the workspace-specific .env file (not global). Should be satisfied.
func TestCollectPerWorkspaceUnsatisfied_WorkspaceEnvOnly(t *testing.T) {
	tmp := t.TempDir()
	writeEnvFile(t, tmp, "dev-lead/.env", "WORKSPACE_KEY=val")

	workspaces := []OrgWorkspace{
		{Name: "Dev Lead", FilesDir: "dev-lead", RequiredEnv: []EnvRequirement{{Name: "WORKSPACE_KEY"}}},
	}

	globals := map[string]struct{}{} // nothing in global
	missing := collectPerWorkspaceUnsatisfied(workspaces, tmp, globals)

	if len(missing) != 0 {
		t.Errorf("WORKSPACE_KEY in ws .env only: should be satisfied, got %d missing", len(missing))
	}
}

// TestCollectPerWorkspaceUnsatisfied_OrgRootEnvOnly covers a key present
// only in the org root .env file (not per-workspace). Should be satisfied.
func TestCollectPerWorkspaceUnsatisfied_OrgRootEnvOnly(t *testing.T) {
	tmp := t.TempDir()
	writeEnvFile(t, tmp, ".env", "ORG_ROOT_KEY=val")

	workspaces := []OrgWorkspace{
		{Name: "ws-b", FilesDir: "ws-b", RequiredEnv: []EnvRequirement{{Name: "ORG_ROOT_KEY"}}},
	}

	globals := map[string]struct{}{}
	missing := collectPerWorkspaceUnsatisfied(workspaces, tmp, globals)

	if len(missing) != 0 {
		t.Errorf("ORG_ROOT_KEY in org root .env only: should be satisfied, got %d missing", len(missing))
	}
}

// TestCollectPerWorkspaceUnsatisfied_GlobalCovers checks that a global
// secret alone satisfies a per-workspace RequiredEnv even when the .env
// files don't have the key.
func TestCollectPerWorkspaceUnsatisfied_GlobalCovers(t *testing.T) {
	tmp := t.TempDir()
	// No .env files at all.

	workspaces := []OrgWorkspace{
		{Name: "ws-c", RequiredEnv: []EnvRequirement{{Name: "GLOBAL_COVERED"}}},
	}

	globals := map[string]struct{}{"GLOBAL_COVERED": {}}
	missing := collectPerWorkspaceUnsatisfied(workspaces, tmp, globals)

	if len(missing) != 0 {
		t.Errorf("GLOBAL_COVERED satisfied by global: should be satisfied, got %d missing", len(missing))
	}
}

// TestCollectPerWorkspaceUnsatisfied_Missing covers the core bug: a
// RequiredEnv declared at the workspace level where the key is absent from
// both global_secrets and the .env file. The import MUST return 412.
func TestCollectPerWorkspaceUnsatisfied_Missing(t *testing.T) {
	tmp := t.TempDir()
	// No .env files at all.

	workspaces := []OrgWorkspace{
		{Name: "Dev Lead", FilesDir: "dev-lead", RequiredEnv: []EnvRequirement{{Name: "MISSING_REQUIRED_KEY"}}},
	}

	globals := map[string]struct{}{} // no global secret
	missing := collectPerWorkspaceUnsatisfied(workspaces, tmp, globals)

	if len(missing) != 1 {
		t.Fatalf("expected 1 missing entry, got %d", len(missing))
	}
	if missing[0].Workspace != "Dev Lead" {
		t.Errorf("expected workspace 'Dev Lead', got %q", missing[0].Workspace)
	}
	if missing[0].Unsatisfied.Name != "MISSING_REQUIRED_KEY" {
		t.Errorf("expected unsatisfied key 'MISSING_REQUIRED_KEY', got %q", missing[0].Unsatisfied.Name)
	}
	if missing[0].FilesDir != "dev-lead" {
		t.Errorf("expected files_dir 'dev-lead', got %q", missing[0].FilesDir)
	}
}

// TestCollectPerWorkspaceUnsatisfied_AnyOfGroup covers an any-of group where
// none of the alternatives are present in global or .env. Should report
// the group as unsatisfied.
func TestCollectPerWorkspaceUnsatisfied_AnyOfGroup(t *testing.T) {
	tmp := t.TempDir()

	workspaces := []OrgWorkspace{
		{
			Name:     "Claude Bot",
			FilesDir: "claude-bot",
			RequiredEnv: []EnvRequirement{
				{AnyOf: []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"}},
			},
		},
	}

	globals := map[string]struct{}{}
	missing := collectPerWorkspaceUnsatisfied(workspaces, tmp, globals)

	if len(missing) != 1 {
		t.Fatalf("expected 1 missing any-of entry, got %d", len(missing))
	}
	if missing[0].Workspace != "Claude Bot" {
		t.Errorf("expected workspace 'Claude Bot', got %q", missing[0].Workspace)
	}
	if len(missing[0].Unsatisfied.AnyOf) != 2 {
		t.Errorf("expected any-of group with 2 members, got %v", missing[0].Unsatisfied.AnyOf)
	}
}

// TestCollectPerWorkspaceUnsatisfied_NestedChildren covers grandchildren
// workspaces that also declare RequiredEnv. The recursive walk must visit
// children and grandchildren.
func TestCollectPerWorkspaceUnsatisfied_NestedChildren(t *testing.T) {
	tmp := t.TempDir()

	workspaces := []OrgWorkspace{
		{
			Name: "Root",
			Children: []OrgWorkspace{
				{
					Name: "Child",
					Children: []OrgWorkspace{
						{Name: "Grandchild", FilesDir: "grandchild", RequiredEnv: []EnvRequirement{{Name: "DEEP_KEY"}}},
					},
				},
			},
		},
	}

	globals := map[string]struct{}{}
	missing := collectPerWorkspaceUnsatisfied(workspaces, tmp, globals)

	if len(missing) != 1 {
		t.Fatalf("expected 1 missing entry from grandchild, got %d", len(missing))
	}
	if missing[0].Workspace != "Grandchild" {
		t.Errorf("expected 'Grandchild', got %q", missing[0].Workspace)
	}
}

// TestCollectPerWorkspaceUnsatisfied_EmptyOrgBaseDir covers the case where
// orgBaseDir is empty (inline template import). No .env files can be
// checked, so missing keys cannot be attributed to .env absence. The
// function should NOT crash and should only report entries satisfiable
// by global (all missing since globals is empty).
func TestCollectPerWorkspaceUnsatisfied_EmptyOrgBaseDir(t *testing.T) {
	workspaces := []OrgWorkspace{
		{Name: "ws-x", RequiredEnv: []EnvRequirement{{Name: "KEY_X"}}},
	}

	globals := map[string]struct{}{}
	missing := collectPerWorkspaceUnsatisfied(workspaces, "", globals)

	// With no orgBaseDir and no global, KEY_X must be reported missing.
	if len(missing) != 1 {
		t.Errorf("expected 1 missing with empty orgBaseDir, got %d", len(missing))
	}
}

// TestCollectPerWorkspaceUnsatisfied_MultipleWorkspaces reports only the
// workspace whose RequiredEnv is unsatisfied, not the whole batch.
func TestCollectPerWorkspaceUnsatisfied_MultipleWorkspaces(t *testing.T) {
	tmp := t.TempDir()
	writeEnvFile(t, tmp, "ws-ok/.env", "OK_KEY=val")

	workspaces := []OrgWorkspace{
		{Name: "ws-ok", FilesDir: "ws-ok", RequiredEnv: []EnvRequirement{{Name: "OK_KEY"}}},
		{Name: "ws-missing", FilesDir: "ws-missing", RequiredEnv: []EnvRequirement{{Name: "BAD_KEY"}}},
	}

	globals := map[string]struct{}{}
	missing := collectPerWorkspaceUnsatisfied(workspaces, tmp, globals)

	if len(missing) != 1 {
		t.Errorf("expected exactly 1 missing (BAD_KEY), got %d", len(missing))
	}
	if missing[0].Workspace != "ws-missing" {
		t.Errorf("expected missing workspace 'ws-missing', got %q", missing[0].Workspace)
	}
}

// writeEnvFile is a test helper that creates a .env file at the given path
// with the given content.
func writeEnvFile(t *testing.T, baseDir, relPath, content string) {
	t.Helper()
	fullPath := filepath.Join(baseDir, relPath)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		t.Fatalf("mkdirAll: %v", err)
	}
	if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
		t.Fatalf("writeFile %s: %v", fullPath, err)
	}
}
