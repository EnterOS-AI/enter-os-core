// @vitest-environment jsdom
import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";

// Tests for platformAuthHeaders — the shared helper extracted in #178
// to consolidate the bearer-token-attach + tenant-slug-attach pattern
// that was previously duplicated across 7 raw-fetch callsites in the
// canvas (uploads + 5 Attachment* components + the api.ts request()
// function).
//
// What we pin here:
//  - Returns a fresh object each call (so callers can mutate without
//    leaking into each other).
//  - Empty result on a non-tenant host with no admin token (the
//    localhost / self-hosted shape).
//  - Bearer attached when NEXT_PUBLIC_ADMIN_TOKEN is set.
//  - X-Molecule-Org-Slug attached when window.location.hostname is a
//    tenant subdomain (<slug>.moleculesai.app).
//  - Both attached when both apply (the production SaaS shape).
//
// Why jsdom: getTenantSlug() reads window.location.hostname. Node-only
// environment yields no window and getTenantSlug returns null
// unconditionally — wouldn't exercise the slug branch.

import { platformAuthHeaders } from "../api";

describe("platformAuthHeaders", () => {
  let originalAdminToken: string | undefined;

  beforeEach(() => {
    originalAdminToken = process.env.NEXT_PUBLIC_ADMIN_TOKEN;
    delete process.env.NEXT_PUBLIC_ADMIN_TOKEN;
  });

  afterEach(() => {
    if (originalAdminToken === undefined) delete process.env.NEXT_PUBLIC_ADMIN_TOKEN;
    else process.env.NEXT_PUBLIC_ADMIN_TOKEN = originalAdminToken;
    // jsdom resets hostname between tests via the @vitest-environment
    // pragma's per-test isolation. No explicit reset needed.
  });

  it("returns an empty object on a non-tenant host with no admin token", () => {
    // jsdom default hostname is "localhost" — not a tenant slug, so
    // getTenantSlug() returns null and no X-Molecule-Org-Slug is added.
    const headers = platformAuthHeaders();
    expect(headers).toEqual({});
  });

  it("attaches Authorization when NEXT_PUBLIC_ADMIN_TOKEN is set", () => {
    process.env.NEXT_PUBLIC_ADMIN_TOKEN = "local-dev-admin";
    const headers = platformAuthHeaders();
    expect(headers).toEqual({ Authorization: "Bearer local-dev-admin" });
  });

  it("does NOT attach Authorization when NEXT_PUBLIC_ADMIN_TOKEN is empty string", () => {
    // Empty-string env is the JS-side shape of `KEY=` in .env.
    // Treating it as unset matches the matched-pair guard in
    // next.config.ts (admin-token-pair.test.ts) — symmetric semantics.
    process.env.NEXT_PUBLIC_ADMIN_TOKEN = "";
    const headers = platformAuthHeaders();
    expect(headers).toEqual({});
  });

  it("attaches X-Molecule-Org-Slug on a tenant subdomain", () => {
    Object.defineProperty(window, "location", {
      value: { hostname: "reno-stars.moleculesai.app" },
      writable: true,
    });
    const headers = platformAuthHeaders();
    expect(headers).toEqual({ "X-Molecule-Org-Slug": "reno-stars" });
  });

  it("attaches both when both apply (production SaaS shape)", () => {
    Object.defineProperty(window, "location", {
      value: { hostname: "reno-stars.moleculesai.app" },
      writable: true,
    });
    process.env.NEXT_PUBLIC_ADMIN_TOKEN = "tenant-bearer";
    const headers = platformAuthHeaders();
    // Pin exact-equality on the full shape — substring/contains
    // assertions would also pass for an extra-header bug.
    expect(headers).toEqual({
      "X-Molecule-Org-Slug": "reno-stars",
      Authorization: "Bearer tenant-bearer",
    });
  });

  it("returns a fresh object each call (callers can mutate safely)", () => {
    process.env.NEXT_PUBLIC_ADMIN_TOKEN = "tok";
    const a = platformAuthHeaders();
    const b = platformAuthHeaders();
    expect(a).not.toBe(b); // distinct refs
    expect(a).toEqual(b); // same content
    a["Content-Type"] = "application/json";
    // Mutation on `a` does not leak into `b`.
    expect(b["Content-Type"]).toBeUndefined();
  });
});
