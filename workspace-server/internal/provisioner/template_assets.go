package provisioner

// template_assets.go — generic template-asset channel (RFC #2843 #24).
//
// This is the "generic, non-secret asset channel" the RFC proposes:
// a workspace's template assets (config.yaml + prompts/) are
// materialized from a template identity (resolved by the caller —
// the template repo path, a cached ref, etc.) rather than forced
// through the AWS Secrets Manager config bundle path that caps at
// cpConfigFilesMaxBytes (256 KiB).
//
// agent-skills are NO LONGER carried on this channel (RFC#2843 #32):
// skills are PLUGINS now, installed dynamically post-online via the
// plugin pipeline (the gitea:// plugin resolver reads the
// agent-skills/<skill> subpath from the template repo at install
// time). See IsCPTemplateAssetPath for the load-bearing allowlist.
//
// The fetcher is interface-typed so tests inject fakes and the real
// implementation (in main.go) wires the Gitea shallow-clone per
// RFC §4.2 transport option (a). The interface is NARROW (Load only)
// to keep the abstraction minimal — the template identity resolution
// and fetch are both the caller's job.
//
// Blast-radius / isolation (Reviewer-CR2 RC #11690 on the prior
// head): the fetcher ONLY materializes TEMPLATE ASSETS. Every
// path the fetcher returns is gated by IsCPTemplateAssetPath
// (this file) BEFORE it lands in the wire payload. Paths outside
// the allowlist — MEMORY.md, USER.md, CLAUDE.md, .claude/sessions/,
// /etc/passwd, traversal sequences — are REJECTED (the provision
// aborts with a structured error) rather than silently admitted.
// This is the load-bearing guard: a fetcher that returns a path
// outside the template-asset namespace is a programming error or
// an attack, and either way the safe response is to fail closed.
//
// Transport split (Reviewer-CR2 addendum on the prior head):
// fetched assets go to a SEPARATE wire field (TemplateAssets on
// cpProvisionRequest) rather than being merged into ConfigFiles.
// ConfigFiles is the bundle the CP stages through AWS Secrets
// Manager — the wrong layer for non-secret assets per the
// core-devops 10:13 SM-inventory RCA. The split lets a future CP
// route TemplateAssets through a non-secret channel (Gitea asset
// pin, S3 non-secret bucket, etc.) without a wire-shape change.
//
// Concurrency: the fetcher's Load is called from prepareProvisionContext,
// which serializes per-workspace (the existing per-workspace
// restart/provision gate at workspace_dispatchers.go:439 holds the
// mutex). No additional locking needed.

import (
	"context"
	"path/filepath"
	"strings"
)

// TemplateAssetFetcher materializes a template's
// config.yaml + prompts/ from a non-secret asset channel
// (template repo, Gitea shallow clone per RFC #2843 §4.2).
// Returned paths are RELATIVE to the template asset root (e.g.
// "config.yaml", "prompts/system.md") and the bytes are raw
// file contents (not base64-encoded — the generic channel
// does not require encoding; the wire format encodes per
// its own transport). agent-skills/* paths are NOT eligible
// (RFC#2843 #32 — skills are plugins now; see IsCPTemplateAssetPath)
// and are rejected at the consumer boundary if returned.
//
// Returned errors: a transport / resolution failure is
// returned as a non-nil error so the caller can abort the
// provision rather than silently regressing to stub-mode
// /configs (the same fail-closed contract as the persisted-
// bundle provider in #2831 PIECE 1).
//
// CONTRACT: every key in the returned map MUST match
// IsCPTemplateAssetPath. Keys that don't match are rejected
// by the caller (the provision aborts). Implementations that
// can't constrain their output to the allowlist must filter
// before returning.
type TemplateAssetFetcher interface {
	Load(ctx context.Context, templateIdentity string) (map[string][]byte, error)
}

// IsCPTemplateAssetPath reports whether a path returned by a
// TemplateAssetFetcher is eligible for transport to the workspace.
//
// Allowlist (load-bearing blast-radius guard — see RC #11690):
//
//   - "config.yaml" — the runtime entrypoint config
//   - "prompts/*"   — system prompts
//
// Everything else is REJECTED. Specifically excluded:
//   - "agent-skills/*" — agent skills are NO LONGER asset-channel
//     eligible (RFC#2843 #32). Skills are PLUGINS now: they install
//     DYNAMICALLY after the workspace boots online, via the plugin
//     install pipeline (gitea:// plugin source → the reconcile reads
//     the agent-skills/<skill> subpath from the template repo at
//     INSTALL time), NOT through this provisioning-time asset channel.
//     Keeping agent-skills/* in this allowlist re-created the original
//     #2831 failure: the ~716 KiB seo-all skill tree got pulled into
//     the provision request, which fail-closed BEFORE the CP was ever
//     called (the asset payload no longer rides SM, but the skills
//     have no business in the provision payload at all now that they
//     are plugins). The skill files MUST remain in the template repo
//     (the gitea:// plugin resolver reads them at install time) — this
//     allowlist only governs what the PROVISION-TIME asset channel
//     carries.
//   - MEMORY.md / USER.md (curated durable memory — agent-owned state,
//     reconciled by the boot entrypoint, not by this collect path),
//   - CLAUDE.md (runtime memory file, agent-owned),
//   - .claude/sessions/* (Claude Code session dir, agent-owned),
//   - anything outside the template-asset namespace.
//
// Path normalization: the function applies filepath.ToSlash
// + filepath.Clean before matching, so Windows-style
// separators, redundant "./" segments, and trailing slashes
// are normalized. Traversal sequences (".." or paths
// containing "/../") are NOT explicitly stripped here —
// callers must check for traversal before calling this
// function (the existing addFile in cp_provisioner.go does).
// This function only validates the namespace; the traversal
// check is a separate invariant.
func IsCPTemplateAssetPath(name string) bool {
	name = filepath.ToSlash(filepath.Clean(name))
	return name == "config.yaml" ||
		strings.HasPrefix(name, "prompts/")
}

// noopTemplateAssetFetcher is the self-host default fetcher (PR-B
// selection, RFC #2843 #24). It returns (nil, nil) — "no assets to
// add" — so the call site in collectCPConfigFiles treats the
// workspace as a self-host: the /configs path comes from the local
// TemplatePath (cfg.TemplatePath) + cfg.ConfigFiles only, and the
// new TemplateAssets wire field is empty.
//
// Why explicit and not nil-interface: WorkspaceConfig's
// TemplateAssetFetcher is a non-nil interface-typed field in the SaaS
// path, so the SaaS codepath can rely on "interface always set"
// without nil-checks. Using a no-op default (rather than nil) keeps
// the field type uniform across deployments and makes
// "self-host = no-op fetcher" an explicit choice rather than an
// accidental absence.
//
// Memory-preserving: the no-op is the only state. The fetcher is
// stateless; concurrent Load calls are safe.
type noopTemplateAssetFetcher struct{}

// Load on noopTemplateAssetFetcher returns (nil, nil) — the
// "no assets" signal. Tests pin this contract.
func (noopTemplateAssetFetcher) Load(_ context.Context, _ string) (map[string][]byte, error) {
	return nil, nil
}

// NoopTemplateAssetFetcher returns the no-op fetcher suitable
// as the self-host default. Exported so main.go can wire it
// via the fetcher-selection helper.
func NoopTemplateAssetFetcher() TemplateAssetFetcher {
	return noopTemplateAssetFetcher{}
}

// FetcherSelection is the result of choosing which
// TemplateAssetFetcher to wire based on deployment mode (SaaS
// vs self-host) and per-deployment token state.
type FetcherSelection struct {
	// Fetcher is the chosen fetcher. For SaaS this is the Gitea
	// fetcher; for self-host this is the no-op fetcher. Never
	// nil — the selection helper always returns a usable
	// fetcher (no-op for self-host is still a valid choice).
	Fetcher TemplateAssetFetcher

	// Authenticated reports whether the chosen fetcher will send
	// an Authorization header. For self-host's no-op fetcher
	// this is false (no-op never sends headers); for SaaS this
	// is true iff a non-empty token was supplied. Logged at
	// boot to make the active mode obvious.
	Authenticated bool

	// Mode is a short human-readable label for the active mode
	// (e.g. "saas-gitea", "saas-gitea-public", "self-host-noop").
	// Used only for boot-time logging — not load-bearing.
	Mode string
}

// SelectTemplateAssetFetcher chooses the fetcher to wire for the
// current deployment. The selection matrix:
//
//   - isSaaSTenant() && token != ""  -> real Gitea fetcher, Authenticated=true
//   - isSaaSTenant() && token == ""  -> real Gitea fetcher, Authenticated=false
//     (the public-fetch activation: molecule-ai/* templates are PUBLIC)
//   - !isSaaSTenant()                 -> no-op fetcher, Authenticated=false
//     (self-host doesn't need an external asset channel —
//     cfg.TemplatePath + cfg.ConfigFiles handle /configs locally)
//
// PR-B keystone (RFC #2843 #24): the token is OPTIONAL. SaaS
// callers may leave the token empty (public-fetch activation)
// and still get the real Gitea fetcher. Self-host callers always
// get the no-op.
//
// The isSaaSTenant function is plumbed in as an argument rather
// than a package-level lookup so the selection is testable in
// isolation (production callers pass a closure over the
// canonical isSaaSTenant helper, tests pass a closure that
// returns a fixed value).
func SelectTemplateAssetFetcher(isSaaSTenant func() bool, baseURL, token string) FetcherSelection {
	if isSaaSTenant == nil || !isSaaSTenant() {
		return FetcherSelection{
			Fetcher:       NoopTemplateAssetFetcher(),
			Authenticated: false,
			Mode:          "self-host-noop",
		}
	}
	// SaaS: real Gitea fetcher (public-fetch if token empty, authenticated if set)
	return FetcherSelection{
		Fetcher:       NewGiteaTemplateAssetFetcher(baseURL, token, nil),
		Authenticated: token != "",
		Mode:          "saas-gitea-public", // PR-B's CTO public-fetch is the SaaS default
	}
}
