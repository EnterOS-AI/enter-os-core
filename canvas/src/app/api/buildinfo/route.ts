import { NextResponse } from "next/server";

// Mirror of workspace-server's GET /buildinfo (PR #2398). Lets a developer
// or the fleet redeploy workflow confirm which git SHA is live on a canvas
// deployment with the same `curl <url>/api/buildinfo` flow used against
// tenant workspaces (core#2235; cross-ref core#2226).
//
// SHA source, in priority order:
//   1. BUILD_SHA — server-only env baked into the canvas Docker image at
//      build time (Dockerfile `ARG BUILD_SHA` → `ENV BUILD_SHA`, wired
//      from `${{ github.sha }}` in publish-canvas-image.yml). This is the
//      authoritative source for the fleet's ECR-image deploy path, which
//      does NOT run on Vercel. Read server-side here (App Router route
//      handler runs on the standalone Node server, `output: "standalone"`),
//      so it is intentionally NOT a NEXT_PUBLIC_ var — keeping it out of
//      the client bundle.
//   2. VERCEL_GIT_COMMIT_SHA — Vercel injects this at build time when the
//      canvas is deployed via Vercel rather than the Docker image.
//   3. "dev" — local `next dev` / test harness, where neither is set. Same
//      sentinel workspace-server uses pre-ldflags-injection, so both
//      surfaces speak the same vocabulary and an unconfigured deploy
//      fails the SHA comparison closed instead of round-tripping "".
//
// force-dynamic so the response is evaluated at request time against the
// runtime env of the standalone server (where ENV BUILD_SHA lives), not
// frozen into a static asset at `next build`.
export const dynamic = "force-dynamic";

export async function GET() {
  const sha =
    process.env.BUILD_SHA ?? process.env.VERCEL_GIT_COMMIT_SHA ?? "dev";
  return NextResponse.json({
    git_sha: sha,
    git_ref: process.env.VERCEL_GIT_COMMIT_REF ?? "",
    vercel_env: process.env.VERCEL_ENV ?? "local",
  });
}
