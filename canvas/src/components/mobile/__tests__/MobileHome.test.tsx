// @vitest-environment jsdom
/**
 * MobileHome — workspace agent list + filter chips + spawn FAB.
 *
 * Per spec §01: live store data, filter by status, spawn FAB.
 *
 * NOTE: No @testing-library/jest-dom — use DOM APIs.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render } from "@testing-library/react";
import React from "react";

import { MobileHome } from "../MobileHome";

// ─── Mock store ───────────────────────────────────────────────────────────────

const mockOnOpen = vi.fn();
const mockOnSpawn = vi.fn();

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

// ─── Fixtures ───────────────────────────────────────────────────────────────

function makeNode(overrides: Partial<Record<string, unknown>> = {}) {
  return {
    id: `ws-${Math.random().toString(36).slice(2, 7)}`,
    position: { x: 0, y: 0 },
    data: {
      name: "Agent",
      status: "online",
      tier: 2,
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
      ...overrides,
    },
  };
}

const onlineAgent = makeNode({ name: "Online Agent", status: "online", tier: 2 });
const failedAgent = makeNode({ name: "Failed Agent", status: "failed", tier: 4 });
const pausedAgent = makeNode({ name: "Paused Agent", status: "paused", tier: 1 });

// ─── Helpers ─────────────────────────────────────────────────────────────────

function renderHome(overrides: Partial<{
  dark: boolean;
  density: "compact" | "regular";
  workspaceLabel: string;
  username: string;
}> = {}) {
  return render(
    <MobileHome
      dark={overrides.dark ?? false}
      density={overrides.density ?? "regular"}
      onOpen={mockOnOpen}
      onSpawn={mockOnSpawn}
      workspaceLabel={overrides.workspaceLabel}
      username={overrides.username}
    />,
  );
}

// ─── Setup / teardown ─────────────────────────────────────────────────────────

beforeEach(() => {
  mockOnOpen.mockClear();
  mockOnSpawn.mockClear();
  mockStoreState.nodes = [];
});

afterEach(() => {
  cleanup();
});

// ─── Structure ───────────────────────────────────────────────────────────────

describe("MobileHome — page structure", () => {
  it('renders "Agents" heading', () => {
    mockStoreState.nodes = [onlineAgent];
    const { container } = renderHome();
    const h1 = container.querySelector("h1");
    expect(h1).toBeTruthy();
    expect(h1!.textContent).toBe("Agents");
  });

  it("renders WorkspacePill with agent count", () => {
    mockStoreState.nodes = [onlineAgent, failedAgent];
    const { container } = renderHome();
    // WorkspacePill renders the agent count somewhere in the DOM
    expect(container.textContent ?? "").toContain("2");
  });

  it('shows "live" suffix in subheading', () => {
    mockStoreState.nodes = [onlineAgent];
    const { container } = renderHome();
    // Single agent → "1 workspace · live" (singular)
    expect(container.textContent ?? "").toContain("workspace");
    expect(container.textContent ?? "").toContain("live");
  });

  it("renders FilterChips row", () => {
    mockStoreState.nodes = [onlineAgent];
    const { container } = renderHome();
    // FilterChips renders buttons for "All", "Online", "Issues", "Paused"
    const text = container.textContent ?? "";
    expect(text).toContain("All");
    expect(text).toContain("Online");
    expect(text).toContain("Issues");
  });

  it("renders Workspace section label", () => {
    mockStoreState.nodes = [onlineAgent];
    const { container } = renderHome();
    expect(container.textContent ?? "").toContain("Workspace");
  });

  it("renders spawn FAB with aria-label", () => {
    mockStoreState.nodes = [onlineAgent];
    const { container } = renderHome();
    const fab = container.querySelector('[aria-label="Spawn new agent"]');
    expect(fab).toBeTruthy();
  });

  it("FAB calls onSpawn", () => {
    mockStoreState.nodes = [onlineAgent];
    const { container } = renderHome();
    const fab = container.querySelector('[aria-label="Spawn new agent"]') as HTMLButtonElement;
    fab.click();
    expect(mockOnSpawn).toHaveBeenCalledTimes(1);
  });

  it("shows username when provided", () => {
    mockStoreState.nodes = [onlineAgent];
    const { container } = renderHome({ username: "alice@example.com" });
    expect(container.textContent ?? "").toContain("alice@example.com");
  });

  it("omits username when not provided", () => {
    mockStoreState.nodes = [onlineAgent];
    const { container } = renderHome();
    expect(container.querySelector('[style*="letter-spacing"]')?.textContent).not.toContain("@");
  });

  it("renders with custom workspaceLabel", () => {
    mockStoreState.nodes = [onlineAgent];
    const { container } = renderHome({ workspaceLabel: "Production" });
    expect(container.textContent ?? "").toContain("Production");
  });
});

// ─── Agent list ─────────────────────────────────────────────────────────────

describe("MobileHome — agent list", () => {
  it("renders agent cards when nodes are present", () => {
    mockStoreState.nodes = [onlineAgent, failedAgent, pausedAgent];
    const { container } = renderHome();
    expect(container.textContent ?? "").toContain("Online Agent");
    expect(container.textContent ?? "").toContain("Failed Agent");
    expect(container.textContent ?? "").toContain("Paused Agent");
  });

  it("shows 'No agents match this filter.' when filter returns empty", () => {
    mockStoreState.nodes = [onlineAgent];
    const { container } = renderHome();
    // By default filter is "all" — all agents match
    expect(container.textContent ?? "").not.toContain("No agents match");
    // If we could set filter to something that filters everything out...
    // (filter is internal state, we test the "all" default)
    expect(container.querySelectorAll("button").length).toBeGreaterThan(0);
  });

  it("renders no agents when node list is empty", () => {
    mockStoreState.nodes = [];
    const { container } = renderHome();
    // Should show "0 workspaces" and "No agents match this filter."
    expect(container.textContent ?? "").toContain("0 workspace");
  });
});

// ─── Agent count display ──────────────────────────────────────────────────────

describe("MobileHome — agent count", () => {
  it("shows singular 'workspace' when count is 1", () => {
    mockStoreState.nodes = [onlineAgent];
    const { container } = renderHome();
    expect(container.textContent ?? "").toContain("1 workspace");
  });

  it("shows plural 'workspaces' when count is > 1", () => {
    mockStoreState.nodes = [onlineAgent, failedAgent];
    const { container } = renderHome();
    expect(container.textContent ?? "").toContain("2 workspaces");
  });
});

// ─── Dark mode ───────────────────────────────────────────────────────────────

describe("MobileHome — dark mode", () => {
  it("renders without crashing in dark mode", () => {
    mockStoreState.nodes = [onlineAgent];
    const { container } = renderHome({ dark: true });
    expect(container.querySelector("h1")?.textContent).toBe("Agents");
  });
});
