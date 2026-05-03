// @vitest-environment jsdom
import { describe, it, expect, beforeEach, afterEach } from "vitest";
import { render, screen, fireEvent, cleanup } from "@testing-library/react";
import { CookieConsent, hasConsent } from "../CookieConsent";

const STORAGE_KEY = "molecule_cookie_consent";

// These tests lock the privacy-preserving default: the banner appears on
// first visit (SaaS mode), clicking either button records a decision, and
// subsequent renders skip the banner until the policy version changes.
//
// The banner is SaaS-only — it references moleculesai.app's hosted privacy
// policy and presumes GDPR/ePrivacy obligations that only apply to the
// hosted offering. Self-hosted / local-dev hosts must not see it. Most
// tests below simulate SaaS by overriding window.location.hostname; the
// "local-dev" test omits that override.

// setSaaSHostname rewrites window.location.hostname to look like a SaaS
// tenant subdomain so isSaaSTenant() returns true. Must run before
// CookieConsent mounts, otherwise its one-shot useEffect captures the
// localhost default. jsdom's location object is read-only via the normal
// setter but defineProperty lets us replace it for the scope of a test.
function setSaaSHostname(host = "acme.moleculesai.app") {
  Object.defineProperty(window, "location", {
    configurable: true,
    value: { ...window.location, hostname: host },
  });
}

beforeEach(() => {
  window.localStorage.clear();
  setSaaSHostname();
});

afterEach(() => {
  cleanup();
  window.localStorage.clear();
});

describe("CookieConsent", () => {
  it("renders the banner when no decision is stored", () => {
    render(<CookieConsent />);
    expect(screen.getByRole("region")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Accept all" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Necessary only" })).toBeTruthy();
  });

  it("stores 'accepted' and hides the banner when user clicks Accept all", () => {
    render(<CookieConsent />);
    fireEvent.click(screen.getByRole("button", { name: "Accept all" }));
    expect(screen.queryByRole("region")).toBeNull();

    const raw = window.localStorage.getItem(STORAGE_KEY);
    expect(raw).not.toBeNull();
    const parsed = JSON.parse(raw!);
    expect(parsed.decision).toBe("accepted");
    expect(parsed.version).toBe(1);
    expect(typeof parsed.decidedAt).toBe("string");
  });

  it("stores 'rejected' and hides the banner when user clicks Necessary only", () => {
    render(<CookieConsent />);
    fireEvent.click(screen.getByRole("button", { name: "Necessary only" }));
    expect(screen.queryByRole("region")).toBeNull();

    const parsed = JSON.parse(window.localStorage.getItem(STORAGE_KEY)!);
    expect(parsed.decision).toBe("rejected");
  });

  it("does NOT render the banner when a current-version decision is already stored", () => {
    window.localStorage.setItem(
      STORAGE_KEY,
      JSON.stringify({ decision: "accepted", decidedAt: new Date().toISOString(), version: 1 }),
    );
    render(<CookieConsent />);
    expect(screen.queryByRole("region")).toBeNull();
  });

  it("re-prompts when the stored decision is on an older policy version", () => {
    window.localStorage.setItem(
      STORAGE_KEY,
      JSON.stringify({ decision: "accepted", decidedAt: new Date().toISOString(), version: 0 }),
    );
    render(<CookieConsent />);
    expect(screen.getByRole("region")).toBeTruthy();
  });

  it("re-prompts when localStorage contains invalid JSON", () => {
    window.localStorage.setItem(STORAGE_KEY, "{not json");
    render(<CookieConsent />);
    expect(screen.getByRole("region")).toBeTruthy();
  });

  it("exposes a privacy-policy link with target='_blank'", () => {
    render(<CookieConsent />);
    const link = screen.getByRole("link", { name: /privacy policy/i });
    expect(link).toBeTruthy();
    expect(link.getAttribute("target")).toBe("_blank");
    expect(link.getAttribute("rel")).toContain("noreferrer");
  });

  it("uses role=region (NOT dialog) with aria-labelledby/describedby — banner is informational, not modal", () => {
    // Regression guard: an earlier version claimed role="dialog"
    // aria-modal="true" without a focus trap. That falsely told screen
    // readers the rest of the page was inert, trapping AT users in a
    // banner they couldn't escape. role="region" lets assistive tech
    // navigate around it normally; the banner stays informational.
    render(<CookieConsent />);
    const banner = screen.getByRole("region");
    expect(banner.getAttribute("aria-labelledby")).toBe("cookie-consent-title");
    expect(banner.getAttribute("aria-describedby")).toBe("cookie-consent-body");
    // No aria-modal claim — explicit guard against regression.
    expect(banner.getAttribute("aria-modal")).toBeNull();
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("does NOT render on local dev (non-SaaS hostname)", () => {
    // Simulate `npm run dev` on localhost — isSaaSTenant() returns false
    // and the banner must stay hidden. Regression test for PR #1871:
    // a fresh-clone Canvas showing the hosted privacy banner on
    // localhost:3000 was confusing for self-hosted users.
    Object.defineProperty(window, "location", {
      configurable: true,
      value: { ...window.location, hostname: "localhost" },
    });
    render(<CookieConsent />);
    expect(screen.queryByRole("region")).toBeNull();
  });

  it("does NOT render on a LAN hostname (192.168.*, *.local)", () => {
    Object.defineProperty(window, "location", {
      configurable: true,
      value: { ...window.location, hostname: "192.168.1.74" },
    });
    render(<CookieConsent />);
    expect(screen.queryByRole("region")).toBeNull();
  });
});

describe("hasConsent", () => {
  it("returns false when no decision is stored (privacy-preserving default)", () => {
    expect(hasConsent()).toBe(false);
  });

  it("returns true only when the stored decision is 'accepted'", () => {
    window.localStorage.setItem(
      STORAGE_KEY,
      JSON.stringify({ decision: "accepted", decidedAt: new Date().toISOString(), version: 1 }),
    );
    expect(hasConsent()).toBe(true);
  });

  it("returns false when stored decision is 'rejected'", () => {
    window.localStorage.setItem(
      STORAGE_KEY,
      JSON.stringify({ decision: "rejected", decidedAt: new Date().toISOString(), version: 1 }),
    );
    expect(hasConsent()).toBe(false);
  });

  it("returns false when stored decision is from an older policy version", () => {
    window.localStorage.setItem(
      STORAGE_KEY,
      JSON.stringify({ decision: "accepted", decidedAt: new Date().toISOString(), version: 0 }),
    );
    expect(hasConsent()).toBe(false);
  });
});
