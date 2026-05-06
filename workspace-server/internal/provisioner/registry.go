package provisioner

import (
	"fmt"
	"os"
)

// defaultRegistryPrefix is the upstream OSS face for all workspace template
// images. Self-hosted Molecule deployments without the MOLECULE_IMAGE_REGISTRY
// override pull from here.
const defaultRegistryPrefix = "ghcr.io/molecule-ai"

// knownRuntimes is the canonical list of workspace template runtimes shipped
// in main. Any runtime added here MUST also have a standalone template repo
// (Molecule-AI/molecule-ai-workspace-template-<name>) and an entry in the
// publish-template-image workflow that builds it.
//
// Order matters for deterministic test snapshots; keep alphabetical.
var knownRuntimes = []string{
	"autogen",
	"claude-code",
	"codex",
	"crewai",
	"deepagents",
	"gemini-cli",
	"hermes",
	"langgraph",
	"openclaw",
}

// defaultRuntime is the fallback when a workspace's config doesn't specify a
// runtime. Picked because LangGraph is the most common in our org templates
// and has the smallest "first impression" cold-start surface.
const defaultRuntime = "langgraph"

// RegistryPrefix returns the registry prefix all workspace-template image
// references should use. Defaults to ghcr.io/molecule-ai (the upstream OSS
// face) and is overridden by the MOLECULE_IMAGE_REGISTRY env var in
// production tenants where we mirror images to a private registry.
//
// The override is set at deploy time (Railway env, EC2 user-data) — never
// from user-supplied input — so the value is trusted by the time it reaches
// this code. Validation is deliberately minimal: an operator-supplied
// prefix that points at a registry the EC2 can't authenticate to will fail
// loudly at docker-pull time, which is the right blast radius.
//
// Example values:
//
//	(unset)                                              → ghcr.io/molecule-ai (OSS default)
//	"123456789012.dkr.ecr.us-east-2.amazonaws.com/molecule-ai"  → AWS ECR mirror
//	"git.moleculesai.app/molecule-ai"                    → self-hosted Gitea Container Registry (future)
//
// Auth is registry-specific and configured outside this function:
//   - GHCR: GHCR_USER/GHCR_TOKEN env vars consumed by ghcrAuthHeader()
//   - ECR: docker credential helper (amazon-ecr-credential-helper) configured
//     in EC2 user-data; ~/.docker/config.json has credHelpers entry; the
//     daemon resolves auth automatically on every pull.
func RegistryPrefix() string {
	if v := os.Getenv("MOLECULE_IMAGE_REGISTRY"); v != "" {
		return v
	}
	return defaultRegistryPrefix
}

// RuntimeImage returns the canonical image reference for the given runtime,
// using the current RegistryPrefix() and the moving `:latest` tag.
//
// For SHA-pinned references (production thin-AMI launches), the
// runtime_image_pins lookup in handlers/runtime_image_pin.go strips the
// `:latest` suffix and appends an immutable `@sha256:<digest>` from the DB.
// That code path naturally inherits any RegistryPrefix() change because it
// reads from RuntimeImages[runtime] and only re-formats the tag suffix.
//
// Returns the empty string for unknown runtimes; callers should fall through
// to DefaultImage in that case (matching legacy behavior).
func RuntimeImage(runtime string) string {
	for _, r := range knownRuntimes {
		if r == runtime {
			return fmt.Sprintf("%s/workspace-template-%s:latest", RegistryPrefix(), runtime)
		}
	}
	return ""
}

// computeRuntimeImages returns the {runtime: image-ref} map evaluated against
// the current RegistryPrefix(). Called at package init to populate the
// exported RuntimeImages var. Tests that flip MOLECULE_IMAGE_REGISTRY between
// expected values use this helper to rebuild the map mid-run.
func computeRuntimeImages() map[string]string {
	out := make(map[string]string, len(knownRuntimes))
	for _, r := range knownRuntimes {
		out[r] = RuntimeImage(r)
	}
	return out
}
