package provisioner

// gitea_template_assets.go — real Gitea-based TemplateAssetFetcher
// (RFC #2843 #24, PR-B). The previous PR-A SCAFFOLD (#2855) wired
// the interface + fields; this file is the production impl.
//
// Transport: GET {baseURL}/api/v1/repos/<owner>/<repo>/archive/<ref>.tar.gz
// with header "Authorization: token <token>" → stream-extract the
// tarball → return map[relpath][]byte for ONLY allowlisted paths
// (config.yaml + prompts/** — agent-skills are plugins now, RFC#2843
// #32, and are skipped by the IsCPTemplateAssetPath filter) → strip
// the archive's top-level dir prefix.
//
// Template identity format: "<owner>/<repo>@<ref>" (e.g.
// "molecule-ai/workspace-template-claude-code@main"). The caller
// (workspace_provision.go) is responsible for resolving the runtime
// name to a (repo, ref) pair via the runtime_registry / manifest.json.
// The fetcher just parses + fetches.
//
// Fail-closed: any transport / extraction / parse error returns a
// non-nil error so the caller can abort the provision rather than
// silently regressing to stub-mode /configs (same contract as the
// persisted-bundle provider in #2831 PIECE 1).
//
// Memory-preservation: this fetcher ONLY materializes TEMPLATE
// ASSETS (config.yaml + prompts/*). agent-skills/* are NOT carried
// (RFC#2843 #32 — skills are plugins now, installed post-online); the
// IsCPTemplateAssetPath filter below skips them. That same allowlist
// in the consumer (collectCPConfigFiles) is the load-bearing guard
// against a fetcher that returns a path outside the template-asset
// namespace (e.g. agent-skills/*, /workspace, MEMORY.md, USER.md,
// CLAUDE.md, .claude/sessions/*) — those would either be rejected by
// the allowlist (provision aborts) or, if they somehow slipped
// through, would land in the SEPARATE TemplateAssets field rather
// than the SM-bound ConfigFiles field. The transport split
// (TemplateAssets vs ConfigFiles) is the second line of defense
// against clobbering agent-owned state.
//
// Auth: per-identity READ-ONLY Gitea token. The token is threaded
// from Infisical SSOT (per #2676 program) into the fetcher via the
// Token field. When the token is set, it MUST be a per-identity PAT
// scoped to the template repo with read-only access — NOT a founder
// PAT, NOT a workspace-admin PAT.
//
// PUBLIC FETCH (empty token): when the token is empty, the fetcher
// performs an UNAUTHENTICATED request — the Authorization header
// is OMITTED ENTIRELY (not sent as "token " with an empty value,
// which Gitea would 401 as a malformed credential). This enables
// the public-fetch activation: SaaS tenants without a configured
// MOLECULE_TEMPLATE_REPO_TOKEN can still fetch molecule-ai/*
// templates, which are PUBLIC repos on the Gitea instance. Self-
// host callers use the no-op fetcher and never reach this code path.
//
// Workspace-server never logs or echoes the token (the httpClient
// logs are scrubbed via the standard net/http strip-header path;
// the token is in the Authorization header, which net/http does
// not log by default). The empty-token path sends NO header at all
// (strictly less information disclosure than a populated header).
//
// NO SIZE CAP: the existing #2845 acbc0da9 added a 16MiB bound
// for TemplateAssets in collectCPConfigFiles (separate from the
// 256KiB SM cap on ConfigFiles). The fetcher itself does NOT cap
// the response — the consumer-side bound is the cap. This matches
// the dispatch's "NO size cap" on the fetcher.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"time"
)

// giteaTemplateAssetFetcher is the production TemplateAssetFetcher
// backed by a Gitea archive endpoint. The baseURL, Token, and
// httpClient fields are exported via a constructor (NewGiteaTemplateAssetFetcher)
// so main.go can wire per-deployment values without making the
// struct fields mutable post-construction.
type giteaTemplateAssetFetcher struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

// NewGiteaTemplateAssetFetcher returns a Gitea-backed fetcher wired
// with the given base URL, read-only PAT, and HTTP client. The HTTP
// client may be nil — the constructor substitutes a sane default
// (30s timeout, no surprises). Callers SHOULD use a shared client
// with a connection pool in production; the per-call client here
// is for tests + fallback.
func NewGiteaTemplateAssetFetcher(baseURL, token string, httpClient *http.Client) TemplateAssetFetcher {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &giteaTemplateAssetFetcher{
		baseURL:    strings.TrimRight(baseURL, "/"),
		token:      token,
		httpClient: httpClient,
	}
}

// parseTemplateIdentity splits "<owner>/<repo>@<ref>" into its
// three parts. Returns an error on missing pieces or extra "@"
// segments. The ref part is the git ref (branch, tag, or SHA); a
// missing "@" is an error (no default — the caller must specify a
// pinned ref so the artifact is reproducible).
func parseTemplateIdentity(identity string) (owner, repo, ref string, err error) {
	if identity == "" {
		return "", "", "", errors.New("templateIdentity is empty (want \"<owner>/<repo>@<ref>\")")
	}
	atIdx := strings.LastIndex(identity, "@")
	if atIdx < 0 {
		return "", "", "", fmt.Errorf("templateIdentity %q has no @ref suffix (want \"<owner>/<repo>@<ref>\")", identity)
	}
	ref = identity[atIdx+1:]
	if ref == "" {
		return "", "", "", fmt.Errorf("templateIdentity %q has empty @ref", identity)
	}
	repoPart := identity[:atIdx]
	slashIdx := strings.Index(repoPart, "/")
	if slashIdx < 0 {
		return "", "", "", fmt.Errorf("templateIdentity %q has no <owner>/<repo> prefix", identity)
	}
	owner = repoPart[:slashIdx]
	repo = repoPart[slashIdx+1:]
	if owner == "" || repo == "" {
		return "", "", "", fmt.Errorf("templateIdentity %q has empty owner or repo", identity)
	}
	if strings.Contains(repo, "@") {
		return "", "", "", fmt.Errorf("templateIdentity %q has extra @ in repo path", identity)
	}
	return owner, repo, ref, nil
}

// Load fetches the template's tarball archive and returns the
// allowlisted asset map. See the package doc-comment for the full
// transport + allowlist contract.
//
// PUBLIC FETCH (empty token): when f.token is empty, the fetcher
// sends an UNAUTHENTICATED request (NO Authorization header). This
// is the public-fetch activation that lets SaaS tenants without a
// configured MOLECULE_TEMPLATE_REPO_TOKEN fetch PUBLIC template
// repos. The earlier code rejected an empty token at Load time
// (forcing SaaS-no-token tenants to fetch ZERO templates — a
// runtime defect caught by the driver in #2903 review). Empty
// token + non-empty token both go through the same code path
// below; the only difference is the optional Authorization header.
func (f *giteaTemplateAssetFetcher) Load(ctx context.Context, templateIdentity string) (map[string][]byte, error) {
	if f.baseURL == "" {
		return nil, errors.New("giteaTemplateAssetFetcher: baseURL is empty")
	}
	owner, repo, ref, err := parseTemplateIdentity(templateIdentity)
	if err != nil {
		return nil, fmt.Errorf("giteaTemplateAssetFetcher: %w", err)
	}

	url := fmt.Sprintf("%s/api/v1/repos/%s/%s/archive/%s.tar.gz", f.baseURL, owner, repo, ref)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("giteaTemplateAssetFetcher: build request: %w", err)
	}
	// Only set Authorization when a token is configured. Sending
	// "token " with an empty value would be a malformed credential
	// that Gitea 401s on — strictly worse than sending no header at
	// all (which is what the public path needs).
	if f.token != "" {
		req.Header.Set("Authorization", "token "+f.token)
	}
	req.Header.Set("Accept", "application/gzip, application/octet-stream")

	resp, err := f.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("giteaTemplateAssetFetcher: GET %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// Read up to 4 KiB of the body for the error message (the
		// body may contain a structured Gitea error envelope). Cap
		// the read so a hostile server can't OOM us.
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("giteaTemplateAssetFetcher: GET %s: HTTP %d: %s", url, resp.StatusCode, string(preview))
	}

	// The Gitea archive endpoint returns a tar.gz — gunzip then
	// stream-extract.
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("giteaTemplateAssetFetcher: gzip.NewReader: %w", err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	assets := make(map[string][]byte)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("giteaTemplateAssetFetcher: tar.Next: %w", err)
		}
		if hdr.FileInfo().IsDir() {
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		// Strip the archive's top-level dir prefix. Gitea tarballs
		// wrap every entry in "<repo>-<sha>/<relpath>" — we want
		// just the relpath so the allowlist check is straightforward.
		rel, ok := stripArchiveTopDir(hdr.Name)
		if !ok {
			// Entry is at the root or has no top-level dir; skip.
			continue
		}
		// Allowlist filter — defense-in-depth. The consumer
		// (collectCPConfigFiles) ALSO gates on IsCPTemplateAssetPath;
		// skipping non-allowlisted entries here is a free perf win
		// (don't allocate bytes for paths the consumer will reject)
		// and a cleaner audit log. Per RFC#2843 #32 this also skips the
		// (potentially large) agent-skills/* tree — skills are plugins
		// now, fetched at install time by the gitea:// plugin resolver,
		// NOT carried on this provisioning-time channel.
		if !IsCPTemplateAssetPath(rel) {
			continue
		}
		// Read the file body. tar.Reader streams; we read in a
		// bounded buffer (16 MiB safety per #2845 acbc0da9 — skill
		// packages can be 700 KiB; we leave headroom for the
		// largest expected template asset). The consumer-side cap
		// is the real bound; this is just to prevent a hostile
		// tarball from allocating a terabyte.
		const perFileSafety = 16 << 20
		data, err := io.ReadAll(io.LimitReader(tr, perFileSafety+1))
		if err != nil {
			return nil, fmt.Errorf("giteaTemplateAssetFetcher: read %s: %w", rel, err)
		}
		if len(data) > perFileSafety {
			return nil, fmt.Errorf("giteaTemplateAssetFetcher: %s exceeds per-file safety bound %d bytes (cap enforcement is at the consumer)", rel, perFileSafety)
		}
		assets[rel] = data
	}
	return assets, nil
}

// stripArchiveTopDir strips the archive's top-level dir prefix
// from a tarball entry. Gitea wraps entries as "<topdir>/<relpath>"
// where <topdir> is typically "<repo>-<sha>" (e.g.
// "workspace-template-claude-code-abcd1234/config.yaml"). Returns
// the relpath and true on success; returns "" and false if the
// entry is at the top level (no slash), is malformed, or
// contains traversal sequences (../).
//
// Traversal check: a legitimate Gitea archive contains ONLY paths
// inside the top dir; any "../" segment in the relpath is a
// hostile-smuggling attempt (a malicious tarball trying to
// land a file at e.g. "../etc/passwd" — which would write OUTSIDE
// the top dir). Reject before path.Clean collapses the segments
// (otherwise "../etc/passwd" → "/etc/passwd" would slip through).
func stripArchiveTopDir(name string) (string, bool) {
	if name == "" {
		return "", false
	}
	// The first slash separates the top-level dir from the
	// relpath. If there's no slash, the entry is at the top
	// level (malformed for our purposes — Gitea's archive always
	// wraps in a top dir).
	slashIdx := strings.Index(name, "/")
	if slashIdx < 0 {
		return "", false
	}
	rel := name[slashIdx+1:]
	if rel == "" {
		return "", false
	}
	// Traversal rejection — check BOTH the raw relpath AND the
	// cleaned form. Raw catches the obvious "../" segment; cleaned
	// catches the sneaky "../../foo" that normalizes but still
	// escaped the top dir. path.Clean collapses segments, so we
	// re-detect via the leading "/" + the resulting
	// non-relpath-but-still-traversed form. The simplest
	// load-bearing check: the cleaned path, when prefixed with
	// "/", must NOT contain a "/../" segment. (path.Clean
	// guarantees no "/../" remains in the output of a clean run,
	// so if "/../" appears it means the input had a traversal
	// that resolved to a path starting with one of its parents.)
	cleaned := path.Clean("/" + rel)
	if strings.Contains(cleaned, "/../") {
		return "", false
	}
	// Also check the raw relpath for the obvious ".." segment
	// prefix (defense in depth — if the tarball entry is just
	// "../" with nothing else, path.Clean("/../") is "/", which
	// is the root, also a reject).
	if strings.HasPrefix(rel, "..") || strings.Contains(rel, "/../") {
		return "", false
	}
	cleaned = strings.TrimPrefix(cleaned, "/")
	if cleaned == "" || cleaned == "." {
		return "", false
	}
	return cleaned, true
}

// Compile-time check: giteaTemplateAssetFetcher implements the
// TemplateAssetFetcher interface. A future refactor that breaks
// the signature is caught at the earliest possible moment
// (compile time of the package, not at runtime via duck-typing).
var _ TemplateAssetFetcher = (*giteaTemplateAssetFetcher)(nil)

// Sentinel for tests that want to assert "this URL was hit".
// (Not exported — tests can re-derive via a custom httpClient.)
var _ = bytes.NewReader // keep import in case future refactor needs it
