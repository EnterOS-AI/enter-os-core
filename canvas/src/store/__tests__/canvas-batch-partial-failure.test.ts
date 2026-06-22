import { describe, it, expect, beforeEach, vi } from "vitest";

// Mock fetch BEFORE importing the store — api.ts uses the global.
// Individual tests replace this mock to drive ok/!ok per-URL.
global.fetch = vi.fn();

import { useCanvasStore } from "../canvas";
import type { WorkspaceNodeData } from "../canvas";

function makeWS(
  overrides: Partial<WorkspaceNodeData> & { id: string },
): WorkspaceNodeData {
  // makeWS builds a minimal WorkspaceNodeData for tests that set state
  // directly on the store (bypassing hydrate). The `id` override is
  // ignored — node IDs live on the outer Node<> wrapper, not inside
  // `data`. It's accepted here so callers can keep their existing
  // `makeWS({ id: "ws-foo" })` call sites even though the id is only
  // used on the Node<> wrapper at the call site.
  void overrides.id;
  return {
    name: "WS",
    role: "agent",
    tier: 1,
    status: "online",
    agentCard: null,
    url: "http://localhost:9000",
    parentId: null,
    activeTasks: 0,
    lastErrorRate: 0,
    lastSampleError: "",
    currentTask: "",
    collapsed: false,
    runtime: "",
    needsRestart: false,
    budgetLimit: null,
    ...overrides,
  };
}

/**
 * Partial-failure contract for batchRestart / batchPause / batchDelete.
 *
 * api.post / api.del throw on non-2xx (src/lib/api.ts:32-34). The store uses
 * Promise.allSettled which swallows those rejections. Before the fix:
 *   - batchDelete removed every id unconditionally, producing ghost workspaces.
 *   - batchRestart cleared needsRestart on every id unconditionally.
 *   - All three resolved undefined, so BatchActionBar's catch was dead code.
 *
 * After the fix: successful ids mutate, failed ids stay selected for retry,
 * and the method throws an Error summarising the failure count.
 */

beforeEach(() => {
  useCanvasStore.setState({
    nodes: [],
    edges: [],
    selectedNodeId: null,
    selectedNodeIds: new Set<string>(),
    panelTab: "details",
    dragOverNodeId: null,
    contextMenu: null,
    searchOpen: false,
    viewport: { x: 0, y: 0, zoom: 1 },
  });
  vi.clearAllMocks();
});

// Drives global.fetch so that a URL matching `failSubstring` returns a 500
// and every other call returns ok:true with an empty JSON body.
function installPartialFetch(failSubstring: string) {
  (global.fetch as unknown as ReturnType<typeof vi.fn>).mockImplementation(
    (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url.includes(failSubstring)) {
        return Promise.resolve({
          ok: false,
          status: 500,
          json: () => Promise.resolve({}),
          text: () => Promise.resolve("boom"),
        } as Response);
      }
      return Promise.resolve({
        ok: true,
        status: 200,
        json: () => Promise.resolve({}),
        text: () => Promise.resolve(""),
      } as Response);
    }
  );
}

// ──────────────────────────────────────────────────────────────────────────
// batchDelete
// ──────────────────────────────────────────────────────────────────────────

describe("batchDelete — partial failure", () => {
  it("leaves the failed workspace in `nodes` (no ghost removal)", async () => {
    useCanvasStore.setState({
      nodes: [
        { id: "ws-ok", type: "workspace", position: { x: 0, y: 0 }, data: makeWS({ id: "ws-ok" }) },
        { id: "ws-fail", type: "workspace", position: { x: 0, y: 0 }, data: makeWS({ id: "ws-fail" }) },
      ],
      selectedNodeIds: new Set(["ws-ok", "ws-fail"]),
    });
    installPartialFetch("ws-fail");

    await expect(useCanvasStore.getState().batchDelete()).rejects.toThrow(/1\/2 delete/);

    const ids = useCanvasStore.getState().nodes.map((n) => n.id);
    expect(ids).toContain("ws-fail");
    expect(ids).not.toContain("ws-ok");
  });

  it("keeps the failed id in selectedNodeIds so the user can retry", async () => {
    useCanvasStore.setState({
      nodes: [
        { id: "ws-ok", type: "workspace", position: { x: 0, y: 0 }, data: makeWS({ id: "ws-ok" }) },
        { id: "ws-fail", type: "workspace", position: { x: 0, y: 0 }, data: makeWS({ id: "ws-fail" }) },
      ],
      selectedNodeIds: new Set(["ws-ok", "ws-fail"]),
    });
    installPartialFetch("ws-fail");

    await useCanvasStore.getState().batchDelete().catch(() => {
      /* swallow — we're asserting state */
    });

    const sel = useCanvasStore.getState().selectedNodeIds;
    expect(sel.has("ws-fail")).toBe(true);
    expect(sel.has("ws-ok")).toBe(false);
  });

  it("rejects so the BatchActionBar error-toast path runs", async () => {
    useCanvasStore.setState({
      nodes: [
        { id: "ws-fail", type: "workspace", position: { x: 0, y: 0 }, data: makeWS({ id: "ws-fail" }) },
      ],
      selectedNodeIds: new Set(["ws-fail"]),
    });
    installPartialFetch("ws-fail");

    await expect(useCanvasStore.getState().batchDelete()).rejects.toBeInstanceOf(Error);
  });
});

// ──────────────────────────────────────────────────────────────────────────
// batchRestart
// ──────────────────────────────────────────────────────────────────────────

describe("batchRestart — partial failure", () => {
  it("keeps needsRestart=true on the workspace that failed to restart", async () => {
    useCanvasStore.setState({
      nodes: [
        {
          id: "ws-ok",
          type: "workspace",
          position: { x: 0, y: 0 },
          data: { ...makeWS({ id: "ws-ok" }), needsRestart: true } as WorkspaceNodeData,
        },
        {
          id: "ws-fail",
          type: "workspace",
          position: { x: 0, y: 0 },
          data: { ...makeWS({ id: "ws-fail" }), needsRestart: true } as WorkspaceNodeData,
        },
      ],
      selectedNodeIds: new Set(["ws-ok", "ws-fail"]),
    });
    installPartialFetch("ws-fail");

    await useCanvasStore.getState().batchRestart().catch(() => {
      /* swallow — we're asserting state */
    });

    const byId = Object.fromEntries(
      useCanvasStore.getState().nodes.map((n) => [n.id, n.data as WorkspaceNodeData])
    );
    expect(byId["ws-ok"].needsRestart).toBe(false);
    expect(byId["ws-fail"].needsRestart).toBe(true);
  });

  it("rejects so the BatchActionBar error-toast path runs", async () => {
    useCanvasStore.setState({
      nodes: [
        {
          id: "ws-fail",
          type: "workspace",
          position: { x: 0, y: 0 },
          data: { ...makeWS({ id: "ws-fail" }), needsRestart: true } as WorkspaceNodeData,
        },
      ],
      selectedNodeIds: new Set(["ws-fail"]),
    });
    installPartialFetch("ws-fail");

    await expect(useCanvasStore.getState().batchRestart()).rejects.toBeInstanceOf(Error);
  });
});

// ──────────────────────────────────────────────────────────────────────────
// batchPause
// ──────────────────────────────────────────────────────────────────────────

describe("batchPause — partial failure", () => {
  it("rejects so the BatchActionBar error-toast path runs", async () => {
    useCanvasStore.setState({
      nodes: [
        { id: "ws-fail", type: "workspace", position: { x: 0, y: 0 }, data: makeWS({ id: "ws-fail" }) },
      ],
      selectedNodeIds: new Set(["ws-fail"]),
    });
    installPartialFetch("ws-fail");

    await expect(useCanvasStore.getState().batchPause()).rejects.toBeInstanceOf(Error);
  });
});
