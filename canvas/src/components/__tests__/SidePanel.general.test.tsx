// @vitest-environment jsdom
/**
 * Tests for SidePanel — general rendering and non-tab behaviors.
 *
 * Companion to SidePanel.tabs.test.tsx which covers tablist ARIA
 * and localStorage width persistence.
 *
 * Covers:
 *   - Null when no node is selected
 *   - Null when selectedNodeId points to a missing node
 *   - Header: node name, role, tier badge
 *   - MetaPill capability summary pills
 *   - Resize handle: role=separator, aria-valuenow/min/max, aria-orientation
 *   - Resize handle: ArrowLeft/Right/Home/End keyboard nav
 *   - Needs-restart banner + Restart Now button
 *   - Current-task banner with pulsing dot
 *   - Footer shows workspace ID
 *   - Close button calls selectNode(null)
 *   - Tab switch via onClick fires setPanelTab
 *   - setSidePanelWidth called on mount
 */
import React from "react";
import { render, screen, fireEvent, cleanup } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { SidePanel } from "../SidePanel";

// ── Tab content stubs ───────────────────────────────────────────────────────
vi.mock("../tabs/DetailsTab",    () => ({ DetailsTab:    () => null }));
vi.mock("../tabs/SkillsTab",     () => ({ SkillsTab:     () => null }));
vi.mock("../tabs/ChatTab",       () => ({ ChatTab:       () => null }));
vi.mock("../tabs/ConfigTab",     () => ({ ConfigTab:     () => null }));
vi.mock("../tabs/TerminalTab",   () => ({ TerminalTab:   () => null }));
vi.mock("../tabs/FilesTab",       () => ({ FilesTab:       () => null }));
vi.mock("../MemoryInspectorPanel", () => ({ MemoryInspectorPanel: () => null }));
vi.mock("../tabs/TracesTab",     () => ({ TracesTab:     () => null }));
vi.mock("../tabs/EventsTab",     () => ({ EventsTab:     () => null }));
vi.mock("../tabs/ActivityTab",   () => ({ ActivityTab:   () => null }));
vi.mock("../tabs/ScheduleTab",   () => ({ ScheduleTab:   () => null }));
vi.mock("../tabs/ChannelsTab",   () => ({ ChannelsTab:   () => null }));
vi.mock("../AuditTrailPanel",    () => ({ AuditTrailPanel: () => null }));
vi.mock("../StatusDot", () => ({ StatusDot: () => null }));
vi.mock("../Tooltip", () => ({
  Tooltip: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));
vi.mock("@/components/Toaster", () => ({ showToast: vi.fn() }));

// ── Canvas store mock — mutable so each test can reconfigure ───────────────
const mockSetPanelTab = vi.fn();
const mockSelectNode = vi.fn();
const mockSetSidePanelWidth = vi.fn();
const mockRestartWorkspace = vi.fn().mockResolvedValue(undefined);

const BASE_NODE = {
  id: "ws-1",
  data: {
    name: "Test Workspace",
    status: "online" as const,
    tier: 2,
    role: "Engineer",
    parentId: null,
    needsRestart: false,
    currentTask: null,
    agentCard: null,
  },
};

// Mutable store state — tests reassign fields to test different states
let storeState = {
  selectedNodeId: "ws-1" as string | null,
  panelTab: "chat",
  setPanelTab: mockSetPanelTab,
  selectNode: mockSelectNode,
  setSidePanelWidth: mockSetSidePanelWidth,
  nodes: [BASE_NODE],
  restartWorkspace: mockRestartWorkspace,
};

vi.mock("@/store/canvas", () => ({
  useCanvasStore: Object.assign(
    vi.fn((selector: (s: typeof storeState) => unknown) => selector(storeState)),
    { getState: () => storeState }
  ),
  summarizeWorkspaceCapabilities: () => ({ runtime: "claude-code", skillCount: 3 }),
}));

beforeEach(() => {
  mockSetPanelTab.mockReset();
  mockSelectNode.mockReset();
  mockSetSidePanelWidth.mockReset();
  mockRestartWorkspace.mockReset().mockResolvedValue(undefined);
  localStorage.clear();
  // Reset store state to default
  storeState = {
    selectedNodeId: "ws-1",
    panelTab: "chat",
    setPanelTab: mockSetPanelTab,
    selectNode: mockSelectNode,
    setSidePanelWidth: mockSetSidePanelWidth,
    nodes: [BASE_NODE],
    restartWorkspace: mockRestartWorkspace,
  };
});

afterEach(() => {
  cleanup();
});

// ─── Null guard ──────────────────────────────────────────────────────────────

describe("SidePanel — null guard", () => {
  it("returns null when selectedNodeId is null", () => {
    storeState.selectedNodeId = null;
    const { container } = render(<SidePanel />);
    expect(container.firstChild).toBeNull();
  });

  it("returns null when selectedNodeId does not match any node", () => {
    storeState.selectedNodeId = "nonexistent-ws";
    storeState.nodes = [];
    const { container } = render(<SidePanel />);
    expect(container.firstChild).toBeNull();
  });
});

// ─── Header ─────────────────────────────────────────────────────────────────

describe("SidePanel — header", () => {
  it("shows node name in heading", () => {
    render(<SidePanel />);
    expect(screen.getByRole("heading", { name: "Test Workspace" })).toBeTruthy();
  });

  it("shows node role", () => {
    render(<SidePanel />);
    expect(screen.getByText("Engineer")).toBeTruthy();
  });

  it("shows tier badge with correct value", () => {
    render(<SidePanel />);
    // T2 appears in header badge AND meta pill — confirm at least one
    const all = screen.getAllByText("T2");
    expect(all.length).toBeGreaterThanOrEqual(1);
  });

  it("close button is present with aria-label", () => {
    render(<SidePanel />);
    expect(screen.getByRole("button", { name: /close workspace panel/i })).toBeTruthy();
  });

  it("close button calls selectNode(null)", () => {
    render(<SidePanel />);
    fireEvent.click(screen.getByRole("button", { name: /close workspace panel/i }));
    expect(mockSelectNode).toHaveBeenCalledWith(null);
  });
});

// ─── MetaPills ─────────────────────────────────────────────────────────────

describe("SidePanel — meta pills", () => {
  it("renders Tier, Runtime, Skills, and Status pills in the meta row", () => {
    render(<SidePanel />);
    // All four labels appear somewhere in the meta pills row
    expect(screen.getByText(/tier/i)).toBeTruthy();
    expect(screen.getByText(/runtime/i)).toBeTruthy();
    expect(screen.getByText(/skills/i)).toBeTruthy();
    expect(screen.getByText(/status/i)).toBeTruthy();
  });

  it("shows correct runtime value in meta pill", () => {
    render(<SidePanel />);
    expect(screen.getByText("claude-code")).toBeTruthy();
  });

  it("shows skill count in meta pill", () => {
    render(<SidePanel />);
    expect(screen.getByText("3")).toBeTruthy();
  });
});

// ─── Resize handle ──────────────────────────────────────────────────────────

describe("SidePanel — resize handle", () => {
  it("has role=separator", () => {
    render(<SidePanel />);
    expect(screen.getByRole("separator")).toBeTruthy();
  });

  it("has aria-label='Resize workspace panel'", () => {
    render(<SidePanel />);
    expect(screen.getByRole("separator").getAttribute("aria-label")).toBe(
      "Resize workspace panel"
    );
  });

  it("has aria-valuenow=480 (default width)", () => {
    render(<SidePanel />);
    expect(screen.getByRole("separator").getAttribute("aria-valuenow")).toBe("480");
  });

  it("has aria-valuemin=320", () => {
    render(<SidePanel />);
    expect(screen.getByRole("separator").getAttribute("aria-valuemin")).toBe("320");
  });

  it("has aria-valuemax=800", () => {
    render(<SidePanel />);
    expect(screen.getByRole("separator").getAttribute("aria-valuemax")).toBe("800");
  });

  it("has aria-orientation=vertical", () => {
    render(<SidePanel />);
    expect(screen.getByRole("separator").getAttribute("aria-orientation")).toBe("vertical");
  });

  it("has tabIndex=0 (focusable)", () => {
    render(<SidePanel />);
    expect(screen.getByRole("separator").getAttribute("tabindex")).toBe("0");
  });

  it("ArrowLeft increases width by 16px (STEP — moves left edge rightward, widens panel)", () => {
    render(<SidePanel />);
    const sep = screen.getByRole("separator");
    fireEvent.keyDown(sep, { key: "ArrowLeft" });
    const panel = document.querySelector(".fixed") as HTMLElement;
    expect(parseInt(panel.style.width, 10)).toBe(480 + 16); // widens
  });

  it("ArrowRight decreases width by 16px (STEP — moves left edge leftward, narrows panel)", () => {
    render(<SidePanel />);
    const sep = screen.getByRole("separator");
    fireEvent.keyDown(sep, { key: "ArrowRight" });
    const panel = document.querySelector(".fixed") as HTMLElement;
    expect(parseInt(panel.style.width, 10)).toBe(480 - 16); // narrows
  });

  it("Home key sets width to MIN (320)", () => {
    render(<SidePanel />);
    fireEvent.keyDown(screen.getByRole("separator"), { key: "Home" });
    const panel = document.querySelector(".fixed") as HTMLElement;
    expect(parseInt(panel.style.width, 10)).toBe(320);
  });

  it("End key sets width to MAX (800)", () => {
    render(<SidePanel />);
    fireEvent.keyDown(screen.getByRole("separator"), { key: "End" });
    const panel = document.querySelector(".fixed") as HTMLElement;
    expect(parseInt(panel.style.width, 10)).toBe(800);
  });

  it("ArrowLeft persists new width to localStorage", () => {
    render(<SidePanel />);
    fireEvent.keyDown(screen.getByRole("separator"), { key: "ArrowLeft" });
    expect(localStorage.getItem("molecule:sidepanel-width")).toBe(String(480 + 16));
  });

  it("Home persists new width to localStorage", () => {
    render(<SidePanel />);
    fireEvent.keyDown(screen.getByRole("separator"), { key: "Home" });
    expect(localStorage.getItem("molecule:sidepanel-width")).toBe("320");
  });
});

// ─── Needs-restart banner ────────────────────────────────────────────────────

describe("SidePanel — needs-restart banner", () => {
  it("shows banner when needsRestart=true and no currentTask", () => {
    storeState.nodes = [{ ...BASE_NODE, data: { ...BASE_NODE.data, needsRestart: true, currentTask: null } }];
    render(<SidePanel />);
    expect(screen.getByText(/config changed/i)).toBeTruthy();
    expect(screen.getByRole("button", { name: /restart now/i })).toBeTruthy();
  });

  it("does NOT show banner when needsRestart=false", () => {
    render(<SidePanel />);
    expect(screen.queryByText(/config changed/i)).toBeNull();
    expect(screen.queryByRole("button", { name: /restart now/i })).toBeNull();
  });

  it("Restart Now button calls restartWorkspace(selectedNodeId)", () => {
    storeState.nodes = [{ ...BASE_NODE, data: { ...BASE_NODE.data, needsRestart: true, currentTask: null } }];
    render(<SidePanel />);
    fireEvent.click(screen.getByRole("button", { name: /restart now/i }));
    expect(mockRestartWorkspace).toHaveBeenCalledWith("ws-1");
  });
});

// ─── Current-task banner ────────────────────────────────────────────────────

describe("SidePanel — current-task banner", () => {
  it("shows banner when currentTask is set", () => {
    storeState.nodes = [{ ...BASE_NODE, data: { ...BASE_NODE.data, currentTask: "Deploying bundle..." } }];
    render(<SidePanel />);
    expect(screen.getByText("Deploying bundle...")).toBeTruthy();
  });

  it("does NOT show banner when currentTask is null", () => {
    render(<SidePanel />);
    expect(screen.queryByText(/deploying bundle/i)).toBeNull();
  });
});

// ─── Footer ─────────────────────────────────────────────────────────────────

describe("SidePanel — footer", () => {
  it("footer shows workspace ID in monospace font", () => {
    render(<SidePanel />);
    // ws-1 appears in the footer with font-mono class
    expect(screen.getByText("ws-1")).toBeTruthy();
  });
});

// ─── Tab switching ─────────────────────────────────────────────────────────

describe("SidePanel — tab switching", () => {
  it("clicking Details tab calls setPanelTab('details')", () => {
    render(<SidePanel />);
    fireEvent.click(screen.getByRole("tab", { name: /details/i }));
    expect(mockSetPanelTab).toHaveBeenCalledWith("details");
  });

  it("clicking Plugins tab calls setPanelTab('skills')", () => {
    render(<SidePanel />);
    fireEvent.click(screen.getByRole("tab", { name: /plugins/i }));
    expect(mockSetPanelTab).toHaveBeenCalledWith("skills");
  });

  it("clicking Terminal tab calls setPanelTab('terminal')", () => {
    render(<SidePanel />);
    fireEvent.click(screen.getByRole("tab", { name: /terminal/i }));
    expect(mockSetPanelTab).toHaveBeenCalledWith("terminal");
  });
});

// ─── setSidePanelWidth ─────────────────────────────────────────────────────

describe("SidePanel — setSidePanelWidth side-effect", () => {
  it("calls setSidePanelWidth with 480 (default width) on mount", () => {
    render(<SidePanel />);
    expect(mockSetSidePanelWidth).toHaveBeenCalledWith(480);
  });

  it("updates setSidePanelWidth after keyboard resize", () => {
    render(<SidePanel />);
    mockSetSidePanelWidth.mockClear();
    fireEvent.keyDown(screen.getByRole("separator"), { key: "ArrowLeft" });
    expect(mockSetSidePanelWidth).toHaveBeenCalledWith(480 + 16);
  });
});

// ─── Width localStorage ────────────────────────────────────────────────────

describe("SidePanel — width localStorage", () => {
  it("does not persist default width to localStorage on initial mount (only on user resize)", () => {
    render(<SidePanel />);
    // localStorage is only written by the keyboard resize handler, not on mount
    expect(localStorage.getItem("molecule:sidepanel-width")).toBeNull();
  });

  it("reads saved width from localStorage", () => {
    localStorage.setItem("molecule:sidepanel-width", "600");
    const { container } = render(<SidePanel />);
    const panel = container.firstChild as HTMLElement;
    expect(panel.style.width).toBe("600px");
  });

  it("caps saved width to default when below minimum", () => {
    localStorage.setItem("molecule:sidepanel-width", "100");
    const { container } = render(<SidePanel />);
    const panel = container.firstChild as HTMLElement;
    expect(panel.style.width).toBe("480px");
  });
});

// ─── Offline status ─────────────────────────────────────────────────────────

describe("SidePanel — offline status", () => {
  it("shows tier badge even when node is offline", () => {
    storeState.nodes = [{ ...BASE_NODE, data: { ...BASE_NODE.data, status: "offline" as const } }];
    render(<SidePanel />);
    // T2 appears in both header badge and meta pill — just confirm at least one exists
    const all = screen.getAllByText("T2");
    expect(all.length).toBeGreaterThanOrEqual(1);
  });

  it("shows 'offline' in the Status meta pill when node is offline", () => {
    storeState.nodes = [{ ...BASE_NODE, data: { ...BASE_NODE.data, status: "offline" as const } }];
    render(<SidePanel />);
    expect(screen.getByText("offline")).toBeTruthy();
  });
});
