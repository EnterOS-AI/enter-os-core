// @vitest-environment jsdom
/**
 * mobile/components.tsx — pure functions.
 *
 * Covers:
 *   - toMobileAgent: full transform, all status/tier/runtime cases
 *   - classifyForFilter: online → "online", failed/degraded → "issue",
 *     starting/paused/offline → "paused"
 *
 * NOTE: No @testing-library/jest-dom — use DOM APIs.
 */
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { Node } from "@xyflow/react";
import type { WorkspaceNodeData } from "@/store/canvas";

import {
  AgentCard,
  FilterChips,
  RemoteBadge,
  classifyForFilter,
  toMobileAgent,
  type MobileAgent,
  type AgentFilter,
} from "../components";

// ─── Mock store ────────────────────────────────────────────────────────────────

const mockSummarize = vi.fn();

vi.mock("@/store/canvas", () => ({
  summarizeWorkspaceCapabilities: (...args: unknown[]) => mockSummarize(...args),
}));

// ─── Helpers ─────────────────────────────────────────────────────────────────

function makeNode(overrides: Partial<WorkspaceNodeData> = {}): Node<WorkspaceNodeData> {
  return {
    id: "ws-1",
    position: { x: 0, y: 0 },
    data: {
      name: "Test Agent",
      status: "online",
      tier: 2,
      agentCard: null,
      activeTasks: 0,
      collapsed: false,
      role: "assistant",
      lastErrorRate: 0,
      lastSampleError: "",
      url: "http://localhost:9000",
      parentId: null,
      runtime: "langgraph",
      currentTask: "",
      budgetLimit: null,
      ...overrides,
    } as WorkspaceNodeData,
  };
}

// ─── toMobileAgent ────────────────────────────────────────────────────────────

describe("toMobileAgent — basic fields", () => {
  beforeEach(() => {
    mockSummarize.mockReturnValue({
      runtime: "langgraph",
      skills: [],
      skillCount: 0,
      currentTask: "",
      hasActiveTask: false,
    });
  });

  it("maps id and name", () => {
    const node = makeNode({ name: "My Agent" });
    const agent = toMobileAgent(node);
    expect(agent.id).toBe("ws-1");
    expect(agent.name).toBe("My Agent");
  });

  it("uses id as name when name is empty", () => {
    const node = makeNode({ name: "" });
    const agent = toMobileAgent(node);
    expect(agent.name).toBe("ws-1");
  });

  it("maps tier correctly for tier 1-4", () => {
    const tiers: Array<[number, MobileAgent["tier"]]> = [
      [1, "T1"],
      [2, "T2"],
      [3, "T3"],
      [4, "T4"],
    ];
    for (const [tier, code] of tiers) {
      const agent = toMobileAgent(makeNode({ tier }));
      expect(agent.tier).toBe(code);
    }
  });

  it("maps status to MobileStatus", () => {
    const statuses: Array<[string, MobileAgent["status"]]> = [
      ["online", "online"],
      ["starting", "starting"],
      ["degraded", "degraded"],
      ["failed", "failed"],
      ["paused", "paused"],
      ["offline", "offline"],
    ];
    for (const [status, mobileStatus] of statuses) {
      const agent = toMobileAgent(makeNode({ status }));
      expect(agent.status).toBe(mobileStatus);
    }
  });

  it("marks remote=true for external runtime", () => {
    mockSummarize.mockReturnValue({ runtime: "external", skills: [], skillCount: 0, currentTask: "", hasActiveTask: false });
    const agent = toMobileAgent(makeNode({ runtime: "external" }));
    expect(agent.remote).toBe(true);
  });

  it("marks remote=false for non-external runtime", () => {
    mockSummarize.mockReturnValue({ runtime: "langgraph", skills: [], skillCount: 0, currentTask: "", hasActiveTask: false });
    const agent = toMobileAgent(makeNode({ runtime: "langgraph" }));
    expect(agent.remote).toBe(false);
  });

  it("maps runtime from summarizeWorkspaceCapabilities", () => {
    mockSummarize.mockReturnValue({ runtime: "claude-code", skills: [], skillCount: 0, currentTask: "", hasActiveTask: false });
    const agent = toMobileAgent(makeNode({ runtime: "" }));
    expect(agent.runtime).toBe("claude-code");
  });

  it("maps skills count from summarizeWorkspaceCapabilities", () => {
    mockSummarize.mockReturnValue({ runtime: "langgraph", skills: ["skill1", "skill2"], skillCount: 2, currentTask: "", hasActiveTask: false });
    const agent = toMobileAgent(makeNode());
    expect(agent.skills).toBe(2);
  });

  it("maps activeTasks to calls", () => {
    const agent = toMobileAgent(makeNode({ activeTasks: 5 }));
    expect(agent.calls).toBe(5);
  });

  it("defaults calls to 0 when activeTasks is not a number", () => {
    const node = makeNode() as Node<WorkspaceNodeData>;
    node.data.activeTasks = "not a number" as unknown as number;
    const agent = toMobileAgent(node);
    expect(agent.calls).toBe(0);
  });

  it("maps role as desc fallback to currentTask", () => {
    mockSummarize.mockReturnValue({ runtime: "langgraph", skills: [], skillCount: 0, currentTask: "Doing analysis", hasActiveTask: true });
    const agent = toMobileAgent(makeNode({ role: "" }));
    expect(agent.desc).toBe("Doing analysis");
  });

  it("uses role as desc when currentTask is empty", () => {
    mockSummarize.mockReturnValue({ runtime: "langgraph", skills: [], skillCount: 0, currentTask: "", hasActiveTask: false });
    const agent = toMobileAgent(makeNode({ role: "researcher" }));
    expect(agent.desc).toBe("researcher");
  });

  it("maps parentId from node data", () => {
    const node = makeNode({ parentId: "ws-parent" });
    const agent = toMobileAgent(node);
    expect(agent.parentId).toBe("ws-parent");
  });
});

// ─── classifyForFilter ─────────────────────────────────────────────────────────

describe("classifyForFilter", () => {
  const cases: Array<[MobileAgent["status"], AgentFilter]> = [
    ["online", "online"],
    ["starting", "paused"],
    ["degraded", "issue"],
    ["failed", "issue"],
    ["paused", "paused"],
    ["offline", "paused"],
  ];

  it.each(cases)("normalizeStatus(%s) → %s", (status, expected) => {
    expect(classifyForFilter(status)).toBe(expected);
  });
});
