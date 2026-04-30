import { NextResponse } from "next/server";

// Mirror of workspace-server's GET /buildinfo (PR #2398). Lets a developer
// confirm which git SHA is live on a canvas deployment with the same
// `curl <url>/buildinfo` flow they use against tenant workspaces.
//
// Vercel injects VERCEL_GIT_COMMIT_SHA / _REF / VERCEL_ENV at build time
// from the deploying commit; outside Vercel (local `next dev`, harness)
// these are unset and the endpoint reports `git_sha: "dev"`. Same sentinel
// the workspace-server uses pre-ldflags-injection so both surfaces speak
// the same vocabulary.
export async function GET() {
  return NextResponse.json({
    git_sha: process.env.VERCEL_GIT_COMMIT_SHA ?? "dev",
    git_ref: process.env.VERCEL_GIT_COMMIT_REF ?? "",
    vercel_env: process.env.VERCEL_ENV ?? "local",
  });
}
