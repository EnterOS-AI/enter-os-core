package handlers

import (
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// gitIdentitySlugPattern collapses any run of non-alphanumeric characters
// into a single hyphen when deriving an email localpart from a workspace
// name. Dots, parentheses, unicode dashes, whitespace — all get squashed.
var gitIdentitySlugPattern = regexp.MustCompile(`[^a-z0-9]+`)

// gitIdentityEmailDomain is the @-part of generated agent emails. These
// addresses are not deliverable — they're identity markers only. Using
// the project's canonical domain keeps them attributable without looking
// like they belong to a real human inbox. If this changes, also update
// docs/authorship.md (when it exists).
const gitIdentityEmailDomain = "agents.moleculesai.app"

// gitAskpassHelperPath is the in-container path of the askpass helper
// installed by every workspace runtime image (workspace/Dockerfile in
// molecule-core; scripts/git-askpass.sh → /usr/local/bin/molecule-askpass
// in each external template-* repo). The helper reads GIT_HTTP_USERNAME
// / GIT_HTTP_PASSWORD (falling back to GITEA_USER / GITEA_TOKEN) from
// env and emits them on the git credential-prompt protocol. Setting
// GIT_ASKPASS to this path is what wires container-side HTTPS git auth
// to the persona credentials already arriving via workspace_secrets,
// with no on-disk .gitconfig / .git-credentials mutation required.
const gitAskpassHelperPath = "/usr/local/bin/molecule-askpass"

// applyAgentGitIdentity sets GIT_AUTHOR_* / GIT_COMMITTER_* env vars so
// every commit from this workspace container carries a distinct author
// in `git log` and `git blame`. Git reads these env vars before falling
// back to `git config user.name` / `user.email`, so this works even if
// the container's git config is untouched.
//
// Idempotent + respectful: if any of the four variables is already set
// (e.g. by an operator-supplied workspace_secret), the existing value
// wins — this function only fills in the defaults.
//
// The workspace name is the display name from org.yaml ("Frontend
// Engineer", "Product Marketing Manager", "Research Lead"). The email
// localpart is the slugified form of that name. Empty workspace names
// leave the env untouched — we don't want to emit
// `unknown@agents.moleculesai.app` for a provisioning glitch that
// dropped the name.
func applyAgentGitIdentity(envVars map[string]string, workspaceName string) {
	if envVars == nil {
		return
	}
	workspaceName = strings.TrimSpace(workspaceName)
	if workspaceName == "" {
		return
	}

	authorName := "Molecule AI " + workspaceName
	slug := slugifyForEmail(workspaceName)
	authorEmail := slug + "@" + gitIdentityEmailDomain

	setIfEmpty(envVars, "GIT_AUTHOR_NAME", authorName)
	setIfEmpty(envVars, "GIT_AUTHOR_EMAIL", authorEmail)
	setIfEmpty(envVars, "GIT_COMMITTER_NAME", authorName)
	setIfEmpty(envVars, "GIT_COMMITTER_EMAIL", authorEmail)

	applyGitAskpass(envVars)
}

// applyGitAskpass points git at the in-image askpass helper so that any
// HTTPS git operation against a remote without a pre-configured
// credential.helper picks up the persona credentials already present in
// the container env (GIT_HTTP_USERNAME / GIT_HTTP_PASSWORD, or
// GITEA_USER / GITEA_TOKEN as fallback — the latter pair is what
// loadPersonaEnvFile delivers from the operator-host bootstrap kit).
//
// Idempotent: if GIT_ASKPASS is already set (e.g. by an operator-
// supplied workspace_secret or an env-mutator plugin), the existing
// value wins. This lets a workspace opt out by setting GIT_ASKPASS=""
// or pointing at a different helper.
//
// No vendor-specific behaviour lives in this function — the host the
// credentials apply to is determined entirely by the deployer choosing
// when to populate GIT_HTTP_USERNAME / GIT_HTTP_PASSWORD (or
// GITEA_USER / GITEA_TOKEN). The helper script itself is generic and
// has no hardcoded hostnames, so it's safe to ship inside the
// open-source workspace template images alongside the platform-managed
// claude-code image.
func applyGitAskpass(envVars map[string]string) {
	if envVars == nil {
		return
	}
	setIfEmpty(envVars, "GIT_ASKPASS", gitAskpassHelperPath)
}

// applyAgentGitHTTPCreds reads the persona's HTTPS git credential from
// the operator-host bootstrap dir and injects it as GIT_HTTP_USERNAME /
// GIT_HTTP_PASSWORD so the in-container askpass helper can emit it on
// git's auth challenge.
//
// Why a dedicated env-var pair instead of reusing GITEA_USER / GITEA_TOKEN:
// the provisioner's forensic #145 denylist (provisioner.scmWriteTokenKeys)
// strips any env var named GITEA_TOKEN / GITHUB_TOKEN / GH_TOKEN /
// GITLAB_TOKEN / GL_TOKEN / BITBUCKET_TOKEN from tenant container env
// before docker run. That denylist is by exact key name, so the same
// token survives transport when shipped under the generic
// GIT_HTTP_USERNAME / GIT_HTTP_PASSWORD names that the askpass helper
// reads first (scripts/git-askpass.sh in each template-*). The username
// half stays an identifier (the persona's Gitea login), the password
// half carries the bytes from the persona token file.
//
// The fallback pair GITEA_USER / GITEA_TOKEN is ALSO set — GITEA_USER
// survives the denylist (it's an identity, not a credential) and
// GITEA_TOKEN is the no-op write that buildContainerEnv will drop.
// Both pairs in lockstep means the askpass helper's GIT_HTTP_*-first /
// GITEA_*-fallback chain works regardless of which lane lands first in
// the container env on any future provisioner refactor.
//
// Idempotent: existing GIT_HTTP_USERNAME / GIT_HTTP_PASSWORD keys are
// preserved. Operator-supplied workspace_secrets win over the persona
// token file by virtue of running BEFORE this helper in
// prepareProvisionContext.
//
// Silent no-op when:
//   - personaKey is empty (no role → no persona dir to consult)
//   - personaKey fails the safe-segment check (defense-in-depth against
//     a crafted role escaping the persona dir)
//   - the persona token file does not exist or is empty (legitimate
//     case for personas that don't ship a git-write credential — e.g.
//     read-only PM/Reviewer/Researcher identities or a partially-
//     provisioned bootstrap)
//
// No vendor-specific behaviour: this function reads bytes from a path
// and emits them as the standard askpass env-var pair. The host the
// credential applies to is determined by the deployer choosing which
// remote to push to — the askpass helper has no hardcoded hostnames.
func applyAgentGitHTTPCreds(envVars map[string]string, personaKey string) {
	if envVars == nil {
		return
	}
	personaKey = strings.TrimSpace(personaKey)
	if !isSafeRoleName(personaKey) {
		// Silent no-op for empty / unsafe keys — same shape as
		// loadPersonaTokenFile. Descriptive-role payloads (multi-word
		// "Frontend Engineer" etc.) take this branch and pick up
		// creds via workspace_secrets / org-import persona-env merge,
		// not the direct persona-token file path.
		return
	}
	root := os.Getenv("MOLECULE_PERSONA_ROOT")
	if root == "" {
		root = "/etc/molecule-bootstrap/personas"
	}
	tokenPath := filepath.Join(root, personaKey, "token")
	data, err := os.ReadFile(tokenPath)
	if err != nil {
		// Persona dir / file absent: legitimate for the host shapes
		// that don't ship the bootstrap kit (dev laptops, CI nodes)
		// or for personas that intentionally carry no git-write
		// credential. Caller decides whether the resulting
		// "Authentication failed" at first push is a configuration
		// error or expected behaviour.
		return
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return
	}

	// Primary lane — survives forensic #145 by virtue of the generic
	// GIT_HTTP_* names not being on the SCM-write denylist.
	setIfEmpty(envVars, "GIT_HTTP_USERNAME", personaKey)
	setIfEmpty(envVars, "GIT_HTTP_PASSWORD", token)

	// Fallback lane — askpass reads GITEA_USER / GITEA_TOKEN second.
	// GITEA_USER survives the denylist; GITEA_TOKEN will be stripped
	// by buildContainerEnv but is set here for completeness so the
	// (envVars map[string]string) contract is consistent for callers
	// inspecting it before the provisioner-level filter runs (e.g.
	// the env-mutator plugin chain).
	setIfEmpty(envVars, "GITEA_USER", personaKey)
	setIfEmpty(envVars, "GITEA_TOKEN", token)

	log.Printf("applyAgentGitHTTPCreds: injected GIT_HTTP_USERNAME/PASSWORD for persona %q (token %d bytes)", personaKey, len(token))
}

// slugifyForEmail collapses a workspace name to a safe email localpart:
// lowercase, non-alphanumeric runs → single hyphen, stripped at edges.
// "Frontend Engineer" → "frontend-engineer".
// "Product Marketing Manager" → "product-marketing-manager".
// "UIUX Designer" → "uiux-designer".
func slugifyForEmail(name string) string {
	lowered := strings.ToLower(name)
	slug := gitIdentitySlugPattern.ReplaceAllString(lowered, "-")
	return strings.Trim(slug, "-")
}

func setIfEmpty(m map[string]string, key, val string) {
	if _, ok := m[key]; ok {
		return
	}
	m[key] = val
}
