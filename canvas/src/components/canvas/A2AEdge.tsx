"use client";

import { memo } from "react";
import {
  BaseEdge,
  EdgeLabelRenderer,
  getBezierPath,
  type EdgeProps,
} from "@xyflow/react";
import { useCanvasStore } from "@/store/canvas";

/**
 * Custom edge for the A2A topology overlay. Solves two problems with the
 * default React Flow edge label rendering:
 *
 *   1. **Z-order.** The default `label` prop renders inside the edge's
 *      SVG group, which always sits below node DOM in React Flow. When
 *      a label happened to land underneath a workspace card, it was
 *      hidden. EdgeLabelRenderer mounts label content in a separate
 *      portal layer that we can pin above nodes via z-index.
 *
 *   2. **Clickability.** Default labels inherit `pointerEvents: none`
 *      from the SVG path so the user can drag through them. The
 *      portaled label is a regular HTML element with its own pointer
 *      events — we set `pointerEvents: all` only on the label pill so
 *      drags on the edge line still pass through to the canvas.
 *
 * On click: selects the source workspace and switches its side panel
 * to Activity, where the user can inspect the underlying delegations.
 */
interface A2AEdgeData {
  count: number;
  lastAt: number;
  isHot: boolean;
  /** Pre-formatted "5 calls · 2m ago" — built upstream by buildA2AEdges
   *  so the same string renders here and in any future tooltip layer. */
  label: string;
}

function A2AEdgeImpl({
  id,
  source,
  sourceX,
  sourceY,
  targetX,
  targetY,
  sourcePosition,
  targetPosition,
  data,
  style = {},
}: EdgeProps) {
  const [edgePath, labelX, labelY] = getBezierPath({
    sourceX,
    sourceY,
    sourcePosition,
    targetX,
    targetY,
    targetPosition,
  });

  const selectNode = useCanvasStore((s) => s.selectNode);
  const setPanelTab = useCanvasStore((s) => s.setPanelTab);

  const edgeData = (data ?? {}) as Partial<A2AEdgeData>;
  const labelText = edgeData.label ?? "";
  const isHot = edgeData.isHot ?? false;
  const count = edgeData.count ?? 0;

  const handleClick = (e: React.MouseEvent) => {
    e.stopPropagation();
    // Select the source (the agent that initiated the delegations).
    // The user's mental model when clicking the edge is "show me the
    // calls FROM here" — that's the source's activity feed.
    //
    // Preserve the current tab when the user re-clicks the same edge
    // (or another edge whose source is already selected). Yanking
    // them back to Activity every click would surprise — they may
    // have intentionally switched to Chat / Memory while looking at
    // this peer. The first click that lands a *different* selection
    // still routes them to Activity, which is the discovery affordance.
    const alreadySelected =
      useCanvasStore.getState().selectedNodeId === source;
    selectNode(source);
    if (!alreadySelected) {
      setPanelTab("activity");
    }
  };

  // The edge stroke color matches what buildA2AEdges sets on the SVG
  // path style. Mirror it on the badge border so the visual identity
  // (hot=violet vs warm=blue) carries to the clickable label.
  const accent = isHot ? "border-violet-500/60" : "border-accent/60";
  const accentText = isHot ? "text-violet-200" : "text-blue-200";
  const ariaLabel = `${count} delegation${count === 1 ? "" : "s"} from ${
    edgeData.label?.split(" · ")[1] ?? "recent"
  }. Click to inspect.`;

  return (
    <>
      <BaseEdge id={id} path={edgePath} style={style} markerEnd="url(#a2a-arrow)" />
      {labelText && (
        <EdgeLabelRenderer>
          <div
            // The label sits in a portal at the canvas root. position:
            // absolute + the (labelX, labelY) translate places it at
            // the edge midpoint. zIndex 5 wins against React Flow's
            // node layer (default z=0) without fighting the controls
            // strip (z=10).
            style={{
              position: "absolute",
              transform: `translate(-50%, -50%) translate(${labelX}px, ${labelY}px)`,
              pointerEvents: "all",
              zIndex: 5,
            }}
            className="nodrag nopan"
          >
            <button
              type="button"
              onClick={handleClick}
              aria-label={ariaLabel}
              title="Open source workspace's activity feed"
              className={`px-2 py-0.5 rounded-full bg-surface-sunken/95 border ${accent} ${accentText} text-[10px] font-medium shadow-md shadow-black/40 backdrop-blur-sm hover:bg-surface-card hover:border-opacity-100 transition-colors cursor-pointer`}
            >
              {labelText}
            </button>
          </div>
        </EdgeLabelRenderer>
      )}
    </>
  );
}

export const A2AEdge = memo(A2AEdgeImpl);
