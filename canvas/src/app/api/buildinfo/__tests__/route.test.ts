/**
 * Canvas /api/buildinfo — version-display endpoint mirroring
 * workspace-server's /buildinfo. Lets `curl <url>/api/buildinfo`
 * confirm which git SHA is live on a canvas deployment (core#2235).
 */
import { describe, it, expect, beforeEach, afterEach } from "vitest";
import { GET } from "../route";

const ENV_KEYS = [
  "BUILD_SHA",
  "VERCEL_GIT_COMMIT_SHA",
  "VERCEL_GIT_COMMIT_REF",
  "VERCEL_ENV",
];

describe("GET /api/buildinfo", () => {
  let saved: Record<string, string | undefined>;

  beforeEach(() => {
    saved = Object.fromEntries(ENV_KEYS.map((k) => [k, process.env[k]]));
    for (const k of ENV_KEYS) delete process.env[k];
  });

  afterEach(() => {
    for (const k of ENV_KEYS) {
      if (saved[k] === undefined) delete process.env[k];
      else process.env[k] = saved[k];
    }
  });

  it("returns dev sentinel when no SHA source is set", async () => {
    const res = await GET();
    const body = await res.json();
    expect(body).toEqual({ git_sha: "dev", git_ref: "", vercel_env: "local" });
  });

  it("reports BUILD_SHA baked into the Docker image (fleet deploy path)", async () => {
    // BUILD_SHA is the authoritative source for the ECR-image fleet deploy,
    // which never runs on Vercel. It must win even when a Vercel var is also
    // present in the environment.
    process.env.BUILD_SHA = "deadbeefcafe";
    process.env.VERCEL_GIT_COMMIT_SHA = "should-not-win";
    const res = await GET();
    const body = await res.json();
    expect(body.git_sha).toBe("deadbeefcafe");
  });

  it("falls back to the SHA Vercel injected when BUILD_SHA is unset", async () => {
    process.env.VERCEL_GIT_COMMIT_SHA = "abc1234567890";
    process.env.VERCEL_GIT_COMMIT_REF = "main";
    process.env.VERCEL_ENV = "production";
    const res = await GET();
    const body = await res.json();
    expect(body.git_sha).toBe("abc1234567890");
    expect(body.git_ref).toBe("main");
    expect(body.vercel_env).toBe("production");
  });

  it("returns 200 status and JSON content type", async () => {
    const res = await GET();
    expect(res.status).toBe(200);
    expect(res.headers.get("content-type")).toContain("application/json");
  });
});
