// @vitest-environment jsdom
/**
 * Tests the molecule:pan-to-node CustomEvent dispatch from canvas-events.ts.
 * Runs in jsdom because window.dispatchEvent is required.
 */
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { handleCanvasEvent, resetProvisioningSequence } from "../canvas-events";
import type { WSMessage } from "../socket";
import type { WorkspaceNodeData } from "../canvas";
import type { Node, Edge } from "@xyflow/react";

// ── Helpers (copied from canvas-events.test.ts) ──────────────────────────────

function makeNode(
  id: string,
  overrides: Partial<WorkspaceNodeData> = {}
): Node<WorkspaceNodeData> {
  return {
    id,
    type: "workspaceNode",
    position: { x: 0, y: 0 },
    data: {
      name: `Node-${id}`,
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
    },
  };
}

function makeMsg(
  overrides: Partial<WSMessage> & { event: string; workspace_id: string }
): WSMessage {
  return { timestamp: new Date().toISOString(), payload: {}, ...overrides };
}

function makeStore(
  nodes: Node<WorkspaceNodeData>[] = [],
  edges: Edge[] = []
) {
  const state = { nodes, edges, selectedNodeId: null, agentMessages: {} };
  const get = () => state;
  const set = vi.fn((partial: Record<string, unknown>) => { Object.assign(state, partial); });
  return { state, get, set };
}

// ─────────────────────────────────────────────────────────────────────────────

describe("canvas-events – molecule:pan-to-node dispatch", () => {
  beforeEach(() => {
    resetProvisioningSequence();
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("dispatches both molecule:pan-to-node AND molecule:fit-deploying-org for a NEW root-level provision", () => {
    // Two custom events are dispatched on NEW root-level provision:
    //   1. molecule:fit-deploying-org — tells useCanvasViewport to
    //      frame the whole deploying subtree. Fires for root nodes
    //      too (commit 5adc8a74) so the canvas centers the just-
    //      landed root immediately instead of waiting for the
    //      first child to arrive.
    //   2. molecule:pan-to-node — pans/zooms to the single node;
    //      only for standalone creates (no parent), so org-import
    //      children don't chase the spawn animation.
    // A previous version of this test expected only #2 and failed
    // when #1 was added for roots. If only one of these ever fires
    // again, this test should flag the regression.
    const { get, set } = makeStore([]);
    const dispatched: Event[] = [];
    const spy = vi.spyOn(window, "dispatchEvent").mockImplementation((e) => {
      dispatched.push(e);
      return true;
    });

    handleCanvasEvent(
      makeMsg({ event: "WORKSPACE_PROVISIONING", workspace_id: "ws-new", payload: {} }),
      get,
      set
    );

    expect(dispatched).toHaveLength(2);
    const panEvent = dispatched.find((e) => e.type === "molecule:pan-to-node");
    const fitEvent = dispatched.find((e) => e.type === "molecule:fit-deploying-org");
    expect(panEvent, "molecule:pan-to-node should fire for standalone create").toBeDefined();
    expect(fitEvent, "molecule:fit-deploying-org should fire so the viewport frames the root").toBeDefined();
    expect((panEvent as CustomEvent).detail?.nodeId).toBe("ws-new");
    expect((fitEvent as CustomEvent).detail?.rootId).toBe("ws-new");

    spy.mockRestore();
  });

  it("does NOT dispatch molecule:pan-to-node when restarting an existing node", () => {
    const { get, set } = makeStore([makeNode("ws-existing")]);
    const dispatched: Event[] = [];
    const spy = vi.spyOn(window, "dispatchEvent").mockImplementation((e) => {
      dispatched.push(e);
      return true;
    });

    handleCanvasEvent(
      makeMsg({ event: "WORKSPACE_PROVISIONING", workspace_id: "ws-existing", payload: {} }),
      get,
      set
    );

    expect(dispatched).toHaveLength(0);
  });
});
