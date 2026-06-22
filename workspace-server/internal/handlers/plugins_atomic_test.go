package handlers

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestInstallVersion_Paths: the path helpers must produce a stable shape
// the in-container exec calls depend on. Pinning the layout here
// catches a future refactor that accidentally changes where staging /
// previous / live dirs live, which would break the swap atomicity.
func TestInstallVersion_Paths(t *testing.T) {
	v := installVersion{plugin: "molecule-skill-foo", stamp: "20260508T141530Z"}

	if got, want := v.stagedPath(), "/configs/plugins/.staging/molecule-skill-foo.20260508T141530Z"; got != want {
		t.Errorf("stagedPath = %q; want %q", got, want)
	}
	if got, want := v.previousPath(), "/configs/plugins/.previous/molecule-skill-foo.20260508T141530Z"; got != want {
		t.Errorf("previousPath = %q; want %q", got, want)
	}
	if got, want := v.livePath(), "/configs/plugins/molecule-skill-foo"; got != want {
		t.Errorf("livePath = %q; want %q", got, want)
	}
	if got, want := v.markerPath(), "/configs/plugins/molecule-skill-foo/.complete"; got != want {
		t.Errorf("markerPath = %q; want %q", got, want)
	}
}

// TestInstallVersion_StampUniqueness: two newInstallVersion calls within
// the same second produce the same stamp (we use second precision); the
// caller relies on the mv-rename being atomic, so collision-free
// stamping is NOT a correctness requirement — but a regression that
// changes stamp shape (e.g. RFC3339 with colons) would break the path
// helpers since path.Join treats a colon as a regular char but ssh +
// docker exec generally don't. Pin the no-colon shape.
func TestInstallVersion_StampShape(t *testing.T) {
	v := newInstallVersion("anything")
	if strings.Contains(v.stamp, ":") {
		t.Errorf("stamp must not contain colons (breaks shell-quoting in exec): %q", v.stamp)
	}
	if strings.Contains(v.stamp, " ") {
		t.Errorf("stamp must not contain spaces: %q", v.stamp)
	}
	// Sanity: stamp parses as the documented format.
	if _, err := time.Parse("20060102T150405Z", v.stamp); err != nil {
		t.Errorf("stamp %q does not parse as 20060102T150405Z: %v", v.stamp, err)
	}
}

// TestTarHostDirWithPrefix_HappyPath: walks a host dir, builds a tar with
// the configured prefix, verifies every entry's name is rooted under
// the prefix, and the file contents survive round-trip.
func TestTarHostDirWithPrefix_HappyPath(t *testing.T) {
	hostDir := t.TempDir()

	// Plant: <host>/plugin.yaml + <host>/skills/foo/SKILL.md + <host>/.complete
	files := map[string]string{
		"plugin.yaml":             "name: foo\nversion: 1.0.0\n",
		"skills/foo/SKILL.md":     "# Foo skill\n",
		".complete":                "", // upstream may already have a marker
	}
	for rel, body := range files {
		full := filepath.Join(hostDir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	prefix := "configs/plugins/.staging/foo.20260508T141530Z"
	buf, err := tarHostDirWithPrefix(hostDir, prefix)
	if err != nil {
		t.Fatalf("tar: %v", err)
	}

	// Read back the tar; collect names + body for regular files.
	got := map[string]string{}
	tr := tar.NewReader(&buf)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar reader: %v", err)
		}
		// Every entry must start with the prefix
		if !strings.HasPrefix(hdr.Name, prefix) {
			t.Errorf("entry %q does not start with prefix %q", hdr.Name, prefix)
		}
		if hdr.Typeflag == tar.TypeReg {
			body, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			rel := strings.TrimPrefix(hdr.Name, prefix+"/")
			got[rel] = string(body)
		}
	}

	for rel, want := range files {
		if got[rel] != want {
			t.Errorf("body[%q] = %q; want %q", rel, got[rel], want)
		}
	}
}

// TestTarHostDirWithPrefix_SkipsSymlinks: a hostile plugin shouldn't be
// able to ship a symlink that, post-extract, points outside its own
// dir. The walker silently skips symlinks (same posture as
// streamDirAsTar). Verify a planted symlink doesn't appear in the tar.
func TestTarHostDirWithPrefix_SkipsSymlinks(t *testing.T) {
	hostDir := t.TempDir()
	// Plant a real file + a symlink pointing outside hostDir.
	if err := os.WriteFile(filepath.Join(hostDir, "real.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(target, []byte("SHOULD NOT APPEAR"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(hostDir, "evil")); err != nil {
		t.Fatal(err)
	}

	buf, err := tarHostDirWithPrefix(hostDir, "p")
	if err != nil {
		t.Fatal(err)
	}

	names := []string{}
	tr := tar.NewReader(&buf)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, hdr.Name)
	}
	sort.Strings(names)

	for _, n := range names {
		if strings.Contains(n, "evil") {
			t.Errorf("symlink leaked into tar: %q", n)
		}
	}
	// real.txt should be present
	found := false
	for _, n := range names {
		if strings.HasSuffix(n, "real.txt") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("real.txt missing from tar; got names: %v", names)
	}
}

// TestTarHostDirWithPrefix_PrefixNormalization: trailing slash on prefix
// should not change the archive shape. Pinning this so a future caller
// passing "foo/" instead of "foo" doesn't double-slash entry names.
func TestTarHostDirWithPrefix_PrefixNormalization(t *testing.T) {
	hostDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(hostDir, "x"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}

	a, err := tarHostDirWithPrefix(hostDir, "foo")
	if err != nil {
		t.Fatal(err)
	}
	b, err := tarHostDirWithPrefix(hostDir, "foo/")
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Errorf("trailing-slash on prefix changed archive shape; tarHostDirWithPrefix should be slash-insensitive")
	}
}

// ─── tarWalk (direct) ─────────────────────────────────────────────────────────

// TestTarWalk_EmptyDirectory: an empty dir produces exactly one tar entry
// (the dir itself, with a trailing slash).
func TestTarWalk_EmptyDirectory(t *testing.T) {
	hostDir := t.TempDir()
	var buf bytes.Buffer
	tw := newTarWriter(&buf)
	if err := tarWalk(hostDir, "prefix", tw); err != nil {
		t.Fatalf("tarWalk: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	entries := readTarNames(&buf)
	if len(entries) != 1 {
		t.Errorf("empty dir: got %d entries; want 1", len(entries))
	}
	if entries[0] != "prefix/" {
		t.Errorf("empty dir sole entry: got %q; want prefix/", entries[0])
	}
}

// TestTarWalk_NestedDirs is defined in plugins_atomic_tar_test.go to avoid
// redeclaration. Deeply nested directory walk is tested there.

// TestTarWalk_DirEntryHasTrailingSlash: directory entries must end with '/'
// per tar format; tar.Header.Typeflag '5' (dir) must produce "name/" not "name".
func TestTarWalk_DirEntryHasTrailingSlash(t *testing.T) {
	hostDir := t.TempDir()
	sub := filepath.Join(hostDir, "subdir")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	tw := newTarWriter(&buf)
	if err := tarWalk(hostDir, "p", tw); err != nil {
		t.Fatalf("tarWalk: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	entries := readTarNames(&buf)
	for _, e := range entries {
		// Only "p/" (the root) and "p/subdir/" are dirs; files have no trailing slash.
		if !strings.HasSuffix(e, ".txt") && !strings.HasSuffix(e, "/") {
			t.Errorf("non-file entry %q missing trailing slash: should be a dir", e)
		}
	}
}

// TestTarWalk_FileContentsPreserved: regular file bytes survive tar round-trip
// through tarWalk + tar.Reader.
func TestTarWalk_FileContentsPreserved(t *testing.T) {
	hostDir := t.TempDir()
	contents := map[string]string{
		"plugin.yaml":           "name: test\nversion: 1.0.0\n",
		"skills/foo/SKILL.md": "# Foo\n",
	}
	for rel, body := range contents {
		full := filepath.Join(hostDir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	tw := newTarWriter(&buf)
	if err := tarWalk(hostDir, "prefix", tw); err != nil {
		t.Fatalf("tarWalk: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Read back and verify contents.
	extracted := map[string]string{}
	tr := tar.NewReader(&buf)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reader: %v", err)
		}
		if hdr.Typeflag == tar.TypeReg {
			data, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			rel := strings.TrimPrefix(hdr.Name, "prefix/")
			extracted[rel] = string(data)
		}
	}
	for rel, want := range contents {
		if got := extracted[rel]; got != want {
			t.Errorf("content[%s] = %q; want %q", rel, got, want)
		}
	}
}

// readTarNames extracts just the Name field from every entry in a tar buffer.
func readTarNames(buf *bytes.Buffer) []string {
	var names []string
	tr := tar.NewReader(buf)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}
		names = append(names, hdr.Name)
		// Advance past non-header bytes.
		if hdr.Size > 0 {
			io.Copy(io.Discard, tr)
		}
	}
	sort.Strings(names)
	return names
}
