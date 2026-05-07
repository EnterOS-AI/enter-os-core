package provisioner

import "os"

// localImagePrefix is the synthetic registry hostname used for images
// that the local-build path produces. It is intentionally NOT a real
// hostname — Docker won't try to pull it from the network (no DNS
// resolution path), and the workspace-image-refresh / image-watch
// paths short-circuit on it.
//
// Tag scheme: `molecule-local/workspace-template-<runtime>:<tag>` where
// `<tag>` is either the 12-char Gitea HEAD sha for SHA-pinned references
// or the moving `:latest` for human inspection (the provisioner
// consumes the SHA-pinned form via EnsureLocalImage()).
//
// Issue #63 / Task #194.
const localImagePrefix = "molecule-local"

// RegistryMode classifies how the provisioner sources workspace-template
// container images. The two modes are mutually exclusive and selected
// by presence/absence of the MOLECULE_IMAGE_REGISTRY env var (Q2 design
// lock, 2026-05-07): set ⇒ SaaS-mode pull; unset ⇒ local-build mode.
//
// Discriminated value rather than a bare string return so every call
// site that decides on image source has to acknowledge the two modes —
// a bare string returning `""` on local-mode would silently produce
// malformed image refs (e.g. `/workspace-template-foo:latest`).
type RegistryMode string

const (
	// RegistryModeSaaS — pull workspace-template-* images from a real
	// container registry whose URL is in `MOLECULE_IMAGE_REGISTRY`.
	// Used by every prod tenant (env injected via Railway / EC2
	// user-data) and any self-hosted operator who has mirrored the
	// images to their own GHCR/ECR/Harbor.
	RegistryModeSaaS RegistryMode = "saas"

	// RegistryModeLocal — clone the workspace-template-<runtime> repo
	// from Gitea
	// (`https://git.moleculesai.app/molecule-ai/molecule-ai-workspace-template-<runtime>`)
	// and `docker build` the image locally. Used by OSS contributors
	// who run `go run ./workspace-server/cmd/server` without setting
	// MOLECULE_IMAGE_REGISTRY. Closes the post-2026-05-06 GHCR-403 gap
	// (Task #194 / Issue #63).
	RegistryModeLocal RegistryMode = "local"
)

// RegistrySource is the SSOT for image-resolution decisions. Returned
// by Resolve(); read by:
//   - the provisioner Start() path — branches on Mode for clone+build
//     vs pull
//   - admin_workspace_images.go — skips remote pull in local mode
//   - imagewatch.Watcher — short-circuits in local mode (no GHCR poll)
//
// SaaS-mode .Prefix matches the existing RegistryPrefix() return value;
// local-mode .Prefix is the synthetic `molecule-local`.
type RegistrySource struct {
	Mode   RegistryMode
	Prefix string
}

// Resolve inspects the runtime environment and returns the image-source
// classification. Treats both unset AND empty-string MOLECULE_IMAGE_REGISTRY
// as "local mode" — an operator who set the var to "" via a misconfigured
// deploy would otherwise silently get malformed image refs in SaaS-mode;
// instead they get the local-build path, which fails loudly if the host
// has no Docker daemon (better blast radius).
//
// Mirrors the existing RegistryPrefix() empty-string handling, so the two
// functions agree on every input.
func Resolve() RegistrySource {
	if v := os.Getenv("MOLECULE_IMAGE_REGISTRY"); v != "" {
		return RegistrySource{Mode: RegistryModeSaaS, Prefix: v}
	}
	return RegistrySource{Mode: RegistryModeLocal, Prefix: localImagePrefix}
}

// IsKnownRuntime reports whether the given runtime name is in the
// canonical knownRuntimes list. Exposed so the local-build path can
// refuse to clone arbitrary repo paths supplied via cfg.Runtime —
// defence-in-depth against a future code path that might let an
// attacker influence the runtime string before it reaches the build
// code.
func IsKnownRuntime(runtime string) bool {
	for _, r := range knownRuntimes {
		if r == runtime {
			return true
		}
	}
	return false
}

// LocalImagePrefix returns the synthetic registry hostname used by the
// local-build path. Exposed so handlers that need to branch on "is
// this a local-built image?" don't have to duplicate the constant.
func LocalImagePrefix() string { return localImagePrefix }
