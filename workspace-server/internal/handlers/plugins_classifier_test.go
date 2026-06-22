package handlers

import (
	"os"
	"path/filepath"
	"testing"
)

// TestIsSkillMarkdown: pin which paths the classifier considers
// hot-reloadable. SKILL.md by basename only — case-sensitive.
func TestIsSkillMarkdown(t *testing.T) {
	yes := []string{
		"SKILL.md",
		"skills/foo/SKILL.md",
		"deeply/nested/SKILL.md",
	}
	no := []string{
		"plugin.yaml",
		"hooks.json",
		"settings.json",
		"README.md",
		"skill.md",  // case-sensitive
		"SKILLS.md", // not a skill file
		"skills/foo/extra.md",
	}
	for _, s := range yes {
		if !isSkillMarkdown(s) {
			t.Errorf("isSkillMarkdown(%q) = false; want true", s)
		}
	}
	for _, s := range no {
		if isSkillMarkdown(s) {
			t.Errorf("isSkillMarkdown(%q) = true; want false", s)
		}
	}
}

// TestHashLocalTree_StableHash: hashing the same content twice must
// produce identical maps. Pinned because if hashLocalTree ever picks up
// mtime/inode (e.g. via a refactor to use os.Lstat metadata), every
// install would classify as cold and we'd lose the hot-reload.
func TestHashLocalTree_StableHash(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "skills/foo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte("name: foo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "skills/foo/SKILL.md"), []byte("# Foo\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	h1, err := hashLocalTree(dir)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := hashLocalTree(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(h1) != len(h2) {
		t.Fatalf("hash count differs: %d vs %d", len(h1), len(h2))
	}
	for k, v := range h1 {
		if h2[k] != v {
			t.Errorf("hash[%q] differs: %q vs %q", k, v, h2[k])
		}
	}
}

// TestHashLocalTree_SymlinkSkipped: symlinks should not appear in the
// hash map — same posture as the tar walker. Otherwise a hostile plugin
// could include a symlink whose hash changes when its target changes,
// silently flipping classification.
func TestHashLocalTree_SymlinkSkipped(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "real.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}

	h, err := hashLocalTree(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := h["link"]; exists {
		t.Errorf("symlink leaked into hash map: %v", h)
	}
	if _, exists := h["real.txt"]; !exists {
		t.Errorf("real.txt missing from hash map: %v", h)
	}
}

// TestShQuote: the classifier injects livePath into a shell command via
// docker exec. Path must be quoted to handle pluginName entries with
// hyphens (which are safe but exercised here) and any future special-
// character edge case. Pin the safe-vs-quoted boundary.
func TestShQuote(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"foo", "foo"},
		{"/configs/plugins/foo-bar", "/configs/plugins/foo-bar"},
		{"with space", "'with space'"},
		{"with'quote", "'with'\\''quote'"},
		{"$envvar", "'$envvar'"},
		{"path/with/dots.txt", "path/with/dots.txt"},
	}
	for _, tc := range cases {
		if got := shQuote(tc.in); got != tc.want {
			t.Errorf("shQuote(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}
