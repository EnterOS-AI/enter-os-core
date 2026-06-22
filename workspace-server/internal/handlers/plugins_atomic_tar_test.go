package handlers

// plugins_atomic_tar_test.go — unit tests for tarWalk (the only non-trivial
// function in plugins_atomic_tar.go). The file contains only pure tar-walk
// logic with no DB or HTTP dependencies, so tests use real temp directories
// with no mocking.

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ─── newTarWriter ─────────────────────────────────────────────────────────────

func TestNewTarWriter_Basic(t *testing.T) {
	var buf bytes.Buffer
	tw := newTarWriter(&buf)
	if tw == nil {
		t.Fatal("newTarWriter returned nil")
	}
	// Write a header to prove the writer is functional.
	hdr := &tar.Header{
		Name: "test.txt",
		Mode: 0644,
		Size: 5,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("WriteHeader failed: %v", err)
	}
	if _, err := tw.Write([]byte("hello")); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
}

// ─── tarWalk: empty directory ─────────────────────────────────────────────────

func TestTarWalk_EmptyDir(t *testing.T) {
	tmp := t.TempDir()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	if err := tarWalk(tmp, "prefix", tw); err != nil {
		t.Fatalf("tarWalk error: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tw.Close error: %v", err)
	}

	// An empty directory should still emit one header (the dir itself).
	rdr := tar.NewReader(&buf)
	hdr, err := rdr.Next()
	if err != nil {
		t.Fatalf("expected at least the dir header, got error: %v", err)
	}
	if !strings.HasSuffix(hdr.Name, "/") {
		t.Errorf("expected directory name ending in '/', got %q", hdr.Name)
	}

	// No more entries.
	if _, err := rdr.Next(); err != io.EOF {
		t.Errorf("expected only one header, got more: %v", err)
	}
}

// ─── tarWalk: single file ─────────────────────────────────────────────────────

func TestTarWalk_SingleFile(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "hello.txt"), []byte("world"), 0644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tarWalk(tmp, "mydir", tw); err != nil {
		t.Fatalf("tarWalk error: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	// Should have 2 entries: the dir prefix, then hello.txt.
	entries := 0
	names := []string{}
	rdr := tar.NewReader(&buf)
	for {
		hdr, err := rdr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("unexpected error reading tar: %v", err)
		}
		entries++
		names = append(names, hdr.Name)

		if hdr.Name == "mydir/hello.txt" {
			if hdr.Size != 5 {
				t.Errorf("expected size 5, got %d", hdr.Size)
			}
			content := make([]byte, 5)
			if _, err := rdr.Read(content); err != nil && err != io.EOF {
				t.Fatalf("read error: %v", err)
			}
			if string(content) != "world" {
				t.Errorf("expected 'world', got %q", string(content))
			}
		}
	}
	if entries != 2 {
		t.Errorf("expected 2 entries, got %d: %v", entries, names)
	}
}

// ─── tarWalk: nested directories ───────────────────────────────────────────────

func TestTarWalk_NestedDirs(t *testing.T) {
	tmp := t.TempDir()
	subdir := filepath.Join(tmp, "a", "b", "c")
	if err := os.MkdirAll(subdir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subdir, "deep.txt"), []byte("nested"), 0644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tarWalk(tmp, "root", tw); err != nil {
		t.Fatalf("tarWalk error: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	// Collect all file paths (not dirs) with content.
	files := map[string]string{}
	rdr := tar.NewReader(&buf)
	for {
		hdr, err := rdr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(hdr.Name, "/") && hdr.Size > 0 {
			content := make([]byte, hdr.Size)
			rdr.Read(content)
			files[hdr.Name] = string(content)
		}
	}

	expected := "root/a/b/c/deep.txt"
	if _, ok := files[expected]; !ok {
		t.Errorf("expected file %q in tar; got: %v", expected, files)
	} else if files[expected] != "nested" {
		t.Errorf("expected content 'nested', got %q", files[expected])
	}
}

// ─── tarWalk: symlinks are skipped ────────────────────────────────────────────

func TestTarWalk_SymlinksSkipped(t *testing.T) {
	tmp := t.TempDir()

	// Create a real file.
	realPath := filepath.Join(tmp, "real.txt")
	if err := os.WriteFile(realPath, []byte("real content"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create a symlink to it.
	linkPath := filepath.Join(tmp, "link.txt")
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tarWalk(tmp, "prefix", tw); err != nil {
		t.Fatalf("tarWalk error: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	// Only real.txt should appear; link.txt should be absent.
	names := []string{}
	rdr := tar.NewReader(&buf)
	for {
		hdr, err := rdr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, hdr.Name)
	}

	foundLink := false
	for _, n := range names {
		if strings.Contains(n, "link") {
			foundLink = true
		}
	}
	if foundLink {
		t.Errorf("symlink should be skipped; got names: %v", names)
	}
}

// ─── tarWalk: prefix trailing slash is normalized ─────────────────────────────

func TestTarWalk_PrefixTrailingSlashNormalized(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "f.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	// Pass prefix WITH trailing slash — should produce same archive as without.
	if err := tarWalk(tmp, "foo/", tw); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	// The file should be under "foo/", not "foo//".
	rdr := tar.NewReader(&buf)
	for {
		hdr, err := rdr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(hdr.Name, "/") && strings.Contains(hdr.Name, "f.txt") {
			if strings.Contains(hdr.Name, "//") {
				t.Errorf("double slash found in path %q — trailing slash not normalized", hdr.Name)
			}
			if !strings.HasPrefix(hdr.Name, "foo/") {
				t.Errorf("expected path to start with 'foo/', got %q", hdr.Name)
			}
		}
	}
}

// ─── tarWalk: prefix = "." emits flat paths ───────────────────────────────────

func TestTarWalk_PrefixDotEmitsFlatPaths(t *testing.T) {
	tmp := t.TempDir()
	subdir := filepath.Join(tmp, "sub")
	if err := os.MkdirAll(subdir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subdir, "file.txt"), []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tarWalk(tmp, ".", tw); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	// With prefix ".", paths should NOT start with "./" (filepath.Clean normalizes it).
	rdr := tar.NewReader(&buf)
	for {
		hdr, err := rdr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(hdr.Name, "/") && strings.Contains(hdr.Name, "file.txt") {
			if strings.HasPrefix(hdr.Name, "./") {
				t.Errorf("prefix '.' should not emit './' prefix; got %q", hdr.Name)
			}
		}
	}
}

// ─── tarWalk: walk error propagates ───────────────────────────────────────────

func TestTarWalk_NonexistentDir(t *testing.T) {
	nonexistent := filepath.Join(t.TempDir(), "does-not-exist")
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	err := tarWalk(nonexistent, "x", tw)
	if err == nil {
		t.Error("expected error for nonexistent directory, got nil")
	}
}
