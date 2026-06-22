package provisioner

// platform_agent_image_drift_test.go — CI DRIFT-GATE for the
// IMAGE-BAKED platform-agent identity (RFC #2843 §10a).
//
// The IMAGE-BAKED impl (workspace-server/Dockerfile.platform-agent)
// bakes the concierge's identity (config.yaml +
// prompts/concierge.md + mcp_servers.yaml + identity-fallback.sh)
// from the platform-agent TEMPLATE REPO into the platform-agent
// image at /opt/molecule-platform-agent-template/. The driver
// hard-requirement:
// "The image-baked config.yaml + prompts/concierge.md +
// mcp_servers.yaml MUST be SOURCED FROM the platform-agent TEMPLATE
// REPO (single SSOT = PR #1's content) — NOT vendored/duplicated in
// core."
//
// A future drift — e.g., someone edits config.yaml in core, or the
// pre-clone step points at the wrong dir, or a build-arg change
// reroutes the source — would silently create a 2-SSOT situation
// (image snapshot diverges from template repo). The driver-rejected
// option (b) MINIMAL IN-CORE FALLBACK was rejected EXPLICITLY
// because of this 2-SSOT drift risk; the IMAGE-BAKED impl survives
// only because the drift-gate closes that risk.
//
// The drift-gate (this test) has TWO halves:
//
//  1. Dockerfile-side checks (ALWAYS RUN, no SSOT needed): pin the
//     Dockerfile's COPY instructions, build-arg declaration, and
//     destination path. Catches a regression in the Dockerfile that
//     re-introduces vendored/duplicated content or breaks the build-
//     arg contract. These are cheap (file-read only) and run on
//     every CI lane, including pull_request where the SSOT may not
//     be pre-cloned.
//
//  2. SSOT-side checks (RUN WHEN SSOT AVAILABLE): byte-equal content
//     between the pre-cloned template repo and the would-be image-
//     baked paths that Dockerfile COPYs. Requires the platform-agent
//     template to be pre-cloned (via scripts/clone-manifest.sh from
//     manifest.json's workspace_templates entry, OR the operator-
//     override env var). Skipped with a t.Logf note when the SSOT
//     is not available — pull_request CI doesn't pre-clone (that's
//     the publish-workspace-server-image.yml workflow's job), and
//     we don't want a missing pre-clone to fail this lane.
//
// How to run: `go test -run TestPlatformAgentImageDriftGate
// ./internal/provisioner/`. Set PLATFORM_AGENT_TEMPLATE_REPO_PATH
// to the pre-cloned template dir to enable the SSOT-side checks
// (the publish-workspace-server-image.yml workflow does this via
// the post-pre-clone test step).
//
// Test scope: the 4 files the Dockerfile COPYs (config.yaml,
// mcp_servers.yaml, prompts/concierge.md, identity-fallback.sh).
// A future concierge-identity change that adds a new file MUST also
// extend the expectedImageBakedFiles list here; the Dockerfile-side
// check catches the missing COPY, and the SSOT-side check (when
// run) catches the missing identity file in the template repo.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// expectedImageBakedFiles is the canonical list of files the
// IMAGE-BAKED impl bakes into the platform-agent image. The list
// MUST match Dockerfile.platform-agent's COPY instructions exactly.
// Adding a new concierge-identity file = adding it here AND in the
// Dockerfile; the test fails if either side drifts.
//
// Paths are RELATIVE to the SSOT root (the platform-agent template
// repo). The Dockerfile's PLATFORM_AGENT_TEMPLATE_DIR build-arg
// points at this same root.
//
// The "identity-fallback.sh" entry is the boot-time per-file copy
// script (template-platform-agent #2, copied into the image and
// invoked from the platform-agent entrypoint). It's a 1st-class
// IMAGE-BAKED asset (NOT metadata / not a future change) — the
// runtime /opt→/configs fallback (workspace-runtime PR #141
// load_config) and the boot-time /opt→/configs fallback (this
// Dockerfile's entrypoint) are complementary, and BOTH need the
// image-baked copy at /opt/.../identity-fallback.sh in the build
// to close the self-host + pre-#29-bootstrap window. Listed here
// so the SSOT-side check rejects a template-repo that ships the
// script (correctly, in the platform-agent template) without the
// matching Dockerfile COPY (regression).
var expectedImageBakedFiles = []string{
	"config.yaml",
	"mcp_servers.yaml",
	"prompts/concierge.md",
	"identity-fallback.sh",
}

// isConciergeIdentityPath reports whether a path in the platform-agent
// template repo is part of the concierge's IDENTITY (the set of
// files the IMAGE-BAKED impl should COPY into the image). A file
// outside this namespace (e.g. README.md, .gitignore) is
// documentation / metadata and is correctly EXCLUDED from the
// image-baked content.
//
// Namespace mirrors the template-asset allowlist in
// internal/provisioner/template_assets.go (IsCPTemplateAssetPath):
//   - "config.yaml"        — runtime entrypoint config
//   - "mcp_servers.yaml"   — MCP wiring (overlay)
//   - "prompts/*"          — system prompts
//   - "identity-fallback.sh" — boot-time /opt→/configs copy script
//                              (template-platform-agent #2, invoked
//                              from the platform-agent entrypoint)
//
// A future RFC that adds a new namespace (e.g. "hooks/*") MUST
// extend this function AND the Dockerfile AND expectedImageBakedFiles
// in lockstep. The drift-gate's value is in the lockstep invariant.
func isConciergeIdentityPath(rel string) bool {
	rel = filepath.ToSlash(filepath.Clean(rel))
	return rel == "config.yaml" ||
		rel == "mcp_servers.yaml" ||
		rel == "identity-fallback.sh" ||
		strings.HasPrefix(rel, "prompts/")
}

// hasDockerfileCopyForRel reports whether Dockerfile.platform-agent contains
// a COPY instruction for the expected IMAGE-BAKED file `rel` (relative to the
// platform-agent template SSOT root). The Dockerfile uses two patterns:
//
//   - COPY ${PLATFORM_AGENT_TEMPLATE_DIR}/<rel> ...   for top-level files
//     (config.yaml, mcp_servers.yaml, identity-fallback.sh).
//   - COPY ${PLATFORM_AGENT_TEMPLATE_DIR}/<dir>/ ...  for directory-baked
//     content (prompts/concierge.md is shipped via the prompts/ dir copy).
//
// COPY instructions may also carry Dockerfile flags such as
// `--chmod=0755` before the source path, so the matcher permits an
// optional flag segment between `COPY` and the source path.
//
// This helper centralises the pattern matching so the test body stays readable
// and the two valid COPY shapes are documented in one place.
func hasDockerfileCopyForRel(dockerfileStr, rel string) bool {
	rel = filepath.ToSlash(filepath.Clean(rel))
	relRe := regexp.QuoteMeta(rel)
	dirRe := regexp.QuoteMeta(filepath.Dir(rel) + "/")

	// Match: COPY [flags] ${PLATFORM_AGENT_TEMPLATE_DIR}/<rel> ...
	// or:    COPY [flags] ${PLATFORM_AGENT_TEMPLATE_DIR}/<dir>/ ...
	// Flags are zero or more `--flag[=value]` tokens (e.g. --chmod=0755,
	// --chown=app:app, --chown=1000:1000) before the source path.
	pattern := `(?m)^COPY(?:\s+--\S+)*\s+\$\{PLATFORM_AGENT_TEMPLATE_DIR\}/(?:` + relRe + `|` + dirRe + `)\s`
	matched, err := regexp.MatchString(pattern, dockerfileStr)
	if err != nil {
		// regexp.QuoteMeta only produces safe patterns; a compile error
		// here is a test-authoring bug, not a product failure.
		panic("invalid hasDockerfileCopyForRel pattern: " + err.Error())
	}
	return matched
}

func TestHasDockerfileCopyForRel(t *testing.T) {
	tests := []struct {
		name        string
		dockerfile  string
		rel         string
		wantMatched bool
	}{
		{
			name:        "top-level file COPY",
			dockerfile:  "COPY ${PLATFORM_AGENT_TEMPLATE_DIR}/config.yaml /opt/molecule-platform-agent-template/config.yaml\n",
			rel:         "config.yaml",
			wantMatched: true,
		},
		{
			name:        "top-level file COPY with --chmod",
			dockerfile:  "COPY --chmod=0755 ${PLATFORM_AGENT_TEMPLATE_DIR}/identity-fallback.sh /opt/molecule-platform-agent-template/identity-fallback.sh\n",
			rel:         "identity-fallback.sh",
			wantMatched: true,
		},
		{
			name:        "top-level file COPY with --chown",
			dockerfile:  "COPY --chown=1000:1000 ${PLATFORM_AGENT_TEMPLATE_DIR}/identity-fallback.sh /opt/molecule-platform-agent-template/identity-fallback.sh\n",
			rel:         "identity-fallback.sh",
			wantMatched: true,
		},
		{
			name:        "top-level file COPY with multiple flags",
			dockerfile:  "COPY --chmod=0755 --chown=node:node ${PLATFORM_AGENT_TEMPLATE_DIR}/identity-fallback.sh /opt/molecule-platform-agent-template/identity-fallback.sh\n",
			rel:         "identity-fallback.sh",
			wantMatched: true,
		},
		{
			name:        "directory COPY for nested file",
			dockerfile:  "COPY ${PLATFORM_AGENT_TEMPLATE_DIR}/prompts/ /opt/molecule-platform-agent-template/prompts/\n",
			rel:         "prompts/concierge.md",
			wantMatched: true,
		},
		{
			name:        "missing COPY",
			dockerfile:  "RUN echo no-copy\n",
			rel:         "config.yaml",
			wantMatched: false,
		},
		{
			name:        "wrong source variable",
			dockerfile:  "COPY ${OTHER_DIR}/config.yaml /opt/molecule-platform-agent-template/config.yaml\n",
			rel:         "config.yaml",
			wantMatched: false,
		},
		{
			name:        "nested file missing directory COPY",
			dockerfile:  "COPY ${PLATFORM_AGENT_TEMPLATE_DIR}/prompts/concierge.md /opt/molecule-platform-agent-template/prompts/concierge.md\n",
			rel:         "prompts/concierge.md",
			wantMatched: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hasDockerfileCopyForRel(tt.dockerfile, tt.rel)
			if got != tt.wantMatched {
				t.Errorf("hasDockerfileCopyForRel(%q, %q) = %v, want %v", tt.dockerfile, tt.rel, got, tt.wantMatched)
			}
		})
	}
}

// canonicalPlatformAgentSSOTRelPath is the default SSOT path the
// drift-gate reads from when PLATFORM_AGENT_TEMPLATE_REPO_PATH is
// unset, RELATIVE TO THE REPO ROOT. It mirrors Dockerfile.platform-
// agent's default PLATFORM_AGENT_TEMPLATE_DIR build-arg (i.e. where
// scripts/clone-manifest.sh places the platform-agent template repo
// after the pre-clone step in publish-workspace-server-image.yml).
//
// The env-var override exists for operators running the test
// outside the canonical CI context (e.g. an ad-hoc build verifying
// the drift-gate against a custom template mirror). When the env
// var is set, the test uses that path verbatim; otherwise it walks
// up from the test's CWD to find the repo root and resolves the
// canonical path from there.
//
// The drift-gate is CWD-AGNOSTIC: the test runs from the package
// dir (workspace-server/internal/provisioner/) which is two levels
// below the repo root, so the walk-up is necessary. This is the
// standard pattern for Go tests that need a repo-rooted fixture.
const canonicalPlatformAgentSSOTRelPath = ".tenant-bundle-deps/workspace-configs-templates/platform-agent"

// repoRoot walks up from the test's CWD until it finds the
// molecule-core repo root (identified by go.mod at workspace-server/
// go.mod or by the presence of manifest.json — the molecule-core
// root marker). Returns the absolute path to the repo root.
//
// Used by the drift-gate to resolve canonicalPlatformAgentSSOTRelPath
// to an absolute path regardless of where the test was invoked
// from. Bounded walk-up (max 10 levels) prevents an infinite loop
// if the test somehow runs from a path that doesn't contain a
// molecule-core repo above it.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	dir := wd
	for i := 0; i < 10; i++ {
		// The canonical repo-root marker: manifest.json (present
		// only at the molecule-core repo root, not in any submodule
		// or vendored copy). workspace-server/go.mod is a weaker
		// signal — it's also present in nested test fixtures.
		if _, err := os.Stat(filepath.Join(dir, "manifest.json")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("could not locate molecule-core repo root from CWD %q (walked up 10 levels; expected manifest.json in some ancestor)", wd)
	return ""
}

// resolveSSOTRoot returns the absolute path to the platform-agent
// template SSOT. The order is: (1) $PLATFORM_AGENT_TEMPLATE_REPO_PATH
// (operator override), (2) canonical CI path (canonicalPlatformAgentSSOTRelPath
// resolved against repoRoot). Returns "" if neither resolves; the
// caller treats that as "SSOT not available, skip SSOT-side checks".
//
// A nil error with a non-empty path means the path EXISTS and is
// readable. A non-nil error means the path doesn't exist (caller
// may choose to skip or fail depending on lane). We deliberately do
// NOT fatal here — the split-half design lets the test run Dockerfile-
// only checks when the SSOT is unavailable.
func resolveSSOTRoot(t *testing.T) (path string, available bool) {
	t.Helper()
	ssotRoot := os.Getenv("PLATFORM_AGENT_TEMPLATE_REPO_PATH")
	if ssotRoot == "" {
		ssotRoot = filepath.Join(repoRoot(t), canonicalPlatformAgentSSOTRelPath)
	}
	if _, err := os.Stat(ssotRoot); err != nil {
		return "", false
	}
	return ssotRoot, true
}

// TestPlatformAgentImageDriftGate pins the IMAGE-BAKED ↔ template
// SSOT invariant. The test has TWO halves:
//
//  1. Dockerfile-side checks (ALWAYS RUN, even without SSOT):
//     pins Dockerfile COPY instructions + build-arg + destination
//     path. Catches any regression in the Dockerfile that
//     re-introduces vendored/duplicated content or breaks the
//     build-arg contract. These run on every CI lane, including
//     pull_request.
//
//  2. SSOT-side checks (RUN WHEN SSOT AVAILABLE): byte-equal
//     content between the pre-cloned template repo and the
//     would-be image-baked paths. Requires the platform-agent
//     template to be pre-cloned (via scripts/clone-manifest.sh
//     from manifest.json's workspace_templates entry, OR the
//     operator-override env var). Skipped with a t.Logf note
//     when the SSOT is not available — pull_request CI doesn't
//     pre-clone (that's the publish-workspace-server-image.yml
//     workflow's job), and we don't want a missing pre-clone
//     to fail this lane.
//
// This split-half design lets the test serve as BOTH:
//   - a CHEAP Dockerfile-shape gate that runs on every PR (catches
//     "someone vendored the config into core"); AND
//   - a FULL SSOT-content gate that runs on the publish workflow
//     (catches "image-baked content drifted from template repo").
func skipIfPlatformAgentImageRemoved(t *testing.T) {
	dockerfilePath := filepath.Join("..", "..", "Dockerfile.platform-agent")
	if _, err := os.Stat(dockerfilePath); os.IsNotExist(err) {
		t.Skipf("%s was removed in #3027 (core no longer builds the platform-agent image); this drift-gate is stale and will be removed once the permanent cleanup PR lands", dockerfilePath)
	}
}

func TestPlatformAgentImageDriftGate(t *testing.T) {
	skipIfPlatformAgentImageRemoved(t)

	// === Half 1: Dockerfile-side checks (always run) ===

	dockerfilePath := filepath.Join("..", "..", "Dockerfile.platform-agent")
	dockerfile, err := os.ReadFile(dockerfilePath)
	if err != nil {
		// #3027 moved the platform-agent image build (and Dockerfile.platform-agent)
		// OUT of core into molecule-ai-workspace-template-claude-code, and
		// rfc-platform-mcp-as-plugin retires the baked-image identity path in favor
		// of delivering the management MCP as a plugin. This core-resident drift
		// gate therefore has nothing to read; the SSOT-integrity check it performed
		// now belongs in the template repo's CI. SKIP (not fatal) so the gate stops
		// red-blocking every workspace-server PR; tracked for re-homing/removal.
		if os.IsNotExist(err) {
			t.Skipf("Dockerfile.platform-agent not in core (moved to template repo in #3027; baked-image path retired per rfc-platform-mcp-as-plugin) — drift gate re-homes to the template repo")
		}
		t.Fatalf("read %s: %v", dockerfilePath, err)
	}
	dockerfileStr := string(dockerfile)

	for _, rel := range expectedImageBakedFiles {
		if !hasDockerfileCopyForRel(dockerfileStr, rel) {
			t.Errorf("Dockerfile COPY missing: %s — the IMAGE-BAKED impl must COPY %s from the platform-agent template SSOT; if a new identity file is added, update Dockerfile.platform-agent AND expectedImageBakedFiles", rel, rel)
		}
	}

	// ALSO verify the Dockerfile references the build-arg + the
	// destination path. A future refactor that changes either of
	// these would silently break the SSOT contract; the test pins
	// the names that the workspace-server's runtime fallback (and
	// any operator inspecting the image) relies on.
	if !strings.Contains(dockerfileStr, "ARG PLATFORM_AGENT_TEMPLATE_DIR=") {
		t.Error("Dockerfile.platform-agent is missing the PLATFORM_AGENT_TEMPLATE_DIR build-arg declaration — the IMAGE-BAKED impl requires this arg to source from the pre-cloned template repo")
	}
	if !strings.Contains(dockerfileStr, "/opt/molecule-platform-agent-template/") {
		t.Error("Dockerfile.platform-agent is missing the /opt/molecule-platform-agent-template/ destination path — the workspace-server runtime fallback (and the drift-gate convention) pins this path; a change requires a coordinated update in both places")
	}

	// === Half 2: SSOT-side checks (conditional on SSOT availability) ===

	ssotRoot, available := resolveSSOTRoot(t)
	if !available {
		// SSOT not pre-cloned (typical for pull_request CI). Run
		// the Dockerfile-side checks only; the SSOT-side checks
		// will run on the publish-workspace-server-image.yml
		// workflow which pre-clones via scripts/clone-manifest.sh.
		t.Logf("platform-agent template SSOT not available at canonical CI path (PLATFORM_AGENT_TEMPLATE_REPO_PATH unset, .tenant-bundle-deps/workspace-configs-templates/platform-agent missing). Dockerfile-side checks ran; SSOT-side checks SKIPPED. Set PLATFORM_AGENT_TEMPLATE_REPO_PATH to the pre-cloned template dir to enable the full gate (the publish-workspace-server-image.yml workflow does this via the post-pre-clone test step).")
		return
	}

	// SSOT-side: each expected file MUST exist at ssotRoot/<relpath>
	// and have non-zero content (zero-byte file = silent miss).
	for _, rel := range expectedImageBakedFiles {
		ssotPath := filepath.Join(ssotRoot, rel)
		data, err := os.ReadFile(ssotPath)
		if err != nil {
			t.Errorf("SSOT missing: %s (read: %v) — the platform-agent template repo is the load-bearing identity SSOT; a missing file is a regression", ssotPath, err)
			continue
		}
		if len(data) == 0 {
			t.Errorf("SSOT empty: %s — zero-byte identity file would silently bake a broken concierge into the image", ssotPath)
		}
	}

	// SSOT-side: scan the platform-agent template repo for any
	// additional files in the concierge-identity namespace (e.g.
	// prompts/foo.md) that the Dockerfile might be missing. The
	// forward-direction check (above) catches a missing expected
	// file; this REVERSE check catches an un-expected new identity
	// file the Dockerfile doesn't COPY. Both must hold for the
	// image-baked content to remain SSOT-equal.
	extraIdentityFiles, err := scanConciergeIdentityFiles(ssotRoot)
	if err != nil {
		t.Errorf("scan SSOT identity files: %v", err)
	} else {
		for _, rel := range extraIdentityFiles {
			found := false
			for _, expected := range expectedImageBakedFiles {
				if rel == expected {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("SSOT has an un-baked concierge-identity file: %s — the IMAGE-BAKED impl is now SILENTLY DRIFTING from the SSOT (a new file was added to the platform-agent template repo without a matching COPY in Dockerfile.platform-agent + entry in expectedImageBakedFiles). Either bake it (update Dockerfile + expected list) or mark it non-identity.", rel)
			}
		}
	}
}

// TestPlatformAgentEntrypointWiring pins the boot-time identity-
// fallback wiring. The IMAGE_BAKED_IDENTITY_PRESENT echo-marker
// that the #2919 PR shipped was a log line that did nothing — a
// partial-template / no-fetch self-host concierge would still
// MISSING_MODEL fail at runtime because /configs would be empty
// even though /opt/molecule-platform-agent-template/ had the
// content. This test pins the WIRE-UP shape that closes the gap:
//
//   1. Dockerfile.platform-agent defines a /entrypoint-platform-agent.sh
//      heredoc that invokes identity-fallback.sh BEFORE handing off
//      to /entrypoint.sh (the base image's entrypoint). The
//      identity-fallback.sh script is the WORKING /opt→/configs
//      fill-absent-only copy from template-platform-agent #2.
//   2. The Dockerfile's ENTRYPOINT directive points at the new
//      /entrypoint-platform-agent.sh (NOT the base image's
//      /entrypoint.sh). Otherwise the wiring is dormant — the
//      fallback would never fire.
//   3. The IMAGE_BAKED_IDENTITY_PRESENT echo-only marker is GONE.
//      A regression that re-adds the echo marker would re-introduce
//      the dormant-fallback bug (script exists but never runs).
//
// Why pin the wiring here (not in a shell-script test): the
// Dockerfile is the source-of-truth for the IMAGE-BAKED impl, and
// the drift-gate already pins the Dockerfile's other shape
// invariants (COPY lines, build-arg, destination path). Adding
// entrypoint-wiring pins to the same file keeps the IMAGE-BAKED
// image contract in a single test surface — operators / reviewers
// reading TestPlatformAgentImageDriftGate see the full contract
// (data + activation), not just the COPY instructions.
//
// A future change that moves the entrypoint to a different
// filename / different invocation order must update this test
// in lockstep. The shape (identity-fallback.sh + /entrypoint.sh
// handoff) is the load-bearing part; the names are conventions.
func TestPlatformAgentEntrypointWiring(t *testing.T) {
	skipIfPlatformAgentImageRemoved(t)

	dockerfilePath := filepath.Join("..", "..", "Dockerfile.platform-agent")
	dockerfile, err := os.ReadFile(dockerfilePath)
	if err != nil {
		// See TestPlatformAgentImageDriftGate: Dockerfile.platform-agent moved
		// out of core (#3027); baked-image path retired (rfc-platform-mcp-as-plugin).
		if os.IsNotExist(err) {
			t.Skipf("Dockerfile.platform-agent not in core (moved to template repo in #3027) — entrypoint-wiring gate re-homes to the template repo")
		}
		t.Fatalf("read %s: %v", dockerfilePath, err)
	}
	dockerfileStr := string(dockerfile)

	// 1. Heredoc-defined entrypoint-platform-agent.sh: must exist,
	//    must invoke identity-fallback.sh, must hand off to
	//    /entrypoint.sh (the base image's entrypoint).
	if !strings.Contains(dockerfileStr, "/entrypoint-platform-agent.sh") {
		t.Errorf("Dockerfile.platform-agent is missing /entrypoint-platform-agent.sh — the platform-agent entrypoint is the load-bearing wire-up that activates the /opt→/configs fallback at boot")
	}
	if !strings.Contains(dockerfileStr, "identity-fallback.sh") {
		t.Errorf("Dockerfile.platform-agent does not reference identity-fallback.sh — the boot-time /opt→/configs fill-absent-only copy script (template-platform-agent #2) is the WORKING fallback that replaces the IMAGE_BAKED_IDENTITY_PRESENT echo-only marker")
	}
	// The hand-off: the new entrypoint must exec /entrypoint.sh
	// (the base image's entrypoint) with the CMD args. A regression
	// that omits the hand-off would skip the docker-socket group
	// setup + memory-plugin sidecar + su-exec /platform boot.
	if !strings.Contains(dockerfileStr, "exec /entrypoint.sh \"$@\"") {
		t.Errorf("Dockerfile.platform-agent entrypoint does not exec /entrypoint.sh \"$@\" — the platform-agent entrypoint must hand off to the base image's entrypoint (docker-socket group setup, memory-plugin sidecar, su-exec /platform); a regression here would skip the base-image boot")
	}

	// 2. ENTRYPOINT directive: must point at the new entrypoint
	//    (NOT the base /entrypoint.sh). The default ENTRYPOINT
	//    (inherited from the base image) is /entrypoint.sh; a
	//    regression that omits the override would activate the
	//    identity-fallback.sh script via COPY but never invoke
	//    it at boot — the dormant-fallback bug.
	if !strings.Contains(dockerfileStr, `ENTRYPOINT ["/entrypoint-platform-agent.sh"]`) {
		t.Errorf(`Dockerfile.platform-agent is missing ENTRYPOINT ["/entrypoint-platform-agent.sh"] — the platform-agent entrypoint override is what activates the identity-fallback at boot; without it the script is COPY'd into the image but never runs`)
	}

	// 3. The IMAGE_BAKED_IDENTITY_PRESENT echo-only marker MUST
	//    be GONE. The marker was a no-op log line that did nothing;
	//    re-introducing it would either (a) replace the
	//    identity-fallback.sh COPY (regression — fallback never
	//    fires) or (b) coexist with the script (which is fine but
	//    leaves a confusing dead file at /opt/.../IMAGE_BAKED_
	//    IDENTITY_PRESENT). Either way it's a regression marker.
	//
	// Pin pattern: a non-comment line that creates the marker
	// file (the original #2919 PR's `RUN echo ... > ...IMAGE_BAKED
	// _IDENTITY_PRESENT` heredoc). A comment that mentions the
	// marker name is fine (documentation); a creation line is a
	// regression. The check requires the marker name to be on a
	// line that ALSO contains a shell-creating token (`>`, `tee`,
	// `cp`, or the start of a `RUN` directive with a heredoc) —
	// this is intentionally a coarse heuristic, not a full
	// Dockerfile parser, but it's tight enough to catch the
	// regression while not flagging the explanatory comment.
	markerCreationRegex := regexp.MustCompile(`(?m)^[^#]*IMAGE_BAKED_IDENTITY_PRESENT[^#]*(>|tee |cp |<<)`)
	if markerCreationRegex.MatchString(dockerfileStr) {
		t.Errorf("Dockerfile.platform-agent still creates the IMAGE_BAKED_IDENTITY_PRESENT echo-only marker — the marker was a no-op log line that did nothing; the identity-fallback.sh script (template-platform-agent #2) is the real working fallback. The marker creation line must be removed when the script is wired in.")
	}
}

// scanConciergeIdentityFiles walks the platform-agent template repo
// and returns the RELATIVE paths of every file in the concierge-
// identity namespace (config.yaml + mcp_servers.yaml +
// identity-fallback.sh + prompts/). Non-identity files (README,
// .gitignore, etc.) are filtered out.
//
// Errors are returned for filesystem-walk failures; the caller turns
// them into a t.Errorf (so other checks still run). The walk is
// deliberately non-recursive beyond the namespace prefix — the
// concierge's identity is config + mcp + fallback-script + prompts,
// nothing nested.
func scanConciergeIdentityFiles(ssotRoot string) ([]string, error) {
	var identity []string
	entries, err := os.ReadDir(ssotRoot)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		// Top-level files: config.yaml, mcp_servers.yaml,
		// identity-fallback.sh
		if !e.IsDir() {
			if isConciergeIdentityPath(e.Name()) {
				identity = append(identity, e.Name())
			}
			continue
		}
		// Directories: scan prompts/
		if e.Name() == "prompts" {
			promptEntries, err := os.ReadDir(filepath.Join(ssotRoot, e.Name()))
			if err != nil {
				return nil, err
			}
			for _, pe := range promptEntries {
				if pe.IsDir() {
					continue
				}
				rel := filepath.ToSlash(filepath.Join(e.Name(), pe.Name()))
				if isConciergeIdentityPath(rel) {
					identity = append(identity, rel)
				}
			}
		}
	}
	return identity, nil
}
