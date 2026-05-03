// @vitest-environment jsdom
import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import { render, cleanup, act } from "@testing-library/react";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

// ── Mocks (defined before dynamic import of component) ───────────────────────
// Use a function type so TypeScript accepts the mock as callable in vi.mock factories.
// ReturnType<typeof vi.fn> resolves to Mock<Procedure|Constructable> in newer Vitest
// type defs, which TS no longer considers directly callable. Casting to a plain
// function type avoids the TS2348 error while keeping full mock API (mockReturnValue etc.).
let mockFetchSession: ((...args: unknown[]) => unknown) & ReturnType<typeof vi.fn>;
let mockRedirectToLogin: ((...args: unknown[]) => unknown) & ReturnType<typeof vi.fn>;
let mockGetTenantSlug: ((...args: unknown[]) => unknown) & ReturnType<typeof vi.fn>;

beforeEach(() => {
  mockFetchSession = vi.fn() as typeof mockFetchSession;
  mockRedirectToLogin = vi.fn() as typeof mockRedirectToLogin;
  mockGetTenantSlug = vi.fn(() => null) as typeof mockGetTenantSlug;
});

vi.mock("@/lib/auth", () => ({
  // Cast required: vi.fn() returns Mock<Procedure | Constructable> which TypeScript
  // won't call directly inside a factory closure (TS2348). Cast to Function resolves it.
  fetchSession: (...args: unknown[]) => (mockFetchSession as unknown as (...a: unknown[]) => unknown)(...args),
  redirectToLogin: (...args: unknown[]) => (mockRedirectToLogin as unknown as (...a: unknown[]) => unknown)(...args),
}));

vi.mock("@/lib/tenant", () => ({
  getTenantSlug: (...args: unknown[]) => (mockGetTenantSlug as unknown as (...a: unknown[]) => unknown)(...args),
}));

// Import after mocks are set up
import { AuthGate } from "../AuthGate";

// ─────────────────────────────────────────────────────────────────────────────

describe("AuthGate — loading state", () => {
  it("renders a blank overlay while session fetch is in-flight (prevents flash of unauth'd content)", () => {
    // getTenantSlug returns a slug so the session fetch is triggered
    mockGetTenantSlug.mockReturnValue("acme");
    // fetchSession never resolves — keeps the component in loading state
    mockFetchSession.mockReturnValue(new Promise(() => {}));

    const { container } = render(
      <AuthGate>
        <div data-testid="child">Protected content</div>
      </AuthGate>
    );

    const overlay = container.querySelector(".bg-surface.fixed.inset-0");
    expect(overlay).not.toBeNull();
    expect(overlay?.getAttribute("aria-hidden")).toBe("true");
  });

  it("does not render children while in loading state", () => {
    mockGetTenantSlug.mockReturnValue("acme");
    mockFetchSession.mockReturnValue(new Promise(() => {}));

    const { queryByTestId } = render(
      <AuthGate>
        <div data-testid="child">Protected content</div>
      </AuthGate>
    );

    expect(queryByTestId("child")).toBeNull();
  });
});

describe("AuthGate — non-SaaS / pass-through mode", () => {
  it("renders children immediately when there is no tenant slug", async () => {
    mockGetTenantSlug.mockReturnValue(null);

    let result: ReturnType<typeof render>;
    await act(async () => {
      result = render(
        <AuthGate>
          <div data-testid="child">Protected content</div>
        </AuthGate>
      );
    });

    expect(result!.getByTestId("child")).toBeTruthy();
  });
});

describe("AuthGate — authenticated state", () => {
  it("renders children after a successful session fetch", async () => {
    mockGetTenantSlug.mockReturnValue("acme");
    mockFetchSession.mockResolvedValue({ userId: "u1", email: "a@b.com" });

    let result: ReturnType<typeof render>;
    await act(async () => {
      result = render(
        <AuthGate>
          <div data-testid="child">Protected content</div>
        </AuthGate>
      );
    });

    expect(result!.getByTestId("child")).toBeTruthy();
  });
});

describe("AuthGate — /cp/auth/* skip guard (redirect loop regression)", () => {
  it("renders children without calling fetchSession or redirect when pathname starts with /cp/auth/", async () => {
    mockGetTenantSlug.mockReturnValue("acme");
    mockFetchSession.mockResolvedValue(null);

    // Simulate being on the login page
    Object.defineProperty(window, "location", {
      writable: true,
      value: { ...window.location, pathname: "/cp/auth/login" },
    });

    let result: ReturnType<typeof render>;
    await act(async () => {
      result = render(
        <AuthGate>
          <div data-testid="child">Protected content</div>
        </AuthGate>
      );
    });

    // Children should render — AuthGate skips session fetch for auth paths
    expect(result!.getByTestId("child")).toBeTruthy();
    expect(mockFetchSession).not.toHaveBeenCalled();
    expect(mockRedirectToLogin).not.toHaveBeenCalled();
  });

  it("renders children without calling redirect for /cp/auth/signup path", async () => {
    mockGetTenantSlug.mockReturnValue("acme");
    mockFetchSession.mockResolvedValue(null);

    Object.defineProperty(window, "location", {
      writable: true,
      value: { ...window.location, pathname: "/cp/auth/signup" },
    });

    let result: ReturnType<typeof render>;
    await act(async () => {
      result = render(
        <AuthGate>
          <div data-testid="child">Protected content</div>
        </AuthGate>
      );
    });

    expect(result!.getByTestId("child")).toBeTruthy();
    expect(mockRedirectToLogin).not.toHaveBeenCalled();
  });
});

describe("AuthGate — anonymous / redirect state", () => {
  it("calls redirectToLogin when session fetch returns null", async () => {
    mockGetTenantSlug.mockReturnValue("acme");
    mockFetchSession.mockResolvedValue(null);
    // Ensure pathname is NOT on /cp/auth/* so the redirect guard fires
    Object.defineProperty(window, "location", {
      writable: true,
      value: { ...window.location, pathname: "/dashboard" },
    });

    await act(async () => {
      render(
        <AuthGate>
          <div data-testid="child">Protected content</div>
        </AuthGate>
      );
    });

    expect(mockRedirectToLogin).toHaveBeenCalledWith("sign-in");
  });
});
