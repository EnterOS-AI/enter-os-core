/**
 * @vitest-environment jsdom
 */
import { describe, it, expect, vi, afterEach } from "vitest";
import { fetchSession, redirectToLogin, signOut } from "../auth";

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("fetchSession", () => {
  it("returns session on 200", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({ user_id: "u1", org_id: "o1", email: "a@x.com" }),
    }));
    const s = await fetchSession();
    expect(s).toEqual({ user_id: "u1", org_id: "o1", email: "a@x.com" });
  });

  it("returns null on 401 without throwing", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: false, status: 401 }));
    const s = await fetchSession();
    expect(s).toBeNull();
  });

  it("throws on 500 so transient outages aren't treated as 'anonymous'", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: false, status: 500, statusText: "oops" }));
    await expect(fetchSession()).rejects.toThrow("500");
  });

  it("sends credentials:include for cross-origin cookies", async () => {
    const fetchMock = vi.fn().mockResolvedValue({ ok: false, status: 401 });
    vi.stubGlobal("fetch", fetchMock);
    await fetchSession();
    expect(fetchMock).toHaveBeenCalledWith(
      expect.stringContaining("/cp/auth/me"),
      expect.objectContaining({ credentials: "include" }),
    );
  });
});

describe("redirectToLogin", () => {
  it("sets window.location to cp login URL with return_to", () => {
    const href = "https://acme.moleculesai.app/dashboard";
    Object.defineProperty(window, "location", {
      writable: true,
      value: {
        href,
        pathname: "/dashboard",
        hostname: "acme.moleculesai.app",
        protocol: "https:",
      },
    });
    redirectToLogin("sign-in");
    expect((window.location as unknown as { href: string }).href).toContain("/cp/auth/login");
    expect((window.location as unknown as { href: string }).href).toContain(
      encodeURIComponent(href),
    );
  });

  it("uses signup path for sign-up screenHint", () => {
    Object.defineProperty(window, "location", {
      writable: true,
      value: {
        href: "https://acme.moleculesai.app/",
        pathname: "/",
        hostname: "acme.moleculesai.app",
        protocol: "https:",
      },
    });
    redirectToLogin("sign-up");
    expect((window.location as unknown as { href: string }).href).toContain("/cp/auth/signup");
  });

  // Regression: AuthGate + redirectToLogin mutual recursion on /cp/auth/login
  // caused double-encoded return_to that grew until the URL exceeded 431.
  // Guard: redirectToLogin must NOT set window.location when already on an
  // auth path, otherwise each call adds another encoding layer.
  it("does NOT set window.location when already on /cp/auth/login (redirect loop guard)", () => {
    const loginHref = "https://app.moleculesai.app/cp/auth/login?return_to=https%3A%2F%2Facme.moleculesai.app%2Fdashboard";
    Object.defineProperty(window, "location", {
      writable: true,
      value: {
        href: loginHref,
        pathname: "/cp/auth/login",
        hostname: "app.moleculesai.app",
        protocol: "https:",
      },
    });
    redirectToLogin("sign-in");
    // href must be unchanged — any mutation means the guard is missing
    expect((window.location as unknown as { href: string }).href).toBe(loginHref);
  });

  it("does NOT set window.location when already on /cp/auth/signup (redirect loop guard)", () => {
    const signupHref = "https://app.moleculesai.app/cp/auth/signup";
    Object.defineProperty(window, "location", {
      writable: true,
      value: {
        href: signupHref,
        pathname: "/cp/auth/signup",
        hostname: "app.moleculesai.app",
        protocol: "https:",
      },
    });
    redirectToLogin("sign-up");
    expect((window.location as unknown as { href: string }).href).toBe(signupHref);
  });
});

describe("signOut", () => {
  // Helper — most tests need the same window.location stub.
  function stubLocation(): void {
    Object.defineProperty(window, "location", {
      writable: true,
      value: {
        href: "https://acme.moleculesai.app/orgs",
        pathname: "/orgs",
        hostname: "acme.moleculesai.app",
        protocol: "https:",
      },
    });
  }

  it("POSTs to /cp/auth/signout with credentials:include", async () => {
    stubLocation();
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({ ok: true, logout_url: "" }),
    });
    vi.stubGlobal("fetch", fetchMock);

    await signOut();

    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(fetchMock).toHaveBeenCalledWith(
      expect.stringContaining("/cp/auth/signout"),
      expect.objectContaining({ method: "POST", credentials: "include" }),
    );
  });

  it("navigates to provider logout_url when the response includes one", async () => {
    // The hosted-logout path is what actually breaks the SSO re-auth
    // loop reported on PR #2913. Without this, AuthKit's browser
    // cookie keeps the user signed in via SSO and any subsequent
    // /cp/auth/login silently re-auths.
    stubLocation();
    const hostedLogout =
      "https://api.workos.com/user_management/sessions/logout?session_id=cookie&return_to=https%3A%2F%2Fapp.moleculesai.app%2Forgs";
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({
        ok: true,
        status: 200,
        json: async () => ({ ok: true, logout_url: hostedLogout }),
      }),
    );

    await signOut();

    const after = (window.location as unknown as { href: string }).href;
    expect(after).toBe(hostedLogout);
  });

  it("falls back to /cp/auth/login when logout_url is empty (DisabledProvider / dev)", async () => {
    // DisabledProvider returns "" — the local /cp/auth/login redirect
    // works in dev/test where there's no SSO session to escape.
    stubLocation();
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({
        ok: true,
        status: 200,
        json: async () => ({ ok: true, logout_url: "" }),
      }),
    );

    await signOut();

    const after = (window.location as unknown as { href: string }).href;
    // Tenant subdomain (acme.moleculesai.app) → auth origin is app.moleculesai.app.
    expect(after).toBe("https://app.moleculesai.app/cp/auth/login");
  });

  it("redirects even when the POST fails so the user isn't stuck on an authed page", async () => {
    // Critical UX invariant: clicking 'Sign out' MUST navigate away from
    // the authenticated app, even if the network is down or the cookie
    // is already invalid. Anything else looks like the button is
    // broken — the precise complaint that triggered this fix.
    stubLocation();
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("network down")));

    await signOut();

    const after = (window.location as unknown as { href: string }).href;
    expect(after).toBe("https://app.moleculesai.app/cp/auth/login");
  });

  it("redirects on 401 (session already invalid) just like 200", async () => {
    // A user with an already-invalid cookie should still see the
    // logout flow complete — no error, no stuck-on-app dead end.
    // Note: 401 means res.ok=false → we don't read .json() at all,
    // so a missing body is fine.
    stubLocation();
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({
        ok: false,
        status: 401,
        json: async () => ({}),
      }),
    );

    await signOut();

    const after = (window.location as unknown as { href: string }).href;
    expect(after).toBe("https://app.moleculesai.app/cp/auth/login");
  });

  it("falls back to /cp/auth/login when the response body is malformed", async () => {
    // Defensive parsing: a body that isn't valid JSON, or doesn't
    // have logout_url, or has logout_url as the wrong type — none of
    // these should strand the user on the authed page. Fallback path
    // takes over.
    stubLocation();
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({
        ok: true,
        status: 200,
        json: async () => {
          throw new Error("not json");
        },
      }),
    );

    await signOut();

    const after = (window.location as unknown as { href: string }).href;
    expect(after).toBe("https://app.moleculesai.app/cp/auth/login");
  });

  it("falls back to /cp/auth/login when logout_url is the wrong type", async () => {
    // Even valid JSON should be type-checked: a non-string logout_url
    // (e.g. server-side bug, version drift) must not crash or open-
    // redirect the user.
    stubLocation();
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({
        ok: true,
        status: 200,
        json: async () => ({ ok: true, logout_url: 42 }),
      }),
    );

    await signOut();

    const after = (window.location as unknown as { href: string }).href;
    expect(after).toBe("https://app.moleculesai.app/cp/auth/login");
  });
});
