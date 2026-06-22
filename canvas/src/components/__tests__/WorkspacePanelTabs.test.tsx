// @vitest-environment jsdom
import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, fireEvent, cleanup } from "@testing-library/react";

afterEach(() => {
  cleanup();
});

// ── Mock every tab content component to a sentinel so we can assert which
//    body renders without dragging in API calls / heavy children. ───────────
vi.mock("../tabs/DetailsTab", () => ({ DetailsTab: () => <div data-testid="body-details" /> }));
vi.mock("../tabs/SkillsTab", () => ({ SkillsTab: () => <div data-testid="body-skills" /> }));
vi.mock("../tabs/ChatTab", () => ({ ChatTab: () => <div data-testid="body-chat" /> }));
vi.mock("../tabs/ConfigTab", () => ({ ConfigTab: () => <div data-testid="body-config" /> }));
vi.mock("../tabs/ContainerConfigTab", () => ({ ContainerConfigTab: () => <div data-testid="body-container" /> }));
vi.mock("../tabs/DisplayTab", () => ({ DisplayTab: () => <div data-testid="body-display" /> }));
vi.mock("../tabs/TerminalTab", () => ({ TerminalTab: () => <div data-testid="body-terminal" /> }));
vi.mock("../tabs/FilesTab", () => ({ FilesTab: () => <div data-testid="body-files" /> }));
vi.mock("../MemoryInspectorPanel", () => ({ MemoryInspectorPanel: () => <div data-testid="body-memory" /> }));
vi.mock("../tabs/TracesTab", () => ({ TracesTab: () => <div data-testid="body-traces" /> }));
vi.mock("../tabs/EventsTab", () => ({ EventsTab: () => <div data-testid="body-events" /> }));
vi.mock("../tabs/ActivityTab", () => ({ ActivityTab: () => <div data-testid="body-activity" /> }));
vi.mock("../tabs/ScheduleTab", () => ({ ScheduleTab: () => <div data-testid="body-schedule" /> }));
vi.mock("../tabs/ChannelsTab", () => ({ ChannelsTab: () => <div data-testid="body-channels" /> }));
vi.mock("../AuditTrailPanel", () => ({ AuditTrailPanel: () => <div data-testid="body-audit" /> }));

vi.mock("../Tooltip", () => ({
  Tooltip: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));
vi.mock("@/components/Toaster", () => ({ showToast: vi.fn() }));

// The store is only consulted for restartWorkspace.
const mockRestart = vi.fn(() => Promise.resolve());
vi.mock("@/store/canvas", () => ({
  useCanvasStore: vi.fn((selector: (s: { restartWorkspace: typeof mockRestart }) => unknown) =>
    selector({ restartWorkspace: mockRestart })
  ),
}));

import { WorkspacePanelTabs, WORKSPACE_PANEL_TABS } from "../WorkspacePanelTabs";

// eslint-disable-next-line @typescript-eslint/no-explicit-any
const node: any = {
  id: "platform-1",
  data: {
    name: "Org Concierge",
    status: "online",
    tier: 0,
    role: "platform",
    parentId: null,
    needsRestart: false,
    currentTask: null,
    agentCard: null,
  },
};

describe("WorkspacePanelTabs — uncontrolled (Settings usage)", () => {
  it("renders the canonical 15-tab tablist for an explicit node", () => {
    render(<WorkspacePanelTabs node={node} />);
    const tablist = screen.getByRole("tablist");
    expect(tablist.getAttribute("aria-label")).toBe("Workspace panel tabs");
    expect(screen.getAllByRole("tab").length).toBe(WORKSPACE_PANEL_TABS.length);
    expect(WORKSPACE_PANEL_TABS.length).toBe(15);
  });

  it("defaults to the chat tab when no defaultTab is given", () => {
    render(<WorkspacePanelTabs node={node} />);
    expect(screen.getByTestId("body-chat")).toBeTruthy();
    expect(document.getElementById("tab-chat")?.getAttribute("aria-selected")).toBe("true");
  });

  it("honours defaultTab='config' (the concierge Settings entry point)", () => {
    render(<WorkspacePanelTabs node={node} defaultTab="config" />);
    expect(screen.getByTestId("body-config")).toBeTruthy();
    expect(document.getElementById("tab-config")?.getAttribute("aria-selected")).toBe("true");
  });

  it("clicking a tab swaps the body using local state (no store panelTab)", () => {
    render(<WorkspacePanelTabs node={node} />);
    fireEvent.click(document.getElementById("tab-channels")!);
    expect(screen.getByTestId("body-channels")).toBeTruthy();
    expect(document.getElementById("tab-channels")?.getAttribute("aria-selected")).toBe("true");
  });
});

describe("WorkspacePanelTabs — controlled (SidePanel usage)", () => {
  it("renders activeTab and calls onTabChange instead of local state", () => {
    const onTabChange = vi.fn();
    render(<WorkspacePanelTabs node={node} activeTab="details" onTabChange={onTabChange} />);
    expect(screen.getByTestId("body-details")).toBeTruthy();
    fireEvent.click(document.getElementById("tab-config")!);
    expect(onTabChange).toHaveBeenCalledWith("config");
    // Controlled: body does NOT change on its own (parent owns the state).
    expect(screen.getByTestId("body-details")).toBeTruthy();
  });

  it("ArrowRight from chat calls onTabChange with the next tab", () => {
    const onTabChange = vi.fn();
    render(<WorkspacePanelTabs node={node} activeTab="chat" onTabChange={onTabChange} />);
    fireEvent.keyDown(screen.getByRole("tablist"), { key: "ArrowRight" });
    expect(onTabChange).toHaveBeenCalledWith("activity");
  });
});
