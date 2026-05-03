"use client";

import { useState } from "react";
import { api } from "@/lib/api";
import { useCanvasStore } from "@/store/canvas";
import { showToast } from "@/components/Toaster";

interface Props {
  /** Root workspace of the org being deployed. The cancel action
   *  cascades delete through workspace-server's existing recursive
   *  delete handler, so we only need the root id. */
  rootId: string;
  rootName: string;
  /** Count rendered in the pill label; updated live as children
   *  come online (the useOrgDeployState hook recomputes on every
   *  status change). */
  workspaceCount: number;
}

/**
 * Cancel-deployment pill attached to the root of a deploying org.
 * One click → confirm dialog → DELETE /workspaces/:rootId?confirm=true
 * which cascades through every descendant server-side.
 *
 * Rendered inside the root's WorkspaceNode card via an absolute-
 * positioned overlay so it sits visually ON the card and moves with
 * drag. `className="nodrag"` stops React Flow from interpreting
 * clicks here as the start of a drag gesture.
 *
 * Deliberately uses only `.mol-deploy-cancel*` classes for styling —
 * every color / easing comes from theme-tokens.css, so a future
 * light-theme (or tenant-branded theme) inherits automatically.
 */
export function OrgCancelButton({ rootId, rootName, workspaceCount }: Props) {
  const [confirming, setConfirming] = useState(false);
  const [submitting, setSubmitting] = useState(false);

  const handleCancel = async () => {
    setSubmitting(true);
    // Populate deletingIds with the subtree so every descendant
    // (and the root) locks into the dim + non-draggable state for
    // the duration of the network round-trip + server cascade —
    // same treatment the regular delete gives. Otherwise the org
    // looks interactive for the several seconds between click and
    // the first WORKSPACE_REMOVED event.
    const preState = useCanvasStore.getState();
    const subtreeIds = new Set<string>();
    const walkStack = [rootId];
    while (walkStack.length) {
      const nid = walkStack.pop()!;
      subtreeIds.add(nid);
      for (const n of preState.nodes) {
        if (n.data.parentId === nid) walkStack.push(n.id);
      }
    }
    preState.beginDelete(subtreeIds);
    try {
      await api.del<{ status: string }>(
        `/workspaces/${rootId}?confirm=true`,
      );
      showToast(`Cancelled deployment of "${rootName}"`, "success");
      // Optimistic local removal — workspace-server broadcasts
      // WORKSPACE_REMOVED per node but the WS may lag; strip the
      // subtree now so the user sees immediate feedback. Re-read
      // the store AFTER the await: children may have landed (or
      // already been removed by WS events) during the network
      // round-trip. If the WS_REMOVED handler already dropped the
      // root during the network call, bail out — the subtree walk
      // would miss any now-orphaned descendants (handleCanvasEvent
      // reparents children of a removed node upward, so they no
      // longer share the original root's id as parentId).
      const postDeleteState = useCanvasStore.getState();
      if (!postDeleteState.nodes.some((n) => n.id === rootId)) {
        return;
      }
      const subtree = new Set<string>();
      const stack = [rootId];
      while (stack.length) {
        const id = stack.pop()!;
        subtree.add(id);
        for (const n of postDeleteState.nodes) {
          if (n.data.parentId === id) stack.push(n.id);
        }
      }
      useCanvasStore.setState({
        nodes: postDeleteState.nodes.filter((n) => !subtree.has(n.id)),
        edges: postDeleteState.edges.filter(
          (e) => !subtree.has(e.source) && !subtree.has(e.target),
        ),
      });
    } catch (e) {
      // Undo the lock so the user can try again / interact with the
      // still-deploying subtree.
      useCanvasStore.getState().endDelete(subtreeIds);
      showToast(
        e instanceof Error ? `Cancel failed: ${e.message}` : "Cancel failed",
        "error",
      );
    } finally {
      // Success path's endDelete is covered implicitly — every node
      // in the subtree is stripped by the optimistic local removal
      // above, and any stragglers are removed by WORKSPACE_REMOVED
      // WS events whose handler is a no-op on already-missing ids.
      // The deletingIds set will naturally empty as endDelete runs
      // in both paths below.
      useCanvasStore.getState().endDelete(subtreeIds);
      setSubmitting(false);
      setConfirming(false);
    }
  };

  if (confirming) {
    return (
      <div
        className="nodrag absolute -top-10 right-0 z-20 flex items-center gap-1.5 rounded-lg bg-surface-sunken/95 px-2 py-1 shadow-lg border border-red-800/60"
        onClick={(e) => e.stopPropagation()}
      >
        <span className="text-[10px] text-ink-mid">
          Delete {workspaceCount} workspace{workspaceCount === 1 ? "" : "s"}?
        </span>
        <button
          type="button"
          onClick={handleCancel}
          disabled={submitting}
          className="mol-deploy-cancel px-2 py-0.5 rounded text-[10px] font-semibold"
        >
          {submitting ? "Deleting…" : "Yes"}
        </button>
        <button
          type="button"
          onClick={() => setConfirming(false)}
          disabled={submitting}
          className="px-2 py-0.5 rounded bg-surface-card/80 hover:bg-surface-card text-[10px] text-ink"
        >
          No
        </button>
      </div>
    );
  }

  return (
    <button
      type="button"
      onClick={(e) => {
        // Stop the click from bubbling to React Flow (selects the
        // node) — the Cancel pill is a UI surface, not a node
        // activation.
        e.stopPropagation();
        setConfirming(true);
      }}
      className="nodrag mol-deploy-cancel mol-deploy-cancel-pulse absolute -top-7 right-1 z-20 flex items-center gap-1 rounded-full px-2.5 py-0.5 text-[10px] font-semibold shadow-md"
      aria-label={`Cancel deployment of ${rootName}`}
    >
      <svg width="10" height="10" viewBox="0 0 16 16" aria-hidden="true">
        <path
          d="M4 4l8 8M12 4l-8 8"
          stroke="currentColor"
          strokeWidth="2"
          strokeLinecap="round"
        />
      </svg>
      <span>Cancel ({workspaceCount})</span>
    </button>
  );
}
