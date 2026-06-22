import { describe, it, expect } from "vitest";
import { summarizeWorkspaceCapabilities } from "../canvas-capabilities";
import type { WorkspaceNodeData } from "../canvas";

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function makeNodeData(overrides: Partial<WorkspaceNodeData> = {}): WorkspaceNodeData {
  return {
    name: "Test Workspace",
    status: "online",
    tier: 1,
    agentCard: null,
    activeTasks: 0,
    collapsed: false,
    role: "agent",
    lastErrorRate: 0,
    lastSampleError: "",
    url: "http://localhost:9000",
    parentId: null,
    currentTask: "",
    needsRestart: false,
    runtime: "",
    budgetLimit: null,
    ...overrides,
  };
}

// ---------------------------------------------------------------------------
// null / empty agentCard
// ---------------------------------------------------------------------------

describe("summarizeWorkspaceCapabilities – null agentCard", () => {
  it("returns null runtime, empty skills, and skillCount 0", () => {
    const result = summarizeWorkspaceCapabilities(makeNodeData({ agentCard: null }));

    expect(result.runtime).toBeNull();
    expect(result.skills).toEqual([]);
    expect(result.skillCount).toBe(0);
  });

  it("hasActiveTask is false when currentTask is empty", () => {
    const result = summarizeWorkspaceCapabilities(makeNodeData({ agentCard: null, currentTask: "" }));
    expect(result.hasActiveTask).toBe(false);
    expect(result.currentTask).toBe("");
  });

  it("hasActiveTask is false when currentTask is whitespace only", () => {
    const result = summarizeWorkspaceCapabilities(makeNodeData({ agentCard: null, currentTask: "   " }));
    expect(result.hasActiveTask).toBe(false);
    expect(result.currentTask).toBe("");
  });

  it("hasActiveTask is true when currentTask has content", () => {
    const result = summarizeWorkspaceCapabilities(
      makeNodeData({ agentCard: null, currentTask: "Processing request" })
    );
    expect(result.hasActiveTask).toBe(true);
    expect(result.currentTask).toBe("Processing request");
  });
});

describe("summarizeWorkspaceCapabilities – agentCard with no skills key", () => {
  it("returns empty skills when agentCard has no skills property", () => {
    const result = summarizeWorkspaceCapabilities(makeNodeData({ agentCard: { name: "Agent" } }));
    expect(result.skills).toEqual([]);
    expect(result.skillCount).toBe(0);
  });
});

// ---------------------------------------------------------------------------
// runtime extraction
// ---------------------------------------------------------------------------

describe("summarizeWorkspaceCapabilities – runtime", () => {
  it("extracts runtime string from agentCard", () => {
    const result = summarizeWorkspaceCapabilities(
      makeNodeData({ agentCard: { runtime: "claude-code", skills: [] } })
    );
    expect(result.runtime).toBe("claude-code");
  });

  it("returns null when runtime is not a string", () => {
    const result = summarizeWorkspaceCapabilities(
      makeNodeData({ agentCard: { runtime: 42, skills: [] } })
    );
    expect(result.runtime).toBeNull();
  });

  it("returns null when runtime is absent", () => {
    const result = summarizeWorkspaceCapabilities(
      makeNodeData({ agentCard: { skills: [] } })
    );
    expect(result.runtime).toBeNull();
  });

  it("returns null when agentCard is null", () => {
    const result = summarizeWorkspaceCapabilities(makeNodeData({ agentCard: null }));
    expect(result.runtime).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// skills extraction
// ---------------------------------------------------------------------------

describe("summarizeWorkspaceCapabilities – skills", () => {
  it("extracts skill names from agentCard", () => {
    const result = summarizeWorkspaceCapabilities(
      makeNodeData({
        agentCard: {
          skills: [
            { id: "write", name: "Writing" },
            { id: "plan", name: "Planning" },
          ],
        },
      })
    );
    expect(result.skills).toEqual(["Writing", "Planning"]);
    expect(result.skillCount).toBe(2);
  });

  it("falls back to skill id when name is absent", () => {
    const result = summarizeWorkspaceCapabilities(
      makeNodeData({ agentCard: { skills: [{ id: "code" }] } })
    );
    expect(result.skills).toEqual(["code"]);
    expect(result.skillCount).toBe(1);
  });

  it("returns empty skills for an empty skills array", () => {
    const result = summarizeWorkspaceCapabilities(
      makeNodeData({ agentCard: { skills: [] } })
    );
    expect(result.skills).toEqual([]);
    expect(result.skillCount).toBe(0);
  });

  it("filters out skills with no name and no id", () => {
    const result = summarizeWorkspaceCapabilities(
      makeNodeData({
        agentCard: {
          skills: [{ name: "Valid Skill" }, {}, { id: "" }],
        },
      })
    );
    expect(result.skills).toEqual(["Valid Skill"]);
    expect(result.skillCount).toBe(1);
  });
});

// ---------------------------------------------------------------------------
// currentTask trimming
// ---------------------------------------------------------------------------

describe("summarizeWorkspaceCapabilities – currentTask", () => {
  it("trims leading and trailing whitespace from currentTask", () => {
    const result = summarizeWorkspaceCapabilities(
      makeNodeData({ currentTask: "  Analysing logs  " })
    );
    expect(result.currentTask).toBe("Analysing logs");
    expect(result.hasActiveTask).toBe(true);
  });

  it("returns trimmed empty string for whitespace-only task", () => {
    const result = summarizeWorkspaceCapabilities(makeNodeData({ currentTask: "\t\n  " }));
    expect(result.currentTask).toBe("");
    expect(result.hasActiveTask).toBe(false);
  });
});

// ---------------------------------------------------------------------------
// Combined full scenario
// ---------------------------------------------------------------------------

describe("summarizeWorkspaceCapabilities – full scenario", () => {
  it("correctly summarises a fully populated WorkspaceNodeData", () => {
    const result = summarizeWorkspaceCapabilities(
      makeNodeData({
        agentCard: {
          runtime: "claude-code",
          skills: [
            { id: "write", name: "Writing" },
            { id: "plan", name: "Planning" },
            { id: "research", name: "Research" },
          ],
        },
        currentTask: "  Reviewing pull request  ",
      })
    );

    expect(result.runtime).toBe("claude-code");
    expect(result.skills).toEqual(["Writing", "Planning", "Research"]);
    expect(result.skillCount).toBe(3);
    expect(result.currentTask).toBe("Reviewing pull request");
    expect(result.hasActiveTask).toBe(true);
  });

  it("handles agentCard with no runtime and no skills", () => {
    const result = summarizeWorkspaceCapabilities(
      makeNodeData({ agentCard: {}, currentTask: "" })
    );

    expect(result.runtime).toBeNull();
    expect(result.skills).toEqual([]);
    expect(result.skillCount).toBe(0);
    expect(result.currentTask).toBe("");
    expect(result.hasActiveTask).toBe(false);
  });
});
