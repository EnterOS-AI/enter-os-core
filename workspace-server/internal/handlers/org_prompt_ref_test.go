package handlers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// YAML parsing coverage for the new phase-1 scalability fields.
// Catches regressions where someone renames a struct tag and the YAML
// loader silently drops the value (what the prior idle_prompt bug was —
// the struct simply didn't declare the field, so org.yaml entries were
// dropped on import).

func TestOrgYAML_IdlePromptFieldsParse(t *testing.T) {
	src := `
name: Test Org
workspaces:
  - name: Role A
    files_dir: role-a
    idle_prompt: "inline idle body"
    idle_interval_seconds: 300
  - name: Role B
    files_dir: role-b
    idle_prompt_file: idle-prompt.md
    idle_interval_seconds: 600
`
	var tmpl OrgTemplate
	if err := yaml.Unmarshal([]byte(src), &tmpl); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if len(tmpl.Workspaces) != 2 {
		t.Fatalf("expected 2 workspaces, got %d", len(tmpl.Workspaces))
	}
	a := tmpl.Workspaces[0]
	if a.IdlePrompt != "inline idle body" {
		t.Errorf("idle_prompt inline not parsed: %q", a.IdlePrompt)
	}
	if a.IdleIntervalSeconds != 300 {
		t.Errorf("idle_interval_seconds not parsed: %d", a.IdleIntervalSeconds)
	}
	b := tmpl.Workspaces[1]
	if b.IdlePromptFile != "idle-prompt.md" {
		t.Errorf("idle_prompt_file not parsed: %q", b.IdlePromptFile)
	}
	if b.IdleIntervalSeconds != 600 {
		t.Errorf("idle_interval_seconds not parsed: %d", b.IdleIntervalSeconds)
	}
}

func TestOrgYAML_PromptFileFieldsParse(t *testing.T) {
	src := `
name: Test Org
defaults:
  initial_prompt_file: shared-boot.md
workspaces:
  - name: Role A
    files_dir: role-a
    initial_prompt_file: initial-prompt.md
    schedules:
      - name: Hourly audit
        cron_expr: "17 * * * *"
        prompt_file: schedules/hourly-audit.md
`
	var tmpl OrgTemplate
	if err := yaml.Unmarshal([]byte(src), &tmpl); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if tmpl.Defaults.InitialPromptFile != "shared-boot.md" {
		t.Errorf("defaults.initial_prompt_file not parsed: %q", tmpl.Defaults.InitialPromptFile)
	}
	if len(tmpl.Workspaces) != 1 {
		t.Fatalf("expected 1 workspace, got %d", len(tmpl.Workspaces))
	}
	w := tmpl.Workspaces[0]
	if w.InitialPromptFile != "initial-prompt.md" {
		t.Errorf("initial_prompt_file not parsed: %q", w.InitialPromptFile)
	}
	if len(w.Schedules) != 1 {
		t.Fatalf("expected 1 schedule, got %d", len(w.Schedules))
	}
	if w.Schedules[0].PromptFile != "schedules/hourly-audit.md" {
		t.Errorf("schedule.prompt_file not parsed: %q", w.Schedules[0].PromptFile)
	}
}

// resolvePromptRef is the single entry point for reading workspace prompt
// bodies from either inline YAML or sibling .md files. Phase 1 of the
// org.yaml externalization work (1801-line file → ~600-line structural
// manifest). Tests cover the 6 cases callers exercise + the traversal
// defense (same class as resolveInsideRoot).

func TestResolvePromptRef_InlineWinsOverFile(t *testing.T) {
	tmp := t.TempDir()
	rolesDir := filepath.Join(tmp, "my-role")
	if err := os.MkdirAll(rolesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Write a file that would be read if inline were empty — prove inline wins.
	if err := os.WriteFile(filepath.Join(rolesDir, "idle.md"), []byte("FROM FILE"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := resolvePromptRef("FROM INLINE", "idle.md", tmp, "my-role")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "FROM INLINE" {
		t.Errorf("inline should win: got %q", got)
	}
}

func TestResolvePromptRef_FileReadWhenInlineEmpty(t *testing.T) {
	tmp := t.TempDir()
	rolesDir := filepath.Join(tmp, "my-role")
	if err := os.MkdirAll(rolesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	want := "You have no active task.\nBacklog-pull + reflect."
	if err := os.WriteFile(filepath.Join(rolesDir, "idle.md"), []byte(want), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := resolvePromptRef("", "idle.md", tmp, "my-role")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestResolvePromptRef_BothEmptyReturnsEmpty(t *testing.T) {
	tmp := t.TempDir()
	got, err := resolvePromptRef("", "", tmp, "any")
	if err != nil {
		t.Errorf("empty inputs should not error, got: %v", err)
	}
	if got != "" {
		t.Errorf("empty inputs should return empty body, got %q", got)
	}
}

func TestResolvePromptRef_DefaultsLevelResolvesRelativeToOrgBase(t *testing.T) {
	// Defaults don't have a files_dir — ref is resolved relative to
	// orgBaseDir itself (shared prompts at the org root).
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "shared.md"), []byte("SHARED"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := resolvePromptRef("", "shared.md", tmp, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "SHARED" {
		t.Errorf("got %q, want SHARED", got)
	}
}

func TestResolvePromptRef_InlineTemplateErrors(t *testing.T) {
	// When the caller used inline-template mode (POST /org/import with a
	// raw Template in the JSON body, no dir), orgBaseDir is empty and file
	// refs are unresolvable. Surface that loudly.
	_, err := resolvePromptRef("", "idle.md", "", "my-role")
	if err == nil {
		t.Error("expected error for file ref without orgBaseDir")
	}
	if !strings.Contains(err.Error(), "inline-template") {
		t.Errorf("error should mention inline-template mode; got: %v", err)
	}
}

func TestResolvePromptRef_RejectsTraversalViaFileRef(t *testing.T) {
	tmp := t.TempDir()
	rolesDir := filepath.Join(tmp, "my-role")
	if err := os.MkdirAll(rolesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Create a sensitive file OUTSIDE the orgBaseDir we can try to exfiltrate.
	outside := filepath.Join(filepath.Dir(tmp), "secret.md")
	if err := os.WriteFile(outside, []byte("SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(outside)

	cases := []string{
		"../../secret.md",
		"../secret.md",
		"../../../../etc/passwd",
	}
	for _, tc := range cases {
		t.Run(tc, func(t *testing.T) {
			_, err := resolvePromptRef("", tc, tmp, "my-role")
			if err == nil {
				t.Errorf("expected error for traversal %q", tc)
			}
		})
	}
}

func TestResolvePromptRef_RejectsTraversalViaFilesDir(t *testing.T) {
	tmp := t.TempDir()
	// The files_dir argument also comes from YAML — ensure it can't escape.
	_, err := resolvePromptRef("", "idle.md", tmp, "../escape")
	if err == nil {
		t.Error("expected error for traversal via files_dir")
	}
}

func TestResolvePromptRef_MissingFileErrors(t *testing.T) {
	tmp := t.TempDir()
	rolesDir := filepath.Join(tmp, "my-role")
	if err := os.MkdirAll(rolesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := resolvePromptRef("", "nonexistent.md", tmp, "my-role")
	if err == nil {
		t.Error("expected error for missing file")
	}
}
