package plugins

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// GithubResolver fetches plugins from a GitHub repository by shallow-
// cloning at the specified ref (default branch if no ref is given).
//
// Spec format: "<owner>/<repo>" or "<owner>/<repo>#<ref>"
//   - "foo/bar"           → clone https://github.com/foo/bar at default branch
//   - "foo/bar#v1.2.0"    → clone at tag v1.2.0
//   - "foo/bar#main"      → clone at branch main
//   - "foo/bar#sha"       → fetch + checkout a specific commit
//
// The resolver shells out to the `git` binary; the platform's Dockerfile
// installs git for this reason. A mockable GitRunner lets tests inject a
// fake without requiring git on the test host.
type GithubResolver struct {
	// GitRunner runs git commands. Defaults to shelling out to the
	// system `git`. Overridable in tests.
	GitRunner func(ctx context.Context, dir string, args ...string) error

	// BaseURL defaults to https://github.com. Tests point it at a local
	// file:// bare repo.
	BaseURL string

	// LastFetchSHA is set by Fetch after a successful clone. It holds the
	// commit SHA that was checked out. callers can retrieve it via LastSHA().
	// Only valid after a successful Fetch call; reset on each Fetch.
	LastFetchSHA string
}

// NewGithubResolver constructs a resolver with sensible defaults.
func NewGithubResolver() *GithubResolver {
	return &GithubResolver{
		GitRunner: defaultGitRunner,
		BaseURL:   "https://github.com",
	}
}

// LastSHA returns the SHA of the last successful Fetch call, or "" if
// Fetch has not been called or the last call failed.
func (r *GithubResolver) LastSHA() string { return r.LastFetchSHA }

// Scheme returns "github".
func (r *GithubResolver) Scheme() string { return "github" }

// repoRE matches "<owner>/<repo>" with optional "#<ref>" suffix.
//
//   - Owner / repo: must start with alphanumeric, then 0–99 chars from
//     [a-zA-Z0-9_.-]. Matches GitHub's validation.
//   - Ref: must NOT start with `-` (prevents ref-as-flag injection like
//     "-exec=/evil"). Then 0–254 chars from [a-zA-Z0-9_./-]. Disallows
//     whitespace and shell metacharacters. The handler additionally
//     passes `--` before the URL when invoking git, for defense in depth.
var repoRE = regexp.MustCompile(
	`^([a-zA-Z0-9][a-zA-Z0-9_.\-]{0,99})/([a-zA-Z0-9][a-zA-Z0-9_.\-]{0,99})(?:#([a-zA-Z0-9_.][a-zA-Z0-9_./\-]{0,254}))?$`,
)

// Fetch clones the repository and copies its contents (minus .git) into dst.
// Returns the repository name (second path segment) as the plugin name.
func (r *GithubResolver) Fetch(ctx context.Context, spec string, dst string) (string, error) {
	spec = strings.TrimSpace(spec)
	m := repoRE.FindStringSubmatch(spec)
	if m == nil {
		return "", fmt.Errorf("github resolver: spec %q must be <owner>/<repo>[#<ref>]", spec)
	}
	owner, repo, ref := m[1], m[2], m[3]

	// Pinned-ref enforcement (#768 Control 2): reject bare "org/repo" specs
	// without a "#ref" fragment. Only pinned refs are accepted in production.
	// PLUGIN_ALLOW_UNPINNED=true bypasses this for local development.
	if ref == "" && os.Getenv("PLUGIN_ALLOW_UNPINNED") != "true" {
		return "", fmt.Errorf("github resolver: spec %q requires a pinned ref (e.g. %s/%s#v1.0.0); "+
			"set PLUGIN_ALLOW_UNPINNED=true for local dev", spec, owner, repo)
	}

	runner := r.GitRunner
	if runner == nil {
		runner = defaultGitRunner
	}
	base := r.BaseURL
	if base == "" {
		base = "https://github.com"
	}
	url := fmt.Sprintf("%s/%s/%s.git", base, owner, repo)

	// Clone into a sibling temp dir, then move contents to dst minus
	// .git. We use a sibling (not dst itself) because `git clone` wants
	// to create the target; dst may already exist as an empty dir.
	workDir, err := os.MkdirTemp("", "molecule-gh-clone-*")
	if err != nil {
		return "", fmt.Errorf("github resolver: tempdir: %w", err)
	}
	defer os.RemoveAll(workDir)

	cloneTarget := filepath.Join(workDir, "repo")
	args := []string{"clone", "--depth=1"}
	if ref != "" {
		args = append(args, "--branch", ref)
	}
	// `--` unconditionally separates flags from positional args; URL +
	// target are positional. Defense in depth against any future arg-
	// parser quirks.
	args = append(args, "--", url, cloneTarget)
	if err := runner(ctx, workDir, args...); err != nil {
		// Map common "repository / ref doesn't exist" outputs to
		// ErrPluginNotFound so the handler returns 404. Everything else
		// stays as a 502 (network, auth, etc.).
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "repository not found") ||
			strings.Contains(msg, "could not find remote branch") ||
			strings.Contains(msg, "remote branch") && strings.Contains(msg, "not found") {
			return "", fmt.Errorf("github resolver: %s: %w", url, ErrPluginNotFound)
		}
		return "", fmt.Errorf("github resolver: clone %s failed: %w", url, err)
	}

	// Capture the SHA before we strip .git. This is the commit that will
	// be installed, used by the drift detector to seed installed_sha so
	// subsequent cycles can detect drift.
	// runGit captures output; errors are non-fatal — an unknown SHA just
	// means drift detection can't work for this row, which is acceptable.
	if shaOut, shaErr := runGitOneLine(ctx, cloneTarget, "rev-parse", "--verify", "HEAD"); shaErr == nil {
		r.LastFetchSHA = strings.TrimSpace(shaOut)
	}

	// Strip .git so the plugin dir doesn't become a nested repo in the
	// workspace container's filesystem.
	if err := os.RemoveAll(filepath.Join(cloneTarget, ".git")); err != nil {
		return "", fmt.Errorf("github resolver: remove .git: %w", err)
	}

	// Move contents to dst.
	if err := copyTree(ctx, cloneTarget, dst); err != nil {
		return "", fmt.Errorf("github resolver: copy to dst: %w", err)
	}

	return repo, nil
}

// runGitOneLine runs git with args in dir and returns stdout trimmed.
// Returns "" on error (caller decides whether to treat it as fatal).
func runGitOneLine(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	childEnv := os.Environ()
	if os.Getenv("HOME") == "" && dir != "" {
		childEnv = append(childEnv, "HOME="+dir)
	}
	childEnv = append(childEnv, "LANG=C", "LC_ALL=C")
	cmd.Env = childEnv
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %v: %w (output: %s)", args, err, string(out))
	}
	return string(out), nil
}

// defaultGitRunner shells out to the system `git`. `dir` is the working
// directory for the command (nil/empty means current process cwd).
func defaultGitRunner(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	// Build a per-child env. We never mutate os.Environ()'s backing
	// slice.
	childEnv := os.Environ()
	//  - HOME: `git clone` touches HOME for credential helpers even on
	//    anonymous HTTPS; set to work dir if the parent process has none.
	if os.Getenv("HOME") == "" && dir != "" {
		childEnv = append(childEnv, "HOME="+dir)
	}
	//  - LANG=C / LC_ALL=C: force English output so our ErrPluginNotFound
	//    mapping ("repository not found", "remote branch ... not found")
	//    doesn't silently stop working under a different locale.
	childEnv = append(childEnv, "LANG=C", "LC_ALL=C")
	cmd.Env = childEnv
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %v: %w (output: %s)", args, err, string(out))
	}
	return nil
}

// ResolveRef resolves a plugin spec to a full commit SHA.
//
// Used by the drift sweeper to compare the SHA installed in a workspace
// against the current upstream SHA for the tracked ref.
//
// Spec shapes:
//   - "owner/repo#tag:v1.0.0" → fetch the tag, return its commit SHA
//   - "owner/repo#tag:latest"  → fetch tags, find the latest tag, return its SHA
//   - "owner/repo#sha:abc123" → already a full SHA; validate and return as-is
//   - "owner/repo#main"       → fetch the branch, return its tip SHA
//
// Returns ErrPluginNotFound if the ref does not exist upstream.
func (r *GithubResolver) ResolveRef(ctx context.Context, spec string) (string, error) {
	spec = strings.TrimSpace(spec)
	m := repoRE.FindStringSubmatch(spec)
	if m == nil {
		return "", fmt.Errorf("github resolver: spec %q must be <owner>/<repo>[#<ref>]", spec)
	}
	owner, repo, ref := m[1], m[2], m[3]
	if ref == "" {
		return "", fmt.Errorf("github resolver: ResolveRef requires a ref (got bare %q)", spec)
	}

	base := r.BaseURL
	if base == "" {
		base = "https://github.com"
	}
	url := fmt.Sprintf("%s/%s/%s.git", base, owner, repo)

	// Clone shallowly into a temp dir, then resolve the SHA.
	// --depth=1 keeps the network cost bounded regardless of repo size.
	workDir, err := os.MkdirTemp("", "molecule-resolve-ref-*")
	if err != nil {
		return "", fmt.Errorf("github resolver: tempdir: %w", err)
	}
	defer os.RemoveAll(workDir)

	runner := r.GitRunner
	if runner == nil {
		runner = defaultGitRunner
	}

	// Build ref to fetch: for "tag:latest" we fetch all tags; for
	// "tag:vX.Y.Z" we fetch that specific tag; for bare refs we fetch
	// the branch/commit directly.
	fetchArgs := []string{"fetch", "--depth=1"}
	switch {
	case strings.HasPrefix(ref, "tag:"):
		tagName := strings.TrimPrefix(ref, "tag:")
		if tagName == "latest" {
			// Fetch all tags so we can find the latest one.
			fetchArgs = []string{"fetch", "--tags", "--deepen=1", "--", url}
		} else {
			fetchArgs = append(fetchArgs, "--", url, "tag", tagName)
		}
	case strings.HasPrefix(ref, "sha:"):
		// Already a SHA; just fetch it directly.
		sha := strings.TrimPrefix(ref, "sha:")
		fetchArgs = append(fetchArgs, "--", url, sha)
	default:
		// Branch or other named ref.
		fetchArgs = append(fetchArgs, "--", url, ref)
	}

	if err := runner(ctx, workDir, fetchArgs...); err != nil {
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "repository not found") ||
			strings.Contains(msg, "could not find remote ref") ||
			strings.Contains(msg, "remote ref not found") {
			return "", fmt.Errorf("github resolver: %s: %w", url, ErrPluginNotFound)
		}
		return "", fmt.Errorf("github resolver: fetch %s %s failed: %w", url, ref, err)
	}

	// Resolve the ref to a SHA.
	var shaOut []byte
	var resolveErr error
	if strings.HasPrefix(ref, "tag:") {
		tagName := strings.TrimPrefix(ref, "tag:")
		if tagName == "latest" {
			// Find the most recent tag by commit date.
			tagCmd := exec.CommandContext(ctx, "git", "-C", workDir,
				"describe", "--tags", "--abbrev=0", "HEAD")
			tagOut, tagErr := tagCmd.CombinedOutput()
			if tagErr != nil {
				return "", fmt.Errorf("github resolver: no tags found in %s: %w (%s)",
					owner+"/"+repo, tagErr, string(tagOut))
			}
			resolvedTag := strings.TrimSpace(string(tagOut))
			shaCmd := exec.CommandContext(ctx, "git", "-C", workDir,
				"rev-parse", "--verify", "refs/tags/"+resolvedTag+"^{commit}")
			shaOut, resolveErr = shaCmd.CombinedOutput()
		} else {
			shaCmd := exec.CommandContext(ctx, "git", "-C", workDir,
				"rev-parse", "--verify", "refs/tags/"+tagName+"^{commit}")
			shaOut, resolveErr = shaCmd.CombinedOutput()
		}
	} else {
		refName := ref
		if strings.HasPrefix(ref, "sha:") {
			refName = strings.TrimPrefix(ref, "sha:")
		}
		shaCmd := exec.CommandContext(ctx, "git", "-C", workDir,
			"rev-parse", "--verify", refName+"^{commit}")
		shaOut, resolveErr = shaCmd.CombinedOutput()
	}
	if resolveErr != nil {
		return "", fmt.Errorf("github resolver: rev-parse %s failed: %w (%s)",
			ref, resolveErr, string(shaOut))
	}
	return strings.TrimSpace(string(shaOut)), nil
}
