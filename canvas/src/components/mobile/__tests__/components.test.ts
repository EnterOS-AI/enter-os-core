import { describe, expect, it } from "vitest";
import type { Node } from "@xyflow/react";

import { type WorkspaceNodeData } from "@/store/canvas";

import { classifyForFilter, toMobileAgent } from "../components";

const baseData: WorkspaceNodeData = {
  name: "test-agent",
  status: "online",
  tier: 2,
  agentCard: null,
  activeTasks: 0,
  collapsed: false,
  role: "",
  lastErrorRate: 0,
  lastSampleError: "",
  url: "",
  parentId: null,
  currentTask: "",
  runtime: "claude-code",
  needsRestart: false,
  budgetLimit: null,
};

const makeNode = (overrides: Partial<WorkspaceNodeData> = {}, id = "ws-1"): Node<WorkspaceNodeData> => ({
  id,
  type: "workspaceNode",
  position: { x: 0, y: 0 },
  data: { ...baseData, ...overrides },
});

describe("toMobileAgent", () => {
  it("maps name, status, tier, runtime through the design's 6-key palette", () => {
    const a = toMobileAgent(makeNode({ status: "online", tier: 3, runtime: "hermes" }));
    expect(a.name).toBe("test-agent");
    expect(a.status).toBe("online");
    expect(a.tier).toBe("T3");
    expect(a.runtime).toBe("hermes");
    expect(a.tag).toBe("hermes"); // tag mirrors runtime in v1
  });

  it("flags 'external' runtime as remote (drives the ★ REMOTE badge)", () => {
    expect(toMobileAgent(makeNode({ runtime: "external" })).remote).toBe(true);
    expect(toMobileAgent(makeNode({ runtime: "claude-code" })).remote).toBe(false);
  });

  it("falls back to 'unknown' runtime when both workspace + agentCard are blank", () => {
    const a = toMobileAgent(makeNode({ runtime: "" }));
    expect(a.runtime).toBe("unknown");
    expect(a.tag).toBe("unknown");
  });

  it("uses workspace id as fallback name when name is missing", () => {
    const a = toMobileAgent(makeNode({ name: "" }, "ws-fallback"));
    expect(a.name).toBe("ws-fallback");
  });

  it("preserves the parent link so MobileCanvas can draw parent→child edges", () => {
    const a = toMobileAgent(makeNode({ parentId: "ws-parent" }, "ws-child"));
    expect(a.parentId).toBe("ws-parent");
  });

  it("maps platform 'provisioning' to design 'starting'", () => {
    expect(toMobileAgent(makeNode({ status: "provisioning" })).status).toBe("starting");
  });

  it("counts skills from agentCard.skills array", () => {
    const a = toMobileAgent(
      makeNode({
        agentCard: {
          skills: [{ name: "skill-a" }, { name: "skill-b" }, { name: "skill-c" }],
        },
      }),
    );
    expect(a.skills).toBe(3);
  });

  it("reports 0 skills when agentCard is null", () => {
    expect(toMobileAgent(makeNode({ agentCard: null })).skills).toBe(0);
  });
});

describe("classifyForFilter", () => {
  it("buckets online statuses to the Online filter", () => {
    expect(classifyForFilter("online")).toBe("online");
  });

  it("buckets failure-state statuses to the Issues filter", () => {
    // Issues = anything the user needs to look at NOW.
    expect(classifyForFilter("failed")).toBe("issue");
    expect(classifyForFilter("degraded")).toBe("issue");
  });

  it("buckets non-online non-failure statuses to the Paused filter", () => {
    // Catch-all for transient or intentional offline states.
    expect(classifyForFilter("paused")).toBe("paused");
    expect(classifyForFilter("offline")).toBe("paused");
    expect(classifyForFilter("starting")).toBe("paused");
  });
});
