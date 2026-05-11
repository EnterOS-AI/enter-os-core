// @vitest-environment jsdom
/**
 * Tests for EmptyState — the full-canvas welcome card shown on first load.
 *
 * Covers:
 *   - Loading state (GET /templates in flight)
 *   - Fetch failure → empty template grid (templates = [])
 *   - Template grid renders with correct content
 *   - Template button disabled while deploying
 *   - "Deploying..." label on the button being deployed
 *   - "Create blank" button POSTs /workspaces
 *   - "Creating..." label while blank workspace is being created
 *   - Blank create error shows error banner
 *   - Error banner has role="alert"
 *   - All buttons disabled while any deploy is in-flight
 *   - handleDeployed fires after 500ms delay
 *
 * Uses vi.hoisted + vi.mock to fully isolate the api module, matching
 * the pattern established in ApprovalBanner, MemoryTab, and ScheduleTab tests.
 */
import React from "react";
import { render, screen, fireEvent, cleanup, act } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { EmptyState } from "../EmptyState";

// ─── Hoisted mock refs ─────────────────────────────────────────────────────────
// vi.hoisted runs in the same hoisting phase as vi.mock factories, so all refs
// are available both to the factory and to test bodies.
const { mockApiGet, mockApiPost } = vi.hoisted(() => ({
  mockApiGet: vi.fn<(args: unknown[]) => Promise<unknown>>(),
  mockApiPost: vi.fn<(args: unknown[]) => Promise<{ id: string }>>(),
}));

// Mutable deploy state — object reference is const; properties can be mutated.
const _deploy = vi.hoisted(() => ({
  deployFn: vi.fn(),
  deploying: undefined as string | undefined,
  error: undefined as string | undefined,
  modal: null as React.ReactNode,
}));

const { mockSelectNode, mockSetPanelTab } = vi.hoisted(() => ({
  mockSelectNode: vi.fn(),
  mockSetPanelTab: vi.fn(),
}));

// ─── Mocks ────────────────────────────────────────────────────────────────────

vi.mock("@/lib/api", () => ({
  api: {
    get: mockApiGet,
    post: mockApiPost,
  },
}));

vi.mock("@/hooks/useTemplateDeploy", () => ({
  useTemplateDeploy: () => ({
    deploy: _deploy.deployFn,
    deploying: _deploy.deploying,
    error: _deploy.error,
    modal: _deploy.modal,
  }),
}));

vi.mock("@/store/canvas", () => ({
  useCanvasStore: Object.assign(
    vi.fn((selector: (s: { getState: () => { selectNode: typeof mockSelectNode; setPanelTab: typeof mockSetPanelTab } }) => unknown) =>
      selector({
        getState: () => ({
          selectNode: mockSelectNode,
          setPanelTab: mockSetPanelTab,
        }),
      })
    ),
    { getState: () => ({ selectNode: mockSelectNode, setPanelTab: mockSetPanelTab }) }
  ),
}));

vi.mock("../TemplatePalette", () => ({
  OrgTemplatesSection: () => null,
}));

vi.mock("../Spinner", () => ({
  Spinner: () => <span data-testid="spinner">⟳</span>,
}));

vi.mock("@/lib/design-tokens", () => ({
  TIER_CONFIG: {
    1: { label: "T1", color: "text-ink-mid bg-surface-card border border-line", border: "text-ink-mid border-line" },
    2: { label: "T2", color: "text-white bg-accent border border-accent-strong", border: "text-accent border-accent" },
    3: { label: "T3", color: "text-white bg-violet-600 border border-violet-700", border: "text-violet-600 border-violet-500" },
    4: { label: "T4", color: "text-white bg-warm border border-warm", border: "text-warm border-warm" },
  },
}));

// ─── Fixtures ─────────────────────────────────────────────────────────────────

const TEMPLATE = {
  id: "tpl-1",
  name: "Claude Code Agent",
  description: "A general-purpose coding assistant",
  tier: 2,
  skill_count: 3,
  model: "claude-opus-4-5",
};

function template(overrides: Partial<typeof TEMPLATE> = {}): typeof TEMPLATE {
  return { ...TEMPLATE, ...overrides };
}

// ─── Helpers ───────────────────────────────────────────────────────────────────

function renderEmpty() {
  return render(<EmptyState />);
}

// Flush React state + microtasks after an act boundary.
async function flush() {
  await act(async () => { await Promise.resolve(); });
}

// Reset deploy state to defaults before each test.
function resetDeployState() {
  _deploy.deployFn.mockReset();
  _deploy.deploying = undefined;
  _deploy.error = undefined;
  _deploy.modal = null;
}

// ─── Tests ─────────────────────────────────────────────────────────────────────

describe("EmptyState — loading", () => {
  beforeEach(() => {
    mockApiGet.mockReset().mockImplementation(
      () => new Promise(() => {}) // never resolves
    );
  });

  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
  });

  it("shows loading state while GET /templates is pending", async () => {
    renderEmpty();
    await flush();
    expect(screen.getByTestId("spinner")).toBeTruthy();
    expect(screen.getByText("Loading templates...")).toBeTruthy();
  });

  // "create blank" is rendered outside the loading/template-grid conditional,
  // so it is always visible — adjust expectation accordingly.
  it("renders 'create blank' button during loading", async () => {
    renderEmpty();
    await flush();
    expect(screen.getByRole("button", { name: "+ Create blank workspace" })).toBeTruthy();
  });

  it("does not render template buttons while loading", async () => {
    renderEmpty();
    await flush();
    expect(screen.queryByText("Claude Code Agent")).toBeNull();
  });
});

describe("EmptyState — templates", () => {
  beforeEach(() => {
    mockApiGet.mockReset().mockResolvedValue([template()]);
    resetDeployState();
  });

  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
  });

  it("renders the welcome heading", async () => {
    renderEmpty();
    await flush();
    expect(screen.getByText("Deploy your first agent")).toBeTruthy();
  });

  it("renders template buttons with name and description", async () => {
    renderEmpty();
    await flush();
    expect(screen.getByText("Claude Code Agent")).toBeTruthy();
    expect(screen.getByText("A general-purpose coding assistant")).toBeTruthy();
  });

  it("renders tier badge and skill count", async () => {
    renderEmpty();
    await flush();
    expect(screen.getByText("T2")).toBeTruthy();
    // skill_count renders as "3 skills · <model>"
    expect(screen.getByText(/^3 skills/)).toBeTruthy();
  });

  it("renders model name when present", async () => {
    renderEmpty();
    await flush();
    expect(screen.getByText(/claude-opus/i)).toBeTruthy();
  });

  it("calls deploy with the template on click", async () => {
    renderEmpty();
    await flush();
    fireEvent.click(screen.getByText("Claude Code Agent"));
    expect(_deploy.deployFn).toHaveBeenCalledWith(template());
  });

  it("shows 'Deploying...' on the button of the template being deployed", async () => {
    _deploy.deploying = "tpl-1";
    renderEmpty();
    await flush();
    expect(screen.getByText("Deploying...")).toBeTruthy();
  });

  it("disables the template button of the deploying template", async () => {
    _deploy.deploying = "tpl-1";
    renderEmpty();
    await flush();
    const btn = screen.getByText("Deploying...").closest("button") as HTMLButtonElement;
    expect(btn.disabled).toBe(true);
  });

  it("disables 'create blank' while a template is deploying", async () => {
    _deploy.deploying = "tpl-1";
    renderEmpty();
    await flush();
    expect(screen.getByRole("button", { name: "+ Create blank workspace" }).disabled).toBe(true);
  });
});

describe("EmptyState — fetch failure / empty templates", () => {
  beforeEach(() => {
    mockApiGet.mockReset().mockResolvedValue([]);
    resetDeployState();
  });

  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
  });

  it("does not render template grid when GET /templates returns []", async () => {
    renderEmpty();
    await flush();
    expect(screen.queryByText("Claude Code Agent")).toBeNull();
  });

  it("renders 'create blank' button when templates list is empty", async () => {
    renderEmpty();
    await flush();
    expect(screen.getByRole("button", { name: "+ Create blank workspace" })).toBeTruthy();
  });

  it("does not render template grid when GET /templates rejects", async () => {
    mockApiGet.mockReset().mockRejectedValue(new Error("Network failure"));
    renderEmpty();
    await flush();
    expect(screen.queryByText("Claude Code Agent")).toBeNull();
  });
});

describe("EmptyState — create blank", () => {
  beforeEach(() => {
    mockApiGet.mockReset().mockResolvedValue([template()]);
    mockApiPost.mockReset().mockResolvedValue({ id: "ws-new" });
    resetDeployState();
    vi.useFakeTimers();
  });

  afterEach(() => {
    cleanup();
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  it("calls POST /workspaces on 'create blank' click", async () => {
    renderEmpty();
    await flush();
    fireEvent.click(screen.getByRole("button", { name: "+ Create blank workspace" }));
    await act(async () => { await Promise.resolve(); });
    expect(mockApiPost).toHaveBeenCalledWith(
      "/workspaces",
      expect.objectContaining({ name: "My First Agent" })
    );
  });

  it("shows 'Creating...' while blank workspace POST is pending", async () => {
    mockApiPost.mockReset().mockImplementation(
      () => new Promise(() => {}) // never resolves
    );
    renderEmpty();
    await flush();
    fireEvent.click(screen.getByRole("button", { name: "+ Create blank workspace" }));
    await act(async () => { await Promise.resolve(); });
    expect(screen.getByRole("button", { name: "Creating..." })).toBeTruthy();
  });

  it("calls selectNode + setPanelTab after 500ms on successful create", async () => {
    renderEmpty();
    await flush();
    fireEvent.click(screen.getByRole("button", { name: "+ Create blank workspace" }));
    await act(async () => { await Promise.resolve(); }); // flush POST
    await act(async () => { vi.advanceTimersByTime(500); });
    expect(mockSelectNode).toHaveBeenCalledWith("ws-new");
    expect(mockSetPanelTab).toHaveBeenCalledWith("chat");
  });

  it("disables template buttons while creating blank workspace", async () => {
    mockApiPost.mockReset().mockImplementation(
      () => new Promise(() => {}) // never resolves
    );
    renderEmpty();
    await flush();
    fireEvent.click(screen.getByRole("button", { name: "+ Create blank workspace" }));
    await act(async () => { await Promise.resolve(); });
    expect((screen.getByText("Claude Code Agent").closest("button") as HTMLButtonElement).disabled).toBe(true);
  });

  it("shows error banner when POST /workspaces fails", async () => {
    mockApiPost.mockReset().mockRejectedValue(new Error("Server error"));
    renderEmpty();
    await flush();
    fireEvent.click(screen.getByRole("button", { name: "+ Create blank workspace" }));
    await act(async () => { await Promise.resolve(); });
    expect(screen.getByRole("alert")).toBeTruthy();
    expect(screen.getByText(/server error/i)).toBeTruthy();
  });

  it("clears 'Creating...' and shows button again after POST failure", async () => {
    mockApiPost.mockReset().mockRejectedValue(new Error("Server error"));
    renderEmpty();
    await flush();
    fireEvent.click(screen.getByRole("button", { name: "+ Create blank workspace" }));
    await act(async () => { await Promise.resolve(); });
    // After rejection, blankCreating = false → button reverts to default label
    expect(screen.getByRole("button", { name: "+ Create blank workspace" })).toBeTruthy();
  });
});

describe("EmptyState — error banner", () => {
  beforeEach(() => {
    mockApiGet.mockReset().mockResolvedValue([template()]);
    resetDeployState();
    vi.useFakeTimers();
  });

  afterEach(() => {
    cleanup();
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  it("has role=alert on the error banner", async () => {
    _deploy.error = "Template deploy failed";
    renderEmpty();
    await flush();
    const alert = screen.getByRole("alert");
    expect(alert).toBeTruthy();
    expect(alert.textContent).toContain("Template deploy failed");
  });

  it("does not show error banner when no errors", async () => {
    renderEmpty();
    await flush();
    expect(screen.queryByRole("alert")).toBeNull();
  });
});
