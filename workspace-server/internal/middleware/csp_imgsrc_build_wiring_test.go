package middleware

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// csp_imgsrc_build_wiring_test.go — guards that the tenant IMAGE BUILD actually
// wires the exact generated-image R2 host into the canvas CSP img-src pin
// (NEXT_PUBLIC_IMAGE_GEN_R2_HOST), not just that the emitters SUPPORT a pin.
//
// Why this lives here: securityheaders_test.go proves the Go emitter renders the
// pinned host when MOLECULE_IMAGE_GEN_R2_HOST is set at runtime, and the canvas
// vitest suite proves buildImgSrc() renders the pinned host when
// NEXT_PUBLIC_IMAGE_GEN_R2_HOST is set. But NEXT_PUBLIC_* is inlined by Next.js
// at BUILD time — if the Dockerfile / CI never set it, the deployed tenant UI
// silently emits the wildcard regardless of the (correct) emitter logic. These
// tests are the missing link: they assert the build plumbing exists.
//
// Follow-up to #3128 (which added the pin support + the wildcard default).

// exactPinnedR2Host is the derived prod/staging generated-image origin:
//
//	<MOLECULE_IMAGE_GEN_BUCKET=molecule-workspace-data>.<MOLECULE_IMAGE_GEN_ENDPOINT
//	host = bfa4e604e168a938e565600b27e2828c.r2.cloudflarestorage.com>
//
// prod and staging share the R2 bucket + Cloudflare account, so this single
// value is correct for both (no cross-env mismatch). It is the CI default when
// the IMAGE_GEN_R2_HOST repo variable is unset.
const exactPinnedR2Host = "https://molecule-workspace-data.bfa4e604e168a938e565600b27e2828c.r2.cloudflarestorage.com"

// repoRoot walks up from the test's working directory (the package dir) to the
// molecule-core repo root by locating the canvas/ dir + .gitea/ dir. Returns ""
// (and the test skips) if not found — keeps the test robust to vendored / split
// checkouts where the repo tree isn't fully present.
func repoRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for i := 0; i < 8; i++ {
		if fi, err := os.Stat(filepath.Join(dir, "workspace-server", "Dockerfile.tenant")); err == nil && !fi.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// TestTenantDockerfile_PinsImageGenR2Host asserts Dockerfile.tenant's canvas
// build stage declares BOTH the ARG and the ENV for NEXT_PUBLIC_IMAGE_GEN_R2_HOST
// before `npm run build`, so the value (passed as a build-arg by CI) is inlined
// into the canvas bundle's CSP at build time. Without the ENV line the ARG is
// inert and the bundle ships the wildcard.
func TestTenantDockerfile_PinsImageGenR2Host(t *testing.T) {
	root := repoRoot()
	if root == "" {
		t.Skip("repo root not found from test cwd; skipping build-wiring guard")
	}
	path := filepath.Join(root, "workspace-server", "Dockerfile.tenant")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	df := string(b)

	if !strings.Contains(df, "ARG NEXT_PUBLIC_IMAGE_GEN_R2_HOST") {
		t.Errorf("Dockerfile.tenant missing `ARG NEXT_PUBLIC_IMAGE_GEN_R2_HOST` in the canvas build stage")
	}
	if !strings.Contains(df, "ENV NEXT_PUBLIC_IMAGE_GEN_R2_HOST=$NEXT_PUBLIC_IMAGE_GEN_R2_HOST") {
		t.Errorf("Dockerfile.tenant missing `ENV NEXT_PUBLIC_IMAGE_GEN_R2_HOST=$NEXT_PUBLIC_IMAGE_GEN_R2_HOST` — the ARG is inert without it (Next.js inlines the ENV at build time)")
	}

	// The ENV must be set BEFORE `npm run build` or Next.js won't inline it.
	envIdx := strings.Index(df, "ENV NEXT_PUBLIC_IMAGE_GEN_R2_HOST=")
	buildIdx := strings.LastIndex(df, "RUN npm run build")
	if envIdx < 0 || buildIdx < 0 || envIdx > buildIdx {
		t.Errorf("Dockerfile.tenant must set NEXT_PUBLIC_IMAGE_GEN_R2_HOST before `RUN npm run build` (env=%d build=%d)", envIdx, buildIdx)
	}
}

// TestPublishWorkflow_PassesExactImageGenR2Host asserts the tenant-image publish
// workflow passes the exact (non-wildcard) host as the canvas build-arg, with
// the production-derived host as the default. This is what makes the deployed
// prod + staging tenant UIs emit the EXACT host instead of the wildcard.
func TestPublishWorkflow_PassesExactImageGenR2Host(t *testing.T) {
	root := repoRoot()
	if root == "" {
		t.Skip("repo root not found from test cwd; skipping CI-wiring guard")
	}
	path := filepath.Join(root, ".gitea", "workflows", "publish-workspace-server-image.yml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("publish workflow not present at %s (split checkout?): %v", path, err)
	}
	wf := string(b)

	if !strings.Contains(wf, "--build-arg NEXT_PUBLIC_IMAGE_GEN_R2_HOST=") {
		t.Errorf("publish-workspace-server-image.yml does not pass --build-arg NEXT_PUBLIC_IMAGE_GEN_R2_HOST to the tenant build")
	}
	if !strings.Contains(wf, exactPinnedR2Host) {
		t.Errorf("publish-workspace-server-image.yml missing the exact default host %q (must be the non-wildcard prod/staging origin)", exactPinnedR2Host)
	}
	// Defense: the default baked into CI must NOT be the wildcard.
	if strings.Contains(wf, "IMAGE_GEN_R2_HOST: ${{ vars.IMAGE_GEN_R2_HOST || 'https://*.r2.cloudflarestorage.com'") {
		t.Errorf("publish workflow defaults the build-arg to the wildcard — defeats the pin")
	}
}
