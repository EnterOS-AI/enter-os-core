/** MessageFlightLayer — flies an envelope from the source agent to the target
 *  agent on the spatial canvas whenever a delegate / message event fires.
 *
 *  Mounted INSIDE <ReactFlow> so its ViewportPortal places the envelope in flow
 *  coordinates; it therefore pans and zooms with the canvas for free. The
 *  flight lifecycle (which events become envelopes, reduced-motion opt-out,
 *  expiry) lives in useA2AFlights — this component only resolves endpoints and
 *  renders.
 *
 *  Endpoints anchor on each workspace's STATUS DOT (the green/glowing presence
 *  indicator), not the card's geometric centre — so an envelope visibly leaves
 *  the source agent's dot and lands on the target agent's dot. The dot carries
 *  `data-flight-anchor`; we read its rendered rect and convert screen→flow via
 *  React Flow, falling back to the card centre only when the dot isn't in the
 *  DOM yet (node just mounted / scrolled out). */
import { useRef } from "react";
import { ViewportPortal, useReactFlow, type Node } from "@xyflow/react";
import { useCanvasStore } from "@/store/canvas";
import { useA2AFlights, type A2AFlight } from "@/hooks/useA2AFlights";
import { FlightEnvelope, type Point } from "./FlightEnvelope";
import type { WorkspaceNodeData } from "@/store/canvas";

// Fallback node footprint when React Flow has not measured a node yet. Matches
// WorkspaceNode's leaf size (w-[300px] min-h-[176px]); a slightly-off centre for
// the first frame after mount is invisible at flight scale.
const DEFAULT_W = 300;
const DEFAULT_H = 176;

function nodeCenter(n: Node<WorkspaceNodeData>): Point {
  const w = n.measured?.width ?? DEFAULT_W;
  const h = n.measured?.height ?? DEFAULT_H;
  return { x: n.position.x + w / 2, y: n.position.y + h / 2 };
}

/** Resolve a node's status-dot centre in FLOW coordinates. Reads the dot's
 *  rendered screen rect (it carries data-flight-anchor) and converts it back to
 *  flow space, so the anchor is exact regardless of pan/zoom and survives any
 *  header-layout change. Falls back to the card centre when the dot isn't
 *  rendered. */
function dotAnchor(
  n: Node<WorkspaceNodeData>,
  screenToFlowPosition: (p: Point) => Point,
): Point {
  if (typeof document !== "undefined") {
    const id =
      typeof CSS !== "undefined" && typeof CSS.escape === "function" ? CSS.escape(n.id) : n.id;
    const el = document.querySelector<HTMLElement>(
      `.react-flow__node[data-id="${id}"] [data-flight-anchor]`,
    );
    if (el) {
      const r = el.getBoundingClientRect();
      if (r.width > 0 && r.height > 0) {
        return screenToFlowPosition({ x: r.left + r.width / 2, y: r.top + r.height / 2 });
      }
    }
  }
  return nodeCenter(n);
}

/** One flight. Captures the source/target dot anchors ONCE on mount (a ref, not
 *  per-render) so a pan/zoom or re-render mid-flight doesn't restart the
 *  animation — mirrors HomeFlight's capture-once contract. */
function CanvasFlight({
  flight,
  nodes,
  screenToFlowPosition,
}: {
  flight: A2AFlight;
  nodes: Node<WorkspaceNodeData>[];
  screenToFlowPosition: (p: Point) => Point;
}) {
  const pos = useRef<{ from: Point; to: Point } | null>(null);
  if (pos.current === null) {
    const src = nodes.find((n) => n.id === flight.sourceId);
    const dst = nodes.find((n) => n.id === flight.targetId);
    // Both endpoints must be on-canvas to draw a path between them.
    if (src && dst) {
      pos.current = {
        from: dotAnchor(src, screenToFlowPosition),
        to: dotAnchor(dst, screenToFlowPosition),
      };
    }
  }
  if (!pos.current) return null;
  return <FlightEnvelope from={pos.current.from} to={pos.current.to} kind={flight.kind} />;
}

export function MessageFlightLayer() {
  const flights = useA2AFlights();
  const nodes = useCanvasStore((s) => s.nodes) as Node<WorkspaceNodeData>[];
  const { screenToFlowPosition } = useReactFlow();

  if (flights.length === 0) return null;

  return (
    <ViewportPortal>
      {flights.map((f) => (
        <CanvasFlight
          key={f.key}
          flight={f}
          nodes={nodes}
          screenToFlowPosition={screenToFlowPosition}
        />
      ))}
    </ViewportPortal>
  );
}
