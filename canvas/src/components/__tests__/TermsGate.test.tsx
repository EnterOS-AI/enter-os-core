// @vitest-environment jsdom
/**
 * Tests for TermsGate component.
 *
 * Covers: loading → accepted (already agreed), loading → pending (show
 * modal), 401 → accepted (not signed in), error state, accept flow,
 * focus management (WCAG 2.4.3), and modal accessibility.
 */
import React from "react";
import { render, screen, fireEvent, cleanup, waitFor, act } from "@testing-library/react";
import { afterEach, describe, expect, it, vi, beforeEach } from "vitest";
import { TermsGate } from "../TermsGate";

// PLATFORM_URL is imported from @/lib/api; we mock it via module mock
vi.mock("@/lib/api", () => ({
  PLATFORM_URL: "https://app.example.com",
}));

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

// ─── Helpers ──────────────────────────────────────────────────────────────────

function mockFetch(res: Response) {
  vi.spyOn(global, "fetch").mockResolvedValueOnce(res);
}

async function resolveFetch(res: Response) {
  await act(async () => {
    vi.spyOn(global, "fetch").mockResolvedValueOnce(res);
  });
}

// ─── Tests ────────────────────────────────────────────────────────────────────

describe("TermsGate — loading → accepted", () => {
  it("renders children immediately (loading state)", () => {
    mockFetch(new Response(JSON.stringify({ accepted: true }), { status: 200 }));
    render(
      <TermsGate>
        <div data-testid="children">App content</div>
      </TermsGate>
    );
    // Children are always rendered (TermsGate does not hide them)
    expect(screen.getByTestId("children")).toBeTruthy();
  });

  it("shows no dialog when server returns accepted=true", async () => {
    mockFetch(new Response(JSON.stringify({ accepted: true }), { status: 200 }));
    render(
      <TermsGate>
        <div data-testid="children">App content</div>
      </TermsGate>
    );
    await waitFor(() => {
      expect(screen.queryByRole("dialog")).toBeNull();
    });
  });

  it("shows no dialog when server returns 401 (not signed in)", async () => {
    mockFetch(new Response(null, { status: 401 }));
    render(
      <TermsGate>
        <div data-testid="children">App content</div>
      </TermsGate>
    );
    await waitFor(() => {
      expect(screen.queryByRole("dialog")).toBeNull();
    });
  });
});

describe("TermsGate — pending state → modal", () => {
  it("shows the terms dialog when server returns accepted=false", async () => {
    mockFetch(new Response(JSON.stringify({ accepted: false }), { status: 200 }));
    render(
      <TermsGate>
        <div data-testid="children">App content</div>
      </TermsGate>
    );
    await waitFor(() => {
      expect(screen.getByRole("dialog")).toBeTruthy();
    });
  });

  it("dialog has aria-modal=true and correct labelling", async () => {
    mockFetch(new Response(JSON.stringify({ accepted: false }), { status: 200 }));
    render(
      <TermsGate>
        <div>App content</div>
      </TermsGate>
    );
    const dialog = await waitFor(() => screen.getByRole("dialog"));
    expect(dialog.getAttribute("aria-modal")).toBe("true");
    expect(dialog.getAttribute("aria-labelledby")).toBeTruthy();
    const title = document.getElementById(dialog.getAttribute("aria-labelledby")!);
    expect(title?.textContent).toMatch(/terms/i);
  });

  it("dialog body contains the terms text", async () => {
    mockFetch(new Response(JSON.stringify({ accepted: false }), { status: 200 }));
    render(<TermsGate><div>App content</div></TermsGate>);
    await waitFor(() => screen.getByRole("dialog"));
    expect(screen.getByText(/Terms of Service/i)).toBeTruthy();
    expect(screen.getByText(/Privacy Policy/i)).toBeTruthy();
    expect(screen.getByText(/AWS us-east-2/i)).toBeTruthy();
  });

  it("the I agree button is present", async () => {
    mockFetch(new Response(JSON.stringify({ accepted: false }), { status: 200 }));
    render(<TermsGate><div>App content</div></TermsGate>);
    await waitFor(() => screen.getByRole("dialog"));
    expect(screen.getByRole("button", { name: /i agree/i })).toBeTruthy();
  });

  it("links to terms and privacy policy have correct hrefs", async () => {
    mockFetch(new Response(JSON.stringify({ accepted: false }), { status: 200 }));
    render(<TermsGate><div>App content</div></TermsGate>);
    await waitFor(() => screen.getByRole("dialog"));
    const links = screen.getAllByRole("link");
    const hrefs = links.map((l) => l.getAttribute("href"));
    expect(hrefs).toContain("/legal/terms");
    expect(hrefs).toContain("/legal/privacy");
  });
});

describe("TermsGate — focus management (WCAG 2.4.3)", () => {
  it("moves focus to the I agree button when modal opens", async () => {
    mockFetch(new Response(JSON.stringify({ accepted: false }), { status: 200 }));
    render(<TermsGate><div>App content</div></TermsGate>);
    const dialog = await waitFor(() => screen.getByRole("dialog"));
    // Focus is moved via requestAnimationFrame — wait a tick
    await act(async () => {
      await new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r)));
    });
    const agreeBtn = screen.getByRole("button", { name: /i agree/i });
    expect(document.activeElement).toBe(agreeBtn);
  });
});

describe("TermsGate — accept flow", () => {
  it("calls POST /cp/auth/accept-terms and closes dialog on success", async () => {
    // First: terms-status → pending
    mockFetch(new Response(JSON.stringify({ accepted: false }), { status: 200 }));
    // Second: accept-terms → 200
    const postMock = mockFetch(new Response(null, { status: 200 }));

    render(<TermsGate><div>App content</div></TermsGate>);
    await waitFor(() => screen.getByRole("dialog"));

    fireEvent.click(screen.getByRole("button", { name: /i agree/i }));

    await waitFor(() => {
      expect(screen.queryByRole("dialog")).toBeNull();
    });

    // Check POST was called
    const calls = vi.mocked(global.fetch).mock.calls;
    expect(calls.some(
      ([url, opts]) =>
        (url as string).includes("/accept-terms") &&
        (opts as RequestInit).method === "POST"
    )).toBe(true);
  });

  it("shows error message and keeps modal open when accept fails", async () => {
    mockFetch(new Response(JSON.stringify({ accepted: false }), { status: 200 }));
    mockFetch(new Response("Internal Server Error", { status: 500 }));

    render(<TermsGate><div>App content</div></TermsGate>);
    await waitFor(() => screen.getByRole("dialog"));

    fireEvent.click(screen.getByRole("button", { name: /i agree/i }));

    await waitFor(() => {
      expect(screen.getByRole("alert")).toBeTruthy();
    });
    // Dialog is still open
    expect(screen.getByRole("dialog")).toBeTruthy();
  });
});

describe("TermsGate — I agree button accessibility", () => {
  it("shows ellipsis on the I agree button while POST is in flight", async () => {
    // Deferred POST so we can control when it resolves and observe the
    // mid-flight button state without fake timers.
    let resolvePost: (r: Response) => void;
    const postDeferred = new Promise<Response>((r) => { resolvePost = r; });
    // Intercept: terms-status → pending (first fetch), POST deferred (second).
    mockFetch(new Response(JSON.stringify({ accepted: false }), { status: 200 }));
    vi.spyOn(global, "fetch").mockImplementation(
      () => postDeferred as unknown as Promise<Response>
    );

    render(<TermsGate><div>App content</div></TermsGate>);
    await waitFor(() => screen.getByRole("dialog"));
    fireEvent.click(screen.getByRole("button", { name: /i agree/i }));

    // Ellipsis replaces "I agree" while POST is in flight
    expect(screen.queryByRole("button", { name: /i agree/i })).toBeNull();
    expect(screen.getAllByRole("button").some((b) => b.textContent === "…")).toBeTruthy();

    act(() => { resolvePost!(new Response("ok", { status: 200 })); });
  });

  it("has aria-disabled while submitting", async () => {
    let resolvePost: (r: Response) => void;
    const postDeferred = new Promise<Response>((r) => { resolvePost = r; });
    mockFetch(new Response(JSON.stringify({ accepted: false }), { status: 200 }));
    vi.spyOn(global, "fetch").mockImplementation(
      () => postDeferred as unknown as Promise<Response>
    );

    render(<TermsGate><div>App content</div></TermsGate>);
    await waitFor(() => screen.getByRole("dialog"));
    fireEvent.click(screen.getByRole("button", { name: /i agree/i }));

    // Find the ellipsis button and check aria-disabled
    const ellipsisBtn = screen.getAllByRole("button").find((b) => b.textContent === "…");
    expect(ellipsisBtn?.getAttribute("aria-disabled")).toBe("true");

    act(() => { resolvePost!(new Response("ok", { status: 200 })); });
  });
});

describe("TermsGate — error state", () => {
  it("shows an error alert when terms-status fetch fails with non-401", async () => {
    mockFetch(new Response("Gateway Timeout", { status: 504 }));
    render(<TermsGate><div>App content</div></TermsGate>);
    await waitFor(() => {
      expect(screen.getByRole("alert")).toBeTruthy();
    });
  });

  it("error alert contains the status code", async () => {
    mockFetch(new Response(null, { status: 503 }));
    render(<TermsGate><div>App content</div></TermsGate>);
    await waitFor(() => {
      expect(screen.getByRole("alert")).toBeTruthy();
    });
    expect(screen.getByRole("alert").textContent).toMatch(/503/);
  });
});

describe("TermsGate — children always rendered", () => {
  it("renders children even when modal is shown (does not gate them)", async () => {
    mockFetch(new Response(JSON.stringify({ accepted: false }), { status: 200 }));
    render(
      <TermsGate>
        <div data-testid="children-visible">Behind the modal</div>
      </TermsGate>
    );
    await waitFor(() => screen.getByRole("dialog"));
    expect(screen.getByTestId("children-visible")).toBeTruthy();
  });
});
