// @vitest-environment jsdom
import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";

// Dedicated file for the 401 → login-redirect tests because they need
// `window.location.hostname` (jsdom), while the rest of api.test.ts
// runs happily in node. Splitting keeps the node tests fast.

// ---------------------------------------------------------------------------
// 401 handling — session-probe-before-redirect
// ---------------------------------------------------------------------------
//
// History:
//   1. fix/quickstart-bugless: gated redirect on SaaS hostname (slug).
//   2. fix/api-401-probe-before-redirect (this file): probe /cp/auth/me
//      before redirecting on a 401 from a non-auth path. The earlier
//      behaviour redirected on EVERY 401, so a single 401 from
//      /workspaces/:id/plugins (workspace-scoped — refused by the
//      tenant admin bearer) yanked the user to AuthKit even when
//      the session was fine. The probe lets us tell "session dead"
//      from "endpoint refused this token."
//
// Matrix:
//   slug    | path             | probe → me | expected
//   ---     | ---              | ---        | ---
//   acme    | /cp/auth/me      | (n/a)      | redirect (path IS auth)
//   acme    | /workspaces/...  | 401        | redirect (session dead)
//   acme    | /workspaces/...  | 200        | throw, no redirect
//   acme    | /workspaces/...  | network err| throw, no redirect
//   ""      | /workspaces/...  | (n/a)      | throw, no redirect (no slug)

const mockFetch = vi.fn();
globalThis.fetch = mockFetch;

function mockNextResponse(status: number, text = "") {
  mockFetch.mockResolvedValueOnce({
    ok: status >= 200 && status < 300,
    status,
    json: () => Promise.reject(new Error("no json")),
    text: () => Promise.resolve(text),
  } as unknown as Response);
}

function mockNextNetworkError() {
  mockFetch.mockRejectedValueOnce(new Error("network"));
}

function setHostname(host: string) {
  Object.defineProperty(window, "location", {
    configurable: true,
    value: { ...window.location, hostname: host },
  });
}

describe("api 401 handling", () => {
  let redirectSpy: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    vi.clearAllMocks();
    vi.resetModules();
    redirectSpy = vi.fn();
    vi.doMock("../auth", () => ({
      redirectToLogin: redirectSpy,
      // Stub siblings so any other import of ../auth in the chain
      // (AuthGate, TermsGate, etc.) still resolves.
      fetchSession: vi.fn().mockResolvedValue(null),
    }));
  });

  afterEach(() => {
    vi.doUnmock("../auth");
    vi.resetModules();
  });

  it("redirects when /cp/auth/me itself 401s — that IS the session-dead signal", async () => {
    setHostname("acme.moleculesai.app");
    // Single fetch: the /cp/auth/me call itself.
    mockNextResponse(401, '{"error":"unauthenticated"}');

    const { api } = await import("../api");
    await expect(api.get("/cp/auth/me")).rejects.toThrow(/Session expired/);
    expect(redirectSpy).toHaveBeenCalledWith("sign-in");
    // No probe fired — we already know the session is dead.
    expect(mockFetch).toHaveBeenCalledTimes(1);
  });

  it("redirects when /cp/auth/me probe ALSO 401s — session genuinely dead", async () => {
    setHostname("acme.moleculesai.app");
    // First call: the workspace-scoped fetch returns 401.
    mockNextResponse(401, '{"error":"workspace token required"}');
    // Second call: the probe to /cp/auth/me also 401s.
    mockNextResponse(401, '{"error":"unauthenticated"}');

    const { api } = await import("../api");
    await expect(api.get("/workspaces/abc/plugins")).rejects.toThrow(/Session expired/);
    expect(redirectSpy).toHaveBeenCalledWith("sign-in");
  });

  it("does NOT redirect when probe returns 200 — endpoint refused this token, session fine", async () => {
    setHostname("acme.moleculesai.app");
    // First call: workspace-scoped 401.
    mockNextResponse(401, '{"error":"workspace token required"}');
    // Second call: probe shows the session is alive.
    mockNextResponse(200, '{"user_id":"u1","org_id":"o1","email":"x@y"}');

    const { api } = await import("../api");
    await expect(api.get("/workspaces/abc/plugins")).rejects.toThrow(/401/);
    expect(redirectSpy).not.toHaveBeenCalled();
  });

  it("does NOT redirect when probe network-errors — conservative fallback", async () => {
    setHostname("acme.moleculesai.app");
    mockNextResponse(401, '{"error":"workspace token required"}');
    mockNextNetworkError();

    const { api } = await import("../api");
    await expect(api.get("/workspaces/abc/plugins")).rejects.toThrow(/401/);
    expect(redirectSpy).not.toHaveBeenCalled();
  });

  it("does NOT redirect on localhost — throws a real error instead", async () => {
    setHostname("localhost");
    mockNextResponse(401, '{"error":"admin auth required"}');

    const { api } = await import("../api");
    await expect(api.get("/workspaces")).rejects.toThrow(/401/);
    expect(redirectSpy).not.toHaveBeenCalled();
    // No slug → no probe fires either.
    expect(mockFetch).toHaveBeenCalledTimes(1);
  });

  it("does NOT redirect on a LAN hostname", async () => {
    setHostname("192.168.1.74");
    mockNextResponse(401, '{"error":"missing workspace auth token"}');

    const { api } = await import("../api");
    await expect(api.get("/workspaces/abc/activity")).rejects.toThrow(/401/);
    expect(redirectSpy).not.toHaveBeenCalled();
  });

  it("does NOT redirect on reserved subdomains (app.moleculesai.app)", async () => {
    // `app` is in reservedSubdomains — getTenantSlug returns "" there.
    // Users landing on app.moleculesai.app (pre-tenant-selection) must
    // see the real 401 error rather than loop on login.
    setHostname("app.moleculesai.app");
    mockNextResponse(401, '{"error":"admin auth required"}');

    const { api } = await import("../api");
    await expect(api.get("/workspaces")).rejects.toThrow(/401/);
    expect(redirectSpy).not.toHaveBeenCalled();
  });
});
