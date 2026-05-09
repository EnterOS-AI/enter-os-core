package plugins

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePlugin(t *testing.T, base, name string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(base, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for path, content := range files {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLocalResolver_Scheme(t *testing.T) {
	if (&LocalResolver{}).Scheme() != "local" {
		t.Error("scheme must be 'local'")
	}
}

func TestLocalResolver_CopiesPluginTree(t *testing.T) {
	base := t.TempDir()
	writePlugin(t, base, "demo", map[string]string{
		"plugin.yaml":               "name: demo\n",
		"rules/one.md":              "- rule",
		"skills/hello/SKILL.md":     "---\nname: hello\ndescription: d\n---\nbody",
		"skills/hello/scripts/t.py": "# tool",
	})

	dst := t.TempDir()
	r := NewLocalResolver(base)
	name, err := r.Fetch(context.Background(), "demo", dst)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if name != "demo" {
		t.Errorf("got name %q", name)
	}
	// Verify every file made it across.
	for _, want := range []string{
		"plugin.yaml",
		"rules/one.md",
		"skills/hello/SKILL.md",
		"skills/hello/scripts/t.py",
	} {
		if _, err := os.Stat(filepath.Join(dst, want)); err != nil {
			t.Errorf("missing %q in dst: %v", want, err)
		}
	}
}

func TestLocalResolver_RejectsPathTraversal(t *testing.T) {
	r := NewLocalResolver(t.TempDir())
	cases := []string{
		"",
		"   ",
		"../evil",
		"foo/bar",
		"..",
		"./hidden",
		"foo\\bar",
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := r.Fetch(context.Background(), name, t.TempDir())
			if err == nil {
				t.Errorf("Fetch(%q) should have failed", name)
			}
		})
	}
}

func TestLocalResolver_MissingPluginReturnsError(t *testing.T) {
	r := NewLocalResolver(t.TempDir())
	_, err := r.Fetch(context.Background(), "not-here", t.TempDir())
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention 'not found': %v", err)
	}
}

func TestLocalResolver_RejectsNonDirectoryTarget(t *testing.T) {
	base := t.TempDir()
	// "demo" exists but is a file, not a dir.
	if err := os.WriteFile(filepath.Join(base, "demo"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := NewLocalResolver(base)
	_, err := r.Fetch(context.Background(), "demo", t.TempDir())
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("error should mention 'not a directory': %v", err)
	}
}

func TestLocalResolver_HonoursContextCancellation(t *testing.T) {
	// Make a plugin with enough files to take a moment.
	base := t.TempDir()
	files := map[string]string{}
	for i := 0; i < 20; i++ {
		files[filepath.Join("data", "f"+string(rune('a'+i))+".txt")] = strings.Repeat("x", 1024)
	}
	writePlugin(t, base, "big", files)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before starting — walk should abort fast

	r := NewLocalResolver(base)
	_, err := r.Fetch(ctx, "big", t.TempDir())
	if err == nil {
		t.Error("expected cancellation error")
	}
}

func TestLocalResolver_BubblesUpCopyFailure(t *testing.T) {
	// Source file the copyTree walk would read; make dst unwritable so
	// the copyFile step fails. Skip when running as root — Linux
	// filesystem permissions are advisory-only for uid 0, so chmod 0o555
	// does not prevent writes and the test passes vacuously instead of
	// exercising the error path (issue #87).
	if os.Getuid() == 0 {
		t.Skip("skipping: chmod 0o555 is ineffective when running as root")
	}
	base := t.TempDir()
	writePlugin(t, base, "demo", map[string]string{
		"plugin.yaml": "name: demo\n",
	})
	dst := t.TempDir()
	// Make dst read-only so creating files inside it fails.
	if err := os.Chmod(dst, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dst, 0o755) })

	r := NewLocalResolver(base)
	_, err := r.Fetch(context.Background(), "demo", dst)
	if err == nil {
		t.Error("expected copy failure when dst is read-only")
	}
}

func TestLocalResolver_CopyFileSourceUnreadable(t *testing.T) {
	base := t.TempDir()
	pluginDir := writePlugin(t, base, "demo", map[string]string{
		"plugin.yaml": "name: demo\n",
	})
	// Make the source file unreadable — copyFile should error.
	if err := os.Chmod(filepath.Join(pluginDir, "plugin.yaml"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(pluginDir, "plugin.yaml"), 0o644) })

	r := NewLocalResolver(base)
	_, err := r.Fetch(context.Background(), "demo", t.TempDir())
	// Root can read any file, so this test only asserts on non-root hosts.
	if os.Getuid() == 0 {
		t.Skip("running as root — cannot exercise unreadable-file branch")
	}
	if err == nil {
		t.Error("expected error when source file is unreadable")
	}
}

func TestLocalResolver_WalkErrorPropagates(t *testing.T) {
	// Put a plugin dir in place, then replace its subdirectory with an
	// unreadable one so filepath.Walk surfaces walkErr to our callback.
	if os.Getuid() == 0 {
		t.Skip("running as root — cannot exercise Walk error branch")
	}
	base := t.TempDir()
	pluginDir := writePlugin(t, base, "demo", map[string]string{
		"sub/file.txt": "x",
	})
	// Make the subdir unreadable so Walk's readdir fails.
	if err := os.Chmod(filepath.Join(pluginDir, "sub"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(pluginDir, "sub"), 0o755) })

	r := NewLocalResolver(base)
	_, err := r.Fetch(context.Background(), "demo", t.TempDir())
	if err == nil {
		t.Error("expected Walk error to propagate")
	}
}

func TestLocalResolver_MissingReturnsErrPluginNotFound(t *testing.T) {
	r := NewLocalResolver(t.TempDir())
	_, err := r.Fetch(context.Background(), "absent", t.TempDir())
	if !errors.Is(err, ErrPluginNotFound) {
		t.Errorf("expected ErrPluginNotFound, got %v", err)
	}
}

func TestLocalResolver_NonNotExistStatErrorIsNotErrPluginNotFound(t *testing.T) {
	// stat failing for reasons other than "doesn't exist" should NOT
	// surface as ErrPluginNotFound. Hard to reliably trigger cross-
	// platform, but we can at least prove the wrapping on a read-only
	// permission-denied BaseDir.
	if os.Getuid() == 0 {
		t.Skip("running as root — cannot exercise permission branch")
	}
	base := t.TempDir()
	locked := base + "/locked"
	if err := os.Mkdir(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	r := NewLocalResolver(locked)
	_, err := r.Fetch(context.Background(), "anything", t.TempDir())
	if err == nil {
		t.Fatal("expected error")
	}
	if errors.Is(err, ErrPluginNotFound) {
		t.Errorf("permission-denied stat must not be surfaced as ErrPluginNotFound: %v", err)
	}
}

func TestLocalResolver_RejectsOverlongName(t *testing.T) {
	// 129-char name (1 + 128 tail); max is 1 + 127 = 128.
	long := "a" + strings.Repeat("b", 128)
	r := NewLocalResolver(t.TempDir())
	_, err := r.Fetch(context.Background(), long, t.TempDir())
	if err == nil {
		t.Error("overlong name should be rejected")
	}
	// Exactly at limit should still work (just the regex check; will
	// still fail at the stat step since the dir doesn't exist, but with
	// ErrPluginNotFound, not a format error).
	atLimit := "a" + strings.Repeat("b", 127)
	_, err = r.Fetch(context.Background(), atLimit, t.TempDir())
	if err == nil {
		t.Error("at-limit name with missing dir should still err")
	} else if !errors.Is(err, ErrPluginNotFound) {
		t.Errorf("at-limit name should err with ErrPluginNotFound (format is fine), got %v", err)
	}
}
