// Package buildinfo exposes the git SHA the binary was built from.
//
// Set at link time:
//
//	go build -ldflags "-X github.com/Molecule-AI/molecule-monorepo/platform/internal/buildinfo.GitSHA=<sha>"
//
// CI passes ${{ github.sha }} via Dockerfile.tenant ARG GIT_SHA; local
// dev builds default to "dev" so unset never reads as success.
//
// Why this package exists: redeploy-fleet (CP) returns ssm_status=Success
// when the SSM RPC didn't error — that's "the deploy command ran",
// NOT "the new code is running on every tenant." Image-tag-as-tag
// (`:latest`) caches in the local Docker daemon so `docker compose up -d`
// without an explicit `docker pull` is a no-op when the tag hasn't been
// invalidated. Both observed 2026-04-30: the user's tenant kept serving
// pre-501a42d7 chat_files even after main published the lazy-heal fix
// (#2395). Exposing GitSHA at /buildinfo lets the redeploy workflow
// verify EVERY tenant is actually running the published SHA before
// reporting success.
package buildinfo

// GitSHA is overwritten at build time via -ldflags. Default catches
// dev builds + any deploy that forgot to wire the build-arg through.
// "dev" is intentional — comparing it to a real SHA always fails,
// which is what we want for an unconfigured deploy.
var GitSHA = "dev"
