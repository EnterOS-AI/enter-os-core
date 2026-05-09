"use client";

import { useEffect } from "react";
import { useCanvasStore } from "@/store/canvas";
import { type NodeChange, type Node } from "@xyflow/react";
import type { WorkspaceNodeData } from "@/store/canvas";

/** Returns true if the node has any direct child in the node list. */
function hasChildren(nodeId: string, nodes: Node<WorkspaceNodeData>[]): boolean {
  return nodes.some((n) => n.data.parentId === nodeId);
}

/**
 * Canvas-wide keyboard shortcuts. All bound to the document window so
 * they work regardless of focused node, except when the user is typing
 * into an input (`inInput` short-circuits handling).
 *
 *   Esc                  — close context menu, clear selection, deselect
 *   Enter                — descend into selected node's first child
 *   Shift+Enter          — ascend to selected node's parent
 *   Cmd/Ctrl+]           — bump selected node forward in z-order
 *   Cmd/Ctrl+[           — bump selected node backward in z-order
 *   Z                    — zoom-to-team if the selected node has children
 *   Arrow keys           — move selected node 10px (50px with Shift)
 *   Cmd/Ctrl+Arrow       — resize selected node (↑↓ height, ←→ width)
 *   Cmd/Ctrl+Shift+Arrow — resize by 2px per press (fine control)
 */
export function useKeyboardShortcuts() {
  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      const tag = (e.target as HTMLElement).tagName;
      const inInput =
        tag === "INPUT" ||
        tag === "TEXTAREA" ||
        tag === "SELECT" ||
        (e.target as HTMLElement).isContentEditable;

      if (e.key === "Escape") {
        const state = useCanvasStore.getState();
        if (state.contextMenu) {
          state.closeContextMenu();
        } else if (state.selectedNodeIds.size > 0) {
          state.clearSelection();
        } else if (state.selectedNodeId) {
          state.selectNode(null);
        }
      }

      // Figma-style hierarchy navigation. Skipped when the user is
      // typing so Enter can still submit forms.
      if (!inInput && (e.key === "Enter" || e.key === "NumpadEnter")) {
        e.preventDefault();
        const state = useCanvasStore.getState();
        const id = state.selectedNodeId;
        if (!id) return;
        if (e.shiftKey) {
          const sel = state.nodes.find((n) => n.id === id);
          const parentId = sel?.data.parentId ?? null;
          if (parentId) state.selectNode(parentId);
        } else {
          const firstChild = state.nodes.find((n) => n.data.parentId === id);
          if (firstChild) state.selectNode(firstChild.id);
        }
      }

      if (
        !inInput &&
        (e.metaKey || e.ctrlKey) &&
        (e.key === "]" || e.key === "[")
      ) {
        e.preventDefault();
        const state = useCanvasStore.getState();
        const id = state.selectedNodeId;
        if (!id) return;
        state.bumpZOrder(id, e.key === "]" ? 1 : -1);
      }

      if (!inInput && (e.key === "z" || e.key === "Z")) {
        const state = useCanvasStore.getState();
        const selectedId = state.selectedNodeId;
        if (!selectedId) return;
        const hasChildren = state.nodes.some(
          (n) => n.data.parentId === selectedId,
        );
        if (hasChildren) {
          window.dispatchEvent(
            new CustomEvent("molecule:zoom-to-team", {
              detail: { nodeId: selectedId },
            }),
          );
        }
      }

      // Arrow-key node movement — Figma-style keyboard drag for keyboard users.
      // 10 px per press, 50 px with Shift held. Only fires when a node
      // is selected and the target isn't a form control. Skipped when a
      // modifier key (Cmd/Ctrl/Alt) is held so those combos can be used
      // for other shortcuts (e.g. Cmd+Arrow = resize).
      if (
        !inInput &&
        !e.metaKey &&
        !e.ctrlKey &&
        !e.altKey &&
        (e.key === "ArrowUp" ||
          e.key === "ArrowDown" ||
          e.key === "ArrowLeft" ||
          e.key === "ArrowRight")
      ) {
        const state = useCanvasStore.getState();
        const selectedId = state.selectedNodeId;
        if (!selectedId) return;
        // Skip when a modal/dialog is already open — dialogs own their own
        // arrow-key semantics and shouldn't trigger canvas moves.
        if (document.querySelector('[role="dialog"][aria-modal="true"]')) return;
        e.preventDefault();
        const step = e.shiftKey ? 50 : 10;
        let dx = 0;
        let dy = 0;
        if (e.key === "ArrowUp") dy = -step;
        else if (e.key === "ArrowDown") dy = step;
        else if (e.key === "ArrowLeft") dx = -step;
        else dx = step;
        state.moveNode(selectedId, dx, dy);
      }

      // Cmd/Ctrl+Arrow — keyboard-accessible node resize.
      // ↑/↓ resizes height, ←/→ resizes width.
      // 10 px per press (2 px with Shift for fine control).
      // Uses the same onNodesChange('dimensions') path that NodeResizer uses.
      if (
        !inInput &&
        (e.metaKey || e.ctrlKey) &&
        (e.key === "ArrowUp" ||
          e.key === "ArrowDown" ||
          e.key === "ArrowLeft" ||
          e.key === "ArrowRight")
      ) {
        const state = useCanvasStore.getState();
        const selectedId = state.selectedNodeId;
        if (!selectedId) return;
        if (document.querySelector('[role="dialog"][aria-modal="true"]')) return;
        e.preventDefault();
        const step = e.shiftKey ? 2 : 10;
        const node = state.nodes.find((n) => n.id === selectedId);
        if (!node) return;
        const currentWidth = (node.width ?? 210) as number;
        const currentHeight = (node.height ?? 110) as number;
        const minWidth = hasChildren(node.id, state.nodes) ? 360 : 210;
        const minHeight = hasChildren(node.id, state.nodes) ? 200 : 110;
        let newWidth = currentWidth;
        let newHeight = currentHeight;
        if (e.key === "ArrowUp") newHeight = Math.max(minHeight, currentHeight - step);
        else if (e.key === "ArrowDown") newHeight = currentHeight + step;
        else if (e.key === "ArrowLeft") newWidth = Math.max(minWidth, currentWidth - step);
        else newWidth = currentWidth + step;
        const change: NodeChange = {
          type: "dimensions",
          id: selectedId,
          dimensions: { width: newWidth, height: newHeight },
        };
        state.onNodesChange([change]);
      }
    };
    window.addEventListener("keydown", handler);
    return () => window.removeEventListener("keydown", handler);
  }, []);
}
