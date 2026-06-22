/**
 * Tests for the CSP nonce logic in canvas/src/middleware.ts
 *
 * Security issue #450: CSP used 'unsafe-inline' + 'unsafe-eval' globally,
 * defeating the XSS protection the header is supposed to provide.
 *
 * Fix: nonce-based script-src in production; permissive only in dev.
 */
import { describe, it, expect, afterEach } from "vitest";
import { buildCsp, buildImgSrc } from "../middleware";

const TEST_NONCE = "dGVzdC1ub25jZQ=="; // base64("test-nonce")

// ---------------------------------------------------------------------------
// Production CSP — the security-critical path
// ---------------------------------------------------------------------------
describe("buildCsp — production", () => {
  const csp = buildCsp(TEST_NONCE, false);

  function scriptSrc(): string {
    return csp.match(/script-src[^;]*/)?.[0] ?? "";
  }

  it("does NOT contain 'unsafe-inline' in script-src (issue #450 fix)", () => {
    expect(scriptSrc()).not.toContain("'unsafe-inline'");
  });

  it("does NOT contain 'unsafe-eval' in script-src (issue #450 fix)", () => {
    expect(scriptSrc()).not.toContain("'unsafe-eval'");
  });

  it("embeds the nonce in script-src", () => {
    expect(scriptSrc()).toContain(`'nonce-${TEST_NONCE}'`);
  });

  it("includes 'strict-dynamic' so Next.js chunks load without allow-listing every URL", () => {
    expect(scriptSrc()).toContain("'strict-dynamic'");
  });

  it("locks object-src to 'none' (no plugins)", () => {
    expect(csp).toContain("object-src 'none'");
  });

  it("allows blob: in frame-src for authenticated PDF previews", () => {
    const frameSrc = csp.match(/frame-src[^;]*/)?.[0] ?? "";
    expect(frameSrc).toContain("'self'");
    expect(frameSrc).toContain("blob:");
  });

  it("locks base-uri to 'self' (prevents base-tag injection)", () => {
    expect(csp).toContain("base-uri 'self'");
  });

  it("locks frame-ancestors to 'none' (prevents clickjacking)", () => {
    expect(csp).toContain("frame-ancestors 'none'");
  });

  it("includes upgrade-insecure-requests", () => {
    expect(csp).toContain("upgrade-insecure-requests");
  });

  it("allows wss: in connect-src (WebSocket to platform)", () => {
    const connectSrc = csp.match(/connect-src[^;]*/)?.[0] ?? "";
    expect(connectSrc).toContain("wss:");
  });

  it("adds NEXT_PUBLIC_PLATFORM_URL origin to connect-src (CP cross-origin fetches)", () => {
    // Canvas calls fetch(PLATFORM_URL + "/cp/auth/me") etc. from the
    // browser. The browser blocks cross-origin fetches that aren't in
    // connect-src, even with CORS headers set server-side. Whitelisting
    // the origin is what makes fresh login + session refresh work on
    // tenant subdomains after they were broken pre-2026-04-20 by a CSP
    // that only allowed 'self' and wss:.
    process.env.NEXT_PUBLIC_PLATFORM_URL = "https://api.example.com";
    const built = buildCsp(TEST_NONCE, false);
    const connectSrc = built.match(/connect-src[^;]*/)?.[0] ?? "";
    expect(connectSrc).toContain("https://api.example.com");
    expect(connectSrc).toContain("wss://api.example.com");
    delete process.env.NEXT_PUBLIC_PLATFORM_URL;
  });

  it("does NOT include bare ws: in connect-src (prod uses wss only)", () => {
    const connectSrc = csp.match(/connect-src[^;]*/)?.[0] ?? "";
    // ws: (without 's') is insecure — should not be in production policy
    // Note: "wss:" contains the substring "ws" so we check for word "ws:"
    const parts = connectSrc.split(/\s+/);
    expect(parts).not.toContain("ws:");
  });

  it("allows blob: in worker-src (React Flow / canvas workers)", () => {
    const workerSrc = csp.match(/worker-src[^;]*/)?.[0] ?? "";
    expect(workerSrc).toContain("blob:");
  });

  it("different nonces produce different CSPs", () => {
    const csp2 = buildCsp("ZGlmZmVyZW50", false);
    expect(csp).not.toBe(csp2);
  });
});

// ---------------------------------------------------------------------------
// Development CSP — HMR / fast-refresh compatibility
// ---------------------------------------------------------------------------
describe("buildCsp — development", () => {
  const csp = buildCsp(TEST_NONCE, true);

  function scriptSrc(): string {
    return csp.match(/script-src[^;]*/)?.[0] ?? "";
  }

  it("retains 'unsafe-inline' so Next.js HMR injects without errors", () => {
    expect(scriptSrc()).toContain("'unsafe-inline'");
  });

  it("retains 'unsafe-eval' so fast-refresh / webpack eval() works", () => {
    expect(scriptSrc()).toContain("'unsafe-eval'");
  });

  it("permits ws:// in connect-src (HMR WebSocket uses plain ws://)", () => {
    const connectSrc = csp.match(/connect-src[^;]*/)?.[0] ?? "";
    // Dev uses `connect-src *` (fully permissive) which subsumes ws:.
    // Accept either the literal `ws:` scheme wildcard OR the blanket
    // `*` since both let HMR through. Prod tests still enforce `ws:`
    // is NOT present.
    expect(connectSrc).toMatch(/\bws:|\*/);
  });
});

// ---------------------------------------------------------------------------
// CSP format invariants (both modes)
// ---------------------------------------------------------------------------
describe("buildCsp — format invariants", () => {
  for (const [label, csp] of [
    ["production", buildCsp(TEST_NONCE, false)],
    ["development", buildCsp(TEST_NONCE, true)],
  ] as const) {
    it(`[${label}] ends with a semicolon`, () => {
      expect(csp.trimEnd()).toMatch(/;$/);
    });

    it(`[${label}] contains default-src 'self'`, () => {
      expect(csp).toContain("default-src 'self'");
    });

    it(`[${label}] allows blob: and data: for img-src (canvas avatars / thumbnails)`, () => {
      const imgSrc = csp.match(/img-src[^;]*/)?.[0] ?? "";
      expect(imgSrc).toContain("blob:");
      expect(imgSrc).toContain("data:");
    });

    it(`[${label}] allows the generated-image R2 host in img-src`, () => {
      const imgSrc = csp.match(/img-src[^;]*/)?.[0] ?? "";
      expect(imgSrc).toContain("r2.cloudflarestorage.com");
    });
  }
});

// ---------------------------------------------------------------------------
// buildImgSrc — generated-image (image-gen socket / R2) host handling
// ---------------------------------------------------------------------------
describe("buildImgSrc — generated-image R2 host", () => {
  afterEach(() => {
    delete process.env.NEXT_PUBLIC_IMAGE_GEN_R2_HOST;
  });

  it("keeps self/blob:/data: for the canvas's own assets", () => {
    const imgSrc = buildImgSrc();
    expect(imgSrc.startsWith("img-src ")).toBe(true);
    expect(imgSrc).toContain("'self'");
    expect(imgSrc).toContain("blob:");
    expect(imgSrc).toContain("data:");
  });

  it("defaults to the documented R2 wildcard when no host is pinned", () => {
    delete process.env.NEXT_PUBLIC_IMAGE_GEN_R2_HOST;
    expect(buildImgSrc()).toContain("https://*.r2.cloudflarestorage.com");
  });

  it("uses NEXT_PUBLIC_IMAGE_GEN_R2_HOST when pinned (tightest policy)", () => {
    const pinned =
      "https://molecule-workspace-data.deadbeef.r2.cloudflarestorage.com";
    process.env.NEXT_PUBLIC_IMAGE_GEN_R2_HOST = pinned;
    const imgSrc = buildImgSrc();
    expect(imgSrc).toContain(pinned);
    expect(imgSrc).not.toContain("*.r2.cloudflarestorage.com");
  });
});

// ---------------------------------------------------------------------------
// Security invariant: connect-src must NOT be widened to R2 (no exfil channel)
// ---------------------------------------------------------------------------
describe("buildCsp — connect-src stays narrow (R2 only displayable, not fetchable)", () => {
  it("does not include any r2.cloudflarestorage.com host in connect-src", () => {
    const csp = buildCsp(TEST_NONCE, false);
    const connectSrc = csp.match(/connect-src[^;]*/)?.[0] ?? "";
    expect(connectSrc).not.toContain("r2.cloudflarestorage.com");
  });
});
