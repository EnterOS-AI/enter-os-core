// @vitest-environment jsdom
/**
 * Tests for TemplatePalette — the floating sidebar drawer.
 *
 * Covers:
 *   - Toggle button aria-label (open / closed)
 *   - Sidebar renders when open, hides when closed
 *   - Sidebar header: "Templates" heading, subtitle
 *   - Loading state
 *   - Empty state ("No templates found")
 *   - Template cards: name, description, tier badge, skill pills
 *   - Deploy button calls deploy()
 *   - Errors swallowed → empty state shown
 *   - setTemplatePaletteOpen called on open/close
 *   - OrgTemplatesSection rendered inside sidebar
 *   - Import Agent Folder button in footer
 *   - Refresh templates button in footer
 */
import React from "react";
import { render, screen, fireEvent, cleanup, act, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

// ── Hoisted mocks — vi.hoisted() so they're available when vi.mock runs ──────
// IMPORTANT: use plain vi.fn() in the return object (NOT `const fn = vi.fn(); return { fn }`)
const { mockDeploy, mockSetTemplatePaletteOpen, mockGet } = vi.hoisted(() => ({
  mockDeploy: vi.fn(),
  mockSetTemplatePaletteOpen: vi.fn(),
  mockGet: vi.fn(),
}));

vi.mock("@/hooks/useTemplateDeploy", () => ({
  useTemplateDeploy: () => ({
    deploy: mockDeploy,
    deploying: null,
    error: null,
    modal: null,
  }),
}));

vi.mock("@/store/canvas", () => ({
  useCanvasStore: vi.fn((selector: (s: { setTemplatePaletteOpen: typeof mockSetTemplatePaletteOpen }) => unknown) =>
    selector({ setTemplatePaletteOpen: mockSetTemplatePaletteOpen })
  ),
}));

vi.mock("@/lib/api", () => ({
  api: { get: mockGet },
}));

vi.mock("../OrgImportPreflightModal", () => ({
  OrgImportPreflightModal: () => null,
}));

vi.mock("../ConfirmDialog", () => ({
  ConfirmDialog: () => null,
}));

vi.mock("../Spinner", () => ({
  Spinner: () => <span data-testid="spinner" aria-hidden="true" />,
}));

vi.mock("../Toaster", () => ({ showToast: vi.fn() }));

// ── Component import — after all mocks ──────────────────────────────────────
import { TemplatePalette } from "../TemplatePalette";

beforeEach(() => {
  mockDeploy.mockReset();
  mockSetTemplatePaletteOpen.mockReset();
  mockGet.mockReset().mockResolvedValue([]);
});

afterEach(() => {
  cleanup();
});

// ── Helpers ──────────────────────────────────────────────────────────────────

async function flush() {
  await act(async () => { await Promise.resolve(); });
}

const MOCK_TEMPLATES = [
  {
    id: "tmpl-1",
    name: "Software Engineer",
    description: "Best for writing code",
    tier: 1,
    skills: ["web-search", "read-file", "write-file"],
  },
  {
    id: "tmpl-2",
    name: "Researcher",
    description: "Deep research agent",
    tier: 2,
    skills: [],
  },
];

// ─── Toggle button ─────────────────────────────────────────────────────────

describe("TemplatePalette — toggle button", () => {
  it("has aria-label='Open template palette' when closed", () => {
    render(<TemplatePalette />);
    expect(screen.getByRole("button", { name: /open template palette/i })).toBeTruthy();
  });

  it("has aria-label='Close template palette' when open", async () => {
    render(<TemplatePalette />);
    fireEvent.click(screen.getByRole("button", { name: /open template palette/i }));
    await flush();
    expect(screen.getByRole("button", { name: /close template palette/i })).toBeTruthy();
  });

  it("clicking toggle opens sidebar", async () => {
    render(<TemplatePalette />);
    fireEvent.click(screen.getByRole("button", { name: /open template palette/i }));
    await flush();
    expect(screen.getByRole("heading", { name: "Templates" })).toBeTruthy();
  });

  it("clicking toggle again closes sidebar", async () => {
    render(<TemplatePalette />);
    fireEvent.click(screen.getByRole("button", { name: /open template palette/i }));
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /close template palette/i }));
    await flush();
    expect(screen.queryByRole("heading", { name: "Templates" })).toBeNull();
  });

  it("calls setTemplatePaletteOpen(true) when opened", async () => {
    render(<TemplatePalette />);
    fireEvent.click(screen.getByRole("button", { name: /open template palette/i }));
    await flush();
    expect(mockSetTemplatePaletteOpen).toHaveBeenCalledWith(true);
  });

  it("calls setTemplatePaletteOpen(false) when closed", async () => {
    render(<TemplatePalette />);
    fireEvent.click(screen.getByRole("button", { name: /open template palette/i }));
    await flush();
    mockSetTemplatePaletteOpen.mockClear();
    fireEvent.click(screen.getByRole("button", { name: /close template palette/i }));
    await flush();
    expect(mockSetTemplatePaletteOpen).toHaveBeenCalledWith(false);
  });
});

// ─── Sidebar content ───────────────────────────────────────────────────────

describe("TemplatePalette — sidebar", () => {
  async function openSidebar() {
    fireEvent.click(screen.getByRole("button", { name: /open template palette/i }));
    await flush();
  }

  it("shows 'Templates' heading", async () => {
    render(<TemplatePalette />);
    await openSidebar();
    expect(screen.getByRole("heading", { name: "Templates" })).toBeTruthy();
  });

  it("shows subtitle 'Click to deploy a workspace'", async () => {
    render(<TemplatePalette />);
    await openSidebar();
    expect(screen.getByText(/click to deploy a workspace/i)).toBeTruthy();
  });

  it("shows loading state", async () => {
    mockGet.mockReturnValue(new Promise(() => {}));
    render(<TemplatePalette />);
    await openSidebar();
    expect(screen.getByTestId("spinner")).toBeTruthy();
    expect(screen.getByText(/loading/i)).toBeTruthy();
  });

  it("shows empty state when no templates", async () => {
    mockGet.mockResolvedValue([]);
    render(<TemplatePalette />);
    await openSidebar();
    expect(screen.getByText(/no templates found/i)).toBeTruthy();
  });

  it("renders template cards", async () => {
    mockGet.mockResolvedValue(MOCK_TEMPLATES);
    render(<TemplatePalette />);
    await openSidebar();
    expect(screen.getByText("Software Engineer")).toBeTruthy();
    expect(screen.getByText("Researcher")).toBeTruthy();
  });

  it("hides runtime-default templates from the deployable product template list", async () => {
    mockGet.mockResolvedValue([
      { id: "claude-code-default", name: "Claude Code Agent", description: "", tier: 4, skills: [] },
      { id: "codex", name: "OpenAI Codex CLI", description: "", tier: 4, skills: [] },
      { id: "hermes", name: "Hermes Agent", description: "", tier: 4, skills: [] },
      { id: "openclaw", name: "OpenClaw Agent", description: "", tier: 4, skills: [] },
      { id: "seo-agent", name: "SEO Agent", description: "SEO workspace template", tier: 4, skills: ["seo"] },
    ]);
    render(<TemplatePalette />);
    await openSidebar();
    expect(screen.getByText("SEO Agent")).toBeTruthy();
    expect(screen.queryByText("Claude Code Agent")).toBeNull();
    expect(screen.queryByText("OpenAI Codex CLI")).toBeNull();
    expect(screen.queryByText("Hermes Agent")).toBeNull();
    expect(screen.queryByText("OpenClaw Agent")).toBeNull();
  });

  it("shows template description", async () => {
    mockGet.mockResolvedValue(MOCK_TEMPLATES);
    render(<TemplatePalette />);
    await openSidebar();
    expect(screen.getByText(/best for writing code/i)).toBeTruthy();
  });

  it("shows tier badge on template card", async () => {
    mockGet.mockResolvedValue(MOCK_TEMPLATES);
    render(<TemplatePalette />);
    await openSidebar();
    // T1 appears in tier badge
    expect(screen.getAllByText("T1").length).toBeGreaterThanOrEqual(1);
  });

  it("shows up to 3 skill pills", async () => {
    mockGet.mockResolvedValue(MOCK_TEMPLATES);
    render(<TemplatePalette />);
    await openSidebar();
    expect(screen.getByText("web-search")).toBeTruthy();
    expect(screen.getByText("read-file")).toBeTruthy();
    expect(screen.getByText("write-file")).toBeTruthy();
  });

  it("shows '+N more' when more than 3 skills", async () => {
    mockGet.mockResolvedValue([
      { id: "tmpl-many", name: "Full Stack", description: "", tier: 1, skills: ["a", "b", "c", "d", "e"] },
    ]);
    render(<TemplatePalette />);
    await openSidebar();
    expect(screen.getByText("+2")).toBeTruthy();
  });

  it("deploy button calls deploy(t)", async () => {
    mockGet.mockResolvedValue(MOCK_TEMPLATES);
    render(<TemplatePalette />);
    await openSidebar();
    const deployBtns = screen.getAllByRole("button", { name: /software engineer/i });
    await act(async () => { deployBtns[0].click(); });
    expect(mockDeploy).toHaveBeenCalledWith(MOCK_TEMPLATES[0]);
  });

  it("shows empty state when api.get rejects (error is swallowed)", async () => {
    mockGet.mockRejectedValue(new Error("server error"));
    render(<TemplatePalette />);
    await openSidebar();
    await waitFor(() => {
      expect(screen.getByText(/no templates found/i)).toBeTruthy();
    });
  });

  it("renders OrgTemplatesSection inside sidebar", async () => {
    render(<TemplatePalette />);
    await openSidebar();
    expect(document.querySelector("[data-testid='org-templates-section']")).toBeTruthy();
  });

  it("renders Import Agent Folder button in footer", async () => {
    render(<TemplatePalette />);
    await openSidebar();
    expect(screen.getByRole("button", { name: /import agent folder/i })).toBeTruthy();
  });

  it("renders Refresh templates button in footer", async () => {
    render(<TemplatePalette />);
    await openSidebar();
    expect(screen.getByRole("button", { name: /^refresh templates$/i })).toBeTruthy();
  });
});
