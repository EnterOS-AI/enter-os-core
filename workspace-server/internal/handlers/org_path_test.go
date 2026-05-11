package handlers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Exercises resolveInsideRoot — the SSRF-class path sanitizer used by
// POST /org/import for `dir` / `template` / `files_dir`. Issue #103.
// The helper is the single chokepoint preventing `../../../etc` escape,
// so it earns a dedicated test file.

func TestResolveInsideRoot_HappyPath(t *testing.T) {
	tmp := t.TempDir()
	sub := filepath.Join(tmp, "molecule-dev")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := resolveInsideRoot(tmp, "molecule-dev")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Compare via Abs to tolerate macOS /private symlink normalization.
	wantAbs, _ := filepath.Abs(sub)
	if got != wantAbs {
		t.Errorf("got %q, want %q", got, wantAbs)
	}
}

func TestResolveInsideRoot_RejectsTraversal(t *testing.T) {
	tmp := t.TempDir()
	cases := []string{
		"../etc",
		"../../etc/passwd",
		"molecule-dev/../../..",
		"../../../../../../../../../etc",
	}
	for _, tc := range cases {
		t.Run(tc, func(t *testing.T) {
			if _, err := resolveInsideRoot(tmp, tc); err == nil {
				t.Errorf("expected error for %q, got nil", tc)
			}
		})
	}
}

func TestResolveInsideRoot_RejectsAbsolute(t *testing.T) {
	tmp := t.TempDir()
	if _, err := resolveInsideRoot(tmp, "/etc/passwd"); err == nil {
		t.Error("absolute path must be rejected")
	}
}

func TestResolveInsideRoot_RejectsEmpty(t *testing.T) {
	tmp := t.TempDir()
	if _, err := resolveInsideRoot(tmp, ""); err == nil {
		t.Error("empty path must be rejected")
	}
}

// A path whose Abs shares a prefix with root but is NOT inside root must be
// rejected. Catches the classic string-prefix bug where "/foo" matches
// "/foobar".
func TestResolveInsideRoot_RejectsPrefixSibling(t *testing.T) {
	tmp := t.TempDir()
	sibling := tmp + "-sibling"
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sibling) })

	// Use a relative path that lexically resolves to the sibling directory.
	up := "../" + filepath.Base(sibling)
	if _, err := resolveInsideRoot(tmp, up); err == nil {
		t.Errorf("sibling-prefix path %q must be rejected", up)
	}
}

// TestResolveInsideRoot_RejectsSymlinkTraversal is a regression test for
// CWE-59 (symlink-based path traversal). An attacker plants a symlink inside
// the allowed directory that points outside; the function must reject it.
func TestResolveInsideRoot_RejectsSymlinkTraversal(t *testing.T) {
	tmp := t.TempDir()
	// Create a subdirectory inside root.
	inner := filepath.Join(tmp, "workspaces", "dev")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	// Plant a symlink that resolves outside root.
	sym := filepath.Join(inner, "leaked")
	if err := os.Symlink("/etc", sym); err != nil {
		t.Fatal(err)
	}

	// Lexically, "workspaces/dev/leaked" is inside tmp — but after symlink
	// resolution it points to /etc and must be rejected.
	if _, err := resolveInsideRoot(tmp, filepath.Join("workspaces", "dev", "leaked")); err == nil {
		t.Error("symlink pointing outside root must be rejected (CWE-59)")
	}

	// Symlink that stays inside root is fine.
	safe := filepath.Join(inner, "safe")
	if err := os.Symlink(filepath.Join(tmp, "other"), safe); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveInsideRoot(tmp, filepath.Join("workspaces", "dev", "safe")); err != nil {
		t.Errorf("symlink staying inside root must be allowed: %v", err)
	}

	// Broken symlink (target does not exist) must also be rejected — broken
	// symlinks cannot be valid org files.
	broken := filepath.Join(inner, "broken")
	if err := os.Symlink("/nonexistent/broken", broken); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveInsideRoot(tmp, filepath.Join("workspaces", "dev", "broken")); err == nil {
		t.Error("broken symlink must be rejected")
	}
}

func TestResolveInsideRoot_DeepSubpath(t *testing.T) {
	tmp := t.TempDir()
	deep := filepath.Join(tmp, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := resolveInsideRoot(tmp, "a/b/c")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantAbs, _ := filepath.Abs(deep)
	if got != wantAbs {
		t.Errorf("got %q want %q", got, wantAbs)
	}
	// Sanity: result is a strict descendant of root.
	rootAbs, _ := filepath.Abs(tmp)
	if !strings.HasPrefix(got, rootAbs+string(filepath.Separator)) {
		t.Errorf("result %q is not inside %q", got, rootAbs)
	}
}
