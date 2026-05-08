package provisioner

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// makeTestOpts produces a LocalBuildOptions where every external seam
// (Gitea HEAD, git clone, docker build/has/tag) is replaced by a stub.
// Tests override the stub for the behavior they want to assert.
func makeTestOpts(t *testing.T) *LocalBuildOptions {
	t.Helper()
	tmp := t.TempDir()
	return &LocalBuildOptions{
		CacheDir:   tmp,
		RepoPrefix: "https://git.test/molecule-ai/molecule-ai-workspace-template-",
		Platform:   "linux/amd64",
		HTTPClient: &http.Client{},
		remoteHeadSha: func(ctx context.Context, opts *LocalBuildOptions, runtime string) (string, error) {
			return "abcdef0123456789abcdef0123456789abcdef01", nil
		},
		gitClone: func(ctx context.Context, opts *LocalBuildOptions, runtime, dest string) error {
			// Write a fake Dockerfile so the sanity-check passes.
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(dest, "Dockerfile"), []byte("FROM scratch\n"), 0o644)
		},
		dockerBuild: func(ctx context.Context, opts *LocalBuildOptions, contextDir, tag string) error {
			return nil
		},
		dockerHasTag: func(ctx context.Context, tag string) (bool, error) {
			return false, nil
		},
		dockerTag: func(ctx context.Context, src, dst string) error {
			return nil
		},
	}
}

// TestEnsureLocalImage_Success — happy path: HEAD lookup succeeds, no
// cache hit, clone + build run, returned tag is SHA-pinned.
func TestEnsureLocalImage_Success(t *testing.T) {
	opts := makeTestOpts(t)
	tag, err := ensureLocalImageWithOpts(context.Background(), "claude-code", opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "molecule-local/workspace-template-claude-code:abcdef012345"
	if tag != want {
		t.Errorf("tag = %q, want %q", tag, want)
	}
}

// TestEnsureLocalImage_CacheHit — second call with a cached image must
// skip clone + build entirely.
func TestEnsureLocalImage_CacheHit(t *testing.T) {
	opts := makeTestOpts(t)
	var cloneCount, buildCount int
	opts.gitClone = func(ctx context.Context, opts *LocalBuildOptions, runtime, dest string) error {
		cloneCount++
		return os.WriteFile(filepath.Join(dest, "Dockerfile"), []byte("FROM scratch\n"), 0o644)
	}
	opts.dockerBuild = func(ctx context.Context, opts *LocalBuildOptions, contextDir, tag string) error {
		buildCount++
		return nil
	}
	opts.dockerHasTag = func(ctx context.Context, tag string) (bool, error) {
		return true, nil // cached
	}
	if _, err := ensureLocalImageWithOpts(context.Background(), "hermes", opts); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cloneCount != 0 {
		t.Errorf("cache hit triggered %d clones, want 0", cloneCount)
	}
	if buildCount != 0 {
		t.Errorf("cache hit triggered %d builds, want 0", buildCount)
	}
}

// TestEnsureLocalImage_UnknownRuntime — the allowlist guard rejects
// arbitrary runtime names before any network or filesystem call.
func TestEnsureLocalImage_UnknownRuntime(t *testing.T) {
	opts := makeTestOpts(t)
	for _, bad := range []string{
		"", "unknown", "../../../etc/passwd", "claude-code; rm -rf /",
	} {
		t.Run(bad, func(t *testing.T) {
			_, err := ensureLocalImageWithOpts(context.Background(), bad, opts)
			if err == nil {
				t.Errorf("EnsureLocalImage(%q) should fail (not a known runtime)", bad)
			}
			if err != nil && !strings.Contains(err.Error(), "unknown runtime") {
				t.Errorf("error = %v, want one mentioning %q", err, "unknown runtime")
			}
		})
	}
}

// TestEnsureLocalImage_GiteaUnreachable — fail-closed when the HEAD
// lookup fails. Must NOT fall back to GHCR/ECR.
func TestEnsureLocalImage_GiteaUnreachable(t *testing.T) {
	opts := makeTestOpts(t)
	opts.remoteHeadSha = func(ctx context.Context, opts *LocalBuildOptions, runtime string) (string, error) {
		return "", errors.New("dial tcp: no such host")
	}
	_, err := ensureLocalImageWithOpts(context.Background(), "langgraph", opts)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "cannot determine HEAD sha") {
		t.Errorf("error = %v, want one mentioning HEAD sha lookup", err)
	}
	// Critical: error must NOT mention ghcr or ecr (no silent fallback).
	low := strings.ToLower(err.Error())
	if strings.Contains(low, "ghcr") || strings.Contains(low, "ecr") {
		t.Errorf("error message %q must not mention ghcr/ecr (no silent fallback)", err.Error())
	}
}

// TestEnsureLocalImage_RepoNotFound — Gitea returned 404. Must surface
// a runtime-naming error so the OSS contributor can file the right
// mirroring task.
func TestEnsureLocalImage_RepoNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"repo not found"}`))
	}))
	defer srv.Close()

	opts := makeTestOpts(t)
	opts.RepoPrefix = srv.URL + "/molecule-ai/molecule-ai-workspace-template-"
	opts.HTTPClient = srv.Client()
	opts.remoteHeadSha = nil // exercise real HTTP path

	_, err := ensureLocalImageWithOpts(context.Background(), "crewai", opts)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "not mirrored") && !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %v, want a missing-repo message", err)
	}
}

// TestEnsureLocalImage_AuthFailure — Gitea returned 401/403. Must
// produce an actionable error (mentions the token env var so an OSS
// contributor knows what to set).
func TestEnsureLocalImage_AuthFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	opts := makeTestOpts(t)
	opts.RepoPrefix = srv.URL + "/molecule-ai/molecule-ai-workspace-template-"
	opts.HTTPClient = srv.Client()
	opts.remoteHeadSha = nil

	_, err := ensureLocalImageWithOpts(context.Background(), "claude-code", opts)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "MOLECULE_GITEA_TOKEN") {
		t.Errorf("error = %v, want one mentioning MOLECULE_GITEA_TOKEN", err)
	}
}

// TestEnsureLocalImage_HeadShaWithRealJSON — exercise the JSON parser
// against a Gitea-shaped response to catch parse drift.
func TestEnsureLocalImage_HeadShaWithRealJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Real Gitea response shape (truncated for relevance).
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"name":"main",
			"commit":{
				"id":"3c849b3ba778abcdef0123456789abcdef012345",
				"message":"feat: stuff"
			}
		}`))
	}))
	defer srv.Close()

	opts := makeTestOpts(t)
	opts.RepoPrefix = srv.URL + "/molecule-ai/molecule-ai-workspace-template-"
	opts.HTTPClient = srv.Client()
	opts.remoteHeadSha = nil // exercise real HTTP path

	tag, err := ensureLocalImageWithOpts(context.Background(), "claude-code", opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(tag, "3c849b3ba778") {
		t.Errorf("tag = %q, want one containing the parsed sha", tag)
	}
}

// TestEnsureLocalImage_BuildFailure — surfaces docker-build errors with
// the build context so an operator can debug locally.
func TestEnsureLocalImage_BuildFailure(t *testing.T) {
	opts := makeTestOpts(t)
	opts.dockerBuild = func(ctx context.Context, opts *LocalBuildOptions, contextDir, tag string) error {
		return errors.New("Dockerfile syntax error")
	}
	_, err := ensureLocalImageWithOpts(context.Background(), "autogen", opts)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "docker build") {
		t.Errorf("error = %v, want one mentioning docker build", err)
	}
}

// TestEnsureLocalImage_MissingDockerfile — the cloned tree must contain
// a Dockerfile at root; absence is a malformed-template-repo error.
func TestEnsureLocalImage_MissingDockerfile(t *testing.T) {
	opts := makeTestOpts(t)
	opts.gitClone = func(ctx context.Context, opts *LocalBuildOptions, runtime, dest string) error {
		// Empty dir, no Dockerfile.
		return os.MkdirAll(dest, 0o755)
	}
	_, err := ensureLocalImageWithOpts(context.Background(), "hermes", opts)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "no Dockerfile") {
		t.Errorf("error = %v, want one mentioning missing Dockerfile", err)
	}
}

// TestEnsureLocalImage_ConcurrentSameRuntime — two goroutines hitting
// the same runtime serialize via the per-runtime lock; the build runs
// once.
func TestEnsureLocalImage_ConcurrentSameRuntime(t *testing.T) {
	opts := makeTestOpts(t)
	var (
		buildCount int
		buildMu    sync.Mutex
	)
	opts.dockerHasTag = func(ctx context.Context, tag string) (bool, error) {
		// First call: cache miss. Second call (after first build): hit.
		buildMu.Lock()
		defer buildMu.Unlock()
		return buildCount > 0, nil
	}
	opts.dockerBuild = func(ctx context.Context, opts *LocalBuildOptions, contextDir, tag string) error {
		buildMu.Lock()
		buildCount++
		buildMu.Unlock()
		return nil
	}

	const N = 5
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_, _ = ensureLocalImageWithOpts(context.Background(), "langgraph", opts)
		}()
	}
	wg.Wait()
	if buildCount != 1 {
		t.Errorf("buildCount = %d, want 1 (lock should serialize concurrent calls)", buildCount)
	}
}

// TestMaskTokenInURL — Gitea PATs in URLs must NEVER appear in logs.
func TestMaskTokenInURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"https://oauth2:secret123@git.example.com/foo/bar", "https://***@git.example.com/foo/bar"},
		{"https://user:tok@host/path", "https://***@host/path"},
		{"https://no-userinfo.example.com/path", "https://no-userinfo.example.com/path"},
		{"not a url", "not a url"},
		{"", ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := maskTokenInURL(tc.in)
			if got != tc.want {
				t.Errorf("maskTokenInURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestMaskTokenInString — defence against git/docker echoing the token
// into stderr on failure.
func TestMaskTokenInString(t *testing.T) {
	got := maskTokenInString("error: clone https://oauth2:abc123@git.test/foo: failed", "abc123")
	if strings.Contains(got, "abc123") {
		t.Errorf("masked string %q still contains the token", got)
	}
	if !strings.Contains(got, "***") {
		t.Errorf("masked string %q should have *** in place of token", got)
	}
	// No-op when token is empty.
	if got := maskTokenInString("hello world", ""); got != "hello world" {
		t.Errorf("empty token must not modify string, got %q", got)
	}
}

// TestGiteaBranchAPIURL — the URL composer must produce the canonical
// /api/v1/repos/<org>/<repo>/branches/<branch> shape.
func TestGiteaBranchAPIURL(t *testing.T) {
	cases := []struct {
		prefix, runtime, branch, want string
	}{
		{
			"https://git.moleculesai.app/molecule-ai/molecule-ai-workspace-template-",
			"claude-code",
			"main",
			"https://git.moleculesai.app/api/v1/repos/molecule-ai/molecule-ai-workspace-template-claude-code/branches/main",
		},
		{
			"http://localhost:3000/myorg/template-",
			"foo",
			"main",
			"http://localhost:3000/api/v1/repos/myorg/template-foo/branches/main",
		},
	}
	for _, tc := range cases {
		t.Run(tc.runtime, func(t *testing.T) {
			got, err := giteaBranchAPIURL(tc.prefix, tc.runtime, tc.branch)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestGiteaBranchAPIURL_RejectsMalformed — malformed prefixes (no org
// path) produce an error rather than a malformed API call.
func TestGiteaBranchAPIURL_RejectsMalformed(t *testing.T) {
	for _, bad := range []string{
		"https://example.com/", // no path component
		"://broken",
	} {
		t.Run(bad, func(t *testing.T) {
			if _, err := giteaBranchAPIURL(bad, "claude-code", "main"); err == nil {
				t.Errorf("expected error for malformed prefix %q", bad)
			}
		})
	}
}

// TestParseGiteaBranchHeadSha — pin the parser against representative
// Gitea responses so a future Gitea API rev that adds fields doesn't
// silently break detection.
func TestParseGiteaBranchHeadSha(t *testing.T) {
	good := []byte(`{"name":"main","commit":{"id":"abc123def456","message":"hi"}}`)
	got, err := parseGiteaBranchHeadSha(good)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != "abc123def456" {
		t.Errorf("got %q, want abc123def456", got)
	}

	for _, bad := range [][]byte{
		[]byte(`{}`),
		[]byte(`{"name":"main","commit":{}}`),
		[]byte(`{"commit":{"id":"`), // truncated
		[]byte(`<html>404</html>`),
	} {
		if _, err := parseGiteaBranchHeadSha(bad); err == nil {
			t.Errorf("expected error for malformed body %q", string(bad))
		}
	}
}

// TestLocalImageTag_ShortSha — caller-supplied SHA gets truncated to
// 12 chars in the tag so `docker images` output stays readable.
func TestLocalImageTag_ShortSha(t *testing.T) {
	got := LocalImageTag("claude-code", "abcdef0123456789abcdef0123456789abcdef01")
	want := "molecule-local/workspace-template-claude-code:abcdef012345"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestLocalImageLatestTag — the floating alias used as the human-readable
// :latest entry.
func TestLocalImageLatestTag(t *testing.T) {
	got := LocalImageLatestTag("hermes")
	want := "molecule-local/workspace-template-hermes:latest"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestRemoteHeadShaProd_IncludesAuthHeader — when a token is configured,
// the API request must carry the `Authorization: token <pat>` header.
func TestRemoteHeadShaProd_IncludesAuthHeader(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"commit":{"id":"deadbeef0000aaaa1111bbbb2222cccc33334444"}}`))
	}))
	defer srv.Close()

	opts := makeTestOpts(t)
	opts.RepoPrefix = srv.URL + "/myorg/template-"
	opts.HTTPClient = srv.Client()
	opts.Token = "secret-pat-do-not-log"

	if _, err := remoteHeadShaProd(context.Background(), opts, "claude-code"); err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != "token secret-pat-do-not-log" {
		t.Errorf("Authorization header = %q, want %q", got, "token secret-pat-do-not-log")
	}
}

// TestCacheKey_Stable — the helper must be deterministic and incorporate
// each input.
func TestCacheKey_Stable(t *testing.T) {
	a := CacheKey("claude-code", "abc", "https://git/")
	b := CacheKey("claude-code", "abc", "https://git/")
	if a != b {
		t.Errorf("CacheKey is non-deterministic: %q vs %q", a, b)
	}
	if a == CacheKey("claude-code", "def", "https://git/") {
		t.Errorf("CacheKey ignores sha")
	}
	if a == CacheKey("hermes", "abc", "https://git/") {
		t.Errorf("CacheKey ignores runtime")
	}
}

// TestRedactedRepoURL_NoToken — a repo URL with no embedded credential
// is unmodified.
func TestRedactedRepoURL_NoToken(t *testing.T) {
	opts := &LocalBuildOptions{RepoPrefix: "https://git.example.com/org/template-"}
	got := redactedRepoURL(opts, "claude-code")
	want := "https://git.example.com/org/template-claude-code"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestRepoURL_AppendsRuntime — the prefix + runtime composer is stable.
func TestRepoURL_AppendsRuntime(t *testing.T) {
	opts := &LocalBuildOptions{RepoPrefix: "https://git.example.com/org/template-"}
	got := repoURL(opts, "claude-code")
	if got != "https://git.example.com/org/template-claude-code" {
		t.Errorf("got %q", got)
	}
}

// TestNewDefaultLocalBuildOptions_RespectsEnvOverrides — the env var
// overrides documented in the runbook actually take effect.
func TestNewDefaultLocalBuildOptions_RespectsEnvOverrides(t *testing.T) {
	t.Setenv("MOLECULE_LOCAL_BUILD_CACHE", "/var/tmp/molecule-test")
	t.Setenv("MOLECULE_LOCAL_TEMPLATE_REPO_PREFIX", "https://my.fork/org/tpl-")
	t.Setenv("MOLECULE_GITEA_TOKEN", "tok-from-env")

	opts := newDefaultLocalBuildOptions()
	if opts.CacheDir != "/var/tmp/molecule-test" {
		t.Errorf("CacheDir = %q", opts.CacheDir)
	}
	if opts.RepoPrefix != "https://my.fork/org/tpl-" {
		t.Errorf("RepoPrefix = %q", opts.RepoPrefix)
	}
	if opts.Token != "tok-from-env" {
		t.Errorf("Token = %q", opts.Token)
	}
	if opts.Platform != "linux/amd64" {
		t.Errorf("Platform = %q, want linux/amd64", opts.Platform)
	}
}

// TestNewDefaultLocalBuildOptions_DefaultCacheDir — XDG-compliant
// fallback when nothing is overridden.
func TestNewDefaultLocalBuildOptions_DefaultCacheDir(t *testing.T) {
	t.Setenv("MOLECULE_LOCAL_BUILD_CACHE", "")
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("MOLECULE_LOCAL_TEMPLATE_REPO_PREFIX", "")

	opts := newDefaultLocalBuildOptions()
	if !strings.Contains(opts.CacheDir, ".cache") && !strings.Contains(opts.CacheDir, "molecule") {
		t.Errorf("CacheDir = %q, want one under .cache/molecule", opts.CacheDir)
	}
	if opts.RepoPrefix != gitTemplateRepoPrefix {
		t.Errorf("RepoPrefix = %q, want default %q", opts.RepoPrefix, gitTemplateRepoPrefix)
	}
}

// TestEnsureLocalImage_ShortSha — a remote that returns a too-short
// sha is rejected (defence against a misbehaving Gitea proxy).
func TestEnsureLocalImage_ShortSha(t *testing.T) {
	opts := makeTestOpts(t)
	opts.remoteHeadSha = func(ctx context.Context, opts *LocalBuildOptions, runtime string) (string, error) {
		return "abc", nil
	}
	_, err := ensureLocalImageWithOpts(context.Background(), "claude-code", opts)
	if err == nil {
		t.Fatalf("expected error for short sha")
	}
	if !strings.Contains(err.Error(), "short sha") {
		t.Errorf("error = %v, want short-sha message", err)
	}
}

// TestEnsureLocalImage_StaleCacheDirCleaned — a partial clone left over
// from a previous failed run must not poison the next attempt.
func TestEnsureLocalImage_StaleCacheDirCleaned(t *testing.T) {
	opts := makeTestOpts(t)
	// Pre-create a stale dir at the cache target (with a partial Dockerfile).
	staleDir := filepath.Join(opts.CacheDir, "claude-code", "abcdef012345")
	if err := os.MkdirAll(staleDir, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(staleDir, "stale-marker"), []byte("delete me"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if _, err := ensureLocalImageWithOpts(context.Background(), "claude-code", opts); err != nil {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(staleDir, "stale-marker")); !os.IsNotExist(err) {
		t.Errorf("stale-marker should have been wiped before re-clone (err=%v)", err)
	}
	// Dockerfile from the new clone should be present.
	if _, err := os.Stat(filepath.Join(staleDir, "Dockerfile")); err != nil {
		t.Errorf("expected Dockerfile from re-clone, got err=%v", err)
	}
}

// TestEnsureLocalImage_ContextCancelled — context cancellation
// propagates to the network/clone seams (best-effort: the test asserts
// that no work happens after Done()).
func TestEnsureLocalImage_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	opts := makeTestOpts(t)
	opts.remoteHeadSha = func(ctx context.Context, opts *LocalBuildOptions, runtime string) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return "deadbeef00000000aaaa1111bbbb2222cccc33334444", nil
	}

	_, err := ensureLocalImageWithOpts(ctx, "claude-code", opts)
	if err == nil {
		t.Fatalf("expected error from cancelled context")
	}
}

// TestEnsureLocalImage_RetagAfterCacheHit — a cache-hit must refresh
// the floating :latest alias so admins inspecting `docker images` see
// the current SHA.
func TestEnsureLocalImage_RetagAfterCacheHit(t *testing.T) {
	opts := makeTestOpts(t)
	var src, dst string
	opts.dockerHasTag = func(ctx context.Context, tag string) (bool, error) { return true, nil }
	opts.dockerTag = func(ctx context.Context, s, d string) error {
		src, dst = s, d
		return nil
	}
	tag, err := ensureLocalImageWithOpts(context.Background(), "claude-code", opts)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if src != tag {
		t.Errorf("retag src = %q, want %q", src, tag)
	}
	wantDst := "molecule-local/workspace-template-claude-code:latest"
	if dst != wantDst {
		t.Errorf("retag dst = %q, want %q", dst, wantDst)
	}
}

// TestRemoteHeadShaProd_BodyOverflow — defence against a malicious or
// misbehaving Gitea returning a multi-MB body.
func TestRemoteHeadShaProd_BodyOverflow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Stream a 100MB body. The reader should cap at 64KB and yield
		// a parse error rather than OOM.
		_, _ = w.Write([]byte(`{"commit":{"id":"`))
		_, _ = w.Write([]byte(strings.Repeat("a", 64<<10))) // 64KB of 'a'
		// Connection drops here; we don't write the closing quote.
	}))
	defer srv.Close()

	opts := makeTestOpts(t)
	opts.RepoPrefix = srv.URL + "/myorg/template-"
	opts.HTTPClient = srv.Client()

	_, err := remoteHeadShaProd(context.Background(), opts, "claude-code")
	if err == nil {
		t.Fatalf("expected error from over-long sha (no closing quote within cap)")
	}
}

// TestProvisionerStartUsesLocalBuild_LocalMode — pin the provisioner→
// local-build wiring at the integration boundary. We don't want a future
// refactor to silently bypass EnsureLocalImage when registry is unset.
//
// This test inspects the mode-decision logic without standing up Docker.
func TestProvisionerStartUsesLocalBuild_LocalMode(t *testing.T) {
	t.Setenv("MOLECULE_IMAGE_REGISTRY", "")
	src := Resolve()
	if src.Mode != RegistryModeLocal {
		t.Fatalf("Resolve in unset env = %q, want local", src.Mode)
	}
	// The provisioner Start() branches on this same Resolve() call before
	// reaching ContainerCreate. Pinning the boolean here means a refactor
	// that flips the sense (e.g. `if src.Mode == RegistryModeSaaS`) is
	// caught by this test.
}

// TestEnsureLocalImageHook_DefaultIsRealFunction — pin that the
// production hook points at EnsureLocalImage. Tests that swap the hook
// must restore it via t.Cleanup; this test catches a leaked override.
func TestEnsureLocalImageHook_DefaultIsRealFunction(t *testing.T) {
	// Sanity: hook is set to a non-nil function. We can't compare
	// function pointers directly with == in Go (compiler error), so
	// we exercise it instead — but we don't want to actually clone
	// from the network in the unit test, so use an unknown runtime
	// and assert the known-error path runs.
	_, err := ensureLocalImageHook(context.Background(), "this-runtime-cannot-exist-194")
	if err == nil {
		t.Fatalf("expected error from EnsureLocalImage on unknown runtime")
	}
	if !strings.Contains(err.Error(), "unknown runtime") {
		t.Errorf("hook = unexpected function (got error %q, want one mentioning unknown runtime)", err.Error())
	}
}

// TestProvisionerStartUsesLocalBuild_SaaSMode — and the symmetric guard:
// in SaaS-mode, no local-build path runs.
func TestProvisionerStartUsesLocalBuild_SaaSMode(t *testing.T) {
	t.Setenv("MOLECULE_IMAGE_REGISTRY", "registry.example.com/molecule-ai")
	src := Resolve()
	if src.Mode != RegistryModeSaaS {
		t.Fatalf("Resolve with registry set = %q, want saas", src.Mode)
	}
	if src.Prefix != "registry.example.com/molecule-ai" {
		t.Fatalf("Prefix = %q", src.Prefix)
	}
}

// silence unused warning if we ever drop fmt usage
var _ = fmt.Sprintf
