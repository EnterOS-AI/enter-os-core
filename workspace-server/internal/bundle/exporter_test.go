package bundle

import (
	"os"
	"path/filepath"
	"testing"
)

// ---------------------------------------------------------------------------
// extractDescription
// ---------------------------------------------------------------------------

func TestExtractDescription_WithFrontmatter(t *testing.T) {
	// YAML frontmatter is skipped; first non-comment, non-empty line after
	// the closing `---` is the description.
	content := `---
title: My Workspace
---
# This is a comment
This is the description line.
Another line.`
	got := extractDescription(content)
	if got != "This is the description line." {
		t.Errorf("got %q, want %q", got, "This is the description line.")
	}
}

func TestExtractDescription_NoFrontmatter(t *testing.T) {
	// No frontmatter: first non-comment, non-empty line is returned.
	content := `# Copyright header
My workspace description
Another line.`
	got := extractDescription(content)
	if got != "My workspace description" {
		t.Errorf("got %q, want %q", got, "My workspace description")
	}
}

func TestExtractDescription_CommentOnly(t *testing.T) {
	// All content is comments or empty → empty string.
	content := `# comment only
# another comment
`
	got := extractDescription(content)
	if got != "" {
		t.Errorf("got %q, want empty string", got)
	}
}

func TestExtractDescription_EmptyInput(t *testing.T) {
	got := extractDescription("")
	if got != "" {
		t.Errorf("got %q, want empty string", got)
	}
}

func TestExtractDescription_UnclosedFrontmatter(t *testing.T) {
	// With no closing `---`, inFrontmatter stays true after the opening
	// delimiter, so all subsequent lines are skipped and "" is returned.
	// This is the documented behaviour: without a closing delimiter,
	// all lines are considered frontmatter.
	content := `---
title: No closing delimiter
This is the description.`
	got := extractDescription(content)
	if got != "" {
		t.Errorf("unclosed frontmatter: got %q, want empty string", got)
	}
}

func TestExtractDescription_FrontmatterThenCommentThenContent(t *testing.T) {
	content := `---
tags: [test]
---
# internal comment
Real description here.
`
	got := extractDescription(content)
	if got != "Real description here." {
		t.Errorf("got %q, want %q", got, "Real description here.")
	}
}

func TestExtractDescription_BlankLinesSkipped(t *testing.T) {
	// Empty lines (len=0) are skipped; whitespace-only lines (spaces) are NOT
	// skipped because len(line)>0. First non-comment, non-empty line is returned.
	content := "\n\n\n\nA. Description\nB. Should not be returned.\n"
	got := extractDescription(content)
	if got != "A. Description" {
		t.Errorf("got %q, want %q", got, "A. Description")
	}
}

// ---------------------------------------------------------------------------
// splitLines
// ---------------------------------------------------------------------------

func TestSplitLines_Basic(t *testing.T) {
	got := splitLines("a\nb\nc")
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("len=%d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d]=%q, want %q", i, got[i], want[i])
		}
	}
}

func TestSplitLines_TrailingNewline(t *testing.T) {
	got := splitLines("line1\nline2\n")
	want := []string{"line1", "line2"}
	if len(got) != len(want) {
		t.Errorf("trailing newline: got %v, want %v", got, want)
	}
}

func TestSplitLines_NoNewline(t *testing.T) {
	got := splitLines("no newline")
	want := []string{"no newline"}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestSplitLines_EmptyString(t *testing.T) {
	got := splitLines("")
	if len(got) != 0 {
		t.Errorf("empty string: got %v, want []", got)
	}
}

func TestSplitLines_OnlyNewlines(t *testing.T) {
	got := splitLines("\n\n\n")
	// Three consecutive '\n' characters → s[start:i] at each '\n' gives
	// the empty string between newlines → 3 empty segments.
	// (No trailing segment because start == len(s) at the end.)
	if len(got) != 3 {
		t.Errorf("only newlines: got %v (len=%d), want 3 empty strings", got, len(got))
	}
	for i, s := range got {
		if s != "" {
			t.Errorf("got[%d]=%q, want empty string", i, s)
		}
	}
}

func TestSplitLines_MultipleConsecutiveNewlines(t *testing.T) {
	got := splitLines("a\n\n\nb")
	// a\n\n\nb → ["a", "", "", "b"]
	if len(got) != 4 {
		t.Errorf("consecutive newlines: got %v (len=%d)", got, len(got))
	}
	if got[0] != "a" || got[3] != "b" {
		t.Errorf("first/last: got %v, want [a, ..., b]", got)
	}
}

// ---------------------------------------------------------------------------
// findConfigDir
// ---------------------------------------------------------------------------

func TestFindConfigDir_NameMatch(t *testing.T) {
	tmp := t.TempDir()

	// Create two sub-dirs; only the one with matching name should be found.
	mustMkdir(filepath.Join(tmp, "workspace-a"))
	mustWrite(filepath.Join(tmp, "workspace-a", "config.yaml"),
		"name: other-workspace\ntier: 1\n")

	mustMkdir(filepath.Join(tmp, "workspace-b"))
	mustWrite(filepath.Join(tmp, "workspace-b", "config.yaml"),
		"name: target-workspace\nruntime: claude-code\n")

	got := findConfigDir(tmp, "target-workspace")
	want := filepath.Join(tmp, "workspace-b")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFindConfigDir_NoMatch_UsesFallback(t *testing.T) {
	tmp := t.TempDir()

	mustMkdir(filepath.Join(tmp, "first"))
	mustWrite(filepath.Join(tmp, "first", "config.yaml"), "name: workspace-a\n")

	mustMkdir(filepath.Join(tmp, "second"))
	mustWrite(filepath.Join(tmp, "second", "config.yaml"), "name: workspace-b\n")

	// No exact name match → fallback to the first directory with a config.yaml.
	got := findConfigDir(tmp, "nonexistent")
	want := filepath.Join(tmp, "first")
	if got != want {
		t.Errorf("no match: got %q, want fallback %q", got, want)
	}
}

func TestFindConfigDir_MissingDir(t *testing.T) {
	got := findConfigDir("/nonexistent/path/for/findConfigDir", "any-name")
	if got != "" {
		t.Errorf("missing dir: got %q, want empty string", got)
	}
}

func TestFindConfigDir_NoSubdirs(t *testing.T) {
	tmp := t.TempDir()
	// Empty directory → no matches, no fallback.
	got := findConfigDir(tmp, "any")
	if got != "" {
		t.Errorf("empty dir: got %q, want empty string", got)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func mustMkdir(path string) {
	os.MkdirAll(path, 0o755)
}

func mustWrite(path, content string) {
	os.WriteFile(path, []byte(content), 0o644)
}

// ---------------------------------------------------------------------------
// findConfigDir
// ---------------------------------------------------------------------------

func TestFindConfigDir_SubdirWithoutConfig(t *testing.T) {
	tmp := t.TempDir()
	mustMkdir(filepath.Join(tmp, "empty-skill"))
	// Sub-dir without config.yaml → skipped.
	got := findConfigDir(tmp, "any")
	if got != "" {
		t.Errorf("no config.yaml: got %q, want empty string", got)
	}
}

func TestFindConfigDir_FirstWithConfigIsFallback(t *testing.T) {
	// When name doesn't match, fallback is the FIRST dir with config.yaml,
	// not the last. Confirm ordering by creating three dirs.
	tmp := t.TempDir()

	mustMkdir(filepath.Join(tmp, "a"))
	mustWrite(filepath.Join(tmp, "a", "config.yaml"), "name: alpha\n")

	mustMkdir(filepath.Join(tmp, "b"))
	mustWrite(filepath.Join(tmp, "b", "config.yaml"), "name: beta\n")

	mustMkdir(filepath.Join(tmp, "c"))
	mustWrite(filepath.Join(tmp, "c", "config.yaml"), "name: gamma\n")

	got := findConfigDir(tmp, "nonexistent")
	want := filepath.Join(tmp, "a") // first dir with config.yaml
	if got != want {
		t.Errorf("fallback order: got %q, want first-with-config %q", got, want)
	}
}
