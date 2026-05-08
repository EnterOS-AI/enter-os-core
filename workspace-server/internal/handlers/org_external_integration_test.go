package handlers

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// PR-B integration test: exercises the REAL gitFetcher (no fakeFetcher
// injection) against a local bare-git repo. Uses git's `insteadOf`
// config to rewrite the configured Gitea URL to the local bare path
// at clone time, so the fetcher's URL-building, ls-remote, clone,
// atomic-rename, and cache-hit paths all run against real git
// without requiring network or modifying production code.
//
// Internal#77 task #233 (PR-B from the design's phasing).

// TestGitFetcher_RealClone_LocalRedirect proves the production
// gitFetcher round-trips correctly against a real git repository.
// Steps:
//   1. Set up a local bare-git repo with workspace content.
//   2. Configure git's `insteadOf` to rewrite the gitea URL → local path
//      via GIT_CONFIG_COUNT/KEY/VALUE env vars (process-scoped).
//   3. Run resolveYAMLIncludes with !external pointing at the gitea URL.
//   4. Assert: cache dir populated; content materialized; path rewrite
//      applied; second invocation hits cache (no second clone).
func TestGitFetcher_RealClone_LocalRedirect(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git binary not found: %v", err)
	}

	if runtime.GOOS == "windows" {
		t.Skip("path-based git URLs behave differently on Windows; skipping")
	}

	// Step 1: create a local bare-git repo at <fixtures>/test-dev-dept.git
	// with workspace content. Use a working clone to add content, then
	// push to the bare.
	fixtures := t.TempDir()
	barePath := filepath.Join(fixtures, "test-dev-dept.git")
	workPath := filepath.Join(fixtures, "work")

	mustGit(t, "", "init", "--bare", "-b", "main", barePath)
	mustGit(t, "", "clone", barePath, workPath)
	mustGit(t, workPath, "config", "user.email", "test@example.com")
	mustGit(t, workPath, "config", "user.name", "Integration Test")

	mustWriteFile(t, filepath.Join(workPath, "dev-lead/workspace.yaml"), `name: Dev Lead
files_dir: dev-lead
children:
  - !include ./core-be/workspace.yaml
`)
	mustWriteFile(t, filepath.Join(workPath, "dev-lead/system-prompt.md"), "Dev Lead persona body.\n")
	mustWriteFile(t, filepath.Join(workPath, "dev-lead/core-be/workspace.yaml"), `name: Core BE
files_dir: dev-lead/core-be
`)
	mustWriteFile(t, filepath.Join(workPath, "dev-lead/core-be/system-prompt.md"), "Core BE persona body.\n")

	mustGit(t, workPath, "add", ".")
	mustGit(t, workPath, "commit", "-m", "seed dev tree")
	mustGit(t, workPath, "push", "origin", "main")

	// Step 2: configure git's insteadOf rewrite. The fetcher will try
	// to clone https://git.moleculesai.app/molecule-ai/test-dev-dept.git;
	// git rewrites to file://<barePath>.
	//
	// GIT_CONFIG_COUNT/KEY/VALUE injects config without touching
	// ~/.gitconfig — process-scoped, no test pollution.
	geesUrl := "https://git.moleculesai.app/molecule-ai/test-dev-dept.git"
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "url."+barePath+".insteadOf")
	t.Setenv("GIT_CONFIG_VALUE_0", geesUrl)

	// Step 3: run resolveYAMLIncludes with !external pointing at the
	// gitea URL. Allowlist is the default (molecule-ai/* on Gitea host).
	rootDir := t.TempDir()
	src := []byte(`workspaces:
  - !external
    repo: molecule-ai/test-dev-dept
    ref: main
    path: dev-lead/workspace.yaml
`)

	out, err := resolveYAMLIncludes(src, rootDir)
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
	if !strings.Contains(dev.FilesDir, ".external-cache") {
		t.Errorf("dev.FilesDir = %q; want cache prefix", dev.FilesDir)
	}
	if !strings.HasSuffix(dev.FilesDir, "dev-lead") {
		t.Errorf("dev.FilesDir = %q; want suffix dev-lead", dev.FilesDir)
	}
	if len(dev.Children) != 1 {
		t.Fatalf("expected nested core-be child; got %+v", dev.Children)
	}
	core := dev.Children[0]
	if core.Name != "Core BE" {
		t.Errorf("core.Name = %q; want Core BE", core.Name)
	}
	if !strings.HasSuffix(core.FilesDir, filepath.Join("dev-lead", "core-be")) {
		t.Errorf("core.FilesDir = %q; want suffix dev-lead/core-be", core.FilesDir)
	}

	// Step 4: verify the cache dir actually exists and contains the
	// materialized files (CopyTemplateToContainer would tar these).
	cacheRoot := filepath.Join(rootDir, ".external-cache")
	entries, err := os.ReadDir(cacheRoot)
	if err != nil {
		t.Fatalf("read cache root: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 cached repo, got %d: %v", len(entries), entries)
	}
	repoDir := filepath.Join(cacheRoot, entries[0].Name())
	shaDirs, _ := os.ReadDir(repoDir)
	if len(shaDirs) != 1 {
		t.Fatalf("expected 1 SHA cache dir, got %d", len(shaDirs))
	}
	cacheDir := filepath.Join(repoDir, shaDirs[0].Name())
	if _, err := os.Stat(filepath.Join(cacheDir, "dev-lead/system-prompt.md")); err != nil {
		t.Errorf("expected dev-lead/system-prompt.md in cache: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "dev-lead/core-be/system-prompt.md")); err != nil {
		t.Errorf("expected dev-lead/core-be/system-prompt.md in cache: %v", err)
	}

	// Step 5: re-run; verify cache hit (no second clone). Set a
	// "marker" file in the cache that a second clone would clobber.
	marker := filepath.Join(cacheDir, ".cache-hit-marker")
	if err := os.WriteFile(marker, []byte("hit"), 0o644); err != nil {
		t.Fatal(err)
	}
	out2, err := resolveYAMLIncludes(src, rootDir)
	if err != nil {
		t.Fatalf("resolveYAMLIncludes second call: %v", err)
	}
	if string(out) != string(out2) {
		t.Errorf("cached output differs from initial — non-deterministic resolve")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("cache hit not honored — marker file disappeared: %v", err)
	}
}

// TestGitFetcher_RealClone_BadRefFails: pointing at a ref that doesn't
// exist in the bare-repo surfaces git's error cleanly.
func TestGitFetcher_RealClone_BadRefFails(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git binary not found: %v", err)
	}
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}

	fixtures := t.TempDir()
	barePath := filepath.Join(fixtures, "empty-repo.git")
	workPath := filepath.Join(fixtures, "work")
	mustGit(t, "", "init", "--bare", "-b", "main", barePath)
	mustGit(t, "", "clone", barePath, workPath)
	mustGit(t, workPath, "config", "user.email", "test@example.com")
	mustGit(t, workPath, "config", "user.name", "Test")
	mustWriteFile(t, filepath.Join(workPath, "README.md"), "x")
	mustGit(t, workPath, "add", ".")
	mustGit(t, workPath, "commit", "-m", "seed")
	mustGit(t, workPath, "push", "origin", "main")

	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "url."+barePath+".insteadOf")
	t.Setenv("GIT_CONFIG_VALUE_0", "https://git.moleculesai.app/molecule-ai/empty-repo.git")

	rootDir := t.TempDir()
	src := []byte(`workspaces:
  - !external
    repo: molecule-ai/empty-repo
    ref: nonexistent-branch
    path: anything.yaml
`)
	_, err := resolveYAMLIncludes(src, rootDir)
	if err == nil {
		t.Fatalf("expected error for nonexistent ref; got nil")
	}
	if !strings.Contains(err.Error(), "ref") && !strings.Contains(err.Error(), "ls-remote") && !strings.Contains(err.Error(), "not found") {
		t.Errorf("error doesn't mention ref/ls-remote: %v", err)
	}
}

// ---------- helpers ----------

func mustGit(t *testing.T, cwd string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if cwd != "" {
		cmd.Dir = cwd
	}
	// Ensure user.email/name are set globally for non-cwd commands too.
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_AUTHOR_NAME=Integration Test",
		"GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Integration Test",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, string(out))
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Verify gitFetcher.Fetch direct invocation (no resolver wrapping) for
// the cache-hit path, exercising the bare API against a local bare-repo.
func TestGitFetcher_DirectFetch_CacheHit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git binary not found: %v", err)
	}
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}

	fixtures := t.TempDir()
	barePath := filepath.Join(fixtures, "direct.git")
	workPath := filepath.Join(fixtures, "w")
	mustGit(t, "", "init", "--bare", "-b", "main", barePath)
	mustGit(t, "", "clone", barePath, workPath)
	mustGit(t, workPath, "config", "user.email", "t@e")
	mustGit(t, workPath, "config", "user.name", "T")
	mustWriteFile(t, filepath.Join(workPath, "marker.txt"), "hello")
	mustGit(t, workPath, "add", ".")
	mustGit(t, workPath, "commit", "-m", "seed")
	mustGit(t, workPath, "push", "origin", "main")

	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "url."+barePath+".insteadOf")
	t.Setenv("GIT_CONFIG_VALUE_0", "https://git.moleculesai.app/molecule-ai/direct.git")

	rootDir := t.TempDir()
	g := &gitFetcher{}
	ctx := context.Background()

	cacheDir1, sha1, err := g.Fetch(ctx, rootDir, "git.moleculesai.app", "molecule-ai/direct", "main")
	if err != nil {
		t.Fatalf("first Fetch: %v", err)
	}
	if sha1 == "" || len(sha1) < 7 {
		t.Errorf("expected SHA-like string, got %q", sha1)
	}
	if _, err := os.Stat(filepath.Join(cacheDir1, "marker.txt")); err != nil {
		t.Errorf("first fetch missing marker.txt: %v", err)
	}

	// Second call: cache hit, returns same dir + sha, no re-clone.
	stamp := filepath.Join(cacheDir1, ".not-clobbered-by-second-fetch")
	if err := os.WriteFile(stamp, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cacheDir2, sha2, err := g.Fetch(ctx, rootDir, "git.moleculesai.app", "molecule-ai/direct", "main")
	if err != nil {
		t.Fatalf("second Fetch: %v", err)
	}
	if cacheDir2 != cacheDir1 || sha2 != sha1 {
		t.Errorf("cache miss on second call: %q/%q vs %q/%q", cacheDir1, sha1, cacheDir2, sha2)
	}
	if _, err := os.Stat(stamp); err != nil {
		t.Errorf("cache hit not honored — stamp file disappeared: %v", err)
	}
}

// TestGitFetcher_RejectsRefWithDoubleDot: defense-in-depth on ref input.
// safeRefPattern allows '.' as a regex character, so ".." would match
// without an explicit deny. Verify it's rejected even though git itself
// would also reject the resulting clone.
func TestGitFetcher_RejectsRefWithDoubleDot(t *testing.T) {
	rootDir := t.TempDir()
	src := []byte(`workspaces:
  - !external
    repo: molecule-ai/x
    ref: foo..bar
    path: x.yaml
`)
	_, err := resolveYAMLIncludes(src, rootDir)
	if err == nil {
		t.Fatalf("expected '..' rejection")
	}
	if !strings.Contains(err.Error(), "..") {
		t.Errorf("expected '..' in error; got %v", err)
	}
}

// TestGitFetcher_CacheValidatedByCompleteMarker: a partially-written
// cache (the .git dir exists but no .complete marker) is treated as
// cache-miss and re-fetched. Catches the broken-cache-permanence bug.
func TestGitFetcher_CacheValidatedByCompleteMarker(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not found: %v", err)
	}
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}

	fixtures := t.TempDir()
	barePath := filepath.Join(fixtures, "test.git")
	workPath := filepath.Join(fixtures, "w")
	mustGit(t, "", "init", "--bare", "-b", "main", barePath)
	mustGit(t, "", "clone", barePath, workPath)
	mustGit(t, workPath, "config", "user.email", "t@e")
	mustGit(t, workPath, "config", "user.name", "T")
	mustWriteFile(t, filepath.Join(workPath, "good.txt"), "from-network")
	mustGit(t, workPath, "add", ".")
	mustGit(t, workPath, "commit", "-m", "seed")
	mustGit(t, workPath, "push", "origin", "main")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "url."+barePath+".insteadOf")
	t.Setenv("GIT_CONFIG_VALUE_0", "https://git.moleculesai.app/molecule-ai/marker-test.git")

	rootDir := t.TempDir()
	g := &gitFetcher{}

	// First fetch — populates the cache (creates .complete marker).
	cacheDir1, _, err := g.Fetch(context.Background(), rootDir, "git.moleculesai.app", "molecule-ai/marker-test", "main")
	if err != nil {
		t.Fatalf("first Fetch: %v", err)
	}
	marker := filepath.Join(cacheDir1, cacheCompleteMarker)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("first fetch should have written .complete marker: %v", err)
	}

	// Now simulate a partial cache: delete the marker but leave .git
	// in place. The next Fetch should treat this as cache-miss and
	// re-fetch (NOT silently use the partial cache).
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	// Drop a sentinel file the second fetch will clobber if it re-fetches.
	sentinel := filepath.Join(cacheDir1, "_should_be_clobbered")
	if err := os.WriteFile(sentinel, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}

	cacheDir2, _, err := g.Fetch(context.Background(), rootDir, "git.moleculesai.app", "molecule-ai/marker-test", "main")
	if err != nil {
		t.Fatalf("second Fetch: %v", err)
	}
	if cacheDir1 != cacheDir2 {
		t.Errorf("cache dirs differ across fetches: %q vs %q", cacheDir1, cacheDir2)
	}
	if _, err := os.Stat(filepath.Join(cacheDir2, cacheCompleteMarker)); err != nil {
		t.Errorf("re-fetch should have re-written .complete marker: %v", err)
	}
	if _, err := os.Stat(sentinel); err == nil {
		t.Errorf("sentinel still present — re-fetch did NOT clobber partial cache")
	}
}
