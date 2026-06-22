// @vitest-environment jsdom
/**
 * Tests for OrgInfoTab — surfaces current org name/slug/UUID with copy
 * buttons, plus a list of the user's other orgs when applicable.
 *
 * Covers (≥3 cases per the closing-the-UX-gap brief):
 *   - Loading state (spinner + aria-live)
 *   - Renders current org matched by session.org_id, with UUID + slug + name
 *   - Copy button writes the UUID to navigator.clipboard
 *   - Falls back to host-slug match when session lookup fails
 *   - Lists other orgs when user belongs to multiple
 *   - Self-host fallback: /cp/orgs 404 → /org/identity single-org card (no error)
 *   - Self-host fallback with only a name (slug/org_id unset) — omits empty rows
 *   - Error banner only when BOTH /cp/orgs AND /org/identity fail
 *   - Empty/no-match state renders the recovery hint, not a crash
 */
import React from "react";
import { render, screen, fireEvent, cleanup, act, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { OrgInfoTab } from "../OrgInfoTab";

const mockGet = vi.fn();
const mockFetchSession = vi.fn();
const mockGetTenantSlug = vi.fn();

vi.mock("@/lib/api", () => ({
  api: { get: (...args: unknown[]) => mockGet(...args) },
}));
vi.mock("@/lib/auth", () => ({
  fetchSession: (...args: unknown[]) => mockFetchSession(...args),
}));
vi.mock("@/lib/tenant", () => ({
  getTenantSlug: (...args: unknown[]) => mockGetTenantSlug(...args),
}));

// Stub clipboard
vi.stubGlobal("navigator", {
  clipboard: { writeText: vi.fn().mockResolvedValue(undefined) },
});

beforeEach(() => {
  vi.useRealTimers();
  mockGet.mockReset();
  mockFetchSession.mockReset();
  mockGetTenantSlug.mockReset();
  mockGetTenantSlug.mockReturnValue("");
  vi.mocked(navigator.clipboard.writeText).mockReset();
});

afterEach(() => {
  cleanup();
});

async function flush() {
  await act(async () => {
    await Promise.resolve();
    await Promise.resolve();
  });
}

const AGENTS_TEAM = {
  id: "2355b568-0799-4cc7-9e7f-806747f9958c",
  slug: "agents-team",
  name: "Agents Team",
  status: "running",
};
const OTHER_ORG = {
  id: "11111111-1111-4111-8111-111111111111",
  slug: "skunkworks",
  name: "Skunkworks",
  status: "running",
};

// ─── Loading ─────────────────────────────────────────────────────────────────

describe("OrgInfoTab — loading", () => {
  it("shows spinner while fetching", () => {
    mockGet.mockImplementation(() => new Promise(() => {}));
    mockFetchSession.mockImplementation(() => new Promise(() => {}));
    render(<OrgInfoTab />);
    const status = screen.getByRole("status");
    expect(status).toBeTruthy();
    expect(status.getAttribute("aria-live")).toBe("polite");
    expect(status.textContent).toContain("Loading organization");
  });
});

// ─── Current org renders + copy ──────────────────────────────────────────────

describe("OrgInfoTab — current org", () => {
  it("renders the org matched by session.org_id with name, slug, UUID", async () => {
    mockFetchSession.mockResolvedValue({
      user_id: "u-1",
      org_id: AGENTS_TEAM.id,
      email: "user@example.com",
    });
    mockGet.mockResolvedValue([AGENTS_TEAM, OTHER_ORG]);

    render(<OrgInfoTab />);
    await flush();
    await waitFor(() => screen.getByText("Current Organization"));

    // Name shown
    expect(screen.getByText("Agents Team")).toBeTruthy();
    // Slug shown
    expect(screen.getByText("agents-team")).toBeTruthy();
    // UUID shown
    expect(screen.getByText(AGENTS_TEAM.id)).toBeTruthy();
  });

  it("copy-UUID button writes the UUID to navigator.clipboard", async () => {
    mockFetchSession.mockResolvedValue({
      user_id: "u-1",
      org_id: AGENTS_TEAM.id,
      email: "user@example.com",
    });
    mockGet.mockResolvedValue([AGENTS_TEAM]);

    render(<OrgInfoTab />);
    await flush();
    await waitFor(() => screen.getByText(AGENTS_TEAM.id));

    const copyUuid = screen.getByRole("button", { name: /Copy UUID/i });
    fireEvent.click(copyUuid);

    expect(navigator.clipboard.writeText).toHaveBeenCalledWith(AGENTS_TEAM.id);
    // Optimistic "Copied" label flip
    await waitFor(() =>
      expect(
        screen.getByRole("button", { name: /Copy UUID/i }).textContent,
      ).toContain("Copied"),
    );
  });

  it("copy-Slug button writes the slug to navigator.clipboard", async () => {
    mockFetchSession.mockResolvedValue({
      user_id: "u-1",
      org_id: AGENTS_TEAM.id,
      email: "user@example.com",
    });
    mockGet.mockResolvedValue([AGENTS_TEAM]);

    render(<OrgInfoTab />);
    await flush();
    await waitFor(() => screen.getByText(AGENTS_TEAM.slug));

    fireEvent.click(screen.getByRole("button", { name: /Copy Slug/i }));
    expect(navigator.clipboard.writeText).toHaveBeenCalledWith(AGENTS_TEAM.slug);
  });
});

// ─── Fallback: host-slug match when session fails ────────────────────────────

describe("OrgInfoTab — fallbacks", () => {
  it("falls back to host-slug match when fetchSession rejects", async () => {
    mockFetchSession.mockRejectedValue(new Error("session probe failed"));
    mockGetTenantSlug.mockReturnValue("agents-team");
    mockGet.mockResolvedValue({ orgs: [AGENTS_TEAM, OTHER_ORG] }); // wrapped shape

    render(<OrgInfoTab />);
    await flush();
    await waitFor(() => screen.getByText("Current Organization"));

    expect(screen.getByText("Agents Team")).toBeTruthy();
    expect(screen.getByText(AGENTS_TEAM.id)).toBeTruthy();
  });

  it("lists other orgs the user belongs to under a separate header", async () => {
    mockFetchSession.mockResolvedValue({
      user_id: "u-1",
      org_id: AGENTS_TEAM.id,
      email: "user@example.com",
    });
    mockGet.mockResolvedValue([AGENTS_TEAM, OTHER_ORG]);

    render(<OrgInfoTab />);
    await flush();
    await waitFor(() => screen.getByText(/Your other organizations/));

    expect(screen.getByText("Skunkworks")).toBeTruthy();
    expect(screen.getByText(OTHER_ORG.id)).toBeTruthy();
  });
});

// ─── Self-host fallback: /cp/orgs absent → /org/identity ─────────────────────

describe("OrgInfoTab — self-host fallback", () => {
  it("renders a single org card from /org/identity when /cp/orgs 404s", async () => {
    mockFetchSession.mockResolvedValue(null);
    mockGet.mockImplementation((path: string) => {
      if (path === "/cp/orgs")
        return Promise.reject(new Error("API GET /cp/orgs: 404 page not found"));
      if (path === "/org/identity")
        return Promise.resolve({
          name: "Molecule AI",
          slug: "molecule-ai",
          org_id: "abc-123",
        });
      return Promise.reject(new Error(`unexpected path ${path}`));
    });

    render(<OrgInfoTab />);
    await flush();
    await waitFor(() => screen.getByText("Current Organization"));

    // Single card from /org/identity — name + slug + UUID, no error banner.
    expect(screen.getByText("Molecule AI")).toBeTruthy();
    expect(screen.getByText("molecule-ai")).toBeTruthy();
    expect(screen.getByText("abc-123")).toBeTruthy();
    // No "other organizations" list and no error.
    expect(screen.queryByText(/Your other organizations/)).toBeNull();
    expect(screen.queryByText(/404/)).toBeNull();
  });

  it("renders only the name when slug/org_id are unset (fresh self-host)", async () => {
    mockFetchSession.mockResolvedValue(null);
    mockGet.mockImplementation((path: string) => {
      if (path === "/cp/orgs")
        return Promise.reject(new Error("API GET /cp/orgs: 404 page not found"));
      if (path === "/org/identity")
        return Promise.resolve({ name: "Molecule AI", slug: "", org_id: "" });
      return Promise.reject(new Error(`unexpected path ${path}`));
    });

    render(<OrgInfoTab />);
    await flush();
    await waitFor(() => screen.getByText("Current Organization"));

    expect(screen.getByText("Molecule AI")).toBeTruthy();
    // Empty slug/UUID rows omitted — no copy buttons rendered.
    expect(screen.queryByRole("button", { name: /Copy Slug/i })).toBeNull();
    expect(screen.queryByRole("button", { name: /Copy UUID/i })).toBeNull();
  });
});

// ─── Error + empty handling ──────────────────────────────────────────────────

describe("OrgInfoTab — error + empty", () => {
  it("renders an error banner only when BOTH /cp/orgs and /org/identity fail", async () => {
    mockFetchSession.mockResolvedValue(null);
    mockGet.mockImplementation((path: string) => {
      if (path === "/cp/orgs")
        return Promise.reject(new Error("API GET /cp/orgs: 404 page not found"));
      if (path === "/org/identity")
        return Promise.reject(new Error("API GET /org/identity: 500 boom"));
      return Promise.reject(new Error(`unexpected path ${path}`));
    });

    render(<OrgInfoTab />);
    await flush();
    await waitFor(() => screen.getByText(/500 boom/));
    expect(screen.queryByText("Current Organization")).toBeNull();
  });

  it("renders the recovery hint when /cp/orgs returns an empty list (no crash)", async () => {
    mockFetchSession.mockResolvedValue(null);
    mockGetTenantSlug.mockReturnValue("");
    mockGet.mockImplementation((path: string) =>
      path === "/cp/orgs"
        ? Promise.resolve([])
        : Promise.reject(new Error(`unexpected path ${path}`)),
    );

    render(<OrgInfoTab />);
    await flush();
    await waitFor(() =>
      screen.getByText(/No organization found for this session/),
    );
  });
});
