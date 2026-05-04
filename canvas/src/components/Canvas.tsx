"use client";

import { useCallback, useMemo } from "react";
import {
  ReactFlow,
  ReactFlowProvider,
  Background,
  Controls,
  MiniMap,
  type Edge,
  BackgroundVariant,
} from "@xyflow/react";
import "@xyflow/react/dist/style.css";

import { useCanvasStore } from "@/store/canvas";
import { useTheme } from "@/lib/theme-provider";
import { A2ATopologyOverlay } from "./A2ATopologyOverlay";
import { WorkspaceNode } from "./WorkspaceNode";
import { SidePanel } from "./SidePanel";
import { CreateWorkspaceButton } from "./CreateWorkspaceDialog";
import { ContextMenu } from "./ContextMenu";
import { TemplatePalette } from "./TemplatePalette";
import { ApprovalBanner } from "./ApprovalBanner";
import { BundleDropZone } from "./BundleDropZone";
import { EmptyState } from "./EmptyState";
import { OnboardingWizard } from "./OnboardingWizard";
import { SearchDialog } from "./SearchDialog";
import { Toaster, showToast } from "./Toaster";
import { Toolbar } from "./Toolbar";
import { ConfirmDialog } from "./ConfirmDialog";
import { api } from "@/lib/api";
import { SettingsPanel, DeleteConfirmDialog } from "./settings";
import { BatchActionBar } from "./BatchActionBar";
import { ProvisioningTimeout } from "./ProvisioningTimeout";

import { DropTargetBadge } from "./canvas/DropTargetBadge";
import { useDragHandlers } from "./canvas/useDragHandlers";
import { useKeyboardShortcuts } from "./canvas/useKeyboardShortcuts";
import { useCanvasViewport } from "./canvas/useCanvasViewport";
import { A2AEdge } from "./canvas/A2AEdge";

const nodeTypes = {
  workspaceNode: WorkspaceNode,
};

// Custom edge types. The default React Flow edge renders its label
// inside the SVG group (always under nodes) with pointerEvents: none
// inherited from the path. A2AEdge portals the label to a sibling
// DOM layer so it renders above nodes and accepts clicks. Keep the
// reference stable (module-scope const) so React Flow doesn't see a
// new edgeTypes object on every render and warn about prop churn.
const edgeTypes = {
  a2a: A2AEdge,
};

const defaultEdgeOptions: Partial<Edge> = {
  animated: true,
  style: {
    stroke: "#3f3f46",
    strokeWidth: 1.5,
  },
};

export function Canvas() {
  return (
    <ReactFlowProvider>
      <CanvasInner />
    </ReactFlowProvider>
  );
}

function CanvasInner() {
  // ReactFlow's `colorMode` prop drives the styling of every viewport
  // primitive it renders directly (background dots, edge defaults,
  // selection rings, controls, minimap mask). Pre-fix this was hard-pinned
  // to "dark" — so on light theme the chrome (toolbar, side panel) flipped
  // to warm-paper but the canvas backplate + edges stayed black, leaving a
  // half-themed page. Pull resolvedTheme so the canvas matches the user's
  // selected mode (and the system preference when they pick "system").
  const { resolvedTheme } = useTheme();
  const rawNodes = useCanvasStore((s) => s.nodes);
  const edges = useCanvasStore((s) => s.edges);
  const a2aEdges = useCanvasStore((s) => s.a2aEdges);
  const showA2AEdges = useCanvasStore((s) => s.showA2AEdges);
  const deletingIds = useCanvasStore((s) => s.deletingIds);
  const allEdges = useMemo(
    () => (showA2AEdges ? [...edges, ...a2aEdges] : edges),
    [edges, a2aEdges, showA2AEdges],
  );
  // Drag-lock during a system-owned operation (deploy OR delete).
  // React Flow respects Node.draggable, which stops the gesture
  // before it starts — preventDefault() on the drag-start callback
  // isn't authoritative in v12. We project `draggable: false` onto
  // each locked node before handing the array to ReactFlow; the
  // drag-start handler in useDragHandlers remains as a belt-and-
  // braces check.
  //
  // Perf: short-circuit when nothing is provisioning so the memo
  // passes rawNodes through unchanged (identity-stable → RF
  // reconciles nothing). When a deploy IS active, build an O(n)
  // root index once and re-use it. Critically, do NOT spread every
  // node — only mutate the locked ones — so unmodified nodes keep
  // their object identity and RF's per-node memo short-circuits.
  const nodes = useMemo(() => {
    const anyProvisioning = rawNodes.some((n) => n.data.status === "provisioning");
    const anyDeleting = deletingIds.size > 0;
    if (!anyProvisioning && !anyDeleting) return rawNodes;

    const byId = new Map<string, typeof rawNodes[number]>();
    for (const n of rawNodes) byId.set(n.id, n);
    const rootOf = new Map<string, string>();
    const resolveRoot = (id: string): string => {
      // Iterative walk guards against a pathological cycle (hostile
      // data) — recursion would hit the stack limit on a deep tree.
      const visited = new Set<string>();
      let cursor: string | null = id;
      while (cursor) {
        if (visited.has(cursor)) break;
        visited.add(cursor);
        const cached = rootOf.get(cursor);
        if (cached) {
          for (const seenId of visited) rootOf.set(seenId, cached);
          return cached;
        }
        const n = byId.get(cursor);
        if (!n) break;
        if (!n.data.parentId) {
          for (const seenId of visited) rootOf.set(seenId, cursor);
          return cursor;
        }
        cursor = n.data.parentId;
      }
      return id;
    };

    const provisioningByRoot = new Map<string, number>();
    for (const n of rawNodes) {
      if (n.data.status !== "provisioning") continue;
      const rootId = resolveRoot(n.id);
      provisioningByRoot.set(rootId, (provisioningByRoot.get(rootId) ?? 0) + 1);
    }

    let touched = false;
    const next = rawNodes.map((n) => {
      const rootId = resolveRoot(n.id);
      const deployLocked = n.id !== rootId && (provisioningByRoot.get(rootId) ?? 0) > 0;
      // Delete-locked: nothing in a subtree whose DELETE is in
      // flight should be draggable, INCLUDING the root of that
      // subtree (unlike deploy, there's no cancel — the delete
      // is irrevocable at this point).
      const deleteLocked = deletingIds.has(n.id);
      const shouldLock = deployLocked || deleteLocked;
      if (shouldLock && n.draggable !== false) {
        touched = true;
        return { ...n, draggable: false };
      }
      if (!shouldLock && n.draggable === false) {
        // Node was locked in a prior render; deploy cancelled /
        // completed, or delete failed and was reverted. Restore
        // default dragability.
        touched = true;
        const { draggable: _d, ...rest } = n;
        void _d;
        return rest as typeof n;
      }
      return n; // identity-preserved
    });
    return touched ? next : rawNodes;
  }, [rawNodes, deletingIds]);
  const onNodesChange = useCanvasStore((s) => s.onNodesChange);
  const selectNode = useCanvasStore((s) => s.selectNode);
  const selectedNodeId = useCanvasStore((s) => s.selectedNodeId);

  // Drag / nest lifecycle — handlers, pending-nest state, confirm/cancel.
  const {
    onNodeDragStart,
    onNodeDrag,
    onNodeDragStop,
    pendingNest,
    confirmNest,
    cancelNest,
  } = useDragHandlers();

  // Window-level keyboard shortcuts (Esc, Enter, Shift+Enter, Cmd+]/[, Z).
  useKeyboardShortcuts();

  // Pan-to-node / zoom-to-team CustomEvent listeners + viewport save.
  const { onMoveEnd } = useCanvasViewport();

  // Delete-confirmation lives in the store so the dialog survives ContextMenu
  // unmounting — the prior local-in-ContextMenu state raced with the menu's
  // outside-click handler.
  const pendingDelete = useCanvasStore((s) => s.pendingDelete);
  const setPendingDelete = useCanvasStore((s) => s.setPendingDelete);
  const removeSubtree = useCanvasStore((s) => s.removeSubtree);
  const confirmDelete = useCallback(async () => {
    if (!pendingDelete) return;
    const { id } = pendingDelete;
    setPendingDelete(null);
    // Compute the full subtree and mark it as "deleting" so every
    // node in the chain renders dim + non-draggable during the
    // network round-trip + the server-side cascade. Matches the
    // deploy-lock UX: once a system-initiated operation owns this
    // subtree, the user shouldn't be able to move its pieces
    // around until it resolves.
    const state = useCanvasStore.getState();
    const subtree = new Set<string>();
    const stack = [id];
    while (stack.length) {
      const nid = stack.pop()!;
      subtree.add(nid);
      for (const n of state.nodes) {
        if (n.data.parentId === nid) stack.push(n.id);
      }
    }
    state.beginDelete(subtree);
    try {
      await api.del(`/workspaces/${id}?confirm=true`);
      // Mirror the server-side cascade locally — drop the parent AND
      // every descendant in one atomic update. The per-descendant
      // WORKSPACE_REMOVED WS events still arrive (and are no-ops
      // because the nodes are already gone), but we no longer depend
      // on them: a wedged WS used to leave orphan child cards on the
      // canvas until the user refreshed the page.
      removeSubtree(id);
      state.endDelete(subtree);
    } catch (e) {
      // Network or server error — restore the subtree to normal
      // interaction and surface the error.
      state.endDelete(subtree);
      showToast(e instanceof Error ? e.message : "Delete failed", "error");
    }
  }, [pendingDelete, setPendingDelete, removeSubtree]);

  const onPaneClick = useCallback(() => {
    selectNode(null);
    const state = useCanvasStore.getState();
    state.closeContextMenu();
    state.clearSelection();
  }, [selectNode]);

  const viewport = useCanvasStore((s) => s.viewport);
  const defaultViewport = useMemo(
    () => ({ x: viewport.x, y: viewport.y, zoom: viewport.zoom }),
    // Only use the initial viewport — don't re-render on every save
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [],
  );

  const settingsWorkspaceId = selectedNodeId ?? "global";

  return (
    <>
      <a
        href="#canvas-main"
        className="sr-only focus:not-sr-only focus:absolute focus:top-2 focus:left-2 focus:z-50 focus:px-4 focus:py-2 focus:bg-surface-sunken focus:text-ink focus:rounded-lg focus:border focus:border-line"
      >
        Skip to canvas
      </a>
      <main id="canvas-main" className="w-screen h-screen bg-surface">
        <ReactFlow
          colorMode={resolvedTheme}
          nodes={nodes}
          edges={allEdges}
          onNodesChange={onNodesChange}
          onNodeDragStart={onNodeDragStart}
          onNodeDrag={onNodeDrag}
          onNodeDragStop={onNodeDragStop}
          onPaneClick={onPaneClick}
          onMoveEnd={onMoveEnd}
          nodeTypes={nodeTypes}
          edgeTypes={edgeTypes}
          defaultEdgeOptions={defaultEdgeOptions}
          defaultViewport={defaultViewport}
          fitView={viewport.x === 0 && viewport.y === 0 && viewport.zoom === 1}
          minZoom={0.1}
          maxZoom={2}
          proOptions={{ hideAttribution: true }}
          aria-label="Molecule AI workspace canvas"
        >
          <Background
            variant={BackgroundVariant.Dots}
            gap={24}
            size={1}
            // Match the line token so dots fade with the surface.
            // Hard-coded zinc-800 was invisible on warm-paper.
            color={resolvedTheme === "dark" ? "#27272a" : "#d4d0c4"}
          />
          <Controls
            className="!bg-surface-sunken/90 !border-line/50 !rounded-lg !shadow-xl !shadow-black/20 [&>button]:!bg-surface-card [&>button]:!border-line/50 [&>button]:!text-ink-mid [&>button:hover]:!bg-surface-card [&>button:hover]:!text-ink"
            showInteractive={false}
          />
          <MiniMap
            className="!bg-surface-sunken/90 !border-line/50 !rounded-lg !shadow-xl !shadow-black/20"
            // Mask dims off-viewport areas; tint matches the surface so
            // the dimming doesn't show as a black bar in light mode.
            maskColor={resolvedTheme === "dark" ? "rgba(0, 0, 0, 0.7)" : "rgba(232, 226, 211, 0.7)"}
            nodeColor={(node) => {
              // Parents show as a filled region — hierarchy visible at
              // a glance in the minimap without needing to zoom.
              const hasChildren = nodes.some((n) => n.parentId === node.id);
              if (hasChildren) return "#3b82f6";
              const status = (node.data as Record<string, unknown>)?.status;
              switch (status) {
                case "online":
                  return "#34d399";
                case "offline":
                  return "#52525b";
                case "degraded":
                  return "#fbbf24";
                case "failed":
                  return "#f87171";
                case "provisioning":
                  return "#38bdf8";
                default:
                  return "#3f3f46";
              }
            }}
            nodeStrokeColor={(node) => {
              const hasChildren = nodes.some((n) => n.parentId === node.id);
              return hasChildren ? "#60a5fa" : "transparent";
            }}
            nodeStrokeWidth={2}
            nodeBorderRadius={4}
          />
          <DropTargetBadge />
        </ReactFlow>

        {/* Screen-reader live region: announces workspace count on canvas load or change */}
        <div role="status" aria-live="polite" className="sr-only">
          {nodes.filter((n) => !n.parentId).length === 0
            ? "No workspaces on canvas"
            : `${nodes.filter((n) => !n.parentId).length} workspace${nodes.filter((n) => !n.parentId).length !== 1 ? "s" : ""} on canvas`}
        </div>

        {nodes.length === 0 && <EmptyState />}
        <A2ATopologyOverlay />
        <OnboardingWizard />
        <Toolbar />
        <ApprovalBanner />
        <BundleDropZone />
        <TemplatePalette />
        <SidePanel />
        <ContextMenu />
        <SearchDialog />
        <Toaster />
        <ProvisioningTimeout />
        {!selectedNodeId && <CreateWorkspaceButton />}
        <BatchActionBar />

        <ConfirmDialog
          open={!!pendingNest}
          title={pendingNest?.targetId ? "Nest Workspace" : "Extract Workspace"}
          message={
            pendingNest?.targetId
              ? `Move "${pendingNest.nodeName}" inside "${pendingNest.targetName}"? This changes the org hierarchy — ${pendingNest.nodeName} will become a sub-workspace of ${pendingNest.targetName}.`
              : `Extract "${pendingNest?.nodeName}" from "${pendingNest?.targetName}"? This moves it to the root level.`
          }
          confirmLabel={pendingNest?.targetId ? "Nest" : "Extract"}
          onConfirm={confirmNest}
          onCancel={cancelNest}
        />

        <ConfirmDialog
          open={!!pendingDelete}
          title={pendingDelete?.hasChildren ? "Delete Workspace and Children" : "Delete Workspace"}
          message={pendingDelete?.hasChildren
            ? `⚠️ Deleting "${pendingDelete?.name}" will permanently delete all of its child workspaces and their data. This cannot be undone.`
            : `Permanently delete "${pendingDelete?.name}"? This will stop the container and remove all configuration. This action cannot be undone.`}
          confirmLabel={pendingDelete?.hasChildren ? "Delete All" : "Delete"}
          confirmVariant="danger"
          onConfirm={confirmDelete}
          onCancel={() => setPendingDelete(null)}
        />

        <SettingsPanel workspaceId={settingsWorkspaceId} />
        <DeleteConfirmDialog workspaceId={settingsWorkspaceId} />
      </main>
    </>
  );
}
