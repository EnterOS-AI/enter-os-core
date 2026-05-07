package provisioner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Local-build mode: clone the workspace-template-<runtime> repo from Gitea
// and `docker build` it on the host so OSS contributors can run molecule-core
// end-to-end without authenticating to (or being able to reach) GHCR/ECR.
//
// The flow:
//
//  1. ensureLocalImage(runtime) is called by the provisioner before
//     ContainerCreate, but only when Resolve().Mode == RegistryModeLocal.
//  2. We compute a cache key from the Gitea repo's HEAD sha (one HTTP
//     call to https://git.moleculesai.app/api/v1/repos/.../branches/main).
//  3. If `molecule-local/workspace-template-<runtime>:<sha12>` already
//     exists in the local Docker image store, we return immediately.
//  4. Otherwise: shallow git-clone the repo into the cache dir, then
//     `docker buildx build --platform=linux/amd64 -t <tag>` on it. We
//     also tag `:latest` so `docker images` shows a friendly entry.
//
// Why amd64 emulation: the provisioner's defaultImagePlatform() forces
// linux/amd64 on Apple Silicon for parity with the (amd64-only) prod
// images. Building native arm64 in local-mode would diverge — see the
// design rationale in Issue #63 and the saved memory
// `feedback_local_must_mimic_production`.
//
// Auth: clone is anonymous (templates are public). If MOLECULE_GITEA_TOKEN
// is set, we use it via the URL's userinfo — the token is masked in
// every log line by maskTokenInURL().
//
// Failure mode: fail-closed. If Gitea is unreachable we surface a clear
// error message including the repo URL; we NEVER fall back to GHCR/ECR
// silently (would be a confusing bug for an OSS contributor who
// happens to have stale ECR creds in their docker config).

// gitTemplateRepoPrefix is the prefix all workspace-template repos live
// under on Gitea. Hardcoded so an attacker who controlled cfg.Runtime
// (defence-in-depth — today the field is platform-validated upstream)
// can only ever reach a repo under molecule-ai/.
//
// Operators who want to point local-build at a fork can override the
// full prefix via MOLECULE_LOCAL_TEMPLATE_REPO_PREFIX (e.g.
// `https://git.example.com/myorg/molecule-ai-workspace-template-`).
// Default-off; opt-in only.
const gitTemplateRepoPrefix = "https://git.moleculesai.app/molecule-ai/molecule-ai-workspace-template-"

// localBuildLockMap serializes concurrent ensureLocalImage calls per
// runtime so two workspace creates that hit the cold path together don't
// race on `docker build` (Docker's daemon would serialize anyway, but
// the duplicate clone + log spam are confusing). Lock granularity is
// per-runtime, so different runtimes still build in parallel.
var (
	localBuildLockMap   = make(map[string]*sync.Mutex)
	localBuildLockMapMu sync.Mutex
)

func runtimeBuildLock(runtime string) *sync.Mutex {
	localBuildLockMapMu.Lock()
	defer localBuildLockMapMu.Unlock()
	if m, ok := localBuildLockMap[runtime]; ok {
		return m
	}
	m := &sync.Mutex{}
	localBuildLockMap[runtime] = m
	return m
}

// LocalBuildOptions controls the local-build path. Exposed so tests can
// inject fakes without standing up a real git+docker chain. Production
// uses zero-value defaults via newDefaultLocalBuildOptions().
type LocalBuildOptions struct {
	// CacheDir is the host filesystem location where cloned template
	// repos are kept between builds. Empty = use $XDG_CACHE_HOME or
	// $HOME/.cache. Override via env var MOLECULE_LOCAL_BUILD_CACHE.
	CacheDir string

	// RepoPrefix is the URL prefix all template repos hang off. Empty
	// = use gitTemplateRepoPrefix. Override via env var
	// MOLECULE_LOCAL_TEMPLATE_REPO_PREFIX.
	RepoPrefix string

	// Token, if non-empty, is sent via URL userinfo to Gitea. Default
	// empty (templates are public). Override via env var
	// MOLECULE_GITEA_TOKEN.
	Token string

	// Platform is the buildx --platform value. Empty = host default;
	// today we always pass linux/amd64 because the provisioner only
	// runs amd64 images. Exposed so tests can override.
	Platform string

	// HTTPClient is used for the Gitea-API HEAD-sha lookup. Empty =
	// http.DefaultClient with a 30s timeout.
	HTTPClient *http.Client

	// remoteHeadSha + dockerBuild + gitClone are seams for tests; if
	// nil, the production implementations are used.
	remoteHeadSha func(ctx context.Context, opts *LocalBuildOptions, runtime string) (string, error)
	gitClone      func(ctx context.Context, opts *LocalBuildOptions, runtime, dest string) error
	dockerBuild   func(ctx context.Context, opts *LocalBuildOptions, contextDir, tag string) error
	dockerHasTag  func(ctx context.Context, tag string) (bool, error)
	dockerTag     func(ctx context.Context, src, dst string) error
}

func newDefaultLocalBuildOptions() *LocalBuildOptions {
	o := &LocalBuildOptions{
		CacheDir:   os.Getenv("MOLECULE_LOCAL_BUILD_CACHE"),
		RepoPrefix: os.Getenv("MOLECULE_LOCAL_TEMPLATE_REPO_PREFIX"),
		Token:      os.Getenv("MOLECULE_GITEA_TOKEN"),
		Platform:   "linux/amd64",
	}
	if o.CacheDir == "" {
		if xdg := os.Getenv("XDG_CACHE_HOME"); xdg != "" {
			o.CacheDir = filepath.Join(xdg, "molecule", "workspace-template-build")
		} else if home, err := os.UserHomeDir(); err == nil {
			o.CacheDir = filepath.Join(home, ".cache", "molecule", "workspace-template-build")
		} else {
			// Last-resort fallback: /tmp. Loses the cache between reboots
			// but at least lets the path produce builds.
			o.CacheDir = filepath.Join(os.TempDir(), "molecule", "workspace-template-build")
		}
	}
	if o.RepoPrefix == "" {
		o.RepoPrefix = gitTemplateRepoPrefix
	}
	o.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	return o
}

// LocalImageTag formats the SHA-pinned tag for a runtime. Exported for
// tests + the provisioner's image-resolution branch.
func LocalImageTag(runtime, sha string) string {
	short := sha
	if len(short) > 12 {
		short = short[:12]
	}
	return fmt.Sprintf("%s/workspace-template-%s:%s", localImagePrefix, runtime, short)
}

// LocalImageLatestTag returns the floating `:latest` form. Used as a
// human-readable alias and as the value RuntimeImage() returns in
// local-mode.
func LocalImageLatestTag(runtime string) string {
	return fmt.Sprintf("%s/workspace-template-%s:latest", localImagePrefix, runtime)
}

// EnsureLocalImage is the entry point the provisioner calls before
// ContainerCreate when Resolve().Mode == RegistryModeLocal. Returns the
// image tag (SHA-pinned form) the caller should hand to Docker, or an
// error if the build/clone fails.
//
// Concurrency: per-runtime lock; parallel calls for the same runtime
// share the build, parallel calls for different runtimes proceed.
//
// Idempotent: a cached SHA-pinned tag short-circuits without network
// or docker calls. The Gitea HEAD lookup is the only network call on
// the cache-hit path.
func EnsureLocalImage(ctx context.Context, runtime string) (string, error) {
	return ensureLocalImageWithOpts(ctx, runtime, newDefaultLocalBuildOptions())
}

// ensureLocalImageHook is the seam Start() calls into. Production code
// uses EnsureLocalImage; tests substitute a fake to exercise the
// provisioner-Start integration without standing up a real
// git+docker chain. Single-process scoped — never reassigned in
// production code.
var ensureLocalImageHook = EnsureLocalImage

func ensureLocalImageWithOpts(ctx context.Context, runtime string, opts *LocalBuildOptions) (string, error) {
	if !IsKnownRuntime(runtime) {
		return "", fmt.Errorf("local-build: refusing to build unknown runtime %q (must be one of %v)", runtime, knownRuntimes)
	}

	lock := runtimeBuildLock(runtime)
	lock.Lock()
	defer lock.Unlock()

	// 1. HEAD lookup → cache key.
	headFn := opts.remoteHeadSha
	if headFn == nil {
		headFn = remoteHeadShaProd
	}
	sha, err := headFn(ctx, opts, runtime)
	if err != nil {
		// Fail-closed: do not fall back to GHCR/ECR. The whole point of
		// local-build mode is that GHCR is unreachable.
		return "", fmt.Errorf("local-build: cannot determine HEAD sha for runtime %q at %s: %w", runtime, repoURL(opts, runtime), err)
	}
	if len(sha) < 12 {
		return "", fmt.Errorf("local-build: Gitea returned a short sha %q for runtime %q (expected ≥12 chars)", sha, runtime)
	}
	tag := LocalImageTag(runtime, sha)
	latest := LocalImageLatestTag(runtime)

	// 2. Cache hit?
	hasFn := opts.dockerHasTag
	if hasFn == nil {
		hasFn = dockerHasTagProd
	}
	exists, hasErr := hasFn(ctx, tag)
	if hasErr != nil {
		log.Printf("local-build: image inspect for %s failed (%v); will rebuild", tag, hasErr)
	}
	if exists {
		log.Printf("local-build: cache hit for %s (sha=%s) — skipping clone+build", tag, sha[:12])
		// Refresh the floating :latest alias so admins inspecting `docker
		// images` see the current sha. Best-effort.
		tagFn := opts.dockerTag
		if tagFn == nil {
			tagFn = dockerTagProd
		}
		if tErr := tagFn(ctx, tag, latest); tErr != nil {
			log.Printf("local-build: best-effort retag of %s → %s failed: %v", tag, latest, tErr)
		}
		return tag, nil
	}

	// 3. Cold path — clone + build.
	dest := filepath.Join(opts.CacheDir, runtime, sha[:12])
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", fmt.Errorf("local-build: prepare cache dir %q: %w", filepath.Dir(dest), err)
	}
	// Idempotent: if the dest exists from a previous failed run, wipe and
	// re-clone so we don't build a partial tree.
	if _, statErr := os.Stat(dest); statErr == nil {
		if rmErr := os.RemoveAll(dest); rmErr != nil {
			return "", fmt.Errorf("local-build: clean stale cache dir %q: %w", dest, rmErr)
		}
	}

	cloneFn := opts.gitClone
	if cloneFn == nil {
		cloneFn = gitCloneProd
	}
	log.Printf("local-build: cloning %s → %s (sha=%s)", redactedRepoURL(opts, runtime), dest, sha[:12])
	cloneStart := time.Now()
	if err := cloneFn(ctx, opts, runtime, dest); err != nil {
		// Best-effort cleanup so a half-cloned tree doesn't poison future runs.
		_ = os.RemoveAll(dest)
		return "", fmt.Errorf("local-build: clone %s: %w", redactedRepoURL(opts, runtime), err)
	}
	log.Printf("local-build: clone complete in %s", time.Since(cloneStart).Round(time.Millisecond))

	// 4. Sanity-check the cloned tree contains a Dockerfile at the root.
	dockerfile := filepath.Join(dest, "Dockerfile")
	info, statErr := os.Stat(dockerfile)
	if statErr != nil || info.IsDir() {
		_ = os.RemoveAll(dest)
		return "", fmt.Errorf("local-build: cloned tree at %s has no Dockerfile (template repo malformed)", dest)
	}

	// 5. Build.
	buildFn := opts.dockerBuild
	if buildFn == nil {
		buildFn = dockerBuildProd
	}
	log.Printf("local-build: docker build start for %s (platform=%s, context=%s)", tag, opts.Platform, dest)
	buildStart := time.Now()
	if err := buildFn(ctx, opts, dest, tag); err != nil {
		return "", fmt.Errorf("local-build: docker build %s: %w", tag, err)
	}
	log.Printf("local-build: docker build done for %s in %s", tag, time.Since(buildStart).Round(time.Second))

	// Tag :latest as a friendly alias.
	tagFn := opts.dockerTag
	if tagFn == nil {
		tagFn = dockerTagProd
	}
	if err := tagFn(ctx, tag, latest); err != nil {
		log.Printf("local-build: best-effort retag of %s → %s failed: %v", tag, latest, err)
	}

	return tag, nil
}

// repoURL composes the full Gitea repo URL for the given runtime. The
// prefix is hardcoded by default; operators can override via env so a
// fork can point local-build at their own Gitea instance.
func repoURL(opts *LocalBuildOptions, runtime string) string {
	return opts.RepoPrefix + runtime
}

// redactedRepoURL returns the same value with any embedded token replaced
// by "***". Use this for log lines.
func redactedRepoURL(opts *LocalBuildOptions, runtime string) string {
	return maskTokenInURL(repoURL(opts, runtime))
}

// maskTokenInURL replaces userinfo (username:password@) in a URL with
// `***@` so log lines never echo a Gitea PAT. Returns the input as-is
// on parse failures (defence: never silently corrupt the visible URL).
//
// Implementation note: net/url's URL.User stringifier percent-encodes
// the username, so `u.User = url.User("***"); u.String()` would yield
// `https://%2A%2A%2A@host/...` — unhelpful for humans grepping logs.
// We drop the userinfo via URL.User=nil, get the canonical scheme-and-
// rest, and re-insert the literal `***@` between the scheme separator
// and the host.
func maskTokenInURL(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.User == nil {
		return s
	}
	u.User = nil
	out := u.String()
	prefix := u.Scheme + "://"
	if !strings.HasPrefix(out, prefix) {
		return s
	}
	return prefix + "***@" + out[len(prefix):]
}

// remoteHeadShaProd looks up the HEAD commit sha of branch `main` for
// the workspace-template-<runtime> repo on Gitea. We use the Gitea API
// (a single HTTPS call) rather than `git ls-remote` so we don't need a
// git binary just for the HEAD lookup — we still need git for the
// clone, but the cache-hit path stays git-free.
func remoteHeadShaProd(ctx context.Context, opts *LocalBuildOptions, runtime string) (string, error) {
	// Convert a `git.example.com/org/prefix-` URL into the API form
	// `git.example.com/api/v1/repos/org/prefix-<runtime>/branches/main`.
	// Works for both git.moleculesai.app (default) and any forks that
	// share the Gitea API shape.
	apiURL, err := giteaBranchAPIURL(opts.RepoPrefix, runtime, "main")
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return "", err
	}
	if opts.Token != "" {
		// Gitea accepts "token <PAT>" in the Authorization header for
		// API calls. Userinfo is also accepted but only matters for
		// the HTTPS clone, not the JSON API.
		req.Header.Set("Authorization", "token "+opts.Token)
	}
	cli := opts.HTTPClient
	if cli == nil {
		cli = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := cli.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("repo not found at %s — runtime %q may not be mirrored to Gitea (only claude-code/hermes/langgraph/autogen today)", apiURL, runtime)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", fmt.Errorf("auth failure (%d) at %s — verify MOLECULE_GITEA_TOKEN if private repo", resp.StatusCode, apiURL)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HEAD lookup at %s returned %d", apiURL, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", fmt.Errorf("read HEAD response body: %w", err)
	}
	// Tiny ad-hoc parser: we want commit.id, no need to drag in encoding/json
	// — actually simpler to use json. Switch to it.
	return parseGiteaBranchHeadSha(body)
}

// giteaBranchAPIURL maps a repo-prefix URL like
// `https://git.moleculesai.app/molecule-ai/molecule-ai-workspace-template-`
// + runtime "claude-code" + branch "main"
// to the API URL
// `https://git.moleculesai.app/api/v1/repos/molecule-ai/molecule-ai-workspace-template-claude-code/branches/main`.
func giteaBranchAPIURL(repoPrefix, runtime, branch string) (string, error) {
	u, err := url.Parse(repoPrefix + runtime)
	if err != nil {
		return "", fmt.Errorf("parse repo URL %q: %w", repoPrefix+runtime, err)
	}
	parts := strings.TrimPrefix(u.Path, "/")
	parts = strings.TrimSuffix(parts, "/")
	if parts == "" {
		return "", fmt.Errorf("repo URL %q has empty path", repoPrefix+runtime)
	}
	// Expect `<org>/<repo>` (single slash) — the prefix already includes
	// org+partial-repo; runtime appends the rest.
	if !strings.Contains(parts, "/") {
		return "", fmt.Errorf("repo URL %q missing org/repo path", repoPrefix+runtime)
	}
	apiURL := url.URL{
		Scheme: u.Scheme,
		Host:   u.Host,
		Path:   "/api/v1/repos/" + parts + "/branches/" + branch,
	}
	return apiURL.String(), nil
}

// parseGiteaBranchHeadSha extracts commit.id from the Gitea
// /branches/<name> response. We use a permissive substring scan so a
// missing-key in the JSON gives a clear error rather than the
// json.Decoder's somewhat opaque "missing field" message.
func parseGiteaBranchHeadSha(body []byte) (string, error) {
	// Look for `"id":"<40-hex>"` inside the commit object.
	idx := strings.Index(string(body), `"id":"`)
	if idx < 0 {
		return "", errors.New("Gitea branch response missing commit.id field")
	}
	rest := string(body[idx+len(`"id":"`):])
	end := strings.IndexByte(rest, '"')
	if end < 0 {
		return "", errors.New("Gitea branch response has malformed commit.id (no closing quote)")
	}
	sha := rest[:end]
	if len(sha) < 7 {
		return "", fmt.Errorf("Gitea returned suspiciously short sha %q", sha)
	}
	return sha, nil
}

// gitCloneProd shallow-clones the runtime's template repo into dest.
//
// We invoke `git` rather than implementing the protocol ourselves —
// every host that runs the workspace-server already needs git available
// (it's a hard dep of go-mod for vendored repos) and the OSS contributor
// onboarding doc lists it as a prerequisite.
func gitCloneProd(ctx context.Context, opts *LocalBuildOptions, runtime, dest string) error {
	cloneURL := repoURL(opts, runtime)
	if opts.Token != "" {
		// HTTPS clone with userinfo: https://oauth2:<token>@host/...
		u, err := url.Parse(cloneURL)
		if err == nil {
			u.User = url.UserPassword("oauth2", opts.Token)
			cloneURL = u.String()
		}
		// On parse failure we silently fall through to the public URL —
		// better to attempt the anonymous clone than to refuse outright.
	}
	cmd := exec.CommandContext(ctx, "git", "clone", "--depth=1", "--branch=main", "--single-branch", cloneURL, dest)
	// Drop git's askpass prompts so we fail-fast on auth errors instead
	// of hanging waiting for an interactive password.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/bin/echo")
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Mask the token in any error string git emits via stderr — git
		// occasionally echoes the URL verbatim on failure.
		errMsg := maskTokenInString(string(out), opts.Token)
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(errMsg))
	}
	return nil
}

// maskTokenInString replaces literal occurrences of the token with `***`.
// Defence against git binary or docker echoing the URL into stderr.
func maskTokenInString(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "***")
}

// dockerBuildProd invokes the docker CLI to build the workspace-template
// image. We shell out rather than use the Docker SDK's ImageBuild — the
// SDK requires hand-tarballing the build context, which adds a
// non-trivial code path with its own bug surface. The docker CLI is
// already a hard dep of the workspace-server (the provisioner needs the
// daemon), so requiring the CLI binary on PATH adds nothing.
//
// Uses the legacy `docker build` (not `docker buildx build`) because
// buildx isn't always installed by default on Linux distros and the
// legacy builder produces an image the local Docker daemon picks up
// automatically. We pass --platform=linux/amd64 directly; with Docker
// 20.10+ this works without buildx because the legacy builder
// auto-promotes to BuildKit when available, falling back to v1
// otherwise (still produces an amd64 image via QEMU).
func dockerBuildProd(ctx context.Context, opts *LocalBuildOptions, contextDir, tag string) error {
	args := []string{"build"}
	if opts.Platform != "" {
		args = append(args, "--platform="+opts.Platform)
	}
	args = append(args,
		"-t", tag,
		"-f", filepath.Join(contextDir, "Dockerfile"),
		contextDir,
	)
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = append(os.Environ(), "DOCKER_BUILDKIT=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Sanitize defensive — docker build output shouldn't contain a
		// token, but maskTokenInString is a no-op when token is empty.
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(maskTokenInString(string(out), opts.Token)))
	}
	return nil
}

// dockerHasTagProd returns true iff the given tag exists in the local
// image store. Used as the fast cache-hit check.
func dockerHasTagProd(ctx context.Context, tag string) (bool, error) {
	cmd := exec.CommandContext(ctx, "docker", "image", "inspect", "--format={{.Id}}", tag)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return strings.TrimSpace(string(out)) != "", nil
	}
	// `docker image inspect` exits 1 with "Error: No such image" when
	// missing — that's a definitive false, not an error condition.
	low := strings.ToLower(string(out))
	if strings.Contains(low, "no such image") || strings.Contains(low, "not found") {
		return false, nil
	}
	return false, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
}

// dockerTagProd creates an alias from src → dst. Used to refresh the
// floating `:latest` after a build or cache hit.
func dockerTagProd(ctx context.Context, src, dst string) error {
	cmd := exec.CommandContext(ctx, "docker", "tag", src, dst)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// CacheKey is exposed for diagnostic logs / tests so the cache-key shape
// is documented in code rather than only as a string format.
//
//	cache_key = sha256(runtime || head_sha || repoPrefix)[:16]
//
// Today only the SHA is consumed, but the helper is kept for future
// extensions (e.g. include Dockerfile-content-hash to invalidate when
// only the Dockerfile changes between two runs targeting the same SHA).
func CacheKey(runtime, sha, repoPrefix string) string {
	h := sha256.Sum256([]byte(runtime + "|" + sha + "|" + repoPrefix))
	return hex.EncodeToString(h[:8])
}
