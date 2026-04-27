import { describe, it, expect, beforeEach, vi } from "vitest";

// Mock fetch globally before importing the store (api.ts uses fetch)
global.fetch = vi.fn(() =>
  Promise.resolve({ ok: true, json: () => Promise.resolve({}) } as Response)
);

import { useCanvasStore, summarizeWorkspaceCapabilities } from "../canvas";
import { __resetTombstonesForTest } from "../deleteTombstones";
import type { WorkspaceData, WSMessage } from "../socket";

// Helper to build a WorkspaceData object with sensible defaults
function makeWS(overrides: Partial<WorkspaceData> & { id: string }): WorkspaceData {
  return {
    name: "WS",
    role: "agent",
    tier: 1,
    status: "online",
    agent_card: null,
    url: "http://localhost:9000",
    parent_id: null,
    active_tasks: 0,
    last_error_rate: 0,
    last_sample_error: "",
    uptime_seconds: 60,
    current_task: "",
    x: 0,
    y: 0,
    collapsed: false,
    runtime: "",
    budget_limit: null,
    ...overrides,
  };
}

function makeMsg(overrides: Partial<WSMessage> & { event: string; workspace_id: string }): WSMessage {
  return {
    timestamp: new Date().toISOString(),
    payload: {},
    ...overrides,
  };
}

beforeEach(() => {
  // Reset to initial state before each test
  useCanvasStore.setState({
    nodes: [],
    edges: [],
    selectedNodeId: null,
    panelTab: "details",
    dragOverNodeId: null,
    contextMenu: null,
    searchOpen: false,
    viewport: { x: 0, y: 0, zoom: 1 },
  });
  // Tombstones leak across tests because the module-level map is
  // process-lifetime by design. Reset between tests so a delete in one
  // test doesn't shadow a hydrate in the next.
  __resetTombstonesForTest();
  vi.clearAllMocks();
});

// ---------- selectNode ----------

describe("selectNode", () => {
  it("sets selectedNodeId", () => {
    useCanvasStore.getState().selectNode("ws-1");
    expect(useCanvasStore.getState().selectedNodeId).toBe("ws-1");
  });

  it("deselects when passed null", () => {
    useCanvasStore.getState().selectNode("ws-1");
    useCanvasStore.getState().selectNode(null);
    expect(useCanvasStore.getState().selectedNodeId).toBeNull();
  });
});

// ---------- hydrate ----------

describe("hydrate", () => {
  it("converts WorkspaceData[] to nodes", () => {
    const workspaces = [
      makeWS({ id: "a", name: "Alpha", x: 10, y: 20 }),
      makeWS({ id: "b", name: "Beta", x: 30, y: 40 }),
    ];

    useCanvasStore.getState().hydrate(workspaces);
    const { nodes, edges } = useCanvasStore.getState();

    expect(nodes).toHaveLength(2);
    expect(nodes[0].id).toBe("a");
    expect(nodes[0].data.name).toBe("Alpha");
    expect(nodes[0].position).toEqual({ x: 10, y: 20 });
    expect(nodes[0].type).toBe("workspaceNode");
    expect(nodes[1].id).toBe("b");
    // No parent-child edges
    expect(edges).toHaveLength(0);
  });

  it("binds children to their parent via React Flow parentId", () => {
    // The old model hid child nodes + embedded them as chips inside the
    // parent card. The new model renders every workspace as a first-class
    // card, using React Flow's native parentId to group them so moving
    // the parent carries the children along.
    const workspaces = [
      makeWS({ id: "parent", name: "Parent" }),
      makeWS({ id: "child", name: "Child", parent_id: "parent" }),
    ];

    useCanvasStore.getState().hydrate(workspaces);
    const { nodes } = useCanvasStore.getState();

    const parent = nodes.find((n) => n.id === "parent")!;
    const child = nodes.find((n) => n.id === "child")!;

    expect(parent.hidden).toBeFalsy();
    expect(child.hidden).toBeFalsy();
    expect(parent.parentId).toBeUndefined();
    expect(child.parentId).toBe("parent");
    expect(child.data.parentId).toBe("parent");
  });

  it("maps all WorkspaceData fields into node data", () => {
    const ws = makeWS({
      id: "x",
      name: "Test",
      role: "lead",
      tier: 2,
      status: "degraded",
      agent_card: { skills: ["code"] },
      url: "http://test:9000",
      active_tasks: 3,
      last_error_rate: 0.75,
      last_sample_error: "timeout",
      collapsed: true,
    });

    useCanvasStore.getState().hydrate([ws]);
    const data = useCanvasStore.getState().nodes[0].data;

    expect(data.name).toBe("Test");
    expect(data.role).toBe("lead");
    expect(data.tier).toBe(2);
    expect(data.status).toBe("degraded");
    expect(data.agentCard).toEqual({ skills: ["code"] });
    expect(data.url).toBe("http://test:9000");
    expect(data.activeTasks).toBe(3);
    expect(data.lastErrorRate).toBe(0.75);
    expect(data.lastSampleError).toBe("timeout");
    expect(data.collapsed).toBe(true);
  });

  it("maps current_task into currentTask", () => {
    const ws = makeWS({ id: "x", current_task: "Processing request" });
    useCanvasStore.getState().hydrate([ws]);
    expect(useCanvasStore.getState().nodes[0].data.currentTask).toBe("Processing request");
  });

  it("defaults currentTask to empty string when missing", () => {
    const ws = makeWS({ id: "x" });
    // current_task is "" from makeWS default
    useCanvasStore.getState().hydrate([ws]);
    expect(useCanvasStore.getState().nodes[0].data.currentTask).toBe("");
  });
});

describe("summarizeWorkspaceCapabilities", () => {
  it("derives runtime, skills, and resume state from node data", () => {
    const summary = summarizeWorkspaceCapabilities({
      name: "Echo",
      status: "online",
      tier: 2,
      agentCard: {
        runtime: "claude-code",
        skills: [{ id: "write", name: "Writing" }, { id: "plan" }],
      },
      activeTasks: 1,
      collapsed: false,
      role: "agent",
      lastErrorRate: 0,
      lastSampleError: "",
      url: "http://localhost:9000",
      parentId: null,
      currentTask: "Reviewing docs",
      needsRestart: false,
      runtime: "claude-code",
      budgetLimit: null,
    });

    expect(summary.runtime).toBe("claude-code");
    expect(summary.skills).toEqual(["Writing", "plan"]);
    expect(summary.skillCount).toBe(2);
    expect(summary.currentTask).toBe("Reviewing docs");
    expect(summary.hasActiveTask).toBe(true);
  });

  it("handles missing agent card and whitespace-only task", () => {
    const summary = summarizeWorkspaceCapabilities({
      name: "Echo",
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
      currentTask: "   ",
      needsRestart: false,
      runtime: "",
      budgetLimit: null,
    });

    expect(summary.runtime).toBeNull();
    expect(summary.skills).toEqual([]);
    expect(summary.skillCount).toBe(0);
    expect(summary.currentTask).toBe("");
    expect(summary.hasActiveTask).toBe(false);
  });
});

// ---------- applyEvent ----------

describe("applyEvent", () => {
  beforeEach(() => {
    useCanvasStore.getState().hydrate([
      makeWS({ id: "ws-1", name: "One", status: "online" }),
      makeWS({ id: "ws-2", name: "Two", status: "online", parent_id: "ws-1" }),
    ]);
  });

  it("WORKSPACE_ONLINE sets status to online", () => {
    // First set it to something else
    useCanvasStore.getState().updateNodeData("ws-1", { status: "offline" });

    useCanvasStore.getState().applyEvent(
      makeMsg({ event: "WORKSPACE_ONLINE", workspace_id: "ws-1" })
    );

    const node = useCanvasStore.getState().nodes.find((n) => n.id === "ws-1")!;
    expect(node.data.status).toBe("online");
  });

  it("WORKSPACE_ONLINE is a no-op for unknown workspace", () => {
    const before = useCanvasStore.getState().nodes.length;
    useCanvasStore.getState().applyEvent(
      makeMsg({ event: "WORKSPACE_ONLINE", workspace_id: "unknown" })
    );
    expect(useCanvasStore.getState().nodes.length).toBe(before);
  });

  it("WORKSPACE_OFFLINE sets status to offline", () => {
    useCanvasStore.getState().applyEvent(
      makeMsg({ event: "WORKSPACE_OFFLINE", workspace_id: "ws-1" })
    );

    const node = useCanvasStore.getState().nodes.find((n) => n.id === "ws-1")!;
    expect(node.data.status).toBe("offline");
  });

  it("WORKSPACE_DEGRADED sets status and error fields", () => {
    useCanvasStore.getState().applyEvent(
      makeMsg({
        event: "WORKSPACE_DEGRADED",
        workspace_id: "ws-1",
        payload: { error_rate: 0.8, sample_error: "connection refused" },
      })
    );

    const node = useCanvasStore.getState().nodes.find((n) => n.id === "ws-1")!;
    expect(node.data.status).toBe("degraded");
    expect(node.data.lastErrorRate).toBe(0.8);
    expect(node.data.lastSampleError).toBe("connection refused");
  });

  it("WORKSPACE_PROVISIONING creates a new node", () => {
    useCanvasStore.getState().applyEvent(
      makeMsg({
        event: "WORKSPACE_PROVISIONING",
        workspace_id: "ws-new",
        payload: { name: "Fresh", tier: 2, runtime: "hermes" },
      })
    );

    const { nodes } = useCanvasStore.getState();
    expect(nodes).toHaveLength(3);

    const newNode = nodes.find((n) => n.id === "ws-new")!;
    expect(newNode).toBeDefined();
    expect(newNode.data.name).toBe("Fresh");
    expect(newNode.data.tier).toBe(2);
    expect(newNode.data.status).toBe("provisioning");
    // Runtime must flow through the provisioning event so the side-panel
    // pill renders the real runtime instead of "unknown" until a refetch.
    expect(newNode.data.runtime).toBe("hermes");
    // Position is offset by existing node count * 40
    expect(newNode.position.x).toBeGreaterThanOrEqual(0);
    expect(newNode.position.y).toBeGreaterThanOrEqual(0);
  });

  it("WORKSPACE_PROVISIONING updates existing node status on restart", () => {
    // ws-1 exists as "online" — a restart should set it to "provisioning"
    useCanvasStore.getState().applyEvent(
      makeMsg({
        event: "WORKSPACE_PROVISIONING",
        workspace_id: "ws-1",
        payload: { name: "PM" },
      })
    );

    const { nodes } = useCanvasStore.getState();
    expect(nodes).toHaveLength(2); // no duplication
    const node = nodes.find((n) => n.id === "ws-1")!;
    expect(node.data.status).toBe("provisioning");
    expect(node.data.needsRestart).toBe(false);
    expect(node.data.currentTask).toBe("");
  });

  it("WORKSPACE_PROVISIONING uses defaults when payload is sparse", () => {
    useCanvasStore.getState().applyEvent(
      makeMsg({
        event: "WORKSPACE_PROVISIONING",
        workspace_id: "ws-default",
        payload: {},
      })
    );

    const node = useCanvasStore.getState().nodes.find((n) => n.id === "ws-default")!;
    expect(node.data.name).toBe("New Workspace");
    expect(node.data.tier).toBe(1);
  });

  it("WORKSPACE_REMOVED removes node and reparents children", () => {
    // ws-2 is a child of ws-1. Removing ws-1 should reparent ws-2 to null (root)
    useCanvasStore.getState().applyEvent(
      makeMsg({ event: "WORKSPACE_REMOVED", workspace_id: "ws-1" })
    );

    const { nodes } = useCanvasStore.getState();
    expect(nodes).toHaveLength(1);
    expect(nodes[0].id).toBe("ws-2");
    expect(nodes[0].data.parentId).toBeNull();
    expect(nodes[0].parentId).toBeUndefined();
  });

  it("WORKSPACE_REMOVED clears selectedNodeId if removed", () => {
    useCanvasStore.getState().selectNode("ws-1");
    useCanvasStore.getState().applyEvent(
      makeMsg({ event: "WORKSPACE_REMOVED", workspace_id: "ws-1" })
    );
    expect(useCanvasStore.getState().selectedNodeId).toBeNull();
  });

  it("WORKSPACE_REMOVED keeps selectedNodeId if different node removed", () => {
    useCanvasStore.getState().selectNode("ws-1");
    useCanvasStore.getState().applyEvent(
      makeMsg({ event: "WORKSPACE_REMOVED", workspace_id: "ws-2" })
    );
    expect(useCanvasStore.getState().selectedNodeId).toBe("ws-1");
  });

  it("AGENT_CARD_UPDATED sets agentCard", () => {
    const card = { name: "Echo Agent", skills: [{ id: "echo" }] };
    useCanvasStore.getState().applyEvent(
      makeMsg({
        event: "AGENT_CARD_UPDATED",
        workspace_id: "ws-1",
        payload: { agent_card: card },
      })
    );

    const node = useCanvasStore.getState().nodes.find((n) => n.id === "ws-1")!;
    expect(node.data.agentCard).toEqual(card);
  });

  it("AGENT_CARD_UPDATED sets null for non-object card", () => {
    useCanvasStore.getState().applyEvent(
      makeMsg({
        event: "AGENT_CARD_UPDATED",
        workspace_id: "ws-1",
        payload: { agent_card: "invalid" },
      })
    );

    const node = useCanvasStore.getState().nodes.find((n) => n.id === "ws-1")!;
    expect(node.data.agentCard).toBeNull();
  });

  it("TASK_UPDATED sets currentTask and activeTasks", () => {
    useCanvasStore.getState().applyEvent(
      makeMsg({
        event: "TASK_UPDATED",
        workspace_id: "ws-1",
        payload: { current_task: "Analyzing data", active_tasks: 2 },
      })
    );

    const node = useCanvasStore.getState().nodes.find((n) => n.id === "ws-1")!;
    expect(node.data.currentTask).toBe("Analyzing data");
    expect(node.data.activeTasks).toBe(2);
  });

  it("TASK_UPDATED clears currentTask when empty", () => {
    // First set a task
    useCanvasStore.getState().updateNodeData("ws-1", { currentTask: "Working" });

    useCanvasStore.getState().applyEvent(
      makeMsg({
        event: "TASK_UPDATED",
        workspace_id: "ws-1",
        payload: { current_task: "", active_tasks: 0 },
      })
    );

    const node = useCanvasStore.getState().nodes.find((n) => n.id === "ws-1")!;
    expect(node.data.currentTask).toBe("");
    expect(node.data.activeTasks).toBe(0);
  });

  it("TASK_UPDATED is a no-op for unknown workspace", () => {
    const nodesBefore = [...useCanvasStore.getState().nodes];
    useCanvasStore.getState().applyEvent(
      makeMsg({
        event: "TASK_UPDATED",
        workspace_id: "unknown",
        payload: { current_task: "task", active_tasks: 1 },
      })
    );
    // Nodes unchanged (same length, same data for ws-1)
    const node = useCanvasStore.getState().nodes.find((n) => n.id === "ws-1")!;
    expect(node.data.currentTask).toBe("");
  });

  it("unknown event is a no-op", () => {
    const nodesBefore = useCanvasStore.getState().nodes;
    useCanvasStore.getState().applyEvent(
      makeMsg({ event: "UNKNOWN_EVENT", workspace_id: "ws-1" })
    );
    expect(useCanvasStore.getState().nodes).toEqual(nodesBefore);
  });
});

// ---------- removeNode ----------

describe("removeNode", () => {
  beforeEach(() => {
    useCanvasStore.getState().hydrate([
      makeWS({ id: "root" }),
      makeWS({ id: "mid", parent_id: "root" }),
      makeWS({ id: "leaf", parent_id: "mid" }),
    ]);
  });

  it("removes the node from the list", () => {
    useCanvasStore.getState().removeNode("leaf");
    const ids = useCanvasStore.getState().nodes.map((n) => n.id);
    expect(ids).toEqual(["root", "mid"]);
  });

  it("reparents children to deleted node's parent", () => {
    // Removing mid: leaf should be reparented to root
    useCanvasStore.getState().removeNode("mid");

    const leaf = useCanvasStore.getState().nodes.find((n) => n.id === "leaf")!;
    expect(leaf.data.parentId).toBe("root");
    expect(leaf.parentId).toBe("root"); // RF binding also re-pointed
  });

  it("reparents children to null when root is deleted", () => {
    useCanvasStore.getState().removeNode("root");

    const mid = useCanvasStore.getState().nodes.find((n) => n.id === "mid")!;
    expect(mid.data.parentId).toBeNull();
    expect(mid.parentId).toBeUndefined();
  });

  it("clears selection if removed node was selected", () => {
    useCanvasStore.getState().selectNode("mid");
    useCanvasStore.getState().removeNode("mid");
    expect(useCanvasStore.getState().selectedNodeId).toBeNull();
  });

  it("preserves selection if a different node is removed", () => {
    useCanvasStore.getState().selectNode("root");
    useCanvasStore.getState().removeNode("leaf");
    expect(useCanvasStore.getState().selectedNodeId).toBe("root");
  });
});

// ---------- removeSubtree ----------

describe("removeSubtree", () => {
  beforeEach(() => {
    useCanvasStore.getState().hydrate([
      makeWS({ id: "root" }),
      makeWS({ id: "mid", parent_id: "root" }),
      makeWS({ id: "leaf", parent_id: "mid" }),
      makeWS({ id: "sibling", parent_id: "root" }),
      makeWS({ id: "unrelated" }), // separate root
    ]);
  });

  it("removes the root and every descendant in one shot", () => {
    useCanvasStore.getState().removeSubtree("root");
    const ids = useCanvasStore
      .getState()
      .nodes.map((n) => n.id)
      .sort();
    expect(ids).toEqual(["unrelated"]);
  });

  it("removes a mid-level node and its descendants but leaves siblings + ancestors", () => {
    useCanvasStore.getState().removeSubtree("mid");
    const ids = useCanvasStore
      .getState()
      .nodes.map((n) => n.id)
      .sort();
    expect(ids).toEqual(["root", "sibling", "unrelated"]);
  });

  it("removing a leaf is a no-op cascade (just drops the leaf)", () => {
    useCanvasStore.getState().removeSubtree("leaf");
    const ids = useCanvasStore
      .getState()
      .nodes.map((n) => n.id)
      .sort();
    expect(ids).toEqual(["mid", "root", "sibling", "unrelated"]);
  });

  it("clears selection when the selected node is anywhere in the removed subtree", () => {
    useCanvasStore.getState().selectNode("leaf");
    useCanvasStore.getState().removeSubtree("root");
    expect(useCanvasStore.getState().selectedNodeId).toBeNull();
  });

  it("preserves selection when the selected node is outside the removed subtree", () => {
    useCanvasStore.getState().selectNode("unrelated");
    useCanvasStore.getState().removeSubtree("root");
    expect(useCanvasStore.getState().selectedNodeId).toBe("unrelated");
  });

  it("drops edges incident to any removed node", () => {
    // The hydrate-built edges connect parent → child. After removing
    // `root`, no edge involving root/mid/leaf/sibling should remain.
    useCanvasStore.getState().removeSubtree("root");
    const remaining = useCanvasStore.getState().edges;
    for (const e of remaining) {
      expect(["root", "mid", "leaf", "sibling"]).not.toContain(e.source);
      expect(["root", "mid", "leaf", "sibling"]).not.toContain(e.target);
    }
  });

  // #2069: lock the tombstone path end-to-end at the store level.
  it("hydrate cannot resurrect ids that removeSubtree just dropped (#2069)", () => {
    useCanvasStore.getState().removeSubtree("root");
    expect(useCanvasStore.getState().nodes.map((n) => n.id).sort())
      .toEqual(["unrelated"]);

    // Simulate the in-flight GET response landing AFTER the delete:
    // the snapshot still contains every original workspace, including
    // the just-removed subtree.
    useCanvasStore.getState().hydrate([
      makeWS({ id: "root" }),
      makeWS({ id: "mid", parent_id: "root" }),
      makeWS({ id: "leaf", parent_id: "mid" }),
      makeWS({ id: "sibling", parent_id: "root" }),
      makeWS({ id: "unrelated" }),
    ]);

    // root/mid/leaf/sibling MUST stay deleted; only `unrelated` survives.
    const ids = useCanvasStore.getState().nodes.map((n) => n.id).sort();
    expect(ids).toEqual(["unrelated"]);
  });
});

// ---------- isDescendant ----------

describe("isDescendant", () => {
  beforeEach(() => {
    useCanvasStore.getState().hydrate([
      makeWS({ id: "a" }),
      makeWS({ id: "b", parent_id: "a" }),
      makeWS({ id: "c", parent_id: "b" }),
      makeWS({ id: "d" }), // unrelated root
    ]);
  });

  it("returns true for direct child", () => {
    expect(useCanvasStore.getState().isDescendant("a", "b")).toBe(true);
  });

  it("returns true for grandchild", () => {
    expect(useCanvasStore.getState().isDescendant("a", "c")).toBe(true);
  });

  it("returns false for ancestor (reverse direction)", () => {
    expect(useCanvasStore.getState().isDescendant("c", "a")).toBe(false);
  });

  it("returns false for unrelated nodes", () => {
    expect(useCanvasStore.getState().isDescendant("a", "d")).toBe(false);
  });

  it("returns false for same node", () => {
    expect(useCanvasStore.getState().isDescendant("a", "a")).toBe(false);
  });

  it("returns false for non-existent nodeId", () => {
    expect(useCanvasStore.getState().isDescendant("a", "nope")).toBe(false);
  });
});

// ---------- updateNodeData ----------

describe("updateNodeData", () => {
  beforeEach(() => {
    useCanvasStore.getState().hydrate([makeWS({ id: "ws-1", name: "Old" })]);
  });

  it("merges partial data into the node", () => {
    useCanvasStore.getState().updateNodeData("ws-1", { name: "New", tier: 3 });
    const data = useCanvasStore.getState().nodes[0].data;
    expect(data.name).toBe("New");
    expect(data.tier).toBe(3);
    // Unaffected fields preserved
    expect(data.status).toBe("online");
  });

  it("is a no-op for unknown id (no crash)", () => {
    useCanvasStore.getState().updateNodeData("nope", { name: "X" });
    expect(useCanvasStore.getState().nodes).toHaveLength(1);
    expect(useCanvasStore.getState().nodes[0].data.name).toBe("Old");
  });
});

// ---------- openContextMenu / closeContextMenu ----------

describe("context menu", () => {
  const menu = {
    x: 100,
    y: 200,
    nodeId: "ws-1",
    nodeData: {
      name: "Test",
      status: "online",
      tier: 1,
      agentCard: null,
      activeTasks: 0,
      collapsed: false,
      role: "",
      lastErrorRate: 0,
      lastSampleError: "",
      url: "",
      parentId: null,
      currentTask: "",
      needsRestart: false,
      runtime: "",
      budgetLimit: null,
    },
  };

  it("openContextMenu sets state", () => {
    useCanvasStore.getState().openContextMenu(menu);
    expect(useCanvasStore.getState().contextMenu).toEqual(menu);
  });

  it("closeContextMenu clears state", () => {
    useCanvasStore.getState().openContextMenu(menu);
    useCanvasStore.getState().closeContextMenu();
    expect(useCanvasStore.getState().contextMenu).toBeNull();
  });
});

// ---------- setPanelTab ----------

describe("setPanelTab", () => {
  it("sets the active panel tab", () => {
    useCanvasStore.getState().setPanelTab("chat");
    expect(useCanvasStore.getState().panelTab).toBe("chat");
  });

  it("can switch between tabs", () => {
    useCanvasStore.getState().setPanelTab("terminal");
    useCanvasStore.getState().setPanelTab("config");
    expect(useCanvasStore.getState().panelTab).toBe("config");
  });

  it("can switch to skills tab", () => {
    useCanvasStore.getState().setPanelTab("skills");
    expect(useCanvasStore.getState().panelTab).toBe("skills");
  });
});

// ---------- getSelectedNode ----------

describe("getSelectedNode", () => {
  beforeEach(() => {
    useCanvasStore.getState().hydrate([makeWS({ id: "ws-1", name: "Alpha" })]);
  });

  it("returns null when nothing selected", () => {
    expect(useCanvasStore.getState().getSelectedNode()).toBeNull();
  });

  it("returns the selected node", () => {
    useCanvasStore.getState().selectNode("ws-1");
    const node = useCanvasStore.getState().getSelectedNode();
    expect(node).not.toBeNull();
    expect(node!.data.name).toBe("Alpha");
  });

  it("returns null when selected id does not match any node", () => {
    useCanvasStore.getState().selectNode("nonexistent");
    expect(useCanvasStore.getState().getSelectedNode()).toBeNull();
  });
});

// ---------- savePosition ----------

describe("savePosition", () => {
  it("calls API to persist position", async () => {
    await useCanvasStore.getState().savePosition("ws-1", 42, 99);
    expect(global.fetch).toHaveBeenCalledWith(
      expect.stringContaining("/workspaces/ws-1"),
      expect.objectContaining({ method: "PATCH" })
    );
  });
});

// ---------- saveViewport ----------

describe("saveViewport", () => {
  it("updates local viewport and calls API", async () => {
    await useCanvasStore.getState().saveViewport(10, 20, 1.5);
    expect(useCanvasStore.getState().viewport).toEqual({ x: 10, y: 20, zoom: 1.5 });
    expect(global.fetch).toHaveBeenCalledWith(
      expect.stringContaining("/canvas/viewport"),
      expect.objectContaining({ method: "PUT" })
    );
  });
});

// ---------- nestNode ----------

describe("nestNode", () => {
  beforeEach(() => {
    useCanvasStore.getState().hydrate([
      makeWS({ id: "a", name: "A" }),
      makeWS({ id: "b", name: "B" }),
    ]);
  });

  it("optimistically updates parentId and the RF parent binding", async () => {
    await useCanvasStore.getState().nestNode("b", "a");

    const b = useCanvasStore.getState().nodes.find((n) => n.id === "b")!;
    expect(b.data.parentId).toBe("a");
    expect(b.parentId).toBe("a");
  });

  it("un-nesting clears parentId and the RF binding", async () => {
    await useCanvasStore.getState().nestNode("b", "a");
    await useCanvasStore.getState().nestNode("b", null);

    const b = useCanvasStore.getState().nodes.find((n) => n.id === "b")!;
    expect(b.data.parentId).toBeNull();
    expect(b.parentId).toBeUndefined();
  });

  it("skips when parentId is already the target", async () => {
    await useCanvasStore.getState().nestNode("b", "a");
    vi.clearAllMocks();
    await useCanvasStore.getState().nestNode("b", "a");
    // No API call since no change
    expect(global.fetch).not.toHaveBeenCalled();
  });

  it("reverts on API failure", async () => {
    (global.fetch as ReturnType<typeof vi.fn>).mockResolvedValueOnce({
      ok: false,
      status: 500,
      text: () => Promise.resolve("internal error"),
    });

    await useCanvasStore.getState().nestNode("b", "a");

    // Should revert to original state (no parent)
    const b = useCanvasStore.getState().nodes.find((n) => n.id === "b")!;
    expect(b.data.parentId).toBeNull();
    expect(b.parentId).toBeUndefined();
  });
});

// ---------- misc state setters ----------

describe("misc state setters", () => {
  it("setDragOverNode", () => {
    useCanvasStore.getState().setDragOverNode("ws-1");
    expect(useCanvasStore.getState().dragOverNodeId).toBe("ws-1");
    useCanvasStore.getState().setDragOverNode(null);
    expect(useCanvasStore.getState().dragOverNodeId).toBeNull();
  });

  it("setSearchOpen", () => {
    useCanvasStore.getState().setSearchOpen(true);
    expect(useCanvasStore.getState().searchOpen).toBe(true);
    useCanvasStore.getState().setSearchOpen(false);
    expect(useCanvasStore.getState().searchOpen).toBe(false);
  });

  it("setViewport", () => {
    useCanvasStore.getState().setViewport({ x: 5, y: 10, zoom: 2 });
    expect(useCanvasStore.getState().viewport).toEqual({ x: 5, y: 10, zoom: 2 });
  });

  it("setPanelTab to activity", () => {
    useCanvasStore.getState().setPanelTab("activity");
    expect(useCanvasStore.getState().panelTab).toBe("activity");
  });
});

// ---------- hydrationError (#554) ----------

describe("hydrationError", () => {
  it("initial value is null", () => {
    expect(useCanvasStore.getState().hydrationError).toBeNull();
  });

  it("setHydrationError stores an error message", () => {
    useCanvasStore.getState().setHydrationError("Network timeout");
    expect(useCanvasStore.getState().hydrationError).toBe("Network timeout");
  });

  it("setHydrationError(null) clears the error", () => {
    useCanvasStore.getState().setHydrationError("Some error");
    useCanvasStore.getState().setHydrationError(null);
    expect(useCanvasStore.getState().hydrationError).toBeNull();
  });

  it("setHydrationError does not affect other state", () => {
    useCanvasStore.getState().hydrate([makeWS({ id: "ws-x", name: "X" })]);
    useCanvasStore.getState().setHydrationError("oops");
    // Nodes should still be intact
    expect(useCanvasStore.getState().nodes).toHaveLength(1);
    expect(useCanvasStore.getState().nodes[0].id).toBe("ws-x");
  });
});

// ---------- ACTIVITY_LOGGED event ----------

describe("ACTIVITY_LOGGED event", () => {
  beforeEach(() => {
    useCanvasStore.getState().hydrate([
      makeWS({ id: "ws-1", name: "Agent" }),
    ]);
  });

  it("does not crash the store (no-op)", () => {
    // ACTIVITY_LOGGED is handled by ActivityTab polling, not the store
    useCanvasStore.getState().applyEvent(
      makeMsg({
        event: "ACTIVITY_LOGGED",
        workspace_id: "ws-1",
        payload: { activity_type: "a2a_receive", method: "message/send" },
      })
    );

    // Store unchanged
    const node = useCanvasStore.getState().nodes.find((n) => n.id === "ws-1")!;
    expect(node.data.status).toBe("online");
    expect(node.data.name).toBe("Agent");
  });
});

// ---------- TASK_UPDATED edge cases ----------

describe("TASK_UPDATED edge cases", () => {
  beforeEach(() => {
    useCanvasStore.getState().hydrate([
      makeWS({ id: "ws-1", name: "Agent", current_task: "Initial task" }),
    ]);
  });

  it("handles missing current_task in payload (defaults to empty)", () => {
    useCanvasStore.getState().applyEvent(
      makeMsg({
        event: "TASK_UPDATED",
        workspace_id: "ws-1",
        payload: { active_tasks: 0 },
      })
    );

    const node = useCanvasStore.getState().nodes.find((n) => n.id === "ws-1")!;
    expect(node.data.currentTask).toBe("");
    expect(node.data.activeTasks).toBe(0);
  });

  it("handles missing active_tasks in payload (defaults to 0)", () => {
    useCanvasStore.getState().applyEvent(
      makeMsg({
        event: "TASK_UPDATED",
        workspace_id: "ws-1",
        payload: { current_task: "New task" },
      })
    );

    const node = useCanvasStore.getState().nodes.find((n) => n.id === "ws-1")!;
    expect(node.data.currentTask).toBe("New task");
    expect(node.data.activeTasks).toBe(0);
  });

  it("preserves other node data when task changes", () => {
    useCanvasStore.getState().applyEvent(
      makeMsg({
        event: "TASK_UPDATED",
        workspace_id: "ws-1",
        payload: { current_task: "New task", active_tasks: 3 },
      })
    );

    const node = useCanvasStore.getState().nodes.find((n) => n.id === "ws-1")!;
    expect(node.data.name).toBe("Agent");
    expect(node.data.status).toBe("online");
    expect(node.data.currentTask).toBe("New task");
  });

  it("does not affect other nodes when task updates", () => {
    useCanvasStore.getState().hydrate([
      makeWS({ id: "ws-1", name: "A", current_task: "Task A" }),
      makeWS({ id: "ws-2", name: "B", current_task: "Task B" }),
    ]);

    useCanvasStore.getState().applyEvent(
      makeMsg({
        event: "TASK_UPDATED",
        workspace_id: "ws-1",
        payload: { current_task: "Updated A", active_tasks: 1 },
      })
    );

    const ws1 = useCanvasStore.getState().nodes.find((n) => n.id === "ws-1")!;
    const ws2 = useCanvasStore.getState().nodes.find((n) => n.id === "ws-2")!;
    expect(ws1.data.currentTask).toBe("Updated A");
    expect(ws2.data.currentTask).toBe("Task B"); // unchanged
  });
});

// ---------- setCollapsed round-trip ----------

describe("setCollapsed", () => {
  beforeEach(() => {
    // Three-level chain so we can test that collapsing an ancestor
    // hides all descendants AND that expanding it correctly preserves
    // any intermediate collapsed state (otherwise setCollapsed and
    // hydrate produce different hidden flags — the drift the review
    // flagged as Critical).
    useCanvasStore.getState().hydrate([
      makeWS({ id: "a", name: "A" }),
      makeWS({ id: "b", name: "B", parent_id: "a" }),
      makeWS({ id: "c", name: "C", parent_id: "b" }),
    ]);
  });

  it("hides the entire subtree when the root is collapsed", () => {
    useCanvasStore.getState().setCollapsed("a", true);
    const { nodes } = useCanvasStore.getState();
    expect(nodes.find((n) => n.id === "a")!.hidden).toBeFalsy();
    expect(nodes.find((n) => n.id === "b")!.hidden).toBe(true);
    expect(nodes.find((n) => n.id === "c")!.hidden).toBe(true);
    expect(nodes.find((n) => n.id === "a")!.data.collapsed).toBe(true);
  });

  it("keeps descendants hidden when an ancestor is un-collapsed but a middle parent is still collapsed", () => {
    // Collapse both A and B, then expand A. C must stay hidden because
    // B — its immediate parent — is still collapsed. Before the fix,
    // setCollapsed naively unhid every descendant of A and drifted from
    // what hydrate would produce.
    useCanvasStore.getState().setCollapsed("a", true);
    useCanvasStore.getState().setCollapsed("b", true);
    useCanvasStore.getState().setCollapsed("a", false);
    const { nodes } = useCanvasStore.getState();
    expect(nodes.find((n) => n.id === "b")!.hidden).toBeFalsy();
    expect(nodes.find((n) => n.id === "c")!.hidden).toBe(true);
  });

  it("matches hydrate's hidden flags (no drift on snapshot refresh)", () => {
    // Run the same scenario through setCollapsed, then re-hydrate from
    // an equivalent server snapshot and assert the hidden flags agree.
    useCanvasStore.getState().setCollapsed("a", true);
    const afterCollapse = useCanvasStore.getState().nodes.map((n) => ({
      id: n.id,
      hidden: !!n.hidden,
    }));

    useCanvasStore.getState().hydrate([
      makeWS({ id: "a", name: "A", collapsed: true }),
      makeWS({ id: "b", name: "B", parent_id: "a" }),
      makeWS({ id: "c", name: "C", parent_id: "b" }),
    ]);
    const afterHydrate = useCanvasStore.getState().nodes.map((n) => ({
      id: n.id,
      hidden: !!n.hidden,
    }));
    expect(afterHydrate).toEqual(afterCollapse);
  });

  it("sizes the expanded parent to fit nested-parent children, not leaf-count", () => {
    // Regression: when a collapsed parent contains a child that is
    // itself a parent (CTO → Dev Lead → 6 engineers), expanding must
    // use each direct child's actual rendered size — not the
    // leaf-count formula. Otherwise the container is too small and
    // Dev Lead (wide enough for 6 engineers in a grid) overflows.
    useCanvasStore.getState().hydrate([
      makeWS({ id: "cto", name: "CTO", collapsed: true }),
      makeWS({ id: "devLead", name: "Dev Lead", parent_id: "cto" }),
      makeWS({ id: "fe", name: "Frontend", parent_id: "devLead" }),
      makeWS({ id: "be", name: "Backend", parent_id: "devLead" }),
      makeWS({ id: "mo", name: "Mobile", parent_id: "devLead" }),
      makeWS({ id: "do", name: "DevOps", parent_id: "devLead" }),
      makeWS({ id: "se", name: "Security", parent_id: "devLead" }),
      makeWS({ id: "qa", name: "QA", parent_id: "devLead" }),
    ]);
    const devLeadNode = useCanvasStore
      .getState()
      .nodes.find((n) => n.id === "devLead")!;
    const devLeadW = devLeadNode.width as number;

    useCanvasStore.getState().setCollapsed("cto", false);

    const ctoAfter = useCanvasStore
      .getState()
      .nodes.find((n) => n.id === "cto")!;
    // CTO's new width must be wide enough to host its Dev Lead child
    // plus the parent's own padding. Leaf-count formula would yield
    // ~272 (one 240px leaf slot); subtree-aware should be ≥ Dev Lead
    // plus side padding.
    expect(ctoAfter.width).toBeGreaterThanOrEqual(devLeadW);
  });
});

// ---------- bumpZOrder ----------

describe("bumpZOrder", () => {
  beforeEach(() => {
    useCanvasStore.getState().hydrate([
      makeWS({ id: "r1", name: "R1" }),
      makeWS({ id: "r2", name: "R2" }),
      makeWS({ id: "r3", name: "R3" }),
    ]);
  });

  it("swaps with the neighbour in the bump direction (no drift on identical zIndex)", () => {
    // Fresh topology: all three siblings start at zIndex=0 (depth=0).
    // Bumping r2 forward must put it above exactly one sibling, not
    // arbitrarily far ahead.
    useCanvasStore.getState().bumpZOrder("r2", 1);
    const nodes = useCanvasStore.getState().nodes;
    const r1Z = nodes.find((n) => n.id === "r1")!.zIndex ?? 0;
    const r2Z = nodes.find((n) => n.id === "r2")!.zIndex ?? 0;
    const r3Z = nodes.find((n) => n.id === "r3")!.zIndex ?? 0;
    // r2 now above at least one neighbour.
    expect(r2Z).toBeGreaterThan(Math.min(r1Z, r3Z));
    // Bumping once more swaps with the remaining one — not unbounded.
    useCanvasStore.getState().bumpZOrder("r2", 1);
    const r2ZAfter = useCanvasStore.getState().nodes.find((n) => n.id === "r2")!.zIndex ?? 0;
    expect(r2ZAfter).toBeLessThanOrEqual(r2Z + 2);
  });

  it("no-ops at the edge of the sibling list", () => {
    const beforeZ = useCanvasStore.getState().nodes.map((n) => n.zIndex ?? 0);
    // First sibling bumped backward has no earlier neighbour.
    useCanvasStore.getState().bumpZOrder("r1", -1);
    const afterZ = useCanvasStore.getState().nodes.map((n) => n.zIndex ?? 0);
    expect(afterZ).toEqual(beforeZ);
  });
});

// ---------- batchNest ----------

describe("batchNest", () => {
  beforeEach(() => {
    (global.fetch as ReturnType<typeof vi.fn>).mockClear();
    // Scenario: two root nodes (a, b) and one nested under a (a-child).
    // Tests below re-parent various subsets into `target`.
    useCanvasStore.getState().hydrate([
      makeWS({ id: "target", name: "Target", x: 1000, y: 0 }),
      makeWS({ id: "a", name: "A", x: 0, y: 0 }),
      makeWS({ id: "b", name: "B", x: 200, y: 0 }),
      makeWS({ id: "a-child", name: "A/Child", parent_id: "a", x: 50, y: 50 }),
    ]);
  });

  it("re-parents every selected root into the target via one PATCH each", async () => {
    const mock = global.fetch as ReturnType<typeof vi.fn>;
    mock.mockImplementation(() =>
      Promise.resolve({ ok: true, json: () => Promise.resolve({}) } as Response),
    );
    // Clear any PATCHes that hydrate's computeAutoLayout may have fired
    // (auto-positioned workspaces trigger a savePosition → PATCH).
    mock.mockClear();
    await useCanvasStore.getState().batchNest(["a", "b"], "target");
    const nodes = useCanvasStore.getState().nodes;
    expect(nodes.find((n) => n.id === "a")!.data.parentId).toBe("target");
    expect(nodes.find((n) => n.id === "b")!.data.parentId).toBe("target");
    // Every PATCH fired by batchNest should target /workspaces/<id>
    // and carry `parent_id: "target"` plus absolute x,y. One per root.
    const nestPatchCalls = mock.mock.calls.filter((c) => {
      const init = c[1] as RequestInit | undefined;
      if (init?.method !== "PATCH") return false;
      const body = init.body ? JSON.parse(init.body as string) : {};
      return body.parent_id === "target";
    });
    expect(nestPatchCalls).toHaveLength(2);
    for (const call of nestPatchCalls) {
      const body = JSON.parse((call[1] as RequestInit).body as string);
      expect(body.x).toBeTypeOf("number");
      expect(body.y).toBeTypeOf("number");
    }
  });

  it("filters out selected descendants so a subtree moves intact", async () => {
    // User selects both A AND its child A/Child, then drags into target.
    // Intent: move the A subtree — A/Child stays under A, not target.
    (global.fetch as ReturnType<typeof vi.fn>).mockImplementation(() =>
      Promise.resolve({ ok: true, json: () => Promise.resolve({}) } as Response),
    );
    await useCanvasStore.getState().batchNest(["a", "a-child"], "target");
    const nodes = useCanvasStore.getState().nodes;
    expect(nodes.find((n) => n.id === "a")!.data.parentId).toBe("target");
    // The descendant is NOT independently re-parented; its parent is still A.
    expect(nodes.find((n) => n.id === "a-child")!.data.parentId).toBe("a");
  });

  it("rolls back only the nodes whose PATCH rejected", async () => {
    // Reject the PATCH for `a`, accept the one for `b`.
    (global.fetch as ReturnType<typeof vi.fn>).mockImplementation((url: string) => {
      if (typeof url === "string" && url.endsWith("/workspaces/a")) {
        return Promise.reject(new Error("network"));
      }
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({}),
      } as Response);
    });
    await useCanvasStore.getState().batchNest(["a", "b"], "target");
    const nodes = useCanvasStore.getState().nodes;
    // `a` rolled back to its original parent (null), `b` stayed committed.
    expect(nodes.find((n) => n.id === "a")!.data.parentId).toBeNull();
    expect(nodes.find((n) => n.id === "b")!.data.parentId).toBe("target");
  });

  it("filters out all selected descendants in a three-level chain", async () => {
    // Re-hydrate to a chain A → B → C. User selects all three.
    // Expected: only A is planned for re-parent; B and C ride with it
    // via React Flow's parent binding.
    useCanvasStore.getState().hydrate([
      makeWS({ id: "target", name: "Target", x: 2000, y: 0 }),
      makeWS({ id: "A", name: "A", x: 0, y: 0 }),
      makeWS({ id: "B", name: "B", parent_id: "A", x: 50, y: 50 }),
      makeWS({ id: "C", name: "C", parent_id: "B", x: 10, y: 10 }),
    ]);
    const mock = global.fetch as ReturnType<typeof vi.fn>;
    mock.mockImplementation(() =>
      Promise.resolve({ ok: true, json: () => Promise.resolve({}) } as Response),
    );
    mock.mockClear();
    await useCanvasStore.getState().batchNest(["A", "B", "C"], "target");
    const nodes = useCanvasStore.getState().nodes;
    expect(nodes.find((n) => n.id === "A")!.data.parentId).toBe("target");
    expect(nodes.find((n) => n.id === "B")!.data.parentId).toBe("A");
    expect(nodes.find((n) => n.id === "C")!.data.parentId).toBe("B");
    // Exactly one nest-PATCH (for A). B and C weren't re-parented.
    const nestPatches = mock.mock.calls.filter((c) => {
      const init = c[1] as RequestInit | undefined;
      if (init?.method !== "PATCH") return false;
      const body = init.body ? JSON.parse(init.body as string) : {};
      return body.parent_id === "target";
    });
    expect(nestPatches).toHaveLength(1);
  });
});
