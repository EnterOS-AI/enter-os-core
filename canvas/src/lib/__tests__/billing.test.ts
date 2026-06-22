// @vitest-environment jsdom
import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import { plans, startCheckout, openBillingPortal } from "../billing";

const originalFetch = global.fetch;

beforeEach(() => {
  // Each test installs its own fetch mock; restore in afterEach so a
  // failing test doesn't leak into the next one.
  global.fetch = vi.fn() as unknown as typeof fetch;
  // jsdom's default location is http://localhost:3000/; anchor the
  // return_to construction there so snapshot assertions are stable.
  Object.defineProperty(window, "location", {
    value: {
      origin: "http://localhost:3000",
      pathname: "/pricing",
      href: "http://localhost:3000/pricing",
    },
    writable: true,
  });
});

afterEach(() => {
  global.fetch = originalFetch;
  vi.restoreAllMocks();
});

describe("plans", () => {
  it("defines three canonical tiers in free → starter → pro order", () => {
    expect(plans.map((p) => p.id)).toEqual(["free", "starter", "pro"]);
  });

  it("marks starter as highlighted (most-popular card)", () => {
    const starter = plans.find((p) => p.id === "starter");
    expect(starter?.highlighted).toBe(true);
  });

  it("gives every plan a price, tagline, and at least one feature", () => {
    for (const plan of plans) {
      expect(plan.price).toBeTruthy();
      expect(plan.tagline).toBeTruthy();
      expect(plan.features.length).toBeGreaterThan(0);
      expect(plan.ctaLabel).toBeTruthy();
    }
  });
});

describe("startCheckout", () => {
  it("POSTs to /cp/billing/checkout with the expected payload shape", async () => {
    (global.fetch as ReturnType<typeof vi.fn>).mockResolvedValue({
      ok: true,
      json: async () => ({ id: "cs_test", url: "https://checkout.stripe.com/pay/cs_test" }),
    });

    const result = await startCheckout("pro", "acme");

    expect(result.url).toBe("https://checkout.stripe.com/pay/cs_test");
    const call = (global.fetch as ReturnType<typeof vi.fn>).mock.calls[0];
    const url = call[0] as string;
    const init = call[1] as RequestInit;

    expect(url).toContain("/cp/billing/checkout");
    expect(init.method).toBe("POST");
    expect(init.credentials).toBe("include");

    const body = JSON.parse(init.body as string);
    expect(body.org_slug).toBe("acme");
    expect(body.plan).toBe("pro");
    expect(body.success_url).toContain("checkout=success");
    expect(body.cancel_url).toContain("checkout=cancel");
  });

  it("throws with status code on non-2xx; body is logged not surfaced", async () => {
    (global.fetch as ReturnType<typeof vi.fn>).mockResolvedValue({
      ok: false,
      status: 402,
      text: async () => "payment required",
      json: async () => ({}),
    });

    // Status code must appear so callers know what happened.
    await expect(startCheckout("starter", "acme")).rejects.toThrow(/402/);
    // Body text must NOT appear — it may contain Stripe API detail.
    await expect(startCheckout("starter", "acme")).rejects.toThrow(/checkout failed/);
    await expect(startCheckout("starter", "acme")).rejects.not.toThrow(/payment required/);
  });

  it("sends users to /orgs on success, back to current page on cancel", async () => {
    // success_url is fixed to /orgs regardless of where checkout was
    // initiated — that's the landing page where post-payment status
    // transitions are visible. cancel_url preserves the current page
    // so users land back on /pricing and can retry.
    (global.fetch as ReturnType<typeof vi.fn>).mockResolvedValue({
      ok: true,
      json: async () => ({ url: "https://checkout.stripe.com/x" }),
    });
    await startCheckout("starter", "acme");
    const body = JSON.parse(
      (global.fetch as ReturnType<typeof vi.fn>).mock.calls[0][1].body,
    );
    expect(body.success_url).toBe("http://localhost:3000/orgs?checkout=success");
    expect(body.cancel_url).toBe("http://localhost:3000/pricing?checkout=cancel");
  });
});

describe("openBillingPortal", () => {
  it("POSTs to /cp/billing/portal and returns the URL", async () => {
    (global.fetch as ReturnType<typeof vi.fn>).mockResolvedValue({
      ok: true,
      json: async () => ({ url: "https://billing.stripe.com/session/xyz" }),
    });
    const url = await openBillingPortal("acme");
    expect(url).toBe("https://billing.stripe.com/session/xyz");

    const call = (global.fetch as ReturnType<typeof vi.fn>).mock.calls[0];
    expect(call[0]).toContain("/cp/billing/portal");
    const body = JSON.parse((call[1] as RequestInit).body as string);
    expect(body.org_slug).toBe("acme");
    expect(body.return_url).toBe("http://localhost:3000/pricing");
  });

  it("throws on non-2xx", async () => {
    (global.fetch as ReturnType<typeof vi.fn>).mockResolvedValue({
      ok: false,
      status: 500,
      text: async () => "boom",
      json: async () => ({}),
    });
    await expect(openBillingPortal("acme")).rejects.toThrow(/500/);
  });
});
