import type { useReactFlow } from "@xyflow/react";
import { useCanvasStore } from "@/store/canvas";

/**
 * Hysteresis threshold for drag-out detach. A child only un-nests from
 * its parent once at least this fraction of its bounding box lies
 * outside the parent's bbox — a twitchy release 1px past the edge stays
 * nested. Miro / tldraw use roughly 20-30%; 20% feels responsive.
 */
export const DETACH_FRACTION = 0.2;

type InternalNode = ReturnType<ReturnType<typeof useReactFlow>["getInternalNode"]>;
type GetInternalNode = (id: string) => InternalNode;

/**
 * True when the child has moved far enough outside its parent's bbox
 * that the gesture is unambiguously an un-nest. Returns true when we
 * can't measure either node (conservative fall-back matches the
 * original behaviour).
 */
export function shouldDetach(
  childId: string,
  parentId: string,
  getInternalNode: GetInternalNode,
): boolean {
  const c = getInternalNode(childId);
  const p = getInternalNode(parentId);
  if (!c || !p) return true;
  const cw = c.measured?.width ?? c.width ?? 220;
  const ch = c.measured?.height ?? c.height ?? 120;
  const pw = p.measured?.width ?? p.width ?? 220;
  const ph = p.measured?.height ?? p.height ?? 120;
  const cx = c.internals.positionAbsolute;
  const px = p.internals.positionAbsolute;
  const overlapW =
    Math.max(0, Math.min(cx.x + cw, px.x + pw) - Math.max(cx.x, px.x));
  const overlapH =
    Math.max(0, Math.min(cx.y + ch, px.y + ph) - Math.max(cx.y, px.y));
  const outsideFractionX = 1 - overlapW / cw;
  const outsideFractionY = 1 - overlapH / ch;
  return outsideFractionX > DETACH_FRACTION || outsideFractionY > DETACH_FRACTION;
}

/**
 * Snap a child back so its bbox is fully inside the parent's bounds.
 * Called on drag-stop when the user drifted slightly past the edge
 * without holding Alt or Cmd — the canvas treats the gesture as a
 * plain move rather than an un-nest.
 */
export function clampChildIntoParent(
  childId: string,
  parentId: string,
  getInternalNode: GetInternalNode,
) {
  const c = getInternalNode(childId);
  const p = getInternalNode(parentId);
  if (!c || !p) return;
  const cw = c.measured?.width ?? c.width ?? 220;
  const ch = c.measured?.height ?? c.height ?? 120;
  const pw = p.measured?.width ?? p.width ?? 220;
  const ph = p.measured?.height ?? p.height ?? 120;
  const { nodes } = useCanvasStore.getState();
  const cur = nodes.find((n) => n.id === childId);
  if (!cur) return;
  const rel = cur.position;
  const clampedX = Math.max(0, Math.min(rel.x, pw - cw));
  const clampedY = Math.max(0, Math.min(rel.y, ph - ch));
  if (clampedX === rel.x && clampedY === rel.y) return;
  useCanvasStore.setState({
    nodes: nodes.map((n) =>
      n.id === childId ? { ...n, position: { x: clampedX, y: clampedY } } : n,
    ),
  });
}
