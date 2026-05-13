// @vitest-environment jsdom
/**
 * MobileCanvas — mobile mini-graph with pinch-zoom and tap-to-open.
 *
 * Per WCAG 2.1 AA / mobile interaction:
 *   - Reset button visible only after zoom/pan (zoomed state)
 *   - Spawn FAB always visible with aria-label
 *   - Legend always visible with all 5 status types
 *   - WorkspacePill shows node count
 *   - Node buttons clickable with onOpen(id) callback
 *
 * NOTE: No @testing-library/jest-dom — use DOM APIs.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render } from "@testing-library/react";
import React from "react";

import { MobileCanvas } from "../MobileCanvas";

// ─── Mock dependencies ──────────────────────────────────────────────────────────

vi.mock("@/lib/theme-provider", () => ({
  useTheme: () => ({ theme: "dark", resolvedTheme: "dark", setTheme: vi.fn() }),
}));

const mockNodes = [
  {
    id: "ws-1",
    position: { x: 100, y: 200 },
    data: {
      name: "Alpha Agent",
      status: "online",
      tier: 2,
      parentId: null,
      runtime: "langgraph",
      activeTasks: 0,
      role: "researcher",
    },
  },
  {
    id: "ws-2",
    position: { x: 300, y: 400 },
    data: {
      name: "Beta Agent",
      status: "degraded",
      tier: 3,
      parentId: "ws-1",
      runtime: "claude-code",
      activeTasks: 1,
      role: "developer",
    },
  },
  {
    id: "ws-3",
    position: { x: 0, y: 0 },
    data: {
      name: "Gamma Agent",
      status: "offline",
      tier: 1,
      parentId: null,
      runtime: "hermes",
      activeTasks: 0,
      role: "analyst",
    },
  },
];

vi.mock("@/store/canvas", () => ({
  useCanvasStore: vi.fn((selector) => {
    if (typeof selector === "function") {
      return selector({ nodes: mockNodes });
    }
    return mockNodes;
  }),
  summarizeWorkspaceCapabilities: vi.fn((data: { status?: string; role?: string }) => ({
    runtime: data.status ? "langgraph" : "unknown",
    skillCount: 0,
    currentTask: data.role ?? "",
  })),
}));

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

// ─── Render ────────────────────────────────────────────────────────────────────

describe("MobileCanvas — render", () => {
  it("renders the canvas container", () => {
    render(
      <MobileCanvas dark={true} onOpen={vi.fn()} onSpawn={vi.fn()} />,
    );
    const container = document.querySelector('[style*="position: absolute"]');
    expect(container).toBeTruthy();
  });

  it("renders the legend with all 5 status types", () => {
    render(
      <MobileCanvas dark={true} onOpen={vi.fn()} onSpawn={vi.fn()} />,
    );
    const legend = Array.from(document.querySelectorAll("div")).find(
      (d) => d.textContent?.includes("Legend"),
    );
    expect(legend).toBeTruthy();
    expect(legend?.textContent).toContain("online");
    expect(legend?.textContent).toContain("starting");
    expect(legend?.textContent).toContain("degraded");
    expect(legend?.textContent).toContain("failed");
    expect(legend?.textContent).toContain("paused");
  });

  it("renders spawn FAB with correct aria-label", () => {
    render(
      <MobileCanvas dark={true} onOpen={vi.fn()} onSpawn={vi.fn()} />,
    );
    const fab = document.querySelector('button[aria-label="Spawn new agent"]');
    expect(fab).toBeTruthy();
  });

  it("renders node buttons for each store node", () => {
    render(
      <MobileCanvas dark={true} onOpen={vi.fn()} onSpawn={vi.fn()} />,
    );
    const buttons = document.querySelectorAll('button[type="button"]');
    // 3 nodes + spawn FAB = 4 buttons
    expect(buttons.length).toBeGreaterThanOrEqual(4);
  });

  it("renders node with correct name text", () => {
    render(
      <MobileCanvas dark={true} onOpen={vi.fn()} onSpawn={vi.fn()} />,
    );
    expect(document.body.textContent).toContain("Alpha Agent");
    expect(document.body.textContent).toContain("Beta Agent");
    expect(document.body.textContent).toContain("Gamma Agent");
  });

  it("reset button is hidden when not zoomed", () => {
    render(
      <MobileCanvas dark={true} onOpen={vi.fn()} onSpawn={vi.fn()} />,
    );
    const reset = document.querySelector('button[aria-label="Reset zoom"]');
    expect(reset).toBeNull();
  });

  it("renders FAB and legend regardless of node count", () => {
    render(
      <MobileCanvas dark={true} onOpen={vi.fn()} onSpawn={vi.fn()} />,
    );
    const fab = document.querySelector('button[aria-label="Spawn new agent"]');
    expect(fab).toBeTruthy();
    const legend = Array.from(document.querySelectorAll("div")).find(
      (d) => d.textContent?.includes("Legend"),
    );
    expect(legend).toBeTruthy();
  });
});

// ─── Interaction ──────────────────────────────────────────────────────────────

describe("MobileCanvas — interaction", () => {
  it("onOpen called with correct node id when node button clicked", () => {
    const onOpen = vi.fn();
    render(
      <MobileCanvas dark={true} onOpen={onOpen} onSpawn={vi.fn()} />,
    );
    const nodeButtons = Array.from(document.querySelectorAll('button[type="button"]')).filter(
      (b) => b.textContent?.includes("Alpha Agent"),
    );
    expect(nodeButtons.length).toBeGreaterThanOrEqual(1);
    nodeButtons[0]!.click();
    expect(onOpen).toHaveBeenCalledWith("ws-1");
  });

  it("onSpawn called when spawn FAB is clicked", () => {
    const onSpawn = vi.fn();
    render(
      <MobileCanvas dark={true} onOpen={vi.fn()} onSpawn={onSpawn} />,
    );
    const fab = document.querySelector('button[aria-label="Spawn new agent"]')!;
    fab.click();
    expect(onSpawn).toHaveBeenCalledTimes(1);
  });
});
