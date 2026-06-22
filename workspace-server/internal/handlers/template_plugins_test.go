package handlers

// template_plugins_test.go — unit tests for parseTemplatePlugins +
// seedTemplatePlugins (RFC#2843 #32). Proves a workspace created directly via
// WorkspaceHandler.Create from a template that declares plugins:
//   1. parses the template config.yaml's `plugins:` block, and
//   2. WRITES the workspace_declared_plugins rows the post-online reconcile
//      needs (the gap this change closes: recordDeclaredPlugin previously ran
//      only in the org/import path, so a singly-provisioned seo-agent got no
//      declared rows → reconcile no-op → seo-all never installed).

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestParseTemplatePlugins_AbsentFile(t *testing.T) {
	got, err := parseTemplatePlugins(t.TempDir())
	if err != nil {
		t.Fatalf("expected nil error for absent config.yaml, got %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil slice, got %#v", got)
	}
}

func TestParseTemplatePlugins_EmptyPath(t *testing.T) {
	got, err := parseTemplatePlugins("")
	if err != nil {
		t.Fatalf("expected nil error for empty path, got %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil slice for empty path, got %#v", got)
	}
}

func TestParseTemplatePlugins_NoPluginsBlock(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "config.yaml"), `
name: Some Template
runtime: claude-code
model: foo/bar
`)
	got, err := parseTemplatePlugins(dir)
	if err != nil {
		t.Fatalf("expected nil error when plugins: absent, got %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected zero plugins, got %d: %v", len(got), got)
	}
}

// TestParseTemplatePlugins_SeoAgentShape pins the real seo-agent template
// config.yaml shape: a top-level `plugins:` list with the seo-all gitea source.
func TestParseTemplatePlugins_SeoAgentShape(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "config.yaml"), `
name: SEO Agent
runtime: claude-code
plugins:
  - gitea://molecule-ai/molecule-ai-workspace-template-seo-agent/agent-skills/seo-all#main
runtime_config:
  model: moonshot/kimi-k2.6
`)
	got, err := parseTemplatePlugins(dir)
	if err != nil {
		t.Fatalf("parseTemplatePlugins: %v", err)
	}
	want := "gitea://molecule-ai/molecule-ai-workspace-template-seo-agent/agent-skills/seo-all#main"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("expected [%q], got %v", want, got)
	}
}

// TestParseTemplatePlugins_DedupAndOptOut pins the mergePlugins-aligned
// semantics: duplicate sources collapse and a leading "!"/"-" opts a plugin
// out (matching the org/import path).
func TestParseTemplatePlugins_DedupAndOptOut(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "config.yaml"), `
name: T
plugins:
  - local://alpha
  - local://beta
  - local://alpha
  - "!local://beta"
`)
	got, err := parseTemplatePlugins(dir)
	if err != nil {
		t.Fatalf("parseTemplatePlugins: %v", err)
	}
	if len(got) != 1 || got[0] != "local://alpha" {
		t.Fatalf("expected dedup + beta opted out → [local://alpha], got %v", got)
	}
}

// TestSeedTemplatePlugins_WritesDeclaredRows is the load-bearing FIX-B proof:
// seedTemplatePlugins derives the install name from each source and UPSERTS a
// workspace_declared_plugins row. Uses sqlmock so the actual INSERT (and its
// derived plugin_name) is asserted — recordDeclaredPlugin no-ops when db.DB is
// nil, so a DB-backed test is required to prove the write really happens.
func TestSeedTemplatePlugins_WritesDeclaredRows(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()

	source := "gitea://molecule-ai/molecule-ai-workspace-template-seo-agent/agent-skills/seo-all#main"
	// gitea source with subpath → install name is the last subpath segment:
	// "seo-all". The upsert keys on (workspace_id, plugin_name) and stores the
	// full source string in source_raw.
	mock.ExpectExec(`INSERT INTO workspace_declared_plugins`).
		WithArgs("ws-create-1", "seo-all", source).
		WillReturnResult(sqlmock.NewResult(1, 1))

	recorded, skipped := seedTemplatePlugins(context.Background(), "ws-create-1", []string{source})
	if recorded != 1 || skipped != 0 {
		t.Fatalf("expected recorded=1 skipped=0, got recorded=%d skipped=%d", recorded, skipped)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations (the declared row was not written as expected): %v", err)
	}
}

// TestSeedTemplatePlugins_SkipsUnparseableSource proves a bad source is skipped
// (logged) rather than aborting the rest of the declared set — and crucially
// does NOT issue a DB write for it.
func TestSeedTemplatePlugins_SkipsUnparseableSource(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()

	// A scheme with no known naming rule → PluginNameFromSource errors → skip.
	// No ExpectExec is programmed; sqlmock fails the test if any unexpected
	// INSERT fires.
	recorded, skipped := seedTemplatePlugins(context.Background(), "ws-create-1", []string{"bogus-scheme://whatever"})
	if recorded != 0 || skipped != 1 {
		t.Fatalf("expected recorded=0 skipped=1 for an unparseable source, got recorded=%d skipped=%d", recorded, skipped)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}
}
