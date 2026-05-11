// @vitest-environment jsdom
/**
 * Tests for EmptyState component — the full-canvas welcome card on first load.
 *
 * Pattern: all vi.fn() refs are created by a SINGLE vi.hoisted() call,
 * returned as a named-const object. Individual vi.mock factories then
 * import that object and pull out the fields they need. This avoids
 * "Cannot access before initialization" errors from vi.mock hoisting.
 */
import React from "react";
import { render, screen, fireEvent, cleanup, waitFor, act } from "@testing-library/react";
import { afterEach, describe, expect, it, vi, beforeEach } from "vitest";
import { EmptyState } from "../EmptyState";

// ─── Module-level mocks ───────────────────────────────────────────────────────
// vi.hoisted is evaluated after module-level vars are declared, so these
// refs are stable and accessible inside vi.mock factories (which are
// hoisted above everything). We return an object so a SINGLE hoisted call
// creates all mocks; each vi.mock then references m.<field>.
const m = vi.hoisted(() => {
  const mockGet = vi.fn<() => Promise<unknown[]>>();
  const mockPost = vi.fn<() => Promise<{ id: string }>>();
  const mockCheckDeploySecrets = vi.fn<
    () => Promise<{
      ok: boolean;
      missingKeys: string[];
      providers: string[];
      runtime: string;
      configuredKeys: string[];
    }>
  >();
  const mockSelectNode = vi.fn<(id: string) => void>();
  const mockSetPanelTab = vi.fn<(tab: string) => void>();
  const mockDeploy = vi.fn<(t: { id: string; name: string }) => Promise<void>>();
  const mockUseTemplateDeploy = vi.fn(() => ({
    deploy: mockDeploy,
    deploying: false,
    error: null,
    modal: null,
  }));

  return {
    mockGet,
    mockPost,
    mockCheckDeploySecrets,
    mockSelectNode,
    mockSetPanelTab,
    mockDeploy,
    mockUseTemplateDeploy,
  };
});

vi.mock("@/lib/api", () => ({
  api: { get: m.mockGet, post: m.mockPost },
}));

vi.mock("@/lib/deploy-preflight", () => ({
  checkDeploySecrets: m.mockCheckDeploySecrets,
}));

vi.mock("@/store/canvas", () => ({
  useCanvasStore: Object.assign(
    // The hook returns an object with selectNode/setPanelTab;
    // the component also calls useCanvasStore.getState() directly.
    vi.fn(() => ({
      selectNode: m.mockSelectNode,
      setPanelTab: m.mockSetPanelTab,
    })),
    {
      getState: () => ({
        selectNode: m.mockSelectNode,
        setPanelTab: m.mockSetPanelTab,
      }),
    },
  ),
}));

vi.mock("@/hooks/useTemplateDeploy", () => ({
  useTemplateDeploy: m.mockUseTemplateDeploy,
}));

// Mock OrgTemplatesSection — tested separately.
vi.mock("../TemplatePalette", () => ({
  OrgTemplatesSection: () => (
    <div data-testid="org-templates-section">Org Templates</div>
  ),
}));

// ─── Test data ───────────────────────────────────────────────────────────────

const TEMPLATE = {
  id: "molecule-dev",
  name: "Molecule Dev",
  tier: 2,
  description: "A full-featured agent workspace for development",
  runtime: "langgraph",
  required_env: ["ANTHROPIC_API_KEY"],
  models: [{ id: "claude-sonnet-4-20250514", required_env: ["ANTHROPIC_API_KEY"] }],
  model: "claude-sonnet-4-20250514",
  skill_count: 12,
};

// ─── Cleanup ─────────────────────────────────────────────────────────────────

beforeEach(() => {
  m.mockGet.mockReset();
  m.mockGet.mockResolvedValue([] as unknown[]);
  m.mockPost.mockReset();
  m.mockPost.mockResolvedValue({ id: "new-ws-123" } as unknown as { id: string });
  m.mockCheckDeploySecrets.mockReset();
  m.mockCheckDeploySecrets.mockResolvedValue({
    ok: true,
    missingKeys: [],
    providers: [],
    runtime: "langgraph",
    configuredKeys: [],
  });
  m.mockSelectNode.mockReset();
  m.mockSetPanelTab.mockReset();
  m.mockDeploy.mockReset();
});

afterEach(() => {
  cleanup();
});

// ─── Tests ────────────────────────────────────────────────────────────────────

describe("EmptyState — loading state", () => {
  it("shows spinner and loading text while templates are being fetched", () => {
    m.mockGet.mockImplementation(() => new Promise(() => {}));
    render(<EmptyState />);
    expect(screen.getByText(/loading templates/i)).toBeTruthy();
  });
});

describe("EmptyState — templates fetched", () => {
  it("renders template grid with name, tier badge, description, skill count", async () => {
    m.mockGet.mockResolvedValueOnce([TEMPLATE] as unknown[]);
    render(<EmptyState />);
    await act(async () => { await new Promise(r => setTimeout(r, 50)); });
    expect(screen.getByText("Molecule Dev")).toBeTruthy();
    expect(screen.getByText("T2")).toBeTruthy();
    expect(screen.getByText(/full-featured agent workspace/i)).toBeTruthy();
    expect(screen.getByText(/12 skills/)).toBeTruthy();
  });

  it("shows model label when template declares a model", async () => {
    m.mockGet.mockResolvedValueOnce([TEMPLATE] as unknown[]);
    render(<EmptyState />);
    await act(async () => { await new Promise(r => setTimeout(r, 50)); });
    expect(screen.getByText(/claude-sonnet/i)).toBeTruthy();
  });

  it("calls deploy(template) when template button is clicked", async () => {
    m.mockGet.mockResolvedValueOnce([TEMPLATE] as unknown[]);
    render(<EmptyState />);
    await act(async () => { await new Promise(r => setTimeout(r, 50)); });
    fireEvent.click(screen.getByRole("button", { name: /molecule dev/i }));
    expect(m.mockDeploy).toHaveBeenCalledWith(
      expect.objectContaining({ id: "molecule-dev", name: "Molecule Dev" }),
    );
  });
});

describe("EmptyState — no templates", () => {
  it("shows only the create-blank button when template list is empty", async () => {
    // beforeEach already sets mockResolvedValue([]) as default — no override needed.
    render(<EmptyState />);
    await act(async () => { await new Promise(r => setTimeout(r, 50)); });
    expect(screen.getByRole("button", { name: /\+ create blank workspace/i })).toBeTruthy();
    expect(screen.queryByText(/molecule dev/i)).toBeNull();
  });

  it("shows only the create-blank button when template fetch fails", async () => {
    m.mockGet.mockRejectedValueOnce(new Error("Network error"));
    render(<EmptyState />);
    await act(async () => { await new Promise(r => setTimeout(r, 50)); });
    expect(screen.getByRole("button", { name: /\+ create blank workspace/i })).toBeTruthy();
    expect(screen.queryByText(/loading templates/i)).toBeNull();
  });
});

describe("EmptyState — create blank workspace", () => {
  it('shows "Creating..." label while blank workspace POST is in-flight', async () => {
    m.mockPost.mockImplementationOnce(() => new Promise(() => {}));
    render(<EmptyState />);
    await act(async () => { await new Promise(r => setTimeout(r, 50)); });
    fireEvent.click(screen.getByRole("button", { name: /\+ create blank workspace/i }));
    await act(async () => { await new Promise(r => setTimeout(r, 50)); });
    expect(screen.getByText("Creating...")).toBeTruthy();
    // The same button is now relabeled; check it is disabled while POST is in-flight.
    expect(screen.getByRole("button", { name: /creating\.\.\./i })).toHaveProperty("disabled", true);
  });

  it("calls POST /workspaces with correct payload on create blank", async () => {
    m.mockPost.mockResolvedValueOnce({ id: "ws-new-456" } as unknown as { id: string });
    render(<EmptyState />);
    await act(async () => { await new Promise(r => setTimeout(r, 50)); });
    fireEvent.click(screen.getByRole("button", { name: /\+ create blank workspace/i }));
    await act(async () => { await new Promise(r => setTimeout(r, 50)); });
    expect(m.mockPost).toHaveBeenCalledWith("/workspaces", {
      name: "My First Agent",
      canvas: { x: 200, y: 150 },
    });
  });

  it("calls selectNode + setPanelTab(chat) after 500ms on blank create success", async () => {
    m.mockPost.mockResolvedValueOnce({ id: "ws-new-789" } as unknown as { id: string });
    render(<EmptyState />);
    await act(async () => { await new Promise(r => setTimeout(r, 50)); });
    fireEvent.click(screen.getByRole("button", { name: /\+ create blank workspace/i }));
    // Wait for the 500ms setTimeout inside handleDeployed to fire and call
    // canvas store methods. Use waitFor so we don't hard-code timing assumptions.
    await waitFor(() => {
      expect(m.mockSelectNode).toHaveBeenCalledWith("ws-new-789");
      expect(m.mockSetPanelTab).toHaveBeenCalledWith("chat");
    }, { timeout: 1000 });
  });

  it("shows error banner on blank create failure", async () => {
    m.mockPost.mockRejectedValueOnce(new Error("Server error"));
    render(<EmptyState />);
    await act(async () => { await new Promise(r => setTimeout(r, 50)); });
    fireEvent.click(screen.getByRole("button", { name: /\+ create blank workspace/i }));
    await act(async () => { await new Promise(r => setTimeout(r, 50)); });
    expect(screen.getByRole("alert")).toBeTruthy();
    expect(screen.getByText(/server error/i)).toBeTruthy();
  });

  it("blank workspace error clears on retry", async () => {
    m.mockPost.mockRejectedValueOnce(new Error("Server error"));
    render(<EmptyState />);
    await act(async () => { await new Promise(r => setTimeout(r, 50)); });
    fireEvent.click(screen.getByRole("button", { name: /\+ create blank workspace/i }));
    await act(async () => { await new Promise(r => setTimeout(r, 50)); });
    expect(screen.getByRole("alert")).toBeTruthy();

    // Retry succeeds — error clears
    m.mockPost.mockResolvedValueOnce({ id: "ws-retry" } as unknown as { id: string });
    fireEvent.click(screen.getByRole("button", { name: /\+ create blank workspace/i }));
    await act(async () => { await new Promise(r => setTimeout(r, 50)); });
    expect(screen.queryByRole("alert")).toBeNull();
  });
});

describe("EmptyState — rendering", () => {
  it("renders the welcome heading and instructions", async () => {
    // beforeEach already sets mockGet to resolve to [] — no override needed.
    render(<EmptyState />);
    await act(async () => { await new Promise(r => setTimeout(r, 50)); });
    expect(screen.getByText(/deploy your first agent/i)).toBeTruthy();
    expect(screen.getByText(/welcome to molecule ai/i)).toBeTruthy();
  });

  it("renders the tips footer", async () => {
    render(<EmptyState />);
    await act(async () => { await new Promise(r => setTimeout(r, 50)); });
    expect(screen.getByText(/drag to nest workspaces/i)).toBeTruthy();
  });

  it("renders OrgTemplatesSection below the create-blank button", async () => {
    render(<EmptyState />);
    await act(async () => { await new Promise(r => setTimeout(r, 50)); });
    expect(screen.getByTestId("org-templates-section")).toBeTruthy();
  });
});
