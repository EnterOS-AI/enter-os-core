package handlers

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
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
	rewriteFilesDirAndIncludes(root, cachePrefix)

	// Replace the !external mapping with the resolved content in-place.
	*n = *root
	if n.Tag == "!external" {
		n.Tag = ""
	}
	return nil
}

// rewriteFilesDirAndIncludes walks the yaml node tree and prepends
// cachePrefix to every files_dir scalar value. Idempotent: if a
// files_dir value already starts with the prefix, no-op.
//
// !include paths are NOT rewritten. They resolve relative to their
// containing file's directory (subDir in expandNode), and after fetch
// that directory IS inside the cache, so relative paths Just Work
// without any rewrite. Rewriting them would double-prefix.
//
// files_dir DOES need rewriting because it's consumed at workspace-
// provisioning time relative to orgBaseDir (the parent template's root),
// not relative to the workspace.yaml's containing dir.
func rewriteFilesDirAndIncludes(n *yaml.Node, cachePrefix string) {
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
		rewriteFilesDirAndIncludes(child, cachePrefix)
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
type gitFetcher struct{}

// Fetch resolves ref → SHA via `git ls-remote`, then `git clone --depth=1`
// if the cache dir is missing. Auth via MOLECULE_GITEA_TOKEN injected
// into the URL.
func (g *gitFetcher) Fetch(ctx context.Context, rootDir, host, repoPath, ref string) (string, string, error) {
	cacheRoot := filepath.Join(rootDir, externalCacheDirName, safeRepoCacheDir(host, repoPath))
	if err := os.MkdirAll(cacheRoot, 0o755); err != nil {
		return "", "", fmt.Errorf("mkdir cache root: %w", err)
	}

	cloneURL := buildAuthedURL(host, repoPath)

	// 1. Resolve ref → SHA (so cache dir is content-addressable).
	sha, err := g.resolveRefToSHA(ctx, cloneURL, ref)
	if err != nil {
		return "", "", fmt.Errorf("ls-remote: %w", err)
	}

	cacheDir := filepath.Join(cacheRoot, sha)
	if _, statErr := os.Stat(filepath.Join(cacheDir, ".git")); statErr == nil {
		// Cache hit.
		return cacheDir, sha, nil
	}

	// 2. Clone.
	tmpDir := cacheDir + ".tmp." + shortHash(time.Now().String())
	cmd := exec.CommandContext(ctx, "git", "clone", "--quiet", "--depth=1", "-b", ref, cloneURL, tmpDir)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		os.RemoveAll(tmpDir)
		return "", "", fmt.Errorf("git clone: %w: %s", err, strings.TrimSpace(string(out)))
	}
	// Atomic rename to final cache path.
	if err := os.Rename(tmpDir, cacheDir); err != nil {
		// Race: another import beat us to it. The other party's content
		// is at the same SHA, so it's equivalent. Cleanup our tmp.
		os.RemoveAll(tmpDir)
		if _, statErr := os.Stat(filepath.Join(cacheDir, ".git")); statErr != nil {
			return "", "", fmt.Errorf("rename clone to cache: %w", err)
		}
	}
	return cacheDir, sha, nil
}

func (g *gitFetcher) resolveRefToSHA(ctx context.Context, cloneURL, ref string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "ls-remote", cloneURL, ref)
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

func buildAuthedURL(host, repoPath string) string {
	token := os.Getenv("MOLECULE_GITEA_TOKEN")
	u := url.URL{Scheme: "https", Host: host, Path: "/" + repoPath + ".git"}
	if token != "" {
		u.User = url.UserPassword("oauth2", token)
	}
	return u.String()
}

func shortHash(s string) string {
	h := sha1.Sum([]byte(s))
	return hex.EncodeToString(h[:4])
}
