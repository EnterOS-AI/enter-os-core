// @vitest-environment jsdom
import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import { render, screen, fireEvent, cleanup } from "@testing-library/react";

afterEach(() => {
  cleanup();
});

// ── Mock all tab content components to null ──────────────────────────────────
vi.mock("../tabs/DetailsTab", () => ({ DetailsTab: () => null }));
vi.mock("../tabs/SkillsTab", () => ({ SkillsTab: () => null }));
vi.mock("../tabs/ChatTab", () => ({ ChatTab: () => null }));
vi.mock("../tabs/ConfigTab", () => ({ ConfigTab: () => null }));
vi.mock("../tabs/ContainerConfigTab", () => ({ ContainerConfigTab: () => null }));
vi.mock("../tabs/DisplayTab", () => ({ DisplayTab: () => null }));
vi.mock("../tabs/TerminalTab", () => ({ TerminalTab: () => null }));
vi.mock("../tabs/FilesTab", () => ({ FilesTab: () => null }));
vi.mock("../MemoryInspectorPanel", () => ({ MemoryInspectorPanel: () => null }));
vi.mock("../tabs/TracesTab", () => ({ TracesTab: () => null }));
vi.mock("../tabs/EventsTab", () => ({ EventsTab: () => null }));
vi.mock("../tabs/ActivityTab", () => ({ ActivityTab: () => null }));
vi.mock("../tabs/ScheduleTab", () => ({ ScheduleTab: () => null }));
vi.mock("../tabs/ChannelsTab", () => ({ ChannelsTab: () => null }));
vi.mock("../AuditTrailPanel", () => ({ AuditTrailPanel: () => null }));

// ── Mock StatusDot and Tooltip ───────────────────────────────────────────────
vi.mock("../StatusDot", () => ({ StatusDot: () => null }));
vi.mock("../Tooltip", () => ({
  Tooltip: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));
vi.mock("@/components/Toaster", () => ({ showToast: vi.fn() }));

// ── Mock canvas store ────────────────────────────────────────────────────────
// Use vi.hoisted() so mock refs are available in the vi.mock factory
// and in test bodies without triggering vitest's top-level variable rule.
const { mockSetPanelTab } = vi.hoisted(() => ({ mockSetPanelTab: vi.fn() }));

const mockStoreState = {
  selectedNodeId: "ws-1",
  panelTab: "chat",
  setPanelTab: mockSetPanelTab,
  selectNode: vi.fn(),
  // Consumed by SidePanel's useEffect — publishes the drag-resized
  // width to the store so Toolbar can re-centre itself on the
  // remaining canvas area when the panel is open.
  setSidePanelWidth: vi.fn(),
  nodes: [
    {
      id: "ws-1",
      data: {
        name: "Test WS",
        status: "online",
        tier: 1,
        role: "Engineer",
        parentId: null,
        needsRestart: false,
        currentTask: null,
        agentCard: null,
      },
    },
  ],
};

vi.mock("@/store/canvas", () => ({
  useCanvasStore: Object.assign(
    vi.fn((selector: (s: typeof mockStoreState) => unknown) =>
      selector(mockStoreState)
    ),
    { getState: () => mockStoreState }
  ),
  summarizeWorkspaceCapabilities: () => ({ runtime: "claude-code", skillCount: 0 }),
}));

// ── Import component under test AFTER all mocks ──────────────────────────────
import { SidePanel } from "../SidePanel";

const TABS = [
  "chat", "activity", "details", "skills", "terminal",
  "display", "container-config", "config", "schedule", "channels", "files", "memory", "traces", "events", "audit",
];

describe("SidePanel — ARIA tablist pattern", () => {
  it("renders a tablist with aria-label='Workspace panel tabs'", () => {
    render(<SidePanel />);
    const tablist = screen.getByRole("tablist");
    expect(tablist).toBeTruthy();
    expect(tablist.getAttribute("aria-label")).toBe("Workspace panel tabs");
  });

  it("renders exactly 15 tab buttons", () => {
    render(<SidePanel />);
    const tabs = screen.getAllByRole("tab");
    expect(tabs.length).toBe(15);
  });

  it("renders the Display tab", () => {
    render(<SidePanel />);
    expect(document.getElementById("tab-display")).toBeTruthy();
  });

  it("renders the Container Config tab", () => {
    render(<SidePanel />);
    expect(document.getElementById("tab-container-config")).toBeTruthy();
  });

  it("active tab (chat) has aria-selected='true'", () => {
    render(<SidePanel />);
    const chatTab = screen.getAllByRole("tab").find(
      (t) => t.id === "tab-chat"
    );
    expect(chatTab?.getAttribute("aria-selected")).toBe("true");
  });

  it("all other 14 tabs have aria-selected='false'", () => {
    render(<SidePanel />);
    const tabs = screen.getAllByRole("tab");
    const inactive = tabs.filter((t) => t.id !== "tab-chat");
    expect(inactive.length).toBe(14);
    for (const tab of inactive) {
      expect(tab.getAttribute("aria-selected")).toBe("false");
    }
  });

  it("active tab has tabIndex=0 and all others have tabIndex=-1 (roving tabIndex)", () => {
    render(<SidePanel />);
    const tabs = screen.getAllByRole("tab");
    const zeros = tabs.filter((t) => t.getAttribute("tabindex") === "0");
    const minusOnes = tabs.filter((t) => t.getAttribute("tabindex") === "-1");
    expect(zeros.length).toBe(1);
    expect(zeros[0].id).toBe("tab-chat");
    expect(minusOnes.length).toBe(14);
  });

  it("active tab has aria-controls='panel-chat' and id='tab-chat'", () => {
    render(<SidePanel />);
    const chatTab = document.getElementById("tab-chat");
    expect(chatTab).toBeTruthy();
    expect(chatTab?.getAttribute("aria-controls")).toBe("panel-chat");
  });

  it("renders a role=tabpanel for the active tab", () => {
    render(<SidePanel />);
    const tabpanel = screen.getByRole("tabpanel");
    expect(tabpanel).toBeTruthy();
  });

  it("tabpanel has id='panel-chat' and aria-labelledby='tab-chat'", () => {
    render(<SidePanel />);
    const panel = document.getElementById("panel-chat");
    expect(panel).toBeTruthy();
    expect(panel?.getAttribute("aria-labelledby")).toBe("tab-chat");
  });

  it("ArrowRight calls setPanelTab with 'activity' (next tab after chat)", () => {
    render(<SidePanel />);
    const tablist = screen.getByRole("tablist");
    fireEvent.keyDown(tablist, { key: "ArrowRight" });
    expect(mockSetPanelTab).toHaveBeenCalledWith("activity");
  });

  it("ArrowLeft from 'chat' (first) wraps to 'audit' (last)", () => {
    render(<SidePanel />);
    const tablist = screen.getByRole("tablist");
    fireEvent.keyDown(tablist, { key: "ArrowLeft" });
    expect(mockSetPanelTab).toHaveBeenCalledWith("audit");
  });

  it("Home key calls setPanelTab with 'chat' (first tab)", () => {
    render(<SidePanel />);
    const tablist = screen.getByRole("tablist");
    fireEvent.keyDown(tablist, { key: "Home" });
    expect(mockSetPanelTab).toHaveBeenCalledWith("chat");
  });

  it("End key calls setPanelTab with 'audit' (last tab)", () => {
    render(<SidePanel />);
    const tablist = screen.getByRole("tablist");
    fireEvent.keyDown(tablist, { key: "End" });
    expect(mockSetPanelTab).toHaveBeenCalledWith("audit");
  });
});

describe("SidePanel — localStorage width persistence (issue #425)", () => {
  const STORAGE_KEY = "molecule:sidepanel-width";

  beforeEach(() => {
    localStorage.clear();
  });

  afterEach(() => {
    cleanup();
    localStorage.clear();
  });

  it("falls back to 480px when localStorage has no saved width", () => {
    const { container } = render(<SidePanel />);
    const panel = container.firstChild as HTMLElement;
    // The outermost div has style={{ width }}
    expect(panel.style.width).toBe("480px");
  });

  it("reads a valid saved width from localStorage on mount", () => {
    localStorage.setItem(STORAGE_KEY, "600");
    const { container } = render(<SidePanel />);
    const panel = container.firstChild as HTMLElement;
    expect(panel.style.width).toBe("600px");
  });

  it("falls back to 480px when localStorage value is below minimum (320px)", () => {
    localStorage.setItem(STORAGE_KEY, "200");
    const { container } = render(<SidePanel />);
    const panel = container.firstChild as HTMLElement;
    expect(panel.style.width).toBe("480px");
  });

  it("falls back to 480px when localStorage value is not a number", () => {
    localStorage.setItem(STORAGE_KEY, "notanumber");
    const { container } = render(<SidePanel />);
    const panel = container.firstChild as HTMLElement;
    expect(panel.style.width).toBe("480px");
  });

  it("persists width to localStorage on mouseup after drag", () => {
    localStorage.setItem(STORAGE_KEY, "600");
    render(<SidePanel />);
    // Simulate a drag: mousedown on resize handle, mousemove, mouseup
    fireEvent.mouseDown(document.querySelector(".cursor-col-resize")!, {
      clientX: 100,
    });
    fireEvent.mouseMove(window, { clientX: 50 }); // dragged 50px left → wider
    fireEvent.mouseUp(window);
    // localStorage should have been updated to the new width
    const saved = localStorage.getItem(STORAGE_KEY);
    expect(saved).not.toBeNull();
    expect(parseInt(saved!, 10)).toBeGreaterThanOrEqual(320);
  });
});

// ── Fix #832: close button accessibility ─────────────────────────────────────
describe("SidePanel — close button a11y (issue #832)", () => {
  it("close button has aria-label='Close workspace panel'", () => {
    render(<SidePanel />);
    const closeBtn = screen.getByRole("button", {
      name: "Close workspace panel",
    });
    expect(closeBtn).toBeTruthy();
  });
});
