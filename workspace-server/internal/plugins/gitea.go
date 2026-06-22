package plugins

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// GiteaResolver fetches plugins from a (typically private) Gitea repository.
// It uses the Gitea archive API for HTTP(S) remotes and falls back to a
// shallow git clone only for local file:// test repos. The archive path is
// fast-fail: private or missing repos return a clear error within seconds
// instead of hanging on a credential prompt.
//
// Source-contract string (the value a template puts in `plugins:`):
//
//	gitea://<owner>/<repo>[/<subpath...>]#<ref>
//
// Examples:
//
//	gitea://molecule-ai/molecule-ai-workspace-template-seo-agent/agent-skills/seo-all#main
//	gitea://molecule-ai/some-repo#v1.2.0          (whole-repo, no subpath)
//	gitea://molecule-ai/some-repo/plugins/foo#sha:abc123
//
// Parsing: the FIRST two path segments are <owner>/<repo>; everything after
// (up to the optional `#<ref>`) is the in-repo subpath. The resolved plugin
// name is the last subpath segment (or <repo> when no subpath is given).
//
// Authentication: private Gitea repos need a PAT. The resolver reads the
// token from the environment (MOLECULE_TEMPLATE_REPO_TOKEN by default — the
// same read-only Gitea PAT CP PR#850 already places on every tenant box).
// The token is sent in an Authorization header for API calls and is never
// logged or returned to clients.
//
// Pinned-ref enforcement mirrors GithubResolver: an unpinned spec (no
// `#<ref>`) is rejected unless PLUGIN_ALLOW_UNPINNED=true.
type GiteaResolver struct {
	// GitRunner runs git commands for file:// / local test remotes.
	// Defaults to defaultGitRunner. Unused for HTTP(S) archive fetches.
	GitRunner func(ctx context.Context, dir string, args ...string) error

	// ArchiveDownloader downloads and extracts a Gitea archive tarball for
	// HTTP(S) remotes. Defaults to defaultArchiveDownloader. Overridable in
	// tests to simulate private-repo 401/403/404 responses.
	ArchiveDownloader func(ctx context.Context, archiveURL, token, dstDir string) error

	// ResolveRefClient optionally overrides the HTTP client used by
	// ResolveRef to fetch the commit SHA via the Gitea API. Defaults to
	// http.DefaultClient. Overridable in tests.
	ResolveRefClient *http.Client

	// BaseURL is the Gitea instance origin, e.g. "https://git.moleculesai.app".
	// Tests point it at a local file:// bare repo (in which case TokenEnv is
	// ignored — file:// has no userinfo auth).
	BaseURL string

	// TokenEnv is the environment variable the PAT is read from at Fetch
	// time. Read lazily (not at construction) so a token rotated into the
	// process env after startup is picked up. Empty disables auth injection
	// (anonymous API calls — works for public repos; fails fast on private).
	TokenEnv string

	// FetchTimeout bounds the archive download + SHA resolution for HTTP(S)
	// remotes. Defaults to 30 seconds. Overridable in tests.
	FetchTimeout time.Duration

	// LastFetchSHA holds the commit SHA checked out by the last successful
	// Fetch. Mirrors GithubResolver so the install pipeline's drift-seed
	// type-switch can pick it up. Reset on each Fetch.
	LastFetchSHA string
}

// NewGiteaResolver constructs a resolver with platform defaults: the
// canonical Gitea origin and the MOLECULE_TEMPLATE_REPO_TOKEN PAT env.
func NewGiteaResolver() *GiteaResolver {
	base := os.Getenv("MOLECULE_GITEA_BASE_URL")
	if base == "" {
		base = "https://git.moleculesai.app"
	}
	return &GiteaResolver{
		GitRunner:         defaultGitRunner,
		ArchiveDownloader: defaultArchiveDownloader,
		BaseURL:           base,
		TokenEnv:          "MOLECULE_TEMPLATE_REPO_TOKEN",
	}
}

// Scheme returns "gitea".
func (r *GiteaResolver) Scheme() string { return "gitea" }

// LastSHA returns the SHA of the last successful Fetch, or "" if Fetch has
// not been called or the last call failed.
func (r *GiteaResolver) LastSHA() string { return r.LastFetchSHA }

// giteaPathRE constrains owner / repo segments. Same grammar as the GitHub
// resolver's owner/repo: start alphanumeric, then [a-zA-Z0-9_.-].
var giteaPathSegRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.\-]{0,99}$`)

// giteaSubpathSegRE constrains each subpath segment. Disallows "..", leading
// "-" (flag injection), and shell metacharacters — only path-safe names.
var giteaSubpathSegRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.\-]{0,127}$`)

// giteaRefRE mirrors GithubResolver's ref grammar: must not start with "-",
// then a bounded set of ref-safe characters.
var giteaRefRE = regexp.MustCompile(`^[a-zA-Z0-9_.][a-zA-Z0-9_./:\-]{0,254}$`)

// parsedGiteaSpec is the decomposition of a gitea spec body.
type parsedGiteaSpec struct {
	owner   string
	repo    string
	subpath string // cleaned, relative, slash-joined; "" when whole-repo
	ref     string // "" when unpinned
}

// parseGiteaSpec decomposes "<owner>/<repo>[/<subpath...>][#<ref>]".
func parseGiteaSpec(spec string) (parsedGiteaSpec, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return parsedGiteaSpec{}, fmt.Errorf("gitea resolver: empty spec")
	}

	var ref string
	if i := strings.Index(spec, "#"); i >= 0 {
		ref = strings.TrimSpace(spec[i+1:])
		spec = spec[:i]
		if ref != "" && !giteaRefRE.MatchString(ref) {
			return parsedGiteaSpec{}, fmt.Errorf("gitea resolver: invalid ref %q", ref)
		}
	}

	segs := strings.Split(strings.Trim(spec, "/"), "/")
	if len(segs) < 2 {
		return parsedGiteaSpec{}, fmt.Errorf(
			"gitea resolver: spec %q must be <owner>/<repo>[/<subpath>][#<ref>]", spec)
	}
	owner, repo := segs[0], segs[1]
	if !giteaPathSegRE.MatchString(owner) || !giteaPathSegRE.MatchString(repo) {
		return parsedGiteaSpec{}, fmt.Errorf("gitea resolver: invalid owner/repo in %q", spec)
	}

	subSegs := segs[2:]
	for _, s := range subSegs {
		if s == "" || s == "." || s == ".." || !giteaSubpathSegRE.MatchString(s) {
			return parsedGiteaSpec{}, fmt.Errorf("gitea resolver: invalid subpath segment %q", s)
		}
	}
	subpath := strings.Join(subSegs, "/")

	return parsedGiteaSpec{owner: owner, repo: repo, subpath: subpath, ref: ref}, nil
}

// cloneURL builds the authenticated clone URL. The PAT (if present) is
// injected into the userinfo component. file:// base URLs are returned as-is
// (no userinfo — local test repos).
func (r *GiteaResolver) cloneURL(owner, repo string) (string, error) {
	base := r.BaseURL
	if base == "" {
		base = "https://git.moleculesai.app"
	}
	path := fmt.Sprintf("/%s/%s.git", owner, repo)

	if strings.HasPrefix(base, "file://") || strings.HasPrefix(base, "/") {
		// Local test repo: BaseURL points at a directory of bare repos.
		return strings.TrimSuffix(base, "/") + path, nil
	}

	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("gitea resolver: invalid base url: %w", err)
	}
	if r.TokenEnv != "" {
		if tok := strings.TrimSpace(os.Getenv(r.TokenEnv)); tok != "" {
			// Gitea accepts the PAT as the username with an empty (or any)
			// password over HTTPS basic auth. url.UserPassword URL-encodes
			// the credential so a token with reserved chars stays valid.
			u.User = url.UserPassword(tok, "x-oauth-basic")
		}
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + path
	return u.String(), nil
}

// archiveRef returns a ref string suitable for the Gitea archive API.
// It strips the "tag:" and "sha:" prefixes used in plugin specs.
func archiveRef(ref string) string {
	switch {
	case strings.HasPrefix(ref, "tag:"):
		return strings.TrimPrefix(ref, "tag:")
	case strings.HasPrefix(ref, "sha:"):
		return strings.TrimPrefix(ref, "sha:")
	}
	return ref
}

// defaultArchiveDownloader downloads a Gitea archive tarball to dstDir and
// extracts it. It is fail-closed and token-safe: the token is sent in the
// Authorization header, never logged, and never surfaced in error messages.
func defaultArchiveDownloader(ctx context.Context, archiveURL, token, dstDir string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, archiveURL, nil)
	if err != nil {
		return fmt.Errorf("gitea resolver: build archive request: %w", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "token "+token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("gitea resolver: archive download timed out")
		}
		return fmt.Errorf("gitea resolver: archive download failed: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		// proceed
	case http.StatusNotFound:
		return fmt.Errorf("gitea resolver: %s: %w", archiveURL, ErrPluginNotFound)
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("gitea resolver: %s: repository not accessible (HTTP %d)", archiveURL, resp.StatusCode)
	default:
		return fmt.Errorf("gitea resolver: %s: unexpected HTTP %d", archiveURL, resp.StatusCode)
	}

	gr, err := gzip.NewReader(resp.Body)
	if err != nil {
		return fmt.Errorf("gitea resolver: gzip archive: %w", err)
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("gitea resolver: read tar archive: %w", err)
		}

		// Clean the header name and reject traversal / absolute paths.
		rel := filepath.Clean(hdr.Name)
		if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			continue
		}
		target := filepath.Join(dstDir, rel)
		cleanTarget, err := filepath.Abs(target)
		if err != nil {
			return fmt.Errorf("gitea resolver: abs target: %w", err)
		}
		cleanDst, err := filepath.Abs(dstDir)
		if err != nil {
			return fmt.Errorf("gitea resolver: abs dst: %w", err)
		}
		if !strings.HasPrefix(cleanTarget, cleanDst+string(filepath.Separator)) {
			continue // tar entry escapes extraction root — skip
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode&0o777)); err != nil {
				return fmt.Errorf("gitea resolver: mkdir %s: %w", target, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("gitea resolver: mkdir %s: %w", filepath.Dir(target), err)
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode&0o777))
			if err != nil {
				return fmt.Errorf("gitea resolver: create %s: %w", target, err)
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return fmt.Errorf("gitea resolver: write %s: %w", target, err)
			}
			f.Close()
		default:
			// Skip symlinks, devices, etc. copyTree also skips symlinks.
		}
	}
	return nil
}

// repoRootFromArchive picks the single top-level directory inside an
// extracted Gitea archive. Gitea archives contain exactly one root directory
// named after the repo.
func repoRootFromArchive(archiveDir string) (string, error) {
	entries, err := os.ReadDir(archiveDir)
	if err != nil {
		return "", fmt.Errorf("gitea resolver: read archive dir: %w", err)
	}
	var dirs []os.DirEntry
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e)
		}
	}
	if len(dirs) != 1 {
		return "", fmt.Errorf("gitea resolver: expected exactly one root dir in archive, found %d", len(dirs))
	}
	return filepath.Join(archiveDir, dirs[0].Name()), nil
}

// Fetch resolves the repo at the pinned ref and copies the (optional) subpath
// into dst. Returns the resolved plugin name.
func (r *GiteaResolver) Fetch(ctx context.Context, spec string, dst string) (string, error) {
	p, err := parseGiteaSpec(spec)
	if err != nil {
		return "", err
	}

	if p.ref == "" && os.Getenv("PLUGIN_ALLOW_UNPINNED") != "true" {
		return "", fmt.Errorf("gitea resolver: spec %q requires a pinned ref "+
			"(e.g. %s/%s#main or #v1.0.0); set PLUGIN_ALLOW_UNPINNED=true for local dev",
			spec, p.owner, p.repo)
	}

	base := r.BaseURL
	if base == "" {
		base = "https://git.moleculesai.app"
	}

	// Local file:// remotes (tests) keep the git-clone path.
	if strings.HasPrefix(base, "file://") || strings.HasPrefix(base, "/") {
		return r.fetchGit(ctx, p, dst)
	}

	return r.fetchArchive(ctx, p, dst, base)
}

// fetchGit is the legacy local-file:// path used only by tests. It preserves
// the original shallow-clone behavior so real-git test fixtures keep working.
func (r *GiteaResolver) fetchGit(ctx context.Context, p parsedGiteaSpec, dst string) (string, error) {
	runner := r.GitRunner
	if runner == nil {
		runner = defaultGitRunner
	}

	cloneURL, err := r.cloneURL(p.owner, p.repo)
	if err != nil {
		return "", err
	}

	workDir, err := os.MkdirTemp("", "molecule-gitea-clone-*")
	if err != nil {
		return "", fmt.Errorf("gitea resolver: tempdir: %w", err)
	}
	defer os.RemoveAll(workDir)

	cloneTarget := filepath.Join(workDir, "repo")
	args := []string{"clone", "--depth=1"}
	if p.ref != "" {
		args = append(args, "--branch", p.ref)
	}
	args = append(args, "--", cloneURL, cloneTarget)
	if err := runner(ctx, workDir, args...); err != nil {
		safeURL := fmt.Sprintf("%s/%s/%s.git", r.BaseURL, p.owner, p.repo)
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "repository not found") ||
			strings.Contains(msg, "could not find remote branch") ||
			(strings.Contains(msg, "remote branch") && strings.Contains(msg, "not found")) ||
			strings.Contains(msg, "not found") && strings.Contains(msg, "did you run git update-server-info") {
			return "", fmt.Errorf("gitea resolver: %s: %w", safeURL, ErrPluginNotFound)
		}
		return "", fmt.Errorf("gitea resolver: clone %s failed: %w", safeURL, err)
	}

	if shaOut, shaErr := runGitOneLine(ctx, cloneTarget, "rev-parse", "--verify", "HEAD"); shaErr == nil {
		r.LastFetchSHA = strings.TrimSpace(shaOut)
	}

	return r.stageTree(ctx, p, cloneTarget, dst)
}

// fetchArchive downloads the repo via the authenticated Gitea archive API,
// extracts it, and stages the requested subpath. It is bounded by a strict
// timeout and maps 401/403/404 to clear, token-safe errors.
func (r *GiteaResolver) fetchArchive(ctx context.Context, p parsedGiteaSpec, dst, base string) (string, error) {
	token := ""
	if r.TokenEnv != "" {
		token = strings.TrimSpace(os.Getenv(r.TokenEnv))
	}

	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("gitea resolver: invalid base url: %w", err)
	}
	ref := archiveRef(p.ref)
	u.Path = fmt.Sprintf("/api/v1/repos/%s/%s/archive/%s.tar.gz", p.owner, p.repo, ref)
	archiveURL := u.String()

	workDir, err := os.MkdirTemp("", "molecule-gitea-archive-*")
	if err != nil {
		return "", fmt.Errorf("gitea resolver: tempdir: %w", err)
	}
	defer os.RemoveAll(workDir)

	archiveDir := filepath.Join(workDir, "extracted")
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		return "", fmt.Errorf("gitea resolver: mkdir archive dir: %w", err)
	}

	downloader := r.ArchiveDownloader
	if downloader == nil {
		downloader = defaultArchiveDownloader
	}

	// Bounded timeout: a private or unreachable repo must fail fast instead
	// of hanging the install request until the gateway gives up (~100 s → 502).
	timeout := r.FetchTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	fetchCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if err := downloader(fetchCtx, archiveURL, token, archiveDir); err != nil {
		return "", err
	}

	cloneTarget, err := repoRootFromArchive(archiveDir)
	if err != nil {
		return "", err
	}

	// Resolve the installed SHA via the Gitea API. Same timeout so a missing
	// or private repo fails fast here too.
	sha, err := r.resolveSHA(fetchCtx, p.owner, p.repo, ref, token, base)
	if err != nil {
		return "", err
	}
	if sha != "" {
		r.LastFetchSHA = sha
	}

	return r.stageTree(ctx, p, cloneTarget, dst)
}

// stageTree copies the repo root (or subpath) into dst and derives the plugin
// name. Shared by fetchGit and fetchArchive.
func (r *GiteaResolver) stageTree(ctx context.Context, p parsedGiteaSpec, cloneTarget, dst string) (string, error) {
	srcTree := cloneTarget
	pluginName := p.repo
	if p.subpath != "" {
		joined := filepath.Join(cloneTarget, filepath.FromSlash(p.subpath))
		relCheck, relErr := filepath.Rel(cloneTarget, joined)
		if relErr != nil || strings.HasPrefix(relCheck, "..") {
			return "", fmt.Errorf("gitea resolver: subpath %q escapes repo", p.subpath)
		}
		info, statErr := os.Stat(joined)
		if statErr != nil {
			if os.IsNotExist(statErr) {
				return "", fmt.Errorf("gitea resolver: subpath %q in %s/%s: %w",
					p.subpath, p.owner, p.repo, ErrPluginNotFound)
			}
			return "", fmt.Errorf("gitea resolver: stat subpath: %w", statErr)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("gitea resolver: subpath %q is not a directory", p.subpath)
		}
		srcTree = joined
		parts := strings.Split(p.subpath, "/")
		pluginName = parts[len(parts)-1]
	} else {
		if err := os.RemoveAll(filepath.Join(cloneTarget, ".git")); err != nil {
			return "", fmt.Errorf("gitea resolver: remove .git: %w", err)
		}
	}

	if err := copyTree(ctx, srcTree, dst); err != nil {
		return "", fmt.Errorf("gitea resolver: copy to dst: %w", err)
	}

	return pluginName, nil
}

// resolveSHA fetches the commit SHA for a ref via the Gitea API. It is used
// by both Fetch (to populate LastFetchSHA) and ResolveRef. The ref argument
// must already be archiveRef-normalized.
func (r *GiteaResolver) resolveSHA(ctx context.Context, owner, repo, ref, token, base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("gitea resolver: invalid base url: %w", err)
	}
	u.Path = fmt.Sprintf("/api/v1/repos/%s/%s/commits", owner, repo)
	q := u.Query()
	q.Set("sha", ref)
	q.Set("limit", "1")
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", fmt.Errorf("gitea resolver: build commits request: %w", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "token "+token)
	}

	client := r.ResolveRefClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("gitea resolver: resolve SHA timed out")
		}
		return "", fmt.Errorf("gitea resolver: resolve SHA request failed: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return "", fmt.Errorf("gitea resolver: %s/%s ref %q: %w", owner, repo, ref, ErrPluginNotFound)
	case http.StatusUnauthorized, http.StatusForbidden:
		return "", fmt.Errorf("gitea resolver: %s/%s ref %q: not accessible (HTTP %d)", owner, repo, ref, resp.StatusCode)
	default:
		return "", fmt.Errorf("gitea resolver: %s/%s ref %q: unexpected HTTP %d", owner, repo, ref, resp.StatusCode)
	}

	var commits []struct {
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&commits); err != nil {
		return "", fmt.Errorf("gitea resolver: decode commits: %w", err)
	}
	if len(commits) == 0 || commits[0].SHA == "" {
		return "", fmt.Errorf("gitea resolver: %s/%s ref %q: no commit returned", owner, repo, ref)
	}
	return strings.TrimSpace(commits[0].SHA), nil
}

// ResolveRef resolves a gitea spec's ref to a full commit SHA, so the drift
// sweeper can compare installed vs upstream for gitea:// sources too.
//
// Accepts the same ref shapes as GithubResolver.ResolveRef:
//
//	"<owner>/<repo>[/<subpath>]#tag:vX.Y.Z"
//	"<owner>/<repo>[/<subpath>]#sha:<full>"
//	"<owner>/<repo>[/<subpath>]#<branch>"
func (r *GiteaResolver) ResolveRef(ctx context.Context, spec string) (string, error) {
	p, err := parseGiteaSpec(spec)
	if err != nil {
		return "", err
	}
	if p.ref == "" {
		return "", fmt.Errorf("gitea resolver: ResolveRef requires a ref (got bare %q)", spec)
	}

	base := r.BaseURL
	if base == "" {
		base = "https://git.moleculesai.app"
	}
	ref := archiveRef(p.ref)

	// Local file:// remotes (tests) keep the git-fetch path.
	if strings.HasPrefix(base, "file://") || strings.HasPrefix(base, "/") {
		return r.resolveRefGit(ctx, p, ref)
	}

	token := ""
	if r.TokenEnv != "" {
		token = strings.TrimSpace(os.Getenv(r.TokenEnv))
	}

	resolveCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	return r.resolveSHA(resolveCtx, p.owner, p.repo, ref, token, base)
}

// resolveRefGit is the legacy local-file:// path used only by tests.
func (r *GiteaResolver) resolveRefGit(ctx context.Context, p parsedGiteaSpec, fetchRef string) (string, error) {
	runner := r.GitRunner
	if runner == nil {
		runner = defaultGitRunner
	}
	cloneURL, err := r.cloneURL(p.owner, p.repo)
	if err != nil {
		return "", err
	}

	workDir, err := os.MkdirTemp("", "molecule-gitea-resolve-*")
	if err != nil {
		return "", fmt.Errorf("gitea resolver: tempdir: %w", err)
	}
	defer os.RemoveAll(workDir)

	if err := runner(ctx, workDir, "init", "-q"); err != nil {
		return "", fmt.Errorf("gitea resolver: git init: %w", err)
	}
	if err := runner(ctx, workDir, "fetch", "--depth=1", "--", cloneURL, fetchRef); err != nil {
		safeURL := fmt.Sprintf("%s/%s/%s.git", r.BaseURL, p.owner, p.repo)
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "repository not found") ||
			strings.Contains(msg, "couldn't find remote ref") ||
			strings.Contains(msg, "could not find remote ref") {
			return "", fmt.Errorf("gitea resolver: %s ref %q: %w", safeURL, fetchRef, ErrPluginNotFound)
		}
		return "", fmt.Errorf("gitea resolver: fetch %s %s failed: %w", safeURL, fetchRef, err)
	}

	sha, err := runGitOneLine(ctx, workDir, "rev-parse", "--verify", "FETCH_HEAD")
	if err != nil {
		return "", fmt.Errorf("gitea resolver: rev-parse FETCH_HEAD: %w", err)
	}
	return strings.TrimSpace(sha), nil
}
