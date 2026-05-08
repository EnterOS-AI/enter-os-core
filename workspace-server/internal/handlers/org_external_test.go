package handlers

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// fakeFetcher pre-stages a "fetched" repo at a fixed path inside the
// rootDir's .external-cache, bypassing the real git clone. Tests
// inject this via SetExternalFetcherForTest to exercise the resolver
// + path-rewrite logic without network.
type fakeFetcher struct {
	// content maps "<host>/<repo>@<ref>" → a function that materializes
	// repo content under cacheDir. Returns the fake SHA to use.
	content map[string]func(cacheDir string) (sha string, err error)
}

func (f *fakeFetcher) Fetch(ctx context.Context, rootDir, host, repoPath, ref string) (string, string, error) {
	key := host + "/" + repoPath + "@" + ref
	stage, ok := f.content[key]
	if !ok {
		return "", "", &fakeNotFoundError{key: key}
	}
	// Use a stable SHA for the test so cache dir is deterministic.
	cacheDir := filepath.Join(rootDir, ".external-cache", safeRepoCacheDir(host, repoPath), "deadbeef")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", "", err
	}
	sha, err := stage(cacheDir)
	if err != nil {
		return "", "", err
	}
	return cacheDir, sha, nil
}

type fakeNotFoundError struct{ key string }

func (e *fakeNotFoundError) Error() string {
	return "fake fetcher: no content registered for " + e.key
}

// stageFiles writes a map of relative-path → content into cacheDir,
// returning a fake SHA. Helper for fakeFetcher closures.
func stageFiles(cacheDir string, files map[string]string) error {
	if err := os.MkdirAll(filepath.Join(cacheDir, ".git"), 0o755); err != nil {
		return err
	}
	for path, content := range files {
		full := filepath.Join(cacheDir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// TestResolveExternalMapping_HappyPath: a parent template with an
// !external entry resolves cleanly into the fetched workspace + path-
// rewrites files_dir + relative !include refs into the cache prefix.
func TestResolveExternalMapping_HappyPath(t *testing.T) {
	tmp := t.TempDir()

	// Stub fetcher: "fetched" content has a workspace.yaml that uses
	// files_dir + nested !include relative to the fetched repo's root.
	fake := &fakeFetcher{
		content: map[string]func(string) (string, error){
			"git.moleculesai.app/molecule-ai/molecule-dev-department@main": func(cacheDir string) (string, error) {
				return "deadbeef", stageFiles(cacheDir, map[string]string{
					"dev-lead/workspace.yaml": `name: Dev Lead
files_dir: dev-lead
children:
  - !include ./core-lead/workspace.yaml
`,
					"dev-lead/core-lead/workspace.yaml": `name: Core Platform Lead
files_dir: dev-lead/core-lead
`,
				})
			},
		},
	}
	cleanup := SetExternalFetcherForTest(fake)
	defer cleanup()

	src := []byte(`name: Parent
workspaces:
  - !external
    repo: molecule-ai/molecule-dev-department
    ref: main
    path: dev-lead/workspace.yaml
`)

	out, err := resolveYAMLIncludes(src, tmp)
	if err != nil {
		t.Fatalf("resolveYAMLIncludes: %v", err)
	}

	var tmpl OrgTemplate
	if err := yaml.Unmarshal(out, &tmpl); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(tmpl.Workspaces) != 1 {
		t.Fatalf("workspaces: %+v", tmpl.Workspaces)
	}
	dev := tmpl.Workspaces[0]
	if dev.Name != "Dev Lead" {
		t.Errorf("dev.Name = %q; want Dev Lead", dev.Name)
	}
	// files_dir should be cache-prefixed.
	wantPrefix := filepath.Join(".external-cache", "git.moleculesai.app__molecule-ai__molecule-dev-department", "deadbeef")
	if !strings.HasPrefix(dev.FilesDir, wantPrefix) {
		t.Errorf("dev.FilesDir = %q; want prefix %q", dev.FilesDir, wantPrefix)
	}
	if !strings.HasSuffix(dev.FilesDir, "dev-lead") {
		t.Errorf("dev.FilesDir = %q; want suffix dev-lead", dev.FilesDir)
	}
	// Nested child: files_dir cache-prefixed, name Core Platform Lead.
	if len(dev.Children) != 1 {
		t.Fatalf("dev.Children: %+v", dev.Children)
	}
	core := dev.Children[0]
	if core.Name != "Core Platform Lead" {
		t.Errorf("core.Name = %q; want Core Platform Lead", core.Name)
	}
	if !strings.HasPrefix(core.FilesDir, wantPrefix) {
		t.Errorf("core.FilesDir = %q; want prefix %q", core.FilesDir, wantPrefix)
	}
	if !strings.HasSuffix(core.FilesDir, filepath.Join("dev-lead", "core-lead")) {
		t.Errorf("core.FilesDir = %q; want suffix dev-lead/core-lead", core.FilesDir)
	}
}

// TestResolveExternalMapping_AllowlistRejection: hostile yaml pointing
// at a non-allowlisted repo gets rejected.
func TestResolveExternalMapping_AllowlistRejection(t *testing.T) {
	tmp := t.TempDir()
	fake := &fakeFetcher{content: map[string]func(string) (string, error){}}
	cleanup := SetExternalFetcherForTest(fake)
	defer cleanup()

	// Default allowlist is git.moleculesai.app/molecule-ai/*.
	// github.com/foo/bar is NOT in it.
	src := []byte(`workspaces:
  - !external
    repo: foo/bar
    ref: main
    path: x.yaml
    url: github.com
`)
	_, err := resolveYAMLIncludes(src, tmp)
	if err == nil {
		t.Fatalf("expected allowlist rejection, got nil")
	}
	if !strings.Contains(err.Error(), "MOLECULE_EXTERNAL_REPO_ALLOWLIST") {
		t.Errorf("expected allowlist error; got %v", err)
	}
}

// TestResolveExternalMapping_PathTraversalRejection: hostile yaml
// with `path: ../../etc/passwd` gets rejected before fetch.
func TestResolveExternalMapping_PathTraversalRejection(t *testing.T) {
	tmp := t.TempDir()
	fake := &fakeFetcher{content: map[string]func(string) (string, error){}}
	cleanup := SetExternalFetcherForTest(fake)
	defer cleanup()

	src := []byte(`workspaces:
  - !external
    repo: molecule-ai/dev-department
    ref: main
    path: ../../etc/passwd
`)
	_, err := resolveYAMLIncludes(src, tmp)
	if err == nil {
		t.Fatalf("expected path traversal rejection, got nil")
	}
	if !strings.Contains(err.Error(), "relative-and-down-only") {
		t.Errorf("expected path traversal error; got %v", err)
	}
}

// TestResolveExternalMapping_BadRefRejection: non-allowlisted ref chars.
func TestResolveExternalMapping_BadRefRejection(t *testing.T) {
	tmp := t.TempDir()
	fake := &fakeFetcher{content: map[string]func(string) (string, error){}}
	cleanup := SetExternalFetcherForTest(fake)
	defer cleanup()

	src := []byte(`workspaces:
  - !external
    repo: molecule-ai/dev-department
    ref: "main; rm -rf /"
    path: foo.yaml
`)
	_, err := resolveYAMLIncludes(src, tmp)
	if err == nil || !strings.Contains(err.Error(), "disallowed characters") {
		t.Errorf("expected ref-validation error; got %v", err)
	}
}

// TestResolveExternalMapping_MissingRequiredFields: repo / ref / path
// are all required.
func TestResolveExternalMapping_MissingRequiredFields(t *testing.T) {
	tmp := t.TempDir()
	fake := &fakeFetcher{content: map[string]func(string) (string, error){}}
	cleanup := SetExternalFetcherForTest(fake)
	defer cleanup()

	cases := []string{
		// missing repo
		`workspaces:
  - !external
    ref: main
    path: x.yaml
`,
		// missing ref
		`workspaces:
  - !external
    repo: molecule-ai/x
    path: x.yaml
`,
		// missing path
		`workspaces:
  - !external
    repo: molecule-ai/x
    ref: main
`,
	}
	for i, src := range cases {
		_, err := resolveYAMLIncludes([]byte(src), tmp)
		if err == nil {
			t.Errorf("case %d: expected required-field error, got nil", i)
		} else if !strings.Contains(err.Error(), "required") {
			t.Errorf("case %d: want 'required' in error; got %v", i, err)
		}
	}
}

// TestRewriteFilesDir: verify the path-rewrite walker
// prefixes files_dir scalars. !include scalars are NOT rewritten —
// they resolve relative to their containing file's dir, which post-
// fetch is naturally inside the cache.
func TestRewriteFilesDir(t *testing.T) {
	src := `name: Foo
files_dir: dev-lead
children:
  - !include ./bar/workspace.yaml
  - !include other-team.yaml
inner:
  files_dir: dev-lead/sub
`
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(src), &n); err != nil {
		t.Fatal(err)
	}
	rewriteFilesDir(&n, ".external-cache/foo/bar")

	out, err := yaml.Marshal(&n)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	for _, want := range []string{
		"files_dir: .external-cache/foo/bar/dev-lead",
		"files_dir: .external-cache/foo/bar/dev-lead/sub",
		// !include preserved as-is; resolves naturally via subDir.
		"!include ./bar/workspace.yaml",
		"!include other-team.yaml",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// TestRewriteFilesDir_Idempotent: re-running the rewriter
// on already-prefixed files_dir doesn't double-prefix.
func TestRewriteFilesDir_Idempotent(t *testing.T) {
	src := `files_dir: .external-cache/foo/bar/dev-lead
inner:
  files_dir: .external-cache/foo/bar/dev-lead/sub
`
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(src), &n); err != nil {
		t.Fatal(err)
	}
	rewriteFilesDir(&n, ".external-cache/foo/bar")

	out, _ := yaml.Marshal(&n)
	got := string(out)
	if strings.Contains(got, ".external-cache/foo/bar/.external-cache") {
		t.Errorf("double-prefix detected:\n%s", got)
	}
	// Should still be valid (single-prefixed) afterwards.
	for _, want := range []string{
		"files_dir: .external-cache/foo/bar/dev-lead",
		"files_dir: .external-cache/foo/bar/dev-lead/sub",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected unchanged %q in:\n%s", want, got)
		}
	}
}

// TestAllowlistedHostPath: env-var override + glob matching.
func TestAllowlistedHostPath(t *testing.T) {
	t.Setenv("MOLECULE_EXTERNAL_REPO_ALLOWLIST", "")
	if !allowlistedHostPath("git.moleculesai.app", "molecule-ai/foo") {
		t.Error("default allowlist should accept molecule-ai/*")
	}
	if allowlistedHostPath("github.com", "molecule-ai/foo") {
		t.Error("default allowlist should reject github.com")
	}
	t.Setenv("MOLECULE_EXTERNAL_REPO_ALLOWLIST", "github.com/me/*,git.moleculesai.app/*")
	if !allowlistedHostPath("github.com", "me/x") {
		t.Error("override should accept github.com/me/*")
	}
	if !allowlistedHostPath("git.moleculesai.app", "any/repo") {
		t.Error("override should accept git.moleculesai.app/*")
	}
	if allowlistedHostPath("github.com", "evil/x") {
		t.Error("override should reject github.com/evil/*")
	}
}
