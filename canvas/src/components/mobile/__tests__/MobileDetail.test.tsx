// @vitest-environment jsdom
/**
 * MobileDetail — agent detail page with tabbed content (Overview/Activity/Config/Memory).
 *
 * Per spec §03: tabbed agent detail page. MobileChat (MR !717) was also tested here.
 *
 * NOTE: No @testing-library/jest-dom — use DOM APIs.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render } from "@testing-library/react";
import React from "react";

import { MobileDetail } from "../MobileDetail";

// ─── Mock store ───────────────────────────────────────────────────────────────

const mockNodeId = "ws-detail-test";
const mockOnBack = vi.fn();
const mockOnChat = vi.fn();

// Module-level mutable state for the mock store.
// Tests mutate this between cases to control what the component sees.
const mockStoreState = {
  nodes: [] as Array<{
    id: string;
    position: { x: number; y: number };
    data: Record<string, unknown>;
    width?: number;
    height?: number;
  }>,
};

vi.mock("@/store/canvas", () => ({
  useCanvasStore: Object.assign(
    vi.fn((sel) => sel(mockStoreState)),
    { getState: () => mockStoreState },
  ),
  summarizeWorkspaceCapabilities: vi.fn((data: Record<string, unknown>) => {
    const agentCard = data.agentCard as Record<string, unknown> | null;
    const skills = Array.isArray(agentCard?.skills)
      ? (agentCard.skills as Array<Record<string, unknown>>).map(
          (s) => String(s.name || s.id || ""),
        ).filter(Boolean)
      : [];
    return {
      runtime: (typeof data.runtime === "string" && data.runtime)
        ? data.runtime
        : (typeof agentCard?.runtime === "string" ? String(agentCard.runtime) : null),
      skills,
      skillCount: skills.length,
      currentTask: String(data.currentTask ?? ""),
      hasActiveTask: String(data.currentTask ?? "").trim().length > 0,
    };
  }),
}));

// Stub the API so DetailActivity doesn't attempt real network calls.
vi.mock("@/lib/api", () => ({ api: { get: vi.fn().mockResolvedValue([]) } }));

// ─── Fixtures ────────────────────────────────────────────────────────────────

const onlineNode = {
  id: mockNodeId,
  position: { x: 100, y: 200 },
  data: {
    name: "Test Agent",
    status: "online",
    tier: 2,
    agentCard: {
      runtime: "claude-code",
      skills: [
        { name: "web-search", id: "skill-1" },
        { name: "code-review", id: "skill-2" },
        { name: "file-ops", id: "skill-3" },
      ],
    },
    currentTask: "Reviewing PR #717",
    activeTasks: 3,
    collapsed: false,
    role: "agent",
    lastErrorRate: 0,
    lastSampleError: "",
    url: "",
    parentId: null,
    runtime: "claude-code",
    needsRestart: false,
  },
  width: 240,
  height: 130,
};

const failedNode = {
  id: "ws-failed",
  position: { x: 0, y: 0 },
  data: {
    name: "Failed Worker",
    status: "failed",
    tier: 4,
    agentCard: null,
    currentTask: "",
    activeTasks: 0,
    collapsed: false,
    role: "agent",
    lastErrorRate: 0.8,
    lastSampleError: "Connection refused",
    url: "",
    parentId: null,
    runtime: "external",
    needsRestart: false,
  },
};

const offlineNode = {
  id: "ws-offline",
  position: { x: 0, y: 0 },
  data: {
    name: "Offline Bot",
    status: "offline",
    tier: 1,
    agentCard: null,
    currentTask: "",
    activeTasks: 0,
    collapsed: false,
    role: "agent",
    lastErrorRate: 0,
    lastSampleError: "",
    url: "",
    parentId: null,
    runtime: "claude-code",
    needsRestart: false,
  },
};

// ─── Helpers ─────────────────────────────────────────────────────────────────

function renderDetail(agentId: string, dark = false) {
  return render(
    <MobileDetail
      agentId={agentId}
      dark={dark}
      onBack={mockOnBack}
      onChat={mockOnChat}
    />,
  );
}

// ─── Setup / teardown ─────────────────────────────────────────────────────────

beforeEach(() => {
  mockOnBack.mockClear();
  mockOnChat.mockClear();
  mockStoreState.nodes = [];
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

// ─── Not found ────────────────────────────────────────────────────────────────

describe("MobileDetail — agent not found", () => {
  it('renders "Agent not found." when no node matches agentId', () => {
    mockStoreState.nodes = [onlineNode];
    const { container } = renderDetail("nonexistent-id");
    expect(container.textContent ?? "").toContain("Agent not found.");
  });

  it("does not render any tab buttons when agent not found", () => {
    mockStoreState.nodes = [];
    const { container } = renderDetail("ghost-agent");
    expect(container.querySelectorAll("button").length).toBe(0);
  });
});

// ─── Hero render ─────────────────────────────────────────────────────────────

describe("MobileDetail — hero section", () => {
  beforeEach(() => {
    mockStoreState.nodes = [onlineNode];
  });

  it("renders the agent name as an h1", () => {
    const { container } = renderDetail(mockNodeId);
    const h1 = container.querySelector("h1");
    expect(h1).toBeTruthy();
    expect(h1!.textContent).toBe("Test Agent");
  });

  it("renders agent tag below the name", () => {
    const { container } = renderDetail(mockNodeId);
    // Tag appears in the hero section, styled differently from the name
    expect(container.textContent ?? "").toContain("claude-code");
  });

  it("renders a Back button with aria-label", () => {
    const { container } = renderDetail(mockNodeId);
    const backBtn = container.querySelector('[aria-label="Back"]');
    expect(backBtn).toBeTruthy();
  });

  it("Back button calls onBack", () => {
    const { container } = renderDetail(mockNodeId);
    const backBtn = container.querySelector('[aria-label="Back"]') as HTMLButtonElement;
    backBtn.click();
    expect(mockOnBack).toHaveBeenCalledTimes(1);
  });

  it("renders a More button", () => {
    const { container } = renderDetail(mockNodeId);
    const moreBtn = container.querySelector('[aria-label="More"]');
    expect(moreBtn).toBeTruthy();
  });

  it("renders Chat CTA with icon text", () => {
    const { container } = renderDetail(mockNodeId);
    expect(container.textContent ?? "").toContain("Open chat");
  });

  it("Chat CTA calls onChat", () => {
    const { container } = renderDetail(mockNodeId);
    const chatBtn = Array.from(container.querySelectorAll("button")).find(
      (b) => b.textContent?.includes("Open chat"),
    );
    expect(chatBtn).toBeTruthy();
    (chatBtn as HTMLButtonElement).click();
    expect(mockOnChat).toHaveBeenCalledTimes(1);
  });
});

// ─── Pill stats ───────────────────────────────────────────────────────────────

describe("MobileDetail — pill stats", () => {
  beforeEach(() => {
    mockStoreState.nodes = [onlineNode];
  });

  it("renders TIER pill with the agent tier", () => {
    const { container } = renderDetail(mockNodeId);
    expect(container.textContent ?? "").toContain("TIER");
  });

  it("renders RUNTIME pill", () => {
    const { container } = renderDetail(mockNodeId);
    expect(container.textContent ?? "").toContain("RUNTIME");
  });

  it("renders SKILLS pill with count", () => {
    const { container } = renderDetail(mockNodeId);
    // 3 skills in the agentCard fixture
    expect(container.textContent ?? "").toContain("SKILLS");
  });

  it("renders STATUS pill", () => {
    const { container } = renderDetail(mockNodeId);
    expect(container.textContent ?? "").toContain("STATUS");
  });

  it("STATUS pill shows agent status value", () => {
    const { container } = renderDetail(mockNodeId);
    // online status from the fixture
    expect(container.textContent ?? "").toContain("online");
  });

  it("renders all 4 pills for online agent", () => {
    const { container } = renderDetail(mockNodeId);
    // Count the pill container divs — each PillStat is a div with specific inline styles
    // We verify by content: TIER, RUNTIME, SKILLS, STATUS should all be present
    const text = container.textContent ?? "";
    expect(text).toContain("TIER");
    expect(text).toContain("RUNTIME");
    expect(text).toContain("SKILLS");
    expect(text).toContain("STATUS");
  });
});

// ─── Tabs ─────────────────────────────────────────────────────────────────────

describe("MobileDetail — tab switching", () => {
  beforeEach(() => {
    mockStoreState.nodes = [onlineNode];
  });

  it("renders all 4 tab buttons", () => {
    const { container } = renderDetail(mockNodeId);
    const text = container.textContent ?? "";
    expect(text).toContain("Overview");
    expect(text).toContain("Activity");
    expect(text).toContain("Config");
    expect(text).toContain("Memory");
  });

  it("defaults to Overview tab", () => {
    const { container } = renderDetail(mockNodeId);
    // DetailOverview renders ID, Tier, Runtime, Active tasks, Skills, Origin rows
    expect(container.textContent ?? "").toContain("ID");
    expect(container.textContent ?? "").toContain("Tier");
  });

  it("Overview tab shows agent ID", () => {
    const { container } = renderDetail(mockNodeId);
    expect(container.textContent ?? "").toContain(mockNodeId);
  });

  it("Overview tab shows active tasks count", () => {
    const { container } = renderDetail(mockNodeId);
    // onlineNode has activeTasks: 3
    expect(container.textContent ?? "").toContain("Active tasks");
    expect(container.textContent ?? "").toContain("3");
  });

  it("Overview tab shows skill count", () => {
    const { container } = renderDetail(mockNodeId);
    // 3 skills in agentCard
    expect(container.textContent ?? "").toContain("Skills");
    expect(container.textContent ?? "").toContain("3 loaded");
  });

  it("Config tab button is findable and is a button element", () => {
    const { container } = renderDetail(mockNodeId);
    const configTab = Array.from(container.querySelectorAll("button")).find(
      (b) => b.textContent?.trim() === "Config",
    );
    expect(configTab).toBeTruthy();
    expect((configTab as HTMLButtonElement).type).toBe("button");
  });

  it("Memory tab button is findable and is a button element", () => {
    const { container } = renderDetail(mockNodeId);
    const memoryTab = Array.from(container.querySelectorAll("button")).find(
      (b) => b.textContent?.trim() === "Memory",
    );
    expect(memoryTab).toBeTruthy();
    expect((memoryTab as HTMLButtonElement).type).toBe("button");
  });
});

// ─── Status rendering ─────────────────────────────────────────────────────────

describe("MobileDetail — status rendering", () => {
  it("renders failed status for failed agent", () => {
    mockStoreState.nodes = [failedNode];
    const { container } = renderDetail("ws-failed");
    expect(container.textContent ?? "").toContain("Failed Worker");
    expect(container.textContent ?? "").toContain("failed");
  });

  it("renders offline status for offline agent", () => {
    mockStoreState.nodes = [offlineNode];
    const { container } = renderDetail("ws-offline");
    expect(container.textContent ?? "").toContain("Offline Bot");
    expect(container.textContent ?? "").toContain("offline");
  });
});

// ─── Dark mode ───────────────────────────────────────────────────────────────

describe("MobileDetail — dark mode", () => {
  beforeEach(() => {
    mockStoreState.nodes = [onlineNode];
  });

  it("renders without crashing in dark mode", () => {
    const { container } = renderDetail(mockNodeId, true);
    expect(container.querySelector("h1")?.textContent).toBe("Test Agent");
  });
});
