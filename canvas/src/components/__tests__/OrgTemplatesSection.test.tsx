// @vitest-environment jsdom

/**
 * Tests for OrgTemplatesSection — collapsible org template import list.
 *
 * Covers:
 *   - Header with count badge (visible only when expanded)
 *   - Collapsed by default, aria-expanded toggles on click
 *   - aria-controls targets org-templates-body div
 *   - Empty state when no org templates
 *   - Loading spinner
 *   - Org template cards: name, description, workspace count
 *   - Import button per card
 *   - Preflight modal opens when org has required_env
 *   - Preflight onProceed fires import
 *   - Preflight onCancel closes modal
 *   - Direct import (no modal) when org has no env requirements
 *   - Import button disabled while that org is importing
 */
// ── ALL mocks MUST be before imports (vi.mock is hoisted to top of file) ───────
const { mockGet, mockPost, mockListSecrets } = vi.hoisted(() => ({
  mockGet: vi.fn(),
  mockPost: vi.fn(),
  mockListSecrets: vi.fn(),
}));

vi.mock("@/lib/api", () => ({
  api: { get: mockGet, post: mockPost },
}));

vi.mock("@/lib/api/secrets", () => ({
  listSecrets: mockListSecrets,
}));

vi.mock("@/store/canvas", () => ({
  useCanvasStore: Object.assign(
    vi.fn(),
    { getState: () => ({ nodes: [], hydrate: vi.fn() }) },
  ),
}));

vi.mock("../Spinner", () => ({
  Spinner: () => <span data-testid="spinner" aria-hidden="true" />,
}));

vi.mock("../OrgImportPreflightModal", () => ({
  OrgImportPreflightModal: vi.fn(({ open, onCancel, onProceed }) =>
    open ? (
      <div data-testid="preflight-modal">
        <button onClick={onProceed}>Import</button>
        <button onClick={onCancel}>Cancel</button>
      </div>
    ) : null
  ),
}));

vi.mock("../ConfirmDialog", () => ({ ConfirmDialog: () => null }));
vi.mock("@/components/Toaster", () => ({ showToast: vi.fn() }));

import React from "react";
import { render, screen, fireEvent, cleanup, act, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { OrgTemplatesSection } from "../TemplatePalette";

// ── Shared data ─────────────────────────────────────────────────────────────
const MOCK_ORGS = [
  { dir: "sample-org", name: "Free Beats All", description: "d1", workspaces: 3 },
  { dir: "smoke-test", name: "MeDo Smoke Test", description: "d2", workspaces: 1 },
];

beforeEach(() => {
  vi.clearAllMocks();
  mockGet.mockResolvedValue(MOCK_ORGS);
  mockPost.mockResolvedValue({ org: "test", workspaces: [], count: 0 });
  mockListSecrets.mockResolvedValue([]);
});

afterEach(() => {
  cleanup();
});


async function expandSection() {
  const toggle = (await screen.findAllByRole("button")).find(
    (b) => b.getAttribute("aria-controls") === "org-templates-body"
  )!;
  fireEvent.click(toggle);
  await waitFor(() => {
    expect(toggle.getAttribute("aria-expanded")).toBe("true");
  });
}

// ─── Collapse / expand ─────────────────────────────────────────────────────

describe("OrgTemplatesSection — collapse/expand", () => {
  it("renders collapsed by default — org cards NOT in DOM", async () => {
    render(<OrgTemplatesSection />);
    const toggle = (await screen.findAllByRole("button")).find(
      (b) => b.getAttribute("aria-controls") === "org-templates-body"
    )!;
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    await waitFor(() => {
      expect(toggle.textContent).toContain("(2)");
    });
    expect(screen.queryByText("Free Beats All")).toBeNull();
  });

  it("clicking header reveals org cards", async () => {
    render(<OrgTemplatesSection />);
    await expandSection();
    expect(screen.getByText("Free Beats All")).toBeTruthy();
    expect(screen.getByText("MeDo Smoke Test")).toBeTruthy();
  });


  it("clicking header again collapses back", async () => {
    render(<OrgTemplatesSection />);
    await expandSection();
    expect(screen.getByText("Free Beats All")).toBeTruthy();
    const toggle = (await screen.findAllByRole("button")).find(
      (b) => b.getAttribute("aria-controls") === "org-templates-body"
    )!;
    fireEvent.click(toggle);
    await waitFor(() => {
      expect(toggle.getAttribute("aria-expanded")).toBe("false");
    });
    expect(screen.queryByText("Free Beats All")).toBeNull();
  });


  it("count badge appears after load", async () => {
    render(<OrgTemplatesSection />);
    const toggle = (await screen.findAllByRole("button")).find(
      (b) => b.getAttribute("aria-controls") === "org-templates-body"
    )!;
    await waitFor(() => {
      expect(toggle.textContent).toContain("(2)");
    });
  });
});

// ─── States ─────────────────────────────────────────────────────────────────

describe("OrgTemplatesSection — states", () => {
  it("shows empty state when no org templates", async () => {
    mockGet.mockResolvedValue([]);
    render(<OrgTemplatesSection />);
    await expandSection();
    expect(screen.getByText(/no org templates/i)).toBeTruthy();
    expect(screen.getByText(/org-templates\//i)).toBeTruthy();
  });

  it("shows loading spinner while fetching", async () => {
    mockGet.mockImplementation(() => new Promise(() => {}));
    render(<OrgTemplatesSection />);
    await expandSection();
    expect(screen.getByTestId("spinner")).toBeTruthy();
    expect(screen.getByText(/loading/i)).toBeTruthy();
  });

  it("shows workspace count badge on org card", async () => {
    render(<OrgTemplatesSection />);
    await expandSection();
    expect(screen.getByText(/3 workspaces/i)).toBeTruthy();
  });

  it("shows org description on card", async () => {
    render(<OrgTemplatesSection />);
    await expandSection();
    expect(screen.getByText("d1")).toBeTruthy();
  });
});

// ─── Import ─────────────────────────────────────────────────────────────────

describe("OrgTemplatesSection — import", () => {
  it("Import button is present for each org", async () => {
    render(<OrgTemplatesSection />);
    await expandSection();
    const importBtns = screen.getAllByRole("button", { name: /import org/i });
    expect(importBtns.length).toBe(2);
  });

  it("preflight modal opens when org has required_env", async () => {
    mockGet.mockResolvedValue([
      { ...MOCK_ORGS[0], required_env: [{ key: "ANTHROPIC_API_KEY" }] },
    ]);
    render(<OrgTemplatesSection />);
    await expandSection();
    fireEvent.click(screen.getAllByRole("button", { name: /import org/i })[0]);
    await waitFor(() => {
      expect(screen.getByTestId("preflight-modal")).toBeTruthy();
    });
  });

  it("preflight onCancel closes the modal", async () => {
    mockGet.mockResolvedValue([
      { ...MOCK_ORGS[0], required_env: [{ key: "STRIPE_KEY" }] },
    ]);
    render(<OrgTemplatesSection />);
    await expandSection();
    fireEvent.click(screen.getAllByRole("button", { name: /import org/i })[0]);
    await waitFor(() => {
      expect(screen.getByTestId("preflight-modal")).toBeTruthy();
    });
    await act(async () => {
      screen.getByRole("button", { name: "Cancel" }).click();
    });
    await waitFor(() => {
      expect(screen.queryByTestId("preflight-modal")).toBeNull();
    });
  });

  it("no preflight modal when org has only recommended_env (direct import)", async () => {
    mockGet.mockResolvedValue([
      { ...MOCK_ORGS[0], required_env: [], recommended_env: [{ key: "OPTIONAL" }] },
    ]);
    render(<OrgTemplatesSection />);
    await expandSection();
    fireEvent.click(screen.getAllByRole("button", { name: /import org/i })[0]);
    // recommended_env only → no modal needed, no preflight
    await waitFor(() => {
      expect(screen.queryByTestId("preflight-modal")).toBeNull();
    });
  });

  it("Import button disabled while that org is importing", async () => {
    mockPost.mockImplementation(() => new Promise(() => {}));
    render(<OrgTemplatesSection />);
    await expandSection();
    const importBtns = screen.getAllByRole("button", { name: /import org/i });
    fireEvent.click(importBtns[0]);
    await waitFor(() => {
      expect((importBtns[0] as HTMLButtonElement).disabled).toBe(true);
    });
  });
});
