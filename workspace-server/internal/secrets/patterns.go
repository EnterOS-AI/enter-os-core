// Package secrets provides the canonical SSOT for credential-shaped
// regex patterns used by:
//
//   - the CI `Secret scan` workflow (.gitea/workflows/secret-scan.yml)
//   - the runtime's bundled pre-commit hook
//     (molecule-ai-workspace-runtime/molecule_runtime/scripts/pre-commit-checks.sh)
//   - the upcoming Phase 2b docker-exec Files API backend, which has
//     to refuse to surface files whose path OR content matches a
//     credential shape (RFC internal#425, Hongming 2026-05-15)
//
// Before this package, the same regex set lived as duplicate bash
// arrays in two unrelated repos; adding a pattern required editing
// both, and pattern drift was caught only via secret-scan workflow
// failures on PRs that had unrelated changes (#2090-class incident
// vector). Centralising in Go makes the Files API the SSOT, with the
// YAML + bash arrays generated/asserted from this package so drift
// is detected at CI time, not at exfiltration time.
//
// This file is Phase 2a of the internal#425 RFC. Phase 2b will import
// `Patterns` from `template_files_docker_exec.go` to gate
// `listFilesViaDockerExec` / `readFileViaDockerExec` against
// secret-shaped paths AND content. Until 2b lands, the package has
// one consumer: this package's own unit tests, which pin the regex
// strings so a refactor that drops or weakens one is caught here.
package secrets

import (
	"fmt"
	"regexp"
	"sync"
)

// Pattern is one named credential shape — a human label plus the
// compiled regex. The label appears in CI error output ("matched:
// github-pat") so an operator can identify the family without seeing
// the actual matched bytes (echoing the bytes widens the blast radius
// per the secret-scan workflow's recovery prose).
type Pattern struct {
	// Name is a short kebab-case identifier (e.g. "github-pat",
	// "anthropic-api-key"). Stable across versions — consumers may
	// switch on it.
	Name string
	// Description is a one-line human-readable explanation of what
	// the pattern matches. Used in CI error messages and the Files
	// API "<denied: secret-shape>" placeholder rationale.
	Description string
	// regexSource is the regex literal in Go-RE2 syntax. Stored as a
	// string so the slice declaration below stays readable; compiled
	// once via sync.Once into a *regexp.Regexp.
	regexSource string
}

// Patterns is the canonical credential-shape regex set.
//
// Adding a pattern here:
//
//  1. Add a new Pattern{} entry below with a kebab-case Name, a
//     one-line Description, and the regex literal. Anchor on a
//     low-false-positive prefix.
//  2. Add a positive + negative test case in patterns_test.go.
//  3. Mirror the regex string into:
//     a. .gitea/workflows/secret-scan.yml SECRET_PATTERNS array
//     b. molecule-ai-workspace-runtime/molecule_runtime/scripts/pre-commit-checks.sh
//     (or wait for the codegen target that consumes this slice — TBD
//     follow-up; tracked in the Phase 2a PR description.)
//
// The order is: alphabetical within each provider family, families
// grouped by ecosystem (GitHub family, AI-provider family, chat
// family, cloud family). Keep this stable so diffs are reviewable.
var Patterns = []Pattern{
	// --- GitHub token family ---
	{
		Name:        "github-pat-classic",
		Description: "GitHub personal access token (classic)",
		regexSource: `ghp_[A-Za-z0-9]{36,}`,
	},
	{
		Name:        "github-app-installation-token",
		Description: "GitHub App installation token (#2090 vector)",
		regexSource: `ghs_[A-Za-z0-9]{36,}`,
	},
	{
		Name:        "github-oauth-user-to-server",
		Description: "GitHub OAuth user-to-server token",
		regexSource: `gho_[A-Za-z0-9]{36,}`,
	},
	{
		Name:        "github-oauth-user",
		Description: "GitHub OAuth user token",
		regexSource: `ghu_[A-Za-z0-9]{36,}`,
	},
	{
		Name:        "github-oauth-refresh",
		Description: "GitHub OAuth refresh token",
		regexSource: `ghr_[A-Za-z0-9]{36,}`,
	},
	{
		Name:        "github-pat-fine-grained",
		Description: "GitHub fine-grained personal access token",
		regexSource: `github_pat_[A-Za-z0-9_]{82,}`,
	},

	// --- AI-provider API key family ---
	{
		Name:        "anthropic-api-key",
		Description: "Anthropic API key",
		regexSource: `sk-ant-[A-Za-z0-9_-]{40,}`,
	},
	{
		Name:        "openai-project-key",
		Description: "OpenAI project API key",
		regexSource: `sk-proj-[A-Za-z0-9_-]{40,}`,
	},
	{
		Name:        "openai-service-account-key",
		Description: "OpenAI service-account API key",
		regexSource: `sk-svcacct-[A-Za-z0-9_-]{40,}`,
	},
	{
		Name:        "minimax-api-key",
		Description: "MiniMax API key (F1088 vector)",
		regexSource: `sk-cp-[A-Za-z0-9_-]{60,}`,
	},

	// --- Chat-platform token family ---
	{
		Name:        "slack-token",
		Description: "Slack token (xoxb/xoxa/xoxp/xoxr/xoxs)",
		regexSource: `xox[baprs]-[A-Za-z0-9-]{20,}`,
	},

	// --- Cloud-provider credential family ---
	{
		Name:        "aws-access-key-id",
		Description: "AWS access key ID",
		regexSource: `AKIA[0-9A-Z]{16}`,
	},
	{
		Name:        "aws-sts-temp-access-key-id",
		Description: "AWS STS temporary access key ID",
		regexSource: `ASIA[0-9A-Z]{16}`,
	},
}

// compiledOnce protects the lazy build of compiledPatterns. We compile
// lazily so package init is cheap; callers pay only on first match
// (typically once per workspace-server boot).
var (
	compiledOnce     sync.Once
	compiledPatterns []*compiledPattern
	compileErr       error
)

type compiledPattern struct {
	Name        string
	Description string
	Re          *regexp.Regexp
}

// compileAll compiles every Pattern.regexSource into a *regexp.Regexp.
// Called once via compiledOnce. Any compile failure here is a build
// bug (the unit tests assert each regex compiles) — surfacing via
// returned error so callers don't panic in request handling.
func compileAll() {
	out := make([]*compiledPattern, 0, len(Patterns))
	for _, p := range Patterns {
		re, err := regexp.Compile(p.regexSource)
		if err != nil {
			compileErr = fmt.Errorf("secrets: pattern %q failed to compile: %w", p.Name, err)
			return
		}
		out = append(out, &compiledPattern{Name: p.Name, Description: p.Description, Re: re})
	}
	compiledPatterns = out
}

// ScanBytes returns a non-nil Match if any pattern matches anywhere
// inside b. Returns (nil, nil) on no match. Returns (nil, err) only
// if a regex in the package fails to compile — that's a build bug,
// not a runtime data issue.
//
// Match contains the pattern Name + Description so the caller can
// emit a path-or-content-denial rationale WITHOUT round-tripping the
// matched bytes (which would defeat the purpose). The matched bytes
// stay inside this function.
//
// The Files API Phase 2b backend will call ScanBytes on:
//
//   - the absolute path string (catches a file literally named
//     `ghs_abc.txt`)
//   - the file content (catches a credential pasted into a workspace
//     file by an agent or user — the Files API refuses to surface it
//     and the canvas renders "<denied: secret-shape>")
//
// Ordering: patterns are tried in declaration order. First match
// wins. This means narrower patterns (e.g. `sk-svcacct-…`) should
// appear in `Patterns` before broader ones (`sk-…`) — today there's
// no overlap, so order is descriptive only.
func ScanBytes(b []byte) (*Match, error) {
	compiledOnce.Do(compileAll)
	if compileErr != nil {
		return nil, compileErr
	}
	for _, cp := range compiledPatterns {
		if cp.Re.Match(b) {
			return &Match{Name: cp.Name, Description: cp.Description}, nil
		}
	}
	return nil, nil
}

// ScanString is the string-input convenience wrapper around ScanBytes.
// Identical semantics — the body never copies, []byte(s) is a
// zero-copy reinterpret for the regex matcher.
func ScanString(s string) (*Match, error) {
	return ScanBytes([]byte(s))
}

// Match describes which pattern caught a value. Deliberately does
// NOT include the matched substring — callers must not echo it.
type Match struct {
	// Name is the pattern's kebab-case identifier (e.g. "github-pat-classic").
	Name string
	// Description is the human-readable line for UI / log surfaces.
	Description string
}
