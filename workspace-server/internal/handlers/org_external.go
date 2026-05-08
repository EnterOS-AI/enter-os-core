package handlers

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// External-ref resolver — gitops-style cross-repo subtree composition.
// Internal#77 RFC, Phase 3a (task #222). Prior art: Helm subcharts +
// dependency cache, Kustomize remote bases, Terraform module sources.
//
// Schema (a `!external`-tagged mapping anywhere a workspace entry is
// allowed — workspaces:, roots:, children:):
//
//   - !external
//     repo: molecule-ai/molecule-dev-department
//     ref: main
//     path: dev-lead/workspace.yaml
//
// At resolve time, the platform fetches the repo at ref into a content-
// addressable cache under <rootDir>/.external-cache/<repo>/<sha>/, loads
// the yaml at <cacheDir>/<path>, rewrites every files_dir + relative
// !include path to be cache-prefixed, then grafts the result in place of
// the !external node. Downstream pipeline (resolveInsideRoot, plugin
// merge, CopyTemplateToContainer) sees ordinary in-tree paths.

// ExternalRef is the deserialized form of an `!external`-tagged mapping.
type ExternalRef struct {
	Repo string `yaml:"repo"`
	Ref  string `yaml:"ref"`
	Path string `yaml:"path"`

	// URL overrides the default Gitea host. Optional; defaults to
	// MOLECULE_EXTERNAL_GITEA_URL env or git.moleculesai.app.
	URL string `yaml:"url,omitempty"`
}

const (
	// maxExternalDepth caps recursion through nested `!external`s. Lower
	// than maxIncludeDepth (16) because each level may issue a network
	// fetch. Composition that genuinely needs >4 layers is a smell.
	maxExternalDepth = 4

	// externalCacheDirName is the per-template cache subdir under rootDir.
	// Content-addressable: keyed by (repo, sha). Operators add this to
	// .gitignore — cache is platform-mutated, not source-tracked.
	externalCacheDirName = ".external-cache"

	// gitFetchTimeout caps a single clone operation. Conservative —
	// org template fetches are typically <100KB.
	gitFetchTimeout = 60 * time.Second
)

// safeRefPattern restricts `ref` values to characters git itself accepts
// for branch / tag / SHA. Belt-and-braces over git's own validation.
var safeRefPattern = regexp.MustCompile(`^[a-zA-Z0-9_./-]+$`)

// allowlistedHostPath returns true if `<host>/<repo>` matches the
// configured allowlist. Default allowlist: git.moleculesai.app/molecule-ai/.
// Override via MOLECULE_EXTERNAL_REPO_ALLOWLIST env var (comma-separated
// patterns). Patterns are matched as prefixes (with trailing-slash
// semantics) or as exact matches. Trailing /* is treated as "any
// descendants of this prefix".
//
// Examples:
//   - "git.moleculesai.app/molecule-ai/" → matches molecule-ai/* (any repo)
//   - "git.moleculesai.app/molecule-ai/*" → same; trailing /* normalized to /
//   - "git.moleculesai.app/molecule-ai/molecule-dev-department" → exact
//   - "git.moleculesai.app/" → matches everything on that host
func allowlistedHostPath(host, repoPath string) bool {
	allow := os.Getenv("MOLECULE_EXTERNAL_REPO_ALLOWLIST")
	if allow == "" {
		allow = "git.moleculesai.app/molecule-ai/"
	}
	hp := host + "/" + repoPath
	for _, pat := range strings.Split(allow, ",") {
		pat = strings.TrimSpace(pat)
		if pat == "" {
			continue
		}
		// Normalize trailing /* → /
		pat = strings.TrimSuffix(pat, "*")
		if pat == hp {
			return true
		}
		if strings.HasSuffix(pat, "/") && strings.HasPrefix(hp+"/", pat) {
			return true
		}
	}
	return false
}

// externalFetcher abstracts the git-clone-into-cache step. Production
// uses gitFetcher (shells out to git); tests inject a fake that
// pre-stages content in a temp dir.
type externalFetcher interface {
	// Fetch ensures rootDir/.external-cache/<safe-repo>/<sha>/ contains
	// the repo content at the given ref. Returns the absolute cache
	// dir + the resolved SHA. Cache hit = no network. Cache miss =
	// clone.
	Fetch(ctx context.Context, rootDir, host, repoPath, ref string) (cacheDir, sha string, err error)
}

// defaultExternalFetcher is the package-level fetcher injection point.
// Production code uses the git-shell fetcher; tests override via
// SetExternalFetcherForTest.
var defaultExternalFetcher externalFetcher = &gitFetcher{}

// SetExternalFetcherForTest swaps the fetcher for testing. Returns a
// cleanup func that restores the previous fetcher.
func SetExternalFetcherForTest(f externalFetcher) func() {
	prev := defaultExternalFetcher
	defaultExternalFetcher = f
	return func() { defaultExternalFetcher = prev }
}

// resolveExternalMapping replaces an `!external`-tagged mapping node
// with the loaded + path-rewritten yaml content from the fetched repo.
//
// `currentDir` and `rootDir` are inherited from expandNode's resolve
// frame. `visited` tracks (repo, sha, path) tuples for cycle detection
// across nested externals.
func resolveExternalMapping(n *yaml.Node, currentDir, rootDir string, visited map[string]bool, depth int) error {
	if depth > maxExternalDepth {
		return fmt.Errorf("!external: max depth %d exceeded (possible cycle)", maxExternalDepth)
	}
	if rootDir == "" {
		return fmt.Errorf("!external at line %d requires a dir-based org template (no rootDir in inline-template mode)", n.Line)
	}

	var ref ExternalRef
	if err := n.Decode(&ref); err != nil {
		return fmt.Errorf("!external at line %d: decode: %w", n.Line, err)
	}
	if ref.Repo == "" || ref.Ref == "" || ref.Path == "" {
		return fmt.Errorf("!external at line %d: repo, ref, path are all required (got %+v)", n.Line, ref)
	}
	if !safeRefPattern.MatchString(ref.Ref) {
		return fmt.Errorf("!external at line %d: ref %q contains disallowed characters", n.Line, ref.Ref)
	}
	// Defense-in-depth: even though git itself rejects refs containing
	// `..`, the regex above currently allows them. Reject explicitly.
	if strings.Contains(ref.Ref, "..") {
		return fmt.Errorf("!external at line %d: ref %q must not contain '..'", n.Line, ref.Ref)
	}
	if strings.Contains(ref.Path, "..") || strings.HasPrefix(ref.Path, "/") {
		return fmt.Errorf("!external at line %d: path %q must be relative-and-down-only", n.Line, ref.Path)
	}

	host := ref.URL
	if host == "" {
		host = os.Getenv("MOLECULE_EXTERNAL_GITEA_URL")
	}
	if host == "" {
		host = "git.moleculesai.app"
	}
	host = strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://")
	host = strings.TrimSuffix(host, "/")

	if !allowlistedHostPath(host, ref.Repo) {
		return fmt.Errorf("!external at line %d: %s/%s not in MOLECULE_EXTERNAL_REPO_ALLOWLIST", n.Line, host, ref.Repo)
	}

	ctx, cancel := context.WithTimeout(context.Background(), gitFetchTimeout)
	defer cancel()

	cacheDir, sha, err := defaultExternalFetcher.Fetch(ctx, rootDir, host, ref.Repo, ref.Ref)
	if err != nil {
		return fmt.Errorf("!external at line %d: fetch %s/%s@%s: %w", n.Line, host, ref.Repo, ref.Ref, err)
	}

	// Cycle key: (repo, sha, path) — same external content reachable
	// via two paths is fine, but a self-referential cycle isn't.
	cycleKey := fmt.Sprintf("%s/%s@%s/%s", host, ref.Repo, sha, ref.Path)
	if visited[cycleKey] {
		return fmt.Errorf("!external cycle detected at %q (line %d)", cycleKey, n.Line)
	}

	// Validate path resolves inside the cache dir (anti-traversal).
	yamlPathAbs, err := resolveInsideRoot(cacheDir, ref.Path)
	if err != nil {
		return fmt.Errorf("!external at line %d: path %q: %w", n.Line, ref.Path, err)
	}
	if _, err := os.Stat(yamlPathAbs); err != nil {
		return fmt.Errorf("!external at line %d: %s/%s@%s does not contain %q: %w", n.Line, host, ref.Repo, sha, ref.Path, err)
	}

	data, err := os.ReadFile(yamlPathAbs)
	if err != nil {
		return fmt.Errorf("!external at line %d: read %q: %w", n.Line, yamlPathAbs, err)
	}

	var sub yaml.Node
	if err := yaml.Unmarshal(data, &sub); err != nil {
		return fmt.Errorf("!external at line %d: parse %q: %w", n.Line, yamlPathAbs, err)
	}
	root := &sub
	if root.Kind == yaml.DocumentNode && len(root.Content) == 1 {
		root = root.Content[0]
	}

	// Recurse FIRST: load all nested !include / !external content into
	// the tree. Then rewrite ALL files_dir scalars in the fully-resolved
	// tree (top + nested) with the cache prefix in one pass. Doing
	// rewrite-before-recurse would leave nested-loaded files_dir paths
	// unprefixed.
	visited[cycleKey] = true
	defer delete(visited, cycleKey)

	subDir := filepath.Dir(yamlPathAbs)
	if err := expandNode(root, subDir, rootDir, visited, depth+1); err != nil {
		return err
	}

	// Path rewrite: prefix every files_dir scalar in the fully-resolved
	// content with the cache-relative-from-rootDir prefix. After this
	// pass, fetched workspaces look like ordinary in-tree workspaces.
	cachePrefix, err := filepath.Rel(rootDir, cacheDir)
	if err != nil {
		return fmt.Errorf("!external at line %d: cannot compute cache prefix: %w", n.Line, err)
	}
	rewriteFilesDir(root, cachePrefix)

	// Replace the !external mapping with the resolved content in-place.
	*n = *root
	if n.Tag == "!external" {
		n.Tag = ""
	}
	return nil
}

// rewriteFilesDir walks the yaml node tree and prepends cachePrefix to
// every files_dir scalar value. Idempotent: if a files_dir value already
// starts with the prefix, no-op.
//
// !include paths are intentionally NOT rewritten. They resolve relative
// to their containing file's directory (subDir in expandNode), and after
// fetch that directory IS inside the cache, so relative !include paths
// Just Work without any rewrite. Rewriting them would double-prefix on
// recursive resolution.
//
// files_dir DOES need rewriting because it's consumed at workspace-
// provisioning time relative to orgBaseDir (the parent template's root),
// not relative to the workspace.yaml's containing dir.
func rewriteFilesDir(n *yaml.Node, cachePrefix string) {
	if n == nil {
		return
	}
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, value := n.Content[i], n.Content[i+1]
			if key.Kind == yaml.ScalarNode && key.Value == "files_dir" && value.Kind == yaml.ScalarNode {
				if !strings.HasPrefix(value.Value, cachePrefix+string(filepath.Separator)) && value.Value != cachePrefix {
					value.Value = filepath.Join(cachePrefix, value.Value)
				}
			}
		}
	}
	for _, child := range n.Content {
		rewriteFilesDir(child, cachePrefix)
	}
}

// safeRepoCacheDir converts a repo path like "molecule-ai/foo" into a
// filesystem-safe segment "molecule-ai__foo". Avoids nesting cache dirs
// (which would complicate cleanup).
func safeRepoCacheDir(host, repoPath string) string {
	hp := host + "/" + repoPath
	hp = strings.ReplaceAll(hp, "/", "__")
	hp = strings.ReplaceAll(hp, ":", "_")
	return hp
}

// gitFetcher is the production externalFetcher: shells out to `git` to
// clone the repo at ref into the cache dir. Cache key includes the
// resolved SHA, so different SHAs of the same ref get different cache
// dirs (no overwrite).
//
// Token handling — important for security. The auth token never enters
// the clone URL (and therefore never lands in the cloned repo's
// .git/config) and never appears in returned errors. We use git's
// `http.extraHeader` config option (passed via `-c`), which sends an
// Authorization header per-request without persisting it. The token is
// briefly visible in the `git` process's argv (so other local users
// with the same uid could see it via `ps`), which is the same exposure
// it has via the env var that supplied it.
//
// Cache validity uses a `.complete` marker written after a successful
// clone+rename. Cache-hit checks for the marker, not just the dir
// existence — a partially-written cache (clone failed mid-way, or a
// concurrent caller wrote a half-baked cache dir) is treated as cache
// miss and re-fetched cleanly.
type gitFetcher struct{}

// cacheCompleteMarker is the filename written after a successful clone.
// Cache-hit requires this marker; without it, the cache dir is treated
// as partially-written and re-fetched.
const cacheCompleteMarker = ".complete"

// Fetch resolves ref → SHA via `git ls-remote`, then `git clone --depth=1`
// if the cache dir is missing or incomplete. Auth via MOLECULE_GITEA_TOKEN
// injected via http.extraHeader (never via URL).
func (g *gitFetcher) Fetch(ctx context.Context, rootDir, host, repoPath, ref string) (string, string, error) {
	cacheRoot := filepath.Join(rootDir, externalCacheDirName, safeRepoCacheDir(host, repoPath))
	if err := os.MkdirAll(cacheRoot, 0o755); err != nil {
		return "", "", fmt.Errorf("mkdir cache root: %w", err)
	}

	cloneURL := buildExternalCloneURL(host, repoPath)
	gitArgs := func(extra ...string) []string {
		args := authConfigArgs()
		return append(args, extra...)
	}

	// 1. Resolve ref → SHA (so cache dir is content-addressable).
	sha, err := g.resolveRefToSHA(ctx, cloneURL, ref, gitArgs)
	if err != nil {
		return "", "", fmt.Errorf("ls-remote: %s", redactToken(err.Error()))
	}

	cacheDir := filepath.Join(cacheRoot, sha)
	// Cache-hit requires the .complete marker AND the .git dir.
	// Without the marker, cache is partially-written → treat as miss.
	if isCacheComplete(cacheDir) {
		return cacheDir, sha, nil
	}

	// Cache miss or partially-written — clean any stale cacheDir before
	// cloning (a previous broken attempt would otherwise block rename).
	os.RemoveAll(cacheDir)

	// 2. Clone into a sibling tmp dir; atomic rename on success.
	tmpDir, err := os.MkdirTemp(cacheRoot, sha+".tmp.")
	if err != nil {
		return "", "", fmt.Errorf("mkdir tmp: %w", err)
	}
	// MkdirTemp creates the dir; git clone refuses to clone into a
	// non-empty dir. Remove + recreate empty.
	os.RemoveAll(tmpDir)
	cloneAndConfig := append(gitArgs("clone", "--quiet", "--depth=1", "-b", ref, cloneURL, tmpDir))
	cmd := exec.CommandContext(ctx, "git", cloneAndConfig...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		os.RemoveAll(tmpDir)
		return "", "", fmt.Errorf("git clone: %w: %s", err, redactToken(strings.TrimSpace(string(out))))
	}

	// Write the .complete marker BEFORE the rename. If rename succeeds,
	// the marker is in place. If rename loses the race (concurrent
	// fetcher won), our tmp gets cleaned up and we trust the winner.
	if err := os.WriteFile(filepath.Join(tmpDir, cacheCompleteMarker), []byte(time.Now().UTC().Format(time.RFC3339)), 0o644); err != nil {
		os.RemoveAll(tmpDir)
		return "", "", fmt.Errorf("write complete marker: %w", err)
	}

	if err := os.Rename(tmpDir, cacheDir); err != nil {
		// Race: another import beat us. Validate THEIR cache, accept it.
		os.RemoveAll(tmpDir)
		if isCacheComplete(cacheDir) {
			return cacheDir, sha, nil
		}
		return "", "", fmt.Errorf("rename clone to cache (and winner's cache is incomplete): %w", err)
	}
	return cacheDir, sha, nil
}

// isCacheComplete reports whether cacheDir contains both the cloned
// repo (.git) and the .complete marker. Treats partial state as miss.
func isCacheComplete(cacheDir string) bool {
	if _, err := os.Stat(filepath.Join(cacheDir, ".git")); err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(cacheDir, cacheCompleteMarker)); err != nil {
		return false
	}
	return true
}

func (g *gitFetcher) resolveRefToSHA(ctx context.Context, cloneURL, ref string, gitArgs func(...string) []string) (string, error) {
	args := gitArgs("ls-remote", cloneURL, ref)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return "", fmt.Errorf("ref %q not found", ref)
	}
	// First whitespace-separated field is the SHA.
	for i, ch := range line {
		if ch == ' ' || ch == '\t' {
			return line[:i], nil
		}
	}
	return line, nil
}

// buildExternalCloneURL constructs the clone URL WITHOUT auth in userinfo.
// Auth is layered on via authConfigArgs's http.extraHeader.
func buildExternalCloneURL(host, repoPath string) string {
	u := url.URL{Scheme: "https", Host: host, Path: "/" + repoPath + ".git"}
	return u.String()
}

// authConfigArgs returns the `-c http.extraHeader=Authorization: token X`
// args to pass to git, OR an empty slice if no token is set. The token
// goes into the request headers (not the URL or .git/config), so it
// doesn't persist on disk and doesn't appear in clone error output.
func authConfigArgs() []string {
	token := os.Getenv("MOLECULE_GITEA_TOKEN")
	if token == "" {
		return nil
	}
	return []string{"-c", "http.extraHeader=Authorization: token " + token}
}

// redactToken scrubs the auth token from a string before it's logged
// or returned in an error. Belt-and-braces: with the http.extraHeader
// approach the token shouldn't appear in git's output, but if some
// future git version or libcurl debug mode emits it, this catches it.
func redactToken(s string) string {
	token := os.Getenv("MOLECULE_GITEA_TOKEN")
	if token == "" || len(token) < 8 {
		return s
	}
	return strings.ReplaceAll(s, token, "<redacted-token>")
}

