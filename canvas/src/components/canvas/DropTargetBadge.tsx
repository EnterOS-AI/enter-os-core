"use client";

import { useReactFlow } from "@xyflow/react";
import { useCanvasStore, type WorkspaceNodeData } from "@/store/canvas";
import {
  defaultChildSlot,
  CHILD_DEFAULT_HEIGHT,
  CHILD_DEFAULT_WIDTH,
} from "@/store/canvas-topology";

/**
 * Floating affordance that tracks the current drag target. Two visuals
 * are layered on top of React Flow, both in screen space:
 *
 *   1. Ghost preview — dashed outline at the next default grid slot
 *      inside the target parent. Whimsical-style: users see exactly
 *      where the card will land before releasing.
 *   2. Text badge — "Drop into: <name>" floating above the target. The
 *      coloured outline alone is ambiguous on dense canvases; spelling
 *      the name out is the Mural pattern.
 *
 * Colour alone isn't an accessible cue, so the pair (outline + label)
 * is deliberate.
 */
export function DropTargetBadge() {
  const dragOverNodeId = useCanvasStore((s) => s.dragOverNodeId);
  const targetName = useCanvasStore((s) => {
    if (!s.dragOverNodeId) return null;
    const n = s.nodes.find((nn) => nn.id === s.dragOverNodeId);
    return (n?.data as WorkspaceNodeData | undefined)?.name ?? null;
  });
  const childCount = useCanvasStore((s) =>
    !s.dragOverNodeId
      ? 0
      : s.nodes.filter((n) => n.parentId === s.dragOverNodeId).length,
  );
  const { getInternalNode, flowToScreenPosition } = useReactFlow();
  if (!dragOverNodeId || !targetName) return null;
  const internal = getInternalNode(dragOverNodeId);
  if (!internal) return null;
  const abs = internal.internals.positionAbsolute;
  const w = internal.measured?.width ?? 220;
  const h = internal.measured?.height ?? 120;
  const badge = flowToScreenPosition({ x: abs.x + w / 2, y: abs.y });

  const slot = defaultChildSlot(childCount);
  const slotTL = flowToScreenPosition({ x: abs.x + slot.x, y: abs.y + slot.y });
  const slotBR = flowToScreenPosition({
    x: abs.x + slot.x + CHILD_DEFAULT_WIDTH,
    y: abs.y + slot.y + CHILD_DEFAULT_HEIGHT,
  });
  // Clip: don't draw the ghost if its rect falls entirely outside the
  // parent (can happen when a parent is smaller than one default slot).
  const parentTL = flowToScreenPosition({ x: abs.x, y: abs.y });
  const parentBR = flowToScreenPosition({ x: abs.x + w, y: abs.y + h });
  const ghostVisible =
    slotBR.x > parentTL.x &&
    slotTL.x < parentBR.x &&
    slotBR.y > parentTL.y &&
    slotTL.y < parentBR.y;

  return (
    <>
      {ghostVisible && (
        <div
          data-testid="ghost-slot"
          className="pointer-events-none absolute z-40 rounded-lg border-2 border-dashed border-emerald-400/70 bg-emerald-500/10"
          style={{
            left: slotTL.x,
            top: slotTL.y,
            width: slotBR.x - slotTL.x,
            height: slotBR.y - slotTL.y,
          }}
        />
      )}
      <div
        data-testid="drop-badge"
        className="pointer-events-none absolute z-50 -translate-x-1/2 -translate-y-full rounded-md bg-emerald-500 px-2 py-0.5 text-[11px] font-medium text-emerald-50 shadow-lg shadow-emerald-950/40"
        style={{ left: badge.x, top: badge.y - 6 }}
      >
        Drop into: {targetName}
      </div>
    </>
  );
}
