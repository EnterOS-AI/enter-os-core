package handlers

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/plugins"
	"github.com/gin-gonic/gin"
)

// ==================== validatePluginName ====================

func TestValidatePluginName_Valid(t *testing.T) {
	valid := []string{
		"my-plugin",
		"plugin_name",
		"MyPlugin",
		"123abc",
		"a",
		"superpowers",
		"molecule-dev",
	}
	for _, name := range valid {
		t.Run(name, func(t *testing.T) {
			if err := validatePluginName(name); err != nil {
				t.Errorf("validatePluginName(%q) returned unexpected error: %v", name, err)
			}
		})
	}
}

func TestValidatePluginName_Empty(t *testing.T) {
	if err := validatePluginName(""); err == nil {
		t.Error("expected error for empty plugin name")
	}
}

func TestValidatePluginName_ForwardSlash(t *testing.T) {
	if err := validatePluginName("foo/bar"); err == nil {
		t.Error("expected error for plugin name containing '/'")
	}
}

func TestValidatePluginName_Backslash(t *testing.T) {
	if err := validatePluginName(`foo\bar`); err == nil {
		t.Error("expected error for plugin name containing '\\'")
	}
}

func TestValidatePluginName_DotDot(t *testing.T) {
	if err := validatePluginName(".."); err == nil {
		t.Error("expected error for '..'")
	}
}

func TestValidatePluginName_DotDotEmbedded(t *testing.T) {
	if err := validatePluginName("foo..bar"); err == nil {
		t.Error("expected error for name containing '..'")
	}
}

func TestValidatePluginName_PathTraversalCases(t *testing.T) {
	cases := []string{
		"../etc",
		"foo/../bar",
		"../../secrets",
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			if err := validatePluginName(name); err == nil {
				t.Errorf("validatePluginName(%q): expected error for path traversal", name)
			}
		})
	}
}

// ==================== dirSize ====================

func TestDirSize_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	size, err := dirSize(dir, 1000)
	if err != nil {
		t.Fatalf("unexpected error on empty dir: %v", err)
	}
	if size != 0 {
		t.Errorf("expected size 0 for empty dir, got %d", size)
	}
}

func TestDirSize_SingleFile(t *testing.T) {
	dir := t.TempDir()
	content := []byte("hello world") // 11 bytes
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), content, 0600); err != nil {
		t.Fatal(err)
	}
	size, err := dirSize(dir, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if size != int64(len(content)) {
		t.Errorf("expected size %d, got %d", len(content), size)
	}
}

func TestDirSize_MultipleFiles(t *testing.T) {
	dir := t.TempDir()
	files := map[string][]byte{
		"a.txt": []byte("hello"),  // 5
		"b.txt": []byte("world!"), // 6
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	size, err := dirSize(dir, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if size != 11 {
		t.Errorf("expected size 11, got %d", size)
	}
}

func TestDirSize_Subdirectories(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "subdir")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "nested.txt"), []byte("nested"), 0600); err != nil {
		t.Fatal(err)
	}
	size, err := dirSize(dir, 1000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if size != 6 {
		t.Errorf("expected size 6, got %d", size)
	}
}

func TestDirSize_ExceedsLimit(t *testing.T) {
	dir := t.TempDir()
	// Write a 100-byte file, set limit to 50.
	if err := os.WriteFile(filepath.Join(dir, "big.bin"), make([]byte, 100), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := dirSize(dir, 50)
	if err == nil {
		t.Error("expected error when dir size exceeds limit")
	}
	if !strings.Contains(err.Error(), "cap") {
		t.Errorf("expected error to mention cap, got: %v", err)
	}
}

func TestDirSize_ExactlyAtLimit(t *testing.T) {
	dir := t.TempDir()
	// A 10-byte file with limit=10 should succeed (not exceed).
	if err := os.WriteFile(filepath.Join(dir, "exact.bin"), make([]byte, 10), 0600); err != nil {
		t.Fatal(err)
	}
	size, err := dirSize(dir, 10)
	if err != nil {
		t.Errorf("exactly at limit should not error, got: %v", err)
	}
	if size != 10 {
		t.Errorf("expected size 10, got %d", size)
	}
}

// ==================== httpErr / newHTTPErr ====================

func TestHTTPErr_Error_ContainsStatus(t *testing.T) {
	e := newHTTPErr(http.StatusBadRequest, gin.H{"error": "bad input"})
	msg := e.Error()
	if !strings.Contains(msg, "400") {
		t.Errorf("Error() should contain status code 400, got: %q", msg)
	}
}

func TestHTTPErr_StatusPreserved(t *testing.T) {
	cases := []int{
		http.StatusBadRequest,
		http.StatusNotFound,
		http.StatusBadGateway,
		http.StatusGatewayTimeout,
		http.StatusRequestEntityTooLarge,
		http.StatusInternalServerError,
	}
	for _, code := range cases {
		e := newHTTPErr(code, gin.H{"error": "test"})
		if e.Status != code {
			t.Errorf("newHTTPErr(%d): Status = %d, want %d", code, e.Status, code)
		}
	}
}

func TestHTTPErr_ErrorsAs_Unwraps(t *testing.T) {
	original := newHTTPErr(http.StatusBadGateway, gin.H{"error": "upstream"})
	wrapped := fmt.Errorf("outer: %w", original)
	var he *httpErr
	if !errors.As(wrapped, &he) {
		t.Fatal("errors.As should unwrap *httpErr through fmt.Errorf %w")
	}
	if he.Status != http.StatusBadGateway {
		t.Errorf("expected 502, got %d", he.Status)
	}
}

// ==================== regexpEscapeForAwk ====================

func TestRegexpEscapeForAwk_PlainName(t *testing.T) {
	// Alphanumeric + hyphen + underscore should be returned unchanged.
	input := "my-plugin_123"
	got := regexpEscapeForAwk(input)
	if got != input {
		t.Errorf("regexpEscapeForAwk(%q) = %q, want unchanged", input, got)
	}
}

func TestRegexpEscapeForAwk_Slash(t *testing.T) {
	// Slash is the awk regex delimiter and MUST be escaped.
	got := regexpEscapeForAwk("a/b")
	if got != `a\/b` {
		t.Errorf("regexpEscapeForAwk(%q) = %q, want %q", "a/b", got, `a\/b`)
	}
}

func TestRegexpEscapeForAwk_Dot(t *testing.T) {
	got := regexpEscapeForAwk("a.b")
	if got != `a\.b` {
		t.Errorf("regexpEscapeForAwk(%q) = %q, want %q", "a.b", got, `a\.b`)
	}
}

func TestRegexpEscapeForAwk_Plus(t *testing.T) {
	got := regexpEscapeForAwk("a+b")
	if got != `a\+b` {
		t.Errorf("regexpEscapeForAwk(%q) = %q, want %q", "a+b", got, `a\+b`)
	}
}

func TestRegexpEscapeForAwk_FullMarkerString(t *testing.T) {
	// The actual marker used in stripPluginMarkersFromMemory.
	// "# Plugin: my-plugin /" must have "/" escaped but " " unescaped.
	marker := "# Plugin: my-plugin /"
	got := regexpEscapeForAwk(marker)
	if !strings.Contains(got, `\/`) {
		t.Errorf("expected escaped slash in output for %q, got %q", marker, got)
	}
	if strings.Contains(got, `\ `) {
		t.Errorf("space should NOT be escaped in %q", marker)
	}
}

func TestRegexpEscapeForAwk_NoDoubleEscape(t *testing.T) {
	// A backslash in the input should itself be escaped.
	got := regexpEscapeForAwk(`a\b`)
	if !strings.HasPrefix(got, `a\\`) {
		t.Errorf("backslash should be escaped, got %q", got)
	}
}

// ==================== streamDirAsTar ====================

func TestStreamDirAsTar_EmptyDir(t *testing.T) {
	root := t.TempDir()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := streamDirAsTar(root, tw); err != nil {
		t.Fatalf("unexpected error on empty dir: %v", err)
	}
	tw.Close()

	tr := tar.NewReader(&buf)
	count := 0
	for {
		if _, err := tr.Next(); err != nil {
			break
		}
		count++
	}
	if count != 0 {
		t.Errorf("expected 0 tar entries for empty dir, got %d", count)
	}
}

func TestStreamDirAsTar_SingleFile(t *testing.T) {
	root := t.TempDir()
	content := []byte("plugin manifest content")
	if err := os.WriteFile(filepath.Join(root, "plugin.yaml"), content, 0600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := streamDirAsTar(root, tw); err != nil {
		t.Fatalf("streamDirAsTar failed: %v", err)
	}
	tw.Close()

	entries := tarEntries(t, &buf)
	if _, ok := entries["plugin.yaml"]; !ok {
		t.Error("tar should contain plugin.yaml")
	}
	if string(entries["plugin.yaml"]) != string(content) {
		t.Errorf("plugin.yaml content mismatch: got %q", entries["plugin.yaml"])
	}
}

func TestStreamDirAsTar_NestedDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "rules"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "rules", "main.md"), []byte("# Rule"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "plugin.yaml"), []byte("name: test\n"), 0600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := streamDirAsTar(root, tw); err != nil {
		t.Fatalf("streamDirAsTar failed: %v", err)
	}
	tw.Close()

	entries := tarEntries(t, &buf)
	if _, ok := entries["plugin.yaml"]; !ok {
		t.Error("tar should contain plugin.yaml")
	}
	// Nested paths must use forward slashes regardless of OS.
	if _, ok := entries["rules/main.md"]; !ok {
		t.Errorf("tar should contain rules/main.md; got entries: %v", entryKeys(entries))
	}
}

func TestStreamDirAsTar_PathsAreRelative(t *testing.T) {
	// Entries must be relative paths — no leading slash, no tempdir prefix.
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := streamDirAsTar(root, tw); err != nil {
		t.Fatalf("streamDirAsTar failed: %v", err)
	}
	tw.Close()

	tr := tar.NewReader(&buf)
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		if strings.HasPrefix(hdr.Name, "/") {
			t.Errorf("tar entry %q has absolute path", hdr.Name)
		}
		if strings.Contains(hdr.Name, "tmp") || strings.Contains(hdr.Name, "var") {
			t.Errorf("tar entry %q leaks tempdir path", hdr.Name)
		}
	}
}

// ==================== resolveAndStage (with stub resolver) ====================

// stubResolver is a minimal SourceResolver for testing resolveAndStage
// without requiring a real Docker client or live plugin registry.
type stubResolver struct {
	scheme   string
	name     string // plugin name returned from Fetch
	content  string // file content written into dst
	fetchErr error  // non-nil causes Fetch to return this error
}

func (s *stubResolver) Scheme() string { return s.scheme }
func (s *stubResolver) Fetch(_ context.Context, _ string, dst string) (string, error) {
	if s.fetchErr != nil {
		return "", s.fetchErr
	}
	// Write a minimal file so dirSize has something to measure.
	if err := os.WriteFile(filepath.Join(dst, "plugin.yaml"), []byte(s.content), 0600); err != nil {
		return "", err
	}
	return s.name, nil
}

func TestResolveAndStage_EmptySource(t *testing.T) {
	h := NewPluginsHandler(t.TempDir(), nil, nil)
	_, err := h.resolveAndStage(context.Background(), installRequest{Source: ""})
	assertHTTPErrStatus(t, err, http.StatusBadRequest, "empty source")
}

func TestResolveAndStage_UnknownScheme(t *testing.T) {
	h := NewPluginsHandler(t.TempDir(), nil, nil)
	_, err := h.resolveAndStage(context.Background(), installRequest{Source: "nosuchthing://plugin"})
	assertHTTPErrStatus(t, err, http.StatusBadRequest, "unknown scheme")
}

func TestResolveAndStage_HappyPath(t *testing.T) {
	h := NewPluginsHandler(t.TempDir(), nil, nil).WithSourceResolver(&stubResolver{
		scheme:  "stub",
		name:    "my-plugin",
		content: "name: my-plugin\nversion: 1.0.0\n",
	})

	result, err := h.resolveAndStage(context.Background(), installRequest{Source: "stub://my-plugin"})
	if err != nil {
		t.Fatalf("unexpected error on happy path: %v", err)
	}
	defer os.RemoveAll(result.StagedDir)

	if result.PluginName != "my-plugin" {
		t.Errorf("expected PluginName 'my-plugin', got %q", result.PluginName)
	}
	if result.Source.Scheme != "stub" {
		t.Errorf("expected Source.Scheme 'stub', got %q", result.Source.Scheme)
	}
	// The staged directory must exist and contain the file.
	if _, err := os.Stat(filepath.Join(result.StagedDir, "plugin.yaml")); os.IsNotExist(err) {
		t.Error("staged dir should contain plugin.yaml after successful fetch")
	}
}

func TestResolveAndStage_StagedDirCleanedOnFetchError(t *testing.T) {
	// resolveAndStage must remove the staging tempdir if Fetch fails.
	// We verify this by capturing the stagedDir path from the error path;
	// since we can't inspect it directly, we verify that no extra tempdirs
	// are left behind after the function returns.
	beforeCount := tempDirCount(t)

	h := NewPluginsHandler(t.TempDir(), nil, nil).WithSourceResolver(&stubResolver{
		scheme:   "stub",
		fetchErr: errors.New("simulated fetch failure"),
	})
	_, err := h.resolveAndStage(context.Background(), installRequest{Source: "stub://plugin"})
	if err == nil {
		t.Fatal("expected error from fetch failure")
	}

	afterCount := tempDirCount(t)
	if afterCount > beforeCount {
		t.Errorf("resolveAndStage left %d orphaned tempdir(s) after fetch error", afterCount-beforeCount)
	}
}

func TestResolveAndStage_FetchReturnsErrPluginNotFound(t *testing.T) {
	h := NewPluginsHandler(t.TempDir(), nil, nil).WithSourceResolver(&stubResolver{
		scheme:   "stub",
		fetchErr: plugins.ErrPluginNotFound,
	})
	_, err := h.resolveAndStage(context.Background(), installRequest{Source: "stub://missing"})
	assertHTTPErrStatus(t, err, http.StatusNotFound, "ErrPluginNotFound")
}

func TestResolveAndStage_FetchReturnsDeadlineExceeded(t *testing.T) {
	h := NewPluginsHandler(t.TempDir(), nil, nil).WithSourceResolver(&stubResolver{
		scheme:   "stub",
		fetchErr: context.DeadlineExceeded,
	})
	_, err := h.resolveAndStage(context.Background(), installRequest{Source: "stub://slow"})
	assertHTTPErrStatus(t, err, http.StatusGatewayTimeout, "DeadlineExceeded")
}

func TestResolveAndStage_FetchReturnsGenericError(t *testing.T) {
	h := NewPluginsHandler(t.TempDir(), nil, nil).WithSourceResolver(&stubResolver{
		scheme:   "stub",
		fetchErr: errors.New("connection refused"),
	})
	_, err := h.resolveAndStage(context.Background(), installRequest{Source: "stub://anything"})
	assertHTTPErrStatus(t, err, http.StatusBadGateway, "generic fetch error")
}

func TestResolveAndStage_ResolverReturnsInvalidName(t *testing.T) {
	// A resolver returning a name with path traversal must yield 400.
	h := NewPluginsHandler(t.TempDir(), nil, nil).WithSourceResolver(&stubResolver{
		scheme:  "stub",
		name:    "foo/bar", // invalid: contains slash
		content: "name: test\n",
	})
	_, err := h.resolveAndStage(context.Background(), installRequest{Source: "stub://anything"})
	assertHTTPErrStatus(t, err, http.StatusBadRequest, "invalid name from resolver")
}

func TestResolveAndStage_LocalSchemePathTraversal(t *testing.T) {
	// "local://../../etc/passwd" must be rejected before Fetch is called,
	// preventing path-traversal on the platform's plugin registry directory.
	h := NewPluginsHandler(t.TempDir(), nil, nil)
	_, err := h.resolveAndStage(context.Background(), installRequest{Source: "local://../../etc/passwd"})
	assertHTTPErrStatus(t, err, http.StatusBadRequest, "local path traversal")
}

// ==================== supply-chain hardening (SAFE-T1102) ====================

// TestPluginInstall_SHA256Mismatch_AbortsInstall verifies that when the caller
// provides a sha256 field that does not match the fetched plugin.yaml, the
// install is aborted with 422 Unprocessable Entity and the staging dir is cleaned up.
func TestPluginInstall_SHA256Mismatch_AbortsInstall(t *testing.T) {
	beforeCount := tempDirCount(t)

	h := NewPluginsHandler(t.TempDir(), nil, nil).WithSourceResolver(&stubResolver{
		scheme:  "stub",
		name:    "my-plugin",
		content: "name: my-plugin\nversion: 1.0.0\n",
	})
	_, err := h.resolveAndStage(context.Background(), installRequest{
		Source: "stub://my-plugin",
		SHA256: "0000000000000000000000000000000000000000000000000000000000000000", // wrong
	})
	assertHTTPErrStatus(t, err, http.StatusUnprocessableEntity, "sha256 mismatch")

	afterCount := tempDirCount(t)
	if afterCount > beforeCount {
		t.Errorf("SHA256 mismatch left %d orphaned staging dir(s)", afterCount-beforeCount)
	}
}

// TestPluginInstall_SHA256Match_Succeeds verifies that resolveAndStage succeeds
// when the caller supplies the correct SHA-256 of the fetched plugin.yaml.
func TestPluginInstall_SHA256Match_Succeeds(t *testing.T) {
	content := "name: my-plugin\nversion: 1.0.0\n"
	sum := sha256.Sum256([]byte(content))
	correctHash := hex.EncodeToString(sum[:])

	h := NewPluginsHandler(t.TempDir(), nil, nil).WithSourceResolver(&stubResolver{
		scheme:  "stub",
		name:    "my-plugin",
		content: content,
	})
	result, err := h.resolveAndStage(context.Background(), installRequest{
		Source: "stub://my-plugin",
		SHA256: correctHash,
	})
	if err != nil {
		t.Fatalf("expected success when sha256 matches, got: %v", err)
	}
	defer os.RemoveAll(result.StagedDir)
	if result.PluginName != "my-plugin" {
		t.Errorf("expected PluginName 'my-plugin', got %q", result.PluginName)
	}
}

// TestPluginInstall_UnpinnedRef_Rejected verifies that a github:// spec without
// a #<ref> suffix is rejected with 422 unless PLUGIN_ALLOW_UNPINNED=true.
func TestPluginInstall_UnpinnedRef_Rejected(t *testing.T) {
	t.Setenv("PLUGIN_ALLOW_UNPINNED", "") // ensure the guard is active

	h := NewPluginsHandler(t.TempDir(), nil, nil).WithSourceResolver(&stubResolver{
		scheme:  "github",
		name:    "my-plugin",
		content: "name: my-plugin\n",
	})
	_, err := h.resolveAndStage(context.Background(), installRequest{
		Source: "github://owner/repo", // no #ref — must be rejected
	})
	assertHTTPErrStatus(t, err, http.StatusUnprocessableEntity, "unpinned ref rejected")
}

// TestPluginInstall_PinnedRef_Accepted verifies that a github:// spec that
// includes a #<ref> suffix passes the pinned-ref guard and completes normally.
func TestPluginInstall_PinnedRef_Accepted(t *testing.T) {
	h := NewPluginsHandler(t.TempDir(), nil, nil).WithSourceResolver(&stubResolver{
		scheme:  "github",
		name:    "my-plugin",
		content: "name: my-plugin\n",
	})
	result, err := h.resolveAndStage(context.Background(), installRequest{
		Source: "github://owner/repo#v1.0.0", // pinned — must be accepted
	})
	if err != nil {
		t.Fatalf("expected success for pinned ref, got: %v", err)
	}
	defer os.RemoveAll(result.StagedDir)
	if result.PluginName != "my-plugin" {
		t.Errorf("expected PluginName 'my-plugin', got %q", result.PluginName)
	}
}

// ==================== helpers ====================

// assertHTTPErrStatus is a test helper that checks err is a *httpErr with
// the expected status code. Fails with a clear message if either condition
// is not met.
func assertHTTPErrStatus(t *testing.T, err error, want int, label string) {
	t.Helper()
	if err == nil {
		t.Fatalf("[%s] expected *httpErr with status %d, got nil error", label, want)
	}
	var he *httpErr
	if !errors.As(err, &he) {
		t.Fatalf("[%s] expected *httpErr, got %T: %v", label, err, err)
	}
	if he.Status != want {
		t.Errorf("[%s] expected status %d, got %d", label, want, he.Status)
	}
}

// tarEntries reads all entries from a tar.Reader backed by buf and returns
// a map of entry name → string content. The caller must have already
// written and closed the tar.Writer before calling this.
func tarEntries(t *testing.T, buf *bytes.Buffer) map[string]string {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(buf.Bytes()))
	entries := make(map[string]string)
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		var b bytes.Buffer
		if _, err := io.Copy(&b, tr); err != nil {
			t.Fatalf("failed to read tar entry %s: %v", hdr.Name, err)
		}
		// Normalize OS path separators so tests pass on Windows too.
		entries[filepath.ToSlash(hdr.Name)] = b.String()
	}
	return entries
}

// entryKeys returns the sorted list of keys from a map[string]string for
// inclusion in error messages.
func entryKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// tempDirCount counts the number of entries in os.TempDir() that look like
// molecule-plugin-fetch-* staging dirs. Used to verify cleanup on error.
func tempDirCount(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		t.Fatalf("failed to read tempdir: %v", err)
	}
	count := 0
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "molecule-plugin-fetch-") {
			count++
		}
	}
	return count
}
