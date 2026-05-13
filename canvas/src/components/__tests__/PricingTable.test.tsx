// @vitest-environment jsdom
import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import { render, screen, fireEvent, cleanup, waitFor } from "@testing-library/react";
import { PricingTable } from "../PricingTable";

// Module mocks — both auth and billing are network-touching, so the tests
// script their return values. redirectToLogin is captured so we can assert
// the "anonymous + paid plan" path bounces correctly.
vi.mock("@/lib/auth", () => ({
  fetchSession: vi.fn(),
  redirectToLogin: vi.fn(),
}));
vi.mock("@/lib/billing", async () => {
  const actual = await vi.importActual<typeof import("@/lib/billing")>("@/lib/billing");
  return {
    ...actual,
    startCheckout: vi.fn(),
  };
});
// getTenantSlug is host-derived; override per test.
vi.mock("@/lib/tenant", () => ({
  getTenantSlug: vi.fn(() => "acme"),
}));

import { fetchSession, redirectToLogin } from "@/lib/auth";
import { startCheckout } from "@/lib/billing";
import { getTenantSlug } from "@/lib/tenant";

const mockedFetchSession = fetchSession as ReturnType<typeof vi.fn>;
const mockedRedirectToLogin = redirectToLogin as ReturnType<typeof vi.fn>;
const mockedStartCheckout = startCheckout as ReturnType<typeof vi.fn>;
const mockedGetTenantSlug = getTenantSlug as ReturnType<typeof vi.fn>;

beforeEach(() => {
  vi.clearAllMocks();
  mockedGetTenantSlug.mockReturnValue("acme");
  // Replace window.location.href with a spy so we can verify redirect
  // intent without actually navigating the jsdom window.
  Object.defineProperty(window, "location", {
    value: { href: "http://localhost:3000/pricing", origin: "http://localhost:3000", pathname: "/pricing" },
    writable: true,
  });
});

afterEach(() => {
  cleanup();
});

describe("PricingTable", () => {
  it("renders all three plans with their CTAs", () => {
    render(<PricingTable />);
    expect(screen.getByRole("heading", { name: "Free" })).toBeTruthy();
    expect(screen.getByRole("heading", { name: "Team" })).toBeTruthy();
    expect(screen.getByRole("heading", { name: "Growth" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Get started" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Upgrade to Team" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Upgrade to Growth" })).toBeTruthy();
  });

  it("shows the 'Most popular' badge only on the Team card", () => {
    render(<PricingTable />);
    const badges = screen.getAllByText("Most popular");
    expect(badges.length).toBe(1);
  });

  it("Free CTA redirects to signup without any session probe", () => {
    render(<PricingTable />);
    fireEvent.click(screen.getByRole("button", { name: "Get started" }));
    expect(mockedRedirectToLogin).toHaveBeenCalledWith("sign-up");
    expect(mockedFetchSession).not.toHaveBeenCalled();
    expect(mockedStartCheckout).not.toHaveBeenCalled();
  });

  it("Paid CTA + anonymous → bounces to signup (no checkout call)", async () => {
    mockedFetchSession.mockResolvedValue(null);
    render(<PricingTable />);
    fireEvent.click(screen.getByRole("button", { name: "Upgrade to Team" }));
    await waitFor(() => expect(mockedRedirectToLogin).toHaveBeenCalledWith("sign-up"));
    expect(mockedStartCheckout).not.toHaveBeenCalled();
  });

  it("Paid CTA + authed → calls startCheckout and redirects to Stripe URL", async () => {
    mockedFetchSession.mockResolvedValue({
      user_id: "u1",
      org_id: "org-acme",
      email: "a@b.com",
    });
    mockedStartCheckout.mockResolvedValue({
      id: "cs_test",
      url: "https://checkout.stripe.com/pay/cs_test",
    });

    render(<PricingTable />);
    fireEvent.click(screen.getByRole("button", { name: "Upgrade to Growth" }));

    await waitFor(() =>
      expect(mockedStartCheckout).toHaveBeenCalledWith("pro", "acme"),
    );
    await waitFor(() =>
      expect(window.location.href).toBe("https://checkout.stripe.com/pay/cs_test"),
    );
    expect(mockedRedirectToLogin).not.toHaveBeenCalled();
  });

  it("Paid CTA + authed + no tenant slug → shows 'pick an org first' error", async () => {
    mockedFetchSession.mockResolvedValue({
      user_id: "u1",
      org_id: "org-acme",
      email: "a@b.com",
    });
    mockedGetTenantSlug.mockReturnValue("");

    render(<PricingTable />);
    fireEvent.click(screen.getByRole("button", { name: "Upgrade to Team" }));

    await waitFor(() => {
      const alert = screen.getByRole("alert");
      expect(alert.textContent).toContain("tenant subdomain");
    });
    expect(mockedStartCheckout).not.toHaveBeenCalled();
  });

  it("surfaces network errors from startCheckout in the error banner", async () => {
    mockedFetchSession.mockResolvedValue({
      user_id: "u1",
      org_id: "org-acme",
      email: "a@b.com",
    });
    mockedStartCheckout.mockRejectedValue(new Error("checkout: 500 boom"));

    render(<PricingTable />);
    fireEvent.click(screen.getByRole("button", { name: "Upgrade to Growth" }));

    await waitFor(() => {
      const alert = screen.getByRole("alert");
      expect(alert.textContent).toContain("500");
    });
  });

  it("treats fetchSession network errors as anonymous (fail-closed to signup)", async () => {
    mockedFetchSession.mockRejectedValue(new Error("network down"));
    render(<PricingTable />);
    fireEvent.click(screen.getByRole("button", { name: "Upgrade to Team" }));
    await waitFor(() => expect(mockedRedirectToLogin).toHaveBeenCalledWith("sign-up"));
    expect(mockedStartCheckout).not.toHaveBeenCalled();
  });

  it("marks feature checkmarks as aria-hidden (decorative, not exposed to screen readers)", () => {
    render(<PricingTable />);
    const checks = document.body.querySelectorAll('[aria-hidden="true"]');
    // Every feature list has a ✓ glyph; all should be aria-hidden.
    expect(checks.length).toBeGreaterThan(0);
    // The checkmark spans use text-accent (decorative SVG-like glyphs).
    checks.forEach((el) => {
      expect(el.textContent?.trim()).toBe("✓");
    });
  });

  it("disables the button while a checkout call is in flight", async () => {
    mockedFetchSession.mockResolvedValue({
      user_id: "u1",
      org_id: "org-acme",
      email: "a@b.com",
    });
    // Return a promise we never resolve so the button stays loading.
    mockedStartCheckout.mockReturnValue(new Promise(() => {}));

    render(<PricingTable />);
    const button = screen.getByRole("button", { name: "Upgrade to Growth" });
    fireEvent.click(button);

    await waitFor(() => {
      const loading = screen.getByRole("button", { name: /opening checkout/i });
      expect((loading as HTMLButtonElement).disabled).toBe(true);
    });
  });
});
