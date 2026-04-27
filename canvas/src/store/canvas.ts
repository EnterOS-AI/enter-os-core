import { create } from "zustand";
import {
  type Node,
  type Edge,
  applyNodeChanges,
  type NodeChange,
} from "@xyflow/react";
import { api } from "@/lib/api";
import { showToast } from "@/components/Toaster";
import type { WorkspaceData, WSMessage } from "./socket";
import { handleCanvasEvent } from "./canvas-events";
import { markDeleted, wasRecentlyDeleted } from "./deleteTombstones";
import {
  buildNodesAndEdges,
  computeAutoLayout,
  defaultChildSlot,
  parentMinSizeFromChildren,
  sortParentsBeforeChildren,
  CHILD_DEFAULT_HEIGHT,
  CHILD_DEFAULT_WIDTH,
  PARENT_BOTTOM_PADDING,
  PARENT_SIDE_PADDING,
} from "./canvas-topology";

/**
 * Walk every parent node and bump its width/height (if explicitly set)
 * so the union of its children's relative bboxes plus padding fits. A
 * parent's size never shrinks via this path — only grows — because
 * shrinking on resize would fight the user's own NodeResizer drag.
 */
function growParentsToFitChildren<T extends Record<string, unknown>>(
  nodes: Node<T>[],
): Node<T>[] {
  // Index children by parentId so the scan is O(n).
  const childrenByParent = new Map<string, Node<T>[]>();
  for (const n of nodes) {
    if (!n.parentId) continue;
    const arr = childrenByParent.get(n.parentId) ?? [];
    arr.push(n);
    childrenByParent.set(n.parentId, arr);
  }
  let changed = false;
  const out = nodes.map((n) => {
    const kids = childrenByParent.get(n.id);
    if (!kids || kids.length === 0) return n;
    // Collapsed parents intentionally render compact — skip the grow
    // pass so their size isn't pushed back out by their hidden kids.
    const nData = n.data as unknown as WorkspaceNodeData | undefined;
    if (nData?.collapsed) return n;
    let maxRight = 0;
    let maxBottom = 0;
    for (const k of kids) {
      const w = (k.measured?.width ?? k.width ?? CHILD_DEFAULT_WIDTH) as number;
      const h = (k.measured?.height ?? k.height ?? CHILD_DEFAULT_HEIGHT) as number;
      maxRight = Math.max(maxRight, k.position.x + w);
      maxBottom = Math.max(maxBottom, k.position.y + h);
    }
    const requiredW = maxRight + PARENT_SIDE_PADDING;
    const requiredH = maxBottom + PARENT_BOTTOM_PADDING;
    const currentW = (n.measured?.width ?? n.width ?? 0) as number;
    const currentH = (n.measured?.height ?? n.height ?? 0) as number;
    if (requiredW <= currentW && requiredH <= currentH) return n;
    changed = true;
    return {
      ...n,
      width: Math.max(currentW, requiredW),
      height: Math.max(currentH, requiredH),
    };
  });
  return changed ? out : nodes;
}

// Re-export extracted types and functions so existing imports from "@/store/canvas" keep working
export { summarizeWorkspaceCapabilities } from "./canvas-capabilities";
export type { WorkspaceCapabilitySummary } from "./canvas-capabilities";

export interface WorkspaceNodeData extends Record<string, unknown> {
  name: string;
  status: string;
  tier: number;
  agentCard: Record<string, unknown> | null;
  activeTasks: number;
  collapsed: boolean;
  role: string;
  lastErrorRate: number;
  lastSampleError: string;
  url: string;
  parentId: string | null;
  currentTask: string;
  runtime: string;
  needsRestart: boolean;
  /** USD spend ceiling set by the user; null = unlimited. Added by issue #541. */
  budgetLimit: number | null;
  /** Cumulative USD spend. Present when the platform tracks spend (issue #541). */
  budgetUsed?: number | null;
  /** Per-workspace provisioning-timeout override in milliseconds (#2054).
   *  Sourced server-side from the workspace's template manifest at provision
   *  time. null/absent = fall through to runtime profile + default in
   *  @/lib/runtimeProfiles. Lets a slow runtime declare its cold-boot
   *  expectation without a canvas release. */
  provisionTimeoutMs?: number | null;
}

export type PanelTab = "details" | "skills" | "chat" | "terminal" | "config" | "schedule" | "channels" | "files" | "memory" | "traces" | "events" | "activity" | "audit";

export interface ContextMenuState {
  x: number;
  y: number;
  nodeId: string;
  nodeData: WorkspaceNodeData;
}

interface CanvasState {
  nodes: Node<WorkspaceNodeData>[];
  edges: Edge[];
  selectedNodeId: string | null;
  panelTab: PanelTab;
  dragOverNodeId: string | null;
  contextMenu: ContextMenuState | null;
  // Live width of the SidePanel in pixels. Only meaningful when
  // selectedNodeId is non-null (panel visible). The Toolbar reads this
  // to stay centred on the remaining canvas area instead of the full
  // viewport, so the "Audit" / "Search" / "Settings" buttons don't get
  // hidden behind the panel when a workspace is selected.
  sidePanelWidth: number;
  setSidePanelWidth: (w: number) => void;
  // Whether the TemplatePalette left-drawer is open. Consumed by the
  // Legend so it can shift right and avoid being hidden under the
  // palette. Set by TemplatePalette's toggle button.
  templatePaletteOpen: boolean;
  setTemplatePaletteOpen: (open: boolean) => void;
  hydrate: (workspaces: WorkspaceData[]) => void;
  applyEvent: (msg: WSMessage) => void;
  onNodesChange: (changes: NodeChange<Node<WorkspaceNodeData>>[]) => void;
  savePosition: (nodeId: string, x: number, y: number) => void;
  selectNode: (id: string | null) => void;
  setPanelTab: (tab: PanelTab) => void;
  getSelectedNode: () => Node<WorkspaceNodeData> | null;
  updateNodeData: (id: string, data: Partial<WorkspaceNodeData>) => void;
  restartWorkspace: (id: string) => Promise<void>;
  removeNode: (id: string) => void;
  /** Remove a node AND every descendant in one atomic update. Mirrors
   *  the server-side cascade — `DELETE /workspaces/:id?confirm=true`
   *  drops the row plus every descendant in one transaction. The
   *  caller (Canvas / DetailsTab delete handlers) used to call
   *  `removeNode(rootId)` and rely on per-descendant WORKSPACE_REMOVED
   *  WS events to clear the rest. When the WS is unhealthy those
   *  events never arrive and the children orphan to the root until a
   *  manual page refresh — `removeSubtree` makes the cascade
   *  WS-independent. */
  removeSubtree: (rootId: string) => void;
  setDragOverNode: (id: string | null) => void;
  nestNode: (draggedId: string, targetId: string | null) => Promise<void>;
  isDescendant: (ancestorId: string, nodeId: string) => boolean;
  /** Re-order siblings in z-index space. `direction = +1` sends the node
   *  one step forward among its parent's children (or among canvas
   *  roots); -1 sends it one step back. Figma Cmd+]/[ parity. */
  bumpZOrder: (nodeId: string, direction: 1 | -1) => void;
  /** Re-parent many nodes at once, preserving each node's absolute
   *  position. Lucidchart pattern: drag a selection into a frame and
   *  the inter-node layout stays intact. Used when the primary dragged
   *  node of a multi-select drag triggers a nest confirmation. */
  batchNest: (nodeIds: string[], targetId: string | null) => Promise<void>;
  /** Run the parent auto-grow pass once. Canvas.onNodeDragStop calls
   *  this so a drag that pushed a child past the parent edge commits
   *  the parent grow on release (commit-on-release pattern). */
  growParentsToFitChildren: () => void;
  /** Re-layout a parent's children to the default 2-column grid. Used
   *  by the "Arrange children" context-menu command so users can rescue
   *  out-of-bounds children on demand — topology no longer does it
   *  automatically (P3.12 opt-in rescue). */
  arrangeChildren: (parentId: string) => void;
  /** Toggle the collapsed flag on a parent and hide/show every
   *  descendant so the card renders as a compact header-only frame.
   *  Miro "frame outline view" analog. */
  setCollapsed: (parentId: string, collapsed: boolean) => void;
  openContextMenu: (menu: ContextMenuState) => void;
  closeContextMenu: () => void;
  // Pending delete confirmation — lives in the store (not inside ContextMenu's
  // local state) so the confirm dialog survives ContextMenu unmounting. The
  // ContextMenu's portal-rendered dialog used to race with its outside-click
  // handler: clicking Confirm registered as "outside", closed the menu, and
  // unmounted the dialog before its onClick fired. Hoisting the state fixes
  // that — see fix/context-menu-delete-race.
  pendingDelete:
    | { id: string; name: string; hasChildren: boolean; children: { id: string; name: string }[] }
    | null;
  setPendingDelete: (
    v: { id: string; name: string; hasChildren: boolean; children: { id: string; name: string }[] } | null
  ) => void;
  /** Node IDs whose DELETE request is in flight. Populated the moment
   *  the user confirms a cascade delete; drained as WORKSPACE_REMOVED
   *  events strip the nodes (or all-at-once on request failure). Lets
   *  the canvas render the "don't touch — something is happening"
   *  treatment (dim + non-draggable) during the network round trip
   *  and the server-side cascade, matching the deploy-lock UX. */
  deletingIds: Set<string>;
  beginDelete: (ids: Iterable<string>) => void;
  endDelete: (ids: Iterable<string>) => void;
  searchOpen: boolean;
  setSearchOpen: (open: boolean) => void;
  viewport: { x: number; y: number; zoom: number };
  setViewport: (v: { x: number; y: number; zoom: number }) => void;
  saveViewport: (x: number, y: number, zoom: number) => void;
  // ── Batch selection (Phase 20.3) ─────────────────────────────────────────
  selectedNodeIds: Set<string>;
  toggleNodeSelection: (id: string) => void;
  clearSelection: () => void;
  batchRestart: () => Promise<void>;
  batchPause: () => Promise<void>;
  batchDelete: () => Promise<void>;
  /** Agent-pushed messages keyed by workspace ID. ChatTab consumes and clears these. */
  agentMessages: Record<string, Array<{ id: string; content: string; timestamp: string; attachments?: Array<{ name: string; uri: string; mimeType?: string; size?: number }> }>>;
  consumeAgentMessages: (workspaceId: string) => Array<{ id: string; content: string; timestamp: string; attachments?: Array<{ name: string; uri: string; mimeType?: string; size?: number }> }>;
  /** WebSocket connection status — drives the live indicator in the Toolbar. */
  wsStatus: "connected" | "connecting" | "disconnected";
  setWsStatus: (status: "connected" | "connecting" | "disconnected") => void;
  /** Hydration error message — set when initial canvas load fails. Null when no error. */
  hydrationError: string | null;
  setHydrationError: (error: string | null) => void;
  // ── A2A topology overlay (issue #744) ─────────────────────────────────────
  /** Directed delegation edges shown as an overlay on the canvas (separate from topology edges). */
  a2aEdges: Edge[];
  setA2AEdges: (edges: Edge[]) => void;
  /** Whether the A2A topology overlay is visible. Persisted to localStorage. Default: true. */
  showA2AEdges: boolean;
  setShowA2AEdges: (show: boolean) => void;
}

export const useCanvasStore = create<CanvasState>((set, get) => ({
  nodes: [],
  edges: [],
  selectedNodeId: null,
  panelTab: "chat",
  dragOverNodeId: null,
  contextMenu: null,
  sidePanelWidth: 480, // matches SIDEPANEL_DEFAULT_WIDTH in SidePanel.tsx
  setSidePanelWidth: (w) => set({ sidePanelWidth: w }),
  templatePaletteOpen: false,
  setTemplatePaletteOpen: (open) => set({ templatePaletteOpen: open }),
  // Batch selection
  selectedNodeIds: new Set<string>(),
  toggleNodeSelection: (id) => {
    const prev = get().selectedNodeIds;
    const next = new Set(prev);
    if (next.has(id)) {
      next.delete(id);
    } else {
      next.add(id);
    }
    set({ selectedNodeIds: next });
  },
  clearSelection: () => set({ selectedNodeIds: new Set<string>() }),
  batchRestart: async () => {
    const ids = Array.from(get().selectedNodeIds);
    const results = await Promise.allSettled(
      ids.map((id) => api.post(`/workspaces/${id}/restart`))
    );
    const failed: string[] = [];
    results.forEach((r, i) => {
      if (r.status === "fulfilled") {
        get().updateNodeData(ids[i], { needsRestart: false });
      } else {
        failed.push(ids[i]);
      }
    });
    // Keep failed IDs selected so the user can retry; drop the successful ones.
    set({ selectedNodeIds: new Set(failed) });
    if (failed.length > 0) {
      throw new Error(`${failed.length}/${ids.length} restart(s) failed`);
    }
  },
  batchPause: async () => {
    const ids = Array.from(get().selectedNodeIds);
    const results = await Promise.allSettled(
      ids.map((id) => api.post(`/workspaces/${id}/pause`))
    );
    const failed: string[] = [];
    results.forEach((r, i) => {
      if (r.status !== "fulfilled") failed.push(ids[i]);
    });
    set({ selectedNodeIds: new Set(failed) });
    if (failed.length > 0) {
      throw new Error(`${failed.length}/${ids.length} pause(s) failed`);
    }
  },
  batchDelete: async () => {
    const ids = Array.from(get().selectedNodeIds);
    const results = await Promise.allSettled(
      ids.map((id) => api.del(`/workspaces/${id}`))
    );
    const failed: string[] = [];
    results.forEach((r, i) => {
      if (r.status === "fulfilled") {
        get().removeNode(ids[i]);
      } else {
        failed.push(ids[i]);
      }
    });
    // Keep failed IDs selected so the user can retry; the successful ones
    // were already removed from `nodes` above.
    set({ selectedNodeIds: new Set(failed) });
    if (failed.length > 0) {
      throw new Error(`${failed.length}/${ids.length} delete(s) failed`);
    }
  },
  wsStatus: "connecting",
  setWsStatus: (status) => set({ wsStatus: status }),
  hydrationError: null,
  setHydrationError: (error) => set({ hydrationError: error }),
  // A2A overlay — default on, persisted to localStorage
  a2aEdges: [],
  setA2AEdges: (edges) => set({ a2aEdges: edges }),
  showA2AEdges:
    typeof window !== "undefined"
      ? localStorage.getItem("molecule:show-a2a-edges") !== "false"
      : true,
  setShowA2AEdges: (show) => {
    set({ showA2AEdges: show });
    if (typeof window !== "undefined") {
      localStorage.setItem("molecule:show-a2a-edges", String(show));
    }
  },

  viewport: { x: 0, y: 0, zoom: 1 },

  selectNode: (id) => set({ selectedNodeId: id }),
  openContextMenu: (menu) => set({ contextMenu: menu }),
  closeContextMenu: () => set({ contextMenu: null }),
  pendingDelete: null,
  setPendingDelete: (v) => set({ pendingDelete: v }),
  deletingIds: new Set<string>(),
  beginDelete: (ids) => {
    const next = new Set(get().deletingIds);
    for (const id of ids) next.add(id);
    set({ deletingIds: next });
  },
  endDelete: (ids) => {
    const next = new Set(get().deletingIds);
    for (const id of ids) next.delete(id);
    set({ deletingIds: next });
  },
  searchOpen: false,
  setSearchOpen: (open) => set({ searchOpen: open }),
  agentMessages: {},
  consumeAgentMessages: (workspaceId) => {
    const msgs = get().agentMessages[workspaceId] || [];
    if (msgs.length > 0) {
      const { agentMessages } = get();
      const { [workspaceId]: _, ...rest } = agentMessages;
      set({ agentMessages: rest });
    }
    return msgs;
  },
  setViewport: (v) => set({ viewport: v }),
  saveViewport: async (x, y, zoom) => {
    set({ viewport: { x, y, zoom } });
    try {
      await api.put(`/canvas/viewport`, { x, y, zoom });
    } catch {
      // Non-critical — viewport save failure doesn't block user
    }
  },
  setPanelTab: (tab) => set({ panelTab: tab }),
  setDragOverNode: (id) => set({ dragOverNodeId: id }),

  batchNest: async (nodeIds, targetId) => {
    if (nodeIds.length === 0) return;
    // Selection-roots filter: if the user selected both A and A's
    // descendant B and dragged the pair into T, the intent is "move
    // the subtree" — B should stay under A, not become a sibling of
    // A under T. Drop every selected node whose ancestor is also
    // selected; those will follow their ancestor via React Flow's
    // parent-of binding automatically.
    const selectedSet = new Set(nodeIds);
    const { nodes: before, edges: beforeEdges } = get();
    const byId = new Map(before.map((n) => [n.id, n]));
    const rootsOnly: string[] = [];
    for (const id of nodeIds) {
      let cursor = byId.get(id)?.data.parentId ?? null;
      let hasSelectedAncestor = false;
      // Seen-set guards against a corrupt parentId cycle. Shouldn't
      // happen with a healthy backend — nestNode itself blocks cycles
      // via isDescendant — but this walk is user-triggered and the
      // cost of the guard is one set allocation per selected node.
      const seen = new Set<string>();
      while (cursor && !seen.has(cursor)) {
        seen.add(cursor);
        if (selectedSet.has(cursor)) {
          hasSelectedAncestor = true;
          break;
        }
        cursor = byId.get(cursor)?.data.parentId ?? null;
      }
      if (!hasSelectedAncestor) rootsOnly.push(id);
    }
    if (rootsOnly.length === 0) return;
    if (rootsOnly.length === 1) {
      await get().nestNode(rootsOnly[0], targetId);
      return;
    }
    // Batch path: do all state math against one snapshot so every
    // selected node sees the same "before" world, commit one set(),
    // then fire every PATCH in parallel. Previously this called
    // nestNode sequentially, which cost 2N round-trips (parent_id +
    // x/y) strictly serialized; now it's 1 round-trip per node, all
    // in flight at once. For a typical 3-5 node selection on a
    // ~200ms link this drops the perceived re-parent latency from
    // ~2s to ~200ms.

    const absOf = (id: string | null | undefined): { x: number; y: number } => {
      let sum = { x: 0, y: 0 };
      let cursor: string | null | undefined = id;
      while (cursor) {
        const n = byId.get(cursor);
        if (!n) break;
        sum = { x: sum.x + n.position.x, y: sum.y + n.position.y };
        cursor = n.data.parentId;
      }
      return sum;
    };
    const depthOf = (id: string | null | undefined): number => {
      let d = 0;
      let cursor: string | null | undefined = id;
      while (cursor) {
        const n = byId.get(cursor);
        if (!n) break;
        cursor = n.data.parentId;
        d += 1;
      }
      return d;
    };

    const newParentAbs = absOf(targetId);
    const newOwnDepth = targetId ? depthOf(targetId) + 1 : 0;

    interface Plan {
      id: string;
      newRelative: { x: number; y: number };
      draggedAbs: { x: number; y: number };
      depthDelta: number;
    }
    const plan: Plan[] = [];
    const movedIds = new Set<string>();
    // Filter out nodes that would be invalid targets / no-ops.
    for (const id of rootsOnly) {
      const dragged = byId.get(id);
      if (!dragged) continue;
      const currentParentId = dragged.data.parentId;
      if (currentParentId === targetId) continue;
      // Can't nest into yourself or your own descendant.
      if (targetId && get().isDescendant(id, targetId)) continue;
      const oldParentAbs = absOf(currentParentId);
      const draggedAbs = {
        x: dragged.position.x + oldParentAbs.x,
        y: dragged.position.y + oldParentAbs.y,
      };
      const newRelative = {
        x: draggedAbs.x - newParentAbs.x,
        y: draggedAbs.y - newParentAbs.y,
      };
      const oldOwnDepth =
        dragged.zIndex ?? depthOf(currentParentId) + (currentParentId ? 1 : 0);
      plan.push({
        id,
        newRelative,
        draggedAbs,
        depthDelta: newOwnDepth - oldOwnDepth,
      });
      movedIds.add(id);
      // Every descendant of a moved node also shifts by the same delta
      // so grandchildren don't fall behind their re-parented ancestor.
      const bfs = [id];
      while (bfs.length) {
        const head = bfs.shift()!;
        for (const n of before) {
          if (n.data.parentId === head && !movedIds.has(n.id)) {
            movedIds.add(n.id);
            bfs.push(n.id);
          }
        }
      }
    }

    if (plan.length === 0) return;
    const planById = new Map(plan.map((p) => [p.id, p]));

    // One optimistic set() covers every re-parent + every descendant
    // zIndex shift; no further state mutations before the PATCHes come
    // back (failed PATCHes roll back individual nodes below).
    set({
      nodes: before.map((n) => {
        const p = planById.get(n.id);
        if (p) {
          return {
            ...n,
            position: p.newRelative,
            parentId: targetId ?? undefined,
            zIndex: newOwnDepth,
            data: { ...n.data, parentId: targetId },
          };
        }
        // Descendant of a moved node — shift zIndex only. Find the
        // nearest ancestor in `plan` (walking up parents) to know
        // which depthDelta applies.
        if (movedIds.has(n.id)) {
          let cursor: string | null | undefined = n.data.parentId;
          while (cursor) {
            const anc = planById.get(cursor);
            if (anc) {
              if (anc.depthDelta === 0) break;
              return { ...n, zIndex: (n.zIndex ?? 0) + anc.depthDelta };
            }
            cursor = byId.get(cursor)?.data.parentId ?? null;
          }
          return n;
        }
        return n;
      }),
      edges: beforeEdges.filter(
        (e) => !movedIds.has(e.source) && !movedIds.has(e.target),
      ),
    });
    // Keep parents before children in the array (same invariant
    // nestNode enforces). Needed after multi-select re-parent because
    // the selection order is user-driven.
    set({ nodes: sortParentsBeforeChildren(get().nodes) });

    // Fire every PATCH in parallel. Individual failures roll back just
    // that node (others remain committed, matching the single-node
    // rollback behaviour in nestNode).
    const results = await Promise.allSettled(
      plan.map((p) =>
        api.patch(`/workspaces/${p.id}`, {
          parent_id: targetId,
          x: p.draggedAbs.x,
          y: p.draggedAbs.y,
        }),
      ),
    );
    const rolledBack: string[] = [];
    for (let i = 0; i < results.length; i++) {
      if (results[i].status === "rejected") rolledBack.push(plan[i].id);
    }
    if (rolledBack.length > 0) {
      const rollbackSet = new Set(rolledBack);
      set({
        nodes: get().nodes.map((n) => {
          if (!rollbackSet.has(n.id)) return n;
          const original = byId.get(n.id);
          if (!original) return n;
          return {
            ...n,
            position: original.position,
            parentId: original.parentId,
            zIndex: original.zIndex,
            data: { ...n.data, parentId: original.data.parentId },
          };
        }),
      });
      // Surface the partial failure — silent rollback would otherwise
      // leave the canvas in a state the user can't explain ("I dragged
      // 5 cards, 3 moved and 2 snapped back?"). Cap the name list so a
      // 50-node partial failure doesn't overflow the toast container.
      const NAMES_IN_TOAST = 3;
      const names = rolledBack
        .map((id) => byId.get(id)?.data.name)
        .filter((n): n is string => Boolean(n));
      const shown = names.slice(0, NAMES_IN_TOAST).join(", ");
      const overflow = names.length - NAMES_IN_TOAST;
      const listFragment = shown
        ? overflow > 0
          ? `: ${shown} and ${overflow} more`
          : `: ${shown}`
        : "";
      showToast(
        `Could not re-parent ${rolledBack.length} of ${plan.length} workspace${plan.length === 1 ? "" : "s"}${listFragment}`,
        "error",
      );
    }
  },

  bumpZOrder: (nodeId, direction) => {
    const { nodes } = get();
    const target = nodes.find((n) => n.id === nodeId);
    if (!target) return;
    // Siblings share parentId; re-rank them by their current zIndex (then
    // insertion order) so we can SWAP the target with its neighbour in
    // the bump direction rather than drifting zIndex up/down unbounded.
    // This keeps sibling zIndex values within `[baseDepth, baseDepth+N)`,
    // which is what findDropTarget's tiebreakers assume.
    const siblings = nodes
      .filter((n) => n.data.parentId === target.data.parentId)
      .slice()
      .sort((a, b) => (a.zIndex ?? 0) - (b.zIndex ?? 0));
    if (siblings.length < 2) return;
    const idx = siblings.findIndex((n) => n.id === nodeId);
    const neighbourIdx = idx + direction;
    if (neighbourIdx < 0 || neighbourIdx >= siblings.length) return;
    const neighbour = siblings[neighbourIdx];
    const targetZ = target.zIndex ?? 0;
    const neighbourZ = neighbour.zIndex ?? 0;
    // Ensure a visible swap even when both had identical zIndex (fresh
    // topology: every sibling starts at zIndex=depth). Nudge the
    // neighbour one step the other way so the pair stays adjacent.
    const resolvedTargetZ = targetZ === neighbourZ ? targetZ + direction : neighbourZ;
    const resolvedNeighbourZ = targetZ === neighbourZ ? targetZ : targetZ;
    set({
      nodes: nodes.map((n) => {
        if (n.id === nodeId) return { ...n, zIndex: resolvedTargetZ };
        if (n.id === neighbour.id) return { ...n, zIndex: resolvedNeighbourZ };
        return n;
      }),
    });
  },

  isDescendant: (ancestorId, nodeId) => {
    const { nodes } = get();
    let current = nodes.find((n) => n.id === nodeId);
    while (current?.data.parentId) {
      if (current.data.parentId === ancestorId) return true;
      current = nodes.find((n) => n.id === current?.data.parentId);
    }
    return false;
  },

  nestNode: async (draggedId, targetId) => {
    const { nodes, edges } = get();
    const dragged = nodes.find((n) => n.id === draggedId);
    if (!dragged) return;
    const currentParentId = dragged.data.parentId;
    if (currentParentId === targetId) return;

    // Compute each ancestor's absolute position by walking up the
    // parentId chain. We need this to translate the dragged node's
    // `position` (relative to its current parent when nested) between
    // the old and new coordinate spaces so the card doesn't visually
    // jump on nest/unnest.
    const absOf = (id: string | null): { x: number; y: number } => {
      let sum = { x: 0, y: 0 };
      let cursor: string | null = id;
      while (cursor) {
        const n = nodes.find((nn) => nn.id === cursor);
        if (!n) break;
        sum = { x: sum.x + n.position.x, y: sum.y + n.position.y };
        cursor = n.data.parentId;
      }
      return sum;
    };
    const oldParentAbs = absOf(currentParentId);
    const newParentAbs = absOf(targetId);
    const draggedAbs = {
      x: dragged.position.x + oldParentAbs.x,
      y: dragged.position.y + oldParentAbs.y,
    };
    const newRelative = {
      x: draggedAbs.x - newParentAbs.x,
      y: draggedAbs.y - newParentAbs.y,
    };

    const newEdges = edges.filter(
      (e) => e.source !== draggedId && e.target !== draggedId,
    );

    // Depth walk so zIndex gets bumped correctly on nest/unnest
    // (children render above their new ancestor chain). `depthOf(null)`
    // returns 0; for any non-null cursor we count one hop per ancestor.
    const depthOf = (id: string | null | undefined): number => {
      let d = 0;
      let cursor: string | null | undefined = id;
      while (cursor) {
        const n = nodes.find((nn) => nn.id === cursor);
        if (!n) break;
        cursor = n.data.parentId;
        d += 1;
      }
      return d;
    };
    const newOwnDepth = targetId ? depthOf(targetId) + 1 : 0;
    const oldOwnDepth = dragged.zIndex ?? depthOf(currentParentId) + (currentParentId ? 1 : 0);
    const depthDelta = newOwnDepth - oldOwnDepth;

    // Collect every descendant of the dragged node so we can shift their
    // zIndex by the same depthDelta — otherwise grandchildren stay at
    // their old depth zIndex after the move and render below ancestors
    // they just joined. BFS to avoid stack surprises on deep hierarchies.
    const movedIds = new Set<string>([draggedId]);
    const bfsQueue = [draggedId];
    while (bfsQueue.length) {
      const head = bfsQueue.shift()!;
      for (const n of nodes) {
        if (n.data.parentId === head && !movedIds.has(n.id)) {
          movedIds.add(n.id);
          bfsQueue.push(n.id);
        }
      }
    }

    // When a child leaves its parent, clear the parent's explicit
    // width/height. growParentsToFitChildren is grow-only so it can't
    // shrink on its own; without this, a parent that auto-grew to
    // contain the dragged child stays at that size after un-nest,
    // leaving a large empty frame. React Flow then measures the new
    // size from the card's own min-width/min-height CSS.
    const shrinkOldParent = !!currentParentId && targetId !== currentParentId;

    set({
      nodes: nodes.map((n) => {
        if (n.id === draggedId) {
          return {
            ...n,
            position: newRelative,
            parentId: targetId ?? undefined,
            zIndex: newOwnDepth,
            data: { ...n.data, parentId: targetId },
          };
        }
        if (shrinkOldParent && n.id === currentParentId) {
          const { width: _w, height: _h, ...rest } = n;
          void _w; void _h;
          return rest as typeof n;
        }
        if (movedIds.has(n.id) && depthDelta !== 0) {
          return { ...n, zIndex: (n.zIndex ?? 0) + depthDelta };
        }
        return n;
      }),
      edges: newEdges,
    });
    // React Flow requires parents before children in the array. Without
    // this re-sort a newly-nested child can end up ahead of its new
    // parent, which makes RF log "Parent node not found" and render the
    // child at canvas-absolute coords (far outside the parent, which
    // is the flash-bug the user just flagged).
    set({ nodes: sortParentsBeforeChildren(get().nodes) });

    try {
      // One round-trip per nest: the /workspaces/:id PATCH handler
      // accepts parent_id + x + y in a single body. The absolute x/y
      // is what the DB stores as canonical (matches savePosition
      // elsewhere), so reload renders the same place regardless of
      // which parent the child was under at save time.
      await api.patch(`/workspaces/${draggedId}`, {
        parent_id: targetId,
        x: draggedAbs.x,
        y: draggedAbs.y,
      });
    } catch {
      set({
        nodes: get().nodes.map((n) =>
          n.id === draggedId
            ? {
                ...n,
                position: dragged.position,
                parentId: currentParentId ?? undefined,
                data: { ...n.data, parentId: currentParentId },
              }
            : n,
        ),
        edges,
      });
    }
  },

  getSelectedNode: () => {
    const { nodes, selectedNodeId } = get();
    if (!selectedNodeId) return null;
    return nodes.find((n) => n.id === selectedNodeId) ?? null;
  },

  updateNodeData: (id, data) => {
    set({
      nodes: get().nodes.map((n) =>
        n.id === id ? { ...n, data: { ...n.data, ...data } } : n
      ),
    });
  },

  restartWorkspace: async (id) => {
    await api.post(`/workspaces/${id}/restart`);
    get().updateNodeData(id, { needsRestart: false });
  },

  removeNode: (id) => {
    const { nodes, edges, selectedNodeId } = get();
    // Re-parent children to the deleted node's parent (or root).
    // Children are first-class RF nodes now — we just re-point their
    // parentId (both RF's native field and our data mirror). No hidden
    // flag is toggled because cards are always visible.
    const deletedNode = nodes.find((n) => n.id === id);
    const parentOfDeleted = deletedNode?.data.parentId ?? null;
    set({
      nodes: nodes
        .filter((n) => n.id !== id)
        .map((n) =>
          n.data.parentId === id
            ? {
                ...n,
                parentId: parentOfDeleted ?? undefined,
                data: { ...n.data, parentId: parentOfDeleted },
              }
            : n
        ),
      edges: edges.filter((e) => e.source !== id && e.target !== id),
      selectedNodeId: selectedNodeId === id ? null : selectedNodeId,
    });
  },

  removeSubtree: (rootId) => {
    const { nodes, edges, selectedNodeId } = get();
    // Build a parentId → childIds index once so the descent is O(N),
    // not O(N · depth). The store typically holds <500 nodes; even
    // doing a linear scan per parent would be fine, but the index
    // keeps the cost predictable as orgs grow.
    const childrenByParent = new Map<string, string[]>();
    for (const n of nodes) {
      const p = n.data.parentId ?? null;
      if (p === null) continue;
      const arr = childrenByParent.get(p);
      if (arr) arr.push(n.id);
      else childrenByParent.set(p, [n.id]);
    }
    const removed = new Set<string>([rootId]);
    const stack = [rootId];
    while (stack.length) {
      const cur = stack.pop()!;
      const kids = childrenByParent.get(cur);
      if (!kids) continue;
      for (const k of kids) {
        if (!removed.has(k)) {
          removed.add(k);
          stack.push(k);
        }
      }
    }
    // Tombstone removed ids so an in-flight GET /workspaces can't
    // resurrect them via hydrate (#2069).
    markDeleted(removed);
    set({
      nodes: nodes.filter((n) => !removed.has(n.id)),
      edges: edges.filter((e) => !removed.has(e.source) && !removed.has(e.target)),
      selectedNodeId:
        selectedNodeId !== null && removed.has(selectedNodeId)
          ? null
          : selectedNodeId,
    });
  },

  hydrate: (workspaces: WorkspaceData[]) => {
    // Drop ids tombstoned by a recent removeSubtree (#2069 — stale
    // in-flight GET /workspaces).
    const live = workspaces.filter((w) => !wasRecentlyDeleted(w.id));
    const layoutOverrides = computeAutoLayout(live);
    // Carry the live measured/grown parent sizes from the existing
    // store into the rebuild. buildNodesAndEdges runs an auto-rescue
    // pass on each child to detach orphans whose stored relative
    // position falls outside the parent bbox — without the live
    // size, the bbox is the initial grid-derived minimum, which
    // false-flags any child the user has dragged into the
    // user-grown area. Periodic rehydrate (socket.ts health check,
    // 30s) was reasserting the rescue against legitimate user
    // placements, causing the "child jumps to weird location, then
    // settles" symptom.
    const current = get().nodes;
    const currentParentSizes = new Map<string, { width: number; height: number }>();
    for (const n of current) {
      const w = (n.measured?.width ?? n.width) as number | undefined;
      const h = (n.measured?.height ?? n.height) as number | undefined;
      if (typeof w === "number" && typeof h === "number") {
        currentParentSizes.set(n.id, { width: w, height: h });
      }
    }
    const { nodes, edges } = buildNodesAndEdges(
      live,
      layoutOverrides,
      currentParentSizes,
    );
    set({ nodes, edges });
    for (const [nodeId, { x, y }] of layoutOverrides) {
      api.patch(`/workspaces/${nodeId}`, { x, y }).catch(() => {});
    }
  },

  applyEvent: (msg: WSMessage) => {
    handleCanvasEvent(msg, get, set);
  },

  onNodesChange: (changes) => {
    const next = applyNodeChanges(changes, get().nodes);
    // Parent auto-grow is intentionally conservative. Running
    // growParentsToFitChildren on every change (including the dozens of
    // position updates emitted during a single drag) caused the
    // "edge-chase" artifact tldraw documented — as the parent grows in
    // response to the child near its edge, the child's relative
    // position becomes valid again and the grow stops mid-drag, only to
    // resume on the next tick. Commit-on-release: only run grow when a
    // change set contains a `dimensions` change (NodeResizer commit),
    // not on pure `position` changes. Drag-stop grow is handled
    // explicitly in Canvas.onNodeDragStop via growOnce().
    const hasDimensionChange = changes.some((c) => c.type === "dimensions");
    set({ nodes: hasDimensionChange ? growParentsToFitChildren(next) : next });
  },

  growParentsToFitChildren: () => {
    set({ nodes: growParentsToFitChildren(get().nodes) });
  },

  setCollapsed: (parentId, collapsed) => {
    const { nodes } = get();
    // Step 1 — apply the new collapsed flag on the target.
    const updatedCollapsed = new Map<string, boolean>();
    for (const n of nodes) {
      updatedCollapsed.set(
        n.id,
        n.id === parentId ? collapsed : !!n.data.collapsed,
      );
    }
    // Step 2 — index children once so the visibility pass is O(n), not
    // O(n·d). Walk roots downward, inheriting `hiddenBecauseAncestor`
    // so a node is hidden iff ANY ancestor in the chain is collapsed.
    // This matches canvas-topology.buildNodesAndEdges so setCollapsed
    // and hydrate produce identical node.hidden flags — no drift when
    // the server pushes a fresh snapshot mid-session.
    const childrenByParent = new Map<string | null, string[]>();
    for (const n of nodes) {
      const p = n.data.parentId ?? null;
      const arr = childrenByParent.get(p) ?? [];
      arr.push(n.id);
      childrenByParent.set(p, arr);
    }
    const hiddenById = new Map<string, boolean>();
    const stack: Array<{ id: string; hidden: boolean }> = (
      childrenByParent.get(null) ?? []
    ).map((id) => ({ id, hidden: false }));
    while (stack.length) {
      const { id, hidden } = stack.pop()!;
      hiddenById.set(id, hidden);
      const isCollapsed = updatedCollapsed.get(id) ?? false;
      for (const childId of childrenByParent.get(id) ?? []) {
        stack.push({ id: childId, hidden: hidden || isCollapsed });
      }
    }
    // Expanded size must fit the target's ACTUAL children, including
    // any nested-parent children that are themselves oversized. Using a
    // leaf-count formula (parentMinSize) would undersize the parent
    // whenever a child was itself a team — e.g. CTO expanding to show
    // Dev Lead (which carries 6 engineers) would render Dev Lead
    // clipped. Read each direct child's current width/height from the
    // node itself; those already reflect the subtree sizing computed
    // in buildNodesAndEdges.
    const directChildIds = childrenByParent.get(parentId) ?? [];
    const childSizes = directChildIds.map((cid) => {
      const cn = nodes.find((n) => n.id === cid);
      return {
        width: (cn?.width as number | undefined) ?? CHILD_DEFAULT_WIDTH,
        height: (cn?.height as number | undefined) ?? CHILD_DEFAULT_HEIGHT,
      };
    });
    const expandedSize = parentMinSizeFromChildren(childSizes);

    set({
      nodes: nodes.map((n) => {
        const isTarget = n.id === parentId;
        const nextHidden = hiddenById.get(n.id) ?? false;
        if (!isTarget && n.hidden === nextHidden) return n;
        if (!isTarget) {
          return { ...n, hidden: nextHidden };
        }
        // Target parent: update collapsed flag + size. Dropping width/
        // height would leave the node at its prior (possibly huge)
        // dimensions after a collapse, leaving a gigantic empty card
        // with no visible children.
        return {
          ...n,
          hidden: nextHidden,
          data: { ...n.data, collapsed },
          width: collapsed ? CHILD_DEFAULT_WIDTH : expandedSize.width,
          height: collapsed ? CHILD_DEFAULT_HEIGHT : expandedSize.height,
        };
      }),
    });
  },

  arrangeChildren: (parentId) => {
    const { nodes } = get();
    const kids = nodes
      .filter((n) => n.parentId === parentId)
      .sort((a, b) => (a.data.name || "").localeCompare(b.data.name || ""));
    if (kids.length === 0) return;
    const slotByKid = new Map<string, { x: number; y: number }>();
    kids.forEach((k, i) => slotByKid.set(k.id, defaultChildSlot(i)));

    // Absolute position of the parent, walking the full ancestor chain.
    // Required for a correct PATCH payload when the parent itself is
    // nested — `parent.position` is RELATIVE to its own parent, so a
    // naive `slot + parent.position` would store parent-local coords
    // as if they were absolute and corrupt the workspace on reload.
    const absOf = (id: string | null | undefined): { x: number; y: number } => {
      let sum = { x: 0, y: 0 };
      let cursor: string | null | undefined = id;
      while (cursor) {
        const n = nodes.find((nn) => nn.id === cursor);
        if (!n) break;
        sum = { x: sum.x + n.position.x, y: sum.y + n.position.y };
        cursor = n.data.parentId;
      }
      return sum;
    };
    const parentAbs = absOf(parentId);

    set({
      nodes: nodes.map((n) => {
        const slot = slotByKid.get(n.id);
        return slot ? { ...n, position: slot } : n;
      }),
    });

    for (const k of kids) {
      const slot = slotByKid.get(k.id)!;
      const absX = slot.x + parentAbs.x;
      const absY = slot.y + parentAbs.y;
      api.patch(`/workspaces/${k.id}`, { x: absX, y: absY }).catch((e) => {
        console.warn(`arrangeChildren: failed to persist position for ${k.id}`, e);
      });
    }
  },

  savePosition: async (nodeId: string, x: number, y: number) => {
    try {
      await api.patch(`/workspaces/${nodeId}`, { x, y });
    } catch {
      // Non-critical — position save failure doesn't block user
    }
  },
}));
